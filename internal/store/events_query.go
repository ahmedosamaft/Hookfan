package store

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// EventFilter narrows an event listing.
type EventFilter struct {
	ListenerID *int64
	RoutingKey string
	Since      *time.Time
	Until      *time.Time
	SigValid   *bool
	// Cursor is an opaque token from a previous page.
	Cursor string
	Limit  int
}

// EventSummary is one row of the events list. The raw body is deliberately
// absent: a listing must not stream megabytes of payload, and the table only
// shows a delivery summary.
type EventSummary struct {
	ID             int64     `json:"id"`
	ListenerID     int64     `json:"listener_id"`
	ListenerSlug   string    `json:"listener_slug"`
	RoutingKeys    []string  `json:"routing_keys"`
	ContentType    string    `json:"content_type,omitempty"`
	ReceivedAt     time.Time `json:"received_at"`
	SignatureValid bool      `json:"signature_valid"`
	Planned        bool      `json:"planned"`
	BodyBytes      int       `json:"body_bytes"`

	// Delivery rollup, so the table can show "2/3 delivered" without a
	// second round trip per row.
	DeliveryTotal   int `json:"delivery_total"`
	DeliverySuccess int `json:"delivery_success"`
	DeliveryPending int `json:"delivery_pending"`
	DeliveryFailed  int `json:"delivery_failed"`
	DeliveryDead    int `json:"delivery_dead"`
}

// EventPage is one page of results plus the cursor for the next.
type EventPage struct {
	Events     []*EventSummary `json:"events"`
	NextCursor string          `json:"next_cursor,omitempty"`
	HasMore    bool            `json:"has_more"`
}

// DefaultEventLimit and MaxEventLimit bound a page.
const (
	DefaultEventLimit = 50
	MaxEventLimit     = 200
)

// ListEvents returns a page of events, newest first.
//
// Pagination is by cursor rather than OFFSET: events arrive continuously, and
// an offset-paginated list shifts under the reader — rows are skipped or
// repeated as new events land. A cursor is a stable position in the id
// sequence, so paging is consistent regardless of what arrives meanwhile.
func (s *Store) ListEvents(ctx context.Context, f EventFilter) (*EventPage, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultEventLimit
	}
	if limit > MaxEventLimit {
		limit = MaxEventLimit
	}

	var beforeID *int64
	if f.Cursor != "" {
		id, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, err
		}
		beforeID = &id
	}

	var routingKey *string
	if f.RoutingKey != "" {
		routingKey = &f.RoutingKey
	}

	// One extra row is fetched to detect whether a further page exists,
	// without a second COUNT query.
	rows, err := s.pool.Query(ctx, `
		SELECT e.id, e.listener_id, l.slug, e.routing_keys,
		       COALESCE(e.content_type,''), e.received_at, e.signature_valid,
		       e.planned_at IS NOT NULL, length(e.raw_body),
		       COALESCE(d.total,0), COALESCE(d.success,0), COALESCE(d.pending,0),
		       COALESCE(d.failed,0), COALESCE(d.dead,0)
		  FROM events e
		  JOIN listeners l ON l.id = e.listener_id
		  LEFT JOIN LATERAL (
		        SELECT count(*) AS total,
		               count(*) FILTER (WHERE status = 'success') AS success,
		               count(*) FILTER (WHERE status IN ('pending','in_flight')) AS pending,
		               count(*) FILTER (WHERE status = 'failed') AS failed,
		               count(*) FILTER (WHERE status = 'dead') AS dead
		          FROM deliveries WHERE event_id = e.id
		  ) d ON true
		 WHERE ($1::bigint IS NULL OR e.listener_id = $1)
		   AND ($2::text   IS NULL OR e.routing_keys @> ARRAY[$2::text])
		   AND ($3::timestamptz IS NULL OR e.received_at >= $3)
		   AND ($4::timestamptz IS NULL OR e.received_at <= $4)
		   AND ($5::boolean IS NULL OR e.signature_valid = $5)
		   AND ($6::bigint IS NULL OR e.id < $6)
		 ORDER BY e.id DESC
		 LIMIT $7`,
		f.ListenerID, routingKey, f.Since, f.Until, f.SigValid, beforeID, limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	page := &EventPage{Events: []*EventSummary{}}
	for rows.Next() {
		var e EventSummary
		if err := rows.Scan(&e.ID, &e.ListenerID, &e.ListenerSlug, &e.RoutingKeys,
			&e.ContentType, &e.ReceivedAt, &e.SignatureValid, &e.Planned, &e.BodyBytes,
			&e.DeliveryTotal, &e.DeliverySuccess, &e.DeliveryPending,
			&e.DeliveryFailed, &e.DeliveryDead); err != nil {
			return nil, err
		}
		page.Events = append(page.Events, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(page.Events) > limit {
		page.Events = page.Events[:limit]
		page.HasMore = true
		page.NextCursor = encodeCursor(page.Events[limit-1].ID)
	}
	return page, nil
}

// encodeCursor renders a position as an opaque token. It is deliberately not
// a bare id: an opaque cursor can gain fields later without breaking clients
// that treat it as a value to echo back.
func encodeCursor(id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte("e:" + strconv.FormatInt(id, 10)))
}

func decodeCursor(cursor string) (int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, fmt.Errorf("invalid cursor")
	}
	value, ok := strings.CutPrefix(string(raw), "e:")
	if !ok {
		return 0, fmt.Errorf("invalid cursor")
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid cursor")
	}
	return id, nil
}

