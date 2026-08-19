package app

// S0 (remote refactor) — AT-FR-02-c, the argv half.
//
// FR-02 forbids the invite from ever reaching argv. Today no invite exists, so
// this guard cannot test the invite; what it can do — and what the requirement
// actually needs — is pin the *shape* of the argv surface so that adding to it
// is a deliberate act a reviewer sees. It is therefore a ratchet, not a
// pattern match: it derives both argv surfaces from source and asserts each is
// exactly the set that exists today.
//
// Why a ratchet and not a regex for invite-shaped names: a regex fails open. A
// flag called --code, --enroll or --token slips past any list of forbidden
// words someone thinks to write, and the failure is silent. Requiring the
// allowlist below to be edited makes the reviewer the check.
//
// Why AST and not a copied list of subcommands: scimux grew from one hidden
// pre-flag subcommand to two in three commits (7e2f946..). A list written into
// a test is a third thing to keep in sync, and unlike the allowlist it fails
// *open* when it rots — a third subcommand would simply not be examined. So
// the dispatch is read out of Run() itself and the allowlist is compared
// against what was read, never used as the source.
//
// Run() has no dispatch *table* — it dispatches with two literal if-statements
// (main.go:135, :138). The AST is therefore the table, and
// TestRunArgvDispatchIsDiscoverable fails loudly if that shape ever changes,
// rather than quietly finding nothing.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// argvSubcommands is the complete set of hidden pre-flag subcommands Run()
// dispatches. Both are stdin→JSONL helpers that run inside a user's own agent
// session (see AGENTS.md: the Claude hook invariants). Adding an entry here is
// the review trigger: the new subcommand must carry no secret on argv.
var argvSubcommands = []string{
	"__claude-notify-hook",
	"__claude-permission-hook",
	"__claude-session-hook",
	"__claude-stop-hook",
}

// argvFlags is the complete set of flags registered in Run(). configureUsage
// registers none — it only sets fs.Usage — so Run()'s own registrations are
// the whole flag surface. Same review trigger as above.
var argvFlags = []string{
	"addr",
	"data",
	"socket",
	"trusted-host",
}

// runInitCalls are the calls that mark the end of "before flag parsing". A
// subcommand dispatched after any of these has already run application setup,
// which is what FR-01 forbids for a hook invocation: a hook must return before
// any remote code could initialise.
var runInitCalls = map[string]bool{
	"Parse":          true, // flag.Parse
	"NewApp":         true,
	"NewHandler":     true,
	"prepareDataDir": true,
}

func parseMainGo(t *testing.T) (*token.FileSet, *ast.File) {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(thisFile), "main.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	return fset, f
}

// mustFindFunc wraps the package's existing findFunc (router_ownership_test.go)
// with the fatal this guard wants: a missing Run() is a rotted guard, not a nil
// dereference three lines later.
func mustFindFunc(t *testing.T, f *ast.File, name string) *ast.FuncDecl {
	t.Helper()
	fn := findFunc(f, name)
	if fn == nil {
		t.Fatalf("main.go declares no func %s", name)
	}
	return fn
}

// packageStringConsts resolves the package's string constants so a dispatch
// written as `os.Args[1] == claudeSessionHookCmd` yields its literal value.
func packageStringConsts(t *testing.T) map[string]string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package dir: %v", err)
	}
	out := map[string]string{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, d := range file.Decls {
				gen, isGen := d.(*ast.GenDecl)
				if !isGen || gen.Tok != token.CONST {
					continue
				}
				for _, spec := range gen.Specs {
					vs, isValue := spec.(*ast.ValueSpec)
					if !isValue {
						continue
					}
					for i, name := range vs.Names {
						if i >= len(vs.Values) {
							continue
						}
						if lit, isLit := vs.Values[i].(*ast.BasicLit); isLit && lit.Kind == token.STRING {
							if v, err := strconv.Unquote(lit.Value); err == nil {
								out[name.Name] = v
							}
						}
					}
				}
			}
		}
	}
	return out
}

// argvDispatch is one hidden subcommand read out of Run().
type argvDispatch struct {
	name  string // the literal argv[1] value it matches
	line  int
	index int // position in Run()'s top-level statement list
}

// readArgvDispatch walks Run()'s top-level statements for comparisons against
// os.Args[1] and reports each matched literal. This is the "dispatch table":
// the source, not a copy of it.
func readArgvDispatch(t *testing.T, fset *token.FileSet, run *ast.FuncDecl, consts map[string]string) (found []argvDispatch, firstInit int) {
	t.Helper()
	firstInit = -1
	for i, stmt := range run.Body.List {
		if firstInit < 0 && statementCallsAny(stmt, runInitCalls) {
			firstInit = i
		}
		ifStmt, isIf := stmt.(*ast.IfStmt)
		if !isIf {
			continue
		}
		name, matched := osArgs1Comparand(ifStmt.Cond, consts)
		if !matched {
			continue
		}
		found = append(found, argvDispatch{
			name:  name,
			line:  fset.Position(ifStmt.Pos()).Line,
			index: i,
		})
	}
	return found, firstInit
}

