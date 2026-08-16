// Package api serves the admin JSON API and the health endpoints.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/aosama/hookfan/internal/store"
)

// PlannerLagThreshold is how far the planner may fall behind before /readyz
// reports the instance as not ready. Well above normal scheduling jitter, low
// enough that a wedged planner is caught quickly.
const PlannerLagThreshold = 60 * time.Second

type Health struct {
	Store *store.Store
	Log   *slog.Logger
}

// Healthz is a liveness probe: the process is up and serving. It deliberately
// does not touch the database, so a database blip never causes the container
// to be restarted.
func (h *Health) Healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// Readyz is a readiness probe: this instance can do useful work. It checks the
// database and the planner's backlog.
func (h *Health) Readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	body := map[string]any{"status": "ok"}
	code := http.StatusOK

	if err := h.Store.Ping(ctx); err != nil {
		h.Log.Warn("readyz: database unreachable", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "unavailable",
			"checks": map[string]any{"database": "unreachable"},
		})
		return
	}

	checks := map[string]any{"database": "ok"}
	lag, err := h.Store.PlannerLag(ctx)
	switch {
	case err != nil:
		h.Log.Warn("readyz: planner lag query failed", "error", err)
		checks["planner"] = "unknown"
	default:
		checks["planner_lag_seconds"] = lag.Seconds()
		if lag > PlannerLagThreshold {
			h.Log.Warn("readyz: planner is behind", "lag", lag.Round(time.Second))
			checks["planner"] = "behind"
			body["status"] = "degraded"
			code = http.StatusServiceUnavailable
		} else {
			checks["planner"] = "ok"
		}
	}
	body["checks"] = checks
	writeJSON(w, code, body)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError sends a single-message error response.
func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

// writeErrors sends a validation failure listing every problem at once, so the
// caller can fix them in one pass rather than one request at a time.
func writeErrors(w http.ResponseWriter, code int, msgs []string) {
	writeJSON(w, code, map[string]any{
		"error":  strings.Join(msgs, "; "),
		"errors": msgs,
	})
}
