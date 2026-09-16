---
name: agent-shell
description: Use when you need to run a coding-agent CLI (claude, codex, gemini, antigravity, cursor) from a script or another agent — one interface, ordered fallback between backends, a normalized result, and process control over the runs it starts.
---

# agent-shell

One CLI over the coding-agent CLIs you already have installed. It execs the
vendor binary — `claude`, `codex`, `gemini`, `agy`, `cursor-agent` — and
normalizes the result, so a caller does not need per-vendor argument handling.

It routes no inference of its own and holds no credentials: each vendor CLI uses
whatever auth it already has. `agent-shell` never talks to `agent-model`; every
command works with `agent-model` absent.

**It provides no sandbox.** A task runs with the full privileges of the calling
user.

## Install

| Channel | |
|---|---|
| Release archive | `agent-shell_<os>_<arch>.tar.gz`, linux/darwin × amd64/arm64 |
| Homebrew | `brew install agent-in-the-shell/tap/agent-shell` |
| Go | `go install github.com/agent-in-the-shell/agent-in-the-shell/cmd/agent-shell@latest` |

**No container image, deliberately.** The distroless image premise is "the one
binary is the image", and that does not hold here: `agent-shell`'s job is
exec'ing `claude`/`codex`/`gemini`/`agy`/`cursor-agent`, none of which exist in a
distroless base (no shell, no node, no npm). An image where `submit`, `run`,
`install` and `doctor` all fail by construction would be a second artifact to
keep patched in exchange for a `--help` printer.

## Environment

### Config inputs

`agent-shell` has **no config file**, but two variables set defaults for the
flags you would otherwise repeat, plus one path override per backend:

| Variable | Effect |
|---|---|
| `AGENT_SHELL_AGENT` | Default for `--agent` (falls back to `claude`) |
| `AGENT_SHELL_AGENTS` | Default for `--agents`, the ordered fallback list |
| `AGENT_SHELL_CLAUDE_PATH` | Absolute path to the `claude` executable |
| `AGENT_SHELL_CODEX_PATH` | …to `codex` |
| `AGENT_SHELL_GEMINI_PATH` | …to `gemini` |
| `AGENT_SHELL_ANTIGRAVITY_PATH` | …to `agy` |
| `AGENT_SHELL_CURSOR_PATH` | …to `cursor-agent` |
| `HEROS_BIN_DIR` | Deploy directory searched for the vendor binary. Consumed through `internal/herospath`. |

Resolution order for a backend's executable: **`AGENT_SHELL_<AGENT>_PATH` →
`$HEROS_BIN_DIR/<name>` → `PATH`**. The `--exec` flag overrides all three for a
single invocation.

Note Cursor's backend executable is `cursor-agent`, not `cursor` — the bare
`cursor` command launches the editor.

It also *detects* `OPENAI_API_KEY`, `ANTHROPIC_API_KEY` and `CURSOR_API_KEY`
during `install --check` / `doctor` to report each backend's auth state — and to
warn that `OPENAI_API_KEY` shadows stored Codex OAuth. It never reads their
values as configuration.

### Protocol

| Variable | Effect |
|---|---|
| `AGENT_RUN_ID` | Read to find the enclosing run, and **exported into the child's environment** as this run's id. A nested `agent-shell` or scheduled run therefore links under its parent, which is what `ps --tree` renders. |
| `XDG_RUNTIME_DIR` | Root of the run registry (see Persistence). |
| `AGENT_TASK_ID` | Default for `stage --task`: which task a pipeline stage belongs to, so staged runs attribute back to it. |

## Persistence

No config file and no database — but **not stateless**: `ps`, `kill`, `signal`,
`wait` and `logs` are backed by an on-disk run registry.

| Directory | Contents |
|---|---|
| `${XDG_RUNTIME_DIR}/agent-in-the-shell/agents/<id>/meta.json` | live run entries |
| `${XDG_RUNTIME_DIR}/agent-in-the-shell/completed/<id>.json` | finished runs: exit status and captured output |

When `XDG_RUNTIME_DIR` is unset — the macOS default — the root falls back to
`os.UserCacheDir()`, i.e. `~/Library/Caches/agent-in-the-shell/…`.

Files are written 0600 via temp-file-and-rename inside 0700 directories.

Three properties worth knowing:

- **This is cache/runtime state, not the `heros/` data tree**, on purpose: the
  registry is ephemeral. On Linux `XDG_RUNTIME_DIR` is typically `/run/user/<uid>`,
  mode 0700 and cleared on logout, which is exactly the intended lifetime.
- `os.TempDir` is deliberately *not* used — it is world-writable, and this is a
  directory whose pids are trusted.
- The cache fallback survives a reboot, so a crash-on-reboot orphan can outlive
  its process. Reconciliation catches it: the pid is dead or its start time
  differs, so the entry reads as stale rather than live.

## Usage

```
agent-shell submit [flags] <task>      # run a task; `run` is an alias
agent-shell agents [--json]            # list backends and their availability
agent-shell install [agent] [--all] [--check] [--dry-run] [--yes] [--strict]
agent-shell doctor [agent]             # alias for: install --check --all
agent-shell ps [--json] [--tree]       # runs on this host
agent-shell kill <id> [--grace d]      # reap a run's process group
agent-shell signal <id> <SIG>          # e.g. TERM, HUP, USR1, 15
agent-shell wait [--timeout d] <id>    # block; exit with the run's exit code
agent-shell logs <id>                  # a finished run's captured output
```

Key flags for `submit`: `--agent` (default `claude`), `--agents` for ordered
fallback, `--model`, `--cwd`, `--system-prompt`, `--exec`, `--timeout` (default
30m), `--json`, `--stream`, `--dry-run`.

### Worked example

```sh
# What is actually installed and authenticated?
agent-shell doctor --json

# See the exact command without running it — the cheapest way to check argument
# translation for a backend you have not used before.
agent-shell submit --agent codex --dry-run "fix the failing tests"

# Run it, with ordered fallback: first backend that is usable wins.
agent-shell submit --agents claude,codex,gemini --json "summarize this repo"

# Long task: start it, watch the tree, reap it if it misbehaves.
agent-shell submit --agent claude --stream "refactor the parser" &
agent-shell ps --tree
agent-shell kill --grace 10s <id>
```

Piping a prompt works too: `printf "write release notes" | agent-shell submit --agent claude -`

### Reading the result

`--json` returns a normalized result across backends: `exit_code`, `status`
(`ok` | `signaled` | `timeout` | `error`), `signal`, `stdout`, `stderr`,
`duration_ms`, and `attempts` when fallback was used. Branch on `status`, not on
stderr text — that is the field that means the same thing for every backend.

## Known gaps

- **No sandbox.** `--dangerously-skip-permissions` passes the backend's own
  bypass flag; the agent then runs with full host access. Isolated environments
  only.
- **Backend list and flags are hand-synced with the docs** — `agent-shell`
  arrived without the AST guard the other services have, so changing
  `supportedAgents` or `buildArgs` requires editing `docs/agentshell.md` by hand.
