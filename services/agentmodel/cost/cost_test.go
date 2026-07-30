package cost_test

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/cost"
)

// EstimateMessages helper test was dropped along with the unused helper.

func TestRegistry_LoadDefault(t *testing.T) {
	r, err := cost.LoadDefault()
	if err != nil {
		t.Fatalf("LoadDefault: %v", err)
	}
	if r.Len() == 0 {
		t.Error("expected non-empty default registry")
	}
	// spot-check a few expected entries
	for _, m := range []string{
		"openai/gpt-4o",
		"anthropic/claude-3-5-sonnet-latest",
		"gemini/gemini-2.5-pro",
		"chatgpt/gpt-5",
	} {
		if _, err := r.Lookup(m); err != nil {
			t.Errorf("Lookup(%q): %v", m, err)
		}
	}
}

func TestCalculate_APIKeyModel(t *testing.T) {
	r, _ := cost.LoadDefault()
	usage := agentmodel.Usage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500,
	}
	got, err := cost.Calculate("openai/gpt-4o", usage, r)
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	// gpt-4o: 1000 * 0.0000025 + 500 * 0.000010 = 0.0025 + 0.005 = 0.0075
	want := 0.0075
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("Calculate: got %v, want %v", got, want)
	}
}

func TestCalculate_SubscriptionModel_Zero(t *testing.T) {
	r, _ := cost.LoadDefault()
	for _, m := range []string{
		"chatgpt/gpt-5",
		"chatgpt/gpt-5-pro",
	} {
		t.Run(m, func(t *testing.T) {
			usage := agentmodel.Usage{
				PromptTokens:     10000,
				CompletionTokens: 5000,
				TotalTokens:      15000,
			}
			got, err := cost.Calculate(m, usage, r)
			if err != nil {
				t.Fatalf("Calculate: %v", err)
			}
			if got != 0 {
				t.Errorf("subscription model %q should cost 0, got %v", m, got)
			}
		})
	}
}

func TestCalculate_AnthropicCacheBreakdown(t *testing.T) {
	r, _ := cost.LoadDefault()
	// Sonnet rates: input $3/M, output $15/M, cache_read $0.30/M, cache_creation $3.75/M.
	// Scenario: 10k uncached + 2000 cache_creation + 500 cache_read + 200 completion.
	// PromptTokens follows OpenAI convention = 10000 + 2000 + 500 = 12500.
	usage := agentmodel.Usage{
		PromptTokens:             12500,
		CompletionTokens:         200,
		TotalTokens:              12700,
		CacheReadInputTokens:     500,
		CacheCreationInputTokens: 2000,
	}
	got, err := cost.Calculate("anthropic/claude-3-5-sonnet-latest", usage, r)
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	// uncached = 10000 -> 10000 * 0.000003 = 0.03
	// cache_read = 500 -> 500 * 0.0000003 = 0.00015
	// cache_creation = 2000 -> 2000 * 0.00000375 = 0.0075
	// completion = 200 -> 200 * 0.000015 = 0.003
	// total = 0.04065
	want := 0.04065
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("Calculate: got %.10f, want %.10f", got, want)
	}
}

func TestCalculate_OpenAICacheReadHalfPrice(t *testing.T) {
	r, _ := cost.LoadDefault()
	// gpt-4o: input $2.5/M, output $10/M, cache_read $1.25/M (50%).
	// 1000 uncached + 9000 cache_read + 500 completion (PromptTokens = 10000).
	usage := agentmodel.Usage{
		PromptTokens:         10000,
		CompletionTokens:     500,
		TotalTokens:          10500,
		CacheReadInputTokens: 9000,
	}
	got, err := cost.Calculate("openai/gpt-4o", usage, r)
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	// uncached = 1000 * 0.0000025 = 0.0025
	// cache_read = 9000 * 0.00000125 = 0.01125
	// output = 500 * 0.000010 = 0.005
	// total = 0.01875
	want := 0.01875
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("Calculate: got %.10f, want %.10f", got, want)
	}
}

func TestCalculate_NoCacheFieldsBackwardsCompatible(t *testing.T) {
	r, _ := cost.LoadDefault()
	// No cache_* fields → behaves like before: PromptTokens × input + Completion × output.
	usage := agentmodel.Usage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500,
	}
	got, err := cost.Calculate("openai/gpt-4o", usage, r)
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	want := 1000*0.0000025 + 500*0.000010
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("Calculate: got %.10f, want %.10f", got, want)
	}
}

