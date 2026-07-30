package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// discardLogger returns a logger that drops all output, so test logs stay quiet
// while warn/error paths still execute.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ─── runMigrate ──────────────────────────────────────────────────────────────

func TestRunMigrateCreatesDBFile(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "x.db")
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfgYAML := "listen: \":0\"\n" +
		"db: " + dbPath + "\n" +
		"auth:\n  bearer_token_env: AGENT_MODEL_TOKEN\n" +
		"model_list:\n" +
		"  - model_name: gpt\n" +
		"    deployments:\n" +
		"      - provider: openai\n" +
		"        model: gpt-4\n" +
		"        auth_mode: api_key\n" +
		"        api_key_env: OPENAI_API_KEY\n" +
		"        weight: 100\n"
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	oldArgs := os.Args
	os.Args = []string{"agent-model", "migrate", "--config", cfgPath}
	defer func() { os.Args = oldArgs }()

	if err := runMigrate(discardLogger()); err != nil {
		t.Fatalf("runMigrate: %v", err)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("expected db file at %s, stat err: %v", dbPath, err)
	}
}

func TestRunMigrateMissingConfigFlag(t *testing.T) {
	oldArgs := os.Args
	os.Args = []string{"agent-model", "migrate"}
	defer func() { os.Args = oldArgs }()
	if err := runMigrate(discardLogger()); err == nil {
		t.Fatal("expected error when --config is missing")
	}
}

func TestRunMigrateBadConfigPath(t *testing.T) {
	oldArgs := os.Args
	os.Args = []string{"agent-model", "migrate", "--config", filepath.Join(t.TempDir(), "nope.yaml")}
	defer func() { os.Args = oldArgs }()
	if err := runMigrate(discardLogger()); err == nil {
		t.Fatal("expected error when config file does not exist")
	}
}
