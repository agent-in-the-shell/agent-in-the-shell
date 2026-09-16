# AgentModel

An OpenAI-compatible LLM gateway that routes requests across multiple providers,
including OpenAI, Anthropic, Gemini, ChatGPT subscription, Azure OpenAI, DeepSeek,
and Replicate. It supports weighted
multi-account pools, per-model fallbacks, request auditing, cost tracking,
bearer-token auth, and an optional [content log](#content-logging) of full
request/response bodies.

Source: `services/agentmodel/`

> agent-model serves inference to HTTP clients. agent-shell independently runs
> installed coding-agent CLIs; it does not route their inference through this gateway.

## Build

```
go build -o build/ ./cmd/agent-model
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
agent-model prompt [--model <name>] [--timeout <dur>] ["<prompt>"]
                                      debug: send one prompt through the gateway ($AGENT_MODEL_URL,
                                      $AGENT_MODEL_TOKEN) and print the reply; proves the model is
                                      online (exit 1 if not). Without an argument it sends a tiny
                                      built-in liveness prompt; model/latency/token counts go to
                                      stderr
agent-model usage [--config <path>] [--db <path>] [--since 7d] [--by model] [--org default] [--limit N] [--json]
                                      report request_logs usage: requests, tokens, cost, error_rate;
                                      resolves the DB from --db, --config, or the default config
                                      path (--db wins when both are set)
agent-model limits [claude|codex] [--profile <name> | --profiles all] [--json] [--watch <dur>]
                                      subscription quota for the accounts this process holds
                                      credentials for; non-consuming, needs no running server;
                                      --watch redraws every interval (300 or 5m)
agent-model status [--config <path>] [--json]
                                      offline health snapshot: gateway liveness, token-store
                                      freshness, and model routing; exit 1 if the gateway is
                                      down or any credential referenced by a nonzero-weight
                                      deployment is unusable
agent-model configure-models [--config <path>] [--timeout <dur>]
                                      query configured upstream model-list endpoints, group results
                                      by credential source, and add selected models to model_list
agent-model configure-fallbacks [--config <path>]
                                      choose a model and set or clear its ordered fallback chain;
                                      both configuration commands save atomically and require restart
agent-model keys <create|list|show|disable|enable|revoke|delete|rotate>
                                      manage DB-backed virtual keys through the running gateway;
                                      uses $AGENT_MODEL_BASE_URL and the master $AGENT_MODEL_TOKEN
```

This list is guarded by `TestCLICommandsDocumented`
(`cmd/agent-model/commands_doc_test.go`): it parses the command dispatch in
`main.go` and fails CI if a dispatched subcommand is missing from this block or
from the `--help` text. Update both when you add or remove a command.

### Runtime virtual keys (`keys`)

The `keys` command is an operator client for the master-only `/v1/keys*`
endpoints. It defaults to `http://127.0.0.1:8080`; set
`AGENT_MODEL_BASE_URL` (or pass `--base-url` after the subcommand) and provide
the master token through `AGENT_MODEL_TOKEN` (or `--token`). Plaintext tokens
are returned only by create/rotate and are never persisted by the CLI.

```sh
agent-model keys create --name example-client --models gpt-5.6-sol,gpt-5.6-luna
agent-model keys list
agent-model keys show key_abc
agent-model keys revoke key_abc
agent-model keys disable key_abc # reversible pause
agent-model keys enable key_abc  # only non-revoked keys
agent-model keys delete key_abc  # alias for permanent revocation; retains history
agent-model keys rotate key_abc              # Service: old secret invalid now; legacy: old key stays active
agent-model keys rotate --revoke-old key_abc # legacy: revoke old key after new key exists
```

Create also accepts `--max-budget`, `--budget-duration`, `--duration`, and a
JSON `--metadata` value. Every command supports `--json`. For secret-manager
pipelines, `create` and `rotate` support `--token-only`: stdout contains only
the one-time token, while errors and diagnostics remain on stderr.

Legacy CLI rotation is for non-revoked keys; Portal keys use the personal page. Service keys use the stable-ID, immediate rotation described below. Legacy rotation copies the
name, model allowlist and budget settings (not expiry). Its metadata records `rotated_from`. The old
key intentionally remains active by default so callers can deploy the new
credential before revoking the old one.

### Subscription quota (`limits`)

```sh
agent-model limits --profiles all              # one snapshot
agent-model limits --profiles all --watch 300  # redraw every 5 minutes
agent-model limits --profiles all --json       # machine-readable, schema_version 2
```

`--watch` takes either a bare second count (`300`, the spelling `watch -n`
takes) or a Go duration (`5m`), with a 5-second floor: every tick is a live
authenticated probe per profile — and may rotate a single-use OAuth refresh
token on the way — while the windows it reports move on 5-hour and weekly
timescales, so a faster poll buys no information. `--watch` and `--json` are
mutually exclusive; loop the shell around `--json` if you want a stream.

**Use `--watch` rather than `watch(1)`.** The full seven-column table is about
125 columns wide, so `watch -n 300 'agent-model limits --profiles all'` folds
every row at an 80-column terminal and adjacent rows interleave — intermittently,
because the RESETS cell changes width as countdowns roll over (`14:30 CST`
versus `Aug 19 09:33 CST`). `--watch` re-measures the terminal on every frame
and degrades the layout instead of wrapping it: RESETS and SOURCE move to an
indented continuation line under each row, and WINDOW is clipped with `..` if
even that does not fit. It also keeps one process on the OAuth probe instead of
re-execing the binary each tick.

The adaptation is keyed on stdout being a terminal. Piped or redirected output
is always the unmodified seven-column table, so anything parsing those columns
is unaffected — by `--watch` as much as by a plain run.

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

On `anthropic`/`anthropic-oauth` deployments, optional `cache_ttl` selects the
TTL of the gateway-injected prompt-cache breakpoint: omit it for the default
`5m`, or set it to `5m` or `1h`. Other providers reject this field.

Whether that breakpoint is placed at all is decided by `prompt_cache_key` —
OpenAI's own field, meaning "requests carrying this key share a prefix", and the
only vocabulary `/v1/chat/completions` has for prompt caching. Sending one tells
the gateway the prefix recurs and is worth caching, in either auth mode. An
Anthropic cache write costs 1.25x normal input, so a request with no key gets no
breakpoint rather than one that can only be paid for and never read back. With
no key the historical default still applies: `auth_mode: subscription` caches,
`api_key` does not. Clients speaking `/v1/messages` place their own
`cache_control` markers instead and the gateway defers to them; either way the
four-breakpoint Anthropic limit is enforced, and a request already at the cap
gets no injected marker.

### Providers

`provider:` selects the upstream client. These providers have a native client,
with their own wire shaping, auth, and capabilities:

| `provider` | Upstream | Notes |
|---|---|---|
| `openai` | OpenAI | Chat, embeddings, images |
| `anthropic` | Anthropic | `api_key`; `x-api-key` auth |
| `anthropic-oauth` | Anthropic | `subscription` via `oauth_token_dir`, or `oauth_token_dirs` for a multi-account pool |
| `chatgpt` | ChatGPT subscription | `subscription`; chat and images; also serves `/v1/messages` and streaming `/v1/responses` (including hosted tools such as `web_search`) |
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
`/v1/responses`, `/v1/messages`, `/v1/embeddings`, `/v1/images/generations`,
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
  Anthropic error envelopes; non-`429` raw Anthropic upstream error bodies are
  still proxied verbatim, while upstream `429` bodies are consumed for
  cooldown/fallback.
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
`used`/`remaining` reporting on that endpoint is ; until it lands, the
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
`""`. All three official OpenAI SDKs treat an absent `code`/`param`
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
absent; non-`429` raw Anthropic upstream error bodies are proxied unchanged.
Upstream `429` bodies are consumed for cooldown/fallback; if the routing walk
exhausts, the gateway returns its typed rate-limit error instead.
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

An upstream failure takes its `type` from the HTTP status the provider actually
returned, not from the text of its response body. The status decides the family
— `401` authentication, `403` permission, `404` not-found, `408`/`504` timeout,
`429` rate limit, other `4xx` invalid-request, `503`/`529` service-unavailable,
other `5xx` upstream — and only within a family is the body consulted, to pick
between `context_length_exceeded`, `content_policy_violation` and a plain bad
request, or between `rate_limit_exceeded` and `insufficient_quota`. That split
matters because the family decides retryability, and therefore whether the
router falls back at all: a genuine `5xx` whose body happened to contain a
request id like `req_c401e9ab`, or a token count like `129403`, must not be read
as a terminal `401`/`403` and stop the walk. Failures that never reached an
upstream — transport errors, cancellations — have no status and are still
classified from their message.

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
OpenAI-compatible provider in the table above). `gemini` does not yet and falls
back to the router default on every `429`. The Replicate passthrough forwards
upstream `429` responses directly, without gateway cooldown handling.

