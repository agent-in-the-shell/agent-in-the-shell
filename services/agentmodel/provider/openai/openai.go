// Package openai implements the agentmodel/provider.Provider interface for
// OpenAI's REST API (api.openai.com/v1) using API-key auth.
//
// The package keeps the wire types identical to OpenAI's documented JSON shape
// (matching agentmodel.ChatRequest / ChatResponse exactly) so that
// transformation cost is near zero. Streaming is parsed as SSE without third-
// party SDKs; only stdlib + iter (Go 1.23+) are used.
package openai

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
)

const (
	defaultBaseURL = "https://api.openai.com/v1"
	defaultTimeout = 5 * time.Minute
)

// Client is the OpenAI provider implementation. Construct via New /
// NewWithBaseURL / NewAzure; the zero value is not usable.
type Client struct {
	auth    auth.Authenticator
	baseURL string
	http    *http.Client

	// Azure OpenAI mode. The zero value is standard OpenAI. When azure is
	// true, resolveURL rewrites each path to Azure's deployment-scoped layout and
	// appends the required ?api-version query param; everything else (request /
	// response wire shape, SSE parsing) is identical, so Complete/Stream/Embed
	// reuse the OpenAI shaping verbatim. Auth differs only in the header name
	// (api-key vs Authorization: Bearer), set by the caller's Authenticator.
	azure      bool
	apiVersion string // Azure api-version query value, e.g. 2024-10-01-preview
	deployment string // Azure deployment name backing the model
}

// New constructs a Client pointed at the public OpenAI endpoint.
func New(authenticator auth.Authenticator) *Client {
	return NewWithBaseURL(authenticator, defaultBaseURL)
}

// NewWithBaseURL is like New but lets callers override the base URL (used for
// local mocks via httptest, Azure-mirror endpoints, or proxies). The base URL
// must not end with a trailing slash.
func NewWithBaseURL(authenticator auth.Authenticator, baseURL string) *Client {
	return &Client{
		auth:    authenticator,
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: defaultTimeout},
	}
}

// NewAzure constructs a Client that targets an Azure OpenAI resource.
// resourceURL is the resource host (https://<resource>.openai.azure.com, any
// trailing slash is trimmed); apiVersion is the required Azure api-version
// (e.g. 2024-10-01-preview); deployment is the Azure deployment name that backs
// the logical model. Azure mirrors OpenAI's request/response wire shape, so only
// URL building (here) and the auth header name (the caller's Authenticator —
// api-key for Azure) differ.
func NewAzure(authenticator auth.Authenticator, resourceURL, apiVersion, deployment string) *Client {
	return &Client{
		auth:       authenticator,
		baseURL:    strings.TrimRight(resourceURL, "/"),
		http:       &http.Client{Timeout: defaultTimeout},
		azure:      true,
		apiVersion: apiVersion,
		deployment: deployment,
	}
}

// Name returns the canonical provider id.
func (c *Client) Name() string { return "openai" }

// AuthMode returns the authentication mode (always api_key for this provider).
func (c *Client) AuthMode() string { return agentmodel.AuthModeAPIKey }

// SupportedModels returns a curated, non-exhaustive list of OpenAI models the
// provider can dispatch. The router treats this as a hint — unknown models
// will still be forwarded if a deployment routes through this provider.
func (c *Client) SupportedModels() []string {
	return []string{
		"gpt-4o",
		"gpt-4o-mini",
		"gpt-4-turbo",
		"gpt-4",
		"gpt-3.5-turbo",
		"gpt-5",
		"text-embedding-3-small",
		"text-embedding-3-large",
		"text-embedding-ada-002",
	}
}

// Complete performs a non-streaming chat completion.
func (c *Client) Complete(ctx context.Context, req agentmodel.ChatRequest) (agentmodel.ChatResponse, error) {
	// Force stream=false; if a caller wants streaming they must use Stream().
	req = sanitizeChatRequest(req)
	req.Stream = false
	httpResp, err := c.postJSON(ctx, "/chat/completions", toWireRequest(req), "chat-completions")
	if err != nil {
		return agentmodel.ChatResponse{}, err
	}
	defer httpResp.Body.Close()
	// Decode into an intermediate type so we can flatten OpenAI's nested
	// prompt_tokens_details.cached_tokens into Usage.CacheReadInputTokens.
	var wire openaiChatResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&wire); err != nil {
		return agentmodel.ChatResponse{}, fmt.Errorf("openai: decode response: %w", err)
	}
	return wire.toAgentmodel(), nil
}

