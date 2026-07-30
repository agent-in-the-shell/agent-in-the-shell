package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/api"
)

// accountResp mirrors the JSON wire shape the agentshell claudeLimitsProbe
// decodes (BackendLimits). Re-declared here to assert the contract.
type accountResp struct {
	Status string `json:"status"`
	Auth   *struct {
		Mode       string `json:"mode"`
		ModeSource string `json:"mode_source"`
	} `json:"auth"`
	Windows []struct {
		LimitName      string   `json:"limit_name"`
		Unit           string   `json:"unit"`
		UsedPercent    *float64 `json:"used_percent"`
		WindowStatus   string   `json:"window_status"`
		Source         string   `json:"source"`
		ResetInSeconds *int64   `json:"reset_in_seconds"`
	} `json:"windows"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// seedNativeAuth writes a fresh (far-future expiry) native-format auth.json so
// the refreshable returns the disk access token without a refresh round-trip.
func seedNativeAuth(t *testing.T, dir, access string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"schema":"agentmodel.oauth/v1","provider":"anthropic","access_token":"` + access +
		`","refresh_token":"sk-ant-ort","expires_at_ms":4102444800000}`
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func getAccountUsage(t *testing.T, ts *httptest.Server, token, profile string) *http.Response {
	t.Helper()
	u := ts.URL + "/v1/account/anthropic/usage"
	if profile != "" {
		u += "?profile=" + profile
	}
	req, err := http.NewRequestWithContext(context.Background(), "GET", u, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

func TestAnthropicUsage_RequiresAuth(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp := getAccountUsage(t, ts, "", "default")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", resp.StatusCode)
	}
}

func TestAnthropicUsage_Success(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	t.Setenv("AGENTMODEL_PROFILE", "")
	seedNativeAuth(t, filepath.Join(base, "work"), "sk-ant-oat-work")

	var gotBearer, gotBeta string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBearer = r.Header.Get("Authorization")
		gotBeta = r.Header.Get("anthropic-beta")
		w.Header().Set("Content-Type", "application/json")
		// Future reset dates: the handler stamps reset_in_seconds off real
		// time.Now(), so past dates would clamp to 0.
		_, _ = w.Write([]byte(`{
			"five_hour":{"utilization":35,"resets_at":"2099-05-21T15:00:00Z"},
			"seven_day":{"utilization":80.5,"resets_at":"2099-05-28T12:00:00Z"},
			"seven_day_sonnet":{"utilization":100,"resets_at":"2099-05-28T12:00:00Z"}
		}`))
	}))
	defer upstream.Close()

	ts, _ := newTestServer(t, nil, func(c *api.Config) { c.AnthropicUsageURL = upstream.URL })
	resp := getAccountUsage(t, ts, testToken, "work")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got accountResp
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}

	if gotBearer != "Bearer sk-ant-oat-work" {
		t.Fatalf("upstream bearer = %q, want the work profile's disk token", gotBearer)
	}
	if gotBeta == "" {
		t.Fatal("upstream did not receive the anthropic-beta header")
	}
	if got.Status != "limited" {
		t.Fatalf("status = %q, want limited", got.Status)
	}
	if got.Auth == nil || got.Auth.Mode != "subscription" || got.Auth.ModeSource != "agentmodel_oauth" {
		t.Fatalf("auth = %#v, want agentmodel subscription", got.Auth)
	}
	if len(got.Windows) != 3 {
		t.Fatalf("windows = %#v, want 3", got.Windows)
	}
	if got.Windows[0].LimitName != "claude_5h" || got.Windows[0].Source != "claude_oauth" {
		t.Fatalf("first window = %#v", got.Windows[0])
	}
	if got.Windows[2].LimitName != "claude_sonnet_weekly" || got.Windows[2].WindowStatus != "limited" {
		t.Fatalf("sonnet window = %#v, want limited", got.Windows[2])
	}
	// reset_in_seconds is derived server-side from the reset_at anchor.
	if got.Windows[0].ResetInSeconds == nil || *got.Windows[0].ResetInSeconds <= 0 {
		t.Fatalf("five_hour reset_in_seconds = %v, want > 0", got.Windows[0].ResetInSeconds)
	}
}

func TestAnthropicUsage_LoginRequired(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	t.Setenv("AGENTMODEL_PROFILE", "")
	// No auth.json seeded for this profile → refreshable reports login required.

	ts, _ := newTestServer(t, nil, func(c *api.Config) { c.AnthropicUsageURL = "http://127.0.0.1:0/unused" })
	resp := getAccountUsage(t, ts, testToken, "missing")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (structured error in body)", resp.StatusCode)
	}
	var got accountResp
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "unknown" {
		t.Fatalf("status = %q, want unknown", got.Status)
	}
	if got.Error == nil || got.Error.Code != "claude_oauth_login_required" {
		t.Fatalf("error = %#v, want claude_oauth_login_required", got.Error)
	}
}

func TestAnthropicUsage_RateLimited(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	t.Setenv("AGENTMODEL_PROFILE", "")
	seedNativeAuth(t, filepath.Join(base, "default"), "sk-ant-oat-default")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error"}}`))
	}))
	defer upstream.Close()

	ts, _ := newTestServer(t, nil, func(c *api.Config) { c.AnthropicUsageURL = upstream.URL })
	resp := getAccountUsage(t, ts, testToken, "") // default profile
	defer resp.Body.Close()
	var got accountResp
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "unknown" {
		t.Fatalf("status = %q, want unknown", got.Status)
	}
	if got.Error == nil || got.Error.Code != "claude_oauth_rate_limited" {
		t.Fatalf("error = %#v, want claude_oauth_rate_limited", got.Error)
	}
}

func TestAnthropicUsage_InvalidProfile(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp := getAccountUsage(t, ts, testToken, "Bad/Name")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var got accountResp
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Error == nil || got.Error.Code != "invalid_profile" {
		t.Fatalf("error = %#v, want invalid_profile", got.Error)
	}
}
