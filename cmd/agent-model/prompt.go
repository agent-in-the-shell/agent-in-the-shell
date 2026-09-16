package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/gateway"
)

// defaultPromptText is the liveness probe sent when no prompt argument is
// given: one short deterministic instruction, so the round trip is cheap.
const defaultPromptText = "Reply with the single word: pong"

// doPrompt is the `agent-model prompt` debug command: one chat completion
// through the gateway to prove the whole chain (server, auth, routing,
// provider credential) is alive. The reply text goes to stdout; the debug
// facts (served model, latency, token counts) go to stderr so stdout stays
// clean, and the exit code answers "is the model online".
func doPrompt(args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("prompt", flag.ContinueOnError)
	fs.SetOutput(errOut)
	model := fs.String("model", "", "model name (default $AGENT_MODEL_MODEL, then the gateway client default)")
	timeout := fs.Duration("timeout", 60*time.Second, "request timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	prompt := defaultPromptText
	if fs.NArg() > 0 {
		prompt = fs.Arg(0)
	}

	client := gateway.NewFromEnv(*model)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	start := time.Now()
	resp, err := client.Complete(ctx, gateway.ChatRequest{
		Messages: []gateway.Message{{Role: "user", Content: prompt}},
	})
	if err != nil {
		return err
	}
	if len(resp.Choices) == 0 {
		return fmt.Errorf("no choices in response")
	}
	fmt.Fprintln(out, resp.Choices[0].Message.Content)
	fmt.Fprintf(errOut, "model=%s latency=%s tokens=%d+%d\n",
		resp.Model, time.Since(start).Round(time.Millisecond),
		resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	return nil
}
