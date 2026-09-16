package conformance

// Live provider integration tests: the real provider adapters against real
// upstreams, covering what fakes structurally cannot — credentials that must
// refresh, opaque values only the provider can mint (thinking-block
// signatures), and prices keyed to model ids upstream actually serves.
//
// Gating, spend, and how to run: services/agentmodel/README.md#live-provider-tests.
//
// Profile paths follow the same resolution the gateway uses and can be
// overridden with AGENTMODEL_LIVE_ANTHROPIC_OAUTH_DIR /
// AGENTMODEL_LIVE_CHATGPT_OAUTH_DIR. Model ids are consts below: this file is
// run by hand, so retargeting a retired model is a one-line edit, not config.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/api"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/contentlog"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

// Cheapest current model per path. Subscription paths must name a model the
// plan actually serves.
const (
	liveAnthropicModel    = "claude-haiku-4-5"
	liveAnthropicSubModel = "claude-sonnet-4-6"
	liveOpenAIModel       = "gpt-5-nano"
	liveChatGPTModel      = "gpt-5.5"
)

// ─── Gating ───────────────────────────────────────────────────────────────────

const (
	liveMasterEnv = "AGENTMODEL_LIVE"
	liveLegacyEnv = "AGENTMODEL_CONFORMANCE_LIVE"
)

// requireLive enforces the master switch. estimate describes what this test
// spends, so the skip message tells a reader what turning it on costs.
func requireLive(t *testing.T, estimate string) {
	t.Helper()
	if os.Getenv(liveMasterEnv) != "1" && os.Getenv(liveLegacyEnv) != "1" {
		t.Skipf("set %s=1 to run live provider tests (this one spends %s)", liveMasterEnv, estimate)
	}
}

// requireKeyEnv skips unless an API key env var is populated.
func requireKeyEnv(t *testing.T, env string) {
	t.Helper()
	if os.Getenv(env) == "" {
		t.Skipf("%s not set", env)
	}
}

// requireOAuthDir resolves a subscription profile directory exactly as the
// gateway does — profile sidecar, AGENTMODEL_PROFILE, and the provider's token
// dir root — and skips unless the store holds a token the request path can
// actually read.
//
// GatewayReadable is the distinction that matters: the nested Claude-CLI/pi
// layout has a token present that readCreds cannot consume. Gating on mere
// presence would run the test and fail with a 401 blamed on the gateway.
func requireOAuthDir(t *testing.T, overrideEnv, provider string) string {
	t.Helper()
	resolve := auth.ResolveProfileTokenDir
	if provider == "chatgpt" {
		resolve = auth.ResolveChatGPTProfileTokenDir
	}
	dir, profile, err := resolve("", os.Getenv(overrideEnv))
	if err != nil {
		t.Skipf("resolve %s token dir: %v", provider, err)
	}
	st, err := auth.InspectStore(provider, dir)
	if err != nil {
		t.Skipf("inspect %s store at %s: %v", provider, dir, err)
	}
	if !st.Exists || !st.HasToken {
		t.Skipf("no %s subscription profile %q at %s (log in with agent-model %s-login, or set %s)",
			provider, profile, dir, provider, overrideEnv)
	}
	if !st.GatewayReadable {
		t.Skipf("%s profile %q at %s holds a %q-layout token the gateway cannot read",
			provider, profile, dir, st.Format)
	}
	return dir
}

// ─── Deployment shapes ────────────────────────────────────────────────────────

// yamlIndent matches the indentation conformanceConfigYAML splices deployment
// keys into.
const yamlIndent = "\n        "

func apiKeyDeployment(keyEnv string) string {
	return `auth_mode: "api_key"` + yamlIndent + `api_key_env: ` + quote(keyEnv)
}

func oauthDeployment(dir string) string {
	return `auth_mode: "subscription"` + yamlIndent + `oauth_token_dir: ` + quote(dir)
}

// okProbe is the cheapest request that still produces usage and an audit row.
func okProbe() agentmodel.ChatRequest {
	return agentmodel.ChatRequest{
		Model:     "task-model",
		Messages:  []agentmodel.Message{{Role: "user", Content: "Reply with the single word: ok"}},
		MaxTokens: iptr(64),
	}
}

// ─── Extended thinking ────────────────────────────────────────────────────────

