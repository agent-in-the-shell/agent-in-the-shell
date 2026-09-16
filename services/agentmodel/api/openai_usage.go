package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
)

// OpenAI/Codex subscription-usage fetch + normalization, the sibling of
// anthropic_usage.go. It lives here for the same reason that one does: this is
// the process that owns the ChatGPT OAuth credential, so it is the only one
// that should ever use or rotate it.
//
// agentshell used to read these numbers by spawning the user's codex CLI as a
// JSON-RPC app-server and calling account/rateLimits/read — a third credential
// store (codex's own) and a subprocess. This endpoint reads the same underlying
// data straight from the backend the codex CLI itself talks to, using the
// credential agent-model already holds from `agent-model chatgpt-login`.
const (
	defaultOpenAIUsagePath    = "/usage"
	limitSourceCodexOAuth     = "codex_oauth"
	codeCodexOAuthUnavailable = "codex_oauth_unavailable"
	codeCodexLoginRequired    = "codex_oauth_login_required"
	codeCodexUsageUnavailable = "codex_oauth_usage_unavailable"
	codeCodexNoLimits         = "codex_oauth_no_limits"

	// weeklyWindowSeconds keeps its historical "weekly" name rather than the
	// mechanical "7d", for continuity with the rows agentshell has always
	// emitted as codex_weekly.
	weeklyWindowSeconds = 604800
	// baseCodexLimitID is the identity of the account-wide limit, which the
	// payload reports without a metered_feature of its own.
	baseCodexLimitID = "codex"
)

// openaiUsageResponse is the shape of GET {ChatGPTAPIBase}/usage. Only the
// fields this endpoint normalizes are declared; the payload also carries
// credits/spend_control/promo blocks that no consumer asks for yet.
type openaiUsageResponse struct {
	PlanType             string                      `json:"plan_type"`
	RateLimit            *openaiRateLimit            `json:"rate_limit"`
	AdditionalRateLimits []openaiAdditionalRateLimit `json:"additional_rate_limits"`
}

type openaiRateLimit struct {
	PrimaryWindow   *openaiWindow `json:"primary_window"`
	SecondaryWindow *openaiWindow `json:"secondary_window"`
}

type openaiWindow struct {
	UsedPercent *float64 `json:"used_percent"`
	// LimitWindowSeconds is the window's own duration. It is the only safe
	// source for the row's name: OpenAI removed the 5-hour window on
	// 2026-07-12 and the weekly window took over the primary slot, so slot
	// position says nothing about duration.
	LimitWindowSeconds *int64 `json:"limit_window_seconds"`
	ResetAt            *int64 `json:"reset_at"`
}

// openaiAdditionalRateLimit is a per-model-family limit. MeteredFeature
// ("codex_bengalfox") is the stable machine identity; LimitName
// ("GPT-5.3-Codex-Spark") is server-controlled display prose that can churn,
// so it rides in RawMessage rather than shaping the key.
type openaiAdditionalRateLimit struct {
	LimitName      string           `json:"limit_name"`
	MeteredFeature string           `json:"metered_feature"`
	RateLimit      *openaiRateLimit `json:"rate_limit"`
}

// openaiWindowLabels maps a duration in seconds onto the two vocabularies the
// report uses: the token embedded in LimitName, and the human Window label.
// They are identical for every duration except a week, which the report has
// always called "weekly" rather than "7d".
//
// ok == false means the payload carried no usable duration, and the caller
// drops the row rather than naming it something that could collide.
func openaiWindowLabels(seconds *int64) (nameToken, windowLabel string, ok bool) {
	if seconds == nil || *seconds <= 0 {
		return "", "", false
	}
	s := *seconds
	if s == weeklyWindowSeconds {
		return "weekly", "7d", true
	}
	var label string
	switch {
	case s%86400 == 0:
		label = fmt.Sprintf("%dd", s/86400)
	case s%3600 == 0:
		label = fmt.Sprintf("%dh", s/3600)
	default:
		label = fmt.Sprintf("%dm", s/60)
	}
	return label, label, true
}

