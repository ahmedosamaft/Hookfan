package api

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// contextWithCancel derives a cancellable context from a request, standing in
// for a browser closing the connection.
func contextWithCancel(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithCancel(r.Context())
}

func testBroker() *Broker {
	return NewBroker(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestBrokerFansOutToEverySubscriber(t *testing.T) {
	b := testBroker()
	a := b.subscribe()
	c := b.subscribe()
	defer b.unsubscribe(a)
	defer b.unsubscribe(c)

	if got := b.Subscribers(); got != 2 {
		t.Fatalf("Subscribers() = %d, want 2", got)
	}

	b.Publish(StreamEventReceived, map[string]any{"id": 1})

	for i, ch := range []chan StreamMessage{a, c} {
		select {
		case msg := <-ch:
			if msg.Kind != StreamEventReceived {
				t.Errorf("subscriber %d: Kind = %q, want %q", i, msg.Kind, StreamEventReceived)
			}
		case <-time.After(time.Second):
			t.Errorf("subscriber %d received nothing", i)
		}
	}
}

// The critical property: a browser that stops reading must not be able to
// stall ingest or a delivery worker.
func TestPublishNeverBlocksOnSlowSubscriber(t *testing.T) {
	b := testBroker()
	slow := b.subscribe()
	defer b.unsubscribe(slow)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Far more than the buffer holds, from a subscriber that never reads.
		for i := range subscriberBuffer * 4 {
			b.Publish(StreamEventReceived, map[string]any{"id": i})
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a subscriber that was not reading")
	}

	// The buffered messages are still deliverable; the excess was dropped.
	if len(slow) != subscriberBuffer {
		t.Errorf("buffered %d messages, want the %d-message buffer to be full",
			len(slow), subscriberBuffer)
	}
	// Dropping must be counted rather than silent, or a UI quietly missing
	// updates looks identical to a quiet system.
	wantDropped := int64(subscriberBuffer*4 - subscriberBuffer)
	if got := b.Dropped(); got != wantDropped {
		t.Errorf("Dropped() = %d, want %d", got, wantDropped)
	}
}

func TestUnsubscribeIsIdempotent(t *testing.T) {
	b := testBroker()
	ch := b.subscribe()

	b.unsubscribe(ch)
	// A second unsubscribe must not panic on an already-closed channel.
	b.unsubscribe(ch)

	if got := b.Subscribers(); got != 0 {
		t.Errorf("Subscribers() = %d, want 0", got)
	}
}

func TestPublishConcurrentlyIsSafe(t *testing.T) {
	b := testBroker()
	for range 4 {
		ch := b.subscribe()
		// Drain continuously so nothing fills.
		go func() {
			for range ch {
			}
		}()
	}

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := range 50 {
				b.Publish(StreamDeliveryUpdated, map[string]any{"w": n, "j": j})
			}
		}(i)
	}
	wg.Wait()
	b.Close()
}

// The wire format has to be exactly what EventSource expects.
func TestStreamWritesSSEFormat(t *testing.T) {
	b := testBroker()

	req := httptest.NewRequest(http.MethodGet, "/api/stream", nil)
	ctx, cancel := contextWithCancel(req)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Stream(rec, req)
	}()

	// Wait for the handler to register before publishing.
	deadline := time.Now().Add(2 * time.Second)
	for b.Subscribers() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if b.Subscribers() == 0 {
		cancel()
		t.Fatal("stream handler never subscribed")
	}

	b.Publish(StreamEventReceived, map[string]any{"id": 42})
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if rec.Header().Get("Cache-Control") != "no-cache" {
		t.Error("Cache-Control should be no-cache")
	}
	// Buffering proxies would defeat a live stream.
	if rec.Header().Get("X-Accel-Buffering") != "no" {
		t.Error("X-Accel-Buffering should be no")
	}
	if !strings.Contains(body, ": connected") {
		t.Error("stream did not open with a comment; the client's onopen would be delayed")
	}
	if !strings.Contains(body, "event: "+StreamEventReceived) {
		t.Errorf("missing event line in:\n%s", body)
	}
	if !strings.Contains(body, `"id":42`) {
		t.Errorf("missing payload in:\n%s", body)
	}
	// Each SSE frame ends with a blank line.
	if !strings.Contains(body, "\n\n") {
		t.Error("frames are not terminated by a blank line")
	}

	scanner := bufio.NewScanner(strings.NewReader(body))
	var sawData bool
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data: ") {
			sawData = true
		}
	}
	if !sawData {
		t.Error("no data: line found")
	}
}

func TestStreamUnsubscribesOnDisconnect(t *testing.T) {
	b := testBroker()

	req := httptest.NewRequest(http.MethodGet, "/api/stream", nil)
	ctx, cancel := contextWithCancel(req)
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Stream(httptest.NewRecorder(), req)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for b.Subscribers() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	<-done

	// A leaked subscriber would keep buffering messages forever.
	if got := b.Subscribers(); got != 0 {
		t.Errorf("Subscribers() = %d after disconnect, want 0", got)
	}
}
