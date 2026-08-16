// Package dispatch performs outbound HTTP to registered services: the link
// handshake that proves a service owns its token, and (later) the delivery
// worker pool.
package dispatch

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/aosama/hookfan/internal/crypto"
)

// VerifyTimeout is the ceiling for the whole link handshake.
const VerifyTimeout = 10 * time.Second

// maxVerifyBody caps how much of the service's response is read. The challenge
// echo is tiny; anything larger is a misconfigured endpoint.
const maxVerifyBody = 64 << 10

// FailureKind classifies why a handshake failed. Operators debugging an
// onboarding problem need to know whether DNS failed, TLS failed, or the
// service simply echoed the wrong value — a generic "verification failed"
// wastes hours.
type FailureKind string

const (
	FailDNS          FailureKind = "dns_error"
	FailConnRefused  FailureKind = "connection_refused"
	FailTLS          FailureKind = "tls_error"
	FailTimeout      FailureKind = "timeout"
	FailHTTPStatus   FailureKind = "http_status"
	FailChallenge    FailureKind = "wrong_challenge"
	FailUnreadable   FailureKind = "unreadable_response"
	FailBlockedLocal FailureKind = "blocked_private_address"
	FailNetwork      FailureKind = "network_error"
)

// VerifyResult describes the outcome of one handshake attempt.
type VerifyResult struct {
	OK         bool          `json:"ok"`
	Kind       FailureKind   `json:"kind,omitempty"`
	Message    string        `json:"message,omitempty"`
	StatusCode int           `json:"status_code,omitempty"`
	Latency    time.Duration `json:"-"`
	LatencyMS  int64         `json:"latency_ms"`
	// Echoed is what the service actually sent back, truncated. Shown in the
	// UI next to the expected value so a mismatch is immediately obvious.
	Echoed string `json:"echoed,omitempty"`
}

// VerifyRequest carries everything needed for one handshake.
type VerifyRequest struct {
	ServicePublicID string
	URL             string
	Method          string
	LinkToken       string
	TimeoutMS       int
	CustomHeaders   map[string]string
}

// verifyPayload is the body sent to the service.
type verifyPayload struct {
	Type      string `json:"type"`
	Challenge string `json:"challenge"`
	ServiceID string `json:"service_id"`
}

// Verifier runs link handshakes.
type Verifier struct {
	Client *http.Client
	Guard  *SSRFGuard
}

