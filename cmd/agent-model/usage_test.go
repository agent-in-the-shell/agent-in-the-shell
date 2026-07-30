package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	modelstore "github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

func writeUsageDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage.db")
	st, err := modelstore.OpenSQLite(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now().UTC()
	logs := []modelstore.RequestLog{
		{ID: "a", OrgID: "default", APIKeyHash: "h", ModelUsed: "gpt-4o", Provider: "", AuthMode: "subscription", PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500, CostUSD: 0, Status: "ok", CreatedAt: now.Add(-time.Hour)},
		{ID: "b", OrgID: "default", APIKeyHash: "h", ModelUsed: "gpt-4o", Provider: "", AuthMode: "subscription", PromptTokens: 2000, CompletionTokens: 100, TotalTokens: 2100, CostUSD: 0, Status: "error", CreatedAt: now.Add(-2 * time.Hour)},
	}
	for _, rl := range logs {
		if err := st.LogRequest(context.Background(), rl); err != nil {
			t.Fatalf("seed %s: %v", rl.ID, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDoUsage_Table(t *testing.T) {
	path := writeUsageDB(t)
	var out, errOut bytes.Buffer
	if err := doUsage([]string{"--db", path, "--by", "model", "--since", "7d"}, &out, &errOut); err != nil {
		t.Fatalf("doUsage: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "gpt-4o") || !strings.Contains(s, "3,600") {
		t.Errorf("table missing model or thousands-separated total tokens:\n%s", s)
	}
	// The provider canary should fire (both rows have provider='').
	if !strings.Contains(errOut.String(), "unset provider") {
		t.Errorf("expected a provider canary warning, got stderr:\n%s", errOut.String())
	}
}

func TestDoUsage_JSON(t *testing.T) {
	path := writeUsageDB(t)
	var out, errOut bytes.Buffer
	if err := doUsage([]string{"--db", path, "--by", "model", "--json"}, &out, &errOut); err != nil {
		t.Fatalf("doUsage: %v", err)
	}
	var env struct {
		Object   string                `json:"object"`
		GroupBy  string                `json:"group_by"`
		Rows     []modelstore.UsageRow `json:"rows"`
		Warnings []string              `json:"warnings"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out.String())
	}
	if env.Object != "usage.report" || env.GroupBy != "model" {
		t.Errorf("envelope = %+v", env)
	}
	if len(env.Rows) != 1 || env.Rows[0].TotalTokens != 3600 {
		t.Fatalf("rows = %+v, want one gpt-4o row with 3600 total tokens", env.Rows)
	}
	if env.Rows[0].ErrorRate != 0.5 {
		t.Errorf("error_rate = %v, want 0.5", env.Rows[0].ErrorRate)
	}
	// JSON mode routes warnings into the structured array, not stderr.
	if len(env.Warnings) == 0 {
		t.Error("expected warnings[] in JSON output")
	}
	if errOut.Len() != 0 {
		t.Errorf("JSON mode should not write canary to stderr, got:\n%s", errOut.String())
	}
}

func TestDoUsage_BadDimension(t *testing.T) {
	path := writeUsageDB(t)
	var out, errOut bytes.Buffer
	if err := doUsage([]string{"--db", path, "--by", "org"}, &out, &errOut); err == nil {
		t.Fatal("want error for an off-allowlist --by, got nil")
	}
}

func TestDoUsage_DefaultConfigFallback(t *testing.T) {
	dbPath := writeUsageDB(t)
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := "db: " + dbPath + "\n" +
		"model_list:\n" +
		"  - model_name: gpt\n" +
		"    deployments:\n" +
		"      - provider: openai\n" +
		"        model: gpt-4o\n" +
		"        auth_mode: api_key\n" +
		"        api_key_env: OPENAI_API_KEY\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_MODEL_CONFIG", cfgPath)

	var out, errOut bytes.Buffer
	if err := doUsage([]string{"--by", "model"}, &out, &errOut); err != nil {
		t.Fatalf("doUsage without --db/--config should fall back to AGENT_MODEL_CONFIG: %v", err)
	}
	if !strings.Contains(out.String(), "gpt-4o") {
		t.Errorf("report missing seeded model:\n%s", out.String())
	}
}

func TestDoUsage_DefaultConfigMissing(t *testing.T) {
	t.Setenv("AGENT_MODEL_CONFIG", filepath.Join(t.TempDir(), "absent.yaml"))
	var out, errOut bytes.Buffer
	if err := doUsage([]string{"--by", "model"}, &out, &errOut); err == nil {
		t.Fatal("want error when the default config does not exist")
	}
}

func TestHumanInt(t *testing.T) {
	cases := map[int64]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567", -1500: "-1,500"}
	for in, want := range cases {
		if got := humanInt(in); got != want {
			t.Errorf("humanInt(%d) = %q, want %q", in, got, want)
		}
	}
}
