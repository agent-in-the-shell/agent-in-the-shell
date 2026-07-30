package router_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/pool"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/stub"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
)

func TestValidateDeployments_CachesAcrossPassesWithinTTL(t *testing.T) {
	openaiStub := &stub.Stub{NameValue: "openai", LiveModels: []string{"gpt-4o"}}
	deps := map[string][]router.Deployment{
		"good":    {{Provider: openaiStub, Model: "gpt-4o", Weight: 1}},
		"drifted": {{Provider: openaiStub, Model: "gpt-gone", Weight: 1}},
	}
	clk := stub.NewClock()
	r := router.New(deps, nil, router.WithNow(clk.Now), router.WithModelCacheTTL(time.Hour))

	// First pass fetches once and flags the drifted deployment.
	if drift := r.ValidateDeployments(context.Background()); len(drift) != 1 || drift[0].ModelName != "drifted" {
		t.Fatalf("pass 1: want 1 drift on 'drifted', got %+v", drift)
	}
	if got := openaiStub.ListModelsCalls(); got != 1 {
		t.Fatalf("pass 1: ListModels calls = %d, want 1", got)
	}

	// Within the TTL the cached list is reused — no refetch.
	clk.Add(30 * time.Minute)
	if drift := r.ValidateDeployments(context.Background()); len(drift) != 1 {
		t.Fatalf("pass 2: want 1 drift, got %+v", drift)
	}
	if got := openaiStub.ListModelsCalls(); got != 1 {
		t.Fatalf("pass 2 (within TTL): ListModels calls = %d, want 1 (cache reused)", got)
	}

	// Past the TTL the list is refetched.
	clk.Add(2 * time.Hour)
	if drift := r.ValidateDeployments(context.Background()); len(drift) != 1 {
		t.Fatalf("pass 3: want 1 drift, got %+v", drift)
	}
	if got := openaiStub.ListModelsCalls(); got != 2 {
		t.Fatalf("pass 3 (past TTL): ListModels calls = %d, want 2 (refetched)", got)
	}
}

func TestValidateDeployments_LastGoodOnFailure(t *testing.T) {
	openaiStub := &stub.Stub{NameValue: "openai", LiveModels: []string{"gpt-4o"}}
	deps := map[string][]router.Deployment{
		"good":    {{Provider: openaiStub, Model: "gpt-4o", Weight: 1}},
		"drifted": {{Provider: openaiStub, Model: "gpt-gone", Weight: 1}},
	}
	clk := stub.NewClock()
	r := router.New(deps, nil, router.WithNow(clk.Now), router.WithModelCacheTTL(time.Hour))

	// Prime a good list.
	if drift := r.ValidateDeployments(context.Background()); len(drift) != 1 {
		t.Fatalf("prime: want 1 drift, got %+v", drift)
	}

	// Provider now fails; advance past the TTL to force a refresh attempt.
	openaiStub.ListModelsErr = errors.New("network down")
	clk.Add(2 * time.Hour)

	// The failed refresh must fall back to the last-good list, so the result is
	// unchanged — exactly one drift ('good' is not re-flagged, 'drifted' stays).
	drift := r.ValidateDeployments(context.Background())
	if len(drift) != 1 || drift[0].ModelName != "drifted" {
		t.Fatalf("after failure: want last-good result (1 drift on 'drifted'), got %+v", drift)
	}
}

