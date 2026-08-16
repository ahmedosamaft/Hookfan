package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"
)

type queryFixture struct {
	store    *Store
	listener *Listener
	other    *Listener
	ctx      context.Context
}

func newQueryFixture(t *testing.T) *queryFixture {
	t.Helper()
	st := TestStore(t)
	ctx := context.Background()

	mk := func(slug string) *Listener {
		l, err := st.CreateListener(ctx, CreateListenerParams{
			Name: slug, Slug: slug, Provider: "meta", VerificationMode: "none",
			RoutingKeyPath: "entry[*].id", Enabled: true,
		})
		if err != nil {
			t.Fatalf("create listener %s: %v", slug, err)
		}
		return l
	}
	return &queryFixture{store: st, listener: mk("primary"), other: mk("secondary"), ctx: ctx}
}

func (f *queryFixture) addEvent(t *testing.T, l *Listener, keys []string, sigValid bool) *Event {
	t.Helper()
	body := fmt.Sprintf(`{"n":%d,"keys":%v}`, time.Now().UnixNano(), keys)
	sum := sha256.Sum256([]byte(body))

	event, dup, err := f.store.InsertEvent(f.ctx, InsertEventParams{
		ListenerID: l.ID, RoutingKeys: keys, RawBody: []byte(body),
		SignatureValid: sigValid, DedupeKey: hex.EncodeToString(sum[:]),
	})
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}
	if dup {
		t.Fatal("unexpected duplicate")
	}
	return event
}

// Cursor pagination must walk every row exactly once, with no gaps and no
// repeats — the failure mode that OFFSET has on a table receiving inserts.
func TestListEventsCursorPagination(t *testing.T) {
	f := newQueryFixture(t)
	const total = 25
	for range total {
		f.addEvent(t, f.listener, []string{"K"}, true)
	}

	seen := map[int64]int{}
	cursor := ""
	pages := 0

	for {
		page, err := f.store.ListEvents(f.ctx, EventFilter{Limit: 10, Cursor: cursor})
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		pages++
		for _, e := range page.Events {
			seen[e.ID]++
		}
		if !page.HasMore {
			if page.NextCursor != "" {
				t.Error("NextCursor is set on the last page")
			}
			break
		}
		if page.NextCursor == "" {
			t.Fatal("HasMore is true but NextCursor is empty")
		}
		cursor = page.NextCursor
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}

	if len(seen) != total {
		t.Errorf("saw %d distinct events, want %d", len(seen), total)
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("event %d appeared %d times, want once", id, count)
		}
	}
	if pages != 3 {
		t.Errorf("took %d pages, want 3 for %d events at 10 per page", pages, total)
	}
}

