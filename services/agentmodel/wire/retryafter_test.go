package wire_test

import (
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/wire"
)

var refNow = time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

// epochOf renders t the way Anthropic's subscription reset headers do: a bare
// unix-second count.
func epochOf(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		desc  string
		value string
		want  time.Duration
	}{
		{"empty", "", 0},
		{"delta seconds", "30", 30 * time.Second},
		{"delta seconds padded", "  45  ", 45 * time.Second},
		{"fractional seconds", "1.5", 1500 * time.Millisecond},
		{"zero", "0", 0},
		{"negative delta", "-5", 0},
		{"garbage", "soon", 0},
		// Anthropic's subscription reset headers carry a unix-epoch second
		// count, not a delta. Read as a delta, a live timestamp would be ~56
		// years — the epoch threshold keeps that from parking a credential
		// until the heat death of the deployment.
		{"unix epoch in the future", epochOf(refNow.Add(3 * time.Hour)), 3 * time.Hour},
		{"unix epoch in the past", "1600000000", 0},
		{"unix epoch beyond the cap", epochOf(refNow.Add(72 * time.Hour)), wire.MaxRetryAfter},
		{"http-date in the future", refNow.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{"http-date in the past", refNow.Add(-90 * time.Second).Format(http.TimeFormat), 0},
		// A bogus far-future value must not park a credential forever.
		{"clamped to the maximum", "999999", wire.MaxRetryAfter},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			if got := wire.ParseRetryAfter(c.value, refNow); got != c.want {
				t.Errorf("ParseRetryAfter(%q) = %v, want %v", c.value, got, c.want)
			}
		})
	}
}

func TestRetryAfterFromHeader(t *testing.T) {
	t.Run("standard Retry-After", func(t *testing.T) {
		h := http.Header{"Retry-After": []string{"60"}}
		if got := wire.RetryAfterFromHeader(h, refNow); got != time.Minute {
			t.Errorf("got %v, want 1m", got)
		}
	})
	t.Run("anthropic unified reset fallback", func(t *testing.T) {
		h := http.Header{"Anthropic-Ratelimit-Unified-Reset": []string{epochOf(refNow.Add(4 * time.Hour))}}
		if got := wire.RetryAfterFromHeader(h, refNow); got != 4*time.Hour {
			t.Errorf("got %v, want 4h", got)
		}
	})
	t.Run("Retry-After wins over the reset header", func(t *testing.T) {
		h := http.Header{
			"Retry-After":                       []string{"60"},
			"Anthropic-Ratelimit-Unified-Reset": []string{epochOf(refNow.Add(4 * time.Hour))},
		}
		if got := wire.RetryAfterFromHeader(h, refNow); got != time.Minute {
			t.Errorf("got %v, want 1m (explicit Retry-After is authoritative)", got)
		}
	})
	t.Run("no headers", func(t *testing.T) {
		if got := wire.RetryAfterFromHeader(http.Header{}, refNow); got != 0 {
			t.Errorf("got %v, want 0", got)
		}
	})
	t.Run("unparseable falls through to the next source", func(t *testing.T) {
		h := http.Header{
			"Retry-After":                       []string{"whenever"},
			"Anthropic-Ratelimit-Unified-Reset": []string{epochOf(refNow.Add(4 * time.Hour))},
		}
		if got := wire.RetryAfterFromHeader(h, refNow); got != 4*time.Hour {
			t.Errorf("got %v, want 4h", got)
		}
	})
}

// hintingErr is a provider-side error that knows when the upstream said to come
// back — the shape every provider uses to feed Wrap.
type hintingErr struct{ d time.Duration }

func (h *hintingErr) Error() string                 { return "429 rate limit exceeded" }
func (h *hintingErr) RetryAfterHint() time.Duration { return h.d }

func TestWrapCarriesRetryAfterHint(t *testing.T) {
	ae := wire.Wrap(&hintingErr{d: 90 * time.Second})
	if ae.Type != wire.ErrTypeRateLimit {
		t.Fatalf("Type=%q, want %q", ae.Type, wire.ErrTypeRateLimit)
	}
	if ae.RetryAfter != 90*time.Second {
		t.Errorf("RetryAfter=%v, want 90s", ae.RetryAfter)
	}
}

