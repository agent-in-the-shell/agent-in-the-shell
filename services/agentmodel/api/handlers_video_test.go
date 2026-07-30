package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/stub"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
)

// videoProviderStub adds the async VideoGenerator surface to the base Stub so a
// deployment can serve POST /v1/videos/generations + GET /v1/videos/{id}.
type videoProviderStub struct {
	*stub.Stub
	submitErr error
	pollErr   error
}

func (v videoProviderStub) SubmitVideo(_ context.Context, _ agentmodel.GenerateVideoRequest) (agentmodel.VideoOperation, error) {
	if v.submitErr != nil {
		return agentmodel.VideoOperation{}, v.submitErr
	}
	return agentmodel.VideoOperation{ID: "op-123", Status: agentmodel.VideoStatusRunning}, nil
}

func (v videoProviderStub) PollVideo(_ context.Context, opID string) (agentmodel.VideoOperation, error) {
	if v.pollErr != nil {
		return agentmodel.VideoOperation{}, v.pollErr
	}
	if opID != "op-123" {
		return agentmodel.VideoOperation{}, agentmodel.NewErrorf(agentmodel.ErrTypeNotFound, "unexpected op id %q", opID)
	}
	return agentmodel.VideoOperation{
		ID:     opID,
		Status: agentmodel.VideoStatusSucceeded,
		Videos: []agentmodel.VideoResult{{URL: "https://example/v.mp4", MimeType: "video/mp4"}},
	}, nil
}

// DownloadVideo returns fixed bytes tagged with the asset URL it was asked to
// fetch, so a test can assert the gateway proxied the right (re-polled) asset.
func (v videoProviderStub) DownloadVideo(_ context.Context, assetURL string) (io.ReadCloser, string, error) {
	return io.NopCloser(strings.NewReader("FAKE-VIDEO-BYTES for " + assetURL)), "video/mp4", nil
}

func videoDeployments() map[string][]router.Deployment {
	return videoDeploymentsWith(videoProviderStub{Stub: &stub.Stub{NameValue: "gemini"}})
}

func videoDeploymentsWith(p videoProviderStub) map[string][]router.Deployment {
	return map[string][]router.Deployment{
		"veo-3": {{
			Name:     "gemini/veo",
			Provider: p,
			Model:    "veo-3.1-generate-preview",
			Weight:   100,
		}},
	}
}

