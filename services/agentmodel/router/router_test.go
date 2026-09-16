package router_test

import (
	"context"
	"errors"
	"io"
	"iter"
	"math/rand"
	"net/http"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/stub"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
)

func newRouter(deps map[string][]router.Deployment, fb map[string][]string) *router.Router {
	return router.NewWithRand(deps, fb, rand.NewSource(42))
}

// nonLister implements provider.Provider but NOT provider.ModelLister, to
// exercise ValidateDeployments' skip path.
type nonLister struct{}

func (nonLister) Name() string              { return "nolister" }
func (nonLister) SupportedModels() []string { return nil }
func (nonLister) AuthMode() string          { return agentmodel.AuthModeAPIKey }
func (nonLister) Complete(context.Context, agentmodel.ChatRequest) (agentmodel.ChatResponse, error) {
	return agentmodel.ChatResponse{}, nil
}
func (nonLister) Stream(context.Context, agentmodel.ChatRequest) (iter.Seq2[provider.StreamChunk, error], error) {
	return nil, nil
}
func (nonLister) Embed(context.Context, agentmodel.EmbeddingRequest) (agentmodel.EmbeddingResponse, error) {
	return agentmodel.EmbeddingResponse{}, nil
}

// ─── Complete: happy path ─────────────────────────────────────────────────

func TestComplete_SingleDeploymentSuccess(t *testing.T) {
	s := &stub.Stub{NameValue: "openai", Models: []string{"gpt-4o"}}
	r := newRouter(map[string][]router.Deployment{
		"gpt-4": {{Provider: s, Model: "gpt-4o", Weight: 100}},
	}, nil)

	resp, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "gpt-4"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Model != "gpt-4o" {
		t.Errorf("Model: got %q, want gpt-4o (resolved upstream id)", resp.Model)
	}
	if resp.Usage.AuthMode != agentmodel.AuthModeAPIKey {
		t.Errorf("AuthMode: got %q, want api_key", resp.Usage.AuthMode)
	}
	if s.CallCount() != 1 {
		t.Errorf("CallCount: got %d, want 1", s.CallCount())
	}
}

// ─── Complete: retryable failure → fallback ───────────────────────────────

func TestComplete_RetryableFailureFallsBackInDeployment(t *testing.T) {
	failer := &stub.Stub{
		NameValue:   "primary",
		CompleteErr: errors.New("503 service unavailable"),
	}
	winner := &stub.Stub{NameValue: "secondary"}

	r := newRouter(map[string][]router.Deployment{
		"gpt-4": {
			{Provider: failer, Model: "gpt-4o", Weight: 1000}, // heavily favored to be tried first
			{Provider: winner, Model: "claude-3-opus", Weight: 1},
		},
	}, nil)

	resp, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "gpt-4"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// Falls back from primary (gpt-4o) to secondary (claude-3-opus) — Model
	// reflects whichever deployment actually responded.
	if resp.Model != "claude-3-opus" {
		t.Errorf("Model: got %q, want claude-3-opus", resp.Model)
	}
	if failer.CallCount() != 1 || winner.CallCount() != 1 {
		t.Errorf("CallCount: failer=%d winner=%d, want 1+1", failer.CallCount(), winner.CallCount())
	}
}

// ─── Complete: non-retryable failure halts immediately ────────────────────

func TestComplete_NonRetryableFailureHalts(t *testing.T) {
	failer := &stub.Stub{
		NameValue:   "primary",
		CompleteErr: errors.New("401 invalid_api_key"),
	}
	other := &stub.Stub{NameValue: "secondary"}

	r := newRouter(map[string][]router.Deployment{
		"gpt-4": {
			{Provider: failer, Model: "gpt-4o", Weight: 1000},
			{Provider: other, Model: "claude", Weight: 1},
		},
	}, nil)

	_, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "gpt-4"})
	if err == nil {
		t.Fatal("expected auth error, got nil")
	}

	var ae *agentmodel.Error
	if !errors.As(err, &ae) {
		t.Fatalf("error is not *agentmodel.Error: %v", err)
	}
	if ae.Type != agentmodel.ErrTypeAuthentication {
		t.Errorf("Type: got %q, want authentication_error", ae.Type)
	}

	if other.CallCount() != 0 {
		t.Errorf("non-retryable should NOT try other deployments; other.CallCount=%d", other.CallCount())
	}
}

// ─── Complete: all deployments fail → fallback model_name ─────────────────

func TestComplete_AllFailFallbackToOtherModel(t *testing.T) {
	primaryFail := &stub.Stub{
		NameValue:   "primary",
		CompleteErr: errors.New("503 overloaded"),
	}
	fallbackOK := &stub.Stub{NameValue: "fallback"}

	r := newRouter(
		map[string][]router.Deployment{
			"gpt-4":    {{Provider: primaryFail, Model: "gpt-4o", Weight: 1}},
			"claude-3": {{Provider: fallbackOK, Model: "claude", Weight: 1}},
		},
		map[string][]string{"gpt-4": {"claude-3"}},
	)

	resp, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "gpt-4"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// Fallback resolved through claude-3 logical → "claude" upstream deployment.
	if resp.Model != "claude" {
		t.Errorf("Model: got %q, want claude (resolved upstream from claude-3 fallback)", resp.Model)
	}
}

// ─── Complete: all paths fail retryably → ErrTypeRateLimit (429) ───────────

// When every deployment + fallback fails with a retryable error (here 503),
// the exhaustion is surfaced as a rate-limit (429) so a client backs off,
// matching messagesPassthroughWithSeen — not 500.
func TestComplete_AllPathsRetryableFailReturnsRateLimit(t *testing.T) {
	a := &stub.Stub{CompleteErr: errors.New("503")}
	b := &stub.Stub{CompleteErr: errors.New("503")}

	r := newRouter(
		map[string][]router.Deployment{
			"primary":  {{Provider: a, Model: "x", Weight: 1}},
			"fallback": {{Provider: b, Model: "y", Weight: 1}},
		},
		map[string][]string{"primary": {"fallback"}},
	)

	_, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "primary"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ae *agentmodel.Error
	if !errors.As(err, &ae) {
		t.Fatalf("error is not *agentmodel.Error: %v", err)
	}
	if ae.Type != agentmodel.ErrTypeRateLimit {
		t.Errorf("Type: got %q, want rate_limit_error", ae.Type)
	}
	if ae.HTTPStatus() != 429 {
		t.Errorf("HTTPStatus: got %d, want 429", ae.HTTPStatus())
	}
}

// TestComplete_AllCooledReturnsRateLimit reproduces the finding's exact scenario:
// a request that finds every deployment already cooled (by a prior retryable
// failure) must return 429, not 500 — even though the provider would now succeed
// if called (it isn't: a cooled deployment is skipped).
func TestComplete_AllCooledReturnsRateLimit(t *testing.T) {
	s := &stub.Stub{CompleteErr: errors.New("503")}
	r := newRouter(map[string][]router.Deployment{
		"primary": {{Provider: s, Model: "x", Name: "d1", Weight: 1}},
	}, nil)

	// First call fails retryably and cools the only deployment.
	if _, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "primary"}); err == nil {
		t.Fatal("expected the first (retryable-failing) call to error")
	}

	// The deployment would now succeed if called — but it is cooled, so the next
	// call must skip it and surface a 429 rather than calling it (or returning 500).
	s.CompleteErr = nil
	_, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "primary"})
	if err == nil {
		t.Fatal("expected 429 while the only deployment is cooled, got success (deployment was not skipped)")
	}
	var ae *agentmodel.Error
	if !errors.As(err, &ae) {
		t.Fatalf("error is not *agentmodel.Error: %v", err)
	}
	if ae.Type != agentmodel.ErrTypeRateLimit {
		t.Errorf("Type: got %q, want rate_limit_error (got HTTP %d)", ae.Type, ae.HTTPStatus())
	}
}

// ─── Complete: no deployment configured ───────────────────────────────────

func TestComplete_NoDeployment(t *testing.T) {
	r := newRouter(map[string][]router.Deployment{}, nil)
	_, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "ghost"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ae *agentmodel.Error
	if !errors.As(err, &ae) {
		t.Fatalf("error is not *agentmodel.Error: %v", err)
	}
	if ae.Type != agentmodel.ErrTypeNotFound {
		t.Errorf("Type: got %q, want not_found_error", ae.Type)
	}
}

// ─── Complete: cycle detection ────────────────────────────────────────────

