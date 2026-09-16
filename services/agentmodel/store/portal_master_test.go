package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPortalMasterAttributionProjections(t *testing.T) {
	ctx := context.Background()
	st := usageStore(t)
	now := time.Date(2026, 9, 8, 18, 0, 0, 0, time.UTC)
	scope := NewPortalReportScope("current-private-hash")
	createUsageKey(t, st, usageOwner("alice"), "alice", "employee-hash")
	if err := st.CreateKey(ctx, ManagedKey{ID: "legacy", Name: "Master key", KeyHash: "legacy-hash"}); err != nil {
		t.Fatal(err)
	}
	for i, h := range []string{"employee-hash", "legacy-hash", "current-private-hash", "current-private-hash", "unknown", ""} {
		if err := st.LogRequest(ctx, RequestLog{ID: fmt.Sprint(i), APIKeyHash: h, CreatedAt: now.Add(-time.Duration(i+1) * time.Hour), ModelRequested: "m", Provider: "p", AuthMode: "api_key", Status: "ok", TotalTokens: 10, LatencyMs: 20, CostSource: "priced", CostUSD: 0.1}); err != nil {
			t.Fatal(err)
		}
	}
	// This log predates the default rolling window but is still attributable to
	// the same current credential when an operator expands the report range.
	old := now.Add(-40 * 24 * time.Hour)
	if err := st.LogRequest(ctx, RequestLog{ID: "old", APIKeyHash: "current-private-hash", CreatedAt: old, ModelRequested: "older", Status: "error"}); err != nil {
		t.Fatal(err)
	}
	f := PortalMonitorFilter{Since: now.Add(-24 * time.Hour), Until: now, Bucket: "day", Page: 1, PageSize: 1, Timezone: "Asia/Taipei"}
	for _, key := range []string{"", PortalMasterKeyID, "alice", "legacy", "missing"} {
		f.KeyID = key
		want := map[string]int64{"": 4, PortalMasterKeyID: 2, "alice": 1, "legacy": 1, "missing": 0}[key]
		full, err := st.GetPortalMonitoring(ctx, f, scope)
		if err != nil {
			t.Fatal(err)
		}
		if full.Stats.RequestCount != want {
			t.Fatalf("key %q count %d want %d", key, full.Stats.RequestCount, want)
		}
		if full.Scope != "retained_managed_keys_and_current_master" {
			t.Fatal(full.Scope)
		}
		for _, view := range []string{"summary", "requests", "options"} {
			q := f
			q.View = view
			got, err := st.GetPortalMonitoring(ctx, q, scope)
			if err != nil {
				t.Fatal(err)
			}
			switch view {
			case "summary":
				if !reflect.DeepEqual(got.Stats, full.Stats) || !reflect.DeepEqual(got.Buckets, full.Buckets) || !reflect.DeepEqual(got.Users, full.Users) || !reflect.DeepEqual(got.Models, full.Models) || !reflect.DeepEqual(got.P95LatencyMs, full.P95LatencyMs) {
					t.Fatal("summary parity")
				}
			case "requests":
				if got.RequestCount != want || !reflect.DeepEqual(got.Requests, full.Requests) {
					t.Fatal("page parity")
				}
			case "options":
				if !reflect.DeepEqual(got.Options.Models, full.Options.Models) || !reflect.DeepEqual(got.Options.Providers, full.Options.Providers) || len(got.Options.Users) != len(full.Users) {
					t.Fatal("option parity")
				}
				owners := map[string]string{}
				for _, u := range got.Options.Users {
					owners[u.ID] = u.Name
				}
				for _, u := range full.Users {
					if owners[u.ID] != u.Name {
						t.Fatal("owner parity")
					}
				}
			}
			b, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), "current-private-hash") {
				t.Fatal("secret leaked")
			}
		}
	}
	f.KeyID = PortalMasterKeyID
	for _, view := range []string{"full", "requests"} {
		f.View = view
		for page, id := range []string{"2", "3", ""} {
			f.Page = page + 1
			got, err := st.GetPortalMonitoring(ctx, f, scope)
			if err != nil {
				t.Fatal(err)
			}
			if id == "" {
				if len(got.Requests) != 0 {
					t.Fatal("past last page")
				}
			} else if len(got.Requests) != 1 || got.Requests[0].ID != id || got.Requests[0].User != PortalMasterKeyName {
				t.Fatal("descending master page")
			}
		}
	}
	f.View = "full"
	f.Page = 1
	taipei, err := st.GetPortalMonitoring(ctx, f, scope)
	if err != nil {
		t.Fatal(err)
	}
	f.Timezone = "UTC"
	utc, err := st.GetPortalMonitoring(ctx, f, scope)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(utc.Stats, taipei.Stats) {
		t.Fatal("timezone changed totals")
	}
	if taipei.Buckets[0].Start.Hour() != 16 || utc.Buckets[0].Start.Hour() != 0 || taipei.Buckets[0].Stats.RequestCount != 2 {
		t.Fatal("Taipei bucket attribution")
	}
	f.Since = old
	f.Until = old.Add(time.Second)
	got, err := st.GetPortalMonitoring(ctx, f, scope)
	if err != nil || got.Stats.RequestCount != 1 {
		t.Fatal("historical current credential", err)
	}
	f.Until = now
	f.Since = now.Add(-24 * time.Hour)
	for _, dimension := range []struct {
		set  func(*PortalMonitorFilter)
		want int64
	}{
		{func(q *PortalMonitorFilter) { q.User = PortalMasterKeyName }, 2},
		{func(q *PortalMonitorFilter) { q.User = "alice@example.com" }, 0},
		{func(q *PortalMonitorFilter) { q.Model = "m"; q.Provider = "p"; q.AuthMode = "api_key"; q.Status = "ok" }, 2},
		{func(q *PortalMonitorFilter) { q.Status = "error" }, 0},
	} {
		q := f
		dimension.set(&q)
		for _, view := range []string{"full", "summary", "requests", "options"} {
			q.View = view
			r, err := st.GetPortalMonitoring(ctx, q, scope)
			if err != nil {
				t.Fatal(err)
			}
			if view == "options" {
				if (len(r.Options.Users) > 0) != (dimension.want > 0) {
					t.Fatal("option filter")
				}
			} else if r.RequestCount != dimension.want {
				t.Fatal("exact filter", view, r.RequestCount)
			}
		}
	}
	// Config scope changes do not migrate rows or retain a rotated master secret.
	f.KeyID = ""
	empty, err := st.GetPortalMonitoring(ctx, f, NewPortalReportScope(""))
	if err != nil || empty.Stats.RequestCount != 2 || empty.Scope != "retained_managed_keys" {
		t.Fatal("empty scope", err)
	}
	rotated, err := st.GetPortalMonitoring(ctx, f, NewPortalReportScope("next-private-hash"))
	if err != nil || rotated.Stats.RequestCount != 2 {
		t.Fatal("rotation scope", err)
	}
	if err := st.LogRequest(ctx, RequestLog{ID: "ongoing", APIKeyHash: "current-private-hash", CreatedAt: now.Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetPortalMonitoring(ctx, f, scope)
	if err != nil || got.Stats.RequestCount != 5 {
		t.Fatal("ongoing ingestion", err)
	}
	var persisted int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE key_hash=? OR id=?`, scope.currentMasterHash, PortalMasterKeyID).Scan(&persisted); err != nil || persisted != 0 {
		t.Fatal("report persisted credential", err)
	}
	//lint:ignore SA9005 Deliberately prove the server-only scope has no JSON fields.
	b, _ := json.Marshal(scope)
	if string(b) != "{}" || strings.Contains(fmt.Sprintf("%v %+v %#v", scope, scope, scope), scope.currentMasterHash) {
		t.Fatal("scope must redact serialization and formatting")
	}
}

func TestPortalMasterCollisionPrecedence(t *testing.T) {
	for _, collision := range []string{"employee_current", "employee_historical_import", "service", "legacy"} {
		t.Run(collision, func(t *testing.T) {
			st := usageStore(t)
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Second)
			scope := NewPortalReportScope("overlap")
			switch collision {
			case "employee_current", "employee_historical_import":
				createUsageKey(t, st, usageOwner("alice"), "alice", "overlap")
				if collision == "employee_historical_import" {
					if _, err := st.RotatePortalKey(ctx, usageOwner("alice"), 1, "new"); err != nil {
						t.Fatal(err)
					}
					if err := st.CreateKey(ctx, ManagedKey{ID: "import", Name: "Imported", KeyHash: "overlap"}); err != nil {
						t.Fatal(err)
					}
				}
			case "service":
				if _, err := st.CreateServiceKey(ctx, ManagedKey{ID: "service", Name: "Service", KeyHash: "overlap"}); err != nil {
					t.Fatal(err)
				}
			case "legacy":
				if err := st.CreateKey(ctx, ManagedKey{ID: "legacy", KeyHash: "overlap"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.LogRequest(ctx, RequestLog{ID: "r", APIKeyHash: "overlap", CreatedAt: now.Add(-time.Hour), TotalTokens: 9}); err != nil {
				t.Fatal(err)
			}
			for _, view := range []string{"full", "summary", "requests", "options"} {
				f := PortalMonitorFilter{View: view, Since: now.Add(-PortalWindow), Until: now, Bucket: "day", Page: 1, PageSize: 25}
				got, err := st.GetPortalMonitoring(ctx, f, scope)
				if err != nil {
					t.Fatal(err)
				}
				if view == "options" {
					if len(got.Options.Users) != 1 || got.Options.Users[0].ID != PortalMasterKeyID {
						t.Fatal("overlap owners")
					}
				} else if got.RequestCount != 1 {
					t.Fatal("duplicate count")
				}
				for _, u := range got.Users {
					if u.ID != PortalMasterKeyID {
						t.Fatal("master precedence")
					}
				}
				for _, r := range got.Requests {
					if r.KeyID != PortalMasterKeyID {
						t.Fatal("master page precedence")
					}
				}
				f.KeyID = "alice"
				got, err = st.GetPortalMonitoring(ctx, f, scope)
				if err != nil || got.RequestCount != 0 || len(got.Options.Users) != 0 {
					t.Fatal("overlap not excluded", err)
				}
			}
			overview, err := st.GetPortalOverview(ctx, now, scope)
			if err != nil || overview.Stats.RequestCount != 1 {
				t.Fatal("overview dedup", err)
			}
			for _, k := range overview.Keys {
				if k.Kind == "master" {
					if k.KeyID != PortalMasterKeyID || k.Name != PortalMasterKeyName || k.Models != nil || k.Stats.RequestCount != 1 {
						t.Fatal("master overview")
					}
				} else if k.Stats.RequestCount != 0 {
					t.Fatal("overlap overview")
				}
			}
			// Disabling the optional report scope restores the established precedence.
			overview, err = st.GetPortalOverview(ctx, now, PortalReportScope{})
			if err != nil || overview.Stats.RequestCount != 1 {
				t.Fatal("legacy semantics", err)
			}
		})
	}
}

func TestPortalMasterReservedIdentityCannotBeEditedOrImpersonated(t *testing.T) {
	st := usageStore(t)
	ctx := context.Background()
	// Even an imported persisted identity cannot turn the reserved ID into an
	// editable portal row or attribute an unrelated hash to Master key.
	if _, err := st.CreatePortalKey(ctx, usageOwner("spoof"), ManagedKey{ID: PortalMasterKeyID, KeyHash: "unrelated"}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(st.SetPortalModels(ctx, PortalMasterKeyID, []string{}), ErrNotFound) || !errors.Is(st.SetPortalDisabled(ctx, PortalMasterKeyID, true), ErrNotFound) {
		t.Fatal("reserved identity editable")
	}
	now := time.Now()
	if err := st.LogRequest(ctx, RequestLog{ID: "spoof", APIKeyHash: "unrelated", CreatedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []PortalReportScope{{}, NewPortalReportScope("current")} {
		overview, err := st.GetPortalOverview(ctx, now, scope)
		if err != nil || overview.Stats.RequestCount != 0 {
			t.Fatal("reserved overview spoof", err)
		}
		f := PortalMonitorFilter{Since: now.Add(-PortalWindow), Until: now, Bucket: "day", Page: 1, PageSize: 25}
		got, err := st.GetPortalMonitoring(ctx, f, scope)
		if err != nil || got.Stats.RequestCount != 0 {
			t.Fatal("reserved report spoof", err)
		}
	}
}
