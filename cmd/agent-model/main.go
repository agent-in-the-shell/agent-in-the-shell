package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/internal/wirecontract"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/api"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/cache"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/contentlog"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/cost"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/factory"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/telemetry"
)

const usage = `agent-model: OpenAI-compatible LLM gateway.

Usage:
  agent-model serve [--config <path>]   start the HTTP server
  agent-model migrate --config <path>   apply DB migrations and exit
  agent-model purge --config <path> [--older-than <dur>]
                                       delete audit rows older than the retention window and exit
  agent-model chatgpt-login [--token-dir <path>] [--profile <name>] [--activate]
                                       run ChatGPT subscription device-code OAuth
  agent-model anthropic-login [--token-dir <path>] [--profile <name>] [--activate]
                                       run Anthropic Claude Code PKCE OAuth login
  agent-model profile <set|list|show|remove|migrate>
                                       manage Anthropic OAuth profiles
  agent-model filter [--url=...] [--model=...] "<system-prompt>"
                                       read JSONL from stdin, rewrite text field, emit JSONL
  agent-model usage [--config <path> | --db <path>] [--since 7d] [--by model] [--org default] [--json]
                                       report request_logs usage: requests, tokens, cost, error_rate
  agent-model status [--config <path>] [--json]
                                       offline health snapshot: gateway liveness, token-store
                                       freshness, and model routing; exit 1 if anything is unusable
  agent-model schema                    print the filter JSONL wire contract and exit

Environment:
  AGENT_MODEL_TOKEN          bearer token for /v1/* endpoints (required for serve)
  AGENT_MODEL_CONFIG        default config path when --config is omitted (default: ~/.config/agentmodel/config.yaml)
  OPENAI_API_KEY            OpenAI API key
  ANTHROPIC_API_KEY         Anthropic API key
  ANTHROPIC_OAUTH_TOKEN     Claude Pro/Max OAuth token (sk-ant-oat*)
  ANTHROPIC_OAUTH_TOKEN_DIR dir for Anthropic auth.json (default: ~/.config/agentmodel/anthropic)
  AGENTMODEL_PROFILE        active Anthropic profile override
  GEMINI_API_KEY            Google AI Studio API key
  DEEPSEEK_API_KEY          DeepSeek API key
  CHATGPT_TOKEN_DIR         dir for ChatGPT auth.json (default: ~/.config/agentmodel/chatgpt)
`

// pipeRecord is the minimal contract `agent-model filter` reads and writes: a
// JSONL record carrying a `text` field (all other fields pass through). It backs
// the `schema` self-description.
type pipeRecord struct {
	Text string `json:"text"`
}

// emitSchema writes the `agent-model schema` self-description. `filter` is a
// passthrough transform: it requires `text`, rewrites it, and preserves every
// other field of each record. Both sides are therefore open records (the same
// shape in and out), declared with additionalProperties so a consumer/compiler
// knows upstream fields survive the stage instead of assuming only `text`.
func emitSchema(w io.Writer) error {
	rec := wirecontract.Open(wirecontract.Reflect(pipeRecord{}))
	return wirecontract.EmitDocument(w, wirecontract.Document{InputSchema: rec, OutputSchema: rec})
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		osExit(2)
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	switch os.Args[1] {
	case "serve":
		if err := runServe(logger); err != nil {
			logger.Error("server failed", "err", err)
			osExit(1)
			return
		}
	case "migrate":
		if err := runMigrate(logger); err != nil {
			logger.Error("migrate failed", "err", err)
			osExit(1)
			return
		}
	case "purge":
		if err := runPurge(logger); err != nil {
			logger.Error("purge failed", "err", err)
			osExit(1)
			return
		}
	case "chatgpt-login":
		if err := runChatGPTLogin(logger); err != nil {
			logger.Error("chatgpt-login failed", "err", err)
			osExit(1)
			return
		}
	case "anthropic-login":
		if err := runAnthropicLogin(logger); err != nil {
			logger.Error("anthropic-login failed", "err", err)
			osExit(1)
			return
		}
	case "profile":
		if err := runProfile(); err != nil {
			logger.Error("profile failed", "err", err)
			osExit(1)
			return
		}
	case "filter":
		if err := doFilter(os.Args[2:], os.Stdin, os.Stdout); err != nil {
			logger.Error("filter failed", "err", err)
			osExit(1)
			return
		}
	case "usage":
		if err := doUsage(os.Args[2:], os.Stdout, os.Stderr); err != nil {
			logger.Error("usage failed", "err", err)
			osExit(1)
			return
		}
	case "status":
		if err := doStatus(os.Args[2:], os.Stdout, os.Stderr); err != nil {
			// A health report (if one was produced) is already on stdout; still
			// log the error so an early failure that prints no report — a bad
			// --config, say — is not a silent exit 1.
			logger.Error("status failed", "err", err)
			osExit(1)
			return
		}
	case "schema":
		// `agent-model schema` self-describes the `filter` JSONL contract.
		if err := emitSchema(os.Stdout); err != nil {
			logger.Error("schema failed", "err", err)
			osExit(1)
			return
		}
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n%s", os.Args[1], usage)
		osExit(2)
		return
	}
}

