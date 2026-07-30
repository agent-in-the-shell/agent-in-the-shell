package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
)

// Anthropic subscription-usage fetch + normalization. This logic used to live
// in services/agentshell/limits.go, which reached past the gateway to hit
// Anthropic directly using agent-model's OAuth credentials. It now lives here,
// on the side that owns those credentials, so a single process is the only one
// that ever uses/refreshes the (single-use, rotating) refresh token. agent-shell
// consumes the result over HTTP via GET /v1/account/anthropic/usage.

const (
	defaultAnthropicUsageURL   = "https://api.anthropic.com/api/oauth/usage"
	anthropicUsageBetaHeader   = "oauth-2025-04-20"
	limitStatusUnknown         = "unknown"
	limitSourceClaudeOAuth     = "claude_oauth"
	codeClaudeOAuthUnavailable = "claude_oauth_unavailable"
	codeClaudeLoginRequired    = "claude_oauth_login_required"
	codeClaudeUsageUnavailable = "claude_oauth_usage_unavailable"
	codeClaudeRateLimited      = "claude_oauth_rate_limited"
	codeClaudeNoLimits         = "claude_oauth_no_limits"
)

type anthropicUsageResponse struct {
	FiveHour          *anthropicUsageWindow `json:"five_hour"`
	SevenDay          *anthropicUsageWindow `json:"seven_day"`
	SevenDayOAuthApps *anthropicUsageWindow `json:"seven_day_oauth_apps"`
	SevenDayOpus      *anthropicUsageWindow `json:"seven_day_opus"`
	SevenDaySonnet    *anthropicUsageWindow `json:"seven_day_sonnet"`
}

type anthropicUsageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

// anthropicRateLimitedError marks an HTTP 429 from the usage endpoint so the
// handler can emit the self-explanatory claude_oauth_rate_limited code rather
// than collapsing throttling into the catch-all usage-unavailable code. The
// usage endpoint shares a per-account budget with the running Claude Code
// session(s), so 429s are typically transient and self-healing.
type anthropicRateLimitedError struct {
	retryAfter time.Duration // 0 when the server sent no parseable Retry-After
}

func (e *anthropicRateLimitedError) Error() string {
	if e.retryAfter > 0 {
		return fmt.Sprintf("claude oauth usage: rate limited (HTTP 429); retry after %s", e.retryAfter.Round(time.Second))
	}
	return "claude oauth usage: rate limited (HTTP 429)"
}

// fetchAnthropicUsage GETs the OAuth usage snapshot with the supplied access
// token. A 401 maps to ErrLoginRequired; a 429 to anthropicRateLimitedError.
func fetchAnthropicUsage(ctx context.Context, client *http.Client, usageURL, token string, now time.Time) (anthropicUsageResponse, error) {
	if usageURL == "" {
		usageURL = defaultAnthropicUsageURL
	}
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		return anthropicUsageResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-beta", anthropicUsageBetaHeader)
	req.Header.Set("User-Agent", "claude-code/1.0.0")

	resp, err := client.Do(req)
	if err != nil {
		return anthropicUsageResponse{}, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return anthropicUsageResponse{}, auth.ErrLoginRequired
	case resp.StatusCode == http.StatusTooManyRequests:
		return anthropicUsageResponse{}, &anthropicRateLimitedError{
			retryAfter: agentmodel.ParseRetryAfter(resp.Header.Get("Retry-After"), now),
		}
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return anthropicUsageResponse{}, fmt.Errorf("claude oauth usage: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var usage anthropicUsageResponse
	if err := json.NewDecoder(resp.Body).Decode(&usage); err != nil {
		return anthropicUsageResponse{}, fmt.Errorf("claude oauth usage: decode: %w", err)
	}
	return usage, nil
}

// anthropicUsageWindows maps the usage response onto the wire limitWindow
// contract agent-shell consumes. Order matches the source-of-truth order the
// agentshell probe emitted before this moved server-side.
func anthropicUsageWindows(usage anthropicUsageResponse, now time.Time) []limitWindow {
	candidates := []struct {
		name   string
		window string
		source *anthropicUsageWindow
	}{
		{name: "claude_5h", window: "5h", source: usage.FiveHour},
		{name: "claude_weekly", window: "7d", source: usage.SevenDay},
		{name: "claude_oauth_apps_weekly", window: "7d", source: usage.SevenDayOAuthApps},
		{name: "claude_opus_weekly", window: "7d", source: usage.SevenDayOpus},
		{name: "claude_sonnet_weekly", window: "7d", source: usage.SevenDaySonnet},
	}
	windows := make([]limitWindow, 0, len(candidates))
	for _, c := range candidates {
		if w := anthropicUsageWindow2limit(c.name, c.window, c.source, now); w != nil {
			windows = append(windows, *w)
		}
	}
	return windows
}

func anthropicUsageWindow2limit(name, window string, raw *anthropicUsageWindow, now time.Time) *limitWindow {
	if raw == nil || raw.Utilization == nil {
		return nil
	}
	usedPercent := usageClampPercent(*raw.Utilization)
	remaining := 100 - usedPercent
	var resetAt *time.Time
	if raw.ResetsAt != "" {
		if t, err := parseAnthropicTime(raw.ResetsAt); err == nil {
			resetAt = &t
		}
	}
	status := limitStatusOK
	if usedPercent >= 100 {
		status = limitStatusLimitd
	}
	limit := 100.0
	return &limitWindow{
		LimitName:    name,
		Window:       window,
		Unit:         "percent",
		Limit:        &limit,
		Remaining:    &remaining,
		Used:         &usedPercent,
		UsedPercent:  &usedPercent,
		ResetAt:      resetAt,
		Source:       limitSourceClaudeOAuth,
		AsOf:         now,
		WindowStatus: status,
	}
}

func parseAnthropicTime(value string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t.UTC(), nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

func usageClampPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
