// Package gemini implements the agentmodel Provider for Google AI Studio
// (generativelanguage.googleapis.com / v1beta).
//
// Auth is via API key in the "x-goog-api-key" header. The model name is
// embedded directly in the URL path (e.g. /v1beta/models/{model}:generateContent).
//
// This client deliberately uses only the Go standard library — no
// google/generative-ai-go dependency.
package gemini

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
)

const defaultBaseURL = "https://generativelanguage.googleapis.com"

// Client is the Google Gemini provider implementation.
type Client struct {
	auth    auth.Authenticator
	baseURL string
	http    *http.Client
}

// New constructs a Client targeting the public Google AI Studio endpoint.
func New(authenticator auth.Authenticator) *Client {
	return NewWithBaseURL(authenticator, defaultBaseURL)
}

// NewWithBaseURL constructs a Client targeting baseURL (useful for tests).
// baseURL should NOT include the /v1beta path; the client appends it.
func NewWithBaseURL(authenticator auth.Authenticator, baseURL string) *Client {
	return &Client{
		auth:    authenticator,
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 5 * time.Minute},
	}
}

// Name returns the canonical provider identifier.
func (c *Client) Name() string { return "gemini" }

// AuthMode reports the credential mode in use.
func (c *Client) AuthMode() string {
	if c.auth == nil {
		return agentmodel.AuthModeAPIKey
	}
	return c.auth.Mode()
}

// SupportedModels returns the model identifiers this provider can dispatch.
func (c *Client) SupportedModels() []string {
	return []string{
		"gemini-2.5-pro",
		"gemini-2.5-flash",
		"text-embedding-004",
		"veo-3.1-generate-preview",
		"veo-3.1-fast-generate-preview",
	}
}

// ----- Wire types (subset of GenerateContent API) -----

type geminiPart struct {
	Text         string            `json:"text,omitempty"`
	FunctionCall *geminiFuncCall   `json:"functionCall,omitempty"`
	FunctionResp *geminiFuncResp   `json:"functionResponse,omitempty"`
	InlineData   *geminiInlineData `json:"inlineData,omitempty"`
}

// geminiInlineData carries base64-encoded binary content. For image generation
// the model returns generated images as inlineData parts (mimeType image/png
// etc.) interleaved with any text parts.
type geminiInlineData struct {
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"` // base64-encoded bytes
}

type geminiFuncCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

type geminiFuncResp struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFuncDecl `json:"functionDeclarations,omitempty"`
}

type geminiFuncDecl struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type geminiGenConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
	// ResponseModalities selects the output modalities for a generateContent
	// call. Image generation requires "IMAGE" (e.g. ["IMAGE"] or
	// ["TEXT","IMAGE"]); the chat path leaves it empty for text-only output.
	ResponseModalities []string `json:"responseModalities,omitempty"`
	// CandidateCount requests N independent candidates (best-effort; image
	// models commonly cap at 1).
	CandidateCount *int `json:"candidateCount,omitempty"`
}

type geminiRequest struct {
	Contents          []geminiContent  `json:"contents"`
	SystemInstruction *geminiContent   `json:"systemInstruction,omitempty"`
	Tools             []geminiTool     `json:"tools,omitempty"`
	GenerationConfig  *geminiGenConfig `json:"generationConfig,omitempty"`
}

type geminiCandidate struct {
	Content      geminiContent `json:"content"`
	FinishReason string        `json:"finishReason,omitempty"`
	Index        int           `json:"index,omitempty"`
}

type geminiUsage struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
	// CachedContentTokenCount is the portion of promptTokenCount served from
	// Gemini's context cache (implicit or explicit). Unlike Anthropic's
	// input_tokens, promptTokenCount already includes these, so we surface them
	// as CacheReadInputTokens without adjusting PromptTokens — matching the cost
	// calculator's uncached = PromptTokens - CacheRead - CacheCreation.
	CachedContentTokenCount int `json:"cachedContentTokenCount,omitempty"`
}

// toUsage maps Gemini's usage metadata onto the canonical agentmodel.Usage. It
// is the single conversion point for both the non-streaming response and the
// streaming usage chunks, so a new field is wired in exactly once.
func (u *geminiUsage) toUsage(authMode string) agentmodel.Usage {
	return agentmodel.Usage{
		PromptTokens:         u.PromptTokenCount,
		CompletionTokens:     u.CandidatesTokenCount,
		TotalTokens:          u.TotalTokenCount,
		CacheReadInputTokens: u.CachedContentTokenCount,
		AuthMode:             authMode,
	}
}