// NewVerifier builds a Verifier with a client tuned for one-off handshakes.
func NewVerifier(guard *SSRFGuard) *Verifier {
	return &Verifier{
		Client: &http.Client{
			Timeout: VerifyTimeout,
			Transport: &http.Transport{
				DialContext:           guard.DialContext,
				TLSHandshakeTimeout:   5 * time.Second,
				ResponseHeaderTimeout: VerifyTimeout,
				MaxIdleConnsPerHost:   2,
			},
			// The handshake proves the token against a specific URL; following
			// a redirect elsewhere would prove it against a different host.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		Guard: guard,
	}
}

// Verify performs the link handshake: send a random challenge signed with the
// link token, and require the service to echo it back.
func (v *Verifier) Verify(ctx context.Context, req VerifyRequest) VerifyResult {
	challenge, err := crypto.RandomHex(16)
	if err != nil {
		return VerifyResult{Kind: FailNetwork, Message: "could not generate challenge: " + err.Error()}
	}

	if err := v.Guard.CheckURL(ctx, req.URL); err != nil {
		return VerifyResult{
			Kind:    FailBlockedLocal,
			Message: err.Error(),
		}
	}

	body, err := json.Marshal(verifyPayload{
		Type:      "link.verify",
		Challenge: challenge,
		ServiceID: req.ServicePublicID,
	})
	if err != nil {
		return VerifyResult{Kind: FailNetwork, Message: err.Error()}
	}

	timeout := VerifyTimeout
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	method := req.Method
	if method == "" {
		method = http.MethodPost
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, req.URL, bytes.NewReader(body))
	if err != nil {
		return VerifyResult{Kind: FailNetwork, Message: "invalid request: " + err.Error()}
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Hookfan-Event", "link.verify")
	httpReq.Header.Set("X-Hookfan-Token", req.LinkToken)
	// The same signature scheme used on every forwarded event, so a service
	// can implement one verification routine and use it everywhere.
	httpReq.Header.Set("X-Hookfan-Signature", "sha256="+crypto.SignHex([]byte(req.LinkToken), body))
	for k, val := range req.CustomHeaders {
		httpReq.Header.Set(k, val)
	}

	start := time.Now()
	resp, err := v.Client.Do(httpReq)
	latency := time.Since(start)

	if err != nil {
		kind, msg := classifyError(err, timeout)
		return VerifyResult{Kind: kind, Message: msg, Latency: latency, LatencyMS: latency.Milliseconds()}
	}
	defer resp.Body.Close()

	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxVerifyBody))
	result := VerifyResult{Latency: latency, LatencyMS: latency.Milliseconds(), StatusCode: resp.StatusCode}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		result.Kind = FailHTTPStatus
		result.Message = fmt.Sprintf("service returned HTTP %d; expected 2xx", resp.StatusCode)
		result.Echoed = truncate(string(respBody), 200)
		return result
	}
	if readErr != nil {
		result.Kind = FailUnreadable
		result.Message = "could not read response body: " + readErr.Error()
		return result
	}

	echoed, ok := extractChallenge(respBody, resp.Header.Get("Content-Type"))
	if !ok || echoed != challenge {
		result.Kind = FailChallenge
		result.Echoed = truncate(strings.TrimSpace(string(respBody)), 200)
		result.Message = fmt.Sprintf(
			"service echoed %q but the challenge was %q; respond with {\"challenge\":\"<value>\"} or the bare value as text/plain",
			truncate(echoed, 64), challenge)
		return result
	}

	result.OK = true
	return result
}

// extractChallenge reads the echoed challenge from either a JSON object or a
// bare text/plain body, since the spec accepts both.
func extractChallenge(body []byte, contentType string) (string, bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return "", false
	}

	if trimmed[0] == '{' {
		var parsed struct {
			Challenge string `json:"challenge"`
		}
		if err := json.Unmarshal(trimmed, &parsed); err == nil && parsed.Challenge != "" {
			return parsed.Challenge, true
		}
		// A JSON object that does not carry a challenge field: report the raw
		// body so the operator sees what the service actually sent.
		return string(trimmed), false
	}
	return string(trimmed), true
}

// classifyError turns a transport error into a specific, actionable reason.
func classifyError(err error, timeout time.Duration) (FailureKind, string) {
	if errors.Is(err, context.DeadlineExceeded) {
		return FailTimeout, fmt.Sprintf("no response within %s", timeout)
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return FailDNS, fmt.Sprintf("DNS lookup for %q failed: %s", dnsErr.Name, dnsErr.Err)
	}

	// A blocked target surfaces through the dialer, so check before the
	// generic network cases.
	var blocked *BlockedAddressError
	if errors.As(err, &blocked) {
		return FailBlockedLocal, blocked.Error()
	}

	var tlsErr *tls.CertificateVerificationError
	if errors.As(err, &tlsErr) {
		return FailTLS, "TLS certificate verification failed: " + tlsErr.Error()
	}
	var recordErr *tls.RecordHeaderError
	if errors.As(err, &recordErr) {
		return FailTLS, "TLS handshake failed; is the endpoint HTTP rather than HTTPS?"
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return FailTimeout, fmt.Sprintf("no response within %s", timeout)
	}

	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if strings.Contains(opErr.Err.Error(), "connection refused") {
			return FailConnRefused, "connection refused; nothing is listening on that address"
		}
		return FailNetwork, opErr.Error()
	}

	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"):
		return FailConnRefused, "connection refused; nothing is listening on that address"
	case strings.Contains(msg, "certificate"), strings.Contains(msg, "tls:"):
		return FailTLS, "TLS error: " + msg
	case strings.Contains(msg, "no such host"):
		return FailDNS, "DNS lookup failed: " + msg
	default:
		return FailNetwork, msg
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
