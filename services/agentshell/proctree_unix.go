//go:build unix

package agentshell

import (
	"os/exec"
	"syscall"
	"time"
)

// setupProcessGroup makes the spawned agent a process-group leader and, on
// context cancellation, kills the WHOLE group (negative PID) rather than just
// the direct child. A subagent CLI that itself forks children (the common case)
// would otherwise orphan them, since exec.CommandContext's default cancel only
// SIGKILLs the immediate process. WaitDelay bounds the grace window before the
// runtime force-closes the pipes.
func setupProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative PID targets the process group led by the child.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second
}
