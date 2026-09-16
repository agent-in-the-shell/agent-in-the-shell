// Package stub provides an in-memory Provider implementation for testing
// router, API handlers, and integration scenarios without any network I/O.
package stub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
)

// Stub is a configurable provider for tests.
type Stub struct {
	NameValue             string
	AuthModeValue         string
	Models                []string
	CompleteResp          agentmodel.ChatResponse
	StreamChunks          []provider.StreamChunk
	EmbedResp             agentmodel.EmbeddingResponse
	ImageResp             agentmodel.ImageResponse
	CompleteErr           error // if set, Complete returns this error
	StreamErr             error // if set, Stream returns this error from .Stream() OR yields it as the first chunk's error
	EmbedErr              error
	ImageErr              error // if set, GenerateImage returns this error
	StreamYieldErr        error // if set, the first stream chunk yields this error (after StreamChunks[0]?)
	StreamDelay           time.Duration
	MessagesPassthroughFn func(context.Context, []byte, string, string) (*http.Response, error)
	LiveModels            []string // returned by ListModels (provider.ModelLister)
	ListModelsErr         error    // if set, ListModels returns this error

	// All three counters are read and written with sync/atomic, so there is no
	// mutex here on purpose — adding one would suggest a lock discipline the
	// accessors do not have.
	callCount        int32
	listModelsCount  int32
	passthroughCount int32
}

// CallCount returns how many times Complete or Stream has been invoked.
// Useful for asserting fallback / retry behavior.
func (s *Stub) CallCount() int32 { return atomic.LoadInt32(&s.callCount) }

func (s *Stub) Name() string {
	if s.NameValue == "" {
		return "stub"
	}
	return s.NameValue
}

func (s *Stub) AuthMode() string {
	if s.AuthModeValue == "" {
		return agentmodel.AuthModeAPIKey
	}
	return s.AuthModeValue
}

func (s *Stub) SupportedModels() []string {
	if s.Models == nil {
		return []string{"stub-model"}
	}
	return s.Models
}

var _ provider.ModelLister = (*Stub)(nil)

// ListModels implements provider.ModelLister for tests. It returns the current
// LiveModels/ListModelsErr (both mutable between calls so tests can simulate a
// provider that changes its list or goes offline) and counts each invocation
// so tests can assert the TTL cache deduplicates fetches.
func (s *Stub) ListModels(_ context.Context) ([]string, error) {
	atomic.AddInt32(&s.listModelsCount, 1)
	return s.LiveModels, s.ListModelsErr
}

// ListModelsCalls returns how many times ListModels has been invoked — used to
// assert the router's TTL cache reuses a fetch instead of re-querying.
func (s *Stub) ListModelsCalls() int32 { return atomic.LoadInt32(&s.listModelsCount) }

func (s *Stub) Complete(_ context.Context, req agentmodel.ChatRequest) (agentmodel.ChatResponse, error) {
	atomic.AddInt32(&s.callCount, 1)
	if s.CompleteErr != nil {
		return agentmodel.ChatResponse{}, s.CompleteErr
	}
	resp := s.CompleteResp
	if resp.ID == "" {
		resp = defaultStubResponse(req)
	}
	return resp, nil
}

func (s *Stub) Stream(ctx context.Context, req agentmodel.ChatRequest) (iter.Seq2[provider.StreamChunk, error], error) {
	atomic.AddInt32(&s.callCount, 1)
	if s.StreamErr != nil {
		return nil, s.StreamErr
	}

	chunks := s.StreamChunks
	if chunks == nil {
		chunks = defaultStubStream(req)
	}
	delay := s.StreamDelay
	yieldErr := s.StreamYieldErr

	seq := func(yield func(provider.StreamChunk, error) bool) {
		for i, ch := range chunks {
			if delay > 0 {
				select {
				case <-ctx.Done():
					yield(provider.StreamChunk{}, ctx.Err())
					return
				case <-time.After(delay):
				}
			}
			if !yield(ch, nil) {
				return
			}
			if yieldErr != nil && i == 0 {
				if !yield(provider.StreamChunk{}, yieldErr) {
					return
				}
			}
		}
	}
	return seq, nil
}

func (s *Stub) Embed(_ context.Context, _ agentmodel.EmbeddingRequest) (agentmodel.EmbeddingResponse, error) {
	atomic.AddInt32(&s.callCount, 1)
	if s.EmbedErr != nil {
		return agentmodel.EmbeddingResponse{}, s.EmbedErr
	}
	if len(s.EmbedResp.Data) > 0 {
		return s.EmbedResp, nil
	}
	return agentmodel.EmbeddingResponse{
		Object: "list",
		Model:  "stub-embed",
		Data: []agentmodel.Embedding{
			{Object: "embedding", Index: 0, Embedding: []float64{0.1, 0.2, 0.3}},
		},
		Usage: agentmodel.Usage{PromptTokens: 5, TotalTokens: 5},
	}, nil
}

