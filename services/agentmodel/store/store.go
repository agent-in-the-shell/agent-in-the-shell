// Package store persists agentmodel request logs for audit, cost reporting,
// and rate-limit / budget enforcement. Wave 1 uses SQLite; the Store interface
// is the seam Postgres / Spanner / etc. drop in behind in later waves.
package store

import (
	"context"
	"errors"
	"time"
)

// RequestLog is one persisted request's audit + cost trail.
type RequestLog struct {
	ID                       string // unique per request (UUIDv7 / response ID)
	RequestID                string // chi request-id; joins to the content log's request_id
	OrgID                    string // tenant identifier
	APIKeyHash               string // bcrypt or sha256 hash of the calling API key (never plaintext)
	ModelRequested           string // logical model name from the client (e.g. "gpt-4")
	ModelUsed                string // resolved provider model id (e.g. "gpt-4o", possibly different after fallback)
	Provider                 string // provider name ("openai", "anthropic-oauth", etc.)
	AuthMode                 string // "api_key" or "subscription"
	PromptTokens             int
	CompletionTokens         int
	TotalTokens              int
	CacheReadInputTokens     int       // cached prompt tokens served at discount rate
	CacheCreationInputTokens int       // tokens written to cache (Anthropic-only on first use)
	CostUSD                  float64   // 0 for subscription requests
	CostSource               string    // why CostUSD is what it is: "priced" | "subscription" | "unpriced" (empty = legacy/unknown). A $0 with "unpriced" means the price was missing, not free.
	LatencyMs                int       // wall-clock latency from request enter to last chunk
	Status                   string    // "ok" or "error"
	ErrorType                string    // populated on error
	CreatedAt                time.Time // unix-second precision; UTC
}

// ManagedKey is a runtime-issued virtual API key (DB-backed), as opposed to
// the static config keys resolved from env at boot (api.resolveKeys). Only the
// sha256 hash of the token is ever persisted — the plaintext is returned once
// at creation and never stored. The cap fields mirror config keys' shape so
// the existing budget machinery (api.resolveCap / SumCostByAPIKey) enforces
// them unchanged; usage attribution rides KeyHash through request_logs.
type ManagedKey struct {
	ID             string     // uuid
	Name           string     // operator-facing label; not unique (the hash is)
	KeyHash        string     // sha256 hex of the plaintext token; the only copy kept
	Models         []string   // logical-model allowlist; nil/empty = all models
	MaxBudget      *float64   // USD per window; nil = uncapped
	BudgetDuration string     // Go duration; "" = lifetime
	ExpiresAt      *time.Time // nil = never expires
	Disabled       bool       // revoked: never authenticates, but row (and its spend) is retained
	Metadata       string     // optional opaque JSON blob; stored verbatim
	CreatedAt      time.Time  // unix-second precision; UTC
}

// Store is the persistence boundary.
type Store interface {
	Ping(ctx context.Context) error
	LogRequest(ctx context.Context, log RequestLog) error
	GetRequestLog(ctx context.Context, id string) (RequestLog, error)
	ListByOrg(ctx context.Context, orgID string, since time.Time, limit int) ([]RequestLog, error)
	ListByAPIKey(ctx context.Context, apiKeyHash string, since time.Time, limit int) ([]RequestLog, error)
	SumCostByOrg(ctx context.Context, orgID string, since time.Time) (float64, error)
	// SumCostByAPIKey totals successful-request spend attributed to one API
	// key hash since the given time — the per-key read for budget enforcement
	// (#47). A zero `since` covers the whole ledger (lifetime caps).
	SumCostByAPIKey(ctx context.Context, apiKeyHash string, since time.Time) (float64, error)
	CountRequestsByOrg(ctx context.Context, orgID string, since time.Time, authMode string) (int64, error)
	// UsageReport aggregates request_logs into per-bucket rollups (requests,
	// tokens incl. the cache split, cost, error_rate) over a time window for
	// one org, grouped by a single dimension. It is the read surface behind the
	// `agent-model usage` CLI and the /v1/usage HTTP endpoint (#500).
	UsageReport(ctx context.Context, f UsageFilter) ([]UsageRow, error)
	// Purge deletes audit rows created before `before` and returns the number
	// removed. Retention is opt-in: rows persist indefinitely until purged.
	Purge(ctx context.Context, before time.Time) (int64, error)

	// --- Runtime virtual keys (#922) ---
	// CreateKey persists a freshly minted key. It fails if KeyHash collides
	// with an existing row (the hash is unique).
	CreateKey(ctx context.Context, k ManagedKey) error
	// GetKeyByHash resolves a key by its sha256 hash — the auth-path read.
	// Returns ErrNotFound if no row matches.
	GetKeyByHash(ctx context.Context, keyHash string) (ManagedKey, error)
	// GetKeyByID resolves a key by its id — the management read behind the
	// info/revoke endpoints. Returns ErrNotFound if no row matches.
	GetKeyByID(ctx context.Context, id string) (ManagedKey, error)
	// ListKeys returns all managed keys, newest first. Never includes plaintext
	// (there is none to include).
	ListKeys(ctx context.Context) ([]ManagedKey, error)
	// SetKeyDisabled toggles a key's revoked flag by id, returning ErrNotFound
	// if no row matches. Revoking retains the row so historical request_logs
	// stay attributable.
	SetKeyDisabled(ctx context.Context, id string, disabled bool) error
	// DeleteKey hard-deletes a key by id, returning ErrNotFound if no row
	// matches.
	DeleteKey(ctx context.Context, id string) error

	Close() error
}

// ErrNotFound is returned by the by-id / by-hash reads (GetRequestLog,
// GetKeyByHash) and the by-id mutations (SetKeyDisabled, DeleteKey) when no
// row matches.
var ErrNotFound = errors.New("agentmodel/store: row not found")
