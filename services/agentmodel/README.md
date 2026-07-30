# AgentModel

Go-native LLM gateway. **OpenAI-compatible API in front, multi-provider routing behind.**
Inspired by [LiteLLM](https://github.com/BerriAI/litellm) but rewritten in Go to fit the rest of the Agent in the Shell family.

## Supported providers

| Provider           | Auth mode    | Notes |
|--------------------|--------------|-------|
| OpenAI             | API key      | Standard `api.openai.com/v1` |
| Azure OpenAI       | API key      | Deployment-scoped `…/openai/deployments/<deployment>/chat/completions?api-version=…` (`api-key` header). Set `base_url` + `api_version`; `deployment_name` defaults to `model` |
| Anthropic          | API key      | `api.anthropic.com/v1/messages` (`x-api-key`) |
| Anthropic          | subscription | Claude Pro/Max via `sk-ant-oat*` OAuth bearer (no flow needed; client provides token) |
| Gemini             | API key      | Google AI Studio `generativelanguage.googleapis.com/v1beta` |
| ChatGPT (Codex)    | subscription | ChatGPT Plus/Pro via Codex CLI's device-code OAuth flow |
| DeepSeek           | API key      | OpenAI-compatible `api.deepseek.com/v1` (Bearer). No embeddings |
| OpenAI-compatible  | API key      | **Groq · Mistral · Together · xAI (Grok) · OpenRouter · Fireworks · Perplexity · DeepInfra · Nebius** — just `provider: <name>` (a default endpoint ships for each, Bearer auth). Any other OpenAI-wire vendor works too via `base_url` |
| Chinese / OSS      | API key      | **Zhipu (GLM-4) · Moonshot (Kimi) · Qwen (DashScope) · Yi (01.AI) · Baichuan · StepFun · SiliconFlow** — same first-class `provider: <name>` (OpenAI-compatible, Bearer). Open-source models are also served by the aggregators above (Together/Fireworks/Groq/OpenRouter/DeepInfra/SiliconFlow) and by local Ollama/vLLM (keyless via `base_url`) |

Subscription requests have `cost_usd: 0` (covered by the user's flat fee); token counts are still recorded for observability.

Azure OpenAI is wire-compatible with OpenAI (chat, streaming, embeddings); only the URL layout and auth header differ. Configure a deployment with `provider: azure`:

```yaml
model_list:
  - model_name: "gpt-4o"
    deployments:
      - provider: azure
        model: "gpt-4o"
        api_key_env: "AZURE_OPENAI_KEY"
        base_url: "https://<resource>.openai.azure.com"   # resource host (required)
        api_version: "2024-10-01-preview"                  # required
        deployment_name: "gpt4o-deploy"                    # optional; defaults to `model`
        weight: 100
```

Cost tracking is keyed by `model`, so pricing resolves exactly as it does for the same model on OpenAI.

## Quick start

### API-key only

```bash
cp cmd/agent-model/example_config.yaml config.yaml
export AGENT_MODEL_TOKEN=$(openssl rand -hex 16)
export OPENAI_API_KEY=sk-...

go run ./cmd/agent-model migrate --config config.yaml
go run ./cmd/agent-model serve --config config.yaml &

curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $AGENT_MODEL_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}'
```

### Claude Pro/Max subscription

Two wiring modes — pick one per deployment:

**Static token** (~12h lifetime, manual re-export when expired):

```bash
export ANTHROPIC_OAUTH_TOKEN=sk-ant-oat-...
# config:  api_key_env: "ANTHROPIC_OAUTH_TOKEN"
go run ./cmd/agent-model serve --config config.yaml
```

**Refreshable** (auto-refreshes against console.anthropic.com; persists rotated tokens):

```bash
# Share auth state with pi-mono so login lives in one place.
ln -sfn ~/.pi/agent ~/.config/agentmodel/anthropic

# config:  oauth_token_dir: ""    (uses default above)
#          OR  oauth_token_dir: "~/.pi/agent"   (no symlink needed)
go run ./cmd/agent-model serve --config config.yaml
```

Either way, calls work the same:

```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $AGENT_MODEL_TOKEN" \
  -d '{"model":"claude","messages":[{"role":"user","content":"hi"}]}'
# Response includes "auth_mode":"subscription", "cost_usd":0
```

Initial login: run `agent-model anthropic-login` (Authorization Code + PKCE
against claude.ai), or import an `sk-ant-oat*` access + `sk-ant-ort*` refresh
pair minted by another Claude Code OAuth client (e.g. `pi /login` or Claude
Desktop). After that agent-model refreshes the tokens itself.

#### Anthropic SDK clients (pi-ai, pi-mom, anthropic-sdk)

Clients that speak the **Anthropic Messages** wire format directly (instead
of OpenAI-shaped chat completions) can use `/v1/messages` as a drop-in
replacement for `https://api.anthropic.com`. The endpoint does a near-
byte-for-byte passthrough — the only field rewritten is `model`, which is
mapped from the caller's logical name to the deployment's upstream id.
Streaming SSE is forwarded unchanged so existing Anthropic-SDK retry /
event-decoding logic works as-is.

```bash
curl -X POST http://localhost:8080/v1/messages \
  -H "Authorization: Bearer $AGENT_MODEL_TOKEN" \
  -H "anthropic-version: 2023-06-01" \
  -d '{"model":"claude","max_tokens":256,"messages":[{"role":"user","content":"hi"}]}'
```

For pi-mom specifically:

1. Add a `claude-sonnet-4-5` model_list entry in your agent-model config
   pointing at an `anthropic-oauth` deployment with
   `oauth_token_dir: "~/.pi/agent"` (so both tools share the same token
   file).
2. Point pi-ai's hardcoded `baseUrl` for `claude-sonnet-4-5` at
   agent-model — Anthropic SDK appends `/v1/messages` automatically:

   ```js
   // in node_modules/.../@mariozechner/pi-ai/dist/models.generated.js
   "claude-sonnet-4-5": {
     ...
     baseUrl: "http://localhost:8080",  // was "https://api.anthropic.com"
   }
   ```
3. Run pi-mom with the agent-model bearer token in `ANTHROPIC_API_KEY` (or
   patch the auth lookup in `pi-mom/dist/agent.js` if you want a separate
   env var).

agent-model handles the upstream OAuth refresh against
console.anthropic.com and records every pi-mom call in its audit log with
`auth_mode=subscription`, `cost_usd=0`.

### ChatGPT Plus/Pro subscription

Run the device-code OAuth flow once:

```bash
go run ./cmd/agent-model chatgpt-login
# 🔑 Visit https://auth.openai.com/codex/device and enter code: ABCD-EFGH
# (token saved to ~/.config/agentmodel/chatgpt/auth.json)
```

Then use the `chatgpt-pro` model:

```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $AGENT_MODEL_TOKEN" \
  -d '{"model":"chatgpt-pro","messages":[{"role":"user","content":"hi"}]}'
```

⚠️ **ChatGPT subscription support is reverse-engineered** (mimics Codex CLI). OpenAI does not formally
support third-party clients on this endpoint. See `provider/chatgpt/RISK.md` before relying on it
for production.

## Endpoints

| Method | Path                              | Description |
|--------|-----------------------------------|-------------|
| POST   | `/v1/chat/completions`            | OpenAI-shaped chat completion (streaming + non-streaming) |
| POST   | `/v1/messages`                    | Anthropic-shaped passthrough (Claude SDK / pi-ai compatible) |
| POST   | `/v1/embeddings`                  | OpenAI-shaped embeddings |
| GET    | `/v1/models`                      | Lists configured models |
| GET    | `/healthz`                        | Liveness (unauthenticated) |
| GET    | `/readyz`                         | Readiness (unauthenticated) |
| POST   | `/v1/oauth/chatgpt/start`         | Start device-code OAuth flow for ChatGPT subscription |
| POST   | `/v1/oauth/chatgpt/poll`          | Poll device-code completion |

All `/v1/*` endpoints require `Authorization: Bearer $AGENT_MODEL_TOKEN`.

## Architecture

```
┌─────────────┐
│   client    │  (any OpenAI-compatible SDK)
└──────┬──────┘
       │ /v1/chat/completions
       ▼
┌──────────────────────────────────────────┐
│  api.Server                              │
│  ─ chi router + bearer auth              │
│  ─ chat / embed / models / health / oauth│
└──────┬───────────────────────────────────┘
       │
       ▼
┌──────────────────────────────────────────┐
│  router.Router                           │
│  ─ weighted shuffle across deployments   │
│  ─ retryable-error fallback chain        │
└──────┬───────────────────────────────────┘
       │
       ▼
┌──────────────────────────────────────────┐
│  provider.Provider                       │
│  (openai, anthropic, gemini, chatgpt)    │
│  ─ uses auth.Authenticator (API key      │
│    or OAuth) configured at deployment    │
└──────────────────────────────────────────┘
       │                                  │
       ▼                                  ▼
   external LLM API              cost.Calculator + store.Store
                                 (per-request cost + audit log)
```

Subscription cost model mirrors LiteLLM: subscription model entries in
`cost/model_prices.json` omit `*_cost_per_token` so `cost.Calculate` returns
`0` for them. Token counts are still attributed in audit logs.

### Refreshing the price catalog

`cost/model_prices.json` is the embedded price/capability catalog (the offline
fallback used for cost calculation). Per-token prices are not served by any
provider API, so the catalog is sourced from LiteLLM's
`model_prices_and_context_window.json` and refreshed on demand:

```sh
# (maintainers, from the source monorepo)
go run ./services/agentmodel/cost/cmd/syncprices -providers openai,anthropic,gemini
```

The sync is a **union with LiteLLM precedence**: LiteLLM refreshes/adds priced
API-key entries (re-keyed to `<provider>/<model>`), while hand-maintained
entries LiteLLM does not carry — subscription/OAuth models (`chatgpt/*`,
`anthropic-oauth/*`) and `-latest` aliases — are preserved. Review the diff
before committing. Note: `/v1/models` lists the **configured deployments**
(`model_list`), not this catalog — the catalog is consulted only for pricing.

### Deployment validation (drift check)

At startup the server runs a **best-effort** check, in the background, that
each configured deployment's upstream model id is still offered by its
provider. Providers that implement live model listing (OpenAI, Anthropic,
Gemini — via their `/models` endpoints) are queried once each; a deployment
whose model id the provider no longer offers is logged as a warning:

```
WARN configured model not offered by its provider model_name=... provider=openai model=...
```

It never blocks or fails startup: providers that can't list models, or whose
fetch fails (offline, auth, timeout), are silently skipped. The upstream lists
are a validation aid, not a source of truth.

**Periodic re-validation** is opt-in via `revalidate_interval` (a Go duration,
e.g. `1h`). When set, the check re-runs on that interval and logs only drift
*transitions* — a model newly missing (`WARN`) or a previously-drifted model
that reappeared (`INFO configured model drift resolved`) — so steady-state
drift isn't re-logged every pass. A TTL'd cache of each provider's live model
list (window == the interval, keyed by provider name + auth_mode so api-key and
subscription credentials don't collide) backs both the startup and periodic
passes; a failed refresh degrades to the last-good list rather than flapping
alerts. Omit `revalidate_interval` (the default) to keep the one-shot
startup-only check.

**Model discovery** is opt-in via `GET /v1/models?available`: in addition to
the routable configured models (tagged `"configured": true`), the response
lists upstream-offered models that have no configured deployment (tagged
`"configured": false`). The default `GET /v1/models` is unchanged and never
includes unconfigured (unroutable) models, so every id in the default list is
always callable.

## Testing

`go test ./...` covers everything offline: unit tests per package, in-process
gateway tests with stub providers (`conformance/integration_test.go`), and
`TestConformance_Stub`, which drives the **real** provider adapters against
native-dialect fake upstreams.

### Live provider tests

`services/agentmodel/conformance/live_test.go` runs the real adapters against
real upstreams. It covers what fakes structurally cannot: credentials that must
refresh, opaque values only the provider can mint (thinking-block signatures),
and prices keyed to the model ids upstream actually serves.

```bash
AGENTMODEL_LIVE=1 go test ./services/agentmodel/conformance/ -run 'TestLive|TestConformance_Live' -count=1 -v -timeout 300s
```

Opt-in in two layers, so nothing spends by accident:

1. **Master switch** — `AGENTMODEL_LIVE=1` is consent to spend. Without it every
   live test skips, so an exported `OPENAI_API_KEY` never causes spend during a
   normal `go test ./...`.
2. **Per-target credentials** — each subtest skips unless *its own* credential is
   present: an API key env var, or a subscription profile directory containing
   `auth.json`. One provider's absence never fails another's test.

Total cost is under a cent; subscription paths spend quota, not dollars.

| Test | Covers | Needs |
| --- | --- | --- |
| `TestLive_AnthropicThinking` | extended thinking survives streaming, non-streaming, and the content log with signatures intact | `ANTHROPIC_API_KEY` or an Anthropic OAuth profile |
| `TestLive_SubscriptionAuth` | OAuth profile loads, authenticates, and meters as `subscription` at $0 | Anthropic and/or ChatGPT OAuth profile |
| `TestLive_APIKeyMetering` | real model ids resolve to a catalog price (`cost_source: priced`) | `OPENAI_API_KEY` |
| `TestConformance_Live` | the multi-turn tool transcript against real providers | `OPENAI_API_KEY` and/or `ANTHROPIC_API_KEY` |

Model ids are consts in `live_test.go` — retargeting a retired model is a
one-line edit. OAuth profiles resolve exactly as the gateway resolves them
(profile sidecar, `AGENTMODEL_PROFILE`, provider token-dir root) and can be
pointed elsewhere with `AGENTMODEL_LIVE_ANTHROPIC_OAUTH_DIR` /
`AGENTMODEL_LIVE_CHATGPT_OAUTH_DIR`. A profile whose token sits in the nested
Claude-CLI layout the request path cannot read skips rather than failing.

A `401` from a live test means the credential was present but rejected. That
fails rather than skips on purpose: it is equally consistent with a stale key
(rotate it, or unset it to skip) and with the gateway sending the credential
wrong, and skipping would hide the second.

## Configuration reference

See [`cmd/agent-model/example_config.yaml`](../../cmd/agent-model/example_config.yaml) for a full annotated example.

Key concepts:

- **`model_list`** — logical model names clients use, each backed by one or
  more **deployments** (provider + model + auth_mode + weight).
- **`fallbacks`** — when all deployments of a logical model fail with
  retryable errors, the router tries each fallback model_name in order.
- **`auth_mode`** — `api_key` (default) or `subscription`. Subscription
  is supported by `anthropic`, `anthropic-oauth`, and `chatgpt` only.
- **`content_log.path`** — opt-in (off by default). When set, the full request
  and response of every `/v1/chat/completions` and `/v1/messages` call is
  appended to a `0600` JSON Lines file (`{timestamp, request_id, request,
  response}`), streaming responses reassembled into the final message. The audit
  DB and telemetry remain metadata-only; this is the only sink holding raw
  prompts/completions, with no redaction. Size/age-based rotation with gzip
  and count/age/total-bytes pruning is configurable (see `content_log` keys);
  there is no age-based deletion by default. See
  [content logging](../../docs/agentmodel.md#content-logging).

## Out of scope (wave 2+)

- Bedrock / Vertex AI / Cohere native adapters (Mistral and Groq already work
  first-class via the OpenAI-compatible provider table above)
- Redis cache backend / semantic caching via Qdrant (an in-process LRU +
  SQLite response cache ships today)
- Guardrails / PII redaction
- AgentPay integration (per-request wallet debit)
- AgentVault integration (per-customer credentials)
- Postgres backend
- 5-level budget hierarchy (key/team/org/user/end-user)
- Audio / batch / fine-tuning / realtime APIs
- Durable gateway-hosted storage of generated images/videos (upstream URLs/bytes are returned as-is; for Veo the returned URL needs the gateway's Gemini credential to fetch)
- Admin dashboard UI

See `docs/superpowers/plans/2026-05-09-agent-model-mvp.md` Wave 2 follow-ups for the priority list.

## References

- Implementation plan: `docs/superpowers/plans/2026-05-09-agent-model-mvp.md`
- Research: `LITELLM_RESEARCH.md`
- Security context (why we're rolling our own): `LITELLM_SUPPLY_CHAIN_ATTACK.md`
- ChatGPT subscription risk: `services/agentmodel/provider/chatgpt/RISK.md`
