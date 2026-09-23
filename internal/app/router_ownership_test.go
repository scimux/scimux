package app

// Packet 4F: source/AST-backed ownership audit for the /api/... routes.
// Extends characterizationAPIRoutes() rather than inventing a second inventory.
// Scans only production Go sources in the application package (never examples/).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestRouterAPIRouteOwnership locks the Phase 4 boundary: every registered API
// route has exactly one production handler definition in its expected feature
// file, router.go stays registration-only, and the local boundary still
// composes withGzip(withRequestBoundary(policy, guardMutations(mux))).
//
// S3 R2 split router.go into newMux (the single owned route table) and
// withLocalBoundary (the unchanged browser chain) so both S3 boundaries can
// wrap one mux without a second inventory. The assertions below were moved to
// follow that split, deliberately and without weakening: the registrations are
// still counted and order-compared one-for-one against the shared table, the
// three middleware calls are still counted (now across the whole file, so a
// second chain anywhere in router.go fails), and the composition is still
// matched structurally down to the mux identifier.
func TestRouterAPIRouteOwnership(t *testing.T) {
	table := characterizationAPIRoutes()
	if len(table) != 46 {
		t.Fatalf("characterization table has %d routes, want 46", len(table))
	}

	// Table-internal uniqueness: each method+pattern pair appears once.
	seenPattern := map[string]string{}
	for _, r := range table {
		key := r.method + " " + r.pattern
		if prev, ok := seenPattern[key]; ok {
			t.Errorf("duplicate table entry %q (also handler %s)", key, prev)
		}
		seenPattern[key] = r.handler
		if r.handler == "" || r.home == "" {
			t.Errorf("route %q missing handler/home", key)
		}
	}

	regs := parseRouterAPIRegistrations(t)
	if len(regs) != len(table) {
		t.Fatalf("router.go HandleFunc count = %d, want %d (table size)", len(regs), len(table))
	}

	// Compare registration order and content against the shared table.
	for i, want := range table {
		got := regs[i]
		wantKey := want.method + " " + want.pattern
		gotKey := got.method + " " + got.pattern
		if gotKey != wantKey {
			t.Errorf("registration[%d] = %q, want %q", i, gotKey, wantKey)
			continue
		}
		if got.handler != want.handler {
			t.Errorf("%s handler = %q, want %q", wantKey, got.handler, want.handler)
		}
	}

	// Also detect extras if table is a subset somehow (order already compared).
	regByKey := map[string]routerAPIReg{}
	for _, r := range regs {
		key := r.method + " " + r.pattern
		if prev, ok := regByKey[key]; ok {
			t.Errorf("duplicate router registration %q (handlers %s and %s)", key, prev.handler, r.handler)
		}
		regByKey[key] = r
	}
	for _, want := range table {
		key := want.method + " " + want.pattern
		if _, ok := regByKey[key]; !ok {
			t.Errorf("router missing registration %q -> %s", key, want.handler)
		}
	}

	// Single production definition home per handler symbol.
	defs := productionHandlerDefs(t)
	wantHome := map[string]string{}
	for _, r := range table {
		if prev, ok := wantHome[r.handler]; ok && prev != r.home {
			t.Errorf("handler %s claimed by both %s and %s in table", r.handler, prev, r.home)
		}
		wantHome[r.handler] = r.home
	}
	for name, home := range wantHome {
		files := defs[name]
		if len(files) == 0 {
			t.Errorf("handler %s has no production definition (expected in %s)", name, home)
			continue
		}
		if len(files) != 1 {
			t.Errorf("handler %s has %d production definitions %v, want exactly one in %s", name, len(files), files, home)
			continue
		}
		if files[0] != home {
			t.Errorf("handler %s defined in %s, want %s", name, files[0], home)
		}
	}

	// Confirmed accepted / retained homes still exist as production files.
	for _, home := range []string{
		"state_api.go", "node_api.go", "conversation_api.go",
		"attachment_api.go", "ui_state_api.go", "security.go",
		"usage.go", "notes.go", "search.go", "preview.go", "agents.go", "update.go",
		"remote_pairing.go",
	} {
		if _, err := os.Stat(home); err != nil {
			t.Errorf("expected home %s: %v", home, err)
		}
	}

	assertRouterRegistrationOnly(t)
	assertNewHandlerReturnsWithGzipOnce(t)
	assertMainGoCompositionOnly(t)
	assertNoStalePhase4Comments(t)
}

