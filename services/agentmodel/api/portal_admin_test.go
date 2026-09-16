package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

type unavailablePortalKey struct{ *store.SQLiteStore }

func (s unavailablePortalKey) GetKeyByHash(context.Context, string) (store.ManagedKey, error) {
	return store.ManagedKey{}, errors.New("database failed")
}

func TestPortalEmptyModelsNeverMeansAllModels(t *testing.T) {
	s, st := portalTestServer(t)
	s.router = router.New(map[string][]router.Deployment{"chatgpt": {}}, nil)
	if _, err := st.CreatePortalKey(context.Background(), store.PortalIdentity{Issuer: "issuer", Subject: "user", Email: "user@example.com"}, store.ManagedKey{ID: "portal", Name: "User", KeyHash: hashAPIKey("employee")}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/v1/models?available=1", "", 200},
		{"POST", "/v1/chat/completions", `{"model":"chatgpt","messages":[{"role":"user","content":"hello"}]}`, 403},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		r.Header.Set("Authorization", "Bearer employee")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatal(w.Code, w.Body.String())
		}
		if tc.method == "GET" && !strings.Contains(w.Body.String(), `"data":[]`) {
			t.Fatal(w.Body.String())
		}
	}
	s.store = unavailablePortalKey{st}
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer employee")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestPortalAdminModelsAuthorizationAndCSRF(t *testing.T) {
	s, st := portalTestServer(t)
	ctx := context.Background()
	s.router = router.New(map[string][]router.Deployment{"chatgpt": {}}, nil)
	for _, id := range []string{"signed", "other"} {
		if _, err := st.CreatePortalKey(ctx, store.PortalIdentity{Issuer: s.portal.Issuer, Subject: id, Email: id + "@example.com"}, store.ManagedKey{ID: id, Name: id, KeyHash: hashAPIKey(id)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateKey(ctx, store.ManagedKey{ID: "legacy", Name: "Employee", KeyHash: "secret-hash", Metadata: `{"secret":"private"}`}); err != nil {
		t.Fatal(err)
	}
	request := func(method, path, assertion, body string, csrf bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://portal.example.com"+path, strings.NewReader(body))
		r.Header.Set("Cf-Access-Jwt-Assertion", assertion)
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
		assertion, body string
		csrf            bool
		status          int
	}{
		{"other", `{"key_id":"other","models":["chatgpt"]}`, true, 403},
		{"other", `{"key_id":"signed","models":["chatgpt"]}`, true, 403},
		{"signed", `{"key_id":"other","models":[]}`, false, 403},
		{"signed", `{"key_id":"other","models":["unknown"]}`, true, 400},
		{"signed", `{"key_id":"other","models":["chatgpt","chatgpt"]}`, true, 400},
		{"signed", `{"key_id":"other"}`, true, 400},
		{"signed", `{"key_id":"other","models":null}`, true, 400},
		{"signed", `{"models":[]}`, true, 400},
		{"signed", `{"key_id":"other","models":[],"name":"evil"}`, true, 400},
		{"signed", `{"key_id":"other","models":[],"max_budget":10}`, true, 400},
		{"signed", `{"key_id":"other","models":[],"disabled":true}`, true, 400},
		{"signed", `{"key_id":"other","models":[],"key_hash":"evil"}`, true, 400},
		{"signed", `{"key_id":"legacy","models":["chatgpt"]}`, true, 409},
		{"signed", `{"key_id":"missing","models":[]}`, true, 409},
		{"signed", `{"key_id":"other","models":["chatgpt"]}`, true, 200},
	} {
		w := request("PUT", "/portal/admin/api/models", tc.assertion, tc.body, tc.csrf)
		if w.Code != tc.status {
			t.Fatal(tc, w.Code, w.Body.String())
		}
	}
	granted, err := st.GetKeyByID(ctx, "other")
	if err != nil || len(granted.Models) != 1 || granted.Models[0] != "chatgpt" {
		t.Fatal(granted, err)
	}
	untouched, err := st.GetKeyByID(ctx, "signed")
	if err != nil || len(untouched.Models) != 0 {
		t.Fatal(untouched, err)
	}
	w := request("GET", "/portal/api/me", "other", "", false)
	var me struct {
		Key struct {
			Models []string `json:"models"`
		} `json:"key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil || len(me.Key.Models) != 1 || me.Key.Models[0] != "chatgpt" {
		t.Fatal(w.Body.String(), err)
	}
	checkModels := func(want int) {
		t.Helper()
		r := httptest.NewRequest("GET", "/v1/models", nil)
		r.Header.Set("Authorization", "Bearer other")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		var body struct {
			Data []json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 || len(body.Data) != want {
			t.Fatal(w.Code, w.Body.String(), err)
		}
	}
	checkModels(1)
	w = request("PUT", "/portal/admin/api/models", "signed", `{"key_id":"other","models":[]}`, true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	checkModels(0)
	granted, err = st.GetKeyByID(ctx, "other")
	if err != nil || granted.Models == nil || len(granted.Models) != 0 {
		t.Fatal(granted, err)
	}
	for _, path := range []string{"/portal/admin/", "/portal/admin/api/overview", "/portal/admin/api/models"} {
		w := request("GET", path, "signed", "", false)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	w = request("GET", "/portal/admin/api/policy", "signed", "", false)
	if w.Code != 404 {
		t.Fatal("shared policy endpoint remains", w.Code)
	}
	w = request("GET", "/portal/admin/api/overview", "signed", "", false)
	for _, secret := range []string{"secret-hash", "private", "key_hash", "metadata"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("secret leaked", w.Body.String())
		}
	}
	for _, subject := range []string{"signed", "other"} {
		w := request("GET", "/portal/api/me", subject, "", false)
		var me struct {
			Admin bool `json:"is_admin"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
			t.Fatal(err)
		}
		if me.Admin != (subject == "signed") {
			t.Fatal(w.Body.String())
		}
	}
}

func TestPortalAdminRequiresVerifiedAdmin(t *testing.T) {
	s, _ := portalTestServer(t)
	for _, path := range []string{"/portal/admin/", "/portal/admin/api/overview", "/portal/admin/api/models"} {
		r := httptest.NewRequest(http.MethodGet, "https://portal.example.com"+path, nil)
		r.Header.Set("Cf-Access-Jwt-Assertion", "other")
		r.Header.Set("Cf-Access-Authenticated-User-Email", "signed@example.com")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s: status %d, want 403", path, w.Code)
		}
	}
}

