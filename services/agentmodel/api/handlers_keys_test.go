package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

// keyJSON mirrors the keyView/keyCreateResponse wire shape (Key is empty
// except on create).
type keyJSON struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	KeyHash        string   `json:"key_hash"`
	Models         []string `json:"models"`
	MaxBudget      *float64 `json:"max_budget"`
	BudgetDuration string   `json:"budget_duration"`
	ExpiresAt      *int64   `json:"expires_at"`
	Disabled       bool     `json:"disabled"`
	CreatedAt      int64    `json:"created_at"`
	Spend          *float64 `json:"spend"`
	Key            string   `json:"key"`
}

func keyReq(t *testing.T, ts *httptest.Server, method, path, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, ts.URL+path, nil)
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

func decodeKeyJSON(t *testing.T, resp *http.Response) keyJSON {
	t.Helper()
	var k keyJSON
	if err := json.NewDecoder(resp.Body).Decode(&k); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return k
}

func TestCreateKey_RoundTrip(t *testing.T) {
	ts, st := newTestServer(t, gpt4Deps())

	body := map[string]any{
		"name":            "ci",
		"models":          []string{"gpt-4"},
		"max_budget":      50.0,
		"budget_duration": "24h",
		"duration":        "720h",
		"metadata":        map[string]any{"team": "core"},
	}
	resp := mustPost(t, ts, "/v1/keys", body, testToken)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status: got %d, want 201", resp.StatusCode)
	}
	created := decodeKeyJSON(t, resp)
	resp.Body.Close()

	if !strings.HasPrefix(created.Key, "sk-am-") {
		t.Errorf("minted token %q lacks sk-am- prefix", created.Key)
	}
	if created.KeyHash != sha256hex(created.Key) {
		t.Errorf("key_hash does not match sha256(token)")
	}
	if created.MaxBudget == nil || *created.MaxBudget != 50.0 {
		t.Errorf("max_budget: got %v, want 50", created.MaxBudget)
	}
	if created.ExpiresAt == nil {
		t.Error("expires_at: got nil, want ~now+720h")
	}

	// The minted token authenticates and is attributed by its hash.
	chat := mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), created.Key)
	if chat.StatusCode != http.StatusOK {
		t.Fatalf("minted key auth: got %d, want 200", chat.StatusCode)
	}
	chat.Body.Close()

	// Hash persisted exactly as request_logs records it.
	if _, err := st.GetKeyByHash(context.Background(), created.KeyHash); err != nil {
		t.Fatalf("GetKeyByHash for created key: %v", err)
	}

	// Info endpoint reports the key plus spend (never the plaintext).
	info := keyReq(t, ts, "GET", "/v1/keys/"+created.ID, testToken)
	if info.StatusCode != http.StatusOK {
		t.Fatalf("info status: got %d, want 200", info.StatusCode)
	}
	got := decodeKeyJSON(t, info)
	info.Body.Close()
	if got.Key != "" {
		t.Error("info endpoint leaked the plaintext token")
	}
	if got.Spend == nil {
		t.Error("info endpoint omitted spend")
	}
}

func TestCreateKey_InvalidJSON(t *testing.T) {
	ts, _ := newTestServer(t, gpt4Deps())
	req, _ := http.NewRequestWithContext(context.Background(), "POST", ts.URL+"/v1/keys", strings.NewReader("{not json"))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", resp.StatusCode)
	}
}

func TestCreateKey_Validation(t *testing.T) {
	ts, _ := newTestServer(t, gpt4Deps())
	cases := []struct {
		name string
		body map[string]any
	}{
		{"missing name", map[string]any{}},
		{"blank name", map[string]any{"name": "   "}},
		{"non-positive budget", map[string]any{"name": "k", "max_budget": 0}},
		{"negative budget", map[string]any{"name": "k", "max_budget": -5}},
		{"bad budget_duration", map[string]any{"name": "k", "budget_duration": "nope"}},
		{"bad duration", map[string]any{"name": "k", "duration": "nope"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := mustPost(t, ts, "/v1/keys", c.body, testToken)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status: got %d, want 400", resp.StatusCode)
			}
		})
	}
}

