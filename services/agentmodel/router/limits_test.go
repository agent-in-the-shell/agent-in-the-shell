package router_test

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/stub"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
)

func intPtr(i int) *int { return &i }

func TestDeploymentLimits_ReportsConfiguredCaps(t *testing.T) {
	r := newRouter(map[string][]router.Deployment{
		"sonnet": {
			{Name: "api/sonnet", Provider: &stub.Stub{NameValue: "anthropic"}, Model: "sonnet", Weight: 1, RPM: intPtr(60), TPM: intPtr(90000)},
		},
		"gpt": {
			{Name: "oa/gpt", Provider: &stub.Stub{NameValue: "openai"}, Model: "gpt-4o", Weight: 1}, // uncapped
		},
	}, nil)

	got := r.DeploymentLimits(time.Now())
	if len(got) != 2 {
		t.Fatalf("len: got %d, want 2", len(got))
	}
	// Sorted by (model_name, deployment): "gpt" sorts before "sonnet".
	if got[0].ModelName != "gpt" || got[1].ModelName != "sonnet" {
		t.Fatalf("not sorted by model_name: %+v", got)
	}

	gpt := got[0]
	if gpt.Provider != "openai" {
		t.Errorf("gpt provider: got %q, want openai", gpt.Provider)
	}
	if gpt.RPM != nil || gpt.TPM != nil {
		t.Errorf("gpt should be uncapped, got rpm=%v tpm=%v", gpt.RPM, gpt.TPM)
	}
	if gpt.CooledUntil != nil {
		t.Errorf("gpt should not be cooled")
	}

	son := got[1]
	if son.Provider != "anthropic" {
		t.Errorf("sonnet provider: got %q, want anthropic", son.Provider)
	}
	if son.RPM == nil || *son.RPM != 60 {
		t.Errorf("sonnet rpm: got %v, want 60", son.RPM)
	}
	if son.TPM == nil || *son.TPM != 90000 {
		t.Errorf("sonnet tpm: got %v, want 90000", son.TPM)
	}
}

func TestDeploymentLimits_ReflectsCooldown(t *testing.T) {
	rl := &stub.Stub{NameValue: "subscription", CompleteErr: stub.ErrTestRateLimit}
	ok := &stub.Stub{NameValue: "apikey"}
	r := newRouter(map[string][]router.Deployment{
		"sonnet": {
			{Name: "sub/sonnet", Provider: rl, Model: "sonnet-sub", Weight: 1000},
			{Name: "api/sonnet", Provider: ok, Model: "sonnet-api", Weight: 1},
		},
	}, nil)

	// One call: the subscription deployment rate-limits and gets cooled, the
	// apikey deployment succeeds.
	if _, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "sonnet"}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	now := time.Now()
	byDep := map[string]router.DeploymentLimit{}
	for _, dl := range r.DeploymentLimits(now) {
		byDep[dl.Deployment] = dl
	}

	sub, ok2 := byDep["sub/sonnet"]
	if !ok2 {
		t.Fatalf("sub/sonnet missing from limits")
	}
	if sub.CooledUntil == nil {
		t.Fatalf("sub/sonnet should be cooled after a rate limit")
	}
	if !sub.CooledUntil.After(now) {
		t.Errorf("cooldown should be in the future: until=%v now=%v", sub.CooledUntil, now)
	}
	if api := byDep["api/sonnet"]; api.CooledUntil != nil {
		t.Errorf("api/sonnet should not be cooled")
	}
}

// TestDeploymentLimits_HonorsConfiguredCooldown proves the park length tracks
// the configured WithCooldown value, not the 5m default — asserted via the
// CooledUntil window so the test stays deterministic (no sleeping).
func TestDeploymentLimits_HonorsConfiguredCooldown(t *testing.T) {
	rl := &stub.Stub{NameValue: "subscription", CompleteErr: stub.ErrTestRateLimit}
	ok := &stub.Stub{NameValue: "apikey"}
	const cooldown = 2 * time.Minute
	r := router.NewWithRand(map[string][]router.Deployment{
		"sonnet": {
			{Name: "sub/sonnet", Provider: rl, Model: "sonnet-sub", Weight: 1000},
			{Name: "api/sonnet", Provider: ok, Model: "sonnet-api", Weight: 1},
		},
	}, nil, rand.NewSource(42), router.WithCooldown(cooldown))

	if _, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "sonnet"}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	now := time.Now()
	var sub *router.DeploymentLimit
	for _, dl := range r.DeploymentLimits(now) {
		if dl.Deployment == "sub/sonnet" {
			d := dl
			sub = &d
		}
	}
	if sub == nil || sub.CooledUntil == nil {
		t.Fatalf("sub/sonnet should be cooled after a rate limit")
	}
	// CooledUntil = parkStart + cooldown, and parkStart <= now, so the remaining
	// window is in (cooldown - ε, cooldown]. A 2m window also proves it is not
	// the 5m default.
	delta := sub.CooledUntil.Sub(now)
	if delta > cooldown || delta < cooldown-2*time.Second {
		t.Errorf("cooldown window: got %v, want ~%v (configured), not the 5m default", delta, cooldown)
	}
}

// TestDeploymentLimits_DisabledCooldownNotParked proves WithCooldown(0) leaves
// no cooldown entry at all after a retryable failure (not just a zero-length
// one), so the deployment is immediately eligible again.
func TestDeploymentLimits_DisabledCooldownNotParked(t *testing.T) {
	rl := &stub.Stub{NameValue: "subscription", CompleteErr: stub.ErrTestRateLimit}
	ok := &stub.Stub{NameValue: "apikey"}
	r := router.NewWithRand(map[string][]router.Deployment{
		"sonnet": {
			{Name: "sub/sonnet", Provider: rl, Model: "sonnet-sub", Weight: 1000},
			{Name: "api/sonnet", Provider: ok, Model: "sonnet-api", Weight: 1},
		},
	}, nil, rand.NewSource(42), router.WithCooldown(0))

	if _, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "sonnet"}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	for _, dl := range r.DeploymentLimits(time.Now()) {
		if dl.CooledUntil != nil {
			t.Errorf("%s: should not be cooled when cooldown disabled, got until=%v", dl.Deployment, dl.CooledUntil)
		}
	}
}

func TestDeploymentLimits_ExpiredCooldownReaped(t *testing.T) {
	rl := &stub.Stub{NameValue: "subscription", CompleteErr: stub.ErrTestRateLimit}
	ok := &stub.Stub{NameValue: "apikey"}
	r := newRouter(map[string][]router.Deployment{
		"sonnet": {
			{Name: "sub/sonnet", Provider: rl, Model: "sonnet-sub", Weight: 1000},
			{Name: "api/sonnet", Provider: ok, Model: "sonnet-api", Weight: 1},
		},
	}, nil)
	if _, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "sonnet"}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Query far in the future: the cooldown (5m) has elapsed, so it must read
	// as not-cooled.
	future := time.Now().Add(time.Hour)
	for _, dl := range r.DeploymentLimits(future) {
		if dl.CooledUntil != nil {
			t.Errorf("%s: cooldown should be reaped by %v, got until=%v", dl.Deployment, future, dl.CooledUntil)
		}
	}
}

func TestDeploymentLimits_Empty(t *testing.T) {
	r := newRouter(map[string][]router.Deployment{}, nil)
	if got := r.DeploymentLimits(time.Now()); len(got) != 0 {
		t.Errorf("expected no limits, got %d", len(got))
	}
}
