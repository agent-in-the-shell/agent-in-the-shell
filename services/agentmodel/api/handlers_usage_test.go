package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

func usageGet(t *testing.T, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), "GET", url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

func seedUsage(t *testing.T, st *store.SQLiteStore) {
	t.Helper()
	now := time.Now().UTC()
	logs := []store.RequestLog{
		{ID: "u1", OrgID: "default", APIKeyHash: "h", ModelUsed: "gpt-4o", Provider: "openai", AuthMode: "api_key", PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, CostUSD: 0.02, Status: "ok", CreatedAt: now.Add(-time.Hour)},
		{ID: "u2", OrgID: "default", APIKeyHash: "h", ModelUsed: "gpt-4o", Provider: "openai", AuthMode: "api_key", PromptTokens: 200, CompletionTokens: 60, TotalTokens: 260, CostUSD: 0.03, Status: "ok", CreatedAt: now.Add(-2 * time.Hour)},
		{ID: "u3", OrgID: "default", APIKeyHash: "h", ModelUsed: "claude-3", Provider: "anthropic", AuthMode: "subscription", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, CostUSD: 0, Status: "error", CreatedAt: now.Add(-3 * time.Hour)},
	}
	for _, rl := range logs {
		if err := st.LogRequest(context.Background(), rl); err != nil {
			t.Fatalf("seed %s: %v", rl.ID, err)
		}
	}
}

func TestUsageEndpoint_GroupByModel(t *testing.T) {
	ts, st := newTestServer(t, nil)
	seedUsage(t, st)

	resp := usageGet(t, ts.URL+"/v1/usage?group_by=model&since=7d", testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got struct {
		Object  string                     `json:"object"`
		GroupBy string                     `json:"group_by"`
		Window  struct{ Start, End int64 } `json:"window"`
		Rows    []store.UsageRow           `json:"rows"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Object != "usage.report" || got.GroupBy != "model" {
		t.Errorf("envelope = %+v", got)
	}
	if len(got.Rows) != 2 {
		t.Fatalf("want 2 rows, got %d: %+v", len(got.Rows), got.Rows)
	}
	if got.Rows[0].Key != "gpt-4o" || got.Rows[0].TotalTokens != 410 || got.Rows[0].Requests != 2 {
		t.Errorf("row0 = %+v, want gpt-4o/410/2", got.Rows[0])
	}
}

func TestUsageEndpoint_RejectsClientOrg(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp := usageGet(t, ts.URL+"/v1/usage?org=evil", testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (client ?org= must be rejected)", resp.StatusCode)
	}
}

func TestUsageEndpoint_InvalidGroupBy(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp := usageGet(t, ts.URL+"/v1/usage?group_by=org", testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an off-allowlist group_by", resp.StatusCode)
	}
}

func TestUsageEndpoint_RequiresAuth(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp := usageGet(t, ts.URL+"/v1/usage", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestUsageEndpoint_EmptyWindow(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp := usageGet(t, ts.URL+"/v1/usage?since=1h", testToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got struct {
		Rows []store.UsageRow `json:"rows"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Rows == nil {
		t.Fatal("rows should be [] not null on an empty window")
	}
}
