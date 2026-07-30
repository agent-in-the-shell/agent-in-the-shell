package gemini_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/gemini"
)

func TestSubmitVideo(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"name":"models/veo-3.1-generate-preview/operations/abc123"}`))
	}))
	defer srv.Close()

	c := gemini.NewWithBaseURL(newAuth(), srv.URL)
	op, err := c.SubmitVideo(context.Background(), agentmodel.GenerateVideoRequest{
		Model:       "veo-3.1-generate-preview",
		Prompt:      "a cat surfing",
		AspectRatio: "16:9",
		N:           1,
	})
	if err != nil {
		t.Fatalf("SubmitVideo: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/v1beta/models/veo-3.1-generate-preview:predictLongRunning" {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.Contains(gotBody, `"prompt":"a cat surfing"`) || !strings.Contains(gotBody, `"aspectRatio":"16:9"`) {
		t.Errorf("request body missing expected fields: %s", gotBody)
	}
	if op.ID != "models/veo-3.1-generate-preview/operations/abc123" {
		t.Errorf("op id = %q (want the provider operation name)", op.ID)
	}
	if op.Status != agentmodel.VideoStatusRunning {
		t.Errorf("status = %q, want running", op.Status)
	}
	if op.Object != agentmodel.VideoOperationObject {
		t.Errorf("object = %q", op.Object)
	}
}

func TestPollVideo_Done(t *testing.T) {
	const opName = "models/veo-3.1-generate-preview/operations/abc123"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/v1beta/"+opName {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"name":"` + opName + `","done":true,"response":{"generateVideoResponse":{"generatedSamples":[{"video":{"uri":"https://example/video.mp4"}}]}}}`))
	}))
	defer srv.Close()

	c := gemini.NewWithBaseURL(newAuth(), srv.URL)
	op, err := c.PollVideo(context.Background(), opName)
	if err != nil {
		t.Fatalf("PollVideo: %v", err)
	}
	if op.Status != agentmodel.VideoStatusSucceeded {
		t.Fatalf("status = %q, want succeeded", op.Status)
	}
	if len(op.Videos) != 1 || op.Videos[0].URL != "https://example/video.mp4" {
		t.Fatalf("videos = %+v", op.Videos)
	}
}

func TestPollVideo_Running(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"op","done":false}`))
	}))
	defer srv.Close()

	c := gemini.NewWithBaseURL(newAuth(), srv.URL)
	op, err := c.PollVideo(context.Background(), "op")
	if err != nil {
		t.Fatalf("PollVideo: %v", err)
	}
	if op.Status != agentmodel.VideoStatusRunning {
		t.Errorf("status = %q, want running", op.Status)
	}
}

func TestPollVideo_Failed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"op","done":true,"error":{"code":3,"message":"safety blocked"}}`))
	}))
	defer srv.Close()

	c := gemini.NewWithBaseURL(newAuth(), srv.URL)
	op, err := c.PollVideo(context.Background(), "op")
	if err != nil {
		t.Fatalf("PollVideo: %v", err)
	}
	if op.Status != agentmodel.VideoStatusFailed || op.Error != "safety blocked" {
		t.Errorf("op = %+v, want failed with message", op)
	}
}

func TestSubmitVideo_Validation(t *testing.T) {
	c := gemini.NewWithBaseURL(newAuth(), "http://127.0.0.1")
	if _, err := c.SubmitVideo(context.Background(), agentmodel.GenerateVideoRequest{Prompt: "x"}); err == nil {
		t.Error("expected error for missing model")
	}
	if _, err := c.SubmitVideo(context.Background(), agentmodel.GenerateVideoRequest{Model: "veo-3.1-generate-preview"}); err == nil {
		t.Error("expected error for missing prompt")
	}
}

// TestDownloadVideo_AppliesAuthAndStreams: the asset fetch carries the
// provider credential (x-goog-api-key) and streams the bytes back (#1493).
func TestDownloadVideo_AppliesAuthAndStreams(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-goog-api-key")
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = io.WriteString(w, "MP4BYTES")
	}))
	defer srv.Close()

	c := gemini.NewWithBaseURL(newAuth(), "http://unused")
	rc, ct, err := c.DownloadVideo(context.Background(), srv.URL+"/asset.mp4")
	if err != nil {
		t.Fatalf("DownloadVideo: %v", err)
	}
	defer rc.Close()
	if gotKey != "AIza-test-key" {
		t.Errorf("x-goog-api-key = %q, want the provider credential", gotKey)
	}
	if ct != "video/mp4" {
		t.Errorf("content type = %q, want video/mp4", ct)
	}
	b, _ := io.ReadAll(rc)
	if string(b) != "MP4BYTES" {
		t.Errorf("body = %q, want MP4BYTES", b)
	}
}
