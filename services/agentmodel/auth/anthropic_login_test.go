package auth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
)

// ─── PKCEPair ────────────────────────────────────────────────────────────────

func TestPKCEPair(t *testing.T) {
	verifier, challenge := auth.PKCEPair()

	// Re-derive challenge from verifier independently.
	sum := sha256.Sum256([]byte(verifier))
	expected := base64.RawURLEncoding.EncodeToString(sum[:])
	if challenge != expected {
		t.Errorf("challenge mismatch:\n  got  %q\n  want %q", challenge, expected)
	}

	// Verifier must be ≥43 chars (32 bytes base64url).
	if len(verifier) < 43 {
		t.Errorf("verifier too short: %d chars, want ≥43", len(verifier))
	}

	// Two calls must produce different verifiers.
	v2, _ := auth.PKCEPair()
	if verifier == v2 {
		t.Error("PKCEPair must return different verifiers on each call")
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// fakeTokenServer returns a test server that responds to POST requests with the
// given access and refresh tokens.
func fakeTokenServer(t *testing.T, access, refresh string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewEncoder(w).Encode(map[string]any{
			"access_token":  access,
			"refresh_token": refresh,
			"expires_in":    3600,
		}); err != nil {
			t.Errorf("fakeTokenServer encode: %v", err)
		}
	}))
}

// stdinPipe returns a reader/writer pair. Call injectCode to write a code line
// to the reader that the login flow will consume from stdin.
func stdinPipe(t *testing.T) (r io.Reader, injectCode func(code string)) {
	t.Helper()
	pr, pw := io.Pipe()
	injectCode = func(code string) {
		go func() {
			fmt.Fprintln(pw, code)
			pw.Close()
		}()
	}
	return pr, injectCode
}

// ─── OOB stdin path — happy path ─────────────────────────────────────────────

// TestAnthropicLogin_StdinCode verifies the primary flow: user authenticates in
// browser, is redirected to console.anthropic.com/oauth/code/callback, copies
// the code, and pastes it as a bare code string.
func TestAnthropicLogin_StdinCode(t *testing.T) {
	tokenSrv := fakeTokenServer(t, "sk-ant-oat-stdin", "sk-ant-ort-stdin")
	defer tokenSrv.Close()

	dir := t.TempDir()
	l := auth.NewAnthropicLogin(dir, &http.Client{Timeout: 5 * time.Second})
	l.SetEndpointsForTest(tokenSrv.URL+"/oauth/token", "https://example.com/auth", "")
	l.SetBrowserOpenerForTest(func(_ string) {}) // no-op: don't open real browser

	stdinR, inject := stdinPipe(t)
	l.SetStdinForTest(stdinR)

	// Inject code immediately (simulates user paste).
	inject("stdin-code-abc")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := l.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	stored := readAuthFile(t, dir)
	if stored["provider"] != "anthropic" {
		t.Errorf("provider: got %v, want anthropic", stored["provider"])
	}
	if stored["access_token"] != "sk-ant-oat-stdin" {
		t.Errorf("access_token: got %v, want sk-ant-oat-stdin", stored["access_token"])
	}
	if stored["refresh_token"] != "sk-ant-ort-stdin" {
		t.Errorf("refresh_token: got %v, want sk-ant-ort-stdin", stored["refresh_token"])
	}
	expires, _ := stored["expires_at_ms"].(float64)
	if int64(expires) <= time.Now().UnixMilli() {
		t.Errorf("expires_at_ms must be in the future, got %v", stored["expires_at_ms"])
	}
}

// TestAnthropicLogin_StdinFullURL verifies that the user can paste the full
// callback URL (https://console.anthropic.com/oauth/code/callback?code=xxx&state=yyy)
// and the code is extracted correctly.
func TestAnthropicLogin_StdinFullURL(t *testing.T) {
	tokenSrv := fakeTokenServer(t, "sk-ant-oat-url", "sk-ant-ort-url")
	defer tokenSrv.Close()

	dir := t.TempDir()
	l := auth.NewAnthropicLogin(dir, &http.Client{Timeout: 5 * time.Second})
	l.SetEndpointsForTest(tokenSrv.URL+"/oauth/token", "https://example.com/auth", "")
	l.SetBrowserOpenerForTest(func(_ string) {})

	stdinR, inject := stdinPipe(t)
	l.SetStdinForTest(stdinR)

	// Paste full callback URL as returned by the hosted callback page.
	inject("https://console.anthropic.com/oauth/code/callback?code=url-code-xyz&state=somestate")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := l.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	stored := readAuthFile(t, dir)
	if stored["access_token"] != "sk-ant-oat-url" {
		t.Errorf("access_token: got %v, want sk-ant-oat-url", stored["access_token"])
	}
}

