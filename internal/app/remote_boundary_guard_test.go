package app

// S0 (remote refactor) — AT-NFR-02-a, plus the go.mod half of the standing
// "no new dependency" check.
//
// NFR-02 makes pion dependency exception #2, confined behind internal/remote/.
// This guard is written *now*, while it is vacuously true and pion is not in
// go.mod at all, for one reason: a guard added alongside the dependency it
// guards is a guard nobody ever saw fail. Landing it first means S6 has to
// keep it green rather than invent it, and any leak between here and there is
// caught on the commit that causes it.
//
// Vacuous truth is the hazard, so the guard is built not to be silently
// vacuous: TestDependencyGuardScansTheModule asserts the walk actually reached
// the source tree and found imports. A path bug that made the scan cover zero
// files would otherwise report "no pion outside internal/remote" forever.

import (
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

// remoteBoundaryPrefix is the only import path allowed to reach pion.
const remoteBoundaryPrefix = "internal/remote"

// confinedModules are third-party module paths that may appear in the tree
// only under a named directory. Both scimux dependency exceptions live here:
// acp-go-sdk (approved, in use) and pion (approved 2026-08-22, lands in S6).
//
// AGENTS.md is the prose statement of these; this is the mechanical one.
var confinedModules = map[string]string{
	"github.com/coder/acp-go-sdk": "internal/acp",
	"github.com/pion/":            remoteBoundaryPrefix,
}

// allowedModuleRequires is every module path go.mod may require directly.
// Standard-library-only is the invariant; this list is the exception set.
//
// pion joined it in S6, which is the mechanical half of the maintainer
// decision of 2026-08-22. Only the one module scimux imports is here:
// github.com/pion/webrtc/v4 pulls in twenty-odd siblings, but those are
// `// indirect` — transitive closure, not decisions — and the loop below
// skips them. The confinement rule above is unaffected: approving pion
// widened what may be imported, never from where.
var allowedModuleRequires = []string{
	"github.com/coder/acp-go-sdk",
	"github.com/pion/webrtc/v4",
}

func repoRootFromTest(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// internal/app/<this file> -> repo root
	root := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %q has no go.mod: %v", root, err)
	}
	return root
}

// goImport is one import found in the tree, with the repo-relative file that
// carries it.
type goImport struct {
	rel  string
	path string
}

// scanGoImports walks every .go file in the module, tests included. Tests are
// deliberately in scope: a helper importing pion outside internal/remote leaks
// the type into the package's API surface just as a production file would, and
// the S6 reviewer's focus note calls that out specifically.
func scanGoImports(t *testing.T) []goImport {
	t.Helper()
	root := repoRootFromTest(t)
	fset := token.NewFileSet()
	var out []goImport
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
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return perr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		for _, spec := range f.Imports {
			p, uerr := strconv.Unquote(spec.Path.Value)
			if uerr != nil {
				continue
			}
			out = append(out, goImport{rel: rel, path: p})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}
	return out
}

// TestDependencyGuardScansTheModule is the anti-vacuity assertion. Without it,
// a broken root path would make every guard below pass by scanning nothing.
func TestDependencyGuardScansTheModule(t *testing.T) {
	imports := scanGoImports(t)
	if len(imports) < 100 {
		t.Fatalf("scanned only %d imports across the module; the walk is not reaching "+
			"the source tree, so every dependency guard below would pass vacuously", len(imports))
	}
	// The one third-party dependency that exists today must be visible to the
	// scan, or the scan is not seeing what it claims to police.
	seen := false
	for _, imp := range imports {
		if strings.HasPrefix(imp.path, "github.com/coder/acp-go-sdk") {
			seen = true
			break
		}
	}
	if !seen {
		t.Fatal("scan found no acp-go-sdk import anywhere; it is in go.mod and in " +
			"internal/acp, so the scan is not working")
	}
}

// TestConfinedModulesStayBehindTheirBoundary is AT-NFR-02-a. pion is the row
// that matters for the remote work; acp-go-sdk rides along because it is the
// same invariant and its boundary is already load-bearing today.
func TestConfinedModulesStayBehindTheirBoundary(t *testing.T) {
	var leaks []string
	for _, imp := range scanGoImports(t) {
		for module, boundary := range confinedModules {
			if !strings.HasPrefix(imp.path, module) {
				continue
			}
			if strings.HasPrefix(imp.rel, boundary+"/") || imp.rel == boundary {
				continue
			}
			leaks = append(leaks, imp.rel+" imports "+imp.path+" (confined to "+boundary+"/)")
		}
	}
	sort.Strings(leaks)
	if len(leaks) > 0 {
		t.Fatalf("confined dependency escaped its boundary (NFR-02):\n\t%s",
			strings.Join(leaks, "\n\t"))
	}
}

// TestGoModRequiresOnlyApprovedModules is the standing check the dispatch plan
// applies at every R2, made mechanical. It reads go.mod rather than `go list`
// so it needs no network and no module cache.
func TestGoModRequiresOnlyApprovedModules(t *testing.T) {
	root := repoRootFromTest(t)
	raw, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}

	var got []string
	inBlock := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		// Indirect requirements are transitive closure, not a decision; the
		// invariant is about direct dependencies. Detect the marker on the raw
		// line: stripping comments first would delete the very text the
		// indirect check below looks for, so every transitive module would be
		// reported as a deliberate one. That bug shipped unnoticed at S0
		// because go.mod had no indirect block at all until pion arrived — the
		// hazard this file's own header warns about, in a second form.
		indirect := strings.Contains(line, "// indirect")
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		switch {
		case line == "require (":
			inBlock = true
			continue
		case inBlock && line == ")":
			inBlock = false
			continue
		case strings.HasPrefix(line, "require "):
			line = strings.TrimSpace(strings.TrimPrefix(line, "require "))
		case !inBlock:
			continue
		}
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if indirect {
			continue
		}
		got = append(got, fields[0])
	}

	sort.Strings(got)
	want := append([]string(nil), allowedModuleRequires...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("go.mod requires %v, approved set is %v.\n"+
			"scimux is standard-library-only with named exceptions (AGENTS.md). "+
			"Adding a module is a maintainer decision: pion is approved (2026-08-22) "+
			"and lands in S6, and anything else needs a new exception.",
			got, want)
	}
}
