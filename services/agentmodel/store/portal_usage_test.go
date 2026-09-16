package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func usageStore(t *testing.T) *SQLiteStore {
	t.Helper()
	st, err := OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func usageOwner(subject string) PortalIdentity {
	return PortalIdentity{Issuer: "issuer", Subject: subject, Email: subject + "@example.com"}
}

func createUsageKey(t *testing.T, st *SQLiteStore, owner PortalIdentity, id, hash string) {
	t.Helper()
	if _, err := st.CreatePortalKey(context.Background(), owner, ManagedKey{ID: id, Name: owner.Email, KeyHash: hash, Models: []string{"chatgpt"}}); err != nil {
		t.Fatal(err)
	}
}

func TestPortalUsageWindowsIsolationAndRotation(t *testing.T) {
	st := usageStore(t)
	ctx := context.Background()
	now := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-30 * 24 * time.Hour)
	alice, bob := usageOwner("alice"), usageOwner("bob")
	createUsageKey(t, st, alice, "alice-id", "alice-0")
	createUsageKey(t, st, bob, "bob-id", "bob-hash")
	log := func(id, hash string, at time.Time, prompt, completion, total int) {
		t.Helper()
		if err := st.LogRequest(ctx, RequestLog{ID: id, APIKeyHash: hash, CreatedAt: at, PromptTokens: prompt, CompletionTokens: completion, TotalTokens: total, CacheReadInputTokens: 800, CacheCreationInputTokens: 900, Status: "error"}); err != nil {
			t.Fatal(err)
		}
	}
	log("outside-30", "alice-0", cutoff.Add(-time.Second), 100, 200, 400)
	log("cutoff", "alice-0", cutoff, 2, 3, 9)
	log("previous-year", "alice-0", time.Date(2023, 12, 31, 23, 59, 59, 0, time.UTC), 900, 900, 900)
	for i, hash := range []string{"alice-1", "alice-2"} {
		if _, err := st.RotatePortalKey(ctx, alice, int64(i+1), hash); err != nil {
			t.Fatal(err)
		}
	}
	// UTC bucketing, not the input location; Feb 29 23:59:59 UTC and Mar 1 UTC.
	zone := time.FixedZone("UTC+8", 8*60*60)
	log("late-old", "alice-0", time.Date(2024, 3, 1, 7, 59, 59, 0, zone), 5, 7, 17)
	log("middle-hash", "alice-1", time.Date(2024, 3, 1, 8, 0, 0, 0, zone), 11, 13, 29)
	log("current", "alice-2", now, 19, 23, 47)
	log("future-today", "alice-2", now.Add(time.Second), 900, 900, 900)
	log("future-day", "alice-2", now.AddDate(0, 0, 1), 900, 900, 900)
	log("bob", "bob-hash", now, 1000, 2000, 4000)
	// Display-email changes must not change attribution.
	alice.Email = bob.Email
	got, err := st.GetPortalUsage(ctx, alice, now.In(zone))
	if err != nil {
		t.Fatal(err)
	}
	want := PortalStats{RequestCount: 4, PromptTokens: 37, CompletionTokens: 46, TotalTokens: 102}
	if got.Stats != want {
		t.Fatalf("stats = %+v, want %+v", got.Stats, want)
	}
	if got.Calendar.Year != 2024 || got.Calendar.Timezone != "UTC" || len(got.Calendar.Days) != 366 {
		t.Fatalf("bad leap-year calendar: %+v", got.Calendar)
	}
	var total, requests int64
	for i, day := range got.Calendar.Days {
		date := time.Date(2024, 1, 1+i, 0, 0, 0, 0, time.UTC)
		if day.Date != date.Format("2006-01-02") {
			t.Fatalf("day %d: %s", i, day.Date)
		}
		if day.Date > "2024-03-01" {
			if day.TotalTokens != nil || day.Requests != nil {
				t.Fatalf("future day presented as usage: %+v", day)
			}
			continue
		}
		if day.TotalTokens == nil || day.Requests == nil {
			t.Fatalf("missing past/current day %s", day.Date)
		}
		total += *day.TotalTokens
		requests += *day.Requests
		wantTokens, wantRequests := int64(0), int64(0)
		switch day.Date {
		case cutoff.Format("2006-01-02"):
			wantTokens, wantRequests = 409, 2
		case "2024-02-29":
			wantTokens, wantRequests = 17, 1
		case "2024-03-01":
			wantTokens, wantRequests = 76, 2
		}
		if *day.TotalTokens != wantTokens || *day.Requests != wantRequests {
			t.Fatalf("%s = %d tokens/%d requests, want %d/%d", day.Date, *day.TotalTokens, *day.Requests, wantTokens, wantRequests)
		}
	}
	if total != got.Stats.TotalTokens+400 || requests != got.Stats.RequestCount+1 {
		t.Fatal("calendar sums disagree with covered records", total, requests)
	}
	other, err := st.GetPortalUsage(ctx, bob, now)
	if err != nil || other.Stats != (PortalStats{RequestCount: 1, PromptTokens: 1000, CompletionTokens: 2000, TotalTokens: 4000}) {
		t.Fatal("other owner", other.Stats, err)
	}
	if day := other.Calendar.Days[60]; day.TotalTokens == nil || *day.TotalTokens != 4000 || *day.Requests != 1 {
		t.Fatal("other calendar", day)
	}
	for _, hash := range []string{"alice-0", "alice-1"} {
		if _, err := st.GetKeyByHash(ctx, hash); !errors.Is(err, ErrNotFound) {
			t.Fatalf("history authenticates: %s: %v", hash, err)
		}
	}
	for _, owner := range []PortalIdentity{usageOwner("unissued"), {Issuer: "other-issuer", Subject: alice.Subject, Email: alice.Email}} {
		zero, err := st.GetPortalUsage(ctx, owner, now)
		if err != nil || zero.Stats != (PortalStats{}) {
			t.Fatal("unbound identity sees usage", zero.Stats, err)
		}
		for _, day := range zero.Calendar.Days {
			if day.TotalTokens != nil && (*day.TotalTokens != 0 || *day.Requests != 0) {
				t.Fatal("unbound identity calendar", day)
			}
		}
	}
}

