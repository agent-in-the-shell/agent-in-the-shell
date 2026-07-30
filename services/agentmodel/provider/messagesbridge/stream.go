package messagesbridge

import (
	"encoding/json"
	"fmt"
	"io"
	"iter"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
	"github.com/google/uuid"
)

// streamWriter synthesizes an Anthropic Messages SSE stream from a sequence of
// OpenAI-shaped StreamChunks.
//
// Anthropic streams one message whose content is a sequence of indexed blocks,
// each delimited by content_block_start / content_block_stop. OpenAI deltas
// instead carry interleaved text and tool-call fragments. streamWriter opens a
// fresh block whenever the delta kind (text vs a distinct tool call) changes,
// closing the previous one, and wraps the whole thing in the
// message_start / message_delta / message_stop envelope.
type streamWriter struct {
	w     io.Writer
	model string
	msgID string

	openKind  string // "", "text", or "tool"
	openIndex int    // Anthropic content-block index of the currently open block
	nextIndex int    // next Anthropic content-block index to assign
	curTool   int    // OpenAI tool index of the open tool block (when openKind=="tool")

	stopReason string
	usage      anthropicUsage
}

func newStreamWriter(w io.Writer, model string) *streamWriter {
	return &streamWriter{
		w:          w,
		model:      model,
		msgID:      "msg_" + uuid.NewString(),
		stopReason: "end_turn",
	}
}

// run drives the whole stream. It returns a non-nil error only when writing to
// w fails (the reader closed the pipe); iterator errors are surfaced to the
// client as an Anthropic error event and produce a nil return.
func (s *streamWriter) run(seq iter.Seq2[provider.StreamChunk, error]) error {
	if err := s.writeMessageStart(); err != nil {
		return err
	}
	for chunk, err := range seq {
		if err != nil {
			// Close any open content block first so the error event never
			// follows an unterminated content_block_start.
			if cerr := s.closeOpen(); cerr != nil {
				return cerr
			}
			return s.writeError(err)
		}
		if werr := s.handleChunk(chunk); werr != nil {
			return werr
		}
	}
	return s.finish()
}

func (s *streamWriter) handleChunk(chunk provider.StreamChunk) error {
	if c := chunk.Delta.Content; c != "" {
		if err := s.openText(); err != nil {
			return err
		}
		if err := s.event("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": s.openIndex,
			"delta": map[string]any{"type": "text_delta", "text": c},
		}); err != nil {
			return err
		}
	}
	for _, tc := range chunk.Delta.ToolCalls {
		if err := s.handleToolDelta(tc); err != nil {
			return err
		}
	}
	if chunk.FinishReason != "" {
		s.stopReason = finishToStopReason(chunk.FinishReason)
	}
	if chunk.Usage != nil {
		s.usage = usageToAnthropic(*chunk.Usage)
	}
	return nil
}

func (s *streamWriter) handleToolDelta(tc agentmodel.ToolCall) error {
	oaiIdx := 0
	if tc.Index != nil {
		oaiIdx = *tc.Index
	}
	// A chunk carrying an ID or Name starts a new tool call; otherwise it is an
	// arguments fragment for the currently open tool block.
	if tc.ID != "" || tc.Function.Name != "" {
		if err := s.openTool(oaiIdx, tc.ID, tc.Function.Name); err != nil {
			return err
		}
	} else if s.openKind != "tool" || s.curTool != oaiIdx {
		// An arguments fragment with no matching open tool block means the
		// upstream sent a delta without a start event. A tool_use block needs a
		// non-empty id/name, which this fragment lacks, so drop the orphan
		// rather than emit a protocol-invalid empty block.
		return nil
	}
	if args := tc.Function.Arguments; args != "" {
		return s.event("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": s.openIndex,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
		})
	}
	return nil
}

func (s *streamWriter) openText() error {
	if s.openKind == "text" {
		return nil
	}
	if err := s.closeOpen(); err != nil {
		return err
	}
	s.openKind = "text"
	s.openIndex = s.nextIndex
	s.nextIndex++
	return s.event("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         s.openIndex,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
}

func (s *streamWriter) openTool(oaiIdx int, id, name string) error {
	if s.openKind == "tool" && s.curTool == oaiIdx {
		return nil // already the open block
	}
	if err := s.closeOpen(); err != nil {
		return err
	}
	s.openKind = "tool"
	s.curTool = oaiIdx
	s.openIndex = s.nextIndex
	s.nextIndex++
	return s.event("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": s.openIndex,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    id,
			"name":  name,
			"input": map[string]any{},
		},
	})
}

func (s *streamWriter) closeOpen() error {
	if s.openKind == "" {
		return nil
	}
	idx := s.openIndex
	s.openKind = ""
	return s.event("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": idx,
	})
}

func (s *streamWriter) writeMessageStart() error {
	return s.event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            s.msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         s.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			// The codex backend only reports token usage at the end, so the real
			// counts arrive in the closing message_delta; both the client and the
			// gateway's cost ledger read usage from there.
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

func (s *streamWriter) finish() error {
	// Guarantee at least one content block. A real Anthropic stream always
	// emits one; an empty completion (finish with no content deltas) would
	// otherwise produce message_start -> message_delta with zero blocks, which
	// some clients treat as a truncated/invalid response.
	if s.nextIndex == 0 {
		if err := s.openText(); err != nil {
			return err
		}
	}
	if err := s.closeOpen(); err != nil {
		return err
	}
	if err := s.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": s.stopReason, "stop_sequence": nil},
		"usage": s.usage,
	}); err != nil {
		return err
	}
	return s.event("message_stop", map[string]any{"type": "message_stop"})
}

// writeError emits the terminal Anthropic `error` event. The payload carries
// the classified type and code rather than a blanket api_error, so a caller can
// tell a blown context window (terminal) from a transient upstream 5xx
// (retryable) — mirroring what /v1/chat/completions has done since #705. The
// gateway's own audit row is derived from these same bytes downstream.
func (s *streamWriter) writeError(err error) error {
	// Marshal the typed error rather than rebuilding it field by field: its
	// omitempty tags then decide what appears, so a field added to
	// agentmodel.Error cannot silently go missing from this frame.
	return s.event("error", map[string]any{"type": "error", "error": agentmodel.Wrap(err)})
}

// event writes a single SSE event in the `event: <name>\ndata: <json>\n\n`
// framing the Anthropic SDK and the /v1/messages handler both expect.
func (s *streamWriter) event(name string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, b)
	return err
}
