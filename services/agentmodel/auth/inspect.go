package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// StoreStatus is a read-only, offline snapshot of one OAuth token store's
// freshness — what `agent-model status` reports without touching the network or
// mutating the store.
type StoreStatus struct {
	// Exists is true when auth.json is present and parsed. A missing file is
	// Exists:false with a nil error (an expected, first-run / not-logged-in
	// state), distinct from a corrupt file, which is a returned error.
	Exists bool
	// HasToken is true when an access token is present in the store.
	HasToken bool
	// ExpiresAtMS is the access-token expiry as unix milliseconds. Zero means
	// "unknown expiry" — the token is trusted until the upstream rejects it,
	// matching oauthCreds.ExpiresAtMS semantics.
	ExpiresAtMS int64
	// Expired is expiredMS(ExpiresAtMS): false when the expiry is unknown.
	Expired bool
	// AccountID is populated only for providers that record one (ChatGPT).
	AccountID string
	// Format identifies the on-disk layout that was recognized: "native",
	// "chatgpt", or "anthropic". Empty when no token was found.
	Format string
	// GatewayReadable is true when the token sits in the flat access_token field
	// the request path (readCreds) actually reads. It is false for the nested
	// Claude-CLI/pi layout (token under anthropic.access), which the gateway
	// cannot consume even though a token is present — so a health check can
	// distinguish "usable" from "present but the gateway will reject it".
	GatewayReadable bool
}

// rawStore is a permissive union of every on-disk token layout agent-model may
// encounter, so a single decode recognizes all three without a provider-keyed
// branch:
//
//   - native oauthCreds ....... flat access_token + expires_at_ms (this tool's own)
//   - Codex / ChatGPT CLI ..... flat access_token + expires_at (SECONDS)
//   - Claude CLI / pi ......... nested anthropic.{access,expires} (MILLISECONDS)
//
// A field absent from a given layout simply unmarshals to its zero value.
type rawStore struct {
	Schema       string `json:"schema"`
	AccessToken  string `json:"access_token"`
	AccountID    string `json:"account_id"`
	ExpiresAtMS  int64  `json:"expires_at_ms"`
	ExpiresAtSec int64  `json:"expires_at"`
	Anthropic    *struct {
		Access  string `json:"access"`
		Expires int64  `json:"expires"` // unix milliseconds
	} `json:"anthropic"`
}

// InspectStore reports the freshness of the token store in tokenDir without any
// network I/O or refresh. It recognizes the native, Codex/ChatGPT, and
// Claude-CLI/pi on-disk formats and judges expiry through the shared expiredMS,
// the single definition of token freshness. A missing store is Exists:false
// with a nil error; only an existing-but-unparseable file returns an error.
//
// provider is accepted for caller clarity and future disambiguation; format
// recognition is driven by the fields actually present on disk.
func InspectStore(provider, tokenDir string) (StoreStatus, error) {
	b, err := os.ReadFile(authFilePath(tokenDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return StoreStatus{}, nil
		}
		return StoreStatus{}, err
	}

	var raw rawStore
	if err := json.Unmarshal(b, &raw); err != nil {
		return StoreStatus{}, fmt.Errorf("parse %s: %w", authFilePath(tokenDir), err)
	}

	st := StoreStatus{Exists: true}
	switch {
	case raw.Anthropic != nil && raw.Anthropic.Access != "":
		st.HasToken = true
		st.ExpiresAtMS = raw.Anthropic.Expires
		st.Format = "anthropic"
	case raw.AccessToken != "":
		st.HasToken = true
		st.GatewayReadable = true // flat access_token is what readCreds reads
		st.AccountID = raw.AccountID
		switch {
		case raw.ExpiresAtMS != 0:
			st.ExpiresAtMS = raw.ExpiresAtMS
			st.Format = "native"
		case raw.ExpiresAtSec != 0:
			st.ExpiresAtMS = raw.ExpiresAtSec * 1000 // codex records seconds
			st.Format = "chatgpt"
		default:
			// Token present but no recorded expiry (e.g. native store with
			// expires_at_ms:0): trusted until the upstream rejects it.
			st.Format = nativeOrForeign(raw.Schema)
		}
	default:
		// File parsed but carries no access token (e.g. a metadata-only record):
		// recover any recorded expiry for display, report HasToken:false.
		if raw.ExpiresAtMS != 0 {
			st.ExpiresAtMS = raw.ExpiresAtMS
		} else if raw.ExpiresAtSec != 0 {
			st.ExpiresAtMS = raw.ExpiresAtSec * 1000
		}
	}

	st.Expired = expiredMS(st.ExpiresAtMS)
	return st, nil
}

// nativeOrForeign labels a token that carries no expiry by its schema tag:
// agent-model's own writer stamps schemaOAuthV1, everything else is treated as
// an imported ChatGPT/Codex store.
func nativeOrForeign(schema string) string {
	if schema == schemaOAuthV1 {
		return "native"
	}
	return "chatgpt"
}
