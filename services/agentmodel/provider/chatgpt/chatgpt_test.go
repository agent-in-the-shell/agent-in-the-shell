package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
)

// writeSSE writes a minimal Responses API SSE stream — response.created, one
// response.output_text.delta per delta, then response.completed carrying usage.
// Since #811, Complete drives the streaming path and aggregates, so its tests
// serve SSE rather than a buffered JSON body.
func writeSSE(w http.ResponseWriter, deltas []string, promptTok, completionTok, totalTok int) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
	for _, d := range deltas {
		encoded, _ := json.Marshal(d)
		_, _ = io.WriteString(w, fmt.Sprintf("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":%s}\n\n", encoded))
	}
	_, _ = io.WriteString(w, fmt.Sprintf("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":%d,\"output_tokens\":%d,\"total_tokens\":%d}}}\n\n", promptTok, completionTok, totalTok))
}

// writeAuthFile writes an auth.json under dir for tests. Mirrors the helper in
// auth/auth_test.go (we can't import it because it's in the _test package).
func writeAuthFile(t *testing.T, dir string, data map[string]any) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "auth.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(data); err != nil {
		t.Fatalf("encode: %v", err)
	}
}

// freshAuth returns a ChatGPTOAuth with a non-expired token already on disk.
// Apply will succeed without any network round-trip.
func freshAuth(t *testing.T) *auth.ChatGPTOAuth {
	t.Helper()
	dir := t.TempDir()
	writeAuthFile(t, dir, map[string]any{
		"access_token":  "valid-token",
		"refresh_token": "rt",
		"expires_at_ms": time.Now().Add(time.Hour).UnixMilli(),
	})
	return auth.NewChatGPTOAuth(dir, &http.Client{Timeout: 5 * time.Second})
}

func TestBuildRequest_ConvertsImageContentParts(t *testing.T) {
	var req agentmodel.ChatRequest
	if err := json.Unmarshal([]byte(`{
		"model":"gpt-5",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"describe this"},
			{"type":"image_url","image_url":{"url":"https://example.com/cat.png","detail":"high"}}
		]}]
	}`), &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}

	body, err := buildRequest(req, false)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if len(body.Input) != 1 {
		t.Fatalf("Input len = %d, want 1", len(body.Input))
	}
	msg, ok := body.Input[0].(responsesMessage)
	if !ok {
		t.Fatalf("Input[0] = %#v, want responsesMessage", body.Input[0])
	}
	if len(msg.Content) != 2 {
		t.Fatalf("content len = %d, want 2", len(msg.Content))
	}
	if msg.Content[0].Type != "input_text" || msg.Content[0].Text != "describe this" {
		t.Fatalf("text part = %+v", msg.Content[0])
	}
	if msg.Content[1].Type != "input_image" || msg.Content[1].ImageURL != "https://example.com/cat.png" || msg.Content[1].Detail != "high" {
		t.Fatalf("image part = %+v", msg.Content[1])
	}
}

func TestBuildRequest_ForwardsPromptCacheKey(t *testing.T) {
	req := agentmodel.ChatRequest{
		Model:          "gpt-5.5",
		PromptCacheKey: "session-cache-key",
		Messages:       []agentmodel.Message{{Role: "user", Content: "hi"}},
	}

	body, err := buildRequest(req, true)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if body.PromptCacheKey != "session-cache-key" {
		t.Errorf("PromptCacheKey = %q, want session-cache-key", body.PromptCacheKey)
	}
}

func TestBuildRequest_RejectsUnsupportedContentParts(t *testing.T) {
	var req agentmodel.ChatRequest
	if err := json.Unmarshal([]byte(`{
		"model":"gpt-5",
		"messages":[{"role":"user","content":[{"type":"input_file","file_id":"file_123"}]}]
	}`), &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}

	_, err := buildRequest(req, false)
	if err == nil {
		t.Fatal("expected unsupported content part to be rejected")
	}
	if ae := agentmodel.Wrap(err); ae.Type != agentmodel.ErrTypeInvalidRequest {
		t.Fatalf("error type = %q, want %q; err=%v", ae.Type, agentmodel.ErrTypeInvalidRequest, err)
	}
}

func TestName(t *testing.T) {
	c := New(freshAuth(t))
	if c.Name() != "chatgpt" {
		t.Errorf("Name() = %q, want %q", c.Name(), "chatgpt")
	}
}

func TestAuthMode(t *testing.T) {
	c := New(freshAuth(t))
	if c.AuthMode() != agentmodel.AuthModeSubscription {
		t.Errorf("AuthMode() = %q, want %q", c.AuthMode(), agentmodel.AuthModeSubscription)
	}
}

func TestSupportedModels(t *testing.T) {
	c := New(freshAuth(t))
	got := map[string]bool{}
	for _, m := range c.SupportedModels() {
		got[m] = true
	}
	for _, want := range []string{"gpt-5", "gpt-5-pro", "gpt-5-codex"} {
		if !got[want] {
			t.Errorf("SupportedModels() missing %q", want)
		}
	}
}

func TestResponsesUsageCachedTokens(t *testing.T) {
	if got := (responsesUsage{}).cachedTokens(); got != 0 {
		t.Fatalf("cachedTokens nil details = %d, want 0", got)
	}
	u := responsesUsage{
		InputTokensDetails: &struct {
			CachedTokens int `json:"cached_tokens,omitempty"`
		}{CachedTokens: 7},
	}
	if got := u.cachedTokens(); got != 7 {
		t.Fatalf("cachedTokens = %d, want 7", got)
	}
}