// osArgs1Comparand reports the string an expression compares os.Args[1]
// against, resolving a constant identifier to its literal value.
func osArgs1Comparand(cond ast.Expr, consts map[string]string) (string, bool) {
	var out string
	var ok bool
	ast.Inspect(cond, func(n ast.Node) bool {
		bin, isBin := n.(*ast.BinaryExpr)
		if !isBin || bin.Op != token.EQL {
			return true
		}
		for _, pair := range [2][2]ast.Expr{{bin.X, bin.Y}, {bin.Y, bin.X}} {
			if !isOSArgsIndex(pair[0], 1) {
				continue
			}
			switch v := pair[1].(type) {
			case *ast.BasicLit:
				if v.Kind == token.STRING {
					if s, err := strconv.Unquote(v.Value); err == nil {
						out, ok = s, true
					}
				}
			case *ast.Ident:
				if s, known := consts[v.Name]; known {
					out, ok = s, true
				}
			}
		}
		return true
	})
	return out, ok
}

// isOSArgsIndex matches the expression os.Args[<want>].
func isOSArgsIndex(e ast.Expr, want int) bool {
	idx, isIndex := e.(*ast.IndexExpr)
	if !isIndex {
		return false
	}
	sel, isSel := idx.X.(*ast.SelectorExpr)
	if !isSel || sel.Sel.Name != "Args" {
		return false
	}
	pkg, isIdent := sel.X.(*ast.Ident)
	if !isIdent || pkg.Name != "os" {
		return false
	}
	lit, isLit := idx.Index.(*ast.BasicLit)
	if !isLit || lit.Kind != token.INT {
		return false
	}
	n, err := strconv.Atoi(lit.Value)
	return err == nil && n == want
}

func statementCallsAny(stmt ast.Stmt, names map[string]bool) bool {
	hit := false
	ast.Inspect(stmt, func(n ast.Node) bool {
		call, isCall := n.(*ast.CallExpr)
		if !isCall {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			if names[fn.Sel.Name] {
				hit = true
			}
		case *ast.Ident:
			if names[fn.Name] {
				hit = true
			}
		}
		return true
	})
	return hit
}

// TestRunArgvDispatchIsDiscoverable is the anti-rot assertion. Every other test
// here reads the dispatch out of Run()'s AST; if a refactor changes that shape
// — a real dispatch table, a switch, a helper — those tests would find nothing
// and pass vacuously. This one fails instead, so the guard has to be updated
// rather than silently stop guarding.
func TestRunArgvDispatchIsDiscoverable(t *testing.T) {
	fset, f := parseMainGo(t)
	run := mustFindFunc(t, f, "Run")
	found, _ := readArgvDispatch(t, fset, run, packageStringConsts(t))
	if len(found) == 0 {
		t.Fatal("read no os.Args[1] dispatch out of Run(): the argv guard has rotted. " +
			"If Run() moved to a real dispatch table or a switch, teach readArgvDispatch " +
			"that shape — do not delete this test, and do not replace it with a copied list.")
	}
}

// TestArgvSubcommandSurfaceIsExactlyAllowlisted is AT-FR-02-c's subcommand
// half. A flag-set assertion alone is incomplete: a subcommand can carry an
// argument that never reaches flag.CommandLine.
func TestArgvSubcommandSurfaceIsExactlyAllowlisted(t *testing.T) {
	fset, f := parseMainGo(t)
	run := mustFindFunc(t, f, "Run")
	found, _ := readArgvDispatch(t, fset, run, packageStringConsts(t))

	got := make([]string, 0, len(found))
	for _, d := range found {
		got = append(got, d.name)
	}
	sort.Strings(got)
	want := append([]string(nil), argvSubcommands...)
	sort.Strings(want)

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Run() dispatches argv subcommands %v, allowlist says %v.\n"+
			"If a subcommand was added: confirm it carries no invite, handle or key "+
			"material on argv (FR-02), then add it to argvSubcommands.\n"+
			"If one was removed: drop it from argvSubcommands.", got, want)
	}
}

// TestArgvSubcommandsDispatchBeforeAnyInit pins the FR-01 property the hook
// paths already have and that nothing should be allowed to take away: a hook
// invocation returns before application setup, so no remote code can
// initialise inside a user's agent session.
func TestArgvSubcommandsDispatchBeforeAnyInit(t *testing.T) {
	fset, f := parseMainGo(t)
	run := mustFindFunc(t, f, "Run")
	found, firstInit := readArgvDispatch(t, fset, run, packageStringConsts(t))
	if firstInit < 0 {
		t.Fatal("found no init call in Run(); runInitCalls needs updating")
	}
	for _, d := range found {
		if d.index >= firstInit {
			t.Errorf("subcommand %q dispatches at main.go:%d, at or after Run()'s first "+
				"init call — a hook invocation must return before any application or "+
				"remote setup runs (FR-01)", d.name, d.line)
		}
	}
}

