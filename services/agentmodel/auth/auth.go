// Package auth provides Authenticator implementations for the agentmodel
// gateway. Two auth modes are supported:
//
//   - api_key: provider issues a static bearer token / API key. Cost is tracked
//     per token and billed against the customer's wallet.
//
//   - subscription: an OAuth bearer token tied to the end-user's consumer
//     subscription (Claude Pro/Max, ChatGPT Plus/Pro). Per-request cost is $0
//     because the subscription covers usage as a flat fee.
//
// The Authenticator interface lets providers stay agnostic to which auth mode
// is in use; callers wire the right impl in based on the deployment config.
package auth

import (
	"context"
	"errors"
	"net/http"
)

// Authenticator applies credentials to outgoing HTTP requests.
type Authenticator interface {
	// Mode returns "api_key" or "subscription".
	Mode() string
	// Apply mutates the request to add auth (headers, query params, etc).
	Apply(ctx context.Context, req *http.Request) error
}

// ErrLoginRequired indicates the user must re-authenticate (OAuth flow).
// Callers can catch this and route to a login UI / fallback provider.
var ErrLoginRequired = errors.New("agentmodel/auth: login required (no valid token available)")

// IsLoginRequired reports whether err indicates re-authentication is needed.
func IsLoginRequired(err error) bool {
	return errors.Is(err, ErrLoginRequired)
}
