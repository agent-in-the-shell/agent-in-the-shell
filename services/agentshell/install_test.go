package agentshell

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeLookup builds a Lookup whose primitives are driven by in-memory maps so
// probes run offline and deterministically (mirrors the arg-level BuildCommand
// tests).
type fakeLookup struct {
	present map[string]string  // binary -> resolved path (absent = not on PATH)
	runs    map[string]fakeRun // binary -> canned status-subcommand result
	env     map[string]string  // env var -> value
	files   map[string]bool    // absolute path -> exists
	home    string             // home dir (empty -> error)
}

type fakeRun struct {
	out  string
	code int
	err  error
}

func (f fakeLookup) lookup() Lookup {
	return Lookup{
		LookPath: func(name string) (string, error) {
			if p, ok := f.present[name]; ok {
				return p, nil
			}
			return "", errors.New("not found")
		},
		Run: func(ctx context.Context, name string, args ...string) (string, int, error) {
			if r, ok := f.runs[name]; ok {
				return r.out, r.code, r.err
			}
			// No canned result: behave like a binary that can't run the probe.
			return "", -1, errors.New("no canned run")
		},
		Getenv:     func(key string) string { return f.env[key] },
		FileExists: func(path string) bool { return f.files[path] },
		HomeDir: func() (string, error) {
			if f.home == "" {
				return "", errors.New("no home")
			}
			return f.home, nil
		},
	}
}

func TestExitCodeForStatus(t *testing.T) {
	tests := []struct {
		status InstallStatus
		strict bool
		want   int
	}{
		{StatusReady, false, ExitOK},
		{StatusUnsupported, false, ExitOK},
		{StatusOutdated, false, ExitOK},
		{StatusOutdated, true, ExitSoftware},
		{StatusSetupNeeded, false, ExitConfig},
		{StatusMissing, false, ExitUnavailable},
		{StatusProbeFailure, false, ExitSoftware},
	}
	for _, tt := range tests {
		if got := exitCodeForStatus(tt.status, tt.strict); got != tt.want {
			t.Errorf("exitCodeForStatus(%q, strict=%v) = %d, want %d", tt.status, tt.strict, got, tt.want)
		}
	}
}

// TestAggregateRankMax guards the exact bug class prior-art doctors shipped:
// setup-needed(78) must NOT mask missing(69) — aggregation is rank-max over
// severity, not numeric-max over exit codes.
func TestAggregateRankMax(t *testing.T) {
	tests := []struct {
		name      string
		statuses  []InstallStatus
		strict    bool
		wantWorst InstallStatus
		wantExit  int
	}{
		{
			name:      "setup-needed does not mask missing",
			statuses:  []InstallStatus{StatusSetupNeeded, StatusMissing, StatusReady},
			wantWorst: StatusMissing,
			wantExit:  ExitUnavailable,
		},
		{
			name:      "probe-failure dominates everything",
			statuses:  []InstallStatus{StatusMissing, StatusProbeFailure, StatusSetupNeeded},
			wantWorst: StatusProbeFailure,
			wantExit:  ExitSoftware,
		},
		{
			name:      "all ready and unsupported stays green",
			statuses:  []InstallStatus{StatusReady, StatusUnsupported, StatusReady},
			wantWorst: StatusReady,
			wantExit:  ExitOK,
		},
		{
			name:      "unsupported alone is non-failing",
			statuses:  []InstallStatus{StatusUnsupported},
			wantWorst: StatusUnsupported,
			wantExit:  ExitOK,
		},
		{
			name:      "outdated is a warning by default",
			statuses:  []InstallStatus{StatusReady, StatusOutdated},
			wantWorst: StatusOutdated,
			wantExit:  ExitOK,
		},
		{
			name:      "outdated fails under strict",
			statuses:  []InstallStatus{StatusReady, StatusOutdated},
			strict:    true,
			wantWorst: StatusOutdated,
			wantExit:  ExitSoftware,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checks := make([]BackendCheck, 0, len(tt.statuses))
			for _, s := range tt.statuses {
				checks = append(checks, BackendCheck{Status: s})
			}
			agg := aggregate(checks, tt.strict)
			if agg.WorstStatus != tt.wantWorst {
				t.Errorf("WorstStatus = %q, want %q", agg.WorstStatus, tt.wantWorst)
			}
			if agg.ExitCode != tt.wantExit {
				t.Errorf("ExitCode = %d, want %d", agg.ExitCode, tt.wantExit)
			}
		})
	}
}

