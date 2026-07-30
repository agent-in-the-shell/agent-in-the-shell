package cost

import (
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
)

// Calculate returns the per-request USD cost for the given model and usage.
//
// For subscription models (auth_mode = "subscription"), the cost is always 0
// — the user has paid a flat subscription fee and individual token usage
// isn't billed.
//
// For API-key models, cost is split between three rate categories so prompt
// caching is correctly priced:
//
//   - cache_creation tokens × cache_creation rate (~1.25× input on Anthropic)
//   - cache_read tokens     × cache_read rate     (~0.10× input on Anthropic,
//     ~0.50× on OpenAI)
//   - remaining input       × input rate
//   - completion tokens     × output rate
//
// Note: usage.PromptTokens follows OpenAI's convention and INCLUDES the cache
// portion. The "remaining input" is computed as PromptTokens minus cached
// minus cache_creation so we don't double-count.
//
// When a model entry omits cache rates (caching unsupported, or simpler
// pricing), the cache portion falls through to the standard input rate,
// matching the legacy behavior.
// lookupPriced resolves a price entry, preferring the catalog's canonical
// provider-prefixed key ("<provider>/<model>", e.g. "groq/llama-3.3-70b") when
// the router recorded the serving provider on usage, and falling back to the
// bare model id. This is what lets provider-specific models (Groq/Mistral/xAI/…
// and DeepSeek) be metered rather than logged unpriced. Empty usage.Provider
// preserves the original bare-id behavior exactly.
// providerPriceAlias maps a config provider name to the litellm_provider used as
// the catalog's key prefix, where the two differ. Keeps friendly config names
// (qwen/together/fireworks) while pricing against LiteLLM's canonical keys.
var providerPriceAlias = map[string]string{
	"qwen":      "dashscope",
	"together":  "together_ai",
	"fireworks": "fireworks_ai",
}

func lookupPriced(model string, usage agentmodel.Usage, registry *Registry) (ModelInfo, error) {
	if usage.Provider != "" {
		prov := usage.Provider
		if a, ok := providerPriceAlias[prov]; ok {
			prov = a
		}
		if info, err := registry.Lookup(prov + "/" + model); err == nil {
			return info, nil
		}
	}
	return registry.Lookup(model)
}

func Calculate(model string, usage agentmodel.Usage, registry *Registry) (float64, error) {
	c, _, err := calculate(model, usage, registry)
	return c, err
}

// calculate is the single classification path behind both exported entry points,
// so the precedence rule below is stated once. The two wrappers differ only in
// how they report an unknown model: an error, or SourceUnpriced.
//
// Precedence: how the request was actually served outranks how the catalog
// classifies the model. A model id can legitimately be both — claude-sonnet-4-6
// is a priced api_key entry synced from LiteLLM, and the same id is served over
// a Claude subscription via OAuth. Pricing that OAuth traffic off the api_key
// entry invents dollar spend for flat-fee usage, which is what a live
// subscription request revealed.
func calculate(model string, usage agentmodel.Usage, registry *Registry) (float64, Source, error) {
	if usage.AuthMode == agentmodel.AuthModeSubscription {
		return 0, SourceSubscription, nil
	}
	info, err := lookupPriced(model, usage, registry)
	if err != nil {
		return 0, SourceUnpriced, err
	}
	if info.IsSubscription() {
		return 0, SourceSubscription, nil
	}
	return priceFromInfo(info, usage), SourcePriced, nil
}

// Source classifies WHY a request's cost has the value it does, so audit logs
// can tell a genuine $0 apart from a price we simply don't have. A bare $0 in a
// spend report is ambiguous; the Source disambiguates it.
type Source string

const (
	// SourcePriced means the cost was computed from a registry entry.
	SourcePriced Source = "priced"
	// SourceSubscription means the model is subscription-billed, so its cost is
	// $0 by design (a flat fee was already paid out-of-band).
	SourceSubscription Source = "subscription"
	// SourceUnpriced means the model was absent from the price registry; cost
	// falls back to $0 but is actually UNKNOWN — spend reports under-count it.
	SourceUnpriced Source = "unpriced"
	// SourceCache means the response was served from the gateway's response
	// cache: token counts stay visible but cost is $0 because no upstream tokens
	// were consumed. Set by the api layer on a cache hit; never produced by
	// CalculateWithSource.
	SourceCache Source = "cache"
)

// CalculateWithSource is like Calculate but, instead of returning an error for
// an unknown model, it returns (0, SourceUnpriced). This lets callers record
// the miss in the audit log (and alert on it) rather than silently logging $0
// as if it were a real price. See issue #511.
func CalculateWithSource(model string, usage agentmodel.Usage, registry *Registry) (float64, Source) {
	c, src, _ := calculate(model, usage, registry)
	return c, src
}

// priceFromInfo is the shared per-token pricing math used by both Calculate and
// CalculateWithSource. The caller is responsible for the subscription / unknown
// short-circuits; this function assumes a real, per-token-priced model.
func priceFromInfo(info ModelInfo, usage agentmodel.Usage) float64 {
	cacheReadRate := info.CacheReadCostPerToken
	if cacheReadRate == 0 {
		cacheReadRate = info.InputCostPerToken
	}
	cacheCreateRate := info.CacheCreationCostPerToken
	if cacheCreateRate == 0 {
		cacheCreateRate = info.InputCostPerToken
	}

	uncached := usage.PromptTokens - usage.CacheReadInputTokens - usage.CacheCreationInputTokens
	if uncached < 0 {
		uncached = 0 // defensive: provider mis-reported
	}

	cost := float64(uncached) * info.InputCostPerToken
	cost += float64(usage.CacheReadInputTokens) * cacheReadRate
	cost += float64(usage.CacheCreationInputTokens) * cacheCreateRate
	cost += float64(usage.CompletionTokens) * info.OutputCostPerToken
	return cost
}
