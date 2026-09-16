package store

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	ErrServiceName         = errors.New("invalid service name")
	ErrServiceNameConflict = errors.New("service name already in use")
)

// NormalizeServiceName is shared by issuance and config-name collision checks.
// Labels retain case; uniqueness uses trimmed Unicode lowercase, not email or
// metadata heuristics. Names are bounded in bytes and controls are never labels.
func NormalizeServiceName(name string) (display, normalized string, err error) {
	if !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) {
		return "", "", ErrServiceName
	}
	display = strings.TrimSpace(name)
	if display == "" || len(display) > 128 {
		return "", "", ErrServiceName
	}
	return display, strings.ToLower(display), nil
}

// CreateServiceKey atomically creates a fresh credential and its classification.
// No existing key is adopted. Taking the writer lock before reading names also
// serializes issuance across gateway processes, not just the local pool.
func (s *SQLiteStore) CreateServiceKey(ctx context.Context, key ManagedKey) (PortalAdminKey, error) {
	name, normalized, err := NormalizeServiceName(key.Name)
	if err != nil {
		return PortalAdminKey{}, err
	}
	if key.ID == "" || key.KeyHash == "" || key.Disabled || key.RevokedAt != nil || !PortalPolicyOK(key) {
		return PortalAdminKey{}, ErrPortalPolicy
	}
	key.Name = name
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PortalAdminKey{}, err
	}
	defer tx.Rollback()
	// Literal [] is deny-all. Caller models/metadata are not issuance grants.
	key.Metadata = ""
	if err := insertManagedKey(ctx, tx, key, "[]"); err != nil {
		return PortalAdminKey{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT name FROM api_keys WHERE id != ?`, key.ID)
	if err != nil {
		return PortalAdminKey{}, err
	}
	for rows.Next() {
		var existing string
		if err := rows.Scan(&existing); err != nil {
			rows.Close()
			return PortalAdminKey{}, err
		}
		// Include disabled and legacy labels, even if they predate name validation.
		if strings.ToLower(strings.TrimSpace(existing)) == normalized {
			rows.Close()
			return PortalAdminKey{}, ErrServiceNameConflict
		}
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return PortalAdminKey{}, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO service_keys(key_id,normalized_name) VALUES (?,?) ON CONFLICT(normalized_name) DO NOTHING`, key.ID, normalized)
	if err != nil {
		return PortalAdminKey{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return PortalAdminKey{}, err
	}
	if n != 1 {
		return PortalAdminKey{}, ErrServiceNameConflict
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO service_key_hashes(key_id,key_hash) VALUES (?,?)`, key.ID, key.KeyHash); err != nil {
		return PortalAdminKey{}, err
	}
	if err := appendKeyEvent(ctx, tx, key.ID, "create"); err != nil {
		return PortalAdminKey{}, err
	}
	if err := tx.Commit(); err != nil {
		return PortalAdminKey{}, err
	}
	return PortalAdminKey{KeyID: key.ID, Name: name, Kind: "service", Revision: 1, State: "active", Models: []string{}}, nil
}

// RotateServiceKey replaces the secret under the writer lock and compares the
// observed revision. A concurrent rotate cannot return two apparently valid
// secrets. A revoked replacement starts deny-all; an independent pause remains.
func (s *SQLiteStore) RotateServiceKey(ctx context.Context, id string, revision int64, hash string) (ManagedKey, error) {
	if id == PortalMasterKeyID || revision < 1 || hash == "" {
		return ManagedKey{}, ErrPortalConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ManagedKey{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE service_keys SET revision=revision+1 WHERE key_id=? AND revision=? AND NOT EXISTS(SELECT 1 FROM portal_identities p WHERE p.key_id=service_keys.key_id)`, id, revision)
	if err != nil {
		return ManagedKey{}, err
	}
	if err := affectedOrNotFound(res, "rotate service"); err != nil {
		return ManagedKey{}, ErrPortalConflict
	}
	key, err := scanManagedKey(tx.QueryRowContext(ctx, `SELECT `+managedKeyColumns+` FROM api_keys WHERE id=?`, id))
	if err != nil {
		return ManagedKey{}, err
	}
	// Service auth already rejects unsupported budget/expiry policy. Never erase
	// those settings to make a rotated secret appear valid.
	if !PortalPolicyOK(key) {
		return ManagedKey{}, ErrPortalPolicy
	}
	if hash == key.KeyHash {
		return ManagedKey{}, ErrPortalConflict
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO service_key_hashes(key_id,key_hash) VALUES (?,?) ON CONFLICT(key_id,key_hash) DO NOTHING`, id, key.KeyHash); err != nil {
		return ManagedKey{}, err
	}
	// Plain INSERT intentionally rejects reuse of any historical service secret.
	if _, err := tx.ExecContext(ctx, `INSERT INTO service_key_hashes(key_id,key_hash) VALUES (?,?)`, id, hash); err != nil {
		return ManagedKey{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET key_hash=?,models=CASE WHEN revoked_at IS NULL THEN models ELSE '[]' END,revoked_at=NULL WHERE id=?`, hash, id); err != nil {
		return ManagedKey{}, err
	}
	if err := appendKeyEvent(ctx, tx, id, "rotate"); err != nil {
		return ManagedKey{}, err
	}
	if key.RevokedAt != nil {
		key.Models = []string{}
	}
	key.KeyHash = hash
	key.RevokedAt = nil
	key.ServiceIssued = true
	key.ServiceRevision = revision + 1
	if err := tx.Commit(); err != nil {
		return ManagedKey{}, err
	}
	return key, nil
}
