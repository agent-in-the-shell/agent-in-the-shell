package conformance

// Cross-provider conformance test (issue #664): an identical task transcript,
// driven by a provider-BLIND driver, must complete and stay OpenAI-shape
// conformant whether the gateway routes it to an OpenAI-family or an
// Anthropic-family deployment — switched by config alone (only the deployments
// block of the YAML differs).
//
// Two modes share one driver and one assertion set:
//
//   - TestConformance_Stub  — deterministic CI. Routes through the REAL provider
//     adapters to native-dialect fake upstreams (fake_upstreams_test.go) reached
//     via the new base_url deployment field. Runs in plain `go test` with no
//     network and no secrets.
//   - TestConformance_Live  — manual, opt-in (AGENTMODEL_CONFORMANCE_LIVE=1) and
//     per-provider key-gated. Same driver against real providers; catches what
//     fakes cannot (provider-side vocabulary/encoding drift).
//
// "OpenAI-shape conformant" here means: created is a positive unix timestamp;
// tool_calls carry id/type/function and (when streamed) a per-fragment integer
// index with id/type/name only on the first fragment, so the official OpenAI
// SDK accumulators reconstruct exactly one call; finish_reason is from the
// closed OpenAI enum; usage fields are populated and self-consistent. The local
// accumulator below enforces the same merge-by-index invariants the official
// openai-go ChatCompletionAccumulator would (wiring that SDK as a test-only dep
// is a deferred [SHOULD], S9, to avoid a heavy dependency without sign-off).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/api"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/cost"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/factory"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

const conformanceToken = "conformance-test-token"

// ─── Test entry points ────────────────────────────────────────────────────────

// TestConformance_Stub runs the transcript against both providers via fake
// upstreams. The two YAML configs differ only in the deployments block — the
// honest "swap by config alone" demonstration.
func TestConformance_Stub(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		model    string
		upstream func(*testing.T) *httptest.Server
	}{
		{"openai", "openai", "gpt-4o", newFakeOpenAIUpstream},
		{"anthropic", "anthropic", "claude-haiku-4-5", newFakeAnthropicUpstream},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up := c.upstream(t)
			cfgYAML := conformanceConfigYAML(c.provider, c.model, "base_url: "+quote(up.URL))
			ts, _ := buildConformanceServer(t, cfgYAML)
			runConformanceTranscript(t, ts.URL, conformanceToken, "task-model", c.name)
		})
	}
}

// TestConformance_Live runs the same transcript against real providers. It is
// manual-only and shares the gate in live_test.go: the master switch is consent
// to spend, and each provider additionally self-skips without its key (so a
// developer's exported OPENAI_API_KEY never triggers spend on its own).
func TestConformance_Live(t *testing.T) {
	requireLive(t, "~$0.003 of OpenAI + Anthropic tokens")
	cases := []struct {
		name     string
		provider string
		model    string
		keyEnv   string
	}{
		// gpt-5-nano / claude-haiku-4-5 are the cheapest current models. If
		// gpt-5-nano's hidden reasoning tokens cause finish_reason:"length" or
		// empty content at max_tokens:300, fall back to gpt-4.1-nano.
		{"openai", "openai", "gpt-5-nano", "OPENAI_API_KEY"},
		{"anthropic", "anthropic", "claude-haiku-4-5", "ANTHROPIC_API_KEY"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			requireKeyEnv(t, c.keyEnv)
			cfgYAML := conformanceConfigYAML(c.provider, c.model, apiKeyDeployment(c.keyEnv))
			ts, st := buildConformanceServer(t, cfgYAML)
			runConformanceTranscript(t, ts.URL, conformanceToken, "task-model", c.name)
			assertTokenBudget(t, st, 5000)
		})
	}
}

// ─── The provider-blind driver ────────────────────────────────────────────────

