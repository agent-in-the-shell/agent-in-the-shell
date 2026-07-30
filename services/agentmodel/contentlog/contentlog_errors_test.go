package contentlog

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenWithOptionsRejectsInvalidPath(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(parent, []byte("file"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	path := filepath.Join(parent, "content.jsonl")

	lg, err := OpenWithOptions(path, Options{})
	if err == nil {
		t.Fatal("OpenWithOptions succeeded for a path below a regular file")
	}
	if lg != nil {
		t.Errorf("logger = %v, want nil on open failure", lg)
	}
	if !strings.Contains(err.Error(), "contentlog: open") || !strings.Contains(err.Error(), path) {
		t.Errorf("error = %q, want operation and path context", err)
	}
}

func TestLogErrors(t *testing.T) {
	t.Run("invalid raw JSON", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "content.jsonl")
		lg, err := Open(path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = lg.Close() })

		err = lg.Log(Record{Request: json.RawMessage(`{`), Response: json.RawMessage(`{}`)})
		if err == nil {
			t.Fatal("Log succeeded with invalid request JSON")
		}
		if !strings.Contains(err.Error(), "contentlog: marshal") {
			t.Errorf("error = %q, want marshal context", err)
		}
		fi, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatalf("Stat: %v", statErr)
		}
		if fi.Size() != 0 {
			t.Errorf("file size = %d, want 0 after rejected record", fi.Size())
		}
	})

	t.Run("closed output file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "content.jsonl")
		lg, err := Open(path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if err := lg.f.Close(); err != nil {
			t.Fatalf("close underlying file: %v", err)
		}

		err = lg.Log(Record{Request: json.RawMessage(`{}`), Response: json.RawMessage(`{}`)})
		if err == nil {
			t.Fatal("Log succeeded with a closed output file")
		}
		if !errors.Is(err, os.ErrClosed) {
			t.Errorf("error = %v, want os.ErrClosed", err)
		}
		if !strings.Contains(err.Error(), "contentlog: write") {
			t.Errorf("error = %q, want write context", err)
		}
	})
}

func TestRotateFailureStillWritesRecord(t *testing.T) {
	tests := []struct {
		name     string
		sabotage func(*testing.T, *Logger, string)
		wantIDs  []string
		wantErr  string
	}{
		{
			name: "close failure",
			sabotage: func(t *testing.T, lg *Logger, _ string) {
				t.Helper()
				if err := lg.f.Close(); err != nil {
					t.Fatalf("close underlying file: %v", err)
				}
			},
			wantIDs: []string{"first", "second"},
			wantErr: "contentlog: rotate close",
		},
		{
			name: "rename failure",
			sabotage: func(t *testing.T, _ *Logger, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatalf("remove active path: %v", err)
				}
			},
			wantIDs: []string{"second"},
			wantErr: "contentlog: rotate rename",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "content.jsonl")
			var reported []error
			lg, err := OpenWithOptions(path, Options{
				MaxSizeBytes: 1,
				OnError:      func(err error) { reported = append(reported, err) },
			})
			if err != nil {
				t.Fatalf("OpenWithOptions: %v", err)
			}
			if err := lg.Log(Record{RequestID: "first", Request: json.RawMessage(`{}`), Response: json.RawMessage(`{}`)}); err != nil {
				t.Fatalf("first Log: %v", err)
			}

			tt.sabotage(t, lg, path)
			if err := lg.Log(Record{RequestID: "second", Request: json.RawMessage(`{}`), Response: json.RawMessage(`{}`)}); err != nil {
				t.Fatalf("Log after rotation failure: %v", err)
			}
			if err := lg.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			if len(reported) != 1 {
				t.Fatalf("OnError calls = %d, want 1", len(reported))
			}
			if !strings.Contains(reported[0].Error(), tt.wantErr) {
				t.Errorf("OnError error = %q, want %q", reported[0], tt.wantErr)
			}
			if got := recordIDs(t, path); !equalStrings(got, tt.wantIDs) {
				t.Errorf("record IDs = %v, want %v", got, tt.wantIDs)
			}
		})
	}
}

