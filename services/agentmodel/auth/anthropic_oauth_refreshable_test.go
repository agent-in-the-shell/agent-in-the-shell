package auth_test

import (
	"context"
	"encoding/json"
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

// writeNativeAuth creates an agent-model native auth.json for the anthropic
// provider in dir, reusing the shared writeAuthFile helper.
func writeNativeAuth(t *testing.T, dir string, access, refresh string, expiresMs int64) {
	t.Helper()
	writeAuthFile(t, dir, map[string]any{
		"schema":        "agentmodel.oauth/v1",
		"provider":      "anthropic",
		"access_token":  access,
		"refresh_token": refresh,
		"expires_at_ms": expiresMs,
	})
}

// ─── Hot-path: fresh cache returns without disk / HTTP ───────────────────

func TestAnthropicOAuthRefreshable_FreshTokenNoRefresh(t *testing.T) {
	dir := t.TempDir()
	writeNativeAuth(t, dir, "sk-ant-oat-fresh", "sk-ant-ort-r1",
		time.Now().Add(time.Hour).UnixMilli())

	var refreshCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&refreshCalls, 1)
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer server.Close()

	a := auth.NewAnthropicOAuthRefreshable(dir, &http.Client{Timeout: 5 * time.Second})
	a.SetEndpointsForTest(server.URL+"/oauth/token", "test-client")

	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	if err := a.Apply(context.Background(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := req.Header.Get("Authorization"); got != "Bearer sk-ant-oat-fresh" {
		t.Errorf("Authorization: got %q, want Bearer sk-ant-oat-fresh", got)
	}
	if !strings.Contains(req.Header.Get("anthropic-beta"), "oauth-2025-04-20") {
		t.Errorf("anthropic-beta missing oauth value: %q", req.Header.Get("anthropic-beta"))
	}
	if req.Header.Get("x-api-key") != "" {
		t.Errorf("x-api-key should not be set: %q", req.Header.Get("x-api-key"))
	}
	if got := atomic.LoadInt32(&refreshCalls); got != 0 {
		t.Errorf("fresh token must not trigger refresh; got %d HTTP calls", got)
	}
}

// ─── Cold path: expired access triggers refresh, persists, surfaces token ─

func TestAnthropicOAuthRefreshable_ExpiredAccessTriggersRefresh(t *testing.T) {
	dir := t.TempDir()
	expiredMs := time.Now().Add(-time.Hour).UnixMilli()
	writeNativeAuth(t, dir, "old-access", "old-refresh", expiredMs)

	var bodyReceived []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyReceived, _ = io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access",
			"refresh_token": "new-refresh",
			"expires_in":    3600,
		})
	}))
	defer server.Close()

	a := auth.NewAnthropicOAuthRefreshable(dir, &http.Client{Timeout: 5 * time.Second})
	a.SetEndpointsForTest(server.URL+"/oauth/token", "client-X")

	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	if err := a.Apply(context.Background(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := req.Header.Get("Authorization"); got != "Bearer new-access" {
		t.Errorf("Authorization: got %q, want Bearer new-access", got)
	}

	// Refresh request body should follow RFC 6749 + Anthropic conventions.
	var parsed map[string]string
	_ = json.Unmarshal(bodyReceived, &parsed)
	if parsed["grant_type"] != "refresh_token" {
		t.Errorf("grant_type: got %q, want refresh_token", parsed["grant_type"])
	}
	if parsed["refresh_token"] != "old-refresh" {
		t.Errorf("refresh_token: got %q, want old-refresh", parsed["refresh_token"])
	}
	if parsed["client_id"] != "client-X" {
		t.Errorf("client_id: got %q, want client-X", parsed["client_id"])
	}

	// auth.json should now hold the refreshed values.
	stored := readAuthFile(t, dir)
	if stored["access_token"] != "new-access" {
		t.Errorf("stored access_token: got %v, want new-access", stored["access_token"])
	}
	if stored["refresh_token"] != "new-refresh" {
		t.Errorf("stored refresh_token: got %v, want new-refresh", stored["refresh_token"])
	}
	expires, _ := stored["expires_at_ms"].(float64)
	if int64(expires) <= time.Now().UnixMilli() {
		t.Errorf("stored expires_at_ms must be in the future, got %v", stored["expires_at_ms"])
	}
}

// ─── ForceRefresh (reactive 401 recovery) ─────────────────────────────────

func TestAnthropicOAuthRefreshable_ForceRefresh(t *testing.T) {
	dir := t.TempDir()
	// A token that is still fresh by local expiry: ForceRefresh must rotate it
	// anyway, since it exists for reactive recovery when the upstream — not the
	// local clock — rejects the token.
	writeNativeAuth(t, dir, "old-access", "old-refresh", time.Now().Add(time.Hour).UnixMilli())

	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access",
			"refresh_token": "new-refresh",
			"expires_in":    3600,
		})
	}))
	defer server.Close()

	a := auth.NewAnthropicOAuthRefreshable(dir, &http.Client{Timeout: 5 * time.Second})
	a.SetEndpointsForTest(server.URL+"/oauth/token", "client")

	if err := a.ForceRefresh(context.Background()); err != nil {
		t.Fatalf("ForceRefresh: %v", err)
	}
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	if err := a.Apply(context.Background(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer new-access" {
		t.Errorf("Authorization: got %q, want Bearer new-access", got)
	}
	stored := readAuthFile(t, dir)
	if stored["refresh_token"] != "new-refresh" {
		t.Errorf("stored refresh_token: got %v, want new-refresh", stored["refresh_token"])
	}

	// A second ForceRefresh inside the coalesce window reuses the result rather
	// than rotating the refresh_token again.
	if err := a.ForceRefresh(context.Background()); err != nil {
		t.Fatalf("ForceRefresh (coalesced): %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("refresh server hits = %d, want 1 (second call coalesced)", got)
	}
}

func TestAnthropicOAuthRefreshable_ForceRefreshNoRefreshToken(t *testing.T) {
	dir := t.TempDir()
	writeNativeAuth(t, dir, "only-access", "", time.Now().Add(time.Hour).UnixMilli())
	a := auth.NewAnthropicOAuthRefreshable(dir, &http.Client{Timeout: 5 * time.Second})
	if err := a.ForceRefresh(context.Background()); err != auth.ErrLoginRequired {
		t.Fatalf("ForceRefresh err = %v, want ErrLoginRequired", err)
	}
}

// ─── Login required when no refresh token on disk ─────────────────────────

func TestAnthropicOAuthRefreshable_LoginRequiredWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	a := auth.NewAnthropicOAuthRefreshable(dir, &http.Client{Timeout: 5 * time.Second})

	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	err := a.Apply(context.Background(), req)
	if !auth.IsLoginRequired(err) {
		t.Errorf("got %v, want ErrLoginRequired", err)
	}
}

