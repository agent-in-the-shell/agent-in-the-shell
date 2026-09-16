package herospath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDataFile(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/xdg")
	if got := DataFile("x.db"); got != filepath.Join("/xdg", "heros", "x.db") {
		t.Fatalf("xdg path = %q", got)
	}
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "/home/u")
	want := filepath.Join("/home/u", ".local", "share", "heros", "x.db")
	if got := DataFile("x.db"); got != want {
		t.Fatalf("home fallback = %q, want %q", got, want)
	}
}

func TestDataPathAndConfigPath(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/u")

	if got, want := DataDir("agentmodel"), filepath.Join("/home/u", ".local", "share", "heros", "agentmodel"); got != want {
		t.Fatalf("DataDir = %q, want %q", got, want)
	}
	if got, want := DataPath("agentmodel", "agentmodel.db"), filepath.Join("/home/u", ".local", "share", "heros", "agentmodel", "agentmodel.db"); got != want {
		t.Fatalf("DataPath = %q, want %q", got, want)
	}
	if got, want := ConfigPath("agentmodel", "config.toml"), filepath.Join("/home/u", ".config", "heros", "agentmodel", "config.toml"); got != want {
		t.Fatalf("ConfigPath = %q, want %q", got, want)
	}
	// XDG wins for both trees, independently.
	t.Setenv("XDG_DATA_HOME", "/data")
	t.Setenv("XDG_CONFIG_HOME", "/cfg")
	if got, want := DataPath("s", "f"), filepath.Join("/data", "heros", "s", "f"); got != want {
		t.Fatalf("XDG_DATA_HOME DataPath = %q, want %q", got, want)
	}
	if got, want := ConfigPath("s", "f"), filepath.Join("/cfg", "heros", "s", "f"); got != want {
		t.Fatalf("XDG_CONFIG_HOME ConfigPath = %q, want %q", got, want)
	}
}

func TestResolveDataAndConfig_EnvOverrideWins(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/data")
	t.Setenv("AGENT_MODEL_DB", "")
	if got, want := ResolveData("AGENT_MODEL_DB", "agentmodel", "agentmodel.db"), filepath.Join("/data", "heros", "agentmodel", "agentmodel.db"); got != want {
		t.Fatalf("no override: %q, want %q", got, want)
	}
	t.Setenv("AGENT_MODEL_DB", "/custom/task.db")
	if got := ResolveData("AGENT_MODEL_DB", "agentmodel", "agentmodel.db"); got != "/custom/task.db" {
		t.Fatalf("override should win, got %q", got)
	}
	// empty envVar arg → no override lookup.
	if got, want := ResolveConfig("", "s", "c.yaml"), ConfigPath("s", "c.yaml"); got != want {
		t.Fatalf("empty envVar: %q, want %q", got, want)
	}
}

