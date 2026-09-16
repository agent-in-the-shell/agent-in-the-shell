package router

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
)

// Video generation dispatch. Video is the gateway's first async
// modality: GenerateVideo submits a job and returns an opaque gateway operation
// id; PollVideo resolves that id back to the owning deployment and reports
// status. The router holds no operation state — the id encodes (logical model,
// provider, provider operation id), and the provider (e.g. Veo) holds the job —
// so a poll survives a gateway restart without any operation store.

// videoOpRef is the decoded payload of a gateway video operation id.
type videoOpRef struct {
	Model      string `json:"m"` // logical model_name the submission routed under
	Provider   string `json:"p"` // provider Name() that owns the operation
	ProviderOp string `json:"o"` // the provider's own operation id
}

const videoOpPrefix = "vidop_"

func encodeVideoOpID(ref videoOpRef) string {
	b, _ := json.Marshal(ref)
	return videoOpPrefix + base64.RawURLEncoding.EncodeToString(b)
}

func decodeVideoOpID(id string) (videoOpRef, error) {
	if !strings.HasPrefix(id, videoOpPrefix) {
		return videoOpRef{}, fmt.Errorf("router: malformed video operation id")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(id, videoOpPrefix))
	if err != nil {
		return videoOpRef{}, fmt.Errorf("router: malformed video operation id: %w", err)
	}
	var ref videoOpRef
	if err := json.Unmarshal(raw, &ref); err != nil {
		return videoOpRef{}, fmt.Errorf("router: malformed video operation id: %w", err)
	}
	if ref.Model == "" || ref.Provider == "" || ref.ProviderOp == "" {
		return videoOpRef{}, fmt.Errorf("router: incomplete video operation id")
	}
	return ref, nil
}

// GenerateVideo submits an async video-generation job to the first
// video-capable deployment of req.Model. Like Embed/GenerateImage there is no
// cross-provider fallback (video models aren't interchangeable); deployments
// whose provider doesn't implement provider.VideoGenerator are skipped, and a
// model with no video-capable deployment fails with invalid_request rather than
// a generic "all failed". On success the returned operation's ID is a
// gateway-issued token PollVideo can resolve back to this deployment.
func (r *Router) GenerateVideo(ctx context.Context, req agentmodel.GenerateVideoRequest) (agentmodel.VideoOperation, error) {
	candidates := r.deployments[req.Model]
	if len(candidates) == 0 {
		return agentmodel.VideoOperation{}, ErrNoDeployment
	}
	ordered := r.weightedShuffle(candidates)
	capable := false
	hitRateLimit := false
	for _, dep := range ordered {
		gen, ok := dep.Provider.(provider.VideoGenerator)
		if !ok {
			continue // this deployment's provider can't generate video
		}
		capable = true
		if !r.acquireRateSlot(req.Model, dep) {
			hitRateLimit = true // at its configured rpm/tpm cap this minute
			continue
		}
		subReq := req
		subReq.Model = dep.Model
		op, err := gen.SubmitVideo(ctx, subReq)
		if err == nil {
			op.ID = encodeVideoOpID(videoOpRef{
				Model:      req.Model,
				Provider:   dep.Provider.Name(),
				ProviderOp: op.ID,
			})
			op.Model = req.Model
			if op.Object == "" {
				op.Object = agentmodel.VideoOperationObject
			}
			r.obsResult(req.Model, dep.Name, dep.Provider.Name(), true)
			return op, nil
		}
		ae := agentmodel.Wrap(err)
		r.obsResult(req.Model, dep.Name, dep.Provider.Name(), false)
		if !ae.Retryable() {
			return agentmodel.VideoOperation{}, ae
		}
		hitRateLimit = true
	}
	if !capable {
		return agentmodel.VideoOperation{}, agentmodel.NewErrorf(agentmodel.ErrTypeInvalidRequest,
			"model %q has no video-capable deployment", req.Model)
	}
	if hitRateLimit {
		return agentmodel.VideoOperation{}, errAllRateLimited(req.Model)
	}
	return agentmodel.VideoOperation{}, ErrAllFailed
}

