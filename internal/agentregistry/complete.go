package agentregistry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// MaxCompletionOutputBytes bounds how much of stdout/stderr a CompletionRecord
// retains, so `agent-shell logs` never persists an unbounded capture.
const MaxCompletionOutputBytes = 64 << 10

// completionRetention is how long a completion record survives before
// opportunistic pruning removes it. There is no daemon here (matching the
// registry's own daemonless design): pruning runs best-effort on the
// RecordCompletion hot path, mirroring Kill's GC of a reaped entry.
const completionRetention = 7 * 24 * time.Hour

// CompletionRecord is the durable record of one finished run, written once by
// the producer at completion so `agent-shell wait`/`agent-shell logs` can find
// it by ID after the running Entry has already been removed.
type CompletionRecord struct {
	ID         string    `json:"id"`
	ParentID   string    `json:"parent_id,omitempty"`
	TaskID     string    `json:"task_id,omitempty"`
	Command    string    `json:"command"`
	Backend    string    `json:"backend,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	// ExitCode/Signal/Status mirror agentshell.Result's cause-of-completion
	// fields: Status is "ok" | "signaled" | "timeout" | "error"; Signal is the
	// signal number when Status=="signaled", 0 otherwise.
	ExitCode int    `json:"exit_code"`
	Signal   int    `json:"signal,omitempty"`
	Status   string `json:"status,omitempty"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
}

// TailBytes keeps only the last max bytes of s, prefixing a truncation marker
// when anything was dropped. Producers already hold the full captured buffer
// by completion time, so this truncates once at persist time rather than
// streaming through a bounded writer (contrast agentsched's tailWriter, which
// bounds a still-running capture).
func TailBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return fmt.Sprintf("[output truncated: showing last %d bytes]\n%s", max, s[len(s)-max:])
}

// TailWriter is a bounded io.Writer that retains only the last max bytes
// written to it, discarding older bytes as new ones arrive — it bounds a
// single capture stream regardless of how much a subprocess emits. Unlike
// TailBytes (which truncates a complete string once), this streams: producers
// tee it live through an io.MultiWriter while keeping the retained tail
// bounded. Not safe for concurrent use; exec.Cmd writes stdout and stderr
// from separate goroutines, but each owns its own TailWriter.
type TailWriter struct {
	max       int
	buf       []byte
	truncated bool
}

// NewTailWriter returns a TailWriter retaining at most max bytes.
func NewTailWriter(max int) *TailWriter { return &TailWriter{max: max} }

func (w *TailWriter) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= w.max {
		if len(p) > w.max {
			w.truncated = true // exactly max bytes drops nothing — no marker
		}
		w.buf = append(w.buf[:0], p[len(p)-w.max:]...)
		return n, nil
	}
	if len(w.buf)+len(p) > w.max {
		drop := len(w.buf) + len(p) - w.max
		w.buf = w.buf[drop:]
		w.truncated = true
	}
	w.buf = append(w.buf, p...)
	return n, nil
}

// String returns the retained tail, prefixed with a truncation marker when
// earlier bytes were dropped so the persisted output is honest about clipping.
func (w *TailWriter) String() string {
	if w.truncated {
		return fmt.Sprintf("[output truncated: showing last %d bytes]\n%s", w.max, string(w.buf))
	}
	return string(w.buf)
}

// RecordCompletion durably persists rec so a later `agent-shell wait`/`agent-shell
// logs` can find it by ID even after the running Entry has been removed.
// Best-effort: a caller must never treat a RecordCompletion failure as
// breaking the run it describes.
func RecordCompletion(rec CompletionRecord) error {
	root, err := CompletedRoot()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("agentregistry: create completed root: %w", err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("agentregistry: marshal completion: %w", err)
	}
	if err := atomicWriteFile(filepath.Join(root, rec.ID+".json"), data, 0o600); err != nil {
		return fmt.Errorf("agentregistry: write completion: %w", err)
	}
	pruneOldCompletions(root, completionRetention)
	return nil
}

// ReadCompletion looks up a persisted completion record by ID. ok is false
// (with a nil error) when no record exists yet — e.g. the run is still active,
// was pruned, or the ID is unknown.
func ReadCompletion(id string) (CompletionRecord, bool, error) {
	root, err := CompletedRoot()
	if err != nil {
		return CompletionRecord{}, false, err
	}
	data, err := os.ReadFile(filepath.Join(root, id+".json"))
	if err != nil {
		if os.IsNotExist(err) {
			return CompletionRecord{}, false, nil
		}
		return CompletionRecord{}, false, fmt.Errorf("agentregistry: read completion %q: %w", id, err)
	}
	var rec CompletionRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return CompletionRecord{}, false, fmt.Errorf("agentregistry: parse completion %q: %w", id, err)
	}
	return rec, true, nil
}

// pruneOldCompletions removes completion files whose mtime is older than
// maxAge. Best-effort and silent: this runs on every RecordCompletion, so a
// failure here must never surface to the caller.
func pruneOldCompletions(root string, maxAge time.Duration) {
	dirents, err := os.ReadDir(root)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, d := range dirents {
		if d.IsDir() {
			continue
		}
		info, err := d.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(root, d.Name()))
	}
}
