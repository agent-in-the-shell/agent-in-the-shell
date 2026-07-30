package api_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/api"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/cost"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/stub"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

const testToken = "test-bearer-token"

func newTestServer(t *testing.T, deps map[string][]router.Deployment, opts ...func(*api.Config)) (*httptest.Server, *store.SQLiteStore) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	registry, err := cost.LoadDefault()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	r := router.New(deps, nil)
	cfg := api.Config{
		Router:      r,
		Store:       st,
		Registry:    registry,
		BearerToken: testToken,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	s := api.New(cfg)

	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, st
}

func mustPost(t *testing.T, ts *httptest.Server, path string, body any, token string) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), "POST", ts.URL+path, bytes.NewReader(b))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

// ─── /healthz ─────────────────────────────────────────────────────────────

func TestHealthz_Unauthenticated(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status: got %d, want 200", resp.StatusCode)
	}
}

func TestReadyz(t *testing.T) {
	ts, st := newTestServer(t, nil)

	resp, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ready status = %d, want 200", resp.StatusCode)
	}

	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	resp, err = http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz closed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("ready status after close = %d, want 503", resp.StatusCode)
	}
}

// ─── Auth ─────────────────────────────────────────────────────────────────

func TestChatCompletions_RequiresBearerToken(t *testing.T) {
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"gpt-4": {{Provider: &stub.Stub{NameValue: "openai"}, Model: "gpt-4o", Weight: 1}},
	})

	body := agentmodel.ChatRequest{
		Model:    "gpt-4",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	}

	t.Run("no header", func(t *testing.T) {
		resp := mustPost(t, ts, "/v1/chat/completions", body, "")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status: got %d, want 401", resp.StatusCode)
		}
	})
	t.Run("wrong token", func(t *testing.T) {
		resp := mustPost(t, ts, "/v1/chat/completions", body, "wrong")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status: got %d, want 401", resp.StatusCode)
		}
	})
}

// TestChatCompletions_XApiKey verifies that Anthropic SDK clients using the
// X-Api-Key header (instead of Authorization: Bearer) are accepted.
func TestChatCompletions_XApiKey(t *testing.T) {
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"gpt-4": {{Provider: &stub.Stub{NameValue: "openai"}, Model: "gpt-4o", Weight: 1}},
	})
	body := agentmodel.ChatRequest{
		Model:    "gpt-4",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(context.Background(), "POST", ts.URL+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("X-Api-Key", testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Errorf("X-Api-Key auth rejected: got 401")
	}
}

// ─── ChatCompletions: non-streaming success ──────────────────────────────

func TestChatCompletions_NonStreaming(t *testing.T) {
	ts, st := newTestServer(t, map[string][]router.Deployment{
		"gpt-4": {{Provider: &stub.Stub{NameValue: "openai"}, Model: "gpt-4o", Weight: 1}},
	})

	body := agentmodel.ChatRequest{
		Model:    "gpt-4",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	}
	resp := mustPost(t, ts, "/v1/chat/completions", body, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}

	var got agentmodel.ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Response.Model reflects the resolved upstream deployment model id,
	// not the caller's logical alias (matches OpenAI semantics).
	if got.Model != "gpt-4o" {
		t.Errorf("Model: got %q, want gpt-4o (upstream)", got.Model)
	}
	if len(got.Choices) == 0 {
		t.Errorf("no choices in response")
	}
	if got.Usage.TotalTokens == 0 {
		t.Errorf("expected non-zero token count")
	}
	if got.Usage.AuthMode != agentmodel.AuthModeAPIKey {
		t.Errorf("AuthMode: got %q, want api_key", got.Usage.AuthMode)
	}
	// Cost should be 0 for stub-model not in registry, OR positive if matching registry.
	// Either way, it should be set or zero, not negative.
	if got.Usage.CostUSD < 0 {
		t.Errorf("CostUSD: got %v, want >= 0", got.Usage.CostUSD)
	}

	// Audit log should have the request.
	logs, err := st.ListByOrg(context.Background(), "default", time.Now().Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	if len(logs) != 1 {
		t.Errorf("audit logs: got %d, want 1", len(logs))
	}
	if logs[0].Status != "ok" {
		t.Errorf("Status: got %q, want ok", logs[0].Status)
	}
	if logs[0].AuthMode != agentmodel.AuthModeAPIKey {
		t.Errorf("AuthMode in log: got %q, want api_key", logs[0].AuthMode)
	}
}

// TestChatCompletions_NonStreaming_BackfillsCreated asserts the handler stamps
// a non-zero created when the provider left it 0 (the Anthropic adapter does),
// keeping every provider's response OpenAI-shape-conformant (#664, fix A).
func TestChatCompletions_NonStreaming_BackfillsCreated(t *testing.T) {
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"gpt-4": {{Provider: &stub.Stub{
			NameValue: "openai",
			CompleteResp: agentmodel.ChatResponse{
				ID:      "chatcmpl-fixed",
				Object:  "chat.completion",
				Created: 0, // provider left it unset
				Model:   "gpt-4o",
				Choices: []agentmodel.Choice{{
					Index:        0,
					Message:      agentmodel.Message{Role: "assistant", Content: "hi"},
					FinishReason: "stop",
				}},
				Usage: agentmodel.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
			},
		}, Model: "gpt-4o", Weight: 1}},
	})

	body := agentmodel.ChatRequest{
		Model:    "gpt-4",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	}
	resp := mustPost(t, ts, "/v1/chat/completions", body, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}

	var got agentmodel.ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Created <= 0 {
		t.Errorf("Created: got %d, want > 0 (handler must backfill)", got.Created)
	}
	if got.ID != "chatcmpl-fixed" {
		t.Errorf("ID should be preserved, got %q", got.ID)
	}
}

