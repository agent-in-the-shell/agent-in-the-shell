package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/access"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

type portalTestVerifier struct{}

func (portalTestVerifier) Verify(_ context.Context, token string) (access.Identity, error) {
	if token != "signed" && token != "other" {
		return access.Identity{}, errors.New("bad signature")
	}
	return access.Identity{Issuer: "https://team.cloudflareaccess.com", Subject: token, Email: token + "@example.com"}, nil
}

func portalTestServer(t *testing.T) (*Server, *store.SQLiteStore) {
	t.Helper()
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := New(Config{Store: st, BearerToken: "master", Portal: agentmodel.PortalConfig{Enabled: true, Origin: "https://portal.example.com", Issuer: "https://team.cloudflareaccess.com", Audience: "aud", EmailDomain: "example.com", AdminEmails: []string{"signed@example.com"}}})
	s.portalVerifier = portalTestVerifier{}
	return s, st
}

func TestPortalInitialNavigationFetchMetadata(t *testing.T) {
	s, _ := portalTestServer(t)
	h := s.Handler()
	for _, site := range []string{"cross-site", "same-site"} {
		for _, path := range []string{"/portal", "/portal/", "/portal/admin", "/portal/admin/"} {
			t.Run(site+path, func(t *testing.T) {
				for _, tc := range []struct {
					name, assertion, host, origin string
					status                        int
				}{
					{"authenticated", "signed", "portal.example.com", "", http.StatusOK},
					{"missing JWT", "", "portal.example.com", "", http.StatusUnauthorized},
					{"invalid JWT", "invalid", "portal.example.com", "", http.StatusUnauthorized},
					{"wrong host", "signed", "evil.example.com", "", http.StatusForbidden},
					{"wrong origin", "signed", "portal.example.com", "https://evil.example.com", http.StatusForbidden},
				} {
					t.Run(tc.name, func(t *testing.T) {
						r := httptest.NewRequest(http.MethodGet, "https://portal.example.com"+path, nil)
						r.Host = tc.host
						r.Header.Set("Cf-Access-Jwt-Assertion", tc.assertion)
						r.Header.Set("Origin", tc.origin)
						r.Header.Set("Sec-Fetch-Site", site)
						r.Header.Set("Sec-Fetch-Mode", "navigate")
						r.Header.Set("Sec-Fetch-Dest", "document")
						w := httptest.NewRecorder()
						h.ServeHTTP(w, r)
						if w.Code != tc.status {
							t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body.String())
						}
						if tc.status == http.StatusOK && !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
							t.Fatal("navigation did not serve HTML")
						}
					})
				}
			})
		}
	}
}

func TestPortalCrossSiteAPIAndMutationDenied(t *testing.T) {
	s, _ := portalTestServer(t)
	h := s.Handler()
	for _, site := range []string{"cross-site", "same-site"} {
		for _, tc := range []struct{ method, path, mode, dest string }{
			{http.MethodGet, "/portal/api/me", "cors", "empty"},
			{http.MethodGet, "/portal/api/me", "navigate", "document"},
			{http.MethodPost, "/portal/api/key", "navigate", "document"},
			{http.MethodPost, "/portal/api/key/rotate", "cors", "empty"},
			{http.MethodPost, "/portal/", "navigate", "document"},
			{http.MethodGet, "/portal/", "cors", "empty"},
			{http.MethodGet, "/portal/", "navigate", "iframe"},
		} {
			t.Run(site+tc.method+tc.path+tc.mode+tc.dest, func(t *testing.T) {
				r := httptest.NewRequest(tc.method, "https://portal.example.com"+tc.path, strings.NewReader("{}"))
				r.Header.Set("Cf-Access-Jwt-Assertion", "signed")
				r.Header.Set("Sec-Fetch-Site", site)
				r.Header.Set("Sec-Fetch-Mode", tc.mode)
				r.Header.Set("Sec-Fetch-Dest", tc.dest)
				// Even otherwise valid origin/CSRF credentials must not bypass fetch metadata.
				r.Header.Set("Origin", "https://portal.example.com")
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("X-CSRF-Token", strings.Repeat("a", 43))
				r.AddCookie(&http.Cookie{Name: portalCSRFCookie, Value: strings.Repeat("a", 43)})
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "cross_site_request") {
					t.Fatalf("status %d: %s", w.Code, w.Body.String())
				}
			})
		}
	}
}

