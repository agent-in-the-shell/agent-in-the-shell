//go:build darwin

package agentregistry

import (
	"errors"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

// TestReconcile_TransientSysctlError verifies the conservative macOS error
// policy: a transient sysctl failure (not ESRCH) on a *live* pid is treated as
// stale, never as a false running. Self-correcting on the next List once the
// transient clears.
func TestReconcile_TransientSysctlError(t *testing.T) {
	useTempRoot(t)
	cmd := exec.Command("sleep", "3600")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	pid := cmd.Process.Pid

	e := sampleEntry("tsx")
	e.PID, e.PGID = pid, pid
	h, err := Register(e) // fills a real start-time while sysctl works
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	// Inject a transient sysctl error; the pid is still alive.
	orig := sysctlKinfoProc
	t.Cleanup(func() { sysctlKinfoProc = orig })
	sysctlKinfoProc = func(string, ...int) (*unix.KinfoProc, error) {
		return nil, errors.New("transient ENOMEM")
	}

	entries, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Status != StatusStale {
		t.Fatalf("transient sysctl error on a live pid: got %+v, want stale (conservative)", entries)
	}
}
