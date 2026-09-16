package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PortalMasterKeyID is a reserved reporting identity, never a managed key.
const PortalMasterKeyID = "system:master"
const PortalMasterKeyName = "Master key"

// PortalReportScope is supplied by the server, separately from browser filters.
// The private credential-derived value cannot be serialized into report JSON.
// Its zero value retains managed-only reporting; an empty master is disabled.
type PortalReportScope struct{ currentMasterHash string }

func NewPortalReportScope(currentMasterHash string) PortalReportScope {
	return PortalReportScope{currentMasterHash: currentMasterHash}
}

func (PortalReportScope) String() string     { return "PortalReportScope{redacted}" }
func (s PortalReportScope) GoString() string { return s.String() }

// Bind once per report, including narrow request projections. No secret SQL literals.
const portalReportCTE = "WITH report_master AS (SELECT '" + PortalMasterKeyID + "' AS key_id,'" + PortalMasterKeyName + "' AS owner,key_hash FROM (SELECT ? AS key_hash) WHERE key_hash != '') "

type PortalMonitorFilter struct {
	// Empty timezone retains UTC; only UTC and Asia/Taipei are supported.
	Timezone string `json:"timezone,omitempty"`
	// Empty/full preserves the original report. Projections also select SQL work.
	View     string    `json:"view,omitempty"`
	Since    time.Time `json:"since"`
	Until    time.Time `json:"until"`
	Bucket   string    `json:"bucket"`
	KeyID    string    `json:"key_id,omitempty"`
	User     string    `json:"user,omitempty"`
	Model    string    `json:"model,omitempty"`
	Provider string    `json:"provider,omitempty"`
	AuthMode string    `json:"auth_mode,omitempty"`
	Status   string    `json:"status,omitempty"`
	Page     int       `json:"page"`
	PageSize int       `json:"page_size"`
}

type PortalMonitorStats struct {
	PortalStats
	ErrorCount             int64    `json:"error_count"`
	CacheReadTokens        int64    `json:"cache_read_tokens"`
	CacheCreationTokens    int64    `json:"cache_creation_tokens"`
	ReasoningTokens        int64    `json:"reasoning_tokens"`
	ReasoningKnownRequests int64    `json:"reasoning_known_requests"`
	MeanLatencyMs          *float64 `json:"mean_latency_ms"`
	SubscriptionRequests   int64    `json:"subscription_requests"`
	PricedRequests         int64    `json:"priced_requests"`
	PricedCostUSD          float64  `json:"priced_cost_usd"`
	UnpricedRequests       int64    `json:"unpriced_requests"`
	ResponseCacheRequests  int64    `json:"response_cache_requests"`
	UnknownCostRequests    int64    `json:"unknown_cost_requests"`
}

type PortalMonitorBucket struct {
	Start time.Time          `json:"start"`
	Stats PortalMonitorStats `json:"stats"`
}

type PortalMonitorGroup struct {
	ID    string             `json:"id"`
	Name  string             `json:"name"`
	Stats PortalMonitorStats `json:"stats"`
}

// Deliberate allowlist: never expose the log's hash, request correlation ID,
// error type/body, or managed-key metadata through the browser.
type PortalMonitorRequest struct {
	ID                  string    `json:"id"`
	KeyID               string    `json:"key_id"`
	User                string    `json:"user"`
	CreatedAt           time.Time `json:"created_at"`
	Model               string    `json:"model"`
	ModelUsed           string    `json:"model_used"`
	Provider            string    `json:"provider"`
	AuthMode            string    `json:"auth_mode"`
	Status              string    `json:"status"`
	PromptTokens        int64     `json:"prompt_tokens"`
	CompletionTokens    int64     `json:"completion_tokens"`
	TotalTokens         int64     `json:"total_tokens"`
	CacheReadTokens     int64     `json:"cache_read_tokens"`
	CacheCreationTokens int64     `json:"cache_creation_tokens"`
	ReasoningTokens     *int64    `json:"reasoning_tokens"`
	LatencyMs           int64     `json:"latency_ms"`
	CostClass           string    `json:"cost_class"`
	PricedCostUSD       *float64  `json:"priced_cost_usd"`
}

