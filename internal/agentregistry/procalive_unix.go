//go:build linux || darwin

package agentregistry

import (
	"errors"
	"os"
	"syscall"
)

// processAlive reports whether pid currently exists, using signal 0 (which
// performs error checking without delivering a signal). os.FindProcess always
// succeeds on Unix, so the existence probe is the Signal call. EPERM means the
// process exists but is not signalable by us — still alive.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// groupAlive reports whether the process group led by pgid still has any member,
// via kill(-pgid, 0): nil => at least one member alive, ESRCH => empty. EPERM
// means a member exists but is not signalable by us — still alive (treating it
// as dead would make the kill ladder skip SIGKILL and leave the group running).
// This is the ladder's escalation probe — it also catches a child that traps
// SIGTERM and outlives the leader, which a leader-only check would miss.
func groupAlive(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	err := syscall.Kill(-pgid, syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
