package ingest

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/aosama/hookfan/internal/crypto"
	"github.com/aosama/hookfan/internal/store"
)

const (
	testSecret      = "meta-app-secret"
	testVerifyToken = "my-verify-token"
	testSlug        = "whatsapp-prod"
)

// metaBody is a realistic single-entry WhatsApp payload.
const metaBody = `{"object":"whatsapp_business_account","entry":[{"id":"WABA_ONE","changes":[{"field":"messages","value":{"messages":[{"id":"wamid.AAA"}]}}]}]}`

// multiEntryBody carries two assets in one request.
const multiEntryBody = `{"object":"whatsapp_business_account","entry":[{"id":"WABA_ONE","changes":[]},{"id":"WABA_TWO","changes":[]}]}`

type fixture struct {
	handler  *Handler
	store    *store.Store
	listener *store.Listener
	notified int
}

func (f *fixture) NotifyEvent() { f.notified++ }

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st := store.TestStore(t)

	cipher, err := crypto.NewCipher(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	enc, err := cipher.EncryptString(testSecret)
	if err != nil {
		t.Fatalf("encrypt secret: %v", err)
	}

	listener, err := st.CreateListener(context.Background(), store.CreateListenerParams{
		Name:                 "WhatsApp Production",
		Slug:                 testSlug,
		Provider:             "meta",
		VerificationMode:     "hmac_sha256",
		SignatureHeader:      "X-Hub-Signature-256",
		SignaturePrefix:      "sha256=",
		Secret:               enc,
		ChallengeVerifyToken: testVerifyToken,
		RoutingKeyPath:       "entry[*].id",
		Enabled:              true,
	})
	if err != nil {
		t.Fatalf("create listener: %v", err)
	}

	f := &fixture{store: st, listener: listener}
	f.handler = &Handler{
		Store:  st,
		Cipher: cipher,
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Notify: f,
	}
	return f
}

// post builds a signed POST unless sig is explicitly supplied.
func (f *fixture) post(body string, sig *string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/hooks/"+testSlug, strings.NewReader(body))
	req.SetPathValue("slug", testSlug)
	req.Header.Set("Content-Type", "application/json")

	signature := "sha256=" + crypto.SignHex([]byte(testSecret), []byte(body))
	if sig != nil {
		signature = *sig
	}
	if signature != "" {
		req.Header.Set("X-Hub-Signature-256", signature)
	}

	rec := httptest.NewRecorder()
	f.handler.Receive(rec, req)
	return rec
}

func ptr(s string) *string { return &s }

// --- Signature verification -----------------------------------------------

func TestReceiveSignatureVerification(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		signature *string // nil = correctly signed
		wantCode  int
	}{
		{"valid signature", metaBody, nil, http.StatusOK},
		{
			// The signature covers exact bytes, so one extra space invalidates it.
			name: "tampered body", body: metaBody + " ",
			signature: ptr("sha256=" + crypto.SignHex([]byte(testSecret), []byte(metaBody))),
			wantCode:  http.StatusUnauthorized,
		},
		{
			name: "wrong secret", body: metaBody,
			signature: ptr("sha256=" + crypto.SignHex([]byte("wrong-secret"), []byte(metaBody))),
			wantCode:  http.StatusUnauthorized,
		},
		{"missing header", metaBody, ptr(""), http.StatusUnauthorized},
		{
			// Present and correct hex, but without the sha256= prefix.
			name: "malformed prefix", body: metaBody,
			signature: ptr(crypto.SignHex([]byte(testSecret), []byte(metaBody))),
			wantCode:  http.StatusUnauthorized,
		},
		{"wrong prefix", metaBody, ptr("sha1=abcdef"), http.StatusUnauthorized},
		{"not hex", metaBody, ptr("sha256=zzzz"), http.StatusUnauthorized},
		{"empty digest", metaBody, ptr("sha256="), http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			rec := f.post(tt.body, tt.signature)

			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tt.wantCode, rec.Body.String())
			}

			// A rejected webhook must never be persisted as a deliverable event.
			n, err := f.store.CountEvents(context.Background(), f.listener.ID)
			if err != nil {
				t.Fatalf("count events: %v", err)
			}
			wantEvents := 0
			if tt.wantCode == http.StatusOK {
				wantEvents = 1
			}
			if n != wantEvents {
				t.Errorf("stored events = %d, want %d", n, wantEvents)
			}
		})
	}
}

