package factory

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/messagesbridge"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/pool"
)

// discardLogger returns a logger that drops all output, so test logs stay quiet
// while warn/error paths still execute.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ─── buildProvider ───────────────────────────────────────────────────────────

func TestBuildProvider(t *testing.T) {
	auther := &auth.StaticKey{HeaderName: "Authorization", Prefix: "Bearer ", Token: "tok"}
	cases := []struct {
		name     string
		wantName string
	}{
		{"openai", "openai"},
		{"anthropic", "anthropic"},
		{"anthropic-oauth", "anthropic"}, // anthropic-oauth maps to anthropic client
		{"gemini", "gemini"},
		{"chatgpt", "chatgpt"},
		{"deepseek", "deepseek"},
		{"azure", "azure"},
		// OpenAI-compatible providers build from the default-base-URL table with
		// no base_url, reporting their own name.
		{"groq", "groq"},
		{"mistral", "mistral"},
		{"together", "together"},
		{"xai", "xai"},
		{"openrouter", "openrouter"},
		{"fireworks", "fireworks"},
		{"perplexity", "perplexity"},
		// Chinese / open-source providers.
		{"zhipu", "zhipu"},
		{"moonshot", "moonshot"},
		{"qwen", "qwen"},
		{"yi", "yi"},
		{"baichuan", "baichuan"},
		{"stepfun", "stepfun"},
		{"siliconflow", "siliconflow"},
	}
	for _, c := range cases {
		p, err := buildProvider(c.name, auther, providerTarget{})
		if err != nil {
			t.Fatalf("buildProvider(%q): unexpected error %v", c.name, err)
		}
		if p == nil {
			t.Fatalf("buildProvider(%q): got nil provider", c.name)
		}
		if got := p.Name(); got != c.wantName {
			t.Errorf("buildProvider(%q): Name()=%q, want %q", c.name, got, c.wantName)
		}
	}
}

func TestBuildProviderWithBaseURL(t *testing.T) {
	auther := &auth.StaticKey{HeaderName: "Authorization", Prefix: "Bearer ", Token: "tok"}
	// A non-empty base_url must still build a usable provider of the same Name.
	for _, name := range []string{"openai", "anthropic", "gemini", "chatgpt", "deepseek", "azure"} {
		p, err := buildProvider(name, auther, providerTarget{baseURL: "http://127.0.0.1:1234", apiVersion: "2024-10-01-preview", deployment: "dep"})
		if err != nil {
			t.Fatalf("buildProvider(%q, baseURL): unexpected error %v", name, err)
		}
		if p == nil {
			t.Fatalf("buildProvider(%q, baseURL): got nil provider", name)
		}
	}
}

func TestBuildProviderUnknown(t *testing.T) {
	p, err := buildProvider("does-not-exist", nil, providerTarget{})
	if err == nil {
		t.Fatal("buildProvider(unknown): expected error, got nil")
	}
	if p != nil {
		t.Errorf("buildProvider(unknown): provider=%v, want nil", p)
	}
}

// TestStaticAuthFor verifies the per-provider auth header mapping.
func TestStaticAuthFor(t *testing.T) {
	cases := []struct {
		provider   string
		wantHeader string
		wantPrefix string
	}{
		{"openai", "Authorization", "Bearer "},
		{"chatgpt", "Authorization", "Bearer "},
		{"anthropic", "x-api-key", ""},
		{"anthropic-oauth", "x-api-key", ""},
		{"gemini", "x-goog-api-key", ""},
		{"deepseek", "Authorization", "Bearer "},                // OpenAI-compatible Bearer auth
		{"unknown-future-provider", "Authorization", "Bearer "}, // default fallback
	}
	for _, c := range cases {
		got := staticAuthFor(c.provider, "tok")
		if got.HeaderName != c.wantHeader {
			t.Errorf("%s: header got %q, want %q", c.provider, got.HeaderName, c.wantHeader)
		}
		if got.Prefix != c.wantPrefix {
			t.Errorf("%s: prefix got %q, want %q", c.provider, got.Prefix, c.wantPrefix)
		}
		if got.Token != "tok" {
			t.Errorf("%s: token got %q, want tok", c.provider, got.Token)
		}
	}
}

