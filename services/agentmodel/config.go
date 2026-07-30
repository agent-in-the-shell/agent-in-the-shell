package agentmodel

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/internal/herospath"
	"gopkg.in/yaml.v3"
)

// Config is the YAML configuration shape for the agentmodel service.
//
// Example:
//
//	listen: ":8080"
//	db: "agentmodel.db"
//	auth:
//	  bearer_token_env: "AGENT_MODEL_TOKEN"
//	model_list:
//	  - model_name: "gpt-4"
//	    deployments:
//	      - provider: "openai"
//	        model: "gpt-4o"
//	        auth_mode: "api_key"
//	        api_key_env: "OPENAI_API_KEY"
//	        weight: 100
//	fallbacks:
//	  - model_name: "gpt-4"
//	    fallback_to: ["claude"]
//	budget:
//	  max_budget: 100.0
//	  budget_duration: "720h"
//	keys:
//	  - name: "ci"
//	    token_env: "AGENT_MODEL_KEY_CI"
//	    max_budget: 10.0
//	    budget_duration: "24h"
//	    models: ["gpt-4"]
type Config struct {
	Listen     string           `yaml:"listen"`
	DB         string           `yaml:"db"`
	Auth       AuthConfig       `yaml:"auth"`
	ModelList  []ModelEntry     `yaml:"model_list"`
	Fallbacks  []FallbackRule   `yaml:"fallbacks"`
	Budget     BudgetConfig     `yaml:"budget,omitempty"`
	Keys       []KeyConfig      `yaml:"keys,omitempty"`
	Telemetry  TelemetryConfig  `yaml:"telemetry"`
	Retention  RetentionConfig  `yaml:"retention"`
	ContentLog ContentLogConfig `yaml:"content_log,omitempty"`
	Cache      CacheConfig      `yaml:"cache,omitempty"`
	// RouterCooldown is how long a deployment is parked after a retryable
	// failure (e.g. a 429) before it is retried, as a Go duration string
	// ("5m", "30s"). Empty uses the built-in default (5m). "0" disables
	// cooling, so a failed deployment is eligible again on the next request.
	RouterCooldown string `yaml:"router_cooldown,omitempty"`
	// RevalidateInterval enables periodic re-validation of configured
	// deployments against their providers' live model lists, as a Go duration
	// string ("1h", "30m"). Empty (the default) keeps the historical
	// one-shot-at-startup check; when set, a background loop re-runs the drift
	// check on this interval and a TTL'd cache of each provider's model list
	// (window == this interval) backs both the startup and periodic passes.
	// The check is always best-effort and off the hot path.
	RevalidateInterval string `yaml:"revalidate_interval,omitempty"`
}

// RetentionConfig controls automatic purging of audit rows. Retention is
// OPT-IN: an empty Period keeps rows forever (the default), matching the
// historical behavior. When Period is set, a background job deletes
// request-log rows older than Period every Interval.
//
//	retention:
//	  period: "720h"     # delete audit rows older than 30 days; empty = keep forever
//	  interval: "1h"     # how often to purge (default 1h)
type RetentionConfig struct {
	Period   string `yaml:"period"`
	Interval string `yaml:"interval,omitempty"`
}

// ContentLogConfig enables an opt-in JSON Lines log of full request and
// response bodies (raw prompts and completions). It is OFF by default: an empty
// Path disables it, matching the gateway's metadata-only default. When set, each
// completed /v1/chat/completions and /v1/messages exchange is appended as one
// JSON line. Streaming responses are reassembled into the final message before
// logging. Records contain raw content with no redaction — restrict the file's
// permissions and retention accordingly.
//
// Rotation, compression, and retention are opt-in and independent. Set any of
// max_size_mb / rotate_every to bound the active file; the rotated files are
// gzipped when compress is true and pruned by max_backups (count), max_age, and
// max_total_mb (whole-log footprint). With none set the log is a single
// unbounded file. Rotation happens under the write lock, so no line is lost.
//
//	content_log:
//	  path: "/var/log/agentmodel/content.jsonl"  # empty = disabled
//	  max_size_mb: 100        # rotate past this size; 0 = no size rotation
//	  rotate_every: "24h"     # rotate at this age; empty = no time rotation
//	  compress: true          # gzip rotated files
//	  max_backups: 30         # keep at most N rotated files; 0 = unlimited
//	  max_age: "720h"         # delete rotated files older than this; empty = keep
//	  max_total_mb: 2048      # cap active+backups; delete oldest over it; 0 = off
type ContentLogConfig struct {
	Path        string `yaml:"path,omitempty"`
	MaxSizeMB   int    `yaml:"max_size_mb,omitempty"`
	RotateEvery string `yaml:"rotate_every,omitempty"`
	Compress    bool   `yaml:"compress,omitempty"`
	MaxBackups  int    `yaml:"max_backups,omitempty"`
	MaxAge      string `yaml:"max_age,omitempty"`
	MaxTotalMB  int    `yaml:"max_total_mb,omitempty"`
}

