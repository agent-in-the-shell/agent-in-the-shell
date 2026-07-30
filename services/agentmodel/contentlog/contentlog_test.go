package contentlog

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestOpen_EmptyPathDisabled(t *testing.T) {
	lg, err := Open("")
	if err != nil {
		t.Fatalf("Open(\"\"): %v", err)
	}
	if lg != nil {
		t.Fatalf("Open(\"\") = %v, want nil (disabled)", lg)
	}
	if lg.Enabled() {
		t.Errorf("nil logger Enabled() = true, want false")
	}
	// Nil-receiver methods must be no-ops, not panics.
	if err := lg.Log(Record{Request: json.RawMessage(`{}`)}); err != nil {
		t.Errorf("nil Log: %v", err)
	}
	if err := lg.Close(); err != nil {
		t.Errorf("nil Close: %v", err)
	}
}

func TestLog_WritesJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "content.jsonl")
	lg, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !lg.Enabled() {
		t.Fatalf("Enabled() = false, want true")
	}

	for i := 0; i < 3; i++ {
		if err := lg.Log(Record{
			RequestID: "req",
			Request:   json.RawMessage(`{"model":"m"}`),
			Response:  json.RawMessage(`{"ok":true}`),
		}); err != nil {
			t.Fatalf("Log: %v", err)
		}
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec Record
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("line %d not valid JSON: %v", n, err)
		}
		if rec.Timestamp.IsZero() {
			t.Errorf("line %d: timestamp not stamped", n)
		}
		if string(rec.Request) != `{"model":"m"}` {
			t.Errorf("line %d: request = %s", n, rec.Request)
		}
		n++
	}
	if n != 3 {
		t.Errorf("lines = %d, want 3", n)
	}
}

// TestLog_Append confirms a second Open on the same path appends rather than
// truncates — restarts must not lose prior records.
func TestLog_Append(t *testing.T) {
	path := filepath.Join(t.TempDir(), "content.jsonl")
	for i := 0; i < 2; i++ {
		lg, err := Open(path)
		if err != nil {
			t.Fatalf("Open #%d: %v", i, err)
		}
		if err := lg.Log(Record{Request: json.RawMessage(`{}`), Response: json.RawMessage(`{}`)}); err != nil {
			t.Fatalf("Log: %v", err)
		}
		lg.Close()
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := countLines(b); got != 2 {
		t.Errorf("lines = %d, want 2", got)
	}
}

// TestLog_ConcurrentLinesIntact verifies serialized writes never interleave:
// every line is independently valid JSON under concurrent Log calls.
func TestLog_ConcurrentLinesIntact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "content.jsonl")
	lg, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = lg.Log(Record{
				Request:  json.RawMessage(`{"a":"` + str(200) + `"}`),
				Response: json.RawMessage(`{"b":"` + str(200) + `"}`),
			})
		}()
	}
	wg.Wait()
	lg.Close()

	f, _ := os.Open(path)
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lines := 0
	for sc.Scan() {
		var rec Record
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("interleaved/corrupt line: %v", err)
		}
		lines++
	}
	if lines != n {
		t.Errorf("lines = %d, want %d", lines, n)
	}
}

func countLines(b []byte) int {
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}

func str(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}

// ─── rotation / compression / retention (#1399) ────────────────────────────

// bigRecord returns a Record whose JSON line is at least n bytes, so a size
// policy can be tripped in a predictable number of writes.
func bigRecord(n int) Record {
	return Record{Request: json.RawMessage(`"` + str(n) + `"`), Response: json.RawMessage(`{}`)}
}

// countAllRecords tallies valid JSON-line records across the active file and
// every rotated backup (decompressing .gz), proving rotation never loses or
// corrupts a line.
func countAllRecords(t *testing.T, path string) int {
	t.Helper()
	matches, err := filepath.Glob(path + "*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	total := 0
	for _, m := range matches {
		f, err := os.Open(m)
		if err != nil {
			t.Fatalf("open %s: %v", m, err)
		}
		var r io.Reader = f
		if filepath.Ext(m) == ".gz" {
			gz, err := gzip.NewReader(f)
			if err != nil {
				t.Fatalf("gzip %s: %v", m, err)
			}
			r = gz
		}
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			var rec Record
			if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
				t.Fatalf("corrupt line in %s: %v", m, err)
			}
			total++
		}
		f.Close()
	}
	return total
}

func backups(t *testing.T, path string) []string {
	t.Helper()
	matches, err := filepath.Glob(path + ".*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var out []string
	for _, m := range matches {
		if m != path {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

func TestRotate_BySize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "content.jsonl")
	lg, err := OpenWithOptions(path, Options{MaxSizeBytes: 300})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	clk := time.Unix(0, 0).UTC()
	lg.now = func() time.Time { clk = clk.Add(time.Second); return clk }

	const n = 20
	for i := 0; i < n; i++ {
		if err := lg.Log(bigRecord(200)); err != nil {
			t.Fatalf("Log: %v", err)
		}
	}
	lg.Close()

	if got := len(backups(t, path)); got == 0 {
		t.Fatalf("no rotation happened; want backups")
	}
	fi, _ := os.Stat(path)
	if fi.Size() > 300 {
		t.Errorf("active file = %d bytes, want <= 300 (should have rotated)", fi.Size())
	}
	if got := countAllRecords(t, path); got != n {
		t.Errorf("records across all files = %d, want %d (lost on rotation)", got, n)
	}
}

func TestRotate_ByTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "content.jsonl")
	lg, err := OpenWithOptions(path, Options{RotateEvery: time.Hour})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	now := time.Unix(0, 0).UTC()
	lg.now = func() time.Time { return now }
	lg.rotatedAt = now // Open stamped it with the real clock; re-anchor to the fake one

	if err := lg.Log(bigRecord(10)); err != nil {
		t.Fatalf("Log: %v", err)
	}
	if got := len(backups(t, path)); got != 0 {
		t.Fatalf("rotated before RotateEvery elapsed: %d backups", got)
	}
	now = now.Add(2 * time.Hour) // age past the threshold
	if err := lg.Log(bigRecord(10)); err != nil {
		t.Fatalf("Log: %v", err)
	}
	lg.Close()
	if got := len(backups(t, path)); got != 1 {
		t.Errorf("time-based rotation: backups = %d, want 1", got)
	}
	if got := countAllRecords(t, path); got != 2 {
		t.Errorf("records = %d, want 2", got)
	}
}

