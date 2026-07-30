package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	modelstore "github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

// TestResolvePurgePeriod guards the `purge --older-than` override against
// non-positive durations, which would otherwise make the retention cutoff a
// future timestamp and delete the entire audit table.
func TestResolvePurgePeriod(t *testing.T) {
	const cfgPeriod = 720 * time.Hour

	cases := []struct {
		name        string
		cfgEnabled  bool
		override    string
		wantPeriod  time.Duration
		wantEnabled bool
		wantErr     bool
	}{
		{name: "no override passes config through (enabled)", cfgEnabled: true, override: "", wantPeriod: cfgPeriod, wantEnabled: true},
		{name: "no override passes config through (disabled)", cfgEnabled: false, override: "", wantPeriod: cfgPeriod, wantEnabled: false},
		{name: "valid override enables and replaces", cfgEnabled: false, override: "168h", wantPeriod: 168 * time.Hour, wantEnabled: true},
		{name: "negative override rejected", cfgEnabled: true, override: "-720h", wantErr: true},
		{name: "zero override rejected", cfgEnabled: true, override: "0", wantErr: true},
		{name: "zero-with-unit override rejected", cfgEnabled: true, override: "0h", wantErr: true},
		{name: "unparseable override rejected", cfgEnabled: true, override: "soon", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotPeriod, gotEnabled, err := resolvePurgePeriod(cfgPeriod, tc.cfgEnabled, tc.override)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolvePurgePeriod(%q) = (%v, %v, nil), want error", tc.override, gotPeriod, gotEnabled)
				}
				// On error the cutoff must never be usable: enabled=false so the
				// caller aborts before computing time.Now().Add(-period).
				if gotEnabled {
					t.Errorf("error case returned enabled=true, want false (would proceed to delete)")
				}
				return
			}
			if err != nil {
				t.Fatalf("resolvePurgePeriod(%q) unexpected error: %v", tc.override, err)
			}
			if gotPeriod != tc.wantPeriod {
				t.Errorf("period = %v, want %v", gotPeriod, tc.wantPeriod)
			}
			if gotEnabled != tc.wantEnabled {
				t.Errorf("enabled = %v, want %v", gotEnabled, tc.wantEnabled)
			}
			if !tc.wantErr && gotEnabled && gotPeriod <= 0 {
				t.Errorf("enabled with non-positive period %v would delete all rows", gotPeriod)
			}
		})
	}
}

