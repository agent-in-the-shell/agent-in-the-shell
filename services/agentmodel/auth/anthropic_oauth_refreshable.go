package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
)

// Anthropic OAuth refresh constants. The client_id is Claude Code's
// public OAuth application identifier; refresh requests must use it. The
// endpoint speaks RFC 6749 standard refresh_token grant.
const (
	AnthropicOAuthTokenURL = "https://api.anthropic.com/v1/oauth/token"
	// This is a public identifier (PKCE flow, no client secret), not a
	// credential — gitleaks' entropy rule false-positives on the UUID.
	AnthropicClaudeCodeClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e" //gitleaks:allow

	// AnthropicAuthorizeURL is the claude.ai authorization endpoint for the
	// Claude Code OAuth client's Authorization Code + PKCE flow.
	AnthropicAuthorizeURL = "https://claude.ai/oauth/authorize"

	// AnthropicOAuthRedirectURI is the hosted callback page users are sent to
	// when they approve the OAuth request. The page displays a code#state string
	// the user can paste into the CLI when the loopback callback is unavailable.
	AnthropicOAuthRedirectURI = "https://console.anthropic.com/oauth/code/callback"

	// AnthropicOAuthScopes is the minimum scope set accepted by
	// GET /api/oauth/usage. Omitting user:profile returns HTTP 403 with
	// "OAuth token does not meet scope requirement user:profile".
	AnthropicOAuthScopes = "user:inference user:profile"
)

// AnthropicOAuthRefreshable is a long-lived bearer-token authenticator that
// auto-refreshes against Anthropic's OAuth token endpoint when the current
// access token nears expiry.
//
// Storage: tokens persist as JSON at <tokenDir>/auth.json in agent-model's
// own native credential format (see oauthCreds). The store is agent-model's
// alone — it is not shared with, nor schema-compatible with, any other tool.
//
// Concurrency: getAccessToken uses a sync.RWMutex so the hot path (cached
// token still fresh) takes only a shared read-lock; only the cold path that
// triggers a refresh acquires the exclusive write-lock. Concurrent inference
// calls don't serialize against each other while the cache is valid.
type AnthropicOAuthRefreshable struct {
	tokenDir   string
	httpClient *http.Client

	// Endpoint + identity overrides for tests. Production code uses the
	// public Anthropic constants.
	tokenURL string
	clientID string

	mu               sync.RWMutex
	cached           string    // access token
	cachedExp        int64     // unix milliseconds
	lastForceRefresh time.Time // when ForceRefresh last rotated the token (coalescing)
}

// NewAnthropicOAuthRefreshable constructs a refreshable Anthropic OAuth
// authenticator persisting tokens under tokenDir. If httpClient is nil a
// default with 30s timeout is used. If tokenDir is empty the
// `ANTHROPIC_OAUTH_TOKEN_DIR` env var is consulted, falling back to
// `~/.config/agentmodel/anthropic`.
func NewAnthropicOAuthRefreshable(tokenDir string, httpClient *http.Client) *AnthropicOAuthRefreshable {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if tokenDir == "" {
		// Route through tokenDirForProfile(DefaultProfile) so the empty-tokenDir
		// path picks up the same legacy-fallback / default-profile resolution
		// the writer (anthropic-login → ResolveProfileTokenDir) uses. Otherwise
		// fresh installs would write to <base>/default/auth.json but read from
		// <base>/auth.json, never finding the just-written tokens.
		tokenDir = tokenDirForProfile(anthropicProfileEnvVar, anthropicProfileSubdir, DefaultProfile)
	}
	return &AnthropicOAuthRefreshable{
		tokenDir:   tokenDir,
		httpClient: httpClient,
		tokenURL:   AnthropicOAuthTokenURL,
		clientID:   AnthropicClaudeCodeClientID,
	}
}

// NewAnthropicOAuthRefreshableForProfile constructs a refresher for a named
// Anthropic profile. The profile directory is resolved under the configured
// Anthropic token root.
func NewAnthropicOAuthRefreshableForProfile(profile string, httpClient *http.Client) *AnthropicOAuthRefreshable {
	if profile == "" {
		profile = DefaultProfile
	}
	return NewAnthropicOAuthRefreshable(tokenDirForProfile(anthropicProfileEnvVar, anthropicProfileSubdir, profile), httpClient)
}

// SetEndpointsForTest points the refresher at a fake server. Exposed for
// cross-package tests; not part of the stable API.
func (a *AnthropicOAuthRefreshable) SetEndpointsForTest(tokenURL, clientID string) {
	a.tokenURL = tokenURL
	if clientID != "" {
		a.clientID = clientID
	}
}

// Mode returns "subscription" — caller-visible auth mode (matches static
// AnthropicOAuth so observability is consistent).
func (a *AnthropicOAuthRefreshable) Mode() string { return agentmodel.AuthModeSubscription }

// Apply sets the standard Anthropic-OAuth headers on req. It triggers a
// refresh if the cached access token is expired or absent. Returns
// ErrLoginRequired if there's no usable refresh_token on disk (initial
// login must be done by an external tool — typically `pi /login`).
func (a *AnthropicOAuthRefreshable) Apply(ctx context.Context, req *http.Request) error {
	token, err := a.getAccessToken(ctx)
	if err != nil {
		return err
	}
	applyAnthropicOAuthHeaders(req, token)
	return nil
}

