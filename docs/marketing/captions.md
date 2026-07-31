# Post captions (文案) — agent-model carousels

Draft launch copy to accompany the carousels. Same grounding rules as the on-image copy:
accurate to `docs/agentmodel.md`, **responsible framing** (subscription/multi-account = a
supported auth mode + a ToS line, never quota-evasion), no absolute overclaims.

> **Repo note:** CTAs say "GitHub 搜 agent-in-the-shell". The public repo doesn't exist
> until the release **M0** ships — this is launch-ready copy; don't post until the repo is live.
> RedNote can't do inline links, so CTAs use a search hint + optional comment-gate.

---

## A. 小红书 — broad / 白話 edition (`agent-model-broad/`)

**Title (pick one, ≤20 字):**
1. 一個工具接遍所有 AI，還能省錢 💸
2. 串 AI 不用一家一家接！省事又省錢
3. 已經付的 AI 訂閱，其實還能這樣用

**正文:**
每接一個 AI 就要重寫一次？😵
OpenAI、Claude、Gemini… 用一個工具全部接起來，換模型只要改設定、不用改程式。

✅ 一個地方接所有 AI
✅ 某家掛了自動換一家，不斷線
✅ 花多少看得一清二楚（只記帳，不看你的內容）
💳 手上的 ChatGPT／Claude 訂閱也能拿來當後端，少開一份 API 帳單

免費、開源，自己架就能用 🛠️
（訂閱／多帳號用途記得依各平台服務條款）

—
🔖 先收藏，需要的時候不用再找
💬 留言「AI」私你 GitHub 連結
👉 GitHub 搜 agent-in-the-shell

**Hashtags:** #AI工具 #ChatGPT #Claude #Gemini #開源軟體 #程式 #省錢 #AI應用 #工程師日常 #LLM

---

## B. 小红书 / Threads — developer zh edition (`agent-model/`)

**Title (pick one):**
1. 一顆 binary 接上所有 LLM｜OpenAI-相容 gateway
2. 別再為每家 LLM 各寫一套 SDK 了
3. 換模型 = 改一行 YAML，不是改程式

**正文:**
還在為每家 LLM 各寫一套 SDK、各自處理 retry／限流／計費？

agent-model：一顆 Go binary 的 OpenAI-相容 gateway。
・多供應商路由 + 自動 fallback（需先設定備援）
・每次呼叫的 token／花費／延遲記帳（只記 metadata，不落地 prompt）
・虛擬金鑰、預算上限、回應快取
・認證可用 API key 或訂閱（ChatGPT Pro／Claude Max）※依各服務條款
・不只文字：圖片／影片生成、embeddings、Replicate passthrough

換模型 = 改一行 YAML。你的 app 照打 OpenAI 就好。

—
⭐ GitHub 搜 agent-in-the-shell（MIT、自架免費）
🔖 收藏｜💬 留言想看的功能

**Hashtags:** #LLM #OpenAI #Claude #Golang #AI開發 #開源 #gateway #後端 #DevTools

---

## C. X / Threads — developer en edition (`agent-model-en/`)

**Post / thread opener:**
One Go binary that fronts every major LLM provider. 🧵

agent-model — an OpenAI-compatible gateway:
• multi-provider routing + auto fallback (pre-configured)
• per-request token / cost / latency accounting (metadata-only)
• virtual keys, budgets, response cache
• auth by API key **or** subscription — ChatGPT Pro / Claude Max*
• not just chat: image/video gen, embeddings, Replicate passthrough

Switch models = one YAML line, not code. MIT, self-host.

→ github.com/agent-in-the-shell/agent-in-the-shell
*follow each provider's ToS for programmatic / multi-account use

**Hashtags:** #LLM #OpenAI #Claude #golang #AItools #opensource #devtools

---

## D. LinkedIn — developer en (longer, for `agent-model-en/`)

If you run LLM features in production, you know the tax: a new SDK per provider, bespoke
retry/rate-limit handling, scattered billing, and a redeploy every time you switch models.

**agent-model** collapses that into one self-hosted Go binary — an OpenAI-compatible gateway
with multi-provider routing, automatic fallback, per-request cost/latency accounting
(metadata-only), virtual keys, budgets, and a response cache. It also serves image/video
generation, embeddings, and a Replicate passthrough. Auth is by API key or subscription
(ChatGPT Pro / Claude Max) — use credentials you're entitled to, per each provider's ToS.

Switch models with one line of YAML, no code change. MIT-licensed, self-hostable.

GitHub: github.com/agent-in-the-shell/agent-in-the-shell

**Hashtags:** #LLM #AI #OpenSource #SoftwareEngineering #DevOps #GoLang

---

## CTA / posting notes (from the growth-persona review)

- **On-platform engagement first** (RedNote ranks on comments + saves): ask for 收藏 + a
  comment-gate ("留言『AI』私你連結") before the off-platform ⭐.
- **Cover carries the scroll-stop**; the caption's first line should echo, not repeat, it.
- Keep the **ToS line** in any caption that mentions subscription/multi-account.
- When copy/claims change in a carousel edition, update the matching caption here too.
