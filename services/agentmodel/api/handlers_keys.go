package api

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

// Runtime virtual-key management (#922). These endpoints live under the
// masterOnly group: minting and revoking tenant credentials is operator
// authority, never tenant authority. Tokens are minted here, returned once,
// and never stored in plaintext — only their sha256 hash persists, the same
// value request_logs attributes spend to.

const (
	keyTokenPrefix = "sk-am-"
	// codeInvalidParameter (handlers_usage.go) is reused for bad key arguments.
	codeKeyNotFound    = "key_not_found"
	codeKeyStoreFailed = "key_store_failed"
)

// keyCreateRequest is the POST /v1/keys body. All fields except name are
// optional; an absent cap/allowlist means unlimited/all-models.
type keyCreateRequest struct {
	Name           string          `json:"name"`
	Models         []string        `json:"models"`
	MaxBudget      *float64        `json:"max_budget"`
	BudgetDuration string          `json:"budget_duration"` // Go duration; "" = lifetime
	Duration       string          `json:"duration"`        // Go duration from now → expires_at; "" = never
	Metadata       json.RawMessage `json:"metadata"`        // opaque JSON object, stored verbatim
}

// keyView is the safe projection of a managed key — everything except the
// token. Returned by list/info/revoke; the create response embeds it and adds
// the one-time plaintext.
type keyView struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	KeyHash        string          `json:"key_hash"`
	Models         []string        `json:"models,omitempty"`
	MaxBudget      *float64        `json:"max_budget,omitempty"`
	BudgetDuration string          `json:"budget_duration,omitempty"`
	ExpiresAt      *int64          `json:"expires_at,omitempty"`
	Disabled       bool            `json:"disabled"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
	CreatedAt      int64           `json:"created_at"`
	Spend          *float64        `json:"spend,omitempty"` // lifetime USD; info endpoint only
}

// keyCreateResponse is keyView plus the plaintext token, returned exactly once
// at creation. The caller must persist it — the gateway keeps only the hash.
type keyCreateResponse struct {
	keyView
	Key string `json:"key"`
}

func keyToView(mk store.ManagedKey) keyView {
	v := keyView{
		ID:             mk.ID,
		Name:           mk.Name,
		KeyHash:        mk.KeyHash,
		Models:         mk.Models,
		MaxBudget:      mk.MaxBudget,
		BudgetDuration: mk.BudgetDuration,
		Disabled:       mk.Disabled,
		CreatedAt:      mk.CreatedAt.Unix(),
	}
	if mk.ExpiresAt != nil {
		u := mk.ExpiresAt.Unix()
		v.ExpiresAt = &u
	}
	if mk.Metadata != "" {
		v.Metadata = json.RawMessage(mk.Metadata)
	}
	return v
}

// createKey mints a new managed key. POST /v1/keys.
func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeKeyStoreUnavailable(w)
		return
	}
	var req keyCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, &agentmodel.Error{Type: agentmodel.ErrTypeInvalidRequest, Code: agentmodel.CodeInvalidJSON, Message: "invalid JSON body"})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeKeyArgError(w, "name is required")
		return
	}
	if req.MaxBudget != nil && *req.MaxBudget <= 0 {
		writeKeyArgError(w, "max_budget must be greater than 0")
		return
	}
	if _, ok := parsePositiveDuration(req.BudgetDuration); !ok {
		writeKeyArgError(w, "budget_duration must be a positive Go duration (e.g. 24h)")
		return
	}
	dur, ok := parsePositiveDuration(req.Duration)
	if !ok {
		writeKeyArgError(w, "duration must be a positive Go duration (e.g. 720h)")
		return
	}
	var expiresAt *time.Time
	if dur > 0 {
		t := time.Now().UTC().Add(dur)
		expiresAt = &t
	}
	meta, ok := normalizeMetadata(req.Metadata)
	if !ok {
		writeKeyArgError(w, "metadata must be a JSON value")
		return
	}

	token, err := generateKeyToken()
	if err != nil {
		writeKeyStoreError(w, "could not generate key material")
		return
	}
	mk := store.ManagedKey{
		ID:             "key_" + uuid.NewString(),
		Name:           req.Name,
		KeyHash:        hashAPIKey(token),
		Models:         req.Models,
		MaxBudget:      req.MaxBudget,
		BudgetDuration: req.BudgetDuration,
		ExpiresAt:      expiresAt,
		Metadata:       meta,
		CreatedAt:      time.Now().UTC(),
	}
	if err := s.store.CreateKey(r.Context(), mk); err != nil {
		writeKeyStoreError(w, "could not persist key")
		return
	}
	writeJSON(w, http.StatusCreated, keyCreateResponse{keyView: keyToView(mk), Key: token})
}

// listKeys returns all managed keys (no plaintext). GET /v1/keys.
func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeKeyStoreUnavailable(w)
		return
	}
	keys, err := s.store.ListKeys(r.Context())
	if err != nil {
		writeKeyStoreError(w, "could not list keys")
		return
	}
	views := make([]keyView, 0, len(keys))
	for _, k := range keys {
		views = append(views, keyToView(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": views})
}

// getKey returns one key's config plus its lifetime spend. GET /v1/keys/{id}.
func (s *Server) getKey(w http.ResponseWriter, r *http.Request) {
	mk, ok := s.lookupKey(w, r)
	if !ok {
		return
	}
	view := keyToView(mk)
	// Spend is the read this whole feature builds on; a lookup failure is
	// non-fatal for an info call — return the config, omit the figure.
	if spend, err := s.store.SumCostByAPIKey(r.Context(), mk.KeyHash, time.Time{}); err == nil {
		view.Spend = &spend
	} else {
		s.logger.ErrorContext(r.Context(), "agentmodel: key spend lookup failed", "key", mk.ID, "error", err)
	}
	writeJSON(w, http.StatusOK, view)
}

// setKeyRevoked builds a handler that toggles a key's disabled flag. Revoking
// retains the row so historical request_logs stay attributable.
func (s *Server) setKeyRevoked(disabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.store == nil {
			writeKeyStoreUnavailable(w)
			return
		}
		id := chi.URLParam(r, "id")
		switch err := s.store.SetKeyDisabled(r.Context(), id, disabled); {
		case errors.Is(err, store.ErrNotFound):
			writeKeyNotFound(w)
			return
		case err != nil:
			writeKeyStoreError(w, "could not update key")
			return
		}
		mk, err := s.store.GetKeyByID(r.Context(), id)
		if err != nil {
			writeKeyStoreError(w, "could not read back key")
			return
		}
		writeJSON(w, http.StatusOK, keyToView(mk))
	}
}

// deleteKey hard-deletes a key. DELETE /v1/keys/{id}.
func (s *Server) deleteKey(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeKeyStoreUnavailable(w)
		return
	}
	switch err := s.store.DeleteKey(r.Context(), chi.URLParam(r, "id")); {
	case errors.Is(err, store.ErrNotFound):
		writeKeyNotFound(w)
	case err != nil:
		writeKeyStoreError(w, "could not delete key")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// lookupKey resolves {id} for the read endpoints, writing the appropriate
// error (and returning ok=false) on a missing store, missing id, or failure.
func (s *Server) lookupKey(w http.ResponseWriter, r *http.Request) (store.ManagedKey, bool) {
	if s.store == nil {
		writeKeyStoreUnavailable(w)
		return store.ManagedKey{}, false
	}
	mk, err := s.store.GetKeyByID(r.Context(), chi.URLParam(r, "id"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeKeyNotFound(w)
		return store.ManagedKey{}, false
	case err != nil:
		writeKeyStoreError(w, "could not read key")
		return store.ManagedKey{}, false
	}
	return mk, true
}

// generateKeyToken mints a high-entropy bearer token. 32 bytes of CSPRNG
// output, base64url-encoded, behind the sk-am- prefix.
func generateKeyToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return keyTokenPrefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// parsePositiveDuration accepts an empty string (→ 0, the "none" sentinel for
// lifetime budgets / never-expiring keys) or a positive Go duration. ok is
// false when a non-empty value is unparseable or non-positive.
func parsePositiveDuration(d string) (time.Duration, bool) {
	if d == "" {
		return 0, true
	}
	parsed, err := time.ParseDuration(d)
	if err != nil || parsed <= 0 {
		return 0, false
	}
	return parsed, true
}

// normalizeMetadata validates the optional metadata blob is JSON and returns
// it as a string ("" when absent).
func normalizeMetadata(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", true
	}
	if !json.Valid(raw) {
		return "", false
	}
	return string(raw), true
}

func writeKeyArgError(w http.ResponseWriter, msg string) {
	writeError(w, http.StatusBadRequest, &agentmodel.Error{Type: agentmodel.ErrTypeInvalidRequest, Code: codeInvalidParameter, Message: msg})
}

func writeKeyNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, &agentmodel.Error{Type: agentmodel.ErrTypeNotFound, Code: codeKeyNotFound, Message: "key not found"})
}

func writeKeyStoreError(w http.ResponseWriter, msg string) {
	// 503 to match the type (ErrTypeServiceUnavailable maps to 503 and is
	// Retryable) — a persistent store failure was previously sent as a terminal
	// 500 whose retryable type contradicted the status.
	writeError(w, http.StatusServiceUnavailable, &agentmodel.Error{Type: agentmodel.ErrTypeServiceUnavailable, Code: codeKeyStoreFailed, Message: msg})
}

func writeKeyStoreUnavailable(w http.ResponseWriter) {
	writeError(w, http.StatusServiceUnavailable, &agentmodel.Error{Type: agentmodel.ErrTypeServiceUnavailable, Code: agentmodel.CodeAuthUnavailable, Message: "key store not configured"})
}
