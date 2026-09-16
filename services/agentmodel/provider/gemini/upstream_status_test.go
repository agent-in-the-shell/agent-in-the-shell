package gemini_test

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

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/gemini"
)

// statusServer returns a server that always replies with the given status and
// body, regardless of path. Same name and shape as the openai package's helper,
// which this file's three tests were otherwise open-coding.
func statusServer(status int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
}

// The adapter formats "gemini: <op>: HTTP %d: %s" with the raw body appended, so
// without an explicit status the body's digits decide the classification.
func TestComplete_UpstreamStatusSurvivesCollidingBody(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantTyp string
		wantRet bool
	}{
		{
			name:    "500 whose message quotes a 400ms timeout",
			status:  http.StatusInternalServerError,
			body:    `{"error":{"code":500,"message":"backend timed out after 400ms","status":"INTERNAL"}}`,
			wantTyp: agentmodel.ErrTypeUpstream,
			wantRet: true,
		},
		{
			name:    "503 overloaded",
			status:  http.StatusServiceUnavailable,
			body:    `{"error":{"code":503,"message":"The model is overloaded.","status":"UNAVAILABLE"}}`,
			wantTyp: agentmodel.ErrTypeServiceUnavailable,
			wantRet: true,
		},
		{
			name:    "401 bad key",
			status:  http.StatusUnauthorized,
			body:    `{"error":{"code":401,"message":"API key not valid","status":"UNAUTHENTICATED"}}`,
			wantTyp: agentmodel.ErrTypeAuthentication,
			wantRet: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := statusServer(c.status, c.body)
			defer srv.Close()

			_, err := gemini.NewWithBaseURL(newAuth(), srv.URL).Complete(context.Background(), agentmodel.ChatRequest{
				Model:    "gemini-2.5-pro",
				Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
			})
			if err == nil {
				t.Fatal("expected an error")
			}
			ae := agentmodel.Wrap(err)
			if ae.Type != c.wantTyp {
				t.Errorf("Type = %q, want %q (err=%v)", ae.Type, c.wantTyp, err)
			}
			if ae.Retryable() != c.wantRet {
				t.Errorf("Retryable() = %v, want %v", ae.Retryable(), c.wantRet)
			}
		})
	}
}

// Complete is not gemini's only HTTP path, and this map is the enumeration of
// the others: ListModels feeds the router's model refresh, PollVideo is retried
// by router/videos.go, and DownloadVideo streams result bytes to the client.
// All classify through Wrap and share Complete's error shape, so without a
// status they share its collision too — a 500 whose body quotes "400ms" reads
// as a terminal invalid_request_error and the caller stops retrying a
// transient failure.
//
// Keep every HTTP path in this package listed here. DownloadVideo was missing
// when this test was written, which is exactly how it kept a hardcoded
// upstream_error long enough to need its own issue.
func TestOtherPaths_UpstreamStatusSurvivesCollidingBody(t *testing.T) {
	const body = `{"error":{"code":500,"message":"backend timed out after 400ms","status":"INTERNAL"}}`

	// The srvURL parameter exists for DownloadVideo, which takes an absolute
	// asset URL rather than a path off the client's base.
	call := map[string]func(c *gemini.Client, srvURL string) error{
		"ListModels": func(c *gemini.Client, _ string) error {
			_, err := c.ListModels(context.Background())
			return err
		},
		"PollVideo": func(c *gemini.Client, _ string) error {
			_, err := c.PollVideo(context.Background(), "operations/abc")
			return err
		},
		"DownloadVideo": func(c *gemini.Client, srvURL string) error {
			rc, _, err := c.DownloadVideo(context.Background(), srvURL+"/asset.mp4")
			if err == nil {
				rc.Close()
			}
			return err
		},
	}

	for name, fn := range call {
		t.Run(name, func(t *testing.T) {
			srv := statusServer(http.StatusInternalServerError, body)
			defer srv.Close()

			err := fn(gemini.NewWithBaseURL(newAuth(), srv.URL), srv.URL)
			if err == nil {
				t.Fatal("expected an error")
			}
			ae := agentmodel.Wrap(err)
			if ae.Type != agentmodel.ErrTypeUpstream {
				t.Errorf("Type = %q, want %q (err=%v)", ae.Type, agentmodel.ErrTypeUpstream, err)
			}
			if !ae.Retryable() {
				t.Error("Retryable() = false, want true")
			}
		})
	}
}

