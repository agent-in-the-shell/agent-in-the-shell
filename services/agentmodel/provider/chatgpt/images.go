// Image generation via the Responses backend's built-in image_generation
// tool. The ChatGPT subscription endpoint has no standalone images API (a
// Codex OAuth token is rejected by /v1/images/generations with a missing
// api.model.images.request scope), but the same Responses backend used for
// chat can drive the image_generation tool as an agent tool, reaching
// gpt-image-2-codex and billing to subscription quota.
package chatgpt

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
)

// compile-time assertion: Client implements provider.ImageGenerator.
var _ provider.ImageGenerator = (*Client)(nil)

// defaultImageGenModel is the model id the image_generation tool reports
// (response.tools[].model) as of issue 's POC. Used only as a fallback
// when that field is absent from the response — the resolved value from the
// wire response is always preferred.
const defaultImageGenModel = "gpt-image-2-codex"

// imageGenTool is the Responses API's built-in image_generation tool shape.
// Background is hardcoded to "auto" by the caller (no knob on the shared
// agentmodel.ImageRequest type); Size/Quality map directly from the request,
// omitted (letting the backend default to "auto") when unset.
//
// NOTE: the Codex backend rejects `tools[0].n` with 400 "Unknown parameter"
// (it always returns a single image), so N is deliberately NOT sent — even
// n:1 fails. size/quality/background are accepted. Verified against the live
// endpoint.
type imageGenTool struct {
	Type       string `json:"type"`
	Size       string `json:"size,omitempty"`
	Quality    string `json:"quality,omitempty"`
	Background string `json:"background,omitempty"`
}

// imageGenRequestBody is the POST /responses body for an image-generation
// call — the same wire shape as chat's responsesRequest, but with a single
// fixed user message and the image_generation tool instead of chat's
// function tools. Store is not omitempty: the backend expects an explicit
// false, matching the POC body exactly.
type imageGenRequestBody struct {
	Model        string             `json:"model"`
	Instructions string             `json:"instructions"`
	Input        []responsesMessage `json:"input"`
	Tools        []imageGenTool     `json:"tools"`
	ToolChoice   string             `json:"tool_choice"`
	Stream       bool               `json:"stream"`
	Store        bool               `json:"store"`
}

// marshalImageRequest builds and encodes the Responses API request body for
// an image-generation call.
func marshalImageRequest(req agentmodel.ImageRequest) ([]byte, error) {
	body, err := json.Marshal(imageGenRequestBody{
		Model:        req.Model,
		Instructions: "You are a helpful assistant.",
		Input: []responsesMessage{{
			Role: "user",
			Content: []responsesContentPart{{
				Type: "input_text",
				Text: "Use the image_generation tool to generate an image: " + req.Prompt,
			}},
		}},
		Tools: []imageGenTool{{
			Type:       "image_generation",
			Size:       req.Size,
			Quality:    req.Quality,
			Background: "auto",
		}},
		ToolChoice: "auto",
		Stream:     true,
		Store:      false,
	})
	if err != nil {
		return nil, fmt.Errorf("chatgpt: marshal image request: %w", err)
	}
	return body, nil
}

// imageOutputItem is one entry in response.completed's response.output[].
// Only the image_generation_call shape is consumed; other item types (e.g.
// a text message the model produced alongside/instead of the tool call) are
// present in the same array but ignored here.
type imageOutputItem struct {
	Type   string `json:"type"`
	Result string `json:"result"`
}

// imageCompletedEvent is the response.completed payload for an image
// generation call. Tools echoes back the resolved tool config (including
// the concrete model, e.g. "gpt-image-2-codex") the backend actually used.
type imageCompletedEvent struct {
	Response struct {
		Output []imageOutputItem `json:"output"`
		Usage  responsesUsage    `json:"usage"`
		Tools  []struct {
			Model string `json:"model"`
		} `json:"tools"`
	} `json:"response"`
}

// imageItemDoneEvent is the response.output_item.done payload. The Codex
// backend delivers the finished image_generation_call — including its base64
// `result` — in THIS event; response.completed's output[] arrives empty. So
// this is the event that actually carries the image, and it must be captured.
type imageItemDoneEvent struct {
	Item struct {
		Type   string `json:"type"`
		Result string `json:"result"`
	} `json:"item"`
}

