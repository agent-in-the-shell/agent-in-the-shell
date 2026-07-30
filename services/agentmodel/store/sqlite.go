package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/internal/herospath"
	_ "modernc.org/sqlite"
)

const sqliteSchema = `
CREATE TABLE IF NOT EXISTS request_logs (
    id                          TEXT    PRIMARY KEY,
    request_id                  TEXT    NOT NULL DEFAULT '',
    org_id                      TEXT    NOT NULL,
    api_key_hash                TEXT    NOT NULL,
    model_requested             TEXT    NOT NULL,
    model_used                  TEXT    NOT NULL,
    provider                    TEXT    NOT NULL,
    auth_mode                   TEXT    NOT NULL,
    prompt_tokens               INTEGER NOT NULL,
    completion_tokens           INTEGER NOT NULL,
    total_tokens                INTEGER NOT NULL,
    cache_read_tokens           INTEGER NOT NULL DEFAULT 0,
    cache_creation_tokens       INTEGER NOT NULL DEFAULT 0,
    cost_usd                    REAL    NOT NULL,
    cost_source                 TEXT    NOT NULL DEFAULT '',
    latency_ms                  INTEGER NOT NULL,
    status                      TEXT    NOT NULL,
    error_type                  TEXT    NOT NULL DEFAULT '',
    created_at                  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_request_logs_org_time
    ON request_logs(org_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_request_logs_apikey_time
    ON request_logs(api_key_hash, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_request_logs_authmode_time
    ON request_logs(auth_mode, created_at DESC);

CREATE TABLE IF NOT EXISTS api_keys (
    id              TEXT    PRIMARY KEY,
    name            TEXT    NOT NULL,
    key_hash        TEXT    NOT NULL UNIQUE,
    models          TEXT    NOT NULL DEFAULT '',
    max_budget      REAL,
    budget_duration TEXT    NOT NULL DEFAULT '',
    expires_at      INTEGER,
    disabled        INTEGER NOT NULL DEFAULT 0,
    metadata        TEXT    NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_hash ON api_keys(key_hash);
`

// sqliteMigrations holds idempotent ALTER statements for evolving the schema.
// Each is wrapped in a guard that ignores "duplicate column" errors so they
// can re-run safely after the initial CREATE.
var sqliteMigrations = []string{
	`ALTER TABLE request_logs ADD COLUMN cache_read_tokens INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE request_logs ADD COLUMN cache_creation_tokens INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE request_logs ADD COLUMN cost_source TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE request_logs ADD COLUMN request_id TEXT NOT NULL DEFAULT ''`,
}

// SQLiteStore is the wave-1 default Store. Concurrent writes are safe; the
// underlying sqlite driver serializes via the global db mutex.
type SQLiteStore struct {
	db *sql.DB
}

// OpenSQLite opens (or creates) a SQLite database at path and runs migrations.
//
// WAL mode and a generous busy_timeout are enabled so concurrent goroutines
// performing writes wait for the lock instead of failing with SQLITE_BUSY.
func OpenSQLite(path string) (*SQLiteStore, error) {
	// Create the parent dir before opening — OpenSQLite historically didn't, so
	// a fresh install on the canonical ~/.local/share/heros/agentmodel/ layout
	// failed with "unable to open database file (14)". EnsureParent is a no-op
	// for ":memory:" (its dir is ".").
	if _, err := herospath.EnsureParent(path); err != nil {
		return nil, fmt.Errorf("agentmodel/store: create data dir: %w", err)
	}
	// DSN tweaks: WAL journal + busy timeout = concurrent-write friendly.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("agentmodel/store: open: %w", err)
	}
	// SQLite serializes writers via its file lock; using more than 1 open
	// connection means concurrent writers race for the lock. Pool of 1
	// removes contention at the cost of throughput, which is fine for
	// wave-1's audit-log-only workload.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if _, err := db.Exec(sqliteSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("agentmodel/store: migrate: %w", err)
	}
	for _, stmt := range sqliteMigrations {
		// "duplicate column name" is the expected outcome on a freshly-created
		// schema; ignore it so the migration is idempotent.
		if _, err := db.Exec(stmt); err != nil && !isDuplicateColumnErr(err) {
			_ = db.Close()
			return nil, fmt.Errorf("agentmodel/store: migrate %q: %w", stmt, err)
		}
	}
	return &SQLiteStore{db: db}, nil
}

func isDuplicateColumnErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate column name")
}

func (s *SQLiteStore) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *SQLiteStore) Close() error { return s.db.Close() }

