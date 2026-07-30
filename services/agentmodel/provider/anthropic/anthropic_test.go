package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
)

// writeRefreshableAuth writes an agent-model native auth.json under dir for the
// AnthropicOAuthRefreshable authenticator used by the 401-recovery tests.
func writeRefreshableAuth(t *testing.T, dir, access, refresh string, expiresMs int64) {
	t.Helper()
	data := map[string]any{
		"schema": "agentmodel.oauth/v1", "provider": "anthropic",
		"access_token": access, "refresh_token": refresh, "expires_at_ms": expiresMs,
	}
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		t.Fatalf("marshal auth: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), b, 0o600); err != nil {
		t.Fatalf("write auth: %v", err)
	}
}

// newRefreshableAuth returns an AnthropicOAuthRefreshable backed by a fresh
// on-disk token, with its OAuth refresh endpoint pointed at oauthURL.
func newRefreshableAuth(t *testing.T, oauthURL, access, refresh string) *auth.AnthropicOAuthRefreshable {
	t.Helper()
	dir := t.TempDir()
	writeRefreshableAuth(t, dir, access, refresh, time.Now().Add(time.Hour).UnixMilli())
	a := auth.NewAnthropicOAuthRefreshable(dir, &http.Client{Timeout: 5 * time.Second})
	a.SetEndpointsForTest(oauthURL, "")
	return a
}

// newRefreshOAuthServer returns a fake Anthropic OAuth token endpoint that
// hands back newAccess and counts how many times it was hit.
func newRefreshOAuthServer(t *testing.T, newAccess string, hits *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  newAccess,
			"refresh_token": "rt-new",
			"expires_in":    3600,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeAnthropic captures requests and returns canned responses.
type fakeAnthropic struct {
	srv         *httptest.Server
	gotMethod   string
	gotPath     string
	gotHeaders  http.Header
	gotBody     []byte
	respStatus  int
	respBody    string
	respHeaders map[string]string
}

func newFakeAnthropic(t *testing.T, respBody string) *fakeAnthropic {
	t.Helper()
	f := &fakeAnthropic{
		respStatus:  http.StatusOK,
		respBody:    respBody,
		respHeaders: map[string]string{"Content-Type": "application/json"},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.gotMethod = r.Method
		f.gotPath = r.URL.Path
		f.gotHeaders = r.Header.Clone()
		body, _ := io.ReadAll(r.Body)
		f.gotBody = body
		for k, v := range f.respHeaders {
			w.Header().Set(k, v)
		}
		w.WriteHeader(f.respStatus)
		_, _ = io.WriteString(w, f.respBody)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newAPIKeyAuth() *auth.StaticKey {
	return &auth.StaticKey{HeaderName: "x-api-key", Prefix: "", Token: "sk-ant-api03-test"}
}

func newOAuthAuth() *auth.AnthropicOAuth {
	return auth.NewAnthropicOAuth("sk-ant-oat-fake-token-123")
}

// --- Test 1: Complete with system + user message ----------------------------

func TestComplete_RejectsNonTextContentParts(t *testing.T) {
	c := NewWithBaseURL(newAPIKeyAuth(), "http://127.0.0.1")
	var req agentmodel.ChatRequest
	if err := json.Unmarshal([]byte(`{
		"model":"claude-3-5-sonnet-latest",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"describe this"},
			{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}
		]}]
	}`), &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}

	_, err := c.Complete(context.Background(), req)
	if err == nil {
		t.Fatal("expected non-text content part to be rejected")
	}
	if ae := agentmodel.Wrap(err); ae.Type != agentmodel.ErrTypeInvalidRequest {
		t.Fatalf("error type = %q, want %q; err=%v", ae.Type, agentmodel.ErrTypeInvalidRequest, err)
	}
}

func TestComplete_SystemExtractedFromMessages(t *testing.T) {
	respBody := `{
		"id": "msg_123",
		"type": "message",
		"role": "assistant",
		"model": "claude-3-5-sonnet-latest",
		"content": [{"type":"text","text":"Hello there!"}],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 12, "output_tokens": 5}
	}`
	fake := newFakeAnthropic(t, respBody)
	c := NewWithBaseURL(newAPIKeyAuth(), fake.srv.URL)

	req := agentmodel.ChatRequest{
		Model: "claude-3-5-sonnet-latest",
		Messages: []agentmodel.Message{
			{Role: "system", Content: "You are helpful."},
			{Role: "user", Content: "Hi!"},
		},
	}
	resp, err := c.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Verify outgoing request shape: system extracted, messages without system.
	if fake.gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", fake.gotMethod)
	}
	if fake.gotPath != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", fake.gotPath)
	}
	if v := fake.gotHeaders.Get("anthropic-version"); v != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want 2023-06-01", v)
	}
	var sent map[string]any
	if err := json.Unmarshal(fake.gotBody, &sent); err != nil {
		t.Fatalf("decode req body: %v\nbody=%s", err, fake.gotBody)
	}
	if got := sent["system"]; got != "You are helpful." {
		t.Errorf("system field = %v, want %q", got, "You are helpful.")
	}
	msgs, ok := sent["messages"].([]any)
	if !ok {
		t.Fatalf("messages not array: %T", sent["messages"])
	}
	if len(msgs) != 1 {
		t.Fatalf("messages len = %d, want 1 (system stripped)", len(msgs))
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "user" {
		t.Errorf("first msg role = %v, want user", first["role"])
	}
	// max_tokens must be present (default applied) since Anthropic requires it.
	if _, ok := sent["max_tokens"]; !ok {
		t.Errorf("max_tokens missing in request; Anthropic requires it")
	}

	// Verify response shape.
	if len(resp.Choices) != 1 {
		t.Fatalf("choices len = %d, want 1", len(resp.Choices))
	}
	if resp.Choices[0].Message.Content != "Hello there!" {
		t.Errorf("content = %q, want %q", resp.Choices[0].Message.Content, "Hello there!")
	}
	if resp.Choices[0].Message.Role != "assistant" {
		t.Errorf("role = %q, want assistant", resp.Choices[0].Message.Role)
	}
	if resp.Usage.PromptTokens != 12 {
		t.Errorf("PromptTokens = %d, want 12", resp.Usage.PromptTokens)
	}
	if resp.Usage.CompletionTokens != 5 {
		t.Errorf("CompletionTokens = %d, want 5", resp.Usage.CompletionTokens)
	}
	if resp.Usage.TotalTokens != 17 {
		t.Errorf("TotalTokens = %d, want 17", resp.Usage.TotalTokens)
	}
}

// --- Test 2: Tool call request shape + tool_use response translation -------

func TestComplete_ToolCall_RequestAndResponseShape(t *testing.T) {
	respBody := `{
		"id": "msg_456",
		"type": "message",
		"role": "assistant",
		"model": "claude-3-5-sonnet-latest",
		"content": [
			{"type":"tool_use","id":"toolu_abc","name":"get_weather","input":{"location":"SF","units":"c"}}
		],
		"stop_reason": "tool_use",
		"usage": {"input_tokens": 30, "output_tokens": 18}
	}`
	fake := newFakeAnthropic(t, respBody)
	c := NewWithBaseURL(newAPIKeyAuth(), fake.srv.URL)

	req := agentmodel.ChatRequest{
		Model: "claude-3-5-sonnet-latest",
		Messages: []agentmodel.Message{
			{Role: "user", Content: "Weather in SF?"},
		},
		Tools: []agentmodel.Tool{
			{
				Type: "function",
				Function: agentmodel.FunctionSchema{
					Name:        "get_weather",
					Description: "Lookup weather",
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"location": map[string]any{"type": "string"},
						},
					},
				},
			},
		},
	}
	resp, err := c.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Outgoing request: tools shape must be Anthropic-flavored (name/description/input_schema).
	var sent map[string]any
	if err := json.Unmarshal(fake.gotBody, &sent); err != nil {
		t.Fatalf("decode req body: %v", err)
	}
	tools, ok := sent["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools shape wrong: %v", sent["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "get_weather" {
		t.Errorf("tool name = %v, want get_weather", tool["name"])
	}
	if tool["description"] != "Lookup weather" {
		t.Errorf("tool desc = %v", tool["description"])
	}
	if _, ok := tool["input_schema"]; !ok {
		t.Errorf("tool missing input_schema; got keys: %v", keys(tool))
	}
	if _, ok := tool["parameters"]; ok {
		t.Errorf("tool should not have OpenAI-style 'parameters'; got: %v", tool["parameters"])
	}
	// Should not have "type":"function" wrapper used by OpenAI.
	if _, ok := tool["function"]; ok {
		t.Errorf("tool should be flat; should not contain nested 'function'")
	}

	// Response: tool_use → tool_calls translation.
	if len(resp.Choices) != 1 {
		t.Fatalf("choices len = %d, want 1", len(resp.Choices))
	}
	choice := resp.Choices[0]
	if choice.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", choice.FinishReason)
	}
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls len = %d, want 1", len(choice.Message.ToolCalls))
	}
	tc := choice.Message.ToolCalls[0]
	if tc.ID != "toolu_abc" {
		t.Errorf("tool_call.id = %q, want toolu_abc", tc.ID)
	}
	if tc.Type != "function" {
		t.Errorf("tool_call.type = %q, want function", tc.Type)
	}
	if tc.Function.Name != "get_weather" {
		t.Errorf("tool_call.function.name = %q, want get_weather", tc.Function.Name)
	}
	// Arguments must be a JSON-encoded string.
	if tc.Function.Arguments == "" {
		t.Fatalf("Arguments empty, want JSON string")
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &parsed); err != nil {
		t.Fatalf("Arguments not valid JSON string: %q (%v)", tc.Function.Arguments, err)
	}
	if parsed["location"] != "SF" {
		t.Errorf("Arguments.location = %v, want SF", parsed["location"])
	}
}