// ─── Exchange request body + auth.json shape ─────────────────────────────────

func TestAnthropicLogin_ExchangeCode(t *testing.T) {
	var capturedBody []byte
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		if err := json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "sk-ant-oat-exch",
			"refresh_token": "sk-ant-ort-exch",
			"expires_in":    7200,
		}); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	defer tokenSrv.Close()

	dir := t.TempDir()
	l := auth.NewAnthropicLogin(dir, &http.Client{Timeout: 5 * time.Second})
	l.SetEndpointsForTest(tokenSrv.URL+"/oauth/token", "https://example.com/auth", "test-client-id")
	l.SetBrowserOpenerForTest(func(_ string) {})

	stdinR, inject := stdinPipe(t)
	l.SetStdinForTest(stdinR)
	inject("exchange-code-xyz")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := l.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Verify exchange request body.
	var body map[string]string
	if err := json.Unmarshal(capturedBody, &body); err != nil {
		t.Fatalf("parse exchange body: %v", err)
	}
	if body["grant_type"] != "authorization_code" {
		t.Errorf("grant_type: got %q, want authorization_code", body["grant_type"])
	}
	if body["code"] != "exchange-code-xyz" {
		t.Errorf("code: got %q, want exchange-code-xyz", body["code"])
	}
	if body["client_id"] != "test-client-id" {
		t.Errorf("client_id: got %q, want test-client-id", body["client_id"])
	}
	// redirect_uri must be the hosted callback page, not a loopback URI.
	if body["redirect_uri"] != auth.AnthropicOAuthRedirectURI {
		t.Errorf("redirect_uri: got %q, want %q", body["redirect_uri"], auth.AnthropicOAuthRedirectURI)
	}
	// code_verifier must be present and match state.
	if body["code_verifier"] == "" {
		t.Error("code_verifier must not be empty")
	}
	if body["state"] != body["code_verifier"] {
		t.Errorf("state must equal code_verifier: state=%q verifier=%q", body["state"], body["code_verifier"])
	}

	// Verify auth.json shape.
	stored := readAuthFile(t, dir)
	if stored["provider"] != "anthropic" {
		t.Errorf("provider: got %v, want anthropic", stored["provider"])
	}
	if stored["access_token"] != "sk-ant-oat-exch" {
		t.Errorf("access_token: got %v, want sk-ant-oat-exch", stored["access_token"])
	}
	if stored["schema"] != "agentmodel.oauth/v1" {
		t.Errorf("schema: got %v, want agentmodel.oauth/v1", stored["schema"])
	}
	expires := int64(stored["expires_at_ms"].(float64))
	// expires_in = 7200s → should be ~2 hours in the future
	minExpires := time.Now().Add(7000 * time.Second).UnixMilli()
	if expires < minExpires {
		t.Errorf("expires_at_ms too soon: got %v, want ≥ %v", expires, minExpires)
	}
}

// ─── Empty stdin — returns error, no token written ───────────────────────────

func TestAnthropicLogin_EmptyStdin(t *testing.T) {
	dir := t.TempDir()
	l := auth.NewAnthropicLogin(dir, &http.Client{Timeout: 5 * time.Second})
	l.SetEndpointsForTest("http://127.0.0.1:1/unused", "https://example.com/auth", "")
	l.SetBrowserOpenerForTest(func(_ string) {})

	// Empty reader → EOF immediately → "stdin closed without input"
	l.SetStdinForTest(strings.NewReader(""))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := l.Run(ctx)
	if err == nil {
		t.Fatal("expected error for empty stdin, got nil")
	}

	// auth.json must NOT have been written.
	if _, statErr := os.Stat(filepath.Join(dir, "auth.json")); !os.IsNotExist(statErr) {
		t.Error("auth.json must not exist when no code was obtained")
	}
}
