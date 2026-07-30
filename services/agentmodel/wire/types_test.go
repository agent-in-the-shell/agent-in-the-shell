package wire_test

import (
	"encoding/json"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/wire"
)

// TestChatRequestRoundTrip verifies that a typical OpenAI-shaped chat request
// survives marshal -> unmarshal without loss.
func TestChatRequestRoundTrip(t *testing.T) {
	temp := 0.7
	maxTok := 1024
	original := wire.ChatRequest{
		Model: "gpt-4o",
		Messages: []wire.Message{
			{Role: "system", Content: "You are helpful."},
			{Role: "user", Content: "Hi."},
		},
		Stream:         true,
		Temperature:    &temp,
		MaxTokens:      &maxTok,
		Stop:           []string{"\n\n", "<END>"},
		User:           "end-user-1",
		PromptCacheKey: "session-1",
	}

	b, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got wire.ChatRequest
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Model != original.Model {
		t.Errorf("Model: got %q, want %q", got.Model, original.Model)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("Messages length: got %d, want 2", len(got.Messages))
	}
	if got.Messages[0].Role != "system" || got.Messages[1].Content != "Hi." {
		t.Errorf("Messages roundtrip mismatch: %+v", got.Messages)
	}
	if got.Stream != true {
		t.Errorf("Stream: got %v, want true", got.Stream)
	}
	if got.Temperature == nil || *got.Temperature != 0.7 {
		t.Errorf("Temperature roundtrip mismatch: %+v", got.Temperature)
	}
	if got.MaxTokens == nil || *got.MaxTokens != 1024 {
		t.Errorf("MaxTokens roundtrip mismatch: %+v", got.MaxTokens)
	}
	if len(got.Stop) != 2 || got.Stop[0] != "\n\n" {
		t.Errorf("Stop roundtrip mismatch: %+v", got.Stop)
	}
	if got.PromptCacheKey != "session-1" {
		t.Errorf("PromptCacheKey: got %q, want session-1", got.PromptCacheKey)
	}
}

// TestChatRequestOptionalFieldsOmitted verifies that nil/zero optional fields
// don't appear in the JSON output (matches OpenAI's API expectations).
func TestChatRequestOptionalFieldsOmitted(t *testing.T) {
	req := wire.ChatRequest{
		Model:    "gpt-4o",
		Messages: []wire.Message{{Role: "user", Content: "Hi"}},
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	for _, key := range []string{"temperature", "max_tokens", "top_p", "stop", "tools", "tool_choice", "stream"} {
		if contains(s, "\""+key+"\"") {
			t.Errorf("expected key %q to be omitted, got: %s", key, s)
		}
	}
}

func TestChatRequestAcceptsContentPartArray(t *testing.T) {
	body := []byte(`{
		"model":"gpt-4o",
		"messages":[{
			"role":"user",
			"content":[
				{"type":"text","text":"hello "},
				{"type":"input_text","text":"world"}
			]
		}]
	}`)

	var got wire.ChatRequest
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal content array: %v", err)
	}
	if got.Messages[0].Content != "hello world" {
		t.Fatalf("text projection = %q, want hello world", got.Messages[0].Content)
	}
	if err := wire.ValidateTextOnlyContent(got); err != nil {
		t.Fatalf("text-only content rejected: %v", err)
	}

	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal marshaled: %v", err)
	}
	msgs := raw["messages"].([]any)
	msg := msgs[0].(map[string]any)
	if _, ok := msg["content"].([]any); !ok {
		t.Fatalf("content was not preserved as array: %s", b)
	}
}

func TestValidateTextOnlyContentRejectsNonTextPartArray(t *testing.T) {
	body := []byte(`{
		"model":"gpt-4o",
		"messages":[{
			"role":"user",
			"content":[
				{"type":"text","text":"describe this"},
				{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}
			]
		}]
	}`)

	var got wire.ChatRequest
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal content array: %v", err)
	}
	if err := wire.ValidateTextOnlyContent(got); err == nil {
		t.Fatal("expected non-text content part to be rejected")
	}
}

// TestToolCallArgumentsAreString verifies that ToolCall.Function.Arguments is
// preserved as a JSON-encoded string (per OpenAI spec), not a parsed object.
func TestToolCallArgumentsAreString(t *testing.T) {
	resp := wire.ChatResponse{
		ID:      "chatcmpl-1",
		Object:  "chat.completion",
		Created: 1700000000,
		Model:   "gpt-4o",
		Choices: []wire.Choice{
			{
				Index: 0,
				Message: wire.Message{
					Role: "assistant",
					ToolCalls: []wire.ToolCall{
						{
							ID:   "call_abc",
							Type: "function",
							Function: wire.ToolCallFunction{
								Name:      "get_weather",
								Arguments: `{"city":"Taipei","unit":"celsius"}`,
							},
						},
					},
				},
				FinishReason: "tool_calls",
			},
		},
		Usage: wire.Usage{PromptTokens: 12, CompletionTokens: 18, TotalTokens: 30},
	}

	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Re-parse as raw JSON; verify "arguments" is a string, not an object.
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	choices, _ := raw["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices: got %d, want 1", len(choices))
	}
	choice0, _ := choices[0].(map[string]any)
	message, _ := choice0["message"].(map[string]any)
	toolCalls, _ := message["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("tool_calls: got %d, want 1", len(toolCalls))
	}
	tc0, _ := toolCalls[0].(map[string]any)
	fn, _ := tc0["function"].(map[string]any)
	args := fn["arguments"]
	if _, ok := args.(string); !ok {
		t.Errorf("arguments: got type %T, want string. Value: %v", args, args)
	}
}