// TestArgvSubcommandBodiesOnlyExit asserts each dispatch body does exactly one
// thing: exit with the helper's status. A body that grew a side effect would be
// running that side effect inside every hook invocation.
func TestArgvSubcommandBodiesOnlyExit(t *testing.T) {
	fset, f := parseMainGo(t)
	run := mustFindFunc(t, f, "Run")
	consts := packageStringConsts(t)
	for _, stmt := range run.Body.List {
		ifStmt, isIf := stmt.(*ast.IfStmt)
		if !isIf {
			continue
		}
		name, matched := osArgs1Comparand(ifStmt.Cond, consts)
		if !matched {
			continue
		}
		line := fset.Position(ifStmt.Pos()).Line
		if ifStmt.Else != nil {
			t.Errorf("subcommand %q (main.go:%d) has an else branch", name, line)
		}
		if len(ifStmt.Body.List) != 1 {
			t.Errorf("subcommand %q (main.go:%d) body has %d statements, want exactly one os.Exit",
				name, line, len(ifStmt.Body.List))
			continue
		}
		if !statementCallsAny(ifStmt.Body.List[0], map[string]bool{"Exit": true}) {
			t.Errorf("subcommand %q (main.go:%d) body does not os.Exit; it would fall "+
				"through into application startup", name, line)
		}
	}
}

// TestArgvFlagSurfaceIsExactlyAllowlisted is AT-FR-02-c's flag half. The names
// are read out of Run()'s registrations, so the allowlist is compared against
// source rather than standing in for it.
func TestArgvFlagSurfaceIsExactlyAllowlisted(t *testing.T) {
	_, f := parseMainGo(t)
	run := mustFindFunc(t, f, "Run")

	var got []string
	ast.Inspect(run.Body, func(n ast.Node) bool {
		call, isCall := n.(*ast.CallExpr)
		if !isCall {
			return true
		}
		sel, isSel := call.Fun.(*ast.SelectorExpr)
		if !isSel {
			return true
		}
		// flag.String/Bool/Int/Duration/... take the name first; flag.Var and
		// the *Var forms take the destination first and the name second.
		nameArg := -1
		switch {
		case strings.HasSuffix(sel.Sel.Name, "Var"):
			nameArg = 1
		case sel.Sel.Name == "String", sel.Sel.Name == "Bool", sel.Sel.Name == "Int",
			sel.Sel.Name == "Int64", sel.Sel.Name == "Uint", sel.Sel.Name == "Uint64",
			sel.Sel.Name == "Float64", sel.Sel.Name == "Duration", sel.Sel.Name == "Func",
			sel.Sel.Name == "TextVar":
			nameArg = 0
		default:
			return true
		}
		pkg, isIdent := sel.X.(*ast.Ident)
		if !isIdent || pkg.Name != "flag" {
			return true
		}
		if nameArg >= len(call.Args) {
			return true
		}
		lit, isLit := call.Args[nameArg].(*ast.BasicLit)
		if !isLit || lit.Kind != token.STRING {
			return true
		}
		if s, err := strconv.Unquote(lit.Value); err == nil {
			got = append(got, s)
		}
		return true
	})

	sort.Strings(got)
	want := append([]string(nil), argvFlags...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Run() registers flags %v, allowlist says %v.\n"+
			"FR-02: no flag may carry an invite. An --invite= style flag must never "+
			"exist; invite input is hidden TTY, --invite-file or --invite-stdin, and "+
			"none of those puts the code on argv.", got, want)
	}
}

// TestNoInviteBearingFlagExists is the one direct FR-02 assertion available
// today, and it is deliberately belt-and-braces on top of the ratchet: it can
// only ever add failures, never license an addition. The ratchet above is what
// actually catches a flag named something the words below do not anticipate.
func TestNoInviteBearingFlagExists(t *testing.T) {
	forbidden := []string{"invite", "code", "secret", "token", "key", "handle", "passphrase", "password"}
	for _, name := range argvFlags {
		lower := strings.ToLower(name)
		for _, bad := range forbidden {
			// --invite-file and --invite-stdin are the FR-02 sanctioned forms:
			// they name a path or a stream, never carry the plaintext. They do
			// not exist yet; when S5 adds them they go here, not in a general
			// exemption for the word "invite".
			if name == "invite-file" || name == "invite-stdin" {
				continue
			}
			if strings.Contains(lower, bad) {
				t.Errorf("flag --%s looks like it carries credential material on argv (FR-02)", name)
			}
		}
	}
	for _, name := range argvSubcommands {
		if !strings.HasPrefix(name, "__") {
			t.Errorf("subcommand %q is not marked hidden with a __ prefix", name)
		}
	}
}