func TestResponsesContentFromMessageVariants(t *testing.T) {
	t.Run("empty string content is omitted", func(t *testing.T) {
		parts, err := responsesContentFromMessage(agentmodel.Message{Role: "user"}, "input_text")
		if err != nil {
			t.Fatalf("responsesContentFromMessage: %v", err)
		}
		if len(parts) != 0 {
			t.Fatalf("parts = %+v, want none", parts)
		}
	})

	t.Run("input_image string url", func(t *testing.T) {
		var msg agentmodel.Message
		if err := json.Unmarshal([]byte(`{"role":"user","content":[{"type":"input_image","image_url":"https://example.com/a.png","detail":"low"}]}`), &msg); err != nil {
			t.Fatal(err)
		}
		parts, err := responsesContentFromMessage(msg, "input_text")
		if err != nil {
			t.Fatalf("responsesContentFromMessage: %v", err)
		}
		if len(parts) != 1 || parts[0].Type != "input_image" || parts[0].ImageURL != "https://example.com/a.png" || parts[0].Detail != "low" {
			t.Fatalf("parts = %+v", parts)
		}
	})

	t.Run("missing image url is rejected", func(t *testing.T) {
		var msg agentmodel.Message
		if err := json.Unmarshal([]byte(`{"role":"user","content":[{"type":"image_url"}]}`), &msg); err != nil {
			t.Fatal(err)
		}
		_, err := responsesContentFromMessage(msg, "input_text")
		if err == nil || !strings.Contains(err.Error(), "missing image_url.url") {
			t.Fatalf("err = %v, want missing image_url.url", err)
		}
	})

	t.Run("invalid image url is rejected", func(t *testing.T) {
		var msg agentmodel.Message
		if err := json.Unmarshal([]byte(`{"role":"user","content":[{"type":"image_url","image_url":123}]}`), &msg); err != nil {
			t.Fatal(err)
		}
		_, err := responsesContentFromMessage(msg, "input_text")
		if err == nil || !strings.Contains(err.Error(), "invalid image_url") {
			t.Fatalf("err = %v, want invalid image_url", err)
		}
	})
}

func TestSanitizeJSONSchemaValueArray(t *testing.T) {
	schema := map[string]any{
		"anyOf": []any{
			map[string]any{"type": "string", "pattern": `(?<=x)y`},
			map[string]any{"type": "string", "pattern": `^[a-z]+$`},
		},
	}
	got := sanitizeJSONSchema(schema)
	anyOf := got["anyOf"].([]any)
	first := anyOf[0].(map[string]any)
	if _, ok := first["pattern"]; ok {
		t.Fatalf("lookbehind pattern was not removed: %+v", first)
	}
	second := anyOf[1].(map[string]any)
	if second["pattern"] != `^[a-z]+$` {
		t.Fatalf("safe pattern = %v, want preserved", second["pattern"])
	}
}

func TestJoinResponsesToolCallID(t *testing.T) {
	if got := joinResponsesToolCallID("call_1", ""); got != "call_1" {
		t.Fatalf("join without item = %q, want call_1", got)
	}
	if got := joinResponsesToolCallID("call_1", "item_1"); got != "call_1|item_1" {
		t.Fatalf("join with item = %q, want call_1|item_1", got)
	}
}

func TestSummarizeStreamFailureShapes(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{"top-level message", `{"message":"bad request"}`, "bad request"},
		{"response error", `{"response":{"error":{"message":"upstream failed"}}}`, "upstream failed"},
		{"incomplete reason", `{"response":{"incomplete_details":{"reason":"max_output_tokens"}}}`, "max_output_tokens"},
		{"status", `{"response":{"status":"failed"}}`, "failed"},
		{"code", `{"code":"rate_limit"}`, "rate_limit"},
		{"raw fallback", ` not json `, "not json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := summarizeStreamFailure(tt.data); got != tt.want {
				t.Fatalf("summarizeStreamFailure() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildRequest_ResponsesCompatibility(t *testing.T) {
	idx := 0
	toolChoice := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": "read",
		},
	}
	body, err := buildRequest(agentmodel.ChatRequest{
		Model: "gpt-5.5",
		Messages: []agentmodel.Message{
			{Role: "developer", Content: "be concise"},
			{Role: "user", Content: "read README"},
			{Role: "assistant", Content: "I'll read it.", ToolCalls: []agentmodel.ToolCall{{
				Index: &idx,
				ID:    "call_1|fc_1",
				Type:  "function",
				Function: agentmodel.ToolCallFunction{
					Name:      "read",
					Arguments: `{"path":"README.md"}`,
				},
			}}},
			{Role: "tool", ToolCallID: "call_1|fc_1", Content: "README contents"},
			{Role: "user", Content: "summarize"},
		},
		ToolChoice: toolChoice,
		Tools: []agentmodel.Tool{{
			Type: "function",
			Function: agentmodel.FunctionSchema{
				Name:        "read",
				Description: "Read a file",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type":    "string",
							"pattern": `^(?!/)(?!.*(?:^|/)\.\.(?:/|$)).+$`,
						},
						"kind": map[string]any{
							"type":    "string",
							"pattern": `^[a-z]+$`,
						},
					},
				},
			},
		}},
	}, true)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}

	if body.Instructions != "be concise" {
		t.Fatalf("Instructions = %q, want developer content", body.Instructions)
	}
	if len(body.Input) != 5 {
		t.Fatalf("Input len = %d, want 5", len(body.Input))
	}
	assistant, ok := body.Input[1].(responsesMessage)
	if !ok {
		t.Fatalf("Input[1] = %#v, want responsesMessage", body.Input[1])
	}
	if assistant.Role != "assistant" || assistant.Content[0].Type != "output_text" {
		t.Fatalf("assistant text input = %+v", assistant)
	}
	fc, ok := body.Input[2].(responsesFunctionCall)
	if !ok {
		t.Fatalf("Input[2] = %#v, want responsesFunctionCall", body.Input[2])
	}
	if fc.Type != "function_call" || fc.ID != "fc_1" || fc.CallID != "call_1" || fc.Name != "read" || fc.Arguments != `{"path":"README.md"}` {
		t.Fatalf("function_call = %+v", fc)
	}
	out, ok := body.Input[3].(responsesFunctionCallOutput)
	if !ok {
		t.Fatalf("Input[3] = %#v, want responsesFunctionCallOutput", body.Input[3])
	}
	if out.Type != "function_call_output" || out.CallID != "call_1" || out.Output != "README contents" {
		t.Fatalf("function_call_output = %+v", out)
	}
	if len(body.Tools) != 1 || body.Tools[0].Name != "read" {
		t.Fatalf("tools = %+v, want flat Responses tool named read", body.Tools)
	}
	if got, ok := body.ToolChoice.(map[string]any); !ok || got["type"] != "function" {
		t.Fatalf("tool_choice = %#v, want forced function choice", body.ToolChoice)
	}
	path := body.Tools[0].Parameters["properties"].(map[string]any)["path"].(map[string]any)
	if _, ok := path["pattern"]; ok {
		t.Fatalf("lookaround regex pattern was not removed: %#v", path["pattern"])
	}
	kind := body.Tools[0].Parameters["properties"].(map[string]any)["kind"].(map[string]any)
	if kind["pattern"] != `^[a-z]+$` {
		t.Fatalf("safe regex pattern = %#v, want preserved", kind["pattern"])
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("Marshal body: %v", err)
	}
	bodyJSON := string(encoded)
	if strings.Contains(bodyJSON, `"role":"tool"`) {
		t.Fatalf("tool role leaked into Responses body: %s", bodyJSON)
	}
	if strings.Contains(bodyJSON, `"tools":[{"type":"function","function"`) {
		t.Fatalf("nested Chat Completions tool leaked into Responses tools body: %s", bodyJSON)
	}
	if !strings.Contains(bodyJSON, `"tool_choice":{"function":{"name":"read"},"type":"function"}`) {
		t.Fatalf("tool_choice missing from Responses body: %s", bodyJSON)
	}
	if !strings.Contains(bodyJSON, `"type":"function_call_output","call_id":"call_1"`) {
		t.Fatalf("function_call_output missing from body: %s", bodyJSON)
	}
}

