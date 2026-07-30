package wire

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// MaxRetryAfter caps how long an upstream hint may park a credential or
// deployment. Subscription quotas legitimately reset hours out (Anthropic's
// 5-hour window, ChatGPT's weekly cap), so the cap has to be generous — but a
// malformed or hostile header must not take a credential out of rotation for
// the life of the process.
const MaxRetryAfter = 24 * time.Hour

// retryAfterEpochThreshold separates a delta-seconds value from a unix-epoch
// timestamp. Anthropic's subscription rate-limit reset headers carry epoch
// seconds while RFC 9110's Retry-After carries a delta; 1e9 (2001-09-09) is far
// beyond any sane delta (31 years) and far below any live timestamp.
const retryAfterEpochThreshold = 1_000_000_000

// retryAfterHeaders lists the response headers consulted, in priority order.
// Retry-After is the standard and authoritative when present; the Anthropic
// unified reset is the subscription-quota fallback, which is what actually
// matters for a pooled Claude subscription (a 429 there means "this account is
// done for the window", not "slow down for a second").
var retryAfterHeaders = []string{"Retry-After", "Anthropic-Ratelimit-Unified-Reset"}

// RetryAfterHinter is implemented by provider errors that know when the
// upstream said to come back. Wrap copies the hint onto Error.RetryAfter, so a
// provider only has to expose this method rather than construct a wire.Error
// itself.
type RetryAfterHinter interface {
	// RetryAfterHint returns how long to wait before retrying, or 0 when the
	// upstream gave no usable hint.
	RetryAfterHint() time.Duration
}

// ParseRetryAfter interprets a Retry-After-style header value as a wait
// duration relative to now. It accepts the RFC 9110 forms (delta-seconds and
// HTTP-date) plus the two shapes real upstreams also emit: a fractional second
// count and a unix-epoch timestamp (see retryAfterEpochThreshold).
//
// Returns 0 when the value is absent, unparseable, or already in the past —
// callers treat 0 as "no hint, use your default". The result is clamped to
// MaxRetryAfter.
func ParseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(value, 64); err == nil {
		if secs >= retryAfterEpochThreshold {
			return ClampRetryAfter(time.Unix(int64(secs), 0).Sub(now))
		}
		return ClampRetryAfter(time.Duration(secs * float64(time.Second)))
	}
	// http.ParseTime covers the three HTTP-date formats Retry-After allows.
	if t, err := http.ParseTime(value); err == nil {
		return ClampRetryAfter(t.Sub(now))
	}
	return 0
}

// RetryAfterFor returns the retry hint a response carries, and 0 for any status
// other than 429. That gate is the whole policy in one place: a Retry-After on a
// 5xx is backoff advice for this request, not evidence the credential is out of
// quota, and honoring it there would park a healthy account. Every provider that
// classifies an HTTP response should call this rather than re-deriving the rule.
func RetryAfterFor(resp *http.Response, now time.Time) time.Duration {
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		return 0
	}
	return RetryAfterFromHeader(resp.Header, now)
}

// WithRetryAfter attaches a retry hint to err, so agentmodel.Wrap copies it onto
// the classified error and the pool/router size their cooldowns from it. This is
// the sanctioned way to satisfy RetryAfterHinter: a provider whose own error
// type already carries status/body detail should implement the interface on that
// type instead, but a provider with nothing else to carry should not invent a
// struct just to hold a duration. Returns err unchanged when d <= 0.
func WithRetryAfter(err error, d time.Duration) error {
	if err == nil || d <= 0 {
		return err
	}
	return &retryAfterError{err: err, d: ClampRetryAfter(d)}
}

type retryAfterError struct {
	err error
	d   time.Duration
}

func (e *retryAfterError) Error() string                 { return e.err.Error() }
func (e *retryAfterError) Unwrap() error                 { return e.err }
func (e *retryAfterError) RetryAfterHint() time.Duration { return e.d }

// RetryAfterFromHeader returns the first usable retry hint among the response
// headers upstreams use to say when to come back. Returns 0 when none of them
// carries a parseable, still-future value.
func RetryAfterFromHeader(h http.Header, now time.Time) time.Duration {
	for _, name := range retryAfterHeaders {
		if d := ParseRetryAfter(h.Get(name), now); d > 0 {
			return d
		}
	}
	return 0
}

// ClampRetryAfter bounds a retry hint to (0, MaxRetryAfter]. Providers that
// read a hint from somewhere other than a header — a JSON error body, say — use
// it so the cap is spelled in exactly one place.
func ClampRetryAfter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	if d > MaxRetryAfter {
		return MaxRetryAfter
	}
	return d
}