type geminiResponse struct {
	Candidates     []geminiCandidate     `json:"candidates"`
	UsageMetadata  *geminiUsage          `json:"usageMetadata,omitempty"`
	PromptFeedback *geminiPromptFeedback `json:"promptFeedback,omitempty"`
}

// geminiPromptFeedback carries a prompt-level safety block: Gemini returns zero
// candidates and a blockReason instead of a candidate with a SAFETY finish.
type geminiPromptFeedback struct {
	BlockReason string `json:"blockReason"`
}

// promptBlockReason returns the prompt-level safety block reason, or "" if the
// response is not prompt-blocked. Shared by Complete and Stream so both surface
// a blocked prompt as an error instead of a silent, signal-less empty success.
func (r *geminiResponse) promptBlockReason() string {
	if len(r.Candidates) == 0 && r.PromptFeedback != nil {
		return r.PromptFeedback.BlockReason
	}
	return ""
}

// promptBlockedError builds the typed content-filter error for a blocked prompt.
// Response-level SAFETY finishes still flow through mapFinishReason on a
// candidate; this is only the zero-candidate prompt-block case.
func promptBlockedError(reason string) error {
	return agentmodel.NewErrorCode(agentmodel.ErrTypeContentFilter, agentmodel.CodeContentPolicy,
		fmt.Sprintf("gemini: prompt blocked by safety filter: reason=%s", reason))
}

type geminiEmbedRequest struct {
	Content geminiContent `json:"content"`
}

type geminiEmbedResponse struct {
	Embedding struct {
		Values []float64 `json:"values"`
	} `json:"embedding"`
}

// ----- Conversion helpers -----

// buildRequest translates an OpenAI-shaped ChatRequest into a Gemini request,
// pulling system messages into systemInstruction.
func buildRequest(req agentmodel.ChatRequest) geminiRequest {
	out := geminiRequest{}

	var sysParts []string
	for _, m := range req.Messages {
		if m.Role == "system" && m.Content != "" {
			sysParts = append(sysParts, m.Content)
			continue
		}
		out.Contents = append(out.Contents, messageToContent(m))
	}
	if len(sysParts) > 0 {
		out.SystemInstruction = &geminiContent{
			Parts: []geminiPart{{Text: strings.Join(sysParts, "\n\n")}},
		}
	}

	if len(req.Tools) > 0 {
		decls := make([]geminiFuncDecl, 0, len(req.Tools))
		for _, t := range req.Tools {
			decls = append(decls, geminiFuncDecl{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  t.Function.Parameters,
			})
		}
		out.Tools = []geminiTool{{FunctionDeclarations: decls}}
	}

	if req.Temperature != nil || req.MaxTokens != nil || req.TopP != nil || len(req.Stop) > 0 {
		out.GenerationConfig = &geminiGenConfig{
			Temperature:     req.Temperature,
			MaxOutputTokens: req.MaxTokens,
			TopP:            req.TopP,
			StopSequences:   req.Stop,
		}
	}

	return out
}

// messageToContent converts a single OpenAI message into a Gemini content entry.
// Roles map: user -> user, assistant -> model, tool -> user (with functionResponse).
func messageToContent(m agentmodel.Message) geminiContent {
	switch m.Role {
	case "assistant":
		c := geminiContent{Role: "model"}
		if m.Content != "" {
			c.Parts = append(c.Parts, geminiPart{Text: m.Content})
		}
		for _, tc := range m.ToolCalls {
			args := map[string]any{}
			if tc.Function.Arguments != "" {
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
			}
			c.Parts = append(c.Parts, geminiPart{
				FunctionCall: &geminiFuncCall{Name: tc.Function.Name, Args: args},
			})
		}
		if len(c.Parts) == 0 {
			c.Parts = []geminiPart{{Text: ""}}
		}
		return c
	case "tool":
		// Tool results: Gemini uses role "user" with a functionResponse part.
		var payload map[string]any
		if m.Content != "" {
			if err := json.Unmarshal([]byte(m.Content), &payload); err != nil {
				payload = map[string]any{"content": m.Content}
			}
		}
		return geminiContent{
			Role: "user",
			Parts: []geminiPart{{
				FunctionResp: &geminiFuncResp{Name: m.Name, Response: payload},
			}},
		}
	default:
		return geminiContent{
			Role:  "user",
			Parts: []geminiPart{{Text: m.Content}},
		}
	}
}

