package dispatch

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/aosama/hookfan/internal/crypto"
	"github.com/aosama/hookfan/internal/store"
	"golang.org/x/time/rate"
)

const (
	// BreakerThreshold is how many consecutive failures disable a service.
	BreakerThreshold = 20

	// ClaimBatchSize is how many deliveries one worker claims per pass.
	ClaimBatchSize = 8

	// PollInterval bounds how long a due delivery waits when no notification
	// arrives.
	PollInterval = time.Second

	// ReapInterval is how often abandoned in-flight rows are returned to the
	// queue, and ReapAfter is how stale a claim must be to qualify. ReapAfter
	// is well above any per-service timeout so a slow-but-alive delivery is
	// never reclaimed underneath its worker.
	ReapInterval = 30 * time.Second
	ReapAfter    = 5 * time.Minute

	// MaxResponseBody is how much of a service's response is retained.
	MaxResponseBody = 4 << 10
)

// Publisher receives delivery state changes for the live UI feed. Declared
// here rather than imported so this package stays independent of the admin API.
type Publisher interface {
	Publish(kind string, data any)
}

// Dispatcher runs the delivery worker pool.
type Dispatcher struct {
	Store  *store.Store
	Cipher *crypto.Cipher
	Guard  *SSRFGuard
	Log    *slog.Logger
	// Stream is optional; delivery never depends on a UI listening.
	Stream Publisher

	Concurrency int

	// clients and limiters are per-service and long-lived: a fresh
	// http.Client per request would discard connection pooling entirely.
	mu       sync.Mutex
	clients  map[int64]*http.Client
	limiters map[int64]*rate.Limiter
	// limiterRPS tracks the rate each limiter was built with, so a
	// reconfigured service picks up its new limit.
	limiterRPS map[int64]int

	wake chan struct{}
}

func NewDispatcher(st *store.Store, cipher *crypto.Cipher, guard *SSRFGuard, log *slog.Logger, concurrency int) *Dispatcher {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Dispatcher{
		Store:       st,
		Cipher:      cipher,
		Guard:       guard,
		Log:         log,
		Concurrency: concurrency,
		clients:     map[int64]*http.Client{},
		limiters:    map[int64]*rate.Limiter{},
		limiterRPS:  map[int64]int{},
		wake:        make(chan struct{}, 1),
	}
}

// Notify wakes a worker. Never blocks: a pending wake-up already covers this.
func (d *Dispatcher) Notify() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Run starts the worker pool and the reaper, returning when ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context) {
	d.Log.Info("dispatcher started",
		"workers", d.Concurrency,
		"breaker_threshold", BreakerThreshold)

	var wg sync.WaitGroup
	for i := range d.Concurrency {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			d.worker(ctx, fmt.Sprintf("worker-%d", id))
		}(i)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		d.reaper(ctx)
	}()

	wg.Wait()
	d.Log.Info("dispatcher stopped")
}

func (d *Dispatcher) worker(ctx context.Context, workerID string) {
	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()

	for {
		// Drain: keep claiming while full batches come back, so a backlog is
		// not metered out one tick at a time.
		for {
			n, err := d.claimAndDeliver(ctx, workerID)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				d.Log.Error("delivery pass failed", "worker", workerID, "error", err)
				break
			}
			if n < ClaimBatchSize {
				break
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-d.wake:
		case <-ticker.C:
		}
	}
}

func (d *Dispatcher) claimAndDeliver(ctx context.Context, workerID string) (int, error) {
	claims, err := d.Store.ClaimDeliveries(ctx, workerID, ClaimBatchSize)
	if err != nil {
		return 0, err
	}
	for _, c := range claims {
		if ctx.Err() != nil {
			return len(claims), nil
		}
		d.deliver(ctx, c)
	}
	return len(claims), nil
}

