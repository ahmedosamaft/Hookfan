package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type Event struct {
	ID             int64             `json:"id"`
	ListenerID     int64             `json:"listener_id"`
	RoutingKeys    []string          `json:"routing_keys"`
	RawBody        []byte            `json:"-"`
	Headers        map[string]string `json:"headers,omitempty"`
	ContentType    string            `json:"content_type,omitempty"`
	ReceivedAt     time.Time         `json:"received_at"`
	SignatureValid bool              `json:"signature_valid"`
	DedupeKey      string            `json:"dedupe_key,omitempty"`
	PlannedAt      *time.Time        `json:"planned_at,omitempty"`
}

// InsertEventParams describes one received webhook.
type InsertEventParams struct {
	ListenerID     int64
	RoutingKeys    []string
	RawBody        []byte
	Headers        map[string]string
	ContentType    string
	SignatureValid bool
	DedupeKey      string
}

// InsertEvent persists a received webhook and reports whether it was a
// duplicate.
//
// This is the whole of the ingest write path: one INSERT, no fan-out. The
// unique index on (listener_id, dedupe_key) makes redelivery idempotent —
// Meta resends the identical payload on timeout, and ON CONFLICT DO NOTHING
// turns the second copy into a no-op. A duplicate returns (nil, true, nil):
// the caller still answers 200, because from the provider's perspective the
// event was accepted the first time.
func (s *Store) InsertEvent(ctx context.Context, p InsertEventParams) (*Event, bool, error) {
	var dedupe *string
	if p.DedupeKey != "" {
		dedupe = &p.DedupeKey
	}
	// Guarantee a non-nil array so the column is '{}' rather than NULL, which
	// keeps the && overlap operator well-behaved during matching.
	keys := p.RoutingKeys
	if keys == nil {
		keys = []string{}
	}
	// Likewise for headers: a nil map encodes as SQL NULL, which the NOT NULL
	// column rejects.
	headers := p.Headers
	if headers == nil {
		headers = map[string]string{}
	}

	row := s.pool.QueryRow(ctx, `
		INSERT INTO events (
			listener_id, routing_keys, raw_body, headers, content_type,
			signature_valid, dedupe_key)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (listener_id, dedupe_key) WHERE dedupe_key IS NOT NULL
		DO NOTHING
		RETURNING id, listener_id, routing_keys, received_at, signature_valid`,
		p.ListenerID, keys, p.RawBody, headers, nullIfEmpty(p.ContentType),
		p.SignatureValid, dedupe)

	var e Event
	err := row.Scan(&e.ID, &e.ListenerID, &e.RoutingKeys, &e.ReceivedAt, &e.SignatureValid)
	if errors.Is(err, pgx.ErrNoRows) {
		// DO NOTHING suppressed the insert: this exact body already arrived.
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	e.DedupeKey = p.DedupeKey
	return &e, false, nil
}

// EventByID loads a single event including its raw body.
func (s *Store) EventByID(ctx context.Context, id int64) (*Event, error) {
	var e Event
	var contentType *string
	var dedupe *string
	err := s.pool.QueryRow(ctx, `
		SELECT id, listener_id, routing_keys, raw_body, headers, content_type,
		       received_at, signature_valid, dedupe_key, planned_at
		  FROM events WHERE id = $1`, id).
		Scan(&e.ID, &e.ListenerID, &e.RoutingKeys, &e.RawBody, &e.Headers,
			&contentType, &e.ReceivedAt, &e.SignatureValid, &dedupe, &e.PlannedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if contentType != nil {
		e.ContentType = *contentType
	}
	if dedupe != nil {
		e.DedupeKey = *dedupe
	}
	return &e, nil
}

// CountEvents returns the number of events for a listener; used by tests and
// the stats endpoint.
func (s *Store) CountEvents(ctx context.Context, listenerID int64) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE listener_id = $1`, listenerID).Scan(&n)
	return n, err
}
