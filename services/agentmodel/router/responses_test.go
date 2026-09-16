package router_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
)

type responsesProvider struct {
	nonLister
	name  string
	calls atomic.Int32
	fn    func(context.Context, []byte, string) (*http.Response, error)
}

func (p *responsesProvider) Name() string {
	if p.name != "" {
		return p.name
	}
	return "responses"
}
func (p *responsesProvider) ResponsesPassthrough(ctx context.Context, body []byte, model string) (*http.Response, error) {
	p.calls.Add(1)
	return p.fn(ctx, body, model)
}

func responsesReply(status int, body string) func(context.Context, []byte, string) (*http.Response, error) {
	return func(context.Context, []byte, string) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	}
}

func TestResponsesPassthrough_RetriesOnlyBeforeStream(t *testing.T) {
	limited := &responsesProvider{fn: responsesReply(http.StatusTooManyRequests, `{}`)}
	winner := &responsesProvider{fn: responsesReply(http.StatusOK, "data: ok\n\n")}
	r := newRouter(map[string][]router.Deployment{"m": {
		{Name: "limited", Provider: limited, Model: "limited-up", Weight: 1000},
		{Name: "winner", Provider: winner, Model: "winner-up", Weight: 1},
	}}, nil)

	resp, dep, served, err := r.ResponsesPassthroughBlocked(context.Background(), []byte(`{"model":"m"}`), "m", nil)
	if err != nil {
		t.Fatalf("ResponsesPassthroughBlocked: %v", err)
	}
	resp.Body.Close()
	if dep.Name != "winner" || served != "m" || limited.calls.Load() != 1 || winner.calls.Load() != 1 {
		t.Fatalf("dep=%q served=%q calls=%d/%d", dep.Name, served, limited.calls.Load(), winner.calls.Load())
	}

	// Once a provider returns a 2xx response, ownership of the live body moves
	// to the handler. A later body error must never re-dispatch the hosted tool.
	streamErr := errors.New("stream dropped")
	first := &responsesProvider{fn: func(context.Context, []byte, string) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: &errorBody{err: streamErr}}, nil
	}}
	never := &responsesProvider{fn: responsesReply(http.StatusOK, "duplicate")}
	r = newRouter(map[string][]router.Deployment{"m": {
		{Name: "first", Provider: first, Model: "first-up", Weight: 1000},
		{Name: "never", Provider: never, Model: "never-up", Weight: 1},
	}}, nil)
	resp, _, _, err = r.ResponsesPassthroughBlocked(context.Background(), nil, "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !errors.Is(err, streamErr) || never.calls.Load() != 0 {
		t.Fatalf("read err=%v second calls=%d, want stream error and no retry", err, never.calls.Load())
	}
}

type errorBody struct{ err error }

func (b *errorBody) Read([]byte) (int, error) { return 0, b.err }
func (b *errorBody) Close() error             { return nil }

func TestResponsesPassthrough_UnsupportedBlockedAndTPM(t *testing.T) {
	t.Run("unsupported", func(t *testing.T) {
		r := newRouter(map[string][]router.Deployment{"m": {{Provider: nonLister{}, Model: "up", Weight: 1}}}, nil)
		_, _, _, err := r.ResponsesPassthroughBlocked(context.Background(), nil, "m", nil)
		if err == nil || agentmodel.Wrap(err).Type != agentmodel.ErrTypeInvalidRequest {
			t.Fatalf("err=%v, want terminal invalid request", err)
		}
	})

	t.Run("blocked fallback", func(t *testing.T) {
		limited := &responsesProvider{fn: responsesReply(http.StatusTooManyRequests, `{}`)}
		fallback := &responsesProvider{fn: responsesReply(http.StatusOK, "ok")}
		r := newRouter(map[string][]router.Deployment{
			"primary":  {{Name: "primary", Provider: limited, Model: "p", Weight: 1}},
			"fallback": {{Name: "fallback", Provider: fallback, Model: "f", Weight: 1}},
		}, map[string][]string{"primary": {"fallback"}})
		_, _, _, err := r.ResponsesPassthroughBlocked(context.Background(), nil, "primary", []string{"fallback"})
		if err == nil || fallback.calls.Load() != 0 {
			t.Fatalf("err=%v fallback calls=%d", err, fallback.calls.Load())
		}
	})

	t.Run("tpm", func(t *testing.T) {
		p := &responsesProvider{fn: responsesReply(http.StatusOK, "ok")}
		r := newMeteredRouter(map[string][]router.Deployment{"m": {{Name: "d", Provider: p, Model: "up", Weight: 1, TPM: intPtr(6)}}}, nil)
		resp, dep, served, err := r.ResponsesPassthroughBlocked(context.Background(), nil, "m", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		r.RecordTokens(served, dep.Name, 6)
		_, _, _, err = r.ResponsesPassthroughBlocked(context.Background(), nil, "m", nil)
		if err == nil || agentmodel.Wrap(err).Type != agentmodel.ErrTypeRateLimit {
			t.Fatalf("err=%v, want tpm rate limit", err)
		}
	})
}

var _ provider.ResponsesPassthroughProvider = (*responsesProvider)(nil)