func TestReopenReportsPathError(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "regular-file")
	if err := os.WriteFile(parent, []byte("file"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	lg := &Logger{path: filepath.Join(parent, "content.jsonl"), now: time.Now}

	err := lg.reopen()
	if err == nil {
		t.Fatal("reopen succeeded below a regular file")
	}
	if !strings.Contains(err.Error(), "contentlog: rotate reopen") {
		t.Errorf("error = %q, want reopen context", err)
	}
}

func TestCompressFailurePreservesSourceAndCleansPartialOutput(t *testing.T) {
	t.Run("missing source", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "missing")
		got, err := compress(src)
		if err == nil {
			t.Fatal("compress succeeded for missing source")
		}
		if got != "" {
			t.Errorf("path = %q, want empty path on failure", got)
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("error = %v, want os.ErrNotExist", err)
		}
	})

	t.Run("destination is directory", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "backup")
		want := []byte("original content")
		if err := os.WriteFile(src, want, 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := os.Mkdir(src+".gz", 0o700); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}

		got, err := compress(src)
		if err == nil {
			t.Fatal("compress succeeded with a directory at the destination")
		}
		if got != "" {
			t.Errorf("path = %q, want empty path on failure", got)
		}
		contents, readErr := os.ReadFile(src)
		if readErr != nil {
			t.Fatalf("source was not preserved: %v", readErr)
		}
		if string(contents) != string(want) {
			t.Errorf("source = %q, want %q", contents, want)
		}
	})

	t.Run("source is directory", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "backup")
		if err := os.Mkdir(src, 0o700); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}

		got, err := compress(src)
		if err == nil {
			t.Fatal("compress succeeded for a directory")
		}
		if got != "" {
			t.Errorf("path = %q, want empty path on failure", got)
		}
		if fi, statErr := os.Stat(src); statErr != nil || !fi.IsDir() {
			t.Errorf("source directory was not preserved: info=%v err=%v", fi, statErr)
		}
		if _, statErr := os.Stat(src + ".gz"); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("partial gzip still exists; Stat error = %v", statErr)
		}
	})
}

func TestBackupHelpersHandleCompressedAndNonstandardNames(t *testing.T) {
	t.Run("compressed twin reserves name", func(t *testing.T) {
		name := filepath.Join(t.TempDir(), "content.jsonl.20200101T000000.000000000Z")
		if !free(name) {
			t.Fatal("free returned false for an unused name")
		}
		if err := os.WriteFile(name+".gz", []byte("gzip placeholder"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if free(name) {
			t.Fatal("free returned true when the compressed twin exists")
		}
	})

	t.Run("malformed timestamp falls back to mtime", func(t *testing.T) {
		dir := t.TempDir()
		active := filepath.Join(dir, "content.jsonl")
		backup := active + ".manual-backup"
		if err := os.WriteFile(backup, []byte("backup"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		mtime := time.Date(2020, time.March, 4, 5, 6, 7, 0, time.Local)
		if err := os.Chtimes(backup, mtime, mtime); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
		fi, err := os.Stat(backup)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}

		lg := &Logger{path: active}
		if got := lg.backupTime(backup, fi); !got.Equal(fi.ModTime()) {
			t.Errorf("backupTime = %v, want file mtime %v", got, fi.ModTime())
		}
	})
}

func TestBadGlobErrorsAndNilDiskUsage(t *testing.T) {
	lg := &Logger{path: "[", size: 42}
	if _, err := lg.listBackups(); !errors.Is(err, filepath.ErrBadPattern) {
		t.Errorf("listBackups error = %v, want filepath.ErrBadPattern", err)
	}
	disk, err := lg.enforceRetention()
	if !errors.Is(err, filepath.ErrBadPattern) {
		t.Errorf("enforceRetention error = %v, want filepath.ErrBadPattern", err)
	}
	if disk != 42 {
		t.Errorf("enforceRetention disk = %d, want active size 42", disk)
	}

	var disabled *Logger
	if got := disabled.DiskUsage(); got != 0 {
		t.Errorf("nil Logger DiskUsage = %d, want 0", got)
	}
}

func recordIDs(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var ids []string
	for lineNumber, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d: invalid JSON: %v", lineNumber+1, err)
		}
		ids = append(ids, rec.RequestID)
	}
	return ids
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
