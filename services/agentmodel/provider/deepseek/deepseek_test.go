package deepseek_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/deepseek"
)

func newAuth(token string) *auth.StaticKey {
	return &auth.StaticKey{HeaderName: "Authorization", Prefix: "Bearer ", Token: token}
}

func newClient(t *testing.T, srv *httptest.Server) *deepseek.Client {
	t.Helper()
	return deepseek.NewWithBaseURL(newAuth("sk-test"), srv.URL)
}

func TestName(t *testing.T) {
	if got := deepseek.New(newAuth("sk")).Name(); got != "deepseek" {
		t.Errorf("Name() = %q, want deepseek", got)
	}
}

func TestAuthMode(t *testing.T) {
	if got := deepseek.New(newAuth("sk")).AuthMode(); got != agentmodel.AuthModeAPIKey {
		t.Errorf("AuthMode() = %q, want %q", got, agentmodel.AuthModeAPIKey)
	}
}

func TestSupportedModels(t *testing.T) {
	got := map[string]bool{}
	for _, m := range deepseek.New(newAuth("sk")).SupportedModels() {
		got[m] = true
	}
	for _, want := range []string{"deepseek-chat", "deepseek-reasoner"} {
		if !got[want] {
			t.Errorf("SupportedModels() missing %q", want)
		}
	}
}

// TestComplete confirms DeepSeek's OpenAI-compatible chat path works through the
// delegated openai shaping, including the reasoning_content field that
// deepseek-reasoner returns alongside content.
func TestComplete(t *testing.T) {
	var gotPath, gotAuth string
	var sent agentmodel.ChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&sent)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "x", "object": "chat.completion", "created": 1, "model": "deepseek-reasoner",
			"choices": [{"index": 0, "message": {"role": "assistant", "content": "42", "reasoning_content": "let me think"}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`))
	}))
	defer srv.Close()

	resp, err := newClient(t, srv).Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "deepseek-reasoner",
		Messages: []agentmodel.Message{{Role: "user", Content: "what is 6*7?"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("auth header = %q, want Bearer sk-test", gotAuth)
	}
	if sent.Model != "deepseek-reasoner" {
		t.Errorf("sent model = %q", sent.Model)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "42" {
		t.Fatalf("unexpected content: %+v", resp.Choices)
	}
	if resp.Choices[0].Message.ReasoningContent != "let me think" {
		t.Errorf("reasoning_content = %q, want 'let me think'", resp.Choices[0].Message.ReasoningContent)
	}
}

func TestStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"hmm\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"42\"},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	seq, err := newClient(t, srv).Stream(context.Background(), agentmodel.ChatRequest{
		Model:    "deepseek-reasoner",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var content, reasoning string
	var usageSeen bool
	for chunk, err := range seq {
		if err != nil {
			t.Fatalf("stream chunk error: %v", err)
		}
		content += chunk.Delta.Content
		reasoning += chunk.Delta.ReasoningContent
		if chunk.Usage != nil {
			usageSeen = true
		}
	}
	if content != "42" {
		t.Errorf("content = %q, want 42", content)
	}
	if reasoning != "hmm" {
		t.Errorf("reasoning = %q, want hmm", reasoning)
	}
	if !usageSeen {
		t.Error("expected a usage chunk")
	}
}

func TestEmbed_NotSupported(t *testing.T) {
	_, err := deepseek.New(newAuth("sk")).Embed(context.Background(), agentmodel.EmbeddingRequest{
		Model: "deepseek-chat", Input: []string{"hi"},
	})
	if !errors.Is(err, provider.ErrNotSupported) {
		t.Fatalf("Embed err = %v, want provider.ErrNotSupported", err)
	}
}

func TestListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"deepseek-chat"},{"id":"deepseek-reasoner"}]}`))
	}))
	defer srv.Close()

	ids, err := newClient(t, srv).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(ids) != 2 || ids[0] != "deepseek-chat" {
		t.Fatalf("ListModels = %v", ids)
	}
}

// TestDoesNotImplementImageGenerator guards the deliberate choice to compose
// (not embed) the openai client: embedding would promote GenerateImage and make
// DeepSeek falsely advertise image generation. DeepSeek is chat-only.
func TestDoesNotImplementImageGenerator(t *testing.T) {
	var p provider.Provider = deepseek.New(newAuth("sk"))
	if _, ok := p.(provider.ImageGenerator); ok {
		t.Error("deepseek.Client must NOT implement provider.ImageGenerator (it is chat-only)")
	}
}

// compile-time assertions mirroring the provider's own declarations.
var (
	_ provider.Provider    = (*deepseek.Client)(nil)
	_ provider.ModelLister = (*deepseek.Client)(nil)
)
