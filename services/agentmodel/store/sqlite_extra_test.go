package store_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

// TestPing_OK exercises the otherwise-untested Ping method against a live DB.
func TestPing_OK(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("Ping on open store: %v", err)
	}
}

// TestListByAPIKey_FilterOrderLimit mirrors the ListByOrg coverage: filtering by
// api_key_hash, DESC ordering, the LIMIT clause, and the `since` lower bound.
func TestListByAPIKey_FilterOrderLimit(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	// Five logs for the target key, newest first by index.
	for i := 0; i < 5; i++ {
		log := sampleLog(stringNum("k-", i), func(r *store.RequestLog) {
			r.APIKeyHash = "hash-target"
			r.CreatedAt = now.Add(-time.Duration(i) * time.Minute)
		})
		if err := s.LogRequest(ctx, log); err != nil {
			t.Fatalf("LogRequest: %v", err)
		}
	}
	// Decoy logs under a different key that must never surface.
	for i := 0; i < 3; i++ {
		log := sampleLog(stringNum("d-", i), func(r *store.RequestLog) {
			r.APIKeyHash = "hash-other"
			r.CreatedAt = now
		})
		if err := s.LogRequest(ctx, log); err != nil {
			t.Fatalf("LogRequest decoy: %v", err)
		}
	}

	got, err := s.ListByAPIKey(ctx, "hash-target", now.Add(-10*time.Minute), 3)
	if err != nil {
		t.Fatalf("ListByAPIKey: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len: got %d, want 3 (LIMIT)", len(got))
	}
	// DESC by created_at: k-0 (now) first, then k-1, k-2.
	if got[0].ID != "k-0" || got[1].ID != "k-1" || got[2].ID != "k-2" {
		t.Errorf("ordering: got %v", []string{got[0].ID, got[1].ID, got[2].ID})
	}
	for _, r := range got {
		if r.APIKeyHash != "hash-target" {
			t.Errorf("filter leaked a non-target key: %q", r.APIKeyHash)
		}
	}
}

// TestListByAPIKey_FiltersBySince proves the created_at >= since bound is applied.
func TestListByAPIKey_FiltersBySince(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	old := sampleLog("old", func(r *store.RequestLog) {
		r.APIKeyHash = "hk"
		r.CreatedAt = time.Unix(1000, 0).UTC()
	})
	recent := sampleLog("recent", func(r *store.RequestLog) {
		r.APIKeyHash = "hk"
		r.CreatedAt = time.Unix(2000, 0).UTC()
	})
	if err := s.LogRequest(ctx, old); err != nil {
		t.Fatalf("LogRequest old: %v", err)
	}
	if err := s.LogRequest(ctx, recent); err != nil {
		t.Fatalf("LogRequest recent: %v", err)
	}

	got, err := s.ListByAPIKey(ctx, "hk", time.Unix(1500, 0), 100)
	if err != nil {
		t.Fatalf("ListByAPIKey: %v", err)
	}
	if len(got) != 1 || got[0].ID != "recent" {
		t.Errorf("expected only the recent log, got %+v", got)
	}
}

// TestListByAPIKey_DefaultLimit hits the limit<=0 default branch: more than the
// implicit default (100) is not reachable cheaply, but we assert that limit=0 is
// accepted and returns rows rather than zero/erroring.
func TestListByAPIKey_DefaultLimit(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		log := sampleLog(stringNum("z-", i), func(r *store.RequestLog) {
			r.APIKeyHash = "hz"
		})
		if err := s.LogRequest(ctx, log); err != nil {
			t.Fatalf("LogRequest: %v", err)
		}
	}

	// limit <= 0 must be coerced to the 100 default, not return empty.
	got, err := s.ListByAPIKey(ctx, "hz", time.Unix(0, 0), 0)
	if err != nil {
		t.Fatalf("ListByAPIKey default-limit: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("default-limit: got %d rows, want 3", len(got))
	}

	// Same default branch for ListByOrg with a negative limit, for symmetry.
	gotOrg, err := s.ListByOrg(ctx, "org-1", time.Unix(0, 0), -5)
	if err != nil {
		t.Fatalf("ListByOrg negative-limit: %v", err)
	}
	if len(gotOrg) != 3 {
		t.Errorf("ListByOrg default-limit: got %d rows, want 3", len(gotOrg))
	}
}