// Stats is the dashboard rollup.
type Stats struct {
	Windows  map[string]*WindowStats `json:"windows"`
	Services []*ServiceHealth        `json:"services"`
	Queue    QueueStats              `json:"queue"`
}

type WindowStats struct {
	Events           int     `json:"events"`
	EventsInvalidSig int     `json:"events_invalid_signature"`
	Deliveries       int     `json:"deliveries"`
	Success          int     `json:"success"`
	Failed           int     `json:"failed"`
	Dead             int     `json:"dead"`
	Pending          int     `json:"pending"`
	SuccessRate      float64 `json:"success_rate"`
}

type ServiceHealth struct {
	PublicID            string     `json:"id"`
	Name                string     `json:"name"`
	Status              string     `json:"status"`
	Enabled             bool       `json:"enabled"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	DisabledAt          *time.Time `json:"disabled_at,omitempty"`
	DisabledReason      string     `json:"disabled_reason,omitempty"`
	Success24h          int        `json:"success_24h"`
	Failed24h           int        `json:"failed_24h"`
	AvgLatencyMS        *int       `json:"avg_latency_ms,omitempty"`
}

type QueueStats struct {
	PendingDeliveries int     `json:"pending_deliveries"`
	UnplannedEvents   int     `json:"unplanned_events"`
	PlannerLagSeconds float64 `json:"planner_lag_seconds"`
}

// statsWindows are the reporting periods.
var statsWindows = []struct {
	Name     string
	Interval string
}{
	{"1h", "1 hour"},
	{"24h", "24 hours"},
	{"7d", "7 days"},
}

// GetStats computes the dashboard rollup.
func (s *Store) GetStats(ctx context.Context) (*Stats, error) {
	stats := &Stats{Windows: map[string]*WindowStats{}}

	for _, w := range statsWindows {
		var ws WindowStats
		err := s.pool.QueryRow(ctx, `
			SELECT
			  (SELECT count(*) FROM events
			    WHERE received_at >= now() - $1::interval),
			  (SELECT count(*) FROM events
			    WHERE received_at >= now() - $1::interval AND NOT signature_valid),
			  (SELECT count(*) FROM deliveries
			    WHERE created_at >= now() - $1::interval),
			  (SELECT count(*) FROM deliveries
			    WHERE created_at >= now() - $1::interval AND status = 'success'),
			  (SELECT count(*) FROM deliveries
			    WHERE created_at >= now() - $1::interval AND status = 'failed'),
			  (SELECT count(*) FROM deliveries
			    WHERE created_at >= now() - $1::interval AND status = 'dead'),
			  (SELECT count(*) FROM deliveries
			    WHERE created_at >= now() - $1::interval AND status IN ('pending','in_flight'))`,
			w.Interval).Scan(&ws.Events, &ws.EventsInvalidSig, &ws.Deliveries,
			&ws.Success, &ws.Failed, &ws.Dead, &ws.Pending)
		if err != nil {
			return nil, fmt.Errorf("stats for window %s: %w", w.Name, err)
		}
		// Rate over settled deliveries only: counting still-pending ones as
		// failures would make a healthy backlog look like an outage.
		settled := ws.Success + ws.Failed + ws.Dead
		if settled > 0 {
			ws.SuccessRate = float64(ws.Success) / float64(settled)
		}
		stats.Windows[w.Name] = &ws
	}

	rows, err := s.pool.Query(ctx, `
		SELECT svc.public_id, svc.name, svc.status, svc.enabled,
		       svc.consecutive_failures, svc.disabled_at,
		       COALESCE(svc.disabled_reason,''),
		       COALESCE(d.success,0), COALESCE(d.failed,0), d.avg_latency
		  FROM services svc
		  LEFT JOIN LATERAL (
		        SELECT count(*) FILTER (WHERE status = 'success') AS success,
		               count(*) FILTER (WHERE status IN ('failed','dead')) AS failed,
		               avg(latency_ms) FILTER (WHERE status = 'success')::int AS avg_latency
		          FROM deliveries
		         WHERE service_id = svc.id AND created_at >= now() - interval '24 hours'
		  ) d ON true
		 ORDER BY svc.name`)
	if err != nil {
		return nil, fmt.Errorf("service health: %w", err)
	}
	defer rows.Close()

	stats.Services = []*ServiceHealth{}
	for rows.Next() {
		var h ServiceHealth
		if err := rows.Scan(&h.PublicID, &h.Name, &h.Status, &h.Enabled,
			&h.ConsecutiveFailures, &h.DisabledAt, &h.DisabledReason,
			&h.Success24h, &h.Failed24h, &h.AvgLatencyMS); err != nil {
			return nil, err
		}
		stats.Services = append(stats.Services, &h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if err := s.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM deliveries WHERE status IN ('pending','in_flight')),
		  (SELECT count(*) FROM events WHERE planned_at IS NULL),
		  (SELECT COALESCE(EXTRACT(EPOCH FROM (now() - MIN(received_at))), 0)
		     FROM events WHERE planned_at IS NULL)`).
		Scan(&stats.Queue.PendingDeliveries, &stats.Queue.UnplannedEvents,
			&stats.Queue.PlannerLagSeconds); err != nil {
		return nil, fmt.Errorf("queue stats: %w", err)
	}
	return stats, nil
}