var osExit = os.Exit

// ─── serve ─────────────────────────────────────────────────────────────────

func runServe(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runServeContext(ctx, logger)
}

func runServeContext(ctx context.Context, logger *slog.Logger) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", "", "path to YAML config (default ~/.config/agentmodel/config.yaml)")
	_ = fs.Parse(os.Args[2:])
	cfg, err := agentmodel.LoadConfig(*configPath)
	if err != nil {
		return err
	}

	bearerToken := os.Getenv(cfg.Auth.BearerTokenEnv)
	if bearerToken == "" {
		return fmt.Errorf("%s is required", cfg.Auth.BearerTokenEnv)
	}

	registry, err := cost.LoadDefault()
	if err != nil {
		return fmt.Errorf("load cost registry: %w", err)
	}

	st, err := store.OpenSQLite(cfg.DB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	deployments, chatgptOAuth, err := factory.BuildDeployments(cfg, logger)
	if err != nil {
		return err
	}

	fallbacks := make(map[string][]string)
	for _, fb := range cfg.Fallbacks {
		fallbacks[fb.ModelName] = fb.FallbackTo
	}

	var ropts []router.Option
	if d, ok, err := cfg.RouterCooldownDuration(); err != nil {
		return err
	} else if ok {
		ropts = append(ropts, router.WithCooldown(d))
	}
	revalInterval, revalOn, err := cfg.RevalidateIntervalDuration()
	if err != nil {
		return err
	}
	if revalOn {
		// Cache window == re-validation period: each periodic pass refreshes.
		ropts = append(ropts, router.WithModelCacheTTL(revalInterval))
	}

	rt := router.New(deployments, fallbacks, ropts...)

	tel, err := telemetry.New(context.Background(), telemetry.Config{
		MetricsEnabled: cfg.Telemetry.Metrics.Enabled,
		TracingEnabled: cfg.Telemetry.Tracing.Enabled,
		ServiceName:    cfg.Telemetry.Tracing.ServiceName,
	})
	if err != nil {
		return fmt.Errorf("init telemetry: %w", err)
	}
	if tel != nil {
		rt.SetObserver(tel)
		logger.Info("telemetry enabled",
			"metrics", cfg.Telemetry.Metrics.Enabled, "tracing", cfg.Telemetry.Tracing.Enabled)
	}

	// Opt-in full request/response content log (off unless content_log.path is
	// set). Records raw prompts and completions — see contentlog package docs.
	// Rotation/compression/retention (#1399) are wired from config here; the
	// callbacks feed rotation success/failure and disk footprint into telemetry.
	contentLog, err := openContentLog(cfg, logger, tel)
	if err != nil {
		return fmt.Errorf("open content log: %w", err)
	}
	defer contentLog.Close()
	if contentLog.Enabled() {
		logger.Info("content logging enabled", "path", cfg.ContentLog.Path)
	}

	// Response cache (#48): opt-in, off by default. In-memory LRU L1 with an
	// optional persistent SQLite L2. Closed on shutdown so the L2 file is flushed.
	var respCache cache.Cache
	if cfg.Cache.Enabled {
		ttl, err := cfg.Cache.TTLDuration()
		if err != nil {
			return err
		}
		respCache, err = cache.New(cache.Config{
			TTL:        ttl,
			MaxItems:   cfg.Cache.MaxItems,
			SQLitePath: cfg.Cache.SQLitePath,
		})
		if err != nil {
			return fmt.Errorf("init cache: %w", err)
		}
		defer func() { _ = respCache.Close() }()
		logger.Info("response cache enabled",
			"ttl", ttl, "max_items", cfg.Cache.MaxItems, "sqlite", cfg.Cache.SQLitePath != "")
	}

	srv := api.New(api.Config{
		Router:      rt,
		Store:       st,
		Registry:    registry,
		BearerToken: bearerToken,
		Budget:      cfg.Budget,
		Keys:        cfg.Keys,
		ChatGPTAuth: chatgptOAuth,
		Logger:      logger,
		Telemetry:   tel,
		ContentLog:  contentLog,
		Cache:       respCache,
	})

	// Background audit-log retention purge — a no-op unless `retention.period`
	// is configured. Stops when serveCtx is cancelled on shutdown.
	serveCtx, cancelServe := context.WithCancel(ctx)
	defer cancelServe()
	startRetentionPurge(serveCtx, logger, st, cfg.Retention)

	// Background content-log disk-usage sampler: refreshes the footprint gauge
	// between rotations so a Prometheus alert can catch abnormal growth (#1399).
	// A no-op when content logging or metrics are off.
	startContentLogSampler(serveCtx, tel, contentLog, contentLogSampleInterval)

	// Background deployment drift re-validation. Always primes once at startup
	// (matching the historical one-shot check); when revalidate_interval is set
	// it then re-checks on each tick and logs drift appearing/resolving. Stops
	// when serveCtx is cancelled on shutdown.
	startDeploymentRevalidation(serveCtx, logger, rt, revalInterval, revalOn)

	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
	}

	idleClosed := make(chan struct{})
	go func() {
		<-serveCtx.Done()
		logger.Info("shutdown signal received")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			logger.Error("graceful shutdown failed", "err", err)
		}
		// Flush any buffered OTLP spans before exit (no-op if tracing is off).
		if err := tel.Shutdown(ctx); err != nil {
			logger.Error("telemetry shutdown failed", "err", err)
		}
		close(idleClosed)
	}()

	logger.Info("listening", "addr", cfg.Listen, "models", len(cfg.ModelList))
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-idleClosed
	return nil
}

