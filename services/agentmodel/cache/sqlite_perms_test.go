package cache

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// response_cache.value is the exact byte slice written to the client —
// api/handlers_chat.go marshals the response once and hands the same body to
// both w.Write and cache.Set. So this file holds completion text verbatim, and
// SQLite would leave it 0644. The -wal matters as much as the database:
// it holds writes not yet checkpointed, which is where the newest completions
// are.
//
// The database is deliberately made 0644 first rather than just checking a
// freshly created one. A fresh-create assertion passes under `umask 077` even
// with the fix removed, because SQLite inherits the umask — it would prove
// nothing on a hardened host. Starting from an explicit 0644, as a database
// written by an older version would be, fails under any umask.
func TestSQLiteCache_TightensWorldReadableDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")

	c, err := newSQLite(path, time.Minute, time.Now)
	if err != nil {
		t.Fatalf("newSQLite: %v", err)
	}
	c.Set(context.Background(), "k", []byte(`{"choices":[{"message":{"content":"secret"}}]}`))
	_ = c.db.Close() // last close checkpoints and removes the sidecars

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	c2, err := newSQLite(path, time.Minute, time.Now)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = c2.db.Close() })
	c2.Set(context.Background(), "k2", []byte("more"))

	// The reopen recreates the -wal. Had the database still been 0644 the
	// sidecar would inherit that, so asserting it here is what covers the WAL
	// on every machine regardless of umask.
	for _, suffix := range []string{"", "-wal"} {
		fi, err := os.Stat(path + suffix)
		if err != nil {
			t.Fatalf("Stat %s%s: %v", filepath.Base(path), suffix, err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s%s mode = %v, want 0600 — completion bodies readable by other local users",
				filepath.Base(path), suffix, got)
		}
	}
}

// SecureSQLiteFile runs before the open, so the database never exists at a
// loose mode even for the moment it takes to apply the schema.
func TestSQLiteCache_NeverExistsWorldReadable(t *testing.T) {
	// A nested path so EnsureParent actually creates the directory — it only
	// sets 0700 on a directory it makes, never on one that already exists
	//, and t.TempDir() hands back 0755.
	dir := filepath.Join(t.TempDir(), "cachedir")
	path := filepath.Join(dir, "cache.db")

	c, err := newSQLite(path, time.Minute, time.Now)
	if err != nil {
		t.Fatalf("newSQLite: %v", err)
	}
	t.Cleanup(func() { _ = c.db.Close() })

	// The parent is ours too: EnsureParent creates it 0700, so even a window
	// on the file would not be reachable by another user.
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}
	if got := fi.Mode().Perm() & 0o077; got != 0 {
		t.Errorf("parent dir mode = %v, want no group/other bits", fi.Mode().Perm())
	}
}
