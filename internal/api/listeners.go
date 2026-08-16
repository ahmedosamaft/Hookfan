package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/aosama/hookfan/internal/crypto"
	"github.com/aosama/hookfan/internal/router"
	"github.com/aosama/hookfan/internal/store"
)

// slugPattern keeps slugs URL-safe: they appear directly in the callback path
// that gets pasted into the Meta app dashboard.
var slugPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)

type Listeners struct {
	Store  *store.Store
	Cipher *crypto.Cipher
	Log    *slog.Logger
}

type listenerRequest struct {
	Name                 *string `json:"name"`
	Slug                 *string `json:"slug"`
	Provider             *string `json:"provider"`
	VerificationMode     *string `json:"verification_mode"`
	SignatureHeader      *string `json:"signature_header"`
	SignaturePrefix      *string `json:"signature_prefix"`
	Secret               *string `json:"secret"`
	ChallengeVerifyToken *string `json:"challenge_verify_token"`
	RoutingKeyPath       *string `json:"routing_key_path"`
	Enabled              *bool   `json:"enabled"`
}

func (h *Listeners) List(w http.ResponseWriter, r *http.Request) {
	listeners, err := h.Store.ListListeners(r.Context())
	if err != nil {
		h.Log.Error("list listeners", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if listeners == nil {
		listeners = []*store.Listener{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"listeners": listeners})
}

func (h *Listeners) Get(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	listener, _, err := h.Store.ListenerByID(r.Context(), id)
	if err != nil {
		writeStoreError(w, h.Log, err, "get listener")
		return
	}
	writeJSON(w, http.StatusOK, listener)
}

func (h *Listeners) Create(w http.ResponseWriter, r *http.Request) {
	var req listenerRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	p := store.CreateListenerParams{
		Provider:         "meta",
		VerificationMode: "hmac_sha256",
		SignatureHeader:  "X-Hub-Signature-256",
		SignaturePrefix:  "sha256=",
		RoutingKeyPath:   "entry[*].id",
		Enabled:          true,
	}
	if req.Name != nil {
		p.Name = strings.TrimSpace(*req.Name)
	}
	if req.Slug != nil {
		p.Slug = strings.TrimSpace(*req.Slug)
	}
	if req.Provider != nil {
		p.Provider = *req.Provider
	}
	if req.VerificationMode != nil {
		p.VerificationMode = *req.VerificationMode
	}
	if req.SignatureHeader != nil {
		p.SignatureHeader = *req.SignatureHeader
	}
	if req.SignaturePrefix != nil {
		p.SignaturePrefix = *req.SignaturePrefix
	}
	if req.RoutingKeyPath != nil {
		p.RoutingKeyPath = *req.RoutingKeyPath
	}
	if req.ChallengeVerifyToken != nil {
		p.ChallengeVerifyToken = *req.ChallengeVerifyToken
	}
	if req.Enabled != nil {
		p.Enabled = *req.Enabled
	}

	if msgs := validateListener(p, req.Secret); len(msgs) > 0 {
		writeErrors(w, http.StatusBadRequest, msgs)
		return
	}

	if req.Secret != nil && *req.Secret != "" {
		enc, err := h.Cipher.EncryptString(*req.Secret)
		if err != nil {
			h.Log.Error("encrypt listener secret", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		p.Secret = enc
	}

	listener, err := h.Store.CreateListener(r.Context(), p)
	if err != nil {
		writeStoreError(w, h.Log, err, "create listener")
		return
	}
	h.Log.Info("listener created", "id", listener.ID, "slug", listener.Slug)
	writeJSON(w, http.StatusCreated, listener)
}

func (h *Listeners) Update(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req listenerRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// The slug is immutable: it is baked into the callback URL already
	// registered with the provider.
	if req.Slug != nil {
		writeError(w, http.StatusBadRequest, "slug is immutable; create a new listener instead")
		return
	}

	var msgs []string
	if req.Provider != nil && !validProvider(*req.Provider) {
		msgs = append(msgs, `provider must be "meta" or "generic"`)
	}
	if req.VerificationMode != nil && !validVerificationMode(*req.VerificationMode) {
		msgs = append(msgs, `verification_mode must be "none" or "hmac_sha256"`)
	}
	if req.RoutingKeyPath != nil {
		if _, err := router.ParsePath(*req.RoutingKeyPath); err != nil {
			msgs = append(msgs, err.Error())
		}
	}
	if len(msgs) > 0 {
		writeErrors(w, http.StatusBadRequest, msgs)
		return
	}

	p := store.UpdateListenerParams{
		Name:             req.Name,
		Provider:         req.Provider,
		VerificationMode: req.VerificationMode,
		SignatureHeader:  req.SignatureHeader,
		SignaturePrefix:  req.SignaturePrefix,
		RoutingKeyPath:   req.RoutingKeyPath,
		Enabled:          req.Enabled,
	}
	// An empty-string token clears it; a nil pointer leaves it alone.
	if req.ChallengeVerifyToken != nil {
		p.ChallengeVerifyToken = req.ChallengeVerifyToken
	}
	if req.Secret != nil && *req.Secret != "" {
		enc, err := h.Cipher.EncryptString(*req.Secret)
		if err != nil {
			h.Log.Error("encrypt listener secret", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		p.Secret = enc
	}

	listener, err := h.Store.UpdateListener(r.Context(), id, p)
	if err != nil {
		writeStoreError(w, h.Log, err, "update listener")
		return
	}
	h.Log.Info("listener updated", "id", listener.ID, "slug", listener.Slug)
	writeJSON(w, http.StatusOK, listener)
}

func (h *Listeners) Delete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := h.Store.DeleteListener(r.Context(), id); err != nil {
		writeStoreError(w, h.Log, err, "delete listener")
		return
	}
	h.Log.Info("listener deleted", "id", id)
	w.WriteHeader(http.StatusNoContent)
}

func validateListener(p store.CreateListenerParams, secret *string) []string {
	var msgs []string
	if p.Name == "" {
		msgs = append(msgs, "name is required")
	}
	if p.Slug == "" {
		msgs = append(msgs, "slug is required")
	} else if !slugPattern.MatchString(p.Slug) {
		msgs = append(msgs, "slug must be lowercase alphanumeric with hyphens, 1-64 characters, e.g. whatsapp-prod")
	}
	if !validProvider(p.Provider) {
		msgs = append(msgs, `provider must be "meta" or "generic"`)
	}
	if !validVerificationMode(p.VerificationMode) {
		msgs = append(msgs, `verification_mode must be "none" or "hmac_sha256"`)
	}
	// Without a secret, hmac_sha256 would reject every webhook at runtime, so
	// the mismatch is caught at create time instead.
	if p.VerificationMode == "hmac_sha256" && (secret == nil || *secret == "") {
		msgs = append(msgs, "secret is required when verification_mode is hmac_sha256")
	}
	if p.Provider == "meta" && p.ChallengeVerifyToken == "" {
		msgs = append(msgs, "challenge_verify_token is required for meta listeners; Meta sends it during the GET handshake")
	}
	if _, err := router.ParsePath(p.RoutingKeyPath); err != nil {
		msgs = append(msgs, err.Error())
	}
	return msgs
}

func validProvider(p string) bool { return p == "meta" || p == "generic" }

func validVerificationMode(m string) bool { return m == "none" || m == "hmac_sha256" }

// pathID parses the {id} path segment.
func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	return id, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func writeStoreError(w http.ResponseWriter, log *slog.Logger, err error, op string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	default:
		log.Error(op, "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}
