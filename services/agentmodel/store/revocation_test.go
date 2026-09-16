package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRevocationMigrationRetainsExistingDisabledAndHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// Exercise an actual pre-column database, not only a fresh schema.
	if _, err = db.Exec(strings.ReplaceAll(sqliteSchema, "    revoked_at      INTEGER,\n", "")); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO api_keys(id,name,key_hash,disabled,created_at) VALUES ('paused','Paused','hash',1,1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key, err := s.GetKeyByID(ctx, "paused")
	if err != nil || !key.Disabled || key.RevokedAt != nil {
		t.Fatal(key, err)
	}
	if err := s.SetKeyDisabled(ctx, key.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := s.LogRequest(ctx, RequestLog{ID: "request", APIKeyHash: key.KeyHash, CostUSD: 2, Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteKey(ctx, key.ID); err != nil {
		t.Fatal(err)
	}
	first, _ := s.GetKeyByID(ctx, key.ID)
	s.Close()
	s, err = OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.DeleteKey(ctx, key.ID); err != nil {
		t.Fatal(err)
	}
	retained, err := s.GetKeyByID(ctx, key.ID)
	if err != nil || retained.RevokedAt == nil || !retained.RevokedAt.Equal(*first.RevokedAt) || retained.KeyHash != key.KeyHash {
		t.Fatal(retained, err)
	}
	if err := s.SetKeyDisabled(ctx, key.ID, false); !errors.Is(err, ErrNotFound) {
		t.Fatal("enabled revocation", err)
	}
	cost, err := s.SumCostByAPIKey(ctx, key.KeyHash, time.Time{})
	if err != nil || cost != 2 {
		t.Fatal(cost, err)
	}
}

func TestRevokedPortalReplacementIsFreshDenyAllAndRetainsOneIdentity(t *testing.T) {
	s := usageStore(t)
	ctx := context.Background()
	owner := usageOwner("alice")
	createUsageKey(t, s, owner, "alice", "original")
	if err := s.SetPortalModels(ctx, "alice", []string{"model"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.LogRequest(ctx, RequestLog{ID: "before", APIKeyHash: "original", Status: "ok", TotalTokens: 7, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokePortalKey(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	revoked, _ := s.GetKeyByID(ctx, "alice")
	for _, change := range []func() error{
		func() error { return s.SetPortalDisabled(ctx, "alice", false) },
		func() error { return s.SetPortalModels(ctx, "alice", []string{"model"}) },
	} {
		if err := change(); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
	}
	// Duplicate/old hashes must roll back both the replacement and revision.
	if _, err := s.RotatePortalKey(ctx, owner, 1, "original"); err == nil {
		t.Fatal("reused original secret")
	}
	still, _ := s.GetKeyByID(ctx, "alice")
	if still.RevokedAt == nil || !still.RevokedAt.Equal(*revoked.RevokedAt) {
		t.Fatal(still)
	}
	next, err := s.RotatePortalKey(ctx, owner, 1, "fresh")
	if err != nil || next.KeyID != "alice" || next.Revision != 2 {
		t.Fatal(next, err)
	}
	key, err := s.GetKeyByHash(ctx, "fresh")
	if err != nil || !key.PortalIssued || key.RevokedAt != nil || len(key.Models) != 0 {
		t.Fatal(key, err)
	}
	if _, err := s.GetKeyByHash(ctx, "original"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.CreatePortalKey(ctx, owner, ManagedKey{ID: "second", Name: owner.Email, KeyHash: "second"}); !errors.Is(err, ErrPortalConflict) {
		t.Fatal("second logical key", err)
	}
	if _, err := s.RotatePortalKey(ctx, owner, 1, "stale"); !errors.Is(err, ErrPortalConflict) {
		t.Fatal("stale revision", err)
	}
	usage, err := s.GetPortalUsage(ctx, owner, now.Add(time.Second))
	if err != nil || usage.Stats.TotalTokens != 7 {
		t.Fatal(usage, err)
	}
	// Normal rotation after reauthorization preserves grants, unlike replacement.
	if err := s.SetPortalModels(ctx, "alice", []string{"model"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RotatePortalKey(ctx, owner, 2, "third"); err != nil {
		t.Fatal(err)
	}
	key, _ = s.GetKeyByHash(ctx, "third")
	if len(key.Models) != 1 {
		t.Fatal(key)
	}
}

func TestRevocationAndReplacementRemainSerializable(t *testing.T) {
	s := usageStore(t)
	ctx := context.Background()
	owner := usageOwner("race")
	createUsageKey(t, s, owner, "race", "old")
	if err := s.SetPortalModels(ctx, "race", []string{"model"}); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	go func() { results <- s.RevokePortalKey(ctx, "race") }()
	go func() { _, err := s.RotatePortalKey(ctx, owner, 1, "new"); results <- err }()
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	key, err := s.GetKeyByHash(ctx, "new")
	if err != nil {
		t.Fatal(err)
	}
	// Either revoke follows rotation (new secret revoked), or replacement follows
	// revoke (new secret deny-all). No ordering restores an authorized credential.
	if key.RevokedAt == nil && len(key.Models) != 0 {
		t.Fatal("authorized replacement", key)
	}
	if _, err := s.GetKeyByHash(ctx, "old"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestGetKeyByIDIncludesCurrentOwnership(t *testing.T) {
	s := usageStore(t)
	ctx := context.Background()
	createUsageKey(t, s, usageOwner("employee"), "employee", "employee-hash")
	if _, err := s.CreateServiceKey(ctx, ManagedKey{ID: "service", Name: "Service", KeyHash: "service-hash"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateKey(ctx, ManagedKey{ID: "legacy", Name: "Legacy", KeyHash: "legacy-hash"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"employee", "service", "legacy"} {
		key, err := s.GetKeyByID(ctx, id)
		if err != nil || key.PortalIssued != (id == "employee") || key.ServiceIssued != (id == "service") {
			t.Fatalf("%s: %+v %v", id, key, err)
		}
	}
}
