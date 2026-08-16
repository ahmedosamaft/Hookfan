package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// UnplannedEvent is an event awaiting its delivery set.
type UnplannedEvent struct {
	ID          int64
	ListenerID  int64
	RoutingKeys []string
	RawBody     []byte
}

// PlannedDelivery describes one delivery the planner wants to create.
type PlannedDelivery struct {
	ServiceID       int64
	SubscriptionIDs []int64
}

// ClaimUnplannedEvents locks a batch of events that have not yet been planned
// and returns them inside the caller's transaction.
//
// SKIP LOCKED lets several planner instances work concurrently without
// contending; rows locked by another planner are simply left for it.
func ClaimUnplannedEvents(ctx context.Context, tx pgx.Tx, limit int) ([]UnplannedEvent, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, listener_id, routing_keys, raw_body
		  FROM events
		 WHERE planned_at IS NULL
		 ORDER BY id
		 FOR UPDATE SKIP LOCKED
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("claim unplanned events: %w", err)
	}
	defer rows.Close()

	var out []UnplannedEvent
	for rows.Next() {
		var e UnplannedEvent
		if err := rows.Scan(&e.ID, &e.ListenerID, &e.RoutingKeys, &e.RawBody); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// InsertDeliveries creates the delivery set for one event and marks it planned.
//
// Both happen in the caller's transaction, so an event is never left half
// planned: either its deliveries exist and planned_at is set, or neither is
// true and the next pass retries it.
func InsertDeliveries(ctx context.Context, tx pgx.Tx, eventID int64, plans []PlannedDelivery) error {
	for _, p := range plans {
		// ON CONFLICT DO NOTHING against the unique (event_id, service_id)
		// index: even a double-planned event cannot produce duplicate sends.
		if _, err := tx.Exec(ctx, `
			INSERT INTO deliveries (event_id, service_id, matched_subscription_ids)
			VALUES ($1, $2, $3)
			ON CONFLICT (event_id, service_id) DO NOTHING`,
			eventID, p.ServiceID, p.SubscriptionIDs); err != nil {
			return fmt.Errorf("insert delivery for service %d: %w", p.ServiceID, err)
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE events SET planned_at = now() WHERE id = $1`, eventID); err != nil {
		return fmt.Errorf("mark event %d planned: %w", eventID, err)
	}
	return nil
}

// PlannerSubscription is the matching input the planner loads per listener.
type PlannerSubscription struct {
	ID          int64
	ServiceID   int64
	FilterType  string
	RoutingKeys []string
	FilterExpr  json.RawMessage
	IsDefault   bool
}

// SubscriptionsForPlanning loads the deliverable subscriptions for a listener
// inside the planner's transaction.
func SubscriptionsForPlanning(ctx context.Context, tx pgx.Tx, listenerID int64) ([]PlannerSubscription, error) {
	rows, err := tx.Query(ctx, `
		SELECT s.id, s.service_id, s.filter_type, s.routing_keys, s.filter_expr, s.is_default
		  FROM subscriptions s
		  JOIN services svc ON svc.id = s.service_id
		 WHERE s.listener_id = $1
		   AND s.enabled
		   AND svc.enabled
		   AND svc.status = 'verified'
		 ORDER BY s.id`, listenerID)
	if err != nil {
		return nil, fmt.Errorf("load subscriptions for listener %d: %w", listenerID, err)
	}
	defer rows.Close()

	var out []PlannerSubscription
	for rows.Next() {
		var s PlannerSubscription
		if err := rows.Scan(&s.ID, &s.ServiceID, &s.FilterType, &s.RoutingKeys,
			&s.FilterExpr, &s.IsDefault); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// CountUnplannedEvents reports the planner backlog.
func (s *Store) CountUnplannedEvents(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE planned_at IS NULL`).Scan(&n)
	return n, err
}
