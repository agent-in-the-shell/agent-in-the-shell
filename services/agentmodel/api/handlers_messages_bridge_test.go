package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/messagesbridge"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/stub"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
)

// TestMessages_BridgedProvider_Streaming exercises the full /v1/messages path
// against a non-Anthropic provider wrapped by messagesbridge: the handler must
// accept the synthesized SSE response, forward it, and extract usage for the
// audit row. This is the end-to-end proof that the bridge's synthesized
// *http.Response satisfies the real handler (cf. codex/pi-mom).
func TestMessages_BridgedProvider_Streaming(t *testing.T) {
	inner := &stub.Stub{
		NameValue:     "chatgpt",
		AuthModeValue: agentmodel.AuthModeSubscription,
		StreamChunks: []provider.StreamChunk{
			{Delta: agentmodel.Message{Role: "assistant", Content: "Hello"}},
			{Delta: agentmodel.Message{Content: " world"}},
			{FinishReason: "stop", Usage: &agentmodel.Usage{PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15, CacheReadInputTokens: 4}},
		},
	}
	ts, st := newTestServer(t, map[string][]router.Deployment{
		"gpt-5-codex": {{Provider: messagesbridge.New(inner), Model: "gpt-5-codex", Weight: 1}},
	})

	resp := mustPost(t, ts, "/v1/messages", map[string]any{
		"model":      "gpt-5-codex",
		"max_tokens": 64,
		"stream":     true,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	}, testToken)
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	var names []string
	var text strings.Builder
	sc := bufio.NewScanner(resp.Body)
	var event, data string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if event != "" {
				names = append(names, event)
			}
			if event == "content_block_delta" {
				var ev struct {
					Delta struct {
						Text string `json:"text"`
					} `json:"delta"`
				}
				_ = json.Unmarshal([]byte(data), &ev)
				text.WriteString(ev.Delta.Text)
			}
			event, data = "", ""
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}

	if text.String() != "Hello world" {
		t.Errorf("streamed text = %q, want 'Hello world'", text.String())
	}
	if len(names) == 0 || names[0] != "message_start" || names[len(names)-1] != "message_stop" {
		t.Errorf("event envelope = %v, want message_start ... message_stop", names)
	}

	// The audit row must reflect usage parsed out of the synthesized stream:
	// input 8 (non-cached) + cache 4 = PromptTokens 12, CompletionTokens 3.
	logs, err := st.ListByOrg(context.Background(), "default", time.Now().Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("audit logs = %d, want 1", len(logs))
	}
	row := logs[0]
	if row.Status != "ok" {
		t.Errorf("status = %q, want ok", row.Status)
	}
	if row.PromptTokens != 12 || row.CompletionTokens != 3 || row.TotalTokens != 15 {
		t.Errorf("usage = prompt %d / completion %d / total %d, want 12/3/15", row.PromptTokens, row.CompletionTokens, row.TotalTokens)
	}
	if row.ModelRequested != "gpt-5-codex" {
		t.Errorf("model_requested = %q, want gpt-5-codex", row.ModelRequested)
	}
}