// ─── ChatCompletions: subscription cost = 0 ──────────────────────────────

func TestChatCompletions_SubscriptionModelHasZeroCost(t *testing.T) {
	subStub := &stub.Stub{
		NameValue:     "chatgpt",
		AuthModeValue: agentmodel.AuthModeSubscription,
	}
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"chatgpt/gpt-5": {{Provider: subStub, Model: "gpt-5", Weight: 1}},
	})

	body := agentmodel.ChatRequest{
		Model:    "chatgpt/gpt-5",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	}
	resp := mustPost(t, ts, "/v1/chat/completions", body, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}

	var got agentmodel.ChatResponse
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got.Usage.CostUSD != 0 {
		t.Errorf("CostUSD: got %v, want 0 for subscription", got.Usage.CostUSD)
	}
	if got.Usage.AuthMode != agentmodel.AuthModeSubscription {
		t.Errorf("AuthMode: got %q, want subscription", got.Usage.AuthMode)
	}
}

// ─── ChatCompletions: streaming ──────────────────────────────────────────

func TestChatCompletions_Streaming(t *testing.T) {
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"gpt-4": {{Provider: &stub.Stub{}, Model: "x", Weight: 1}},
	})

	body := agentmodel.ChatRequest{
		Model:    "gpt-4",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
		Stream:   true,
	}
	resp := mustPost(t, ts, "/v1/chat/completions", body, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("Content-Type: got %q, want text/event-stream", got)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	chunkCount := 0
	doneSeen := false
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			doneSeen = true
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err == nil {
			chunkCount++
			if chunk["object"] != "chat.completion.chunk" {
				t.Errorf("object: got %v, want chat.completion.chunk", chunk["object"])
			}
		}
	}
	if chunkCount == 0 {
		t.Errorf("no chunks received")
	}
	if !doneSeen {
		t.Errorf("[DONE] terminator missing")
	}
}

