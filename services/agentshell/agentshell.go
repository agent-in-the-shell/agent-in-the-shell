// Package agentshell provides a small unified runner for local coding-agent CLIs.
package agentshell

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/agent-in-the-shell/agent-in-the-shell/internal/agentregistry"
	"github.com/agent-in-the-shell/agent-in-the-shell/internal/herospath"
	"github.com/agent-in-the-shell/agent-in-the-shell/internal/procexit"
)

const (
	AgentClaude = "claude"
	AgentCodex  = "codex"
	AgentGemini = "gemini"
	// AgentAntigravity wraps Google's Antigravity CLI. The backend name is
	// "antigravity", but the headless CLI executable is `agy`.
	AgentAntigravity = "antigravity"
	AgentCursor      = "cursor"
)

const defaultVersionProbeTimeout = 2 * time.Second

type supportedAgent struct {
	Name       string
	ExecName   string
	PathEnvVar string
}

var supportedAgents = []supportedAgent{
	{Name: AgentClaude, ExecName: "claude", PathEnvVar: "AGENT_SHELL_CLAUDE_PATH"},
	{Name: AgentCodex, ExecName: "codex", PathEnvVar: "AGENT_SHELL_CODEX_PATH"},
	{Name: AgentGemini, ExecName: "gemini", PathEnvVar: "AGENT_SHELL_GEMINI_PATH"},
	{Name: AgentAntigravity, ExecName: "agy", PathEnvVar: "AGENT_SHELL_ANTIGRAVITY_PATH"},
	// Cursor's headless agent ships as `cursor-agent` (installed via
	// `curl https://cursor.com/install | bash`). The bare `cursor` command
	// launches the editor, so it must NOT be used as the backend executable.
	{Name: AgentCursor, ExecName: "cursor-agent", PathEnvVar: "AGENT_SHELL_CURSOR_PATH"},
}

// Request describes one task submission to an underlying agent CLI.
type Request struct {
	Agent        string        `json:"agent"`
	Prompt       string        `json:"prompt"`
	CWD          string        `json:"cwd,omitempty"`
	Model        string        `json:"model,omitempty"`
	SystemPrompt string        `json:"system_prompt,omitempty"`
	Executable   string        `json:"executable,omitempty"`
	Timeout      time.Duration `json:"timeout,omitempty"`
	ExtraArgs    []string      `json:"extra_args,omitempty"`
	DryRun       bool          `json:"dry_run,omitempty"`
	Stream       bool          `json:"stream,omitempty"`
	StreamStdout io.Writer     `json:"-"`
	StreamStderr io.Writer     `json:"-"`
	// DangerouslySkipPermissions passes the backend-specific permission/approval
	// bypass flag (e.g. Claude's --dangerously-skip-permissions). agent-shell
	// provides no sandbox of its own, so the agent runs with full host access.
	DangerouslySkipPermissions bool `json:"dangerously_skip_permissions,omitempty"`
	// Env injects/overrides environment variables for the spawned process, layered
	// over the inherited os.Environ() (a key present here wins). agent-def carries
	// these from a def's env: block, ${VAR}-expanded at run time.
	Env map[string]string `json:"env,omitempty"`
	// Producer labels who launched this run in the process registry
	// (`agent-shell ps`). Empty defaults to "agent-shell"; callers that funnel
	// through Submit (e.g. `agent run <def>`) set their own label so `ps`
	// attributes the run to the right producer.
	Producer string `json:"producer,omitempty"`
}