// openaiUsageWindows flattens the account-wide limit plus every per-model-family
// limit into one row set. Rows are named `<limitID>_<durationToken>`, so two
// windows of different durations can never share a name.
func openaiUsageWindows(usage openaiUsageResponse, now time.Time) []limitWindow {
	seen := map[string]bool{}
	windows := appendOpenAIWindows(nil, baseCodexLimitID, "", usage.RateLimit, now, seen)

	// Iterate sorted so row order is deterministic. The map restates the base
	// limit under its own id on some payloads; that entry is skipped rather
	// than relied on to collide by name.
	extra := make([]openaiAdditionalRateLimit, len(usage.AdditionalRateLimits))
	copy(extra, usage.AdditionalRateLimits)
	sort.Slice(extra, func(i, j int) bool { return extra[i].MeteredFeature < extra[j].MeteredFeature })
	for _, e := range extra {
		id := e.MeteredFeature
		if id == "" || id == baseCodexLimitID {
			continue
		}
		windows = appendOpenAIWindows(windows, id, e.LimitName, e.RateLimit, now, seen)
	}
	return windows
}

func appendOpenAIWindows(dst []limitWindow, limitID, displayLabel string, rl *openaiRateLimit, now time.Time, seen map[string]bool) []limitWindow {
	if rl == nil {
		return dst
	}
	for _, raw := range []*openaiWindow{rl.PrimaryWindow, rl.SecondaryWindow} {
		token, label, ok := openaiWindowLabels(rawWindowSeconds(raw))
		if !ok {
			continue // no usable duration: no correct name exists, so drop it
		}
		name := limitID + "_" + token
		if seen[name] {
			continue
		}
		w := openaiWindow2limit(name, label, raw, now)
		if w == nil {
			continue
		}
		w.RawMessage = displayLabel
		seen[name] = true
		dst = append(dst, *w)
	}
	return dst
}

func rawWindowSeconds(w *openaiWindow) *int64 {
	if w == nil {
		return nil
	}
	return w.LimitWindowSeconds
}

func openaiWindow2limit(name, window string, raw *openaiWindow, now time.Time) *limitWindow {
	if raw == nil || raw.UsedPercent == nil {
		return nil
	}
	usedPercent := usageClampPercent(*raw.UsedPercent)
	remaining := 100 - usedPercent
	var resetAt *time.Time
	if raw.ResetAt != nil {
		t := time.Unix(*raw.ResetAt, 0).UTC()
		resetAt = &t
	}
	status := limitStatusOK
	if usedPercent >= 100 {
		status = limitStatusLimitd
	}
	limit := float64(100)
	return &limitWindow{
		LimitName:    name,
		Window:       window,
		Unit:         "percent",
		Limit:        &limit,
		Remaining:    &remaining,
		Used:         &usedPercent,
		UsedPercent:  &usedPercent,
		ResetAt:      resetAt,
		Source:       limitSourceCodexOAuth,
		AsOf:         now,
		WindowStatus: status,
	}
}

// fetchOpenAIUsage GETs the codex backend's usage endpoint with the ChatGPT
// subscription credential applied. Read-only: it never POSTs /responses, so it
// cannot consume the quota it reports.
func fetchOpenAIUsage(ctx context.Context, client *http.Client, baseURL string, authenticator *auth.ChatGPTOAuth) (openaiUsageResponse, error) {
	var out openaiUsageResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+defaultOpenAIUsagePath, nil)
	if err != nil {
		return out, err
	}
	if err := authenticator.Apply(ctx, req); err != nil {
		return out, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", auth.ChatGPTUserAgent)
	req.Header.Set("originator", auth.ChatGPTOriginator)

	resp, err := client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Bounded echo of the upstream body, matching the anthropic sibling.
		// Both handlers pass it through unredacted;  tracks that, and now
		// covers this one too — a divergent half-redactor here would be worse
		// than one shared fix.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return out, fmt.Errorf("codex usage: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("codex usage: decode: %w", err)
	}
	return out, nil
}

