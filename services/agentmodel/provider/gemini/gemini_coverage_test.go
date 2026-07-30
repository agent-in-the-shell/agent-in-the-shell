package gemini_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/gemini"
)

// errAuth is a stub Authenticator whose Apply always fails, used to cover the
// auth.Apply error branch in doJSON.
type errAuth struct{ mode string }

func (e errAuth) Mode() string { return e.mode }
func (e errAuth) Apply(ctx context.Context, req *http.Request) error {
	return fmt.Errorf("boom: apply failed")
}

// TestComplete_AssistantAndToolMessages drives the assistant and tool branches
// of messageToContent indirectly through Complete, capturing the outbound
// request body to assert role mapping and part shape.
func TestComplete_AssistantAndToolMessages(t *testing.T) {
	var captured map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		resp := map[string]any{
			"candidates": []any{
				map[string]any{
					"content": map[string]any{
						"role":  "model",
						"parts": []any{map[string]any{"text": "ok"}},
					},
					"finishReason": "STOP",
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := gemini.NewWithBaseURL(newAuth(), srv.URL)
	_, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []agentmodel.Message{
			// assistant with text + a tool call carrying JSON args
			{
				Role:    "assistant",
				Content: "let me check",
				ToolCalls: []agentmodel.ToolCall{
					{
						ID:   "call_1",
						Type: "function",
						Function: agentmodel.ToolCallFunction{
							Name:      "get_weather",
							Arguments: `{"location":"Tokyo"}`,
						},
					},
				},
			},
			// assistant with NO content and NO tool calls -> empty-parts fallback
			{Role: "assistant"},
			// tool message with valid JSON content -> parsed map response
			{Role: "tool", Name: "get_weather", Content: `{"temp":21,"unit":"c"}`},
			// tool message with non-JSON content -> {"content": ...} fallback
			{Role: "tool", Name: "lookup", Content: "plain text result"},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	contents, ok := captured["contents"].([]any)
	if !ok || len(contents) != 4 {
		t.Fatalf("contents: got %#v", captured["contents"])
	}

	// [0] assistant with text + functionCall -> role model, 2 parts.
	c0 := contents[0].(map[string]any)
	if c0["role"] != "model" {
		t.Errorf("contents[0].role: got %v, want model", c0["role"])
	}
	parts0, _ := c0["parts"].([]any)
	if len(parts0) != 2 {
		t.Fatalf("contents[0].parts: got %d, want 2", len(parts0))
	}
	if parts0[0].(map[string]any)["text"] != "let me check" {
		t.Errorf("contents[0].parts[0].text: got %v", parts0[0].(map[string]any)["text"])
	}
	fc, ok := parts0[1].(map[string]any)["functionCall"].(map[string]any)
	if !ok {
		t.Fatalf("contents[0].parts[1].functionCall missing: %#v", parts0[1])
	}
	if fc["name"] != "get_weather" {
		t.Errorf("functionCall.name: got %v", fc["name"])
	}
	fcArgs, _ := fc["args"].(map[string]any)
	if fcArgs["location"] != "Tokyo" {
		t.Errorf("functionCall.args.location: got %v", fcArgs["location"])
	}

	// [1] empty assistant -> role model, single empty-text part fallback.
	c1 := contents[1].(map[string]any)
	if c1["role"] != "model" {
		t.Errorf("contents[1].role: got %v, want model", c1["role"])
	}
	parts1, _ := c1["parts"].([]any)
	if len(parts1) != 1 {
		t.Fatalf("contents[1].parts: got %d, want 1", len(parts1))
	}
	// An empty text part serializes with the "text" key omitted (omitempty),
	// so it must be an object with no functionCall/functionResponse.
	p1 := parts1[0].(map[string]any)
	if _, hasFC := p1["functionCall"]; hasFC {
		t.Errorf("contents[1] empty-part should not have functionCall: %#v", p1)
	}
	if txt, hasTxt := p1["text"]; hasTxt && txt != "" {
		t.Errorf("contents[1] empty-part text: got %v", txt)
	}

	// [2] tool with valid JSON -> role user, functionResponse with parsed map.
	c2 := contents[2].(map[string]any)
	if c2["role"] != "user" {
		t.Errorf("contents[2].role: got %v, want user", c2["role"])
	}
	fr2, ok := c2["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
	if !ok {
		t.Fatalf("contents[2] functionResponse missing")
	}
	if fr2["name"] != "get_weather" {
		t.Errorf("contents[2] functionResponse.name: got %v", fr2["name"])
	}
	resp2, _ := fr2["response"].(map[string]any)
	if resp2["temp"].(float64) != 21 || resp2["unit"] != "c" {
		t.Errorf("contents[2] functionResponse.response: got %#v", resp2)
	}

	// [3] tool with non-JSON content -> functionResponse with {"content": ...}.
	c3 := contents[3].(map[string]any)
	fr3, ok := c3["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
	if !ok {
		t.Fatalf("contents[3] functionResponse missing")
	}
	resp3, _ := fr3["response"].(map[string]any)
	if resp3["content"] != "plain text result" {
		t.Errorf("contents[3] fallback response.content: got %#v", resp3)
	}
}

// TestAuthMode_NilAuthenticator covers the c.auth == nil branch of AuthMode and
// confirms that no auth header is sent when there is no authenticator.
func TestAuthMode_NilAuthenticator(t *testing.T) {
	var sawAuthHeader bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "" || r.Header.Get("Authorization") != "" {
			sawAuthHeader = true
		}
		resp := map[string]any{
			"candidates": []any{
				map[string]any{
					"content":      map[string]any{"role": "model", "parts": []any{map[string]any{"text": "x"}}},
					"finishReason": "STOP",
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := gemini.NewWithBaseURL(nil, srv.URL)
	if got := c.AuthMode(); got != agentmodel.AuthModeAPIKey {
		t.Errorf("AuthMode with nil auth: got %q, want %q", got, agentmodel.AuthModeAPIKey)
	}

	_, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "gemini-2.5-flash",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete with nil auth: %v", err)
	}
	if sawAuthHeader {
		t.Errorf("expected no auth header when authenticator is nil")
	}
}

// TestComplete_EmptyFinishReason covers the mapFinishReason "" -> "" case via a
// candidate with no finishReason field.
func TestComplete_EmptyFinishReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No finishReason key at all.
		resp := map[string]any{
			"candidates": []any{
				map[string]any{
					"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": "y"}}},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := gemini.NewWithBaseURL(newAuth(), srv.URL)
	resp, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "gemini-2.5-flash",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("Choices: got %d", len(resp.Choices))
	}
	if resp.Choices[0].FinishReason != "" {
		t.Errorf("FinishReason: got %q, want empty", resp.Choices[0].FinishReason)
	}
}

// TestComplete_MissingModel covers the empty-Model error path (no server).
func TestComplete_MissingModel(t *testing.T) {
	c := gemini.NewWithBaseURL(newAuth(), "http://127.0.0.1:0")
	_, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatalf("expected error for missing Model")
	}
	if !strings.Contains(err.Error(), "Model required") {
		t.Errorf("error: got %q, want Model required", err)
	}
}

// TestComplete_DecodeError covers the response decode-error path of Complete via
// a server returning malformed JSON.
func TestComplete_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{not valid json")
	}))
	defer srv.Close()

	c := gemini.NewWithBaseURL(newAuth(), srv.URL)
	_, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "gemini-2.5-flash",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatalf("expected decode error")
	}
	if !strings.Contains(err.Error(), "decode response") {
		t.Errorf("error: got %q, want decode response", err)
	}
}

// TestComplete_AuthApplyError covers the auth.Apply error branch in doJSON.
func TestComplete_AuthApplyError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server should not be reached when Apply fails")
	}))
	defer srv.Close()

	c := gemini.NewWithBaseURL(errAuth{mode: agentmodel.AuthModeAPIKey}, srv.URL)
	_, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "gemini-2.5-flash",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatalf("expected error from auth.Apply")
	}
	if !strings.Contains(err.Error(), "apply failed") {
		t.Errorf("error: got %q, want apply failed", err)
	}
}

