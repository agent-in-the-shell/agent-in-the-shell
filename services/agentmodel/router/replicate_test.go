package router

import (
	"context"
	"net/http"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/stub"
)

// replicateStub adds the ReplicateProxy surface to the base Stub.
type replicateStub struct {
	*stub.Stub
	lastMethod string
	lastPath   string
}

func (rs *replicateStub) Forward(_ context.Context, method, path string, _ []byte, _ http.Header) (*http.Response, error) {
	rs.lastMethod, rs.lastPath = method, path
	return &http.Response{StatusCode: http.StatusCreated, Body: http.NoBody}, nil
}

func TestReplicatePassthrough_DispatchesToCapableDeployment(t *testing.T) {
	rs := &replicateStub{Stub: &stub.Stub{NameValue: "replicate"}}
	r := New(map[string][]Deployment{
		"replicate": {{Name: "replicate/passthrough", Provider: rs, Model: "passthrough", Weight: 1}},
	}, nil)

	resp, dep, err := r.ReplicatePassthrough(context.Background(), http.MethodPost, "/v1/predictions", []byte(`{}`), http.Header{})
	if err != nil {
		t.Fatalf("ReplicatePassthrough: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}
	if dep.Name != "replicate/passthrough" {
		t.Errorf("dep = %q, want replicate/passthrough", dep.Name)
	}
	if rs.lastMethod != http.MethodPost || rs.lastPath != "/v1/predictions" {
		t.Errorf("forwarded %s %s, want POST /v1/predictions", rs.lastMethod, rs.lastPath)
	}
}

func TestReplicatePassthrough_NoCapableDeployment(t *testing.T) {
	// A plain (non-ReplicateProxy) provider must be skipped, surfacing
	// invalid_request rather than a generic exhaustion.
	r := New(map[string][]Deployment{
		"gpt-4": {{Name: "openai", Provider: &stub.Stub{NameValue: "openai"}, Model: "gpt-4o", Weight: 1}},
	}, nil)

	_, _, err := r.ReplicatePassthrough(context.Background(), http.MethodPost, "/v1/predictions", []byte(`{}`), http.Header{})
	if err == nil {
		t.Fatal("expected error when no Replicate-capable deployment is configured")
	}
	ae := agentmodel.Wrap(err)
	if ae.Type != agentmodel.ErrTypeInvalidRequest {
		t.Errorf("err type = %q, want invalid_request", ae.Type)
	}
}
