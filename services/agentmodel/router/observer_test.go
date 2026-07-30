package router_test

import (
	"context"
	"errors"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/stub"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
)

type capturingObserver struct {
	results   []string // "model/provider/deployment=ok"
	cooldowns []string // "model/provider/deployment"
}

func (c *capturingObserver) DeploymentResult(model, provider, deployment string, ok bool) {
	v := "false"
	if ok {
		v = "true"
	}
	c.results = append(c.results, model+"/"+provider+"/"+deployment+"="+v)
}

func (c *capturingObserver) Cooldown(model, provider, deployment string) {
	c.cooldowns = append(c.cooldowns, model+"/"+provider+"/"+deployment)
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestObserver_Success(t *testing.T) {
	obs := &capturingObserver{}
	r := router.New(map[string][]router.Deployment{
		"gpt-4": {{Name: "primary", Provider: &stub.Stub{NameValue: "openai"}, Model: "gpt-4o", Weight: 1}},
	}, nil)
	r.SetObserver(obs)

	if _, err := r.Complete(context.Background(), agentmodel.ChatRequest{
		Model: "gpt-4", Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if want := []string{"gpt-4/openai/primary=true"}; !eqStrings(obs.results, want) {
		t.Errorf("results = %v, want %v", obs.results, want)
	}
	if len(obs.cooldowns) != 0 {
		t.Errorf("cooldowns = %v, want none", obs.cooldowns)
	}
}

// TestObserver_RetryableFailureCoolsAndFallsBack uses one deployment per model
// (so weighted-shuffle order is irrelevant): the primary fails retryably and
// cools, then the fallback model's deployment succeeds.
func TestObserver_RetryableFailureCoolsAndFallsBack(t *testing.T) {
	obs := &capturingObserver{}
	failer := &stub.Stub{NameValue: "openai", CompleteErr: errors.New("503 service unavailable")}
	winner := &stub.Stub{NameValue: "secondary"}
	r := router.New(map[string][]router.Deployment{
		"primary": {{Name: "p1", Provider: failer, Model: "m-a", Weight: 1}},
		"backup":  {{Name: "b1", Provider: winner, Model: "m-b", Weight: 1}},
	}, map[string][]string{"primary": {"backup"}})
	r.SetObserver(obs)

	if _, err := r.Complete(context.Background(), agentmodel.ChatRequest{
		Model: "primary", Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	wantResults := []string{"primary/openai/p1=false", "backup/secondary/b1=true"}
	if !eqStrings(obs.results, wantResults) {
		t.Errorf("results = %v, want %v", obs.results, wantResults)
	}
	wantCooldowns := []string{"primary/openai/p1"}
	if !eqStrings(obs.cooldowns, wantCooldowns) {
		t.Errorf("cooldowns = %v, want %v", obs.cooldowns, wantCooldowns)
	}
}

// TestObserver_DisabledCooldownEmitsNoCooldownEvent mirrors the test above but
// with cooling disabled: the failed attempt still reports a (false) result and
// falls back, but no Cooldown event is emitted because nothing was parked.
func TestObserver_DisabledCooldownEmitsNoCooldownEvent(t *testing.T) {
	obs := &capturingObserver{}
	failer := &stub.Stub{NameValue: "openai", CompleteErr: errors.New("503 service unavailable")}
	winner := &stub.Stub{NameValue: "secondary"}
	r := router.New(map[string][]router.Deployment{
		"primary": {{Name: "p1", Provider: failer, Model: "m-a", Weight: 1}},
		"backup":  {{Name: "b1", Provider: winner, Model: "m-b", Weight: 1}},
	}, map[string][]string{"primary": {"backup"}}, router.WithCooldown(0))
	r.SetObserver(obs)

	if _, err := r.Complete(context.Background(), agentmodel.ChatRequest{
		Model: "primary", Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	wantResults := []string{"primary/openai/p1=false", "backup/secondary/b1=true"}
	if !eqStrings(obs.results, wantResults) {
		t.Errorf("results = %v, want %v", obs.results, wantResults)
	}
	if len(obs.cooldowns) != 0 {
		t.Errorf("cooldowns = %v, want none (cooling disabled)", obs.cooldowns)
	}
}
