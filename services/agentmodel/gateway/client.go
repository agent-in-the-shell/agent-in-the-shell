// Package gateway is the Go client for the agent-model HTTP gateway.
//
// Services in this repo do not embed provider SDKs — they POST to a local
// agent-model instance, which owns provider routing, credentials, fallback and
// cost tracking. This package is that call, and it is the only agent-model
// package a consumer should need: it depends on services/agentmodel/wire (the
// request/response contract) and nothing else, so importing it never drags the
// server's router, store, or providers into a consumer's dependency closure.
//
// Scope: chat completions, blocking (Complete) and incremental (Stream). The
// gateway serves other routes (embeddings, images, video) whose request types
// exist in wire but have no method here yet.
package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/wire"
)

// Re-exported wire types, so a caller that only sends chat completions needs
// this one import. Anything richer — errors, embeddings, video — should import
// services/agentmodel/wire directly.
type (
	ChatRequest  = wire.ChatRequest
	ChatResponse = wire.ChatResponse
	Message      = wire.Message
	Choice       = wire.Choice
	Usage        = wire.Usage
)

// Default agent-model gateway endpoint and judge/utility model, shared by every
// command that builds a client from the environment (see NewFromEnv).
const (
	defaultBaseURL = "http://localhost:8090"
	defaultModel   = "claude-haiku-4-5-20251001"
)

// Client posts chat completion requests to the agentmodel HTTP gateway.
type Client struct {
	baseURL     string
	model       string
	bearerToken string
	http        *http.Client
}

func New(baseURL, model string) *Client {
	return &Client{
		baseURL: baseURL,
		model:   model,
		http:    &http.Client{},
	}
}

func NewWithToken(baseURL, model, bearerToken string) *Client {
	return &Client{
		baseURL:     baseURL,
		model:       model,
		bearerToken: bearerToken,
		http:        &http.Client{},
	}
}