`router_cooldown: "0"` disables cooling at the deployment layer, and an upstream
hint does not override it. It does not reach the pool's per-credential cooldown,
which is not configurable — a pooled credential is parked for the hint (or 5m)
regardless, up to the 24h clamp.

A gateway-generated `429` on chat and Messages carries `Retry-After` in whole
seconds only when its routing walk produced a concrete retry duration: an
upstream hint, a pooled deployment's calculated credential cooldown, or time
remaining on a deployment cooldown from an earlier request. It omits the header
when exhaustion comes solely from `rpm`/`tpm` caps or from fresh, unhinted
retryable failures in unpooled deployments.

At startup, the server also runs a best-effort background drift check against
live model lists for providers that support listing. Set `revalidate_interval`
to a positive Go duration such as `"1h"` to repeat the check and log only drift
transitions; omit it for the default one-shot startup check. The interval is
also the live-model-list cache TTL, and failed refreshes reuse a last-good list.

## Environment variables

| Var | Purpose |
|---|---|
| `AGENT_MODEL_URL` | base URL read by the shared Go gateway client (default: `http://localhost:8090`) |
| `AGENT_MODEL_MODEL` | default model read by the shared Go gateway client (default: `claude-haiku-4-5-20251001`) |
| `AGENT_MODEL_TOKEN` | bearer token read by the shared Go gateway client; also the default master-token env for `serve`, which reads the env named by `auth.bearer_token_env` |
| `AGENT_MODEL_CONFIG` | default `serve`/`status` config path when `--config` is omitted, and the `usage` config path when neither `--db` nor `--config` is supplied (default: `${XDG_CONFIG_HOME:-~/.config}/heros/agentmodel/config.yaml`; an existing `~/.config/agentmodel/config.yaml` is used in place) |
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
agent-model limits claude --profile work
agent-model limits claude --profiles all
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
per request. This is an experimental, reverse-engineered path.

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
upstream with the provider credential, so the client never needs it:

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
the upstream asset rather than persisting the bytes.
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

## Employee portal (opt-in)

The same binary embeds a small self-service page at `/portal/`. This is separate
from `/v1` bearer authentication: an Access assertion never authenticates a
model request, and a master/API key never authenticates the portal. Place the
portal origin behind Cloudflare Access; forward `Cf-Access-Jwt-Assertion` and
preserve the original Host. The application verifies RS256 signatures from the
configured issuer's fixed `/cdn-cgi/access/certs` URL (no redirects), issuer,
audience, expiry, immutable subject and exact email domain. Email proxy headers
are ignored. Signing keys are cached for five minutes; failed refreshes retry no
more than once per minute, with a five-second network timeout and bounded body.

Example only (not a deployment change):