// TestLive_AnthropicThinking is the real-provider guard for the reasoning
// surface: extended thinking must survive the gateway streamed and
// non-streamed, and reach the content log with signatures intact.
//
// Signatures are why this cannot be a stub test — only Anthropic can mint one,
// and a gateway that drops them still passes every stubbed test while making
// real multi-turn thinking unreplayable.
func TestLive_AnthropicThinking(t *testing.T) {
	requireLive(t, "~$0.005 of Anthropic tokens, or subscription quota")

	// deployment resolves credentials lazily, so a missing one skips its own
	// subtest rather than the whole test.
	targets := []struct {
		name       string
		provider   string
		model      string
		deployment func(*testing.T) string
	}{
		{"api_key", "anthropic", liveAnthropicModel, func(t *testing.T) string {
			requireKeyEnv(t, "ANTHROPIC_API_KEY")
			return apiKeyDeployment("ANTHROPIC_API_KEY")
		}},
		{"subscription", "anthropic-oauth", liveAnthropicSubModel, func(t *testing.T) string {
			return oauthDeployment(requireOAuthDir(t, "AGENTMODEL_LIVE_ANTHROPIC_OAUTH_DIR", "anthropic"))
		}},
	}

	for _, target := range targets {
		t.Run(target.name, func(t *testing.T) {
			dep := target.deployment(t)

			logPath := filepath.Join(t.TempDir(), "content.jsonl")
			lg, err := contentlog.Open(logPath)
			if err != nil {
				t.Fatalf("open content log: %v", err)
			}
			t.Cleanup(func() { lg.Close() })
			ts, _ := buildConformanceServer(t,
				conformanceConfigYAML(target.provider, target.model, dep),
				func(c *api.Config) { c.ContentLog = lg })

			// max_tokens must exceed the thinking budget, or Anthropic rejects
			// the request outright. 1024 is the minimum budget it accepts.
			req := agentmodel.ChatRequest{
				Model:     "task-model",
				Messages:  []agentmodel.Message{{Role: "user", Content: "What is 17 * 23? Think it through, then answer with just the number."}},
				MaxTokens: iptr(2048),
				Thinking:  &agentmodel.ThinkingConfig{Type: "enabled", BudgetTokens: 1024},
			}

			t.Run("non_streaming", func(t *testing.T) {
				got := postChatJSON(t, ts.URL, conformanceToken, req)
				if len(got.Choices) == 0 {
					t.Fatalf("no choices: %+v", got)
				}
				assertThinking(t, "non-streaming", got.Choices[0].Message.ReasoningContent, got.Choices[0].Message.ThinkingBlocks)
			})

			t.Run("streaming", func(t *testing.T) {
				streamReq := req
				streamReq.Stream = true
				resp := postChat(t, ts.URL, conformanceToken, streamReq)
				chunks, sawDone := collectStream(t, resp)
				if !sawDone {
					t.Error("stream did not terminate with data: [DONE]")
				}

				var reasoning strings.Builder
				var blocks []agentmodel.ThinkingBlock
				for _, c := range chunks {
					for _, ch := range c.Choices {
						reasoning.WriteString(ch.Delta.ReasoningContent)
						blocks = append(blocks, ch.Delta.ThinkingBlocks...)
					}
				}
				assertThinking(t, "streamed", reasoning.String(), blocks)

				// Streaming used to log reasoning text only, dropping the
				// signatures (, fixed in ).
				logged := lastLoggedMessage(t, logPath)
				assertThinking(t, "content log", logged.ReasoningContent, logged.ThinkingBlocks)
			})
		})
	}
}

// assertThinking checks the invariants the gateway owns: a block per thinking
// turn, each typed and signed, and the two representations of the same thinking
// agreeing.
//
// It does not assert thinking text. Anthropic may return an empty `thinking`
// with an intact signature — observed 2026-07-27 over subscription OAuth, and on
// the native /v1/messages passthrough too, with
// usage.output_tokens_details.thinking_tokens confirming the model did think.
// Requiring text would encode provider policy we cannot fix.
func assertThinking(t *testing.T, ctx, reasoning string, blocks []agentmodel.ThinkingBlock) {
	t.Helper()
	if len(blocks) == 0 {
		t.Fatalf("%s: no thinking_blocks — thinking was requested and billed for", ctx)
	}
	var anyText bool
	for i, b := range blocks {
		switch b.Type {
		case "thinking":
			anyText = anyText || strings.TrimSpace(b.Thinking) != ""
		case "redacted_thinking":
			// Opaque by design: signature only, no readable text.
		default:
			t.Errorf("%s: thinking_blocks[%d] unexpected type %q", ctx, i, b.Type)
		}
		// Anthropic rejects thinking blocks replayed on a later tool-use turn
		// without their signature, so a dropped one makes the history
		// uncontinuable.
		if b.Signature == "" {
			t.Errorf("%s: thinking_blocks[%d] (%s) has no signature", ctx, i, b.Type)
		}
	}
	// Both views are built from the same upstream blocks, so text in one and
	// nothing in the other means the gateway lost it in translation.
	if anyText && strings.TrimSpace(reasoning) == "" {
		t.Errorf("%s: thinking_blocks carry text but reasoning_content is empty", ctx)
	}
}

// ─── Subscription auth ────────────────────────────────────────────────────────

