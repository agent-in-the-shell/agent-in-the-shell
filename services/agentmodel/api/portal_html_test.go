package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func setPortalHTMLDir(t *testing.T, s *Server, dir string) {
	t.Helper()
	data, err := yaml.Marshal(map[string]string{"html_dir": dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(data, &s.portal); err != nil {
		t.Fatal(err)
	}
}

func portalHTMLRequest(h http.Handler, path, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "https://portal.example.com"+path, nil)
	r.Header.Set("Cf-Access-Jwt-Assertion", token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func assertPortalHTML(t *testing.T, w *httptest.ResponseRecorder, template string) string {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	csp := w.Header().Get("Content-Security-Policy")
	match := regexp.MustCompile(`script-src 'nonce-([A-Za-z0-9_-]{43})'`).FindStringSubmatch(csp)
	if len(match) != 2 || !strings.Contains(csp, "style-src 'nonce-"+match[1]+"'") {
		t.Fatalf("missing script/style nonce: %s", csp)
	}
	if got := w.Body.String(); got != strings.ReplaceAll(template, "{{NONCE}}", match[1]) {
		t.Fatal("page does not match selected template with substituted nonce")
	}
	for key, want := range map[string]string{
		"Content-Type":           "text/html; charset=utf-8",
		"Cache-Control":          "no-store",
		"Pragma":                 "no-cache",
		"Referrer-Policy":        "no-referrer",
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
	} {
		if got := w.Header().Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	return match[1]
}

func TestPortalHTMLSources(t *testing.T) {
	for _, page := range []struct{ path, file string }{
		{"/portal/", "portal.html"},
		{"/portal/admin/", "portal_admin.html"},
	} {
		t.Run(page.file, func(t *testing.T) {
			s, _ := portalTestServer(t)
			h := s.Handler()
			embedded, err := portalPage(portalAssets.ReadFile, page.file)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(embedded, "{{SHARED_") || !strings.Contains(embedded, "function api(") {
				t.Fatal("shared fragments were not spliced into the embedded page")
			}
			assertPortalHTML(t, portalHTMLRequest(h, page.path, "signed"), embedded)

			dir := t.TempDir()
			setPortalHTMLDir(t, s, dir)
			filename := filepath.Join(dir, page.file)
			first := `<html><style nonce="{{NONCE}}"></style><script nonce="{{NONCE}}">first</script></html>`
			if err := os.WriteFile(filename, []byte(first), 0644); err != nil {
				t.Fatal(err)
			}
			nonce := assertPortalHTML(t, portalHTMLRequest(h, page.path, "signed"), first)
			// Atomic replacement is visible to the same running handler, without a cache.
			second := strings.ReplaceAll(first, "first", "second")
			if err := os.WriteFile(filename+".new", []byte(second), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filename+".new", filename); err != nil {
				t.Fatal(err)
			}
			if next := assertPortalHTML(t, portalHTMLRequest(h, strings.TrimSuffix(page.path, "/"), "signed"), second); next == nonce {
				t.Fatal("nonce reused across requests")
			}
			// Query parameters cannot select another filename.
			assertPortalHTML(t, portalHTMLRequest(h, page.path+"?file=../secret.html&filename=other.html", "signed"), second)

			if err := os.Remove(filename); err != nil {
				t.Fatal(err)
			}
			for _, unreadable := range []string{"missing", "directory"} {
				if unreadable == "directory" {
					if err := os.Mkdir(filename, 0755); err != nil {
						t.Fatal(err)
					}
				}
				w := portalHTMLRequest(h, page.path, "signed")
				if w.Code != 503 || !strings.Contains(w.Body.String(), "portal_html_unavailable") || strings.Contains(w.Header().Get("Content-Type"), "text/html") {
					t.Fatalf("%s should fail closed before HTML: %d %s", unreadable, w.Code, w.Body.String())
				}
			}
		})
	}
}

func TestPortalHTMLAuthBeforeRead(t *testing.T) {
	s, _ := portalTestServer(t)
	setPortalHTMLDir(t, s, filepath.Join(t.TempDir(), "missing"))
	h := s.Handler()
	for _, tc := range []struct {
		path, token string
		status      int
		code        string
	}{
		{"/portal/", "", 401, "access_assertion_required"},
		{"/portal/admin/", "", 401, "access_assertion_required"},
		{"/portal/admin/", "other", 403, "admin_required"},
		{"/portal/", "other", 503, "portal_html_unavailable"},
		{"/portal/admin/", "signed", 503, "portal_html_unavailable"},
	} {
		w := portalHTMLRequest(h, tc.path, tc.token)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) {
			t.Errorf("%s token %q: %d %.160s", tc.path, tc.token, w.Code, w.Body.String())
		}
	}
	if w := portalHTMLRequest(h, "/portal/api/me", "signed"); w.Code != 200 {
		t.Fatalf("missing HTML must not break API: %d %s", w.Code, w.Body.String())
	}
}

func TestPortalHTMLFixedFilenamesNotStatic(t *testing.T) {
	s, _ := portalTestServer(t)
	dir := t.TempDir()
	setPortalHTMLDir(t, s, dir)
	for _, name := range []string{"portal.html", "portal_admin.html", "secret.html", "index.html"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("private-file-marker"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	h := s.Handler()
	for _, path := range []string{
		"/portal/portal.html", "/portal/portal_admin.html", "/portal/secret.html",
		"/portal/admin/portal_admin.html", "/portal/admin/secret.html",
		"/portal/%2e%2e/secret.html", "/portal/admin/%2e%2e/secret.html",
		"/public/portal.html", "/secret.html",
	} {
		w := portalHTMLRequest(h, path, "signed")
		if w.Code == 200 || strings.Contains(w.Body.String(), "private-file-marker") {
			t.Errorf("unexpected static file access at %s: %d", path, w.Code)
		}
	}
}
