package factory

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// docsPath is the user-facing provider reference this guard ties to the code.
const docsPath = "../../../docs/agentmodel.md"

// TestProvidersDocumented ties the "### Providers" tables in docs/agentmodel.md
// to the actual provider wiring in factory.go. It parses the two real sources of
// truth — the `switch name` in buildProvider (native clients) and the
// openaiCompatBaseURLs map (OpenAI-wire vendors) — and asserts every provider
// name appears in the docs, plus that each compat vendor's documented default
// endpoint still matches the one in the map.
//
// Adding a provider without documenting it now fails CI, instead of leaving it
// undiscoverable: xAI/Grok, Groq, Mistral, OpenRouter, Qwen and 11 others were
// all wired and priced but entirely absent from the docs, so the only way to
// learn `provider: xai` worked was to read factory.go.
func TestProvidersDocumented(t *testing.T) {
	docs, err := os.ReadFile(docsPath)
	if err != nil {
		t.Fatalf("read %s: %v", docsPath, err)
	}
	docsText := string(docs)

	native := nativeProviders(t)
	if len(native) == 0 {
		t.Fatal("no providers extracted from the buildProvider switch — parser or dispatch shape changed")
	}
	for _, name := range native {
		if !strings.Contains(docsText, "`"+name+"`") {
			t.Errorf("provider %q is dispatched in buildProvider but missing from the ### Providers section of docs/agentmodel.md", name)
		}
	}

	compat := compatProviders(t)
	if len(compat) == 0 {
		t.Fatal("no entries extracted from openaiCompatBaseURLs — map shape changed")
	}
	for name, baseURL := range compat {
		if !strings.Contains(docsText, "`"+name+"`") {
			t.Errorf("OpenAI-compatible provider %q is wired in openaiCompatBaseURLs but missing from docs/agentmodel.md", name)
		}
		if !strings.Contains(docsText, baseURL) {
			t.Errorf("provider %q: default endpoint %q not found in docs/agentmodel.md (endpoint changed without updating the table?)", name, baseURL)
		}
	}
}

// optionalCapabilities are the provider interfaces a client opts into by
// implementing a method (provider.go: ImageGenerator, VideoGenerator). They are
// the detectable capability surface: Complete/Stream/Embed sit on the base
// Provider interface, so every client has them and method presence proves
// nothing about whether the upstream really serves that modality.
//
// keyword is what the Notes cell must name for the capability to count as
// documented; it is matched case-insensitively so "images" and "image
// generation" both satisfy "image".
var optionalCapabilities = []struct{ method, keyword string }{
	{"GenerateImage", "image"},
	{"SubmitVideo", "video"},
}

// TestProviderCapabilitiesDocumented ties each native provider's optional
// capability interfaces to the Notes column of the "### Providers" table in
// docs/agentmodel.md. TestProvidersDocumented only guards the provider *names*,
// so a client could gain a whole modality and the table would still pass CI
// describing nothing but its auth header — which is exactly what happened: the
// gemini row read "`x-goog-api-key` auth" while gemini was the gateway's only
// video backend (provider/gemini/videos.go) and served images too, so
// /v1/videos/generations had no discoverable provider in the docs at all.
//
// Capability is read from the code, not a hand-maintained list: the buildProvider
// switch maps each provider name to its client package, and the package is parsed
// for the interface methods above.
func TestProviderCapabilitiesDocumented(t *testing.T) {
	docs, err := os.ReadFile(docsPath)
	if err != nil {
		t.Fatalf("read %s: %v", docsPath, err)
	}
	docsText := string(docs)

	pkgs := nativeProviderPackages(t)
	if len(pkgs) == 0 {
		t.Fatal("no provider->package pairs extracted from the buildProvider switch — dispatch shape changed")
	}

	for name, pkg := range pkgs {
		methods := packageMethods(t, pkg)
		for _, cap := range optionalCapabilities {
			if !methods[cap.method] {
				continue
			}
			notes, ok := providerNotesCell(docsText, name)
			if !ok {
				t.Errorf("provider %q implements %s but has no row in the ### Providers table of docs/agentmodel.md", name, cap.method)
				continue
			}
			if !strings.Contains(strings.ToLower(notes), cap.keyword) {
				t.Errorf("provider %q implements %s (provider/%s) but its Notes cell never mentions %q — a reader cannot discover which provider serves that modality.\n  Notes: %s",
					name, cap.method, pkg, cap.keyword, notes)
			}
		}
	}
}

