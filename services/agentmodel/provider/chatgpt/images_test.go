package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
)

// writeImageSSE writes the SSE event sequence a real chatgpt image
// generation call actually produces, confirmed against the live Codex
// backend (#1059): the base64 result arrives in a response.output_item.done
// event, and response.completed's own output[] is left EMPTY — only its
// usage/tools fields are read. An earlier version of this helper (and this
// package) assumed the image arrived inline in response.completed.output[],
// which does not happen in production; every test using this helper now
// exercises the real wire shape instead of that incorrect assumption.
func writeImageSSE(w http.ResponseWriter, result string, inputTok, outputTok, totalTok int) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "event: response.output_item.added\n")
	_, _ = io.WriteString(w, `data: {"type":"response.output_item.added","item":{"type":"image_generation_call","id":"ig_1","status":"in_progress"}}`+"\n\n")
	_, _ = io.WriteString(w, "event: response.output_item.done\n")
	itemDone, _ := json.Marshal(map[string]any{
		"type": "response.output_item.done",
		"item": map[string]any{"type": "image_generation_call", "id": "ig_1", "status": "completed", "result": result},
	})
	_, _ = io.WriteString(w, "data: "+string(itemDone)+"\n\n")
	_, _ = io.WriteString(w, "event: response.completed\n")
	body, _ := json.Marshal(map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"output": []map[string]any{}, // the real backend leaves this empty
			"usage": map[string]any{
				"input_tokens": inputTok, "output_tokens": outputTok, "total_tokens": totalTok,
			},
			"tools": []map[string]any{
				{"type": "image_generation", "model": "gpt-image-2-codex", "background": "auto", "size": "auto", "quality": "auto", "output_format": "png"},
			},
		},
	})
	_, _ = io.WriteString(w, "data: "+string(body)+"\n\n")
}

func TestGenerateImage_Success(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		writeImageSSE(w, "base64-png-bytes", 50, 229, 279)
	}))
	defer srv.Close()

	c := NewWithBaseURL(freshAuth(t), srv.URL)
	resp, err := c.GenerateImage(context.Background(), agentmodel.ImageRequest{
		Model:  "gpt-5.5",
		Prompt: "a red circle",
		N:      1, // non-zero so omitempty can't mask a regressed N pass-through (#1059)
	})
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if len(resp.Data) != 1 || resp.Data[0].B64JSON != "base64-png-bytes" {
		t.Fatalf("Data = %+v, want one item with B64JSON=base64-png-bytes", resp.Data)
	}
	if resp.Model != "gpt-image-2-codex" {
		t.Errorf("Model = %q, want gpt-image-2-codex", resp.Model)
	}
	if resp.Usage.TotalTokens != 279 || resp.Usage.PromptTokens != 50 || resp.Usage.CompletionTokens != 229 {
		t.Errorf("Usage = %+v, want prompt=50 completion=229 total=279", resp.Usage)
	}
	if resp.Usage.AuthMode != agentmodel.AuthModeSubscription {
		t.Errorf("Usage.AuthMode = %q, want subscription", resp.Usage.AuthMode)
	}

	var sent map[string]any
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("decode sent body: %v", err)
	}
	if sent["model"] != "gpt-5.5" {
		t.Errorf("sent model = %v, want gpt-5.5", sent["model"])
	}
	if sent["tool_choice"] != "auto" {
		t.Errorf("sent tool_choice = %v, want auto", sent["tool_choice"])
	}
	if sent["stream"] != true {
		t.Errorf("sent stream = %v, want true", sent["stream"])
	}
	if sent["store"] != false {
		t.Errorf("sent store = %v, want false", sent["store"])
	}
	tools, _ := sent["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("sent tools = %v, want one entry", sent["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "image_generation" || tool["background"] != "auto" {
		t.Errorf("sent tool = %v, want type=image_generation background=auto", tool)
	}
	if _, hasN := tool["n"]; hasN {
		t.Errorf("sent tool = %v, must not include \"n\" — the Codex backend rejects tools[0].n with 400 even for n:1 (#1059)", tool)
	}
	input, _ := sent["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("sent input = %v, want one message", sent["input"])
	}
	msg, _ := input[0].(map[string]any)
	if msg["role"] != "user" {
		t.Errorf("sent input[0].role = %v, want user", msg["role"])
	}
	content, _ := msg["content"].([]any)
	part, _ := content[0].(map[string]any)
	if text, _ := part["text"].(string); text != "Use the image_generation tool to generate an image: a red circle" {
		t.Errorf("sent input[0].content[0].text = %q", text)
	}
}

