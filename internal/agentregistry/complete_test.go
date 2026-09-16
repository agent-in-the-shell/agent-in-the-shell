//go:build !windows

package agentregistry

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sampleCompletion(id string) CompletionRecord {
	now := time.Now().UTC().Truncate(time.Second)
	return CompletionRecord{
		ID:         id,
		Command:    "echo hi",
		Backend:    "test",
		StartedAt:  now,
		FinishedAt: now,
		ExitCode:   0,
		Status:     "ok",
	}
}

func TestRecordCompletion_RoundTrip(t *testing.T) {
	root := useTempRoot(t) // exercises Root's resolver; CompletedRoot shares it
	_ = root

	rec := sampleCompletion("comp1")
	rec.ParentID = "parent1"
	rec.Stdout = "hello\n"
	rec.Stderr = "warn\n"
	if err := RecordCompletion(rec); err != nil {
		t.Fatalf("RecordCompletion: %v", err)
	}

	got, ok, err := ReadCompletion("comp1")
	if err != nil {
		t.Fatalf("ReadCompletion: %v", err)
	}
	if !ok {
		t.Fatal("ReadCompletion: ok=false, want true")
	}
	if got.ID != rec.ID || got.ParentID != rec.ParentID || got.Stdout != rec.Stdout || got.Stderr != rec.Stderr {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, rec)
	}
}

func TestReadCompletion_UnknownIDNotAnError(t *testing.T) {
	useTempRoot(t)
	_, ok, err := ReadCompletion("nope")
	if err != nil {
		t.Fatalf("ReadCompletion: %v", err)
	}
	if ok {
		t.Error("ok=true for an unknown id, want false")
	}
}

func TestReadCompletion_MissingRootIsNotFound(t *testing.T) {
	// CompletedRoot resolves fine (XDG_RUNTIME_DIR set), but no completion has
	// ever been recorded, so the directory itself may not exist yet.
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	_, ok, err := ReadCompletion("nope")
	if err != nil {
		t.Fatalf("ReadCompletion: %v", err)
	}
	if ok {
		t.Error("ok=true with no completed root, want false")
	}
}

func TestTailBytes(t *testing.T) {
	if got := TailBytes("short", 100); got != "short" {
		t.Errorf("short string truncated: %q", got)
	}
	long := "0123456789"
	got := TailBytes(long, 4)
	want := "[output truncated: showing last 4 bytes]\n6789"
	if got != want {
		t.Errorf("TailBytes = %q, want %q", got, want)
	}
}

func TestRecordCompletion_PrunesOld(t *testing.T) {
	root := useTempRoot(t)
	compRoot, err := CompletedRoot()
	if err != nil {
		t.Fatal(err)
	}
	_ = root

	if err := RecordCompletion(sampleCompletion("old")); err != nil {
		t.Fatal(err)
	}
	// Backdate the "old" record's mtime past the retention window.
	oldFile := filepath.Join(compRoot, "old.json")
	past := time.Now().Add(-completionRetention - time.Hour)
	if err := os.Chtimes(oldFile, past, past); err != nil {
		t.Fatal(err)
	}

	// A second RecordCompletion should opportunistically prune "old".
	if err := RecordCompletion(sampleCompletion("new")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Errorf("old completion not pruned: stat err=%v", err)
	}
	if _, ok, err := ReadCompletion("new"); err != nil || !ok {
		t.Errorf("new completion missing: ok=%v err=%v", ok, err)
	}
}
