package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestRouterHandleRegistrationsAreAudited accounts for every production
// mux.Handle registration. The frozen suite counts HandleFunc only. Exactly
// one additional method, path, and handler is permitted: POST
// /api/harnesses/latest served by handleHarnessLatest. A new Handle anywhere
// in the application package fails this test.
func TestRouterHandleRegistrationsAreAudited(t *testing.T) {
	got := parseProductionHandleRegistrations(t)
	want := []handleRegistration{
		{"backend_ownership.go", "newWebMux", "GET /assets", "RedirectHandler"},
		{"backend_ownership.go", "newWebMux", "GET /assets/", "assets"},
		{"backend_ownership.go", "newWebMux", "GET /css/", "css"},
		{"backend_ownership.go", "newWebMux", "GET /js/", "js"},
		{"backend_ownership.go", "newWebMux", "GET /{$}", "index"},
		{"backend_ownership.go", "newWebMux", "/api/remote", "NotFoundHandler"},
		{"backend_ownership.go", "newWebMux", "/api/remote/", "NotFoundHandler"},
		{"backend_ownership.go", "newWebMux", "/api/", "core"},
		{"router.go", "newMux", "GET /assets", "RedirectHandler"},
		{"router.go", "newMux", "GET /assets/", "assets"},
		{"router.go", "newMux", "GET /css/", "css"},
		{"router.go", "newMux", "GET /js/", "js"},
		{"router.go", "newMux", "GET /{$}", "index"},
		{"router.go", "newMux", "POST /api/harnesses/latest", "handleHarnessLatest"},
	}
	if len(got) != len(want) {
		t.Fatalf("Handle registrations = %d, want %d\n%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Handle[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	funcs := map[string]string{}
	for _, reg := range parseRouterAPIRegistrations(t) {
		funcs[reg.method+" "+reg.pattern] = reg.handler
	}
	extras := 0
	for _, reg := range got {
		method, pattern, ok := strings.Cut(reg.pattern, " ")
		if !ok || !strings.HasPrefix(pattern, "/api/") {
			continue
		}
		key := method + " " + pattern
		if _, listed := funcs[key]; listed {
			t.Errorf("Handle %s duplicates a HandleFunc registration", key)
		}
		extras++
		if key != "POST /api/harnesses/latest" || reg.handler != "handleHarnessLatest" || reg.fn != "newMux" {
			t.Errorf("untracked method route %+v", reg)
		}
	}
	if extras != 1 {
		t.Fatalf("additional method/path/handler count = %d, want 1", extras)
	}
	if funcs["GET /api/harnesses/latest"] != "handleHarnessLatest" {
		t.Fatalf("GET /api/harnesses/latest handler = %q, want handleHarnessLatest", funcs["GET /api/harnesses/latest"])
	}

	src, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	if strings.Contains(text, "not a HandleFunc registration") || strings.Contains(text, "inventory bypass") {
		t.Fatal("POST /api/harnesses/latest is still described as an inventory bypass")
	}
	if !strings.Contains(text, "audited Handle exception") {
		t.Fatal("router.go does not describe the audited Handle exception")
	}
}

type handleRegistration struct {
	file, fn, pattern, handler string
}

func parseProductionHandleRegistrations(t *testing.T) []handleRegistration {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var out []handleRegistration
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Handle" || len(call.Args) != 2 {
					return true
				}
				lit, ok := call.Args[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Errorf("%s: Handle pattern is not a string literal", name)
					return true
				}
				out = append(out, handleRegistration{
					file:    name,
					fn:      fn.Name.Name,
					pattern: strings.Trim(lit.Value, `"`),
					handler: handleTargetName(call.Args[1]),
				})
				return true
			})
		}
	}
	return out
}

func handleTargetName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	case *ast.CallExpr:
		sel, ok := e.Fun.(*ast.SelectorExpr)
		if !ok {
			return ""
		}
		if sel.Sel.Name == "HandlerFunc" && len(e.Args) == 1 {
			return handleTargetName(e.Args[0])
		}
		return sel.Sel.Name
	default:
		return ""
	}
}
