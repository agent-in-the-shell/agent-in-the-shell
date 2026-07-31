# Marketing cards (RedNote-style, hand-built)

Hand-drawn "knowledge card" marketing images for the public release (release plan §5),
in the 小红书 / RedNote carousel style (see the target reference in
`../infographic-references/rednote/00-TARGET-handdrawn-knowledge-card.jpg`).

**Internal drafts.** `docs/design/**` is deny-listed in `release.yaml`, so nothing here
exports to the public repo yet. When a card is approved for a release, move/allowlist it
deliberately.

## Approach

Hand-built **HTML/CSS → PNG** (not an image model): the strength of this style is its
pixel-perfect bilingual text, which image models garble. Each card is one HTML file,
rendered to a 2160×2160 PNG (1080 logical @2x) with headless Chrome. Copy/colors are just
text edits — and this doubles as the reusable **template for every service's card**.

## Render

```bash
./render.sh agent-model-card.html            # -> agent-model-card.png
```

Needs Google Chrome (path overridable via `CHROME=...`).

### Carousel

`agent-model/` is a 7-page carousel (`page-1..7.html` sharing `style.css`):
cover → **subscription auth** → pain → how-it-works → routing/fallback/cost →
**multi-modal (image/video/embeddings)** → quickstart. Re-render the whole set with
`agent-model/render-all.sh`. `agent-model-card.html` (parent dir) is the condensed
single-card version.

`agent-model-en/` is the **English** edition of the same 7-page carousel (its own
`style.css` is Latin-first: Patrick Hand marker headlines + clean sans body).

`agent-model-broad/` is the **broad / 白話 zh** edition (6 pages) — a different **TA**:
the general 小红书 scroller, not developers. Jargon is translated to plain language
(endpoint → 「一個地方」, fallback → 「壞了自動換」, binary → 「執行檔」, YAML → 「設定」),
benefit-led, lower density. Same design system + fonts.

Editions to keep in sync when copy/claims change: `agent-model/` (dev zh),
`agent-model-en/` (dev en), `agent-model-broad/` (broad 白話 zh).

## Design system (keep consistent across services)

- **1080×1080**, cream background `#fbf9f1`, faint dot grid, carousel index top-right.
- Fonts (bundled, SIL OFL — see `fonts/`): **ZCOOL KuaiLe** (CJK marker) + **Patrick Hand**
  (Latin marker). Embedded via local `@font-face` so rendering is deterministic/offline.
- Two accents: **green** `#1f8a43`/`#146b32` (structure, card outlines, headlines) and
  **red-orange** `#e5432a` (the two hooks — sub-hook under the title, takeaway at the bottom).
- Body = **3 rounded cards**, light-green fill + green outline, hand-drawn wobble via the
  `#rough` SVG `feTurbulence`/`feDisplacementMap` filter; each card: doodle SVG icon + green
  headline + gray body.
- Hand-drawn underlines (SVG paths) under the title and the takeaway; `≥ ≤` marks flank
  title/takeaway; `✦` marks flank the sub-hook.

To make a new service card: copy `agent-model-card.html`, swap the title/subtitle/hook, the
three card headings+bodies+icons, and the takeaway. Keep everything else.

## Copy grounding

Copy is verified against `docs/<svc>.md` / the service's real behavior.

**Ground in the FULL feature surface — not the one-liner.** Before writing a card, enumerate
every capability class and endpoint the service exposes; only then decide what to feature or
cut, and cut *deliberately*. Do **not** let `release.yaml`'s one-line `description:` bound
coverage — it is a label, not the spec. Lead with the most *differentiated* hook, which is
often absent from the one-liner.

> Example failure to avoid: the first agent-model card anchored on release.yaml's
> "routing, fallback, cost tracking" and silently dropped agent-model's strongest hooks —
> **subscription auth** (run on a ChatGPT Pro / Claude Max subscription, no API key; image
> generation billed to subscription quota) and **image/video generation** — plus embeddings
> and the Replicate passthrough.

agent-model's full surface (from `docs/agentmodel.md`), for reference:
`/v1/chat/completions` · `/v1/messages` (Anthropic wire) · `/v1/embeddings` ·
`/v1/images/generations` · `/v1/videos/*` · `/v1/predictions/*` (Replicate passthrough) ·
`/v1/models` · `/v1/limits`; providers OpenAI · Anthropic (API key **or** Claude Pro/Max
OAuth) · Gemini · ChatGPT subscription · Azure · DeepSeek · Replicate · other OpenAI-wire
vendors; `auth_mode: api_key | subscription` with multi-account pools + rotation; weighted
routing · fallback + `router_cooldown`; gateway/per-key budgets · virtual keys · response
cache · usage/cost/latency accounting (metadata-only audit log) · Prometheus/OTLP telemetry.
