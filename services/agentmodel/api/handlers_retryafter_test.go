package api_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/stub"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
)

// A gateway 429 must tell its own clients when to come back. Without the
// header, every caller falls back to guessing — the same waste the credential
// cooldowns just stopped making internally.
func TestChatErrorResponseRetryAfterHeader(t *testing.T) {
	cases := []struct {
		desc        string
		completeErr error
		want        string
	}{
		{"upstream hint is passed through", stub.RateLimitAfter(15 * time.Minute), "900"},
		// Retry-After is whole seconds: a sub-second hint floored to "0" would
		// read as "retry immediately" and hammer an upstream that asked for a
		// pause.
		{"sub-second hint rounds up", stub.RateLimitAfter(300 * time.Millisecond), "1"},
		// No hint, no header: an invented value is a guess dressed as fact.
		{"no hint leaves the header off", stub.ErrTestRateLimit, ""},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			ts, _ := newTestServer(t, map[string][]router.Deployment{
				"gpt-4": {{
					Name:     "oa/gpt",
					Provider: &stub.Stub{NameValue: "openai", CompleteErr: c.completeErr},
					Model:    "gpt-4o",
					Weight:   1,
				}},
			})

			resp := mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), testToken)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429", resp.StatusCode)
			}
			if got := resp.Header.Get("Retry-After"); got != c.want {
				t.Errorf("Retry-After = %q, want %q", got, c.want)
			}
		})
	}
}

// The Anthropic-shaped frontend serves the same clients (Claude Code, pi-ai)
// and needs the same header. This path never sees a Go error — the upstream 429
// arrives as a live response, so the hint has to be read off its headers.
func TestMessagesRateLimitResponseCarriesRetryAfterHeader(t *testing.T) {
	limited := &stub.Stub{NameValue: "anthropic", MessagesPassthroughFn: func(context.Context, []byte, string, string) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Retry-After": []string{"90"}},
			Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error"}}`)),
		}, nil
	}}
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"claude": {{
			Name:     "anth/claude",
			Provider: limited,
			Model:    "claude-sonnet-4-5",
			Weight:   1,
		}},
	})

	resp := mustPost(t, ts, "/v1/messages", map[string]any{
		"model":      "claude",
		"max_tokens": 16,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	}, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "90" {
		t.Errorf("Retry-After = %q, want \"90\"", got)
	}
}