func TestCheckBackendMissing(t *testing.T) {
	f := fakeLookup{present: map[string]string{}} // nothing on PATH
	check := CheckBackend(context.Background(), AgentClaude, f.lookup())
	if check.Status != StatusMissing {
		t.Fatalf("status = %q, want missing", check.Status)
	}
	if check.ExitCategory != ExitUnavailable {
		t.Errorf("exit_category = %d, want %d", check.ExitCategory, ExitUnavailable)
	}
	if check.ResolvedPath != "" {
		t.Errorf("resolved_path = %q, want empty for missing binary", check.ResolvedPath)
	}
}

// No current backend is uninstallable by design. Insert a fixture into the
// package-level registry to verify unsupported backends are reported correctly.
func TestCheckBackendUnsupportedBackendIsNotAFailure(t *testing.T) {
	const name = "fixture-built-from-source"
	backendRegistry[name] = Backend{
		Name:        name,
		Binary:      name,
		InstallHint: "go build -o " + name + " ./cmd/" + name,
		Supported:   false,
		probe:       func(context.Context, Backend, Lookup) (InstallStatus, string) { return StatusReady, "" },
	}
	defer delete(backendRegistry, name)

	f := fakeLookup{present: map[string]string{}} // nothing on PATH
	check := CheckBackend(context.Background(), name, f.lookup())

	if check.Status != StatusUnsupported {
		t.Fatalf("status = %q, want unsupported — a backend with no installer by design is not missing", check.Status)
	}
	// The whole point of the status: it must never fail an --all rollup.
	if check.ExitCategory != ExitOK {
		t.Errorf("exit_category = %d, want %d (unsupported must not fail --all)", check.ExitCategory, ExitOK)
	}
	if !strings.Contains(check.Detail, "build from source") {
		t.Errorf("detail = %q, want it to point at building from source", check.Detail)
	}
}

func TestCheckBackendUnsupportedName(t *testing.T) {
	f := fakeLookup{}
	check := CheckBackend(context.Background(), "nope", f.lookup())
	if check.Status != StatusProbeFailure {
		t.Fatalf("status = %q, want unknown-probe-failure", check.Status)
	}
}

func TestProbeClaudeStatusSubcommand(t *testing.T) {
	t.Run("authenticated", func(t *testing.T) {
		f := fakeLookup{
			present: map[string]string{"claude": "/bin/claude"},
			runs:    map[string]fakeRun{"claude": {code: 0}},
		}
		check := CheckBackend(context.Background(), AgentClaude, f.lookup())
		if check.Status != StatusReady {
			t.Fatalf("status = %q, want ready", check.Status)
		}
	})
	t.Run("not authenticated", func(t *testing.T) {
		f := fakeLookup{
			present: map[string]string{"claude": "/bin/claude"},
			runs:    map[string]fakeRun{"claude": {code: 1}},
		}
		check := CheckBackend(context.Background(), AgentClaude, f.lookup())
		if check.Status != StatusSetupNeeded {
			t.Fatalf("status = %q, want setup-needed", check.Status)
		}
		if check.ExitCategory != ExitConfig {
			t.Errorf("exit_category = %d, want %d", check.ExitCategory, ExitConfig)
		}
	})
}

// When the status subcommand can't run (old binary / timeout), claude falls
// back to a credential heuristic and reports ready (unverified).
func TestProbeClaudeFallbackHeuristic(t *testing.T) {
	f := fakeLookup{
		present: map[string]string{"claude": "/bin/claude"},
		runs:    map[string]fakeRun{"claude": {code: -1, err: errors.New("unknown subcommand")}},
		env:     map[string]string{"ANTHROPIC_API_KEY": "sk-xxx"},
	}
	check := CheckBackend(context.Background(), AgentClaude, f.lookup())
	if check.Status != StatusReady {
		t.Fatalf("status = %q, want ready (env fallback)", check.Status)
	}
}

