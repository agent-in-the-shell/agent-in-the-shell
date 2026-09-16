package pool_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/provider/pool"
)

type pooledResponsesProvider struct {
	nonPassthrough
	calls atomic.Int32
	code  int
	body  string
}

func (p *pooledResponsesProvider) ResponsesPassthrough(context.Context, []byte, string) (*http.Response, error) {
	p.calls.Add(1)
	return &http.Response{StatusCode: p.code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(p.body))}, nil
}

func TestPool_ResponsesPassthroughForwardsAndRotatesOn429(t *testing.T) {
	limited := &pooledResponsesProvider{code: http.StatusTooManyRequests}
	winner := &pooledResponsesProvider{code: http.StatusOK, body: "stream"}
	p := pool.New([]provider.Provider{limited, winner})

	resp, err := p.ResponsesPassthrough(context.Background(), []byte(`{"tools":[{"type":"web_search"}]}`), "gpt")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "stream" || limited.calls.Load() != 1 || winner.calls.Load() != 1 {
		t.Fatalf("body=%q calls=%d/%d", body, limited.calls.Load(), winner.calls.Load())
	}
}

func TestPool_ResponsesPassthroughNoCapableProvider(t *testing.T) {
	p := pool.New([]provider.Provider{&nonPassthrough{}})
	_, err := p.ResponsesPassthrough(context.Background(), nil, "gpt")
	if err != provider.ErrNotSupported {
		t.Fatalf("err=%v, want ErrNotSupported", err)
	}
}
