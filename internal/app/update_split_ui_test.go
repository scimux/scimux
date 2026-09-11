package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

func TestUpdateUIPromisesWebHandoffWithoutHarnessRestart(t *testing.T) {
	index, err := os.ReadFile("../../web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	entry, err := os.ReadFile("../../web/js/app.js")
	if err != nil {
		t.Fatal(err)
	}
	all := string(index) + string(entry)
	for _, want := range []string{"Update web interface", "Running agent sessions stay connected to the muxer."} {
		if !strings.Contains(all, want) {
			t.Errorf("update UI missing %q", want)
		}
	}
	for _, stale := range []string{"agents are stopped by the restart", "Update &amp; restart"} {
		if strings.Contains(all, stale) {
			t.Errorf("update UI still claims %q", stale)
		}
	}
}

func TestWebUpdateCannotShutdownHarnesses(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "update.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var apply *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "handleUpdateApply" {
			apply = fn
			break
		}
	}
	if apply == nil {
		t.Fatal("handleUpdateApply not found")
	}
	prepared := false
	ast.Inspect(apply.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if x.Sel.Name == "Shutdown" {
				t.Errorf("web update contains a harness Shutdown call at %s", fset.Position(x.Pos()))
			}
		case *ast.Ident:
			if x.Name == "prepareWebUpdate" {
				prepared = true
			}
		}
		return true
	})
	if !prepared {
		t.Fatal("web update no longer prepares a replacement generation")
	}
}