// --- Test 3: Streaming SSE → OpenAI delta chunks ----------------------------

func TestStream_SSEEventsTranslatedToDeltas(t *testing.T) {
	// Anthropic SSE event sequence: message_start, content_block_start,
	// content_block_delta (text), content_block_stop, message_delta (usage),
	// message_stop.
	sseBody := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_s1","model":"claude-3-5-sonnet-latest","role":"assistant","usage":{"input_tokens":7,"output_tokens":0}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
		"",
	}, "\n")
	fake := newFakeAnthropic(t, sseBody)
	fake.respHeaders = map[string]string{"Content-Type": "text/event-stream"}
	c := NewWithBaseURL(newAPIKeyAuth(), fake.srv.URL)

	req := agentmodel.ChatRequest{
		Model:    "claude-3-5-sonnet-latest",
		Stream:   true,
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	}
	seq, err := c.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var contents []string
	var finalUsage *agentmodel.Usage
	var finishReason string
	for ch, err := range seq {
		if err != nil {
			t.Fatalf("stream chunk err: %v", err)
		}
		if ch.Delta.Content != "" {
			contents = append(contents, ch.Delta.Content)
		}
		if ch.FinishReason != "" {
			finishReason = ch.FinishReason
		}
		if ch.Usage != nil {
			finalUsage = ch.Usage
		}
	}

	// Verify request was streaming.
	var sent map[string]any
	if err := json.Unmarshal(fake.gotBody, &sent); err != nil {
		t.Fatalf("decode req body: %v", err)
	}
	if sent["stream"] != true {
		t.Errorf("stream = %v, want true", sent["stream"])
	}

	got := strings.Join(contents, "")
	if got != "Hello world" {
		t.Errorf("concat content = %q, want %q", got, "Hello world")
	}
	if finishReason == "" {
		t.Errorf("no finish_reason emitted")
	}
	if finalUsage == nil {
		t.Fatalf("no final usage emitted")
	}
	if finalUsage.PromptTokens != 7 {
		t.Errorf("PromptTokens = %d, want 7", finalUsage.PromptTokens)
	}
	if finalUsage.CompletionTokens != 4 {
		t.Errorf("CompletionTokens = %d, want 4", finalUsage.CompletionTokens)
	}
	if finalUsage.TotalTokens != 11 {
		t.Errorf("TotalTokens = %d, want 11", finalUsage.TotalTokens)
	}
}

// TestStream_ToolCall_FirstFragmentOnly asserts the OpenAI-conformant streamed
// tool-call shape (issue #664, fix B): id/type/name appear exactly once, on the
// first fragment; every fragment carries an integer index; later fragments carry
// only {index, arguments}. Re-sending id/name on every fragment (the old bug)
// makes the official OpenAI accumulators concatenate "get_weatherget_weather".
func TestStream_ToolCall_FirstFragmentOnly(t *testing.T) {
	sseBody := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_t1","model":"claude-3-5-sonnet-latest","role":"assistant","usage":{"input_tokens":9,"output_tokens":0}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_abc","name":"get_weather","input":{}}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"location\":"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"SF\"}"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":6}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
		"",
	}, "\n")
	fake := newFakeAnthropic(t, sseBody)
	fake.respHeaders = map[string]string{"Content-Type": "text/event-stream"}
	c := NewWithBaseURL(newAPIKeyAuth(), fake.srv.URL)

	seq, err := c.Stream(context.Background(), agentmodel.ChatRequest{
		Model:    "claude-3-5-sonnet-latest",
		Stream:   true,
		Messages: []agentmodel.Message{{Role: "user", Content: "weather?"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var fragments []agentmodel.ToolCall
	for ch, err := range seq {
		if err != nil {
			t.Fatalf("stream chunk err: %v", err)
		}
		fragments = append(fragments, ch.Delta.ToolCalls...)
	}

	if len(fragments) < 3 {
		t.Fatalf("expected >=3 tool-call fragments (start + 2 arg deltas), got %d", len(fragments))
	}

	// Every fragment must carry an integer index.
	for i, f := range fragments {
		if f.Index == nil {
			t.Errorf("fragment %d: Index is nil, want a pointer to an int", i)
		} else if *f.Index != 0 {
			t.Errorf("fragment %d: Index = %d, want 0", i, *f.Index)
		}
	}

	// First fragment carries id/type/name; later fragments must NOT repeat them.
	first := fragments[0]
	if first.ID != "toolu_abc" || first.Type != "function" || first.Function.Name != "get_weather" {
		t.Errorf("first fragment id/type/name = %q/%q/%q, want toolu_abc/function/get_weather",
			first.ID, first.Type, first.Function.Name)
	}
	var args strings.Builder
	args.WriteString(first.Function.Arguments)
	for i, f := range fragments[1:] {
		if f.ID != "" || f.Type != "" || f.Function.Name != "" {
			t.Errorf("fragment %d repeats id/type/name (id=%q type=%q name=%q); must be args-only",
				i+1, f.ID, f.Type, f.Function.Name)
		}
		args.WriteString(f.Function.Arguments)
	}

	// Concatenated arguments must reconstruct valid JSON matching the input.
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args.String()), &parsed); err != nil {
		t.Fatalf("accumulated arguments %q not valid JSON: %v", args.String(), err)
	}
	if parsed["location"] != "SF" {
		t.Errorf("accumulated arguments location = %v, want SF", parsed["location"])
	}
}

// --- Test 4: API key mode sets x-api-key, not Authorization ----------------

func TestComplete_APIKeyMode_HeadersCorrect(t *testing.T) {
	respBody := `{"id":"msg_x","type":"message","role":"assistant","model":"claude-3-5-haiku-latest","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	fake := newFakeAnthropic(t, respBody)
	c := NewWithBaseURL(newAPIKeyAuth(), fake.srv.URL)

	if c.AuthMode() != agentmodel.AuthModeAPIKey {
		t.Errorf("AuthMode = %q, want %q", c.AuthMode(), agentmodel.AuthModeAPIKey)
	}

	_, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "claude-3-5-haiku-latest",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if got := fake.gotHeaders.Get("x-api-key"); got != "sk-ant-api03-test" {
		t.Errorf("x-api-key = %q, want sk-ant-api03-test", got)
	}
	if got := fake.gotHeaders.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want empty in API-key mode", got)
	}
}

// --- Test 5: OAuth (subscription) mode ---

func TestComplete_OAuthMode_HeadersCorrect(t *testing.T) {
	respBody := `{"id":"msg_x","type":"message","role":"assistant","model":"claude-3-5-sonnet-latest","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	fake := newFakeAnthropic(t, respBody)
	c := NewWithBaseURL(newOAuthAuth(), fake.srv.URL)

	if c.AuthMode() != agentmodel.AuthModeSubscription {
		t.Errorf("AuthMode = %q, want %q", c.AuthMode(), agentmodel.AuthModeSubscription)
	}

	_, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "claude-3-5-sonnet-latest",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if got := fake.gotHeaders.Get("Authorization"); got != "Bearer sk-ant-oat-fake-token-123" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer sk-ant-oat-fake-token-123")
	}
	if got := fake.gotHeaders.Get("x-api-key"); got != "" {
		t.Errorf("x-api-key = %q, want empty in OAuth mode", got)
	}
	beta := fake.gotHeaders.Get("anthropic-beta")
	if !strings.Contains(beta, auth.AnthropicOAuthBetaHeader) {
		t.Errorf("anthropic-beta = %q, want to contain %q", beta, auth.AnthropicOAuthBetaHeader)
	}
}

// --- Test 6: Embed returns ErrNotSupported ----------------------------------

func TestEmbed_NotSupported(t *testing.T) {
	c := New(newAPIKeyAuth())
	_, err := c.Embed(context.Background(), agentmodel.EmbeddingRequest{Model: "x", Input: []string{"a"}})
	if !errors.Is(err, provider.ErrNotSupported) {
		t.Errorf("Embed err = %v, want ErrNotSupported", err)
	}
}

// --- Misc -------------------------------------------------------------------

func TestName(t *testing.T) {
	c := New(newAPIKeyAuth())
	if c.Name() != "anthropic" {
		t.Errorf("Name = %q, want anthropic", c.Name())
	}
}

func TestSupportedModels(t *testing.T) {
	c := New(newAPIKeyAuth())
	models := c.SupportedModels()
	want := []string{"claude-3-5-sonnet-latest", "claude-3-5-haiku-latest", "claude-3-7-sonnet-latest"}
	have := map[string]bool{}
	for _, m := range models {
		have[m] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("missing model %q in %v", w, models)
		}
	}
}