// contentLogSampleInterval is how often the content-log footprint gauge is
// refreshed between rotations.
const contentLogSampleInterval = time.Minute

// openContentLog opens the opt-in content log with rotation, compression, and
// retention derived from config (#1399). Sizes are configured in megabytes and
// converted to bytes here; durations were validated at load time. The rotation
// callbacks feed success/failure and the post-rotation disk footprint into
// telemetry and the structured log, which is the alerting substrate. A nil tel
// (metrics off) is fine — its methods are nil-safe.
func openContentLog(cfg *agentmodel.Config, logger *slog.Logger, tel *telemetry.Telemetry) (*contentlog.Logger, error) {
	cc := cfg.ContentLog
	rotateEvery, _, _ := cc.RotateEveryDuration()
	maxAge, _, _ := cc.MaxAgeDuration()
	const mib = 1024 * 1024
	opts := contentlog.Options{
		MaxSizeBytes:  int64(cc.MaxSizeMB) * mib,
		RotateEvery:   rotateEvery,
		Compress:      cc.Compress,
		MaxBackups:    cc.MaxBackups,
		MaxAge:        maxAge,
		MaxTotalBytes: int64(cc.MaxTotalMB) * mib,
		OnRotate: func(ev contentlog.RotateEvent) {
			logger.Info("content log rotated", "backup", ev.Backup, "disk_bytes", ev.DiskBytes)
			tel.ContentLogRotation(true)
			tel.SetContentLogBytes(ev.DiskBytes)
		},
		OnError: func(err error) {
			logger.Error("content log rotation failed", "err", err)
			tel.ContentLogRotation(false)
		},
	}
	return contentlog.OpenWithOptions(cc.Path, opts)
}

