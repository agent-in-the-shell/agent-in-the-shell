package api_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/api"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/cost"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/stub"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/telemetry"
)

// TestMetricsEndpoint_ServedWhenEnabled wires a telemetry-enabled server, drives
// one request, and asserts the /metrics scrape reflects it — end-to-end proof
// that the handler funnel fans out to Prometheus.
func TestMetricsEndpoint_ServedWhenEnabled(t *testing.T) {
	tel, err := telemetry.New(context.Background(), telemetry.Config{MetricsEnabled: true})
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}

	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	registry, _ := cost.LoadDefault()
	r := router.New(map[string][]router.Deployment{
		"gpt-4": {{Name: "primary", Provider: &stub.Stub{NameValue: "openai"}, Model: "openai/gpt-4o", Weight: 1}},
	}, nil)
	s := api.New(api.Config{Router: r, Store: st, Registry: registry, BearerToken: testToken, Telemetry: tel})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	resp := mustPost(t, ts, "/v1/chat/completions", agentmodel.ChatRequest{
		Model:    "gpt-4",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	}, testToken)
	resp.Body.Close()

	mresp, err := http.Get(ts.URL + "/metrics") // unauthenticated
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer mresp.Body.Close()
	if mresp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", mresp.StatusCode)
	}
	body, _ := io.ReadAll(mresp.Body)
	for _, want := range []string{
		`agentmodel_requests_total{auth_mode="api_key",model="openai/gpt-4o",status="ok"}`,
		`agentmodel_cost_source_total{source="priced"}`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/metrics missing %q", want)
		}
	}
}

// TestMetricsEndpoint_NotServedWhenDisabled confirms /metrics is absent (404)
// unless telemetry metrics are explicitly enabled.
func TestMetricsEndpoint_NotServedWhenDisabled(t *testing.T) {
	ts, _ := newTestServer(t, nil) // newTestServer wires no telemetry
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/metrics status = %d, want 404 when telemetry disabled", resp.StatusCode)
	}
}
