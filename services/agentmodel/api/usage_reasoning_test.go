package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/anthropic"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/azure"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/chatgpt"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/deepseek"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/gemini"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/messagesbridge"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/openai"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/openaicompat"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/stub"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
)

// Real adapters and the HTTP audit writer share these fixtures: no stub can
// accidentally make the test pass by bypassing an upstream usage conversion.
func TestReasoningUsageCollection(t *testing.T) {
	key := &auth.StaticKey{HeaderName: "Authorization", Prefix: "Bearer ", Token: "test"}
	adapters := []struct {
		name, shape string
		new         func(string) provider.Provider
	}{
		{"openai", "chat", func(url string) provider.Provider { return openai.NewWithBaseURL(key, url) }},
		{"azure", "chat", func(url string) provider.Provider { return azure.New(key, url, "2024-10-01-preview", "model") }},
		{"deepseek", "chat", func(url string) provider.Provider { return deepseek.NewWithBaseURL(key, url) }},
		{"compat", "chat", func(url string) provider.Provider { return openaicompat.New("compat", url, key) }},
		{"chatgpt", "responses", func(url string) provider.Provider { return chatgpt.NewWithBaseURL(key, url) }},
		{"anthropic", "messages", func(url string) provider.Provider { return anthropic.NewWithBaseURL(key, url) }},
		{"gemini", "gemini", func(url string) provider.Provider { return gemini.NewWithBaseURL(key, url) }},
	}
	for _, a := range adapters {
		for _, value := range []string{"missing", "0", "7"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", a.name, value, stream), func(t *testing.T) {
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						var req struct {
							Stream bool `json:"stream"`
						}
						_ = json.NewDecoder(r.Body).Decode(&req)
						isStream := req.Stream || strings.Contains(r.URL.Path, "streamGenerateContent")
						detail := ""
						if value != "missing" {
							detail = `,"completion_tokens_details":{"reasoning_tokens":` + value + `}`
						}
						usage := `{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30,"prompt_tokens_details":{"cached_tokens":4}` + detail + `}`
						body := `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":` + usage + `}`
						sse := "data: " + body + "\n\ndata: [DONE]\n\n"
						switch a.shape {
						case "responses":
							detail = ""
							if value != "missing" {
								detail = `,"output_tokens_details":{"reasoning_tokens":` + value + `}`
							}
							usage = `{"input_tokens":20,"output_tokens":10,"total_tokens":30,"input_tokens_details":{"cached_tokens":4}` + detail + `}`
							sse = "event: response.completed\ndata: " + `{"type":"response.completed","response":{"usage":` + usage + `}}` + "\n\n"
						case "messages":
							detail = ""
							if value != "missing" {
								detail = `,"output_tokens_details":{"thinking_tokens":` + value + `}`
							}
							usage = `{"input_tokens":16,"output_tokens":10,"cache_read_input_tokens":4` + detail + `}`
							body = `{"type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":` + usage + `}`
							sse = "event: message_start\ndata: " + `{"message":{"role":"assistant","usage":{"input_tokens":16,"cache_read_input_tokens":4}}}` + "\n\nevent: message_delta\ndata: " + `{"delta":{"stop_reason":"end_turn"},"usage":` + usage + `}` + "\n\nevent: message_stop\ndata: {}\n\n"
						case "gemini":
							detail = ""
							if value != "missing" {
								detail = `,"thoughtsTokenCount":` + value
							}
							usage = `{"promptTokenCount":20,"candidatesTokenCount":10,"totalTokenCount":30,"cachedContentTokenCount":4` + detail + `}`
							body = `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":` + usage + `}`
							sse = "data: " + body + "\n\n"
						}
						if isStream {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, sse)
						} else {
							w.Header().Set("Content-Type", "application/json")
							_, _ = io.WriteString(w, body)
						}
					}))
					defer upstream.Close()
					p := a.new(upstream.URL)
					req := agentmodel.ChatRequest{Model: "model", Messages: []agentmodel.Message{{Role: "user", Content: "hello"}}}
					var usage agentmodel.Usage
					if stream {
						seq, err := p.Stream(context.Background(), req)
						if err != nil {
							t.Fatal(err)
						}
						for chunk, err := range seq {
							if err != nil {
								t.Fatal(err)
							}
							if chunk.Usage != nil {
								usage = *chunk.Usage
							}
						}
					} else {
						resp, err := p.Complete(context.Background(), req)
						if err != nil {
							t.Fatal(err)
						}
						usage = resp.Usage
					}
					encoded, err := json.Marshal(usage)
					if err != nil {
						t.Fatal(err)
					}
					var decoded struct {
						Details struct {
							Reasoning *int `json:"reasoning_tokens"`
						} `json:"completion_tokens_details"`
					}
					_ = json.Unmarshal(encoded, &decoded)
					assertReasoning(t, decoded.Details.Reasoning, value)
					if usage.PromptTokens != 20 || usage.CompletionTokens != 10 || usage.TotalTokens != 30 || usage.CacheReadInputTokens != 4 {
						t.Fatalf("counts changed: %+v", usage)
					}
					endpoints := []string{"/v1/chat/completions", "/v1/messages"}
					if a.name == "chatgpt" {
						endpoints = append(endpoints, "/v1/responses")
					}
					for _, endpoint := range endpoints {
						t.Run(endpoint, func(t *testing.T) {
							serving := p
							if endpoint == "/v1/messages" && a.name != "anthropic" {
								serving = messagesbridge.New(p)
							}
							gateway, st := newTestServer(t, map[string][]router.Deployment{"model": {{Name: a.name + "/model", Provider: serving, Model: "model", Weight: 1}}})
							resp := mustPost(t, gateway, endpoint, map[string]any{"model": "model", "max_tokens": 100, "input": "hello", "messages": []map[string]string{{"role": "user", "content": "hello"}}, "stream": stream}, testToken)
							_, _ = io.Copy(io.Discard, resp.Body)
							_ = resp.Body.Close()
							if resp.StatusCode != 200 {
								t.Fatalf("status %d", resp.StatusCode)
							}
							// Streaming audit persistence completes just after the final bytes flush.
							deadline := time.Now().Add(time.Second)
							for {
								logs, err := st.ListByOrg(context.Background(), "default", time.Time{}, 10)
								if err != nil {
									t.Fatal(err)
								}
								if len(logs) == 1 {
									assertReasoning(t, logs[0].ReasoningTokens, value)
									if logs[0].TotalTokens != 30 || logs[0].CompletionTokens != 10 || logs[0].CacheReadInputTokens != 4 {
										t.Fatalf("log counts changed: %+v", logs[0])
									}
									break
								}
								if time.Now().After(deadline) {
									t.Fatalf("logs: %v", logs)
								}
								time.Sleep(time.Millisecond)
							}
						})
					}
				})
			}
		}
	}
}

