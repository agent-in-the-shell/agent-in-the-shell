package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
)

// imageGenerations handles POST /v1/images/generations — the OpenAI-shaped
// image-generation surface. It is the single client-facing contract for image
// output regardless of upstream: OpenAI's /images/generations and Gemini's
// inline generateContent image parts both normalize to ImageResponse behind the
// router's provider.ImageGenerator dispatch.
func (s *Server) imageGenerations(w http.ResponseWriter, r *http.Request) {
	var req agentmodel.ImageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, &agentmodel.Error{Type: agentmodel.ErrTypeInvalidRequest, Code: agentmodel.CodeInvalidJSON, Message: "invalid JSON: " + err.Error()})
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, &agentmodel.Error{Type: agentmodel.ErrTypeInvalidRequest, Code: agentmodel.CodeMissingRequiredParam, Param: "model", Message: "model is required"})
		return
	}
	if req.Prompt == "" {
		writeError(w, http.StatusBadRequest, &agentmodel.Error{Type: agentmodel.ErrTypeInvalidRequest, Code: agentmodel.CodeMissingRequiredParam, Param: "prompt", Message: "prompt is required"})
		return
	}

	// Pre-request enforcement. Image generation has no fallback walk,
	// so the entry-model allowlist + budget check alone bounds access.
	if ae := s.enforce(r.Context(), vkFromCtx(r.Context()), req.Model); ae != nil {
		s.logImageFailure(r.Context(), req, ae, 0)
		writeAPIError(w, ae)
		return
	}

	start := time.Now()
	resp, err := s.router.GenerateImage(r.Context(), req)
	latency := time.Since(start)

	if err != nil {
		s.logImageFailure(r.Context(), req, err, latency)
		writeAPIError(w, err)
		return
	}

	// Price from the resolved upstream model id (resp.Model), matching how
	// chat/embeddings attribute cost. Subscription models resolve to $0; models
	// absent from the price registry record cost_source="unpriced".
	src := s.priceUsage(r.Context(), resp.Model, &resp.Usage)
	if resp.Created == 0 {
		resp.Created = time.Now().Unix()
	}
	s.logImageSuccess(r.Context(), req, resp, latency, src)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) logImageSuccess(ctx context.Context, req agentmodel.ImageRequest, resp agentmodel.ImageResponse, latency time.Duration, costSource string) {
	s.writeRequestLog(ctx, buildUsageLog(req.Model, resp.Model, resp.Usage, latency, statusOk, "", costSource))
}

func (s *Server) logImageFailure(ctx context.Context, req agentmodel.ImageRequest, err error, latency time.Duration) {
	ae := agentmodel.Wrap(err)
	s.writeRequestLog(ctx, buildUsageLog(req.Model, req.Model, agentmodel.Usage{}, latency, statusError, ae.Type, ""))
}
