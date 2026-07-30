package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/api"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/stub"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

// gpt4Deps is the minimal single-deployment routing table the managed-key auth
// tests route through.
func gpt4Deps() map[string][]router.Deployment {
	return map[string][]router.Deployment{
		"gpt-4": {{Provider: &stub.Stub{NameValue: "openai"}, Model: "gpt-4o", Weight: 1}},
	}
}

func mustCreateKey(t *testing.T, st *store.SQLiteStore, k store.ManagedKey) {
	t.Helper()
	if err := st.CreateKey(context.Background(), k); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
}

func TestManagedKey_Authenticates(t *testing.T) {
	ts, st := newTestServer(t, gpt4Deps())
	const tok = "sk-am-live"
	mustCreateKey(t, st, store.ManagedKey{ID: "id-live", Name: "live", KeyHash: sha256hex(tok)})

	resp := mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}

	// Usage is attributed to the key's hash — the metering half this work
	// builds on (#922).
	logs, err := st.ListByAPIKey(context.Background(), sha256hex(tok), time.Time{}, 10)
	if err != nil {
		t.Fatalf("ListByAPIKey: %v", err)
	}
	if len(logs) == 0 {
		t.Error("expected a request_log attributed to the managed key's hash")
	}
}

func TestManagedKey_Revoked_Rejected(t *testing.T) {
	ts, st := newTestServer(t, gpt4Deps())
	const tok = "sk-am-revoked"
	mustCreateKey(t, st, store.ManagedKey{ID: "id-rev", Name: "rev", KeyHash: sha256hex(tok), Disabled: true})

	resp := mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", resp.StatusCode)
	}
	// Reject as a plain invalid token — don't reveal the key once existed.
	typ, code, _ := errBody(t, resp)
	if typ != agentmodel.ErrTypeAuthentication || code != agentmodel.CodeInvalidAPIKey {
		t.Errorf("error: got (%s, %s), want authentication/invalid_api_key", typ, code)
	}
}

func TestManagedKey_Expired_Rejected(t *testing.T) {
	ts, st := newTestServer(t, gpt4Deps())
	const tok = "sk-am-expired"
	past := time.Now().Add(-time.Hour)
	mustCreateKey(t, st, store.ManagedKey{ID: "id-exp", Name: "exp", KeyHash: sha256hex(tok), ExpiresAt: &past})

	resp := mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", resp.StatusCode)
	}
}

func TestManagedKey_NotYetExpired_Authenticates(t *testing.T) {
	ts, st := newTestServer(t, gpt4Deps())
	const tok = "sk-am-future"
	future := time.Now().Add(time.Hour)
	mustCreateKey(t, st, store.ManagedKey{ID: "id-fut", Name: "fut", KeyHash: sha256hex(tok), ExpiresAt: &future})

	resp := mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
}

func TestManagedKey_ModelAllowlist_Enforced(t *testing.T) {
	ts, st := newTestServer(t, gpt4Deps())
	const tok = "sk-am-scoped"
	mustCreateKey(t, st, store.ManagedKey{
		ID: "id-scope", Name: "scope", KeyHash: sha256hex(tok), Models: []string{"claude-only"},
	})

	resp := mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", resp.StatusCode)
	}
	typ, code, _ := errBody(t, resp)
	if typ != agentmodel.ErrTypePermissionDenied || code != agentmodel.CodeModelAccessDenied {
		t.Errorf("error: got (%s, %s), want permission/model_access_denied", typ, code)
	}
}

func TestManagedKey_BudgetEnforced(t *testing.T) {
	ts, st := newTestServer(t, gpt4Deps())
	const tok = "sk-am-capped"
	mustCreateKey(t, st, store.ManagedKey{
		ID: "id-cap", Name: "cap", KeyHash: sha256hex(tok), MaxBudget: limitsFloatPtr(1.0), // lifetime
	})
	// Spend already at the cap, attributed to this key's hash.
	seedSpend(t, st, sha256hex(tok), 1.0, time.Now().UTC())

	resp := mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", resp.StatusCode)
	}
	typ, _, _ := errBody(t, resp)
	if typ != agentmodel.ErrTypeBudgetExceeded {
		t.Errorf("type: got %s, want budget_exceeded", typ)
	}
}

// A managed key authenticates even when config keys are also present: the
// lookup runs after the master + config-key checks miss (precedence: master →
// config → DB).
func TestManagedKey_AuthenticatesAlongsideConfigKeys(t *testing.T) {
	t.Setenv("TEST_VK_CFG", "vk-cfg")
	ts, st := newTestServer(t, gpt4Deps(), func(c *api.Config) {
		c.Keys = []agentmodel.KeyConfig{{Name: "cfg", TokenEnv: "TEST_VK_CFG"}}
	})
	const tok = "sk-am-mixed"
	mustCreateKey(t, st, store.ManagedKey{ID: "id-mix", Name: "mix", KeyHash: sha256hex(tok)})

	for _, token := range []string{"vk-cfg", tok, testToken} {
		resp := mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), token)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("token %q: got %d, want 200", token, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// When the store is unavailable, a token that is neither master nor a config
// key cannot be verified, so the request fails closed with 503 — never a
// misleading 401 that a valid key might also receive.
func TestManagedKey_StoreUnavailable_FailsClosed(t *testing.T) {
	ts, st := newTestServer(t, gpt4Deps())
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	resp := mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), "sk-am-unknown")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", resp.StatusCode)
	}
	typ, code, _ := errBody(t, resp)
	if typ != agentmodel.ErrTypeServiceUnavailable || code != agentmodel.CodeAuthUnavailable {
		t.Errorf("error: got (%s, %s), want service_unavailable/auth_check_unavailable", typ, code)
	}
}