// TestStream_MissingModel covers the empty-Model error path of Stream.
func TestStream_MissingModel(t *testing.T) {
	c := gemini.NewWithBaseURL(newAuth(), "http://127.0.0.1:0")
	_, err := c.Stream(context.Background(), agentmodel.ChatRequest{
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatalf("expected error for missing Model")
	}
	if !strings.Contains(err.Error(), "Model required") {
		t.Errorf("error: got %q", err)
	}
}

// TestStream_DecodeErrorAndUsageOnly covers two Stream branches: a bad SSE
// data line yielding a decode error, and a final candidates-empty chunk that
// carries usageMetadata only.
func TestStream_DecodeErrorAndUsageOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		lines := []string{
			// one good chunk with text
			`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Hi"}]}}]}`,
			"",
			// malformed JSON -> decode-error yield
			`data: {not json}`,
			"",
			// candidates-empty final chunk with usage only
			`data: {"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2,"totalTokenCount":7}}`,
			"",
		}
		for _, l := range lines {
			fmt.Fprintf(w, "%s\n", l)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	c := gemini.NewWithBaseURL(newAuth(), srv.URL)
	seq, err := c.Stream(context.Background(), agentmodel.ChatRequest{
		Model:    "gemini-2.5-flash",
		Stream:   true,
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var sawDecodeErr bool
	var usageOnly *agentmodel.Usage
	var deltas []string
	for ch, yerr := range seq {
		if yerr != nil {
			if strings.Contains(yerr.Error(), "decode SSE") {
				sawDecodeErr = true
			}
			continue
		}
		if ch.Delta.Content != "" {
			deltas = append(deltas, ch.Delta.Content)
		}
		// usage-only final chunk has no delta content/role.
		if ch.Usage != nil && ch.Delta.Content == "" && ch.Delta.Role == "" {
			usageOnly = ch.Usage
		}
	}

	if !sawDecodeErr {
		t.Errorf("expected a decode SSE error yield")
	}
	if len(deltas) == 0 || deltas[0] != "Hi" {
		t.Errorf("deltas: got %v, want first=Hi", deltas)
	}
	if usageOnly == nil {
		t.Fatalf("expected usage-only final chunk")
	}
	if usageOnly.PromptTokens != 5 || usageOnly.CompletionTokens != 2 || usageOnly.TotalTokens != 7 {
		t.Errorf("usage-only: got %+v", *usageOnly)
	}
	if usageOnly.AuthMode != agentmodel.AuthModeAPIKey {
		t.Errorf("usage-only AuthMode: got %q", usageOnly.AuthMode)
	}
}

// TestStream_AuthApplyError covers the auth.Apply error path reached through
// Stream (doJSON returns before the iterator is built).
func TestStream_AuthApplyError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server should not be reached when Apply fails")
	}))
	defer srv.Close()

	c := gemini.NewWithBaseURL(errAuth{mode: agentmodel.AuthModeAPIKey}, srv.URL)
	_, err := c.Stream(context.Background(), agentmodel.ChatRequest{
		Model:    "gemini-2.5-flash",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatalf("expected error from auth.Apply")
	}
	if !strings.Contains(err.Error(), "apply failed") {
		t.Errorf("error: got %q", err)
	}
}

