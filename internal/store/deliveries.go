package store

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// Delivery is one attempt-set of an event to a service.
type Delivery struct {
	ID         int64  `json:"id"`
	EventID    int64  `json:"event_id"`
	ServiceID  int64  `json:"-"`
	ServicePub string `json:"service_id"`

	Status       string     `json:"status"`
	AttemptCount int        `json:"attempt_count"`
	NextAttempt  time.Time  `json:"next_attempt_at"`
	ClaimedAt    *time.Time `json:"claimed_at,omitempty"`
	ClaimedBy    string     `json:"claimed_by,omitempty"`

	LastStatusCode   *int   `json:"last_status_code,omitempty"`
	LastResponseBody string `json:"last_response_body,omitempty"`
	LastError        string `json:"last_error,omitempty"`
	LatencyMS        *int   `json:"latency_ms,omitempty"`

	MatchedSubscriptionIDs []int64    `json:"matched_subscription_ids"`
	CreatedAt              time.Time  `json:"created_at"`
	CompletedAt            *time.Time `json:"completed_at,omitempty"`
}

// ClaimedDelivery carries everything a worker needs to perform one attempt,
// joined from the delivery, its event, and its target service.
type ClaimedDelivery struct {
	DeliveryID   int64
	EventID      int64
	AttemptCount int

	ServiceID     int64
	ServicePubID  string
	ServiceName   string
	URL           string
	Method        string
	LinkToken     []byte // still encrypted
	TimeoutMS     int
	MaxAttempts   int
	RateLimitRPS  int
	CustomHeaders map[string]string

	RawBody           []byte
	ListenerSlug      string
	OriginalSignature string
}