func TestComplete_RequestShape(t *testing.T) {
	var (
		gotPath        string
		gotMethod      string
		gotAuth        string
		gotBeta        string
		gotContentType string
		gotBody        []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		gotBeta = r.Header.Get("OpenAI-Beta")
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)

		writeSSE(w, []string{"hi", " there"}, 7, 4, 11)
	}))
	defer srv.Close()

	c := NewWithBaseURL(freshAuth(t), srv.URL)
	resp, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model: "gpt-5",
		Messages: []agentmodel.Message{
			{Role: "system", Content: "you are concise"},
			{Role: "user", Content: "hello"},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Endpoint shape.
	if gotPath != "/responses" {
		t.Errorf("path = %q, want /responses", gotPath)
	}
	if gotMethod != "POST" {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotAuth != "Bearer valid-token" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer valid-token")
	}
	if gotBeta != "responses=v1" {
		t.Errorf("OpenAI-Beta = %q, want responses=v1", gotBeta)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}

	// Request body: Responses API shape.
	var body map[string]any
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("decode req body: %v", err)
	}
	if body["model"] != "gpt-5" {
		t.Errorf("body.model = %v, want gpt-5", body["model"])
	}
	if body["instructions"] != "you are concise" {
		t.Errorf("body.instructions = %v, want %q", body["instructions"], "you are concise")
	}
	if v, ok := body["stream"].(bool); !ok || !v {
		t.Errorf("body.stream = %v, want true (codex is stream-only)", body["stream"])
	}
	input, ok := body["input"].([]any)
	if !ok {
		t.Fatalf("body.input not an array: %#v", body["input"])
	}
	if len(input) != 1 {
		t.Fatalf("body.input len = %d, want 1 (system stripped)", len(input))
	}
	first, _ := input[0].(map[string]any)
	if first["role"] != "user" {
		t.Errorf("input[0].role = %v, want user", first["role"])
	}
	content, _ := first["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("input[0].content len = %d, want 1", len(content))
	}
	cpart, _ := content[0].(map[string]any)
	if cpart["type"] != "input_text" {
		t.Errorf("input[0].content[0].type = %v, want input_text", cpart["type"])
	}
	if cpart["text"] != "hello" {
		t.Errorf("input[0].content[0].text = %v, want hello", cpart["text"])
	}

	// Response aggregation. ID/Model/Created are not asserted: the aggregating
	// Complete leaves them zero (StreamChunk carries no such fields); the router
	// stamps Model and the HTTP handler backfills ID downstream.
	if resp.Object != "chat.completion" {
		t.Errorf("resp.Object = %q, want chat.completion", resp.Object)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("len(resp.Choices) = %d, want 1", len(resp.Choices))
	}
	if resp.Choices[0].Message.Role != "assistant" {
		t.Errorf("choice.Message.Role = %q, want assistant", resp.Choices[0].Message.Role)
	}
	if resp.Choices[0].Message.Content != "hi there" {
		t.Errorf("choice.Message.Content = %q, want hi there", resp.Choices[0].Message.Content)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("choice.FinishReason = %q, want stop", resp.Choices[0].FinishReason)
	}

	// Usage translation: input_tokens → PromptTokens, etc.
	if resp.Usage.PromptTokens != 7 {
		t.Errorf("Usage.PromptTokens = %d, want 7", resp.Usage.PromptTokens)
	}
	if resp.Usage.CompletionTokens != 4 {
		t.Errorf("Usage.CompletionTokens = %d, want 4", resp.Usage.CompletionTokens)
	}
	if resp.Usage.TotalTokens != 11 {
		t.Errorf("Usage.TotalTokens = %d, want 11", resp.Usage.TotalTokens)
	}
	if resp.Usage.CostUSD != 0 {
		t.Errorf("Usage.CostUSD = %f, want 0 (subscription)", resp.Usage.CostUSD)
	}
	if resp.Usage.AuthMode != agentmodel.AuthModeSubscription {
		t.Errorf("Usage.AuthMode = %q, want %q", resp.Usage.AuthMode, agentmodel.AuthModeSubscription)
	}
}

func TestComplete_MultiSystem(t *testing.T) {
	// Multiple system messages should concatenate (newline-joined) into instructions.
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		writeSSE(w, []string{"ok"}, 1, 1, 2)
	}))
	defer srv.Close()

	c := NewWithBaseURL(freshAuth(t), srv.URL)
	if _, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model: "gpt-5",
		Messages: []agentmodel.Message{
			{Role: "system", Content: "rule one"},
			{Role: "system", Content: "rule two"},
			{Role: "user", Content: "hi"},
		},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	instr, _ := body["instructions"].(string)
	if !strings.Contains(instr, "rule one") || !strings.Contains(instr, "rule two") {
		t.Errorf("instructions should contain both system messages, got %q", instr)
	}
}

