package api_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/api"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/cost"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/anthropic"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/stub"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

// newMessagesTestServer wires an agentmodel server with a single anthropic
// deployment for `modelName` whose upstream Anthropic endpoint is replaced
// with `upstream` (a fake httptest server). Returns the agentmodel server
// and the fake-upstream handle so tests can assert on the proxied bytes.
func newMessagesTestServer(t *testing.T, modelName, upstreamModel string, upstream *httptest.Server) (*httptest.Server, *store.SQLiteStore) {
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

	authn := &auth.StaticKey{HeaderName: "x-api-key", Token: "sk-ant-api03-test"}
	client := anthropic.NewWithBaseURL(authn, upstream.URL)

	deps := map[string][]router.Deployment{
		modelName: {{Provider: client, Model: upstreamModel, Weight: 1}},
	}
	r := router.New(deps, nil)
	s := api.New(api.Config{
		Router:      r,
		Store:       st,
		Registry:    registry,
		BearerToken: testToken,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, st
}

func TestMessages_NonStreamingPassthrough(t *testing.T) {
	respBody := `{
		"id": "msg_123",
		"type": "message",
		"role": "assistant",
		"model": "claude-3-5-sonnet-latest",
		"content": [{"type":"text","text":"Hello!"}],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 12, "output_tokens": 5}
	}`

	var gotPath string
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
	}))
	defer upstream.Close()

	ts, st := newMessagesTestServer(t, "claude-sonnet-4-5", "claude-3-5-sonnet-latest", upstream)

	req := map[string]any{
		"model":      "claude-sonnet-4-5",
		"max_tokens": 100,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	}
	resp := mustPost(t, ts, "/v1/messages", req, testToken)
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"id": "msg_123"`) {
		t.Errorf("response did not include upstream id: %s", body)
	}

	if gotPath != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages", gotPath)
	}

	// Body should have model rewritten to deployment's upstream id.
	var sent map[string]any
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("decode upstream body: %v", err)
	}
	if got, want := sent["model"], "claude-3-5-sonnet-latest"; got != want {
		t.Errorf("upstream model = %v, want %q", got, want)
	}

	// Audit row recorded with cost calculation.
	logs, err := st.ListByOrg(context.Background(), "default", time.Now().Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(logs))
	}
	got := logs[0]
	if got.ModelRequested != "claude-sonnet-4-5" {
		t.Errorf("model_requested = %q", got.ModelRequested)
	}
	if got.ModelUsed != "claude-3-5-sonnet-latest" {
		t.Errorf("model_used = %q", got.ModelUsed)
	}
	if got.PromptTokens != 12 || got.CompletionTokens != 5 || got.TotalTokens != 17 {
		t.Errorf("tokens = (%d, %d, %d), want (12, 5, 17)", got.PromptTokens, got.CompletionTokens, got.TotalTokens)
	}
	if got.Status != "ok" {
		t.Errorf("status = %q, want ok", got.Status)
	}
}

// TestMessages_RecordsProvider pins the audit row's provider column for both
// /v1/messages paths.  wired usage.Provider through the chat handlers so
// cost lookup could try the catalog's canonical "<provider>/<model>" key, but
// the messages handlers were missed: every row landed with provider=” and the
// prefixed lookup silently fell back to the bare model id. That made every
// subscription model on this path log as "unpriced".
func TestMessages_RecordsProvider(t *testing.T) {
	nonStreamBody := `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":3,"output_tokens":2}}`
	streamBody := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","role":"assistant","usage":{"input_tokens":3,"output_tokens":0}}}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
		"",
		"",
	}, "\n")

	for _, stream := range []bool{false, true} {
		name := "non-streaming"
		if stream {
			name = "streaming"
		}
		t.Run(name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(200)
					_, _ = io.WriteString(w, streamBody)
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, nonStreamBody)
			}))
			defer upstream.Close()

			ts, st := newMessagesTestServer(t, "claude-sonnet-4-5", "claude-3-5-sonnet-latest", upstream)

			req := map[string]any{
				"model": "claude-sonnet-4-5", "max_tokens": 100,
				"messages": []map[string]any{{"role": "user", "content": "hi"}},
			}
			if stream {
				req["stream"] = true
			}
			body, _ := json.Marshal(req)
			httpReq, _ := http.NewRequestWithContext(context.Background(), "POST", ts.URL+"/v1/messages", bytes.NewReader(body))
			httpReq.Header.Set("Authorization", "Bearer "+testToken)
			httpReq.Header.Set("Content-Type", "application/json")
			resp, err := ts.Client().Do(httpReq)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)

			logs, err := st.ListByOrg(context.Background(), "default", time.Now().Add(-time.Minute), 10)
			if err != nil {
				t.Fatalf("recent: %v", err)
			}
			if len(logs) != 1 {
				t.Fatalf("logs = %d, want 1", len(logs))
			}
			if got := logs[0].Provider; got != "anthropic" {
				t.Errorf("provider = %q, want %q (the deployment's provider name)", got, "anthropic")
			}
		})
	}
}

