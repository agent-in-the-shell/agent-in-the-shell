package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

func portalKey(id, hash string) store.ManagedKey {
	k := sampleKey(id, hash)
	k.MaxBudget = nil
	k.BudgetDuration = ""
	return k
}

func newPortalStore(t *testing.T) *store.SQLiteStore {
	t.Helper()
	return newStore(t)
}

func TestPortalRotationPreservesIdentityScopeAndRevocation(t *testing.T) {
	s := newPortalStore(t)
	ctx := context.Background()
	identity := store.PortalIdentity{Issuer: "issuer", Subject: "subject", Email: "employee@example.com"}
	key := portalKey("stable-id", "old-hash")
	key.Disabled = true
	binding, err := s.CreatePortalKey(ctx, identity, key)
	if err != nil {
		t.Fatal(err)
	}
	if binding.Revision != 1 {
		t.Fatal(binding)
	}
	if err := s.SetPortalModels(ctx, key.ID, key.Models); err != nil {
		t.Fatal(err)
	}
	rotated, err := s.RotatePortalKey(ctx, identity, 1, "new-hash")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Revision != 2 || rotated.KeyID != key.ID {
		t.Fatal(rotated)
	}
	got, err := s.GetKeyByID(ctx, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	key.KeyHash = "new-hash"
	key.PortalIssued = true // Management reads include current ownership.
	if !reflect.DeepEqual(got, key) {
		t.Fatalf("rotation changed policy: got %+v want %+v", got, key)
	}
	if _, err := s.GetKeyByHash(ctx, "old-hash"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old token authenticates: %v", err)
	}
	if _, err := s.RotatePortalKey(ctx, identity, 1, "stale"); !errors.Is(err, store.ErrPortalConflict) {
		t.Fatalf("stale CAS: %v", err)
	}
	// No ledger rewrite: late old-token requests retain their attribution and
	// org accounting; these intentionally uncapped keys have no lifetime counter.
	if err := s.LogRequest(ctx, store.RequestLog{ID: "late", OrgID: "default", APIKeyHash: "old-hash", CostUSD: 4, Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	spent, err := s.SumCostByOrg(ctx, "default", time.Time{})
	if err != nil || spent != 4 {
		t.Fatal(spent, err)
	}
	if err := s.DeleteKey(ctx, key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePortalKey(ctx, identity, portalKey("replacement", "replacement")); !errors.Is(err, store.ErrPortalConflict) {
		t.Fatalf("delete reopened issuance: %v", err)
	}
}

func TestPortalCreateLegacyConflictAndUnsupportedPolicy(t *testing.T) {
	s := newPortalStore(t)
	ctx := context.Background()
	identity := store.PortalIdentity{Issuer: "issuer", Subject: "subject", Email: "employee@example.com"}
	legacy := portalKey("legacy", "legacy")
	legacy.Name = " Employee@Example.com "
	legacy.Disabled = true
	if err := s.CreateKey(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePortalKey(ctx, identity, portalKey("new", "new")); !errors.Is(err, store.ErrMigrationRequired) {
		t.Fatalf("implicit adoption: %v", err)
	}
	if _, err := s.GetKeyByID(ctx, "new"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("failed create left key")
	}
	if _, err := s.RotatePortalKey(ctx, identity, 1, "rotated"); !errors.Is(err, store.ErrPortalConflict) {
		t.Fatal("legacy key rotated", err)
	}
	for _, mutate := range []func(*store.ManagedKey){func(k *store.ManagedKey) { k.MaxBudget = ptrF(10) }, func(k *store.ManagedKey) { now := time.Now(); k.ExpiresAt = &now }, func(k *store.ManagedKey) { k.BudgetDuration = "24h" }} {
		k := portalKey("restricted", "restricted")
		mutate(&k)
		if _, err := s.CreatePortalKey(ctx, identity, k); !errors.Is(err, store.ErrPortalPolicy) {
			t.Fatal("unsupported policy accepted", err)
		}
	}
}

func TestPortalReopenAndFailedRotationRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "portal.db")
	st, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	identity := store.PortalIdentity{Issuer: "issuer", Subject: "subject", Email: "employee@example.com"}
	if _, err := st.CreatePortalKey(ctx, identity, portalKey("id", "old")); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateKey(ctx, portalKey("unrelated", "collision")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RotatePortalKey(ctx, identity, 1, "collision"); err == nil {
		t.Fatal("hash collision committed")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	binding, err := st.GetPortalBinding(ctx, identity)
	if err != nil || binding.KeyID != "id" || binding.Revision != 1 {
		t.Fatal(binding, err)
	}
	if _, err := st.GetKeyByHash(ctx, "old"); err != nil {
		t.Fatal("rollback lost old token", err)
	}
	// Changing the display email is not changing the immutable owner.
	identity.Email = "renamed@example.com"
	if _, err := st.RotatePortalKey(ctx, identity, 1, "new"); err != nil {
		t.Fatal(err)
	}
}

func TestPortalConcurrentCreateAndRotate(t *testing.T) {
	s := newPortalStore(t)
	ctx := context.Background()
	identity := store.PortalIdentity{Issuer: "issuer", Subject: "subject", Email: "employee@example.com"}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, id := range []string{"a", "b"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, err := s.CreatePortalKey(ctx, identity, portalKey(id, id))
			results <- err
		}(id)
	}
	wg.Wait()
	successes := 0
	for range 2 {
		if err := <-results; err == nil {
			successes++
		} else if !errors.Is(err, store.ErrPortalConflict) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("creates: %d", successes)
	}
	for _, hash := range []string{"c", "d"} {
		wg.Add(1)
		go func(hash string) {
			defer wg.Done()
			_, err := s.RotatePortalKey(ctx, identity, 1, hash)
			results <- err
		}(hash)
	}
	wg.Wait()
	successes = 0
	for range 2 {
		if err := <-results; err == nil {
			successes++
		} else if !errors.Is(err, store.ErrPortalConflict) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("rotations: %d", successes)
	}
}
