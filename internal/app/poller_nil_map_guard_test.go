package app

// Packet 10 Stage B: the poll path must not carry lazy nil-map guards. Every
// map field is initialized in newApp via initMaps; tests that reach poll()
// call the same helper on their fixtures instead of making production ship a
// branch that can only fire in a test.
//
// The check is AST-shaped, not comment-shaped, so rewording the comment does
// not smuggle the pattern back in. It matches the exact leak: an `if a.X ==
// nil` whose body does nothing but assign an empty map to that same field.
// Behavioral short-circuits that return a value (sessionSnapshot's bare-server
// path) allocate nothing and are not this pattern.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// nilMapGuardFiles are the production files on the poll path. Scoped
// deliberately: lazy initialization is legitimate elsewhere (the usage cache,
// the claude-model probe), and this test speaks only for poll-path state that
// newApp already builds.
var nilMapGuardFiles = []string{"poller.go", "conversation_api.go"}

func TestPollPathHasNoLazyNilMapGuards(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	fset := token.NewFileSet()
	var hits []string
	for _, name := range nilMapGuardFiles {
		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			stmt, isIf := n.(*ast.IfStmt)
			if !isIf {
				return true
			}
			field, isNilCheck := nilCheckedSelector(stmt.Cond)
			if !isNilCheck {
				return true
			}
			if !bodyOnlyAssignsEmptyMap(stmt.Body, field) {
				return true
			}
			hits = append(hits, name+":"+
				strconv.Itoa(fset.Position(stmt.Pos()).Line)+" if "+field+" == nil { "+field+" = map[…]…{} }")
			return true
		})
	}
	if len(hits) > 0 {
		t.Fatalf("poll-path files reintroduced lazy nil-map guard(s):\n  %s\n"+
			"initialize the field in initMaps and call a.initMaps() in the fixture instead",
			strings.Join(hits, "\n  "))
	}
}

// nilCheckedSelector reports the rendered `x.Field` of an `x.Field == nil`
// condition.
func nilCheckedSelector(cond ast.Expr) (string, bool) {
	bin, ok := cond.(*ast.BinaryExpr)
	if !ok || bin.Op != token.EQL {
		return "", false
	}
	ident, ok := bin.Y.(*ast.Ident)
	if !ok || ident.Name != "nil" {
		return "", false
	}
	return renderSelector(bin.X)
}

// bodyOnlyAssignsEmptyMap reports whether the block is exactly one assignment
// of an empty map composite literal to field.
func bodyOnlyAssignsEmptyMap(body *ast.BlockStmt, field string) bool {
	if body == nil || len(body.List) != 1 {
		return false
	}
	assign, ok := body.List[0].(*ast.AssignStmt)
	if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
		return false
	}
	lhs, ok := renderSelector(assign.Lhs[0])
	if !ok || lhs != field {
		return false
	}
	lit, ok := assign.Rhs[0].(*ast.CompositeLit)
	if !ok || len(lit.Elts) != 0 {
		return false
	}
	_, isMap := lit.Type.(*ast.MapType)
	return isMap
}

// renderSelector renders `x.Field` selector expressions over a plain
// identifier; anything more complex is not the shape this test polices.
func renderSelector(e ast.Expr) (string, bool) {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	base, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	return base.Name + "." + sel.Sel.Name, true
}
