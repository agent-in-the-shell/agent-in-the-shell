// Package cost provides per-request USD cost calculation backed by a JSON
// model price registry. Subscription models intentionally omit cost fields
// and resolve to $0 — mirroring LiteLLM's approach.
package cost

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
)

//go:embed model_prices.json
var defaultPricesJSON []byte

// ModelInfo describes one model's pricing and capability metadata.
//
// Cache pricing fields:
//
//   - CacheReadCostPerToken: rate for cached prompt tokens (typically 10% of
//     input rate for Anthropic, ~50% for OpenAI). Omitted = same as input rate
//     (no discount); set to 0 explicitly to mark "free cache hits".
//
//   - CacheCreationCostPerToken: rate for tokens written to cache on first
//     use (Anthropic charges ~1.25× input). Omitted = same as input rate.
type ModelInfo struct {
	Provider                  string  `json:"litellm_provider"`
	InputCostPerToken         float64 `json:"input_cost_per_token,omitempty"`
	OutputCostPerToken        float64 `json:"output_cost_per_token,omitempty"`
	CacheReadCostPerToken     float64 `json:"cache_read_input_token_cost,omitempty"`
	CacheCreationCostPerToken float64 `json:"cache_creation_input_token_cost,omitempty"`

	// Per-output pricing for non-token modalities (image/video/audio generated
	// via passthrough providers such as Replicate). Cost = unit_count × rate,
	// where unit_count is derived from the request at submit time. At most one
	// applies per model: images for image gen, output video-seconds for video
	// gen, input characters for TTS. Omitted (0) = not per-output priced.
	OutputCostPerImage        float64 `json:"output_cost_per_image,omitempty"`
	OutputCostPerVideoSecond  float64 `json:"output_cost_per_video_second,omitempty"`
	InputCostPerCharacter     float64 `json:"input_cost_per_character,omitempty"`
	SupportsCaching           bool    `json:"supports_prompt_caching,omitempty"`
	MaxInputTokens            int     `json:"max_input_tokens,omitempty"`
	MaxOutputTokens           int     `json:"max_output_tokens,omitempty"`
	SupportsFunctionCalling   bool    `json:"supports_function_calling,omitempty"`
	SupportsVision            bool    `json:"supports_vision,omitempty"`
	SupportsParallelFunctions bool    `json:"supports_parallel_function_calling,omitempty"`
	Mode                      string  `json:"mode,omitempty"` // "chat", "embedding", "responses", "image_generation", "video_generation"
	AuthMode                  string  `json:"auth_mode"`      // "api_key" or "subscription"
	SubscriptionPlan          string  `json:"subscription_plan,omitempty"`
}

// IsSubscription reports whether the model is billed via a subscription tier
// (e.g. Claude Pro/Max, ChatGPT Plus/Pro) rather than per-token API key.
func (m *ModelInfo) IsSubscription() bool {
	return m.AuthMode == agentmodel.AuthModeSubscription
}

// Registry maps a model identifier (e.g. "openai/gpt-4o") to its ModelInfo.
type Registry struct {
	models map[string]ModelInfo
}

// ErrModelNotFound is returned when no entry matches the model identifier.
var ErrModelNotFound = errors.New("agentmodel/cost: model not found in registry")

// LoadDefault loads the registry from the embedded model_prices.json shipped
// with the binary. Suitable for production use; tests may prefer LoadJSON.
func LoadDefault() (*Registry, error) {
	return LoadJSON(defaultPricesJSON)
}

// LoadJSON parses the supplied JSON bytes into a Registry.
func LoadJSON(data []byte) (*Registry, error) {
	var raw map[string]ModelInfo
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("agentmodel/cost: parse registry: %w", err)
	}
	return &Registry{models: raw}, nil
}

// Lookup returns the ModelInfo for model, or ErrModelNotFound.
func (r *Registry) Lookup(model string) (ModelInfo, error) {
	info, ok := r.models[model]
	if !ok {
		return ModelInfo{}, fmt.Errorf("%w: %q", ErrModelNotFound, model)
	}
	return info, nil
}

// Models returns all registered model identifiers.
func (r *Registry) Models() []string {
	out := make([]string, 0, len(r.models))
	for k := range r.models {
		out = append(out, k)
	}
	return out
}

// Len returns the number of models in the registry.
func (r *Registry) Len() int { return len(r.models) }