func TestRotate_Compress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "content.jsonl")
	lg, err := OpenWithOptions(path, Options{MaxSizeBytes: 250, Compress: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	clk := time.Unix(0, 0).UTC()
	lg.now = func() time.Time { clk = clk.Add(time.Second); return clk }

	for i := 0; i < 10; i++ {
		if err := lg.Log(bigRecord(200)); err != nil {
			t.Fatalf("Log: %v", err)
		}
	}
	lg.Close()

	bs := backups(t, path)
	if len(bs) == 0 {
		t.Fatal("no backups")
	}
	for _, b := range bs {
		if filepath.Ext(b) != ".gz" {
			t.Errorf("backup %s not gzipped", b)
		}
	}
	if got := countAllRecords(t, path); got != 10 {
		t.Errorf("records = %d, want 10", got)
	}
}

func TestRetention_MaxBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "content.jsonl")
	lg, err := OpenWithOptions(path, Options{MaxSizeBytes: 250, MaxBackups: 2})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	clk := time.Unix(0, 0).UTC()
	lg.now = func() time.Time { clk = clk.Add(time.Second); return clk }

	for i := 0; i < 30; i++ {
		if err := lg.Log(bigRecord(200)); err != nil {
			t.Fatalf("Log: %v", err)
		}
	}
	lg.Close()
	if got := len(backups(t, path)); got > 2 {
		t.Errorf("backups = %d, want <= 2 (MaxBackups)", got)
	}
}

func TestRetention_MaxTotalBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "content.jsonl")
	// Each line ~230B. Cap total at ~600B: a couple of backups plus the active
	// file at most.
	lg, err := OpenWithOptions(path, Options{MaxSizeBytes: 250, MaxTotalBytes: 600})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	clk := time.Unix(0, 0).UTC()
	lg.now = func() time.Time { clk = clk.Add(time.Second); return clk }

	for i := 0; i < 30; i++ {
		if err := lg.Log(bigRecord(200)); err != nil {
			t.Fatalf("Log: %v", err)
		}
	}
	if got := lg.DiskUsage(); got > 600+260 { // allow one active file over the cap
		t.Errorf("DiskUsage = %d, want near cap 600 (+ one active file)", got)
	}
	lg.Close()
}

func TestRetention_MaxAge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "content.jsonl")
	lg, err := OpenWithOptions(path, Options{MaxSizeBytes: 250, MaxAge: time.Hour})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	now := time.Unix(0, 0).UTC()
	lg.now = func() time.Time { return now }

	// Two rotations at t=0, then jump 2h so both are past MaxAge, then one more
	// rotation which triggers retention and should sweep the aged backups.
	lg.Log(bigRecord(200))
	lg.Log(bigRecord(200)) // rotates #1
	lg.Log(bigRecord(200)) // rotates #2
	before := len(backups(t, path))
	if before < 2 {
		t.Fatalf("setup: want >=2 backups, got %d", before)
	}
	now = now.Add(2 * time.Hour)
	lg.Log(bigRecord(200))
	lg.Log(bigRecord(200)) // rotates again -> retention deletes the aged ones
	lg.Close()

	// Only the freshly-rotated (t=2h) backups should remain; the t=0 ones aged out.
	if got := len(backups(t, path)); got >= before+2 {
		t.Errorf("aged backups not pruned: %d backups remain", got)
	}
}

func TestRotate_CallbacksAndDiskUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "content.jsonl")
	var rotations int
	var lastDisk int64
	lg, err := OpenWithOptions(path, Options{
		MaxSizeBytes: 250,
		OnRotate:     func(ev RotateEvent) { rotations++; lastDisk = ev.DiskBytes },
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	clk := time.Unix(0, 0).UTC()
	lg.now = func() time.Time { clk = clk.Add(time.Second); return clk }

	for i := 0; i < 10; i++ {
		lg.Log(bigRecord(200))
	}
	lg.Close()
	if rotations == 0 {
		t.Error("OnRotate never called")
	}
	if lastDisk <= 0 {
		t.Errorf("RotateEvent.DiskBytes = %d, want > 0", lastDisk)
	}
}

// TestOpen_NoOptionsUnbounded confirms the zero Options keeps the historic
// single-file behavior: no rotation regardless of size.
func TestOpen_NoOptionsUnbounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "content.jsonl")
	lg, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < 50; i++ {
		lg.Log(bigRecord(200))
	}
	lg.Close()
	if got := len(backups(t, path)); got != 0 {
		t.Errorf("unbounded logger rotated: %d backups", got)
	}
}