// TestChatCompletions_Streaming_SynthesizesFinishReason verifies the handler
// guarantees a terminal finish_reason even when the provider ends the stream
// without one. A finish_reason-less stream makes strict OpenAI clients fail
// with "Stream ended without finish_reason"; the safety net synthesizes a
// trailing stop chunk so that never happens.
func TestChatCompletions_Streaming_SynthesizesFinishReason(t *testing.T) {
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"gpt-4": {{Provider: &stub.Stub{StreamChunks: []provider.StreamChunk{
			{Delta: agentmodel.Message{Role: "assistant", Content: "hi"}},
			// No finish_reason chunk — mimics an upstream drop the provider
			// failed to surface.
		}}, Model: "x", Weight: 1}},
	})

	body := agentmodel.ChatRequest{
		Model:    "gpt-4",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
		Stream:   true,
	}
	resp := mustPost(t, ts, "/v1/chat/completions", body, testToken)
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var sawFinish bool
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Role *string `json:"role"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("unmarshal chunk: %v", err)
		}
		for _, c := range chunk.Choices {
			if c.Delta.Role != nil && *c.Delta.Role == "" {
				t.Error("stream chunk emitted an empty role; role must be omitted or a valid message role")
			}
			if c.FinishReason != nil && *c.FinishReason == "stop" {
				sawFinish = true
			}
		}
	}
	if !sawFinish {
		t.Error("handler did not synthesize a terminal finish_reason=stop chunk")
	}
}

// TestChatCompletions_Streaming_SynthesizedFinishReasonToolAware verifies that
// when a stream carries tool-call deltas but ends without a finish_reason, the
// synthesized terminal reason is "tool_calls", not "stop". Defaulting to "stop"
// mid tool call makes agents drop the pending call (cf. LiteLLM #19744/#12862).
func TestChatCompletions_Streaming_SynthesizedFinishReasonToolAware(t *testing.T) {
	idx := 0
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"gpt-4": {{Provider: &stub.Stub{StreamChunks: []provider.StreamChunk{
			{Delta: agentmodel.Message{Role: "assistant", ToolCalls: []agentmodel.ToolCall{{
				Index:    &idx,
				ID:       "call_1",
				Type:     "function",
				Function: agentmodel.ToolCallFunction{Name: "get_weather", Arguments: "{}"},
			}}}},
			// No finish_reason chunk — drop after a tool-call delta.
		}}, Model: "x", Weight: 1}},
	})

	resp := mustPost(t, ts, "/v1/chat/completions", agentmodel.ChatRequest{
		Model:    "gpt-4",
		Messages: []agentmodel.Message{{Role: "user", Content: "weather?"}},
		Stream:   true,
	}, testToken)
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var finish string
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("unmarshal chunk: %v", err)
		}
		for _, c := range chunk.Choices {
			if c.FinishReason != nil {
				finish = *c.FinishReason
			}
		}
	}
	if finish != "tool_calls" {
		t.Errorf("synthesized finish_reason = %q, want tool_calls", finish)
	}
}

// ─── ChatCompletions: cost source (#511) ─────────────────────────────────

// TestChatCompletions_RecordsCostSource verifies the audit log distinguishes a
// genuine $0 (subscription) from a price we don't have (unpriced) from a real
// computed price (priced). resp.Model resolves to the deployment Model, and the
// registry is exact-match keyed (e.g. "openai/gpt-4o"), so the deployment Model
// deterministically selects which source path is exercised.
func TestChatCompletions_RecordsCostSource(t *testing.T) {
	cases := []struct {
		name       string
		depModel   string // becomes resp.Model
		wantSource string
	}{
		{"priced", "openai/gpt-4o", "priced"},              // exact registry key, api_key model
		{"priced via provider prefix", "gpt-4o", "priced"}, // bare id prices via the provider's "openai/gpt-4o" key
		{"subscription", "chatgpt/gpt-5", "subscription"},  // exact registry key, subscription model
		{"unpriced", "no-such-model-xyz", "unpriced"},      // absent under both the prefixed and bare key
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, st := newTestServer(t, map[string][]router.Deployment{
				"m": {{Provider: &stub.Stub{NameValue: "openai"}, Model: tc.depModel, Weight: 1}},
			})
			resp := mustPost(t, ts, "/v1/chat/completions", agentmodel.ChatRequest{
				Model:    "m",
				Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
			}, testToken)
			resp.Body.Close()

			logs, err := st.ListByOrg(context.Background(), "default", time.Now().Add(-time.Minute), 10)
			if err != nil {
				t.Fatalf("ListByOrg: %v", err)
			}
			if len(logs) != 1 {
				t.Fatalf("audit logs: got %d, want 1", len(logs))
			}
			if logs[0].CostSource != tc.wantSource {
				t.Errorf("CostSource: got %q, want %q", logs[0].CostSource, tc.wantSource)
			}
		})
	}
}

// TestChatCompletions_UnpricedModelEmitsWarning verifies an unknown model both
// warns (so the miss is visible) and persists CostSource="unpriced" (so spend
// reports don't silently treat it as a real $0).
func TestChatCompletions_UnpricedModelEmitsWarning(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	registry, _ := cost.LoadDefault()
	r := router.New(map[string][]router.Deployment{
		// A model absent from the registry under BOTH "<provider>/<model>" and the
		// bare id, so it genuinely stays unpriced (bare "gpt-4o" would now price via
		// the "openai/gpt-4o" key — see TestChatCompletions_RecordsCostSource).
		"gpt-4": {{Provider: &stub.Stub{NameValue: "openai"}, Model: "no-such-model-xyz", Weight: 1}},
	}, nil)
	s := api.New(api.Config{Router: r, Store: st, Registry: registry, BearerToken: testToken, Logger: logger})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	resp := mustPost(t, ts, "/v1/chat/completions", agentmodel.ChatRequest{
		Model:    "gpt-4",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	}, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}

	if logged := buf.String(); !strings.Contains(logged, "missing from price registry") {
		t.Errorf("expected unpriced-model warning, got: %q", logged)
	}
	logs, err := st.ListByOrg(context.Background(), "default", time.Now().Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	if len(logs) != 1 || logs[0].CostSource != "unpriced" {
		t.Errorf("want one row with CostSource=unpriced, got %+v", logs)
	}
}

// ─── ChatCompletions: error mapping ──────────────────────────────────────

func TestChatCompletions_RateLimitMappedTo429(t *testing.T) {
	failer := &stub.Stub{CompleteErr: errors.New("429 rate limit exceeded")}
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"gpt-4": {{Provider: failer, Model: "x", Weight: 1}},
	})

	resp := mustPost(t, ts, "/v1/chat/completions", agentmodel.ChatRequest{
		Model:    "gpt-4",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	}, testToken)
	defer resp.Body.Close()

	// All deployments failed with retryable error → ErrAllFailed which is 500.
	// But underlying type is upstream-class, not 429 specifically. Verify the
	// error envelope is OpenAI-shaped.
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if _, ok := body["error"]; !ok {
		t.Errorf("body missing 'error' key: %v", body)
	}
}

func TestChatCompletions_AuthErrorReturns401(t *testing.T) {
	failer := &stub.Stub{CompleteErr: errors.New("401 invalid_api_key")}
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"gpt-4": {{Provider: failer, Model: "x", Weight: 1}},
	})

	resp := mustPost(t, ts, "/v1/chat/completions", agentmodel.ChatRequest{
		Model:    "gpt-4",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	}, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", resp.StatusCode)
	}
}

// ─── ChatCompletions: validation ─────────────────────────────────────────

func TestChatCompletions_RequiresModel(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp := mustPost(t, ts, "/v1/chat/completions", map[string]any{
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
}

func TestChatCompletions_RequiresMessages(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp := mustPost(t, ts, "/v1/chat/completions", map[string]any{"model": "gpt-4"}, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
}

// ─── Error envelope wire shape (#705) ────────────────────────────────────

// TestErrorEnvelope_WireShape pins the OpenAI-shaped error body produced by
// writeError: a non-empty code/param appears with its documented value, and an
// empty one is OMITTED entirely (never serialized as "") so the response
// matches OpenAI's absent/null contract. Decoding into map[string]any is
// deliberate — a struct field cannot distinguish `"code":""` from an absent
// key, which is exactly the regression being pinned.
func TestErrorEnvelope_WireShape(t *testing.T) {
	ts, _ := newTestServer(t, nil) // empty router → an unknown model has no deployment

	cases := []struct {
		name   string
		token  string
		raw    string // raw request body; when "" the body field is marshalled
		body   any
		status int
		typ    string
		code   string // "" means the key must be ABSENT
		param  string // "" means the key must be ABSENT
	}{
		{
			name:   "no bearer token",
			token:  "",
			body:   agentmodel.ChatRequest{Model: "gpt-4", Messages: []agentmodel.Message{{Role: "user", Content: "hi"}}},
			status: http.StatusUnauthorized,
			typ:    "authentication_error",
			code:   "invalid_api_key",
		},
		{
			name:   "model with no deployment",
			token:  testToken,
			body:   agentmodel.ChatRequest{Model: "nope", Messages: []agentmodel.Message{{Role: "user", Content: "hi"}}},
			status: http.StatusNotFound,
			typ:    "not_found_error",
			code:   "model_not_found",
		},
		{
			name:   "empty messages",
			token:  testToken,
			body:   map[string]any{"model": "gpt-4", "messages": []any{}},
			status: http.StatusBadRequest,
			typ:    "invalid_request_error",
			code:   "empty_array",
			param:  "messages",
		},
		{
			name:   "invalid json",
			token:  testToken,
			raw:    "{",
			status: http.StatusBadRequest,
			typ:    "invalid_request_error",
			code:   "invalid_json",
		},
		{
			name:   "missing model",
			token:  testToken,
			body:   map[string]any{"messages": []map[string]string{{"role": "user", "content": "hi"}}},
			status: http.StatusBadRequest,
			typ:    "invalid_request_error",
			code:   "missing_required_parameter",
			param:  "model",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reader io.Reader
			if tc.raw != "" {
				reader = strings.NewReader(tc.raw)
			} else {
				b, err := json.Marshal(tc.body)
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				reader = bytes.NewReader(b)
			}
			req, err := http.NewRequestWithContext(context.Background(), "POST", ts.URL+"/v1/chat/completions", reader)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.status {
				t.Errorf("status: got %d, want %d", resp.StatusCode, tc.status)
			}

			var out struct {
				Error map[string]any `json:"error"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			if out.Error == nil {
				t.Fatalf("response has no error object")
			}
			if got := out.Error["type"]; got != tc.typ {
				t.Errorf("type: got %v, want %q", got, tc.typ)
			}
			if got, ok := out.Error["message"].(string); !ok || got == "" {
				t.Errorf("message must always be present and non-empty, got %v", out.Error["message"])
			}
			assertErrorKey(t, out.Error, "code", tc.code)
			assertErrorKey(t, out.Error, "param", tc.param)
		})
	}
}

