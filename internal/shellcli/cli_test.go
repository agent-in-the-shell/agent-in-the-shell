package shellcli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentshell"
)

func TestUsageListsAntigravity(t *testing.T) {
	if !strings.Contains(usage, "antigravity") {
		t.Fatalf("usage missing antigravity: %s", usage)
	}
}

func TestParseInstallArgsAndResolveInstallAgents(t *testing.T) {
	selected, all, opts, jsonOut, err := parseInstallArgs([]string{"codex", "--dry-run", "--strict", "--json", "--yes"})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0] != "codex" || all || !opts.DryRun || !opts.Strict || !opts.Yes || !jsonOut {
		t.Fatalf("selected=%#v all=%v opts=%+v json=%v", selected, all, opts, jsonOut)
	}

	selected, all, opts, jsonOut, err = parseInstallArgs([]string{"--all", "--check"})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 0 || !all || !opts.CheckOnly || jsonOut {
		t.Fatalf("selected=%#v all=%v opts=%+v json=%v", selected, all, opts, jsonOut)
	}

	for _, args := range [][]string{{"--nope"}, {"--check", "--yes"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if _, _, _, _, err := parseInstallArgs(args); err == nil {
				t.Fatalf("parseInstallArgs(%v) = nil err, want error", args)
			}
		})
	}
	out := captureStdout(t, func() {
		if _, _, _, _, err := parseInstallArgs([]string{"--help"}); !errors.Is(err, errHelpRequested) {
			t.Fatalf("help err = %v, want errHelpRequested", err)
		}
	})
	if !strings.Contains(out, "agent-shell:") {
		t.Fatalf("install help output = %q", out)
	}

	if got, err := resolveInstallAgents([]string{"codex"}, false); err != nil || !reflect.DeepEqual(got, []string{agentshell.AgentCodex}) {
		t.Fatalf("resolveInstallAgents(codex) = %v, %v", got, err)
	}
	if got, err := resolveInstallAgents(nil, true); err != nil || got != nil {
		t.Fatalf("resolveInstallAgents(all) = %v, %v; want nil nil", got, err)
	}
	if _, err := resolveInstallAgents([]string{"codex", "claude"}, false); err == nil {
		t.Fatal("resolveInstallAgents(two selected) = nil err, want error")
	}
	if _, err := resolveInstallAgents([]string{"nope"}, false); err == nil {
		t.Fatal("resolveInstallAgents(unsupported) = nil err, want error")
	}
}

func TestPrintInstallReport(t *testing.T) {
	var out bytes.Buffer
	printInstallReport(&out, agentshell.InstallReport{
		InstallCheckReport: agentshell.InstallCheckReport{
			Backends: []agentshell.BackendCheck{
				{Agent: agentshell.AgentClaude, Binary: "claude", Status: agentshell.StatusReady, ResolvedPath: "/bin/claude", Detail: "ok"},
				{Agent: agentshell.AgentCodex, Binary: "codex", Status: agentshell.StatusMissing},
			},
			Aggregate: agentshell.Aggregate{WorstStatus: agentshell.StatusMissing, ExitCode: agentshell.ExitUnavailable},
		},
		Actions: []agentshell.InstallAction{
			{Agent: agentshell.AgentCodex, Action: "install", Command: "npm install -g @openai/codex", Executed: true, Succeeded: false, Detail: "network down"},
			{Agent: agentshell.AgentGemini, Action: "guide", Command: "npm install -g @google/gemini-cli", Detail: "dry-run"},
		},
	}, false)
	got := out.String()
	for _, want := range []string{
		"AGENT", "claude", "/bin/claude", "codex", "missing",
		"install codex: ran", "failed", "network down",
		"install gemini:", "dry-run", "worst: missing", "exit: 69",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("install report = %q, want substring %q", got, want)
		}
	}

	out.Reset()
	printInstallReport(&out, agentshell.InstallReport{
		InstallCheckReport: agentshell.InstallCheckReport{
			Backends:  []agentshell.BackendCheck{{Agent: agentshell.AgentGemini, Binary: "gemini", Status: agentshell.StatusReady}},
			Aggregate: agentshell.Aggregate{WorstStatus: agentshell.StatusReady, ExitCode: agentshell.ExitOK},
		},
		Actions: []agentshell.InstallAction{{Agent: agentshell.AgentGemini, Action: "guide", Detail: "sign in"}},
	}, true)
	if strings.Contains(out.String(), "sign in") {
		t.Fatalf("check-only report should not print actions: %q", out.String())
	}
}

