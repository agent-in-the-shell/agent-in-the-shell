package gemini_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/gemini"
)

func newAuth() auth.Authenticator {
	return &auth.StaticKey{HeaderName: "x-goog-api-key", Token: "AIza-test-key"}
}

func TestComplete_RejectsNonTextContentParts(t *testing.T) {
	c := gemini.NewWithBaseURL(newAuth(), "http://127.0.0.1")
	var req agentmodel.ChatRequest
	if err := json.Unmarshal([]byte(`{
		"model":"gemini-2.5-pro",
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

func TestName(t *testing.T) {
	c := gemini.New(newAuth())
	if c.Name() != "gemini" {
		t.Errorf("Name: got %q, want gemini", c.Name())
	}
	if c.AuthMode() != agentmodel.AuthModeAPIKey {
		t.Errorf("AuthMode: got %q, want %q", c.AuthMode(), agentmodel.AuthModeAPIKey)
	}
	models := c.SupportedModels()
	want := map[string]bool{"gemini-2.5-pro": false, "gemini-2.5-flash": false, "text-embedding-004": false}
	for _, m := range models {
		if _, ok := want[m]; ok {
			want[m] = true
		}
	}
	for m, found := range want {
		if !found {
			t.Errorf("SupportedModels missing %q", m)
		}
	}
}

func TestComplete_SystemAndUser(t *testing.T) {
	var captured map[string]any
	var capturedAuth string
	var capturedPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("x-goog-api-key")
		capturedPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)

		resp := map[string]any{
			"candidates": []any{
				map[string]any{
					"content": map[string]any{
						"role":  "model",
						"parts": []any{map[string]any{"text": "Hi there!"}},
					},
					"finishReason": "STOP",
				},
			},
			"usageMetadata": map[string]any{
				"promptTokenCount":     7,
				"candidatesTokenCount": 3,
				"totalTokenCount":      10,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := gemini.NewWithBaseURL(newAuth(), srv.URL)
	temp := 0.5
	max := 200
	resp, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []agentmodel.Message{
			{Role: "system", Content: "be brief"},
			{Role: "user", Content: "hello"},
		},
		Temperature: &temp,
		MaxTokens:   &max,
		Stop:        []string{"END"},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if capturedAuth != "AIza-test-key" {
		t.Errorf("x-goog-api-key header: got %q", capturedAuth)
	}
	if !strings.Contains(capturedPath, "/v1beta/models/gemini-2.5-flash:generateContent") {
		t.Errorf("path: got %q", capturedPath)
	}

	// systemInstruction must be separate.
	si, ok := captured["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatalf("systemInstruction missing or wrong type: %#v", captured["systemInstruction"])
	}
	parts, _ := si["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("systemInstruction.parts: got %d, want 1", len(parts))
	}
	if got := parts[0].(map[string]any)["text"]; got != "be brief" {
		t.Errorf("systemInstruction text: got %v", got)
	}

	// contents must contain only user (no system).
	contents, ok := captured["contents"].([]any)
	if !ok || len(contents) != 1 {
		t.Fatalf("contents: got %#v", captured["contents"])
	}
	if role := contents[0].(map[string]any)["role"]; role != "user" {
		t.Errorf("contents[0].role: got %v, want user", role)
	}

	// generationConfig
	gc, ok := captured["generationConfig"].(map[string]any)
	if !ok {
		t.Fatalf("generationConfig missing")
	}
	if gc["temperature"].(float64) != 0.5 {
		t.Errorf("temperature: got %v", gc["temperature"])
	}
	if gc["maxOutputTokens"].(float64) != 200 {
		t.Errorf("maxOutputTokens: got %v", gc["maxOutputTokens"])
	}
	stops, _ := gc["stopSequences"].([]any)
	if len(stops) != 1 || stops[0] != "END" {
		t.Errorf("stopSequences: got %v", stops)
	}

	// Response shape
	if len(resp.Choices) != 1 {
		t.Fatalf("Choices: got %d", len(resp.Choices))
	}
	if resp.Choices[0].Message.Content != "Hi there!" {
		t.Errorf("Content: got %q", resp.Choices[0].Message.Content)
	}
	if resp.Choices[0].Message.Role != "assistant" {
		t.Errorf("Role: got %q, want assistant", resp.Choices[0].Message.Role)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("FinishReason: got %q", resp.Choices[0].FinishReason)
	}
	if resp.Usage.PromptTokens != 7 || resp.Usage.CompletionTokens != 3 || resp.Usage.TotalTokens != 10 {
		t.Errorf("Usage: got %+v", resp.Usage)
	}
	if resp.Model != "gemini-2.5-flash" {
		t.Errorf("Model: got %q", resp.Model)
	}
}

func TestComplete_ToolCall(t *testing.T) {
	var captured map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)

		// Respond with a functionCall
		resp := map[string]any{
			"candidates": []any{
				map[string]any{
					"content": map[string]any{
						"role": "model",
						"parts": []any{
							map[string]any{
								"functionCall": map[string]any{
									"name": "get_weather",
									"args": map[string]any{"location": "Tokyo", "unit": "c"},
								},
							},
						},
					},
					"finishReason": "STOP",
				},
			},
			"usageMetadata": map[string]any{
				"promptTokenCount":     12,
				"candidatesTokenCount": 8,
				"totalTokenCount":      20,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := gemini.NewWithBaseURL(newAuth(), srv.URL)
	resp, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model: "gemini-2.5-pro",
		Messages: []agentmodel.Message{
			{Role: "user", Content: "weather in Tokyo?"},
		},
		Tools: []agentmodel.Tool{
			{
				Type: "function",
				Function: agentmodel.FunctionSchema{
					Name:        "get_weather",
					Description: "Get the weather",
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"location": map[string]any{"type": "string"},
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Verify request shape
	tools, ok := captured["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools: got %#v", captured["tools"])
	}
	fdRaw, ok := tools[0].(map[string]any)["functionDeclarations"].([]any)
	if !ok || len(fdRaw) != 1 {
		t.Fatalf("functionDeclarations: got %#v", tools[0])
	}
	fd := fdRaw[0].(map[string]any)
	if fd["name"] != "get_weather" {
		t.Errorf("name: got %v", fd["name"])
	}
	if fd["description"] != "Get the weather" {
		t.Errorf("description: got %v", fd["description"])
	}
	if _, ok := fd["parameters"].(map[string]any); !ok {
		t.Errorf("parameters: got %#v", fd["parameters"])
	}

	// Verify response translation
	if len(resp.Choices) != 1 {
		t.Fatalf("Choices: got %d", len(resp.Choices))
	}
	tcs := resp.Choices[0].Message.ToolCalls
	if len(tcs) != 1 {
		t.Fatalf("tool_calls: got %d", len(tcs))
	}
	if tcs[0].Function.Name != "get_weather" {
		t.Errorf("tool name: got %q", tcs[0].Function.Name)
	}
	if tcs[0].Type != "function" {
		t.Errorf("tool type: got %q", tcs[0].Type)
	}
	if tcs[0].ID == "" {
		t.Errorf("tool ID empty")
	}
	// Arguments must be a JSON-encoded string
	var args map[string]any
	if err := json.Unmarshal([]byte(tcs[0].Function.Arguments), &args); err != nil {
		t.Fatalf("Arguments not JSON: %q (%v)", tcs[0].Function.Arguments, err)
	}
	if args["location"] != "Tokyo" || args["unit"] != "c" {
		t.Errorf("args: got %+v", args)
	}
}

func TestComplete_FinishReasonMapping(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"STOP", "stop"},
		{"MAX_TOKENS", "length"},
		{"SAFETY", "content_filter"},
		{"RECITATION", "stop"},
		{"OTHER", "stop"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				resp := map[string]any{
					"candidates": []any{
						map[string]any{
							"content": map[string]any{
								"role":  "model",
								"parts": []any{map[string]any{"text": "x"}},
							},
							"finishReason": tc.in,
						},
					},
					"usageMetadata": map[string]any{
						"promptTokenCount": 1, "candidatesTokenCount": 1, "totalTokenCount": 2,
					},
				}
				_ = json.NewEncoder(w).Encode(resp)
			}))
			defer srv.Close()

			c := gemini.NewWithBaseURL(newAuth(), srv.URL)
			resp, err := c.Complete(context.Background(), agentmodel.ChatRequest{
				Model: "gemini-2.5-flash",
				Messages: []agentmodel.Message{
					{Role: "user", Content: "hi"},
				},
			})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if resp.Choices[0].FinishReason != tc.want {
				t.Errorf("FinishReason: got %q, want %q", resp.Choices[0].FinishReason, tc.want)
			}
		})
	}
}

func TestStream(t *testing.T) {
	var capturedPath string
	var capturedAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path + "?" + r.URL.RawQuery
		capturedAuth = r.Header.Get("x-goog-api-key")
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		// Each chunk is a full GenerateContentResponse; the text field is
		// cumulative across chunks (the impl computes deltas).
		chunks := []string{
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]}}]}`,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hello world"}]}}]}`,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hello world!"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":3,"totalTokenCount":7}}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
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

	var got []string
	var finalUsage *agentmodel.Usage
	var finalReason string
	for ch, err := range seq {
		if err != nil {
			t.Fatalf("yield error: %v", err)
		}
		if ch.Delta.Content != "" {
			got = append(got, ch.Delta.Content)
		}
		if ch.Usage != nil {
			finalUsage = ch.Usage
		}
		if ch.FinishReason != "" {
			finalReason = ch.FinishReason
		}
	}

	if capturedAuth != "AIza-test-key" {
		t.Errorf("auth header missing: got %q", capturedAuth)
	}
	if !strings.Contains(capturedPath, ":streamGenerateContent") || !strings.Contains(capturedPath, "alt=sse") {
		t.Errorf("path: got %q", capturedPath)
	}

	// Deltas: "Hello", " world", "!"
	joined := strings.Join(got, "")
	if joined != "Hello world!" {
		t.Errorf("joined deltas: got %q, want %q", joined, "Hello world!")
	}
	if len(got) < 3 {
		t.Errorf("expected >=3 deltas, got %d: %v", len(got), got)
	}
	if got[0] != "Hello" {
		t.Errorf("first delta: got %q, want %q", got[0], "Hello")
	}
	if got[1] != " world" {
		t.Errorf("second delta: got %q, want %q", got[1], " world")
	}
	if got[2] != "!" {
		t.Errorf("third delta: got %q, want %q", got[2], "!")
	}
	if finalReason != "stop" {
		t.Errorf("FinishReason: got %q", finalReason)
	}
	if finalUsage == nil {
		t.Fatalf("final Usage nil")
	}
	if finalUsage.PromptTokens != 4 || finalUsage.CompletionTokens != 3 || finalUsage.TotalTokens != 7 {
		t.Errorf("Usage: got %+v", *finalUsage)
	}
}

func TestEmbed(t *testing.T) {
	var capturedAuth string
	var calls int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("x-goog-api-key")
		calls++
		if !strings.Contains(r.URL.Path, "/v1beta/models/text-embedding-004:embedContent") {
			t.Errorf("path: got %q", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)

		// Single-input shape: { content: { parts: [{text}] } }
		content, ok := req["content"].(map[string]any)
		if !ok {
			t.Fatalf("content missing in req: %#v", req)
		}
		parts, _ := content["parts"].([]any)
		if len(parts) != 1 {
			t.Errorf("parts: got %d, want 1", len(parts))
		}

		resp := map[string]any{
			"embedding": map[string]any{
				"values": []float64{0.1, 0.2, 0.3},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := gemini.NewWithBaseURL(newAuth(), srv.URL)
	resp, err := c.Embed(context.Background(), agentmodel.EmbeddingRequest{
		Model: "text-embedding-004",
		Input: []string{"hello", "world"},
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if capturedAuth != "AIza-test-key" {
		t.Errorf("auth: got %q", capturedAuth)
	}
	if calls != 2 {
		t.Errorf("expected 2 single-input calls, got %d", calls)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("Data: got %d, want 2", len(resp.Data))
	}
	for i, e := range resp.Data {
		if e.Index != i {
			t.Errorf("Data[%d].Index: got %d", i, e.Index)
		}
		if len(e.Embedding) != 3 {
			t.Errorf("Data[%d].Embedding len: got %d", i, len(e.Embedding))
		}
	}
	if resp.Model != "text-embedding-004" {
		t.Errorf("Model: got %q", resp.Model)
	}
}

func TestComplete_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"bad request"}}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	c := gemini.NewWithBaseURL(newAuth(), srv.URL)
	_, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []agentmodel.Message{
			{Role: "user", Content: "hi"},
		},
	})
	if err == nil {
		t.Fatalf("expected error on HTTP 400")
	}
}

func TestListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1beta/models" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"models":[{"name":"models/gemini-2.5-flash"},{"name":"models/gemini-2.5-pro"}]}`)
	}))
	defer srv.Close()

	got, err := gemini.NewWithBaseURL(newAuth(), srv.URL).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	// The "models/" prefix is stripped to the bare id.
	if len(got) != 2 || got[0] != "gemini-2.5-flash" || got[1] != "gemini-2.5-pro" {
		t.Errorf("got %v, want [gemini-2.5-flash gemini-2.5-pro]", got)
	}
}

