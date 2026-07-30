package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
)

// compile-time assertion: Client satisfies provider.ImageGenerator.
var _ provider.ImageGenerator = (*Client)(nil)

func TestGenerateImage(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"created": 1700000000,
			"data": [{"b64_json": "aGVsbG8=", "revised_prompt": "a cat, refined"}],
			"usage": {"input_tokens": 12, "output_tokens": 250, "total_tokens": 262}
		}`))
	}))
	defer srv.Close()

	c := newClient(t, srv, "sk-foo")
	resp, err := c.GenerateImage(context.Background(), agentmodel.ImageRequest{
		Model:  "gpt-image-1",
		Prompt: "a cat",
		N:      1,
		Size:   "1024x1024",
	})
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}

	if gotPath != "/images/generations" {
		t.Errorf("path = %q, want /images/generations", gotPath)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatalf("decode sent body: %v", err)
	}
	if sent["model"] != "gpt-image-1" || sent["prompt"] != "a cat" || sent["size"] != "1024x1024" {
		t.Errorf("unexpected request body: %s", gotBody)
	}

	if len(resp.Data) != 1 || resp.Data[0].B64JSON != "aGVsbG8=" {
		t.Fatalf("unexpected data: %+v", resp.Data)
	}
	if resp.Data[0].RevisedPrompt != "a cat, refined" {
		t.Errorf("revised prompt = %q", resp.Data[0].RevisedPrompt)
	}
	if resp.Usage.PromptTokens != 12 || resp.Usage.CompletionTokens != 250 || resp.Usage.TotalTokens != 262 {
		t.Errorf("usage mapping wrong: %+v", resp.Usage)
	}
	if resp.Model != "gpt-image-1" {
		t.Errorf("model = %q, want gpt-image-1", resp.Model)
	}
}

func TestGenerateImage_Validation(t *testing.T) {
	c := New(newAuth("sk-foo"))
	if _, err := c.GenerateImage(context.Background(), agentmodel.ImageRequest{Prompt: "x"}); err == nil {
		t.Error("expected error for missing model")
	}
	if _, err := c.GenerateImage(context.Background(), agentmodel.ImageRequest{Model: "gpt-image-1"}); err == nil {
		t.Error("expected error for missing prompt")
	}
}

func TestGenerateImage_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()

	c := newClient(t, srv, "sk-foo")
	if _, err := c.GenerateImage(context.Background(), agentmodel.ImageRequest{Model: "gpt-image-1", Prompt: "a cat"}); err == nil {
		t.Fatal("expected error on 401")
	}
}