type routerAPIReg struct {
	method  string
	pattern string
	handler string
}

func parseRouterAPIRegistrations(t *testing.T) []routerAPIReg {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "router.go", nil, 0)
	if err != nil {
		t.Fatalf("parse router.go: %v", err)
	}
	registrar := findFunc(f, "newMux")
	if registrar == nil || registrar.Body == nil {
		t.Fatal("router.go: newMux not found (the single owned route table)")
	}

	var regs []routerAPIReg
	ast.Inspect(registrar.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "HandleFunc" {
			return true
		}
		if len(call.Args) != 2 {
			t.Errorf("HandleFunc with %d args, want 2", len(call.Args))
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			t.Errorf("HandleFunc first arg is not a string literal: %T", call.Args[0])
			return true
		}
		// Unquote "METHOD /path" form used by Go 1.22+ ServeMux.
		raw := strings.Trim(lit.Value, `"`)
		method, pattern, ok := strings.Cut(raw, " ")
		if !ok {
			t.Errorf("HandleFunc pattern %q missing method separator", raw)
			return true
		}
		handler := handlerName(call.Args[1])
		if handler == "" {
			t.Errorf("could not resolve handler for %s %s (%T)", method, pattern, call.Args[1])
			return true
		}
		regs = append(regs, routerAPIReg{method: method, pattern: pattern, handler: handler})
		return true
	})
	return regs
}

func handlerName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	default:
		return ""
	}
}

func findFunc(f *ast.File, name string) *ast.FuncDecl {
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if ok && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

// productionHandlerDefs maps handler symbol -> defining basenames among
// production (non-test) application-package sources. The package directory is
// deliberately the scan boundary, so nested packages are never inspected.
func productionHandlerDefs(t *testing.T) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir .: %v", err)
	}
	fset := token.NewFileSet()
	out := map[string][]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// Defense in depth: never open anything under a path separator.
		if strings.Contains(name, string(filepath.Separator)) {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		if f.Name.Name != "app" {
			continue
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			out[fn.Name.Name] = append(out[fn.Name.Name], name)
		}
	}
	for name, files := range out {
		sort.Strings(files)
		out[name] = files
	}
	return out
}

func assertRouterRegistrationOnly(t *testing.T) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "router.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse router.go: %v", err)
	}
	var funcs []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		funcs = append(funcs, fn.Name.Name)
	}
	sort.Strings(funcs)
	want := []string{"NewHandler", "newMux", "withLocalBoundary"}
	sort.Strings(want)
	if strings.Join(funcs, ",") != strings.Join(want, ",") {
		t.Fatalf("router.go funcs = %v, want exactly %v (registration/construction only)", funcs, want)
	}

	// Forbidden behavioral calls anywhere in router.go — feature logic must
	// live in feature files, not the router. Scanned over every function in
	// the file, so moving code out of NewHandler cannot dodge the check.
	forbidden := map[string]bool{
		"http.Error": true, "writeJSON": true, "decodeJSON": true,
		"json.NewEncoder": true, "json.NewDecoder": true,
		"os.WriteFile": true, "os.ReadFile": true,
	}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := callExprName(call.Fun)
		if forbidden[name] {
			t.Errorf("router.go calls %s (handler behavior, not registration)", name)
		}
		// Direct feature-handler invocations (not as HandleFunc args) would
		// look like a bare Ident/Selector used as a CallExpr.Fun.
		if id, ok := call.Fun.(*ast.Ident); ok && strings.HasPrefix(id.Name, "handle") {
			t.Errorf("router.go invokes %s as a call (must only register)", id.Name)
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && strings.HasPrefix(sel.Sel.Name, "handle") {
			// mux.HandleFunc(..., a.handleX) is SelectorExpr as an *arg*, not Fun.
			// Fun being a.handleX would mean invoking the handler.
			t.Errorf("router.go invokes %s as a call (must only register)", sel.Sel.Name)
		}
		return true
	})
}

