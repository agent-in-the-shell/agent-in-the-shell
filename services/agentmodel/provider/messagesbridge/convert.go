package messagesbridge

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/google/uuid"
)

// ─── Anthropic request wire types (the subset we translate) ──────────────────

type anthropicRequest struct {
	Model       string             `json:"model"`
	System      json.RawMessage    `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	MaxTokens   int                `json:"max_tokens"`
	Temperature *float64           `json:"temperature,omitempty"`
	TopP        *float64           `json:"top_p,omitempty"`
	StopSeqs    []string           `json:"stop_sequences,omitempty"`
	Stream      bool               `json:"stream,omitempty"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
	ToolChoice  json.RawMessage    `json:"tool_choice,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// anthropicBlock covers the request content blocks we care about: text,
// tool_use (assistant), and tool_result (user). Other block types are ignored.
type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

// anthropicToChatRequest decodes an Anthropic Messages request body into an
// agentmodel.ChatRequest, rewriting the model to modelOverride when non-empty.
func anthropicToChatRequest(body []byte, modelOverride string) (agentmodel.ChatRequest, error) {
	var ar anthropicRequest
	if err := json.Unmarshal(body, &ar); err != nil {
		return agentmodel.ChatRequest{}, fmt.Errorf("decode request: %w", err)
	}

	model := modelOverride
	if model == "" {
		model = ar.Model
	}

	var msgs []agentmodel.Message
	if sys := decodeText(ar.System); sys != "" {
		msgs = append(msgs, agentmodel.Message{Role: "system", Content: sys})
	}
	for _, m := range ar.Messages {
		converted, err := convertMessage(m)
		if err != nil {
			return agentmodel.ChatRequest{}, err
		}
		msgs = append(msgs, converted...)
	}

	req := agentmodel.ChatRequest{
		Model:       model,
		Messages:    msgs,
		Stream:      ar.Stream,
		Temperature: ar.Temperature,
		TopP:        ar.TopP,
		Stop:        ar.StopSeqs,
		Tools:       convertTools(ar.Tools),
		ToolChoice:  convertToolChoice(ar.ToolChoice),
	}
	if ar.MaxTokens > 0 {
		mt := ar.MaxTokens
		req.MaxTokens = &mt
	}
	return req, nil
}

// convertMessage turns one Anthropic message into zero or more OpenAI messages.
// Anthropic content is either a bare string or an array of typed blocks.
func convertMessage(m anthropicMessage) ([]agentmodel.Message, error) {
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return []agentmodel.Message{{Role: m.Role, Content: s}}, nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil, fmt.Errorf("decode %s message content: %w", m.Role, err)
	}
	if m.Role == "assistant" {
		return []agentmodel.Message{convertAssistantBlocks(blocks)}, nil
	}
	return convertUserBlocks(m.Role, blocks), nil
}

// convertAssistantBlocks folds an assistant turn's text into Content and its
// tool_use blocks into ToolCalls. The tool id is carried verbatim — the chatgpt
// provider's split/joinResponsesToolCallID round-trips it back to a call_id.
func convertAssistantBlocks(blocks []anthropicBlock) agentmodel.Message {
	var text strings.Builder
	var toolCalls []agentmodel.ToolCall
	for _, b := range blocks {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			toolCalls = append(toolCalls, agentmodel.ToolCall{
				ID:   b.ID,
				Type: "function",
				Function: agentmodel.ToolCallFunction{
					Name:      b.Name,
					Arguments: inputToArgs(b.Input),
				},
			})
		}
	}
	return agentmodel.Message{Role: "assistant", Content: text.String(), ToolCalls: toolCalls}
}

// convertUserBlocks turns a user turn into OpenAI messages: each tool_result
// block becomes a separate role:"tool" message (keyed by tool_use_id), and any
// free text becomes one user message.
func convertUserBlocks(role string, blocks []anthropicBlock) []agentmodel.Message {
	var out []agentmodel.Message
	var text strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_result":
			out = append(out, agentmodel.Message{
				Role:       "tool",
				ToolCallID: b.ToolUseID,
				Content:    decodeText(b.Content),
			})
		}
	}
	if text.Len() > 0 {
		out = append(out, agentmodel.Message{Role: role, Content: text.String()})
	}
	return out
}

// decodeText reads an Anthropic "string OR array of text blocks" field (system,
// tool_result content) into a flat string.
func decodeText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

func convertTools(tools []anthropicTool) []agentmodel.Tool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]agentmodel.Tool, 0, len(tools))
	for _, t := range tools {
		out = append(out, agentmodel.Tool{
			Type: "function",
			Function: agentmodel.FunctionSchema{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}
	return out
}

// convertToolChoice maps the Anthropic tool_choice object to the OpenAI form.
func convertToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return nil
	}
	switch tc.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		return map[string]any{"type": "function", "function": map[string]any{"name": tc.Name}}
	}
	return nil
}

// jsonObjectOrEmpty returns raw when it is non-empty valid JSON, else the empty
// object literal {}. It keeps tool arguments/inputs well-formed in both
// translation directions (see inputToArgs / argsToInput).
func jsonObjectOrEmpty(raw []byte) []byte {
	if len(raw) == 0 || !json.Valid(raw) {
		return []byte("{}")
	}
	return raw
}

// inputToArgs renders an Anthropic tool_use input object as the JSON-string
// arguments the OpenAI tool-call schema expects.
func inputToArgs(raw json.RawMessage) string {
	return string(jsonObjectOrEmpty(raw))
}

// ─── Anthropic response wire types ───────────────────────────────────────────

type anthropicResponse struct {
	ID           string               `json:"id"`
	Type         string               `json:"type"`
	Role         string               `json:"role"`
	Model        string               `json:"model"`
	Content      []anthropicRespBlock `json:"content"`
	StopReason   string               `json:"stop_reason"`
	StopSequence *string              `json:"stop_sequence"`
	Usage        anthropicUsage       `json:"usage"`
}

type anthropicRespBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type thinkingTokensDetails struct {
	ThinkingTokens *int `json:"thinking_tokens"`
}

type anthropicUsage struct {
	OutputTokensDetails      *thinkingTokensDetails `json:"output_tokens_details,omitempty"`
	InputTokens              int                    `json:"input_tokens"`
	OutputTokens             int                    `json:"output_tokens"`
	CacheReadInputTokens     int                    `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int                    `json:"cache_creation_input_tokens,omitempty"`
}