// reaper returns deliveries abandoned by a crashed worker to the queue.
// Without it, a row stuck in_flight would never be retried.
func (d *Dispatcher) reaper(ctx context.Context) {
	ticker := time.NewTicker(ReapInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := d.Store.ReapStuckDeliveries(ctx, ReapAfter)
			if err != nil {
				if ctx.Err() == nil {
					d.Log.Error("reaper failed", "error", err)
				}
				continue
			}
			if n > 0 {
				d.Log.Warn("reclaimed stuck deliveries",
					"count", n, "stale_after", ReapAfter)
				d.Notify()
			}
		}
	}
}

// deliver performs one attempt and records the outcome.
func (d *Dispatcher) deliver(ctx context.Context, c store.ClaimedDelivery) {
	token, err := d.Cipher.DecryptString(c.LinkToken)
	if err != nil {
		// Not retryable: the key changed, and every attempt would fail the
		// same way.
		d.Log.Error("could not decrypt link token",
			"service", c.ServiceName, "delivery_id", c.DeliveryID, "error", err)
		d.complete(ctx, c, store.DeliveryOutcome{
			DeliveryID: c.DeliveryID,
			Terminal:   true,
			Error:      "could not decrypt link token; has SECRET_ENCRYPTION_KEY changed?",
		})
		return
	}

	// Rate limit before dialling, so a limited service is paced rather than
	// hammered and then retried.
	if lim := d.limiter(c.ServiceID, c.RateLimitRPS); lim != nil {
		waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := lim.Wait(waitCtx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return // shutting down; the reaper will requeue this row
			}
			d.Log.Warn("rate limiter wait failed",
				"service", c.ServiceName, "error", err)
		}
	}

	req, err := d.buildRequest(ctx, c, token)
	if err != nil {
		d.complete(ctx, c, store.DeliveryOutcome{
			DeliveryID: c.DeliveryID, Terminal: true,
			Error: "could not build request: " + err.Error(),
		})
		return
	}

	client := d.client(c.ServiceID, c.TimeoutMS)
	start := time.Now()
	resp, doErr := client.Do(req)
	latency := time.Since(start)

	var statusCode *int
	var respBody string
	var header http.Header
	if doErr == nil {
		defer resp.Body.Close()
		header = resp.Header
		statusCode = &resp.StatusCode
		if b, readErr := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBody)); readErr == nil {
			respBody = string(b)
		}
	}

	code := 0
	if statusCode != nil {
		code = *statusCode
	}
	decision := Classify(code, doErr, header, c.AttemptCount, c.MaxAttempts)

	outcome := store.DeliveryOutcome{
		DeliveryID:   c.DeliveryID,
		Success:      decision.Success,
		Terminal:     decision.Terminal,
		StatusCode:   statusCode,
		ResponseBody: respBody,
		Error:        decision.Reason,
		LatencyMS:    int(latency.Milliseconds()),
	}
	if decision.Success {
		outcome.Error = ""
	}
	if decision.Retry {
		outcome.NextAttempt = time.Now().Add(decision.Delay)
	}

	switch {
	case decision.Success:
		d.Log.Info("delivered",
			"delivery_id", c.DeliveryID, "service", c.ServiceName,
			"event_id", c.EventID, "status", code,
			"attempt", c.AttemptCount, "latency_ms", latency.Milliseconds())
	case decision.Retry:
		d.Log.Warn("delivery failed, retrying",
			"delivery_id", c.DeliveryID, "service", c.ServiceName,
			"attempt", c.AttemptCount, "of", c.MaxAttempts,
			"retry_in", decision.Delay.Round(time.Millisecond),
			"reason", decision.Reason)
	default:
		d.Log.Error("delivery gave up",
			"delivery_id", c.DeliveryID, "service", c.ServiceName,
			"attempt", c.AttemptCount, "terminal", decision.Terminal,
			"reason", decision.Reason)
	}

	d.complete(ctx, c, outcome)
}