// New events arriving mid-pagination must not shift the pages already being
// walked. This is the whole reason for cursors over OFFSET.
func TestListEventsCursorIsStableUnderInserts(t *testing.T) {
	f := newQueryFixture(t)
	for range 10 {
		f.addEvent(t, f.listener, []string{"K"}, true)
	}

	first, err := f.store.ListEvents(f.ctx, EventFilter{Limit: 5})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	firstIDs := map[int64]bool{}
	for _, e := range first.Events {
		firstIDs[e.ID] = true
	}

	// Five more events land between the two page requests.
	for range 5 {
		f.addEvent(t, f.listener, []string{"K"}, true)
	}

	second, err := f.store.ListEvents(f.ctx, EventFilter{Limit: 5, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("ListEvents page 2: %v", err)
	}
	for _, e := range second.Events {
		if firstIDs[e.ID] {
			t.Errorf("event %d appeared on both pages; the cursor shifted", e.ID)
		}
	}
}

func TestListEventsFilters(t *testing.T) {
	f := newQueryFixture(t)
	f.addEvent(t, f.listener, []string{"WABA_ONE"}, true)
	f.addEvent(t, f.listener, []string{"WABA_TWO"}, true)
	f.addEvent(t, f.listener, []string{"WABA_ONE", "WABA_TWO"}, true)
	f.addEvent(t, f.other, []string{"WABA_ONE"}, true)
	f.addEvent(t, f.listener, []string{"WABA_ONE"}, false)

	t.Run("by listener", func(t *testing.T) {
		page, err := f.store.ListEvents(f.ctx, EventFilter{ListenerID: &f.listener.ID})
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		if len(page.Events) != 4 {
			t.Errorf("got %d events, want 4 for the primary listener", len(page.Events))
		}
	})

	t.Run("by routing key matches multi-key events", func(t *testing.T) {
		page, err := f.store.ListEvents(f.ctx, EventFilter{RoutingKey: "WABA_TWO"})
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		// The single-key event and the two-key event both contain WABA_TWO.
		if len(page.Events) != 2 {
			t.Errorf("got %d events, want 2 containing WABA_TWO", len(page.Events))
		}
	})

	t.Run("by signature validity", func(t *testing.T) {
		invalid := false
		page, err := f.store.ListEvents(f.ctx, EventFilter{SigValid: &invalid})
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		if len(page.Events) != 1 {
			t.Errorf("got %d events, want 1 with an invalid signature", len(page.Events))
		}
	})

	t.Run("by time range", func(t *testing.T) {
		future := time.Now().Add(time.Hour)
		page, err := f.store.ListEvents(f.ctx, EventFilter{Since: &future})
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		if len(page.Events) != 0 {
			t.Errorf("got %d events from the future, want 0", len(page.Events))
		}
	})
}

func TestListEventsRejectsBadCursor(t *testing.T) {
	f := newQueryFixture(t)
	for _, bad := range []string{"nonsense", "!!!", "YWJj"} {
		if _, err := f.store.ListEvents(f.ctx, EventFilter{Cursor: bad}); err == nil {
			t.Errorf("cursor %q was accepted, want an error", bad)
		}
	}
}

func TestListEventsClampsLimit(t *testing.T) {
	f := newQueryFixture(t)
	f.addEvent(t, f.listener, []string{"K"}, true)

	// An unbounded limit would let one request stream the whole table.
	page, err := f.store.ListEvents(f.ctx, EventFilter{Limit: 100000})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(page.Events))
	}
}