```yaml
portal:
  enabled: false              # explicitly opt in after configuring Access
  html_dir: ""                # embedded HTML; optional absolute directory
  origin: https://portal.example.com
  gateway_origin: https://agent-model.example.com # base URL shown to clients; optional
  issuer: https://your-team.cloudflareaccess.com
  audience: e74d87962a061c972188da0ecb062212a408c179ef26fbfec9071591b7fb0854
  email_domain: example.com # set the exact authorized employee domain
  admin_emails: [admin@example.com] # exact verified emails, not proxy headers
```

Enabled configuration requires the origin and identity fields. `admin_emails`
is optional (empty means no administrators); entries must be valid emails in the
employee domain. Administrator matching uses exact lowercased, trimmed JWT email.
Each portal-issued key has its own explicit model allowlist in `api_keys.models`.
New keys always store `[]`: no inference until an administrator grants models.
Empty or nil portal allowlists deny every model, never mean unrestricted access.
The per-key list is read on every bearer request, including model listing and
fallback routes; edits apply on the next request without rotation or restart.
Already-running requests retain their authenticated scope. Creation and rotation
work with an empty list. There is no individual budget or expiry; organization
budgets still apply. Traditional keys retain their existing empty-means-all semantics.
`GET /portal/api/me` returns only the caller's safe key policy, revision,
CSRF token, the configured `gateway_origin` and personal token statistics;
`?fields=session` omits the statistics. `key.models` (when a key exists) reports
that person's allowlist. `POST /portal/api/key`
creates once; `POST /portal/api/key/rotate` requires `{ "revision": N }`.
Mutation requests require exact Origin, JSON and a double-submit CSRF token
(`X-CSRF-Token` plus a Secure/HttpOnly/SameSite=Strict host cookie). Responses
are no-store, the page uses nonce CSP, and plaintext is returned only once and
never persisted or put in browser storage. A lost response requires refreshing
and rotating again; plaintext cannot be recovered.

Ownership is `(issuer, sub)` mapped durably to one stable managed-key ID, not
email. Creation and rotation are transactional; concurrent/stale rotations
return 409. Rotation changes only the current hash and revision, preserving the
individual model allowlist and disabled status. Old tokens no longer authenticate.
Revocation permanently retires the current secret, not the user's Google identity.
A user can replace a revoked credential via the same revision-checked rotation
endpoint: a fresh secret keeps the stable key ID and hash history, but starts
with **no model grants**. Only an administrator can reauthorize it. A separate
pause, if set, survives replacement. The old secret cannot be enabled or restored.
Historically hard-deleted Portal rows (from older binaries) remain read-only
identity tombstones; this change does not guess or recreate their missing policy. The temporary
portal does not change ledger, retention or other managed-key semantics:
request logs retain their original hashes, including late old-token completions;
organization accounting is unaffected. There is no cross-rotation per-key spend
aggregation. Creation/rotation reject capped keys, expiry or budget durations
with `unsupported_key_policy` (409), including policy
added by an operator after creation. Portal keys with such later policy edits
also fail closed at bearer authentication rather than silently undercounting. Use existing operator-managed keys outside
the portal when a per-key budget or expiry is needed.

### External portal HTML (optional)

By default (`portal.html_dir: ""`), both pages use the binary's embedded HTML.
For UI edits without restarting the gateway, set `portal.html_dir` to an absolute
filesystem directory; on a VM, `/var/lib/agent-model/portal` is recommended. Copy
the four files from `services/agentmodel/api/` there before enabling the setting:
`portal.html`, `portal_admin.html`, and the fragments both pages splice in,
`portal_shared.css` and `portal_shared.js`. Keep the directory root-owned with mode `0755`
and the HTML files root-owned with mode `0644`, readable but not writable by the
service account. These are trusted UI code, **not credentials**: never put keys,
tokens, or other secrets in the HTML or this directory.

Restart the gateway once to enable the setting. Thereafter each authenticated
page GET reads exactly `portal.html` or `portal_admin.html` from that directory,
plus `portal_shared.css` / `portal_shared.js` wherever the page carries the
`{{SHARED_CSS}}` / `{{SHARED_JS}}` placeholders (a fully inlined page needs neither),
without caching; HTML edits need no rebuild or restart. Write a new file beside
the target with the same ownership/mode, then atomically rename it over the target
(on the same filesystem) so a request never sees a partially written page.
Changing the configured directory itself still requires a restart.

This is not a public static directory: there is no directory listing, arbitrary
filename route, or URL-selected file. Existing Access JWT and administrator checks
run before reading HTML; nonce substitution (`{{NONCE}}` in inline script/style
attributes), CSP and no-store/security headers remain unchanged. Preserve those
nonce placeholders when editing. An unreadable or missing configured page returns
HTTP `503` with `portal_html_unavailable`, never stale embedded HTML. Config
validation checks the path format, not file existence; portal APIs are independent
of HTML file availability.

### Administrator dashboard and migration

`/portal/admin/` is restricted to verified configured administrators; employees
receive 403 on every admin path. The employee page only shows the admin link for
administrators. `GET /portal/admin/api/models` returns the configured gateway model
catalog; `PUT /portal/admin/api/models` accepts only
`{"key_id":"key_...","models":["configured-alias"]}` (or an explicit empty array),
using the same Origin/JSON/CSRF checks. Unknown/duplicate models, missing/null
arrays and extra fields are rejected. The atomic store update requires a matching
portal identity or explicit service marker and an existing key; legacy keys, revoked credentials and historical missing rows cannot be
modified. Only that key's models change, never its name, budget, status or secret.
Concurrent saves to the same key use last committed write wins.

There is no pre-issuance directory: an employee creates a deny-all key on the
personal page first, then appears in the dashboard for individual authorization.
The empty selection is shown as inference denied, distinct from revoked/disabled
credentials. No shared policy, template or role system is involved. The former
`/portal/admin/api/policy` endpoint is removed. A `portal_policy` table left by
an earlier build is neither created nor read; it can be dropped.

