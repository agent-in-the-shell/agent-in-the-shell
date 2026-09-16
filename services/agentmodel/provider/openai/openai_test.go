package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
)

// compile-time assertion: Client satisfies provider.Provider.
var _ provider.Provider = (*Client)(nil)

// newAuth returns a StaticKey authenticator suitable for OpenAI.
func newAuth(token string) *auth.StaticKey {
	return &auth.StaticKey{HeaderName: "Authorization", Prefix: "Bearer ", Token: token}
}

// newClient builds a Client pointed at the test server's URL (no trailing slash).
func newClient(t *testing.T, srv *httptest.Server, token string) *Client {
	t.Helper()
	return NewWithBaseURL(newAuth(token), srv.URL)
}

func TestName(t *testing.T) {
	c := New(newAuth("sk-foo"))
	if c.Name() != "openai" {
		t.Errorf("Name() = %q, want %q", c.Name(), "openai")
	}
}

func TestAuthMode(t *testing.T) {
	c := New(newAuth("sk-foo"))
	if c.AuthMode() != agentmodel.AuthModeAPIKey {
		t.Errorf("AuthMode() = %q, want %q", c.AuthMode(), agentmodel.AuthModeAPIKey)
	}
}

func TestSupportedModels(t *testing.T) {
	c := New(newAuth("sk-foo"))
	models := c.SupportedModels()
	wanted := []string{"gpt-4o", "gpt-4o-mini", "gpt-5", "text-embedding-3-small", "text-embedding-3-large"}
	got := map[string]bool{}
	for _, m := range models {
		got[m] = true
	}
	for _, w := range wanted {
		if !got[w] {
			t.Errorf("SupportedModels() missing %q", w)
		}
	}
}

func TestComplete_Simple(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q, want %q", r.URL.Path, "/chat/completions")
		}
		if r.Method != "POST" {
			t.Errorf("method = %q, want POST", r.Method)
		}
		// Verify auth header
		got := r.Header.Get("Authorization")
		if got != "Bearer sk-foo" {
			t.Errorf("Authorization header = %q, want %q", got, "Bearer sk-foo")
		}
		// Verify request body
		body, _ := io.ReadAll(r.Body)
		var req agentmodel.ChatRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Model != "gpt-4o-mini" {
			t.Errorf("req.Model = %q, want gpt-4o-mini", req.Model)
		}
		if len(req.Messages) != 1 || req.Messages[0].Content != "hello" {
			t.Errorf("unexpected messages: %#v", req.Messages)
		}
		// Send canned OpenAI response
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id": "chatcmpl-abc",
			"object": "chat.completion",
			"created": 1700000000,
			"model": "gpt-4o-mini",
			"choices": [
				{
					"index": 0,
					"message": {"role": "assistant", "content": "hi there"},
					"finish_reason": "stop"
				}
			],
			"usage": {"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8}
		}`)
	}))
	defer srv.Close()

	c := newClient(t, srv, "sk-foo")
	resp, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []agentmodel.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.ID != "chatcmpl-abc" {
		t.Errorf("ID = %q, want chatcmpl-abc", resp.ID)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices len = %d, want 1", len(resp.Choices))
	}
	if resp.Choices[0].Message.Content != "hi there" {
		t.Errorf("content = %q, want hi there", resp.Choices[0].Message.Content)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish = %q, want stop", resp.Choices[0].FinishReason)
	}
	if resp.Usage.TotalTokens != 8 {
		t.Errorf("total tokens = %d, want 8", resp.Usage.TotalTokens)
	}
}

func TestComplete_ShortensLongToolCallIDs(t *testing.T) {
	longID := "call_" + strings.Repeat("x", 80)
	var got agentmodel.ChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"chatcmpl-sanitized-id",
			"object":"chat.completion",
			"created":1700000000,
			"model":"gpt-5.5",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`)
	}))
	defer srv.Close()

	idx := 0
	msgs := []agentmodel.Message{
		{Role: "user", Content: "read"},
		{Role: "assistant", ToolCalls: []agentmodel.ToolCall{{
			Index: &idx,
			ID:    longID,
			Type:  "function",
			Function: agentmodel.ToolCallFunction{
				Name:      "read",
				Arguments: `{"path":"README.md"}`,
			},
		}}},
		{Role: "tool", ToolCallID: longID, Content: "contents"},
		{Role: "user", Content: "summarize"},
	}
	c := newClient(t, srv, "sk-foo")
	_, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "gpt-5.5",
		Messages: msgs,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	shortID := got.Messages[1].ToolCalls[0].ID
	if shortID == longID || len(shortID) > 64 {
		t.Fatalf("sanitized tool_call id = %q len=%d", shortID, len(shortID))
	}
	if got.Messages[2].ToolCallID != shortID {
		t.Fatalf("tool_call_id = %q, want matching %q", got.Messages[2].ToolCallID, shortID)
	}
	// The caller's nested ToolCalls must keep the original ID: sanitizeMessages
	// copies the nested slice before shortening, so a router failover replays
	// the untouched original to the next provider.
	if msgs[1].ToolCalls[0].ID != longID {
		t.Fatalf("caller's tool-call ID mutated to %q; sanitize must not write through the shared backing array", msgs[1].ToolCalls[0].ID)
	}
}