func TestStream_Chunks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"stream":true`) {
			t.Errorf("expected stream:true in body, got %s", string(body))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = io.WriteString(w, s)
			if flusher != nil {
				flusher.Flush()
			}
		}
		// Responses API SSE: event: <type> + data: <json>.
		write("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_s\"}}\n\n")
		write("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello\"}\n\n")
		write("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\" world\"}\n\n")
		write("event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"text\":\"Hello world\"}\n\n")
		write("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_s\",\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n")
	}))
	defer srv.Close()

	c := NewWithBaseURL(freshAuth(t), srv.URL)
	seq, err := c.Stream(context.Background(), agentmodel.ChatRequest{
		Model:    "gpt-5",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var (
		text         strings.Builder
		finishReason string
		usage        *agentmodel.Usage
	)
	for ch, err := range seq {
		if err != nil {
			t.Fatalf("stream yielded error: %v", err)
		}
		text.WriteString(ch.Delta.Content)
		if ch.FinishReason != "" {
			finishReason = ch.FinishReason
		}
		if ch.Usage != nil {
			usage = ch.Usage
		}
	}
	if got := text.String(); got != "Hello world" {
		t.Errorf("streamed text = %q, want %q", got, "Hello world")
	}
	if finishReason != "stop" {
		t.Errorf("finishReason = %q, want stop", finishReason)
	}
	if usage == nil {
		t.Fatal("expected non-nil Usage on final chunk")
	}
	if usage.PromptTokens != 3 || usage.CompletionTokens != 2 || usage.TotalTokens != 5 {
		t.Errorf("usage = %+v, want {3, 2, 5, ...}", usage)
	}
	if usage.AuthMode != agentmodel.AuthModeSubscription {
		t.Errorf("usage.AuthMode = %q, want %q", usage.AuthMode, agentmodel.AuthModeSubscription)
	}
	if usage.CostUSD != 0 {
		t.Errorf("usage.CostUSD = %f, want 0", usage.CostUSD)
	}
}

func TestComplete_FunctionCallResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Codex is stream-only, so Complete now aggregates the streamed
		// function-call fragments: one output_item.added carrying the id/name,
		// then argument deltas, then response.completed.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: response.output_item.added\n"+
			`data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":""}}`+"\n\n")
		_, _ = io.WriteString(w, "event: response.function_call_arguments.delta\n"+
			`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"path\":"}`+"\n\n")
		_, _ = io.WriteString(w, "event: response.function_call_arguments.delta\n"+
			`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"\"README.md\"}"}`+"\n\n")
		_, _ = io.WriteString(w, "event: response.completed\n"+
			`data: {"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14}}}`+"\n\n")
	}))
	defer srv.Close()

	c := NewWithBaseURL(freshAuth(t), srv.URL)
	resp, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "gpt-5",
		Messages: []agentmodel.Message{{Role: "user", Content: "read README"}},
		Tools: []agentmodel.Tool{{
			Type:     "function",
			Function: agentmodel.FunctionSchema{Name: "read"},
		}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("choice = %+v, want tool_calls finish", resp.Choices)
	}
	got := resp.Choices[0].Message.ToolCalls
	if len(got) != 1 {
		t.Fatalf("tool calls len = %d, want 1", len(got))
	}
	if got[0].ID != "call_1|fc_1" || got[0].Function.Name != "read" || got[0].Function.Arguments != `{"path":"README.md"}` {
		t.Fatalf("tool call = %+v", got[0])
	}
	if got[0].Index == nil || *got[0].Index != 0 {
		t.Fatalf("tool call index = %v, want 0", got[0].Index)
	}
}

