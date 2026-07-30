package azure_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/azure"
)

// apiKeyAuth is the Azure auth shape: an api-key header, no Bearer prefix.
func apiKeyAuth(key string) *auth.StaticKey {
	return &auth.StaticKey{HeaderName: "api-key", Token: key}
}

func newClient(srv *httptest.Server) *azure.Client {
	return azure.New(apiKeyAuth("az-secret"), srv.URL, "2024-10-01-preview", "gpt4o-deploy")
}

func TestNameAndAuthMode(t *testing.T) {
	c := azure.New(apiKeyAuth("k"), "https://res.openai.azure.com", "2024-10-01-preview", "d")
	if c.Name() != "azure" {
		t.Errorf("Name() = %q, want azure", c.Name())
	}
	if c.AuthMode() != agentmodel.AuthModeAPIKey {
		t.Errorf("AuthMode() = %q, want %q", c.AuthMode(), agentmodel.AuthModeAPIKey)
	}
}

func TestSupportedModelsDelegatesToOpenAI(t *testing.T) {
	c := azure.New(apiKeyAuth("k"), "https://res.openai.azure.com", "2024-10-01-preview", "d")
	models := c.SupportedModels()
	got := map[string]bool{}
	for _, model := range models {
		got[model] = true
	}
	for _, want := range []string{"gpt-4o", "gpt-5", "text-embedding-3-small"} {
		if !got[want] {
			t.Errorf("SupportedModels() missing %q in %v", want, models)
		}
	}
}

// TestComplete confirms the Azure URL layout (deployment-scoped path +
// api-version query) and the api-key auth header, with chat shaping delegated to
// the openai provider.
func TestComplete(t *testing.T) {
	var gotPath, gotQuery, gotAPIKey, gotAuthz string
	var sent agentmodel.ChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAPIKey = r.Header.Get("api-key")
		gotAuthz = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&sent)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "x", "object": "chat.completion", "created": 1, "model": "gpt-4o",
			"choices": [{"index": 0, "message": {"role": "assistant", "content": "42"}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`))
	}))
	defer srv.Close()

	resp, err := newClient(srv).Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "gpt-4o",
		Messages: []agentmodel.Message{{Role: "user", Content: "what is 6*7?"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotPath != "/openai/deployments/gpt4o-deploy/chat/completions" {
		t.Errorf("path = %q, want /openai/deployments/gpt4o-deploy/chat/completions", gotPath)
	}
	if gotQuery != "api-version=2024-10-01-preview" {
		t.Errorf("query = %q, want api-version=2024-10-01-preview", gotQuery)
	}
	if gotAPIKey != "az-secret" {
		t.Errorf("api-key header = %q, want az-secret", gotAPIKey)
	}
	if gotAuthz != "" {
		t.Errorf("Authorization header = %q, want empty (Azure uses api-key)", gotAuthz)
	}
	if sent.Model != "gpt-4o" {
		t.Errorf("sent model = %q", sent.Model)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "42" {
		t.Fatalf("unexpected content: %+v", resp.Choices)
	}
	if resp.Usage.TotalTokens != 15 {
		t.Errorf("usage total = %d, want 15", resp.Usage.TotalTokens)
	}
}

func TestStream(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"4\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"2\"},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	seq, err := newClient(srv).Stream(context.Background(), agentmodel.ChatRequest{
		Model:    "gpt-4o",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var content string
	var usageSeen bool
	for chunk, err := range seq {
		if err != nil {
			t.Fatalf("stream chunk error: %v", err)
		}
		content += chunk.Delta.Content
		if chunk.Usage != nil {
			usageSeen = true
			if chunk.Usage.TotalTokens != 15 {
				t.Errorf("stream usage total = %d, want 15", chunk.Usage.TotalTokens)
			}
		}
	}
	if content != "42" {
		t.Errorf("streamed content = %q, want 42", content)
	}
	if !usageSeen {
		t.Error("never saw the terminal usage chunk")
	}
	if gotPath != "/openai/deployments/gpt4o-deploy/chat/completions" || gotQuery != "api-version=2024-10-01-preview" {
		t.Errorf("stream path/query = %q?%q, want deployment-scoped + api-version", gotPath, gotQuery)
	}
}

func TestEmbed(t *testing.T) {
	var gotPath, gotQuery, gotAPIKey string
	var sent agentmodel.EmbeddingRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAPIKey = r.Header.Get("api-key")
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Fatalf("decode embedding request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"object": "list",
			"data": [{"object": "embedding", "index": 0, "embedding": [0.1, 0.2, 0.3]}],
			"model": "text-embedding-3-small",
			"usage": {"prompt_tokens": 2, "total_tokens": 2}
		}`)
	}))
	defer srv.Close()

	resp, err := newClient(srv).Embed(context.Background(), agentmodel.EmbeddingRequest{
		Model: "text-embedding-3-small",
		Input: []string{"hello"},
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if gotPath != "/openai/deployments/gpt4o-deploy/embeddings" {
		t.Errorf("path = %q, want /openai/deployments/gpt4o-deploy/embeddings", gotPath)
	}
	if gotQuery != "api-version=2024-10-01-preview" {
		t.Errorf("query = %q, want api-version=2024-10-01-preview", gotQuery)
	}
	if gotAPIKey != "az-secret" {
		t.Errorf("api-key header = %q, want az-secret", gotAPIKey)
	}
	if sent.Model != "text-embedding-3-small" || len(sent.Input) != 1 || sent.Input[0] != "hello" {
		t.Fatalf("sent request = %+v", sent)
	}
	if len(resp.Data) != 1 || len(resp.Data[0].Embedding) != 3 {
		t.Fatalf("embedding response = %+v", resp)
	}
	if resp.Usage.TotalTokens != 2 {
		t.Errorf("usage total = %d, want 2", resp.Usage.TotalTokens)
	}
}

func TestListModels(t *testing.T) {
	var gotPath, gotQuery, gotAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAPIKey = r.Header.Get("api-key")
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"gpt-4o"},{"id":"gpt-5.5"}]}`)
	}))
	defer srv.Close()

	models, err := newClient(srv).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if gotPath != "/openai/models" {
		t.Errorf("path = %q, want /openai/models", gotPath)
	}
	if gotQuery != "api-version=2024-10-01-preview" {
		t.Errorf("query = %q, want api-version=2024-10-01-preview", gotQuery)
	}
	if gotAPIKey != "az-secret" {
		t.Errorf("api-key header = %q, want az-secret", gotAPIKey)
	}
	if len(models) != 2 || models[0] != "gpt-4o" || models[1] != "gpt-5.5" {
		t.Fatalf("models = %v, want [gpt-4o gpt-5.5]", models)
	}
}
