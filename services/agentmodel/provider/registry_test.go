package provider_test

import (
	"errors"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/stub"
)

func TestResolve_PrefixRoute(t *testing.T) {
	openai := &stub.Stub{NameValue: "openai"}
	anthropic := &stub.Stub{NameValue: "anthropic"}
	r := provider.NewRegistry(map[string]provider.Provider{
		"openai":    openai,
		"anthropic": anthropic,
	})

	cases := []struct {
		input    string
		wantName string
		wantRest string
	}{
		{"openai/gpt-4o", "openai", "gpt-4o"},
		{"anthropic/claude-3-5-sonnet-latest", "anthropic", "claude-3-5-sonnet-latest"},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			p, rest, err := r.Resolve(c.input)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if p.Name() != c.wantName {
				t.Errorf("provider: got %q, want %q", p.Name(), c.wantName)
			}
			if rest != c.wantRest {
				t.Errorf("rest: got %q, want %q", rest, c.wantRest)
			}
		})
	}
}

func TestResolve_DefaultProvider(t *testing.T) {
	openai := &stub.Stub{NameValue: "openai"}
	anthropic := &stub.Stub{NameValue: "anthropic"}
	r := provider.NewRegistryWithOptions(
		map[string]provider.Provider{"openai": openai, "anthropic": anthropic},
		provider.WithDefault("openai"),
	)

	p, rest, err := r.Resolve("gpt-4o")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.Name() != "openai" {
		t.Errorf("default: got %q, want openai", p.Name())
	}
	if rest != "gpt-4o" {
		t.Errorf("rest: got %q, want gpt-4o", rest)
	}
}

func TestResolve_UnknownPrefixUsesDefault(t *testing.T) {
	openai := &stub.Stub{NameValue: "openai"}
	r := provider.NewRegistryWithOptions(
		map[string]provider.Provider{"openai": openai},
		provider.WithDefault("openai"),
	)

	// "unknown/foo" — prefix "unknown" not registered → fall back to default.
	p, rest, err := r.Resolve("unknown/foo")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.Name() != "openai" {
		t.Errorf("provider: got %q, want openai (default)", p.Name())
	}
	// When falling back to default, full model is preserved.
	if rest != "unknown/foo" {
		t.Errorf("rest: got %q, want unknown/foo", rest)
	}
}

func TestResolve_NoDefaultNoPrefix(t *testing.T) {
	openai := &stub.Stub{NameValue: "openai"}
	r := provider.NewRegistry(map[string]provider.Provider{"openai": openai})
	r2 := *r // make a clone with no default for the test
	_ = r2
	// Force-unset default by constructing with the empty fallback removed.
	rNoDefault := provider.NewRegistryWithOptions(
		map[string]provider.Provider{"openai": openai},
		provider.WithDefault(""),
	)

	_, _, err := rNoDefault.Resolve("gpt-4o")
	if !errors.Is(err, provider.ErrProviderNotFound) {
		t.Errorf("expected ErrProviderNotFound, got %v", err)
	}
}

func TestResolve_EmptyModel(t *testing.T) {
	r := provider.NewRegistry(map[string]provider.Provider{"openai": &stub.Stub{}})
	_, _, err := r.Resolve("")
	if !errors.Is(err, provider.ErrProviderNotFound) {
		t.Errorf("expected ErrProviderNotFound, got %v", err)
	}
}

func TestResolve_EmptyModelAfterPrefix(t *testing.T) {
	r := provider.NewRegistry(map[string]provider.Provider{"openai": &stub.Stub{NameValue: "openai"}})
	_, _, err := r.Resolve("openai/")
	if !errors.Is(err, provider.ErrProviderNotFound) {
		t.Errorf("expected ErrProviderNotFound, got %v", err)
	}
}

func TestResolve_AnthropicOAuth_DistinctFromAnthropic(t *testing.T) {
	// Two distinct providers — same upstream, different auth — registered
	// under different prefixes.
	apiKey := &stub.Stub{NameValue: "anthropic"}
	oauth := &stub.Stub{NameValue: "anthropic-oauth"}
	r := provider.NewRegistry(map[string]provider.Provider{
		"anthropic":       apiKey,
		"anthropic-oauth": oauth,
	})

	p1, _, _ := r.Resolve("anthropic/claude-3-5-sonnet-latest")
	p2, _, _ := r.Resolve("anthropic-oauth/claude-3-5-sonnet-latest")
	if p1.Name() != "anthropic" {
		t.Errorf("api-key path: got %q", p1.Name())
	}
	if p2.Name() != "anthropic-oauth" {
		t.Errorf("oauth path: got %q", p2.Name())
	}
}

func TestRegistry_NamesAndGet(t *testing.T) {
	r := provider.NewRegistry(map[string]provider.Provider{
		"openai":    &stub.Stub{NameValue: "openai"},
		"anthropic": &stub.Stub{NameValue: "anthropic"},
	})
	names := r.Names()
	if len(names) != 2 {
		t.Errorf("Names: got %d, want 2", len(names))
	}
	if _, ok := r.Get("openai"); !ok {
		t.Errorf("Get(openai): not found")
	}
	if _, ok := r.Get("nope"); ok {
		t.Errorf("Get(nope): unexpectedly found")
	}
}

// TestNewRegistry_DefaultIsDeterministic guards : the default provider is
// the lexicographically-first name, stable across builds — not a random pick
// from Go's map-iteration order.
func TestNewRegistry_DefaultIsDeterministic(t *testing.T) {
	entries := map[string]provider.Provider{
		"zebra":  &stub.Stub{NameValue: "zebra"},
		"alpha":  &stub.Stub{NameValue: "alpha"},
		"middle": &stub.Stub{NameValue: "middle"},
	}
	for i := 0; i < 20; i++ {
		r := provider.NewRegistry(entries)
		got, _, err := r.Resolve("no-prefix-model") // unprefixed -> default provider
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.Name() != "alpha" {
			t.Fatalf("default provider = %q, want alpha (lexicographically first) on iter %d", got.Name(), i)
		}
	}
}
