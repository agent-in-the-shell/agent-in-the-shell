package api_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/api"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/contentlog"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/cost"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/anthropic"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/stub"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

// contentRecord mirrors the on-disk JSONL shape so tests can decode it without
// reaching into the response/request bodies' concrete types.
type contentRecord struct {
	RequestID string          `json:"request_id"`
	Request   json.RawMessage `json:"request"`
	Response  json.RawMessage `json:"response"`
}

// withContentLog returns a newTestServer option that points the server at a
// content log written to `path`.
func withContentLog(t *testing.T, path string) func(*api.Config) {
	t.Helper()
	lg, err := contentlog.Open(path)
	if err != nil {
		t.Fatalf("open content log: %v", err)
	}
	t.Cleanup(func() { lg.Close() })
	return func(c *api.Config) { c.ContentLog = lg }
}

func readContentLog(t *testing.T, path string) []contentRecord {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open content log: %v", err)
	}
	defer f.Close()
	var recs []contentRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var rec contentRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("decode content line %q: %v", sc.Text(), err)
		}
		recs = append(recs, rec)
	}
	return recs
}

// soleChatResponse decodes the single record in the content log at path as an
// OpenAI-shaped chat response, failing if the log holds any other count.
func soleChatResponse(t *testing.T, path string) agentmodel.ChatResponse {
	t.Helper()
	recs := readContentLog(t, path)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	var got agentmodel.ChatResponse
	if err := json.Unmarshal(recs[0].Response, &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return got
}

// TestContentLog_DisabledByDefault confirms a server with no ContentLog writes
// no content file and serves normally — the privacy-preserving default.
func TestContentLog_DisabledByDefault(t *testing.T) {
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"gpt-4": {{Provider: &stub.Stub{}, Model: "x", Weight: 1}},
	})
	resp := mustPost(t, ts, "/v1/chat/completions", agentmodel.ChatRequest{
		Model:    "gpt-4",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	}, testToken)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestContentLog_ChatNonStreaming(t *testing.T) {
	path := filepath.Join(t.TempDir(), "content.jsonl")
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"gpt-4": {{Provider: &stub.Stub{}, Model: "x", Weight: 1}},
	}, withContentLog(t, path))

	resp := mustPost(t, ts, "/v1/chat/completions", agentmodel.ChatRequest{
		Model:    "gpt-4",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	}, testToken)
	resp.Body.Close()

	recs := readContentLog(t, path)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}

	var req agentmodel.ChatRequest
	if err := json.Unmarshal(recs[0].Request, &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if req.Model != "gpt-4" || len(req.Messages) != 1 || req.Messages[0].Content != "hi" {
		t.Errorf("logged request mismatch: %+v", req)
	}

	var got agentmodel.ChatResponse
	if err := json.Unmarshal(recs[0].Response, &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Choices) != 1 || got.Choices[0].Message.Content != "stub response" {
		t.Errorf("logged response content = %q, want %q", contentOf(got), "stub response")
	}
}

func TestContentLog_ChatStreamingReassembled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "content.jsonl")
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"gpt-4": {{Provider: &stub.Stub{}, Model: "x", Weight: 1}},
	}, withContentLog(t, path))

	resp := mustPost(t, ts, "/v1/chat/completions", agentmodel.ChatRequest{
		Model:    "gpt-4",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
		Stream:   true,
	}, testToken)
	io.Copy(io.Discard, resp.Body) // drain so the handler finishes and logs
	resp.Body.Close()

	got := soleChatResponse(t, path)
	// Default stub stream emits "stub " + "response" across two deltas; the
	// content log must record the reassembled whole, not the fragments.
	if c := contentOf(got); c != "stub response" {
		t.Errorf("reassembled content = %q, want %q", c, "stub response")
	}
	if len(got.Choices) == 0 || got.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", finishOf(got))
	}
	if len(got.Choices) > 0 && got.Choices[0].Message.Role != "assistant" {
		t.Errorf("role = %q, want assistant", got.Choices[0].Message.Role)
	}
}