`GET /portal/admin/api/overview` lists portal identity/key state, including revoked
credentials (`revoked_at` is an RFC3339 timestamp when known), historical missing
rows displayed as revoked without an invented timestamp, service keys labeled `service`, traditional managed keys labeled `legacy_current_hash`,
and a read-only `master` row labeled **Master key** when the current master is configured.
The reserved public `key_id: system:master` is a reporting identity, not an
`api_keys`/`service_keys` row or Google identity; its `models` is `null`.
Per-key and displayed totals cover retained logs from the rolling 30 × 24 hours,
not organization-wide usage. Portal rows include all tracked rotation hashes;
Service, legacy and Master rows include only their current credential. Attribution
precedence is current master, historical employee ownership, then current managed
keys; reused hashes are counted once, identically in Monitoring and Users.
Other config-only keys are not listed. No prompt, completion, plaintext token, hash or
key metadata is exposed. Each live employee/service row includes its models and controls;
legacy rows are visibly unmanaged; Master and revoked rows cannot be edited.
The backend explicitly rejects model/state writes for the reserved Master identity,
even if a persisted key has that ID. Existing persisted rows using this reserved ID
are excluded from admin attribution, not treated as master traffic. This dashboard
cannot reveal existing secrets, add roles, change names, or change budgets/expiry.
`PUT /portal/admin/api/state` accepts only
`{"key_id":"key_...","disabled":true}` (or `false`) with the same admin,
Origin/JSON/CSRF checks. The atomic update rejects legacy/missing/revoked keys;
it changes only the existing disabled flag. Enable/Disable takes effect on the
next authentication and preserves model permissions, secret and rotation history.
It does not cancel in-flight requests. User yearly heatmaps are unchanged.

`POST /portal/admin/api/revoke` accepts only `{"key_id":"key_..."}` with the same
admin/Origin/JSON/CSRF checks. It permanently retires the credential and is
idempotent: repeat calls preserve the original revocation timestamp. There is no
restore or separate deleted state. The row, token hash, ownership, model policy,
and request/cost associations remain stored; grants cannot be edited while revoked.
Authentication reads revocation from the DB on every request, before cache use.
Service replacement rotates the same logical key/name and starts with no grants;
retired names remain reserved for that identity. Portal replacement
uses the personal-page flow described above; it does not create a second identity.

The Users tab groups Portal, Service, Master and Legacy rows. Active groups start
expanded; Disabled / expired and Revoked groups start collapsed with counts.
Revoked rows retain request-history links and show the revocation time, but no
Enable or grant editor. Searching opens matching groups. Disable remains a
reversible pause; Revoke has a permanent-action confirmation.

All managed-key delete entrypoints now retain records. `DELETE /v1/keys/{id}`
returns 204 for an existing key (including repeated calls), and the key remains
visible in list/info. `POST /v1/keys/{id}/revoke` is permanent.
`POST /v1/keys/{id}/enable` lifts reversible pauses on non-revoked keys;
attempting to enable a revoked key returns 409. Config and Master credentials
remain managed outside Portal.

SQLite startup adds nullable `api_keys.revoked_at`; existing disabled rows remain
**disabled**, not retrospectively permanent. Back up the database before upgrade.
An old binary does not enforce this new column: do not roll back to it while new
revocations exist without first stopping access and handling those credentials.

#### Create a service API key

In **Users**, enter a service name and choose **Create Service API key**. The
admin-only `POST /portal/admin/api/service-keys` accepts only
`{"name":"build-worker"}` under the existing verified Access administrator and
Origin/JSON/CSRF gate. Model grants and all other properties are rejected at
creation. A successful **201** returns `key_id`, `name`, `kind: "service"`,
`state: "active"`, `models: []`, zero `stats`, and the one-time plaintext `key`.
Only its SHA-256 hash is persisted; no hash is returned. Save the secret in a
password manager or client secret store before dismissing it. The browser uses
text-only rendering and clears the reveal on dismiss, tab navigation and
pagehide; it never puts the key in URLs, history or browser storage. Copying is
explicit and puts it on the system clipboard (dismiss does not erase clipboard).
The new row is appended without discarding other unsaved model selections.

Service names are trimmed, nonempty, at most 128 UTF-8 bytes, and contain no
control characters. Uniqueness uses trimmed Unicode lowercase (not Unicode
canonical normalization). Collisions with existing managed or configured key
names, including disabled keys, return **409 `service_name_conflict`** without
changing the existing key. Service-service duplicates are database-enforced.
There is no automatic retry: after an interrupted response, refresh before
trying again because creation may have committed. A lost secret cannot be
revealed; disable that key and create a differently named replacement.

Services have **no Google/Access employee identity**, employee self-service
binding or legacy adoption. They start with **deny-all models**; grant models
using the existing model editor, and use the existing Enable/Disable controls.
The next request sees policy/state changes. Traditional keys retain their
empty-means-all behavior. Service credentials share the employee **inference-only
route policy**: synchronous inference and model listing only, never master
operations, shared accounts/organization usage, or unscoped asynchronous jobs.

The additive, idempotent schema maintains `service_keys(key_id, normalized_name,
revision)`, plus `service_key_hashes` for cross-rotation reporting. Issuance
atomically inserts literal `[]`, the marker, initial hash history and a management
event. Existing keys are not reclassified; their current Service hashes are
backfilled, but previously lost hashes cannot be reconstructed.

**Service rotation** is available in Admin and `agent-model keys rotate <id>`.
It preserves the logical ID, name, classification, creation time, metadata and
history. Active rotation retains models; replacing a revoked credential clears
all grants. An independent disabled pause survives both. The old secret stops
authenticating immediately; the new secret is shown once and never added to
history or browser storage. There is no overlap/grace period for Service rotation,
and `--revoke-old` does not change this. If the response is lost, refresh and rotate
again; the old secret cannot be recovered. Portal keys still rotate through the
personal identity-bound page; legacy CLI rotation remains unchanged.

