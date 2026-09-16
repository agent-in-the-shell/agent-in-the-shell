package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
	"github.com/google/uuid"
)

// modelCatalog lists the routable logical model names; never nil, so it
// serializes as [] when nothing is configured.
func (s *Server) modelCatalog() []string {
	catalog := []string{}
	if s.router != nil {
		for _, model := range s.router.Models() {
			catalog = append(catalog, model.Name)
		}
	}
	return catalog
}

// Admin routes: the gate has verified an administrator and, for writes, the
// Origin/JSON/CSRF triple.
func (s *Server) portalOverview(w http.ResponseWriter, r *http.Request) {
	overview, err := s.portalStore.GetPortalOverview(r.Context(), time.Now(), s.portalReportScope())
	if portalStoreError(w, err) {
		return
	}
	writeJSON(w, 200, overview)
}

func (s *Server) portalModelCatalog(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{"catalog": s.modelCatalog()})
}

func (s *Server) portalSetModels(w http.ResponseWriter, r *http.Request) {
	var body struct {
		KeyID    string    `json:"key_id"`
		Revision int64     `json:"revision"`
		Models   *[]string `json:"models"`
	}
	if !decodePortalBody(w, r, &body) {
		return
	}
	if body.KeyID == store.PortalMasterKeyID {
		portalError(w, 409, "key_unavailable")
		return
	}
	if strings.TrimSpace(body.KeyID) == "" {
		portalError(w, 400, "key_id_required")
		return
	}
	if body.Models == nil {
		portalError(w, 400, "models_array_required")
		return
	}
	catalog := map[string]bool{}
	for _, model := range s.modelCatalog() {
		catalog[model] = true
	}
	seen := map[string]bool{}
	for _, model := range *body.Models {
		if !catalog[model] || seen[model] {
			portalError(w, 400, "invalid_model")
			return
		}
		seen[model] = true
	}
	if portalStoreError(w, s.portalStore.SetPortalModels(r.Context(), body.KeyID, *body.Models, body.Revision)) {
		return
	}
	writeJSON(w, 200, map[string]any{"key_id": body.KeyID, "models": *body.Models})
}

// Only a name is accepted: grants use the existing, separately confirmed editor.
func (s *Server) portalCreateServiceKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !decodePortalBody(w, r, &body) {
		return
	}
	name, normalized, err := store.NormalizeServiceName(body.Name)
	if err != nil {
		portalError(w, 400, "invalid_service_name")
		return
	}
	for _, key := range s.keys {
		if strings.ToLower(strings.TrimSpace(key.name)) == normalized {
			portalError(w, 409, "service_name_conflict")
			return
		}
	}
	token, err := generateKeyToken()
	if err != nil {
		portalError(w, 503, "random_unavailable")
		return
	}
	key, err := s.portalStore.CreateServiceKey(r.Context(), store.ManagedKey{ID: "key_" + uuid.NewString(), Name: name, KeyHash: hashAPIKey(token)})
	if errors.Is(err, store.ErrServiceNameConflict) {
		portalError(w, 409, "service_name_conflict")
		return
	}
	if portalStoreError(w, err) {
		return
	}
	// Deliberate public projection: never serialize ManagedKey or its hash.
	writeJSON(w, 201, struct {
		store.PortalAdminKey
		Key string `json:"key"`
	}{key, token})
}

func (s *Server) portalRevokeKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		KeyID string `json:"key_id"`
	}
	if !decodePortalBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.KeyID) == "" || strings.TrimSpace(body.KeyID) != body.KeyID || len(body.KeyID) > 256 {
		portalError(w, 400, "key_id_required")
		return
	}
	if portalStoreError(w, s.portalStore.RevokePortalKey(r.Context(), body.KeyID)) {
		return
	}
	mk, err := s.store.GetKeyByID(r.Context(), body.KeyID)
	if portalStoreError(w, err) {
		return
	}
	state := portalKeyState(mk)
	writeJSON(w, 200, map[string]any{"key_id": body.KeyID, "state": state, "revoked_at": mk.RevokedAt, "models": append([]string{}, mk.Models...)})
}

func (s *Server) portalRotateServiceKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		KeyID    string `json:"key_id"`
		Revision int64  `json:"revision"`
	}
	if !decodePortalBody(w, r, &body) {
		return
	}
	if body.KeyID == "" || len(body.KeyID) > 256 || body.Revision < 1 {
		portalError(w, 400, "invalid_rotation")
		return
	}
	token, err := generateKeyToken()
	if err != nil {
		portalError(w, 503, "random_unavailable")
		return
	}
	k, err := s.portalStore.RotateServiceKey(r.Context(), body.KeyID, body.Revision, hashAPIKey(token))
	if portalStoreError(w, err) {
		return
	}
	writeJSON(w, 200, struct {
		store.PortalAdminKey
		Key string `json:"key"`
	}{
		store.PortalAdminKey{KeyID: k.ID, Name: k.Name, Kind: "service", State: portalKeyState(k), Models: append([]string{}, k.Models...), Revision: k.ServiceRevision}, token})
}

func (s *Server) portalKeyHistory(w http.ResponseWriter, r *http.Request) {
	keyID := r.URL.Query().Get("key_id")
	before := int64(0)
	if v := r.URL.Query().Get("before"); v != "" {
		var err error
		before, err = strconv.ParseInt(v, 10, 64)
		if err != nil || before < 0 {
			portalError(w, 400, "invalid_history_filter")
			return
		}
	}
	if keyID == "" || len(keyID) > 256 {
		portalError(w, 400, "invalid_history_filter")
		return
	}
	events, err := s.portalStore.GetKeyHistory(r.Context(), keyID, before)
	if portalStoreError(w, err) {
		return
	}
	writeJSON(w, 200, map[string]any{"events": events})
}
