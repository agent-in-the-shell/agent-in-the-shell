// Package replicate implements a transparent reverse proxy to Replicate.com for
// the agentmodel gateway. Unlike the typed providers (openai, gemini, …) it does
// NOT normalize to the gateway's OpenAI-shaped types; it forwards Replicate's own
// prediction wire protocol and lets the handler parse the response out-of-band
// for attribution. It is the Replicate analog of the Anthropic /v1/messages
// passthrough.
//
// Auth: the gateway authenticates the CLIENT via its own bearer/virtual key
// (the api.Server's bearerAuth middleware), then this provider injects the
// gateway's configured upstream Replicate credential — REPLICATE_API_TOKEN as
// "Authorization: Bearer <token>" — replacing whatever Authorization the client
// sent. So a redirected consumer points REPLICATE_BASE_URL at the gateway and
// sets its Replicate token env to the gateway token; no upstream secret leaves
// the gateway.
//
// This client deliberately uses only the Go standard library.
package replicate

import (
	"bytes"
	"context"
	"io"
	"iter"
	"net/http"
	"strings"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
)

const defaultBaseURL = "https://api.replicate.com"

// forwardableClientHeaders is the allowlist of inbound client headers copied to
// the upstream request. Authorization is deliberately excluded — the gateway
// injects its own credential. Host and hop-by-hop headers are excluded too.
// Prefer carries Replicate's synchronous "wait" semantics, which must survive.
var forwardableClientHeaders = []string{
	"Content-Type",
	"Accept",
	"Prefer",
}

// Client is the Replicate reverse-proxy provider implementation.
type Client struct {
	auth    auth.Authenticator
	baseURL string
	http    *http.Client
}

// New constructs a Client targeting the public Replicate API.
func New(authenticator auth.Authenticator) *Client {
	return NewWithBaseURL(authenticator, defaultBaseURL)
}

// NewWithBaseURL constructs a Client targeting baseURL (useful for tests).
// baseURL is the upstream host root WITHOUT a trailing slash or version segment;
// the full Replicate path (e.g. /v1/predictions) is supplied per Forward call.
func NewWithBaseURL(authenticator auth.Authenticator, baseURL string) *Client {
	return &Client{
		auth:    authenticator,
		baseURL: strings.TrimRight(baseURL, "/"),
		// Video / lip-sync predictions plus Prefer: wait can hold the connection
		// for tens of seconds; a generous timeout matches the other providers.
		http: &http.Client{Timeout: 5 * time.Minute},
	}
}

// Name returns the canonical provider identifier.
func (c *Client) Name() string { return "replicate" }

// AuthMode reports the credential mode in use (always api_key for Replicate).
func (c *Client) AuthMode() string {
	if c.auth == nil {
		return agentmodel.AuthModeAPIKey
	}
	return c.auth.Mode()
}

// SupportedModels returns nil: the passthrough's model is whatever the request
// body names (e.g. "minimax/speech-02-turbo" or a version hash), not a fixed
// list. Dispatch keys off the configured deployment, not this enumeration.
func (c *Client) SupportedModels() []string { return nil }

// Complete is not supported: Replicate is a prediction API, not a chat backend.
func (c *Client) Complete(context.Context, agentmodel.ChatRequest) (agentmodel.ChatResponse, error) {
	return agentmodel.ChatResponse{}, provider.ErrNotSupported
}

// Stream is not supported (see Complete).
func (c *Client) Stream(context.Context, agentmodel.ChatRequest) (iter.Seq2[provider.StreamChunk, error], error) {
	return nil, provider.ErrNotSupported
}

// Embed is not supported (see Complete).
func (c *Client) Embed(context.Context, agentmodel.EmbeddingRequest) (agentmodel.EmbeddingResponse, error) {
	return agentmodel.EmbeddingResponse{}, provider.ErrNotSupported
}

// Forward proxies a raw Replicate request to the upstream. It copies an
// allowlisted subset of the client's headers, injects the gateway's own
// Replicate credential (replacing any client Authorization), and returns the
// upstream response AS-IS — including non-2xx — so the caller copies status +
// body verbatim. The caller MUST close the returned response body.
//
// Implements provider.ReplicateProxy.
func (c *Client) Forward(ctx context.Context, method, path string, body []byte, clientHeaders http.Header) (*http.Response, error) {
	var rdr io.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	for _, h := range forwardableClientHeaders {
		if v := clientHeaders.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	if c.auth != nil {
		if err := c.auth.Apply(ctx, req); err != nil {
			return nil, err
		}
	}
	return c.http.Do(req)
}

// compile-time assertions.
var (
	_ provider.Provider       = (*Client)(nil)
	_ provider.ReplicateProxy = (*Client)(nil)
)
