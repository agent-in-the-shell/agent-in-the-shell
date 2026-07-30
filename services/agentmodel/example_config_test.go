package agentmodel

import "testing"

// TestExampleConfigLoads guards the quick-start: the shipped example config
// must load and validate as-is, because the public README tells users to copy
// it and start the service. (The first release shipped an example that failed
// validation — subscription deployments with an empty oauth_token_dir.)
func TestExampleConfigLoads(t *testing.T) {
	if _, err := LoadConfig("../../cmd/agent-model/example_config.yaml"); err != nil {
		t.Fatalf("example config does not load: %v", err)
	}
}