// assertErrorKey checks that field is present with want when want != "", or
// ABSENT (never an empty string) when want == "" — the #705 omitempty contract.
func assertErrorKey(t *testing.T, obj map[string]any, field, want string) {
	t.Helper()
	got, present := obj[field]
	if want == "" {
		if present {
			t.Errorf("%s: want key ABSENT (omitempty), got %v", field, got)
		}
		return
	}
	if !present {
		t.Errorf("%s: want %q, got absent", field, want)
		return
	}
	if got != want {
		t.Errorf("%s: got %v, want %q", field, got, want)
	}
}

// TestChatCompletions_Streaming_MidStreamErrorFrame verifies a mid-stream
// failure is surfaced as a `data: {"error":...}` SSE frame whose error object
// carries the Wrap-classified type and code (#705) — the frame previously
// hardcoded type=upstream_error with no code. When the error is unclassifiable
// the code key is omitted rather than emitted as "".
func TestChatCompletions_Streaming_MidStreamErrorFrame(t *testing.T) {
	cases := []struct {
		name     string
		yieldErr error
		wantType string
		wantCode string // "" → code key must be absent
	}{
		{"classified rate limit", errors.New("429 rate limit exceeded"), "rate_limit_error", "rate_limit_exceeded"},
		{"unclassified", errors.New("something unexpected went sideways"), "upstream_error", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, _ := newTestServer(t, map[string][]router.Deployment{
				"gpt-4": {{Provider: &stub.Stub{
					StreamChunks:   []provider.StreamChunk{{Delta: agentmodel.Message{Role: "assistant", Content: "hi"}}},
					StreamYieldErr: tc.yieldErr,
				}, Model: "x", Weight: 1}},
			})
			resp := mustPost(t, ts, "/v1/chat/completions", agentmodel.ChatRequest{
				Model:    "gpt-4",
				Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
				Stream:   true,
			}, testToken)
			defer resp.Body.Close()

			scanner := bufio.NewScanner(resp.Body)
			scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			var errObj map[string]any
			for scanner.Scan() {
				line := scanner.Text()
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				payload := strings.TrimPrefix(line, "data: ")
				if payload == "[DONE]" {
					continue
				}
				var frame struct {
					Error map[string]any `json:"error"`
				}
				if err := json.Unmarshal([]byte(payload), &frame); err != nil {
					continue
				}
				if frame.Error != nil {
					errObj = frame.Error
				}
			}
			if errObj == nil {
				t.Fatal("no mid-stream error frame found")
			}
			if got := errObj["type"]; got != tc.wantType {
				t.Errorf("type: got %v, want %q", got, tc.wantType)
			}
			assertErrorKey(t, errObj, "code", tc.wantCode)
		})
	}
}

