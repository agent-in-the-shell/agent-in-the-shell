//go:build !windows

package shellcli_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/internal/shellcli"
)

// TestShellBoundary_SignalDeathExitCode is the load-bearing proof that the
// exit-code honesty fix reaches the outermost process boundary: a signal-killed
// agent must make `agent-shell` itself exit 143 (128+SIGTERM), not 1 or 255.
// shellcli.Run calls os.Exit(result.ExitCode), so this is only observable by
// reading a real process's exit status.
//
// It uses the standard Go helper-process pattern (re-exec this already-compiled
// test binary) rather than `go build`, so it adds no compile load to the suite.
func TestShellBoundary_SignalDeathExitCode(t *testing.T) {
	// Helper branch: when re-executed with SHELLCLI_BOUNDARY_STUB set, act as the
	// agent-shell CLI. shellcli.Run calls os.Exit, ending this child process with
	// the honest code.
	if stub := os.Getenv("SHELLCLI_BOUNDARY_STUB"); stub != "" {
		shellcli.Run([]string{"--agent", "claude", "--exec", stub, "do a thing"})
		return // unreachable: Run os.Exits on a nonzero result
	}

	// A stub "agent" that self-sends SIGTERM — deterministic, no out-of-band race.
	stub := filepath.Join(t.TempDir(), "fake-agent")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nkill -TERM $$\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	child := exec.Command(os.Args[0], "-test.run=^TestShellBoundary_SignalDeathExitCode$")
	child.Env = append(os.Environ(), "SHELLCLI_BOUNDARY_STUB="+stub)
	err := child.Run()

	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected the agent-shell boundary to exit nonzero via ExitError, got err=%v", err)
	}
	if ee.ExitCode() != 143 {
		t.Errorf("agent-shell exit code = %d, want 143 (128+SIGTERM); the honesty fix did not reach the os.Exit boundary", ee.ExitCode())
	}
}
