//go:build !windows

package agentregistry

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// useTempRoot points Root() at a fresh temp dir via XDG_RUNTIME_DIR (path 1 of
// Root), exercising the real resolver. t.Setenv forbids t.Parallel, which is
// fine — these tests are cheap and sequential. Shared by the !windows test files.
func useTempRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	root, err := Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	return root
}

func sampleEntry(id string) Entry {
	return Entry{
		ID:        id,
		TaskID:    "task-" + id,
		PID:       os.Getpid(),
		PGID:      os.Getpid(),
		Command:   "echo hi",
		Backend:   "test",
		StartedAt: time.Now().UTC().Truncate(time.Second),
	}
}

// deadPID returns a pid that is guaranteed dead: a trivial child run to
// completion and reaped. PID reuse within the test window is implausible.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn trivial proc: %v", err)
	}
	return cmd.Process.Pid
}

func TestRegister_RoundTrip(t *testing.T) {
	root := useTempRoot(t)
	h, err := Register(sampleEntry("rt1"))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	dir := filepath.Join(root, "rt1")
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("entry dir: stat=%v isDir=%v", err, fi != nil && fi.IsDir())
	}
	meta := filepath.Join(dir, metaFile)
	fi, err := os.Stat(meta)
	if err != nil {
		t.Fatalf("meta.json: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("meta.json perm = %o, want 0600", fi.Mode().Perm())
	}
	if _, err := os.Stat(meta + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp left behind: err=%v", err)
	}

	data, _ := os.ReadFile(meta)
	var got Entry
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.SchemaVersion != entrySchemaVersion || got.ID != "rt1" || got.PID != os.Getpid() {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.StartTimeUnixNano == 0 { // filled by Register from the live pid on unix
		t.Error("StartTimeUnixNano not filled for a live pid")
	}
}

func TestRegister_MkdirAllFailureAborts(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Root under a regular file => MkdirAll(<root>/<id>) fails (ENOTDIR).
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(file, "under-a-file"))
	if _, err := Register(sampleEntry("mk1")); err == nil {
		t.Fatal("expected Register to fail when the entry dir cannot be created")
	}
}

func TestRegister_RenameFailureCleansTmp(t *testing.T) {
	root := useTempRoot(t)
	dir := filepath.Join(root, "rn1")
	// Pre-create meta.json as a non-empty directory so os.Rename(tmp, meta) fails.
	if err := os.MkdirAll(filepath.Join(dir, metaFile, "block"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Register(sampleEntry("rn1")); err == nil {
		t.Fatal("expected Register to fail when meta.json cannot be renamed into place")
	}
	if _, err := os.Stat(filepath.Join(dir, metaFile+".tmp")); !os.IsNotExist(err) {
		t.Errorf(".tmp not cleaned after rename failure: err=%v", err)
	}
}

func TestRegister_IncompleteEntryTolerated(t *testing.T) {
	useTempRoot(t)
	// A dead pid yields no start-time; the entry must still be written, with
	// StartTimeUnix=0, and reconcile must treat it liveness-only (dead => stale).
	e := sampleEntry("inc1")
	e.PID = deadPID(t)
	e.PGID = e.PID
	h, err := Register(e)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	entries, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].StartTimeUnixNano != 0 {
		t.Fatalf("want one entry with StartTimeUnixNano=0, got %+v", entries)
	}
	if entries[0].Status != StatusStale {
		t.Errorf("dead pid: Status=%q, want stale", entries[0].Status)
	}
}

func TestHandle_CloseIdempotent(t *testing.T) {
	root := useTempRoot(t)
	h, err := Register(sampleEntry("cl1"))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "cl1")); !os.IsNotExist(err) {
		t.Errorf("dir not removed: %v", err)
	}
	if err := h.Close(); err != nil { // second Close is a no-op
		t.Errorf("second Close: %v", err)
	}
	if err := (*Handle)(nil).Close(); err != nil {
		t.Errorf("nil Close: %v", err)
	}
}

func TestList_SkipsBadEntries(t *testing.T) {
	root := useTempRoot(t)
	h, err := Register(sampleEntry("good"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	if err := os.MkdirAll(filepath.Join(root, "nometa"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "garbage"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "garbage", metaFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(root, "stray.tmp"), []byte("x"), 0o600)

	entries, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != "good" {
		t.Fatalf("want only the good entry, got %+v", entries)
	}
}

func TestList_MissingRootIsEmpty(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	entries, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("want no entries, got %+v", entries)
	}
}

// TestList_VanishingDirRace uses the beforeReadEntry seam to delete an entry
// deterministically after ReadDir saw it but before its ReadFile, proving List
// tolerates a mid-read vanish (skip, no panic, no error).
func TestList_VanishingDirRace(t *testing.T) {
	root := useTempRoot(t)
	hStable, err := Register(sampleEntry("stable"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hStable.Close() })
	if _, err := Register(sampleEntry("doomed")); err != nil {
		t.Fatal(err)
	}

	beforeReadEntry = func(name string) {
		if name == "doomed" {
			_ = os.RemoveAll(filepath.Join(root, "doomed"))
		}
	}
	t.Cleanup(func() { beforeReadEntry = nil })

	entries, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, e := range entries {
		if e.ID == "doomed" {
			t.Fatalf("doomed entry should have been skipped, got %+v", entries)
		}
	}
}

// TestConcurrentRegisterAndList is the explicit source of concurrent same-root
// access that makes `go test -race` meaningful: N registrars land mid-List while
// readers loop. Assertions: no panic, no half-decoded entry, and the final List
// contains exactly the still-open entries.
func TestConcurrentRegisterAndList(t *testing.T) {
	useTempRoot(t)
	const n = 10
	var wg sync.WaitGroup
	handles := make([]*Handle, n)

	stop := make(chan struct{})
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				got, err := List()
				if err != nil {
					t.Errorf("List during concurrency: %v", err)
					return
				}
				for _, e := range got {
					if e.ID == "" || e.SchemaVersion == 0 {
						t.Errorf("half-decoded entry observed: %+v", e)
						return
					}
				}
			}
		}()
	}

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			time.Sleep(time.Duration(i) * time.Millisecond) // stagger so writes land mid-List
			h, err := Register(sampleEntry(fmt.Sprintf("c%02d", i)))
			if err != nil {
				t.Errorf("Register c%02d: %v", i, err)
				return
			}
			handles[i] = h
		}(i)
	}

	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()

	open := map[string]bool{}
	for i, h := range handles {
		if h == nil {
			continue
		}
		if i%2 == 0 {
			if err := h.Close(); err != nil {
				t.Errorf("Close c%02d: %v", i, err)
			}
		} else {
			open[fmt.Sprintf("c%02d", i)] = true
			t.Cleanup(func() { _ = h.Close() })
		}
	}
	final, err := List()
	if err != nil {
		t.Fatalf("final List: %v", err)
	}
	got := map[string]bool{}
	for _, e := range final {
		got[e.ID] = true
	}
	if len(got) != len(open) {
		t.Fatalf("final List = %v, want exactly the open set %v", got, open)
	}
	for id := range open {
		if !got[id] {
			t.Errorf("open entry %s missing from final List", id)
		}
	}
}
