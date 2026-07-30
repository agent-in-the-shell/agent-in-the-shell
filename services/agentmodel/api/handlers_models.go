package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// discoverTimeout bounds the live-list fetch the discovery path may trigger on
// a cold cache, so an unreachable provider can never hang the request. A warm
// cache (primed at startup / refreshed by the periodic loop) makes discovery a
// pure read and never hits this.
const discoverTimeout = 5 * time.Second

// listModels returns an OpenAI-compatible /v1/models response listing the
// configured logical model_names — the actual routable surface, as clients
// pass them in the "model" field — not the cost price catalog. Useful for
// clients (e.g. Cursor / Continue / OpenWebUI) that introspect available
// models before issuing requests. owned_by is the provider of each model's
// primary deployment.
//
// With the opt-in ?available query parameter, the response additionally lists
// upstream-offered models that have no configured deployment, each tagged with
// a "configured" flag (true for the routable surface, false for discovered
// models). The default response (no ?available) is byte-identical to before and
// never includes unroutable models, so a client can always call every id it
// sees in the default list (#526).
func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	type model struct {
		ID         string `json:"id"`
		Object     string `json:"object"`
		Created    int64  `json:"created"`
		OwnedBy    string `json:"owned_by"`
		Configured *bool  `json:"configured,omitempty"` // set only in ?available mode
	}
	type response struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}

	available := r.URL.Query().Has("available")
	out := response{Object: "list"}
	now := time.Now().Unix()
	if s.router != nil {
		for _, m := range s.router.Models() {
			row := model{ID: m.Name, Object: "model", Created: now, OwnedBy: m.Provider}
			if available {
				yes := true
				row.Configured = &yes
			}
			out.Data = append(out.Data, row)
		}
		if available {
			ctx, cancel := context.WithTimeout(r.Context(), discoverTimeout)
			defer cancel()
			for _, am := range s.router.DiscoverModels(ctx) {
				if am.Configured {
					continue // already listed (by logical name) via Models()
				}
				no := false
				out.Data = append(out.Data, model{
					ID:         am.Model,
					Object:     "model",
					Created:    now,
					OwnedBy:    am.Provider,
					Configured: &no,
				})
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
