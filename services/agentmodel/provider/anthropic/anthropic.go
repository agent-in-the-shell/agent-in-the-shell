// Package anthropic implements the agentmodel Provider for the Anthropic
// Messages API (https://api.anthropic.com/v1/messages).
//
// The provider is auth-mode-agnostic: callers wire in either an
// *auth.StaticKey (for x-api-key header, "api_key" mode) or
// *auth.AnthropicOAuth (for Authorization Bearer + anthropic-beta,
// "subscription" mode). The provider just calls auth.Apply on every request;
// header rewriting / mode-specific concerns live entirely in the auth impl.
package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
)

const (
	defaultBaseURL           = "https://api.anthropic.com"
	anthropicVersion         = "2023-06-01"
	defaultMaxTokens         = 1024
	messagesPath             = "/v1/messages"
	contentTypeJSON          = "application/json"
	headerVersionKey         = "anthropic-version"
	headerContentType        = "Content-Type"
	claudeCodeIdentityPrompt = "You are Claude Code, Anthropic's official CLI for Claude."
	betaInterleavedThinking  = "interleaved-thinking-2025-05-14"
	// maxCacheBreakpoints is the hard limit Anthropic enforces on the number of
	// cache_control markers in a single request (across tools + system +
	// messages). Exceeding it is a 400. The proxy auto-injects one breakpoint on
	// the system prompt in subscription mode, so it must count the breakpoints a
	// client already placed and skip its own injection when doing so would push
	// the request over the limit.
	maxCacheBreakpoints = 4
)

// Pre-computed JSON fragments used by preparePassthroughBody. Both are fixed
// constants derived from claudeCodeIdentityPrompt; computing them once avoids
// a json.Marshal call on every subscription passthrough request.
var (
	// claudeCodeSystemJSON is the JSON-encoded string for the system field when
	// no caller-supplied system is present.
	claudeCodeSystemJSON = json.RawMessage(`"` + claudeCodeIdentityPrompt + `"`)
	// claudeCodePrefixBlock is the JSON-encoded text block prepended to array
	// system fields (cached content block form).
	claudeCodePrefixBlock = json.RawMessage(`{"type":"text","text":"` + claudeCodeIdentityPrompt + `"}`)
)

// Client is an Anthropic Messages API provider.
type Client struct {
	auth    auth.Authenticator
	baseURL string
	http    *http.Client
	// cacheTTL is the ttl applied to the prompt-cache breakpoint the proxy
	// auto-injects on the system prompt in subscription mode. Empty uses
	// Anthropic's default 5-minute ephemeral cache; "1h" requests the 1-hour
	// TTL (at a higher cache-write cost). It does NOT alter cache_control hints
	// a client supplies itself — those are passed through verbatim.
	cacheTTL string
}

// Option customizes a Client at construction time.
type Option func(*Client)

// WithCacheTTL sets the TTL for the prompt-cache breakpoint the proxy injects
// on the system prompt in subscription mode. Valid values are "" (Anthropic's
// default 5-minute ephemeral cache), "5m", and "1h"; any other value panics, as
// a misconfigured TTL would be silently rejected by the API on every request.
func WithCacheTTL(ttl string) Option {
	switch ttl {
	case "", "5m", "1h":
		return func(c *Client) { c.cacheTTL = ttl }
	default:
		panic(fmt.Sprintf("anthropic: invalid cache TTL %q (want \"\", \"5m\", or \"1h\")", ttl))
	}
}

// New constructs a Client targeting the public Anthropic API.
func New(authenticator auth.Authenticator, opts ...Option) *Client {
	return NewWithBaseURL(authenticator, defaultBaseURL, opts...)
}

