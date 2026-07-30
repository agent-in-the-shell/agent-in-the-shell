package api

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
)

// hopByHopHeaders cannot be proxied per RFC 7230 § 6.1 — they describe the
// connection between this proxy and the upstream, not the end-to-end
// payload. Forwarding them confuses HTTP/2 clients and intermediaries.
var hopByHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

// anthropicUsageBlock is the on-the-wire usage shape Anthropic uses both in
// non-streaming responses (top-level usage) and in SSE message_start /
// message_delta events. Defined once so the four field names live in one
// place.
type anthropicUsageBlock struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// messages implements POST /v1/messages — an Anthropic-shaped frontend that
// proxies straight to an anthropic-provider deployment without the
// OpenAI⇄Anthropic conversion layer used by /v1/chat/completions. Lets
// Anthropic-SDK clients (pi-ai, anthropic-sdk) use agentmodel as a drop-in
// replacement for api.anthropic.com while still recording auth refresh,
// audit, and cost.
//
// Uses router.MessagesPassthrough for full fallback + cooldown semantics,
// matching the behavior of /v1/chat/completions.
func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, agentmodel.ErrTypeInvalidRequest, "read body: "+err.Error())
		return
	}

	var peek struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &peek); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, agentmodel.ErrTypeInvalidRequest, "invalid JSON: "+err.Error())
		return
	}
	if peek.Model == "" {
		writeAnthropicError(w, http.StatusBadRequest, agentmodel.ErrTypeInvalidRequest, "model is required")
		return
	}

	// Pre-request enforcement (#47/#52); mirrors chatCompletions but with the
	// Anthropic error envelope. Latency 0 for policy rejections, matching
	// chat/embeddings — the elapsed time so far is body-read, not enforcement.
	vk := vkFromCtx(r.Context())
	if ae := s.enforce(r.Context(), vk, peek.Model); ae != nil {
		s.logMessagesFailure(r, peek.Model, router.Deployment{}, ae, 0)
		writeAnthropicAPIError(w, ae)
		return
	}

	clientBetas := strings.Join(r.Header.Values("anthropic-beta"), ",")
	upstream, dep, servedModel, err := s.router.MessagesPassthroughBlocked(r.Context(), body, peek.Model, clientBetas, s.blockedModels(vk))
	if err != nil {
		s.logMessagesFailure(r, peek.Model, dep, err, time.Since(start))
		writeAnthropicAPIError(w, err)
		return
	}
	defer upstream.Body.Close()

	// SSE only when upstream actually opened one. On non-2xx replies we fall
	// through to the JSON path so the caller sees the upstream error body
	// verbatim instead of a half-open SSE.
	if peek.Stream && upstream.StatusCode == http.StatusOK &&
		strings.HasPrefix(upstream.Header.Get("Content-Type"), "text/event-stream") {
		s.streamMessages(w, r, upstream, body, peek.Model, servedModel, dep, start)
		return
	}
	s.proxyMessagesJSON(w, r, upstream, body, peek.Model, servedModel, dep, start)
}

