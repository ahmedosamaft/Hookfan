package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type Subscription struct {
	ID          int64           `json:"id"`
	ListenerID  int64           `json:"listener_id"`
	ServiceID   int64           `json:"-"`
	ServicePub  string          `json:"service_id"`
	FilterType  string          `json:"filter_type"`
	RoutingKeys []string        `json:"routing_keys"`
	FilterExpr  json.RawMessage `json:"filter_expr"`
	IsDefault   bool            `json:"is_default"`
	Enabled     bool            `json:"enabled"`
	CreatedAt   time.Time       `json:"created_at"`
}

const subscriptionColumns = `
	s.id, s.listener_id, s.service_id, svc.public_id, s.filter_type,
	s.routing_keys, s.filter_expr, s.is_default, s.enabled, s.created_at`

func scanSubscription(row pgx.Row) (*Subscription, error) {
	var s Subscription
	err := row.Scan(&s.ID, &s.ListenerID, &s.ServiceID, &s.ServicePub,
		&s.FilterType, &s.RoutingKeys, &s.FilterExpr, &s.IsDefault,
		&s.Enabled, &s.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *Store) SubscriptionByID(ctx context.Context, id int64) (*Subscription, error) {
	return scanSubscription(s.pool.QueryRow(ctx, `
		SELECT `+subscriptionColumns+`
		  FROM subscriptions s JOIN services svc ON svc.id = s.service_id
		 WHERE s.id = $1`, id))
}

// ListSubscriptions returns all subscriptions, optionally filtered to one
// listener.
func (s *Store) ListSubscriptions(ctx context.Context, listenerID *int64) ([]*Subscription, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+subscriptionColumns+`
		  FROM subscriptions s JOIN services svc ON svc.id = s.service_id
		 WHERE ($1::bigint IS NULL OR s.listener_id = $1)
		 ORDER BY s.listener_id, s.id`, listenerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Subscription
	for rows.Next() {
		sub, err := scanSubscription(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// SubscriptionsForListener returns the enabled subscriptions the planner needs
// to match an event against.
//
// Only services that are verified and enabled are joined in: a service in any
// other status receives no events, and filtering here means the planner never
// creates a delivery that would immediately be skipped.
func (s *Store) SubscriptionsForListener(ctx context.Context, listenerID int64) ([]*Subscription, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+subscriptionColumns+`
		  FROM subscriptions s
		  JOIN services svc ON svc.id = s.service_id
		 WHERE s.listener_id = $1
		   AND s.enabled
		   AND svc.enabled
		   AND svc.status = 'verified'
		 ORDER BY s.id`, listenerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Subscription
	for rows.Next() {
		sub, err := scanSubscription(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

type CreateSubscriptionParams struct {
	ListenerID  int64
	ServiceID   int64
	FilterType  string
	RoutingKeys []string
	FilterExpr  json.RawMessage
	IsDefault   bool
	Enabled     bool
}

func (s *Store) CreateSubscription(ctx context.Context, p CreateSubscriptionParams) (*Subscription, error) {
	keys := p.RoutingKeys
	if keys == nil {
		keys = []string{}
	}
	expr := p.FilterExpr
	if len(expr) == 0 {
		expr = json.RawMessage(`[]`)
	}

	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO subscriptions (
			listener_id, service_id, filter_type, routing_keys, filter_expr,
			is_default, enabled)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING id`,
		p.ListenerID, p.ServiceID, p.FilterType, keys, expr, p.IsDefault, p.Enabled).Scan(&id)
	if err != nil {
		return nil, err
	}
	return s.SubscriptionByID(ctx, id)
}

type UpdateSubscriptionParams struct {
	FilterType  *string
	RoutingKeys []string
	FilterExpr  json.RawMessage
	IsDefault   *bool
	Enabled     *bool
}

func (s *Store) UpdateSubscription(ctx context.Context, id int64, p UpdateSubscriptionParams) (*Subscription, error) {
	var keys any
	if p.RoutingKeys != nil {
		keys = p.RoutingKeys
	}
	var expr any
	if len(p.FilterExpr) > 0 {
		expr = p.FilterExpr
	}

	tag, err := s.pool.Exec(ctx, `
		UPDATE subscriptions SET
			filter_type  = COALESCE($2, filter_type),
			routing_keys = COALESCE($3, routing_keys),
			filter_expr  = COALESCE($4, filter_expr),
			is_default   = COALESCE($5, is_default),
			enabled      = COALESCE($6, enabled)
		WHERE id = $1`,
		id, p.FilterType, keys, expr, p.IsDefault, p.Enabled)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	return s.SubscriptionByID(ctx, id)
}

func (s *Store) DeleteSubscription(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM subscriptions WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
