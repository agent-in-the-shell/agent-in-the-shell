package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

func TestPortalMonitoringAdminBoundaryAndValidation(t *testing.T) {
	s, st := portalTestServer(t)
	ctx := context.Background()
	for _, id := range []string{"signed", "other"} {
		if _, err := st.CreatePortalKey(ctx, store.PortalIdentity{Issuer: s.portal.Issuer, Subject: id, Email: id + "@example.com"}, store.ManagedKey{ID: id, KeyHash: hashAPIKey(id)}); err != nil {
			t.Fatal(err)
		}
		if err := st.LogRequest(ctx, store.RequestLog{ID: id, APIKeyHash: hashAPIKey(id), CreatedAt: time.Now().Add(-time.Hour), TotalTokens: 7, ReasoningTokens: nil, ErrorType: "sensitive body"}); err != nil {
			t.Fatal(err)
		}
	}
	request := func(path, assertion string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "https://portal.example.com"+path, nil)
		r.Header.Set("Cf-Access-Jwt-Assertion", assertion)
		r.Header.Set("Cf-Access-Authenticated-User-Email", "signed@example.com")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	for _, subject := range []string{"other", ""} {
		for _, view := range []string{"full", "summary", "requests", "options"} {
			w := request("/portal/admin/api/monitoring?key_id=signed&view="+view, subject)
			want := 403
			if subject == "" {
				want = 401
			}
			if w.Code != want {
				t.Fatal(subject, w.Code, w.Body.String())
			}
		}
	}
	for _, q := range []string{"timezone=", "timezone=UTC&timezone=Asia%2FTaipei", "timezone=America%2FLos_Angeles", "timezone=%zz", "view=bogus", "view=", "view=summary&view=requests", "page=0", "page=-1", "page=1000001", "page=1&page=2", "page_size=101", "page_size=0", "since=bad", "since=2027-01-01T00:00:00Z", "since=2020-01-01T00:00:00Z", "since=2020-01-02T00:00:00Z&until=2020-01-01T00:00:00Z", "bucket=week", "auth_mode=oauth", "status=500", "raw=true", "user=%00", "since=2026-01-01T00:00:00Z&until=2026-03-01T00:00:00Z&bucket=hour"} {
		w := request("/portal/admin/api/monitoring?"+q, "signed")
		if w.Code != 400 {
			t.Fatal(q, w.Code, w.Body.String())
		}
	}
	w := request("/portal/admin/api/monitoring?key_id=other&page_size=1", "signed")
	var report store.PortalMonitoring
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil || w.Code != 200 {
		t.Fatal(w.Code, w.Body.String(), err)
	}
	if report.Stats.RequestCount != 1 || len(report.Requests) != 1 || report.Requests[0].KeyID != "other" || report.Filter.Until.Sub(report.Filter.Since) != 30*24*time.Hour {
		t.Fatal(report)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Header())
	}
	for _, secret := range []string{"api_key_hash", "sensitive body", hashAPIKey("other")} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal(w.Body.String())
		}
	}
	// Personal reporting is still identity-derived and does not honor an admin selector.
	w = request("/portal/api/me?key_id=signed", "other")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var me struct {
		Stats store.PortalStats `json:"stats"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.Stats.RequestCount != 1 {
		t.Fatal(w.Body.String())
	}
}

func TestPortalAdminDisableCSRFAndImmediateAuthentication(t *testing.T) {
	s, st := portalTestServer(t)
	ctx := context.Background()
	if _, err := st.CreatePortalKey(ctx, store.PortalIdentity{Issuer: s.portal.Issuer, Subject: "other", Email: "other@example.com"}, store.ManagedKey{ID: "other", KeyHash: hashAPIKey("employee")}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetPortalModels(ctx, "other", []string{"allowed"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateKey(ctx, store.ManagedKey{ID: "legacy", KeyHash: "legacy"}); err != nil {
		t.Fatal(err)
	}
	request := func(subject, body string, csrf bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PUT", "https://portal.example.com/portal/admin/api/state", strings.NewReader(body))
		r.Header.Set("Cf-Access-Jwt-Assertion", subject)
		if csrf {
			r.Header.Set("Origin", s.portal.Origin)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-CSRF-Token", strings.Repeat("a", 43))
			r.AddCookie(&http.Cookie{Name: portalCSRFCookie, Value: strings.Repeat("a", 43)})
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	for _, tc := range []struct {
		subject, body string
		csrf          bool
		status        int
	}{
		{"other", `{"key_id":"other","disabled":true}`, true, 403},
		{"signed", `{"key_id":"other","disabled":true}`, false, 403},
		{"signed", `{"key_id":"other"}`, true, 400},
		{"signed", `{"key_id":"other","disabled":null}`, true, 400},
		{"signed", `{"key_id":"other","disabled":true,"models":[]}`, true, 400},
		{"signed", `{"disabled":true}`, true, 400},
		{"signed", `{"key_id":"legacy","disabled":true}`, true, 409},
		{"signed", `{"key_id":"other","disabled":true}`, true, 200},
	} {
		w := request(tc.subject, tc.body, tc.csrf)
		if w.Code != tc.status {
			t.Fatal(tc, w.Code, w.Body.String())
		}
	}
	checkAuth := func(want int) {
		t.Helper()
		r := httptest.NewRequest("GET", "/v1/models", nil)
		r.Header.Set("Authorization", "Bearer employee")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != want {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	checkAuth(401)
	w := request("signed", `{"key_id":"other","disabled":false}`, true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	checkAuth(200)
	key, err := st.GetKeyByID(ctx, "other")
	if err != nil || key.Disabled || len(key.Models) != 1 || key.Models[0] != "allowed" {
		t.Fatal(key, err)
	}
}

func TestPortalMonitoringProjections(t *testing.T) {
	s, _ := portalTestServer(t)
	for _, tc := range []struct {
		view   string
		fields []string
	}{
		{"summary", []string{"filter", "scope", "stats", "p95_latency_ms", "buckets", "models", "users", "options"}},
		{"requests", []string{"filter", "scope", "request_count", "requests"}},
		{"options", []string{"filter", "scope", "options"}},
	} {
		t.Run(tc.view, func(t *testing.T) {
			r := httptest.NewRequest("GET", "https://portal.example.com/portal/admin/api/monitoring?timezone=Asia%2FTaipei&view="+tc.view, nil)
			r.Header.Set("Cf-Access-Jwt-Assertion", "signed")
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != 200 {
				t.Fatalf("projection rejected: %d %s", w.Code, w.Body.String())
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
				t.Fatal(err)
			}
			var filter store.PortalMonitorFilter
			if err := json.Unmarshal(fields["filter"], &filter); err != nil || filter.Timezone != "Asia/Taipei" {
				t.Fatal("projection lost timezone", filter, err)
			}
			if len(fields) != len(tc.fields) {
				t.Fatalf("unexpected projection fields: %s", w.Body.String())
			}
			for _, field := range tc.fields {
				if _, ok := fields[field]; !ok {
					t.Fatalf("missing %s: %s", field, w.Body.String())
				}
			}
		})
	}
}

func TestPortalMonitoringTaipeiQuery(t *testing.T) {
	now := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	r := httptest.NewRequest("GET", "/portal/admin/api/monitoring?timezone=Asia%2FTaipei&since=2024-12-31T23:59:59%2B08:00&until=2025-01-01T00:00:00%2B08:00", nil)
	f, err := portalMonitorFilter(r, now)
	if err != nil {
		t.Fatal("Taipei reporting query rejected:", err)
	}
	if f.Since.Format(time.RFC3339) != "2024-12-31T15:59:59Z" || f.Until.Format(time.RFC3339) != "2024-12-31T16:00:00Z" {
		t.Fatal(f)
	}
}
