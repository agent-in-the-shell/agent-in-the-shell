// Package auth — anthropic_login.go implements the Authorization Code + PKCE
// OAuth flow for Anthropic's Claude Code public client.
package auth

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// AnthropicLogin performs the Authorization Code + PKCE OAuth flow for the
// Anthropic Claude Code public client, writing refreshable tokens to disk
// in agent-model's native auth.json format (see oauthCreds).
type AnthropicLogin struct {
	tokenDir   string
	httpClient *http.Client

	// Overridable endpoints + identity for tests.
	tokenURL     string
	authorizeURL string
	clientID     string

	// Overridable I/O for tests.
	stdin         io.Reader
	openBrowserFn func(url string)
}

// NewAnthropicLogin constructs a login flow. If tokenDir is empty the
// ANTHROPIC_OAUTH_TOKEN_DIR env var is consulted, then
// ~/.config/agentmodel/anthropic. If httpClient is nil a 30s client is used;
// the token exchange always uses its own 120s timeout regardless.
func NewAnthropicLogin(tokenDir string, httpClient *http.Client) *AnthropicLogin {
	if tokenDir == "" {
		tokenDir = defaultTokenDir("ANTHROPIC_OAUTH_TOKEN_DIR", "anthropic")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &AnthropicLogin{
		tokenDir:     tokenDir,
		httpClient:   httpClient,
		tokenURL:     AnthropicOAuthTokenURL,
		authorizeURL: AnthropicAuthorizeURL,
		clientID:     AnthropicClaudeCodeClientID,
		stdin:        os.Stdin,
	}
}

// SetEndpointsForTest overrides the token URL, authorize URL, and client ID.
// Pass empty string to keep the current value. Only for use in tests.
func (l *AnthropicLogin) SetEndpointsForTest(tokenURL, authorizeURL, clientID string) {
	if tokenURL != "" {
		l.tokenURL = tokenURL
	}
	if authorizeURL != "" {
		l.authorizeURL = authorizeURL
	}
	if clientID != "" {
		l.clientID = clientID
	}
}

// SetBrowserOpenerForTest replaces the function that opens the system browser.
// Only for use in tests.
func (l *AnthropicLogin) SetBrowserOpenerForTest(fn func(url string)) {
	l.openBrowserFn = fn
}

// SetStdinForTest replaces os.Stdin with a custom reader. Only for use in tests.
func (l *AnthropicLogin) SetStdinForTest(r io.Reader) {
	l.stdin = r
}

// PKCEPair generates a fresh PKCE verifier (32 random bytes, base64url) and
// its S256 challenge. Exported so callers and tests can verify the round-trip.
func PKCEPair() (verifier, challenge string) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		panic("anthropic login: rand.Read: " + err.Error())
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	challenge = pkceChallenge(verifier)
	return
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// parseCallbackCode extracts the authorization code from:
//   - bare code: "abc123"
//   - fragment style: "abc123#state"
//   - full callback URL: "https://console.anthropic.com/oauth/code/callback?code=abc123&state=..."
func parseCallbackCode(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", fmt.Errorf("empty input")
	}
	// Full URL
	if strings.HasPrefix(input, "http://") || strings.HasPrefix(input, "https://") {
		u, err := url.Parse(input)
		if err != nil {
			return "", fmt.Errorf("parse URL: %w", err)
		}
		code := u.Query().Get("code")
		if code == "" {
			return "", fmt.Errorf("no code parameter in URL")
		}
		return code, nil
	}
	// Fragment style: CODE#STATE
	if i := strings.IndexByte(input, '#'); i >= 0 {
		code := strings.TrimSpace(input[:i])
		if code == "" {
			return "", fmt.Errorf("empty code before '#'")
		}
		return code, nil
	}
	// Bare code
	return input, nil
}

// OpenBrowser attempts to open rawURL in the system browser. Non-fatal on all
// platforms; error is silently ignored.
func OpenBrowser(rawURL string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", rawURL)
	case "linux":
		cmd = exec.Command("xdg-open", rawURL)
	default:
		return
	}
	_ = cmd.Start()
}

