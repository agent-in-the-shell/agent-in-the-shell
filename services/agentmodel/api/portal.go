package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/access"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const portalCSRFCookie = "__Host-agentmodel-csrf"

type assertionVerifier interface {
	Verify(context.Context, string) (access.Identity, error)
}

func portalError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

type portalCtxKey struct{}

// portalSession is what the gate establishes for every portal request: the
// verified identity, whether it is an administrator, and the store owner key.
type portalSession struct {
	identity access.Identity
	admin    bool
	owner    store.PortalIdentity
}

func portalSessionFromCtx(ctx context.Context) portalSession {
	session, _ := ctx.Value(portalCtxKey{}).(portalSession)
	return session
}

// mountPortal registers the employee portal as a chi sub-router so every route
// is visible to chi.Walk (and so to route-level tests), with the shared gate as
// middleware instead of hand-written path dispatch. Handlers below assume the
// gate has run.
func (s *Server) mountPortal(r chi.Router) {
	r.Route("/portal", func(r chi.Router) {
		r.Use(s.portalGate)
		r.Get("/", s.portalPage("portal.html"))
		r.Get("/api/me", s.portalMe)
		r.Post("/api/key", s.portalCreateKey)
		r.Post("/api/key/rotate", s.portalRotateKey)
		r.Route("/admin", func(r chi.Router) {
			r.Get("/", s.portalPage("portal_admin.html"))
			r.Get("/api/overview", s.portalOverview)
			r.Post("/api/service-keys", s.portalCreateServiceKey)
			r.Post("/api/service-keys/rotate", s.portalRotateServiceKey)
			r.Get("/api/history", s.portalKeyHistory)
			r.Get("/api/models", s.portalModelCatalog)
			r.Put("/api/models", s.portalSetModels)
			r.Get("/api/monitoring", s.servePortalMonitoring)
			r.Put("/api/state", s.servePortalState)
			r.Post("/api/revoke", s.portalRevokeKey)
		})
	})
}

// portalGate runs on every /portal request, 404s included: response headers,
// availability, host, fetch metadata, Origin, the Access assertion, the admin
// requirement under /portal/admin, and Origin/JSON/CSRF for every mutation.
// Order matters for the codes clients see and is covered by the portal tests.
func (s *Server) portalGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'; connect-src 'self'")
		// Both are resolved once at construction: the verifier is nil exactly
		// when the config failed validation, the store when it cannot back the
		// portal. No per-request re-validation or type assertion.
		if s.portalStore == nil || s.portalVerifier == nil {
			portalError(w, 503, "portal_unavailable")
			return
		}
		if r.Host != s.portalHost {
			portalError(w, 403, "invalid_host")
			return
		}
		// External links may load the HTML shell, but never API data or mutations.
		navigation := r.Method == http.MethodGet && (r.URL.Path == "/portal" || r.URL.Path == "/portal/" || r.URL.Path == "/portal/admin" || r.URL.Path == "/portal/admin/") &&
			r.Header.Get("Sec-Fetch-Mode") == "navigate" && r.Header.Get("Sec-Fetch-Dest") == "document"
		if fetch := r.Header.Get("Sec-Fetch-Site"); fetch != "" && fetch != "same-origin" && fetch != "none" &&
			!(navigation && (fetch == "cross-site" || fetch == "same-site")) {
			portalError(w, 403, "cross_site_request")
			return
		}
		if o := r.Header.Get("Origin"); o != "" && o != s.portal.Origin {
			portalError(w, 403, "invalid_origin")
			return
		}
		identity, err := s.portalVerifier.Verify(r.Context(), r.Header.Get("Cf-Access-Jwt-Assertion"))
		if err != nil {
			portalError(w, 401, "access_assertion_required")
			return
		}
		session := portalSession{identity: identity, admin: s.portal.IsAdmin(identity.Email),
			owner: store.PortalIdentity{Issuer: identity.Issuer, Subject: identity.Subject, Email: identity.Email}}
		if strings.HasPrefix(r.URL.Path, "/portal/admin") && !session.admin {
			portalError(w, 403, "admin_required")
			return
		}
		if r.Method != http.MethodGet {
			cookie, err := r.Cookie(portalCSRFCookie)
			csrf := r.Header.Get("X-CSRF-Token")
			contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if r.Header.Get("Origin") != s.portal.Origin || err != nil || len(csrf) != 43 || subtle.ConstantTimeCompare([]byte(csrf), []byte(cookie.Value)) != 1 {
				portalError(w, 403, "csrf_rejected")
				return
			}
			if contentType != "application/json" {
				portalError(w, 415, "json_required")
				return
			}
		}
		actorKind := "user"
		if session.admin {
			actorKind = "admin"
		}
		// A stable opaque principal reference is sufficient for attribution without
		// copying login emails or assertions into immutable management history.
		actorID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(identity.Issuer+"\x00"+identity.Subject)).String()
		ctx := store.WithKeyActor(r.Context(), store.KeyActor{Kind: actorKind, ID: actorID})
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, portalCtxKey{}, session)))
	})
}

func (s *Server) portalPage(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { s.servePortalHTML(w, name) }
}

