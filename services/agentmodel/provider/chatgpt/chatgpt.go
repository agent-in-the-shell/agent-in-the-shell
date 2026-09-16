// Package chatgpt implements a Provider that talks to OpenAI's ChatGPT
// subscription endpoint (chatgpt.com/backend-api/codex) via the Codex CLI's
// OAuth flow. Cost is $0 because usage is covered by the user's ChatGPT
// Plus/Pro subscription.
//
// IMPORTANT: this endpoint is reverse-engineered. It is NOT a documented
// third-party API. Compatibility can change independently of this gateway;
// configure an API-key provider as fallback when availability matters.
//
// The endpoint speaks OpenAI's "Responses API" shape (newer than chat
// completions). This package translates between Responses and the Chat
// Completions shape used by the rest of the gateway.
package chatgpt

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
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

// Client is the ChatGPT subscription Provider. Construct with New /
// NewWithBaseURL. The authenticator is typically a *auth.ChatGPTOAuth.
type Client struct {
	baseURL    string
	auth       auth.Authenticator
	httpClient *http.Client

	// streamRetries is the number of additional attempts made when a stream
	// drops transiently before any chunk has reached the consumer (the
	// "transient empty stream" failure the Responses backend occasionally
	// returns). streamRetryWait is the base for exponential backoff between
	// attempts. Both are fields so tests can shrink them.
	streamRetries   int
	streamRetryWait time.Duration
}

// Defaults for transient-empty-stream retry. Two retries with a 250ms base
// backoff (250ms then 500ms) recovers the occasional empty stream without
// noticeably delaying a genuine upstream failure. Overridable via the Client
// fields in tests.
const (
	defaultStreamRetries   = 2
	defaultStreamRetryWait = 250 * time.Millisecond
)

// New constructs a Client pointed at the production ChatGPT codex endpoint.
func New(authenticator auth.Authenticator) *Client {
	return NewWithBaseURL(authenticator, auth.ChatGPTAPIBase)
}

// NewWithBaseURL constructs a Client pointed at a custom base URL. Used in
// tests with httptest.NewServer.
func NewWithBaseURL(authenticator auth.Authenticator, baseURL string) *Client {
	return &Client{
		baseURL:         strings.TrimRight(baseURL, "/"),
		auth:            authenticator,
		httpClient:      &http.Client{Timeout: 300 * time.Second},
		streamRetries:   defaultStreamRetries,
		streamRetryWait: defaultStreamRetryWait,
	}
}

// Name returns the canonical provider identifier.
func (c *Client) Name() string { return "chatgpt" }

// AuthMode returns subscription. Cost per request is $0.
func (c *Client) AuthMode() string { return agentmodel.AuthModeSubscription }

// SupportedModels returns the chat model identifiers this provider serves
// against the ChatGPT subscription endpoint. Embeddings are not supported.
func (c *Client) SupportedModels() []string {
	return []string{"gpt-5", "gpt-5-pro", "gpt-5-codex"}
}

// ListModels returns the live model catalog offered to this ChatGPT
// subscription account. The Codex backend uses {"models":[{"slug":...}]}
// rather than the public OpenAI API's {"data":[{"id":...}]} shape.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	models, err := c.listModelsOnce(ctx)
	if err != nil && c.recoverExpiredToken(ctx, err) {
		models, err = c.listModelsOnce(ctx)
	}
	return models, err
}

