package main

import (
	"strings"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"gopkg.in/yaml.v3"
)

func TestParseOrderedSelectionPreservesOrder(t *testing.T) {
	got, err := parseOrderedSelection("3,1,2", 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []int{2, 0, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("selection = %v, want %v", got, want)
		}
	}
	if _, err := parseOrderedSelection("1,1", 3); err == nil {
		t.Fatal("expected duplicate error")
	}
}

func TestReplaceFallbackRuleSetsAndClearsOneModel(t *testing.T) {
	rules := []agentmodel.FallbackRule{
		{ModelName: "a", FallbackTo: []string{"b"}},
		{ModelName: "c", FallbackTo: []string{"a"}},
	}
	got := replaceFallbackRule(rules, "a", []string{"c", "b"})
	if strings.Join(fallbackTargets(got, "a"), ",") != "c,b" {
		t.Fatalf("updated rules = %#v", got)
	}
	got = replaceFallbackRule(got, "a", nil)
	if fallbackTargets(got, "a") != nil || len(got) != 1 {
		t.Fatalf("cleared rules = %#v", got)
	}
}

func TestReplaceFallbacksNodePreservesCommentsAndAddsSection(t *testing.T) {
	original := []byte(`# config comment
model_list:
  - model_name: a
    deployments:
      - provider: openai
        model: a
        base_url: http://example.test
  - model_name: b
    deployments:
      - provider: openai
        model: b
        base_url: http://example.test
telemetry:
  metrics:
    enabled: false
`)
	updated, err := replaceFallbacksNode(original, []agentmodel.FallbackRule{{ModelName: "a", FallbackTo: []string{"b"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated), "# config comment") || !strings.Contains(string(updated), "fallback_to:") {
		t.Fatalf("updated YAML:\n%s", updated)
	}
	var cfg agentmodel.Config
	if err := yaml.Unmarshal(updated, &cfg); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fallbackTargets(cfg.Fallbacks, "a"), ","); got != "b" {
		t.Fatalf("fallback = %q", got)
	}
}

func TestReplaceFallbacksNodeRemovesEmptySection(t *testing.T) {
	original := []byte(`model_list:
  - model_name: a
    deployments:
      - provider: openai
        model: a
        base_url: http://example.test
fallbacks:
  - model_name: a
    fallback_to: [a]
`)
	updated, err := replaceFallbacksNode(original, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(updated), "fallbacks:") {
		t.Fatalf("fallbacks section remains:\n%s", updated)
	}
}
