package api

import (
	"crypto/tls"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
	"time"
)

func TestParseUsageWindowBranches(t *testing.T) {
	now := time.Unix(2000000000, 0).UTC()

	start, end, ok := parseUsageWindow(url.Values{}, now)
	if !ok || !end.Equal(now) || end.Sub(start) != usageDefaultSince {
		t.Fatalf("default window = (%s, %s, %v), want default since ending now", start, end, ok)
	}

	start, end, ok = parseUsageWindow(url.Values{
		"start": {"100"},
		"end":   {"200"},
	}, now)
	if !ok || start.Unix() != 100 || end.Unix() != 200 {
		t.Fatalf("explicit window = (%s, %s, %v), want 100..200", start, end, ok)
	}

	start, end, ok = parseUsageWindow(url.Values{
		"end":   {"200"},
		"since": {"1h"},
	}, now)
	if !ok || start.Unix() != 200-3600 || end.Unix() != 200 {
		t.Fatalf("since window = (%s, %s, %v), want 1h before explicit end", start, end, ok)
	}

	start, end, ok = parseUsageWindow(url.Values{
		"start": {"0"},
		"end":   {"2000000000"},
	}, now)
	if !ok || end.Sub(start) != usageMaxWindow {
		t.Fatalf("clamped window = (%s, %s, %v), want max span %s", start, end, ok, usageMaxWindow)
	}

	for _, q := range []url.Values{
		{"start": {"bad"}},
		{"end": {"bad"}},
		{"start": {"200"}, "end": {"100"}},
		{"since": {"forever"}},
	} {
		if _, _, ok := parseUsageWindow(q, now); ok {
			t.Fatalf("parseUsageWindow(%v) ok, want false", q)
		}
	}
}

func TestReplicatePureHelpers(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest("GET", "http://gateway.test/v1/predictions/p", nil)
	if got := s.gatewayBaseURL(req); got != "http://gateway.test" {
		t.Fatalf("gatewayBaseURL http = %q", got)
	}
	req.Header.Set("X-Forwarded-Proto", "https")
	if got := s.gatewayBaseURL(req); got != "https://gateway.test" {
		t.Fatalf("gatewayBaseURL forwarded = %q", got)
	}
	req.Header.Del("X-Forwarded-Proto")
	req.TLS = &tls.ConnectionState{}
	if got := s.gatewayBaseURL(req); got != "https://gateway.test" {
		t.Fatalf("gatewayBaseURL tls = %q", got)
	}

	for _, body := range [][]byte{
		[]byte("not-json"),
		[]byte(`{"id":"p"}`),
		[]byte(`{"urls":"not-object"}`),
		[]byte(`{"urls":{"stream":"https://stream.replicate.com/x"}}`),
	} {
		if got := rewritePredictionURLs(body, "https://gw"); string(got) != string(body) {
			t.Fatalf("rewritePredictionURLs(%s) = %s, want unchanged", body, got)
		}
	}

	r := httptest.NewRequest("POST", "http://gateway.test/v1/predictions", nil)
	if got := replicateRequestedModel(r, []byte(`{"model":"owner/model","version":"abc"}`)); got != "owner/model" {
		t.Fatalf("replicateRequestedModel model = %q", got)
	}
	if got := replicateRequestedModel(r, []byte(`{"version":"abc"}`)); got != "abc" {
		t.Fatalf("replicateRequestedModel version = %q", got)
	}
	if got := replicateRequestedModel(r, []byte(`{`)); got != "" {
		t.Fatalf("replicateRequestedModel malformed = %q, want empty", got)
	}
	if got := replicateUsedModel([]byte(`{"model":"used/model"}`), "fallback"); got != "used/model" {
		t.Fatalf("replicateUsedModel = %q", got)
	}
	if got := replicateUsedModel([]byte(`{`), "fallback"); got != "fallback" {
		t.Fatalf("replicateUsedModel malformed = %q", got)
	}
	if status, errType, src := replicateOutcome(201); status != statusOk || errType != "" || src != "unpriced" {
		t.Fatalf("replicateOutcome(201) = (%q,%q,%q)", status, errType, src)
	}
	if status, errType, src := replicateOutcome(500); status != statusError || errType == "" || src != "" {
		t.Fatalf("replicateOutcome(500) = (%q,%q,%q)", status, errType, src)
	}
}

func TestAnthropicMessageAssemblerBranches(t *testing.T) {
	a := newAnthropicMessageAssembler()
	a.feed("", "")
	a.feed("message_start", "{")
	if out := a.finalize(); out != nil {
		t.Fatalf("finalize without message = %s, want nil", out)
	}

	a.feed("message_start", `{"message":{"id":"m1","type":"message","role":"assistant","usage":{"input_tokens":1}}}`)
	a.feed("content_block_delta", `{"index":9,"delta":{"type":"text_delta","text":"lost"}}`)
	a.feed("content_block_start", `{"index":1,"content_block":{"type":"tool_use","id":"tool1","name":"lookup"}}`)
	a.feed("content_block_delta", `{"index":1,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"hi\"}"}}`)
	a.feed("content_block_start", `{"index":0,"content_block":{"type":"text"}}`)
	a.feed("content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"hello"}}`)
	a.feed("content_block_start", `{"index":2,"content_block":{"type":"thinking"}}`)
	a.feed("content_block_delta", `{"index":2,"delta":{"type":"thinking_delta","thinking":"chain"}}`)
	a.feed("content_block_delta", `{"index":2,"delta":{"type":"signature_delta","signature":"sig"}}`)
	a.feed("message_delta", `{"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`)
	a.feed("done", "[DONE]")

	var got map[string]any
	if err := json.Unmarshal(a.finalize(), &got); err != nil {
		t.Fatalf("decode assembled message: %v", err)
	}
	if got["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %v", got["stop_reason"])
	}
	content, ok := got["content"].([]any)
	if !ok || len(content) != 3 {
		t.Fatalf("content = %#v, want three blocks", got["content"])
	}
	text := content[1].(map[string]any)
	if text["text"] != "hello" {
		t.Fatalf("text block = %#v", text)
	}
	tool := content[0].(map[string]any)
	if !reflect.DeepEqual(tool["input"], map[string]any{"q": "hi"}) {
		t.Fatalf("tool input = %#v", tool["input"])
	}
	thinking := content[2].(map[string]any)
	if thinking["thinking"] != "chain" || thinking["signature"] != "sig" {
		t.Fatalf("thinking block = %#v", thinking)
	}

	b := newAnthropicMessageAssembler()
	b.feed("message_start", `{"message":{"id":"m2"}}`)
	b.feed("content_block_start", `{"index":0,"content_block":{"type":"tool_use"}}`)
	b.feed("content_block_delta", `{"index":0,"delta":{"type":"input_json_delta","partial_json":"not-json"}}`)
	var fallback map[string]any
	if err := json.Unmarshal(b.finalize(), &fallback); err != nil {
		t.Fatalf("decode fallback message: %v", err)
	}
	block := fallback["content"].([]any)[0].(map[string]any)
	if block["input"] != "not-json" {
		t.Fatalf("invalid partial input = %#v, want raw string", block["input"])
	}
}