// Result is the normalized outcome of an agent CLI execution.
type Result struct {
	Agent      string    `json:"agent"`
	Command    []string  `json:"command"`
	CWD        string    `json:"cwd,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	DurationMs int64     `json:"duration_ms"`
	// ExitCode is the process-plane result: a normal exit's 0–255 code, or
	// 128+signal when the agent was killed by a signal (SIGTERM=143,
	// SIGKILL=137, SIGPIPE=141). For start/adapter failures where no usable
	// agent result exists, 1 is used as an error placeholder.
	ExitCode int `json:"exit_code"`
	// Status is the cause-of-completion: "ok" | "signaled" | "timeout" | "error".
	Status string `json:"status,omitempty"`
	// Signal is the signal number when Status=="signaled"; 0 otherwise.
	Signal   int       `json:"signal,omitempty"`
	Stdout   string    `json:"stdout,omitempty"`
	Stderr   string    `json:"stderr,omitempty"`
	Error    string    `json:"error,omitempty"`
	DryRun   bool      `json:"dry_run,omitempty"`
	Attempts []Attempt `json:"attempts,omitempty"`
}

// Attempt describes one candidate agent considered during fallback.
type Attempt struct {
	Agent    string   `json:"agent"`
	Command  []string `json:"command,omitempty"`
	Skipped  bool     `json:"skipped"`
	ExitCode int      `json:"exit_code,omitempty"`
	Error    string   `json:"error,omitempty"`
}

// AgentInfo describes one supported agent CLI and whether it is installed.
type AgentInfo struct {
	Agent      string `json:"agent"`
	Executable string `json:"executable"`
	Path       string `json:"path,omitempty"`
	Installed  bool   `json:"installed"`
	Version    string `json:"version,omitempty"`
	Error      string `json:"error,omitempty"`
}

// SupportedAgents returns the canonical names supported by agent-shell.
func SupportedAgents() []string {
	agents := make([]string, 0, len(supportedAgents))
	for _, agent := range supportedAgents {
		agents = append(agents, agent.Name)
	}
	return agents
}

// ScanInstalledAgents reports supported agent CLIs present on PATH or via
// AGENT_SHELL_*_PATH overrides. Version probing is best-effort.
func ScanInstalledAgents(ctx context.Context) []AgentInfo {
	infos := make([]AgentInfo, 0, len(supportedAgents))
	for _, agent := range supportedAgents {
		info := scanInstalledAgent(ctx, agent)
		infos = append(infos, info)
	}
	return infos
}

// BuildCommand returns the executable and argv for req without starting it.
func BuildCommand(req Request) (string, []string, error) {
	agent := normalizeAgent(req.Agent)
	if agent == "" {
		return "", nil, errors.New("agent is required")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return "", nil, errors.New("prompt is required")
	}

	execPath := req.Executable
	if execPath == "" {
		execPath = defaultExecutable(agent)
	}
	if execPath == "" {
		return "", nil, fmt.Errorf("unsupported agent %q", req.Agent)
	}

	req.Timeout = effectiveTimeout(req.Timeout)
	args, err := buildArgs(agent, req)
	if err != nil {
		return "", nil, err
	}
	args = append(args, req.ExtraArgs...)
	return execPath, args, nil
}

// Submit executes req and captures stdout/stderr. When req.Stream is true and
// stream sinks are supplied, process output is also written to those sinks as it
// arrives while still being captured in the returned Result.
func Submit(ctx context.Context, req Request) (Result, error) {
	execPath, args, err := BuildCommand(req)
	if err != nil {
		// An adapter rejection produces no usable agent result, so the Result
		// carries the documented error placeholder rather than a zero value a
		// consumer would read as a clean success.
		return Result{Agent: normalizeAgent(req.Agent), ExitCode: 1, Status: "error", Error: err.Error()}, err
	}

	cwd := req.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	result := Result{
		Agent:   normalizeAgent(req.Agent),
		Command: append([]string{execPath}, args...),
		CWD:     cwd,
		DryRun:  req.DryRun,
	}
	if req.DryRun {
		return result, nil
	}

	timeout := effectiveTimeout(req.Timeout)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, execPath, args...)
	// Kill the whole process tree (not just the direct child) on cancel/timeout,
	// so a subagent that forks children does not orphan them.
	setupProcessGroup(cmd)
	if result.Agent == AgentAntigravity {
		cmd.Stdin = nil
	}
	if req.CWD != "" {
		cmd.Dir = req.CWD
	}
	// runID is this run's own registry ID, generated up front (rather than
	// inline at Register) so it can also be exported as AGENT_RUN_ID into the
	// child's environment: if that child itself spawns a nested agent-shell/
	// agent-sched run, that run reads AGENT_RUN_ID back out as its own
	// ParentID, giving `agent-shell ps --tree` a real process tree. parentID is
	// this process's own ParentID, read the same way from its own environment.
	runID := uuid.NewString()
	parentID := os.Getenv("AGENT_RUN_ID")
	env := make(map[string]string, len(req.Env)+1)
	for k, v := range req.Env {
		env[k] = v
	}
	env["AGENT_RUN_ID"] = runID
	cmd.Env = mergeEnv(os.Environ(), env)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if req.Stream {
		if req.StreamStdout != nil {
			cmd.Stdout = io.MultiWriter(&stdout, req.StreamStdout)
		}
		if req.StreamStderr != nil {
			cmd.Stderr = io.MultiWriter(&stderr, req.StreamStderr)
		}
	}

	result.StartedAt = time.Now().UTC()
	// Start + register + wait (== cmd.Run, with a registry entry in between) so
	// `agent-shell ps` can list this run and `agent-shell kill/signal` can control
	// it. The daemonless registry entry lives for the run's duration and is removed
	// on return. Registration is best-effort: a failure must never break the run.
	registered := false
	runErr := cmd.Start()
	if runErr == nil {
		if h, regErr := agentregistry.Register(agentregistry.Entry{
			ID:        runID,
			ParentID:  parentID,
			PID:       cmd.Process.Pid,
			PGID:      cmd.Process.Pid, // setupProcessGroup makes the child its own group leader
			Command:   strings.Join(result.Command, " "),
			Backend:   cmp.Or(req.Producer, "agent-shell"),
			StartedAt: result.StartedAt,
		}); regErr == nil {
			registered = true
			defer h.Close()
		}
		runErr = cmd.Wait()
	}
	result.FinishedAt = time.Now().UTC()
	result.DurationMs = result.FinishedAt.Sub(result.StartedAt).Milliseconds()
	result.Stdout = stdout.String()
	result.Stderr = stderr.String()

	// finish records a durable completion (best-effort, only when this run was
	// actually registered — a dry run or a Start failure never appeared in
	// `agent-shell ps`, so there is nothing for `wait`/`logs` to look up) and
	// returns its arguments unchanged, so every return path stays a one-liner.
	// Called before returning (i.e. before the deferred h.Close() runs), so a
	// caller polling `wait`/`logs` never observes the entry gone with no
	// completion recorded yet.
	finish := func(res Result, err error) (Result, error) {
		if registered {
			_ = agentregistry.RecordCompletion(agentregistry.CompletionRecord{
				ID:         runID,
				ParentID:   parentID,
				Command:    strings.Join(res.Command, " "),
				Backend:    cmp.Or(req.Producer, "agent-shell"),
				StartedAt:  res.StartedAt,
				FinishedAt: res.FinishedAt,
				ExitCode:   res.ExitCode,
				Signal:     res.Signal,
				Status:     res.Status,
				Stdout:     agentregistry.TailBytes(res.Stdout, agentregistry.MaxCompletionOutputBytes),
				Stderr:     agentregistry.TailBytes(res.Stderr, agentregistry.MaxCompletionOutputBytes),
			})
		}
		return res, err
	}

	if runErr != nil {
		d := procexit.Decode(runErr)
		result.ExitCode, result.Signal = d.ExitCode, d.Signal
		switch {
		case errors.Is(runCtx.Err(), context.DeadlineExceeded):
			// Mechanism (SIGKILL) lives in ExitCode (137); Signal is reserved for
			// Status=="signaled", so clear it on timeout to honor the contract.
			result.Status = "timeout"
			result.Signal = 0
			if !d.Known { // killed by the deadline (SIGKILL) before producing an exit status
				result.ExitCode = 137
			}
			if result.Stderr != "" && !strings.HasSuffix(result.Stderr, "\n") {
				result.Stderr += "\n"
			}
			result.Stderr += fmt.Sprintf("agent-shell: timed out after %s\n", timeout)
		case d.Signaled:
			result.Status = "signaled"
		case d.Known:
			result.Status = "ok" // includes nonzero clean exits
		default:
			result.Status = "error" // start/pipe failure; ExitCode is a placeholder
		}
		result.Error = runErr.Error()
		if _, ok := runErr.(*exec.Error); ok {
			result.Error += "; run `agent-shell agents` to inspect installed agents"
		}
		return finish(result, runErr)
	}

	result.Status = "ok"
	if result.Agent == AgentAntigravity && strings.TrimSpace(result.Stdout) == "" {
		result.Status = "error"
		result.ExitCode = 1
		result.Error = "antigravity adapter returned empty stdout; run `agy auth login` once on this host and retry. If authenticated, this may be the upstream agy headless empty-output issue tracked in google-antigravity/antigravity-cli #76/#187."
		if result.Stderr != "" && !strings.HasSuffix(result.Stderr, "\n") {
			result.Stderr += "\n"
		}
		result.Stderr += "agent-shell: " + result.Error + "\n"
		return finish(result, errors.New(result.Error))
	}
	return finish(result, nil)
}

// SubmitWithFallback tries candidate agents in order until one can run req.
// It falls back only for pre-execution failures: unsupported adapter options,
// missing executables, or process start errors. If an agent starts and exits
// nonzero, its task-level failure is returned without trying later agents.
func SubmitWithFallback(ctx context.Context, req Request, agents []string) (Result, error) {
	if strings.TrimSpace(req.Executable) != "" {
		return Result{}, errors.New("executable override is not supported with fallback agents")
	}
	if req.Stream {
		return Result{}, errors.New("streaming is not supported with fallback agents; use --agent instead of --agents")
	}
	agents = normalizeAgentList(agents)
	if len(agents) == 0 {
		return Result{}, errors.New("at least one fallback agent is required")
	}

	var attempts []Attempt
	var messages []string
	for _, agent := range agents {
		candidate := req
		candidate.Agent = agent
		candidate.Executable = ""

		execPath, args, err := BuildCommand(candidate)
		attempt := Attempt{Agent: normalizeAgent(agent)}
		if err != nil {
			attempt.Skipped = true
			attempt.Error = err.Error()
			attempts = append(attempts, attempt)
			messages = append(messages, fmt.Sprintf("%s: %s", attempt.Agent, attempt.Error))
			continue
		}
		attempt.Command = append([]string{execPath}, args...)

		if _, err := exec.LookPath(execPath); err != nil {
			attempt.Skipped = true
			attempt.Error = missingExecutableError(execPath)
			attempts = append(attempts, attempt)
			messages = append(messages, fmt.Sprintf("%s: %s", attempt.Agent, attempt.Error))
			continue
		}

		result, err := Submit(ctx, candidate)
		if err == nil {
			attempt.ExitCode = result.ExitCode
			result.Attempts = append(attempts, attempt)
			return result, nil
		}

		attempt.ExitCode = result.ExitCode
		attempt.Error = result.Error
		if _, ok := err.(*exec.Error); ok {
			attempt.Skipped = true
			attempts = append(attempts, attempt)
			messages = append(messages, fmt.Sprintf("%s: %s", attempt.Agent, attempt.Error))
			continue
		}

		result.Attempts = append(attempts, attempt)
		return result, err
	}

	err := fmt.Errorf("no fallback agents could run: %s", strings.Join(messages, "; "))
	return Result{
		Agent:    agents[0],
		ExitCode: 1,
		Status:   "error", // no candidate ever spawned, so there is no agent result
		Error:    err.Error(),
		Attempts: attempts,
		DryRun:   req.DryRun,
	}, err
}

// NormalizeAgent canonicalizes an agent name, resolving accepted aliases
// (e.g. "code" -> codex, "agy" -> antigravity) to the supported agent names.
func NormalizeAgent(agent string) string {
	return normalizeAgent(agent)
}

func normalizeAgent(agent string) string {
	switch strings.ToLower(strings.TrimSpace(agent)) {
	case "", AgentClaude:
		return AgentClaude
	case "code", AgentCodex:
		return AgentCodex
	case AgentGemini:
		return AgentGemini
	case "agy", AgentAntigravity:
		return AgentAntigravity
	case "cursor-agent", AgentCursor:
		return AgentCursor
	default:
		return strings.ToLower(strings.TrimSpace(agent))
	}
}

func normalizeAgentList(agents []string) []string {
	seen := map[string]struct{}{}
	normalized := make([]string, 0, len(agents))
	for _, entry := range agents {
		for _, part := range strings.Split(entry, ",") {
			// Drop blanks BEFORE normalizing: normalizeAgent maps "" to claude
			// (the right default for a single --agent), so checking after would
			// turn a trailing comma into an unrequested claude fallback.
			if strings.TrimSpace(part) == "" {
				continue
			}
			agent := normalizeAgent(part)
			if _, ok := seen[agent]; ok {
				continue
			}
			seen[agent] = struct{}{}
			normalized = append(normalized, agent)
		}
	}
	return normalized
}

func defaultExecutable(agent string) string {
	for _, supported := range supportedAgents {
		if supported.Name == agent {
			return resolveAgentExec(supported)
		}
	}
	return ""
}

func missingExecutableError(executable string) string {
	if filepath.IsAbs(executable) || strings.ContainsRune(executable, os.PathSeparator) {
		return fmt.Sprintf("executable %q not found or not executable", executable)
	}
	return "executable not found in PATH"
}

func effectiveTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return 30 * time.Minute
	}
	return timeout
}

// mergeEnv layers extra over a base "KEY=VALUE" environment: a key present in
// both is overridden in place (keeping base order); new keys are appended in
// sorted order so the spawned process env is deterministic. Returns base
// unchanged when extra is empty.
func mergeEnv(base []string, extra map[string]string) []string {
	if len(extra) == 0 {
		return base
	}
	out := make([]string, len(base))
	copy(out, base)
	at := make(map[string]int, len(base))
	for i, kv := range out {
		if eq := strings.IndexByte(kv, '='); eq >= 0 {
			at[kv[:eq]] = i
		}
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		kv := k + "=" + extra[k]
		if i, ok := at[k]; ok {
			out[i] = kv
		} else {
			out = append(out, kv)
		}
	}
	return out
}

func buildArgs(agent string, req Request) ([]string, error) {
	switch agent {
	case AgentClaude:
		outputFormat := "text"
		if req.Stream {
			outputFormat = "stream-json"
		}
		args := []string{"-p", req.Prompt, "--output-format", outputFormat}
		if req.Stream {
			args = append(args, "--verbose", "--include-partial-messages")
		}
		if req.Model != "" {
			args = append(args, "--model", req.Model)
		}
		if req.SystemPrompt != "" {
			args = append(args, "--system-prompt", req.SystemPrompt)
		}
		if req.DangerouslySkipPermissions {
			args = append(args, "--dangerously-skip-permissions")
		}
		return args, nil
	case AgentCodex:
		args := []string{"exec"}
		if req.Stream {
			args = append(args, "--json") // codex emits JSONL events under --json
		}
		if req.DangerouslySkipPermissions {
			args = append(args, "--dangerously-bypass-approvals-and-sandbox")
		}
		if req.CWD != "" {
			args = append(args, "--cd", req.CWD)
		}
		if req.Model != "" {
			args = append(args, "--model", req.Model)
		}
		if req.SystemPrompt != "" {
			args = append(args, "--config", "instructions="+quoteTOMLString(req.SystemPrompt))
		}
		args = append(args, req.Prompt)
		return args, nil
	case AgentGemini:
		outputFormat := "text"
		if req.Stream {
			outputFormat = "stream-json"
		}
		args := []string{"--prompt", req.Prompt, "--output-format", outputFormat}
		if req.Model != "" {
			args = append(args, "--model", req.Model)
		}
		if req.SystemPrompt != "" {
			return nil, errors.New("gemini adapter does not support system prompts")
		}
		if req.DangerouslySkipPermissions {
			args = append(args, "--approval-mode", "yolo")
		}
		return args, nil
	case AgentAntigravity:
		if req.SystemPrompt != "" {
			return nil, errors.New("antigravity adapter does not support system prompts")
		}
		args := []string{"--print", req.Prompt, "--print-timeout", effectiveTimeout(req.Timeout).String()}
		if req.Model != "" {
			args = append(args, "--model", req.Model)
		}
		if req.CWD != "" {
			absCWD, err := filepath.Abs(req.CWD)
			if err != nil {
				return nil, fmt.Errorf("antigravity adapter resolve --cwd: %w", err)
			}
			args = append(args, "--add-dir", absCWD)
		}
		if req.DangerouslySkipPermissions {
			args = append(args, "--dangerously-skip-permissions")
		}
		return args, nil
	case AgentCursor:
		// Cursor's headless agent: `cursor-agent -p [flags] "<prompt>"`.
		// `-p`/`--print` is a boolean non-interactive print flag; the prompt is
		// a positional argument (not the value of -p). --output-format accepts
		// text|json|stream-json. Verified against current Cursor CLI docs
		// (cursor.com/docs/cli/reference/parameters, /cli/headless) on
		// 2026-06-03.
		outputFormat := "text"
		if req.Stream {
			outputFormat = "stream-json"
		}
		args := []string{"-p", "--output-format", outputFormat}
		if req.Stream {
			// Surface incremental text deltas in stream-json mode.
			args = append(args, "--stream-partial-output")
		}
		if req.Model != "" {
			args = append(args, "--model", req.Model)
		}
		if req.SystemPrompt != "" {
			// Cursor's CLI exposes no system-prompt/custom-instructions flag;
			// reject rather than silently dropping it (mirrors gemini).
			return nil, errors.New("cursor adapter does not support system prompts")
		}
		if req.CWD != "" {
			// Cursor uses --workspace (not --cwd) to select the working dir.
			args = append(args, "--workspace", req.CWD)
		}
		if req.DangerouslySkipPermissions {
			// --force ("yolo") force-allows command/file actions in print mode.
			args = append(args, "--force")
		}
		args = append(args, req.Prompt)
		return args, nil
	default:
		return nil, fmt.Errorf("unsupported agent %q", agent)
	}
}

func quoteTOMLString(s string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
	return `"` + replacer.Replace(s) + `"`
}

func scanInstalledAgent(ctx context.Context, agent supportedAgent) AgentInfo {
	executable := resolveAgentExec(agent)
	info := AgentInfo{
		Agent:      agent.Name,
		Executable: executable,
	}

	path, err := exec.LookPath(executable)
	if err != nil {
		info.Error = missingExecutableError(executable)
		return info
	}

	info.Path = path
	info.Installed = true
	version, err := detectVersion(ctx, path, defaultVersionProbeTimeout)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	info.Version = version
	return info
}

func detectVersion(ctx context.Context, executable string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = defaultVersionProbeTimeout
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, executable, "--version")
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output

	if err := cmd.Run(); err != nil {
		if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("version probe timed out after %s", timeout)
		}
		msg := strings.TrimSpace(output.String())
		if msg != "" {
			return "", fmt.Errorf("version probe failed: %s", firstLine(msg))
		}
		return "", fmt.Errorf("version probe failed: %w", err)
	}

	version := strings.TrimSpace(output.String())
	if version == "" {
		return "", nil
	}
	return firstLine(version), nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// resolveAgentExec picks a backend agent's executable, preferring the DEPLOY
// binary over a stale PATH shadow (e.g. a ~/go/bin go-install copy that predates
// the deploy and wins on the login PATH — the shadow-drift the deploy
// prevents for scheduled jobs but not for an interactive agent-shell).
// Resolution order:
//  1. the agent's explicit path env var (an operator override always wins);
//  2. $HEROS_BIN_DIR/<ExecName> when HEROS_BIN_DIR is set AND holds an executable
//     of that name — so a context that sets the deploy bin dir (the deploy jobs,
//     or an operator who exports it) runs the deployed binary, not a PATH shadow;
//  3. the bare ExecName (PATH-resolved by the caller — unchanged historical
//     behavior, so nothing changes where HEROS_BIN_DIR is unset).
func resolveAgentExec(agent supportedAgent) string {
	if v := strings.TrimSpace(os.Getenv(agent.PathEnvVar)); v != "" {
		return v
	}
	// The deploy-binary-over-PATH-shadow rule, from the shared helper.
	return herospath.ResolveBin(agent.ExecName)
}
