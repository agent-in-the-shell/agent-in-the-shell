//go:build !windows

package agentregistry

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestRunPS_Tree(t *testing.T) {
	useTempRoot(t)
	root := sampleEntry("root")
	if _, err := Register(root); err != nil {
		t.Fatal(err)
	}
	child := sampleEntry("child")
	child.ParentID = "root"
	if _, err := Register(child); err != nil {
		t.Fatal(err)
	}
	grandchild := sampleEntry("grandchild")
	grandchild.ParentID = "child"
	if _, err := Register(grandchild); err != nil {
		t.Fatal(err)
	}
	// Dangling ParentID (no such entry) still shows up, at the top level.
	orphan := sampleEntry("orphan")
	orphan.ParentID = "no-such-parent"
	if _, err := Register(orphan); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if code := RunPS([]string{"--tree"}, &out, &out); code != 0 {
		t.Fatalf("RunPS --tree exit=%d, output=%s", code, out.String())
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	indexOf := func(id string) int {
		for i, l := range lines {
			if strings.Contains(l, id) {
				return i
			}
		}
		t.Fatalf("id %q not found in tree output:\n%s", id, out.String())
		return -1
	}
	rootLine, childLine, grandchildLine, orphanLine := lines[indexOf("root")], lines[indexOf("child")], lines[indexOf("grandchild")], lines[indexOf("orphan")]

	if strings.HasPrefix(rootLine, " ") {
		t.Errorf("root line indented: %q", rootLine)
	}
	if !strings.HasPrefix(childLine, "  ") || strings.HasPrefix(childLine, "    ") {
		t.Errorf("child line indent wrong (want depth 1): %q", childLine)
	}
	if !strings.HasPrefix(grandchildLine, "    ") {
		t.Errorf("grandchild line indent wrong (want depth 2): %q", grandchildLine)
	}
	if strings.HasPrefix(orphanLine, " ") {
		t.Errorf("orphan (dangling ParentID) should render at top level: %q", orphanLine)
	}
	// grandchild must render after its parent (child), which renders after root.
	if !(indexOf("root") < indexOf("child") && indexOf("child") < indexOf("grandchild")) {
		t.Errorf("tree ordering wrong: root=%d child=%d grandchild=%d", indexOf("root"), indexOf("child"), indexOf("grandchild"))
	}
}

func TestRunPS_TreeSelfParentIsRoot(t *testing.T) {
	useTempRoot(t)
	e := sampleEntry("self")
	e.ParentID = "self" // pathological: guard must treat this as a root, not recurse
	if _, err := Register(e); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := RunPS([]string{"--tree"}, &out, &out); code != 0 {
		t.Fatalf("RunPS --tree exit=%d, output=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "self") {
		t.Errorf("self-parented entry missing from output:\n%s", out.String())
	}
}

func TestRunWait_ImmediateCompletion(t *testing.T) {
	useTempRoot(t)
	if err := RecordCompletion(CompletionRecord{ID: "w1", ExitCode: 3, Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := RunWait([]string{"w1"}, &out, &errb)
	if code != 3 {
		t.Fatalf("RunWait exit=%d, want 3 (stdout=%q stderr=%q)", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "w1") || !strings.Contains(out.String(), "exit_code=3") {
		t.Errorf("RunWait stdout = %q", out.String())
	}
}

func TestRunWait_NotRunningNoCompletion(t *testing.T) {
	useTempRoot(t)
	var out, errb bytes.Buffer
	code := RunWait([]string{"nope"}, &out, &errb)
	if code != 1 {
		t.Fatalf("RunWait unknown id exit=%d, want 1 (stderr=%q)", code, errb.String())
	}
}

func TestRunWait_PollsUntilCompletion(t *testing.T) {
	useTempRoot(t)
	// Still registered (running) at first: RunWait must poll rather than
	// immediately declare "not running".
	live := sampleEntry("poll1")
	h, err := Register(live)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = RecordCompletion(CompletionRecord{ID: "poll1", ExitCode: 0, Status: "ok"})
		_ = h.Close()
	}()

	var out, errb bytes.Buffer
	code := RunWait([]string{"--timeout", "2s", "poll1"}, &out, &errb)
	if code != 0 {
		t.Fatalf("RunWait exit=%d, want 0 (stdout=%q stderr=%q)", code, out.String(), errb.String())
	}
}

func TestRunWait_Timeout(t *testing.T) {
	useTempRoot(t)
	live := sampleEntry("w-timeout")
	h, err := Register(live)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })

	var out, errb bytes.Buffer
	code := RunWait([]string{"--timeout", "50ms", "w-timeout"}, &out, &errb)
	if code != 124 {
		t.Fatalf("RunWait exit=%d, want 124 (timeout); stderr=%q", code, errb.String())
	}
}

func TestRunWait_UsageErrors(t *testing.T) {
	useTempRoot(t)
	if code := RunWait(nil, nil, new(bytes.Buffer)); code != 2 {
		t.Errorf("RunWait no-arg exit=%d, want 2", code)
	}
	if code := RunWait([]string{"--nope", "x"}, nil, new(bytes.Buffer)); code != 2 {
		t.Errorf("RunWait bad-flag exit=%d, want 2", code)
	}
}

func TestRunLogs_FinishedRun(t *testing.T) {
	useTempRoot(t)
	if err := RecordCompletion(CompletionRecord{ID: "l1", ExitCode: 0, Status: "ok", Stdout: "out\n", Stderr: "warn\n"}); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := RunLogs([]string{"l1"}, &out, &errb); code != 0 {
		t.Fatalf("RunLogs exit=%d, stderr=%q", code, errb.String())
	}
	if out.String() != "out\n" {
		t.Errorf("RunLogs stdout = %q, want %q", out.String(), "out\n")
	}
	if !strings.Contains(errb.String(), "warn") {
		t.Errorf("RunLogs stderr = %q, want it to include the recorded stderr", errb.String())
	}
}

func TestRunLogs_StillRunning(t *testing.T) {
	useTempRoot(t)
	live := sampleEntry("l2")
	h, err := Register(live)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })

	var out, errb bytes.Buffer
	code := RunLogs([]string{"l2"}, &out, &errb)
	if code != 1 {
		t.Fatalf("RunLogs still-running exit=%d, want 1", code)
	}
	if !strings.Contains(errb.String(), "still running") {
		t.Errorf("RunLogs stderr = %q, want a still-running message", errb.String())
	}
}

func TestRunLogs_UnknownID(t *testing.T) {
	useTempRoot(t)
	var out, errb bytes.Buffer
	if code := RunLogs([]string{"nope"}, &out, &errb); code != 1 {
		t.Errorf("RunLogs unknown id exit=%d, want 1", code)
	}
}

func TestRunLogs_UsageErrors(t *testing.T) {
	useTempRoot(t)
	if code := RunLogs(nil, nil, new(bytes.Buffer)); code != 2 {
		t.Errorf("RunLogs no-arg exit=%d, want 2", code)
	}
	if code := RunLogs([]string{"--nope", "x"}, nil, new(bytes.Buffer)); code != 2 {
		t.Errorf("RunLogs bad-flag exit=%d, want 2", code)
	}
}