func TestCompleteErrorBranches(t *testing.T) {
	t.Run("http status", func(t *testing.T) {
		fake := newFakeAnthropic(t, `{"error":"rate limited"}`)
		fake.respStatus = http.StatusTooManyRequests
		c := NewWithBaseURL(newAPIKeyAuth(), fake.srv.URL)
		_, err := c.Complete(context.Background(), agentmodel.ChatRequest{
			Model:    "claude-3-5-sonnet-latest",
			Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
		})
		if err == nil || !strings.Contains(err.Error(), "status 429") {
			t.Fatalf("Complete status err = %v", err)
		}
	})

	t.Run("decode response", func(t *testing.T) {
		fake := newFakeAnthropic(t, `{bad-json`)
		c := NewWithBaseURL(newAPIKeyAuth(), fake.srv.URL)
		_, err := c.Complete(context.Background(), agentmodel.ChatRequest{
			Model:    "claude-3-5-sonnet-latest",
			Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
		})
		if err == nil || !strings.Contains(err.Error(), "decode response") {
			t.Fatalf("Complete decode err = %v", err)
		}
	})
}

func TestStreamStatusError(t *testing.T) {
	fake := newFakeAnthropic(t, `stream down`)
	fake.respStatus = http.StatusServiceUnavailable
	c := NewWithBaseURL(newAPIKeyAuth(), fake.srv.URL)
	_, err := c.Stream(context.Background(), agentmodel.ChatRequest{
		Model:    "claude-3-5-sonnet-latest",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "stream status 503") {
		t.Fatalf("Stream status err = %v", err)
	}
}

func TestListModelsAdditionalBranches(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		fake := newFakeAnthropic(t, `{"data":[{"id":"claude-a"},{"id":"claude-b"}]}`)
		c := NewWithBaseURL(newAPIKeyAuth(), fake.srv.URL)
		ids, err := c.ListModels(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if fake.gotMethod != http.MethodGet || fake.gotPath != "/v1/models" {
			t.Fatalf("list models request = %s %s", fake.gotMethod, fake.gotPath)
		}
		if strings.Join(ids, ",") != "claude-a,claude-b" {
			t.Fatalf("ids = %#v", ids)
		}
	})
	t.Run("http error", func(t *testing.T) {
		fake := newFakeAnthropic(t, `nope`)
		fake.respStatus = http.StatusInternalServerError
		c := NewWithBaseURL(newAPIKeyAuth(), fake.srv.URL)
		_, err := c.ListModels(context.Background())
		if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
			t.Fatalf("ListModels http err = %v", err)
		}
	})
	t.Run("decode error", func(t *testing.T) {
		fake := newFakeAnthropic(t, `{bad-json`)
		c := NewWithBaseURL(newAPIKeyAuth(), fake.srv.URL)
		_, err := c.ListModels(context.Background())
		if err == nil || !strings.Contains(err.Error(), "decode list-models") {
			t.Fatalf("ListModels decode err = %v", err)
		}
	})
}

