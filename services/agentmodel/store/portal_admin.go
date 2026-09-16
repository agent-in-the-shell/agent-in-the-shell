package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type PortalAdminKey struct {
	Revision  int64       `json:"revision,omitempty"`
	RevokedAt *time.Time  `json:"revoked_at,omitempty"`
	KeyID     string      `json:"key_id"`
	Name      string      `json:"name"`
	Kind      string      `json:"kind"`
	State     string      `json:"state"`
	Models    []string    `json:"models"`
	Stats     PortalStats `json:"stats"`
}

type PortalOverview struct {
	Since time.Time        `json:"since"`
	Until time.Time        `json:"until"`
	Keys  []PortalAdminKey `json:"keys"`
	Stats PortalStats      `json:"stats"`
}

func validPolicyModels(models []string) bool {
	seen := make(map[string]bool)
	for _, model := range models {
		if model == "" || strings.TrimSpace(model) != model || seen[model] {
			return false
		}
		seen[model] = true
	}
	return true
}

// SetPortalModels changes only an employee or explicit service key's allowlist.
// Classification and revocation are part of the write, so legacy/retired keys cannot
// be edited through this API. Catalog validation belongs to the HTTP boundary.
func (s *SQLiteStore) SetPortalModels(ctx context.Context, keyID string, models []string, revision ...int64) error {
	if keyID == PortalMasterKeyID {
		return ErrNotFound
	}
	if models == nil || !validPolicyModels(models) {
		return errors.New("models must be an explicit valid array")
	}
	encoded, err := json.Marshal(models)
	if err != nil {
		return err
	}
	// Service edits from HTTP carry the observed credential revision. Store-only
	// maintenance callers can omit it, but all paths serialize against rotation.
	if len(revision) > 1 {
		return ErrPortalConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	query := `UPDATE api_keys SET models=? WHERE id=? AND revoked_at IS NULL AND (EXISTS(SELECT 1 FROM portal_identities p WHERE p.key_id=api_keys.id) OR EXISTS(SELECT 1 FROM service_keys sk WHERE sk.key_id=api_keys.id))`
	args := []any{string(encoded), keyID}
	if len(revision) == 1 {
		query += ` AND NOT EXISTS(SELECT 1 FROM service_keys sk WHERE sk.key_id=api_keys.id AND sk.revision!=?)`
		args = append(args, revision[0])
	}
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	if err := affectedOrNotFound(res, "set models"); err != nil {
		if len(revision) == 1 {
			return ErrPortalConflict
		}
		return err
	}
	if err := appendKeyEvent(ctx, tx, keyID, "models"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) GetPortalOverview(ctx context.Context, now time.Time, scope PortalReportScope) (PortalOverview, error) {
	ctx, cancel := context.WithTimeout(ctx, portalReportTimeout)
	defer cancel()
	result := PortalOverview{Since: now.UTC().Add(-PortalWindow), Until: now.UTC(), Keys: []PortalAdminKey{}}
	// Tombstones stay visible; no prompt, token, or hash leaves here. Attribution
	// uses the same ownership relation as monitoring.
	rows, err := s.reporting.QueryContext(ctx, portalReportCTE+`, owners AS (
 SELECT key_id,owner AS name,'master' AS kind,'active' AS state,'null' AS models, NULL AS revoked_at FROM report_master
 UNION ALL
  SELECT p.key_id, p.email AS name, 'portal' AS kind,
   CASE WHEN k.id IS NULL OR k.revoked_at IS NOT NULL THEN 'revoked' WHEN k.disabled != 0 THEN 'disabled'
        WHEN k.expires_at IS NOT NULL AND k.expires_at <= ? THEN 'expired' ELSE 'active' END AS state,
   COALESCE(NULLIF(k.models,''),'[]') AS models, k.revoked_at
  FROM portal_identities p LEFT JOIN api_keys k ON k.id=p.key_id WHERE p.key_id != '`+PortalMasterKeyID+`'
  UNION ALL
  SELECT k.id, k.name, CASE WHEN sk.key_id IS NOT NULL THEN 'service' ELSE 'legacy_current_hash' END,
   CASE WHEN k.revoked_at IS NOT NULL THEN 'revoked' WHEN k.disabled != 0 THEN 'disabled' WHEN k.expires_at IS NOT NULL AND k.expires_at <= ? THEN 'expired' ELSE 'active' END, CASE WHEN sk.key_id IS NOT NULL THEN COALESCE(NULLIF(k.models,''),'[]') ELSE 'null' END, k.revoked_at
  FROM api_keys k LEFT JOIN service_keys sk ON sk.key_id=k.id WHERE k.id != '`+PortalMasterKeyID+`' AND NOT EXISTS(SELECT 1 FROM portal_identities p WHERE p.key_id=k.id)
 ), hashes AS (`+portalHashOwnership+`)
 SELECT o.key_id,o.name,o.kind,o.state,o.models,o.revoked_at,COALESCE((SELECT revision FROM service_keys sk WHERE sk.key_id=o.key_id),0),COUNT(r.id),COALESCE(SUM(r.prompt_tokens),0),COALESCE(SUM(r.completion_tokens),0),COALESCE(SUM(r.total_tokens),0)
 FROM owners o LEFT JOIN hashes h ON h.key_id=o.key_id
 LEFT JOIN request_logs r ON r.api_key_hash=h.key_hash AND r.created_at >= ? AND r.created_at <= ?
 GROUP BY o.key_id,o.name,o.kind,o.state,o.models,o.revoked_at ORDER BY o.kind,o.name,o.key_id`, scope.currentMasterHash, now.Unix(), now.Unix(), result.Since.Unix(), now.Unix())
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var k PortalAdminKey
		var models string
		var revoked sql.NullInt64
		if err := rows.Scan(&k.KeyID, &k.Name, &k.Kind, &k.State, &models, &revoked, &k.Revision, &k.Stats.RequestCount, &k.Stats.PromptTokens, &k.Stats.CompletionTokens, &k.Stats.TotalTokens); err != nil {
			return result, err
		}
		if revoked.Valid {
			t := time.Unix(revoked.Int64, 0).UTC()
			k.RevokedAt = &t
		}
		if err := json.Unmarshal([]byte(models), &k.Models); err != nil {
			return result, err
		}
		if (k.Kind == "portal" || k.Kind == "service") && k.Models == nil {
			k.Models = []string{}
		}
		result.Keys = append(result.Keys, k)
		result.Stats.add(k.Stats)
	}
	return result, rows.Err()
}

// RevokePortalKey excludes config/master/legacy and preserves reporting joins.
func (s *SQLiteStore) RevokePortalKey(ctx context.Context, id string) error {
	if id == PortalMasterKeyID {
		return ErrNotFound
	}
	return s.keyMutation(ctx, id, "revoke", `UPDATE api_keys SET revoked_at=COALESCE(revoked_at,?) WHERE id=? AND (EXISTS(SELECT 1 FROM portal_identities p WHERE p.key_id=api_keys.id) OR EXISTS(SELECT 1 FROM service_keys sk WHERE sk.key_id=api_keys.id))`, time.Now().UTC().Unix(), id)
}