// --- GET challenge --------------------------------------------------------

func TestChallenge(t *testing.T) {
	tests := []struct {
		name     string
		mode     string
		token    string
		verify   string
		wantCode int
		wantBody string
	}{
		{"correct token", "subscribe", testVerifyToken, "challenge-123", http.StatusOK, "challenge-123"},
		{"wrong token", "subscribe", "not-the-token", "challenge-123", http.StatusForbidden, ""},
		{"empty token", "subscribe", "", "challenge-123", http.StatusForbidden, ""},
		{"wrong mode", "unsubscribe", testVerifyToken, "challenge-123", http.StatusForbidden, ""},
		{"missing mode", "", testVerifyToken, "challenge-123", http.StatusForbidden, ""},
		{"missing challenge", "subscribe", testVerifyToken, "", http.StatusForbidden, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)

			q := url.Values{}
			if tt.mode != "" {
				q.Set("hub.mode", tt.mode)
			}
			q.Set("hub.verify_token", tt.token)
			if tt.verify != "" {
				q.Set("hub.challenge", tt.verify)
			}

			req := httptest.NewRequest(http.MethodGet, "/api/hooks/"+testSlug+"?"+q.Encode(), nil)
			req.SetPathValue("slug", testSlug)
			rec := httptest.NewRecorder()
			f.handler.Challenge(rec, req)

			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if tt.wantCode != http.StatusOK {
				return
			}
			// Meta requires the raw challenge and nothing else — no JSON, no
			// trailing newline.
			if got := rec.Body.String(); got != tt.wantBody {
				t.Errorf("body = %q, want exactly %q", got, tt.wantBody)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
				t.Errorf("Content-Type = %q, want text/plain", ct)
			}
		})
	}
}

