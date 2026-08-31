package app

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"codeberg.org/chrberger/scimux/internal/remote"
)

// errNoTerminalForJoin3 stands in for the hidden-TTY factory. Join 3 is about
// what a remote startup hands the client, not about enrolment: a run that
// cannot open a terminal still has to build a tunnel boundary on the way.
var errNoTerminalForJoin3 = errors.New("join3: no terminal in tests")

// join3RID is a well-formed rendezvous id, the same shape s3Peer() uses.
var join3RID = strings.Repeat("ab", 32)

// join3TunnelHandler runs the product remote startup and returns the handler
// that run would serve to a proven peer. Every row that inspects the boundary
// goes through here rather than through newBoundaries, because "the
// constructor works" is exactly the thing already proven and exactly the thing
// no product path reaches.
func join3TunnelHandler(t *testing.T) http.Handler {
	t.Helper()
	data := t.TempDir()
	cmd := join3Command(t,
		[]string{"scimux", "--remote", "-data", data, "-addr", "127.0.0.1:0"},
		remote.Config{
			DataDir:       data,
			RendezvousURL: "http://127.0.0.1:1",
			HTTPClient:    unreachableHTTP(),
			NewTerminal:   func() (remote.Terminal, error) { return nil, errNoTerminalForJoin3 },
		})
	if cmd.tunnelHandlerFor == nil {
		t.Fatal("remote startup built no tunnel handler factory")
	}
	h := cmd.tunnelHandlerFor(remote.TunnelPeer{DeviceID: "device-1", RID: join3RID})
	if h == nil {
		t.Fatal("the tunnel handler factory returned nil for a proven peer")
	}
	return h
}
