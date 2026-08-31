package app

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/remote"
)

// The rendezvous seam. Until this file, remote.Config.Origin was assigned
// nowhere in product code and remote.Config.RendezvousURL only by tests, so
// every build of the binary was hardwired to remote.DefaultOrigin. That is not
// a default anybody chose: it is the absence of an operator path, and it made
// a self-hosted rendezvous unreachable from the product while eight acceptance
// files talked to httptest servers and looked healthy.
//
// Origin is the field that carries the change, not RendezvousURL, because
// origin is inside the pairing transcript (rendezvous-v1 §11) and rvBase()
// already falls back to it. One flag therefore moves both the address requests
// go to and the identity string both ends bind, and the two cannot drift apart
// by an operator setting only one of them.

// runCommand parses argv through Run() and returns the Command so the caller
// can inspect the remote.Config that flag parsing produced. It deliberately
// omits --remote: flags are parsed before any remote work, so this stays
// hermetic and touches no network.
func runCommand(t *testing.T, args ...string) *Command {
	t.Helper()
	data := t.TempDir()
	argv := append([]string{"scimux", "-data", data, "-addr", "127.0.0.1:0"}, args...)
	cmd := &Command{
		Args:   argv,
		Stdout: io.Discard,
		Stderr: new(bytes.Buffer),
		Home:   t.TempDir(),
		Config: remote.Config{DataDir: data, HTTPClient: unreachableHTTP()},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cmd.Run(ctx); err != nil {
		t.Fatalf("run %v: %v", argv, err)
	}
	return cmd
}

// TestRendezvousURLFlagReachesRemoteConfig is the whole point of the seam: an
// operator naming their own rendezvous must reach the field internal/remote
// actually reads.
func TestRendezvousURLFlagReachesRemoteConfig(t *testing.T) {
	cmd := runCommand(t, "--rendezvous-url", "https://rv.example")
	if cmd.Config.Origin != "https://rv.example" {
		t.Fatalf("--rendezvous-url set Config.Origin = %q, want %q.\n"+
			"Without this the binary can only ever talk to remote.DefaultOrigin.",
			cmd.Config.Origin, "https://rv.example")
	}
}

// TestRendezvousURLDefaultsToTheDefaultOrigin pins that omitting the flag
// changes nothing: the field stays empty and internal/remote supplies
// DefaultOrigin, exactly as before the flag existed.
func TestRendezvousURLDefaultsToTheDefaultOrigin(t *testing.T) {
	cmd := runCommand(t)
	if cmd.Config.Origin != "" {
		t.Fatalf("without --rendezvous-url Config.Origin = %q, want empty so "+
			"remote.DefaultOrigin applies", cmd.Config.Origin)
	}
}

// TestRendezvousURLIsNormalised trims the trailing slash an operator will
// paste from a browser. rvBase() is concatenated with "/v1/..." paths, so a
// trailing slash would produce "//v1" — which some servers route and some
// reject, and neither outcome is the one the operator typed.
func TestRendezvousURLIsNormalised(t *testing.T) {
	cmd := runCommand(t, "--rendezvous-url", "https://rv.example/")
	if cmd.Config.Origin != "https://rv.example" {
		t.Fatalf("Config.Origin = %q, want the trailing slash trimmed", cmd.Config.Origin)
	}
}

// TestRendezvousURLMustBeAbsolute refuses at startup rather than at the first
// request. A bare host name is the likely typo, and the failure it causes
// otherwise surfaces much later as an unexplained dial error.
func TestRendezvousURLMustBeAbsolute(t *testing.T) {
	for _, bad := range []string{"rv.example", "ftp://rv.example", "/v1"} {
		data := t.TempDir()
		cmd := &Command{
			Args:   []string{"scimux", "-data", data, "-addr", "127.0.0.1:0", "--rendezvous-url", bad},
			Stdout: io.Discard,
			Stderr: new(bytes.Buffer),
			Home:   t.TempDir(),
			Config: remote.Config{DataDir: data, HTTPClient: unreachableHTTP()},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := cmd.Run(ctx)
		cancel()
		if err == nil {
			t.Fatalf("--rendezvous-url %q was accepted; want an http(s) URL requirement", bad)
		}
		if !strings.Contains(err.Error(), "rendezvous-url") {
			t.Fatalf("--rendezvous-url %q failed with %v, which does not name the flag", bad, err)
		}
	}
}
