package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/api"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/stub"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
)

// tLimitWindow / tLimitsResp mirror the JSON the agentshell BackendLimits
// consumer decodes; we re-declare them here to assert the wire shape.
type tLimitWindow struct {
	LimitName    string   `json:"limit_name"`
	Window       string   `json:"window"`
	Unit         string   `json:"unit"`
	Limit        *float64 `json:"limit"`
	Remaining    *float64 `json:"remaining"`
	Used         *float64 `json:"used"`
	Source       string   `json:"source"`
	WindowStatus string   `json:"window_status"`
}

type tLimitsResp struct {
	Agent   string         `json:"agent"`
	Status  string         `json:"status"`
	Windows []tLimitWindow `json:"windows"`
}

func limitsIntPtr(i int) *int { return &i }

func getLimits(t *testing.T, ts *httptest.Server, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), "GET", ts.URL+"/v1/limits", nil)
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

func TestLimits_RequiresAuth(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp := getLimits(t, ts, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", resp.StatusCode)
	}
}

func TestLimits_ReportsConfiguredCaps(t *testing.T) {
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"sonnet": {
			{Name: "api/sonnet", Provider: &stub.Stub{NameValue: "anthropic"}, Model: "sonnet", Weight: 1, RPM: limitsIntPtr(60), TPM: limitsIntPtr(90000)},
		},
	})

	resp := getLimits(t, ts, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	var got tLimitsResp
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Agent != "agentmodel" {
		t.Errorf("agent: got %q, want agentmodel", got.Agent)
	}
	if got.Status != "ok" {
		t.Errorf("status: got %q, want ok", got.Status)
	}
	if len(got.Windows) != 2 {
		t.Fatalf("windows: got %d, want 2 (rpm+tpm): %+v", len(got.Windows), got.Windows)
	}

	byUnit := map[string]tLimitWindow{}
	for _, w := range got.Windows {
		byUnit[w.Unit] = w
		if w.Source != "agentmodel" {
			t.Errorf("window %s source: got %q, want agentmodel", w.LimitName, w.Source)
		}
		if w.WindowStatus != "ok" {
			t.Errorf("window %s status: got %q, want ok", w.LimitName, w.WindowStatus)
		}
		//  metering: with no traffic yet, used is 0 and the full cap remains.
		if w.Used == nil || *w.Used != 0 {
			t.Errorf("window %s used: got %v, want 0", w.LimitName, w.Used)
		}
		if w.Remaining == nil || w.Limit == nil || *w.Remaining != *w.Limit {
			t.Errorf("window %s remaining: got %v, want full cap %v", w.LimitName, w.Remaining, w.Limit)
		}
		if w.Window != "1m" {
			t.Errorf("window %s window: got %q, want 1m", w.LimitName, w.Window)
		}
	}
	rpm, ok := byUnit["requests"]
	if !ok {
		t.Fatalf("missing requests window")
	}
	if rpm.Limit == nil || *rpm.Limit != 60 {
		t.Errorf("rpm limit: got %v, want 60", rpm.Limit)
	}
	tpm, ok := byUnit["tokens"]
	if !ok {
		t.Fatalf("missing tokens window")
	}
	if tpm.Limit == nil || *tpm.Limit != 90000 {
		t.Errorf("tpm limit: got %v, want 90000", tpm.Limit)
	}
}

func TestLimits_NoCapsReturnsOKWithEmptyWindows(t *testing.T) {
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"gpt": {
			{Name: "oa/gpt", Provider: &stub.Stub{NameValue: "openai"}, Model: "gpt-4o", Weight: 1},
		},
	})

	resp := getLimits(t, ts, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	var got tLimitsResp
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != "ok" {
		t.Errorf("status: got %q, want ok", got.Status)
	}
	if len(got.Windows) != 0 {
		t.Errorf("windows: got %d, want 0 (no caps configured)", len(got.Windows))
	}
	// windows must be a JSON array, never null, so consumers can range over it.
	if got.Windows == nil {
		t.Errorf("windows must serialize as [] not null")
	}
}