func TestContentLog_ChatStreamingToolCalls(t *testing.T) {
	idx := 0
	streamChunks := []provider.StreamChunk{
		{Delta: agentmodel.Message{Role: "assistant", ToolCalls: []agentmodel.ToolCall{{
			Index: &idx, ID: "call_1", Type: "function",
			Function: agentmodel.ToolCallFunction{Name: "get_weather", Arguments: `{"loc`},
		}}}},
		{Delta: agentmodel.Message{ToolCalls: []agentmodel.ToolCall{{
			Index:    &idx,
			Function: agentmodel.ToolCallFunction{Arguments: `ation":"SF"}`},
		}}}},
		{Delta: agentmodel.Message{}, FinishReason: "tool_calls",
			Usage: &agentmodel.Usage{PromptTokens: 4, CompletionTokens: 6, TotalTokens: 10}},
	}

	path := filepath.Join(t.TempDir(), "content.jsonl")
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"gpt-4": {{Provider: &stub.Stub{StreamChunks: streamChunks}, Model: "x", Weight: 1}},
	}, withContentLog(t, path))

	resp := mustPost(t, ts, "/v1/chat/completions", agentmodel.ChatRequest{
		Model:    "gpt-4",
		Messages: []agentmodel.Message{{Role: "user", Content: "weather?"}},
		Stream:   true,
	}, testToken)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	got := soleChatResponse(t, path)
	if len(got.Choices) != 1 || len(got.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("tool calls not reassembled: %+v", got.Choices)
	}
	tc := got.Choices[0].Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "get_weather" {
		t.Errorf("tool call id/name = %q/%q", tc.ID, tc.Function.Name)
	}
	if tc.Function.Arguments != `{"location":"SF"}` {
		t.Errorf("reassembled arguments = %q, want %q", tc.Function.Arguments, `{"location":"SF"}`)
	}
}

// TestContentLog_ChatStreamingThinking pins the streamed record to the same
// reasoning surface the non-streaming one carries: reasoning_content text and
// the completed thinking blocks with their signatures.
func TestContentLog_ChatStreamingThinking(t *testing.T) {
	blocks := []agentmodel.ThinkingBlock{
		{Type: "thinking", Thinking: "Let me think.", Signature: "sig-abc"},
		{Type: "redacted_thinking", Signature: "sig-def"},
	}
	streamChunks := []provider.StreamChunk{
		{Delta: agentmodel.Message{Role: "assistant", ReasoningContent: "Let me "}},
		{Delta: agentmodel.Message{ReasoningContent: "think."}},
		{Delta: agentmodel.Message{Content: "42"}},
		// Anthropic's terminal chunk: whole blocks, signatures attached.
		{Delta: agentmodel.Message{ThinkingBlocks: blocks}},
		{Delta: agentmodel.Message{}, FinishReason: "stop",
			Usage: &agentmodel.Usage{PromptTokens: 3, CompletionTokens: 9, TotalTokens: 12}},
	}

	path := filepath.Join(t.TempDir(), "content.jsonl")
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"gpt-4": {{Provider: &stub.Stub{StreamChunks: streamChunks}, Model: "x", Weight: 1}},
	}, withContentLog(t, path))

	resp := mustPost(t, ts, "/v1/chat/completions", agentmodel.ChatRequest{
		Model:    "gpt-4",
		Messages: []agentmodel.Message{{Role: "user", Content: "answer?"}},
		Stream:   true,
	}, testToken)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Compare the whole message: a future delta field that add() forgets to merge
	// fails here, not just the fields this test thought to name.
	want := agentmodel.Message{
		Role:             "assistant",
		Content:          "42",
		ReasoningContent: "Let me think.",
		ThinkingBlocks:   blocks,
	}
	got := soleChatResponse(t, path)
	if len(got.Choices) != 1 || !reflect.DeepEqual(got.Choices[0].Message, want) {
		t.Errorf("logged message = %+v, want %+v", got.Choices, want)
	}
}

// TestChatContentAcc_MergesEveryDeltaField guards the drift that produced the
// dropped-thinking_blocks bug: chatContentAcc.add merges Message field by field,
// so a newly added field is silently absent from every streamed content-log
// record until someone teaches add() about it. Companion to the reflection
// guards in cmd/agent-model/config_doc_test.go.
//
// Blind spot worth knowing: Message.rawContent is unexported (array-shaped
// content, set only via UnmarshalJSON), so no exported-field scheme can cover
// it — the streamed log records the text projection, never the parts array.
func TestChatContentAcc_MergesEveryDeltaField(t *testing.T) {
	// Fields add() folds into the assembled assistant message.
	merged := map[string]bool{
		"role": true, "content": true, "tool_calls": true,
		"reasoning_content": true, "thinking_blocks": true,
	}
	// Request-side only — never present on an assistant delta.
	notMerged := map[string]bool{
		"tool_call_id": true, "name": true, "cache_control": true,
	}

	typ := reflect.TypeOf(agentmodel.Message{})
	for i := range typ.NumField() {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		if !merged[tag] && !notMerged[tag] {
			t.Errorf("Message field %s (json:%q) is new: teach chatContentAcc.add to merge it, "+
				"or list it as request-side only", f.Name, tag)
		}
	}
}