// The listing carries a delivery rollup so the table can show "2/3 delivered"
// without a query per row.
func TestListEventsIncludesDeliverySummary(t *testing.T) {
	f := newQueryFixture(t)
	event := f.addEvent(t, f.listener, []string{"K"}, true)

	svc := func(name string) int64 {
		s, err := f.store.CreateService(f.ctx, CreateServiceParams{
			PublicID: "svc_" + name, Name: name, URL: "https://" + name + ".test",
			Method: "POST", LinkToken: []byte("x"), TimeoutMS: 1000, MaxAttempts: 3, Enabled: true,
		})
		if err != nil {
			t.Fatalf("create service: %v", err)
		}
		return s.ID
	}
	a, b, c := svc("a"), svc("b"), svc("c")

	for id, status := range map[int64]string{a: "success", b: "success", c: "dead"} {
		if _, err := f.store.Pool().Exec(f.ctx,
			`INSERT INTO deliveries (event_id, service_id, status) VALUES ($1,$2,$3)`,
			event.ID, id, status); err != nil {
			t.Fatalf("insert delivery: %v", err)
		}
	}

	page, err := f.store.ListEvents(f.ctx, EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	got := page.Events[0]
	if got.DeliveryTotal != 3 {
		t.Errorf("DeliveryTotal = %d, want 3", got.DeliveryTotal)
	}
	if got.DeliverySuccess != 2 {
		t.Errorf("DeliverySuccess = %d, want 2", got.DeliverySuccess)
	}
	if got.DeliveryDead != 1 {
		t.Errorf("DeliveryDead = %d, want 1", got.DeliveryDead)
	}
	if got.BodyBytes == 0 {
		t.Error("BodyBytes = 0, want the body length")
	}
}

// Replay must clear existing deliveries: the unique (event_id, service_id)
// index would otherwise make re-planning a silent no-op.
func TestReplayEventClearsDeliveriesAndPlanning(t *testing.T) {
	f := newQueryFixture(t)
	event := f.addEvent(t, f.listener, []string{"K"}, true)

	svc, err := f.store.CreateService(f.ctx, CreateServiceParams{
		PublicID: "svc_r", Name: "r", URL: "https://r.test", Method: "POST",
		LinkToken: []byte("x"), TimeoutMS: 1000, MaxAttempts: 3, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create service: %v", err)
	}
	if _, err := f.store.Pool().Exec(f.ctx,
		`INSERT INTO deliveries (event_id, service_id, status) VALUES ($1,$2,'success')`,
		event.ID, svc.ID); err != nil {
		t.Fatalf("insert delivery: %v", err)
	}
	if _, err := f.store.Pool().Exec(f.ctx,
		`UPDATE events SET planned_at = now() WHERE id = $1`, event.ID); err != nil {
		t.Fatalf("mark planned: %v", err)
	}

	if err := f.store.ReplayEvent(f.ctx, event.ID); err != nil {
		t.Fatalf("ReplayEvent: %v", err)
	}

	deliveries, err := f.store.DeliveriesForEvent(f.ctx, event.ID)
	if err != nil {
		t.Fatalf("DeliveriesForEvent: %v", err)
	}
	if len(deliveries) != 0 {
		t.Errorf("got %d deliveries after replay, want 0", len(deliveries))
	}

	stored, err := f.store.EventByID(f.ctx, event.ID)
	if err != nil {
		t.Fatalf("EventByID: %v", err)
	}
	if stored.PlannedAt != nil {
		t.Error("planned_at is still set; the planner would skip this event")
	}
	// The event itself must survive, byte-exact.
	if len(stored.RawBody) == 0 {
		t.Error("raw_body was lost during replay")
	}
}

func TestReplayEventNotFound(t *testing.T) {
	f := newQueryFixture(t)
	if err := f.store.ReplayEvent(f.ctx, 999999); err == nil {
		t.Error("ReplayEvent on a missing event returned nil, want an error")
	}
}

func TestGetStats(t *testing.T) {
	f := newQueryFixture(t)
	for range 3 {
		f.addEvent(t, f.listener, []string{"K"}, true)
	}
	f.addEvent(t, f.listener, []string{"K"}, false)

	stats, err := f.store.GetStats(f.ctx)
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}

	for _, w := range []string{"1h", "24h", "7d"} {
		if stats.Windows[w] == nil {
			t.Fatalf("window %q is missing", w)
		}
	}
	if got := stats.Windows["1h"].Events; got != 4 {
		t.Errorf("1h events = %d, want 4", got)
	}
	if got := stats.Windows["1h"].EventsInvalidSig; got != 1 {
		t.Errorf("1h invalid-signature events = %d, want 1", got)
	}
	// Every event is unplanned here, so the lag metric must be non-zero.
	if stats.Queue.UnplannedEvents != 4 {
		t.Errorf("unplanned events = %d, want 4", stats.Queue.UnplannedEvents)
	}
}

// The success rate must count settled deliveries only: treating a pending
// backlog as failure would make a healthy queue look like an outage.
func TestStatsSuccessRateExcludesPending(t *testing.T) {
	f := newQueryFixture(t)
	event := f.addEvent(t, f.listener, []string{"K"}, true)

	mk := func(name, status string) {
		s, err := f.store.CreateService(f.ctx, CreateServiceParams{
			PublicID: "svc_" + name, Name: name, URL: "https://" + name + ".test",
			Method: "POST", LinkToken: []byte("x"), TimeoutMS: 1000, MaxAttempts: 3, Enabled: true,
		})
		if err != nil {
			t.Fatalf("create service: %v", err)
		}
		if _, err := f.store.Pool().Exec(f.ctx,
			`INSERT INTO deliveries (event_id, service_id, status) VALUES ($1,$2,$3)`,
			event.ID, s.ID, status); err != nil {
			t.Fatalf("insert delivery: %v", err)
		}
	}
	mk("ok1", "success")
	mk("ok2", "success")
	mk("waiting", "pending")

	stats, err := f.store.GetStats(f.ctx)
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	// 2 of 2 settled, not 2 of 3 total.
	if rate := stats.Windows["1h"].SuccessRate; rate != 1.0 {
		t.Errorf("SuccessRate = %v, want 1.0 (pending must not count as failure)", rate)
	}
}

func TestEventsPerMinuteFillsGaps(t *testing.T) {
	f := newQueryFixture(t)
	f.addEvent(t, f.listener, []string{"K"}, true)

	series, err := f.store.EventsPerMinute(f.ctx, 10)
	if err != nil {
		t.Fatalf("EventsPerMinute: %v", err)
	}
	// Quiet minutes must appear as zero, or the sparkline's shape lies.
	if len(series) != 10 {
		t.Errorf("got %d buckets, want 10 including empty minutes", len(series))
	}
	var total int
	for _, b := range series {
		total += b.Count
	}
	if total != 1 {
		t.Errorf("total events across the series = %d, want 1", total)
	}
}
