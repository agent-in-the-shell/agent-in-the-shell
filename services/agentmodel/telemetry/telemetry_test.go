package telemetry_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/telemetry"
)

// TestNew_DisabledIsNilNoOp verifies that with both subsystems off, New returns
// a nil *Telemetry and every method is a safe no-op on that nil.
func TestNew_DisabledIsNilNoOp(t *testing.T) {
	tel, err := telemetry.New(context.Background(), telemetry.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if tel != nil {
		t.Fatalf("expected nil Telemetry when fully disabled, got %#v", tel)
	}
	// All methods must tolerate a nil receiver.
	tel.RecordRequest(context.Background(), store.RequestLog{ModelUsed: "x"})
	tel.DeploymentResult("m", "p", "d", true)
	tel.Cooldown("m", "p", "d")
	if tel.MetricsEnabled() {
		t.Error("nil.MetricsEnabled() should be false")
	}
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Errorf("nil.Shutdown(): %v", err)
	}
}

// TestRecordRequest_Metrics drives RecordRequest + the router-observer methods
// and asserts the Prometheus scrape reflects them.
func TestRecordRequest_Metrics(t *testing.T) {
	tel, err := telemetry.New(context.Background(), telemetry.Config{MetricsEnabled: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !tel.MetricsEnabled() {
		t.Fatal("MetricsEnabled() should be true")
	}

	tel.RecordRequest(context.Background(), store.RequestLog{
		ModelUsed: "openai/gpt-4o", AuthMode: "api_key", Status: "ok",
		PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150,
		CostUSD: 0.0075, CostSource: "priced", LatencyMs: 1200,
	})
	tel.RecordRequest(context.Background(), store.RequestLog{
		ModelUsed: "mystery-model", AuthMode: "api_key", Status: "ok",
		CostSource: "unpriced", LatencyMs: 5,
	})
	tel.DeploymentResult("gpt-4", "openai", "primary", false)
	tel.DeploymentResult("gpt-4", "openai", "fallback", true)
	tel.Cooldown("gpt-4", "openai", "primary")

	body := scrape(t, tel)
	wants := []string{
		`agentmodel_requests_total{auth_mode="api_key",model="openai/gpt-4o",status="ok"} 1`,
		`agentmodel_cost_source_total{source="priced"} 1`,
		`agentmodel_cost_source_total{source="unpriced"} 1`,
		`agentmodel_spend_usd_total{model="openai/gpt-4o"} 0.0075`,
		`agentmodel_tokens_total{kind="completion",model="openai/gpt-4o"} 50`,
		`agentmodel_tokens_total{kind="prompt",model="openai/gpt-4o"} 100`,
		`agentmodel_latency_per_output_token_seconds_bucket{model="openai/gpt-4o",le="+Inf"} 1`,
		`agentmodel_request_duration_seconds_bucket{model="openai/gpt-4o",le="+Inf"} 1`,
		`agentmodel_deployment_requests_total{deployment="primary",model="gpt-4",provider="openai",result="failure"} 1`,
		`agentmodel_deployment_requests_total{deployment="fallback",model="gpt-4",provider="openai",result="success"} 1`,
		`agentmodel_deployment_cooldowns_total{deployment="primary",model="gpt-4",provider="openai"} 1`,
	}
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("scrape missing line:\n  %s", want)
		}
	}
	// An unpriced request must NOT add to spend.
	if strings.Contains(body, `agentmodel_spend_usd_total{model="mystery-model"}`) {
		t.Error("unpriced request should not record spend")
	}
}

func TestRecordRequest_PolicyRejectionSkipsLatency(t *testing.T) {
	tel, err := telemetry.New(context.Background(), telemetry.Config{MetricsEnabled: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tel.RecordRequest(context.Background(), store.RequestLog{
		ModelUsed: "blocked-model",
		AuthMode:  "api_key",
		Status:    "error",
		ErrorType: agentmodel.ErrTypeBudgetExceeded,
		LatencyMs: 900,
	})

	body := scrape(t, tel)
	if !strings.Contains(body, `agentmodel_requests_total{auth_mode="api_key",model="blocked-model",status="error"} 1`) {
		t.Fatalf("scrape missing blocked request counter:\n%s", body)
	}
	if strings.Contains(body, `agentmodel_request_duration_seconds_bucket{model="blocked-model"`) {
		t.Fatalf("policy rejection should not record duration histogram:\n%s", body)
	}
}

// TestRecordRequest_TracingDisabledNoPanic ensures the span path is skipped
// (no nil tracer deref) when only metrics are enabled.
func TestRecordRequest_TracingDisabledNoPanic(t *testing.T) {
	tel, err := telemetry.New(context.Background(), telemetry.Config{MetricsEnabled: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tel.RecordRequest(context.Background(), store.RequestLog{
		ModelUsed: "m", Status: "error", ErrorType: "upstream_error", LatencyMs: 10,
	})
}

func TestRecordRequest_TracingExportsAndShutdown(t *testing.T) {
	var exports atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exports.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", ts.URL)

	tel, err := telemetry.New(context.Background(), telemetry.Config{
		TracingEnabled: true,
		ServiceName:    "agentmodel-test",
	})
	if err != nil {
		t.Fatalf("New tracing telemetry: %v", err)
	}
	tel.RecordRequest(context.Background(), store.RequestLog{
		ModelRequested:   "gpt-4o",
		ModelUsed:        "gpt-4o",
		Provider:         "openai",
		AuthMode:         "api_key",
		PromptTokens:     7,
		CompletionTokens: 3,
		TotalTokens:      10,
		CostUSD:          0.001,
		CostSource:       "priced",
		Status:           "error",
		ErrorType:        agentmodel.ErrTypeUpstream,
		LatencyMs:        25,
	})
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if exports.Load() == 0 {
		t.Fatal("expected tracing shutdown to export spans")
	}
}

func scrape(t *testing.T, tel *telemetry.Telemetry) string {
	t.Helper()
	ts := httptest.NewServer(tel.MetricsHandler())
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatalf("scrape GET: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}
