package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"testing"
)

// agedEventSeq keeps every fixture body distinct, so the (listener_id,
// dedupe_key) unique index does not collapse several inserts into one.
var agedEventSeq atomic.Int64

// insertAgedEvent creates an event backdated by the given number of days.
func insertAgedEvent(t *testing.T, s *Store, listenerID int64, ageDays int) int64 {
	t.Helper()
	ctx := context.Background()
	body := fmt.Sprintf(`{"age":%d,"seq":%d}`, ageDays, agedEventSeq.Add(1))
	sum := sha256.Sum256([]byte(body))

	event, _, err := s.InsertEvent(ctx, InsertEventParams{
		ListenerID: listenerID, RoutingKeys: []string{"K"}, RawBody: []byte(body),
		SignatureValid: true, DedupeKey: hex.EncodeToString(sum[:]),
	})
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}
	if _, err := s.Pool().Exec(ctx,
		`UPDATE events SET received_at = now() - make_interval(days => $2) WHERE id = $1`,
		event.ID, ageDays); err != nil {
		t.Fatalf("backdate event: %v", err)
	}
	return event.ID
}

func retentionFixture(t *testing.T) (*Store, *Listener) {
	t.Helper()
	s := TestStore(t)
	l, err := s.CreateListener(context.Background(), CreateListenerParams{
		Name: "r", Slug: "r", Provider: "meta", VerificationMode: "none",
		RoutingKeyPath: "entry[*].id", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create listener: %v", err)
	}
	return s, l
}

func TestPurgeDeletesOnlyExpiredEvents(t *testing.T) {
	s, l := retentionFixture(t)
	ctx := context.Background()

	fresh := insertAgedEvent(t, s, l.ID, 1)
	recent := insertAgedEvent(t, s, l.ID, 29)
	old := insertAgedEvent(t, s, l.ID, 31)
	ancient := insertAgedEvent(t, s, l.ID, 400)

	deleted, err := s.PurgeExpiredEvents(ctx, 30, 1000)
	if err != nil {
		t.Fatalf("PurgeExpiredEvents: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted %d events, want 2 (the 31- and 400-day-old ones)", deleted)
	}

	for _, id := range []int64{fresh, recent} {
		if _, err := s.EventByID(ctx, id); err != nil {
			t.Errorf("event %d was deleted but is inside the retention window", id)
		}
	}
	for _, id := range []int64{old, ancient} {
		if _, err := s.EventByID(ctx, id); err == nil {
			t.Errorf("event %d is past retention but still present", id)
		}
	}
}

// Deliveries must go with their event, or the table would grow unbounded while
// events shrank.
func TestPurgeCascadesToDeliveries(t *testing.T) {
	s, l := retentionFixture(t)
	ctx := context.Background()

	old := insertAgedEvent(t, s, l.ID, 60)
	svc, err := s.CreateService(ctx, CreateServiceParams{
		PublicID: "svc_purge", Name: "p", URL: "https://p.test", Method: "POST",
		LinkToken: []byte("x"), TimeoutMS: 1000, MaxAttempts: 3, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create service: %v", err)
	}
	if _, err := s.Pool().Exec(ctx,
		`INSERT INTO deliveries (event_id, service_id, status) VALUES ($1,$2,'success')`,
		old, svc.ID); err != nil {
		t.Fatalf("insert delivery: %v", err)
	}

	if _, err := s.PurgeExpiredEvents(ctx, 30, 1000); err != nil {
		t.Fatalf("PurgeExpiredEvents: %v", err)
	}

	var orphans int
	if err := s.Pool().QueryRow(ctx,
		`SELECT count(*) FROM deliveries WHERE event_id = $1`, old).Scan(&orphans); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if orphans != 0 {
		t.Errorf("%d deliveries survived their purged event", orphans)
	}
	// The service itself must not be touched.
	if _, _, err := s.ServiceByID(ctx, svc.ID); err != nil {
		t.Error("purging events deleted the service")
	}
}

// The job purges in batches so one statement cannot lock the table for long.
func TestPurgeRespectsBatchLimit(t *testing.T) {
	s, l := retentionFixture(t)
	ctx := context.Background()

	for range 7 {
		insertAgedEvent(t, s, l.ID, 90)
	}

	deleted, err := s.PurgeExpiredEvents(ctx, 30, 3)
	if err != nil {
		t.Fatalf("PurgeExpiredEvents: %v", err)
	}
	if deleted != 3 {
		t.Fatalf("deleted %d in one batch, want the 3-row limit", deleted)
	}

	// Looping to exhaustion is what the job does.
	var total int64 = deleted
	for {
		n, err := s.PurgeExpiredEvents(ctx, 30, 3)
		if err != nil {
			t.Fatalf("PurgeExpiredEvents: %v", err)
		}
		total += n
		if n < 3 {
			break
		}
	}
	if total != 7 {
		t.Errorf("purged %d events in total, want 7", total)
	}

	remaining, err := s.CountExpiredEvents(ctx, 30)
	if err != nil {
		t.Fatalf("CountExpiredEvents: %v", err)
	}
	if remaining != 0 {
		t.Errorf("%d expired events remain", remaining)
	}
}

func TestPurgeRejectsNonPositiveRetention(t *testing.T) {
	s, _ := retentionFixture(t)
	// A zero or negative window would delete everything, including events
	// received seconds ago.
	for _, days := range []int{0, -1} {
		if _, err := s.PurgeExpiredEvents(context.Background(), days, 100); err == nil {
			t.Errorf("PurgeExpiredEvents(retentionDays=%d) was accepted, want an error", days)
		}
	}
}

func TestPurgeOnEmptyTable(t *testing.T) {
	s, _ := retentionFixture(t)
	deleted, err := s.PurgeExpiredEvents(context.Background(), 30, 100)
	if err != nil {
		t.Fatalf("PurgeExpiredEvents: %v", err)
	}
	if deleted != 0 {
		t.Errorf("deleted %d from an empty table, want 0", deleted)
	}
}
