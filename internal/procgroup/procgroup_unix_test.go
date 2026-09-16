//go:build !windows

package procgroup

import (
	"os/exec"
	"syscall"
	"testing"
)

func TestNewGroupSetsLeader(t *testing.T) {
	cmd := exec.Command("true")
	NewGroup(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatalf("NewGroup: Setpgid not set: %+v", cmd.SysProcAttr)
	}
	if cmd.SysProcAttr.Pgid != 0 {
		t.Errorf("NewGroup: Pgid = %d, want 0 (a fresh group led by the child)", cmd.SysProcAttr.Pgid)
	}
}

func TestJoinGroupSetsPgid(t *testing.T) {
	cmd := exec.Command("true")
	JoinGroup(cmd, 4242)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatalf("JoinGroup: Setpgid not set: %+v", cmd.SysProcAttr)
	}
	if cmd.SysProcAttr.Pgid != 4242 {
		t.Errorf("JoinGroup: Pgid = %d, want 4242 (joins the leader's group)", cmd.SysProcAttr.Pgid)
	}
}

// NewGroup/JoinGroup must preserve any SysProcAttr the caller already set rather
// than clobbering it.
func TestGroupHelpersPreserveExistingAttr(t *testing.T) {
	cmd := exec.Command("true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	NewGroup(cmd)
	if !cmd.SysProcAttr.Setsid {
		t.Error("NewGroup clobbered a pre-existing SysProcAttr field")
	}
	if !cmd.SysProcAttr.Setpgid {
		t.Error("NewGroup did not set Setpgid on the existing SysProcAttr")
	}
}

// EnsureGroup on a live leader makes (or confirms) its own group. Running it
// against this test process's own group id is a benign no-op that must not error.
func TestEnsureGroupSelfIsBenign(t *testing.T) {
	cmd := exec.Command("sleep", "1")
	NewGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	if err := EnsureGroup(cmd.Process.Pid); err != nil {
		t.Errorf("EnsureGroup on a live leader = %v, want nil (EACCES is benign)", err)
	}
}

func TestSignalChecksLiveGroup(t *testing.T) {
	cmd := exec.Command("sleep", "1")
	NewGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	if err := EnsureGroup(cmd.Process.Pid); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if err := Signal(cmd.Process.Pid, 0); err != nil {
		t.Fatalf("Signal live process group with signal 0 = %v, want nil", err)
	}
}

func TestEnsureGroupInvalidPID(t *testing.T) {
	if err := EnsureGroup(-1); err == nil {
		t.Fatal("EnsureGroup invalid pid err = nil, want error")
	}
}