// mapFinishReason translates Gemini finish reasons to OpenAI-shaped strings.
func mapFinishReason(r string) string {
	switch r {
	case "STOP", "RECITATION", "OTHER", "":
		if r == "" {
			return ""
		}
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY":
		return "content_filter"
	default:
		return "stop"
	}
}

// candidateToMessage extracts the assistant message + tool calls from a Gemini
// candidate.
func candidateToMessage(cand geminiCandidate, idCounter *int) agentmodel.Message {
	out := agentmodel.Message{Role: "assistant"}
	var sb strings.Builder
	for _, p := range cand.Content.Parts {
		if p.Text != "" {
			sb.WriteString(p.Text)
		}
		if p.FunctionCall != nil {
			argBytes, _ := json.Marshal(p.FunctionCall.Args)
			if argBytes == nil {
				argBytes = []byte("{}")
			}
			*idCounter++
			out.ToolCalls = append(out.ToolCalls, agentmodel.ToolCall{
				ID:   fmt.Sprintf("call_%d", *idCounter),
				Type: "function",
				Function: agentmodel.ToolCallFunction{
					Name:      p.FunctionCall.Name,
					Arguments: string(argBytes),
				},
			})
		}
	}
	out.Content = sb.String()
	return out
}

// ----- HTTP helpers -----

var _ provider.ModelLister = (*Client)(nil)

