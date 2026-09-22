package app

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/remote"
)

// The S9 real run found two ways startup lies to the operator, and both cost
// something irreplaceable. These are the tests that would have caught them.

// TestListenBeforeInvite is finding 3 of the 2026-08-27 run. An invite is
// single-use: redeeming it is the one step of startup that cannot be retried.
// The run that spent invite B then failed on a port already in use, so the
// operator paid an invite for a process that never served a request.
//
// The listener is the cheapest thing that can fail and the only one that is
// contended, so it is bound first. This asserts the order at the seam the
// remote client itself announces: with the port occupied, Run must fail and
// OnRemoteInit must never fire.
func TestListenBeforeInvite(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy a port: %v", err)
	}
	defer busy.Close()

	data := t.TempDir()
	var remoteInits int
	cmd := &Command{
		ExperimentalRemote: true,
		Args:               []string{"scimux", "--remote", "-data", data, "-addr", busy.Addr().String()},
		Stdout:             io.Discard,
		Stderr:             new(bytes.Buffer),
		Home:               t.TempDir(),
		Config: remote.Config{
			DataDir:       data,
			RendezvousURL: "http://127.0.0.1:1",
			HTTPClient:    unreachableHTTP(),
			// Never reached if the bind check works, which is exactly what
			// this asserts — so it is supplied to make the failure mode
			// legible rather than a prompt on the developer's own tty.
			NewTerminal: noTestTerminal,
			Hooks:       remote.Hooks{OnRemoteInit: func() { remoteInits++ }},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = cmd.Run(ctx)
	if err == nil {
		t.Fatal("startup succeeded on an address already in use; the failure would arrive after the invite was spent")
	}
	if !strings.Contains(err.Error(), busy.Addr().String()) {
		t.Fatalf("the bind failure does not name the address the operator has to fix: %v", err)
	}
	if remoteInits != 0 {
		t.Fatalf("remote startup ran %d times before the listener was proven; an invite can be redeemed by a run that cannot serve", remoteInits)
	}
}

// TestListenerIsHandedToTheCaller is the other half: binding early is only
// safe if the bound listener is the one the server goes on to use. A second
// bind of the same address would fail in production exactly as it did here.
func TestListenerIsHandedToTheCaller(t *testing.T) {
	data := t.TempDir()
	cmd := &Command{
		Args:   []string{"scimux", "-data", data, "-addr", "127.0.0.1:0"},
		Stdout: io.Discard,
		Stderr: new(bytes.Buffer),
		Home:   t.TempDir(),
		Config: remote.Config{DataDir: data},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := cmd.Run(ctx); err != nil {
		t.Fatalf("local run: %v", err)
	}
	ln := cmd.Listener()
	if ln == nil {
		t.Fatal("Run bound no listener; main would bind a second time and the early check would prove nothing")
	}
	defer ln.Close()
	if _, ok := ln.Addr().(*net.TCPAddr); !ok {
		t.Fatalf("listener is not a TCP address: %T", ln.Addr())
	}
	if strings.HasSuffix(ln.Addr().String(), ":0") {
		t.Fatal("listener reports port 0; the resolved port is what the operator is told to open")
	}
}

// TestDegradedStartSaysWhy is finding 2. Remote can be revoked, disabled or
// unavailable and scimux still starts — that is deliberate, because losing
// remote access must not lock an operator out of their own chats. What was
// not deliberate is the silence: the class was mapped to a nil error and the
// reason discarded, so a revoked computer started looking exactly like a
// healthy one. The status API knew; the person reading the terminal did not.
func TestDegradedStartSaysWhy(t *testing.T) {
	data := t.TempDir()
	writeEnrolledState(t, data)
	dis := remote.NewClient(remote.Config{DataDir: data, Remote: true})
	if err := dis.DisableAll(context.Background()); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := dis.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	stderr := new(bytes.Buffer)
	cmd := &Command{
		ExperimentalRemote: true,
		Args:               []string{"scimux", "--remote", "-data", data, "-addr", "127.0.0.1:0"},
		Stdout:             io.Discard,
		Stderr:             stderr,
		Home:               t.TempDir(),
		Config: remote.Config{
			DataDir:       data,
			Remote:        true,
			RendezvousURL: "http://127.0.0.1:1",
			HTTPClient:    unreachableHTTP(),
			NewTerminal:   noTestTerminal,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := cmd.Run(ctx); err != nil {
		t.Fatalf("a disabled remote must not stop scimux from starting: %v", err)
	}
	out := stderr.String()
	if !strings.Contains(out, "remote access is off") {
		t.Fatalf("a degraded start said nothing about remote being off:\n%s", out)
	}
	if !strings.Contains(out, "--remote") && !strings.Contains(out, "scimux") {
		t.Fatalf("the notice does not identify itself as scimux's:\n%s", out)
	}
}

// TestDegradedStartNeverPrintsSecrets guards the notice the same way every
// other operator-facing remote string is guarded: Guidance may name a state,
// never an invite, a key or a rendezvous id.
func TestDegradedStartNeverPrintsSecrets(t *testing.T) {
	data := t.TempDir()
	writeEnrolledState(t, data)
	dis := remote.NewClient(remote.Config{DataDir: data, Remote: true})
	if err := dis.DisableAll(context.Background()); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := dis.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	stderr := new(bytes.Buffer)
	cmd := &Command{
		ExperimentalRemote: true,
		Args:               []string{"scimux", "--remote", "-data", data, "-addr", "127.0.0.1:0"},
		Stdout:             io.Discard,
		Stderr:             stderr,
		Home:               t.TempDir(),
		Config: remote.Config{
			DataDir:       data,
			Remote:        true,
			RendezvousURL: "http://127.0.0.1:1",
			HTTPClient:    unreachableHTTP(),
			NewTerminal:   noTestTerminal,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := cmd.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, banned := range []string{"private", "secret", "invite:", "handle"} {
		if strings.Contains(strings.ToLower(stderr.String()), banned) {
			t.Fatalf("degraded-start notice mentions %q:\n%s", banned, stderr.String())
		}
	}
}