// proxyMessagesJSON copies a non-streaming upstream response back to the
// client and records one audit row.
func (s *Server) proxyMessagesJSON(w http.ResponseWriter, r *http.Request, upstream *http.Response, reqBody []byte, modelRequested, servedModel string, dep router.Deployment, start time.Time) {
	modelUsed := dep.Model
	respBody, err := io.ReadAll(upstream.Body)
	if err != nil {
		s.logMessagesFailure(r, modelRequested, dep, err, time.Since(start))
		writeAnthropicError(w, http.StatusBadGateway, agentmodel.ErrTypeUpstream, "read upstream body: "+err.Error())
		return
	}

	usage := parseAnthropicUsage(respBody)
	usage.AuthMode = dep.Provider.AuthMode()
	// The cost lookup keys on "<provider>/<model>" (the catalog's canonical
	// form) before falling back to the bare model id, so an unset Provider
	// silently downgrades every subscription model on this path to "unpriced".
	usage.Provider = dep.Provider.Name()
	// Credit TPM where the walk metered: servedModel is the logical model the
	// router admitted the dispatch under (the fallback target when one fired);
	// the audit row keeps the caller's requested model.
	s.router.RecordTokens(servedModel, dep.Name, usage.TotalTokens)
	src := s.priceUsage(r.Context(), modelUsed, &usage)

	w.Header().Set("Content-Type", upstream.Header.Get("Content-Type"))
	w.WriteHeader(upstream.StatusCode)
	_, _ = w.Write(respBody)

	status, errType := statusOk, ""
	if upstream.StatusCode/100 != 2 {
		status, errType = statusError, agentmodel.ErrTypeUpstream
	}
	s.writeRequestLog(r.Context(), buildUsageLog(modelRequested, modelUsed, usage, time.Since(start), status, errType, src))

	// Content log: the request and the upstream reply are both raw Anthropic
	// JSON, so log them verbatim (no reassembly needed for the non-stream path).
	s.writeContent(r.Context(), reqBody, respBody)
}

