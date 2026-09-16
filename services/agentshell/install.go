package agentshell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// InstallStatus is the health of one backend, modeling the two-phase
// bootstrap problem (binary-on-PATH, then authed/usable) as more than a
// simple installed/missing boolean.
type InstallStatus string

const (
	// StatusReady: binary present and the backend reports (or heuristically
	// appears) authenticated/usable. May carry an "(unverified)" detail when
	// the signal is a credential heuristic rather than a real status check.
	StatusReady InstallStatus = "ready"
	// StatusSetupNeeded: binary present but not authenticated/configured. The
	// install path can only *guide* this (interactive browser OAuth).
	StatusSetupNeeded InstallStatus = "setup-needed"
	// StatusMissing: binary not on PATH.
	StatusMissing InstallStatus = "missing"
	// StatusOutdated: installed but below a known floor. Reserved; not asserted
	// by v1 probes (no version-floor table yet). A warning, not a failure,
	// unless --strict promotes it.
	StatusOutdated InstallStatus = "outdated"
	// StatusUnsupported: no installer by design (a backend that builds from
	// depends on a sibling service). A first-class, non-failing state.
	StatusUnsupported InstallStatus = "unsupported"
	// StatusProbeFailure: the probe itself could not determine state (timeout or
	// unexpected error), distinct from a backend that ran and reported not-authed.
	StatusProbeFailure InstallStatus = "unknown-probe-failure"
)

// sysexits-based exit codes (see man sysexits.h). The install/doctor path emits
// these from an agent-shell-originated namespace that is orthogonal to the
// child-process exit-code passthrough used by submit/run.
const (
	ExitOK          = 0  // ready / unsupported / outdated (non-strict)
	ExitUsage       = 64 // EX_USAGE: bad invocation
	ExitUnavailable = 69 // EX_UNAVAILABLE: a backend is missing
	ExitSoftware    = 70 // EX_SOFTWARE: probe failure (or outdated under --strict)
	ExitConfig      = 78 // EX_CONFIG: a backend needs setup/auth
)

const defaultAuthProbeTimeout = 3 * time.Second

// Lookup injects the OS-touching primitives so probes run offline and
// deterministically under test. The zero value is unusable; use DefaultLookup.
type Lookup struct {
	// LookPath resolves a binary on PATH (exec.LookPath).
	LookPath func(string) (string, error)
	// Run executes name with args under ctx and returns combined output plus the
	// process exit code. A non-nil error means the command could not be run at
	// all (binary vanished, timeout); in that case code is -1. A nil error with
	// a nonzero code means the command ran and reported that code — the normal
	// "not authenticated" signal for status subcommands.
	Run func(ctx context.Context, name string, args ...string) (out string, code int, err error)
	// Getenv reads an environment variable (os.Getenv).
	Getenv func(string) string
	// FileExists reports whether a path exists (os.Stat, swallowing the error).
	FileExists func(string) bool
	// HomeDir returns the user home directory (os.UserHomeDir).
	HomeDir func() (string, error)
}

// DefaultLookup returns a Lookup backed by the real OS.
func DefaultLookup() Lookup {
	return Lookup{
		LookPath: exec.LookPath,
		Run:      runProbe,
		Getenv:   os.Getenv,
		FileExists: func(path string) bool {
			_, err := os.Stat(path)
			return err == nil
		},
		HomeDir: os.UserHomeDir,
	}
}

func runProbe(ctx context.Context, name string, args ...string) (string, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	if err == nil {
		return buf.String(), 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// Ran to completion with a nonzero status: a real signal, not a failure.
		return buf.String(), exitErr.ExitCode(), nil
	}
	// Could not run (not found, timeout, permission): caller falls back.
	return buf.String(), -1, err
}

// Backend describes one installable/checkable agent CLI. Binary is sourced from
// defaultExecutable(Name) so the registry can never drift from the execution
// adapters.
type Backend struct {
	Name        string
	Binary      string
	InstallHint string
	// Supported is false for backends with no installer by design — one that
	// builds from source rather than shipping an installable artifact.
	//
	// All current backends are supported; retain this field for callers that
	// need to describe a backend with no installer.
	// TestCheckBackendUnsupportedBackendIsNotAFailure drives the branch through
	// a fixture entry so "dormant" cannot decay into "broken".
	Supported bool
	probe     func(ctx context.Context, b Backend, lk Lookup) (InstallStatus, string)
}