// NewWithBaseURL is identical to New but lets callers point at a fake server
// (used in tests) or a self-hosted gateway.
func NewWithBaseURL(authenticator auth.Authenticator, baseURL string, opts ...Option) *Client {
	c := &Client{
		auth:    authenticator,
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    http.DefaultClient,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Name implements provider.Provider.
func (c *Client) Name() string { return "anthropic" }

// AuthMode delegates to the underlying authenticator so the provider can serve
// both API-key and subscription modes without branching.
func (c *Client) AuthMode() string { return c.auth.Mode() }

// SupportedModels returns the model identifiers this provider routes.
func (c *Client) SupportedModels() []string {
	return []string{
		"claude-3-5-sonnet-latest",
		"claude-3-5-haiku-latest",
		"claude-3-7-sonnet-latest",
	}
}

// Embed is unsupported on the Anthropic Messages API.
func (c *Client) Embed(_ context.Context, _ agentmodel.EmbeddingRequest) (agentmodel.EmbeddingResponse, error) {
	return agentmodel.EmbeddingResponse{}, provider.ErrNotSupported
}

// statusError is a non-2xx response from the Messages API. It carries the
// status code so the auth-recovery path can recognize a 401, while its Error()
// string preserves the legacy "anthropic: status <code>" / "anthropic: stream
// status <code>" wording that callers and tests key on.
type statusError struct {
	code   int
	body   string
	stream bool
	// retryAfter is the upstream's own "come back at", read off a 429's headers
	// by wire.RetryAfterFor. Zero for every other status.
	retryAfter time.Duration
}

func (e *statusError) Error() string {
	label := "status "
	if e.stream {
		label = "stream status "
	}
	return fmt.Sprintf("anthropic: %s%d: %s", label, e.code, e.body)
}

// RetryAfterHint implements wire.RetryAfterHinter so agentmodel.Wrap copies the
// upstream's window onto the classified error, where the pool and router use it
// to size their cooldowns.
func (e *statusError) RetryAfterHint() time.Duration { return e.retryAfter }

// forceRefresher is the optional capability an Authenticator advertises when it
// can re-mint its access token on demand. *auth.AnthropicOAuthRefreshable
// implements it; a static api-key or static OAuth token does not (and so never
// triggers a retry).
type forceRefresher interface {
	ForceRefresh(ctx context.Context) error
}

// recoverUnauthorized reports whether err is a 401 that a forced credential
// refresh recovered, so the caller retries once. Anthropic subscription tokens
// are opaque (no parseable expiry) and the 401 body is not a stable machine
// code, so any 401 from a refreshable authenticator triggers one refresh +
// retry; api-key auth (no refresher) surfaces the 401 unchanged.
func (c *Client) recoverUnauthorized(ctx context.Context, err error) bool {
	var se *statusError
	if !errors.As(err, &se) || se.code != http.StatusUnauthorized {
		return false
	}
	fr, ok := c.auth.(forceRefresher)
	if !ok {
		return false
	}
	return fr.ForceRefresh(ctx) == nil
}

// doMessages issues one /v1/messages request and returns the live response on a
// 2xx status, or a *statusError on non-2xx so the caller can recognize a
// recoverable 401. stream selects the SSE Accept header and the error wording.
func (c *Client) doMessages(ctx context.Context, anthReq anthropicReq, body []byte, stream bool) (*http.Response, error) {
	httpReq, err := c.newRequest(ctx, body)
	if err != nil {
		return nil, err
	}
	// Budget-based thinking requires this beta header; adaptive (4.6+) does not.
	if anthReq.Thinking != nil && anthReq.Thinking.Type == "enabled" {
		httpReq.Header.Add("anthropic-beta", betaInterleavedThinking)
	}
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: http: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(resp.Body)
		retryAfter := agentmodel.RetryAfterFor(resp, time.Now())
		_ = resp.Body.Close()
		return nil, &statusError{code: resp.StatusCode, body: string(errBody), stream: stream, retryAfter: retryAfter}
	}
	return resp, nil
}

// Complete dispatches a non-streaming chat completion to /v1/messages. On a 401
// from a refreshable authenticator it forces one credential refresh and retries
// once, so a token that expired between requests recovers transparently.
func (c *Client) Complete(ctx context.Context, req agentmodel.ChatRequest) (agentmodel.ChatResponse, error) {
	if err := agentmodel.ValidateTextOnlyContent(req); err != nil {
		return agentmodel.ChatResponse{}, err
	}
	anthReq := toAnthropicReq(req, false, c.AuthMode(), c.cacheTTL)
	body, err := json.Marshal(anthReq)
	if err != nil {
		return agentmodel.ChatResponse{}, fmt.Errorf("anthropic: marshal request: %w", err)
	}
	resp, err := c.doMessages(ctx, anthReq, body, false)
	if err != nil && c.recoverUnauthorized(ctx, err) {
		resp, err = c.doMessages(ctx, anthReq, body, false)
	}
	if err != nil {
		return agentmodel.ChatResponse{}, err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return agentmodel.ChatResponse{}, fmt.Errorf("anthropic: read body: %w", err)
	}
	var aResp anthropicResp
	if err := json.Unmarshal(respBytes, &aResp); err != nil {
		return agentmodel.ChatResponse{}, fmt.Errorf("anthropic: decode response: %w", err)
	}
	out, err := fromAnthropicResp(aResp)
	if err != nil {
		return agentmodel.ChatResponse{}, err
	}
	out.Usage.AuthMode = c.AuthMode()
	return out, nil
}

// Stream dispatches a streaming chat completion and returns a Go-iterator
// that yields chunks in OpenAI delta shape until the stream ends. Like
// Complete, a 401 from a refreshable authenticator is recovered with one
// forced refresh + re-open before the stream begins.
func (c *Client) Stream(ctx context.Context, req agentmodel.ChatRequest) (iter.Seq2[provider.StreamChunk, error], error) {
	if err := agentmodel.ValidateTextOnlyContent(req); err != nil {
		return nil, err
	}
	anthReq := toAnthropicReq(req, true, c.AuthMode(), c.cacheTTL)
	body, err := json.Marshal(anthReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal stream request: %w", err)
	}
	resp, err := c.doMessages(ctx, anthReq, body, true)
	if err != nil && c.recoverUnauthorized(ctx, err) {
		resp, err = c.doMessages(ctx, anthReq, body, true)
	}
	if err != nil {
		return nil, err
	}

	authMode := c.AuthMode()
	seq := func(yield func(provider.StreamChunk, error) bool) {
		defer resp.Body.Close()
		streamSSE(resp.Body, authMode, yield)
	}
	return seq, nil
}

// MessagesPassthrough sends a pre-formed Anthropic Messages JSON body to
// /v1/messages on the configured upstream and returns the live HTTP response
// for the caller to copy through. Used by the /v1/messages frontend so
// Anthropic-shaped clients (pi-ai, anthropic-sdk, etc.) can talk to
// agentmodel without the OpenAI-shape conversion.
//
// If modelOverride is non-empty, the body's "model" field is rewritten to it
// so the caller's logical model_name (e.g. "claude-sonnet-4-5") gets mapped
// to the deployment's upstream id (e.g. "claude-3-5-sonnet-latest").
//
// The caller MUST close the returned response body.
//
// On a 401 from a refreshable authenticator, the expired token is refreshed
// once and the request re-issued, so the Anthropic-shaped frontend recovers
// transparently instead of passing the 401 through to the client. Any other
// status (including a 401 that survives the retry, or from a non-refreshable
// api-key auth) is returned to the caller unchanged.
func (c *Client) MessagesPassthrough(ctx context.Context, body []byte, modelOverride, clientBetas string) (*http.Response, error) {
	resp, err := c.messagesPassthrough(ctx, body, modelOverride, clientBetas)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		if fr, ok := c.auth.(forceRefresher); ok && fr.ForceRefresh(ctx) == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			return c.messagesPassthrough(ctx, body, modelOverride, clientBetas)
		}
	}
	return resp, nil
}

