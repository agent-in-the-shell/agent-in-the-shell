package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
)

// responses implements a deliberately narrow OpenAI Responses frontend. The
// body remains in Responses shape end-to-end so hosted-tool calls, annotations,
// and future upstream event types survive without a lossy chat conversion.
func (s *Server) responses(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, agentmodel.NewErrorf(agentmodel.ErrTypeInvalidRequest, "read body: %v", err))
		return
	}
	var peek struct {
		Model  string `json:"model"`
		Stream *bool  `json:"stream"`
	}
	if err := json.Unmarshal(body, &peek); err != nil {
		writeError(w, http.StatusBadRequest, &agentmodel.Error{Type: agentmodel.ErrTypeInvalidRequest, Code: agentmodel.CodeInvalidJSON, Message: "invalid JSON: " + err.Error()})
		return
	}
	if peek.Model == "" {
		writeError(w, http.StatusBadRequest, &agentmodel.Error{Type: agentmodel.ErrTypeInvalidRequest, Code: agentmodel.CodeMissingRequiredParam, Param: "model", Message: "model is required"})
		return
	}
	clientStreams := peek.Stream != nil && *peek.Stream
	body, err = responsesUpstreamBody(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, &agentmodel.Error{Type: agentmodel.ErrTypeInvalidRequest, Code: agentmodel.CodeInvalidJSON, Message: "invalid JSON: " + err.Error()})
		return
	}

	vk := vkFromCtx(r.Context())
	if ae := s.enforce(r.Context(), vk, peek.Model); ae != nil {
		s.logResponsesFailure(r, peek.Model, router.Deployment{}, ae, 0)
		writeAPIError(w, ae)
		return
	}
	upstream, dep, servedModel, err := s.router.ResponsesPassthroughBlocked(r.Context(), body, peek.Model, s.blockedModels(vk))
	if err != nil {
		s.logResponsesFailure(r, peek.Model, dep, err, time.Since(start))
		writeAPIError(w, err)
		return
	}
	defer upstream.Body.Close()
	stopCancel := context.AfterFunc(r.Context(), func() { _ = upstream.Body.Close() })
	defer stopCancel()

	// The ChatGPT Codex backend currently labels successful SSE as text/plain.
	// This endpoint has already required stream:true, so status — not the
	// unreliable Content-Type — decides whether the body is streamed.
	if upstream.StatusCode/100 != 2 {
		respBody, readErr := io.ReadAll(upstream.Body)
		if readErr != nil {
			s.logResponsesFailure(r, peek.Model, dep, readErr, time.Since(start))
			writeError(w, http.StatusBadGateway, agentmodel.NewErrorf(agentmodel.ErrTypeUpstream, "read upstream body: %v", readErr))
			return
		}
		copyEndToEndHeaders(w.Header(), upstream.Header)
		w.WriteHeader(upstream.StatusCode)
		_, _ = w.Write(respBody)
		status, errType := statusOk, ""
		if upstream.StatusCode/100 != 2 {
			status, errType = statusError, agentmodel.ErrTypeUpstream
		}
		usage := responsesUsageFromJSON(respBody)
		s.logResponsesResult(r, peek.Model, dep, servedModel, usage, start, status, errType)
		return
	}
	if clientStreams {
		s.streamResponses(w, r, upstream, peek.Model, servedModel, dep, start)
		return
	}
	s.bufferResponses(w, r, upstream, peek.Model, servedModel, dep, start)
}

// responsesUpstreamBody makes the stream-only ChatGPT backend usable through
// both modes of the public Responses interface. Omitting store means false for
// this gateway: the subscription backend rejects a missing value and does not
// support server-side response persistence.
func responsesUpstreamBody(body []byte) ([]byte, error) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	request["stream"] = json.RawMessage("true")
	if _, ok := request["store"]; !ok {
		request["store"] = json.RawMessage("false")
	}
	// The subscription backend rejects max_output_tokens rather than applying
	// it. Match the chat-completions adapter's established behavior: omit the
	// unsupported cap instead of making an otherwise valid request fail.
	delete(request, "max_output_tokens")
	return json.Marshal(request)
}

func copyEndToEndHeaders(dst, src http.Header) {
	for k, values := range src {
		if _, hop := hopByHopHeaders[http.CanonicalHeaderKey(k)]; hop {
			continue
		}
		for _, value := range values {
			dst.Add(k, value)
		}
	}
}

