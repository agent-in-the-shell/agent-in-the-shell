package agentshell

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/internal/agentregistry"
)

// TestSubmitRegistersRunInFlight verifies the producer wiring: while
// an agent run is executing, the daemonless registry lists it (Backend
// "agent-shell", Status running, live pid) — so `agent-shell ps` can see it — and
// the entry is removed once the run returns.
func TestSubmitRegistersRunInFlight(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	// Isolate the registry root to this test so List() sees only our run.
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	// A fake agent that blocks until a release file appears, so the run stays
	// in-flight while we observe the registry.
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	exe := filepath.Join(dir, "fake-agent")
	script := "#!/bin/sh\nwhile [ ! -f " + release + " ]; do sleep 0.02; done\n"
	if err := os.WriteFile(exe, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = Submit(context.Background(), Request{
			Agent:      "claude",
			Prompt:     "ignored",
			Executable: exe,
			Timeout:    10 * time.Second,
		})
	}()
	// Always release so a failed assertion can't leave the goroutine blocked.
	defer func() {
		_ = os.WriteFile(release, nil, 0600)
		wg.Wait()
	}()

	// Poll until the run appears (Register runs right after Start, concurrently
	// with the child coming up — so wait for it rather than racing a single read).
	var got agentregistry.Entry
	waitFor(t, 5*time.Second, "the run to appear in the registry", func() bool {
		es, _ := agentregistry.List()
		if len(es) == 1 && es[0].Status == agentregistry.StatusRunning {
			got = es[0]
			return true
		}
		return false
	})
	if got.Backend != "agent-shell" || got.PID <= 0 {
		t.Fatalf("entry = %#v, want backend agent-shell + live pid", got)
	}

	// Release the agent and wait for Submit to return.
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	// The entry must be gone once the run completes.
	es, err := agentregistry.List()
	if err != nil {
		t.Fatalf("List after run: %v", err)
	}
	if len(es) != 0 {
		t.Fatalf("registry entries after run = %d, want 0: %#v", len(es), es)
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
