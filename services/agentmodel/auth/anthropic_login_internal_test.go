package auth

import "testing"

func TestPKCEChallenge_Deterministic(t *testing.T) {
	// Same verifier must always produce the same challenge.
	v := "test-verifier-determinism-check"
	c1 := pkceChallenge(v)
	c2 := pkceChallenge(v)
	if c1 != c2 {
		t.Errorf("pkceChallenge is not deterministic: %q != %q", c1, c2)
	}
	if c1 == "" || c1 == v {
		t.Errorf("challenge must be a non-empty hash of the verifier, got %q", c1)
	}
}

func TestParseCallbackCode(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{"bare code", "abc123", "abc123", false},
		{"code with leading/trailing spaces", "  abc123  ", "abc123", false},
		{"code#state fragment", "abc123#mystate", "abc123", false},
		{"code#state with spaces", "  abc123#mystate  ", "abc123", false},
		{"full http URL", "http://localhost:12345/callback?code=abc123&state=xyz", "abc123", false},
		{"full https URL (hosted callback)", "https://console.anthropic.com/oauth/code/callback?code=abc123&state=xyz", "abc123", false},
		{"empty string", "", "", true},
		{"only hash no code", "#state-only", "", true},
		{"URL missing code param", "http://localhost:12345/callback?state=xyz", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCallbackCode(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Errorf("parseCallbackCode(%q): want error, got %q", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Errorf("parseCallbackCode(%q): unexpected error: %v", tc.input, err)
				return
			}
			if got != tc.want {
				t.Errorf("parseCallbackCode(%q): got %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
