package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/api"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/cost"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/stub"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

func sha256hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func chatBody(model string) agentmodel.ChatRequest {
	return agentmodel.ChatRequest{
		Model:    model,
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	}
}

// seedSpend writes one successful audit row so budget checks see prior spend.
func seedSpend(t *testing.T, st *store.SQLiteStore, keyHash string, costUSD float64, at time.Time) {
	t.Helper()
	err := st.LogRequest(context.Background(), store.RequestLog{
		ID:         "seed-" + keyHash + "-" + at.UTC().Format(time.RFC3339Nano),
		OrgID:      "default",
		APIKeyHash: keyHash,
		CostUSD:    costUSD,
		Status:     "ok",
		CreatedAt:  at,
	})
	if err != nil {
		t.Fatalf("seed spend: %v", err)
	}
}

func errBody(t *testing.T, resp *http.Response) (typ, code, message string) {
	t.Helper()
	var got struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return got.Error.Type, got.Error.Code, got.Error.Message
}

// ─── Virtual key auth ────────────────────────────────────────────────

func TestVirtualKeyAuth(t *testing.T) {
	t.Setenv("TEST_VK_CI", "vk-token-ci")
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"gpt-4": {{Provider: &stub.Stub{NameValue: "openai"}, Model: "gpt-4o", Weight: 1}},
	}, func(c *api.Config) {
		c.Keys = []agentmodel.KeyConfig{{Name: "ci", TokenEnv: "TEST_VK_CI"}}
	})

	t.Run("key token authenticates", func(t *testing.T) {
		resp := mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), "vk-token-ci")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status: got %d, want 200", resp.StatusCode)
		}
	})
	t.Run("master token still authenticates", func(t *testing.T) {
		resp := mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), testToken)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status: got %d, want 200", resp.StatusCode)
		}
	})
	t.Run("unknown token rejected", func(t *testing.T) {
		resp := mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), "not-a-token")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status: got %d, want 401", resp.StatusCode)
		}
	})
}

func TestVirtualKeyAuth_UnsetEnvFailsClosed(t *testing.T) {
	// TEST_VK_GHOST is deliberately not set: the key must be disabled, and no
	// token can authenticate as it.
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"gpt-4": {{Provider: &stub.Stub{NameValue: "openai"}, Model: "gpt-4o", Weight: 1}},
	}, func(c *api.Config) {
		c.Keys = []agentmodel.KeyConfig{{Name: "ghost", TokenEnv: "TEST_VK_GHOST"}}
	})

	// An empty bearer token must not match the disabled key's empty resolved
	// value (the classic unset-env auth bypass).
	req, _ := http.NewRequestWithContext(context.Background(), "POST", ts.URL+"/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer ")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("empty token: got %d, want 401 (fail-closed)", resp.StatusCode)
	}
}

// ─── Model allowlist ─────────────────────────────────────────────────

func TestModelAllowlist(t *testing.T) {
	t.Setenv("TEST_VK_LTD", "vk-token-ltd")
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"gpt-4":  {{Provider: &stub.Stub{NameValue: "openai"}, Model: "gpt-4o", Weight: 1}},
		"claude": {{Provider: &stub.Stub{NameValue: "anthropic"}, Model: "claude-sonnet", Weight: 1}},
	}, func(c *api.Config) {
		c.Keys = []agentmodel.KeyConfig{{Name: "ltd", TokenEnv: "TEST_VK_LTD", Models: []string{"gpt-4"}}}
	})

	t.Run("allowed model serves", func(t *testing.T) {
		resp := mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), "vk-token-ltd")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status: got %d, want 200", resp.StatusCode)
		}
	})
	t.Run("disallowed model rejected 403", func(t *testing.T) {
		resp := mustPost(t, ts, "/v1/chat/completions", chatBody("claude"), "vk-token-ltd")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status: got %d, want 403", resp.StatusCode)
		}
		typ, code, _ := errBody(t, resp)
		if typ != agentmodel.ErrTypePermissionDenied || code != "model_access_denied" {
			t.Errorf("error: got (%s, %s), want (permission_error, model_access_denied)", typ, code)
		}
	})
	t.Run("master token unrestricted", func(t *testing.T) {
		resp := mustPost(t, ts, "/v1/chat/completions", chatBody("claude"), testToken)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status: got %d, want 200", resp.StatusCode)
		}
	})
}