// TestLive_SubscriptionAuth exercises the OAuth-backed providers, which no stub
// test reaches: the profile on disk must load, refresh if stale, authenticate
// upstream, and meter as subscription rather than as tokens. A refresh or header
// regression here is invisible to CI and total at runtime.
func TestLive_SubscriptionAuth(t *testing.T) {
	requireLive(t, "subscription quota (no per-token charge)")

	cases := []struct {
		name     string
		provider string
		model    string
		dirEnv   string
		dirName  string
	}{
		{"anthropic_oauth", "anthropic-oauth", liveAnthropicSubModel, "AGENTMODEL_LIVE_ANTHROPIC_OAUTH_DIR", "anthropic"},
		{"chatgpt", "chatgpt", liveChatGPTModel, "AGENTMODEL_LIVE_CHATGPT_OAUTH_DIR", "chatgpt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := requireOAuthDir(t, c.dirEnv, c.dirName)
			ts, st := buildConformanceServer(t,
				conformanceConfigYAML(c.provider, c.model, oauthDeployment(dir)))

			got := postChatJSON(t, ts.URL, conformanceToken, okProbe())
			if len(got.Choices) == 0 || strings.TrimSpace(got.Choices[0].Message.Content) == "" {
				t.Fatalf("no content returned: %+v", got)
			}
			if got.Usage.AuthMode != agentmodel.AuthModeSubscription {
				t.Errorf("usage.auth_mode = %q, want %q", got.Usage.AuthMode, agentmodel.AuthModeSubscription)
			}
			log := soleRequestLog(t, st)
			if log.AuthMode != agentmodel.AuthModeSubscription {
				t.Errorf("audit log auth_mode = %q, want %q", log.AuthMode, agentmodel.AuthModeSubscription)
			}
			// Quota-funded: billing per token would double-charge a flat-rate plan.
			if log.CostSource != "subscription" {
				t.Errorf("audit log cost_source = %q, want subscription", log.CostSource)
			}
			if log.CostUSD != 0 {
				t.Errorf("audit log cost_usd = %v, want 0 for a subscription request", log.CostUSD)
			}
		})
	}
}

// ─── Metering ─────────────────────────────────────────────────────────────────

// TestLive_APIKeyMetering pins metering against real model ids. The price
// catalog is keyed by provider model id, so a rename or a dropped entry makes
// real traffic meter at $0 with cost_source "unpriced" — spend that silently
// stops being attributed. Stub tests cannot catch it: they invent model ids that
// were never in the catalog.
func TestLive_APIKeyMetering(t *testing.T) {
	requireLive(t, "~$0.001 of OpenAI tokens")
	requireKeyEnv(t, "OPENAI_API_KEY")

	ts, st := buildConformanceServer(t,
		conformanceConfigYAML("openai", liveOpenAIModel, apiKeyDeployment("OPENAI_API_KEY")))

	got := postChatJSON(t, ts.URL, conformanceToken, okProbe())
	if got.Usage.TotalTokens <= 0 {
		t.Fatalf("usage not reported: %+v", got.Usage)
	}

	log := soleRequestLog(t, st)
	if log.CostSource != "priced" {
		t.Errorf("cost_source = %q, want priced — %q is missing from the price catalog "+
			"(run make sync-model-prices)", log.CostSource, liveOpenAIModel)
	}
	if log.CostUSD <= 0 {
		t.Errorf("cost_usd = %v, want > 0 for a priced api_key request", log.CostUSD)
	}
	if log.PromptTokens <= 0 || log.CompletionTokens <= 0 {
		t.Errorf("token counts not persisted: prompt=%d completion=%d", log.PromptTokens, log.CompletionTokens)
	}
	if log.AuthMode != agentmodel.AuthModeAPIKey {
		t.Errorf("auth_mode = %q, want %q", log.AuthMode, agentmodel.AuthModeAPIKey)
	}
	if log.ModelUsed != liveOpenAIModel {
		t.Errorf("model_used = %q, want %q", log.ModelUsed, liveOpenAIModel)
	}
}

// ─── Shared helpers ───────────────────────────────────────────────────────────

// soleRequestLog returns the single audit row the test's request wrote.
func soleRequestLog(t *testing.T, st store.Store) store.RequestLog {
	t.Helper()
	logs, err := st.ListByOrg(context.Background(), "default", time.Now().Add(-5*time.Minute), 10)
	if err != nil {
		t.Fatalf("list audit logs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("audit logs = %d, want 1", len(logs))
	}
	if logs[0].Status != "ok" {
		t.Fatalf("audit log status = %q (error_type %q), want ok", logs[0].Status, logs[0].ErrorType)
	}
	return logs[0]
}

// lastLoggedMessage returns the assistant message from the final content-log
// record, which is the one the calling test just produced.
func lastLoggedMessage(t *testing.T, path string) agentmodel.Message {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open content log: %v", err)
	}
	defer f.Close()

	var last agentmodel.Message
	var found bool
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var rec contentlog.Record
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("decode content log line: %v", err)
		}
		var resp agentmodel.ChatResponse
		if err := json.Unmarshal(rec.Response, &resp); err != nil {
			t.Fatalf("decode logged response: %v", err)
		}
		if len(resp.Choices) > 0 {
			last, found = resp.Choices[0].Message, true
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan content log: %v", err)
	}
	if !found {
		t.Fatal("content log recorded no response")
	}
	return last
}