func TestMessages_StreamingPassthrough(t *testing.T) {
	// Minimal Anthropic SSE stream covering the events that contribute to
	// usage: message_start (input_tokens), message_delta (output_tokens).
	sseBody := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","role":"assistant","usage":{"input_tokens":42,"output_tokens":1,"cache_read_input_tokens":3}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
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

	ts, st := newMessagesTestServer(t, "claude-sonnet-4-5", "claude-3-5-sonnet-latest", upstream)

	req := map[string]any{
		"model":      "claude-sonnet-4-5",
		"max_tokens": 100,
		"stream":     true,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	}
	body, _ := json.Marshal(req)
	httpReq, _ := http.NewRequestWithContext(context.Background(), "POST", ts.URL+"/v1/messages", bytes.NewReader(body))
	httpReq.Header.Set("Authorization", "Bearer "+testToken)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(httpReq)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("content-type = %q, want text/event-stream*", got)
	}

	// Read the full SSE body and confirm it contains both upstream events
	// verbatim (proves byte-for-byte passthrough, not OpenAI-reformatted).
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var got bytes.Buffer
	for scanner.Scan() {
		got.WriteString(scanner.Text())
		got.WriteString("\n")
	}
	if !strings.Contains(got.String(), "event: message_start") {
		t.Errorf("missing message_start in stream; got:\n%s", got.String())
	}
	if !strings.Contains(got.String(), "event: message_delta") {
		t.Errorf("missing message_delta in stream; got:\n%s", got.String())
	}

	// After stream finishes, audit row should reflect parsed usage.
	logs, err := st.ListByOrg(context.Background(), "default", time.Now().Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(logs))
	}
	if logs[0].PromptTokens != 45 { // input(42) + cache_read(3)
		t.Errorf("prompt_tokens = %d, want 45", logs[0].PromptTokens)
	}
	if logs[0].CompletionTokens != 7 {
		t.Errorf("completion_tokens = %d, want 7", logs[0].CompletionTokens)
	}
	if logs[0].CacheReadInputTokens != 3 {
		t.Errorf("cache_read = %d, want 3", logs[0].CacheReadInputTokens)
	}
}