func (c *Client) messagesPassthrough(ctx context.Context, body []byte, modelOverride, clientBetas string) (*http.Response, error) {
	isSubscription := c.auth.Mode() == agentmodel.AuthModeSubscription
	if modelOverride != "" || isSubscription {
		var err error
		body, err = preparePassthroughBody(body, modelOverride, isSubscription, c.cacheTTL)
		if err != nil {
			return nil, fmt.Errorf("anthropic passthrough: %w", err)
		}
	}
	httpReq, err := c.newRequest(ctx, body)
	if err != nil {
		return nil, err
	}
	if clientBetas != "" {
		existing := httpReq.Header.Get("anthropic-beta")
		httpReq.Header.Set("anthropic-beta", auth.MergeCSV(existing, clientBetas))
	}
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	return c.http.Do(httpReq)
}

// preparePassthroughBody applies model override and Claude Code system
// injection in a single JSON round-trip. Returns body unchanged when neither
// transform produces a mutation (model already matches, system already injected).
//
// Subscription tokens require the Claude Code identity system prompt — Anthropic
// enforces this for Sonnet+ models when the claude-code-20250219 beta is present.
func preparePassthroughBody(body []byte, modelOverride string, injectSystem bool, cacheTTL string) ([]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	mutated := false

	if modelOverride != "" {
		var current string
		// json.Unmarshal of a JSON string into string cannot fail for a valid body.
		if json.Unmarshal(raw["model"], &current) != nil || current != modelOverride {
			enc, _ := json.Marshal(modelOverride) // marshal of string cannot fail
			raw["model"] = enc
			mutated = true
		}
	}

	if injectSystem {
		// cacheControlIfRoom returns the cache_control hint for our injected
		// system breakpoint, or nil when the client already holds the maximum
		// number of markers (counting tools + system + messages) — adding another
		// would 400. It's a closure so the breakpoint scan runs lazily, only when
		// we're actually about to cache a block (never for the no-system case, and
		// never on a block the client already cached). The identity block is
		// prepended regardless — it's required for subscription tokens.
		cacheControlIfRoom := func() json.RawMessage {
			if countRawCacheBreakpoints(raw) >= maxCacheBreakpoints {
				return nil
			}
			enc, _ := json.Marshal(ephemeralCacheControl(cacheTTL)) // marshal of map[string]string cannot fail
			return enc
		}
		switch existing := raw["system"]; {
		case existing == nil:
			raw["system"] = claudeCodeSystemJSON
			mutated = true
		default:
			var s string
			if err := json.Unmarshal(existing, &s); err == nil {
				if !strings.HasPrefix(s, claudeCodeIdentityPrompt) {
					// Build array form: CC identity block + user block, caching the
					// user's system prompt across turns when under the breakpoint cap.
					ub := map[string]any{"type": "text", "text": s}
					if cc := cacheControlIfRoom(); cc != nil {
						ub["cache_control"] = cc
					}
					userBlock, _ := json.Marshal(ub)
					merged, _ := json.Marshal([]json.RawMessage{claudeCodePrefixBlock, userBlock})
					raw["system"] = merged
					mutated = true
				}
			} else {
				// Array form (cached content blocks) — prepend CC block and add
				// cache_control to last user block if not already set and under cap.
				var blocks []json.RawMessage
				if err := json.Unmarshal(existing, &blocks); err == nil {
					alreadyInjected := false
					if len(blocks) > 0 {
						var first map[string]json.RawMessage
						if jerr := json.Unmarshal(blocks[0], &first); jerr == nil {
							var firstText string
							if json.Unmarshal(first["text"], &firstText) == nil && strings.HasPrefix(firstText, claudeCodeIdentityPrompt) {
								alreadyInjected = true
							}
						}
					}
					if !alreadyInjected {
						if len(blocks) > 0 {
							var lastBlock map[string]json.RawMessage
							if jerr := json.Unmarshal(blocks[len(blocks)-1], &lastBlock); jerr == nil && lastBlock["cache_control"] == nil {
								if cc := cacheControlIfRoom(); cc != nil {
									lastBlock["cache_control"] = cc
									if enc, merr := json.Marshal(lastBlock); merr == nil {
										blocks[len(blocks)-1] = enc
									}
								}
							}
						}
						merged, _ := json.Marshal(append([]json.RawMessage{claudeCodePrefixBlock}, blocks...))
						raw["system"] = merged
						mutated = true
					}
				}
				// unknown shape — leave untouched
			}
		}
	}

	if !mutated {
		return body, nil
	}
	return json.Marshal(raw)
}

