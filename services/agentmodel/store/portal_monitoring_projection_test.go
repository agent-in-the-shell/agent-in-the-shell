package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"modernc.org/sqlite"
)

// Record actual statements at the SQL driver seam, not just JSON omissions.
// Each connector owns its trace; no global hooks or production instrumentation.
type monitorTraceConnector struct {
	queries     *[]string
	dsn         string
	beforeQuery func(string)
}

func (c monitorTraceConnector) Driver() driver.Driver { return &sqlite.Driver{} }
func (c monitorTraceConnector) Connect(context.Context) (driver.Conn, error) {
	dsn := c.dsn
	if dsn == "" {
		dsn = ":memory:"
	}
	conn, err := c.Driver().Open(dsn)
	if err != nil {
		return nil, err
	}
	return monitorTraceConn{Conn: conn, queries: c.queries, beforeQuery: c.beforeQuery}, nil
}

type monitorTraceConn struct {
	driver.Conn
	queries     *[]string
	beforeQuery func(string)
}

func (c monitorTraceConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	*c.queries = append(*c.queries, query)
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}
func (c monitorTraceConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if c.beforeQuery != nil {
		c.beforeQuery(query)
	}
	*c.queries = append(*c.queries, query)
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func TestPortalMonitoringProjectionWork(t *testing.T) {
	var queries []string
	db := sql.OpenDB(monitorTraceConnector{queries: &queries})
	db.SetMaxOpenConns(1)
	defer db.Close()
	if _, err := db.Exec(sqliteSchema); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range sqliteMigrations {
		_, _ = db.Exec(stmt)
	}
	st := &SQLiteStore{db: db, reporting: db}
	ctx := context.Background()
	createUsageKey(t, st, usageOwner("alice"), "alice", "hash")
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	if err := st.LogRequest(ctx, RequestLog{ID: "r", APIKeyHash: "hash", CreatedAt: now.Add(-time.Hour), LatencyMs: 12}); err != nil {
		t.Fatal(err)
	}
	for _, view := range []string{"summary", "requests", "options"} {
		t.Run(view, func(t *testing.T) {
			f := PortalMonitorFilter{Since: now.Add(-24 * time.Hour), Until: now, Bucket: "day", Page: 1, PageSize: 25}
			// JSON lets this proof run against the pre-projection implementation too.
			if err := json.Unmarshal([]byte(`{"view":"`+view+`"}`), &f); err != nil {
				t.Fatal(err)
			}
			queries = nil
			if _, err := st.GetPortalMonitoring(ctx, f, NewPortalReportScope("trace-master-private")); err != nil {
				t.Fatal(err)
			}
			all := strings.Join(queries, "\n")
			if strings.Contains(all, "trace-master-private") {
				t.Fatal("credential interpolated in SQL")
			}
			wantQueries := map[string]int{"summary": 8, "requests": 2, "options": 4}[view]
			if len(queries) != wantQueries {
				t.Fatalf("%s: got %d statements, want %d:\n%s", view, len(queries), wantQueries, all)
			}
			if view == "summary" {
				if strings.Contains(all, "ORDER BY created_at DESC,id DESC") {
					t.Fatal("summary executed metadata pagination")
				}
			} else {
				for _, forbidden := range []string{"SUM(", "AVG(", "ORDER BY latency_ms", "GROUP BY"} {
					if strings.Contains(all, forbidden) {
						t.Fatalf("%s executed dashboard work %q", view, forbidden)
					}
				}
			}
			if view == "requests" {
				if strings.Contains(queries[0], "cost_source") || strings.Contains(queries[0], "tokens") || strings.Contains(queries[0], "latency_ms") {
					t.Fatal("count read wide metadata")
				}
				if !strings.Contains(queries[1], "SELECT id,key_id,owner FROM portal_scope") || !strings.Contains(queries[1], "LIMIT ? OFFSET ?) p JOIN request_logs r ON r.id=p.id") {
					t.Fatal("metadata must be loaded only after narrow page selection")
				}
				for _, forbidden := range []string{"CREATE TEMP", "DISTINCT"} {
					if strings.Contains(all, forbidden) {
						t.Fatalf("page executed %s", forbidden)
					}
				}
			}
		})
	}
}

func (c monitorTraceConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func TestPortalMonitoringPageCountAndRowsShareSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.db")
	writer, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	createUsageKey(t, writer, usageOwner("alice"), "alice", "hash")
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	log := RequestLog{ID: "first", APIKeyHash: "hash", CreatedAt: now.Add(-time.Hour)}
	if err := writer.LogRequest(ctx, log); err != nil {
		t.Fatal(err)
	}
	var queries []string
	inserted := false
	// Commit a late log on an independent WAL connection between count and page.
	_, reportingDSN, _, err := sqliteDSNs(path)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(monitorTraceConnector{queries: &queries, dsn: reportingDSN, beforeQuery: func(query string) {
		if inserted || !strings.Contains(query, "LIMIT ? OFFSET ?") {
			return
		}
		inserted = true
		log.ID = "late"
		if err := writer.LogRequest(ctx, log); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.RotatePortalKey(ctx, usageOwner("alice"), 1, "rotated"); err != nil {
			t.Fatal(err)
		}
		if err := writer.CreateKey(ctx, ManagedKey{ID: "import", Name: "not alice", KeyHash: "hash"}); err != nil {
			t.Fatal(err)
		}
	}})
	db.SetMaxOpenConns(1)
	defer db.Close()
	// Instrument only the production reader; nested report acquisition or a
	// fallback to core would deadlock/fail this count-to-page coordination.
	original := writer.reporting
	writer.reporting = db
	defer func() { writer.reporting = original }()
	reader := writer
	f := PortalMonitorFilter{View: "requests", Since: now.Add(-24 * time.Hour), Until: now, Bucket: "day", Page: 1, PageSize: 25}
	page, err := reader.GetPortalMonitoring(ctx, f, PortalReportScope{})
	if err != nil {
		t.Fatal(err)
	}
	if !inserted || page.RequestCount != 1 || len(page.Requests) != 1 || page.Requests[0].ID != "first" {
		t.Fatalf("inconsistent snapshot: %+v", page)
	}
	next, err := reader.GetPortalMonitoring(ctx, f, PortalReportScope{})
	if err != nil {
		t.Fatal(err)
	}
	if next.RequestCount != 2 || len(next.Requests) != 2 {
		t.Fatalf("next response must see new commit: %+v", next)
	}
	for _, response := range []PortalMonitoring{page, next} {
		for _, row := range response.Requests {
			if row.KeyID != "alice" || row.User != usageOwner("alice").Email {
				t.Fatalf("rotation/import stole historical ownership: %+v", row)
			}
		}
	}
}
