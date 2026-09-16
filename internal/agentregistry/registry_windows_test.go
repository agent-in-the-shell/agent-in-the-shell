//go:build windows

package agentregistry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// On Windows the registry is best-effort: no cgo-free start-time source, so the
// identity guard is unavailable (StartTimeUnix=0, liveness-only). These pure-Go
// FS/JSON tests verify the cross-platform paths a build-only gate would miss.

func TestRoot_WindowsFallback(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "") // force the UserCacheDir fallback
	root, err := Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Skip("no user cache dir on this host")
	}
	want := filepath.Join(cache, dirName, "agents")
	if root != want {
		t.Errorf("Root() = %q, want %q", root, want)
	}
	if !strings.Contains(root, string(os.PathSeparator)) {
		t.Errorf("Root() not path-joined for Windows: %q", root)
	}
}

func TestRegister_WindowsRoundTripBestEffort(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	e := Entry{
		ID:        "win1",
		PID:       os.Getpid(),
		PGID:      os.Getpid(),
		Command:   "do thing",
		Backend:   "test",
		StartedAt: time.Now().UTC().Truncate(time.Second),
	}
	h, err := Register(e)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	entries, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	got := entries[0]
	if got.ID != "win1" || got.SchemaVersion != entrySchemaVersion || got.Command != "do thing" {
		t.Errorf("round-trip data loss: %+v", got)
	}
	// Degraded guard: no start-time source, so StartTimeUnixNano is 0 and
	// reconcile falls back to liveness-only (a live pid reads running).
	if got.StartTimeUnixNano != 0 {
		t.Errorf("StartTimeUnixNano = %d, want 0 (no guard on Windows)", got.StartTimeUnixNano)
	}
	if got.Status != StatusRunning {
		t.Errorf("best-effort reconcile Status = %q, want running", got.Status)
	}
}
