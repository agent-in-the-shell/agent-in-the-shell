package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/chatgpt"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
)

func TestImageGenerations_ChatGPTReasoningUsagePersistence(t *testing.T) {
	for _, value := range []string{"missing", "0", "7"} {
		t.Run(value, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				detail := ""
				if value != "missing" {
					detail = `,"output_tokens_details":{"reasoning_tokens":` + value + `}`
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: response.output_item.done\ndata: "+`{"type":"response.output_item.done","item":{"type":"image_generation_call","result":"base64-png-bytes"}}`+"\n\n")
				_, _ = io.WriteString(w, "event: response.completed\ndata: "+`{"type":"response.completed","response":{"output":[],"usage":{"input_tokens":20,"output_tokens":10,"total_tokens":30,"input_tokens_details":{"cached_tokens":4}`+detail+`},"tools":[{"model":"gpt-image-2-codex"}]}}`+"\n\n")
			}))
			defer upstream.Close()

			p := chatgpt.NewWithBaseURL(&auth.StaticKey{HeaderName: "Authorization", Prefix: "Bearer ", Token: "test"}, upstream.URL)
			gateway, st := newTestServer(t, map[string][]router.Deployment{
				"image": {{Name: "chatgpt/gpt-5.5", Provider: p, Model: "gpt-5.5", Weight: 1}},
			})
			resp := mustPost(t, gateway, "/v1/images/generations", agentmodel.ImageRequest{Model: "image", Prompt: "a red circle"}, testToken)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			var out agentmodel.ImageResponse
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				t.Fatal(err)
			}
			if out.Model != "gpt-5.5" || len(out.Data) != 1 || out.Data[0].B64JSON != "base64-png-bytes" {
				t.Fatalf("unexpected image response: %+v", out)
			}

			// Image success logging is synchronous, before the HTTP response is written.
			logs, err := st.ListByOrg(context.Background(), "default", time.Time{}, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(logs) != 1 {
				t.Fatalf("logs = %v, want one success log", logs)
			}
			log := logs[0]
			assertReasoning(t, log.ReasoningTokens, value)
			if log.PromptTokens != 20 || log.CompletionTokens != 10 || log.TotalTokens != 30 || log.CacheReadInputTokens != 4 {
				t.Errorf("log counts changed: %+v", log)
			}
			if log.Status != "ok" || log.ModelUsed != "gpt-5.5" || log.AuthMode != string(agentmodel.AuthModeSubscription) || log.CostUSD != 0 {
				t.Errorf("unexpected success log: %+v", log)
			}
		})
	}
}