// RotateEveryDuration returns the active-file rotation age and whether time-based
// rotation is enabled. An empty value reports (0, false, nil).
func (cc ContentLogConfig) RotateEveryDuration() (time.Duration, bool, error) {
	return parsePositiveDuration("content_log.rotate_every", cc.RotateEvery)
}

// MaxAgeDuration returns the rotated-file retention age and whether age-based
// retention is enabled. An empty value reports (0, false, nil).
func (cc ContentLogConfig) MaxAgeDuration() (time.Duration, bool, error) {
	return parsePositiveDuration("content_log.max_age", cc.MaxAge)
}

// validate surfaces malformed content-log sizing/duration fields at load time.
func (cc ContentLogConfig) validate() error {
	if cc.MaxSizeMB < 0 {
		return fmt.Errorf("agentmodel/config: content_log.max_size_mb must be >= 0, got %d", cc.MaxSizeMB)
	}
	if cc.MaxTotalMB < 0 {
		return fmt.Errorf("agentmodel/config: content_log.max_total_mb must be >= 0, got %d", cc.MaxTotalMB)
	}
	if cc.MaxBackups < 0 {
		return fmt.Errorf("agentmodel/config: content_log.max_backups must be >= 0, got %d", cc.MaxBackups)
	}
	if _, _, err := cc.RotateEveryDuration(); err != nil {
		return err
	}
	if _, _, err := cc.MaxAgeDuration(); err != nil {
		return err
	}
	return nil
}

// CacheConfig enables an opt-in response cache for non-streaming
// /v1/chat/completions. It is OFF by default (enabled:false). A bounded
// in-memory LRU is the L1; setting sqlite_path adds a persistent L2 that
// survives restarts. A cache hit returns the original response and is recorded
// in the ledger at $0 (cost_source "cache"), so it never advances a budget.
//
//	cache:
//	  enabled: true
//	  ttl: "10m"          # entry lifetime; empty/"0" = no expiry (L1 LRU-bounded)
//	  max_items: 1000     # L1 LRU capacity (default 1000)
//	  sqlite_path: ""     # optional persistent L2; empty = memory-only
type CacheConfig struct {
	Enabled    bool   `yaml:"enabled,omitempty"`
	TTL        string `yaml:"ttl,omitempty"`
	MaxItems   int    `yaml:"max_items,omitempty"`
	SQLitePath string `yaml:"sqlite_path,omitempty"`
}

// TTLDuration parses the cache entry lifetime. An empty value or "0" means no
// expiry (0, nil); any other value must be a non-negative Go duration.
func (cc CacheConfig) TTLDuration() (time.Duration, error) {
	raw := strings.TrimSpace(cc.TTL)
	if raw == "" || raw == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("agentmodel/config: cache.ttl %q: %w", cc.TTL, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("agentmodel/config: cache.ttl must be non-negative, got %q", cc.TTL)
	}
	return d, nil
}

// parsePositiveDuration parses an optional positive Go-duration config field.
// An empty value reports (0, false, nil) — the field is unset, so the feature is
// disabled or falls back to a default. A set value must parse to a positive
// duration; a non-positive or malformed value is an error tagged with label
// (e.g. "retention.period"). It is the shared core of the "omit to disable,
// else must be positive" config fields.
func parsePositiveDuration(label, raw string) (time.Duration, bool, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, false, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, false, fmt.Errorf("agentmodel/config: %s %q: %w", label, raw, err)
	}
	if d <= 0 {
		return 0, false, fmt.Errorf("agentmodel/config: %s must be positive, got %q", label, raw)
	}
	return d, true, nil
}

// PurgePeriod returns the retention window and whether auto-purge is enabled.
// An empty Period reports (0, false, nil): retention disabled, keep forever.
func (rc RetentionConfig) PurgePeriod() (time.Duration, bool, error) {
	return parsePositiveDuration("retention.period", rc.Period)
}

