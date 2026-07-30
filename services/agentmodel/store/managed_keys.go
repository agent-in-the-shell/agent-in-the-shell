package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Runtime virtual-key persistence (#922). The plaintext token never reaches
// this layer — callers hash it (api.hashAPIKey) and store only KeyHash. The
// nullable columns (max_budget, expires_at) map to typed pointers; models is a
// JSON array, stored "" when empty so an all-models key and a freshly decoded
// nil round-trip identically.

// managedKeyColumns is the column list every api_keys read projects, in the
// order scanManagedKey scans. Kept in one place so a schema change touches one
// string instead of three queries.
const managedKeyColumns = `id, name, key_hash, models, max_budget, budget_duration,
               expires_at, disabled, metadata, created_at`

func (s *SQLiteStore) CreateKey(ctx context.Context, k ManagedKey) error {
	if k.CreatedAt.IsZero() {
		k.CreatedAt = time.Now().UTC()
	}
	modelsJSON, err := encodeModels(k.Models)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
        INSERT INTO api_keys (
            id, name, key_hash, models, max_budget, budget_duration,
            expires_at, disabled, metadata, created_at
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    `,
		k.ID, k.Name, k.KeyHash, modelsJSON,
		nullFloat(k.MaxBudget), k.BudgetDuration,
		nullUnix(k.ExpiresAt), boolToInt(k.Disabled), k.Metadata, k.CreatedAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("agentmodel/store: create key: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetKeyByHash(ctx context.Context, keyHash string) (ManagedKey, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+managedKeyColumns+` FROM api_keys WHERE key_hash = ?`, keyHash)
	k, err := scanManagedKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ManagedKey{}, ErrNotFound
	}
	return k, err
}

func (s *SQLiteStore) GetKeyByID(ctx context.Context, id string) (ManagedKey, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+managedKeyColumns+` FROM api_keys WHERE id = ?`, id)
	k, err := scanManagedKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ManagedKey{}, ErrNotFound
	}
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
	res, err := s.db.ExecContext(ctx,
		`UPDATE api_keys SET disabled = ? WHERE id = ?`, boolToInt(disabled), id)
	if err != nil {
		return fmt.Errorf("agentmodel/store: set key disabled: %w", err)
	}
	return affectedOrNotFound(res, "set key disabled")
}

func (s *SQLiteStore) DeleteKey(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM api_keys WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("agentmodel/store: delete key: %w", err)
	}
	return affectedOrNotFound(res, "delete key")
}

func scanManagedKey(s rowScanner) (ManagedKey, error) {
	var (
		k          ManagedKey
		modelsJSON string
		maxBudget  sql.NullFloat64
		expiresAt  sql.NullInt64
		disabled   int
		createdAt  int64
	)
	err := s.Scan(
		&k.ID, &k.Name, &k.KeyHash, &modelsJSON, &maxBudget, &k.BudgetDuration,
		&expiresAt, &disabled, &k.Metadata, &createdAt,
	)
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
