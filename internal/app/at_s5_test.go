package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"go/ast"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/remote"
)

// storedEnrollmentKeypair is a deterministic Ed25519 identity used only by
// the stored-enrollment fixture. The private key's public half matches the
// stored public key; the pair is not a live secret.
func storedEnrollmentKeypair() (pubHex, privHex string) {
	seed := make([]byte, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	return hex.EncodeToString(pub), hex.EncodeToString(priv)
}

// AT-FR-01-a: no --remote ⇒ zero remote-state work, zero rendezvous requests.
func TestAT_FR_01_a_NoRemoteFlagZeroContact(t *testing.T) {
	data := t.TempDir()
	var remoteInits int
	stderr := new(bytes.Buffer)
	cmd := &Command{
		Args:   []string{"scimux", "-data", data, "-addr", "127.0.0.1:0"},
		Stdout: io.Discard,
		Stderr: stderr,
		Home:   t.TempDir(),
		Config: remote.Config{
			DataDir:       data,
			RendezvousURL: "http://127.0.0.1:1",
			HTTPClient:    unreachableHTTP(),
			Hooks: remote.Hooks{
				OnRemoteInit: func() { remoteInits++ },
				OnStateLoad:  func() { remoteInits++ },
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cmd.Run(ctx); err != nil {
		t.Fatalf("AT-FR-01-a: startup without --remote: %v", err)
	}
	if remoteInits != 0 {
		t.Fatalf("AT-FR-01-a: remote-state work ran %d times without --remote", remoteInits)
	}
	if _, err := os.Stat(filepath.Join(data, remote.PrivateDirName)); !os.IsNotExist(err) {
		t.Fatal("AT-FR-01-a: remote private directory created without --remote")
	}
	h := cmd.Handler()
	if h == nil {
		t.Fatal("AT-FR-01-a: localhost handler missing")
	}
	rec := routeRequest(h, http.MethodGet, "/api/state", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("AT-FR-01-a: GET /api/state = %d, want 200", rec.Code)
	}
}

// AT-FR-01-b: --remote with no enrollment and no invite fails locally with
// setup guidance and no network. Stored enrollment needs no invite.
func TestAT_FR_01_b_RemoteWithoutInviteFailsLocally(t *testing.T) {
	t.Run("no-enrollment-no-invite", func(t *testing.T) {
		data := t.TempDir()
		var enrolls int
		cmd := &Command{
			Args:   []string{"scimux", "--remote", "-data", data, "-addr", "127.0.0.1:0"},
			Stdout: io.Discard,
			Stderr: new(bytes.Buffer),
			Home:   t.TempDir(),
			Config: remote.Config{
				DataDir:       data,
				RendezvousURL: "http://127.0.0.1:1",
				HTTPClient:    unreachableHTTP(),
				Hooks: remote.Hooks{
					OnEnrollAttempt: func() { enrolls++ },
				},
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := cmd.Run(ctx)
		if err == nil {
			t.Fatal("AT-FR-01-b: --remote with no enrollment succeeded")
		}
		if errors.Is(err, remote.ErrUnimplemented) {
			t.Fatal("AT-FR-01-b: unimplemented, not setup guidance")
		}
		var re *remote.Error
		if !errors.As(err, &re) || re.Class != remote.ClassNeedInvite {
			t.Fatalf("AT-FR-01-b: want ClassNeedInvite, got %v", err)
		}
		g := strings.ToLower(re.Guidance + err.Error())
		for _, w := range []string{"invite", "--invite-file", "--invite-stdin"} {
			if !strings.Contains(g, w) {
				t.Errorf("AT-FR-01-b: setup guidance missing %q: %v", w, err)
			}
		}
		if enrolls != 0 {
			t.Fatal("AT-FR-01-b: enroll attempted without invite")
		}
	})

	t.Run("stored-enrollment-no-invite", func(t *testing.T) {
		data := t.TempDir()
		c := remote.NewClient(remote.Config{DataDir: data, Remote: true})
		if err := os.MkdirAll(c.PrivateDir(), 0o700); err != nil {
			t.Fatal(err)
		}
		pubHex, privHex := storedEnrollmentKeypair()
		raw := []byte(`{"v":1,"status":"enrolled","handle":"ih_041061050R3GG28A","public_key":"` + pubHex + `","private_key":"` + privHex + `","origin":"https://my.scimux.eu"}`)
		if err := os.WriteFile(c.StatePath(), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := &Command{
			Args:   []string{"scimux", "--remote", "-data", data, "-addr", "127.0.0.1:0"},
			Stdout: io.Discard,
			Stderr: new(bytes.Buffer),
			Home:   t.TempDir(),
			Config: remote.Config{
				DataDir:       data,
				RendezvousURL: "http://127.0.0.1:1",
				HTTPClient:    unreachableHTTP(),
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := cmd.Run(ctx); err != nil {
			t.Fatalf("AT-FR-01-b: --remote with stored enrollment: %v", err)
		}
	})
}

// TestRunDelegatesToRemoteAwareCommand pins production entry-point
// integration: app.Run() must construct Command and call Command.Run, the
// same seam the S5 ATs exercise. Hidden hook subcommands must still
// dispatch before that remote initialization. This is red until R2 wires it.
func TestRunDelegatesToRemoteAwareCommand(t *testing.T) {
	fset, f := parseMainGo(t)
	run := mustFindFunc(t, f, "Run")
	found, firstInit := readArgvDispatch(t, fset, run, packageStringConsts(t))
	if len(found) == 0 {
		t.Fatal("no hidden subcommands discovered from Run()")
	}

	var commandLitLine, commandRunLine int
	ast.Inspect(run.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.UnaryExpr:
			if lit, ok := x.X.(*ast.CompositeLit); ok && typeName(lit.Type) == "Command" {
				commandLitLine = fset.Position(x.Pos()).Line
			}
		case *ast.CompositeLit:
			if typeName(x.Type) == "Command" && commandLitLine == 0 {
				commandLitLine = fset.Position(x.Pos()).Line
			}
		case *ast.CallExpr:
			sel, ok := x.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Run" {
				return true
			}
			commandRunLine = fset.Position(x.Pos()).Line
		}
		return true
	})
	if commandLitLine == 0 {
		t.Fatal("app.Run() does not construct Command — the AT seam and the executable are separable")
	}
	if commandRunLine == 0 {
		t.Fatal("app.Run() does not call Command.Run — the AT seam and the executable are separable")
	}
	for _, d := range found {
		if commandLitLine <= d.line || commandRunLine <= d.line {
			t.Errorf("Command.Run at main.go:%d is at or before hook %s (line %d); hooks must dispatch before remote initialization",
				commandRunLine, d.name, d.line)
		}
		if firstInit >= 0 && d.index >= firstInit {
			t.Errorf("hook %s is not before first init (FR-01)", d.name)
		}
	}
}

func typeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return t.Sel.Name
	}
	return ""
}

func TestAT_FR_01_HiddenSubcommandsSkipRemoteInit(t *testing.T) {
	fset, f := parseMainGo(t)
	run := mustFindFunc(t, f, "Run")
	found, _ := readArgvDispatch(t, fset, run, packageStringConsts(t))
	if len(found) == 0 {
		t.Fatal("no hidden subcommands discovered from Run()")
	}
	for _, d := range found {
		t.Run(d.name, func(t *testing.T) {
			var remoteInits, dispatches int
			cmd := &Command{
				Args:   []string{"scimux", d.name},
				Stdin:  strings.NewReader("{}\n"),
				Stdout: io.Discard,
				Stderr: io.Discard,
				Config: remote.Config{
					RendezvousURL: "http://127.0.0.1:1",
					HTTPClient:    unreachableHTTP(),
					Hooks: remote.Hooks{
						OnRemoteInit:   func() { remoteInits++ },
						OnHookDispatch: func(string) { dispatches++ },
					},
				},
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = cmd.Run(ctx)
			if remoteInits != 0 {
				t.Fatalf("hidden subcommand %s performed remote init", d.name)
			}
			if dispatches == 0 {
				t.Fatalf("hidden subcommand %s did not dispatch before remote initialization", d.name)
			}
		})
	}
}

// AT-NFR-16-a: rendezvous permanently unreachable; localhost startup and a
// representative local handler request still succeed.
func TestAT_NFR_16_a_LocalhostWorksWhenRendezvousUnreachable(t *testing.T) {
	data := t.TempDir()
	cmd := &Command{
		Args:   []string{"scimux", "-data", data, "-addr", "127.0.0.1:0"},
		Stdout: io.Discard,
		Stderr: new(bytes.Buffer),
		Home:   t.TempDir(),
		Config: remote.Config{
			DataDir:       data,
			RendezvousURL: "http://127.0.0.1:1",
			HTTPClient:    unreachableHTTP(),
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cmd.Run(ctx); err != nil {
		t.Fatalf("AT-NFR-16-a: localhost startup: %v", err)
	}
	h := cmd.Handler()
	if h == nil {
		t.Fatal("AT-NFR-16-a: localhost handler is nil")
	}
	rec := routeRequest(h, http.MethodGet, "/api/state", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("AT-NFR-16-a: GET /api/state = %d body=%q", rec.Code, rec.Body.String())
	}
}

func TestAT_FR_32_a_DisableAllLeavesLocalhost(t *testing.T) {
	data := t.TempDir()
	a := newTestApp(t, &fakeTmux{})
	h := newTestHandler(t, a)
	cmd := &Command{
		Args:   []string{"scimux", "--remote", "-data", data, "-addr", "127.0.0.1:0"},
		Stdout: io.Discard,
		Stderr: new(bytes.Buffer),
		Home:   t.TempDir(),
		Config: remote.Config{DataDir: data, Remote: true},
	}
	cmd.handler = h
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cmd.DisableAll(ctx); err != nil {
		t.Fatalf("AT-FR-32-a: disable-all: %v", err)
	}
	rec := routeRequest(h, http.MethodGet, "/api/state", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("AT-FR-32-a: localhost GET /api/state after disable-all = %d", rec.Code)
	}
	rec = routeRequest(h, http.MethodGet, "/api/agents", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("AT-FR-32-a: localhost GET /api/agents after disable-all = %d", rec.Code)
	}
}

func unreachableHTTP() *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("rendezvous unreachable")
		}),
		Timeout: 50 * time.Millisecond,
	}
}
