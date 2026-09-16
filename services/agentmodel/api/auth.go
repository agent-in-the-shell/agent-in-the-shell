package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

type ctxKey int

const (
	ctxKeyAPIKeyHash ctxKey = iota
	ctxKeyOrgID
	ctxKeyVirtualKey
)

// defaultOrgID is the single tenant everything is attributed to. Virtual keys
// authenticate independently but still spend against this org's budget.
const defaultOrgID = "default"

// resolvedKey is a virtual key (config `keys`) in its single runtime
// representation: bearer token resolved from token_env, budget cap resolved
// from the config strings, allowlist as a set. A disabled key (unset env or
// unparseable window) can never match an incoming token — fail-closed — but
// keeps its configured cap so /v1/limits still reports it.
type resolvedKey struct {
	portalIssued  bool
	serviceIssued bool
	name          string
	token         string
	hash          string // sha256 hex of token; matches RequestLog.APIKeyHash
	cap           *spendCap
	models        map[string]bool // nil = all models allowed
	disabled      bool
}

// resolveKeys materializes every configured virtual key, reading each token
// from its env var. Keys that cannot be enforced are kept but disabled with
// a warning, never silently dropped.
func resolveKeys(keys []agentmodel.KeyConfig, logger *slog.Logger) []resolvedKey {
	out := make([]resolvedKey, 0, len(keys))
	for _, k := range keys {
		rk := resolvedKey{name: k.Name, models: buildModelSet(k.Models)}
		var capOK bool
		rk.cap, capOK = resolveCap(k.MaxBudget, k.BudgetDuration)
		if !capOK {
			// Unreachable through LoadConfig (Validate rejects it); guards
			// direct api.Config construction.
			logger.Warn("agentmodel: virtual key disabled — invalid budget_duration (fail-closed)",
				"key", k.Name)
			rk.disabled = true
			out = append(out, rk)
			continue
		}
		tok := os.Getenv(k.TokenEnv)
		if tok == "" {
			logger.Warn("agentmodel: virtual key disabled — token env unset or empty (fail-closed)",
				"key", k.Name, "token_env", k.TokenEnv)
			rk.disabled = true
			out = append(out, rk)
			continue
		}
		rk.token = tok
		rk.hash = hashAPIKey(tok)
		out = append(out, rk)
	}
	return out
}

// resolvedKeyFromManaged adapts a DB-backed managed key into the same
// runtime representation as a config key, so model-allowlist + budget
// enforcement and audit attribution all flow through the existing path. The
// token plaintext is never available here (only its hash is stored), so the
// key authenticates by hash lookup rather than constant-time compare. ok is
// false when the stored budget_duration cannot be parsed — unreachable through
// the create endpoint (which validates it), so the caller fails closed.
func resolvedKeyFromManaged(mk store.ManagedKey) (rk *resolvedKey, ok bool) {
	out := &resolvedKey{name: mk.Name, hash: mk.KeyHash, models: buildModelSet(mk.Models)}
	cap, capOK := resolveCap(mk.MaxBudget, mk.BudgetDuration)
	if !capOK {
		return nil, false
	}
	out.cap = cap
	return out, true
}

// buildModelSet turns a model allowlist into a set for O(1) lookup. A nil/empty
// allowlist returns nil — the "all models allowed" sentinel (see allowsModel).
func buildModelSet(models []string) map[string]bool {
	if len(models) == 0 {
		return nil
	}
	set := make(map[string]bool, len(models))
	for _, m := range models {
		set[m] = true
	}
	return set
}

// keyExpired reports whether a managed key's expiry has passed. A nil
// ExpiresAt never expires; the boundary instant counts as expired.
func keyExpired(mk store.ManagedKey, now time.Time) bool {
	return mk.ExpiresAt != nil && !mk.ExpiresAt.After(now)
}

// allowsModel reports whether the key may consume the given logical model.
// The nil receiver is the master token: unrestricted.
func (k *resolvedKey) allowsModel(model string) bool {
	if k == nil || k.models == nil {
		return true
	}
	return k.models[model]
}

