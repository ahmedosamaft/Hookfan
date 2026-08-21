package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The public routes live under /api/ alongside the admin ones, but must stay
// outside RequireAuth: Meta calls the hooks endpoints with no credentials, and
// a probe that needed a token would be useless to a load balancer.
//
// ServeMux gives exact patterns precedence over the "/api/" subtree, which is
// what keeps them separate. This test pins that arrangement — registering a
// public route on the admin mux by mistake would break Meta silently.
func TestPublicRoutesBypassAuthUnderAPIPrefix(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	const token = "the-admin-token"

	reached := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot) // a status no middleware would produce
	}

	adminMux := http.NewServeMux()
	adminMux.HandleFunc("GET /api/listeners", reached)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/healthz", reached)
	mux.HandleFunc("GET /api/readyz", reached)
	mux.HandleFunc("GET /api/hooks/{slug}", reached)
	mux.HandleFunc("POST /api/hooks/{slug}", reached)
	mux.Handle("/api/", CORS(nil, RequireAuth(token, log, adminMux)))

	tests := []struct {
		name, method, path string
		authorize          bool
		want               int
	}{
		{"healthz needs no token", "GET", "/api/healthz", false, http.StatusTeapot},
		{"readyz needs no token", "GET", "/api/readyz", false, http.StatusTeapot},
		{"hook challenge needs no token", "GET", "/api/hooks/demo", false, http.StatusTeapot},
		{"hook receive needs no token", "POST", "/api/hooks/demo", false, http.StatusTeapot},

		{"admin route rejects an absent token", "GET", "/api/listeners", false, http.StatusUnauthorized},
		{"admin route accepts a valid token", "GET", "/api/listeners", true, http.StatusTeapot},

		// Anything else under /api/ falls through to the guarded subtree.
		{"unknown api path is still guarded", "GET", "/api/nope", false, http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			if tt.authorize {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Errorf("%s %s = %d, want %d", tt.method, tt.path, rec.Code, tt.want)
			}
		})
	}
}