// AccountUsageResponse is the wire shape of the /v1/account/*/usage endpoints,
// exported so `agent-model limits` can render it without a running server.
type AccountUsageResponse = accountUsageResponse

// FetchOpenAIUsage fetches and normalizes Codex subscription usage in-process,
// building its own credential from the default token dir. It exists so the CLI
// does not have to round-trip through a gateway it may not be running — the
// process that owns the credential is the same one rendering the table.
func FetchOpenAIUsage(ctx context.Context, now time.Time) AccountUsageResponse {
	// One client for both the credential's refresh round-trip and the usage GET,
	// mirroring how the server shares anthropicClient between the two.
	client := &http.Client{Timeout: 30 * time.Second}
	usage, err := fetchOpenAIUsage(ctx, client, auth.ChatGPTAPIBase, auth.NewChatGPTOAuth("", client))
	if err != nil {
		code := codeCodexUsageUnavailable
		if auth.IsLoginRequired(err) {
			code = codeCodexLoginRequired
		}
		return AccountUsageResponse{Status: limitStatusUnknown, Windows: []limitWindow{},
			Error: &accountProbeError{Code: code, Message: err.Error()}}
	}
	windows := openaiUsageWindows(usage, now)
	if len(windows) == 0 {
		return AccountUsageResponse{Status: limitStatusUnknown, Windows: []limitWindow{},
			Error: &accountProbeError{Code: codeCodexNoLimits, Message: "Codex usage endpoint returned no limit windows."}}
	}
	fillResetInSeconds(windows, now)
	return AccountUsageResponse{
		Status:  rollupWindowStatus(windows),
		Auth:    &accountAuthInfo{Mode: agentmodel.AuthModeSubscription, ModeSource: "agentmodel_oauth", Plan: usage.PlanType},
		Windows: windows,
	}
}

// FetchAnthropicUsage is FetchOpenAIUsage's sibling for one Anthropic profile.
func FetchAnthropicUsage(ctx context.Context, profile string, now time.Time) AccountUsageResponse {
	if profile == "" {
		profile = auth.DefaultProfile
	}
	client := &http.Client{Timeout: 30 * time.Second}
	token, err := auth.NewAnthropicOAuthRefreshableForProfile(profile, client).Token(ctx)
	if err != nil {
		code := codeClaudeOAuthUnavailable
		if auth.IsLoginRequired(err) {
			code = codeClaudeLoginRequired
		}
		return AccountUsageResponse{Status: limitStatusUnknown, Windows: []limitWindow{},
			Error: &accountProbeError{Code: code, Message: err.Error()}}
	}
	usage, err := fetchAnthropicUsage(ctx, client, defaultAnthropicUsageURL, token, now)
	if err != nil {
		return AccountUsageResponse{Status: limitStatusUnknown, Windows: []limitWindow{},
			Error: &accountProbeError{Code: codeClaudeUsageUnavailable, Message: err.Error()}}
	}
	windows := anthropicUsageWindows(usage, now)
	if len(windows) == 0 {
		return AccountUsageResponse{Status: limitStatusUnknown, Windows: []limitWindow{},
			Error: &accountProbeError{Code: codeClaudeNoLimits, Message: "Claude OAuth usage endpoint returned no limit windows."}}
	}
	fillResetInSeconds(windows, now)
	return AccountUsageResponse{
		Status:  rollupWindowStatus(windows),
		Auth:    &accountAuthInfo{Mode: agentmodel.AuthModeSubscription, ModeSource: "agentmodel_oauth"},
		Windows: windows,
	}
}

// fillResetInSeconds derives a clock-skew-safe countdown from the report's own
// anchor, so a consumer never has to trust its own clock against reset_at.
func fillResetInSeconds(windows []limitWindow, now time.Time) {
	for i := range windows {
		if windows[i].ResetAt == nil {
			continue
		}
		secs := int64(windows[i].ResetAt.Sub(now).Round(time.Second).Seconds())
		if secs < 0 {
			secs = 0
		}
		windows[i].ResetInSeconds = &secs
	}
}