// TestMessages_StreamingErrorFrame_RecordsStatusError pins the audit row for a
// stream that dies mid-flight. streamMessages used to log statusOk
// unconditionally once the upstream body was drained, so a terminal `event:
// error` frame was recorded as a *success* with zero tokens — dashboards and
// health checks then reported 100% success while callers were seeing errors.
func TestMessages_StreamingErrorFrame_RecordsStatusError(t *testing.T) {
	cases := []struct {
		desc         string
		errFrame     string
		wantErrType  string
		wantPromptTk int
	}{
		{
			desc:         "context window exceeded after message_start",
			errFrame:     `data: {"type":"error","error":{"type":"context_window_exceeded","code":"context_length_exceeded","message":"input exceeds the context window"}}`,
			wantErrType:  "context_window_exceeded",
			wantPromptTk: 42,
		},
		{
			desc:         "upstream overloaded mid-stream",
			errFrame:     `data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			wantErrType:  "overloaded_error",
			wantPromptTk: 42,
		},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			sseBody := strings.Join([]string{
				"event: message_start",
				`data: {"type":"message_start","message":{"id":"msg_1","role":"assistant","usage":{"input_tokens":42,"output_tokens":0}}}`,
				"",
				"event: error",
				c.errFrame,
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

			ts, st := newMessagesTestServer(t, "claude-sonnet-4-5", "claude-3-5-sonnet-latest", upstream)

			req := map[string]any{
				"model":      "claude-sonnet-4-5",
				"max_tokens": 100,
				"stream":     true,
				"messages":   []map[string]any{{"role": "user", "content": "hi"}},
			}
			body, _ := json.Marshal(req)
			httpReq, _ := http.NewRequestWithContext(context.Background(), "POST", ts.URL+"/v1/messages", bytes.NewReader(body))
			httpReq.Header.Set("Authorization", "Bearer "+testToken)
			httpReq.Header.Set("Content-Type", "application/json")
			resp, err := ts.Client().Do(httpReq)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			defer resp.Body.Close()

			// The error frame still reaches the client byte-for-byte.
			raw, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(raw), "event: error") {
				t.Fatalf("error frame not forwarded to client; got:\n%s", raw)
			}

			logs, err := st.ListByOrg(context.Background(), "default", time.Now().Add(-time.Minute), 10)
			if err != nil {
				t.Fatalf("recent: %v", err)
			}
			if len(logs) != 1 {
				t.Fatalf("logs = %d, want 1", len(logs))
			}
			got := logs[0]
			if got.Status != "error" {
				t.Errorf("status = %q, want error (a failed stream must not be audited as a success)", got.Status)
			}
			if got.ErrorType != c.wantErrType {
				t.Errorf("error_type = %q, want %q", got.ErrorType, c.wantErrType)
			}
			// Usage seen before the failure is still worth recording — the
			// upstream billed for the prompt it read.
			if got.PromptTokens != c.wantPromptTk {
				t.Errorf("prompt_tokens = %d, want %d", got.PromptTokens, c.wantPromptTk)
			}
		})
	}
}

// TestMessages_StreamingPassthrough_NoErrorFrameStaysOk guards the fix above
// from over-firing: a normal stream must still audit as a success.
func TestMessages_StreamingPassthrough_NoErrorFrameStaysOk(t *testing.T) {
	sseBody := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","role":"assistant","usage":{"input_tokens":5,"output_tokens":0}}}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
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
		// Flush so the reply is chunked, as a real SSE stream is. Without it Go
		// buffers this short body and sets Content-Length, which the gateway
		// forwards verbatim — the client then sees EOF after N bytes, racing the
		// handler's audit-row write.
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()

	ts, st := newMessagesTestServer(t, "claude-sonnet-4-5", "claude-3-5-sonnet-latest", upstream)

	req := map[string]any{
		"model": "claude-sonnet-4-5", "max_tokens": 100, "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}
	body, _ := json.Marshal(req)
	httpReq, _ := http.NewRequestWithContext(context.Background(), "POST", ts.URL+"/v1/messages", bytes.NewReader(body))
	httpReq.Header.Set("Authorization", "Bearer "+testToken)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(httpReq)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	_, _ = io.Copy(io.Discard, resp.Body)

	logs, err := st.ListByOrg(context.Background(), "default", time.Now().Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(logs))
	}
	if logs[0].Status != "ok" || logs[0].ErrorType != "" {
		t.Errorf("status/error_type = %q/%q, want ok/\"\"", logs[0].Status, logs[0].ErrorType)
	}
}

func TestMessages_StreamingPassthrough_ByteForByte(t *testing.T) {
	// Regression for the duplicate SSE blank-line bug: streamMessages once
	// emitted the blank event-boundary line twice (once via normal line
	// forwarding, once inside flushBlank), so each event got TWO blank lines
	// instead of one — breaking the documented byte-for-byte copy. This test
	// compares the forwarded bytes against the exact upstream payload.
	//
	// We read the raw response body (NOT a line scanner, which would collapse
	// the doubled blanks and hide the bug).
	sseBody := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","role":"assistant","usage":{"input_tokens":10,"output_tokens":1}}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
		"",
	}, "\n")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, sseBody)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()

	ts, _ := newMessagesTestServer(t, "claude-sonnet-4-5", "claude-3-5-sonnet-latest", upstream)

	reqBody, _ := json.Marshal(map[string]any{
		"model":      "claude-sonnet-4-5",
		"max_tokens": 100,
		"stream":     true,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	})
	httpReq, _ := http.NewRequestWithContext(context.Background(), "POST", ts.URL+"/v1/messages", bytes.NewReader(reqBody))
	httpReq.Header.Set("Authorization", "Bearer "+testToken)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(httpReq)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	// The scanner in streamMessages splits on \n and re-joins each line with
	// a trailing \n, so the forwarded bytes are the upstream payload with a
	// trailing newline normalization. The decisive property is: no event
	// boundary may become a DOUBLE blank line.
	if strings.Contains(string(got), "\n\n\n") {
		t.Errorf("forwarded stream contains a double blank line (\\n\\n\\n) — "+
			"the SSE separator was written twice; got:\n%q", string(got))
	}

	// And it must be byte-identical to the upstream payload (scanner re-adds a
	// trailing \n to the final empty line, so upstream ends ...\n\n and output
	// matches exactly).
	if string(got) != sseBody {
		t.Errorf("forwarded stream is not byte-for-byte equal to upstream.\n got: %q\nwant: %q", string(got), sseBody)
	}
}

func TestMessages_NoDeployment_Returns404(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	defer upstream.Close()
	ts, _ := newMessagesTestServer(t, "claude-sonnet-4-5", "claude-3-5-sonnet-latest", upstream)

	resp := mustPost(t, ts, "/v1/messages", map[string]any{
		"model":      "no-such-model",
		"max_tokens": 1,
		"messages":   []map[string]any{{"role": "user", "content": "x"}},
	}, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestMessages_NonPassthroughProvider_ReturnsError(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	registry, _ := cost.LoadDefault()

	// A stub with no MessagesPassthroughFn returns nil, nil — router skips it
	// and exhausts all candidates → ErrAllFailed (500).
	deps := map[string][]router.Deployment{
		"gpt-4": {{Provider: &stub.Stub{NameValue: "openai"}, Model: "gpt-4o", Weight: 1}},
	}
	r := router.New(deps, nil)
	s := api.New(api.Config{Router: r, Store: st, Registry: registry, BearerToken: testToken})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp := mustPost(t, ts, "/v1/messages", map[string]any{
		"model":      "gpt-4",
		"max_tokens": 1,
		"messages":   []map[string]any{{"role": "user", "content": "x"}},
	}, testToken)
	defer resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Errorf("status = %d, want an error status", resp.StatusCode)
	}
}

func TestMessages_UpstreamRateLimitReturns429(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	}))
	defer upstream.Close()
	ts, _ := newMessagesTestServer(t, "claude-sonnet-4-5", "claude-3-5-sonnet-latest", upstream)

	resp := mustPost(t, ts, "/v1/messages", map[string]any{
		"model":      "claude-sonnet-4-5",
		"max_tokens": 1,
		"messages":   []map[string]any{{"role": "user", "content": "x"}},
	}, testToken)
	defer resp.Body.Close()
	// Router exhausts all deployments (all returned 429) and surfaces a 429
	// rate_limit_error in agentmodel's error envelope.
	if resp.StatusCode != 429 {
		t.Errorf("status = %d, want 429", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "rate_limit") {
		t.Errorf("body did not indicate rate limit: %s", body)
	}
}

func TestMessages_InvalidJSON_Returns400(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	defer upstream.Close()
	ts, _ := newMessagesTestServer(t, "claude-sonnet-4-5", "claude-3-5-sonnet-latest", upstream)
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/v1/messages", strings.NewReader("not-json"))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["type"] != "error" {
		t.Errorf("body[type] = %v, want error (Anthropic envelope)", body["type"])
	}
}

func TestMessages_MissingModel_Returns400(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	defer upstream.Close()
	ts, _ := newMessagesTestServer(t, "claude-sonnet-4-5", "claude-3-5-sonnet-latest", upstream)
	defer ts.Close()

	resp := mustPost(t, ts, "/v1/messages", map[string]any{
		"max_tokens": 1,
		"messages":   []map[string]any{{"role": "user", "content": "x"}},
	}, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["type"] != "error" {
		t.Errorf("body[type] = %v, want error (Anthropic envelope)", body["type"])
	}
}

func TestMessages_ClientBetasForwardedUpstream(t *testing.T) {
	var gotBeta string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBeta = r.Header.Get("anthropic-beta")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"m1","type":"message","role":"assistant","model":"claude-3-5-sonnet-latest","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	ts, _ := newMessagesTestServer(t, "claude-sonnet-4-5", "claude-3-5-sonnet-latest", upstream)
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/v1/messages", bytes.NewReader(func() []byte {
		b, _ := json.Marshal(map[string]any{
			"model":      "claude-sonnet-4-5",
			"max_tokens": 1,
			"messages":   []map[string]any{{"role": "user", "content": "hi"}},
		})
		return b
	}()))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-beta", "my-custom-beta-2024-01-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(gotBeta, "my-custom-beta-2024-01-01") {
		t.Errorf("upstream anthropic-beta = %q, want to contain my-custom-beta-2024-01-01", gotBeta)
	}
}

func TestMessages_RouterError_ReturnsAnthropicEnvelope(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	defer upstream.Close()
	ts, _ := newMessagesTestServer(t, "claude-sonnet-4-5", "claude-3-5-sonnet-latest", upstream)
	defer ts.Close()

	resp := mustPost(t, ts, "/v1/messages", map[string]any{
		"model":      "no-such-model",
		"max_tokens": 1,
		"messages":   []map[string]any{{"role": "user", "content": "x"}},
	}, testToken)
	defer resp.Body.Close()

	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["type"] != "error" {
		t.Errorf("body[type] = %v, want \"error\" (Anthropic envelope)", body["type"])
	}
	inner, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("body[error] = %v, want map", body["error"])
	}
	if inner["type"] == nil {
		t.Error("body[error][type] is nil, want a string")
	}
}

// TestMessages_StreamingUpstreamReadError_RecordsStatusError guards : a
// stream that dies WITHOUT an error frame — here a single line exceeding the
// 1 MiB scanner cap, which surfaces as scanner.Err() (bufio.ErrTooLong) exactly
// like a mid-stream transport drop — must be audited status=error, not the
// silent status=ok the loop produced before the fix.
func TestMessages_StreamingUpstreamReadError_RecordsStatusError(t *testing.T) {
	oversized := strings.Repeat("x", 1024*1024+16) // > scanner cap → ErrTooLong
	sseBody := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","role":"assistant","usage":{"input_tokens":42,"output_tokens":0}}}`,
		"",
		"data: " + oversized,
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

	ts, st := newMessagesTestServer(t, "claude-sonnet-4-5", "claude-3-5-sonnet-latest", upstream)

	req := map[string]any{
		"model":      "claude-sonnet-4-5",
		"max_tokens": 100,
		"stream":     true,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	}
	body, _ := json.Marshal(req)
	httpReq, _ := http.NewRequestWithContext(context.Background(), "POST", ts.URL+"/v1/messages", bytes.NewReader(body))
	httpReq.Header.Set("Authorization", "Bearer "+testToken)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(httpReq)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	logs, err := st.ListByOrg(context.Background(), "default", time.Now().Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(logs))
	}
	if logs[0].Status != "error" {
		t.Errorf("status = %q, want error (an upstream read failure must not audit as success)", logs[0].Status)
	}
}
