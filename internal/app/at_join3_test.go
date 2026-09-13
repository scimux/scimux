package app

// Join 3 — the tunnel boundary must be reachable from the production web
// child. Constructor-only tests cannot prove this join: the remote client must
// receive the same handler factory that serveWebChild builds and owns.

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/backend"
	"codeberg.org/chrberger/scimux/internal/remote"
)

func TestWebChildJoinsRemoteTunnelToRealRoutes(t *testing.T) {
	tunnelFor := join3WebChildTunnelFactory(t, true)
	if tunnelFor == nil {
		t.Fatal("production web child handed the remote client no tunnel handler")
	}
	h := tunnelFor(remote.TunnelPeer{DeviceID: "phone", RID: strings.Repeat("ab", 32)})
	if h == nil {
		t.Fatal("production tunnel handler factory returned nil for a proven peer")
	}

	// A non-nil handler is insufficient: it must reach the muxer's real API.
	state := httptest.NewRecorder()
	h.ServeHTTP(state, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	if state.Code != http.StatusOK {
		t.Fatalf("GET /api/state over production tunnel = %d, want 200; body=%q", state.Code, state.Body.String())
	}
	if state.Header().Get("Content-Type") == "" {
		t.Fatal("tunnelled state response has no content type; handler is not the muxer route")
	}

	// A local handler would reject a remote browser, which has no local Origin,
	// Fetch Metadata, or CSRF token. The tunnel handler instead stamps FR-28's
	// remote policy and admits the already-authenticated peer.
	if got := state.Header().Get("Content-Security-Policy"); got != remoteCSP {
		t.Fatalf("tunnelled CSP = %q, want %q", got, remoteCSP)
	}
	for _, name := range []string{"X-Content-Type-Options", "Referrer-Policy", "Permissions-Policy"} {
		if state.Header().Get(name) == "" {
			t.Errorf("tunnelled response is missing FR-28 header %s", name)
		}
	}
	post := httptest.NewRecorder()
	h.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/api/nodes/node-1/send", strings.NewReader(`{}`)))
	if post.Code == http.StatusForbidden {
		t.Fatalf("production tunnel used the local mutation boundary: %d %q", post.Code, post.Body.String())
	}
}

func TestLocalWebChildHandsOverNoTunnelFactory(t *testing.T) {
	if tunnelFor := join3WebChildTunnelFactory(t, false); tunnelFor != nil {
		t.Fatal("local-only web child constructed a remote tunnel factory")
	}
}

func join3WebChildTunnelFactory(t *testing.T, remoteEnabled bool) func(remote.TunnelPeer) http.Handler {
	t.Helper()
	a := newTestApp(t, &fakeTmux{})
	muxer, err := newMuxerBackend(a)
	if err != nil {
		t.Fatal(err)
	}
	coreHandler, err := muxer.handler(nil)
	if err != nil {
		t.Fatal(err)
	}
	core, err := backend.Listen(t.TempDir(), coreHandler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	readyR, readyW := io.Pipe()
	activateR, activateW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var tunnelFor func(remote.TunnelPeer) http.Handler
	deps := webChildDeps{newRemote: func(cfg remote.Config) webRemoteClient {
		tunnelFor = cfg.TunnelHandlerFor
		return newFakeWebRemote("enrolled")
	}}
	cfg := webChildConfig{
		Link: core.Link(), Generation: 1, ListenAddr: ln.Addr().String(),
		DataDir: t.TempDir(), Remote: remoteEnabled, CSRFToken: mustToken(),
	}
	go func() {
		defer readyW.Close()
		done <- serveWebChild(ctx, cfg, ln, readyW, activateR, strings.NewReader(""), io.Discard, io.Discard, deps)
	}()
	dec := json.NewDecoder(readyR)
	var ready webChildEvent
	if err := dec.Decode(&ready); err != nil || ready.Phase != "ready" {
		cancel()
		t.Fatalf("production web child readiness = %#v, %v", ready, err)
	}
	if _, err := activateW.Write([]byte{1}); err != nil {
		cancel()
		t.Fatal(err)
	}
	_ = activateW.Close()
	var active webChildEvent
	if err := dec.Decode(&active); err != nil || active.Phase != "active" {
		cancel()
		t.Fatalf("production web child activation = %#v, %v", active, err)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("stop production web child: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("production web child did not stop")
		}
	})
	return tunnelFor
}
