package factory

import (
	"io"
	"log/slog"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
)

func TestDiscoverySourceNameDistinguishesAPIKeyCredentials(t *testing.T) {
	first := discoverySourceName(agentmodel.DeploymentConfig{
		Provider: "azure", AuthMode: agentmodel.AuthModeAPIKey,
		BaseURL: "https://example.test", APIKeyEnv: "AZURE_KEY_A",
	})
	second := discoverySourceName(agentmodel.DeploymentConfig{
		Provider: "azure", AuthMode: agentmodel.AuthModeAPIKey,
		BaseURL: "https://example.test", APIKeyEnv: "AZURE_KEY_B",
	})
	if first == second {
		t.Fatalf("source names are ambiguous: %q", first)
	}
}

func TestDiscoverSourcesDeduplicatesConnectionAcrossRoutingPolicy(t *testing.T) {
	one, hundred := 1, 100
	cfg := &agentmodel.Config{ModelList: []agentmodel.ModelEntry{
		{ModelName: "first", Deployments: []agentmodel.DeploymentConfig{{
			Provider: "openai-compatible", Model: "upstream-a", BaseURL: "http://example.test", Weight: &hundred,
		}}},
		{ModelName: "second", Deployments: []agentmodel.DeploymentConfig{{
			Provider: "openai-compatible", Model: "upstream-b", BaseURL: "http://example.test", Weight: &one,
		}}},
	}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sources, unsupported, err := DiscoverSources(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	if len(unsupported) != 0 {
		t.Fatalf("unsupported = %v", unsupported)
	}
	if len(sources) != 1 {
		t.Fatalf("sources = %d, want 1", len(sources))
	}
	if got := sources[0].Template.EffectiveWeight(); got != 100 {
		t.Fatalf("template weight = %d, want first source policy 100", got)
	}
	if sources[0].Template.Model != "" {
		t.Fatalf("template model = %q, want empty", sources[0].Template.Model)
	}
}
