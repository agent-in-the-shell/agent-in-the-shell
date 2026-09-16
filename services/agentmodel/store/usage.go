package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// UsageDimension is the closed set of columns a usage report may group by.
// The set is fixed because a GROUP BY column can never be a bound `?`
// parameter — see usageDimSQL, the sole SQL-injection boundary for the report.
type UsageDimension string

const (
	DimModel    UsageDimension = "model"
	DimProvider UsageDimension = "provider"
	DimAPIKey   UsageDimension = "api_key"
	DimAuthMode UsageDimension = "auth_mode"
	DimDay      UsageDimension = "day"
)

// usageDimSQL maps each allowed dimension to literal SQL fragments. The raw
// flag/query string is NEVER interpolated; it must resolve through this fixed
// allowlist first (see ParseDimension). This map is the single injection
// boundary for the usage report.
//
// The `day` bucket MUST use strftime(..., 'unixepoch'): created_at is stored as
// INTEGER unix-seconds, so a bare date(created_at) misreads the integer as a
// Julian day and returns garbage with no error.
var usageDimSQL = map[UsageDimension]string{
	DimModel:    "model_used",
	DimProvider: "provider",
	DimAPIKey:   "api_key_hash",
	DimAuthMode: "auth_mode",
	DimDay:      "strftime('%Y-%m-%d', created_at, 'unixepoch')",
}

// UsageFilter selects and groups rows for a usage report. By is a slice that is
// len==1 in Wave 1 but hedges the costly future interface break (CLI + HTTP +
// JSON all bind it) when multi-key grouping arrives.
type UsageFilter struct {
	OrgID string
	Start time.Time
	End   time.Time
	By    []UsageDimension
	Limit int
}