// EventsPerMinute returns a time series for the dashboard sparkline.
func (s *Store) EventsPerMinute(ctx context.Context, minutes int) ([]TimeBucket, error) {
	if minutes <= 0 || minutes > 1440 {
		minutes = 60
	}
	// generate_series fills gaps, so quiet minutes appear as zero rather than
	// being missing from the series and distorting the shape.
	rows, err := s.pool.Query(ctx, `
		SELECT g.bucket, COALESCE(count(e.id), 0)
		  FROM generate_series(
		         date_trunc('minute', now()) - make_interval(mins => $1 - 1),
		         date_trunc('minute', now()),
		         interval '1 minute') AS g(bucket)
		  LEFT JOIN events e
		         ON date_trunc('minute', e.received_at) = g.bucket
		 GROUP BY g.bucket
		 ORDER BY g.bucket`, minutes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TimeBucket
	for rows.Next() {
		var b TimeBucket
		if err := rows.Scan(&b.At, &b.Count); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

type TimeBucket struct {
	At    time.Time `json:"at"`
	Count int       `json:"count"`
}

// ReplayEvent clears the event's planning state so the planner rebuilds its
// delivery set from the subscriptions in force now.
//
// Existing deliveries are removed first: without that, the unique
// (event_id, service_id) index would make re-planning a no-op, and a replay
// would silently do nothing.
func (s *Store) ReplayEvent(ctx context.Context, eventID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())

	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT true FROM events WHERE id = $1`, eventID).Scan(&exists); err != nil {
		return ErrNotFound
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM deliveries WHERE event_id = $1`, eventID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE events SET planned_at = NULL WHERE id = $1`, eventID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