func (d *Dispatcher) complete(ctx context.Context, c store.ClaimedDelivery, o store.DeliveryOutcome) {
	// context.Background: the result must be recorded even during shutdown,
	// or the row is left in_flight for the reaper to rediscover.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	tripped, err := d.Store.CompleteDelivery(writeCtx, c.ServiceID, o, BreakerThreshold)
	if err != nil {
		d.Log.Error("could not record delivery outcome",
			"delivery_id", c.DeliveryID, "error", err)
		return
	}

	if d.Stream != nil {
		status := "pending"
		switch {
		case o.Success:
			status = "success"
		case o.Terminal:
			status = "failed"
		case o.NextAttempt.IsZero():
			status = "dead"
		}
		d.Stream.Publish("delivery.updated", map[string]any{
			"id":               c.DeliveryID,
			"event_id":         c.EventID,
			"service_id":       c.ServicePubID,
			"service_name":     c.ServiceName,
			"status":           status,
			"attempt_count":    c.AttemptCount,
			"last_status_code": o.StatusCode,
			"latency_ms":       o.LatencyMS,
			"last_error":       o.Error,
		})
	}

	if tripped {
		d.Log.Error("circuit breaker tripped; service disabled",
			"service", c.ServiceName, "service_id", c.ServicePubID,
			"consecutive_failures", BreakerThreshold,
			"action", "re-enable manually once the service is healthy")
		if d.Stream != nil {
			// The UI shows a banner for a tripped service, so this must not
			// wait for the next stats poll.
			d.Stream.Publish("service.updated", map[string]any{
				"id":                   c.ServicePubID,
				"name":                 c.ServiceName,
				"status":               "disabled",
				"consecutive_failures": BreakerThreshold,
				"disabled_reason":      "circuit breaker: 20 consecutive failures",
			})
		}
	}
}

// buildRequest constructs the forwarded request.
//
// The body is the original bytes, unchanged: that is what makes the provider's
// own signature still verify downstream.
func (d *Dispatcher) buildRequest(ctx context.Context, c store.ClaimedDelivery, token string) (*http.Request, error) {
	method := c.Method
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL, bytes.NewReader(c.RawBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hookfan-Event", "event.forward")
	req.Header.Set("X-Hookfan-Event-Id", strconv.FormatInt(c.EventID, 10))
	req.Header.Set("X-Hookfan-Delivery-Id", strconv.FormatInt(c.DeliveryID, 10))
	req.Header.Set("X-Hookfan-Listener", c.ListenerSlug)
	req.Header.Set("X-Hookfan-Attempt", strconv.Itoa(c.AttemptCount))
	req.Header.Set("X-Hookfan-Token", token)
	// Signed over the same bytes the service receives, so it can authenticate
	// the gateway on every request rather than only at link time.
	req.Header.Set("X-Hookfan-Signature", "sha256="+crypto.SignHex([]byte(token), c.RawBody))
	if c.OriginalSignature != "" {
		// Still valid: the payload was never split or re-marshalled.
		req.Header.Set("X-Hookfan-Original-Signature", c.OriginalSignature)
	}

	// Applied last so an operator can override anything above.
	for k, v := range c.CustomHeaders {
		req.Header.Set(k, v)
	}
	return req, nil
}

// client returns the shared http.Client for a service, creating it on first
// use. One client per service keeps connections pooled across deliveries.
func (d *Dispatcher) client(serviceID int64, timeoutMS int) *http.Client {
	d.mu.Lock()
	defer d.mu.Unlock()

	if c, ok := d.clients[serviceID]; ok {
		return c
	}
	timeout := time.Duration(timeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	c := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:           d.Guard.DialContext,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   32,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
			ForceAttemptHTTP2:     true,
		},
		// A webhook target must handle the payload itself, not redirect it.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	d.clients[serviceID] = c
	return c
}

// limiter returns the token bucket for a service, or nil when unlimited.
func (d *Dispatcher) limiter(serviceID int64, rps int) *rate.Limiter {
	if rps <= 0 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if l, ok := d.limiters[serviceID]; ok && d.limiterRPS[serviceID] == rps {
		return l
	}
	// Burst equal to the rate lets a quiet service absorb a small spike
	// without pacing every single request.
	l := rate.NewLimiter(rate.Limit(rps), rps)
	d.limiters[serviceID] = l
	d.limiterRPS[serviceID] = rps
	return l
}