// ListModels returns the model ids Anthropic currently offers
// (GET /v1/models). Implements provider.ModelLister.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/models?limit=1000", nil)
	if err != nil {
		return nil, fmt.Errorf("anthropic: build list-models request: %w", err)
	}
	httpReq.Header.Set(headerVersionKey, anthropicVersion)
	if err := c.auth.Apply(ctx, httpReq); err != nil {
		return nil, fmt.Errorf("anthropic: apply auth: %w", err)
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: do list-models request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("anthropic: list-models HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("anthropic: decode list-models response: %w", err)
	}
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

func (c *Client) newRequest(ctx context.Context, body []byte) (*http.Request, error) {
	url := c.baseURL + messagesPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}
	httpReq.Header.Set(headerContentType, contentTypeJSON)
	httpReq.Header.Set(headerVersionKey, anthropicVersion)
	if err := c.auth.Apply(ctx, httpReq); err != nil {
		return nil, fmt.Errorf("anthropic: apply auth: %w", err)
	}
	return httpReq, nil
}

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

type anthropicReq struct {
	Model    string             `json:"model"`
	Messages []anthropicMessage `json:"messages"`
	// System is either a string (no system caching) or []anthropicContent
	// (when at least one system message has cache_control set). The Anthropic
	// API accepts both forms; we pick whichever lets us preserve cache hints.
	System       any                    `json:"system,omitempty"`
	MaxTokens    int                    `json:"max_tokens"`
	Temperature  *float64               `json:"temperature,omitempty"`
	TopP         *float64               `json:"top_p,omitempty"`
	StopSeq      []string               `json:"stop_sequences,omitempty"`
	Stream       bool                   `json:"stream,omitempty"`
	Tools        []anthropicTool        `json:"tools,omitempty"`
	ToolChoice   any                    `json:"tool_choice,omitempty"`
	Thinking     *anthropicThinking     `json:"thinking,omitempty"`
	OutputConfig *anthropicOutputConfig `json:"output_config,omitempty"`
}

type anthropicMessage struct {
	Role    string             `json:"role"`
	Content []anthropicContent `json:"content"`
}

// anthropicContent is a single content block. Different fields are populated
// depending on Type ("text" | "tool_use" | "tool_result" | "thinking" | "redacted_thinking").
//
// CacheControl: opt-in prompt-caching hint passed through from the client
// (typically `{"type": "ephemeral"}`). Anthropic uses the LAST cache_control
// in the prompt as the cache breakpoint and reuses cached prefix tokens at
// 1/10 the standard input cost.
type anthropicContent struct {
	Type string `json:"type"`
	// text block
	Text string `json:"text,omitempty"`
	// tool_use block
	ID    string         `json:"id,omitempty"`
	Name  string         `json:"name,omitempty"`
	Input map[string]any `json:"input,omitempty"`
	// tool_result block
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	// caching
	CacheControl any `json:"cache_control,omitempty"`
	// Thinking text for thinking content blocks in responses.
	Thinking string `json:"thinking,omitempty"`
	// Signature is the verification signature for regular thinking blocks.
	Signature string `json:"signature,omitempty"`
	// Data is the opaque signature payload for redacted_thinking blocks.
	Data string `json:"data,omitempty"`
}

