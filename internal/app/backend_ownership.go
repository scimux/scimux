package app

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"sync"

	"github.com/scimux/scimux/internal/backend"
)

// muxerBackend owns global metadata and the private routing registry. Each
// session worker owns its agent connection and log writer; a web generation
// receives only the muxer's HTTP capability, never either owner's state.
type muxerBackend struct {
	app    *app
	status *muxerRuntimeStatus
}

// newCoreMux exposes the established route table only beneath /api/. newMux
// remains the single inventory and the frozen monolithic oracle; the wrapper
// makes its embedded static handlers unreachable from the muxer socket.
func newCoreMux(a *app) (http.Handler, error) {
	mux, err := newMux(a, webFS)
	if err != nil {
		return nil, err
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	}), nil
}

// newWebMux owns every byte interpreted by a browser: the embedded page and
// assets, update metadata/licenses linked into this web generation, and the
// rendezvous/WebRTC control surface. All harness and store APIs cross Core.
// A catch-all proxy rather than a duplicated API inventory preserves the
// exact 404/405 behavior of the one route table in newMux.
func newWebMux(web fs.FS, core http.Handler, pairing hostedPairingClient) (*http.ServeMux, error) {
	if web == nil {
		return nil, errors.New("web backend: nil web filesystem")
	}
	if core == nil {
		return nil, errors.New("web backend: nil muxer handler")
	}
	webHandlers, err := newWebHandlers(web)
	if err != nil {
		return nil, err
	}
	remoteHTTP := remoteHandlers{client: func() hostedPairingClient { return pairing }}

	mux := http.NewServeMux()
	mux.Handle("GET /assets", http.RedirectHandler("/assets/", http.StatusMovedPermanently))
	mux.Handle("GET /assets/", webHandlers.assets)
	mux.Handle("GET /css/", webHandlers.css)
	mux.Handle("GET /js/", webHandlers.js)
	mux.Handle("GET /{$}", webHandlers.index)
	mux.HandleFunc("GET /api/update/check", handleUpdateCheck)
	mux.HandleFunc("GET /api/licenses", handleLicenses)
	if pairing != nil {
		mux.HandleFunc("POST /api/remote/pairing", remoteHTTP.serveRemotePairingMint)
		mux.HandleFunc("GET /api/remote/pairing/{code}", remoteHTTP.serveRemotePairingState)
		mux.HandleFunc("POST /api/remote/pairing/{code}/confirm", remoteHTTP.serveRemotePairingConfirm)
		mux.HandleFunc("POST /api/remote/pairing/{code}/cancel", remoteHTTP.serveRemotePairingCancel)
		mux.HandleFunc("GET /api/remote/devices", remoteHTTP.serveRemoteDeviceList)
		mux.HandleFunc("PATCH /api/remote/devices/{id}", remoteHTTP.serveRemoteDeviceRename)
		mux.HandleFunc("DELETE /api/remote/devices/{id}", remoteHTTP.serveRemoteDeviceRevoke)
		mux.HandleFunc("POST /api/remote/unenroll", remoteHTTP.serveRemoteUnenroll)
		mux.HandleFunc("GET /api/remote/status", remoteHTTP.serveRemoteStatus)
		mux.HandleFunc("GET /api/remote/bootstrap", serveRemoteBootstrapManifest)
	} else {
		// Do not let an inactive experimental surface fall through to the core
		// mux, whose frozen monolithic route inventory still contains these paths.
		// The exact-root handler suppresses net/http's automatic slash redirect;
		// together these keep every method and unknown child uniformly absent
		// rather than exposing the experiment through 405 responses.
		mux.Handle("/api/remote", http.NotFoundHandler())
		mux.Handle("/api/remote/", http.NotFoundHandler())
	}
	mux.Handle("/api/", core)
	return mux, nil
}

func newMuxerBackend(a *app) (*muxerBackend, error) {
	if a == nil {
		return nil, errors.New("muxer backend: nil app")
	}
	status := &muxerRuntimeStatus{version: version}
	a.runtimeStatus = status
	return &muxerBackend{app: a, status: status}, nil
}

