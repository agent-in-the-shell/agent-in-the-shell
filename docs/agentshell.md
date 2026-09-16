# AgentShell

`agent-shell` is a small unified command-line interface for submitting one-off
tasks to local agent CLIs.

Source: `services/agentshell/` and `cmd/agent-shell/`

> Confused by agent-model vs agent-shell? They sit at different layers:
> agent-model is the LLM gateway that serves an OpenAI-compatible API;
> agent-shell execs the vendor coding-agent CLIs you already have installed.
> agent-shell never talks to agent-model and holds no credentials of its own —
> every command works with agent-model absent. Subscription quota moved to
> `agent-model limits`: reporting an account's quota requires that account's
> credential, and agent-model is the process that owns it.

## Build

```sh
go build -o build/ ./cmd/agent-shell
```

## Commands

```sh
agent-shell submit --agent claude "summarize this repo"
agent-shell submit --agent claude --stream "fix the failing tests"
agent-shell submit --agent codex --cwd /path/to/repo "fix the failing tests"
agent-shell submit --agents claude,codex,gemini,antigravity "summarize this repo"
agent-shell submit --agent antigravity --cwd /path/to/repo "fix the failing tests"
agent-shell submit --agent cursor --dry-run "hello"
agent-shell run --agent gemini --model gemini-2.5-pro "review this design"
agent-shell ps
agent-shell ps --json
agent-shell ps --tree
agent-shell signal <id> TERM
agent-shell kill --grace 10s <id>
agent-shell wait <id>
agent-shell logs <id>
printf "write release notes" | agent-shell submit --agent claude -
agent-shell agents
agent-shell agents --json
agent-shell doctor
agent-shell install --check --all
agent-shell install claude
agent-shell install gemini --dry-run
agent-shell install codex --yes
agent-shell stage build -- go build ./...
```

`run` is an alias for `submit`. If no subcommand is supplied, non-flag
arguments are treated as a submit task. Run `agent-shell --help` or
`agent-shell` with no arguments to print help.

## Supported Agents

| Agent | Default executable | Invocation |
|---|---|---|
| `claude` | `claude` | `claude -p <task> --output-format text` |
| `codex` | `codex` | `codex exec <task>` (`--stream` adds `--json`) |
| `gemini` | `gemini` | `gemini --prompt <task> --output-format text` |
| `antigravity` | `agy` | `agy --print <task> --print-timeout <timeout>` (`--cwd` also adds `--add-dir <cwd>`) |
| `cursor` | `cursor-agent` | `cursor-agent -p --output-format text <task>` |

> **Antigravity (experimental):** the backend name is `antigravity`; the
> executable is `agy`. `agy` is accepted as an alias for `--agent`. The
> non-interactive invocation is `agy --print <task> --print-timeout <timeout>`
> with the prompt immediately after `--print`; when `--cwd` is set, `agent-shell`
> also sets the process working directory and passes an absolute
> `--add-dir <cwd>`. `--model` maps to
> `--model`; `--system-prompt` is rejected because Antigravity CLI exposes no
> equivalent flag. `--timeout` maps to `--print-timeout` so `agy`'s print-mode
> timeout stays aligned with agent-shell's timeout. `--dangerously-skip-permissions`
> maps to `--dangerously-skip-permissions`.
>
> `agy` currently requires a one-time interactive `agy auth login` on the host
> and does not have an API-key/CI auth path. The adapter treats exit-0 with
> empty stdout as a failure and points at the auth precondition because current
> Antigravity builds can otherwise appear to succeed while returning no model
> output in headless subprocesses. Newer `agy` flags can be passed by API
> callers through `Request.ExtraArgs`; `agent-shell` intentionally maps only
> stable flags.

> **Cursor:** the backend executable is `cursor-agent` (the headless terminal
> agent installed via `curl https://cursor.com/install -fsS | bash`), **not** the
> bare `cursor` command, which launches the editor. `cursor` (and the
> `cursor-agent` alias) are accepted backend names. `--model` maps to
> `--model`, `--cwd` maps to `--workspace`, and `--dangerously-skip-permissions`
> maps to `--force`. Cursor's CLI exposes no system-prompt flag, so
> `--system-prompt` is rejected before execution (like Gemini). The prompt is a
> positional argument; `-p`/`--print` is the boolean non-interactive print flag.
> Cursor requires authentication (`cursor-agent login` or `CURSOR_API_KEY`)
> before headless runs; `agent-shell` surfaces the CLI's own auth error.

