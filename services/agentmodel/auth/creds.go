package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// schemaOAuthV1 tags agent-model's own on-disk OAuth credential format so a
// store written by this version is self-identifying.
const schemaOAuthV1 = "agentmodel.oauth/v1"

// oauthExpirySkew is subtracted from a token's expiry before checking
// freshness, to absorb clock skew and in-flight request latency. Shared by
// every provider's expiry check via expiredMS.
const oauthExpirySkew = 60 * time.Second

// expiryFromExpiresIn converts an OAuth expires_in (seconds) to a unix-ms
// expiry, returning 0 (== "unknown, trusted until the upstream rejects it")
// when expires_in is absent or non-positive. expires_in is only RECOMMENDED by
// RFC 6749, so a refresh response may omit it; computing now()+0 would mark the
// just-minted token immediately expired (expiredMS(now)==true) and trigger a
// refresh on every subsequent request. The login/device paths already store 0
// in this case; this keeps the refresh paths consistent.
func expiryFromExpiresIn(expiresInSeconds int) int64 {
	if expiresInSeconds <= 0 {
		return 0
	}
	return time.Now().Add(time.Duration(expiresInSeconds) * time.Second).UnixMilli()
}

// expiredMS reports whether the supplied unix-millisecond expiry is in the
// past after applying oauthExpirySkew. Zero means "unknown expiry" — the token
// is trusted until the upstream rejects it (this convention is the meaning of
// oauthCreds.ExpiresAtMS == 0). This is the single definition of token
// freshness for all providers.
func expiredMS(unixMillis int64) bool {
	if unixMillis == 0 {
		return false
	}
	return time.Now().UnixMilli()+oauthExpirySkew.Milliseconds() >= unixMillis
}

// Provider identifiers stamped into the on-disk credential record.
const (
	providerAnthropic = "anthropic"
	providerChatGPT   = "chatgpt"
)

// oauthCreds is agent-model's native on-disk OAuth credential record.
//
// It is deliberately independent of any external tool's auth.json layout:
// agent-model owns its credential store outright and neither borrows another
// tool's schema nor shares a store with one. Each provider's tokens live in
// their own file (<tokenDir>/auth.json), one provider per file — the writer
// never reads, merges, or preserves foreign content.
//
// IDToken and AccountID are only populated for providers that issue them
// (ChatGPT); they are omitted from the anthropic record.
type oauthCreds struct {
	Schema       string `json:"schema"`
	Provider     string `json:"provider"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token,omitempty"`
	AccountID    string `json:"account_id,omitempty"`
	// ExpiresAtMS is the access-token expiry as unix milliseconds. Zero means
	// "unknown expiry" — the token is trusted until the upstream rejects it.
	ExpiresAtMS int64 `json:"expires_at_ms"`
}

// authFilePath returns the auth.json path within a token directory.
func authFilePath(tokenDir string) string { return filepath.Join(tokenDir, "auth.json") }

// readCreds decodes native credentials from <tokenDir>/auth.json. A missing
// file surfaces as os.ErrNotExist (via os.ReadFile) so callers can distinguish
// first-run (expected) from a corrupt store (operator must investigate).
func readCreds(tokenDir string) (*oauthCreds, error) {
	path := authFilePath(tokenDir)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c oauthCreds
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &c, nil
}

// writeCreds persists creds to <tokenDir>/auth.json atomically (tmp+rename),
// creating tokenDir with 0700 if needed. The file holds exactly one provider's
// credentials; nothing else on disk is read or preserved.
func writeCreds(tokenDir string, c *oauthCreds) error {
	c.Schema = schemaOAuthV1 // writeCreds owns the schema tag; callers set Provider
	if err := os.MkdirAll(tokenDir, 0o700); err != nil {
		return fmt.Errorf("auth: mkdir %s: %w", tokenDir, err)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(authFilePath(tokenDir), b)
}