func TestValidateDeployments_CacheKeyDistinguishesAuthMode(t *testing.T) {
	// Same provider name, different credentials/entitlements: the cache key
	// composes name+auth_mode, so each is validated against its OWN list.
	keyStub := &stub.Stub{NameValue: "anthropic", AuthModeValue: agentmodel.AuthModeAPIKey, LiveModels: []string{"claude-a"}}
	subStub := &stub.Stub{NameValue: "anthropic", AuthModeValue: agentmodel.AuthModeSubscription, LiveModels: []string{"claude-b"}}

	deps := map[string][]router.Deployment{
		"key-ok":    {{Provider: keyStub, Model: "claude-a", Weight: 1}}, // in key list
		"sub-ok":    {{Provider: subStub, Model: "claude-b", Weight: 1}}, // in sub list
		"key-wrong": {{Provider: keyStub, Model: "claude-b", Weight: 1}}, // NOT in key list -> drift
		"sub-wrong": {{Provider: subStub, Model: "claude-a", Weight: 1}}, // NOT in sub list -> drift
	}
	r := router.New(deps, nil)

	drift := r.ValidateDeployments(context.Background())
	if len(drift) != 2 {
		t.Fatalf("want exactly 2 drifts (key-wrong, sub-wrong), got %+v", drift)
	}
	got := map[string]bool{}
	for _, d := range drift {
		got[d.ModelName] = true
	}
	if !got["key-wrong"] || !got["sub-wrong"] {
		t.Errorf("drift set = %v, want {key-wrong, sub-wrong}", got)
	}
	// Distinct keys are fetched independently.
	if keyStub.ListModelsCalls() != 1 || subStub.ListModelsCalls() != 1 {
		t.Errorf("ListModels calls: key=%d sub=%d, want 1+1", keyStub.ListModelsCalls(), subStub.ListModelsCalls())
	}
}

func TestDiscoverModels(t *testing.T) {
	openaiStub := &stub.Stub{NameValue: "openai", LiveModels: []string{"gpt-4o", "gpt-4o-mini", "gpt-5"}}
	deps := map[string][]router.Deployment{
		"my-gpt": {{Provider: openaiStub, Model: "gpt-4o", Weight: 1}},
	}
	r := router.New(deps, nil)

	got := r.DiscoverModels(context.Background())
	if len(got) != 3 {
		t.Fatalf("want 3 discovered models, got %+v", got)
	}
	configured := map[string]bool{}
	for _, am := range got {
		if am.Provider != "openai" {
			t.Errorf("%s: provider = %q, want openai", am.Model, am.Provider)
		}
		configured[am.Model] = am.Configured
	}
	if !configured["gpt-4o"] {
		t.Errorf("gpt-4o should be Configured (a deployment routes to it): %+v", got)
	}
	if configured["gpt-4o-mini"] || configured["gpt-5"] {
		t.Errorf("unconfigured upstream models must be Configured=false: %+v", got)
	}
	// Discovery returns sorted by (provider, model).
	want := []string{"gpt-4o", "gpt-4o-mini", "gpt-5"}
	for i, am := range got {
		if am.Model != want[i] {
			t.Errorf("order[%d]: got %q, want %q", i, am.Model, want[i])
		}
	}
}

// TestValidateDeployments_PooledDeploymentIsValidated: a multi-credential
// deployment must still be drift-checked. ValidateDeployments type-asserts
// provider.ModelLister and silently skips anything that fails it, so a pool
// that does not forward the capability would quietly disable model validation
// for every pooled provider.
func TestValidateDeployments_PooledDeploymentIsValidated(t *testing.T) {
	acctA := &stub.Stub{NameValue: "anthropic", LiveModels: []string{"claude-sonnet-4-5"}}
	acctB := &stub.Stub{NameValue: "anthropic", LiveModels: []string{"claude-sonnet-4-5"}}
	pooled := pool.New([]provider.Provider{acctA, acctB})
	deps := map[string][]router.Deployment{
		"claude": {{Provider: pooled, Model: "claude-gone", Weight: 1}},
	}
	r := router.New(deps, nil)

	drift := r.ValidateDeployments(context.Background())
	if len(drift) != 1 || drift[0].Provider != "anthropic" || drift[0].Model != "claude-gone" {
		t.Fatalf("want 1 drift on anthropic/claude-gone, got %+v", drift)
	}
	if acctB.ListModelsCalls() != 0 {
		t.Errorf("second credential should be untouched, got %d calls", acctB.ListModelsCalls())
	}
}
