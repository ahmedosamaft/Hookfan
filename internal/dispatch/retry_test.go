package dispatch

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		err          error
		attempt      int
		maxAttempts  int
		wantSuccess  bool
		wantTerminal bool
		wantRetry    bool
	}{
		{name: "200 is success", status: 200, attempt: 1, maxAttempts: 6, wantSuccess: true},
		{name: "201 is success", status: 201, attempt: 1, maxAttempts: 6, wantSuccess: true},
		{name: "204 is success", status: 204, attempt: 1, maxAttempts: 6, wantSuccess: true},

		// A 400 will still be a 400 on attempt six.
		{name: "400 is terminal", status: 400, attempt: 1, maxAttempts: 6, wantTerminal: true},
		{name: "401 is terminal", status: 401, attempt: 1, maxAttempts: 6, wantTerminal: true},
		{name: "403 is terminal", status: 403, attempt: 1, maxAttempts: 6, wantTerminal: true},
		{name: "404 is terminal", status: 404, attempt: 1, maxAttempts: 6, wantTerminal: true},
		{name: "422 is terminal", status: 422, attempt: 1, maxAttempts: 6, wantTerminal: true},

		// The two 4xx exceptions.
		{name: "408 is retried", status: 408, attempt: 1, maxAttempts: 6, wantRetry: true},
		{name: "429 is retried", status: 429, attempt: 1, maxAttempts: 6, wantRetry: true},

		{name: "500 is retried", status: 500, attempt: 1, maxAttempts: 6, wantRetry: true},
		{name: "502 is retried", status: 502, attempt: 1, maxAttempts: 6, wantRetry: true},
		{name: "503 is retried", status: 503, attempt: 1, maxAttempts: 6, wantRetry: true},

		{name: "transport error is retried", err: errors.New("connection reset"), attempt: 1, maxAttempts: 6, wantRetry: true},

		// Retries stop once attempts are exhausted: neither success nor
		// terminal, so the delivery becomes dead.
		{name: "500 on last attempt is dead", status: 500, attempt: 6, maxAttempts: 6},
		{name: "429 on last attempt is dead", status: 429, attempt: 6, maxAttempts: 6},
		{name: "transport error on last attempt is dead", err: errors.New("x"), attempt: 6, maxAttempts: 6},

		// Redirects are not followed, so a 3xx reaches the classifier.
		{name: "302 is retried as unexpected", status: 302, attempt: 1, maxAttempts: 6, wantRetry: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.status, tt.err, http.Header{}, tt.attempt, tt.maxAttempts)

			if got.Success != tt.wantSuccess {
				t.Errorf("Success = %v, want %v", got.Success, tt.wantSuccess)
			}
			if got.Terminal != tt.wantTerminal {
				t.Errorf("Terminal = %v, want %v", got.Terminal, tt.wantTerminal)
			}
			if got.Retry != tt.wantRetry {
				t.Errorf("Retry = %v, want %v (reason: %s)", got.Retry, tt.wantRetry, got.Reason)
			}
			if tt.wantRetry && got.Delay <= 0 {
				t.Error("Retry is true but Delay is not positive")
			}
			// Every non-success outcome must carry an explanation.
			if !tt.wantSuccess && got.Reason == "" {
				t.Error("Reason is empty for a non-success outcome")
			}
		})
	}
}

// A terminal 4xx must not be retried even when attempts remain.
func TestClassifyTerminalIgnoresRemainingAttempts(t *testing.T) {
	got := Classify(400, nil, http.Header{}, 1, 20)
	if got.Retry {
		t.Error("a 400 was scheduled for retry despite 19 attempts remaining")
	}
	if !got.Terminal {
		t.Error("Terminal = false, want true")
	}
}

