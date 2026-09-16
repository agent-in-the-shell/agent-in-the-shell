package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
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
    reasoning_tokens            INTEGER NULL,
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
    revoked_at      INTEGER,
    metadata        TEXT    NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_hash ON api_keys(key_hash);

-- Explicit machine credentials, never inferred from names or metadata.
-- Revocation and rotation preserve the marker and credential history.
CREATE TABLE IF NOT EXISTS service_keys (
    key_id TEXT PRIMARY KEY NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    revision INTEGER NOT NULL DEFAULT 1,
    normalized_name TEXT NOT NULL UNIQUE CHECK (length(normalized_name) > 0)
);

-- Service hash history supports attribution, not authentication.
CREATE TABLE IF NOT EXISTS service_key_hashes (
    key_id TEXT NOT NULL REFERENCES service_keys(key_id),
    key_hash TEXT NOT NULL UNIQUE,
    PRIMARY KEY (key_id, key_hash)
);
INSERT INTO service_key_hashes(key_id,key_hash)
 SELECT sk.key_id,k.key_hash FROM service_keys sk JOIN api_keys k ON k.id=sk.key_id
 WHERE 1 ON CONFLICT(key_id,key_hash) DO NOTHING;

-- No cascading FK: management history survives even out-of-band key removal.
-- Deliberately excludes request bodies, secret/hash values, names and emails.
CREATE TABLE IF NOT EXISTS key_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    key_id TEXT NOT NULL,
    actor_kind TEXT NOT NULL CHECK(actor_kind IN ('admin','user','master','system')),
    actor_id TEXT NOT NULL,
    action TEXT NOT NULL CHECK(action IN ('create','disable','enable','revoke','rotate','models')),
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_key_events_key ON key_events(key_id,id DESC);

-- Portal identities survive revocation: replacement rotates the secret on the
-- same logical key, never opens a second self-service identity.
CREATE TABLE IF NOT EXISTS portal_identities (
    issuer TEXT NOT NULL,
    subject TEXT NOT NULL,
    email TEXT NOT NULL,
    key_id TEXT NOT NULL UNIQUE,
    revision INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (issuer, subject)
);

-- Reporting only: auth still reads exclusively from api_keys.key_hash.
-- Retain history even after credential deletion, like the identity tombstone.
CREATE TABLE IF NOT EXISTS portal_key_hashes (
    key_id TEXT NOT NULL REFERENCES portal_identities(key_id),
    key_hash TEXT NOT NULL UNIQUE,
    PRIMARY KEY (key_id, key_hash)
);
INSERT INTO portal_key_hashes(key_id, key_hash)
    SELECT p.key_id, k.key_hash FROM portal_identities p
    JOIN api_keys k ON k.id = p.key_id
    WHERE 1
    ON CONFLICT (key_id, key_hash) DO NOTHING;
