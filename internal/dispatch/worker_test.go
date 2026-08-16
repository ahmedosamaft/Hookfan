package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aosama/hookfan/internal/crypto"
	"github.com/aosama/hookfan/internal/store"
)

type workerFixture struct {
	store      *store.Store
	dispatcher *Dispatcher
	cipher     *crypto.Cipher
	listener   *store.Listener
	ctx        context.Context
}

func newWorkerFixture(t *testing.T) *workerFixture {
	t.Helper()
	st := store.TestStore(t)
	ctx := context.Background()

	cipher, err := crypto.NewCipher(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	listener, err := st.CreateListener(ctx, store.CreateListenerParams{
		Name: "L", Slug: "l", Provider: "meta", VerificationMode: "none",
		RoutingKeyPath: "entry[*].id", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create listener: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &workerFixture{
		store:      st,
		cipher:     cipher,
		listener:   listener,
		ctx:        ctx,
		dispatcher: NewDispatcher(st, cipher, NewSSRFGuard(true), log, 4),
	}
}

// addService registers a verified service pointing at url with the given token.
func (f *workerFixture) addService(t *testing.T, name, url, token string, maxAttempts int) *store.Service {
	t.Helper()
	enc, err := f.cipher.EncryptString(token)
	if err != nil {
		t.Fatalf("encrypt token: %v", err)
	}
	pub, _ := crypto.RandomToken(8)
	svc, err := f.store.CreateService(f.ctx, store.CreateServiceParams{
		PublicID: "svc_" + pub, Name: name, URL: url, Method: "POST",
		LinkToken: enc, TimeoutMS: 3000, MaxAttempts: maxAttempts, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create service: %v", err)
	}
	verified, err := f.store.MarkVerified(f.ctx, svc.ID)
	if err != nil {
		t.Fatalf("mark verified: %v", err)
	}
	return verified
}

// queue inserts an event and a delivery for it, ready to be claimed.
func (f *workerFixture) queue(t *testing.T, svc *store.Service, body string) (eventID, deliveryID int64) {
	t.Helper()
	event, _, err := f.store.InsertEvent(f.ctx, store.InsertEventParams{
		ListenerID: f.listener.ID, RoutingKeys: []string{"K"}, RawBody: []byte(body),
		Headers:        map[string]string{"X-Hub-Signature-256": "sha256=originalsig"},
		SignatureValid: true, DedupeKey: crypto.BodyHash([]byte(body)),
	})
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}

	err = f.store.Pool().QueryRow(f.ctx, `
		INSERT INTO deliveries (event_id, service_id) VALUES ($1,$2) RETURNING id`,
		event.ID, svc.ID).Scan(&deliveryID)
	if err != nil {
		t.Fatalf("insert delivery: %v", err)
	}
	return event.ID, deliveryID
}

func (f *workerFixture) delivery(t *testing.T, id int64) *store.Delivery {
	t.Helper()
	var eventID int64
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT event_id FROM deliveries WHERE id=$1`, id).Scan(&eventID); err != nil {
		t.Fatalf("find delivery: %v", err)
	}
	all, err := f.store.DeliveriesForEvent(f.ctx, eventID)
	if err != nil {
		t.Fatalf("load deliveries: %v", err)
	}
	for _, d := range all {
		if d.ID == id {
			return d
		}
	}
	t.Fatalf("delivery %d not found", id)
	return nil
}

// runOnce performs a single claim-and-deliver pass.
func (f *workerFixture) runOnce(t *testing.T) {
	t.Helper()
	if _, err := f.dispatcher.claimAndDeliver(f.ctx, "test-worker"); err != nil {
		t.Fatalf("claimAndDeliver: %v", err)
	}
}

// The receiving service must be able to authenticate the gateway, and the body
// must arrive byte-for-byte so the provider's own signature still verifies.
func TestDeliverForwardsBodyAndHeaders(t *testing.T) {
	const token = "link-token-abc"
	body := `{"object":"whatsapp_business_account","entry":[{"id":"WABA_ONE"}]}`

	var gotBody []byte
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := newWorkerFixture(t)
	svc := f.addService(t, "alpha", srv.URL, token, 6)
	eventID, deliveryID := f.queue(t, svc, body)

	f.runOnce(t)

	if string(gotBody) != body {
		t.Errorf("forwarded body = %q, want byte-identical %q", gotBody, body)
	}
	if got := gotHeaders.Get("X-Hookfan-Token"); got != token {
		t.Errorf("X-Hookfan-Token = %q, want %q", got, token)
	}
	sig := strings.TrimPrefix(gotHeaders.Get("X-Hookfan-Signature"), "sha256=")
	if !crypto.VerifyHex([]byte(token), gotBody, sig) {
		t.Error("X-Hookfan-Signature does not verify against the forwarded body")
	}
	// Still valid, because the payload was never split or re-marshalled.
	if got := gotHeaders.Get("X-Hookfan-Original-Signature"); got != "sha256=originalsig" {
		t.Errorf("X-Hookfan-Original-Signature = %q, want the provider's original", got)
	}
	if got := gotHeaders.Get("X-Hookfan-Event-Id"); got == "" {
		t.Error("X-Hookfan-Event-Id is missing")
	}
	if got := gotHeaders.Get("X-Hookfan-Listener"); got != "l" {
		t.Errorf("X-Hookfan-Listener = %q, want the listener slug", got)
	}
	if got := gotHeaders.Get("X-Hookfan-Attempt"); got != "1" {
		t.Errorf("X-Hookfan-Attempt = %q, want 1", got)
	}

	d := f.delivery(t, deliveryID)
	if d.Status != "success" {
		t.Errorf("status = %q, want success", d.Status)
	}
	if d.EventID != eventID {
		t.Errorf("event_id = %d, want %d", d.EventID, eventID)
	}
}

func TestDeliverTerminalOn4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	f := newWorkerFixture(t)
	svc := f.addService(t, "alpha", srv.URL, "tok", 6)
	_, deliveryID := f.queue(t, svc, `{"a":1}`)

	f.runOnce(t)

	d := f.delivery(t, deliveryID)
	// A 400 must not consume five more attempts.
	if d.Status != "failed" {
		t.Errorf("status = %q, want failed (terminal)", d.Status)
	}
	if d.AttemptCount != 1 {
		t.Errorf("attempt_count = %d, want 1", d.AttemptCount)
	}
	if d.LastStatusCode == nil || *d.LastStatusCode != 400 {
		t.Errorf("last_status_code = %v, want 400", d.LastStatusCode)
	}
}

func TestDeliverRetriesOn5xx(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	f := newWorkerFixture(t)
	svc := f.addService(t, "alpha", srv.URL, "tok", 3)
	_, deliveryID := f.queue(t, svc, `{"a":1}`)

	f.runOnce(t)

	d := f.delivery(t, deliveryID)
	if d.Status != "pending" {
		t.Errorf("status = %q, want pending (scheduled for retry)", d.Status)
	}
	if d.AttemptCount != 1 {
		t.Errorf("attempt_count = %d, want 1", d.AttemptCount)
	}
	if !d.NextAttempt.After(time.Now()) {
		t.Error("next_attempt_at is not in the future")
	}
	if hits.Load() != 1 {
		t.Errorf("service was hit %d times, want 1", hits.Load())
	}
}

// After max_attempts the delivery is dead, not retried forever.
func TestDeliverBecomesDeadAfterMaxAttempts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	f := newWorkerFixture(t)
	svc := f.addService(t, "alpha", srv.URL, "tok", 2)
	_, deliveryID := f.queue(t, svc, `{"a":1}`)

	// Attempt 1 schedules a retry; force it due, then attempt 2 exhausts.
	f.runOnce(t)
	if _, err := f.store.Pool().Exec(f.ctx,
		`UPDATE deliveries SET next_attempt_at = now() WHERE id=$1`, deliveryID); err != nil {
		t.Fatalf("force due: %v", err)
	}
	f.runOnce(t)

	d := f.delivery(t, deliveryID)
	if d.Status != "dead" {
		t.Errorf("status = %q, want dead after exhausting attempts", d.Status)
	}
	if d.AttemptCount != 2 {
		t.Errorf("attempt_count = %d, want 2", d.AttemptCount)
	}
}

func TestDeliverHonoursRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	f := newWorkerFixture(t)
	svc := f.addService(t, "alpha", srv.URL, "tok", 6)
	_, deliveryID := f.queue(t, svc, `{"a":1}`)

	before := time.Now()
	f.runOnce(t)

	d := f.delivery(t, deliveryID)
	if d.Status != "pending" {
		t.Fatalf("status = %q, want pending", d.Status)
	}
	// The service asked for 60s; our own backoff would have been far shorter.
	wait := d.NextAttempt.Sub(before)
	if wait < 55*time.Second || wait > 65*time.Second {
		t.Errorf("next attempt in %v, want about 60s from Retry-After", wait)
	}
}

// The breaker must trip on sustained failure and disable the service.
func TestCircuitBreakerTrips(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()

	f := newWorkerFixture(t)
	svc := f.addService(t, "alpha", srv.URL, "tok", 100)

	// Exactly one failure per pass, so the trip point is the threshold rather
	// than whatever extra retries happen to land.
	for i := range BreakerThreshold {
		f.queue(t, svc, randomBody())

		var failures int
		_ = f.store.Pool().QueryRow(f.ctx,
			`SELECT consecutive_failures FROM services WHERE id=$1`, svc.ID).Scan(&failures)
		if failures != i {
			t.Fatalf("before attempt %d: consecutive_failures = %d, want %d", i+1, failures, i)
		}
		// The breaker must not trip early.
		if disabled, _ := f.store.ServiceDisabled(f.ctx, svc.ID); disabled && i < BreakerThreshold-1 {
			t.Fatalf("service disabled after only %d failures, want %d", i, BreakerThreshold)
		}

		f.runOnce(t)
		if _, err := f.store.Pool().Exec(f.ctx,
			`UPDATE deliveries SET next_attempt_at = now() + interval '1 hour'
			  WHERE status = 'pending'`); err != nil {
			t.Fatalf("defer retries: %v", err)
		}
	}

	disabled, err := f.store.ServiceDisabled(f.ctx, svc.ID)
	if err != nil {
		t.Fatalf("check disabled: %v", err)
	}
	if !disabled {
		var failures int
		_ = f.store.Pool().QueryRow(f.ctx,
			`SELECT consecutive_failures FROM services WHERE id=$1`, svc.ID).Scan(&failures)
		t.Errorf("service not disabled after %d consecutive failures (counter = %d)",
			BreakerThreshold, failures)
	}

	var reason string
	_ = f.store.Pool().QueryRow(f.ctx,
		`SELECT COALESCE(disabled_reason,'') FROM services WHERE id=$1`, svc.ID).Scan(&reason)
	if reason == "" {
		t.Error("disabled_reason is empty; the UI banner needs an explanation")
	}
}

// A success must clear the counter, so intermittent errors never accumulate
// into a trip.
func TestCircuitBreakerResetsOnSuccess(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := newWorkerFixture(t)
	svc := f.addService(t, "alpha", srv.URL, "tok", 100)

	// One delivery at a time: a pass claims every due row, so leaving retried
	// rows due would attempt them again and inflate the counter.
	for range 5 {
		f.queue(t, svc, randomBody())
		f.runOnce(t)
		// Push retries out of the way so the next pass sees only the new row.
		if _, err := f.store.Pool().Exec(f.ctx,
			`UPDATE deliveries SET next_attempt_at = now() + interval '1 hour'
			  WHERE status = 'pending'`); err != nil {
			t.Fatalf("defer retries: %v", err)
		}
	}

	var failures int
	_ = f.store.Pool().QueryRow(f.ctx,
		`SELECT consecutive_failures FROM services WHERE id=$1`, svc.ID).Scan(&failures)
	if failures != 5 {
		t.Fatalf("consecutive_failures = %d, want 5", failures)
	}

	fail.Store(false)
	_, _ = f.queue(t, svc, randomBody())
	f.runOnce(t)

	_ = f.store.Pool().QueryRow(f.ctx,
		`SELECT consecutive_failures FROM services WHERE id=$1`, svc.ID).Scan(&failures)
	if failures != 0 {
		t.Errorf("consecutive_failures = %d after a success, want 0", failures)
	}
}

// Concurrent workers must never deliver the same row twice: SKIP LOCKED plus
// the atomic claim is what guarantees it.
func TestConcurrentClaimsNeverDoubleDeliver(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Hookfan-Delivery-Id")
		mu.Lock()
		seen[id]++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := newWorkerFixture(t)
	svc := f.addService(t, "alpha", srv.URL, "tok", 6)

	const deliveries = 40
	for range deliveries {
		f.queue(t, svc, randomBody())
	}

	// Eight workers racing over the same queue.
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for range 10 {
				if _, err := f.dispatcher.claimAndDeliver(f.ctx, "worker-"+string(rune('a'+n))); err != nil {
					t.Errorf("claimAndDeliver: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != deliveries {
		t.Errorf("delivered %d distinct rows, want %d", len(seen), deliveries)
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("delivery %s was sent %d times, want exactly 1", id, count)
		}
	}
}

// A worker that dies mid-flight leaves a row claimed; the reaper must return
// it to the queue rather than leave it stuck forever.
func TestReaperRequeuesStuckDeliveries(t *testing.T) {
	f := newWorkerFixture(t)
	svc := f.addService(t, "alpha", "http://unused.test/hook", "tok", 6)
	_, deliveryID := f.queue(t, svc, `{"a":1}`)

	// Simulate a crashed worker: claimed long ago, never completed.
	if _, err := f.store.Pool().Exec(f.ctx, `
		UPDATE deliveries
		   SET status='in_flight', claimed_at = now() - interval '10 minutes',
		       claimed_by='dead-worker'
		 WHERE id=$1`, deliveryID); err != nil {
		t.Fatalf("simulate crash: %v", err)
	}

	n, err := f.store.ReapStuckDeliveries(f.ctx, ReapAfter)
	if err != nil {
		t.Fatalf("ReapStuckDeliveries: %v", err)
	}
	if n != 1 {
		t.Fatalf("reaped %d deliveries, want 1", n)
	}
	if d := f.delivery(t, deliveryID); d.Status != "pending" {
		t.Errorf("status = %q, want pending after reaping", d.Status)
	}
}

// A recently claimed row must be left alone: reaping it would double-send a
// delivery that is still in flight.
func TestReaperLeavesFreshClaimsAlone(t *testing.T) {
	f := newWorkerFixture(t)
	svc := f.addService(t, "alpha", "http://unused.test/hook", "tok", 6)
	_, deliveryID := f.queue(t, svc, `{"a":1}`)

	if _, err := f.store.Pool().Exec(f.ctx, `
		UPDATE deliveries SET status='in_flight', claimed_at=now(), claimed_by='busy'
		 WHERE id=$1`, deliveryID); err != nil {
		t.Fatalf("claim: %v", err)
	}

	n, err := f.store.ReapStuckDeliveries(f.ctx, ReapAfter)
	if err != nil {
		t.Fatalf("ReapStuckDeliveries: %v", err)
	}
	if n != 0 {
		t.Errorf("reaped %d fresh claims, want 0", n)
	}
}

func TestRetryDeliveryRequeues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad", http.StatusBadRequest)
	}))
	defer srv.Close()

	f := newWorkerFixture(t)
	svc := f.addService(t, "alpha", srv.URL, "tok", 6)
	_, deliveryID := f.queue(t, svc, `{"a":1}`)

	f.runOnce(t)
	if d := f.delivery(t, deliveryID); d.Status != "failed" {
		t.Fatalf("status = %q, want failed", d.Status)
	}

	if err := f.store.RetryDelivery(f.ctx, deliveryID); err != nil {
		t.Fatalf("RetryDelivery: %v", err)
	}
	if d := f.delivery(t, deliveryID); d.Status != "pending" {
		t.Errorf("status = %q, want pending after an operator retry", d.Status)
	}
}

func TestRateLimiterIsPerService(t *testing.T) {
	f := newWorkerFixture(t)

	a := f.dispatcher.limiter(1, 10)
	b := f.dispatcher.limiter(1, 10)
	if a != b {
		t.Error("limiter() returned different limiters for the same service")
	}
	if c := f.dispatcher.limiter(2, 10); c == a {
		t.Error("two services share one limiter")
	}
	if f.dispatcher.limiter(3, 0) != nil {
		t.Error("rate_limit_rps=0 should mean unlimited (nil limiter)")
	}
	// A reconfigured service must pick up its new rate.
	if d := f.dispatcher.limiter(1, 50); d == a {
		t.Error("limiter was not rebuilt after the rate changed")
	}
}

func TestClientIsSharedPerService(t *testing.T) {
	f := newWorkerFixture(t)
	a := f.dispatcher.client(1, 5000)
	if b := f.dispatcher.client(1, 5000); a != b {
		t.Error("client() built a new client for the same service; connection pooling would be lost")
	}
	if c := f.dispatcher.client(2, 5000); c == a {
		t.Error("two services share one client")
	}
}

var bodyCounter atomic.Int64

// randomBody keeps each event distinct so the dedupe index does not collapse
// them into one row.
func randomBody() string {
	n := bodyCounter.Add(1)
	b, _ := json.Marshal(map[string]any{"seq": n, "entry": []any{map[string]string{"id": "K"}}})
	return string(b)
}
