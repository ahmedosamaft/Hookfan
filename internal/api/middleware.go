package api

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/aosama/hookfan/internal/crypto"
)

// RequireAuth guards the admin API with a single bearer token from the
// environment. The token is compared in constant time.
func RequireAuth(token string, log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Preflight carries no credentials by design, so it is answered before
		// the auth check; CORS has already restricted the allowed origins.
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}

		const prefix = "Bearer "
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, prefix) ||
			!crypto.EqualConstantTime(strings.TrimPrefix(auth, prefix), token) {
			log.Warn("admin request rejected", "path", r.URL.Path, "method", r.Method)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// CORS answers preflight requests and adds the response headers the UI needs.
//
// Origins are an explicit allowlist and the default is to deny: the admin API
// carries a bearer token held in sessionStorage, and a wildcard origin
// alongside that is a bad combination.
func CORS(allowed []string, next http.Handler) http.Handler {
	index := make(map[string]struct{}, len(allowed))
	for _, o := range allowed {
		index[o] = struct{}{}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			if _, ok := index[strings.TrimSuffix(origin, "/")]; ok {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				w.Header().Set("Access-Control-Max-Age", "600")
				// Responses differ by Origin, so caches must key on it.
				w.Header().Add("Vary", "Origin")
			}
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