func TestProbeGeminiHeuristic(t *testing.T) {
	t.Run("env key -> ready unverified", func(t *testing.T) {
		f := fakeLookup{
			present: map[string]string{"gemini": "/bin/gemini"},
			env:     map[string]string{"GEMINI_API_KEY": "k"},
		}
		check := CheckBackend(context.Background(), AgentGemini, f.lookup())
		if check.Status != StatusReady {
			t.Fatalf("status = %q, want ready", check.Status)
		}
	})
	t.Run("cached oauth -> ready", func(t *testing.T) {
		home := "/home/u"
		f := fakeLookup{
			present: map[string]string{"gemini": "/bin/gemini"},
			home:    home,
			files:   map[string]bool{filepath.Join(home, ".gemini/oauth_creds.json"): true},
		}
		check := CheckBackend(context.Background(), AgentGemini, f.lookup())
		if check.Status != StatusReady {
			t.Fatalf("status = %q, want ready", check.Status)
		}
	})
	t.Run("nothing -> setup-needed", func(t *testing.T) {
		f := fakeLookup{
			present: map[string]string{"gemini": "/bin/gemini"},
			home:    "/home/u",
		}
		check := CheckBackend(context.Background(), AgentGemini, f.lookup())
		if check.Status != StatusSetupNeeded {
			t.Fatalf("status = %q, want setup-needed", check.Status)
		}
	})
}

func TestProbeAntigravityAuthStatus(t *testing.T) {
	t.Run("authenticated", func(t *testing.T) {
		f := fakeLookup{
			present: map[string]string{"agy": "/bin/agy"},
			runs:    map[string]fakeRun{"agy": {code: 0}},
		}
		check := CheckBackend(context.Background(), AgentAntigravity, f.lookup())
		if check.Status != StatusReady {
			t.Fatalf("status = %q, want ready", check.Status)
		}
	})
	t.Run("not authenticated", func(t *testing.T) {
		f := fakeLookup{
			present: map[string]string{"agy": "/bin/agy"},
			runs:    map[string]fakeRun{"agy": {code: 1}},
		}
		check := CheckBackend(context.Background(), AgentAntigravity, f.lookup())
		if check.Status != StatusSetupNeeded {
			t.Fatalf("status = %q, want setup-needed", check.Status)
		}
		if check.ExitCategory != ExitConfig {
			t.Errorf("exit_category = %d, want %d", check.ExitCategory, ExitConfig)
		}
	})
}

// --check must be strictly read-only: no install action is ever taken, even for
// a missing backend.
func TestInstallCheckOnlyIsReadOnly(t *testing.T) {
	f := fakeLookup{present: map[string]string{}} // claude missing
	report := Install(context.Background(), InstallOptions{
		Agents:    []string{AgentClaude},
		CheckOnly: true,
	}, f.lookup())
	if len(report.Actions) != 0 {
		t.Fatalf("check-only produced %d actions, want 0", len(report.Actions))
	}
	if report.Aggregate.ExitCode != ExitUnavailable {
		t.Errorf("exit = %d, want %d", report.Aggregate.ExitCode, ExitUnavailable)
	}
}

// --dry-run on a missing backend guides (prints the command) but never executes.
func TestInstallDryRunGuidesWithoutExecuting(t *testing.T) {
	f := fakeLookup{present: map[string]string{}}
	report := Install(context.Background(), InstallOptions{
		Agents: []string{AgentClaude},
		DryRun: true,
	}, f.lookup())
	if len(report.Actions) != 1 {
		t.Fatalf("got %d actions, want 1", len(report.Actions))
	}
	a := report.Actions[0]
	if a.Executed {
		t.Errorf("dry-run executed the install command; must not")
	}
	if a.Action != "guide" {
		t.Errorf("action = %q, want guide", a.Action)
	}
	if a.Command == "" {
		t.Errorf("dry-run should surface the install command")
	}
}

// Without --yes, a missing backend is guided, not installed.
func TestInstallDefaultGuidesMissing(t *testing.T) {
	f := fakeLookup{present: map[string]string{}}
	report := Install(context.Background(), InstallOptions{Agents: []string{AgentClaude}}, f.lookup())
	if len(report.Actions) != 1 || report.Actions[0].Action != "guide" {
		t.Fatalf("expected a single guide action, got %+v", report.Actions)
	}
	if report.Actions[0].Executed {
		t.Errorf("default install executed without --yes; must only guide")
	}
}

