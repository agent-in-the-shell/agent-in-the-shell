package auth

import (
	"context"
	"net/http"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
)

// StaticKey applies a fixed bearer token / API key via a configured header.
//
// Examples:
//
//	&StaticKey{HeaderName: "Authorization", Prefix: "Bearer ", Token: "sk-..."}
//	&StaticKey{HeaderName: "x-api-key",     Prefix: "",        Token: "sk-ant-api03-..."}
//	&StaticKey{HeaderName: "x-goog-api-key", Prefix: "",       Token: "AIza..."}
type StaticKey struct {
	HeaderName string
	Prefix     string
	Token      string
}

func (s *StaticKey) Mode() string { return agentmodel.AuthModeAPIKey }

func (s *StaticKey) Apply(_ context.Context, req *http.Request) error {
	req.Header.Set(s.HeaderName, s.Prefix+s.Token)
	return nil
}
