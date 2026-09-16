package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
)

func TestParseModelSelection(t *testing.T) {
	got, err := parseNumberSelection("1, 3-5,4", 6)
	if err != nil {
		t.Fatal(err)
	}
	want := []int{0, 2, 3, 4}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selection = %v, want %v", got, want)
	}
}

func TestParseModelSelectionRejectsOutOfRange(t *testing.T) {
	if _, err := parseNumberSelection("2-4", 3); err == nil {
		t.Fatal("expected out-of-range error")
	}
}

func TestAppendModelEntriesPreservesCommentsAndExistingConfig(t *testing.T) {
	original := []byte(`# gateway comment
listen: ":8080"
auth:
  bearer_token_env: AGENT_MODEL_TOKEN
model_list:
  # existing model
  - model_name: old
    deployments:
      - provider: openai
        model: old-upstream
        api_key_env: OPENAI_API_KEY
        weight: 100
telemetry:
  metrics:
    enabled: false
`)
	weight := 1
	updated, err := appendModelEntries(original, []agentmodel.ModelEntry{{
		ModelName: "new-model",
		Deployments: []agentmodel.DeploymentConfig{{
			Provider: "azure", Model: "new-model", BaseURL: "https://example.test",
			APIVersion: "2024-12-01-preview", DeploymentName: "new-model", Weight: &weight,
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	text := string(updated)
	for _, want := range []string{"# gateway comment", "# existing model", "model_name: old", "model_name: new-model", "telemetry:"} {
		if !strings.Contains(text, want) {
			t.Errorf("updated YAML missing %q:\n%s", want, text)
		}
	}
}

func TestAtomicReplaceConfigUses0600AndDetectsConflict(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original := []byte("model_list: []\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeOwner := before.Sys().(*syscall.Stat_t)
	if err := atomicReplaceConfig(path, original, []byte("model_list: [new]\n")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
	afterOwner := info.Sys().(*syscall.Stat_t)
	if afterOwner.Uid != beforeOwner.Uid || afterOwner.Gid != beforeOwner.Gid {
		t.Fatalf("owner changed from %d:%d to %d:%d", beforeOwner.Uid, beforeOwner.Gid, afterOwner.Uid, afterOwner.Gid)
	}
	if err := atomicReplaceConfig(path, original, []byte("bad\n")); err == nil {
		t.Fatal("expected concurrent-change error")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "model_list: [new]\n" {
		t.Fatalf("conflict changed file to %q", got)
	}
}