// TestGenerateImage_NoImageInOutput exercises the "found nothing anywhere"
// error branch: no response.output_item.done ever captured an image into
// pending, and response.completed.output[] also carries no image_generation_call
// (here, a text-only message item instead). The exact wire shape a real
// text-only refusal takes hasn't been live-verified (unlike the success path
// in #1059) — this only asserts the code's fallback-exhausted error path.
func TestGenerateImage_NoImageInOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"sorry, I can't do that"}]}],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}`+"\n\n")
	}))
	defer srv.Close()

	c := NewWithBaseURL(freshAuth(t), srv.URL)
	_, err := c.GenerateImage(context.Background(), agentmodel.ImageRequest{Model: "gpt-5.5", Prompt: "a red circle"})
	if err == nil {
		t.Fatal("GenerateImage: want error, got nil")
	}
	var ae *agentmodel.Error
	if !errors.As(err, &ae) || ae.Type != agentmodel.ErrTypeUpstream {
		t.Fatalf("err = %v, want *agentmodel.Error{Type: upstream_error}", err)
	}
}

func TestGenerateImage_UpstreamFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: response.failed\n")
		_, _ = io.WriteString(w, `data: {"type":"response.failed","response":{"error":{"message":"content policy violation","code":"content_policy"}}}`+"\n\n")
	}))
	defer srv.Close()

	c := NewWithBaseURL(freshAuth(t), srv.URL)
	_, err := c.GenerateImage(context.Background(), agentmodel.ImageRequest{Model: "gpt-5.5", Prompt: "a red circle"})
	if err == nil || !strings.Contains(err.Error(), "content policy violation") {
		t.Fatalf("err = %v, want to contain \"content policy violation\"", err)
	}
}

func TestGenerateImage_RetriesOnExpiredToken(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"Provided authentication token is expired.","code":"token_expired"}}`)
			return
		}
		writeImageSSE(w, "second-attempt-png", 1, 1, 2)
	}))
	defer srv.Close()

	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "refreshed-token", "refresh_token": "new-rt", "expires_in": 3600,
		})
	}))
	defer authSrv.Close()

	dir := t.TempDir()
	writeAuthFile(t, dir, map[string]any{
		"access_token": "stale", "refresh_token": "old-rt", "expires_at_ms": time.Now().Add(time.Hour).UnixMilli(),
	})
	a := auth.NewChatGPTOAuth(dir, &http.Client{Timeout: 5 * time.Second})
	a.OverrideURLs(authSrv.URL+"/devicecode", authSrv.URL+"/devicetoken", authSrv.URL+"/oauth/token")

	c := NewWithBaseURL(a, srv.URL)
	resp, err := c.GenerateImage(context.Background(), agentmodel.ImageRequest{Model: "gpt-5.5", Prompt: "a red circle"})
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if resp.Data[0].B64JSON != "second-attempt-png" {
		t.Errorf("B64JSON = %q, want second-attempt-png", resp.Data[0].B64JSON)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("api calls = %d, want 2 (401 then 200)", got)
	}
}

