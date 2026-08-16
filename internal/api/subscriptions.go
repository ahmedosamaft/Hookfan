package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/aosama/hookfan/internal/router"
	"github.com/aosama/hookfan/internal/store"
)

type Subscriptions struct {
	Store *store.Store
	Log   *slog.Logger
}

type subscriptionRequest struct {
	ListenerID  *int64          `json:"listener_id"`
	ServiceID   *string         `json:"service_id"`
	FilterType  *string         `json:"filter_type"`
	RoutingKeys []string        `json:"routing_keys"`
	FilterExpr  json.RawMessage `json:"filter_expr"`
	IsDefault   *bool           `json:"is_default"`
	Enabled     *bool           `json:"enabled"`
}

func (h *Subscriptions) List(w http.ResponseWriter, r *http.Request) {
	var listenerID *int64
	if raw := r.URL.Query().Get("listener_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "listener_id must be an integer")
			return
		}
		listenerID = &id
	}

	subs, err := h.Store.ListSubscriptions(r.Context(), listenerID)
	if err != nil {
		h.Log.Error("list subscriptions", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if subs == nil {
		subs = []*store.Subscription{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscriptions": subs})
}

func (h *Subscriptions) Create(w http.ResponseWriter, r *http.Request) {
	var req subscriptionRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	var msgs []string
	if req.ListenerID == nil {
		msgs = append(msgs, "listener_id is required")
	}
	if req.ServiceID == nil || *req.ServiceID == "" {
		msgs = append(msgs, "service_id is required")
	}

	filterType := router.FilterAll
	if req.FilterType != nil {
		filterType = *req.FilterType
	}
	msgs = append(msgs, validateFilter(filterType, req.RoutingKeys, req.FilterExpr)...)

	if len(msgs) > 0 {
		writeErrors(w, http.StatusBadRequest, msgs)
		return
	}

	// Listener and service are resolved before insert so a bad reference gives
	// a clear 404 rather than a foreign-key violation.
	if _, _, err := h.Store.ListenerByID(r.Context(), *req.ListenerID); err != nil {
		writeStoreError(w, h.Log, err, "resolve listener")
		return
	}
	svc, _, err := h.Store.ServiceByPublicID(r.Context(), *req.ServiceID)
	if err != nil {
		writeStoreError(w, h.Log, err, "resolve service")
		return
	}

	p := store.CreateSubscriptionParams{
		ListenerID:  *req.ListenerID,
		ServiceID:   svc.ID,
		FilterType:  filterType,
		RoutingKeys: req.RoutingKeys,
		FilterExpr:  req.FilterExpr,
		Enabled:     true,
	}
	if req.IsDefault != nil {
		p.IsDefault = *req.IsDefault
	}
	if req.Enabled != nil {
		p.Enabled = *req.Enabled
	}

	sub, err := h.Store.CreateSubscription(r.Context(), p)
	if err != nil {
		writeStoreError(w, h.Log, err, "create subscription")
		return
	}
	h.Log.Info("subscription created",
		"id", sub.ID, "listener_id", sub.ListenerID, "service_id", sub.ServicePub,
		"filter_type", sub.FilterType, "is_default", sub.IsDefault)
	writeJSON(w, http.StatusCreated, sub)
}

func (h *Subscriptions) Update(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req subscriptionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ListenerID != nil || req.ServiceID != nil {
		writeError(w, http.StatusBadRequest,
			"listener_id and service_id are immutable; delete this subscription and create another")
		return
	}

	existing, err := h.Store.SubscriptionByID(r.Context(), id)
	if err != nil {
		writeStoreError(w, h.Log, err, "get subscription")
		return
	}

	// Validate against the resulting state, not just the supplied fields: a
	// filter_type change alone can invalidate the stored keys or expression.
	filterType := existing.FilterType
	if req.FilterType != nil {
		filterType = *req.FilterType
	}
	keys := existing.RoutingKeys
	if req.RoutingKeys != nil {
		keys = req.RoutingKeys
	}
	expr := existing.FilterExpr
	if len(req.FilterExpr) > 0 {
		expr = req.FilterExpr
	}
	if msgs := validateFilter(filterType, keys, expr); len(msgs) > 0 {
		writeErrors(w, http.StatusBadRequest, msgs)
		return
	}

	sub, err := h.Store.UpdateSubscription(r.Context(), id, store.UpdateSubscriptionParams{
		FilterType:  req.FilterType,
		RoutingKeys: req.RoutingKeys,
		FilterExpr:  req.FilterExpr,
		IsDefault:   req.IsDefault,
		Enabled:     req.Enabled,
	})
	if err != nil {
		writeStoreError(w, h.Log, err, "update subscription")
		return
	}
	h.Log.Info("subscription updated", "id", sub.ID)
	writeJSON(w, http.StatusOK, sub)
}

func (h *Subscriptions) Delete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := h.Store.DeleteSubscription(r.Context(), id); err != nil {
		writeStoreError(w, h.Log, err, "delete subscription")
		return
	}
	h.Log.Info("subscription deleted", "id", id)
	w.WriteHeader(http.StatusNoContent)
}

func validateFilter(filterType string, keys []string, expr json.RawMessage) []string {
	var msgs []string
	switch filterType {
	case router.FilterAll:
	case router.FilterRoutingKeyIn:
		if len(keys) == 0 {
			msgs = append(msgs,
				`routing_keys must contain at least one value when filter_type is "routing_key_in"`)
		}
	case router.FilterJSONPathMatch:
		conds, err := router.ParseConditions(expr)
		if err != nil {
			msgs = append(msgs, err.Error())
		} else if len(conds) == 0 {
			msgs = append(msgs,
				`filter_expr must contain at least one condition when filter_type is "jsonpath_match"`)
		}
	default:
		msgs = append(msgs,
			`filter_type must be one of "all", "routing_key_in", "jsonpath_match"`)
	}
	return msgs
}
