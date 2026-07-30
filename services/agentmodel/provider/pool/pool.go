// Package pool provides a multi-credential provider that rotates across N
// underlying providers when one hits a rate limit. Per-(credential,model)
// cooldowns prevent a saturated account from blocking requests to other models
// on the same credential.
//
// Selection is sticky, not round-robin: traffic stays on the credential that
// last served until it rate-limits. See Pool.order for why.
package pool

import (
	"context"
	"fmt"
	"iter"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
)

// rateLimitCooldown is how long a credential is parked when the upstream gives
// no usable Retry-After. It is a guess, and a bad one for subscription quotas
// (Anthropic's 5-hour window, ChatGPT's weekly cap) — an upstream hint always
// wins over it, see cool.
const rateLimitCooldown = 5 * time.Minute

// Pool wraps N providers with identical capabilities. On a rate-limit response,
// the responsible (index, model) pair is cooled down and the next provider is
// tried. Non-rate-limit errors are returned immediately without cycling.
//
// Note: Pool implements the base provider.Provider interface directly (not by
// embedding it), so — unlike messagesbridge.Bridge — Go does not silently
// promote anything here; each optional capability interface (e.g.
// provider.PassthroughProvider, provider.ImageGenerator, provider.ModelLister)
// needs its own cycling method below. Add another one here when a wrapped
// provider gains a new optional capability (e.g. provider.VideoGenerator — and
// if VideoGenerator, also forward provider.VideoDownloader, or pooled video
// would generate but fail every /content fetch).
type Pool struct {
	providers []provider.Provider
	now       func() time.Time
	mu        sync.Mutex
	cooldowns map[string]time.Time // key: "idx:model"
	cur       int                  // index that last served; scans start here
}

// Option configures a Pool.
type Option func(*Pool)

// WithNow overrides the clock used for cooldown bookkeeping. Injectable for
// tests; production uses time.Now.
func WithNow(now func() time.Time) Option {
	return func(p *Pool) {
		if now != nil {
			p.now = now
		}
	}
}

