package agentshell

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/internal/agentregistry"
)

func TestBuildCommandClaude(t *testing.T) {
	execPath, args, err := BuildCommand(Request{
		Agent:        "claude",
		Prompt:       "fix tests",
		Model:        "sonnet",
		SystemPrompt: "be concise",
	})
	if err != nil {
		t.Fatal(err)
	}
	if execPath != "claude" {
		t.Fatalf("execPath = %q, want claude", execPath)
	}
	want := []string{"-p", "fix tests", "--output-format", "text", "--model", "sonnet", "--system-prompt", "be concise"}
	assertStrings(t, args, want)
}

func TestBuildCommandCodex(t *testing.T) {
	execPath, args, err := BuildCommand(Request{
		Agent:        "codex",
		Prompt:       "implement feature",
		CWD:          "/tmp/repo",
		Model:        "gpt-5.4",
		SystemPrompt: "follow repo style",
	})
	if err != nil {
		t.Fatal(err)
	}
	if execPath != "codex" {
		t.Fatalf("execPath = %q, want codex", execPath)
	}
	want := []string{
		"exec",
		"--cd", "/tmp/repo",
		"--model", "gpt-5.4",
		"--config", `instructions="follow repo style"`,
		"implement feature",
	}
	assertStrings(t, args, want)
}

func TestBuildCommandGemini(t *testing.T) {
	_, args, err := BuildCommand(Request{Agent: "gemini", Prompt: "summarize"})
	if err != nil {
		t.Fatal(err)
	}
	assertStrings(t, args, []string{"--prompt", "summarize", "--output-format", "text"})
}