// streamMessages copies an Anthropic SSE stream byte-for-byte to the client
// while parsing message_start / message_delta events on the side to extract
// final usage for the audit log. The wire bytes the SDK sees are unchanged
// from what api.anthropic.com would have sent.
func (s *Server) streamMessages(w http.ResponseWriter, r *http.Request, upstream *http.Response, reqBody []byte, modelRequested, servedModel string, dep router.Deployment, start time.Time) {
	modelUsed, authMode, providerName := dep.Model, dep.Provider.AuthMode(), dep.Provider.Name()
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, http.StatusInternalServerError, agentmodel.ErrTypeUpstream, "streaming unavailable")
		return
	}

	for k, vs := range upstream.Header {
		if _, hop := hopByHopHeaders[http.CanonicalHeaderKey(k)]; hop {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(upstream.StatusCode)
	flusher.Flush()

	scanner := bufio.NewScanner(upstream.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var (
		eventName  string
		dataBuf    strings.Builder
		usage      anthropicUsageBlock
		clientGone bool
		// streamErrType is set when the upstream emits a terminal `error`
		// frame. The bytes still reach the client verbatim; this only decides
		// how the request is audited.
		streamErrType string
	)

	// Reassemble the streamed message for the content log (opt-in; nil when
	// content logging is disabled, so the parse cost is only paid when on).
	var asm *anthropicMessageAssembler
	if s.contentLog.Enabled() {
		asm = newAnthropicMessageAssembler()
	}

	writeLine := func(line string) {
		if clientGone {
			return
		}
		if _, err := io.WriteString(w, line); err != nil {
			clientGone = true
		}
	}

	// flushBlank runs at an event boundary (the blank line). The blank line
	// itself is already forwarded by the normal line write below
	// (writeLine(line+"\n") with line==""); writing it again here would emit a
	// double blank, breaking the documented byte-for-byte copy. So we only
	// flush + parse usage + reset here, leaving the separator to the caller.
	flushBlank := func() {
		flusher.Flush()
		if eventName != "" || dataBuf.Len() > 0 {
			data := dataBuf.String()
			updateUsageFromEvent(eventName, data, &usage)
			if t := errorTypeFromEvent(eventName, data); t != "" {
				streamErrType = t
			}
			if asm != nil {
				asm.feed(eventName, data)
			}
		}
		eventName = ""
		dataBuf.Reset()
	}

	for scanner.Scan() {
		if clientGone {
			break
		}
		line := scanner.Text()
		writeLine(line + "\n")
		if line == "" {
			flushBlank()
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			d := strings.TrimPrefix(line, "data:")
			d = strings.TrimPrefix(d, " ")
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(d)
		}
	}
	if eventName != "" || dataBuf.Len() > 0 {
		data := dataBuf.String()
		updateUsageFromEvent(eventName, data, &usage)
		if t := errorTypeFromEvent(eventName, data); t != "" {
			streamErrType = t
		}
		if asm != nil {
			asm.feed(eventName, data)
		}
	}

	// A transport read error (dropped upstream socket, or a line exceeding the
	// scanner's 1 MiB cap) ends the loop with no `error` frame, so streamErrType
	// stays empty and the request would audit as a success. Treat it as an
	// upstream failure. A client-side disconnect (clientGone) is not an upstream
	// fault, so it is excluded.
	if streamErrType == "" && !clientGone && scanner.Err() != nil {
		streamErrType = agentmodel.ErrTypeUpstream
	}

	final := usageBlockToAgentmodel(usage)
	final.AuthMode = authMode
	final.Provider = providerName
	s.router.RecordTokens(servedModel, dep.Name, final.TotalTokens)
	src := s.priceUsage(r.Context(), modelUsed, &final)
	// A stream that ended on an `error` frame is a failed request, however many
	// tokens it billed before dying. Auditing it as statusOk made mid-stream
	// upstream failures invisible to /v1/usage and to any health check reading
	// the request log.
	status, errType := statusOk, ""
	if streamErrType != "" {
		status, errType = statusError, streamErrType
	}
	s.writeRequestLog(r.Context(), buildUsageLog(modelRequested, modelUsed, final, time.Since(start), status, errType, src))

	// Content log: the request is raw Anthropic JSON; the response is the
	// stream reassembled into a final Messages object. A nil finalize() (no
	// message_start seen) is skipped — writeContent embeds it verbatim.
	if asm != nil {
		if msg := asm.finalize(); msg != nil {
			s.writeContent(r.Context(), reqBody, msg)
		}
	}
}

// parseAnthropicUsage extracts token counts from a non-streaming Anthropic
// Messages response body.
func parseAnthropicUsage(body []byte) agentmodel.Usage {
	var resp struct {
		Usage anthropicUsageBlock `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return agentmodel.Usage{}
	}
	return usageBlockToAgentmodel(resp.Usage)
}

// errorTypeFromEvent reports the error type carried by a terminal SSE `error`
// frame, or "" for any other event.
//
// Anthropic's wire shape is {"type":"error","error":{"type":"overloaded_error",
// "message":...}}, and messagesbridge synthesizes the same shape with
// agentmodel's classified types. Either the event name or the payload's
// top-level type identifies the frame — upstreams have been seen to send one
// without the other. A frame we recognize but cannot parse still means the
// request failed, so it degrades to ErrTypeUpstream rather than to "success".
func errorTypeFromEvent(name, raw string) string {
	if raw == "" || raw == "[DONE]" {
		if name == "error" {
			return agentmodel.ErrTypeUpstream
		}
		return ""
	}
	var frame struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &frame); err != nil {
		if name == "error" {
			return agentmodel.ErrTypeUpstream
		}
		return ""
	}
	if name != "error" && frame.Type != "error" {
		return ""
	}
	if frame.Error.Type != "" {
		return frame.Error.Type
	}
	return agentmodel.ErrTypeUpstream
}

// updateUsageFromEvent merges token counts from a single SSE event into the
// running tally. Errors are silently ignored — a malformed usage event must
// not break the byte-for-byte stream forwarding to the client.
func updateUsageFromEvent(name, raw string, dst *anthropicUsageBlock) {
	if raw == "" || raw == "[DONE]" {
		return
	}
	switch name {
	case "message_start":
		var ev struct {
			Message struct {
				Usage anthropicUsageBlock `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(raw), &ev); err == nil {
			dst.InputTokens = ev.Message.Usage.InputTokens
			dst.CacheReadInputTokens = ev.Message.Usage.CacheReadInputTokens
			dst.CacheCreationInputTokens = ev.Message.Usage.CacheCreationInputTokens
		}
	case "message_delta":
		var ev struct {
			Usage anthropicUsageBlock `json:"usage"`
		}
		if err := json.Unmarshal([]byte(raw), &ev); err == nil {
			if ev.Usage.OutputTokens > 0 {
				dst.OutputTokens = ev.Usage.OutputTokens
			}
			if ev.Usage.InputTokens > 0 {
				dst.InputTokens = ev.Usage.InputTokens
			}
			if ev.Usage.CacheReadInputTokens > 0 {
				dst.CacheReadInputTokens = ev.Usage.CacheReadInputTokens
			}
			if ev.Usage.CacheCreationInputTokens > 0 {
				dst.CacheCreationInputTokens = ev.Usage.CacheCreationInputTokens
			}
		}
	}
}

// usageBlockToAgentmodel maps the wire shape onto agentmodel.Usage following
// the OpenAI convention: PromptTokens INCLUDES the cached portion, with the
// breakdown reported separately so cost calculation can apply per-token
// rates to the cache-read / cache-write slices.
func usageBlockToAgentmodel(u anthropicUsageBlock) agentmodel.Usage {
	prompt := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	return agentmodel.Usage{
		PromptTokens:             prompt,
		CompletionTokens:         u.OutputTokens,
		TotalTokens:              prompt + u.OutputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens,
	}
}

// writeAnthropicError writes the Anthropic error envelope expected by SDK
// clients on /v1/messages, for a failure the gateway itself raised (no
// machine-readable code to report).
func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	writeAnthropicTypedError(w, status, &agentmodel.Error{Type: errType, Message: message})
}

