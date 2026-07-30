// Package agentmodel exposes the OpenAI-compatible request/response types used
// across the AgentModel gateway. All providers normalize to these shapes.
package wire

import (
	"bytes"
	"encoding/json"
)

// ChatRequest is the OpenAI-compatible chat-completion request.
type ChatRequest struct {
	Model          string          `json:"model"`
	Messages       []Message       `json:"messages"`
	Stream         bool            `json:"stream,omitempty"`
	Temperature    *float64        `json:"temperature,omitempty"`
	MaxTokens      *int            `json:"max_tokens,omitempty"`
	TopP           *float64        `json:"top_p,omitempty"`
	Stop           []string        `json:"stop,omitempty"`
	Tools          []Tool          `json:"tools,omitempty"`
	ToolChoice     any             `json:"tool_choice,omitempty"`
	User           string          `json:"user,omitempty"`
	PromptCacheKey string          `json:"prompt_cache_key,omitempty"`
	Thinking       *ThinkingConfig `json:"thinking,omitempty"`
	// ReasoningEffort enables adaptive thinking: "low"|"medium"|"high"|"max".
	// "max" is only valid on Opus 4.6. Mirrors LiteLLM's reasoning_effort param.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

// Message is a single chat message. `Role` is "system", "user", "assistant", or "tool".
//
// CacheControl is an optional Anthropic-style hint indicating the message
// should be cached for prompt-caching purposes (e.g.
// `{"type": "ephemeral"}`). Providers that don't support explicit caching
// (OpenAI auto-caches; Gemini uses a separate API) ignore this field.
type Message struct {
	Role             string          `json:"role,omitempty"`
	Content          string          `json:"content,omitempty"`
	ToolCalls        []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	Name             string          `json:"name,omitempty"`
	CacheControl     any             `json:"cache_control,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	ThinkingBlocks   []ThinkingBlock `json:"thinking_blocks,omitempty"`

	// rawContent preserves OpenAI-compatible non-string content (usually an
	// array of content parts) so agentmodel can accept and forward modern chat
	// requests instead of failing JSON decode with "cannot unmarshal array into
	// ... content of type string". Content remains populated with the textual
	// projection for providers that only understand text.
	rawContent json.RawMessage
}

// UnmarshalJSON accepts both legacy string content and modern OpenAI content
// part arrays. For arrays, rawContent is preserved for re-marshalling and
// Content is populated with a best-effort text projection.
func (m *Message) UnmarshalJSON(data []byte) error {
	var aux struct {
		Role             string          `json:"role"`
		Content          json.RawMessage `json:"content"`
		ToolCalls        []ToolCall      `json:"tool_calls,omitempty"`
		ToolCallID       string          `json:"tool_call_id,omitempty"`
		Name             string          `json:"name,omitempty"`
		CacheControl     any             `json:"cache_control,omitempty"`
		ReasoningContent string          `json:"reasoning_content,omitempty"`
		ThinkingBlocks   []ThinkingBlock `json:"thinking_blocks,omitempty"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*m = Message{
		Role:             aux.Role,
		ToolCalls:        aux.ToolCalls,
		ToolCallID:       aux.ToolCallID,
		Name:             aux.Name,
		CacheControl:     aux.CacheControl,
		ReasoningContent: aux.ReasoningContent,
		ThinkingBlocks:   aux.ThinkingBlocks,
	}
	if len(aux.Content) == 0 || bytes.Equal(aux.Content, []byte("null")) {
		return nil
	}
	var s string
	if err := json.Unmarshal(aux.Content, &s); err == nil {
		m.Content = s
		return nil
	}
	trimmed := bytes.TrimSpace(aux.Content)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		// Preserve the old validation behavior for unsupported content types:
		// OpenAI-compatible chat content is string or array, not object/number/etc.
		return json.Unmarshal(aux.Content, new(string))
	}
	m.rawContent = append(m.rawContent[:0], aux.Content...)
	m.Content = textFromRawContent(aux.Content)
	return nil
}

// MarshalJSON preserves raw non-string content when present; otherwise it emits
// the historical string content shape.
func (m Message) MarshalJSON() ([]byte, error) {
	out := make(map[string]any)
	if m.Role != "" {
		out["role"] = m.Role
	}
	if len(m.rawContent) > 0 {
		out["content"] = json.RawMessage(m.rawContent)
	} else if m.Content != "" {
		out["content"] = m.Content
	}
	if len(m.ToolCalls) > 0 {
		out["tool_calls"] = m.ToolCalls
	}
	if m.ToolCallID != "" {
		out["tool_call_id"] = m.ToolCallID
	}
	if m.Name != "" {
		out["name"] = m.Name
	}
	if m.CacheControl != nil {
		out["cache_control"] = m.CacheControl
	}
	if m.ReasoningContent != "" {
		out["reasoning_content"] = m.ReasoningContent
	}
	if len(m.ThinkingBlocks) > 0 {
		out["thinking_blocks"] = m.ThinkingBlocks
	}
	return json.Marshal(out)
}

func textFromRawContent(raw json.RawMessage) string {
	parts, err := parseRawContentParts(raw)
	if err != nil {
		return ""
	}
	var buf bytes.Buffer
	for _, p := range parts {
		if p.Text != "" && p.isTextPart() {
			buf.WriteString(p.Text)
		}
	}
	return buf.String()
}

