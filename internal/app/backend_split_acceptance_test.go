package app

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"codeberg.org/chrberger/scimux/internal/backend"
)

// TestWebGenerationChangePreservesInFlightPermission is the first backend
// split acceptance test: the replaceable web side is rebuilt from scratch
// while the muxer-owned app and its structured transport stay alive. A
// pending permission is useful here because it is both an in-flight turn and
// an externally named piece of harness state; preserving only the node would
// be too weak a test.
func TestWebGenerationChangePreservesInFlightPermission(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "split-pending", "pi", "acp")
	stub := &stubProc{
		live:           "active",
		hasSession:     true,
		hasPending:     true,
		pending:        allowPending("incarnation-a:7"),
		clearOnDeliver: true,
	}
	a.testProc = stub

	coreMux, err := newCoreMux(a)
	if err != nil {
		t.Fatal(err)
	}
	core, err := backend.Listen(t.TempDir(), coreMux)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })

	first := newTestWebGeneration(t, a, core.Link(), nil)
	before := splitChat(t, first, n.ID)
	if before["turn_in_flight"] != true || before["perm_request_id"] != "incarnation-a:7" {
		t.Fatalf("generation 1 lost in-flight permission: %#v", before)
	}

	// Constructing a new public gateway models replacement of every
	// web-owned object. Only the muxer app and its private Unix-socket API are
	// shared between generations.
	second := newTestWebGeneration(t, a, core.Link(), nil)
	after := splitChat(t, second, n.ID)
	if after["turn_in_flight"] != true || after["perm_request_id"] != "incarnation-a:7" {
		t.Fatalf("generation 2 lost in-flight permission: %#v", after)
	}
	if stub.shutdownCalls.Load() != 0 {
		t.Fatalf("web replacement shut down structured transports %d times", stub.shutdownCalls.Load())
	}

	body := `{"key":"1","request_id":"incarnation-a:7"}`
	rec := routeRequest(second, http.MethodPost, "/api/nodes/"+n.ID+"/key", body, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("answer through generation 2 = %d (%s)", rec.Code, rec.Body.String())
	}
	if stub.deliverCalls != 1 || stub.lastPrepareReqID != "incarnation-a:7" {
		t.Fatalf("permission delivery = %d for %q, want once for original request",
			stub.deliverCalls, stub.lastPrepareReqID)
	}
}

func splitChat(t *testing.T, h http.Handler, id string) map[string]any {
	t.Helper()
	rec := routeRequest(h, http.MethodGet, "/api/nodes/"+id+"/chat", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET chat = %d (%s)", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func newTestWebGeneration(t *testing.T, a *app, link backend.Link, pairing hostedPairingClient) http.Handler {
	t.Helper()
	client, err := backend.NewClient(link)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.Hello(context.Background()); err != nil {
		t.Fatal(err)
	}
	w, err := newWebBackend(webBackendConfig{
		Web: webFS, Core: client.Proxy(), RequestPolicy: a.requestPolicy, Pairing: pairing,
	})
	if err != nil {
		t.Fatal(err)
	}
	return w.local
}