func TestListKeys(t *testing.T) {
	ts, st := newTestServer(t, gpt4Deps())
	mustCreateKey(t, st, store.ManagedKey{ID: "k1", Name: "one", KeyHash: sha256hex("sk-am-1")})
	mustCreateKey(t, st, store.ManagedKey{ID: "k2", Name: "two", KeyHash: sha256hex("sk-am-2")})

	resp := keyReq(t, ts, "GET", "/v1/keys", testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	var out struct {
		Object string    `json:"object"`
		Data   []keyJSON `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Data) != 2 {
		t.Fatalf("list len: got %d, want 2", len(out.Data))
	}
	for _, k := range out.Data {
		if k.Key != "" {
			t.Error("list leaked a plaintext token")
		}
	}
}

func TestGetKey_NotFound(t *testing.T) {
	ts, _ := newTestServer(t, gpt4Deps())
	resp := keyReq(t, ts, "GET", "/v1/keys/ghost", testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", resp.StatusCode)
	}
}

func TestGetKey_ReportsSpend(t *testing.T) {
	ts, st := newTestServer(t, gpt4Deps())
	mustCreateKey(t, st, store.ManagedKey{ID: "k-spend", Name: "s", KeyHash: sha256hex("sk-am-spend")})
	seedSpend(t, st, sha256hex("sk-am-spend"), 3.25, time.Now().UTC())

	resp := keyReq(t, ts, "GET", "/v1/keys/k-spend", testToken)
	defer resp.Body.Close()
	got := decodeKeyJSON(t, resp)
	if got.Spend == nil || *got.Spend != 3.25 {
		t.Errorf("spend: got %v, want 3.25", got.Spend)
	}
}

func TestRevokeUnrevoke(t *testing.T) {
	ts, st := newTestServer(t, gpt4Deps())
	const tok = "sk-am-toggle"
	mustCreateKey(t, st, store.ManagedKey{ID: "k-tog", Name: "tog", KeyHash: sha256hex(tok)})

	// Initially authenticates.
	if resp := mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), tok); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("pre-revoke auth: got %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// Revoke → no longer authenticates.
	rv := keyReq(t, ts, "POST", "/v1/keys/k-tog/revoke", testToken)
	if rv.StatusCode != http.StatusOK {
		t.Fatalf("revoke status: got %d, want 200", rv.StatusCode)
	}
	if got := decodeKeyJSON(t, rv); !got.Disabled {
		t.Error("revoke: disabled not set")
	}
	rv.Body.Close()
	if resp := mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), tok); resp.StatusCode != http.StatusUnauthorized {
		resp.Body.Close()
		t.Fatalf("revoked auth: got %d, want 401", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// Unrevoke → authenticates again.
	urv := keyReq(t, ts, "POST", "/v1/keys/k-tog/unrevoke", testToken)
	urv.Body.Close()
	if urv.StatusCode != http.StatusOK {
		t.Fatalf("unrevoke status: got %d, want 200", urv.StatusCode)
	}
	if resp := mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), tok); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("unrevoked auth: got %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

func TestDeleteKey(t *testing.T) {
	ts, st := newTestServer(t, gpt4Deps())
	const tok = "sk-am-del"
	mustCreateKey(t, st, store.ManagedKey{ID: "k-del", Name: "del", KeyHash: sha256hex(tok)})

	del := keyReq(t, ts, "DELETE", "/v1/keys/k-del", testToken)
	del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status: got %d, want 204", del.StatusCode)
	}
	// Gone from info and from auth.
	if info := keyReq(t, ts, "GET", "/v1/keys/k-del", testToken); info.StatusCode != http.StatusNotFound {
		info.Body.Close()
		t.Fatalf("post-delete info: got %d, want 404", info.StatusCode)
	} else {
		info.Body.Close()
	}
	if resp := mustPost(t, ts, "/v1/chat/completions", chatBody("gpt-4"), tok); resp.StatusCode != http.StatusUnauthorized {
		resp.Body.Close()
		t.Fatalf("deleted key auth: got %d, want 401", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// Deleting again → 404.
	if again := keyReq(t, ts, "DELETE", "/v1/keys/k-del", testToken); again.StatusCode != http.StatusNotFound {
		again.Body.Close()
		t.Errorf("re-delete: got %d, want 404", again.StatusCode)
	} else {
		again.Body.Close()
	}
}

// Every management endpoint is master-only: a valid tenant (managed) key is
// rejected 403, never allowed to mint or revoke credentials.
func TestKeyEndpoints_RequireMaster(t *testing.T) {
	ts, st := newTestServer(t, gpt4Deps())
	const tenant = "sk-am-tenant"
	mustCreateKey(t, st, store.ManagedKey{ID: "k-tenant", Name: "tenant", KeyHash: sha256hex(tenant)})

	t.Run("POST /v1/keys", func(t *testing.T) {
		resp := mustPost(t, ts, "/v1/keys", map[string]any{"name": "x"}, tenant)
		defer resp.Body.Close()
		assertForbidden(t, resp)
	})
	reads := []struct{ method, path string }{
		{"GET", "/v1/keys"},
		{"GET", "/v1/keys/k-tenant"},
		{"POST", "/v1/keys/k-tenant/revoke"},
		{"POST", "/v1/keys/k-tenant/unrevoke"},
		{"DELETE", "/v1/keys/k-tenant"},
	}
	for _, rc := range reads {
		t.Run(rc.method+" "+rc.path, func(t *testing.T) {
			resp := keyReq(t, ts, rc.method, rc.path, tenant)
			defer resp.Body.Close()
			assertForbidden(t, resp)
		})
	}
}

func assertForbidden(t *testing.T, resp *http.Response) {
	t.Helper()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", resp.StatusCode)
	}
	typ, code, _ := errBody(t, resp)
	if typ != agentmodel.ErrTypePermissionDenied || code != agentmodel.CodeMasterRequired {
		t.Errorf("error: got (%s, %s), want permission/master_token_required", typ, code)
	}
}