func TestComplete_FallbackCycleTerminates(t *testing.T) {
	a := &stub.Stub{CompleteErr: errors.New("503")}
	b := &stub.Stub{CompleteErr: errors.New("503")}

	r := newRouter(
		map[string][]router.Deployment{
			"a": {{Provider: a, Model: "x", Weight: 1}},
			"b": {{Provider: b, Model: "y", Weight: 1}},
		},
		map[string][]string{"a": {"b"}, "b": {"a"}},
	)

	_, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "a"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// must not infinite loop
}

// ─── Stream: happy path ───────────────────────────────────────────────────

func TestStream_HappyPath(t *testing.T) {
	s := &stub.Stub{NameValue: "openai"}
	r := newRouter(map[string][]router.Deployment{
		"gpt-4": {{Provider: s, Model: "gpt-4o", Weight: 1}},
	}, nil)

	seq, meta, err := r.Stream(context.Background(), agentmodel.ChatRequest{Model: "gpt-4", Stream: true})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if meta.AuthMode != agentmodel.AuthModeAPIKey {
		t.Errorf("AuthMode: got %q, want api_key", meta.AuthMode)
	}
	if meta.ModelUsed != "gpt-4o" {
		t.Errorf("ModelUsed: got %q, want gpt-4o", meta.ModelUsed)
	}

	var chunks []provider.StreamChunk
	for c, err := range seq {
		if err != nil {
			t.Fatalf("chunk error: %v", err)
		}
		chunks = append(chunks, c)
	}
	if len(chunks) == 0 {
		t.Errorf("no chunks received")
	}
}

// ─── Stream: retryable error → fallback ───────────────────────────────────

func TestStream_RetryableErrorFallsBack(t *testing.T) {
	failer := &stub.Stub{StreamErr: errors.New("503 service unavailable")}
	winner := &stub.Stub{NameValue: "winner"}

	r := newRouter(map[string][]router.Deployment{
		"gpt-4": {
			{Provider: failer, Model: "gpt-4o", Weight: 1000},
			{Provider: winner, Model: "claude", Weight: 1},
		},
	}, nil)

	seq, _, err := r.Stream(context.Background(), agentmodel.ChatRequest{Model: "gpt-4", Stream: true})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunkCount := 0
	for _, err := range seq {
		if err != nil {
			t.Fatalf("chunk error: %v", err)
		}
		chunkCount++
	}
	if chunkCount == 0 {
		t.Error("no chunks received from winner")
	}
}

// ─── Embed: happy path ────────────────────────────────────────────────────

func TestEmbed_HappyPath(t *testing.T) {
	s := &stub.Stub{NameValue: "openai"}
	r := newRouter(map[string][]router.Deployment{
		"text-embedding-3-small": {{Provider: s, Model: "text-embedding-3-small", Weight: 1}},
	}, nil)

	resp, err := r.Embed(context.Background(), agentmodel.EmbeddingRequest{
		Model: "text-embedding-3-small",
		Input: []string{"hi"},
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(resp.Data) == 0 {
		t.Error("no embeddings returned")
	}
	if resp.Usage.AuthMode != agentmodel.AuthModeAPIKey {
		t.Errorf("AuthMode: got %q, want api_key", resp.Usage.AuthMode)
	}
}

// ─── Weighted shuffle distribution sanity ─────────────────────────────────

func TestWeightedShuffle_HighWeightDominates(t *testing.T) {
	heavy := &stub.Stub{NameValue: "heavy"}
	light := &stub.Stub{NameValue: "light"}

	deployments := map[string][]router.Deployment{
		"x": {
			{Provider: heavy, Model: "h", Weight: 99},
			{Provider: light, Model: "l", Weight: 1},
		},
	}
	// Use deterministic seed to make this stable.
	r := router.NewWithRand(deployments, nil, rand.NewSource(7))

	const N = 200
	for i := 0; i < N; i++ {
		_, _ = r.Complete(context.Background(), agentmodel.ChatRequest{Model: "x"})
	}
	// heavy should be picked first WAY more often than light.
	heavyCount := heavy.CallCount()
	lightCount := light.CallCount()
	if heavyCount < int32(float64(N)*0.85) {
		t.Errorf("heavy was tried first only %d/%d times; expected >= %d", heavyCount, N, int(float64(N)*0.85))
	}
	_ = lightCount
}

// ─── Cooldown: retryable error cools deployment ───────────────────────────

func TestComplete_CooldownSkipsDepAfterRateLimit(t *testing.T) {
	rl := &stub.Stub{
		NameValue:   "subscription",
		CompleteErr: stub.ErrTestRateLimit,
	}
	ok := &stub.Stub{NameValue: "apikey"}

	r := newRouter(map[string][]router.Deployment{
		"sonnet": {
			{Name: "sub/sonnet", Provider: rl, Model: "sonnet-sub", Weight: 1000},
			{Name: "api/sonnet", Provider: ok, Model: "sonnet-api", Weight: 1},
		},
	}, nil)

	// First call: subscription rate-limits, apikey succeeds. sub is now cooled.
	if _, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "sonnet"}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	callAfterFirst := rl.CallCount()

	// Second call: subscription should be cooled and skipped entirely.
	if _, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "sonnet"}); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if rl.CallCount() != callAfterFirst {
		t.Errorf("cooled deployment was retried on second call")
	}
	if ok.CallCount() != 2 {
		t.Errorf("apikey provider expected 2 calls, got %d", ok.CallCount())
	}
}

// WithCooldown(0) disables cooling: a deployment that rate-limits on one call
// is retried (not skipped) on the next, unlike the default-cooldown case above.
func TestComplete_DisabledCooldownRetriesDep(t *testing.T) {
	rl := &stub.Stub{
		NameValue:   "subscription",
		CompleteErr: stub.ErrTestRateLimit,
	}
	ok := &stub.Stub{NameValue: "apikey"}

	r := router.NewWithRand(map[string][]router.Deployment{
		"sonnet": {
			{Name: "sub/sonnet", Provider: rl, Model: "sonnet-sub", Weight: 1000},
			{Name: "api/sonnet", Provider: ok, Model: "sonnet-api", Weight: 1},
		},
	}, nil, rand.NewSource(42), router.WithCooldown(0))

	if _, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "sonnet"}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	callAfterFirst := rl.CallCount()

	// Second call: with cooling disabled, the subscription dep is tried again
	// (it rate-limits again, then apikey succeeds).
	if _, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "sonnet"}); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if rl.CallCount() <= callAfterFirst {
		t.Errorf("disabled cooldown: rate-limited deployment was not retried on second call")
	}
}

// ─── MessagesPassthrough ──────────────────────────────────────────────────

func passthroughFn(code int, body string) func(context.Context, []byte, string, string) (*http.Response, error) {
	return stub.PassthroughFunc(code, body)
}

