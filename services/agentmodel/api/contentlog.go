package api

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/contentlog"
)

// writeContent appends one raw request/response pair to the content log. Both
// arguments must already be valid JSON (they are embedded verbatim). A no-op
// when content logging is disabled. Errors are logged and swallowed — content
// logging must never fail or slow a request.
func (s *Server) writeContent(ctx context.Context, reqJSON, respJSON json.RawMessage) {
	if !s.contentLog.Enabled() {
		return
	}
	rec := contentlog.Record{
		RequestID: middleware.GetReqID(ctx),
		Request:   reqJSON,
		Response:  respJSON,
	}
	if err := s.contentLog.Log(rec); err != nil {
		s.logger.WarnContext(ctx, "agentmodel: content log write failed", "err", err)
	}
}

// logChatContent marshals an OpenAI-shaped request/response pair and records it.
// A no-op when content logging is disabled, so the marshal cost is only paid
// when the log is on.
func (s *Server) logChatContent(ctx context.Context, req agentmodel.ChatRequest, resp agentmodel.ChatResponse) {
	if !s.contentLog.Enabled() {
		return
	}
	reqJSON, err := json.Marshal(req)
	if err != nil {
		s.logger.WarnContext(ctx, "agentmodel: content log request marshal failed", "err", err)
		return
	}
	respJSON, err := json.Marshal(resp)
	if err != nil {
		s.logger.WarnContext(ctx, "agentmodel: content log response marshal failed", "err", err)
		return
	}
	s.writeContent(ctx, reqJSON, respJSON)
}

// ─── chat streaming reassembly ─────────────────────────────────────────────

// chatContentAcc reconstructs the final assistant message from a sequence of
// OpenAI streaming deltas so the content log records a complete response rather
// than a stream of fragments. Built only when content logging is enabled.
type chatContentAcc struct {
	role      string
	content   strings.Builder
	reasoning strings.Builder
	thinking  []agentmodel.ThinkingBlock
	toolCalls map[int]*agentmodel.ToolCall
	toolOrder []int
	finish    string
}

func newChatContentAcc() *chatContentAcc {
	return &chatContentAcc{toolCalls: map[int]*agentmodel.ToolCall{}}
}

// add merges one streaming delta and its finish_reason (which may be empty).
func (a *chatContentAcc) add(delta agentmodel.Message, finish string) {
	if a.role == "" && delta.Role != "" {
		a.role = delta.Role
	}
	a.content.WriteString(delta.Content)
	a.reasoning.WriteString(delta.ReasoningContent)
	// Per StreamChunk, thinking blocks arrive whole, so append rather than merge.
	a.thinking = append(a.thinking, delta.ThinkingBlocks...)
	for _, tc := range delta.ToolCalls {
		idx := 0
		if tc.Index != nil {
			idx = *tc.Index
		}
		acc, ok := a.toolCalls[idx]
		if !ok {
			acc = &agentmodel.ToolCall{}
			a.toolCalls[idx] = acc
			a.toolOrder = append(a.toolOrder, idx)
		}
		if tc.ID != "" {
			acc.ID = tc.ID
		}
		if tc.Type != "" {
			acc.Type = tc.Type
		}
		if tc.Function.Name != "" {
			acc.Function.Name = tc.Function.Name
		}
		// Per OpenAI, function arguments stream as a fragmented JSON string.
		acc.Function.Arguments += tc.Function.Arguments
	}
	if finish != "" {
		a.finish = finish
	}
}

// response builds the assembled OpenAI ChatResponse for the content log,
// mirroring the non-streaming response shape.
func (a *chatContentAcc) response(id, model string, created int64, usage agentmodel.Usage) agentmodel.ChatResponse {
	role := a.role
	if role == "" {
		role = "assistant"
	}
	msg := agentmodel.Message{
		Role:             role,
		Content:          a.content.String(),
		ReasoningContent: a.reasoning.String(),
		ThinkingBlocks:   a.thinking,
	}
	for _, idx := range a.toolOrder {
		msg.ToolCalls = append(msg.ToolCalls, *a.toolCalls[idx])
	}
	return agentmodel.ChatResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []agentmodel.Choice{{Index: 0, Message: msg, FinishReason: a.finish}},
		Usage:   usage,
	}
}
