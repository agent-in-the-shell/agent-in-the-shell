package auth_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
)

func TestResolveProfileTokenDirPrecedence(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	t.Setenv(auth.ProfileEnvVar, "")

	dir, profile, err := auth.ResolveProfileTokenDir("work", "")
	if err != nil {
		t.Fatal(err)
	}
	if profile != "work" || dir != filepath.Join(base, "work") {
		t.Fatalf("flag profile resolved dir=%q profile=%q", dir, profile)
	}

	t.Setenv(auth.ProfileEnvVar, "personal")
	dir, profile, err = auth.ResolveProfileTokenDir("", "")
	if err != nil {
		t.Fatal(err)
	}
	if profile != "personal" || dir != filepath.Join(base, "personal") {
		t.Fatalf("env profile resolved dir=%q profile=%q", dir, profile)
	}

	dir, profile, err = auth.ResolveProfileTokenDir("work", filepath.Join(base, "legacy"))
	if err != nil {
		t.Fatal(err)
	}
	if profile != "" || dir != filepath.Join(base, "legacy") {
		t.Fatalf("token-dir override resolved dir=%q profile=%q", dir, profile)
	}
}

func TestResolveChatGPTProfileTokenDir(t *testing.T) {
	base := t.TempDir()
	t.Setenv("CHATGPT_TOKEN_DIR", base)
	t.Setenv(auth.ProfileEnvVar, "")

	// Flag wins.
	dir, profile, err := auth.ResolveChatGPTProfileTokenDir("work", "")
	if err != nil {
		t.Fatal(err)
	}
	if profile != "work" || dir != filepath.Join(base, "work") {
		t.Fatalf("flag profile resolved dir=%q profile=%q", dir, profile)
	}

	// AGENTMODEL_PROFILE env is shared across providers.
	t.Setenv(auth.ProfileEnvVar, "personal")
	dir, profile, err = auth.ResolveChatGPTProfileTokenDir("", "")
	if err != nil {
		t.Fatal(err)
	}
	if profile != "personal" || dir != filepath.Join(base, "personal") {
		t.Fatalf("env profile resolved dir=%q profile=%q", dir, profile)
	}

	// --token-dir override skips profile resolution.
	dir, profile, err = auth.ResolveChatGPTProfileTokenDir("work", filepath.Join(base, "legacy"))
	if err != nil {
		t.Fatal(err)
	}
	if profile != "" || dir != filepath.Join(base, "legacy") {
		t.Fatalf("token-dir override resolved dir=%q profile=%q", dir, profile)
	}
}

func TestChatGPTActiveProfileSidecarIsSeparate(t *testing.T) {
	anthropicBase := t.TempDir()
	chatgptBase := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", anthropicBase)
	t.Setenv("CHATGPT_TOKEN_DIR", chatgptBase)
	t.Setenv(auth.ProfileEnvVar, "")

	if err := auth.WriteActiveChatGPTProfile("work"); err != nil {
		t.Fatal(err)
	}

	// ChatGPT sidecar drives ChatGPT resolution.
	dir, profile, err := auth.ResolveChatGPTProfileTokenDir("", "")
	if err != nil {
		t.Fatal(err)
	}
	if profile != "work" || dir != filepath.Join(chatgptBase, "work") {
		t.Fatalf("chatgpt sidecar resolved dir=%q profile=%q", dir, profile)
	}

	// The Anthropic store is untouched — still resolves to default.
	if active, err := auth.ReadActiveProfile(); err != nil || active != "" {
		t.Fatalf("anthropic sidecar leaked active=%q err=%v", active, err)
	}
}

func TestActiveProfileSidecar(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	t.Setenv(auth.ProfileEnvVar, "")

	if active, err := auth.ReadActiveProfile(); err != nil || active != "" {
		t.Fatalf("missing sidecar active=%q err=%v", active, err)
	}
	if err := auth.WriteActiveProfile("work"); err != nil {
		t.Fatal(err)
	}
	active, err := auth.ReadActiveProfile()
	if err != nil {
		t.Fatal(err)
	}
	if active != "work" {
		t.Fatalf("active = %q, want work", active)
	}
	dir, profile, err := auth.ResolveProfileTokenDir("", "")
	if err != nil {
		t.Fatal(err)
	}
	if profile != "work" || dir != filepath.Join(base, "work") {
		t.Fatalf("sidecar resolved dir=%q profile=%q", dir, profile)
	}

	if err := auth.ClearActiveProfile(); err != nil {
		t.Fatal(err)
	}
	active, err = auth.ReadActiveProfile()
	if err != nil {
		t.Fatal(err)
	}
	if active != "" {
		t.Fatalf("active after clear = %q, want empty", active)
	}
	if err := auth.ClearActiveProfile(); err != nil {
		t.Fatalf("second clear should be a no-op: %v", err)
	}
}

