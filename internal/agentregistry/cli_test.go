//go:build !windows

package agentregistry

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunPS_TableAndJSON(t *testing.T) {
	root := useTempRoot(t)
	// One live entry (this process) and one stale (a reaped pid).
	live := sampleEntry("live")
	if _, err := Register(live); err != nil {
		t.Fatal(err)
	}
	stale := sampleEntry("stale")
	stale.PID, stale.PGID = deadPID(t), 0
	if _, err := Register(stale); err != nil {
		t.Fatal(err)
	}
	_ = root

	// Table.
	var out, errb bytes.Buffer
	if code := RunPS(nil, &out, &errb); code != 0 {
		t.Fatalf("RunPS table exit=%d, stderr=%q", code, errb.String())
	}
	table := out.String()
	if !strings.Contains(table, "ID\tPID") && !strings.Contains(table, "ID ") {
		t.Errorf("table missing header:\n%s", table)
	}
	if !strings.Contains(table, "live") || !strings.Contains(table, StatusRunning) ||
		!strings.Contains(table, "stale") || !strings.Contains(table, StatusStale) {
		t.Errorf("table missing rows/statuses:\n%s", table)
	}

	// JSON — must include the `status` field (regression guard for json:"status").
	out.Reset()
	errb.Reset()
	if code := RunPS([]string{"--json"}, &out, &errb); code != 0 {
		t.Fatalf("RunPS --json exit=%d, stderr=%q", code, errb.String())
	}
	var got []Entry
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d", len(got))
	}
	statuses := map[string]string{}
	for _, e := range got {
		statuses[e.ID] = e.Status
	}
	if statuses["live"] != StatusRunning || statuses["stale"] != StatusStale {
		t.Errorf("statuses = %v, want live=running stale=stale", statuses)
	}
	// Assert the raw JSON literally carries "status" (proves the tag).
	if !strings.Contains(out.String(), `"status"`) {
		t.Errorf("--json output missing the status field:\n%s", out.String())
	}
}

func TestRunPS_BadFlagAndEmpty(t *testing.T) {
	useTempRoot(t)
	if code := RunPS([]string{"--nope"}, io.Discard, io.Discard); code != 2 {
		t.Errorf("bad flag exit=%d, want 2", code)
	}
	// Empty/missing root => exit 0, no rows.
	var out bytes.Buffer
	if code := RunPS(nil, &out, io.Discard); code != 0 {
		t.Errorf("empty exit=%d, want 0", code)
	}
}

func TestRunPS_WriteErrors(t *testing.T) {
	useTempRoot(t)
	if _, err := Register(sampleEntry("writeerr")); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	if code := RunPS([]string{"--json"}, errWriter{}, &stderr); code != 1 {
		t.Fatalf("RunPS json writer exit=%d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "encode") {
		t.Fatalf("json stderr = %q, want encode", stderr.String())
	}

	stderr.Reset()
	if code := RunPS(nil, errWriter{}, &stderr); code != 1 {
		t.Fatalf("RunPS table writer exit=%d, want 1", code)
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

func TestRootRuntimeDirAndUptimeEdges(t *testing.T) {
	rt := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", rt)
	got, err := Root()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(rt, dirName, "agents"); got != want {
		t.Fatalf("Root = %q, want %q", got, want)
	}

	now := time.Unix(1_700_000_000, 0)
	if got := uptime(now, time.Time{}); got != "-" {
		t.Fatalf("uptime zero = %q, want -", got)
	}
	if got := uptime(now, now.Add(time.Hour)); got != "0s" {
		t.Fatalf("uptime future = %q, want 0s", got)
	}
}

func TestRunKill_ExitCodes(t *testing.T) {
	root := useTempRoot(t)
	// Stale entry (dead pid) => Kill refuses => exit 1.
	stale := sampleEntry("ck1")
	stale.PID, stale.PGID = deadPID(t), deadPID(t)
	if _, err := Register(stale); err != nil {
		t.Fatal(err)
	}
	if code := RunKill([]string{"ck1"}, io.Discard, io.Discard); code != 1 {
		t.Errorf("RunKill stale exit=%d, want 1", code)
	}
	// Missing id (no positional) => usage error => exit 2.
	if code := RunKill(nil, io.Discard, io.Discard); code != 2 {
		t.Errorf("RunKill no-arg exit=%d, want 2", code)
	}
	// Bad flag => exit 2.
	if code := RunKill([]string{"--nope", "x"}, io.Discard, io.Discard); code != 2 {
		t.Errorf("RunKill bad-flag exit=%d, want 2", code)
	}
	_ = root
}

func TestRunSignal_ExitCodes(t *testing.T) {
	useTempRoot(t)
	// Unknown signal name => usage error => exit 2 (before any lookup).
	if code := RunSignal([]string{"someid", "NOTASIG"}, io.Discard, io.Discard); code != 2 {
		t.Errorf("RunSignal bad-signal exit=%d, want 2", code)
	}
	// Wrong arg count => exit 2.
	if code := RunSignal([]string{"only-one"}, io.Discard, io.Discard); code != 2 {
		t.Errorf("RunSignal one-arg exit=%d, want 2", code)
	}
	// Valid signal, missing agent => refusal => exit 1.
	if code := RunSignal([]string{"missing", "TERM"}, io.Discard, io.Discard); code != 1 {
		t.Errorf("RunSignal missing-agent exit=%d, want 1", code)
	}
}

// TestRunPS_StatusAdvisory documents and asserts that List's Status is a
// point-in-time snapshot: a stale entry reports stale, and once its dir is
// deleted it is simply absent (no error, no panic). A caller that acts on an
// entry must re-reconcile, never trust an earlier List's Status.
func TestRunPS_StatusAdvisory(t *testing.T) {
	root := useTempRoot(t)
	stale := sampleEntry("adv")
	stale.PID = deadPID(t)
	if _, err := Register(stale); err != nil {
		t.Fatal(err)
	}
	entries, err := List()
	if err != nil || len(entries) != 1 || entries[0].Status != StatusStale {
		t.Fatalf("want one stale entry, got %+v err=%v", entries, err)
	}
	if err := os.RemoveAll(root + "/adv"); err != nil {
		t.Fatal(err)
	}
	entries, err = List()
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("deleted entry should be absent, got %+v", entries)
	}
}