### Gemini to Antigravity migration

Google Antigravity CLI is an additional backend, not a replacement for the
existing `gemini` adapter. Keep using `--agent gemini` for Gemini CLI installs
that still authenticate through paid API-key, Vertex, or enterprise paths.

Users affected by the Gemini CLI consumer-tier shutdown on June 18, 2026
(Google AI Pro, Ultra, free, or individual Gemini Code Assist auth) should
bootstrap Antigravity with `agy auth login`, migrate Gemini CLI plugins with
`agy plugin import gemini` where applicable, then switch scripts from:

```sh
agent-shell submit --agent gemini "task"
```

to:

```sh
agent-shell submit --agent antigravity "task"
```

## Flags

`submit` and `run` accept:

```text
--agent string          Agent backend: claude, codex, gemini, antigravity, cursor
--agents string         Ordered fallback agent list, e.g. claude,codex,gemini,antigravity
--model string          Model name passed to the underlying agent when supported
--cwd string            Working directory for the task when supported
--system-prompt string  System/developer instructions when supported
--exec string           Override agent executable path
--timeout duration      Execution timeout (default 30m)
--json                  Print normalized JSON result
--stream, --follow      Stream backend output live while still capturing it
--dry-run               Print the command without executing it
--dangerously-skip-permissions
                        Bypass the agent's permission/approval prompts
```

**Flags must precede the task.** Go's flag parser stops at the first non-flag
argument, so anything after the task would be joined into the prompt rather than
taking effect. `agent-shell submit "task" --agent codex` is rejected with an
error naming the flag instead of silently running on `claude` with a polluted
prompt. A prompt may still contain flag-shaped words that are not flags this
command defines (`"explain the --deeply-nested flag"`), and a bare `-` still
means stdin.