// TestNormalizeToolChoice covers the OpenAI -> Anthropic tool_choice mapping,
// including the two shapes that 400 on Anthropic when left untranslated (#664):
// "required" and the named-function object form.
func TestNormalizeToolChoice(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want any
	}{
		{"auto", "auto", map[string]any{"type": "auto"}},
		{"none", "none", map[string]any{"type": "none"}},
		{"required maps to any", "required", map[string]any{"type": "any"}},
		{
			"named function maps to tool",
			map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}},
			map[string]any{"type": "tool", "name": "get_weather"},
		},
		{"unknown string passes through", "custom", "custom"},
		{"nil passes through", nil, nil},
		{
			"already-anthropic object passes through",
			map[string]any{"type": "any"},
			map[string]any{"type": "any"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := normalizeToolChoice(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("normalizeToolChoice(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// TestMapStopReason covers the clamp of Anthropic stop_reason onto OpenAI's
// closed finish_reason enum (#664, fix C): unknown values become "stop".
func TestMapStopReason(t *testing.T) {
	cases := map[string]string{
		"end_turn":                      "stop",
		"stop_sequence":                 "stop",
		"pause_turn":                    "stop",
		"max_tokens":                    "length",
		"model_context_window_exceeded": "length",
		"tool_use":                      "tool_calls",
		"refusal":                       "content_filter",
		"some_new_anthropic_reason":     "stop",
		"":                              "stop",
	}
	for in, want := range cases {
		if got := mapStopReason(in); got != want {
			t.Errorf("mapStopReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTranslationEdgeBranches(t *testing.T) {
	if got := normalizeToolChoice("auto").(map[string]any)["type"]; got != "auto" {
		t.Fatalf("normalizeToolChoice(auto) = %v", got)
	}
	if got := normalizeToolChoice("custom"); got != "custom" {
		t.Fatalf("normalizeToolChoice(custom) = %v", got)
	}

	toolMsg := toAnthropicMessage(agentmodel.Message{Role: "tool", ToolCallID: "toolu_1", Content: "done"})
	if toolMsg.Role != "user" || len(toolMsg.Content) != 1 || toolMsg.Content[0].Type != "tool_result" {
		t.Fatalf("tool message = %#v", toolMsg)
	}

	msg := toAnthropicMessage(agentmodel.Message{
		Role: "assistant",
		ToolCalls: []agentmodel.ToolCall{{
			ID: "call_1",
			Function: agentmodel.ToolCallFunction{
				Name:      "broken_args",
				Arguments: `{bad-json`,
			},
		}},
	})
	if len(msg.Content) != 1 || msg.Content[0].Type != "tool_use" || msg.Content[0].Input != nil {
		t.Fatalf("assistant tool-use message = %#v", msg)
	}

	if got := mapStopReason("max_tokens"); got != "length" {
		t.Fatalf("mapStopReason(max_tokens) = %q", got)
	}
	if got := mapStopReason("stop_sequence"); got != "stop" {
		t.Fatalf("mapStopReason(stop_sequence) = %q", got)
	}
	// Unknown stop reasons clamp to "stop" rather than leaking a non-OpenAI
	// value that a strict client would reject (#664, fix C).
	if got := mapStopReason("custom"); got != "stop" {
		t.Fatalf("mapStopReason(custom) = %q, want stop (clamped)", got)
	}

	_, err := fromAnthropicResp(anthropicResp{
		Content: []anthropicContent{{Type: "tool_use", Name: "bad", Input: map[string]any{"bad": func() {}}}},
	})
	if err == nil || !strings.Contains(err.Error(), "marshal tool input") {
		t.Fatalf("fromAnthropicResp marshal err = %v", err)
	}
}

func keys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// --- Thinking: budget-based request shape -----------------------------------

func TestComplete_BudgetThinking_RequestShape(t *testing.T) {
	respBody := `{"id":"msg_t1","type":"message","role":"assistant","model":"claude-sonnet-4-5",` +
		`"content":[{"type":"thinking","thinking":"I reasoned.","signature":"sig1"},` +
		`{"type":"text","text":"Answer."}],` +
		`"stop_reason":"end_turn","usage":{"input_tokens":20,"output_tokens":30}}`
	fake := newFakeAnthropic(t, respBody)
	c := NewWithBaseURL(newAPIKeyAuth(), fake.srv.URL)

	budget := 8192
	_, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []agentmodel.Message{{Role: "user", Content: "think hard"}},
		Thinking: &agentmodel.ThinkingConfig{Type: "enabled", BudgetTokens: budget},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var sent map[string]any
	if err := json.Unmarshal(fake.gotBody, &sent); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	// thinking field present and correct
	th, ok := sent["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking field missing or wrong type: %v", sent["thinking"])
	}
	if th["type"] != "enabled" {
		t.Errorf("thinking.type = %v, want enabled", th["type"])
	}
	if int(th["budget_tokens"].(float64)) != budget {
		t.Errorf("thinking.budget_tokens = %v, want %d", th["budget_tokens"], budget)
	}

	// output_config must NOT be present for budget-based
	if _, ok := sent["output_config"]; ok {
		t.Errorf("output_config must not be set for budget-based thinking")
	}

	// temperature must be stripped
	if _, ok := sent["temperature"]; ok {
		t.Errorf("temperature must be stripped when thinking is enabled")
	}

	// interleaved-thinking beta header must be present
	beta := fake.gotHeaders.Get("anthropic-beta")
	if !strings.Contains(beta, "interleaved-thinking-2025-05-14") {
		t.Errorf("anthropic-beta = %q, want to contain interleaved-thinking-2025-05-14", beta)
	}
}

func TestComplete_AdaptiveThinking_RequestShape(t *testing.T) {
	respBody := `{"id":"msg_t2","type":"message","role":"assistant","model":"claude-sonnet-4-6",` +
		`"content":[{"type":"text","text":"Done."}],` +
		`"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`
	fake := newFakeAnthropic(t, respBody)
	c := NewWithBaseURL(newAPIKeyAuth(), fake.srv.URL)

	temp := 0.7
	_, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:           "claude-sonnet-4-6",
		Messages:        []agentmodel.Message{{Role: "user", Content: "think"}},
		ReasoningEffort: "high",
		Temperature:     &temp,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var sent map[string]any
	if err := json.Unmarshal(fake.gotBody, &sent); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	// thinking field: type adaptive
	th, ok := sent["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking field missing: %v", sent["thinking"])
	}
	if th["type"] != "adaptive" {
		t.Errorf("thinking.type = %v, want adaptive", th["type"])
	}

	// output_config present with effort
	oc, ok := sent["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("output_config missing: %v", sent["output_config"])
	}
	if oc["effort"] != "high" {
		t.Errorf("output_config.effort = %v, want high", oc["effort"])
	}

	// temperature stripped
	if _, ok := sent["temperature"]; ok {
		t.Errorf("temperature must be stripped for adaptive thinking")
	}

	// interleaved-thinking beta must NOT be present (not needed for 4.6+)
	beta := fake.gotHeaders.Get("anthropic-beta")
	if strings.Contains(beta, "interleaved-thinking-2025-05-14") {
		t.Errorf("anthropic-beta should NOT contain interleaved-thinking-2025-05-14 for adaptive thinking")
	}
}

// --- Caching ---------------------------------------------------------------

// TestCache_SystemArrayWhenControlSet verifies that a system message with
// CacheControl forces the array form on the wire (so the hint survives the
// concat into anthropic's `system` field).
func TestCache_SystemArrayWhenControlSet(t *testing.T) {
	respBody := `{
		"id":"m1","type":"message","role":"assistant","model":"claude-3-5-sonnet-latest",
		"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":10,"output_tokens":1,"cache_creation_input_tokens":900,"cache_read_input_tokens":0}
	}`
	fake := newFakeAnthropic(t, respBody)
	c := NewWithBaseURL(newAPIKeyAuth(), fake.srv.URL)

	resp, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model: "claude-3-5-sonnet-latest",
		Messages: []agentmodel.Message{
			{Role: "system", Content: "very long system prompt", CacheControl: map[string]string{"type": "ephemeral"}},
			{Role: "user", Content: "hi"},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Wire-shape check: system must be an array containing cache_control.
	var got map[string]any
	_ = json.Unmarshal(fake.gotBody, &got)
	system, ok := got["system"].([]any)
	if !ok {
		t.Fatalf("system: got %T (%v), want []any", got["system"], got["system"])
	}
	if len(system) != 1 {
		t.Fatalf("system blocks: got %d, want 1", len(system))
	}
	block := system[0].(map[string]any)
	if block["type"] != "text" {
		t.Errorf("system block type: got %v, want text", block["type"])
	}
	if _, has := block["cache_control"]; !has {
		t.Errorf("system block missing cache_control: %+v", block)
	}

	// Usage must surface cache fields.
	if resp.Usage.CacheCreationInputTokens != 900 {
		t.Errorf("CacheCreationInputTokens: got %d, want 900", resp.Usage.CacheCreationInputTokens)
	}
	// PromptTokens is OpenAI-shape: includes cache_creation + cache_read.
	if resp.Usage.PromptTokens != 10+900 {
		t.Errorf("PromptTokens: got %d, want 910 (10 input + 900 cache_creation)", resp.Usage.PromptTokens)
	}
}

// TestCache_SystemStringWhenNoControl confirms backwards compatibility — when
// no system message asks for caching we still emit the legacy string form.
func TestCache_SystemStringWhenNoControl(t *testing.T) {
	respBody := `{
		"id":"m2","type":"message","role":"assistant","model":"claude-3-5-sonnet-latest",
		"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":3,"output_tokens":1}
	}`
	fake := newFakeAnthropic(t, respBody)
	c := NewWithBaseURL(newAPIKeyAuth(), fake.srv.URL)

	_, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model: "claude-3-5-sonnet-latest",
		Messages: []agentmodel.Message{
			{Role: "system", Content: "stable system prompt"},
			{Role: "user", Content: "hi"},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var got map[string]any
	_ = json.Unmarshal(fake.gotBody, &got)
	if _, ok := got["system"].(string); !ok {
		t.Errorf("system: got %T, want string when no cache_control", got["system"])
	}
}

// TestCache_UserMessageBlock_LastBlockTagged verifies user-message-level
// cache_control attaches to the last content block of that message.
func TestCache_UserMessageBlock_LastBlockTagged(t *testing.T) {
	respBody := `{
		"id":"m3","type":"message","role":"assistant","model":"claude-3-5-sonnet-latest",
		"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":500}
	}`
	fake := newFakeAnthropic(t, respBody)
	c := NewWithBaseURL(newAPIKeyAuth(), fake.srv.URL)

	resp, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model: "claude-3-5-sonnet-latest",
		Messages: []agentmodel.Message{
			{Role: "user", Content: "long context here", CacheControl: map[string]string{"type": "ephemeral"}},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var got map[string]any
	_ = json.Unmarshal(fake.gotBody, &got)
	msgs := got["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages: got %d, want 1", len(msgs))
	}
	user := msgs[0].(map[string]any)
	content := user["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("user content: got %d, want 1", len(content))
	}
	if _, has := content[0].(map[string]any)["cache_control"]; !has {
		t.Errorf("user content[0] missing cache_control: %+v", content[0])
	}

	// Cache read surfaces in usage.
	if resp.Usage.CacheReadInputTokens != 500 {
		t.Errorf("CacheReadInputTokens: got %d, want 500", resp.Usage.CacheReadInputTokens)
	}
}

// injectedSystemCacheControl runs a subscription-mode Complete and returns the
// cache_control object attached to the auto-injected system block (the last
// block, after the Claude Code identity block), or nil if none was attached.
func injectedSystemCacheControl(t *testing.T, opts []Option, msgs []agentmodel.Message) map[string]any {
	t.Helper()
	respBody := `{"id":"m","type":"message","role":"assistant","model":"claude-3-5-sonnet-latest","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	fake := newFakeAnthropic(t, respBody)
	c := NewWithBaseURL(newOAuthAuth(), fake.srv.URL, opts...)
	if _, err := c.Complete(context.Background(), agentmodel.ChatRequest{Model: "claude-3-5-sonnet-latest", Messages: msgs}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(fake.gotBody, &got); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	system, ok := got["system"].([]any)
	if !ok || len(system) == 0 {
		t.Fatalf("system: got %T (%v), want non-empty []any", got["system"], got["system"])
	}
	last := system[len(system)-1].(map[string]any)
	cc, _ := last["cache_control"].(map[string]any)
	return cc
}

// TestCache_TTL_DefaultOmitsTTL confirms the auto-injected breakpoint uses the
// bare ephemeral form (Anthropic's 5-minute default) when no TTL is configured.
func TestCache_TTL_DefaultOmitsTTL(t *testing.T) {
	cc := injectedSystemCacheControl(t, nil, []agentmodel.Message{
		{Role: "system", Content: "stable system prompt"},
		{Role: "user", Content: "hi"},
	})
	if cc == nil {
		t.Fatal("injected system block missing cache_control")
	}
	if cc["type"] != "ephemeral" {
		t.Errorf("cache_control type: got %v, want ephemeral", cc["type"])
	}
	if _, has := cc["ttl"]; has {
		t.Errorf("cache_control should omit ttl by default, got %+v", cc)
	}
}

// TestCache_TTL_AppliedToInjectedSystem confirms WithCacheTTL("1h") sets the
// 1-hour TTL on the proxy-injected system breakpoint.
func TestCache_TTL_AppliedToInjectedSystem(t *testing.T) {
	cc := injectedSystemCacheControl(t, []Option{WithCacheTTL("1h")}, []agentmodel.Message{
		{Role: "system", Content: "stable system prompt"},
		{Role: "user", Content: "hi"},
	})
	if cc == nil {
		t.Fatal("injected system block missing cache_control")
	}
	if cc["ttl"] != "1h" {
		t.Errorf("cache_control ttl: got %v, want 1h", cc["ttl"])
	}
}

// TestCache_BreakpointCap_SkipsInjectionAtLimit verifies the proxy does NOT add
// its system breakpoint when the client already placed the maximum number, so
// the request stays at the 4-marker limit Anthropic enforces (instead of 400ing
// with a 5th). The identity block must still be prepended.
func TestCache_BreakpointCap_SkipsInjectionAtLimit(t *testing.T) {
	msgs := []agentmodel.Message{{Role: "system", Content: "stable system prompt"}}
	for i := 0; i < maxCacheBreakpoints; i++ {
		msgs = append(msgs, agentmodel.Message{Role: "user", Content: "turn", CacheControl: map[string]string{"type": "ephemeral"}})
	}
	cc := injectedSystemCacheControl(t, nil, msgs)
	if cc != nil {
		t.Errorf("system breakpoint should be skipped at the cap, got cache_control %+v", cc)
	}

	// Identity block is still required even when caching is skipped.
	respBody := `{"id":"m","type":"message","role":"assistant","model":"claude-3-5-sonnet-latest","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	fake := newFakeAnthropic(t, respBody)
	c := NewWithBaseURL(newOAuthAuth(), fake.srv.URL)
	if _, err := c.Complete(context.Background(), agentmodel.ChatRequest{Model: "claude-3-5-sonnet-latest", Messages: msgs}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if n := countJSONKey(fake.gotBody, "cache_control"); n != maxCacheBreakpoints {
		t.Errorf("cache_control markers: got %d, want %d (no 5th injected)", n, maxCacheBreakpoints)
	}
	var got map[string]any
	_ = json.Unmarshal(fake.gotBody, &got)
	system := got["system"].([]any)
	first := system[0].(map[string]any)
	if first["text"] != claudeCodeIdentityPrompt {
		t.Errorf("identity block missing: first system block = %+v", first)
	}
}

// TestCache_BreakpointCap_AllowsInjectionUnderLimit confirms the proxy still
// caches the system prompt when the client is one breakpoint under the cap.
func TestCache_BreakpointCap_AllowsInjectionUnderLimit(t *testing.T) {
	msgs := []agentmodel.Message{{Role: "system", Content: "stable system prompt"}}
	for i := 0; i < maxCacheBreakpoints-1; i++ {
		msgs = append(msgs, agentmodel.Message{Role: "user", Content: "turn", CacheControl: map[string]string{"type": "ephemeral"}})
	}
	cc := injectedSystemCacheControl(t, nil, msgs)
	if cc == nil {
		t.Error("system breakpoint should be injected when under the cap")
	}
}

// TestCache_Passthrough_TTLAndCap exercises the raw-body passthrough path: the
// configured TTL rides the injected system breakpoint, and the breakpoint is
// skipped once the body already holds the maximum number of markers.
func TestCache_Passthrough_TTLAndCap(t *testing.T) {
	t.Run("ttl applied", func(t *testing.T) {
		body := []byte(`{"model":"m","system":"big prompt","messages":[{"role":"user","content":"hi"}]}`)
		out, err := preparePassthroughBody(body, "", true, "1h")
		if err != nil {
			t.Fatalf("preparePassthroughBody: %v", err)
		}
		var got map[string]any
		_ = json.Unmarshal(out, &got)
		system := got["system"].([]any)
		last := system[len(system)-1].(map[string]any)
		cc := last["cache_control"].(map[string]any)
		if cc["ttl"] != "1h" {
			t.Errorf("ttl: got %v, want 1h", cc["ttl"])
		}
	})

	t.Run("skips injection at cap", func(t *testing.T) {
		// Four cache_control markers already in messages → no room for a 5th.
		body := []byte(`{"model":"m","system":"big prompt","messages":[
			{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}]},
			{"role":"user","content":[{"type":"text","text":"b","cache_control":{"type":"ephemeral"}}]},
			{"role":"user","content":[{"type":"text","text":"c","cache_control":{"type":"ephemeral"}}]},
			{"role":"user","content":[{"type":"text","text":"d","cache_control":{"type":"ephemeral"}}]}
		]}`)
		out, err := preparePassthroughBody(body, "", true, "")
		if err != nil {
			t.Fatalf("preparePassthroughBody: %v", err)
		}
		if n := countJSONKey(out, "cache_control"); n != maxCacheBreakpoints {
			t.Errorf("cache_control markers: got %d, want %d", n, maxCacheBreakpoints)
		}
		var got map[string]any
		_ = json.Unmarshal(out, &got)
		system := got["system"].([]any)
		last := system[len(system)-1].(map[string]any)
		if _, has := last["cache_control"]; has {
			t.Errorf("system breakpoint should be skipped at cap, got %+v", last)
		}
	})
}

// TestWithCacheTTL_RejectsInvalid confirms the option guards against TTLs the
// API would reject on every request.
func TestWithCacheTTL_RejectsInvalid(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("WithCacheTTL(\"90m\") should panic")
		}
	}()
	WithCacheTTL("90m")
}

// TestCache_StreamingUsageIncludesCacheBreakdown verifies cache fields are
// extracted from message_start + message_delta SSE events.
func TestCache_StreamingUsageIncludesCacheBreakdown(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"role":"assistant","usage":{"input_tokens":100,"output_tokens":0,"cache_creation_input_tokens":2000,"cache_read_input_tokens":500}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":12}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	fake := newFakeAnthropic(t, sse)
	fake.respHeaders = map[string]string{"Content-Type": "text/event-stream"}
	c := NewWithBaseURL(newAPIKeyAuth(), fake.srv.URL)

	seq, err := c.Stream(context.Background(), agentmodel.ChatRequest{
		Model:    "claude-3-5-sonnet-latest",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var finalUsage *agentmodel.Usage
	for chunk, err := range seq {
		if err != nil {
			t.Fatalf("chunk: %v", err)
		}
		if chunk.Usage != nil {
			finalUsage = chunk.Usage
		}
	}
	if finalUsage == nil {
		t.Fatal("no terminal chunk with Usage")
	}
	if finalUsage.CacheCreationInputTokens != 2000 {
		t.Errorf("CacheCreationInputTokens: got %d, want 2000", finalUsage.CacheCreationInputTokens)
	}
	if finalUsage.CacheReadInputTokens != 500 {
		t.Errorf("CacheReadInputTokens: got %d, want 500", finalUsage.CacheReadInputTokens)
	}
	// PromptTokens = input + cache_creation + cache_read = 100 + 2000 + 500.
	if finalUsage.PromptTokens != 2600 {
		t.Errorf("PromptTokens: got %d, want 2600", finalUsage.PromptTokens)
	}
}

func TestComplete_ThinkingResponse_Collected(t *testing.T) {
	respBody := `{
		"id": "msg_th1",
		"type": "message",
		"role": "assistant",
		"model": "claude-sonnet-4-5",
		"content": [
			{"type":"thinking","thinking":"Step 1: consider X. Step 2: conclude Y.","signature":"sigABC"},
			{"type":"text","text":"The answer is Y."}
		],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 50, "output_tokens": 30}
	}`
	fake := newFakeAnthropic(t, respBody)
	c := NewWithBaseURL(newAPIKeyAuth(), fake.srv.URL)

	resp, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []agentmodel.Message{{Role: "user", Content: "think"}},
		Thinking: &agentmodel.ThinkingConfig{Type: "enabled", BudgetTokens: 4096},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	msg := resp.Choices[0].Message

	if msg.Content != "The answer is Y." {
		t.Errorf("Content = %q, want %q", msg.Content, "The answer is Y.")
	}
	if msg.ReasoningContent != "Step 1: consider X. Step 2: conclude Y." {
		t.Errorf("ReasoningContent = %q", msg.ReasoningContent)
	}
	if len(msg.ThinkingBlocks) != 1 {
		t.Fatalf("ThinkingBlocks len = %d, want 1", len(msg.ThinkingBlocks))
	}
	tb := msg.ThinkingBlocks[0]
	if tb.Type != "thinking" {
		t.Errorf("ThinkingBlock.Type = %q, want thinking", tb.Type)
	}
	if tb.Thinking != "Step 1: consider X. Step 2: conclude Y." {
		t.Errorf("ThinkingBlock.Thinking = %q", tb.Thinking)
	}
	if tb.Signature != "sigABC" {
		t.Errorf("ThinkingBlock.Signature = %q, want sigABC", tb.Signature)
	}
}

func TestStream_ThinkingDeltas_EmittedAndCollected(t *testing.T) {
	sseBody := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"role":"assistant","usage":{"input_tokens":20,"output_tokens":0}}}`,
		"",
		// thinking block
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Step 1."}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":" Step 2."}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sigXYZ"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		// text block
		"event: content_block_start",
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Answer."}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":1}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":15}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
		"",
	}, "\n")

	fake := newFakeAnthropic(t, sseBody)
	fake.respHeaders = map[string]string{"Content-Type": "text/event-stream"}
	c := NewWithBaseURL(newAPIKeyAuth(), fake.srv.URL)

	seq, err := c.Stream(context.Background(), agentmodel.ChatRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []agentmodel.Message{{Role: "user", Content: "think"}},
		Thinking: &agentmodel.ThinkingConfig{Type: "enabled", BudgetTokens: 4096},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var reasoningDeltas []string
	var textDeltas []string
	var terminalBlocks []agentmodel.ThinkingBlock
	var finalUsage *agentmodel.Usage

	for chunk, err := range seq {
		if err != nil {
			t.Fatalf("chunk err: %v", err)
		}
		if chunk.Delta.ReasoningContent != "" {
			reasoningDeltas = append(reasoningDeltas, chunk.Delta.ReasoningContent)
		}
		if chunk.Delta.Content != "" {
			textDeltas = append(textDeltas, chunk.Delta.Content)
		}
		if len(chunk.Delta.ThinkingBlocks) > 0 {
			terminalBlocks = chunk.Delta.ThinkingBlocks
		}
		if chunk.Usage != nil {
			finalUsage = chunk.Usage
		}
	}

	gotReasoning := strings.Join(reasoningDeltas, "")
	if gotReasoning != "Step 1. Step 2." {
		t.Errorf("reasoning deltas = %q, want %q", gotReasoning, "Step 1. Step 2.")
	}
	if strings.Join(textDeltas, "") != "Answer." {
		t.Errorf("text deltas = %q, want Answer.", strings.Join(textDeltas, ""))
	}
	if len(terminalBlocks) != 1 {
		t.Fatalf("terminal ThinkingBlocks len = %d, want 1", len(terminalBlocks))
	}
	tb := terminalBlocks[0]
	if tb.Type != "thinking" {
		t.Errorf("ThinkingBlock.Type = %q, want thinking", tb.Type)
	}
	if tb.Thinking != "Step 1. Step 2." {
		t.Errorf("ThinkingBlock.Thinking = %q, want %q", tb.Thinking, "Step 1. Step 2.")
	}
	if tb.Signature != "sigXYZ" {
		t.Errorf("ThinkingBlock.Signature = %q, want sigXYZ", tb.Signature)
	}
	if finalUsage == nil {
		t.Fatal("no final usage")
	}
	if finalUsage.CompletionTokens != 15 {
		t.Errorf("CompletionTokens = %d, want 15", finalUsage.CompletionTokens)
	}
}

func TestComplete_RedactedThinkingResponse_Collected(t *testing.T) {
	respBody := `{
		"id": "msg_th2",
		"type": "message",
		"role": "assistant",
		"model": "claude-sonnet-4-5",
		"content": [
			{"type":"redacted_thinking","data":"opaqueSignatureXYZ"},
			{"type":"text","text":"I cannot show my reasoning."}
		],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 10, "output_tokens": 10}
	}`
	fake := newFakeAnthropic(t, respBody)
	c := NewWithBaseURL(newAPIKeyAuth(), fake.srv.URL)

	resp, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []agentmodel.Message{{Role: "user", Content: "think"}},
		Thinking: &agentmodel.ThinkingConfig{Type: "enabled", BudgetTokens: 2048},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	msg := resp.Choices[0].Message

	if msg.Content != "I cannot show my reasoning." {
		t.Errorf("Content = %q", msg.Content)
	}
	if msg.ReasoningContent != "" {
		t.Errorf("ReasoningContent = %q, want empty for redacted", msg.ReasoningContent)
	}
	if len(msg.ThinkingBlocks) != 1 {
		t.Fatalf("ThinkingBlocks len = %d, want 1", len(msg.ThinkingBlocks))
	}
	tb := msg.ThinkingBlocks[0]
	if tb.Type != "redacted_thinking" {
		t.Errorf("ThinkingBlock.Type = %q, want redacted_thinking", tb.Type)
	}
	if tb.Signature != "opaqueSignatureXYZ" {
		t.Errorf("ThinkingBlock.Signature = %q, want opaqueSignatureXYZ", tb.Signature)
	}
}

// --- System prompt double-injection guard -----------------------------------

// TestPreparePassthroughBody_ArrayBranch_NoDoubleInject verifies that when the
// system field is already an array whose first block contains the CC identity
// prompt, preparePassthroughBody does not prepend it a second time.
func TestPreparePassthroughBody_ArrayBranch_NoDoubleInject(t *testing.T) {
	// Build a body where system is already an array starting with the CC block.
	alreadyInjected := `{"model":"claude-3-5-sonnet-latest","messages":[{"role":"user","content":"hi"}],"system":[{"type":"text","text":"` + claudeCodeIdentityPrompt + `"},{"type":"text","text":"user context","cache_control":{"type":"ephemeral"}}]}`

	out, err := preparePassthroughBody([]byte(alreadyInjected), "", true, "")
	if err != nil {
		t.Fatalf("preparePassthroughBody: %v", err)
	}

	var result map[string]json.RawMessage
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(result["system"], &blocks); err != nil {
		t.Fatalf("unmarshal system blocks: %v", err)
	}

	if len(blocks) != 2 {
		t.Errorf("system blocks len = %d, want 2 (got double-injected)", len(blocks))
	}

	var firstText string
	if err := json.Unmarshal(blocks[0]["text"], &firstText); err != nil {
		t.Fatalf("unmarshal first block text: %v", err)
	}
	if firstText != claudeCodeIdentityPrompt {
		t.Errorf("first block text = %q, want CC identity prompt", firstText)
	}
}

// --- MessagesPassthrough coverage -------------------------------------------

// TestMessagesPassthrough_BasicProxy verifies the body is forwarded as-is
// when using API key auth (no subscription injection).
func TestMessagesPassthrough_BasicProxy(t *testing.T) {
	fake := newFakeAnthropic(t, `{"id":"m1","type":"message"}`)
	c := NewWithBaseURL(newAPIKeyAuth(), fake.srv.URL)

	body := []byte(`{"model":"claude-3-5-sonnet-latest","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := c.MessagesPassthrough(context.Background(), body, "", "")
	if err != nil {
		t.Fatalf("MessagesPassthrough: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	// Body passed through unchanged (no model override, no subscription injection).
	var got map[string]json.RawMessage
	if err := json.Unmarshal(fake.gotBody, &got); err != nil {
		t.Fatalf("parse forwarded body: %v", err)
	}
	var model string
	_ = json.Unmarshal(got["model"], &model)
	if model != "claude-3-5-sonnet-latest" {
		t.Errorf("forwarded model = %q, want claude-3-5-sonnet-latest", model)
	}
}

// TestMessagesPassthrough_ModelOverride verifies the model field is rewritten.
func TestMessagesPassthrough_ModelOverride(t *testing.T) {
	fake := newFakeAnthropic(t, `{"id":"m2","type":"message"}`)
	c := NewWithBaseURL(newAPIKeyAuth(), fake.srv.URL)

	body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := c.MessagesPassthrough(context.Background(), body, "claude-3-5-sonnet-latest", "")
	if err != nil {
		t.Fatalf("MessagesPassthrough: %v", err)
	}
	defer resp.Body.Close()

	var got map[string]json.RawMessage
	_ = json.Unmarshal(fake.gotBody, &got)
	var model string
	_ = json.Unmarshal(got["model"], &model)
	if model != "claude-3-5-sonnet-latest" {
		t.Errorf("forwarded model = %q, want claude-3-5-sonnet-latest", model)
	}
}

// TestMessagesPassthrough_ClientBetasMerged verifies that clientBetas are merged
// into the upstream anthropic-beta header (after auth betas are already set).
func TestMessagesPassthrough_ClientBetasMerged(t *testing.T) {
	fake := newFakeAnthropic(t, `{"id":"m3","type":"message"}`)
	c := NewWithBaseURL(newAPIKeyAuth(), fake.srv.URL)

	body := []byte(`{"model":"claude-3-5-sonnet-latest","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := c.MessagesPassthrough(context.Background(), body, "", "token-counting-2024-11-01,extra-flag")
	if err != nil {
		t.Fatalf("MessagesPassthrough: %v", err)
	}
	defer resp.Body.Close()

	betaHeader := fake.gotHeaders.Get("anthropic-beta")
	if !strings.Contains(betaHeader, "token-counting-2024-11-01") {
		t.Errorf("anthropic-beta = %q, want to contain token-counting-2024-11-01", betaHeader)
	}
	if !strings.Contains(betaHeader, "extra-flag") {
		t.Errorf("anthropic-beta = %q, want to contain extra-flag", betaHeader)
	}
}

// TestMessagesPassthrough_OAuthInjectsSystem verifies that OAuth (subscription)
// auth causes the CC identity block to be injected when no system is present.
func TestMessagesPassthrough_OAuthInjectsSystem(t *testing.T) {
	fake := newFakeAnthropic(t, `{"id":"m4","type":"message"}`)
	c := NewWithBaseURL(newOAuthAuth(), fake.srv.URL)

	body := []byte(`{"model":"claude-3-5-sonnet-latest","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := c.MessagesPassthrough(context.Background(), body, "", "")
	if err != nil {
		t.Fatalf("MessagesPassthrough: %v", err)
	}
	defer resp.Body.Close()

	var got map[string]json.RawMessage
	_ = json.Unmarshal(fake.gotBody, &got)
	// system should be the CC identity string
	var system string
	if err := json.Unmarshal(got["system"], &system); err != nil {
		t.Fatalf("unmarshal system: %v (got %s)", err, got["system"])
	}
	if system != claudeCodeIdentityPrompt {
		t.Errorf("system = %q, want CC identity prompt", system)
	}
}

// --- preparePassthroughBody additional coverage -----------------------------

func TestPreparePassthroughBody_InvalidJSON(t *testing.T) {
	_, err := preparePassthroughBody([]byte(`not json`), "", false, "")
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestPreparePassthroughBody_ModelOverride_Rewrites(t *testing.T) {
	body := []byte(`{"model":"old-model","messages":[]}`)
	out, err := preparePassthroughBody(body, "new-model", false, "")
	if err != nil {
		t.Fatalf("preparePassthroughBody: %v", err)
	}
	var result map[string]json.RawMessage
	_ = json.Unmarshal(out, &result)
	var model string
	_ = json.Unmarshal(result["model"], &model)
	if model != "new-model" {
		t.Errorf("model = %q, want new-model", model)
	}
}

func TestPreparePassthroughBody_ModelOverride_AlreadyMatches_NoMutation(t *testing.T) {
	body := []byte(`{"model":"same-model","messages":[]}`)
	out, err := preparePassthroughBody(body, "same-model", false, "")
	if err != nil {
		t.Fatalf("preparePassthroughBody: %v", err)
	}
	// No mutation: should return original body slice.
	if string(out) != string(body) {
		t.Errorf("body mutated unnecessarily: got %s, want %s", out, body)
	}
}

func TestPreparePassthroughBody_NilSystem_InjectsString(t *testing.T) {
	body := []byte(`{"model":"m","messages":[]}`)
	out, err := preparePassthroughBody(body, "", true, "")
	if err != nil {
		t.Fatalf("preparePassthroughBody: %v", err)
	}
	var result map[string]json.RawMessage
	_ = json.Unmarshal(out, &result)
	var system string
	if err := json.Unmarshal(result["system"], &system); err != nil {
		t.Fatalf("system not a string: %v (got %s)", err, result["system"])
	}
	if system != claudeCodeIdentityPrompt {
		t.Errorf("system = %q, want CC identity prompt", system)
	}
}

func TestPreparePassthroughBody_StringSystem_CCPrefix_NoChange(t *testing.T) {
	// System already starts with CC identity → no mutation.
	body := []byte(`{"model":"m","messages":[],"system":"` + claudeCodeIdentityPrompt + ` extra"}`)
	out, err := preparePassthroughBody(body, "", true, "")
	if err != nil {
		t.Fatalf("preparePassthroughBody: %v", err)
	}
	var result map[string]json.RawMessage
	_ = json.Unmarshal(out, &result)
	var system string
	_ = json.Unmarshal(result["system"], &system)
	if !strings.HasPrefix(system, claudeCodeIdentityPrompt) {
		t.Errorf("system should still start with CC identity, got %q", system)
	}
	// Should be a string, not an array.
	if result["system"][0] != '"' {
		t.Errorf("system should remain a string, got %s", result["system"])
	}
}

func TestPreparePassthroughBody_StringSystem_NoCC_WrapsArray(t *testing.T) {
	body := []byte(`{"model":"m","messages":[],"system":"user system prompt"}`)
	out, err := preparePassthroughBody(body, "", true, "")
	if err != nil {
		t.Fatalf("preparePassthroughBody: %v", err)
	}
	var result map[string]json.RawMessage
	_ = json.Unmarshal(out, &result)
	// system should now be an array
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(result["system"], &blocks); err != nil {
		t.Fatalf("system not an array: %v (got %s)", err, result["system"])
	}
	if len(blocks) != 2 {
		t.Fatalf("system blocks = %d, want 2", len(blocks))
	}
	// First block is CC identity
	var firstText string
	_ = json.Unmarshal(blocks[0]["text"], &firstText)
	if firstText != claudeCodeIdentityPrompt {
		t.Errorf("first block text = %q, want CC identity prompt", firstText)
	}
	// Second block is user's original prompt with cache_control
	var secondText string
	_ = json.Unmarshal(blocks[1]["text"], &secondText)
	if secondText != "user system prompt" {
		t.Errorf("second block text = %q, want user system prompt", secondText)
	}
	if blocks[1]["cache_control"] == nil {
		t.Error("second block missing cache_control")
	}
}

func TestPreparePassthroughBody_ArraySystem_FreshArray_PrependsCC(t *testing.T) {
	// Array form without CC at block[0] → CC gets prepended.
	body := []byte(`{"model":"m","messages":[],"system":[{"type":"text","text":"user block"}]}`)
	out, err := preparePassthroughBody(body, "", true, "")
	if err != nil {
		t.Fatalf("preparePassthroughBody: %v", err)
	}
	var result map[string]json.RawMessage
	_ = json.Unmarshal(out, &result)
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(result["system"], &blocks); err != nil {
		t.Fatalf("system not an array: %v", err)
	}
	if len(blocks) != 2 {
		t.Fatalf("system blocks = %d, want 2 (CC prepended)", len(blocks))
	}
	var firstText string
	_ = json.Unmarshal(blocks[0]["text"], &firstText)
	if firstText != claudeCodeIdentityPrompt {
		t.Errorf("first block = %q, want CC identity", firstText)
	}
	// Last block gets cache_control added
	if blocks[1]["cache_control"] == nil {
		t.Error("last block missing cache_control")
	}
}

var _ provider.ModelLister = (*Client)(nil)

func TestListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Errorf("missing anthropic-version header")
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"claude-opus-4-8","type":"model"},{"id":"claude-sonnet-4-5","type":"model"}],"has_more":false}`)
	}))
	defer srv.Close()

	c := NewWithBaseURL(newAPIKeyAuth(), srv.URL)
	got, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(got) != 2 || got[0] != "claude-opus-4-8" || got[1] != "claude-sonnet-4-5" {
		t.Errorf("got %v, want [claude-opus-4-8 claude-sonnet-4-5]", got)
	}
}

func TestComplete_RetriesOn401WithRefresh(t *testing.T) {
	var refreshHits int32
	oauthSrv := newRefreshOAuthServer(t, "sk-ant-oat-refreshed", &refreshHits)

	var calls int32
	var retryAuth string
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"token expired"}}`)
			return
		}
		retryAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-3-5-sonnet-latest","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer apiSrv.Close()

	c := NewWithBaseURL(newRefreshableAuth(t, oauthSrv.URL, "sk-ant-oat-old", "rt-old"), apiSrv.URL)
	resp, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "claude-3-5-sonnet-latest",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Choices[0].Message.Content != "ok" {
		t.Errorf("content = %q, want ok", resp.Choices[0].Message.Content)
	}
	if got := atomic.LoadInt32(&refreshHits); got != 1 {
		t.Errorf("refresh hits = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("api calls = %d, want 2 (401 then 200)", got)
	}
	if retryAuth != "Bearer sk-ant-oat-refreshed" {
		t.Errorf("retry Authorization = %q, want Bearer sk-ant-oat-refreshed", retryAuth)
	}
}

func TestComplete_401ApiKeyNoRetry(t *testing.T) {
	// api-key auth is not a forceRefresher: a 401 must surface unchanged, with
	// no refresh and no retry.
	var calls int32
	fake := newFakeAnthropic(t, `{"type":"error","error":{"message":"unauthorized"}}`)
	fake.respStatus = http.StatusUnauthorized
	orig := fake.srv.Config.Handler
	fake.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		orig.ServeHTTP(w, r)
	})

	c := NewWithBaseURL(newAPIKeyAuth(), fake.srv.URL)
	_, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "claude-3-5-sonnet-latest",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "status 401") {
		t.Fatalf("Complete err = %v, want status 401", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("api calls = %d, want 1 (no retry)", got)
	}
}

func TestMessagesPassthrough_RetriesOn401WithRefresh(t *testing.T) {
	var refreshHits int32
	oauthSrv := newRefreshOAuthServer(t, "sk-ant-oat-refreshed", &refreshHits)

	var calls int32
	var retryAuth string
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"token expired"}}`)
			return
		}
		retryAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message"}`)
	}))
	defer apiSrv.Close()

	c := NewWithBaseURL(newRefreshableAuth(t, oauthSrv.URL, "sk-ant-oat-old", "rt-old"), apiSrv.URL)
	resp, err := c.MessagesPassthrough(context.Background(), []byte(`{"model":"claude-3-5-sonnet-latest","messages":[]}`), "", "")
	if err != nil {
		t.Fatalf("MessagesPassthrough: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 after refresh", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&refreshHits); got != 1 {
		t.Errorf("refresh hits = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("api calls = %d, want 2 (401 then 200)", got)
	}
	if retryAuth != "Bearer sk-ant-oat-refreshed" {
		t.Errorf("retry Authorization = %q, want Bearer sk-ant-oat-refreshed", retryAuth)
	}
}

// A 429 from Anthropic carries when the window rolls. The provider must expose
// it so the pool and router can size the cooldown on fact instead of the 5m
// guess — a subscription 429 means "this account is done for hours", and
// probing every 5 minutes just burns requests.
func TestCompleteRateLimitCarriesRetryAfterHint(t *testing.T) {
	fake := newFakeAnthropic(t, `{"error":"rate limited"}`)
	fake.respStatus = http.StatusTooManyRequests
	fake.respHeaders = map[string]string{"Retry-After": "1800"}
	c := NewWithBaseURL(newAPIKeyAuth(), fake.srv.URL)

	_, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "claude-3-5-sonnet-latest",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected a rate-limit error")
	}
	ae := agentmodel.Wrap(err)
	if ae.Type != agentmodel.ErrTypeRateLimit {
		t.Fatalf("Type=%q, want %q", ae.Type, agentmodel.ErrTypeRateLimit)
	}
	if ae.RetryAfter != 30*time.Minute {
		t.Errorf("RetryAfter=%v, want 30m", ae.RetryAfter)
	}
}