// startContentLogSampler launches a background goroutine that periodically
// publishes the content log's on-disk footprint to the telemetry gauge, so
// abnormal growth is visible even between rotations. A no-op when telemetry is
// off or content logging is disabled. Stops when ctx is cancelled.
func startContentLogSampler(ctx context.Context, tel *telemetry.Telemetry, cl *contentlog.Logger, interval time.Duration) {
	if tel == nil || !cl.Enabled() {
		return
	}
	go func() {
		tel.SetContentLogBytes(cl.DiskUsage())
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				tel.SetContentLogBytes(cl.DiskUsage())
			}
		}
	}()
}

// startRetentionPurge launches a background goroutine that deletes audit rows
// older than the configured retention period, on the configured interval. It
// is a no-op when retention is disabled (empty period). The goroutine runs one
// purge immediately, then on each tick, and stops when ctx is cancelled.
func startRetentionPurge(ctx context.Context, logger *slog.Logger, st store.Store, rc agentmodel.RetentionConfig) {
	period, enabled, err := rc.PurgePeriod()
	if err != nil || !enabled {
		return // durations already validated at load time; disabled => nothing to do
	}
	interval, _ := rc.PurgeInterval()

	purge := func() {
		c, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		n, err := st.Purge(c, time.Now().Add(-period))
		if err != nil {
			logger.Error("retention purge failed", "err", err)
			return
		}
		if n > 0 {
			logger.Info("retention purge", "deleted", n, "older_than", period.String())
		}
	}

	go func() {
		logger.Info("retention enabled", "period", period.String(), "interval", interval.String())
		purge()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				purge()
			}
		}
	}()
}

// startDeploymentRevalidation launches a background goroutine that checks each
// configured deployment's model id against its provider's live model list. It
// always runs one pass immediately to prime the TTL cache and warn about
// startup drift (preserving the historical one-shot behavior). When enabled, it
// then re-runs on the given interval, logging only drift TRANSITIONS — a model
// newly missing (Warn) or a previously-drifted model that reappeared (Info) —
// so steady-state drift doesn't re-log every tick. It is best-effort and never
// blocks; the goroutine stops when ctx is cancelled.
func startDeploymentRevalidation(ctx context.Context, logger *slog.Logger, rt *router.Router, interval time.Duration, enabled bool) {
	// router.Drift is a comparable (all-string) struct, so it doubles as the
	// transition-tracking set key directly — no separate key construction.
	pass := func(prev map[router.Drift]bool) map[router.Drift]bool {
		c, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		cur := make(map[router.Drift]bool)
		for _, d := range rt.ValidateDeployments(c) {
			cur[d] = true
			if !prev[d] {
				logger.Warn("configured model not offered by its provider",
					"model_name", d.ModelName, "provider", d.Provider, "model", d.Model)
			}
		}
		for d := range prev {
			if !cur[d] {
				logger.Info("configured model drift resolved",
					"model_name", d.ModelName, "provider", d.Provider, "model", d.Model)
			}
		}
		return cur
	}

	go func() {
		prev := pass(nil) // prime the cache + warn about initial drift
		if !enabled {
			return
		}
		logger.Info("deployment re-validation enabled", "interval", interval.String())
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				prev = pass(prev)
			}
		}
	}()
}