// backendRegistry is populated in init() and keyed by canonical agent name. The
// set of names is taken from SupportedAgents() so the doctor covers exactly the
// backends agent-shell can run.
var backendRegistry = map[string]Backend{}

// backendOrder preserves a stable, deterministic reporting order.
var backendOrder []string

func init() {
	meta := map[string]struct {
		hint      string
		supported bool
		probe     func(ctx context.Context, b Backend, lk Lookup) (InstallStatus, string)
	}{
		AgentClaude: {
			hint:      "curl -fsSL https://claude.ai/install.sh | bash  (or: brew install --cask claude-code)",
			supported: true,
			probe:     probeClaude,
		},
		AgentCodex: {
			hint:      "npm install -g @openai/codex  (or: brew install --cask codex)",
			supported: true,
			probe:     probeCodex,
		},
		AgentGemini: {
			hint:      "npm install -g @google/gemini-cli  (or: brew install gemini-cli)",
			supported: true,
			probe:     probeGemini,
		},
		AgentAntigravity: {
			hint:      "curl -fsSL https://antigravity.google/cli/install.sh | bash",
			supported: true,
			probe:     probeAntigravity,
		},
		AgentCursor: {
			hint:      "curl https://cursor.com/install -fsS | bash",
			supported: true,
			probe:     probeCursor,
		},
	}

	for _, name := range SupportedAgents() {
		m, ok := meta[name]
		if !ok {
			continue
		}
		backendRegistry[name] = Backend{
			Name:        name,
			Binary:      defaultExecutable(name),
			InstallHint: m.hint,
			Supported:   m.supported,
			probe:       m.probe,
		}
		backendOrder = append(backendOrder, name)
	}
}

// BackendNames returns the canonical names the install/doctor command covers,
// in stable reporting order.
func BackendNames() []string {
	out := make([]string, len(backendOrder))
	copy(out, backendOrder)
	return out
}

// InstallHint returns the human-facing install guidance for a backend.
func InstallHint(name string) (string, bool) {
	b, ok := backendRegistry[normalizeAgent(name)]
	if !ok {
		return "", false
	}
	return b.InstallHint, true
}

// BackendCheck is the per-backend doctor result.
type BackendCheck struct {
	Agent        string        `json:"agent"`
	Binary       string        `json:"binary"`
	ResolvedPath string        `json:"resolved_path,omitempty"`
	Status       InstallStatus `json:"status"`
	Detail       string        `json:"detail,omitempty"`
	InstallHint  string        `json:"install_hint,omitempty"`
	ExitCategory int           `json:"exit_category"`
}

// Aggregate summarizes a multi-backend check.
type Aggregate struct {
	Counts      map[InstallStatus]int `json:"counts"`
	WorstStatus InstallStatus         `json:"worst_status"`
	ExitCode    int                   `json:"exit_code"`
}

// InstallCheckReport is the full doctor output; Backends is always an array,
// even for a single target, matching the existing --json conventions.
type InstallCheckReport struct {
	Backends  []BackendCheck `json:"backends"`
	Aggregate Aggregate      `json:"aggregate"`
}

// resolveBackendExec resolves a backend's executable the SAME way run does
// (agentshell.resolveAgentExec): a PathEnvVar override, then the deploy binary
// over a stale PATH shadow, else the bare name. Routing the doctor probe through
// it stops doctor from green-lighting a shadowed binary that diverges from what
// run would spawn. Falls back to b.Binary when no supportedAgent matches (every
// registered backend is sourced from SupportedAgents(), so this is defensive).
func resolveBackendExec(b Backend) string {
	for _, sa := range supportedAgents {
		if sa.Name == b.Name {
			return resolveAgentExec(sa)
		}
	}
	return b.Binary
}

