package anthropic

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
)

// systemBlocks runs one Complete against a fake upstream and returns the
// `system` field of the body that went out, normalized to a slice: Anthropic
// accepts a bare string or an array of blocks, and which one we send is itself
// part of what these tests pin.
func systemBlocks(t *testing.T, apiKey bool, req agentmodel.ChatRequest) (blocks []map[string]any, bare string) {
	t.Helper()
	respBody := `{"id":"m","type":"message","role":"assistant","model":"claude-3-5-sonnet-latest","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	fake := newFakeAnthropic(t, respBody)
	var a auth.Authenticator = newOAuthAuth()
	if apiKey {
		a = newAPIKeyAuth()
	}
	req.Model = "claude-3-5-sonnet-latest"
	if _, err := NewWithBaseURL(a, fake.srv.URL).Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(fake.gotBody, &got); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	switch s := got["system"].(type) {
	case nil:
		return nil, ""
	case string:
		return nil, s
	case []any:
		for _, b := range s {
			blocks = append(blocks, b.(map[string]any))
		}
		return blocks, ""
	default:
		t.Fatalf("system: unexpected type %T (%v)", got["system"], got["system"])
		return nil, ""
	}
}

func lastCacheControl(blocks []map[string]any) map[string]any {
	if len(blocks) == 0 {
		return nil
	}
	cc, _ := blocks[len(blocks)-1]["cache_control"].(map[string]any)
	return cc
}

func hasIdentity(blocks []map[string]any, bare string) bool {
	if bare == claudeCodeIdentityPrompt {
		return true
	}
	for _, b := range blocks {
		if b["text"] == claudeCodeIdentityPrompt {
			return true
		}
	}
	return false
}

var cacheKeyMessages = []agentmodel.Message{
	{Role: "system", Content: "stable system prompt"},
	{Role: "user", Content: "hi"},
}

// TestPromptCacheKey_APIKeyModeCachesWhenKeyPresent is the behaviour change:
// an api_key request that carries prompt_cache_key gets a system breakpoint.
// Before this, caching was reachable only in subscription mode, because the
// breakpoint shared an `if` with the Claude Code identity block — two unrelated
// decisions that could only move together.
//
// The key is the client saying "this prefix recurs" in the only vocabulary
// /v1/chat/completions has for it. We do not invent a second one.
func TestPromptCacheKey_APIKeyModeCachesWhenKeyPresent(t *testing.T) {
	blocks, bare := systemBlocks(t, true, agentmodel.ChatRequest{
		Messages:       cacheKeyMessages,
		PromptCacheKey: "session-abc",
	})
	if cc := lastCacheControl(blocks); cc == nil {
		t.Fatalf("api_key + prompt_cache_key should cache the system prompt; system = %v / %q", blocks, bare)
	}
	// Unbraiding means exactly this: caching arrived without the identity block
	// tagging along. Sending it on an api_key request would misrepresent a
	// plain API client as a Claude Code one.
	if hasIdentity(blocks, bare) {
		t.Errorf("api_key request must not carry the Claude Code identity block; system = %v", blocks)
	}
}

// TestPromptCacheKey_APIKeyModeNoKeyNoCache pins the other half of the signal:
// without a key we do not cache. A cache write costs 1.25x normal input, so
// marking a prefix the client never sends again loses 25% on that request —
// silence has to mean "don't", not "cache anyway".
func TestPromptCacheKey_APIKeyModeNoKeyNoCache(t *testing.T) {
	blocks, bare := systemBlocks(t, true, agentmodel.ChatRequest{Messages: cacheKeyMessages})
	if cc := lastCacheControl(blocks); cc != nil {
		t.Errorf("api_key without prompt_cache_key must not cache, got cache_control %+v", cc)
	}
	if hasIdentity(blocks, bare) {
		t.Errorf("api_key request must not carry the Claude Code identity block; system = %v", blocks)
	}
}

// TestPromptCacheKey_SubscriptionUnchangedWithoutKey is the regression guard on
// the compatibility promise: subscription clients that have never heard of
// prompt_cache_key must keep the caching they have today. The local deployment
// is subscription, so a key-gated rewrite that forgot this would silently drop
// every existing client's cache hits.
func TestPromptCacheKey_SubscriptionUnchangedWithoutKey(t *testing.T) {
	blocks, bare := systemBlocks(t, false, agentmodel.ChatRequest{Messages: cacheKeyMessages})
	if cc := lastCacheControl(blocks); cc == nil {
		t.Errorf("subscription without a key must still cache; system = %v / %q", blocks, bare)
	}
	if !hasIdentity(blocks, bare) {
		t.Errorf("subscription request must carry the Claude Code identity block; system = %v", blocks)
	}
}

// TestPromptCacheKey_RespectsBreakpointCap confirms the key does not buy a way
// past Anthropic's four-marker limit: a client that already spent them all gets
// no injected breakpoint, key or no key, because a fifth marker 400s.
func TestPromptCacheKey_RespectsBreakpointCap(t *testing.T) {
	msgs := []agentmodel.Message{{Role: "system", Content: "stable system prompt"}}
	for i := 0; i < maxCacheBreakpoints; i++ {
		msgs = append(msgs, agentmodel.Message{Role: "user", Content: "turn", CacheControl: map[string]string{"type": "ephemeral"}})
	}
	blocks, _ := systemBlocks(t, true, agentmodel.ChatRequest{Messages: msgs, PromptCacheKey: "session-abc"})
	if cc := lastCacheControl(blocks); cc != nil {
		t.Errorf("breakpoint cap must hold even with a cache key, got cache_control %+v", cc)
	}
}

// TestPromptCacheKey_PreservesClientPlacedMarker checks we never move a
// breakpoint the client chose. Overwriting it would relocate their marker
// without changing the count — no error, just a worse cache than they asked for.
func TestPromptCacheKey_PreservesClientPlacedMarker(t *testing.T) {
	blocks, _ := systemBlocks(t, true, agentmodel.ChatRequest{
		PromptCacheKey: "session-abc",
		Messages: []agentmodel.Message{
			{Role: "system", Content: "first", CacheControl: map[string]string{"type": "ephemeral", "ttl": "1h"}},
			{Role: "system", Content: "second"},
			{Role: "user", Content: "hi"},
		},
	})
	if len(blocks) < 2 {
		t.Fatalf("expected the two client system blocks, got %v", blocks)
	}
	cc, _ := blocks[0]["cache_control"].(map[string]any)
	if cc == nil || cc["ttl"] != "1h" {
		t.Errorf("client's own marker was not preserved: %+v", blocks[0])
	}
}
