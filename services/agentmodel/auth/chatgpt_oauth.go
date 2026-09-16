package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
)

// Constants ported from litellm/llms/chatgpt/common_utils.py. We mimic the
// Codex CLI client identity because OpenAI's ChatGPT subscription endpoint
// (chatgpt.com/backend-api/codex) is what Codex CLI uses; third-party clients
// authenticate the same way.
//
// IMPORTANT: this is a reverse-engineered integration. OpenAI does not
// formally support third-party clients on this endpoint. Track upstream
// LiteLLM and Codex CLI for changes; pin our defaults to known-working values.
const (
	ChatGPTAuthBase           = "https://auth.openai.com"
	ChatGPTDeviceCodeURL      = ChatGPTAuthBase + "/api/accounts/deviceauth/usercode"
	ChatGPTDeviceTokenURL     = ChatGPTAuthBase + "/api/accounts/deviceauth/token"
	ChatGPTOAuthTokenURL      = ChatGPTAuthBase + "/oauth/token"
	ChatGPTDeviceVerifyURL    = ChatGPTAuthBase + "/codex/device"
	ChatGPTAPIBase            = "https://chatgpt.com/backend-api/codex"
	ChatGPTClientID           = "app_EMoamEEZ73f0CkXaXp7hrann"
	ChatGPTOriginator         = "codex_cli_rs"
	ChatGPTClientVersion      = "0.144.1"
	ChatGPTUserAgent          = "codex_cli_rs/" + ChatGPTClientVersion + " (Unknown 0; unknown) unknown"
	devicePollDefaultInterval = 5 * time.Second

	// forceRefreshCoalesceWindow bounds how often ForceRefresh actually rotates
	// the refresh_token. When several in-flight requests are rejected at once
	// (a 401 "token_expired" storm), the first refresh wins and siblings within
	// this window reuse its result instead of each rotating the token again.
	forceRefreshCoalesceWindow = 10 * time.Second
)

// DeviceCodeChallenge is what gets returned from the device-code start endpoint.
// The end user is expected to open VerificationURI and enter UserCode; while
// they do that, we poll PollDeviceCode in the background.
type DeviceCodeChallenge struct {
	DeviceCode      string        `json:"device_code"`
	DeviceAuthID    string        `json:"device_auth_id"`
	UserCode        string        `json:"user_code"`
	VerificationURI string        `json:"verification_uri"`
	ExpiresIn       int           `json:"expires_in"`
	ExpiresAt       string        `json:"expires_at"`
	Interval        json.Number   `json:"interval"`
	IssuedAt        time.Time     `json:"-"`
	pollDelay       time.Duration `json:"-"`
}

// ChatGPTOAuth holds the device-code OAuth state for a ChatGPT subscription.
// Tokens are persisted to a JSON file under tokenDir; concurrent Apply calls
// share refresh state via mu.
//
// Concurrency: the hot path takes only a shared read-lock so concurrent
// inference calls don't serialize when the cached token is still fresh.
// Refresh / disk I/O / cold-path lookups acquire the exclusive write-lock.
type ChatGPTOAuth struct {
	tokenDir   string
	httpClient *http.Client

	// Endpoint URLs are overridable to make tests hermetic.
	deviceCodeURL  string
	deviceTokenURL string
	oauthTokenURL  string

	mu               sync.RWMutex
	cached           string
	cachedExp        int64 // unix milliseconds; 0 = not cached / unknown expiry
	cachedAccount    string
	lastForceRefresh time.Time // when ForceRefresh last rotated the token (coalescing)
}

// NewChatGPTOAuth constructs a ChatGPTOAuth using the supplied directory for
// auth.json storage. If httpClient is nil a default with 30s timeout is used.
func NewChatGPTOAuth(tokenDir string, httpClient *http.Client) *ChatGPTOAuth {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if tokenDir == "" {
		// Resolve to the default profile so a flagless `chatgpt-login` and a
		// flagless serve read/write the same path. tokenDirForProfile keeps
		// returning the legacy <base>/auth.json while it exists, so existing
		// single-store installs are not stranded.
		tokenDir = tokenDirForProfile(chatgptProfileEnvVar, chatgptProfileSubdir, DefaultProfile)
	}
	return &ChatGPTOAuth{
		tokenDir:       tokenDir,
		httpClient:     httpClient,
		deviceCodeURL:  ChatGPTDeviceCodeURL,
		deviceTokenURL: ChatGPTDeviceTokenURL,
		oauthTokenURL:  ChatGPTOAuthTokenURL,
	}
}