func TestListProfilesIncludesLegacyDefault(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	if err := os.WriteFile(filepath.Join(base, "auth.json"), []byte(`{"anthropic":{"type":"oauth"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "work", "auth.json"), []byte(`{"anthropic":{"type":"oauth"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	profiles, err := auth.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 2 || profiles[0] != "default" || profiles[1] != "work" {
		t.Fatalf("profiles = %#v, want default/work", profiles)
	}

	dir, profile, err := auth.ResolveProfileTokenDir("", "")
	if err != nil {
		t.Fatal(err)
	}
	if profile != "default" || dir != base {
		t.Fatalf("legacy default resolved dir=%q profile=%q", dir, profile)
	}
}

func TestMigrateLegacyDefault(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	t.Setenv(auth.ProfileEnvVar, "")

	// No legacy file → no-op, no error, NeedsMigrate false.
	status, err := auth.MigrateLegacyDefault()
	if err != nil {
		t.Fatalf("idempotent no-op: %v", err)
	}
	if status.NeedsMigrate || status.LegacyExists {
		t.Fatalf("no-op status = %+v, want LegacyExists=false NeedsMigrate=false", status)
	}

	// Legacy file present, new target absent → migrated.
	legacyPath := filepath.Join(base, "auth.json")
	payload := []byte(`{"anthropic":{"type":"oauth","access":"a","refresh":"r","expires":0}}`)
	if err := os.WriteFile(legacyPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	status, err = auth.MigrateLegacyDefault()
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if status.LegacyExists || !status.NewExists {
		t.Fatalf("post-migrate status = %+v, want LegacyExists=false NewExists=true", status)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy auth.json still present after migrate: err=%v", err)
	}
	newPath := filepath.Join(base, "default", "auth.json")
	if data, err := os.ReadFile(newPath); err != nil {
		t.Fatalf("new auth.json: %v", err)
	} else if string(data) != string(payload) {
		t.Fatalf("payload not preserved by rename: got %q want %q", data, payload)
	}

	// Resolution is now deterministic: "default" resolves to <base>/default,
	// no longer to <base>.
	dir, profile, err := auth.ResolveProfileTokenDir("", "")
	if err != nil {
		t.Fatal(err)
	}
	if profile != "default" || dir != filepath.Join(base, "default") {
		t.Fatalf("after migrate, default resolved dir=%q profile=%q", dir, profile)
	}

	// Both-files-exist case → refuse rather than silently overwrite.
	if err := os.WriteFile(legacyPath, []byte("conflict"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.MigrateLegacyDefault(); err == nil {
		t.Fatal("expected error when both legacy and new auth.json exist")
	}
}

func TestValidateProfileName(t *testing.T) {
	valid := []string{"default", "work", "personal_1", "a-b"}
	for _, name := range valid {
		if err := auth.ValidateProfileName(name); err != nil {
			t.Fatalf("%q should be valid: %v", name, err)
		}
	}
	invalid := []string{
		"", "../x", "x/y", "profile", "auth.json", "type", "-bad",
		"Work", "WORK", "Default", // uppercase rejected so APFS/NTFS can't alias Work and work
	}
	for _, name := range invalid {
		if err := auth.ValidateProfileName(name); err == nil {
			t.Fatalf("%q should be invalid", name)
		}
	}
}

func TestProfileTokenDir(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)

	dir, err := auth.ProfileTokenDir("work")
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(base, "work") {
		t.Fatalf("ProfileTokenDir = %q", dir)
	}
	if _, err := auth.ProfileTokenDir("Work"); err == nil {
		t.Fatal("expected invalid profile name error")
	}
}

func TestResolveAnthropicProfilePrecedence(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	t.Setenv(auth.ProfileEnvVar, "")

	if got, err := auth.ResolveAnthropicProfile(""); err != nil || got != auth.DefaultProfile {
		t.Fatalf("default profile = %q, err=%v; want %q", got, err, auth.DefaultProfile)
	}

	if err := auth.WriteActiveProfile("sidecar"); err != nil {
		t.Fatal(err)
	}
	if got, err := auth.ResolveAnthropicProfile(""); err != nil || got != "sidecar" {
		t.Fatalf("sidecar profile = %q, err=%v; want sidecar", got, err)
	}

	t.Setenv(auth.ProfileEnvVar, "environment")
	if got, err := auth.ResolveAnthropicProfile(""); err != nil || got != "environment" {
		t.Fatalf("environment profile = %q, err=%v; want environment", got, err)
	}
	if got, err := auth.ResolveAnthropicProfile("flag"); err != nil || got != "flag" {
		t.Fatalf("flag profile = %q, err=%v; want flag", got, err)
	}

	if got, err := auth.ResolveAnthropicProfile("../escape"); err == nil || got != "" {
		t.Fatalf("invalid flag profile = %q, err=%v; want empty profile and error", got, err)
	}
}

func TestActiveProfileSidecarErrors(t *testing.T) {
	t.Run("invalid contents", func(t *testing.T) {
		base := t.TempDir()
		t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
		t.Setenv(auth.ProfileEnvVar, "")
		if err := os.WriteFile(filepath.Join(base, "profile"), []byte("../escape\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		if got, err := auth.ReadActiveProfile(); err == nil || got != "" {
			t.Fatalf("ReadActiveProfile() = %q, %v; want empty profile and validation error", got, err)
		}
		if got, err := auth.ResolveAnthropicProfile(""); err == nil || got != "" {
			t.Fatalf("ResolveAnthropicProfile() = %q, %v; want sidecar validation error", got, err)
		}
	})

	t.Run("read failure", func(t *testing.T) {
		base := t.TempDir()
		t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
		if err := os.Mkdir(filepath.Join(base, "profile"), 0o700); err != nil {
			t.Fatal(err)
		}

		got, err := auth.ReadActiveProfile()
		if err == nil || got != "" || !strings.Contains(err.Error(), "read active profile") {
			t.Fatalf("ReadActiveProfile() = %q, %v; want wrapped read error", got, err)
		}
	})

	t.Run("invalid write", func(t *testing.T) {
		t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", t.TempDir())
		if err := auth.WriteActiveProfile("UPPERCASE"); err == nil {
			t.Fatal("WriteActiveProfile(UPPERCASE) = nil; want validation error")
		}
	})

	t.Run("mkdir failure", func(t *testing.T) {
		base := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(base, []byte("occupied"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)

		err := auth.WriteActiveProfile("work")
		if err == nil || !strings.Contains(err.Error(), "mkdir profile base") {
			t.Fatalf("WriteActiveProfile() error = %v; want wrapped mkdir error", err)
		}
	})

	t.Run("clear failure", func(t *testing.T) {
		base := t.TempDir()
		t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
		sidecarDir := filepath.Join(base, "profile")
		if err := os.Mkdir(sidecarDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sidecarDir, "keep"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}

		err := auth.ClearActiveProfile()
		if err == nil || !strings.Contains(err.Error(), "clear active profile") {
			t.Fatalf("ClearActiveProfile() error = %v; want wrapped remove error", err)
		}
		if _, err := os.Stat(sidecarDir); err != nil {
			t.Fatalf("failed clear removed sidecar directory: %v", err)
		}
	})
}

func TestListProfilesMissingAndUnreadableBase(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		base := filepath.Join(t.TempDir(), "missing")
		t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)

		profiles, err := auth.ListProfiles()
		if err != nil || len(profiles) != 0 {
			t.Fatalf("ListProfiles() = %#v, %v; want empty result", profiles, err)
		}
	})

	t.Run("base is a file", func(t *testing.T) {
		base := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(base, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)

		profiles, err := auth.ListProfiles()
		if err == nil || profiles != nil || !strings.Contains(err.Error(), "list profiles") {
			t.Fatalf("ListProfiles() = %#v, %v; want wrapped read-dir error", profiles, err)
		}
	})
}

func TestCheckLegacyDefaultReportsStatErrors(t *testing.T) {
	t.Run("legacy path", func(t *testing.T) {
		base := t.TempDir()
		t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
		if err := os.Symlink("auth.json", filepath.Join(base, "auth.json")); err != nil {
			t.Fatal(err)
		}

		status, err := auth.CheckLegacyDefault()
		if err == nil || !strings.Contains(err.Error(), status.LegacyPath) {
			t.Fatalf("CheckLegacyDefault() status=%+v err=%v; want legacy stat error", status, err)
		}
	})

	t.Run("new path", func(t *testing.T) {
		base := t.TempDir()
		t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
		defaultDir := filepath.Join(base, auth.DefaultProfile)
		if err := os.Mkdir(defaultDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("auth.json", filepath.Join(defaultDir, "auth.json")); err != nil {
			t.Fatal(err)
		}

		status, err := auth.CheckLegacyDefault()
		if err == nil || !strings.Contains(err.Error(), status.NewPath) {
			t.Fatalf("CheckLegacyDefault() status=%+v err=%v; want new-path stat error", status, err)
		}
	})
}

func TestMigrateLegacyDefaultReportsMkdirFailure(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	legacyPath := filepath.Join(base, "auth.json")
	if err := os.WriteFile(legacyPath, []byte(`{"access_token":"token"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing-target", filepath.Join(base, auth.DefaultProfile)); err != nil {
		t.Fatal(err)
	}

	status, err := auth.MigrateLegacyDefault()
	if err == nil || !strings.Contains(err.Error(), "mkdir default dir") {
		t.Fatalf("MigrateLegacyDefault() status=%+v err=%v; want mkdir error", status, err)
	}
	if data, readErr := os.ReadFile(legacyPath); readErr != nil || string(data) != `{"access_token":"token"}` {
		t.Fatalf("legacy credentials changed after failed migration: data=%q err=%v", data, readErr)
	}
}