// CheckBackend resolves and probes a single backend. An unknown name yields a
// probe-failure check rather than a panic, so callers can surface a clear error.
func CheckBackend(ctx context.Context, name string, lk Lookup) BackendCheck {
	canonical := normalizeAgent(name)
	b, ok := backendRegistry[canonical]
	if !ok {
		return BackendCheck{
			Agent:        canonical,
			Status:       StatusProbeFailure,
			Detail:       fmt.Sprintf("unsupported agent %q (supported: %v)", name, BackendNames()),
			ExitCategory: ExitSoftware,
		}
	}

	check := BackendCheck{
		Agent:       b.Name,
		Binary:      b.Binary,
		InstallHint: b.InstallHint,
	}

	// Resolve the executable the SAME way run does (resolveAgentExec): a
	// PathEnvVar override, then the deploy binary ($HEROS_BIN_DIR/<name>) over a
	// stale PATH shadow, else the bare name. A bare LookPath here could green-light
	// a PATH-shadowed binary that diverges from what run would actually spawn.
	exe := resolveBackendExec(b)
	// The auth probe below runs lk.Run(b.Binary, ...), so point b.Binary at the
	// resolved executable — the probe must hit the same binary run does.
	b.Binary = exe

	var path string
	found := false
	if filepath.IsAbs(exe) {
		// An absolute path from $HEROS_BIN_DIR or a PathEnvVar override: stat it
		// directly. LookPath is for PATH-resolving a bare name, not verifying a
		// fully-qualified one.
		if lk.FileExists != nil && lk.FileExists(exe) {
			path, found = exe, true
		}
	} else if p, err := lk.LookPath(exe); err == nil {
		path, found = p, true
	}
	if !found {
		if b.Supported {
			check.Status = StatusMissing
			check.Detail = missingExecutableError(exe)
		} else {
			// A by-design-uninstallable backend missing is not a failure.
			check.Status = StatusUnsupported
			check.Detail = "not installed; build from source — " + b.InstallHint
		}
		check.ExitCategory = exitCodeForStatus(check.Status, false)
		return check
	}
	check.ResolvedPath = path

	status, detail := b.probe(ctx, b, lk)
	check.Status = status
	check.Detail = detail
	check.ExitCategory = exitCodeForStatus(status, false)
	return check
}

// CheckBackends probes every requested backend (best-effort, never fail-fast)
// and aggregates by rank-max severity. An empty names slice checks all.
func CheckBackends(ctx context.Context, names []string, strict bool, lk Lookup) InstallCheckReport {
	if len(names) == 0 {
		names = BackendNames()
	}
	report := InstallCheckReport{
		Backends: make([]BackendCheck, 0, len(names)),
	}
	for _, name := range names {
		report.Backends = append(report.Backends, CheckBackend(ctx, name, lk))
	}
	report.Aggregate = aggregate(report.Backends, strict)
	return report
}

// aggregate reduces per-backend checks to the highest-severity status (rank-max,
// NOT numeric-max over exit codes — setup-needed(78) must not mask missing(69))
// and the exit code that status maps to.
func aggregate(checks []BackendCheck, strict bool) Aggregate {
	agg := Aggregate{Counts: map[InstallStatus]int{}}
	worst := StatusUnsupported
	worstRank := -1
	for _, c := range checks {
		agg.Counts[c.Status]++
		if r := severityRank(c.Status); r > worstRank {
			worstRank = r
			worst = c.Status
		}
	}
	if len(checks) == 0 {
		worst = StatusUnsupported
	}
	agg.WorstStatus = worst
	agg.ExitCode = exitCodeForStatus(worst, strict)
	return agg
}

// severityRank orders statuses for --all aggregation. Higher wins.
func severityRank(s InstallStatus) int {
	switch s {
	case StatusProbeFailure:
		return 5
	case StatusMissing:
		return 4
	case StatusSetupNeeded:
		return 3
	case StatusOutdated:
		return 2
	case StatusReady:
		return 1
	case StatusUnsupported:
		return 0
	default:
		return 5 // treat the unknown as worst, fail loud
	}
}

// exitCodeForStatus maps a status to its sysexits code. outdated is a warning
// (exit 0) unless --strict promotes it to a software failure.
func exitCodeForStatus(s InstallStatus, strict bool) int {
	switch s {
	case StatusReady, StatusUnsupported:
		return ExitOK
	case StatusOutdated:
		if strict {
			return ExitSoftware
		}
		return ExitOK
	case StatusSetupNeeded:
		return ExitConfig
	case StatusMissing:
		return ExitUnavailable
	case StatusProbeFailure:
		return ExitSoftware
	default:
		return ExitSoftware
	}
}

// probeContext bounds an auth probe so a hung status subcommand can't wedge the
// doctor.
func probeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, defaultAuthProbeTimeout)
}

// probeClaude uses `claude auth status` (documented exit 0 when authenticated),
// degrading to a credential heuristic when the subcommand can't be run (older
// binary, timeout).
func probeClaude(ctx context.Context, b Backend, lk Lookup) (InstallStatus, string) {
	pctx, cancel := probeContext(ctx)
	defer cancel()
	_, code, err := lk.Run(pctx, b.Binary, "auth", "status")
	if err == nil {
		if code == 0 {
			return StatusReady, "authenticated (claude auth status)"
		}
		return StatusSetupNeeded, "not authenticated — run `claude` to sign in (browser OAuth)"
	}
	// Fallback: credential heuristic (cannot prove validity).
	if envSet(lk, "ANTHROPIC_API_KEY") {
		return StatusReady, "ANTHROPIC_API_KEY set (unverified — no status subcommand)"
	}
	if homeFileExists(lk, ".claude.json") {
		return StatusReady, "~/.claude.json present (unverified — no status subcommand)"
	}
	return StatusSetupNeeded, "could not verify auth; run `claude` to sign in"
}

