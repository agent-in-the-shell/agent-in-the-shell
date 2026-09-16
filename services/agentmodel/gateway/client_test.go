package gateway_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/gateway"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/wire"
)

func TestClient_Complete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		resp := wire.ChatResponse{
			Choices: []wire.Choice{
				{Message: wire.Message{Role: "assistant", Content: "hello"}, FinishReason: "stop"},
			},
			Usage: wire.Usage{PromptTokens: 10, CompletionTokens: 5},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := gateway.New(srv.URL, "test-model")
	req := wire.ChatRequest{
		Model:    "test-model",
		Messages: []wire.Message{{Role: "user", Content: "hi"}},
	}
	resp, err := c.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content != "hello" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

// TestClient_Complete_ClearsStreamField guards a request built for Stream and
// then routed to Complete by mistake (or reused across both calls): Complete
// must force stream=false regardless of what the caller set, since decoding
// an SSE body with json.Decoder.Decode fails opaquely ("invalid character
// 'd'") rather than a useful error.
func TestClient_Complete_ClearsStreamField(t *testing.T) {
	var gotStream bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in wire.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Errorf("server decode: %v", err)
		}
		gotStream = in.Stream
		json.NewEncoder(w).Encode(wire.ChatResponse{
			Choices: []wire.Choice{{Message: wire.Message{Role: "assistant", Content: "ok"}, FinishReason: "stop"}},
		})
	}))
	defer srv.Close()

	c := gateway.New(srv.URL, "test-model")
	_, err := c.Complete(context.Background(), wire.ChatRequest{
		Messages: []wire.Message{{Role: "user", Content: "hi"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotStream {
		t.Fatal("Complete must clear req.Stream before sending, got stream=true on the wire")
	}
}

func TestNewClientWithToken_ModelGetter(t *testing.T) {
	c := gateway.NewWithToken("http://example.invalid", "gpt-test", "secret")
	if got := c.Model(); got != "gpt-test" {
		t.Fatalf("Model() = %q, want %q", got, "gpt-test")
	}
}

func TestNewClientFromEnv_DefaultModelAndOverride(t *testing.T) {
	t.Setenv("AGENT_MODEL_URL", "")
	t.Setenv("AGENT_MODEL_MODEL", "")
	t.Setenv("AGENT_MODEL_TOKEN", "")

	c := gateway.NewFromEnv("")
	if got, want := c.Model(), "claude-haiku-4-5-20251001"; got != want {
		t.Fatalf("Model() = %q, want default %q", got, want)
	}

	t.Setenv("AGENT_MODEL_MODEL", "env-model")
	override := gateway.NewFromEnv("override-model")
	if got, want := override.Model(), "override-model"; got != want {
		t.Fatalf("Model() = %q, want override %q", got, want)
	}
}

func TestNewClientFromEnv_UsesEnvURLModelAndToken(t *testing.T) {
	var gotAuth, gotModel, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var in wire.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Errorf("server decode: %v", err)
		}
		gotModel = in.Model
		json.NewEncoder(w).Encode(wire.ChatResponse{
			Choices: []wire.Choice{
				{Message: wire.Message{Role: "assistant", Content: "from env"}, FinishReason: "stop"},
			},
		})
	}))
	defer srv.Close()

	t.Setenv("AGENT_MODEL_URL", srv.URL+"///")
	t.Setenv("AGENT_MODEL_MODEL", "env-model")
	t.Setenv("AGENT_MODEL_TOKEN", "env-token")

	c := gateway.NewFromEnv("")
	resp, err := c.Complete(context.Background(), wire.ChatRequest{
		Messages: []wire.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q, want trimmed gateway path", gotPath)
	}
	if gotAuth != "Bearer env-token" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "Bearer env-token")
	}
	if gotModel != "env-model" {
		t.Fatalf("server saw model %q, want env model %q", gotModel, "env-model")
	}
	if resp.Choices[0].Message.Content != "from env" {
		t.Fatalf("unexpected content: %q", resp.Choices[0].Message.Content)
	}
}

