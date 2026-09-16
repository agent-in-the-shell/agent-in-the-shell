package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func timezoneInstant(t *testing.T, value string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func TestPortalMonitoringTaipeiBoundaries(t *testing.T) {
	// File-backed storage exercises the separate reporting reader as well as SQL grouping.
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "timezone.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	owner := usageOwner("alice")
	createUsageKey(t, st, owner, "alice", "old")
	if _, err := st.RotatePortalKey(ctx, owner, 1, "new"); err != nil {
		t.Fatal(err)
	}
	for i, at := range []string{"2024-12-31T15:59:59Z", "2024-12-31T16:00:00Z", "2025-01-01T15:59:59Z", "2025-01-01T16:00:00Z"} {
		if err := st.LogRequest(ctx, RequestLog{ID: at, APIKeyHash: []string{"old", "new"}[i%2], CreatedAt: timezoneInstant(t, at), TotalTokens: i + 1}); err != nil {
			t.Fatal(err)
		}
	}
	f := PortalMonitorFilter{Since: timezoneInstant(t, "2024-12-31T15:59:59Z"), Until: timezoneInstant(t, "2025-01-01T16:00:00Z"), Bucket: "day", Page: 1, PageSize: 2}
	// JSON also allows this regression to run against the original UTC-only implementation.
	if err := json.Unmarshal([]byte(`{"timezone":"Asia/Taipei"}`), &f); err != nil {
		t.Fatal(err)
	}
	full, err := st.GetPortalMonitoring(ctx, f, PortalReportScope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Buckets) != 2 || full.Buckets[0].Start.Format(time.RFC3339) != "2024-12-30T16:00:00Z" || full.Buckets[1].Start.Format(time.RFC3339) != "2024-12-31T16:00:00Z" {
		t.Fatalf("Taipei midnight buckets: %+v", full.Buckets)
	}
	if full.Buckets[0].Stats.RequestCount != 1 || full.Buckets[1].Stats.RequestCount != 2 || full.Stats.RequestCount != 3 || full.Stats.TotalTokens != 6 {
		t.Fatalf("half-open range or rotation history lost: %+v", full)
	}
	for _, view := range []string{"summary", "requests", "options"} {
		f.View = view
		got, err := st.GetPortalMonitoring(ctx, f, PortalReportScope{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Filter != f {
			t.Fatal("projection changed filter", got.Filter)
		}
		switch view {
		case "summary":
			if !reflect.DeepEqual(got.Buckets, full.Buckets) || !reflect.DeepEqual(got.Stats, full.Stats) || !reflect.DeepEqual(got.Models, full.Models) || !reflect.DeepEqual(got.Users, full.Users) {
				t.Fatal("summary/full mismatch")
			}
		case "requests":
			if got.RequestCount != 3 || !reflect.DeepEqual(got.Requests, full.Requests) {
				t.Fatal("requests/full mismatch", got)
			}
			f.Page = 2
			page, err := st.GetPortalMonitoring(ctx, f, PortalReportScope{})
			if err != nil || page.Filter != f || page.RequestCount != 3 || len(page.Requests) != 1 || page.Requests[0].ID != "2024-12-31T15:59:59Z" {
				t.Fatal("frozen page mismatch", page, err)
			}
			if page.Requests[0].ReasoningTokens != nil || page.Requests[0].PricedCostUSD != nil {
				t.Fatal("unknown converted to zero")
			}
			f.Page = 1
		case "options":
			if !reflect.DeepEqual(got.Options, full.Options) {
				t.Fatal("options/full mismatch")
			}
		}
	}
	// UTC remains the default; changing the reporting zone must not change totals or rows.
	f.View, f.Timezone = "", ""
	utc, err := st.GetPortalMonitoring(ctx, f, PortalReportScope{})
	if err != nil || !reflect.DeepEqual(utc.Stats, full.Stats) || !reflect.DeepEqual(utc.Requests, full.Requests) || utc.Buckets[0].Start.Hour() != 0 {
		t.Fatal("UTC default or instant scope changed", utc, err)
	}
	f.Timezone = "Asia/Taipei"
	f.Since = timezoneInstant(t, "2024-12-29T16:00:00Z")
	emptyDays, err := st.GetPortalMonitoring(ctx, f, PortalReportScope{})
	if err != nil || len(emptyDays.Buckets) != 3 || emptyDays.Buckets[0].Start != f.Since || emptyDays.Buckets[0].Stats.RequestCount != 0 || emptyDays.Buckets[0].Stats.MeanLatencyMs != nil {
		t.Fatal("Taipei daily zero-fill", emptyDays, err)
	}
	personal, err := st.GetPortalUsage(ctx, owner, f.Until.Add(-time.Second), "Asia/Taipei")
	if err != nil || personal.Calendar.Year != 2025 || *personal.Calendar.Days[0].Requests != 2 || personal.Stats != full.Stats.PortalStats {
		t.Fatal("separate reader personal/monitoring parity", personal, err)
	}
	f.Since = full.Filter.Since
	f.Bucket = "hour"
	hours, err := st.GetPortalMonitoring(ctx, f, PortalReportScope{})
	if err != nil || len(hours.Buckets) != 25 || hours.Buckets[0].Start.Format(time.RFC3339) != "2024-12-31T15:00:00Z" || hours.Buckets[2].Stats.RequestCount != 0 || hours.Buckets[2].Stats.MeanLatencyMs != nil {
		t.Fatal("hour grouping/zero/null changed", hours, err)
	}
}

func TestPortalUsageTaipeiYearLeapAndFuture(t *testing.T) {
	st := usageStore(t)
	ctx := context.Background()
	owner := usageOwner("alice")
	createUsageKey(t, st, owner, "alice", "old")
	if _, err := st.RotatePortalKey(ctx, owner, 1, "new"); err != nil {
		t.Fatal(err)
	}
	for _, at := range []string{"2024-02-28T15:59:59Z", "2024-02-28T16:00:00Z", "2024-02-29T15:59:59Z", "2024-02-29T16:00:00Z", "2024-12-01T15:59:59Z", "2024-12-01T16:00:00Z", "2024-12-31T15:59:59Z", "2024-12-31T16:00:00Z", "2024-12-31T16:00:01Z"} {
		if err := st.LogRequest(ctx, RequestLog{ID: at, APIKeyHash: "old", CreatedAt: timezoneInstant(t, at), TotalTokens: 7}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		now, today string
		year, days int
		requests   int64
	}{
		{"2024-02-28T15:59:59Z", "2024-02-28", 2024, 366, 1},
		{"2024-02-28T16:00:00Z", "2024-02-29", 2024, 366, 1},
		{"2024-02-29T15:59:59Z", "2024-02-29", 2024, 366, 2},
		{"2024-02-29T16:00:00Z", "2024-03-01", 2024, 366, 1},
		{"2024-12-31T15:59:59Z", "2024-12-31", 2024, 366, 1},
		{"2024-12-31T16:00:00Z", "2025-01-01", 2025, 365, 1},
	} {
		t.Run(tc.now, func(t *testing.T) {
			now := timezoneInstant(t, tc.now)
			got, err := st.GetPortalUsage(ctx, owner, now, "Asia/Taipei")
			if err != nil {
				t.Fatal(err)
			}
			if got.Calendar.Timezone != "Asia/Taipei" || got.Calendar.Year != tc.year || len(got.Calendar.Days) != tc.days {
				t.Fatal("wrong calendar", got.Calendar)
			}
			utc, err := st.GetPortalUsage(ctx, owner, now)
			if err != nil || got.Stats != utc.Stats {
				t.Fatal("rolling duration changed", got.Stats, utc.Stats, err)
			}
			if tc.year == 2025 && (got.Stats.RequestCount != 3 || got.Stats.TotalTokens != 21) {
				t.Fatal("cutoff/current second inclusion or future exclusion changed", got.Stats)
			}
			for _, day := range got.Calendar.Days {
				if day.Date > tc.today {
					if day.Requests != nil || day.TotalTokens != nil {
						t.Fatal("future not null", day)
					}
				} else if day.Requests == nil || day.TotalTokens == nil {
					t.Fatal("past/current not zero-filled", day)
				} else if day.Date == tc.today && (*day.Requests != tc.requests || *day.TotalTokens != 7*tc.requests) {
					t.Fatal("wrong Taipei day aggregate", day)
				}
			}
		})
	}
	if _, err := st.GetPortalUsage(ctx, owner, time.Now(), "America/Los_Angeles"); err == nil {
		t.Fatal("accepted unsupported timezone")
	}
}
