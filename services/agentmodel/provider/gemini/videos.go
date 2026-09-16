package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
)

// compile-time assertions: Client implements the video interfaces.
var (
	_ provider.VideoGenerator  = (*Client)(nil)
	_ provider.VideoDownloader = (*Client)(nil)
)

// DownloadVideo fetches a Veo result asset with the gateway's provider
// credential (Veo URLs require x-goog-api-key), returning the byte stream and
// content type so the gateway can proxy it to a client that holds only the
// gateway bearer. The caller closes the returned reader.
func (c *Client) DownloadVideo(ctx context.Context, assetURL string) (io.ReadCloser, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("gemini: build download-video request: %w", err)
	}
	resp, err := c.send(ctx, req, "download-video")
	if err != nil {
		return nil, "", err
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "video/mp4"
	}
	return resp.Body, ct, nil
}

// Veo video generation via the Gemini API. Submission uses the long-running
// predictLongRunning method, which returns a google.longrunning.Operation; the
// poll is a plain GET on that operation name. The resulting video URI requires
// the gateway's Gemini credential to download (x-goog-api-key), so it is handed
// back as-is for now — proxying/persisting the bytes is a follow-up.

type veoInstance struct {
	Prompt string `json:"prompt,omitempty"`
}

type veoParameters struct {
	AspectRatio     string `json:"aspectRatio,omitempty"`
	Resolution      string `json:"resolution,omitempty"`
	NegativePrompt  string `json:"negativePrompt,omitempty"`
	DurationSeconds int    `json:"durationSeconds,omitempty"`
	NumberOfVideos  int    `json:"numberOfVideos,omitempty"`
}

type veoRequest struct {
	Instances  []veoInstance  `json:"instances"`
	Parameters *veoParameters `json:"parameters,omitempty"`
}

// veoOperation is the google.longrunning.Operation envelope returned by both
// :predictLongRunning (submit) and the GET poll.
type veoOperation struct {
	Name     string       `json:"name"`
	Done     bool         `json:"done"`
	Error    *veoError    `json:"error,omitempty"`
	Response *veoResponse `json:"response,omitempty"`
}

type veoError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type veoResponse struct {
	GenerateVideoResponse struct {
		GeneratedSamples []struct {
			Video struct {
				URI string `json:"uri"`
			} `json:"video"`
		} `json:"generatedSamples"`
	} `json:"generateVideoResponse"`
}

// SubmitVideo starts a Veo generation job and returns an operation handle whose
// ID is the provider operation name (the token PollVideo accepts).
// Implements provider.VideoGenerator.
func (c *Client) SubmitVideo(ctx context.Context, req agentmodel.GenerateVideoRequest) (agentmodel.VideoOperation, error) {
	if req.Model == "" {
		return agentmodel.VideoOperation{}, agentmodel.NewErrorf(agentmodel.ErrTypeInvalidRequest, "gemini: model required")
	}
	if req.Prompt == "" {
		return agentmodel.VideoOperation{}, agentmodel.NewErrorf(agentmodel.ErrTypeInvalidRequest, "gemini: prompt required")
	}

	body := veoRequest{Instances: []veoInstance{{Prompt: req.Prompt}}}
	if p := buildVeoParameters(req); p != nil {
		body.Parameters = p
	}

	path := fmt.Sprintf("/v1beta/models/%s:predictLongRunning", req.Model)
	httpResp, err := c.doJSON(ctx, path, body, "submit-video")
	if err != nil {
		return agentmodel.VideoOperation{}, err
	}
	defer httpResp.Body.Close()

	var op veoOperation
	if err := json.NewDecoder(httpResp.Body).Decode(&op); err != nil {
		return agentmodel.VideoOperation{}, fmt.Errorf("gemini: decode submit-video response: %w", err)
	}
	if op.Name == "" {
		return agentmodel.VideoOperation{}, agentmodel.NewErrorf(agentmodel.ErrTypeUpstream, "gemini: submit-video returned no operation name")
	}
	return veoToOperation(op, req.Model), nil
}

// PollVideo reports the current state of a previously submitted Veo operation,
// keyed by the provider operation name. Implements provider.VideoGenerator.
func (c *Client) PollVideo(ctx context.Context, providerOpID string) (agentmodel.VideoOperation, error) {
	if providerOpID == "" {
		return agentmodel.VideoOperation{}, agentmodel.NewErrorf(agentmodel.ErrTypeInvalidRequest, "gemini: operation id required")
	}
	httpResp, err := c.doGET(ctx, "/v1beta/"+strings.TrimPrefix(providerOpID, "/"), "poll-video")
	if err != nil {
		return agentmodel.VideoOperation{}, err
	}
	defer httpResp.Body.Close()

	var op veoOperation
	if err := json.NewDecoder(httpResp.Body).Decode(&op); err != nil {
		return agentmodel.VideoOperation{}, fmt.Errorf("gemini: decode poll-video response: %w", err)
	}
	return veoToOperation(op, ""), nil
}

// buildVeoParameters maps the gateway request's optional knobs onto Veo's
// parameters block, returning nil when none are set so the request omits it.
func buildVeoParameters(req agentmodel.GenerateVideoRequest) *veoParameters {
	p := veoParameters{
		AspectRatio:     req.AspectRatio,
		Resolution:      req.Resolution,
		NegativePrompt:  req.NegativePrompt,
		DurationSeconds: req.DurationSeconds,
		NumberOfVideos:  req.N,
	}
	if p == (veoParameters{}) {
		return nil
	}
	return &p
}

// veoToOperation maps the long-running-operation envelope onto the gateway's
// normalized VideoOperation. model is stamped on submit and left empty on poll
// (the router re-stamps the logical model from the gateway operation id).
func veoToOperation(op veoOperation, model string) agentmodel.VideoOperation {
	out := agentmodel.VideoOperation{
		ID:     op.Name,
		Object: agentmodel.VideoOperationObject,
		Model:  model,
	}
	switch {
	case op.Error != nil:
		out.Status = agentmodel.VideoStatusFailed
		out.Error = op.Error.Message
	case op.Done:
		out.Status = agentmodel.VideoStatusSucceeded
		if op.Response != nil {
			for _, s := range op.Response.GenerateVideoResponse.GeneratedSamples {
				if s.Video.URI != "" {
					out.Videos = append(out.Videos, agentmodel.VideoResult{URL: s.Video.URI, MimeType: "video/mp4"})
				}
			}
		}
	default:
		out.Status = agentmodel.VideoStatusRunning
	}
	return out
}

// doGET issues an authenticated GET against the Gemini API. The POST sibling
// (doJSON) lives in gemini.go; the long-running-operation poll is the first GET
// path beyond ListModels that the provider needs.
func (c *Client) doGET(ctx context.Context, path, op string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	return c.send(ctx, req, op)
}
