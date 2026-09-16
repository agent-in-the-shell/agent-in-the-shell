package openai

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
)

// compile-time assertion: Client implements provider.ImageGenerator.
var _ provider.ImageGenerator = (*Client)(nil)

// openaiImageRequest mirrors OpenAI's POST /images/generations body. Only the
// fields the gateway forwards are declared; omitempty keeps the request minimal
// so model-specific defaults (size/quality) apply when the caller omits them.
type openaiImageRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n,omitempty"`
	Size           string `json:"size,omitempty"`
	Quality        string `json:"quality,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
	User           string `json:"user,omitempty"`
}

// openaiImageResponse captures OpenAI's images response. gpt-image-1 returns a
// usage object with token counts; dall-e models omit it (decoded as zero).
type openaiImageResponse struct {
	Created int64 `json:"created"`
	Data    []struct {
		B64JSON       string `json:"b64_json,omitempty"`
		URL           string `json:"url,omitempty"`
		RevisedPrompt string `json:"revised_prompt,omitempty"`
	} `json:"data"`
	Usage *openaiImageUsage `json:"usage,omitempty"`
}

// openaiImageUsage is gpt-image-1's usage breakdown. We map input_tokens onto
// PromptTokens and output_tokens (the image tokens) onto CompletionTokens so the
// shared per-token cost path prices it without a bespoke image-cost branch.
type openaiImageUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// GenerateImage performs a text-to-image request against POST /images/generations.
// Implements provider.ImageGenerator.
func (c *Client) GenerateImage(ctx context.Context, req agentmodel.ImageRequest) (agentmodel.ImageResponse, error) {
	if req.Model == "" {
		return agentmodel.ImageResponse{}, agentmodel.NewErrorf(agentmodel.ErrTypeInvalidRequest, "openai: model required")
	}
	if req.Prompt == "" {
		return agentmodel.ImageResponse{}, agentmodel.NewErrorf(agentmodel.ErrTypeInvalidRequest, "openai: prompt required")
	}

	httpResp, err := c.postJSON(ctx, "/images/generations", openaiImageRequest{
		Model:          req.Model,
		Prompt:         req.Prompt,
		N:              req.N,
		Size:           req.Size,
		Quality:        req.Quality,
		ResponseFormat: req.ResponseFormat,
		User:           req.User,
	}, "images-generations")
	if err != nil {
		return agentmodel.ImageResponse{}, err
	}
	defer httpResp.Body.Close()

	var wire openaiImageResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&wire); err != nil {
		return agentmodel.ImageResponse{}, fmt.Errorf("openai: decode image response: %w", err)
	}

	out := agentmodel.ImageResponse{
		Created: wire.Created,
		Model:   req.Model,
		Data:    make([]agentmodel.ImageData, 0, len(wire.Data)),
	}
	for _, d := range wire.Data {
		out.Data = append(out.Data, agentmodel.ImageData{
			B64JSON:       d.B64JSON,
			URL:           d.URL,
			RevisedPrompt: d.RevisedPrompt,
		})
	}
	if wire.Usage != nil {
		out.Usage = agentmodel.Usage{
			PromptTokens:     wire.Usage.InputTokens,
			CompletionTokens: wire.Usage.OutputTokens,
			TotalTokens:      wire.Usage.TotalTokens,
		}
	}
	return out, nil
}
