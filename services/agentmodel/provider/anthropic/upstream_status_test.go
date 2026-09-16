package anthropic

import (
	"context"
	"net/http"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
)

// statusError already knows the upstream status; classification must use it
// rather than re-deriving it from the body it also carries.
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
			body:    `{"type":"error","error":{"type":"api_error","message":"internal"},"request_id":"req_401abc"}`,
			wantTyp: agentmodel.ErrTypeUpstream,
			wantRet: true,
		},
		{
			name:    "429 whose retry hint contains 400",
			status:  http.StatusTooManyRequests,
			body:    `{"type":"error","error":{"type":"rate_limit_error","message":"retry in 400ms"}}`,
			wantTyp: agentmodel.ErrTypeRateLimit,
			wantRet: true,
		},
		{
			// Anthropic's documented overload status, with a body that says
			// nothing recognizable. Only the status identifies it.
			name:    "529 overloaded with an opaque body",
			status:  529,
			body:    `{"type":"error","error":{"type":"api_error"}}`,
			wantTyp: agentmodel.ErrTypeServiceUnavailable,
			wantRet: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := newFakeAnthropic(t, c.body)
			fake.respStatus = c.status

			_, err := NewWithBaseURL(newAPIKeyAuth(), fake.srv.URL).Complete(context.Background(), agentmodel.ChatRequest{
				Model:    "claude-sonnet-4-5",
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

// ListModels does not build a statusError — it keeps its own "list-models HTTP"
// wording — so implementing the interface on statusError leaves it uncovered.
// It feeds the router's model refresh and classifies through Wrap, so a 500
// whose body quotes "400ms" would report a transient outage as a terminal
// invalid_request_error.
func TestListModels_UpstreamStatusSurvivesCollidingBody(t *testing.T) {
	fake := newFakeAnthropic(t, `{"type":"error","error":{"message":"upstream closed after 400ms"}}`)
	fake.respStatus = http.StatusInternalServerError

	_, err := NewWithBaseURL(newAPIKeyAuth(), fake.srv.URL).ListModels(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	ae := agentmodel.Wrap(err)
	if ae.Type != agentmodel.ErrTypeUpstream {
		t.Errorf("Type = %q, want %q (err=%v)", ae.Type, agentmodel.ErrTypeUpstream, err)
	}
	if !ae.Retryable() {
		t.Error("Retryable() = false, want true")
	}
}
