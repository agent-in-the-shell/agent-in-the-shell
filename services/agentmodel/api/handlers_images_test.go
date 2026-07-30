package api_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/stub"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
)

func TestImageGenerations_Success(t *testing.T) {
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"image": {{
			Name:     "openai/gpt-image-1",
			Provider: &stub.Stub{NameValue: "openai"},
			Model:    "gpt-image-1",
			Weight:   100,
		}},
	})

	resp := mustPost(t, ts, "/v1/images/generations", agentmodel.ImageRequest{
		Model:  "image",
		Prompt: "a cat",
	}, testToken)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out agentmodel.ImageResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Data) != 1 || out.Data[0].B64JSON == "" {
		t.Fatalf("unexpected data: %+v", out.Data)
	}
	// Response reports the resolved upstream model id, not the logical alias.
	if out.Model != "gpt-image-1" {
		t.Errorf("model = %q, want gpt-image-1", out.Model)
	}
}

func TestImageGenerations_Validation(t *testing.T) {
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"image": {{Name: "openai/gpt-image-1", Provider: &stub.Stub{}, Model: "gpt-image-1", Weight: 100}},
	})

	// Missing prompt → 400.
	resp := mustPost(t, ts, "/v1/images/generations", map[string]any{"model": "image"}, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	resp = mustPost(t, ts, "/v1/images/generations", map[string]any{"prompt": "x"}, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing model status = %d, want 400", resp.StatusCode)
	}

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/images/generations", strings.NewReader("{"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err = ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do invalid json: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid json status = %d, want 400", resp.StatusCode)
	}
}

func TestImageGenerations_ProviderError(t *testing.T) {
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"image": {{Name: "openai/gpt-image-1", Provider: &stub.Stub{ImageErr: errors.New("upstream down")}, Model: "gpt-image-1", Weight: 100}},
	})

	resp := mustPost(t, ts, "/v1/images/generations", agentmodel.ImageRequest{Model: "image", Prompt: "x"}, testToken)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("provider error status = 200, want error")
	}
}

func TestImageGenerations_Unauthorized(t *testing.T) {
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"image": {{Name: "openai/gpt-image-1", Provider: &stub.Stub{}, Model: "gpt-image-1", Weight: 100}},
	})

	resp := mustPost(t, ts, "/v1/images/generations", agentmodel.ImageRequest{Model: "image", Prompt: "x"}, "wrong-token")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}
