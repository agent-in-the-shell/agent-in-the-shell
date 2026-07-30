package wire

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Error is the unified error type that wraps a provider's failure mode and
// surfaces it in OpenAI's error-object shape via JSON marshalling.
//
// The Type field uses the same vocabulary as OpenAI's API errors so that
// existing OpenAI-compatible clients can interpret responses without changes.
type Error struct {
	Type    string `json:"type"`            // "authentication_error", "rate_limit_error", etc.
	Code    string `json:"code,omitempty"`  // OpenAI-style error code
	Message string `json:"message"`         // Human-readable message
	Param   string `json:"param,omitempty"` // Optional field that caused the error
	Wrapped error  `json:"-"`               // underlying provider error, not serialized
	// RetryAfter is how long the upstream said to wait before retrying, 0 when
	// it gave no usable hint. Not serialized into the error object (neither
	// OpenAI nor Anthropic put it there) — it travels as the Retry-After
	// response header, and in-process it sizes the credential/deployment
	// cooldowns so a subscription parked by a 5-hour quota window is not
	// retried on a 5-minute default.
	RetryAfter time.Duration `json:"-"`
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("agentmodel: %s [%s]: %s", e.Type, e.Code, e.Message)
	}
	return fmt.Sprintf("agentmodel: %s: %s", e.Type, e.Message)
}

// Unwrap exposes the underlying provider error so callers can use errors.Is
// against deeper sentinels (e.g. context.Canceled).
func (e *Error) Unwrap() error { return e.Wrapped }