// TestClient_Complete_BearerHeader asserts the bearer-token branch sets the
// Authorization header, and that an empty req.Model is defaulted to c.model.
func TestClient_Complete_BearerHeader(t *testing.T) {
	var gotAuth, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var in wire.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Errorf("server decode: %v", err)
		}
		gotModel = in.Model
		resp := wire.ChatResponse{
			Choices: []wire.Choice{
				{Message: wire.Message{Role: "assistant", Content: "ok"}, FinishReason: "stop"},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := gateway.NewWithToken(srv.URL, "default-model", "my-token")
	// req.Model left empty so the default-from-client branch runs.
	resp, err := c.Complete(context.Background(), wire.ChatRequest{
		Messages: []wire.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer my-token" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "Bearer my-token")
	}
	if gotModel != "default-model" {
		t.Fatalf("server saw model %q, want default %q", gotModel, "default-model")
	}
	if resp.Choices[0].Message.Content != "ok" {
		t.Fatalf("unexpected content: %q", resp.Choices[0].Message.Content)
	}
}

// TestClient_Complete_NoToken asserts that without a bearer token the
// Authorization header is not set.
func TestClient_Complete_NoToken(t *testing.T) {
	var hadAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadAuth = r.Header["Authorization"]
		json.NewEncoder(w).Encode(wire.ChatResponse{})
	}))
	defer srv.Close()

	c := gateway.New(srv.URL, "m")
	if _, err := c.Complete(context.Background(), wire.ChatRequest{Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if hadAuth {
		t.Fatal("Authorization header should be absent when no token is set")
	}
}

// TestClient_Complete_StatusError hits the non-200 status branch.
func TestClient_Complete_StatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := gateway.New(srv.URL, "m")
	_, err := c.Complete(context.Background(), wire.ChatRequest{Model: "m"})
	if err == nil {
		t.Fatal("expected error on non-200 status")
	}
	if !strings.Contains(err.Error(), "gateway status 500") {
		t.Fatalf("error = %v, want it to mention status 500", err)
	}
}

// TestClient_Complete_DecodeError hits the JSON decode-error branch with a 200
// response carrying a malformed body.
func TestClient_Complete_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("{not valid json"))
	}))
	defer srv.Close()

	c := gateway.New(srv.URL, "m")
	_, err := c.Complete(context.Background(), wire.ChatRequest{Model: "m"})
	if err == nil {
		t.Fatal("expected decode error")
	}
	if !strings.Contains(err.Error(), "gateway decode") {
		t.Fatalf("error = %v, want it to mention 'gateway decode'", err)
	}
}

// TestClient_Complete_RequestError hits the http.Do error branch by pointing
// at a closed server (connection refused).
func TestClient_Complete_RequestError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // now nothing is listening at url

	c := gateway.New(url, "m")
	_, err := c.Complete(context.Background(), wire.ChatRequest{Model: "m"})
	if err == nil {
		t.Fatal("expected request error against closed server")
	}
	if !strings.Contains(err.Error(), "gateway request") {
		t.Fatalf("error = %v, want it to mention 'gateway request'", err)
	}
}

// TestClient_Complete_MarshalError verifies unsupported request values fail
// before any HTTP request is attempted.
func TestClient_Complete_MarshalError(t *testing.T) {
	c := gateway.New("http://example.invalid", "m")
	_, err := c.Complete(context.Background(), wire.ChatRequest{
		Model:      "m",
		ToolChoice: make(chan int),
	})
	if err == nil {
		t.Fatal("expected marshal error")
	}
	if !strings.Contains(err.Error(), "gateway marshal") {
		t.Fatalf("error = %v, want it to mention 'gateway marshal'", err)
	}
}

// TestClient_Complete_InvalidBaseURL verifies malformed client configuration
// returns the request-construction error without making a network call.
func TestClient_Complete_InvalidBaseURL(t *testing.T) {
	c := gateway.New("http://[::1", "m")
	_, err := c.Complete(context.Background(), wire.ChatRequest{Model: "m"})
	if err == nil {
		t.Fatal("expected invalid URL error")
	}
	if strings.Contains(err.Error(), "gateway request") {
		t.Fatalf("error = %v, should come from request construction", err)
	}
}

