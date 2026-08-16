package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/aosama/hookfan/internal/planner"
	"github.com/aosama/hookfan/internal/store"
)

type Events struct {
	Store   *store.Store
	Planner *planner.Planner
	Log     *slog.Logger
}

// List returns a cursor-paginated page of events.
func (h *Events) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.EventFilter{
		RoutingKey: q.Get("routing_key"),
		Cursor:     q.Get("cursor"),
	}

	var msgs []string
	if raw := q.Get("listener_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			msgs = append(msgs, "listener_id must be an integer")
		} else {
			f.ListenerID = &id
		}
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			msgs = append(msgs, "limit must be a positive integer")
		} else {
			f.Limit = n
		}
	}
	if raw := q.Get("since"); raw != "" {
		t, err := parseTime(raw)
		if err != nil {
			msgs = append(msgs, "since must be an RFC3339 timestamp, e.g. 2026-08-16T12:00:00Z")
		} else {
			f.Since = &t
		}
	}
	if raw := q.Get("until"); raw != "" {
		t, err := parseTime(raw)
		if err != nil {
			msgs = append(msgs, "until must be an RFC3339 timestamp, e.g. 2026-08-16T12:00:00Z")
		} else {
			f.Until = &t
		}
	}
	if raw := q.Get("signature_valid"); raw != "" {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			msgs = append(msgs, "signature_valid must be true or false")
		} else {
			f.SigValid = &b
		}
	}
	if len(msgs) > 0 {
		writeErrors(w, http.StatusBadRequest, msgs)
		return
	}

	page, err := h.Store.ListEvents(r.Context(), f)
	if err != nil {
		// A bad cursor is the caller's mistake, not a server fault.
		if err.Error() == "invalid cursor" {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
		h.Log.Error("list events", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// eventDetail is a single event with its body and delivery attempts.
type eventDetail struct {
	*store.Event
	ListenerSlug string            `json:"listener_slug"`
	Body         string            `json:"body"`
	BodyEncoding string            `json:"body_encoding"`
	BodyBytes    int               `json:"body_bytes"`
	Deliveries   []*store.Delivery `json:"deliveries"`
}

// Get returns one event including its raw body and every delivery.
func (h *Events) Get(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}

	event, err := h.Store.EventByID(r.Context(), id)
	if err != nil {
		writeStoreError(w, h.Log, err, "get event")
		return
	}
	deliveries, err := h.Store.DeliveriesForEvent(r.Context(), id)
	if err != nil {
		h.Log.Error("load deliveries", "event_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if deliveries == nil {
		deliveries = []*store.Delivery{}
	}

	listener, _, err := h.Store.ListenerByID(r.Context(), event.ListenerID)
	slug := ""
	if err == nil {
		slug = listener.Slug
	}

	detail := eventDetail{
		Event:        event,
		ListenerSlug: slug,
		BodyBytes:    len(event.RawBody),
		Deliveries:   deliveries,
	}
	// The stored body is arbitrary bytes. Valid UTF-8 is returned as-is so the
	// UI can pretty-print it; anything else would corrupt the JSON response, so
	// it is base64-encoded and labelled.
	if utf8.Valid(event.RawBody) {
		detail.Body = string(event.RawBody)
		detail.BodyEncoding = "utf8"
	} else {
		detail.Body = encodeBase64(event.RawBody)
		detail.BodyEncoding = "base64"
	}

	writeJSON(w, http.StatusOK, detail)
}

// Replay rebuilds an event's delivery set from the subscriptions in force now.
func (h *Events) Replay(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := h.Store.ReplayEvent(r.Context(), id); err != nil {
		writeStoreError(w, h.Log, err, "replay event")
		return
	}
	// Wake the planner so a replay looks immediate rather than waiting a tick.
	h.Planner.NotifyEvent()

	h.Log.Info("event replayed by operator", "event_id", id)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"event_id": id,
		"status":   "replanning",
		"note":     "deliveries are rebuilt from the subscriptions currently in force",
	})
}

// Stats serves the dashboard rollup.
func (h *Events) Stats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.Store.GetStats(r.Context())
	if err != nil {
		h.Log.Error("get stats", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	minutes := 60
	if raw := r.URL.Query().Get("sparkline_minutes"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			minutes = n
		}
	}
	series, err := h.Store.EventsPerMinute(r.Context(), minutes)
	if err != nil {
		h.Log.Error("events per minute", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if series == nil {
		series = []store.TimeBucket{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"windows":           stats.Windows,
		"services":          stats.Services,
		"queue":             stats.Queue,
		"events_per_minute": series,
	})
}

// parseTime accepts RFC3339, with or without sub-second precision.
func parseTime(raw string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, raw)
}

func encodeBase64(b []byte) string {
	enc, _ := json.Marshal(b) // []byte marshals to a base64 JSON string
	// Trim the surrounding quotes json.Marshal adds.
	if len(enc) >= 2 {
		return string(enc[1 : len(enc)-1])
	}
	return ""
}