func (c *Client) listModelsOnce(ctx context.Context) ([]string, error) {
	u, err := url.Parse(c.baseURL + "/models")
	if err != nil {
		return nil, fmt.Errorf("chatgpt: build models URL: %w", err)
	}
	query := u.Query()
	query.Set("client_version", auth.ChatGPTClientVersion)
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("chatgpt: build models request: %w", err)
	}
	if err := c.auth.Apply(ctx, req); err != nil {
		return nil, fmt.Errorf("chatgpt: apply auth: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("chatgpt: list models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		return nil, &apiError{
			statusCode: resp.StatusCode,
			status:     resp.Status,
			body:       strings.TrimSpace(string(body)),
		}
	}

	var payload struct {
		Models []struct {
			Slug string `json:"slug"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("chatgpt: decode models response: %w", err)
	}
	seen := make(map[string]bool, len(payload.Models))
	models := make([]string, 0, len(payload.Models))
	for _, model := range payload.Models {
		slug := strings.TrimSpace(model.Slug)
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true
		models = append(models, slug)
	}
	if len(models) == 0 {
		return nil, errors.New("chatgpt: models response contained no model slugs")
	}
	return models, nil
}

var _ provider.ModelLister = (*Client)(nil)

// ─── Responses API request/response wire types ───────────────────────────

// responsesRequest is the request body for POST /responses.
type responsesRequest struct {
	Model          string          `json:"model"`
	Input          []any           `json:"input"`
	Instructions   string          `json:"instructions,omitempty"`
	Stream         bool            `json:"stream,omitempty"`
	Tools          []responsesTool `json:"tools,omitempty"`
	ToolChoice     any             `json:"tool_choice,omitempty"`
	Temperature    *float64        `json:"temperature,omitempty"`
	MaxTokens      *int            `json:"max_output_tokens,omitempty"`
	TopP           *float64        `json:"top_p,omitempty"`
	User           string          `json:"user,omitempty"`
	PromptCacheKey string          `json:"prompt_cache_key,omitempty"`
	Store          bool            `json:"store"`
}

// responsesMessage is one chat-style message entry in input[].
type responsesMessage struct {
	Role    string                 `json:"role"`
	Content []responsesContentPart `json:"content"`
}

// responsesFunctionCall is an assistant tool-call history item in Responses
// API input[]. Chat Completions carries this inside assistant.tool_calls.
type responsesFunctionCall struct {
	Type      string `json:"type"`
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// responsesFunctionCallOutput is a tool-result history item in Responses API
// input[]. Chat Completions carries this as a role:"tool" message.
type responsesFunctionCallOutput struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

// responsesTool is the flat Responses API function-tool shape. Chat
// Completions nests the same schema under {type:"function", function:{...}}.
type responsesTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// responsesContentPart is one Responses API message content item.
type responsesContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

type chatContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	ImageURL json.RawMessage `json:"image_url,omitempty"`
	Detail   string          `json:"detail,omitempty"`
}

type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails *struct {
		CachedTokens int `json:"cached_tokens,omitempty"`
	} `json:"input_tokens_details,omitempty"`
	OutputTokensDetails struct {
		ReasoningTokens *int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// cachedTokens returns the cached prompt-token count (or 0 if absent).
func (u responsesUsage) cachedTokens() int {
	if u.InputTokensDetails == nil {
		return 0
	}
	return u.InputTokensDetails.CachedTokens
}

// ─── Translation ─────────────────────────────────────────────────────────

// buildRequest converts an OpenAI ChatRequest into the Responses API shape.
// System/developer messages are concatenated into instructions. User messages
// become input_text items, assistant text history becomes output_text items,
// assistant tool-call history becomes function_call items, and role:"tool"
// results become function_call_output items.
func buildRequest(req agentmodel.ChatRequest, stream bool) (responsesRequest, error) {
	var (
		systemParts []string
		inputs      []any
	)
	for i, m := range req.Messages {
		if m.Role == "system" || m.Role == "developer" {
			if m.Content != "" {
				systemParts = append(systemParts, m.Content)
			}
			continue
		}
		if err := appendMessageInput(&inputs, m); err != nil {
			return responsesRequest{}, fmt.Errorf("message %d: %w", i, err)
		}
	}
	instructions := strings.Join(systemParts, "\n")
	if instructions == "" {
		instructions = "You are a helpful assistant."
	}
	// The ChatGPT codex backend (chatgpt.com/backend-api/codex) accepts only the
	// minimal sampling parameter set the Codex CLI sends. It rejects
	// max_output_tokens, temperature, and top_p with a 400
	// "Unsupported parameter: ..." (gpt-5.x are reasoning models), so none of
	// those are forwarded even when the caller supplies them. This provider only
	// ever talks to that backend.
	return responsesRequest{
		Model:          req.Model,
		Input:          inputs,
		Instructions:   instructions,
		Stream:         stream,
		Tools:          convertTools(req.Tools),
		ToolChoice:     req.ToolChoice,
		User:           req.User,
		PromptCacheKey: req.PromptCacheKey,
		Store:          false,
	}, nil
}

// stableSessionID derives the value for the codex `session-id` request header.
//
// The ChatGPT/codex backend (chatgpt.com/backend-api/codex) pins its prompt
// cache to the node that first served a given session-id and only serves a
// cached prefix back to requests carrying the SAME session-id. Requests without
// one are load-balanced across nodes, so the large stable prefix (system +
// tools + early history) only caches by luck — the real Codex CLI reaches
// 30–80% cache hits by sending a per-session UUID, while a header-less gateway
// sees near-zero. The body prompt_cache_key has no effect on this
// backend; the header is what matters.
//
// This gateway is stateless (each HTTP turn is independent), so it cannot use a
// per-session UUID. Instead it derives a deterministic id from the parts of the
// request that stay byte-identical across the turns of one conversation but
// differ across conversations: the system/developer instructions, the tool
// schemas, and the FIRST non-system message (the anchor — later messages grow
// every turn, so they are excluded). All turns of a conversation therefore map
// to one id and route to one node; a client that already knows its session can
// override the derivation by setting PromptCacheKey.
func stableSessionID(req agentmodel.ChatRequest) string {
	if req.PromptCacheKey != "" {
		return req.PromptCacheKey
	}
	h := sha256.New()
	for _, m := range req.Messages {
		if m.Role == "system" || m.Role == "developer" {
			_, _ = io.WriteString(h, m.Content)
			_, _ = h.Write([]byte{0})
		}
	}
	_, _ = h.Write([]byte{1})
	if b, err := json.Marshal(req.Tools); err == nil {
		_, _ = h.Write(b)
	}
	_, _ = h.Write([]byte{2})
	for _, m := range req.Messages {
		if m.Role != "system" && m.Role != "developer" {
			_, _ = io.WriteString(h, m.Role)
			_, _ = io.WriteString(h, m.Content)
			break
		}
	}
	sum := h.Sum(nil)
	var b [16]byte
	copy(b[:], sum)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func appendMessageInput(inputs *[]any, m agentmodel.Message) error {
	switch m.Role {
	case "assistant":
		content, err := responsesContentFromMessage(m, "output_text")
		if err != nil {
			return err
		}
		if len(content) > 0 {
			*inputs = append(*inputs, responsesMessage{
				Role:    "assistant",
				Content: content,
			})
		}
		for _, tc := range m.ToolCalls {
			callID, itemID := splitResponsesToolCallID(tc.ID)
			*inputs = append(*inputs, responsesFunctionCall{
				Type:      "function_call",
				ID:        itemID,
				CallID:    callID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
	case "tool":
		callID, _ := splitResponsesToolCallID(m.ToolCallID)
		*inputs = append(*inputs, responsesFunctionCallOutput{
			Type:   "function_call_output",
			CallID: callID,
			Output: m.Content,
		})
	default:
		content, err := responsesContentFromMessage(m, "input_text")
		if err != nil {
			return err
		}
		*inputs = append(*inputs, responsesMessage{
			Role:    m.Role,
			Content: content,
		})
	}
	return nil
}

func responsesContentFromMessage(m agentmodel.Message, textType string) ([]responsesContentPart, error) {
	parts, ok, err := rawChatContentParts(m)
	if err != nil {
		return nil, err
	}
	if !ok {
		if m.Content == "" {
			return nil, nil
		}
		return []responsesContentPart{{Type: textType, Text: m.Content}}, nil
	}
	out := make([]responsesContentPart, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "", "text", "input_text":
			if p.Text != "" {
				out = append(out, responsesContentPart{Type: textType, Text: p.Text})
			}
		case "image_url":
			imageURL, detail, err := parseImageURLPart(p)
			if err != nil {
				return nil, err
			}
			if imageURL == "" {
				return nil, agentmodel.NewErrorf(agentmodel.ErrTypeInvalidRequest, "image_url content part is missing image_url.url")
			}
			out = append(out, responsesContentPart{Type: "input_image", ImageURL: imageURL, Detail: detail})
		case "input_image":
			imageURL, detail, err := parseImageURLPart(p)
			if err != nil {
				return nil, err
			}
			if imageURL == "" {
				return nil, agentmodel.NewErrorf(agentmodel.ErrTypeInvalidRequest, "input_image content part is missing image_url")
			}
			out = append(out, responsesContentPart{Type: "input_image", ImageURL: imageURL, Detail: detail})
		default:
			return nil, agentmodel.NewErrorf(agentmodel.ErrTypeInvalidRequest, "unsupported content part type %q", p.Type)
		}
	}
	return out, nil
}

func parseImageURLPart(p chatContentPart) (string, string, error) {
	if len(p.ImageURL) == 0 {
		return "", p.Detail, nil
	}
	var url string
	if err := json.Unmarshal(p.ImageURL, &url); err == nil {
		return url, p.Detail, nil
	}
	var obj struct {
		URL    string `json:"url"`
		Detail string `json:"detail,omitempty"`
	}
	if err := json.Unmarshal(p.ImageURL, &obj); err != nil {
		return "", "", agentmodel.NewErrorf(agentmodel.ErrTypeInvalidRequest, "image_url content part has invalid image_url")
	}
	detail := p.Detail
	if detail == "" {
		detail = obj.Detail
	}
	return obj.URL, detail, nil
}

func rawChatContentParts(m agentmodel.Message) ([]chatContentPart, bool, error) {
	encoded, err := json.Marshal(m)
	if err != nil {
		return nil, false, fmt.Errorf("marshal message content: %w", err)
	}
	var aux struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(encoded, &aux); err != nil {
		return nil, false, fmt.Errorf("decode message content: %w", err)
	}
	trimmed := bytes.TrimSpace(aux.Content)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, false, nil
	}
	var parts []chatContentPart
	if err := json.Unmarshal(trimmed, &parts); err != nil {
		return nil, false, fmt.Errorf("decode content parts: %w", err)
	}
	return parts, true, nil
}

func convertTools(tools []agentmodel.Tool) []responsesTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]responsesTool, 0, len(tools))
	for _, tool := range tools {
		out = append(out, responsesTool{
			Type:        "function",
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			Parameters:  sanitizeJSONSchema(tool.Function.Parameters),
		})
	}
	return out
}

func sanitizeJSONSchema(schema map[string]any) map[string]any {
	if schema == nil {
		return nil
	}
	sanitized, _ := sanitizeJSONSchemaValue(schema).(map[string]any)
	return sanitized
}

func sanitizeJSONSchemaValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, value := range x {
			if k == "pattern" {
				pattern, ok := value.(string)
				if ok && containsRegexLookaround(pattern) {
					continue
				}
			}
			out[k] = sanitizeJSONSchemaValue(value)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = sanitizeJSONSchemaValue(item)
		}
		return out
	default:
		return v
	}
}

func containsRegexLookaround(pattern string) bool {
	return strings.Contains(pattern, "(?=") ||
		strings.Contains(pattern, "(?!") ||
		strings.Contains(pattern, "(?<=") ||
		strings.Contains(pattern, "(?<!")
}

func splitResponsesToolCallID(id string) (callID string, itemID string) {
	callID = id
	if before, after, ok := strings.Cut(id, "|"); ok {
		callID = before
		itemID = after
	}
	return callID, itemID
}

func joinResponsesToolCallID(callID, itemID string) string {
	if itemID == "" {
		return callID
	}
	return callID + "|" + itemID
}

func ptr(i int) *int { return &i }

// summarizeStreamFailure extracts a human-readable reason from a terminal
// stream-failure event payload (error / response.failed / response.incomplete).
// It tolerates the several shapes the Responses backend uses and falls back to
// the raw payload when none match.
func summarizeStreamFailure(data string) string {
	var ev struct {
		// Top-level "error" event.
		Message string `json:"message"`
		Code    string `json:"code"`
		// response.failed / response.incomplete wrap details under response.
		Response struct {
			Error *struct {
				Message string `json:"message"`
				Code    string `json:"code"`
			} `json:"error"`
			IncompleteDetails *struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
			Status string `json:"status"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(data), &ev); err == nil {
		switch {
		case ev.Message != "":
			return ev.Message
		case ev.Response.Error != nil && ev.Response.Error.Message != "":
			return ev.Response.Error.Message
		case ev.Response.IncompleteDetails != nil && ev.Response.IncompleteDetails.Reason != "":
			return ev.Response.IncompleteDetails.Reason
		case ev.Response.Status != "":
			return ev.Response.Status
		case ev.Code != "":
			return ev.Code
		}
	}
	return strings.TrimSpace(data)
}

// ─── HTTP plumbing ───────────────────────────────────────────────────────

// newRequest builds an HTTP request with the auth + Codex headers applied. body
// is the already-marshaled request payload (marshaled once by the caller so a
// refresh-retry reuses it rather than re-encoding).
func (c *Client) newRequest(ctx context.Context, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("chatgpt: build request: %w", err)
	}
	if err := c.auth.Apply(ctx, req); err != nil {
		return nil, err
	}
	// Set the Responses API beta header. The auth.Apply call already set
	// User-Agent and Originator (and ChatGPT-Account-Id when known).
	req.Header.Set("OpenAI-Beta", "responses=v1")
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// apiError is a non-2xx HTTP response from the Responses backend. It preserves
// the status code so the auth-recovery path can recognize a 401, while its
// Error() string matches the legacy "chatgpt: <status>: <body>" format callers
// and tests already key on.
type apiError struct {
	statusCode int
	status     string // e.g. "401 Unauthorized"
	body       string
	// retryAfter is the upstream's own "come back at", read off a 429's headers
	// or body. Zero for every other status.
	retryAfter time.Duration
}

func (e *apiError) Error() string {
	return fmt.Sprintf("chatgpt: %s: %s", e.status, e.body)
}

// RetryAfterHint implements wire.RetryAfterHinter so agentmodel.Wrap copies the
// upstream's window onto the classified error, where the pool and router use it
// to size their cooldowns.
func (e *apiError) RetryAfterHint() time.Duration { return e.retryAfter }

// UpstreamStatus implements wire.UpstreamStatusHinter so agentmodel.Wrap
// classifies by the status we already hold rather than by substrings of the
// body Error() interpolates. See the same method on the anthropic adapter's
// statusError for why the body cannot be trusted to name its own family.
func (e *apiError) UpstreamStatus() int { return e.statusCode }

// retryAfterFromBody reads the reset hint the codex backend puts in a 429 body
// rather than in a header: {"error":{"type":"usage_limit_reached",
// "resets_in_seconds":N}}. This is the signal that matters for a pooled
// subscription — plan exhaustion, not per-minute throttling — and it arrives
// with no Retry-After at all. Returns 0 when the body has no usable field.
func retryAfterFromBody(body string) time.Duration {
	var parsed struct {
		Error struct {
			ResetsInSeconds *float64 `json:"resets_in_seconds"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return 0
	}
	if parsed.Error.ResetsInSeconds == nil {
		return 0
	}
	return agentmodel.ClampRetryAfter(time.Duration(*parsed.Error.ResetsInSeconds * float64(time.Second)))
}

// isTokenExpired reports whether this is the upstream "access token expired"
// 401 that a forced credential refresh can recover. It is deliberately narrow
// (not every 401): a refresh only helps when the token itself is stale, not
// when the account or scope is rejected.
func (e *apiError) isTokenExpired() bool {
	if e.statusCode != http.StatusUnauthorized {
		return false
	}
	// "token_expired" is the precise code the codex backend returns; matching
	// the broader "expired" substring also covers wording variants and is a
	// strict superset, so the single check is equivalent to (and simpler than)
	// testing both.
	return strings.Contains(strings.ToLower(e.body), "expired")
}

// forceRefresher is the optional capability an Authenticator advertises when it
// can re-mint its access token on demand. *auth.ChatGPTOAuth implements it; a
// static key does not (and so never triggers a retry).
type forceRefresher interface {
	ForceRefresh(ctx context.Context) error
}

// recoverExpiredToken reports whether err is an expired-token 401 that a forced
// refresh recovered, in which case the caller retries the request once. The
// refresh is performed here as a side effect; a false return means either the
// error wasn't an expired-token 401 or the refresh failed.
func (c *Client) recoverExpiredToken(ctx context.Context, err error) bool {
	var ae *apiError
	if !errors.As(err, &ae) || !ae.isTokenExpired() {
		return false
	}
	fr, ok := c.auth.(forceRefresher)
	if !ok {
		return false
	}
	return fr.ForceRefresh(ctx) == nil
}

// Complete executes a non-streaming chat completion. The ChatGPT codex backend
// (chatgpt.com/backend-api/codex) is stream-only — it rejects any request
// without stream:true with 400 "Stream must be set to true" — so Complete
// cannot issue a buffered request. Instead it drives the streaming path and
// aggregates the SSE deltas (text and tool-call fragments) into a single
// ChatResponse, so callers still get a normal non-streaming result. Stream also
// carries the expired-token refresh, transient-empty-stream retry, and
// terminal-failure surfacing, which Complete inherits for free.
//
// ID, Model, and Created are intentionally left zero: StreamChunk carries no
// such fields. The router stamps Model and the HTTP handler backfills ID, so
// the non-streaming contract callers see is unchanged.
func (c *Client) Complete(ctx context.Context, in agentmodel.ChatRequest) (agentmodel.ChatResponse, error) {
	seq, err := c.Stream(ctx, in)
	if err != nil {
		return agentmodel.ChatResponse{}, err
	}

	var (
		content      strings.Builder
		role         = "assistant"
		finishReason string
		usage        agentmodel.Usage
		toolCalls    []agentmodel.ToolCall
		toolIndexes  = map[int]int{}
	)
	for chunk, err := range seq {
		if err != nil {
			// The stream surfaces upstream failures (response.failed/error) and
			// decode errors as a terminal error. Propagate it and discard any
			// partial output to preserve Complete's all-or-nothing contract.
			return agentmodel.ChatResponse{}, err
		}
		if chunk.Delta.Role != "" {
			role = chunk.Delta.Role
		}
		content.WriteString(chunk.Delta.Content)
		toolCalls = mergeToolCallDeltas(toolCalls, toolIndexes, chunk.Delta.ToolCalls)
		if chunk.FinishReason != "" {
			finishReason = chunk.FinishReason
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
	}
	if finishReason == "" {
		finishReason = "stop"
	}

	return agentmodel.ChatResponse{
		Object: "chat.completion",
		Choices: []agentmodel.Choice{{
			Index: 0,
			Message: agentmodel.Message{
				Role:      role,
				Content:   content.String(),
				ToolCalls: toolCalls,
			},
			FinishReason: finishReason,
		}},
		Usage: usage,
	}, nil
}

// mergeToolCallDeltas folds streaming tool-call fragments into accumulated tool
// calls keyed by their stream index: the first fragment for an index carries
// the ID/type/name and later fragments append argument text. This reconstructs
// the same shape the non-streaming output[] would have yielded.
func mergeToolCallDeltas(acc []agentmodel.ToolCall, byIndex map[int]int, deltas []agentmodel.ToolCall) []agentmodel.ToolCall {
	for _, d := range deltas {
		idx := len(acc)
		if d.Index != nil {
			idx = *d.Index
		}
		pos, ok := byIndex[idx]
		if !ok {
			pos = len(acc)
			byIndex[idx] = pos
			acc = append(acc, agentmodel.ToolCall{Index: d.Index})
		}
		tc := &acc[pos]
		if tc.Index == nil {
			tc.Index = d.Index
		}
		if d.ID != "" {
			tc.ID = d.ID
		}
		if d.Type != "" {
			tc.Type = d.Type
		}
		if d.Function.Name != "" {
			tc.Function.Name = d.Function.Name
		}
		tc.Function.Arguments += d.Function.Arguments
	}
	return acc
}

// marshalRequest builds and JSON-encodes the Responses API request body once,
// so the refresh-retry path reuses the bytes instead of re-encoding.
func marshalRequest(in agentmodel.ChatRequest, stream bool) ([]byte, error) {
	reqBody, err := buildRequest(in, stream)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("chatgpt: marshal body: %w", err)
	}
	return body, nil
}

// Stream executes a streaming chat-completion call. It maps Responses API SSE
// events back to OpenAI chat-completion chunks, including tool-call deltas.
func (c *Client) Stream(ctx context.Context, in agentmodel.ChatRequest) (iter.Seq2[provider.StreamChunk, error], error) {
	body, err := marshalRequest(in, true)
	if err != nil {
		return nil, err
	}
	sessionID := stableSessionID(in)

	// Establish the first stream eagerly so a non-2xx status (or transport
	// error) surfaces from Stream() itself, letting the router fall back to
	// another deployment before any streaming has begun. An expired-token 401
	// here is recovered with one forced refresh + re-open before giving up.
	resp, err := c.doStream(ctx, body, sessionID)
	if err != nil && c.recoverExpiredToken(ctx, err) {
		resp, err = c.doStream(ctx, body, sessionID)
	}
	if err != nil {
		return nil, err
	}

	// seq drives one or more upstream attempts. A fresh attempt is made only
	// when the previous one dropped transiently before any chunk reached the
	// consumer — the "Stream ended without finish_reason" / transient-empty
	// failure the Responses backend occasionally returns. Once a chunk has been
	// emitted downstream we can no longer retry (the client already holds
	// partial output), so such a drop is surfaced as an error instead.
	seq := func(yield func(provider.StreamChunk, error) bool) {
		current := resp
		for attempt := 0; ; attempt++ {
			emitted, aborted, retryable, cErr := c.consumeStream(current, yield)
			if aborted {
				return
			}
			if cErr == nil {
				return // response.completed reached
			}
			if !retryable || emitted || attempt >= c.streamRetries {
				yield(provider.StreamChunk{}, cErr)
				return
			}
			// Transient pre-emit drop with attempts left: back off, then open a
			// fresh upstream stream and start over from a clean slate.
			select {
			case <-ctx.Done():
				yield(provider.StreamChunk{}, ctx.Err())
				return
			case <-time.After(c.streamRetryWait << attempt):
			}
			next, rErr := c.doStream(ctx, body, sessionID)
			if rErr != nil {
				yield(provider.StreamChunk{}, rErr)
				return
			}
			current = next
		}
	}
	return seq, nil
}

// doStream opens a single streaming Responses request and returns the live
// response on a 2xx status, closing the body and returning an error otherwise.
func (c *Client) doStream(ctx context.Context, body []byte, sessionID string) (*http.Response, error) {
	req, err := c.newRequest(ctx, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	// The codex backend pins prompt-cache routing to session-id; a stable value
	// across a conversation's turns is what makes the prefix actually cache
	//. Empty for one-shot calls (e.g. image generation).
	if sessionID != "" {
		req.Header.Set("session-id", sessionID)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("chatgpt: do: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		body := strings.TrimSpace(string(errBody))
		retryAfter := agentmodel.RetryAfterFor(resp, time.Now())
		if retryAfter == 0 && resp.StatusCode == http.StatusTooManyRequests {
			retryAfter = retryAfterFromBody(body)
		}
		_ = resp.Body.Close()
		return nil, &apiError{statusCode: resp.StatusCode, status: resp.Status, body: body, retryAfter: retryAfter}
	}
	return resp, nil
}

// consumeStream reads one SSE response, mapping Responses API events to
// StreamChunks via yield. It reports:
//
//	emitted   - at least one content/tool chunk was delivered to the consumer
//	aborted   - the consumer asked to stop (yield returned false)
//	retryable - the failure was a transient drop (clean EOF or read error) that
//	            a fresh attempt may recover; only meaningful when err != nil
//	err       - the terminal error, or nil if response.completed was reached
//
// On a terminal error consumeStream does not itself yield it — the caller
// decides whether to retry or surface it.
func (c *Client) consumeStream(resp *http.Response, yield func(provider.StreamChunk, error) bool) (emitted, aborted, retryable bool, err error) {
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	// SSE events can have large payloads (full response on completed event).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var (
		eventType       string
		dataBuf         strings.Builder
		nextToolIndex   int
		sawToolCall     bool
		completed       bool
		toolCallIndexes = map[string]int{}
	)
	// emit forwards a content chunk and records that output has begun, so the
	// caller knows a subsequent drop can no longer be retried.
	emit := func(ch provider.StreamChunk) bool {
		emitted = true
		if !yield(ch, nil) {
			aborted = true
			return false
		}
		return true
	}
	// fail records a terminal, non-retryable error and stops the scan. The
	// caller surfaces it once.
	fail := func(e error) bool {
		err = e
		return false
	}
	dispatch := func() bool {
		if dataBuf.Len() == 0 {
			eventType = ""
			return true
		}
		data := dataBuf.String()
		dataBuf.Reset()
		et := eventType
		eventType = ""

		// Determine event type. Prefer the explicit event: header, fall
		// back to the JSON payload's "type" field.
		if et == "" {
			var probe struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(data), &probe); err == nil {
				et = probe.Type
			}
		}

		switch et {
		case "response.output_item.added":
			var ev struct {
				Item struct {
					Type      string `json:"type"`
					ID        string `json:"id"`
					CallID    string `json:"call_id"`
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"item"`
			}
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				return fail(fmt.Errorf("chatgpt: decode output item added: %w", err))
			}
			if ev.Item.Type != "function_call" {
				return true
			}
			sawToolCall = true
			idx := nextToolIndex
			nextToolIndex++
			if ev.Item.ID != "" {
				toolCallIndexes[ev.Item.ID] = idx
			}
			if ev.Item.CallID != "" {
				toolCallIndexes[ev.Item.CallID] = idx
			}
			return emit(provider.StreamChunk{
				Delta: agentmodel.Message{Role: "assistant", ToolCalls: []agentmodel.ToolCall{{
					Index: ptr(idx),
					ID:    joinResponsesToolCallID(ev.Item.CallID, ev.Item.ID),
					Type:  "function",
					Function: agentmodel.ToolCallFunction{
						Name:      ev.Item.Name,
						Arguments: ev.Item.Arguments,
					},
				}}},
			})

		case "response.function_call_arguments.delta":
			var ev struct {
				ItemID string `json:"item_id"`
				CallID string `json:"call_id"`
				Delta  string `json:"delta"`
			}
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				return fail(fmt.Errorf("chatgpt: decode function call delta: %w", err))
			}
			idx, ok := toolCallIndexes[ev.ItemID]
			if ev.CallID != "" {
				if callIdx, callOK := toolCallIndexes[ev.CallID]; callOK {
					idx = callIdx
					ok = true
				}
			}
			if !ok {
				return fail(fmt.Errorf("chatgpt: function call delta for unknown item_id %q call_id %q", ev.ItemID, ev.CallID))
			}
			return emit(provider.StreamChunk{
				Delta: agentmodel.Message{Role: "assistant", ToolCalls: []agentmodel.ToolCall{{
					Index:    ptr(idx),
					Function: agentmodel.ToolCallFunction{Arguments: ev.Delta},
				}}},
			})

		case "response.output_text.delta":
			var ev struct {
				Delta string `json:"delta"`
			}
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				return fail(fmt.Errorf("chatgpt: decode delta: %w", err))
			}
			if ev.Delta == "" {
				return true
			}
			return emit(provider.StreamChunk{
				Delta: agentmodel.Message{Role: "assistant", Content: ev.Delta},
			})

		case "response.completed":
			var ev struct {
				Response struct {
					Usage responsesUsage `json:"usage"`
				} `json:"response"`
			}
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				return fail(fmt.Errorf("chatgpt: decode completed: %w", err))
			}
			usage := &agentmodel.Usage{
				PromptTokens:         ev.Response.Usage.InputTokens,
				CompletionTokens:     ev.Response.Usage.OutputTokens,
				TotalTokens:          ev.Response.Usage.TotalTokens,
				ReasoningTokens:      ev.Response.Usage.OutputTokensDetails.ReasoningTokens,
				CacheReadInputTokens: ev.Response.Usage.cachedTokens(),
				CostUSD:              0,
				AuthMode:             agentmodel.AuthModeSubscription,
			}
			finishReason := "stop"
			if sawToolCall {
				finishReason = "tool_calls"
			}
			completed = true
			// Terminal success chunk. Not routed through emit: it carries no
			// content, and the loop stops regardless of the consumer's reply.
			if !yield(provider.StreamChunk{FinishReason: finishReason, Usage: usage}, nil) {
				aborted = true
			}
			return false

		case "error", "response.failed", "response.incomplete":
			// Explicit terminal failure events from the Responses backend.
			// Without surfacing these the stream would end with no
			// finish_reason and no error, which the downstream OpenAI client
			// reports as "Stream ended without finish_reason". These are a
			// definite upstream verdict (not a transient empty stream), so
			// surface them without retrying.
			return fail(fmt.Errorf("chatgpt: stream %s: %s", et, summarizeStreamFailure(data)))

		default:
			// response.created, response.output_text.done, and any
			// other event types are not surfaced for MVP.
			return true
		}
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			// Blank line terminates an event.
			if !dispatch() {
				return
			}
			continue
		}
		// No default: anything that is neither an event nor a data line is
		// ignored, which is what the SSE spec asks for with comment lines (":"
		// prefix, used upstream as keep-alives) and with fields we do not
		// consume (id:, retry:).
		switch {
		case strings.HasPrefix(line, "event:"):
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	// Flush any trailing event without a terminating blank line.
	if dataBuf.Len() > 0 {
		if !dispatch() {
			return emitted, aborted, retryable, err
		}
	}
	if rerr := scanner.Err(); rerr != nil {
		// A read error mid-stream is a transient transport drop; a fresh
		// attempt may recover if nothing was emitted yet.
		return emitted, aborted, true, fmt.Errorf("chatgpt: stream read: %w", rerr)
	}
	// The Responses backend can close the connection without ever emitting
	// response.completed (or a terminal failure event) — the transient empty
	// stream behind "Stream ended without finish_reason". Treat it as a
	// retryable drop so the caller can re-establish from a clean slate.
	if !completed {
		return emitted, aborted, true, fmt.Errorf("chatgpt: stream ended without response.completed")
	}
	return emitted, aborted, retryable, err
}