// probeCodex uses `codex login status` (documented exit 0 when logged in) and
// warns on the OPENAI_API_KEY-shadows-OAuth footgun (openai/codex#15151).
func probeCodex(ctx context.Context, b Backend, lk Lookup) (InstallStatus, string) {
	advisory := ""
	if envSet(lk, "OPENAI_API_KEY") && homeFileExists(lk, ".codex/auth.json") {
		advisory = " (note: OPENAI_API_KEY shadows stored OAuth — codex#15151)"
	}
	pctx, cancel := probeContext(ctx)
	defer cancel()
	_, code, err := lk.Run(pctx, b.Binary, "login", "status")
	if err == nil {
		if code == 0 {
			return StatusReady, "logged in (codex login status)" + advisory
		}
		return StatusSetupNeeded, "not logged in — run `codex login`" + advisory
	}
	if envSet(lk, "OPENAI_API_KEY") {
		return StatusReady, "OPENAI_API_KEY set (unverified — no status subcommand)"
	}
	if homeFileExists(lk, ".codex/auth.json") {
		return StatusReady, "~/.codex/auth.json present (unverified — no status subcommand)" + advisory
	}
	return StatusSetupNeeded, "could not verify auth; run `codex login`"
}

// probeGemini has no non-interactive status command, so it reads env vars and
// cached OAuth credentials and biases honestly toward "unverified"/setup-needed
// rather than over-claiming ready.
func probeGemini(ctx context.Context, b Backend, lk Lookup) (InstallStatus, string) {
	for _, key := range []string{"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS"} {
		if envSet(lk, key) {
			return StatusReady, key + " set (unverified — gemini has no non-interactive auth check)"
		}
	}
	if homeFileExists(lk, ".gemini/oauth_creds.json") {
		return StatusReady, "~/.gemini/oauth_creds.json present (unverified — gemini has no non-interactive auth check)"
	}
	return StatusSetupNeeded, "no API key or cached OAuth found — run `gemini` to authenticate"
}

// probeAntigravity checks the OAuth-only agy auth state when the CLI exposes a
// status command, falling back to setup guidance rather than claiming readiness
// from undocumented keychain internals.
func probeAntigravity(ctx context.Context, b Backend, lk Lookup) (InstallStatus, string) {
	pctx, cancel := probeContext(ctx)
	defer cancel()
	_, code, err := lk.Run(pctx, b.Binary, "auth", "status")
	if err == nil {
		if code == 0 {
			return StatusReady, "authenticated (agy auth status)"
		}
		return StatusSetupNeeded, "not authenticated — run `agy auth login`"
	}
	return StatusSetupNeeded, "could not verify auth — run `agy auth login`"
}

// probeCursor likewise has no documented non-interactive auth gate; mirror the
// gemini heuristic (env key → unverified ready, else setup-needed).
func probeCursor(ctx context.Context, b Backend, lk Lookup) (InstallStatus, string) {
	if envSet(lk, "CURSOR_API_KEY") {
		return StatusReady, "CURSOR_API_KEY set (unverified — no non-interactive auth check)"
	}
	return StatusSetupNeeded, "could not verify auth — run `cursor-agent login`"
}

func envSet(lk Lookup, key string) bool {
	return len(trimmedEnv(lk, key)) > 0
}

func trimmedEnv(lk Lookup, key string) string {
	if lk.Getenv == nil {
		return ""
	}
	return strings.TrimSpace(lk.Getenv(key))
}

func homeFileExists(lk Lookup, rel string) bool {
	if lk.HomeDir == nil || lk.FileExists == nil {
		return false
	}
	home, err := lk.HomeDir()
	if err != nil || home == "" {
		return false
	}
	return lk.FileExists(filepath.Join(home, rel))
}

// InstallOptions configures Install.
type InstallOptions struct {
	// Agents is the resolved target list; empty means all backends.
	Agents []string
	// CheckOnly makes the run a pure read-only doctor (no mutation).
	CheckOnly bool
	// DryRun prints what would be done without executing installs.
	DryRun bool
	// Strict promotes outdated to a failing exit code.
	Strict bool
	// Yes permits actually executing install commands for missing backends.
	// Without it (the default), install only guides.
	Yes bool
}

