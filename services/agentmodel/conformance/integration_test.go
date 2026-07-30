package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/api"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/cost"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/stub"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/wire"
)

const integrationToken = "integration-test-token"

type integrationFixture struct {
	server *httptest.Server
	store  *store.SQLiteStore
}

func setupIntegration(t *testing.T) *integrationFixture {
	t.Helper()

	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "integration.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	registry, err := cost.LoadDefault()
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}

	apiKeyStub := &stub.Stub{NameValue: "openai"}
	subStub := &stub.Stub{NameValue: "chatgpt", AuthModeValue: wire.AuthModeSubscription}

	deployments := map[string][]router.Deployment{
		"gpt-4": {
			{Provider: apiKeyStub, Model: "gpt-4o", Weight: 100},
		},
		"chatgpt-pro": {
			{Provider: subStub, Model: "gpt-5", Weight: 100},
		},
	}
	rt := router.New(deployments, nil)

	srv := api.New(api.Config{
		Router:      rt,
		Store:       st,
		Registry:    registry,
		BearerToken: integrationToken,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &integrationFixture{server: ts, store: st}
}

func (f *integrationFixture) post(t *testing.T, path string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(context.Background(), "POST", f.server.URL+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+integrationToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", path, err)
	}
	return resp
}

// Scenario 1: end-to-end non-streaming chat completion with audit log.
func TestIntegration_NonStreamingChatWithAuditLog(t *testing.T) {
	f := setupIntegration(t)

	resp := f.post(t, "/v1/chat/completions", wire.ChatRequest{
		Model:    "gpt-4",
		Messages: []wire.Message{{Role: "user", Content: "hello"}},
	})
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 200; body: %s", resp.StatusCode, body)
	}

	var got wire.ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Model != "gpt-4o" {
		t.Errorf("Model: got %q, want gpt-4o (upstream)", got.Model)
	}
	if got.Usage.AuthMode != wire.AuthModeAPIKey {
		t.Errorf("AuthMode: got %q, want api_key", got.Usage.AuthMode)
	}

	logs, _ := f.store.ListByOrg(context.Background(), "default", time.Now().Add(-time.Minute), 10)
	if len(logs) != 1 {
		t.Fatalf("audit logs: got %d, want 1", len(logs))
	}
	if logs[0].AuthMode != wire.AuthModeAPIKey || logs[0].Status != "ok" {
		t.Errorf("log: got %+v", logs[0])
	}
	if logs[0].LatencyMs < 0 {
		t.Errorf("LatencyMs: got %d, want >= 0", logs[0].LatencyMs)
	}
}

// Scenario 2: streaming chat returns SSE chunks with usage and audit log.
func TestIntegration_StreamingChatWithUsage(t *testing.T) {
	f := setupIntegration(t)

	resp := f.post(t, "/v1/chat/completions", wire.ChatRequest{
		Model:    "gpt-4",
		Messages: []wire.Message{{Role: "user", Content: "hi"}},
		Stream:   true,
	})
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	if !strings.Contains(bodyStr, "data: ") {
		t.Errorf("body missing SSE data lines: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "data: [DONE]") {
		t.Errorf("body missing [DONE] terminator: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, `"object":"chat.completion.chunk"`) {
		t.Errorf("body missing chunk object: %s", bodyStr)
	}
}

// Scenario 3: subscription model returns CostUSD = 0; audit log records authmode.
func TestIntegration_SubscriptionPath(t *testing.T) {
	f := setupIntegration(t)

	resp := f.post(t, "/v1/chat/completions", wire.ChatRequest{
		Model:    "chatgpt-pro",
		Messages: []wire.Message{{Role: "user", Content: "hello"}},
	})
	defer resp.Body.Close()

	var got wire.ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Usage.CostUSD != 0 {
		t.Errorf("CostUSD: got %v, want 0 for subscription", got.Usage.CostUSD)
	}
	if got.Usage.AuthMode != wire.AuthModeSubscription {
		t.Errorf("AuthMode: got %q, want subscription", got.Usage.AuthMode)
	}

	logs, _ := f.store.ListByOrg(context.Background(), "default", time.Now().Add(-time.Minute), 10)
	if len(logs) != 1 {
		t.Fatalf("audit logs: got %d, want 1", len(logs))
	}
	if logs[0].AuthMode != wire.AuthModeSubscription {
		t.Errorf("audit log AuthMode: got %q, want subscription", logs[0].AuthMode)
	}
	if logs[0].CostUSD != 0 {
		t.Errorf("audit log CostUSD: got %v, want 0", logs[0].CostUSD)
	}
}

// Scenario 4: 100 concurrent requests across mixed auth modes.
func TestIntegration_ConcurrentMixedAuthModes(t *testing.T) {
	f := setupIntegration(t)
	const N = 100

	var wg sync.WaitGroup
	errs := make(chan error, N)

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			model := "gpt-4"
			if i%3 == 0 {
				model = "chatgpt-pro"
			}
			resp := f.post(t, "/v1/chat/completions", wire.ChatRequest{
				Model:    model,
				Messages: []wire.Message{{Role: "user", Content: "hi"}},
			})
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				body, _ := io.ReadAll(resp.Body)
				errs <- fmt.Errorf("req %d (%s): status %d, body %s", i, model, resp.StatusCode, body)
				return
			}
			io.Copy(io.Discard, resp.Body)
		}(i)
	}
	wg.Wait()
	close(errs)

	for e := range errs {
		t.Error(e)
	}

	logs, _ := f.store.ListByOrg(context.Background(), "default", time.Now().Add(-time.Minute), 200)
	if len(logs) != N {
		t.Errorf("audit logs: got %d, want %d", len(logs), N)
	}

	// Verify both auth modes recorded.
	apiKeyCount, _ := f.store.CountRequestsByOrg(context.Background(), "default", time.Now().Add(-time.Minute), wire.AuthModeAPIKey)
	subCount, _ := f.store.CountRequestsByOrg(context.Background(), "default", time.Now().Add(-time.Minute), wire.AuthModeSubscription)
	if apiKeyCount == 0 || subCount == 0 {
		t.Errorf("expected mixed auth modes, got api_key=%d sub=%d", apiKeyCount, subCount)
	}
	if apiKeyCount+subCount != int64(N) {
		t.Errorf("total: got %d, want %d", apiKeyCount+subCount, N)
	}
}

