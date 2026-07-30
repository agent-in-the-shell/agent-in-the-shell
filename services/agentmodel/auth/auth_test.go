package auth_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
)

// ─── StaticKey ────────────────────────────────────────────────────────────

func TestStaticKey_AppliesAuthorizationHeader(t *testing.T) {
	a := &auth.StaticKey{HeaderName: "Authorization", Prefix: "Bearer ", Token: "sk-abc"}
	req, _ := http.NewRequest("POST", "https://example.com", nil)
	if err := a.Apply(context.Background(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-abc" {
		t.Errorf("Authorization: got %q, want %q", got, "Bearer sk-abc")
	}
	if a.Mode() != agentmodel.AuthModeAPIKey {
		t.Errorf("Mode: got %q, want %q", a.Mode(), agentmodel.AuthModeAPIKey)
	}
}

func TestStaticKey_AppliesAnthropicXAPIKeyHeader(t *testing.T) {
	a := &auth.StaticKey{HeaderName: "x-api-key", Prefix: "", Token: "sk-ant-api03-foo"}
	req, _ := http.NewRequest("POST", "https://api.anthropic.com", nil)
	if err := a.Apply(context.Background(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := req.Header.Get("x-api-key"); got != "sk-ant-api03-foo" {
		t.Errorf("x-api-key: got %q, want raw token", got)
	}
}

// ─── AnthropicOAuth ───────────────────────────────────────────────────────

func TestIsAnthropicOAuthKey(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"sk-ant-oat01-aaa", true},
		{"Bearer sk-ant-oat01-bbb", true},
		{"sk-ant-api03-zzz", false},
		{"", false},
		{"random", false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := auth.IsAnthropicOAuthKey(c.in); got != c.want {
				t.Errorf("IsAnthropicOAuthKey(%q): got %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestAnthropicOAuth_RewritesHeaders(t *testing.T) {
	a := &auth.AnthropicOAuth{Token: "sk-ant-oat01-foo"}
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	req.Header.Set("x-api-key", "should-be-removed")
	req.Header.Set("anthropic-beta", "computer-use-2024-10-22")

	if err := a.Apply(context.Background(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := req.Header.Get("x-api-key"); got != "" {
		t.Errorf("x-api-key should be removed, got %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-ant-oat01-foo" {
		t.Errorf("Authorization: got %q, want %q", got, "Bearer sk-ant-oat01-foo")
	}
	if got := req.Header.Get("anthropic-dangerous-direct-browser-access"); got != "true" {
		t.Errorf("anthropic-dangerous-direct-browser-access: got %q, want true", got)
	}
	beta := req.Header.Get("anthropic-beta")
	if !strings.Contains(beta, "computer-use-2024-10-22") {
		t.Errorf("anthropic-beta should preserve existing value, got %q", beta)
	}
	if !strings.Contains(beta, auth.AnthropicOAuthBetaHeader) {
		t.Errorf("anthropic-beta should include OAuth beta %q, got %q", auth.AnthropicOAuthBetaHeader, beta)
	}

	if a.Mode() != agentmodel.AuthModeSubscription {
		t.Errorf("Mode: got %q, want %q", a.Mode(), agentmodel.AuthModeSubscription)
	}
}

func TestAnthropicOAuth_NewFromBearerToken(t *testing.T) {
	a := auth.NewAnthropicOAuth("Bearer sk-ant-oat01-foo")
	if a.Token != "sk-ant-oat01-foo" {
		t.Errorf("token: got %q, want stripped of Bearer prefix", a.Token)
	}
}

// ─── ChatGPTOAuth (device-code flow) ──────────────────────────────────────

func TestChatGPTOAuth_LoadsExistingToken(t *testing.T) {
	dir := t.TempDir()
	authData := map[string]any{
		"access_token":  "valid-token",
		"refresh_token": "rt",
		"expires_at_ms": time.Now().Add(time.Hour).UnixMilli(),
	}
	writeAuthFile(t, dir, authData)

	a := auth.NewChatGPTOAuth(dir, &http.Client{Timeout: 5 * time.Second})

	req, _ := http.NewRequest("POST", "https://chatgpt.com/backend-api/codex/responses", nil)
	if err := a.Apply(context.Background(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer valid-token" {
		t.Errorf("Authorization: got %q, want %q", got, "Bearer valid-token")
	}
	if got := req.Header.Get("Originator"); got != "codex_cli_rs" {
		t.Errorf("Originator: got %q, want codex_cli_rs", got)
	}
	if got := req.Header.Get("User-Agent"); !strings.HasPrefix(got, "codex_cli_rs/") {
		t.Errorf("User-Agent: got %q, want prefix codex_cli_rs/", got)
	}
}

func TestChatGPTOAuth_RefreshesExpiredToken(t *testing.T) {
	var refreshCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			atomic.AddInt32(&refreshCount, 1)
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "grant_type=refresh_token") {
				t.Errorf("refresh body missing grant_type=refresh_token: %s", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "new-access-token",
				"refresh_token": "new-refresh-token",
				"id_token":      "new-id-token",
				"expires_in":    3600,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	expiredAuth := map[string]any{
		"access_token":  "expired-token",
		"refresh_token": "old-refresh-token",
		"expires_at_ms": time.Now().Add(-1 * time.Hour).UnixMilli(), // expired
	}
	writeAuthFile(t, dir, expiredAuth)

	a := auth.NewChatGPTOAuth(dir, &http.Client{Timeout: 5 * time.Second})
	a.OverrideURLs(server.URL+"/devicecode", server.URL+"/devicetoken", server.URL+"/oauth/token")

	req, _ := http.NewRequest("POST", "https://chatgpt.com/backend-api/codex/responses", nil)
	if err := a.Apply(context.Background(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := atomic.LoadInt32(&refreshCount); got != 1 {
		t.Errorf("refreshCount: got %d, want 1", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer new-access-token" {
		t.Errorf("Authorization: got %q, want Bearer new-access-token", got)
	}

	// Verify auth file was updated.
	stored := readAuthFile(t, dir)
	if stored["access_token"] != "new-access-token" {
		t.Errorf("stored access_token: got %v, want new-access-token", stored["access_token"])
	}
}

func TestChatGPTOAuth_LoginRequiredWhenNoToken(t *testing.T) {
	dir := t.TempDir()
	a := auth.NewChatGPTOAuth(dir, &http.Client{Timeout: 5 * time.Second})

	req, _ := http.NewRequest("POST", "https://chatgpt.com/backend-api/codex/responses", nil)
	err := a.Apply(context.Background(), req)
	if err == nil {
		t.Fatal("expected ErrLoginRequired, got nil")
	}
	if !auth.IsLoginRequired(err) {
		t.Errorf("expected ErrLoginRequired, got %v", err)
	}
}

func TestChatGPTOAuth_DeviceCodeFlow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/devicecode":
			if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("devicecode Content-Type = %q, want application/json", ct)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_auth_id": "dc-abc",
				"user_code":      "ABCD-EFGH",
				"expires_at":     time.Now().Add(15 * time.Minute).Format(time.RFC3339Nano),
				"interval":       1,
			})
		case "/devicetoken":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("token body decode: %v", err)
				http.Error(w, "bad request", 400)
				return
			}
			if body["device_auth_id"] != "dc-abc" || body["user_code"] != "ABCD-EFGH" {
				t.Errorf("token body = %#v, want device_auth_id/user_code", body)
				http.Error(w, "bad request", 400)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "fresh-token",
				"refresh_token": "fresh-refresh",
				"id_token":      "fresh-id",
				"expires_in":    3600,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	a := auth.NewChatGPTOAuth(dir, &http.Client{Timeout: 5 * time.Second})
	a.OverrideURLs(server.URL+"/devicecode", server.URL+"/devicetoken", server.URL+"/oauth/token")

	challenge, err := a.LoginDeviceCode(context.Background())
	if err != nil {
		t.Fatalf("LoginDeviceCode: %v", err)
	}
	if challenge.UserCode != "ABCD-EFGH" {
		t.Errorf("UserCode: got %q, want ABCD-EFGH", challenge.UserCode)
	}
	if challenge.VerificationURI == "" {
		t.Error("VerificationURI: got empty, want non-empty")
	}

	if err := a.PollDeviceCode(context.Background(), *challenge); err != nil {
		t.Fatalf("PollDeviceCode: %v", err)
	}

	stored := readAuthFile(t, dir)
	if stored["access_token"] != "fresh-token" {
		t.Errorf("stored access_token: got %v, want fresh-token", stored["access_token"])
	}
}

func TestChatGPTOAuth_DeviceCodeFlowRespectsIntervalAndSlowDown(t *testing.T) {
	var attempts int
	var requestTimes []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/devicecode":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_auth_id": "dc-slow",
				"user_code":      "SLOW-DOWN",
				"expires_at":     time.Now().Add(15 * time.Minute).Format(time.RFC3339Nano),
				"interval":       "1",
			})
		case "/devicetoken":
			requestTimes = append(requestTimes, time.Now())
			attempts++
			if attempts == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "slow_down"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "slow-token",
				"refresh_token": "slow-refresh",
				"expires_in":    3600,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	a := auth.NewChatGPTOAuth(dir, &http.Client{Timeout: 5 * time.Second})
	a.OverrideURLs(server.URL+"/devicecode", server.URL+"/devicetoken", server.URL+"/oauth/token")

	challenge, err := a.LoginDeviceCode(context.Background())
	if err != nil {
		t.Fatalf("LoginDeviceCode: %v", err)
	}
	start := time.Now()
	if err := a.PollDeviceCode(context.Background(), *challenge); err != nil {
		t.Fatalf("PollDeviceCode: %v", err)
	}
	if len(requestTimes) != 2 {
		t.Fatalf("token requests = %d, want 2", len(requestTimes))
	}
	if elapsed := requestTimes[1].Sub(requestTimes[0]); elapsed < 2*time.Second {
		t.Fatalf("slow_down poll delay = %v, want at least 2s", elapsed)
	}
	if elapsed := time.Since(start); elapsed < 2*time.Second {
		t.Fatalf("total poll elapsed = %v, want at least 2s", elapsed)
	}
}

func TestChatGPTOAuth_DeviceCodeFlowRejectsEmptyTokenResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/devicecode":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_auth_id": "dc-empty",
				"user_code":      "ABCD-EFGH",
				"expires_at":     time.Now().Add(15 * time.Minute).Format(time.RFC3339Nano),
				"interval":       "1",
			})
		case "/devicetoken":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"expires_at": time.Now().Add(time.Hour).Unix(),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	a := auth.NewChatGPTOAuth(dir, &http.Client{Timeout: 5 * time.Second})
	a.OverrideURLs(server.URL+"/devicecode", server.URL+"/devicetoken", server.URL+"/oauth/token")

	challenge, err := a.LoginDeviceCode(context.Background())
	if err != nil {
		t.Fatalf("LoginDeviceCode: %v", err)
	}
	err = a.PollDeviceCode(context.Background(), *challenge)
	if err == nil || !strings.Contains(err.Error(), "missing access_token or refresh_token") {
		t.Fatalf("PollDeviceCode error = %v, want missing token error", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "auth.json")); !os.IsNotExist(statErr) {
		t.Fatalf("auth.json stat error = %v, want not exist", statErr)
	}
}

func TestChatGPTOAuth_ConcurrentRefreshSerializes(t *testing.T) {
	var refreshCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			atomic.AddInt32(&refreshCount, 1)
			time.Sleep(50 * time.Millisecond) // simulate slow refresh
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "shared-new-token",
				"refresh_token": "shared-new-refresh",
				"expires_in":    3600,
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	dir := t.TempDir()
	writeAuthFile(t, dir, map[string]any{
		"access_token":  "expired",
		"refresh_token": "rt",
		"expires_at_ms": time.Now().Add(-time.Hour).UnixMilli(),
	})

	a := auth.NewChatGPTOAuth(dir, &http.Client{Timeout: 5 * time.Second})
	a.OverrideURLs(server.URL+"/devicecode", server.URL+"/devicetoken", server.URL+"/oauth/token")

	const N = 10
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("POST", "https://example.com", nil)
			if err := a.Apply(context.Background(), req); err != nil {
				t.Errorf("Apply: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&refreshCount); got != 1 {
		t.Errorf("refresh should be called once across %d concurrent Apply calls, got %d", N, got)
	}
}

// ─── helpers ───────────────────────────────────────────────────────────────

func writeAuthFile(t *testing.T, dir string, data map[string]any) {
	t.Helper()
	path := filepath.Join(dir, "auth.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(data); err != nil {
		t.Fatalf("encode: %v", err)
	}
}

func readAuthFile(t *testing.T, dir string) map[string]any {
	t.Helper()
	path := filepath.Join(dir, "auth.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

// silence unused import warnings on platforms where some funcs are no-ops
var _ = fmt.Sprintf

// ─── MergeCSV ────────────────────────────────────────────────────────────────

func TestMergeCSV_basic(t *testing.T) {
	if got := auth.MergeCSV("a,b", "b,c"); got != "a,b,c" {
		t.Errorf("got %q, want %q", got, "a,b,c")
	}
}

func TestMergeCSV_multiValueNew(t *testing.T) {
	if got := auth.MergeCSV("a", "b,c"); got != "a,b,c" {
		t.Errorf("got %q, want %q", got, "a,b,c")
	}
}

func TestMergeCSV_substringNotDuplicate(t *testing.T) {
	// "cache-control" is a substring of "cache-control-5min" but is a distinct item
	if got := auth.MergeCSV("cache-control-5min", "cache-control"); got != "cache-control-5min,cache-control" {
		t.Errorf("got %q, want %q", got, "cache-control-5min,cache-control")
	}
}

func TestMergeCSV_emptyExisting(t *testing.T) {
	if got := auth.MergeCSV("", "a,b"); got != "a,b" {
		t.Errorf("got %q, want %q", got, "a,b")
	}
}

func TestMergeCSV_emptyNew(t *testing.T) {
	if got := auth.MergeCSV("a,b", ""); got != "a,b" {
		t.Errorf("got %q, want %q", got, "a,b")
	}
}

func TestMergeCSV_allDuplicates(t *testing.T) {
	if got := auth.MergeCSV("a,b", "a,b"); got != "a,b" {
		t.Errorf("got %q, want %q", got, "a,b")
	}
}

// ─── ChatGPTOAuth.ForceRefresh ───────────────────────────────────────────────

func TestChatGPTOAuth_ForceRefresh(t *testing.T) {
	var refreshCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&refreshCount, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "refreshed-token",
			"refresh_token": "new-rt",
			"expires_in":    3600,
		})
	}))
	defer server.Close()

	// A token that is still fresh by local expiry: ForceRefresh must rotate it
	// anyway, since it exists for reactive recovery when the upstream — not the
	// local clock — rejects the token.
	dir := t.TempDir()
	writeAuthFile(t, dir, map[string]any{
		"access_token":  "still-fresh-locally",
		"refresh_token": "old-rt",
		"expires_at_ms": time.Now().Add(time.Hour).UnixMilli(),
	})
	a := auth.NewChatGPTOAuth(dir, &http.Client{Timeout: 5 * time.Second})
	a.OverrideURLs(server.URL+"/devicecode", server.URL+"/devicetoken", server.URL+"/oauth/token")

	if err := a.ForceRefresh(context.Background()); err != nil {
		t.Fatalf("ForceRefresh: %v", err)
	}

	// The rotated token is applied on the next request and persisted to disk.
	req, _ := http.NewRequest("POST", "https://example.com", nil)
	if err := a.Apply(context.Background(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer refreshed-token" {
		t.Errorf("Authorization = %q, want Bearer refreshed-token", got)
	}
	if got := readAuthFile(t, dir)["refresh_token"]; got != "new-rt" {
		t.Errorf("persisted refresh_token = %v, want new-rt", got)
	}

	// A second ForceRefresh inside the coalesce window reuses the result rather
	// than rotating the refresh_token again.
	if err := a.ForceRefresh(context.Background()); err != nil {
		t.Fatalf("ForceRefresh (coalesced): %v", err)
	}
	if got := atomic.LoadInt32(&refreshCount); got != 1 {
		t.Errorf("refresh server hits = %d, want 1 (second call coalesced)", got)
	}
}

func TestChatGPTOAuth_ForceRefreshNoRefreshToken(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, dir, map[string]any{
		"access_token":  "only-access",
		"expires_at_ms": time.Now().Add(time.Hour).UnixMilli(),
	})
	a := auth.NewChatGPTOAuth(dir, &http.Client{Timeout: 5 * time.Second})
	if err := a.ForceRefresh(context.Background()); err != auth.ErrLoginRequired {
		t.Fatalf("ForceRefresh err = %v, want ErrLoginRequired", err)
	}
}
