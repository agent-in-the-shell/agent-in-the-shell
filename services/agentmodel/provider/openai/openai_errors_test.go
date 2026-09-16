package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
)

// errAuth is a fake Authenticator whose Apply always fails. It lets us drive
// the "apply auth" error path in newRequest without a real credential source.
type errAuth struct{ err error }

func (e errAuth) Mode() string { return agentmodel.AuthModeAPIKey }
func (e errAuth) Apply(ctx context.Context, req *http.Request) error {
	return e.err
}

// statusServer returns a server that always replies with the given status and
// body, regardless of path.
func statusServer(status int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
}

func newChatReq() agentmodel.ChatRequest {
	return agentmodel.ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	}
}

func newEmbedReq() agentmodel.EmbeddingRequest {
	return agentmodel.EmbeddingRequest{
		Model: "text-embedding-3-small",
		Input: []string{"hello"},
	}
}

// --- mapHTTPError branches via Complete ------------------------------------

func TestComplete_400_ContextLengthExceeded(t *testing.T) {
	srv := statusServer(http.StatusBadRequest,
		`{"error":{"message":"too long","code":"context_length_exceeded"}}`)
	defer srv.Close()

	c := newClient(t, srv, "sk-foo")
	_, err := c.Complete(context.Background(), newChatReq())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "context window exceeded") {
		t.Errorf("err = %q, want substring 'context window exceeded'", err.Error())
	}
	// The raw body must be passed through for visibility.
	if !strings.Contains(err.Error(), "context_length_exceeded") {
		t.Errorf("err = %q, want raw body passthrough", err.Error())
	}
}

func TestComplete_400_GenericBadRequest(t *testing.T) {
	srv := statusServer(http.StatusBadRequest,
		`{"error":{"message":"missing model"}}`)
	defer srv.Close()

	c := newClient(t, srv, "sk-foo")
	_, err := c.Complete(context.Background(), newChatReq())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "bad request") {
		t.Errorf("err = %q, want substring 'bad request'", err.Error())
	}
	if !strings.Contains(err.Error(), "status=400") {
		t.Errorf("err = %q, want substring 'status=400'", err.Error())
	}
	// Must NOT be misclassified as a context-window error.
	if strings.Contains(err.Error(), "context window") {
		t.Errorf("err = %q, plain 400 misclassified as context-window error", err.Error())
	}
}

func TestComplete_500_UpstreamError(t *testing.T) {
	srv := statusServer(http.StatusInternalServerError, `{"error":{"message":"boom"}}`)
	defer srv.Close()

	c := newClient(t, srv, "sk-foo")
	_, err := c.Complete(context.Background(), newChatReq())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "upstream error") {
		t.Errorf("err = %q, want substring 'upstream error'", err.Error())
	}
	if !strings.Contains(err.Error(), "status=500") {
		t.Errorf("err = %q, want substring 'status=500'", err.Error())
	}
}

func TestComplete_DefaultStatusFallback(t *testing.T) {
	// 418 is not specially classified, not >=500: hits the default branch.
	srv := statusServer(http.StatusTeapot, `short and stout`)
	defer srv.Close()

	c := newClient(t, srv, "sk-foo")
	_, err := c.Complete(context.Background(), newChatReq())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "request failed") {
		t.Errorf("err = %q, want substring 'request failed'", err.Error())
	}
	if !strings.Contains(err.Error(), "status=418") {
		t.Errorf("err = %q, want substring 'status=418'", err.Error())
	}
	// Body trimmed/passed through.
	if !strings.Contains(err.Error(), "short and stout") {
		t.Errorf("err = %q, want raw body passthrough", err.Error())
	}
}

// --- Complete decode + network ---------------------------------------------