// Subscription quota exhaustion reports the reset as a unix-epoch second count
// on anthropic-ratelimit-unified-reset rather than a Retry-After delta.
func TestCompleteRateLimitFallsBackToUnifiedResetHeader(t *testing.T) {
	fake := newFakeAnthropic(t, `{"error":"quota exhausted"}`)
	fake.respStatus = http.StatusTooManyRequests
	reset := time.Now().Add(2 * time.Hour)
	fake.respHeaders = map[string]string{
		"Anthropic-Ratelimit-Unified-Reset": strconv.FormatInt(reset.Unix(), 10),
	}
	c := NewWithBaseURL(newAPIKeyAuth(), fake.srv.URL)

	_, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "claude-3-5-sonnet-latest",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected a rate-limit error")
	}
	got := agentmodel.Wrap(err).RetryAfter
	if got < 110*time.Minute || got > 2*time.Hour {
		t.Errorf("RetryAfter=%v, want ~2h from the unified reset header", got)
	}
}

// A non-429 must not carry a hint: cooling a deployment on an upstream 500 for
// however long an unrelated header says would park a healthy credential.
func TestCompleteNonRateLimitCarriesNoHint(t *testing.T) {
	fake := newFakeAnthropic(t, `{"error":"boom"}`)
	fake.respStatus = http.StatusInternalServerError
	fake.respHeaders = map[string]string{"Retry-After": "1800"}
	c := NewWithBaseURL(newAPIKeyAuth(), fake.srv.URL)

	_, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "claude-3-5-sonnet-latest",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := agentmodel.Wrap(err).RetryAfter; got != 0 {
		t.Errorf("RetryAfter=%v, want 0 on a non-429", got)
	}
}
