package cost_test

import (
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/cost"
)

func TestMergeLiteLLM(t *testing.T) {
	// A trimmed LiteLLM-shaped catalog: a template entry to skip, two allowed
	// providers, one disallowed provider, and a name that already carries a
	// provider prefix.
	litellm := []byte(`{
		"sample_spec": {"litellm_provider": "openai", "input_cost_per_token": 0.0},
		"gpt-5.5": {"litellm_provider": "openai", "input_cost_per_token": 0.000002, "output_cost_per_token": 0.000008, "mode": "chat", "max_input_tokens": 400000},
		"claude-opus-4-8": {"litellm_provider": "anthropic", "input_cost_per_token": 0.000005, "output_cost_per_token": 0.000025, "mode": "chat"},
		"llama-3-70b": {"litellm_provider": "replicate", "input_cost_per_token": 0.000001},
		"vertex_ai/gemini-2.5-pro": {"litellm_provider": "vertex_ai", "input_cost_per_token": 0.000001}
	}`)

	// What we maintain by hand today: an API-key alias LiteLLM does NOT carry
	// ("-latest"), an API-key entry LiteLLM DOES carry (price should refresh),
	// and a subscription entry LiteLLM does not know about.
	current := map[string]cost.ModelInfo{
		"anthropic/claude-3-5-sonnet-latest": {Provider: "anthropic", AuthMode: agentmodel.AuthModeAPIKey, InputCostPerToken: 3},
		"openai/gpt-5.5":                     {Provider: "openai", AuthMode: agentmodel.AuthModeAPIKey, InputCostPerToken: 999},
		"chatgpt/gpt-5":                      {Provider: "chatgpt", AuthMode: agentmodel.AuthModeSubscription, SubscriptionPlan: "plus"},
	}

	merged, err := cost.MergeLiteLLM(litellm, current, cost.SyncOptions{
		AllowProviders: []string{"openai", "anthropic", "vertex_ai"},
	})
	if err != nil {
		t.Fatalf("MergeLiteLLM: %v", err)
	}

	// LiteLLM wins on a conflicting key: the hand-maintained openai/gpt-5.5
	// (input rate 999) is refreshed to LiteLLM's pricing.
	if got := merged["openai/gpt-5.5"]; got.InputCostPerToken != 0.000002 || got.OutputCostPerToken != 0.000008 {
		t.Errorf("openai/gpt-5.5: got input=%v output=%v, want LiteLLM 2e-6/8e-6 (LiteLLM should win)", got.InputCostPerToken, got.OutputCostPerToken)
	}
	// A brand-new LiteLLM model arrives, re-keyed to <provider>/<model>.
	if _, ok := merged["anthropic/claude-opus-4-8"]; !ok {
		t.Errorf("expected anthropic/claude-opus-4-8 to be imported; keys=%v", keys(merged))
	}

	// The template entry is skipped.
	if _, ok := merged["openai/sample_spec"]; ok {
		t.Errorf("sample_spec must not be imported")
	}

	// Disallowed provider is filtered out.
	if _, ok := merged["replicate/llama-3-70b"]; ok {
		t.Errorf("replicate provider should be filtered out; keys=%v", keys(merged))
	}

	// A name already carrying its provider prefix is not double-prefixed.
	if _, ok := merged["vertex_ai/gemini-2.5-pro"]; !ok {
		t.Errorf("expected vertex_ai/gemini-2.5-pro (no double prefix); keys=%v", keys(merged))
	}
	if _, ok := merged["vertex_ai/vertex_ai/gemini-2.5-pro"]; ok {
		t.Errorf("double-prefixed key should not exist")
	}

	// Hand-maintained subscription entry survives the resync.
	sub, ok := merged["chatgpt/gpt-5"]
	if !ok {
		t.Fatalf("subscription entry chatgpt/gpt-5 must be preserved; keys=%v", keys(merged))
	}
	if !sub.IsSubscription() || sub.SubscriptionPlan != "plus" {
		t.Errorf("preserved subscription entry corrupted: %+v", sub)
	}

	// A hand-maintained "-latest" alias LiteLLM does not carry is PRESERVED
	// (LiteLLM keys Anthropic by dated ids only). Dropping it would silently
	// break cost lookups for deployments configured with the alias.
	if got, ok := merged["anthropic/claude-3-5-sonnet-latest"]; !ok || got.InputCostPerToken != 3 {
		t.Errorf("anthropic/claude-3-5-sonnet-latest must be preserved with its rate; got ok=%v info=%+v", ok, got)
	}
}

func TestMarshalCatalogIsStable(t *testing.T) {
	cat := map[string]cost.ModelInfo{
		"b/y": {Provider: "b", AuthMode: agentmodel.AuthModeAPIKey},
		"a/x": {Provider: "a", AuthMode: agentmodel.AuthModeAPIKey},
	}
	first, err := cost.MarshalCatalog(cat)
	if err != nil {
		t.Fatalf("MarshalCatalog: %v", err)
	}
	second, _ := cost.MarshalCatalog(cat)
	if string(first) != string(second) {
		t.Errorf("MarshalCatalog not deterministic")
	}
	// Round-trips back into a usable registry.
	if _, err := cost.LoadJSON(first); err != nil {
		t.Errorf("marshaled catalog does not load: %v", err)
	}
}

func keys(m map[string]cost.ModelInfo) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