func callExprName(fun ast.Expr) string {
	switch e := fun.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		if id, ok := e.X.(*ast.Ident); ok {
			return id.Name + "." + e.Sel.Name
		}
		return e.Sel.Name
	default:
		return ""
	}
}

// assertNewHandlerReturnsWithGzipOnce locks the middleware order:
// withGzip(withRequestBoundary(policy, guardMutations(mux))). Compression is
// outermost; the request boundary wraps the mutation guard so Host validation
// covers the entire public handler; the mutation guard still wraps the mux
// exactly once.
func assertNewHandlerReturnsWithGzipOnce(t *testing.T) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "router.go", nil, 0)
	if err != nil {
		t.Fatalf("parse router.go: %v", err)
	}
	fn := findFunc(f, "withLocalBoundary")
	if fn == nil || fn.Body == nil {
		t.Fatal("withLocalBoundary missing")
	}

	// Count each middleware call across the whole file, so a second chain
	// anywhere in router.go — not just inside one function — fails.
	counts := map[string]int{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok {
			switch id.Name {
			case "guardMutations", "withGzip", "withRequestBoundary":
				counts[id.Name]++
			}
		}
		return true
	})
	if counts["guardMutations"] != 1 {
		t.Fatalf("router.go contains %d guardMutations calls, want exactly 1", counts["guardMutations"])
	}
	if counts["withGzip"] != 1 {
		t.Fatalf("router.go contains %d withGzip calls, want exactly 1", counts["withGzip"])
	}
	if counts["withRequestBoundary"] != 1 {
		t.Fatalf("router.go contains %d withRequestBoundary calls, want exactly 1", counts["withRequestBoundary"])
	}

	// NewHandler must still return the local boundary over the owned mux, and
	// nothing else: withLocalBoundary(a, mux), nil.
	assertNewHandlerDelegatesToLocalBoundary(t, f)

	// withLocalBoundary's return must be
	// withGzip(withRequestBoundary(..., guardMutations(mux))).
	stmts := fn.Body.List
	if len(stmts) == 0 {
		t.Fatal("withLocalBoundary body empty")
	}
	ret, ok := stmts[len(stmts)-1].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		t.Fatalf("withLocalBoundary final statement is not a single-value return: %T", stmts[len(stmts)-1])
	}
	outer, ok := ret.Results[0].(*ast.CallExpr)
	if !ok {
		t.Fatalf("withLocalBoundary returns %T, want withGzip(...)", ret.Results[0])
	}
	outerID, ok := outer.Fun.(*ast.Ident)
	if !ok || outerID.Name != "withGzip" {
		t.Fatalf("withLocalBoundary return wrapper = %s, want withGzip", callExprName(outer.Fun))
	}
	if len(outer.Args) != 1 {
		t.Fatalf("withGzip args = %d, want 1", len(outer.Args))
	}
	mid, ok := outer.Args[0].(*ast.CallExpr)
	if !ok {
		t.Fatalf("withGzip argument = %T, want withRequestBoundary(...)", outer.Args[0])
	}
	midID, ok := mid.Fun.(*ast.Ident)
	if !ok || midID.Name != "withRequestBoundary" {
		t.Fatalf("inner wrapper = %s, want withRequestBoundary", callExprName(mid.Fun))
	}
	if len(mid.Args) != 2 {
		t.Fatalf("withRequestBoundary args = %d, want 2 (policy, guardMutations(mux))", len(mid.Args))
	}
	inner, ok := mid.Args[1].(*ast.CallExpr)
	if !ok {
		t.Fatalf("withRequestBoundary second arg = %T, want guardMutations(...)", mid.Args[1])
	}
	innerID, ok := inner.Fun.(*ast.Ident)
	if !ok || innerID.Name != "guardMutations" {
		t.Fatalf("guard wrapper = %s, want guardMutations", callExprName(inner.Fun))
	}
	if len(inner.Args) != 1 {
		t.Fatalf("guardMutations args = %d, want 1 (the mux)", len(inner.Args))
	}
	muxID, ok := inner.Args[0].(*ast.Ident)
	if !ok || muxID.Name != "mux" {
		t.Fatalf("guardMutations argument = %T, want mux", inner.Args[0])
	}
}

