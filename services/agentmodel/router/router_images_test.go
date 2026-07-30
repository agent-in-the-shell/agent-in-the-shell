package router_test

import (
	"context"
	"iter"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/messagesbridge"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/pool"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/stub"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
)

func TestGenerateImage_Success(t *testing.T) {
	r := newRouter(map[string][]router.Deployment{
		"image": {{Name: "openai/gpt-image-1", Provider: &stub.Stub{}, Model: "gpt-image-1", Weight: 1}},
	}, nil)

	resp, err := r.GenerateImage(context.Background(), agentmodel.ImageRequest{Model: "image", Prompt: "a cat"})
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if len(resp.Data) == 0 {
		t.Fatal("expected image data")
	}
	if resp.Model != "gpt-image-1" {
		t.Errorf("model = %q, want resolved upstream id gpt-image-1", resp.Model)
	}
	if resp.Usage.AuthMode != agentmodel.AuthModeAPIKey {
		t.Errorf("auth mode = %q", resp.Usage.AuthMode)
	}
}

// TestGenerateImage_BridgeWrappedProvider guards against a real regression:
// every chatgpt deployment is wrapped in messagesbridge.Bridge (for
// /v1/messages passthrough), and Bridge embeds provider.Provider — embedding
// an interface promotes only that interface's methods, so without an
// explicit forwarding method on Bridge, dep.Provider.(provider.ImageGenerator)
// silently fails for every bridge-wrapped provider even when the wrapped
// provider itself implements ImageGenerator.
func TestGenerateImage_BridgeWrappedProvider(t *testing.T) {
	r := newRouter(map[string][]router.Deployment{
		"image": {{Name: "chatgpt/gpt-image-codex", Provider: messagesbridge.New(&stub.Stub{}), Model: "gpt-5.5", Weight: 1}},
	}, nil)

	resp, err := r.GenerateImage(context.Background(), agentmodel.ImageRequest{Model: "image", Prompt: "a cat"})
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if len(resp.Data) == 0 {
		t.Fatal("expected image data forwarded through the bridge")
	}
}

// TestGenerateImage_BridgeWrappedPoolProvider mirrors the real multi-account
// factory chain (messagesbridge.New(pool.New(providers)), factory.go:164) —
// the config path used when a chatgpt deployment sets oauth_token_dirs. Pool
// implements provider.Provider directly (not by embedding), so it needed its
// own GenerateImage forwarding method for the same reason Bridge did; this
// guards that the two wrappers compose correctly end to end.
func TestGenerateImage_BridgeWrappedPoolProvider(t *testing.T) {
	r := newRouter(map[string][]router.Deployment{
		"image": {{
			Name:     "chatgpt/gpt-image-codex",
			Provider: messagesbridge.New(pool.New([]provider.Provider{&stub.Stub{}, &stub.Stub{}})),
			Model:    "gpt-5.5",
			Weight:   1,
		}},
	}, nil)

	resp, err := r.GenerateImage(context.Background(), agentmodel.ImageRequest{Model: "image", Prompt: "a cat"})
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if len(resp.Data) == 0 {
		t.Fatal("expected image data forwarded through bridge and pool")
	}
}

func TestGenerateImage_NoDeployment(t *testing.T) {
	r := newRouter(map[string][]router.Deployment{}, nil)
	if _, err := r.GenerateImage(context.Background(), agentmodel.ImageRequest{Model: "image", Prompt: "x"}); err == nil {
		t.Fatal("expected ErrNoDeployment for unknown model")
	}
}

// chatOnlyProvider implements provider.Provider but NOT provider.ImageGenerator,
// to exercise the router's "no image-capable deployment" branch.
type chatOnlyProvider struct{}

func (chatOnlyProvider) Name() string              { return "chat-only" }
func (chatOnlyProvider) SupportedModels() []string { return []string{"chat-only"} }
func (chatOnlyProvider) AuthMode() string          { return agentmodel.AuthModeAPIKey }
func (chatOnlyProvider) Complete(context.Context, agentmodel.ChatRequest) (agentmodel.ChatResponse, error) {
	return agentmodel.ChatResponse{}, nil
}
func (chatOnlyProvider) Stream(context.Context, agentmodel.ChatRequest) (iter.Seq2[provider.StreamChunk, error], error) {
	return nil, nil
}
func (chatOnlyProvider) Embed(context.Context, agentmodel.EmbeddingRequest) (agentmodel.EmbeddingResponse, error) {
	return agentmodel.EmbeddingResponse{}, nil
}

func TestGenerateImage_ModelNotImageCapable(t *testing.T) {
	r := newRouter(map[string][]router.Deployment{
		"chat": {{Name: "chat-only/chat-only", Provider: chatOnlyProvider{}, Model: "chat-only", Weight: 1}},
	}, nil)

	_, err := r.GenerateImage(context.Background(), agentmodel.ImageRequest{Model: "chat", Prompt: "x"})
	if err == nil {
		t.Fatal("expected error for model with no image-capable deployment")
	}
	if ae := agentmodel.Wrap(err); ae.Type != agentmodel.ErrTypeInvalidRequest {
		t.Fatalf("error type = %q, want %q", ae.Type, agentmodel.ErrTypeInvalidRequest)
	}
}