// TestMessages_ErrorBodyCarriesCode pins that the non-streaming JSON error body
// and the streaming SSE error frame describe the same failure the same way.
//
// writeAnthropicError used to flatten the typed error down to {type, message},
// dropping the machine-readable code — so a caller could read
// "context_length_exceeded" off a stream but not off the non-stream reply to
// the identical request, and had to string-match the message to tell a blown
// context window from a transient upstream fault.
func TestMessages_ErrorBodyCarriesCode(t *testing.T) {
	// Verbatim shape the ChatGPT Responses backend returns.
	upstreamErr := errors.New(`chatgpt: stream error: {"type":"error","error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model."}}`)

	// StreamYieldErr (not StreamErr) so the stream actually opens and then dies
	// mid-flight — that is the path that emits an SSE error frame. StreamErr
	// fails before any bytes are sent and falls back to the JSON error path.
	newSrv := func(t *testing.T) *httptest.Server {
		inner := &stub.Stub{
			NameValue:      "chatgpt",
			AuthModeValue:  agentmodel.AuthModeSubscription,
			CompleteErr:    upstreamErr,
			StreamChunks:   []provider.StreamChunk{{Delta: agentmodel.Message{Role: "assistant", Content: "partial"}}},
			StreamYieldErr: upstreamErr,
		}
		ts, _ := newTestServer(t, map[string][]router.Deployment{
			"gpt-5-codex": {{Provider: messagesbridge.New(inner), Model: "gpt-5-codex", Weight: 1}},
		})
		return ts
	}

	t.Run("non-streaming JSON body", func(t *testing.T) {
		ts := newSrv(t)
		resp := mustPost(t, ts, "/v1/messages", map[string]any{
			"model": "gpt-5-codex", "max_tokens": 64,
			"messages": []map[string]any{{"role": "user", "content": "ping"}},
		}, testToken)
		defer resp.Body.Close()

		if resp.StatusCode != 400 {
			t.Errorf("status = %d, want 400 (terminal, caller-caused)", resp.StatusCode)
		}
		var body struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Type != "error" {
			t.Errorf("envelope type = %q, want error", body.Type)
		}
		if body.Error.Type != agentmodel.ErrTypeContextWindow {
			t.Errorf("error.type = %q, want %q", body.Error.Type, agentmodel.ErrTypeContextWindow)
		}
		if body.Error.Code != agentmodel.CodeContextLength {
			t.Errorf("error.code = %q, want %q", body.Error.Code, agentmodel.CodeContextLength)
		}
		if !strings.Contains(body.Error.Message, "context window") {
			t.Errorf("error.message = %q, want the upstream detail preserved", body.Error.Message)
		}
	})

	t.Run("streaming SSE frame agrees", func(t *testing.T) {
		ts := newSrv(t)
		resp := mustPost(t, ts, "/v1/messages", map[string]any{
			"model": "gpt-5-codex", "max_tokens": 64, "stream": true,
			"messages": []map[string]any{{"role": "user", "content": "ping"}},
		}, testToken)
		defer resp.Body.Close()

		var frame struct {
			Error struct {
				Type string `json:"type"`
				Code string `json:"code"`
			} `json:"error"`
		}
		found := false
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if err := json.Unmarshal([]byte(payload), &frame); err == nil && frame.Error.Type != "" {
				found = true
			}
		}
		if !found {
			t.Fatal("no error frame in stream")
		}
		if frame.Error.Type != agentmodel.ErrTypeContextWindow {
			t.Errorf("frame error.type = %q, want %q", frame.Error.Type, agentmodel.ErrTypeContextWindow)
		}
		if frame.Error.Code != agentmodel.CodeContextLength {
			t.Errorf("frame error.code = %q, want %q", frame.Error.Code, agentmodel.CodeContextLength)
		}
	})
}