// The allowlist must bound fallbacks too: a restricted key's request is never
// served by a fallback target outside its allowlist, while the master token
// falls back normally (the documented  contract).
func TestModelAllowlist_BoundsFallbacks(t *testing.T) {
	t.Setenv("TEST_VK_FB", "vk-token-fb")
	fallbackOK := &stub.Stub{NameValue: "anthropic"}
	st, err := store.OpenSQLite(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	registry, err := cost.LoadDefault()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	r := router.New(map[string][]router.Deployment{
		"gpt-4":  {{Provider: &stub.Stub{NameValue: "openai", CompleteErr: errors.New("503 overloaded")}, Model: "gpt-4o", Weight: 1}},
		"claude": {{Provider: fallbackOK, Model: "claude-sonnet", Weight: 1}},
	}, map[string][]string{"gpt-4": {"claude"}})
	s := api.New(api.Config{
		Router:      r,
		Store:       st,
		Registry:    registry,
		BearerToken: testToken,
		Keys:        []agentmodel.KeyConfig{{Name: "fb", TokenEnv: "TEST_VK_FB", Models: []string{"gpt-4"}}},
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	t.Run("master falls back to claude", func(t *testing.T) {
		resp := mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), testToken)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status: got %d, want 200 via fallback", resp.StatusCode)
		}
	})
	t.Run("restricted key cannot reach the fallback", func(t *testing.T) {
		before := fallbackOK.CallCount()
		resp := mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), "vk-token-fb")
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatal("restricted key must not be served by the blocked fallback")
		}
		if fallbackOK.CallCount() != before {
			t.Errorf("blocked fallback provider was called")
		}
	})
}

// ─── Budget enforcement ──────────────────────────────────────────────

func TestOrgBudgetEnforced(t *testing.T) {
	ts, st := newTestServer(t, map[string][]router.Deployment{
		"gpt-4": {{Provider: &stub.Stub{NameValue: "openai"}, Model: "gpt-4o", Weight: 1}},
	}, func(c *api.Config) {
		c.Budget = agentmodel.BudgetConfig{MaxBudget: limitsFloatPtr(1.0)} // lifetime
	})

	// Under cap: serves.
	seedSpend(t, st, sha256hex(testToken), 0.5, time.Now().UTC())
	resp := mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), testToken)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("under cap: got %d, want 200", resp.StatusCode)
	}

	// At/over cap: rejected before upstream, 400 budget_exceeded.
	seedSpend(t, st, sha256hex(testToken), 0.6, time.Now().UTC())
	resp = mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("over cap: got %d, want 400", resp.StatusCode)
	}
	typ, code, msg := errBody(t, resp)
	if typ != agentmodel.ErrTypeBudgetExceeded || code != "budget_exceeded" {
		t.Errorf("error: got (%s, %s, %q), want budget_exceeded", typ, code, msg)
	}
}

func TestKeyBudgetEnforced(t *testing.T) {
	t.Setenv("TEST_VK_CAP", "vk-token-cap")
	t.Setenv("TEST_VK_FREE", "vk-token-free")
	ts, st := newTestServer(t, map[string][]router.Deployment{
		"gpt-4": {{Provider: &stub.Stub{NameValue: "openai"}, Model: "gpt-4o", Weight: 1}},
	}, func(c *api.Config) {
		c.Keys = []agentmodel.KeyConfig{
			{Name: "cap", TokenEnv: "TEST_VK_CAP", MaxBudget: limitsFloatPtr(1.0)},
			{Name: "free", TokenEnv: "TEST_VK_FREE"},
		}
	})

	// Spend 1.0 attributed to the capped key: it is now at its cap.
	seedSpend(t, st, sha256hex("vk-token-cap"), 1.0, time.Now().UTC())

	t.Run("capped key rejected", func(t *testing.T) {
		resp := mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), "vk-token-cap")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status: got %d, want 400", resp.StatusCode)
		}
		typ, _, _ := errBody(t, resp)
		if typ != agentmodel.ErrTypeBudgetExceeded {
			t.Errorf("type: got %s, want budget_exceeded", typ)
		}
	})
	t.Run("other key unaffected", func(t *testing.T) {
		resp := mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), "vk-token-free")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status: got %d, want 200", resp.StatusCode)
		}
	})
	t.Run("master unaffected", func(t *testing.T) {
		resp := mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), testToken)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status: got %d, want 200", resp.StatusCode)
		}
	})
}

