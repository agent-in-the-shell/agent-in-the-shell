package provider

import (
	"errors"
	"fmt"
	"strings"
)

// Registry maps provider name (e.g. "openai", "anthropic", "anthropic-oauth",
// "gemini", "chatgpt") to a configured Provider. Same provider may appear
// multiple times under different keys when multiple auth modes are wired.
type Registry struct {
	providers   map[string]Provider
	defaultName string
}

// ErrProviderNotFound is returned when no entry matches the resolved key.
var ErrProviderNotFound = errors.New("provider: provider not found in registry")

// NewRegistry builds a Registry from the supplied entries. The
// lexicographically-first provider name is the default for unprefixed model
// names — deterministic, since Go map iteration order is randomized and would
// otherwise pick a different default across process starts. Pass WithDefault to
// override.
func NewRegistry(entries map[string]Provider) *Registry {
	r := &Registry{providers: make(map[string]Provider, len(entries))}
	for name, p := range entries {
		r.providers[name] = p
		if r.defaultName == "" || name < r.defaultName {
			r.defaultName = name
		}
	}
	return r
}

// Option mutates a Registry during NewRegistry call (functional option pattern).
type Option func(*Registry)

// WithDefault sets the provider used when a model name has no provider prefix.
func WithDefault(name string) Option {
	return func(r *Registry) { r.defaultName = name }
}

// NewRegistryWithOptions is the variadic-options sibling of NewRegistry.
func NewRegistryWithOptions(entries map[string]Provider, opts ...Option) *Registry {
	r := NewRegistry(entries)
	for _, o := range opts {
		o(r)
	}
	return r
}

// Resolve looks up the right Provider for model and returns the bare model
// name to pass through. Resolution rules:
//
//  1. "provider/model-name" — split on first "/", look up provider by prefix.
//  2. "model-name" without slash — use the registry's default provider.
//  3. Unknown prefix or empty default — return ErrProviderNotFound.
//
// Examples:
//
//	openai/gpt-4o            -> (openai provider, "gpt-4o")
//	anthropic-oauth/...      -> (anthropic-oauth provider, "...")
//	gpt-4o                   -> (default provider, "gpt-4o")
func (r *Registry) Resolve(model string) (Provider, string, error) {
	if model == "" {
		return nil, "", fmt.Errorf("%w: empty model name", ErrProviderNotFound)
	}

	if i := strings.Index(model, "/"); i > 0 {
		prefix, rest := model[:i], model[i+1:]
		if p, ok := r.providers[prefix]; ok {
			if rest == "" {
				return nil, "", fmt.Errorf("%w: empty model after prefix %q", ErrProviderNotFound, prefix)
			}
			return p, rest, nil
		}
		// Unknown prefix; fall through to default if configured.
	}

	if r.defaultName != "" {
		if p, ok := r.providers[r.defaultName]; ok {
			return p, model, nil
		}
	}

	return nil, "", fmt.Errorf("%w: no provider for model %q", ErrProviderNotFound, model)
}

// Get returns the provider with the given name, or false if not registered.
func (r *Registry) Get(name string) (Provider, bool) {
	p, ok := r.providers[name]
	return p, ok
}

// Names returns the registered provider names.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.providers))
	for k := range r.providers {
		out = append(out, k)
	}
	return out
}