func TestWrapFindsHintDeeperInTheChain(t *testing.T) {
	wrapped := &wrapErr{msg: "anthropic: ", err: &hintingErr{d: 30 * time.Second}}
	if got := wire.Wrap(wrapped).RetryAfter; got != 30*time.Second {
		t.Errorf("RetryAfter=%v, want 30s (hint must be found through Unwrap)", got)
	}
}

func TestWrapWithoutHintLeavesRetryAfterZero(t *testing.T) {
	if got := wire.Wrap(errors.New("429 rate limit exceeded")).RetryAfter; got != 0 {
		t.Errorf("RetryAfter=%v, want 0", got)
	}
}

func TestWrapPreservesRetryAfterOnAlreadyNormalizedError(t *testing.T) {
	orig := &wire.Error{Type: wire.ErrTypeRateLimit, Message: "x", RetryAfter: time.Minute}
	if got := wire.Wrap(orig).RetryAfter; got != time.Minute {
		t.Errorf("RetryAfter=%v, want 1m (pass-through must not drop it)", got)
	}
}

type wrapErr struct {
	msg string
	err error
}

func (w *wrapErr) Error() string { return w.msg + w.err.Error() }
func (w *wrapErr) Unwrap() error { return w.err }

func TestRetryAfterFor(t *testing.T) {
	cases := []struct {
		desc   string
		status int
		header http.Header
		want   time.Duration
	}{
		{"429 with a hint", http.StatusTooManyRequests, http.Header{"Retry-After": []string{"120"}}, 2 * time.Minute},
		{"429 without a hint", http.StatusTooManyRequests, http.Header{}, 0},
		// The whole point of the gate: a Retry-After on a 5xx is backoff advice
		// for this request, not evidence the credential is out of quota.
		{"503 with a Retry-After", http.StatusServiceUnavailable, http.Header{"Retry-After": []string{"120"}}, 0},
		{"401 with a Retry-After", http.StatusUnauthorized, http.Header{"Retry-After": []string{"120"}}, 0},
		{"200", http.StatusOK, http.Header{"Retry-After": []string{"120"}}, 0},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			resp := &http.Response{StatusCode: c.status, Header: c.header}
			if got := wire.RetryAfterFor(resp, refNow); got != c.want {
				t.Errorf("RetryAfterFor(%d) = %v, want %v", c.status, got, c.want)
			}
		})
	}
	t.Run("nil response", func(t *testing.T) {
		if got := wire.RetryAfterFor(nil, refNow); got != 0 {
			t.Errorf("got %v, want 0", got)
		}
	})
}

func TestWithRetryAfter(t *testing.T) {
	base := errors.New("429 rate limit exceeded")

	t.Run("hint reaches the classified error", func(t *testing.T) {
		ae := wire.Wrap(wire.WithRetryAfter(base, 90*time.Second))
		if ae.Type != wire.ErrTypeRateLimit {
			t.Fatalf("Type=%q, want %q", ae.Type, wire.ErrTypeRateLimit)
		}
		if ae.RetryAfter != 90*time.Second {
			t.Errorf("RetryAfter=%v, want 90s", ae.RetryAfter)
		}
	})
	t.Run("message and chain are preserved", func(t *testing.T) {
		wrapped := wire.WithRetryAfter(base, time.Minute)
		if wrapped.Error() != base.Error() {
			t.Errorf("Error()=%q, want the original %q", wrapped.Error(), base.Error())
		}
		if !errors.Is(wrapped, base) {
			t.Error("errors.Is must still find the wrapped error")
		}
	})
	t.Run("clamped to the maximum", func(t *testing.T) {
		if got := wire.Wrap(wire.WithRetryAfter(base, 72*time.Hour)).RetryAfter; got != wire.MaxRetryAfter {
			t.Errorf("RetryAfter=%v, want %v", got, wire.MaxRetryAfter)
		}
	})
	t.Run("no hint returns the error untouched", func(t *testing.T) {
		if got := wire.WithRetryAfter(base, 0); got != base {
			t.Errorf("got %v, want the original error unwrapped", got)
		}
	})
	t.Run("nil error stays nil", func(t *testing.T) {
		if got := wire.WithRetryAfter(nil, time.Minute); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
}
