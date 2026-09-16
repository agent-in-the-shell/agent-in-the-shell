package openai

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// Client.send is the one place credentials are applied and a non-200 becomes a
// typed error, so nothing else in the package may reach the HTTP client or
// judge a status itself. See send for why that matters beyond this package.
//
// The gemini provider has the twin of this test
// (provider/gemini/upstream_status_test.go). They are deliberately duplicated
// rather than shared — two callers do not pay for a package — but that means a
// widening here should be mirrored there. This version already reflects one:
// an earlier form matched only `c.http.Do`, and a probe showed `c.http.Get`
// and a one-line alias walking straight past it.
//
// It works by forbidding the two things a bypass needs: touching the c.http
// field at all (which covers .Do, .Get, .Post and `cl := c.http` aliasing
// alike) and reading resp.StatusCode.
//
// What it does NOT catch, so nobody mistakes it for a proof: a new file that
// builds its own http.Client or reaches for http.DefaultClient, and anything
// keying off resp.Status (the string). It is a syntactic guard, not a
// type-checked one — the type-accurate version needs golang.org/x/tools, which
// this package does not depend on.
//
// If you need a legitimate non-200 distinction outside send (a 202 from some
// future async endpoint, say), add the function to `allowed` and say why. Do
// not route around this by renaming.
func TestAllHTTPCallsGoThroughSend(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	fset := token.NewFileSet()

	// send owns the request and the status decision; mapHTTPError renders the
	// error and needs the status to do it.
	allowed := map[string]bool{"send": true, "mapHTTPError": true, "rawHTTPError": true}
	forbidden := map[string]string{
		"http":       "reads the c.http client directly",
		"StatusCode": "judges the upstream status itself",
	}

	checked := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("ParseFile %s: %v", name, err)
		}
		checked++
		ast.Inspect(file, func(n ast.Node) bool {
			if fd, ok := n.(*ast.FuncDecl); ok && allowed[fd.Name.Name] {
				return false
			}
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if why, bad := forbidden[sel.Sel.Name]; bad {
				t.Errorf("%s: %s — route it through Client.send, which maps non-200 via mapHTTPError",
					fset.Position(sel.Pos()), why)
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("parsed no source files — the guard would pass vacuously")
	}
}

// The guard above only forbids touching c.http, so a caller may still hand-build
// a request and pass it to send. That is fine — but only because send applies
// credentials. While auth lived in newRequest, that path went out bare, came
// back 401, and classified as a terminal auth failure that stops the router's
// fallback walk.
func TestSendAppliesAuthToHandBuiltRequest(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), "GET", srv.URL+"/models", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := newClient(t, srv, "sk-hand-built").send(context.Background(), req, "probe")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	defer resp.Body.Close()
	if gotAuth != "Bearer sk-hand-built" {
		t.Errorf("Authorization = %q, want the credential — send must apply it, not newRequest", gotAuth)
	}
}
