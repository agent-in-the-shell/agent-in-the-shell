package main

import (
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
)

// TestExampleConfigDocumentsAllFields keeps example_config.yaml honest: every
// yaml field on agentmodel.Config (recursively) must appear in the example, so
// adding a config field without showing it there fails CI instead of leaving
// the user-facing example silently behind. Companion to the API-table guard in
// services/agentmodel/api/docs_endpoints_test.go.
//
// Presence-only by design: the example may show a field commented out, and a
// field is matched as a yaml key (start-of-token) so "token_env" is not
// satisfied by "bearer_token_env". This guards the common drift — a new field
// nobody documented — not exact placement.
func TestExampleConfigDocumentsAllFields(t *testing.T) {
	data, err := os.ReadFile("example_config.yaml")
	if err != nil {
		t.Fatalf("read example_config.yaml: %v", err)
	}
	example := string(data)

	tags := map[string]bool{}
	collectYAMLTags(reflect.TypeOf(agentmodel.Config{}), map[reflect.Type]bool{}, tags)
	if len(tags) == 0 {
		t.Fatal("no yaml tags discovered on agentmodel.Config — reflection broke")
	}

	for _, name := range sortedKeys(tags) {
		re := regexp.MustCompile(`(^|[\s#])` + regexp.QuoteMeta(name) + `:`)
		if !re.MatchString(example) {
			t.Errorf("config field %q (yaml) is not shown in example_config.yaml — document it", name)
		}
	}
}

// collectYAMLTags gathers every yaml field name reachable from t, descending
// through nested structs, pointers, and slice element types.
func collectYAMLTags(t reflect.Type, seen map[reflect.Type]bool, out map[string]bool) {
	for t.Kind() == reflect.Ptr || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || seen[t] {
		return
	}
	seen[t] = true
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		if name := strings.Split(tag, ",")[0]; name != "" {
			out[name] = true
		}
		collectYAMLTags(f.Type, seen, out)
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
