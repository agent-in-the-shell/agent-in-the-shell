package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/cost"
)

// Replicate passthrough (#847). A transparent reverse proxy that speaks
// Replicate's own prediction wire protocol and records one request_logs row per
// create. It is the Replicate analog of the Anthropic /v1/messages passthrough,
// with one extra step: the prediction response embeds absolute callback URLs
// (urls.get/cancel) pointing at api.replicate.com, so a verbatim copy would let
// the client's poll bypass the gateway. createPrediction/getPrediction rewrite
// those URLs to return through the gateway (see rewritePredictionURLs).

// replicateProviderName is the fixed provider/enforcement/attribution key for
// passthrough rows. The Replicate model itself (from the request body) is
// recorded separately as model_requested/model_used.
const replicateProviderName = "replicate"

// createPrediction handles POST /v1/predictions and the model-based
// POST /v1/models/{owner}/{name}/predictions. It forwards the raw body to
// Replicate, rewrites the embedded callback URLs, copies status+body back, and
// logs one audit row attributed to "replicate".
func (s *Server) createPrediction(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, &agentmodel.Error{Type: agentmodel.ErrTypeInvalidRequest, Message: "read body: " + err.Error()})
		return
	}
	modelRequested := replicateRequestedModel(r, body)

	// Pre-request enforcement (#47/#52): budget + the virtual-key allowlist,
	// keyed on the fixed "replicate" pseudo-model (a key is granted the
	// passthrough as a unit, not per upstream Replicate model).
	if ae := s.enforce(r.Context(), vkFromCtx(r.Context()), replicateProviderName); ae != nil {
		s.logReplicateFailure(r.Context(), modelRequested, ae, 0)
		writeAPIError(w, ae)
		return
	}

	upstream, _, err := s.router.ReplicatePassthrough(r.Context(), http.MethodPost, r.URL.Path, body, r.Header)
	if err != nil {
		s.logReplicateFailure(r.Context(), modelRequested, err, time.Since(start))
		writeAPIError(w, err)
		return
	}
	defer upstream.Body.Close()

	respBody, err := io.ReadAll(upstream.Body)
	if err != nil {
		s.logReplicateFailure(r.Context(), modelRequested, err, time.Since(start))
		writeError(w, http.StatusBadGateway, &agentmodel.Error{Type: agentmodel.ErrTypeUpstream, Message: "read upstream body: " + err.Error()})
		return
	}

	s.proxyReplicateResponse(w, r, upstream, respBody)

	modelUsed := replicateUsedModel(respBody, modelRequested)
	status, errType, costSource := replicateOutcome(upstream.StatusCode)
	// Submit-time per-output cost (#851): for Official Models the billable unit
	// is in the request body, so cost is computed here rather than via a
	// completion poll. Models without a per-output rate stay unpriced.
	var costUSD float64
	if status == statusOk {
		c, src := s.replicatePredictionCost(modelRequested, body)
		costUSD, costSource = c, string(src)
	}
	s.logReplicate(r.Context(), modelRequested, modelUsed, status, errType, costSource, costUSD, time.Since(start))
	s.writeContent(r.Context(), body, respBody)
}

// getPrediction handles GET /v1/predictions/{id} — polling a prediction created
// via the passthrough. Like getVideo, the poll is a read and is not logged
// (completion-time settle-row logging is a follow-up). The callback URLs in the
// poll response are rewritten so the client keeps polling through the gateway.
func (s *Server) getPrediction(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "id") == "" {
		writeError(w, http.StatusBadRequest, &agentmodel.Error{Type: agentmodel.ErrTypeInvalidRequest, Code: agentmodel.CodeMissingRequiredParam, Param: "id", Message: "prediction id is required"})
		return
	}
	s.replicateForwardAndCopy(w, r, http.MethodGet)
}

// cancelPrediction handles POST /v1/predictions/{id}/cancel. Cancel returns the
// prediction object (with callback URLs), so it is rewritten too. Not logged as
// a distinct billable event in the MVP.
func (s *Server) cancelPrediction(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "id") == "" {
		writeError(w, http.StatusBadRequest, &agentmodel.Error{Type: agentmodel.ErrTypeInvalidRequest, Code: agentmodel.CodeMissingRequiredParam, Param: "id", Message: "prediction id is required"})
		return
	}
	s.replicateForwardAndCopy(w, r, http.MethodPost)
}

// replicateForwardAndCopy forwards a body-less control request (poll/cancel) and
// copies the rewritten response back. Shared by getPrediction and
// cancelPrediction.
func (s *Server) replicateForwardAndCopy(w http.ResponseWriter, r *http.Request, method string) {
	upstream, _, err := s.router.ReplicatePassthrough(r.Context(), method, r.URL.Path, nil, r.Header)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	defer upstream.Body.Close()
	respBody, err := io.ReadAll(upstream.Body)
	if err != nil {
		writeError(w, http.StatusBadGateway, &agentmodel.Error{Type: agentmodel.ErrTypeUpstream, Message: "read upstream body: " + err.Error()})
		return
	}
	s.proxyReplicateResponse(w, r, upstream, respBody)
}