func TestComplete_DropsThinkingFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "reasoning_effort") {
			t.Fatalf("request leaked reasoning_effort to OpenAI chat/completions: %s", string(body))
		}
		if strings.Contains(string(body), "thinking") {
			t.Fatalf("request leaked thinking to OpenAI chat/completions: %s", string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"chatcmpl-sanitized",
			"object":"chat.completion",
			"created":1700000000,
			"model":"gpt-5.5",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`)
	}))
	defer srv.Close()

	budget := 1024
	c := newClient(t, srv, "sk-foo")
	_, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "gpt-5.5",
		Messages: []agentmodel.Message{{Role: "user", Content: "hello"}},
		Tools: []agentmodel.Tool{
			{Type: "function", Function: agentmodel.FunctionSchema{Name: "get_weather"}},
		},
		ReasoningEffort: "medium",
		Thinking:        &agentmodel.ThinkingConfig{Type: "enabled", BudgetTokens: budget},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func TestStream_DropsThinkingFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "reasoning_effort") {
			t.Fatalf("stream request leaked reasoning_effort to OpenAI chat/completions: %s", string(body))
		}
		if strings.Contains(string(body), "thinking") {
			t.Fatalf("stream request leaked thinking to OpenAI chat/completions: %s", string(body))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := newClient(t, srv, "sk-foo")
	seq, err := c.Stream(context.Background(), agentmodel.ChatRequest{
		Model:           "gpt-5.5",
		Messages:        []agentmodel.Message{{Role: "user", Content: "hello"}},
		ReasoningEffort: "medium",
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for _, err := range seq {
		if err != nil {
			t.Fatalf("stream yielded error: %v", err)
		}
	}
}

// TestComplete_TranslatesMaxTokensForReasoningModels locks in the fix for
// OpenAI's "Unsupported parameter: 'max_tokens' is not supported with this
// model. Use 'max_completion_tokens' instead." The o-series and gpt-5 families
// reject the legacy max_tokens on chat/completions, so the provider must send
// max_completion_tokens instead.
func TestComplete_TranslatesMaxTokensForReasoningModels(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"chatcmpl-mct",
			"object":"chat.completion",
			"created":1700000000,
			"model":"gpt-5",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`)
	}))
	defer srv.Close()

	budget := 256
	c := newClient(t, srv, "sk-foo")
	if _, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:     "gpt-5",
		Messages:  []agentmodel.Message{{Role: "user", Content: "hello"}},
		MaxTokens: &budget,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, ok := gotBody["max_tokens"]; ok {
		t.Fatalf("reasoning model request leaked max_tokens; OpenAI rejects it: %v", gotBody)
	}
	got, ok := gotBody["max_completion_tokens"]
	if !ok {
		t.Fatalf("reasoning model request missing max_completion_tokens: %v", gotBody)
	}
	if got != float64(budget) {
		t.Fatalf("max_completion_tokens = %v, want %d", got, budget)
	}
}