func TestBuildCommandAntigravity(t *testing.T) {
	execPath, args, err := BuildCommand(Request{
		Agent:                      "agy",
		Prompt:                     "fix tests",
		CWD:                        "/tmp/repo",
		Model:                      "Gemini 3.1 Pro (High)",
		Timeout:                    7 * time.Minute,
		DangerouslySkipPermissions: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if execPath != "agy" {
		t.Fatalf("execPath = %q, want agy", execPath)
	}
	want := []string{
		"--print", "fix tests",
		"--print-timeout", "7m0s",
		"--model", "Gemini 3.1 Pro (High)",
		"--add-dir", "/tmp/repo",
		"--dangerously-skip-permissions",
	}
	assertStrings(t, args, want)
}

func TestBuildCommandAntigravityRejectsUnsupportedOptions(t *testing.T) {
	if _, _, err := BuildCommand(Request{Agent: "antigravity", Prompt: "go", SystemPrompt: "be concise"}); err == nil ||
		!strings.Contains(err.Error(), "antigravity adapter does not support system prompts") {
		t.Fatalf("system prompt err = %v, want antigravity system-prompt rejection", err)
	}
}

func TestBuildCommandAntigravityAbsolutizesAddDir(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(base)

	_, args, err := BuildCommand(Request{Agent: "antigravity", Prompt: "go", CWD: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	if !containsPair(args, "--add-dir", repo) {
		t.Fatalf("args = %#v, want --add-dir %q", args, repo)
	}
}

func TestBuildCommandCursor(t *testing.T) {
	execPath, args, err := BuildCommand(Request{
		Agent:  "cursor",
		Prompt: "implement feature",
		CWD:    "/tmp/repo",
		Model:  "auto",
	})
	if err != nil {
		t.Fatal(err)
	}
	if execPath != "cursor-agent" {
		t.Fatalf("execPath = %q, want cursor-agent", execPath)
	}
	want := []string{
		"-p", "--output-format", "text",
		"--model", "auto",
		"--workspace", "/tmp/repo",
		"implement feature",
	}
	assertStrings(t, args, want)
}

func TestBuildCommandCursorRejectsSystemPrompt(t *testing.T) {
	_, _, err := BuildCommand(Request{Agent: "cursor", Prompt: "go", SystemPrompt: "be concise"})
	if err == nil || !strings.Contains(err.Error(), "cursor adapter does not support system prompts") {
		t.Fatalf("err = %v, want cursor system-prompt rejection", err)
	}
}

func TestNormalizeAgentCursorAlias(t *testing.T) {
	if got := NormalizeAgent("cursor-agent"); got != AgentCursor {
		t.Fatalf("NormalizeAgent(cursor-agent) = %q, want cursor", got)
	}
}

func TestNormalizeAgentAntigravityAlias(t *testing.T) {
	if got := NormalizeAgent("agy"); got != AgentAntigravity {
		t.Fatalf("NormalizeAgent(agy) = %q, want antigravity", got)
	}
}

func TestBuildCommandStream(t *testing.T) {
	cases := []struct {
		agent string
		want  []string
	}{
		{
			agent: "claude",
			want:  []string{"-p", "go", "--output-format", "stream-json", "--verbose", "--include-partial-messages"},
		},
		{
			agent: "codex",
			want:  []string{"exec", "--json", "go"},
		},
		{
			agent: "gemini",
			want:  []string{"--prompt", "go", "--output-format", "stream-json"},
		},
		{
			agent: "antigravity",
			want:  []string{"--print", "go", "--print-timeout", "30m0s"},
		},
		{
			agent: "cursor",
			want:  []string{"-p", "--output-format", "stream-json", "--stream-partial-output", "go"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.agent, func(t *testing.T) {
			_, args, err := BuildCommand(Request{
				Agent:  tc.agent,
				Prompt: "go",
				Stream: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			assertStrings(t, args, tc.want)
		})
	}
}

func TestBuildCommandDangerouslySkipPermissions(t *testing.T) {
	cases := []struct {
		agent string
		want  []string
	}{
		{
			agent: "claude",
			want:  []string{"-p", "go", "--output-format", "text", "--dangerously-skip-permissions"},
		},
		{
			agent: "codex",
			want:  []string{"exec", "--dangerously-bypass-approvals-and-sandbox", "go"},
		},
		{
			agent: "gemini",
			want:  []string{"--prompt", "go", "--output-format", "text", "--approval-mode", "yolo"},
		},
		{
			agent: "antigravity",
			want:  []string{"--print", "go", "--print-timeout", "30m0s", "--dangerously-skip-permissions"},
		},
		{
			agent: "cursor",
			want:  []string{"-p", "--output-format", "text", "--force", "go"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.agent, func(t *testing.T) {
			_, args, err := BuildCommand(Request{
				Agent:                      tc.agent,
				Prompt:                     "go",
				DangerouslySkipPermissions: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			assertStrings(t, args, tc.want)
		})
	}
}

func TestSubmitDryRun(t *testing.T) {
	got, err := Submit(context.Background(), Request{
		Agent:      "codex",
		Prompt:     "hello",
		Executable: "/bin/codex",
		DryRun:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.DryRun {
		t.Fatal("DryRun = false, want true")
	}
	assertStrings(t, got.Command, []string{"/bin/codex", "exec", "hello"})
}

func TestSubmitCapturesOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	exe := filepath.Join(t.TempDir(), "fake-agent")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nprintf stdout\nprintf stderr >&2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	got, err := Submit(context.Background(), Request{
		Agent:      "claude",
		Prompt:     "ignored",
		Executable: exe,
		Timeout:    5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Stdout != "stdout" || got.Stderr != "stderr" || got.ExitCode != 0 {
		t.Fatalf("unexpected result: %#v", got)
	}
	if got.CWD == "" || !filepath.IsAbs(got.CWD) {
		t.Fatalf("CWD = %q, want absolute", got.CWD)
	}
}

func TestSubmitAntigravityEmptyStdoutFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	exe := filepath.Join(t.TempDir(), "agy")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nprintf 'diagnostic' >&2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	got, err := Submit(context.Background(), Request{
		Agent:      "antigravity",
		Prompt:     "ignored",
		Executable: exe,
		Timeout:    5 * time.Second,
	})
	if err == nil {
		t.Fatal("err = nil, want empty-stdout antigravity failure")
	}
	if got.ExitCode != 1 || got.Status != "error" || got.Stdout != "" || !strings.Contains(got.Stderr, "diagnostic") {
		t.Fatalf("result = %#v, want adapter error preserving stderr", got)
	}
	if !strings.Contains(got.Error, "agy auth login") || !strings.Contains(got.Error, "#76/#187") ||
		!strings.Contains(got.Stderr, "agy auth login") {
		t.Fatalf("error = %q stderr = %q, want auth/upstream empty-output guidance", got.Error, got.Stderr)
	}
}

func TestSubmitStreamsAndCapturesOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	exe := filepath.Join(t.TempDir(), "fake-agent")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nprintf stdout\nprintf stderr >&2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	var streamOut, streamErr bytes.Buffer
	got, err := Submit(context.Background(), Request{
		Agent:        "claude",
		Prompt:       "ignored",
		Executable:   exe,
		Timeout:      5 * time.Second,
		Stream:       true,
		StreamStdout: &streamOut,
		StreamStderr: &streamErr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Stdout != "stdout" || got.Stderr != "stderr" {
		t.Fatalf("captured = (%q, %q), want stdout/stderr", got.Stdout, got.Stderr)
	}
	if streamOut.String() != "stdout" || streamErr.String() != "stderr" {
		t.Fatalf("streamed = (%q, %q), want stdout/stderr", streamOut.String(), streamErr.String())
	}
}

func TestSubmitCapturesFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	exe := filepath.Join(t.TempDir(), "fake-agent")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nprintf bad >&2\nexit 7\n"), 0700); err != nil {
		t.Fatal(err)
	}
	got, err := Submit(context.Background(), Request{
		Agent:      "claude",
		Prompt:     "ignored",
		Executable: exe,
		Timeout:    5 * time.Second,
	})
	if err == nil {
		t.Fatal("err = nil, want failure")
	}
	// 7 is a real *normal* nonzero exit (not a signal), so it is unaffected by
	// the signal-decode change; Status is the clean-completion "ok".
	if got.ExitCode != 7 || got.Stderr != "bad" || got.Error == "" || got.Status != "ok" {
		t.Fatalf("unexpected result: %#v", got)
	}
}

// TestSubmit_SignalDeathFields locks in the additive Status/Signal population on
// a real signal death. The stub self-sends SIGTERM (deterministic, no race), so
// Submit must report the honest 143/15/signaled instead of the former raw -1.
// Without this test a refactor could silently drop the population and every
// other agentshell test (which only assert ExitCode) would still pass.
func TestSubmit_SignalDeathFields(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal death is Unix-only")
	}
	exe := filepath.Join(t.TempDir(), "fake-agent")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nkill -TERM $$\n"), 0700); err != nil {
		t.Fatal(err)
	}
	got, err := Submit(context.Background(), Request{
		Agent:      "claude",
		Prompt:     "ignored",
		Executable: exe,
		Timeout:    5 * time.Second,
	})
	if err == nil {
		t.Fatal("err = nil, want signal-death failure")
	}
	if got.ExitCode != 143 || got.Signal != 15 || got.Status != "signaled" {
		t.Fatalf("signal death: got ExitCode=%d Signal=%d Status=%q; want 143/15/signaled", got.ExitCode, got.Signal, got.Status)
	}
}

// TestSubmit_RecordsCompletionWithParentPropagation locks in the Axis 9 MVP
// wiring end to end: this process's own AGENT_RUN_ID is read as the run's
// ParentID, a fresh AGENT_RUN_ID is exported into the child's env (so a
// nested invocation could pick it up as its own ParentID), and a durable
// CompletionRecord is written — readable by `agent-shell wait`/`agent-shell
// logs` — after the run finishes.
func TestSubmit_RecordsCompletionWithParentPropagation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("AGENT_RUN_ID", "parent-run-id")

	exe := filepath.Join(t.TempDir(), "fake-agent")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nprintf '%s' \"$AGENT_RUN_ID\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	got, err := Submit(context.Background(), Request{
		Agent:      "claude",
		Prompt:     "ignored",
		Executable: exe,
		Timeout:    5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	childRunID := got.Stdout
	if childRunID == "" || childRunID == "parent-run-id" {
		t.Fatalf("child's own AGENT_RUN_ID = %q, want a fresh id distinct from the parent's", childRunID)
	}

	rec, ok, rerr := agentregistry.ReadCompletion(childRunID)
	if rerr != nil {
		t.Fatalf("ReadCompletion: %v", rerr)
	}
	if !ok {
		t.Fatalf("no CompletionRecord found for %q", childRunID)
	}
	if rec.ParentID != "parent-run-id" {
		t.Errorf("CompletionRecord.ParentID = %q, want parent-run-id", rec.ParentID)
	}
	if rec.ExitCode != 0 || rec.Status != "ok" {
		t.Errorf("CompletionRecord = %+v, want exit_code=0 status=ok", rec)
	}
	if rec.Stdout != childRunID {
		t.Errorf("CompletionRecord.Stdout = %q, want %q", rec.Stdout, childRunID)
	}
}

// TestSubmit_RecordsCompletionOnFailure proves the completion record is
// written on the error return path too (`finish` wraps every return, not just
// the clean-success one) — `agent-shell wait`/`logs` must work for a failed
// run, not only a successful one.
func TestSubmit_RecordsCompletionOnFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	exe := filepath.Join(t.TempDir(), "fake-agent")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nprintf bad >&2\nexit 7\n"), 0700); err != nil {
		t.Fatal(err)
	}
	got, err := Submit(context.Background(), Request{
		Agent:      "claude",
		Prompt:     "ignored",
		Executable: exe,
		Timeout:    5 * time.Second,
	})
	if err == nil {
		t.Fatal("err = nil, want failure")
	}

	entries, lerr := agentregistry.List()
	if lerr != nil {
		t.Fatalf("List: %v", lerr)
	}
	if len(entries) != 0 {
		t.Fatalf("registry entry not removed after Submit returned: %+v", entries)
	}

	// The run's own generated ID isn't returned by Submit today, so recover it
	// the same way an operator would: it's the only completion record present.
	root, perr := agentregistry.CompletedRoot()
	if perr != nil {
		t.Fatal(perr)
	}
	dirents, derr := os.ReadDir(root)
	if derr != nil || len(dirents) != 1 {
		t.Fatalf("completed root = %v (err=%v), want exactly one record", dirents, derr)
	}
	id := strings.TrimSuffix(dirents[0].Name(), ".json")
	rec, ok, rerr := agentregistry.ReadCompletion(id)
	if rerr != nil || !ok {
		t.Fatalf("ReadCompletion(%q): ok=%v err=%v", id, ok, rerr)
	}
	if rec.ExitCode != 7 || rec.Status != "ok" || rec.Stderr != "bad" {
		t.Errorf("CompletionRecord = %+v, want exit_code=7 status=ok stderr=bad", rec)
	}
	if got.ExitCode != rec.ExitCode {
		t.Errorf("Result.ExitCode=%d != CompletionRecord.ExitCode=%d", got.ExitCode, rec.ExitCode)
	}
}

// TestSubmitDryRunRecordsNoCompletion proves a dry run never registers or
// records a completion — there is no process, so nothing exists for
// `agent-shell wait`/`logs` to ever legitimately find by an ID nobody has.
func TestSubmitDryRunRecordsNoCompletion(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	if _, err := Submit(context.Background(), Request{
		Agent:      "codex",
		Prompt:     "hello",
		Executable: "/bin/codex",
		DryRun:     true,
	}); err != nil {
		t.Fatal(err)
	}
	root, err := agentregistry.CompletedRoot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("completed root created for a dry run: stat err=%v", err)
	}
}

func TestSubmitWithFallbackSkipsMissingAgent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	dir := t.TempDir()
	codexPath := filepath.Join(dir, "codex")
	writeExecutable(t, codexPath, "#!/bin/sh\nprintf 'codex ran'\n")
	clearAgentPathEnv(t)
	t.Setenv("AGENT_SHELL_CLAUDE_PATH", filepath.Join(dir, "missing-claude"))
	t.Setenv("AGENT_SHELL_CODEX_PATH", codexPath)

	got, err := SubmitWithFallback(context.Background(), Request{
		Prompt:  "hello",
		Timeout: 5 * time.Second,
	}, []string{"claude", "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Agent != AgentCodex || got.Stdout != "codex ran" {
		t.Fatalf("result = %#v, want codex success", got)
	}
	if len(got.Attempts) != 2 || !got.Attempts[0].Skipped || got.Attempts[1].Skipped {
		t.Fatalf("attempts = %#v, want skipped claude then codex", got.Attempts)
	}
}

func TestSubmitWithFallbackAllMissing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	dir := t.TempDir()
	clearAgentPathEnv(t)
	t.Setenv("AGENT_SHELL_CLAUDE_PATH", filepath.Join(dir, "missing-claude"))
	t.Setenv("AGENT_SHELL_CODEX_PATH", filepath.Join(dir, "missing-codex"))

	got, err := SubmitWithFallback(context.Background(), Request{Prompt: "hello"}, []string{"claude", "codex"})
	if err == nil {
		t.Fatal("err = nil, want all-missing failure")
	}
	if got.ExitCode != 1 || !strings.Contains(got.Error, "no fallback agents could run") {
		t.Fatalf("result = %#v, want combined fallback error", got)
	}
	if len(got.Attempts) != 2 || !got.Attempts[0].Skipped || !got.Attempts[1].Skipped {
		t.Fatalf("attempts = %#v, want both skipped", got.Attempts)
	}
}

func TestSubmitWithFallbackSkipsUnsupportedOptions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	dir := t.TempDir()
	codexPath := filepath.Join(dir, "codex")
	writeExecutable(t, codexPath, "#!/bin/sh\nprintf 'codex handled system prompt'\n")
	clearAgentPathEnv(t)
	t.Setenv("AGENT_SHELL_CODEX_PATH", codexPath)

	got, err := SubmitWithFallback(context.Background(), Request{
		Prompt:       "hello",
		SystemPrompt: "be concise",
		Timeout:      5 * time.Second,
	}, []string{"gemini", "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Agent != AgentCodex || !strings.Contains(got.Attempts[0].Error, "gemini adapter does not support system prompts") {
		t.Fatalf("result = %#v attempts = %#v, want gemini skipped then codex", got, got.Attempts)
	}
}

func TestSubmitWithFallbackUsesAntigravityEnvOverride(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	dir := t.TempDir()
	agyPath := filepath.Join(dir, "custom-agy")
	writeExecutable(t, agyPath, "#!/bin/sh\nprintf 'agy ran'\n")
	clearAgentPathEnv(t)
	t.Setenv("AGENT_SHELL_GEMINI_PATH", filepath.Join(dir, "missing-gemini"))
	t.Setenv("AGENT_SHELL_ANTIGRAVITY_PATH", agyPath)

	got, err := SubmitWithFallback(context.Background(), Request{
		Prompt:  "hello",
		Timeout: 5 * time.Second,
	}, []string{"gemini", "antigravity"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Agent != AgentAntigravity || got.Stdout != "agy ran" {
		t.Fatalf("result = %#v, want antigravity success", got)
	}
	if len(got.Attempts) != 2 || !got.Attempts[0].Skipped || got.Attempts[1].Skipped {
		t.Fatalf("attempts = %#v, want skipped gemini then antigravity", got.Attempts)
	}
}

func TestSubmitWithFallbackDoesNotFallbackAfterStartedFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	dir := t.TempDir()
	claudePath := filepath.Join(dir, "claude")
	codexPath := filepath.Join(dir, "codex")
	writeExecutable(t, claudePath, "#!/bin/sh\nprintf 'claude failed' >&2\nexit 7\n")
	writeExecutable(t, codexPath, "#!/bin/sh\nprintf 'codex should not run'\n")
	clearAgentPathEnv(t)
	t.Setenv("AGENT_SHELL_CLAUDE_PATH", claudePath)
	t.Setenv("AGENT_SHELL_CODEX_PATH", codexPath)

	got, err := SubmitWithFallback(context.Background(), Request{
		Prompt:  "hello",
		Timeout: 5 * time.Second,
	}, []string{"claude", "codex"})
	if err == nil {
		t.Fatal("err = nil, want claude task failure")
	}
	if got.Agent != AgentClaude || got.ExitCode != 7 || got.Stderr != "claude failed" {
		t.Fatalf("result = %#v, want claude failure", got)
	}
	if len(got.Attempts) != 1 || got.Attempts[0].Skipped {
		t.Fatalf("attempts = %#v, want only started claude", got.Attempts)
	}
}

func TestSubmitWithFallbackRejectsExecutableOverride(t *testing.T) {
	_, err := SubmitWithFallback(context.Background(), Request{
		Prompt:     "hello",
		Executable: "/bin/echo",
	}, []string{"claude", "codex"})
	if err == nil || !strings.Contains(err.Error(), "executable override is not supported") {
		t.Fatalf("err = %v, want executable override error", err)
	}
}

func TestSubmitWithFallbackRejectsStream(t *testing.T) {
	_, err := SubmitWithFallback(context.Background(), Request{
		Prompt: "hello",
		Stream: true,
	}, []string{"claude", "codex"})
	if err == nil || !strings.Contains(err.Error(), "streaming is not supported with fallback agents") {
		t.Fatalf("err = %v, want streaming fallback error", err)
	}
}

func TestScanInstalledAgents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "claude"), "#!/bin/sh\nprintf '2.1.0 (Claude Code)\\n'\n")
	t.Setenv("PATH", dir)
	clearAgentPathEnv(t)

	got := ScanInstalledAgents(context.Background())
	if len(got) != len(SupportedAgents()) {
		t.Fatalf("len = %d, want all %d backends: %#v", len(got), len(SupportedAgents()), got)
	}

	byAgent := map[string]AgentInfo{}
	for _, info := range got {
		byAgent[info.Agent] = info
	}

	claude := byAgent[AgentClaude]
	if !claude.Installed || claude.Path == "" || claude.Version != "2.1.0 (Claude Code)" || claude.Error != "" {
		t.Fatalf("claude scan = %#v, want installed with version", claude)
	}

	codex := byAgent[AgentCodex]
	if codex.Installed || codex.Executable != "codex" || !strings.Contains(codex.Error, "not found") {
		t.Fatalf("codex scan = %#v, want missing", codex)
	}
}

func TestScanInstalledAgentsUsesEnvOverride(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "custom-codex")
	writeExecutable(t, exe, "#!/bin/sh\nprintf 'codex-cli 0.100.0\\n'\n")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("AGENT_SHELL_CLAUDE_PATH", "")
	t.Setenv("AGENT_SHELL_CODEX_PATH", exe)
	t.Setenv("AGENT_SHELL_GEMINI_PATH", "")
	t.Setenv("AGENT_SHELL_ANTIGRAVITY_PATH", "")
	t.Setenv("AGENT_SHELL_CURSOR_PATH", "")

	var codex AgentInfo
	for _, info := range ScanInstalledAgents(context.Background()) {
		if info.Agent == AgentCodex {
			codex = info
			break
		}
	}
	if !codex.Installed || codex.Executable != exe || codex.Path != exe || codex.Version != "codex-cli 0.100.0" {
		t.Fatalf("codex scan = %#v, want env override executable", codex)
	}
}

func TestScanInstalledAgentsAntigravityUsesEnvOverride(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "custom-agy")
	writeExecutable(t, exe, "#!/bin/sh\nprintf 'agy 1.0.2\\n'\n")
	t.Setenv("PATH", t.TempDir())
	clearAgentPathEnv(t)
	t.Setenv("AGENT_SHELL_ANTIGRAVITY_PATH", exe)

	var antigravity AgentInfo
	for _, info := range ScanInstalledAgents(context.Background()) {
		if info.Agent == AgentAntigravity {
			antigravity = info
			break
		}
	}
	if !antigravity.Installed || antigravity.Executable != exe || antigravity.Path != exe || antigravity.Version != "agy 1.0.2" {
		t.Fatalf("antigravity scan = %#v, want env override executable", antigravity)
	}
}