// The map above is hand-maintained, which is how DownloadVideo went missing and
//
//	survived two PRs. This is the structural half: Client.send is the one
//
// place the upstream status gets attached, so nothing else may reach the HTTP
// client or judge a status itself.
//
// It works by forbidding the two things a bypass needs: touching the c.http
// field at all (which covers .Do, .Get, .Post, and `cl := c.http` aliasing
// alike), and reading resp.StatusCode. That is deliberately broader than
// "calls c.http.Do" — an earlier version checked only that spelling, and a
// probe confirmed c.http.Get and a one-line alias both walked straight past it.
//
// What it does NOT catch, so nobody mistakes it for a proof: a new file that
// builds its own http.Client or reaches for http.DefaultClient, and anything
// keying off resp.Status (the string) instead of resp.StatusCode. It is a
// syntactic guard over a three-file package, not a type-checked one — the
// type-accurate version needs golang.org/x/tools, and this package's doc
// deliberately keeps it to the standard library.
//
// If you need a legitimate 2xx/3xx distinction outside send (a 202 from a
// long-running submit, say), widen `allowed` and say why — do not route around
// this by renaming.
//
// The openai provider has the twin of this test
// (provider/openai/send_guard_test.go). They are deliberately duplicated rather
// than shared — two callers do not pay for a package — so a widening here
// should be mirrored there.
func TestAllHTTPCallsGoThroughSend(t *testing.T) {
	// Match the repo's other AST invariant tests, which use ParseFile:
	// parser.ParseDir is deprecated, and staticcheck only stays quiet about it
	// because the pin is old.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	fset := token.NewFileSet()

	// send owns the request and the status decision; everything else must go
	// through it.
	allowed := map[string]bool{"send": true}
	// Reading any of these outside send means a path that can skip the status.
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
				t.Errorf("%s: %s — route it through Client.send, which attaches the upstream status",
					fset.Position(sel.Pos()), why)
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("parsed no source files — the guard would pass vacuously")
	}
}

// The test above pins the retryable half for every path. This one pins the
// terminal half for DownloadVideo specifically, which is what  got wrong:
// it hardcoded upstream_error, so an expired asset URL or a bad credential came
// back retryable and told the client to retry a download that cannot succeed.
// Veo asset URLs expire, so the 404 is an ordinary case rather than a corner one.
func TestDownloadVideo_TerminalStatusesStayTerminal(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantTyp string
	}{
		{
			name:    "404 expired or missing asset",
			status:  http.StatusNotFound,
			body:    `{"error":{"code":404,"message":"Requested entity was not found.","status":"NOT_FOUND"}}`,
			wantTyp: agentmodel.ErrTypeNotFound,
		},
		{
			name:    "401 bad credential",
			status:  http.StatusUnauthorized,
			body:    `{"error":{"code":401,"message":"API key not valid","status":"UNAUTHENTICATED"}}`,
			wantTyp: agentmodel.ErrTypeAuthentication,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := statusServer(c.status, c.body)
			defer srv.Close()

			rc, _, err := gemini.NewWithBaseURL(newAuth(), srv.URL).
				DownloadVideo(context.Background(), srv.URL+"/asset.mp4")
			if err == nil {
				rc.Close()
				t.Fatal("expected an error")
			}
			ae := agentmodel.Wrap(err)
			if ae.Type != c.wantTyp {
				t.Errorf("Type = %q, want %q (err=%v)", ae.Type, c.wantTyp, err)
			}
			// Retryable is the axis the bug turned: it gates whether a caller
			// retries at all, and HTTPStatus follows from Type (pinned in wire).
			if ae.Retryable() {
				t.Error("Retryable() = true, want false — a terminal failure must not invite a retry")
			}
		})
	}
}