// TestMessages_FailureRowNamesDeployment pins the audit row for an upstream
// failure that never opened a stream. The router walk knows which deployment
// produced the error; before this, it discarded that and the row landed with
// provider=” and model_used=<the requested alias>. A mid-stream failure of the
// same request on the same deployment recorded both — the two rows disagreed
// about what had just happened.
func TestMessages_FailureRowNamesDeployment(t *testing.T) {
	upstreamErr := errors.New(`chatgpt: stream error: {"type":"error","error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"too long"}}`)
	inner := &stub.Stub{
		NameValue:     "chatgpt",
		AuthModeValue: agentmodel.AuthModeSubscription,
		CompleteErr:   upstreamErr,
	}
	ts, st := newTestServer(t, map[string][]router.Deployment{
		"claude-sonnet-4-5": {{Name: "sub/codex", Provider: messagesbridge.New(inner), Model: "gpt-5.5", Weight: 1}},
	})

	resp := mustPost(t, ts, "/v1/messages", map[string]any{
		"model": "claude-sonnet-4-5", "max_tokens": 64,
		"messages": []map[string]any{{"role": "user", "content": "ping"}},
	}, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	logs, err := st.ListByOrg(context.Background(), "default", time.Now().Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(logs))
	}
	row := logs[0]
	if row.Status != "error" || row.ErrorType != agentmodel.ErrTypeContextWindow {
		t.Errorf("status/error_type = %q/%q", row.Status, row.ErrorType)
	}
	if row.Provider != "chatgpt" {
		t.Errorf("provider = %q, want chatgpt", row.Provider)
	}
	if row.ModelRequested != "claude-sonnet-4-5" {
		t.Errorf("model_requested = %q, want the caller's alias", row.ModelRequested)
	}
	if row.ModelUsed != "gpt-5.5" {
		t.Errorf("model_used = %q, want the upstream id the deployment dispatched to", row.ModelUsed)
	}
	// The deployment is subscription-billed, so the row's $0 is explained, not
	// unknown. A bare $0 with no source reads as "we lost the price".
	if row.CostSource != "subscription" {
		t.Errorf("cost_source = %q, want subscription", row.CostSource)
	}
}

// A failure with no deployment to blame must not invent one. Policy rejections
// happen before any walk; "no deployment configured" never reaches a provider.
func TestMessages_UnattributableFailureRowHasNoProvider(t *testing.T) {
	ts, st := newTestServer(t, map[string][]router.Deployment{})

	resp := mustPost(t, ts, "/v1/messages", map[string]any{
		"model": "nonexistent", "max_tokens": 64,
		"messages": []map[string]any{{"role": "user", "content": "ping"}},
	}, testToken)
	defer resp.Body.Close()

	logs, err := st.ListByOrg(context.Background(), "default", time.Now().Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(logs))
	}
	row := logs[0]
	if row.Provider != "" {
		t.Errorf("provider = %q, want empty (nothing to attribute)", row.Provider)
	}
	if row.ModelUsed != "nonexistent" {
		t.Errorf("model_used = %q, want the requested model as the fallback", row.ModelUsed)
	}
	// Nothing dispatched, so there is no pricing decision to report — and no
	// WARN about a model that was never sent anywhere.
	if row.CostSource != "" {
		t.Errorf("cost_source = %q, want empty (nothing was dispatched)", row.CostSource)
	}
}

// TestMessages_BridgedProvider_NonStreaming covers the buffered JSON path.
func TestMessages_BridgedProvider_NonStreaming(t *testing.T) {
	inner := &stub.Stub{
		NameValue:     "chatgpt",
		AuthModeValue: agentmodel.AuthModeSubscription,
		CompleteResp: agentmodel.ChatResponse{
			ID:      "resp_1",
			Choices: []agentmodel.Choice{{Message: agentmodel.Message{Role: "assistant", Content: "pong"}, FinishReason: "stop"}},
			Usage:   agentmodel.Usage{PromptTokens: 9, CompletionTokens: 1, TotalTokens: 10},
		},
	}
	ts, st := newTestServer(t, map[string][]router.Deployment{
		"gpt-5-codex": {{Provider: messagesbridge.New(inner), Model: "gpt-5-codex", Weight: 1}},
	})

	resp := mustPost(t, ts, "/v1/messages", map[string]any{
		"model":      "gpt-5-codex",
		"max_tokens": 64,
		"messages":   []map[string]any{{"role": "user", "content": "ping"}},
	}, testToken)
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Type != "message" || body.Role != "assistant" || body.StopReason != "end_turn" {
		t.Errorf("envelope = %+v", body)
	}
	if len(body.Content) != 1 || body.Content[0].Text != "pong" {
		t.Errorf("content = %+v, want one text 'pong'", body.Content)
	}

	logs, err := st.ListByOrg(context.Background(), "default", time.Now().Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	if len(logs) != 1 || logs[0].PromptTokens != 9 || logs[0].CompletionTokens != 1 {
		t.Fatalf("audit usage = %+v, want prompt 9 / completion 1", logs)
	}
}
