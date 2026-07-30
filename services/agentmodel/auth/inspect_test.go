package auth

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Deterministic expiry anchors far from any real test clock so expiredMS's
// verdict never depends on wall time.
const (
	inspectFutureMS  = int64(4102444800000) // 2100-01-01, always fresh
	inspectPastMS    = int64(946684800000)  // 2000-01-01, always expired
	inspectFutureSec = int64(4102444800)    // same instants, seconds (codex format)
	inspectPastSec   = int64(946684800)
)

// writeStore drops a raw auth.json into a fresh token dir and returns the dir.
func writeStore(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
	return dir
}

func TestInspectStore(t *testing.T) {
	tests := []struct {
		name        string
		provider    string
		body        string
		wantExists  bool
		wantToken   bool
		wantExpMS   int64
		wantExpired bool
		wantAccount string
	}{
		{
			name:       "native_fresh",
			provider:   "chatgpt",
			body:       `{"schema":"agentmodel.oauth/v1","provider":"chatgpt","access_token":"tok","account_id":"acct-1","expires_at_ms":4102444800000}`,
			wantExists: true, wantToken: true, wantExpMS: inspectFutureMS, wantExpired: false, wantAccount: "acct-1",
		},
		{
			name:       "native_expired",
			provider:   "chatgpt",
			body:       `{"schema":"agentmodel.oauth/v1","provider":"chatgpt","access_token":"tok","expires_at_ms":946684800000}`,
			wantExists: true, wantToken: true, wantExpMS: inspectPastMS, wantExpired: true,
		},
		{
			name:       "native_unknown_expiry_is_not_expired",
			provider:   "anthropic",
			body:       `{"schema":"agentmodel.oauth/v1","provider":"anthropic","access_token":"tok","expires_at_ms":0}`,
			wantExists: true, wantToken: true, wantExpMS: 0, wantExpired: false,
		},
		{
			name:       "chatgpt_codex_fresh_seconds",
			provider:   "chatgpt",
			body:       `{"access_token":"tok","refresh_token":"r","id_token":"i","account_id":"acct-2","expires_at":4102444800}`,
			wantExists: true, wantToken: true, wantExpMS: inspectFutureMS, wantExpired: false, wantAccount: "acct-2",
		},
		{
			name:       "chatgpt_codex_expired_seconds",
			provider:   "chatgpt",
			body:       `{"access_token":"tok","refresh_token":"r","account_id":"acct-3","expires_at":946684800}`,
			wantExists: true, wantToken: true, wantExpMS: inspectPastMS, wantExpired: true, wantAccount: "acct-3",
		},
		{
			name:       "anthropic_nested_fresh_millis",
			provider:   "anthropic",
			body:       `{"anthropic":{"type":"oauth","access":"tok","refresh":"r","expires":4102444800000}}`,
			wantExists: true, wantToken: true, wantExpMS: inspectFutureMS, wantExpired: false,
		},
		{
			name:       "anthropic_nested_expired_millis",
			provider:   "anthropic",
			body:       `{"anthropic":{"type":"oauth","access":"tok","refresh":"r","expires":946684800000}}`,
			wantExists: true, wantToken: true, wantExpMS: inspectPastMS, wantExpired: true,
		},
		{
			name:       "present_but_no_token",
			provider:   "chatgpt",
			body:       `{"schema":"agentmodel.oauth/v1","provider":"chatgpt","expires_at_ms":4102444800000}`,
			wantExists: true, wantToken: false, wantExpMS: inspectFutureMS, wantExpired: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeStore(t, tc.body)
			got, err := InspectStore(tc.provider, dir)
			if err != nil {
				t.Fatalf("InspectStore: unexpected error: %v", err)
			}
			if got.Exists != tc.wantExists {
				t.Errorf("Exists: got %v, want %v", got.Exists, tc.wantExists)
			}
			if got.HasToken != tc.wantToken {
				t.Errorf("HasToken: got %v, want %v", got.HasToken, tc.wantToken)
			}
			if got.ExpiresAtMS != tc.wantExpMS {
				t.Errorf("ExpiresAtMS: got %d, want %d", got.ExpiresAtMS, tc.wantExpMS)
			}
			if got.Expired != tc.wantExpired {
				t.Errorf("Expired: got %v, want %v", got.Expired, tc.wantExpired)
			}
			if got.AccountID != tc.wantAccount {
				t.Errorf("AccountID: got %q, want %q", got.AccountID, tc.wantAccount)
			}
		})
	}
}

