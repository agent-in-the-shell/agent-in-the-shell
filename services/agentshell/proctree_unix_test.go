//go:build unix

package agentshell

import (
	"os/exec"
	"testing"
)

func TestSetupProcessGroupUnix(t *testing.T) {
	cmd := exec.Command("true")
	setupProcessGroup(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("expected Setpgid=true so the child leads its own process group")
	}
	if cmd.Cancel == nil {
		t.Fatal("expected a group-kill Cancel func")
	}
	if cmd.WaitDelay == 0 {
		t.Error("expected a non-zero WaitDelay grace window")
	}
}