// ResponsesPassthrough forwards a caller-authored Responses API request to the
// Codex backend while replacing only its logical model. Authentication is
// always applied from this Client; caller headers never enter this interface.
func (c *Client) ResponsesPassthrough(ctx context.Context, body []byte, modelOverride string) (*http.Response, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, agentmodel.NewErrorf(agentmodel.ErrTypeInvalidRequest, "chatgpt: invalid Responses JSON: %v", err)
	}
	model, err := json.Marshal(modelOverride)
	if err != nil {
		return nil, fmt.Errorf("chatgpt: marshal model override: %w", err)
	}
	payload["model"] = model
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("chatgpt: marshal Responses request: %w", err)
	}
	resp, err := c.doStream(ctx, rewritten, "")
	if err != nil && c.recoverExpiredToken(ctx, err) {
		resp, err = c.doStream(ctx, rewritten, "")
	}
	return resp, err
}

// Embed is not supported by the ChatGPT subscription endpoint.
func (c *Client) Embed(_ context.Context, _ agentmodel.EmbeddingRequest) (agentmodel.EmbeddingResponse, error) {
	return agentmodel.EmbeddingResponse{}, provider.ErrNotSupported
}

// Compile-time interface check.
var (
	_ provider.Provider                     = (*Client)(nil)
	_ provider.ResponsesPassthroughProvider = (*Client)(nil)
)
