package api

import (
	"embed"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

//go:embed portal.html portal_admin.html portal_shared.css portal_shared.js
var portalAssets embed.FS

// portalFragments maps a page placeholder to the fragment file spliced in its
// place. Both pages share one stylesheet and one script this way while each
// response stays a single nonce'd document: no static route, no CSP change.
var portalFragments = map[string]string{"{{SHARED_CSS}}": "portal_shared.css", "{{SHARED_JS}}": "portal_shared.js"}

// portalPage composes a page from a source (the embed or an html_dir). A
// fragment is read only when the page carries its placeholder, so a fully
// inlined custom page needs no companion files.
func portalPage(read func(string) ([]byte, error), name string) (string, error) {
	raw, err := read(name)
	if err != nil {
		return "", err
	}
	page := string(raw)
	for placeholder, file := range portalFragments {
		if !strings.Contains(page, placeholder) {
			continue
		}
		fragment, err := read(file)
		if err != nil {
			return "", err
		}
		page = strings.ReplaceAll(page, placeholder, string(fragment))
	}
	return page, nil
}

// embeddedPortalPages composes each embedded page once and splits it on the
// nonce placeholder, so a request costs one nonce and one write per segment
// instead of copying ~60 KB three times.
var embeddedPortalPages = func() map[string][]string {
	pages := map[string][]string{}
	for _, name := range []string{"portal.html", "portal_admin.html"} {
		page, err := portalPage(portalAssets.ReadFile, name)
		if err != nil {
			panic("agentmodel/api: embedded portal page " + name + ": " + err.Error())
		}
		pages[name] = strings.Split(page, "{{NONCE}}")
	}
	return pages
}()

// Called after portal authentication with a fixed basename, never a request path.
// An html_dir page is read per request; a failed read must not hide a broken
// deploy by falling back to an older embedded page.
func (s *Server) servePortalHTML(w http.ResponseWriter, name string) {
	segments := embeddedPortalPages[name]
	if s.portal.HTMLDir != "" {
		page, err := portalPage(func(file string) ([]byte, error) { return os.ReadFile(filepath.Join(s.portal.HTMLDir, file)) }, name)
		if err != nil {
			portalError(w, http.StatusServiceUnavailable, "portal_html_unavailable")
			return
		}
		segments = strings.Split(page, "{{NONCE}}")
	}
	nonce, err := randomToken()
	if err != nil {
		portalError(w, http.StatusServiceUnavailable, "random_unavailable")
		return
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+nonce+"'; style-src 'nonce-"+nonce+"'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	for i, segment := range segments {
		if i > 0 {
			io.WriteString(w, nonce)
		}
		io.WriteString(w, segment)
	}
}