// ValidateTextOnlyContent fails fast when a request contains OpenAI content
// part arrays that include non-text modalities. Providers that only consume
// Message.Content should call this before translating the request; otherwise an
// image/file/etc. part would be silently dropped by the text projection.
func ValidateTextOnlyContent(req ChatRequest) *Error {
	for i, m := range req.Messages {
		if !m.rawContentTextOnly() {
			return NewErrorf(ErrTypeInvalidRequest, "message %d content contains non-text content parts; this provider only supports text content", i)
		}
	}
	return nil
}

type rawContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func parseRawContentParts(raw json.RawMessage) ([]rawContentPart, error) {
	var parts []rawContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, err
	}
	return parts, nil
}

func (p rawContentPart) isTextPart() bool {
	return p.Type == "" || p.Type == "text" || p.Type == "input_text"
}

func (m Message) rawContentTextOnly() bool {
	if len(m.rawContent) == 0 {
		return true
	}
	parts, err := parseRawContentParts(m.rawContent)
	if err != nil {
		return false
	}
	for _, p := range parts {
		if !p.isTextPart() {
			return false
		}
		if p.Type == "" && p.Text == "" {
			return false
		}
	}
	return true
}

// Tool describes a callable function available to the model.
type Tool struct {
	Type     string         `json:"type"` // "function" in MVP
	Function FunctionSchema `json:"function"`
}

// FunctionSchema is the JSON-Schema-shaped description of a tool function.
type FunctionSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// ToolCall represents a model's invocation of a tool.
type ToolCall struct {
	Index    *int             `json:"index,omitempty"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction holds the called function's name and argument JSON.
//
// Per OpenAI's spec, Arguments is a JSON-encoded string (NOT a parsed object).
// We preserve that to maintain wire compatibility.
type ToolCallFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ChatResponse is the OpenAI-compatible chat-completion response.
type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"` // "chat.completion"
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// Choice is a single completion candidate.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// Usage holds token counts and Agentica-specific cost / auth-mode extensions.
//
// CostUSD is 0 for subscription-based requests (Claude Pro/Max OAuth, ChatGPT
// subscription) since per-token cost is not meaningful when the user has paid
// a flat subscription fee. Token counts remain populated for observability
// regardless of auth mode.
//
// PromptTokens is the canonical input-token count and INCLUDES cached tokens
// (matching OpenAI's wire convention). CacheCreationInputTokens (Anthropic's
// "write to cache") and CacheReadInputTokens (any provider's "read from
// cache") are reported separately so cost calculation can apply different
// rates per category. When a provider doesn't break out cache counts (no
// caching support, or the response didn't hit the cache), both fields are 0.
type Usage struct {
	PromptTokens             int     `json:"prompt_tokens"`
	CompletionTokens         int     `json:"completion_tokens"`
	TotalTokens              int     `json:"total_tokens"`
	CacheCreationInputTokens int     `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int     `json:"cache_read_input_tokens,omitempty"`
	CostUSD                  float64 `json:"cost_usd,omitempty"`
	AuthMode                 string  `json:"auth_mode,omitempty"` // "api_key" or "subscription"
	// Provider is the serving provider's Name() (e.g. "groq"), set by the router.
	// It lets the price lookup try the catalog's canonical "<provider>/<model>"
	// key before falling back to the bare model id, so provider-specific models
	// are metered instead of logged unpriced.
	Provider string `json:"-"`
}

type promptTokensDetails struct {
	CachedTokens     int `json:"cached_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// MarshalJSON keeps the historical flat cache fields while also emitting the
// OpenAI-compatible nested prompt_tokens_details shape consumed by clients like
// pi-ai's openai-completions adapter.
func (u Usage) MarshalJSON() ([]byte, error) {
	type usageAlias Usage
	out := struct {
		usageAlias
		PromptTokensDetails *promptTokensDetails `json:"prompt_tokens_details,omitempty"`
	}{usageAlias: usageAlias(u)}
	if u.CacheReadInputTokens != 0 || u.CacheCreationInputTokens != 0 {
		out.PromptTokensDetails = &promptTokensDetails{
			CachedTokens:     u.CacheReadInputTokens,
			CacheWriteTokens: u.CacheCreationInputTokens,
		}
	}
	return json.Marshal(out)
}

// EmbeddingRequest is the OpenAI-compatible embedding request.
type EmbeddingRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
	User  string   `json:"user,omitempty"`
}

// EmbeddingResponse is the OpenAI-compatible embedding response.
type EmbeddingResponse struct {
	Object string      `json:"object"`
	Data   []Embedding `json:"data"`
	Model  string      `json:"model"`
	Usage  Usage       `json:"usage"`
}

// Embedding is one vector in an embedding response.
type Embedding struct {
	Object    string    `json:"object"`
	Index     int       `json:"index"`
	Embedding []float64 `json:"embedding"`
}

// ThinkingConfig enables budget-based extended thinking on Anthropic models
// that support it (e.g. claude-sonnet-4-5). Mirrors LiteLLM's wire shape.
type ThinkingConfig struct {
	Type         string `json:"type"` // "enabled"
	BudgetTokens int    `json:"budget_tokens"`
}

// ThinkingBlock is one reasoning block returned by Anthropic when thinking is
// active. Type is "thinking" (text visible) or "redacted_thinking" (opaque).
type ThinkingBlock struct {
	Type      string `json:"type"`
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
}

// AuthMode constants for Usage.AuthMode and request log attribution.
const (
	AuthModeAPIKey       = "api_key"
	AuthModeSubscription = "subscription"
)
