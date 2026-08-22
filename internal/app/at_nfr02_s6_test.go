package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codeberg.org/chrberger/scimux/legal"
)

// AT-NFR-02-a (S6 load-bearing half): AGENTS.md must record pion as a
// taken, scoped exception before the dependency lands. R1 fails because
// the amendment is still the pending note, not permission.
func TestAT_NFR_02_a_AgentsAmendmentRecordsTakenPionException(t *testing.T) {
	const at = "AT-NFR-02-a"
	root := repoRootFromTest(t)
	raw, err := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if err != nil {
		t.Fatalf("%s: read AGENTS.md: %v", at, err)
	}
	text := string(raw)
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "pion") {
		t.Fatalf("%s: AGENTS.md does not mention pion", at)
	}
	if !strings.Contains(text, "internal/remote") {
		t.Fatalf("%s: AGENTS.md does not confine pion to internal/remote/", at)
	}
	if strings.Contains(lower, "not yet taken") {
		t.Fatalf("%s: AGENTS.md still records pion as pending (not yet taken); NFR-02 requires the amendment before code lands", at)
	}
	if strings.Contains(lower, "this note is not permission") {
		t.Fatalf("%s: AGENTS.md still withholds permission to take pion", at)
	}
}

// AT-NFR-02-a: license notices for the pion module tree are embedded in
// legal/ and therefore in the About sheet. R1 fails because they are absent.
func TestAT_NFR_02_a_PionLicenseNoticesEmbedded(t *testing.T) {
	const at = "AT-NFR-02-a"
	entries, err := fs.ReadDir(legal.Files, ".")
	if err != nil {
		t.Fatalf("%s: legal.Files: %v", at, err)
	}
	var names []string
	found := false
	for _, e := range entries {
		names = append(names, e.Name())
		if strings.Contains(strings.ToLower(e.Name()), "pion") {
			found = true
		}
	}
	if !found {
		t.Fatalf("%s: legal/ embed has no pion license notice (have %v)", at, names)
	}
}

// AT-NFR-02-a (reviewer's focus): no pion type appears in any signature
// outside internal/remote/, including test helpers. The S0 import guard
// does not catch a type leaked through an alias. Vacuous in R1 (no pion
// types exist); load-bearing once R2 takes the dependency.
func TestAT_NFR_02_a_PionTypesStayInsideRemote(t *testing.T) {
	const at = "AT-NFR-02-a"
	root := repoRootFromTest(t)
	fset := token.NewFileSet()
	var leaks []string
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
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "internal/remote/") || rel == "internal/remote" {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		for _, spec := range f.Imports {
			p := strings.Trim(spec.Path.Value, `"`)
			if strings.Contains(p, "github.com/pion/") {
				leaks = append(leaks, rel+" imports "+p)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Type == nil {
				return true
			}
			if fn.Name != nil && !fn.Name.IsExported() && (fn.Recv == nil || !exportedRecv(fn.Recv)) {
				return true
			}
			ast.Inspect(fn.Type, func(n ast.Node) bool {
				if name := pionTypeName(n); name != "" {
					leaks = append(leaks, rel+" exported signature names "+name)
				}
				return true
			})
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("%s: walk: %v", at, err)
	}
	if len(leaks) > 0 {
		t.Fatalf("%s: pion type leaked outside internal/remote/:\n\t%s", at, strings.Join(leaks, "\n\t"))
	}
}

func exportedRecv(fl *ast.FieldList) bool {
	if fl == nil || len(fl.List) == 0 {
		return false
	}
	for _, f := range fl.List {
		switch t := f.Type.(type) {
		case *ast.Ident:
			return t.IsExported()
		case *ast.StarExpr:
			if id, ok := t.X.(*ast.Ident); ok {
				return id.IsExported()
			}
		}
	}
	return false
}

func pionTypeName(n ast.Node) string {
	switch x := n.(type) {
	case *ast.Ident:
		n := x.Name
		if looksLikePionType(n) {
			return n
		}
	case *ast.SelectorExpr:
		sel := x.Sel.Name
		pkg := ""
		if id, ok := x.X.(*ast.Ident); ok {
			pkg = id.Name
		}
		if looksLikePionType(pkg) || looksLikePionType(sel) || looksLikePionType(pkg+"."+sel) {
			if pkg != "" {
				return pkg + "." + sel
			}
			return sel
		}
	}
	return ""
}

func looksLikePionType(name string) bool {
	l := strings.ToLower(name)
	if strings.Contains(l, "pion") || strings.Contains(l, "webrtc") || strings.Contains(l, "rtcpeer") {
		return true
	}
	return false
}