// ListModels returns the model ids Gemini currently offers
// (GET /v1beta/models). The upstream prefixes names with "models/"; that
// prefix is stripped to match the bare model id used in deployments.
// Implements provider.ModelLister.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1beta/models?pageSize=1000", nil)
	if err != nil {
		return nil, fmt.Errorf("gemini: build list-models request: %w", err)
	}
	if c.auth != nil {
		if err := c.auth.Apply(ctx, req); err != nil {
			return nil, err
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gemini: do list-models request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("gemini: list-models HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("gemini: decode list-models response: %w", err)
	}
	ids := make([]string, 0, len(out.Models))
	for _, m := range out.Models {
		ids = append(ids, strings.TrimPrefix(m.Name, "models/"))
	}
	return ids, nil
}

func (c *Client) doJSON(ctx context.Context, path string, body any) (*http.Response, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.auth != nil {
		if err := c.auth.Apply(ctx, req); err != nil {
			return nil, err
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("gemini: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp, nil
}

// ----- Provider methods -----

// Complete executes a non-streaming generateContent call.
func (c *Client) Complete(ctx context.Context, req agentmodel.ChatRequest) (agentmodel.ChatResponse, error) {
	if err := agentmodel.ValidateTextOnlyContent(req); err != nil {
		return agentmodel.ChatResponse{}, err
	}
	if req.Model == "" {
		return agentmodel.ChatResponse{}, fmt.Errorf("gemini: Model required")
	}
	body := buildRequest(req)
	path := fmt.Sprintf("/v1beta/models/%s:generateContent", req.Model)

	httpResp, err := c.doJSON(ctx, path, body)
	if err != nil {
		return agentmodel.ChatResponse{}, err
	}
	defer httpResp.Body.Close()

	var gr geminiResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&gr); err != nil {
		return agentmodel.ChatResponse{}, fmt.Errorf("gemini: decode response: %w", err)
	}

	if reason := gr.promptBlockReason(); reason != "" {
		return agentmodel.ChatResponse{}, promptBlockedError(reason)
	}

	out := agentmodel.ChatResponse{
		ID:      fmt.Sprintf("gemini-%d", time.Now().UnixNano()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
	}
	idCounter := 0
	for i, cand := range gr.Candidates {
		out.Choices = append(out.Choices, agentmodel.Choice{
			Index:        i,
			Message:      candidateToMessage(cand, &idCounter),
			FinishReason: mapFinishReason(cand.FinishReason),
		})
	}
	if gr.UsageMetadata != nil {
		out.Usage = gr.UsageMetadata.toUsage(c.AuthMode())
	}
	return out, nil
}

// Stream executes a streaming streamGenerateContent call (alt=sse) and returns
// an iterator yielding StreamChunks. Each upstream chunk is a full
// GenerateContentResponse where the text content is cumulative; this client
// computes per-chunk deltas client-side.
func (c *Client) Stream(ctx context.Context, req agentmodel.ChatRequest) (iter.Seq2[provider.StreamChunk, error], error) {
	if err := agentmodel.ValidateTextOnlyContent(req); err != nil {
		return nil, err
	}
	if req.Model == "" {
		return nil, fmt.Errorf("gemini: Model required")
	}
	body := buildRequest(req)
	path := fmt.Sprintf("/v1beta/models/%s:streamGenerateContent?alt=sse", req.Model)

	httpResp, err := c.doJSON(ctx, path, body)
	if err != nil {
		return nil, err
	}

	authMode := c.AuthMode()

	seq := func(yield func(provider.StreamChunk, error) bool) {
		defer httpResp.Body.Close()

		scanner := bufio.NewScanner(httpResp.Body)
		// Allow large lines for long completions.
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

		var prevText string
		idCounter := 0
		emittedRole := false

		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" || payload == "[DONE]" {
				continue
			}

			var gr geminiResponse
			if err := json.Unmarshal([]byte(payload), &gr); err != nil {
				if !yield(provider.StreamChunk{}, fmt.Errorf("gemini: decode SSE: %w", err)) {
					return
				}
				continue
			}

			if len(gr.Candidates) == 0 {
				// A prompt-level safety block arrives as zero candidates + a
				// blockReason; surface it rather than swallowing it into a
				// truncated success (#1487).
				if reason := gr.promptBlockReason(); reason != "" {
					yield(provider.StreamChunk{}, promptBlockedError(reason))
					return
				}
				// Otherwise a final chunk with usage only.
				if gr.UsageMetadata != nil {
					usage := gr.UsageMetadata.toUsage(authMode)
					if !yield(provider.StreamChunk{Usage: &usage}, nil) {
						return
					}
				}
				continue
			}

			cand := gr.Candidates[0]
			var curText strings.Builder
			var newToolCalls []agentmodel.ToolCall
			for _, p := range cand.Content.Parts {
				if p.Text != "" {
					curText.WriteString(p.Text)
				}
				if p.FunctionCall != nil {
					argBytes, _ := json.Marshal(p.FunctionCall.Args)
					if argBytes == nil {
						argBytes = []byte("{}")
					}
					idCounter++
					newToolCalls = append(newToolCalls, agentmodel.ToolCall{
						ID:   fmt.Sprintf("call_%d", idCounter),
						Type: "function",
						Function: agentmodel.ToolCallFunction{
							Name:      p.FunctionCall.Name,
							Arguments: string(argBytes),
						},
					})
				}
			}

			full := curText.String()
			delta := strings.TrimPrefix(full, prevText)
			prevText = full

			chunk := provider.StreamChunk{}
			if !emittedRole {
				chunk.Delta.Role = "assistant"
				emittedRole = true
			}
			chunk.Delta.Content = delta
			chunk.Delta.ToolCalls = newToolCalls
			chunk.FinishReason = mapFinishReason(cand.FinishReason)
			if gr.UsageMetadata != nil {
				usage := gr.UsageMetadata.toUsage(authMode)
				chunk.Usage = &usage
			}

			if !yield(chunk, nil) {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			yield(provider.StreamChunk{}, fmt.Errorf("gemini: stream read: %w", err))
		}
	}

	return seq, nil
}

// Embed calls embedContent once per input string. Gemini's single-input
// endpoint is the simplest path; batchEmbedContents is an optimization left
// for a follow-up.
func (c *Client) Embed(ctx context.Context, req agentmodel.EmbeddingRequest) (agentmodel.EmbeddingResponse, error) {
	if req.Model == "" {
		return agentmodel.EmbeddingResponse{}, fmt.Errorf("gemini: Model required")
	}
	out := agentmodel.EmbeddingResponse{
		Object: "list",
		Model:  req.Model,
		Data:   make([]agentmodel.Embedding, 0, len(req.Input)),
	}

	path := fmt.Sprintf("/v1beta/models/%s:embedContent", req.Model)

	for i, text := range req.Input {
		body := geminiEmbedRequest{
			Content: geminiContent{Parts: []geminiPart{{Text: text}}},
		}
		httpResp, err := c.doJSON(ctx, path, body)
		if err != nil {
			return agentmodel.EmbeddingResponse{}, err
		}
		var er geminiEmbedResponse
		if err := json.NewDecoder(httpResp.Body).Decode(&er); err != nil {
			_ = httpResp.Body.Close()
			return agentmodel.EmbeddingResponse{}, fmt.Errorf("gemini: decode embedding: %w", err)
		}
		_ = httpResp.Body.Close()
		out.Data = append(out.Data, agentmodel.Embedding{
			Object:    "embedding",
			Index:     i,
			Embedding: er.Embedding.Values,
		})
	}

	out.Usage = agentmodel.Usage{AuthMode: c.AuthMode()}
	return out, nil
}

// compile-time assertion: Client implements provider.Provider.
var _ provider.Provider = (*Client)(nil)