func TestMessagesPassthrough_FirstSucceeds(t *testing.T) {
	s := &stub.Stub{MessagesPassthroughFn: passthroughFn(200, `{"id":"r1"}`)}
	r := newRouter(map[string][]router.Deployment{
		"m": {{Name: "dep/m", Provider: s, Model: "m-upstream", Weight: 1}},
	}, nil)

	resp, dep, _, err := r.MessagesPassthrough(context.Background(), []byte(`{}`), "m", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != `{"id":"r1"}` {
		t.Errorf("unexpected body: %s", b)
	}
	if dep.Model != "m-upstream" {
		t.Errorf("dep.Model: got %q, want m-upstream", dep.Model)
	}
}

func TestMessagesPassthrough_429FallsToNextDeployment(t *testing.T) {
	rl := &stub.Stub{MessagesPassthroughFn: passthroughFn(429, `{}`)}
	ok := &stub.Stub{MessagesPassthroughFn: passthroughFn(200, `{"id":"r2"}`)}
	r := newRouter(map[string][]router.Deployment{
		"m": {
			{Name: "sub/m", Provider: rl, Model: "m-sub", Weight: 1000},
			{Name: "api/m", Provider: ok, Model: "m-api", Weight: 1},
		},
	}, nil)

	resp, dep, _, err := r.MessagesPassthrough(context.Background(), []byte(`{}`), "m", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != `{"id":"r2"}` {
		t.Errorf("unexpected body: %s", b)
	}
	if dep.Model != "m-api" {
		t.Errorf("dep.Model: got %q, want m-api", dep.Model)
	}
}

func TestMessagesPassthrough_FollowsFallbackChain(t *testing.T) {
	rl := &stub.Stub{MessagesPassthroughFn: passthroughFn(429, `{}`)}
	fb := &stub.Stub{MessagesPassthroughFn: passthroughFn(200, `{"id":"fb"}`)}
	r := newRouter(
		map[string][]router.Deployment{
			"primary":  {{Name: "sub/primary", Provider: rl, Model: "primary-upstream", Weight: 1}},
			"fallback": {{Name: "api/fallback", Provider: fb, Model: "fallback-upstream", Weight: 1}},
		},
		map[string][]string{"primary": {"fallback"}},
	)

	resp, dep, _, err := r.MessagesPassthrough(context.Background(), []byte(`{}`), "primary", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if dep.Model != "fallback-upstream" {
		t.Errorf("dep.Model: got %q, want fallback-upstream", dep.Model)
	}
}

func TestMessagesPassthrough_AllExhaustedReturnsError(t *testing.T) {
	rl := &stub.Stub{MessagesPassthroughFn: passthroughFn(429, `{}`)}
	r := newRouter(map[string][]router.Deployment{
		"m": {{Name: "sub/m", Provider: rl, Model: "m-sub", Weight: 1}},
	}, nil)

	_, _, _, err := r.MessagesPassthrough(context.Background(), []byte(`{}`), "m", "")
	if err == nil {
		t.Fatal("expected error when all deployments exhausted")
	}
}

func TestMessagesPassthrough_CooldownSkipsOnSecondCall(t *testing.T) {
	rl := &stub.Stub{MessagesPassthroughFn: passthroughFn(429, `{}`)}
	ok := &stub.Stub{MessagesPassthroughFn: passthroughFn(200, `{"id":"ok"}`)}
	r := newRouter(map[string][]router.Deployment{
		"m": {
			{Name: "sub/m", Provider: rl, Model: "m-sub", Weight: 1000},
			{Name: "api/m", Provider: ok, Model: "m-api", Weight: 1},
		},
	}, nil)

	// First call: rl gets 429, cools it, ok succeeds.
	resp1, _, _, err := r.MessagesPassthrough(context.Background(), []byte(`{}`), "m", "")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	resp1.Body.Close()
	rlAfterFirst := rl.CallCount()

	// Second call: rl is cooled and must be skipped.
	resp2, _, _, err := r.MessagesPassthrough(context.Background(), []byte(`{}`), "m", "")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	resp2.Body.Close()
	if rl.CallCount() != rlAfterFirst {
		t.Errorf("cooled deployment was called again on second request")
	}
}

func TestMessagesPassthrough_NonPassthroughProviderSkipped(t *testing.T) {
	// A stub with no MessagesPassthroughFn returns nil, nil — router skips it.
	noPassthrough := &stub.Stub{} // returns nil, nil from MessagesPassthrough
	ok := &stub.Stub{MessagesPassthroughFn: passthroughFn(200, `{"id":"ok"}`)}
	r := newRouter(map[string][]router.Deployment{
		"m": {
			{Name: "no/m", Provider: noPassthrough, Model: "no-upstream", Weight: 1000},
			{Name: "ok/m", Provider: ok, Model: "ok-upstream", Weight: 1},
		},
	}, nil)

	resp, dep, _, err := r.MessagesPassthrough(context.Background(), []byte(`{}`), "m", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if dep.Model != "ok-upstream" {
		t.Errorf("dep.Model: got %q, want ok-upstream", dep.Model)
	}
}

// ─── AuthMode propagation: subscription path ──────────────────────────────

func TestComplete_PropagatesSubscriptionAuthMode(t *testing.T) {
	subStub := &stub.Stub{NameValue: "chatgpt", AuthModeValue: agentmodel.AuthModeSubscription}
	r := newRouter(map[string][]router.Deployment{
		"chatgpt-pro": {{Provider: subStub, Model: "gpt-5", Weight: 1}},
	}, nil)

	resp, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "chatgpt-pro"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Usage.AuthMode != agentmodel.AuthModeSubscription {
		t.Errorf("AuthMode: got %q, want subscription", resp.Usage.AuthMode)
	}
}

// ─── PickDeployment ───────────────────────────────────────────────────────

func TestPickDeployment_NoDeployment(t *testing.T) {
	r := newRouter(map[string][]router.Deployment{}, nil)
	_, err := r.PickDeployment("ghost")
	if err == nil {
		t.Fatal("expected ErrNoDeployment, got nil")
	}
	var ae *agentmodel.Error
	if !errors.As(err, &ae) {
		t.Fatalf("error is not *agentmodel.Error: %v", err)
	}
	if ae.Type != agentmodel.ErrTypeNotFound {
		t.Errorf("Type: got %q, want not_found_error", ae.Type)
	}
}

func TestPickDeployment_Success(t *testing.T) {
	s := &stub.Stub{NameValue: "openai"}
	r := newRouter(map[string][]router.Deployment{
		"gpt-4": {{Name: "only", Provider: s, Model: "gpt-4o", Weight: 100}},
	}, nil)

	dep, err := r.PickDeployment("gpt-4")
	if err != nil {
		t.Fatalf("PickDeployment: %v", err)
	}
	if dep.Model != "gpt-4o" {
		t.Errorf("dep.Model: got %q, want gpt-4o", dep.Model)
	}
	if dep.Name != "only" {
		t.Errorf("dep.Name: got %q, want only", dep.Name)
	}
	if dep.Provider != s {
		t.Errorf("dep.Provider: got %v, want the configured stub", dep.Provider)
	}
}

// ─── Stream: no deployment, no fallback → ErrNoDeployment ─────────────────

func TestStream_NoDeployment(t *testing.T) {
	r := newRouter(map[string][]router.Deployment{}, nil)
	_, _, err := r.Stream(context.Background(), agentmodel.ChatRequest{Model: "ghost", Stream: true})
	if err == nil {
		t.Fatal("expected ErrNoDeployment, got nil")
	}
	var ae *agentmodel.Error
	if !errors.As(err, &ae) {
		t.Fatalf("error is not *agentmodel.Error: %v", err)
	}
	if ae.Type != agentmodel.ErrTypeNotFound {
		t.Errorf("Type: got %q, want not_found_error", ae.Type)
	}
}

// ─── Stream: non-retryable error halts immediately ────────────────────────

func TestStream_NonRetryableErrorHalts(t *testing.T) {
	failer := &stub.Stub{NameValue: "primary", StreamErr: errors.New("401 invalid_api_key")}
	other := &stub.Stub{NameValue: "secondary"}

	r := newRouter(map[string][]router.Deployment{
		"gpt-4": {
			{Provider: failer, Model: "gpt-4o", Weight: 1000},
			{Provider: other, Model: "claude", Weight: 1},
		},
	}, nil)

	_, _, err := r.Stream(context.Background(), agentmodel.ChatRequest{Model: "gpt-4", Stream: true})
	if err == nil {
		t.Fatal("expected auth error, got nil")
	}
	var ae *agentmodel.Error
	if !errors.As(err, &ae) {
		t.Fatalf("error is not *agentmodel.Error: %v", err)
	}
	if ae.Type != agentmodel.ErrTypeAuthentication {
		t.Errorf("Type: got %q, want authentication_error", ae.Type)
	}
	if other.CallCount() != 0 {
		t.Errorf("non-retryable should NOT try other deployments; other.CallCount=%d", other.CallCount())
	}
}

// ─── Stream: all deployments fail (retryable) → fallback model_name ───────

func TestStream_RetryableFallsBackToOtherModel(t *testing.T) {
	primaryFail := &stub.Stub{NameValue: "primary", StreamErr: errors.New("503 overloaded")}
	fallbackOK := &stub.Stub{NameValue: "fallback"}

	r := newRouter(
		map[string][]router.Deployment{
			"gpt-4":    {{Provider: primaryFail, Model: "gpt-4o", Weight: 1}},
			"claude-3": {{Provider: fallbackOK, Model: "claude", Weight: 1}},
		},
		map[string][]string{"gpt-4": {"claude-3"}},
	)

	seq, meta, err := r.Stream(context.Background(), agentmodel.ChatRequest{Model: "gpt-4", Stream: true})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if meta.ModelUsed != "claude" {
		t.Errorf("ModelUsed: got %q, want claude (resolved from fallback)", meta.ModelUsed)
	}
	chunkCount := 0
	for _, err := range seq {
		if err != nil {
			t.Fatalf("chunk error: %v", err)
		}
		chunkCount++
	}
	if chunkCount == 0 {
		t.Error("no chunks received from fallback")
	}
	if primaryFail.CallCount() != 1 || fallbackOK.CallCount() != 1 {
		t.Errorf("CallCount: primary=%d fallback=%d, want 1+1", primaryFail.CallCount(), fallbackOK.CallCount())
	}
}

// ─── Stream: all paths exhausted → ErrAllFailed ──────────────────────────

// When every deployment + fallback fails retryably, Stream surfaces a 429 (not
// 500), matching Complete and messagesPassthroughWithSeen.
func TestStream_AllPathsRetryableFailReturnsRateLimit(t *testing.T) {
	a := &stub.Stub{StreamErr: errors.New("503")}
	b := &stub.Stub{StreamErr: errors.New("503")}

	r := newRouter(
		map[string][]router.Deployment{
			"primary":  {{Provider: a, Model: "x", Weight: 1}},
			"fallback": {{Provider: b, Model: "y", Weight: 1}},
		},
		map[string][]string{"primary": {"fallback"}},
	)

	_, _, err := r.Stream(context.Background(), agentmodel.ChatRequest{Model: "primary", Stream: true})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ae *agentmodel.Error
	if !errors.As(err, &ae) {
		t.Fatalf("error is not *agentmodel.Error: %v", err)
	}
	if ae.Type != agentmodel.ErrTypeRateLimit {
		t.Errorf("Type: got %q, want rate_limit_error", ae.Type)
	}
}

// ─── Stream: fallback cycle guard terminates ──────────────────────────────

func TestStream_FallbackCycleTerminates(t *testing.T) {
	a := &stub.Stub{StreamErr: errors.New("503")}
	b := &stub.Stub{StreamErr: errors.New("503")}

	r := newRouter(
		map[string][]router.Deployment{
			"a": {{Provider: a, Model: "x", Weight: 1}},
			"b": {{Provider: b, Model: "y", Weight: 1}},
		},
		map[string][]string{"a": {"b"}, "b": {"a"}},
	)

	// Must terminate (seen-cycle guard) rather than recurse forever.
	_, _, err := r.Stream(context.Background(), agentmodel.ChatRequest{Model: "a", Stream: true})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// ─── MessagesPassthrough: non-retryable provider error returns err ────────

func TestMessagesPassthrough_NonRetryableProviderErrorReturns(t *testing.T) {
	// Provider returns a non-retryable (auth) error → router returns it
	// immediately without trying the other deployment.
	authErr := errors.New("401 invalid_api_key")
	failer := &stub.Stub{
		MessagesPassthroughFn: func(context.Context, []byte, string, string) (*http.Response, error) {
			return nil, authErr
		},
	}
	other := &stub.Stub{MessagesPassthroughFn: passthroughFn(200, `{"id":"never"}`)}

	r := newRouter(map[string][]router.Deployment{
		"m": {
			{Name: "fail/m", Provider: failer, Model: "m-fail", Weight: 1000},
			{Name: "ok/m", Provider: other, Model: "m-ok", Weight: 1},
		},
	}, nil)

	_, _, _, err := r.MessagesPassthrough(context.Background(), []byte(`{}`), "m", "")
	if err == nil {
		t.Fatal("expected non-retryable error, got nil")
	}
	if !errors.Is(err, authErr) {
		t.Errorf("expected the raw provider error to propagate, got %v", err)
	}
}

// ─── MessagesPassthrough: nil resp skipped, then 429-only → ErrTypeRateLimit ─

func TestMessagesPassthrough_NilRespThenRateLimitAggregate(t *testing.T) {
	// First deployment returns (nil, nil) → skipped (resp == nil continue).
	// Second deployment 429s on every try → no success, hitRateLimit set →
	// aggregate ErrTypeRateLimit is returned.
	nilProvider := &stub.Stub{
		MessagesPassthroughFn: func(context.Context, []byte, string, string) (*http.Response, error) {
			return nil, nil
		},
	}
	rl := &stub.Stub{MessagesPassthroughFn: passthroughFn(429, `{}`)}

	r := newRouter(map[string][]router.Deployment{
		"m": {
			{Name: "nil/m", Provider: nilProvider, Model: "m-nil", Weight: 1000},
			{Name: "rl/m", Provider: rl, Model: "m-rl", Weight: 1},
		},
	}, nil)

	_, _, _, err := r.MessagesPassthrough(context.Background(), []byte(`{}`), "m", "")
	if err == nil {
		t.Fatal("expected rate-limit error, got nil")
	}
	var ae *agentmodel.Error
	if !errors.As(err, &ae) {
		t.Fatalf("error is not *agentmodel.Error: %v", err)
	}
	if ae.Type != agentmodel.ErrTypeRateLimit {
		t.Errorf("Type: got %q, want rate_limit_error", ae.Type)
	}
}

// ─── MessagesPassthrough: fallback chain exhausts to rate-limit error ─────

func TestMessagesPassthrough_FallbackChainExhaustsToRateLimit(t *testing.T) {
	// Primary 429s (cools + hitRateLimit), then fallback model also 429s.
	// The fallback recursion returns ErrTypeRateLimit, which is retryable, so
	// the outer loop sets hitRateLimit and the final return is ErrTypeRateLimit.
	rlPrimary := &stub.Stub{MessagesPassthroughFn: passthroughFn(429, `{}`)}
	rlFallback := &stub.Stub{MessagesPassthroughFn: passthroughFn(429, `{}`)}

	r := newRouter(
		map[string][]router.Deployment{
			"primary":  {{Name: "sub/primary", Provider: rlPrimary, Model: "primary-upstream", Weight: 1}},
			"fallback": {{Name: "api/fallback", Provider: rlFallback, Model: "fallback-upstream", Weight: 1}},
		},
		map[string][]string{"primary": {"fallback"}},
	)

	_, _, _, err := r.MessagesPassthrough(context.Background(), []byte(`{}`), "primary", "")
	if err == nil {
		t.Fatal("expected rate-limit error, got nil")
	}
	var ae *agentmodel.Error
	if !errors.As(err, &ae) {
		t.Fatalf("error is not *agentmodel.Error: %v", err)
	}
	if ae.Type != agentmodel.ErrTypeRateLimit {
		t.Errorf("Type: got %q, want rate_limit_error", ae.Type)
	}
}

// ─── MessagesPassthrough: fallback non-retryable terminal error ───────────

func TestMessagesPassthrough_FallbackNonRetryableTerminates(t *testing.T) {
	// Primary 429s and falls back; the fallback deployment returns a
	// non-retryable provider error, which must propagate as the terminal error.
	rlPrimary := &stub.Stub{MessagesPassthroughFn: passthroughFn(429, `{}`)}
	authErr := errors.New("401 invalid_api_key")
	fbFail := &stub.Stub{
		MessagesPassthroughFn: func(context.Context, []byte, string, string) (*http.Response, error) {
			return nil, authErr
		},
	}

	r := newRouter(
		map[string][]router.Deployment{
			"primary":  {{Name: "sub/primary", Provider: rlPrimary, Model: "primary-upstream", Weight: 1}},
			"fallback": {{Name: "api/fallback", Provider: fbFail, Model: "fallback-upstream", Weight: 1}},
		},
		map[string][]string{"primary": {"fallback"}},
	)

	_, _, _, err := r.MessagesPassthrough(context.Background(), []byte(`{}`), "primary", "")
	if err == nil {
		t.Fatal("expected non-retryable error, got nil")
	}
	var ae *agentmodel.Error
	if !errors.As(err, &ae) {
		t.Fatalf("error is not *agentmodel.Error: %v", err)
	}
	if ae.Type != agentmodel.ErrTypeAuthentication {
		t.Errorf("Type: got %q, want authentication_error", ae.Type)
	}
}

// ─── MessagesPassthrough: cycle guard returns ErrAllFailed ────────────────

func TestMessagesPassthrough_FallbackCycleTerminates(t *testing.T) {
	rlA := &stub.Stub{MessagesPassthroughFn: passthroughFn(429, `{}`)}
	rlB := &stub.Stub{MessagesPassthroughFn: passthroughFn(429, `{}`)}

	r := newRouter(
		map[string][]router.Deployment{
			"a": {{Name: "a/dep", Provider: rlA, Model: "a-upstream", Weight: 1}},
			"b": {{Name: "b/dep", Provider: rlB, Model: "b-upstream", Weight: 1}},
		},
		map[string][]string{"a": {"b"}, "b": {"a"}},
	)

	// Cycle a→b→a; the seen guard must break recursion. Because every leg
	// rate-limited, the aggregate result is a rate-limit error.
	_, _, _, err := r.MessagesPassthrough(context.Background(), []byte(`{}`), "a", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// ─── Embed: retryable error → next deployment wins ────────────────────────

func TestEmbed_RetryableFallsBackToNextDeployment(t *testing.T) {
	failer := &stub.Stub{NameValue: "primary", EmbedErr: errors.New("503 overloaded")}
	winner := &stub.Stub{NameValue: "secondary"}

	r := newRouter(map[string][]router.Deployment{
		"emb": {
			{Provider: failer, Model: "emb-a", Weight: 1000},
			{Provider: winner, Model: "emb-b", Weight: 1},
		},
	}, nil)

	resp, err := r.Embed(context.Background(), agentmodel.EmbeddingRequest{
		Model: "emb",
		Input: []string{"hi"},
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if resp.Model != "emb-b" {
		t.Errorf("Model: got %q, want emb-b (resolved from winning deployment)", resp.Model)
	}
	if failer.CallCount() != 1 || winner.CallCount() != 1 {
		t.Errorf("CallCount: failer=%d winner=%d, want 1+1", failer.CallCount(), winner.CallCount())
	}
}

// ─── Embed: non-retryable error halts immediately ────────────────────────

func TestEmbed_NonRetryableErrorHalts(t *testing.T) {
	failer := &stub.Stub{NameValue: "primary", EmbedErr: errors.New("401 invalid_api_key")}
	other := &stub.Stub{NameValue: "secondary"}

	r := newRouter(map[string][]router.Deployment{
		"emb": {
			{Provider: failer, Model: "emb-a", Weight: 1000},
			{Provider: other, Model: "emb-b", Weight: 1},
		},
	}, nil)

	_, err := r.Embed(context.Background(), agentmodel.EmbeddingRequest{
		Model: "emb",
		Input: []string{"hi"},
	})
	if err == nil {
		t.Fatal("expected auth error, got nil")
	}
	var ae *agentmodel.Error
	if !errors.As(err, &ae) {
		t.Fatalf("error is not *agentmodel.Error: %v", err)
	}
	if ae.Type != agentmodel.ErrTypeAuthentication {
		t.Errorf("Type: got %q, want authentication_error", ae.Type)
	}
	if other.CallCount() != 0 {
		t.Errorf("non-retryable should NOT try other deployments; other.CallCount=%d", other.CallCount())
	}
}

// ─── Embed: all deployments fail (retryable) → ErrAllFailed ──────────────

func TestEmbed_AllFailReturnsErrAllFailed(t *testing.T) {
	a := &stub.Stub{EmbedErr: errors.New("503")}
	b := &stub.Stub{EmbedErr: errors.New("503")}

	r := newRouter(map[string][]router.Deployment{
		"emb": {
			{Provider: a, Model: "emb-a", Weight: 1},
			{Provider: b, Model: "emb-b", Weight: 1},
		},
	}, nil)

	_, err := r.Embed(context.Background(), agentmodel.EmbeddingRequest{
		Model: "emb",
		Input: []string{"hi"},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ae *agentmodel.Error
	if !errors.As(err, &ae) {
		t.Fatalf("error is not *agentmodel.Error: %v", err)
	}
	if ae.Type != agentmodel.ErrTypeAllDeploymentsFail {
		t.Errorf("Type: got %q, want all_deployments_failed", ae.Type)
	}
	if a.CallCount() != 1 || b.CallCount() != 1 {
		t.Errorf("CallCount: a=%d b=%d, want 1+1 (both tried)", a.CallCount(), b.CallCount())
	}
}

func TestValidateDeployments(t *testing.T) {
	// openai offers gpt-4o but NOT the configured "gpt-4o-old" -> drift.
	openaiStub := &stub.Stub{NameValue: "openai", LiveModels: []string{"gpt-4o", "gpt-5.5"}}
	// anthropic's list fetch fails -> its deployment is skipped, no drift.
	anthropicStub := &stub.Stub{NameValue: "anthropic", ListModelsErr: errors.New("network down")}

	deps := map[string][]router.Deployment{
		"good":    {{Provider: openaiStub, Model: "gpt-4o", Weight: 100}},
		"drifted": {{Provider: openaiStub, Model: "gpt-4o-old", Weight: 100}},
		"offline": {{Provider: anthropicStub, Model: "claude-x", Weight: 100}},
	}
	r := newRouter(deps, nil)

	drift := r.ValidateDeployments(context.Background())
	if len(drift) != 1 {
		t.Fatalf("expected exactly 1 drift, got %d: %+v", len(drift), drift)
	}
	d := drift[0]
	if d.ModelName != "drifted" || d.Provider != "openai" || d.Model != "gpt-4o-old" {
		t.Errorf("unexpected drift: %+v", d)
	}
}

func TestValidateDeployments_SkipsNonListers(t *testing.T) {
	// A provider that does not implement provider.ModelLister is skipped
	// entirely (no drift, no panic).
	deps := map[string][]router.Deployment{
		"m": {{Provider: nonLister{}, Model: "whatever", Weight: 100}},
	}
	r := newRouter(deps, nil)
	if drift := r.ValidateDeployments(context.Background()); len(drift) != 0 {
		t.Errorf("non-lister provider should yield no drift, got %+v", drift)
	}
}

// ─── Blocked fallback targets (virtual-key model allowlist, ) ───────────

// A blocked model must be skipped by the fallback walk: the restricted key's
// request never reaches it, even though it would have served the request.
func TestCompleteBlocked_SkipsBlockedFallback(t *testing.T) {
	primaryFail := &stub.Stub{CompleteErr: errors.New("503 overloaded")}
	fallbackOK := &stub.Stub{NameValue: "fallback"}

	r := newRouter(
		map[string][]router.Deployment{
			"gpt-4":    {{Provider: primaryFail, Model: "gpt-4o", Weight: 1}},
			"claude-3": {{Provider: fallbackOK, Model: "claude", Weight: 1}},
		},
		map[string][]string{"gpt-4": {"claude-3"}},
	)

	// Unblocked control: fallback serves.
	resp, _, err := r.CompleteBlocked(context.Background(), agentmodel.ChatRequest{Model: "gpt-4"}, nil)
	if err != nil || resp.Model != "claude" {
		t.Fatalf("control: got (%q, %v), want (claude, nil)", resp.Model, err)
	}

	// Blocked: the fallback is invisible; exhaustion surfaces as an error,
	// and the blocked provider is never called again.
	before := fallbackOK.CallCount()
	_, _, err = r.CompleteBlocked(context.Background(), agentmodel.ChatRequest{Model: "gpt-4"}, []string{"claude-3"})
	if err == nil {
		t.Fatal("blocked fallback must not serve the request")
	}
	if fallbackOK.CallCount() != before {
		t.Errorf("blocked fallback provider was called %d extra times", fallbackOK.CallCount()-before)
	}
}

func TestStreamBlocked_SkipsBlockedFallback(t *testing.T) {
	primaryFail := &stub.Stub{StreamErr: errors.New("503 overloaded")}
	fallbackOK := &stub.Stub{NameValue: "fallback"}

	r := newRouter(
		map[string][]router.Deployment{
			"gpt-4":    {{Provider: primaryFail, Model: "gpt-4o", Weight: 1}},
			"claude-3": {{Provider: fallbackOK, Model: "claude", Weight: 1}},
		},
		map[string][]string{"gpt-4": {"claude-3"}},
	)

	if _, _, err := r.StreamBlocked(context.Background(), agentmodel.ChatRequest{Model: "gpt-4"}, nil); err != nil {
		t.Fatalf("control stream via fallback: %v", err)
	}
	if _, _, err := r.StreamBlocked(context.Background(), agentmodel.ChatRequest{Model: "gpt-4"}, []string{"claude-3"}); err == nil {
		t.Fatal("blocked fallback must not serve the stream")
	}
}

// The blocked seed must bound the passthrough walk too — this is the /v1/messages
// half of the allowlist-bounds-fallbacks contract. A regression here is
// a silent allowlist bypass on /v1/messages.
func TestMessagesPassthroughBlocked_SkipsBlockedFallback(t *testing.T) {
	failing := &stub.Stub{MessagesPassthroughFn: passthroughFn(429, `{}`)}
	fb := &stub.Stub{MessagesPassthroughFn: passthroughFn(200, `{"id":"fb"}`)}
	mk := func() *router.Router {
		return newRouter(
			map[string][]router.Deployment{
				"primary":  {{Name: "sub/primary", Provider: failing, Model: "primary-upstream", Weight: 1}},
				"fallback": {{Name: "api/fallback", Provider: fb, Model: "fallback-upstream", Weight: 1}},
			},
			map[string][]string{"primary": {"fallback"}},
		)
	}

	// Control: unblocked walk reaches the fallback.
	resp, dep, _, err := mk().MessagesPassthroughBlocked(context.Background(), []byte(`{}`), "primary", "", nil)
	if err != nil {
		t.Fatalf("control: %v", err)
	}
	resp.Body.Close()
	if dep.Model != "fallback-upstream" {
		t.Fatalf("control dep: got %q, want fallback-upstream", dep.Model)
	}

	// Blocked: the fallback must be unreachable.
	before := fb.CallCount()
	_, _, _, err = mk().MessagesPassthroughBlocked(context.Background(), []byte(`{}`), "primary", "", []string{"fallback"})
	if err == nil {
		t.Fatal("blocked fallback must not serve the passthrough request")
	}
	if fb.CallCount() != before {
		t.Errorf("blocked fallback provider was called")
	}
}

// ─── RPM/TPM pre-call enforcement ─────────────────────────────────────

// pinnedNow is a mid-minute instant; meterClock pins a router's rate-meter
// clock there so RPM/TPM tests cannot flake across a real minute boundary.
var pinnedNow = time.Date(2026, 6, 11, 10, 0, 30, 0, time.UTC)

func newMeteredRouter(deps map[string][]router.Deployment, fb map[string][]string) *router.Router {
	return router.NewWithRand(deps, fb, rand.NewSource(42), router.WithNow(func() time.Time { return pinnedNow }))
}

// A deployment at its RPM cap is skipped without an upstream call; with no
// alternative, exhaustion surfaces as 429 so the caller backs off until the
// minute rolls.
func TestComplete_RPMCapBlocksWithin429(t *testing.T) {
	p := &stub.Stub{NameValue: "openai"}
	r := newMeteredRouter(map[string][]router.Deployment{
		"m": {{Name: "d", Provider: p, Model: "m-up", Weight: 1, RPM: intPtr(1)}},
	}, nil)

	if _, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "m"}); err != nil {
		t.Fatalf("first request: %v", err)
	}
	calls := p.CallCount()

	_, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "m"})
	if err == nil {
		t.Fatal("second request within the minute must be rejected")
	}
	ae := agentmodel.Wrap(err)
	if ae.Type != agentmodel.ErrTypeRateLimit {
		t.Errorf("error type: got %s, want rate_limit_error", ae.Type)
	}
	if p.CallCount() != calls {
		t.Errorf("capped deployment received an upstream call")
	}
}

// A capped-out deployment fails over to an uncapped sibling — the cap sheds
// load sideways before it rejects.
func TestComplete_RPMCapFailsOverToSibling(t *testing.T) {
	capped := &stub.Stub{NameValue: "openai"}
	free := &stub.Stub{NameValue: "anthropic"}
	r := newMeteredRouter(map[string][]router.Deployment{
		"m": {
			{Name: "capped", Provider: capped, Model: "m-cap", Weight: 1000, RPM: intPtr(1)},
			{Name: "free", Provider: free, Model: "m-free", Weight: 1},
		},
	}, nil)

	// Drive enough requests that the capped deployment is certainly exhausted;
	// every request must still succeed via the sibling.
	for i := 0; i < 5; i++ {
		if _, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "m"}); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if capped.CallCount() > 1 {
		t.Errorf("capped deployment called %d times, cap is 1", capped.CallCount())
	}
}

// TPM: observed usage from a completed request blocks the next one at the cap.
func TestComplete_TPMCapUsesObservedUsage(t *testing.T) {
	p := &stub.Stub{
		NameValue:    "openai",
		CompleteResp: agentmodel.ChatResponse{ID: "tpm-test", Usage: agentmodel.Usage{TotalTokens: 50}},
	}
	r := newMeteredRouter(map[string][]router.Deployment{
		"m": {{Name: "d", Provider: p, Model: "m-up", Weight: 1, TPM: intPtr(50)}},
	}, nil)

	if _, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "m"}); err != nil {
		t.Fatalf("first request: %v", err)
	}
	_, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "m"})
	if err == nil {
		t.Fatal("second request must be blocked: 50 of 50 tokens consumed")
	}
	if agentmodel.Wrap(err).Type != agentmodel.ErrTypeRateLimit {
		t.Errorf("error type: got %s, want rate_limit_error", agentmodel.Wrap(err).Type)
	}
}

// Streaming responses credit their final usage chunk to the TPM window.
func TestStream_RecordsUsageForTPM(t *testing.T) {
	usage := &agentmodel.Usage{TotalTokens: 80}
	p := &stub.Stub{
		NameValue:    "openai",
		StreamChunks: []provider.StreamChunk{{Delta: agentmodel.Message{Content: "hi"}}, {Usage: usage}},
	}
	r := newMeteredRouter(map[string][]router.Deployment{
		"m": {{Name: "d", Provider: p, Model: "m-up", Weight: 1, TPM: intPtr(100)}},
	}, nil)

	seq, _, err := r.Stream(context.Background(), agentmodel.ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	for range seq {
	} // drain — the meter wrapper records the usage chunk

	limits := r.DeploymentLimits(pinnedNow)
	if len(limits) != 1 || limits[0].UsedTokens != 80 {
		t.Fatalf("UsedTokens: got %+v, want 80", limits)
	}
	if limits[0].UsedRequests != 1 {
		t.Errorf("UsedRequests: got %d, want 1", limits[0].UsedRequests)
	}
}

// Embed: an all-capped model surfaces 429, not 500.
func TestEmbed_RPMCap429(t *testing.T) {
	p := &stub.Stub{NameValue: "openai", EmbedResp: agentmodel.EmbeddingResponse{}}
	r := newMeteredRouter(map[string][]router.Deployment{
		"e": {{Name: "d", Provider: p, Model: "e-up", Weight: 1, RPM: intPtr(1)}},
	}, nil)

	if _, err := r.Embed(context.Background(), agentmodel.EmbeddingRequest{Model: "e"}); err != nil {
		t.Fatalf("first embed: %v", err)
	}
	_, err := r.Embed(context.Background(), agentmodel.EmbeddingRequest{Model: "e"})
	if err == nil {
		t.Fatal("second embed must be rejected at cap")
	}
	if agentmodel.Wrap(err).Type != agentmodel.ErrTypeRateLimit {
		t.Errorf("error type: got %s, want rate_limit_error", agentmodel.Wrap(err).Type)
	}
}

// MessagesPassthrough: the RPM cap bounds the passthrough walk too.
func TestMessagesPassthrough_RPMCap429(t *testing.T) {
	p := &stub.Stub{MessagesPassthroughFn: stub.PassthroughFunc(200, `{"id":"r"}`)}
	r := newMeteredRouter(map[string][]router.Deployment{
		"m": {{Name: "d", Provider: p, Model: "m-up", Weight: 1, RPM: intPtr(1)}},
	}, nil)

	resp, _, _, err := r.MessagesPassthrough(context.Background(), []byte(`{}`), "m", "")
	if err != nil {
		t.Fatalf("first passthrough: %v", err)
	}
	resp.Body.Close()

	_, _, _, err = r.MessagesPassthrough(context.Background(), []byte(`{}`), "m", "")
	if err == nil {
		t.Fatal("second passthrough must be rejected at cap")
	}
	if agentmodel.Wrap(err).Type != agentmodel.ErrTypeRateLimit {
		t.Errorf("error type: got %s, want rate_limit_error", agentmodel.Wrap(err).Type)
	}
}

// RecordTokens (the /v1/messages usage path) feeds the same TPM window the
// pre-call check reads.
func TestRecordTokens_FeedsTPMEnforcement(t *testing.T) {
	p := &stub.Stub{MessagesPassthroughFn: stub.PassthroughFunc(200, `{"id":"r"}`)}
	r := newMeteredRouter(map[string][]router.Deployment{
		"m": {{Name: "d", Provider: p, Model: "m-up", Weight: 1, TPM: intPtr(10)}},
	}, nil)

	resp, dep, _, err := r.MessagesPassthrough(context.Background(), []byte(`{}`), "m", "")
	if err != nil {
		t.Fatalf("passthrough: %v", err)
	}
	resp.Body.Close()
	r.RecordTokens("m", dep.Name, 10)

	_, _, _, err = r.MessagesPassthrough(context.Background(), []byte(`{}`), "m", "")
	if err == nil {
		t.Fatal("next passthrough must be blocked: recorded usage reached the tpm cap")
	}
}

// Stream usage is a cumulative snapshot, not a delta: providers (Gemini) can
// attach running totals to many chunks. The meter must record the LAST
// snapshot once, never the sum of snapshots — summing would spuriously
// exhaust TPM windows.
func TestStream_CumulativeUsageChunksRecordedOnce(t *testing.T) {
	p := &stub.Stub{
		NameValue: "gemini",
		StreamChunks: []provider.StreamChunk{
			{Delta: agentmodel.Message{Content: "a"}, Usage: &agentmodel.Usage{TotalTokens: 30}},
			{Delta: agentmodel.Message{Content: "b"}, Usage: &agentmodel.Usage{TotalTokens: 80}},
		},
	}
	r := newMeteredRouter(map[string][]router.Deployment{
		"g": {{Name: "d", Provider: p, Model: "g-up", Weight: 1, TPM: intPtr(100)}},
	}, nil)

	seq, _, err := r.Stream(context.Background(), agentmodel.ChatRequest{Model: "g"})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	for range seq {
	}

	limits := r.DeploymentLimits(pinnedNow)
	if len(limits) != 1 || limits[0].UsedTokens != 80 {
		t.Fatalf("UsedTokens: got %+v, want 80 (last snapshot, not 110 summed)", limits)
	}
}

// A fallback-served passthrough meters under the SERVING model; RecordTokens
// keyed by the returned served model must land on the same window the walk
// admitted against.
func TestMessagesPassthrough_FallbackReturnsServedModelForMetering(t *testing.T) {
	failing := &stub.Stub{MessagesPassthroughFn: stub.PassthroughFunc(429, `{}`)}
	fb := &stub.Stub{MessagesPassthroughFn: stub.PassthroughFunc(200, `{"id":"fb"}`)}
	r := newMeteredRouter(
		map[string][]router.Deployment{
			"primary":  {{Name: "sub/p", Provider: failing, Model: "p-up", Weight: 1}},
			"fallback": {{Name: "api/f", Provider: fb, Model: "f-up", Weight: 1, TPM: intPtr(100)}},
		},
		map[string][]string{"primary": {"fallback"}},
	)

	resp, dep, served, err := r.MessagesPassthrough(context.Background(), []byte(`{}`), "primary", "")
	if err != nil {
		t.Fatalf("passthrough: %v", err)
	}
	resp.Body.Close()
	if served != "fallback" {
		t.Fatalf("served model: got %q, want fallback", served)
	}
	r.RecordTokens(served, dep.Name, 60)

	for _, dl := range r.DeploymentLimits(pinnedNow) {
		if dl.ModelName == "fallback" && dl.UsedTokens != 60 {
			t.Errorf("fallback UsedTokens: got %d, want 60", dl.UsedTokens)
		}
		if dl.ModelName == "primary" && dl.UsedTokens != 0 {
			t.Errorf("primary UsedTokens: got %d, want 0 (tokens belong to the serving model)", dl.UsedTokens)
		}
	}
}

// A terminal (non-retryable) passthrough failure must name the deployment that
// produced it. The walk knows which one failed; discarding it left the caller's
// audit row with an empty provider and the *requested* model id, so a
// non-streaming upstream failure was logged with strictly less attribution than
// a mid-stream one — same request, same deployment, different audit detail.
func TestMessagesPassthrough_TerminalErrorNamesFailingDeployment(t *testing.T) {
	// context_length_exceeded classifies as ErrTypeContextWindow: terminal, so
	// the walk stops here rather than cooling the deployment and moving on.
	terminal := errors.New(`chatgpt: stream error: {"error":{"code":"context_length_exceeded"}}`)
	failing := &stub.Stub{
		NameValue: "chatgpt",
		MessagesPassthroughFn: func(context.Context, []byte, string, string) (*http.Response, error) {
			return nil, terminal
		},
	}
	r := newRouter(map[string][]router.Deployment{
		"claude-sonnet-4-5": {{Name: "sub/codex", Provider: failing, Model: "gpt-5.5", Weight: 1}},
	}, nil)

	_, dep, served, err := r.MessagesPassthrough(context.Background(), []byte(`{}`), "claude-sonnet-4-5", "")
	if err == nil {
		t.Fatal("want a terminal error")
	}
	if dep.Provider == nil {
		t.Fatal("dep.Provider = nil; the failing deployment must be reported")
	}
	if dep.Provider.Name() != "chatgpt" {
		t.Errorf("dep.Provider.Name() = %q, want chatgpt", dep.Provider.Name())
	}
	if dep.Model != "gpt-5.5" {
		t.Errorf("dep.Model = %q, want gpt-5.5 (the upstream id, not the requested alias)", dep.Model)
	}
	if served != "claude-sonnet-4-5" {
		t.Errorf("served = %q, want the model the walk was admitted under", served)
	}
}

// A terminal failure reached through a fallback must name the fallback's
// deployment, not the primary's.
func TestMessagesPassthrough_TerminalErrorThroughFallbackNamesFallback(t *testing.T) {
	rateLimited := &stub.Stub{NameValue: "anthropic", MessagesPassthroughFn: passthroughFn(429, `{}`)}
	terminal := &stub.Stub{
		NameValue: "chatgpt",
		MessagesPassthroughFn: func(context.Context, []byte, string, string) (*http.Response, error) {
			return nil, errors.New(`chatgpt: 400 invalid_request_error`)
		},
	}
	r := newRouter(
		map[string][]router.Deployment{
			"primary":  {{Name: "api/primary", Provider: rateLimited, Model: "primary-upstream", Weight: 1}},
			"fallback": {{Name: "sub/fallback", Provider: terminal, Model: "fallback-upstream", Weight: 1}},
		},
		map[string][]string{"primary": {"fallback"}},
	)

	_, dep, _, err := r.MessagesPassthrough(context.Background(), []byte(`{}`), "primary", "")
	if err == nil {
		t.Fatal("want a terminal error")
	}
	if dep.Provider == nil || dep.Model != "fallback-upstream" {
		t.Errorf("dep = %+v, want the fallback deployment that actually failed", dep)
	}
}

// When there is no single deployment to blame — nothing configured, everything
// rate-limited, fallback cycle — the zero Deployment is the honest answer. The
// caller must not attribute the failure to an arbitrary provider.
func TestMessagesPassthrough_UnattributableErrorsReturnZeroDeployment(t *testing.T) {
	t.Run("no deployment", func(t *testing.T) {
		r := newRouter(map[string][]router.Deployment{}, nil)
		_, dep, _, err := r.MessagesPassthrough(context.Background(), []byte(`{}`), "missing", "")
		if err == nil {
			t.Fatal("want an error")
		}
		if dep.Provider != nil {
			t.Errorf("dep.Provider = %v, want nil (no deployment to attribute)", dep.Provider)
		}
	})

	t.Run("all rate limited", func(t *testing.T) {
		limited := &stub.Stub{NameValue: "anthropic", MessagesPassthroughFn: passthroughFn(429, `{}`)}
		r := newRouter(map[string][]router.Deployment{
			"primary": {{Name: "api/primary", Provider: limited, Model: "primary-upstream", Weight: 1}},
		}, nil)
		_, dep, _, err := r.MessagesPassthrough(context.Background(), []byte(`{}`), "primary", "")
		if err == nil {
			t.Fatal("want an error")
		}
		if dep.Provider != nil {
			t.Errorf("dep.Provider = %v, want nil (exhaustion is not one deployment's fault)", dep.Provider)
		}
	})
}

// TestMessagesPassthrough_NoCapableDeploymentIsTerminal: a model whose only
// deployment is chat-only (no PassthroughProvider) must yield a terminal
// invalid_request, not a 500 ErrAllFailed the client retries — mirroring
// GenerateImage's capability-miss handling.
func TestMessagesPassthrough_NoCapableDeploymentIsTerminal(t *testing.T) {
	r := newRouter(map[string][]router.Deployment{
		"m": {{Name: "chat/m", Provider: chatOnlyProvider{}, Model: "m", Weight: 1}},
	}, nil)
	_, _, _, err := r.MessagesPassthrough(context.Background(), []byte(`{}`), "m", "")
	if err == nil {
		t.Fatal("expected a terminal error when no deployment implements passthrough")
	}
	ae := agentmodel.Wrap(err)
	if ae.Type != agentmodel.ErrTypeInvalidRequest {
		t.Errorf("Type = %q, want %q (capability miss is terminal)", ae.Type, agentmodel.ErrTypeInvalidRequest)
	}
	if ae.Retryable() {
		t.Error("capability miss must not be retryable")
	}
}

// TestCompleteBlocked_TerminalFailureAttributesDeployment guards : a
// terminal (non-retryable) failure must return the failing deployment's
// attribution via StreamMeta, so /v1/chat/completions audit rows carry
// provider/auth_mode instead of "" (the /v1/messages path already did).
func TestCompleteBlocked_TerminalFailureAttributesDeployment(t *testing.T) {
	// "401 unauthorized" -> authentication_error, which is TERMINAL. NOT
	// stub.ErrTestAuth: "stub: auth failed" misses Wrap's substrings and is
	// classified retryable upstream_error, which exhausts the walk instead.
	s := &stub.Stub{NameValue: "openai", AuthModeValue: agentmodel.AuthModeAPIKey, CompleteErr: errors.New("401 unauthorized")}
	r := newRouter(map[string][]router.Deployment{
		"m": {{Name: "openai/m", Provider: s, Model: "m-up", Weight: 1}},
	}, nil)
	_, meta, err := r.CompleteBlocked(context.Background(), agentmodel.ChatRequest{Model: "m"}, nil)
	if err == nil {
		t.Fatal("expected a terminal error")
	}
	if meta.Provider != "openai" || meta.AuthMode != agentmodel.AuthModeAPIKey {
		t.Errorf("meta = %+v, want Provider=openai AuthMode=api_key (a failed request must still attribute its deployment)", meta)
	}
}

// TestStreamBlocked_TerminalFailureAttributesDeployment: same for streaming.
func TestStreamBlocked_TerminalFailureAttributesDeployment(t *testing.T) {
	s := &stub.Stub{NameValue: "openai", AuthModeValue: agentmodel.AuthModeAPIKey, StreamErr: errors.New("401 unauthorized")}
	r := newRouter(map[string][]router.Deployment{
		"m": {{Name: "openai/m", Provider: s, Model: "m-up", Weight: 1}},
	}, nil)
	_, meta, err := r.StreamBlocked(context.Background(), agentmodel.ChatRequest{Model: "m"}, nil)
	if err == nil {
		t.Fatal("expected a terminal error")
	}
	if meta.Provider != "openai" {
		t.Errorf("meta.Provider = %q, want openai", meta.Provider)
	}
}

// ─── Retry-After sized deployment cooldowns ───────────────────────────────

func TestComplete_RetryAfterHintSizesDeploymentCooldown(t *testing.T) {
	// router_cooldown is a guess; the upstream's hint is fact. A deployment
	// told to come back in 30m must not be probed at the 5m default.
	rl := &stub.Stub{NameValue: "subscription", CompleteErr: stub.RateLimitAfter(30 * time.Minute)}
	clk := stub.NewClock()
	r := router.NewWithRand(map[string][]router.Deployment{
		"sonnet": {{Name: "sub/sonnet", Provider: rl, Model: "sonnet-sub", Weight: 1}},
	}, nil, rand.NewSource(42), router.WithNow(clk.Now))

	if _, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "sonnet"}); err == nil {
		t.Fatal("first call: expected the rate-limit error to surface")
	}
	afterFirst := rl.CallCount()

	// Past the 5m default, inside the hint: still parked.
	clk.Add(10 * time.Minute)
	if _, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "sonnet"}); err == nil {
		t.Fatal("second call: expected rate-limit exhaustion")
	}
	if rl.CallCount() != afterFirst {
		t.Errorf("deployment retried after 10m, want it parked for the full 30m hint")
	}

	// Past the hint: eligible again.
	clk.Add(21 * time.Minute)
	if _, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "sonnet"}); err == nil {
		t.Fatal("third call: expected rate-limit exhaustion")
	}
	if rl.CallCount() != afterFirst+1 {
		t.Errorf("calls=%d, want %d (hint expired, deployment returns)", rl.CallCount(), afterFirst+1)
	}
}

