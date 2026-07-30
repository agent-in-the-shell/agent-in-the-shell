package agentmodel_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
)

func writeYAML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestDefaultConfigPath(t *testing.T) {
	t.Setenv("AGENT_MODEL_CONFIG", "/custom/agentmodel.yaml")
	if got := agentmodel.DefaultConfigPath(); got != "/custom/agentmodel.yaml" {
		t.Fatalf("DefaultConfigPath with env = %q", got)
	}

	t.Setenv("AGENT_MODEL_CONFIG", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	want := filepath.Join(home, ".config", "agentmodel", "config.yaml")
	if got := agentmodel.DefaultConfigPath(); got != want {
		t.Fatalf("DefaultConfigPath = %q, want %q", got, want)
	}
}

func TestLoadConfig_EmptyPathUsesDefault(t *testing.T) {
	// An empty path resolves to AGENT_MODEL_CONFIG; point it at a written file.
	path := writeYAML(t, `
model_list:
  - model_name: "gpt-4"
    deployments:
      - provider: "openai"
        model: "gpt-4o"
        auth_mode: "api_key"
        api_key_env: "OPENAI_API_KEY"
        weight: 100
`)
	t.Setenv("AGENT_MODEL_CONFIG", path)
	c, err := agentmodel.LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig(\"\"): %v", err)
	}
	if len(c.ModelList) != 1 {
		t.Fatalf("ModelList: got %d, want 1", len(c.ModelList))
	}
}

func TestLoadConfig_ValidFullExample(t *testing.T) {
	path := writeYAML(t, `
listen: ":9000"
db: "agentmodel.db"
auth:
  bearer_token_env: "AGENT_MODEL_TOKEN"
model_list:
  - model_name: "gpt-4"
    deployments:
      - provider: "openai"
        model: "gpt-4o"
        auth_mode: "api_key"
        api_key_env: "OPENAI_API_KEY"
        weight: 70
      - provider: "anthropic"
        model: "claude-3-5-sonnet-latest"
        auth_mode: "api_key"
        api_key_env: "ANTHROPIC_API_KEY"
        weight: 30
  - model_name: "chatgpt-pro"
    deployments:
      - provider: "chatgpt"
        model: "gpt-5"
        auth_mode: "subscription"
        oauth_token_dir: "${HOME}/.config/agentmodel/chatgpt"
        weight: 100
fallbacks:
  - model_name: "gpt-4"
    fallback_to: ["chatgpt-pro"]
`)
	c, err := agentmodel.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if c.Listen != ":9000" {
		t.Errorf("Listen: got %q, want :9000", c.Listen)
	}
	if len(c.ModelList) != 2 {
		t.Errorf("ModelList: got %d, want 2", len(c.ModelList))
	}
	if got := c.ModelList[0].Deployments[0].EffectiveWeight(); got != 70 {
		t.Errorf("first weight: got %d, want 70", got)
	}
	if len(c.Fallbacks) != 1 {
		t.Errorf("Fallbacks: got %d, want 1", len(c.Fallbacks))
	}
}

func TestLoadConfig_BaseURLKeylessOK(t *testing.T) {
	// A deployment with base_url and no api_key_env loads OK (a keyless local
	// backend or a test fake upstream); the factory builds it with an empty key.
	path := writeYAML(t, `
model_list:
  - model_name: "local"
    deployments:
      - provider: "openai"
        model: "llama3"
        auth_mode: "api_key"
        base_url: "http://127.0.0.1:11434/v1"
        weight: 100
`)
	c, err := agentmodel.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := c.ModelList[0].Deployments[0].BaseURL; got != "http://127.0.0.1:11434/v1" {
		t.Errorf("BaseURL: got %q", got)
	}
}