// Spend outside the current fixed window must not count against a windowed cap.
func TestKeyBudgetWindowExcludesOldSpend(t *testing.T) {
	t.Setenv("TEST_VK_WIN", "vk-token-win")
	ts, st := newTestServer(t, map[string][]router.Deployment{
		"gpt-4": {{Provider: &stub.Stub{NameValue: "openai"}, Model: "gpt-4o", Weight: 1}},
	}, func(c *api.Config) {
		c.Keys = []agentmodel.KeyConfig{
			{Name: "win", TokenEnv: "TEST_VK_WIN", MaxBudget: limitsFloatPtr(1.0), BudgetDuration: "24h"},
		}
	})

	// Heavy spend two windows ago: invisible to the current window.
	seedSpend(t, st, sha256hex("vk-token-win"), 50.0, time.Now().UTC().Add(-48*time.Hour))
	resp := mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), "vk-token-win")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("old spend leaked into current window: got %d, want 200", resp.StatusCode)
	}

	// Spend inside the current window trips the cap. Truncate-aligned windows
	// start at multiples of 24h (midnight UTC), so "now" is always inside.
	seedSpend(t, st, sha256hex("vk-token-win"), 1.0, time.Now().UTC())
	resp = mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), "vk-token-win")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("in-window spend at cap: got %d, want 400", resp.StatusCode)
	}
}

// A configured cap with no ledger must fail closed (503), not silently allow.
func TestBudgetFailsClosedWithoutStore(t *testing.T) {
	r := router.New(map[string][]router.Deployment{
		"gpt-4": {{Provider: &stub.Stub{NameValue: "openai"}, Model: "gpt-4o", Weight: 1}},
	}, nil)
	s := api.New(api.Config{
		Router:      r,
		BearerToken: testToken,
		Budget:      agentmodel.BudgetConfig{MaxBudget: limitsFloatPtr(1.0)},
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp := mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want 503 (fail-closed)", resp.StatusCode)
	}
}

// Budget exhaustion must never lock the caller out of the read endpoints —
// /v1/limits stays observable so headroom can be diagnosed.
func TestBudgetDoesNotGateReadEndpoints(t *testing.T) {
	ts, st := newTestServer(t, map[string][]router.Deployment{
		"gpt-4": {{Provider: &stub.Stub{NameValue: "openai"}, Model: "gpt-4o", Weight: 1}},
	}, func(c *api.Config) {
		c.Budget = agentmodel.BudgetConfig{MaxBudget: limitsFloatPtr(0.5)}
	})
	seedSpend(t, st, sha256hex(testToken), 9.0, time.Now().UTC())

	resp := getLimits(t, ts, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/v1/limits while over budget: got %d, want 200", resp.StatusCode)
	}
}

// ─── /v1/messages enforcement (Anthropic envelope) ─────────────────────────

func anthropicErrBody(t *testing.T, resp *http.Response) (typ, message string) {
	t.Helper()
	var got struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode anthropic error body: %v", err)
	}
	return got.Error.Type, got.Error.Message
}

