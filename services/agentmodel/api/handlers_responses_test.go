package api_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/chatgpt"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/stub"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
	storepkg "github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

func TestResponses_PreservesHostedToolsSSEAndSeparatesAuth(t *testing.T) {
	const sse = "event: response.output_item.added\n" +
		"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"web_search_call\",\"id\":\"ws_1\"}}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"sunny\",\"annotations\":[{\"type\":\"url_citation\",\"url\":\"https://example.test/weather\"}]}]}],\"usage\":{\"input_tokens\":4,\"output_tokens\":2,\"total_tokens\":6}}}\n\n"

	var got map[string]json.RawMessage
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if value := r.Header.Get("Authorization"); value != "Bearer upstream-oauth" {
			t.Errorf("upstream Authorization = %q", value)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	defer upstream.Close()

	client := chatgpt.NewWithBaseURL(&auth.StaticKey{HeaderName: "Authorization", Prefix: "Bearer ", Token: "upstream-oauth"}, upstream.URL)
	gateway, store := newTestServer(t, map[string][]router.Deployment{
		"search-model": {{Name: "chatgpt/gpt-real|subscription", Provider: client, Model: "gpt-real", Weight: 1}},
	})
	resp := mustPost(t, gateway, "/v1/responses", map[string]any{
		"model":  "search-model",
		"input":  "weather",
		"stream": true,
		"tools":  []map[string]any{{"type": "web_search"}},
	}, testToken)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if string(body) != sse {
		t.Fatalf("SSE changed\ngot:  %q\nwant: %q", body, sse)
	}
	var model string
	if err := json.Unmarshal(got["model"], &model); err != nil || model != "gpt-real" {
		t.Fatalf("rewritten model = %q, err = %v", model, err)
	}
	if !bytes.Equal(got["tools"], []byte(`[{"type":"web_search"}]`)) {
		t.Fatalf("tools changed: %s", got["tools"])
	}
	if string(got["stream"]) != "true" {
		t.Fatalf("stream = %s", got["stream"])
	}

	var logs []storepkg.RequestLog
	var err error
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		logs, err = store.ListByOrg(context.Background(), "default", time.Now().Add(-time.Minute), 10)
		if err != nil || len(logs) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || len(logs) != 1 {
		t.Fatalf("audit logs = %d, err = %v", len(logs), err)
	}
	if logs[0].ModelRequested != "search-model" || logs[0].ModelUsed != "gpt-real" || logs[0].TotalTokens != 6 || logs[0].Provider != "chatgpt" {
		t.Fatalf("audit log = %+v", logs[0])
	}
}

func TestResponses_CompletedIncludesCollectedOutputItems(t *testing.T) {
	const item0 = `{"type":"web_search_call","id":"ws_1","status":"completed"}`
	const item1 = `{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"Hello!"}],"phase":"final_answer"}`
	// Deliberately deliver done events out of order: output_index, not arrival
	// order, defines the final response.output sequence.
	const sse = "event: response.output_item.done\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":" + item1 + "}\n\n" +
		"event: response.output_item.done\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":" + item0 + "}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Production ChatGPT currently mislabels its SSE body as text/plain.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, sse)
	}))
	defer upstream.Close()
	client := chatgpt.NewWithBaseURL(&auth.StaticKey{HeaderName: "Authorization", Prefix: "Bearer ", Token: "oauth"}, upstream.URL)
	gateway, _ := newTestServer(t, map[string][]router.Deployment{
		"search": {{Name: "chatgpt/gpt|subscription", Provider: client, Model: "gpt", Weight: 1}},
	})

	resp := mustPost(t, gateway, "/v1/responses", map[string]any{"model": "search", "input": "hi", "stream": true}, testToken)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	var completed struct {
		Type     string `json:"type"`
		Response struct {
			ID     string            `json:"id"`
			Output []json.RawMessage `json:"output"`
		} `json:"response"`
	}
	for _, event := range strings.Split(string(body), "\n\n") {
		for _, line := range strings.Split(event, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var envelope struct {
				Type string `json:"type"`
			}
			payload := strings.TrimPrefix(line, "data: ")
			if json.Unmarshal([]byte(payload), &envelope) == nil && envelope.Type == "response.completed" {
				if err := json.Unmarshal([]byte(payload), &completed); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if completed.Response.ID != "resp_1" || len(completed.Response.Output) != 2 {
		t.Fatalf("completed response = %+v; stream = %s", completed.Response, body)
	}
	for i, want := range [][]byte{[]byte(item0), []byte(item1)} {
		if !bytes.Equal(completed.Response.Output[i], want) {
			t.Fatalf("terminal output[%d] = %s, want %s", i, completed.Response.Output[i], want)
		}
	}
}

func TestResponses_NonStreamingAggregatesSSEAndDefaultsStoreFalse(t *testing.T) {
	const item = `{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"Hello!"}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if string(request["stream"]) != "true" || string(request["store"]) != "false" {
			t.Errorf("upstream request stream=%s store=%s", request["stream"], request["store"])
		}
		if _, ok := request["max_output_tokens"]; ok {
			t.Error("unsupported max_output_tokens was forwarded upstream")
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "event: response.output_item.done\n"+
			"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":"+item+"}\n\n"+
			"event: response.completed\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n")
	}))
	defer upstream.Close()
	client := chatgpt.NewWithBaseURL(&auth.StaticKey{HeaderName: "Authorization", Prefix: "Bearer ", Token: "oauth"}, upstream.URL)
	gateway, _ := newTestServer(t, map[string][]router.Deployment{
		"search": {{Name: "chatgpt/gpt|subscription", Provider: client, Model: "gpt", Weight: 1}},
	})

	resp := mustPost(t, gateway, "/v1/responses", map[string]any{"model": "search", "input": "hi", "max_output_tokens": 32}, testToken)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("status = %d, content-type = %q, body = %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	var response struct {
		ID     string            `json:"id"`
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	if response.ID != "resp_1" || len(response.Output) != 1 || !bytes.Equal(response.Output[0], []byte(item)) {
		t.Fatalf("response = %+v, body = %s", response, body)
	}
}

func TestResponses_StreamTerminalStateAndTransparency(t *testing.T) {
	completed := "event: response.completed\r\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":4,\"output_tokens\":2,\"total_tokens\":6}}}\r\n\r\n"
	usageWithoutTerminal := "event: response.output_text.done\n" +
		"data: {\"type\":\"response.output_text.done\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n"
	incomplete := "data: {\"type\":\"response.incomplete\",\"response\":{\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n"
	failed := "data: {\"type\":\"response.failed\",\"response\":{\"usage\":{\"input_tokens\":2,\"output_tokens\":2,\"total_tokens\":4}}}\n\n"
	transparent := ": comment\r\nevent: future.event\r\ndata: {\"type\":\"future.event\",\"value\":1}\r\n\r\n" + completed
	overLimit := "data: " + strings.Repeat("x", maxTestResponsesSSELine+1) + "\n\n" + completed

	for _, tc := range []struct {
		name       string
		chunks     []string
		wantStatus string
		wantTokens int
	}{
		{name: "completed terminal", chunks: []string{completed}, wantStatus: "ok", wantTokens: 6},
		{name: "clean EOF before terminal", chunks: []string{usageWithoutTerminal}, wantStatus: "error", wantTokens: 5},
		{name: "incomplete terminal", chunks: []string{incomplete}, wantStatus: "error", wantTokens: 3},
		{name: "failed terminal", chunks: []string{failed}, wantStatus: "error", wantTokens: 4},
		{name: "CRLF comments unknown events and chunk boundaries", chunks: []string{transparent[:1], transparent[1:17], transparent[17:49], transparent[49:]}, wantStatus: "ok", wantTokens: 6},
		{name: "over-limit line", chunks: []string{overLimit[:1024], overLimit[1024:]}, wantStatus: "ok", wantTokens: 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				for _, chunk := range tc.chunks {
					_, _ = io.WriteString(w, chunk)
					w.(http.Flusher).Flush()
				}
			}))
			defer upstream.Close()

			client := chatgpt.NewWithBaseURL(&auth.StaticKey{HeaderName: "Authorization", Prefix: "Bearer ", Token: "oauth"}, upstream.URL)
			gateway, store := newTestServer(t, map[string][]router.Deployment{
				"search": {{Name: "chatgpt/gpt|subscription", Provider: client, Model: "gpt", Weight: 1}},
			})
			resp := mustPost(t, gateway, "/v1/responses", map[string]any{"model": "search", "input": "hi", "stream": true}, testToken)
			got, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			want := strings.Join(tc.chunks, "")
			if string(got) != want {
				t.Fatalf("SSE changed: got %d bytes, want %d", len(got), len(want))
			}

			logs, err := store.ListByOrg(context.Background(), "default", time.Now().Add(-time.Minute), 10)
			if err != nil || len(logs) != 1 {
				t.Fatalf("audit logs = %d, err = %v", len(logs), err)
			}
			if logs[0].Status != tc.wantStatus || logs[0].TotalTokens != tc.wantTokens {
				t.Fatalf("audit log status/tokens = %q/%d, want %q/%d", logs[0].Status, logs[0].TotalTokens, tc.wantStatus, tc.wantTokens)
			}
			if tc.wantStatus == "error" && logs[0].ErrorType == "" {
				t.Fatal("error terminal state has empty error type")
			}
		})
	}
}

const maxTestResponsesSSELine = 8 << 20

func TestResponses_UnsupportedProviderIsClear(t *testing.T) {
	gateway, _ := newTestServer(t, map[string][]router.Deployment{
		"plain": {{Name: "openai/plain|api_key", Provider: &stub.Stub{NameValue: "openai"}, Model: "plain-upstream", Weight: 1}},
	})
	resp := mustPost(t, gateway, "/v1/responses", map[string]any{"model": "plain", "input": "hi", "stream": true}, testToken)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), `no Responses-capable deployment`) {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
}

func TestResponses_ValidationErrors(t *testing.T) {
	gateway, _ := newTestServer(t, nil)
	for _, tc := range []struct {
		name, body, want string
	}{
		{"invalid JSON", `{`, "invalid_json"},
		{"missing model", `{"input":"hi","stream":true}`, "missing_required_parameter"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest("POST", gateway.URL+"/v1/responses", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+testToken)
			resp, err := gateway.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), tc.want) {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
			}
		})
	}
}

func TestResponses_ClientCancellationCancelsUpstream(t *testing.T) {
	cancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		flusher.Flush()
		<-r.Context().Done()
		close(cancelled)
	}))
	defer upstream.Close()
	client := chatgpt.NewWithBaseURL(&auth.StaticKey{HeaderName: "Authorization", Prefix: "Bearer ", Token: "oauth"}, upstream.URL)
	gateway, _ := newTestServer(t, map[string][]router.Deployment{
		"search": {{Name: "chatgpt/gpt|subscription", Provider: client, Model: "gpt", Weight: 1}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	body := strings.NewReader(`{"model":"search","input":"hi","stream":true,"tools":[{"type":"web_search"}]}`)
	req, _ := http.NewRequestWithContext(ctx, "POST", gateway.URL+"/v1/responses", body)
	req.Close = true
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := gateway.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(resp.Body)
	if !scanner.Scan() {
		t.Fatal("did not receive initial event")
	}
	cancel()
	_ = resp.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request was not cancelled")
	}
}
