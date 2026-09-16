package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/stub"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
)

func TestMainExitBranches(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		setup      func(*testing.T)
		wantCode   int
		wantStderr string
	}{
		{name: "no args", args: []string{"agent-model"}, wantCode: 2, wantStderr: "agent-model"},
		{name: "unknown", args: []string{"agent-model", "nope"}, wantCode: 2, wantStderr: "unknown command"},
		{name: "migrate", args: []string{"agent-model", "migrate"}, wantCode: 1, wantStderr: "migrate failed"},
		{name: "purge", args: []string{"agent-model", "purge"}, wantCode: 1, wantStderr: "purge failed"},
		{
			name:       "prompt",
			args:       []string{"agent-model", "prompt"},
			setup:      func(t *testing.T) { t.Setenv("AGENT_MODEL_URL", "http://127.0.0.1:1") },
			wantCode:   1,
			wantStderr: "prompt failed",
		},
		{
			name:       "usage",
			args:       []string{"agent-model", "usage"},
			setup:      func(t *testing.T) { t.Setenv("AGENT_MODEL_CONFIG", filepath.Join(t.TempDir(), "missing.yaml")) },
			wantCode:   1,
			wantStderr: "usage failed",
		},
		{name: "profile", args: []string{"agent-model", "profile"}, wantCode: 1, wantStderr: "profile failed"},
		{
			name:       "serve",
			args:       []string{"agent-model", "serve"},
			setup:      func(t *testing.T) { t.Setenv("AGENT_MODEL_CONFIG", filepath.Join(t.TempDir(), "missing.yaml")) },
			wantCode:   1,
			wantStderr: "server failed",
		},
		{
			name: "anthropic-login",
			args: []string{"agent-model", "anthropic-login", "--profile", "Work"},
			setup: func(t *testing.T) {
				t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", t.TempDir())
				t.Setenv(auth.ProfileEnvVar, "")
			},
			wantCode:   1,
			wantStderr: "anthropic-login failed",
		},
		{
			name: "chatgpt-login",
			args: []string{"agent-model", "chatgpt-login", "--profile", "Work"},
			setup: func(t *testing.T) {
				t.Setenv("CHATGPT_TOKEN_DIR", t.TempDir())
			},
			wantCode:   1,
			wantStderr: "chatgpt-login failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup(t)
			}
			oldArgs := os.Args
			oldStderr := os.Stderr
			oldExit := osExit
			stderr, err := os.CreateTemp(t.TempDir(), "stderr-*")
			if err != nil {
				t.Fatal(err)
			}
			os.Args = tt.args
			os.Stderr = stderr
			var exitCode int
			osExit = func(code int) {
				exitCode = code
				panic(mainExit{})
			}
			defer func() {
				os.Args = oldArgs
				os.Stderr = oldStderr
				osExit = oldExit
				_ = stderr.Close()
			}()
			defer func() {
				if r := recover(); r != nil {
					if _, ok := r.(mainExit); !ok {
						t.Fatalf("main panic = %v", r)
					}
					if exitCode != tt.wantCode {
						t.Fatalf("exit code = %d, want %d", exitCode, tt.wantCode)
					}
					if _, err := stderr.Seek(0, 0); err != nil {
						t.Fatal(err)
					}
					raw, err := io.ReadAll(stderr)
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(string(raw), tt.wantStderr) {
						t.Fatalf("stderr = %q, want %q", raw, tt.wantStderr)
					}
					return
				}
				t.Fatal("main returned without exiting")
			}()

			main()
		})
	}
}

func TestMainSuccessBranches(t *testing.T) {
	for _, tt := range []struct {
		name       string
		args       []string
		wantStdout string
	}{
		{name: "help", args: []string{"agent-model", "--help"}, wantStdout: "agent-model"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			oldArgs := os.Args
			oldStdout := os.Stdout
			oldExit := osExit
			stdout, err := os.CreateTemp(t.TempDir(), "stdout-*")
			if err != nil {
				t.Fatal(err)
			}
			os.Args = tt.args
			os.Stdout = stdout
			osExit = func(code int) {
				t.Fatalf("main called osExit(%d)", code)
			}
			defer func() {
				os.Args = oldArgs
				os.Stdout = oldStdout
				osExit = oldExit
				_ = stdout.Close()
			}()

			main()

			if _, err := stdout.Seek(0, 0); err != nil {
				t.Fatal(err)
			}
			raw, err := io.ReadAll(stdout)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), tt.wantStdout) {
				t.Fatalf("stdout = %q, want %q", raw, tt.wantStdout)
			}
		})
	}
}

type mainExit struct{}

