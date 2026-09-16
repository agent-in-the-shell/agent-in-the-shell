//go:build windows

package agentregistry

import (
	"os"
	"syscall"
	"testing"
	"time"
)

// On Windows, signalling is terminate-only: agent-shell signal with a non-terminating
// signal must be rejected (error) WITHOUT touching the process. This is verified
// by the rejection path only — we never deliver a terminating signal to the test
// process. Compile-checked via `GOOS=windows go vet`; runs under a real Windows
// `go test`.
func TestSignal_WindowsRejectsNonTerminating(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	e := Entry{
		ID: "w-sig", PID: os.Getpid(), PGID: os.Getpid(),
		Command: "self", Backend: "test", StartedAt: time.Now(),
	}
	h, err := Register(e)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	// The entry reconciles to running (best-effort liveness), so the guard clears
	// and we reach procgroup, which rejects a non-terminating signal on Windows.
	if err := Signal("w-sig", syscall.SIGHUP); err == nil {
		t.Error("Signal(SIGHUP) should be rejected as unsupported on Windows")
	}
	// We are obviously still running, so nothing was terminated.
}

func TestKill_WindowsMissingID(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	if err := Kill("nope", 100*time.Millisecond); err == nil {
		t.Error("Kill of unknown id should error on Windows too")
	}
}
