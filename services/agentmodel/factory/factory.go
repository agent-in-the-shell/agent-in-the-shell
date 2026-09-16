// Package factory builds the router deployment map from a validated config.
//
// It is the production wiring that turns each model_list[].deployments[] entry
// into a live provider.Provider behind a router.Deployment. It lives in its own
// leaf package (rather than cmd/agent-model) so tests — notably the
// cross-provider conformance suite — can exercise the exact LoadConfig → factory
// → router → api construction path the server uses, including the base_url seam
// that points a provider at a fake or self-hosted upstream.
//
// Placement note: this cannot live in the root services/agentmodel package
// because every provider imports agentmodel, so a factory there would form an
// import cycle. As a leaf importing agentmodel + auth + provider/* it is
// cycle-free.
package factory

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/anthropic"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/azure"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/chatgpt"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/deepseek"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/gemini"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/messagesbridge"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/openai"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/openaicompat"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/pool"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/replicate"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
)

// BuildDeployments wires each configured deployment to a real Provider, using
// the credentials available in the environment. Deployments whose credentials
// are missing are skipped with a warning (a keyless deployment is valid only
// when base_url is set — it builds with an empty static key). The returned
// *auth.ChatGPTOAuth is non-nil when a ChatGPT subscription deployment created
// a shared OAuth client (reused across deployments and surfaced on /v1/oauth).
func BuildDeployments(cfg *agentmodel.Config, logger *slog.Logger) (map[string][]router.Deployment, *auth.ChatGPTOAuth, error) {
	deployments := make(map[string][]router.Deployment, len(cfg.ModelList))
	var sharedChatGPTAuth *auth.ChatGPTOAuth

	for _, m := range cfg.ModelList {
		var deps []router.Deployment
		for _, d := range m.Deployments {
			p, chatgptAuth, err := buildDeploymentProvider(d, m.ModelName, logger, sharedChatGPTAuth)
			if err != nil {
				return nil, nil, err
			}
			if chatgptAuth != nil {
				sharedChatGPTAuth = chatgptAuth
			}
			if p == nil {
				continue // skipped (missing creds)
			}
			// weight: 0 is an explicit "disabled" — drop the deployment. An
			// omitted weight defaults to 1 (EffectiveWeight), so this only fires
			// when the config sets 0 on purpose.
			w := d.EffectiveWeight()
			if w == 0 {
				logger.Info("skipping deployment: weight 0 (disabled)", "model", m.ModelName, "provider", d.Provider, "model_upstream", d.Model)
				continue
			}
			deps = append(deps, router.Deployment{
				// Include the auth mode so two deployments under one model_name
				// that share a provider string + model but differ in credentials
				// get distinct Names. Name is the router's cooldown + rate-meter
				// identity (depKey); a collision would park both on one 429 and
				// co-mingle their RPM/TPM, mirroring providerCacheKey's
				// Name()+AuthMode() disambiguation.
				Name:     fmt.Sprintf("%s/%s|%s", d.Provider, d.Model, p.AuthMode()),
				Provider: p,
				Model:    d.Model,
				Weight:   w,
				RPM:      d.RPM,
				TPM:      d.TPM,
			})
		}
		if len(deps) > 0 {
			deployments[m.ModelName] = deps
		} else {
			logger.Warn("model has no usable deployments (all skipped due to missing creds)", "model", m.ModelName)
		}
	}

	return deployments, sharedChatGPTAuth, nil
}