func TestScanInstalledAgentsVersionProbeFailureIsNonFatal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "gemini"), "#!/bin/sh\nprintf 'bad version probe' >&2\nexit 2\n")
	t.Setenv("PATH", dir)
	t.Setenv("AGENT_SHELL_CLAUDE_PATH", "")
	t.Setenv("AGENT_SHELL_CODEX_PATH", "")
	t.Setenv("AGENT_SHELL_GEMINI_PATH", "")
	t.Setenv("AGENT_SHELL_ANTIGRAVITY_PATH", "")
	t.Setenv("AGENT_SHELL_CURSOR_PATH", "")

	var gemini AgentInfo
	for _, info := range ScanInstalledAgents(context.Background()) {
		if info.Agent == AgentGemini {
			gemini = info
			break
		}
	}
	if !gemini.Installed || gemini.Version != "" || !strings.Contains(gemini.Error, "version probe failed") {
		t.Fatalf("gemini scan = %#v, want installed with version probe error", gemini)
	}
}

func assertStrings(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d\ngot:  %#v\nwant: %#v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg[%d] = %q, want %q\ngot:  %#v\nwant: %#v", i, got[i], want[i], got, want)
		}
	}
}

func writeExecutable(t *testing.T, path, script string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
}

func clearAgentPathEnv(t *testing.T) {
	t.Helper()
	t.Setenv("AGENT_SHELL_CLAUDE_PATH", "")
	t.Setenv("AGENT_SHELL_CODEX_PATH", "")
	t.Setenv("AGENT_SHELL_GEMINI_PATH", "")
	t.Setenv("AGENT_SHELL_ANTIGRAVITY_PATH", "")
	t.Setenv("AGENT_SHELL_CURSOR_PATH", "")
}