`POST /portal/admin/api/service-keys/rotate` takes `{key_id, revision}`;
`POST /v1/keys/{id}/rotate` takes `{revision}` and requires Master. Get the revision
from overview or key info. Service model edits also require this observed
`revision`. Stale credential revisions return 409: refresh, review, retry. The
SQLite writer transaction serializes rotation, revocation and model changes.
Unsupported out-of-band Service budget/expiry policies are rejected, not erased.
Reports, request drilldowns and key-info spend include tracked Service hashes,
including late logs from in-flight requests; only the current hash authenticates.
Employee attribution takes precedence on historical collisions as before.

**Management history** is read-only under each key in Admin. It records successful
create, disable, enable, revoke, rotate and model-edit operations, including
repeated successful commands, in `key_events`. Each event contains an ordered ID,
stable target key ID, actor reference, action and UTC timestamp; the UI displays
Taipei time. No token, credential hash, request body, model list, name, email or
assertion is copied into this history. Model edits are recorded as events, not a
before/after policy diff. Actor references for Portal operations are deterministic
opaque UUIDs derived from verified issuer/subject (not request-body fields).
They identify a principal without exposing login PII; Master/CLI operations say
`master / shared-master`, not an invented human caller. Direct Store maintenance
calls say `system / store`. History starts at upgrade; old events are not invented.

`GET /portal/admin/api/history?key_id=<id>&before=<event-id>` requires the existing
admin gate. It returns at most 100 newest events; omit `before` for the first page,
then use the last ID to fetch older events. Events survive rotation/replacement
and request-log retention. The mutation and audit insertion commit together:
a history-write failure rolls back the key change. This is an application audit
trail, not tamper-proof storage against a database administrator.

No new usage limits are implemented or exposed here. Model access, reversible disable and permanent
revocation are the service controls in this release. Existing USD-budget and
expiry options belong to the ordinary key CLI/API, not this service creation
flow. RPM/TPM settings are deployment-level, not per-key; per-key token caps and
concurrency limits are not supported.

`GET /portal/admin/api/monitoring` is a separate admin-only, metadata-only report.
The dashboard's monitoring section applies one filter set to complete SQL totals,
continuous time buckets, requested-model and user/key breakdowns, and paginated
request details. The key-management section deliberately remains its separately
labeled rolling-30-day overview; its text search hides rows only, not monitoring
results. Personal reporting remains identity-derived.

Parameters (single values; unknown/repeated/empty parameters rejected):

- `view`: `full` (default, backward-compatible combined report), `summary`
  (dashboard and options, no request metadata), `requests` (only matching count
  and metadata page), or `options` (only selectors). All modes validate the same
  filters. Overview uses `summary`; Requests uses `requests`, obtaining options
  separately on initial load/filter changes and reusing them during pagination.
- `since`, `until`: whole-second RFC3339 timestamps, inclusive/exclusive, converted
  to UTC. Default is rolling 30 days ending now; an explicit `until` without
  `since` selects its preceding 30 days. No future endpoints; maximum 366 days.
- `timezone`: `UTC` (default, preserving existing non-UI consumers) or
  `Asia/Taipei` (fixed UTC+8). Only these two reporting zones are supported.
  An explicit value is echoed in `filter.timezone`; omission retains the original
  response shape. This changes calendar grouping, not timestamp storage or bounds.
- `bucket`: `day` (default) or `hour` (maximum 31 days). Aligned to the reporting
  timezone and zero-filled; first/last buckets may be partial. Bucket `start`
  remains a UTC RFC3339 instant: Taipei January 1 midnight is December 31
  `16:00:00Z`. Zero means no matching retained records, not proof no usage occurred.
- `user`, `key_id`, `model` (requested alias), `provider`: exact-match selectors,
  at most 256 bytes each. Model/provider options come from all retained managed-key
  logs within the selected time range, independent of the page/other selections.
- `auth_mode`: `subscription` or `api_key`; `status`: `ok` or `error`.
- `page`: 1–1,000,000; `page_size`: 1–100 (default 25). Deterministic newest-first
  metadata order; totals are independent of pagination. The browser freezes the
  returned range between pages; a fresh HTTP request uses a fresh SQL snapshot.

Both Portal pages explicitly request `timezone=Asia/Taipei` for reporting. All
user-facing request times, ranges, key expiry, chart/day details and accessible
labels use Asia/Taipei (UTC+8), independent of the browser timezone. Admin
`datetime-local` inputs represent Taipei wall time and are converted to UTC
RFC3339 instants; browser URLs retain the server-resolved, frozen `since`/`until`
across Apply, drilldowns, pagination, refresh and Back. The reporting zone is
fixed by the page, not a user-selectable URL preference. The Users overview
remains a rolling duration, not 30 Taipei calendar dates. Database timestamps,
budget windows and key policy are unchanged; no timezone migration is needed.

Every response includes the resolved `filter` and
`scope: retained_managed_keys_and_current_master` when a nonempty master credential
is configured, otherwise the legacy `scope: retained_managed_keys`.
The credential-derived reporting scope is supplied internally by the server, not
accepted in HTTP parameters or returned in JSON.
`full` additionally returns `stats`, `p95_latency_ms`, `buckets`, `models`, `users`,
`requests` and selector `options`; `summary` returns the same except `requests`.
`requests` adds only `request_count` and `requests`; `options` adds only `options`.
Omitted metrics are not computed zero/null results. Selector `options` contains
`models` and `providers` across the time range, plus `users` (`id`, `name`) matching
all dimension filters, preserving the dashboard's user/key suggestions.