// With --yes, a missing backend's install command runs, and a successful install
// flips the re-check to ready.
func TestInstallYesExecutesAndRechecks(t *testing.T) {
	installed := false
	lk := Lookup{
		LookPath: func(name string) (string, error) {
			if name == "claude" && installed {
				return "/bin/claude", nil
			}
			return "", errors.New("not found")
		},
		Run: func(ctx context.Context, name string, args ...string) (string, int, error) {
			// The install command runs via the shell; mark installed, then the
			// post-install auth probe reports authenticated.
			if name == "sh" || name == "cmd" {
				installed = true
				return "", 0, nil
			}
			if name == "claude" {
				return "", 0, nil // auth status: authenticated
			}
			return "", -1, errors.New("unexpected")
		},
		Getenv:     func(string) string { return "" },
		FileExists: func(string) bool { return false },
		HomeDir:    func() (string, error) { return "/home/u", nil },
	}
	report := Install(context.Background(), InstallOptions{
		Agents: []string{AgentClaude},
		Yes:    true,
	}, lk)
	if len(report.Actions) != 1 {
		t.Fatalf("got %d actions, want 1", len(report.Actions))
	}
	if a := report.Actions[0]; !a.Executed || !a.Succeeded || a.Action != "install" {
		t.Fatalf("action = %+v, want executed+succeeded install", a)
	}
	if report.Aggregate.WorstStatus != StatusReady {
		t.Errorf("after install re-check worst = %q, want ready", report.Aggregate.WorstStatus)
	}
}

func TestPrimaryInstallCommand(t *testing.T) {
	tests := []struct {
		hint string
		want string
	}{
		{"curl -fsSL https://x | bash  (or: brew install y)", "curl -fsSL https://x | bash"},
		{"npm install -g pkg", "npm install -g pkg"},
		{"a (or: b)", "a"},
	}
	for _, tt := range tests {
		if got := primaryInstallCommand(tt.hint); got != tt.want {
			t.Errorf("primaryInstallCommand(%q) = %q, want %q", tt.hint, got, tt.want)
		}
	}
}

// The registry must cover exactly the executable backends and never drift from
// the execution adapters (Binary == defaultExecutable(name)).
func TestBackendRegistryMatchesSupported(t *testing.T) {
	for _, name := range SupportedAgents() {
		b, ok := backendRegistry[name]
		if !ok {
			t.Errorf("backend %q in SupportedAgents but missing from install registry", name)
			continue
		}
		if b.Binary != defaultExecutable(name) {
			t.Errorf("backend %q binary = %q, want %q (drift from adapters)", name, b.Binary, defaultExecutable(name))
		}
		// InstallHint is what shellcli prints when a backend is missing, so an
		// empty one is a dead end for the operator. Check every supported backend.
		if hint, ok := InstallHint(name); !ok || hint == "" {
			t.Errorf("backend %q carries no install hint", name)
		}
	}
}

func TestDefaultLookupAndRunProbe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	dir := t.TempDir()
	okPath := filepath.Join(dir, "ok-probe")
	failPath := filepath.Join(dir, "fail-probe")
	writeExecutable(t, okPath, "#!/bin/sh\nprintf 'ready\\n'\n")
	writeExecutable(t, failPath, "#!/bin/sh\nprintf 'nope\\n' >&2\nexit 7\n")

	lk := DefaultLookup()
	if lk.LookPath == nil || lk.Run == nil || lk.Getenv == nil || lk.FileExists == nil || lk.HomeDir == nil {
		t.Fatalf("DefaultLookup returned nil primitive: %#v", lk)
	}
	if !lk.FileExists(okPath) {
		t.Fatalf("DefaultLookup.FileExists(%q) = false, want true", okPath)
	}
	if lk.FileExists(filepath.Join(dir, "missing")) {
		t.Fatalf("DefaultLookup.FileExists(missing) = true, want false")
	}

	out, code, err := runProbe(context.Background(), okPath)
	if err != nil || code != 0 || out != "ready\n" {
		t.Fatalf("runProbe(ok) = out %q code %d err %v, want ready/0/nil", out, code, err)
	}
	out, code, err = runProbe(context.Background(), failPath)
	if err != nil || code != 7 || !strings.Contains(out, "nope") {
		t.Fatalf("runProbe(exit) = out %q code %d err %v, want nope/7/nil", out, code, err)
	}
	out, code, err = runProbe(context.Background(), filepath.Join(dir, "missing"))
	if err == nil || code != -1 {
		t.Fatalf("runProbe(missing) = out %q code %d err %v, want err/-1", out, code, err)
	}
}