func TestLoadConfig_BaseURLValidationErrors(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
	}{
		{"not a url", "not a url"},
		{"relative", "/v1/chat"},
		{"no scheme", "127.0.0.1:8080"},
		{"ftp scheme", "ftp://example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeYAML(t, `
model_list:
  - model_name: "m"
    deployments:
      - provider: "openai"
        model: "gpt-4o"
        base_url: "`+tc.baseURL+`"
        weight: 1
`)
			_, err := agentmodel.LoadConfig(path)
			if err == nil {
				t.Fatalf("expected error for base_url %q", tc.baseURL)
			}
			if !strings.Contains(err.Error(), "base_url") {
				t.Errorf("error should mention base_url, got: %v", err)
			}
		})
	}
}

func TestLoadConfig_AzureValid(t *testing.T) {
	path := writeYAML(t, `
model_list:
  - model_name: "gpt-4o"
    deployments:
      - provider: "azure"
        model: "gpt-4o"
        api_key_env: "AZURE_OPENAI_KEY"
        base_url: "https://res.openai.azure.com"
        api_version: "2024-10-01-preview"
        deployment_name: "gpt4o-deploy"
        weight: 100
`)
	c, err := agentmodel.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	d := c.ModelList[0].Deployments[0]
	if d.APIVersion != "2024-10-01-preview" || d.DeploymentName != "gpt4o-deploy" {
		t.Errorf("azure fields: api_version=%q deployment_name=%q", d.APIVersion, d.DeploymentName)
	}
}

func TestLoadConfig_AzureValidationErrors(t *testing.T) {
	cases := []struct {
		name, deployment, wantSubstr string
	}{
		{
			name: "missing api_version",
			deployment: `
      - provider: "azure"
        model: "gpt-4o"
        api_key_env: "AZURE_OPENAI_KEY"
        base_url: "https://res.openai.azure.com"
        weight: 1`,
			wantSubstr: "api_version",
		},
		{
			name: "missing base_url",
			deployment: `
      - provider: "azure"
        model: "gpt-4o"
        api_key_env: "AZURE_OPENAI_KEY"
        api_version: "2024-10-01-preview"
        weight: 1`,
			wantSubstr: "base_url",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeYAML(t, "model_list:\n  - model_name: \"m\"\n    deployments:"+tc.deployment+"\n")
			_, err := agentmodel.LoadConfig(path)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error should mention %q, got: %v", tc.wantSubstr, err)
			}
		})
	}
}

func TestLoadConfig_AppliesDefaults(t *testing.T) {
	path := writeYAML(t, `
model_list:
  - model_name: "gpt-4"
    deployments:
      - provider: "openai"
        model: "gpt-4o"
        auth_mode: "api_key"
        api_key_env: "OPENAI_API_KEY"
        weight: 1
`)
	c, err := agentmodel.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if c.Listen != ":8080" {
		t.Errorf("Listen default: got %q, want :8080", c.Listen)
	}
	if c.Auth.BearerTokenEnv != "AGENT_MODEL_TOKEN" {
		t.Errorf("BearerTokenEnv default: got %q, want AGENT_MODEL_TOKEN", c.Auth.BearerTokenEnv)
	}
}

func TestLoadConfig_ValidationErrors(t *testing.T) {
	cases := []struct {
		desc    string
		yaml    string
		wantSub string
	}{
		{
			desc:    "empty model_list",
			yaml:    `model_list: []`,
			wantSub: "model_list must be non-empty",
		},
		{
			desc: "missing model_name",
			yaml: `
model_list:
  - deployments:
      - provider: openai
        model: x
        auth_mode: api_key
        api_key_env: OPENAI_API_KEY
        weight: 1
`,
			wantSub: "model_name is required",
		},
		{
			desc: "duplicate model_name",
			yaml: `
model_list:
  - model_name: gpt-4
    deployments:
      - provider: openai
        model: x
        auth_mode: api_key
        api_key_env: OPENAI_API_KEY
        weight: 1
  - model_name: gpt-4
    deployments:
      - provider: anthropic
        model: y
        auth_mode: api_key
        api_key_env: ANTHROPIC_API_KEY
        weight: 1
`,
			wantSub: "duplicate model_name",
		},
		{
			desc: "no deployments",
			yaml: `
model_list:
  - model_name: gpt-4
    deployments: []
`,
			wantSub: "deployments must be non-empty",
		},
		{
			desc: "zero total weight",
			yaml: `
model_list:
  - model_name: gpt-4
    deployments:
      - provider: openai
        model: gpt-4o
        auth_mode: api_key
        api_key_env: OPENAI_API_KEY
        weight: 0
`,
			wantSub: "total weight is 0",
		},
		{
			desc: "missing api_key_env for api_key mode",
			yaml: `
model_list:
  - model_name: gpt-4
    deployments:
      - provider: openai
        model: gpt-4o
        auth_mode: api_key
        weight: 1
`,
			wantSub: "api_key_env or api_key_envs required for api_key",
		},
		{
			desc: "subscription on non-supporting provider",
			yaml: `
model_list:
  - model_name: gemini
    deployments:
      - provider: gemini
        model: gemini-2.5-pro
        auth_mode: subscription
        api_key_env: GEMINI_OAUTH
        weight: 1
`,
			wantSub: "does not support subscription",
		},
		{
			desc: "subscription without env or dir",
			yaml: `
model_list:
  - model_name: chatgpt-pro
    deployments:
      - provider: chatgpt
        model: gpt-5
        auth_mode: subscription
        weight: 1
`,
			wantSub: "requires api_key_env",
		},
		{
			desc: "unknown auth_mode",
			yaml: `
model_list:
  - model_name: x
    deployments:
      - provider: openai
        model: m
        auth_mode: oauth_pkce
        api_key_env: X
        weight: 1
`,
			wantSub: "unknown auth_mode",
		},
		{
			desc: "fallback to unknown model",
			yaml: `
model_list:
  - model_name: gpt-4
    deployments:
      - provider: openai
        model: gpt-4o
        auth_mode: api_key
        api_key_env: X
        weight: 1
fallbacks:
  - model_name: gpt-4
    fallback_to: [ghost]
`,
			wantSub: "unknown model_name",
		},
		{
			desc: "oauth_token_dir and oauth_token_dirs both set",
			yaml: `
model_list:
  - model_name: chatgpt-pro
    deployments:
      - provider: chatgpt
        model: gpt-5
        auth_mode: subscription
        oauth_token_dir: /a
        oauth_token_dirs: [/b, /c]
        weight: 1
`,
			wantSub: "mutually exclusive",
		},
		{
			desc: "oauth_token_dirs on a provider without subscription support",
			yaml: `
model_list:
  - model_name: gem
    deployments:
      - provider: gemini
        model: gemini-pro
        auth_mode: subscription
        oauth_token_dirs: [/a, /b]
        weight: 1
`,
			wantSub: "does not support subscription auth_mode",
		},
		{
			desc: "chatgpt subscription with only api_key_env (silently ignored)",
			yaml: `
model_list:
  - model_name: gpt-5
    deployments:
      - provider: chatgpt
        model: gpt-5.5
        auth_mode: subscription
        api_key_env: SOME_KEY
        weight: 1
`,
			wantSub: "chatgpt subscription requires oauth_token_dir",
		},
		{
			desc: "oauth_token_dirs with empty entry",
			yaml: `
model_list:
  - model_name: chatgpt-pro
    deployments:
      - provider: chatgpt
        model: gpt-5
        auth_mode: subscription
        oauth_token_dirs: ["/a", ""]
        weight: 1
`,
			wantSub: "must be non-empty",
		},
		{
			desc: "oauth_token_dirs with duplicate entry",
			yaml: `
model_list:
  - model_name: chatgpt-pro
    deployments:
      - provider: chatgpt
        model: gpt-5
        auth_mode: subscription
        oauth_token_dirs: ["/a", "/a"]
        weight: 1
`,
			wantSub: "duplicate directory",
		},
	}

	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			path := writeYAML(t, c.yaml)
			_, err := agentmodel.LoadConfig(path)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.wantSub)
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("error: got %q, want substring %q", err.Error(), c.wantSub)
			}
		})
	}
}

func TestLoadConfig_ChatGPTOAuthTokenDirsPool(t *testing.T) {
	path := writeYAML(t, `
model_list:
  - model_name: "chatgpt-pro"
    deployments:
      - provider: "chatgpt"
        model: "gpt-5"
        auth_mode: "subscription"
        oauth_token_dirs:
          - "/a"
          - "/b"
        weight: 100
`)
	c, err := agentmodel.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	dirs := c.ModelList[0].Deployments[0].OAuthTokenDirs
	if len(dirs) != 2 || dirs[0] != "/a" || dirs[1] != "/b" {
		t.Errorf("OAuthTokenDirs: got %v, want [/a /b]", dirs)
	}
}

func TestLoadConfig_AnthropicOAuthTokenDirsPool(t *testing.T) {
	// Multi-account Anthropic subscriptions pool through refreshable token
	// directories, the same knob chatgpt uses (#58).
	path := writeYAML(t, `
model_list:
  - model_name: "claude-sonnet-4-5"
    deployments:
      - provider: "anthropic-oauth"
        model: "claude-sonnet-4-5"
        auth_mode: "subscription"
        oauth_token_dirs:
          - "/accounts/work"
          - "/accounts/personal"
        weight: 100
`)
	c, err := agentmodel.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	dirs := c.ModelList[0].Deployments[0].OAuthTokenDirs
	if len(dirs) != 2 || dirs[0] != "/accounts/work" || dirs[1] != "/accounts/personal" {
		t.Errorf("OAuthTokenDirs: got %v, want [/accounts/work /accounts/personal]", dirs)
	}
}

func TestLoadConfig_RPMTPMFields(t *testing.T) {
	path := writeYAML(t, `
model_list:
  - model_name: "gpt-4"
    deployments:
      - provider: "openai"
        model: "gpt-4o"
        auth_mode: "api_key"
        api_key_env: "OPENAI_API_KEY"
        weight: 1
        rpm: 500
        tpm: 200000
`)
	c, err := agentmodel.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	d := c.ModelList[0].Deployments[0]
	if d.RPM == nil || *d.RPM != 500 {
		t.Errorf("RPM: got %v, want 500", d.RPM)
	}
	if d.TPM == nil || *d.TPM != 200000 {
		t.Errorf("TPM: got %v, want 200000", d.TPM)
	}
}

func TestLoadConfig_RPMTPMNilWhenOmitted(t *testing.T) {
	path := writeYAML(t, `
model_list:
  - model_name: "gpt-4"
    deployments:
      - provider: "openai"
        model: "gpt-4o"
        auth_mode: "api_key"
        api_key_env: "OPENAI_API_KEY"
        weight: 1
`)
	c, err := agentmodel.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	d := c.ModelList[0].Deployments[0]
	if d.RPM != nil {
		t.Errorf("RPM: want nil when omitted, got %d", *d.RPM)
	}
	if d.TPM != nil {
		t.Errorf("TPM: want nil when omitted, got %d", *d.TPM)
	}
}

func TestLoadConfig_RPMTPMValidationErrors(t *testing.T) {
	base := func(extra string) string {
		return `
model_list:
  - model_name: "gpt-4"
    deployments:
      - provider: "openai"
        model: "gpt-4o"
        auth_mode: "api_key"
        api_key_env: "OPENAI_API_KEY"
        weight: 1
` + extra
	}
	cases := []struct {
		desc    string
		yaml    string
		wantSub string
	}{
		{
			desc:    "rpm zero",
			yaml:    base("        rpm: 0\n"),
			wantSub: "rpm must be > 0",
		},
		{
			desc:    "rpm negative",
			yaml:    base("        rpm: -1\n"),
			wantSub: "rpm must be > 0",
		},
		{
			desc:    "tpm zero",
			yaml:    base("        tpm: 0\n"),
			wantSub: "tpm must be > 0",
		},
		{
			desc:    "tpm negative",
			yaml:    base("        tpm: -10\n"),
			wantSub: "tpm must be > 0",
		},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			path := writeYAML(t, c.yaml)
			_, err := agentmodel.LoadConfig(path)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.wantSub)
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("error: got %q, want substring %q", err.Error(), c.wantSub)
			}
		})
	}
}

func TestLoadConfig_FileNotFound(t *testing.T) {
	_, err := agentmodel.LoadConfig("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestRetentionConfig_ParseAndDefaults(t *testing.T) {
	// Disabled by default: empty period => keep forever.
	var rc agentmodel.RetentionConfig
	if _, enabled, err := rc.PurgePeriod(); err != nil || enabled {
		t.Errorf("empty period: enabled=%v err=%v, want disabled", enabled, err)
	}
	// Interval defaults to 1h.
	if iv, err := rc.PurgeInterval(); err != nil || iv != time.Hour {
		t.Errorf("default interval: got %v err=%v, want 1h", iv, err)
	}
	// Valid durations parse.
	rc = agentmodel.RetentionConfig{Period: "720h", Interval: "30m"}
	d, enabled, err := rc.PurgePeriod()
	if err != nil || !enabled || d != 720*time.Hour {
		t.Errorf("period: got %v enabled=%v err=%v, want 720h enabled", d, enabled, err)
	}
	if iv, err := rc.PurgeInterval(); err != nil || iv != 30*time.Minute {
		t.Errorf("interval: got %v err=%v, want 30m", iv, err)
	}
	// Non-positive is rejected.
	if _, _, err := (agentmodel.RetentionConfig{Period: "0h"}).PurgePeriod(); err == nil {
		t.Error("zero period should be rejected")
	}
}

func TestLoadConfig_RejectsBadRetention(t *testing.T) {
	path := writeYAML(t, `
model_list:
  - model_name: "gpt-4"
    deployments:
      - provider: "openai"
        model: "gpt-4o"
        auth_mode: "api_key"
        api_key_env: "OPENAI_API_KEY"
        weight: 1
retention:
  period: "not-a-duration"
`)
	if _, err := agentmodel.LoadConfig(path); err == nil {
		t.Fatal("expected error for malformed retention.period, got nil")
	}
}

func TestRouterCooldownDuration_ParseAndDefaults(t *testing.T) {
	// Empty => not configured (router applies its own default).
	if d, ok, err := (&agentmodel.Config{}).RouterCooldownDuration(); err != nil || ok || d != 0 {
		t.Errorf("empty: got (%v, ok=%v, err=%v), want (0, false, nil)", d, ok, err)
	}
	// Valid duration parses and is reported as configured.
	if d, ok, err := (&agentmodel.Config{RouterCooldown: "90s"}).RouterCooldownDuration(); err != nil || !ok || d != 90*time.Second {
		t.Errorf("90s: got (%v, ok=%v, err=%v), want (90s, true, nil)", d, ok, err)
	}
	// "0" is valid and explicitly disables cooling (configured, zero).
	if d, ok, err := (&agentmodel.Config{RouterCooldown: "0"}).RouterCooldownDuration(); err != nil || !ok || d != 0 {
		t.Errorf("0: got (%v, ok=%v, err=%v), want (0, true, nil)", d, ok, err)
	}
	// Negative is rejected.
	if _, _, err := (&agentmodel.Config{RouterCooldown: "-5m"}).RouterCooldownDuration(); err == nil {
		t.Error("negative router_cooldown should be rejected")
	}
	// Malformed is rejected.
	if _, _, err := (&agentmodel.Config{RouterCooldown: "soon"}).RouterCooldownDuration(); err == nil {
		t.Error("malformed router_cooldown should be rejected")
	}
}

func TestRevalidateIntervalDuration_ParseAndDefaults(t *testing.T) {
	// Empty => disabled (one-shot startup check only).
	if d, ok, err := (&agentmodel.Config{}).RevalidateIntervalDuration(); err != nil || ok || d != 0 {
		t.Errorf("empty: got (%v, ok=%v, err=%v), want (0, false, nil)", d, ok, err)
	}
	// Valid positive duration parses and is reported as enabled.
	if d, ok, err := (&agentmodel.Config{RevalidateInterval: "1h"}).RevalidateIntervalDuration(); err != nil || !ok || d != time.Hour {
		t.Errorf("1h: got (%v, ok=%v, err=%v), want (1h, true, nil)", d, ok, err)
	}
	// "0" is rejected: omit the field to disable rather than setting zero.
	if _, _, err := (&agentmodel.Config{RevalidateInterval: "0"}).RevalidateIntervalDuration(); err == nil {
		t.Error("zero revalidate_interval should be rejected (omit to disable)")
	}
	// Negative is rejected.
	if _, _, err := (&agentmodel.Config{RevalidateInterval: "-1h"}).RevalidateIntervalDuration(); err == nil {
		t.Error("negative revalidate_interval should be rejected")
	}
	// Malformed is rejected.
	if _, _, err := (&agentmodel.Config{RevalidateInterval: "later"}).RevalidateIntervalDuration(); err == nil {
		t.Error("malformed revalidate_interval should be rejected")
	}
}

func TestLoadConfig_RejectsBadRouterCooldown(t *testing.T) {
	path := writeYAML(t, `
model_list:
  - model_name: "gpt-4"
    deployments:
      - provider: "openai"
        model: "gpt-4o"
        auth_mode: "api_key"
        api_key_env: "OPENAI_API_KEY"
        weight: 1
router_cooldown: "not-a-duration"
`)
	if _, err := agentmodel.LoadConfig(path); err == nil {
		t.Fatal("expected error for malformed router_cooldown, got nil")
	}
}

func TestContentLogConfig_Durations(t *testing.T) {
	// Empty => disabled.
	if d, ok, err := (agentmodel.ContentLogConfig{}).RotateEveryDuration(); err != nil || ok || d != 0 {
		t.Errorf("empty rotate_every: got (%v, %v, %v), want (0, false, nil)", d, ok, err)
	}
	if d, ok, err := (agentmodel.ContentLogConfig{RotateEvery: "24h"}).RotateEveryDuration(); err != nil || !ok || d != 24*time.Hour {
		t.Errorf("rotate_every 24h: got (%v, %v, %v)", d, ok, err)
	}
	if d, ok, err := (agentmodel.ContentLogConfig{MaxAge: "720h"}).MaxAgeDuration(); err != nil || !ok || d != 720*time.Hour {
		t.Errorf("max_age 720h: got (%v, %v, %v)", d, ok, err)
	}
	if _, _, err := (agentmodel.ContentLogConfig{RotateEvery: "nope"}).RotateEveryDuration(); err == nil {
		t.Error("malformed rotate_every should be rejected")
	}
}

func TestLoadConfig_RejectsBadContentLog(t *testing.T) {
	cases := map[string]string{
		"negative max_size_mb":  "  max_size_mb: -1",
		"negative max_total_mb": "  max_total_mb: -5",
		"negative max_backups":  "  max_backups: -2",
		"bad rotate_every":      `  rotate_every: "soon"`,
		"bad max_age":           `  max_age: "-3h"`,
	}
	for name, field := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeYAML(t, minimalModelList+"content_log:\n  path: \"/tmp/c.jsonl\"\n"+field+"\n")
			if _, err := agentmodel.LoadConfig(path); err == nil {
				t.Fatalf("expected error for %s, got nil", name)
			}
		})
	}
}

func TestLoadConfig_AcceptsContentLogRotation(t *testing.T) {
	path := writeYAML(t, minimalModelList+`
content_log:
  path: "/var/log/agentmodel/content.jsonl"
  max_size_mb: 100
  rotate_every: "24h"
  compress: true
  max_backups: 30
  max_age: "720h"
  max_total_mb: 2048
`)
	cfg, err := agentmodel.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ContentLog.MaxSizeMB != 100 || !cfg.ContentLog.Compress || cfg.ContentLog.MaxTotalMB != 2048 {
		t.Errorf("content_log not parsed: %+v", cfg.ContentLog)
	}
}

// minimalModelList is the boilerplate every budget/keys test case needs to
// pass the unrelated model_list validation.
const minimalModelList = `
model_list:
  - model_name: "gpt-4"
    deployments:
      - provider: "openai"
        model: "gpt-4o"
        auth_mode: "api_key"
        api_key_env: "OPENAI_API_KEY"
        weight: 1
  - model_name: "claude"
    deployments:
      - provider: "anthropic"
        model: "claude-sonnet-4"
        auth_mode: "api_key"
        api_key_env: "ANTHROPIC_API_KEY"
        weight: 1
`

func TestLoadConfig_BudgetAndKeys(t *testing.T) {
	path := writeYAML(t, minimalModelList+`
budget:
  max_budget: 100.5
  budget_duration: "720h"
keys:
  - name: "ci"
    token_env: "AGENT_MODEL_KEY_CI"
    max_budget: 10.0
    budget_duration: "24h"
    models: ["gpt-4", "claude"]
  - name: "scratch"
    token_env: "AGENT_MODEL_KEY_SCRATCH"
`)
	c, err := agentmodel.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if c.Budget.MaxBudget == nil || *c.Budget.MaxBudget != 100.5 {
		t.Errorf("budget.max_budget: got %v, want 100.5", c.Budget.MaxBudget)
	}
	if d, ok, err := c.Budget.Window(); err != nil || !ok || d != 720*time.Hour {
		t.Errorf("budget window: got (%v, ok=%v, err=%v), want (720h, true, nil)", d, ok, err)
	}
	if len(c.Keys) != 2 {
		t.Fatalf("keys: got %d, want 2", len(c.Keys))
	}
	ci := c.Keys[0]
	if ci.Name != "ci" || ci.TokenEnv != "AGENT_MODEL_KEY_CI" {
		t.Errorf("ci identity: got %+v", ci)
	}
	if ci.MaxBudget == nil || *ci.MaxBudget != 10.0 {
		t.Errorf("ci.max_budget: got %v, want 10", ci.MaxBudget)
	}
	if d, ok, err := ci.Window(); err != nil || !ok || d != 24*time.Hour {
		t.Errorf("ci window: got (%v, ok=%v, err=%v), want (24h, true, nil)", d, ok, err)
	}
	if len(ci.Models) != 2 {
		t.Errorf("ci.models: got %v, want 2 entries", ci.Models)
	}
	// Identity-only key: no cap, lifetime window reports not-set.
	scratch := c.Keys[1]
	if scratch.MaxBudget != nil {
		t.Errorf("scratch.max_budget: got %v, want nil", scratch.MaxBudget)
	}
	if _, ok, err := scratch.Window(); err != nil || ok {
		t.Errorf("scratch window: got (ok=%v, err=%v), want (false, nil)", ok, err)
	}
}

func TestLoadConfig_BudgetOmittedIsNil(t *testing.T) {
	path := writeYAML(t, minimalModelList)
	c, err := agentmodel.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if c.Budget.MaxBudget != nil {
		t.Errorf("budget.max_budget: got %v, want nil when omitted", c.Budget.MaxBudget)
	}
	if len(c.Keys) != 0 {
		t.Errorf("keys: got %d, want 0 when omitted", len(c.Keys))
	}
}

func TestLoadConfig_BudgetKeysValidationErrors(t *testing.T) {
	cases := []struct {
		desc    string
		yaml    string
		wantSub string
	}{
		{
			desc:    "zero org max_budget",
			yaml:    "budget:\n  max_budget: 0\n",
			wantSub: "max_budget must be a finite number > 0",
		},
		{
			desc:    "negative org max_budget",
			yaml:    "budget:\n  max_budget: -5\n",
			wantSub: "max_budget must be a finite number > 0",
		},
		{
			desc:    "org duration without max_budget",
			yaml:    "budget:\n  budget_duration: \"24h\"\n",
			wantSub: "budget_duration requires max_budget",
		},
		{
			desc:    "malformed org duration",
			yaml:    "budget:\n  max_budget: 10\n  budget_duration: \"monthly\"\n",
			wantSub: "budget_duration",
		},
		{
			desc:    "negative org duration",
			yaml:    "budget:\n  max_budget: 10\n  budget_duration: \"-24h\"\n",
			wantSub: "budget_duration must be positive",
		},
		{
			desc:    "key missing name",
			yaml:    "keys:\n  - token_env: \"K1\"\n",
			wantSub: "name is required",
		},
		{
			desc:    "duplicate key name",
			yaml:    "keys:\n  - name: \"a\"\n    token_env: \"K1\"\n  - name: \"a\"\n    token_env: \"K2\"\n",
			wantSub: "duplicate key name",
		},
		{
			desc:    "key missing token_env",
			yaml:    "keys:\n  - name: \"a\"\n",
			wantSub: "token_env is required",
		},
		{
			desc:    "duplicate token_env",
			yaml:    "keys:\n  - name: \"a\"\n    token_env: \"K1\"\n  - name: \"b\"\n    token_env: \"K1\"\n",
			wantSub: "duplicate token_env",
		},
		{
			desc:    "token_env collides with master",
			yaml:    "keys:\n  - name: \"a\"\n    token_env: \"AGENT_MODEL_TOKEN\"\n",
			wantSub: "collides with auth.bearer_token_env",
		},
		{
			desc:    "zero key max_budget",
			yaml:    "keys:\n  - name: \"a\"\n    token_env: \"K1\"\n    max_budget: 0\n",
			wantSub: "max_budget must be a finite number > 0",
		},
		{
			desc:    "key duration without max_budget",
			yaml:    "keys:\n  - name: \"a\"\n    token_env: \"K1\"\n    budget_duration: \"24h\"\n",
			wantSub: "budget_duration requires max_budget",
		},
		{
			desc:    "key allowlist references unknown model",
			yaml:    "keys:\n  - name: \"a\"\n    token_env: \"K1\"\n    models: [\"nope\"]\n",
			wantSub: "unknown model_name",
		},
		{
			desc:    "key allowlist duplicate model",
			yaml:    "keys:\n  - name: \"a\"\n    token_env: \"K1\"\n    models: [\"gpt-4\", \"gpt-4\"]\n",
			wantSub: "duplicate model_name",
		},
		{
			desc:    "non-finite org max_budget (+inf)",
			yaml:    "budget:\n  max_budget: .inf\n",
			wantSub: "finite number",
		},
		{
			desc:    "non-finite org max_budget (nan)",
			yaml:    "budget:\n  max_budget: .nan\n",
			wantSub: "finite number",
		},
		{
			desc:    "non-finite key max_budget",
			yaml:    "keys:\n  - name: \"a\"\n    token_env: \"K1\"\n    max_budget: .inf\n",
			wantSub: "finite number",
		},
		{
			desc:    "token_env collides with deployment api_key_env",
			yaml:    "keys:\n  - name: \"a\"\n    token_env: \"OPENAI_API_KEY\"\n",
			wantSub: "collides with a deployment api_key_env",
		},
		{
			desc:    "explicit empty models allowlist",
			yaml:    "keys:\n  - name: \"a\"\n    token_env: \"K1\"\n    models: []\n",
			wantSub: "models must be non-empty when set",
		},
		{
			desc:    "lifetime cap with retention enabled",
			yaml:    "budget:\n  max_budget: 10\nretention:\n  period: \"720h\"\n",
			wantSub: "lifetime cap",
		},
		{
			desc:    "budget window longer than retention",
			yaml:    "budget:\n  max_budget: 10\n  budget_duration: \"1440h\"\nretention:\n  period: \"720h\"\n",
			wantSub: "exceeds retention.period",
		},
		{
			desc:    "key lifetime cap with retention enabled",
			yaml:    "keys:\n  - name: \"a\"\n    token_env: \"K1\"\n    max_budget: 5\nretention:\n  period: \"720h\"\n",
			wantSub: "lifetime cap",
		},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			path := writeYAML(t, minimalModelList+tc.yaml)
			_, err := agentmodel.LoadConfig(path)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error: got %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestLoadConfig_BudgetDurationNormalized(t *testing.T) {
	// A whitespace-only duration is lifetime to the config layer; Validate
	// must normalize it so /v1/limits reporting agrees with Window().
	path := writeYAML(t, minimalModelList+"budget:\n  max_budget: 10\n  budget_duration: \"   \"\n")
	c, err := agentmodel.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if c.Budget.BudgetDuration != "" {
		t.Errorf("budget_duration: got %q, want normalized to empty", c.Budget.BudgetDuration)
	}
	if _, ok, err := c.Budget.Window(); err != nil || ok {
		t.Errorf("window: got (ok=%v, err=%v), want lifetime (false, nil)", ok, err)
	}
}

func TestLoadConfig_BudgetWithinRetentionOK(t *testing.T) {
	path := writeYAML(t, minimalModelList+"budget:\n  max_budget: 10\n  budget_duration: \"24h\"\nretention:\n  period: \"720h\"\n")
	if _, err := agentmodel.LoadConfig(path); err != nil {
		t.Fatalf("budget window inside retention must validate, got: %v", err)
	}
}

// TestValidate_CacheTTL guards #1494: cache_ttl must be a valid Anthropic value
// (WithCacheTTL would otherwise panic) and is anthropic-only.
func TestValidate_CacheTTL(t *testing.T) {
	load := func(yaml string) error {
		p := filepath.Join(t.TempDir(), "c.yaml")
		if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := agentmodel.LoadConfig(p)
		return err
	}
	base := func(provider, ttl string) string {
		return "model_list:\n  - model_name: m\n    deployments:\n      - provider: " + provider +
			"\n        model: x\n        auth_mode: api_key\n        api_key_env: K\n        cache_ttl: \"" + ttl + "\"\n        weight: 1\n"
	}
	if err := load(base("anthropic", "1h")); err != nil {
		t.Errorf("cache_ttl 1h on anthropic should be valid, got: %v", err)
	}
	if err := load(base("anthropic", "10m")); err == nil {
		t.Error("cache_ttl 10m should be rejected")
	}
	if err := load(base("openai", "5m")); err == nil {
		t.Error("cache_ttl on a non-anthropic provider should be rejected")
	}
}
