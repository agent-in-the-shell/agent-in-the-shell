package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net/url"
	"os"

	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Reserve core until each reporting call returns: the deadline is only a
// deadlock guard, not a latency assertion. Reports must not acquire core at all.
func TestPortalReportsDoNotAcquireCore(t *testing.T) {
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "reports.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	owner := usageOwner("alice")
	createUsageKey(t, st, owner, "alice", "hash")
	if err := st.LogRequest(context.Background(), RequestLog{ID: "first", APIKeyHash: "hash", CreatedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	conn, err := st.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, view := range []string{"full", "summary", "requests", "options", "overview", "usage"} {
		t.Run(view, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			switch view {
			case "overview":
				got, err := st.GetPortalOverview(ctx, now, NewPortalReportScope("hash"))
				if err != nil {
					t.Fatal(err)
				}
				if got.Stats.RequestCount != 1 {
					t.Fatal(got)
				}
			case "usage":
				got, err := st.GetPortalUsage(ctx, owner, now)
				if err != nil {
					t.Fatal(err)
				}
				if got.Stats.RequestCount != 1 || len(got.Calendar.Days) < 365 {
					t.Fatal(got)
				}
			default:
				got, err := st.GetPortalMonitoring(ctx, PortalMonitorFilter{View: view, Since: now.Add(-24 * time.Hour), Until: now, Bucket: "day", Page: 1, PageSize: 25}, NewPortalReportScope("hash"))
				if err != nil {
					t.Fatal(err)
				}
				if view == "requests" && (got.RequestCount != 1 || len(got.Requests) != 1) {
					t.Fatal(got)
				}
			}
		})
	}
}

func TestSQLiteReportingFileURI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "uri database.db")
	st, err := OpenSQLite("file:" + path + "?mode=rwc")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.LogRequest(context.Background(), RequestLog{ID: "uri"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetPortalOverview(context.Background(), time.Now(), PortalReportScope{}); err != nil {
		t.Fatal(err)
	}
}

// A pinned read snapshot stays open throughout lookup, mutation and log commit.
// No sleep/race against a fast query: core must finish BEFORE rollback releases it.
func TestSQLiteReportingSnapshotDoesNotBlockCore(t *testing.T) {
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "snapshot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	owner := usageOwner("alice")
	createUsageKey(t, st, owner, "alice", "hash")
	tx, err := st.reporting.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM request_logs").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetKeyByHash(ctx, "hash"); err != nil {
		t.Fatal(err)
	}
	if err := st.LogRequest(ctx, RequestLog{ID: "during-report", APIKeyHash: "hash"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetPortalDisabled(ctx, "alice", true); err != nil {
		t.Fatal(err)
	}
	key, err := st.GetKeyByHash(ctx, "hash")
	if err != nil || !key.Disabled {
		t.Fatalf("core policy read: %+v %v", key, err)
	}
	if _, err := st.GetPortalBinding(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SumCostByAPIKey(ctx, "hash", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM request_logs").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("snapshot changed: %d", count)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := st.reporting.QueryRowContext(ctx, "SELECT COUNT(*) FROM request_logs").Scan(&count); err != nil || count != 1 {
		t.Fatalf("new snapshot %d: %v", count, err)
	}
}

func TestSQLiteReportingReadOnlyAndTempCleanup(t *testing.T) {
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "readonly.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	createUsageKey(t, st, usageOwner("alice"), "alice", "hash")
	// ReadOnly TxOptions alone are not enforcement in this driver. The open mode
	// must reject writes even outside a transaction and on a replacement connection.
	for attempt := 0; attempt < 2; attempt++ {
		for _, query := range []string{"UPDATE api_keys SET name='bad' WHERE id='alice'", "CREATE TABLE main.bad(id)", "DELETE FROM portal_key_hashes"} {
			if _, err := st.reporting.Exec(query); err == nil || !strings.Contains(err.Error(), "readonly") {
				t.Fatalf("write %q: %v", query, err)
			}
		}
		tx, err := st.reporting.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec("CREATE TEMP TABLE scratch(id); INSERT INTO scratch VALUES(1)"); err != nil {
			t.Fatal(err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := st.reporting.QueryRow("SELECT COUNT(*) FROM sqlite_temp_master").Scan(&n); err != nil || n != 0 {
			t.Fatalf("temp rollback %d %v", n, err)
		}
		now := time.Now()
		for _, view := range []string{"summary", "options", "full", "requests", "summary"} {
			if _, err := st.GetPortalMonitoring(ctx, PortalMonitorFilter{View: view, Since: now.Add(-24 * time.Hour), Until: now, Bucket: "day", Page: 1, PageSize: 25}, PortalReportScope{}); err != nil {
				t.Fatal(err)
			}
			if err := st.reporting.QueryRow("SELECT COUNT(*) FROM sqlite_temp_master").Scan(&n); err != nil || n != 0 {
				t.Fatalf("report temp cleanup %d %v", n, err)
			}
		}
		// Force reconnection, proving read-only is not a one-time connection PRAGMA.
		st.reporting.SetMaxIdleConns(0)
		st.reporting.SetMaxIdleConns(1)
	}
}

func TestSQLiteReportingMemoryAndLifecycle(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	for _, dsn := range []string{":memory:", "", "file::memory:?cache=shared", "file:report-test?mode=memory&cache=shared", "report-test?mode=memory&cache=shared", ":memory:?_pragma=foreign_keys(on)"} {
		t.Run(dsn, func(t *testing.T) {
			st, err := OpenSQLite(dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			createUsageKey(t, st, usageOwner("alice"), "alice", "hash")
			if err := st.LogRequest(context.Background(), RequestLog{ID: "memory", APIKeyHash: "hash"}); err != nil {
				t.Fatal(err)
			}
			got, err := st.GetPortalUsage(context.Background(), usageOwner("alice"), time.Now())
			if err != nil || got.Stats.RequestCount != 1 {
				t.Fatalf("memory split: %+v %v", got, err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			if err := st.db.Ping(); err == nil {
				t.Fatal("core open after close")
			}
			if err := st.reporting.Ping(); err == nil {
				t.Fatal("reporting open after close")
			}
		})
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("memory DSNs created disk artifacts: %v %v", entries, err)
	}
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "close.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.db.Ping(); err == nil {
		t.Fatal("core open after close")
	}
	if err := st.reporting.Ping(); err == nil {
		t.Fatal("reporting open after close")
	}
}

func TestSQLiteReportingCancellationReleasesReader(t *testing.T) {
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "cancel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// Cancel after temp materialization but before aggregation, at the driver seam.
	// Real SQLite rollback must discard the temp schema and release the only slot.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var queries []string
	path := ""
	if err := st.db.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path); err != nil {
		t.Fatal(err)
	}
	_, dsn, _, err := sqliteDSNs(path)
	if err != nil {
		t.Fatal(err)
	}
	traced := sql.OpenDB(monitorTraceConnector{dsn: dsn, queries: &queries, beforeQuery: func(string) { cancel() }})
	traced.SetMaxOpenConns(1)
	original := st.reporting
	st.reporting = traced
	defer func() {
		st.reporting = original
		if err := traced.Close(); err != nil {
			t.Error(err)
		}
	}()
	now := time.Now()
	f := PortalMonitorFilter{View: "summary", Since: now.Add(-24 * time.Hour), Until: now, Bucket: "day", Page: 1, PageSize: 25}
	if _, err := st.GetPortalMonitoring(ctx, f, PortalReportScope{}); err == nil {
		t.Fatal("canceled report succeeded")
	}
	if _, err := st.GetPortalMonitoring(context.Background(), f, PortalReportScope{}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := traced.QueryRow("SELECT COUNT(*) FROM sqlite_temp_master").Scan(&n); err != nil || n != 0 {
		t.Fatalf("temp after cancellation: %d %v", n, err)
	}
}

func TestPortalReportDeadlineAndQueueCancellation(t *testing.T) {
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "deadline.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	conn, err := st.reporting.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// An already canceled caller must not wait for the occupied report slot.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := st.GetPortalOverview(ctx, time.Now(), PortalReportScope{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("queue cancellation: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	var queries []string
	var deadlines []time.Time
	trace := sql.OpenDB(reportDeadlineConnector{monitorTraceConnector: monitorTraceConnector{queries: &queries}, deadlines: &deadlines})
	trace.SetMaxOpenConns(1)
	defer trace.Close()
	if _, err := trace.Exec(sqliteSchema); err != nil {
		t.Fatal(err)
	}
	traced := &SQLiteStore{db: trace, reporting: trace}
	now := time.Now()
	f := PortalMonitorFilter{View: "requests", Since: now.Add(-24 * time.Hour), Until: now, Bucket: "day", Page: 1, PageSize: 25}
	start := time.Now()
	if _, err := traced.GetPortalMonitoring(context.Background(), f, PortalReportScope{}); err != nil {
		t.Fatal(err)
	}
	if _, err := traced.GetPortalOverview(context.Background(), now, PortalReportScope{}); err != nil {
		t.Fatal(err)
	}
	if _, err := traced.GetPortalUsage(context.Background(), usageOwner("alice"), now); err != nil {
		t.Fatal(err)
	}
	for _, d := range deadlines {
		if d.Before(start.Add(portalReportTimeout)) || d.After(time.Now().Add(portalReportTimeout)) {
			t.Fatalf("missing/unexpected report deadline: %v", d)
		}
	}
	if len(deadlines) != 4 {
		t.Fatalf("trace missed report queries: %v", deadlines)
	}
	deadlines = nil
	earlier := time.Now().Add(time.Second)
	ctx, cancel = context.WithDeadline(context.Background(), earlier)
	defer cancel()
	if _, err := traced.GetPortalOverview(ctx, now, PortalReportScope{}); err != nil {
		t.Fatal(err)
	}
	if len(deadlines) != 1 || !deadlines[0].Equal(earlier) {
		t.Fatalf("extended caller deadline: %v", deadlines)
	}
}

type reportDeadlineConnector struct {
	monitorTraceConnector
	deadlines *[]time.Time
}

func (c reportDeadlineConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.monitorTraceConnector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return reportDeadlineConn{monitorTraceConn: conn.(monitorTraceConn), deadlines: c.deadlines}, nil
}

type reportDeadlineConn struct {
	monitorTraceConn
	deadlines *[]time.Time
}

func (c reportDeadlineConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	d, _ := ctx.Deadline()
	*c.deadlines = append(*c.deadlines, d)
	return c.monitorTraceConn.QueryContext(ctx, query, args)
}

func TestSQLiteReportingURIPermissionsAndOpenFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report #%.db")
	uri := (&url.URL{Scheme: "file", Path: path}).String() + "?mode=rwc&_pragma=cache_size(100)"
	st, err := OpenSQLite(uri)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// Tighten existing sidecars as well as newly-created files, while the first
	// store keeps them alive. This also exercises a second constructor/migration.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Chmod(path+suffix, 0644); err != nil {
			t.Fatal(err)
		}
	}
	second, err := OpenSQLite(uri)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("insecure %s: %v", suffix, info.Mode())
		}
	}
	for _, bad := range []string{"file:" + filepath.Join(dir, "bad.db") + "?mode=ro", "file:" + path + "?immutable=1", "file:" + path + "?nolock=1", "file:" + path + "?vfs=unix-none", "file:" + path + "?mode=invalid", "file:" + path + "?bad=%zz", "file://remote/db"} {
		failed, err := OpenSQLite(bad)
		if err == nil || failed != nil {
			t.Fatalf("unsafe DSN %q: %v %v", bad, failed, err)
		}
	}
	// Fail after sql.Open (bad pragma) and after connection creation (migration).
	for _, bad := range []string{filepath.Join(dir, "pragma.db") + "?_pragma=invalid( syntax", filepath.Join(dir, "corrupt.db")} {
		if strings.HasSuffix(bad, "corrupt.db") {
			if err := os.WriteFile(bad, []byte("not a database"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		failed, err := OpenSQLite(bad)
		if err == nil || failed != nil {
			t.Fatalf("open failure: %v %v", failed, err)
		}
	}
}
