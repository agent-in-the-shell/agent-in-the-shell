package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestServiceRotationHistoryAndAttribution(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithKeyActor(context.Background(), KeyActor{Kind: "admin", ID: "operator-1"})
	if _, err := s.CreateServiceKey(ctx, ManagedKey{ID: "svc", Name: "Worker", KeyHash: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPortalModels(ctx, "svc", []string{"model"}, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.SetKeyDisabled(ctx, "svc", true); err != nil {
		t.Fatal(err)
	}
	if err := s.LogRequest(ctx, RequestLog{ID: "request", APIKeyHash: "old", Status: "ok", TotalTokens: 7, CostUSD: 2, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	k, err := s.RotateServiceKey(ctx, "svc", 1, "new")
	if err != nil {
		t.Fatal(err)
	}
	if k.ID != "svc" || k.Name != "Worker" || !k.ServiceIssued || !k.Disabled || len(k.Models) != 1 || k.ServiceRevision != 2 {
		t.Fatalf("rotation lost policy: %+v", k)
	}
	if _, err := s.GetKeyByHash(ctx, "old"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.RotateServiceKey(ctx, "svc", 1, "stale"); !errors.Is(err, ErrPortalConflict) {
		t.Fatal("stale rotate", err)
	}
	if err := s.SetPortalModels(ctx, "svc", []string{"stale"}, 1); !errors.Is(err, ErrPortalConflict) {
		t.Fatal("stale grants", err)
	}
	if err := s.DeleteKey(ctx, "svc"); err != nil {
		t.Fatal(err)
	}
	k, err = s.RotateServiceKey(ctx, "svc", 2, "replacement")
	if err != nil {
		t.Fatal(err)
	}
	if k.RevokedAt != nil || !k.Disabled || len(k.Models) != 0 {
		t.Fatalf("unsafe replacement: %+v", k)
	}
	if _, err = s.RotateServiceKey(ctx, "svc", 3, "old"); err == nil {
		t.Fatal("reused retired secret")
	}
	s.Close()
	s, err = OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	history, err := s.GetKeyHistory(ctx, "svc", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 6 {
		t.Fatalf("events=%+v", history)
	}
	for _, e := range history {
		if e.KeyID != "svc" || e.Actor.ID != "operator-1" || e.CreatedAt.IsZero() {
			t.Fatalf("bad event %+v", e)
		}
	}
	cost, err := s.SumCostByAPIKey(ctx, "replacement", time.Time{})
	if err != nil || cost != 2 {
		t.Fatal(cost, err)
	}
	report, err := s.GetPortalOverview(ctx, time.Now(), PortalReportScope{})
	if err != nil || len(report.Keys) != 1 || report.Keys[0].Stats.TotalTokens != 7 {
		t.Fatal(report, err)
	}
}

func TestKeyAuditFailureRollsBackEveryMutation(t *testing.T) {
	s := usageStore(t)
	ctx := context.Background()
	if _, err := s.CreateServiceKey(ctx, ManagedKey{ID: "svc", Name: "Worker", KeyHash: "old"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_audit BEFORE INSERT ON key_events BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	for name, fn := range map[string]func() error{
		"create":  func() error { return s.CreateKey(ctx, ManagedKey{ID: "other", Name: "other", KeyHash: "other"}) },
		"disable": func() error { return s.SetKeyDisabled(ctx, "svc", true) },
		"revoke":  func() error { return s.DeleteKey(ctx, "svc") },
		"models":  func() error { return s.SetPortalModels(ctx, "svc", []string{"model"}, 1) },
		"rotate":  func() error { _, err := s.RotateServiceKey(ctx, "svc", 1, "new"); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := fn(); err == nil {
				t.Fatal("write succeeded without audit")
			}
		})
	}
	k, err := s.GetKeyByID(ctx, "svc")
	if err != nil || k.KeyHash != "old" || k.Disabled || k.RevokedAt != nil || len(k.Models) != 0 {
		t.Fatal(k, err)
	}
	if _, err := s.GetKeyByID(ctx, "other"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestConcurrentServiceRotationAndRevocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	other, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	ctx := context.Background()
	for i := range 12 {
		id := fmt.Sprintf("key-%d", i)
		if _, err := s.CreateServiceKey(ctx, ManagedKey{ID: id, Name: id, KeyHash: id + "-old"}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetPortalModels(ctx, id, []string{"model"}, 1); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		result := make(chan error, 2)
		go func() { <-start; result <- s.DeleteKey(ctx, id) }()
		go func() { <-start; _, err := other.RotateServiceKey(ctx, id, 1, id+"-new"); result <- err }()
		close(start)
		for range 2 {
			if err := <-result; err != nil {
				t.Fatal(err)
			}
		}
		key, err := s.GetKeyByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if key.RevokedAt == nil && len(key.Models) != 0 {
			t.Fatal("resurrected authorized key", key)
		}
		events, err := s.GetKeyHistory(ctx, id, 0)
		if err != nil || len(events) != 4 {
			t.Fatal(events, err)
		}
	}
}

func TestPortalAuditSurvivesReplacementAndFailsClosed(t *testing.T) {
	s := usageStore(t)
	ctx := WithKeyActor(context.Background(), KeyActor{Kind: "user", ID: "principal"})
	owner := usageOwner("user")
	createUsageKey(t, s, owner, "user", "old")
	if err := s.RevokePortalKey(ctx, "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RotatePortalKey(ctx, owner, 1, "new"); err != nil {
		t.Fatal(err)
	}
	events, err := s.GetKeyHistory(ctx, "user", 0)
	if err != nil || len(events) != 3 || events[0].Action != "rotate" || events[1].Action != "revoke" {
		t.Fatal(events, err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_audit BEFORE INSERT ON key_events BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RotatePortalKey(ctx, owner, 2, "failed"); err == nil {
		t.Fatal("rotation missing audit")
	}
	binding, err := s.GetPortalBinding(ctx, owner)
	if err != nil || binding.Revision != 2 {
		t.Fatal(binding, err)
	}
	if _, err := s.GetKeyByHash(ctx, "new"); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokePortalKey(ctx, "user"); err == nil {
		t.Fatal("revoke missing audit")
	}
	if err := s.SetPortalDisabled(ctx, "user", true); err == nil {
		t.Fatal("disable missing audit")
	}
	if _, err := s.CreatePortalKey(ctx, usageOwner("other"), ManagedKey{ID: "other", Name: "other", KeyHash: "other"}); err == nil {
		t.Fatal("issuance missing audit")
	}
	if _, err := s.GetPortalBinding(ctx, usageOwner("other")); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestHistoryPagination(t *testing.T) {
	s := usageStore(t)
	ctx := context.Background()
	if err := s.CreateKey(ctx, ManagedKey{ID: "key", Name: "key", KeyHash: "hash"}); err != nil {
		t.Fatal(err)
	}
	for i := range 101 {
		if err := s.SetKeyDisabled(ctx, "key", i%2 == 0); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.GetKeyHistory(ctx, "key", 0)
	if err != nil || len(first) != 100 {
		t.Fatal(len(first), err)
	}
	second, err := s.GetKeyHistory(ctx, "key", first[99].ID)
	if err != nil || len(second) != 2 || second[0].ID >= first[99].ID {
		t.Fatal(second, err)
	}
}

func TestHistoryMigrationBackfillsServiceWithoutInventingEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE api_keys(id TEXT PRIMARY KEY,name TEXT NOT NULL,key_hash TEXT NOT NULL UNIQUE,models TEXT NOT NULL DEFAULT '',max_budget REAL,budget_duration TEXT NOT NULL DEFAULT '',expires_at INTEGER,disabled INTEGER NOT NULL DEFAULT 0,revoked_at INTEGER,metadata TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL);
 CREATE TABLE service_keys(key_id TEXT PRIMARY KEY NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,normalized_name TEXT NOT NULL UNIQUE);
 INSERT INTO api_keys(id,name,key_hash,models,created_at) VALUES ('svc','Worker','existing','["model"]',1);
 INSERT INTO service_keys(key_id,normalized_name) VALUES ('svc','worker');`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	for range 2 {
		s, err := OpenSQLite(path)
		if err != nil {
			t.Fatal(err)
		}
		k, err := s.GetKeyByID(context.Background(), "svc")
		if err != nil || k.ServiceRevision != 1 || len(k.Models) != 1 {
			t.Fatal(k, err)
		}
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM service_key_hashes WHERE key_id='svc' AND key_hash='existing'`).Scan(&n); err != nil || n != 1 {
			t.Fatal(n, err)
		}
		events, err := s.GetKeyHistory(context.Background(), "svc", 0)
		if err != nil || len(events) != 0 {
			t.Fatal(events, err)
		}
		s.Close()
	}
}

func TestConcurrentServiceRotationsHaveSingleWinner(t *testing.T) {
	s := usageStore(t)
	ctx := context.Background()
	if _, err := s.CreateServiceKey(ctx, ManagedKey{ID: "svc", Name: "Worker", KeyHash: "old"}); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	start := make(chan struct{})
	for _, hash := range []string{"new1", "new2"} {
		go func() { <-start; _, err := s.RotateServiceKey(ctx, "svc", 1, hash); results <- err }()
	}
	close(start)
	success := 0
	conflict := 0
	for range 2 {
		err := <-results
		if err == nil {
			success++
		} else if errors.Is(err, ErrPortalConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal(success, conflict)
	}
	events, err := s.GetKeyHistory(ctx, "svc", 0)
	if err != nil || len(events) != 2 {
		t.Fatal(events, err)
	}
}

func TestStaleServiceGrantCannotAuthorizeRevokedReplacement(t *testing.T) {
	s := usageStore(t)
	ctx := context.Background()
	for i := range 12 {
		id := fmt.Sprintf("grant-%d", i)
		if _, err := s.CreateServiceKey(ctx, ManagedKey{ID: id, Name: id, KeyHash: id + "-old"}); err != nil {
			t.Fatal(err)
		}
		if err := s.RevokePortalKey(ctx, id); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		results := make(chan error, 2)
		go func() { <-start; results <- s.SetPortalModels(ctx, id, []string{"model"}, 1) }()
		go func() { <-start; _, err := s.RotateServiceKey(ctx, id, 1, id+"-new"); results <- err }()
		close(start)
		success := 0
		conflicts := 0
		for range 2 {
			err := <-results
			if err == nil {
				success++
			} else if errors.Is(err, ErrPortalConflict) {
				conflicts++
			} else {
				t.Fatal(err)
			}
		}
		if success != 1 || conflicts != 1 {
			t.Fatal(success, conflicts)
		}
		key, err := s.GetKeyByID(ctx, id)
		if err != nil || key.RevokedAt != nil || len(key.Models) != 0 {
			t.Fatal(key, err)
		}
		history, err := s.GetKeyHistory(ctx, id, 0)
		if err != nil || len(history) != 3 {
			t.Fatal(history, err)
		}
	}
}
