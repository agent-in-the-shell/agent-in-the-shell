package wire_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/wire"
)

func TestError_HTTPStatus(t *testing.T) {
	cases := []struct {
		typ  string
		want int
	}{
		{wire.ErrTypeAuthentication, http.StatusUnauthorized},
		{wire.ErrTypePermissionDenied, http.StatusForbidden},
		{wire.ErrTypeNotFound, http.StatusNotFound},
		{wire.ErrTypeInvalidRequest, http.StatusBadRequest},
		// Caller-caused and terminal: 4xx, never 5xx. A 5xx tells SDKs the
		// server broke and the request is worth retrying, which burns quota on
		// a request that can never succeed unchanged.
		{wire.ErrTypeContextWindow, http.StatusBadRequest},
		{wire.ErrTypeContentFilter, http.StatusBadRequest},
		{wire.ErrTypeRateLimit, http.StatusTooManyRequests},
		{wire.ErrTypeUpstream, http.StatusBadGateway},
		{wire.ErrTypeTimeout, http.StatusGatewayTimeout},
		{wire.ErrTypeServiceUnavailable, http.StatusServiceUnavailable},
		{"unknown_type", http.StatusInternalServerError},
	}
	for _, c := range cases {
		t.Run(c.typ, func(t *testing.T) {
			e := wire.NewError(c.typ, "msg")
			if got := e.HTTPStatus(); got != c.want {
				t.Errorf("HTTPStatus: got %d, want %d", got, c.want)
			}
		})
	}
}

func TestError_Retryable(t *testing.T) {
	retryable := []string{
		wire.ErrTypeRateLimit,
		wire.ErrTypeUpstream,
		wire.ErrTypeTimeout,
		wire.ErrTypeServiceUnavailable,
	}
	for _, typ := range retryable {
		t.Run(typ+"_retryable", func(t *testing.T) {
			e := wire.NewError(typ, "x")
			if !e.Retryable() {
				t.Errorf("%q should be retryable", typ)
			}
		})
	}

	terminal := []string{
		wire.ErrTypeAuthentication,
		wire.ErrTypePermissionDenied,
		wire.ErrTypeNotFound,
		wire.ErrTypeInvalidRequest,
		wire.ErrTypeContextWindow,
		wire.ErrTypeContentFilter,
		wire.ErrTypeLoginRequired,
	}
	for _, typ := range terminal {
		t.Run(typ+"_terminal", func(t *testing.T) {
			e := wire.NewError(typ, "x")
			if e.Retryable() {
				t.Errorf("%q should not be retryable", typ)
			}
		})
	}
}

func TestWrap_NormalizesProviderErrors(t *testing.T) {
	cases := []struct {
		desc       string
		input      error
		wantType   string
		wantStatus int
	}{
		{"401 auth", errors.New("openai: 401 invalid_api_key"), wire.ErrTypeAuthentication, http.StatusUnauthorized},
		{"403 permission", errors.New("403 permission denied"), wire.ErrTypePermissionDenied, http.StatusForbidden},
		{"404 model not found", errors.New("404 model not found"), wire.ErrTypeNotFound, http.StatusNotFound},
		{"400 invalid request", errors.New(`anthropic: status 400: {"type":"error","error":{"type":"invalid_request_error","message":"model: claude-sonnet-4-6"}}`), wire.ErrTypeInvalidRequest, http.StatusBadRequest},
		{"429 rate limit", errors.New("status 429: rate limit exceeded"), wire.ErrTypeRateLimit, http.StatusTooManyRequests},
		{"context window", errors.New("This model's maximum context length is 128000 tokens"), wire.ErrTypeContextWindow, http.StatusInternalServerError},
		{"content filter", errors.New("content_policy_violation"), wire.ErrTypeContentFilter, http.StatusInternalServerError},
		{"insufficient quota", errors.New("insufficient_quota"), wire.ErrTypeRateLimit, http.StatusTooManyRequests},
		{"timeout", errors.New("context deadline exceeded"), wire.ErrTypeTimeout, http.StatusGatewayTimeout},
		{"service unavailable", errors.New("503 service unavailable"), wire.ErrTypeServiceUnavailable, http.StatusServiceUnavailable},
		{"500 upstream", errors.New("500 internal server error"), wire.ErrTypeUpstream, http.StatusBadGateway},
		{"unknown -> upstream", errors.New("something went wrong"), wire.ErrTypeUpstream, http.StatusBadGateway},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			ae := wire.Wrap(c.input)
			if ae.Type != c.wantType {
				t.Errorf("Type: got %q, want %q (input: %v)", ae.Type, c.wantType, c.input)
			}
			// HTTPStatus depends on type; only sanity-check known cases.
			if c.wantStatus != http.StatusInternalServerError {
				if got := ae.HTTPStatus(); got != c.wantStatus {
					t.Errorf("HTTPStatus: got %d, want %d", got, c.wantStatus)
				}
			}
			if !errors.Is(ae, c.input) {
				t.Errorf("Unwrap chain broken; expected errors.Is to find original")
			}
		})
	}
}

