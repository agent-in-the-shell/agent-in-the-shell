package messagesbridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"strings"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/stub"
)

func iptr(i int) *int { return &i }

// ─── request translation ─────────────────────────────────────────────────────

func TestAnthropicToChatRequest(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-5",
		"system": "be terse",
		"max_tokens": 64,
		"stream": true,
		"temperature": 0.5,
		"tools": [{"name":"get_weather","description":"w","input_schema":{"type":"object"}}],
		"tool_choice": {"type":"auto"},
		"messages": [
			{"role":"user","content":"hi"},
			{"role":"assistant","content":[
				{"type":"text","text":"let me check"},
				{"type":"tool_use","id":"call_1|item_1","name":"get_weather","input":{"city":"SF"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"call_1|item_1","content":"sunny"}
			]}
		]
	}`)

	req, err := anthropicToChatRequest(body, "gpt-5-codex")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	if req.Model != "gpt-5-codex" {
		t.Errorf("model = %q, want gpt-5-codex (override)", req.Model)
	}
	if req.MaxTokens == nil || *req.MaxTokens != 64 {
		t.Errorf("max_tokens = %v, want 64", req.MaxTokens)
	}
	if !req.Stream {
		t.Error("stream should be true")
	}
	if req.Temperature == nil || *req.Temperature != 0.5 {
		t.Errorf("temperature = %v, want 0.5", req.Temperature)
	}
	if req.ToolChoice != "auto" {
		t.Errorf("tool_choice = %v, want auto", req.ToolChoice)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("tools = %+v, want one get_weather function", req.Tools)
	}

	// system, user, assistant(text+tool_use), tool(result)
	want := []struct {
		role    string
		content string
	}{
		{"system", "be terse"},
		{"user", "hi"},
		{"assistant", "let me check"},
		{"tool", "sunny"},
	}
	if len(req.Messages) != len(want) {
		t.Fatalf("messages = %d, want %d: %+v", len(req.Messages), len(want), req.Messages)
	}
	for i, w := range want {
		if req.Messages[i].Role != w.role || req.Messages[i].Content != w.content {
			t.Errorf("message[%d] = {%q,%q}, want {%q,%q}", i, req.Messages[i].Role, req.Messages[i].Content, w.role, w.content)
		}
	}

	asst := req.Messages[2]
	if len(asst.ToolCalls) != 1 {
		t.Fatalf("assistant tool calls = %d, want 1", len(asst.ToolCalls))
	}
	tc := asst.ToolCalls[0]
	if tc.ID != "call_1|item_1" || tc.Function.Name != "get_weather" {
		t.Errorf("tool call = %+v, want id call_1|item_1 name get_weather", tc)
	}
	if tc.Function.Arguments != `{"city":"SF"}` {
		t.Errorf("tool args = %q, want {\"city\":\"SF\"}", tc.Function.Arguments)
	}
	if req.Messages[3].ToolCallID != "call_1|item_1" {
		t.Errorf("tool result id = %q, want call_1|item_1", req.Messages[3].ToolCallID)
	}
}

func TestAnthropicToChatRequest_SystemBlocks(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":8,"system":[{"type":"text","text":"a"},{"type":"text","text":"b"}],"messages":[{"role":"user","content":"x"}]}`)
	req, err := anthropicToChatRequest(body, "")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if req.Model != "m" {
		t.Errorf("model = %q, want m (no override)", req.Model)
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Content != "a\n\nb" {
		t.Errorf("system = %+v, want joined a\\n\\nb", req.Messages[0])
	}
}

// ─── response translation ────────────────────────────────────────────────────

