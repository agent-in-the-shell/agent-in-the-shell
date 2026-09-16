package wire

import (
	"encoding/json"
	"testing"
)

func TestUsageReasoningRoundTrip(t *testing.T) {
	for _, detail := range []string{"", `,"completion_tokens_details":{}`, `,"completion_tokens_details":{"reasoning_tokens":null}`, `,"completion_tokens_details":{"reasoning_tokens":0}`, `,"completion_tokens_details":{"reasoning_tokens":7}`} {
		t.Run(detail, func(t *testing.T) {
			raw := `{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30,"cache_read_input_tokens":4,"cache_creation_input_tokens":2` + detail + `}`
			var u Usage
			if err := json.Unmarshal([]byte(raw), &u); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(u)
			if err != nil {
				t.Fatal(err)
			}
			var got, want map[string]json.RawMessage
			_ = json.Unmarshal(encoded, &got)
			_ = json.Unmarshal([]byte(raw), &want)
			var expected map[string]json.RawMessage
			_ = json.Unmarshal(want["completion_tokens_details"], &expected)
			if v := string(expected["reasoning_tokens"]); v == "0" || v == "7" {
				if string(got["completion_tokens_details"]) != string(want["completion_tokens_details"]) {
					t.Fatalf("reasoning detail lost: %s", encoded)
				}
			} else if _, ok := got["completion_tokens_details"]; ok {
				t.Fatalf("invented reasoning: %s", encoded)
			}
			var roundTrip Usage
			if err := json.Unmarshal(encoded, &roundTrip); err != nil {
				t.Fatal(err)
			}
			if (u.ReasoningTokens == nil) != (roundTrip.ReasoningTokens == nil) || (u.ReasoningTokens != nil && *u.ReasoningTokens != *roundTrip.ReasoningTokens) {
				t.Fatalf("round trip: %+v -> %+v", u, roundTrip)
			}
			if u.PromptTokens != 20 || u.CompletionTokens != 10 || u.TotalTokens != 30 || u.CacheReadInputTokens != 4 || u.CacheCreationInputTokens != 2 {
				t.Fatalf("counts changed: %+v", u)
			}
		})
	}
}

func TestUsageCacheDetailCompatibility(t *testing.T) {
	for _, raw := range []string{
		`{"prompt_tokens_details":{"cached_tokens":4,"cache_write_tokens":2}}`,
		`{"cache_read_input_tokens":4,"cache_creation_input_tokens":2}`,
		`{"cache_read_input_tokens":4,"cache_creation_input_tokens":2,"prompt_tokens_details":{"cached_tokens":8,"cache_write_tokens":9}}`,
	} {
		var u Usage
		if err := json.Unmarshal([]byte(raw), &u); err != nil {
			t.Fatal(err)
		}
		if u.CacheReadInputTokens != 4 || u.CacheCreationInputTokens != 2 {
			t.Fatalf("cache: %+v", u)
		}
	}
	var u Usage
	if err := json.Unmarshal([]byte(`{"cache_read_input_tokens":0,"prompt_tokens_details":{"cached_tokens":8}}`), &u); err != nil {
		t.Fatal(err)
	}
	if u.CacheReadInputTokens != 0 {
		t.Fatalf("explicit flat zero lost: %+v", u)
	}
}