// readImageStream scans one SSE response from the Responses backend for an
// image-generation result. The base64 image arrives in a
// response.output_item.done event (image_generation_call with .result);
// response.completed is terminal and carries usage/model but an empty output[].
// The in-progress/generating/partial_image events carry nothing this
// synchronous, single-image caller needs.
func readImageStream(httpResp *http.Response) (agentmodel.ImageResponse, error) {
	defer httpResp.Body.Close()
	scanner := bufio.NewScanner(httpResp.Body)
	// A base64-encoded PNG arrives as a single SSE data: line inside
	// response.completed (chat's smaller 1MB cap only ever holds short text
	// deltas), so the max token size here needs enough headroom for a
	// multi-MB image plus base64's ~33% inflation.
	scanner.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)

	var eventType string
	// dataBuf accumulates one event's data: line(s). A base64-encoded PNG can
	// be multi-MB, so this uses bytes.Buffer (not strings.Builder) and scans
	// with scanner.Bytes() rather than scanner.Text(): both avoid an extra
	// full-payload copy that scanner.Text() plus a string->[]byte round trip
	// for json.Unmarshal would otherwise incur on every large image.
	var dataBuf bytes.Buffer

	// pending holds image(s) captured from response.output_item.done events,
	// which is where the Codex backend actually delivers the base64 result.
	// response.completed then terminates the stream and supplies usage/model.
	var pending []agentmodel.ImageData

	// dispatch handles one complete SSE event. done reports whether it was a
	// terminal event (response.completed or a failure); when done is false
	// the scan continues.
	dispatch := func() (out agentmodel.ImageResponse, done bool, err error) {
		if dataBuf.Len() == 0 {
			eventType = ""
			return agentmodel.ImageResponse{}, false, nil
		}
		// data aliases dataBuf's backing array; safe to read until the next
		// Write (the next Scan() iteration, after this call returns).
		data := dataBuf.Bytes()
		dataBuf.Reset()
		et := eventType
		eventType = ""
		if et == "" {
			var probe struct {
				Type string `json:"type"`
			}
			if jsonErr := json.Unmarshal(data, &probe); jsonErr == nil {
				et = probe.Type
			}
		}

		switch et {
		case "response.completed":
			var ev imageCompletedEvent
			if jsonErr := json.Unmarshal(data, &ev); jsonErr != nil {
				return agentmodel.ImageResponse{}, true, fmt.Errorf("chatgpt: decode image completed: %w", jsonErr)
			}
			images := make([]agentmodel.ImageData, 0, len(ev.Response.Output))
			for _, item := range ev.Response.Output {
				if item.Type == "image_generation_call" && item.Result != "" {
					images = append(images, agentmodel.ImageData{B64JSON: item.Result})
				}
			}
			// The Codex backend leaves response.completed.output[] empty and
			// delivers the image in an earlier response.output_item.done event,
			// captured into pending. Fall back to it.
			if len(images) == 0 {
				images = pending
			}
			if len(images) == 0 {
				return agentmodel.ImageResponse{}, true, agentmodel.NewErrorf(agentmodel.ErrTypeUpstream, "chatgpt: response contained no image data")
			}
			model := defaultImageGenModel
			if len(ev.Response.Tools) > 0 && ev.Response.Tools[0].Model != "" {
				model = ev.Response.Tools[0].Model
			}
			return agentmodel.ImageResponse{
				Model: model,
				Data:  images,
				Usage: agentmodel.Usage{
					PromptTokens:         ev.Response.Usage.InputTokens,
					CompletionTokens:     ev.Response.Usage.OutputTokens,
					TotalTokens:          ev.Response.Usage.TotalTokens,
					ReasoningTokens:      ev.Response.Usage.OutputTokensDetails.ReasoningTokens,
					CacheReadInputTokens: ev.Response.Usage.cachedTokens(),
					AuthMode:             agentmodel.AuthModeSubscription,
				},
			}, true, nil

		case "response.output_item.done":
			var ev imageItemDoneEvent
			if jsonErr := json.Unmarshal(data, &ev); jsonErr == nil &&
				ev.Item.Type == "image_generation_call" && ev.Item.Result != "" {
				pending = append(pending, agentmodel.ImageData{B64JSON: ev.Item.Result})
			}
			return agentmodel.ImageResponse{}, false, nil

		case "error", "response.failed", "response.incomplete":
			return agentmodel.ImageResponse{}, true, fmt.Errorf("chatgpt: stream %s: %s", et, summarizeStreamFailure(string(data)))

		default:
			return agentmodel.ImageResponse{}, false, nil
		}
	}

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			if out, done, err := dispatch(); done {
				return out, err
			}
			continue
		}
		switch {
		case bytes.HasPrefix(line, []byte("event:")):
			eventType = strings.TrimSpace(string(bytes.TrimPrefix(line, []byte("event:"))))
		case bytes.HasPrefix(line, []byte("data:")):
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.Write(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))))
		}
	}
	if dataBuf.Len() > 0 {
		if out, done, err := dispatch(); done {
			return out, err
		}
	}
	if rerr := scanner.Err(); rerr != nil {
		return agentmodel.ImageResponse{}, fmt.Errorf("chatgpt: image stream read: %w", rerr)
	}
	return agentmodel.ImageResponse{}, fmt.Errorf("chatgpt: image stream ended without response.completed")
}

// GenerateImage performs a text-to-image request via the Responses backend's
// image_generation tool. Implements provider.ImageGenerator.
func (c *Client) GenerateImage(ctx context.Context, req agentmodel.ImageRequest) (agentmodel.ImageResponse, error) {
	if req.Model == "" {
		return agentmodel.ImageResponse{}, agentmodel.NewErrorf(agentmodel.ErrTypeInvalidRequest, "chatgpt: model required")
	}
	if req.Prompt == "" {
		return agentmodel.ImageResponse{}, agentmodel.NewErrorf(agentmodel.ErrTypeInvalidRequest, "chatgpt: prompt required")
	}

	body, err := marshalImageRequest(req)
	if err != nil {
		return agentmodel.ImageResponse{}, err
	}

	// Image generation is a one-shot request with no conversational prefix to
	// cache, so it sends no session-id (session stickiness is for chat turns).
	httpResp, err := c.doStream(ctx, body, "")
	if err != nil && c.recoverExpiredToken(ctx, err) {
		httpResp, err = c.doStream(ctx, body, "")
	}
	if err != nil {
		return agentmodel.ImageResponse{}, err
	}

	return readImageStream(httpResp)
}