// Scenario 5: GET /v1/models returns the configured logical model_names
// (the routable surface), not the cost price catalog.
func TestIntegration_ListModels(t *testing.T) {
	f := setupIntegration(t)

	req, _ := http.NewRequest("GET", f.server.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+integrationToken)
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	var got struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&got)
	if got.Object != "list" {
		t.Errorf("Object: got %q, want list", got.Object)
	}
	// setupIntegration configures exactly these two logical models.
	owners := map[string]string{}
	for _, m := range got.Data {
		owners[m.ID] = m.OwnedBy
	}
	if owners["gpt-4"] != "openai" {
		t.Errorf("gpt-4 owned_by: got %q, want openai (owners=%v)", owners["gpt-4"], owners)
	}
	if owners["chatgpt-pro"] != "chatgpt" {
		t.Errorf("chatgpt-pro owned_by: got %q, want chatgpt (owners=%v)", owners["chatgpt-pro"], owners)
	}
	if len(got.Data) != 2 {
		t.Errorf("model count: got %d, want 2 (the configured model_names, not the price catalog): %v", len(got.Data), owners)
	}
}

// Scenario 6 (cache): cache fields plumbed end-to-end through the gateway.
func TestIntegration_CacheTokensPlumbedThrough(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	registry, _ := cost.LoadDefault()

	// Configure stub to return a response with cache breakdown matching what
	// Anthropic Sonnet would surface for a long cached prompt.
	cachedStub := &stub.Stub{
		NameValue: "anthropic",
		CompleteResp: wire.ChatResponse{
			ID:     "stub-cached",
			Object: "chat.completion",
			Model:  "stub-model",
			Choices: []wire.Choice{{
				Index:        0,
				Message:      wire.Message{Role: "assistant", Content: "cached reply"},
				FinishReason: "stop",
			}},
			Usage: wire.Usage{
				PromptTokens:             12500, // 10000 fresh + 2000 creation + 500 read
				CompletionTokens:         200,
				TotalTokens:              12700,
				CacheReadInputTokens:     500,
				CacheCreationInputTokens: 2000,
			},
		},
	}

	deployments := map[string][]router.Deployment{
		// Map logical "claude" to the anthropic Sonnet entry in registry so
		// cost.Calculate uses real Sonnet rates (input $3/M, cache_read $0.30/M,
		// cache_creation $3.75/M).
		"claude": {{Provider: cachedStub, Model: "anthropic/claude-3-5-sonnet-latest", Weight: 100}},
	}

	srv := api.New(api.Config{
		Router:      router.New(deployments, nil),
		Store:       st,
		Registry:    registry,
		BearerToken: integrationToken,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	body, _ := json.Marshal(wire.ChatRequest{
		Model:    "claude",
		Messages: []wire.Message{{Role: "user", Content: "hi"}},
	})
	req, _ := http.NewRequestWithContext(context.Background(), "POST", ts.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+integrationToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	var got wire.ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Cache fields surface in the API response.
	if got.Usage.CacheReadInputTokens != 500 {
		t.Errorf("CacheReadInputTokens: got %d, want 500", got.Usage.CacheReadInputTokens)
	}
	if got.Usage.CacheCreationInputTokens != 2000 {
		t.Errorf("CacheCreationInputTokens: got %d, want 2000", got.Usage.CacheCreationInputTokens)
	}

	// Cost reflects the cache split (Sonnet rates):
	//   uncached    = 10000 * $3.00/M       = 0.030000
	//   cache_read  = 500   * $0.30/M       = 0.000150
	//   cache_write = 2000  * $3.75/M       = 0.007500
	//   completion  = 200   * $15.00/M      = 0.003000
	//   total       =                          0.040650
	want := 0.04065
	if got.Usage.CostUSD < want-1e-6 || got.Usage.CostUSD > want+1e-6 {
		t.Errorf("CostUSD: got %.10f, want %.10f", got.Usage.CostUSD, want)
	}

	// Cache columns persist in audit log.
	logs, err := st.ListByOrg(context.Background(), "default", time.Now().Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("audit logs: got %d, want 1", len(logs))
	}
	if logs[0].CacheReadInputTokens != 500 || logs[0].CacheCreationInputTokens != 2000 {
		t.Errorf("audit log cache fields: read=%d creation=%d, want 500/2000",
			logs[0].CacheReadInputTokens, logs[0].CacheCreationInputTokens)
	}
}

// Scenario 7: bearer auth required everywhere on /v1/*.
func TestIntegration_AuthRequired(t *testing.T) {
	f := setupIntegration(t)

	endpoints := []string{
		"/v1/chat/completions",
		"/v1/embeddings",
		"/v1/models",
		"/v1/limits",
	}
	for _, ep := range endpoints {
		t.Run(ep, func(t *testing.T) {
			req, _ := http.NewRequest("GET", f.server.URL+ep, nil)
			resp, err := f.server.Client().Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status: got %d, want 401", resp.StatusCode)
			}
		})
	}

	// Health endpoint must NOT require auth.
	resp, err := http.Get(f.server.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("/healthz unauthenticated: got %d, want 200", resp.StatusCode)
	}
}
