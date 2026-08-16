// Package ingest implements the public webhook endpoints: the provider
// challenge handshake and the event receive path.
//
// The receive path is deliberately minimal — read the body, verify the
// signature, write one row, return 200. No subscription matching, no delivery
// creation, no outbound calls happen on the request goroutine; the planner
// stage owns all of that. Meta retries aggressively and disables callback URLs
// that misbehave, so the endpoint answers quickly and answers 200.
package ingest

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/aosama/hookfan/internal/crypto"
	"github.com/aosama/hookfan/internal/router"
	"github.com/aosama/hookfan/internal/store"
)

// MaxBodyBytes caps an inbound webhook. Meta payloads are far smaller; the cap
// stops an unauthenticated endpoint from being a memory DoS.
const MaxBodyBytes = 1 << 20 // 1MB

// Notifier is told that an event landed, so the planner can wake immediately
// instead of waiting for its poll tick.
type Notifier interface {
	NotifyEvent()
}

// Publisher receives a copy of each accepted event for the live UI feed.
// Declared here rather than imported so this package stays independent of the
// admin API.
type Publisher interface {
	Publish(kind string, data any)
}

type Handler struct {
	Store  *store.Store
	Cipher *crypto.Cipher
	Log    *slog.Logger
	// Notify is optional; ingest works without it because the planner also polls.
	Notify Notifier
	// Stream is optional; nothing on the ingest path depends on a UI listening.
	Stream Publisher
}