// TestComplete_CachedContentTokens verifies Gemini's cachedContentTokenCount is
// surfaced as CacheReadInputTokens (non-streaming). PromptTokens already
// includes the cached portion, so it is reported unchanged.
func TestComplete_CachedContentTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],
			"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":20,"totalTokenCount":1020,"cachedContentTokenCount":800}
		}`)
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
	if resp.Usage.CacheReadInputTokens != 800 {
		t.Errorf("CacheReadInputTokens: got %d, want 800", resp.Usage.CacheReadInputTokens)
	}
	// PromptTokens already includes the cached tokens (unlike Anthropic).
	if resp.Usage.PromptTokens != 1000 {
		t.Errorf("PromptTokens: got %d, want 1000", resp.Usage.PromptTokens)
	}
}

// TestStream_CachedContentTokens verifies cachedContentTokenCount surfaces as
// CacheReadInputTokens on the terminal streaming usage chunk.
func TestStream_CachedContentTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		chunks := []string{
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":20,"totalTokenCount":1020,"cachedContentTokenCount":800}}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
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
	var finalUsage *agentmodel.Usage
	for ch, err := range seq {
		if err != nil {
			t.Fatalf("yield error: %v", err)
		}
		if ch.Usage != nil {
			finalUsage = ch.Usage
		}
	}
	if finalUsage == nil {
		t.Fatal("final Usage nil")
	}
	if finalUsage.CacheReadInputTokens != 800 {
		t.Errorf("CacheReadInputTokens: got %d, want 800", finalUsage.CacheReadInputTokens)
	}
}

// assertPromptBlocked verifies a prompt-level safety block surfaces as a TYPED
// content-filter error (a terminal 400, not the retryable 502 a bare error would
// classify to), naming the block reason.
func assertPromptBlocked(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a content-filter error for a prompt-blocked response, got nil")
	}
	var ae *agentmodel.Error
	if !errors.As(err, &ae) {
		t.Fatalf("error is not *agentmodel.Error: %v", err)
	}
	if ae.Type != agentmodel.ErrTypeContentFilter {
		t.Errorf("error type = %q, want %q", ae.Type, agentmodel.ErrTypeContentFilter)
	}
	if !strings.Contains(ae.Message, "SAFETY") {
		t.Errorf("error should name the block reason, got: %q", ae.Message)
	}
}

// TestComplete_PromptBlockedSurfacesError: a prompt-level safety block (zero
// candidates + promptFeedback.blockReason) surfaces as a typed content-filter
// error rather than a silent, signal-less empty HTTP 200 (#1487).
func TestComplete_PromptBlockedSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"promptFeedback":{"blockReason":"SAFETY"}}`)
	}))
	defer srv.Close()

	c := gemini.NewWithBaseURL(newAuth(), srv.URL)
	_, err := c.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "gemini-2.5-flash",
		Messages: []agentmodel.Message{{Role: "user", Content: "x"}},
	})
	assertPromptBlocked(t, err)
}

// TestStream_PromptBlockedSurfacesError: the SAME block on the streaming path
// (the default for SDK clients) must yield the error, not swallow it into a
// truncated success (#1487) — the gap /simplify caught in the first pass.
func TestStream_PromptBlockedSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"promptFeedback\":{\"blockReason\":\"SAFETY\"}}\n\n")
	}))
	defer srv.Close()

	c := gemini.NewWithBaseURL(newAuth(), srv.URL)
	seq, err := c.Stream(context.Background(), agentmodel.ChatRequest{
		Model:    "gemini-2.5-flash",
		Stream:   true,
		Messages: []agentmodel.Message{{Role: "user", Content: "x"}},
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
	assertPromptBlocked(t, gotErr)
}