// NewClientFromEnv builds a Client from the standard agent-model environment —
// AGENT_MODEL_URL (base URL), AGENT_MODEL_TOKEN (bearer, optional), and the model
// (modelOverride if non-empty, else AGENT_MODEL_MODEL) — applying the shared
// defaults. It centralizes the env convention every command that talks to the
// agent-model gateway would otherwise re-implement. An empty bearer token is
// harmless: Complete only sets the Authorization header when one is present.
func NewFromEnv(modelOverride string) *Client {
	model := modelOverride
	if model == "" {
		model = envOr("AGENT_MODEL_MODEL", defaultModel)
	}
	baseURL := strings.TrimRight(envOr("AGENT_MODEL_URL", defaultBaseURL), "/")
	return NewWithToken(baseURL, model, os.Getenv("AGENT_MODEL_TOKEN"))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Complete sends a chat request and returns the full response.
func (c *Client) Complete(ctx context.Context, req wire.ChatRequest) (wire.ChatResponse, error) {
	if req.Model == "" {
		req.Model = c.model
	}
	// A caller may build one ChatRequest and route it to either Complete or
	// Stream; force this to a blocking request regardless of what the caller
	// set, so a stray Stream:true doesn't make Complete try to decode an SSE
	// body as a single JSON object.
	req.Stream = false

	body, err := json.Marshal(req)
	if err != nil {
		return wire.ChatResponse{}, fmt.Errorf("gateway marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return wire.ChatResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.bearerToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.bearerToken)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return wire.ChatResponse{}, fmt.Errorf("gateway request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Surface the gateway's structured error (type + message) instead of a
		// bare status code, so callers can distinguish auth vs budget vs
		// bad-request failures.
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		var env struct {
			Error *wire.Error `json:"error"`
		}
		if json.Unmarshal(raw, &env) == nil && env.Error != nil {
			return wire.ChatResponse{}, fmt.Errorf("gateway status %d: %s: %s", resp.StatusCode, env.Error.Type, env.Error.Message)
		}
		return wire.ChatResponse{}, fmt.Errorf("gateway status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var result wire.ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return wire.ChatResponse{}, fmt.Errorf("gateway decode: %w", err)
	}
	return result, nil
}

// Model returns the configured model name.
func (c *Client) Model() string { return c.model }

// Stream is Complete's incremental sibling: it POSTs the same request with
// stream=true and returns a sequence of the chunks the gateway emits.
//
// The two error channels are distinct on purpose. The returned error covers
// failures that happen before any chunk arrives — a dial failure, a non-200
// status, or a 200 whose Content-Type isn't SSE — where the caller should not
// begin iterating. A failure part-way through the stream arrives as the
// second value of the sequence, after however many chunks did make it, so a
// caller can keep the partial output.
//
// The sequence ends at the `data: [DONE]` terminator or at EOF. Cancel ctx to
// stop early; the underlying response body is closed when iteration ends,
// whether it ran to completion or the caller broke out.
//
// Stream has already completed the HTTP round trip and holds a live response
// body by the time it returns, whether or not the caller ever ranges over the
// returned sequence. A caller that discards the sequence without iterating it
// must still cancel ctx eventually — that closes the body as a fallback — or
// the connection leaks for the life of the process. The sequence is
// single-use: ranging over one that has already run to completion (or whose
// body was closed some other way) yields a single read-on-closed-body error
// rather than a fresh empty stream.
func (c *Client) Stream(ctx context.Context, req wire.ChatRequest) (iter.Seq2[wire.StreamChunk, error], error) {
	if req.Model == "" {
		req.Model = c.model
	}
	req.Stream = true

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("gateway marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.bearerToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.bearerToken)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gateway request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		var env struct {
			Error *wire.Error `json:"error"`
		}
		if json.Unmarshal(raw, &env) == nil && env.Error != nil {
			return nil, fmt.Errorf("gateway status %d: %s: %s", resp.StatusCode, env.Error.Type, env.Error.Message)
		}
		return nil, fmt.Errorf("gateway status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	// A 200 that isn't actually SSE (e.g. the gateway fell back to a plain
	// JSON ChatResponse) would otherwise decode as an empty, error-free
	// stream: nothing after this point recognizes JSON-object framing, so
	// bufio.Scanner would just find no newline-delimited "data:" lines and
	// return cleanly. Reject it here, before the caller starts iterating,
	// naming what was actually received.
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		resp.Body.Close()
		return nil, fmt.Errorf("gateway stream: expected Content-Type text/event-stream, got %q", ct)
	}

	var closeOnce sync.Once
	closeBody := func() { closeOnce.Do(func() { resp.Body.Close() }) }

	// done is closed when the returned sequence finishes iterating (normally,
	// early break, or error). Until then, watch ctx independently of
	// iteration: if the caller discards the sequence without ever ranging
	// over it, the closure below never runs and its deferred close would
	// never fire, leaking the connection. Cancelling ctx is the only signal
	// available in that case, so treat it as a fallback close path. If the
	// sequence does get ranged over and finishes first, this goroutine exits
	// via done without waiting on ctx (which may be context.Background() and
	// never fire).
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			closeBody()
		case <-done:
		}
	}()

	return func(yield func(wire.StreamChunk, error) bool) {
		defer close(done)
		defer closeBody()
		sc := bufio.NewScanner(resp.Body)
		// A chunk carrying a large tool-call argument blob can exceed
		// bufio's 64KB default, which would surface as a truncated-line
		// error rather than the data it is.
		sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue // frame separator
			}
			// The space after the colon is optional per the SSE spec
			// (`data:{...}` is as valid as `data: {...}`) — match on
			// "data:" alone and trim whatever follows, the same form
			// provider/openai/openai.go's SSE reader uses, rather than
			// CutPrefix("data: ") which silently drops no-space frames.
			if !strings.HasPrefix(line, "data:") {
				continue // comments, event: lines, anything else SSE allows
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				return
			}
			// A mid-stream provider failure arrives as an OpenAI-shaped
			// {"error": {...}} envelope (streaming.Writer.SendError, used by
			// handlers_chat.go's sw.SendError(agentmodel.Wrap(err)) when the
			// upstream call fails after the response is already a 200), with
			// the same wire.Error shape the pre-stream non-200 branch above
			// parses. wire.StreamChunk has no error field, so decoding this
			// straight into a chunk would silently succeed with a zero-value
			// chunk and swallow the failure — check for the envelope first.
			var errEnv struct {
				Error *wire.Error `json:"error"`
			}
			if json.Unmarshal([]byte(payload), &errEnv) == nil && errEnv.Error != nil {
				yield(wire.StreamChunk{}, fmt.Errorf("gateway stream error: %s: %s", errEnv.Error.Type, errEnv.Error.Message))
				return
			}
			var chunk wire.StreamChunk
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				yield(wire.StreamChunk{}, fmt.Errorf("gateway decode chunk: %w", err))
				return
			}
			if !yield(chunk, nil) {
				return
			}
		}
		if err := sc.Err(); err != nil {
			yield(wire.StreamChunk{}, fmt.Errorf("gateway stream: %w", err))
		}
	}, nil
}
