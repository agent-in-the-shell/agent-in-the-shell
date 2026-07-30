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

func TestGenerateImage(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"candidates": [{"content": {"parts": [
				{"text": "here is your image"},
				{"inlineData": {"mimeType": "image/png", "data": "aW1hZ2VieXRlcw=="}}
			]}}],
			"usageMetadata": {"promptTokenCount": 8, "candidatesTokenCount": 1290, "totalTokenCount": 1298}
		}`))
	}))
	defer srv.Close()

	c := gemini.NewWithBaseURL(newAuth(), srv.URL)
	resp, err := c.GenerateImage(context.Background(), agentmodel.ImageRequest{
		Model:  "gemini-2.5-flash-image",
		Prompt: "a banana",
	})
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}

	if gotPath != "/v1beta/models/gemini-2.5-flash-image:generateContent" {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.Contains(gotBody, "responseModalities") || !strings.Contains(gotBody, "IMAGE") {
		t.Errorf("request missing IMAGE response modality: %s", gotBody)
	}

	if len(resp.Data) != 1 || resp.Data[0].B64JSON != "aW1hZ2VieXRlcw==" {
		t.Fatalf("unexpected data: %+v", resp.Data)
	}
	if resp.Usage.PromptTokens != 8 || resp.Usage.CompletionTokens != 1290 {
		t.Errorf("usage mapping wrong: %+v", resp.Usage)
	}
	if resp.Model != "gemini-2.5-flash-image" {
		t.Errorf("model = %q", resp.Model)
	}
}

func TestGenerateImage_NoImageData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"refused"}]}}]}`))
	}))
	defer srv.Close()

	c := gemini.NewWithBaseURL(newAuth(), srv.URL)
	if _, err := c.GenerateImage(context.Background(), agentmodel.ImageRequest{Model: "gemini-2.5-flash-image", Prompt: "x"}); err == nil {
		t.Fatal("expected error when no inline image data returned")
	}
}

func TestGenerateImage_Validation(t *testing.T) {
	c := gemini.NewWithBaseURL(newAuth(), "http://127.0.0.1")
	if _, err := c.GenerateImage(context.Background(), agentmodel.ImageRequest{Prompt: "x"}); err == nil {
		t.Error("expected error for missing model")
	}
	if _, err := c.GenerateImage(context.Background(), agentmodel.ImageRequest{Model: "gemini-2.5-flash-image"}); err == nil {
		t.Error("expected error for missing prompt")
	}
}
