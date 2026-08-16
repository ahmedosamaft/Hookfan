package dispatch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aosama/hookfan/internal/crypto"
)

const testToken = "test-link-token"

// echoServer is a correct receiver implementation: it validates the token and
// signature, then echoes the challenge.
func echoServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		if r.Header.Get("X-Hookfan-Token") != testToken {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		sig := strings.TrimPrefix(r.Header.Get("X-Hookfan-Signature"), "sha256=")
		if !crypto.VerifyHex([]byte(testToken), body, sig) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}

		var payload struct {
			Type      string `json:"type"`
			Challenge string `json:"challenge"`
			ServiceID string `json:"service_id"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"challenge": payload.Challenge})
	}))
}

func testVerifier() *Verifier {
	// httptest servers listen on loopback, so private targets must be allowed.
	return NewVerifier(NewSSRFGuard(true))
}

func request(url string) VerifyRequest {
	return VerifyRequest{
		ServicePublicID: "svc_test",
		URL:             url,
		Method:          http.MethodPost,
		LinkToken:       testToken,
		TimeoutMS:       2000,
	}
}

func TestVerifySuccess(t *testing.T) {
	srv := echoServer(t)
	defer srv.Close()

	result := testVerifier().Verify(context.Background(), request(srv.URL))
	if !result.OK {
		t.Fatalf("Verify failed: kind=%s message=%s", result.Kind, result.Message)
	}
	if result.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", result.StatusCode)
	}
}

// The spec accepts a bare challenge string as text/plain too.
func TestVerifyAcceptsPlainTextEcho(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Challenge string `json:"challenge"`
		}
		_ = json.Unmarshal(body, &payload)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, payload.Challenge)
	}))
	defer srv.Close()

	if result := testVerifier().Verify(context.Background(), request(srv.URL)); !result.OK {
		t.Fatalf("plain-text echo rejected: kind=%s message=%s", result.Kind, result.Message)
	}
}

// Each failure must be distinguishable: a generic error here wastes hours.
func TestVerifyFailureModes(t *testing.T) {
	tests := []struct {
		name     string
		handler  http.HandlerFunc
		wantKind FailureKind
	}{
		{
			name: "wrong challenge echoed",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]string{"challenge": "not-the-challenge"})
			},
			wantKind: FailChallenge,
		},
		{
			name: "empty body",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			},
			wantKind: FailChallenge,
		},
		{
			name: "json without a challenge field",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			},
			wantKind: FailChallenge,
		},
		{
			name: "500 from the service",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "boom", http.StatusInternalServerError)
			},
			wantKind: FailHTTPStatus,
		},
		{
			name: "404 from the service",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "no such route", http.StatusNotFound)
			},
			wantKind: FailHTTPStatus,
		},
		{
			// A redirect is not followed: the token must be proven against the
			// configured URL, not wherever it points.
			name: "redirect is not followed",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "https://example.test/elsewhere", http.StatusFound)
			},
			wantKind: FailHTTPStatus,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()

			result := testVerifier().Verify(context.Background(), request(srv.URL))
			if result.OK {
				t.Fatal("Verify succeeded, want failure")
			}
			if result.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q (message: %s)", result.Kind, tt.wantKind, result.Message)
			}
			if result.Message == "" {
				t.Error("Message is empty; the UI needs a specific reason")
			}
		})
	}
}

func TestVerifyTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	req := request(srv.URL)
	req.TimeoutMS = 200

	start := time.Now()
	result := testVerifier().Verify(context.Background(), req)
	elapsed := time.Since(start)

	if result.OK {
		t.Fatal("Verify succeeded, want a timeout")
	}
	if result.Kind != FailTimeout {
		t.Errorf("Kind = %q, want %q (message: %s)", result.Kind, FailTimeout, result.Message)
	}
	// The configured timeout must actually bound the call.
	if elapsed > time.Second {
		t.Errorf("took %v, want the 200ms timeout to apply", elapsed)
	}
}

func TestVerifyConnectionRefused(t *testing.T) {
	// Bind then immediately close, so the port is almost certainly free.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	result := testVerifier().Verify(context.Background(), request(url))
	if result.OK {
		t.Fatal("Verify succeeded against a closed port")
	}
	if result.Kind != FailConnRefused && result.Kind != FailNetwork {
		t.Errorf("Kind = %q, want %q", result.Kind, FailConnRefused)
	}
}

func TestVerifyDNSFailure(t *testing.T) {
	req := request("http://this-host-should-never-resolve.invalid/hook")
	req.TimeoutMS = 3000

	result := testVerifier().Verify(context.Background(), req)
	if result.OK {
		t.Fatal("Verify succeeded against an unresolvable host")
	}
	if result.Kind != FailDNS && result.Kind != FailNetwork {
		t.Errorf("Kind = %q, want %q (message: %s)", result.Kind, FailDNS, result.Message)
	}
}

// The receiving service must be able to authenticate the gateway, so the token
// and a valid signature accompany every handshake.
func TestVerifySendsTokenAndSignature(t *testing.T) {
	var gotToken, gotSig, gotEvent string
	var gotBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotToken = r.Header.Get("X-Hookfan-Token")
		gotSig = r.Header.Get("X-Hookfan-Signature")
		gotEvent = r.Header.Get("X-Hookfan-Event")

		var payload struct {
			Challenge string `json:"challenge"`
		}
		_ = json.Unmarshal(gotBody, &payload)
		_ = json.NewEncoder(w).Encode(map[string]string{"challenge": payload.Challenge})
	}))
	defer srv.Close()

	if result := testVerifier().Verify(context.Background(), request(srv.URL)); !result.OK {
		t.Fatalf("Verify failed: %s", result.Message)
	}

	if gotToken != testToken {
		t.Errorf("X-Hookfan-Token = %q, want %q", gotToken, testToken)
	}
	if gotEvent != "link.verify" {
		t.Errorf("X-Hookfan-Event = %q, want link.verify", gotEvent)
	}
	if !crypto.VerifyHex([]byte(testToken), gotBody, strings.TrimPrefix(gotSig, "sha256=")) {
		t.Errorf("X-Hookfan-Signature %q does not verify against the body", gotSig)
	}

	var payload map[string]string
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if payload["type"] != "link.verify" {
		t.Errorf(`body type = %q, want "link.verify"`, payload["type"])
	}
	if payload["service_id"] != "svc_test" {
		t.Errorf("body service_id = %q, want svc_test", payload["service_id"])
	}
	// 16 random bytes, hex-encoded.
	if len(payload["challenge"]) != 32 {
		t.Errorf("challenge = %q, want 32 hex characters", payload["challenge"])
	}
}

func TestVerifyRejectsPrivateTargetWhenGuarded(t *testing.T) {
	srv := echoServer(t)
	defer srv.Close()

	// With the guard active, a loopback target must be refused outright.
	v := NewVerifier(NewSSRFGuard(false))
	result := v.Verify(context.Background(), request(srv.URL))

	if result.OK {
		t.Fatal("Verify succeeded against a loopback address with the SSRF guard active")
	}
	if result.Kind != FailBlockedLocal {
		t.Errorf("Kind = %q, want %q (message: %s)", result.Kind, FailBlockedLocal, result.Message)
	}
}
