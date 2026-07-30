package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
)

// createVideo handles POST /v1/videos/generations — the async video-generation
// surface. Unlike the synchronous image/chat handlers it returns 202 Accepted
// with a gateway operation id; the client polls GET /v1/videos/{id} until the
// operation reaches a terminal status. The router's provider.VideoGenerator
// dispatch normalizes upstreams (Veo today) to one VideoOperation shape.
func (s *Server) createVideo(w http.ResponseWriter, r *http.Request) {
	var req agentmodel.GenerateVideoRequest
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

	// Pre-request enforcement (#47/#52). Video has no fallback walk, so the
	// entry-model allowlist + budget check alone bounds access.
	if ae := s.enforce(r.Context(), vkFromCtx(r.Context()), req.Model); ae != nil {
		s.logVideoFailure(r.Context(), req, ae, 0)
		writeAPIError(w, ae)
		return
	}

	start := time.Now()
	op, err := s.router.GenerateVideo(r.Context(), req)
	latency := time.Since(start)
	if err != nil {
		s.logVideoFailure(r.Context(), req, err, latency)
		writeAPIError(w, err)
		return
	}

	s.logVideoSubmit(r.Context(), req, op, latency)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(op)
}

// getVideo handles GET /v1/videos/{id} — polling an async operation created by
// createVideo. It re-dispatches to the owning provider and returns the current
// status (200 OK), including result URLs once the operation has succeeded.
func (s *Server) getVideo(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, &agentmodel.Error{Type: agentmodel.ErrTypeInvalidRequest, Code: agentmodel.CodeMissingRequiredParam, Param: "id", Message: "operation id is required"})
		return
	}
	op, err := s.router.PollVideo(r.Context(), id)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	// Rewrite each result URL from the raw upstream asset (which needs the
	// gateway's provider credential and would 401 the client) to a gateway
	// content URL the client can fetch with its own bearer (#1493). Only rewrite
	// when the provider actually needs proxying (implements VideoDownloader) —
	// a future provider whose asset URLs are public (e.g. Replicate) is passed
	// through unchanged rather than pointed at a /content route that would fail.
	if s.router.VideoProxyable(id) {
		base := s.gatewayVideoContentURL(r, op.ID)
		for i := range op.Videos {
			if op.Videos[i].URL != "" {
				op.Videos[i].URL = fmt.Sprintf("%s?index=%d", base, i)
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(op)
}

// gatewayVideoContentURL builds the /v1/videos/{id}/content base URL (add
// ?index=N) that proxies a result video through the gateway. The op id is
// base64url (path-safe). Reuses gatewayBaseURL for proxy-aware scheme/host.
func (s *Server) gatewayVideoContentURL(r *http.Request, id string) string {
	return s.gatewayBaseURL(r) + "/v1/videos/" + id + "/content"
}

// downloadVideoContent handles GET /v1/videos/{id}/content — it streams the
// result video's bytes, fetched from the upstream with the gateway's provider
// credential, so a client without that credential can retrieve the asset
// (#1493). Interim limitation: it does not honor Range nor forward
// Content-Length (no seek / progress) — serving via http.ServeContent from
// persisted bytes is the #841 follow-up.
func (s *Server) downloadVideoContent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, &agentmodel.Error{Type: agentmodel.ErrTypeInvalidRequest, Code: agentmodel.CodeMissingRequiredParam, Param: "id", Message: "operation id is required"})
		return
	}
	index := 0
	if v := r.URL.Query().Get("index"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, &agentmodel.Error{Type: agentmodel.ErrTypeInvalidRequest, Param: "index", Message: "index must be a non-negative integer"})
			return
		}
		index = n
	}
	body, contentType, err := s.router.DownloadVideo(r.Context(), id, index)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", contentType)
	_, _ = io.Copy(w, body)
}

// logVideoSubmit records the audit row for an accepted submission. Per-call /
// per-second video cost metering is a follow-up (#841), so the row carries no
// token counts or cost today; the poll (a read) is not logged.
func (s *Server) logVideoSubmit(ctx context.Context, req agentmodel.GenerateVideoRequest, op agentmodel.VideoOperation, latency time.Duration) {
	s.writeRequestLog(ctx, buildUsageLog(req.Model, op.Model, agentmodel.Usage{}, latency, statusOk, "", ""))
}

func (s *Server) logVideoFailure(ctx context.Context, req agentmodel.GenerateVideoRequest, err error, latency time.Duration) {
	ae := agentmodel.Wrap(err)
	s.writeRequestLog(ctx, buildUsageLog(req.Model, req.Model, agentmodel.Usage{}, latency, statusError, ae.Type, ""))
}