func TestAnthropicOAuthRefreshable_LoginRequiredWhenRefreshFails(t *testing.T) {
	dir := t.TempDir()
	expiredMs := time.Now().Add(-time.Hour).UnixMilli()
	writeNativeAuth(t, dir, "expired", "bad-refresh", expiredMs)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	a := auth.NewAnthropicOAuthRefreshable(dir, &http.Client{Timeout: 5 * time.Second})
	a.SetEndpointsForTest(server.URL+"/oauth/token", "")

	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	err := a.Apply(context.Background(), req)
	if err == nil {
		t.Fatal("expected error when refresh fails, got nil")
	}
	// We surface the upstream error verbatim rather than wrapping in
	// ErrLoginRequired so callers can see "invalid_grant" vs network failure.
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("error should propagate upstream message: %v", err)
	}
}

// ─── Concurrent Apply calls share a single refresh round-trip ─────────────

func TestAnthropicOAuthRefreshable_ConcurrentRefreshSerialized(t *testing.T) {
	dir := t.TempDir()
	expiredMs := time.Now().Add(-time.Hour).UnixMilli()
	writeNativeAuth(t, dir, "expired", "rt", expiredMs)

	var refreshCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&refreshCalls, 1)
		time.Sleep(40 * time.Millisecond) // simulate latency
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "shared-new-access",
			"refresh_token": "shared-new-refresh",
			"expires_in":    3600,
		})
	}))
	defer server.Close()

	a := auth.NewAnthropicOAuthRefreshable(dir, &http.Client{Timeout: 5 * time.Second})
	a.SetEndpointsForTest(server.URL+"/oauth/token", "client")

	const N = 20
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

	if got := atomic.LoadInt32(&refreshCalls); got != 1 {
		t.Errorf("refresh should be called once across %d concurrent Apply calls; got %d", N, got)
	}
}

// ─── Refresh response without refresh_token reuses old one ────────────────