func TestComplete_MalformedBody_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{not valid json`)
	}))
	defer srv.Close()

	c := newClient(t, srv, "sk-foo")
	_, err := c.Complete(context.Background(), newChatReq())
	if err == nil {
		t.Fatal("expected decode error")
	}
	if !strings.Contains(err.Error(), "decode response") {
		t.Errorf("err = %q, want substring 'decode response'", err.Error())
	}
}

func TestComplete_NetworkError(t *testing.T) {
	// Start a server, capture its URL, then close it so Do() fails to connect.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	c := newClient(t, srv, "sk-foo")
	srv.Close()

	_, err := c.Complete(context.Background(), newChatReq())
	if err == nil {
		t.Fatal("expected network error")
	}
	// The op label send stamps on transport failures. Complete's message used
	// to be a bare "do request", the one call site with no name in it.
	if !strings.Contains(err.Error(), "openai: chat-completions:") {
		t.Errorf("err = %q, want the endpoint label 'openai: chat-completions:'", err.Error())
	}
}

// --- toAgentmodel cached-token flattening ----------------------------------

func TestComplete_CachedTokensFlattened(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id": "chatcmpl-cache",
			"object": "chat.completion",
			"created": 1700000000,
			"model": "gpt-4o-mini",
			"choices": [
				{"index": 0, "message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}
			],
			"usage": {
				"prompt_tokens": 100,
				"completion_tokens": 10,
				"total_tokens": 110,
				"prompt_tokens_details": {"cached_tokens": 64}
			}
		}`)
	}))
	defer srv.Close()

	c := newClient(t, srv, "sk-foo")
	resp, err := c.Complete(context.Background(), newChatReq())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Usage.CacheReadInputTokens != 64 {
		t.Errorf("CacheReadInputTokens = %d, want 64", resp.Usage.CacheReadInputTokens)
	}
	if resp.Usage.PromptTokens != 100 || resp.Usage.TotalTokens != 110 {
		t.Errorf("usage = %+v, want prompt=100 total=110", resp.Usage)
	}
}

// --- Embed error + decode paths --------------------------------------------

func TestEmbed_Non200_ErrorMapping(t *testing.T) {
	srv := statusServer(http.StatusInternalServerError, `{"error":{"message":"down"}}`)
	defer srv.Close()

	c := newClient(t, srv, "sk-foo")
	_, err := c.Embed(context.Background(), newEmbedReq())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "upstream error") {
		t.Errorf("err = %q, want substring 'upstream error'", err.Error())
	}
}

func TestEmbed_MalformedBody_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `}{ broken`)
	}))
	defer srv.Close()

	c := newClient(t, srv, "sk-foo")
	_, err := c.Embed(context.Background(), newEmbedReq())
	if err == nil {
		t.Fatal("expected decode error")
	}
	if !strings.Contains(err.Error(), "decode embed response") {
		t.Errorf("err = %q, want substring 'decode embed response'", err.Error())
	}
}

func TestEmbed_NetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	c := newClient(t, srv, "sk-foo")
	srv.Close()

	_, err := c.Embed(context.Background(), newEmbedReq())
	if err == nil {
		t.Fatal("expected network error")
	}
	if !strings.Contains(err.Error(), "openai: embeddings:") {
		t.Errorf("err = %q, want the endpoint label 'openai: embeddings:'", err.Error())
	}
}

// --- Stream error/decode/early-break paths ---------------------------------

func TestStream_Non200_BeforeStreaming(t *testing.T) {
	srv := statusServer(http.StatusBadRequest,
		`{"error":{"code":"context_length_exceeded"}}`)
	defer srv.Close()

	c := newClient(t, srv, "sk-foo")
	seq, err := c.Stream(context.Background(), newChatReq())
	if err == nil {
		t.Fatal("expected error before streaming")
	}
	if seq != nil {
		t.Errorf("seq = %v, want nil on pre-stream error", seq)
	}
	if !strings.Contains(err.Error(), "context window exceeded") {
		t.Errorf("err = %q, want substring 'context window exceeded'", err.Error())
	}
}

func TestStream_MalformedChunk_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// A good chunk, then a malformed data line.
		_, _ = io.WriteString(w, "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {not json}\n\n")
	}))
	defer srv.Close()

	c := newClient(t, srv, "sk-foo")
	seq, err := c.Stream(context.Background(), newChatReq())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var sawErr error
	var goodChunks int
	for ch, err := range seq {
		if err != nil {
			sawErr = err
			break
		}
		if ch.Delta.Content != "" {
			goodChunks++
		}
	}
	if goodChunks != 1 {
		t.Errorf("goodChunks = %d, want 1 before the decode error", goodChunks)
	}
	if sawErr == nil {
		t.Fatal("expected a decode error yielded from the malformed chunk")
	}
	if !strings.Contains(sawErr.Error(), "decode stream chunk") {
		t.Errorf("err = %q, want substring 'decode stream chunk'", sawErr.Error())
	}
}

