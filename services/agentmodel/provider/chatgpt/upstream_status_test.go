package chatgpt

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
)

// apiError already knows the upstream status; classification must use it rather
// than re-deriving it from the body Error() interpolates.
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
			body:    `{"error":{"message":"internal","type":"server_error"},"request_id":"req_401abc"}`,
			wantTyp: agentmodel.ErrTypeUpstream,
			wantRet: true,
		},
		{
			name:    "502 whose message quotes a 400ms timeout",
			status:  http.StatusBadGateway,
			body:    `{"error":{"message":"upstream closed after 400ms","type":"server_error"}}`,
			wantTyp: agentmodel.ErrTypeUpstream,
			wantRet: true,
		},
		{
			name:    "401 genuinely bad credential stays terminal",
			status:  http.StatusUnauthorized,
			body:    `{"error":{"message":"invalid token"}}`,
			wantTyp: agentmodel.ErrTypeAuthentication,
			wantRet: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()

			_, err := NewWithBaseURL(freshAuth(t), srv.URL).Complete(context.Background(), agentmodel.ChatRequest{
				Model:    "gpt-5",
				Messages: []agentmodel.Message{{Role: "user", Content: "hi"}},
			})
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