func TestAnthropicOAuthRefreshable_RotatesAccessOnlyResponse(t *testing.T) {
	dir := t.TempDir()
	expiredMs := time.Now().Add(-time.Hour).UnixMilli()
	writeNativeAuth(t, dir, "expired", "kept-refresh", expiredMs)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Anthropic *may* omit refresh_token. Spec says reuse the old one.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "rotated-access",
			"expires_in":   3600,
		})
	}))
	defer server.Close()

	a := auth.NewAnthropicOAuthRefreshable(dir, &http.Client{Timeout: 5 * time.Second})
	a.SetEndpointsForTest(server.URL+"/oauth/token", "client")

	req, _ := http.NewRequest("POST", "https://example.com", nil)
	if err := a.Apply(context.Background(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	stored := readAuthFile(t, dir)
	if stored["access_token"] != "rotated-access" {
		t.Errorf("access_token: got %v, want rotated-access", stored["access_token"])
	}
	if stored["refresh_token"] != "kept-refresh" {
		t.Errorf("refresh_token: got %v, want kept-refresh (reused)", stored["refresh_token"])
	}
}

// ─── Mode + AuthMode constant alignment ──────────────────────────────────

func TestAnthropicOAuthRefreshable_ModeMatchesSubscriptionConstant(t *testing.T) {
	a := auth.NewAnthropicOAuthRefreshable(t.TempDir(), nil)
	if a.Mode() != agentmodel.AuthModeSubscription {
		t.Errorf("Mode: got %q, want %q", a.Mode(), agentmodel.AuthModeSubscription)
	}
}

// ─── Corrupted auth.json surfaces parse error (doesn't silently nuke state) ─

func TestAnthropicOAuthRefreshable_CorruptedAuthJSONReturnsError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte("{ this is not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	a := auth.NewAnthropicOAuthRefreshable(dir, &http.Client{Timeout: 5 * time.Second})

	req, _ := http.NewRequest("POST", "https://example.com", nil)
	err := a.Apply(context.Background(), req)
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if auth.IsLoginRequired(err) {
		t.Errorf("corrupted file should not be treated as login-required (silent reset risk): %v", err)
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error should mention parse failure, got %v", err)
	}
}

// ─── Token() exported method ────────────────────────────────────────────────

func TestAnthropicOAuthRefreshable_Token(t *testing.T) {
	dir := t.TempDir()
	expiredMs := time.Now().Add(-time.Hour).UnixMilli()
	writeNativeAuth(t, dir, "sk-ant-oat-expired", "sk-ant-ort-valid", expiredMs)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "sk-ant-oat-test",
			"refresh_token": "sk-ant-ort-test",
			"expires_in":    3600,
		})
	}))
	defer server.Close()

	a := auth.NewAnthropicOAuthRefreshable(dir, &http.Client{Timeout: 5 * time.Second})
	a.SetEndpointsForTest(server.URL+"/oauth/token", "test-client")

	tok, err := a.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() error: %v", err)
	}
	if tok != "sk-ant-oat-test" {
		t.Errorf("got %q, want %q", tok, "sk-ant-oat-test")
	}
}

// TestAnthropicOAuthRefreshable_RefreshWithoutExpiresInIsTrusted guards #1488:
// a refresh response that omits expires_in (only RECOMMENDED by RFC 6749) must
// store 0 ("unknown/trusted"), not now() — otherwise the just-minted token is
// immediately expired and every subsequent request refreshes again, a
// refresh-per-request storm against the token endpoint.
func TestAnthropicOAuthRefreshable_RefreshWithoutExpiresInIsTrusted(t *testing.T) {
	dir := t.TempDir()
	writeNativeAuth(t, dir, "old-access", "old-refresh", time.Now().Add(-time.Hour).UnixMilli())

	var refreshCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&refreshCalls, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access",
			"refresh_token": "new-refresh",
			// no expires_in
		})
	}))
	defer server.Close()

	a := auth.NewAnthropicOAuthRefreshable(dir, &http.Client{Timeout: 5 * time.Second})
	a.SetEndpointsForTest(server.URL+"/oauth/token", "client-X")

	// Two sequential applies: the first refreshes (old token expired); the
	// second must serve the new token from cache without a second refresh.
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
		if err := a.Apply(context.Background(), req); err != nil {
			t.Fatalf("Apply #%d: %v", i, err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer new-access" {
			t.Errorf("Apply #%d Authorization: got %q, want Bearer new-access", i, got)
		}
	}
	if got := atomic.LoadInt32(&refreshCalls); got != 1 {
		t.Errorf("expected exactly 1 refresh (a token with no expires_in is trusted, not re-refreshed per request); got %d — refresh storm", got)
	}

	stored := readAuthFile(t, dir)
	if expires, _ := stored["expires_at_ms"].(float64); int64(expires) != 0 {
		t.Errorf("stored expires_at_ms = %v, want 0 (unknown/trusted)", stored["expires_at_ms"])
	}
}