func TestComplete_NoHintUsesConfiguredCooldown(t *testing.T) {
	rl := &stub.Stub{NameValue: "subscription", CompleteErr: stub.ErrTestRateLimit}
	clk := stub.NewClock()
	r := router.NewWithRand(map[string][]router.Deployment{
		"sonnet": {{Name: "sub/sonnet", Provider: rl, Model: "sonnet-sub", Weight: 1}},
	}, nil, rand.NewSource(42), router.WithNow(clk.Now), router.WithCooldown(time.Minute))

	if _, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "sonnet"}); err == nil {
		t.Fatal("first call: expected the rate-limit error to surface")
	}
	afterFirst := rl.CallCount()

	clk.Add(30 * time.Second)
	if _, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "sonnet"}); err == nil {
		t.Fatal("second call: expected rate-limit exhaustion")
	}
	if rl.CallCount() != afterFirst {
		t.Errorf("deployment retried inside the configured 1m cooldown")
	}

	clk.Add(31 * time.Second)
	if _, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "sonnet"}); err == nil {
		t.Fatal("third call: expected rate-limit exhaustion")
	}
	if rl.CallCount() != afterFirst+1 {
		t.Errorf("calls=%d, want %d (configured cooldown expired)", rl.CallCount(), afterFirst+1)
	}
}

