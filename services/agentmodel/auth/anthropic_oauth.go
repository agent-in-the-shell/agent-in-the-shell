package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
)

// AnthropicOAuthTokenPrefix is the prefix Anthropic uses for OAuth access
// tokens issued via Claude Code's "Login with Claude" flow. These tokens
// represent a Claude Pro/Max subscription bearer rather than an API key.
const AnthropicOAuthTokenPrefix = "sk-ant-oat"

// AnthropicOAuthBetaHeader is the full set of Anthropic beta flags sent when
// calling the API via an OAuth subscription token. Mirrors the set used by the
// Claude Code CLI (verified against 9router open-sse/config/providers.js, May 2026):
//   - claude-code-20250219: identifies client as Claude Code subscription (required for Sonnet+)
//   - oauth-2025-04-20: standard OAuth beta flag
//   - interleaved-thinking-2025-05-14: thinking blocks interleaved with tool calls
//   - fine-grained-tool-streaming-2025-05-14: correct streaming tool-call delta behavior
//   - context-management-2025-06-27: server-side context window management
//   - prompt-caching-scope-2026-01-05: extended prompt caching scope
//   - effort-2025-11-24: request-level effort/reasoning budget control
//   - structured-outputs-2025-12-15: structured JSON outputs
//   - fast-mode-2026-02-01: fast response mode
//   - redact-thinking-2026-02-12: redacted thinking blocks (already handled in streaming)
//   - token-efficient-tools-2026-03-28: compressed tool call wire format
const AnthropicOAuthBetaHeader = "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14,context-management-2025-06-27,prompt-caching-scope-2026-01-05,effort-2025-11-24,structured-outputs-2025-12-15,fast-mode-2026-02-01,redact-thinking-2026-02-12,token-efficient-tools-2026-03-28"

// claudeCodeUserAgent and claudeCodeXApp mimic the Claude Code CLI identity
// headers pi-ai sends for OAuth requests (anthropic.js lines 438-439).
const claudeCodeUserAgent = "claude-cli/2.1.75"
const claudeCodeXApp = "cli"

// applyAnthropicOAuthHeaders mutates req to carry the Authorization Bearer +
// anthropic-beta + dangerous-direct-browser-access headers required for any
// Anthropic API call authenticated via OAuth (Claude Code Pro/Max
// subscription). Shared between the static AnthropicOAuth and the
// refreshable variant so they can't drift.
func applyAnthropicOAuthHeaders(req *http.Request, token string) {
	req.Header.Del("x-api-key")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-dangerous-direct-browser-access", "true")
	existing := req.Header.Get("anthropic-beta")
	req.Header.Set("anthropic-beta", mergeCSV(existing, AnthropicOAuthBetaHeader))
	req.Header.Set("User-Agent", claudeCodeUserAgent)
	req.Header.Set("x-app", claudeCodeXApp)
}

// AnthropicOAuth wraps a Claude OAuth bearer token (sk-ant-oat*) and applies
// the headers required to authenticate via subscription rather than API key.
type AnthropicOAuth struct {
	Token string
}

// NewAnthropicOAuth strips an optional "Bearer " prefix from token and returns
// an AnthropicOAuth ready to apply.
func NewAnthropicOAuth(token string) *AnthropicOAuth {
	return &AnthropicOAuth{Token: stripBearer(token)}
}

// IsAnthropicOAuthKey reports whether the supplied raw value (with or without
// a "Bearer " prefix) looks like a Claude OAuth access token.
func IsAnthropicOAuthKey(value string) bool {
	if value == "" {
		return false
	}
	v := stripBearer(value)
	return strings.HasPrefix(v, AnthropicOAuthTokenPrefix)
}

func (a *AnthropicOAuth) Mode() string { return agentmodel.AuthModeSubscription }

func (a *AnthropicOAuth) Apply(_ context.Context, req *http.Request) error {
	applyAnthropicOAuthHeaders(req, a.Token)
	return nil
}

func stripBearer(v string) string {
	const p = "Bearer "
	if strings.HasPrefix(v, p) {
		return v[len(p):]
	}
	return v
}

// MergeCSV merges two comma-separated header values, adding only items from
// new that are not already present in existing. Order is preserved.
func MergeCSV(existing, new string) string {
	if new == "" {
		return existing
	}
	if existing == "" {
		return new
	}
	seen := make(map[string]struct{})
	parts := strings.Split(existing, ",")
	for _, p := range parts {
		if p != "" {
			seen[p] = struct{}{}
		}
	}
	added := false
	for _, p := range strings.Split(new, ",") {
		if p == "" {
			continue
		}
		if _, ok := seen[p]; !ok {
			seen[p] = struct{}{}
			parts = append(parts, p)
			added = true
		}
	}
	if !added {
		return existing
	}
	return strings.Join(parts, ",")
}

func mergeCSV(existing, new string) string { return MergeCSV(existing, new) }
