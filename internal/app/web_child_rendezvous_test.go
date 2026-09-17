package app

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/backend"
	"codeberg.org/chrberger/scimux/internal/remote"
)

// The web child is the only process that builds a remote client in
// production: the muxer returns before it reaches one. So the config it hands
// to the factory is the whole of what production configures, and a field the
// monolith used to set is simply gone unless this file says otherwise.
//
// Backoff is the field that fails silently. Without it the client enrolls,
// reports "enrolled", mints codes and draws QR codes, and never runs the wait
// loop that registers the pairing waiter — so the rendezvous tells the phone
// the code is not in use, seconds after the computer printed it.
func TestWebChildRemoteConfigRunsRendezvousLoop(t *testing.T) {
	testWebChildRemoteConfig(t, false)
}

func TestReplacementWebChildRetainsViewerOrigin(t *testing.T) {
	testWebChildRemoteConfig(t, true)
}

func testWebChildRemoteConfig(t *testing.T, replacement bool) {
	t.Helper()
	coreMux := http.NewServeMux()
	coreMux.HandleFunc("POST /_scimux/status", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	core, err := backend.Listen("", coreMux)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	readyR, readyW := io.Pipe()
	activateR, activateW := io.Pipe()
	defer activateW.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got := make(chan remote.Config, 1)
	deps := webChildDeps{newRemote: func(cfg remote.Config) webRemoteClient {
		select {
		case got <- cfg:
		default:
		}
		return newFakeWebRemote("enrolled")
	}}
	cfg := webChildConfig{
		Link: core.Link(), Generation: 3, ListenAddr: ln.Addr().String(),
		DataDir: t.TempDir(), Remote: true, Replacement: replacement, RVOrigin: remote.DefaultOrigin,
		ViewerOrigin: "https://viewer.example",
	}
	done := make(chan error, 1)
	go func() {
		defer readyW.Close()
		done <- serveWebChild(ctx, cfg, ln, readyW, activateR, strings.NewReader(""), io.Discard, io.Discard, deps)
	}()

	var ready webChildEvent
	if err := json.NewDecoder(readyR).Decode(&ready); err != nil || ready.Phase != "ready" {
		t.Fatalf("ready = %#v, %v", ready, err)
	}

	var rc remote.Config
	select {
	case rc = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("remote client was never constructed")
	}
	if !rc.RunsRendezvousLoop() {
		t.Fatal("the web child built a remote client that never registers a pairing waiter")
	}
	if rc.Backoff != remote.DefaultBackoff() {
		t.Fatalf("Backoff = %#v, want remote.DefaultBackoff()", rc.Backoff)
	}
	if rc.ViewerOrigin != cfg.ViewerOrigin {
		t.Fatalf("ViewerOrigin = %q, want %q", rc.ViewerOrigin, cfg.ViewerOrigin)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serveWebChild: %v", err)
	}
}
