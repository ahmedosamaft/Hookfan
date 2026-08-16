package api

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/aosama/hookfan/internal/crypto"
	"github.com/aosama/hookfan/internal/dispatch"
	"github.com/aosama/hookfan/internal/store"
)

// LinkTokenBytes is the entropy of a generated link token.
const LinkTokenBytes = 32

type Services struct {
	Store    *store.Store
	Cipher   *crypto.Cipher
	Verifier *dispatch.Verifier
	Guard    *dispatch.SSRFGuard
	Log      *slog.Logger
}

type serviceRequest struct {
	Name          *string           `json:"name"`
	URL           *string           `json:"url"`
	Method        *string           `json:"method"`
	TimeoutMS     *int              `json:"timeout_ms"`
	MaxAttempts   *int              `json:"max_attempts"`
	RateLimitRPS  *int              `json:"rate_limit_rps"`
	CustomHeaders map[string]string `json:"custom_headers"`
	Enabled       *bool             `json:"enabled"`
	ResetBreaker  *bool             `json:"reset_breaker"`
}

func (h *Services) List(w http.ResponseWriter, r *http.Request) {
	services, err := h.Store.ListServices(r.Context())
	if err != nil {
		h.Log.Error("list services", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if services == nil {
		services = []*store.Service{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"services": services})
}

func (h *Services) Get(w http.ResponseWriter, r *http.Request) {
	svc, _, err := h.Store.ServiceByPublicID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreError(w, h.Log, err, "get service")
		return
	}
	writeJSON(w, http.StatusOK, svc)
}

// Create registers a service and returns its link token exactly once.
func (h *Services) Create(w http.ResponseWriter, r *http.Request) {
	var req serviceRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	p := store.CreateServiceParams{
		Method:      http.MethodPost,
		TimeoutMS:   10000,
		MaxAttempts: 6,
		Enabled:     true,
	}
	if req.Name != nil {
		p.Name = strings.TrimSpace(*req.Name)
	}
	if req.URL != nil {
		p.URL = strings.TrimSpace(*req.URL)
	}
	if req.Method != nil {
		p.Method = strings.ToUpper(*req.Method)
	}
	if req.TimeoutMS != nil {
		p.TimeoutMS = *req.TimeoutMS
	}
	if req.MaxAttempts != nil {
		p.MaxAttempts = *req.MaxAttempts
	}
	if req.RateLimitRPS != nil {
		p.RateLimitRPS = *req.RateLimitRPS
	}
	p.CustomHeaders = req.CustomHeaders
	if req.Enabled != nil {
		p.Enabled = *req.Enabled
	}

	if msgs := h.validate(r, p); len(msgs) > 0 {
		writeErrors(w, http.StatusBadRequest, msgs)
		return
	}

	token, err := crypto.RandomToken(LinkTokenBytes)
	if err != nil {
		h.Log.Error("generate link token", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	encToken, err := h.Cipher.EncryptString(token)
	if err != nil {
		h.Log.Error("encrypt link token", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	p.LinkToken = encToken

	publicID, err := crypto.RandomToken(16)
	if err != nil {
		h.Log.Error("generate public id", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	p.PublicID = "svc_" + publicID

	svc, err := h.Store.CreateService(r.Context(), p)
	if err != nil {
		writeStoreError(w, h.Log, err, "create service")
		return
	}

	h.Log.Info("service created", "public_id", svc.PublicID, "url", svc.URL)
	// The only time the plaintext token is ever returned, apart from rotation.
	writeJSON(w, http.StatusCreated, map[string]any{
		"service":    svc,
		"link_token": token,
		"warning":    "This token is shown only once. Copy it now and configure it on your backend.",
	})
}

func (h *Services) Update(w http.ResponseWriter, r *http.Request) {
	svc, _, err := h.Store.ServiceByPublicID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreError(w, h.Log, err, "get service")
		return
	}

	var req serviceRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	var msgs []string
	if req.URL != nil {
		if err := h.Guard.CheckURL(r.Context(), strings.TrimSpace(*req.URL)); err != nil {
			msgs = append(msgs, err.Error())
		}
	}
	if req.Method != nil && !validMethod(strings.ToUpper(*req.Method)) {
		msgs = append(msgs, "method must be POST, PUT, or PATCH")
	}
	if req.TimeoutMS != nil && (*req.TimeoutMS < 100 || *req.TimeoutMS > 120000) {
		msgs = append(msgs, "timeout_ms must be between 100 and 120000")
	}
	if req.MaxAttempts != nil && (*req.MaxAttempts < 1 || *req.MaxAttempts > 20) {
		msgs = append(msgs, "max_attempts must be between 1 and 20")
	}
	if req.RateLimitRPS != nil && *req.RateLimitRPS < 0 {
		msgs = append(msgs, "rate_limit_rps must be 0 (unlimited) or greater")
	}
	if len(msgs) > 0 {
		writeErrors(w, http.StatusBadRequest, msgs)
		return
	}

	p := store.UpdateServiceParams{
		Name:          req.Name,
		Method:        req.Method,
		TimeoutMS:     req.TimeoutMS,
		MaxAttempts:   req.MaxAttempts,
		RateLimitRPS:  req.RateLimitRPS,
		CustomHeaders: req.CustomHeaders,
		Enabled:       req.Enabled,
	}
	if req.URL != nil {
		trimmed := strings.TrimSpace(*req.URL)
		p.URL = &trimmed
	}
	if req.ResetBreaker != nil {
		p.ResetBreaker = *req.ResetBreaker
	}

	updated, err := h.Store.UpdateService(r.Context(), svc.ID, p)
	if err != nil {
		writeStoreError(w, h.Log, err, "update service")
		return
	}
	h.Log.Info("service updated", "public_id", updated.PublicID, "status", updated.Status)
	writeJSON(w, http.StatusOK, updated)
}

func (h *Services) Delete(w http.ResponseWriter, r *http.Request) {
	svc, _, err := h.Store.ServiceByPublicID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreError(w, h.Log, err, "get service")
		return
	}
	if err := h.Store.DeleteService(r.Context(), svc.ID); err != nil {
		writeStoreError(w, h.Log, err, "delete service")
		return
	}
	h.Log.Info("service deleted", "public_id", svc.PublicID)
	w.WriteHeader(http.StatusNoContent)
}

// Verify runs the link handshake and records the precise outcome.
func (h *Services) Verify(w http.ResponseWriter, r *http.Request) {
	svc, encToken, err := h.Store.ServiceByPublicID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreError(w, h.Log, err, "get service")
		return
	}
	token, err := h.Cipher.DecryptString(encToken)
	if err != nil {
		h.Log.Error("decrypt link token", "public_id", svc.PublicID, "error", err)
		writeError(w, http.StatusInternalServerError,
			"could not decrypt the link token; has SECRET_ENCRYPTION_KEY changed?")
		return
	}

	result := h.Verifier.Verify(r.Context(), dispatch.VerifyRequest{
		ServicePublicID: svc.PublicID,
		URL:             svc.URL,
		Method:          svc.Method,
		LinkToken:       token,
		TimeoutMS:       svc.TimeoutMS,
		CustomHeaders:   svc.CustomHeaders,
	})

	var updated *store.Service
	if result.OK {
		updated, err = h.Store.MarkVerified(r.Context(), svc.ID)
		h.Log.Info("service verified", "public_id", svc.PublicID, "latency_ms", result.LatencyMS)
	} else {
		// Store the specific reason so the UI can show it inline rather than a
		// generic failure.
		reason := string(result.Kind) + ": " + result.Message
		updated, err = h.Store.MarkVerifyFailed(r.Context(), svc.ID, reason)
		h.Log.Warn("service verification failed",
			"public_id", svc.PublicID, "kind", result.Kind, "reason", result.Message)
	}
	if err != nil {
		writeStoreError(w, h.Log, err, "record verification result")
		return
	}

	code := http.StatusOK
	if !result.OK {
		// 422: the request was valid, the remote service failed the handshake.
		code = http.StatusUnprocessableEntity
	}
	writeJSON(w, code, map[string]any{"service": updated, "result": result})
}

// RotateToken issues a new link token, returning it once.
func (h *Services) RotateToken(w http.ResponseWriter, r *http.Request) {
	svc, _, err := h.Store.ServiceByPublicID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreError(w, h.Log, err, "get service")
		return
	}

	token, err := crypto.RandomToken(LinkTokenBytes)
	if err != nil {
		h.Log.Error("generate link token", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	encToken, err := h.Cipher.EncryptString(token)
	if err != nil {
		h.Log.Error("encrypt link token", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Rotation returns the service to `pending`: the new token has not been
	// proven against the backend, and until it is, no events are delivered.
	updated, err := h.Store.RotateLinkToken(r.Context(), svc.ID, encToken)
	if err != nil {
		writeStoreError(w, h.Log, err, "rotate token")
		return
	}
	h.Log.Info("link token rotated", "public_id", svc.PublicID)
	writeJSON(w, http.StatusOK, map[string]any{
		"service":    updated,
		"link_token": token,
		"warning":    "This token is shown only once. Update your backend, then click Verify.",
	})
}

func (h *Services) validate(r *http.Request, p store.CreateServiceParams) []string {
	var msgs []string
	if p.Name == "" {
		msgs = append(msgs, "name is required")
	}
	if p.URL == "" {
		msgs = append(msgs, "url is required")
	} else if err := h.Guard.CheckURL(r.Context(), p.URL); err != nil {
		msgs = append(msgs, err.Error())
	}
	if !validMethod(p.Method) {
		msgs = append(msgs, "method must be POST, PUT, or PATCH")
	}
	if p.TimeoutMS < 100 || p.TimeoutMS > 120000 {
		msgs = append(msgs, "timeout_ms must be between 100 and 120000")
	}
	if p.MaxAttempts < 1 || p.MaxAttempts > 20 {
		msgs = append(msgs, "max_attempts must be between 1 and 20")
	}
	if p.RateLimitRPS < 0 {
		msgs = append(msgs, "rate_limit_rps must be 0 (unlimited) or greater")
	}
	return msgs
}

func validMethod(m string) bool {
	return m == http.MethodPost || m == http.MethodPut || m == http.MethodPatch
}
