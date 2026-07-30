package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

func approxEqual(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

// usageLog builds a request log with sane usage defaults, overridable per test.
func usageLog(id string, opts ...func(*store.RequestLog)) store.RequestLog {
	rl := store.RequestLog{
		ID:               id,
		OrgID:            "default",
		APIKeyHash:       "hash-a",
		ModelRequested:   "gpt-4",
		ModelUsed:        "gpt-4o",
		Provider:         "openai",
		AuthMode:         "api_key",
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
		CostUSD:          0.01,
		Status:           "ok",
		CreatedAt:        time.Unix(1700000000, 0).UTC(),
	}
	for _, o := range opts {
		o(&rl)
	}
	return rl
}

func TestUsageReport_GroupByModel(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()
	for _, rl := range []store.RequestLog{
		usageLog("a", func(r *store.RequestLog) { r.ModelUsed = "gpt-4o"; r.TotalTokens = 100 }),
		usageLog("b", func(r *store.RequestLog) { r.ModelUsed = "gpt-4o"; r.TotalTokens = 200 }),
		usageLog("c", func(r *store.RequestLog) { r.ModelUsed = "claude-3"; r.TotalTokens = 50 }),
	} {
		if err := s.LogRequest(ctx, rl); err != nil {
			t.Fatalf("log %s: %v", rl.ID, err)
		}
	}

	rows, err := s.UsageReport(ctx, store.UsageFilter{
		OrgID: "default",
		Start: time.Unix(0, 0),
		End:   time.Unix(1800000000, 0),
		By:    []store.UsageDimension{store.DimModel},
	})
	if err != nil {
		t.Fatalf("UsageReport: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d: %+v", len(rows), rows)
	}
	// ORDER BY total_tokens DESC: gpt-4o (300) first.
	if rows[0].Key != "gpt-4o" || rows[0].TotalTokens != 300 || rows[0].Requests != 2 {
		t.Errorf("row0 = %+v, want gpt-4o/300/2", rows[0])
	}
	if rows[1].Key != "claude-3" || rows[1].TotalTokens != 50 {
		t.Errorf("row1 = %+v, want claude-3/50", rows[1])
	}
}

// TestUsageReport_DayBucketUnixepoch guards the silent-fail trap: created_at is
// INTEGER unix-seconds, so day bucketing must use strftime(..., 'unixepoch').
// Two rows straddling a UTC midnight must land in two distinct day keys.
func TestUsageReport_DayBucketUnixepoch(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()
	// 2023-11-14 23:59:00 UTC and 2023-11-15 00:01:00 UTC.
	day1 := time.Date(2023, 11, 14, 23, 59, 0, 0, time.UTC)
	day2 := time.Date(2023, 11, 15, 0, 1, 0, 0, time.UTC)
	if err := s.LogRequest(ctx, usageLog("d1", func(r *store.RequestLog) { r.CreatedAt = day1 })); err != nil {
		t.Fatal(err)
	}
	if err := s.LogRequest(ctx, usageLog("d2", func(r *store.RequestLog) { r.CreatedAt = day2 })); err != nil {
		t.Fatal(err)
	}

	rows, err := s.UsageReport(ctx, store.UsageFilter{
		OrgID: "default",
		Start: time.Unix(0, 0),
		End:   time.Unix(1800000000, 0),
		By:    []store.UsageDimension{store.DimDay},
	})
	if err != nil {
		t.Fatalf("UsageReport: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 day buckets, got %d: %+v", len(rows), rows)
	}
	// ORDER BY key ASC for day.
	if rows[0].Key != "2023-11-14" || rows[1].Key != "2023-11-15" {
		t.Errorf("day keys = %q, %q; want 2023-11-14, 2023-11-15", rows[0].Key, rows[1].Key)
	}
}

// TestUsageReport_ErrorRateCast guards the second silent-fail trap: error_rate
// must CAST(... AS REAL) or SQLite integer-division collapses it to 0/1. A
// 1-error-of-4 group must report 0.25.
func TestUsageReport_ErrorRateCast(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()
	for i, st := range []string{"ok", "ok", "ok", "error"} {
		id := string(rune('a' + i))
		if err := s.LogRequest(ctx, usageLog(id, func(r *store.RequestLog) { r.Status = st })); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.UsageReport(ctx, store.UsageFilter{
		OrgID: "default",
		Start: time.Unix(0, 0),
		End:   time.Unix(1800000000, 0),
		By:    []store.UsageDimension{store.DimModel},
	})
	if err != nil {
		t.Fatalf("UsageReport: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].ErrorRate != 0.25 {
		t.Errorf("error_rate = %v, want 0.25", rows[0].ErrorRate)
	}
}

// TestUsageReport_CostBillableSplit confirms CostUSD covers all statuses while
// CostUSDBillable only counts status='ok'.
func TestUsageReport_CostBillableSplit(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()
	if err := s.LogRequest(ctx, usageLog("ok", func(r *store.RequestLog) { r.CostUSD = 0.10; r.Status = "ok" })); err != nil {
		t.Fatal(err)
	}
	if err := s.LogRequest(ctx, usageLog("err", func(r *store.RequestLog) { r.CostUSD = 0.05; r.Status = "error" })); err != nil {
		t.Fatal(err)
	}
	rows, err := s.UsageReport(ctx, store.UsageFilter{
		OrgID: "default", Start: time.Unix(0, 0), End: time.Unix(1800000000, 0),
		By: []store.UsageDimension{store.DimModel},
	})
	if err != nil {
		t.Fatalf("UsageReport: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if got := rows[0].CostUSD; !approxEqual(got, 0.15) {
		t.Errorf("cost_usd = %v, want 0.15", got)
	}
	if got := rows[0].CostUSDBillable; !approxEqual(got, 0.10) {
		t.Errorf("cost_usd_billable = %v, want 0.10", got)
	}
}

// TestUsageReport_WindowAndOrgScope confirms the window is half-open [start,end)
// and other orgs are excluded.
func TestUsageReport_WindowAndOrgScope(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()
	base := time.Unix(1700000000, 0).UTC()
	rows := []store.RequestLog{
		usageLog("in", func(r *store.RequestLog) { r.CreatedAt = base }),
		usageLog("other-org", func(r *store.RequestLog) { r.OrgID = "other"; r.CreatedAt = base }),
		usageLog("too-old", func(r *store.RequestLog) { r.CreatedAt = base.Add(-time.Hour) }),
	}
	for _, rl := range rows {
		if err := s.LogRequest(ctx, rl); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.UsageReport(ctx, store.UsageFilter{
		OrgID: "default",
		Start: base,
		End:   base.Add(time.Minute),
		By:    []store.UsageDimension{store.DimModel},
	})
	if err != nil {
		t.Fatalf("UsageReport: %v", err)
	}
	if len(got) != 1 || got[0].Requests != 1 {
		t.Fatalf("want exactly the single in-window default-org row, got %+v", got)
	}
}

func TestUsageReport_RejectsBadDimensionCount(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	_, err := s.UsageReport(context.Background(), store.UsageFilter{
		OrgID: "default", Start: time.Unix(0, 0), End: time.Unix(1, 0),
		By: []store.UsageDimension{store.DimModel, store.DimDay},
	})
	if err == nil {
		t.Fatal("want error for multi-dimension group-by, got nil")
	}
}

func TestParseDimension(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"model", "provider", "api_key", "auth_mode", "day"} {
		if _, err := store.ParseDimension(in); err != nil {
			t.Errorf("ParseDimension(%q) errored: %v", in, err)
		}
	}
	// The injection boundary: anything off the allowlist is rejected, never
	// interpolated into SQL.
	for _, bad := range []string{"", "created_at; DROP TABLE request_logs", "MODEL", "org"} {
		if _, err := store.ParseDimension(bad); err == nil {
			t.Errorf("ParseDimension(%q) accepted a non-allowlisted dimension", bad)
		}
	}
}

func TestParseSince(t *testing.T) {
	t.Parallel()
	cases := map[string]time.Duration{
		"7d":  7 * 24 * time.Hour,
		"24h": 24 * time.Hour,
		"30m": 30 * time.Minute,
		"0d":  0,
	}
	for in, want := range cases {
		got, err := store.ParseSince(in)
		if err != nil {
			t.Errorf("ParseSince(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseSince(%q) = %v, want %v", in, got, want)
		}
	}
	for _, bad := range []string{"", "7", "-3d", "-1h", "abc", "1w"} {
		if _, err := store.ParseSince(bad); err == nil {
			t.Errorf("ParseSince(%q) accepted invalid input", bad)
		}
	}
}

// TestOpenSQLiteReadOnly confirms a report can read a server-written DB without
// write access and without mutating schema.
func TestOpenSQLiteReadOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "ro.db")

	// Server writes some rows, then closes (checkpointing the WAL).
	wr, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	ctx := context.Background()
	if err := wr.LogRequest(ctx, usageLog("x")); err != nil {
		t.Fatal(err)
	}
	if err := wr.Close(); err != nil {
		t.Fatal(err)
	}

	ro, err := store.OpenSQLiteReadOnly(path)
	if err != nil {
		t.Fatalf("OpenSQLiteReadOnly: %v", err)
	}
	defer ro.Close()

	rows, err := ro.UsageReport(ctx, store.UsageFilter{
		OrgID: "default", Start: time.Unix(0, 0), End: time.Unix(1800000000, 0),
		By: []store.UsageDimension{store.DimModel},
	})
	if err != nil {
		t.Fatalf("read-only UsageReport: %v", err)
	}
	if len(rows) != 1 || rows[0].Requests != 1 {
		t.Fatalf("want one row read back, got %+v", rows)
	}

	// A write through the read-only handle must fail (proves mode=ro took hold).
	if err := ro.LogRequest(ctx, usageLog("y")); err == nil {
		t.Fatal("expected write to fail on a read-only handle")
	}
}

func TestOpenSQLiteReadOnly_MissingDB(t *testing.T) {
	t.Parallel()
	_, err := store.OpenSQLiteReadOnly(filepath.Join(t.TempDir(), "nope.db"))
	if err == nil {
		t.Fatal("want a friendly error for a missing DB, got nil")
	}
}
