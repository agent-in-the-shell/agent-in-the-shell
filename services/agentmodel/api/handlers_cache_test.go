package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/api"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/cache"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/stub"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
)

// withCache enables an in-memory response cache on the test server.
func withCache(t *testing.T) func(*api.Config) {
	t.Helper()
	c, err := cache.New(cache.Config{})
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return func(cfg *api.Config) { cfg.Cache = c }
}

func cacheStub() *stub.Stub {
	return &stub.Stub{
		CompleteResp: agentmodel.ChatResponse{
			ID:      "stub-cmpl-1", // non-empty so the stub returns CompleteResp verbatim
			Object:  "chat.completion",
			Model:   "gpt-4",
			Choices: []agentmodel.Choice{{Index: 0, Message: agentmodel.Message{Role: "assistant", Content: "cached-answer"}, FinishReason: "stop"}},
			Usage:   agentmodel.Usage{PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12},
		},
		StreamChunks: []provider.StreamChunk{{Delta: agentmodel.Message{Role: "assistant", Content: "hi"}, FinishReason: "stop"}},
	}
}

func cacheReq(content string) agentmodel.ChatRequest {
	return agentmodel.ChatRequest{Model: "gpt-4", Messages: []agentmodel.Message{{Role: "user", Content: content}}}
}

func assistantText(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var cr agentmodel.ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatalf("decode chat response: %v", err)
	}
	if len(cr.Choices) == 0 {
		t.Fatalf("response had no choices")
	}
	return cr.Choices[0].Message.Content
}

// TestChatCache_HitServesFromCacheWithoutUpstream is the headline behavior: a
// second identical request is served from the cache, the upstream is called
// exactly once, and the hit is ledgered at $0 with cost_source "cache" so it
// never advances a budget.
func TestChatCache_HitServesFromCacheWithoutUpstream(t *testing.T) {
	st := cacheStub()
	ts, ledger := newTestServer(t, map[string][]router.Deployment{
		"gpt-4": {{Provider: st, Model: "gpt-4", Weight: 1}},
	}, withCache(t))

	r1 := mustPost(t, ts, "/v1/chat/completions", cacheReq("same prompt"), testToken)
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d, want 200", r1.StatusCode)
	}
	if got := r1.Header.Get("X-Agentmodel-Cache"); got != "miss" {
		t.Fatalf("first X-Agentmodel-Cache = %q, want miss", got)
	}
	if got := assistantText(t, r1); got != "cached-answer" {
		t.Fatalf("first body = %q, want cached-answer", got)
	}

	r2 := mustPost(t, ts, "/v1/chat/completions", cacheReq("same prompt"), testToken)
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d, want 200", r2.StatusCode)
	}
	if got := r2.Header.Get("X-Agentmodel-Cache"); got != "hit" {
		t.Fatalf("second X-Agentmodel-Cache = %q, want hit", got)
	}
	if got := assistantText(t, r2); got != "cached-answer" {
		t.Fatalf("second body = %q, want cached-answer (from cache)", got)
	}

	if n := st.CallCount(); n != 1 {
		t.Fatalf("upstream CallCount = %d, want 1 (second request must be served from cache)", n)
	}

	// Ledger: two rows; exactly one is the cache hit at $0.
	rows, err := ledger.ListByOrg(context.Background(), "default", time.Now().Add(-time.Hour), 100)
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("ledger rows = %d, want 2 (one miss + one cache hit)", len(rows))
	}
	var cacheRows int
	for _, rl := range rows {
		if rl.CostSource == "cache" {
			cacheRows++
			if rl.CostUSD != 0 {
				t.Fatalf("cache-hit row cost = %v, want 0 (must not advance a budget)", rl.CostUSD)
			}
			if rl.TotalTokens != 12 {
				t.Fatalf("cache-hit row total_tokens = %d, want 12 (tokens saved should stay visible)", rl.TotalTokens)
			}
		}
	}
	if cacheRows != 1 {
		t.Fatalf("cache-source rows = %d, want 1", cacheRows)
	}
}

// TestChatCache_DistinctRequestsMiss confirms different prompts do not collide:
// each is an independent miss that reaches the upstream.
func TestChatCache_DistinctRequestsMiss(t *testing.T) {
	st := cacheStub()
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"gpt-4": {{Provider: st, Model: "gpt-4", Weight: 1}},
	}, withCache(t))

	for _, prompt := range []string{"first", "second"} {
		r := mustPost(t, ts, "/v1/chat/completions", cacheReq(prompt), testToken)
		if got := r.Header.Get("X-Agentmodel-Cache"); got != "miss" {
			t.Fatalf("prompt %q X-Agentmodel-Cache = %q, want miss", prompt, got)
		}
		r.Body.Close()
	}
	if n := st.CallCount(); n != 2 {
		t.Fatalf("CallCount = %d, want 2 (distinct prompts must not share a cache entry)", n)
	}
}

// TestChatCache_DisabledByDefault confirms the cache is opt-in: with no Cache
// configured there is no cache header and identical requests both hit upstream.
func TestChatCache_DisabledByDefault(t *testing.T) {
	st := cacheStub()
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"gpt-4": {{Provider: st, Model: "gpt-4", Weight: 1}},
	})

	for i := 0; i < 2; i++ {
		r := mustPost(t, ts, "/v1/chat/completions", cacheReq("same prompt"), testToken)
		if got := r.Header.Get("X-Agentmodel-Cache"); got != "" {
			t.Fatalf("X-Agentmodel-Cache = %q with cache disabled, want empty", got)
		}
		r.Body.Close()
	}
	if n := st.CallCount(); n != 2 {
		t.Fatalf("CallCount = %d, want 2 (no caching when disabled)", n)
	}
}

// TestChatCache_StreamingNotCached documents the v1 scope: streaming requests
// bypass the cache entirely (no cache header, every call reaches the upstream).
func TestChatCache_StreamingNotCached(t *testing.T) {
	st := cacheStub()
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"gpt-4": {{Provider: st, Model: "gpt-4", Weight: 1}},
	}, withCache(t))

	for i := 0; i < 2; i++ {
		req := cacheReq("same prompt")
		req.Stream = true
		r := mustPost(t, ts, "/v1/chat/completions", req, testToken)
		if got := r.Header.Get("X-Agentmodel-Cache"); got != "" {
			t.Fatalf("streaming X-Agentmodel-Cache = %q, want empty (streaming not cached)", got)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		r.Body.Close()
	}
	if n := st.CallCount(); n != 2 {
		t.Fatalf("CallCount = %d, want 2 (streaming must not be cached)", n)
	}
}