// buildDeploymentProvider constructs the provider.Provider for a single
// DeploymentConfig entry. Returns (nil, nil, nil) when the deployment should
// be skipped (missing credentials). The third return value is non-nil only
// when the deployment is a ChatGPT subscription that created a new shared auth.
func buildDeploymentProvider(d agentmodel.DeploymentConfig, modelName string, logger *slog.Logger, existingChatGPTAuth *auth.ChatGPTOAuth) (provider.Provider, *auth.ChatGPTOAuth, error) {
	// Resolve the list of API key env var names, preferring api_key_envs over api_key_env.
	keyEnvs := d.APIKeyEnvs
	if len(keyEnvs) == 0 && d.APIKeyEnv != "" {
		keyEnvs = []string{d.APIKeyEnv}
	}

	switch d.AuthMode {
	case "", agentmodel.AuthModeAPIKey:
		// Azure routes to a deployment that defaults to the model name; other
		// providers ignore the Azure fields on providerTarget.
		deployment := d.DeploymentName
		if deployment == "" {
			deployment = d.Model
		}
		p, err := buildProviderPool(d.Provider, keyEnvs, providerTarget{
			baseURL: d.BaseURL, apiVersion: d.APIVersion, deployment: deployment, cacheTTL: d.CacheTTL,
		}, func(token string) auth.Authenticator {
			return staticAuthFor(d.Provider, token)
		}, modelName, logger)
		if err != nil {
			return nil, nil, err
		}
		if p == nil {
			logger.Warn("skipping deployment: no usable credentials", "model", modelName)
			return nil, nil, nil
		}
		return p, nil, nil

	case agentmodel.AuthModeSubscription:
		switch d.Provider {
		case "anthropic", "anthropic-oauth":
			// oauth_token_dir(s) take precedence over api_key_env(s): these
			// credentials refresh themselves, static OAuth bearer tokens expire.
			// Multiple dirs = one refreshable authenticator per account, rotated
			// by the pool on rate-limit. A single dir builds the bare
			// client — a pool of one only adds a cooldown that can park the sole
			// credential with nothing to fall back to.
			if dirs := oauthTokenDirs(d); len(dirs) > 0 {
				if len(keyEnvs) > 0 {
					logger.Warn("oauth_token_dir(s) take precedence over api_key_env(s) for this deployment",
						"model", modelName, "provider", d.Provider)
				}
				providers := make([]provider.Provider, 0, len(dirs))
				for _, dir := range dirs {
					p, err := buildProvider(d.Provider, auth.NewAnthropicOAuthRefreshable(dir, nil), providerTarget{baseURL: d.BaseURL, cacheTTL: d.CacheTTL})
					if err != nil {
						return nil, nil, err
					}
					providers = append(providers, p)
				}
				if len(providers) == 1 {
					return providers[0], nil, nil
				}
				return pool.New(providers), nil, nil
			}
			p, err := buildProviderPool(d.Provider, keyEnvs, providerTarget{baseURL: d.BaseURL, cacheTTL: d.CacheTTL}, func(token string) auth.Authenticator {
				return auth.NewAnthropicOAuth(token)
			}, modelName, logger)
			if err != nil {
				return nil, nil, err
			}
			if p == nil {
				logger.Warn("skipping subscription deployment: no usable credentials", "model", modelName)
				return nil, nil, nil
			}
			return p, nil, nil

		case "chatgpt":
			// Multi-account pool: oauth_token_dirs lists one auth.json directory
			// per logged-in ChatGPT account. Build a distinct OAuth client per
			// directory and rotate across them on rate-limit. The first account's
			// auth is surfaced as the shared client (for /v1/oauth login + reuse
			// by sibling deployments), preserving single-dir semantics.
			if len(d.OAuthTokenDirs) > 0 {
				// "First chatgpt deployment wins" for the shared client (matching
				// the single-dir reuse path below): keep any pre-existing shared
				// auth, otherwise adopt the first account's own pooled client so a
				// /v1/oauth login refreshes the client actually serving traffic.
				shared := existingChatGPTAuth
				providers := make([]provider.Provider, 0, len(d.OAuthTokenDirs))
				for _, dir := range d.OAuthTokenDirs {
					ca := auth.NewChatGPTOAuth(dir, nil)
					if shared == nil {
						shared = ca
					}
					p, err := buildProvider(d.Provider, ca, providerTarget{baseURL: d.BaseURL, cacheTTL: d.CacheTTL})
					if err != nil {
						return nil, nil, err
					}
					providers = append(providers, p)
				}
				// Wrap the rotating pool in one bridge so both /v1/chat/completions
				// and /v1/messages traffic rotates across accounts. A one-entry
				// list skips the pool for the same reason the anthropic branch
				// does: a pool of one only adds a cooldown that can park the sole
				// credential with nothing to fall back to.
				if len(providers) == 1 {
					return messagesbridge.New(providers[0]), shared, nil
				}
				return messagesbridge.New(pool.New(providers)), shared, nil
			}

			chatgptAuth := existingChatGPTAuth
			if chatgptAuth == nil {
				chatgptAuth = auth.NewChatGPTOAuth(d.OAuthTokenDir, nil)
			}
			p, err := buildProvider(d.Provider, chatgptAuth, providerTarget{baseURL: d.BaseURL, cacheTTL: d.CacheTTL})
			if err != nil {
				return nil, nil, err
			}
			// Wrap so the deployment also serves Anthropic /v1/messages traffic
			// (e.g. pi-mom / Claude Code) by translating to the provider's
			// OpenAI/Responses shape. /v1/chat/completions is unaffected — the
			// wrapper delegates the standard Provider methods.
			return messagesbridge.New(p), chatgptAuth, nil

		default:
			return nil, nil, fmt.Errorf("provider %q does not support subscription auth_mode", d.Provider)
		}
	}
	return nil, nil, fmt.Errorf("unknown auth_mode %q", d.AuthMode)
}