// The 429 the gateway hands back to its own clients must carry the soonest
// moment the model_name becomes usable again — otherwise every caller is left
// guessing exactly the way the pool used to.
func TestComplete_ExhaustionCarriesSoonestRetryAfter(t *testing.T) {
	slow := &stub.Stub{NameValue: "acct-a", CompleteErr: stub.RateLimitAfter(30 * time.Minute)}
	soon := &stub.Stub{NameValue: "acct-b", CompleteErr: stub.RateLimitAfter(10 * time.Minute)}
	clk := stub.NewClock()
	r := router.NewWithRand(map[string][]router.Deployment{
		"sonnet": {
			{Name: "a/sonnet", Provider: slow, Model: "sonnet", Weight: 1},
			{Name: "b/sonnet", Provider: soon, Model: "sonnet", Weight: 1},
		},
	}, nil, rand.NewSource(42), router.WithNow(clk.Now))

	_, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "sonnet"})
	if err == nil {
		t.Fatal("expected rate-limit exhaustion")
	}
	ae := agentmodel.Wrap(err)
	if ae.Type != agentmodel.ErrTypeRateLimit {
		t.Fatalf("Type=%q, want %q", ae.Type, agentmodel.ErrTypeRateLimit)
	}
	if ae.RetryAfter != 10*time.Minute {
		t.Errorf("RetryAfter=%v, want 10m (the soonest of the two)", ae.RetryAfter)
	}
}

