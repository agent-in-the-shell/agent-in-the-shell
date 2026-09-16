//go:build windows

// Package procgroup signals a whole process group with one syscall. Windows has
// no POSIX process groups or signals, so this is a best-effort, terminate-only
// fallback: only SIGKILL/SIGTERM are honored (as TerminateProcess on the leader),
// and any other signal is rejected. Validating which signals are supported is
// therefore done here, in the platform layer, so callers stay platform-agnostic.
package procgroup

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// NewGroup / JoinGroup are no-ops on Windows (no POSIX process groups). Plugins
// and stages run in the default group; group reaping degrades to best-effort
// per-leader termination via Signal.
func NewGroup(_ *exec.Cmd)         {}
func JoinGroup(_ *exec.Cmd, _ int) {}

// EnsureGroup is a no-op on Windows (no POSIX process groups).
func EnsureGroup(_ int) error { return nil }

// Signal best-effort terminates the process identified by pid (Windows has no
// group semantics). Only SIGKILL/SIGTERM are supported; any other signal returns
// an error without touching the process.
func Signal(pid int, sig syscall.Signal) error {
	if sig != syscall.SIGKILL && sig != syscall.SIGTERM {
		return fmt.Errorf("procgroup: signal %v unsupported on Windows (terminate-only)", sig)
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
