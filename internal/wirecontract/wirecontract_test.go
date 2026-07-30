package wirecontract

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"testing"
	"time"
)

type sampleRec struct {
	ID         string          `json:"id"`
	Title      string          `json:"title"`
	Summary    string          `json:"summary,omitempty"`
	Count      int             `json:"count"`
	OK         bool            `json:"ok"`
	Tags       []string        `json:"tags,omitempty"`
	When       time.Time       `json:"when"`
	Raw        json.RawMessage `json:"raw,omitempty"`
	Ptr        *string         `json:"ptr,omitempty"`
	Hidden     string          `json:"-"`
	unexported string
}

func TestReflectTypesAndRequired(t *testing.T) {
	s := Reflect(sampleRec{})
	if s.Type != "object" {
		t.Fatalf("type = %q", s.Type)
	}
	wantTypes := map[string]string{
		"id": "string", "title": "string", "summary": "string",
		"count": "integer", "ok": "boolean", "tags": "array",
		"when": "string", "raw": "object", "ptr": "string",
	}
	for name, want := range wantTypes {
		if got := s.Properties[name].Type; got != want {
			t.Errorf("property %q type = %q, want %q", name, got, want)
		}
	}
	if _, ok := s.Properties["-"]; ok {
		t.Error("json:\"-\" field must be skipped")
	}
	if _, ok := s.Properties["unexported"]; ok {
		t.Error("unexported field must be skipped")
	}
	// Required = non-omitempty, non-pointer fields.
	want := []string{"count", "id", "ok", "title", "when"}
	got := append([]string(nil), s.Required...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("required = %v, want %v", got, want)
	}
}

func TestEmitProducerOnly(t *testing.T) {
	var buf bytes.Buffer
	if err := Emit(&buf, nil, sampleRec{}); err != nil {
		t.Fatal(err)
	}
	var doc Document
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("emitted invalid JSON: %v", err)
	}
	if doc.InputSchema != nil {
		t.Error("input schema should be omitted for a producer-only stage")
	}
	if doc.OutputSchema == nil || doc.OutputSchema.Properties["title"].Type != "string" {
		t.Fatalf("output schema not emitted correctly: %+v", doc.OutputSchema)
	}
}

func TestEmitTransform(t *testing.T) {
	type txt struct {
		Text string `json:"text"`
	}
	var buf bytes.Buffer
	if err := Emit(&buf, txt{}, txt{}); err != nil {
		t.Fatal(err)
	}
	var doc Document
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.InputSchema == nil || doc.OutputSchema == nil {
		t.Fatal("transform must emit both input and output schemas")
	}
	if len(doc.InputSchema.Required) != 1 || doc.InputSchema.Required[0] != "text" {
		t.Fatalf("input required = %v, want [text]", doc.InputSchema.Required)
	}
}

func TestReflectIsClosedByDefault(t *testing.T) {
	// A reflected schema makes no additionalProperties claim — the field is
	// omitted (false) so a struct-shaped record stays a closed, exact shape.
	s := Reflect(sampleRec{})
	if s.AdditionalProperties {
		t.Fatal("Reflect must not set AdditionalProperties")
	}
	var buf bytes.Buffer
	if err := Emit(&buf, nil, sampleRec{}); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(buf.Bytes(), []byte("additionalProperties")) {
		t.Fatalf("closed schema must omit additionalProperties, got: %s", buf.String())
	}
}

func TestOpenMarksAdditionalProperties(t *testing.T) {
	if Open(nil) != nil {
		t.Fatal("Open(nil) must return nil")
	}
	type txt struct {
		Text string `json:"text"`
	}
	rec := Open(Reflect(txt{}))
	if !rec.AdditionalProperties {
		t.Fatal("Open must set AdditionalProperties")
	}
	// A passthrough transform declares the same open record on both sides.
	var buf bytes.Buffer
	if err := EmitDocument(&buf, Document{InputSchema: rec, OutputSchema: rec}); err != nil {
		t.Fatal(err)
	}
	var doc Document
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("emitted invalid JSON: %v", err)
	}
	if doc.InputSchema == nil || !doc.InputSchema.AdditionalProperties {
		t.Errorf("input schema must be open: %+v", doc.InputSchema)
	}
	if doc.OutputSchema == nil || !doc.OutputSchema.AdditionalProperties {
		t.Errorf("output schema must be open: %+v", doc.OutputSchema)
	}
	// additionalProperties must serialize as the JSON boolean true.
	if !bytes.Contains(buf.Bytes(), []byte(`"additionalProperties": true`)) {
		t.Fatalf("expected additionalProperties:true in output, got: %s", buf.String())
	}
}