func TestClassifyHonoursRetryAfter(t *testing.T) {
	tests := []struct {
		name   string
		status int
		header string
		want   time.Duration
	}{
		{"429 with delta-seconds", 429, "30", 30 * time.Second},
		{"503 with delta-seconds", 503, "45", 45 * time.Second},
		{"zero is ignored", 429, "0", 0},
		{"negative is ignored", 429, "-5", 0},
		{"unparseable is ignored", 429, "soon", 0},
		// Beyond the cap it is clamped rather than trusted.
		{"absurd value clamps to cap", 429, "999999999", BackoffCap},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			h.Set("Retry-After", tt.header)
			got := Classify(tt.status, nil, h, 1, 6)

			if !got.Retry {
				t.Fatalf("Retry = false, want true (reason: %s)", got.Reason)
			}
			if tt.want == 0 {
				// Falls back to jittered backoff, which is bounded by the base.
				if got.Delay > BackoffBase {
					t.Errorf("Delay = %v, want a jittered backoff below %v", got.Delay, BackoffBase)
				}
				return
			}
			if got.Delay != tt.want {
				t.Errorf("Delay = %v, want %v", got.Delay, tt.want)
			}
		})
	}
}

func TestParseRetryAfterHTTPDate(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

	t.Run("future date", func(t *testing.T) {
		future := now.Add(90 * time.Second).Format(http.TimeFormat)
		got := parseRetryAfter(future, now)
		// Second granularity in the HTTP date format.
		if got < 89*time.Second || got > 91*time.Second {
			t.Errorf("parseRetryAfter(%q) = %v, want about 90s", future, got)
		}
	})

	t.Run("past date yields zero", func(t *testing.T) {
		past := now.Add(-time.Hour).Format(http.TimeFormat)
		if got := parseRetryAfter(past, now); got != 0 {
			t.Errorf("parseRetryAfter(past) = %v, want 0", got)
		}
	})

	t.Run("far future clamps to cap", func(t *testing.T) {
		far := now.Add(30 * 24 * time.Hour).Format(http.TimeFormat)
		if got := parseRetryAfter(far, now); got != BackoffCap {
			t.Errorf("parseRetryAfter(far future) = %v, want the %v cap", got, BackoffCap)
		}
	})

	t.Run("empty yields zero", func(t *testing.T) {
		if got := parseRetryAfter("", now); got != 0 {
			t.Errorf("parseRetryAfter(\"\") = %v, want 0", got)
		}
	})
}

// Full jitter: the delay is uniform over [0, ceiling), and the ceiling doubles
// per attempt up to the cap.
func TestBackoffBounds(t *testing.T) {
	for attempt := 1; attempt <= 12; attempt++ {
		ceiling := time.Duration(float64(BackoffBase) * pow2(attempt-1))
		if ceiling > BackoffCap {
			ceiling = BackoffCap
		}

		for range 200 {
			got := Backoff(attempt)
			if got < 0 {
				t.Fatalf("attempt %d: Backoff = %v, want non-negative", attempt, got)
			}
			if got > ceiling {
				t.Fatalf("attempt %d: Backoff = %v, want at most %v", attempt, got, ceiling)
			}
		}
	}
}

func TestBackoffNeverExceedsCap(t *testing.T) {
	// A pathological attempt number must not overflow into a negative or
	// enormous duration.
	for _, attempt := range []int{50, 100, 1000} {
		for range 50 {
			if got := Backoff(attempt); got < 0 || got > BackoffCap {
				t.Fatalf("Backoff(%d) = %v, want within [0, %v]", attempt, got, BackoffCap)
			}
		}
	}
}

// Full jitter must actually spread: values clustered at the ceiling would mean
// every failed delivery retries in lockstep.
func TestBackoffIsJittered(t *testing.T) {
	const attempt = 8
	ceiling := time.Duration(float64(BackoffBase) * pow2(attempt-1))

	var belowHalf, aboveHalf int
	for range 500 {
		if Backoff(attempt) < ceiling/2 {
			belowHalf++
		} else {
			aboveHalf++
		}
	}
	// A uniform distribution puts roughly half on each side; this is a loose
	// bound that still catches a constant or ceiling-clustered implementation.
	if belowHalf < 150 || aboveHalf < 150 {
		t.Errorf("distribution looks unjittered: %d below half, %d above", belowHalf, aboveHalf)
	}
}

func pow2(n int) float64 {
	result := 1.0
	for range n {
		result *= 2
	}
	return result
}
