package app

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

func TestDocCommentTruth(t *testing.T) {
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	fileName := map[*ast.File]string{}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, f)
		fileName[f] = name
	}

	documented := 0
	got := map[string]string{} // "file func" -> "file:line func <- doc starts <word>"
	byName := map[string]*ast.FuncDecl{}
	for _, f := range files {
		name := fileName[f]
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			byName[fn.Name.Name] = fn
			if fn.Doc == nil {
				continue
			}
			documented++
			fields := strings.Fields(fn.Doc.Text())
			word := ""
			if len(fields) > 0 {
				word = fields[0]
			}
			if word == fn.Name.Name {
				continue
			}
			pos := fset.Position(fn.Pos())
			key := name + " " + fn.Name.Name
			got[key] = fmt.Sprintf("%s:%d %s <- doc starts %s", name, pos.Line, fn.Name.Name, word)
		}
	}
	if documented < 200 {
		t.Fatalf("documented functions = %d, want at least 200 (broken parse or wrong directory)", documented)
	}

	// Deliberate first-word exceptions. If a future mismatch appears, this
	// test must fail rather than be widened.
	allow := map[string]bool{
		"state_api.go handleState": true, // Snapshot boundary for handleState — a deliberate prose heading for a bulleted locking contract, not drift
	}

	var unexpected, stale []string
	for key, msg := range got {
		if !allow[key] {
			unexpected = append(unexpected, msg)
		}
	}
	for key := range allow {
		if _, ok := got[key]; !ok {
			stale = append(stale, key)
		}
	}
	sort.Strings(unexpected)
	sort.Strings(stale)
	if len(unexpected) > 0 || len(stale) > 0 {
		all := make([]string, 0, len(got))
		for _, msg := range got {
			all = append(all, msg)
		}
		sort.Strings(all)
		t.Errorf("doc-comment name mismatches (%d):\n  %s", len(got), strings.Join(all, "\n  "))
		if len(unexpected) > 0 {
			t.Errorf("not on the allowlist:\n  %s", strings.Join(unexpected, "\n  "))
		}
		if len(stale) > 0 {
			t.Errorf("allowlist entries with no mismatch (do not keep them): %s", strings.Join(stale, ", "))
		}
	}

	seams := []string{
		"processClaudeHookEvent",
		"claudeGeneration",
		"advanceClaudeClearAfterWeb",
		"mintClaudeVisibleEpoch",
		"claudeLaunchError",
	}
	var missing []string
	for _, name := range seams {
		fn := byName[name]
		if fn == nil {
			missing = append(missing, name+" (function not found)")
			continue
		}
		text := ""
		if fn.Doc != nil {
			text = fn.Doc.Text()
		}
		if !strings.Contains(text, "Test seam:") {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("test-only seams missing %q in their doc comment:\n  %s", "Test seam:", strings.Join(missing, "\n  "))
	}
}