// ─── /v1/embeddings ──────────────────────────────────────────────────────

func TestEmbeddings_HappyPath(t *testing.T) {
	embStub := &stub.Stub{
		NameValue: "openai",
		EmbedResp: agentmodel.EmbeddingResponse{
			Object: "list",
			Data: []agentmodel.Embedding{
				{Object: "embedding", Index: 0, Embedding: []float64{0.1, 0.2}},
			},
			Usage: agentmodel.Usage{PromptTokens: 5, TotalTokens: 5},
		},
	}
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"text-embedding-3-small": {{Provider: embStub, Model: "text-embedding-3-small", Weight: 1}},
	})

	resp := mustPost(t, ts, "/v1/embeddings", agentmodel.EmbeddingRequest{
		Model: "text-embedding-3-small",
		Input: []string{"hello"},
	}, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	var got agentmodel.EmbeddingResponse
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if len(got.Data) == 0 {
		t.Errorf("no embeddings returned")
	}
}

func TestEmbeddings_ValidationAndProviderError(t *testing.T) {
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"text-embedding-3-small": {{
			Provider: &stub.Stub{NameValue: "openai", EmbedErr: errors.New("503 overloaded")},
			Model:    "text-embedding-3-small",
			Weight:   1,
		}},
	})

	tests := []struct {
		name string
		body string
	}{
		{"invalid json", "{"},
		{"missing model", `{"input":["hello"]}`},
		{"empty input", `{"model":"text-embedding-3-small","input":[]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/v1/embeddings", strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			req.Header.Set("Authorization", "Bearer "+testToken)
			req.Header.Set("Content-Type", "application/json")
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
		})
	}

	resp := mustPost(t, ts, "/v1/embeddings", agentmodel.EmbeddingRequest{
		Model: "text-embedding-3-small",
		Input: []string{"hello"},
	}, testToken)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("provider error status = 200, want error")
	}
}

// ─── /v1/models ──────────────────────────────────────────────────────────

func TestListModels(t *testing.T) {
	// /v1/models lists the configured logical model_names (the routable
	// surface), not the cost price catalog — so a model configured under a
	// custom alias appears, and catalog-only entries do not.
	deps := map[string][]router.Deployment{
		"my-gpt":    {{Provider: &stub.Stub{NameValue: "openai"}, Model: "gpt-4o", Weight: 100}},
		"my-claude": {{Provider: &stub.Stub{NameValue: "anthropic"}, Model: "claude-3-5-sonnet-latest", Weight: 100}},
	}
	ts, _ := newTestServer(t, deps)
	req, _ := http.NewRequest("GET", ts.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /v1/models: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status: got %d, want 200", resp.StatusCode)
	}
	var got struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Object != "list" {
		t.Errorf("Object: got %q, want list", got.Object)
	}
	owners := map[string]string{}
	for _, m := range got.Data {
		owners[m.ID] = m.OwnedBy
		if m.Object != "model" {
			t.Errorf("%s: object got %q, want model", m.ID, m.Object)
		}
	}
	// Configured aliases must be listed, owned_by the primary deployment's provider.
	if owners["my-gpt"] != "openai" {
		t.Errorf("my-gpt owned_by: got %q, want openai (owners=%v)", owners["my-gpt"], owners)
	}
	if owners["my-claude"] != "anthropic" {
		t.Errorf("my-claude owned_by: got %q, want anthropic (owners=%v)", owners["my-claude"], owners)
	}
	// The list reflects config, not the embedded price catalog: catalog-only
	// entries (e.g. "openai/gpt-4o") must not leak through.
	if _, leaked := owners["openai/gpt-4o"]; leaked {
		t.Errorf("price-catalog entry leaked into /v1/models: %v", owners)
	}
}

func TestListModels_Available(t *testing.T) {
	// openai offers an unconfigured upstream model ("gpt-5-preview") alongside
	// the configured one ("gpt-4o", wired under the logical name "my-gpt").
	openaiStub := &stub.Stub{NameValue: "openai", LiveModels: []string{"gpt-4o", "gpt-5-preview"}}
	deps := map[string][]router.Deployment{
		"my-gpt": {{Provider: openaiStub, Model: "gpt-4o", Weight: 100}},
	}
	ts, _ := newTestServer(t, deps)

	type modelRow struct {
		ID         string `json:"id"`
		OwnedBy    string `json:"owned_by"`
		Configured *bool  `json:"configured"`
	}
	get := func(path string) []modelRow {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s: status %d, want 200", path, resp.StatusCode)
		}
		var got struct {
			Data []modelRow `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		return got.Data
	}

	// Default response: only the routable logical name, no "configured" field,
	// and the unconfigured upstream model must NOT leak in.
	for _, m := range get("/v1/models") {
		if m.ID == "gpt-5-preview" {
			t.Errorf("default /v1/models leaked unconfigured upstream model: %+v", m)
		}
		if m.Configured != nil {
			t.Errorf("default /v1/models must omit the configured flag, got %+v", m)
		}
	}

	// ?available: routable model tagged configured=true, the unconfigured
	// upstream model present and tagged configured=false.
	rows := get("/v1/models?available=1")
	var sawConfigured, sawDiscovered bool
	for _, m := range rows {
		switch m.ID {
		case "my-gpt":
			if m.Configured == nil || !*m.Configured {
				t.Errorf("my-gpt: want configured=true, got %+v", m)
			}
			sawConfigured = true
		case "gpt-5-preview":
			if m.Configured == nil || *m.Configured {
				t.Errorf("gpt-5-preview: want configured=false, got %+v", m)
			}
			if m.OwnedBy != "openai" {
				t.Errorf("gpt-5-preview: owned_by=%q, want openai", m.OwnedBy)
			}
			sawDiscovered = true
		}
	}
	if !sawConfigured {
		t.Errorf("?available missing configured model my-gpt: %+v", rows)
	}
	if !sawDiscovered {
		t.Errorf("?available missing discovered model gpt-5-preview: %+v", rows)
	}
}

