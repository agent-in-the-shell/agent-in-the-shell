// Package api wires the agentmodel HTTP surface: OpenAI-compatible
// /v1/chat/completions, /v1/embeddings, /v1/images/generations,
// /v1/videos/generations (+ GET /v1/videos/{id}), /v1/models, the /v1/limits
// rate-limit snapshot, the /v1/account/anthropic/usage subscription-usage
// probe, /healthz, /readyz, plus the ChatGPT OAuth helper endpoints under
// /v1/oauth/chatgpt/*.
package api

import (
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/cache"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/contentlog"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/cost"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/telemetry"
)

// Server holds the wired-up dependencies. Construct with New, then call
// Handler() to obtain an http.Handler suitable for http.ListenAndServe.
// maxRequestBodyBytes caps request bodies so an authenticated caller can't force
// an unbounded allocation. 64 MiB is far above any real prompt (the largest
// accepted upstream context is ~272k tokens) while still bounding abuse.
const maxRequestBodyBytes = 64 << 20

type Server struct {
	router      *router.Router
	store       store.Store
	registry    *cost.Registry
	bearerToken string
	orgCap      *spendCap // org-wide budget, resolved once at construction; nil = uncapped
	// orgCapInvalid marks a configured org budget whose window failed to
	// parse (unreachable via LoadConfig; possible via direct construction).
	// Spending requests fail closed while it is set.
	orgCapInvalid bool
	keys          []resolvedKey      // all configured keys; disabled ones never authenticate but still report caps
	chatgptAuth   *auth.ChatGPTOAuth // may be nil if subscription provider not configured
	// Anthropic subscription-usage endpoint (GET /v1/account/anthropic/usage).
	// anthropicUsageURL/anthropicClient are overridable for tests; empty/nil use
	// production defaults. anthropicRefreshers caches one refreshable per profile
	// so this process is the single owner that ever rotates the (single-use)
	// refresh token — see anthropic_usage.go.
	anthropicUsageURL   string
	anthropicClient     *http.Client
	anthropicMu         sync.Mutex
	anthropicRefreshers map[string]*auth.AnthropicOAuthRefreshable
	logger              *slog.Logger
	telemetry           *telemetry.Telemetry // optional; nil is a no-op
	contentLog          *contentlog.Logger   // optional; nil disables full request/response body logging
	cache               cache.Cache          // optional; nil disables response caching (#48)
}

// Config is what callers pass to New.
type Config struct {
	Router      *router.Router
	Store       store.Store
	Registry    *cost.Registry
	BearerToken string
	Budget      agentmodel.BudgetConfig // optional; org-wide spend cap, reported on /v1/limits (enforcement: #47)
	Keys        []agentmodel.KeyConfig  // optional; virtual-key caps, reported on /v1/limits (auth wiring: #52)
	ChatGPTAuth *auth.ChatGPTOAuth      // optional
	// AnthropicUsageURL/AnthropicClient override the Anthropic subscription-usage
	// endpoint and HTTP client (GET /v1/account/anthropic/usage). Both optional;
	// empty/nil use the production endpoint and a 30s client. Intended for tests.
	AnthropicUsageURL string
	AnthropicClient   *http.Client
	Logger            *slog.Logger         // optional; defaults to slog.Default()
	Telemetry         *telemetry.Telemetry // optional; nil disables metrics/tracing
	ContentLog        *contentlog.Logger   // optional; nil disables full request/response body logging
	Cache             cache.Cache          // optional; nil disables response caching (#48)
}

