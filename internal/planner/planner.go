// Package planner turns received events into delivery sets.
//
// It exists so the ingest path can stay minimal: a webhook handler writes one
// row and returns, and all subscription matching and delivery creation happens
// here, off the request goroutine. Each event is planned in a single
// transaction, so a crash mid-plan leaves planned_at NULL and the next pass
// simply retries it.
package planner

import (
	"context"
	"log/slog"
	"time"

	"github.com/aosama/hookfan/internal/router"
	"github.com/aosama/hookfan/internal/store"
	"github.com/jackc/pgx/v5"
)

// BatchSize is how many events one planning pass claims.
const BatchSize = 64

// PollInterval is the fallback tick. A notification normally wakes the planner
// sooner; this bounds the delay if one is ever missed.
const PollInterval = time.Second

type Planner struct {
	Store *store.Store
	Log   *slog.Logger

	// wake carries notifications from ingest. Buffered and coalesced: a burst
	// of webhooks results in one planning pass, not one per event.
	wake chan struct{}
	// OnPlanned is called after deliveries are created, so the dispatcher can
	// start work without waiting for its own poll.
	OnPlanned func(deliveries int)
}

func New(st *store.Store, log *slog.Logger) *Planner {
	return &Planner{
		Store: st,
		Log:   log,
		wake:  make(chan struct{}, 1),
	}
}

// NotifyEvent wakes the planner. It never blocks: if a wake-up is already
// pending, this one is redundant.
func (p *Planner) NotifyEvent() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Run plans events until the context is cancelled.
func (p *Planner) Run(ctx context.Context) {
	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()

	p.Log.Info("planner started", "batch_size", BatchSize, "poll_interval", PollInterval)

	for {
		// Drain fully on every wake-up: one pass may claim only BatchSize
		// events, and the backlog must not wait for the next tick.
		for {
			n, err := p.planBatch(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				p.Log.Error("planning pass failed", "error", err)
				break
			}
			if n < BatchSize {
				break
			}
		}

		select {
		case <-ctx.Done():
			p.Log.Info("planner stopped")
			return
		case <-p.wake:
		case <-ticker.C:
		}
	}
}

// planBatch claims and plans one batch, returning how many events it handled.
func (p *Planner) planBatch(ctx context.Context) (int, error) {
	tx, err := p.Store.Pool().Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())

	events, err := store.ClaimUnplannedEvents(ctx, tx, BatchSize)
	if err != nil {
		return 0, err
	}
	if len(events) == 0 {
		return 0, nil
	}

	// Subscriptions are loaded once per listener rather than per event: a
	// batch usually comes from one listener.
	subsCache := map[int64][]router.Subscription{}
	var totalDeliveries int

	for _, event := range events {
		subs, ok := subsCache[event.ListenerID]
		if !ok {
			subs, err = loadSubscriptions(ctx, tx, event.ListenerID)
			if err != nil {
				return 0, err
			}
			subsCache[event.ListenerID] = subs
		}

		plans, usedDefault := p.planEvent(event, subs)
		if err := store.InsertDeliveries(ctx, tx, event.ID, plans); err != nil {
			return 0, err
		}
		totalDeliveries += len(plans)

		switch {
		case len(plans) == 0:
			// Not an error: a listener with no verified subscriber is a normal
			// state during setup. Logged so it is visible rather than silent.
			p.Log.Warn("event matched no service",
				"event_id", event.ID,
				"listener_id", event.ListenerID,
				"routing_keys", event.RoutingKeys)
		case usedDefault:
			p.Log.Info("event routed to default subscription",
				"event_id", event.ID,
				"routing_keys", event.RoutingKeys,
				"services", len(plans))
		default:
			p.Log.Debug("event planned",
				"event_id", event.ID,
				"services", len(plans))
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}

	if totalDeliveries > 0 && p.OnPlanned != nil {
		p.OnPlanned(totalDeliveries)
	}
	p.Log.Debug("planning pass complete", "events", len(events), "deliveries", totalDeliveries)
	return len(events), nil
}

// planEvent matches one event and returns the deduplicated delivery set.
func (p *Planner) planEvent(event store.UnplannedEvent, subs []router.Subscription) ([]store.PlannedDelivery, bool) {
	if len(subs) == 0 {
		return nil, false
	}

	// The payload is parsed only for filter evaluation; the stored and
	// forwarded bytes are always the originals.
	var doc any
	if needsPayload(subs) {
		if parsed, err := router.ParseJSON(event.RawBody); err == nil {
			doc = parsed
		}
	}

	result := router.MatchAll(subs, event.RoutingKeys, doc)
	plans := make([]store.PlannedDelivery, 0, len(result.ServiceIDs))
	for _, serviceID := range result.ServiceIDs {
		plans = append(plans, store.PlannedDelivery{
			ServiceID:       serviceID,
			SubscriptionIDs: result.SubscriptionsByService[serviceID],
		})
	}
	return plans, result.UsedDefault
}

// needsPayload reports whether any subscription actually inspects the body, so
// a listener using only routing-key filters never pays for JSON parsing.
func needsPayload(subs []router.Subscription) bool {
	for _, s := range subs {
		if s.FilterType == router.FilterJSONPathMatch {
			return true
		}
	}
	return false
}

func loadSubscriptions(ctx context.Context, tx pgx.Tx, listenerID int64) ([]router.Subscription, error) {
	stored, err := store.SubscriptionsForPlanning(ctx, tx, listenerID)
	if err != nil {
		return nil, err
	}
	out := make([]router.Subscription, 0, len(stored))
	for _, s := range stored {
		conds, err := router.ParseConditions(s.FilterExpr)
		if err != nil {
			// A malformed filter must not take down planning for the whole
			// listener; the subscription simply matches nothing.
			conds = nil
		}
		out = append(out, router.Subscription{
			ID:          s.ID,
			ServiceID:   s.ServiceID,
			FilterType:  s.FilterType,
			RoutingKeys: s.RoutingKeys,
			FilterExpr:  conds,
			IsDefault:   s.IsDefault,
		})
	}
	return out, nil
}
