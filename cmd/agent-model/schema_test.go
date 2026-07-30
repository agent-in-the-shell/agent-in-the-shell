package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/internal/wirecontract"
)

// TestEmitSchema asserts `agent-model schema` honestly self-describes the
// `filter` passthrough: both sides require `text` and are open records, because
// filter rewrites `text` but preserves every other field of each input record.
func TestEmitSchema(t *testing.T) {
	var buf bytes.Buffer
	if err := emitSchema(&buf); err != nil {
		t.Fatal(err)
	}
	var doc wirecontract.Document
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("emitted invalid JSON: %v", err)
	}
	if doc.InputSchema == nil || doc.OutputSchema == nil {
		t.Fatal("filter is a transform: both input and output schemas required")
	}
	for name, s := range map[string]*wirecontract.JSONSchema{"input": doc.InputSchema, "output": doc.OutputSchema} {
		if !s.AdditionalProperties {
			t.Errorf("%s schema must be open (passthrough preserves other fields): %+v", name, s)
		}
		if s.Properties["text"].Type != "string" {
			t.Errorf("%s schema must declare text:string, got %+v", name, s.Properties)
		}
		if len(s.Required) != 1 || s.Required[0] != "text" {
			t.Errorf("%s required = %v, want [text]", name, s.Required)
		}
	}
}
