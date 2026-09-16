package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Runtime virtual-key persistence. The plaintext token never reaches
// this layer — callers hash it (api.hashAPIKey) and store only KeyHash. The
// nullable columns (max_budget, expires_at) map to typed pointers; models is a
// JSON array, stored "" when empty so an all-models key and a freshly decoded
// nil round-trip identically.

// managedKeyColumns is the column list every api_keys read projects, in the
// order scanManagedKey scans. Kept in one place so a schema change touches one
// string instead of three queries.
const managedKeyColumns = `id, name, key_hash, models, max_budget, budget_duration,
               expires_at, disabled, metadata, created_at, revoked_at`

func (s *SQLiteStore) CreateKey(ctx context.Context, k ManagedKey) error {
	modelsJSON, err := encodeModels(k.Models)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertManagedKey(ctx, tx, k, modelsJSON); err != nil {
		return fmt.Errorf("agentmodel/store: create key: %w", err)
	}
	if err := appendKeyEvent(ctx, tx, k.ID, "create"); err != nil {
		return err
	}
	return tx.Commit()
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// insertManagedKey is the single INSERT into api_keys; CreateKey and the
// portal issuance transaction both use it, so a schema change touches one
// statement. modelsJSON is passed pre-encoded because the portal deliberately
// stores "[]" (a deny-all allowlist) where encodeModels would store "".
func insertManagedKey(ctx context.Context, db execer, k ManagedKey, modelsJSON string) error {
	if k.CreatedAt.IsZero() {
		k.CreatedAt = time.Now().UTC()
	}
	_, err := db.ExecContext(ctx, `INSERT INTO api_keys (`+managedKeyColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.Name, k.KeyHash, modelsJSON,
		nullFloat(k.MaxBudget), k.BudgetDuration,
		nullUnix(k.ExpiresAt), boolToInt(k.Disabled), k.Metadata, k.CreatedAt.Unix(), nullUnix(k.RevokedAt),
	)
	return err
}

func (s *SQLiteStore) GetKeyByHash(ctx context.Context, keyHash string) (ManagedKey, error) {
	// Employee ownership and explicit service classification ride on the same hit; auth
	// stays at one round trip on the single pooled connection.
	row := s.db.QueryRowContext(ctx, `SELECT `+managedKeyColumns+`, EXISTS(SELECT 1 FROM portal_identities p WHERE p.key_id = api_keys.id), EXISTS(SELECT 1 FROM service_keys sk WHERE sk.key_id = api_keys.id), COALESCE((SELECT revision FROM service_keys sk WHERE sk.key_id=api_keys.id),0) FROM api_keys WHERE key_hash = ?`, keyHash)
	var issued, service int
	var revision int64
	k, err := scanManagedKey(row, &issued, &service, &revision)
	k.ServiceRevision = revision
	if errors.Is(err, sql.ErrNoRows) {
		return ManagedKey{}, ErrNotFound
	}
	k.PortalIssued = issued != 0
	k.ServiceIssued = service != 0
	return k, err
}

func (s *SQLiteStore) GetKeyByID(ctx context.Context, id string) (ManagedKey, error) {
	// Resolve ownership in the same snapshot as the stable-ID lookup. A later
	// lookup by the returned hash could miss if the credential rotated meanwhile.
	row := s.db.QueryRowContext(ctx, `SELECT `+managedKeyColumns+`, EXISTS(SELECT 1 FROM portal_identities p WHERE p.key_id = api_keys.id), EXISTS(SELECT 1 FROM service_keys sk WHERE sk.key_id = api_keys.id), COALESCE((SELECT revision FROM service_keys sk WHERE sk.key_id=api_keys.id),0) FROM api_keys WHERE id = ?`, id)
	var issued, service int
	var revision int64
	k, err := scanManagedKey(row, &issued, &service, &revision)
	k.ServiceRevision = revision
	if errors.Is(err, sql.ErrNoRows) {
		return ManagedKey{}, ErrNotFound
	}
	k.PortalIssued = issued != 0
	k.ServiceIssued = service != 0
	return k, err
}

func (s *SQLiteStore) ListKeys(ctx context.Context) ([]ManagedKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+managedKeyColumns+` FROM api_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("agentmodel/store: list keys: %w", err)
	}
	defer rows.Close()
	var out []ManagedKey
	for rows.Next() {
		k, err := scanManagedKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) SetKeyDisabled(ctx context.Context, id string, disabled bool) error {
	if id == PortalMasterKeyID {
		return ErrNotFound
	}
	action := "enable"
	if disabled {
		action = "disable"
	}
	return s.keyMutation(ctx, id, action, `UPDATE api_keys SET disabled=? WHERE id=? AND revoked_at IS NULL`, boolToInt(disabled), id)
}

func (s *SQLiteStore) DeleteKey(ctx context.Context, id string) error {
	return s.keyMutation(ctx, id, "revoke", `UPDATE api_keys SET revoked_at=COALESCE(revoked_at,?) WHERE id=? AND id!=?`, time.Now().UTC().Unix(), id, PortalMasterKeyID)
}

// extra receives any columns a caller projects after managedKeyColumns.
func scanManagedKey(s rowScanner, extra ...any) (ManagedKey, error) {
	var (
		k          ManagedKey
		modelsJSON string
		maxBudget  sql.NullFloat64
		expiresAt  sql.NullInt64
		revokedAt  sql.NullInt64
		disabled   int
		createdAt  int64
	)
	err := s.Scan(append([]any{
		&k.ID, &k.Name, &k.KeyHash, &modelsJSON, &maxBudget, &k.BudgetDuration,
		&expiresAt, &disabled, &k.Metadata, &createdAt, &revokedAt,
	}, extra...)...)
	if err != nil {
		return ManagedKey{}, err
	}
	k.Models, err = decodeModels(modelsJSON)
	if err != nil {
		return ManagedKey{}, err
	}
	if maxBudget.Valid {
		k.MaxBudget = &maxBudget.Float64
	}
	if expiresAt.Valid {
		t := time.Unix(expiresAt.Int64, 0).UTC()
		k.ExpiresAt = &t
	}
	if revokedAt.Valid {
		t := time.Unix(revokedAt.Int64, 0).UTC()
		k.RevokedAt = &t
	}
	k.Disabled = disabled != 0
	k.CreatedAt = time.Unix(createdAt, 0).UTC()
	return k, nil
}

// encodeModels serializes the allowlist to JSON, collapsing nil/empty to ""
// so an all-models key stores no array.
func encodeModels(models []string) (string, error) {
	if len(models) == 0 {
		return "", nil
	}
	b, err := json.Marshal(models)
	if err != nil {
		return "", fmt.Errorf("agentmodel/store: encode models: %w", err)
	}
	return string(b), nil
}

func decodeModels(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("agentmodel/store: decode models: %w", err)
	}
	return out, nil
}

func nullFloat(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullUnix(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Unix()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func affectedOrNotFound(res sql.Result, op string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("agentmodel/store: %s rows affected: %w", op, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
