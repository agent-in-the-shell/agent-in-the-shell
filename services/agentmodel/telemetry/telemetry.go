// Package telemetry fans a completed request out to two observability sinks:
// a Prometheus /metrics endpoint and OTLP (OpenTelemetry) spans. It is the
// agentmodel analogue of LiteLLM's callback/exporter layer.
//
// A nil *Telemetry is a valid, fully-functional no-op: every method guards on
// nil so callers can hold an optional *Telemetry (and pass it where a
// router.Observer is wanted) without branching. New returns (nil, nil) when
// all subsystems are disabled.
//
// The normalized per-request input is store.RequestLog — the same audit row
// the store persists — so metrics, traces, and the DB all describe one
// request from one source of truth. No prompt/response content is exported;
// only metadata (models, tokens, cost, latency, status).
package telemetry

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

// Config controls which subsystems are active. When both are false, New
// returns a nil *Telemetry (a no-op).
type Config struct {
	MetricsEnabled bool
	TracingEnabled bool
	// ServiceName labels exported spans; defaults to "agentmodel".
	ServiceName string
}

// Telemetry holds the Prometheus registry/collectors and the OTLP tracer.
type Telemetry struct {
	reg            *prometheus.Registry
	metricsEnabled bool

	requests   *prometheus.CounterVec   // by model, auth_mode, status
	tokens     *prometheus.CounterVec   // by model, kind (prompt|completion)
	spend      *prometheus.CounterVec   // by model
	costSource *prometheus.CounterVec   // by source (priced|subscription|unpriced)
	duration   *prometheus.HistogramVec // by model — request seconds
	tpot       *prometheus.HistogramVec // by model — seconds per output token

	depResults *prometheus.CounterVec // by model, provider, deployment, result
	cooldowns  *prometheus.CounterVec // by model, provider, deployment

	contentRotations *prometheus.CounterVec // by result (success|failure)
	contentBytes     prometheus.Gauge       // content log on-disk footprint

	tracer trace.Tracer
	tp     *sdktrace.TracerProvider
}

// New builds a Telemetry from cfg. It returns (nil, nil) when both subsystems
// are disabled. When tracing is enabled it constructs an OTLP/HTTP exporter
// configured from the standard OTEL_EXPORTER_OTLP_* environment variables.
func New(ctx context.Context, cfg Config) (*Telemetry, error) {
	if !cfg.MetricsEnabled && !cfg.TracingEnabled {
		return nil, nil
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = "agentmodel"
	}

	t := &Telemetry{reg: prometheus.NewRegistry(), metricsEnabled: cfg.MetricsEnabled}

	t.requests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "agentmodel_requests_total",
		Help: "Total requests dispatched, by resolved model, auth mode, and status.",
	}, []string{"model", "auth_mode", "status"})
	t.tokens = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "agentmodel_tokens_total",
		Help: "Total tokens processed, by resolved model and kind (prompt|completion).",
	}, []string{"model", "kind"})
	t.spend = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "agentmodel_spend_usd_total",
		Help: "Total computed USD spend, by resolved model (0 for subscription/unpriced).",
	}, []string{"model"})
	t.costSource = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "agentmodel_cost_source_total",
		Help: "Requests by cost source: priced | subscription | unpriced. A rising 'unpriced' means the price registry is missing models.",
	}, []string{"source"})
	t.duration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "agentmodel_request_duration_seconds",
		Help:    "End-to-end request latency in seconds, by resolved model.",
		Buckets: prometheus.DefBuckets,
	}, []string{"model"})
	t.tpot = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "agentmodel_latency_per_output_token_seconds",
		Help:    "Latency divided by completion tokens, by resolved model.",
		Buckets: []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25},
	}, []string{"model"})
	t.depResults = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "agentmodel_deployment_requests_total",
		Help: "Deployment attempt outcomes, by logical model, provider, deployment, and result (success|failure).",
	}, []string{"model", "provider", "deployment", "result"})
	t.cooldowns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "agentmodel_deployment_cooldowns_total",
		Help: "Times a deployment was cooled down after a retryable failure, by logical model, provider, and deployment.",
	}, []string{"model", "provider", "deployment"})
	t.contentRotations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "agentmodel_content_log_rotations_total",
		Help: "Content-log rotation attempts by result (success|failure). A rising 'failure' means rotation/retention is not keeping the log bounded.",
	}, []string{"result"})
	t.contentBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "agentmodel_content_log_disk_bytes",
		Help: "Current on-disk footprint of the content log (active file plus rotated backups). Alert on abnormal growth.",
	})

	t.reg.MustRegister(t.requests, t.tokens, t.spend, t.costSource, t.duration, t.tpot, t.depResults, t.cooldowns, t.contentRotations, t.contentBytes)

	if cfg.TracingEnabled {
		exp, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("agentmodel/telemetry: otlp exporter: %w", err)
		}
		res, err := resource.New(ctx, resource.WithAttributes(attribute.String("service.name", cfg.ServiceName)))
		if err != nil {
			return nil, fmt.Errorf("agentmodel/telemetry: resource: %w", err)
		}
		t.tp = sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
		t.tracer = t.tp.Tracer("agentmodel")
	}

	return t, nil
}