Set `AGENT_SHELL_AGENT` to change the default backend from `claude`. The
same variable also scopes a bare `instead of fanning out to all supported ones.
Set `AGENT_SHELL_AGENTS` to configure a default ordered fallback list.

`code`, `agy`, and `cursor-agent` are accepted as aliases for `codex`,
`antigravity`, and `cursor`, respectively.

## Go API surface

`agentshell.Submit` accepts a `Request` with three fields the CLI does not expose
as flags. They exist for in-process callers that wrap a submission.

| Field | Effect |
|---|---|
| `ExtraArgs []string` | Appended verbatim after the mapped flags. The escape hatch for vendor flags agent-shell doesn't map. |
| `Env map[string]string` | Layered over the inherited `os.Environ()`; a key present here wins. |
| `Producer string` | The label this run carries in `agent-shell ps`. Defaults to `agent-shell`. |

> **Removed in the MVP.** `Tools`, `Profile`, `MCPConfig`, `WorkspaceWrite`,
> `Worktree`/`WorktreeKeep`/`WorktreeDir` and `OutputFormat` were withdrawn along
> with the worktree isolation, capability-sandbox and JSON-envelope features they
> drove. Two of them (`Tools`, `WorkspaceWrite`) shipped fail-open — a requested
> restriction the backend could not enforce was silently dropped rather than
> rejected — so their contract was not one a caller should have relied on. If they
> return, they return with that contract enforced and tested.

### Result contract

`Result` separates the process plane from the cause of completion, so a caller
can tell a task that failed from a task that was killed:

| Field | Meaning |
|---|---|
| `Status` | `ok` \| `signaled` \| `timeout` \| `error`. `ok` includes a clean nonzero exit — the agent ran and reported failure. `error` means the process never produced a usable status: a start or pipe failure, or an adapter rejecting the request before spawning. |
| `ExitCode` | A normal exit's 0–255 code, or 128+signal when killed by one (SIGTERM 143, SIGKILL 137, SIGPIPE 141). `1` is the placeholder where no agent result exists. |
| `Signal` | The signal number when `Status == "signaled"`; `0` otherwise — including on timeout, where the SIGKILL lives in `ExitCode` (137) instead. |

A timeout also appends `agent-shell: timed out after <d>` to `Stderr`. This is
the child-process passthrough that the install/doctor [exit codes](#exit-codes)
section contrasts its own sysexits namespace against.

## Streaming Output

By default, `agent-shell submit` captures backend stdout and stderr and prints
them after the agent exits. Use `--stream` (or its alias `--follow`) to surface
backend output as it arrives while still preserving `Result.Stdout` and
`Result.Stderr`:

```sh
agent-shell submit --agent claude --stream "implement issue "
```

For Claude, `--stream` switches the backend invocation to
`--output-format stream-json --verbose --include-partial-messages`. For Gemini,
it uses `--output-format stream-json`. For Codex, it uses `codex exec --json`.
For Cursor, it uses `--output-format stream-json --stream-partial-output`.
For Antigravity, there is no separate event format; `agent-shell` streams the
stdout/stderr produced by `agy --print`. These modes emit backend event
JSONL where the backend supports that format, not the interactive TUI.

When `--json` and `--stream` are combined, live stream output is written to
stderr so stdout remains the final normalized `Result` JSON. `--stream` is
single-agent only and cannot be combined with `--agents`, because fallback
normally hides skipped pre-execution candidates before selecting the agent that
actually runs.

## Process Registry

Every non-dry-run `submit`/`run` attempts to record the spawned agent in a
daemonless per-user registry after the process starts and removes the entry when
the run returns. Registration is best-effort for observability and control; if
it fails, the run still proceeds.

```sh
agent-shell ps
agent-shell ps --json
agent-shell ps --tree
agent-shell signal <id> TERM
agent-shell kill --grace 10s <id>
agent-shell wait --timeout 5m <id>
agent-shell logs <id>
agent-shell stage build -- go build ./...
```

`stage <name> [--task ID] [--tail-bytes N] -- <cmd> [args...]` wraps one command
in the registry, streams its output live, retains a bounded stdout/stderr tail
for the completion record, and propagates the command's exit code.

`ps` lists running-agent registry entries newest first. The table shows `ID`,
`PID`, `PGID`, `STATUS`, `STARTED`, `UPTIME`, and `COMMAND`; `--json` emits the
entries as JSON, including the derived `status` field (`running` or `stale`). A
stale entry means the recorded process is gone, no longer matches the original
process identity, or its identity cannot be confirmed.

`--tree` indents each run under its parent, using the `ParentID` recorded from
the `AGENT_RUN_ID` environment variable: every registered run's own ID is
exported as `AGENT_RUN_ID` into its child's environment, so registered work
launched by a nested `agent-shell`, `agent run`, `agent-react`, `agent-sched`,
or `agent-flow` process records that value as its `ParentID`. A run whose parent
already exited (or was never a registered run) renders at the top level rather
than being dropped.

`signal` sends one signal to the registered process group after rechecking the
entry. On Unix, it accepts names such as `TERM`, `SIGTERM`, `HUP`, `USR1`, or a
signal number. `kill` sends `SIGTERM`, waits for `--grace` (default `5s`), then
sends `SIGKILL` to survivors. Both commands refuse absent, stale, or
identity-unconfirmed entries. `kill` reports success only after the process
group is gone, then attempts to remove the registry entry; producer cleanup may
race that removal. On Windows, process identity and process-group behavior are
best-effort and only terminating signals are honored.

`wait` polls for a run's durable completion record, then prints its exit status
and exits with that same code. Completion persistence is best-effort; on normal
producer cleanup, it is attempted before the live registry entry is removed.
`--timeout` (default: wait forever) exits `124` if the run hasn't finished in
time; if neither a completion record nor a live entry exists, `wait` exits `1`.
`logs` prints a finished run's retained stdout/stderr from that same completion
record. Both work with
`agent-shell submit`/`run`, `agent run`, `agent-react`, `agent-shell stage`,
`agent-sched` scheduled runs, and `agent-flow`'s executor. Records become
eligible for opportunistic pruning 7 days after completion. `kill` itself never
writes a completion record. For child-owning producers (`agent-shell
submit`/`run`, `agent run`, `agent-react`, `agent-shell stage`, and
`agent-sched`), it signals the child's process group, so the producer survives,
observes the signaled exit, and attempts to record the completion. Because
`kill` can remove the live entry before the producer writes that record, an
immediate `wait`/`logs` can briefly report that no completion record exists;
once the producer has written the record, both commands work normally. An
`agent-flow` executor instead registers its own process, so a successful `kill`
can terminate that producer before it writes a completion record.

## Fallback Agents

Use `--agents` to provide an ordered fallback list:

```sh
agent-shell submit --agents claude,codex,gemini,antigravity "summarize this repo"
```

`agent-shell` tries the agents in order and skips candidates that cannot run
before execution starts, such as missing executables or adapter option conflicts.
Once an agent starts and returns a task-level failure, `agent-shell` returns that
failure instead of retrying the task on another agent.

This means `--agents gemini,antigravity` skips to Antigravity when `gemini` is
missing or rejected before execution, but it does not replay the task if Gemini
starts and exits with an auth or runtime error. Consumer Gemini CLI users who
need to migrate should switch the selected backend explicitly instead of
expecting an automatic post-execution fallback.

`--exec` is single-agent only and cannot be combined with `--agents`. JSON output
includes an `attempts` array showing skipped candidates and, when one starts,
the final agent that ran. `--stream` is also single-agent only and is rejected
with `--agents`.

## Security model

**agent-shell is a process spawner, not a sandbox.** It resolves a vendor CLI you
already installed, builds an argv, and execs it as you, with your environment and
your filesystem. Everything below follows from that one sentence. If you need the
agent contained, contain it outside agent-shell — a disposable VM, a container,
a user with fewer rights. agent-shell will not do it for you and does not pretend
to.

### The default posture

**The only gate is the backend's own.** Each vendor CLI decides when to ask you
before running a command; agent-shell adds nothing to that and takes nothing away
— until `--dangerously-skip-permissions`, which removes it (see below).

**agent-shell makes no network connection.** The tree imports no `net/http` and
no `net`: its only outward action is `exec`. Every byte that leaves your machine
is sent by the vendor CLI under its own credentials and its own policy, not by
agent-shell. This is verifiable, not aspirational — `grep -r '"net/http"'` over
`services/agentshell/`, `internal/shellcli/` and `internal/agentregistry/`
returns nothing.

**agent-shell holds and reads no credentials.** The shipped code opens no files
at all: `os.ReadFile` and `os.Open` appear nowhere in `services/agentshell/` or
`internal/shellcli/` outside tests (where they read the test's own temp
files). Credential
files (`~/.claude.json`, `~/.codex/auth.json`, `~/.gemini/oauth_creds.json`) are
`os.Stat`'d for existence by `doctor` and never opened. Credential environment
variables (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GEMINI_API_KEY`,
`GOOGLE_API_KEY`, `GOOGLE_APPLICATION_CREDENTIALS`, `CURSOR_API_KEY`) are read
only to test whether they are non-empty, for the doctor's "unverified ready"
heuristic; the values are never printed, transmitted, or written. Subscription
quota — the one feature that needed a credential — lives in `agent-model limits`.

