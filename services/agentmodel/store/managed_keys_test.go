package store_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

func ptrF(f float64) *float64 { return &f }

func sampleKey(id, hash string, opts ...func(*store.ManagedKey)) store.ManagedKey {
	k := store.ManagedKey{
		ID:             id,
		Name:           "ci",
		KeyHash:        hash,
		Models:         []string{"claude-opus-4-8", "gpt-4o"},
		MaxBudget:      ptrF(50),
		BudgetDuration: "24h",
		Metadata:       `{"team":"core"}`,
		CreatedAt:      time.Unix(1_700_000_000, 0).UTC(),
	}
	for _, o := range opts {
		o(&k)
	}
	return k
}

func TestManagedKey_CreateGetRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	exp := time.Unix(1_800_000_000, 0).UTC()
	want := sampleKey("id-1", "hash-1", func(k *store.ManagedKey) { k.ExpiresAt = &exp })
	if err := s.CreateKey(ctx, want); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	got, err := s.GetKeyByHash(ctx, "hash-1")
	if err != nil {
		t.Fatalf("GetKeyByHash: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestManagedKey_CreateMinimal(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// nil models / nil budget / nil expiry — the uncapped, all-models shape.
	want := sampleKey("id-min", "hash-min", func(k *store.ManagedKey) {
		k.Models = nil
		k.MaxBudget = nil
		k.BudgetDuration = ""
		k.Metadata = ""
	})
	if err := s.CreateKey(ctx, want); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	got, err := s.GetKeyByHash(ctx, "hash-min")
	if err != nil {
		t.Fatalf("GetKeyByHash: %v", err)
	}
	if got.Models != nil {
		t.Errorf("Models: got %v, want nil", got.Models)
	}
	if got.MaxBudget != nil {
		t.Errorf("MaxBudget: got %v, want nil", got.MaxBudget)
	}
	if got.ExpiresAt != nil {
		t.Errorf("ExpiresAt: got %v, want nil", got.ExpiresAt)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestManagedKey_CreatedAtDefaulted(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	k := sampleKey("id-ts", "hash-ts", func(k *store.ManagedKey) { k.CreatedAt = time.Time{} })
	if err := s.CreateKey(ctx, k); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	got, err := s.GetKeyByHash(ctx, "hash-ts")
	if err != nil {
		t.Fatalf("GetKeyByHash: %v", err)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt: zero value was not defaulted on create")
	}
}

func TestGetKeyByHash_NotFound(t *testing.T) {
	s := newStore(t)
	if _, err := s.GetKeyByHash(context.Background(), "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestGetKeyByID(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	want := sampleKey("id-byid", "hash-byid")
	if err := s.CreateKey(ctx, want); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	got, err := s.GetKeyByID(ctx, "id-byid")
	if err != nil {
		t.Fatalf("GetKeyByID: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
	if _, err := s.GetKeyByID(ctx, "ghost"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing id: got %v, want ErrNotFound", err)
	}
}

func TestCreateKey_DuplicateHash(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if err := s.CreateKey(ctx, sampleKey("id-a", "dup")); err != nil {
		t.Fatalf("first CreateKey: %v", err)
	}
	if err := s.CreateKey(ctx, sampleKey("id-b", "dup")); err == nil {
		t.Error("second CreateKey with duplicate hash: want error, got nil")
	}
}

func TestListKeys_NewestFirst(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mk := func(id, hash string, ts int64) store.ManagedKey {
		return sampleKey(id, hash, func(k *store.ManagedKey) { k.CreatedAt = time.Unix(ts, 0).UTC() })
	}
	// Insert out of order; expect created_at DESC.
	if err := s.CreateKey(ctx, mk("id-mid", "h-mid", 200)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateKey(ctx, mk("id-new", "h-new", 300)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateKey(ctx, mk("id-old", "h-old", 100)); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListKeys(ctx)
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	wantIDs := []string{"id-new", "id-mid", "id-old"}
	if len(got) != len(wantIDs) {
		t.Fatalf("ListKeys len: got %d, want %d", len(got), len(wantIDs))
	}
	for i, id := range wantIDs {
		if got[i].ID != id {
			t.Errorf("position %d: got %q, want %q", i, got[i].ID, id)
		}
	}
}

func TestListKeys_Empty(t *testing.T) {
	s := newStore(t)
	got, err := s.ListKeys(context.Background())
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListKeys on empty store: got %d rows, want 0", len(got))
	}
}

func TestSetKeyDisabled(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if err := s.CreateKey(ctx, sampleKey("id-d", "h-d")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetKeyDisabled(ctx, "id-d", true); err != nil {
		t.Fatalf("SetKeyDisabled(true): %v", err)
	}
	got, err := s.GetKeyByHash(ctx, "h-d")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Disabled {
		t.Error("Disabled: got false after revoke, want true")
	}
	// Revoke retains the row (and its spend attribution).
	if err := s.SetKeyDisabled(ctx, "id-d", false); err != nil {
		t.Fatalf("SetKeyDisabled(false): %v", err)
	}
	got, _ = s.GetKeyByHash(ctx, "h-d")
	if got.Disabled {
		t.Error("Disabled: got true after un-revoke, want false")
	}
}

func TestSetKeyDisabled_NotFound(t *testing.T) {
	s := newStore(t)
	if err := s.SetKeyDisabled(context.Background(), "ghost", true); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestDeleteKey(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if err := s.CreateKey(ctx, sampleKey("id-del", "h-del")); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteKey(ctx, "id-del"); err != nil {
		t.Fatalf("DeleteKey: %v", err)
	}
	if k, err := s.GetKeyByHash(ctx, "h-del"); err != nil || k.RevokedAt == nil {
		t.Fatalf("revocation not retained: %+v %v", k, err)
	}
}

func TestDeleteKey_NotFound(t *testing.T) {
	s := newStore(t)
	if err := s.DeleteKey(context.Background(), "ghost"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestDeleteRetainsIdentityAndCannotEnable(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	key := sampleKey("retained", "retained-hash")
	if err := s.CreateKey(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteKey(ctx, key.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetKeyByID(ctx, key.ID)
	if err != nil {
		t.Fatalf("revocation lost identity: %v", err)
	}
	if got.KeyHash != key.KeyHash {
		t.Fatal("revocation changed attribution")
	}
	if err := s.SetKeyDisabled(ctx, key.ID, false); err == nil {
		t.Fatal("revoked credential could be enabled")
	}
}