func (s *Server) portalMe(w http.ResponseWriter, r *http.Request) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		portalError(w, 400, "invalid_reporting_timezone")
		return
	}
	timezone := query.Get("timezone")
	if values, ok := query["timezone"]; ok {
		if _, err := store.PortalReportingLocation(timezone); err != nil || len(values) != 1 || timezone == "" {
			portalError(w, 400, "invalid_reporting_timezone")
			return
		}
	}
	session := portalSessionFromCtx(r.Context())
	csrf := ""
	if cookie, err := r.Cookie(portalCSRFCookie); err == nil && len(cookie.Value) == 43 {
		csrf = cookie.Value
	}
	if csrf == "" {
		var err error
		csrf, err = randomToken()
		if err != nil {
			portalError(w, 503, "random_unavailable")
			return
		}
		http.SetCookie(w, &http.Cookie{Name: portalCSRFCookie, Value: csrf, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	}
	response := map[string]any{"email": session.identity.Email, "csrf_token": csrf, "is_admin": session.admin, "gateway_origin": s.portal.GatewayOrigin}
	binding, err := s.portalStore.GetPortalBinding(r.Context(), session.owner)
	if err == nil {
		response["binding"] = binding
		mk, err := s.store.GetKeyByID(r.Context(), binding.KeyID)
		if errors.Is(err, store.ErrNotFound) {
			response["state"] = "revoked"
		} else if err != nil {
			portalError(w, 503, "store_unavailable")
			return
		} else {
			response["state"] = portalKeyState(mk)
			response["key"] = map[string]any{"models": append([]string{}, mk.Models...), "max_budget": mk.MaxBudget, "budget_duration": mk.BudgetDuration, "expires_at": mk.ExpiresAt, "disabled": mk.Disabled, "revoked_at": mk.RevokedAt}
		}
	} else if errors.Is(err, store.ErrNotFound) {
		response["state"] = "unissued"
	} else {
		portalError(w, 503, "store_unavailable")
		return
	}
	// The admin page only needs the session (email, CSRF token); the year-wide
	// usage scan is skipped when it asks for that alone.
	if r.URL.Query().Get("fields") != "session" {
		usage, err := s.portalStore.GetPortalUsage(r.Context(), session.owner, time.Now(), timezone)
		if err != nil {
			portalError(w, 503, "store_unavailable")
			return
		}
		response["stats"] = usage.Stats
		response["calendar"] = usage.Calendar
	}
	writeJSON(w, 200, response)
}

func (s *Server) portalCreateKey(w http.ResponseWriter, r *http.Request) {
	session := portalSessionFromCtx(r.Context())
	// Issuance takes no parameters; an unknown field (models, budget) is a 400.
	var body struct{}
	if !decodePortalBody(w, r, &body) {
		return
	}
	for _, key := range s.keys {
		if strings.EqualFold(strings.TrimSpace(key.name), session.identity.Email) {
			portalError(w, 409, "migration_required")
			return
		}
	}
	token, err := generateKeyToken()
	if err != nil {
		portalError(w, 503, "random_unavailable")
		return
	}
	binding, err := s.portalStore.CreatePortalKey(r.Context(), session.owner, store.ManagedKey{ID: "key_" + uuid.NewString(), Name: session.identity.Email, KeyHash: hashAPIKey(token)})
	if portalStoreError(w, err) {
		return
	}
	writeJSON(w, 201, map[string]any{"key_id": binding.KeyID, "revision": binding.Revision, "key": token})
}

func (s *Server) portalRotateKey(w http.ResponseWriter, r *http.Request) {
	session := portalSessionFromCtx(r.Context())
	var body struct {
		Revision int64 `json:"revision"`
	}
	if !decodePortalBody(w, r, &body) {
		return
	}
	if body.Revision < 1 {
		portalError(w, 400, "revision_required")
		return
	}
	token, err := generateKeyToken()
	if err != nil {
		portalError(w, 503, "random_unavailable")
		return
	}
	binding, err := s.portalStore.RotatePortalKey(r.Context(), session.owner, body.Revision, hashAPIKey(token))
	if portalStoreError(w, err) {
		return
	}
	writeJSON(w, 200, map[string]any{"key_id": binding.KeyID, "revision": binding.Revision, "key": token})
}

func decodePortalBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(dst) != nil || d.Decode(new(any)) != io.EOF {
		portalError(w, 400, "invalid_json")
		return false
	}
	return true
}
func portalStoreError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrMigrationRequired):
		portalError(w, 409, "migration_required")
	case errors.Is(err, store.ErrPortalPolicy):
		portalError(w, 409, "unsupported_key_policy")
	case errors.Is(err, store.ErrPortalConflict):
		portalError(w, 409, "revision_conflict")
	case errors.Is(err, store.ErrNotFound):
		portalError(w, 409, "key_unavailable")
	default:
		portalError(w, 503, "store_unavailable")
	}
	return true
}

// State is current-credential state. Revocation wins over pause/expiry.
func portalKeyState(key store.ManagedKey) string {
	if key.RevokedAt != nil {
		return "revoked"
	}
	if key.Disabled {
		return "disabled"
	}
	if keyExpired(key, time.Now()) {
		return "expired"
	}
	return "active"
}