var _ provider.ImageGenerator = (*Stub)(nil)

// GenerateImage implements provider.ImageGenerator for tests. It returns the
// configured ImageResp/ImageErr, or a deterministic default image otherwise.
func (s *Stub) GenerateImage(_ context.Context, req agentmodel.ImageRequest) (agentmodel.ImageResponse, error) {
	atomic.AddInt32(&s.callCount, 1)
	if s.ImageErr != nil {
		return agentmodel.ImageResponse{}, s.ImageErr
	}
	if len(s.ImageResp.Data) > 0 {
		return s.ImageResp, nil
	}
	return agentmodel.ImageResponse{
		Created: time.Now().Unix(),
		Model:   req.Model,
		Data:    []agentmodel.ImageData{{B64JSON: "c3R1Yi1pbWFnZQ=="}}, // "stub-image"
		Usage:   agentmodel.Usage{PromptTokens: 7, CompletionTokens: 100, TotalTokens: 107},
	}, nil
}

func defaultStubResponse(req agentmodel.ChatRequest) agentmodel.ChatResponse {
	return agentmodel.ChatResponse{
		ID:      "stub-" + req.Model,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []agentmodel.Choice{
			{
				Index:        0,
				Message:      agentmodel.Message{Role: "assistant", Content: "stub response"},
				FinishReason: "stop",
			},
		},
		Usage: agentmodel.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}
}

func defaultStubStream(req agentmodel.ChatRequest) []provider.StreamChunk {
	_ = req
	return []provider.StreamChunk{
		{Delta: agentmodel.Message{Role: "assistant", Content: "stub "}},
		{Delta: agentmodel.Message{Content: "response"}},
		{
			Delta:        agentmodel.Message{},
			FinishReason: "stop",
			Usage:        &agentmodel.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		},
	}
}

// PassthroughCalls returns how many times MessagesPassthrough has been invoked.
// Separate from CallCount, which covers the Complete/Stream/Embed paths.
func (s *Stub) PassthroughCalls() int32 { return atomic.LoadInt32(&s.passthroughCount) }

func (s *Stub) MessagesPassthrough(ctx context.Context, body []byte, modelOverride, clientBetas string) (*http.Response, error) {
	atomic.AddInt32(&s.passthroughCount, 1)
	if s.MessagesPassthroughFn != nil {
		return s.MessagesPassthroughFn(ctx, body, modelOverride, clientBetas)
	}
	return nil, nil
}

// PassthroughFunc returns a MessagesPassthroughFn that responds with the given
// HTTP status code and body, plus optional response headers (a 429's
// Retry-After, say). Shared between pool_test, router_test, and the api tests.
func PassthroughFunc(statusCode int, body string, header ...http.Header) func(context.Context, []byte, string, string) (*http.Response, error) {
	h := http.Header{}
	for _, extra := range header {
		for k, vs := range extra {
			h[k] = vs
		}
	}
	return func(_ context.Context, _ []byte, _ string, _ string) (*http.Response, error) {
		return &http.Response{
			StatusCode: statusCode,
			Header:     h.Clone(),
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	}
}

// Clock is a manually-advanced time source for cooldown/TTL tests, shared by
// the pool and router suites.
type Clock struct{ t time.Time }

// NewClock returns a Clock at a fixed, arbitrary instant.
func NewClock() *Clock { return &Clock{t: time.Unix(1_700_000_000, 0)} }

// Now returns the current fake time; pass it as the WithNow option.
func (c *Clock) Now() time.Time { return c.t }

// Add advances the fake clock.
func (c *Clock) Add(d time.Duration) { c.t = c.t.Add(d) }

// Errors a test stub commonly raises to drive routing/fallback paths.
var (
	ErrTestRateLimit = fmt.Errorf("stub: rate limited")
	ErrTestAuth      = errors.New("stub: auth failed")
)

// RateLimitAfter returns a rate-limit error carrying the upstream's own "come
// back at" — the shape a provider produces from a 429 that named its reset
// window. Satisfies wire.RetryAfterHinter structurally, so agentmodel.Wrap
// copies d onto the classified error's RetryAfter.
func RateLimitAfter(d time.Duration) error {
	return agentmodel.WithRetryAfter(errors.New("stub: 429 rate limited"), d)
}