type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type anthropicOutputConfig struct {
	Effort string `json:"effort"`
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

type anthropicResp struct {
	ID         string             `json:"id"`
	Type       string             `json:"type"`
	Role       string             `json:"role"`
	Model      string             `json:"model"`
	Content    []anthropicContent `json:"content"`
	StopReason string             `json:"stop_reason"`
	Usage      anthropicUsage     `json:"usage"`
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// ---------------------------------------------------------------------------
// Translation: OpenAI → Anthropic
// ---------------------------------------------------------------------------

func toAnthropicReq(req agentmodel.ChatRequest, stream bool, authMode, cacheTTL string) anthropicReq {
	system, msgs := splitSystem(req.Messages)

	// OAuth subscription: prepend Claude Code identity block so Anthropic
	// recognises the request as coming from a Claude Code client. Add
	// cache_control to the last user-provided system block so Anthropic caches
	// the (often large) system prompt across turns at ~10% of normal input cost.
	if authMode == agentmodel.AuthModeSubscription {
		// Anthropic allows at most maxCacheBreakpoints cache_control markers per
		// request. Only inject our system breakpoint when the client hasn't
		// already used them all, otherwise the extra marker 400s the request.
		canCache := countClientCacheBreakpoints(req.Messages) < maxCacheBreakpoints
		switch s := system.(type) {
		case nil:
			system = claudeCodeIdentityPrompt
		case string:
			userBlock := anthropicContent{Type: "text", Text: s}
			if canCache {
				userBlock.CacheControl = ephemeralCacheControl(cacheTTL)
			}
			system = []anthropicContent{
				{Type: "text", Text: claudeCodeIdentityPrompt},
				userBlock,
			}
		case []anthropicContent:
			blocks := make([]anthropicContent, len(s))
			copy(blocks, s)
			if canCache && len(blocks) > 0 && blocks[len(blocks)-1].CacheControl == nil {
				blocks[len(blocks)-1].CacheControl = ephemeralCacheControl(cacheTTL)
			}
			system = append([]anthropicContent{{Type: "text", Text: claudeCodeIdentityPrompt}}, blocks...)
		}
	}

	maxTok := defaultMaxTokens
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		maxTok = *req.MaxTokens
	}

	// Temperature is incompatible with thinking (Anthropic rejects it).
	// Strip it automatically when either thinking mode is active.
	thinkingActive := req.Thinking != nil || req.ReasoningEffort != ""
	var temp *float64
	if !thinkingActive {
		temp = req.Temperature
	}

	out := anthropicReq{
		Model:       req.Model,
		Messages:    msgs,
		System:      system,
		MaxTokens:   maxTok,
		Temperature: temp,
		TopP:        req.TopP,
		StopSeq:     req.Stop,
		Stream:      stream,
		ToolChoice:  normalizeToolChoice(req.ToolChoice),
	}

	// Budget-based thinking: caller controls the token budget.
	if req.Thinking != nil {
		out.Thinking = &anthropicThinking{
			Type:         req.Thinking.Type,
			BudgetTokens: req.Thinking.BudgetTokens,
		}
	}

	// Adaptive thinking: caller sets effort level; model decides how much to think.
	if req.ReasoningEffort != "" {
		out.Thinking = &anthropicThinking{Type: "adaptive"}
		out.OutputConfig = &anthropicOutputConfig{Effort: req.ReasoningEffort}
	}

	if len(req.Tools) > 0 {
		out.Tools = make([]anthropicTool, 0, len(req.Tools))
		for _, t := range req.Tools {
			out.Tools = append(out.Tools, anthropicTool{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				InputSchema: t.Function.Parameters,
			})
		}
	}
	return out
}

// ephemeralCacheControl builds the cache_control hint the proxy attaches to its
// auto-injected system breakpoint. An empty ttl yields the bare ephemeral form
// (Anthropic's 5-minute default); a non-empty ttl ("5m"/"1h") is included. The
// raw-body passthrough path marshals the result into JSON, so this is the single
// source of truth for the hint's shape.
func ephemeralCacheControl(ttl string) map[string]string {
	cc := map[string]string{"type": "ephemeral"}
	if ttl != "" {
		cc["ttl"] = ttl
	}
	return cc
}

// countClientCacheBreakpoints counts the cache_control markers a client already
// placed on its OpenAI-shaped messages. Each message carries at most one (it
// rides the message's last content block — see toAnthropicMessage), so the
// breakpoint count equals the number of messages with a non-nil CacheControl.
// System messages count too: splitSystem turns each into its own cached block.
func countClientCacheBreakpoints(msgs []agentmodel.Message) int {
	n := 0
	for _, m := range msgs {
		if m.CacheControl != nil {
			n++
		}
	}
	return n
}

// countRawCacheBreakpoints counts cache_control markers already present in a
// raw Anthropic-shaped body by recursively scanning tools, system, and
// messages. cache_control only ever appears as a JSON object key on a content
// block or tool definition, so a recursive key count is exact (a "cache_control"
// substring inside text content is a string value, not a key).
func countRawCacheBreakpoints(raw map[string]json.RawMessage) int {
	n := 0
	for _, field := range []string{"tools", "system", "messages"} {
		n += countJSONKey(raw[field], "cache_control")
	}
	return n
}

// countJSONKey recursively counts occurrences of key across a JSON value.
func countJSONKey(data json.RawMessage, key string) int {
	if len(data) == 0 {
		return 0
	}
	var v any
	if json.Unmarshal(data, &v) != nil {
		return 0
	}
	return countKeyIn(v, key)
}

func countKeyIn(v any, key string) int {
	n := 0
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if k == key {
				n++
			}
			n += countKeyIn(val, key)
		}
	case []any:
		for _, e := range t {
			n += countKeyIn(e, key)
		}
	}
	return n
}

// normalizeToolChoice converts OpenAI-style tool_choice values to Anthropic's
// object format. Anthropic's valid enum is auto|any|tool|none, so OpenAI's
// "required" maps to {"type":"any"} (NOT the invalid {"type":"required"}), and
// OpenAI's named-function form {"type":"function","function":{"name":N}} maps to
// {"type":"tool","name":N}. Both untranslated shapes 400 on Anthropic (#664).
func normalizeToolChoice(tc any) any {
	switch v := tc.(type) {
	case string:
		switch v {
		case "auto", "none":
			return map[string]any{"type": v}
		case "required":
			return map[string]any{"type": "any"}
		}
	case map[string]any:
		// OpenAI named-function object: force a specific tool by name.
		if v["type"] == "function" {
			if fn, ok := v["function"].(map[string]any); ok {
				if name, ok := fn["name"].(string); ok && name != "" {
					return map[string]any{"type": "tool", "name": name}
				}
			}
		}
	}
	return tc
}