Each response uses one read transaction, keeping its count/rows (or dashboard
aggregates) consistent under concurrent ingestion and rotation. Page reads do not
materialize the dashboard scope or recompute options, aggregates, P95, buckets or
breakdowns. Separate summary/options/page responses do **not** share a snapshot:
frozen bounds prevent a moving window, but late logs, retention and ownership
changes can still change counts, choices and page membership between responses.
Browser selector choices are reused only for the current resolved range and
filters; reload or reapply filters to refresh them. Portal history includes late
records written under old rotation hashes and deleted key tombstones; legacy
keys retain current-hash attribution only. **Master key** includes all retained
records matching the **current configured master credential**, regardless of when
those records were written, plus ongoing traffic logged under that credential.
No log migration or timestamp changes are needed. The user/key breakdown and
selectors label it Master key; drilldowns use the exact reserved public identity
`system:master`, not a potentially reused service name. Requests are sorted newest
first (including time-of-day) to inspect latest use. The Users row is read-only and
links to those requests; it does not grant master management permissions.

This is shared-credential attribution, **not identification of the actual caller**.
After a future master rotation, only the newly configured credential is matched:
prior rotated secrets unavailable to the server cannot be identified as master.
Such records remain stored and are either attributed through existing managed-key
ownership or excluded, never classified as master merely because they are unowned.
An empty/disabled master configuration adds no identity and does not match an
empty credential's digest. Other config virtual keys and arbitrary unknown hashes
remain excluded. Reporting exposes no master secret or digest in JSON, URLs, UI
or diagnostic logs, and does not persist it as a managed/service key. Existing
request-log credential digests remain unchanged. Session/admin authentication,
bearer precedence and permissions, personal reporting, service/employee controls
and reporting-reader isolation are unchanged. This is **not an organization-wide
report** or a budget-enforcement input.

**SQLite reporting isolation (operations):** a disk-backed `OpenSQLite` store owns
one core read/write connection and one dedicated `mode=ro` reporting connection
(each max-open/max-idle = 1). Only monitoring (all views), the admin key overview,
and personal totals/yearly heatmap use the reader. Identity binding, authentication,
policy/key mutation, budget checks and `LogRequest` remain on core. Non-Portal
`UsageReport`/`UsageHealthReport`, log lookup/listing, key management reads and
health checks also remain on core; the standalone read-only CLI constructor keeps
its existing single handle. No policy cache or budget-accounting semantics change.

Each Portal report has a **15-second context deadline**, including time waiting
for the sole reader and SQL execution; an earlier caller deadline/cancellation
wins. This is per store call, not an HTTP/SSE timeout or an admission queue size
limit. It bounds cooperative database work, not a hard OS scheduling/cleanup
wall-clock guarantee. Errors use the existing Portal store-error response (503).
The server intentionally has no whole-request timeout for streams. Fifteen seconds
leaves headroom over local ~3.2-second 1M-row summaries without allowing an
unbounded WAL snapshot; it is not a production latency/capacity guarantee.

Only core runs migrations and enables WAL; opening a disk store fails if WAL or
its reporting reader is unavailable. The reader cannot mutate persistent main
DB tables but can create writable TEMP tables; report rollback removes those
scopes, including on cancellation (`query_only` is deliberately not used).
Filesystem paths and local `file:` URIs with query parameters are accepted;
URI paths are decoded before securing the actual DB/WAL/SHM as 0600. Writable
opens accept `mode=rw`/`rwc`, not `ro`, `immutable`, `nolock` or custom `vfs`.
`:memory:`, empty paths (private ephemeral memory), and `mode=memory`/memory URI
DSNs explicitly reuse core for reporting: they do **not** gain read-only or WAL
isolation and never open an unrelated second memory database. Use a disk DB for
production isolation. Closing the store closes both owned handles.

Reports still compete for CPU, memory and disk I/O. Long snapshots can defer WAL
checkpoints and grow the WAL; monitor report errors/duration, disk space and WAL
growth under sustained load. This change does **not** address the independent
synchronous content-log gzip/mutex stall, nor establish a 100-session production
SLO on a resource-constrained VM.

Metrics include requests, errors, prompt/completion/total tokens, cache read/write,
reported reasoning sum and known-record count, mean latency and nearest-rank p95.
Cache tokens are already within prompt tokens; reasoning is never added to total.
Cache zero cannot distinguish unreported from zero; nullable reasoning distinguishes
unknown from reported zero. Request rows exclude hashes, correlation IDs, key
metadata and error bodies/types, and contain no raw prompt/completion/headers.

Cost uses recorded classification, not recalculation from today's registry:
response cache first, then subscription per-request accounting $0, API `priced`
computed USD, explicit `unpriced`, and legacy/unknown separately. Subscription
accounting $0 is **not actual free service**; the flat fee is out-of-band. Missing
prices are not zero-priced usage. Response-cache token counts represent replayed
usage, not new upstream consumption. API computed USD includes both successful
and failed logged requests in the selected set; it is not an invoice. Equivalent
API cost is unavailable. No TTFT/concurrency instruments or budgets are added.

**Required existing-deployment cutover (operator action, not automatic):** reset
**every portal-issued key** to `[]`, rather than silently restoring historical
snapshots superseded by shared-policy edits. Stop the service and keep traffic
closed through verification. Back up the current binary, YAML and SQLite using
SQLite's `.backup` (captures committed WAL content), with owner-only permissions.
For example, as the service operator, replace the database path with the actual
configured path:

```sh
sudo systemctl stop agent-model
umask 077
DB=/absolute/path/to/agentmodel.db
BACKUP="${DB}.before-per-person.$(date -u +%Y%m%dT%H%M%SZ)"
sqlite3 "$DB" ".backup '$BACKUP'"
chmod 600 "$BACKUP"
```

After the backup succeeds, run this one-time SQL against that same database
before starting the new build:

```sql
BEGIN IMMEDIATE;
UPDATE api_keys SET models = '[]'
WHERE EXISTS (SELECT 1 FROM portal_identities p WHERE p.key_id = api_keys.id);
SELECT changes() AS reset_portal_keys;
COMMIT;
SELECT k.id, k.models FROM api_keys k
JOIN portal_identities p ON p.key_id = k.id;
```