// ClaimDeliveries atomically claims a batch of due deliveries.
//
// The locking SELECT and the UPDATE are one statement via a CTE. The
// `WHERE id IN (SELECT ... FOR UPDATE SKIP LOCKED)` form looks equivalent but
// is not: the outer UPDATE re-locks those ids and blocks on a concurrent
// worker rather than skipping past it, so workers serialise under load.
//
// ORDER BY inside the locking SELECT keeps claim order stable and avoids
// lock-order inversion between workers.
func (s *Store) ClaimDeliveries(ctx context.Context, workerID string, limit int) ([]ClaimedDelivery, error) {
	rows, err := s.pool.Query(ctx, `
		WITH claimed AS (
			SELECT id FROM deliveries
			 WHERE status = 'pending' AND next_attempt_at <= now()
			 ORDER BY next_attempt_at
			 FOR UPDATE SKIP LOCKED
			 LIMIT $2
		)
		UPDATE deliveries d
		   SET status = 'in_flight',
		       attempt_count = d.attempt_count + 1,
		       claimed_at = now(),
		       claimed_by = $1
		  FROM claimed c
		 WHERE d.id = c.id
		RETURNING d.id, d.event_id, d.attempt_count, d.service_id`, workerID, limit)
	if err != nil {
		return nil, err
	}

	type claim struct {
		deliveryID, eventID int64
		attempt             int
		serviceID           int64
	}
	var claims []claim
	for rows.Next() {
		var c claim
		if err := rows.Scan(&c.deliveryID, &c.eventID, &c.attempt, &c.serviceID); err != nil {
			rows.Close()
			return nil, err
		}
		claims = append(claims, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(claims) == 0 {
		return nil, nil
	}

	// Hydrate the claims with event and service detail. Done as a second query
	// so the claiming UPDATE stays as short as possible — it holds row locks.
	ids := make([]int64, len(claims))
	for i, c := range claims {
		ids[i] = c.deliveryID
	}

	detailRows, err := s.pool.Query(ctx, `
		SELECT d.id, d.event_id, d.attempt_count,
		       svc.id, svc.public_id, svc.name, svc.url, svc.method, svc.link_token,
		       svc.timeout_ms, svc.max_attempts, svc.rate_limit_rps, svc.custom_headers,
		       e.raw_body, l.slug,
		       COALESCE(e.headers->>'X-Hub-Signature-256', '')
		  FROM deliveries d
		  JOIN services  svc ON svc.id = d.service_id
		  JOIN events    e   ON e.id   = d.event_id
		  JOIN listeners l   ON l.id   = e.listener_id
		 WHERE d.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer detailRows.Close()

	var out []ClaimedDelivery
	for detailRows.Next() {
		var c ClaimedDelivery
		if err := detailRows.Scan(&c.DeliveryID, &c.EventID, &c.AttemptCount,
			&c.ServiceID, &c.ServicePubID, &c.ServiceName, &c.URL, &c.Method,
			&c.LinkToken, &c.TimeoutMS, &c.MaxAttempts, &c.RateLimitRPS,
			&c.CustomHeaders, &c.RawBody, &c.ListenerSlug, &c.OriginalSignature); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, detailRows.Err()
}

// DeliveryOutcome records the result of one attempt.
type DeliveryOutcome struct {
	DeliveryID   int64
	Success      bool
	Terminal     bool // a 4xx that will never succeed: stop retrying
	StatusCode   *int
	ResponseBody string
	Error        string
	LatencyMS    int
	NextAttempt  time.Time // zero when there is no further attempt
}

// CompleteDelivery writes an attempt's result and advances the delivery's
// state machine, adjusting the target service's circuit breaker in the same
// transaction so the counter can never drift from reality.
func (s *Store) CompleteDelivery(ctx context.Context, serviceID int64, o DeliveryOutcome, breakerThreshold int) (tripped bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.Background())

	switch {
	case o.Success:
		_, err = tx.Exec(ctx, `
			UPDATE deliveries
			   SET status='success', last_status_code=$2, last_response_body=$3,
			       last_error=NULL, latency_ms=$4, completed_at=now(), claimed_by=NULL
			 WHERE id=$1`,
			o.DeliveryID, o.StatusCode, nullIfEmpty(o.ResponseBody), o.LatencyMS)

	case o.Terminal:
		// A 400 will still be a 400 on attempt six.
		_, err = tx.Exec(ctx, `
			UPDATE deliveries
			   SET status='failed', last_status_code=$2, last_response_body=$3,
			       last_error=$4, latency_ms=$5, completed_at=now(), claimed_by=NULL
			 WHERE id=$1`,
			o.DeliveryID, o.StatusCode, nullIfEmpty(o.ResponseBody),
			nullIfEmpty(o.Error), o.LatencyMS)

	case o.NextAttempt.IsZero():
		// Attempts exhausted.
		_, err = tx.Exec(ctx, `
			UPDATE deliveries
			   SET status='dead', last_status_code=$2, last_response_body=$3,
			       last_error=$4, latency_ms=$5, completed_at=now(), claimed_by=NULL
			 WHERE id=$1`,
			o.DeliveryID, o.StatusCode, nullIfEmpty(o.ResponseBody),
			nullIfEmpty(o.Error), o.LatencyMS)

	default:
		_, err = tx.Exec(ctx, `
			UPDATE deliveries
			   SET status='pending', next_attempt_at=$2, last_status_code=$3,
			       last_response_body=$4, last_error=$5, latency_ms=$6, claimed_by=NULL
			 WHERE id=$1`,
			o.DeliveryID, o.NextAttempt, o.StatusCode, nullIfEmpty(o.ResponseBody),
			nullIfEmpty(o.Error), o.LatencyMS)
	}
	if err != nil {
		return false, err
	}

	// The breaker counter lives on the row rather than in process memory: with
	// several replicas an in-memory counter would need threshold*N real
	// failures to trip, and would reset on every deploy.
	if o.Success {
		_, err = tx.Exec(ctx,
			`UPDATE services SET consecutive_failures = 0 WHERE id = $1`, serviceID)
		if err != nil {
			return false, err
		}
		return false, tx.Commit(ctx)
	}

	var failures int
	if err := tx.QueryRow(ctx, `
		UPDATE services SET consecutive_failures = consecutive_failures + 1
		 WHERE id = $1
		RETURNING consecutive_failures`, serviceID).Scan(&failures); err != nil {
		return false, err
	}

	if failures >= breakerThreshold {
		// Disable rather than keep hammering a service that is clearly down.
		// Re-enabling is a deliberate operator action.
		if _, err := tx.Exec(ctx, `
			UPDATE services
			   SET status='disabled', disabled_at=now(),
			       disabled_reason=$2
			 WHERE id=$1 AND status <> 'disabled'`,
			serviceID, "circuit breaker: "+strconv.Itoa(failures)+" consecutive failures"); err != nil {
			return false, err
		}
		tripped = true
	}
	return tripped, tx.Commit(ctx)
}

// ReapStuckDeliveries returns deliveries abandoned by a crashed worker to the
// queue.
//
// The timeout must comfortably exceed any service's request timeout plus its
// Retry-After sleep, or a slow-but-alive delivery would be sent twice.
func (s *Store) ReapStuckDeliveries(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE deliveries
		   SET status='pending', claimed_by=NULL,
		       last_error='reclaimed: worker did not report a result'
		 WHERE status='in_flight'
		   AND claimed_at < now() - $1::interval`,
		olderThan.String())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DeliveriesForEvent lists a single event's deliveries.
func (s *Store) DeliveriesForEvent(ctx context.Context, eventID int64) ([]*Delivery, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT d.id, d.event_id, d.service_id, svc.public_id, d.status,
		       d.attempt_count, d.next_attempt_at, d.claimed_at,
		       COALESCE(d.claimed_by,''), d.last_status_code,
		       COALESCE(d.last_response_body,''), COALESCE(d.last_error,''),
		       d.latency_ms, d.matched_subscription_ids, d.created_at, d.completed_at
		  FROM deliveries d JOIN services svc ON svc.id = d.service_id
		 WHERE d.event_id = $1
		 ORDER BY d.id`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Delivery
	for rows.Next() {
		var d Delivery
		if err := rows.Scan(&d.ID, &d.EventID, &d.ServiceID, &d.ServicePub, &d.Status,
			&d.AttemptCount, &d.NextAttempt, &d.ClaimedAt, &d.ClaimedBy,
			&d.LastStatusCode, &d.LastResponseBody, &d.LastError, &d.LatencyMS,
			&d.MatchedSubscriptionIDs, &d.CreatedAt, &d.CompletedAt); err != nil {
			return nil, err
		}
		out = append(out, &d)
	}
	return out, rows.Err()
}

// RetryDelivery requeues one delivery immediately, for the UI's retry button.
func (s *Store) RetryDelivery(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE deliveries
		   SET status='pending', next_attempt_at=now(), completed_at=NULL,
		       last_error=NULL
		 WHERE id=$1 AND status IN ('failed','dead','success')`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CountPendingDeliveries reports the queue depth.
func (s *Store) CountPendingDeliveries(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM deliveries WHERE status IN ('pending','in_flight')`).Scan(&n)
	return n, err
}

// ServiceDisabled reports whether a service has been disabled by the breaker.
func (s *Store) ServiceDisabled(ctx context.Context, serviceID int64) (bool, error) {
	var disabled bool
	err := s.pool.QueryRow(ctx,
		`SELECT status = 'disabled' FROM services WHERE id = $1`, serviceID).Scan(&disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	return disabled, err
}
