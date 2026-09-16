package agentregistry

import (
	"strings"
	"testing"
)

// TestTailWriter_BoundsRetention verifies the capped capture writer keeps only
// the last max bytes, flags truncation, and never grows its buffer past the cap
// regardless of how much is written (the runaway-RSS guard). This is the shared
// streaming bounded writer used by both the agentsched runner and RunStage.
func TestTailWriter_BoundsRetention(t *testing.T) {
	t.Run("under cap keeps everything verbatim", func(t *testing.T) {
		w := NewTailWriter(100)
		n, err := w.Write([]byte("hello"))
		if err != nil || n != 5 {
			t.Fatalf("Write = (%d, %v), want (5, nil)", n, err)
		}
		if w.truncated {
			t.Error("did not expect truncation under cap")
		}
		if got := w.String(); got != "hello" {
			t.Errorf("String = %q, want %q", got, "hello")
		}
	})

	t.Run("incremental writes over cap keep only the tail", func(t *testing.T) {
		w := NewTailWriter(4)
		for _, chunk := range []string{"ab", "cd", "ef"} {
			if _, err := w.Write([]byte(chunk)); err != nil {
				t.Fatalf("Write(%q): %v", chunk, err)
			}
		}
		if len(w.buf) != 4 {
			t.Errorf("retained %d bytes, want 4 (cap)", len(w.buf))
		}
		if !w.truncated {
			t.Error("expected truncated flag after exceeding cap")
		}
		if !strings.HasSuffix(w.String(), "cdef") {
			t.Errorf("String tail = %q, want suffix %q", w.String(), "cdef")
		}
		if !strings.HasPrefix(w.String(), "[output truncated") {
			t.Errorf("String missing truncation marker: %q", w.String())
		}
	})

	t.Run("single oversized write keeps only its tail and stays bounded", func(t *testing.T) {
		w := NewTailWriter(8)
		big := strings.Repeat("x", 1<<20) + "TAILTAIL" // 1 MiB + marker
		n, err := w.Write([]byte(big))
		if err != nil || n != len(big) {
			t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(big))
		}
		if len(w.buf) != 8 {
			t.Fatalf("retained %d bytes after 1MiB write, want 8 (RSS guard failed)", len(w.buf))
		}
		if string(w.buf) != "TAILTAIL" {
			t.Errorf("retained tail = %q, want %q", string(w.buf), "TAILTAIL")
		}
	})
}