func TestProbeCodexStatusAndFallbacks(t *testing.T) {
	home := "/home/u"
	tests := []struct {
		name       string
		run        fakeRun
		env        map[string]string
		files      map[string]bool
		wantStatus InstallStatus
		wantDetail string
	}{
		{
			name:       "login status ready with api key advisory",
			run:        fakeRun{code: 0},
			env:        map[string]string{"OPENAI_API_KEY": "sk-test"},
			files:      map[string]bool{filepath.Join(home, ".codex/auth.json"): true},
			wantStatus: StatusReady,
			wantDetail: "shadows stored OAuth",
		},
		{
			name:       "login status setup needed",
			run:        fakeRun{code: 1},
			wantStatus: StatusSetupNeeded,
			wantDetail: "codex login",
		},
		{
			name:       "api key fallback",
			run:        fakeRun{code: -1, err: errors.New("old binary")},
			env:        map[string]string{"OPENAI_API_KEY": "sk-test"},
			wantStatus: StatusReady,
			wantDetail: "OPENAI_API_KEY set",
		},
		{
			name:       "cached oauth fallback",
			run:        fakeRun{code: -1, err: errors.New("old binary")},
			files:      map[string]bool{filepath.Join(home, ".codex/auth.json"): true},
			wantStatus: StatusReady,
			wantDetail: ".codex/auth.json",
		},
		{
			name:       "no credentials",
			run:        fakeRun{code: -1, err: errors.New("old binary")},
			wantStatus: StatusSetupNeeded,
			wantDetail: "could not verify auth",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := fakeLookup{
				present: map[string]string{"codex": "/bin/codex"},
				runs:    map[string]fakeRun{"codex": tt.run},
				env:     tt.env,
				files:   tt.files,
				home:    home,
			}
			check := CheckBackend(context.Background(), AgentCodex, f.lookup())
			if check.Status != tt.wantStatus {
				t.Fatalf("status = %q, want %q; check=%+v", check.Status, tt.wantStatus, check)
			}
			if !strings.Contains(check.Detail, tt.wantDetail) {
				t.Fatalf("detail = %q, want substring %q", check.Detail, tt.wantDetail)
			}
		})
	}
}

func TestProbeCursorPresent(t *testing.T) {
	cursorReady := CheckBackend(context.Background(), AgentCursor, fakeLookup{
		present: map[string]string{"cursor-agent": "/bin/cursor-agent"},
		env:     map[string]string{"CURSOR_API_KEY": "cursor-key"},
	}.lookup())
	if cursorReady.Status != StatusReady {
		t.Fatalf("cursor env status = %q, want ready", cursorReady.Status)
	}

	cursorSetup := CheckBackend(context.Background(), AgentCursor, fakeLookup{
		present: map[string]string{"cursor-agent": "/bin/cursor-agent"},
	}.lookup())
	if cursorSetup.Status != StatusSetupNeeded {
		t.Fatalf("cursor setup status = %q, want setup-needed", cursorSetup.Status)
	}
}

func TestInstallFailureDetail(t *testing.T) {
	runErr := installFailureDetail("", -1, errors.New("permission denied"))
	if !strings.Contains(runErr, "failed to run") || !strings.Contains(runErr, "permission denied") {
		t.Fatalf("run error detail = %q", runErr)
	}
	withOutput := installFailureDetail("first line\nsecond line", 2, nil)
	if withOutput != "install command exited 2: first line" {
		t.Fatalf("with output = %q", withOutput)
	}
	withoutOutput := installFailureDetail("", 3, nil)
	if withoutOutput != "install command exited 3" {
		t.Fatalf("without output = %q", withoutOutput)
	}
}

func TestSortedStatusCounts(t *testing.T) {
	got := SortedStatusCounts(map[InstallStatus]int{
		StatusReady:        2,
		StatusMissing:      1,
		StatusSetupNeeded:  3,
		StatusProbeFailure: 4,
	})
	want := []struct {
		status InstallStatus
		count  int
	}{
		{StatusProbeFailure, 4},
		{StatusMissing, 1},
		{StatusSetupNeeded, 3},
		{StatusReady, 2},
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Status != want[i].status || got[i].Count != want[i].count {
			t.Fatalf("got[%d] = %+v, want %q/%d; full=%#v", i, got[i], want[i].status, want[i].count, got)
		}
	}
}
