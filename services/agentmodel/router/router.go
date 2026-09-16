// Package router multiplexes a logical model name across multiple
// deployments (provider + provider-specific model id), applies weighted
// random selection, and falls back to other deployments / fallback model
// names on retryable errors.
package router

import (
	"context"
	"errors"
	"iter"
	"math"
	"math/rand"
	"net/http"
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
)

// Deployment binds a Provider to a specific model id with a weight that
// influences selection probability.
type Deployment struct {
	// Name is the deployment's IDENTITY, not just a log label: the cooldown map,
	// the RPM/TPM rate meter (depKey), /v1/limits, the telemetry `deployment`
	// label, and the replicate dedup all key off it. The factory makes it unique
	// per (provider, model, auth mode) as "provider/model|authmode" — do NOT drop
	// the "|authmode" suffix, or two credentials of one provider+model would share
	// a single cooldown / rate-limit bucket.
	Name     string
	Provider provider.Provider // the wired provider (already configured w/ auth)
	Model    string            // the bare model id this deployment dispatches to
	Weight   int               // relative weight for weighted selection (>= 0)
	// Configured rate caps. nil = uncapped. Enforced pre-call: a
	// deployment at its cap for the current UTC minute is skipped exactly
	// like a cooled deployment, and an all-capped exhaustion surfaces as
	// 429. Live consumption is reported via DeploymentLimits / GET /v1/limits.
	RPM *int // requests per minute
	TPM *int // tokens per minute (input + output)
}

// defaultRouterCooldown is the cooldown applied to a deployment after a
// retryable failure when none is configured. Override per-server via
// WithCooldown (wired from the `router_cooldown` config key).
const defaultRouterCooldown = 5 * time.Minute

// Observer receives deployment-level routing events for metrics/tracing. It is
// optional; a nil Observer (the default) disables the hooks entirely. Methods
// must be safe for concurrent use and must not block. Implemented structurally
// by *telemetry.Telemetry.
type Observer interface {
	// DeploymentResult reports the outcome of one deployment attempt.
	DeploymentResult(model, provider, deployment string, ok bool)
	// Cooldown reports that a deployment was cooled down after a retryable failure.
	Cooldown(model, provider, deployment string)
}

// Router is safe for concurrent use after construction.
type Router struct {
	deployments  map[string][]Deployment // model_name -> list of deployments
	fallbacks    map[string][]string     // model_name -> ordered fallback model names
	rand         *rand.Rand
	mu           sync.Mutex           // protects rand only
	coolMu       sync.Mutex           // protects depCooldowns
	depCooldowns map[string]time.Time // key: "logicalModel:depName"
	cooldown     time.Duration        // how long a deployment is parked after a retryable failure; <= 0 disables cooling
	meter        *rateMeter           // per-deployment RPM/TPM counters
	now          func() time.Time     // clock for the rate meter; injectable so tests can pin the minute window
	obs          Observer             // optional; nil disables routing-event hooks

	// Cross-pass TTL cache of each provider's live model list, shared by
	// ValidateDeployments and DiscoverModels. A pure read when the
	// cached entry is fresh; a singleflight-collapsed refresh otherwise, with
	// last-good-on-failure so a transient upstream blip never flaps drift.
	valMu      sync.Mutex                // protects modelCache
	modelCache map[string]modelListEntry // cacheKey -> last-known live model set
	modelTTL   time.Duration             // freshness window; <= 0 means never reuse across passes (one-shot semantics)
	modelSF    singleflight.Group        // collapses concurrent list fetches for the same provider key
}

// modelListEntry is one provider's cached live model set. ok distinguishes a
// real fetched set (reusable as last-good) from a never-succeeded key.
type modelListEntry struct {
	models    map[string]bool
	fetchedAt time.Time
	ok        bool
}

// SetObserver attaches an Observer for deployment-level routing events. Pass
// nil to detach. Intended to be called once at wiring time, before serving.
func (r *Router) SetObserver(obs Observer) { r.obs = obs }

// obsResult and obsCooldown are nil-safe shims so call sites stay terse.
func (r *Router) obsResult(model, depName, provider string, ok bool) {
	if r.obs != nil {
		r.obs.DeploymentResult(model, provider, depName, ok)
	}
}

func (r *Router) obsCooldown(model, depName, provider string) {
	// Guard on cooldown > 0 so a server with cooling disabled never emits a
	// Cooldown event for a deployment that was not actually parked.
	if r.obs != nil && r.cooldown > 0 {
		r.obs.Cooldown(model, provider, depName)
	}
}

// Option configures a Router at construction time.
type Option func(*Router)

// WithCooldown sets how long a deployment is parked after a retryable failure
// before it is eligible again. A non-positive duration disables cooling
// entirely — a failed deployment can be retried on the very next request.
// When unset, the router uses defaultRouterCooldown.
func WithCooldown(d time.Duration) Option {
	return func(r *Router) { r.cooldown = d }
}

// WithNow injects the clock used by the rate meter. Tests pin it
// mid-minute so RPM/TPM assertions cannot flake across a real minute
// boundary; production uses the default time.Now.
func WithNow(now func() time.Time) Option {
	return func(r *Router) {
		if now != nil {
			r.now = now
		}
	}
}

// WithModelCacheTTL sets how long a provider's fetched live model list is
// reused across validation passes before it is refreshed. It is the
// freshness window backing both ValidateDeployments and DiscoverModels: within
// a single pass each provider is always queried at most once, and across passes
// a cached list younger than the TTL is reused instead of re-querying. A
// non-positive duration (the default) disables cross-pass reuse, preserving the
// one-shot startup behavior exactly. Wired from the `revalidate_interval`
// config key so the cache window matches the re-validation period.
func WithModelCacheTTL(d time.Duration) Option {
	return func(r *Router) { r.modelTTL = d }
}

