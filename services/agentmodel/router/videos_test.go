package router_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/stub"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
)

// videoStub embeds the standard Stub (for the Provider surface) and adds the
// async VideoGenerator methods.
type videoStub struct {
	*stub.Stub
	submitOp  agentmodel.VideoOperation
	submitErr error
	polled    string // records the provider op id PollVideo received
}

func (v *videoStub) SubmitVideo(_ context.Context, _ agentmodel.GenerateVideoRequest) (agentmodel.VideoOperation, error) {
	if v.submitErr != nil {
		return agentmodel.VideoOperation{}, v.submitErr
	}
	return v.submitOp, nil
}

func (v *videoStub) PollVideo(_ context.Context, opID string) (agentmodel.VideoOperation, error) {
	v.polled = opID
	return agentmodel.VideoOperation{
		ID:     opID,
		Status: agentmodel.VideoStatusSucceeded,
		Videos: []agentmodel.VideoResult{{URL: "https://x/v.mp4"}},
	}, nil
}

func TestGenerateAndPollVideo_RoundTrip(t *testing.T) {
	vs := &videoStub{
		Stub:     &stub.Stub{NameValue: "gemini"},
		submitOp: agentmodel.VideoOperation{ID: "models/veo/operations/xyz", Status: agentmodel.VideoStatusRunning},
	}
	r := router.New(map[string][]router.Deployment{
		"veo-3": {{Name: "gemini/veo", Provider: vs, Model: "veo-3.1-generate-preview", Weight: 1}},
	}, nil)

	op, err := r.GenerateVideo(context.Background(), agentmodel.GenerateVideoRequest{Model: "veo-3", Prompt: "hi"})
	if err != nil {
		t.Fatalf("GenerateVideo: %v", err)
	}
	if op.Model != "veo-3" {
		t.Errorf("model = %q, want the logical name", op.Model)
	}
	if op.ID == "models/veo/operations/xyz" || op.ID == "" {
		t.Errorf("op id should be a gateway-encoded token, got %q", op.ID)
	}
	if op.Status != agentmodel.VideoStatusRunning {
		t.Errorf("status = %q, want running", op.Status)
	}

	// Poll with the gateway id; the router must resolve it back to the provider
	// operation id and re-dispatch to the same provider.
	poll, err := r.PollVideo(context.Background(), op.ID)
	if err != nil {
		t.Fatalf("PollVideo: %v", err)
	}
	if vs.polled != "models/veo/operations/xyz" {
		t.Errorf("provider polled with %q, want the original provider op id", vs.polled)
	}
	if poll.ID != op.ID {
		t.Errorf("poll id = %q, want the gateway id %q", poll.ID, op.ID)
	}
	if poll.Status != agentmodel.VideoStatusSucceeded || len(poll.Videos) != 1 {
		t.Errorf("poll = %+v", poll)
	}
}

func TestGenerateVideo_NoDeployment(t *testing.T) {
	r := router.New(map[string][]router.Deployment{}, nil)
	_, err := r.GenerateVideo(context.Background(), agentmodel.GenerateVideoRequest{Model: "missing", Prompt: "x"})
	if !errors.Is(err, router.ErrNoDeployment) {
		t.Errorf("err = %v, want ErrNoDeployment", err)
	}
}

func TestGenerateVideo_NotVideoCapable(t *testing.T) {
	r := router.New(map[string][]router.Deployment{
		"chat": {{Name: "openai/x", Provider: &stub.Stub{NameValue: "openai"}, Model: "gpt-4o", Weight: 1}},
	}, nil)
	_, err := r.GenerateVideo(context.Background(), agentmodel.GenerateVideoRequest{Model: "chat", Prompt: "x"})
	if err == nil {
		t.Fatal("expected invalid_request for a non-video-capable model")
	}
	if ae := agentmodel.Wrap(err); ae.Type != agentmodel.ErrTypeInvalidRequest {
		t.Errorf("type = %q, want invalid_request", ae.Type)
	}
}