// Challenge handles GET /hooks/{slug}, the provider subscription handshake.
//
// Meta calls this unauthenticated when the callback URL is first configured:
// it must echo hub.challenge verbatim as text/plain when hub.verify_token
// matches, and 403 otherwise.
func (h *Handler) Challenge(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	q := r.URL.Query()
	mode := q.Get("hub.mode")
	token := q.Get("hub.verify_token")
	challenge := q.Get("hub.challenge")

	listener, _, err := h.Store.ListenerBySlug(r.Context(), slug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.Log.Warn("challenge for unknown listener", "slug", slug)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h.Log.Error("challenge lookup failed", "slug", slug, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if mode != "subscribe" || challenge == "" {
		h.Log.Warn("challenge rejected: bad parameters",
			"slug", slug, "mode", mode, "has_challenge", challenge != "")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	// Constant-time compare: the verify token is a shared secret, and a
	// byte-by-byte comparison would leak it to a timing attack.
	if listener.ChallengeVerifyToken == "" ||
		!crypto.EqualConstantTime(listener.ChallengeVerifyToken, token) {
		h.Log.Warn("challenge rejected: verify token mismatch", "slug", slug)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// The response body must be the raw challenge and nothing else.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(w, challenge); err != nil {
		h.Log.Warn("challenge write failed", "slug", slug, "error", err)
		return
	}
	h.Log.Info("challenge verified", "slug", slug)
}

// Receive handles POST /hooks/{slug}.
func (h *Handler) Receive(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	slug := r.PathValue("slug")

	listener, encSecret, err := h.Store.ListenerBySlug(r.Context(), slug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.Log.Warn("webhook for unknown listener", "slug", slug)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		h.Log.Error("listener lookup failed", "slug", slug, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !listener.Enabled {
		// 200 rather than an error: a disabled listener is our state, not a
		// provider mistake, and errors here get callback URLs disabled.
		h.Log.Info("webhook for disabled listener ignored", "slug", slug)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Read the exact bytes before any decoding. Re-marshalling would reorder
	// keys and change whitespace, and the signature covers these bytes only.
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			h.Log.Warn("webhook body too large", "slug", slug, "limit_bytes", MaxBodyBytes)
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		h.Log.Warn("webhook body read failed", "slug", slug, "error", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if !h.verify(w, r, listener, encSecret, body) {
		return // verify has already written the response and logged.
	}

	keys := router.ExtractRoutingKeys(body, listener.RoutingKeyPath)

	// One event per received request; raw_body stays byte-identical, and every
	// entry's id is recorded so a batch can match several subscriptions.
	event, duplicate, err := h.Store.InsertEvent(r.Context(), store.InsertEventParams{
		ListenerID:     listener.ID,
		RoutingKeys:    keys,
		RawBody:        body,
		Headers:        collectHeaders(r.Header),
		ContentType:    r.Header.Get("Content-Type"),
		SignatureValid: true,
		DedupeKey:      crypto.BodyHash(body),
	})
	if err != nil {
		h.Log.Error("event insert failed", "slug", slug, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if duplicate {
		// Counted, not silent: if this ever fires for genuinely distinct
		// payloads, the log is how anyone would find out.
		h.Log.Info("duplicate webhook ignored",
			"slug", slug,
			"body_sha256", crypto.BodyHash(body),
			"duration_ms", time.Since(start).Milliseconds())
		w.WriteHeader(http.StatusOK)
		return
	}

	if h.Notify != nil {
		h.Notify.NotifyEvent()
	}
	if h.Stream != nil {
		h.Stream.Publish("event.received", map[string]any{
			"id":              event.ID,
			"listener_id":     listener.ID,
			"listener_slug":   listener.Slug,
			"routing_keys":    keys,
			"received_at":     event.ReceivedAt,
			"signature_valid": true,
			"body_bytes":      len(body),
		})
	}

	h.Log.Info("event received",
		"slug", slug,
		"event_id", event.ID,
		"routing_keys", keys,
		"bytes", len(body),
		"duration_ms", time.Since(start).Milliseconds())

	// Always 200 on a verified webhook, even when no subscription matches:
	// routing is our concern, and Meta disables endpoints that return errors.
	w.WriteHeader(http.StatusOK)
}

// verify checks the provider signature, writing the response itself when the
// request must be rejected. It reports whether ingest should continue.
func (h *Handler) verify(w http.ResponseWriter, r *http.Request, l *store.Listener, encSecret, body []byte) bool {
	if l.VerificationMode == "none" {
		return true
	}

	if len(encSecret) == 0 {
		h.Log.Error("listener requires signature verification but has no secret", "slug", l.Slug)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	}
	secret, err := h.Cipher.Decrypt(encSecret)
	if err != nil {
		// Almost always SECRET_ENCRYPTION_KEY having changed.
		h.Log.Error("listener secret could not be decrypted", "slug", l.Slug, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	}

	header := l.SignatureHeader
	if header == "" {
		header = "X-Hub-Signature-256"
	}
	provided := r.Header.Get(header)
	if provided == "" {
		h.Log.Warn("webhook rejected: signature header missing",
			"slug", l.Slug, "header", header, "body_sha256", crypto.BodyHash(body))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}

	// Meta sends "sha256=<hex>". A missing or wrong prefix is a malformed
	// signature, not a mismatch, and is rejected rather than tolerated.
	digest := provided
	if l.SignaturePrefix != "" {
		if !strings.HasPrefix(provided, l.SignaturePrefix) {
			h.Log.Warn("webhook rejected: malformed signature prefix",
				"slug", l.Slug, "want_prefix", l.SignaturePrefix,
				"body_sha256", crypto.BodyHash(body))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return false
		}
		digest = strings.TrimPrefix(provided, l.SignaturePrefix)
	}

	if !crypto.VerifyHex(secret, body, digest) {
		// The body is never logged: it may hold message contents. Its hash is
		// enough to correlate with a provider-side redelivery.
		h.Log.Warn("webhook rejected: signature mismatch",
			"slug", l.Slug, "body_sha256", crypto.BodyHash(body), "bytes", len(body))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

// headerAllowlist is the set of headers preserved with an event. An allowlist
// rather than a copy of everything: inbound requests carry cookies and
// authorization headers that must not be stored.
var headerAllowlist = []string{
	"Content-Type",
	"User-Agent",
	"X-Hub-Signature-256",
	"X-Hub-Signature",
	"X-Forwarded-For",
	"X-Request-Id",
}

func collectHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(headerAllowlist))
	for _, name := range headerAllowlist {
		if v := h.Get(name); v != "" {
			out[name] = v
		}
	}
	return out
}
