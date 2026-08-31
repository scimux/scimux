package app

// Phase 3 characterization — the flag surface behind Command.Run.
//
// scimux parses the same eight flags twice: once on flag.CommandLine in
// Run() (main.go) and once on Command.Run's own FlagSet. The first parse
// exists only to seed the Config the second one then re-derives from the
// identical argv, and its -addr and -trusted-host values are discarded
// outright. Before the duplicate can be deleted, the surviving parse needs
// assertions over the *effective* configuration it produces, so a
// regression shows up as a failing row rather than as a flag that quietly
// stops working.
//
// Every row drives the real Command.Run. It stays hermetic by leaving
// -remote off: flags are applied and the listener is bound before any
// rendezvous work, so nothing here talks to a network.

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"go/ast"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/remote"
)

// flagCommand runs argv through Command.Run and hands back the command, the
// stderr it wrote, and the error. It does not fail on error: the -h row and
// the unknown-flag row are about the error.
func flagCommand(t *testing.T, args ...string) (*Command, *bytes.Buffer, error) {
	t.Helper()
	stderr := new(bytes.Buffer)
	cmd := &Command{
		Args:   append([]string{"scimux"}, args...),
		Stdin:  strings.NewReader(""),
		Stdout: io.Discard,
		Stderr: stderr,
		Home:   t.TempDir(),
		Config: remote.Config{HTTPClient: unreachableHTTP()},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	err := cmd.Run(ctx)
	t.Cleanup(cmd.closeListener)
	return cmd, stderr, err
}

// loopback keeps a row off the default port. Rows that assert a default
// other than -addr prepend it so two rows never contend for one socket.
func loopback(args ...string) []string {
	return append([]string{"-addr", "127.0.0.1:0"}, args...)
}

func TestCommandRunFlagParse(t *testing.T) {
	t.Run("data reaches the config and the store", func(t *testing.T) {
		dir := t.TempDir()
		cmd, _, err := flagCommand(t, loopback("-data", dir)...)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if cmd.Config.DataDir != dir {
			t.Errorf("Config.DataDir = %q, want %q", cmd.Config.DataDir, dir)
		}
		if want := filepath.Join(dir, "nodes.jsonl"); cmd.application.storePath != want {
			t.Errorf("storePath = %q, want %q", cmd.application.storePath, want)
		}
	})

	t.Run("data defaults under home", func(t *testing.T) {
		cmd, _, err := flagCommand(t, loopback()...)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		want := filepath.Join(cmd.Home, ".scimux")
		if cmd.Config.DataDir != want {
			t.Errorf("Config.DataDir = %q, want %q", cmd.Config.DataDir, want)
		}
	})

	t.Run("socket reaches the banner value", func(t *testing.T) {
		cmd, _, err := flagCommand(t, loopback("-socket", "alt-socket")...)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		// c.socket is what the startup banner prints as the tmux -L name, so
		// it is the observable that matters for this flag.
		if cmd.socket != "alt-socket" {
			t.Errorf("socket = %q, want %q", cmd.socket, "alt-socket")
		}
	})

	t.Run("socket defaults to scimux", func(t *testing.T) {
		cmd, _, err := flagCommand(t, loopback()...)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if cmd.socket != "scimux" {
			t.Errorf("socket = %q, want %q", cmd.socket, "scimux")
		}
	})

	t.Run("addr reaches the bound listener", func(t *testing.T) {
		cmd, _, err := flagCommand(t, "-addr", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if cmd.listenAddr != "127.0.0.1:0" {
			t.Errorf("listenAddr = %q, want %q", cmd.listenAddr, "127.0.0.1:0")
		}
		ln := cmd.Listener()
		if ln == nil {
			t.Fatal("Run bound no listener")
		}
		if host, _, _ := strings.Cut(ln.Addr().String(), ":"); host != "127.0.0.1" {
			t.Errorf("listener bound on %s, want loopback", ln.Addr())
		}
	})

	t.Run("addr defaults to loopback 8787", func(t *testing.T) {
		// This row deliberately does not prepend -addr: the default is the
		// thing under test. listenAddr is assigned before the bind, so the
		// assertion holds whether or not 8787 happens to be free on this
		// machine — which is why the error is not fatal here.
		cmd, _, _ := flagCommand(t)
		if cmd.listenAddr != "127.0.0.1:8787" {
			t.Errorf("listenAddr = %q, want %q", cmd.listenAddr, "127.0.0.1:8787")
		}
	})

	t.Run("remote defaults off", func(t *testing.T) {
		cmd, _, err := flagCommand(t, loopback()...)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if cmd.Config.Remote {
			t.Error("Config.Remote is true without -remote")
		}
		if cmd.Client() != nil {
			t.Error("a local run built a remote client")
		}
	})

	t.Run("invite-file reaches the config", func(t *testing.T) {
		cmd, _, err := flagCommand(t, loopback("-invite-file", "/nonexistent/invite")...)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if cmd.Config.InviteFile != "/nonexistent/invite" {
			t.Errorf("Config.InviteFile = %q, want %q", cmd.Config.InviteFile, "/nonexistent/invite")
		}
	})

	t.Run("invite-stdin reaches the config", func(t *testing.T) {
		cmd, _, err := flagCommand(t, loopback("-invite-stdin")...)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !cmd.Config.InviteStdin {
			t.Error("Config.InviteStdin is false with -invite-stdin")
		}
	})

	t.Run("rendezvous-url is normalised into Origin", func(t *testing.T) {
		cmd, _, err := flagCommand(t, loopback("-rendezvous-url", "https://rv.example/")...)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if cmd.Config.Origin != "https://rv.example" {
			t.Errorf("Config.Origin = %q, want %q", cmd.Config.Origin, "https://rv.example")
		}
	})

	t.Run("rendezvous-url rejects a non-http scheme", func(t *testing.T) {
		_, _, err := flagCommand(t, loopback("-rendezvous-url", "ftp://rv.example")...)
		if err == nil {
			t.Fatal("Run accepted a non-http rendezvous URL")
		}
	})

	t.Run("trusted-host reaches the request policy", func(t *testing.T) {
		cmd, _, err := flagCommand(t, loopback("-trusted-host", "box.example", "-trusted-host", "10.0.0.7")...)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		p := cmd.application.requestPolicy
		if p == nil {
			t.Fatal("Run installed no request policy")
		}
		for _, host := range []string{"box.example", "10.0.0.7"} {
			if !p.allow(host) {
				t.Errorf("-trusted-host %q did not reach newRequestPolicy: policy rejects it", host)
			}
		}
		if p.allow("evil.example") {
			t.Error("policy allows a host nobody trusted; -trusted-host is not additive")
		}
	})

	t.Run("an unparseable trusted-host fails startup", func(t *testing.T) {
		_, _, err := flagCommand(t, loopback("-trusted-host", "not a host")...)
		if err == nil {
			t.Fatal("Run accepted an unparseable -trusted-host")
		}
	})

	t.Run("an unknown flag is an error, not an exit", func(t *testing.T) {
		cmd, stderr, err := flagCommand(t, loopback("-nope")...)
		if err == nil {
			t.Fatal("Run accepted an unknown flag")
		}
		if cmd.Listener() != nil {
			t.Error("Run bound a listener after a flag error")
		}
		// The FlagSet reports argv errors itself. The sentinel is how the
		// process entry point knows not to print a second copy, and how it
		// keeps flag.ExitOnError's status 2.
		if !errors.Is(err, errFlagsReported) {
			t.Errorf("Run error = %v, want it to wrap errFlagsReported", err)
		}
		if n := strings.Count(stderr.String(), "flag provided but not defined: -nope"); n != 1 {
			t.Errorf("the argv error appears %d times on stderr, want exactly 1", n)
		}
	})
}

// TestCommandRunHelpIsTheProcessUsage is the one assertion in this file that
// does not hold before the refactor. Today `-h` reaches flag.CommandLine
// first, which configureUsage has taught to print the product summary and
// the "Usage: <argv0> [options]" line; Command.Run's own FlagSet has no
// Usage at all and falls back to the flag package's bare "Usage of scimux:".
// Deleting the first parse would therefore silently downgrade -h. The
// surviving parse has to carry the same usage.
func TestCommandRunHelpIsTheProcessUsage(t *testing.T) {
	cmd, stderr, err := flagCommand(t, "-h")
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("Run(-h) error = %v, want flag.ErrHelp", err)
	}
	if cmd.Listener() != nil {
		t.Error("Run bound a listener while printing usage")
	}
	out := stderr.String()
	if !strings.Contains(out, appSummary) {
		t.Errorf("-h did not print the product summary.\ngot:\n%s", out)
	}
	if !strings.Contains(out, "Usage: scimux [options]") {
		t.Errorf("-h did not print the process usage line.\ngot:\n%s", out)
	}
	if !strings.Contains(out, "-trusted-host") {
		t.Errorf("-h did not print the flag defaults.\ngot:\n%s", out)
	}
}

// TestRunTreatsHelpAsACleanExit covers the half of -h that no in-process test
// can reach: app.Run() ends in os.Exit, so only the source says what a
// flag.ErrHelp from Command.Run does to the process. Before Phase 3,
// flag.CommandLine's ExitOnError printed usage and exited 0. Command.Run
// returns the error instead, so without this branch `scimux -h` would print
// usage, then "scimux: flag: help requested", then exit 1.
func TestRunTreatsHelpAsACleanExit(t *testing.T) {
	_, f := parseMainGo(t)
	run := mustFindFunc(t, f, "Run")

	found := false
	ast.Inspect(run.Body, func(n ast.Node) bool {
		call, isCall := n.(*ast.CallExpr)
		if !isCall || len(call.Args) != 2 {
			return true
		}
		sel, isSel := call.Fun.(*ast.SelectorExpr)
		if !isSel || sel.Sel.Name != "Is" {
			return true
		}
		if pkg, isIdent := sel.X.(*ast.Ident); !isIdent || pkg.Name != "errors" {
			return true
		}
		arg, isSel := call.Args[1].(*ast.SelectorExpr)
		if !isSel || arg.Sel.Name != "ErrHelp" {
			return true
		}
		if pkg, isIdent := arg.X.(*ast.Ident); isIdent && pkg.Name == "flag" {
			found = true
		}
		return true
	})
	if !found {
		t.Fatal("Run() does not test Command.Run's error against flag.ErrHelp; " +
			"`scimux -h` would print usage and then exit 1 with a bogus error line")
	}
}

// TestRunKeepsTheArgvExitStatus is the other half of the source-level -h
// contract. flag.ExitOnError exited 2 on a bad flag; Command.Run returns
// instead, so Run() has to reproduce both statuses or a script that checks
// for 2 silently starts seeing 1.
func TestRunKeepsTheArgvExitStatus(t *testing.T) {
	_, f := parseMainGo(t)
	run := mustFindFunc(t, f, "Run")

	found := false
	ast.Inspect(run.Body, func(n ast.Node) bool {
		ifStmt, isIf := n.(*ast.IfStmt)
		if !isIf || !callsErrorsIs(ifStmt.Cond, "errFlagsReported") {
			return true
		}
		ast.Inspect(ifStmt.Body, func(inner ast.Node) bool {
			call, isCall := inner.(*ast.CallExpr)
			if !isCall || len(call.Args) != 1 {
				return true
			}
			sel, isSel := call.Fun.(*ast.SelectorExpr)
			if !isSel || sel.Sel.Name != "Exit" {
				return true
			}
			if pkg, isIdent := sel.X.(*ast.Ident); !isIdent || pkg.Name != "os" {
				return true
			}
			if lit, isLit := call.Args[0].(*ast.BasicLit); isLit && lit.Value == "2" {
				found = true
			}
			return true
		})
		return true
	})
	if !found {
		t.Fatal("Run() does not exit 2 on an errFlagsReported error; a bad flag " +
			"used to exit 2 through flag.ExitOnError")
	}
}

// callsErrorsIs reports whether an expression contains errors.Is(_, <ident>).
func callsErrorsIs(e ast.Expr, target string) bool {
	hit := false
	ast.Inspect(e, func(n ast.Node) bool {
		call, isCall := n.(*ast.CallExpr)
		if !isCall || len(call.Args) != 2 {
			return true
		}
		sel, isSel := call.Fun.(*ast.SelectorExpr)
		if !isSel || sel.Sel.Name != "Is" {
			return true
		}
		if pkg, isIdent := sel.X.(*ast.Ident); !isIdent || pkg.Name != "errors" {
			return true
		}
		if id, isIdent := call.Args[1].(*ast.Ident); isIdent && id.Name == target {
			hit = true
		}
		return true
	})
	return hit
}