// runConformanceTranscript drives an identical multi-turn tool transcript with
// NO provider-specific branches and asserts OpenAI-shape conformance throughout.
func runConformanceTranscript(t *testing.T, gatewayURL, token, model, label string) {
	t.Helper()

	weatherTool := agentmodel.Tool{
		Type: "function",
		Function: agentmodel.FunctionSchema{
			Name:        conformanceToolName,
			Description: "Look up the current weather for a location.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"location": map[string]any{"type": "string", "description": "City name"},
				},
				"required": []any{"location"},
			},
		},
	}
	userMsg := agentmodel.Message{Role: "user", Content: "What's the weather in SF? Use the get_weather tool."}

	// ── N-series: non-streaming forced tool call ──
	nsResp := postChatJSON(t, gatewayURL, token, agentmodel.ChatRequest{
		Model:       model,
		Messages:    []agentmodel.Message{userMsg},
		Tools:       []agentmodel.Tool{weatherTool},
		ToolChoice:  "required",
		Temperature: f64(0),
		MaxTokens:   iptr(300),
	})
	t.Logf("conformance: provider=%s mode=non-streaming check=tool_call result=ok", label)
	if nsResp.Created <= 0 {
		t.Errorf("[N3] created = %d, want > 0", nsResp.Created)
	}
	if len(nsResp.Choices) == 0 {
		t.Fatalf("[N7] no choices in non-streaming response")
	}
	ch := nsResp.Choices[0]
	if ch.FinishReason != "tool_calls" {
		t.Errorf("[N7] finish_reason = %q, want tool_calls", ch.FinishReason)
	}
	if len(ch.Message.ToolCalls) == 0 {
		t.Fatalf("[N7] expected >=1 tool_calls")
	}
	for i, tc := range ch.Message.ToolCalls {
		assertToolCallShape(t, fmt.Sprintf("[N8] tool_calls[%d]", i), tc.ID, tc.Type, tc.Function.Name, tc.Function.Arguments)
	}
	assertUsage(t, "[N10] non-streaming", nsResp.Usage.PromptTokens, nsResp.Usage.CompletionTokens, nsResp.Usage.TotalTokens)

	// ── S-series: streaming forced tool call ──
	stoolResp := postChat(t, gatewayURL, token, agentmodel.ChatRequest{
		Model:       model,
		Messages:    []agentmodel.Message{userMsg},
		Tools:       []agentmodel.Tool{weatherTool},
		ToolChoice:  "required",
		Temperature: f64(0),
		MaxTokens:   iptr(300),
		Stream:      true,
	})
	chunks, sawDone := collectStream(t, stoolResp)
	if !sawDone {
		t.Errorf("[S1] streamed tool call did not terminate with data: [DONE]")
	}
	assertChunkEnvelope(t, "[S2] tool-call stream", chunks)
	streamedCall := assertStreamedToolCall(t, chunks)
	t.Logf("conformance: provider=%s mode=streaming check=tool_call_deltas result=ok", label)

	// ── M-series: echo the tool result back; streamed final answer ends "stop" ──
	finalResp := postChat(t, gatewayURL, token, agentmodel.ChatRequest{
		Model: model,
		Messages: []agentmodel.Message{
			userMsg,
			{Role: "assistant", ToolCalls: []agentmodel.ToolCall{{
				ID:       streamedCall.id,
				Type:     "function",
				Function: agentmodel.ToolCallFunction{Name: streamedCall.name, Arguments: streamedCall.args},
			}}},
			{Role: "tool", ToolCallID: streamedCall.id, Content: `{"temp_c":21,"summary":"sunny"}`},
		},
		ToolChoice:  "auto",
		Temperature: f64(0),
		MaxTokens:   iptr(300),
		Stream:      true,
	})
	finalChunks, finalDone := collectStream(t, finalResp)
	if !finalDone {
		t.Errorf("[S1] final-answer stream did not terminate with data: [DONE]")
	}
	assertChunkEnvelope(t, "[S2] final-answer stream", finalChunks)
	assertStreamedFinalAnswer(t, finalChunks)
	t.Logf("conformance: provider=%s mode=streaming check=final_answer result=ok", label)
}

// ─── Assertions ───────────────────────────────────────────────────────────────

func assertToolCallShape(t *testing.T, ctx, id, typ, name, args string) {
	t.Helper()
	if id == "" {
		t.Errorf("%s: id is empty", ctx)
	}
	if typ != "function" {
		t.Errorf("%s: type = %q, want function", ctx, typ)
	}
	if name != conformanceToolName {
		t.Errorf("%s: name = %q, want %q (an offered tool)", ctx, name, conformanceToolName)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(args), &obj); err != nil {
		t.Errorf("%s: arguments %q not a valid JSON object: %v", ctx, args, err)
	}
}

func assertUsage(t *testing.T, ctx string, prompt, completion, total int) {
	t.Helper()
	if prompt <= 0 {
		t.Errorf("%s: prompt_tokens = %d, want > 0", ctx, prompt)
	}
	if completion <= 0 {
		t.Errorf("%s: completion_tokens = %d, want > 0", ctx, completion)
	}
	if total != prompt+completion {
		t.Errorf("%s: total_tokens = %d, want %d (prompt+completion)", ctx, total, prompt+completion)
	}
}