// ─── /v1/messages ──────────────────────────────────────────────────────────

func newMessagesContentServer(t *testing.T, path string, upstream *httptest.Server) *httptest.Server {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	registry, err := cost.LoadDefault()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	lg, err := contentlog.Open(path)
	if err != nil {
		t.Fatalf("content log: %v", err)
	}
	t.Cleanup(func() { lg.Close() })

	authn := &auth.StaticKey{HeaderName: "x-api-key", Token: "sk-ant-api03-test"}
	client := anthropic.NewWithBaseURL(authn, upstream.URL)
	r := router.New(map[string][]router.Deployment{
		"claude-sonnet-4-5": {{Provider: client, Model: "claude-3-5-sonnet-latest", Weight: 1}},
	}, nil)
	s := api.New(api.Config{Router: r, Store: st, Registry: registry, BearerToken: testToken, ContentLog: lg})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestContentLog_MessagesNonStreaming(t *testing.T) {
	upstreamResp := `{"id":"msg_123","type":"message","role":"assistant","model":"claude-3-5-sonnet-latest","content":[{"type":"text","text":"Hello!"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":5}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstreamResp)
	}))
	defer upstream.Close()

	path := filepath.Join(t.TempDir(), "content.jsonl")
	ts := newMessagesContentServer(t, path, upstream)

	resp := mustPost(t, ts, "/v1/messages", map[string]any{
		"model":      "claude-sonnet-4-5",
		"max_tokens": 100,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	}, testToken)
	resp.Body.Close()

	recs := readContentLog(t, path)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	// Request is logged verbatim (raw Anthropic JSON), response is the upstream
	// reply verbatim.
	var req map[string]any
	if err := json.Unmarshal(recs[0].Request, &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if req["model"] != "claude-sonnet-4-5" {
		t.Errorf("logged request model = %v", req["model"])
	}
	if !strings.Contains(string(recs[0].Response), `"Hello!"`) {
		t.Errorf("logged response missing upstream text: %s", recs[0].Response)
	}
}

func TestContentLog_MessagesStreamingReassembled(t *testing.T) {
	sseBody := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-3-5-sonnet-latest","content":[],"usage":{"input_tokens":42,"output_tokens":1}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo!"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
		"",
	}, "\n")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, sseBody)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()

	path := filepath.Join(t.TempDir(), "content.jsonl")
	ts := newMessagesContentServer(t, path, upstream)

	body, _ := json.Marshal(map[string]any{
		"model":      "claude-sonnet-4-5",
		"max_tokens": 100,
		"stream":     true,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	})
	httpReq, _ := http.NewRequestWithContext(context.Background(), "POST", ts.URL+"/v1/messages", bytes.NewReader(body))
	httpReq.Header.Set("Authorization", "Bearer "+testToken)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(httpReq)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	recs := readContentLog(t, path)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	// The reassembled message is a complete non-streaming Messages object.
	var msg struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(recs[0].Response, &msg); err != nil {
		t.Fatalf("decode reassembled message: %v\n%s", err, recs[0].Response)
	}
	if len(msg.Content) != 1 || msg.Content[0].Text != "Hello!" {
		t.Errorf("reassembled content = %+v, want text %q", msg.Content, "Hello!")
	}
	if msg.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", msg.StopReason)
	}
	if msg.Usage.OutputTokens != 7 {
		t.Errorf("usage.output_tokens = %d, want 7 (folded from message_delta)", msg.Usage.OutputTokens)
	}
}

func contentOf(r agentmodel.ChatResponse) string {
	if len(r.Choices) == 0 {
		return ""
	}
	return r.Choices[0].Message.Content
}

func finishOf(r agentmodel.ChatResponse) string {
	if len(r.Choices) == 0 {
		return ""
	}
	return r.Choices[0].FinishReason
}