func (s *Server) bufferResponses(w http.ResponseWriter, r *http.Request, upstream *http.Response, requested, served string, dep router.Deployment, start time.Time) {
	parser := newResponsesStreamParser()
	var transformed bytes.Buffer
	transformer := newResponsesStreamTransformer(&transformed, parser)
	_, copyErr := io.Copy(transformer, upstream.Body)
	if finishErr := transformer.finish(); copyErr == nil {
		copyErr = finishErr
	}
	summary := parser.finish()
	if copyErr != nil {
		summary.status, summary.errType = statusError, agentmodel.ErrTypeUpstream
		s.logResponsesResult(r, requested, dep, served, summary.usage, start, summary.status, summary.errType)
		writeError(w, http.StatusBadGateway, agentmodel.NewErrorf(agentmodel.ErrTypeUpstream, "read upstream stream: %v", copyErr))
		return
	}
	if len(transformer.terminalResponse) == 0 {
		summary.status, summary.errType = statusError, agentmodel.ErrTypeUpstream
		s.logResponsesResult(r, requested, dep, served, summary.usage, start, summary.status, summary.errType)
		writeError(w, http.StatusBadGateway, agentmodel.NewErrorf(agentmodel.ErrTypeUpstream, "upstream stream ended without a terminal response"))
		return
	}
	copyEndToEndHeaders(w.Header(), upstream.Header)
	w.Header().Del("Content-Length")
	w.Header().Del("Transfer-Encoding")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(upstream.StatusCode)
	_, _ = w.Write(transformer.terminalResponse)
	s.logResponsesResult(r, requested, dep, served, summary.usage, start, summary.status, summary.errType)
}

func (s *Server) streamResponses(w http.ResponseWriter, r *http.Request, upstream *http.Response, requested, served string, dep router.Deployment, start time.Time) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, agentmodel.NewErrorf(agentmodel.ErrTypeUpstream, "streaming unavailable"))
		return
	}
	copyEndToEndHeaders(w.Header(), upstream.Header)
	// Terminal events may grow when an upstream omits response.output, so the
	// upstream byte count is no longer authoritative.
	w.Header().Del("Content-Length")
	w.WriteHeader(upstream.StatusCode)
	flusher.Flush()

	fw := flushWriter{w: w, flusher: flusher}
	parser := newResponsesStreamParser()
	transformer := newResponsesStreamTransformer(fw, parser)
	_, copyErr := io.Copy(transformer, upstream.Body)
	if finishErr := transformer.finish(); copyErr == nil {
		copyErr = finishErr
	}
	summary := parser.finish()
	if copyErr != nil {
		summary.status, summary.errType = statusError, agentmodel.ErrTypeUpstream
	}
	s.logResponsesResult(r, requested, dep, served, summary.usage, start, summary.status, summary.errType)
}

type flushWriter struct {
	w       io.Writer
	flusher http.Flusher
}

func (w flushWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.flusher.Flush()
	return n, err
}

type responsesStreamSummary struct {
	usage           agentmodel.Usage
	status, errType string
}

const maxResponsesSSELine = 8 << 20

type responsesStreamTransformer struct {
	out              io.Writer
	observer         io.Writer
	event            []byte
	completedItems   map[int]json.RawMessage
	nextItemIndex    int
	terminalResponse json.RawMessage
	passthrough      bool
}

func newResponsesStreamTransformer(out, observer io.Writer) *responsesStreamTransformer {
	return &responsesStreamTransformer{
		out:            out,
		observer:       observer,
		event:          make([]byte, 0, 4096),
		completedItems: make(map[int]json.RawMessage),
	}
}

func (t *responsesStreamTransformer) Write(chunk []byte) (int, error) {
	written := len(chunk)
	_, _ = t.observer.Write(chunk)
	for len(chunk) > 0 {
		i := bytes.IndexByte(chunk, '\n')
		if i < 0 {
			t.event = append(t.event, chunk...)
			if len(t.event) > maxResponsesSSELine {
				if _, err := t.out.Write(t.event); err != nil {
					return 0, err
				}
				t.event = t.event[:0]
				t.passthrough = true
			}
			break
		}
		line := chunk[:i+1]
		chunk = chunk[i+1:]
		if t.passthrough {
			if _, err := t.out.Write(line); err != nil {
				return 0, err
			}
			if isSSEBlankLine(line) {
				t.passthrough = false
			}
			continue
		}
		t.event = append(t.event, line...)
		if len(t.event) > maxResponsesSSELine {
			if _, err := t.out.Write(t.event); err != nil {
				return 0, err
			}
			t.event = t.event[:0]
			t.passthrough = !isSSEBlankLine(line)
			continue
		}
		if isSSEBlankLine(line) {
			if err := t.flushEvent(); err != nil {
				return 0, err
			}
		}
	}
	return written, nil
}