// New constructs a Server from Config. Virtual-key tokens are resolved from
// their token_env vars here, once at construction — a key whose env var is
// unset is disabled (fail-closed) with a warning.
func New(c Config) *Server {
	logger := c.Logger
	if logger == nil {
		logger = slog.Default()
	}
	orgCap, orgCapOK := resolveCap(c.Budget.MaxBudget, c.Budget.BudgetDuration)
	if !orgCapOK {
		logger.Error("agentmodel: org budget_duration failed to parse — spending requests will fail closed")
	}
	// One shared, bounded client for both the Anthropic usage GET and the token
	// refresh. Without this the usage GET would fall back to http.DefaultClient
	// (no timeout) while each per-profile refresher built its own 30s client;
	// sharing one gives the GET the same 30s bound and avoids the client sprawl.
	anthropicClient := c.AnthropicClient
	if anthropicClient == nil {
		anthropicClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Server{
		router:        c.Router,
		store:         c.Store,
		registry:      c.Registry,
		bearerToken:   c.BearerToken,
		orgCap:        orgCap,
		orgCapInvalid: !orgCapOK,
		keys:          resolveKeys(c.Keys, logger),
		chatgptAuth:   c.ChatGPTAuth,

		anthropicUsageURL:   c.AnthropicUsageURL,
		anthropicClient:     anthropicClient,
		anthropicRefreshers: map[string]*auth.AnthropicOAuthRefreshable{},

		logger:     logger,
		telemetry:  c.Telemetry,
		contentLog: c.ContentLog,
		cache:      c.Cache,
	}
}

// Handler returns the configured chi router. Mount this at the desired
// path; the routes inside are absolute (e.g. /v1/chat/completions).
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()

	// Production hygiene: every request gets a UUID for log correlation,
	// real-IP extraction handles X-Forwarded-For correctly, and Recoverer
	// catches handler panics so a single bad request doesn't crash the
	// process.
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)

	// Health endpoints — unauthenticated.
	r.Get("/healthz", s.healthz)
	r.Get("/livez", s.livez)
	r.Get("/readyz", s.readyz)

	// Prometheus scrape endpoint — unauthenticated, opt-in via telemetry config.
	// Exposes only operational metadata (counts/latency/spend), never content.
	if s.telemetry.MetricsEnabled() {
		r.Handle("/metrics", s.telemetry.MetricsHandler())
	}

	// Authenticated /v1/* surface.
	r.Route("/v1", func(r chi.Router) {
		r.Use(s.bearerAuth)
		// Cap request bodies so an authenticated caller can't force an unbounded
		// allocation. One middleware covers every POST in the group (chat,
		// messages, embeddings, images, video, predictions) rather than each
		// handler re-wrapping r.Body.
		r.Use(middleware.RequestSize(maxRequestBodyBytes))
		r.Post("/chat/completions", s.chatCompletions)
		r.Post("/messages", s.messages)
		r.Post("/embeddings", s.embeddings)
		r.Post("/images/generations", s.imageGenerations)
		r.Post("/videos/generations", s.createVideo)
		r.Get("/videos/{id}", s.getVideo)
		r.Get("/videos/{id}/content", s.downloadVideoContent)
		// Replicate passthrough (#847): a raw vendor-protocol surface alongside
		// the normalized routes above. Both SDK base-URL conventions (Python
		// host-without-/v1 appends /v1; JS base includes /v1) land on
		// /v1/predictions. Kept self-contained so the group can be lifted out
		// later once the ledger moves off embedded SQLite.
		r.Post("/predictions", s.createPrediction)
		r.Get("/predictions/{id}", s.getPrediction)
		r.Post("/predictions/{id}/cancel", s.cancelPrediction)
		r.Post("/models/{owner}/{name}/predictions", s.createPrediction)
		r.Get("/models", s.listModels)
		r.Get("/limits", s.limits)
		r.Get("/usage", s.usage)
		// Upstream provider-account subscription usage, read through the gateway
		// so this process is the sole owner of the OAuth refresh token. Distinct
		// from /v1/limits (the gateway's OWN caps/cooldowns/budgets).
		r.Get("/account/anthropic/usage", s.anthropicUsage)
		// Credential management rebinds the gateway's upstream subscription
		// tokens — operator authority, never tenant authority. Master token
		// only: a virtual key completing a device-code flow could otherwise
		// replace the upstream account (#52).
		r.Group(func(r chi.Router) {
			r.Use(s.masterOnly)
			r.Post("/oauth/chatgpt/start", s.chatgptOAuthStart)
			r.Post("/oauth/chatgpt/poll", s.chatgptOAuthPoll)
			// Runtime virtual-key management (#922): minting/revoking tenant
			// credentials is operator authority, never tenant authority.
			r.Post("/keys", s.createKey)
			r.Get("/keys", s.listKeys)
			r.Get("/keys/{id}", s.getKey)
			r.Post("/keys/{id}/revoke", s.setKeyRevoked(true))
			r.Post("/keys/{id}/unrevoke", s.setKeyRevoked(false))
			r.Delete("/keys/{id}", s.deleteKey)
		})
	})

	return r
}
