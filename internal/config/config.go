// Package config parses and validates all runtime configuration from the
// environment. Every problem is reported at startup, together, so an operator
// fixes one round of errors instead of discovering them one restart at a time.
package config

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// SecretKeyLen is the AES-256 key size expected in SECRET_ENCRYPTION_KEY.
const SecretKeyLen = 32

type Config struct {
	DatabaseURL     string
	AdminToken      string
	SecretKey       []byte // decoded, always SecretKeyLen bytes
	Port            int
	AllowedOrigins  []string
	WorkerConcur    int
	RetentionDays   int
	AllowPrivate    bool
	LogLevel        slog.Level
	ShutdownTimeout time.Duration
}

// errs accumulates validation failures so Load can report them all at once.
type errs []string

func (e *errs) addf(format string, args ...any) {
	*e = append(*e, fmt.Sprintf(format, args...))
}

// Load reads configuration from the environment and validates it. The returned
// error, when non-nil, lists every problem found.
func Load() (*Config, error) {
	var e errs
	c := &Config{ShutdownTimeout: 15 * time.Second}

	c.DatabaseURL = os.Getenv("DATABASE_URL")
	if c.DatabaseURL == "" {
		e.addf("DATABASE_URL is required (e.g. postgres://hookfan:hookfan@db:5432/hookfan?sslmode=disable)")
	} else if _, err := url.Parse(c.DatabaseURL); err != nil {
		e.addf("DATABASE_URL is not a valid URL: %v", err)
	}

	c.AdminToken = os.Getenv("ADMIN_TOKEN")
	switch {
	case c.AdminToken == "":
		e.addf("ADMIN_TOKEN is required; it guards the whole admin API")
	case len(c.AdminToken) < 16:
		e.addf("ADMIN_TOKEN must be at least 16 characters (got %d); generate one with: openssl rand -base64 32", len(c.AdminToken))
	}

	c.SecretKey = loadSecretKey(&e)
	c.Port = intVar(&e, "PORT", 8080, 1, 65535)
	c.WorkerConcur = intVar(&e, "WORKER_CONCURRENCY", 16, 1, 1024)
	c.RetentionDays = intVar(&e, "EVENT_RETENTION_DAYS", 30, 1, 3650)
	c.AllowPrivate = boolVar(&e, "ALLOW_PRIVATE_TARGETS", false)
	c.LogLevel = logLevel(&e)
	c.AllowedOrigins = origins(&e)

	if len(e) > 0 {
		return nil, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(e, "\n  - "))
	}
	return c, nil
}

// loadSecretKey decodes SECRET_ENCRYPTION_KEY, which protects listener secrets
// and service link tokens at rest. The length check is explicit because a
// too-short key fails deep inside AES with an opaque message.
func loadSecretKey(e *errs) []byte {
	raw := os.Getenv("SECRET_ENCRYPTION_KEY")
	if raw == "" {
		e.addf("SECRET_ENCRYPTION_KEY is required: %d bytes, base64-encoded; generate one with: openssl rand -base64 %d", SecretKeyLen, SecretKeyLen)
		return nil
	}
	// Accept both standard and URL-safe alphabets, with or without padding, so
	// a key copied from any tool works.
	key, err := decodeBase64(raw)
	if err != nil {
		e.addf("SECRET_ENCRYPTION_KEY is not valid base64: %v; generate one with: openssl rand -base64 %d", err, SecretKeyLen)
		return nil
	}
	if len(key) != SecretKeyLen {
		e.addf("SECRET_ENCRYPTION_KEY must decode to exactly %d bytes, got %d; generate one with: openssl rand -base64 %d", SecretKeyLen, len(key), SecretKeyLen)
		return nil
	}
	return key
}

func decodeBase64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	encs := []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	}
	var lastErr error
	for _, enc := range encs {
		b, err := enc.DecodeString(s)
		if err == nil {
			return b, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// origins parses ALLOWED_ORIGINS. Empty means deny every cross-origin request:
// a wildcard combined with a bearer token in sessionStorage is a bad trade.
func origins(e *errs) []string {
	raw := strings.TrimSpace(os.Getenv("ALLOWED_ORIGINS"))
	if raw == "" {
		return nil
	}
	var out []string
	for _, o := range strings.Split(raw, ",") {
		o = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(o), "/"))
		if o == "" {
			continue
		}
		if o == "*" {
			e.addf(`ALLOWED_ORIGINS must not be "*": the admin API uses a bearer token, so list exact origins (e.g. http://localhost:8080)`)
			continue
		}
		u, err := url.Parse(o)
		if err != nil || u.Scheme == "" || u.Host == "" {
			e.addf("ALLOWED_ORIGINS entry %q must be a full origin including scheme (e.g. http://localhost:8080)", o)
			continue
		}
		out = append(out, u.Scheme+"://"+u.Host)
	}
	return out
}

func intVar(e *errs, name string, def, min, max int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		e.addf("%s must be an integer, got %q", name, raw)
		return def
	}
	if v < min || v > max {
		e.addf("%s must be between %d and %d, got %d", name, min, max, v)
		return def
	}
	return v
}

func boolVar(e *errs, name string, def bool) bool {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		e.addf("%s must be a boolean (true/false), got %q", name, raw)
		return def
	}
	return v
}

func logLevel(e *errs) slog.Level {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL")))
	switch raw {
	case "", "info":
		return slog.LevelInfo
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		e.addf("LOG_LEVEL must be one of debug, info, warn, error; got %q", raw)
		return slog.LevelInfo
	}
}

// Addr is the listen address for the HTTP server.
func (c *Config) Addr() string { return fmt.Sprintf(":%d", c.Port) }