### What the privileged flags actually grant

**`--dangerously-skip-permissions` removes the agent's approval gate, and nothing
replaces it.** It maps to the backend's own bypass flag (`--dangerously-skip-
permissions`, `--dangerously-bypass-approvals-and-sandbox`, `--approval-mode
yolo`, `--force`). The agent then runs local commands without stopping to ask —
on the host, as you. Use it only where you are willing to have an unattended
agent modify everything you can modify. It is opt-in per invocation with no
backing environment variable, and `--agents` never implies it.

**`--exec` and the path environment variables choose which binary runs.**
`--exec`, `AGENT_SHELL_<AGENT>_PATH`, and `HEROS_BIN_DIR` each select the
executable agent-shell spawns, in that precedence. Anyone who can set those
variables, or write to a directory they point at, chooses what agent-shell
executes. They are convenience and deploy-pinning mechanisms, not trust
mechanisms.

**`--cwd` is not a jail.** It sets the child's working directory and, where the
backend supports it, the corresponding flag (`--cd`, `--add-dir`,
`--workspace`). It constrains nothing: the agent may read and write anywhere
your account can. There is no isolation option in agent-shell.

### What is deliberately not a boundary

**The environment is passed through whole.** `Submit` spawns with
`cmd.Env = mergeEnv(os.Environ(), req.Env)`, so the vendor CLI inherits every
variable this process has, including every API key. This is necessary — the CLI
needs its own credentials — but it means "agent-shell reads no tokens" is not the
same as "tokens do not flow through agent-shell." Anything in the environment
reaches the agent.

**The prompt is not validated, and in automation it is often not yours.** It is
passed to the backend verbatim. A pipeline that builds prompts from issue text,
web content, or model output is handing an untrusted string to a program you have
possibly also granted `--dangerously-skip-permissions`.

**Agent output is not validated either.** `Result.Stdout` is whatever the model
produced. Scripts that branch on it are trusting a language model's output as
control flow.

### Secrets: at rest

**agent-shell writes prompts and agent output to disk.** The process registry
records each run's full argv — and for `claude -p <prompt>`, the prompt *is* the
argv. Completion records additionally retain a bounded stdout/stderr tail so
`agent-shell logs <id>` can replay it. So a secret in a prompt is persisted even
though agent-shell never touched a credential: the exposure here is *content*,
not tokens.

Records live under `$XDG_RUNTIME_DIR/agent-in-the-shell/` or the user cache
directory, are written 0600 into 0700 directories via tmp+rename, and become
eligible for opportunistic pruning 7 days after completion. Two caveats worth
knowing: `os.MkdirAll` does not tighten a directory that already exists, so a
root created by an older build can stay group/world-readable, and no
permission guard currently covers the registry — the
assertion test that would catch it only wires up agent-model's sinks.

### What process control does and does not bound

**Timeouts kill the process group, not just the child.** Every spawn gets its own
process group and a `--timeout` (default 30m); on expiry the whole tree is
killed, so a subagent that forked children does not orphan them. `Result.Status`
distinguishes `timeout` from `signaled` from a clean nonzero `ok`.

**`kill` is cooperative with the producer.** It signals the child's process group
so the producer survives to record the completion. It is not a containment
mechanism — anything the agent already did to your filesystem or to the network
has already happened.

### Dependency surface

agent-shell links six first-party packages and one external dependency
(`github.com/google/uuid`). It links **no** `agent-model` package: `go list -deps
./cmd/agent-shell | grep services/agentmodel` is empty, and every command works
with agent-model absent. Keep future imports independent of the gateway
server to preserve this separation.

## Skipping Permission Prompts (DANGEROUS)

Some agent CLIs require interactive approval before they run shell commands.
That blocks scheduled or unattended workflows. `--dangerously-skip-permissions`
passes the backend's permission/approval bypass flag through so the agent runs
local commands without stopping for an approval prompt:

```sh
agent-shell submit --agent claude --dangerously-skip-permissions "run ./script.sh"
```

It maps to the backend-specific bypass flag:

| Agent | Mapped flag |
|---|---|
| `claude` | `--dangerously-skip-permissions` (equivalent to `--permission-mode bypassPermissions`) |
| `codex` | `--dangerously-bypass-approvals-and-sandbox` |
| `gemini` | `--approval-mode yolo` |
| `antigravity` | `--dangerously-skip-permissions` |
| `cursor` | `--force` (alias `--yolo`; force-allows command/file actions in print mode) |

**Read before using this:**

- **agent-shell provides no sandbox** — see [Security model](#security-model)
  for what that does and does not mean. Use this flag only in an environment you
  are willing to have an unattended agent modify (a disposable VM or container
  you control).
- For direct `agent-shell submit`/`run`, it is **opt-in per invocation.** There
  is no backing environment variable, and `--agents` never implies it; the CLI
  flag must be passed explicitly. In-process callers can set
  `Request.DangerouslySkipPermissions`.
- When the direct CLI flag is set (outside `--dry-run`), agent-shell prints a
  one-line warning to stderr; stdout and `--json`/`--dry-run` output are
  unaffected.

## Installed Agent Scan

`agent-shell agents` scans the local machine and reports whether each supported
agent CLI is installed. It checks a per-agent path override first, then an
executable under `HEROS_BIN_DIR`, then the default executable on `PATH`:

```text
AGENT_SHELL_CLAUDE_PATH
AGENT_SHELL_CODEX_PATH
AGENT_SHELL_GEMINI_PATH
AGENT_SHELL_ANTIGRAVITY_PATH
AGENT_SHELL_CURSOR_PATH
```

Version detection is best-effort. A broken or slow `--version` probe does not
hide an installed executable; the scan reports the executable and includes the
probe error in JSON output.

## Install & Doctor

`agent-shell install` (and its read-only alias `agent-shell doctor`) takes a
fresh machine toward a usable backend. Bootstrapping a coding-agent CLI is a
**two-phase** problem: phase 1 puts the binary on `PATH`; phase 2
authenticates it. Phase 2 defaults to an interactive browser OAuth flow that no
installer can complete headlessly, so the honest contract is **install the
binary, then guide the auth** — and `--check` is a doctor that reports the gap.

```sh
agent-shell doctor                 # read-only health of every backend (= install --check --all)
agent-shell install --check        # same, explicit
agent-shell install --check claude # one backend
agent-shell doctor claude          # same; an explicit agent beats the injected --all
agent-shell install gemini --dry-run   # show the install command, run nothing
agent-shell install claude         # guide: print the install command (no mutation without --yes)
agent-shell install claude --yes   # actually run the recommended install command
agent-shell install --all          # act on every backend
agent-shell doctor --json          # machine-readable report for CI
```

### Six-state model

Each backend resolves to one status, not a simple installed/missing boolean:

| Status | Meaning | Exit category |
|---|---|---|
| `ready` | Binary present and authenticated (or a credential heuristic says so — detail notes `unverified`). | `0` |
| `setup-needed` | Binary present but not authenticated/configured — run the printed auth command. | `78` (EX_CONFIG) |
| `missing` | A supported backend's resolved executable (override, deploy path, or `PATH` name) is absent. | `69` (EX_UNAVAILABLE) |
| `outdated` | Installed below a known floor. Reserved (not asserted by v1); a warning unless `--strict`. | `0` (or `70` with `--strict`) |
| `unsupported` | No installer by design — a backend that builds from source rather than shipping one. Never fails `--all`. No backend sets this today; the branch stays for the next one that does. | `0` |
| `unknown-probe-failure` | Reserved for an indeterminate probe; direct service checks of unknown backend names also use it. | `70` (EX_SOFTWARE) |

### Auth detection per backend (side-effect-free, no task submission)

- **claude** — `claude auth status` (exit `0` when authenticated); falls back to
  `ANTHROPIC_API_KEY` / `~/.claude.json` when the subcommand can't run.
- **codex** — `codex login status` (exit `0` when logged in); its status result
  warns when `OPENAI_API_KEY` and `~/.codex/auth.json` both exist (the key
  shadows stored OAuth, openai/codex#15151). If the status command cannot run,
  detection falls back to those same environment/file credential heuristics.
- **gemini** — no non-interactive status command, so detection reads
  `GEMINI_API_KEY` / `GOOGLE_API_KEY` / `GOOGLE_APPLICATION_CREDENTIALS` and
  `~/.gemini/oauth_creds.json`, biasing honestly to `setup-needed` and labeling
  any positive as `unverified`.
- **antigravity** — `agy auth status` (exit `0` when authenticated); otherwise
  `setup-needed` with guidance to run `agy auth login`.
- **cursor** — likewise no non-interactive auth gate; `CURSOR_API_KEY` →
  `ready (unverified)`, otherwise `setup-needed` (run `cursor-agent login`).
Deep backend reachability — whether the vendor's service answers — is out of
scope for this check, which is offline and read-only by design.

### Exit codes

`--all`/`doctor` aggregate by **rank-max severity**, not numeric-max over exit
codes (so `setup-needed` cannot mask a `missing`), then exit with the winning
status's code. The sysexits namespace (`64`/`69`/`70`/`78`) is agent-shell's own
and is **orthogonal** to the child-process exit-code passthrough used by
`submit`/`run` (defined in [Result contract](#result-contract)). Bad
`install`/`doctor` invocations exit `64` (EX_USAGE).

### Flags

| Flag | Effect |
|---|---|
| `--check` | Read-only doctor; never mutates. Cannot be combined with `--yes`. |
| `--dry-run` | Print the install command(s) without executing. |
| `--yes` (`-y`) | Permit actually running install commands for `missing` backends. Without it, install only guides. |
| `--all` | Act on every supported backend. |
| `--strict` | Promote `outdated` to a failing exit code. |
| `--json` | Emit the structured report (`backends[]` + `aggregate`). |

`install`/`doctor` are non-destructive by default: the interactive auth half is
always guidance, and the binary half mutates only under `--yes`. `--check` and
`--dry-run` are guaranteed read-only.
