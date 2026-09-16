package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
)

// compile-time assertion: Client implements provider.ImageGenerator.
var _ provider.ImageGenerator = (*Client)(nil)

// GenerateImage performs a text-to-image request. Gemini ("nano banana", e.g.
// gemini-2.5-flash-image) has no dedicated images endpoint: image output comes
// back inline from the normal generateContent call when an IMAGE response
// modality is requested. This method drives that path and extracts the inline
// image parts into the OpenAI-shaped ImageResponse, so callers see the same
// contract as the OpenAI provider. Implements provider.ImageGenerator.
//
// Gemini returns images as base64 inline data only (no hosted URL), so the
// response is always b64_json regardless of req.ResponseFormat.
func (c *Client) GenerateImage(ctx context.Context, req agentmodel.ImageRequest) (agentmodel.ImageResponse, error) {
	if req.Model == "" {
		return agentmodel.ImageResponse{}, agentmodel.NewErrorf(agentmodel.ErrTypeInvalidRequest, "gemini: model required")
	}
	if req.Prompt == "" {
		return agentmodel.ImageResponse{}, agentmodel.NewErrorf(agentmodel.ErrTypeInvalidRequest, "gemini: prompt required")
	}

	greq := geminiRequest{
		Contents: []geminiContent{{
			Role:  "user",
			Parts: []geminiPart{{Text: req.Prompt}},
		}},
		GenerationConfig: &geminiGenConfig{
			ResponseModalities: []string{"IMAGE"},
		},
	}
	if req.N > 1 {
		n := req.N
		greq.GenerationConfig.CandidateCount = &n
	}

	path := fmt.Sprintf("/v1beta/models/%s:generateContent", req.Model)
	httpResp, err := c.doJSON(ctx, path, greq, "generate-image")
	if err != nil {
		return agentmodel.ImageResponse{}, err
	}
	defer httpResp.Body.Close()

	var gr geminiResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&gr); err != nil {
		return agentmodel.ImageResponse{}, fmt.Errorf("gemini: decode image response: %w", err)
	}

	out := agentmodel.ImageResponse{
		Created: time.Now().Unix(),
		Model:   req.Model,
		Data:    make([]agentmodel.ImageData, 0, len(gr.Candidates)),
	}
	for _, cand := range gr.Candidates {
		for _, p := range cand.Content.Parts {
			if p.InlineData != nil && p.InlineData.Data != "" {
				out.Data = append(out.Data, agentmodel.ImageData{B64JSON: p.InlineData.Data})
			}
		}
	}
	if len(out.Data) == 0 {
		return agentmodel.ImageResponse{}, agentmodel.NewErrorf(agentmodel.ErrTypeUpstream, "gemini: response contained no image data")
	}
	if gr.UsageMetadata != nil {
		out.Usage = gr.UsageMetadata.toUsage(c.AuthMode())
	}
	return out, nil
}
