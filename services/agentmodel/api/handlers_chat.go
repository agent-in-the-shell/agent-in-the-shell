package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/cost"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/streaming"
)

func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	var req agentmodel.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, &agentmodel.Error{Type: agentmodel.ErrTypeInvalidRequest, Code: agentmodel.CodeInvalidJSON, Message: "invalid JSON: " + err.Error()})
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, &agentmodel.Error{Type: agentmodel.ErrTypeInvalidRequest, Code: agentmodel.CodeMissingRequiredParam, Param: "model", Message: "model is required"})
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, &agentmodel.Error{Type: agentmodel.ErrTypeInvalidRequest, Code: agentmodel.CodeEmptyArray, Param: "messages", Message: "messages must be non-empty"})
		return
	}

	// Pre-request enforcement (#47/#52): model allowlist, then budget caps —
	// both rejected before any upstream call. Rejections are audit-logged so
	// a throttled key is visible in the ledger.
	vk := vkFromCtx(r.Context())
	if ae := s.enforce(r.Context(), vk, req.Model); ae != nil {
		s.logFailure(r.Context(), req, ae, 0, router.StreamMeta{})
		writeAPIError(w, ae)
		return
	}

	blocked := s.blockedModels(vk)
	if req.Stream {
		s.handleStreamingChat(w, r, req, blocked)
		return
	}
	s.handleNonStreamingChat(w, r, req, blocked)
}

func (s *Server) handleNonStreamingChat(w http.ResponseWriter, r *http.Request, req agentmodel.ChatRequest, blocked []string) {
	// Response cache (#48): a content-addressed lookup keyed on the
	// output-affecting request fields. A hit returns the original response
	// verbatim — ledgered at $0 (cost_source "cache") so it advances no budget —
	// and never reaches an upstream provider. Enforcement (allowlist + budget)
	// already ran in chatCompletions, so a restricted/over-budget key is gated
	// before it can ever be served from cache.
	var cacheKey string
	if s.cache != nil {
		cacheKey = chatCacheKey(req)
		cstart := time.Now()
		if raw, ok := s.cache.Get(r.Context(), cacheKey); ok {
			var cached agentmodel.ChatResponse
			if err := json.Unmarshal(raw, &cached); err == nil {
				s.logCacheHit(r.Context(), req, &cached, time.Since(cstart))
				w.Header().Set("X-Agentmodel-Cache", "hit")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(raw) // the cached bytes ARE the original response — no re-encode
				s.logChatContent(r.Context(), req, cached)
				return
			}
		}
		w.Header().Set("X-Agentmodel-Cache", "miss")
	}

	start := time.Now()
	resp, meta, err := s.router.CompleteBlocked(r.Context(), req, blocked)
	latency := time.Since(start)

	if err != nil {
		s.logFailure(r.Context(), req, err, latency, meta)
		writeAPIError(w, err)
		return
	}

	// Compute cost from the final usage. Subscription models resolve to 0;
	// API-key models compute via the per-token registry. Use resp.Model
	// (resolved upstream id) since the registry is keyed on actual model ids
	// rather than the caller's logical alias.
	src := s.priceUsage(r.Context(), resp.Model, &resp.Usage)
	if resp.ID == "" {
		resp.ID = "chatcmpl-" + uuid.NewString()
	}
	// Backfill created: the OpenAI contract requires a non-zero unix timestamp,
	// but not every provider sets it (the Anthropic adapter leaves it 0). Stamp
	// it here so all providers' responses are OpenAI-shape-conformant (#664).
	if resp.Created == 0 {
		resp.Created = time.Now().Unix()
	}

	s.logSuccess(r.Context(), req, &resp, latency, src)

	// Marshal once: the same bytes serve the client and (after) seed the cache,
	// avoiding a second full encode of the completion body on a cacheable miss.
	// The trailing newline matches json.Encoder's framing, so a cached replay
	// (which writes these exact bytes) is byte-identical to a fresh response.
	body, _ := json.Marshal(resp)
	body = append(body, '\n')
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)

	// Seed the cache after the client response is written (off the latency path),
	// with a detached context so a client that disconnects mid-response still
	// caches the result for the next caller. Streaming is not cached (#48 v1).
	if s.cache != nil && len(resp.Choices) > 0 {
		s.cache.Set(context.WithoutCancel(r.Context()), cacheKey, body)
	}

	// Content log last: its file write is off the path to the client response.
	s.logChatContent(r.Context(), req, resp)
}

