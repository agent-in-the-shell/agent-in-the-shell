// Package shellcli holds the agent-shell command glue shared by both the
// standalone `agent-shell` binary and the `shell` subcommand of the `agent`
// root dispatcher, so the two invocation forms run identical code.
package shellcli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"
	// Embed the IANA timezone database so reset times render in the system
	// zone even on scratch/Alpine/distroless images that ship no zoneinfo.
	_ "time/tzdata"

	"github.com/agent-in-the-shell/agent-in-the-shell/internal/agentregistry"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentshell"
)

// errHelpRequested unwinds a hand-rolled parser that already printed --help.
// (Named for limits when that command owned it; install is the remaining user.)
var errHelpRequested = errors.New("help requested")

var osExit = os.Exit

const usage = `agent-shell: unified CLI for submitting tasks to local agents.

Usage:
  agent-shell submit [flags] <task>
  agent-shell run [flags] <task>
  agent-shell agents [--json]
  agent-shell install [agent] [--all] [--check] [--dry-run] [--yes] [--strict] [--json]
  agent-shell doctor [agent] [--json]   (alias for: install --check --all)
  agent-shell ps [--json] [--tree]         List running agents on this host
  agent-shell kill <id> [--grace d]        Reap a running agent's process group
  agent-shell signal <id> <SIG>            Send one signal (e.g. TERM, HUP, USR1, 15)
  agent-shell wait [--timeout d] <id>      Block until a run finishes; exit with its exit code
  agent-shell logs <id>                    Print a finished run's captured stdout/stderr

Flags:
  --agent string          Agent backend: claude, codex, gemini, antigravity, cursor (default "claude")
  --agents string         Ordered fallback agent list, e.g. claude,codex,gemini,antigravity
  --model string          Model name passed to the underlying agent when supported
  --cwd string            Working directory for the task
  --system-prompt string  System/developer instructions when supported
  --exec string           Override agent executable path
  --timeout duration      Execution timeout (default 30m)
  --json                  Print normalized JSON result
  --stream, --follow      Stream backend output live while still capturing it
  --dry-run               Print the command without executing it
  --dangerously-skip-permissions
                          Bypass the agent's permission/approval prompts
                          (DANGEROUS; isolated environments only). agent-shell
                          provides no sandbox, so the agent runs with full host
                          access. Rejected by adapters that have no bypass flag
                          of their own.

If <task> is "-", agent-shell reads the task from stdin.

Pipelines:
  agent-shell stage <name> [--task ID] [--tail-bytes N] -- <cmd> [args...]
      run one command as an observed pipeline stage: a live 'ps' entry for its
      duration and a durable per-stage record ('logs <id>') at the end. Wrap
      each step of a multi-stage script to get per-stage exit/output/timing
      without a workflow runtime.
`

// Run executes the agent-shell command. args is the argument list after the
// program name (os.Args[1:]) so callers can forward either `agent-shell <args>`
// or `agent shell <args>` through the same dispatch.
func Run(args []string) {
	if len(args) == 0 {
		fmt.Fprint(os.Stdout, usage)
		return
	}

	switch args[0] {
	case "submit", "run":
		runSubmit(args[1:])
	case "agents":
		runAgents(args[1:])
	case "install":
		runInstall(args[1:])
	case "doctor":
		// doctor is the discoverable read-only verb: install --check --all.
		runInstall(append([]string{"--check", "--all"}, args[1:]...))
	case "ps":
		osExit(agentregistry.RunPS(args[1:], os.Stdout, os.Stderr))
	case "kill":
		osExit(agentregistry.RunKill(args[1:], os.Stdout, os.Stderr))
	case "signal":
		osExit(agentregistry.RunSignal(args[1:], os.Stdout, os.Stderr))
	case "wait":
		osExit(agentregistry.RunWait(args[1:], os.Stdout, os.Stderr))
	case "logs":
		osExit(agentregistry.RunLogs(args[1:], os.Stdout, os.Stderr))
	case "stage":
		osExit(agentregistry.RunStage(args[1:], os.Stdin, os.Stdout, os.Stderr))
	case "-h", "--help", "help":
		fmt.Fprint(os.Stdout, usage)
	default:
		runSubmit(args)
	}
}

// definedFlagAmong reports whether any positional argument is one of the flags
// fs itself defines. Go's flag package stops parsing at the first non-flag
// argument, so `submit "task" --agent codex` leaves --agent in fs.Args() and
// readPrompt joins it into the prompt: the flag silently does nothing and the
// agent is handed its text. Erroring beats guessing.
//
// The check is against fs's OWN flag names rather than any `-`-prefixed word, so
// a prompt may still contain flag-shaped text ("explain the --deeply-nested
// flag"). Only a name this command would have acted on is ambiguous enough to
// reject, and a bare "-" stays free to mean stdin.
func definedFlagAmong(fs *flag.FlagSet, positional []string) (string, bool) {
	defined := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) { defined["--"+f.Name], defined["-"+f.Name] = true, true })
	for _, arg := range positional {
		if name, _, found := strings.Cut(arg, "="); found && defined[name] {
			return arg, true
		}
		if defined[arg] {
			return arg, true
		}
	}
	return "", false
}

