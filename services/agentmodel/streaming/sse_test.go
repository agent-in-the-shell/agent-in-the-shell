package streaming_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/streaming"
)

func TestNewWriter_SetsHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	_, err := streaming.NewWriter(rec)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type: got %q, want text/event-stream", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control: got %q, want no-cache", got)
	}
	if got := rec.Header().Get("Connection"); got != "keep-alive" {
		t.Errorf("Connection: got %q, want keep-alive", got)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("StatusCode: got %d, want 200", rec.Code)
	}
}

func TestNewWriter_RequiresFlusher(t *testing.T) {
	// Plain ResponseWriter without flusher.
	w := nonFlushingWriter{}
	_, err := streaming.NewWriter(&w)
	if !errors.Is(err, streaming.ErrFlushUnsupported) {
		t.Errorf("error: got %v, want ErrFlushUnsupported", err)
	}
}

func TestSend_FormatsAsSSE(t *testing.T) {
	rec := httptest.NewRecorder()
	w, err := streaming.NewWriter(rec)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Send(map[string]any{"hello": "world"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data: {"hello":"world"}`) {
		t.Errorf("body missing data line: %q", body)
	}
	if !strings.HasSuffix(body, "\n\n") {
		t.Errorf("body should end with double-newline: %q", body)
	}
}

func TestSend_MarshalError(t *testing.T) {
	rec := httptest.NewRecorder()
	w, err := streaming.NewWriter(rec)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Send(make(chan int)); err == nil || !strings.Contains(err.Error(), "streaming: marshal") {
		t.Fatalf("Send marshal err = %v, want streaming marshal error", err)
	}
}

func TestSend_WriteError(t *testing.T) {
	wantErr := errors.New("write failed")
	rec := &errorWriter{writeErr: wantErr}
	w, err := streaming.NewWriter(rec)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Send(map[string]string{"ok": "no"}); !errors.Is(err, wantErr) {
		t.Fatalf("Send write err = %v, want %v", err, wantErr)
	}
}

func TestSendEvent_FormatsAsTypedSSE(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := streaming.NewWriter(rec)
	if err := w.SendEvent("custom", map[string]int{"n": 42}); err != nil {
		t.Fatalf("SendEvent: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: custom\n") {
		t.Errorf("missing event line: %q", body)
	}
	if !strings.Contains(body, `data: {"n":42}`) {
		t.Errorf("missing data line: %q", body)
	}
}

func TestSendEvent_MarshalError(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := streaming.NewWriter(rec)
	if err := w.SendEvent("custom", make(chan int)); err == nil || !strings.Contains(err.Error(), "streaming: marshal") {
		t.Fatalf("SendEvent marshal err = %v, want streaming marshal error", err)
	}
}

func TestSendEvent_WriteError(t *testing.T) {
	wantErr := errors.New("event write failed")
	rec := &errorWriter{writeErr: wantErr}
	w, err := streaming.NewWriter(rec)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.SendEvent("custom", map[string]int{"n": 1}); !errors.Is(err, wantErr) {
		t.Fatalf("SendEvent write err = %v, want %v", err, wantErr)
	}
}

func TestDone_WritesTerminator(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := streaming.NewWriter(rec)
	if err := w.Done(); err != nil {
		t.Fatalf("Done: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]\n\n") {
		t.Errorf("body missing [DONE] terminator: %q", rec.Body.String())
	}
}

func TestDone_WriteError(t *testing.T) {
	wantErr := errors.New("done write failed")
	rec := &errorWriter{writeErr: wantErr}
	w, err := streaming.NewWriter(rec)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Done(); !errors.Is(err, wantErr) {
		t.Fatalf("Done err = %v, want %v", err, wantErr)
	}
}

func TestSendError_WrapsInError(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := streaming.NewWriter(rec)
	if err := w.SendError(map[string]string{"type": "rate_limit"}); err != nil {
		t.Fatalf("SendError: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"error":`) {
		t.Errorf("body missing error wrapper: %q", body)
	}
	if !strings.Contains(body, `"type":"rate_limit"`) {
		t.Errorf("body missing inner error: %q", body)
	}
}

func TestSendError_MarshalError(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := streaming.NewWriter(rec)
	if err := w.SendError(make(chan int)); err == nil || !strings.Contains(err.Error(), "streaming: marshal") {
		t.Fatalf("SendError marshal err = %v, want streaming marshal error", err)
	}
}

func TestSendKeepalive_WritesCommentLine(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := streaming.NewWriter(rec)
	if err := w.SendKeepalive(); err != nil {
		t.Fatalf("SendKeepalive: %v", err)
	}
	if !strings.Contains(rec.Body.String(), ": keepalive\n\n") {
		t.Errorf("body missing keepalive comment: %q", rec.Body.String())
	}
}

func TestSendKeepalive_WriteError(t *testing.T) {
	wantErr := errors.New("keepalive write failed")
	rec := &errorWriter{writeErr: wantErr}
	w, err := streaming.NewWriter(rec)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.SendKeepalive(); !errors.Is(err, wantErr) {
		t.Fatalf("SendKeepalive err = %v, want %v", err, wantErr)
	}
}

func TestSequentialFraming(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := streaming.NewWriter(rec)

	chunks := []string{"hello", " ", "world"}
	for _, c := range chunks {
		if err := w.Send(map[string]string{"text": c}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	if err := w.Done(); err != nil {
		t.Fatalf("Done: %v", err)
	}

	body := rec.Body.String()
	// Each chunk should appear as its own data line.
	for _, c := range chunks {
		if !strings.Contains(body, `"text":"`+c+`"`) {
			t.Errorf("body missing chunk %q: %q", c, body)
		}
	}
	// Should be exactly 4 data: lines (3 chunks + DONE).
	if got := strings.Count(body, "data: "); got != 4 {
		t.Errorf("data lines: got %d, want 4", got)
	}
}

// nonFlushingWriter implements http.ResponseWriter but NOT http.Flusher,
// to test the constructor's flush check.
type nonFlushingWriter struct {
	header http.Header
	body   []byte
	code   int
}

func (w *nonFlushingWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (w *nonFlushingWriter) Write(b []byte) (int, error) {
	w.body = append(w.body, b...)
	return len(b), nil
}
func (w *nonFlushingWriter) WriteHeader(code int) { w.code = code }

type errorWriter struct {
	header   http.Header
	writeErr error
	code     int
	flushed  int
}

func (w *errorWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *errorWriter) Write([]byte) (int, error) {
	return 0, w.writeErr
}

func (w *errorWriter) WriteHeader(code int) { w.code = code }

func (w *errorWriter) Flush() { w.flushed++ }