func limitsFloatPtr(f float64) *float64 { return &f }

func TestLimits_ReportsBudgetCaps(t *testing.T) {
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"sonnet": {
			{Name: "api/sonnet", Provider: &stub.Stub{NameValue: "anthropic"}, Model: "sonnet", Weight: 1, RPM: limitsIntPtr(60)},
		},
	}, func(c *api.Config) {
		c.Budget = agentmodel.BudgetConfig{MaxBudget: limitsFloatPtr(100.5), BudgetDuration: "720h"}
		c.Keys = []agentmodel.KeyConfig{
			{Name: "ci", TokenEnv: "K_CI", MaxBudget: limitsFloatPtr(10), BudgetDuration: "24h"},
			{Name: "lifetime", TokenEnv: "K_LT", MaxBudget: limitsFloatPtr(5)},
			{Name: "uncapped", TokenEnv: "K_UN"}, // identity-only: no window emitted
		}
	})

	resp := getLimits(t, ts, testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	var got tLimitsResp
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != "ok" {
		t.Errorf("status: got %q, want ok (budget windows stay ok until live spend metering is implemented)", got.Status)
	}
	// 3 budget windows (org + 2 capped keys) + 1 rpm window.
	if len(got.Windows) != 4 {
		t.Fatalf("windows: got %d, want 4: %+v", len(got.Windows), got.Windows)
	}

	byName := map[string]tLimitWindow{}
	for _, w := range got.Windows {
		byName[w.LimitName] = w
	}

	org, ok := byName["org/default:budget"]
	if !ok {
		t.Fatalf("missing org budget window; have %v", byName)
	}
	if org.Unit != "usd" {
		t.Errorf("org unit: got %q, want usd", org.Unit)
	}
	if org.Limit == nil || *org.Limit != 100.5 {
		t.Errorf("org limit: got %v, want 100.5", org.Limit)
	}
	if org.Window != "720h" {
		t.Errorf("org window: got %q, want 720h", org.Window)
	}
	if org.Used != nil || org.Remaining != nil {
		t.Errorf("org used/remaining must be null until live spend metering is implemented (got used=%v remaining=%v)", org.Used, org.Remaining)
	}
	if org.WindowStatus != "ok" {
		t.Errorf("org status: got %q, want ok", org.WindowStatus)
	}

	ci, ok := byName["key/ci:budget"]
	if !ok {
		t.Fatalf("missing ci key budget window")
	}
	if ci.Limit == nil || *ci.Limit != 10 || ci.Window != "24h" || ci.Unit != "usd" {
		t.Errorf("ci window: got %+v", ci)
	}

	lt, ok := byName["key/lifetime:budget"]
	if !ok {
		t.Fatalf("missing lifetime key budget window")
	}
	if lt.Window != "" {
		t.Errorf("lifetime window field: got %q, want empty (lifetime cap)", lt.Window)
	}

	if _, ok := byName["key/uncapped:budget"]; ok {
		t.Errorf("uncapped key must not emit a budget window")
	}
	if _, ok := byName["sonnet/api/sonnet:rpm"]; !ok {
		t.Errorf("rpm window missing alongside budget windows; have %v", byName)
	}
}

func TestLimits_NoBudgetsNoBudgetWindows(t *testing.T) {
	ts, _ := newTestServer(t, map[string][]router.Deployment{
		"gpt": {
			{Name: "oa/gpt", Provider: &stub.Stub{NameValue: "openai"}, Model: "gpt-4o", Weight: 1},
		},
	})
	resp := getLimits(t, ts, testToken)
	defer resp.Body.Close()
	var got tLimitsResp
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, w := range got.Windows {
		if w.Unit == "usd" {
			t.Errorf("unexpected budget window %q with no budgets configured", w.LimitName)
		}
	}
}
