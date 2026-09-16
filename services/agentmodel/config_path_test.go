package agentmodel_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
)

// : DefaultConfigPath built ~/.config/agentmodel/config.yaml by hand and
// never called herospath, so it sat outside the canonical heros/ tree and
// XDG_CONFIG_HOME was silently ignored for it — while it worked for the
// database. An operator who relocates their config tree got a partial move with
// no error.
func TestDefaultConfigPath_HonorsXDGConfigHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_MODEL_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", dir)
	// HOME must be isolated too: the legacy fallback is $HOME-relative, and on a
	// developer machine that file exists — without this the test reads the real
	// ~/.config/agentmodel/config.yaml and passes or fails for the wrong reason.
	t.Setenv("HOME", t.TempDir())

	want := filepath.Join(dir, "heros", "agentmodel", "config.yaml")
	if got := agentmodel.DefaultConfigPath(); got != want {
		t.Errorf("DefaultConfigPath() = %q, want %q", got, want)
	}
}

// A fresh install converges on the canonical heros/ layout, matching where the
// database already goes.
func TestDefaultConfigPath_FreshInstallIsCanonical(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_MODEL_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", home)

	want := filepath.Join(home, ".config", "heros", "agentmodel", "config.yaml")
	if got := agentmodel.DefaultConfigPath(); got != want {
		t.Errorf("DefaultConfigPath() = %q, want %q", got, want)
	}
}

// The whole reason to use ResolveConfigLegacy rather than moving outright:
// agent-model is released, so an install that already has a config at the old
// path must keep reading it. Moving someone's config out from under them would
// look like "all my providers disappeared".
func TestDefaultConfigPath_ExistingLegacyConfigWinsInPlace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_MODEL_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", home)

	legacy := filepath.Join(home, ".config", "agentmodel", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(legacy, []byte("listen: \":8080\"\n"), 0o600); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}

	if got := agentmodel.DefaultConfigPath(); got != legacy {
		t.Errorf("DefaultConfigPath() = %q, want the existing legacy file %q", got, legacy)
	}
}

// Once the canonical file exists it wins, even with the legacy one still
// present — that is the "already migrated" case, and without it a stale legacy
// file would shadow the config the operator actually edits.
func TestDefaultConfigPath_CanonicalWinsOverLegacyWhenBothExist(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_MODEL_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", home)

	for _, p := range []string{
		filepath.Join(home, ".config", "agentmodel", "config.yaml"),
		filepath.Join(home, ".config", "heros", "agentmodel", "config.yaml"),
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte("listen: \":8080\"\n"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	want := filepath.Join(home, ".config", "heros", "agentmodel", "config.yaml")
	if got := agentmodel.DefaultConfigPath(); got != want {
		t.Errorf("DefaultConfigPath() = %q, want the canonical %q", got, want)
	}
}

// The operator escape hatch keeps winning over everything.
func TestDefaultConfigPath_EnvOverrideStillWins(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AGENT_MODEL_CONFIG", "/custom/agentmodel.yaml")
	if got := agentmodel.DefaultConfigPath(); got != "/custom/agentmodel.yaml" {
		t.Errorf("DefaultConfigPath() = %q, want the env override", got)
	}
}

// The legacy-in-place rule has a sharp edge worth pinning: once the old config
// exists, XDG_CONFIG_HOME stops relocating it. That is deliberate — never move a
// running install's config — but it means "set XDG and everything moves" is not
// true, and the SKILL.md says so.
func TestDefaultConfigPath_LegacyDefeatsXDGConfigHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_MODEL_CONFIG", "")
	t.Setenv("HOME", home)

	legacy := filepath.Join(home, ".config", "agentmodel", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(legacy, []byte("listen: \":8080\"\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if got := agentmodel.DefaultConfigPath(); got != legacy {
		t.Errorf("DefaultConfigPath() = %q, want the in-place legacy file %q", got, legacy)
	}
}