// Run executes the PKCE Authorization Code flow using Anthropic's hosted
// callback page (AnthropicOAuthRedirectURI). The Anthropic public client only
// accepts that URI — dynamic loopback URIs are not on the allowlist.
//
//  1. Generate verifier + S256 challenge
//  2. Build authorize URL with the fixed redirect URI; open browser (non-fatal); print URL
//  3. User authenticates in the browser, gets redirected to console.anthropic.com/oauth/code/callback
//  4. User copies the code (or full callback URL) and pastes it here
//  5. Exchange code for tokens (120s timeout)
//  6. Persist tokens to auth.json
func (l *AnthropicLogin) Run(ctx context.Context) error {
	verifier, challenge := PKCEPair()

	// AnthropicOAuthRedirectURI is the only redirect URI registered for the
	// Claude Code public client. Dynamic loopback URIs are rejected by the
	// authorization server.
	redirectURI := AnthropicOAuthRedirectURI

	authURL := l.buildAuthorizeURL(verifier, challenge, redirectURI)

	fmt.Printf("🔑 Opening browser for Anthropic login...\n")
	fmt.Printf("   %s\n\n", authURL)
	fmt.Printf("   After authenticating, paste the code or callback URL here: ")

	opener := l.openBrowserFn
	if opener == nil {
		opener = OpenBrowser
	}
	opener(authURL)

	type callbackResult struct {
		code string
		err  error
	}
	ch := make(chan callbackResult, 1)

	// Read the authorization code from stdin. The user authenticates in the
	// browser, is redirected to console.anthropic.com/oauth/code/callback which
	// displays the code, then pastes it here as a bare code, code#state, or the
	// full callback URL.
	go func() {
		scanner := bufio.NewScanner(l.stdin)
		if scanner.Scan() {
			code, err := parseCallbackCode(scanner.Text())
			ch <- callbackResult{code: code, err: err}
		} else {
			ch <- callbackResult{err: fmt.Errorf("stdin closed without input")}
		}
	}()

	var r callbackResult
	select {
	case r = <-ch:
	case <-ctx.Done():
		return ctx.Err()
	}

	if r.err != nil {
		return fmt.Errorf("anthropic login: callback: %w", r.err)
	}

	tokens, err := l.exchangeCode(ctx, r.code, verifier, redirectURI)
	if err != nil {
		return fmt.Errorf("anthropic login: token exchange: %w", err)
	}

	creds := &oauthCreds{
		Provider:     providerAnthropic,
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		ExpiresAtMS:  expiryFromExpiresIn(tokens.ExpiresIn),
	}
	if err := writeCreds(l.tokenDir, creds); err != nil {
		return fmt.Errorf("anthropic login: persist: %w", err)
	}
	return nil
}

func (l *AnthropicLogin) buildAuthorizeURL(verifier, challenge, redirectURI string) string {
	params := url.Values{}
	params.Set("response_type", "code")
	params.Set("client_id", l.clientID)
	params.Set("redirect_uri", redirectURI)
	params.Set("scope", AnthropicOAuthScopes)
	params.Set("state", verifier)
	params.Set("code_challenge", challenge)
	params.Set("code_challenge_method", "S256")
	params.Set("code", "true") // required by claude.ai consent page to render the code
	return l.authorizeURL + "?" + params.Encode()
}

type anthropicTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

func (l *AnthropicLogin) exchangeCode(ctx context.Context, code, verifier, redirectURI string) (anthropicTokenResponse, error) {
	body, err := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     l.clientID,
		"code":          code,
		"state":         verifier, // Anthropic cross-checks state == code_verifier
		"redirect_uri":  redirectURI,
		"code_verifier": verifier,
	})
	if err != nil {
		return anthropicTokenResponse{}, err
	}

	// Token exchange has 40-60s tail latency under load. Use at most 120s;
	// parent context cancellation still propagates.
	exchangeCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(exchangeCtx, "POST", l.tokenURL, bytes.NewReader(body))
	if err != nil {
		return anthropicTokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := l.httpClient.Do(req)
	if err != nil {
		return anthropicTokenResponse{}, fmt.Errorf("HTTP: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return anthropicTokenResponse{}, fmt.Errorf("read response body: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return anthropicTokenResponse{}, fmt.Errorf("HTTP %s: %s", resp.Status, string(respBody))
	}
	var tr anthropicTokenResponse
	if err := json.Unmarshal(respBody, &tr); err != nil {
		return anthropicTokenResponse{}, fmt.Errorf("decode: %w", err)
	}
	if tr.AccessToken == "" {
		return anthropicTokenResponse{}, fmt.Errorf("empty access_token in response")
	}
	return tr, nil
}