// splitSystem extracts the system messages and returns:
//
//   - system: a string when no system message has cache_control (compact
//     wire format), or []anthropicContent blocks when at least one does
//     (Anthropic requires the array form to attach cache_control to system
//     content). Returns nil when there are no system messages.
//   - remaining non-system messages translated to Anthropic shape.
func splitSystem(in []agentmodel.Message) (any, []anthropicMessage) {
	out := make([]anthropicMessage, 0, len(in))
	var systems []agentmodel.Message
	for _, m := range in {
		if m.Role == "system" {
			if m.Content != "" {
				systems = append(systems, m)
			}
			continue
		}
		out = append(out, toAnthropicMessage(m))
	}
	if len(systems) == 0 {
		return nil, out
	}

	// If any system message asks for caching, emit the array form so the
	// hint can ride along. Otherwise concatenate to keep the wire payload
	// minimal.
	anyCached := false
	for _, m := range systems {
		if m.CacheControl != nil {
			anyCached = true
			break
		}
	}
	if !anyCached {
		parts := make([]string, len(systems))
		for i, m := range systems {
			parts[i] = m.Content
		}
		return strings.Join(parts, "\n\n"), out
	}
	blocks := make([]anthropicContent, 0, len(systems))
	for _, m := range systems {
		blocks = append(blocks, anthropicContent{
			Type:         "text",
			Text:         m.Content,
			CacheControl: m.CacheControl,
		})
	}
	return blocks, out
}

func toAnthropicMessage(m agentmodel.Message) anthropicMessage {
	role := m.Role
	// Anthropic only accepts "user" and "assistant"; map "tool" → "user" with
	// tool_result block.
	if role == "tool" {
		role = "user"
		return anthropicMessage{
			Role: role,
			Content: []anthropicContent{{
				Type:         "tool_result",
				ToolUseID:    m.ToolCallID,
				Content:      m.Content,
				CacheControl: m.CacheControl,
			}},
		}
	}

	var blocks []anthropicContent
	if m.Content != "" {
		// CacheControl rides on the LAST content block of the message. We
		// have only one text block (and tool_use blocks below); attaching to
		// the last appended block at the end gives Anthropic its breakpoint.
		blocks = append(blocks, anthropicContent{Type: "text", Text: m.Content})
	}
	for _, tc := range m.ToolCalls {
		var input map[string]any
		if tc.Function.Arguments != "" {
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
		}
		blocks = append(blocks, anthropicContent{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: input,
		})
	}
	if m.CacheControl != nil && len(blocks) > 0 {
		blocks[len(blocks)-1].CacheControl = m.CacheControl
	}
	return anthropicMessage{Role: role, Content: blocks}
}

// ---------------------------------------------------------------------------
// Translation: Anthropic → OpenAI
// ---------------------------------------------------------------------------

func fromAnthropicResp(r anthropicResp) (agentmodel.ChatResponse, error) {
	msg := agentmodel.Message{Role: "assistant"}
	finishReason := mapStopReason(r.StopReason)

	var textParts []string
	var reasoningParts []string
	for _, block := range r.Content {
		switch block.Type {
		case "text":
			textParts = append(textParts, block.Text)
		case "thinking":
			reasoningParts = append(reasoningParts, block.Thinking)
			msg.ThinkingBlocks = append(msg.ThinkingBlocks, agentmodel.ThinkingBlock{
				Type:      "thinking",
				Thinking:  block.Thinking,
				Signature: block.Signature,
			})
		case "redacted_thinking":
			msg.ThinkingBlocks = append(msg.ThinkingBlocks, agentmodel.ThinkingBlock{
				Type:      "redacted_thinking",
				Signature: block.Data,
			})
		case "tool_use":
			argsBytes, err := json.Marshal(block.Input)
			if err != nil {
				return agentmodel.ChatResponse{}, fmt.Errorf("anthropic: marshal tool input: %w", err)
			}
			msg.ToolCalls = append(msg.ToolCalls, agentmodel.ToolCall{
				ID:   block.ID,
				Type: "function",
				Function: agentmodel.ToolCallFunction{
					Name:      block.Name,
					Arguments: string(argsBytes),
				},
			})
			finishReason = "tool_calls"
		}
	}
	msg.Content = strings.Join(textParts, "")
	msg.ReasoningContent = strings.Join(reasoningParts, "")

	// Anthropic reports cache_creation_input_tokens and cache_read_input_tokens
	// SEPARATELY from input_tokens (input_tokens excludes the cached portion).
	// Our wire convention follows OpenAI: PromptTokens INCLUDES cache, with
	// breakdown reported in CacheReadInputTokens / CacheCreationInputTokens.
	prompt := r.Usage.InputTokens + r.Usage.CacheReadInputTokens + r.Usage.CacheCreationInputTokens
	return agentmodel.ChatResponse{
		ID:      r.ID,
		Object:  "chat.completion",
		Model:   r.Model,
		Choices: []agentmodel.Choice{{Index: 0, Message: msg, FinishReason: finishReason}},
		Usage: agentmodel.Usage{
			PromptTokens:             prompt,
			CompletionTokens:         r.Usage.OutputTokens,
			TotalTokens:              prompt + r.Usage.OutputTokens,
			CacheCreationInputTokens: r.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     r.Usage.CacheReadInputTokens,
		},
	}, nil
}

