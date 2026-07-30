// Package streaming provides a Server-Sent Events writer used by the chat
// completion endpoint to stream chunks back to clients in OpenAI's wire format.
package streaming

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Writer wraps an http.ResponseWriter and emits SSE messages with proper
// flushing. It must be constructed via NewWriter; the constructor returns an
// error if the underlying writer doesn't support flushing (required for SSE).
type Writer struct {
	w   http.ResponseWriter
	flu http.Flusher
}

// ErrFlushUnsupported is returned by NewWriter when the response writer does
// not implement http.Flusher (e.g. some test recorders).
var ErrFlushUnsupported = errors.New("streaming: response writer does not support flushing")

// NewWriter prepares the response for SSE: sets Content-Type, Cache-Control,
// Connection headers, writes status 200, and flushes. The supplied writer must
// implement http.Flusher.
func NewWriter(w http.ResponseWriter) (*Writer, error) {
	flu, ok := w.(http.Flusher)
	if !ok {
		return nil, ErrFlushUnsupported
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable buffering on nginx-style proxies
	w.WriteHeader(http.StatusOK)
	flu.Flush()
	return &Writer{w: w, flu: flu}, nil
}

// Send marshals data to JSON and writes a single SSE message line:
//
//	data: {...json...}\n\n
//
// Then flushes so the client receives it immediately.
func (s *Writer) Send(data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("streaming: marshal: %w", err)
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", b); err != nil {
		return fmt.Errorf("streaming: write: %w", err)
	}
	s.flu.Flush()
	return nil
}

// SendEvent writes a typed SSE event:
//
//	event: <eventType>\n
//	data: {...json...}\n\n
//
// Useful when the receiver discriminates on event type (e.g. final usage chunk).
func (s *Writer) SendEvent(eventType string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("streaming: marshal: %w", err)
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", eventType, b); err != nil {
		return fmt.Errorf("streaming: write: %w", err)
	}
	s.flu.Flush()
	return nil
}

// SendError writes an OpenAI-shaped error block as an SSE message and flushes.
// Used for mid-stream failures where the response is already 200.
func (s *Writer) SendError(err any) error {
	payload := map[string]any{"error": err}
	return s.Send(payload)
}

// Done writes the OpenAI-standard `data: [DONE]\n\n` terminator and flushes.
func (s *Writer) Done() error {
	if _, err := fmt.Fprint(s.w, "data: [DONE]\n\n"); err != nil {
		return fmt.Errorf("streaming: write done: %w", err)
	}
	s.flu.Flush()
	return nil
}

// SendKeepalive writes an SSE comment line (": ping\n\n") to keep the
// connection alive on long-running streams. Comments are ignored by clients
// but prevent intermediaries from closing the socket.
func (s *Writer) SendKeepalive() error {
	if _, err := fmt.Fprint(s.w, ": keepalive\n\n"); err != nil {
		return err
	}
	s.flu.Flush()
	return nil
}