// TestWrap_SpecificCodeBeatsGenericType pins the classification of real
// upstream bodies, which name a generic Anthropic/OpenAI error *type* and the
// specific *code* in the same payload. The specific code is the useful signal —
// "context window blown, retrying won't help" reads very differently from
// "malformed request" — so it must win over the enclosing invalid_request_error.
//
// Verbatim bodies observed from the ChatGPT Responses backend via the
// chatgpt provider.
func TestWrap_SpecificCodeBeatsGenericType(t *testing.T) {
	cases := []struct {
		desc     string
		input    error
		wantType string
		wantCode string
	}{
		{
			desc:     "context_length_exceeded nested in invalid_request_error",
			input:    errors.New(`chatgpt: stream error: {"type":"error","error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model. Please adjust your input and try again.","param":"input"},"sequence_number":2}`),
			wantType: wire.ErrTypeContextWindow,
			wantCode: wire.CodeContextLength,
		},
		{
			desc:     "content_policy_violation nested in invalid_request_error",
			input:    errors.New(`openai: upstream error (status=400): {"error":{"type":"invalid_request_error","code":"content_policy_violation","message":"Your request was rejected."}}`),
			wantType: wire.ErrTypeContentFilter,
			wantCode: wire.CodeContentPolicy,
		},
		{
			desc:     "plain invalid_request_error keeps the generic type",
			input:    errors.New(`anthropic: status 400: {"type":"error","error":{"type":"invalid_request_error","message":"max_tokens is required"}}`),
			wantType: wire.ErrTypeInvalidRequest,
			wantCode: "",
		},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			ae := wire.Wrap(c.input)
			if ae.Type != c.wantType {
				t.Errorf("Type: got %q, want %q", ae.Type, c.wantType)
			}
			if ae.Code != c.wantCode {
				t.Errorf("Code: got %q, want %q", ae.Code, c.wantCode)
			}
		})
	}
}

func TestWrap_NilReturnsNil(t *testing.T) {
	if got := wire.Wrap(nil); got != nil {
		t.Errorf("Wrap(nil): got %v, want nil", got)
	}
}

func TestWrap_AlreadyNormalizedPassesThrough(t *testing.T) {
	original := wire.NewError(wire.ErrTypeRateLimit, "throttled")
	wrapped := wire.Wrap(original)
	if wrapped != original {
		t.Errorf("Wrap should pass through *Error unchanged")
	}
}

func TestError_JSONShape(t *testing.T) {
	e := &wire.Error{
		Type:    wire.ErrTypeAuthentication,
		Code:    wire.CodeInvalidAPIKey,
		Message: "invalid api key",
		Param:   "api_key",
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out["type"] != "authentication_error" {
		t.Errorf("type: got %v, want authentication_error", out["type"])
	}
	if out["code"] != "invalid_api_key" {
		t.Errorf("code: got %v, want invalid_api_key", out["code"])
	}
	if out["message"] != "invalid api key" {
		t.Errorf("message: got %v, want invalid api key", out["message"])
	}
	if out["param"] != "api_key" {
		t.Errorf("param: got %v, want api_key", out["param"])
	}
}

// TestWrap_NotSupportedIsTerminal: a provider that structurally cannot serve an
// operation returns provider.ErrNotSupported, which must map to a terminal
// invalid_request (non-retryable). Otherwise the router treats it as a transient
// upstream failure, walks every deployment, and returns a misleading 5xx/429
// instead of a terminal 4xx (#1491).
func TestWrap_NotSupportedIsTerminal(t *testing.T) {
	// The literal must equal provider.ErrNotSupported.Error(). wire cannot import
	// provider (import cycle) and — per the release boundary (consumers must not
	// pull provider into even their test closure via wire) — its tests must not
	// either. The real sentinel -> Wrap -> terminal composition is covered in
	// pool_test / router_test, which legitimately import provider.
	err := errors.New("agentmodel/provider: operation not supported by this provider")
	ae := wire.Wrap(err)
	if ae.Type != wire.ErrTypeInvalidRequest {
		t.Errorf("Type = %q, want %q", ae.Type, wire.ErrTypeInvalidRequest)
	}
	if ae.Retryable() {
		t.Error("ErrNotSupported must not be Retryable")
	}
}
