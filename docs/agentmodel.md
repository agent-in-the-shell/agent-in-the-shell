# AgentModel

An OpenAI-compatible LLM gateway that routes requests across multiple providers,
including OpenAI, Anthropic, Gemini, ChatGPT subscription, Azure OpenAI, DeepSeek,
and Replicate. It supports weighted
multi-account pools, per-model fallbacks, request auditing, cost tracking,
bearer-token auth, and an optional [content log](#content-logging) of full
request/response bodies.

Source: `services/agentmodel/`

> agent-model is the LLM-gateway layer of a wider agent tool family (agent-pi
> coding agent, agent-shell launcher, agent-run def runner — released in later
> waves). It is infrastructure: the thing other tools call to talk to an LLM.

## Build

```
go build -o agent-model ./cmd/agent-model
```

## Commands

```
agent-model serve [--config <path>]    start the HTTP server
agent-model migrate --config <path>    apply DB migrations and exit
agent-model purge --config <path> [--older-than <dur>]
                                      delete audit rows older than the retention window and exit
agent-model chatgpt-login [--token-dir <path>] [--profile <name>] [--activate]
                                      run ChatGPT device-code OAuth
agent-model anthropic-login [--token-dir <path>] [--profile <name>] [--activate]
                                      run Anthropic Claude Code OAuth
agent-model profile set <name> [--force]
agent-model profile list [--json]
agent-model profile show [--json]
agent-model profile remove <name> [--force] [--missing-ok]
agent-model profile migrate [--dry-run]
                                      manage Anthropic OAuth profiles
agent-model filter [--url <url>] [--model <name>] "<system-prompt>"
                                      read JSONL from stdin, rewrite the text field, emit JSONL
agent-model usage [--config <path>] [--db <path>] [--since 7d] [--by model] [--org default] [--limit N] [--json]
                                      report request_logs usage: requests, tokens, cost, error_rate;
                                      resolves the DB from --db, --config, or the default config
                                      path (--db wins when both are set)
agent-model status [--config <path>] [--json]
                                      offline health snapshot: gateway liveness, token-store
                                      freshness, and model routing; exit 1 if the gateway is
                                      down or any credential referenced by a nonzero-weight
                                      deployment is unusable
agent-model schema
                                      print the filter JSONL wire contract and exit
```

This list is guarded by `TestCLICommandsDocumented`
(`cmd/agent-model/commands_doc_test.go`): it parses the command dispatch in
`main.go` and fails CI if a dispatched subcommand is missing from this block or
from the `--help` text. Update both when you add or remove a command.

## Configure

```yaml
listen: ":8080"
db: "agentmodel.db"
auth:
  bearer_token_env: "AGENT_MODEL_TOKEN"
router_cooldown: "5m"   # cooldown after a retryable failure; "0" disables; omit => 5m
model_list:
  - model_name: "gpt-4"
    deployments:
      - provider: "openai"
        model: "gpt-4o"
        auth_mode: "api_key"
        api_key_env: "OPENAI_API_KEY"
        weight: 100
  - model_name: "claude"
    deployments:
      - provider: "anthropic-oauth"
        model: "claude-3-5-sonnet-latest"
        auth_mode: "subscription"
        oauth_token_dir: "/home/alice/.config/agentmodel/anthropic/default"
        weight: 100
fallbacks:
  - model_name: "gpt-4"
    fallback_to: ["claude"]
budget:                       # optional org-wide spend cap
  max_budget: 100.0           # USD per window; omit for unlimited
  budget_duration: "720h"     # optional; empty = lifetime cap, never resets
keys:                         # optional virtual API keys
  - name: "ci"                # config identifier (shows up on /v1/limits)
    token_env: "AGENT_MODEL_KEY_CI"
    max_budget: 10.0          # optional per-key cap, USD per window
    budget_duration: "24h"
    models: ["gpt-4"]         # optional allowlist of model_names; omit = all
```

`auth_mode` is `api_key` (also the default when omitted) or `subscription`. For
multi-account pools, use `api_key_envs` instead of `api_key_env` — or, for
subscriptions (ChatGPT and Anthropic alike), `oauth_token_dirs` (a list of
per-account `auth.json` directories) instead of `oauth_token_dir`. Pooled
subscription credentials keep refreshing themselves, so unlike `api_key_envs`
they do not go stale. The pool wrapper rotates supported chat, embedding, image,
and Messages calls to the next credential when one hits a rate limit. It does
not expose video generation or the Replicate passthrough; those capabilities
require a single usable credential.

Selection is **sticky, not round-robin**: traffic stays on whichever credential
last served until that one rate-limits, then moves on and stays there. This is a
prompt-cache decision. Caches are per-account and never shared between
credentials — a different Anthropic subscription is a different organization,
and the ChatGPT `session-id` cache lives on the node serving one account — so
every move costs one full prompt re-read on the new account. Staying put keeps
that to one cache miss per rotation instead of one per request; round-robin
would miss on every request and burn quota *faster* than not pooling at all. A
credential whose cooldown expires does not pull traffic back from the one
currently serving (that would abandon a warm cache for a cold one); it is picked
up when the scan next wraps, so no credential's quota is stranded.

This applies *within* a pooled deployment only. Listing two deployments of the
same provider under one `model_name` (different accounts, hence different
caches) still selects between them by weight on every request, which misses the
cache whenever consecutive requests land differently — `weight` means what it
says, and honoring it requires re-sampling. Prefer one pooled deployment with
`oauth_token_dirs`/`api_key_envs` over several single-credential deployments of
the same provider when prompt caching matters.
Optional `rpm` and `tpm` limits cap individual deployments; omit them for
unlimited. Caps are **enforced pre-call** over fixed UTC-minute windows: a
deployment at its cap is skipped exactly like a cooled deployment (no upstream
call). Chat and Messages traffic can then use sibling deployments and configured
fallbacks; the other spending routes try eligible deployments but do not traverse
`fallbacks`. When every eligible candidate is capped, the request gets `429`
until the minute rolls. `rpm` counts dispatch attempts;
`tpm` blocks once *observed* usage (input + output tokens, recorded after
each response) reaches the cap — request size is not predicted pre-call.

`base_url` can point an `auth_mode: api_key` deployment at a compatible
upstream such as a local Ollama/vLLM server or a mirror; with an explicit
`base_url`, the credential fields may be omitted for a keyless upstream. Paths
from `oauth_token_dir`/`oauth_token_dirs` are used literally, without shell
tilde expansion, so use absolute paths in YAML rather than `~`. For the OpenAI wire
path (`provider: openai`, plus `azure`, `deepseek`, and every
OpenAI-compatible provider in the table below, which all compose it), chat
requests are sanitized before dispatch: gateway-only
`thinking`/`reasoning_effort` fields and per-message `cache_control` hints are
omitted, and tool-call IDs over OpenAI's 64-character limit are shortened. For
OpenAI reasoning-family models (`o1`/`o1-*`, `o3`/`o3-*`, `o4`/`o4-*`, and
`gpt-5*`), which reject the
legacy `max_tokens` on `chat/completions`, `max_tokens` is relocated to
`max_completion_tokens`; every other model keeps `max_tokens` so
OpenAI-compatible servers (vLLM, llama.cpp, Ollama) that understand only the
legacy field still work. The router's original request is preserved for
fallback to providers where those fields are meaningful.

For `auth_mode: subscription` on `anthropic`/`anthropic-oauth` deployments,
optional `cache_ttl` selects the TTL of the gateway-injected prompt-cache
breakpoint: omit it for the default `5m`, or set it to `5m` or `1h`. Other
providers reject this field.

### Providers

`provider:` selects the upstream client. These providers have a native client,
with their own wire shaping, auth, and capabilities:

| `provider` | Upstream | Notes |
|---|---|---|
| `openai` | OpenAI | Chat, embeddings, images |
| `anthropic` | Anthropic | `api_key`; `x-api-key` auth |
| `anthropic-oauth` | Anthropic | `subscription` via `oauth_token_dir`, or `oauth_token_dirs` for a multi-account pool |
| `chatgpt` | ChatGPT subscription | `subscription`; chat and images; also serves `/v1/messages` |
| `gemini` | Google Gemini | `x-goog-api-key` auth; chat, embeddings, images ("nano banana"), and video (Veo) — the only provider serving `/v1/videos/generations` |
| `azure` | Azure OpenAI | Needs `base_url` + `api_version`; optional `deployment_name` defaults to `model`; `api-key` auth |
| `deepseek` | DeepSeek | Chat only, no embeddings |
| `replicate` | Replicate | Prediction create/get/cancel passthrough; rewrites API callback URLs through the gateway |

Everything else that speaks OpenAI's `/chat/completions` wire format is served
by one generic client, so each vendor is still a first-class provider — its own
observability label and pricing key — without a bespoke file. These names work
with **no `base_url`**; just set the key env var:

| `provider` | Default endpoint |
|---|---|
| `groq` | `https://api.groq.com/openai/v1` |
| `mistral` | `https://api.mistral.ai/v1` |
| `together` | `https://api.together.xyz/v1` |
| `xai` | `https://api.x.ai/v1` |
| `openrouter` | `https://openrouter.ai/api/v1` |
| `fireworks` | `https://api.fireworks.ai/inference/v1` |
| `perplexity` | `https://api.perplexity.ai` |
| `deepinfra` | `https://api.deepinfra.com/v1/openai` |
| `nebius` | `https://api.studio.nebius.ai/v1` |
| `zhipu` | `https://open.bigmodel.cn/api/paas/v4` |
| `moonshot` | `https://api.moonshot.cn/v1` |
| `qwen` | `https://dashscope.aliyuncs.com/compatible-mode/v1` |
| `yi` | `https://api.lingyiwanwu.com/v1` |
| `baichuan` | `https://api.baichuan-ai.com/v1` |
| `stepfun` | `https://api.stepfun.com/v1` |
| `siliconflow` | `https://api.siliconflow.cn/v1` |

All of them authenticate with `Authorization: Bearer <key>` and take the key
from `api_key_env` (or `api_key_envs` for a rotating pool). Any *unlisted*
OpenAI-compatible endpoint also works — give it any `provider` name you like and
point it at the upstream with an explicit `base_url`. Without a `base_url`, an
unrecognized name is rejected at startup — provided the deployment's credential
env var is actually set; a deployment whose `api_key_env` is unset is
warn-and-skipped (like any missing-credential deployment) before the unknown
name is reached.

**Grok (xAI)** uses the built-in `provider: xai` — the table is keyed by
*vendor*, not by model family. `provider: grok` works only as a custom name
paired with an explicit `base_url`:

```yaml
model_list:
  - model_name: "grok"
    deployments:
      - provider: "xai"          # built-in xAI endpoint; "grok" would need base_url
        model: "grok-4"          # or grok-4-latest, grok-3-mini, grok-code-fast-1, …
        api_key_env: "XAI_API_KEY"
        weight: 100
```

Cost lookup prefers the catalog key `<provider>/<model>` (here `xai/grok-4`)
and then falls back to the bare resolved-model key. If neither key exists, the
request prices at zero with `cost_source="unpriced"` and a warning. Where the
provider and catalog prefixes differ, the calculator aliases the config name to the
catalog prefix — `qwen` → `dashscope`, `together` → `together_ai`, `fireworks` →
`fireworks_ai`. Any new provider whose config name differs from its catalog
prefix needs the same treatment.

### Budgets and virtual keys

`budget` caps gateway-wide spend (everything is attributed to the single
`default` org today); `keys` declares named bearer tokens with optional
per-key caps and model allowlists.

Reset semantics: with a `budget_duration`, spend is metered over fixed
windows computed with `time.Truncate(budget_duration)` — boundaries fall at
multiples of the duration since Go's zero time (0001-01-01T00:00:00Z), which
aligns to UTC midnight exactly when the duration is a multiple of `24h`
(`"24h"` resets at midnight UTC; `"720h"` boundaries are *not* month-aligned).
Calendar tokens (`"30d"`, `"1mo"`) do not parse and are reserved for a future
extension. Without a `budget_duration`, `max_budget` is a lifetime cap over
the whole ledger — which is incompatible with audit-log retention (purged
rows would undercount spend), so YAML config validation rejects a lifetime
`budget`/`keys[]` cap when `retention.period` is set, and rejects any configured
`budget_duration` longer than `retention.period`.

Enforcement (live on every spending endpoint — `/v1/chat/completions`,
`/v1/messages`, `/v1/embeddings`, `/v1/images/generations`,
`/v1/videos/generations`, and the two Replicate prediction-create routes;
read endpoints like `/v1/models`, `/v1/limits`, `/v1/usage`, the `GET` job
polls, and Replicate cancellation are not budget-gated):

- **Auth-by-key**: a request authenticating with a virtual key's token gets
  that key's identity; the master token is exempt from the per-key model
  allowlist and per-key cap, but the org-wide `budget` still applies to it. A
  key whose `token_env` is unset or empty at startup is disabled (fail-closed)
  and logged — it can never match an incoming token.
- **Model allowlist**: a restricted key calling a model outside its `models`
  list gets `403` / `model_access_denied`. The allowlist is enforced on the
  requested model and on live fallback walks: when a restricted key's request
  falls back (`fallbacks`), targets outside the allowlist are skipped. The
  response cache is keyed by the request rather than the caller or the
  fallback that originally produced a response, so a key allowed to call the
  requested model can receive a shared cached response that was previously
  produced through a fallback outside its own allowlist. Disable the cache
  when keys for the same logical model must have different fallback
  permissions. Replicate prediction creates check the fixed logical name
  `replicate` as one passthrough permission, not the vendor model in the
  request body or path.
- **Budgets**: before any upstream call, the gateway sums ledger spend over
  the current window (org-wide, and per-key when the key has a cap) and
  rejects with `400` / `budget_exceeded` once spend reaches the cap —
  LiteLLM semantics: terminal for the caller, not a retry-soon `429`. If a
  cap is configured but the ledger is unavailable the request fails closed
  with `503` / `budget_check_unavailable` (the cause is logged server-side
  only). Handlers send rejections through the audit pipeline (status `error`,
  error type `budget_exceeded`/`permission_error`/`service_unavailable`).
  When the ledger itself is unavailable, persistence is best-effort and may
  fail; enabled telemetry still receives the row independently.
- **Operator-only endpoints**: the ChatGPT OAuth credential and virtual-key
  management endpoints (`/v1/oauth/chatgpt/*` and `/v1/keys*`) require the master token — a virtual key gets
  `403` / `master_token_required` (completing a device-code flow would
  rebind the gateway's upstream subscription credentials).

Caveats worth knowing:

- Error `code`s (`model_access_denied`, `budget_check_unavailable`) appear in
  OpenAI-shaped envelopes (`/v1/chat/completions`, `/v1/embeddings`) and,
  when the gateway or messagesbridge has a typed error, inside `/v1/messages`
  Anthropic error envelopes; raw Anthropic upstream error bodies are still
  proxied verbatim.
- The cap bounds *sustained* spend, not instantaneous spend: concurrent
  in-flight requests can each pass the pre-call check before any of their
  cost rows land, and a stream's cost enters the ledger only when the stream
  ends. A `/v1/messages` stream that ends with an Anthropic `error` event is
  audited as `status="error"` with the event's error type while preserving
  usage parsed before the failure; client-aborted streams are still logged
  with whatever usage arrived.
- Runtime-managed key caps created through `POST /v1/keys` do not run the YAML
  retention compatibility check. When `retention.period` is enabled, give
  those keys a `budget_duration` no longer than the retention period and do
  not use a lifetime cap, or purged spend will undercount the cap.
- Caps sum only successful (`status="ok"`) registry-priced rows. A model
  missing from the price catalog logs `$0` (`cost_source="unpriced"`) and never
  advances a cap; failed rows also do not advance caps even if they retain
  usage and computed cost. Watch
  `agentmodel_cost_source_total{source="unpriced"}`.
- The budget-exceeded error shown to a tenant key redacts the shared org's
  spend figures. However, `/v1/usage` and `/v1/limits` are not master-only:
  any authenticated caller can see the single org's usage rollups and the
  configured `keys[]` names and caps.

Validation rules: `max_budget` must be a finite number `> 0` when set,
`budget_duration` requires `max_budget` and, when set, must be a positive Go
duration; every key must set `name` and `token_env`; key names and `token_env`s
must be unique, a key's `token_env` must not collide with
`auth.bearer_token_env` or any deployment's `api_key_env`/`api_key_envs`,
`models` must be non-empty when present (omit it to allow all models), and
`models` entries must be unique and reference configured `model_name`s.

Caps are also reported on `GET /v1/limits` (unit `usd`). Live
`used`/`remaining` reporting on that endpoint is #500; until it lands, the
budget windows show the configured cap with `used`/`remaining` omitted, and
the live signal for an exhausted budget is the `400 budget_exceeded`
rejection itself.

### Error format

Gateway-raised errors on `/v1/*` routes use the OpenAI envelope, except errors
raised inside the `/v1/messages` or `/v1/account/anthropic/usage` handlers.
Replicate upstream
responses, including non-2xx responses, remain in Replicate's native shape:

```json
{ "error": { "type": "invalid_request_error", "code": "empty_array", "param": "messages", "message": "messages must be non-empty" } }
```

`type` and `message` are always present. `code` and `param` are present only
when meaningful and are **omitted entirely** when absent — never serialized as
`""` (#705). All three official OpenAI SDKs treat an absent `code`/`param`
identically to a `null` one, so clients should branch on HTTP status and the
`type`/`code` *when present*. Common codes:

| code | example trigger | HTTP |
|---|---|---|
| `invalid_api_key` | missing / wrong bearer token | 401 |
| `model_not_found` | no deployment was built for `model_name` and it has no configured fallback | 404 |
| `missing_required_parameter` | required field absent (carries `param`) | 400 |
| `empty_array` | a required array is empty (carries `param`) | 400 |
| `invalid_json` | request body is not valid JSON | 400 |
| `context_length_exceeded` | request exceeds the model context window | 400 |
| `content_policy_violation` | provider content filter rejected the request | 400 |
| `rate_limit_exceeded` | all deployments/credentials rate-limited | 429 |
| `model_access_denied` | restricted key called a model outside its allowlist | 403 |
| `budget_exceeded` | spend reached the org/key cap | 400 |
| `budget_check_unavailable` | cap configured but ledger unreachable (fail-closed) | 503 |
| `master_token_required` | virtual key called an OAuth or key-management endpoint | 403 |

Errors raised inside the `/v1/messages` handler use Anthropic's envelope instead:

```json
{ "type": "error", "error": { "type": "context_window_exceeded", "code": "context_length_exceeded", "message": "input exceeds the context window" } }
```

For gateway-raised errors and messagesbridge errors, the inner object is the
same typed error, so `code`/`param` appear when known and are omitted when
absent; raw Anthropic upstream error bodies are proxied unchanged.
Authentication and master-token checks run before the handler, so their errors
use the OpenAI envelope even when the requested route is `/v1/messages`.

OpenAI-shaped mid-stream failures (after the `200` SSE header is sent) are
delivered as a `data: {"error": …}` frame carrying the same typed error object
(an accurate `type` and, when classifiable, `code`), followed by
`data: [DONE]`. On `/v1/messages`, Anthropic SSE is forwarded line-by-line with
line endings normalized to `\n`; a terminal `event: error`/`type:"error"` frame
is audited as `status="error"`
with the frame's `error.type`, and bridged-provider frames carry the same typed
error object (`code` when known).

On chat and Messages routes, `router_cooldown` controls how long a deployment is
parked after a retryable failure (e.g. a `429`) before the router retries it;
during the cooldown, requests for that model_name skip straight to the next
deployment / fallback. It is a Go duration string (`"5m"`, `"30s"`) and applies
server-wide. Omit it for the built-in `5m` default, or set `"0"` to disable
deployment cooling so a failed deployment is eligible again on the very next
request. Credential pools have a separate `5m` per-credential default that this
setting does not change. This is the knob for a "subscription first, API-key on
exhaustion" setup: a shorter cooldown retries the subscription deployment sooner
after it rate-limits.

Both defaults are only used when the upstream says nothing. When a `429` names
its own reset — a `Retry-After` header, Anthropic's
`anthropic-ratelimit-unified-reset`, or the `resets_in_seconds` the ChatGPT
backend puts in a usage-limit body — that value sizes the cooldown instead,
clamped to 24h. This is what makes pooled subscriptions worth having: a plan
quota that resets in hours is parked for hours rather than re-probed (and
re-rejected) every 5 minutes. Hints are read from `429` responses only; a
`Retry-After` on a `5xx` is backoff advice for that request, not evidence a
credential is out of quota.

Which providers report a hint: `anthropic`/`anthropic-oauth`, `chatgpt`, and the
whole OpenAI wire path (`openai`, `azure`, `deepseek`, and every
OpenAI-compatible provider in the table above). `gemini` and `replicate` do not
yet, and fall back to the defaults on every `429`.

`router_cooldown: "0"` disables cooling at the deployment layer, and an upstream
hint does not override it. It does not reach the pool's per-credential cooldown,
which is not configurable — a pooled credential is parked for the hint (or 5m)
regardless, up to the 24h clamp.

A `429` the gateway returns to its own clients carries `Retry-After` in whole
seconds whenever it knows the answer: the soonest moment any deployment for that
`model_name` becomes eligible again, whether that came from an upstream hint or
from the time left on a cooldown it already applied.

At startup, the server also runs a best-effort background drift check against
live model lists for providers that support listing. Set `revalidate_interval`
to a positive Go duration such as `"1h"` to repeat the check and log only drift
transitions; omit it for the default one-shot startup check. The interval is
also the live-model-list cache TTL, and failed refreshes reuse a last-good list.

## Environment variables

| Var | Purpose |
|---|---|
| `AGENT_MODEL_URL` | base URL read by the shared Go gateway client and `filter` (default: `http://localhost:8090`) |
| `AGENT_MODEL_MODEL` | default model read by the shared Go gateway client (default: `claude-haiku-4-5-20251001`) |
| `AGENT_MODEL_TOKEN` | bearer token read by the shared Go gateway client and `filter`; also the default master-token env for `serve`, which reads the env named by `auth.bearer_token_env` |
| `AGENT_MODEL_CONFIG` | default `serve`/`status` config path when `--config` is omitted, and the `usage` config path when neither `--db` nor `--config` is supplied (default: `~/.config/agentmodel/config.yaml`) |
| `AGENT_MODEL_DB` | SQLite default when config `db` is omitted; an explicit config `db` takes precedence |
| `XDG_DATA_HOME` | base for the canonical SQLite default when config `db` and `AGENT_MODEL_DB` are omitted; a fresh install uses `$XDG_DATA_HOME/heros/agentmodel/agentmodel.db` (or `~/.local/share/heros/agentmodel/agentmodel.db` when unset), while an existing legacy DB stays in place |
| `keys[].token_env` vars | one bearer token per configured virtual key (unset = key disabled) |
| `OPENAI_API_KEY` | OpenAI API key |
| `ANTHROPIC_API_KEY` | Anthropic API key |
| `ANTHROPIC_OAUTH_TOKEN` | Claude Pro/Max OAuth token (`sk-ant-oat*`) |
| `ANTHROPIC_OAUTH_TOKEN_DIR` | dir for Anthropic profiles (default: `~/.config/agentmodel/anthropic`) |
| `AGENTMODEL_PROFILE` | active-profile override for OAuth login and profile resolution; applies to both the Anthropic and ChatGPT profile stores |
| `GEMINI_API_KEY` | Google AI Studio API key |
| `DEEPSEEK_API_KEY` | DeepSeek API key |
| `REPLICATE_API_TOKEN` | Replicate API token (for the `/v1/predictions/*` passthrough) |
| `CHATGPT_TOKEN_DIR` | root dir for ChatGPT profiles (default: `~/.config/agentmodel/chatgpt`; legacy `<root>/auth.json` is supported) |

The provider-key rows (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`,
`ANTHROPIC_OAUTH_TOKEN`, `GEMINI_API_KEY`, `DEEPSEEK_API_KEY`,
`REPLICATE_API_TOKEN`) are the **conventional** names; the gateway reads
whatever env var each deployment's `api_key_env`/`api_key_envs` names, or reads
`auth.json` from its literal `oauth_token_dir`/`oauth_token_dirs` paths, so
nothing is hard-wired to the conventional env-var spellings.
Directory values read from `ANTHROPIC_OAUTH_TOKEN_DIR` and `CHATGPT_TOKEN_DIR`
are also used literally; set an absolute path rather than a value beginning
with `~`.

## Anthropic OAuth Profiles

Anthropic OAuth tokens can be stored in named profiles:

```sh
agent-model anthropic-login --profile work
agent-model anthropic-login --profile personal
agent-model profile set work
agent-model profile list
agent-shell limits claude --profile work
agent-shell limits claude --profiles all
```

With the default profile root, profiles live under:

```text
~/.config/agentmodel/anthropic/<profile>/auth.json
```

`--token-dir` remains a direct override for legacy and service deployments. If
you pass `--token-dir`, profile resolution is skipped and the command reads or
writes `auth.json` in that exact directory.

Profile precedence for CLI commands is:

```text
--profile > AGENTMODEL_PROFILE > <anthropic-profile-root>/profile > default
```

`<anthropic-profile-root>` is `ANTHROPIC_OAUTH_TOKEN_DIR` when that variable is
set, otherwise `~/.config/agentmodel/anthropic`.

`agent-model anthropic-login --profile work` writes the `work` profile but does
not switch the active profile. Add `--activate` or run
`agent-model profile set work` when you want subsequent commands to use it by
default.

Profile names must match `^[a-z0-9][a-z0-9_-]{0,63}$` (lowercase letters,
digits, `_`, `-`) and must not be a reserved internal or Windows device name.
Uppercase is rejected so two names cannot alias to the
same directory on case-insensitive filesystems (default macOS APFS / Windows
NTFS) — `--profile Work` followed by `--profile work` would otherwise
silently overwrite the first profile's tokens.

### ChatGPT profiles

`chatgpt-login` accepts the same `--profile`, `--token-dir`, and `--activate`
flags, with an independent set of profiles under the default root:

```text
~/.config/agentmodel/chatgpt/<profile>/auth.json
```

```sh
agent-model chatgpt-login --profile work
agent-model chatgpt-login --profile personal --activate
```

Precedence and name rules match Anthropic
(`--profile > AGENTMODEL_PROFILE > <chatgpt-profile-root>/profile > default`);
`<chatgpt-profile-root>` is `CHATGPT_TOKEN_DIR` when that variable is set,
otherwise `~/.config/agentmodel/chatgpt`. The ChatGPT active-profile sidecar is
separate from the Anthropic one.
Point a deployment's `oauth_token_dir` at the chosen profile directory to serve
with that account. Existing single-store `<chatgpt-profile-root>/auth.json`
installs keep working — the reader treats them as the `default` profile.

### ChatGPT image generation

A `chatgpt` deployment can serve `POST /v1/images/generations` even though
the ChatGPT subscription has no standalone images API — the Responses
backend's built-in `image_generation` tool reaches `gpt-image-2-codex` and is
billed to subscription quota instead. Point the deployment's `model` at the
account's *current* Codex chat model (not a fixed value like `gpt-5` —
that's rejected by the backend):

```yaml
model_list:
  - model_name: "gpt-image-codex"      # what clients call
    deployments:
      - provider: "chatgpt"
        model: "gpt-5.5"                # account's current Codex model
        auth_mode: "subscription"
        oauth_token_dir: "/home/alice/.config/agentmodel/chatgpt/default"
```

Clients then call `POST /v1/images/generations` with `model: "gpt-image-codex"`.
For this ChatGPT-backed path, `size` and `quality` are forwarded to the tool,
but `n` is deliberately not sent because the Codex backend rejects
`tools[0].n`; current ChatGPT subscription image generation returns one image
per request. This is an experimental, reverse-engineered path — see
`services/agentmodel/provider/chatgpt/RISK.md` for the same caveats that
apply to chat.

### Gemini image and video generation

`gemini` is the only provider that serves both extra modalities, and the only
one wired into `/v1/videos/generations` (the Replicate passthrough can reach
video models too, but only as a raw vendor-protocol surface, not this
normalized one). Both run on a single `GEMINI_API_KEY`:

```yaml
model_list:
  - model_name: "image-nano-banana"
    deployments:
      - provider: "gemini"
        model: "gemini-2.5-flash-image"   # "nano banana"
        auth_mode: "api_key"
        api_key_env: "GEMINI_API_KEY"

  - model_name: "veo-3"
    deployments:
      - provider: "gemini"
        model: "veo-3.1-generate-preview"
        auth_mode: "api_key"
        api_key_env: "GEMINI_API_KEY"
```

**Images** (`POST /v1/images/generations`) normalize to the same OpenAI-shaped
response as the `openai` provider, but Gemini has no dedicated images endpoint —
output comes back inline from `generateContent` with an `IMAGE` response
modality. Because Gemini returns base64 inline data and never a hosted URL, the
response is always `b64_json` **regardless of what `response_format` you ask
for**.

**Video** (`POST /v1/videos/generations`) is the gateway's only normalized async
modality. The call returns a `VideoOperation` immediately, with a gateway-issued
opaque `id`; poll `GET /v1/videos/{id}` until `status` is terminal. On success
each `videos[].url` is a gateway content URL (`/v1/videos/{id}/content?index=N`)
the client fetches with its own bearer — the gateway proxies the bytes from the
upstream with the provider credential, so the client never needs it (#1493):

| `status` | Meaning |
|---|---|
| `queued` | Submitted, not started |
| `running` | Generating |
| `succeeded` | `videos[]` populated |
| `failed` | `error` populated |

Alongside `prompt`, the request accepts `negative_prompt`, `aspect_ratio`
(e.g. `16:9`), `resolution` (e.g. `1080p`), `duration_seconds`, and `n` (the
provider may cap it). It also accepts a `user` tracking field, which Gemini does
not forward. The gateway stores no operation state: the token encodes the
logical model, provider name, and provider operation ID. With unchanged config
and a single matching video-capable provider deployment, a poll survives a
gateway restart.

Two things bite in practice. Fetching the gateway content URL still requires
the caller's gateway bearer token, and each fetch re-polls Gemini and proxies
the upstream asset rather than persisting the bytes (#841).
And video is unmetered: the `gemini/veo-*` registry entries carry no cost
fields, so submissions record no cost at all (see [Cost tracking](#cost-tracking))
and spend reports under-count them. Images are priced normally.

### Migrating a legacy single-store install

Users who logged in before this change have a single `<base>/auth.json` file.
The reader transparently treats it as the `default` profile, so existing
setups keep working. To convert the layout explicitly — eliminating the
conditional-alias resolution and matching what fresh installs do — run:

```sh
agent-model profile migrate            # rename <base>/auth.json → <base>/default/auth.json
agent-model profile migrate --dry-run  # describe the move without touching anything
```

The migration is idempotent (a no-op when no legacy file exists). Note that
`os.Rename` moves a symlink itself rather than its target: if `<base>/auth.json`
was a symlink, the symlink ends up at `<base>/default/auth.json` after migration.
Any tool that reads the old path directly needs to be re-pointed, and a relative
symlink may need its target adjusted for the new directory.

Running `agent-model profile set` does not hot-swap identities for an already
running `agent-model serve` process. Server identity is bound at startup from
the YAML deployment config. For a service deployment, point `oauth_token_dir` at
the exact profile directory:

```yaml
oauth_token_dir: "/home/alice/.config/agentmodel/anthropic/work"
```

Profiles are also how you stock a multi-account pool: log each subscription into
its own profile directory, then list them all on one deployment so the pool
rotates between accounts on rate-limit.

```console
$ agent-model anthropic-login --profile work
$ agent-model anthropic-login --profile personal
```

```yaml
oauth_token_dirs:
  - "/home/alice/.config/agentmodel/anthropic/work"
  - "/home/alice/.config/agentmodel/anthropic/personal"
```

## API

All `/v1/*` endpoints require a valid token via `Authorization: Bearer <token>`
or `X-Api-Key: <token>` (for Anthropic SDK-compatible clients). The master token
comes from `auth.bearer_token_env` (default `AGENT_MODEL_TOKEN`); virtual keys
work on non-master endpoints. Health endpoints are unauthenticated.

Endpoints marked **(master only)** reject virtual keys — they require the
master bearer token (`403 master_token_required` otherwise).

This table is kept honest by `TestDocEndpointsMatchRoutes`
(`services/agentmodel/api/docs_endpoints_test.go`): it walks the live router and
fails CI if any `/v1` route here is missing, stale, or undocumented. Update this
table in the same change that adds or removes a route.

| Method | Path | Description |
|---|---|---|
| `POST` | `/v1/chat/completions` | OpenAI-shaped chat completions, streaming and non-streaming |
| `POST` | `/v1/messages` | Anthropic-shaped Messages API: native passthrough or bridged-provider translation |
| `POST` | `/v1/embeddings` | OpenAI-shaped embeddings |
| `POST` | `/v1/images/generations` | OpenAI-shaped image generation |
| `POST` | `/v1/videos/generations` | Video generation (async); returns a job id to poll |
| `GET` | `/v1/videos/{id}` | Poll a video-generation job |
| `GET` | `/v1/videos/{id}/content` | Stream a result video's bytes, proxied with the gateway's provider credential |
| `POST` | `/v1/predictions` | Replicate passthrough: create a prediction |
| `GET` | `/v1/predictions/{id}` | Replicate passthrough: get a prediction |
| `POST` | `/v1/predictions/{id}/cancel` | Replicate passthrough: cancel a prediction |
| `POST` | `/v1/models/{owner}/{name}/predictions` | Replicate passthrough: create a prediction addressed by model |
| `GET` | `/v1/models` | Model list; `?available` also lists unconfigured upstream models, each tagged with a `configured` flag |
| `GET` | `/v1/limits` | Rate-limit + budget snapshot (see below) |
| `GET` | `/v1/usage` | Usage/spend rollups, grouped by `model`/`provider`/`api_key`/`auth_mode`/`day` |
| `GET` | `/v1/account/anthropic/usage` | Upstream Anthropic subscription usage for one `?profile=` (see below) |
| `POST` | `/v1/oauth/chatgpt/start` | Start ChatGPT device-code OAuth **(master only)** |
| `POST` | `/v1/oauth/chatgpt/poll` | Poll ChatGPT device-code OAuth **(master only)** |
| `POST` | `/v1/keys` | Mint a virtual API key; returns the token once **(master only)** |
| `GET` | `/v1/keys` | List virtual API keys (never returns plaintext) **(master only)** |
| `GET` | `/v1/keys/{id}` | A key's config plus successful spend retained in `request_logs` (lifetime only when no rows were purged) **(master only)** |
| `POST` | `/v1/keys/{id}/revoke` | Disable a key without deleting it **(master only)** |
| `POST` | `/v1/keys/{id}/unrevoke` | Re-enable a revoked key **(master only)** |
| `DELETE` | `/v1/keys/{id}` | Delete a key **(master only)** |
| `GET` | `/healthz`, `/livez`, `/readyz` | Liveness/readiness checks (unauthenticated) |
| `GET` | `/metrics` | Prometheus metrics when `telemetry.metrics.enabled` is true (unauthenticated) |

OpenAI-compatible clients can point at agent-model by overriding the base URL.
Anthropic SDK-compatible clients can point at the same base URL and use
`/v1/messages`.

### `GET /v1/limits`

Returns a quota/rate-limit snapshot so callers can make routing/backoff
decisions without spending a request. The response shape matches the
`BackendLimits` contract consumed by `agent-shell`'s `limits` probe
(`services/agentshell/limits.go`): a `status` (`ok`/`limited`) plus a `windows`
array. One window is emitted per configured cap — `rpm` (unit `requests`) and
`tpm` (unit `tokens`) — on each deployment present in the live router, carrying
the configured `limit`; a deployment currently parked in a rate-limit cooldown
is reported with `window_status: "limited"` and `reset_at`/`reset_in_seconds`.
If a cooled deployment has no `rpm` or `tpm` cap, a synthetic `:cooldown` status
window carries the same reset. Configured spend caps are reported the same way,
one window per cap with unit `usd`:
`org/<id>:budget` for the gateway-wide `budget` and `key/<name>:budget` for each
`keys[]` entry with a `max_budget`
(the `window` field carries the `budget_duration`; a lifetime cap omits it).
`rpm`/`tpm` windows now carry live current-minute numbers — `used`,
`remaining`, `used_percent` from the router's rate meter (the same counters
pre-call enforcement skips deployments on), flipping to
`window_status: "limited"` with a reset at the next minute boundary once the
cap is reached. Budget (`usd`) windows still omit `used`/`remaining` until
ledger metering is wired here (#500); budget caps are *enforced* on the
spending endpoints regardless (`400 budget_exceeded` pre-call).

```jsonc
{
  "agent": "agentmodel",
  "status": "ok",
  "windows": [
    {
      "limit_name": "org/default:budget",
      "window": "720h", "unit": "usd",
      "limit": 100.0,  // remaining/used omitted until live metering (#500)
      "reset_description": "fixed UTC window; resets every 720h",
      "source": "agentmodel", "window_status": "ok",
      "as_of": "2026-06-16T10:44:51Z"   // every window carries as_of
    },
    {
      "limit_name": "sonnet/anthropic/claude-sonnet-4|api_key:rpm",
      "window": "1m", "unit": "requests",
      "limit": 60,
      // rpm/tpm windows always carry live current-minute usage:
      "used": 12, "remaining": 48, "used_percent": 20.0,
      "source": "agentmodel", "window_status": "ok",
      "as_of": "2026-06-16T10:44:51Z"
    }
  ]
}
```

### `GET /v1/account/anthropic/usage`

Reports the **upstream Anthropic subscription** account's usage — distinct from
`/v1/limits`, which reports the gateway's *own* rpm/tpm/budget caps. It is served
here rather than fetched directly by `agent-shell`; concurrent calls to this
endpoint for the same profile share a cached refresher within agent-model.
`agent-shell`'s `limits claude` probe consumes this over HTTP.

`?profile=<name>` selects the Anthropic profile (default `default`). The response
is the same `BackendLimits` shape as `/v1/limits`: a `status` plus `windows`
whose possible names are `claude_5h`, `claude_weekly`,
`claude_oauth_apps_weekly`, `claude_opus_weekly`, and
`claude_sonnet_weekly`. Each emitted window has `used_percent`, optional
`reset_at`, and `source: "claude_oauth"`. Failure to obtain a usable token
returns HTTP 200 with a structured `error`
(`claude_oauth_login_required` / `claude_oauth_unavailable`) so the caller
renders a normal `unknown` row instead of treating the probe as a transport
failure. An expired access token is refreshed first when a refresh token is
available. Re-authenticate with `agent-model anthropic-login`.

## Cost tracking

Token-priced requests compute USD cost from provider-returned usage and the
embedded price registry (`services/agentmodel/cost/model_prices.json`,
keyed first as `<provider>/<resolved-model>` and then by bare resolved model
id, e.g. `openai/gpt-4o`). Replicate prediction creates instead compute the
supported per-output costs from the request body and registry; video-generation
submissions currently record no cost. Provider-backed audit rows that compute cost record
a `cost_source` so a `$0` is not ambiguous; failed requests with no computed
cost can leave it empty:

| `cost_source` | Meaning |
|---|---|
| `priced` | Cost computed from a registry entry. |
| `subscription` | Subscription-billed model (Claude Pro/Max, ChatGPT) — `$0` by design. |
| `unpriced` | Model absent from the registry — cost falls back to `$0` but is actually **unknown**, and spend reports under-count these. The token-priced paths (chat/messages/embeddings/images) log a warning; the Replicate passthrough records `unpriced` without one. |
| `cache` | Served from the [response cache](#response-cache) — `$0`, advances no budget (see that section). |

If you see `unpriced` rows, add the model with the applicable rates supported
by its request path so its spend is counted. Subscription models are
intentionally `$0`. The registry's ChatGPT subscription entries are
`chatgpt/gpt-5`, `chatgpt/gpt-5-codex`, `chatgpt/gpt-5-pro`,
`chatgpt/gpt-5.5`, `chatgpt/gpt-5.6-sol`, `chatgpt/gpt-5.6-terra`,
`chatgpt/gpt-5.6-luna`, `chatgpt/gpt-5.4`, `chatgpt/gpt-5.4-mini`, and
`chatgpt/gpt-5.3-codex-spark`; these report `cost_source: "subscription"`
rather than `unpriced`.

When provider usage includes prompt-cache tokens, OpenAI-shaped responses keep
the flat gateway fields (`cache_read_input_tokens`,
`cache_creation_input_tokens`) and also emit OpenAI-compatible
`prompt_tokens_details.cached_tokens` /
`prompt_tokens_details.cache_write_tokens`.

## Response cache

An opt-in cache for **non-streaming** `/v1/chat/completions`. It is **off by
default**; enable it in config:

```yaml
cache:
  enabled: true
  ttl: "10m"          # entry lifetime; empty/"0" = no expiry (L1 LRU-bounded)
  max_items: 1000     # in-memory LRU capacity (default 1000)
  sqlite_path: ""     # optional persistent L2 path; empty = memory-only
```

The cache is content-addressed: the key is a SHA-256 over the cache-key-affecting
request fields (`model`, `messages`, `temperature`, `max_tokens`, `top_p`,
`stop`, `tools`, `tool_choice`, `prompt_cache_key`, `thinking`,
`reasoning_effort`). `stream` and `user` are excluded from the key; streaming
requests bypass the cache, and `user` is treated as a tracking tag rather than
an output determinant (matching LiteLLM's default key). ChatGPT subscription deployments also forward
`prompt_cache_key` to the upstream Responses body. For ChatGPT-backed chat, the
gateway sends the Codex `session-id` header used for prompt-cache stickiness:
an explicit `prompt_cache_key` becomes that header; otherwise agentmodel
derives a UUID-shaped stable id from system/developer text, tools, and the role
and text projection of the first message that is neither a system nor a
developer message, so later turns in the same conversation route to the same
backend node. ChatGPT image generation is one-shot and sends no `session-id`.

Two layers: a bounded in-memory **LRU (L1)**, and — when `sqlite_path` is set —
a persistent **SQLite (L2)** that survives restarts (a read missing in L1 but
found in L2 is promoted back into L1). A `ttl` limits how long newly written
persisted entries are served, but it is not a live on-disk size cap. Expiry
differs by layer: L1 deletes an expired entry when a read finds it, but L2 treats an expired row as a
miss *without* deleting it on the read path (a write there would contend for the
single writer) — an expired L2 row remains until a later `Set` overwrites that
key or the one-shot startup sweep deletes it.

Each cache-enabled, non-streaming chat request that reaches the cache lookup
carries an `X-Agentmodel-Cache: hit|miss` response header. A **hit** returns
the original response verbatim, never reaches an upstream provider, and is
recorded in the audit log at **`$0` with `cost_source: "cache"`** — token counts
stay visible (so "tokens saved" is measurable) but a cache hit advances no
budget. Allowlist and budget enforcement still run *before* the cache, so a
request for a model outside the calling key's allowlist, or whose applicable
budget is exhausted, cannot be served from it.

Caveats: identical requests return identical bodies, so enable it only where that
is acceptable (a non-zero `temperature` still replays the cached completion).
Streaming responses are not cached in this version.

## Telemetry (metrics & tracing)

Two optional, off-by-default observability sinks receive the same audit-row
structure sent to the store. **Only
metadata is exported — never prompt/response content.** Enable them in config:

```yaml
telemetry:
  metrics:
    enabled: true          # serve GET /metrics (Prometheus)
  tracing:
    enabled: true          # export OTLP spans
    service_name: agentmodel
```

**Prometheus** — when `metrics.enabled`, an unauthenticated `GET /metrics`
endpoint is served alongside the health checks. Exposed series (labels in
parentheses):

| Metric | Type | Labels |
|---|---|---|
| `agentmodel_requests_total` | counter | model, auth_mode, status |
| `agentmodel_tokens_total` | counter | model, kind (prompt\|completion) |
| `agentmodel_spend_usd_total` | counter | model |
| `agentmodel_cost_source_total` | counter | source (priced\|subscription\|unpriced\|cache) |
| `agentmodel_request_duration_seconds` | histogram | model |
| `agentmodel_latency_per_output_token_seconds` | histogram | model |
| `agentmodel_deployment_requests_total` | counter | model, provider, deployment, result |
| `agentmodel_deployment_cooldowns_total` | counter | model, provider, deployment |
| `agentmodel_content_log_rotations_total` | counter | result (success\|failure) |
| `agentmodel_content_log_disk_bytes` | gauge | none |

Labels are deliberately low-cardinality — there are no per-key/per-org/per-user
labels (use the audit log or `/v1/usage` for that). A rising
`agentmodel_cost_source_total{source="unpriced"}` is the alert for a stale
price registry.

**OTLP tracing** — when `tracing.enabled`, one span per completed audited request
is exported via OTLP/HTTP. The exporter is configured by the standard OpenTelemetry environment
variables; common ones include:

| Var | Purpose |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | collector base URL (e.g. `http://localhost:4318`) |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | traces-specific endpoint override |
| `OTEL_EXPORTER_OTLP_HEADERS` | auth/routing headers (e.g. `Authorization=Bearer …`) |

Point these at any OTLP-compatible backend (Tempo, Honeycomb, Datadog, a
collector fanning out to Langfuse, etc.). Spans flush on graceful shutdown.

## Content logging

The audit log and telemetry above are **metadata only** and never carry prompts
or completions. The audit log includes the hashed key; telemetry exports the
request's model, token, cost, latency, and status metadata without that key hash.
The content log is
the one place agentmodel records the **raw exchange**: enable it when you need to
debug a provider translation, review prompts, or reproduce a bad completion.

It is **off by default**. Set a path to enable it:

```yaml
content_log:
  path: "/var/log/agentmodel/content.jsonl"   # empty/omitted = disabled (the default)
```

**Format.** Each content record is appended as one line of JSON (JSON Lines):

```json
{"timestamp":"2026-06-16T10:44:51.301Z","request_id":"host/abc-000004","request":{…},"response":{…}}
```

| Field | Meaning |
|---|---|
| `timestamp` | UTC time the record was written (after the response completes) |
| `request_id` | the inbound `X-Request-Id`, or a gateway-generated ID when that header is absent |
| `request` | request JSON in the endpoint's shape; chat is re-marshaled from the parsed request, while Messages and Replicate creates preserve the raw JSON |
| `response` | the response body (see streaming note below); Replicate creates preserve the raw upstream JSON before callback URLs are rewritten for the client |

**Coverage.** `/v1/chat/completions` (OpenAI shape), `/v1/messages` (Anthropic
shape), and Replicate prediction creates (`POST /v1/predictions` and
`POST /v1/models/{owner}/{name}/predictions`, Replicate shape) are logged.
All other endpoints, including `/v1/embeddings`, images, and videos, are not
logged. The `request`/`response` JSON is in whichever shape the endpoint
speaks, so chat records are OpenAI-shaped, messages records are
Anthropic-shaped, and Replicate records are Replicate-shaped.

**Streaming is reassembled.** For successful streamed calls the `response` is
the *final assembled message*, not the delta stream — OpenAI chunks are merged back into a
`chat.completion` (content, tool-call arguments, reasoning), and the Anthropic
SSE sequence is rebuilt into a Messages object (text / `tool_use` / `thinking`
blocks, `stop_reason`, folded usage). If the client disconnects, the partial
content assembled up to that point can still be logged. A request rejected
before it reaches the upstream (auth, validation, a `4xx` the gateway raises)
writes no content record. Chat calls also write no content record when provider
dispatch fails, a non-streaming provider call returns an error, or a stream
yields a provider error. By contrast, Messages and Replicate creates preserve
raw JSON non-2xx upstream response bodies; a Messages stream is logged only
when a `message_start` event made an assembled response possible.

**Reading it.** It is plain JSON Lines — use `jq`:

```sh
# last prompt + reply text from an Anthropic-shaped record
tail -1 content.jsonl | jq '{model: .request.model,
  prompt: (.request.messages | last | .content),
  reply: ([.response.content[]? | select(.type=="text") | .text] | join(""))}'
```

**Operational caveats.**

- **One record per logged call, full context each time.** Agentic clients (pi, Claude
  Code) resend the *entire* conversation on every turn, so the log grows quickly
  and is highly repetitive — a single multi-turn task can write several
  multi-hundred-message records. Expect this file to be large.
- **It holds raw prompts and completions** — system prompts, tool I/O,
  everything. The logger applies no redaction or configured size truncation;
  streamed records can still be partial on client disconnect as noted above.
  The file is created `0600`; treat it as sensitive and put it on encrypted
  storage if your threat model needs it.
- **Best-effort, synchronous writes.** A write or marshal error is logged and
  swallowed, so content logging never fails the request. Writes, inline rotation,
  compression, and pruning are serialized and can add work after the response
  is written. When disabled it does no work (streaming reassembly is skipped
  entirely).

### Rotation, compression, and retention

The metadata `retention` purge (below) does **not** touch the content log — it
is a separate file with its own policy. Left unbounded it once filled the
agent-model disk, so the content log rotates itself. Every knob is opt-in and
independent; with none set the log stays a single ever-growing file (the historic
behavior):

```yaml
content_log:
  path: "/var/log/agentmodel/content.jsonl"
  max_size_mb: 100        # active-file size threshold; 0 = no size rotation
  rotate_every: "24h"     # rotate when the active file reaches this age; empty = no time rotation
  compress: true          # gzip rotated files (adds a .gz suffix)
  max_backups: 30         # keep at most N rotated files; 0 = unlimited
  max_age: "720h"         # delete rotated files older than this; empty = keep
  max_total_mb: 2048      # delete oldest rotated files until active+backups fits; 0 = off
```

- **Triggers.** The active file rotates when the next line would push it past
  `max_size_mb`, or once it has been open at least `rotate_every`. Both may be
  set; either one fires. An empty active file never rotates.
- **Rotated name.** `content.jsonl` is renamed to
  `content.jsonl.<UTC-timestamp>Z` (e.g. `content.jsonl.20260717T120000.000000000Z`),
  with `-N` appended after `Z` if that name already exists, then gzipped when
  `compress` is on. The timestamp portion of each name is lexically sortable.
- **Serialized rotation.** Rotation — close, rename, reopen, compress, prune —
  runs under the same lock that serializes writes, so concurrent lines do not
  interleave and a successful rotation does not split the in-flight line.
  Rotation or write failures are reported and can lose that record.
- **Retention** is applied after each rotation, in order: delete files older than
  `max_age`, then keep only the newest `max_backups`, then delete the oldest
  remaining until `active + backups ≤ max_total_mb`. The active file is never
  deleted, so a single active file larger than `max_total_mb` is left in place.
- **Monitoring.** Two metrics are exported (when `telemetry.metrics` is on):
  `agentmodel_content_log_rotations_total{result="success|failure"}` and the
  gauge `agentmodel_content_log_disk_bytes` (active + backups, refreshed every
  minute and after each rotation). Alert on a rising `failure` count and on
  abnormal `disk_bytes` growth; rotation failures are also logged at `ERROR`
  (`content log rotation failed`).

## Data retention & privacy posture

**The `request_logs` audit table stores metadata only — no prompt/response
content.** It records token counts, computed cost, latency,
status, the requested and used-model fields, provider attribution where the
handler populates it, and a **hashed** API key. For `/v1/messages`, terminal
provider/messagesbridge failures are attributed to the deployment that failed;
failures with no single
deployment to blame (for example policy rejection, an unconfigured model, or an
exhausted fallback walk) keep the requested model as `model_used` and leave
`provider`/`cost_source` empty. There are no message/response columns — request
and completion content is never written to `request_logs`. Telemetry exports a
subset of that metadata. The API key is stored as a hash, never in plaintext.

**At rest and egress.** The audit SQLite database and optional persistent cache
SQLite file live on the operator's own disk and are not encrypted by agentmodel;
protect them with filesystem/disk encryption if your threat model requires it.
Requests and responses travel to and from the configured upstream providers.
Authenticated callers can retrieve audit-derived rollups through `/v1/usage`;
when enabled, `/metrics` exposes operational metrics and OTLP tracing exports
request metadata. None of those surfaces carries prompt/response content.

**Retention is opt-in.** By default rows are kept forever. Set a window to have
old rows purged automatically:

```yaml
retention:
  period: "720h"     # delete audit rows older than 30 days; empty = keep forever
  interval: "1h"     # how often the background purge runs (default 1h)
```

When `period` is set, `serve` runs a purge once at startup and then every
`interval`. You can also purge on demand without running the server:

```sh
agent-model purge --config config.yaml                 # uses retention.period
agent-model purge --config config.yaml --older-than 168h   # one-off override
```

**Content logging is a separate, explicit opt-in (defaults off).** The audit
table and telemetry exporters are metadata-only and stay that way. The
[content log](#content-logging) can hold both raw prompts and completions; the
optional response cache also retains completion bodies, in memory and
optionally in SQLite. The content log is off unless you set `content_log.path`,
created as a `0600` JSON Lines file with no redaction, and **not** covered by the
`retention` purge above; configure its separate rotation/retention policy. See
[Content logging](#content-logging) for the full description.

See `cmd/agent-model/example_config.yaml` and `services/agentmodel/README.md`
for a fuller configuration walkthrough.