// TestOpenSQLite_MigrateError forces db.Exec(sqliteSchema) to fail by pointing
// the DSN at a path that is itself an existing directory. EnsureParent succeeds
// because the parent already exists, and the failure must surface as a wrapped
// error with no store handed back.
//
// This used to reach the CREATE TABLE migration and assert on "migrate".
// SecureSQLiteFile now runs before the open and refuses the directory
// first, which is the same verdict earlier and with the path named — so the
// assertion is on the wrapped path rather than on which step caught it.
func TestOpenSQLite_UnusablePathError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// The DB path is itself a directory: parent exists (EnsureParent is a no-op)
	// but the file cannot be opened for the migration.
	badPath := filepath.Join(dir, "db-is-a-dir")
	if err := os.MkdirAll(badPath, 0o755); err != nil {
		t.Fatalf("mkdir bad path: %v", err)
	}

	s, err := store.OpenSQLite(badPath)
	if err == nil {
		if s != nil {
			_ = s.Close()
		}
		t.Fatalf("OpenSQLite(%q): want an error, got nil", badPath)
	}
	if s != nil {
		t.Fatalf("OpenSQLite returned non-nil store alongside error: %v", s)
	}
	if !strings.Contains(err.Error(), badPath) {
		t.Errorf("error should name the offending path, got: %v", err)
	}
	if !strings.Contains(err.Error(), "agentmodel/store") {
		t.Errorf("error should be wrapped with the package prefix, got: %v", err)
	}
}

// TestClosedStore_AllMethodsError closes the underlying *sql.DB and then drives
// every method through its Exec/Query/QueryRow/Ping error wrapper. Each must
// surface a non-nil error rather than panic or silently succeed.
func TestClosedStore_AllMethodsError(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "closed.db")
	s, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	ctx := context.Background()
	// Seed one row so the read paths have data to (attempt to) return, proving
	// the failure is the closed handle and not an empty table.
	if err := s.LogRequest(ctx, sampleLog("seed")); err != nil {
		t.Fatalf("seed LogRequest: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	t.Run("Ping", func(t *testing.T) {
		if err := s.Ping(ctx); err == nil {
			t.Error("Ping on closed DB: want error, got nil")
		}
	})

	t.Run("LogRequest", func(t *testing.T) {
		err := s.LogRequest(ctx, sampleLog("after-close"))
		if err == nil {
			t.Error("LogRequest on closed DB: want error, got nil")
		} else if !strings.Contains(err.Error(), "insert") {
			t.Errorf("LogRequest error not wrapped as insert: %v", err)
		}
	})

	t.Run("GetRequestLog", func(t *testing.T) {
		_, err := s.GetRequestLog(ctx, "seed")
		if err == nil {
			t.Error("GetRequestLog on closed DB: want error, got nil")
		}
	})

	t.Run("ListByOrg", func(t *testing.T) {
		_, err := s.ListByOrg(ctx, "org-1", time.Unix(0, 0), 10)
		if err == nil {
			t.Error("ListByOrg on closed DB: want error, got nil")
		} else if !strings.Contains(err.Error(), "list by org") {
			t.Errorf("ListByOrg error not wrapped: %v", err)
		}
	})

	t.Run("ListByAPIKey", func(t *testing.T) {
		_, err := s.ListByAPIKey(ctx, "hash-abc", time.Unix(0, 0), 10)
		if err == nil {
			t.Error("ListByAPIKey on closed DB: want error, got nil")
		} else if !strings.Contains(err.Error(), "list by api key") {
			t.Errorf("ListByAPIKey error not wrapped: %v", err)
		}
	})

	t.Run("SumCostByOrg", func(t *testing.T) {
		_, err := s.SumCostByOrg(ctx, "org-1", time.Unix(0, 0))
		if err == nil {
			t.Error("SumCostByOrg on closed DB: want error, got nil")
		} else if !strings.Contains(err.Error(), "sum cost") {
			t.Errorf("SumCostByOrg error not wrapped: %v", err)
		}
	})

	t.Run("CountRequestsByOrg", func(t *testing.T) {
		_, err := s.CountRequestsByOrg(ctx, "org-1", time.Unix(0, 0), agentmodel.AuthModeAPIKey)
		if err == nil {
			t.Error("CountRequestsByOrg on closed DB: want error, got nil")
		} else if !strings.Contains(err.Error(), "count") {
			t.Errorf("CountRequestsByOrg error not wrapped: %v", err)
		}
	})
}