func TestChallengeUnknownListener(t *testing.T) {
	f := newFixture(t)
	req := httptest.NewRequest(http.MethodGet,
		"/api/hooks/nope?hub.mode=subscribe&hub.verify_token=x&hub.challenge=y", nil)
	req.SetPathValue("slug", "nope")
	rec := httptest.NewRecorder()
	f.handler.Challenge(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// --- Event persistence ----------------------------------------------------

func TestReceiveStoresByteIdenticalBody(t *testing.T) {
	f := newFixture(t)
	// Deliberately awkward spacing: the stored bytes must match exactly, since
	// the forwarded body has to re-verify against Meta's original signature.
	body := `{"object":"x",  "entry":[ {"id":"WABA_ONE"} ]}`

	if rec := f.post(body, nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	event := f.latestEvent(t)
	if !bytes.Equal(event.RawBody, []byte(body)) {
		t.Errorf("raw_body = %q, want byte-identical %q", event.RawBody, body)
	}
	if !event.SignatureValid {
		t.Error("signature_valid = false, want true")
	}
	if event.PlannedAt != nil {
		t.Error("planned_at should be NULL until the planner runs")
	}
}

func TestReceiveMultiEntryKeepsOneEventWithAllKeys(t *testing.T) {
	f := newFixture(t)
	if rec := f.post(multiEntryBody, nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// One row per request: the payload is never split, so the body stays
	// byte-exact and Meta's own signature keeps verifying downstream.
	n, err := f.store.CountEvents(context.Background(), f.listener.ID)
	if err != nil {
		t.Fatalf("count events: %v", err)
	}
	if n != 1 {
		t.Fatalf("stored events = %d, want exactly 1 for a multi-entry batch", n)
	}

	event := f.latestEvent(t)
	want := []string{"WABA_ONE", "WABA_TWO"}
	if len(event.RoutingKeys) != len(want) {
		t.Fatalf("routing_keys = %v, want %v", event.RoutingKeys, want)
	}
	for i, k := range want {
		if event.RoutingKeys[i] != k {
			t.Errorf("routing_keys[%d] = %q, want %q", i, event.RoutingKeys[i], k)
		}
	}
	if !bytes.Equal(event.RawBody, []byte(multiEntryBody)) {
		t.Error("raw_body was modified for a multi-entry batch")
	}
}

func TestReceiveDedupe(t *testing.T) {
	f := newFixture(t)

	// Meta redelivers the identical payload when it does not see a timely 200.
	for i := range 3 {
		if rec := f.post(metaBody, nil); rec.Code != http.StatusOK {
			t.Fatalf("attempt %d: status = %d, want 200", i+1, rec.Code)
		}
	}

	n, err := f.store.CountEvents(context.Background(), f.listener.ID)
	if err != nil {
		t.Fatalf("count events: %v", err)
	}
	if n != 1 {
		t.Fatalf("stored events = %d, want 1 after three identical deliveries", n)
	}
	// Only the first delivery should have woken the planner.
	if f.notified != 1 {
		t.Errorf("planner notified %d times, want 1", f.notified)
	}
}

func TestReceiveDistinctBodiesAreNotDeduped(t *testing.T) {
	f := newFixture(t)
	bodies := []string{
		`{"entry":[{"id":"WABA_ONE"}],"n":1}`,
		`{"entry":[{"id":"WABA_ONE"}],"n":2}`,
	}
	for _, b := range bodies {
		if rec := f.post(b, nil); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	}

	n, err := f.store.CountEvents(context.Background(), f.listener.ID)
	if err != nil {
		t.Fatalf("count events: %v", err)
	}
	if n != 2 {
		t.Errorf("stored events = %d, want 2 for distinct payloads", n)
	}
}

// --- Edge cases -----------------------------------------------------------

func TestReceiveUnknownListener(t *testing.T) {
	f := newFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/api/hooks/nope", strings.NewReader(metaBody))
	req.SetPathValue("slug", "nope")
	rec := httptest.NewRecorder()
	f.handler.Receive(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestReceiveDisabledListenerReturns200(t *testing.T) {
	f := newFixture(t)
	disabled := false
	if _, err := f.store.UpdateListener(context.Background(), f.listener.ID,
		store.UpdateListenerParams{Enabled: &disabled}); err != nil {
		t.Fatalf("disable listener: %v", err)
	}

	// A disabled listener is our state, not a provider error: returning
	// anything but 200 risks Meta disabling the callback URL.
	if rec := f.post(metaBody, nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a disabled listener", rec.Code)
	}
	n, _ := f.store.CountEvents(context.Background(), f.listener.ID)
	if n != 0 {
		t.Errorf("stored events = %d, want 0 while disabled", n)
	}
}

func TestReceiveBodyTooLarge(t *testing.T) {
	f := newFixture(t)
	huge := `{"pad":"` + strings.Repeat("x", MaxBodyBytes+1024) + `"}`

	if rec := f.post(huge, nil); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

func TestReceiveUnparseableBodyStillStored(t *testing.T) {
	f := newFixture(t)
	// A correctly signed but non-JSON body is a real event: it must be stored
	// (with no routing keys) so it reaches the default subscription rather than
	// vanishing.
	if rec := f.post("this is not json", nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	event := f.latestEvent(t)
	if len(event.RoutingKeys) != 0 {
		t.Errorf("routing_keys = %v, want none", event.RoutingKeys)
	}
}

func TestReceiveVerificationModeNone(t *testing.T) {
	f := newFixture(t)
	mode := "none"
	if _, err := f.store.UpdateListener(context.Background(), f.listener.ID,
		store.UpdateListenerParams{VerificationMode: &mode}); err != nil {
		t.Fatalf("update listener: %v", err)
	}

	// No signature header at all, and it should still be accepted.
	if rec := f.post(metaBody, ptr("")); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with verification disabled", rec.Code)
	}
}

// latestEvent returns the most recently stored event for the fixture listener.
func (f *fixture) latestEvent(t *testing.T) *store.Event {
	t.Helper()
	var id int64
	err := f.store.Pool().QueryRow(context.Background(),
		`SELECT id FROM events WHERE listener_id = $1 ORDER BY id DESC LIMIT 1`,
		f.listener.ID).Scan(&id)
	if err != nil {
		t.Fatalf("find latest event: %v", err)
	}
	event, err := f.store.EventByID(context.Background(), id)
	if err != nil {
		t.Fatalf("load event: %v", err)
	}
	return event
}