// ─── OAuth endpoints (no chatgpt auth configured) ────────────────────────

func TestChatGPTOAuthStart_ReturnsNotFoundWhenUnconfigured(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp := mustPost(t, ts, "/v1/oauth/chatgpt/start", map[string]any{}, testToken)
	defer resp.Body.Close()
	// 404 to match the not_found_error type — it previously sent 501 (whose
	// not_found type mapped to 404, a status/type contradiction, #1494).
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", resp.StatusCode)
	}
}

func newChatGPTOAuthServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	authServer := httptest.NewServer(handler)
	t.Cleanup(authServer.Close)

	chatAuth := auth.NewChatGPTOAuth(filepath.Join(t.TempDir(), "chatgpt"), authServer.Client())
	chatAuth.OverrideURLs(authServer.URL+"/devicecode", authServer.URL+"/devicetoken", authServer.URL+"/oauth")

	ts, _ := newTestServer(t, nil, func(c *api.Config) {
		c.ChatGPTAuth = chatAuth
	})
	return ts
}

func TestChatGPTOAuthStart_Configured(t *testing.T) {
	ts := newChatGPTOAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/devicecode" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q, want application/json", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_auth_id":   "auth-123",
			"user_code":        "CODE-123",
			"verification_uri": "https://example.test/device",
			"expires_in":       900,
			"interval":         1,
		})
	})

	resp := mustPost(t, ts, "/v1/oauth/chatgpt/start", map[string]any{}, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["device_code"] != "auth-123" || got["device_auth_id"] != "auth-123" || got["user_code"] != "CODE-123" {
		t.Fatalf("oauth start response = %#v", got)
	}
}