// openaiChatResponse mirrors agentmodel.ChatResponse but captures OpenAI's
// nested usage detail fields. We translate to the flat shape on the way out.
type openaiChatResponse struct {
	ID      string              `json:"id"`
	Object  string              `json:"object"`
	Created int64               `json:"created"`
	Model   string              `json:"model"`
	Choices []agentmodel.Choice `json:"choices"`
	Usage   openaiUsage         `json:"usage"`
}

type openaiUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens,omitempty"`
	} `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails struct {
		ReasoningTokens *int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func (w openaiChatResponse) toAgentmodel() agentmodel.ChatResponse {
	u := agentmodel.Usage{
		PromptTokens:     w.Usage.PromptTokens,
		CompletionTokens: w.Usage.CompletionTokens,
		TotalTokens:      w.Usage.TotalTokens,
		ReasoningTokens:  w.Usage.CompletionTokensDetails.ReasoningTokens,
	}
	if w.Usage.PromptTokensDetails != nil {
		u.CacheReadInputTokens = w.Usage.PromptTokensDetails.CachedTokens
	}
	return agentmodel.ChatResponse{
		ID:      w.ID,
		Object:  w.Object,
		Created: w.Created,
		Model:   w.Model,
		Choices: w.Choices,
		Usage:   u,
	}
}

// streamRequest is what we POST when streaming. Mirrors the wire request plus
// the stream_options sub-object.
type streamRequest struct {
	openaiWireRequest
	StreamOptions streamOptions `json:"stream_options"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// openaiWireRequest is the request actually POSTed to /chat/completions. It
// embeds ChatRequest so every standard field marshals verbatim, and adds
// max_completion_tokens for reasoning-family models. toWireRequest guarantees
// exactly one of max_tokens / max_completion_tokens is ever emitted.
type openaiWireRequest struct {
	agentmodel.ChatRequest
	MaxCompletionTokens *int `json:"max_completion_tokens,omitempty"`
}

// toWireRequest translates a sanitized ChatRequest into the wire shape. OpenAI's
// reasoning families (o-series, gpt-5) reject the legacy max_tokens on
// chat/completions — "Unsupported parameter: 'max_tokens' is not supported with
// this model. Use 'max_completion_tokens' instead." — so for those models the
// value is relocated to max_completion_tokens. Every other model keeps
// max_tokens, because OpenAI-compatible servers reached via the openaicompat
// provider (vLLM, llama.cpp, Ollama) understand only the legacy field.
func toWireRequest(req agentmodel.ChatRequest) openaiWireRequest {
	w := openaiWireRequest{ChatRequest: req}
	if req.MaxTokens != nil && requiresMaxCompletionTokens(req.Model) {
		w.MaxCompletionTokens = req.MaxTokens
		w.ChatRequest.MaxTokens = nil
	}
	return w
}

// requiresMaxCompletionTokens reports whether model belongs to an OpenAI
// reasoning family whose chat/completions endpoint rejects max_tokens: the
// o-series (o1/o3/o4…) and the gpt-5 family.
func requiresMaxCompletionTokens(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if strings.HasPrefix(m, "gpt-5") {
		return true
	}
	for _, fam := range []string{"o1", "o3", "o4"} {
		if m == fam || strings.HasPrefix(m, fam+"-") {
			return true
		}
	}
	return false
}

func sanitizeChatRequest(req agentmodel.ChatRequest) agentmodel.ChatRequest {
	// The gateway-level Thinking/ReasoningEffort fields are LiteLLM-compatible
	// controls for providers that explicitly translate them (currently
	// Anthropic). Forwarding them verbatim to OpenAI's chat/completions can make
	// tool calls fail for GPT-5.x models; callers that need native OpenAI
	// reasoning controls should use a provider path that targets Responses.
	req.Thinking = nil
	req.ReasoningEffort = ""
	req.Messages = sanitizeMessages(req.Messages)
	return req
}