// PurgeInterval returns how often to run the purge, defaulting to 1h.
func (rc RetentionConfig) PurgeInterval() (time.Duration, error) {
	if strings.TrimSpace(rc.Interval) == "" {
		return time.Hour, nil
	}
	d, err := time.ParseDuration(rc.Interval)
	if err != nil {
		return 0, fmt.Errorf("agentmodel/config: retention.interval %q: %w", rc.Interval, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("agentmodel/config: retention.interval must be positive, got %q", rc.Interval)
	}
	return d, nil
}

// RouterCooldownDuration returns the configured per-deployment cooldown and
// whether it was explicitly set. An empty value returns ok=false so the router
// applies its built-in default. "0" is valid and returns (0, true) to disable
// cooling. Negative durations are rejected.
func (c *Config) RouterCooldownDuration() (d time.Duration, ok bool, err error) {
	if strings.TrimSpace(c.RouterCooldown) == "" {
		return 0, false, nil
	}
	d, err = time.ParseDuration(c.RouterCooldown)
	if err != nil {
		return 0, false, fmt.Errorf("agentmodel/config: router_cooldown %q: %w", c.RouterCooldown, err)
	}
	if d < 0 {
		return 0, false, fmt.Errorf("agentmodel/config: router_cooldown must be >= 0, got %q", c.RouterCooldown)
	}
	return d, true, nil
}

// RevalidateIntervalDuration returns the periodic re-validation interval and
// whether it was configured. An empty value returns ok=false: periodic
// re-validation is disabled and only the one-shot startup check runs. A set
// value must be a positive Go duration (mirroring retention.interval); a
// non-positive or malformed value is an error, so to disable the loop omit the
// field rather than setting it to "0".
func (c *Config) RevalidateIntervalDuration() (time.Duration, bool, error) {
	d, ok, err := parsePositiveDuration("revalidate_interval", c.RevalidateInterval)
	if err != nil {
		// Unlike router_cooldown, "0" is rejected here — nudge toward omitting.
		return 0, false, fmt.Errorf("%w (omit to disable)", err)
	}
	return d, ok, nil
}

// BudgetConfig caps gateway-wide spend (the single "default" org in the
// current single-tenant deployment; a future multi-org config nests this
// per org).
//
//	budget:
//	  max_budget: 100.0       # USD per window
//	  budget_duration: "720h" # optional; empty = lifetime cap, never resets
//
// Reset semantics: when budget_duration is set, spend is metered over fixed
// windows computed with time.Truncate(budget_duration) — boundaries fall at
// multiples of the duration since Go's zero time (0001-01-01T00:00:00Z), so
// they align to UTC midnight exactly when the duration is a multiple of 24h
// ("24h" resets at midnight UTC; "720h" boundaries are NOT month-aligned).
// budget_duration is a Go duration string; calendar tokens ("30d", "1mo",
// LiteLLM-style) do not parse today and are reserved for a future additive
// extension. An empty budget_duration makes max_budget a lifetime cap over
// the whole ledger, which is why Validate rejects it when audit-log retention
// is enabled: purged rows would silently undercount spend.
//
// Caveat: caps meter registry-priced spend only. A model missing from the
// price catalog logs cost $0 with cost_source="unpriced" and never advances
// any cap — keep the price registry in sync (cost/cmd/syncprices) and watch
// the agentmodel_cost_source_total{source="unpriced"} metric.
//
// Enforcement (#47/#52) is live on the spending endpoints; live
// used/remaining reporting on /v1/limits is #500.
type BudgetConfig struct {
	// nil = no cap. Pointer prevents silent-zero: YAML unmarshals a missing
	// field to 0 for plain float64, which is indistinguishable from "block all".
	MaxBudget      *float64 `yaml:"max_budget,omitempty"`      // USD per window
	BudgetDuration string   `yaml:"budget_duration,omitempty"` // Go duration; empty = lifetime
}

// Window returns the configured budget window and whether one was set. An
// empty budget_duration reports (0, false, nil): lifetime cap, never resets.
func (b BudgetConfig) Window() (time.Duration, bool, error) {
	return parseBudgetDuration(b.BudgetDuration, "budget")
}

// KeyConfig declares one virtual API key: a named bearer token (read from
// token_env) with optional per-key spend cap and model allowlist. The schema
// is the multi-tenancy contract: auth-by-key is wired in #52 and budget
// enforcement in #47; until then keys are validated and their caps reported
// on /v1/limits, but the gateway still authenticates only the master token.
//
//	keys:
//	  - name: "ci"
//	    token_env: "AGENT_MODEL_KEY_CI"
//	    max_budget: 10.0        # optional; USD per window
//	    budget_duration: "24h"  # optional; empty = lifetime (requires max_budget)
//	    models: ["gpt-4"]       # optional allowlist of model_names; omit = all
//
// Two contract details #52 must honor. First, models is a hard authorization
// boundary INCLUDING fallbacks: when a request from this key falls back
// (config `fallbacks`), targets outside the allowlist are skipped, never
// served — fallbacks shrink availability for a restricted key, they never
// widen access. Second, a key whose token_env is unset or empty at startup is
// disabled (fail-closed) and logged, never matched against incoming tokens.
type KeyConfig struct {
	Name           string   `yaml:"name"`
	TokenEnv       string   `yaml:"token_env"`
	MaxBudget      *float64 `yaml:"max_budget,omitempty"`
	BudgetDuration string   `yaml:"budget_duration,omitempty"`
	Models         []string `yaml:"models,omitempty"`
}

// Window returns the key's budget window; same semantics as BudgetConfig.Window.
func (k KeyConfig) Window() (time.Duration, bool, error) {
	return parseBudgetDuration(k.BudgetDuration, fmt.Sprintf("keys (%s)", k.Name))
}

func parseBudgetDuration(raw, ctx string) (time.Duration, bool, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, false, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, false, fmt.Errorf("agentmodel/config: %s: budget_duration %q: %w", ctx, raw, err)
	}
	if d <= 0 {
		return 0, false, fmt.Errorf("agentmodel/config: %s: budget_duration must be positive, got %q", ctx, raw)
	}
	return d, true, nil
}