// runPurge is the one-off `agent-model purge` command: it deletes audit rows
// older than the retention period (or --older-than override) and exits.
func runPurge(logger *slog.Logger) error {
	fs := flag.NewFlagSet("purge", flag.ExitOnError)
	configPath := fs.String("config", "", "path to YAML config (required)")
	olderThan := fs.String("older-than", "", "duration override (e.g. 720h); defaults to retention.period")
	_ = fs.Parse(os.Args[2:])
	if *configPath == "" {
		return errors.New("--config <path> is required")
	}
	cfg, err := agentmodel.LoadConfig(*configPath)
	if err != nil {
		return err
	}

	period, enabled, err := cfg.Retention.PurgePeriod()
	if err != nil {
		return err
	}
	period, enabled, err = resolvePurgePeriod(period, enabled, *olderThan)
	if err != nil {
		return err
	}
	if !enabled {
		return errors.New("no retention period configured; set retention.period or pass --older-than")
	}

	st, err := store.OpenSQLite(cfg.DB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	n, err := st.Purge(context.Background(), time.Now().Add(-period))
	if err != nil {
		return err
	}
	logger.Info("purged audit rows", "deleted", n, "older_than", period.String(), "db", cfg.DB)
	return nil
}

// resolvePurgePeriod determines the effective retention window for `purge`,
// applying the optional --older-than override on top of the configured period.
//
// The override is validated to be strictly positive, mirroring the d<=0
// invariant that RetentionConfig.PurgePeriod already enforces for the
// config-driven path. Without this guard a negative duration (time.ParseDuration
// accepts e.g. "-720h") would make the cutoff time.Now().Add(-period) a
// timestamp in the FUTURE, so `DELETE ... WHERE created_at < cutoff` would match
// and wipe every audit row; "0" would delete everything older than now.
func resolvePurgePeriod(cfgPeriod time.Duration, cfgEnabled bool, override string) (time.Duration, bool, error) {
	if override == "" {
		return cfgPeriod, cfgEnabled, nil
	}
	d, err := time.ParseDuration(override)
	if err != nil {
		return 0, false, fmt.Errorf("--older-than %q: %w", override, err)
	}
	if d <= 0 {
		return 0, false, fmt.Errorf("--older-than must be positive, got %q", override)
	}
	return d, true, nil
}

// ─── migrate ───────────────────────────────────────────────────────────────

func runMigrate(logger *slog.Logger) error {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	configPath := fs.String("config", "", "path to YAML config (required)")
	_ = fs.Parse(os.Args[2:])
	if *configPath == "" {
		return errors.New("--config <path> is required")
	}
	cfg, err := agentmodel.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	st, err := store.OpenSQLite(cfg.DB)
	if err != nil {
		return err
	}
	if err := st.Close(); err != nil {
		return err
	}
	logger.Info("schema applied", "db", cfg.DB)
	return nil
}

// ─── chatgpt-login ─────────────────────────────────────────────────────────

// oauthLoginSpec captures the provider-specific pieces of an OAuth login;
// runOAuthLogin supplies the shared scaffold (flag parsing, profile
// resolution, --activate, and success reporting).
type oauthLoginSpec struct {
	provider    string                                                         // "chatgpt" / "anthropic", used for command + log names
	resolve     func(profileFlag, tokenDirFlag string) (string, string, error) // -> tokenDir, profile, err
	login       func(ctx context.Context, tokenDir string) error               // run the provider's OAuth flow
	writeActive func(profile string) error                                     // persist the active profile on --activate
}

func runOAuthLogin(logger *slog.Logger, spec oauthLoginSpec) error {
	fs := flag.NewFlagSet(spec.provider+"-login", flag.ExitOnError)
	tokenDir := fs.String("token-dir", "", "directory for auth.json; overrides profile resolution")
	profile := fs.String("profile", "", spec.provider+" profile name (default: active profile, then default)")
	activate := fs.Bool("activate", false, "make this profile active after a successful login")
	_ = fs.Parse(os.Args[2:])

	resolvedTokenDir, resolvedProfile, err := spec.resolve(*profile, *tokenDir)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	if err := spec.login(ctx, resolvedTokenDir); err != nil {
		return err
	}
	if *activate && resolvedProfile != "" {
		if err := spec.writeActive(resolvedProfile); err != nil {
			return err
		}
	}
	fmt.Println("✅ Logged in. Token stored.")
	logger.Info(spec.provider + " login complete")
	return nil
}

func runChatGPTLogin(logger *slog.Logger) error {
	return runOAuthLogin(logger, oauthLoginSpec{
		provider:    "chatgpt",
		resolve:     auth.ResolveChatGPTProfileTokenDir,
		writeActive: auth.WriteActiveChatGPTProfile,
		login: func(ctx context.Context, tokenDir string) error {
			a := auth.NewChatGPTOAuth(tokenDir, nil)
			challenge, err := a.LoginDeviceCode(ctx)
			if err != nil {
				return fmt.Errorf("device code start: %w", err)
			}
			fmt.Printf("🔑 Opening browser — visit %s and enter code: %s\n", challenge.VerificationURI, challenge.UserCode)
			fmt.Printf("   (waiting for confirmation, will time out in %d minutes...)\n", challenge.ExpiresIn/60)
			auth.OpenBrowser(challenge.VerificationURI)
			if err := a.PollDeviceCode(ctx, *challenge); err != nil {
				return fmt.Errorf("device code poll: %w", err)
			}
			return nil
		},
	})
}

// ─── anthropic-login ───────────────────────────────────────────────────────

func runAnthropicLogin(logger *slog.Logger) error {
	return runOAuthLogin(logger, oauthLoginSpec{
		provider:    "anthropic",
		resolve:     auth.ResolveProfileTokenDir,
		writeActive: auth.WriteActiveProfile,
		login: func(ctx context.Context, tokenDir string) error {
			return auth.NewAnthropicLogin(tokenDir, nil).Run(ctx)
		},
	})
}

// ─── profile ───────────────────────────────────────────────────────────────

func runProfile() error {
	if len(os.Args) < 3 {
		return errors.New("usage: agent-model profile <set|list|show|remove|migrate>")
	}
	switch os.Args[2] {
	case "set":
		return runProfileSet(os.Args[3:])
	case "list":
		return runProfileList(os.Args[3:])
	case "show":
		return runProfileShow(os.Args[3:])
	case "remove":
		return runProfileRemove(os.Args[3:])
	case "migrate":
		return runProfileMigrate(os.Args[3:])
	default:
		return fmt.Errorf("unknown profile command: %s", os.Args[2])
	}
}

// runProfileMigrate moves the legacy <base>/auth.json into the new
// <base>/default/auth.json layout, making tokenDirForProfile("default")
// resolution deterministic (no more conditional alias on filesystem state).
// Idempotent: a no-op when the legacy file is already absent.
//
// Flags:
//
//	--dry-run   describe what would happen without touching the filesystem.
func runProfileMigrate(args []string) error {
	fs := flag.NewFlagSet("profile migrate", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "describe the planned migration without renaming anything")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Reject stray positional args — e.g. `profile migrate --dryrun` (typo)
	// would otherwise leave dryRun=false and silently run the real migration.
	if fs.NArg() > 0 {
		return fmt.Errorf("profile migrate: unexpected argument %q (only flags are accepted)", fs.Arg(0))
	}
	status, err := auth.CheckLegacyDefault()
	if err != nil {
		return err
	}
	if !status.LegacyExists {
		fmt.Printf("Nothing to migrate: no legacy %s on disk.\n", status.LegacyPath)
		return nil
	}
	if status.NewExists {
		return fmt.Errorf("both %s and %s exist; refusing to overwrite — remove one manually", status.LegacyPath, status.NewPath)
	}
	if *dryRun {
		fmt.Printf("Would rename %s → %s\n", status.LegacyPath, status.NewPath)
		fmt.Println("(dry-run; not modifying the filesystem)")
		return nil
	}
	fmt.Printf("Renaming %s → %s\n", status.LegacyPath, status.NewPath)
	if _, err := auth.MigrateLegacyDefault(); err != nil {
		return err
	}
	fmt.Println("Migrated. The legacy single-store layout is now the 'default' profile.")
	fmt.Println("Note: os.Rename moves a symlink rather than its target — if your")
	fmt.Println("auth.json was a symlink, the symlink itself now lives at the new")
	fmt.Println("path. Update any tools that read the old path directly.")
	return nil
}

func runProfileSet(args []string) error {
	name, force, err := parseProfileSetArgs(args)
	if err != nil {
		return err
	}
	if err := auth.ValidateProfileName(name); err != nil {
		return err
	}
	if !force {
		dir, err := auth.ProfileTokenDir(name)
		if err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(dir, "auth.json")); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("profile %q has no auth.json; rerun with --force to activate anyway", name)
			}
			return err
		}
	}
	if err := auth.WriteActiveProfile(name); err != nil {
		return err
	}
	fmt.Printf("Active Anthropic profile: %s\n", name)
	return nil
}