// sanitizeMessages returns a copy of messages normalized for the OpenAI wire
// dialect: tool-call IDs shortened to OpenAI's length limit, and the
// Anthropic-style per-message cache_control hint removed (clients may set
// it on system messages; OpenAI itself ignores unknown fields, but a strict
// OpenAI-compatible local server behind base_url — vLLM, llama.cpp, some
// Ollama configs — can reject the whole request over it). The caller's slice
// is never mutated, so a router failover can still replay the original
// messages to a provider where cache_control is meaningful.
func sanitizeMessages(messages []agentmodel.Message) []agentmodel.Message {
	if len(messages) == 0 {
		return messages
	}
	out := make([]agentmodel.Message, len(messages))
	copy(out, messages)
	ids := map[string]string{}
	for i := range out {
		out[i].CacheControl = nil
		if len(out[i].ToolCalls) > 0 {
			// The struct copy above shares the nested ToolCalls backing array
			// with the caller; copy it before shortening IDs so a failover
			// replay to another provider sees the original IDs.
			tcs := make([]agentmodel.ToolCall, len(out[i].ToolCalls))
			copy(tcs, out[i].ToolCalls)
			out[i].ToolCalls = tcs
		}
		for j := range out[i].ToolCalls {
			id := out[i].ToolCalls[j].ID
			if id == "" {
				continue
			}
			short := shortenToolCallID(id)
			ids[id] = short
			out[i].ToolCalls[j].ID = short
		}
		if out[i].ToolCallID == "" {
			continue
		}
		if short, ok := ids[out[i].ToolCallID]; ok {
			out[i].ToolCallID = short
		} else {
			out[i].ToolCallID = shortenToolCallID(out[i].ToolCallID)
		}
	}
	return out
}

