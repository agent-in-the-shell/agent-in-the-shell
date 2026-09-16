package api

import "testing"

func TestReasoningResponsesPartialUsage(t *testing.T) {
	p := newResponsesStreamParser()
	for _, raw := range []string{
		`{"type":"response.in_progress","response":{"usage":{"input_tokens":20,"output_tokens":10,"total_tokens":30,"input_tokens_details":{"cached_tokens":4},"output_tokens_details":{"reasoning_tokens":7}}}}`,
		`{"type":"response.in_progress","response":{"usage":{"output_tokens_details":{"reasoning_tokens":0}}}}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":20,"output_tokens":10,"total_tokens":30,"input_tokens_details":{"cached_tokens":4}}}}`,
	} {
		_, _ = p.Write([]byte("data: " + raw + "\n\n"))
	}
	summary := p.finish()
	u := summary.usage
	if summary.status != statusOk || u.ReasoningTokens == nil || *u.ReasoningTokens != 0 || u.TotalTokens != 30 || u.CompletionTokens != 10 || u.CacheReadInputTokens != 4 {
		t.Fatalf("summary: %+v, usage: %+v", summary, u)
	}
	// A detail-only event must not erase the inclusive totals already observed.
	_, _ = p.Write([]byte("data: " + `{"response":{"usage":{"output_tokens_details":{"reasoning_tokens":7}}}}` + "\n\n"))
	if u = p.finish().usage; u.TotalTokens != 30 || u.ReasoningTokens == nil || *u.ReasoningTokens != 7 {
		t.Fatalf("partial: %+v", u)
	}
}

func TestReasoningMessagesPartialUsage(t *testing.T) {
	var usage anthropicUsageBlock
	updateUsageFromEvent("message_start", `{"message":{"usage":{"input_tokens":14,"cache_read_input_tokens":4,"cache_creation_input_tokens":2,"output_tokens_details":{"thinking_tokens":7}}}}`, &usage)
	updateUsageFromEvent("message_delta", `{"usage":{"output_tokens":10,"output_tokens_details":{"thinking_tokens":0}}}`, &usage)
	updateUsageFromEvent("message_delta", `{"usage":{"output_tokens":10}}`, &usage)
	u := usageBlockToAgentmodel(usage)
	if u.ReasoningTokens == nil || *u.ReasoningTokens != 0 || u.PromptTokens != 20 || u.CompletionTokens != 10 || u.TotalTokens != 30 || u.CacheReadInputTokens != 4 || u.CacheCreationInputTokens != 2 {
		t.Fatalf("merged usage: %+v", u)
	}
}