// oauthTokenDirs normalizes the singular/plural OAuth token-dir spellings into
// one list. Config validation rejects setting both, so at most one is non-empty.
// Returns nil when the deployment names no token directory at all.
func oauthTokenDirs(d agentmodel.DeploymentConfig) []string {
	if len(d.OAuthTokenDirs) > 0 {
		return d.OAuthTokenDirs
	}
	if d.OAuthTokenDir != "" {
		return []string{d.OAuthTokenDir}
	}
	return nil
}

// providerTarget bundles the upstream-routing inputs a provider may need: the
// base_url override (all providers) plus the Azure-only api-version + deployment
// (, ignored elsewhere). Grouping them keeps buildProvider/buildProviderPool
// signatures stable as more endpoint-shaped providers are added.
type providerTarget struct {
	baseURL    string
	apiVersion string // Azure only
	deployment string // Azure only
	cacheTTL   string // Anthropic only: prompt-cache TTL ("", "5m", "1h")
}

// buildProviderPool iterates keyEnvs, reads each token from the environment,
// builds a provider for each non-empty token via authFn, and returns a single
// provider (or a pool when multiple tokens are present). When no tokens are
// available but base_url is set, it builds a single keyless provider with an
// empty static key (a local/fake OpenAI-compatible backend needs no key).
// Returns nil when no tokens are available and no base_url is set (caller should
// warn and skip the deployment).
func buildProviderPool(providerName string, keyEnvs []string, t providerTarget, authFn func(string) auth.Authenticator, modelName string, logger *slog.Logger) (provider.Provider, error) {
	var providers []provider.Provider
	for _, env := range keyEnvs {
		token := os.Getenv(env)
		if token == "" {
			logger.Warn("skipping credential: env var unset", "model", modelName, "env", env)
			continue
		}
		p, err := buildProvider(providerName, authFn(token), t)
		if err != nil {
			return nil, err
		}
		providers = append(providers, p)
	}
	if len(providers) == 0 {
		// A keyless backend is reachable only through base_url; build one
		// provider with an empty static key (wire-harmless).
		if t.baseURL != "" {
			return buildProvider(providerName, staticAuthFor(providerName, ""), t)
		}
		return nil, nil
	}
	if len(providers) == 1 {
		return providers[0], nil
	}
	return pool.New(providers), nil
}

// staticAuthHeaders defines per-provider header shape for static API keys.
// Defaults to Authorization: Bearer when no entry matches (matches openai
// and any future Bearer-token providers).
var staticAuthHeaders = map[string]struct {
	Header string
	Prefix string
}{
	"anthropic":       {Header: "x-api-key"},
	"anthropic-oauth": {Header: "x-api-key"},
	"gemini":          {Header: "x-goog-api-key"},
	"azure":           {Header: "api-key"}, // Azure OpenAI uses api-key, not Bearer
}

// staticAuthFor returns a StaticKey configured for the provider's expected header.
func staticAuthFor(providerName, token string) *auth.StaticKey {
	if h, ok := staticAuthHeaders[providerName]; ok {
		return &auth.StaticKey{HeaderName: h.Header, Prefix: h.Prefix, Token: token}
	}
	return &auth.StaticKey{HeaderName: "Authorization", Prefix: "Bearer ", Token: token}
}