func shortenToolCallID(id string) string {
	if len(id) <= 64 {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "call_" + hex.EncodeToString(sum[:])[:40]
}

// streamChunk is the SSE payload OpenAI returns. Only fields we read are
// declared. Usage uses the openaiUsage shape so we can extract nested
// prompt_tokens_details.cached_tokens; we re-flatten before yielding.
type streamChunk struct {
	ID      string              `json:"id"`
	Object  string              `json:"object"`
	Created int64               `json:"created"`
	Model   string              `json:"model"`
	Choices []streamChunkChoice `json:"choices"`
	Usage   *openaiUsage        `json:"usage"`
	// Error carries a mid-stream error object. OpenAI-compatible backends
	// (vLLM/Ollama/Azure content-filter) can emit `data: {"error":{...}}` after
	// some tokens; it must be surfaced, not swallowed into a truncated success.
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

type streamChunkChoice struct {
	Index        int                `json:"index"`
	Delta        agentmodel.Message `json:"delta"`
	FinishReason string             `json:"finish_reason"`
}

// Stream performs a streaming chat completion. The returned iterator yields
// chunks until the upstream sends `data: [DONE]` or the context is cancelled.
// The terminal usage chunk (when include_usage is set) is yielded as a chunk
// whose Delta is empty but Usage is non-nil.
func (c *Client) Stream(ctx context.Context, req agentmodel.ChatRequest) (iter.Seq2[provider.StreamChunk, error], error) {
	req = sanitizeChatRequest(req)
	req.Stream = true
	sreq := streamRequest{openaiWireRequest: toWireRequest(req), StreamOptions: streamOptions{IncludeUsage: true}}
	body, err := json.Marshal(sreq)
	if err != nil {
		return nil, fmt.Errorf("openai: marshal stream request: %w", err)
	}
	httpReq, err := c.newRequest(ctx, "POST", "/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	httpResp, err := c.send(ctx, httpReq, "chat-completions-stream")
	if err != nil {
		return nil, err
	}

	seq := func(yield func(provider.StreamChunk, error) bool) {
		defer httpResp.Body.Close()
		scanner := bufio.NewScanner(httpResp.Body)
		// SSE lines can be large (long tool-call deltas); allow up to 1 MiB.
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			// Only `data:` lines carry payload. Ignore comments / event:
			// lines etc.
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				return
			}
			var sc streamChunk
			if err := json.Unmarshal([]byte(payload), &sc); err != nil {
				yield(provider.StreamChunk{}, fmt.Errorf("openai: decode stream chunk: %w (raw=%q)", err, payload))
				return
			}
			if sc.Error != nil {
				msg := sc.Error.Message
				if msg == "" {
					msg = "upstream stream error"
				}
				// Include type+code so agentmodel.Wrap can classify (e.g. a
				// content_filter code) rather than defaulting to a retryable 502.
				yield(provider.StreamChunk{}, fmt.Errorf("openai: stream error (type=%s code=%s): %s", sc.Error.Type, sc.Error.Code, msg))
				return
			}
			out := provider.StreamChunk{}
			if sc.Usage != nil {
				u := agentmodel.Usage{
					PromptTokens:     sc.Usage.PromptTokens,
					CompletionTokens: sc.Usage.CompletionTokens,
					TotalTokens:      sc.Usage.TotalTokens,
					ReasoningTokens:  sc.Usage.CompletionTokensDetails.ReasoningTokens,
				}
				if sc.Usage.PromptTokensDetails != nil {
					u.CacheReadInputTokens = sc.Usage.PromptTokensDetails.CachedTokens
				}
				out.Usage = &u
			}
			if len(sc.Choices) > 0 {
				out.Delta = sc.Choices[0].Delta
				out.FinishReason = sc.Choices[0].FinishReason
			}
			if !yield(out, nil) {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			yield(provider.StreamChunk{}, fmt.Errorf("openai: stream read: %w", err))
		}
	}
	return seq, nil
}

// Embed performs an embedding request.
func (c *Client) Embed(ctx context.Context, req agentmodel.EmbeddingRequest) (agentmodel.EmbeddingResponse, error) {
	httpResp, err := c.postJSON(ctx, "/embeddings", req, "embeddings")
	if err != nil {
		return agentmodel.EmbeddingResponse{}, err
	}
	defer httpResp.Body.Close()
	var resp agentmodel.EmbeddingResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
		return agentmodel.EmbeddingResponse{}, fmt.Errorf("openai: decode embed response: %w", err)
	}
	return resp, nil
}

// ListModels returns the model ids OpenAI currently offers (GET /v1/models).
// Implements provider.ModelLister.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	httpReq, err := c.newRequest(ctx, "GET", "/models", nil)
	if err != nil {
		return nil, err
	}
	httpResp, err := c.send(ctx, httpReq, "list-models")
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
		return nil, fmt.Errorf("openai: decode list-models response: %w", err)
	}
	out := make([]string, 0, len(resp.Data))
	for _, m := range resp.Data {
		out = append(out, m.ID)
	}
	return out, nil
}

// newRequest builds an http.Request for the given path under c.baseURL.
// Credentials are NOT applied here — send does that, so that a request reaching
// the wire without them is not expressible (see send).
func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.resolveURL(path), body)
	if err != nil {
		return nil, fmt.Errorf("openai: build request: %w", err)
	}
	return req, nil
}

// resolveURL maps an OpenAI-relative path (e.g. /chat/completions) to the full
// upstream URL. Standard mode is c.baseURL+path. Azure mode rewrites to
// the deployment-scoped layout and appends the required api-version query:
//
//	/chat/completions -> /openai/deployments/<deployment>/chat/completions?api-version=<v>
//	/embeddings       -> /openai/deployments/<deployment>/embeddings?api-version=<v>
//	/models           -> /openai/models?api-version=<v>   (account-scoped, not per-deployment)
func (c *Client) resolveURL(path string) string {
	if !c.azure {
		return c.baseURL + path
	}
	var p string
	switch path {
	case "/chat/completions", "/embeddings":
		p = "/openai/deployments/" + c.deployment + path
	case "/models":
		p = "/openai/models"
	default:
		// Account-scoped fallback. A NEW deployment-scoped endpoint must be added
		// to the cases above, or it is mis-routed here (account- not per-deployment).
		p = "/openai" + path
	}
	return c.baseURL + p + "?api-version=" + url.QueryEscape(c.apiVersion)
}