// Token returns the current access token, refreshing if needed.
// Use this when you need the raw token string (e.g. to inject as env var).
func (a *AnthropicOAuthRefreshable) Token(ctx context.Context) (string, error) {
	return a.getAccessToken(ctx)
}

func (a *AnthropicOAuthRefreshable) getAccessToken(ctx context.Context) (string, error) {
	// Hot path: shared read-lock. Lets concurrent inference calls return
	// the cached token in parallel.
	a.mu.RLock()
	if a.cached != "" && !expiredMS(a.cachedExp) {
		token := a.cached
		a.mu.RUnlock()
		return token, nil
	}
	a.mu.RUnlock()

	// Cold path: take exclusive lock for refresh.
	a.mu.Lock()
	defer a.mu.Unlock()

	// Re-check under exclusive lock (a sibling goroutine may have refreshed
	// between RUnlock and Lock).
	if a.cached != "" && !expiredMS(a.cachedExp) {
		return a.cached, nil
	}

	// Re-read disk so we pick up tokens written by a concurrent anthropic-login
	// and avoid an unnecessary refresh round-trip. Distinguish missing-file
	// (expected on first run) from parse failure (operator must know about a
	// corrupted auth.json).
	disk, err := readCreds(a.tokenDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("anthropic auth: read %s: %w", authFilePath(a.tokenDir), err)
	}
	if disk != nil && disk.AccessToken != "" && !expiredMS(disk.ExpiresAtMS) {
		a.cached = disk.AccessToken
		a.cachedExp = disk.ExpiresAtMS
		return a.cached, nil
	}

	if disk == nil || disk.RefreshToken == "" {
		return "", ErrLoginRequired
	}

	refreshed, err := a.refresh(ctx, disk.RefreshToken)
	if err != nil {
		return "", err
	}
	if err := writeCreds(a.tokenDir, refreshed); err != nil {
		return "", err
	}
	a.cached = refreshed.AccessToken
	a.cachedExp = refreshed.ExpiresAtMS
	return a.cached, nil
}

// ForceRefresh discards any cached token and refreshes against the OAuth
// endpoint using the on-disk refresh_token, regardless of the locally recorded
// expiry. It exists for reactive recovery: the upstream can reject an access
// token our local expiry still considered valid — Anthropic subscription tokens
// are opaque (no parseable expiry), so a token can lapse before our recorded
// expiry fires.
//
// Returns ErrLoginRequired when there is no refresh_token to refresh with.
// Concurrent callers are coalesced via forceRefreshCoalesceWindow (shared with
// the ChatGPT authenticator) so a 401 storm rotates the refresh_token at most
// once.
func (a *AnthropicOAuthRefreshable) ForceRefresh(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	// A sibling goroutine may have just refreshed in response to the same 401
	// storm; reuse its result rather than rotating the refresh_token again. A
	// zero lastForceRefresh (never refreshed) is far in the past, so the window
	// check alone correctly falls through to a real refresh.
	if a.cached != "" && time.Since(a.lastForceRefresh) < forceRefreshCoalesceWindow {
		return nil
	}

	disk, err := readCreds(a.tokenDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("anthropic auth: read %s: %w", authFilePath(a.tokenDir), err)
	}
	if disk == nil || disk.RefreshToken == "" {
		return ErrLoginRequired
	}
	refreshed, err := a.refresh(ctx, disk.RefreshToken)
	if err != nil {
		return err
	}
	if err := writeCreds(a.tokenDir, refreshed); err != nil {
		return err
	}
	a.cached = refreshed.AccessToken
	a.cachedExp = refreshed.ExpiresAtMS
	a.lastForceRefresh = time.Now()
	return nil
}

func (a *AnthropicOAuthRefreshable) refresh(ctx context.Context, refreshToken string) (*oauthCreds, error) {
	body, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     a.clientID,
		"refresh_token": refreshToken,
	})
	if err != nil {
		return nil, fmt.Errorf("anthropic refresh: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", a.tokenURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic refresh: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic refresh: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("anthropic refresh: %s: %s", resp.Status, string(respBody))
	}

	var data struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Scope        string `json:"scope,omitempty"`
	}
	if err := json.Unmarshal(respBody, &data); err != nil {
		return nil, fmt.Errorf("anthropic refresh: decode: %w", err)
	}
	if data.AccessToken == "" {
		return nil, fmt.Errorf("anthropic refresh: empty access_token in response: %s", string(respBody))
	}

	out := &oauthCreds{
		Provider:     providerAnthropic,
		AccessToken:  data.AccessToken,
		RefreshToken: data.RefreshToken,
		ExpiresAtMS:  expiryFromExpiresIn(data.ExpiresIn),
	}
	if out.RefreshToken == "" {
		// Some refresh responses omit refresh_token; reuse the previous one.
		out.RefreshToken = refreshToken
	}
	return out, nil
}