func TestRunInstallJSONExitsWithAggregateCode(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	clearAgentPathEnv(t)

	stdout, stderr, code, exited := captureOutputWithExit(t, func() {
		runInstall([]string{"gemini", "--check", "--json"})
	})
	if !exited || code != agentshell.ExitUnavailable {
		t.Fatalf("runInstall exit = (%v, %d), want unavailable %d", exited, code, agentshell.ExitUnavailable)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}

	var report agentshell.InstallReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("invalid json %q: %v", stdout, err)
	}
	if len(report.Backends) != 1 ||
		report.Backends[0].Agent != agentshell.AgentGemini ||
		report.Backends[0].Status != agentshell.StatusMissing ||
		report.Aggregate.ExitCode != agentshell.ExitUnavailable {
		t.Fatalf("report = %#v, want missing gemini aggregate unavailable", report)
	}
}

func TestRunInstallUsageErrorsExitUsage(t *testing.T) {
	for _, args := range [][]string{
		{"codex", "claude"},
		{"--check", "--yes"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, stderr, code, exited := captureOutputWithExit(t, func() {
				runInstall(args)
			})
			if !exited || code != agentshell.ExitUsage {
				t.Fatalf("runInstall(%v) exit = (%v, %d), want usage %d", args, exited, code, agentshell.ExitUsage)
			}
			if !strings.Contains(stderr, "agent-shell:") {
				t.Fatalf("stderr = %q, want agent-shell error", stderr)
			}
		})
	}
}

func TestRunInstallHelpReturns(t *testing.T) {
	stdout, stderr, _, exited := captureOutputWithExit(t, func() {
		runInstall([]string{"--help"})
	})
	if exited {
		t.Fatalf("help should return without exit")
	}
	if !strings.Contains(stdout, "agent-shell:") || stderr != "" {
		t.Fatalf("stdout=%q stderr=%q, want usage on stdout only", stdout, stderr)
	}
}

func TestRunAgentsPlainOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	dir := t.TempDir()
	writeTestExecutable(t, filepath.Join(dir, "claude"), "#!/bin/sh\nprintf 'claude 2.1.0\\n'\n")
	t.Setenv("PATH", dir)
	clearAgentPathEnv(t)

	out := captureStdout(t, func() {
		runAgents(nil)
	})

	if !strings.Contains(out, "claude") || !strings.Contains(out, "installed") || !strings.Contains(out, filepath.Join(dir, "claude")) {
		t.Fatalf("output = %q, want installed claude path", out)
	}
	if !strings.Contains(out, "codex") || !strings.Contains(out, "missing") {
		t.Fatalf("output = %q, want missing codex", out)
	}
}

func TestRunAgentsJSONOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	dir := t.TempDir()
	writeTestExecutable(t, filepath.Join(dir, "gemini"), "#!/bin/sh\nprintf 'gemini 1.2.3\\n'\n")
	t.Setenv("PATH", dir)
	clearAgentPathEnv(t)

	out := captureStdout(t, func() {
		runAgents([]string{"--json"})
	})

	var infos []agentshell.AgentInfo
	if err := json.Unmarshal([]byte(out), &infos); err != nil {
		t.Fatalf("invalid json %q: %v", out, err)
	}
	if len(infos) != len(agentshell.SupportedAgents()) {
		t.Fatalf("len = %d, want all %d backends: %#v", len(infos), len(agentshell.SupportedAgents()), infos)
	}
	var gemini agentshell.AgentInfo
	for _, info := range infos {
		if info.Agent == agentshell.AgentGemini {
			gemini = info
		}
	}
	if !gemini.Installed || !strings.Contains(gemini.Path, "gemini") {
		t.Fatalf("gemini = %#v, want installed gemini path", gemini)
	}
	if gemini.Version != "" && gemini.Version != "gemini 1.2.3" {
		t.Fatalf("gemini version = %q, want empty or fixture version", gemini.Version)
	}
}

func TestRunSubmitAgentsJSONFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	dir := t.TempDir()
	codexPath := filepath.Join(dir, "codex")
	writeTestExecutable(t, codexPath, "#!/bin/sh\nprintf 'fallback ok'\n")
	t.Setenv("PATH", t.TempDir())
	clearAgentPathEnv(t)
	t.Setenv("AGENT_SHELL_CLAUDE_PATH", filepath.Join(dir, "missing-claude"))
	t.Setenv("AGENT_SHELL_CODEX_PATH", codexPath)

	out := captureStdout(t, func() {
		runSubmit([]string{"--agents", "claude,codex", "--json", "hello"})
	})

	var result agentshell.Result
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("invalid json %q: %v", out, err)
	}
	if result.Agent != agentshell.AgentCodex || result.Stdout != "fallback ok" {
		t.Fatalf("result = %#v, want codex fallback success", result)
	}
	if len(result.Attempts) != 2 || !result.Attempts[0].Skipped || result.Attempts[1].Skipped {
		t.Fatalf("attempts = %#v, want skipped claude then codex", result.Attempts)
	}
}

func TestRunSubmitDangerouslySkipPermissionsDryRun(t *testing.T) {
	clearAgentPathEnv(t)

	out := captureStdout(t, func() {
		runSubmit([]string{"--agent", "claude", "--dangerously-skip-permissions", "--dry-run", "--json", "run ./script.sh"})
	})

	var result agentshell.Result
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("invalid json %q: %v", out, err)
	}
	if !result.DryRun {
		t.Fatalf("result = %#v, want dry-run", result)
	}
	var found bool
	for _, arg := range result.Command {
		if arg == "--dangerously-skip-permissions" {
			found = true
		}
	}
	if !found {
		t.Fatalf("command = %#v, want --dangerously-skip-permissions", result.Command)
	}
}

func TestRunSubmitStreamJSONKeepsStdoutParseable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	exe := filepath.Join(t.TempDir(), "fake-agent")
	writeTestExecutable(t, exe, "#!/bin/sh\nprintf stream-event\nprintf stream-error >&2\n")

	out := captureStdout(t, func() {
		runSubmit([]string{"--agent", "claude", "--exec", exe, "--stream", "--json", "hello"})
	})

	var result agentshell.Result
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("invalid json %q: %v", out, err)
	}
	if result.Stdout != "stream-event" || result.Stderr != "stream-error" {
		t.Fatalf("result = %#v, want captured stream output", result)
	}
}

