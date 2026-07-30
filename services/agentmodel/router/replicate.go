package router

import (
	"context"
	"net/http"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
)

// Replicate passthrough dispatch (#847). Unlike chat/video/image, the Replicate
// model lives in the request BODY (a "version" hash or "owner/name"), not in a
// configured model_name, so there is no model-name routing and no fallback
// graph (Replicate models aren't interchangeable). The gateway has a single
// Replicate upstream, selected by capability: any deployment whose provider
// implements provider.ReplicateProxy. Metering/observability key off a fixed
// logical model so the rate meter and obs hooks stay consistent.
const replicateLogicalModel = "replicate"

// ReplicatePassthrough forwards a raw Replicate request (method + upstream path
// + body + inbound client headers) to a ReplicateProxy-capable deployment and
// returns the raw upstream response (caller MUST close Body), the resolved
// Deployment, and any terminal error. It mirrors GenerateImage's no-fallback
// shape: a model with no Replicate-capable deployment fails with invalid_request
// rather than a generic exhaustion.
func (r *Router) ReplicatePassthrough(ctx context.Context, method, path string, body []byte, clientHeaders http.Header) (*http.Response, Deployment, error) {
	candidates := r.replicateCandidates()
	if len(candidates) == 0 {
		return nil, Deployment{}, agentmodel.NewErrorf(agentmodel.ErrTypeInvalidRequest,
			"no Replicate-capable deployment configured")
	}
	ordered := r.weightedShuffle(candidates)
	hitRateLimit := false
	for _, dep := range ordered {
		proxy, ok := dep.Provider.(provider.ReplicateProxy)
		if !ok {
			continue // defensive: replicateCandidates already filtered for this
		}
		if !r.acquireRateSlot(replicateLogicalModel, dep) {
			hitRateLimit = true // at its configured rpm/tpm cap this minute
			continue
		}
		resp, err := proxy.Forward(ctx, method, path, body, clientHeaders)
		if err == nil {
			r.obsResult(replicateLogicalModel, dep.Name, dep.Provider.Name(), true)
			return resp, dep, nil
		}
		ae := agentmodel.Wrap(err)
		r.obsResult(replicateLogicalModel, dep.Name, dep.Provider.Name(), false)
		if !ae.Retryable() {
			return nil, Deployment{}, ae
		}
		hitRateLimit = true
	}
	if hitRateLimit {
		return nil, Deployment{}, errAllRateLimited(replicateLogicalModel)
	}
	return nil, Deployment{}, ErrAllFailed
}

// replicateCandidates collects every configured deployment whose provider can
// proxy Replicate predictions, deduped across model_names a provider may be
// listed under (the model_name is irrelevant to this dispatch).
func (r *Router) replicateCandidates() []Deployment {
	var out []Deployment
	seen := make(map[string]bool)
	for _, deps := range r.deployments {
		for _, d := range deps {
			if _, ok := d.Provider.(provider.ReplicateProxy); !ok {
				continue
			}
			key := d.Name + "\x00" + d.Model
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, d)
		}
	}
	return out
}
