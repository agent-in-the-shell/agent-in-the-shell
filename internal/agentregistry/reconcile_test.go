//go:build linux || darwin

package agentregistry

import (
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// startSleeper forks `sleep 3600` (far beyond any List-within-seconds window, so
// it cannot self-exit and flake the "running" assertion) and returns its pid and
// a cleanup that kills+reaps it.
func startSleeper(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "3600")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd.Process.Pid
}

func TestReconcile_LiveThenKilled(t *testing.T) {
	useTempRoot(t)
	cmd := exec.Command("sleep", "3600")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	pid := cmd.Process.Pid

	e := sampleEntry("rec1")
	e.PID, e.PGID = pid, pid
	h, err := Register(e)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	// Live => running, with the identity guard active (start-time recorded).
	entries, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Status != StatusRunning {
		t.Fatalf("live sleeper: got %+v, want one running", entries)
	}
	if entries[0].StartTimeUnixNano == 0 {
		t.Error("expected a recorded start-time for the live pid")
	}

	// Kill + reap, then confirm the pid is gone before asserting stale.
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	for i := 0; i < 200 && processAlive(pid); i++ {
		time.Sleep(5 * time.Millisecond)
	}
	entries, err = List()
	if err != nil {
		t.Fatalf("List after kill: %v", err)
	}
	if len(entries) != 1 || entries[0].Status != StatusStale {
		t.Fatalf("dead sleeper: got %+v, want one stale", entries)
	}
}

// TestReconcile_PIDReuseMismatch proves the guard rejects a recycled pid (alive,
// but a different start-time), not merely a dead one.
func TestReconcile_PIDReuseMismatch(t *testing.T) {
	root := useTempRoot(t)
	pid := startSleeper(t) // still alive

	e := sampleEntry("reuse1")
	e.PID, e.PGID = pid, pid
	h, err := Register(e)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	// Hand-rewrite meta.json with a different start-time (the pid is still alive).
	// reconcile matches start-time exactly, so any divergence is a mismatch; we
	// use a full second (1e9 ns) to make the intent obvious and to model a pid
	// recycled at a clearly different time. This proves the guard rejects on
	// start-time divergence, not merely on liveness.
	meta := filepath.Join(root, "reuse1", metaFile)
	data, _ := os.ReadFile(meta)
	var on Entry
	if err := json.Unmarshal(data, &on); err != nil {
		t.Fatal(err)
	}
	on.StartTimeUnixNano -= nanosPerSec
	rewritten, _ := json.Marshal(on)
	if err := os.WriteFile(meta, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}

	entries, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Status != StatusStale {
		t.Fatalf("recycled pid (alive, mismatched start-time): got %+v, want stale", entries)
	}
}

// TestGuardAllowsAction_CircuitBreaker proves the guard is not just a correct
// verdict but a working circuit breaker: only a freshly-reconciled running entry
// clears it; a recycled or dead pid blocks it. This is the contract
// `agent-shell kill` calls after re-reconciling.
func TestGuardAllowsAction_CircuitBreaker(t *testing.T) {
	root := useTempRoot(t)
	cmd := exec.Command("sleep", "3600")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	pid := cmd.Process.Pid
	e := sampleEntry("cb1")
	e.PID, e.PGID = pid, pid
	h, err := Register(e)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	// (1) live + matching => allowed.
	entries, _ := List()
	if len(entries) != 1 || !guardAllowsAction(entries[0]) {
		t.Fatalf("live running entry should clear the guard, got %+v", entries)
	}

	// (2) recycled pid (alive, mismatched start-time) => blocked.
	meta := filepath.Join(root, "cb1", metaFile)
	data, _ := os.ReadFile(meta)
	var on Entry
	_ = json.Unmarshal(data, &on)
	on.StartTimeUnixNano -= nanosPerSec
	rewritten, _ := json.Marshal(on)
	_ = os.WriteFile(meta, rewritten, 0o600)
	entries, _ = List()
	if len(entries) != 1 || guardAllowsAction(entries[0]) {
		t.Fatalf("recycled pid must block the guard, got %+v", entries)
	}

	// (3) dead pid => blocked.
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	for i := 0; i < 200 && processAlive(pid); i++ {
		time.Sleep(5 * time.Millisecond)
	}
	entries, _ = List()
	if len(entries) != 1 || guardAllowsAction(entries[0]) {
		t.Fatalf("dead pid must block the guard, got %+v", entries)
	}
}

// TestProcessStartTime_Bounds checks the reader returns a plausible wall-clock
// time in nanoseconds. A *freshly forked* process must read within ±2s of now
// (it just started); ±2s absorbs scheduling jitter while still catching a
// grossly-wrong parse. The current (test) process started some seconds ago, so
// it is only sanity-checked (not in the future, not absurdly old). A nonexistent
// pid must error, not panic.
func TestProcessStartTime_Bounds(t *testing.T) {
	now := time.Now().UnixNano()

	pid := startSleeper(t) // just forked => start-time ~ now
	got, err := processStartTime(pid)
	if err != nil {
		t.Fatalf("processStartTime(fresh): %v", err)
	}
	if delta := math.Abs(float64(time.Now().UnixNano() - got)); delta > 2*nanosPerSec {
		t.Errorf("fresh process start-time %d is %.1fs from now; want within 2s", got, delta/nanosPerSec)
	}

	self, err := processStartTime(os.Getpid())
	if err != nil {
		t.Fatalf("processStartTime(self): %v", err)
	}
	if self <= 0 || self > now+nanosPerSec || self < now-3600*nanosPerSec {
		t.Errorf("self start-time %d implausible (now=%d)", self, now)
	}

	if _, err := processStartTime(deadPID(t)); err == nil {
		// A reaped pid: the reader should error (not panic). On Linux the /proc
		// dir is gone; on macOS sysctl returns ESRCH.
		t.Error("expected an error reading the start-time of a dead pid")
	}
}