// Once both deployments are parked, the walk sees no upstream errors at all —
// every candidate is skipped as cooled. The remaining cooldown is still the
// honest answer, so it has to come off the cooldown map.
func TestComplete_ExhaustionByCooldownReportsRemaining(t *testing.T) {
	slow := &stub.Stub{NameValue: "acct-a", CompleteErr: stub.RateLimitAfter(30 * time.Minute)}
	soon := &stub.Stub{NameValue: "acct-b", CompleteErr: stub.RateLimitAfter(10 * time.Minute)}
	clk := stub.NewClock()
	r := router.NewWithRand(map[string][]router.Deployment{
		"sonnet": {
			{Name: "a/sonnet", Provider: slow, Model: "sonnet", Weight: 1},
			{Name: "b/sonnet", Provider: soon, Model: "sonnet", Weight: 1},
		},
	}, nil, rand.NewSource(42), router.WithNow(clk.Now))

	if _, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "sonnet"}); err == nil {
		t.Fatal("first call: expected rate-limit exhaustion")
	}

	clk.Add(5 * time.Minute)
	_, err := r.Complete(context.Background(), agentmodel.ChatRequest{Model: "sonnet"})
	if err == nil {
		t.Fatal("second call: expected rate-limit exhaustion")
	}
	if got := agentmodel.Wrap(err).RetryAfter; got != 5*time.Minute {
		t.Errorf("RetryAfter=%v, want 5m (10m hint, 5m elapsed)", got)
	}
}
