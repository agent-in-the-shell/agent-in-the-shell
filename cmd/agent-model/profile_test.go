package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
)

// captureStdout swaps os.Stdout for a pipe, runs fn, and returns everything fn
// wrote to stdout. The profile --json paths write directly to os.Stdout, so a
// pipe swap is the only way to observe them.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w

	// Drain the read end concurrently so writers never block on a full pipe
	// buffer, then restore os.Stdout before returning.
	done := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- b
	}()

	runErr := fn()
	_ = w.Close()
	os.Stdout = orig
	out := <-done
	return string(out), runErr
}

// seedProfile creates <base>/<name>/auth.json so the profile is discoverable.
func seedProfile(t *testing.T, base, name string) {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"anthropic":{"type":"oauth"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ─── runProfileList ──────────────────────────────────────────────────────────

func TestRunProfileListText(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	t.Setenv(auth.ProfileEnvVar, "")
	seedProfile(t, base, "work")
	seedProfile(t, base, "personal")
	if err := auth.WriteActiveProfile("work"); err != nil {
		t.Fatal(err)
	}

	out, err := captureStdout(t, func() error { return runProfileList(nil) })
	if err != nil {
		t.Fatalf("runProfileList: %v", err)
	}
	// Active profile is marked with '*'; others with ' '. Sorted alphabetically.
	if !strings.Contains(out, "* work") {
		t.Errorf("expected active marker on work, got:\n%s", out)
	}
	if !strings.Contains(out, "  personal") {
		t.Errorf("expected inactive personal line, got:\n%s", out)
	}
	// personal sorts before work.
	if strings.Index(out, "personal") > strings.Index(out, "work") {
		t.Errorf("expected sorted output (personal before work), got:\n%s", out)
	}
}

func TestRunProfileListJSON(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	t.Setenv(auth.ProfileEnvVar, "")
	seedProfile(t, base, "work")
	seedProfile(t, base, "personal")
	if err := auth.WriteActiveProfile("work"); err != nil {
		t.Fatal(err)
	}

	out, err := captureStdout(t, func() error { return runProfileList([]string{"--json"}) })
	if err != nil {
		t.Fatalf("runProfileList --json: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("unmarshal json: %v\nraw: %s", err, out)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	activeByName := map[string]bool{}
	for _, row := range rows {
		activeByName[row["name"].(string)] = row["active"].(bool)
	}
	if !activeByName["work"] {
		t.Errorf("work should be active in json output: %v", rows)
	}
	if activeByName["personal"] {
		t.Errorf("personal should not be active in json output: %v", rows)
	}
}

// ─── runProfileShow ──────────────────────────────────────────────────────────

func TestRunProfileShowTextWithAuth(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	t.Setenv(auth.ProfileEnvVar, "work")
	seedProfile(t, base, "work")

	out, err := captureStdout(t, func() error { return runProfileShow(nil) })
	if err != nil {
		t.Fatalf("runProfileShow: %v", err)
	}
	if strings.TrimSpace(out) != "work" {
		t.Errorf("show output = %q, want work", strings.TrimSpace(out))
	}
}

func TestRunProfileShowTextMissingAuthErrors(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	t.Setenv(auth.ProfileEnvVar, "work")
	// no auth.json seeded -> show should print the name then return an error.
	out, err := captureStdout(t, func() error { return runProfileShow(nil) })
	if err == nil {
		t.Fatal("expected error when profile has no auth.json")
	}
	if strings.TrimSpace(out) != "work" {
		t.Errorf("show should still print the profile name, got %q", strings.TrimSpace(out))
	}
}

func TestRunProfileShowJSONHasAuth(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	t.Setenv(auth.ProfileEnvVar, "work")
	seedProfile(t, base, "work")

	out, err := captureStdout(t, func() error { return runProfileShow([]string{"--json"}) })
	if err != nil {
		t.Fatalf("runProfileShow --json: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("unmarshal: %v\nraw: %s", err, out)
	}
	if obj["profile"] != "work" {
		t.Errorf("profile = %v, want work", obj["profile"])
	}
	if obj["has_auth"] != true {
		t.Errorf("has_auth = %v, want true", obj["has_auth"])
	}
	if td, ok := obj["token_dir"].(string); !ok || !strings.Contains(td, "work") {
		t.Errorf("token_dir = %v, want a path containing work", obj["token_dir"])
	}
}

func TestRunProfileShowJSONNoAuth(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	t.Setenv(auth.ProfileEnvVar, "work")
	// JSON path reports has_auth=false but does NOT return an error.
	out, err := captureStdout(t, func() error { return runProfileShow([]string{"--json"}) })
	if err != nil {
		t.Fatalf("runProfileShow --json (no auth) should not error: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("unmarshal: %v\nraw: %s", err, out)
	}
	if obj["has_auth"] != false {
		t.Errorf("has_auth = %v, want false", obj["has_auth"])
	}
}

// ─── runProfileMigrate ───────────────────────────────────────────────────────

func TestRunProfileMigrateNothingToMigrate(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	t.Setenv(auth.ProfileEnvVar, "")
	out, err := captureStdout(t, func() error { return runProfileMigrate(nil) })
	if err != nil {
		t.Fatalf("runProfileMigrate: %v", err)
	}
	if !strings.Contains(out, "Nothing to migrate") {
		t.Errorf("expected 'Nothing to migrate', got:\n%s", out)
	}
}

func TestRunProfileMigrateDryRun(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	t.Setenv(auth.ProfileEnvVar, "")
	// Seed a legacy <base>/auth.json so migration is pending.
	if err := os.WriteFile(filepath.Join(base, "auth.json"), []byte(`{"anthropic":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error { return runProfileMigrate([]string{"--dry-run"}) })
	if err != nil {
		t.Fatalf("runProfileMigrate --dry-run: %v", err)
	}
	if !strings.Contains(out, "Would rename") || !strings.Contains(out, "dry-run") {
		t.Errorf("expected dry-run description, got:\n%s", out)
	}
	// Dry-run must not move the file.
	if _, err := os.Stat(filepath.Join(base, "auth.json")); err != nil {
		t.Errorf("dry-run should not move legacy auth.json: %v", err)
	}
}

func TestRunProfileMigratePerformsMigration(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	t.Setenv(auth.ProfileEnvVar, "")
	if err := os.WriteFile(filepath.Join(base, "auth.json"), []byte(`{"anthropic":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error { return runProfileMigrate(nil) })
	if err != nil {
		t.Fatalf("runProfileMigrate: %v", err)
	}
	if !strings.Contains(out, "Renaming") || !strings.Contains(out, "Migrated") {
		t.Errorf("expected migration output, got:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(base, "auth.json")); !os.IsNotExist(err) {
		t.Errorf("legacy auth.json should be gone after migration, stat err: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "default", "auth.json")); err != nil {
		t.Errorf("expected <base>/default/auth.json after migration: %v", err)
	}
}

func TestRunProfileMigrateRejectsStrayArg(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	t.Setenv(auth.ProfileEnvVar, "")
	if err := runProfileMigrate([]string{"oops"}); err == nil {
		t.Fatal("expected error for stray positional argument")
	}
}

func TestRunProfileMigrateBothExist(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	t.Setenv(auth.ProfileEnvVar, "")
	if err := os.WriteFile(filepath.Join(base, "auth.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	seedProfile(t, base, "default")
	if err := runProfileMigrate(nil); err == nil {
		t.Fatal("expected error when both legacy and new auth.json exist")
	}
}

// ─── runProfile dispatcher ───────────────────────────────────────────────────

func TestRunProfileDispatchUnknownCommand(t *testing.T) {
	oldArgs := os.Args
	os.Args = []string{"agent-model", "profile", "frobnicate"}
	defer func() { os.Args = oldArgs }()
	if err := runProfile(); err == nil {
		t.Fatal("expected error for unknown profile subcommand")
	}
}

func TestRunProfileDispatchNoSubcommand(t *testing.T) {
	oldArgs := os.Args
	os.Args = []string{"agent-model", "profile"}
	defer func() { os.Args = oldArgs }()
	if err := runProfile(); err == nil {
		t.Fatal("expected usage error when no subcommand is given")
	}
}

func TestRunProfileDispatchList(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	t.Setenv(auth.ProfileEnvVar, "")
	seedProfile(t, base, "work")
	oldArgs := os.Args
	os.Args = []string{"agent-model", "profile", "list"}
	defer func() { os.Args = oldArgs }()

	out, err := captureStdout(t, func() error { return runProfile() })
	if err != nil {
		t.Fatalf("runProfile list: %v", err)
	}
	if !strings.Contains(out, "work") {
		t.Errorf("expected list to include work, got:\n%s", out)
	}
}

func TestRunProfileDispatchSet(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	t.Setenv(auth.ProfileEnvVar, "")
	seedProfile(t, base, "work")
	oldArgs := os.Args
	os.Args = []string{"agent-model", "profile", "set", "work"}
	defer func() { os.Args = oldArgs }()

	if _, err := captureStdout(t, func() error { return runProfile() }); err != nil {
		t.Fatalf("runProfile set: %v", err)
	}
	active, err := auth.ReadActiveProfile()
	if err != nil {
		t.Fatal(err)
	}
	if active != "work" {
		t.Errorf("active = %q, want work", active)
	}
}

// ─── runProfileSet uncovered branches ────────────────────────────────────────

func TestRunProfileSetInvalidName(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	t.Setenv(auth.ProfileEnvVar, "")
	// Uppercase is rejected by ValidateProfileName even with --force.
	if err := runProfileSet([]string{"Work", "--force"}); err == nil {
		t.Fatal("expected invalid-name error for uppercase profile")
	}
}

func TestRunProfileSetParseError(t *testing.T) {
	if err := runProfileSet([]string{"--bogus"}); err == nil {
		t.Fatal("expected unknown-flag parse error")
	}
}

// ─── runProfileRemove uncovered branches ─────────────────────────────────────

func TestRunProfileRemoveActiveWithoutForce(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	t.Setenv(auth.ProfileEnvVar, "")
	seedProfile(t, base, "work")
	if err := auth.WriteActiveProfile("work"); err != nil {
		t.Fatal(err)
	}
	if err := runProfileRemove([]string{"work"}); err == nil {
		t.Fatal("expected error removing active profile without --force")
	}
	// Profile dir must still exist (removal refused).
	if _, err := os.Stat(filepath.Join(base, "work")); err != nil {
		t.Errorf("work profile should still exist after refused removal: %v", err)
	}
}

func TestRunProfileRemoveMissingNotOK(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	t.Setenv(auth.ProfileEnvVar, "")
	if err := runProfileRemove([]string{"ghost"}); err == nil {
		t.Fatal("expected 'not found' error for missing profile without --missing-ok")
	}
}

func TestRunProfileRemoveMissingOKNoop(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	t.Setenv(auth.ProfileEnvVar, "")
	if err := runProfileRemove([]string{"ghost", "--missing-ok"}); err != nil {
		t.Fatalf("expected no error with --missing-ok on absent profile, got %v", err)
	}
}

func TestRunProfileRemoveMissingOKActiveClearsSidecar(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	t.Setenv(auth.ProfileEnvVar, "")
	// Mark "work" active via sidecar but never create its dir, then remove
	// with --missing-ok + --force: the active sidecar must be cleared.
	if err := auth.WriteActiveProfile("work"); err != nil {
		t.Fatal(err)
	}
	if err := runProfileRemove([]string{"work", "--missing-ok", "--force"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	active, err := auth.ReadActiveProfile()
	if err != nil {
		t.Fatal(err)
	}
	if active != "" {
		t.Errorf("active = %q, want cleared", active)
	}
}

func TestRunProfileRemoveLegacyDefaultRemovesOnlyLegacyFile(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	t.Setenv(auth.ProfileEnvVar, "")
	// Legacy default: <base>/auth.json present, plus a sibling profile dir.
	// Removing "default" must delete only the legacy file, not the sibling.
	if err := os.WriteFile(filepath.Join(base, "auth.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	seedProfile(t, base, "work")

	if err := runProfileRemove([]string{"default", "--force"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "auth.json")); !os.IsNotExist(err) {
		t.Errorf("legacy auth.json should be removed, stat err: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "work", "auth.json")); err != nil {
		t.Errorf("sibling profile must survive legacy-default removal: %v", err)
	}
}

func TestRunProfileRemoveInvalidName(t *testing.T) {
	if err := runProfileRemove([]string{"BadName"}); err == nil {
		t.Fatal("expected invalid-name error")
	}
}

func TestRunProfileRemoveParseError(t *testing.T) {
	if err := runProfileRemove([]string{"--bogus"}); err == nil {
		t.Fatal("expected unknown-flag parse error")
	}
}

// ─── parser helpers ──────────────────────────────────────────────────────────

func TestParseProfileSetArgs(t *testing.T) {
	t.Run("name only", func(t *testing.T) {
		name, force, err := parseProfileSetArgs([]string{"work"})
		if err != nil || name != "work" || force {
			t.Fatalf("got (%q,%v,%v), want (work,false,nil)", name, force, err)
		}
	})
	t.Run("name and force", func(t *testing.T) {
		name, force, err := parseProfileSetArgs([]string{"work", "--force"})
		if err != nil || name != "work" || !force {
			t.Fatalf("got (%q,%v,%v), want (work,true,nil)", name, force, err)
		}
	})
	t.Run("unknown flag", func(t *testing.T) {
		if _, _, err := parseProfileSetArgs([]string{"work", "--nope"}); err == nil {
			t.Fatal("expected unknown-flag error")
		}
	})
	t.Run("duplicate name", func(t *testing.T) {
		if _, _, err := parseProfileSetArgs([]string{"a", "b"}); err == nil {
			t.Fatal("expected duplicate-name error")
		}
	})
	t.Run("missing name", func(t *testing.T) {
		if _, _, err := parseProfileSetArgs([]string{"--force"}); err == nil {
			t.Fatal("expected missing-name error")
		}
	})
	t.Run("empty", func(t *testing.T) {
		if _, _, err := parseProfileSetArgs(nil); err == nil {
			t.Fatal("expected missing-name error on empty args")
		}
	})
}

func TestParseProfileRemoveArgs(t *testing.T) {
	t.Run("all flags", func(t *testing.T) {
		name, force, missingOK, err := parseProfileRemoveArgs([]string{"work", "--force", "--missing-ok"})
		if err != nil || name != "work" || !force || !missingOK {
			t.Fatalf("got (%q,%v,%v,%v), want (work,true,true,nil)", name, force, missingOK, err)
		}
	})
	t.Run("unknown flag", func(t *testing.T) {
		if _, _, _, err := parseProfileRemoveArgs([]string{"work", "--nope"}); err == nil {
			t.Fatal("expected unknown-flag error")
		}
	})
	t.Run("duplicate name", func(t *testing.T) {
		if _, _, _, err := parseProfileRemoveArgs([]string{"a", "b"}); err == nil {
			t.Fatal("expected duplicate-name error")
		}
	})
	t.Run("missing name", func(t *testing.T) {
		if _, _, _, err := parseProfileRemoveArgs([]string{"--force"}); err == nil {
			t.Fatal("expected missing-name error")
		}
	})
}
