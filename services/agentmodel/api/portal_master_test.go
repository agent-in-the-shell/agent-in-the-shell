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

// Exercise server wiring before extending the store interface: configured master
// traffic must join retained history without admitting arbitrary unowned hashes.
func TestPortalCurrentMasterReporting(t *testing.T) {
	s, st := portalTestServer(t)
	s.bearerToken = "private-current-credential"
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	if err := st.CreateKey(ctx, store.ManagedKey{ID: "owned", Name: "Owned", KeyHash: hashAPIKey("owned")}); err != nil {
		t.Fatal(err)
	}
	for i, token := range []string{"owned", s.bearerToken, s.bearerToken, "unknown", ""} {
		if err := st.LogRequest(ctx, store.RequestLog{ID: string(rune('a' + i)), APIKeyHash: hashAPIKey(token), CreatedAt: now.Add(-time.Duration(i+1) * time.Hour), TotalTokens: 7}); err != nil {
			t.Fatal(err)
		}
	}
	get := func(path, subject string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "https://portal.example.com"+path, nil)
		r.Header.Set("Cf-Access-Jwt-Assertion", subject)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	for _, view := range []string{"full", "summary", "requests", "options"} {
		path := "/portal/admin/api/monitoring?view=" + view + "&key_id=system%3Amaster&timezone=Asia%2FTaipei&page_size=1"
		for _, subject := range []string{"", "other"} {
			w := get(path, subject)
			if w.Code != 401 && w.Code != 403 {
				t.Fatal(w.Code)
			}
		}
		w := get(path, "signed")
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		for _, secret := range []string{s.bearerToken, hashAPIKey(s.bearerToken), "api_key_hash", "current_master_hash"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("credential leaked")
			}
		}
		var report struct {
			store.PortalMonitoring
			Count int64 `json:"request_count"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		switch view {
		case "full", "summary":
			if report.Stats.RequestCount != 2 || len(report.Users) != 1 || report.Users[0].Name != "Master key" {
				t.Fatalf("master history missing: %s", w.Body.String())
			}
		case "requests":
			if report.Count != 2 || len(report.Requests) != 1 || report.Requests[0].ID != "b" {
				t.Fatal(w.Body.String())
			}
		case "options":
			if len(report.Options.Users) != 1 || report.Options.Users[0].Name != "Master key" {
				t.Fatal(w.Body.String())
			}
		}
	}
	w := get("/portal/admin/api/monitoring", "signed")
	var report store.PortalMonitoring
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil || report.Stats.RequestCount != 3 {
		t.Fatal(w.Body.String(), err)
	}
	w = get("/portal/admin/api/overview", "signed")
	var overview store.PortalOverview
	if err := json.Unmarshal(w.Body.Bytes(), &overview); err != nil || overview.Stats.RequestCount != 3 {
		t.Fatal("master overview missing", err)
	}
	found := false
	for _, key := range overview.Keys {
		if key.Kind == "master" {
			found = key.KeyID == store.PortalMasterKeyID && key.Stats.RequestCount == 2 && key.Models == nil
		}
	}
	if !found {
		t.Fatal("read-only master row missing")
	}
	for _, path := range []string{"/portal/admin/api/monitoring?current_master_hash=anything", "/portal/admin/api/monitoring?master_hash=anything"} {
		if w := get(path, "signed"); w.Code != 400 {
			t.Fatal("client supplied scope accepted")
		}
	}
	for _, route := range []struct{ path, body string }{
		{"models", `{"key_id":"system:master","models":[]}`},
		{"state", `{"key_id":"system:master","disabled":true}`},
	} {
		r := httptest.NewRequest("PUT", "https://portal.example.com/portal/admin/api/"+route.path, strings.NewReader(route.body))
		r.Header.Set("Cf-Access-Jwt-Assertion", "signed")
		r.Header.Set("Origin", s.portal.Origin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-CSRF-Token", strings.Repeat("a", 43))
		r.AddCookie(&http.Cookie{Name: portalCSRFCookie, Value: strings.Repeat("a", 43)})
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 409 {
			t.Fatal("master edit accepted", w.Code)
		}
	}
	s.bearerToken = ""
	w = get("/portal/admin/api/monitoring", "signed")
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil || report.Stats.RequestCount != 1 {
		t.Fatal(w.Body.String(), err)
	}
}