// mapStopReason maps Anthropic stop_reason values onto OpenAI's closed
// finish_reason enum (stop|length|tool_calls|content_filter). The default
// clamps to "stop" rather than leaking the raw Anthropic value (#664): a strict
// OpenAI client rejects an unknown finish_reason, and downstream code defends
// against leaked values like "end_turn" (services/agentpi/core/loop.go).
func mapStopReason(s string) string {
	switch s {
	case "end_turn", "stop_sequence", "pause_turn":
		return "stop"
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}

// ---------------------------------------------------------------------------
// Streaming
// ---------------------------------------------------------------------------

// streamSSE consumes Anthropic's SSE event stream from r and yields OpenAI-
// shaped delta chunks via the supplied yield callback. The Anthropic event
// vocabulary we translate:
//
//	message_start         -> initial role chunk + cache input_tokens for usage
//	content_block_start   -> if tool_use, emit a stub tool_call chunk so the
//	                          client knows which call deltas attach to
//	content_block_delta   -> text_delta becomes a content delta;
//	                          input_json_delta becomes a tool-call args delta
//	content_block_stop    -> no-op
//	message_delta         -> stop_reason -> FinishReason; usage.output_tokens
//	message_stop          -> emit terminal chunk with cached usage if present
func streamSSE(body io.Reader, authMode string, yield func(provider.StreamChunk, error) bool) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var (
		eventName        string
		dataBuf          strings.Builder
		inputTokens      int
		outputTokens     int
		cacheReadTokens  int
		cacheWriteTokens int
		finishReason     string
		toolBlocks       = map[int]*toolBlockState{}
		thinkingBlocks   = map[int]*thinkingBlockState{}
		thinkingAccum    []agentmodel.ThinkingBlock
		// toolSeq is the next OpenAI-side tool_calls[] index to assign. Anthropic
		// numbers ALL content blocks (text/thinking/tool_use share one index
		// space), but OpenAI's tool_calls[].index counts only tool calls, so we
		// map block index -> tool index rather than reusing Anthropic's.
		toolSeq int
	)

	flushEvent := func() bool {
		if eventName == "" && dataBuf.Len() == 0 {
			return true
		}
		raw := dataBuf.String()
		dataBuf.Reset()
		name := eventName
		eventName = ""
		if raw == "" || raw == "[DONE]" {
			return true
		}
		return handleSSE(name, raw, &inputTokens, &outputTokens, &cacheReadTokens, &cacheWriteTokens,
			&finishReason, toolBlocks, &toolSeq, thinkingBlocks, &thinkingAccum, yield)
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if !flushEvent() {
				return
			}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			d := strings.TrimPrefix(line, "data:")
			d = strings.TrimPrefix(d, " ")
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(d)
			continue
		}
	}
	if !flushEvent() {
		return
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		yield(provider.StreamChunk{}, err)
		return
	}

	// Emit terminal chunk: completed thinking blocks with signatures.
	if len(thinkingAccum) > 0 {
		if !yield(provider.StreamChunk{Delta: agentmodel.Message{ThinkingBlocks: thinkingAccum}}, nil) {
			return
		}
	}

	prompt := inputTokens + cacheReadTokens + cacheWriteTokens
	finalUsage := &agentmodel.Usage{
		PromptTokens:             prompt,
		CompletionTokens:         outputTokens,
		TotalTokens:              prompt + outputTokens,
		CacheReadInputTokens:     cacheReadTokens,
		CacheCreationInputTokens: cacheWriteTokens,
		AuthMode:                 authMode,
	}
	yield(provider.StreamChunk{FinishReason: finishReason, Usage: finalUsage}, nil)
}

type toolBlockState struct {
	id    string
	name  string
	index int // OpenAI-side tool_calls[] index assigned at content_block_start
}

type thinkingBlockState struct {
	thinking  string
	signature string
	redacted  bool
}

