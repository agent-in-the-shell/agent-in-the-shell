package api

import (
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
)

func cacheKeyReq(key string) agentmodel.ChatRequest {
	return agentmodel.ChatRequest{
		Model:          "claude-3-5-sonnet-latest",
		Messages:       []agentmodel.Message{{Role: "user", Content: "same prompt"}},
		PromptCacheKey: key,
	}
}

// TestChatCacheKey_IgnoresPromptCacheKey guards a regression that only appears
// once clients start sending a per-session prompt_cache_key.
//
// The field is metadata for the upstream (route these together, this prefix
// recurs), not an output determinant: the same prompt returns the same answer
// whatever key rides along. It is also per-session by construction, so leaving
// it in the hash would give every session its own private response cache and
// two identical prompts would stop finding each other. The gateway's hit rate
// would fall as adoption rose, which is a hard shape of bug to attribute after
// the fact.
func TestChatCacheKey_IgnoresPromptCacheKey(t *testing.T) {
	a := chatCacheKey(cacheKeyReq("session-a"))
	b := chatCacheKey(cacheKeyReq("session-b"))
	none := chatCacheKey(cacheKeyReq(""))

	if a != b {
		t.Errorf("two sessions sending the same prompt got different cache keys:\n  %s\n  %s", a, b)
	}
	if a != none {
		t.Errorf("a keyed request and an unkeyed one with the same prompt got different cache keys:\n  %s\n  %s", a, none)
	}
}

// TestChatCacheKey_StillSeparatesPrompts is the control: zeroing
// PromptCacheKey must not have blunted the key into ignoring real content.
func TestChatCacheKey_StillSeparatesPrompts(t *testing.T) {
	a := cacheKeyReq("session-a")
	b := cacheKeyReq("session-a")
	b.Messages = []agentmodel.Message{{Role: "user", Content: "a different prompt"}}

	if chatCacheKey(a) == chatCacheKey(b) {
		t.Error("different prompts must not share a cache key")
	}
}
