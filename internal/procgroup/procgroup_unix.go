//go:build !windows

// Package procgroup signals a whole process group with one syscall. It is the
// single shared atom between the agentsched runner (which kills the child group
// it owns) and the agent registry (which kills a group by its recorded pgid) —
// the two callers' control flows differ, but the group-signal primitive is the
// same.
package procgroup

import (
	"os/exec"
	"syscall"
)

// Signal sends sig to the entire process group led by pgid: kill(-pgid, sig).
// A pgid that no longer has any member yields the underlying ESRCH error.
func Signal(pgid int, sig syscall.Signal) error {
	return syscall.Kill(-pgid, sig)
}

// sysProcAttr returns cmd's SysProcAttr, allocating it if unset — so the group
// helpers configure process-group placement without clobbering other attrs the
// caller may have already set.
func sysProcAttr(cmd *exec.Cmd) *syscall.SysProcAttr {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	return cmd.SysProcAttr
}

// NewGroup makes cmd start as the leader of a fresh process group (Setpgid), so
// its pgid == its pid and a group signal reaps it and all its descendants. Call
// before cmd.Start.
func NewGroup(cmd *exec.Cmd) {
	sysProcAttr(cmd).Setpgid = true
}

// JoinGroup makes cmd start in the existing process group led by pgid, so a
// signal to that group also reaps cmd. Call before cmd.Start.
func JoinGroup(cmd *exec.Cmd, pgid int) {
	a := sysProcAttr(cmd)
	a.Setpgid = true
	a.Pgid = pgid
}

// EnsureGroup confirms, from the parent, that pid leads its own process group.
// NewGroup arranges this child-side (setpgid after fork), but the parent cannot
// know that call has landed; doing it here too closes the fork/exec window in
// which a sibling JoinGroup could try to enter the group before the leader's own
// setpgid completes (ESRCH). Call after the leader's Start and before starting
// any joiner. EACCES — the child has already exec'd and set it itself — is the
// expected benign outcome and reported as success.
func EnsureGroup(pid int) error {
	if err := syscall.Setpgid(pid, pid); err != nil && err != syscall.EACCES {
		return err
	}
	return nil
}