func TestStream_FunctionCallChunks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: response.output_item.added\n")
		_, _ = io.WriteString(w, `data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":""}}`+"\n\n")
		_, _ = io.WriteString(w, "event: response.function_call_arguments.delta\n")
		_, _ = io.WriteString(w, `data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"path\""}`+"\n\n")
		_, _ = io.WriteString(w, "event: response.function_call_arguments.delta\n")
		_, _ = io.WriteString(w, `data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":":\"README.md\"}"}`+"\n\n")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14}}}`+"\n\n")
	}))
	defer srv.Close()

	c := NewWithBaseURL(freshAuth(t), srv.URL)
	seq, err := c.Stream(context.Background(), agentmodel.ChatRequest{
		Model:    "gpt-5",
		Messages: []agentmodel.Message{{Role: "user", Content: "read README"}},
		Tools: []agentmodel.Tool{{
			Type:     "function",
			Function: agentmodel.FunctionSchema{Name: "read"},
		}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var (
		toolCalls    []agentmodel.ToolCall
		finishReason string
		usage        *agentmodel.Usage
	)
	for ch, err := range seq {
		if err != nil {
			t.Fatalf("stream yielded error: %v", err)
		}
		toolCalls = append(toolCalls, ch.Delta.ToolCalls...)
		if ch.FinishReason != "" {
			finishReason = ch.FinishReason
		}
		if ch.Usage != nil {
			usage = ch.Usage
		}
	}
	if len(toolCalls) != 3 {
		t.Fatalf("toolCalls len = %d, want 3", len(toolCalls))
	}
	if toolCalls[0].ID != "call_1|fc_1" || toolCalls[0].Function.Name != "read" {
		t.Fatalf("initial tool call = %+v, want call_1|fc_1/read", toolCalls[0])
	}
	if toolCalls[1].Function.Arguments != `{"path"` {
		t.Fatalf("first args delta = %q", toolCalls[1].Function.Arguments)
	}
	if toolCalls[1].ID != "" || toolCalls[1].Type != "" || toolCalls[1].Function.Name != "" {
		t.Fatalf("first args delta included non-delta fields: %+v", toolCalls[1])
	}
	if toolCalls[2].Function.Arguments != `:"README.md"}` {
		t.Fatalf("second args delta = %q", toolCalls[2].Function.Arguments)
	}
	if toolCalls[2].ID != "" || toolCalls[2].Type != "" || toolCalls[2].Function.Name != "" {
		t.Fatalf("second args delta included non-delta fields: %+v", toolCalls[2])
	}
	if finishReason != "tool_calls" {
		t.Fatalf("finishReason = %q, want tool_calls", finishReason)
	}
	if usage == nil || usage.TotalTokens != 14 {
		t.Fatalf("usage = %+v, want total tokens 14", usage)
	}
}

func TestStream_FunctionCallDeltaUnknownIDFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: response.function_call_arguments.delta\n")
		_, _ = io.WriteString(w, `data: {"type":"response.function_call_arguments.delta","item_id":"missing","delta":"{}"}`+"\n\n")
	}))
	defer srv.Close()

	c := NewWithBaseURL(freshAuth(t), srv.URL)
	seq, err := c.Stream(context.Background(), agentmodel.ChatRequest{
		Model:    "gpt-5",
		Messages: []agentmodel.Message{{Role: "user", Content: "read README"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for _, err := range seq {
		if err == nil {
			continue
		}
		if !strings.Contains(err.Error(), "unknown item_id") {
			t.Fatalf("err = %v, want unknown item_id", err)
		}
		return
	}
	t.Fatal("expected stream error")
}

func TestEmbed_NotSupported(t *testing.T) {
	c := New(freshAuth(t))
	_, err := c.Embed(context.Background(), agentmodel.EmbeddingRequest{
		Model: "text-embedding-3-small",
		Input: []string{"hi"},
	})
	if !errors.Is(err, provider.ErrNotSupported) {
		t.Errorf("Embed err = %v, want ErrNotSupported", err)
	}
}

func TestComplete_RefreshOnExpiredToken(t *testing.T) {
	// Two test servers: one for OAuth refresh (auth.openai.com substitute), one
	// for the chatgpt responses endpoint. Auth file holds an expired token.
	var refreshHits int32
	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&refreshHits, 1)
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "grant_type=refresh_token") {
			t.Errorf("refresh body missing grant_type=refresh_token: %s", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "refreshed-token",
			"refresh_token": "new-rt",
			"expires_in":    3600,
		})
	}))
	defer authSrv.Close()

	var apiAuth string
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiAuth = r.Header.Get("Authorization")
		writeSSE(w, []string{"ok"}, 1, 1, 2)
	}))
	defer apiSrv.Close()

	dir := t.TempDir()
	writeAuthFile(t, dir, map[string]any{
		"access_token":  "expired",
		"refresh_token": "old-rt",
		"expires_at_ms": time.Now().Add(-time.Hour).UnixMilli(),
	})

	a := auth.NewChatGPTOAuth(dir, &http.Client{Timeout: 5 * time.Second})
	a.OverrideURLs(authSrv.URL+"/devicecode", authSrv.URL+"/devicetoken", authSrv.URL+"/oauth/token")

	c := NewWithBaseURL(a, apiSrv.URL)
	if _, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "gpt-5",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if got := atomic.LoadInt32(&refreshHits); got != 1 {
		t.Errorf("refresh hits = %d, want 1", got)
	}
	if apiAuth != "Bearer refreshed-token" {
		t.Errorf("api Authorization = %q, want Bearer refreshed-token", apiAuth)
	}
}

func TestComplete_BetaHeader(t *testing.T) {
	// Dedicated test for the OpenAI-Beta header so a regression there fails
	// loudly even if other tests get reorganized.
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("OpenAI-Beta")
		writeSSE(w, []string{"ok"}, 0, 0, 0)
	}))
	defer srv.Close()

	c := NewWithBaseURL(freshAuth(t), srv.URL)
	if _, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "gpt-5",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got != "responses=v1" {
		t.Errorf("OpenAI-Beta = %q, want responses=v1", got)
	}
}

func TestComplete_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"unauthorized"}}`)
	}))
	defer srv.Close()

	c := NewWithBaseURL(freshAuth(t), srv.URL)
	_, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "gpt-5",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "401") &&
		!strings.Contains(strings.ToLower(err.Error()), "unauthorized") {
		t.Errorf("err = %q, want substring 401/unauthorized", err.Error())
	}
}

func TestComplete_StreamError(t *testing.T) {
	// A terminal failure event mid-stream (after a 200 OK) must surface from
	// Complete as a non-nil error with partial output discarded — not a silent
	// empty success. Complete inherits this from the streaming path.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
		_, _ = io.WriteString(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"model exploded\"}}}\n\n")
	}))
	defer srv.Close()

	c := NewWithBaseURL(freshAuth(t), srv.URL)
	resp, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "gpt-5",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatalf("Complete err = nil, want non-nil on mid-stream failure; resp=%+v", resp)
	}
	if !strings.Contains(err.Error(), "model exploded") && !strings.Contains(err.Error(), "response.failed") {
		t.Errorf("err = %q, want response.failed / model exploded", err.Error())
	}
	if len(resp.Choices) != 0 {
		t.Errorf("resp.Choices = %+v, want empty on error", resp.Choices)
	}
}

func TestComplete_ErrorBranches(t *testing.T) {
	t.Run("auth apply", func(t *testing.T) {
		c := NewWithBaseURL(failingAuth{}, "http://example.test")
		_, err := c.Complete(context.Background(), agentmodel.ChatRequest{
			Model:    "gpt-5",
			Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
		})
		if err == nil || !strings.Contains(err.Error(), "apply failed") {
			t.Fatalf("Complete err = %v, want apply failed", err)
		}
	})

	t.Run("transport", func(t *testing.T) {
		c := NewWithBaseURL(freshAuth(t), "http://example.test")
		c.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("transport failed")
		})
		_, err := c.Complete(context.Background(), agentmodel.ChatRequest{
			Model:    "gpt-5",
			Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
		})
		if err == nil || !strings.Contains(err.Error(), "transport failed") {
			t.Fatalf("Complete err = %v, want transport failed", err)
		}
	})

	t.Run("decode", func(t *testing.T) {
		// Complete now aggregates the stream, so a malformed delta surfaces as
		// the stream's decode error rather than a buffered-response decode error.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {bad-json\n\n")
		}))
		defer srv.Close()

		c := NewWithBaseURL(freshAuth(t), srv.URL)
		_, err := c.Complete(context.Background(), agentmodel.ChatRequest{
			Model:    "gpt-5",
			Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
		})
		if err == nil || !strings.Contains(err.Error(), "decode delta") {
			t.Fatalf("Complete err = %v, want decode delta", err)
		}
	})
}

