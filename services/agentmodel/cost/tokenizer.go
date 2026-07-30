package cost

// EstimateTokens returns an approximate token count for text. Used pre-flight
// for budget estimation when an exact tokenizer for the provider is not
// available (Anthropic, Gemini). Algorithm: roughly 4 characters per token,
// matching the ballpark used by Anthropic's docs and LiteLLM's char estimator.
//
// For OpenAI / ChatGPT models, callers should prefer a real tokenizer
// (tiktoken-go) for precision; that is wave-2 work.
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	// 4 chars per token is a reasonable default across English-heavy corpora.
	// Multi-byte (CJK) text actually packs higher density per char; we keep
	// the simple heuristic and revisit when we add language-aware tokenization.
	return (len(text) + 3) / 4
}
