// Package azure implements agentmodel/provider.Provider for Azure OpenAI (#892).
//
// Azure OpenAI is wire-compatible with OpenAI's /chat/completions (including SSE
// streaming and the usage shape), differing only in (1) the URL layout —
// deployment-scoped paths under /openai/deployments/<deployment> with a required
// ?api-version query — and (2) the auth header (api-key instead of
// Authorization: Bearer). Both differences are handled by openai.NewAzure and the
// caller's Authenticator, so chat + streaming + embeddings delegate verbatim to
// the openai provider's shaping. The client reports its own identity ("azure")
// for routing/observability, mirroring how deepseek composes the openai client.
package azure

import (
	"context"
	"iter"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/openai"
)

// Client is the Azure OpenAI provider implementation. Construct via New; the
// zero value is not usable.
type Client struct {
	oa *openai.Client
}

// New constructs a Client targeting an Azure OpenAI resource. resourceURL is the
// resource host (https://<resource>.openai.azure.com); apiVersion is the required
// Azure api-version (e.g. 2024-10-01-preview); deployment is the Azure deployment
// name that backs the logical model. Unlike other providers Azure has no public
// default endpoint — the resource URL is always required — so there is no
// New()/NewWithBaseURL() split.
func New(authenticator auth.Authenticator, resourceURL, apiVersion, deployment string) *Client {
	return &Client{oa: openai.NewAzure(authenticator, resourceURL, apiVersion, deployment)}
}

// Name returns the canonical provider id.
func (c *Client) Name() string { return "azure" }

// AuthMode returns the authentication mode (always api_key for this provider).
func (c *Client) AuthMode() string { return agentmodel.AuthModeAPIKey }

// SupportedModels reports the OpenAI models Azure commonly hosts, delegating to
// the composed openai client (Azure hosts the same catalog). Azure routes by
// deployment name (set per config), so this is only a non-exhaustive hint — the
// router forwards any configured model through this provider regardless.
func (c *Client) SupportedModels() []string {
	return c.oa.SupportedModels()
}

// Complete performs a non-streaming chat completion via Azure's deployment-scoped
// /chat/completions endpoint.
func (c *Client) Complete(ctx context.Context, req agentmodel.ChatRequest) (agentmodel.ChatResponse, error) {
	return c.oa.Complete(ctx, req)
}

// Stream performs a streaming chat completion (SSE).
func (c *Client) Stream(ctx context.Context, req agentmodel.ChatRequest) (iter.Seq2[provider.StreamChunk, error], error) {
	return c.oa.Stream(ctx, req)
}

// Embed performs an embedding request via Azure's deployment-scoped /embeddings.
func (c *Client) Embed(ctx context.Context, req agentmodel.EmbeddingRequest) (agentmodel.EmbeddingResponse, error) {
	return c.oa.Embed(ctx, req)
}

// ListModels delegates to Azure's account-scoped GET /openai/models so the router
// can re-validate configured deployments. Implements provider.ModelLister.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	return c.oa.ListModels(ctx)
}

var (
	_ provider.Provider    = (*Client)(nil)
	_ provider.ModelLister = (*Client)(nil)
)