func TestStream_ErrorBranches(t *testing.T) {
	t.Run("http status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", http.StatusBadGateway)
		}))
		defer srv.Close()

		c := NewWithBaseURL(freshAuth(t), srv.URL)
		_, err := c.Stream(context.Background(), agentmodel.ChatRequest{
			Model:    "gpt-5",
			Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
		})
		if err == nil || !strings.Contains(err.Error(), "502") {
			t.Fatalf("Stream err = %v, want 502", err)
		}
	})

	t.Run("decode delta", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {bad-json\n\n")
		}))
		defer srv.Close()

		c := NewWithBaseURL(freshAuth(t), srv.URL)
		seq, err := c.Stream(context.Background(), agentmodel.ChatRequest{
			Model:    "gpt-5",
			Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
		})
		if err != nil {
			t.Fatalf("Stream setup: %v", err)
		}
		for _, err := range seq {
			if err == nil || !strings.Contains(err.Error(), "decode delta") {
				t.Fatalf("stream err = %v, want decode delta", err)
			}
			return
		}
		t.Fatal("stream ended without decode error")
	})

	t.Run("decode completed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
			_, _ = io.WriteString(w, "event: response.completed\ndata: {bad-json\n\n")
		}))
		defer srv.Close()

		c := NewWithBaseURL(freshAuth(t), srv.URL)
		seq, err := c.Stream(context.Background(), agentmodel.ChatRequest{
			Model:    "gpt-5",
			Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
		})
		if err != nil {
			t.Fatalf("Stream setup: %v", err)
		}
		var sawDelta bool
		for ch, err := range seq {
			if err != nil {
				if !sawDelta || !strings.Contains(err.Error(), "decode completed") {
					t.Fatalf("stream err = %v sawDelta=%v, want decode completed after delta", err, sawDelta)
				}
				return
			}
			if ch.Delta.Content == "hi" {
				sawDelta = true
			}
		}
		t.Fatal("stream ended without completed decode error")
	})

	t.Run("premature eof without completed", func(t *testing.T) {
		// Deltas arrive but the backend drops the stream before
		// response.completed. The provider must surface an explicit error
		// rather than ending silently with no finish_reason.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
		}))
		defer srv.Close()

		c := NewWithBaseURL(freshAuth(t), srv.URL)
		seq, err := c.Stream(context.Background(), agentmodel.ChatRequest{
			Model:    "gpt-5",
			Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
		})
		if err != nil {
			t.Fatalf("Stream setup: %v", err)
		}
		var gotErr error
		for _, err := range seq {
			if err != nil {
				gotErr = err
			}
		}
		if gotErr == nil || !strings.Contains(gotErr.Error(), "ended without response.completed") {
			t.Fatalf("stream err = %v, want ended without response.completed", gotErr)
		}
	})

	t.Run("terminal failure event", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.failed\ndata: {\"response\":{\"error\":{\"message\":\"upstream boom\"}}}\n\n")
		}))
		defer srv.Close()

		c := NewWithBaseURL(freshAuth(t), srv.URL)
		seq, err := c.Stream(context.Background(), agentmodel.ChatRequest{
			Model:    "gpt-5",
			Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
		})
		if err != nil {
			t.Fatalf("Stream setup: %v", err)
		}
		var gotErr error
		for _, err := range seq {
			if err != nil {
				gotErr = err
			}
		}
		if gotErr == nil || !strings.Contains(gotErr.Error(), "response.failed") || !strings.Contains(gotErr.Error(), "upstream boom") {
			t.Fatalf("stream err = %v, want response.failed: upstream boom", gotErr)
		}
	})
}

func TestStream_RetriesTransientEmptyStream(t *testing.T) {
	t.Run("recovers on a later attempt", func(t *testing.T) {
		var calls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			if atomic.AddInt32(&calls, 1) == 1 {
				// Transient empty stream: 200 then immediate close, no events.
				return
			}
			_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
			_, _ = io.WriteString(w, "event: response.completed\ndata: {\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
		}))
		defer srv.Close()

		c := NewWithBaseURL(freshAuth(t), srv.URL)
		c.streamRetryWait = time.Millisecond
		seq, err := c.Stream(context.Background(), agentmodel.ChatRequest{
			Model:    "gpt-5",
			Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
		})
		if err != nil {
			t.Fatalf("Stream setup: %v", err)
		}
		var content, finish string
		for ch, err := range seq {
			if err != nil {
				t.Fatalf("unexpected stream err: %v", err)
			}
			content += ch.Delta.Content
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
		}
		if content != "hi" || finish != "stop" {
			t.Fatalf("content=%q finish=%q, want hi/stop", content, finish)
		}
		if n := atomic.LoadInt32(&calls); n != 2 {
			t.Fatalf("upstream calls = %d, want 2 (1 empty + 1 retry)", n)
		}
	})

	t.Run("surfaces error after retries exhausted", func(t *testing.T) {
		var calls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			w.Header().Set("Content-Type", "text/event-stream")
			// Always a transient empty stream.
		}))
		defer srv.Close()

		c := NewWithBaseURL(freshAuth(t), srv.URL)
		c.streamRetries = 2
		c.streamRetryWait = time.Millisecond
		seq, err := c.Stream(context.Background(), agentmodel.ChatRequest{
			Model:    "gpt-5",
			Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
		})
		if err != nil {
			t.Fatalf("Stream setup: %v", err)
		}
		var gotErr error
		for _, err := range seq {
			if err != nil {
				gotErr = err
			}
		}
		if gotErr == nil || !strings.Contains(gotErr.Error(), "ended without response.completed") {
			t.Fatalf("stream err = %v, want ended without response.completed", gotErr)
		}
		if n := atomic.LoadInt32(&calls); n != 3 {
			t.Fatalf("upstream calls = %d, want 3 (1 + 2 retries)", n)
		}
	})

	t.Run("does not retry after content emitted", func(t *testing.T) {
		var calls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			w.Header().Set("Content-Type", "text/event-stream")
			// Emit content, then drop without response.completed. Because a
			// chunk already reached the consumer, retrying would duplicate
			// output, so the drop must surface as an error instead.
			_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
		}))
		defer srv.Close()

		c := NewWithBaseURL(freshAuth(t), srv.URL)
		c.streamRetryWait = time.Millisecond
		seq, err := c.Stream(context.Background(), agentmodel.ChatRequest{
			Model:    "gpt-5",
			Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
		})
		if err != nil {
			t.Fatalf("Stream setup: %v", err)
		}
		var gotErr error
		for _, err := range seq {
			if err != nil {
				gotErr = err
			}
		}
		if gotErr == nil || !strings.Contains(gotErr.Error(), "ended without response.completed") {
			t.Fatalf("stream err = %v, want ended without response.completed", gotErr)
		}
		if n := atomic.LoadInt32(&calls); n != 1 {
			t.Fatalf("upstream calls = %d, want 1 (no retry after emit)", n)
		}
	})
}

