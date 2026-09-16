package conformance

import (
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/api"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/cache"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/contentlog"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/cost"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/stub"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

// Nothing the gateway writes may be readable by another local user. The content
// log holds raw prompts and completions, the response cache holds completion
// bodies verbatim, and the audit database holds org ids and spend history.
//
// The same bug was fixed three times before this existed —  (content log
// created 0600 but an existing file left alone),  (the enforcement),
// (SQLite at 0644) — each guarded by a test next to the one opener it knew
// about. This asserts the property instead: drive real traffic through an
// assembled gateway with the sinks pointed at one directory, then walk it.
//
// What that does and does not buy, stated precisely, because a guard that reads
// stronger than it is becomes the next :
//
//   - COVERED without being named: sidecars and rotated artifacts of the wired
//     sinks — -wal, -shm, rotated backups, .gz. That is exactly what  was,
//     and what the backup-tightening this test forced turned out to be.
//   - NOT covered: a sink reached through a new config key. There are four
//     path-shaped keys (db, cache.sqlite_path, content_log.path and
//     oauth_token_dir); the first three are wired below, and credential files
//     under the fourth are auth's to guard. A new key lands under root only if
//     someone adds it to the wiring here.
//
// The second pass is what makes this worth having. Creating a file fresh proves
// nothing on its own: under `umask 077` SQLite already produces 0600, so a
// create-only assertion passes with every fix in this repo deleted — measured,
// not assumed. Loosening the tree to 0644 and reopening is the umask-independent
// question, and it is also the one the three original bugs were about: all
// three were the SECOND open, on a file that already existed.
func TestIntegration_NothingOnDiskIsWorldReadable(t *testing.T) {
	// Each sink gets its own subdirectory so the directory assertion means
	// something: those are created by the gateway's own EnsureParent rather than
	// by the test. t.TempDir() is 0755 and MkdirAll never tightens an existing
	// directory, so asserting on a directory the test made would be
	// asserting the test's own mkdir.
	root := t.TempDir()
	dbPath := filepath.Join(root, "audit", "audit.db")
	cachePath := filepath.Join(root, "cache", "cache.db")
	logPath := filepath.Join(root, "content", "content.jsonl")
	// No mkdir for the content log's parent: contentlog creates and tightens it
	// itself now. Letting it do so is the point — it makes this the only
	// place herospath.SecureDir is exercised, and it keeps the directory
	// assertion below asserting the gateway's work rather than the test's own.

	open := func() (*store.SQLiteStore, cache.Cache, *contentlog.Logger, *httptest.Server) {
		t.Helper()
		st, err := store.OpenSQLite(dbPath)
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		respCache, err := cache.New(cache.Config{TTL: time.Hour, SQLitePath: cachePath})
		if err != nil {
			t.Fatalf("open cache: %v", err)
		}
		// Small size cap plus compression so rotated backups and a .gz land on
		// disk too — files created by a path other than the original open.
		cl, err := contentlog.OpenWithOptions(logPath, contentlog.Options{
			MaxSizeBytes: 512,
			Compress:     true,
			MaxBackups:   10,
		})
		if err != nil {
			t.Fatalf("open content log: %v", err)
		}
		registry, err := cost.LoadDefault()
		if err != nil {
			t.Fatalf("load registry: %v", err)
		}
		rt := router.New(map[string][]router.Deployment{
			"gpt-4": {{Provider: &stub.Stub{NameValue: "openai"}, Model: "gpt-4o", Weight: 100}},
		}, nil)
		ts := httptest.NewServer(api.New(api.Config{
			Router: rt, Store: st, Registry: registry,
			BearerToken: integrationToken, ContentLog: cl, Cache: respCache,
		}).Handler())
		return st, respCache, cl, ts
	}

	// Two requests, not one: an empty active file never rotates, so the first
	// fills it past MaxSizeBytes and the second rotates it aside and gzips it.
	// Rotation runs inline under the write lock, so the .gz is on disk by the
	// time the second post returns — nothing to flush before the walk.
	drive := func(ts *httptest.Server, tag string) {
		t.Helper()
		for i := 0; i < 2; i++ {
			resp := postChat(t, ts.URL, integrationToken, agentmodel.ChatRequest{
				Model: "gpt-4",
				Messages: []agentmodel.Message{
					{Role: "user", Content: strings.Repeat("x", 200) + tag + string(rune('0'+i))},
				},
			})
			_ = resp.Body.Close()
		}
	}

	closeAll := func(st *store.SQLiteStore, c cache.Cache, cl *contentlog.Logger, ts *httptest.Server) {
		t.Helper()
		ts.Close()
		if err := cl.Close(); err != nil {
			t.Fatalf("close content log: %v", err)
		}
		if err := c.Close(); err != nil {
			t.Fatalf("close cache: %v", err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
	}

	st, respCache, cl, ts := open()
	drive(ts, "a")
	walkAndAssertModes(t, root, "first open")
	closeAll(st, respCache, cl, ts)

	// Loosen everything, the way an older version of this service, an external
	// rotator, or a restore from an archive would leave it.
	loosened := 0
	if err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		loosened++
		return os.Chmod(p, 0o644)
	}); err != nil {
		t.Fatalf("loosen: %v", err)
	}
	if loosened == 0 {
		t.Fatal("loosened nothing — the second pass would assert nothing")
	}

	st, respCache, cl, ts = open()
	drive(ts, "b")
	walkAndAssertModes(t, root, "after reopening a world-readable tree")
	closeAll(st, respCache, cl, ts)
}

// walkAndAssertModes checks every file is 0600 and every directory has no group
// or other bits, and that the walk actually reached each sink — a walk that
// found nothing would otherwise pass silently.
func walkAndAssertModes(t *testing.T, root, stage string) {
	t.Helper()
	files := 0
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if p == root {
			// The root stands in for the operator-created parent, which
			// docs/agentmodel.md says is theirs, not ours. t.TempDir() hands it
			// back 0755 and nothing in the gateway would tighten it.
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			if got := fi.Mode().Perm() & 0o077; got != 0 {
				t.Errorf("%s: directory %s mode = %v, want no group/other bits", stage, rel, fi.Mode().Perm())
			}
			return nil
		}
		files++
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s: file %s mode = %v, want 0600 — readable by other local users", stage, rel, got)
		}
		switch base := filepath.Base(p); {
		case base == "audit.db":
			seen["audit db"] = true
		case base == "cache.db":
			seen["cache db"] = true
		case base == "content.jsonl":
			seen["content log"] = true
		case strings.HasSuffix(base, ".gz"):
			seen["rotated backup"] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("%s: walk: %v", stage, err)
	}
	for _, want := range []string{"audit db", "cache db", "content log", "rotated backup"} {
		if !seen[want] {
			t.Errorf("%s: walk never reached the %s — it is not covering the sinks (saw %d files)",
				stage, want, files)
		}
	}
}