func runProfileList(args []string) error {
	fs := flag.NewFlagSet("profile list", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print JSON")
	_ = fs.Parse(args)
	profiles, err := auth.ListProfiles()
	if err != nil {
		return err
	}
	active, err := auth.ResolveAnthropicProfile("")
	if err != nil {
		return err
	}
	if *jsonOut {
		rows := make([]map[string]any, 0, len(profiles))
		for _, name := range profiles {
			rows = append(rows, map[string]any{"name": name, "active": name == active})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	for _, name := range profiles {
		marker := " "
		if name == active {
			marker = "*"
		}
		fmt.Printf("%s %s\n", marker, name)
	}
	return nil
}

func runProfileShow(args []string) error {
	fs := flag.NewFlagSet("profile show", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print JSON")
	_ = fs.Parse(args)
	name, err := auth.ResolveAnthropicProfile("")
	if err != nil {
		return err
	}
	dir, err := auth.ProfileTokenDir(name)
	if err != nil {
		return err
	}
	exists := false
	if _, err := os.Stat(filepath.Join(dir, "auth.json")); err == nil {
		exists = true
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"profile": name, "token_dir": dir, "has_auth": exists})
	}
	fmt.Printf("%s\n", name)
	if !exists {
		return fmt.Errorf("profile %q has no auth.json", name)
	}
	return nil
}

func runProfileRemove(args []string) error {
	name, force, missingOK, err := parseProfileRemoveArgs(args)
	if err != nil {
		return err
	}
	if err := auth.ValidateProfileName(name); err != nil {
		return err
	}
	active, err := auth.ResolveAnthropicProfile("")
	if err != nil {
		return err
	}
	if active == name && !force {
		return fmt.Errorf("profile %q is active; rerun with --force to remove it", name)
	}
	dir, err := auth.ProfileTokenDir(name)
	if err != nil {
		return err
	}
	// Legacy-default safety: when ProfileTokenDir("default") resolves to the
	// shared base dir (because a legacy <base>/auth.json exists alongside
	// other profile subdirs), os.RemoveAll(dir) would also wipe every
	// sibling profile. Detect that case and remove only the legacy
	// <base>/auth.json file.
	legacyDefault := name == auth.DefaultProfile && filepath.Base(dir) != auth.DefaultProfile
	target := dir
	if legacyDefault {
		target = filepath.Join(dir, "auth.json")
	}
	if _, statErr := os.Stat(target); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			if missingOK {
				if active == name {
					return auth.ClearActiveProfile()
				}
				return nil
			}
			return fmt.Errorf("profile %q not found", name)
		}
		// Surface real Stat failures (EACCES, ELOOP, …) instead of letting
		// them silently fall through into the remove call.
		return statErr
	}
	if legacyDefault {
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else if err := os.RemoveAll(target); err != nil {
		return err
	}
	if active == name {
		if err := auth.ClearActiveProfile(); err != nil {
			return err
		}
	}
	fmt.Printf("Removed Anthropic profile: %s\n", name)
	return nil
}

func parseProfileSetArgs(args []string) (name string, force bool, err error) {
	for _, arg := range args {
		switch arg {
		case "--force":
			force = true
		default:
			if len(arg) > 0 && arg[0] == '-' {
				return "", false, fmt.Errorf("unknown profile set flag %q", arg)
			}
			if name != "" {
				return "", false, errors.New("usage: agent-model profile set <name> [--force]")
			}
			name = arg
		}
	}
	if name == "" {
		return "", false, errors.New("usage: agent-model profile set <name> [--force]")
	}
	return name, force, nil
}

func parseProfileRemoveArgs(args []string) (name string, force bool, missingOK bool, err error) {
	for _, arg := range args {
		switch arg {
		case "--force":
			force = true
		case "--missing-ok":
			missingOK = true
		default:
			if len(arg) > 0 && arg[0] == '-' {
				return "", false, false, fmt.Errorf("unknown profile remove flag %q", arg)
			}
			if name != "" {
				return "", false, false, errors.New("usage: agent-model profile remove <name> [--force] [--missing-ok]")
			}
			name = arg
		}
	}
	if name == "" {
		return "", false, false, errors.New("usage: agent-model profile remove <name> [--force] [--missing-ok]")
	}
	return name, force, missingOK, nil
}