Only identity-bound keys are reset. Legacy CLI/config keys, names, hashes,
revocation, identity revisions, history and usage remain untouched. Do not rerun
this reset after administrators have started granting individual access.
Remove obsolete `portal.models` if present; retain/configure `portal.admin_emails`.
Install the new build and restart. Verify existing portal keys list no models and
receive 403 for inference, old CLI keys retain their behavior, then authorize
people individually in `/portal/admin/`. Fresh installations need no policy seed:
employees can create empty keys immediately. No schema change is required.

**Rollback warning:** old builds can treat empty per-key scopes as unrestricted
or consult the historical shared policy. Keep traffic closed when rolling back;
restore a reviewed matching binary/config/database backup and verify authorization
before reopening. This also discards post-backup changes; do not assume the new
per-person restrictions survive rollback.

The same `/portal/api/me` response includes `stats` with `request_count`,
`prompt_tokens` (input), `completion_tokens` (output) and `total_tokens` for the
past rolling 30 × 24 hours, inclusive of the cutoff and current second.
`/portal/api/me` accepts optional `timezone=UTC` (default) or
`timezone=Asia/Taipei` (fixed UTC+8); empty, repeated, unsupported or malformed
values return 400 (`invalid_reporting_timezone`). Other identity/key selectors
remain ignored. This also validates the timezone on `fields=session` requests,
which still skip usage reporting.
`calendar` contains the current `year` in the reporting timezone, its `timezone`
name, and a `days` series from January 1 through December 31 (including leap day
when applicable). The personal page explicitly requests Asia/Taipei: the year
changes at December 31 `16:00:00Z`, and daily aggregation changes at `16:00:00Z`,
not at UTC midnight. The default API calendar remains UTC for other consumers.
Each day has `date` (`YYYY-MM-DD`), `total_tokens` and `requests`. Past/current
dates in the reporting timezone are zero-filled; future dates have null counts
and appear blank/disabled, not as zero usage. Today's bucket includes only records through the current
second. The calendar and rolling summary intentionally cover different windows;
in early January the summary can include the previous December.

Both views aggregate raw retained request-log counts, including logged errors,
without adding cache columns or recomputing `total_tokens` from input/output.
Ownership is selected solely by the verified issuer/subject binding, never email
or browser-supplied key selectors. A portal-only hash history is written in the
create/rotate transaction and backfilled idempotently with existing bound current
hashes on schema migration. Historical hashes are **reporting only** and never
authenticate. Two or more rotations and late writes under old tracked hashes
remain attributable; no logs are rewritten and there is no cost/quota feature.
Unissued users have zero totals. Retention can truncate either window: zero/no
records is not proof of no usage, and hashes lost before tracking cannot be
reconstructed. The dependency-free calendar uses fixed daily-token color bins
(0, 1–999, 1,000–9,999, 10,000–99,999, 100,000+), Taipei dates, accessible day labels
and a refresh button for both views.

Email-named existing config or managed keys block creation with
`migration_required`; disabled keys also block. There is no implicit adoption.
There is no binding endpoint, migration tool or role framework. Existing
employees keep using their existing key and contact the operator for changes;
the portal never rewrites YAML or adopts their token.

Portal-issued keys cannot access shared `/v1/usage`, `/v1/limits`, provider
account usage, key/OAuth management or unscoped asynchronous resources. Their
allowed surfaces are model listing, chat, responses, messages, embeddings and
image generation. They neither read nor seed the shared response cache, avoiding
cross-policy fallback reuse. Model allowlists and budget checks remain enforced
on inference. Existing non-portal bearer keys retain their behavior.

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
| `POST` | `/v1/responses` | OpenAI Responses API for routed ChatGPT subscription models, including hosted tools; supports streaming and buffered responses, defaults omitted `store` to `false`, omits the subscription backend's unsupported `max_output_tokens`, rewrites `model`, and fills an empty terminal `response.output` from preceding `response.output_item.done` events; does not retry after an upstream response has opened |
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
| `GET` | `/v1/account/openai/usage` | Upstream OpenAI/Codex subscription usage (see below) |
| `POST` | `/v1/oauth/chatgpt/start` | Start ChatGPT device-code OAuth **(master only)** |
| `POST` | `/v1/oauth/chatgpt/poll` | Poll ChatGPT device-code OAuth **(master only)** |
| `POST` | `/v1/keys` | Mint a virtual API key; returns the token once **(master only)** |
| `GET` | `/v1/keys` | List virtual API keys (never returns plaintext) **(master only)** |
| `GET` | `/v1/keys/{id}` | A key's config plus successful spend retained in `request_logs` (lifetime only when no rows were purged) **(master only)** |
| `POST` | `/v1/keys/{id}/rotate` | Replace Service secret with stable ID; requires revision **(master only)** |
| `POST` | `/v1/keys/{id}/revoke` | Permanently revoke a key, retaining history **(master only)** |
| `POST` | `/v1/keys/{id}/disable` | Reversibly pause a non-revoked key **(master only)** |
| `POST` | `/v1/keys/{id}/enable` | Enable a paused, non-revoked key **(master only)** |
| `DELETE` | `/v1/keys/{id}` | Permanently revoke a key (idempotent; history retained) **(master only)** |
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
ledger metering is wired here; budget caps are *enforced* on the
spending endpoints regardless (`400 budget_exceeded` pre-call).