func runSubmit(args []string) {
	fs := flag.NewFlagSet("submit", flag.ExitOnError)
	agent := fs.String("agent", envDefault("AGENT_SHELL_AGENT", agentshell.AgentClaude), "Agent backend: claude, codex, gemini, antigravity, cursor")
	agents := fs.String("agents", envDefault("AGENT_SHELL_AGENTS", ""), "Ordered fallback agent list, e.g. claude,codex,gemini,antigravity")
	model := fs.String("model", "", "Model name passed to the underlying agent when supported")
	cwd := fs.String("cwd", "", "Working directory for the task")
	systemPrompt := fs.String("system-prompt", "", "System/developer instructions when supported")
	executable := fs.String("exec", "", "Override agent executable path")
	timeout := fs.Duration("timeout", 30*time.Minute, "Execution timeout")
	jsonOut := fs.Bool("json", false, "Print normalized JSON result")
	stream := fs.Bool("stream", false, "Stream backend output live while still capturing it")
	follow := fs.Bool("follow", false, "Alias for --stream")
	dryRun := fs.Bool("dry-run", false, "Print the command without executing it")
	dangerouslySkipPermissions := fs.Bool("dangerously-skip-permissions", false, "Bypass the agent's permission/approval prompts (DANGEROUS; isolated environments only)")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	fs.Parse(args)

	if flagArg, ok := definedFlagAmong(fs, fs.Args()); ok {
		fatal("%s was given after the task, where Go's flag parser stops looking, so it would have been swallowed into the prompt instead of taking effect; put flags before the task", flagArg)
	}

	prompt, err := readPrompt(fs.Args(), os.Stdin)
	if err != nil {
		fatal("%v", err)
	}

	req := agentshell.Request{
		Agent:                      *agent,
		Prompt:                     prompt,
		CWD:                        *cwd,
		Model:                      *model,
		SystemPrompt:               *systemPrompt,
		Executable:                 *executable,
		Timeout:                    *timeout,
		DryRun:                     *dryRun,
		Stream:                     *stream || *follow,
		DangerouslySkipPermissions: *dangerouslySkipPermissions,
	}
	if req.Stream {
		if *jsonOut {
			req.StreamStdout = os.Stderr
			req.StreamStderr = os.Stderr
		} else {
			req.StreamStdout = os.Stdout
			req.StreamStderr = os.Stderr
		}
	}

	if *dangerouslySkipPermissions && !*dryRun {
		fmt.Fprintln(os.Stderr, "agent-shell: WARNING: permission gate bypassed; no sandbox is provided — the agent runs with full host access.")
	}

	var result agentshell.Result
	if strings.TrimSpace(*agents) != "" {
		if strings.TrimSpace(*executable) != "" {
			fatal("--exec cannot be combined with --agents")
		}
		if req.Stream {
			fatal("--stream cannot be combined with --agents")
		}
		result, err = agentshell.SubmitWithFallback(context.Background(), req, strings.Split(*agents, ","))
	} else {
		result, err = agentshell.Submit(context.Background(), req)
	}
	if err != nil && len(result.Command) == 0 && !*jsonOut {
		fatal("%v", err)
	}
	if *jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(result)
	} else if *dryRun {
		fmt.Println(shellJoin(result.Command))
	} else if req.Stream {
		if err != nil && result.Stderr == "" && result.Error != "" {
			fmt.Fprintln(os.Stderr, result.Error)
		}
	} else {
		fmt.Fprint(os.Stdout, result.Stdout)
		if result.Stderr != "" {
			fmt.Fprint(os.Stderr, result.Stderr)
		} else if err != nil && result.Error != "" {
			fmt.Fprintln(os.Stderr, result.Error)
		}
		if err == nil && len(result.Attempts) > 1 {
			fmt.Fprintf(os.Stderr, "agent-shell: fell back to %s after %d skipped agent(s)\n", result.Agent, len(result.Attempts)-1)
		}
	}

	if err != nil {
		if result.ExitCode != 0 {
			osExit(result.ExitCode)
			return
		}
		fatal("%v", err)
	}
}

func runAgents(args []string) {
	fs := flag.NewFlagSet("agents", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "Print installed-agent scan as JSON")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	fs.Parse(args)

	agents := agentshell.ScanInstalledAgents(context.Background())
	if *jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(agents)
		return
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, agent := range agents {
		status := "missing"
		if agent.Installed {
			status = "installed"
		}
		version := agent.Version
		if version == "" {
			version = "-"
		}
		path := agent.Path
		if path == "" {
			path = agent.Executable
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", agent.Agent, status, path, version)
	}
	_ = tw.Flush()
}

// runInstall handles `agent-shell install` (and, via Run, the `doctor` alias).
// It probes the requested backend(s), optionally remediates the binary half of
// missing backends, and exits with the rank-max sysexits code. --check/--dry-run
// are guaranteed read-only.
func runInstall(args []string) {
	selected, all, opts, jsonOut, err := parseInstallArgs(args)
	if err != nil {
		if errors.Is(err, errHelpRequested) {
			return
		}
		// Bad invocation: EX_USAGE (64), not the blanket fatal() 1.
		exitWith(agentshell.ExitUsage, "%v", err)
	}

	agents, err := resolveInstallAgents(selected, all)
	if err != nil {
		exitWith(agentshell.ExitUsage, "%v", err)
	}
	opts.Agents = agents

	report := agentshell.Install(context.Background(), opts, agentshell.DefaultLookup())

	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
	} else {
		printInstallReport(os.Stdout, report, opts.CheckOnly)
	}

	osExit(report.Aggregate.ExitCode)
}

