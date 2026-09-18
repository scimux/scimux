package app

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/remote"
)

// No test may reach the owner's terminal.
//
// Command.Run defaults Config.NewTerminal to remote.OpenOwnerTerminal
// (remote_command.go), and the no-enrollment branch of Client.Start reads an
// invite unconditionally. A test that constructs a --remote Command without
// supplying its own factory therefore opens /dev/tty, disables echo, and
// blocks on the developer's keyboard.
//
// It does not fail there: it fails on the machine that has no controlling
// terminal — which is to say it passes in CI and on a computer it hijacks, and
// fails only for the person running the suite from an interactive shell,
// with a stray keystroke read as the invite. That is the worst shape a test
// failure can have, so the property is pinned mechanically rather than left
// to whoever writes the next one.
func TestNoRemoteTestReachesTheOwnerTerminal(t *testing.T) {
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no test sources found; the guard would pass vacuously")
	}

	fset := token.NewFileSet()
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isCommandLit(lit) {
				return true
			}
			if !litMentions(lit, "--remote") || litNames(lit, "NewTerminal") {
				return true
			}
			t.Errorf("%s: this --remote Command supplies no NewTerminal, so "+
				"Command.Run defaults it to remote.OpenOwnerTerminal and the "+
				"test reads the invite from /dev/tty",
				fset.Position(lit.Pos()))
			return true
		})
	}
}

func isCommandLit(lit *ast.CompositeLit) bool {
	id, ok := lit.Type.(*ast.Ident)
	return ok && id.Name == "Command"
}

// litMentions reports whether s appears in any string literal inside lit,
// including the Args slice a --remote Command is recognised by.
func litMentions(lit *ast.CompositeLit, s string) bool {
	found := false
	ast.Inspect(lit, func(n ast.Node) bool {
		b, ok := n.(*ast.BasicLit)
		if ok && b.Kind == token.STRING && strings.Contains(b.Value, s) {
			found = true
		}
		return !found
	})
	return found
}

// litNames reports whether the identifier appears anywhere inside lit. A
// field set from a helper variable still names it at the key.
func litNames(lit *ast.CompositeLit, name string) bool {
	found := false
	ast.Inspect(lit, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}

// errNoTestTerminal is what a test's invite prompt gets instead of the
// developer's keyboard. It is an error rather than a canned invite so a
// test that unexpectedly reaches the prompt fails loudly at the call
// site, rather than quietly enrolling with a value nobody wrote.
var errNoTestTerminal = errors.New("test: no owner terminal")

func noTestTerminal() (remote.Terminal, error) { return nil, errNoTestTerminal }