func TestStream_TranslatesMaxTokensForReasoningModels(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	budget := 256
	c := newClient(t, srv, "sk-foo")
	seq, err := c.Stream(context.Background(), agentmodel.ChatRequest{
		Model:     "o3-mini",
		Messages:  []agentmodel.Message{{Role: "user", Content: "hello"}},
		MaxTokens: &budget,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for _, err := range seq {
		if err != nil {
			t.Fatalf("stream yielded error: %v", err)
		}
	}
	if _, ok := gotBody["max_tokens"]; ok {
		t.Fatalf("reasoning model stream leaked max_tokens; OpenAI rejects it: %v", gotBody)
	}
	if got, ok := gotBody["max_completion_tokens"]; !ok || got != float64(budget) {
		t.Fatalf("stream max_completion_tokens = %v (present=%v), want %d", got, ok, budget)
	}
}

// TestComplete_PreservesMaxTokensForNonReasoningModels guards the openaicompat
// reuse of this shaping: OpenAI-compatible local servers (vLLM, llama.cpp,
// Ollama) understand max_tokens and may reject max_completion_tokens, so the
// translation must be scoped to reasoning-family model names only.
func TestComplete_PreservesMaxTokensForNonReasoningModels(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"chatcmpl-legacy",
			"object":"chat.completion",
			"created":1700000000,
			"model":"gpt-4o",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`)
	}))
	defer srv.Close()

	budget := 256
	c := newClient(t, srv, "sk-foo")
	if _, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:     "gpt-4o",
		Messages:  []agentmodel.Message{{Role: "user", Content: "hello"}},
		MaxTokens: &budget,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, ok := gotBody["max_completion_tokens"]; ok {
		t.Fatalf("non-reasoning model must keep legacy max_tokens, not max_completion_tokens: %v", gotBody)
	}
	if got, ok := gotBody["max_tokens"]; !ok || got != float64(budget) {
		t.Fatalf("max_tokens = %v (present=%v), want %d", got, ok, budget)
	}
}

// TestComplete_StripsCacheControl locks in the OSS-robustness fix (axis 11):
// cache_control is an Anthropic-style hint clients may set on a system
// message; a strict OpenAI-compatible local server (vLLM/llama.cpp behind
// base_url) can 400 on the unknown field, so the OpenAI path must strip it —
// without mutating the caller's messages, which may be replayed to an
// Anthropic deployment where the hint is meaningful.
func TestComplete_StripsCacheControl(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "cache_control") {
			t.Fatalf("request leaked cache_control to an OpenAI-compatible server: %s", string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"chatcmpl-nocache",
			"object":"chat.completion",
			"created":1700000000,
			"model":"qwen2.5:7b",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`)
	}))
	defer srv.Close()

	msgs := []agentmodel.Message{
		{Role: "system", Content: "be helpful", CacheControl: map[string]string{"type": "ephemeral"}},
		{Role: "user", Content: "hello"},
	}
	c := newClient(t, srv, "sk-foo")
	if _, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "qwen2.5:7b",
		Messages: msgs,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if msgs[0].CacheControl == nil {
		t.Fatal("caller's message was mutated: CacheControl must survive for replay to an Anthropic deployment")
	}
}

