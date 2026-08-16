// Package retention deletes events and their deliveries once they age past
// EVENT_RETENTION_DAYS, so the tables do not grow without bound.
package retention

import (
	"context"
	"log/slog"
	"time"

	"github.com/aosama/hookfan/internal/store"
)

const (
	// BatchSize bounds one DELETE. Purging in batches keeps each statement's
	// lock footprint small, so a large backlog cannot block ingest.
	BatchSize = 1000

	// Interval is how often a purge pass runs.
	Interval = time.Hour

	// StartupDelay lets the service finish starting before the first pass, so
	// a restart loop never turns into a delete loop.
	StartupDelay = 5 * time.Minute
)

type Job struct {
	Store         *store.Store
	Log           *slog.Logger
	RetentionDays int
}

// Run purges expired events until the context is cancelled.
func (j *Job) Run(ctx context.Context) {
	j.Log.Info("retention job started",
		"retention_days", j.RetentionDays,
		"interval", Interval,
		"batch_size", BatchSize)

	select {
	case <-ctx.Done():
		return
	case <-time.After(StartupDelay):
	}

	ticker := time.NewTicker(Interval)
	defer ticker.Stop()

	for {
		j.purge(ctx)
		select {
		case <-ctx.Done():
			j.Log.Info("retention job stopped")
			return
		case <-ticker.C:
		}
	}
}

// purge deletes expired events in batches until none remain.
func (j *Job) purge(ctx context.Context) {
	start := time.Now()
	var total int64

	for {
		if ctx.Err() != nil {
			return
		}
		deleted, err := j.Store.PurgeExpiredEvents(ctx, j.RetentionDays, BatchSize)
		if err != nil {
			j.Log.Error("retention purge failed", "error", err, "deleted_so_far", total)
			return
		}
		total += deleted
		if deleted < BatchSize {
			break
		}
		// Yield between batches so a large purge does not monopolise the pool.
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}

	if total > 0 {
		j.Log.Info("retention purge complete",
			"events_deleted", total,
			"older_than_days", j.RetentionDays,
			"duration", time.Since(start).Round(time.Millisecond))
	} else {
		j.Log.Debug("retention purge: nothing to delete", "older_than_days", j.RetentionDays)
	}
}