func TestRunDispatchHelpAndDefaultSubmit(t *testing.T) {
	out := captureStdout(t, func() { Run(nil) })
	if !strings.Contains(out, "agent-shell:") {
		t.Fatalf("Run(nil) output = %q", out)
	}

	out = captureStdout(t, func() { Run([]string{"help"}) })
	if !strings.Contains(out, "agent-shell:") {
		t.Fatalf("Run(help) output = %q", out)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestReadPromptEnvDefaultAndShellJoin(t *testing.T) {
	prompt, err := readPrompt([]string{"hello", "world"}, strings.NewReader(""))
	if err != nil || prompt != "hello world" {
		t.Fatalf("readPrompt(args) = %q, %v", prompt, err)
	}
	prompt, err = readPrompt([]string{"-"}, strings.NewReader("  stdin task\n"))
	if err != nil || prompt != "stdin task" {
		t.Fatalf("readPrompt(stdin) = %q, %v", prompt, err)
	}
	if _, err := readPrompt(nil, strings.NewReader("  \n")); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("blank readPrompt err = %v, want required", err)
	}
	if _, err := readPrompt(nil, errReader{}); err == nil || !strings.Contains(err.Error(), "read stdin") {
		t.Fatalf("reader error = %v, want read stdin", err)
	}

	t.Setenv("AGENT_SHELL_TEST_DEFAULT", " env value ")
	if got := envDefault("AGENT_SHELL_TEST_DEFAULT", "fallback"); got != "env value" {
		t.Fatalf("envDefault set = %q", got)
	}
	t.Setenv("AGENT_SHELL_TEST_DEFAULT", " ")
	if got := envDefault("AGENT_SHELL_TEST_DEFAULT", "fallback"); got != "fallback" {
		t.Fatalf("envDefault blank = %q", got)
	}

	got := shellJoin([]string{"plain", "two words", "has'quote", "", "$HOME"})
	want := "plain 'two words' 'has'\\''quote' '' '$HOME'"
	if got != want {
		t.Fatalf("shellJoin = %q, want %q", got, want)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()

	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func clearAgentPathEnv(t *testing.T) {
	t.Helper()
	t.Setenv("AGENT_SHELL_CLAUDE_PATH", "")
	t.Setenv("AGENT_SHELL_CODEX_PATH", "")
	t.Setenv("AGENT_SHELL_GEMINI_PATH", "")
	t.Setenv("AGENT_SHELL_ANTIGRAVITY_PATH", "")
	t.Setenv("AGENT_SHELL_CURSOR_PATH", "")
}

func writeTestExecutable(t *testing.T, path, script string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
}

type testExit struct {
	code int
}

func captureOutputWithExit(t *testing.T, fn func()) (stdout string, stderr string, code int, exited bool) {
	t.Helper()

	dir := t.TempDir()
	outFile, err := os.CreateTemp(dir, "stdout-*")
	if err != nil {
		t.Fatal(err)
	}
	errFile, err := os.CreateTemp(dir, "stderr-*")
	if err != nil {
		t.Fatal(err)
	}

	oldStdout := os.Stdout
	oldStderr := os.Stderr
	oldExit := osExit
	os.Stdout = outFile
	os.Stderr = errFile
	osExit = func(code int) {
		panic(testExit{code: code})
	}
	defer func() {
		os.Stdout = oldStdout
		os.Stderr = oldStderr
		osExit = oldExit
	}()

	var recovered any
	func() {
		defer func() {
			recovered = recover()
		}()
		fn()
	}()

	if err := outFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err := errFile.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(outFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	errOut, err := os.ReadFile(errFile.Name())
	if err != nil {
		t.Fatal(err)
	}

	if recovered != nil {
		trap, ok := recovered.(testExit)
		if !ok {
			panic(recovered)
		}
		return string(out), string(errOut), trap.code, true
	}
	return string(out), string(errOut), 0, false
}

// TestRunSubmitRejectsDefinedFlagAfterPrompt checks flag placement. Go's flag package
// stops parsing at the first non-flag argument and readPrompt joins whatever is
// left, so `submit "task" --agent codex` silently runs on claude with "--agent
// codex" appended to the prompt. Erroring is the MVP-safe reading: the unified
// interface is the product, and a silently ignored --agent defeats it.
func TestRunSubmitRejectsDefinedFlagAfterPrompt(t *testing.T) {
	_, stderr, code, exited := captureOutputWithExit(t, func() {
		runSubmit([]string{"--dry-run", "fix the bug", "--agent", "codex"})
	})
	if !exited || code == 0 {
		t.Fatalf("expected a nonzero exit, got exited=%v code=%d", exited, code)
	}
	if !strings.Contains(stderr, "--agent") {
		t.Errorf("error should name the swallowed flag, got: %q", stderr)
	}
}

// A prompt may legitimately contain flag-shaped words that are not flags this
// command defines; only the real ones are ambiguous enough to reject.
func TestRunSubmitAllowsUnknownFlagShapedPromptWords(t *testing.T) {
	stdout, _, _, _ := captureOutputWithExit(t, func() {
		runSubmit([]string{"--dry-run", "explain the --deeply-nested flag"})
	})
	if !strings.Contains(stdout, "--deeply-nested") {
		t.Errorf("prompt text was altered or rejected: %q", stdout)
	}
}

// TestResolveInstallAgentsPositionalBeatsAll checks positional selection: `doctor codex` is
// rewritten to `--check --all codex`, and --all discarding the positional makes
// the command report (and exit on) every backend's status.
func TestResolveInstallAgentsPositionalBeatsAll(t *testing.T) {
	got, err := resolveInstallAgents([]string{"codex"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "codex" {
		t.Fatalf("resolveInstallAgents([codex], all=true) = %v, want [codex]", got)
	}
}