func TestChatResponseToAnthropic(t *testing.T) {
	resp := agentmodel.ChatResponse{
		ID: "resp_1",
		Choices: []agentmodel.Choice{{
			Message: agentmodel.Message{
				Role:    "assistant",
				Content: "here you go",
				ToolCalls: []agentmodel.ToolCall{{
					ID:       "call_9",
					Type:     "function",
					Function: agentmodel.ToolCallFunction{Name: "get_weather", Arguments: `{"city":"SF"}`},
				}},
			},
			FinishReason: "tool_calls",
		}},
		Usage: agentmodel.Usage{PromptTokens: 30, CompletionTokens: 7, TotalTokens: 37, CacheReadInputTokens: 10},
	}

	ar := chatResponseToAnthropic(resp, "gpt-5-codex")
	if ar.Type != "message" || ar.Role != "assistant" || ar.Model != "gpt-5-codex" {
		t.Errorf("envelope = %+v", ar)
	}
	if ar.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", ar.StopReason)
	}
	if len(ar.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2 (text+tool_use)", len(ar.Content))
	}
	if ar.Content[0].Type != "text" || ar.Content[0].Text != "here you go" {
		t.Errorf("content[0] = %+v, want text block", ar.Content[0])
	}
	if ar.Content[1].Type != "tool_use" || ar.Content[1].ID != "call_9" || ar.Content[1].Name != "get_weather" {
		t.Errorf("content[1] = %+v, want tool_use", ar.Content[1])
	}
	if string(ar.Content[1].Input) != `{"city":"SF"}` {
		t.Errorf("tool input = %s, want {\"city\":\"SF\"}", ar.Content[1].Input)
	}
	// PromptTokens (30) includes the 10 cached; Anthropic input_tokens is the
	// non-cached 20 with cache reported separately.
	if ar.Usage.InputTokens != 20 || ar.Usage.OutputTokens != 7 || ar.Usage.CacheReadInputTokens != 10 {
		t.Errorf("usage = %+v, want input 20 / output 7 / cache 10", ar.Usage)
	}
}

func TestConvertToolChoiceVariants(t *testing.T) {
	tests := []struct {
		name string
		raw  json.RawMessage
		want any
	}{
		{name: "empty", raw: nil, want: nil},
		{name: "malformed", raw: json.RawMessage(`{`), want: nil},
		{name: "any", raw: json.RawMessage(`{"type":"any"}`), want: "required"},
		{name: "none", raw: json.RawMessage(`{"type":"none"}`), want: "none"},
		{name: "tool", raw: json.RawMessage(`{"type":"tool","name":"lookup"}`), want: map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}}},
		{name: "unknown", raw: json.RawMessage(`{"type":"custom"}`), want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := convertToolChoice(tt.raw)
			gb, _ := json.Marshal(got)
			wb, _ := json.Marshal(tt.want)
			if string(gb) != string(wb) {
				t.Fatalf("convertToolChoice() = %s, want %s", gb, wb)
			}
		})
	}
}

func TestChatResponseToAnthropicDefaultsAndUsageClamp(t *testing.T) {
	resp := agentmodel.ChatResponse{
		Choices: []agentmodel.Choice{{FinishReason: "length"}},
		Usage: agentmodel.Usage{
			PromptTokens:             3,
			CompletionTokens:         2,
			CacheReadInputTokens:     4,
			CacheCreationInputTokens: 5,
		},
	}

	ar := chatResponseToAnthropic(resp, "model")
	if !strings.HasPrefix(ar.ID, "msg_") {
		t.Fatalf("generated id = %q, want msg_ prefix", ar.ID)
	}
	if ar.StopReason != "max_tokens" {
		t.Fatalf("stop_reason = %q, want max_tokens", ar.StopReason)
	}
	if ar.Usage.InputTokens != 0 {
		t.Fatalf("input_tokens = %d, want clamped 0", ar.Usage.InputTokens)
	}
	if ar.Usage.CacheReadInputTokens != 4 || ar.Usage.CacheCreationInputTokens != 5 {
		t.Fatalf("usage cache fields = %+v", ar.Usage)
	}
}

func TestJSONDefaultsForMalformedToolPayloads(t *testing.T) {
	if got := inputToArgs(json.RawMessage(`not-json`)); got != `{}` {
		t.Fatalf("inputToArgs malformed = %q, want {}", got)
	}
	if got := string(argsToInput(`not-json`)); got != `{}` {
		t.Fatalf("argsToInput malformed = %q, want {}", got)
	}
	if got := string(argsToInput(`{"ok":true}`)); got != `{"ok":true}` {
		t.Fatalf("argsToInput valid = %q, want valid object", got)
	}
}

func TestAnthropicToChatRequestRejectsMalformedContent(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":123}]}`)
	_, err := anthropicToChatRequest(body, "")
	if err == nil || !strings.Contains(err.Error(), "decode user message content") {
		t.Fatalf("anthropicToChatRequest err = %v, want malformed content error", err)
	}
}

// ─── MessagesPassthrough: non-streaming ──────────────────────────────────────