```jsonc
{
  "agent": "agentmodel",
  "status": "ok",
  "windows": [
    {
      "limit_name": "org/default:budget",
      "window": "720h", "unit": "usd",
      "limit": 100.0,  // remaining/used omitted until live metering
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

### `GET /v1/account/openai/usage`

The sibling of the Anthropic endpoint, for the **OpenAI/Codex subscription**,
served here for the same reason: this process owns the ChatGPT OAuth credential
(`agent-model chatgpt-login`), so it is the only one that uses or rotates it.

It reads `GET {ChatGPTAPIBase}/usage` — the backend the Codex CLI itself talks
to — and normalizes it to the same `BackendLimits` shape. Windows are named
`<limit-id>_<duration>`: `codex_weekly` for the account-wide limit, and
`<metered_feature>_<duration>` for each per-model-family limit, e.g.
`codex_bengalfox_weekly`. Each carries `used_percent`, optional `reset_at`, and
`source: "codex_oauth"`. `auth.plan` reports the subscription tier.

**A window is named from its own `limit_window_seconds`, never from the slot it
arrives in.** OpenAI removed the 5-hour window on 2026-07-12 and the weekly
window took over the `primary_window` slot, so slot position is not a stable
signal of duration. A window carrying no usable duration is dropped rather than
given a name that could collide with a real one, since two rows sharing a
`limit_name` would print identical labels over contradicting numbers.

A missing credential or an upstream failure returns HTTP 200 with a structured
`error` (`codex_oauth_login_required` / `codex_oauth_usage_unavailable` /
`codex_oauth_no_limits`), matching the Anthropic endpoint's contract.

The read is non-consuming: it never POSTs `/responses`, so reporting quota
cannot spend it.

## Cost tracking

Token-priced requests compute USD cost from provider-returned usage and the
embedded price registry (`services/agentmodel/cost/model_prices.json`,
keyed first as `<provider>/<resolved-model>` and then by bare resolved model
id, e.g. `openai/gpt-4o`). Replicate prediction creates instead compute
supported unit-based costs from the request body and registry; video-generation
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

### Reasoning-token collection

Audit rows retain upstream-reported reasoning counts in nullable
`request_logs.reasoning_tokens` for later analysis. Missing or JSON `null`
means **unknown**, not zero; an explicit `0` is retained. Opening the SQLite
store automatically adds `reasoning_tokens INTEGER NULL` to older databases;
the migration is idempotent and historical rows remain NULL. No historical
backfill, token estimation, reasoning-text inspection, or new raw-payload
storage is performed.

Collection covers non-streaming and streaming usage (often only the final
usage event), including the `/v1/messages` bridge and native `/v1/responses`
audit path:

| Upstream | Reported field |
|---|---|
| OpenAI chat completions, Azure, DeepSeek, other OpenAI-compatible adapters | `completion_tokens_details.reasoning_tokens`, **only if supplied** |
| ChatGPT subscription Responses (including chat-completion translation) | `output_tokens_details.reasoning_tokens` |
| Anthropic | `output_tokens_details.thinking_tokens`, **only if supplied** |
| Gemini | `usageMetadata.thoughtsTokenCount` |

OpenAI-shaped gateway responses expose known counts as
`usage.completion_tokens_details.reasoning_tokens`; the Go client and response
cache preserve this field on JSON round trips. A missing count is omitted.
Replicate and adapters/responses without a numeric detail remain unknown;
support for reasoning text does not imply support for a reasoning-token count.
These mappings do not guarantee any particular model or subscription reports it.

Reasoning is **detail, never an extra addend** to `total_tokens`. OpenAI /
Responses completion/output counts already include reasoning; adding it again
would double-count. Existing provider conventions stay unchanged: in particular,
Gemini completion counts remain `candidatesTokenCount`, with its separately
reported `totalTokenCount` retained rather than reconstructed. Prompt totals
continue to include cache-read/write tokens, with those details stored separately.
This collector does not change cost, budgets, aggregate reports, or portal UI.

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
`stop`, `tools`, `tool_choice`, `thinking`, `reasoning_effort`). `stream`,
`user`, and `prompt_cache_key` are excluded from the key; streaming requests
bypass the cache, `user` is treated as a tracking tag rather than an output
determinant (matching LiteLLM's default key), and `prompt_cache_key` is upstream
routing/prefix-recurrence metadata rather than an output determinant. ChatGPT subscription deployments also forward
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
yields a provider error. By contrast, Replicate creates preserve raw JSON
non-2xx upstream response bodies; Messages preserves non-`429` bodies, while
its router consumes raw `429` responses for cooldown/fallback rather than
logging them. A Messages stream is logged only when a `message_start` event
made an assembled response possible.

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
  The file is `0600` — enforced on every open, not only at creation, so a log
  pre-created by `touch` or by an external rotator's `create 0644` is tightened
  rather than left readable. If the mode cannot be set (the file is owned by
  another user, say), startup fails instead of writing unredacted content to a
  file whose readers we cannot bound. The parent directory is created if it is
  absent and forced to `0700` — including one you created yourself, since a
  directory left `0755` is what makes the file's mode moot. Give the content log
  its own directory rather than a shared one: pointing it at `/var/log` would
  narrow that directory for everything else living there. Treat the whole
  directory as sensitive and put it on encrypted storage if your threat model
  needs it.
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
is a `0600` JSON Lines file with no redaction, and is **not** covered by the
`retention` purge above; configure its separate rotation/retention policy. See
[Content logging](#content-logging) for the full description.

Every file the gateway writes that can hold prompt or completion text is `0600`,
and the mode is applied before the file is first written rather than after, so
it never exists world-readable even briefly: the content log, the SQLite
response cache (`cache.sqlite_path`) and its `-wal`, and the audit database
(`database.path`). A file left `0644` by an older version is tightened the next
time the gateway opens it, and an open that cannot reach `0600` fails rather
than proceeding. Two consequences worth knowing: the parent directories are
yours to create — nothing here widens or narrows an existing one — and after the
first restart on this version, `agent-model usage --db` run as a different user
than the gateway will get a permission error where it previously worked.

See `cmd/agent-model/example_config.yaml` and `services/agentmodel/README.md`
for a fuller configuration walkthrough.