// OverrideURLs lets tests point at a fake auth server.
func (a *ChatGPTOAuth) OverrideURLs(deviceCode, deviceToken, oauthToken string) {
	a.deviceCodeURL = deviceCode
	a.deviceTokenURL = deviceToken
	a.oauthTokenURL = oauthToken
}

func (a *ChatGPTOAuth) Mode() string { return agentmodel.AuthModeSubscription }

func (a *ChatGPTOAuth) Apply(ctx context.Context, req *http.Request) error {
	token, accountID, err := a.getAccessTokenAndAccount(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", ChatGPTUserAgent)
	req.Header.Set("Originator", ChatGPTOriginator)
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", accountID)
	}
	return nil
}

// getAccessTokenAndAccount returns a usable access token plus the account ID,
// refreshing if expired. If no refresh-able token is available, returns
// ErrLoginRequired.
//
// Hot path takes only a shared read-lock so concurrent Apply calls don't
// serialize. Cold path / refresh promotes to the exclusive write-lock and
// re-checks state in case a sibling goroutine just finished a refresh.
func (a *ChatGPTOAuth) getAccessTokenAndAccount(ctx context.Context) (token, accountID string, err error) {
	a.mu.RLock()
	if a.cached != "" && !a.cacheExpired() {
		token, accountID = a.cached, a.cachedAccount
		a.mu.RUnlock()
		return token, accountID, nil
	}
	a.mu.RUnlock()

	a.mu.Lock()
	defer a.mu.Unlock()

	// Re-check under exclusive lock: another goroutine may have refreshed
	// between RUnlock and Lock.
	if a.cached != "" && !a.cacheExpired() {
		return a.cached, a.cachedAccount, nil
	}

	// Cold path / expired: re-read disk (catches token written by a concurrent
	// `agentmodel chatgpt-login`).
	auth, _ := readCreds(a.tokenDir)
	if auth != nil && auth.AccessToken != "" && !a.tokenExpired(auth) {
		a.populateCache(auth)
		return auth.AccessToken, auth.AccountID, nil
	}
	if auth != nil && auth.RefreshToken != "" {
		refreshed, refreshErr := a.refresh(ctx, auth)
		if refreshErr == nil {
			a.populateCache(refreshed)
			return refreshed.AccessToken, refreshed.AccountID, nil
		}
		// fall through: refresh failed, treat as login required
	}
	return "", "", ErrLoginRequired
}

func (a *ChatGPTOAuth) populateCache(auth *oauthCreds) {
	a.cached = auth.AccessToken
	a.cachedExp = auth.ExpiresAtMS
	a.cachedAccount = auth.AccountID
}

func (a *ChatGPTOAuth) cacheExpired() bool { return expiredMS(a.cachedExp) }

// ForceRefresh discards any cached token and refreshes against the OAuth
// endpoint using the on-disk refresh_token, regardless of the locally recorded
// expiry. It exists for reactive recovery: the upstream can reject an access
// token our local expiry still considered valid — e.g. the access_token JWT
// expired before our recorded expires_at, or expires_at was absent (a
// Codex-derived auth.json stores expiry inside the JWT, so tokenExpired never
// fired and we never proactively refreshed).
//
// Returns ErrLoginRequired when there is no refresh_token to refresh with.
// Concurrent callers are coalesced via forceRefreshCoalesceWindow so a 401
// storm rotates the refresh_token at most once.
func (a *ChatGPTOAuth) ForceRefresh(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	// A sibling goroutine may have just refreshed in response to the same 401
	// storm; reuse its result rather than rotating the refresh_token again. A
	// zero lastForceRefresh (never refreshed) is far in the past, so the window
	// check alone correctly falls through to a real refresh.
	if a.cached != "" && time.Since(a.lastForceRefresh) < forceRefreshCoalesceWindow {
		return nil
	}

	auth, _ := readCreds(a.tokenDir)
	if auth == nil || auth.RefreshToken == "" {
		return ErrLoginRequired
	}
	refreshed, err := a.refresh(ctx, auth)
	if err != nil {
		return err
	}
	a.populateCache(refreshed)
	a.lastForceRefresh = time.Now()
	return nil
}

