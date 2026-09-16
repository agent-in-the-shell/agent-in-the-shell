package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestPortalIndividualModelsLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.db")
	st, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	owner := usageOwner("alice")
	key := ManagedKey{ID: "alice", Name: owner.Email, KeyHash: "old", Models: []string{"untrusted-snapshot"}}
	if _, err := st.CreatePortalKey(ctx, owner, key); err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetKeyByID(ctx, "alice")
	if err != nil || stored.Models == nil || len(stored.Models) != 0 {
		t.Fatal(stored, err)
	}
	var raw string
	if err := st.db.QueryRow(`SELECT models FROM api_keys WHERE id='alice'`).Scan(&raw); err != nil || raw != "[]" {
		t.Fatal(raw, err)
	}
	createUsageKey(t, st, usageOwner("bob"), "bob", "bob-hash")
	if err := st.SetPortalModels(ctx, "alice", []string{"first"}); err != nil {
		t.Fatal(err)
	}
	bob, err := st.GetKeyByID(ctx, "bob")
	if err != nil || len(bob.Models) != 0 {
		t.Fatal("cross-user edit", bob, err)
	}
	if err := st.SetKeyDisabled(ctx, "alice", true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RotatePortalKey(ctx, owner, 1, "new"); err != nil {
		t.Fatal(err)
	}
	stored, err = st.GetKeyByID(ctx, "alice")
	if err != nil || !reflect.DeepEqual(stored.Models, []string{"first"}) || !stored.Disabled || stored.Name != key.Name {
		t.Fatal(stored, err)
	}
	if _, err := st.GetKeyByHash(ctx, "old"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	var hashes int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM portal_key_hashes WHERE key_id='alice'`).Scan(&hashes); err != nil || hashes != 2 {
		t.Fatal(hashes, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	stored, err = st.GetKeyByID(ctx, "alice")
	if err != nil || !reflect.DeepEqual(stored.Models, []string{"first"}) {
		t.Fatal(stored, err)
	}
	if err := st.SetPortalModels(ctx, "alice", []string{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RotatePortalKey(ctx, owner, 2, "third"); err != nil {
		t.Fatal(err)
	}
	stored, err = st.GetKeyByID(ctx, "alice")
	if err != nil || stored.Models == nil || len(stored.Models) != 0 || !stored.Disabled {
		t.Fatal(stored, err)
	}
}

func TestPortalModelEditRejectsLegacyDeletedAndInvalid(t *testing.T) {
	st := usageStore(t)
	ctx := context.Background()
	if err := st.CreateKey(ctx, ManagedKey{ID: "legacy", Name: "legacy", KeyHash: "legacy"}); err != nil {
		t.Fatal(err)
	}
	createUsageKey(t, st, usageOwner("deleted"), "deleted", "deleted-hash")
	if err := st.DeleteKey(ctx, "deleted"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"legacy", "deleted", "missing"} {
		if err := st.SetPortalModels(ctx, id, []string{"chatgpt"}); !errors.Is(err, ErrNotFound) {
			t.Fatal(id, err)
		}
	}
	createUsageKey(t, st, usageOwner("alice"), "alice", "alice-hash")
	for _, models := range [][]string{nil, {""}, {" space"}, {"a", "a"}} {
		if err := st.SetPortalModels(ctx, "alice", models); err == nil {
			t.Fatal("invalid models accepted", models)
		}
	}
	legacy, err := st.GetKeyByID(ctx, "legacy")
	if err != nil || legacy.Models != nil {
		t.Fatal(legacy, err)
	}
}

func TestPortalOverviewRotationLegacyAndWindows(t *testing.T) {
	st := usageStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	owner := usageOwner("alice")
	createUsageKey(t, st, owner, "alice", "old")
	if _, err := st.RotatePortalKey(ctx, owner, 1, "new"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateKey(ctx, ManagedKey{ID: "legacy", Name: "legacy user", KeyHash: "legacy", Disabled: true}); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct {
		id, hash string
		at       time.Time
		tokens   int
	}{
		{"old", "old", now, 5}, {"new", "new", now, 7}, {"legacy", "legacy", now.Add(-30 * 24 * time.Hour), 11},
		{"expired", "old", now.Add(-30*24*time.Hour - time.Second), 1000}, {"future", "old", now.Add(time.Second), 1000}, {"unrelated", "master", now, 1000},
	} {
		if err := st.LogRequest(ctx, RequestLog{ID: entry.id, APIKeyHash: entry.hash, CreatedAt: entry.at, PromptTokens: entry.tokens, CompletionTokens: 1, TotalTokens: entry.tokens + 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.DeleteKey(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	overview, err := st.GetPortalOverview(ctx, now, PortalReportScope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(overview.Keys) != 2 || overview.Stats != (PortalStats{RequestCount: 3, PromptTokens: 23, CompletionTokens: 3, TotalTokens: 26}) {
		t.Fatal(overview)
	}
	for _, row := range overview.Keys {
		switch row.KeyID {
		case "alice":
			if row.Kind != "portal" || row.State != "revoked" || row.RevokedAt == nil || row.Stats.RequestCount != 2 || row.Stats.TotalTokens != 14 {
				t.Fatal(row)
			}
		case "legacy":
			if row.Kind != "legacy_current_hash" || row.State != "disabled" || row.Stats.RequestCount != 1 {
				t.Fatal(row)
			}
		default:
			t.Fatal(row)
		}
	}
}