// handleSSE dispatches a single SSE event by name. Returns false if the
// downstream consumer cancelled iteration (yield returned false).
func handleSSE(
	name, raw string,
	inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens *int,
	finishReason *string,
	toolBlocks map[int]*toolBlockState,
	toolSeq *int,
	thinkingBlocks map[int]*thinkingBlockState,
	thinkingAccum *[]agentmodel.ThinkingBlock,
	yield func(provider.StreamChunk, error) bool,
) bool {
	switch name {
	case "message_start":
		var ev struct {
			Message struct {
				Role  string         `json:"role"`
				Usage anthropicUsage `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			return yield(provider.StreamChunk{}, fmt.Errorf("anthropic: parse message_start: %w", err))
		}
		*inputTokens = ev.Message.Usage.InputTokens
		*cacheReadTokens = ev.Message.Usage.CacheReadInputTokens
		*cacheWriteTokens = ev.Message.Usage.CacheCreationInputTokens
		role := ev.Message.Role
		if role == "" {
			role = "assistant"
		}
		return yield(provider.StreamChunk{Delta: agentmodel.Message{Role: role}}, nil)

	case "content_block_start":
		var ev struct {
			Index        int              `json:"index"`
			ContentBlock anthropicContent `json:"content_block"`
		}
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			return yield(provider.StreamChunk{}, fmt.Errorf("anthropic: parse content_block_start: %w", err))
		}
		switch ev.ContentBlock.Type {
		case "tool_use":
			// Assign the next OpenAI tool index and emit id/type/name ONCE here.
			// Subsequent input_json_delta fragments carry only {index, arguments}
			// so the official OpenAI accumulators reconstruct a single call
			// (re-sending id/name on every fragment doubles them).
			idx := *toolSeq
			*toolSeq++
			toolBlocks[ev.Index] = &toolBlockState{id: ev.ContentBlock.ID, name: ev.ContentBlock.Name, index: idx}
			return yield(provider.StreamChunk{
				Delta: agentmodel.Message{
					ToolCalls: []agentmodel.ToolCall{{
						Index: &idx,
						ID:    ev.ContentBlock.ID,
						Type:  "function",
						Function: agentmodel.ToolCallFunction{
							Name:      ev.ContentBlock.Name,
							Arguments: "",
						},
					}},
				},
			}, nil)
		case "thinking":
			thinkingBlocks[ev.Index] = &thinkingBlockState{}
		case "redacted_thinking":
			thinkingBlocks[ev.Index] = &thinkingBlockState{
				redacted:  true,
				signature: ev.ContentBlock.Data,
			}
		}
		return true

	case "content_block_delta":
		var ev struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text,omitempty"`
				PartialJSON string `json:"partial_json,omitempty"`
				Thinking    string `json:"thinking,omitempty"`
				Signature   string `json:"signature,omitempty"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			return yield(provider.StreamChunk{}, fmt.Errorf("anthropic: parse content_block_delta: %w", err))
		}
		switch ev.Delta.Type {
		case "text_delta":
			return yield(provider.StreamChunk{Delta: agentmodel.Message{Content: ev.Delta.Text}}, nil)
		case "input_json_delta":
			if tb, ok := toolBlocks[ev.Index]; ok {
				// Args-only fragment: carry the OpenAI index so the client knows
				// which call this attaches to, but NOT id/type/name (those were
				// sent once at content_block_start).
				idx := tb.index
				return yield(provider.StreamChunk{
					Delta: agentmodel.Message{
						ToolCalls: []agentmodel.ToolCall{{
							Index: &idx,
							Function: agentmodel.ToolCallFunction{
								Arguments: ev.Delta.PartialJSON,
							},
						}},
					},
				}, nil)
			}
		case "thinking_delta":
			if tb, ok := thinkingBlocks[ev.Index]; ok {
				tb.thinking += ev.Delta.Thinking
			}
			return yield(provider.StreamChunk{Delta: agentmodel.Message{ReasoningContent: ev.Delta.Thinking}}, nil)
		case "signature_delta":
			if tb, ok := thinkingBlocks[ev.Index]; ok {
				tb.signature += ev.Delta.Signature
			}
			// Signatures are bookkeeping only — no chunk emitted.
		}
		return true

	case "content_block_stop":
		var ev struct {
			Index int `json:"index"`
		}
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			return true // non-fatal
		}
		if tb, ok := thinkingBlocks[ev.Index]; ok {
			if tb.redacted {
				*thinkingAccum = append(*thinkingAccum, agentmodel.ThinkingBlock{
					Type:      "redacted_thinking",
					Signature: tb.signature,
				})
			} else {
				*thinkingAccum = append(*thinkingAccum, agentmodel.ThinkingBlock{
					Type:      "thinking",
					Thinking:  tb.thinking,
					Signature: tb.signature,
				})
			}
			delete(thinkingBlocks, ev.Index)
		}
		return true

	case "message_delta":
		var ev struct {
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage anthropicUsage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			return yield(provider.StreamChunk{}, fmt.Errorf("anthropic: parse message_delta: %w", err))
		}
		if ev.Usage.OutputTokens > 0 {
			*outputTokens = ev.Usage.OutputTokens
		}
		if ev.Usage.InputTokens > 0 {
			*inputTokens = ev.Usage.InputTokens
		}
		if ev.Usage.CacheReadInputTokens > 0 {
			*cacheReadTokens = ev.Usage.CacheReadInputTokens
		}
		if ev.Usage.CacheCreationInputTokens > 0 {
			*cacheWriteTokens = ev.Usage.CacheCreationInputTokens
		}
		if ev.Delta.StopReason != "" {
			*finishReason = mapStopReason(ev.Delta.StopReason)
		}
		return true

	case "message_stop":
		return true

	case "ping", "":
		return true

	case "error":
		return yield(provider.StreamChunk{}, fmt.Errorf("anthropic: stream error: %s", raw))

	default:
		// Unknown event types are tolerated; Anthropic adds new ones over time.
		return true
	}
}
