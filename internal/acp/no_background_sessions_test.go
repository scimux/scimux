package acp

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// A protocol session is not a read. dsh flushes every session/new straight to
// ~/.dsh/sessions before it answers, its ACP server offers no deletion, and
// closing the connection leaves the session listable and resumable — so a
// background probe would quietly fill a user's own history with empty chats
// they never started. Only a chat the user asked for may open one, which in
// this package means Session.Launch and the /clear page turn that replaces it.
//
// vibe-acp is the one exception: its catalog probe opens one disposable
// session and must close it before the process is reaped. dsh still has no
// probe. The exception is the function, not a general permission to call
// session/new from discovery.
//
// This is a source-level pin because the defect is invisible at runtime: the
// probe worked, returned a real catalog, and left the litter behind it.
func TestOnlyAUserChatOpensAProtocolSession(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	allowed := map[string]bool{"Launch": true, "Clear": true, "probeVibeCatalog": true}
	var found int
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "NewSession" {
					return true
				}
				found++
				if !allowed[fn.Name.Name] {
					t.Errorf("%s: %s calls NewSession; a dsh session is durable and undeletable, "+
						"so only a chat the user launched may create one", fset.Position(call.Pos()), fn.Name.Name)
				}
				return true
			})
			if fn.Name.Name == "probeVibeCatalog" && !funcCalls(fn, "CloseSession") {
				t.Error("probeVibeCatalog calls NewSession without CloseSession")
			}
			if fn.Name.Name == "probeVibeCatalog" && funcCalls(fn, "Prompt") {
				t.Error("probeVibeCatalog calls session/prompt")
			}
		}
	}
	if found == 0 {
		t.Fatal("found no NewSession call at all — the guard is scanning the wrong thing")
	}
}

func funcCalls(fn *ast.FuncDecl, name string) bool {
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if ok && sel.Sel.Name == name {
			found = true
		}
		return true
	})
	return found
}
