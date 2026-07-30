package api_test

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/replicate"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
)

// replicateUpstreamStub stands in for api.replicate.com. It records the
// Authorization header it received and serves prediction JSON whose urls.* point
// (as the real API does) at https://api.replicate.com.
func replicateUpstreamStub(t *testing.T, gotAuth *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/predictions":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"pred1","model":"echo/model","status":"starting","urls":{"get":"https://api.replicate.com/v1/predictions/pred1","cancel":"https://api.replicate.com/v1/predictions/pred1/cancel","stream":"https://stream.replicate.com/v1/x"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/models/minimax/speech-02-turbo/predictions":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"pred2","model":"minimax/speech-02-turbo","status":"processing","urls":{"get":"https://api.replicate.com/v1/predictions/pred2"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/predictions/pred1":
			_, _ = io.WriteString(w, `{"id":"pred1","model":"echo/model","status":"succeeded","output":"https://replicate.delivery/out.mp4","urls":{"get":"https://api.replicate.com/v1/predictions/pred1"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/predictions/pred1/cancel":
			_, _ = io.WriteString(w, `{"id":"pred1","model":"echo/model","status":"canceled","urls":{"get":"https://api.replicate.com/v1/predictions/pred1","cancel":"https://api.replicate.com/v1/predictions/pred1/cancel"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"detail":"unexpected stub path"}`)
		}
	}))
}

func replicateDeployments(upstreamURL string) map[string][]router.Deployment {
	key := &auth.StaticKey{HeaderName: "Authorization", Prefix: "Bearer ", Token: "r8_gateway"}
	return map[string][]router.Deployment{
		"replicate": {{
			Name:     "replicate/passthrough",
			Provider: replicate.NewWithBaseURL(key, upstreamURL),
			Model:    "passthrough",
			Weight:   100,
		}},
	}
}

func mustGet(t *testing.T, ts *httptest.Server, path, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

func TestCreatePrediction_ForwardsRewritesAndLogs(t *testing.T) {
	var gotAuth string
	upstream := replicateUpstreamStub(t, &gotAuth)
	defer upstream.Close()
	ts, st := newTestServer(t, replicateDeployments(upstream.URL))

	resp := mustPost(t, ts, "/v1/predictions", map[string]any{
		"version": "abc123",
		"input":   map[string]any{"prompt": "hi"},
	}, testToken)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (copied from upstream)", resp.StatusCode)
	}
	// The gateway injected its OWN Replicate credential upstream.
	if gotAuth != "Bearer r8_gateway" {
		t.Errorf("upstream Authorization = %q, want gateway-injected token", gotAuth)
	}

	var pred struct {
		ID   string `json:"id"`
		URLs struct {
			Get    string `json:"get"`
			Cancel string `json:"cancel"`
			Stream string `json:"stream"`
		} `json:"urls"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &pred); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, body)
	}
	// urls.get/cancel are rewritten back to the gateway so polls/cancels return
	// through us; urls.stream (a different host) is left untouched in the MVP.
	if !strings.HasPrefix(pred.URLs.Get, ts.URL+"/v1/predictions/pred1") {
		t.Errorf("urls.get = %q, want rewritten to gateway %q", pred.URLs.Get, ts.URL)
	}
	if !strings.HasPrefix(pred.URLs.Cancel, ts.URL+"/v1/predictions/pred1/cancel") {
		t.Errorf("urls.cancel = %q, want rewritten to gateway", pred.URLs.Cancel)
	}
	if strings.Contains(pred.URLs.Get, "api.replicate.com") {
		t.Errorf("urls.get still points at api.replicate.com: %q", pred.URLs.Get)
	}
	if pred.URLs.Stream != "https://stream.replicate.com/v1/x" {
		t.Errorf("urls.stream = %q, want left untouched", pred.URLs.Stream)
	}

	// One audit row, attributed to replicate, $0 / unpriced.
	logs, err := st.ListByOrg(context.Background(), "default", time.Now().Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("logged %d rows, want 1", len(logs))
	}
	row := logs[0]
	if row.Provider != "replicate" {
		t.Errorf("row.Provider = %q, want replicate (must be set inline)", row.Provider)
	}
	if row.ModelRequested != "abc123" {
		t.Errorf("row.ModelRequested = %q, want abc123 (body version)", row.ModelRequested)
	}
	if row.ModelUsed != "echo/model" {
		t.Errorf("row.ModelUsed = %q, want echo/model (response model)", row.ModelUsed)
	}
	if row.Status != "ok" {
		t.Errorf("row.Status = %q, want ok", row.Status)
	}
	if row.CostSource != "unpriced" {
		t.Errorf("row.CostSource = %q, want unpriced", row.CostSource)
	}
	if row.AuthMode != "api_key" {
		t.Errorf("row.AuthMode = %q, want api_key", row.AuthMode)
	}
}

