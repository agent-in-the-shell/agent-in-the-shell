// Package repoguard holds repo-wide invariants that belong to no single
// service. It deliberately contains no non-test files: nothing imports it, and
// its only job is to fail the build when the tree grows something it shouldn't.
package repoguard_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// executableMagic is the leading byte sequence of every binary format a build
// on a developer machine or CI runner can produce.
var executableMagic = [][]byte{
	{0x7f, 'E', 'L', 'F'},    // ELF (linux)
	{0xcf, 0xfa, 0xed, 0xfe}, // Mach-O 64-bit little-endian (darwin arm64/amd64)
	{0xfe, 0xed, 0xfa, 0xcf}, // Mach-O 64-bit big-endian
	{0xca, 0xfe, 0xba, 0xbe}, // Mach-O universal
	{'M', 'Z'},               // PE (windows)
}

func looksExecutable(head []byte) bool {
	for _, magic := range executableMagic {
		if bytes.HasPrefix(head, magic) {
			return true
		}
	}
	return false
}

// TestNoTrackedExecutables fails if any file under version control is a
// compiled binary.
//
// This exists because .gitignore cannot enforce it. Ignore rules stop an
// accidental `git add`, but they do nothing about a file that is ALREADY
// tracked, and they only cover names somebody thought to list — a future
// cmd/agent-foo would produce an unlisted /agent-foo. An earlier commit shipped a 30MB
// Mach-O this way: `CLAUDE.md` documents `go build -o agent-model
// ./cmd/agent-model`, which drops the binary in the repo root, one `git add -A`
// away from being committed.
//
// Skips outside a git checkout: this invariant concerns version-controlled files.
func TestNoTrackedExecutables(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Skipf("not a git checkout (%v); this guard only applies to a tracked tree", err)
	}

	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}

	var found []string
	for _, name := range strings.Split(string(out), "\x00") {
		if name == "" {
			continue
		}
		path := filepath.Join(root, name)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() < 4 {
			continue
		}
		head, err := firstBytes(path)
		if err != nil {
			continue
		}
		if looksExecutable(head) {
			found = append(found, name)
		}
	}

	if len(found) > 0 {
		t.Errorf("compiled binaries are under version control:\n  %s\n\n"+
			"Build into ./build/ instead (`go build -o build/ ./cmd/...`), and run\n"+
			"`git rm --cached <file>` to untrack. Note that untracking leaves the blob\n"+
			"in history — clone size does not shrink.", strings.Join(found, "\n  "))
	}
}

// TestGuardDetectsARealBinary is the guard's own guard. Without it, a typo in
// the magic table would leave TestNoTrackedExecutables passing forever while
// detecting nothing — a green test that checks the empty set.
func TestGuardDetectsARealBinary(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Skipf("not a git checkout: %v", err)
	}
	bin := filepath.Join(t.TempDir(), "probe")
	build := exec.Command("go", "build", "-o", bin, "./cmd/agent-shell")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("could not build a probe binary: %v: %s", err, out)
	}
	head, err := firstBytes(bin)
	if err != nil {
		t.Fatalf("read probe: %v", err)
	}
	if !looksExecutable(head) {
		t.Fatalf("detector missed a freshly built binary (head=% x) — the magic table is wrong, "+
			"which would make TestNoTrackedExecutables silently vacuous", head)
	}
}

func firstBytes(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	head := make([]byte, 4)
	n, err := f.Read(head)
	if err != nil {
		return nil, err
	}
	return head[:n], nil
}

func repoRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