// HTTPStatus maps the error type to an HTTP status code.
func (e *Error) HTTPStatus() int {
	switch e.Type {
	case ErrTypeAuthentication:
		return http.StatusUnauthorized
	case ErrTypePermissionDenied:
		return http.StatusForbidden
	case ErrTypeNotFound:
		return http.StatusNotFound
	case ErrTypeInvalidRequest:
		return http.StatusBadRequest
	case ErrTypeContextWindow, ErrTypeContentFilter:
		// Both are terminal rejections of the caller's input — the same 400 the
		// upstreams themselves return, and the same 400 these carried while
		// they were (mis)classified as ErrTypeInvalidRequest. Falling through
		// to a 500 here would tell SDKs to retry a request that cannot succeed.
		return http.StatusBadRequest
	case ErrTypeBudgetExceeded:
		// 400 matches LiteLLM's budget_exceeded semantics: terminal for the
		// caller (not retryable-soon like a 429 — the window has to roll over
		// or the cap has to be raised).
		return http.StatusBadRequest
	case ErrTypeRateLimit:
		return http.StatusTooManyRequests
	case ErrTypeUpstream:
		return http.StatusBadGateway
	case ErrTypeTimeout:
		return http.StatusGatewayTimeout
	case ErrTypeServiceUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// Retryable reports whether the error is safe to retry (e.g. transient
// upstream failures, rate limits) versus terminal (auth, invalid request).
//
// The router uses this to decide whether to fall back to another deployment.
func (e *Error) Retryable() bool {
	switch e.Type {
	case ErrTypeRateLimit,
		ErrTypeUpstream,
		ErrTypeTimeout,
		ErrTypeServiceUnavailable:
		return true
	default:
		return false
	}
}

// Error type constants — use these instead of magic strings.
const (
	ErrTypeAuthentication     = "authentication_error"
	ErrTypePermissionDenied   = "permission_error"
	ErrTypeNotFound           = "not_found_error"
	ErrTypeInvalidRequest     = "invalid_request_error"
	ErrTypeRateLimit          = "rate_limit_error"
	ErrTypeContextWindow      = "context_window_exceeded"
	ErrTypeContentFilter      = "content_filter_error"
	ErrTypeUpstream           = "upstream_error"
	ErrTypeTimeout            = "timeout_error"
	ErrTypeServiceUnavailable = "service_unavailable"
	ErrTypeLoginRequired      = "login_required"
	ErrTypeAllDeploymentsFail = "all_deployments_failed"
	ErrTypeBudgetExceeded     = "budget_exceeded"
)

// Common error code constants.
const (
	CodeInvalidAPIKey     = "invalid_api_key"
	CodeRateLimitExceeded = "rate_limit_exceeded"
	CodeContextLength     = "context_length_exceeded"
	CodeModelNotFound     = "model_not_found"
	CodeContentPolicy     = "content_policy_violation"
	CodeQuotaExceeded     = "insufficient_quota"
	CodeRequestLogin      = "subscription_login_required"
	// Enforcement codes (#47/#52): pre-request policy rejections.
	CodeBudgetExceeded    = "budget_exceeded"
	CodeModelAccessDenied = "model_access_denied"
	CodeBudgetUnavailable = "budget_check_unavailable"
	CodeMasterRequired    = "master_token_required"
	// CodeAuthUnavailable (#922): a managed-key lookup could not be completed
	// (store unavailable, or a stored cap is unenforceable). Fail-closed 503 —
	// distinct from invalid_api_key, which means the token is genuinely wrong.
	CodeAuthUnavailable = "auth_check_unavailable"
	// Request-validation codes (#705): gateway-side rejections of a malformed
	// request before any upstream call. CodeEmptyArray and
	// CodeMissingRequiredParam mirror OpenAI's current vocabulary; CodeInvalidJSON
	// is gateway-minted (OpenAI emits a null code for malformed JSON).
	CodeEmptyArray           = "empty_array"
	CodeMissingRequiredParam = "missing_required_parameter"
	CodeInvalidJSON          = "invalid_json"
)

// NewError constructs an Error of the given type with a formatted message.
func NewError(typ, message string) *Error {
	return &Error{Type: typ, Message: message}
}

// NewErrorf is the printf-style sibling of NewError.
func NewErrorf(typ, format string, args ...any) *Error {
	return &Error{Type: typ, Message: fmt.Sprintf(format, args...)}
}

// NewErrorCode constructs an Error carrying a machine-readable code alongside
// the type and message.
func NewErrorCode(typ, code, message string) *Error {
	return &Error{Type: typ, Code: code, Message: message}
}

// Wrap converts a provider-specific error into our normalized form, attaching
// the original error chain via Unwrap().
//
// Heuristics inspect the error string for common patterns. Providers may
// also call NewError directly when they have authoritative status info.
//
// A provider error implementing RetryAfterHinter has its hint copied onto
// RetryAfter, so the upstream's own "come back at" survives classification and
// can size the cooldowns.
func Wrap(err error) *Error {
	if err == nil {
		return nil
	}
	// Already normalized: pass through.
	var ae *Error
	if errors.As(err, &ae) {
		return ae
	}

	out := classify(err)
	var hinter RetryAfterHinter
	if errors.As(err, &hinter) {
		out.RetryAfter = ClampRetryAfter(hinter.RetryAfterHint())
	}
	return out
}

func classify(err error) *Error {
	msg := err.Error()
	lower := strings.ToLower(msg)

	switch {
	case containsAny(lower, "401", "invalid_api_key", "invalid api key", "authentication failed", "authentication"):
		return &Error{Type: ErrTypeAuthentication, Code: CodeInvalidAPIKey, Message: msg, Wrapped: err}
	case containsAny(lower, "403", "permission denied", "forbidden"):
		return &Error{Type: ErrTypePermissionDenied, Message: msg, Wrapped: err}
	case containsAny(lower, "404", "model not found", "model_not_found"):
		return &Error{Type: ErrTypeNotFound, Code: CodeModelNotFound, Message: msg, Wrapped: err}
	// provider.ErrNotSupported: the provider structurally cannot serve this
	// operation (e.g. Replicate has no chat Complete, DeepSeek no Embed). It is a
	// terminal client error — mapping it to the retryable default would make the
	// router walk every deployment and surface a misleading 5xx/429 instead of a
	// terminal 4xx. The substring matches ErrNotSupported's message exactly.
	case containsAny(lower, "not supported by this provider"):
		return &Error{Type: ErrTypeInvalidRequest, Message: msg, Wrapped: err}
	// Context-window and content-filter rejections arrive from OpenAI-shaped
	// backends as a *code* nested inside a generic invalid_request_error type,
	// so they must be tested before the 400 case below — otherwise the generic
	// case swallows them and ErrTypeContextWindow / ErrTypeContentFilter are
	// unreachable for those providers.
	case containsAny(lower, "context_length_exceeded", "context length", "maximum context length", "context window"):
		return &Error{Type: ErrTypeContextWindow, Code: CodeContextLength, Message: msg, Wrapped: err}
	case containsAny(lower, "content_policy_violation", "content_filter", "content policy", "content filter", "responsibleaipolicyviolation"):
		return &Error{Type: ErrTypeContentFilter, Code: CodeContentPolicy, Message: msg, Wrapped: err}
	case containsAny(lower, "400", "bad request", "invalid_request_error", "invalid request"):
		return &Error{Type: ErrTypeInvalidRequest, Message: msg, Wrapped: err}
	case containsAny(lower, "429", "rate limit", "rate_limit", "ratelimit"):
		return &Error{Type: ErrTypeRateLimit, Code: CodeRateLimitExceeded, Message: msg, Wrapped: err}
	case containsAny(lower, "quota", "insufficient_quota"):
		return &Error{Type: ErrTypeRateLimit, Code: CodeQuotaExceeded, Message: msg, Wrapped: err}
	case containsAny(lower, "timeout", "deadline exceeded"):
		return &Error{Type: ErrTypeTimeout, Message: msg, Wrapped: err}
	case containsAny(lower, "503", "service unavailable", "overloaded"):
		return &Error{Type: ErrTypeServiceUnavailable, Message: msg, Wrapped: err}
	default:
		// Default to upstream/5xx — covers "500 internal server error" and
		// genuinely unknown errors. Specific 4xx/auth/etc. cases above are
		// detected by their substrings.
		return &Error{Type: ErrTypeUpstream, Message: msg, Wrapped: err}
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