// TelemetryConfig configures the optional observability sinks: a Prometheus
// /metrics endpoint and OTLP (OpenTelemetry) span export. Both default off.
// The OTLP exporter reads the standard OTEL_EXPORTER_OTLP_* environment
// variables (endpoint, headers, protocol).
//
//	telemetry:
//	  metrics:
//	    enabled: true        # serve GET /metrics (Prometheus)
//	  tracing:
//	    enabled: true        # export OTLP spans
//	    service_name: agentmodel
type TelemetryConfig struct {
	Metrics MetricsConfig `yaml:"metrics"`
	Tracing TracingConfig `yaml:"tracing"`
}

// MetricsConfig toggles the Prometheus /metrics endpoint.
type MetricsConfig struct {
	Enabled bool `yaml:"enabled"`
}

// TracingConfig toggles OTLP span export. ServiceName labels the exported
// spans (defaults to "agentmodel" when empty).
type TracingConfig struct {
	Enabled     bool   `yaml:"enabled"`
	ServiceName string `yaml:"service_name,omitempty"`
}

// AuthConfig holds server-side auth settings.
type AuthConfig struct {
	BearerTokenEnv string `yaml:"bearer_token_env"`
}

// ModelEntry binds one logical model_name to one or more deployments.
type ModelEntry struct {
	ModelName   string             `yaml:"model_name"`
	Deployments []DeploymentConfig `yaml:"deployments"`
}