type failingAuth struct{}

func (failingAuth) Mode() string { return agentmodel.AuthModeSubscription }

func (failingAuth) Apply(context.Context, *http.Request) error {
	return errors.New("apply failed")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestBuildRequest_OmitsCodexUnsupportedParams asserts the codex-incompatible
// sampling parameters are never forwarded, even when the caller supplies them.
// The codex backend rejects max_output_tokens / temperature / top_p with a 400.
func TestBuildRequest_OmitsCodexUnsupportedParams(t *testing.T) {
	temp, topP, maxTok := 0.7, 0.9, 64
	r, err := buildRequest(agentmodel.ChatRequest{
		Model:       "gpt-5.5",
		Messages:    []agentmodel.Message{{Role: "user", Content: "hi"}},
		Temperature: &temp,
		TopP:        &topP,
		MaxTokens:   &maxTok,
	}, true)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if r.Temperature != nil {
		t.Errorf("temperature forwarded: %v, want omitted", *r.Temperature)
	}
	if r.TopP != nil {
		t.Errorf("top_p forwarded: %v, want omitted", *r.TopP)
	}
	if r.MaxTokens != nil {
		t.Errorf("max_output_tokens forwarded: %v, want omitted", *r.MaxTokens)
	}
}

func TestComplete_RetriesOnExpiredToken(t *testing.T) {
	// Auth file holds a *fresh* token, so the first attempt uses it directly
	// (no proactive refresh). The API rejects it as token_expired; the provider
	// must force one refresh and retry, succeeding on the second attempt.
	var refreshHits int32
	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&refreshHits, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "refreshed-token",
			"refresh_token": "new-rt",
			"expires_in":    3600,
		})
	}))
	defer authSrv.Close()

	var calls int32
	var retryAuth string
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"Provided authentication token is expired.","code":"token_expired"}}`)
			return
		}
		retryAuth = r.Header.Get("Authorization")
		writeSSE(w, []string{"ok"}, 1, 1, 2)
	}))
	defer apiSrv.Close()

	dir := t.TempDir()
	writeAuthFile(t, dir, map[string]any{
		"access_token":  "fresh-but-server-rejects",
		"refresh_token": "old-rt",
		"expires_at_ms": time.Now().Add(time.Hour).UnixMilli(),
	})
	a := auth.NewChatGPTOAuth(dir, &http.Client{Timeout: 5 * time.Second})
	a.OverrideURLs(authSrv.URL+"/devicecode", authSrv.URL+"/devicetoken", authSrv.URL+"/oauth/token")

	c := NewWithBaseURL(a, apiSrv.URL)
	resp, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "gpt-5",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := resp.Choices[0].Message.Content; got != "ok" {
		t.Errorf("content = %q, want ok", got)
	}
	if got := atomic.LoadInt32(&refreshHits); got != 1 {
		t.Errorf("refresh hits = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("api calls = %d, want 2 (401 then 200)", got)
	}
	if retryAuth != "Bearer refreshed-token" {
		t.Errorf("retry Authorization = %q, want Bearer refreshed-token", retryAuth)
	}
}

func TestComplete_NonExpired401NoRetry(t *testing.T) {
	// A 401 that is not an expired token (e.g. a revoked credential) must not
	// trigger a refresh; it surfaces as an error after a single attempt. Uses
	// freshAuth so this also asserts no network refresh is attempted.
	var calls int32
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"unauthorized"}}`)
	}))
	defer apiSrv.Close()

	c := NewWithBaseURL(freshAuth(t), apiSrv.URL)
	_, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "gpt-5",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("Complete err = %v, want 401 error", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("api calls = %d, want 1 (no refresh/retry)", got)
	}
}

