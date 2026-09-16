---
name: agent-model
description: Use when you need one OpenAI-compatible endpoint in front of several LLM providers — weighted routing, fallback on retryable failures, per-request cost accounting, and subscription-quota reporting for Claude/Codex accounts.
---

# agent-model

An OpenAI-compatible HTTP gateway over multiple providers. Point any
OpenAI-shaped client at it and requests are routed to a deployment, retried
against a fallback when the failure is retryable, and recorded with their cost.

Request path:

```
api.Server (chi + bearer auth) → router.Router → provider.Provider → upstream
                                 weighted random pick, cooldown on failure
                                 cost.Calculator + store.Store (audit)
```

## Install

| Channel | |
|---|---|
| Release archive | `agent-model_<os>_<arch>.tar.gz`, linux/darwin × amd64/arm64 |
| Container | `docker run --rm ghcr.io/agent-in-the-shell/agent-model:latest --help` |
| Homebrew | `brew install agent-in-the-shell/tap/agent-model` |
| Go | `go install github.com/agent-in-the-shell/agent-in-the-shell/cmd/agent-model@latest` |

## Environment

### Config inputs

| Variable | Effect |
|---|---|
| `AGENT_MODEL_TOKEN` | Bearer token for `/v1/*`. **Required for `serve`.** |
| `AGENT_MODEL_CONFIG` | Config path when `--config` is omitted. |
| `AGENT_MODEL_DB` | SQLite path — used **only when the config omits `db`**; a config-set `db` is read verbatim. |
| `AGENTMODEL_PROFILE` | Active Anthropic OAuth profile override. |
| `ANTHROPIC_OAUTH_TOKEN` | Claude Pro/Max OAuth token (`sk-ant-oat*`). |
| `ANTHROPIC_OAUTH_TOKEN_DIR` | Directory for Anthropic `auth.json`. Default `~/.config/agentmodel/anthropic`. |
| `CHATGPT_TOKEN_DIR` | Directory for ChatGPT `auth.json`. Default `~/.config/agentmodel/chatgpt`. |

**Provider API keys are config-directed, not hardcoded.** A deployment names the
variable holding its key:

```yaml
api_key_env: "OPENAI_API_KEY"
```

so the conventional `OPENAI_API_KEY` / `ANTHROPIC_API_KEY` / `GEMINI_API_KEY` /
`DEEPSEEK_API_KEY` are whatever your config points at. Grepping the source for
those names will not find them — look at your config's `api_key_env` values.
Virtual keys work the same way through `token_env`.

### For callers, not the server

`services/agentmodel/gateway` is the Go client other services import to talk to
a gateway. It reads two variables of its own, which configure the *caller*:

| Variable | Effect |
|---|---|
| `AGENT_MODEL_URL` | Base URL of the gateway to call |
| `AGENT_MODEL_MODEL` | Default model for requests that do not name one |

### Protocol

| Variable | Effect |
|---|---|
| `XDG_DATA_HOME` | Relocates the data tree, so the database becomes `$XDG_DATA_HOME/heros/agentmodel/agentmodel.db`. Consumed through `internal/herospath`. |
| `XDG_CONFIG_HOME` | Relocates the config tree, so the config becomes `$XDG_CONFIG_HOME/heros/agentmodel/config.yaml`. |

Both have the same sharp edge: an existing file at a **legacy** path is used in
place and therefore defeats the XDG variable entirely. Relocating only takes
effect for a path that has not already been established.

## Persistence

| File | Default path | Override |
|---|---|---|
| Audit database | `${XDG_DATA_HOME:-~/.local/share}/heros/agentmodel/agentmodel.db` | `AGENT_MODEL_DB`, or `db:` in the config |
| Config | `${XDG_CONFIG_HOME:-~/.config}/heros/agentmodel/config.yaml` | `AGENT_MODEL_CONFIG`, `--config` |
| Anthropic tokens | `~/.config/agentmodel/anthropic/auth.json` | `ANTHROPIC_OAUTH_TOKEN_DIR` |
| ChatGPT tokens | `~/.config/agentmodel/chatgpt/auth.json` | `CHATGPT_TOKEN_DIR` |

An existing legacy `~/.config/agentmodel/agentmodel.db` (the database beside the
config) is used in place, so an older install never orphans its audit history.

An existing legacy `~/.config/agentmodel/config.yaml` is likewise used in place.
Both fallbacks exist so an install that predates the `heros/` layout keeps
reading the files it already has; a fresh install converges on the canonical
paths.

Credential files are written **0600 inside 0700 directories, via temp-file and
rename**, so a reader never sees a partial token. The database is likewise
tightened at every open.

## Usage

```
agent-model serve   [--config <path>]                 start the HTTP server
agent-model migrate --config <path>                   apply DB migrations and exit
agent-model purge   --config <path> [--older-than d]  drop audit rows past retention
agent-model status  [--config <path>] [--json]        offline health snapshot
agent-model prompt  [--model <name>] ["<prompt>"]     send one real request
agent-model usage   [--since 7d] [--by model] [--json] read request_logs
agent-model limits  [claude|codex] [--profile <name> | --profiles all] [--json]
agent-model chatgpt-login   [--token-dir <path>] [--profile <name>] [--activate]
agent-model anthropic-login [--token-dir <path>] [--profile <name>] [--activate]
agent-model profile <set|list|show|remove|migrate>
```

### Worked example: bring one up and prove it works

```sh
cp examples/agent-model.yaml config.yaml
export AGENT_MODEL_TOKEN=$(openssl rand -hex 16)
export OPENAI_API_KEY=sk-...

agent-model migrate --config config.yaml
agent-model serve   --config config.yaml &

# Any OpenAI-shaped client works:
curl -s localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $AGENT_MODEL_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"model":"default","messages":[{"role":"user","content":"hi"}]}'
```

### Which command to reach for

The three diagnostics answer different questions, and picking the wrong one
wastes a debugging cycle:

- **`status`** — offline. Gateway liveness, token-store freshness, model routing.
  Exits 1 if anything is unusable. Costs nothing; start here.
- **`prompt`** — sends one *real* request through the gateway. This is the only
  one that proves a model is actually reachable, and the only one that spends
  money. Exits 1 if the model is not online.
- **`limits`** — subscription quota for the accounts this process holds
  credentials for. Non-consuming, and needs no running server.

`usage` reads the audit table for requests, tokens, cost and error rate.

### Behaviours worth relying on

- **Upstream errors are classified by HTTP status, not response text.** Retry
  behaviour therefore does not depend on digits appearing in a message body.
- **Credential pools are sticky, not round-robin.** Traffic stays on the current
  credential until it is rate-limited, and does not move back when an older one
  cools down. This is a prompt-cache decision — the cache is per-account, so
  every switch is a full prompt re-read.
- **Fail-closed defaults.** A virtual key with its env unset is disabled rather
  than silently permitted; a managed key that cannot be looked up returns 503,
  not 401; an unparseable budget window rejects the request.

## Known gaps

- **`limits` needs the gateway's own credentials**, which is why it lives here
  rather than in `agent-shell`: Anthropic refresh tokens are single-use, so two
  processes rotating them independently produce `invalid_grant`.
