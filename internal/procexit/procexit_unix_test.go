//go:build !windows

package procexit_test

import (
	"os/exec"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/internal/procexit"
)

// runErr runs `sh -c script` and returns the error from cmd.Run() so the test
// feeds real *exec.ExitError values (including genuine signal deaths) through
// Decode. The signal rows are deterministic: the child raises the signal on
// *itself*, so there is no out-of-band startup/signal race. Only the nil and
// non-ExitError rows avoid a subprocess.
func runErr(t *testing.T, script string) error {
	t.Helper()
	return exec.Command("sh", "-c", script).Run()
}

func TestDecode_Unix(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		exitCode int
		signal   int
		signaled bool
		known    bool
	}{
		{"nil", nil, 0, 0, false, true},
		{"exit0", runErr(t, "exit 0"), 0, 0, false, true},
		{"exit65", runErr(t, "exit 65"), 65, 0, false, true},
		{"exit75", runErr(t, "exit 75"), 75, 0, false, true},
		{"sigterm", runErr(t, "kill -TERM $$"), 143, 15, true, true},
		{"sigkill", runErr(t, "kill -KILL $$"), 137, 9, true, true},
		{"sigpipe", runErr(t, "kill -PIPE $$"), 141, 13, true, true},
		// A command name that cannot be resolved yields an *exec.Error, not an
		// *exec.ExitError: the process never produced an exit status, so the
		// code is an unknown placeholder.
		{"startfail", exec.Command("agentshell-nonexistent-binary-xyzzy-12345").Run(), 1, 0, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := procexit.Decode(tt.err)
			if got.ExitCode != tt.exitCode || got.Signal != tt.signal ||
				got.Signaled != tt.signaled || got.Known != tt.known {
				t.Errorf("Decode(%s) = %+v; want {ExitCode:%d Signal:%d Signaled:%v Known:%v}",
					tt.name, got, tt.exitCode, tt.signal, tt.signaled, tt.known)
			}
		})
	}
}
