package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestPortalMonitoringFiltersHistoryAndCoverage(t *testing.T) {
	st := usageStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	owner := usageOwner("alice")
	createUsageKey(t, st, owner, "alice", "old-secret-hash")
	createUsageKey(t, st, usageOwner("bob"), "bob", "bob-secret-hash")
	if _, err := st.RotatePortalKey(ctx, owner, 1, "new-secret-hash"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateKey(ctx, ManagedKey{ID: "legacy", Name: "Legacy", KeyHash: "legacy-secret-hash"}); err != nil {
		t.Fatal(err)
	}
	// Administrative imports can reuse a no-longer-current portal hash; history
	// ownership wins so an old request cannot be counted for two owners.
	if err := st.CreateKey(ctx, ManagedKey{ID: "reused", Name: "Reused old hash", KeyHash: "old-secret-hash"}); err != nil {
		t.Fatal(err)
	}
	zero, three := 0, 3
	// The old-hash record arrives after rotation, just like a late streaming log.
	logs := []RequestLog{
		{ID: "old", APIKeyHash: "old-secret-hash", ModelRequested: "m", ModelUsed: "upstream", Provider: "p", AuthMode: "subscription", CostSource: "subscription", Status: "ok", ReasoningTokens: &zero, LatencyMs: 10},
		{ID: "new", APIKeyHash: "new-secret-hash", ModelRequested: "m", Provider: "p", AuthMode: "api_key", CostSource: "priced", CostUSD: 0.2, Status: "error", ReasoningTokens: &three, LatencyMs: 20, ErrorType: "private upstream body"},
		{ID: "bob", APIKeyHash: "bob-secret-hash", ModelRequested: "other", Provider: "q", AuthMode: "api_key", CostSource: "unpriced", Status: "ok", LatencyMs: 30},
		{ID: "cache", APIKeyHash: "new-secret-hash", ModelRequested: "m", Provider: "p", AuthMode: "subscription", CostSource: "cache", Status: "ok", LatencyMs: 40},
		{ID: "legacy", APIKeyHash: "legacy-secret-hash", ModelRequested: "m", Provider: "p", AuthMode: "api_key", Status: "ok", LatencyMs: 50},
		{ID: "master", APIKeyHash: "master-secret-hash", ModelRequested: "m", Provider: "p", AuthMode: "api_key", CostSource: "priced", CostUSD: 900, Status: "ok"},
	}
	for i, log := range logs {
		log.CreatedAt = now.Add(-time.Duration(i+1) * time.Hour)
		log.PromptTokens, log.CompletionTokens, log.TotalTokens = 10, 5, 15
		log.CacheReadInputTokens, log.CacheCreationInputTokens = 4, 2
		if err := st.LogRequest(ctx, log); err != nil {
			t.Fatal(err)
		}
	}
	for _, at := range []time.Time{now.Add(-48*time.Hour - time.Second), now} {
		if err := st.LogRequest(ctx, RequestLog{ID: at.String(), APIKeyHash: "new-secret-hash", CreatedAt: at, TotalTokens: 900}); err != nil {
			t.Fatal(err)
		}
	}
	f := PortalMonitorFilter{Since: now.Add(-48 * time.Hour), Until: now, Bucket: "day", Page: 1, PageSize: 2}
	report, err := st.GetPortalMonitoring(ctx, f, PortalReportScope{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Stats.RequestCount != 5 || report.Stats.TotalTokens != 75 || len(report.Requests) != 2 || report.Stats.ErrorCount != 1 {
		t.Fatalf("%+v", report)
	}
	if report.Stats.ReasoningKnownRequests != 2 || report.Stats.ReasoningTokens != 3 || report.Stats.CacheReadTokens != 20 || report.Stats.CacheCreationTokens != 10 {
		t.Fatal(report.Stats)
	}
	if report.Stats.SubscriptionRequests != 1 || report.Stats.PricedRequests != 1 || report.Stats.UnpricedRequests != 1 || report.Stats.ResponseCacheRequests != 1 || report.Stats.UnknownCostRequests != 1 || report.Stats.PricedCostUSD != .2 {
		t.Fatal(report.Stats)
	}
	if report.Stats.MeanLatencyMs == nil || *report.Stats.MeanLatencyMs != 30 || report.P95LatencyMs == nil || *report.P95LatencyMs != 50 {
		t.Fatal(report.Stats, report.P95LatencyMs)
	}
	if len(report.Buckets) != 3 || report.Buckets[0].Stats.RequestCount != 0 || report.Buckets[1].Stats.RequestCount != 0 || report.Buckets[2].Stats.RequestCount != 5 {
		t.Fatal(report.Buckets)
	}
	raw, _ := json.Marshal(report)
	for _, secret := range []string{"secret-hash", "private upstream body", "error_type", "api_key_hash", "request_id"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("leaked %s: %s", secret, raw)
		}
	}
	checkPortalProjections(t, st, f, report)
	f.Page = 2
	second, err := st.GetPortalMonitoring(ctx, f, PortalReportScope{})
	if err != nil || second.Stats.RequestCount != 5 || len(second.Requests) != 2 || second.Requests[0].ID == report.Requests[0].ID {
		t.Fatal(second, err)
	}
	checkPortalProjections(t, st, f, second)
	f.Page = 9
	empty, err := st.GetPortalMonitoring(ctx, f, PortalReportScope{})
	if err != nil || empty.Stats.RequestCount != 5 || len(empty.Requests) != 0 {
		t.Fatal(empty, err)
	}
	checkPortalProjections(t, st, f, empty)
	f.Page = 1
	filters := []struct {
		name  string
		apply func(*PortalMonitorFilter)
		want  int64
	}{
		{"key", func(f *PortalMonitorFilter) { f.KeyID = "alice" }, 3},
		{"user", func(f *PortalMonitorFilter) { f.User = owner.Email }, 3},
		{"model", func(f *PortalMonitorFilter) { f.Model = "other" }, 1},
		{"provider", func(f *PortalMonitorFilter) { f.Provider = "q" }, 1},
		{"auth", func(f *PortalMonitorFilter) { f.AuthMode = "subscription" }, 2},
		{"status", func(f *PortalMonitorFilter) { f.Status = "error" }, 1},
		{"injection", func(f *PortalMonitorFilter) { f.Model = "' OR 1=1 --" }, 0},
		{"combined", func(f *PortalMonitorFilter) {
			f.KeyID = "alice"
			f.Model = "m"
			f.Provider = "p"
			f.AuthMode = "api_key"
			f.Status = "error"
		}, 1},
	}
	for _, tc := range filters {
		t.Run(tc.name, func(t *testing.T) {
			query := f
			query.PageSize = 100
			tc.apply(&query)
			got, err := st.GetPortalMonitoring(ctx, query, PortalReportScope{})
			if err != nil {
				t.Fatal(err)
			}
			checkPortalProjections(t, st, query, got)
			var buckets, models, users int64
			for _, b := range got.Buckets {
				buckets += b.Stats.RequestCount
			}
			for _, b := range got.Models {
				models += b.Stats.RequestCount
			}
			for _, b := range got.Users {
				users += b.Stats.RequestCount
			}
			if got.Stats.RequestCount != tc.want || buckets != tc.want || models != tc.want || users != tc.want || int64(len(got.Requests)) != tc.want {
				t.Fatal(got)
			}
		})
	}
	if err := st.DeleteKey(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	f.KeyID = "alice"
	tombstone, err := st.GetPortalMonitoring(ctx, f, PortalReportScope{})
	if err != nil || tombstone.Stats.RequestCount != 3 {
		t.Fatal(tombstone, err)
	}
	checkPortalProjections(t, st, f, tombstone)
}

func TestPortalMonitoringHourlyPaginationAndInvalidBounds(t *testing.T) {
	st := usageStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 8, 12, 30, 0, 0, time.UTC)
	createUsageKey(t, st, usageOwner("alice"), "alice", "hash")
	for i := 0; i < 105; i++ {
		if err := st.LogRequest(ctx, RequestLog{ID: fmt.Sprintf("r-%03d", i), APIKeyHash: "hash", CreatedAt: now.Add(-time.Hour), AuthMode: "subscription", CostSource: "priced", CostUSD: 5, ReasoningTokens: nil}); err != nil {
			t.Fatal(err)
		}
	}
	f := PortalMonitorFilter{Since: now.Add(-2 * time.Hour), Until: now, Bucket: "hour", Page: 2, PageSize: 100}
	got, err := st.GetPortalMonitoring(ctx, f, PortalReportScope{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Stats.RequestCount != 105 || len(got.Requests) != 5 || got.Stats.PricedCostUSD != 0 || got.Stats.SubscriptionRequests != 105 || got.Stats.ReasoningKnownRequests != 0 {
		t.Fatal(got)
	}
	if len(got.Buckets) != 3 || got.Buckets[0].Stats.RequestCount != 0 || got.Buckets[1].Stats.RequestCount != 105 || got.Buckets[2].Stats.RequestCount != 0 {
		t.Fatal(got.Buckets)
	}
	if got.Requests[0].ID != "r-004" || got.Requests[4].ID != "r-000" || got.Requests[0].ReasoningTokens != nil || got.Requests[0].PricedCostUSD != nil {
		t.Fatal(got.Requests)
	}
	checkPortalProjections(t, st, f, got)
	for _, mutate := range []func(*PortalMonitorFilter){
		func(f *PortalMonitorFilter) { f.View = "bogus" }, func(f *PortalMonitorFilter) { f.Page = 0 }, func(f *PortalMonitorFilter) { f.PageSize = 101 }, func(f *PortalMonitorFilter) { f.Until = f.Since },
		func(f *PortalMonitorFilter) { f.Bucket = "week" }, func(f *PortalMonitorFilter) { f.Since = f.Until.Add(-32 * 24 * time.Hour) },
		func(f *PortalMonitorFilter) { f.Bucket = "day"; f.Since = f.Until.Add(-367 * 24 * time.Hour) }, func(f *PortalMonitorFilter) { f.Status = "500" },
	} {
		bad := f
		mutate(&bad)
		if _, err := st.GetPortalMonitoring(ctx, bad, PortalReportScope{}); err == nil {
			t.Fatal("accepted invalid filter", bad)
		}
	}
}

func TestPortalDisableOnlyPortalAndPreservesPolicy(t *testing.T) {
	st := usageStore(t)
	ctx := context.Background()
	createUsageKey(t, st, usageOwner("alice"), "alice", "a")
	if err := st.SetPortalModels(ctx, "alice", []string{"m"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateKey(ctx, ManagedKey{ID: "legacy", KeyHash: "l"}); err != nil {
		t.Fatal(err)
	}
	for _, disabled := range []bool{true, false} {
		if err := st.SetPortalDisabled(ctx, "alice", disabled); err != nil {
			t.Fatal(err)
		}
		key, err := st.GetKeyByID(ctx, "alice")
		if err != nil || key.Disabled != disabled || len(key.Models) != 1 || key.Models[0] != "m" || key.KeyHash != "a" {
			t.Fatal(key, err)
		}
		for _, id := range []string{"legacy", "missing"} {
			if err := st.SetPortalDisabled(ctx, id, disabled); !errors.Is(err, ErrNotFound) {
				t.Fatal(id, err)
			}
		}
	}
	if err := st.DeleteKey(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetPortalDisabled(ctx, "alice", false); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

// Compare the projections at the store interface across the same attribution,
// filters, empty ranges, tied timestamps and null/cost fixtures as the full report.
func checkPortalProjections(t *testing.T, st *SQLiteStore, f PortalMonitorFilter, full PortalMonitoring) {
	t.Helper()
	f.View = "requests"
	page, err := st.GetPortalMonitoring(context.Background(), f, PortalReportScope{})
	if err != nil {
		t.Fatal(err)
	}
	if page.RequestCount != full.Stats.RequestCount || !reflect.DeepEqual(page.Requests, full.Requests) {
		t.Fatalf("page differs from full: %+v vs %+v", page, full)
	}
	f.View = "summary"
	summary, err := st.GetPortalMonitoring(context.Background(), f, PortalReportScope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Requests) != 0 {
		t.Fatal("summary contains metadata")
	}
	want := full
	want.Filter = f
	want.Requests = []PortalMonitorRequest{}
	if !reflect.DeepEqual(summary, want) {
		t.Fatalf("summary differs: %+v vs %+v", summary, want)
	}
	f.View = "options"
	options, err := st.GetPortalMonitoring(context.Background(), f, PortalReportScope{})
	if err != nil {
		t.Fatal(err)
	}
	expected := full.Options
	expected.Users = append([]PortalMonitorOwner{}, expected.Users...)
	sort.Slice(expected.Users, func(i, j int) bool { return expected.Users[i].ID < expected.Users[j].ID })
	if !reflect.DeepEqual(options.Options, expected) {
		t.Fatalf("options differ: %+v vs %+v", options.Options, expected)
	}
}
