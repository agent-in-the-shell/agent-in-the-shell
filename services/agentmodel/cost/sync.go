package cost

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
)

// SyncOptions controls how a LiteLLM catalog is merged into our registry.
type SyncOptions struct {
	// AllowProviders limits which litellm_provider values are imported. Empty
	// = import everything. agent-model only routes a handful of providers, so
	// callers normally pass that set to avoid embedding LiteLLM's full
	// multi-thousand-model catalog.
	AllowProviders []string
}

// MergeLiteLLM merges a LiteLLM model_prices_and_context_window.json blob into
// the current catalog as a union with LiteLLM precedence: every existing entry
// is preserved, and LiteLLM entries are overlaid on top (refreshing prices for
// matching keys, adding new models).
//
// Preserving the existing entries is deliberate, not just conservative.
// LiteLLM keys Anthropic models by dated ids (claude-3-5-sonnet-20241022) and
// has no "-latest" aliases, and it does not carry our subscription/OAuth
// entries (chatgpt/*). Cost lookups — and deployment configs — reference those
// exact keys, so dropping them on a resync would silently break pricing. A
// model genuinely retired upstream lingers, which is the safe failure mode.
//
// Only chatgpt/* is reachable: a subscription key resolves when its prefix
// matches usage.Provider (see lookupPriced), and the Anthropic client reports
// "anthropic" for OAuth deployments too, so anthropic-oauth/* never resolves and
// the api_key entry for the same id wins. Two such entries were dropped; don't
// add more. Anthropic subscription requests are $0 via the request's auth mode
// instead (see calculate).
//
// LiteLLM keys models by a bare id ("gpt-4o"); our registry keys by
// "<provider>/<model>" ("openai/gpt-4o") and cost lookups depend on that
// convention, so each imported entry is re-keyed to litellm_provider + "/" +
// name. Returns the merged catalog map.
func MergeLiteLLM(litellm []byte, current map[string]ModelInfo, opts SyncOptions) (map[string]ModelInfo, error) {
	var incoming map[string]json.RawMessage
	if err := json.Unmarshal(litellm, &incoming); err != nil {
		return nil, fmt.Errorf("agentmodel/cost: parse litellm catalog: %w", err)
	}

	allow := make(map[string]bool, len(opts.AllowProviders))
	for _, p := range opts.AllowProviders {
		allow[p] = true
	}

	// Start from everything we already maintain; LiteLLM overlays on top.
	out := make(map[string]ModelInfo, len(current)+len(incoming))
	for key, info := range current {
		out[key] = info
	}

	for name, raw := range incoming {
		if name == "sample_spec" {
			continue // LiteLLM ships a template entry, not a real model
		}
		var info ModelInfo
		if err := json.Unmarshal(raw, &info); err != nil {
			continue // skip non-standard entries rather than fail the whole sync
		}
		if info.Provider == "" {
			continue
		}
		if len(allow) > 0 && !allow[info.Provider] {
			continue
		}
		// LiteLLM entries are per-token, API-key priced.
		if info.AuthMode == "" {
			info.AuthMode = agentmodel.AuthModeAPIKey
		}
		out[catalogKey(info.Provider, name)] = info
	}

	return out, nil
}

// catalogKey normalizes a (provider, model-name) pair to the registry's
// "<provider>/<model>" key, avoiding a doubled prefix when the LiteLLM name
// already carries one (e.g. "vertex_ai/gemini-1.5-pro").
func catalogKey(provider, name string) string {
	if strings.HasPrefix(name, provider+"/") {
		return name
	}
	return provider + "/" + name
}

// MarshalCatalog renders a catalog map as stable, sorted, indented JSON
// suitable for writing back to model_prices.json. encoding/json sorts map
// keys, so the output is deterministic and diff-friendly.
func MarshalCatalog(cat map[string]ModelInfo) ([]byte, error) {
	b, err := json.MarshalIndent(cat, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("agentmodel/cost: marshal catalog: %w", err)
	}
	return append(b, '\n'), nil
}
