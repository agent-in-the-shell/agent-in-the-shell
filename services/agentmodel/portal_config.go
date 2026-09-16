package agentmodel

import (
	"errors"
	"net/mail"
	"path/filepath"
	"strings"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/access"
)

// PortalConfig is an explicit opt-in, independent of /v1 bearer authentication.
// Model policy is owned by the database, never individual keys or YAML.
type PortalConfig struct {
	Enabled bool   `yaml:"enabled,omitempty"`
	HTMLDir string `yaml:"html_dir,omitempty"`
	Origin  string `yaml:"origin,omitempty"`
	// GatewayOrigin is the base the pages tell clients to call (the /v1 host),
	// which is usually not the portal's own origin. Reported by /portal/api/me;
	// the page falls back to its own origin when empty.
	GatewayOrigin string   `yaml:"gateway_origin,omitempty"`
	Issuer        string   `yaml:"issuer,omitempty"`
	Audience      string   `yaml:"audience,omitempty"`
	EmailDomain   string   `yaml:"email_domain,omitempty"`
	AdminEmails   []string `yaml:"admin_emails,omitempty"`
}

// normalizeEmail is the one definition of "the same address" for admin
// matching; the store applies the same lower(trim()) in SQL.
func normalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

func (c PortalConfig) IsAdmin(email string) bool {
	email = normalizeEmail(email)
	for _, admin := range c.AdminEmails {
		if email != "" && email == normalizeEmail(admin) {
			return true
		}
	}
	return false
}

func (c PortalConfig) Validate() error {
	// Absolute paths stay unambiguous under service managers. Files are checked
	// on each page request, not here, so operators can replace them at runtime.
	if c.HTMLDir != "" && (!filepath.IsAbs(c.HTMLDir) || strings.ContainsRune(c.HTMLDir, '\x00')) {
		return errors.New("agentmodel/config: portal.html_dir must be an absolute filesystem path")
	}
	if !c.Enabled {
		return nil
	}
	invalid := errors.New("agentmodel/config: portal requires an HTTPS origin, Cloudflare Access issuer, audience, email_domain and valid admin_emails")
	if _, err := access.ParseHTTPSOrigin(c.Origin); err != nil {
		return invalid
	}
	if c.GatewayOrigin != "" {
		if _, err := access.ParseHTTPSOrigin(c.GatewayOrigin); err != nil {
			return errors.New("agentmodel/config: portal.gateway_origin must be an https origin")
		}
	}
	if _, err := access.NewVerifier(c.Issuer, c.Audience, c.EmailDomain); err != nil {
		return invalid
	}
	seen := map[string]bool{}
	for _, email := range c.AdminEmails {
		normalized := normalizeEmail(email)
		address, err := mail.ParseAddress(normalized)
		if err != nil || address.Address != normalized || seen[normalized] || !strings.HasSuffix(normalized, "@"+strings.ToLower(c.EmailDomain)) {
			return invalid
		}
		seen[normalized] = true
	}
	return nil
}
