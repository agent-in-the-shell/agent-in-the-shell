package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestCLICommandsDocumented ties the CLI docs to the actual command dispatch.
// It parses the `switch os.Args[1]` in main.go — the real source of truth for
// which subcommands exist — and asserts every dispatched command appears both
// in the `--help` text (the `usage` const) and in the "## Commands" block of
// docs/agentmodel.md. Adding or removing a command without updating either
// fails CI, instead of letting the two hand-maintained lists drift (they had:
// `--help` was missing `schema`, the docs were missing `usage`).
func TestCLICommandsDocumented(t *testing.T) {
	cmds := dispatchedCommands(t)
	if len(cmds) == 0 {
		t.Fatal("no commands extracted from the os.Args[1] switch — parser or dispatch shape changed")
	}

	docsText := commandsBlock(t, "../../docs/agentmodel.md")

	for _, c := range cmds {
		needle := "agent-model " + c
		if !strings.Contains(usage, needle) {
			t.Errorf("command %q is dispatched but missing from the --help text (usage const in main.go)", c)
		}
		if !strings.Contains(docsText, needle) {
			t.Errorf("command %q is dispatched but missing from the ## Commands block in docs/agentmodel.md", c)
		}
	}
}

// commandsBlock returns the fenced code block that follows the "## Commands"
// heading — the list this test actually claims to guard.
//
// It used to search the whole file, which quietly stopped checking anything the
// moment prose elsewhere in the doc mentioned a command by name: the `limits`
// entry could be deleted from the block and CI would still pass, because
// "agent-model limits" also appears in a usage example further down. Scoping to
// the structure rather than to a substring is the same lesson  records
// after this family of guard was defeated twice the same way.
func commandsBlock(t *testing.T, path string) string {
	t.Helper()
	docs, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	_, after, ok := strings.Cut(string(docs), "\n## Commands\n")
	if !ok {
		t.Fatalf("%s has no '## Commands' heading — the doc's structure changed", path)
	}
	_, after, ok = strings.Cut(after, "```\n")
	if !ok {
		t.Fatalf("%s: no fenced block opens after '## Commands'", path)
	}
	block, _, ok := strings.Cut(after, "```")
	if !ok {
		t.Fatalf("%s: the block after '## Commands' is never closed", path)
	}
	return block
}

// dispatchedCommands parses main.go and returns the case values of the
// top-level `switch os.Args[1]`, excluding the help aliases.
func dispatchedCommands(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	skip := map[string]bool{"-h": true, "--help": true, "help": true}
	var cmds []string
	ast.Inspect(f, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok || !isOsArgsIndex(sw.Tag, "1") {
			return true
		}
		for _, stmt := range sw.Body.List {
			cc, ok := stmt.(*ast.CaseClause)
			if !ok {
				continue // the default clause has no List
			}
			for _, e := range cc.List {
				lit, ok := e.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err == nil && !skip[v] {
					cmds = append(cmds, v)
				}
			}
		}
		return false // the matched dispatch switch has no nested switches to find
	})
	return cmds
}

// isOsArgsIndex reports whether e is the expression `os.Args[<idx>]`.
func isOsArgsIndex(e ast.Expr, idx string) bool {
	ix, ok := e.(*ast.IndexExpr)
	if !ok {
		return false
	}
	sel, ok := ix.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "os" || sel.Sel.Name != "Args" {
		return false
	}
	lit, ok := ix.Index.(*ast.BasicLit)
	return ok && lit.Kind == token.INT && lit.Value == idx
}