`

// sqliteMigrations holds idempotent ALTER statements for evolving the schema.
// Each is wrapped in a guard that ignores "duplicate column" errors so they
// can re-run safely after the initial CREATE.
var sqliteMigrations = []string{
	`ALTER TABLE service_keys ADD COLUMN revision INTEGER NOT NULL DEFAULT 1`,
	`ALTER TABLE api_keys ADD COLUMN revoked_at INTEGER NULL`,
	`ALTER TABLE request_logs ADD COLUMN reasoning_tokens INTEGER NULL`,
	`ALTER TABLE request_logs ADD COLUMN cache_read_tokens INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE request_logs ADD COLUMN cache_creation_tokens INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE request_logs ADD COLUMN cost_source TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE request_logs ADD COLUMN request_id TEXT NOT NULL DEFAULT ''`,
}

// SQLiteStore serializes core operations on one connection. Disk-backed portal
// reports use one separate read-only WAL connection, never the core queue.
type SQLiteStore struct {
	db        *sql.DB
	reporting *sql.DB
}

// OpenSQLite opens (or creates) a SQLite database at path and runs migrations.
//
// WAL mode and a generous busy_timeout are enabled so concurrent goroutines
// performing writes wait for the lock instead of failing with SQLITE_BUSY.
func OpenSQLite(path string) (_ *SQLiteStore, openErr error) {
	coreDSN, readerDSN, diskPath, err := sqliteDSNs(path)
	if err != nil {
		return nil, fmt.Errorf("agentmodel/store: DSN: %w", err)
	}
	if diskPath != "" {
		if _, err := herospath.EnsureParent(diskPath); err != nil {
			return nil, fmt.Errorf("agentmodel/store: create data dir: %w", err)
		}
		if err := herospath.SecureSQLiteFile(diskPath); err != nil {
			return nil, fmt.Errorf("agentmodel/store: secure %q: %w", diskPath, err)
		}
	}
	db, err := sql.Open("sqlite", coreDSN)
	if err != nil {
		return nil, fmt.Errorf("agentmodel/store: open: %w", err)
	}
	// Keep core writer serialization; report isolation is not a general pool.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	defer func() {
		if openErr != nil {
			openErr = errors.Join(openErr, db.Close())
		}
	}()

	if _, err := db.Exec(sqliteSchema); err != nil {
		return nil, fmt.Errorf("agentmodel/store: migrate: %w", err)
	}
	for _, stmt := range sqliteMigrations {
		// "duplicate column name" is the expected outcome on a freshly-created
		// schema; ignore it so the migration is idempotent.
		if _, err := db.Exec(stmt); err != nil && !isDuplicateColumnErr(err) {
			return nil, fmt.Errorf("agentmodel/store: migrate %q: %w", stmt, err)
		}
	}
	// SQLite memory/temporary databases cannot provide mode=ro WAL isolation.
	// Retain the exact core handle rather than opening an unrelated database.
	reporting := db
	if diskPath != "" {
		var journal string
		if err := db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
			return nil, err
		}
		if journal != "wal" {
			return nil, fmt.Errorf("agentmodel/store: reporting requires WAL, got %q", journal)
		}
		reporting, err = sql.Open("sqlite", readerDSN)
		if err != nil {
			return nil, fmt.Errorf("agentmodel/store: open reporting: %w", err)
		}
		reporting.SetMaxOpenConns(1)
		reporting.SetMaxIdleConns(1)
		if err := reporting.Ping(); err != nil {
			return nil, errors.Join(fmt.Errorf("agentmodel/store: open reporting: %w", err), reporting.Close())
		}
	}
	return &SQLiteStore{db: db, reporting: reporting}, nil
}

func isDuplicateColumnErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate column name")
}

func (s *SQLiteStore) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *SQLiteStore) Close() error {
	var err error
	if s.reporting != nil && s.reporting != s.db {
		err = s.reporting.Close()
	}
	return errors.Join(err, s.db.Close())
}

func (s *SQLiteStore) LogRequest(ctx context.Context, log RequestLog) error {
	if log.CreatedAt.IsZero() {
		log.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO request_logs (
            id, request_id, org_id, api_key_hash, model_requested, model_used, provider,
            auth_mode, prompt_tokens, completion_tokens, total_tokens, reasoning_tokens,
            cache_read_tokens, cache_creation_tokens,
            cost_usd, cost_source, latency_ms, status, error_type, created_at
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    `,
		log.ID, log.RequestID, log.OrgID, log.APIKeyHash, log.ModelRequested, log.ModelUsed,
		log.Provider, log.AuthMode, log.PromptTokens, log.CompletionTokens,
		log.TotalTokens, log.ReasoningTokens, log.CacheReadInputTokens, log.CacheCreationInputTokens,
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
               auth_mode, prompt_tokens, completion_tokens, total_tokens, reasoning_tokens,
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
               auth_mode, prompt_tokens, completion_tokens, total_tokens, reasoning_tokens,
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
               auth_mode, prompt_tokens, completion_tokens, total_tokens, reasoning_tokens,
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
	// Service rotations retain spend across all secrets on the same logical key.
	// Legacy and Portal budget semantics remain unchanged.
	var sum float64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(cost_usd),0) FROM request_logs
 WHERE status='ok' AND created_at>=? AND (api_key_hash=? OR api_key_hash IN (
 SELECT h.key_hash FROM service_key_hashes h JOIN service_keys sk ON sk.key_id=h.key_id JOIN api_keys current ON current.id=sk.key_id WHERE current.key_hash=?))`, since.Unix(), apiKeyHash, apiKeyHash).Scan(&sum)
	return sum, err
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
		&rl.TotalTokens, &rl.ReasoningTokens, &rl.CacheReadInputTokens, &rl.CacheCreationInputTokens,
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

// Only file URIs interpret escaping; ordinary paths are escaped before being
// handed to SQLite. Security helpers always receive the actual filesystem path.
func sqliteDSNs(path string) (core, reader, diskPath string, err error) {
	name, query, _ := strings.Cut(path, "?")
	params, err := url.ParseQuery(query)
	if err != nil {
		return "", "", "", err
	}
	if strings.HasPrefix(name, "file:") {
		u, e := url.Parse(name)
		if e != nil {
			return "", "", "", e
		}
		if u.Host != "" && u.Host != "localhost" {
			return "", "", "", errors.New("unsupported SQLite URI host")
		}
		name = u.Path
		if u.Opaque != "" {
			name, err = url.PathUnescape(u.Opaque)
			if err != nil {
				return "", "", "", err
			}
		}
	}
	// An empty filename plus driver query parameters is otherwise interpreted
	// as a literal on-disk filename by modernc. Keep it private and ephemeral.
	if name == "" {
		name = ":memory:"
	}
	memory := name == ":memory:" || params.Get("mode") == "memory"
	if !memory {
		if mode := params.Get("mode"); mode != "" && mode != "rw" && mode != "rwc" {
			return "", "", "", fmt.Errorf("unsupported writable SQLite mode %q", mode)
		}
		for _, option := range []string{"immutable", "nolock", "vfs"} {
			if params.Has(option) {
				return "", "", "", fmt.Errorf("unsupported SQLite option %q for WAL reporting", option)
			}
		}
		diskPath, err = filepath.Abs(name)
		if err != nil {
			return "", "", "", err
		}
		name = (&url.URL{Scheme: "file", Path: diskPath}).String()
		// Never inherit shared-cache locks, txlock, or write pragmas on the reader.
		reader = name + "?mode=ro&cache=private&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)"
	} else if strings.HasPrefix(path, "file:") || params.Get("mode") == "memory" {
		name = "file:" + url.PathEscape(name)
	}
	params.Add("_pragma", "journal_mode(WAL)")
	params.Add("_pragma", "busy_timeout(5000)")
	params.Add("_pragma", "foreign_keys(on)")
	return name + "?" + params.Encode(), reader, diskPath, nil
}

// This bounds both waiting for the sole reporting connection and SQL work.
// The HTTP server deliberately has no whole-request timeout (SSE); reports must
// not hold a WAL snapshot indefinitely. Earlier caller deadlines still win.
const portalReportTimeout = 15 * time.Second
