package api

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/cost"
)

// Replicate per-output cost metering (Phase 2a, ).
//
// Replicate exposes no cost via its API (verified: no billing endpoint, no cost
// field on the prediction). For its Official Models, billing is per output unit
// — and crucially that unit is knowable from the REQUEST body at submit time,
// so cost = units × rate can be computed in createPrediction with no completion
// poll. The rates live in the price registry (model_prices.json); this file is
// the Replicate-specific glue that maps a request body to a unit count.
//
// Out of scope (these stay cost_source=unpriced — a deliberate "price missing"
// marker, not a real $0): per-GPU-second community models (cost needs
// metrics.predict_time at completion), bria/video-remove-background (billed on
// output video length, which is not in the request), and bare /v1/predictions
// creates carrying only a version hash (no owner/name to key a rate on). Those
// are a settle-time follow-up.

const klingModel = "kwaivgi/kling-v3-omni-video"

// replicatePredictionCost returns the submit-time USD cost and its cost_source
// for a Replicate prediction, derived from the request body. It returns
// (0, SourceUnpriced) when the model is not per-output priced or its rate is
// absent from the registry — never a fabricated $0.
func (s *Server) replicatePredictionCost(model string, body []byte) (float64, cost.Source) {
	if s.registry == nil {
		return 0, cost.SourceUnpriced
	}
	input := replicateInputObject(body)
	switch {
	case model == klingModel:
		// Per output-video-second, tiered by (mode, generate_audio). The
		// generate_audio default is false (confirmed against the model schema);
		// duration defaults to 5.
		mode := stringField(input, "mode", "standard")
		audio := boolField(input, "generate_audio", false)
		seconds := numberField(input, "duration", 5)
		info, err := s.registry.Lookup(klingTierKey(mode, audio))
		if err != nil || info.OutputCostPerVideoSecond == 0 {
			return 0, cost.SourceUnpriced
		}
		return info.OutputCostPerVideoSecond * seconds, cost.SourcePriced

	case model == "qwen/qwen-image" || model == "qwen/qwen-image-edit-plus":
		// Exactly one output image per create — neither schema has a batch/count
		// param (qwen-image-edit-plus's input.image is reference inputs, not
		// outputs, so it must NOT be counted).
		info, err := s.registry.Lookup(model)
		if err != nil || info.OutputCostPerImage == 0 {
			return 0, cost.SourceUnpriced
		}
		return info.OutputCostPerImage, cost.SourcePriced

	case strings.HasPrefix(model, "minimax/speech-"):
		// TTS bills per input character (1 char ≈ 1 token); output is $0. Counted
		// over runes so multibyte text (e.g. CJK) bills one unit per character.
		info, err := s.registry.Lookup(model)
		if err != nil || info.InputCostPerCharacter == 0 {
			return 0, cost.SourceUnpriced
		}
		chars := utf8.RuneCountInString(stringField(input, "text", ""))
		return info.InputCostPerCharacter * float64(chars), cost.SourcePriced
	}
	return 0, cost.SourceUnpriced
}

// klingTierKey builds the synthetic registry key for one Kling pricing tier,
// mirroring the existing OpenAI image-gen key convention
// (e.g. "openai/high/1024-x-1024/gpt-image-1").
func klingTierKey(mode string, audio bool) string {
	a := "noaudio"
	if audio {
		a = "audio"
	}
	return klingModel + "/" + mode + "/" + a
}

// replicateInputObject extracts the "input" object from a Replicate prediction
// request body. A nil result (parse miss / no input) is treated by callers as
// all-defaults.
func replicateInputObject(body []byte) map[string]any {
	var peek struct {
		Input map[string]any `json:"input"`
	}
	if err := json.Unmarshal(body, &peek); err != nil {
		return nil
	}
	return peek.Input
}

func stringField(m map[string]any, key, def string) string {
	if v, ok := m[key].(string); ok && v != "" {
		return v
	}
	return def
}

func boolField(m map[string]any, key string, def bool) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return def
}

// numberField reads a JSON number (generic unmarshal yields float64).
func numberField(m map[string]any, key string, def float64) float64 {
	if v, ok := m[key].(float64); ok {
		return v
	}
	return def
}
