// scimux — supervise tmux-wrapped agent chats (Claude Code, Codex) from a
// local web page. Each chat is a node in a research tree; prompts go in via
// tmux, replies come back from the transcript files the agent CLIs write.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	mimepkg "mime"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// version is stamped at build time: go build -ldflags "-X main.version=v0.5.0"
var version = "dev"

const appSummary = "scimux supervises agent chats from a local web page."

// hostname is resolved once at startup; shown in the UI statusbar.
var hostname = "scimux"

// ---------- store ----------

// jsonBodyMax bounds every JSON request body (prompts included; 1 MiB is
// generous). -addr may be bound wider than loopback, so unbounded decodes
// would be an easy memory-exhaustion hole (R18.6). /api/ui has its own,
// larger uiStateMax limit.
const jsonBodyMax = 1 << 20

// decodeJSON decodes a bounded JSON request body.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, jsonBodyMax)
	return json.NewDecoder(r.Body).Decode(v)
}

// csrfToken is a per-process secret embedded in the served index page (as the
// scimux-csrf meta tag) and echoed back by the UI in the X-Scimux-CSRF header
// on every unsafe request. It is the write-side security boundary: scimux is
// intentionally unauthenticated and usually loopback-bound, but a loopback
// service is still reachable by any web page the operator happens to open, and
// the unsafe API can send prompts, answer approvals, interrupt turns, upload
// files and self-update. A cross-origin page cannot read this token (the
// same-origin policy hides the HTML body) and cannot forge the custom header on
// a simple request, so requiring it closes the CSRF surface with no dependency.
var csrfToken = mustToken()

func mustToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// A weak/empty token would silently defeat the control it exists to be;
		// fail loudly at startup instead.
		panic("scimux: generate CSRF token: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// guardMutations enforces same-origin + CSRF-token on every unsafe method
// before the request reaches a handler. Safe methods (GET/HEAD/OPTIONS) pass
// through untouched — they neither mutate state nor are readable cross-origin
// without CORS, which scimux never grants.
func guardMutations(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if !sameOrigin(r) {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Scimux-CSRF")), []byte(csrfToken)) != 1 {
			http.Error(w, "missing or invalid CSRF token", http.StatusForbidden)
			return
		}
		if !validContentType(r) {
			http.Error(w, "unsupported content type", http.StatusUnsupportedMediaType)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin rejects a browser request whose Origin (or, absent that, Referer)
// names a different host than the one it was sent to. A missing Origin *and*
// Referer is allowed: non-browser clients (curl, scripts) send neither, and for
// them the CSRF token is the gate. Browsers always attach Origin to unsafe
// cross-origin fetches, so this catches the case the token alone would not (a
// buggy client that leaked the token can still not be driven cross-origin).
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		if ref := r.Header.Get("Referer"); ref != "" {
			if u, err := url.Parse(ref); err == nil {
				origin = u.Scheme + "://" + u.Host
			}
		}
	}
	if origin == "" {
		return true // non-browser client; the token requirement still applies
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

// validContentType keeps unsafe requests to the content types the API actually
// accepts: JSON everywhere, multipart only on the attachment upload route. A
// bodyless request (interrupt, resolve, delete) carries no Content-Type and is
// fine. This is defense in depth behind the token — it also blocks the classic
// simple-request form POST (which cannot set the token header anyway).
func validContentType(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return true
	}
	mediaType, _, err := mimepkg.ParseMediaType(ct)
	if err != nil {
		return false
	}
	if mediaType == "multipart/form-data" {
		return strings.HasSuffix(r.URL.Path, "/attachments")
	}
	return mediaType == "application/json"
}

// ---------- background poller ----------

// warmStartup pays the cold replay cost before the browser can ask for it:
// one poll discovers/catches up tmux transcript mirrors, then the per-node
// segment cache is populated so the first /api/state or chat poll reads
// settled logs from memory instead of parsing every chat on demand.
func (a *app) warmStartup() {
	a.poll()

	a.mu.Lock()
	nodes := make([]*Node, len(a.nodes))
	copy(nodes, a.nodes)
	a.mu.Unlock()
	for _, n := range nodes {
		a.segment(n)
	}
}

func configureUsage(fs *flag.FlagSet, name string) {
	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprintln(out, appSummary)
		fmt.Fprintln(out)
		fmt.Fprintf(out, "Usage: %s [options]\n\n", name)
		fs.PrintDefaults()
	}
}

type startupStatus struct {
	w       io.Writer
	label   string
	start   time.Time
	stop    chan struct{}
	stopped chan struct{}
	once    sync.Once
}

func startStatus(w io.Writer, label string, animate bool) *startupStatus {
	s := &startupStatus{w: w, label: label, start: time.Now()}
	if !animate {
		fmt.Fprintf(w, "%s ...\n", label)
		return s
	}
	s.stop = make(chan struct{})
	s.stopped = make(chan struct{})
	go func() {
		defer close(s.stopped)
		frames := []byte{'|', '/', '-', '\\'}
		tick := time.NewTicker(120 * time.Millisecond)
		defer tick.Stop()
		i := 0
		for {
			fmt.Fprintf(w, "\r%s %c", label, frames[i%len(frames)])
			i++
			select {
			case <-s.stop:
				return
			case <-tick.C:
			}
		}
	}()
	return s
}

func (s *startupStatus) Done() {
	s.once.Do(func() {
		elapsed := time.Since(s.start).Round(time.Millisecond)
		if s.stop != nil {
			close(s.stop)
			<-s.stopped
			fmt.Fprintf(s.w, "\r\033[K%s done (%s)\n", s.label, elapsed)
			return
		}
		fmt.Fprintf(s.w, "%s done (%s)\n", s.label, elapsed)
	})
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// ---------- UI state ----------

// The web client owns a small blob of cross-device state — map group tabs,
// the archived-card set, private notes — that must survive scimux restarts,
// so it lives next to the node store instead of in localStorage (per-device
// state like drafts stays client-side). The blob is opaque JSON to the
// server: its shape belongs to the client.
//
// Writes are revisioned, not last-writer-wins: every GET carries an ETag
// derived from the stored bytes, every PUT must name the revision it was
// based on (If-Match), and a stale base gets 409 — the client refetches,
// replays its local operations on the fresh document, and retries. That is
// what lets two of the supervisor's devices add notes concurrently without
// silently erasing each other. Clients poll GET with If-None-Match (304) on
// the same cadence as /api/state.
const uiStateMax = 1 << 20

// uiETag derives the revision tag from the canonical stored bytes.
func uiETag(b []byte) string {
	h := fnv.New64a()
	h.Write(b)
	return fmt.Sprintf(`"%x"`, h.Sum64())
}

// readUILocked returns the stored UI document, "{}" when none exists yet.
// Any error other than not-exist is a real storage failure the client must
// see — reporting it as an empty document would invite the next mutation to
// overwrite whatever the unreadable file still holds. Callers hold a.uiMu.
func (a *app) readUILocked() ([]byte, error) {
	b, err := os.ReadFile(a.uiPath)
	if os.IsNotExist(err) {
		return []byte("{}"), nil
	}
	if err != nil {
		return nil, err
	}
	if !json.Valid(b) {
		return nil, fmt.Errorf("stored ui state is not valid JSON")
	}
	return b, nil
}

func (a *app) handleUIGet(w http.ResponseWriter, r *http.Request) {
	a.uiMu.Lock()
	b, err := a.readUILocked()
	a.uiMu.Unlock()
	if err != nil {
		http.Error(w, "read ui state: "+err.Error(), 500)
		return
	}
	etag := uiETag(b)
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func (a *app) handleUIPut(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, uiStateMax+1))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if len(b) > uiStateMax {
		http.Error(w, "ui state too large", 413)
		return
	}
	if !json.Valid(b) {
		http.Error(w, "ui state must be valid JSON", 400)
		return
	}
	match := r.Header.Get("If-Match")
	if match == "" {
		http.Error(w, "ui writes require If-Match (use * to bootstrap)", 428)
		return
	}
	// Atomic replace under the UI lock (not a.mu): a crash mid-write must never
	// leave a truncated file, concurrent PUTs must not interleave tmp files, and
	// the revision check must be atomic with the write it guards — but this is
	// pure I/O over a private document, so it must not stall the poller.
	a.uiMu.Lock()
	defer a.uiMu.Unlock()
	cur, err := a.readUILocked()
	if err != nil {
		http.Error(w, "read ui state: "+err.Error(), 500)
		return
	}
	if match != "*" && match != uiETag(cur) {
		http.Error(w, "ui state changed since this revision was read", 409)
		return
	}
	// Private notes live here: owner-only permissions. The tmp write + rename is
	// atomic for content (rename swaps the inode), which is the guarantee this
	// per-device UI blob needs; unlike the audit store it is not fsync'd — a lost
	// note after a host crash is recoverable, a corrupted nodes.jsonl is not.
	tmp := a.uiPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if err := os.Rename(tmp, a.uiPath); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("ETag", uiETag(b))
	writeJSON(w, map[string]string{"ok": "saved"})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// ---------- main ----------

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "scimux:", err)
		os.Exit(1)
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		hostname = h
	}
	configureUsage(flag.CommandLine, os.Args[0])
	addr := flag.String("addr", "127.0.0.1:8787", "listen address (loopback only; use an SSH tunnel for remote access)")
	data := flag.String("data", filepath.Join(home, ".scimux"), "data directory for the node store")
	socket := flag.String("socket", "scimux", "tmux socket name (tmux -L) for the private server")
	flag.Parse()

	if err := prepareDataDir(*data); err != nil {
		fmt.Fprintln(os.Stderr, "scimux:", err)
		os.Exit(1)
	}
	a, err := NewApp(Config{
		Home:    home,
		DataDir: *data,
		Socket:  *socket,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "scimux:", err)
		os.Exit(1)
	}
	status := startStatus(os.Stderr, "scimux: preparing chats before opening the web UI", isTerminal(os.Stderr))
	a.warmStartup()
	status.Done()

	// Structured-protocol subprocesses (ACP, codex app-server) are ours: unlike
	// tmux sessions (which deliberately survive scimux exit), they must not
	// orphan. Kill every such process group on shutdown. tmux sessions are
	// untouched.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		a.acp.Shutdown()
		a.codex.Shutdown()
		os.Exit(0)
	}()

	go func() {
		for {
			a.poll()
			// Subscription usage refreshes only while the user is active; the
			// cache no-ops outside the prompt-driven window, so this cannot
			// probe providers overnight (usage.go).
			a.maybeRefreshUsageAsync()
			time.Sleep(2 * time.Second)
		}
	}()
	// Warm the harness/model probe (it shells out to the agent CLIs) so the
	// first new-activity dialog doesn't wait on subprocesses.
	go detectAgents()
	// Learn the concrete claude model ids (the CLI mis-resolves its own family
	// aliases) — cached for claudeCacheTTL, so this billed call runs at most
	// weekly. Background: launches fall back to the bare alias until it returns.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		a.refreshClaudeModels(ctx)
	}()

	handler, err := NewHandler(a, webFS)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scimux:", err)
		os.Exit(1)
	}

	fmt.Printf("scimux: http://%s/  (tmux socket %q, store %s)\n", *addr, *socket, a.storePath)
	fmt.Printf("scimux: attach to a chat by hand: tmux -L %s attach -t <node-id>\n", *socket)
	// -addr may be bound wider than loopback, so give the server real
	// timeouts (slowloris defense). No ReadTimeout/WriteTimeout: legitimate
	// handlers can be slow (structured sends, the self-update download);
	// ReadHeaderTimeout covers the attack that matters.
	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "scimux:", err)
		os.Exit(1)
	}
}