// chatResponseToAnthropic converts a non-streaming ChatResponse into the
// Anthropic Messages response shape.
func chatResponseToAnthropic(resp agentmodel.ChatResponse, model string) anthropicResponse {
	content := []anthropicRespBlock{}
	finish := ""
	if len(resp.Choices) > 0 {
		ch := resp.Choices[0]
		finish = ch.FinishReason
		if ch.Message.Content != "" {
			content = append(content, anthropicRespBlock{Type: "text", Text: ch.Message.Content})
		}
		for _, tc := range ch.Message.ToolCalls {
			content = append(content, anthropicRespBlock{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: argsToInput(tc.Function.Arguments),
			})
		}
	}
	if len(content) == 0 {
		// Guarantee at least one content block, mirroring the streaming path
		// (stream.go finish): a strict Anthropic client treats a zero-block
		// response as truncated/invalid.
		content = append(content, anthropicRespBlock{Type: "text", Text: ""})
	}
	id := resp.ID
	if id == "" {
		id = "msg_" + uuid.NewString()
	}
	return anthropicResponse{
		ID:         id,
		Type:       "message",
		Role:       "assistant",
		Model:      model,
		Content:    content,
		StopReason: finishToStopReason(finish),
		Usage:      usageToAnthropic(resp.Usage),
	}
}

// usageToAnthropic maps agentmodel.Usage onto the Anthropic usage block.
// agentmodel.PromptTokens INCLUDES the cached portion; Anthropic's input_tokens
// is the non-cached prompt with cache counted separately. The /v1/messages
// handler re-sums input + cache, so we split them back out here to round-trip.
func usageToAnthropic(u agentmodel.Usage) anthropicUsage {
	input := u.PromptTokens - u.CacheReadInputTokens - u.CacheCreationInputTokens
	if input < 0 {
		input = 0
	}
	var details *thinkingTokensDetails
	if u.ReasoningTokens != nil {
		details = &thinkingTokensDetails{ThinkingTokens: u.ReasoningTokens}
	}
	return anthropicUsage{
		OutputTokensDetails:      details,
		InputTokens:              input,
		OutputTokens:             u.CompletionTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens,
	}
}

// finishToStopReason inverts the OpenAI finish_reason → Anthropic stop_reason
// mapping (the chatgpt provider only ever emits stop / tool_calls / length).
func finishToStopReason(finish string) string {
	switch finish {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

// argsToInput renders OpenAI JSON-string tool arguments as a JSON object for an
// Anthropic tool_use block, defaulting to {} when absent or malformed.
func argsToInput(args string) json.RawMessage {
	return json.RawMessage(jsonObjectOrEmpty([]byte(args)))
}
