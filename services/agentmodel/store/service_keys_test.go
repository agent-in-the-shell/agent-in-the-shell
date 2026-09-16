package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestServiceIssuanceAtomicConcurrentReopen(t *testing.T) {
	ctx := context.Background()
	for _, disk := range []bool{false, true} {
		t.Run(fmt.Sprint(disk), func(t *testing.T) {
			path := ":memory:"
			if disk {
				path = filepath.Join(t.TempDir(), "keys.db")
			}
			st, err := OpenSQLite(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { st.Close() }()
			stores := []*SQLiteStore{st}
			if disk {
				other, err := OpenSQLite(path)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
				stores = append(stores, other)
			}
			var wg sync.WaitGroup
			results := make(chan error, 12)
			for i := 0; i < 12; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					name := "  WÖRKER  "
					if i%2 == 0 {
						name = "wörker"
					}
					_, err := stores[i%len(stores)].CreateServiceKey(ctx, ManagedKey{ID: fmt.Sprint(i), Name: name, KeyHash: fmt.Sprint("hash-", i), Models: []string{"must-not-grant"}})
					results <- err
				}(i)
			}
			wg.Wait()
			close(results)
			success := 0
			for err := range results {
				if err == nil {
					success++
				} else if !errors.Is(err, ErrServiceNameConflict) {
					t.Fatal(err)
				}
			}
			if success != 1 {
				t.Fatal("successes", success)
			}
			keys, err := st.ListKeys(ctx)
			if err != nil || len(keys) != 1 {
				t.Fatal(keys, err)
			}
			key := keys[0]
			var models string
			var markers, employees int
			if err := st.db.QueryRow(`SELECT models FROM api_keys WHERE id=?`, key.ID).Scan(&models); err != nil {
				t.Fatal(err)
			}
			if models != "[]" {
				t.Fatal("not literal deny-all", models)
			}
			st.db.QueryRow(`SELECT COUNT(*) FROM service_keys`).Scan(&markers)
			st.db.QueryRow(`SELECT COUNT(*) FROM portal_identities`).Scan(&employees)
			if markers != 1 || employees != 0 {
				t.Fatal(markers, employees)
			}
			// A hash collision must not reserve a name or persist a marker.
			if _, err := st.CreateServiceKey(ctx, ManagedKey{ID: "collision", Name: "another", KeyHash: key.KeyHash}); err == nil {
				t.Fatal("hash collision accepted")
			}
			if _, err := st.CreateServiceKey(ctx, ManagedKey{ID: "another", Name: "another", KeyHash: "another-hash"}); err != nil {
				t.Fatal(err)
			}
			if disk {
				if err := st.Close(); err != nil {
					t.Fatal(err)
				}
				st, err = OpenSQLite(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			got, err := st.GetKeyByHash(ctx, key.KeyHash)
			if err != nil || !got.ServiceIssued || got.PortalIssued || got.Models == nil || len(got.Models) != 0 {
				t.Fatal(got, err)
			}
			// Database uniqueness remains authoritative even if an operator renamed api_keys.
			if _, err := st.db.Exec(`UPDATE api_keys SET name='renamed' WHERE id=?`, key.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := st.CreateServiceKey(ctx, ManagedKey{ID: "duplicate", Name: "wörker", KeyHash: "duplicate"}); !errors.Is(err, ErrServiceNameConflict) {
				t.Fatal(err)
			}
			if err := st.DeleteKey(ctx, key.ID); err != nil {
				t.Fatal(err)
			}
			st.db.QueryRow(`SELECT COUNT(*) FROM service_keys WHERE key_id=?`, key.ID).Scan(&markers)
			if markers != 1 {
				t.Fatal("revocation lost service classification")
			}
			if _, err := st.CreateServiceKey(ctx, ManagedKey{ID: "replacement", Name: "wörker", KeyHash: "replacement"}); !errors.Is(err, ErrServiceNameConflict) {
				t.Fatal(err)
			}
		})
	}
}

func TestServiceReportingHistoricalHashesAndEmployeePrecedence(t *testing.T) {
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "reports.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	_, err = st.CreateServiceKey(ctx, ManagedKey{ID: "service", Name: "Worker", KeyHash: "service-old"})
	if err != nil {
		t.Fatal(err)
	}
	// Even an out-of-band edit does not erase previously recorded attribution.
	if _, err := st.db.Exec(`UPDATE api_keys SET key_hash='service-current' WHERE id='service'`); err != nil {
		t.Fatal(err)
	}
	identity := PortalIdentity{Issuer: "issuer", Subject: "employee", Email: "employee@example.com"}
	if _, err := st.CreatePortalKey(ctx, identity, ManagedKey{ID: "employee", Name: identity.Email, KeyHash: "employee-old"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RotatePortalKey(ctx, identity, 1, "employee-current"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateServiceKey(ctx, ManagedKey{ID: "reused", Name: "Reused", KeyHash: "employee-old"}); err != nil {
		t.Fatal(err)
	}
	for _, hash := range []string{"service-old", "service-current", "employee-old", "employee-current"} {
		if err := st.LogRequest(ctx, RequestLog{ID: hash, APIKeyHash: hash, CreatedAt: now.Add(-time.Hour), TotalTokens: 10, ModelRequested: "chatgpt", Provider: "test", Status: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	overview, err := st.GetPortalOverview(ctx, now, PortalReportScope{})
	if err != nil {
		t.Fatal(err)
	}
	if overview.Stats.RequestCount != 4 {
		t.Fatal(overview)
	}
	for _, k := range overview.Keys {
		want := int64(0)
		if k.KeyID == "service" {
			want = 2
		}
		if k.KeyID == "employee" {
			want = 2
		}
		if k.Stats.RequestCount != want {
			t.Fatal(k)
		}
		if k.KeyID != "employee" && (k.Kind != "service" || k.Models == nil) {
			t.Fatal(k)
		}
	}
	for _, view := range []string{"full", "summary", "requests", "options"} {
		for _, id := range []string{"service", "reused", "employee"} {
			f := PortalMonitorFilter{View: view, Since: now.Add(-24 * time.Hour), Until: now, Bucket: "day", Page: 1, PageSize: 25, KeyID: id}
			report, err := st.GetPortalMonitoring(ctx, f, PortalReportScope{})
			if err != nil {
				t.Fatal(view, id, err)
			}
			want := int64(0)
			if id == "service" {
				want = 2
			}
			if id == "employee" {
				want = 2
			}
			switch view {
			case "full", "summary":
				if report.Stats.RequestCount != want {
					t.Fatal(view, id, report.Stats)
				}
			case "requests":
				if report.RequestCount != want || int64(len(report.Requests)) != want {
					t.Fatal(view, id, report)
				}
			case "options":
				if (len(report.Options.Users) > 0) != (want > 0) {
					t.Fatal(view, id, report.Options)
				}
			}
			for _, r := range report.Requests {
				if r.KeyID != id {
					t.Fatal("wrong attribution", r)
				}
			}
		}
	}
}

func TestServiceNameValidation(t *testing.T) {
	for _, name := range []string{"", "  ", "a\nb", "\tworker", string([]byte{0xff}), strings.Repeat("x", 129), "worker\x7f"} {
		if _, _, err := NormalizeServiceName(name); !errors.Is(err, ErrServiceName) {
			t.Fatalf("accepted %q", name)
		}
	}
	display, normalized, err := NormalizeServiceName("  WÖRKER  ")
	if err != nil || display != "WÖRKER" || normalized != "wörker" {
		t.Fatal(display, normalized, err)
	}
}
