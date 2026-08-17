package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Event kinds published on the stream.
const (
	StreamEventReceived   = "event.received"
	StreamDeliveryUpdated = "delivery.updated"
	StreamServiceUpdated  = "service.updated"
)

// StreamMessage is one item on the feed.
type StreamMessage struct {
	Kind string `json:"kind"`
	Data any    `json:"data"`
}

// subscriberBuffer is how many messages a slow client may fall behind before
// its messages start being dropped.
const subscriberBuffer = 64

// Broker fans messages out to connected SSE clients.
//
// Publishing never blocks: a browser on a slow connection must not be able to
// stall the ingest path or a delivery worker. A subscriber whose buffer is
// full loses messages instead, which is the right trade for a live dashboard —
// the authoritative data is always a page refresh away.
type Broker struct {
	mu          sync.RWMutex
	subscribers map[chan StreamMessage]struct{}
	log         *slog.Logger
	// Atomic because Publish increments it under RLock, which several
	// publishers hold at once — a plain ++ there is a read-modify-write race.
	dropped atomic.Int64
}

func NewBroker(log *slog.Logger) *Broker {
	return &Broker{
		subscribers: map[chan StreamMessage]struct{}{},
		log:         log,
	}
}

func (b *Broker) subscribe() chan StreamMessage {
	ch := make(chan StreamMessage, subscriberBuffer)
	b.mu.Lock()
	b.subscribers[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *Broker) unsubscribe(ch chan StreamMessage) {
	b.mu.Lock()
	if _, ok := b.subscribers[ch]; ok {
		delete(b.subscribers, ch)
		close(ch)
	}
	b.mu.Unlock()
}

// Publish sends a message to every connected client, dropping it for any
// client that cannot keep up.
func (b *Broker) Publish(kind string, data any) {
	msg := StreamMessage{Kind: kind, Data: data}

	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subscribers {
		select {
		case ch <- msg:
		default:
			// Never block a publisher on a slow reader.
			b.dropped.Add(1)
		}
	}
}

// Dropped reports how many messages were discarded because a subscriber was
// not keeping up. A steadily rising figure means the UI is missing live
// updates, which a page refresh corrects.
func (b *Broker) Dropped() int64 { return b.dropped.Load() }

// Subscribers reports how many clients are connected.
func (b *Broker) Subscribers() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
}

// Stream serves GET /api/stream as Server-Sent Events.
func (b *Broker) Stream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported by this server")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Proxies that buffer responses would defeat the point of a live stream.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ch := b.subscribe()
	defer b.unsubscribe(ch)

	// An initial comment opens the stream immediately, so the client's
	// onopen fires without waiting for the first real event.
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	b.log.Debug("sse client connected", "subscribers", b.Subscribers())

	// Keep-alive comments stop idle connections being closed by intermediate
	// proxies, and surface a dead peer to the write below.
	keepAlive := time.NewTicker(20 * time.Second)
	defer keepAlive.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			b.log.Debug("sse client disconnected", "subscribers", b.Subscribers()-1)
			return

		case msg, open := <-ch:
			if !open {
				return
			}
			payload, err := json.Marshal(msg.Data)
			if err != nil {
				b.log.Warn("could not encode stream message", "kind", msg.Kind, "error", err)
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", msg.Kind, payload); err != nil {
				return // client is gone
			}
			flusher.Flush()

		case <-keepAlive.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// Close disconnects every client, used during shutdown.
func (b *Broker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subscribers {
		delete(b.subscribers, ch)
		close(ch)
	}
}
