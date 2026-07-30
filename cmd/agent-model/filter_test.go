package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
)

func TestDoFilterReplacesText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(agentmodel.ChatResponse{ //nolint:errcheck
			Choices: []agentmodel.Choice{{Message: agentmodel.Message{Content: "rewritten text"}}},
		})
	}))
	defer srv.Close()

	input := `{"text":"original","channel":"slack","chat_id":"C012"}` + "\n"
	var out strings.Builder
	if err := doFilter([]string{"--url=" + srv.URL, "rewrite for social"}, strings.NewReader(input), &out); err != nil {
		t.Fatalf("doFilter: %v", err)
	}

	var rec map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &rec); err != nil {
		t.Fatalf("unmarshal output: %v\nraw: %s", err, out.String())
	}
	var text string
	json.Unmarshal(rec["text"], &text) //nolint:errcheck
	if text != "rewritten text" {
		t.Errorf("text = %q, want rewritten text", text)
	}
	var channel string
	json.Unmarshal(rec["channel"], &channel) //nolint:errcheck
	if channel != "slack" {
		t.Errorf("channel = %q, want slack (pass-through broken)", channel)
	}
}

func TestDoFilterSkipsOnModelError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":"overloaded"}`) //nolint:errcheck
	}))
	defer srv.Close()

	input := `{"text":"first","channel":"slack"}` + "\n" +
		`{"text":"second","channel":"slack"}` + "\n"
	var out strings.Builder
	doFilter([]string{"--url=" + srv.URL, "rewrite"}, strings.NewReader(input), &out) //nolint:errcheck
	if strings.TrimSpace(out.String()) != "" {
		t.Errorf("expected empty output on model errors, got: %s", out.String())
	}
}

// TestDoFilterSkipsOnDecodeError verifies a malformed (non-JSON) model response
// body causes the record to be skipped, not propagated.
func TestDoFilterSkipsOnDecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `this is not json`) //nolint:errcheck
	}))
	defer srv.Close()

	input := `{"text":"original","channel":"slack"}` + "\n"
	var out strings.Builder
	if err := doFilter([]string{"--url=" + srv.URL, "rewrite"}, strings.NewReader(input), &out); err != nil {
		t.Fatalf("doFilter should not surface per-record decode errors: %v", err)
	}
	if strings.TrimSpace(out.String()) != "" {
		t.Errorf("expected empty output on decode error, got: %s", out.String())
	}
}

// TestDoFilterSkipsOnEmptyChoices verifies a 200 response with zero choices is
// treated as an error and the record skipped.
func TestDoFilterSkipsOnEmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(agentmodel.ChatResponse{Choices: nil}) //nolint:errcheck
	}))
	defer srv.Close()

	input := `{"text":"original","channel":"slack"}` + "\n"
	var out strings.Builder
	if err := doFilter([]string{"--url=" + srv.URL, "rewrite"}, strings.NewReader(input), &out); err != nil {
		t.Fatalf("doFilter should not surface empty-choices errors: %v", err)
	}
	if strings.TrimSpace(out.String()) != "" {
		t.Errorf("expected empty output on empty choices, got: %s", out.String())
	}
}

// TestDoFilterSkipsRecordsWithoutTextField verifies records missing the text
// field are skipped (the model is never called).
func TestDoFilterSkipsRecordsWithoutTextField(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(agentmodel.ChatResponse{ //nolint:errcheck
			Choices: []agentmodel.Choice{{Message: agentmodel.Message{Content: "x"}}},
		})
	}))
	defer srv.Close()

	input := `{"channel":"slack"}` + "\n" // no "text" field
	var out strings.Builder
	if err := doFilter([]string{"--url=" + srv.URL, "rewrite"}, strings.NewReader(input), &out); err != nil {
		t.Fatalf("doFilter: %v", err)
	}
	if called {
		t.Error("model should not be called for a record lacking a text field")
	}
	if strings.TrimSpace(out.String()) != "" {
		t.Errorf("expected no output for text-less record, got: %s", out.String())
	}
}

// TestDoFilterUnknownFlag verifies flag-parse errors propagate as a returned error.
func TestDoFilterUnknownFlag(t *testing.T) {
	var out strings.Builder
	if err := doFilter([]string{"--nope"}, strings.NewReader(""), &out); err == nil {
		t.Fatal("expected error from unknown flag")
	}
}

// TestDoFilterMissingSystemPrompt verifies the usage error when no positional arg.
func TestDoFilterMissingSystemPrompt(t *testing.T) {
	var out strings.Builder
	if err := doFilter([]string{"--url=http://x"}, strings.NewReader(""), &out); err == nil {
		t.Fatal("expected usage error when system-prompt arg is missing")
	}
}

// TestCallFilterModelEmptyChoices exercises callFilterModel's no-choices branch directly.
func TestCallFilterModelEmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(agentmodel.ChatResponse{Choices: nil}) //nolint:errcheck
	}))
	defer srv.Close()

	_, err := callFilterModel(srv.URL, "tok", "m", "sys", "text")
	if err == nil {
		t.Fatal("expected 'no choices' error")
	}
	if !strings.Contains(err.Error(), "no choices") {
		t.Errorf("error = %v, want 'no choices'", err)
	}
}

// TestCallFilterModelNon200 exercises callFilterModel's HTTP-status branch directly.
func TestCallFilterModelNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	_, err := callFilterModel(srv.URL, "tok", "m", "sys", "text")
	if err == nil {
		t.Fatal("expected HTTP-status error")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error = %v, want HTTP 403", err)
	}
}