type PortalMonitorOwner struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type PortalMonitorOptions struct {
	Users     []PortalMonitorOwner `json:"users"`
	Models    []string             `json:"models"`
	Providers []string             `json:"providers"`
}

// Projections populate only their selected fields. RequestCount belongs to the
// requests projection; aggregate Stats is intentionally not computed there.
type PortalMonitoring struct {
	RequestCount int64                  `json:"-"`
	Options      PortalMonitorOptions   `json:"options"`
	Filter       PortalMonitorFilter    `json:"filter"`
	Scope        string                 `json:"scope"`
	Stats        PortalMonitorStats     `json:"stats"`
	P95LatencyMs *int64                 `json:"p95_latency_ms"`
	Buckets      []PortalMonitorBucket  `json:"buckets"`
	Models       []PortalMonitorGroup   `json:"models"`
	Users        []PortalMonitorGroup   `json:"users"`
	Requests     []PortalMonitorRequest `json:"requests"`
}

func (s *SQLiteStore) SetPortalDisabled(ctx context.Context, keyID string, disabled bool) error {
	if keyID == PortalMasterKeyID {
		return ErrNotFound
	}
	action := "enable"
	if disabled {
		action = "disable"
	}
	return s.keyMutation(ctx, keyID, action, `UPDATE api_keys SET disabled=? WHERE id=? AND revoked_at IS NULL AND (EXISTS(SELECT 1 FROM portal_identities p WHERE p.key_id=api_keys.id) OR EXISTS(SELECT 1 FROM service_keys sk WHERE sk.key_id=api_keys.id))`, boolToInt(disabled), keyID)
}

// Keep bounds at the store boundary as well as HTTP: the zero-filled chart and
// SQL offset must stay bounded even when a future caller bypasses the handler.
func (f PortalMonitorFilter) Validate() error {
	if _, err := PortalReportingLocation(f.Timezone); err != nil {
		return err
	}
	switch f.View {
	case "", "full", "summary", "requests", "options":
	default:
		return errors.New("invalid monitoring view")
	}
	if f.Since.IsZero() || !f.Until.After(f.Since) || f.Since.Unix() < 0 || f.Until.Sub(f.Since) > 366*24*time.Hour || f.Page < 1 || f.Page > 1000000 || f.PageSize < 1 || f.PageSize > 100 {
		return errors.New("invalid monitoring range or pagination")
	}
	if f.Bucket != "day" && f.Bucket != "hour" {
		return errors.New("invalid monitoring bucket")
	}
	if f.Bucket == "hour" && f.Until.Sub(f.Since) > 31*24*time.Hour {
		return errors.New("hourly range exceeds 31 days")
	}
	if f.AuthMode != "" && f.AuthMode != "api_key" && f.AuthMode != "subscription" {
		return errors.New("invalid auth mode")
	}
	if f.Status != "" && f.Status != "ok" && f.Status != "error" {
		return errors.New("invalid status")
	}
	for _, value := range []string{f.KeyID, f.User, f.Model, f.Provider} {
		if len(value) > 256 || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("invalid monitoring filter")
		}
	}
	return nil
}

// One attribution relation for every projection and the overview. The current
// master wins (matching bearer authentication precedence), then historical portal
// ownership, then current managed hashes. Each hash joins at most once. The
// reserved public ID cannot be impersonated by a persisted managed identity.
const portalHashOwnership = `SELECT key_id,owner,key_hash FROM report_master
 UNION ALL
 SELECT p.key_id,p.email AS owner,h.key_hash FROM portal_identities p JOIN portal_key_hashes h ON h.key_id=p.key_id
 WHERE p.key_id != '` + PortalMasterKeyID + `' AND NOT EXISTS(SELECT 1 FROM report_master m WHERE m.key_hash=h.key_hash)
 UNION ALL
SELECT k.id,k.name,h.key_hash FROM api_keys k JOIN service_key_hashes h ON h.key_id=k.id
WHERE k.id != '` + PortalMasterKeyID + `' AND NOT EXISTS(SELECT 1 FROM portal_identities p WHERE p.key_id=k.id)
AND NOT EXISTS(SELECT 1 FROM portal_key_hashes p WHERE p.key_hash=h.key_hash)
AND NOT EXISTS(SELECT 1 FROM report_master m WHERE m.key_hash=h.key_hash)
 UNION ALL
 SELECT k.id,k.name,k.key_hash FROM api_keys k WHERE k.id != '` + PortalMasterKeyID + `' AND NOT EXISTS(SELECT 1 FROM portal_identities p WHERE p.key_id=k.id)
 AND NOT EXISTS(SELECT 1 FROM portal_key_hashes h WHERE h.key_hash=k.key_hash)
 AND NOT EXISTS(SELECT 1 FROM service_key_hashes h WHERE h.key_hash=k.key_hash)
 AND NOT EXISTS(SELECT 1 FROM report_master m WHERE m.key_hash=k.key_hash)`