func (m *muxerBackend) handler(requestStop func()) (http.Handler, error) {
	if m == nil || m.app == nil {
		return nil, errors.New("muxer backend: nil app")
	}
	core, err := newCoreMux(m.app)
	if err != nil {
		return nil, err
	}
	var stopOnce sync.Once
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/_scimux/status" {
			var report backend.Status
			if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&report); err != nil ||
				report.Generation == 0 || report.Version == "" {
				http.Error(w, "invalid web status", http.StatusBadRequest)
				return
			}
			m.status.accept(report)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/_scimux/stop" {
			if requestStop == nil {
				http.Error(w, "muxer stop unavailable", http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			stopOnce.Do(requestStop)
			return
		}
		core.ServeHTTP(w, r)
	}), nil
}

func (m *muxerBackend) enableRemote(enabled bool) {
	if m == nil || m.app == nil {
		return
	}
	if enabled {
		m.app.hostedRemote = m.status
		return
	}
	m.app.hostedRemote = nil
}

// shutdownHarnesses belongs only to full muxer shutdown. Web replacement has
// no reference to this method or to the managers it closes.
func (m *muxerBackend) shutdownHarnesses() {
	if m == nil || m.app == nil {
		return
	}
	if m.app.workers != nil {
		m.app.workers.Shutdown()
		return
	}
	m.app.acp.Shutdown()
	m.app.codex.Shutdown()
	m.app.muse.Shutdown()
}

// releaseHarnessesAfterStartupFailure must not convert a bad new web/muxer
// generation into lost chats. Production workers are detached and remain
// discoverable; only the legacy in-process test/monolith path is shut down.
func (m *muxerBackend) releaseHarnessesAfterStartupFailure() {
	if m == nil || m.app == nil {
		return
	}
	if m.app.workers != nil {
		m.app.workers.Detach()
		return
	}
	m.app.acp.Shutdown()
	m.app.codex.Shutdown()
	m.app.muse.Shutdown()
}

type muxerRuntimeStatus struct {
	mu         sync.Mutex
	generation uint64
	version    string
	remote     string
}

func (s *muxerRuntimeStatus) accept(report backend.Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if report.Generation < s.generation {
		return
	}
	s.generation = report.Generation
	s.version = report.Version
	if report.Remote != nil {
		s.remote = *report.Remote
	}
}

func (s *muxerRuntimeStatus) HostedStatus() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.remote
}

func (s *muxerRuntimeStatus) Version() string {
	if s == nil {
		return version
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.version == "" {
		return version
	}
	return s.version
}

// activeWebVersion keeps muxer-owned APIs aligned with the serving child during
// the response-safe interval after standby activation and before muxer exec.
func (a *app) activeWebVersion() string {
	if a.runtimeStatus != nil {
		return a.runtimeStatus.Version()
	}
	return version
}

// webBackendConfig is deliberately capability-shaped. Core is an HTTP
// capability to the muxer, Pairing is the remote client owned by this web
// generation, and neither grants access to harness manager state.
type webBackendConfig struct {
	Web           fs.FS
	Core          http.Handler
	RequestPolicy *requestPolicy
	Pairing       hostedPairingClient
	RemoteClose   io.Closer
}

// webBackend is the replaceable half. It owns presentation, browser security,
// and the optional rendezvous/WebRTC client. local and tunnel share one route
// mux so a remote browser sees the same generation of every embedded asset.
type webBackend struct {
	local       http.Handler
	tunnelFor   func(tunnelPeer) http.Handler
	remoteClose io.Closer
}

func newWebBackend(cfg webBackendConfig) (*webBackend, error) {
	if cfg.Core == nil {
		return nil, errors.New("web backend: nil muxer handler")
	}
	mux, err := newWebMux(cfg.Web, cfg.Core, cfg.Pairing)
	if err != nil {
		return nil, err
	}
	w := &webBackend{
		local:       withGzip(withRequestBoundary(cfg.RequestPolicy, guardMutations(mux))),
		remoteClose: cfg.RemoteClose,
	}
	w.tunnelFor = func(peer tunnelPeer) http.Handler {
		return withTunnelBoundary(peer, mux)
	}
	return w, nil
}

func (w *webBackend) Close() error {
	if w == nil || w.remoteClose == nil {
		return nil
	}
	return w.remoteClose.Close()
}