// assertChunkEnvelope checks S1/S2: every chunk is a chat.completion.chunk with
// a constant non-empty id and a positive created.
func assertChunkEnvelope(t *testing.T, ctx string, chunks []wireChunk) {
	t.Helper()
	if len(chunks) == 0 {
		t.Fatalf("%s: no chunks received", ctx)
	}
	id := chunks[0].ID
	for i, c := range chunks {
		if c.Object != "chat.completion.chunk" {
			t.Errorf("%s: chunk[%d].object = %q, want chat.completion.chunk", ctx, i, c.Object)
		}
		if c.ID == "" {
			t.Errorf("%s: chunk[%d].id is empty", ctx, i)
		}
		if c.ID != id {
			t.Errorf("%s: chunk[%d].id = %q, want constant %q", ctx, i, c.ID, id)
		}
		if c.Created <= 0 {
			t.Errorf("%s: chunk[%d].created = %d, want > 0", ctx, i, c.Created)
		}
	}
}

type accumulatedToolCall struct {
	id, name, args string
}

// assertStreamedToolCall checks S4/S5/S6 and reconstructs the call the way the
// official OpenAI accumulators do: every tool-call delta carries an integer
// index; id/type/name appear only on the first fragment per index; arguments
// concatenate (in arrival order) into a valid JSON object.
func assertStreamedToolCall(t *testing.T, chunks []wireChunk) accumulatedToolCall {
	t.Helper()
	type acc struct {
		id, typ, name string
		args          strings.Builder
		sawFirst      bool
	}
	byIndex := map[int]*acc{}
	var order []int
	sawToolFinish := false

	for _, c := range chunks {
		for _, choice := range c.Choices {
			if choice.FinishReason == "tool_calls" {
				sawToolFinish = true
			}
			for _, tc := range choice.Delta.ToolCalls {
				if tc.Index == nil {
					t.Errorf("[S4] streamed tool-call delta missing integer index")
					continue
				}
				idx := *tc.Index
				a, ok := byIndex[idx]
				if !ok {
					a = &acc{}
					byIndex[idx] = a
					order = append(order, idx)
				}
				if !a.sawFirst {
					// First fragment per index carries id/type/name.
					a.id, a.typ, a.name = tc.ID, tc.Type, tc.Function.Name
					a.sawFirst = true
				} else {
					// Later fragments must NOT repeat id/type/name (#664 fix B).
					if tc.ID != "" || tc.Type != "" || tc.Function.Name != "" {
						t.Errorf("[S5] tool-call index %d repeats id/type/name on a non-first fragment (id=%q type=%q name=%q)",
							idx, tc.ID, tc.Type, tc.Function.Name)
					}
				}
				a.args.WriteString(tc.Function.Arguments)
			}
		}
	}

	if !sawToolFinish {
		t.Errorf("[S5] streamed tool call did not end with finish_reason tool_calls")
	}
	if len(order) == 0 {
		t.Fatalf("[S4] no streamed tool-call deltas observed")
	}
	first := byIndex[order[0]]
	assertToolCallShape(t, "[S6] streamed tool_call", first.id, first.typ, first.name, first.args.String())
	return accumulatedToolCall{id: first.id, name: first.name, args: first.args.String()}
}

// assertStreamedFinalAnswer checks the final streamed turn: text accumulates to
// something non-empty, it ends with finish_reason "stop", and usage is present
// and self-consistent (S8).
func assertStreamedFinalAnswer(t *testing.T, chunks []wireChunk) {
	t.Helper()
	var content strings.Builder
	sawStop := false
	var usage *wireUsage
	for _, c := range chunks {
		for _, choice := range c.Choices {
			content.WriteString(choice.Delta.Content)
			if choice.FinishReason == "stop" {
				sawStop = true
			}
		}
		if c.Usage != nil {
			usage = c.Usage
		}
	}
	if strings.TrimSpace(content.String()) == "" {
		t.Errorf("[M2] final streamed answer had no content")
	}
	if !sawStop {
		t.Errorf("[M2] final streamed answer did not end with finish_reason stop")
	}
	if usage == nil {
		t.Fatalf("[S8] no usage on the streamed final answer")
	}
	assertUsage(t, "[S8] streaming", usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens)
}

