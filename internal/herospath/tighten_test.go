package herospath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSecureSQLiteFile(t *testing.T) {
	// The case that matters on upgrade: a database and a WAL left behind by an
	// older version. The WAL is the one that gets reused rather than recreated,
	// so it keeps 0644 unless chmod-ed directly.
	t.Run("tightens an existing database and its sidecars", func(t *testing.T) {
		db := filepath.Join(t.TempDir(), "x.db")
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.WriteFile(db+suffix, []byte("x"), 0o600); err != nil {
				t.Fatalf("WriteFile %s: %v", suffix, err)
			}
			if err := os.Chmod(db+suffix, 0o644); err != nil {
				t.Fatalf("Chmod %s: %v", suffix, err)
			}
		}
		if err := SecureSQLiteFile(db); err != nil {
			t.Fatalf("SecureSQLiteFile: %v", err)
		}
		for _, suffix := range []string{"", "-wal", "-shm"} {
			fi, err := os.Stat(db + suffix)
			if err != nil {
				t.Fatalf("Stat %s: %v", suffix, err)
			}
			if got := fi.Mode().Perm(); got != 0o600 {
				t.Errorf("x.db%s mode = %v, want 0600", suffix, got)
			}
		}
	})

	// The point of running before the open: SQLite never gets to create the
	// file, so it never exists at a loose mode even momentarily.
	t.Run("creates an absent database 0600", func(t *testing.T) {
		db := filepath.Join(t.TempDir(), "new.db")
		if err := SecureSQLiteFile(db); err != nil {
			t.Fatalf("SecureSQLiteFile: %v", err)
		}
		fi, err := os.Stat(db)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("mode = %v, want 0600", got)
		}
		if fi.Size() != 0 {
			t.Errorf("size = %d, want an empty file SQLite will initialize", fi.Size())
		}
	})

	// Sidecars exist only while a WAL is live, so their absence is the normal
	// case and must not be an error.
	t.Run("absent sidecars are not an error", func(t *testing.T) {
		db := filepath.Join(t.TempDir(), "x.db")
		if err := os.WriteFile(db, []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := SecureSQLiteFile(db); err != nil {
			t.Errorf("SecureSQLiteFile with no sidecars: %v", err)
		}
	})

	// store.OpenSQLite accepts ":memory:"; creating a file for it would be
	// wrong, and EnsureParent is likewise a no-op there.
	t.Run("non-file DSNs are left alone", func(t *testing.T) {
		if err := SecureSQLiteFile(":memory:"); err != nil {
			t.Errorf("SecureSQLiteFile(\":memory:\"): %v", err)
		}
		if _, err := os.Stat(":memory:"); err == nil {
			_ = os.Remove(":memory:")
			t.Error(`a file named ":memory:" was created`)
		}
	})

	// A path we cannot secure must surface. Callers fail closed on it rather
	// than opening a database they cannot protect.
	t.Run("an unusable path is returned as an error", func(t *testing.T) {
		notADir := filepath.Join(t.TempDir(), "regular-file")
		if err := os.WriteFile(notADir, []byte("file"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := SecureSQLiteFile(filepath.Join(notADir, "x.db")); err == nil {
			t.Error("expected an error for a path below a regular file")
		}
	})
}