// PollVideo resolves a gateway video operation id to the deployment that owns
// it and returns the job's current status. Stateless: the id carries the owning
// (model, provider, provider-op), so no operation store is consulted. The
// returned operation re-exposes the gateway id so a client keeps polling the
// same token.
// resolveVideoDeployment finds the VideoGenerator that owns a decoded op ref
// (the provider a poll/download must re-dispatch to). Shared by PollVideo and
// DownloadVideo so the resolution and its not-found error stay in one place.
func (r *Router) resolveVideoDeployment(ref videoOpRef) (provider.VideoGenerator, error) {
	for _, dep := range r.deployments[ref.Model] {
		if dep.Provider == nil || dep.Provider.Name() != ref.Provider {
			continue
		}
		if gen, ok := dep.Provider.(provider.VideoGenerator); ok {
			return gen, nil
		}
	}
	return nil, agentmodel.NewErrorf(agentmodel.ErrTypeNotFound,
		"no video-capable deployment for operation (model %q, provider %q)", ref.Model, ref.Provider)
}

func (r *Router) PollVideo(ctx context.Context, gatewayOpID string) (agentmodel.VideoOperation, error) {
	ref, err := decodeVideoOpID(gatewayOpID)
	if err != nil {
		return agentmodel.VideoOperation{}, agentmodel.NewError(agentmodel.ErrTypeInvalidRequest, err.Error())
	}
	gen, err := r.resolveVideoDeployment(ref)
	if err != nil {
		return agentmodel.VideoOperation{}, err
	}
	op, err := gen.PollVideo(ctx, ref.ProviderOp)
	if err != nil {
		return agentmodel.VideoOperation{}, agentmodel.Wrap(err)
	}
	op.ID = gatewayOpID
	op.Model = ref.Model
	if op.Object == "" {
		op.Object = agentmodel.VideoOperationObject
	}
	return op, nil
}

// VideoProxyable reports whether the operation's owning provider needs the
// gateway to proxy its result bytes (implements provider.VideoDownloader). The
// handler rewrites result URLs to gateway /content URLs only when true; a
// provider with public asset URLs is passed through. Malformed/unknown ids are
// false (nothing to rewrite).
func (r *Router) VideoProxyable(gatewayOpID string) bool {
	ref, err := decodeVideoOpID(gatewayOpID)
	if err != nil {
		return false
	}
	gen, err := r.resolveVideoDeployment(ref)
	if err != nil {
		return false
	}
	_, ok := gen.(provider.VideoDownloader)
	return ok
}

// DownloadVideo streams the bytes of the index-th result video for a succeeded
// operation, fetched with the owning provider's credential. It re-polls to get
// a fresh asset URL (provider URLs can expire) and requires the provider to
// implement provider.VideoDownloader. The caller closes the returned reader.
func (r *Router) DownloadVideo(ctx context.Context, gatewayOpID string, index int) (io.ReadCloser, string, error) {
	ref, err := decodeVideoOpID(gatewayOpID)
	if err != nil {
		return nil, "", agentmodel.NewError(agentmodel.ErrTypeInvalidRequest, err.Error())
	}
	gen, err := r.resolveVideoDeployment(ref)
	if err != nil {
		return nil, "", err
	}
	dl, ok := gen.(provider.VideoDownloader)
	if !ok {
		return nil, "", agentmodel.NewErrorf(agentmodel.ErrTypeInvalidRequest,
			"provider %q does not support video download", ref.Provider)
	}
	op, err := gen.PollVideo(ctx, ref.ProviderOp)
	if err != nil {
		return nil, "", agentmodel.Wrap(err)
	}
	if op.Status != agentmodel.VideoStatusSucceeded {
		return nil, "", agentmodel.NewErrorf(agentmodel.ErrTypeInvalidRequest,
			"video operation is not ready (status %q)", op.Status)
	}
	if index < 0 || index >= len(op.Videos) {
		return nil, "", agentmodel.NewErrorf(agentmodel.ErrTypeNotFound,
			"no video at index %d (operation has %d)", index, len(op.Videos))
	}
	return dl.DownloadVideo(ctx, op.Videos[index].URL)
}
