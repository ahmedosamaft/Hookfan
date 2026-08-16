package dispatch

import (
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// Retry schedule bounds.
const (
	BackoffBase = 2 * time.Second
	BackoffCap  = time.Hour
)

// Decision is what to do after one attempt.
type Decision struct {
	Success bool
	// Terminal means the request will never succeed, so no further attempt is
	// scheduled even if attempts remain.
	Terminal bool
	Retry    bool
	Delay    time.Duration
	Reason   string
}

// Classify decides the outcome of an attempt.
//
//   - 2xx                          success
//   - 4xx except 408 and 429       terminal; a 400 will still be a 400 later
//   - 429                          retry, honouring Retry-After when present
//   - 408, 5xx, transport errors   retry with backoff until max_attempts
func Classify(statusCode int, transportErr error, header http.Header, attempt, maxAttempts int) Decision {
	if transportErr != nil {
		return retryOrDead(attempt, maxAttempts, "transport error: "+transportErr.Error(), 0)
	}

	switch {
	case statusCode >= 200 && statusCode <= 299:
		return Decision{Success: true}

	case statusCode == http.StatusTooManyRequests:
		// The service told us its own pace; prefer it over our backoff.
		delay := parseRetryAfter(header.Get("Retry-After"), time.Now())
		return retryOrDead(attempt, maxAttempts, "rate limited (429)", delay)

	case statusCode == http.StatusRequestTimeout:
		return retryOrDead(attempt, maxAttempts, "request timeout (408)", 0)

	case statusCode >= 400 && statusCode <= 499:
		return Decision{
			Terminal: true,
			Reason:   "client error (HTTP " + strconv.Itoa(statusCode) + "); not retrying",
		}

	case statusCode >= 500:
		delay := parseRetryAfter(header.Get("Retry-After"), time.Now())
		return retryOrDead(attempt, maxAttempts,
			"server error (HTTP "+strconv.Itoa(statusCode)+")", delay)

	default:
		// 1xx and 3xx are not valid webhook responses; redirects are not
		// followed, so a 3xx arrives here.
		return retryOrDead(attempt, maxAttempts,
			"unexpected status HTTP "+strconv.Itoa(statusCode), 0)
	}
}

// retryOrDead schedules the next attempt, or gives up once attempts run out.
func retryOrDead(attempt, maxAttempts int, reason string, hint time.Duration) Decision {
	if attempt >= maxAttempts {
		return Decision{Reason: reason + "; attempts exhausted"}
	}
	delay := hint
	if delay <= 0 {
		delay = Backoff(attempt)
	}
	if delay > BackoffCap {
		delay = BackoffCap
	}
	return Decision{Retry: true, Delay: delay, Reason: reason}
}

// Backoff returns the delay before the given attempt number using exponential
// backoff with full jitter.
//
// Full jitter means the delay is uniform over [0, ceiling) rather than
// ceiling plus a small wobble — the low end matters, because it is what stops
// a fleet of failed deliveries retrying in lockstep and stampeding a service
// as it recovers.
func Backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	// Cap the exponent before shifting so the multiplication cannot overflow.
	exp := attempt - 1
	if exp > 30 {
		exp = 30
	}
	ceiling := float64(BackoffBase) * math.Pow(2, float64(exp))
	if ceiling > float64(BackoffCap) {
		ceiling = float64(BackoffCap)
	}
	return time.Duration(rand.Float64() * ceiling)
}

// parseRetryAfter reads a Retry-After header, which may be either
// delta-seconds or an HTTP-date. Absurd or unparseable values yield 0, letting
// the caller fall back to its own backoff.
func parseRetryAfter(value string, now time.Time) time.Duration {
	if value == "" {
		return 0
	}
	if secs, err := strconv.Atoi(value); err == nil {
		if secs <= 0 {
			return 0
		}
		d := time.Duration(secs) * time.Second
		if d > BackoffCap {
			return BackoffCap
		}
		return d
	}
	if t, err := http.ParseTime(value); err == nil {
		d := t.Sub(now)
		if d <= 0 {
			return 0
		}
		if d > BackoffCap {
			return BackoffCap
		}
		return d
	}
	return 0
}