// nativeProviderPackages parses the buildProvider switch and maps each case value
// to the client package it constructs (e.g. "gemini" -> "gemini", and both
// "anthropic" and "anthropic-oauth" -> "anthropic"), by reading the package
// qualifier off the returned constructor call.
func nativeProviderPackages(t *testing.T) map[string]string {
	t.Helper()
	fn := findFunc(t, "buildProvider")

	out := map[string]string{}
	ast.Inspect(fn, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		id, ok := sw.Tag.(*ast.Ident)
		if !ok || id.Name != "name" {
			return true
		}
		for _, stmt := range sw.Body.List {
			cc, ok := stmt.(*ast.CaseClause)
			if !ok {
				continue // the default clause has no List
			}
			pkg := constructedPackage(cc)
			if pkg == "" {
				continue
			}
			for _, e := range cc.List {
				if v, ok := stringLit(e); ok {
					out[v] = pkg
				}
			}
		}
		return false
	})
	return out
}

// constructedPackage returns the package qualifier of the first
// `return <pkg>.New*(...)` inside a case clause.
func constructedPackage(cc *ast.CaseClause) string {
	var pkg string
	ast.Inspect(cc, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || pkg != "" {
			return pkg == ""
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !strings.HasPrefix(sel.Sel.Name, "New") {
			return true
		}
		if x, ok := sel.X.(*ast.Ident); ok {
			pkg = x.Name
		}
		return pkg == ""
	})
	return pkg
}

// packageMethods returns the set of method names declared on the client type in
// services/agentmodel/provider/<pkg>, excluding _test.go files.
func packageMethods(t *testing.T, pkg string) map[string]bool {
	t.Helper()
	dir := filepath.Join("..", "provider", pkg)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read provider package %s: %v", dir, err)
	}

	out := map[string]bool{}
	fset := token.NewFileSet()
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, 0)
		if err != nil {
			t.Fatalf("parse %s/%s: %v", dir, n, err)
		}
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv != nil {
				out[fn.Name.Name] = true
			}
		}
	}
	return out
}

// providerNotesCell finds the "### Providers" table row whose first cell is
// `<name>` and returns its Notes (third) cell.
func providerNotesCell(docsText, name string) (string, bool) {
	want := "`" + name + "`"
	for _, line := range strings.Split(docsText, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		if len(cells) < 3 {
			continue
		}
		if strings.TrimSpace(cells[0]) != want {
			continue
		}
		return strings.TrimSpace(cells[2]), true
	}
	return "", false
}

// nativeProviders parses factory.go and returns the case values of the
// `switch name` inside buildProvider, excluding the default clause.
func nativeProviders(t *testing.T) []string {
	t.Helper()
	fn := findFunc(t, "buildProvider")

	var names []string
	ast.Inspect(fn, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		id, ok := sw.Tag.(*ast.Ident)
		if !ok || id.Name != "name" {
			return true
		}
		for _, stmt := range sw.Body.List {
			cc, ok := stmt.(*ast.CaseClause)
			if !ok {
				continue // the default clause has no List
			}
			for _, e := range cc.List {
				if v, ok := stringLit(e); ok {
					names = append(names, v)
				}
			}
		}
		return false
	})
	return names
}

// compatProviders parses factory.go and returns the openaiCompatBaseURLs map as
// name -> default base URL.
func compatProviders(t *testing.T) map[string]string {
	t.Helper()
	f := parseFactory(t)

	out := map[string]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "openaiCompatBaseURLs" || len(vs.Values) != 1 {
			return true
		}
		lit, ok := vs.Values[0].(*ast.CompositeLit)
		if !ok {
			return false
		}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			k, kok := stringLit(kv.Key)
			v, vok := stringLit(kv.Value)
			if kok && vok {
				out[k] = v
			}
		}
		return false
	})
	return out
}

func parseFactory(t *testing.T) *ast.File {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "factory.go", nil, 0)
	if err != nil {
		t.Fatalf("parse factory.go: %v", err)
	}
	return f
}

func findFunc(t *testing.T, name string) *ast.FuncDecl {
	t.Helper()
	f := parseFactory(t)
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == name {
			return fn
		}
	}
	t.Fatalf("func %s not found in factory.go", name)
	return nil
}

// stringLit unquotes e when it is a string literal.
func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	return v, err == nil
}