// assertTokenBudget guards live spend: the sum of recorded TotalTokens must stay
// under cap (a runaway loop or accidental large generation trips this).
func assertTokenBudget(t *testing.T, st store.Store, cap int) {
	t.Helper()
	logs, err := st.ListByOrg(context.Background(), "default", time.Now().Add(-time.Hour), 1000)
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	total := 0
	for _, l := range logs {
		total += l.TotalTokens
	}
	if total >= cap {
		t.Errorf("token guard: recorded %d tokens across %d requests, want < %d", total, len(logs), cap)
	}
	t.Logf("conformance: live token usage = %d across %d requests (cap %d)", total, len(logs), cap)
}

// ─── HTTP + SSE plumbing ──────────────────────────────────────────────────────

func postChat(t *testing.T, gatewayURL, token string, body agentmodel.ChatRequest) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	// A live provider that stalls would otherwise hang until the -timeout kills
	// the whole binary, losing every subtest that had not run yet.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatewayURL+"/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/chat/completions: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		// 401/403 fails rather than skips on purpose: a rejected credential is
		// equally consistent with a stale key and with the gateway sending the
		// credential wrong, and skipping would hide the second.
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			t.Fatalf("upstream rejected the credential (%d). Either it is stale — rotate it, or unset "+
				"it to skip this test — or the gateway is sending it wrong, which is a real bug. Body: %s",
				resp.StatusCode, b)
		}
		t.Fatalf("POST status = %d, want 200; body: %s", resp.StatusCode, b)
	}
	return resp
}

func postChatJSON(t *testing.T, gatewayURL, token string, body agentmodel.ChatRequest) agentmodel.ChatResponse {
	t.Helper()
	resp := postChat(t, gatewayURL, token, body)
	defer resp.Body.Close()
	var out agentmodel.ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode chat response: %v", err)
	}
	return out
}

type wireUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// wireChunk is the OpenAI streaming chunk shape the gateway emits.
type wireChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role string `json:"role"`
			// ReasoningContent and ThinkingBlocks carry extended thinking: the
			// text streams incrementally, the blocks arrive whole (signatures
			// attached) in a terminal chunk. Asserted by the live thinking test.
			ReasoningContent string                     `json:"reasoning_content"`
			ThinkingBlocks   []agentmodel.ThinkingBlock `json:"thinking_blocks"`
			Content          string                     `json:"content"`
			ToolCalls        []struct {
				Index    *int   `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
}

// collectStream reads the gateway's SSE body into chunks, stopping at the
// OpenAI-standard data: [DONE] terminator (returned as sawDone).
func collectStream(t *testing.T, resp *http.Response) (chunks []wireChunk, sawDone bool) {
	t.Helper()
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			sawDone = true
			continue
		}
		var c wireChunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Errorf("decode SSE chunk %q: %v", data, err)
			continue
		}
		chunks = append(chunks, c)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan SSE: %v", err)
	}
	return chunks, sawDone
}

// ─── Server construction (LoadConfig -> factory -> router -> api) ─────────────

// conformanceConfigYAML renders a gateway config whose single model "task-model"
// has one deployment for provider/model. deploymentExtra is spliced into the
// deployment (either `base_url: ...` for stub mode or auth_mode/api_key_env for
// live mode) — the ONLY thing that differs between the two provider configs.
func conformanceConfigYAML(provider, model, deploymentExtra string) string {
	return fmt.Sprintf(`listen: ":0"
db: "conformance.db"
auth:
  bearer_token_env: "AGENT_MODEL_TOKEN"
model_list:
  - model_name: "task-model"
    deployments:
      - provider: %q
        model: %q
        %s
        weight: 100
`, provider, model, deploymentExtra)
}

// opts mutate the assembled api.Config before the server is built (the live
// thinking test uses one to attach a content log).
func buildConformanceServer(t *testing.T, cfgYAML string, opts ...func(*api.Config)) (*httptest.Server, store.Store) {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := agentmodel.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	deployments, _, err := factory.BuildDeployments(cfg, logger)
	if err != nil {
		t.Fatalf("factory.BuildDeployments: %v", err)
	}
	if len(deployments["task-model"]) == 0 {
		t.Fatalf("no deployments built for task-model (config swap is wired wrong)")
	}

	rt := router.New(deployments, nil)
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "conformance-store.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	registry, err := cost.LoadDefault()
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}

	apiCfg := api.Config{
		Router:      rt,
		Store:       st,
		Registry:    registry,
		BearerToken: conformanceToken,
		Logger:      logger,
	}
	for _, opt := range opts {
		opt(&apiCfg)
	}
	srv := api.New(apiCfg)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, st
}

// ─── small helpers ────────────────────────────────────────────────────────────

func f64(v float64) *float64 { return &v }
func iptr(v int) *int        { return &v }
func quote(s string) string  { return fmt.Sprintf("%q", s) }
