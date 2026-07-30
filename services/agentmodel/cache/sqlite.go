package cache

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// sqliteCache is the optional persistent L2. It mirrors the store package's
// SQLite posture: the pure-Go modernc driver, WAL + busy_timeout for
// concurrent-write friendliness, and a single open connection so writers
// serialize on the file lock rather than racing it.
//
// Schema is a single content-addressed table. expires_at is a unix-nano deadline
// (0 = never); rows are dropped lazily when read past their deadline, and a
// one-shot sweep at open clears anything stale from a previous run.
type sqliteCache struct {
	db    *sql.DB
	ttl   time.Duration
	clock func() time.Time
}

const sqliteCacheSchema = `
CREATE TABLE IF NOT EXISTS response_cache (
    key        TEXT    PRIMARY KEY,
    value      BLOB    NOT NULL,
    expires_at INTEGER NOT NULL DEFAULT 0
);`

func newSQLite(path string, ttl time.Duration, clock func() time.Time) (*sqliteCache, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("agentmodel/cache: open: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(sqliteCacheSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("agentmodel/cache: migrate: %w", err)
	}
	c := &sqliteCache{db: db, ttl: ttl, clock: clock}
	// Clear anything that expired while the process was down so a restart does
	// not resurrect stale responses.
	_, _ = db.Exec(`DELETE FROM response_cache WHERE expires_at != 0 AND expires_at < ?`, c.clock().UnixNano())
	return c, nil
}

func (c *sqliteCache) Get(ctx context.Context, key string) ([]byte, bool) {
	var value []byte
	var expiresAt int64
	err := c.db.QueryRowContext(ctx, `SELECT value, expires_at FROM response_cache WHERE key = ?`, key).Scan(&value, &expiresAt)
	if err != nil {
		return nil, false // sql.ErrNoRows or a read error — treat as a miss
	}
	if expiresAt != 0 && c.clock().UnixNano() > expiresAt {
		// Expired → a miss. The row is reclaimed by the next Set's upsert or the
		// open-time sweep; deleting it here would put a write on the read path,
		// contending for the single write connection.
		return nil, false
	}
	return value, true
}

func (c *sqliteCache) Set(ctx context.Context, key string, value []byte) {
	var expiresAt int64
	if c.ttl > 0 {
		expiresAt = c.clock().Add(c.ttl).UnixNano()
	}
	_, _ = c.db.ExecContext(ctx,
		`INSERT INTO response_cache (key, value, expires_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, expires_at = excluded.expires_at`,
		key, value, expiresAt)
}

func (c *sqliteCache) Close() error { return c.db.Close() }