// resolveInstallAgents picks the backends to act on. A single positional agent
// or --all selects the target; a bare invocation defaults to all backends
// (returned as nil, which the service resolves to every backend). More than one
// positional agent is rejected.
func resolveInstallAgents(selected []string, all bool) ([]string, error) {
	// An explicit agent beats --all. `doctor <agent>` is rewritten by Run into
	// `--check --all <agent>`, so letting --all win here would discard the very
	// argument the user typed and exit on some other backend's status.
	if len(selected) == 0 && all {
		return nil, nil
	}
	if len(selected) > 1 {
		return nil, fmt.Errorf("install accepts at most one agent (got %v); use --all for every backend", selected)
	}
	if len(selected) == 1 {
		name := agentshell.NormalizeAgent(selected[0])
		if _, ok := agentshell.InstallHint(name); !ok {
			return nil, fmt.Errorf("unsupported agent %q (supported: %v)", selected[0], agentshell.BackendNames())
		}
		return []string{name}, nil
	}
	return nil, nil
}

func parseInstallArgs(args []string) (selected []string, all bool, opts agentshell.InstallOptions, jsonOut bool, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--all":
			all = true
		case arg == "--check":
			opts.CheckOnly = true
		case arg == "--dry-run":
			opts.DryRun = true
		case arg == "--yes" || arg == "-y":
			opts.Yes = true
		case arg == "--strict":
			opts.Strict = true
		case arg == "--json":
			jsonOut = true
		case arg == "-h" || arg == "--help":
			fmt.Fprint(os.Stdout, usage)
			return nil, false, opts, false, errHelpRequested
		case strings.HasPrefix(arg, "-"):
			return nil, false, opts, false, fmt.Errorf("unknown install flag %q", arg)
		default:
			selected = append(selected, arg)
		}
	}
	if opts.CheckOnly && opts.Yes {
		return nil, false, opts, false, fmt.Errorf("--check is read-only and cannot be combined with --yes")
	}
	return selected, all, opts, jsonOut, nil
}

func printInstallReport(w io.Writer, report agentshell.InstallReport, checkOnly bool) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tSTATUS\tPATH\tDETAIL")
	for _, b := range report.Backends {
		path := b.ResolvedPath
		if path == "" {
			path = b.Binary
		}
		detail := b.Detail
		if detail == "" {
			detail = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", b.Agent, b.Status, path, detail)
	}
	_ = tw.Flush()

	if !checkOnly && len(report.Actions) > 0 {
		fmt.Fprintln(w)
		for _, a := range report.Actions {
			switch a.Action {
			case "install":
				outcome := "ok"
				if !a.Succeeded {
					outcome = "failed"
				}
				fmt.Fprintf(w, "install %s: ran %q (%s)", a.Agent, a.Command, outcome)
				if a.Detail != "" {
					fmt.Fprintf(w, " — %s", a.Detail)
				}
				fmt.Fprintln(w)
			case "guide":
				if a.Command != "" {
					fmt.Fprintf(w, "install %s: %s\n", a.Agent, a.Command)
				}
				if a.Detail != "" {
					fmt.Fprintf(w, "  %s\n", a.Detail)
				}
			}
		}
	}

	worst := report.Aggregate.WorstStatus
	fmt.Fprintf(w, "\nworst: %s  exit: %d\n", worst, report.Aggregate.ExitCode)
}

func readPrompt(args []string, r io.Reader) (string, error) {
	if len(args) > 0 {
		prompt := strings.Join(args, " ")
		if prompt != "-" {
			return prompt, nil
		}
	}

	data, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	prompt := strings.TrimSpace(string(data))
	if prompt == "" {
		return "", fmt.Errorf("task prompt is required")
	}
	return prompt, nil
}

func envDefault(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

func shellJoin(parts []string) string {
	quoted := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			quoted = append(quoted, "''")
			continue
		}
		if strings.ContainsAny(part, " \t\n'\"\\$`!*?[]{}()<>|&;") {
			quoted = append(quoted, "'"+strings.ReplaceAll(part, "'", "'\\''")+"'")
			continue
		}
		quoted = append(quoted, part)
	}
	return strings.Join(quoted, " ")
}

func fatal(format string, args ...any) {
	exitWith(1, format, args...)
}

// exitWith prints an agent-shell error and exits with an explicit code, letting
// the install/doctor path emit sysexits codes (e.g. 64 EX_USAGE) instead of the
// blanket 1 that fatal uses for the legacy command surface.
func exitWith(code int, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "agent-shell: "+format+"\n", args...)
	osExit(code)
}
