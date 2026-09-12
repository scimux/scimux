package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codeberg.org/chrberger/scimux/internal/backend"
)

func TestMuxerBackendStatusAndOwnership(t *testing.T) {
	if _, err := newMuxerBackend(nil); err == nil {
		t.Fatal("accepted nil app")
	}
	a := newTestApp(t, &fakeTmux{})
	m, err := newMuxerBackend(a)
	if err != nil {
		t.Fatal(err)
	}
	stopCalls := 0
	h, err := m.handler(func() { stopCalls++ })
	if err != nil {
		t.Fatal(err)
	}

	bad := httptest.NewRecorder()
	h.ServeHTTP(bad, httptest.NewRequest(http.MethodPost, "/_scimux/status", strings.NewReader(`{}`)))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("bad status report = %d", bad.Code)
	}
	postStatus := func(body string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/_scimux/status", strings.NewReader(body)))
		return rec.Code
	}
	if got := postStatus(`{"generation":4,"version":"v4","remote":"enrolled"}`); got != http.StatusNoContent {
		t.Fatalf("status report = %d", got)
	}
	if got := postStatus(`{"generation":3,"version":"stale","remote":"revoked"}`); got != http.StatusNoContent {
		t.Fatalf("stale report = %d", got)
	}
	if m.status.Version() != "v4" || m.status.HostedStatus() != "enrolled" {
		t.Fatalf("runtime status regressed: version=%q remote=%q", m.status.Version(), m.status.HostedStatus())
	}
	m.enableRemote(true)
	if a.hostedRemote != m.status {
		t.Fatal("remote status projection is not muxer-owned runtime status")
	}
	m.enableRemote(false)
	if a.hostedRemote != nil {
		t.Fatal("disabled remote still projected")
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"version":"v4"`) {
		t.Fatalf("state did not project active web version: %d %s", rec.Code, rec.Body.String())
	}
	for range 2 {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/_scimux/stop", nil))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("stop = %d", rec.Code)
		}
	}
	if stopCalls != 1 {
		t.Fatalf("stop callback calls = %d, want 1", stopCalls)
	}
	withoutStop, err := m.handler(nil)
	if err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	withoutStop.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/_scimux/stop", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured stop = %d", rec.Code)
	}
	m.shutdownHarnesses()

	var nilMuxer *muxerBackend
	if _, err := nilMuxer.handler(nil); err == nil {
		t.Fatal("nil muxer returned a handler")
	}
	nilMuxer.enableRemote(true)
	nilMuxer.shutdownHarnesses()
	var nilStatus *muxerRuntimeStatus
	if nilStatus.HostedStatus() != "" || nilStatus.Version() != version {
		t.Fatal("nil runtime status is not inert")
	}
}

type closeRecorder struct{ calls int }

func (c *closeRecorder) Close() error { c.calls++; return nil }

func TestMuxerShutdownUsesSessionWorkersWhenInstalled(t *testing.T) {
	manager := &workerManager{entries: map[string]*workerEntry{}}
	a := &app{workers: manager}
	(&muxerBackend{app: a}).shutdownHarnesses()
	if a.workers != manager {
		t.Fatal("worker manager changed during shutdown")
	}
}

func TestWebBackendConstructionAndClose(t *testing.T) {
	if _, err := newWebMux(nil, http.NotFoundHandler(), nil); err == nil {
		t.Fatal("web mux accepted nil embedded tree")
	}
	if _, err := newWebMux(webFS, nil, nil); err == nil {
		t.Fatal("web mux accepted nil core capability")
	}
	if _, err := newWebBackend(webBackendConfig{Web: webFS}); err == nil {
		t.Fatal("accepted nil muxer capability")
	}
	if _, err := newWebBackend(webBackendConfig{Core: http.NotFoundHandler()}); err == nil {
		t.Fatal("accepted missing embedded web tree")
	}
	closer := &closeRecorder{}
	w, err := newWebBackend(webBackendConfig{
		Web: webFS, Core: http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
			rw.WriteHeader(http.StatusTeapot)
		}), RemoteClose: closer,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	w.local.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/state", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("web proxy = %d", rec.Code)
	}
	if w.tunnelFor(tunnelPeer{}) == nil {
		t.Fatal("web backend has no tunnel boundary")
	}
	if err := w.Close(); err != nil || closer.calls != 1 {
		t.Fatalf("close = %v, calls=%d", err, closer.calls)
	}
	var nilWeb *webBackend
	if err := nilWeb.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeStatusAcceptsEqualGenerationRefresh(t *testing.T) {
	s := &muxerRuntimeStatus{}
	if s.Version() != version {
		t.Fatal("empty runtime status did not fall back to process version")
	}
	remoteOne, remoteTwo := "unavailable", "enrolled"
	s.accept(backend.Status{Generation: 2, Version: "v2", Remote: &remoteOne})
	s.accept(backend.Status{Generation: 2, Version: "v2", Remote: &remoteTwo})
	if s.HostedStatus() != "enrolled" {
		t.Fatalf("equal-generation remote refresh = %q", s.HostedStatus())
	}
	s.accept(backend.Status{Generation: 3, Version: "v3"})
	if s.HostedStatus() != "enrolled" || s.Version() != "v3" {
		t.Fatal("nil remote update erased hosted state")
	}
}
