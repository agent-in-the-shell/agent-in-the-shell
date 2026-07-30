// Package deepseek implements agentmodel/provider.Provider for DeepSeek's
// OpenAI-compatible API (https://api.deepseek.com).
//
// DeepSeek is wire-compatible with OpenAI's /chat/completions (including SSE
// streaming and the reasoning_content field returned by deepseek-reasoner), so
// chat + streaming delegate verbatim to the openai provider's shaping. The
// client reports its own identity and declines embeddings, which DeepSeek does
// not offer. It deliberately composes (not embeds) the openai client so it does
// NOT inherit image generation — DeepSeek is a chat-only provider.
package deepseek

import (
	"context"
	"iter"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/openai"
)

// defaultBaseURL is DeepSeek's OpenAI-compatible endpoint.
const defaultBaseURL = "https://api.deepseek.com/v1"

// Client is the DeepSeek provider implementation. Construct via New /
// NewWithBaseURL; the zero value is not usable.
type Client struct {
	oa *openai.Client
}

// New constructs a Client pointed at the public DeepSeek endpoint.
func New(authenticator auth.Authenticator) *Client {
	return NewWithBaseURL(authenticator, defaultBaseURL)
}

// NewWithBaseURL is like New but lets callers override the base URL (used for
// local mocks via httptest or proxies). The base URL must not end with a
// trailing slash.
func NewWithBaseURL(authenticator auth.Authenticator, baseURL string) *Client {
	return &Client{oa: openai.NewWithBaseURL(authenticator, baseURL)}
}

// Name returns the canonical provider id.
func (c *Client) Name() string { return "deepseek" }

// AuthMode returns the authentication mode (always api_key for this provider).
func (c *Client) AuthMode() string { return agentmodel.AuthModeAPIKey }

// SupportedModels returns the DeepSeek chat models the provider can dispatch.
// deepseek-chat is V3; deepseek-reasoner is R1 (returns reasoning_content
// alongside content). The router treats this as a hint — unknown models still
// forward if a deployment routes through this provider.
func (c *Client) SupportedModels() []string {
	return []string{"deepseek-chat", "deepseek-reasoner"}
}

// Complete performs a non-streaming chat completion via DeepSeek's
// OpenAI-compatible /chat/completions endpoint.
func (c *Client) Complete(ctx context.Context, req agentmodel.ChatRequest) (agentmodel.ChatResponse, error) {
	return c.oa.Complete(ctx, req)
}

// Stream performs a streaming chat completion (SSE).
func (c *Client) Stream(ctx context.Context, req agentmodel.ChatRequest) (iter.Seq2[provider.StreamChunk, error], error) {
	return c.oa.Stream(ctx, req)
}

// Embed is unsupported: DeepSeek has no embeddings API.
func (c *Client) Embed(_ context.Context, _ agentmodel.EmbeddingRequest) (agentmodel.EmbeddingResponse, error) {
	return agentmodel.EmbeddingResponse{}, provider.ErrNotSupported
}

// ListModels delegates to DeepSeek's OpenAI-compatible GET /models so the
// router can re-validate configured deployments against live availability.
// Implements provider.ModelLister.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	return c.oa.ListModels(ctx)
}

var (
	_ provider.Provider    = (*Client)(nil)
	_ provider.ModelLister = (*Client)(nil)
)