// Share attribution and time scoping across all projections. Only the dashboard
// materializes wide rows for reuse by its aggregates; pagination selects IDs
// from an inlined narrow scope and loads metadata only after LIMIT/OFFSET.
const portalMonitorFrom = ` FROM request_logs r JOIN (` + portalHashOwnership + `) h ON h.key_hash=r.api_key_hash
 WHERE r.created_at >= ? AND r.created_at < ?`
const portalMonitorNarrow = `SELECT r.id,h.key_id,h.owner,r.created_at,r.model_requested,r.provider,r.auth_mode,r.status` + portalMonitorFrom
const portalMonitorCostClass = `CASE WHEN r.cost_source='cache' THEN 'response_cache'
      WHEN r.auth_mode='subscription' OR r.cost_source='subscription' THEN 'subscription'
      WHEN r.cost_source='priced' THEN 'priced'
      WHEN r.cost_source='unpriced' THEN 'unpriced' ELSE 'unknown' END`
const portalMonitorScope = `CREATE TEMP TABLE portal_scope AS ` + portalReportCTE + `
 SELECT r.id,h.key_id,h.owner,r.created_at,r.model_requested,r.model_used,r.provider,r.auth_mode,r.status,
 r.prompt_tokens,r.completion_tokens,r.total_tokens,r.cache_read_tokens,r.cache_creation_tokens,r.reasoning_tokens,r.latency_ms,r.cost_usd,
 ` + portalMonitorCostClass + ` AS cost_class` + portalMonitorFrom

const portalMonitorAggregate = `COUNT(*),COALESCE(SUM(prompt_tokens),0),COALESCE(SUM(completion_tokens),0),COALESCE(SUM(total_tokens),0),
 COALESCE(SUM(status='error'),0),COALESCE(SUM(cache_read_tokens),0),COALESCE(SUM(cache_creation_tokens),0),COALESCE(SUM(reasoning_tokens),0),COUNT(reasoning_tokens),AVG(latency_ms),
 COALESCE(SUM(cost_class='subscription'),0),COALESCE(SUM(cost_class='priced'),0),COALESCE(SUM(CASE WHEN cost_class='priced' THEN cost_usd ELSE 0 END),0),
 COALESCE(SUM(cost_class='unpriced'),0),COALESCE(SUM(cost_class='response_cache'),0),COALESCE(SUM(cost_class='unknown'),0)`

func (s *PortalMonitorStats) scanTargets() []any {
	return []any{&s.RequestCount, &s.PromptTokens, &s.CompletionTokens, &s.TotalTokens, &s.ErrorCount, &s.CacheReadTokens, &s.CacheCreationTokens, &s.ReasoningTokens, &s.ReasoningKnownRequests, &s.MeanLatencyMs, &s.SubscriptionRequests, &s.PricedRequests, &s.PricedCostUSD, &s.UnpricedRequests, &s.ResponseCacheRequests, &s.UnknownCostRequests}
}

func portalMonitorWhere(f PortalMonitorFilter) (string, []any) {
	// All metrics, pages and owner suggestions share exact dimension filters.
	// Model/provider options alone ignore them so selection never hides choices.
	where, args := " WHERE 1=1", []any{}
	for _, filter := range []struct{ column, value string }{{"key_id", f.KeyID}, {"owner", f.User}, {"model_requested", f.Model}, {"provider", f.Provider}, {"auth_mode", f.AuthMode}, {"status", f.Status}} {
		if filter.value != "" {
			where += " AND " + filter.column + " = ?"
			args = append(args, filter.value)
		}
	}
	return where, args
}