func TestReasoningStreamSnapshots(t *testing.T) {
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/messages"} {
		t.Run(endpoint, func(t *testing.T) {
			positive, zero := 7, 0
			p := &stub.Stub{NameValue: "openai", StreamChunks: []provider.StreamChunk{
				{Usage: &agentmodel.Usage{PromptTokens: 20, CompletionTokens: 8, TotalTokens: 28, ReasoningTokens: &positive}},
				{Usage: &agentmodel.Usage{PromptTokens: 20, CompletionTokens: 9, TotalTokens: 29, ReasoningTokens: &zero}},
				{FinishReason: "stop", Usage: &agentmodel.Usage{PromptTokens: 20, CompletionTokens: 10, TotalTokens: 30}},
			}}
			var serving provider.Provider = p
			if endpoint == "/v1/messages" {
				serving = messagesbridge.New(p)
			}
			gateway, st := newTestServer(t, map[string][]router.Deployment{"model": {{Name: "openai/model", Provider: serving, Model: "model", Weight: 1}}})
			resp := mustPost(t, gateway, endpoint, map[string]any{"model": "model", "messages": []map[string]string{{"role": "user", "content": "hello"}}, "stream": true, "max_tokens": 100}, testToken)
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("status %d", resp.StatusCode)
			}
			deadline := time.Now().Add(time.Second)
			for {
				logs, err := st.ListByOrg(context.Background(), "default", time.Time{}, 10)
				if err != nil {
					t.Fatal(err)
				}
				if len(logs) == 1 {
					assertReasoning(t, logs[0].ReasoningTokens, "0")
					if logs[0].TotalTokens != 30 {
						t.Fatalf("summed snapshots: %+v", logs[0])
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("logs: %v", logs)
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

func assertReasoning(t *testing.T, got *int, value string) {
	t.Helper()
	if value == "missing" {
		if got != nil {
			t.Fatalf("missing became %d", *got)
		}
		return
	}
	if got == nil || fmt.Sprint(*got) != value {
		t.Fatalf("reasoning = %v, want %s", got, value)
	}
}
