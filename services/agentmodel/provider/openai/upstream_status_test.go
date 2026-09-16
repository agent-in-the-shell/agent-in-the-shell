package openai

import (
	"context"
	"net/http"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
)

// A transient upstream failure must stay retryable even when its body contains
// digits that collide with a status needle. Classifying this as an auth error
// both lies to the caller and stops the router's fallback walk, because
// authentication_error is terminal.
//
// This adapter is composed by azure, deepseek and every openaicompat vendor, so
// the status it reports covers all of them.
func TestComplete_UpstreamStatusSurvivesCollidingBody(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantTyp string
		wantRet bool
	}{
		{
			name:    "500 whose request id contains 401",
			status:  http.StatusInternalServerError,
			body:    `{"error":{"message":"The server had an error","type":"server_error"},"request_id":"req_c401e9ab"}`,
			wantTyp: agentmodel.ErrTypeUpstream,
			wantRet: true,
		},
		{
			name:    "502 whose message quotes a 400ms timeout",
			status:  http.StatusBadGateway,
			body:    `{"error":{"message":"backend timed out after 400ms","type":"server_error"}}`,
			wantTyp: agentmodel.ErrTypeUpstream,
			wantRet: true,
		},
		{
			name:    "400 context window whose token count contains 403",
			status:  http.StatusBadRequest,
			body:    `{"error":{"message":"This model's maximum context length is 128000 tokens, however you requested 129403 tokens","code":"context_length_exceeded"}}`,
			wantTyp: agentmodel.ErrTypeContextWindow,
			wantRet: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := statusServer(c.status, c.body)
			defer srv.Close()

			_, err := newClient(t, srv, "sk-test").Complete(context.Background(), newChatReq())
			if err == nil {
				t.Fatal("expected an error")
			}
			ae := agentmodel.Wrap(err)
			if ae.Type != c.wantTyp {
				t.Errorf("Type = %q, want %q (err=%v)", ae.Type, c.wantTyp, err)
			}
			if ae.Retryable() != c.wantRet {
				t.Errorf("Retryable() = %v, want %v", ae.Retryable(), c.wantRet)
			}
		})
	}
}
