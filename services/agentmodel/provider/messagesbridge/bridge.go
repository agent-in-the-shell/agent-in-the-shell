// Package messagesbridge adapts an OpenAI/Chat-Completions-shaped
// provider.Provider so it can serve Anthropic Messages API (POST /v1/messages)
// traffic. It implements provider.PassthroughProvider by translating an
// Anthropic Messages request into an agentmodel.ChatRequest, invoking the
// wrapped provider's Complete/Stream, and synthesizing an Anthropic-shaped
// HTTP response (JSON for non-streaming, SSE for streaming).
//
// This lets Anthropic-SDK clients (e.g. pi-mom / Claude Code) target providers
// that only speak OpenAI/Responses — notably the chatgpt (ChatGPT/codex)
// subscription provider, which otherwise has no /v1/messages path.
//
// Scope: text and tool_use/tool_result content only. Image and document blocks
// and extended-thinking blocks are not translated — the providers this targets
// accept text + function tools, so such content is dropped during conversion.
package messagesbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
)

// Bridge wraps a provider.Provider, delegating the standard Provider methods to
// it (so /v1/chat/completions and /v1/embeddings keep working unchanged) while
// additionally implementing provider.PassthroughProvider for /v1/messages.
//
// Note: embedding the interface promotes only the Provider methods. Optional
// capability interfaces (e.g. provider.ImageGenerator) are NOT promoted and
// need an explicit forwarding method on Bridge — GenerateImage below is one
// such forwarder, added when chatgpt gained image generation. Add more here
// as wrapped providers gain more optional capabilities.
//
// Bridge is the OUTERMOST wrapper for pooled chatgpt (factory builds
// messagesbridge.New(pool.New(...))), so it masks any capability the pool
// forwards but Bridge does not — keep the two in step.
type Bridge struct {
	provider.Provider
}

// New returns a Bridge wrapping inner.
func New(inner provider.Provider) *Bridge {
	return &Bridge{Provider: inner}
}

// Compile-time checks: a Bridge is still a Provider and a PassthroughProvider,
// plus every optional capability it forwards (see GenerateImage / ListModels
// below). Adding an entry here without its forwarder fails the build — which is
// the point, since a MISSING entry fails nothing at all and the capability is
// silently erased for every wrapped provider.
var (
	_ provider.Provider                     = (*Bridge)(nil)
	_ provider.PassthroughProvider          = (*Bridge)(nil)
	_ provider.ResponsesPassthroughProvider = (*Bridge)(nil)
	_ provider.ImageGenerator               = (*Bridge)(nil)
	_ provider.ModelLister                  = (*Bridge)(nil)
)

// ResponsesPassthrough preserves the wrapped provider's raw Responses
// capability; Bridge must forward it because factory places Bridge outermost.
func (b *Bridge) ResponsesPassthrough(ctx context.Context, body []byte, modelOverride string) (*http.Response, error) {
	pp, ok := b.Provider.(provider.ResponsesPassthroughProvider)
	if !ok {
		return nil, agentmodel.NewErrorf(agentmodel.ErrTypeInvalidRequest,
			"messagesbridge: wrapped provider does not support Responses passthrough")
	}
	return pp.ResponsesPassthrough(ctx, body, modelOverride)
}

// GenerateImage implements provider.ImageGenerator by forwarding to the
// wrapped provider when it supports image generation. Bridge itself always
// satisfies ImageGenerator — embedding provider.Provider does not promote
// optional capability interfaces (see the type doc above), so without this
// forwarder the router's type assertion for image-capable deployments would
// silently fail for every bridge-wrapped provider, including chatgpt. A
// wrapped provider that doesn't implement ImageGenerator surfaces an
// invalid_request error here instead of the router's generic "no
// image-capable deployment" message.
func (b *Bridge) GenerateImage(ctx context.Context, req agentmodel.ImageRequest) (agentmodel.ImageResponse, error) {
	gen, ok := b.Provider.(provider.ImageGenerator)
	if !ok {
		return agentmodel.ImageResponse{}, agentmodel.NewErrorf(agentmodel.ErrTypeInvalidRequest,
			"messagesbridge: wrapped provider does not support image generation")
	}
	return gen.GenerateImage(ctx, req)
}

// ListModels implements provider.ModelLister by forwarding to the wrapped
// provider. Bridge is the outermost wrapper for a pooled chatgpt deployment
// (the factory builds messagesbridge.New(pool.New(...))), so without this the
// pool's own ModelLister forward is erased and the deployment drops out of the
// router's drift detection — the exact silent-skip this capability was wired
// through the pool to fix.
func (b *Bridge) ListModels(ctx context.Context) ([]string, error) {
	lister, ok := b.Provider.(provider.ModelLister)
	if !ok {
		return nil, agentmodel.NewErrorf(agentmodel.ErrTypeInvalidRequest,
			"messagesbridge: wrapped provider does not support model listing")
	}
	return lister.ListModels(ctx)
}

// MessagesPassthrough implements provider.PassthroughProvider. It converts the
// Anthropic Messages request in body to an agentmodel.ChatRequest (rewriting
// the model to modelOverride), runs it through the wrapped provider, and
// returns an Anthropic-shaped *http.Response. clientBetas is ignored — the
// target providers do not honor Anthropic beta headers.
func (b *Bridge) MessagesPassthrough(ctx context.Context, body []byte, modelOverride, _ string) (*http.Response, error) {
	req, err := anthropicToChatRequest(body, modelOverride)
	if err != nil {
		return nil, agentmodel.NewErrorf(agentmodel.ErrTypeInvalidRequest, "messagesbridge: %v", err)
	}

	if req.Stream {
		// Establish the stream eagerly so a non-2xx upstream status surfaces as
		// an error here (letting the router fall back / cool down) rather than
		// after we have already returned a 200 SSE response.
		seq, err := b.Provider.Stream(ctx, req)
		if err != nil {
			return nil, err
		}
		pr, pw := io.Pipe()
		go func() {
			// run() returns non-nil only when writing to the pipe fails (the
			// reader went away); iterator errors are surfaced to the client as
			// an Anthropic error event. Either way the body is complete.
			_ = newStreamWriter(pw, req.Model).run(seq)
			_ = pw.Close()
		}()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       pr,
		}, nil
	}

	resp, err := b.Provider.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(chatResponseToAnthropic(resp, req.Model))
	if err != nil {
		return nil, agentmodel.NewErrorf(agentmodel.ErrTypeUpstream, "messagesbridge: encode response: %v", err)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(payload)),
	}, nil
}
