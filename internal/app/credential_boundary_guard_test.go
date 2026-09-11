package app

// The harness-credential boundary, made mechanical.
//
// scimux runs no login flow and reads no harness credential store. Every
// agent CLI owns its own auth — `claude /login`, `codex login`, `muse auth` —
// and scimux execs the binary and reads what the binary writes. The README
// states that publicly under "Your agents, your accounts", and it is the
// sentence that keeps §3.2-shaped clauses true across every vendor: a
// subscription credential is for use with its own harness, and a supervisor
// that never touches it cannot be the thing that moves it elsewhere.
//
// This guard exists because the rule is an *absence*, and an absence has no
// line to comment. There is no code to annotate with "do not read the
// credential file here"; the only durable statement is a test that fails when
// someone adds one. scimux already lost that argument once in the other
// direction — 473fb0b removed a real Claude OAuth read that had shipped — and
// TestNoClaudeCredentialOrOAuthPathRemains (claude_usage_collect_test.go) was
// written then. That one is deliberately kept: it is narrower and sharper,
// banning four exact endpoint and JSON-key spellings that no path-shaped rule
// below would recognise. This file is the other half — every vendor, every
// package, one mechanism.
//
// Written in the shape of remote_boundary_guard_test.go, including the part
// that matters most: it is built not to be silently vacuous. A guard over a
// tree that is clean today reports "clean" just as happily over a tree it
// never reached, so the walk asserts its own coverage and the matcher is run
// against a planted violation.
//
// What it cannot do, stated so nobody mistakes its silence for proof: it
// matches string literals, so a path assembled from fragments at runtime
// walks straight past. That is deliberate evasion rather than the accident
// this guards against, and no test stops a contributor who means it. What the
// guard does stop is the plausible version — an "authenticated check for
// better rate limits", a usage gauge that reads the token instead of asking
// the CLI, a new adapter that copies a competitor's credential discovery.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// bannedCredentialPaths are file-path fragments that only appear in code that
// is going after a harness's stored credentials. Each is path-shaped on
// purpose: the bare word "credentials" is ordinary English and shows up in
// comments and fixture prose, while ".credentials" and "auth.json" are the
// files themselves.
//
// The value is the reason, printed on failure, because a guard that only says
// "banned" invites the reader to conclude it is over-strict and delete it.
var bannedCredentialPaths = map[string]string{
	".credentials":     "the Claude CLI's stored OAuth file (~/.claude/.credentials.json); reading it is what 473fb0b removed",
	"auth.json":        "the Codex CLI's stored credentials (~/.codex/auth.json)",
	"credentials.json": "a stored credential file under any harness's config directory",
	".netrc":           "machine credentials; scimux authenticates to no vendor",
}

// bannedProviderEnv are provider API-key environment variables. scimux needs
// none of them: the harness reads its own environment, and scimux passing the
// environment through to a child is inheritance, not a read. Naming one in
// this tree means something here went looking for a key.
var bannedProviderEnv = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"OPENAI_API_KEY",
	"XAI_API_KEY",
	"GROK_API_KEY",
	"GEMINI_API_KEY",
	"GOOGLE_API_KEY",
	"OPENROUTER_API_KEY",
}

// credentialGuardExemptFiles are the files that must name the banned strings
// in order to ban them. Two guards, and nothing else, ever.
//
// An allowlist rather than "skip all tests" because a test helper that reads a
// credential store is a real leak, not a fixture: it would read the
// credentials of whoever runs the suite. Adding a file here is a new
// exception and shows up as one in review — the same shape as
// allowedModuleRequires next door.
var credentialGuardExemptFiles = map[string]string{
	"internal/app/credential_boundary_guard_test.go": "this guard",
	"internal/app/claude_usage_collect_test.go":      "the narrower Claude-specific guard",
}

// goLiteral is one string literal found in the tree, with the repo-relative
// file and line that carry it.
type goLiteral struct {
	rel   string
	line  int
	value string
}

// scanGoStringLiterals walks every .go file in the module and returns the
// value of every string literal. Comments are not literals and never appear
// here, which is the point of parsing rather than grepping: usage.go and
// claude_statusline.go both describe the removed OAuth read in prose, and
// that prose is the institutional memory of why the rule exists. A text grep
// would force those comments to talk around the thing they are about.
func scanGoStringLiterals(t *testing.T) []goLiteral {
	t.Helper()
	root := repoRootFromTest(t)
	fset := token.NewFileSet()
	var out []goLiteral
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if _, exempt := credentialGuardExemptFiles[rel]; exempt {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, uerr := strconv.Unquote(lit.Value)
			if uerr != nil {
				return true
			}
			out = append(out, goLiteral{
				rel:   rel,
				line:  fset.Position(lit.Pos()).Line,
				value: v,
			})
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}
	return out
}

