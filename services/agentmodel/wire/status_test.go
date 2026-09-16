package wire_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/wire"
)

// A provider that knows the upstream status must classify by it, not by digits
// that happen to appear in the upstream's response body. Every message below is
// a realistic body whose incidental digits collide with a status needle the
// substring heuristic tests earlier than the true one.
func TestWrap_UpstreamStatusBeatsBodyDigits(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		msg     string
		wantTyp string
	}{
		{
			// "129403" contains "403", which the heuristic tests three cases
			// before "context length".
			name:    "context window body whose token count contains 403",
			status:  http.StatusBadRequest,
			msg:     `openai: bad request (status=400): {"error":{"message":"This model's maximum context length is 128000 tokens, however you requested 129403 tokens","code":"context_length_exceeded"}}`,
			wantTyp: wire.ErrTypeContextWindow,
		},
		{
			// A request id containing "401" must not read as an auth failure —
			// that would report a transient 500 as a credential problem and stop
			// the router's fallback walk.
			name:    "upstream 500 whose request id contains 401",
			status:  http.StatusInternalServerError,
			msg:     `openai: upstream error (status=500): {"error":{"message":"The server had an error","type":"server_error"},"request_id":"req_c401e9ab"}`,
			wantTyp: wire.ErrTypeUpstream,
		},
		{
			name:    "upstream 500 whose message quotes a 400ms timeout",
			status:  http.StatusInternalServerError,
			msg:     `openai: upstream error (status=500): {"error":{"message":"backend timed out after 400ms","type":"server_error"}}`,
			wantTyp: wire.ErrTypeUpstream,
		},
		{
			name:    "rate limit whose retry hint contains 400",
			status:  http.StatusTooManyRequests,
			msg:     `anthropic: status 429: {"type":"error","error":{"type":"rate_limit_error","message":"retry in 400ms"}}`,
			wantTyp: wire.ErrTypeRateLimit,
		},
		{
			// Anthropic's documented overload status. No needle matches it, so
			// the heuristic reaches it only via the word "overloaded" in the body.
			name:    "anthropic 529 overloaded",
			status:  529,
			msg:     `anthropic: status 529: {"type":"error","error":{"type":"overloaded_error"}}`,
			wantTyp: wire.ErrTypeServiceUnavailable,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := wire.Wrap(wire.WithUpstreamStatus(errors.New(c.msg), c.status))
			if got.Type != c.wantTyp {
				t.Errorf("Type = %q, want %q", got.Type, c.wantTyp)
			}
		})
	}
}

// Classifying by status must not flatten the sub-classifications that share one
// status. The body still disambiguates WITHIN the status family — it just no
// longer decides the family.
func TestWrap_UpstreamStatusKeepsWithinFamilyCodes(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		msg      string
		wantTyp  string
		wantCode string
	}{
		{
			name:     "400 context length",
			status:   http.StatusBadRequest,
			msg:      `openai: {"error":{"code":"context_length_exceeded"}}`,
			wantTyp:  wire.ErrTypeContextWindow,
			wantCode: wire.CodeContextLength,
		},
		{
			name:     "400 content policy",
			status:   http.StatusBadRequest,
			msg:      `openai: {"error":{"code":"content_policy_violation"}}`,
			wantTyp:  wire.ErrTypeContentFilter,
			wantCode: wire.CodeContentPolicy,
		},
		{
			name:    "400 otherwise generic",
			status:  http.StatusBadRequest,
			msg:     `openai: {"error":{"message":"unknown parameter foo"}}`,
			wantTyp: wire.ErrTypeInvalidRequest,
		},
		{
			name:     "429 quota exhaustion rather than throughput",
			status:   http.StatusTooManyRequests,
			msg:      `openai: {"error":{"code":"insufficient_quota"}}`,
			wantTyp:  wire.ErrTypeRateLimit,
			wantCode: wire.CodeQuotaExceeded,
		},
		{
			name:     "429 plain rate limit",
			status:   http.StatusTooManyRequests,
			msg:      `openai: rate limit exceeded`,
			wantTyp:  wire.ErrTypeRateLimit,
			wantCode: wire.CodeRateLimitExceeded,
		},
		{
			name:     "401",
			status:   http.StatusUnauthorized,
			msg:      `openai: authentication failed`,
			wantTyp:  wire.ErrTypeAuthentication,
			wantCode: wire.CodeInvalidAPIKey,
		},
		{
			name:     "404",
			status:   http.StatusNotFound,
			msg:      `openai: model gpt-9 does not exist`,
			wantTyp:  wire.ErrTypeNotFound,
			wantCode: wire.CodeModelNotFound,
		},
		{
			name:    "403",
			status:  http.StatusForbidden,
			msg:     `openai: you do not have access to this model`,
			wantTyp: wire.ErrTypePermissionDenied,
		},
		{
			name:    "408 request timeout",
			status:  http.StatusRequestTimeout,
			msg:     `openai: request timeout`,
			wantTyp: wire.ErrTypeTimeout,
		},
		{
			name:    "503",
			status:  http.StatusServiceUnavailable,
			msg:     `openai: service unavailable`,
			wantTyp: wire.ErrTypeServiceUnavailable,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := wire.Wrap(wire.WithUpstreamStatus(errors.New(c.msg), c.status))
			if got.Type != c.wantTyp {
				t.Errorf("Type = %q, want %q", got.Type, c.wantTyp)
			}
			if c.wantCode != "" && got.Code != c.wantCode {
				t.Errorf("Code = %q, want %q", got.Code, c.wantCode)
			}
		})
	}
}