func TestPortalUsageNewYearAndEmptyCalendar(t *testing.T) {
	st := usageStore(t)
	ctx := context.Background()
	owner := usageOwner("alice")
	createUsageKey(t, st, owner, "id", "hash")
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := st.LogRequest(ctx, RequestLog{ID: "last-year", APIKeyHash: "hash", CreatedAt: now.Add(-time.Second), TotalTokens: 42}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetPortalUsage(ctx, owner, now)
	if err != nil || got.Stats.RequestCount != 1 || got.Stats.TotalTokens != 42 {
		t.Fatal(got.Stats, err)
	}
	if got.Calendar.Year != 2025 || len(got.Calendar.Days) != 365 {
		t.Fatal("non-leap year", got.Calendar)
	}
	if day := got.Calendar.Days[0]; day.Date != "2025-01-01" || day.TotalTokens == nil || *day.TotalTokens != 0 || *day.Requests != 0 {
		t.Fatal("today missing zero", day)
	}
	for _, day := range got.Calendar.Days[1:] {
		if day.Requests != nil || day.TotalTokens != nil {
			t.Fatal("future not blank", day)
		}
	}
}

func TestPortalHistoryBackfillAndRollback(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "portal.db")
	st, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { st.Close() }()
	alice, bob := usageOwner("alice"), usageOwner("bob")
	createUsageKey(t, st, alice, "alice", "old")
	// Simulate the pre-history portal schema, retaining only a current binding.
	if _, err := st.db.Exec(`DROP TABLE portal_key_hashes`); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		st, err = OpenSQLite(path)
		if err != nil {
			t.Fatal(err)
		}
		var n int
		if err := st.db.QueryRow(`SELECT COUNT(*) FROM portal_key_hashes WHERE key_id = 'alice' AND key_hash = 'old'`).Scan(&n); err != nil || n != 1 {
			t.Fatal("backfill not idempotent", n, err)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
	}
	st, err = OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	// Failure after inserting history must roll back history, binding and key.
	if _, err := st.db.Exec(`CREATE TRIGGER fail_history AFTER INSERT ON portal_key_hashes WHEN NEW.key_hash = 'fail' BEGIN SELECT RAISE(ABORT, 'test history failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePortalKey(ctx, bob, ManagedKey{ID: "bob", Name: bob.Email, KeyHash: "fail", Models: []string{"chatgpt"}}); err == nil {
		t.Fatal("create ignored history error")
	}
	if _, err := st.GetPortalBinding(ctx, bob); !errors.Is(err, ErrNotFound) {
		t.Fatal("orphan binding", err)
	}
	if _, err := st.GetKeyByID(ctx, "bob"); !errors.Is(err, ErrNotFound) {
		t.Fatal("orphan key", err)
	}
	if _, err := st.RotatePortalKey(ctx, alice, 1, "fail"); err == nil {
		t.Fatal("rotate ignored history error")
	}
	binding, err := st.GetPortalBinding(ctx, alice)
	if err != nil || binding.Revision != 1 {
		t.Fatal("rotation not rolled back", binding, err)
	}
	if _, err := st.GetKeyByHash(ctx, "old"); err != nil {
		t.Fatal("lost current hash", err)
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM portal_key_hashes`).Scan(&n); err != nil || n != 1 {
		t.Fatal("orphan history", n, err)
	}
	if _, err := st.RotatePortalKey(ctx, alice, 1, "new"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePortalKey(ctx, bob, ManagedKey{ID: "bob", Name: bob.Email, KeyHash: "old", Models: []string{"chatgpt"}}); err == nil {
		t.Fatal("historical hash reassigned to another owner")
	}
}