func TestInspectStore_MissingFileIsNotAnError(t *testing.T) {
	got, err := InspectStore("chatgpt", t.TempDir()) // dir exists, auth.json does not
	if err != nil {
		t.Fatalf("missing auth.json should not error, got: %v", err)
	}
	if got.Exists {
		t.Errorf("Exists: got true, want false for a missing store")
	}
	if got.HasToken {
		t.Errorf("HasToken: got true, want false for a missing store")
	}
}

func TestInspectStore_CorruptFileErrors(t *testing.T) {
	dir := writeStore(t, `{not valid json`)
	_, err := InspectStore("chatgpt", dir)
	if err == nil {
		t.Fatal("corrupt auth.json should return an error, got nil")
	}
}

// A missing token directory is the same observable state as a missing file:
// nothing to report, no error.
func TestInspectStore_MissingDirIsNotAnError(t *testing.T) {
	got, err := InspectStore("anthropic", filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing dir should not surface a non-NotExist error, got: %v", err)
	}
	if err != nil {
		t.Fatalf("missing dir should be reported as Exists:false, not an error: %v", err)
	}
	if got.Exists {
		t.Errorf("Exists: got true, want false for a missing dir")
	}
}

// TestInspectStore_GatewayReadable: the request path (readCreds) reads only the
// flat access_token field, so native + Codex tokens are gateway-readable but the
// nested Claude-CLI layout (token under anthropic.access) is NOT — status must be
// able to flag a store the gateway cannot actually consume (#1490/#1487 review).
func TestInspectStore_GatewayReadable(t *testing.T) {
	cases := []struct {
		name, provider, body string
		want                 bool
	}{
		{"native", "anthropic", `{"schema":"agentmodel.oauth/v1","access_token":"t","expires_at_ms":4102444800000}`, true},
		{"codex", "chatgpt", `{"access_token":"t","expires_at":4102444800}`, true},
		{"nested_anthropic", "anthropic", `{"anthropic":{"access":"t","expires":4102444800000}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, err := InspectStore(tc.provider, writeStore(t, tc.body))
			if err != nil {
				t.Fatalf("InspectStore: %v", err)
			}
			if st.GatewayReadable != tc.want {
				t.Errorf("GatewayReadable = %v, want %v", st.GatewayReadable, tc.want)
			}
		})
	}
}

// TestInspectStore_GatewayReadableMatchesReadCreds pins the coupling: a store is
// GatewayReadable exactly when the request path's readCreds extracts a token from
// it. If readCreds ever learns a new on-disk layout, this guard forces
// GatewayReadable to keep pace instead of silently lying (#1490).
func TestInspectStore_GatewayReadableMatchesReadCreds(t *testing.T) {
	bodies := []string{
		`{"schema":"agentmodel.oauth/v1","access_token":"t","expires_at_ms":4102444800000}`, // native
		`{"access_token":"t","expires_at":4102444800}`,                                      // codex flat
		`{"anthropic":{"access":"t","expires":4102444800000}}`,                              // nested Claude-CLI
		`{"schema":"agentmodel.oauth/v1","expires_at_ms":4102444800000}`,                    // no token
	}
	for _, body := range bodies {
		dir := writeStore(t, body)
		st, err := InspectStore("anthropic", dir)
		if err != nil {
			t.Fatalf("InspectStore(%s): %v", body, err)
		}
		creds, _ := readCreds(dir)
		readByGateway := creds != nil && creds.AccessToken != ""
		if st.GatewayReadable != readByGateway {
			t.Errorf("body %s: GatewayReadable=%v but readCreds token-present=%v", body, st.GatewayReadable, readByGateway)
		}
	}
}