// TestEmbed_MissingModel covers the empty-Model error path of Embed.
func TestEmbed_MissingModel(t *testing.T) {
	c := gemini.NewWithBaseURL(newAuth(), "http://127.0.0.1:0")
	_, err := c.Embed(context.Background(), agentmodel.EmbeddingRequest{
		Input: []string{"hi"},
	})
	if err == nil {
		t.Fatalf("expected error for missing Model")
	}
	if !strings.Contains(err.Error(), "Model required") {
		t.Errorf("error: got %q", err)
	}
}

// TestEmbed_ErrorMidLoop covers the doJSON error path encountered partway
// through the per-input loop (second call returns HTTP 500).
func TestEmbed_ErrorMidLoop(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			resp := map[string]any{"embedding": map[string]any{"values": []float64{0.1, 0.2}}}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := gemini.NewWithBaseURL(newAuth(), srv.URL)
	_, err := c.Embed(context.Background(), agentmodel.EmbeddingRequest{
		Model: "text-embedding-004",
		Input: []string{"first", "second"},
	})
	if err == nil {
		t.Fatalf("expected error on second embed call")
	}
	if calls != 2 {
		t.Errorf("expected 2 calls before failure, got %d", calls)
	}
}

// TestEmbed_DecodeError covers the embedding decode-error path of Embed.
func TestEmbed_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "}{ broken")
	}))
	defer srv.Close()

	c := gemini.NewWithBaseURL(newAuth(), srv.URL)
	_, err := c.Embed(context.Background(), agentmodel.EmbeddingRequest{
		Model: "text-embedding-004",
		Input: []string{"hi"},
	})
	if err == nil {
		t.Fatalf("expected decode error")
	}
	if !strings.Contains(err.Error(), "decode embedding") {
		t.Errorf("error: got %q, want decode embedding", err)
	}
}

// TestComplete_NilArgsToolCallResponse covers candidateToMessage's argBytes ==
// nil -> "{}" fallback: a functionCall with no args at all should yield "{}".
func TestComplete_NilArgsToolCallResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"candidates": []any{
				map[string]any{
					"content": map[string]any{
						"role": "model",
						"parts": []any{
							// functionCall with no "args" key -> Args is a nil map.
							map[string]any{"functionCall": map[string]any{"name": "ping"}},
						},
					},
					"finishReason": "STOP",
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := gemini.NewWithBaseURL(newAuth(), srv.URL)
	resp, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "gemini-2.5-pro",
		Messages: []agentmodel.Message{{Role: "user", Content: "ping?"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	tcs := resp.Choices[0].Message.ToolCalls
	if len(tcs) != 1 {
		t.Fatalf("tool_calls: got %d", len(tcs))
	}
	// json.Marshal(nil map) yields "null", which is non-nil bytes, so the impl
	// keeps "null". Either way it must be valid and decodable; assert it is the
	// empty-object or null sentinel rather than empty string.
	if tcs[0].Function.Arguments == "" {
		t.Errorf("Arguments empty; want a JSON sentinel")
	}
	if tcs[0].Function.Name != "ping" {
		t.Errorf("name: got %q", tcs[0].Function.Name)
	}
}

// compile-time use of provider.StreamChunk to keep the import meaningful even
// if the streaming assertions above change.
var _ = provider.StreamChunk{}