// chatCacheKey derives a content-addressed cache key from the request fields
// that affect the model's output. It hashes the request itself with the two
// non-determinants zeroed — Stream (so a streamed and non-streamed call for the
// same prompt share an entry) and User (a tracking tag, not an output
// determinant; matching LiteLLM's default key). Keying by exclusion rather than
// an allowlist means a newly-added request field is key-affecting by default:
// the safe failure direction for a cache key is a spurious miss, never a wrong
// hit. req is a value copy, so zeroing here does not touch the caller's request.
func chatCacheKey(req agentmodel.ChatRequest) string {
	req.Stream = false
	req.User = ""
	b, err := json.Marshal(req)
	if err != nil {
		return "chat:nokey:" + uuid.NewString()
	}
	sum := sha256.Sum256(b)
	return "chat:" + hex.EncodeToString(sum[:])
}

func (s *Server) handleStreamingChat(w http.ResponseWriter, r *http.Request, req agentmodel.ChatRequest, blocked []string) {
	start := time.Now()
	seq, meta, err := s.router.StreamBlocked(r.Context(), req, blocked)
	if err != nil {
		s.logFailure(r.Context(), req, err, time.Since(start), meta)
		writeAPIError(w, err)
		return
	}

	sw, err := streaming.NewWriter(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError, &agentmodel.Error{Type: agentmodel.ErrTypeUpstream, Message: "streaming unavailable: " + err.Error()})
		return
	}

	respID := "chatcmpl-" + uuid.NewString()
	created := time.Now().Unix()
	var finalUsage agentmodel.Usage
	var sawFinish bool
	var sawToolCall bool

	// Reassemble the streamed response for the content log (opt-in; nil when
	// content logging is disabled, so the merge cost is only paid when on).
	// logContent records the assembled response at whichever exit is reached
	// (client disconnect or normal completion); it reads finalUsage by capture.
	var contentAcc *chatContentAcc
	if s.contentLog.Enabled() {
		contentAcc = newChatContentAcc()
	}
	logContent := func() {
		if contentAcc != nil {
			s.logChatContent(r.Context(), req, contentAcc.response(respID, meta.ModelUsed, created, finalUsage))
		}
	}

	for chunk, err := range seq {
		if err != nil {
			// Route through Wrap so the SSE error frame carries the accurate
			// type and (when classifiable) code/param instead of a hardcoded
			// upstream_error with no code (#705).
			_ = sw.SendError(agentmodel.Wrap(err))
			_ = sw.Done()
			s.logFailure(r.Context(), req, err, time.Since(start), meta)
			return
		}

		// Translate provider StreamChunk -> OpenAI streaming chunk shape.
		out := streamChunkOpenAI{
			ID:      respID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   req.Model,
			Choices: []streamChunkChoice{
				{
					Index:        0,
					Delta:        chunk.Delta,
					FinishReason: chunk.FinishReason,
				},
			},
		}
		if chunk.FinishReason != "" {
			sawFinish = true
		}
		if len(chunk.Delta.ToolCalls) > 0 {
			sawToolCall = true
		}
		if contentAcc != nil {
			contentAcc.add(chunk.Delta, chunk.FinishReason)
		}
		if chunk.Usage != nil {
			out.Usage = chunk.Usage
			finalUsage = *chunk.Usage
		}
		if err := sw.Send(out); err != nil {
			// Client disconnected. The upstream tokens were still generated
			// and billed, so persist whatever usage we accumulated — an
			// unlogged aborted stream would be invisible to the cost ledger
			// and let a capped key evade its budget by aborting streams
			// (#47). Usage may be zero if the provider's usage chunk never
			// arrived; logging the row is still the right fail-direction.
			finalUsage.AuthMode = meta.AuthMode
			finalUsage.Provider = meta.Provider
			src := s.priceUsage(r.Context(), meta.ModelUsed, &finalUsage)
			resp := agentmodel.ChatResponse{ID: respID, Model: meta.ModelUsed, Usage: finalUsage}
			s.logSuccess(r.Context(), req, &resp, time.Since(start), src)
			logContent()
			return
		}
	}

	// Safety net: a provider may end a stream cleanly without ever emitting a
	// terminal finish_reason (e.g. an upstream connection drop the provider
	// failed to surface). Strict OpenAI clients reject such a stream with
	// "Stream ended without finish_reason", so synthesize a terminal chunk to
	// guarantee well-formed output regardless of provider — mirroring LiteLLM's
	// finish_reason_handler default. Prefer "tool_calls" when tool-call deltas
	// were seen: defaulting to "stop" mid tool call makes agents conclude the
	// turn ended and silently drop the call (cf. LiteLLM #19744, #12862).
	if !sawFinish {
		reason := "stop"
		if sawToolCall {
			reason = "tool_calls"
		}
		_ = sw.Send(streamChunkOpenAI{
			ID:      respID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   req.Model,
			Choices: []streamChunkChoice{{Index: 0, FinishReason: reason}},
		})
	}
	_ = sw.Done()

	// Compute cost retroactively after stream completion.
	finalUsage.AuthMode = meta.AuthMode
	finalUsage.Provider = meta.Provider
	src := s.priceUsage(r.Context(), meta.ModelUsed, &finalUsage)
	resp := agentmodel.ChatResponse{
		ID:    respID,
		Model: meta.ModelUsed,
		Usage: finalUsage,
	}
	s.logSuccess(r.Context(), req, &resp, time.Since(start), src)
	logContent()
}

