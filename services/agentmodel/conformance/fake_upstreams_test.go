package conformance

// Stateful fake upstream servers that speak the NATIVE OpenAI and Anthropic
// wire dialects, sitting behind agent-model's real provider adapters. They are
// the deterministic CI substrate for the cross-provider conformance test
// (issue #664): the gateway translates a single OpenAI-shaped request into each
// provider's dialect, the fake replies in that dialect, and the gateway
// translates back. A stub plugged in behind the adapters (provider/stub) could
// only prove the normalized waist equals itself — these fakes exercise the
// translation code the conformance claim is actually about.
//
// Dispatch is on request CONTENT, mirroring a real multi-turn tool conversation:
//   - no tool result, non-streaming -> a native tool-call response (turn A)
//   - no tool result, streaming     -> a native streamed tool call    (turn B1)
//   - tool result present, streaming -> a native streamed final answer (turn B2)
//
// Each fake also asserts the inbound request is in its own dialect, so a
// translation regression in either direction fails loudly.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The single tool the conformance transcript offers and the canned arguments
// the fakes "call" it with. Kept in one place so the driver can assert that the
// emitted tool name is one it offered (N8).
const (
	conformanceToolName = "get_weather"
	conformanceToolArgs = `{"location":"SF"}`
)

// writeSSEData writes one OpenAI-style `data: {json}` SSE frame and flushes.
func writeSSEData(w http.ResponseWriter, fl http.Flusher, v any) {
	b, _ := json.Marshal(v)
	_, _ = io.WriteString(w, "data: "+string(b)+"\n\n")
	if fl != nil {
		fl.Flush()
	}
}

// writeSSEEvent writes one Anthropic-style `event: x\ndata: {json}` SSE frame
// and flushes (Anthropic streams name their events).
func writeSSEEvent(w http.ResponseWriter, fl http.Flusher, event string, v any) {
	b, _ := json.Marshal(v)
	_, _ = io.WriteString(w, "event: "+event+"\ndata: "+string(b)+"\n\n")
	if fl != nil {
		fl.Flush()
	}
}

// ─── OpenAI fake upstream ─────────────────────────────────────────────────────

func newFakeOpenAIUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		// Dialect assertions: this must look like an OpenAI request.
		if !strings.Contains(r.URL.Path, "/chat/completions") {
			t.Errorf("fakeOpenAI: unexpected path %q, want .../chat/completions", r.URL.Path)
		}
		if a := r.Header.Get("Authorization"); !strings.HasPrefix(a, "Bearer") {
			t.Errorf("fakeOpenAI: Authorization header = %q, want Bearer prefix", a)
		}

		var req struct {
			Stream     bool `json:"stream"`
			StreamOpts *struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
			Tools []json.RawMessage `json:"tools"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("fakeOpenAI: decode request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		hasToolResult := false
		for _, m := range req.Messages {
			if m.Role == "tool" {
				hasToolResult = true
			}
		}

		if req.Stream {
			if req.StreamOpts == nil || !req.StreamOpts.IncludeUsage {
				t.Errorf("fakeOpenAI: streaming request missing stream_options.include_usage")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl, _ := w.(http.Flusher)
			if hasToolResult {
				streamOpenAIFinalAnswer(w, fl)
			} else {
				streamOpenAIToolCall(w, fl)
			}
			return
		}

		// Non-streaming turn: forced tool call.
		if len(req.Tools) == 0 {
			t.Errorf("fakeOpenAI: non-streaming turn expected tools to be offered")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openAIToolCallResponse())
	}))
	t.Cleanup(srv.Close)
	return srv
}

func openAIToolCallResponse() map[string]any {
	return map[string]any{
		"id":      "chatcmpl-tool",
		"object":  "chat.completion",
		"created": 1700000000,
		"model":   "gpt-4o",
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{map[string]any{
					"id":   "call_ns_1",
					"type": "function",
					"function": map[string]any{
						"name":      conformanceToolName,
						"arguments": conformanceToolArgs,
					},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 8, "total_tokens": 18},
	}
}

func streamOpenAIToolCall(w http.ResponseWriter, fl http.Flusher) {
	id := "chatcmpl-stool"
	base := func(delta any, finish string) map[string]any {
		choice := map[string]any{"index": 0, "delta": delta}
		if finish != "" {
			choice["finish_reason"] = finish
		}
		return map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": 1700000000,
			"model": "gpt-4o", "choices": []any{choice},
		}
	}
	writeSSEData(w, fl, base(map[string]any{"role": "assistant", "content": ""}, ""))
	writeSSEData(w, fl, base(map[string]any{"tool_calls": []any{map[string]any{
		"index": 0, "id": "call_s_1", "type": "function",
		"function": map[string]any{"name": conformanceToolName, "arguments": ""},
	}}}, ""))
	writeSSEData(w, fl, base(map[string]any{"tool_calls": []any{map[string]any{
		"index": 0, "function": map[string]any{"arguments": `{"location":`},
	}}}, ""))
	writeSSEData(w, fl, base(map[string]any{"tool_calls": []any{map[string]any{
		"index": 0, "function": map[string]any{"arguments": `"SF"}`},
	}}}, ""))
	writeSSEData(w, fl, base(map[string]any{}, "tool_calls"))
	writeSSEData(w, fl, map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": 1700000000, "model": "gpt-4o",
		"choices": []any{}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 8, "total_tokens": 18},
	})
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

func streamOpenAIFinalAnswer(w http.ResponseWriter, fl http.Flusher) {
	id := "chatcmpl-final"
	base := func(delta any, finish string) map[string]any {
		choice := map[string]any{"index": 0, "delta": delta}
		if finish != "" {
			choice["finish_reason"] = finish
		}
		return map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": 1700000000,
			"model": "gpt-4o", "choices": []any{choice},
		}
	}
	writeSSEData(w, fl, base(map[string]any{"role": "assistant", "content": "It"}, ""))
	writeSSEData(w, fl, base(map[string]any{"content": " is sunny in SF."}, ""))
	writeSSEData(w, fl, base(map[string]any{}, "stop"))
	writeSSEData(w, fl, map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": 1700000000, "model": "gpt-4o",
		"choices": []any{}, "usage": map[string]any{"prompt_tokens": 20, "completion_tokens": 5, "total_tokens": 25},
	})
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

// ─── Anthropic fake upstream ──────────────────────────────────────────────────

func newFakeAnthropicUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s := string(body)

		// Dialect assertions: this must look like an Anthropic Messages request.
		if !strings.Contains(r.URL.Path, "/v1/messages") {
			t.Errorf("fakeAnthropic: unexpected path %q, want .../v1/messages", r.URL.Path)
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Errorf("fakeAnthropic: missing anthropic-version header")
		}

		var req struct {
			Stream    bool              `json:"stream"`
			MaxTokens *int              `json:"max_tokens"`
			Tools     []json.RawMessage `json:"tools"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("fakeAnthropic: decode request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req.MaxTokens == nil {
			t.Errorf("fakeAnthropic: request missing max_tokens (Anthropic requires it)")
		}

		hasToolResult := strings.Contains(s, "tool_result")

		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl, _ := w.(http.Flusher)
			if hasToolResult {
				streamAnthropicFinalAnswer(w, fl)
			} else {
				streamAnthropicToolUse(w, fl)
			}
			return
		}

		// Non-streaming turn: forced tool call. Assert tools are flat Anthropic
		// shape (name + input_schema, never the OpenAI nested-function form).
		if len(req.Tools) == 0 {
			t.Errorf("fakeAnthropic: non-streaming turn expected tools to be offered")
		} else {
			var tool map[string]any
			if err := json.Unmarshal(req.Tools[0], &tool); err == nil {
				if _, ok := tool["input_schema"]; !ok {
					t.Errorf("fakeAnthropic: tool missing input_schema (got %v)", tool)
				}
				if _, ok := tool["function"]; ok {
					t.Errorf("fakeAnthropic: tool has OpenAI nested 'function'; want flat")
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(anthropicToolUseResponse())
	}))
	t.Cleanup(srv.Close)
	return srv
}

func anthropicToolUseResponse() map[string]any {
	return map[string]any{
		"id":    "msg_tool",
		"type":  "message",
		"role":  "assistant",
		"model": "claude-haiku-4-5",
		"content": []any{map[string]any{
			"type":  "tool_use",
			"id":    "toolu_ns",
			"name":  conformanceToolName,
			"input": map[string]any{"location": "SF"},
		}},
		"stop_reason": "tool_use",
		"usage":       map[string]any{"input_tokens": 10, "output_tokens": 8},
	}
}

func streamAnthropicToolUse(w http.ResponseWriter, fl http.Flusher) {
	writeSSEEvent(w, fl, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_stool", "model": "claude-haiku-4-5", "role": "assistant",
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 0},
		},
	})
	writeSSEEvent(w, fl, "content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "tool_use", "id": "toolu_s", "name": conformanceToolName, "input": map[string]any{}},
	})
	writeSSEEvent(w, fl, "content_block_delta", map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": `{"location":`},
	})
	writeSSEEvent(w, fl, "content_block_delta", map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": `"SF"}`},
	})
	writeSSEEvent(w, fl, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	writeSSEEvent(w, fl, "message_delta", map[string]any{
		"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"},
		"usage": map[string]any{"output_tokens": 8},
	})
	writeSSEEvent(w, fl, "message_stop", map[string]any{"type": "message_stop"})
}

func streamAnthropicFinalAnswer(w http.ResponseWriter, fl http.Flusher) {
	writeSSEEvent(w, fl, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_final", "model": "claude-haiku-4-5", "role": "assistant",
			"usage": map[string]any{"input_tokens": 20, "output_tokens": 0},
		},
	})
	writeSSEEvent(w, fl, "content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
	writeSSEEvent(w, fl, "content_block_delta", map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": "It is sunny in SF."},
	})
	writeSSEEvent(w, fl, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	writeSSEEvent(w, fl, "message_delta", map[string]any{
		"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"},
		"usage": map[string]any{"output_tokens": 5},
	})
	writeSSEEvent(w, fl, "message_stop", map[string]any{"type": "message_stop"})
}
