package openai

import "testing"

// Azure mode (#892) rewrites OpenAI-relative paths to the deployment-scoped
// layout and appends the required api-version query; standard mode is untouched.
func TestResolveURL(t *testing.T) {
	std := NewWithBaseURL(nil, "https://api.openai.com/v1")
	if got := std.resolveURL("/chat/completions"); got != "https://api.openai.com/v1/chat/completions" {
		t.Errorf("standard /chat/completions = %q", got)
	}

	az := NewAzure(nil, "https://res.openai.azure.com/", "2024-10-01-preview", "gpt4o-deploy")
	cases := map[string]string{
		"/chat/completions": "https://res.openai.azure.com/openai/deployments/gpt4o-deploy/chat/completions?api-version=2024-10-01-preview",
		"/embeddings":       "https://res.openai.azure.com/openai/deployments/gpt4o-deploy/embeddings?api-version=2024-10-01-preview",
		"/models":           "https://res.openai.azure.com/openai/models?api-version=2024-10-01-preview",
	}
	for path, want := range cases {
		if got := az.resolveURL(path); got != want {
			t.Errorf("azure resolveURL(%q) = %q, want %q", path, got, want)
		}
	}
}
