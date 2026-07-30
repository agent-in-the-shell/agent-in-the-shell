package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChatGPTOAuthInternalHelpers(t *testing.T) {
	dir := t.TempDir()
	a := NewChatGPTOAuth(dir, nil)
	if a.httpClient == nil {
		t.Fatal("default http client was not set")
	}
	if a.Mode() != "subscription" {
		t.Fatalf("Mode = %q", a.Mode())
	}

	a.populateCache(&oauthCreds{
		AccessToken: "access",
		AccountID:   "account",
		ExpiresAtMS: time.Now().Add(time.Hour).UnixMilli(),
	})
	if a.cached != "access" || a.cachedAccount != "account" || a.cacheExpired() {
		t.Fatalf("cache state = token:%q account:%q exp:%d", a.cached, a.cachedAccount, a.cachedExp)
	}
	a.cachedExp = 0
	if a.cacheExpired() {
		t.Fatal("zero cache expiry should be treated as valid")
	}
	a.cachedExp = time.Now().Add(-time.Hour).UnixMilli()
	if !a.cacheExpired() {
		t.Fatal("past cache expiry should be expired")
	}

	if a.tokenExpired(&oauthCreds{ExpiresAtMS: 0}) {
		t.Fatal("zero token expiry should be treated as valid")
	}
	if !a.tokenExpired(&oauthCreds{ExpiresAtMS: time.Now().Add(-time.Hour).UnixMilli()}) {
		t.Fatal("past token expiry should be expired")
	}
}

func TestChatGPTDefaultTokenDirAndRedaction(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CHATGPT_TOKEN_DIR", "")

	// No legacy <base>/auth.json exists, so the default profile resolves to
	// the <base>/default subdir (mirrors the Anthropic refreshable).
	a := NewChatGPTOAuth("", nil)
	if a.tokenDir != filepath.Join(home, ".config", "agentmodel", "chatgpt", "default") {
		t.Fatalf("tokenDir = %q", a.tokenDir)
	}

	// A pre-existing legacy single-store auth.json keeps the flat <base> path
	// so existing installs are not stranded by the profile layout.
	legacyBase := filepath.Join(home, ".config", "agentmodel", "chatgpt")
	if err := os.MkdirAll(legacyBase, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyBase, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if legacy := NewChatGPTOAuth("", nil); legacy.tokenDir != legacyBase {
		t.Fatalf("legacy tokenDir = %q, want %q", legacy.tokenDir, legacyBase)
	}

	got := redactSensitiveJSON([]byte(`{
		"access_token": "secret",
		"nested": [{"code_verifier": "verifier", "safe": "value"}]
	}`))
	if strings.Contains(got, `"secret"`) || strings.Contains(got, `"verifier"`) {
		t.Fatalf("redaction leaked sensitive data: %s", got)
	}
	if !strings.Contains(got, `"safe":"value"`) {
		t.Fatalf("redaction removed safe data: %s", got)
	}
	if got := redactSensitiveJSON([]byte(`{`)); got != "<invalid json>" {
		t.Fatalf("invalid redaction = %q", got)
	}
}

func TestChatGPTExchangeDeviceAuthorizationCode(t *testing.T) {
	var sawForm bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth" {
			t.Fatalf("path = %s, want /oauth", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Fatalf("content-type = %q", ct)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		sawForm = r.Form.Get("grant_type") == "authorization_code" &&
			r.Form.Get("code") == "auth-code" &&
			r.Form.Get("code_verifier") == "verifier" &&
			strings.Contains(r.Form.Get("redirect_uri"), "/deviceauth/callback")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access",
			"refresh_token": "refresh",
			"id_token":      "id",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	a := NewChatGPTOAuth(t.TempDir(), srv.Client())
	a.OverrideURLs("", "", srv.URL+"/oauth")
	got, err := a.exchangeDeviceAuthorizationCode(context.Background(), "auth-code", "verifier")
	if err != nil {
		t.Fatal(err)
	}
	if !sawForm {
		t.Fatal("oauth endpoint did not receive expected form fields")
	}
	if got.AccessToken != "access" || got.RefreshToken != "refresh" || got.IDToken != "id" || got.ExpiresIn != 3600 {
		t.Fatalf("token response = %#v", got)
	}
	if _, err := a.exchangeDeviceAuthorizationCode(context.Background(), "auth-code", ""); err == nil ||
		!strings.Contains(err.Error(), "missing code_verifier") {
		t.Fatalf("missing verifier err = %v", err)
	}
}

func TestChatGPTExchangeDeviceAuthorizationCodeErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
		code int
		want string
	}{
		{"http error", `{"error":"bad"}`, http.StatusBadGateway, "502"},
		{"decode error", `{bad-json`, http.StatusOK, "decode"},
		{"missing token", `{"access_token":"access"}`, http.StatusOK, "missing access_token or refresh_token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.code)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			a := NewChatGPTOAuth(t.TempDir(), srv.Client())
			a.OverrideURLs("", "", srv.URL)
			_, err := a.exchangeDeviceAuthorizationCode(context.Background(), "auth-code", "verifier")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestAnthropicRefreshableProfileConstructorAndOpenBrowser(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)

	work := NewAnthropicOAuthRefreshableForProfile("work", nil)
	if work.tokenDir != filepath.Join(base, "work") {
		t.Fatalf("work tokenDir = %q", work.tokenDir)
	}
	def := NewAnthropicOAuthRefreshableForProfile("", nil)
	if def.tokenDir != filepath.Join(base, DefaultProfile) {
		t.Fatalf("default tokenDir = %q", def.tokenDir)
	}

	login := NewAnthropicLogin("", nil)
	if login.tokenDir != filepath.Join(base, DefaultProfile) && login.tokenDir != base {
		t.Fatalf("login tokenDir = %q, want profile/default-resolved dir under %q", login.tokenDir, base)
	}

	t.Setenv("PATH", "")
	OpenBrowser("http://localhost:1")
}

func TestAtomicWriteFileModeCleansTempOnRenameFailure(t *testing.T) {
	dir := t.TempDir()
	targetDir := filepath.Join(dir, "target")
	if err := os.Mkdir(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	err := atomicWriteFileMode(targetDir, []byte("secret"), 0o600)
	if err == nil {
		t.Fatal("atomicWriteFileMode to existing directory = nil err, want rename error")
	}
	if _, statErr := os.Stat(targetDir + ".tmp"); !os.IsNotExist(statErr) {
		t.Fatalf("temp file was not cleaned up; stat err=%v", statErr)
	}
}