// LoginDeviceCode initiates the device-code OAuth flow. Returns the challenge
// (user_code, verification_uri, etc) that the caller should display to the
// user. The caller then invokes PollDeviceCode to wait for completion.
func (a *ChatGPTOAuth) LoginDeviceCode(ctx context.Context) (*DeviceCodeChallenge, error) {
	if err := a.ensureTokenDir(); err != nil {
		return nil, err
	}

	payload, err := json.Marshal(map[string]string{"client_id": ChatGPTClientID})
	if err != nil {
		return nil, fmt.Errorf("device code: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", a.deviceCodeURL, strings.NewReader(string(payload)))
	if err != nil {
		return nil, fmt.Errorf("device code: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", ChatGPTUserAgent)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device code: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("device code: %s: %s", resp.Status, string(body))
	}

	var c DeviceCodeChallenge
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("device code: decode: %w", err)
	}
	if c.DeviceCode == "" {
		c.DeviceCode = c.DeviceAuthID
	}
	if c.VerificationURI == "" {
		c.VerificationURI = ChatGPTDeviceVerifyURL
	}
	if c.ExpiresIn == 0 && c.ExpiresAt != "" {
		if expiresAt, err := time.Parse(time.RFC3339Nano, c.ExpiresAt); err == nil {
			c.ExpiresIn = int(time.Until(expiresAt).Seconds())
		}
	}
	c.IssuedAt = time.Now()
	c.pollDelay = devicePollDefaultInterval
	if interval, err := c.Interval.Int64(); err == nil && interval > 0 {
		c.pollDelay = time.Duration(interval) * time.Second
	}
	return &c, nil
}

// PollDeviceCode polls the device-token endpoint until the user has approved
// the device code (or expiration). On success the resulting tokens are
// persisted and Apply will start using them.
func (a *ChatGPTOAuth) PollDeviceCode(ctx context.Context, challenge DeviceCodeChallenge) error {
	deviceAuthID := challenge.DeviceAuthID
	if deviceAuthID == "" {
		deviceAuthID = challenge.DeviceCode
	}
	userCode := challenge.UserCode
	if userCode == "" {
		userCode = challenge.DeviceCode
	}
	pollDelay := challenge.pollDelay
	if pollDelay <= 0 {
		if interval, err := challenge.Interval.Int64(); err == nil && interval > 0 {
			pollDelay = time.Duration(interval) * time.Second
		} else {
			pollDelay = devicePollDefaultInterval
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	deadline := time.Now().Add(15 * time.Minute)
	for {
		if time.Now().After(deadline) {
			return errors.New("device code: expired (15 min)")
		}

		payload, err := json.Marshal(map[string]string{
			"client_id":      ChatGPTClientID,
			"device_auth_id": deviceAuthID,
			"user_code":      userCode,
		})
		if err != nil {
			return fmt.Errorf("device code: encode poll request: %w", err)
		}

		req, err := http.NewRequestWithContext(ctx, "POST", a.deviceTokenURL, strings.NewReader(string(payload)))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", ChatGPTUserAgent)

		resp, err := a.httpClient.Do(req)
		if err != nil {
			return err
		}

		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode/100 == 2 {
			var data struct {
				AccessToken       string `json:"access_token"`
				RefreshToken      string `json:"refresh_token"`
				IDToken           string `json:"id_token"`
				ExpiresIn         int    `json:"expires_in"`
				ExpiresAt         int64  `json:"expires_at"`
				AuthorizationCode string `json:"authorization_code"`
				CodeVerifier      string `json:"code_verifier"`
				Token             struct {
					AccessToken  string `json:"access_token"`
					RefreshToken string `json:"refresh_token"`
					IDToken      string `json:"id_token"`
					ExpiresIn    int    `json:"expires_in"`
					ExpiresAt    int64  `json:"expires_at"`
				} `json:"token"`
			}
			if err := json.Unmarshal(body, &data); err != nil {
				return fmt.Errorf("decode token: %w", err)
			}
			if data.AccessToken == "" {
				data.AccessToken = data.Token.AccessToken
			}
			if data.RefreshToken == "" {
				data.RefreshToken = data.Token.RefreshToken
			}
			if data.IDToken == "" {
				data.IDToken = data.Token.IDToken
			}
			if data.ExpiresIn == 0 {
				data.ExpiresIn = data.Token.ExpiresIn
			}
			if data.ExpiresAt == 0 {
				data.ExpiresAt = data.Token.ExpiresAt
			}
			if data.AccessToken == "" && data.AuthorizationCode != "" {
				exchanged, err := a.exchangeDeviceAuthorizationCode(ctx, data.AuthorizationCode, data.CodeVerifier)
				if err != nil {
					return err
				}
				data.AccessToken = exchanged.AccessToken
				data.RefreshToken = exchanged.RefreshToken
				data.IDToken = exchanged.IDToken
				data.ExpiresIn = exchanged.ExpiresIn
			}
			if data.AccessToken == "" || data.RefreshToken == "" {
				return fmt.Errorf("device code: token response missing access_token or refresh_token: %s", redactSensitiveJSON(body))
			}
			// The device-token response reports expiry as unix seconds
			// (expires_at) or a TTL (expires_in); agent-model stores unix ms.
			var expiresAtMS int64
			switch {
			case data.ExpiresAt > 0:
				expiresAtMS = data.ExpiresAt * 1000
			case data.ExpiresIn > 0:
				expiresAtMS = time.Now().Add(time.Duration(data.ExpiresIn) * time.Second).UnixMilli()
			}
			auth := &oauthCreds{
				Provider:     providerChatGPT,
				AccessToken:  data.AccessToken,
				RefreshToken: data.RefreshToken,
				IDToken:      data.IDToken,
				ExpiresAtMS:  expiresAtMS,
			}
			return writeCreds(a.tokenDir, auth)
		}

		// authorization_pending → keep polling; any other error is fatal.
		var errResp struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		var wrappedErrResp struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &errResp)
		_ = json.Unmarshal(body, &wrappedErrResp)
		code := errResp.Error
		if code == "" {
			code = errResp.Code
		}
		if code == "" {
			code = wrappedErrResp.Error.Code
		}
		if code != "authorization_pending" && code != "slow_down" && code != "deviceauth_authorization_pending" {
			return fmt.Errorf("device code: %s: %s", resp.Status, string(body))
		}
		if code == "slow_down" {
			pollDelay += devicePollDefaultInterval
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollDelay):
		}
	}
}

type chatgptTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int    `json:"expires_in"`
}

func (a *ChatGPTOAuth) exchangeDeviceAuthorizationCode(ctx context.Context, authorizationCode, codeVerifier string) (*chatgptTokenResponse, error) {
	if authorizationCode == "" || codeVerifier == "" {
		return nil, fmt.Errorf("device code: authorization_code response missing code_verifier")
	}
	form := url.Values{}
	form.Set("client_id", ChatGPTClientID)
	form.Set("grant_type", "authorization_code")
	form.Set("code", authorizationCode)
	form.Set("redirect_uri", strings.TrimRight(ChatGPTAuthBase, "/")+"/deviceauth/callback")
	form.Set("code_verifier", codeVerifier)

	req, err := http.NewRequestWithContext(ctx, "POST", a.oauthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", ChatGPTUserAgent)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device code authorization_code exchange: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("device code authorization_code exchange: %s: %s", resp.Status, string(body))
	}
	var data chatgptTokenResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("device code authorization_code exchange decode: %w", err)
	}
	if data.AccessToken == "" || data.RefreshToken == "" {
		return nil, fmt.Errorf("device code authorization_code exchange missing access_token or refresh_token: %s", redactSensitiveJSON(body))
	}
	return &data, nil
}

func (a *ChatGPTOAuth) refresh(ctx context.Context, current *oauthCreds) (*oauthCreds, error) {
	form := url.Values{}
	form.Set("client_id", ChatGPTClientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", current.RefreshToken)

	req, err := http.NewRequestWithContext(ctx, "POST", a.oauthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("refresh: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("refresh: %s: %s", resp.Status, string(body))
	}

	var data struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("refresh: decode: %w", err)
	}

	updated := &oauthCreds{
		Provider:     providerChatGPT,
		AccessToken:  data.AccessToken,
		RefreshToken: data.RefreshToken,
		IDToken:      data.IDToken,
		AccountID:    current.AccountID, // preserve account id
		ExpiresAtMS:  expiryFromExpiresIn(data.ExpiresIn),
	}
	if updated.RefreshToken == "" {
		// some refresh responses omit refresh_token; reuse current.
		updated.RefreshToken = current.RefreshToken
	}
	if err := writeCreds(a.tokenDir, updated); err != nil {
		return nil, err
	}
	return updated, nil
}

func (a *ChatGPTOAuth) tokenExpired(auth *oauthCreds) bool { return expiredMS(auth.ExpiresAtMS) }

func (a *ChatGPTOAuth) ensureTokenDir() error {
	return os.MkdirAll(a.tokenDir, 0o700)
}

func redactSensitiveJSON(body []byte) string {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return "<invalid json>"
	}
	redactSensitive(v)
	b, err := json.Marshal(v)
	if err != nil {
		return "<unmarshalable json>"
	}
	return string(b)
}

func redactSensitive(v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if isSensitiveKey(k) {
				x[k] = "<redacted>"
				continue
			}
			redactSensitive(val)
		}
	case []any:
		for _, item := range x {
			redactSensitive(item)
		}
	}
}

func isSensitiveKey(k string) bool {
	lk := strings.ToLower(k)
	return strings.Contains(lk, "token") ||
		strings.Contains(lk, "secret") ||
		strings.Contains(lk, "authorization_code") ||
		strings.Contains(lk, "code_verifier") ||
		strings.Contains(lk, "code_challenge")
}