func TestMessagesPassthrough_NonStreaming(t *testing.T) {
	inner := &stub.Stub{
		AuthModeValue: agentmodel.AuthModeSubscription,
		CompleteResp: agentmodel.ChatResponse{
			ID:      "resp_1",
			Choices: []agentmodel.Choice{{Message: agentmodel.Message{Role: "assistant", Content: "pong"}, FinishReason: "stop"}},
			Usage:   agentmodel.Usage{PromptTokens: 5, CompletionTokens: 1, TotalTokens: 6},
		},
	}
	b := New(inner)

	body := []byte(`{"model":"x","max_tokens":16,"messages":[{"role":"user","content":"ping"}]}`)
	resp, err := b.MessagesPassthrough(context.Background(), body, "gpt-5-codex", "")
	if err != nil {
		t.Fatalf("passthrough: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}

	var ar anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ar.Model != "gpt-5-codex" || ar.StopReason != "end_turn" {
		t.Errorf("resp = %+v", ar)
	}
	if len(ar.Content) != 1 || ar.Content[0].Text != "pong" {
		t.Errorf("content = %+v, want one text 'pong'", ar.Content)
	}
}

// ─── MessagesPassthrough: streaming ──────────────────────────────────────────

type sseEvent struct {
	name string
	data string
}

func collectSSE(t *testing.T, r io.Reader) []sseEvent {
	t.Helper()
	var events []sseEvent
	var name, data string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if name != "" || data != "" {
				events = append(events, sseEvent{name, data})
			}
			name, data = "", ""
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	if name != "" || data != "" {
		events = append(events, sseEvent{name, data})
	}
	return events
}

func eventNames(evs []sseEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.name
	}
	return out
}

func TestMessagesPassthrough_StreamingText(t *testing.T) {
	inner := &stub.Stub{
		AuthModeValue: agentmodel.AuthModeSubscription,
		StreamChunks: []provider.StreamChunk{
			{Delta: agentmodel.Message{Role: "assistant", Content: "Hello"}},
			{Delta: agentmodel.Message{Content: " world"}},
			{FinishReason: "stop", Usage: &agentmodel.Usage{PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15, CacheReadInputTokens: 4}},
		},
	}
	b := New(inner)

	body := []byte(`{"model":"x","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	resp, err := b.MessagesPassthrough(context.Background(), body, "gpt-5-codex", "")
	if err != nil {
		t.Fatalf("passthrough: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	evs := collectSSE(t, resp.Body)
	wantOrder := []string{
		"message_start", "content_block_start", "content_block_delta",
		"content_block_delta", "content_block_stop", "message_delta", "message_stop",
	}
	if got := eventNames(evs); strings.Join(got, ",") != strings.Join(wantOrder, ",") {
		t.Fatalf("event order = %v, want %v", got, wantOrder)
	}

	// Reassemble streamed text.
	var text strings.Builder
	for _, e := range evs {
		if e.name != "content_block_delta" {
			continue
		}
		var ev struct {
			Delta struct {
				Text string `json:"text"`
			} `json:"delta"`
		}
		_ = json.Unmarshal([]byte(e.data), &ev)
		text.WriteString(ev.Delta.Text)
	}
	if text.String() != "Hello world" {
		t.Errorf("streamed text = %q, want 'Hello world'", text.String())
	}

	// Final usage must carry input (non-cached) + output so the gateway ledger
	// records it: PromptTokens 12 - cache 4 = input 8, output 3.
	var md struct {
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
		Usage struct {
			InputTokens          int `json:"input_tokens"`
			OutputTokens         int `json:"output_tokens"`
			CacheReadInputTokens int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(evs[5].data), &md); err != nil {
		t.Fatalf("decode message_delta: %v", err)
	}
	if md.Delta.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", md.Delta.StopReason)
	}
	if md.Usage.InputTokens != 8 || md.Usage.OutputTokens != 3 || md.Usage.CacheReadInputTokens != 4 {
		t.Errorf("message_delta usage = %+v, want input 8 / output 3 / cache 4", md.Usage)
	}
}

func TestMessagesPassthrough_StreamingToolUse(t *testing.T) {
	inner := &stub.Stub{
		AuthModeValue: agentmodel.AuthModeSubscription,
		StreamChunks: []provider.StreamChunk{
			{Delta: agentmodel.Message{Role: "assistant", ToolCalls: []agentmodel.ToolCall{{
				Index: iptr(0), ID: "call_1|item_1", Type: "function",
				Function: agentmodel.ToolCallFunction{Name: "get_weather"},
			}}}},
			{Delta: agentmodel.Message{ToolCalls: []agentmodel.ToolCall{{
				Index: iptr(0), Function: agentmodel.ToolCallFunction{Arguments: `{"city":`},
			}}}},
			{Delta: agentmodel.Message{ToolCalls: []agentmodel.ToolCall{{
				Index: iptr(0), Function: agentmodel.ToolCallFunction{Arguments: `"SF"}`},
			}}}},
			{FinishReason: "tool_calls", Usage: &agentmodel.Usage{PromptTokens: 20, CompletionTokens: 5, TotalTokens: 25}},
		},
	}
	b := New(inner)

	body := []byte(`{"model":"x","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"weather?"}]}`)
	resp, err := b.MessagesPassthrough(context.Background(), body, "gpt-5-codex", "")
	if err != nil {
		t.Fatalf("passthrough: %v", err)
	}
	defer resp.Body.Close()

	evs := collectSSE(t, resp.Body)

	// content_block_start tool_use carries id + name.
	var startSeen bool
	var args strings.Builder
	var stopReason string
	for _, e := range evs {
		switch e.name {
		case "content_block_start":
			var ev struct {
				ContentBlock struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"content_block"`
			}
			_ = json.Unmarshal([]byte(e.data), &ev)
			if ev.ContentBlock.Type == "tool_use" {
				startSeen = true
				if ev.ContentBlock.ID != "call_1|item_1" || ev.ContentBlock.Name != "get_weather" {
					t.Errorf("tool_use start = %+v", ev.ContentBlock)
				}
			}
		case "content_block_delta":
			var ev struct {
				Delta struct {
					Type        string `json:"type"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			_ = json.Unmarshal([]byte(e.data), &ev)
			if ev.Delta.Type == "input_json_delta" {
				args.WriteString(ev.Delta.PartialJSON)
			}
		case "message_delta":
			var ev struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
			}
			_ = json.Unmarshal([]byte(e.data), &ev)
			stopReason = ev.Delta.StopReason
		}
	}
	if !startSeen {
		t.Error("no tool_use content_block_start emitted")
	}
	if args.String() != `{"city":"SF"}` {
		t.Errorf("assembled tool args = %q, want {\"city\":\"SF\"}", args.String())
	}
	if stopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", stopReason)
	}
}

func TestMessagesPassthrough_StreamingError(t *testing.T) {
	inner := &stub.Stub{
		AuthModeValue:  agentmodel.AuthModeSubscription,
		StreamChunks:   []provider.StreamChunk{{Delta: agentmodel.Message{Role: "assistant", Content: "partial"}}},
		StreamYieldErr: errors.New("upstream boom"),
	}
	b := New(inner)

	body := []byte(`{"model":"x","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	resp, err := b.MessagesPassthrough(context.Background(), body, "gpt-5-codex", "")
	if err != nil {
		t.Fatalf("passthrough: %v", err)
	}
	defer resp.Body.Close()

	evs := collectSSE(t, resp.Body)
	// A content block was open (the "partial" text), so the error must be
	// preceded by content_block_stop — no unterminated block before the error.
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "error"}
	if got := eventNames(evs); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event order = %v, want %v", got, want)
	}
	if !strings.Contains(evs[len(evs)-1].data, "upstream boom") {
		t.Errorf("error event = %q, want it to mention 'upstream boom'", evs[len(evs)-1].data)
	}
}

// TestMessagesPassthrough_StreamingErrorClassifiesType is the /v1/messages
// sibling of the #705 fix already applied to /v1/chat/completions: the SSE
// error frame must carry the classified type and code, not a blanket
// "api_error". Callers key retry decisions off this — a blown context window
// is terminal, a transient upstream 500 is not.
func TestMessagesPassthrough_StreamingErrorClassifiesType(t *testing.T) {
	cases := []struct {
		desc     string
		yieldErr error
		wantType string
		wantCode string
	}{
		{
			desc:     "context window",
			yieldErr: errors.New(`chatgpt: stream error: {"type":"error","error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model."}}`),
			wantType: agentmodel.ErrTypeContextWindow,
			wantCode: agentmodel.CodeContextLength,
		},
		{
			desc:     "rate limit",
			yieldErr: errors.New("chatgpt: 429: rate limit exceeded"),
			wantType: agentmodel.ErrTypeRateLimit,
			wantCode: agentmodel.CodeRateLimitExceeded,
		},
		{
			desc:     "unclassifiable stays upstream_error",
			yieldErr: errors.New("upstream boom"),
			wantType: agentmodel.ErrTypeUpstream,
			wantCode: "",
		},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			inner := &stub.Stub{
				AuthModeValue:  agentmodel.AuthModeSubscription,
				StreamChunks:   []provider.StreamChunk{{Delta: agentmodel.Message{Role: "assistant", Content: "partial"}}},
				StreamYieldErr: c.yieldErr,
			}
			b := New(inner)

			body := []byte(`{"model":"x","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
			resp, err := b.MessagesPassthrough(context.Background(), body, "gpt-5-codex", "")
			if err != nil {
				t.Fatalf("passthrough: %v", err)
			}
			defer resp.Body.Close()

			evs := collectSSE(t, resp.Body)
			last := evs[len(evs)-1]
			if last.name != "error" {
				t.Fatalf("last event = %q, want error", last.name)
			}

			var frame struct {
				Error struct {
					Type    string `json:"type"`
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(last.data), &frame); err != nil {
				t.Fatalf("decode error frame %q: %v", last.data, err)
			}
			if frame.Error.Type != c.wantType {
				t.Errorf("error.type = %q, want %q", frame.Error.Type, c.wantType)
			}
			if frame.Error.Code != c.wantCode {
				t.Errorf("error.code = %q, want %q", frame.Error.Code, c.wantCode)
			}
			// The upstream detail must survive classification.
			if !strings.Contains(frame.Error.Message, c.yieldErr.Error()) {
				t.Errorf("error.message = %q, want it to contain %q", frame.Error.Message, c.yieldErr.Error())
			}
		})
	}
}

func TestMessagesPassthrough_StreamingEmptyCompletion(t *testing.T) {
	// A finish with no content deltas must still produce one content block so
	// the stream is well-formed for clients that expect at least one block.
	inner := &stub.Stub{
		AuthModeValue: agentmodel.AuthModeSubscription,
		StreamChunks: []provider.StreamChunk{
			{FinishReason: "stop", Usage: &agentmodel.Usage{PromptTokens: 5, CompletionTokens: 0, TotalTokens: 5}},
		},
	}
	b := New(inner)

	body := []byte(`{"model":"x","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	resp, err := b.MessagesPassthrough(context.Background(), body, "gpt-5-codex", "")
	if err != nil {
		t.Fatalf("passthrough: %v", err)
	}
	defer resp.Body.Close()

	evs := collectSSE(t, resp.Body)
	want := []string{"message_start", "content_block_start", "content_block_stop", "message_delta", "message_stop"}
	if got := eventNames(evs); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event order = %v, want %v", got, want)
	}
}

func TestMessagesPassthrough_StreamingOrphanToolArgs(t *testing.T) {
	// An arguments fragment with no preceding start (no id/name) must be dropped
	// rather than emitted as a tool_use block with an empty id/name.
	inner := &stub.Stub{
		AuthModeValue: agentmodel.AuthModeSubscription,
		StreamChunks: []provider.StreamChunk{
			{Delta: agentmodel.Message{Role: "assistant", ToolCalls: []agentmodel.ToolCall{{
				Index: iptr(0), Function: agentmodel.ToolCallFunction{Arguments: `{"x":1}`},
			}}}},
			{FinishReason: "tool_calls", Usage: &agentmodel.Usage{PromptTokens: 5, CompletionTokens: 1, TotalTokens: 6}},
		},
	}
	b := New(inner)

	body := []byte(`{"model":"x","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	resp, err := b.MessagesPassthrough(context.Background(), body, "gpt-5-codex", "")
	if err != nil {
		t.Fatalf("passthrough: %v", err)
	}
	defer resp.Body.Close()

	for _, e := range collectSSE(t, resp.Body) {
		if e.name == "content_block_start" && strings.Contains(e.data, "tool_use") {
			if strings.Contains(e.data, `"id":""`) || strings.Contains(e.data, `"name":""`) {
				t.Fatalf("emitted tool_use block with empty id/name: %s", e.data)
			}
		}
	}
}

// ─── optional-capability forwarding ──────────────────────────────────────────

func TestBridge_GenerateImage_ForwardsToWrappedProvider(t *testing.T) {
	// Embedding provider.Provider does not promote optional capability
	// interfaces like provider.ImageGenerator, so Bridge needs an explicit
	// forwarding method — without it, a router type assertion for
	// provider.ImageGenerator against a Bridge-wrapped provider silently fails
	// even when the wrapped provider supports image generation.
	inner := &stub.Stub{}
	b := New(inner)

	var _ provider.ImageGenerator = b // compile-time: Bridge must satisfy it

	resp, err := b.GenerateImage(context.Background(), agentmodel.ImageRequest{Model: "gpt-image-1", Prompt: "a cat"})
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if len(resp.Data) == 0 {
		t.Fatal("expected image data forwarded from the wrapped provider")
	}
}

func TestBridge_GenerateImage_WrappedProviderNotCapable(t *testing.T) {
	// A wrapped provider that doesn't implement provider.ImageGenerator must
	// surface a clean invalid_request error, not a panic or a silent no-op.
	inner := notImageCapable{}
	b := New(inner)

	_, err := b.GenerateImage(context.Background(), agentmodel.ImageRequest{Model: "x", Prompt: "a cat"})
	if err == nil {
		t.Fatal("expected error for a wrapped provider without image generation")
	}
	if ae := agentmodel.Wrap(err); ae.Type != agentmodel.ErrTypeInvalidRequest {
		t.Fatalf("error type = %q, want %q", ae.Type, agentmodel.ErrTypeInvalidRequest)
	}
}

// notImageCapable implements provider.Provider but deliberately NOT
// provider.ImageGenerator, to exercise Bridge.GenerateImage's fallback error.
type notImageCapable struct{}

func (notImageCapable) Name() string              { return "not-image-capable" }
func (notImageCapable) SupportedModels() []string { return nil }
func (notImageCapable) AuthMode() string          { return agentmodel.AuthModeAPIKey }
func (notImageCapable) Complete(context.Context, agentmodel.ChatRequest) (agentmodel.ChatResponse, error) {
	return agentmodel.ChatResponse{}, nil
}
func (notImageCapable) Stream(context.Context, agentmodel.ChatRequest) (iter.Seq2[provider.StreamChunk, error], error) {
	return nil, nil
}
func (notImageCapable) Embed(context.Context, agentmodel.EmbeddingRequest) (agentmodel.EmbeddingResponse, error) {
	return agentmodel.EmbeddingResponse{}, nil
}

// TestChatResponseToAnthropic_EmptyCompletionHasOneBlock guards #1494: a
// non-stream empty completion must carry one (empty) content block, matching the
// streaming finish() guard — a zero-block response reads as truncated to a
// strict Anthropic client.
func TestChatResponseToAnthropic_EmptyCompletionHasOneBlock(t *testing.T) {
	// No choices at all, and a choice with empty content + no tool calls.
	for _, resp := range []agentmodel.ChatResponse{
		{},
		{Choices: []agentmodel.Choice{{Message: agentmodel.Message{Role: "assistant", Content: ""}}}},
	} {
		ar := chatResponseToAnthropic(resp, "model")
		if len(ar.Content) != 1 {
			t.Errorf("content blocks = %d, want 1 (empty completion must still emit one block); resp=%+v", len(ar.Content), resp)
		}
	}
}

// Bridge is the OUTERMOST wrapper for a pooled chatgpt deployment (the factory
// builds messagesbridge.New(pool.New(...))), so any optional capability it fails
// to forward is erased no matter how faithfully the pool forwards it. pool.Pool
// forwards provider.ModelLister; without the matching forwarder here, a pooled
// deployment drops out of drift detection exactly as it did before that was
// fixed.
func TestBridgeForwardsListModels(t *testing.T) {
	inner := &stub.Stub{NameValue: "chatgpt", LiveModels: []string{"gpt-5", "gpt-5-codex"}}
	b := New(inner)

	lister, ok := any(b).(provider.ModelLister)
	if !ok {
		t.Fatal("Bridge must satisfy provider.ModelLister — the router type-asserts it and silently skips what fails")
	}
	models, err := lister.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 || models[0] != "gpt-5" {
		t.Errorf("models=%v, want the wrapped provider's list", models)
	}
}

func TestBridgeListModelsOnNonListerIsTerminal(t *testing.T) {
	// A wrapped provider that cannot list must produce a terminal capability
	// miss, matching GenerateImage, not a retryable failure the router walks.
	b := New(notImageCapable{}) // also not a ModelLister
	_, err := b.ListModels(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if ae := agentmodel.Wrap(err); ae.Retryable() {
		t.Errorf("capability miss must not be retryable, got type %q", ae.Type)
	}
}