// ─── buildProviderPool ───────────────────────────────────────────────────────

func TestBuildProviderPoolSingleToken(t *testing.T) {
	t.Setenv("KEY_A", "secret-a")
	authFn := func(token string) auth.Authenticator {
		return staticAuthFor("openai", token)
	}
	p, err := buildProviderPool("openai", []string{"KEY_A"}, providerTarget{}, authFn, "m", discardLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("expected a single provider, got nil")
	}
	if p.Name() != "openai" {
		t.Errorf("Name()=%q, want openai (single provider, not pool)", p.Name())
	}
}

func TestBuildProviderPoolMultipleTokensReturnsPool(t *testing.T) {
	t.Setenv("KEY_A", "secret-a")
	t.Setenv("KEY_B", "secret-b")
	authFn := func(token string) auth.Authenticator {
		return staticAuthFor("openai", token)
	}
	p, err := buildProviderPool("openai", []string{"KEY_A", "KEY_B"}, providerTarget{}, authFn, "m", discardLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("expected a pool provider, got nil")
	}
	// Two tokens must wrap in pool.New. Assert by type, not Name(): a pool now
	// delegates Name() to its (homogeneous) underlying provider so cost pricing
	// resolves, so "pool" is no longer an observable marker.
	if _, ok := p.(*pool.Pool); !ok {
		t.Errorf("want *pool.Pool, got %T", p)
	}
}

func TestBuildProviderPoolSkipsUnsetEnv(t *testing.T) {
	// KEY_A set, KEY_MISSING unset -> only one usable token -> single provider.
	t.Setenv("KEY_A", "secret-a")
	t.Setenv("KEY_MISSING", "")
	authFn := func(token string) auth.Authenticator {
		return staticAuthFor("openai", token)
	}
	p, err := buildProviderPool("openai", []string{"KEY_MISSING", "KEY_A"}, providerTarget{}, authFn, "m", discardLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("expected single provider, got nil")
	}
	if p.Name() != "openai" {
		t.Errorf("Name()=%q, want openai (one token skipped, one usable)", p.Name())
	}
}