func TestStream_RetriesOnExpiredToken(t *testing.T) {
	var refreshHits int32
	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&refreshHits, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "refreshed-token",
			"refresh_token": "new-rt",
			"expires_in":    3600,
		})
	}))
	defer authSrv.Close()

	var calls int32
	var retryAuth string
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"code":"token_expired"}}`)
			return
		}
		retryAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	}))
	defer apiSrv.Close()

	dir := t.TempDir()
	writeAuthFile(t, dir, map[string]any{
		"access_token":  "fresh-but-server-rejects",
		"refresh_token": "old-rt",
		"expires_at_ms": time.Now().Add(time.Hour).UnixMilli(),
	})
	a := auth.NewChatGPTOAuth(dir, &http.Client{Timeout: 5 * time.Second})
	a.OverrideURLs(authSrv.URL+"/devicecode", authSrv.URL+"/devicetoken", authSrv.URL+"/oauth/token")

	c := NewWithBaseURL(a, apiSrv.URL)
	seq, err := c.Stream(context.Background(), agentmodel.ChatRequest{
		Model:    "gpt-5",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var content, finish string
	for ch, err := range seq {
		if err != nil {
			t.Fatalf("stream err: %v", err)
		}
		content += ch.Delta.Content
		if ch.FinishReason != "" {
			finish = ch.FinishReason
		}
	}
	if content != "hi" || finish != "stop" {
		t.Errorf("content=%q finish=%q, want hi/stop", content, finish)
	}
	if got := atomic.LoadInt32(&refreshHits); got != 1 {
		t.Errorf("refresh hits = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("api calls = %d, want 2 (401 then SSE)", got)
	}
	if retryAuth != "Bearer refreshed-token" {
		t.Errorf("retry Authorization = %q, want Bearer refreshed-token", retryAuth)
	}
}

// ─── session-id / prompt cache stickiness (issue #1279) ──────────────────────

// The ChatGPT/codex backend keys its prompt cache on the session-id request
// header, not the body prompt_cache_key. stableSessionID must yield the SAME id
// across consecutive turns of one conversation (so every turn routes to the same
// cache node) while differing across distinct conversations.
func TestStableSessionID_StableAcrossTurns(t *testing.T) {
	sys := agentmodel.Message{Role: "system", Content: "you are a big stable system prompt"}
	first := agentmodel.Message{Role: "user", Content: "the first message anchors the conversation"}
	turn1 := agentmodel.ChatRequest{
		Messages: []agentmodel.Message{sys, first,
			{Role: "assistant", Content: "ok"}},
		Tools: []agentmodel.Tool{{Type: "function", Function: agentmodel.FunctionSchema{Name: "read"}}},
	}
	// turn2: same system+tools+first message, but the history has grown.
	turn2 := agentmodel.ChatRequest{
		Messages: []agentmodel.Message{sys, first,
			{Role: "assistant", Content: "ok"},
			{Role: "user", Content: "another turn"},
			{Role: "assistant", Content: "sure"}},
		Tools: turn1.Tools,
	}
	if a, b := stableSessionID(turn1), stableSessionID(turn2); a != b {
		t.Errorf("session id changed across turns of the same conversation: %q vs %q", a, b)
	}
}

func TestStableSessionID_DiffersByConversation(t *testing.T) {
	base := agentmodel.ChatRequest{Messages: []agentmodel.Message{
		{Role: "system", Content: "sys A"}, {Role: "user", Content: "first msg A"}}}
	diffSystem := agentmodel.ChatRequest{Messages: []agentmodel.Message{
		{Role: "system", Content: "sys B"}, {Role: "user", Content: "first msg A"}}}
	diffFirst := agentmodel.ChatRequest{Messages: []agentmodel.Message{
		{Role: "system", Content: "sys A"}, {Role: "user", Content: "first msg B"}}}
	id := stableSessionID(base)
	if id == stableSessionID(diffSystem) {
		t.Error("different system prompt should yield a different session id")
	}
	if id == stableSessionID(diffFirst) {
		t.Error("different first user message should yield a different session id")
	}
}

func TestStableSessionID_RespectsExplicitKey(t *testing.T) {
	req := agentmodel.ChatRequest{
		PromptCacheKey: "client-session-42",
		Messages:       []agentmodel.Message{{Role: "user", Content: "hi"}},
	}
	if got := stableSessionID(req); got != "client-session-42" {
		t.Errorf("explicit PromptCacheKey not honored: got %q", got)
	}
}

func TestStableSessionID_UUIDShape(t *testing.T) {
	id := stableSessionID(agentmodel.ChatRequest{Messages: []agentmodel.Message{{Role: "user", Content: "hi"}}})
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		t.Errorf("session id is not UUID-shaped: %q", id)
	}
}

// The stream request must carry a stable session-id header so the codex backend
// serves the cached prompt prefix from the same node on later turns.
func TestStream_SendsSessionIDHeader(t *testing.T) {
	var gotSession string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSession = r.Header.Get("session-id")
		writeSSE(w, []string{"hi"}, 5, 1, 6)
	}))
	defer srv.Close()

	req := agentmodel.ChatRequest{
		Model: "gpt-5",
		Messages: []agentmodel.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "hello"},
		},
	}
	c := NewWithBaseURL(freshAuth(t), srv.URL)
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotSession == "" {
		t.Fatal("session-id header not sent")
	}
	if want := stableSessionID(req); gotSession != want {
		t.Errorf("session-id header = %q, want %q", gotSession, want)
	}
}

// A ChatGPT subscription 429 is the "you are out of quota until X" signal for a
// pooled Codex account. Surfacing when the window rolls is what lets the pool
// park that account for the window instead of re-probing it every 5 minutes.
func TestComplete_RateLimitCarriesRetryAfterHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "900")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error"}}`)
	}))
	defer srv.Close()
	c := NewWithBaseURL(freshAuth(t), srv.URL)

	_, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "gpt-5",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected a rate-limit error")
	}
	ae := agentmodel.Wrap(err)
	if ae.Type != agentmodel.ErrTypeRateLimit {
		t.Fatalf("Type=%q, want %q", ae.Type, agentmodel.ErrTypeRateLimit)
	}
	if ae.RetryAfter != 15*time.Minute {
		t.Errorf("RetryAfter=%v, want 15m", ae.RetryAfter)
	}
}

// The codex backend reports subscription exhaustion in the body
// (usage_limit_reached + resets_in_seconds) rather than a Retry-After header.
func TestComplete_UsageLimitBodyCarriesResetHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","resets_in_seconds":7200}}`)
	}))
	defer srv.Close()
	c := NewWithBaseURL(freshAuth(t), srv.URL)

	_, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "gpt-5",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected a rate-limit error")
	}
	if got := agentmodel.Wrap(err).RetryAfter; got != 2*time.Hour {
		t.Errorf("RetryAfter=%v, want 2h from resets_in_seconds", got)
	}
}

func TestComplete_NonRateLimitCarriesNoHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "900")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"type":"server_error"}}`)
	}))
	defer srv.Close()
	c := NewWithBaseURL(freshAuth(t), srv.URL)

	_, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "gpt-5",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := agentmodel.Wrap(err).RetryAfter; got != 0 {
		t.Errorf("RetryAfter=%v, want 0 on a non-429", got)
	}
}
