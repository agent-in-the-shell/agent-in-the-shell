package factory

import (
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
	"gopkg.in/yaml.v3"
)

// DiscoverySource is one unique configured upstream connection that can list
// its live model catalog. Template is safe to copy when adding a model: its
// model-specific fields are cleared by DiscoverSources.
type DiscoverySource struct {
	Name     string
	Template agentmodel.DeploymentConfig
	Lister   provider.ModelLister
}

// DiscoverSources builds one model-listing source per unique configured
// provider connection. Repeated deployments commonly differ only by model id;
// those collapse into a single source so interactive configuration does not
// query the same account once per model.
func DiscoverSources(cfg *agentmodel.Config, logger *slog.Logger) ([]DiscoverySource, []string, error) {
	seen := make(map[string]bool)
	var sources []DiscoverySource
	var unsupported []string
	var shared *auth.ChatGPTOAuth

	for _, model := range cfg.ModelList {
		for _, deployment := range model.Deployments {
			template := deployment
			template.Model = ""
			template.DeploymentName = ""
			identity := template
			// Routing policy is not part of an upstream connection's identity.
			// Preserve it on Template, but do not query the same account again
			// merely because two configured models use different limits/weights.
			identity.Weight = nil
			identity.RPM = nil
			identity.TPM = nil
			identity.CacheTTL = ""
			keyBytes, err := yaml.Marshal(identity)
			if err != nil {
				return nil, nil, fmt.Errorf("encode discovery source: %w", err)
			}
			key := string(keyBytes)
			if seen[key] {
				continue
			}
			seen[key] = true

			p, chatgptAuth, err := buildDeploymentProvider(deployment, model.ModelName, logger, shared)
			if err != nil {
				return nil, nil, err
			}
			if chatgptAuth != nil {
				shared = chatgptAuth
			}
			name := discoverySourceName(deployment)
			if p == nil {
				unsupported = append(unsupported, name+": credentials unavailable")
				continue
			}
			lister, ok := p.(provider.ModelLister)
			if !ok {
				unsupported = append(unsupported, name+": provider does not support model discovery")
				continue
			}
			sources = append(sources, DiscoverySource{Name: name, Template: template, Lister: lister})
		}
	}

	sort.Slice(sources, func(i, j int) bool { return sources[i].Name < sources[j].Name })
	sort.Strings(unsupported)
	return sources, unsupported, nil
}

func discoverySourceName(d agentmodel.DeploymentConfig) string {
	name := d.Provider + "/" + d.AuthMode
	if d.BaseURL != "" {
		host := d.BaseURL
		if parsed, err := url.Parse(d.BaseURL); err == nil && parsed.Host != "" {
			host = parsed.Host
		}
		name += "@" + host
	}
	var credential string
	switch {
	case len(d.APIKeyEnvs) > 0:
		credential = strings.Join(d.APIKeyEnvs, ",")
	case d.APIKeyEnv != "":
		credential = d.APIKeyEnv
	case len(d.OAuthTokenDirs) > 0:
		dirs := make([]string, len(d.OAuthTokenDirs))
		for i, dir := range d.OAuthTokenDirs {
			dirs[i] = filepath.Base(dir)
		}
		credential = strings.Join(dirs, ",")
	case d.OAuthTokenDir != "":
		credential = filepath.Base(d.OAuthTokenDir)
	}
	if credential != "" {
		name += " [" + credential + "]"
	}
	return name
}
