//go:build linux || darwin

package agentregistry

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func mustUnmarshalEntry(t *testing.T, data []byte) Entry {
	t.Helper()
	var e Entry
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatalf("unmarshal entry: %v", err)
	}
	return e
}

func writeJSON(t *testing.T, path string, e Entry) {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// startGroupLeader forks `sh -c command` as its own process-group leader (so
// pgid == pid) and reaps it the moment it dies (a background Wait) — without
// that, a killed-but-unreaped leader lingers as a zombie that kill(0) still sees
// as alive, which would make groupAlive() never go false. Returns the leader pid.
func startGroupLeader(t *testing.T, command string) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start group leader: %v", err)
	}
	go func() { _ = cmd.Wait() }()
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	return pid
}

func registerLeader(t *testing.T, id string, pid int, command string) {
	t.Helper()
	h, err := Register(Entry{
		ID: id, PID: pid, PGID: pid, Command: command,
		Backend: "test", StartedAt: time.Now().UTC().Truncate(time.Second),
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
}

// settleDead polls until pid is gone (bounded ~1s) so assertions don't race the
// kernel reaping the process after SIGKILL.
func settleDead(pid int) bool {
	for i := 0; i < 200; i++ {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return !processAlive(pid)
}

func TestKill_ReapsRunningAgent(t *testing.T) {
	root := useTempRoot(t)
	pid := startGroupLeader(t, "sleep 3600")
	registerLeader(t, "k1", pid, "sleep 3600")

	if err := Kill("k1", 200*time.Millisecond); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if !settleDead(pid) {
		t.Errorf("leader pid %d still alive after Kill", pid)
	}
	if _, err := os.Stat(filepath.Join(root, "k1")); !os.IsNotExist(err) {
		t.Errorf("entry dir not removed after Kill: err=%v", err)
	}
}

// TestKill_NoOrphans is the epic conformance test: Kill must reap the WHOLE
// group (leader + children), leaving no orphan.
func TestKill_NoOrphans(t *testing.T) {
	useTempRoot(t)
	childFile := filepath.Join(t.TempDir(), "child.pid")
	// sh (leader) backgrounds a sleep child, records its pid, then waits.
	pid := startGroupLeader(t, "sleep 3600 & echo $! > "+childFile+"; wait")

	// Wait for the child pid to be recorded.
	var childPID int
	for i := 0; i < 200 && childPID == 0; i++ {
		if b, err := os.ReadFile(childFile); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				childPID = n
			}
		}
		if childPID == 0 {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if childPID == 0 {
		t.Fatal("child pid never recorded")
	}
	if !processAlive(childPID) {
		t.Fatalf("child %d not alive before Kill", childPID)
	}
	registerLeader(t, "k2", pid, "sleep+child")

	if err := Kill("k2", 200*time.Millisecond); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if !settleDead(pid) {
		t.Errorf("leader %d survived (orphaned)", pid)
	}
	if !settleDead(childPID) {
		t.Errorf("child %d survived (orphaned) — group kill missed it", childPID)
	}
}

// TestKill_RefusesStale proves Kill refuses a stale/reused entry and — the key
// assertion — does NOT signal it. A SIGTERM-trap sentinel file gives positive
// proof no signal was delivered (vs merely inferring it from the pid surviving).
func TestKill_RefusesStale(t *testing.T) {
	root := useTempRoot(t)
	sentinel := filepath.Join(t.TempDir(), "got-term")
	pid := startGroupLeader(t, `trap 'touch `+sentinel+`' TERM; sleep 3600`)
	registerLeader(t, "k3", pid, "trap-term")

	// Simulate PID reuse: rewrite meta.json with a mismatched start-time (pid
	// still alive). reconcile must judge it stale.
	meta := filepath.Join(root, "k3", metaFile)
	data, _ := os.ReadFile(meta)
	on := mustUnmarshalEntry(t, data)
	on.StartTimeUnixNano -= nanosPerSec
	writeJSON(t, meta, on)

	err := Kill("k3", 200*time.Millisecond)
	if err == nil {
		t.Fatal("Kill should refuse a stale/reused entry")
	}
	// Positive proof no SIGTERM was sent: the trap sentinel must not exist. Give a
	// brief window so a (buggy) signal would have had time to fire the trap.
	time.Sleep(50 * time.Millisecond)
	if _, statErr := os.Stat(sentinel); statErr == nil {
		t.Error("Kill signalled a stale entry — SIGTERM trap fired (recycled-pid safety violated)")
	}
	if !processAlive(pid) {
		t.Error("the original (still-valid) process was killed despite the refusal")
	}
}

// TestKill_SurvivorNotDeregistered proves the post-reap liveness re-check: if a
// group member outlives the bounded SIGKILL reap window (e.g. stuck in
// uninterruptible D-state), Kill must NOT report success and must NOT remove the
// entry — the still-running process has to stay visible and re-killable. We force
// the survivor condition by stubbing the groupAliveFn seam to always report alive
// (a real D-state survivor cannot be produced deterministically).
func TestKill_SurvivorNotDeregistered(t *testing.T) {
	root := useTempRoot(t)
	// A real, killable process so SIGTERM/SIGKILL succeed; only the liveness
	// *observation* is stubbed to simulate a member the reap window can't see die.
	pid := startGroupLeader(t, "sleep 3600")
	registerLeader(t, "k5", pid, "sleep 3600")

	orig := groupAliveFn
	groupAliveFn = func(int) bool { return true } // never goes false
	t.Cleanup(func() { groupAliveFn = orig })

	err := Kill("k5", 50*time.Millisecond)
	if err == nil {
		t.Fatal("Kill should error when the group is still alive after the reap window")
	}
	// The entry must remain so the survivor stays visible and re-killable.
	if _, statErr := os.Stat(filepath.Join(root, "k5")); statErr != nil {
		t.Errorf("entry dir removed despite a live survivor: %v", statErr)
	}
}

func TestKill_UnknownID(t *testing.T) {
	useTempRoot(t)
	if err := Kill("nope", 100*time.Millisecond); err == nil {
		t.Error("Kill of unknown id should error")
	}
}

// TestKill_EscalatesToKill: a group that ignores SIGTERM is escalated to SIGKILL
// after the grace period.
func TestKill_EscalatesToKill(t *testing.T) {
	useTempRoot(t)
	pid := startGroupLeader(t, `trap '' TERM; while :; do :; done`)
	registerLeader(t, "k4", pid, "ignores-term")

	start := time.Now()
	if err := Kill("k4", 200*time.Millisecond); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if !settleDead(pid) {
		t.Errorf("TERM-ignoring leader %d survived; escalation failed", pid)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Kill took %v; want roughly the 200ms grace + reap", elapsed)
	}
}

func TestSignal_DeliversAndGuards(t *testing.T) {
	root := useTempRoot(t)
	// Delivery: a plain sleeper dies on SIGTERM.
	pid := startGroupLeader(t, "sleep 3600")
	registerLeader(t, "s1", pid, "sleep")
	if err := Signal("s1", syscall.SIGTERM); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	if !settleDead(pid) {
		t.Errorf("Signal(TERM) did not reach pid %d", pid)
	}

	// Guard: a stale entry is refused without signalling.
	sentinel := filepath.Join(t.TempDir(), "sig-term")
	pid2 := startGroupLeader(t, `trap 'touch `+sentinel+`' TERM; sleep 3600`)
	registerLeader(t, "s2", pid2, "trap-term")
	meta := filepath.Join(root, "s2", metaFile)
	data, _ := os.ReadFile(meta)
	on := mustUnmarshalEntry(t, data)
	on.StartTimeUnixNano -= nanosPerSec
	writeJSON(t, meta, on)
	if err := Signal("s2", syscall.SIGTERM); err == nil {
		t.Error("Signal should refuse a stale entry")
	}
	time.Sleep(50 * time.Millisecond)
	if _, statErr := os.Stat(sentinel); statErr == nil {
		t.Error("Signal delivered to a stale entry (trap fired)")
	}

	if err := Signal("missing", syscall.SIGTERM); err == nil {
		t.Error("Signal of unknown id should error")
	}
}

func TestParseSignal(t *testing.T) {
	cases := map[string]syscall.Signal{
		"TERM":    syscall.SIGTERM,
		"SIGTERM": syscall.SIGTERM,
		"term":    syscall.SIGTERM,
		"15":      syscall.SIGTERM,
		"HUP":     syscall.SIGHUP,
		"KILL":    syscall.SIGKILL,
	}
	for in, want := range cases {
		got, err := parseSignal(in)
		if err != nil || got != want {
			t.Errorf("parseSignal(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseSignal("NOTASIGNAL"); err == nil {
		t.Error("parseSignal of garbage should error")
	}
}
