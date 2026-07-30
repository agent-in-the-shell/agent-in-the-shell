# ChatGPT Subscription Provider — Risk Notice

This provider talks to `https://chatgpt.com/backend-api/codex/responses`, the
endpoint that OpenAI's [Codex CLI](https://github.com/openai/codex) uses to
serve ChatGPT Plus / Pro subscribers. It is **not** a documented public API.
Image generation (`provider.ImageGenerator`, via the Responses backend's
built-in `image_generation` tool) talks to the same endpoint and carries the
same risk.

We work today by mimicking the Codex CLI client identity exactly:

- `User-Agent: codex_cli_rs/0.0.0 (Unknown 0; unknown) unknown`
- `Originator: codex_cli_rs`
- `OpenAI-Beta: responses=v1`
- OAuth Bearer token obtained via the Codex device-code flow

## What can go wrong

1. **OpenAI changes the endpoint.** They have no obligation to keep wire
   stability for non-public APIs. A path change, a payload-shape change, or a
   new required header can break this provider overnight.
2. **OpenAI adds anti-spoofing.** Today we're indistinguishable from Codex CLI
   on the wire. OpenAI could add client attestation (signed binaries, request
   signatures, etc.) that a third-party Go client cannot satisfy.
3. **Terms of Service.** Using ChatGPT subscription quota from a
   non-OpenAI-published client may be construed as a ToS violation. Account
   suspension is possible. End users — not the gateway operator — bear this
   risk; communicate it before they sign in.
4. **Per-user quota.** Subscription tiers have rate limits enforced server
   side. We do not expose remaining quota; a 429 just means "try later or fall
   back".

## Mitigations

- **Pin the Codex CLI version.** Track upstream
  [`openai/codex`](https://github.com/openai/codex) and update the
  `User-Agent` / `Originator` constants only when the CLI itself changes them.
- **Watch upstream LiteLLM.** The
  [`litellm/llms/chatgpt`](https://github.com/BerriAI/litellm/tree/main/litellm/llms/chatgpt)
  integration is our reference; their breakages signal ours.
- **Always wire a fallback.** Configure the router so requests to
  `chatgpt/...` fall back to `openai/...` (API-key-mode) on persistent 4xx /
  5xx. A subscription outage should degrade to paid-API rather than fail.
- **Surface auth state.** Map authentication failures to `ErrLoginRequired`
  so the caller can re-trigger the device-code flow rather than crash.
- **Don't market this as production-grade.** Document in the deployment guide
  that subscription mode is "best effort, may break".

## Triggers for revisiting

- Codex CLI bumps its `Originator` or `User-Agent`.
- OpenAI publishes an official third-party Responses API endpoint.
- We see a sustained spike in 401 / 403 / 426 from this provider — that's
  likely an anti-spoofing rollout.

If any of those happen, treat removing this provider as a real option, not a
last resort.
