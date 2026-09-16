// Package provider defines the contract every LLM provider implements.
//
// Providers translate to and from the OpenAI-shaped types in agentmodel,
// handle their own HTTP / SDK calls, and surface streaming via a Go iterator.
package provider

import (
	"context"
	"errors"
	"io"
	"iter"
	"net/http"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
)

// Provider is the gateway-internal interface every LLM vendor implements.
type Provider interface {
	// Name is the canonical identifier (e.g. "openai", "anthropic", "gemini",
	// "chatgpt"). Used for routing + observability.
	Name() string

	// SupportedModels returns the model identifiers this provider can dispatch.
	// May overlap across providers (e.g. multiple "claude-3-5-sonnet-latest"
	// deployments differ only in auth mode).
	SupportedModels() []string

	// AuthMode is "api_key" or "subscription".
	AuthMode() string

	// Complete executes a non-streaming chat-completion call.
	Complete(ctx context.Context, req agentmodel.ChatRequest) (agentmodel.ChatResponse, error)

	// Stream executes a streaming chat-completion call. The returned iterator
	// yields chunks until exhausted; the final chunk's Usage (if non-nil)
	// holds the total token counts. Iteration stops on any non-nil error.
	Stream(ctx context.Context, req agentmodel.ChatRequest) (iter.Seq2[StreamChunk, error], error)

	// Embed returns embedding vectors for the supplied input.
	Embed(ctx context.Context, req agentmodel.EmbeddingRequest) (agentmodel.EmbeddingResponse, error)
}

// StreamChunk is one increment of a streaming completion. Delta carries the
// new content (role, partial text, partial tool call, etc); FinishReason and
// Usage are populated only on the terminal chunk.
//
// Delta.ThinkingBlocks is the exception to "partial": a provider must only emit
// complete blocks, signatures attached, never fragments — the Anthropic provider
// holds them back to a terminal chunk for exactly this reason. Consumers
// therefore accumulate them by appending, so a provider that streamed them
// incrementally would produce a fragmented, unreplayable reassembly.
type StreamChunk struct {
	Delta        agentmodel.Message
	FinishReason string
	Usage        *agentmodel.Usage
}

// ModelLister is an optional interface implemented by providers that can
// enumerate the models their upstream API currently offers (OpenAI
// GET /v1/models, Anthropic GET /v1/models, Gemini GET /v1beta/models). It is
// used to validate configured deployments against live provider availability.
// Providers that do not implement it are simply skipped during validation;
// the upstream lists are a best-effort aid, never a source of truth.
type ModelLister interface {
	// ListModels returns the bare model ids the upstream currently offers.
	ListModels(ctx context.Context) ([]string, error)
}

// ImageGenerator is an optional interface implemented by providers that can
// synthesize images from a text prompt. The /v1/images/generations endpoint
// requires it; providers that only implement Provider are not eligible for the
// image-generation path and the router surfaces an invalid_request error for
// such models. Both an OpenAI-native (/images/generations) and a Gemini-native
// (generateContent with an IMAGE response modality) provider satisfy this one
// interface — the normalization to ImageRequest/ImageResponse is the unifying
// point, regardless of the upstream's wire shape.
type ImageGenerator interface {
	// GenerateImage executes a text-to-image request and returns the generated
	// image(s). Implementations populate ImageResponse.Usage on a best-effort
	// basis (some upstreams report no usage).
	GenerateImage(ctx context.Context, req agentmodel.ImageRequest) (agentmodel.ImageResponse, error)
}

// VideoGenerator is an optional interface implemented by providers that can
// generate video. Every video API is asynchronous — submit a job, then poll —
// so unlike the synchronous ImageGenerator it is modeled as two calls. The
// /v1/videos surface requires this interface; providers that don't implement it
// are skipped by the router's video dispatch.
type VideoGenerator interface {
	// SubmitVideo starts an asynchronous generation job and returns an operation
	// handle whose ID is the provider's own operation identifier — the token
	// PollVideo accepts. Status is typically queued or running.
	SubmitVideo(ctx context.Context, req agentmodel.GenerateVideoRequest) (agentmodel.VideoOperation, error)

	// PollVideo reports the current state of a job previously started by
	// SubmitVideo, keyed by the provider operation id from that call's
	// VideoOperation.ID. On Status == succeeded the result carries the video(s).
	PollVideo(ctx context.Context, providerOpID string) (agentmodel.VideoOperation, error)
}

// VideoDownloader is an optional interface for VideoGenerator providers whose
// result asset URLs require the gateway's provider credential to fetch (Veo's
// do). The gateway proxies GET /v1/videos/{id}/content through this so a client
// — which holds only the gateway bearer, not the upstream credential — can
// retrieve the bytes instead of receiving a URL it would 401 on.
type VideoDownloader interface {
	// DownloadVideo fetches the asset at assetURL using the provider's
	// credential and returns a stream of its bytes plus the content type. The
	// caller closes the reader.
	DownloadVideo(ctx context.Context, assetURL string) (io.ReadCloser, string, error)
}

// PassthroughProvider is an optional interface implemented by providers that
// support native Anthropic Messages API passthrough. The /v1/messages endpoint
// requires this interface; generic providers that only implement Provider are
// not eligible for the passthrough path.
type PassthroughProvider interface {
	// MessagesPassthrough forwards a pre-formed Anthropic Messages JSON body to
	// the upstream and returns the raw HTTP response. The caller MUST close the
	// response body. modelOverride rewrites the body's "model" field if non-empty.
	MessagesPassthrough(ctx context.Context, body []byte, modelOverride, clientBetas string) (*http.Response, error)
}

// ResponsesPassthroughProvider is an optional interface implemented by providers
// that expose an OpenAI Responses API without translating it through Chat
// Completions. The gateway uses it for hosted tools whose wire events cannot be
// represented by the normalized Provider stream (for example web_search).
type ResponsesPassthroughProvider interface {
	// ResponsesPassthrough forwards a Responses request using the provider's own
	// credential. modelOverride replaces the caller's logical model. The caller
	// must close the returned response body.
	ResponsesPassthrough(ctx context.Context, body []byte, modelOverride string) (*http.Response, error)
}

// ReplicateProxy is an optional interface implemented by providers that proxy
// raw Replicate predictions. The /v1/predictions surface (and the model-based
// /v1/models/{owner}/{name}/predictions form) requires it; providers that only
// implement Provider are not eligible for the Replicate passthrough path.
//
// Unlike the typed ImageGenerator/VideoGenerator interfaces, this is a
// transparent reverse proxy: the gateway forwards Replicate's own wire protocol
// and parses the prediction JSON only out-of-band for attribution. It is the
// analog of PassthroughProvider for a non-Anthropic upstream.
type ReplicateProxy interface {
	// Forward proxies a raw Replicate request (the caller supplies method, the
	// path under the upstream host, the request body, and the inbound client
	// headers) and returns the upstream HTTP response. The caller MUST close the
	// response body. Implementations apply the gateway's own upstream credential
	// (replacing any client Authorization header) and return the response as-is —
	// including non-2xx — so the handler can copy status + body verbatim.
	Forward(ctx context.Context, method, path string, body []byte, clientHeaders http.Header) (*http.Response, error)
}

// ErrNotSupported indicates the provider does not implement an operation
// (e.g. embeddings on a chat-only provider).
var ErrNotSupported = errors.New("agentmodel/provider: operation not supported by this provider")