// send applies credentials, performs req, and turns any non-200 into a typed
// error via mapHTTPError. On success the response is returned with its body
// still open — the caller owns it, which the streaming path depends on.
//
// Every HTTP call in this package goes through here, which is the point. The
// status used to be mapped by each call site remembering to read the body and
// call mapHTTPError on its own `!= StatusOK` branch — five sites, five chances
// to forget. Gemini had the identical shape and one of its four sites did
// forget, leaving a hardcoded retryable error. One chokepoint makes the class unrepresentable
// rather than merely absent. See mapHTTPError for why the blast radius
// is wider than this package.
//
// Auth lives here rather than in newRequest so that both halves of "a request
// that is safe to put on the wire" are behind one call. Splitting them meant a
// caller could hand-build a request and still reach send — which the guard test
// permits, since it only forbids touching c.http — and an unauthenticated call
// comes back 401, classifies as a terminal auth failure, and stops the router's
// fallback walk. req already carries the context, so send takes none.
//
// op names the call ("embeddings", "list-models") and is required. It is
// stamped on both failure paths: a transport error and a mapped status error
// are equally useless in a log without knowing which endpoint produced them,
// and the status half is the one you get paged for.
func (c *Client) send(ctx context.Context, req *http.Request, op string) (*http.Response, error) {
	if c.auth != nil {
		if err := c.auth.Apply(ctx, req); err != nil {
			return nil, fmt.Errorf("openai: %s: apply auth: %w", op, err)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai: %s: %w", op, err)
	}
	if resp.StatusCode == http.StatusOK {
		return resp, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return nil, fmt.Errorf("openai: %s: %w", op, mapHTTPError(resp, body))
}

// postJSON marshals body, builds a POST to path, and sends it. The three JSON
// endpoints (chat, embeddings, images) differ only in path and payload, so this
// is where that sameness lives; Stream and ListModels build their own requests
// because one sets an SSE Accept header and the other has no body.
func (c *Client) postJSON(ctx context.Context, path string, body any, op string) (*http.Response, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openai: %s: marshal request: %w", op, err)
	}
	req, err := c.newRequest(ctx, "POST", path, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.send(ctx, req, op)
}

// mapHTTPError converts a non-2xx OpenAI response to a typed error. We avoid
// declaring sentinel Err* values in this package per the plan; callers can
// match on substrings or status code text in the meantime.
//
// Every error carries the upstream status via agentmodel.WithUpstreamStatus, so
// wire.Wrap classifies by it rather than by substrings of the body interpolated
// below — see wire/status.go for why that body cannot name its own family.
//
// resp also supplies the retry hint a 429 carries (wire.RetryAfterFor gates
// that to 429s). This matters beyond openai: azure, deepseek, and every
// openaicompat vendor compose this client, so one hint here covers all of them.
func mapHTTPError(resp *http.Response, body []byte) error {
	return agentmodel.WithUpstreamStatus(
		rawHTTPError(resp, strings.TrimSpace(string(body))), resp.StatusCode)
}

// rawHTTPError builds the message for mapHTTPError. Split out so the status is
// attached in exactly one place rather than on every return.
func rawHTTPError(resp *http.Response, bodyStr string) error {
	status := resp.StatusCode
	switch status {
	case http.StatusUnauthorized:
		return errors.New("openai: authentication failed: " + bodyStr)
	case http.StatusTooManyRequests:
		return agentmodel.WithRetryAfter(
			errors.New("openai: rate limit exceeded: "+bodyStr),
			agentmodel.RetryAfterFor(resp, time.Now()))
	case http.StatusBadRequest:
		// Detect context-window errors so the router can surface a sensible
		// fallback. We pass through the raw body for visibility.
		if strings.Contains(bodyStr, "context_length_exceeded") {
			return errors.New("openai: context window exceeded: " + bodyStr)
		}
		return fmt.Errorf("openai: bad request (status=%d): %s", status, bodyStr)
	}
	if status >= 500 {
		return fmt.Errorf("openai: upstream error (status=%d): %s", status, bodyStr)
	}
	return fmt.Errorf("openai: request failed (status=%d): %s", status, bodyStr)
}