func TestCalculate_UnknownModel(t *testing.T) {
	r, _ := cost.LoadDefault()
	_, err := cost.Calculate("nonexistent/model", agentmodel.Usage{}, r)
	if !errors.Is(err, cost.ErrModelNotFound) {
		t.Errorf("error: got %v, want ErrModelNotFound", err)
	}
}

// TestSubscriptionEntriesAreZeroCost guards the hand-maintained subscription
// namespace that `syncprices` preserves across a LiteLLM resync. An entry under
// this prefix is billed by a flat out-of-band fee, so it must be flagged
// auth_mode=subscription and must carry no per-token prices — a resync that
// overwrote one with the vendor's API prices would silently start billing spend
// against a plan the user already paid for. Only chatgpt/ is reachable — see
// MergeLiteLLM in sync.go.
func TestSubscriptionEntriesAreZeroCost(t *testing.T) {
	r, err := cost.LoadDefault()
	if err != nil {
		t.Fatalf("LoadDefault: %v", err)
	}
	prefixes := []string{"chatgpt/"}
	seen := 0
	for _, model := range r.Models() {
		matched := false
		for _, p := range prefixes {
			if strings.HasPrefix(model, p) {
				matched = true
			}
		}
		if !matched {
			continue
		}
		seen++
		info, err := r.Lookup(model)
		if err != nil {
			t.Fatalf("Lookup(%q): %v", model, err)
		}
		if !info.IsSubscription() {
			t.Errorf("%s: auth_mode = %q, want subscription", model, info.AuthMode)
		}
		if info.InputCostPerToken != 0 || info.OutputCostPerToken != 0 ||
			info.CacheReadCostPerToken != 0 || info.CacheCreationCostPerToken != 0 {
			t.Errorf("%s: carries per-token prices, want none (subscription is flat-fee)", model)
		}
		if _, src := cost.CalculateWithSource(model, agentmodel.Usage{PromptTokens: 1e6, CompletionTokens: 1e6}, r); src != cost.SourceSubscription {
			t.Errorf("%s: source = %q, want %q", model, src, cost.SourceSubscription)
		}
	}
	if seen == 0 {
		t.Fatal("no subscription entries found — the prefixes or the catalog changed")
	}
}

// TestServedSubscriptionModelsArePriced pins the models the live gateway
// actually routes to a subscription backend. A model absent here logs as
// "unpriced", which an operator reads as "spend is under-counted" rather than
// "$0 by design".
//
// Catalog hygiene, not the primary guard: calculate() short-circuits on
// usage.AuthMode, so these entries only matter to a caller that prices usage
// without an auth mode set (today: none in the gateway). The primary guards are
// the "subscription auth_mode outranks priced entry" case above and
// TestLive_SubscriptionAuth end to end.
func TestServedSubscriptionModelsArePriced(t *testing.T) {
	r, err := cost.LoadDefault()
	if err != nil {
		t.Fatalf("LoadDefault: %v", err)
	}
	// provider name as configured -> upstream model id on the deployment.
	served := map[string]string{
		"chatgpt": "gpt-5.5",
	}
	for provider, model := range served {
		_, src := cost.CalculateWithSource(model, agentmodel.Usage{Provider: provider, PromptTokens: 100}, r)
		if src != cost.SourceSubscription {
			t.Errorf("%s/%s: source = %q, want %q — add a %s/%s entry to model_prices.json",
				provider, model, src, cost.SourceSubscription, provider, model)
		}
	}
}

