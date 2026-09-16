//go:build !windows

package procexit

import (
	"os/exec"
	"syscall"
)

// signalOf returns the positive signal number that killed the process, if any.
// syscall.WaitStatus, Signaled() and Signal() are present on both Linux and
// macOS, so this file compiles and behaves identically on both.
func signalOf(ee *exec.ExitError) (int, bool) {
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return 0, false
	}
	return int(ws.Signal()), true
}
