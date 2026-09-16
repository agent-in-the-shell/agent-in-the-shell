package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/wire"
)

// newPromptServer returns a gateway stub that records the last decoded chat
// request and serves the given handler's response.
func newPromptServer(t *testing.T, handler func(w http.ResponseWriter)) (*httptest.Server, *wire.ChatRequest) {
	t.Helper()
	var got wire.ChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		handler(w)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func okReply(text string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		resp := wire.ChatResponse{
			Model:   "m-served",
			Choices: []wire.Choice{{Message: wire.Message{Role: "assistant", Content: text}}},
			Usage:   wire.Usage{PromptTokens: 3, CompletionTokens: 1},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// TestPromptPrintsReply: `agent-model prompt "<text>"` sends the text as the
// user message, prints the assistant reply to stdout, and puts the debug
// facts (model, latency, tokens) on stderr where they don't corrupt a pipe.
func TestPromptPrintsReply(t *testing.T) {
	srv, got := newPromptServer(t, okReply("pong"))
	t.Setenv("AGENT_MODEL_URL", srv.URL)
	t.Setenv("AGENT_MODEL_TOKEN", "")

	var out, errOut bytes.Buffer
	if err := doPrompt([]string{"--model", "m", "say pong"}, &out, &errOut); err != nil {
		t.Fatalf("doPrompt: %v", err)
	}
	if out.String() != "pong\n" {
		t.Errorf("stdout = %q, want %q", out.String(), "pong\n")
	}
	if got.Model != "m" {
		t.Errorf("request model = %q, want %q", got.Model, "m")
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != "user" || got.Messages[0].Content != "say pong" {
		t.Errorf("request messages = %+v, want one user message %q", got.Messages, "say pong")
	}
	diag := errOut.String()
	for _, want := range []string{"model=m-served", "latency=", "tokens=3+1"} {
		if !strings.Contains(diag, want) {
			t.Errorf("stderr diagnostics %q missing %q", diag, want)
		}
	}
}

// TestPromptDefaultPing: with no positional argument the command still probes
// the gateway, using the built-in liveness prompt.
func TestPromptDefaultPing(t *testing.T) {
	srv, got := newPromptServer(t, okReply("pong"))
	t.Setenv("AGENT_MODEL_URL", srv.URL)

	var out, errOut bytes.Buffer
	if err := doPrompt(nil, &out, &errOut); err != nil {
		t.Fatalf("doPrompt: %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Content != defaultPromptText {
		t.Errorf("request messages = %+v, want the default liveness prompt %q", got.Messages, defaultPromptText)
	}
}

// TestPromptGatewayError: a gateway error surfaces its structured type and
// message in the returned error (and hence a non-zero exit).
func TestPromptGatewayError(t *testing.T) {
	srv, _ := newPromptServer(t, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"type":"upstream_error","message":"boom"}}`))
	})
	t.Setenv("AGENT_MODEL_URL", srv.URL)

	var out, errOut bytes.Buffer
	err := doPrompt(nil, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "upstream_error") {
		t.Fatalf("err = %v, want the gateway's upstream_error surfaced", err)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty on failure", out.String())
	}
}

// TestPromptEmptyChoices: a 200 with no choices is not proof the model is
// online — it must fail rather than print nothing and exit 0.
func TestPromptEmptyChoices(t *testing.T) {
	srv, _ := newPromptServer(t, func(w http.ResponseWriter) {
		_ = json.NewEncoder(w).Encode(wire.ChatResponse{})
	})
	t.Setenv("AGENT_MODEL_URL", srv.URL)

	var out, errOut bytes.Buffer
	err := doPrompt(nil, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "no choices") {
		t.Fatalf("err = %v, want a no-choices failure", err)
	}
}

// TestPromptBadFlag: unknown flags fail parse instead of being swallowed.
func TestPromptBadFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := doPrompt([]string{"--bogus"}, &out, &errOut); err == nil {
		t.Fatal("want flag parse error for --bogus")
	}
}