// TestClient_Complete_SurfacesGatewayError guards : a non-200 must include
// the gateway's structured error type+message, not just a bare status code, so
// callers can tell auth vs budget vs bad-request apart.
func TestClient_Complete_SurfacesGatewayError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"type":"budget_exceeded","message":"org cap reached"}}`)
	}))
	defer srv.Close()

	c := gateway.New(srv.URL, "m")
	_, err := c.Complete(context.Background(), wire.ChatRequest{Model: "m", Messages: []wire.Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("expected an error on 403")
	}
	if !strings.Contains(err.Error(), "budget_exceeded") || !strings.Contains(err.Error(), "org cap reached") {
		t.Errorf("error should carry the structured type+message, got: %v", err)
	}
}

func TestClient_Stream_YieldsChunksThenStopsOnDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got wire.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if !got.Stream {
			t.Errorf("Stream must set stream=true on the request, got %+v", got)
		}
		if got.Model != "test-model" {
			t.Errorf("Stream must default an empty req.Model to the client's model, got %+v", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, text := range []string{"he", "llo"} {
			chunk := wire.StreamChunk{
				Object:  "chat.completion.chunk",
				Choices: []wire.StreamChoice{{Delta: wire.Message{Role: "assistant", Content: text}}},
			}
			b, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		// Written after [DONE], still within this handler invocation. There
		// is no Flush() call anywhere in this handler, so the response body
		// is buffered and delivered to the client as one unit when the
		// handler returns — this does NOT establish that the connection was
		// still open, from the client's perspective, at the moment Stream
		// read [DONE]. What it does prove: given a body that contains bytes
		// after the [DONE] sentinel, Stream must stop exactly at [DONE] and
		// never yield (or accumulate) anything that follows it.
		extra := wire.StreamChunk{
			Object:  "chat.completion.chunk",
			Choices: []wire.StreamChoice{{Delta: wire.Message{Role: "assistant", Content: "EXTRA"}}},
		}
		eb, _ := json.Marshal(extra)
		fmt.Fprintf(w, "data: %s\n\n", eb)
	}))
	defer srv.Close()

	c := gateway.New(srv.URL, "test-model")
	seq, err := c.Stream(context.Background(), wire.ChatRequest{
		Messages: []wire.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var text string
	for chunk, err := range seq {
		if err != nil {
			t.Fatalf("mid-stream error: %v", err)
		}
		for _, ch := range chunk.Choices {
			text += ch.Delta.Content
		}
	}
	if text != "hello" {
		t.Fatalf("accumulated %q, want %q (stream must stop at [DONE], not consume frames written after it)", text, "hello")
	}
}

func TestClient_Stream_SurfacesGatewayErrorBeforeIterating(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"type": "authentication_error", "message": "bad key"},
		})
	}))
	defer srv.Close()

	c := gateway.New(srv.URL, "test-model")
	_, err := c.Stream(context.Background(), wire.ChatRequest{
		Messages: []wire.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected an error for a 401 before any chunk")
	}
	// Exact match, not a substring check: the structured-parse branch
	// formats "gateway status %d: %s: %s" (status, type, message). The
	// raw-body fallback would instead echo the whole JSON body verbatim
	// (e.g. `gateway status 401: {"error":{"type":"authentication_error",...`),
	// which also contains the substring "authentication_error" but does not
	// equal this exact string — so this assertion only passes if the
	// structured `type`/`message` fields were actually parsed out.
	wantErr := "gateway status 401: authentication_error: bad key"
	if err.Error() != wantErr {
		t.Fatalf("err = %q, want %q", err.Error(), wantErr)
	}
}

// TestClient_Stream_SurfacesMidStreamGatewayError guards against a
// mid-stream provider failure being silently swallowed as an empty chunk.
//
// The gateway's real handler (services/agentmodel/api/handlers_chat.go) can
// fail after the response is already a 200 (e.g. the upstream provider
// errors partway through generation). On that path it calls
// sw.SendError(agentmodel.Wrap(err)) then sw.Done() — streaming.Writer's
// SendError (services/agentmodel/streaming/sse.go) writes exactly
// `data: {"error": <wire.Error JSON>}\n\n`, the same wire.Error shape the
// pre-stream non-200 branch already parses. This test builds that envelope
// by hand (via encoding/json + wire.Error) rather than importing
// services/agentmodel/streaming directly, keeping even test dependencies
// independent of the gateway server implementation.
func TestClient_Stream_SurfacesMidStreamGatewayError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		good := wire.StreamChunk{
			Object:  "chat.completion.chunk",
			Choices: []wire.StreamChoice{{Delta: wire.Message{Role: "assistant", Content: "he"}}},
		}
		gb, _ := json.Marshal(good)
		fmt.Fprintf(w, "data: %s\n\n", gb)

		errEnv := map[string]any{
			"error": wire.Error{Type: wire.ErrTypeUpstream, Message: "boom mid-stream"},
		}
		eb, _ := json.Marshal(errEnv)
		fmt.Fprintf(w, "data: %s\n\n", eb)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := gateway.New(srv.URL, "test-model")
	seq, err := c.Stream(context.Background(), wire.ChatRequest{
		Messages: []wire.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var text string
	var chunkCount int
	var gotErr error
	for chunk, cerr := range seq {
		if cerr != nil {
			gotErr = cerr
			break
		}
		chunkCount++
		for _, ch := range chunk.Choices {
			text += ch.Delta.Content
		}
	}
	if gotErr == nil {
		t.Fatal("expected a mid-stream error, got nil (the error envelope was silently decoded as an empty chunk)")
	}
	wantErr := "gateway stream error: upstream_error: boom mid-stream"
	if gotErr.Error() != wantErr {
		t.Fatalf("err = %q, want %q", gotErr.Error(), wantErr)
	}
	// The chunk that arrived before the error must still have reached the
	// caller — the brief's documented contract is that a mid-stream failure
	// "arrives as the second value of the sequence, after however many
	// chunks did make it, so a caller can keep the partial output."
	if chunkCount != 1 || text != "he" {
		t.Fatalf("got %d chunk(s) accumulating %q before the error, want 1 chunk %q (partial output should survive)", chunkCount, text, "he")
	}
}

// TestClient_Stream_NoSpaceAfterColon guards the SSE spec's optional space
// after the colon: "data:{...}" is exactly as valid as "data: {...}". A
// scanner that only recognizes the "data: " (with space) form drops the
// no-space frame silently — no error, no signal, just a missing chunk.
func TestClient_Stream_NoSpaceAfterColon(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := wire.StreamChunk{
			Object:  "chat.completion.chunk",
			Choices: []wire.StreamChoice{{Delta: wire.Message{Role: "assistant", Content: "NOSPACE"}}},
		}
		b, _ := json.Marshal(chunk)
		// No space between "data:" and the payload, unlike every other test
		// in this file.
		fmt.Fprintf(w, "data:%s\n\n", b)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := gateway.New(srv.URL, "test-model")
	seq, err := c.Stream(context.Background(), wire.ChatRequest{
		Messages: []wire.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var text string
	for chunk, err := range seq {
		if err != nil {
			t.Fatalf("mid-stream error: %v", err)
		}
		for _, ch := range chunk.Choices {
			text += ch.Delta.Content
		}
	}
	if text != "NOSPACE" {
		t.Fatalf("accumulated %q, want %q (a data: frame with no space after the colon must not be dropped)", text, "NOSPACE")
	}
}

// TestClient_Stream_BearerHeader asserts the bearer-token branch sets the
// Authorization header on the streaming path, mirroring
// TestClient_Complete_BearerHeader for Complete. Deleting client.go's bearer
// block for Stream previously left the whole gateway suite green because
// nothing asserted on this header for Stream.
func TestClient_Stream_BearerHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := gateway.NewWithToken(srv.URL, "test-model", "my-token")
	seq, err := c.Stream(context.Background(), wire.ChatRequest{
		Messages: []wire.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for _, err := range seq {
		if err != nil {
			t.Fatalf("unexpected mid-stream error: %v", err)
		}
	}
	if gotAuth != "Bearer my-token" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "Bearer my-token")
	}
}

// TestClient_Stream_RejectsNonSSEContentType guards against a 200 response
// that isn't actually SSE (e.g. the gateway fell back to a plain JSON
// ChatResponse) being decoded as a silent, empty, error-free stream —
// indistinguishable from a genuinely empty completion.
func TestClient_Stream_RejectsNonSSEContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(wire.ChatResponse{
			Choices: []wire.Choice{
				{Message: wire.Message{Role: "assistant", Content: "hello"}, FinishReason: "stop"},
			},
		})
	}))
	defer srv.Close()

	c := gateway.New(srv.URL, "test-model")
	_, err := c.Stream(context.Background(), wire.ChatRequest{
		Messages: []wire.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected an error when the gateway returns a 200 that isn't SSE")
	}
	if !strings.Contains(err.Error(), "application/json") {
		t.Fatalf("error = %v, want it to name the Content-Type actually received", err)
	}
}

// TestClient_Stream_LargeFrame verifies a single SSE frame well past bufio's
// 64KB default line-length limit round-trips intact — the case a large
// tool-call-argument blob would hit. A no-op in place of client.go's
// sc.Buffer(...) call keeps the rest of the suite green since every other
// test here uses sub-200-byte frames.
func TestClient_Stream_LargeFrame(t *testing.T) {
	large := strings.Repeat("x", 200_000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := wire.StreamChunk{
			Object:  "chat.completion.chunk",
			Choices: []wire.StreamChoice{{Delta: wire.Message{Role: "assistant", Content: large}}},
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := gateway.New(srv.URL, "test-model")
	seq, err := c.Stream(context.Background(), wire.ChatRequest{
		Messages: []wire.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var got string
	for chunk, err := range seq {
		if err != nil {
			t.Fatalf("mid-stream error: %v", err)
		}
		for _, ch := range chunk.Choices {
			got += ch.Delta.Content
		}
	}
	if got != large {
		t.Fatalf("got %d bytes back, want %d bytes (a large frame must round-trip through the enlarged scanner buffer, not truncate or error)", len(got), len(large))
	}
}

// TestClient_Stream_ClosesBodyOnContextCancelWithoutRanging is a black-box
// contract test for the doc comment's promise: a caller that discards the
// sequence without ever ranging over it can still reclaim the connection by
// cancelling ctx. This test never ranges over seq at all — it only cancels
// ctx — and asserts the server actually observes the client disconnect.
//
// Caveat: as of this Go toolchain, net/http's default Transport already
// closes the underlying connection when a request's context is cancelled,
// independent of whether resp.Body is ever read or closed by the caller
// (verified separately, outside this package, with a bare http.Client and no
// gateway code involved at all). That means this assertion would still pass
// even without client.go's explicit ctx-watching goroutine — it guards the
// end-to-end contract, not that specific goroutine. The goroutine stays as a
// deliberate, local guarantee that doesn't depend on Transport internals
// (relevant if c.http ever gets a non-default RoundTripper), matching what
// the fix asks for; it just isn't independently falsifiable through this
// package's public API today.
func TestClient_Stream_ClosesBodyOnContextCancelWithoutRanging(t *testing.T) {
	serverSawDisconnect := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if f, ok := w.(http.Flusher); ok {
			f.Flush() // send headers now so the client's Do() returns
		}
		select {
		case <-r.Context().Done():
			close(serverSawDisconnect)
		case <-time.After(5 * time.Second):
			close(serverSawDisconnect) // avoid hanging the handler forever on failure
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	c := gateway.New(srv.URL, "test-model")
	seq, err := c.Stream(ctx, wire.ChatRequest{
		Messages: []wire.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		cancel()
		t.Fatalf("Stream: %v", err)
	}
	_ = seq // deliberately never ranged over

	cancel()

	select {
	case <-serverSawDisconnect:
	case <-time.After(3 * time.Second):
		t.Fatal("server never observed a client disconnect after ctx was cancelled; " +
			"Stream's response body was never closed for a sequence that was never ranged over (connection leak)")
	}
}