func TestGenerateImage_LargeImagePayload(t *testing.T) {
	// A base64 PNG can exceed the old 1MB SSE line cap for higher-quality
	// images; this asserts the larger buffer accepts it.
	large := strings.Repeat("A", 2*1024*1024) // 2MB base64 payload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeImageSSE(w, large, 1, 1, 2)
	}))
	defer srv.Close()

	c := NewWithBaseURL(freshAuth(t), srv.URL)
	resp, err := c.GenerateImage(context.Background(), agentmodel.ImageRequest{Model: "gpt-5.5", Prompt: "a red circle"})
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if len(resp.Data) != 1 || resp.Data[0].B64JSON != large {
		t.Fatalf("Data[0].B64JSON length = %d, want %d", len(resp.Data[0].B64JSON), len(large))
	}
}

// TestGenerateImage_MultipleImages_CompletedOutputFallback exercises the
// response.completed.output[] parsing path directly, independent of the
// response.output_item.done path real traffic actually uses (#1059 confirmed
// the live backend leaves output[] empty and rejects tools[0].n outright, so
// a real request can never produce more than one image — this is NOT a
// realistic response, just a unit-level check that readImageStream still
// collects every image_generation_call item from output[] if it's ever
// populated, rather than only the first, kept as defensive/forward-compatible
// parsing per images.go's own fallback design).
func TestGenerateImage_MultipleImages_CompletedOutputFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: response.completed\n")
		body, _ := json.Marshal(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"output": []map[string]any{
					{"type": "image_generation_call", "id": "ig_1", "status": "completed", "result": "image-one"},
					{"type": "image_generation_call", "id": "ig_2", "status": "completed", "result": "image-two"},
				},
				"usage": map[string]any{"input_tokens": 2, "output_tokens": 2, "total_tokens": 4},
			},
		})
		_, _ = io.WriteString(w, "data: "+string(body)+"\n\n")
	}))
	defer srv.Close()

	c := NewWithBaseURL(freshAuth(t), srv.URL)
	resp, err := c.GenerateImage(context.Background(), agentmodel.ImageRequest{Model: "gpt-5.5", Prompt: "two red circles"})
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("len(Data) = %d, want 2", len(resp.Data))
	}
	if resp.Data[0].B64JSON != "image-one" || resp.Data[1].B64JSON != "image-two" {
		t.Errorf("Data = %+v, want [image-one, image-two]", resp.Data)
	}
}

// TestGenerateImage_MultipleOutputItemDoneEvents exercises the real
// production event path (response.output_item.done, per #1059) with more
// than one such event before response.completed — verifying the pending
// accumulator collects every one of them, not just the first. A single
// current request can't actually trigger this (the backend rejects
// tools[0].n, so it never emits more than one image_generation_call), but
// this is the code path real multi-image support would need if a future
// backend version allows it, and it deserves its own coverage distinct from
// the legacy response.completed.output[] fallback above.
func TestGenerateImage_MultipleOutputItemDoneEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i, result := range []string{"item-done-one", "item-done-two"} {
			_, _ = io.WriteString(w, "event: response.output_item.done\n")
			itemDone, _ := json.Marshal(map[string]any{
				"type": "response.output_item.done",
				"item": map[string]any{"type": "image_generation_call", "id": fmt.Sprintf("ig_%d", i+1), "status": "completed", "result": result},
			})
			_, _ = io.WriteString(w, "data: "+string(itemDone)+"\n\n")
		}
		_, _ = io.WriteString(w, "event: response.completed\n")
		body, _ := json.Marshal(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"output": []map[string]any{},
				"usage":  map[string]any{"input_tokens": 2, "output_tokens": 2, "total_tokens": 4},
			},
		})
		_, _ = io.WriteString(w, "data: "+string(body)+"\n\n")
	}))
	defer srv.Close()

	c := NewWithBaseURL(freshAuth(t), srv.URL)
	resp, err := c.GenerateImage(context.Background(), agentmodel.ImageRequest{Model: "gpt-5.5", Prompt: "two red circles"})
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("len(Data) = %d, want 2", len(resp.Data))
	}
	if resp.Data[0].B64JSON != "item-done-one" || resp.Data[1].B64JSON != "item-done-two" {
		t.Errorf("Data = %+v, want [item-done-one, item-done-two]", resp.Data)
	}
}