func TestMessagesEnforcement(t *testing.T) {
	t.Setenv("TEST_VK_MSG", "vk-token-msg")
	t.Setenv("TEST_VK_MSGCAP", "vk-token-msgcap")
	ts, st := newTestServer(t, map[string][]router.Deployment{
		"claude": {{Provider: &stub.Stub{NameValue: "anthropic", MessagesPassthroughFn: stub.PassthroughFunc(200, `{"id":"m1","usage":{"input_tokens":1,"output_tokens":1}}`)}, Model: "claude-sonnet", Weight: 1}},
		"gpt-4":  {{Provider: &stub.Stub{NameValue: "openai"}, Model: "gpt-4o", Weight: 1}},
	}, func(c *api.Config) {
		c.Keys = []agentmodel.KeyConfig{
			{Name: "msg", TokenEnv: "TEST_VK_MSG", Models: []string{"gpt-4"}},
			{Name: "msgcap", TokenEnv: "TEST_VK_MSGCAP", MaxBudget: limitsFloatPtr(1.0)},
		}
	})

	msgBody := map[string]any{"model": "claude", "max_tokens": 16, "messages": []map[string]any{{"role": "user", "content": "hi"}}}

	t.Run("allowlist rejection in anthropic envelope", func(t *testing.T) {
		resp := mustPost(t, ts, "/v1/messages", msgBody, "vk-token-msg")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status: got %d, want 403", resp.StatusCode)
		}
		typ, _ := anthropicErrBody(t, resp)
		if typ != agentmodel.ErrTypePermissionDenied {
			t.Errorf("error type: got %q, want permission_error", typ)
		}
	})
	t.Run("budget rejection in anthropic envelope", func(t *testing.T) {
		seedSpend(t, st, sha256hex("vk-token-msgcap"), 1.0, time.Now().UTC())
		resp := mustPost(t, ts, "/v1/messages", msgBody, "vk-token-msgcap")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status: got %d, want 400", resp.StatusCode)
		}
		typ, _ := anthropicErrBody(t, resp)
		if typ != agentmodel.ErrTypeBudgetExceeded {
			t.Errorf("error type: got %q, want budget_exceeded", typ)
		}
	})
	t.Run("master passes through", func(t *testing.T) {
		resp := mustPost(t, ts, "/v1/messages", msgBody, testToken)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status: got %d, want 200", resp.StatusCode)
		}
	})
}

// ─── /v1/embeddings enforcement ────────────────────────────────────────────

func TestEmbeddingsEnforcement(t *testing.T) {
	t.Setenv("TEST_VK_EMB", "vk-token-emb")
	ts, st := newTestServer(t, map[string][]router.Deployment{
		"embed-model": {{Provider: &stub.Stub{NameValue: "openai"}, Model: "text-embedding-3-small", Weight: 1}},
		"gpt-4":       {{Provider: &stub.Stub{NameValue: "openai"}, Model: "gpt-4o", Weight: 1}},
	}, func(c *api.Config) {
		c.Keys = []agentmodel.KeyConfig{
			{Name: "emb", TokenEnv: "TEST_VK_EMB", Models: []string{"gpt-4"}, MaxBudget: limitsFloatPtr(1.0)},
		}
	})

	embBody := agentmodel.EmbeddingRequest{Model: "embed-model", Input: []string{"hello"}}

	t.Run("allowlist rejection", func(t *testing.T) {
		resp := mustPost(t, ts, "/v1/embeddings", embBody, "vk-token-emb")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status: got %d, want 403", resp.StatusCode)
		}
	})
	t.Run("budget rejection on allowed model", func(t *testing.T) {
		seedSpend(t, st, sha256hex("vk-token-emb"), 1.0, time.Now().UTC())
		resp := mustPost(t, ts, "/v1/embeddings", agentmodel.EmbeddingRequest{Model: "gpt-4", Input: []string{"x"}}, "vk-token-emb")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status: got %d, want 400", resp.StatusCode)
		}
		typ, code, _ := errBody(t, resp)
		if typ != agentmodel.ErrTypeBudgetExceeded || code != "budget_exceeded" {
			t.Errorf("error: got (%s, %s), want budget_exceeded", typ, code)
		}
	})
}