func isSSEBlankLine(line []byte) bool {
	return bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n"))
}

func (t *responsesStreamTransformer) finish() error {
	if len(t.event) == 0 {
		return nil
	}
	if t.passthrough {
		_, err := t.out.Write(t.event)
		t.event = t.event[:0]
		return err
	}
	return t.flushEvent()
}

func (t *responsesStreamTransformer) flushEvent() error {
	raw := t.event
	t.event = t.event[:0]
	data := responsesSSEData(raw)
	if len(data) == 0 {
		_, err := t.out.Write(raw)
		return err
	}
	var envelope struct {
		Type        string          `json:"type"`
		Item        json.RawMessage `json:"item"`
		OutputIndex *int            `json:"output_index"`
		Response    struct {
			Output []json.RawMessage `json:"output"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		_, err := t.out.Write(raw)
		return err
	}
	switch envelope.Type {
	case "response.output_item.done":
		if len(envelope.Item) > 0 && string(envelope.Item) != "null" {
			index := t.nextItemIndex
			if envelope.OutputIndex != nil && *envelope.OutputIndex >= 0 {
				index = *envelope.OutputIndex
			}
			t.completedItems[index] = bytes.Clone(envelope.Item)
			if index >= t.nextItemIndex {
				t.nextItemIndex = index + 1
			}
		}
	case "response.completed":
		if len(envelope.Response.Output) == 0 {
			items := t.orderedCompletedItems()
			if len(items) > 0 {
				rewritten, err := responsesCompletedWithOutput(data, items)
				if err == nil {
					data = rewritten
					raw = replaceResponsesSSEData(raw, rewritten)
				}
			}
		}
	}
	if envelope.Type == "response.completed" || envelope.Type == "response.incomplete" || envelope.Type == "response.failed" {
		var terminal struct {
			Response json.RawMessage `json:"response"`
		}
		if json.Unmarshal(data, &terminal) == nil && len(terminal.Response) > 0 {
			t.terminalResponse = bytes.Clone(terminal.Response)
		}
	}
	_, err := t.out.Write(raw)
	return err
}

func (t *responsesStreamTransformer) orderedCompletedItems() []json.RawMessage {
	items := make([]json.RawMessage, 0, len(t.completedItems))
	for i := 0; i < t.nextItemIndex; i++ {
		if item := t.completedItems[i]; len(item) > 0 {
			items = append(items, item)
		}
	}
	return items
}

func responsesSSEData(raw []byte) []byte {
	var data []byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		if len(data) > 0 {
			data = append(data, '\n')
		}
		data = append(data, bytes.TrimSpace(line[len("data:"):])...)
	}
	return data
}

func responsesCompletedWithOutput(data []byte, items []json.RawMessage) ([]byte, error) {
	var event map[string]json.RawMessage
	if err := json.Unmarshal(data, &event); err != nil {
		return nil, err
	}
	var response map[string]json.RawMessage
	if err := json.Unmarshal(event["response"], &response); err != nil {
		return nil, err
	}
	output, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	response["output"] = output
	event["response"], err = json.Marshal(response)
	if err != nil {
		return nil, err
	}
	return json.Marshal(event)
}

func replaceResponsesSSEData(raw, data []byte) []byte {
	newline := []byte("\n")
	if bytes.Contains(raw, []byte("\r\n")) {
		newline = []byte("\r\n")
	}
	lines := bytes.Split(raw, newline)
	out := make([]byte, 0, len(raw)+len(data))
	replaced := false
	for _, line := range lines {
		if bytes.HasPrefix(line, []byte("data:")) {
			if replaced {
				continue
			}
			line = append([]byte("data: "), data...)
			replaced = true
		}
		if len(out) > 0 {
			out = append(out, newline...)
		}
		out = append(out, line...)
	}
	return out
}

type responsesStreamParser struct {
	line          []byte
	data          []byte
	discardLine   bool
	discardEvent  bool
	terminalEvent string
	usage         agentmodel.Usage
}

func newResponsesStreamParser() *responsesStreamParser {
	return &responsesStreamParser{
		line: make([]byte, 0, 4096),
		data: make([]byte, 0, 4096),
	}
}

// Write observes SSE without participating in delivery. It always consumes the
// full chunk and never returns an error, so malformed or oversized side-channel
// input cannot truncate or backpressure the byte-for-byte client stream.
func (p *responsesStreamParser) Write(chunk []byte) (int, error) {
	written := len(chunk)
	for len(chunk) > 0 {
		i := bytes.IndexByte(chunk, '\n')
		if i < 0 {
			p.appendLine(chunk)
			break
		}
		p.appendLine(chunk[:i])
		p.finishLine()
		chunk = chunk[i+1:]
	}
	return written, nil
}

func (p *responsesStreamParser) appendLine(fragment []byte) {
	if p.discardLine {
		return
	}
	if len(p.line)+len(fragment) > maxResponsesSSELine {
		p.line = p.line[:0]
		p.discardLine = true
		p.discardEvent = true
		return
	}
	p.line = append(p.line, fragment...)
}

func (p *responsesStreamParser) finishLine() {
	if p.discardLine {
		p.discardLine = false
		p.line = p.line[:0]
		return
	}
	line := bytes.TrimSuffix(p.line, []byte{'\r'})
	if len(line) == 0 {
		p.finishEvent()
	} else if bytes.HasPrefix(line, []byte("data:")) && !p.discardEvent {
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(p.data)+len(payload)+1 > maxResponsesSSELine {
			p.data = p.data[:0]
			p.discardEvent = true
			p.line = p.line[:0]
			return
		}
		if len(p.data) > 0 {
			p.data = append(p.data, '\n')
		}
		p.data = append(p.data, payload...)
	}
	p.line = p.line[:0]
}

func (p *responsesStreamParser) finishEvent() {
	if p.discardEvent {
		p.discardEvent = false
		p.data = p.data[:0]
		return
	}
	if len(p.data) == 0 {
		return
	}
	u := responsesUsageFromEvent(p.data)
	if u.TotalTokens > 0 {
		p.usage = p.usage.Absorb(u)
	} else if u.ReasoningTokens != nil {
		// A detail-only frame updates the count without replacing the totals.
		p.usage.ReasoningTokens = u.ReasoningTokens
	}
	var event struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(p.data, &event) == nil {
		switch event.Type {
		case "response.completed":
			if p.terminalEvent == "" {
				p.terminalEvent = event.Type
			}
		case "response.failed", "response.incomplete", "error":
			p.terminalEvent = event.Type
		}
	}
	p.data = p.data[:0]
}

func (p *responsesStreamParser) finish() responsesStreamSummary {
	if len(p.line) > 0 || p.discardLine {
		p.finishLine()
	}
	p.finishEvent()
	summary := responsesStreamSummary{usage: p.usage, status: statusError, errType: agentmodel.ErrTypeUpstream}
	if p.terminalEvent == "response.completed" {
		summary.status, summary.errType = statusOk, ""
	}
	return summary
}

func responsesUsageFromEvent(raw []byte) agentmodel.Usage {
	var event struct {
		Response struct {
			Usage responsesUsageBlock `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &event) != nil {
		return agentmodel.Usage{}
	}
	return event.Response.Usage.agentmodel()
}

func responsesUsageFromJSON(raw []byte) agentmodel.Usage {
	var response struct {
		Usage responsesUsageBlock `json:"usage"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return agentmodel.Usage{}
	}
	return response.Usage.agentmodel()
}

type responsesUsageBlock struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
	InputDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputDetails struct {
		ReasoningTokens *int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func (u responsesUsageBlock) agentmodel() agentmodel.Usage {
	return agentmodel.Usage{PromptTokens: u.InputTokens, CompletionTokens: u.OutputTokens, TotalTokens: u.TotalTokens, CacheReadInputTokens: u.InputDetails.CachedTokens, ReasoningTokens: u.OutputDetails.ReasoningTokens}
}

func (s *Server) logResponsesResult(r *http.Request, requested string, dep router.Deployment, served string, usage agentmodel.Usage, start time.Time, status, errType string) {
	usage.Provider, usage.AuthMode = dep.Provider.Name(), dep.Provider.AuthMode()
	s.router.RecordTokens(served, dep.Name, usage.TotalTokens)
	src := s.priceUsage(r.Context(), dep.Model, &usage)
	s.writeRequestLog(r.Context(), buildUsageLog(requested, dep.Model, usage, time.Since(start), status, errType, src))
}

func (s *Server) logResponsesFailure(r *http.Request, requested string, dep router.Deployment, err error, latency time.Duration) {
	ae := agentmodel.Wrap(err)
	used, usage, src := requested, agentmodel.Usage{}, ""
	if dep.Provider != nil {
		used = dep.Model
		usage.Provider, usage.AuthMode = dep.Provider.Name(), dep.Provider.AuthMode()
		src = s.priceUsage(r.Context(), used, &usage)
	}
	s.writeRequestLog(r.Context(), buildUsageLog(requested, used, usage, latency, statusError, ae.Type, src))
}
