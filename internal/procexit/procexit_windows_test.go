//go:build windows

package procexit_test

import (
	"os/exec"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/internal/procexit"
)

// On Windows there is no POSIX signal-death concept: signalOf is a no-op, so a
// nonzero exit always flows through the normal-ExitError branch and Signaled is
// never true. Signal-death testing is intentionally absent (see plan §5.1).
func TestDecode_Windows(t *testing.T) {
	if err := exec.Command("cmd", "/c", "exit 0").Run(); err != nil {
		t.Fatalf("exit 0 unexpectedly errored: %v", err)
	}

	// nil -> clean known zero.
	if got := procexit.Decode(nil); got != (procexit.Decoded{ExitCode: 0, Known: true}) {
		t.Errorf("Decode(nil) = %+v; want {0,0,false,true}", got)
	}

	// Normal nonzero exit -> normal-ExitError branch (never Signaled).
	nonzero := exec.Command("cmd", "/c", "exit 65").Run()
	if got := procexit.Decode(nonzero); got.ExitCode != 65 || got.Signaled || got.Signal != 0 || !got.Known {
		t.Errorf("Decode(exit 65) = %+v; want {65,0,false,true}", got)
	}

	// Unresolvable command -> unknown placeholder.
	startfail := exec.Command("agentshell-nonexistent-binary-xyzzy-12345").Run()
	if got := procexit.Decode(startfail); got.ExitCode != 1 || got.Signaled || got.Known {
		t.Errorf("Decode(startfail) = %+v; want {1,0,false,false}", got)
	}
}