// A stream:true request over budget must be rejected with a plain JSON error
// before any SSE stream is opened.
func TestStreamingChatRejectedPreStream(t *testing.T) {
	ts, st := newTestServer(t, map[string][]router.Deployment{
		"gpt-4": {{Provider: &stub.Stub{NameValue: "openai"}, Model: "gpt-4o", Weight: 1}},
	}, func(c *api.Config) {
		c.Budget = agentmodel.BudgetConfig{MaxBudget: limitsFloatPtr(0.5)}
	})
	seedSpend(t, st, sha256hex(testToken), 1.0, time.Now().UTC())

	body := chatBody("gpt-4")
	body.Stream = true
	resp := mustPost(t, ts, "/v1/chat/completions", body, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400 (pre-stream rejection)", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type: got %q, want application/json (not SSE)", ct)
	}
	typ, _, _ := errBody(t, resp)
	if typ != agentmodel.ErrTypeBudgetExceeded {
		t.Errorf("type: got %s, want budget_exceeded", typ)
	}
}

// ─── Operator-only credential endpoints ────────────────────────────────────

// Virtual keys must never reach the ChatGPT OAuth endpoints: completing a
// device-code flow rebinds the gateway's upstream subscription credentials.
func TestOAuthEndpointsMasterOnly(t *testing.T) {
	t.Setenv("TEST_VK_OAUTH", "vk-token-oauth")
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"gpt-4": {{Provider: &stub.Stub{NameValue: "openai"}, Model: "gpt-4o", Weight: 1}},
	}, func(c *api.Config) {
		c.Keys = []agentmodel.KeyConfig{{Name: "oauth", TokenEnv: "TEST_VK_OAUTH"}}
	})

	for _, path := range []string{"/v1/oauth/chatgpt/start", "/v1/oauth/chatgpt/poll"} {
		resp := mustPost(t, ts, path, map[string]any{}, "vk-token-oauth")
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s with virtual key: got %d, want 403", path, resp.StatusCode)
		}
		// Master token must not be blocked by the masterOnly gate (the handler
		// may still fail for other reasons, e.g. OAuth not configured).
		resp = mustPost(t, ts, path, map[string]any{}, testToken)
		resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden {
			t.Errorf("%s with master token: unexpectedly 403", path)
		}
	}
}

// The audit ledger must record enforcement rejections (docs: "visible in the
// ledger") — and those rows must not count as spend.
func TestRejectionWritesAuditRow(t *testing.T) {
	t.Setenv("TEST_VK_AUD", "vk-token-aud")
	ts, st := newTestServer(t, map[string][]router.Deployment{
		"gpt-4":  {{Provider: &stub.Stub{NameValue: "openai"}, Model: "gpt-4o", Weight: 1}},
		"claude": {{Provider: &stub.Stub{NameValue: "anthropic"}, Model: "claude-sonnet", Weight: 1}},
	}, func(c *api.Config) {
		c.Keys = []agentmodel.KeyConfig{{Name: "aud", TokenEnv: "TEST_VK_AUD", Models: []string{"gpt-4"}}}
	})

	resp := mustPost(t, ts, "/v1/chat/completions", chatBody("claude"), "vk-token-aud")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", resp.StatusCode)
	}

	rows, err := st.ListByAPIKey(context.Background(), sha256hex("vk-token-aud"), time.Unix(0, 0), 10)
	if err != nil {
		t.Fatalf("ListByAPIKey: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("audit rows: got %d, want 1", len(rows))
	}
	if rows[0].Status != "error" || rows[0].ErrorType != agentmodel.ErrTypePermissionDenied {
		t.Errorf("row: got status=%q type=%q, want error/permission_error", rows[0].Status, rows[0].ErrorType)
	}
	if rows[0].CostUSD != 0 {
		t.Errorf("rejection row cost: got %v, want 0", rows[0].CostUSD)
	}
	sum, err := st.SumCostByAPIKey(context.Background(), sha256hex("vk-token-aud"), time.Unix(0, 0))
	if err != nil || sum != 0 {
		t.Errorf("rejection must not count as spend: got (%v, %v)", sum, err)
	}
}

// ─── RPM/TPM pre-call enforcement ─────────────────────────────────────

