// Package openaicompat implements agentmodel/provider.Provider for any vendor
// that speaks OpenAI's /chat/completions wire format — Groq, Mistral, Together,
// xAI (Grok), OpenRouter, Fireworks, Perplexity, DeepInfra, Nebius, and any
// other OpenAI-compatible endpoint. It composes the openai client (so chat,
// streaming, embeddings, and GET /models delegate verbatim) but reports a
// configurable provider name, making each vendor a first-class provider (its
// own observability label + pricing key) without a bespoke per-vendor file.
package openaicompat

import (
	"context"
	"iter"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/openai"
)

// Client is a generic OpenAI-wire-compatible provider with a configurable name
// and base URL. Construct via New; the zero value is not usable.
type Client struct {
	oa   *openai.Client
	name string
}

// New constructs a Client for the named provider at baseURL (its OpenAI-compatible
// endpoint, e.g. https://api.groq.com/openai/v1). baseURL must not end with a
// trailing slash. Auth is whatever the factory supplies — Bearer by default.
func New(name, baseURL string, authenticator auth.Authenticator) *Client {
	return &Client{oa: openai.NewWithBaseURL(authenticator, baseURL), name: name}
}

// Name returns the configured provider id (e.g. "groq"), so the ledger, metrics,
// and price keys attribute to the actual vendor rather than "openai".
func (c *Client) Name() string { return c.name }

// AuthMode is always api_key — these providers authenticate with a Bearer key.
func (c *Client) AuthMode() string { return agentmodel.AuthModeAPIKey }

// SupportedModels returns nil: the model set is deployment-driven (config names
// the upstream model), and the router treats SupportedModels only as a hint.
func (c *Client) SupportedModels() []string { return nil }

// Complete performs a non-streaming chat completion.
func (c *Client) Complete(ctx context.Context, req agentmodel.ChatRequest) (agentmodel.ChatResponse, error) {
	return c.oa.Complete(ctx, req)
}

// Stream performs a streaming chat completion (SSE).
func (c *Client) Stream(ctx context.Context, req agentmodel.ChatRequest) (iter.Seq2[provider.StreamChunk, error], error) {
	return c.oa.Stream(ctx, req)
}

// Embed delegates to the OpenAI-compatible /embeddings endpoint. Vendors without
// embeddings (Groq, xAI, …) return an upstream error, surfaced to the caller.
func (c *Client) Embed(ctx context.Context, req agentmodel.EmbeddingRequest) (agentmodel.EmbeddingResponse, error) {
	return c.oa.Embed(ctx, req)
}

// ListModels delegates to GET /models so the router can re-validate deployments.
// Implements provider.ModelLister.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	return c.oa.ListModels(ctx)
}

var (
	_ provider.Provider    = (*Client)(nil)
	_ provider.ModelLister = (*Client)(nil)
)
