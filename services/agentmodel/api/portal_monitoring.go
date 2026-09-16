package api

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

// Only the current configured master is known; never hash an empty credential.
func (s *Server) portalReportScope() store.PortalReportScope {
	if s.bearerToken == "" {
		return store.PortalReportScope{}
	}
	return store.NewPortalReportScope(hashAPIKey(s.bearerToken))
}

func portalMonitorFilter(r *http.Request, now time.Time) (store.PortalMonitorFilter, error) {
	now = now.UTC().Truncate(time.Second)
	f := store.PortalMonitorFilter{Since: now.Add(-store.PortalWindow), Until: now, Bucket: "day", Page: 1, PageSize: 25}
	// Reject typos and repeated parameters rather than quietly displaying a
	// broader dataset than the operator requested.
	allowed := map[string]bool{"timezone": true, "view": true, "since": true, "until": true, "bucket": true, "key_id": true, "user": true, "model": true, "provider": true, "auth_mode": true, "status": true, "page": true, "page_size": true}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return f, err
	}
	for name, values := range q {
		if !allowed[name] || len(values) != 1 || values[0] == "" {
			return f, errors.New("invalid query")
		}
	}
	for _, field := range []struct {
		name   string
		target *time.Time
	}{{"since", &f.Since}, {"until", &f.Until}} {
		if value := q.Get(field.name); value != "" {
			at, err := time.Parse(time.RFC3339, value)
			if err != nil || at.Nanosecond() != 0 || at.After(now) {
				return f, errors.New("invalid date")
			}
			*field.target = at.UTC()
		}
	}
	if q.Get("since") == "" {
		f.Since = f.Until.Add(-store.PortalWindow)
	}
	for _, field := range []struct {
		name   string
		target *int
	}{{"page", &f.Page}, {"page_size", &f.PageSize}} {
		if value := q.Get(field.name); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil {
				return f, err
			}
			*field.target = n
		}
	}
	if value := q.Get("bucket"); value != "" {
		f.Bucket = value
	}
	f.KeyID, f.User, f.Model, f.Provider, f.AuthMode, f.Status = q.Get("key_id"), q.Get("user"), q.Get("model"), q.Get("provider"), q.Get("auth_mode"), q.Get("status")
	f.View = q.Get("view")
	f.Timezone = q.Get("timezone")
	return f, f.Validate()
}

// Both routes sit under /portal/admin, so the gate has verified an administrator
// and, for the write, the Origin/JSON/CSRF triple.
func (s *Server) servePortalMonitoring(w http.ResponseWriter, r *http.Request) {
	f, err := portalMonitorFilter(r, time.Now())
	if err != nil {
		portalError(w, 400, "invalid_monitoring_filter")
		return
	}
	report, err := s.portalStore.GetPortalMonitoring(r.Context(), f, s.portalReportScope())
	if portalStoreError(w, err) {
		return
	}
	// Keep the legacy full shape while projections omit unavailable metrics rather
	// than presenting zero/null placeholders as computed results.
	switch f.View {
	case "requests":
		writeJSON(w, 200, map[string]any{"filter": report.Filter, "scope": report.Scope, "request_count": report.RequestCount, "requests": report.Requests})
	case "options":
		writeJSON(w, 200, map[string]any{"filter": report.Filter, "scope": report.Scope, "options": report.Options})
	case "summary":
		writeJSON(w, 200, map[string]any{"filter": report.Filter, "scope": report.Scope, "stats": report.Stats, "p95_latency_ms": report.P95LatencyMs, "buckets": report.Buckets, "models": report.Models, "users": report.Users, "options": report.Options})
	default:
		writeJSON(w, 200, report)
	}
}

func (s *Server) servePortalState(w http.ResponseWriter, r *http.Request) {
	var body struct {
		KeyID    string `json:"key_id"`
		Disabled *bool  `json:"disabled"`
	}
	if !decodePortalBody(w, r, &body) {
		return
	}
	if body.KeyID == store.PortalMasterKeyID {
		portalError(w, 409, "key_unavailable")
		return
	}
	if body.KeyID == "" || strings.TrimSpace(body.KeyID) != body.KeyID || len(body.KeyID) > 256 || body.Disabled == nil {
		portalError(w, 400, "key_id_and_disabled_required")
		return
	}
	if portalStoreError(w, s.portalStore.SetPortalDisabled(r.Context(), body.KeyID, *body.Disabled)) {
		return
	}
	mk, err := s.store.GetKeyByID(r.Context(), body.KeyID)
	if portalStoreError(w, err) {
		return
	}
	writeJSON(w, 200, map[string]any{"key_id": body.KeyID, "disabled": mk.Disabled, "state": portalKeyState(mk), "revoked_at": mk.RevokedAt})
}