// DeploymentConfig is one provider+model+auth combination.
//
// auth_mode is "api_key" or "subscription". For api_key, api_key_env names an
// env var holding the key (e.g. OPENAI_API_KEY). For subscription auth,
// either api_key_env points at an OAuth bearer token (Anthropic sk-ant-oat*),
// or oauth_token_dir points at a directory containing auth.json (ChatGPT
// device-code flow).
//
// For multi-account pools, set api_key_envs to a list of env var names instead
// of the single api_key_env. On a rate-limit the router cools down the
// exhausted (credential, model) pair and retries with the next credential.
// api_key_env and api_key_envs are mutually exclusive; api_key_envs takes
// precedence when both are set.
//
// Subscriptions pool the same way via oauth_token_dirs: a list of auth.json
// directories, one per logged-in account (`agent-model chatgpt-login` /
// `anthropic-login` with --token-dir or --profile writes each one). On a
// rate-limit the pool rotates to the next account's directory. Unlike
// api_key_envs, these credentials refresh themselves, so a pooled subscription
// does not go stale. oauth_token_dir and oauth_token_dirs are mutually
// exclusive, and oauth_token_dir(s) take precedence over api_key_env(s) on the
// Anthropic providers.
type DeploymentConfig struct {
	Provider       string   `yaml:"provider"`
	Model          string   `yaml:"model"`
	AuthMode       string   `yaml:"auth_mode"`
	APIKeyEnv      string   `yaml:"api_key_env,omitempty"`
	APIKeyEnvs     []string `yaml:"api_key_envs,omitempty"`
	OAuthTokenDir  string   `yaml:"oauth_token_dir,omitempty"`
	OAuthTokenDirs []string `yaml:"oauth_token_dirs,omitempty"`
	// BaseURL overrides the provider's default upstream endpoint (LiteLLM calls
	// this api_base). An absolute http(s) URL. It points the provider at a
	// drop-in OpenAI/Anthropic-compatible backend: a keyless local server
	// (Ollama/vLLM), a self-hosted mirror, or — in tests — an httptest fake
	// upstream. When set, api_key_env/api_key_envs become optional (a keyless
	// backend builds with an empty static key).
	BaseURL string `yaml:"base_url,omitempty"`
	// Azure OpenAI only (#892). APIVersion is the required Azure api-version query
	// value (e.g. 2024-10-01-preview). DeploymentName is the Azure deployment that
	// backs this model; when empty it defaults to Model. Both are ignored by every
	// other provider. For provider: azure, base_url is the resource host
	// (https://<resource>.openai.azure.com) and is required.
	APIVersion     string `yaml:"api_version,omitempty"`
	DeploymentName string `yaml:"deployment_name,omitempty"`
	// Weight is a pointer so an omitted field (nil, defaults to 1) is
	// distinguishable from an explicit weight: 0 (DISABLED — the factory drops
	// the deployment). A plain int coerced both to the same value.
	Weight *int `yaml:"weight,omitempty"`
	// CacheTTL selects the Anthropic prompt-cache TTL: "" (default 5m), "5m", or
	// "1h". Only the anthropic/anthropic-oauth providers honor it.
	CacheTTL string `yaml:"cache_ttl,omitempty"`
	// nil = no cap. Pointer prevents silent-zero: YAML unmarshals a missing
	// field to 0 for plain int, which is indistinguishable from "block all".
	RPM *int `yaml:"rpm,omitempty"` // requests per minute
	TPM *int `yaml:"tpm,omitempty"` // total tokens per minute (input + output)
}

// EffectiveWeight returns the deployment's selection weight: an omitted weight
// defaults to 1; an explicit value (including 0 = disabled) is returned as-is.
func (d DeploymentConfig) EffectiveWeight() int {
	if d.Weight == nil {
		return 1
	}
	return *d.Weight
}

// FallbackRule defines an ordered list of model_names to try when the primary
// model_name's deployments all fail with retryable errors.
type FallbackRule struct {
	ModelName  string   `yaml:"model_name"`
	FallbackTo []string `yaml:"fallback_to"`
}

// DefaultConfigPath returns the config path used when none is given on the
// command line: $AGENT_MODEL_CONFIG if set, else ~/.config/agentmodel/config.yaml.
func DefaultConfigPath() string {
	if v := os.Getenv("AGENT_MODEL_CONFIG"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".config", "agentmodel", "config.yaml")
	}
	return filepath.Join(home, ".config", "agentmodel", "config.yaml")
}

// DBPath resolves the SQLite path used ONLY when the config omits `db` (a
// config-set db is read verbatim — this changes just the code default). It
// dedups the former inline "agentmodel.db" default. Order: $AGENT_MODEL_DB
// (full-path operator override) → the canonical
// ~/.local/share/heros/agentmodel/agentmodel.db, falling back IN PLACE to an
// existing legacy DB — first the config-dir sibling
// ~/.config/agentmodel/agentmodel.db, then the historical CWD-relative
// ./agentmodel.db — so an existing install never orphans its data. A fresh
// install converges on the canonical layout.
func DBPath() string {
	if v := strings.TrimSpace(os.Getenv("AGENT_MODEL_DB")); v != "" {
		return v
	}
	return herospath.ResolveDataLegacy("", "agentmodel", "agentmodel.db",
		legacyConfigDBPath(), "agentmodel.db")
}

// legacyConfigDBPath is the pre-migration on-disk DB location that sat beside
// the config file (~/.config/agentmodel/agentmodel.db).
func legacyConfigDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".config", "agentmodel", "agentmodel.db")
	}
	return filepath.Join(home, ".config", "agentmodel", "agentmodel.db")
}

