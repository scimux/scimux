package app

// Phase 4 — a package with no doc comment is a package whose reason for
// existing lives only in the head of whoever wrote it.
//
// This guard is deliberately a walk of the module, not an allowlist: a new
// leaf subsystem under internal/ is exactly the case where the rationale is
// most expensive to lose and least likely to be written down, and an
// allowlist would have to be edited to start covering it. There is no opt-out
// list for the same reason — a package that genuinely needs no explanation
// can say so in one line, which is itself the documentation.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// moduleRoot walks up from this test file to the directory holding go.mod.
// Derived rather than hardcoded so moving internal/app cannot silently make
// the walk cover nothing.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("walked past the filesystem root without finding go.mod")
		}
		dir = parent
	}
}

// skippedDirs are the directories a package walk must not descend into.
// testdata is Go's own convention for "not code"; the rest hold no packages.
var skippedDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"testdata":     true,
	"vendor":       true,
}

func TestEveryPackageHasADocComment(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	var missing []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && (skippedDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
			return filepath.SkipDir
		}
		pkgs, perr := parser.ParseDir(fset, path, func(fi fs.FileInfo) bool {
			return !strings.HasSuffix(fi.Name(), "_test.go")
		}, parser.ParseComments)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		for name, pkg := range pkgs {
			if hasPackageDoc(pkg) {
				continue
			}
			rel, _ := filepath.Rel(root, path)
			missing = append(missing, rel+" (package "+name+")")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(missing) > 0 {
		t.Fatalf("packages with no doc comment:\n  %s\n\n"+
			"Write one sentence above the package clause saying what the package is "+
			"for and what rule governs it. For a leaf subsystem that is the only place "+
			"a contributor who is not reading AGENTS.md will find the constraint.",
			strings.Join(missing, "\n  "))
	}
}

// hasPackageDoc reports whether any file in the package carries a doc comment
// on its package clause. Go's convention is that exactly one file does; this
// guard only asks that the package is explained somewhere, not where.
func hasPackageDoc(pkg *ast.Package) bool {
	for _, f := range pkg.Files {
		if f.Doc != nil && strings.TrimSpace(f.Doc.Text()) != "" {
			return true
		}
	}
	return false
}