func TestCreatePrediction_ModelBasedPathAttribution(t *testing.T) {
	var gotAuth string
	upstream := replicateUpstreamStub(t, &gotAuth)
	defer upstream.Close()
	ts, st := newTestServer(t, replicateDeployments(upstream.URL))

	resp := mustPost(t, ts, "/v1/models/minimax/speech-02-turbo/predictions", map[string]any{
		"input": map[string]any{"text": "hi"},
	}, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	logs, err := st.ListByOrg(context.Background(), "default", time.Now().Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("logged %d rows, want 1", len(logs))
	}
	// model_requested comes from the {owner}/{name} path segments.
	if logs[0].ModelRequested != "minimax/speech-02-turbo" {
		t.Errorf("row.ModelRequested = %q, want minimax/speech-02-turbo", logs[0].ModelRequested)
	}
}

func TestGetPrediction_RewritesAndDoesNotLog(t *testing.T) {
	var gotAuth string
	upstream := replicateUpstreamStub(t, &gotAuth)
	defer upstream.Close()
	ts, st := newTestServer(t, replicateDeployments(upstream.URL))

	resp := mustGet(t, ts, "/v1/predictions/pred1", testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var pred struct {
		Output string `json:"output"`
		URLs   struct {
			Get string `json:"get"`
		} `json:"urls"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &pred); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(pred.URLs.Get, ts.URL) {
		t.Errorf("poll urls.get = %q, want rewritten to gateway", pred.URLs.Get)
	}
	// Output asset URLs (replicate.delivery) are NOT rewritten.
	if pred.Output != "https://replicate.delivery/out.mp4" {
		t.Errorf("output = %q, want left untouched", pred.Output)
	}

	// The poll is a read: no audit row written (matches getVideo).
	logs, err := st.ListByOrg(context.Background(), "default", time.Now().Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	if len(logs) != 0 {
		t.Errorf("poll logged %d rows, want 0", len(logs))
	}
}

func TestCancelPrediction_RewritesAndDoesNotLog(t *testing.T) {
	var gotAuth string
	upstream := replicateUpstreamStub(t, &gotAuth)
	defer upstream.Close()
	ts, st := newTestServer(t, replicateDeployments(upstream.URL))

	resp := mustPost(t, ts, "/v1/predictions/pred1/cancel", nil, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var pred struct {
		Status string `json:"status"`
		URLs   struct {
			Get    string `json:"get"`
			Cancel string `json:"cancel"`
		} `json:"urls"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &pred); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pred.Status != "canceled" {
		t.Fatalf("status = %q, want canceled", pred.Status)
	}
	if !strings.HasPrefix(pred.URLs.Get, ts.URL) || !strings.HasPrefix(pred.URLs.Cancel, ts.URL) {
		t.Fatalf("urls not rewritten to gateway: %+v", pred.URLs)
	}
	logs, err := st.ListByOrg(context.Background(), "default", time.Now().Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	if len(logs) != 0 {
		t.Errorf("cancel logged %d rows, want 0", len(logs))
	}
}

func TestCreatePrediction_PerOutputCostLogged(t *testing.T) {
	// Stub the kling model-based create path; the logged cost is derived from the
	// REQUEST body (mode/audio/duration), independent of the stub's response.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"k1","model":"kwaivgi/kling-v3-omni-video","status":"starting","urls":{"get":"https://api.replicate.com/v1/predictions/k1"}}`)
	}))
	defer upstream.Close()
	ts, st := newTestServer(t, replicateDeployments(upstream.URL))

	resp := mustPost(t, ts, "/v1/models/kwaivgi/kling-v3-omni-video/predictions", map[string]any{
		"input": map[string]any{"prompt": "a fox", "mode": "pro", "generate_audio": true, "duration": 10},
	}, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	logs, err := st.ListByOrg(context.Background(), "default", time.Now().Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("ListByOrg: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("logged %d rows, want 1", len(logs))
	}
	row := logs[0]
	if row.CostSource != "priced" {
		t.Errorf("row.CostSource = %q, want priced", row.CostSource)
	}
	if want := 0.28 * 10; math.Abs(row.CostUSD-want) > 1e-9 {
		t.Errorf("row.CostUSD = %v, want %v (pro+audio $0.28 × 10s)", row.CostUSD, want)
	}
	if row.Provider != "replicate" || row.ModelRequested != "kwaivgi/kling-v3-omni-video" {
		t.Errorf("attribution: provider=%q model=%q", row.Provider, row.ModelRequested)
	}
}

func TestPredictions_RequireBearerToken(t *testing.T) {
	var gotAuth string
	upstream := replicateUpstreamStub(t, &gotAuth)
	defer upstream.Close()
	ts, _ := newTestServer(t, replicateDeployments(upstream.URL))

	resp := mustPost(t, ts, "/v1/predictions", map[string]any{"version": "x"}, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (passthrough is behind bearerAuth)", resp.StatusCode)
	}
}
