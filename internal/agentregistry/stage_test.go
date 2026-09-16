package agentregistry

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stageEnv points the registry roots at a temp dir so a test's stage records
// never touch the developer's real ~/.local/state registry.
func stageEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(dir, "run"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("HOME", dir)
	t.Setenv("AGENT_RUN_ID", "") // no inherited parent
}

// TestRunStage_RecordsSuccess: a passing stage streams its output, exits 0,
// and leaves a durable ok completion record with the stage name and output.
func TestRunStage_RecordsSuccess(t *testing.T) {
	stageEnv(t)
	var out, errOut bytes.Buffer
	code := RunStage([]string{"scope", "--", "sh", "-c", "echo hello-stage"}, nil, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr=%q)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "hello-stage") {
		t.Fatalf("stdout not streamed through: %q", out.String())
	}

	recs := completionRecords(t)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	r := recs[0]
	if r.Status != "ok" || r.ExitCode != 0 {
		t.Fatalf("record status/exit = %q/%d", r.Status, r.ExitCode)
	}
	if !strings.HasPrefix(r.Command, "scope: ") || !strings.Contains(r.Stdout, "hello-stage") {
		t.Fatalf("record command/stdout = %q / %q", r.Command, r.Stdout)
	}
	if r.Backend != "agent-stage" {
		t.Fatalf("backend = %q", r.Backend)
	}
}

// TestRunStage_PropagatesFailure: a failing stage returns its exit code (so
// `set -e` short-circuits the script) and records status ok with the nonzero
// code — a clean nonzero exit is "ok" mechanism-wise, the code carries failure.
func TestRunStage_PropagatesFailure(t *testing.T) {
	stageEnv(t)
	var out, errOut bytes.Buffer
	code := RunStage([]string{"decide", "--", "sh", "-c", "echo oops >&2; exit 7"}, nil, &out, &errOut)
	if code != 7 {
		t.Fatalf("exit = %d, want 7", code)
	}
	recs := completionRecords(t)
	if len(recs) != 1 || recs[0].ExitCode != 7 {
		t.Fatalf("record = %+v", recs)
	}
	if !strings.Contains(recs[0].Stderr, "oops") {
		t.Fatalf("stderr tail not captured: %q", recs[0].Stderr)
	}
}

// TestRunStage_ParentLinkage: a stage records AGENT_RUN_ID as its ParentID and
// exports its own id as AGENT_RUN_ID to the child (so nested runs link under
// it in the tree).
func TestRunStage_ParentLinkage(t *testing.T) {
	stageEnv(t)
	t.Setenv("AGENT_RUN_ID", "parent-run-1")
	var out, errOut bytes.Buffer
	code := RunStage([]string{"gen", "--", "sh", "-c", "printf %s \"$AGENT_RUN_ID\""}, nil, &out, &errOut)
	if code != 0 {
		t.Fatal(errOut.String())
	}
	recs := completionRecords(t)
	if len(recs) != 1 || recs[0].ParentID != "parent-run-1" {
		t.Fatalf("parent id = %q, want parent-run-1", recs[0].ParentID)
	}
	// The child saw AGENT_RUN_ID overridden to the stage's own (uuid) id, not
	// the inherited parent — proving nested runs link under the stage.
	childSaw := strings.TrimSpace(out.String())
	if childSaw == "" || childSaw == "parent-run-1" {
		t.Fatalf("child AGENT_RUN_ID = %q, want the stage's own id", childSaw)
	}
	if recs[0].ID != childSaw {
		t.Fatalf("stage id %q != what the child saw %q", recs[0].ID, childSaw)
	}
}

// TestParseStageArgs covers the `<name> [flags] -- <cmd>` grammar and its
// rejections.
func TestParseStageArgs(t *testing.T) {
	name, cmd, task, tail, err := parseStageArgs([]string{"build", "--task", "t1", "--", "go", "build"})
	if err != nil || name != "build" || task != "t1" || tail != MaxCompletionOutputBytes {
		t.Fatalf("parse = %q/%v/%d/%v", name, task, tail, err)
	}
	if strings.Join(cmd, " ") != "go build" {
		t.Fatalf("cmd = %v", cmd)
	}
	// A stage command's own flags after -- are never consumed as stage flags.
	_, cmd, _, _, err = parseStageArgs([]string{"s", "--", "tool", "--verbose", "-n", "5"})
	if err != nil || strings.Join(cmd, " ") != "tool --verbose -n 5" {
		t.Fatalf("cmd flags leaked: %v (%v)", cmd, err)
	}
	for _, bad := range [][]string{
		{"noSeparator", "go", "build"}, // missing --
		{"--"},                         // no command
		{"--", "go"},                   // missing name (name would be "--")... actually name before --
	} {
		if _, _, _, _, err := parseStageArgs(bad); err == nil {
			t.Errorf("parseStageArgs(%v) = nil error, want rejection", bad)
		}
	}
}

// completionRecords reads every persisted stage record in the test's registry.
func completionRecords(t *testing.T) []CompletionRecord {
	t.Helper()
	root, err := CompletedRoot()
	if err != nil {
		t.Fatal(err)
	}
	var out []CompletionRecord
	dirents, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	for _, d := range dirents {
		if !strings.HasSuffix(d.Name(), ".json") {
			continue
		}
		rec, ok, err := ReadCompletion(strings.TrimSuffix(d.Name(), ".json"))
		if err != nil || !ok {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// TestRunStage_NotFoundReturns127: a missing command is the shell not-found
// convention, no record written.
func TestRunStage_NotFoundReturns127(t *testing.T) {
	stageEnv(t)
	var out, errOut bytes.Buffer
	code := RunStage([]string{"missing", "--", "definitely-not-a-real-binary-xyz"}, nil, &out, &errOut)
	if code != 127 {
		t.Fatalf("exit = %d, want 127", code)
	}
	if len(completionRecords(t)) != 0 {
		t.Fatal("a start failure must not write a record")
	}
}

// TestRunStage_ClosedDownstreamStillRecords: a stdout writer that errors on
// every write (a broken downstream pipe) must NOT fail the stage or drop the
// record — the tee is best-effort, the record is the source of truth.
func TestRunStage_ClosedDownstreamStillRecords(t *testing.T) {
	stageEnv(t)
	var errOut bytes.Buffer
	code := RunStage([]string{"scope", "--", "sh", "-c", "echo captured-anyway; exit 0"}, nil, brokenWriter{}, &errOut)
	if code != 0 {
		t.Fatalf("broken downstream faked a failure: exit = %d", code)
	}
	recs := completionRecords(t)
	if len(recs) != 1 || recs[0].Status != "ok" {
		t.Fatalf("record = %+v", recs)
	}
	if !strings.Contains(recs[0].Stdout, "captured-anyway") {
		t.Fatalf("tail lost when downstream broke: %q", recs[0].Stdout)
	}
}

// brokenWriter fails every write, simulating a closed downstream pipe.
type brokenWriter struct{}

func (brokenWriter) Write(p []byte) (int, error) { return 0, io.ErrClosedPipe }
