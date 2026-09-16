//go:build !unix

package agentshell

import "os/exec"

// setupProcessGroup is a no-op on non-unix platforms; exec.CommandContext's
// default direct-child cancellation applies.
func setupProcessGroup(cmd *exec.Cmd) {}