func TestPollVideo_BadID(t *testing.T) {
	r := router.New(map[string][]router.Deployment{}, nil)
	_, err := r.PollVideo(context.Background(), "not-a-valid-op-id")
	if err == nil {
		t.Fatal("expected error for a malformed operation id")
	}
	if ae := agentmodel.Wrap(err); ae.Type != agentmodel.ErrTypeInvalidRequest {
		t.Errorf("type = %q, want invalid_request", ae.Type)
	}
}

func (v *videoStub) DownloadVideo(_ context.Context, assetURL string) (io.ReadCloser, string, error) {
	return io.NopCloser(strings.NewReader("bytes:" + assetURL)), "video/mp4", nil
}

// TestDownloadVideo_ResolvesRepollsAndStreams: the router decodes the gateway op
// id, re-polls for a fresh asset URL, and streams via VideoDownloader (#1493).
func TestDownloadVideo_ResolvesRepollsAndStreams(t *testing.T) {
	vs := &videoStub{
		Stub:     &stub.Stub{NameValue: "gemini"},
		submitOp: agentmodel.VideoOperation{ID: "models/veo/operations/xyz", Status: agentmodel.VideoStatusRunning},
	}
	r := router.New(map[string][]router.Deployment{
		"veo-3": {{Name: "gemini/veo", Provider: vs, Model: "veo-3.1-generate-preview", Weight: 1}},
	}, nil)
	op, err := r.GenerateVideo(context.Background(), agentmodel.GenerateVideoRequest{Model: "veo-3", Prompt: "hi"})
	if err != nil {
		t.Fatalf("GenerateVideo: %v", err)
	}
	rc, ct, err := r.DownloadVideo(context.Background(), op.ID, 0)
	if err != nil {
		t.Fatalf("DownloadVideo: %v", err)
	}
	defer rc.Close()
	if ct != "video/mp4" {
		t.Errorf("content type = %q", ct)
	}
	b, _ := io.ReadAll(rc)
	if string(b) != "bytes:https://x/v.mp4" {
		t.Errorf("body = %q, want the re-polled asset URL's bytes", b)
	}
}

// videoGenOnly is a VideoGenerator that is NOT a VideoDownloader (its asset URLs
// would be publicly fetchable), to exercise VideoProxyable's false branch.
type videoGenOnly struct{ *stub.Stub }

func (videoGenOnly) SubmitVideo(context.Context, agentmodel.GenerateVideoRequest) (agentmodel.VideoOperation, error) {
	return agentmodel.VideoOperation{ID: "op/1", Status: agentmodel.VideoStatusRunning}, nil
}
func (videoGenOnly) PollVideo(_ context.Context, id string) (agentmodel.VideoOperation, error) {
	return agentmodel.VideoOperation{ID: id, Status: agentmodel.VideoStatusSucceeded}, nil
}

func TestVideoProxyable(t *testing.T) {
	// Downloader provider → proxyable (gateway must rewrite URLs).
	dl := &videoStub{Stub: &stub.Stub{NameValue: "gemini"}, submitOp: agentmodel.VideoOperation{ID: "op/1", Status: agentmodel.VideoStatusRunning}}
	rd := router.New(map[string][]router.Deployment{"m": {{Name: "gemini/v", Provider: dl, Model: "veo", Weight: 1}}}, nil)
	opD, _ := rd.GenerateVideo(context.Background(), agentmodel.GenerateVideoRequest{Model: "m", Prompt: "x"})
	if !rd.VideoProxyable(opD.ID) {
		t.Error("downloader provider should be proxyable")
	}

	// Non-downloader provider → not proxyable (public URLs pass through).
	gen := videoGenOnly{Stub: &stub.Stub{NameValue: "public"}}
	rg := router.New(map[string][]router.Deployment{"m": {{Name: "public/v", Provider: gen, Model: "veo", Weight: 1}}}, nil)
	opG, _ := rg.GenerateVideo(context.Background(), agentmodel.GenerateVideoRequest{Model: "m", Prompt: "x"})
	if rg.VideoProxyable(opG.ID) {
		t.Error("non-downloader provider must NOT be proxyable")
	}

	// Malformed id → false, not a panic.
	if rd.VideoProxyable("not-a-real-id") {
		t.Error("malformed id must be non-proxyable")
	}
}