// proxyReplicateResponse copies the upstream headers (minus hop-by-hop and the
// now-stale Content-Length), rewrites the callback URLs on a 2xx, and writes the
// status + body to the client.
func (s *Server) proxyReplicateResponse(w http.ResponseWriter, r *http.Request, upstream *http.Response, respBody []byte) {
	if upstream.StatusCode/100 == 2 {
		respBody = rewritePredictionURLs(respBody, s.gatewayBaseURL(r))
	}
	for k, vs := range upstream.Header {
		ck := http.CanonicalHeaderKey(k)
		if _, hop := hopByHopHeaders[ck]; hop || ck == "Content-Length" {
			continue // Content-Length omitted: the body may have been rewritten
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(upstream.StatusCode)
	_, _ = w.Write(respBody)
}

// gatewayBaseURL derives the gateway's own externally-visible base URL from the
// inbound request, so rewritten callback URLs point back here. Honors
// X-Forwarded-Proto (the gateway runs behind a proxy / on plain HTTP internally).
func (s *Server) gatewayBaseURL(r *http.Request) string {
	scheme := "http"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	} else if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// replicateUpstreamHost is the upstream whose embedded URLs are rewritten back
// to the gateway. stream.replicate.com (urls.stream) and replicate.delivery
// (output asset URLs) are deliberately left untouched: the gateway does not
// proxy the SSE stream or asset bytes in the MVP.
const replicateUpstreamHost = "https://api.replicate.com"

// rewritePredictionURLs rewrites the api.replicate.com host in a prediction's
// urls.* object (get/cancel) to the gateway base, so a client following those
// links keeps polling/canceling through the gateway rather than bypassing it.
// It touches ONLY the urls sub-object; every other field is preserved as raw
// bytes. A body that isn't a JSON object, or has no urls, is returned unchanged.
func rewritePredictionURLs(body []byte, gatewayBase string) []byte {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body
	}
	raw, ok := top["urls"]
	if !ok {
		return body
	}
	var urls map[string]string
	if err := json.Unmarshal(raw, &urls); err != nil {
		return body
	}
	changed := false
	for k, v := range urls {
		if strings.HasPrefix(v, replicateUpstreamHost) {
			urls[k] = gatewayBase + strings.TrimPrefix(v, replicateUpstreamHost)
			changed = true
		}
	}
	if !changed {
		return body
	}
	nu, err := json.Marshal(urls)
	if err != nil {
		return body
	}
	top["urls"] = nu
	out, err := json.Marshal(top)
	if err != nil {
		return body
	}
	return out
}

// replicateRequestedModel extracts the model identifier for attribution: the
// {owner}/{name} from the model-based path, else the body's "version" or
// "model" field.
func replicateRequestedModel(r *http.Request, body []byte) string {
	if owner, name := chi.URLParam(r, "owner"), chi.URLParam(r, "name"); owner != "" && name != "" {
		return owner + "/" + name
	}
	var peek struct {
		Version string `json:"version"`
		Model   string `json:"model"`
	}
	if err := json.Unmarshal(body, &peek); err == nil {
		if peek.Model != "" {
			return peek.Model
		}
		if peek.Version != "" {
			return peek.Version
		}
	}
	return ""
}

// replicateUsedModel reads the "model" echoed in the prediction response,
// falling back to the requested model when absent.
func replicateUsedModel(respBody []byte, fallback string) string {
	var peek struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(respBody, &peek); err == nil && peek.Model != "" {
		return peek.Model
	}
	return fallback
}

// replicateOutcome maps an upstream HTTP status onto the audit row's
// (status, error_type, cost_source). On success the row is logged $0 with
// cost_source="unpriced" — a deliberate marker that the price is missing, not
// that the call was free (Replicate returns no dollar cost; per-prediction
// metering is a follow-up).
func replicateOutcome(code int) (status, errType, costSource string) {
	if code/100 == 2 {
		return statusOk, "", string(cost.SourceUnpriced)
	}
	return statusError, agentmodel.ErrTypeUpstream, ""
}

// logReplicate writes one passthrough audit row. Unlike buildUsageLog's other
// callers it sets Provider inline — buildUsageLog drops it — so these rows are
// queryable by provider in usage reports. costUSD is the submit-time per-output
// cost (0 for failures and unpriced models); it rides in via Usage.CostUSD,
// which buildUsageLog copies onto the row.
func (s *Server) logReplicate(ctx context.Context, modelRequested, modelUsed, status, errType, costSource string, costUSD float64, latency time.Duration) {
	usage := agentmodel.Usage{AuthMode: agentmodel.AuthModeAPIKey, CostUSD: costUSD}
	rl := buildUsageLog(modelRequested, modelUsed, usage, latency, status, errType, costSource)
	rl.Provider = replicateProviderName
	s.writeRequestLog(ctx, rl)
}

func (s *Server) logReplicateFailure(ctx context.Context, modelRequested string, err error, latency time.Duration) {
	ae := agentmodel.Wrap(err)
	s.logReplicate(ctx, modelRequested, modelRequested, statusError, ae.Type, "", 0, latency)
}
