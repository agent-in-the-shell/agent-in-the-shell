package agentmodel

import (
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPortalHTMLDirConfig(t *testing.T) {
	for _, tc := range []struct {
		dir   string
		valid bool
	}{
		{"", true},
		{filepath.Join(t.TempDir(), "not-created"), true},
		{"public", false},
		{"../portal", false},
		{"/tmp/portal\x00", false},
	} {
		c := PortalConfig{Enabled: true, Origin: "https://portal.example.com", Issuer: "https://team.cloudflareaccess.com", Audience: "aud", EmailDomain: "example.com"}
		data, err := yaml.Marshal(map[string]string{"html_dir": tc.dir})
		if err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal(data, &c); err != nil {
			t.Fatal(err)
		}
		if err := c.Validate(); (err == nil) != tc.valid {
			t.Errorf("html_dir %q: valid=%v, error=%v", tc.dir, tc.valid, err)
		}
	}
}

func TestPortalConfigFailClosed(t *testing.T) {
	good := PortalConfig{Enabled: true, Origin: "https://portal.example.com", Issuer: "https://team.cloudflareaccess.com", Audience: "aud", EmailDomain: "example.com", AdminEmails: []string{"Admin@example.com"}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*PortalConfig)
	}{
		{"origin", func(c *PortalConfig) { c.Origin = "" }}, {"issuer", func(c *PortalConfig) { c.Issuer = "" }}, {"audience", func(c *PortalConfig) { c.Audience = "" }}, {"domain", func(c *PortalConfig) { c.EmailDomain = "" }}, {"invalid admin", func(c *PortalConfig) { c.AdminEmails = []string{"not-an-email"} }}, {"empty admin", func(c *PortalConfig) { c.AdminEmails = []string{""} }}, {"external admin", func(c *PortalConfig) { c.AdminEmails = []string{"admin@evil.com"} }}, {"duplicate admin", func(c *PortalConfig) { c.AdminEmails = []string{"Admin@example.com", "admin@example.com"} }}, {"origin path", func(c *PortalConfig) { c.Origin += "/path" }}, {"origin http", func(c *PortalConfig) { c.Origin = "http://portal.example.com" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := good
			tc.mutate(&c)
			if c.Validate() == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	if err := (PortalConfig{}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, email := range []string{"admin@example.com", " ADMIN@example.com "} {
		if !good.IsAdmin(email) {
			t.Fatalf("normalized admin rejected: %q", email)
		}
	}
	for _, email := range []string{"", "other@example.com", "admin@example.com.evil", "prefixadmin@example.com"} {
		if good.IsAdmin(email) {
			t.Fatalf("non-admin accepted: %q", email)
		}
	}
	good.AdminEmails = nil
	if err := good.Validate(); err != nil || good.IsAdmin("admin@example.com") {
		t.Fatal("empty admin list must disable administration", err)
	}
}