// LoadConfig reads, parses, and validates a config file. An empty path falls
// back to DefaultConfigPath.
func LoadConfig(path string) (*Config, error) {
	if path == "" {
		path = DefaultConfigPath()
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("agentmodel/config: read: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("agentmodel/config: parse: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate enforces invariants and applies defaults in place.
func (c *Config) Validate() error {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.DB == "" {
		c.DB = DBPath()
	}
	if c.Auth.BearerTokenEnv == "" {
		c.Auth.BearerTokenEnv = "AGENT_MODEL_TOKEN"
	}
	if len(c.ModelList) == 0 {
		return errors.New("agentmodel/config: model_list must be non-empty")
	}

	known := make(map[string]bool, len(c.ModelList))
	for i, m := range c.ModelList {
		if m.ModelName == "" {
			return fmt.Errorf("agentmodel/config: model_list[%d]: model_name is required", i)
		}
		if known[m.ModelName] {
			return fmt.Errorf("agentmodel/config: model_list[%d]: duplicate model_name %q", i, m.ModelName)
		}
		known[m.ModelName] = true

		if len(m.Deployments) == 0 {
			return fmt.Errorf("agentmodel/config: model_list[%d] (%s): deployments must be non-empty", i, m.ModelName)
		}

		var totalWeight int
		for j, d := range m.Deployments {
			if err := validateDeployment(d, fmt.Sprintf("model_list[%d].deployments[%d]", i, j)); err != nil {
				return err
			}
			if d.EffectiveWeight() < 0 {
				return fmt.Errorf("agentmodel/config: model_list[%d].deployments[%d]: weight must be >= 0", i, j)
			}
			totalWeight += d.EffectiveWeight() // omitted => 1; explicit 0 => 0 (disabled)
		}
		if totalWeight == 0 {
			return fmt.Errorf("agentmodel/config: model_list[%d] (%s): total weight is 0 (all deployments disabled?)", i, m.ModelName)
		}
	}

	for i, fb := range c.Fallbacks {
		if !known[fb.ModelName] {
			return fmt.Errorf("agentmodel/config: fallbacks[%d]: unknown model_name %q", i, fb.ModelName)
		}
		for j, target := range fb.FallbackTo {
			if !known[target] {
				return fmt.Errorf("agentmodel/config: fallbacks[%d].fallback_to[%d]: unknown model_name %q", i, j, target)
			}
		}
	}

	// Budget caps and virtual keys: surface a malformed contract at load
	// time, before #47/#52 start enforcing it. Durations are normalized
	// (trimmed) first so validation, Window(), and /v1/limits reporting all
	// read the same canonical string.
	c.Budget.BudgetDuration = strings.TrimSpace(c.Budget.BudgetDuration)
	for i := range c.Keys {
		c.Keys[i].BudgetDuration = strings.TrimSpace(c.Keys[i].BudgetDuration)
	}
	retention, retentionOn, err := c.Retention.PurgePeriod()
	if err != nil {
		return err
	}
	if err := validateBudgetCap(c.Budget.MaxBudget, c.Budget.BudgetDuration, retention, retentionOn, "budget"); err != nil {
		return err
	}
	// Upstream credential env vars must not double as virtual-key tokens: a
	// collision would turn a provider secret into an inbound credential.
	upstreamEnvs := make(map[string]bool)
	for _, m := range c.ModelList {
		for _, d := range m.Deployments {
			if d.APIKeyEnv != "" {
				upstreamEnvs[d.APIKeyEnv] = true
			}
			for _, e := range d.APIKeyEnvs {
				upstreamEnvs[e] = true
			}
		}
	}
	keyNames := make(map[string]bool, len(c.Keys))
	tokenEnvs := make(map[string]bool, len(c.Keys))
	for i, k := range c.Keys {
		ctx := fmt.Sprintf("keys[%d]", i)
		if k.Name == "" {
			return fmt.Errorf("agentmodel/config: %s: name is required", ctx)
		}
		if keyNames[k.Name] {
			return fmt.Errorf("agentmodel/config: %s: duplicate key name %q", ctx, k.Name)
		}
		keyNames[k.Name] = true
		if k.TokenEnv == "" {
			return fmt.Errorf("agentmodel/config: %s (%s): token_env is required", ctx, k.Name)
		}
		if tokenEnvs[k.TokenEnv] {
			return fmt.Errorf("agentmodel/config: %s (%s): duplicate token_env %q", ctx, k.Name, k.TokenEnv)
		}
		tokenEnvs[k.TokenEnv] = true
		// The master token must not double as a virtual key: the key would be
		// indistinguishable from the master and silently inherit full access.
		if k.TokenEnv == c.Auth.BearerTokenEnv {
			return fmt.Errorf("agentmodel/config: %s (%s): token_env %q collides with auth.bearer_token_env", ctx, k.Name, k.TokenEnv)
		}
		if upstreamEnvs[k.TokenEnv] {
			return fmt.Errorf("agentmodel/config: %s (%s): token_env %q collides with a deployment api_key_env/api_key_envs entry", ctx, k.Name, k.TokenEnv)
		}
		if err := validateBudgetCap(k.MaxBudget, k.BudgetDuration, retention, retentionOn, fmt.Sprintf("%s (%s)", ctx, k.Name)); err != nil {
			return err
		}
		// Distinguish omitted (nil = all models) from an explicit empty list,
		// which reads as "no models" but would silently mean "all".
		if k.Models != nil && len(k.Models) == 0 {
			return fmt.Errorf("agentmodel/config: %s (%s): models must be non-empty when set (omit the field to allow all models)", ctx, k.Name)
		}
		seenModels := make(map[string]bool, len(k.Models))
		for j, m := range k.Models {
			if !known[m] {
				return fmt.Errorf("agentmodel/config: %s (%s): models[%d]: unknown model_name %q", ctx, k.Name, j, m)
			}
			if seenModels[m] {
				return fmt.Errorf("agentmodel/config: %s (%s): models[%d]: duplicate model_name %q", ctx, k.Name, j, m)
			}
			seenModels[m] = true
		}
	}

	// Surface a malformed retention interval at load time, not at first
	// purge. (PurgePeriod was already validated above for the budget
	// cross-check.)
	if _, err := c.Retention.PurgeInterval(); err != nil {
		return err
	}

	// Surface a malformed router_cooldown at load time, not at first failover.
	if _, _, err := c.RouterCooldownDuration(); err != nil {
		return err
	}

	// Surface a malformed revalidate_interval at load time, not at first tick.
	if _, _, err := c.RevalidateIntervalDuration(); err != nil {
		return err
	}

	// Surface malformed content-log rotation/retention fields at load time.
	if err := c.ContentLog.validate(); err != nil {
		return err
	}

	return nil
}

func validateDeployment(d DeploymentConfig, ctx string) error {
	if d.Provider == "" {
		return fmt.Errorf("agentmodel/config: %s: provider is required", ctx)
	}
	if d.Model == "" {
		return fmt.Errorf("agentmodel/config: %s: model is required", ctx)
	}
	if d.BaseURL != "" {
		u, err := url.Parse(d.BaseURL)
		if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("agentmodel/config: %s: base_url must be an absolute http(s) URL, got %q", ctx, d.BaseURL)
		}
	}
	// Azure OpenAI routes to deployment-scoped paths on a resource host, so it
	// needs the resource URL and an api-version — both absent for plain OpenAI.
	if d.Provider == "azure" {
		if d.BaseURL == "" {
			return fmt.Errorf("agentmodel/config: %s: azure provider requires base_url (https://<resource>.openai.azure.com)", ctx)
		}
		if d.APIVersion == "" {
			return fmt.Errorf("agentmodel/config: %s: azure provider requires api_version (e.g. 2024-10-01-preview)", ctx)
		}
	}
	hasKey := d.APIKeyEnv != "" || len(d.APIKeyEnvs) > 0
	hasOAuthDir := d.OAuthTokenDir != "" || len(d.OAuthTokenDirs) > 0
	switch d.AuthMode {
	case "", AuthModeAPIKey:
		// A keyless backend (Ollama/vLLM or a test fake upstream) is reachable
		// only via base_url; relax the key requirement in that case. The factory
		// builds it with an empty static key (wire-harmless).
		if !hasKey && d.BaseURL == "" {
			return fmt.Errorf("agentmodel/config: %s: api_key_env or api_key_envs required for api_key auth_mode", ctx)
		}
	case AuthModeSubscription:
		// Subscription needs either an env-var OAuth token (Anthropic) or a
		// token directory (ChatGPT device-code).
		if !hasKey && !hasOAuthDir {
			return fmt.Errorf("agentmodel/config: %s: subscription auth_mode requires api_key_env, api_key_envs, oauth_token_dir, or oauth_token_dirs", ctx)
		}
		// oauth_token_dir and oauth_token_dirs are two spellings of the same
		// knob; requiring exactly one keeps "which directory wins" unambiguous.
		if d.OAuthTokenDir != "" && len(d.OAuthTokenDirs) > 0 {
			return fmt.Errorf("agentmodel/config: %s: oauth_token_dir and oauth_token_dirs are mutually exclusive", ctx)
		}
		// oauth_token_dirs is the multi-account pool for every subscription
		// provider (#58): chatgpt device-code dirs and Anthropic refreshable
		// dirs alike. The "provider supports subscription" check below is the
		// only gate — a provider that cannot do subscription auth at all is
		// rejected there with a clearer message.
		//
		// chatgpt subscription authenticates ONLY via a device-code token
		// directory; the factory never reads api_key_env for chatgpt, so a
		// key-only config would silently fall back to the default-profile store
		// instead of the intended credential. Require an oauth dir.
		if d.Provider == "chatgpt" && !hasOAuthDir {
			return fmt.Errorf("agentmodel/config: %s: chatgpt subscription requires oauth_token_dir or oauth_token_dirs (api_key_env is ignored for chatgpt)", ctx)
		}
		seenDir := make(map[string]bool, len(d.OAuthTokenDirs))
		for i, dir := range d.OAuthTokenDirs {
			if strings.TrimSpace(dir) == "" {
				return fmt.Errorf("agentmodel/config: %s: oauth_token_dirs[%d] must be non-empty (each entry is one account's auth.json directory)", ctx, i)
			}
			if seenDir[dir] {
				return fmt.Errorf("agentmodel/config: %s: oauth_token_dirs[%d]: duplicate directory %q (pooling one account twice shares its rate limit)", ctx, i, dir)
			}
			seenDir[dir] = true
		}
		// Subscription is supported by anthropic and chatgpt only in MVP.
		if d.Provider != "anthropic" && d.Provider != "anthropic-oauth" && d.Provider != "chatgpt" {
			return fmt.Errorf("agentmodel/config: %s: provider %q does not support subscription auth_mode", ctx, d.Provider)
		}
	default:
		return fmt.Errorf("agentmodel/config: %s: unknown auth_mode %q (must be api_key or subscription)", ctx, d.AuthMode)
	}
	if err := validatePositiveOptionalInt(d.RPM, "rpm", ctx); err != nil {
		return err
	}
	if err := validatePositiveOptionalInt(d.TPM, "tpm", ctx); err != nil {
		return err
	}
	// cache_ttl is Anthropic-only and must be one of the values WithCacheTTL
	// accepts (an invalid value would otherwise panic at build time). Reject it
	// on other providers rather than silently ignoring it.
	if d.CacheTTL != "" {
		if d.Provider != "anthropic" && d.Provider != "anthropic-oauth" {
			return fmt.Errorf("agentmodel/config: %s: cache_ttl is only supported by the anthropic providers", ctx)
		}
		if d.CacheTTL != "5m" && d.CacheTTL != "1h" {
			return fmt.Errorf("agentmodel/config: %s: cache_ttl must be \"5m\" or \"1h\" (empty = default 5m), got %q", ctx, d.CacheTTL)
		}
	}
	return nil
}

// validateBudgetCap checks one (max_budget, budget_duration) pair. A duration
// without a cap is rejected: it would read as a configured budget while
// capping nothing. Non-finite caps are rejected because encoding/json cannot
// marshal them — a NaN/Inf limit would blank the entire /v1/limits response.
// When audit-log retention is enabled, the budget window must fit inside the
// retention period (and a lifetime cap is rejected outright): spend older
// than retention.period is purged, so a longer window would silently
// undercount and the cap could never be enforced as documented.
func validateBudgetCap(maxBudget *float64, duration string, retention time.Duration, retentionOn bool, ctx string) error {
	if maxBudget != nil && (*maxBudget <= 0 || math.IsNaN(*maxBudget) || math.IsInf(*maxBudget, 0)) {
		return fmt.Errorf("agentmodel/config: %s: max_budget must be a finite number > 0 when set (omit the field for unlimited)", ctx)
	}
	if maxBudget == nil && duration != "" {
		return fmt.Errorf("agentmodel/config: %s: budget_duration requires max_budget", ctx)
	}
	d, windowed, err := parseBudgetDuration(duration, ctx)
	if err != nil {
		return err
	}
	if maxBudget != nil && retentionOn {
		if !windowed {
			return fmt.Errorf("agentmodel/config: %s: a lifetime cap (max_budget without budget_duration) cannot coexist with retention.period: purged spend would undercount; set a budget_duration <= retention.period or disable retention", ctx)
		}
		if d > retention {
			return fmt.Errorf("agentmodel/config: %s: budget_duration %q exceeds retention.period: spend older than retention is purged, so the cap cannot be enforced; shorten budget_duration or extend retention.period", ctx, duration)
		}
	}
	return nil
}

func validatePositiveOptionalInt(v *int, field, ctx string) error {
	if v != nil && *v <= 0 {
		return fmt.Errorf("agentmodel/config: %s: %s must be > 0 when set (omit the field for unlimited)", ctx, field)
	}
	return nil
}