func TestPortalAuthCreateRotateIsolation(t *testing.T) {
	s, st := portalTestServer(t)
	h := s.Handler()
	request := func(method, path, assertion, origin, csrf, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://portal.example.com"+path, strings.NewReader(body))
		r.Header.Set("Cf-Access-Jwt-Assertion", assertion)
		r.Header.Set("Origin", origin)
		r.Header.Set("X-CSRF-Token", csrf)
		r.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/portal/", "/portal/api/me"} {
		w := request("GET", path, "", "", "", "", nil)
		if w.Code != 401 {
			t.Fatalf("anonymous %s: %d", path, w.Code)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("no-store absent on rejection")
		}
	}
	w := request("GET", "/portal/api/me", "signed", "", "", "", nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var me struct {
		CSRF string `json:"csrf_token"`
	}
	json.Unmarshal(w.Body.Bytes(), &me)
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("unsafe csrf cookie", cookies)
	}
	cookie := cookies[0]
	for _, test := range []struct{ origin, csrf string }{{"", me.CSRF}, {"https://evil.example.com", me.CSRF}, {"https://portal.example.com", ""}} {
		w = request("POST", "/portal/api/key", "signed", test.origin, test.csrf, "{}", cookie)
		if w.Code != 403 {
			t.Fatalf("CSRF accepted %d", w.Code)
		}
	}
	w = request("POST", "/portal/api/key", "signed", "https://portal.example.com", me.CSRF, `{"models":["forbidden"]}`, cookie)
	if w.Code != 400 {
		t.Fatal("client policy override accepted", w.Code)
	}
	w = request("POST", "/portal/api/key", "signed", "https://portal.example.com", me.CSRF, "{}", cookie)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var created struct {
		Key      string `json:"key"`
		Revision int64  `json:"revision"`
		KeyID    string `json:"key_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Key == "" || created.Revision != 1 {
		t.Fatal(w.Body.String())
	}
	mk, err := st.GetKeyByID(context.Background(), created.KeyID)
	if err != nil || mk.Models == nil || len(mk.Models) != 0 {
		t.Fatal(mk, err)
	}
	w = request("GET", "/portal/api/me", "signed", "", "", "", cookie)
	if strings.Contains(w.Body.String(), created.Key) || strings.Contains(w.Body.String(), "key_hash") {
		t.Fatal("plaintext/hash exposed")
	}
	w = request("GET", "/portal/api/me", "other", "", "", "", nil)
	if strings.Contains(w.Body.String(), created.KeyID) {
		t.Fatal("other employee sees key")
	}
	// Portal-issued keys must never expose organization usage, upstream account
	// usage, key management or asynchronous resources belonging to other callers.
	for _, path := range []string{"/v1/usage", "/v1/limits", "/v1/account/openai/usage", "/v1/account/anthropic/usage", "/v1/keys", "/v1/videos/somebody-elses", "/v1/predictions/somebody-elses"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer "+created.Key)
		out := httptest.NewRecorder()
		h.ServeHTTP(out, r)
		if out.Code != 403 {
			t.Fatalf("portal key %s: %d", path, out.Code)
		}
	}
	w = request("POST", "/portal/api/key/rotate", "signed", "https://portal.example.com", me.CSRF, `{"revision":1}`, cookie)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var rotated struct {
		Key string `json:"key"`
	}
	json.Unmarshal(w.Body.Bytes(), &rotated)
	if rotated.Key == "" || rotated.Key == created.Key {
		t.Fatal("no new token")
	}
	if _, err := st.GetKeyByHash(context.Background(), hashAPIKey(created.Key)); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("old key accepted")
	}
	w = request("POST", "/portal/api/key/rotate", "signed", "https://portal.example.com", me.CSRF, `{"revision":1}`, cookie)
	if w.Code != 409 {
		t.Fatal("stale rotation accepted", w.Code)
	}
}

func TestPortalLegacyConfigAndManagedMigration(t *testing.T) {
	for _, managed := range []bool{false, true} {
		t.Run(map[bool]string{false: "config", true: "managed"}[managed], func(t *testing.T) {
			s, st := portalTestServer(t)
			if managed {
				if err := st.CreateKey(context.Background(), store.ManagedKey{ID: "old", Name: "SIGNED@EXAMPLE.COM", KeyHash: "old"}); err != nil {
					t.Fatal(err)
				}
			} else {
				s.keys = []resolvedKey{{name: "signed@example.com", disabled: true}}
			}
			r := httptest.NewRequest("POST", "https://portal.example.com/portal/api/key", strings.NewReader("{}"))
			r.Header.Set("Cf-Access-Jwt-Assertion", "signed")
			r.Header.Set("Origin", "https://portal.example.com")
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-CSRF-Token", strings.Repeat("a", 43))
			r.AddCookie(&http.Cookie{Name: portalCSRFCookie, Value: strings.Repeat("a", 43)})
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != 409 || !strings.Contains(w.Body.String(), "migration_required") {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
}

func TestPortalDisabledAndBearerIndependent(t *testing.T) {
	s := New(Config{BearerToken: "master"})
	r := httptest.NewRequest("GET", "/portal/", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
	s, _ = portalTestServer(t)
	r = httptest.NewRequest("GET", "/v1/keys", nil)
	r.Header.Set("Cf-Access-Jwt-Assertion", "signed")
	r.Header.Set("Cf-Access-Authenticated-User-Email", "admin@example.com")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("JWT authenticated bearer route", w.Code)
	}
	r = httptest.NewRequest("GET", "/v1/keys", nil)
	r.Header.Set("Authorization", "Bearer master")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("bearer now needs access JWT", w.Code)
	}
}