// The status hint decides retryability, which is what the router's fallback walk
// gates on. This is the behavior the digit collisions actually broke.
func TestWrap_UpstreamStatusDrivesRetryable(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
		{529, true},
		{http.StatusTooManyRequests, true},
		{http.StatusRequestTimeout, true},
		{http.StatusUnauthorized, false},
		{http.StatusForbidden, false},
		{http.StatusNotFound, false},
		{http.StatusBadRequest, false},
	}
	for _, c := range cases {
		// A body carrying every needle at once: only the status may decide.
		msg := "upstream said 401 403 404 400 429 503 rate limit authentication overloaded timeout"
		got := wire.Wrap(wire.WithUpstreamStatus(errors.New(msg), c.status))
		if got.Retryable() != c.want {
			t.Errorf("status %d: Retryable() = %v, want %v (type %q)", c.status, got.Retryable(), c.want, got.Type)
		}
	}
}

// Errors with no status to hint — transport failures, context cancellation,
// anything that never reached an upstream — keep the substring heuristic.
func TestWrap_WithoutUpstreamStatusUsesHeuristic(t *testing.T) {
	cases := []struct {
		msg     string
		wantTyp string
	}{
		{"openai: do: dial tcp: i/o timeout", wire.ErrTypeTimeout},
		{"anthropic: http: context deadline exceeded", wire.ErrTypeTimeout},
		{"openai: authentication failed: bad key", wire.ErrTypeAuthentication},
		{"something nobody has seen before", wire.ErrTypeUpstream},
	}
	for _, c := range cases {
		got := wire.Wrap(errors.New(c.msg))
		if got.Type != c.wantTyp {
			t.Errorf("Wrap(%q).Type = %q, want %q", c.msg, got.Type, c.wantTyp)
		}
	}
}

// WithUpstreamStatus must compose with WithRetryAfter — a 429 needs both its
// family and its cooldown window, and neither may drop the other.
func TestWithUpstreamStatus_ComposesWithRetryAfter(t *testing.T) {
	base := errors.New(`anthropic: status 429: {"error":{"type":"rate_limit_error"}}`)
	err := wire.WithUpstreamStatus(wire.WithRetryAfter(base, 90*1e9), http.StatusTooManyRequests)

	got := wire.Wrap(err)
	if got.Type != wire.ErrTypeRateLimit {
		t.Errorf("Type = %q, want %q", got.Type, wire.ErrTypeRateLimit)
	}
	if got.RetryAfter != 90*1e9 {
		t.Errorf("RetryAfter = %v, want 90s", got.RetryAfter)
	}
	if !errors.Is(err, base) {
		t.Error("WithUpstreamStatus broke the error chain")
	}
}

// A zero or nonsensical status carries no information, so it must fall through
// to the heuristic rather than classifying everything as unknown.
func TestWithUpstreamStatus_IgnoresUnusableStatus(t *testing.T) {
	for _, status := range []int{0, -1, 99} {
		err := wire.WithUpstreamStatus(errors.New("openai: rate limit exceeded"), status)
		if got := wire.Wrap(err); got.Type != wire.ErrTypeRateLimit {
			t.Errorf("status %d: Type = %q, want heuristic result %q", status, got.Type, wire.ErrTypeRateLimit)
		}
	}
	if wire.WithUpstreamStatus(nil, 500) != nil {
		t.Error("WithUpstreamStatus(nil, ...) should stay nil")
	}
}
