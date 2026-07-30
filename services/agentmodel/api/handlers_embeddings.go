package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

func (s *Server) embeddings(w http.ResponseWriter, r *http.Request) {
	var req agentmodel.EmbeddingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, &agentmodel.Error{Type: agentmodel.ErrTypeInvalidRequest, Code: agentmodel.CodeInvalidJSON, Message: "invalid JSON: " + err.Error()})
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, &agentmodel.Error{Type: agentmodel.ErrTypeInvalidRequest, Code: agentmodel.CodeMissingRequiredParam, Param: "model", Message: "model is required"})
		return
	}
	if len(req.Input) == 0 {
		writeError(w, http.StatusBadRequest, &agentmodel.Error{Type: agentmodel.ErrTypeInvalidRequest, Code: agentmodel.CodeEmptyArray, Param: "input", Message: "input must be non-empty"})
		return
	}

	// Pre-request enforcement (#47/#52). Embed has no fallback walk, so the
	// entry-model allowlist check alone bounds access.
	if ae := s.enforce(r.Context(), vkFromCtx(r.Context()), req.Model); ae != nil {
		s.logEmbedFailure(r.Context(), req, ae, 0)
		writeAPIError(w, ae)
		return
	}

	start := time.Now()
	resp, err := s.router.Embed(r.Context(), req)
	latency := time.Since(start)

	if err != nil {
		s.logEmbedFailure(r.Context(), req, err, latency)
		writeAPIError(w, err)
		return
	}

	src := s.priceUsage(r.Context(), resp.Model, &resp.Usage)
	s.logEmbedSuccess(r.Context(), req, resp, latency, src)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) logEmbedSuccess(ctx context.Context, req agentmodel.EmbeddingRequest, resp agentmodel.EmbeddingResponse, latency time.Duration, costSource string) {
	s.writeRequestLog(ctx, store.RequestLog{
		ModelRequested:       req.Model,
		ModelUsed:            resp.Model,
		AuthMode:             resp.Usage.AuthMode,
		PromptTokens:         resp.Usage.PromptTokens,
		TotalTokens:          resp.Usage.TotalTokens,
		CacheReadInputTokens: resp.Usage.CacheReadInputTokens,
		CostUSD:              resp.Usage.CostUSD,
		CostSource:           costSource,
		LatencyMs:            int(latency.Milliseconds()),
		Status:               "ok",
	})
}

func (s *Server) logEmbedFailure(ctx context.Context, req agentmodel.EmbeddingRequest, err error, latency time.Duration) {
	ae := agentmodel.Wrap(err)
	s.writeRequestLog(ctx, store.RequestLog{
		ModelRequested: req.Model,
		ModelUsed:      req.Model,
		Status:         "error",
		ErrorType:      ae.Type,
		LatencyMs:      int(latency.Milliseconds()),
	})
}
