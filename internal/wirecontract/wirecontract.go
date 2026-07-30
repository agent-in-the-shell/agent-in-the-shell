// Package wirecontract defines the self-description a pipe-able agent CLI emits
// from its `schema` subcommand: an MCP-style {inputSchema, outputSchema}
// document describing the JSONL records it consumes and produces. It is emitted
// by 8 live producers as the typed hand-off contract between pipe stages; the
// former plan-time consumer inside agent-run was removed with the pipeline
// engine (#939), so today the schema is a lint/inspection surface (`<cmd>
// schema`) rather than something resolved automatically at run time.
package wirecontract

import (
	"encoding/json"
	"io"
	"reflect"
	"sort"
	"strings"
	"time"
)

// Property is a single JSON Schema property (one level deep — agent-run's model
// is flat, so nested objects collapse to type "object").
type Property struct {
	Type string `json:"type"` // string|number|integer|boolean|array|object|null
}

// JSONSchema is a minimal object schema for one record shape.
type JSONSchema struct {
	Type       string              `json:"type"`
	Properties map[string]Property `json:"properties,omitempty"`
	Required   []string            `json:"required,omitempty"`
	// AdditionalProperties, when true, declares the record open: it carries the
	// listed properties plus arbitrary passthrough fields. A flat passthrough
	// transform (e.g. agent-model filter, agent-post publish) sets this so a
	// downstream consumer/compiler knows upstream fields survive the stage,
	// rather than assuming the record is exactly the declared shape.
	AdditionalProperties bool `json:"additionalProperties,omitempty"`
}

// Document is the value a `<cmd> schema` subcommand prints. A producer-only
// stage sets OutputSchema; a consumer-only stage sets InputSchema; a transform
// sets both.
type Document struct {
	InputSchema  *JSONSchema `json:"inputSchema,omitempty"`
	OutputSchema *JSONSchema `json:"outputSchema,omitempty"`
}

// Emit reflects the given record structs into a Document and writes it as
// indented JSON. Pass nil for in or out to omit that side.
func Emit(w io.Writer, in, out any) error {
	doc := Document{}
	if in != nil {
		doc.InputSchema = Reflect(in)
	}
	if out != nil {
		doc.OutputSchema = Reflect(out)
	}
	return EmitDocument(w, doc)
}

// EmitDocument writes a pre-built Document as indented JSON. Use this (with Open
// and/or hand-built schemas) when a stage's real contract cannot be captured by
// struct reflection alone — e.g. a passthrough transform whose records carry
// arbitrary additional fields beyond the declared ones.
func EmitDocument(w io.Writer, doc Document) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// Open marks a schema as accepting arbitrary additional properties and returns
// it, for chaining: Open(Reflect(rec{})). Use it for a passthrough record that
// carries the declared fields plus unknown upstream fields. A nil schema is
// returned unchanged.
func Open(s *JSONSchema) *JSONSchema {
	if s != nil {
		s.AdditionalProperties = true
	}
	return s
}

var (
	timeType = reflect.TypeOf(time.Time{})
	rawType  = reflect.TypeOf(json.RawMessage(nil))
)

// Reflect builds a one-level JSON Schema from a struct value. Fields use their
// json tag name; a non-pointer field without `omitempty` is required. The
// schema is derived from the Go wire type, so it cannot drift from the code.
func Reflect(v any) *JSONSchema {
	t := reflect.TypeOf(v)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return &JSONSchema{Type: "object"}
	}
	s := &JSONSchema{Type: "object", Properties: map[string]Property{}}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		s.Properties[name] = Property{Type: schemaType(f.Type)}
		if !strings.Contains(opts, "omitempty") && f.Type.Kind() != reflect.Pointer {
			s.Required = append(s.Required, name)
		}
	}
	sort.Strings(s.Required)
	return s
}

func schemaType(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == timeType {
		return "string"
	}
	if t == rawType {
		return "object"
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.Map, reflect.Struct:
		return "object"
	default:
		return "string"
	}
}