func TestResolveBin_PrefersDeployDir(t *testing.T) {
	t.Setenv("HEROS_BIN_DIR", "")
	if got := ResolveBin("agent-model"); got != "agent-model" {
		t.Fatalf("unset HEROS_BIN_DIR should return bare name, got %q", got)
	}
	dir := t.TempDir()
	t.Setenv("HEROS_BIN_DIR", dir)
	// no such executable yet → bare name.
	if got := ResolveBin("agent-model"); got != "agent-model" {
		t.Fatalf("no executable present, got %q", got)
	}
	// a real executable → its deploy path wins.
	exe := filepath.Join(dir, "agent-model")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ResolveBin("agent-model"); got != exe {
		t.Fatalf("deploy binary should win, got %q want %q", got, exe)
	}
	if BinDir() != dir {
		t.Fatalf("BinDir = %q, want %q", BinDir(), dir)
	}
	// non-executable file must not be picked.
	if err := os.Chmod(exe, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ResolveBin("agent-model"); got != "agent-model" {
		t.Fatalf("non-exec candidate, got %q", got)
	}
}

func TestEnsureParent(t *testing.T) {
	f := filepath.Join(t.TempDir(), "a", "b", "x.db")
	got, err := EnsureParent(f)
	if err != nil || got != f {
		t.Fatalf("EnsureParent = %q, %v", got, err)
	}
	if fi, err := os.Stat(filepath.Dir(f)); err != nil || !fi.IsDir() {
		t.Fatalf("parent not created: %v", err)
	}
}

func TestResolveDataLegacy(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmp) // canonical = tmp/heros/agentmodel/agentmodel.db
	t.Setenv("AGENT_MODEL_DB", "")
	canonical := DataPath("agentmodel", "agentmodel.db")
	legacy := filepath.Join(tmp, ".agentmodel", "agentmodel.db")

	// Fresh install: neither exists → canonical (new layout).
	if got := ResolveDataLegacy("AGENT_MODEL_DB", "agentmodel", "agentmodel.db", legacy); got != canonical {
		t.Fatalf("fresh: got %q, want canonical %q", got, canonical)
	}
	// Existing install: only legacy exists → use it IN PLACE (no orphaning).
	if _, err := EnsureParent(legacy); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(legacy, []byte("data"), 0o600)
	if got := ResolveDataLegacy("AGENT_MODEL_DB", "agentmodel", "agentmodel.db", legacy); got != legacy {
		t.Fatalf("existing legacy: got %q, want legacy %q (must not orphan)", got, legacy)
	}
	// Already migrated: canonical exists → prefer it over legacy.
	if _, err := EnsureParent(canonical); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(canonical, []byte("data"), 0o600)
	if got := ResolveDataLegacy("AGENT_MODEL_DB", "agentmodel", "agentmodel.db", legacy); got != canonical {
		t.Fatalf("migrated: got %q, want canonical %q", got, canonical)
	}
	// Operator override always wins.
	t.Setenv("AGENT_MODEL_DB", "/custom/d.db")
	if got := ResolveDataLegacy("AGENT_MODEL_DB", "agentmodel", "agentmodel.db", legacy); got != "/custom/d.db" {
		t.Fatalf("override: got %q", got)
	}
	// A directory at the legacy path is NOT treated as the file.
	t.Setenv("AGENT_MODEL_DB", "")
	os.Remove(canonical)
	dirLegacy := filepath.Join(tmp, "dirlegacy")
	os.MkdirAll(dirLegacy, 0o755)
	if got := ResolveDataLegacy("AGENT_MODEL_DB", "agentmodel", "agentmodel.db", dirLegacy); got != DataPath("agentmodel", "agentmodel.db") {
		t.Fatalf("dir legacy must be skipped, got %q", got)
	}
}

func TestResolveDataDirLegacy(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmp) // canonical = tmp/heros/agentchat/media
	t.Setenv("AGENT_CHAT_MEDIA_DIR", "")
	svc := filepath.Join("agentchat", "media") // a nested data dir (agentchat's media subdir)
	canonical := DataDir(svc)
	legacy := filepath.Join(tmp, "data", "media")

	// Fresh install: neither dir exists → canonical (new layout).
	if got := ResolveDataDirLegacy("AGENT_CHAT_MEDIA_DIR", svc, legacy); got != canonical {
		t.Fatalf("fresh: got %q, want canonical %q", got, canonical)
	}
	// Existing install: only the legacy dir exists → use it IN PLACE (no orphaning).
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ResolveDataDirLegacy("AGENT_CHAT_MEDIA_DIR", svc, legacy); got != legacy {
		t.Fatalf("existing legacy: got %q, want legacy %q (must not orphan)", got, legacy)
	}
	// Already migrated: the canonical dir exists → prefer it over legacy.
	if err := os.MkdirAll(canonical, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ResolveDataDirLegacy("AGENT_CHAT_MEDIA_DIR", svc, legacy); got != canonical {
		t.Fatalf("migrated: got %q, want canonical %q", got, canonical)
	}
	// Operator override always wins.
	t.Setenv("AGENT_CHAT_MEDIA_DIR", "/custom/media")
	if got := ResolveDataDirLegacy("AGENT_CHAT_MEDIA_DIR", svc, legacy); got != "/custom/media" {
		t.Fatalf("override: got %q", got)
	}
	// A FILE at the legacy path is NOT treated as the directory.
	t.Setenv("AGENT_CHAT_MEDIA_DIR", "")
	os.RemoveAll(canonical)
	fileLegacy := filepath.Join(tmp, "filelegacy")
	if err := os.WriteFile(fileLegacy, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ResolveDataDirLegacy("AGENT_CHAT_MEDIA_DIR", svc, fileLegacy); got != canonical {
		t.Fatalf("file legacy must be skipped, got %q want %q", got, canonical)
	}
}
