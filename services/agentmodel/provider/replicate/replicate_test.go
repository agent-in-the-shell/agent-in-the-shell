package replicate

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
)

// bearerKey mirrors what cmd/agent-model's staticAuthFor builds for a provider
// that is not in the special-header map: Authorization: Bearer <token>.
func bearerKey(token string) *auth.StaticKey {
	return &auth.StaticKey{HeaderName: "Authorization", Prefix: "Bearer ", Token: token}
}

func TestForward_InjectsGatewayCredentialAndForwardsRequest(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotAuth   string
		gotPrefer string
		gotCT     string
		gotBody   []byte
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotPrefer = r.Header.Get("Prefer")
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"pred1","status":"starting"}`))
	}))
	defer upstream.Close()

	c := NewWithBaseURL(bearerKey("r8_gateway_token"), upstream.URL)

	// The client sends its OWN (irrelevant) Authorization plus Prefer/Content-Type.
	clientHeaders := http.Header{}
	clientHeaders.Set("Authorization", "Bearer client-should-be-dropped")
	clientHeaders.Set("Prefer", "wait")
	clientHeaders.Set("Content-Type", "application/json")

	body := []byte(`{"version":"abc","input":{"prompt":"hi"}}`)
	resp, err := c.Forward(context.Background(), http.MethodPost, "/v1/predictions", body, clientHeaders)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	defer resp.Body.Close()

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/predictions" {
		t.Errorf("path = %q, want /v1/predictions", gotPath)
	}
	// The gateway credential MUST replace the client's Authorization.
	if gotAuth != "Bearer r8_gateway_token" {
		t.Errorf("upstream Authorization = %q, want gateway-injected Bearer token", gotAuth)
	}
	if gotPrefer != "wait" {
		t.Errorf("Prefer = %q, want wait (must survive for sync semantics)", gotPrefer)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	if string(gotBody) != string(body) {
		t.Errorf("upstream body = %q, want %q", gotBody, body)
	}

	// Non-2xx-or-2xx alike, Forward returns the raw response; here 201 + body.
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201 (returned verbatim)", resp.StatusCode)
	}
	out, _ := io.ReadAll(resp.Body)
	if string(out) != `{"id":"pred1","status":"starting"}` {
		t.Errorf("response body = %q, want upstream body verbatim", out)
	}
}

func TestForward_ReturnsUpstreamErrorVerbatim(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"detail":"bad input"}`))
	}))
	defer upstream.Close()

	c := NewWithBaseURL(bearerKey("tok"), upstream.URL)
	resp, err := c.Forward(context.Background(), http.MethodPost, "/v1/predictions", []byte(`{}`), http.Header{})
	if err != nil {
		t.Fatalf("Forward should not error on upstream 4xx: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 returned as-is", resp.StatusCode)
	}
}

func TestForward_GETHasNoBody(t *testing.T) {
	var sawBody bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sawBody = len(b) > 0
		_, _ = w.Write([]byte(`{"id":"pred1","status":"succeeded"}`))
	}))
	defer upstream.Close()

	c := NewWithBaseURL(bearerKey("tok"), upstream.URL)
	resp, err := c.Forward(context.Background(), http.MethodGet, "/v1/predictions/pred1", nil, http.Header{})
	if err != nil {
		t.Fatalf("Forward GET: %v", err)
	}
	defer resp.Body.Close()
	if sawBody {
		t.Error("GET forwarded a non-empty body")
	}
}

func TestProviderIdentity(t *testing.T) {
	c := New(bearerKey("tok"))
	if c.Name() != "replicate" {
		t.Errorf("Name = %q, want replicate", c.Name())
	}
	if c.AuthMode() != agentmodel.AuthModeAPIKey {
		t.Errorf("AuthMode = %q, want api_key", c.AuthMode())
	}
	if c.SupportedModels() != nil {
		t.Errorf("SupportedModels = %v, want nil (passthrough)", c.SupportedModels())
	}
}

func TestChatMethodsUnsupported(t *testing.T) {
	c := New(bearerKey("tok"))
	if _, err := c.Complete(context.Background(), agentmodel.ChatRequest{}); !errors.Is(err, provider.ErrNotSupported) {
		t.Errorf("Complete err = %v, want ErrNotSupported", err)
	}
	if _, err := c.Stream(context.Background(), agentmodel.ChatRequest{}); !errors.Is(err, provider.ErrNotSupported) {
		t.Errorf("Stream err = %v, want ErrNotSupported", err)
	}
	if _, err := c.Embed(context.Background(), agentmodel.EmbeddingRequest{}); !errors.Is(err, provider.ErrNotSupported) {
		t.Errorf("Embed err = %v, want ErrNotSupported", err)
	}
}
