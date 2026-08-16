package planner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/aosama/hookfan/internal/crypto"
	"github.com/aosama/hookfan/internal/store"
)

type harness struct {
	store    *store.Store
	planner  *Planner
	listener *store.Listener
	ctx      context.Context
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st := store.TestStore(t)
	ctx := context.Background()

	listener, err := st.CreateListener(ctx, store.CreateListenerParams{
		Name: "Test", Slug: "test", Provider: "meta",
		VerificationMode: "none", RoutingKeyPath: "entry[*].id", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create listener: %v", err)
	}

	return &harness{
		store:    st,
		planner:  New(st, slog.New(slog.NewTextHandler(io.Discard, nil))),
		listener: listener,
		ctx:      ctx,
	}
}

// addService creates a service already in the `verified` state, since only
// verified services receive events.
func (h *harness) addService(t *testing.T, name string) *store.Service {
	t.Helper()
	pub, _ := crypto.RandomToken(8)
	svc, err := h.store.CreateService(h.ctx, store.CreateServiceParams{
		PublicID: "svc_" + pub, Name: name, URL: "https://" + name + ".test/hook",
		Method: "POST", LinkToken: []byte("enc"), TimeoutMS: 10000,
		MaxAttempts: 6, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create service: %v", err)
	}
	verified, err := h.store.MarkVerified(h.ctx, svc.ID)
	if err != nil {
		t.Fatalf("mark verified: %v", err)
	}
	return verified
}

func (h *harness) subscribe(t *testing.T, svc *store.Service, filterType string, keys []string, expr string, isDefault bool) *store.Subscription {
	t.Helper()
	sub, err := h.store.CreateSubscription(h.ctx, store.CreateSubscriptionParams{
		ListenerID: h.listener.ID, ServiceID: svc.ID, FilterType: filterType,
		RoutingKeys: keys, FilterExpr: json.RawMessage(expr),
		IsDefault: isDefault, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	return sub
}

func (h *harness) insertEvent(t *testing.T, body string, keys []string) *store.Event {
	t.Helper()
	event, dup, err := h.store.InsertEvent(h.ctx, store.InsertEventParams{
		ListenerID: h.listener.ID, RoutingKeys: keys, RawBody: []byte(body),
		SignatureValid: true, DedupeKey: crypto.BodyHash([]byte(body)),
	})
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}
	if dup {
		t.Fatal("unexpected duplicate")
	}
	return event
}

// deliveries returns the delivery rows for an event.
func (h *harness) deliveries(t *testing.T, eventID int64) []struct {
	ServiceID int64
	SubIDs    []int64
} {
	t.Helper()
	rows, err := h.store.Pool().Query(h.ctx, `
		SELECT service_id, matched_subscription_ids
		  FROM deliveries WHERE event_id = $1 ORDER BY service_id`, eventID)
	if err != nil {
		t.Fatalf("query deliveries: %v", err)
	}
	defer rows.Close()

	var out []struct {
		ServiceID int64
		SubIDs    []int64
	}
	for rows.Next() {
		var d struct {
			ServiceID int64
			SubIDs    []int64
		}
		if err := rows.Scan(&d.ServiceID, &d.SubIDs); err != nil {
			t.Fatalf("scan delivery: %v", err)
		}
		out = append(out, d)
	}
	return out
}

func (h *harness) plan(t *testing.T) {
	t.Helper()
	if _, err := h.planner.planBatch(h.ctx); err != nil {
		t.Fatalf("planBatch: %v", err)
	}
}

// The headline guarantee: a service matching two subscriptions in one batch
// receives exactly one delivery, not two identical POSTs.
func TestPlanDeduplicatesDeliveriesPerService(t *testing.T) {
	h := newHarness(t)
	svcA := h.addService(t, "alpha")
	svcB := h.addService(t, "beta")

	subOne := h.subscribe(t, svcA, "routing_key_in", []string{"WABA_ONE"}, "", false)
	subTwo := h.subscribe(t, svcA, "routing_key_in", []string{"WABA_TWO"}, "", false)
	h.subscribe(t, svcB, "all", nil, "", false)

	event := h.insertEvent(t,
		`{"entry":[{"id":"WABA_ONE"},{"id":"WABA_TWO"}]}`,
		[]string{"WABA_ONE", "WABA_TWO"})
	h.plan(t)

	deliveries := h.deliveries(t, event.ID)
	if len(deliveries) != 2 {
		t.Fatalf("got %d deliveries, want exactly 2 (one per service)", len(deliveries))
	}

	// Provenance: alpha's single delivery records both matching subscriptions.
	for _, d := range deliveries {
		if d.ServiceID != svcA.ID {
			continue
		}
		if len(d.SubIDs) != 2 {
			t.Errorf("alpha delivery matched_subscription_ids = %v, want both %d and %d",
				d.SubIDs, subOne.ID, subTwo.ID)
		}
	}
}

func TestPlanMarksEventPlanned(t *testing.T) {
	h := newHarness(t)
	svc := h.addService(t, "alpha")
	h.subscribe(t, svc, "all", nil, "", false)

	event := h.insertEvent(t, `{"entry":[{"id":"X"}]}`, []string{"X"})

	before, _ := h.store.CountUnplannedEvents(h.ctx)
	if before != 1 {
		t.Fatalf("unplanned before = %d, want 1", before)
	}

	h.plan(t)

	after, _ := h.store.CountUnplannedEvents(h.ctx)
	if after != 0 {
		t.Errorf("unplanned after = %d, want 0", after)
	}
	stored, err := h.store.EventByID(h.ctx, event.ID)
	if err != nil {
		t.Fatalf("load event: %v", err)
	}
	if stored.PlannedAt == nil {
		t.Error("planned_at is still NULL after planning")
	}
}

// Planning twice must not double-deliver: the unique (event_id, service_id)
// index makes it a database invariant.
func TestPlanIsIdempotent(t *testing.T) {
	h := newHarness(t)
	svc := h.addService(t, "alpha")
	h.subscribe(t, svc, "all", nil, "", false)

	event := h.insertEvent(t, `{"entry":[{"id":"X"}]}`, []string{"X"})
	h.plan(t)

	// Force a replan, simulating a crash after inserting deliveries but before
	// the commit that marked the event planned.
	if _, err := h.store.Pool().Exec(h.ctx,
		`UPDATE events SET planned_at = NULL WHERE id = $1`, event.ID); err != nil {
		t.Fatalf("reset planned_at: %v", err)
	}
	h.plan(t)

	if got := len(h.deliveries(t, event.ID)); got != 1 {
		t.Errorf("deliveries after replanning = %d, want 1", got)
	}
}

func TestPlanDefaultSubscriptionFallback(t *testing.T) {
	h := newHarness(t)
	specific := h.addService(t, "specific")
	fallback := h.addService(t, "fallback")

	h.subscribe(t, specific, "routing_key_in", []string{"KNOWN"}, "", false)
	h.subscribe(t, fallback, "routing_key_in", []string{"NEVER"}, "", true)

	// An event whose asset nobody subscribed to must not be dropped.
	event := h.insertEvent(t, `{"entry":[{"id":"UNKNOWN"}]}`, []string{"UNKNOWN"})
	h.plan(t)

	deliveries := h.deliveries(t, event.ID)
	if len(deliveries) != 1 {
		t.Fatalf("got %d deliveries, want 1 (the default)", len(deliveries))
	}
	if deliveries[0].ServiceID != fallback.ID {
		t.Errorf("delivered to service %d, want the default %d", deliveries[0].ServiceID, fallback.ID)
	}
}

func TestPlanSkipsUnverifiedAndDisabledServices(t *testing.T) {
	h := newHarness(t)

	verified := h.addService(t, "verified")
	h.subscribe(t, verified, "all", nil, "", false)

	// A pending service (never verified) must receive nothing.
	pub, _ := crypto.RandomToken(8)
	pending, err := h.store.CreateService(h.ctx, store.CreateServiceParams{
		PublicID: "svc_" + pub, Name: "pending", URL: "https://pending.test/hook",
		Method: "POST", LinkToken: []byte("enc"), TimeoutMS: 10000, MaxAttempts: 6, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create pending service: %v", err)
	}
	h.subscribe(t, pending, "all", nil, "", false)

	// A verified but disabled service must also receive nothing.
	disabled := h.addService(t, "disabled")
	enabled := false
	if _, err := h.store.UpdateService(h.ctx, disabled.ID,
		store.UpdateServiceParams{Enabled: &enabled}); err != nil {
		t.Fatalf("disable service: %v", err)
	}
	h.subscribe(t, disabled, "all", nil, "", false)

	event := h.insertEvent(t, `{"entry":[{"id":"X"}]}`, []string{"X"})
	h.plan(t)

	deliveries := h.deliveries(t, event.ID)
	if len(deliveries) != 1 {
		t.Fatalf("got %d deliveries, want 1 (only the verified, enabled service)", len(deliveries))
	}
	if deliveries[0].ServiceID != verified.ID {
		t.Errorf("delivered to service %d, want %d", deliveries[0].ServiceID, verified.ID)
	}
}

func TestPlanJSONPathFilter(t *testing.T) {
	h := newHarness(t)
	svc := h.addService(t, "alpha")
	h.subscribe(t, svc, "jsonpath_match", nil,
		`[{"path":"object","op":"eq","value":"whatsapp_business_account"}]`, false)

	matching := h.insertEvent(t, `{"object":"whatsapp_business_account","entry":[{"id":"X"}]}`, []string{"X"})
	other := h.insertEvent(t, `{"object":"instagram","entry":[{"id":"Y"}]}`, []string{"Y"})
	h.plan(t)

	if got := len(h.deliveries(t, matching.ID)); got != 1 {
		t.Errorf("matching event deliveries = %d, want 1", got)
	}
	if got := len(h.deliveries(t, other.ID)); got != 0 {
		t.Errorf("non-matching event deliveries = %d, want 0", got)
	}
}

// An event matching nothing is still marked planned, or the planner would
// retry it forever and the lag metric would never clear.
func TestPlanEventWithNoMatchIsStillMarkedPlanned(t *testing.T) {
	h := newHarness(t)
	svc := h.addService(t, "alpha")
	h.subscribe(t, svc, "routing_key_in", []string{"NEVER"}, "", false)

	event := h.insertEvent(t, `{"entry":[{"id":"X"}]}`, []string{"X"})
	h.plan(t)

	if got := len(h.deliveries(t, event.ID)); got != 0 {
		t.Errorf("deliveries = %d, want 0", got)
	}
	unplanned, _ := h.store.CountUnplannedEvents(h.ctx)
	if unplanned != 0 {
		t.Errorf("unplanned events = %d, want 0; an unmatched event must not be retried forever", unplanned)
	}
}

func TestPlanBatchHandlesEmptyQueue(t *testing.T) {
	h := newHarness(t)
	n, err := h.planner.planBatch(h.ctx)
	if err != nil {
		t.Fatalf("planBatch on empty queue: %v", err)
	}
	if n != 0 {
		t.Errorf("planned %d events, want 0", n)
	}
}
