package app

// Join 3 — the tunnel boundary must be reachable from a paired device.
//
// S3 built two boundary constructors over one owned mux (tunnel_boundary.go):
// withLocalBoundary for the TCP listener and withTunnelBoundary for a proven
// peer. S4 built the codec. S6 built the transport, and join 2 made a live
// session serve whatever handler Config.TunnelHandler names. Nothing joins
// them.
//
// Concretely, `grep -rn newBoundaries --include=*.go internal/app/` outside
// tests returns only the definition. Both product startup paths
// (main.go and remote_command.go) call NewHandler, which returns
// withLocalBoundary(a, mux) alone; remote.Config.TunnelHandler is never
// assigned anywhere in this package. So a device can pair, negotiate a
// session, frame a request through the codec — and the laptop answers
// ClassUnavailable, because tunnelHandler() is nil. Every AT that proves the
// tunnel boundary drives a handler the product never constructs.
//
// These rows assert the join from the product entry point: run Command the
// way main() does, then drive the handler the run actually handed to the
// remote client. They deliberately do not call newBoundaries or
// withTunnelBoundary themselves, because the defect being closed is precisely
// that those exist and no path reaches them.
//
// The pion end of this is already covered by join 2
// (TestLiveSessionCarriesATunnelledRequest in internal/remote): once the
// field is populated with the real boundary, a live data channel serves it.
// This package must not import pion — remote_boundary_guard_test.go scans
// test files too, deliberately.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/remote"
)

// join3Command runs the product startup path against an already-enrolled data
// directory and returns the Command, so a test can inspect what the run handed
// to the remote client. Enrollment failures are not the subject here: the
// tunnel handler must be constructed and handed over on the way to Start,
// whether or not the rendezvous is reachable.
func join3Command(t *testing.T, args []string, cfg remote.Config) *Command {
	t.Helper()
	cmd := &Command{
		Args:   args,
		Stdout: io.Discard,
		Stderr: new(bytes.Buffer),
		Home:   t.TempDir(),
		Config: cfg,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// The error is deliberately not asserted: an unreachable rendezvous is a
	// legitimate outcome of this path and says nothing about the join.
	_ = cmd.Run(ctx)
	return cmd
}

// TestRemoteStartupHandsOverATunnelHandler is the join. Starting with
// --remote must give the remote client a tunnel handler to serve.
func TestRemoteStartupHandsOverATunnelHandler(t *testing.T) {
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
		t.Fatal("startup with --remote handed the remote client no tunnel handler; " +
			"a paired device's session has nothing to serve")
	}
	h := cmd.tunnelHandlerFor(remote.TunnelPeer{DeviceID: "phone", RID: join3RID})
	if h == nil {
		t.Fatal("the tunnel handler factory returned nil for a proven peer")
	}
}

// TestTunnelHandlerServesTheRealRoutes keeps the join honest. A non-nil
// handler proves nothing if it is a stub: it must be the owned mux, so the
// device reaches the same application the browser does.
func TestTunnelHandlerServesTheRealRoutes(t *testing.T) {
	h := join3TunnelHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/state over the tunnel boundary = %d, want 200; body=%q",
			rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct == "" {
		t.Fatal("tunnelled response carries no content type; this is not the owned mux")
	}
}

// TestTunnelHandlerIsNotTheLocalBoundary is the discriminator. Handing over
// NewHandler's local chain would satisfy both rows above while enforcing the
// browser's guards against a peer that cannot satisfy them — the device has
// no Origin, no Sec-Fetch-Site and no CSRF token, so a local boundary would
// reject its every mutation. FR-17 itself is proven at the primitive in
// at_fr17_test.go with a spy injected straight into withTunnelBoundary; what
// is new here is that the product path chooses that constructor at all, which
// a header-less request answered 200 rather than 403 establishes.
func TestTunnelHandlerIsNotTheLocalBoundary(t *testing.T) {
	h := join3TunnelHandler(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, s3ChannelRequest(http.MethodGet, "/api/state", ""))

	// FR-28: the remote policy, stamped by the tunnel boundary and never the
	// browser one. The local boundary stamps a different CSP.
	if got := rec.Header().Get("Content-Security-Policy"); got != remoteCSP {
		t.Fatalf("tunnelled CSP = %q, want the FR-28 remote policy %q", got, remoteCSP)
	}
	for _, name := range []string{"X-Content-Type-Options", "Referrer-Policy", "Permissions-Policy"} {
		if rec.Header().Get(name) == "" {
			t.Errorf("tunnelled response is missing FR-28 header %s", name)
		}
	}

	if rec.Code == http.StatusForbidden {
		t.Fatalf("a header-less GET was rejected 403: the local boundary is in the chain, "+
			"and no device can satisfy guards it has no browser for; body=%q", rec.Body.String())
	}

	// The same fact on the unsafe side, where the local chain's mutation
	// guard lives: no CSRF token, no Origin, no Sec-Fetch-Site.
	post := s3ChannelRequest(http.MethodPost, "/api/nodes/node-1/send", "{}")
	prec := httptest.NewRecorder()
	h.ServeHTTP(prec, post)
	if prec.Code == http.StatusForbidden {
		t.Fatalf("a header-less POST was rejected 403: guardMutations is still in the tunnel chain; body=%q",
			prec.Body.String())
	}
}

// TestLocalStartupHandsOverNoTunnelHandler is the control. Without it the
// join row would pass against a build that serves a tunnel to nobody — and a
// purely local run must stay byte-for-byte what it is today (FR-15).
func TestLocalStartupHandsOverNoTunnelHandler(t *testing.T) {
	data := t.TempDir()
	cmd := join3Command(t,
		[]string{"scimux", "-data", data, "-addr", "127.0.0.1:0"},
		remote.Config{DataDir: data})

	if cmd.tunnelHandlerFor != nil {
		t.Fatal("a local run built a tunnel boundary; remote is a runtime switch and this one is off")
	}
}