// writeAnthropicTypedError marshals the typed error into the Anthropic
// envelope. Marshalling the struct (rather than flattening it to type+message)
// lets its omitempty tags apply: code and param appear when known and vanish
// when not. This keeps the JSON body in lockstep with the SSE error frame,
// which has carried the code since messagesbridge started classifying — a
// caller could otherwise read context_length_exceeded off a stream but not off
// the non-stream reply to the identical request.
func writeAnthropicTypedError(w http.ResponseWriter, status int, e *agentmodel.Error) {
	w.Header().Set("Content-Type", "application/json")
	setRetryAfter(w, e)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Type  string            `json:"type"`
		Error *agentmodel.Error `json:"error"`
	}{"error", e})
}

// writeAnthropicAPIError translates an internal error into an Anthropic HTTP error response.
func writeAnthropicAPIError(w http.ResponseWriter, err error) {
	ae := agentmodel.Wrap(err)
	writeAnthropicTypedError(w, ae.HTTPStatus(), ae)
}

// logMessagesFailure records a failed /v1/messages request. dep names the
// deployment that produced the failure when the router could attribute it; its
// zero value (dep.Provider == nil) means nothing ran or nothing single is at
// fault — a policy rejection, an unconfigured model, an exhausted walk — and
// the row then falls back to the requested model with no provider rather than
// blaming an arbitrary one.
func (s *Server) logMessagesFailure(r *http.Request, modelRequested string, dep router.Deployment, err error, latency time.Duration) {
	ae := agentmodel.Wrap(err)
	modelUsed, usage, src := modelRequested, agentmodel.Usage{}, ""
	if dep.Provider != nil {
		modelUsed = dep.Model
		usage.Provider = dep.Provider.Name()
		usage.AuthMode = dep.Provider.AuthMode()
		// Zero tokens either way, but the source explains the $0: a
		// subscription deployment's failure cost nothing by design, and a
		// genuinely unpriced model is worth the same warning it gets on
		// success. Skipped when nothing dispatched — pricing a model that was
		// never sent anywhere would warn about a missing price for no reason.
		src = s.priceUsage(r.Context(), modelUsed, &usage)
	}
	s.writeRequestLog(r.Context(), buildUsageLog(modelRequested, modelUsed, usage, latency, statusError, ae.Type, src))
}