func TestPortalRevokeGateAndReadOnlyRevokedPolicy(t *testing.T) {
	s, st := portalTestServer(t)
	ctx := context.Background()
	if _, err := st.CreateServiceKey(ctx, store.ManagedKey{ID: "service", Name: "Service", KeyHash: hashAPIKey("secret")}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateKey(ctx, store.ManagedKey{ID: "legacy", Name: "legacy", KeyHash: "legacy"}); err != nil {
		t.Fatal(err)
	}
	request := func(path, method, assertion, body string, csrf bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://portal.example.com"+path, strings.NewReader(body))
		r.Header.Set("Cf-Access-Jwt-Assertion", assertion)
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
		assertion, body string
		csrf            bool
		status          int
	}{
		{"other", `{"key_id":"service"}`, true, 403},
		{"signed", `{"key_id":"service"}`, false, 403},
		{"signed", `{"key_id":"system:master"}`, true, 409},
		{"signed", `{"key_id":"legacy"}`, true, 409},
		{"signed", `{"key_id":"config"}`, true, 409},
		{"signed", `{"key_id":"service","disabled":false}`, true, 400},
		{"signed", `{"key_id":"service"}`, true, 200},
		{"signed", `{"key_id":"service"}`, true, 200},
	} {
		w := request("/portal/admin/api/revoke", "POST", tc.assertion, tc.body, tc.csrf)
		if w.Code != tc.status {
			t.Fatal(tc, w.Code, w.Body.String())
		}
		if w.Code == 200 && !strings.Contains(w.Body.String(), `"revoked_at"`) {
			t.Fatal(w.Body.String())
		}
	}
	for _, tc := range []struct{ path, body string }{
		{"state", `{"key_id":"service","disabled":false}`},
		{"models", `{"key_id":"service","models":[]}`},
	} {
		w := request("/portal/admin/api/"+tc.path, "PUT", "signed", tc.body, true)
		if w.Code != 409 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w := request("/portal/admin/api/overview", "GET", "signed", "", false)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"revoked"`) || strings.Contains(w.Body.String(), hashAPIKey("secret")) {
		t.Fatal(w.Code, w.Body.String())
	}
}

// Rotate immediately after the stable-ID read to deterministically reproduce
// the old handler's race between key info and its second hash lookup.
type rotateAfterKeyRead struct {
	*store.SQLiteStore
	owner store.PortalIdentity
}

func (s rotateAfterKeyRead) GetKeyByID(ctx context.Context, id string) (store.ManagedKey, error) {
	key, err := s.SQLiteStore.GetKeyByID(ctx, id)
	if err != nil {
		return key, err
	}
	_, err = s.SQLiteStore.RotatePortalKey(ctx, s.owner, 1, "rotated-hash")
	return key, err
}
func TestGetKeyConcurrentPortalRotationKeepsConsistentClassification(t *testing.T) {
	s, st := portalTestServer(t)
	owner := store.PortalIdentity{Issuer: "issuer", Subject: "subject", Email: "employee@example.com"}
	if _, err := st.CreatePortalKey(context.Background(), owner, store.ManagedKey{ID: "key", Name: "Employee", KeyHash: "original-hash"}); err != nil {
		t.Fatal(err)
	}
	s.store = rotateAfterKeyRead{st, owner}
	r := httptest.NewRequest("GET", "/v1/keys/key", nil)
	r.Header.Set("Authorization", "Bearer master")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("rotation caused spurious failure: %d %s", w.Code, w.Body.String())
	}
	var got keyView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Kind != "portal" || got.ID != "key" || got.KeyHash != "original-hash" {
		t.Fatalf("inconsistent ID-read projection: %+v", got)
	}
	current, err := st.GetKeyByHash(context.Background(), "rotated-hash")
	if err != nil || current.ID != got.ID {
		t.Fatal("rotation did not run", current, err)
	}
}
