package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
)

func doFilter(args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("filter", flag.ContinueOnError)
	defaultURL := os.Getenv("AGENT_MODEL_URL")
	if defaultURL == "" {
		defaultURL = "http://localhost:8090"
	}
	serverURL := fs.String("url", defaultURL, "agent-model server URL (default $AGENT_MODEL_URL or :8090)")
	model := fs.String("model", "claude-sonnet-4-6", "model name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: agent-model filter [--url=...] [--model=...] <system-prompt>")
	}
	systemPrompt := fs.Arg(0)
	token := os.Getenv("AGENT_MODEL_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "warning: AGENT_MODEL_TOKEN is unset; requests will likely fail with 401")
	}

	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 1<<20), 1<<20) // allow lines up to 1 MB
	enc := json.NewEncoder(out)
	for scanner.Scan() {
		var rec map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			log.Printf("filter: unmarshal: %v", err)
			continue
		}
		var text string
		if err := json.Unmarshal(rec["text"], &text); err != nil {
			log.Printf("filter: text field: %v", err)
			continue
		}
		rewritten, err := callFilterModel(*serverURL, token, *model, systemPrompt, text)
		if err != nil {
			log.Printf("filter: model call: %v", err)
			continue
		}
		rec["text"], _ = json.Marshal(rewritten)
		if err := enc.Encode(rec); err != nil {
			return fmt.Errorf("write stdout: %w", err)
		}
	}
	return scanner.Err()
}

func callFilterModel(serverURL, token, model, systemPrompt, text string) (string, error) {
	body, _ := json.Marshal(agentmodel.ChatRequest{
		Model: model,
		Messages: []agentmodel.Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: text},
		},
	})
	req, _ := http.NewRequest(http.MethodPost, serverURL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("model server: HTTP %d", resp.StatusCode)
	}
	var cr agentmodel.ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return "", err
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("no choices in response")
	}
	return cr.Choices[0].Message.Content, nil
}