func TestRunPurgeDeletesRowsOlderThanOverride(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "agentmodel.db")
	cfgPath := filepath.Join(dir, "config.yaml")
	writePurgeConfig(t, cfgPath, dbPath, "")

	st, err := modelstore.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	if err := st.LogRequest(ctx, modelstore.RequestLog{
		ID:             "old",
		OrgID:          "default",
		ModelRequested: "gpt",
		ModelUsed:      "gpt-4o",
		Provider:       "openai",
		Status:         "ok",
		CreatedAt:      now.Add(-48 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.LogRequest(ctx, modelstore.RequestLog{
		ID:             "new",
		OrgID:          "default",
		ModelRequested: "gpt",
		ModelUsed:      "gpt-4o",
		Provider:       "openai",
		Status:         "ok",
		CreatedAt:      now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	oldArgs := os.Args
	os.Args = []string{"agent-model", "purge", "--config", cfgPath, "--older-than", "24h"}
	defer func() { os.Args = oldArgs }()

	if err := runPurge(discardLogger()); err != nil {
		t.Fatalf("runPurge: %v", err)
	}

	st, err = modelstore.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.GetRequestLog(ctx, "old"); !errors.Is(err, modelstore.ErrNotFound) {
		t.Fatalf("old row error = %v, want ErrNotFound", err)
	}
	if got, err := st.GetRequestLog(ctx, "new"); err != nil || got.ID != "new" {
		t.Fatalf("new row = %+v err=%v", got, err)
	}
}

func TestRunPurgeRequiresRetentionOrOverride(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writePurgeConfig(t, cfgPath, filepath.Join(dir, "agentmodel.db"), "")

	oldArgs := os.Args
	os.Args = []string{"agent-model", "purge", "--config", cfgPath}
	defer func() { os.Args = oldArgs }()

	if err := runPurge(discardLogger()); err == nil {
		t.Fatal("expected error when retention is disabled and --older-than is absent")
	}
}

func TestRunPurgeUsesConfiguredRetention(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "agentmodel.db")
	cfgPath := filepath.Join(dir, "config.yaml")
	writePurgeConfig(t, cfgPath, dbPath, "retention:\n  period: 24h\n")

	oldArgs := os.Args
	os.Args = []string{"agent-model", "purge", "--config", cfgPath}
	defer func() { os.Args = oldArgs }()

	if err := runPurge(discardLogger()); err != nil {
		t.Fatalf("runPurge with configured retention: %v", err)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("expected db file at %s: %v", dbPath, err)
	}
}

func TestStartRetentionPurge(t *testing.T) {
	st := &retentionStore{purged: make(chan time.Time, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startRetentionPurge(ctx, discardLogger(), st, agentmodel.RetentionConfig{
		Period:   "24h",
		Interval: "1h",
	})

	select {
	case before := <-st.purged:
		if time.Since(before) < 23*time.Hour {
			t.Fatalf("purge cutoff = %s, want roughly 24h old", before)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for immediate retention purge")
	}
	cancel()
}

func TestStartRetentionPurgeDisabledOrInvalid(t *testing.T) {
	for _, rc := range []agentmodel.RetentionConfig{
		{},
		{Period: "-1h"},
	} {
		st := &retentionStore{purged: make(chan time.Time, 1)}
		ctx, cancel := context.WithCancel(context.Background())
		startRetentionPurge(ctx, discardLogger(), st, rc)
		cancel()
		select {
		case before := <-st.purged:
			t.Fatalf("unexpected purge for %+v at %s", rc, before)
		default:
		}
	}
}

func writePurgeConfig(t *testing.T, path string, dbPath string, extra string) {
	t.Helper()

	cfg := "listen: \":0\"\n" +
		"db: " + dbPath + "\n" +
		"auth:\n  bearer_token_env: AGENT_MODEL_TOKEN\n" +
		"model_list:\n" +
		"  - model_name: gpt\n" +
		"    deployments:\n" +
		"      - provider: openai\n" +
		"        model: gpt-4o\n" +
		"        auth_mode: api_key\n" +
		"        api_key_env: OPENAI_API_KEY\n" +
		"        weight: 100\n" +
		extra
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

type retentionStore struct {
	purged chan time.Time
}

func (s *retentionStore) Ping(context.Context) error { return nil }
func (s *retentionStore) LogRequest(context.Context, modelstore.RequestLog) error {
	return nil
}
func (s *retentionStore) GetRequestLog(context.Context, string) (modelstore.RequestLog, error) {
	return modelstore.RequestLog{}, modelstore.ErrNotFound
}
func (s *retentionStore) ListByOrg(context.Context, string, time.Time, int) ([]modelstore.RequestLog, error) {
	return nil, nil
}
func (s *retentionStore) ListByAPIKey(context.Context, string, time.Time, int) ([]modelstore.RequestLog, error) {
	return nil, nil
}
func (s *retentionStore) SumCostByOrg(context.Context, string, time.Time) (float64, error) {
	return 0, nil
}
func (s *retentionStore) SumCostByAPIKey(context.Context, string, time.Time) (float64, error) {
	return 0, nil
}
func (s *retentionStore) CountRequestsByOrg(context.Context, string, time.Time, string) (int64, error) {
	return 0, nil
}
func (s *retentionStore) UsageReport(context.Context, modelstore.UsageFilter) ([]modelstore.UsageRow, error) {
	return nil, nil
}
func (s *retentionStore) Purge(ctx context.Context, before time.Time) (int64, error) {
	select {
	case s.purged <- before:
	default:
	}
	return 1, nil
}
func (s *retentionStore) CreateKey(context.Context, modelstore.ManagedKey) error { return nil }
func (s *retentionStore) GetKeyByHash(context.Context, string) (modelstore.ManagedKey, error) {
	return modelstore.ManagedKey{}, modelstore.ErrNotFound
}
func (s *retentionStore) GetKeyByID(context.Context, string) (modelstore.ManagedKey, error) {
	return modelstore.ManagedKey{}, modelstore.ErrNotFound
}
func (s *retentionStore) ListKeys(context.Context) ([]modelstore.ManagedKey, error) { return nil, nil }
func (s *retentionStore) SetKeyDisabled(context.Context, string, bool) error        { return nil }
func (s *retentionStore) DeleteKey(context.Context, string) error                   { return nil }
func (s *retentionStore) Close() error                                              { return nil }