func TestChatGPTOAuthStart_UpstreamFailure(t *testing.T) {
	ts := newChatGPTOAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/devicecode" {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "no device code", http.StatusBadGateway)
	})

	resp := mustPost(t, ts, "/v1/oauth/chatgpt/start", map[string]any{}, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: got %d, want 502", resp.StatusCode)
	}
}

func TestChatGPTOAuthPoll_ValidationErrors(t *testing.T) {
	ts := newChatGPTOAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	req, err := http.NewRequestWithContext(context.Background(), "POST", ts.URL+"/v1/oauth/chatgpt/poll", strings.NewReader("{"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do malformed poll: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed status: got %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()

	tests := []struct {
		name string
		body map[string]any
	}{
		{"missing device_code", map[string]any{"device_auth_id": "auth-123", "user_code": "CODE-123"}},
		{"missing device_auth_id", map[string]any{"device_code": "device-123", "user_code": "CODE-123"}},
		{"missing user_code", map[string]any{"device_code": "device-123", "device_auth_id": "auth-123"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := mustPost(t, ts, "/v1/oauth/chatgpt/poll", tt.body, testToken)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status: got %d, want 400", resp.StatusCode)
			}
		})
	}
}

func TestChatGPTOAuthPoll_Configured(t *testing.T) {
	var sawPoll bool
	ts := newChatGPTOAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/devicetoken" {
			http.NotFound(w, r)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode poll body: %v", err)
		}
		sawPoll = body["device_auth_id"] == "auth-123" && body["user_code"] == "CODE-123"
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-token",
			"refresh_token": "refresh-token",
			"expires_in":    3600,
		})
	})

	resp := mustPost(t, ts, "/v1/oauth/chatgpt/poll", map[string]any{
		"device_code":    "device-123",
		"device_auth_id": "auth-123",
		"user_code":      "CODE-123",
	}, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if !sawPoll {
		t.Fatal("device token endpoint did not receive expected poll fields")
	}
	var got map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["status"] != "complete" {
		t.Fatalf("status body = %#v, want complete", got)
	}
}