func TestVideoGenerations_SubmitAndPoll(t *testing.T) {
	ts, _ := newTestServer(t, videoDeployments())

	resp := mustPost(t, ts, "/v1/videos/generations", agentmodel.GenerateVideoRequest{
		Model:  "veo-3",
		Prompt: "a cat surfing",
	}, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("submit status = %d, want 202", resp.StatusCode)
	}
	var op agentmodel.VideoOperation
	if err := json.NewDecoder(resp.Body).Decode(&op); err != nil {
		t.Fatalf("decode submit: %v", err)
	}
	if op.ID == "" || op.Status != agentmodel.VideoStatusRunning {
		t.Fatalf("unexpected submit op: %+v", op)
	}
	if op.Model != "veo-3" {
		t.Errorf("model = %q, want veo-3", op.Model)
	}

	// Poll the gateway operation id through GET /v1/videos/{id}.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/v1/videos/"+op.ID, nil)
	if err != nil {
		t.Fatalf("new poll request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	pollResp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	defer pollResp.Body.Close()
	if pollResp.StatusCode != http.StatusOK {
		t.Fatalf("poll status = %d, want 200", pollResp.StatusCode)
	}
	var poll agentmodel.VideoOperation
	if err := json.NewDecoder(pollResp.Body).Decode(&poll); err != nil {
		t.Fatalf("decode poll: %v", err)
	}
	if poll.Status != agentmodel.VideoStatusSucceeded || len(poll.Videos) != 1 {
		t.Fatalf("unexpected poll op: %+v", poll)
	}
	// The result URL is rewritten to a gateway content URL the client can fetch
	// with its own bearer (#1493), not the raw upstream asset that would 401.
	wantSuffix := "/v1/videos/" + op.ID + "/content?index=0"
	if !strings.HasSuffix(poll.Videos[0].URL, wantSuffix) {
		t.Errorf("video url = %q, want a gateway content URL ending %q", poll.Videos[0].URL, wantSuffix)
	}
	if strings.Contains(poll.Videos[0].URL, "example/v.mp4") {
		t.Errorf("raw upstream asset URL leaked to the client: %q", poll.Videos[0].URL)
	}
}

// TestVideoContent_ProxiesBytes guards #1493: GET /v1/videos/{id}/content
// streams the asset bytes fetched with the gateway's provider credential, so a
// client that holds only the gateway bearer can retrieve the video.
func TestVideoContent_ProxiesBytes(t *testing.T) {
	ts, _ := newTestServer(t, videoDeployments())

	// Submit to obtain a real gateway op id.
	resp := mustPost(t, ts, "/v1/videos/generations", agentmodel.GenerateVideoRequest{Model: "veo-3", Prompt: "x"}, testToken)
	var op agentmodel.VideoOperation
	_ = json.NewDecoder(resp.Body).Decode(&op)
	_ = resp.Body.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/v1/videos/"+op.ID+"/content?index=0", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	cResp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("content GET: %v", err)
	}
	defer cResp.Body.Close()
	if cResp.StatusCode != http.StatusOK {
		t.Fatalf("content status = %d, want 200", cResp.StatusCode)
	}
	if ct := cResp.Header.Get("Content-Type"); ct != "video/mp4" {
		t.Errorf("Content-Type = %q, want video/mp4", ct)
	}
	body, _ := io.ReadAll(cResp.Body)
	// Proves the gateway re-polled to the fresh upstream asset and streamed it.
	if !strings.Contains(string(body), "FAKE-VIDEO-BYTES for https://example/v.mp4") {
		t.Errorf("content body = %q, want the proxied upstream asset bytes", body)
	}
}

// TestVideoContent_BadIndex: an out-of-range index is a terminal 404, not a hang.
func TestVideoContent_BadIndex(t *testing.T) {
	ts, _ := newTestServer(t, videoDeployments())
	resp := mustPost(t, ts, "/v1/videos/generations", agentmodel.GenerateVideoRequest{Model: "veo-3", Prompt: "x"}, testToken)
	var op agentmodel.VideoOperation
	_ = json.NewDecoder(resp.Body).Decode(&op)
	_ = resp.Body.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/v1/videos/"+op.ID+"/content?index=5", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	cResp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("content GET: %v", err)
	}
	defer cResp.Body.Close()
	if cResp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for an out-of-range index", cResp.StatusCode)
	}
}

func TestVideoGenerations_Validation(t *testing.T) {
	ts, _ := newTestServer(t, videoDeployments())

	// Missing prompt → 400.
	resp := mustPost(t, ts, "/v1/videos/generations", map[string]any{"model": "veo-3"}, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	resp = mustPost(t, ts, "/v1/videos/generations", map[string]any{"prompt": "x"}, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing model status = %d, want 400", resp.StatusCode)
	}

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/videos/generations", strings.NewReader("{"))
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

func TestVideoGenerations_ProviderErrors(t *testing.T) {
	ts, _ := newTestServer(t, videoDeploymentsWith(videoProviderStub{
		Stub:      &stub.Stub{NameValue: "gemini"},
		submitErr: errors.New("submit failed"),
	}))

	resp := mustPost(t, ts, "/v1/videos/generations", agentmodel.GenerateVideoRequest{Model: "veo-3", Prompt: "x"}, testToken)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted {
		t.Fatalf("submit error status = 202, want error")
	}
}

func TestVideoGenerations_Unauthorized(t *testing.T) {
	ts, _ := newTestServer(t, videoDeployments())

	resp := mustPost(t, ts, "/v1/videos/generations", agentmodel.GenerateVideoRequest{Model: "veo-3", Prompt: "x"}, "wrong-token")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}
