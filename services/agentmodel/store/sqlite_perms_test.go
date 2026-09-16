package store

import (
	"os"
	"path/filepath"
	"testing"
)

// Narrower exposure than the response cache — request_logs holds no message
// content and api_keys only hashes — but it is org ids, model usage and spend
// history, and SQLite would leave it 0644.
//
// EnsureParent's 0700 is not a substitute: MkdirAll does not tighten a
// directory that already exists, and on the documented quick-start the
// path is CWD-relative so EnsureParent is a no-op entirely.
//
// Starting from an explicit 0644 rather than checking a fresh create: a
// fresh-create assertion passes under `umask 077` even with the fix removed.
func TestOpenSQLite_TightensWorldReadableDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agentmodel.db")

	st, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	_ = st.Close()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	st2, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = st2.Close() })

	// The reopen recreates the -wal from the database's mode, so asserting it
	// covers the sidecar on every machine regardless of umask.
	for _, suffix := range []string{"", "-wal"} {
		fi, err := os.Stat(path + suffix)
		if err != nil {
			t.Fatalf("Stat %s%s: %v", filepath.Base(path), suffix, err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s%s mode = %v, want 0600", filepath.Base(path), suffix, got)
		}
	}
}

// ":memory:" reaches SecureSQLiteFile like any other path and must not have a
// file created for it.
func TestOpenSQLite_InMemoryCreatesNoFile(t *testing.T) {
	st, err := OpenSQLite(":memory:")
	if err != nil {
		t.Fatalf("OpenSQLite(\":memory:\"): %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := os.Stat(":memory:"); err == nil {
		_ = os.Remove(":memory:")
		t.Error(`a file named ":memory:" was created`)
	}
}