func TestCalculateWithSource(t *testing.T) {
	r, _ := cost.LoadDefault()
	usage := agentmodel.Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}

	t.Run("priced", func(t *testing.T) {
		got, src := cost.CalculateWithSource("openai/gpt-4o", usage, r)
		if src != cost.SourcePriced {
			t.Errorf("source: got %q, want %q", src, cost.SourcePriced)
		}
		// matches TestCalculate_APIKeyModel: 1000*0.0000025 + 500*0.00001
		if want := 0.0075; math.Abs(got-want) > 1e-9 {
			t.Errorf("cost: got %v, want %v", got, want)
		}
	})

	t.Run("subscription", func(t *testing.T) {
		got, src := cost.CalculateWithSource("chatgpt/gpt-5", usage, r)
		if src != cost.SourceSubscription {
			t.Errorf("source: got %q, want %q", src, cost.SourceSubscription)
		}
		if got != 0 {
			t.Errorf("cost: got %v, want 0 for subscription", got)
		}
	})

	// The live gateway serves gpt-5.5 over the ChatGPT subscription. Without a
	// chatgpt/gpt-5.5 catalog entry the bare-id fallback misses and the request
	// is logged "unpriced" — which means "cost UNKNOWN, spend reports
	// under-count this" — when the truth is "$0 by design, flat fee already
	// paid". Every request through the live gateway hit this.
	t.Run("subscription gpt-5.5 via provider prefix", func(t *testing.T) {
		u := usage
		u.Provider = "chatgpt"
		got, src := cost.CalculateWithSource("gpt-5.5", u, r)
		if src != cost.SourceSubscription {
			t.Errorf("source: got %q, want %q", src, cost.SourceSubscription)
		}
		if got != 0 {
			t.Errorf("cost: got %v, want 0 for subscription", got)
		}
	})

	// Request auth mode outranks catalog classification — see calculate(). The
	// two cases below differ ONLY in AuthMode, which is the discriminator
	// production actually has: the anthropic client reports Name() "anthropic"
	// for api-key and OAuth deployments alike, so both resolve to the same
	// priced catalog entry. A live subscription request metered at "priced".
	t.Run("subscription auth_mode outranks priced entry", func(t *testing.T) {
		u := usage
		u.Provider = "anthropic"
		if _, src := cost.CalculateWithSource("claude-sonnet-4-6", u, r); src != cost.SourcePriced {
			t.Fatalf("api_key source = %q, want %q (catalog entry missing?)", src, cost.SourcePriced)
		}

		u.AuthMode = agentmodel.AuthModeSubscription
		got, src := cost.CalculateWithSource("claude-sonnet-4-6", u, r)
		if src != cost.SourceSubscription {
			t.Errorf("source: got %q, want %q", src, cost.SourceSubscription)
		}
		if got != 0 {
			t.Errorf("cost: got %v, want 0 — subscription traffic is flat-fee", got)
		}
		if c, err := cost.Calculate("claude-sonnet-4-6", u, r); err != nil || c != 0 {
			t.Errorf("Calculate: got (%v, %v), want (0, nil)", c, err)
		}
	})

	t.Run("unpriced", func(t *testing.T) {
		// An unknown model resolves to (0, unpriced) — NOT an error — so callers
		// can record the miss instead of silently logging $0 as a real price.
		got, src := cost.CalculateWithSource("nonexistent/model", usage, r)
		if src != cost.SourceUnpriced {
			t.Errorf("source: got %q, want %q", src, cost.SourceUnpriced)
		}
		if got != 0 {
			t.Errorf("cost: got %v, want 0 for unpriced", got)
		}
	})
}

func TestModelInfo_IsSubscription(t *testing.T) {
	r, _ := cost.LoadDefault()

	apiKeyModel, _ := r.Lookup("openai/gpt-4o")
	if apiKeyModel.IsSubscription() {
		t.Errorf("openai/gpt-4o should not be subscription")
	}

	subModel, _ := r.Lookup("chatgpt/gpt-5")
	if !subModel.IsSubscription() {
		t.Errorf("chatgpt/gpt-5 should be subscription")
	}
}

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		in  string
		min int
		max int
	}{
		{"", 0, 0},
		{"hi", 0, 1},
		{"hello world", 2, 4},
		{strings.Repeat("a", 400), 95, 105},
	}
	for _, c := range cases {
		got := cost.EstimateTokens(c.in)
		if got < c.min || got > c.max {
			t.Errorf("EstimateTokens(%q): got %d, want in [%d,%d]", c.in, got, c.min, c.max)
		}
	}
}

// TestCalculateWithSource_ProviderPrefixAndAlias verifies newly-synced providers
// price via the <provider>/<model> key, including the friendly-name aliases.
func TestCalculateWithSource_ProviderPrefixAndAlias(t *testing.T) {
	reg, err := cost.LoadDefault()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ provider, model string }{
		{"groq", "llama-3.1-8b-instant"},     // direct name match
		{"moonshot", "kimi-k2-0711-preview"}, // Kimi — direct match
		{"qwen", "qwen-flash"},               // alias -> dashscope/qwen-flash
		{"xai", "grok-2"},                    // direct match
	}
	for _, c := range cases {
		_, src := cost.CalculateWithSource(c.model, agentmodel.Usage{Provider: c.provider, PromptTokens: 100, CompletionTokens: 50}, reg)
		if src != cost.SourcePriced {
			t.Errorf("%s/%s: source=%q, want priced", c.provider, c.model, src)
		}
	}
	// No provider → bare lookup misses these prefixed keys → unpriced (unchanged).
	if _, src := cost.CalculateWithSource("qwen-flash", agentmodel.Usage{PromptTokens: 100}, reg); src != cost.SourceUnpriced {
		t.Errorf("bare qwen-flash without provider: src=%q, want unpriced", src)
	}
}
