package store

import (
	"context"
	"fmt"
)

// PurgeExpiredEvents deletes up to limit events older than retentionDays and
// returns how many were removed.
//
// Deliveries are removed by the ON DELETE CASCADE on deliveries.event_id, so
// the two never diverge.
//
// The delete is batched by primary key rather than issued as one statement:
// a single unbounded DELETE over months of history would hold locks long
// enough to stall ingest, which must stay fast.
func (s *Store) PurgeExpiredEvents(ctx context.Context, retentionDays, limit int) (int64, error) {
	if retentionDays <= 0 {
		return 0, fmt.Errorf("retentionDays must be positive, got %d", retentionDays)
	}
	if limit <= 0 {
		limit = 1000
	}

	tag, err := s.pool.Exec(ctx, `
		DELETE FROM events
		 WHERE id IN (
		       SELECT id FROM events
		        WHERE received_at < now() - make_interval(days => $1)
		        ORDER BY id
		        LIMIT $2
		 )`, retentionDays, limit)
	if err != nil {
		return 0, fmt.Errorf("purge expired events: %w", err)
	}
	return tag.RowsAffected(), nil
}

// CountExpiredEvents reports how many events are eligible for purging.
func (s *Store) CountExpiredEvents(ctx context.Context, retentionDays int) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM events
		 WHERE received_at < now() - make_interval(days => $1)`, retentionDays).Scan(&n)
	return n, err
}