// assertNewHandlerDelegatesToLocalBoundary pins the other half of the S3
// split: NewHandler builds the owned mux and hands it to the local boundary,
// so the public handler is still exactly the chain asserted above.
func assertNewHandlerDelegatesToLocalBoundary(t *testing.T, f *ast.File) {
	t.Helper()
	fn := findFunc(f, "NewHandler")
	if fn == nil || fn.Body == nil {
		t.Fatal("NewHandler missing")
	}
	stmts := fn.Body.List
	if len(stmts) == 0 {
		t.Fatal("NewHandler body empty")
	}
	ret, ok := stmts[len(stmts)-1].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 2 {
		t.Fatalf("NewHandler final statement is not a two-value return: %T", stmts[len(stmts)-1])
	}
	call, ok := ret.Results[0].(*ast.CallExpr)
	if !ok {
		t.Fatalf("NewHandler returns %T, want withLocalBoundary(...)", ret.Results[0])
	}
	id, ok := call.Fun.(*ast.Ident)
	if !ok || id.Name != "withLocalBoundary" {
		t.Fatalf("NewHandler return wrapper = %s, want withLocalBoundary", callExprName(call.Fun))
	}
	if len(call.Args) != 2 {
		t.Fatalf("withLocalBoundary args = %d, want 2 (app, mux)", len(call.Args))
	}
	if muxID, ok := call.Args[1].(*ast.Ident); !ok || muxID.Name != "mux" {
		t.Fatalf("withLocalBoundary second argument = %T, want mux", call.Args[1])
	}
	if nilID, ok := ret.Results[1].(*ast.Ident); !ok || nilID.Name != "nil" {
		t.Fatalf("NewHandler second return = %T, want nil", ret.Results[1])
	}
}

// assertMainGoCompositionOnly fails if main.go regains route bodies, store
// replay, poll transitions, or state-projection implementations.
func assertMainGoCompositionOnly(t *testing.T) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	forbiddenNames := map[string]bool{
		// Store / lifecycle bodies (Phase 2 homes).
		"loadStore": true, "appendRecord": true, "removeNodeLocked": true,
		"archiveSessionLog": true, "sessionLogPath": true,
		// Poll / projection (Phase 3 homes).
		"poll": true, "handleState": true, "sysload": true, "nodeView": true,
		// HTTP handlers (Phase 4 homes) — any handle* definition is banned.
	}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		name := fn.Name.Name
		if forbiddenNames[name] {
			t.Errorf("main.go defines %s (must remain composition/startup only)", name)
		}
		if strings.HasPrefix(name, "handle") {
			t.Errorf("main.go defines HTTP handler %s (handlers belong in feature files)", name)
		}
	}
}

func assertNoStalePhase4Comments(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	// Patterns that would indicate unfinished Phase 4 extraction notes.
	stale := []string{
		"until Packet 4",
		"until packet 4",
		"until Packet 4A",
		"until Packet 4B",
		"until Packet 4C",
		"until Packet 4D",
		"until Packet 4E",
		"until Packet 4F",
		"lives in main.go until",
		"remain in main.go until",
		"deferred to 4",
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		if strings.Contains(e.Name(), string(filepath.Separator)) {
			continue
		}
		b, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		text := string(b)
		for _, s := range stale {
			if strings.Contains(text, s) {
				t.Errorf("%s contains stale Phase 4 comment marker %q", e.Name(), s)
			}
		}
	}
}