type streamChunkOpenAI struct {
	ID      string              `json:"id"`
	Object  string              `json:"object"`
	Created int64               `json:"created"`
	Model   string              `json:"model"`
	Choices []streamChunkChoice `json:"choices"`
	Usage   *agentmodel.Usage   `json:"usage,omitempty"`
}

type streamChunkChoice struct {
	Index        int                `json:"index"`
	Delta        agentmodel.Message `json:"delta"`
	FinishReason string             `json:"finish_reason,omitempty"`
}

// ─── Logging helpers ──────────────────────────────────────────────────────

// writeRequestLog persists one audit row. Caller fills in everything except
// ID, CreatedAt, OrgID, APIKeyHash — those are stamped here so callers don't
// repeat the boilerplate.
//
// Log ID is independent of any response ID — providers may reuse the same
// completion ID across concurrent calls (e.g. the stub), so we always
// generate a fresh UUID per audit row.
func (s *Server) writeRequestLog(ctx context.Context, rl store.RequestLog) {
	if rl.ID == "" {
		rl.ID = "log-" + uuid.NewString()
	}
	if rl.CreatedAt.IsZero() {
		rl.CreatedAt = time.Now().UTC()
	}
	rl.OrgID = orgIDFromCtx(ctx)
	rl.APIKeyHash = apiKeyHashFromCtx(ctx)
	rl.RequestID = middleware.GetReqID(ctx) // same id the content log keys on — lets the two join

	// The audit row must land even when the client has gone away — a
	// canceled request context would otherwise abort the store write,
	// making disconnected streams invisible to budget enforcement (#47).
	ctx = context.WithoutCancel(ctx)

	// Fan the audit row out to the observability sinks (Prometheus + OTLP).
	// No-op when telemetry is disabled; independent of the store so metrics
	// still flow if persistence is off.
	s.telemetry.RecordRequest(ctx, rl)

	if s.store != nil {
		_ = s.store.LogRequest(ctx, rl)
	}
}

// statusOk / statusError populate RequestLog.Status across all endpoints so
// downstream queries can filter on a stable vocabulary.
const (
	statusOk    = "ok"
	statusError = "error"
)

// priceUsage computes the cost for modelUsed, writes it into usage.CostUSD, and
// returns the cost Source ("priced" | "subscription" | "unpriced") for the
// audit log. When the model is absent from the price registry the cost is $0
// but the Source is "unpriced" — and we emit a warning — so the spend gap is
// visible rather than silently logged as a real $0. See issue #511.
func (s *Server) priceUsage(ctx context.Context, modelUsed string, usage *agentmodel.Usage) string {
	c, src := cost.CalculateWithSource(modelUsed, *usage, s.registry)
	usage.CostUSD = c
	if src == cost.SourceUnpriced {
		s.logger.WarnContext(ctx, "agentmodel: model missing from price registry; cost recorded as $0",
			"model", modelUsed, "org", orgIDFromCtx(ctx))
	}
	return string(src)
}