// credentialViolations reports every literal carrying a banned fragment.
// Matching is case-insensitive: AUTH.JSON is the same file.
func credentialViolations(lits []goLiteral, banned map[string]string) []string {
	var out []string
	for _, lit := range lits {
		low := strings.ToLower(lit.value)
		for frag, why := range banned {
			if !strings.Contains(low, strings.ToLower(frag)) {
				continue
			}
			out = append(out, lit.rel+":"+strconv.Itoa(lit.line)+" names "+frag+" — "+why)
		}
	}
	sort.Strings(out)
	return out
}

// TestCredentialGuardScansTheModule is the first anti-vacuity assertion: the
// walk must actually reach the source tree.
//
// The sentinel is ~/.claude/projects, in internal/transcript. It proves three
// things at once — the walk leaves internal/app, literals are being extracted
// rather than silently dropped, and the matcher sees a real harness path. It
// is also a path scimux legitimately reads, which is the distinction the
// whole guard turns on: the transcript a CLI writes is scimux's to read, the
// credential beside it is not.
func TestCredentialGuardScansTheModule(t *testing.T) {
	lits := scanGoStringLiterals(t)
	if len(lits) < 1000 {
		t.Fatalf("scanned only %d string literals across the module; the walk is not "+
			"reaching the source tree, so the credential guards would pass vacuously",
			len(lits))
	}
	var sawTranscriptPath bool
	pkgs := map[string]bool{}
	for _, lit := range lits {
		pkgs[filepath.Dir(lit.rel)] = true
		if lit.value == "projects" && strings.HasPrefix(lit.rel, "internal/transcript/") {
			sawTranscriptPath = true
		}
	}
	if !sawTranscriptPath {
		t.Fatal(`scan found no "projects" literal in internal/transcript; that is where ` +
			"the Claude transcript path is built, so the scan is not seeing what it polices")
	}
	if len(pkgs) < 5 {
		t.Fatalf("scan covered only %d directories (%v); the guard must span every "+
			"package, not just the one it lives in", len(pkgs), pkgs)
	}
}

// TestCredentialGuardCatchesAPlantedPath is the second anti-vacuity
// assertion, and the one the walk cannot make: a matcher that stopped
// matching would leave every test below green over a tree full of
// violations. So the matcher is run against a file that definitely has one.
func TestCredentialGuardCatchesAPlantedPath(t *testing.T) {
	const planted = `package p

import "path/filepath"

func leak(home string) string {
	return filepath.Join(home, ".claude", ".credentials.json")
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "planted.go", planted, 0)
	if err != nil {
		t.Fatalf("parse planted source: %v", err)
	}
	var lits []goLiteral
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		v, uerr := strconv.Unquote(lit.Value)
		if uerr != nil {
			return true
		}
		lits = append(lits, goLiteral{rel: "planted.go", line: fset.Position(lit.Pos()).Line, value: v})
		return true
	})
	if got := credentialViolations(lits, bannedCredentialPaths); len(got) == 0 {
		t.Fatal("the matcher did not flag a planted ~/.claude/.credentials.json read; " +
			"every credential guard in this file is therefore meaningless")
	}

	env := []goLiteral{{rel: "planted.go", line: 1, value: "ANTHROPIC_API_KEY"}}
	if got := credentialViolations(env, providerEnvRules()); len(got) == 0 {
		t.Fatal("the matcher did not flag a planted ANTHROPIC_API_KEY read")
	}
}

// providerEnvRules adapts the env list to the shared matcher.
func providerEnvRules() map[string]string {
	out := make(map[string]string, len(bannedProviderEnv))
	for _, name := range bannedProviderEnv {
		out[name] = "a provider API key; the harness reads its own environment and scimux needs none"
	}
	return out
}

// TestNoHarnessCredentialPathInTree is the rule itself. It is expected to be
// vacuously true forever; the two tests above are what make that mean
// something.
func TestNoHarnessCredentialPathInTree(t *testing.T) {
	hits := credentialViolations(scanGoStringLiterals(t), bannedCredentialPaths)
	if len(hits) > 0 {
		t.Fatalf("scimux reads no harness credential store (AGENTS.md; README "+
			"\"Your agents, your accounts\"), and this names one:\n\t%s\n\n"+
			"Each agent CLI owns its own auth. If a feature seems to need the "+
			"credential, it wants the CLI to answer instead — that is how the "+
			"usage gauge was rebuilt after 473fb0b.",
			strings.Join(hits, "\n\t"))
	}
}

// TestNoProviderAPIKeyEnvRead is the same boundary at the other door. A key in
// the environment is still the user's credential; scimux passing its
// environment to a child is inheritance, not a read.
func TestNoProviderAPIKeyEnvRead(t *testing.T) {
	hits := credentialViolations(scanGoStringLiterals(t), providerEnvRules())
	if len(hits) > 0 {
		t.Fatalf("scimux names a provider API key:\n\t%s\n\n"+
			"Nothing here authenticates to a model provider. Everything scimux "+
			"learns about a harness is free and unauthenticated (--version, the "+
			"model catalog, the transcript the CLI writes itself).",
			strings.Join(hits, "\n\t"))
	}
}