func TestResolveAgentExec_PrefersDeployBinary(t *testing.T) {
	ag := supportedAgent{Name: AgentCursor, ExecName: "cursor-agent", PathEnvVar: "AGENT_SHELL_CURSOR_PATH"}
	dir := t.TempDir()
	t.Setenv("HEROS_BIN_DIR", "") // unset → PATH fallback (unchanged behavior)

	if got := resolveAgentExec(ag); got != "cursor-agent" {
		t.Fatalf("HEROS_BIN_DIR unset: got %q, want bare ExecName", got)
	}

	// A real executable in $HEROS_BIN_DIR wins over PATH.
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(bin, "cursor-agent")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HEROS_BIN_DIR", bin)
	if got := resolveAgentExec(ag); got != exe {
		t.Fatalf("HEROS_BIN_DIR: got %q, want %q", got, exe)
	}

	// A non-executable file must NOT be picked — fall back to the bare name.
	if err := os.Chmod(exe, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolveAgentExec(ag); got != "cursor-agent" {
		t.Fatalf("non-executable candidate: got %q, want bare ExecName", got)
	}

	// An explicit per-agent override always wins.
	if err := os.Chmod(exe, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_SHELL_CURSOR_PATH", "/custom/path/cursor-agent")
	if got := resolveAgentExec(ag); got != "/custom/path/cursor-agent" {
		t.Fatalf("override: got %q, want the explicit path", got)
	}
}

// TestNormalizeAgentListDropsEmptyEntries checks empty entries. normalizeAgent maps ""
// to claude, so the `if agent == ""` guard in normalizeAgentList never fires and
// a trailing comma in --agents (or AGENT_SHELL_AGENTS) appends a backend the
// user never selected — which then runs the task and spends that account.
func TestNormalizeAgentListDropsEmptyEntries(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want []string
	}{
		{[]string{"gemini,codex,"}, []string{"gemini", "codex"}},
		{[]string{" "}, nil},
		{[]string{"codex,,gemini"}, []string{"codex", "gemini"}},
	} {
		got := normalizeAgentList(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("normalizeAgentList(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("normalizeAgentList(%q) = %v, want %v", tc.in, got, tc.want)
				break
			}
		}
	}
}

// TestSubmitAdapterRejectionResultIsAnError checks adapter failures:
// an adapter rejection returns a zero-value Result, so --json emits
// "exit_code":0 with no "status" and a consumer keying off the normalized
// Result — the contract this type exists to provide — reads a failed run as a
// clean success.
func TestSubmitAdapterRejectionResultIsAnError(t *testing.T) {
	// gemini has no system-prompt flag, so BuildCommand rejects before spawning.
	result, err := Submit(context.Background(), Request{
		Agent:        AgentGemini,
		Prompt:       "task",
		SystemPrompt: "you are a bot",
	})
	if err == nil {
		t.Fatal("expected the adapter rejection to be an error")
	}
	if result.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1 (the documented placeholder for adapter failures)", result.ExitCode)
	}
	if result.Status != "error" {
		t.Errorf("Status = %q, want \"error\"", result.Status)
	}
}

// containsPair reports whether args contains flag immediately followed by val.
// Moved here from the removed tools_test.go, whose deletion left the surviving
// antigravity --add-dir assertion as its only caller.
func containsPair(args []string, flag, val string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == val {
			return true
		}
	}
	return false
}

// TestSubmitWithFallbackExhaustedResultIsAnError is the sibling of
// TestSubmitAdapterRejectionResultIsAnError: when every candidate is rejected
// before execution, the Result carries ExitCode 1 but must also carry the
// Status the contract defines, or a --json consumer sees a failure with no
// cause-of-completion at all.
func TestSubmitWithFallbackExhaustedResultIsAnError(t *testing.T) {
	// Both adapters reject a system prompt, so no candidate ever spawns.
	result, err := SubmitWithFallback(context.Background(), Request{
		Prompt:       "task",
		SystemPrompt: "you are a bot",
	}, []string{"gemini", "cursor"})
	if err == nil {
		t.Fatal("expected an error when no candidate can run")
	}
	if result.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", result.ExitCode)
	}
	if result.Status != "error" {
		t.Errorf("Status = %q, want \"error\"", result.Status)
	}
}