// InstallAction records one remediation step Install took or would take.
type InstallAction struct {
	Agent     string `json:"agent"`
	Action    string `json:"action"` // "noop" | "guide" | "install"
	Command   string `json:"command,omitempty"`
	Executed  bool   `json:"executed"`
	Succeeded bool   `json:"succeeded,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// InstallReport is the result of Install: the doctor report plus any actions.
type InstallReport struct {
	InstallCheckReport
	Actions []InstallAction `json:"actions,omitempty"`
}

// Install runs the doctor and, unless CheckOnly, remediates the binary half of
// missing backends — guiding by default, executing the install command only
// under Yes. The interactive auth half is always left as guidance. After any
// mutation it re-checks so the returned report reflects the new state.
func Install(ctx context.Context, opts InstallOptions, lk Lookup) InstallReport {
	report := InstallReport{
		InstallCheckReport: CheckBackends(ctx, opts.Agents, opts.Strict, lk),
	}
	if opts.CheckOnly {
		return report
	}

	mutated := false
	for _, check := range report.Backends {
		switch check.Status {
		case StatusMissing:
			action := InstallAction{Agent: check.Agent, Command: check.InstallHint}
			switch {
			case opts.DryRun:
				action.Action = "guide"
				action.Detail = "dry-run: would suggest the install command"
			case opts.Yes && check.InstallHint != "":
				action.Action = "install"
				out, code, err := runInstallCommand(ctx, check.InstallHint, lk)
				action.Executed = true
				action.Succeeded = err == nil && code == 0
				if !action.Succeeded {
					action.Detail = installFailureDetail(out, code, err)
				}
				mutated = true
			default:
				action.Action = "guide"
				action.Detail = "re-run with --yes to execute the install command"
			}
			report.Actions = append(report.Actions, action)
		case StatusSetupNeeded:
			report.Actions = append(report.Actions, InstallAction{
				Agent:   check.Agent,
				Action:  "guide",
				Detail:  check.Detail,
				Command: "",
			})
		}
	}

	if mutated {
		report.InstallCheckReport = CheckBackends(ctx, opts.Agents, opts.Strict, lk)
	}
	return report
}

// runInstallCommand executes a backend's install hint via the system shell. The
// hint is a vendor-documented one-liner (it may contain pipes/&&), so it is run
// through `sh -c`. Only ever reached under --yes.
func runInstallCommand(ctx context.Context, hint string, lk Lookup) (string, int, error) {
	// Strip a trailing "(or: ...)" alternative; run the recommended first form.
	cmd := primaryInstallCommand(hint)
	shell := "sh"
	if runtime.GOOS == "windows" {
		shell = "cmd"
	}
	if runtime.GOOS == "windows" {
		return lk.Run(ctx, shell, "/c", cmd)
	}
	return lk.Run(ctx, shell, "-c", cmd)
}

// primaryInstallCommand extracts the recommended command from a hint that may
// carry a parenthetical fallback, e.g. "curl ... | bash  (or: brew ...)".
func primaryInstallCommand(hint string) string {
	if i := strings.Index(hint, "(or:"); i >= 0 {
		return strings.TrimSpace(hint[:i])
	}
	return strings.TrimSpace(hint)
}

func installFailureDetail(out string, code int, err error) string {
	if err != nil {
		return fmt.Sprintf("install command failed to run: %v", err)
	}
	msg := firstLine(strings.TrimSpace(out))
	if msg != "" {
		return fmt.Sprintf("install command exited %d: %s", code, msg)
	}
	return fmt.Sprintf("install command exited %d", code)
}

// SortedStatusCounts returns the status counts in a stable, presentation-ready
// order (worst-first) so report rendering is deterministic.
func SortedStatusCounts(counts map[InstallStatus]int) []struct {
	Status InstallStatus
	Count  int
} {
	out := make([]struct {
		Status InstallStatus
		Count  int
	}, 0, len(counts))
	for s, n := range counts {
		out = append(out, struct {
			Status InstallStatus
			Count  int
		}{s, n})
	}
	sort.Slice(out, func(i, j int) bool {
		ri, rj := severityRank(out[i].Status), severityRank(out[j].Status)
		if ri != rj {
			return ri > rj
		}
		return out[i].Status < out[j].Status
	})
	return out
}