// buildUsageLog assembles a RequestLog from the per-call values that vary
// (models, usage, latency, status, costSource). Shared by handlers_chat and
// handlers_messages so the audit row shape stays in lockstep — token-field
// mapping changes only need to land in one place.
func buildUsageLog(modelRequested, modelUsed string, usage agentmodel.Usage, latency time.Duration, status, errType, costSource string) store.RequestLog {
	return store.RequestLog{
		ModelRequested:           modelRequested,
		ModelUsed:                modelUsed,
		Provider:                 usage.Provider,
		AuthMode:                 usage.AuthMode,
		PromptTokens:             usage.PromptTokens,
		CompletionTokens:         usage.CompletionTokens,
		TotalTokens:              usage.TotalTokens,
		CacheReadInputTokens:     usage.CacheReadInputTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
		CostUSD:                  usage.CostUSD,
		CostSource:               costSource,
		LatencyMs:                int(latency.Milliseconds()),
		Status:                   status,
		ErrorType:                errType,
	}
}

func (s *Server) logSuccess(ctx context.Context, req agentmodel.ChatRequest, resp *agentmodel.ChatResponse, latency time.Duration, costSource string) {
	s.writeRequestLog(ctx, buildUsageLog(req.Model, resp.Model, resp.Usage, latency, statusOk, "", costSource))
}

// logCacheHit records a cache-served response. Token counts are preserved (so
// "tokens saved" is visible) but CostUSD is forced to 0 — the request consumed
// no upstream tokens, so it must not advance any budget window.
func (s *Server) logCacheHit(ctx context.Context, req agentmodel.ChatRequest, resp *agentmodel.ChatResponse, latency time.Duration) {
	rl := buildUsageLog(req.Model, resp.Model, resp.Usage, latency, statusOk, "", string(cost.SourceCache))
	rl.CostUSD = 0
	s.writeRequestLog(ctx, rl)
}

func (s *Server) logFailure(ctx context.Context, req agentmodel.ChatRequest, err error, latency time.Duration, meta router.StreamMeta) {
	ae := agentmodel.Wrap(err)
	// Failed requests have no computed cost, so leave CostSource empty. meta
	// attributes the failing deployment (empty for pre-routing rejections and
	// multi-deployment exhaustion, which have no single one to blame) so the
	// audit row carries provider/auth_mode like the /v1/messages path (#1492).
	usage := agentmodel.Usage{Provider: meta.Provider, AuthMode: meta.AuthMode}
	// model_used is the resolved UPSTREAM id when known (matching
	// logMessagesFailure), falling back to the caller's alias — otherwise the
	// same failure reads differently depending on which frontend was hit.
	modelUsed := req.Model
	if meta.ModelUsed != "" {
		modelUsed = meta.ModelUsed
	}
	rl := buildUsageLog(req.Model, modelUsed, usage, latency, statusError, ae.Type, "")
	s.writeRequestLog(ctx, rl)
}

// writeAPIError translates an internal error into an HTTP error response.
func writeAPIError(w http.ResponseWriter, err error) {
	ae := agentmodel.Wrap(err)
	writeError(w, ae.HTTPStatus(), ae)
}

// setRetryAfter emits the RFC 9110 Retry-After header when the error carries an
// upstream hint, so a client backs off for as long as the upstream actually
// asked instead of guessing. Whole seconds, rounded up: a sub-second hint would
// floor to "0" and read as "retry immediately". Silent when there is no hint —
// an invented value is a guess dressed as fact.
func setRetryAfter(w http.ResponseWriter, e *agentmodel.Error) {
	if e == nil || e.RetryAfter <= 0 {
		return
	}
	secs := int64(math.Ceil(e.RetryAfter.Seconds()))
	w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
}

// writeError writes an OpenAI-shaped error JSON response. It marshals the typed
// *agentmodel.Error so the struct's omitempty tags apply — an empty Code or
// Param is omitted from the wire entirely rather than emitted as "" (#705). All
// three official OpenAI SDKs treat an absent code identically to a null one.
func writeError(w http.ResponseWriter, status int, e *agentmodel.Error) {
	w.Header().Set("Content-Type", "application/json")
	setRetryAfter(w, e)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error *agentmodel.Error `json:"error"`
	}{e})
}