// sameMinute runs fn up to twice: if the UTC minute rolled while fn ran (the
// meter window reset between the test's requests), the first attempt is
// discarded and fn reruns in the fresh window. Keeps wall-clock rate-limit
// assertions deterministic without plumbing a fake clock through api.Config.
func sameMinute(t *testing.T, fn func(t *testing.T)) {
	t.Helper()
	for attempt := 0; attempt < 2; attempt++ {
		start := time.Now().UTC().Truncate(time.Minute)
		done := t.Run(fmt.Sprintf("attempt-%d", attempt), fn)
		if time.Now().UTC().Truncate(time.Minute).Equal(start) {
			if !done {
				t.Fatal("failed within a single minute window")
			}
			return
		}
		// Minute rolled mid-attempt: counters reset under us; retry fresh.
	}
	t.Fatal("minute boundary crossed on both attempts")
}

func TestRPMEnforcementEndToEnd(t *testing.T) {
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"gpt-4": {{Name: "oa/gpt", Provider: &stub.Stub{NameValue: "openai"}, Model: "gpt-4o", Weight: 1, RPM: limitsIntPtr(1)}},
	})

	sameMinute(t, func(t *testing.T) {
		resp := mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), testToken)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("first request: got %d, want 200", resp.StatusCode)
		}

		resp = mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), testToken)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("second request: got %d, want 429", resp.StatusCode)
		}
		typ, _, _ := errBody(t, resp)
		if typ != agentmodel.ErrTypeRateLimit {
			t.Errorf("type: got %s, want rate_limit_error", typ)
		}

		// /v1/limits reflects the consumed window: used=1, remaining=0, limited,
		// with a reset at the next minute boundary.
		lresp := getLimits(t, ts, testToken)
		defer lresp.Body.Close()
		var got tLimitsResp
		if err := json.NewDecoder(lresp.Body).Decode(&got); err != nil {
			t.Fatalf("decode limits: %v", err)
		}
		if got.Status != "limited" {
			t.Errorf("limits status: got %q, want limited", got.Status)
		}
		var rpmWin *tLimitWindow
		for i := range got.Windows {
			if got.Windows[i].Unit == "requests" {
				rpmWin = &got.Windows[i]
			}
		}
		if rpmWin == nil {
			t.Fatalf("no requests window in %+v", got.Windows)
		}
		if rpmWin.Used == nil || *rpmWin.Used != 1 {
			t.Errorf("used: got %v, want 1", rpmWin.Used)
		}
		if rpmWin.Remaining == nil || *rpmWin.Remaining != 0 {
			t.Errorf("remaining: got %v, want 0", rpmWin.Remaining)
		}
		if rpmWin.WindowStatus != "limited" {
			t.Errorf("window status: got %q, want limited", rpmWin.WindowStatus)
		}
	})
}

// The /v1/messages passthrough path meters the same way, including TPM fed by
// the handler-parsed usage.
func TestMessagesTPMEnforcementEndToEnd(t *testing.T) {
	pass := stub.PassthroughFunc(200, `{"id":"m1","usage":{"input_tokens":6,"output_tokens":6}}`)
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"claude": {{Name: "sub/claude", Provider: &stub.Stub{NameValue: "anthropic", MessagesPassthroughFn: pass}, Model: "claude-sonnet", Weight: 1, TPM: limitsIntPtr(10)}},
	})
	msgBody := map[string]any{"model": "claude", "max_tokens": 8, "messages": []map[string]any{{"role": "user", "content": "hi"}}}

	sameMinute(t, func(t *testing.T) {
		// First request succeeds and records 12 tokens >= tpm 10.
		resp := mustPost(t, ts, "/v1/messages", msgBody, testToken)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("first messages: got %d, want 200", resp.StatusCode)
		}

		resp = mustPost(t, ts, "/v1/messages", msgBody, testToken)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("second messages: got %d, want 429 (tpm consumed)", resp.StatusCode)
		}
		typ, _ := anthropicErrBody(t, resp)
		if typ != agentmodel.ErrTypeRateLimit {
			t.Errorf("type: got %s, want rate_limit_error", typ)
		}
	})
}