func TestStream_StripsCacheControl(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "cache_control") {
			t.Fatalf("stream request leaked cache_control to an OpenAI-compatible server: %s", string(body))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := newClient(t, srv, "sk-foo")
	seq, err := c.Stream(context.Background(), agentmodel.ChatRequest{
		Model: "qwen2.5:7b",
		Messages: []agentmodel.Message{
			{Role: "system", Content: "be helpful", CacheControl: map[string]string{"type": "ephemeral"}},
			{Role: "user", Content: "hello"},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for _, err := range seq {
		if err != nil {
			t.Fatalf("stream yielded error: %v", err)
		}
	}
}

func TestComplete_ToolCall(t *testing.T) {
	// OpenAI returns tool_calls with arguments as a JSON-encoded string. We
	// must preserve that string verbatim.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id": "chatcmpl-tool",
			"object": "chat.completion",
			"created": 1700000000,
			"model": "gpt-4o",
			"choices": [
				{
					"index": 0,
					"message": {
						"role": "assistant",
						"content": "",
						"tool_calls": [
							{
								"id": "call_1",
								"type": "function",
								"function": {
									"name": "get_weather",
									"arguments": "{\"location\": \"SF\"}"
								}
							}
						]
					},
					"finish_reason": "tool_calls"
				}
			],
			"usage": {"prompt_tokens": 10, "completion_tokens": 8, "total_tokens": 18}
		}`)
	}))
	defer srv.Close()

	c := newClient(t, srv, "sk-foo")
	resp, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "gpt-4o",
		Messages: []agentmodel.Message{{Role: "user", Content: "weather?"}},
		Tools: []agentmodel.Tool{
			{Type: "function", Function: agentmodel.FunctionSchema{Name: "get_weather"}},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices len = %d", len(resp.Choices))
	}
	tcs := resp.Choices[0].Message.ToolCalls
	if len(tcs) != 1 {
		t.Fatalf("tool_calls len = %d, want 1", len(tcs))
	}
	if tcs[0].ID != "call_1" {
		t.Errorf("tool_call id = %q, want call_1", tcs[0].ID)
	}
	if tcs[0].Function.Name != "get_weather" {
		t.Errorf("tool_call name = %q, want get_weather", tcs[0].Function.Name)
	}
	// Arguments must remain a JSON-encoded string (not a parsed object).
	wantArgs := `{"location": "SF"}`
	if tcs[0].Function.Arguments != wantArgs {
		t.Errorf("arguments = %q, want %q", tcs[0].Function.Arguments, wantArgs)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish = %q, want tool_calls", resp.Choices[0].FinishReason)
	}
}

func TestStream_Chunks(t *testing.T) {
	// Serve a 4-event SSE stream: 2 deltas + final delta + DONE.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify stream + include_usage requested
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"stream":true`) {
			t.Errorf("expected stream:true in body, got %s", string(body))
		}
		if !strings.Contains(string(body), `"include_usage":true`) {
			t.Errorf("expected include_usage:true in body, got %s", string(body))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		writeChunk := func(s string) {
			_, _ = io.WriteString(w, s)
			if flusher != nil {
				flusher.Flush()
			}
		}
		writeChunk("data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello\"}}]}\n\n")
		writeChunk("data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" world\"}}]}\n\n")
		writeChunk("data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		writeChunk("data: {\"id\":\"x\",\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2,\"total_tokens\":6}}\n\n")
		writeChunk("data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := newClient(t, srv, "sk-foo")
	seq, err := c.Stream(context.Background(), agentmodel.ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var (
		chunks       []string
		finishReason string
		usage        *agentmodel.Usage
	)
	for ch, err := range seq {
		if err != nil {
			t.Fatalf("stream yielded error: %v", err)
		}
		chunks = append(chunks, ch.Delta.Content)
		if ch.FinishReason != "" {
			finishReason = ch.FinishReason
		}
		if ch.Usage != nil {
			usage = ch.Usage
		}
	}
	if len(chunks) < 3 {
		t.Errorf("got %d chunks, want >=3", len(chunks))
	}
	if finishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", finishReason)
	}
	if usage == nil {
		t.Fatalf("expected non-nil Usage on final chunk")
	}
	if usage.TotalTokens != 6 {
		t.Errorf("usage.TotalTokens = %d, want 6", usage.TotalTokens)
	}
}

func TestStream_ToolCallDeltasPreserveIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read","arguments":""}}]}}]}

`)
		_, _ = io.WriteString(w, `data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\""}}]}}]}

`)
		_, _ = io.WriteString(w, `data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":\"/tmp/x\"}"}}]},"finish_reason":"tool_calls"}]}