// UsageRow is one aggregated bucket. Token sums cover ALL statuses (tokens
// burned on failed requests are real signal). Two cost figures are exposed:
// CostUSD (all rows) and CostUSDBillable (status='ok', byte-for-byte reconciling
// with SumCostByOrg). Most traffic is flat-fee subscription where both are 0 by
// design — the CLI footer carries that interpretation once.
type UsageRow struct {
	Key                      string     `json:"key"`
	BucketStart              *time.Time `json:"bucket_start,omitempty"`
	BucketEnd                *time.Time `json:"bucket_end,omitempty"`
	Requests                 int64      `json:"requests"`
	PromptTokens             int64      `json:"prompt_tokens"`
	CompletionTokens         int64      `json:"completion_tokens"`
	TotalTokens              int64      `json:"total_tokens"`
	CacheReadInputTokens     int64      `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64      `json:"cache_creation_input_tokens"`
	CostUSD                  float64    `json:"cost_usd"`
	CostUSDBillable          float64    `json:"cost_usd_billable"`
	ErrorRate                float64    `json:"error_rate"`
}

// ParseDimension resolves a user-supplied group-by string against the fixed
// allowlist. It is the only safe way to turn input into a UsageDimension.
func ParseDimension(s string) (UsageDimension, error) {
	d := UsageDimension(strings.TrimSpace(s))
	if _, ok := usageDimSQL[d]; !ok {
		return "", fmt.Errorf("agentmodel/store: unknown usage dimension %q (want model|provider|api_key|auth_mode|day)", s)
	}
	return d, nil
}

// ParseSince converts a lookback window like "7d", "24h", or "30m" into a
// duration. time.ParseDuration tops out at hours and rejects "7d", so an Nd
// (days) form is handled here before delegating.
func ParseSince(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("agentmodel/store: empty lookback window")
	}
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, fmt.Errorf("agentmodel/store: invalid window %q: %w", s, err)
		}
		if n < 0 {
			return 0, fmt.Errorf("agentmodel/store: window must be non-negative, got %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("agentmodel/store: invalid window %q: %w", s, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("agentmodel/store: window must be non-negative, got %q", s)
	}
	return d, nil
}

// UsageReport aggregates request_logs into per-bucket rollups over [Start, End)
// for one org, grouped by exactly one dimension. It generalizes the proven
// SumCostByOrg/CountRequestsByOrg idioms with a GROUP BY whose column comes from
// the usageDimSQL allowlist — never from caller input.
func (s *SQLiteStore) UsageReport(ctx context.Context, f UsageFilter) ([]UsageRow, error) {
	if len(f.By) != 1 {
		return nil, fmt.Errorf("agentmodel/store: usage report requires exactly one group-by dimension, got %d", len(f.By))
	}
	dim := f.By[0]
	expr, ok := usageDimSQL[dim]
	if !ok {
		return nil, fmt.Errorf("agentmodel/store: unknown usage dimension %q", dim)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	// Day buckets read most naturally chronologically; everything else leads
	// with the heaviest consumer. Both order by an output-column alias.
	orderBy := "total_tokens DESC"
	if dim == DimDay {
		orderBy = "key ASC"
	}

	// expr (used for both SELECT and GROUP BY) and orderBy are literal fragments
	// from the fixed allowlist above — no caller input reaches the SQL text.
	// OrgID, Start, End and limit bind as `?` params.
	q := fmt.Sprintf(`
        SELECT
            %s AS key,
            COUNT(*) AS requests,
            COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens,
            COALESCE(SUM(completion_tokens), 0) AS completion_tokens,
            COALESCE(SUM(total_tokens), 0) AS total_tokens,
            COALESCE(SUM(cache_read_tokens), 0) AS cache_read_tokens,
            COALESCE(SUM(cache_creation_tokens), 0) AS cache_creation_tokens,
            COALESCE(SUM(cost_usd), 0) AS cost_usd,
            COALESCE(SUM(CASE WHEN status = 'ok' THEN cost_usd ELSE 0 END), 0) AS cost_usd_billable,
            CAST(SUM(CASE WHEN status = 'error' THEN 1 ELSE 0 END) AS REAL) / COUNT(*) AS error_rate
        FROM request_logs
        WHERE org_id = ? AND created_at >= ? AND created_at < ?
        GROUP BY %s
        ORDER BY %s
        LIMIT ?
    `, expr, expr, orderBy)

	rows, err := s.db.QueryContext(ctx, q, f.OrgID, f.Start.Unix(), f.End.Unix(), limit)
	if err != nil {
		return nil, fmt.Errorf("agentmodel/store: usage report: %w", err)
	}
	defer rows.Close()

	var out []UsageRow
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(
			&r.Key, &r.Requests, &r.PromptTokens, &r.CompletionTokens,
			&r.TotalTokens, &r.CacheReadInputTokens, &r.CacheCreationInputTokens,
			&r.CostUSD, &r.CostUSDBillable, &r.ErrorRate,
		); err != nil {
			return nil, fmt.Errorf("agentmodel/store: usage scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UsageHealth reports data-quality counts over the same window a UsageReport
// covers. The report's first real value is as a canary: provider is written ”
// on every row today (a live writer bug) and there is no Wave-1 multi-tenancy,
// so the report should surface degenerate dimensions rather than paper over them.
type UsageHealth struct {
	Total              int64
	UnsetProvider      int64
	UnsetModel         int64
	UnsetAPIKey        int64
	ZeroCostWithTokens int64
}

// UsageHealthReport counts rows whose attribution dimensions are unset (or whose
// cost is structurally zero despite non-zero tokens) over [start, end) for one
// org. It is intentionally not part of the Store interface — only the CLI, which
// holds the concrete *SQLiteStore, consumes it.
func (s *SQLiteStore) UsageHealthReport(ctx context.Context, orgID string, start, end time.Time) (UsageHealth, error) {
	var h UsageHealth
	err := s.db.QueryRowContext(ctx, `
        SELECT
            COUNT(*),
            COALESCE(SUM(CASE WHEN provider = '' THEN 1 ELSE 0 END), 0),
            COALESCE(SUM(CASE WHEN model_used = '' THEN 1 ELSE 0 END), 0),
            COALESCE(SUM(CASE WHEN api_key_hash = '' THEN 1 ELSE 0 END), 0),
            COALESCE(SUM(CASE WHEN cost_usd = 0 AND total_tokens > 0 THEN 1 ELSE 0 END), 0)
        FROM request_logs
        WHERE org_id = ? AND created_at >= ? AND created_at < ?
    `, orgID, start.Unix(), end.Unix()).Scan(
		&h.Total, &h.UnsetProvider, &h.UnsetModel, &h.UnsetAPIKey, &h.ZeroCostWithTokens,
	)
	if err != nil {
		return UsageHealth{}, fmt.Errorf("agentmodel/store: usage health: %w", err)
	}
	return h, nil
}

// OpenSQLiteReadOnly opens an existing DB for reading without running any DDL.
// A reporting command must not mutate schema-on-open or need write access
// against a live server's database; WAL mode lets it take a safe concurrent
// read snapshot.
//
// The `file:` prefix is REQUIRED for `mode=ro` to take effect — a bare
// `path?mode=ro` is silently treated as a literal filename. `query_only(1)` is
// avoided deliberately: it still opens read-write and needs write permissions.
func OpenSQLiteReadOnly(path string) (*SQLiteStore, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("agentmodel/store: database %q does not exist (has the gateway run yet?)", path)
		}
		return nil, fmt.Errorf("agentmodel/store: stat %q: %w", path, err)
	}
	dsn := "file:" + path + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("agentmodel/store: open read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(context.Background()); err != nil {
		return nil, errors.Join(fmt.Errorf("agentmodel/store: open read-only %q: %w", path, err), db.Close())
	}
	return &SQLiteStore{db: db, reporting: db}, nil
}