// New constructs a Router. Pass model_name -> deployments and optional
// fallbacks. The router does not validate that fallbacks resolve to other
// configured model names — unresolved fallbacks just produce ErrAllFailed.
func New(deployments map[string][]Deployment, fallbacks map[string][]string, opts ...Option) *Router {
	if fallbacks == nil {
		fallbacks = make(map[string][]string)
	}
	r := &Router{
		deployments:  deployments,
		fallbacks:    fallbacks,
		rand:         rand.New(rand.NewSource(time.Now().UnixNano())),
		depCooldowns: make(map[string]time.Time),
		cooldown:     defaultRouterCooldown,
		meter:        newRateMeter(),
		now:          time.Now,
		modelCache:   make(map[string]modelListEntry),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// NewWithRand constructs a Router with a deterministic source — used by tests.
func NewWithRand(deployments map[string][]Deployment, fallbacks map[string][]string, src rand.Source, opts ...Option) *Router {
	r := New(deployments, fallbacks, opts...)
	r.rand = rand.New(src)
	return r
}

// ConfiguredModel describes one routable logical model for introspection
// (e.g. GET /v1/models). Provider is the owning provider of the model's
// primary (first-listed) deployment.
type ConfiguredModel struct {
	Name     string
	Provider string
}

// Models returns the configured logical models — the actual routable surface,
// keyed by the model_name clients pass as "model" — sorted by name. This is
// the source of truth for /v1/models, distinct from the cost price catalog.
func (r *Router) Models() []ConfiguredModel {
	out := make([]ConfiguredModel, 0, len(r.deployments))
	for name, deps := range r.deployments {
		var prov string
		if len(deps) > 0 && deps[0].Provider != nil {
			prov = deps[0].Provider.Name()
		}
		out = append(out, ConfiguredModel{Name: name, Provider: prov})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// DeploymentLimit describes one deployment's configured rate caps, its live
// current-minute consumption, and its cooldown posture, for GET /v1/limits
// introspection. CooledUntil is non-nil when the deployment is presently
// parked in a rate-limit cooldown (its expiry); callers can treat that as
// "limited right now". UsedRequests/UsedTokens are the rateMeter counters for
// the current UTC minute — the same numbers pre-call enforcement
// checks against RPM/TPM.
type DeploymentLimit struct {
	ModelName    string
	Deployment   string // deployment Name (provider/model label)
	Provider     string
	RPM          *int
	TPM          *int
	UsedRequests int
	UsedTokens   int
	CooledUntil  *time.Time
}

// DeploymentLimits returns the configured rate caps and live cooldown state for
// every deployment, sorted by (model_name, deployment). It is the source of
// truth for GET /v1/limits, mirroring Models() for /v1/models. Cooldowns that
// have already expired as of now are reported as not-cooled (and lazily
// reaped), matching isCooledDep's semantics.
func (r *Router) DeploymentLimits(now time.Time) []DeploymentLimit {
	out := make([]DeploymentLimit, 0)

	r.coolMu.Lock()
	for key, until := range r.depCooldowns {
		if !until.After(now) {
			delete(r.depCooldowns, key)
		}
	}
	// Snapshot the still-live cooldowns so we can release the lock before the
	// per-deployment loop.
	cooled := make(map[string]time.Time, len(r.depCooldowns))
	for key, until := range r.depCooldowns {
		cooled[key] = until
	}
	r.coolMu.Unlock()

	for modelName, deps := range r.deployments {
		for _, d := range deps {
			dl := DeploymentLimit{
				ModelName:  modelName,
				Deployment: d.Name,
				RPM:        d.RPM,
				TPM:        d.TPM,
			}
			dl.UsedRequests, dl.UsedTokens = r.meter.usage(depKey(modelName, d.Name), now)
			if d.Provider != nil {
				dl.Provider = d.Provider.Name()
			}
			if until, ok := cooled[depKey(modelName, d.Name)]; ok {
				u := until
				dl.CooledUntil = &u
			}
			out = append(out, dl)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ModelName != out[j].ModelName {
			return out[i].ModelName < out[j].ModelName
		}
		return out[i].Deployment < out[j].Deployment
	})
	return out
}

// Drift describes a configured deployment whose upstream model id is not in
// its provider's live model list — i.e. configuration that points at a model
// the provider no longer offers.
type Drift struct {
	ModelName string // logical model_name the deployment is configured under
	Provider  string // provider name
	Model     string // the deployment's upstream model id that wasn't found
}

// ValidateDeployments checks each configured deployment's upstream model id
// against the live model list reported by its provider, for providers that
// implement provider.ModelLister. It returns the deployments whose model id the
// provider no longer offers, sorted by (model_name, model).
//
// It is best-effort and never fatal: providers that don't implement
// ModelLister, and providers whose live-list fetch fails with no cached
// fallback, are skipped — the upstream APIs are a validation aid, not a source
// of truth. Each distinct provider is queried at most once per call; across
// calls a list younger than the configured cache TTL (WithModelCacheTTL) is
// reused, and a failed refresh degrades to the last-good list rather than
// re-flagging every deployment as drifted.
func (r *Router) ValidateDeployments(ctx context.Context) []Drift {
	now := r.now()
	// Per-pass dedup keyed by provider cache key. A stored nil set marks a key
	// we already attempted but can't validate against — so we resolve each
	// provider at most once per call even when its fetch errors.
	resolved := make(map[string]map[string]bool)

	var drift []Drift
	for modelName, deps := range r.deployments {
		for _, dep := range deps {
			lister, isLister := dep.Provider.(provider.ModelLister)
			if !isLister {
				continue
			}
			key := providerCacheKey(dep.Provider)
			set, seen := resolved[key]
			if !seen {
				set, _ = r.listProviderModels(ctx, key, lister, now)
				resolved[key] = set // nil when unresolvable → attempted, skip
			}
			if set != nil && !set[dep.Model] {
				drift = append(drift, Drift{ModelName: modelName, Provider: dep.Provider.Name(), Model: dep.Model})
			}
		}
	}

	sort.Slice(drift, func(i, j int) bool {
		if drift[i].ModelName != drift[j].ModelName {
			return drift[i].ModelName < drift[j].ModelName
		}
		return drift[i].Model < drift[j].Model
	})
	return drift
}

// AvailableModel describes one model a provider's upstream API currently
// offers, tagged with whether a configured deployment already routes to that
// (provider, model) pair. It is the unit of the opt-in discovery surface
// (GET /v1/models?available): unconfigured rows (Configured == false) are
// upstream-offered models the gateway has not wired up.
type AvailableModel struct {
	Provider   string // provider name (Provider.Name())
	Model      string // upstream model id
	Configured bool   // true when a deployment already routes to this (provider, model)
}

// DiscoverModels returns every model the configured providers' upstream APIs
// currently offer, tagged with whether a deployment already routes to it,
// sorted by (provider, model). It reuses the same TTL cache as
// ValidateDeployments, so a warm cache makes this a pure read. Providers that
// don't implement provider.ModelLister, and those whose fetch fails with no
// cached fallback, contribute nothing. Best-effort and never fatal — it never
// returns an error and never blocks the routable surface.
func (r *Router) DiscoverModels(ctx context.Context) []AvailableModel {
	now := r.now()

	// Configured (cacheKey, model) pairs + one representative provider per key.
	type cacheModel struct{ key, model string }
	configured := make(map[cacheModel]bool)
	type provRef struct {
		name   string
		lister provider.ModelLister
	}
	provs := make(map[string]provRef)
	for _, deps := range r.deployments {
		for _, dep := range deps {
			lister, isLister := dep.Provider.(provider.ModelLister)
			if !isLister {
				continue
			}
			key := providerCacheKey(dep.Provider)
			configured[cacheModel{key, dep.Model}] = true
			if _, seen := provs[key]; !seen {
				provs[key] = provRef{name: dep.Provider.Name(), lister: lister}
			}
		}
	}

	var out []AvailableModel
	for key, pr := range provs {
		set, ok := r.listProviderModels(ctx, key, pr.lister, now)
		if !ok {
			continue
		}
		for m := range set {
			out = append(out, AvailableModel{
				Provider:   pr.name,
				Model:      m,
				Configured: configured[cacheModel{key, m}],
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// providerCacheKey is the identity under which a provider's live model list is
// cached and deduped. It composes Name() and AuthMode() so that two
// deployments sharing a provider name but differing in credentials (e.g.
// "anthropic" api_key vs subscription) validate against their own entitlements
// instead of colliding on the bare name.
func providerCacheKey(p provider.Provider) string {
	return p.Name() + "|" + p.AuthMode()
}

// listProviderModels returns the live model set for a provider cache key,
// serving the cached entry when it is younger than the TTL and otherwise
// refreshing via the lister. Concurrent refreshes for the same key are
// collapsed (singleflight). On a refresh failure the last-good set is returned
// (stale) when one exists; only a key that has never succeeded returns
// (nil, false). now is the pass clock (r.now()), injectable for tests.
func (r *Router) listProviderModels(ctx context.Context, key string, lister provider.ModelLister, now time.Time) (map[string]bool, bool) {
	r.valMu.Lock()
	entry, cached := r.modelCache[key]
	if cached && entry.ok && r.modelTTL > 0 && now.Sub(entry.fetchedAt) < r.modelTTL {
		r.valMu.Unlock()
		return entry.models, true
	}
	r.valMu.Unlock()

	v, _, _ := r.modelSF.Do(key, func() (interface{}, error) {
		models, err := lister.ListModels(ctx)
		if err != nil {
			return nil, err // caller applies the last-good fallback below
		}
		set := make(map[string]bool, len(models))
		for _, m := range models {
			set[m] = true
		}
		r.valMu.Lock()
		r.modelCache[key] = modelListEntry{models: set, fetchedAt: now, ok: true}
		r.valMu.Unlock()
		return set, nil
	})
	if set, ok := v.(map[string]bool); ok && set != nil {
		return set, true
	}

	// Refresh failed: serve the last-good set if we ever fetched one.
	r.valMu.Lock()
	entry, cached = r.modelCache[key]
	r.valMu.Unlock()
	if cached && entry.ok {
		return entry.models, true
	}
	return nil, false
}

// isCooledDep reports whether a deployment is parked and, if so, how much of
// its cooldown is left. The remaining time is what a client-facing 429 reports
// as Retry-After once every candidate is parked: by then the walk sees no
// upstream errors to read a hint from, only skips.
func (r *Router) isCooledDep(logicalModel, depName string) (bool, time.Duration) {
	if depName == "" {
		return false, 0
	}
	r.coolMu.Lock()
	defer r.coolMu.Unlock()
	if len(r.depCooldowns) == 0 {
		return false, 0
	}
	key := depKey(logicalModel, depName)
	until, ok := r.depCooldowns[key]
	if !ok {
		return false, 0
	}
	now := r.now()
	if now.After(until) {
		delete(r.depCooldowns, key)
		return false, 0
	}
	return true, until.Sub(now)
}

// coolDep parks a deployment for retryAfter when the upstream said when to come
// back, and for the configured router_cooldown otherwise. The hint wins because
// the default is a guess: too short and every window costs a wasted request per
// retry, too long and a recovered deployment idles. WithCooldown(0) still
// disables cooling entirely — an explicit "never park" is not overridden by an
// upstream hint.
func (r *Router) coolDep(logicalModel, depName string, retryAfter time.Duration) {
	if depName == "" || r.cooldown <= 0 {
		return
	}
	d := retryAfter
	if d <= 0 {
		d = r.cooldown
	}
	key := depKey(logicalModel, depName)
	r.coolMu.Lock()
	r.depCooldowns[key] = r.now().Add(d)
	r.coolMu.Unlock()
}

// acquireRateSlot atomically admits or rejects one dispatch against the
// deployment's RPM/TPM caps for the current minute, counting the
// attempt when admitted. Checked in the same place as the cooldown so an
// over-limit deployment is skipped without an upstream call; callers treat
// the skip as rate-limited so an all-metered exhaustion surfaces as 429,
// matching the cooled-deployment semantics.
func (r *Router) acquireRateSlot(logicalModel string, dep Deployment) bool {
	return r.meter.tryAcquire(depKey(logicalModel, dep.Name), dep.RPM, dep.TPM, r.now())
}

// RecordTokens credits observed token usage (input + output) to a
// deployment's TPM window. The router records usage itself where it sees the
// response (Complete, Embed) or the stream (Stream wraps the iterator); the
// /v1/messages passthrough proxies opaque bytes, so its handler parses usage
// and reports it here, keyed by the served logical model the passthrough
// walk returned.
func (r *Router) RecordTokens(logicalModel, depName string, tokens int) {
	r.meter.recordTokens(depKey(logicalModel, depName), tokens, r.now())
}

// depKey is the shared identity for the cooldown map and the rate meter:
// one logical model's view of one deployment.
func depKey(logicalModel, depName string) string {
	return logicalModel + ":" + depName
}

// errAllRateLimited is the shared all-paths-rate-limited exhaustion (429),
// surfaced when every candidate was cooled, metered out, or upstream-429'd.
// Used by the walks that cannot say when the model becomes eligible again.
func errAllRateLimited(model string) *agentmodel.Error {
	return agentmodel.NewErrorCode(agentmodel.ErrTypeRateLimit,
		agentmodel.CodeRateLimitExceeded,
		"all deployments rate-limited for model "+model)
}

// errAllRateLimitedAfter is errAllRateLimited for the walks that track when the
// soonest candidate becomes eligible again. That duration reaches the client as
// the Retry-After header.
func errAllRateLimitedAfter(model string, retryAfter time.Duration) *agentmodel.Error {
	e := errAllRateLimited(model)
	e.RetryAfter = retryAfter
	return e
}

// retryTracker accumulates the soonest retry moment seen while walking a
// model_name's candidates: upstream hints on 429s, and the time left on
// already-parked deployments. The minimum is the right answer — the model_name
// is usable again as soon as ANY of its deployments is.
type retryTracker struct{ soonest time.Duration }

func (rt *retryTracker) note(d time.Duration) {
	if d <= 0 {
		return
	}
	if rt.soonest == 0 || d < rt.soonest {
		rt.soonest = d
	}
}

// ErrAllFailed indicates every deployment + every fallback model name failed.
var ErrAllFailed = agentmodel.NewError(
	agentmodel.ErrTypeAllDeploymentsFail,
	"all deployments and fallbacks exhausted",
)

// ErrNoDeployment indicates the requested model_name has no configured
// deployments (and no fallbacks).
var ErrNoDeployment = agentmodel.NewErrorCode(
	agentmodel.ErrTypeNotFound,
	agentmodel.CodeModelNotFound,
	"no deployment configured for model",
)

// Complete dispatches a chat completion to the first successful deployment.
//
// Algorithm:
//  1. Look up deployments for req.Model. If none, return ErrNoDeployment.
//  2. Order them by weighted shuffle (high-weight deployments more likely first).
//  3. Try each in order. On non-retryable error, return immediately.
//     On retryable error, try the next deployment.
//  4. After all deployments fail with retryable errors, recursively try each
//     configured fallback model_name.
//  5. If all paths exhausted, return ErrAllFailed.
func (r *Router) Complete(ctx context.Context, req agentmodel.ChatRequest) (agentmodel.ChatResponse, error) {
	resp, _, err := r.completeWithSeen(ctx, req, make(map[string]bool))
	return resp, err
}

// CompleteBlocked is Complete with the given logical model names removed from
// the fallback graph. Used by the virtual-key model allowlist: a
// restricted key's request must never be served by a fallback outside its
// allowlist — fallbacks shrink availability for a restricted key, they never
// widen access. Blocked models are seeded into the visited set, so the walk
// skips them exactly as it skips an already-tried model.
//
// Invariant: callers must reject a blocked ENTRY model (403) before calling —
// the api handlers do this via checkAccess. Passing req.Model itself in
// blocked hits the cycle guard and surfaces as a misleading 500 exhaustion,
// not a permission error. Relatedly, when a restricted key's only fallbacks
// are blocked, exhaustion takes whatever shape the allowed paths produce
// (429 if they rate-limited, 500 on /v1/messages if no allowed path could
// serve at all) — the error does not name the allowlist as the reason.
func (r *Router) CompleteBlocked(ctx context.Context, req agentmodel.ChatRequest, blocked []string) (agentmodel.ChatResponse, StreamMeta, error) {
	return r.completeWithSeen(ctx, req, seedSeen(blocked))
}

// StreamBlocked is Stream with blocked fallback targets; see CompleteBlocked.
func (r *Router) StreamBlocked(ctx context.Context, req agentmodel.ChatRequest, blocked []string) (iter.Seq2[provider.StreamChunk, error], StreamMeta, error) {
	return r.streamWithSeen(ctx, req, seedSeen(blocked))
}

// MessagesPassthroughBlocked is MessagesPassthrough with blocked fallback
// targets; see CompleteBlocked.
// The third return value is the logical model that served the request (the
// fallback target when one fired) — the key for RecordTokens.
func (r *Router) MessagesPassthroughBlocked(ctx context.Context, body []byte, modelName, clientBetas string, blocked []string) (*http.Response, Deployment, string, error) {
	return r.messagesPassthroughWithSeen(ctx, body, modelName, clientBetas, seedSeen(blocked))
}

func seedSeen(blocked []string) map[string]bool {
	seen := make(map[string]bool, len(blocked)+4)
	for _, m := range blocked {
		seen[m] = true
	}
	return seen
}

func (r *Router) completeWithSeen(ctx context.Context, req agentmodel.ChatRequest, seen map[string]bool) (agentmodel.ChatResponse, StreamMeta, error) {
	var lastErr error
	if seen[req.Model] {
		// Cycle in fallback graph; treat as exhausted.
		return agentmodel.ChatResponse{}, StreamMeta{}, ErrAllFailed
	}
	seen[req.Model] = true

	// hitRateLimit records that at least one candidate path was skipped or failed
	// for a retryable (typically 429) reason. If no path ultimately succeeds, an
	// all-retryable exhaustion is surfaced as ErrTypeRateLimit (429) rather than
	// ErrAllFailed (500), so a purely rate-limited client can back off correctly
	// — matching messagesPassthroughWithSeen.
	hitRateLimit := false
	var retry retryTracker
	candidates := r.deployments[req.Model]
	if len(candidates) > 0 {
		ordered := r.weightedShuffle(candidates)
		for _, dep := range ordered {
			if cooled, left := r.isCooledDep(req.Model, dep.Name); cooled {
				hitRateLimit = true // cooled by a prior retryable failure on this path
				retry.note(left)
				continue
			}
			if !r.acquireRateSlot(req.Model, dep) {
				hitRateLimit = true // at its configured rpm/tpm cap this minute
				continue
			}
			subReq := req
			subReq.Model = dep.Model
			resp, err := dep.Provider.Complete(ctx, subReq)
			if err == nil {
				// Stamp the resolved upstream model id (matches OpenAI's
				// response.model semantics: "the model that actually
				// responded"). Cost calculation downstream relies on this
				// to look up the correct per-token rate in the registry.
				m := depMeta(dep)
				resp.Model = dep.Model
				resp.Usage.AuthMode = m.AuthMode
				resp.Usage.Provider = m.Provider
				r.meter.recordTokens(depKey(req.Model, dep.Name), resp.Usage.TotalTokens, r.now())
				r.obsResult(req.Model, dep.Name, dep.Provider.Name(), true)
				return resp, m, nil
			}
			ae := agentmodel.Wrap(err)
			lastErr = ae
			r.obsResult(req.Model, dep.Name, dep.Provider.Name(), false)
			if !ae.Retryable() {
				// Terminal: the walk stops here, so this deployment is the one to
				// blame. Return its attribution so the audit row isn't unattributed.
				return agentmodel.ChatResponse{}, depMeta(dep), ae
			}
			r.coolDep(req.Model, dep.Name, ae.RetryAfter)
			retry.note(ae.RetryAfter)
			r.obsCooldown(req.Model, dep.Name, dep.Provider.Name())
			hitRateLimit = true
		}
	} else if _, hasFallback := r.fallbacks[req.Model]; !hasFallback {
		// No deployments and no fallback paths: definitive miss.
		return agentmodel.ChatResponse{}, StreamMeta{}, ErrNoDeployment
	}

	for _, fb := range r.fallbacks[req.Model] {
		fbReq := req
		fbReq.Model = fb
		resp, meta, err := r.completeWithSeen(ctx, fbReq, seen)
		if err == nil {
			return resp, meta, nil
		}
		ae := agentmodel.Wrap(err)
		lastErr = ae
		if !ae.Retryable() && !errors.Is(ae, ErrAllFailed) && !errors.Is(ae, ErrNoDeployment) {
			// Terminal inside the fallback frame: propagate its attribution.
			return agentmodel.ChatResponse{}, meta, ae
		}
		if ae.Type == agentmodel.ErrTypeRateLimit {
			hitRateLimit = true
			retry.note(ae.RetryAfter)
		}
	}

	// Rate-limit/cooled exhaustion takes precedence: surface 429 so a purely
	// rate-limited client backs off. Otherwise fall back to the richest error we
	// have (the last upstream failure), then to a bare ErrAllFailed. These
	// exhaustion paths span multiple deployments, so there is no single one to
	// attribute — the audit row stays unattributed on purpose.
	if hitRateLimit {
		return agentmodel.ChatResponse{}, StreamMeta{}, errAllRateLimitedAfter(req.Model, retry.soonest)
	}
	if lastErr != nil {
		return agentmodel.ChatResponse{}, StreamMeta{}, &agentmodel.Error{
			Type:    agentmodel.ErrTypeAllDeploymentsFail,
			Message: "all deployments and fallbacks exhausted: last error: " + lastErr.Error(),
			Wrapped: lastErr,
		}
	}
	return agentmodel.ChatResponse{}, StreamMeta{}, ErrAllFailed
}

// StreamMeta names the deployment that served — or, on a terminal failure,
// that failed — a request. Despite the name it is not streaming-only:
// CompleteBlocked returns it too, so a failed non-streaming request can be
// attributed in the audit log. A zero value means "no single deployment
// to attribute" (a pre-routing rejection, or exhaustion across several).
type StreamMeta struct {
	AuthMode  string // "api_key" or "subscription"
	ModelUsed string // upstream model id (e.g. "anthropic/claude-3-5-sonnet-latest")
	Provider  string // serving provider Name() (for the price lookup)
}

// depMeta builds the attribution metadata for a deployment.
func depMeta(dep Deployment) StreamMeta {
	return StreamMeta{
		AuthMode:  dep.Provider.AuthMode(),
		ModelUsed: dep.Model,
		Provider:  dep.Provider.Name(),
	}
}

// Stream dispatches a streaming chat completion. Mirrors Complete's
// fallback semantics: if obtaining the iterator from a deployment fails with
// a retryable error, we try the next deployment. We do NOT mid-stream
// failover (once chunks have started flowing, an in-flight error is final).
func (r *Router) Stream(ctx context.Context, req agentmodel.ChatRequest) (iter.Seq2[provider.StreamChunk, error], StreamMeta, error) {
	return r.streamWithSeen(ctx, req, make(map[string]bool))
}

func (r *Router) streamWithSeen(ctx context.Context, req agentmodel.ChatRequest, seen map[string]bool) (iter.Seq2[provider.StreamChunk, error], StreamMeta, error) {
	if seen[req.Model] {
		return nil, StreamMeta{}, ErrAllFailed
	}
	seen[req.Model] = true

	// See completeWithSeen: an all-retryable exhaustion is surfaced as 429, not
	// 500, so a rate-limited client backs off correctly.
	hitRateLimit := false
	var retry retryTracker
	candidates := r.deployments[req.Model]
	if len(candidates) > 0 {
		ordered := r.weightedShuffle(candidates)
		for _, dep := range ordered {
			if cooled, left := r.isCooledDep(req.Model, dep.Name); cooled {
				hitRateLimit = true // cooled by a prior retryable failure on this path
				retry.note(left)
				continue
			}
			if !r.acquireRateSlot(req.Model, dep) {
				hitRateLimit = true // at its configured rpm/tpm cap this minute
				continue
			}
			subReq := req
			subReq.Model = dep.Model
			seq, err := dep.Provider.Stream(ctx, subReq)
			if err == nil {
				r.obsResult(req.Model, dep.Name, dep.Provider.Name(), true)
				return r.meterStream(seq, req.Model, dep.Name), depMeta(dep), nil
			}
			ae := agentmodel.Wrap(err)
			r.obsResult(req.Model, dep.Name, dep.Provider.Name(), false)
			if !ae.Retryable() {
				// Terminal: attribute the failing deployment (mirrors Complete).
				return nil, depMeta(dep), ae
			}
			r.coolDep(req.Model, dep.Name, ae.RetryAfter)
			retry.note(ae.RetryAfter)
			r.obsCooldown(req.Model, dep.Name, dep.Provider.Name())
			hitRateLimit = true
		}
	} else if _, hasFallback := r.fallbacks[req.Model]; !hasFallback {
		return nil, StreamMeta{}, ErrNoDeployment
	}

	for _, fb := range r.fallbacks[req.Model] {
		fbReq := req
		fbReq.Model = fb
		seq, meta, err := r.streamWithSeen(ctx, fbReq, seen)
		if err == nil {
			return seq, meta, nil
		}
		ae := agentmodel.Wrap(err)
		if !ae.Retryable() && !errors.Is(ae, ErrAllFailed) && !errors.Is(ae, ErrNoDeployment) {
			return nil, meta, ae // terminal inside the fallback frame; propagate attribution
		}
		if ae.Type == agentmodel.ErrTypeRateLimit {
			hitRateLimit = true
			retry.note(ae.RetryAfter)
		}
	}

	if hitRateLimit {
		return nil, StreamMeta{}, errAllRateLimitedAfter(req.Model, retry.soonest)
	}
	return nil, StreamMeta{}, ErrAllFailed
}

// meterStream passes a provider stream through unchanged while crediting its
// usage to the deployment's TPM window — the streaming counterpart of
// Complete's post-response recording. Stream usage is a CUMULATIVE SNAPSHOT,
// not a delta: providers may attach usage to many chunks with running totals
// (Gemini does, per frame), so the wrapper keeps the latest snapshot
// (last-wins, matching the chat handler's finalUsage handling) and records it
// exactly once when iteration ends — including early termination on client
// disconnect, where whatever snapshot arrived is still real consumption.
func (r *Router) meterStream(seq iter.Seq2[provider.StreamChunk, error], logicalModel, depName string) iter.Seq2[provider.StreamChunk, error] {
	return func(yield func(provider.StreamChunk, error) bool) {
		total := 0
		defer func() {
			if total > 0 {
				r.meter.recordTokens(depKey(logicalModel, depName), total, r.now())
			}
		}()
		for chunk, err := range seq {
			if chunk.Usage != nil {
				total = chunk.Usage.TotalTokens // cumulative snapshot, last-wins
			}
			if !yield(chunk, err) {
				return
			}
		}
	}
}

// MessagesPassthrough dispatches an Anthropic-shaped /v1/messages request with
// full fallback semantics: cooled or non-passthrough deployments are skipped,
// and 429 responses cool the deployment and trigger fallback to the next
// candidate or fallback model_name — mirroring Complete/Stream behavior.
// Returns the raw upstream response (caller must close Body), the resolved
// Deployment, and any terminal error.
// The third return value is the logical model that served the request (the
// fallback target when one fired) — the key for RecordTokens.
func (r *Router) MessagesPassthrough(ctx context.Context, body []byte, modelName, clientBetas string) (*http.Response, Deployment, string, error) {
	return r.messagesPassthroughWithSeen(ctx, body, modelName, clientBetas, make(map[string]bool))
}

// messagesPassthroughWithSeen returns, alongside the upstream response and
// deployment, the logical model name of the walk frame that actually served
// the request (== modelName unless a fallback fired). The served model is the
// meter key the walk admitted the dispatch under, so the handler must credit
// post-response token usage to it — crediting the entry model would let
// fallback traffic consume a deployment's TPM invisibly.
//
// On a TERMINAL error the returned Deployment names the one that produced it,
// so the caller can attribute the failure in its audit row. On an
// unattributable error — nothing configured, a fallback cycle, or every path
// exhausted — the Deployment is zero (dep.Provider == nil): no single
// deployment is at fault and the caller must not pretend otherwise.
func (r *Router) messagesPassthroughWithSeen(ctx context.Context, body []byte, modelName, clientBetas string, seen map[string]bool) (*http.Response, Deployment, string, error) {
	if seen[modelName] {
		return nil, Deployment{}, "", ErrAllFailed
	}
	seen[modelName] = true

	candidates := r.deployments[modelName]
	hitRateLimit := false
	var retry retryTracker
	capable := false
	if len(candidates) > 0 {
		for _, dep := range r.weightedShuffle(candidates) {
			pp, ok := dep.Provider.(provider.PassthroughProvider)
			if !ok {
				continue
			}
			capable = true // capability is independent of cooldown/rate state
			if cooled, left := r.isCooledDep(modelName, dep.Name); cooled {
				hitRateLimit = true // previously rate-limited on this request path
				retry.note(left)
				continue
			}
			if !r.acquireRateSlot(modelName, dep) {
				hitRateLimit = true // at its configured rpm/tpm cap this minute
				continue
			}
			resp, err := pp.MessagesPassthrough(ctx, body, dep.Model, clientBetas)
			if err != nil {
				ae := agentmodel.Wrap(err)
				r.obsResult(modelName, dep.Name, dep.Provider.Name(), false)
				if ae.Retryable() {
					r.coolDep(modelName, dep.Name, ae.RetryAfter)
					retry.note(ae.RetryAfter)
					r.obsCooldown(modelName, dep.Name, dep.Provider.Name())
					hitRateLimit = true
					continue
				}
				// Terminal: the walk stops here, so this deployment is the one
				// to blame. Report it (and the frame that admitted it).
				return nil, dep, modelName, err
			}
			if resp == nil {
				continue // provider indicated "not available", try next
			}
			if resp.StatusCode == http.StatusTooManyRequests {
				// No Go error here — the hint lives on the response.
				retryAfter := agentmodel.RetryAfterFromHeader(resp.Header, r.now())
				_ = resp.Body.Close()
				r.obsResult(modelName, dep.Name, dep.Provider.Name(), false)
				r.coolDep(modelName, dep.Name, retryAfter)
				retry.note(retryAfter)
				r.obsCooldown(modelName, dep.Name, dep.Provider.Name())
				hitRateLimit = true
				continue
			}
			r.obsResult(modelName, dep.Name, dep.Provider.Name(), true)
			return resp, dep, modelName, nil
		}
	} else if _, hasFallback := r.fallbacks[modelName]; !hasFallback {
		return nil, Deployment{}, "", ErrNoDeployment
	}

	for _, fb := range r.fallbacks[modelName] {
		resp, dep, served, err := r.messagesPassthroughWithSeen(ctx, body, fb, clientBetas, seen)
		if err == nil {
			return resp, dep, served, nil
		}
		ae := agentmodel.Wrap(err)
		if ae.Retryable() || errors.Is(ae, ErrAllFailed) || errors.Is(ae, ErrNoDeployment) {
			if ae.Type == agentmodel.ErrTypeRateLimit {
				hitRateLimit = true
				retry.note(ae.RetryAfter)
			}
			continue
		}
		// Terminal inside the fallback frame: propagate its attribution rather
		// than blaming the entry model's deployment, which never ran.
		return nil, dep, served, ae
	}

	if hitRateLimit {
		return nil, Deployment{}, "", errAllRateLimitedAfter(modelName, retry.soonest)
	}
	if !capable {
		// The model has deployments but none implement passthrough — a terminal
		// capability miss, not a transient exhaustion. Checked after hitRateLimit
		// so a rate-limited fallback still reports 429 (mirrors GenerateImage).
		return nil, Deployment{}, "", agentmodel.NewErrorf(agentmodel.ErrTypeInvalidRequest,
			"model %q has no passthrough-capable deployment", modelName)
	}
	return nil, Deployment{}, "", ErrAllFailed
}

// ResponsesPassthroughBlocked routes a raw OpenAI Responses request through a
// deployment that explicitly supports that protocol. It shares the deployment
// admission, fallback, cooldown, and observation semantics of Messages.
func (r *Router) ResponsesPassthroughBlocked(ctx context.Context, body []byte, modelName string, blocked []string) (*http.Response, Deployment, string, error) {
	return r.responsesPassthroughWithSeen(ctx, body, modelName, seedSeen(blocked))
}

func (r *Router) responsesPassthroughWithSeen(ctx context.Context, body []byte, modelName string, seen map[string]bool) (*http.Response, Deployment, string, error) {
	if seen[modelName] {
		return nil, Deployment{}, "", ErrAllFailed
	}
	seen[modelName] = true

	candidates := r.deployments[modelName]
	hitRateLimit := false
	capable := false
	var retry retryTracker
	if len(candidates) > 0 {
		for _, dep := range r.weightedShuffle(candidates) {
			pp, ok := dep.Provider.(provider.ResponsesPassthroughProvider)
			if !ok {
				continue
			}
			capable = true
			if cooled, left := r.isCooledDep(modelName, dep.Name); cooled {
				hitRateLimit = true
				retry.note(left)
				continue
			}
			if !r.acquireRateSlot(modelName, dep) {
				hitRateLimit = true
				continue
			}
			resp, err := pp.ResponsesPassthrough(ctx, body, dep.Model)
			if err != nil {
				ae := agentmodel.Wrap(err)
				r.obsResult(modelName, dep.Name, dep.Provider.Name(), false)
				if ae.Retryable() {
					r.coolDep(modelName, dep.Name, ae.RetryAfter)
					retry.note(ae.RetryAfter)
					r.obsCooldown(modelName, dep.Name, dep.Provider.Name())
					hitRateLimit = true
					continue
				}
				return nil, dep, modelName, ae
			}
			if resp == nil {
				continue
			}
			if resp.StatusCode == http.StatusTooManyRequests {
				retryAfter := agentmodel.RetryAfterFromHeader(resp.Header, r.now())
				_ = resp.Body.Close()
				r.obsResult(modelName, dep.Name, dep.Provider.Name(), false)
				r.coolDep(modelName, dep.Name, retryAfter)
				retry.note(retryAfter)
				r.obsCooldown(modelName, dep.Name, dep.Provider.Name())
				hitRateLimit = true
				continue
			}
			r.obsResult(modelName, dep.Name, dep.Provider.Name(), true)
			return resp, dep, modelName, nil
		}
	} else if _, hasFallback := r.fallbacks[modelName]; !hasFallback {
		return nil, Deployment{}, "", ErrNoDeployment
	}

	for _, fb := range r.fallbacks[modelName] {
		resp, dep, served, err := r.responsesPassthroughWithSeen(ctx, body, fb, seen)
		if err == nil {
			return resp, dep, served, nil
		}
		ae := agentmodel.Wrap(err)
		if ae.Retryable() || errors.Is(ae, ErrAllFailed) || errors.Is(ae, ErrNoDeployment) {
			if ae.Type == agentmodel.ErrTypeRateLimit {
				hitRateLimit = true
				retry.note(ae.RetryAfter)
			}
			continue
		}
		return nil, dep, served, ae
	}
	if hitRateLimit {
		return nil, Deployment{}, "", errAllRateLimitedAfter(modelName, retry.soonest)
	}
	if !capable {
		return nil, Deployment{}, "", agentmodel.NewErrorf(agentmodel.ErrTypeInvalidRequest,
			"model %q has no Responses-capable deployment", modelName)
	}
	return nil, Deployment{}, "", ErrAllFailed
}

// PickDeployment returns one weighted-shuffle pick from the configured
// deployments for modelName, or ErrNoDeployment if none exist. Unlike
// MessagesPassthrough, this does not iterate fallbacks — kept for callers that
// need a single provider+model pair without retry logic.
func (r *Router) PickDeployment(modelName string) (Deployment, error) {
	candidates := r.deployments[modelName]
	if len(candidates) == 0 {
		return Deployment{}, ErrNoDeployment
	}
	return r.weightedShuffle(candidates)[0], nil
}

// Embed dispatches an embedding request. No fallback (embeddings rarely
// have meaningful inter-provider equivalents).
func (r *Router) Embed(ctx context.Context, req agentmodel.EmbeddingRequest) (agentmodel.EmbeddingResponse, error) {
	candidates := r.deployments[req.Model]
	if len(candidates) == 0 {
		return agentmodel.EmbeddingResponse{}, ErrNoDeployment
	}
	ordered := r.weightedShuffle(candidates)
	hitRateLimit := false
	for _, dep := range ordered {
		if !r.acquireRateSlot(req.Model, dep) {
			hitRateLimit = true // at its configured rpm/tpm cap this minute
			continue
		}
		subReq := req
		subReq.Model = dep.Model
		resp, err := dep.Provider.Embed(ctx, subReq)
		if err == nil {
			resp.Model = dep.Model
			resp.Usage.AuthMode = dep.Provider.AuthMode()
			resp.Usage.Provider = dep.Provider.Name()
			r.meter.recordTokens(depKey(req.Model, dep.Name), resp.Usage.TotalTokens, r.now())
			r.obsResult(req.Model, dep.Name, dep.Provider.Name(), true)
			return resp, nil
		}
		ae := agentmodel.Wrap(err)
		r.obsResult(req.Model, dep.Name, dep.Provider.Name(), false)
		if !ae.Retryable() {
			return agentmodel.EmbeddingResponse{}, ae
		}
	}
	// An all-metered exhaustion is a 429 so the caller backs off until the
	// minute window rolls. Scope note: only the meter skip sets the flag here
	// — Embed has no cooldown integration, so (pre-existing) an exhaustion
	// from retryable upstream failures still surfaces as ErrAllFailed.
	if hitRateLimit {
		return agentmodel.EmbeddingResponse{}, errAllRateLimited(req.Model)
	}
	return agentmodel.EmbeddingResponse{}, ErrAllFailed
}

// GenerateImage dispatches a text-to-image request to a deployment of req.Model
// that implements provider.ImageGenerator. Like Embed there is no cross-provider
// fallback (image models are not freely interchangeable); deployments that don't
// support image generation are skipped, and if none of the model's deployments
// are image-capable the request fails with an invalid_request error rather than
// a generic "all failed".
func (r *Router) GenerateImage(ctx context.Context, req agentmodel.ImageRequest) (agentmodel.ImageResponse, error) {
	candidates := r.deployments[req.Model]
	if len(candidates) == 0 {
		return agentmodel.ImageResponse{}, ErrNoDeployment
	}
	ordered := r.weightedShuffle(candidates)
	capable := false
	hitRateLimit := false
	for _, dep := range ordered {
		gen, ok := dep.Provider.(provider.ImageGenerator)
		if !ok {
			continue // this deployment's provider can't generate images
		}
		capable = true
		if !r.acquireRateSlot(req.Model, dep) {
			hitRateLimit = true // at its configured rpm/tpm cap this minute
			continue
		}
		subReq := req
		subReq.Model = dep.Model
		resp, err := gen.GenerateImage(ctx, subReq)
		if err == nil {
			resp.Model = dep.Model
			resp.Usage.AuthMode = dep.Provider.AuthMode()
			resp.Usage.Provider = dep.Provider.Name()
			r.meter.recordTokens(depKey(req.Model, dep.Name), resp.Usage.TotalTokens, r.now())
			r.obsResult(req.Model, dep.Name, dep.Provider.Name(), true)
			return resp, nil
		}
		ae := agentmodel.Wrap(err)
		r.obsResult(req.Model, dep.Name, dep.Provider.Name(), false)
		if !ae.Retryable() {
			return agentmodel.ImageResponse{}, ae
		}
	}
	if !capable {
		return agentmodel.ImageResponse{}, agentmodel.NewErrorf(agentmodel.ErrTypeInvalidRequest,
			"model %q has no image-capable deployment", req.Model)
	}
	if hitRateLimit {
		return agentmodel.ImageResponse{}, errAllRateLimited(req.Model)
	}
	return agentmodel.ImageResponse{}, ErrAllFailed
}

// weightedShuffle returns a permutation of deps where higher-weighted entries
// have higher probability of appearing earlier.
//
// Algorithm: assign each entry a key = -ln(random) / weight; sort ascending.
// Equivalent to weighted random sampling without replacement.
//
// Note this is the opposite policy from pool.Pool's sticky credential
// selection, deliberately: weight means what it says, and re-sampling per
// request is what honors it. The cost is that two deployments of the SAME
// provider under one model_name are different accounts, so consecutive requests
// landing on different ones miss each other's prompt cache. Fixing that needs
// affinity keyed by the request's cache identity (chatgpt.stableSessionID is
// the same idea), not last-served stickiness, which cannot reason about
// concurrent conversations. A pooled deployment is a single entry here, so the
// shuffle is a no-op for it and the two policies do not currently interact.
func (r *Router) weightedShuffle(deps []Deployment) []Deployment {
	if len(deps) <= 1 {
		out := make([]Deployment, len(deps))
		copy(out, deps)
		return out
	}

	type keyed struct {
		key float64
		dep Deployment
	}

	r.mu.Lock()
	ks := make([]keyed, len(deps))
	for i, d := range deps {
		w := d.Weight
		if w <= 0 {
			w = 1
		}
		// Avoid log(0); rand.Float64() returns [0,1), so ensure non-zero.
		u := r.rand.Float64()
		if u == 0 {
			u = 1e-9
		}
		// -ln(u) / weight: higher weight => smaller (better) key.
		ks[i] = keyed{key: -math.Log(u) / float64(w), dep: d}
	}
	r.mu.Unlock()

	sort.Slice(ks, func(i, j int) bool { return ks[i].key < ks[j].key })

	out := make([]Deployment, len(ks))
	for i, k := range ks {
		out[i] = k.dep
	}
	return out
}