// MetricsEnabled reports whether the /metrics endpoint should be served.
func (t *Telemetry) MetricsEnabled() bool { return t != nil && t.metricsEnabled }

// MetricsHandler returns the Prometheus exposition handler over this
// Telemetry's private registry. Only call when MetricsEnabled is true.
func (t *Telemetry) MetricsHandler() http.Handler {
	return promhttp.HandlerFor(t.reg, promhttp.HandlerOpts{})
}

// RecordRequest records one completed request: it increments the Prometheus
// per-request metrics and (when tracing is on) emits a retroactive span whose
// duration matches the request's measured latency. Safe on a nil receiver.
func (t *Telemetry) RecordRequest(ctx context.Context, rl store.RequestLog) {
	if t == nil {
		return
	}
	model := rl.ModelUsed
	t.requests.WithLabelValues(model, rl.AuthMode, rl.Status).Inc()
	t.tokens.WithLabelValues(model, "prompt").Add(float64(rl.PromptTokens))
	t.tokens.WithLabelValues(model, "completion").Add(float64(rl.CompletionTokens))
	if rl.CostUSD > 0 {
		t.spend.WithLabelValues(model).Add(rl.CostUSD)
	}
	if rl.CostSource != "" {
		t.costSource.WithLabelValues(rl.CostSource).Inc()
	}
	sec := float64(rl.LatencyMs) / 1000.0
	// Pre-request policy rejections (budget/allowlist, #47/#52) never reach
	// an upstream, so their 0s rows would drag the latency percentiles of
	// models that were never actually invoked. Gate on the explicit policy
	// error types — not on (status, latency==0) — so a genuinely fast
	// upstream failure still lands in the histogram.
	policyRejection := rl.ErrorType == agentmodel.ErrTypeBudgetExceeded ||
		rl.ErrorType == agentmodel.ErrTypePermissionDenied
	if !policyRejection {
		t.duration.WithLabelValues(model).Observe(sec)
	}
	if rl.CompletionTokens > 0 {
		t.tpot.WithLabelValues(model).Observe(sec / float64(rl.CompletionTokens))
	}

	if t.tracer == nil {
		return
	}
	end := time.Now()
	start := end.Add(-time.Duration(rl.LatencyMs) * time.Millisecond)
	_, span := t.tracer.Start(ctx, "agentmodel.request",
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithTimestamp(start))
	span.SetAttributes(
		attribute.String("agentmodel.model_requested", rl.ModelRequested),
		attribute.String("agentmodel.model_used", rl.ModelUsed),
		attribute.String("agentmodel.auth_mode", rl.AuthMode),
		attribute.Int("agentmodel.prompt_tokens", rl.PromptTokens),
		attribute.Int("agentmodel.completion_tokens", rl.CompletionTokens),
		attribute.Int("agentmodel.total_tokens", rl.TotalTokens),
		attribute.Float64("agentmodel.cost_usd", rl.CostUSD),
		attribute.String("agentmodel.cost_source", rl.CostSource),
		attribute.String("agentmodel.status", rl.Status),
		attribute.Int64("agentmodel.latency_ms", int64(rl.LatencyMs)),
	)
	if rl.Provider != "" {
		span.SetAttributes(attribute.String("agentmodel.provider", rl.Provider))
	}
	if rl.Status == statusError {
		span.SetStatus(codes.Error, rl.ErrorType)
		if rl.ErrorType != "" {
			span.SetAttributes(attribute.String("agentmodel.error_type", rl.ErrorType))
		}
	}
	span.End(trace.WithTimestamp(end))
}

// statusError mirrors the api package's audit status vocabulary without
// importing it (telemetry must not depend on the HTTP layer).
const statusError = "error"

// DeploymentResult records a single deployment attempt outcome. It satisfies
// router.Observer (structurally). Safe on a nil receiver.
func (t *Telemetry) DeploymentResult(model, provider, deployment string, ok bool) {
	if t == nil {
		return
	}
	result := "success"
	if !ok {
		result = "failure"
	}
	t.depResults.WithLabelValues(model, provider, deployment, result).Inc()
}

// Cooldown records that a deployment was cooled down after a retryable
// failure. It satisfies router.Observer (structurally). Safe on a nil receiver.
func (t *Telemetry) Cooldown(model, provider, deployment string) {
	if t == nil {
		return
	}
	t.cooldowns.WithLabelValues(model, provider, deployment).Inc()
}

// ContentLogRotation records one content-log rotation attempt by outcome.
// Safe on a nil receiver.
func (t *Telemetry) ContentLogRotation(ok bool) {
	if t == nil {
		return
	}
	result := "success"
	if !ok {
		result = "failure"
	}
	t.contentRotations.WithLabelValues(result).Inc()
}

// SetContentLogBytes publishes the content log's current on-disk footprint.
// Safe on a nil receiver.
func (t *Telemetry) SetContentLogBytes(n int64) {
	if t == nil {
		return
	}
	t.contentBytes.Set(float64(n))
}

// Shutdown flushes and stops the tracer provider. Safe on a nil receiver or
// when tracing was never enabled.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	if t == nil || t.tp == nil {
		return nil
	}
	return t.tp.Shutdown(ctx)
}
