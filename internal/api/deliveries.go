package api

import (
	"log/slog"
	"net/http"

	"github.com/aosama/hookfan/internal/dispatch"
	"github.com/aosama/hookfan/internal/store"
)

type Deliveries struct {
	Store      *store.Store
	Dispatcher *dispatch.Dispatcher
	Log        *slog.Logger
}

// Retry requeues a single delivery for immediate re-attempt.
func (h *Deliveries) Retry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := h.Store.RetryDelivery(r.Context(), id); err != nil {
		writeStoreError(w, h.Log, err, "retry delivery")
		return
	}
	// Wake a worker rather than waiting for the next poll: a retry is an
	// operator action and should look immediate.
	h.Dispatcher.Notify()

	h.Log.Info("delivery requeued by operator", "delivery_id", id)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"delivery_id": id,
		"status":      "pending",
	})
}
