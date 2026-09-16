package pool_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/pool"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/stub"
)

var (
	rateLimitErr = stub.ErrTestRateLimit // "stub: rate limited" → classified as ErrTypeRateLimit by Wrap
	authErr      = stub.ErrTestAuth      // "stub: auth failed" → Wrap classifies it upstream_error (RETRYABLE); the pool still returns it immediately because it only cycles on rate_limit
)

var okResp = agentmodel.ChatResponse{
	ID:    "resp-1",
	Model: "m",
	Choices: []agentmodel.Choice{
		{Message: agentmodel.Message{Role: "assistant", Content: "ok"}},
	},
}

// ──────────────────────────────────────────────────────────────────────────────
// Complete
// ──────────────────────────────────────────────────────────────────────────────

func TestPool_Complete_FirstSucceeds(t *testing.T) {
	p1 := &stub.Stub{CompleteResp: okResp}
	p2 := &stub.Stub{CompleteResp: okResp}
	pl := pool.New([]provider.Provider{p1, p2})

	resp, err := pl.Complete(ctx(), req("m"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ID != "resp-1" {
		t.Errorf("unexpected response: %+v", resp)
	}
	if p1.CallCount() != 1 {
		t.Errorf("expected p1 called once, got %d", p1.CallCount())
	}
	if p2.CallCount() != 0 {
		t.Errorf("expected p2 not called, got %d", p2.CallCount())
	}
}

func TestPool_Complete_RateLimitFallsToSecond(t *testing.T) {
	p1 := &stub.Stub{CompleteErr: rateLimitErr}
	p2 := &stub.Stub{CompleteResp: okResp}
	pl := pool.New([]provider.Provider{p1, p2})

	resp, err := pl.Complete(ctx(), req("m"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ID != "resp-1" {
		t.Errorf("unexpected response: %+v", resp)
	}
	if p1.CallCount() != 1 {
		t.Errorf("expected p1 tried once")
	}
	if p2.CallCount() != 1 {
		t.Errorf("expected p2 tried once")
	}
}

func TestPool_Complete_AllRateLimited(t *testing.T) {
	p1 := &stub.Stub{CompleteErr: rateLimitErr}
	p2 := &stub.Stub{CompleteErr: rateLimitErr}
	pl := pool.New([]provider.Provider{p1, p2})

	_, err := pl.Complete(ctx(), req("m"))
	if err == nil {
		t.Fatal("expected error when all credentials rate-limited")
	}
	ae := agentmodel.Wrap(err)
	if ae.Type != agentmodel.ErrTypeRateLimit {
		t.Errorf("expected rate_limit_error, got %q", ae.Type)
	}
}

func TestPool_Complete_NonRateLimitErrorNotRetried(t *testing.T) {
	p1 := &stub.Stub{CompleteErr: authErr}
	p2 := &stub.Stub{CompleteResp: okResp}
	pl := pool.New([]provider.Provider{p1, p2})

	_, err := pl.Complete(ctx(), req("m"))
	if err == nil {
		t.Fatal("expected error")
	}
	// p2 must NOT have been tried
	if p2.CallCount() != 0 {
		t.Errorf("non-rate-limit error should not trigger fallback, but p2 was called")
	}
}

func TestPool_Complete_CooldownPerModel(t *testing.T) {
	// A rate limit on model "a" must not park the same credential for model "b".
	// Both credentials fail on "a" so neither is selected, leaving affinity at
	// index 0 — otherwise stickiness, not the cooldown key, would decide which
	// credential serves "b" and the per-model isolation would go untested.
	p1 := &stub.Stub{CompleteErr: fmt.Errorf("429 rate limit for model a")}
	p2 := &stub.Stub{CompleteErr: fmt.Errorf("429 rate limit for model a")}
	pl := pool.New([]provider.Provider{p1, p2})

	if _, err := pl.Complete(ctx(), req("a")); err == nil {
		t.Fatal("expected exhaustion on model a")
	}
	callsAfterA := p1.CallCount()

	// Model "b" is untouched by those cooldowns: p1 must serve it.
	p1.CompleteErr = nil
	p1.CompleteResp = okResp
	if _, err := pl.Complete(ctx(), req("b")); err != nil {
		t.Fatalf("model b request failed: %v", err)
	}
	if p1.CallCount() != callsAfterA+1 {
		t.Errorf("p1 calls=%d, want %d — a cooldown on model a leaked into model b",
			p1.CallCount(), callsAfterA+1)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Stream
// ──────────────────────────────────────────────────────────────────────────────

func TestPool_Stream_FirstSucceeds(t *testing.T) {
	p1 := &stub.Stub{}
	p2 := &stub.Stub{}
	pl := pool.New([]provider.Provider{p1, p2})

	seq, err := pl.Stream(ctx(), req("m"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	count := 0
	for _, e := range seq {
		if e != nil {
			t.Errorf("unexpected stream error: %v", e)
		}
		count++
	}
	if count == 0 {
		t.Errorf("expected at least one chunk")
	}
}

func TestPool_Stream_RateLimitFallsToSecond(t *testing.T) {
	p1 := &stub.Stub{StreamErr: rateLimitErr}
	p2 := &stub.Stub{}
	pl := pool.New([]provider.Provider{p1, p2})

	_, err := pl.Stream(ctx(), req("m"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p2.CallCount() != 1 {
		t.Errorf("expected p2 called once, got %d", p2.CallCount())
	}
}

func TestPool_Stream_NonRateLimitErrorNotRetried(t *testing.T) {
	// pool.go:81 — a non-rate-limit error from .Stream() is returned
	// immediately; the second provider is never consulted.
	p1 := &stub.Stub{StreamErr: authErr}
	p2 := &stub.Stub{}
	pl := pool.New([]provider.Provider{p1, p2})

	seq, err := pl.Stream(ctx(), req("m"))
	if err == nil {
		t.Fatal("expected error from first provider's stream")
	}
	if seq != nil {
		t.Errorf("expected nil iterator on error, got non-nil")
	}
	if !errors.Is(err, authErr) {
		t.Errorf("expected original auth error to propagate, got %v", err)
	}
	if ae := agentmodel.Wrap(err); ae.Type == agentmodel.ErrTypeRateLimit {
		t.Errorf("auth error must not be classified as rate_limit_error")
	}
	if p1.CallCount() != 1 {
		t.Errorf("expected p1 tried once, got %d", p1.CallCount())
	}
	if p2.CallCount() != 0 {
		t.Errorf("non-rate-limit stream error must not trigger fallback, but p2 was called %d time(s)", p2.CallCount())
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// SupportedModels — delegates to providers[0]
// ──────────────────────────────────────────────────────────────────────────────

func TestPool_SupportedModels_DelegatesToFirst(t *testing.T) {
	want := []string{"alpha", "beta"}
	p1 := &stub.Stub{Models: want}
	p2 := &stub.Stub{Models: []string{"gamma"}} // must be ignored
	pl := pool.New([]provider.Provider{p1, p2})

	got := pl.SupportedModels()
	if len(got) != len(want) {
		t.Fatalf("expected %d models, got %d (%v)", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("model[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Embed — mirrors the Complete fallback table
// ──────────────────────────────────────────────────────────────────────────────

var okEmbed = agentmodel.EmbeddingResponse{
	Object: "list",
	Model:  "embed-m",
	Data: []agentmodel.Embedding{
		{Object: "embedding", Index: 0, Embedding: []float64{0.5, 0.6}},
	},
	Usage: agentmodel.Usage{PromptTokens: 3, TotalTokens: 3},
}

func embReq(model string) agentmodel.EmbeddingRequest {
	return agentmodel.EmbeddingRequest{Model: model, Input: []string{"hello"}}
}

func TestPool_Embed_FirstSucceeds(t *testing.T) {
	p1 := &stub.Stub{EmbedResp: okEmbed}
	p2 := &stub.Stub{EmbedResp: okEmbed}
	pl := pool.New([]provider.Provider{p1, p2})

	resp, err := pl.Embed(ctx(), embReq("embed-m"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Model != "embed-m" || len(resp.Data) != 1 {
		t.Errorf("unexpected embed response: %+v", resp)
	}
	if p1.CallCount() != 1 {
		t.Errorf("expected p1 called once, got %d", p1.CallCount())
	}
	if p2.CallCount() != 0 {
		t.Errorf("expected p2 not called, got %d", p2.CallCount())
	}
}

func TestPool_Embed_RateLimitFallsToSecond(t *testing.T) {
	p1 := &stub.Stub{EmbedErr: rateLimitErr}
	p2 := &stub.Stub{EmbedResp: okEmbed}
	pl := pool.New([]provider.Provider{p1, p2})

	resp, err := pl.Embed(ctx(), embReq("embed-m"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Model != "embed-m" {
		t.Errorf("expected second provider response, got %+v", resp)
	}
	if p1.CallCount() != 1 {
		t.Errorf("expected p1 tried once, got %d", p1.CallCount())
	}
	if p2.CallCount() != 1 {
		t.Errorf("expected p2 tried once, got %d", p2.CallCount())
	}
}

func TestPool_Embed_AllRateLimited(t *testing.T) {
	p1 := &stub.Stub{EmbedErr: rateLimitErr}
	p2 := &stub.Stub{EmbedErr: rateLimitErr}
	pl := pool.New([]provider.Provider{p1, p2})

	_, err := pl.Embed(ctx(), embReq("embed-m"))
	if err == nil {
		t.Fatal("expected error when all credentials rate-limited")
	}
	ae := agentmodel.Wrap(err)
	if ae.Type != agentmodel.ErrTypeRateLimit {
		t.Errorf("expected rate_limit_error, got %q", ae.Type)
	}
}

func TestPool_Embed_NonRateLimitErrorNotRetried(t *testing.T) {
	p1 := &stub.Stub{EmbedErr: authErr}
	p2 := &stub.Stub{EmbedResp: okEmbed}
	pl := pool.New([]provider.Provider{p1, p2})

	_, err := pl.Embed(ctx(), embReq("embed-m"))
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, authErr) {
		t.Errorf("expected original auth error to propagate, got %v", err)
	}
	if ae := agentmodel.Wrap(err); ae.Type == agentmodel.ErrTypeRateLimit {
		t.Errorf("auth error must not be classified as rate_limit_error")
	}
	if p2.CallCount() != 0 {
		t.Errorf("non-rate-limit embed error should not trigger fallback, but p2 was called %d time(s)", p2.CallCount())
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// MessagesPassthrough
// ──────────────────────────────────────────────────────────────────────────────

func TestPool_MessagesPassthrough_FirstSucceeds(t *testing.T) {
	pt1 := &stub.Stub{MessagesPassthroughFn: passthroughResp(200, `{"id":"r1"}`)}
	pt2 := &stub.Stub{MessagesPassthroughFn: passthroughResp(200, `{"id":"r2"}`)}
	pl := pool.New([]provider.Provider{pt1, pt2})

	resp, err := pl.MessagesPassthrough(ctx(), []byte(`{}`), "m", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != `{"id":"r1"}` {
		t.Errorf("expected first provider response, got %s", b)
	}
	if pt2.CallCount() != 0 {
		t.Errorf("second provider should not have been called")
	}
}

func TestPool_MessagesPassthrough_429FallsToSecond(t *testing.T) {
	pt1 := &stub.Stub{MessagesPassthroughFn: passthroughResp(429, `{"error":"rate limited"}`)}
	pt2 := &stub.Stub{MessagesPassthroughFn: passthroughResp(200, `{"id":"r2"}`)}
	pl := pool.New([]provider.Provider{pt1, pt2})

	resp, err := pl.MessagesPassthrough(ctx(), []byte(`{}`), "m", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != `{"id":"r2"}` {
		t.Errorf("expected second provider response, got %s", b)
	}
}

func TestPool_MessagesPassthrough_AllExhausted(t *testing.T) {
	pt1 := &stub.Stub{MessagesPassthroughFn: passthroughResp(429, `{}`)}
	pt2 := &stub.Stub{MessagesPassthroughFn: passthroughResp(429, `{}`)}
	pl := pool.New([]provider.Provider{pt1, pt2})

	_, err := pl.MessagesPassthrough(ctx(), []byte(`{}`), "m", "")
	if err == nil {
		t.Fatal("expected error when all 429")
	}
}

// nonPassthrough implements provider.Provider but NOT provider.PassthroughProvider,
// so the type assertion in MessagesPassthrough fails and the loop skips it.
type nonPassthrough struct{}

func (n *nonPassthrough) Name() string              { return "non-passthrough" }
func (n *nonPassthrough) AuthMode() string          { return agentmodel.AuthModeAPIKey }
func (n *nonPassthrough) SupportedModels() []string { return []string{"m"} }
func (n *nonPassthrough) Complete(context.Context, agentmodel.ChatRequest) (agentmodel.ChatResponse, error) {
	return agentmodel.ChatResponse{}, nil
}
func (n *nonPassthrough) Stream(context.Context, agentmodel.ChatRequest) (iter.Seq2[provider.StreamChunk, error], error) {
	return nil, nil
}
func (n *nonPassthrough) Embed(context.Context, agentmodel.EmbeddingRequest) (agentmodel.EmbeddingResponse, error) {
	return agentmodel.EmbeddingResponse{}, nil
}

func TestPool_MessagesPassthrough_SkipsNonPassthroughProvider(t *testing.T) {
	// pool.go:116-118 — first provider does not implement PassthroughProvider,
	// so it is skipped and the passthrough-capable second provider serves.
	np := &nonPassthrough{}
	pt := &stub.Stub{MessagesPassthroughFn: passthroughResp(200, `{"id":"served"}`)}
	pl := pool.New([]provider.Provider{np, pt})

	resp, err := pl.MessagesPassthrough(ctx(), []byte(`{}`), "m", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != `{"id":"served"}` {
		t.Errorf("expected passthrough provider to serve, got %s", b)
	}
}

func TestPool_MessagesPassthrough_RateLimitErrorFallsToSecond(t *testing.T) {
	// pool.go:121-125 — err!=nil classified as rate-limit cools and continues.
	rlFn := func(context.Context, []byte, string, string) (*http.Response, error) {
		return nil, rateLimitErr
	}
	pt1 := &stub.Stub{MessagesPassthroughFn: rlFn}
	pt2 := &stub.Stub{MessagesPassthroughFn: passthroughResp(200, `{"id":"r2"}`)}
	pl := pool.New([]provider.Provider{pt1, pt2})

	resp, err := pl.MessagesPassthrough(ctx(), []byte(`{}`), "m", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != `{"id":"r2"}` {
		t.Errorf("expected second provider response after rate-limit, got %s", b)
	}
}

func TestPool_MessagesPassthrough_NilRespFallsToSecond(t *testing.T) {
	// pool.go MessagesPassthrough — a passthrough provider may signal "not
	// available" by returning (nil, nil). The pool must skip it (continue),
	// NOT dereference the nil *http.Response, and fall through to the next
	// provider. Mirrors the router's `if resp == nil { continue }` guard.
	nilFn := func(context.Context, []byte, string, string) (*http.Response, error) {
		return nil, nil
	}
	pt1 := &stub.Stub{MessagesPassthroughFn: nilFn}
	pt2 := &stub.Stub{MessagesPassthroughFn: passthroughResp(200, `{"id":"r2"}`)}
	pl := pool.New([]provider.Provider{pt1, pt2})

	resp, err := pl.MessagesPassthrough(ctx(), []byte(`{}`), "m", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response from second provider")
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != `{"id":"r2"}` {
		t.Errorf("expected second provider to serve after nil resp, got %s", b)
	}
}

func TestPool_MessagesPassthrough_AllNilExhausted(t *testing.T) {
	// When every passthrough provider returns (nil, nil), the pool must not
	// panic and must surface the "all exhausted" error rather than a nil resp.
	nilFn := func(context.Context, []byte, string, string) (*http.Response, error) {
		return nil, nil
	}
	pt1 := &stub.Stub{MessagesPassthroughFn: nilFn}
	pt2 := &stub.Stub{MessagesPassthroughFn: nilFn}
	pl := pool.New([]provider.Provider{pt1, pt2})

	resp, err := pl.MessagesPassthrough(ctx(), []byte(`{}`), "m", "")
	if err == nil {
		t.Fatal("expected error when all providers return nil resp")
	}
	if resp != nil {
		resp.Body.Close()
		t.Errorf("expected nil response when all exhausted, got %+v", resp)
	}
}

func TestPool_MessagesPassthrough_NonRateLimitErrorNotRetried(t *testing.T) {
	// pool.go:126-127 — err!=nil non-rate-limit is returned immediately.
	authFn := func(context.Context, []byte, string, string) (*http.Response, error) {
		return nil, authErr
	}
	pt1 := &stub.Stub{MessagesPassthroughFn: authFn}
	pt2 := &stub.Stub{MessagesPassthroughFn: passthroughResp(200, `{"id":"r2"}`)}
	pl := pool.New([]provider.Provider{pt1, pt2})

	resp, err := pl.MessagesPassthrough(ctx(), []byte(`{}`), "m", "")
	if err == nil {
		t.Fatal("expected error from first passthrough provider")
	}
	if resp != nil {
		t.Errorf("expected nil response on error, got %+v", resp)
	}
	if !errors.Is(err, authErr) {
		t.Errorf("expected original auth error to propagate, got %v", err)
	}
	if ae := agentmodel.Wrap(err); ae.Type == agentmodel.ErrTypeRateLimit {
		t.Errorf("auth error must not be classified as rate_limit_error")
	}
	if pt2.CallCount() != 0 {
		t.Errorf("non-rate-limit passthrough error must not fall through, but pt2 was called %d time(s)", pt2.CallCount())
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Constructor
// ──────────────────────────────────────────────────────────────────────────────

// ──────────────────────────────────────────────────────────────────────────────
// GenerateImage
// ──────────────────────────────────────────────────────────────────────────────

func TestPool_GenerateImage_FirstSucceeds(t *testing.T) {
	p1 := &stub.Stub{}
	p2 := &stub.Stub{}
	pl := pool.New([]provider.Provider{p1, p2})

	resp, err := pl.GenerateImage(ctx(), agentmodel.ImageRequest{Model: "m", Prompt: "a cat"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Data) == 0 {
		t.Fatal("expected image data")
	}
	if p1.CallCount() != 1 {
		t.Errorf("expected p1 called once, got %d", p1.CallCount())
	}
	if p2.CallCount() != 0 {
		t.Errorf("expected p2 not called, got %d", p2.CallCount())
	}
}

func TestPool_GenerateImage_RateLimitFallsToSecond(t *testing.T) {
	p1 := &stub.Stub{ImageErr: rateLimitErr}
	p2 := &stub.Stub{}
	pl := pool.New([]provider.Provider{p1, p2})

	resp, err := pl.GenerateImage(ctx(), agentmodel.ImageRequest{Model: "m", Prompt: "a cat"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Data) == 0 {
		t.Fatal("expected image data from second provider")
	}
	if p2.CallCount() != 1 {
		t.Errorf("expected p2 tried once")
	}
}

func TestPool_GenerateImage_AllRateLimited(t *testing.T) {
	p1 := &stub.Stub{ImageErr: rateLimitErr}
	p2 := &stub.Stub{ImageErr: rateLimitErr}
	pl := pool.New([]provider.Provider{p1, p2})

	_, err := pl.GenerateImage(ctx(), agentmodel.ImageRequest{Model: "m", Prompt: "a cat"})
	if err == nil {
		t.Fatal("expected error when all rate limited")
	}
}

func TestPool_GenerateImage_NonRateLimitErrorNotRetried(t *testing.T) {
	p1 := &stub.Stub{ImageErr: authErr}
	p2 := &stub.Stub{}
	pl := pool.New([]provider.Provider{p1, p2})

	_, err := pl.GenerateImage(ctx(), agentmodel.ImageRequest{Model: "m", Prompt: "a cat"})
	if err == nil {
		t.Fatal("expected error to propagate immediately")
	}
	if p2.CallCount() != 0 {
		t.Errorf("expected p2 not tried for a non-rate-limit error")
	}
}

// TestPool_GenerateImage_SkipsNonImageCapableProvider guards the same class of
// bug messagesbridge.Bridge had: Pool implements provider.Provider directly
// (not by embedding), so this loop — not Go's interface promotion — is what
// makes image generation work through a multi-credential pool at all.
func TestPool_GenerateImage_SkipsNonImageCapableProvider(t *testing.T) {
	notCapable := &nonPassthrough{}
	p2 := &stub.Stub{}
	pl := pool.New([]provider.Provider{notCapable, p2})

	resp, err := pl.GenerateImage(ctx(), agentmodel.ImageRequest{Model: "m", Prompt: "a cat"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Data) == 0 {
		t.Fatal("expected image data from the capable provider")
	}
	if p2.CallCount() != 1 {
		t.Errorf("expected p2 tried once")
	}
}

func TestPool_New_PanicsOnEmpty(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on empty providers")
		}
	}()
	pool.New(nil)
}

func TestPool_Metadata(t *testing.T) {
	p1 := &stub.Stub{NameValue: "groq", AuthModeValue: agentmodel.AuthModeSubscription}
	pl := pool.New([]provider.Provider{p1})
	if pl.AuthMode() != agentmodel.AuthModeSubscription {
		t.Errorf("expected subscription auth mode")
	}
	// Name must delegate to the underlying provider (not a literal "pool") so
	// cost pricing stays keyed "<provider>/<model>" — see Pool.Name.
	if pl.Name() != "groq" {
		t.Errorf("expected delegated name 'groq', got %q", pl.Name())
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// helpers
// ──────────────────────────────────────────────────────────────────────────────

func ctx() context.Context { return context.Background() }

func req(model string) agentmodel.ChatRequest {
	return agentmodel.ChatRequest{
		Model:    model,
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	}
}

func passthroughResp(statusCode int, body string) func(context.Context, []byte, string, string) (*http.Response, error) {
	return stub.PassthroughFunc(statusCode, body)
}

// TestPool_MessagesPassthrough_NoCapableProviderReturnsNotSupported: when NO
// inner provider implements passthrough, the pool must return the terminal
// provider.ErrNotSupported, not a retryable "all rate-limited" 429 for a request
// that can never succeed.
func TestPool_MessagesPassthrough_NoCapableProviderReturnsNotSupported(t *testing.T) {
	pl := pool.New([]provider.Provider{&nonPassthrough{}, &nonPassthrough{}})
	_, err := pl.MessagesPassthrough(ctx(), []byte(`{}`), "m", "")
	if !errors.Is(err, provider.ErrNotSupported) {
		t.Errorf("want provider.ErrNotSupported (terminal), got %v", err)
	}
	if ae := agentmodel.Wrap(err); ae.Retryable() {
		t.Errorf("capability miss must not be retryable, got type %q", ae.Type)
	}
}

// TestPool_GenerateImage_NoCapableProviderReturnsNotSupported: same for image gen.
func TestPool_GenerateImage_NoCapableProviderReturnsNotSupported(t *testing.T) {
	pl := pool.New([]provider.Provider{&nonPassthrough{}, &nonPassthrough{}})
	_, err := pl.GenerateImage(ctx(), agentmodel.ImageRequest{Model: "m", Prompt: "x"})
	if !errors.Is(err, provider.ErrNotSupported) {
		t.Errorf("want provider.ErrNotSupported (terminal), got %v", err)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// ListModels (provider.ModelLister)
// ──────────────────────────────────────────────────────────────────────────────

func TestPool_ListModels_ReturnsFirstSuccess(t *testing.T) {
	// A pool wraps N credentials of ONE provider, so the live catalog is the
	// same for each: the first credential that answers is authoritative and the
	// rest are left untouched.
	p1 := &stub.Stub{LiveModels: []string{"claude-sonnet-4-5", "claude-haiku-4-5"}}
	p2 := &stub.Stub{LiveModels: []string{"should-not-be-used"}}
	pl := pool.New([]provider.Provider{p1, p2})

	models, err := pl.ListModels(ctx())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 2 || models[0] != "claude-sonnet-4-5" {
		t.Errorf("models=%v, want the first credential's list", models)
	}
	if p2.ListModelsCalls() != 0 {
		t.Errorf("expected p2 untouched, got %d calls", p2.ListModelsCalls())
	}
}

func TestPool_ListModels_FallsToNextOnAnyError(t *testing.T) {
	// Discovery is best-effort and read-only: unlike the inference paths, ANY
	// error moves to the next credential (a broken or logged-out account must
	// not blank out drift detection for the whole deployment).
	p1 := &stub.Stub{ListModelsErr: authErr}
	p2 := &stub.Stub{LiveModels: []string{"claude-sonnet-4-5"}}
	pl := pool.New([]provider.Provider{p1, p2})

	models, err := pl.ListModels(ctx())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 1 || models[0] != "claude-sonnet-4-5" {
		t.Errorf("models=%v, want the second credential's list", models)
	}
}

func TestPool_ListModels_AllFailReturnsLastError(t *testing.T) {
	p1 := &stub.Stub{ListModelsErr: authErr}
	p2 := &stub.Stub{ListModelsErr: rateLimitErr}
	pl := pool.New([]provider.Provider{p1, p2})

	_, err := pl.ListModels(ctx())
	if err == nil {
		t.Fatal("expected an error when every credential fails")
	}
	if !errors.Is(err, rateLimitErr) {
		t.Errorf("err=%v, want the last credential's error", err)
	}
}

// TestPool_ListModels_NoCapableProviderReturnsNotSupported: a pool of providers
// that cannot list must report the terminal capability miss, matching
// MessagesPassthrough/GenerateImage. The router treats "not a ModelLister" as
// "skip drift checks", so this must not masquerade as a transient failure.
func TestPool_ListModels_NoCapableProviderReturnsNotSupported(t *testing.T) {
	pl := pool.New([]provider.Provider{&nonPassthrough{}, &nonPassthrough{}})
	_, err := pl.ListModels(ctx())
	if !errors.Is(err, provider.ErrNotSupported) {
		t.Errorf("want provider.ErrNotSupported (terminal), got %v", err)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Retry-After sized cooldowns
// ──────────────────────────────────────────────────────────────────────────────

func TestPool_CooldownLength(t *testing.T) {
	// One timeline covers both sizings. The hinted credential's 30m must outlast
	// the other's 5m default, and both must actually expire. Every credential
	// fails so the scan is forced to walk them: selection is sticky, so a
	// healthy credential would mask when a parked one becomes eligible again.
	clk := stub.NewClock()
	hinted := &stub.Stub{CompleteErr: stub.RateLimitAfter(30 * time.Minute)}
	defaulted := &stub.Stub{CompleteErr: rateLimitErr} // no hint => 5m default
	pl := pool.New([]provider.Provider{hinted, defaulted}, pool.WithNow(clk.Now))

	steps := []struct {
		desc          string
		advance       time.Duration
		wantHinted    int32
		wantDefaulted int32
	}{
		{"both tried, both parked", 0, 1, 1},
		{"inside both cooldowns", 4 * time.Minute, 1, 1},
		{"past the 5m default only", 2 * time.Minute, 1, 2},
		{"past the 30m hint", 25 * time.Minute, 2, 3},
	}
	for _, st := range steps {
		clk.Add(st.advance)
		if _, err := pl.Complete(ctx(), req("m")); err == nil {
			t.Fatalf("%s: expected exhaustion, every credential rate-limits", st.desc)
		}
		if hinted.CallCount() != st.wantHinted {
			t.Errorf("%s: hinted calls=%d, want %d", st.desc, hinted.CallCount(), st.wantHinted)
		}
		if defaulted.CallCount() != st.wantDefaulted {
			t.Errorf("%s: defaulted calls=%d, want %d", st.desc, defaulted.CallCount(), st.wantDefaulted)
		}
	}
}

func TestPool_MessagesPassthrough_RetryAfterHeaderSizesCooldown(t *testing.T) {
	// The passthrough path never sees a Go error for an upstream 429 — it gets
	// the live response — so it must read the header itself.
	clk := stub.NewClock()
	limited := &stub.Stub{MessagesPassthroughFn: stub.PassthroughFunc(
		http.StatusTooManyRequests, `{"error":"slow down"}`,
		http.Header{"Retry-After": []string{"1800"}})} // 30m
	capped := &stub.Stub{MessagesPassthroughFn: stub.PassthroughFunc(
		http.StatusTooManyRequests, `{"error":"slow down"}`)} // no header => 5m default
	pl := pool.New([]provider.Provider{limited, capped}, pool.WithNow(clk.Now))

	if _, err := pl.MessagesPassthrough(ctx(), []byte(`{}`), "m", ""); err == nil {
		t.Fatal("first call: expected exhaustion")
	}
	if limited.PassthroughCalls() != 1 || capped.PassthroughCalls() != 1 {
		t.Fatalf("first call: limited=%d capped=%d, want 1 and 1", limited.PassthroughCalls(), capped.PassthroughCalls())
	}

	// Past the 5m default, inside the 30m header: only the capped one retries.
	clk.Add(10 * time.Minute)
	if _, err := pl.MessagesPassthrough(ctx(), []byte(`{}`), "m", ""); err == nil {
		t.Fatal("second call: expected exhaustion")
	}
	if limited.PassthroughCalls() != 1 {
		t.Errorf("limited calls=%d, want 1 — the 30m Retry-After must outlive the 5m default", limited.PassthroughCalls())
	}
	if capped.PassthroughCalls() != 2 {
		t.Errorf("capped calls=%d, want 2 (its 5m default expired)", capped.PassthroughCalls())
	}

	// Past the header value: eligible again.
	clk.Add(21 * time.Minute)
	if _, err := pl.MessagesPassthrough(ctx(), []byte(`{}`), "m", ""); err == nil {
		t.Fatal("third call: expected exhaustion")
	}
	if limited.PassthroughCalls() != 2 {
		t.Errorf("limited calls=%d, want 2 (Retry-After expired)", limited.PassthroughCalls())
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Sticky selection (prompt-cache affinity)
// ──────────────────────────────────────────────────────────────────────────────

func TestPool_StaysOnTheCredentialThatWorked(t *testing.T) {
	// Prompt caches are per-account and never shared across subscriptions, so
	// every move to another credential costs one full prompt re-read. Once a
	// credential is serving, an earlier one becoming eligible again must NOT
	// pull traffic back — that would abandon a warm cache for a cold one.
	clk := stub.NewClock()
	p1 := &stub.Stub{CompleteErr: stub.RateLimitAfter(10 * time.Minute)}
	p2 := &stub.Stub{CompleteResp: okResp}
	pl := pool.New([]provider.Provider{p1, p2}, pool.WithNow(clk.Now))

	// p1 rate-limits, p2 serves: the pool is now on p2.
	if _, err := pl.Complete(ctx(), req("m")); err != nil {
		t.Fatalf("first call: %v", err)
	}
	afterFirst := p1.CallCount()

	// p1's cooldown expires. p2 is still healthy, so nothing should move.
	clk.Add(11 * time.Minute)
	for i := range 3 {
		if _, err := pl.Complete(ctx(), req("m")); err != nil {
			t.Fatalf("call %d: %v", i+2, err)
		}
	}
	if p1.CallCount() != afterFirst {
		t.Errorf("p1 calls=%d, want %d — traffic snapped back to a cold credential", p1.CallCount(), afterFirst)
	}
	if p2.CallCount() != 4 {
		t.Errorf("p2 calls=%d, want 4 (it should serve every call once selected)", p2.CallCount())
	}
}

func TestPool_WrapsBackWhenTheStickyCredentialFails(t *testing.T) {
	// Staying put must not strand a credential: when the one in use rate-limits,
	// the scan wraps around to an earlier, recovered credential.
	clk := stub.NewClock()
	p1 := &stub.Stub{CompleteErr: rateLimitErr}
	p2 := &stub.Stub{CompleteResp: okResp}
	pl := pool.New([]provider.Provider{p1, p2}, pool.WithNow(clk.Now))

	// Land on p2.
	if _, err := pl.Complete(ctx(), req("m")); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// p1 recovers, p2 exhausts.
	clk.Add(6 * time.Minute)
	p1.CompleteErr = nil
	p1.CompleteResp = okResp
	p2.CompleteErr = rateLimitErr
	before := p1.CallCount()

	if _, err := pl.Complete(ctx(), req("m")); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if p1.CallCount() != before+1 {
		t.Errorf("p1 calls=%d, want %d — the scan must wrap back to an earlier credential", p1.CallCount(), before+1)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Exhaustion carries the soonest retry
// ──────────────────────────────────────────────────────────────────────────────

func TestPool_ExhaustionCarriesSoonestRetryAfter(t *testing.T) {
	// The pool knows exactly when each credential frees up. Dropping that on the
	// floor makes the router park the whole deployment on its 5m default and
	// hands the client a 429 with no Retry-After — the very re-probe loop the
	// per-credential cooldowns exist to stop, one layer up.
	clk := stub.NewClock()
	slow := &stub.Stub{CompleteErr: stub.RateLimitAfter(30 * time.Minute)}
	soon := &stub.Stub{CompleteErr: stub.RateLimitAfter(10 * time.Minute)}
	pl := pool.New([]provider.Provider{slow, soon}, pool.WithNow(clk.Now))

	_, err := pl.Complete(ctx(), req("m"))
	if err == nil {
		t.Fatal("expected exhaustion")
	}
	ae := agentmodel.Wrap(err)
	if ae.Type != agentmodel.ErrTypeRateLimit {
		t.Fatalf("Type=%q, want %q", ae.Type, agentmodel.ErrTypeRateLimit)
	}
	if ae.RetryAfter != 10*time.Minute {
		t.Errorf("RetryAfter=%v, want 10m (the soonest of the two)", ae.RetryAfter)
	}

	// Time passes: the reported wait shrinks with the remaining cooldown.
	clk.Add(4 * time.Minute)
	_, err = pl.Complete(ctx(), req("m"))
	if err == nil {
		t.Fatal("expected exhaustion on the second call")
	}
	if got := agentmodel.Wrap(err).RetryAfter; got != 6*time.Minute {
		t.Errorf("RetryAfter=%v, want 6m (10m hint, 4m elapsed)", got)
	}
}

func TestPool_ExhaustionWithoutCooldownsHasNoHint(t *testing.T) {
	// Every credential capability-missed rather than rate-limited: nothing is
	// parked, so there is no honest answer to give.
	pl := pool.New([]provider.Provider{&nonPassthrough{}, &nonPassthrough{}})
	_, err := pl.MessagesPassthrough(ctx(), []byte(`{}`), "m", "")
	if got := agentmodel.Wrap(err).RetryAfter; got != 0 {
		t.Errorf("RetryAfter=%v, want 0", got)
	}
}
