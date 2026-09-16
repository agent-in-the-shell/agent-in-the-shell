package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// PortalIdentity uses the Access issuer and immutable subject, never email, as
// ownership. Email is only an operator label and a legacy-migration tripwire.
type PortalIdentity struct{ Issuer, Subject, Email string }

type PortalBinding struct {
	KeyID    string `json:"key_id"`
	Revision int64  `json:"revision"`
}

// PortalUsage is personal reporting only, never an authentication or budget input.
type PortalUsage struct {
	Stats    PortalStats    `json:"stats"`
	Calendar PortalCalendar `json:"calendar"`
}

type PortalStats struct {
	RequestCount     int64 `json:"request_count"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

type PortalCalendar struct {
	Year     int         `json:"year"`
	Timezone string      `json:"timezone"`
	Days     []PortalDay `json:"days"`
}

// Nil counts mark future dates, not zero usage. Past/current zeros mean no
// retained records, which cannot establish that no usage occurred.
type PortalDay struct {
	Date        string `json:"date"`
	TotalTokens *int64 `json:"total_tokens"`
	Requests    *int64 `json:"requests"`
}

var (
	ErrPortalConflict    = errors.New("portal identity already bound or revision changed")
	ErrMigrationRequired = errors.New("migration_required")
	ErrPortalPolicy      = errors.New("portal only supports uncapped keys without expiry")
)

// PortalStore isolates the temporary portal lifecycle from the core Store.
// Existing keys cannot be adopted; no budget/accounting methods are changed.
// PortalStore is everything the employee portal needs from persistence, kept
// off the core Store so the /v1 surface does not depend on it. The api package
// resolves it once at construction; only SQLiteStore implements it.
type PortalStore interface {
	CreateServiceKey(context.Context, ManagedKey) (PortalAdminKey, error)
	RotateServiceKey(context.Context, string, int64, string) (ManagedKey, error)
	GetKeyHistory(context.Context, string, int64) ([]KeyEvent, error)
	GetPortalBinding(context.Context, PortalIdentity) (PortalBinding, error)
	CreatePortalKey(context.Context, PortalIdentity, ManagedKey) (PortalBinding, error)
	RotatePortalKey(context.Context, PortalIdentity, int64, string) (PortalBinding, error)
	GetPortalUsage(context.Context, PortalIdentity, time.Time, ...string) (PortalUsage, error)
	SetPortalModels(context.Context, string, []string, ...int64) error
	GetPortalOverview(context.Context, time.Time, PortalReportScope) (PortalOverview, error)
	GetPortalMonitoring(context.Context, PortalMonitorFilter, PortalReportScope) (PortalMonitoring, error)
	SetPortalDisabled(context.Context, string, bool) error
	RevokePortalKey(context.Context, string) error
}

// PortalWindow is the rolling reporting window every portal total covers.
const PortalWindow = 30 * 24 * time.Hour

// PortalPolicyOK reports whether a key's policy is one the portal can own.
// Hash history is reporting only, not a cross-rotation budget ledger, so a
// budget or expiry on a portal key has no defined semantics: issuance and
// rotation refuse it, and auth rejects a key an operator edited into that state.
func PortalPolicyOK(k ManagedKey) bool {
	return k.MaxBudget == nil && k.ExpiresAt == nil && k.BudgetDuration == ""
}

func (s *PortalStats) add(o PortalStats) {
	s.RequestCount += o.RequestCount
	s.PromptTokens += o.PromptTokens
	s.CompletionTokens += o.CompletionTokens
	s.TotalTokens += o.TotalTokens
}

func (s *SQLiteStore) GetPortalUsage(ctx context.Context, identity PortalIdentity, now time.Time, timezone ...string) (PortalUsage, error) {
	ctx, cancel := context.WithTimeout(ctx, portalReportTimeout)
	defer cancel()
	// The optional argument preserves UTC for existing store callers.
	name := ""
	if len(timezone) > 1 {
		return PortalUsage{}, errors.New("expected one reporting timezone")
	}
	if len(timezone) == 1 {
		name = timezone[0]
	}
	loc, err := PortalReportingLocation(name)
	if err != nil {
		return PortalUsage{}, err
	}
	now = now.In(loc)
	_, offset := now.Zone()
	start := time.Date(now.Year(), time.January, 1, 0, 0, 0, 0, loc)
	cutoff := now.Add(-PortalWindow)
	// Scan the union of the two windows once; early January's summary also
	// includes December, whereas the calendar only covers the current year.
	since := start
	if cutoff.Before(since) {
		since = cutoff
	}
	// Shift only the grouping expression, never the stored instants or rolling cutoff.
	rows, err := s.reporting.QueryContext(ctx, `
		SELECT (r.created_at + ?) / 86400,
		       COUNT(*), COALESCE(SUM(r.total_tokens), 0),
		       SUM(CASE WHEN r.created_at >= ? THEN 1 ELSE 0 END),
		       SUM(CASE WHEN r.created_at >= ? THEN r.prompt_tokens ELSE 0 END),
		       SUM(CASE WHEN r.created_at >= ? THEN r.completion_tokens ELSE 0 END),
		       SUM(CASE WHEN r.created_at >= ? THEN r.total_tokens ELSE 0 END)
		FROM portal_identities p
		JOIN portal_key_hashes h ON h.key_id = p.key_id
		JOIN request_logs r ON r.api_key_hash = h.key_hash
		WHERE p.issuer = ? AND p.subject = ? AND r.created_at >= ? AND r.created_at <= ?
		GROUP BY (r.created_at + ?) / 86400`,
		offset, cutoff.Unix(), cutoff.Unix(), cutoff.Unix(), cutoff.Unix(), identity.Issuer, identity.Subject, since.Unix(), now.Unix(), offset)
	if err != nil {
		return PortalUsage{}, err
	}
	defer rows.Close()
	usage := PortalUsage{Calendar: PortalCalendar{Year: now.Year(), Timezone: loc.String()}}
	daily := make(map[string]PortalDay)
	for rows.Next() {
		var dayNumber, requests, tokens int64
		var stats PortalStats
		if err := rows.Scan(&dayNumber, &requests, &tokens, &stats.RequestCount, &stats.PromptTokens, &stats.CompletionTokens, &stats.TotalTokens); err != nil {
			return PortalUsage{}, err
		}
		date := time.Unix(dayNumber*86400, 0).UTC().Format("2006-01-02")
		daily[date] = PortalDay{Date: date, TotalTokens: &tokens, Requests: &requests}
		usage.Stats.add(stats)
	}
	if err := rows.Err(); err != nil {
		return PortalUsage{}, err
	}
	for day := start; day.Year() == now.Year(); day = day.AddDate(0, 0, 1) {
		date := day.Format("2006-01-02")
		entry := PortalDay{Date: date}
		if !day.After(now) {
			entry = daily[date]
			if entry.Date == "" {
				entry = PortalDay{Date: date, TotalTokens: new(int64), Requests: new(int64)}
			}
		}
		usage.Calendar.Days = append(usage.Calendar.Days, entry)
	}
	return usage, nil
}

func (s *SQLiteStore) GetPortalBinding(ctx context.Context, identity PortalIdentity) (PortalBinding, error) {
	var b PortalBinding
	err := s.db.QueryRowContext(ctx, `SELECT key_id, revision FROM portal_identities WHERE issuer = ? AND subject = ?`, identity.Issuer, identity.Subject).Scan(&b.KeyID, &b.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrNotFound
	}
	return b, err
}

func validPortalIdentity(i PortalIdentity) bool {
	return i.Issuer != "" && i.Subject != "" && strings.TrimSpace(i.Email) != ""
}

func (s *SQLiteStore) CreatePortalKey(ctx context.Context, identity PortalIdentity, key ManagedKey) (PortalBinding, error) {
	if !PortalPolicyOK(key) {
		return PortalBinding{}, ErrPortalPolicy
	}
	if !validPortalIdentity(identity) || key.ID == "" || key.KeyHash == "" {
		return PortalBinding{}, ErrPortalConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PortalBinding{}, err
	}
	defer tx.Rollback()
	// Claim ownership before checking labels: serializes duplicate creates even
	// from separate gateway processes, not just this connection pool.
	if err := insertPortalBinding(ctx, tx, identity, key.ID); err != nil {
		return PortalBinding{}, err
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM api_keys WHERE lower(trim(name)) = lower(trim(?)))`, identity.Email).Scan(&exists); err != nil {
		return PortalBinding{}, err
	}
	if exists {
		return PortalBinding{}, ErrMigrationRequired
	}
	// Issuance never grants inference, regardless of caller-supplied Models.
	// Unlike legacy keys, an explicit [] is a deny-all portal allowlist.
	if err := insertManagedKey(ctx, tx, key, "[]"); err != nil {
		return PortalBinding{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO portal_key_hashes(key_id,key_hash) VALUES (?,?)`, key.ID, key.KeyHash); err != nil {
		return PortalBinding{}, err
	}
	if err := appendKeyEvent(ctx, tx, key.ID, "create"); err != nil {
		return PortalBinding{}, err
	}
	if err := tx.Commit(); err != nil {
		return PortalBinding{}, err
	}
	return PortalBinding{KeyID: key.ID, Revision: 1}, nil
}

func insertPortalBinding(ctx context.Context, tx *sql.Tx, i PortalIdentity, keyID string) error {
	res, err := tx.ExecContext(ctx, `INSERT INTO portal_identities(issuer,subject,email,key_id) VALUES (?,?,?,?) ON CONFLICT DO NOTHING`, i.Issuer, i.Subject, strings.ToLower(strings.TrimSpace(i.Email)), keyID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrPortalConflict
	}
	return nil
}

func (s *SQLiteStore) RotatePortalKey(ctx context.Context, identity PortalIdentity, revision int64, hash string) (PortalBinding, error) {
	if !validPortalIdentity(identity) || revision < 1 || hash == "" {
		return PortalBinding{}, ErrPortalConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PortalBinding{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE portal_identities SET revision = revision + 1 WHERE issuer = ? AND subject = ? AND revision = ?`, identity.Issuer, identity.Subject, revision)
	if err != nil {
		return PortalBinding{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return PortalBinding{}, err
	}
	if n != 1 {
		return PortalBinding{}, ErrPortalConflict
	}
	var b PortalBinding
	if err := tx.QueryRowContext(ctx, `SELECT key_id,revision FROM portal_identities WHERE issuer = ? AND subject = ?`, identity.Issuer, identity.Subject).Scan(&b.KeyID, &b.Revision); err != nil {
		return b, err
	}
	key, err := scanManagedKey(tx.QueryRowContext(ctx, `SELECT `+managedKeyColumns+` FROM api_keys WHERE id = ?`, b.KeyID))
	if errors.Is(err, sql.ErrNoRows) {
		return PortalBinding{}, ErrNotFound
	}
	if err != nil {
		return PortalBinding{}, err
	}
	// Including operator edits made after issuance; see PortalPolicyOK.
	if !PortalPolicyOK(key) {
		return PortalBinding{}, ErrPortalPolicy
	}
	// A revoked credential is never restored: replacement uses a fresh secret,
	// retains the one logical identity/history, and starts with zero grants.
	// A reversible pause survives both normal rotation and replacement.
	if key.RevokedAt != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET models = '[]', revoked_at = NULL WHERE id = ?`, b.KeyID); err != nil {
			return PortalBinding{}, err
		}
	}
	// Rotate only the secret for a non-revoked key: scope and pause survive.
	res, err = tx.ExecContext(ctx, `UPDATE api_keys SET key_hash = ? WHERE id = ?`, hash, b.KeyID)
	if err != nil {
		return PortalBinding{}, err
	}
	if err := affectedOrNotFound(res, "rotate portal key"); err != nil {
		return PortalBinding{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO portal_key_hashes(key_id,key_hash) VALUES (?,?)`, b.KeyID, hash); err != nil {
		return PortalBinding{}, err
	}
	if err := appendKeyEvent(ctx, tx, b.KeyID, "rotate"); err != nil {
		return PortalBinding{}, err
	}
	if err := tx.Commit(); err != nil {
		return PortalBinding{}, err
	}
	return b, nil
}