func TestChatGPTOAuthPoll_UpstreamFailure(t *testing.T) {
	ts := newChatGPTOAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/devicetoken" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"access_denied"}`))
	})

	resp := mustPost(t, ts, "/v1/oauth/chatgpt/poll", map[string]any{
		"device_code":    "device-123",
		"device_auth_id": "auth-123",
		"user_code":      "CODE-123",
	}, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status: got %d, want 502", resp.StatusCode)
	}
}

// TestChatCompletions_FailureRowNamesDeployment guards #1492: a terminal
// upstream failure on /v1/chat/completions must audit with the FAILING
// deployment's provider/auth_mode and the upstream model id — matching the
// /v1/messages path. Before the fix the router discarded the deployment on a
// terminal error and this row was written unattributed (provider="").
func TestChatCompletions_FailureRowNamesDeployment(t *testing.T) {
	ts, st := newTestServer(t, map[string][]router.Deployment{
		"gpt-4": {{
			Name: "openai/gpt-4o|api_key",
			// "401 unauthorized" classifies as authentication_error, which is
			// TERMINAL. (Do not use stub.ErrTestAuth: "stub: auth failed" misses
			// Wrap's substrings and is classified retryable upstream_error, which
			// would exhaust the walk and legitimately produce an unattributed row.)
			Provider: &stub.Stub{
				NameValue:     "openai",
				AuthModeValue: agentmodel.AuthModeAPIKey,
				CompleteErr:   errors.New("401 unauthorized"),
			},
			Model:  "gpt-4o",
			Weight: 1,
		}},
	})

	body, _ := json.Marshal(map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	req, _ := http.NewRequest("POST", ts.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("expected a failure status, got 200")
	}

	logs, err := st.ListByOrg(context.Background(), "default", time.Now().Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("audit logs = %d, want 1", len(logs))
	}
	got := logs[0]
	if got.Status != "error" {
		t.Errorf("Status = %q, want error", got.Status)
	}
	if got.Provider != "openai" {
		t.Errorf("Provider = %q, want openai (a failed chat request must name its deployment)", got.Provider)
	}
	if got.AuthMode != agentmodel.AuthModeAPIKey {
		t.Errorf("AuthMode = %q, want api_key", got.AuthMode)
	}
	if got.ModelUsed != "gpt-4o" {
		t.Errorf("ModelUsed = %q, want gpt-4o (upstream id, matching the messages path)", got.ModelUsed)
	}
}
