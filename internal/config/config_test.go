package config

import (
	"strings"
	"testing"
)

// validEnv is a minimal configuration that must always load cleanly.
func validEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://hookfan:hookfan@db:5432/hookfan?sslmode=disable")
	t.Setenv("ADMIN_TOKEN", "0123456789abcdef0123")
	// 32 bytes of zeros, base64.
	t.Setenv("SECRET_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
}

func TestLoadDefaults(t *testing.T) {
	validEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Port != 8080 {
		t.Errorf("Port = %d, want 8080", cfg.Port)
	}
	if cfg.WorkerConcur != 16 {
		t.Errorf("WorkerConcur = %d, want 16", cfg.WorkerConcur)
	}
	if cfg.RetentionDays != 30 {
		t.Errorf("RetentionDays = %d, want 30", cfg.RetentionDays)
	}
	if cfg.AllowPrivate {
		t.Error("AllowPrivate = true, want false by default")
	}
	if len(cfg.SecretKey) != SecretKeyLen {
		t.Errorf("SecretKey length = %d, want %d", len(cfg.SecretKey), SecretKeyLen)
	}
	// Default-deny: no ALLOWED_ORIGINS means no cross-origin request is allowed.
	if len(cfg.AllowedOrigins) != 0 {
		t.Errorf("AllowedOrigins = %v, want empty by default", cfg.AllowedOrigins)
	}
}

func TestSecretEncryptionKeyValidation(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		wantErr string
	}{
		{"missing", "", "SECRET_ENCRYPTION_KEY is required"},
		{"not base64", "!!!not base64!!!", "not valid base64"},
		{"too short", "AAAAAAAAAAAAAAAAAAAAAA==", "must decode to exactly 32 bytes, got 16"},
		// 48 valid base64 chars decode to 36 bytes: parses fine, wrong length.
		{"too long", strings.Repeat("A", 48), "must decode to exactly 32 bytes, got 36"},
		{"valid std", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", ""},
		// URL-safe alphabets are accepted so a key copied from any tool works.
		{"valid urlsafe", "-_-_-_-_-_-_-_-_-_-_-_-_-_-_-_-_-_-_-_-_-_0=", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validEnv(t)
			t.Setenv("SECRET_ENCRYPTION_KEY", tt.key)

			_, err := Load()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Load() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Load() error = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Load() error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestRequiredVarsReportedTogether(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("ADMIN_TOKEN", "")
	t.Setenv("SECRET_ENCRYPTION_KEY", "")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want error")
	}
	// One restart should surface every problem, not just the first.
	for _, want := range []string{"DATABASE_URL", "ADMIN_TOKEN", "SECRET_ENCRYPTION_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing mention of %s", err, want)
		}
	}
}

func TestAdminTokenMinLength(t *testing.T) {
	validEnv(t)
	t.Setenv("ADMIN_TOKEN", "short")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "at least 16 characters") {
		t.Fatalf("Load() error = %v, want a minimum-length complaint", err)
	}
}

func TestAllowedOrigins(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []string
		wantErr string
	}{
		{"single", "http://localhost:8080", []string{"http://localhost:8080"}, ""},
		{"multiple with spaces", "http://a.test, https://b.test", []string{"http://a.test", "https://b.test"}, ""},
		{"trailing slash trimmed", "http://a.test/", []string{"http://a.test"}, ""},
		{"path stripped to origin", "https://a.test/admin", []string{"https://a.test"}, ""},
		// A wildcard plus a bearer token in sessionStorage is a bad combination.
		{"wildcard rejected", "*", nil, `must not be "*"`},
		{"scheme required", "localhost:8080", nil, "full origin including scheme"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validEnv(t)
			t.Setenv("ALLOWED_ORIGINS", tt.raw)

			cfg, err := Load()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Load() error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if len(cfg.AllowedOrigins) != len(tt.want) {
				t.Fatalf("AllowedOrigins = %v, want %v", cfg.AllowedOrigins, tt.want)
			}
			for i, w := range tt.want {
				if cfg.AllowedOrigins[i] != w {
					t.Errorf("AllowedOrigins[%d] = %q, want %q", i, cfg.AllowedOrigins[i], w)
				}
			}
		})
	}
}

func TestNumericBounds(t *testing.T) {
	tests := []struct {
		name, env, val, wantErr string
	}{
		{"port not a number", "PORT", "eighty", "must be an integer"},
		{"port out of range", "PORT", "70000", "must be between 1 and 65535"},
		{"workers zero", "WORKER_CONCURRENCY", "0", "must be between 1 and 1024"},
		{"retention zero", "EVENT_RETENTION_DAYS", "0", "must be between 1 and 3650"},
		{"bad bool", "ALLOW_PRIVATE_TARGETS", "yes-please", "must be a boolean"},
		{"bad log level", "LOG_LEVEL", "verbose", "must be one of debug, info, warn, error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validEnv(t)
			t.Setenv(tt.env, tt.val)

			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}
