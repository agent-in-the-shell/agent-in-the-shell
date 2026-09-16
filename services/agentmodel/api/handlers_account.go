package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
)

// GET /v1/account/anthropic/usage — upstream Anthropic subscription usage for
// one profile.
//
// This is the read side of the provider account's own quota, distinct from
// /v1/limits (which reports the gateway's OWN rpm/tpm/budget caps). It is served
// here rather than fetched by the caller so agent-model — the owner of the
// Anthropic OAuth credentials — is the single process that ever uses or rotates
// the (single-use, rotating) refresh token. agent-shell's `limits claude` probe
// consumes this over HTTP; the JSON shape is the BackendLimits/LimitWindow
// contract in services/agentshell/limits.go (mirrored, not imported, to keep the
// dependency one-way).
//
// ?profile=<name> selects the Anthropic profile (default: "default"). A missing
// or expired token yields HTTP 200 with a structured error (claude_oauth_login_
// required / _unavailable) so the caller renders a normal "unknown" row rather
// than treating the probe as a transport failure.
type accountUsageResponse struct {
	Status  string             `json:"status"`
	Auth    *accountAuthInfo   `json:"auth,omitempty"`
	Windows []limitWindow      `json:"windows"`
	Error   *accountProbeError `json:"error,omitempty"`
}

// accountAuthInfo carries only what this endpoint actually emits: the auth mode
// and its source. (agent-shell's BackendLimits mirror also has account/plan
// fields, populated by the codex probe — not by this subscription endpoint.)
type accountAuthInfo struct {
	Mode       string `json:"mode,omitempty"`
	ModeSource string `json:"mode_source,omitempty"`
	// Plan is the subscription tier when the upstream reports one. The
	// anthropic usage endpoint does not; the codex one does (plan_type).
	Plan string `json:"plan,omitempty"`
}

type accountProbeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (s *Server) anthropicUsage(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	profile := strings.TrimSpace(r.URL.Query().Get("profile"))
	if profile == "" {
		profile = auth.DefaultProfile
	}
	if err := auth.ValidateProfileName(profile); err != nil {
		writeJSON(w, http.StatusBadRequest, accountUsageResponse{
			Status:  limitStatusUnknown,
			Windows: []limitWindow{},
			Error:   &accountProbeError{Code: "invalid_profile", Message: err.Error()},
		})
		return
	}

	token, err := s.anthropicRefresher(profile).Token(r.Context())
	if err != nil {
		code := codeClaudeOAuthUnavailable
		if auth.IsLoginRequired(err) {
			code = codeClaudeLoginRequired
		}
		writeUsageUnknown(w, code, err.Error())
		return
	}

	usage, err := fetchAnthropicUsage(r.Context(), s.anthropicClient, s.anthropicUsageURL, token, now)
	if err != nil {
		code := codeClaudeUsageUnavailable
		var rl *anthropicRateLimitedError
		switch {
		case errors.As(err, &rl):
			code = codeClaudeRateLimited
		case auth.IsLoginRequired(err):
			code = codeClaudeLoginRequired
		}
		writeUsageUnknown(w, code, err.Error())
		return
	}

	windows := anthropicUsageWindows(usage, now)
	if len(windows) == 0 {
		writeUsageUnknown(w, codeClaudeNoLimits, "Claude OAuth usage endpoint returned no limit windows.")
		return
	}

	fillResetInSeconds(windows, now)

	writeJSON(w, http.StatusOK, accountUsageResponse{
		Status:  rollupWindowStatus(windows),
		Auth:    &accountAuthInfo{Mode: agentmodel.AuthModeSubscription, ModeSource: "agentmodel_oauth"},
		Windows: windows,
	})
}

// writeUsageUnknown emits an HTTP 200 with a structured "unknown" row carrying a
// probe error. Login and usage-fetch failures are reported this way — not as an
// HTTP error status — so the agent-shell consumer renders a normal row instead
// of treating the probe as a transport fault. (Malformed requests still 400.)
func writeUsageUnknown(w http.ResponseWriter, code, message string) {
	writeJSON(w, http.StatusOK, accountUsageResponse{
		Status:  limitStatusUnknown,
		Windows: []limitWindow{},
		Error:   &accountProbeError{Code: code, Message: message},
	})
}

// anthropicRefresher returns the profile's cached refreshable, constructing it
// on first use. Caching per profile keeps token rotation coordinated within
// this process: two concurrent usage probes for the same profile share one
// refresher (and its RWMutex-guarded token cache) instead of racing to rotate
// the single-use refresh token.
func (s *Server) anthropicRefresher(profile string) *auth.AnthropicOAuthRefreshable {
	s.anthropicMu.Lock()
	defer s.anthropicMu.Unlock()
	if ref, ok := s.anthropicRefreshers[profile]; ok {
		return ref
	}
	ref := auth.NewAnthropicOAuthRefreshableForProfile(profile, s.anthropicClient)
	s.anthropicRefreshers[profile] = ref
	return ref
}

func rollupWindowStatus(windows []limitWindow) string {
	for _, w := range windows {
		if w.WindowStatus == limitStatusLimitd {
			return limitStatusLimitd
		}
	}
	return limitStatusOK
}

// GET /v1/account/openai/usage — upstream OpenAI/Codex subscription usage.
//
// The sibling of anthropicUsage, and served here for the same reason: this
// process owns the ChatGPT OAuth credential. A missing credential or an
// upstream failure yields HTTP 200 with a structured error so the caller
// renders a normal "unknown" row rather than treating it as a transport fault.
func (s *Server) openaiUsage(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	if s.chatgptAuth == nil {
		writeUsageUnknown(w, codeCodexLoginRequired,
			"no ChatGPT subscription credential is configured; run `agent-model chatgpt-login`")
		return
	}

	base := s.openaiUsageBase
	if base == "" {
		base = auth.ChatGPTAPIBase
	}
	usage, err := fetchOpenAIUsage(r.Context(), s.openaiClient, base, s.chatgptAuth)
	if err != nil {
		code := codeCodexUsageUnavailable
		if auth.IsLoginRequired(err) {
			code = codeCodexLoginRequired
		}
		writeUsageUnknown(w, code, err.Error())
		return
	}

	windows := openaiUsageWindows(usage, now)
	if len(windows) == 0 {
		writeUsageUnknown(w, codeCodexNoLimits, "Codex usage endpoint returned no limit windows.")
		return
	}
	fillResetInSeconds(windows, now)

	writeJSON(w, http.StatusOK, accountUsageResponse{
		Status: rollupWindowStatus(windows),
		Auth: &accountAuthInfo{
			Mode:       agentmodel.AuthModeSubscription,
			ModeSource: "agentmodel_oauth",
			Plan:       usage.PlanType,
		},
		Windows: windows,
	})
}