`)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := newClient(t, srv, "sk-foo")
	seq, err := c.Stream(context.Background(), agentmodel.ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []agentmodel.Message{{Role: "user", Content: "read /tmp/x"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var got []agentmodel.ToolCall
	var finish string
	for ch, err := range seq {
		if err != nil {
			t.Fatalf("stream yielded error: %v", err)
		}
		if len(ch.Delta.ToolCalls) > 0 {
			got = append(got, ch.Delta.ToolCalls[0])
		}
		if ch.FinishReason != "" {
			finish = ch.FinishReason
		}
	}
	if len(got) != 3 {
		t.Fatalf("tool call delta chunks = %d, want 3", len(got))
	}
	for i, tc := range got {
		if tc.Index == nil || *tc.Index != 0 {
			t.Fatalf("tool call delta %d index = %v, want 0", i, tc.Index)
		}
	}
	if got[0].ID != "call_1" || got[0].Function.Name != "read" {
		t.Fatalf("first tool call delta = %#v, want id/name", got[0])
	}
	if got[1].Function.Name != "" || got[1].Function.Arguments != `{"path"` {
		t.Fatalf("second tool call delta = %#v, want args fragment only", got[1])
	}
	secondJSON, err := json.Marshal(got[1])
	if err != nil {
		t.Fatalf("marshal second tool call delta: %v", err)
	}
	if string(secondJSON) != `{"index":0,"function":{"arguments":"{\"path\""}}` {
		t.Fatalf("second tool call delta JSON = %s, want index + arguments only", secondJSON)
	}
	if finish != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", finish)
	}
}

func TestEmbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("path = %q, want /embeddings", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req agentmodel.EmbeddingRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("decode embed req: %v", err)
		}
		if req.Model != "text-embedding-3-small" {
			t.Errorf("model = %q", req.Model)
		}
		if len(req.Input) != 1 || req.Input[0] != "hello" {
			t.Errorf("input = %#v", req.Input)
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

	c := newClient(t, srv, "sk-foo")
	resp, err := c.Embed(context.Background(), agentmodel.EmbeddingRequest{
		Model: "text-embedding-3-small",
		Input: []string{"hello"},
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("data len = %d", len(resp.Data))
	}
	want := []float64{0.1, 0.2, 0.3}
	if len(resp.Data[0].Embedding) != len(want) {
		t.Fatalf("embedding len = %d, want %d", len(resp.Data[0].Embedding), len(want))
	}
	for i, v := range want {
		if resp.Data[0].Embedding[i] != v {
			t.Errorf("embedding[%d] = %v, want %v", i, resp.Data[0].Embedding[i], v)
		}
	}
	if resp.Usage.TotalTokens != 2 {
		t.Errorf("usage.TotalTokens = %d", resp.Usage.TotalTokens)
	}
}

func TestComplete_401_Auth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error": {"message": "Invalid API key", "type": "invalid_request_error", "code": "invalid_api_key"}}`)
	}))
	defer srv.Close()

	c := newClient(t, srv, "sk-bad")
	_, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "auth") {
		t.Errorf("err = %q, want substring 'auth'", err.Error())
	}
}

func TestComplete_429_RateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error": {"message": "Rate limit exceeded", "type": "rate_limit_exceeded"}}`)
	}))
	defer srv.Close()

	c := newClient(t, srv, "sk-foo")
	_, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "rate limit") && !strings.Contains(msg, "rate_limit") {
		t.Errorf("err = %q, expected rate-limit signal", err.Error())
	}
}

func TestAuthHeaderApplied(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","created":0,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`)
	}))
	defer srv.Close()

	c := newClient(t, srv, "sk-foo")
	if _, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotAuth != "Bearer sk-foo" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer sk-foo")
	}
}

var _ provider.ModelLister = (*Client)(nil)

func TestListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/models" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("auth header: got %q", got)
		}
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"gpt-4o","object":"model"},{"id":"gpt-5.5","object":"model"}]}`)
	}))
	defer srv.Close()

	got, err := newClient(t, srv, "sk-test").ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(got) != 2 || got[0] != "gpt-4o" || got[1] != "gpt-5.5" {
		t.Errorf("got %v, want [gpt-4o gpt-5.5]", got)
	}
}

// TestStream_SurfacesMidStreamError locks in that an OpenAI-compatible backend
// (vLLM/Ollama/Azure content-filter) emitting a mid-stream `data: {"error":...}`
// object is surfaced as an error, not swallowed into a truncated "success"
// that ends with no finish_reason and no error.
func TestStream_SurfacesMidStreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"par\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"error\":{\"message\":\"boom\",\"type\":\"server_error\"}}\n\n")
		// no [DONE]: the stream died mid-way
	}))
	defer srv.Close()

	c := newClient(t, srv, "sk-foo")
	seq, err := c.Stream(context.Background(), agentmodel.ChatRequest{
		Model:    "gpt-4o",
		Messages: []agentmodel.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var gotErr error
	for _, e := range seq {
		if e != nil {
			gotErr = e
		}
	}
	if gotErr == nil {
		t.Fatal("expected the mid-stream error object to surface as an error, got none")
	}
	if !strings.Contains(gotErr.Error(), "boom") {
		t.Errorf("error should carry the upstream message, got: %v", gotErr)
	}
}