// New creates a Pool. Panics if providers is empty. All providers MUST be
// homogeneous — same Name(), AuthMode(), and SupportedModels() — because Pool
// reports each of those by delegating to providers[0]. The factory is the only
// producer and builds every pool from a single provider name, upholding this.
func New(providers []provider.Provider, opts ...Option) *Pool {
	if len(providers) == 0 {
		panic("pool: providers slice must be non-empty")
	}
	p := &Pool{
		providers: providers,
		now:       time.Now,
		cooldowns: make(map[string]time.Time),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Compile-time checks: Pool forwards these optional capabilities (see
// GenerateImage / ListModels below).
var (
	_ provider.ImageGenerator = (*Pool)(nil)
	_ provider.ModelLister    = (*Pool)(nil)
)

// Name delegates to the first underlying provider. A Pool wraps N credentials
// of the SAME provider (factory builds it from one provider's oauth_token_dirs
// or api_key_envs), so providers[0].Name() is representative. It must NOT report
// a literal "pool": the router copies Name() into Usage.Provider and the cost
// registry is keyed "<provider>/<model>", so a "pool" name would miss the
// catalog and price every pooled deployment as unpriced ($0), silently bypassing
// its budget cap (#1486).
func (p *Pool) Name() string              { return p.providers[0].Name() }
func (p *Pool) AuthMode() string          { return p.providers[0].AuthMode() }
func (p *Pool) SupportedModels() []string { return p.providers[0].SupportedModels() }

// Complete cycles through non-cooled providers until one succeeds.
func (p *Pool) Complete(ctx context.Context, req agentmodel.ChatRequest) (agentmodel.ChatResponse, error) {
	for _, i := range p.order() {
		if p.isCooled(i, req.Model) {
			continue
		}
		resp, err := p.providers[i].Complete(ctx, req)
		if err == nil {
			p.selected(i)
			return resp, nil
		}
		ae := agentmodel.Wrap(err)
		if ae.Type == agentmodel.ErrTypeRateLimit {
			p.cool(i, req.Model, ae.RetryAfter)
			continue
		}
		return agentmodel.ChatResponse{}, err
	}
	return agentmodel.ChatResponse{}, p.errAllCooled(req.Model)
}

// Stream cycles through non-cooled providers until one successfully opens a stream.
func (p *Pool) Stream(ctx context.Context, req agentmodel.ChatRequest) (iter.Seq2[provider.StreamChunk, error], error) {
	for _, i := range p.order() {
		if p.isCooled(i, req.Model) {
			continue
		}
		seq, err := p.providers[i].Stream(ctx, req)
		if err == nil {
			p.selected(i)
			return seq, nil
		}
		ae := agentmodel.Wrap(err)
		if ae.Type == agentmodel.ErrTypeRateLimit {
			p.cool(i, req.Model, ae.RetryAfter)
			continue
		}
		return nil, err
	}
	return nil, p.errAllCooled(req.Model)
}

// Embed cycles through non-cooled providers until one succeeds.
func (p *Pool) Embed(ctx context.Context, req agentmodel.EmbeddingRequest) (agentmodel.EmbeddingResponse, error) {
	for _, i := range p.order() {
		if p.isCooled(i, req.Model) {
			continue
		}
		resp, err := p.providers[i].Embed(ctx, req)
		if err == nil {
			p.selected(i)
			return resp, nil
		}
		ae := agentmodel.Wrap(err)
		if ae.Type == agentmodel.ErrTypeRateLimit {
			p.cool(i, req.Model, ae.RetryAfter)
			continue
		}
		return agentmodel.EmbeddingResponse{}, err
	}
	return agentmodel.EmbeddingResponse{}, p.errAllCooled(req.Model)
}

// MessagesPassthrough implements provider.PassthroughProvider by cycling through
// inner providers that support passthrough. A 429 HTTP status returned by the
// upstream is treated as a rate-limit and cools that credential.
func (p *Pool) MessagesPassthrough(ctx context.Context, body []byte, modelOverride, clientBetas string) (*http.Response, error) {
	anyCapable := false
	for _, i := range p.order() {
		pp, ok := p.providers[i].(provider.PassthroughProvider)
		if !ok {
			continue
		}
		anyCapable = true // capability is independent of cooldown
		if p.isCooled(i, modelOverride) {
			continue
		}
		resp, err := pp.MessagesPassthrough(ctx, body, modelOverride, clientBetas)
		if err != nil {
			ae := agentmodel.Wrap(err)
			if ae.Type == agentmodel.ErrTypeRateLimit {
				p.cool(i, modelOverride, ae.RetryAfter)
				continue
			}
			return nil, err
		}
		// A passthrough provider may return (nil, nil) to signal "not available"
		// (e.g. it does not serve this model). Skip it rather than dereference a
		// nil *http.Response — mirrors the router's guard at router.go.
		if resp == nil {
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			// This path never sees a Go error, so the hint has to come off the
			// live response before the body is closed.
			retryAfter := agentmodel.RetryAfterFromHeader(resp.Header, p.now())
			_ = resp.Body.Close()
			p.cool(i, modelOverride, retryAfter)
			continue
		}
		p.selected(i)
		return resp, nil
	}
	if !anyCapable {
		// No inner provider implements passthrough: a terminal capability miss,
		// not a transient rate limit. ErrNotSupported maps to a terminal 4xx so
		// the router doesn't retry a request that can never succeed.
		return nil, provider.ErrNotSupported
	}
	return nil, p.errAllCooled(modelOverride)
}

// GenerateImage implements provider.ImageGenerator by cycling through inner
// providers that support image generation. A rate-limit error cools that
// credential and moves to the next; any other error returns immediately.
func (p *Pool) GenerateImage(ctx context.Context, req agentmodel.ImageRequest) (agentmodel.ImageResponse, error) {
	anyCapable := false
	for _, i := range p.order() {
		gen, ok := p.providers[i].(provider.ImageGenerator)
		if !ok {
			continue
		}
		anyCapable = true // capability is independent of cooldown
		if p.isCooled(i, req.Model) {
			continue
		}
		resp, err := gen.GenerateImage(ctx, req)
		if err == nil {
			p.selected(i)
			return resp, nil
		}
		ae := agentmodel.Wrap(err)
		if ae.Type == agentmodel.ErrTypeRateLimit {
			p.cool(i, req.Model, ae.RetryAfter)
			continue
		}
		return agentmodel.ImageResponse{}, err
	}
	if !anyCapable {
		// No inner provider implements image generation: a terminal capability
		// miss, not a transient rate limit (see MessagesPassthrough).
		return agentmodel.ImageResponse{}, provider.ErrNotSupported
	}
	return agentmodel.ImageResponse{}, p.errAllCooled(req.Model)
}

// ListModels implements provider.ModelLister by returning the live catalog of
// the first credential that answers. Without this forward, a pooled deployment
// is invisible to the router's drift check and /v1/models discovery — both
// type-assert provider.ModelLister and silently skip anything that fails it, so
// pooling a provider would quietly disable its model validation.
//
// Two deliberate differences from the inference paths above:
//
//   - Cooldowns are not consulted or set. This is a read-only metadata call with
//     no model to key a per-(credential, model) cooldown on, and parking a
//     credential over a catalog fetch would take it out of serving rotation.
//   - ANY error moves to the next credential, not just a rate limit. Discovery
//     is best-effort: one logged-out account must not blank out drift detection
//     for the whole deployment. The last error is returned only if every
//     credential fails, and the router already falls back to its last-good list.
//   - It reads the serving order but does not update it: starting at the
//     credential known to be working avoids re-probing a dead one on every
//     revalidation pass, while a metadata fetch has no business moving which
//     credential serves traffic.
func (p *Pool) ListModels(ctx context.Context) ([]string, error) {
	var lastErr error
	anyCapable := false
	for _, i := range p.order() {
		lister, ok := p.providers[i].(provider.ModelLister)
		if !ok {
			continue
		}
		anyCapable = true
		models, err := lister.ListModels(ctx)
		if err == nil {
			return models, nil
		}
		lastErr = err
	}
	if !anyCapable {
		// No inner provider can list: a terminal capability miss, mirroring
		// MessagesPassthrough/GenerateImage.
		return nil, provider.ErrNotSupported
	}
	return nil, lastErr
}

// order returns the indices to try, starting at the credential that last served
// and wrapping around.
//
// Sticky rather than round-robin is a cache decision, not a fairness one:
// prompt caches are per-account and never shared across credentials, so every
// move costs one full prompt re-read on the new account. Staying put until the
// current credential actually rate-limits keeps that to one miss per rotation
// instead of one per request. Wrapping keeps capacity whole — a credential that
// recovers is picked up as soon as the one in use runs out, and a credential
// whose cooldown expires never pulls traffic off a warm one to do it.
//
// Affinity is pool-wide while cooldowns are per-(credential, model). That
// asymmetry is deliberate: a Pool is built per deployment and the router pins
// each request to that deployment's single upstream model, so one Pool serves
// one model. The cooldown key keeps the isolation anyway because it is free;
// giving affinity a model dimension would cost a map for a case the factory
// cannot produce.
func (p *Pool) order() []int {
	p.mu.Lock()
	start := p.cur
	p.mu.Unlock()
	idx := make([]int, len(p.providers))
	for k := range p.providers {
		idx[k] = (start + k) % len(p.providers)
	}
	return idx
}

// selected records the credential that just served, so the next request starts
// there and reuses its warm cache.
func (p *Pool) selected(i int) {
	p.mu.Lock()
	p.cur = i
	p.mu.Unlock()
}

// errAllCooled is the shared exhaustion error: every credential was either
// parked or rate-limited on this pass. It carries the soonest moment any of
// them is eligible again, so the router sizes its own deployment cooldown from
// that instead of its default, and the client-facing 429 gets a Retry-After.
// Dropping it here would rebuild, one layer up, exactly the re-probe loop the
// per-credential cooldowns exist to stop.
func (p *Pool) errAllCooled(model string) *agentmodel.Error {
	e := agentmodel.NewErrorCode(agentmodel.ErrTypeRateLimit, agentmodel.CodeRateLimitExceeded,
		fmt.Sprintf("all %d credentials rate-limited for model %s", len(p.providers), model))
	e.RetryAfter = p.soonestRetry(model)
	return e
}

// soonestRetry returns the shortest wait until some credential is eligible for
// model again, or 0 when none is parked (a capability miss, say, parks nobody).
func (p *Pool) soonestRetry(model string) time.Duration {
	now := p.now()
	var soonest time.Duration
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.providers {
		until, ok := p.cooldowns[cooldownKey(i, model)]
		if !ok {
			continue
		}
		if d := until.Sub(now); d > 0 && (soonest == 0 || d < soonest) {
			soonest = d
		}
	}
	return soonest
}

// cooldownKey identifies one (credential, model) pair in the cooldown map.
// Mirrors the router's depKey: plain concatenation, because this runs once per
// credential on every request.
func cooldownKey(idx int, model string) string {
	return strconv.Itoa(idx) + ":" + model
}

func (p *Pool) isCooled(idx int, model string) bool {
	key := cooldownKey(idx, model)
	p.mu.Lock()
	defer p.mu.Unlock()
	until, ok := p.cooldowns[key]
	if !ok {
		return false
	}
	if p.now().After(until) {
		delete(p.cooldowns, key)
		return false
	}
	return true
}

// cool parks the (credential, model) pair for retryAfter, or for
// rateLimitCooldown when the upstream gave no hint. An upstream that says when
// its window rolls is authoritative: guessing shorter burns a request per guess,
// guessing longer idles a credential that is already usable.
func (p *Pool) cool(idx int, model string, retryAfter time.Duration) {
	d := retryAfter
	if d <= 0 {
		d = rateLimitCooldown
	}
	key := cooldownKey(idx, model)
	p.mu.Lock()
	p.cooldowns[key] = p.now().Add(d)
	p.mu.Unlock()
}