func (s *SQLiteStore) LogRequest(ctx context.Context, log RequestLog) error {
	if log.CreatedAt.IsZero() {
		log.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO request_logs (
            id, request_id, org_id, api_key_hash, model_requested, model_used, provider,
            auth_mode, prompt_tokens, completion_tokens, total_tokens,
            cache_read_tokens, cache_creation_tokens,
            cost_usd, cost_source, latency_ms, status, error_type, created_at
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    `,
		log.ID, log.RequestID, log.OrgID, log.APIKeyHash, log.ModelRequested, log.ModelUsed,
		log.Provider, log.AuthMode, log.PromptTokens, log.CompletionTokens,
		log.TotalTokens, log.CacheReadInputTokens, log.CacheCreationInputTokens,
		log.CostUSD, log.CostSource, log.LatencyMs, log.Status, log.ErrorType,
		log.CreatedAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("agentmodel/store: insert: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetRequestLog(ctx context.Context, id string) (RequestLog, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT id, request_id, org_id, api_key_hash, model_requested, model_used, provider,
               auth_mode, prompt_tokens, completion_tokens, total_tokens,
               cache_read_tokens, cache_creation_tokens,
               cost_usd, cost_source, latency_ms, status, error_type, created_at
        FROM request_logs
        WHERE id = ?
    `, id)
	rl, err := scanRequestLog(row)
	if errors.Is(err, sql.ErrNoRows) {
		return RequestLog{}, ErrNotFound
	}
	return rl, err
}

func (s *SQLiteStore) ListByOrg(ctx context.Context, orgID string, since time.Time, limit int) ([]RequestLog, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, request_id, org_id, api_key_hash, model_requested, model_used, provider,
               auth_mode, prompt_tokens, completion_tokens, total_tokens,
               cache_read_tokens, cache_creation_tokens,
               cost_usd, cost_source, latency_ms, status, error_type, created_at
        FROM request_logs
        WHERE org_id = ? AND created_at >= ?
        ORDER BY created_at DESC
        LIMIT ?
    `, orgID, since.Unix(), limit)
	if err != nil {
		return nil, fmt.Errorf("agentmodel/store: list by org: %w", err)
	}
	defer rows.Close()
	return scanLogs(rows)
}

func (s *SQLiteStore) ListByAPIKey(ctx context.Context, apiKeyHash string, since time.Time, limit int) ([]RequestLog, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, request_id, org_id, api_key_hash, model_requested, model_used, provider,
               auth_mode, prompt_tokens, completion_tokens, total_tokens,
               cache_read_tokens, cache_creation_tokens,
               cost_usd, cost_source, latency_ms, status, error_type, created_at
        FROM request_logs
        WHERE api_key_hash = ? AND created_at >= ?
        ORDER BY created_at DESC
        LIMIT ?
    `, apiKeyHash, since.Unix(), limit)
	if err != nil {
		return nil, fmt.Errorf("agentmodel/store: list by api key: %w", err)
	}
	defer rows.Close()
	return scanLogs(rows)
}

func (s *SQLiteStore) SumCostByOrg(ctx context.Context, orgID string, since time.Time) (float64, error) {
	return s.sumCost(ctx, "org_id", orgID, since)
}

func (s *SQLiteStore) SumCostByAPIKey(ctx context.Context, apiKeyHash string, since time.Time) (float64, error) {
	return s.sumCost(ctx, "api_key_hash", apiKeyHash, since)
}

// sumCost totals successful-request spend for one attribution column. column
// is one of the fixed identifiers above, never caller input.
func (s *SQLiteStore) sumCost(ctx context.Context, column, value string, since time.Time) (float64, error) {
	var sum sql.NullFloat64
	err := s.db.QueryRowContext(ctx,
		`SELECT SUM(cost_usd) FROM request_logs WHERE `+column+` = ? AND created_at >= ? AND status = 'ok'`,
		value, since.Unix()).Scan(&sum)
	if err != nil {
		return 0, fmt.Errorf("agentmodel/store: sum cost by %s: %w", column, err)
	}
	if !sum.Valid {
		return 0, nil
	}
	return sum.Float64, nil
}

func (s *SQLiteStore) CountRequestsByOrg(ctx context.Context, orgID string, since time.Time, authMode string) (int64, error) {
	q := `SELECT COUNT(*) FROM request_logs WHERE org_id = ? AND created_at >= ?`
	args := []any{orgID, since.Unix()}
	if authMode != "" {
		q += ` AND auth_mode = ?`
		args = append(args, authMode)
	}
	var n int64
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("agentmodel/store: count: %w", err)
	}
	return n, nil
}

func (s *SQLiteStore) Purge(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM request_logs WHERE created_at < ?`, before.Unix())
	if err != nil {
		return 0, fmt.Errorf("agentmodel/store: purge: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("agentmodel/store: purge rows affected: %w", err)
	}
	return n, nil
}

// rowScanner is the common interface satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanRequestLog(s rowScanner) (RequestLog, error) {
	var rl RequestLog
	var ts int64
	err := s.Scan(
		&rl.ID, &rl.RequestID, &rl.OrgID, &rl.APIKeyHash, &rl.ModelRequested, &rl.ModelUsed,
		&rl.Provider, &rl.AuthMode, &rl.PromptTokens, &rl.CompletionTokens,
		&rl.TotalTokens, &rl.CacheReadInputTokens, &rl.CacheCreationInputTokens,
		&rl.CostUSD, &rl.CostSource, &rl.LatencyMs, &rl.Status, &rl.ErrorType,
		&ts,
	)
	if err != nil {
		return RequestLog{}, err
	}
	rl.CreatedAt = time.Unix(ts, 0).UTC()
	return rl, nil
}

func scanLogs(rows *sql.Rows) ([]RequestLog, error) {
	var out []RequestLog
	for rows.Next() {
		rl, err := scanRequestLog(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rl)
	}
	return out, rows.Err()
}