// bearerAuth accepts either `Authorization: Bearer <token>` or `X-Api-Key: <token>`.
// Anthropic SDK clients send x-api-key; OpenAI SDK clients send Authorization Bearer.
//
// The token is matched against the master bearer token first, then each
// resolved virtual key. Each individual comparison is constant-time in
// the token contents (subtle.ConstantTimeCompare still returns early on
// length mismatch, so secret *lengths* are observable — accepted: lengths of
// our tokens are not secret). Iterating the key list additionally leaks at
// most the number of configured keys. On a virtual-key match the key's
// identity rides the context so downstream handlers can enforce its model
// allowlist and budget.
// The hash of the token is stuffed into the request context for audit attribution.
func (s *Server) bearerAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got string
		if auth := r.Header.Get("Authorization"); auth != "" {
			got = strings.TrimPrefix(auth, "Bearer ")
			if got == auth {
				// Authorization header present but not Bearer-prefixed.
				got = ""
			}
		}
		if got == "" {
			got = r.Header.Get("X-Api-Key")
		}
		if got == "" {
			writeError(w, http.StatusUnauthorized, &agentmodel.Error{Type: agentmodel.ErrTypeAuthentication, Code: agentmodel.CodeInvalidAPIKey, Message: "missing or malformed Authorization header"})
			return
		}
		// Hashed once here and reused for the managed-key lookup and the audit
		// attribution below — every matched key's hash equals this value (config
		// keys hash their own token; a managed key is found by it).
		incomingHash := hashAPIKey(got)

		var vk *resolvedKey
		matched := subtle.ConstantTimeCompare([]byte(got), []byte(s.bearerToken)) == 1
		if !matched {
			for i := range s.keys {
				if s.keys[i].disabled {
					continue // fail-closed: an unresolved token never matches
				}
				if subtle.ConstantTimeCompare([]byte(got), []byte(s.keys[i].token)) == 1 {
					vk = &s.keys[i]
					matched = true
					break
				}
			}
		}
		// DB-backed managed keys are tried last, only when the master
		// token and every config key miss: operator/legacy credentials win,
		// and an invalid token costs one indexed lookup. Unlike config keys
		// (matched by constant-time compare on plaintext we hold), a managed
		// key is matched by the hash of a 256-bit secret — a plain index hit,
		// no timing concern.
		if !matched && s.store != nil {
			mk, err := s.store.GetKeyByHash(r.Context(), incomingHash)
			switch {
			case err == nil:
				if mk.RevokedAt != nil || mk.Disabled || keyExpired(mk, time.Now()) {
					// Revoked or expired: reject as a plain invalid token below;
					// don't reveal that the key once existed.
					break
				}
				rk, ok := resolvedKeyFromManaged(mk)
				if !ok {
					// Stored cap is unenforceable: fail closed rather than run
					// the key uncapped. Unreachable via the create endpoint
					// (it validates budget_duration).
					s.logger.ErrorContext(r.Context(), "agentmodel: managed key rejected — invalid stored budget_duration (fail-closed)", "key", mk.Name)
					writeError(w, http.StatusServiceUnavailable, &agentmodel.Error{Type: agentmodel.ErrTypeServiceUnavailable, Code: agentmodel.CodeAuthUnavailable,
						Message: "key configured but currently unenforceable; request rejected (fail-closed)"})
					return
				}
				rk.portalIssued = mk.PortalIssued
				rk.serviceIssued = mk.ServiceIssued
				if mk.PortalIssued || mk.ServiceIssued {
					if !store.PortalPolicyOK(mk) {
						portalError(w, http.StatusForbidden, "unsupported_key_policy")
						return
					}
					// Employee/service empty/nil allowlists deny all, unlike legacy keys, so the
					// nil-means-all sentinel from buildModelSet becomes an empty set.
					// Read per request: edits affect models and fallback immediately.
					if rk.models = buildModelSet(mk.Models); rk.models == nil {
						rk.models = map[string]bool{}
					}
				}
				vk = rk
				matched = true
			case errors.Is(err, store.ErrNotFound):
				// genuinely unknown token — fall through to the 401 below
			default:
				// Store unavailable: the token cannot be verified. Fail closed
				// with 503 so a valid key never gets a misleading 401.
				s.logger.ErrorContext(r.Context(), "agentmodel: managed-key lookup failed — failing closed", "error", err)
				writeError(w, http.StatusServiceUnavailable, &agentmodel.Error{Type: agentmodel.ErrTypeServiceUnavailable, Code: agentmodel.CodeAuthUnavailable,
					Message: "authentication backend unavailable; request rejected (fail-closed)"})
				return
			}
		}
		if !matched {
			writeError(w, http.StatusUnauthorized, &agentmodel.Error{Type: agentmodel.ErrTypeAuthentication, Code: agentmodel.CodeInvalidAPIKey, Message: "invalid bearer token"})
			return
		}

		ctx := context.WithValue(r.Context(), ctxKeyAPIKeyHash, incomingHash)
		// Single-org deployment: virtual keys spend against "default" too.
		ctx = context.WithValue(ctx, ctxKeyOrgID, defaultOrgID)
		if vk != nil {
			ctx = context.WithValue(ctx, ctxKeyVirtualKey, vk)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// vkFromCtx returns the authenticated virtual key, or nil for the master token.
func vkFromCtx(ctx context.Context) *resolvedKey {
	v, _ := ctx.Value(ctxKeyVirtualKey).(*resolvedKey)
	return v
}

// masterOnly rejects virtual keys: the wrapped routes carry operator
// authority (e.g. rebinding upstream OAuth credentials), which a tenant key
// must never hold regardless of its budget or model allowlist.
// Employee (portal-issued) and explicit service keys may infer synchronously
// and list models, never
// inspect shared provider accounts, organization usage or unscoped async job
// IDs. The policy is expressed where routes are registered: every /v1 route is
// wrapped in exactly one of employeeKeysAllowed or operatorKeysOnly (masterOnly
// implies the latter), and TestEveryV1RouteClassifiesEmployeeKeys walks the
// router to prove it, so a new route cannot fall into either bucket silently.
func (s *Server) employeeKeysAllowed(next http.Handler) http.Handler {
	return next
}

func (s *Server) operatorKeysOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if vk := vkFromCtx(r.Context()); vk != nil && (vk.portalIssued || vk.serviceIssued) {
			writeError(w, http.StatusForbidden, &agentmodel.Error{Type: agentmodel.ErrTypePermissionDenied, Code: agentmodel.CodeMasterRequired, Message: "endpoint unavailable to employee or service keys"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) masterOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if vk := vkFromCtx(r.Context()); vk != nil {
			writeError(w, http.StatusForbidden, &agentmodel.Error{Type: agentmodel.ErrTypePermissionDenied, Code: agentmodel.CodeMasterRequired,
				Message: "this endpoint requires the master bearer token"})
			return
		}
		next.ServeHTTP(w, r.WithContext(store.WithKeyActor(r.Context(), store.KeyActor{Kind: "master", ID: "shared-master"})))
	})
}

func hashAPIKey(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func apiKeyHashFromCtx(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyAPIKeyHash).(string)
	return v
}

func orgIDFromCtx(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyOrgID).(string)
	if v == "" {
		return defaultOrgID
	}
	return v
}