// TestUsageAgenticaExtensions verifies our extension fields (CostUSD, AuthMode)
// are preserved and omitted when zero.
func TestUsageAgenticaExtensions(t *testing.T) {
	usage := wire.Usage{
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
		CostUSD:          0.0125,
		AuthMode:         "api_key",
	}
	b, _ := json.Marshal(usage)
	var got wire.Usage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.CostUSD != 0.0125 {
		t.Errorf("CostUSD: got %v, want 0.0125", got.CostUSD)
	}
	if got.AuthMode != "api_key" {
		t.Errorf("AuthMode: got %q, want api_key", got.AuthMode)
	}

	// Zero extensions should be omitted.
	zero := wire.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}
	b, _ = json.Marshal(zero)
	if contains(string(b), "cost_usd") {
		t.Errorf("expected cost_usd to be omitted: %s", b)
	}
	if contains(string(b), "auth_mode") {
		t.Errorf("expected auth_mode to be omitted: %s", b)
	}
}

func TestUsageEmitsOpenAIPromptTokenDetails(t *testing.T) {
	usage := wire.Usage{
		PromptTokens:             120,
		CompletionTokens:         10,
		TotalTokens:              130,
		CacheReadInputTokens:     100,
		CacheCreationInputTokens: 5,
	}
	b, err := json.Marshal(usage)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		PromptTokensDetails *struct {
			CachedTokens     int `json:"cached_tokens"`
			CacheWriteTokens int `json:"cache_write_tokens"`
		} `json:"prompt_tokens_details"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.PromptTokensDetails == nil {
		t.Fatalf("prompt_tokens_details missing from %s", b)
	}
	if got.PromptTokensDetails.CachedTokens != 100 || got.PromptTokensDetails.CacheWriteTokens != 5 {
		t.Errorf("prompt_tokens_details = %+v, want cached=100 cache_write=5", got.PromptTokensDetails)
	}
}

func TestEmbeddingRequestRoundTrip(t *testing.T) {
	req := wire.EmbeddingRequest{
		Model: "text-embedding-3-small",
		Input: []string{"hello", "world"},
		User:  "u1",
	}
	b, _ := json.Marshal(req)
	var got wire.EmbeddingRequest
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Model != req.Model || len(got.Input) != 2 || got.User != "u1" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

func TestThinkingConfig_RoundTrip(t *testing.T) {
	budget := 8192
	req := wire.ChatRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []wire.Message{{Role: "user", Content: "think"}},
		Thinking: &wire.ThinkingConfig{Type: "enabled", BudgetTokens: budget},
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got wire.ChatRequest
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Thinking == nil {
		t.Fatal("Thinking nil after roundtrip")
	}
	if got.Thinking.Type != "enabled" {
		t.Errorf("Type = %q, want enabled", got.Thinking.Type)
	}
	if got.Thinking.BudgetTokens != budget {
		t.Errorf("BudgetTokens = %d, want %d", got.Thinking.BudgetTokens, budget)
	}
}

func TestReasoningEffort_RoundTrip(t *testing.T) {
	req := wire.ChatRequest{
		Model:           "claude-sonnet-4-6",
		Messages:        []wire.Message{{Role: "user", Content: "think"}},
		ReasoningEffort: "high",
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got wire.ChatRequest
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ReasoningEffort != "high" {
		t.Errorf("ReasoningEffort = %q, want high", got.ReasoningEffort)
	}
}

func TestThinkingBlock_RoundTrip(t *testing.T) {
	msg := wire.Message{
		Role:             "assistant",
		Content:          "Here is my answer.",
		ReasoningContent: "I thought about it.",
		ThinkingBlocks: []wire.ThinkingBlock{
			{Type: "thinking", Thinking: "I thought about it.", Signature: "sig123"},
		},
	}
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got wire.Message
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ReasoningContent != "I thought about it." {
		t.Errorf("ReasoningContent = %q", got.ReasoningContent)
	}
	if len(got.ThinkingBlocks) != 1 {
		t.Fatalf("ThinkingBlocks len = %d, want 1", len(got.ThinkingBlocks))
	}
	if got.ThinkingBlocks[0].Signature != "sig123" {
		t.Errorf("Signature = %q, want sig123", got.ThinkingBlocks[0].Signature)
	}
}

// helper
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