func TestStream_CachedTokensFlattened(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"x\",\"choices\":[],\"usage\":{\"prompt_tokens\":80,\"completion_tokens\":4,\"total_tokens\":84,\"prompt_tokens_details\":{\"cached_tokens\":32}}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := newClient(t, srv, "sk-foo")
	seq, err := c.Stream(context.Background(), newChatReq())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var usage *agentmodel.Usage
	for ch, err := range seq {
		if err != nil {
			t.Fatalf("stream yielded error: %v", err)
		}
		if ch.Usage != nil {
			usage = ch.Usage
		}
	}
	if usage == nil {
		t.Fatal("expected non-nil usage chunk")
	}
	if usage.CacheReadInputTokens != 32 {
		t.Errorf("CacheReadInputTokens = %d, want 32", usage.CacheReadInputTokens)
	}
	if usage.TotalTokens != 84 {
		t.Errorf("TotalTokens = %d, want 84", usage.TotalTokens)
	}
}

func TestStream_ConsumerEarlyBreak(t *testing.T) {
	// Consumer breaks after the first chunk; the yield returns false and the
	// producer's `if !yield(...) { return }` path runs (body closed via defer).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"one\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"two\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"three\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := newClient(t, srv, "sk-foo")
	seq, err := c.Stream(context.Background(), newChatReq())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var seen int
	var first string
	for ch, err := range seq {
		if err != nil {
			t.Fatalf("stream yielded error: %v", err)
		}
		seen++
		first = ch.Delta.Content
		break
	}
	if seen != 1 {
		t.Errorf("consumed %d chunks, want exactly 1 (early break)", seen)
	}
	if first != "one" {
		t.Errorf("first chunk = %q, want 'one'", first)
	}
}

// --- newRequest auth branches ----------------------------------------------

func TestNewRequest_AuthApplyError(t *testing.T) {
	// Auth that fails on Apply; the error must surface through Complete.
	c := NewWithBaseURL(errAuth{err: errors.New("token expired")}, "http://example.invalid")
	_, err := c.Complete(context.Background(), newChatReq())
	if err == nil {
		t.Fatal("expected apply-auth error")
	}
	if !strings.Contains(err.Error(), "apply auth") {
		t.Errorf("err = %q, want substring 'apply auth'", err.Error())
	}
	if !strings.Contains(err.Error(), "token expired") {
		t.Errorf("err = %q, want wrapped cause 'token expired'", err.Error())
	}
}

func TestNewRequest_NilAuthSkipped(t *testing.T) {
	// A Client with nil auth must skip the Apply branch entirely and still
	// reach the server (no Authorization header set).
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","created":0,"model":"m","choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`)
	}))
	defer srv.Close()

	c := NewWithBaseURL(nil, srv.URL)
	resp, err := c.Complete(context.Background(), newChatReq())
	if err != nil {
		t.Fatalf("Complete with nil auth: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty (nil auth skipped)", gotAuth)
	}
	if resp.ID != "x" {
		t.Errorf("resp.ID = %q, want 'x'", resp.ID)
	}
}

// --- Retry-After hint (429 only) -------------------------------------------

// mapHTTPError is the single non-2xx classifier for the OpenAI wire path, which
// azure, deepseek, and every openaicompat vendor compose. A 429 that names its
// own reset must carry that through, or a multi-key pool falls back to guessing
// on every provider except anthropic and chatgpt.
func TestComplete_429_CarriesRetryAfterHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down"}}`)
	}))
	defer srv.Close()

	c := newClient(t, srv, "sk-foo")
	_, err := c.Complete(context.Background(), newChatReq())
	if err == nil {
		t.Fatal("expected a rate-limit error")
	}
	ae := agentmodel.Wrap(err)
	if ae.Type != agentmodel.ErrTypeRateLimit {
		t.Fatalf("Type=%q, want %q", ae.Type, agentmodel.ErrTypeRateLimit)
	}
	if ae.RetryAfter != 10*time.Minute {
		t.Errorf("RetryAfter=%v, want 10m", ae.RetryAfter)
	}
}

// A Retry-After on a 5xx is backoff advice for this request, not evidence the
// credential is out of quota — parking it would idle a healthy key.
func TestComplete_500_CarriesNoRetryAfterHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	}))
	defer srv.Close()

	c := newClient(t, srv, "sk-foo")
	_, err := c.Complete(context.Background(), newChatReq())
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := agentmodel.Wrap(err).RetryAfter; got != 0 {
		t.Errorf("RetryAfter=%v, want 0 on a non-429", got)
	}
}