func TestRunOAuthLoginSuccessActivates(t *testing.T) {
	oldArgs := os.Args
	os.Args = []string{"agent-model", "fake-login", "--token-dir", "tokens", "--profile", "work", "--activate"}
	defer func() { os.Args = oldArgs }()

	var loginDir, activated string
	out, err := captureStdout(t, func() error {
		return runOAuthLogin(discardLogger(), oauthLoginSpec{
			provider: "fake",
			resolve: func(profileFlag, tokenDirFlag string) (string, string, error) {
				if profileFlag != "work" || tokenDirFlag != "tokens" {
					t.Fatalf("resolve flags = %q/%q", profileFlag, tokenDirFlag)
				}
				return tokenDirFlag, profileFlag, nil
			},
			login: func(ctx context.Context, tokenDir string) error {
				loginDir = tokenDir
				return nil
			},
			writeActive: func(profile string) error {
				activated = profile
				return nil
			},
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	if loginDir != "tokens" || activated != "work" {
		t.Fatalf("loginDir=%q activated=%q", loginDir, activated)
	}
	if !strings.Contains(out, "Logged in") {
		t.Fatalf("stdout = %q, want success message", out)
	}
}

func TestRunOAuthLoginErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		spec oauthLoginSpec
		want string
	}{
		{
			name: "resolve",
			args: []string{"agent-model", "fake-login"},
			spec: oauthLoginSpec{
				provider: "fake",
				resolve: func(string, string) (string, string, error) {
					return "", "", errors.New("resolve failed")
				},
			},
			want: "resolve failed",
		},
		{
			name: "login",
			args: []string{"agent-model", "fake-login"},
			spec: oauthLoginSpec{
				provider: "fake",
				resolve: func(string, string) (string, string, error) {
					return "tokens", "work", nil
				},
				login: func(context.Context, string) error {
					return errors.New("login failed")
				},
			},
			want: "login failed",
		},
		{
			name: "activate",
			args: []string{"agent-model", "fake-login", "--activate"},
			spec: oauthLoginSpec{
				provider: "fake",
				resolve: func(string, string) (string, string, error) {
					return "tokens", "work", nil
				},
				login: func(context.Context, string) error {
					return nil
				},
				writeActive: func(string) error {
					return errors.New("activate failed")
				},
			},
			want: "activate failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldArgs := os.Args
			os.Args = tt.args
			defer func() { os.Args = oldArgs }()

			err := runOAuthLogin(discardLogger(), tt.spec)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("runOAuthLogin error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestRunChatGPTLoginInvalidProfile(t *testing.T) {
	t.Setenv("CHATGPT_TOKEN_DIR", t.TempDir())

	oldArgs := os.Args
	os.Args = []string{"agent-model", "chatgpt-login", "--profile", "Work"}
	defer func() { os.Args = oldArgs }()

	err := runChatGPTLogin(discardLogger())
	if err == nil || !strings.Contains(err.Error(), "invalid profile name") {
		t.Fatalf("runChatGPTLogin invalid profile err = %v", err)
	}
}

func TestStartDeploymentRevalidationTransitions(t *testing.T) {
	p := &revalidationStub{
		Stub:  &stub.Stub{NameValue: "stub"},
		live:  []string{"other-model"},
		calls: make(chan struct{}, 8),
	}
	rt := router.New(map[string][]router.Deployment{
		"logical": {
			{Name: "dep", Provider: p, Model: "missing-model", Weight: 1},
		},
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startDeploymentRevalidation(ctx, discardLogger(), rt, time.Millisecond, true)

	waitRevalidationCall(t, p.calls)
	p.setLive([]string{"missing-model"})
	waitRevalidationCall(t, p.calls)
	cancel()
}

type revalidationStub struct {
	*stub.Stub
	mu    sync.Mutex
	live  []string
	calls chan struct{}
}

func (s *revalidationStub) setLive(models []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live = append([]string(nil), models...)
}

func (s *revalidationStub) ListModels(context.Context) ([]string, error) {
	select {
	case s.calls <- struct{}{}:
	default:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.live...), nil
}

// Failure timeouts, not budgets. Every use below sits in a select/poll that
// exits the instant the awaited thing happens, so a generous bound costs a
// passing run nothing and only decides how long a genuinely stuck run waits
// before failing. They were 1-2s, which is not enough headroom on a loaded CI
// runner: runServeContext does LoadConfig + cost.LoadDefault + store.OpenSQLite
// (creating the DB file) + factory.BuildDeployments before it ever listens.
// TestRunServeContextStartsAndStops flaked on exactly that in a previous run.
const (
	waitStartup  = 30 * time.Second
	waitShutdown = 10 * time.Second
	waitSignal   = 10 * time.Second
)

func waitRevalidationCall(t *testing.T, calls <-chan struct{}) {
	t.Helper()
	select {
	case <-calls:
	case <-time.After(waitSignal):
		t.Fatal("timed out waiting for deployment revalidation")
	}
}

func TestRunProfileSetRequiresExistingAuthUnlessForced(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	t.Setenv(auth.ProfileEnvVar, "")

	if err := runProfileSet([]string{"work"}); err == nil {
		t.Fatal("runProfileSet should reject missing auth.json")
	}
	if err := os.MkdirAll(filepath.Join(base, "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "work", "auth.json"), []byte(`{"anthropic":{"type":"oauth"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runProfileSet([]string{"work"}); err != nil {
		t.Fatal(err)
	}
	active, err := auth.ReadActiveProfile()
	if err != nil {
		t.Fatal(err)
	}
	if active != "work" {
		t.Fatalf("active = %q, want work", active)
	}
}

func TestRunProfileSetAcceptsForceAfterName(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	t.Setenv(auth.ProfileEnvVar, "")

	if err := runProfileSet([]string{"work", "--force"}); err != nil {
		t.Fatal(err)
	}
	active, err := auth.ReadActiveProfile()
	if err != nil {
		t.Fatal(err)
	}
	if active != "work" {
		t.Fatalf("active = %q, want work", active)
	}
}

func TestRunProfileRemoveActiveClearsSidecar(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	t.Setenv(auth.ProfileEnvVar, "")
	if err := os.MkdirAll(filepath.Join(base, "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "work", "auth.json"), []byte(`{"anthropic":{"type":"oauth"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := auth.WriteActiveProfile("work"); err != nil {
		t.Fatal(err)
	}

	if err := runProfileRemove([]string{"work", "--force"}); err != nil {
		t.Fatal(err)
	}
	active, err := auth.ReadActiveProfile()
	if err != nil {
		t.Fatal(err)
	}
	if active != "" {
		t.Fatalf("active = %q, want cleared", active)
	}
	if _, err := os.Stat(filepath.Join(base, "work")); !os.IsNotExist(err) {
		t.Fatalf("work profile dir still exists or stat failed with non-missing error: %v", err)
	}
}

func TestRunServeDefaultsConfigPath(t *testing.T) {
	// With no --config, serve falls back to DefaultConfigPath (AGENT_MODEL_CONFIG
	// here). Point it at a missing file and assert serve tried to read that path
	// rather than rejecting the missing flag.
	missing := filepath.Join(t.TempDir(), "absent.yaml")
	t.Setenv("AGENT_MODEL_CONFIG", missing)

	oldArgs := os.Args
	os.Args = []string{"agent-model", "serve"}
	defer func() { os.Args = oldArgs }()

	err := runServe(discardLogger())
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("runServe default-path err = %v, want read error for %q", err, missing)
	}
}

func TestRunServeRequiresBearerToken(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writePurgeConfig(t, cfgPath, filepath.Join(dir, "agentmodel.db"), "")
	t.Setenv("AGENT_MODEL_TOKEN", "")
	t.Setenv("OPENAI_API_KEY", "sk-test")

	oldArgs := os.Args
	os.Args = []string{"agent-model", "serve", "--config", cfgPath}
	defer func() { os.Args = oldArgs }()

	err := runServe(discardLogger())
	if err == nil || !strings.Contains(err.Error(), "AGENT_MODEL_TOKEN is required") {
		t.Fatalf("runServe bearer err = %v", err)
	}
}

func TestRunServeContextStartsAndStops(t *testing.T) {
	dir := t.TempDir()
	port := freeTCPPort(t)
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf(`listen: "127.0.0.1:%d"
db: %s
auth:
  bearer_token_env: AGENT_MODEL_TOKEN
model_list:
  - model_name: gpt
    deployments:
      - provider: openai
        model: gpt-4o
        auth_mode: api_key
        api_key_env: OPENAI_API_KEY
        weight: 100
`, port, filepath.Join(dir, "agentmodel.db"))
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_MODEL_TOKEN", "server-token")
	t.Setenv("OPENAI_API_KEY", "")

	oldArgs := os.Args
	os.Args = []string{"agent-model", "serve", "--config", cfgPath}
	defer func() { os.Args = oldArgs }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runServeContext(ctx, discardLogger())
	}()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.After(waitStartup)
	for {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		select {
		case err := <-done:
			t.Fatalf("runServeContext returned before listening: %v", err)
		case <-deadline:
			cancel()
			t.Fatalf("server did not start listening on %s", addr)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runServeContext returned error: %v", err)
		}
	case <-time.After(waitShutdown):
		t.Fatal("runServeContext did not stop after cancellation")
	}
}

func TestRunMigrateRequiresConfig(t *testing.T) {
	oldArgs := os.Args
	os.Args = []string{"agent-model", "migrate"}
	defer func() { os.Args = oldArgs }()

	err := runMigrate(discardLogger())
	if err == nil || !strings.Contains(err.Error(), "--config") {
		t.Fatalf("runMigrate missing config err = %v", err)
	}
}

func TestRunAnthropicLoginInvalidProfile(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ANTHROPIC_OAUTH_TOKEN_DIR", base)
	t.Setenv(auth.ProfileEnvVar, "")

	oldArgs := os.Args
	os.Args = []string{"agent-model", "anthropic-login", "--profile", "Work"}
	defer func() { os.Args = oldArgs }()

	err := runAnthropicLogin(discardLogger())
	if err == nil || !strings.Contains(err.Error(), "invalid profile name") {
		t.Fatalf("runAnthropicLogin invalid profile err = %v", err)
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