// buildProvider constructs a provider client for name, pointing it at t.baseURL
// (via the provider's NewWithBaseURL constructor) when non-empty. The Azure
// fields on t are used only by the azure case.
func buildProvider(name string, auther auth.Authenticator, t providerTarget) (provider.Provider, error) {
	switch name {
	case "azure":
		// Azure OpenAI reuses the openai wire shaping through a deployment-scoped,
		// api-version'd client. base_url is the resource host (required by
		// config validation); deployment is resolved by the caller (defaults to
		// the model).
		return azure.New(auther, t.baseURL, t.apiVersion, t.deployment), nil
	case "openai":
		if t.baseURL != "" {
			return openai.NewWithBaseURL(auther, t.baseURL), nil
		}
		return openai.New(auther), nil
	case "anthropic", "anthropic-oauth":
		if t.baseURL != "" {
			return anthropic.NewWithBaseURL(auther, t.baseURL, anthropic.WithCacheTTL(t.cacheTTL)), nil
		}
		return anthropic.New(auther, anthropic.WithCacheTTL(t.cacheTTL)), nil
	case "gemini":
		if t.baseURL != "" {
			return gemini.NewWithBaseURL(auther, t.baseURL), nil
		}
		return gemini.New(auther), nil
	case "chatgpt":
		if t.baseURL != "" {
			return chatgpt.NewWithBaseURL(auther, t.baseURL), nil
		}
		return chatgpt.New(auther), nil
	case "deepseek":
		// OpenAI-compatible; Bearer auth via the default staticAuthFor path.
		if t.baseURL != "" {
			return deepseek.NewWithBaseURL(auther, t.baseURL), nil
		}
		return deepseek.New(auther), nil
	case "replicate":
		if t.baseURL != "" {
			return replicate.NewWithBaseURL(auther, t.baseURL), nil
		}
		return replicate.New(auther), nil
	default:
		// Any other name that speaks the OpenAI /chat/completions wire format: a
		// known popular vendor (default base URL from openaiCompatBaseURLs) or any
		// endpoint with an explicit base_url. Bearer auth via staticAuthFor.
		baseURL := t.baseURL
		if baseURL == "" {
			baseURL = openaiCompatBaseURLs[name]
		}
		if baseURL != "" {
			return openaicompat.New(name, baseURL, auther), nil
		}
		return nil, fmt.Errorf("unknown provider %q (set base_url for an OpenAI-compatible endpoint)", name)
	}
}

// openaiCompatBaseURLs is the default endpoint for popular OpenAI-wire-compatible
// providers, so `provider: <name>` works with no base_url; a deployment may still
// override base_url, and any unlisted OpenAI-compatible endpoint works if it sets
// one. All authenticate with a Bearer key (the staticAuthFor default).
var openaiCompatBaseURLs = map[string]string{
	// Western / aggregator OpenAI-compatible endpoints.
	"groq":       "https://api.groq.com/openai/v1",
	"mistral":    "https://api.mistral.ai/v1",
	"together":   "https://api.together.xyz/v1",
	"xai":        "https://api.x.ai/v1",
	"openrouter": "https://openrouter.ai/api/v1",
	"fireworks":  "https://api.fireworks.ai/inference/v1",
	"perplexity": "https://api.perplexity.ai",
	"deepinfra":  "https://api.deepinfra.com/v1/openai",
	"nebius":     "https://api.studio.nebius.ai/v1",
	// Chinese / open-source model providers — all OpenAI-compatible, Bearer auth.
	"zhipu":       "https://open.bigmodel.cn/api/paas/v4",              // GLM-4 / GLM-4-Plus / GLM-4-Flash (Zhipu AI)
	"moonshot":    "https://api.moonshot.cn/v1",                        // Kimi
	"qwen":        "https://dashscope.aliyuncs.com/compatible-mode/v1", // Alibaba Qwen (DashScope OpenAI-compatible mode)
	"yi":          "https://api.lingyiwanwu.com/v1",                    // 01.AI Yi
	"baichuan":    "https://api.baichuan-ai.com/v1",                    // Baichuan
	"stepfun":     "https://api.stepfun.com/v1",                        // StepFun
	"siliconflow": "https://api.siliconflow.cn/v1",                     // aggregator: Qwen/GLM/DeepSeek/Yi/... open-source
}