func (s *SQLiteStore) GetPortalMonitoring(ctx context.Context, f PortalMonitorFilter, scopeConfig PortalReportScope) (result PortalMonitoring, err error) {
	ctx, cancel := context.WithTimeout(ctx, portalReportTimeout)
	defer cancel()
	result = PortalMonitoring{Filter: f, Scope: "retained_managed_keys", Buckets: []PortalMonitorBucket{}, Models: []PortalMonitorGroup{}, Users: []PortalMonitorGroup{}, Requests: []PortalMonitorRequest{}}
	if err := f.Validate(); err != nil {
		return result, err
	}
	if scopeConfig.currentMasterHash != "" {
		result.Scope = "retained_managed_keys_and_current_master"
	}
	where, args := portalMonitorWhere(f)
	base := "SELECT %s FROM portal_scope" + where
	// Every projection uses one read snapshot; rollback also removes any temp
	// scope from the pooled connection. Separate responses are not one snapshot.
	tx, err := s.reporting.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer func() {
		// Cancellation may already have rolled back the transaction. Any other
		// cleanup failure must reach the caller, not silently return a report.
		if rollbackErr := tx.Rollback(); !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
	}()
	if f.View == "requests" {
		prefix := portalReportCTE + ", portal_scope AS NOT MATERIALIZED (" + portalMonitorNarrow + ") "
		scopedArgs := append([]any{scopeConfig.currentMasterHash, f.Since.Unix(), f.Until.Unix()}, args...)
		if err := tx.QueryRowContext(ctx, prefix+fmt.Sprintf(base, "COUNT(*)"), scopedArgs...).Scan(&result.RequestCount); err != nil {
			return result, err
		}
		// The subquery's LIMIT prevents flattening: sorting/offset carries only IDs
		// and ownership, never the wide token/cost/latency metadata of the full scope.
		page := fmt.Sprintf(base, "id,key_id,owner") + " ORDER BY created_at DESC,id DESC LIMIT ? OFFSET ?"
		query := prefix + `SELECT r.id,p.key_id,p.owner,r.created_at,r.model_requested,r.model_used,r.provider,r.auth_mode,r.status,
   r.prompt_tokens,r.completion_tokens,r.total_tokens,r.cache_read_tokens,r.cache_creation_tokens,r.reasoning_tokens,r.latency_ms,
   ` + portalMonitorCostClass + `,CASE WHEN (` + portalMonitorCostClass + `)='priced' THEN r.cost_usd ELSE NULL END
   FROM (` + page + `) p JOIN request_logs r ON r.id=p.id ORDER BY r.created_at DESC,r.id DESC`
		result.Requests, err = portalMonitorRequests(ctx, tx, query, append(scopedArgs, f.PageSize, (f.Page-1)*f.PageSize))
		return result, err
	}
	scope := portalMonitorScope
	if f.View == "options" {
		scope = "CREATE TEMP TABLE portal_scope AS " + portalReportCTE + portalMonitorNarrow
	}
	if _, err := tx.ExecContext(ctx, scope, scopeConfig.currentMasterHash, f.Since.Unix(), f.Until.Unix()); err != nil {
		return result, err
	}
	result.Options = PortalMonitorOptions{Models: []string{}, Providers: []string{}, Users: []PortalMonitorOwner{}}
	for _, option := range []struct {
		column string
		target *[]string
	}{{"model_requested", &result.Options.Models}, {"provider", &result.Options.Providers}} {
		rows, err := tx.QueryContext(ctx, "SELECT DISTINCT "+option.column+" FROM portal_scope WHERE "+option.column+" != '' ORDER BY 1")
		if err != nil {
			return result, err
		}
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				rows.Close()
				return result, err
			}
			*option.target = append(*option.target, value)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return result, err
		}
	}
	if f.View == "options" {
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(base, "DISTINCT key_id,owner")+" ORDER BY 1,2", args...)
		if err != nil {
			return result, err
		}
		defer rows.Close()
		for rows.Next() {
			var owner PortalMonitorOwner
			if err := rows.Scan(&owner.ID, &owner.Name); err != nil {
				return result, err
			}
			result.Options.Users = append(result.Options.Users, owner)
		}
		return result, rows.Err()
	}
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(base, portalMonitorAggregate), args...).Scan(result.Stats.scanTargets()...); err != nil {
		return result, err
	}
	if result.Stats.RequestCount > 0 {
		p95Args := append(append([]any{}, args...), (result.Stats.RequestCount*95+99)/100-1)
		if err := tx.QueryRowContext(ctx, fmt.Sprintf(base, "latency_ms")+" ORDER BY latency_ms LIMIT 1 OFFSET ?", p95Args...).Scan(&result.P95LatencyMs); err != nil {
			return result, err
		}
	}
	step := int64(86400)
	if f.Bucket == "hour" {
		step = 3600
	}
	// Expression uses a validated constant, never caller-provided SQL.
	loc, _ := PortalReportingLocation(f.Timezone) // validated above
	_, offset := f.Since.In(loc).Zone()
	shift := int64(offset)
	bucketExpr := fmt.Sprintf("((created_at + %d) / %d) * %d - %d", shift, step, step, shift)
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(base, bucketExpr+","+portalMonitorAggregate)+" GROUP BY 1 ORDER BY 1", args...)
	if err != nil {
		return result, err
	}
	byStart := map[int64]PortalMonitorStats{}
	for rows.Next() {
		var start int64
		var stats PortalMonitorStats
		if err := rows.Scan(append([]any{&start}, stats.scanTargets()...)...); err != nil {
			rows.Close()
			return result, err
		}
		byStart[start] = stats
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	for start := (f.Since.Unix()+shift)/step*step - shift; start < f.Until.Unix(); start += step {
		result.Buckets = append(result.Buckets, PortalMonitorBucket{Start: time.Unix(start, 0).UTC(), Stats: byStart[start]})
	}
	for _, group := range []struct {
		columns string
		target  *[]PortalMonitorGroup
	}{{"model_requested,model_requested", &result.Models}, {"key_id,owner", &result.Users}} {
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(base, group.columns+","+portalMonitorAggregate)+" GROUP BY 1,2 ORDER BY COUNT(*) DESC,1,2", args...)
		if err != nil {
			return result, err
		}
		for rows.Next() {
			var g PortalMonitorGroup
			if err := rows.Scan(append([]any{&g.ID, &g.Name}, g.Stats.scanTargets()...)...); err != nil {
				rows.Close()
				return result, err
			}
			*group.target = append(*group.target, g)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return result, err
		}
	}
	result.RequestCount = result.Stats.RequestCount
	for _, user := range result.Users {
		result.Options.Users = append(result.Options.Users, PortalMonitorOwner{ID: user.ID, Name: user.Name})
	}
	if f.View == "summary" {
		return result, nil
	}
	pageArgs := append(append([]any{}, args...), f.PageSize, (f.Page-1)*f.PageSize)
	result.Requests, err = portalMonitorRequests(ctx, tx, fmt.Sprintf(base, `id,key_id,owner,created_at,model_requested,model_used,provider,auth_mode,status,prompt_tokens,completion_tokens,total_tokens,cache_read_tokens,cache_creation_tokens,reasoning_tokens,latency_ms,cost_class,CASE WHEN cost_class='priced' THEN cost_usd ELSE NULL END`)+" ORDER BY created_at DESC,id DESC LIMIT ? OFFSET ?", pageArgs)
	return result, err
}

func portalMonitorRequests(ctx context.Context, tx *sql.Tx, query string, args []any) ([]PortalMonitorRequest, error) {
	result := []PortalMonitorRequest{}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var r PortalMonitorRequest
		var at int64
		if err := rows.Scan(&r.ID, &r.KeyID, &r.User, &at, &r.Model, &r.ModelUsed, &r.Provider, &r.AuthMode, &r.Status, &r.PromptTokens, &r.CompletionTokens, &r.TotalTokens, &r.CacheReadTokens, &r.CacheCreationTokens, &r.ReasoningTokens, &r.LatencyMs, &r.CostClass, &r.PricedCostUSD); err != nil {
			rows.Close()
			return result, err
		}
		r.CreatedAt = time.Unix(at, 0).UTC()
		result = append(result, r)
	}
	err = rows.Err()
	rows.Close()
	return result, err
}