func TestBuildProviderPoolNoTokens(t *testing.T) {
	t.Setenv("KEY_MISSING", "")
	authFn := func(token string) auth.Authenticator {
		return staticAuthFor("openai", token)
	}
	p, err := buildProviderPool("openai", []string{"KEY_MISSING"}, providerTarget{}, authFn, "m", discardLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p != nil {
		t.Errorf("expected nil provider when no tokens available, got %v", p)
	}
}

func TestBuildProviderPoolKeylessBaseURL(t *testing.T) {
	// No key envs but base_url set: build exactly one provider with an empty
	// static key (a local/fake OpenAI-compatible backend).
	authFn := func(token string) auth.Authenticator {
		return staticAuthFor("openai", token)
	}
	p, err := buildProviderPool("openai", nil, providerTarget{baseURL: "http://127.0.0.1:1234"}, authFn, "m", discardLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("expected a single keyless provider, got nil")
	}
	if p.Name() != "openai" {
		t.Errorf("Name()=%q, want openai (keyless base_url single provider)", p.Name())
	}
}

func TestBuildProviderPoolUnknownProviderErrors(t *testing.T) {
	t.Setenv("KEY_A", "secret-a")
	authFn := func(token string) auth.Authenticator {
		return staticAuthFor("openai", token)
	}
	p, err := buildProviderPool("bogus-provider", []string{"KEY_A"}, providerTarget{}, authFn, "m", discardLogger())
	if err == nil {
		t.Fatal("expected error for unknown provider, got nil")
	}
	if p != nil {
		t.Errorf("provider=%v, want nil on error", p)
	}
}

// ─── buildDeploymentProvider ─────────────────────────────────────────────────

func TestBuildDeploymentProviderAPIKey(t *testing.T) {
	t.Setenv("OPENAI_KEY", "sk-test")
	d := agentmodel.DeploymentConfig{
		Provider:  "openai",
		Model:     "gpt-4",
		AuthMode:  agentmodel.AuthModeAPIKey,
		APIKeyEnv: "OPENAI_KEY",
	}
	p, chatgptAuth, err := buildDeploymentProvider(d, "gpt-4", discardLogger(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chatgptAuth != nil {
		t.Errorf("chatgptAuth should be nil for non-chatgpt deployment")
	}
	if p == nil || p.Name() != "openai" {
		t.Fatalf("provider=%v, want openai", p)
	}
}

func TestBuildDeploymentProviderAzure(t *testing.T) {
	// End-to-end: provider:azure must route to the azure client with
	// api-key auth and a deployment that defaults to the model name, then dispatch
	// to Azure's deployment-scoped path with the api-version query.
	var gotPath, gotQuery, gotAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotAPIKey = r.URL.Path, r.URL.RawQuery, r.Header.Get("api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()

	t.Setenv("AZURE_KEY", "az-secret")
	d := agentmodel.DeploymentConfig{
		Provider:   "azure",
		Model:      "gpt-4o", // no deployment_name → deployment defaults to the model
		AuthMode:   agentmodel.AuthModeAPIKey,
		APIKeyEnv:  "AZURE_KEY",
		BaseURL:    srv.URL,
		APIVersion: "2024-10-01-preview",
	}
	p, _, err := buildDeploymentProvider(d, "gpt-4o", discardLogger(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p == nil || p.Name() != "azure" {
		t.Fatalf("provider=%v, want azure", p)
	}
	if _, err := p.Complete(context.Background(), agentmodel.ChatRequest{
		Model:    "gpt-4o",
		Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotPath != "/openai/deployments/gpt-4o/chat/completions" {
		t.Errorf("path = %q, want deployment defaulting to model gpt-4o", gotPath)
	}
	if gotQuery != "api-version=2024-10-01-preview" {
		t.Errorf("query = %q, want api-version=2024-10-01-preview", gotQuery)
	}
	if gotAPIKey != "az-secret" {
		t.Errorf("api-key header = %q, want az-secret", gotAPIKey)
	}
}

func TestBuildDeploymentProviderKeylessBaseURL(t *testing.T) {
	// api_key mode, no api_key_env, base_url set: builds one keyless deployment.
	d := agentmodel.DeploymentConfig{
		Provider: "openai",
		Model:    "llama3",
		AuthMode: agentmodel.AuthModeAPIKey,
		BaseURL:  "http://127.0.0.1:11434/v1",
	}
	p, _, err := buildDeploymentProvider(d, "local", discardLogger(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p == nil || p.Name() != "openai" {
		t.Fatalf("provider=%v, want openai (keyless base_url)", p)
	}
}

func TestBuildDeploymentProviderAPIKeyDefaultAuthMode(t *testing.T) {
	// AuthMode "" should behave like api_key.
	t.Setenv("OPENAI_KEY", "sk-test")
	d := agentmodel.DeploymentConfig{
		Provider:  "openai",
		Model:     "gpt-4",
		AuthMode:  "",
		APIKeyEnv: "OPENAI_KEY",
	}
	p, _, err := buildDeploymentProvider(d, "gpt-4", discardLogger(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p == nil || p.Name() != "openai" {
		t.Fatalf("provider=%v, want openai", p)
	}
}

func TestBuildDeploymentProviderAPIKeyMissingCredsSkips(t *testing.T) {
	t.Setenv("OPENAI_KEY", "")
	d := agentmodel.DeploymentConfig{
		Provider:  "openai",
		Model:     "gpt-4",
		AuthMode:  agentmodel.AuthModeAPIKey,
		APIKeyEnv: "OPENAI_KEY",
	}
	p, chatgptAuth, err := buildDeploymentProvider(d, "gpt-4", discardLogger(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p != nil || chatgptAuth != nil {
		t.Errorf("expected (nil, nil) skip on missing creds, got (%v, %v)", p, chatgptAuth)
	}
}

func TestBuildDeploymentProviderAPIKeyEnvsPreferred(t *testing.T) {
	// api_key_envs should be preferred over api_key_env; two keys -> pool.
	t.Setenv("KEY_1", "k1")
	t.Setenv("KEY_2", "k2")
	t.Setenv("IGNORED", "should-not-be-used")
	d := agentmodel.DeploymentConfig{
		Provider:   "openai",
		Model:      "gpt-4",
		AuthMode:   agentmodel.AuthModeAPIKey,
		APIKeyEnv:  "IGNORED",
		APIKeyEnvs: []string{"KEY_1", "KEY_2"},
	}
	p, _, err := buildDeploymentProvider(d, "gpt-4", discardLogger(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := p.(*pool.Pool); !ok {
		t.Fatalf("provider=%T, want *pool.Pool (api_key_envs with two keys)", p)
	}
}

func TestBuildDeploymentProviderSubscriptionAnthropicOAuthToken(t *testing.T) {
	t.Setenv("ANT_OAUTH", "sk-ant-oat-abc")
	d := agentmodel.DeploymentConfig{
		Provider:  "anthropic",
		Model:     "claude-3-5-sonnet",
		AuthMode:  agentmodel.AuthModeSubscription,
		APIKeyEnv: "ANT_OAUTH",
	}
	p, chatgptAuth, err := buildDeploymentProvider(d, "claude", discardLogger(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chatgptAuth != nil {
		t.Errorf("chatgptAuth should be nil for anthropic subscription")
	}
	if p == nil || p.Name() != "anthropic" {
		t.Fatalf("provider=%v, want anthropic", p)
	}
}

func TestBuildDeploymentProviderSubscriptionAnthropicMissingCredsSkips(t *testing.T) {
	t.Setenv("ANT_OAUTH", "")
	d := agentmodel.DeploymentConfig{
		Provider:  "anthropic",
		Model:     "claude-3-5-sonnet",
		AuthMode:  agentmodel.AuthModeSubscription,
		APIKeyEnv: "ANT_OAUTH",
	}
	p, _, err := buildDeploymentProvider(d, "claude", discardLogger(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p != nil {
		t.Errorf("expected nil provider on missing subscription creds, got %v", p)
	}
}

func TestBuildDeploymentProviderSubscriptionAnthropicOAuthTokenDir(t *testing.T) {
	// oauth_token_dir takes precedence; no env token needed.
	dir := t.TempDir()
	d := agentmodel.DeploymentConfig{
		Provider:      "anthropic-oauth",
		Model:         "claude-3-5-sonnet",
		AuthMode:      agentmodel.AuthModeSubscription,
		OAuthTokenDir: dir,
		APIKeyEnv:     "SOME_KEY", // present but should be ignored (warns)
	}
	t.Setenv("SOME_KEY", "ignored")
	p, chatgptAuth, err := buildDeploymentProvider(d, "claude", discardLogger(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chatgptAuth != nil {
		t.Errorf("chatgptAuth should be nil for anthropic-oauth")
	}
	if p == nil || p.Name() != "anthropic" {
		t.Fatalf("provider=%v, want anthropic (oauth_token_dir path)", p)
	}
}

func TestBuildDeploymentProviderSubscriptionChatGPTCreatesAuth(t *testing.T) {
	dir := t.TempDir()
	d := agentmodel.DeploymentConfig{
		Provider:      "chatgpt",
		Model:         "gpt-4o",
		AuthMode:      agentmodel.AuthModeSubscription,
		OAuthTokenDir: dir,
	}
	p, chatgptAuth, err := buildDeploymentProvider(d, "gpt-4o", discardLogger(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chatgptAuth == nil {
		t.Fatal("expected a non-nil shared ChatGPT auth to be returned")
	}
	if p == nil || p.Name() != "chatgpt" {
		t.Fatalf("provider=%v, want chatgpt", p)
	}
}

func TestBuildDeploymentProviderSubscriptionChatGPTReusesExistingAuth(t *testing.T) {
	dir := t.TempDir()
	existing := auth.NewChatGPTOAuth(dir, nil)
	d := agentmodel.DeploymentConfig{
		Provider:      "chatgpt",
		Model:         "gpt-4o",
		AuthMode:      agentmodel.AuthModeSubscription,
		OAuthTokenDir: dir,
	}
	p, chatgptAuth, err := buildDeploymentProvider(d, "gpt-4o", discardLogger(), existing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chatgptAuth != existing {
		t.Errorf("expected the existing shared ChatGPT auth to be reused/returned")
	}
	if p == nil || p.Name() != "chatgpt" {
		t.Fatalf("provider=%v, want chatgpt", p)
	}
}

func TestBuildDeploymentProviderSubscriptionAnthropicOAuthTokenDirsPool(t *testing.T) {
	// Two accounts, each its own refreshable auth.json directory: the deployment
	// must become a rotating pool, not silently fall through to api_key_envs.
	d := agentmodel.DeploymentConfig{
		Provider:       "anthropic",
		Model:          "claude-3-5-sonnet",
		AuthMode:       agentmodel.AuthModeSubscription,
		OAuthTokenDirs: []string{t.TempDir(), t.TempDir()},
	}
	p, chatgptAuth, err := buildDeploymentProvider(d, "claude", discardLogger(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chatgptAuth != nil {
		t.Errorf("chatgptAuth should be nil for anthropic subscription")
	}
	if p == nil {
		t.Fatal("provider is nil, want a *pool.Pool")
	}
	if _, ok := p.(*pool.Pool); !ok {
		t.Fatalf("provider=%T, want *pool.Pool (two dirs should wrap in pool.New)", p)
	}
	// Unlike chatgpt, anthropic speaks /v1/messages natively — no bridge wrapper.
	if _, ok := p.(*messagesbridge.Bridge); ok {
		t.Error("anthropic pool must not be wrapped in a messagesbridge")
	}
	// Name() must stay "anthropic" so the cost registry keys resolve.
	if p.Name() != "anthropic" {
		t.Errorf("Name()=%q, want anthropic (pool delegates to its underlying provider)", p.Name())
	}
}

func TestBuildDeploymentProviderSubscriptionAnthropicOAuthTokenDirsSingleIsNotPooled(t *testing.T) {
	// A one-entry list is the singular oauth_token_dir spelled differently: it
	// must build the bare provider, not a pool of one (a pool of one adds a
	// cooldown layer that can only ever park the sole credential).
	d := agentmodel.DeploymentConfig{
		Provider:       "anthropic-oauth",
		Model:          "claude-3-5-sonnet",
		AuthMode:       agentmodel.AuthModeSubscription,
		OAuthTokenDirs: []string{t.TempDir()},
	}
	p, _, err := buildDeploymentProvider(d, "claude", discardLogger(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("provider is nil, want a bare anthropic client")
	}
	if _, ok := p.(*pool.Pool); ok {
		t.Errorf("provider=%T, want a bare client (one dir must not be pooled)", p)
	}
	if p.Name() != "anthropic" {
		t.Errorf("Name()=%q, want anthropic", p.Name())
	}
}

func TestBuildDeploymentProviderSubscriptionChatGPTOAuthTokenDirsPool(t *testing.T) {
	d := agentmodel.DeploymentConfig{
		Provider:       "chatgpt",
		Model:          "gpt-5",
		AuthMode:       agentmodel.AuthModeSubscription,
		OAuthTokenDirs: []string{t.TempDir(), t.TempDir()},
	}
	p, chatgptAuth, err := buildDeploymentProvider(d, "gpt-5", discardLogger(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chatgptAuth == nil {
		t.Fatal("expected the first account's auth to be returned as the shared client")
	}
	// Two dirs => a messagesbridge.Bridge wrapping a rotating pool. Assert by
	// type through the bridge's embedded provider: Name() no longer marks a pool
	// (it delegates to the underlying "chatgpt" so cost pricing resolves, ).
	if p == nil {
		t.Fatal("provider is nil, want a bridge-wrapped pool")
	}
	br, ok := p.(*messagesbridge.Bridge)
	if !ok {
		t.Fatalf("provider=%T, want *messagesbridge.Bridge", p)
	}
	if _, ok := br.Provider.(*pool.Pool); !ok {
		t.Fatalf("bridge wraps %T, want *pool.Pool (two dirs should wrap in pool.New)", br.Provider)
	}
	if p.Name() != "chatgpt" {
		t.Errorf("Name()=%q, want chatgpt (pool delegates to its underlying provider)", p.Name())
	}
}

func TestBuildDeploymentProviderSubscriptionChatGPTOAuthTokenDirsKeepsFirstSharedAuth(t *testing.T) {
	existing := auth.NewChatGPTOAuth(t.TempDir(), nil)
	d := agentmodel.DeploymentConfig{
		Provider:       "chatgpt",
		Model:          "gpt-5",
		AuthMode:       agentmodel.AuthModeSubscription,
		OAuthTokenDirs: []string{t.TempDir(), t.TempDir()},
	}
	// An earlier chatgpt deployment already established the shared auth; the
	// pool deployment must not steal /v1/oauth from it.
	_, chatgptAuth, err := buildDeploymentProvider(d, "gpt-5", discardLogger(), existing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chatgptAuth != existing {
		t.Errorf("expected the pre-existing shared auth to be preserved (first wins)")
	}
}

func TestBuildDeploymentProviderSubscriptionUnsupportedProvider(t *testing.T) {
	d := agentmodel.DeploymentConfig{
		Provider:  "gemini",
		Model:     "gemini-pro",
		AuthMode:  agentmodel.AuthModeSubscription,
		APIKeyEnv: "X",
	}
	p, _, err := buildDeploymentProvider(d, "gemini", discardLogger(), nil)
	if err == nil {
		t.Fatal("expected error: gemini does not support subscription auth_mode")
	}
	if p != nil {
		t.Errorf("provider=%v, want nil on error", p)
	}
}

func TestBuildDeploymentProviderUnknownAuthMode(t *testing.T) {
	d := agentmodel.DeploymentConfig{
		Provider:  "openai",
		Model:     "gpt-4",
		AuthMode:  "carrier-pigeon",
		APIKeyEnv: "X",
	}
	p, _, err := buildDeploymentProvider(d, "gpt-4", discardLogger(), nil)
	if err == nil {
		t.Fatal("expected error for unknown auth_mode")
	}
	if p != nil {
		t.Errorf("provider=%v, want nil on error", p)
	}
}

// ─── BuildDeployments ────────────────────────────────────────────────────────

func TestBuildDeploymentsWiresAndSkips(t *testing.T) {
	t.Setenv("OPENAI_KEY", "sk-test")
	t.Setenv("MISSING_KEY", "")
	cfg := &agentmodel.Config{
		ModelList: []agentmodel.ModelEntry{
			{
				ModelName: "gpt",
				Deployments: []agentmodel.DeploymentConfig{
					{Provider: "openai", Model: "gpt-4", AuthMode: agentmodel.AuthModeAPIKey, APIKeyEnv: "OPENAI_KEY", Weight: wptr(100)},
				},
			},
			{
				ModelName: "skipped-model",
				Deployments: []agentmodel.DeploymentConfig{
					{Provider: "openai", Model: "gpt-4", AuthMode: agentmodel.AuthModeAPIKey, APIKeyEnv: "MISSING_KEY", Weight: wptr(100)},
				},
			},
		},
	}
	deployments, chatgptAuth, err := BuildDeployments(cfg, discardLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chatgptAuth != nil {
		t.Errorf("chatgptAuth should be nil with no chatgpt deployments")
	}
	if _, ok := deployments["gpt"]; !ok {
		t.Errorf("expected model gpt to have deployments")
	}
	if got := len(deployments["gpt"]); got != 1 {
		t.Errorf("gpt deployments = %d, want 1", got)
	}
	dep := deployments["gpt"][0]
	if dep.Name != "openai/gpt-4|api_key" {
		t.Errorf("deployment name = %q, want openai/gpt-4|api_key (auth mode disambiguates the cooldown/rate-meter identity)", dep.Name)
	}
	if dep.Weight != 100 {
		t.Errorf("deployment weight = %d, want 100", dep.Weight)
	}
	var _ provider.Provider = dep.Provider
	// The model whose only deployment was skipped should not appear.
	if _, ok := deployments["skipped-model"]; ok {
		t.Errorf("skipped-model should have no usable deployments and be absent")
	}
}

func TestBuildDeploymentsSharesChatGPTAuth(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	cfg := &agentmodel.Config{
		ModelList: []agentmodel.ModelEntry{
			{
				ModelName: "chatgpt-a",
				Deployments: []agentmodel.DeploymentConfig{
					{Provider: "chatgpt", Model: "gpt-4o", AuthMode: agentmodel.AuthModeSubscription, OAuthTokenDir: dir1, Weight: wptr(100)},
				},
			},
			{
				ModelName: "chatgpt-b",
				Deployments: []agentmodel.DeploymentConfig{
					{Provider: "chatgpt", Model: "gpt-4o-mini", AuthMode: agentmodel.AuthModeSubscription, OAuthTokenDir: dir2, Weight: wptr(100)},
				},
			},
		},
	}
	deployments, chatgptAuth, err := BuildDeployments(cfg, discardLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chatgptAuth == nil {
		t.Fatal("expected a shared ChatGPT auth instance")
	}
	if len(deployments) != 2 {
		t.Errorf("expected 2 models wired, got %d", len(deployments))
	}
}

func TestBuildDeploymentsPropagatesError(t *testing.T) {
	cfg := &agentmodel.Config{
		ModelList: []agentmodel.ModelEntry{
			{
				ModelName: "bad",
				Deployments: []agentmodel.DeploymentConfig{
					{Provider: "openai", Model: "gpt-4", AuthMode: "totally-bogus", APIKeyEnv: "X", Weight: wptr(100)},
				},
			},
		},
	}
	_, _, err := BuildDeployments(cfg, discardLogger())
	if err == nil {
		t.Fatal("expected BuildDeployments to propagate the unknown auth_mode error")
	}
}

// TestBuildProvider_OpenAICompat covers unlisted OpenAI-compatible endpoints
// (explicit base_url) and the unknown-provider error.
func TestBuildProvider_OpenAICompat(t *testing.T) {
	auther := &auth.StaticKey{HeaderName: "Authorization", Prefix: "Bearer ", Token: "tok"}
	// Any unlisted OpenAI-compatible endpoint works with an explicit base_url,
	// reporting its configured name.
	p, err := buildProvider("myvendor", auther, providerTarget{baseURL: "https://api.myvendor.com/v1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p == nil || p.Name() != "myvendor" {
		t.Fatalf("got %v, want Name()=myvendor", p)
	}
	// A truly-unknown provider with no base_url is an error.
	if _, err := buildProvider("bogus", auther, providerTarget{}); err == nil {
		t.Error("unknown provider with no base_url should error")
	}
}

// TestBuildDeployments_SameProviderModelDifferentAuthGetDistinctNames guards
// : two deployments under one model_name that share a provider string and
// model but differ in auth mode must get DISTINCT Names. dep.Name is the cooldown
// and rate-meter identity (router.depKey), so identical Names would make cooling
// the subscription deployment after a 429 also park the api_key one — defeating
// the very failover the pair exists to provide — and co-mingle their RPM/TPM.
func TestBuildDeployments_SameProviderModelDifferentAuthGetDistinctNames(t *testing.T) {
	t.Setenv("ANTHROPIC_KEY", "sk-test")
	cfg := &agentmodel.Config{
		ModelList: []agentmodel.ModelEntry{{
			ModelName: "claude",
			Deployments: []agentmodel.DeploymentConfig{
				{Provider: "anthropic", Model: "claude-3-5-sonnet", AuthMode: agentmodel.AuthModeSubscription, OAuthTokenDir: t.TempDir(), Weight: wptr(1)},
				{Provider: "anthropic", Model: "claude-3-5-sonnet", AuthMode: agentmodel.AuthModeAPIKey, APIKeyEnv: "ANTHROPIC_KEY", Weight: wptr(1)},
			},
		}},
	}
	deps, _, err := BuildDeployments(cfg, discardLogger())
	if err != nil {
		t.Fatalf("BuildDeployments: %v", err)
	}
	got := deps["claude"]
	if len(got) != 2 {
		t.Fatalf("deployments = %d, want 2", len(got))
	}
	if got[0].Name == got[1].Name {
		t.Errorf("two deployments differing only in auth mode share Name %q — they will share one cooldown/rate-limit bucket", got[0].Name)
	}
}

// wptr returns a pointer to w, for DeploymentConfig.Weight (*int) literals.
func wptr(w int) *int { return &w }

// TestBuildDeployments_Weight0Disables guards : an explicit weight: 0 drops
// the deployment; an omitted weight defaults to 1.
func TestBuildDeployments_Weight0Disables(t *testing.T) {
	t.Setenv("K1", "a")
	t.Setenv("K2", "b")
	zero := 0
	cfg := &agentmodel.Config{ModelList: []agentmodel.ModelEntry{{
		ModelName: "m",
		Deployments: []agentmodel.DeploymentConfig{
			{Provider: "openai", Model: "gpt-4o", AuthMode: "api_key", APIKeyEnv: "K1", Weight: wptr(5)},
			{Provider: "anthropic", Model: "claude", AuthMode: "api_key", APIKeyEnv: "K2", Weight: &zero}, // disabled
		},
	}}}
	deps, _, err := BuildDeployments(cfg, discardLogger())
	if err != nil {
		t.Fatalf("BuildDeployments: %v", err)
	}
	got := deps["m"]
	if len(got) != 1 {
		t.Fatalf("deployments = %d, want 1 (weight:0 must be disabled)", len(got))
	}
	if got[0].Weight != 5 {
		t.Errorf("surviving weight = %d, want 5", got[0].Weight)
	}
}

func TestBuildDeployments_OmittedWeightDefaultsTo1(t *testing.T) {
	t.Setenv("K1", "a")
	cfg := &agentmodel.Config{ModelList: []agentmodel.ModelEntry{{
		ModelName: "m",
		Deployments: []agentmodel.DeploymentConfig{
			{Provider: "openai", Model: "gpt-4o", AuthMode: "api_key", APIKeyEnv: "K1"}, // Weight nil
		},
	}}}
	deps, _, err := BuildDeployments(cfg, discardLogger())
	if err != nil {
		t.Fatalf("BuildDeployments: %v", err)
	}
	if got := deps["m"][0].Weight; got != 1 {
		t.Errorf("omitted weight built as %d, want 1", got)
	}
}

// TestBuildDeployments_CacheTTLBuildsWithoutPanic guards : a valid cache_ttl
// threads into anthropic.WithCacheTTL without panicking.
func TestBuildDeployments_CacheTTLBuildsWithoutPanic(t *testing.T) {
	t.Setenv("AK", "x")
	cfg := &agentmodel.Config{ModelList: []agentmodel.ModelEntry{{
		ModelName: "c",
		Deployments: []agentmodel.DeploymentConfig{
			{Provider: "anthropic", Model: "claude", AuthMode: "api_key", APIKeyEnv: "AK", CacheTTL: "1h", Weight: wptr(1)},
		},
	}}}
	deps, _, err := BuildDeployments(cfg, discardLogger())
	if err != nil {
		t.Fatalf("BuildDeployments: %v", err)
	}
	if len(deps["c"]) != 1 {
		t.Fatalf("want 1 anthropic deployment, got %d", len(deps["c"]))
	}
}
