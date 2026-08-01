// scimux — supervise tmux-wrapped agent chats (Claude Code, Codex) from a
// local web page. Each chat is a node in a research tree; prompts go in via
// tmux, replies come back from the transcript files the agent CLIs write.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
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

	"codeberg.org/chrberger/scimux/internal/asset"
	"codeberg.org/chrberger/scimux/internal/dialoghint"
	"codeberg.org/chrberger/scimux/internal/sessionlog"
	"codeberg.org/chrberger/scimux/internal/tmuxsession"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

// version is stamped at build time: go build -ldflags "-X main.version=v0.5.0"
var version = "dev"

const appSummary = "scimux supervises agent chats from a local web page."

// hostname is resolved once at startup; shown in the UI statusbar.
var hostname = "scimux"

// segment returns the node's current conversation — the session log's tail
// after its last source seam. A /clear appends a seam, so this is what makes
// "fresh chat surface under the same activity" uniform across transports.
func (a *app) segment(n *Node) sessionlog.Segment {
	if a.sessionsDir == "" {
		return sessionlog.Segment{Turns: []transcript.Turn{}} // bare test apps
	}
	a.mu.Lock()
	if a.segCache == nil {
		a.segCache = map[string]*sessionlog.Cache{}
	}
	c := a.segCache[n.ID]
	if c == nil {
		c = &sessionlog.Cache{}
		a.segCache[n.ID] = c
	}
	a.mu.Unlock()
	return c.Segment(a.sessionLogPath(n.ID))
}

// ---------- store ----------

// jsonBodyMax bounds every JSON request body (prompts included; 1 MiB is
// generous). -addr may be bound wider than loopback, so unbounded decodes
// would be an easy memory-exhaustion hole (R18.6). /api/ui has its own,
// larger uiStateMax limit.
const jsonBodyMax = 1 << 20

// attachUploadMax bounds a multipart attachment upload. Files blow past
// jsonBodyMax, so uploads have their own, larger cap and never ride the prompt
// JSON. Bytes land on disk; the session log only ever stores a text reference.
const attachUploadMax = 25 << 20

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

// ---------- HTTP ----------

// handleAdopt registers an already-running tmux session (which scimux did
// not start) as a node. No tmux state is touched — this only writes the
// node record, so adoption is always safe for the running agent.
func (a *app) handleAdopt(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Session     string `json:"session"`
		Title       string `json:"title"`
		Prompt      string `json:"prompt"`
		Description string `json:"description"`
		Agent       string `json:"agent"`
		Model       string `json:"model"`
		Dir         string `json:"dir"`
		SessionID   string `json:"session_id"`
		Transcript  string `json:"transcript"`
	}
	if err := decodeJSON(w, r, &body); err != nil || body.Session == "" {
		http.Error(w, "bad request: need session", 400)
		return
	}
	// The tmux session name becomes the node id (and its session-log
	// filename): hold it to the same rules NewSession applies to created
	// nodes. tmux itself already rejects '.' and ':', so this is defense in
	// depth against names that would misbehave as ids (e.g. containing '/').
	if !tmuxsession.ValidName(body.Session) {
		http.Error(w, fmt.Sprintf("session name %q is not adoptable as a node id", body.Session), 400)
		return
	}
	agent := body.Agent
	if agent == "" {
		agent = "claude"
	}
	// Codex now uses the app-server protocol: scimux starts the subprocess
	// itself via POST /api/nodes. Reject before the tmux Alive check so any
	// agent:"codex" request gets the clear explanation regardless of whether
	// the named session exists.
	if agent == "codex" {
		http.Error(w, "codex uses the app-server protocol; use POST /api/nodes with agent:\"codex\" to create a new activity", 400)
		return
	}
	// Hold adopted agents to the creation allowlist (minus codex): a direct API
	// client must not persist a node with an agent the rest of the code does not
	// support. Only tmux-transport agents are adoptable — pi/opencode over ACP
	// are created, not adopted, but a legacy tmux-run pi/opencode may exist.
	switch agent {
	case "claude", "pi", "opencode":
	default:
		http.Error(w, fmt.Sprintf("unknown agent %q (adoptable: claude, pi, opencode)", agent), 400)
		return
	}
	// A transcript override, if supplied, must be an absolute, cleaned path under
	// the agent's transcript root. Otherwise a crafted path (paired with a
	// guessed tmux session) could make scimux parse and mirror an arbitrary
	// local file under a node's identity. Only claude uses transcript files.
	if body.Transcript != "" {
		root := filepath.Join(a.home, ".claude", "projects") + string(filepath.Separator)
		clean := filepath.Clean(body.Transcript)
		if agent != "claude" || !filepath.IsAbs(clean) || !strings.HasPrefix(clean, root) {
			http.Error(w, "transcript override must be an absolute path under ~/.claude/projects/", 400)
			return
		}
		body.Transcript = clean
	}
	s := a.server.Session(body.Session)
	if !s.Alive() {
		http.Error(w, fmt.Sprintf("no session %q on socket %q", body.Session, a.server.Socket), 404)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, taken := a.byID[body.Session]; taken {
		http.Error(w, "node already exists", 409)
		return
	}
	// The same taken-checks uniqueID applies to created ids (R20.2, R20.4):
	// an id reserved by an in-flight create must not be adopted out from under
	// the launch, and a leftover session log under this slug is a dead node's
	// history — adopting onto it would bind the mirror to foreign turns under
	// the dead node's identity.
	if a.reserved[body.Session] {
		http.Error(w, fmt.Sprintf("a node %q is being created right now; retry or pick another session name", body.Session), 409)
		return
	}
	if a.sessionLogExists(body.Session) {
		http.Error(w, fmt.Sprintf("session log %s.jsonl already holds a dead node's history; move it out of the sessions directory (or into sessions/archive/) before adopting this name", body.Session), 409)
		return
	}
	dir := body.Dir
	if dir == "" {
		if cwd, err := s.Cwd(); err == nil {
			dir = cwd
		}
	}
	title := body.Title
	if title == "" {
		title = body.Session
	}
	n := &Node{ID: body.Session, Title: title, Prompt: body.Prompt, Agent: agent,
		Model: body.Model, Dir: dir, SessionID: body.SessionID, Transcript: body.Transcript,
		Description: body.Description, Adopted: true, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if n.Description == "" {
		n.Description = n.Prompt
	}
	// Adopted claude session without a known id: first try the pane's own
	// process arguments (claude --resume <id> / --session-id <id>), which is
	// deterministic even with several sessions in one directory. Fall back
	// to the newest session log in the working directory. A wrong or missing
	// guess still leaves peek + send working.
	if n.Agent == "claude" && n.SessionID == "" && n.Transcript == "" {
		if pid, err := s.PanePID(); err == nil {
			n.SessionID = sessionFromPane(pid, sessionArgFromCmdline)
		}
		// A process-derived id remains authoritative even if Claude has not
		// created (or we cannot yet see) its transcript. discoverTranscript
		// will retry it; do not replace it with the nondeterministic mtime guess.
		if n.SessionID == "" {
			if path, sid, ok := transcript.FindClaudeNewestInDir(a.home, n.Dir); ok {
				n.Transcript, n.SessionID = path, sid
			}
		}
	}
	// Any known claude session id — supplied explicitly (the migration script
	// sends one) or extracted from the pane above — gets its deterministic
	// lookup now (the id is the transcript filename). If the file is not visible
	// yet, discoverTranscript retries the same lookup.
	if n.Agent == "claude" && n.SessionID != "" && n.Transcript == "" {
		if path, ok := transcript.FindClaudeTranscript(a.home, n.SessionID); ok {
			n.Transcript = path
		}
	}
	// Transcript exclusivity holds for adoption too: a path another node
	// owns (or a discovery is reserving right now) must not be published twice.
	if n.Transcript != "" && a.pathClaimedLocked(n.Transcript, n.ID) {
		http.Error(w, fmt.Sprintf("transcript %q already belongs to another node", n.Transcript), 409)
		return
	}
	// Persist before publishing: an adopted node that exists only in memory
	// would silently vanish from the registry on restart.
	if err := a.appendRecord(storeRecord{Type: "node", Node: n}); err != nil {
		http.Error(w, "persist node: "+err.Error(), 500)
		return
	}
	a.nodes = append(a.nodes, n)
	a.byID[n.ID] = n
	writeJSON(w, n)
}

func (a *app) handleNewNode(w http.ResponseWriter, r *http.Request) {
	var n Node
	if err := decodeJSON(w, r, &n); err != nil {
		http.Error(w, "bad request: "+err.Error(), 400)
		return
	}
	// Scrub every server-owned field before validation: a create request only
	// supplies launch config (title/description/prompt/agent/model/effort/dir/
	// parent/lane_id/rationale). Identity, adoption, liveness, and the linked
	// transcript are all minted or managed by the server, and trusting them from
	// the body would let a client be born "Closed" (ended_at), keep an owned
	// tmux session alive forever (adopted → closeOwned never kills it), or bind
	// the mirror to an arbitrary transcript path (no pathClaimed check on
	// create, unlike handleAdopt). The UI never sends these; this closes the
	// gap for any other client. ForkKind is recomputed in resolveNode.
	n.ID, n.SessionID, n.Transcript, n.CreatedAt, n.EndedAt, n.ForkKind, n.Adopted = "", "", "", "", "", "", false
	// Resolve and validate the launch configuration first: no request that
	// fails validation (empty prompt, unknown parent, bad agent or dir) ever
	// reaches createNode, and the transport decision reads the *resolved* agent
	// (finding 25).
	a.mu.Lock()
	status, err := a.resolveNode(&n)
	a.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	// Snapshot current session names before the critical section (tmux is
	// slow); the ID allocator must avoid unadopted sessions.
	taken := map[string]bool{}
	for _, s := range a.server.Sessions() {
		taken[s] = true
	}
	// createNode manages a.mu itself: the id reservation happens under the
	// lock, the external launch outside it.
	status, err = a.createNode(&n, taken)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	// The launch carried the node's first prompt (it rides the agent's command
	// line), so a successful create is a budget-consuming turn for Claude/Codex.
	if strings.TrimSpace(n.Prompt) != "" {
		a.noteUsagePrompt(n.Agent)
	}
	writeJSON(w, n)
}

func (a *app) handleUpdateNode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Title       *string `json:"title"`
		Description *string `json:"description"`
		LaneID      *string `json:"lane_id"`
		// Station, when set, targets an OLD map station (its start-time key)
		// instead of the node: the edit is written as a per-station label
		// snapshot and the node record is left untouched, so renaming a closed
		// station never bleeds into the head or its siblings (spec E). The head
		// station has no key here — it is edited through the node fields above.
		Station *string `json:"station"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		http.Error(w, "bad request: "+err.Error(), 400)
		return
	}
	a.mu.Lock()
	n, ok := a.byID[id]
	if !ok {
		a.mu.Unlock()
		http.Error(w, "not found", 404)
		return
	}
	// Old-station edit: snapshot the label for that stop only, node untouched.
	if body.Station != nil {
		a.mu.Unlock()
		seam := strings.TrimSpace(*body.Station)
		if seam == "" {
			http.Error(w, "station must not be empty", 400)
			return
		}
		title, desc := "", ""
		if body.Title != nil {
			title = strings.TrimSpace(*body.Title)
		}
		if body.Description != nil {
			desc = strings.TrimSpace(*body.Description)
		}
		if title == "" {
			http.Error(w, "title must not be empty", 400)
			return
		}
		if a.sessionsDir == "" {
			http.Error(w, "no session store", 409)
			return
		}
		wtr := &sessionlog.Writer{Path: a.sessionLogPath(id)}
		if err := wtr.Append(sessionlog.NewStation(seam, title, desc)); err != nil {
			http.Error(w, "persist station: "+err.Error(), 500)
			return
		}
		writeJSON(w, sessionlog.StationLabel{Title: title, Desc: desc})
		return
	}
	next := *n
	if body.Title != nil {
		title := strings.TrimSpace(*body.Title)
		if title == "" {
			a.mu.Unlock()
			http.Error(w, "title must not be empty", 400)
			return
		}
		next.Title = title
	}
	if body.Description != nil {
		next.Description = strings.TrimSpace(*body.Description)
	}
	if body.LaneID != nil {
		laneID := strings.TrimSpace(*body.LaneID)
		switch {
		case n.LaneID == "" && laneID != "":
			next.LaneID = laneID
		case n.LaneID != laneID:
			a.mu.Unlock()
			http.Error(w, "lane assignment is immutable", 409)
			return
		}
	}
	if next.Description == "" {
		next.Description = next.Prompt
	}
	if err := a.appendRecord(storeRecord{Type: "node", Node: &next}); err != nil {
		a.mu.Unlock()
		http.Error(w, "persist node: "+err.Error(), 500)
		return
	}
	*n = next
	a.mu.Unlock()
	writeJSON(w, next)
}

// handleExitNode implements /exit: the deliberate "ended" head state. Unlike
// delete, the node stays on the map (dead-end ⊣ cap, visible and rideable); unlike
// a mechanical process exit, this is a user decision recorded durably. Order
// mirrors delete — persist the intent (the ended node record) before tearing the
// process down, so a crash between the two never loses the closed marker; then
// stop the owned process via the same closeOwned path delete uses.
func (a *app) handleExitNode(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	a.mu.Lock()
	cur, ok := a.byID[n.ID]
	if !ok {
		a.mu.Unlock()
		http.Error(w, "not found", 404)
		return
	}
	firstExit := cur.EndedAt == ""
	if firstExit {
		next := *cur
		next.EndedAt = time.Now().UTC().Format(time.RFC3339)
		if err := a.appendRecord(storeRecord{Type: "node", Node: &next}); err != nil {
			a.mu.Unlock()
			http.Error(w, "persist node: "+err.Error(), 500)
			return
		}
		*cur = next
	}
	a.mu.Unlock()
	// The response reports whether the underlying process actually stopped so the
	// UI can tell the truth instead of promising a stop it may not have
	// delivered: an adopted tmux session is deliberately left running
	// (closeOwned returns nil without killing it), and a kill can fail. In both
	// cases the node is "closed in scimux" but the agent is still alive.
	stopped, reason := true, ""
	if firstExit {
		// Best-effort process teardown, once. The thread is already marked ended
		// in the durable store, so a failure to reach the agent must not un-end
		// it — the node stays visibly closed regardless.
		if err := a.closeOwned(n); err != nil {
			stopped, reason = false, "kill_failed"
			fmt.Fprintf(os.Stderr, "scimux: exit of %s marked ended but failed to close the agent: %v\n", n.ID, err)
		} else if a.proc(n) == nil && n.Adopted {
			stopped, reason = false, "adopted"
		}
	} else {
		// Idempotent re-exit (only reachable by a direct API call — the UI hides
		// the control once ended): do not tear the process down a second time.
		// Report the agent's current standing from mechanical liveness instead of
		// attempting a fresh kill that a first, already-successful exit made moot.
		a.mu.Lock()
		live := a.live[n.ID]
		a.mu.Unlock()
		switch {
		case a.proc(n) == nil && n.Adopted:
			stopped, reason = false, "adopted"
		case live == "active" || live == "quiet":
			stopped, reason = false, "running"
		}
	}
	writeJSON(w, map[string]any{"node": *n, "closed": true, "stopped": stopped, "reason": reason})
}

func (a *app) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	// Persist the delete before tearing anything down. The store is the
	// supervisor's durable truth; killing an owned agent first and only then
	// recording the delete risks a killed agent the store still replays as a
	// live node. Order is persist-intent → close → finalize, and the node is
	// re-asserted if the close fails so the durable record matches reality.
	a.mu.Lock()
	if _, ok := a.byID[n.ID]; !ok {
		a.mu.Unlock()
		http.Error(w, "not found", 404)
		return
	}
	if err := a.appendRecord(storeRecord{Type: "delete", ID: n.ID, Time: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		a.mu.Unlock()
		http.Error(w, "persist delete: "+err.Error(), 500)
		return
	}
	a.mu.Unlock()

	if err := a.closeOwned(n); err != nil {
		// The agent outlived the delete record. Re-assert the node so replay
		// (and this process) keep showing it live: a later node record wins
		// over the delete, exactly as the concurrent-launch path relies on.
		a.mu.Lock()
		if _, ok := a.byID[n.ID]; ok {
			if rerr := a.appendRecord(storeRecord{Type: "node", Node: n}); rerr != nil {
				fmt.Fprintf(os.Stderr, "scimux: delete of %s failed to close the agent and failed to re-assert the node: %v\n", n.ID, rerr)
			}
		}
		a.mu.Unlock()
		http.Error(w, "close session: "+err.Error(), 500)
		return
	}

	a.mu.Lock()
	a.removeNodeLocked(n.ID)
	a.mu.Unlock()
	a.archiveSessionLog(n.ID)
	a.archiveAttachments(n.ID)
	a.archiveAssets(n.ID)
	writeJSON(w, map[string]string{"ok": "deleted"})
}

// Attachment references an uploaded file stored on disk. The bytes live under
// ~/.scimux/attachments/<node-id>/; only this text reference is ever recorded
// in a store — nodes.jsonl and the session log stay grep-able, never binary.
type Attachment struct {
	Path string `json:"path"`
	Mime string `json:"mime"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	// AssetID is the durable session-asset backing this upload (see
	// ingestAttachmentAsset, upload_assets.go). Populated by
	// handleUploadAttachments; never sent by the client.
	AssetID string `json:"assetId,omitempty"`
}

func (a *app) attachmentDir(id string) string { return filepath.Join(a.attachmentsDir, id) }

// storeAttachment writes one uploaded file into the node's attachment directory
// and returns its reference. The stored leaf is prefixed with a random token so
// re-uploading a name never collides or overwrites, and so a crafted filename
// can never escape the directory: filepath.Base strips any separators and the
// token guarantees a non-empty, traversal-free leaf. Written O_EXCL 0600.
func (a *app) storeAttachment(id, name, mime string, src io.Reader) (Attachment, error) {
	base := filepath.Base(name)
	if base == "." || base == ".." || base == "" || base == string(filepath.Separator) {
		base = "file"
	}
	var tok [4]byte
	if _, err := rand.Read(tok[:]); err != nil {
		return Attachment{}, err
	}
	dir := a.attachmentDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Attachment{}, err
	}
	dst := filepath.Join(dir, hex.EncodeToString(tok[:])+"-"+base)
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Attachment{}, err
	}
	n, err := io.Copy(f, src)
	cerr := f.Close()
	if err != nil || cerr != nil {
		os.Remove(dst)
		if err != nil {
			return Attachment{}, err
		}
		return Attachment{}, cerr
	}
	if mime == "" || mime == "application/octet-stream" {
		if t := mimepkg.TypeByExtension(filepath.Ext(base)); t != "" {
			mime = t
		}
	}
	return Attachment{Path: dst, Mime: mime, Name: base, Size: n}, nil
}

// archiveAttachments mirrors archiveSessionLog for a deleted node's uploaded
// files: it moves ~/.scimux/attachments/<id> into attachments/archive/ so a
// reissued slug can never inherit a dead node's files. Best-effort — retention
// never blocks a delete.
func (a *app) archiveAttachments(id string) {
	if a.attachmentsDir == "" {
		return
	}
	src := a.attachmentDir(id)
	if _, err := os.Stat(src); err != nil {
		return
	}
	dir := filepath.Join(a.attachmentsDir, "archive")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "scimux: archive attachments for %s: %v\n", id, err)
		return
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	if err := os.Rename(src, filepath.Join(dir, id+"."+stamp)); err != nil {
		fmt.Fprintf(os.Stderr, "scimux: archive attachments for %s: %v\n", id, err)
	}
}

// archiveAssets mirrors archiveSessionLog/archiveAttachments for a deleted
// node's blob-stored session assets (upload-design.md Phase 6): it moves
// ~/.scimux/assets/<id> into assets/archive/ so a reissued slug can never
// inherit a dead node's asset blobs, and a deleted node's images/files don't
// dangle as orphaned-but-still-servable files (the download endpoint checks
// n.ID against a.byID, so once removeNodeLocked has run the live path is
// already unreachable — this only prevents the blobs themselves from
// lingering under the live directory). Best-effort — retention never blocks
// a delete. Inline assets need no such move: their bytes live in the
// already-archived session log, not in assetsDir.
func (a *app) archiveAssets(id string) {
	if a.assetsDir == "" {
		return
	}
	src := asset.NodeDir(a.assetsDir, id)
	if _, err := os.Stat(src); err != nil {
		return
	}
	dir := filepath.Join(a.assetsDir, "archive")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "scimux: archive assets for %s: %v\n", id, err)
		return
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	if err := os.Rename(src, filepath.Join(dir, id+"."+stamp)); err != nil {
		fmt.Fprintf(os.Stderr, "scimux: archive assets for %s: %v\n", id, err)
	}
}

// extendPrompt appends a plain-text reference to each attachment so the agent
// reads the local file — proven across every transport to trigger image
// ingestion (see attic/multimodal-input-design.md), so one mechanism serves all three
// with no per-transport image plumbing. Delivery-time only: never stored on
// n.Prompt, so cards and the research question stay clean. The reference left in
// the recorded user turn (mirror echo for tmux; the manager's own user event
// for ACP/codex) is also what the chat view renders (P3).
func extendPrompt(text string, atts []Attachment) string {
	if len(atts) == 0 {
		return text
	}
	lines := make([]string, 0, len(atts))
	for _, at := range atts {
		kind := "file"
		if strings.HasPrefix(at.Mime, "image/") {
			kind = "image"
		}
		lines = append(lines, fmt.Sprintf("[attached %s: %s]", kind, at.Path))
	}
	ref := strings.Join(lines, "\n")
	if strings.TrimSpace(text) == "" {
		return ref
	}
	return text + "\n\n" + ref
}

// resolveAttachments validates each posted attachment reference: it must point
// at a file inside THIS node's attachment directory (the only legitimate source
// — the client uploaded it via POST …/attachments). This closes off a send that
// would otherwise make the agent read an arbitrary local path. Metadata (name,
// size, mime) is refreshed from disk.
func (a *app) resolveAttachments(id string, refs []Attachment) ([]Attachment, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	nodeDir := a.attachmentDir(id) + string(filepath.Separator)
	out := make([]Attachment, 0, len(refs))
	for _, r := range refs {
		clean := filepath.Clean(r.Path)
		if !strings.HasPrefix(clean, nodeDir) {
			return nil, fmt.Errorf("attachment %q is not one of this activity's uploads", r.Name)
		}
		fi, err := os.Stat(clean)
		if err != nil || fi.IsDir() {
			return nil, fmt.Errorf("attachment %q not found", r.Name)
		}
		at := Attachment{Path: clean, Name: r.Name, Mime: r.Mime, Size: fi.Size()}
		if at.Name == "" {
			at.Name = filepath.Base(clean)
		}
		if at.Mime == "" {
			at.Mime = mimepkg.TypeByExtension(filepath.Ext(clean))
		}
		out = append(out, at)
	}
	return out, nil
}

// handleUploadAttachments stores one uploaded file for a node and returns its
// reference (as a one-element list, matching the multipart field). It is
// deliberately separate from the JSON send path: files exceed jsonBodyMax, so
// uploads get their own larger cap (attachUploadMax) and never ride the prompt
// JSON. The prompt references the returned path (P2).
func (a *app) handleUploadAttachments(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	if a.refuseEnded(w, n) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, attachUploadMax)
	if err := r.ParseMultipartForm(attachUploadMax); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, fmt.Sprintf("upload too large: the total is capped at %d MiB", attachUploadMax>>20), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad multipart request", 400)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		http.Error(w, "no files in upload (expected multipart field \"files\")", 400)
		return
	}
	// One file per request. The composer already uploads files individually
	// (each staged file is its own POST), so a single-file contract loses
	// nothing — and it makes a partial-batch failure structurally impossible:
	// there is never an earlier file already committed to the append-only
	// session log when a later one fails. A caller that batches several files
	// is rejected outright rather than silently half-ingested.
	if len(files) > 1 {
		http.Error(w, "one file per upload request", 400)
		return
	}
	fh := files[0]
	src, err := fh.Open()
	if err != nil {
		http.Error(w, "open upload: "+err.Error(), 400)
		return
	}
	att, err := a.storeAttachment(n.ID, fh.Filename, fh.Header.Get("Content-Type"), src)
	src.Close()
	if err != nil {
		http.Error(w, "store upload: "+err.Error(), 500)
		return
	}
	// Past this point any failure removes the staged file so a rejected upload
	// leaves nothing behind; ingestAttachmentAsset removes its own blob on a
	// failed log append (see ingestAssetBytes).
	data, err := os.ReadFile(att.Path)
	if err != nil {
		os.Remove(att.Path)
		http.Error(w, "read upload: "+err.Error(), 500)
		return
	}
	ev, err := a.ingestAttachmentAsset(n.ID, att.Name, att.Mime, att.Path, data)
	if err != nil {
		os.Remove(att.Path)
		http.Error(w, "ingest upload: "+err.Error(), 500)
		return
	}
	att.AssetID = ev.ID
	writeJSON(w, map[string]any{"attachments": []Attachment{att}})
}

// handleAttachment serves one uploaded file's bytes for the chat view. Guarded:
// only a bare filename inside THIS node's attachment directory is served — no
// traversal, no cross-node reach. That scoping is also what keeps the delivered
// path-marker spoof-safe: a marker pointing outside the node's own uploads has
// no servable URL, so a crafted "[attached image: /etc/passwd]" renders nothing.
// Read-only; live nodes only (a deleted node's files are archived away).
func (a *app) handleAttachment(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	name := filepath.Base(r.PathValue("name")) // strips any path separators
	if name == "." || name == ".." || name == "" {
		http.Error(w, "not found", 404)
		return
	}
	dir := a.attachmentDir(n.ID)
	full := filepath.Join(dir, name)
	if !strings.HasPrefix(full, dir+string(filepath.Separator)) { // defense in depth
		http.Error(w, "not found", 404)
		return
	}
	fi, err := os.Stat(full)
	if err != nil || fi.IsDir() {
		http.Error(w, "not found", 404)
		return
	}
	// Serve uploads as inert content: an uploaded .html/.svg opened from the
	// app origin would otherwise run as same-origin active content (and, once
	// the CSRF token exists, read it). nosniff pins the type we set; only a
	// whitelist of safe raster images is served inline, everything else — SVG,
	// HTML, unknown — is forced to download. We set Content-Type explicitly so
	// ServeFile does not sniff its own.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if raster := inlineImageTypes[strings.ToLower(filepath.Ext(name))]; raster != "" {
		w.Header().Set("Content-Type", raster)
	} else {
		// Anything not on the raster whitelist downloads as an opaque blob:
		// octet-stream + nosniff + attachment leaves no path for an uploaded
		// .html/.svg to run as same-origin active content, whatever its name.
		w.Header().Set("Content-Type", "application/octet-stream")
		disp := name // strip the storage token for the download name (as the UI does)
		if i := strings.IndexByte(disp, '-'); i == 8 {
			disp = disp[i+1:]
		}
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(disp))
	}
	http.ServeFile(w, r, full)
}

// handleAsset serves one session asset's bytes: GET /api/nodes/{id}/assets/{assetID}.
// The URL carries only an opaque asset ID, never a path — BlobPath lives in
// the node's own session log and is resolved (with cross-node/traversal
// containment) via internal/asset.ResolveBlobPath before anything is served.
// Same inert-serving contract as handleAttachment: nosniff always, inline
// only for the safe raster whitelist, everything else forced to download.
func (a *app) handleAsset(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	id := r.PathValue("assetID")
	if id == "" || a.sessionsDir == "" {
		http.Error(w, "not found", 404)
		return
	}
	idx := sessionlog.ReadAssets(a.sessionLogPath(n.ID))
	rec, ok := idx[id]
	if !ok {
		http.Error(w, "not found", 404)
		return
	}

	var data []byte
	var servePath string
	switch rec.Storage {
	case "inline":
		b, err := base64.StdEncoding.DecodeString(rec.Bytes)
		if err != nil {
			http.Error(w, "not found", 404)
			return
		}
		data = b
	case "blob":
		full, err := asset.ResolveBlobPath(a.assetsDir, n.ID, rec.BlobPath)
		if err != nil {
			http.Error(w, "not found", 404)
			return
		}
		servePath = full
	default:
		http.Error(w, "not found", 404)
		return
	}

	name := rec.Name
	if name == "" {
		name = "asset"
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if raster := inlineImageTypes[strings.ToLower(filepath.Ext(name))]; raster != "" {
		w.Header().Set("Content-Type", raster)
	} else {
		// Same reasoning as handleAttachment: never trust the recorded MIME
		// for inline rendering — force an opaque download so an agent- or
		// user-supplied .html/.svg can never run as same-origin content.
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
	}
	if servePath != "" {
		http.ServeFile(w, r, servePath)
		return
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}

// inlineImageTypes maps the file extensions served inline (as an <img> or a
// new-tab open) to their content type: safe raster formats with no active
// content. SVG is excluded on purpose — it can carry script — as is everything
// else, which downloads. Keyed by extension so the whitelist does not depend on
// the host's mime table knowing webp.
var inlineImageTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

func (a *app) node(r *http.Request) (*Node, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n, ok := a.byID[r.PathValue("id")]
	return n, ok
}

// refuseEnded reports whether the node is a deliberately closed ("ended")
// thread and, if so, writes a 409. /exit makes the ended head an immutable
// dead-end cap; every mutation endpoint (send, /clear, upload, interrupt,
// remote key) refuses it so a "Closed" thread can never accept new history —
// including an adopted thread whose process outlived the cap, or one whose
// teardown failed. Read paths (peek, chat) and the allowed post-end
// operations (fork, edit, archive, delete) are deliberately not guarded:
// pick-up is via fork, not by reopening the dead-end.
func (a *app) refuseEnded(w http.ResponseWriter, n *Node) bool {
	a.mu.Lock()
	ended := n.EndedAt != ""
	a.mu.Unlock()
	if ended {
		http.Error(w, "thread is closed; fork to continue", http.StatusConflict)
	}
	return ended
}

// handleSend delivers a web prompt and reports what the delivery evidence
// supports: "acknowledged" when the pane visibly reacted to Enter or a new
// transcript turn appeared, "unconfirmed" otherwise. {ok:"sent"} alone would
// only mean "tmux accepted the keystrokes" — the TUI may have held the text
// in its editor (e.g. a pre-existing draft), and the next blind send would
// concatenate with it. Sends are serialized per node; an unresolved delivery
// holds further sends until the supervisor rechecks (POST …/send/resolve).
func (a *app) handleSend(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	if a.refuseEnded(w, n) {
		return
	}
	var body struct {
		Text        string       `json:"text"`
		Attachments []Attachment `json:"attachments"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		// The body cap is a deliberate defense (R18.6), but a bare "bad
		// request" for an oversized prompt gives the user no size hint and
		// invites retries that can never succeed — name the limit. (Files ride
		// the separate multipart upload, not this JSON body.)
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, fmt.Sprintf("prompt too large: the request body is capped at %d bytes (%d MiB); shorten the prompt or point the agent at a file instead", jsonBodyMax, jsonBodyMax>>20), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad request", 400)
		return
	}
	// A send may carry text, attachments, or both — an image-only turn is valid.
	if strings.TrimSpace(body.Text) == "" && len(body.Attachments) == 0 {
		http.Error(w, "bad request", 400)
		return
	}
	atts, err := a.resolveAttachments(n.ID, body.Attachments)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	// delivered is what the transport receives and records: the user's text with
	// a plain-text reference to each uploaded file appended (extendPrompt). The
	// raw text is kept only for /clear detection and unconfirmed-draft echo.
	delivered := extendPrompt(body.Text, atts)
	// Structured-protocol delivery (ACP, codex) is reliable (no pane-ack race,
	// so no "unconfirmed" state), but the preflight still holds: refuse a second
	// turn while one is in flight, and refuse entirely if the subprocess is
	// gone. The manager records the user turn before prompting (a log-append
	// failure refuses the send) — see the Send contract on each manager.
	if pm := a.proc(n); pm != nil {
		if pm.Live(n.ID) == "active" {
			http.Error(w, "a turn to this node is still in flight", 409)
			return
		}
		// The structured transports have no TUI to interpret slash commands,
		// so scimux implements /clear itself: a fresh protocol session on the
		// same process, recorded as a source seam — same page-turn semantics
		// as Claude's /clear, same log file, same node.
		if strings.TrimSpace(body.Text) == "/clear" {
			// Freeze the closing station's label before the page turns, so a
			// later rename only moves the fresh head (spec A/B/C). The manager
			// appends the clear seam inside Clear; this reads the pre-clear stop
			// chain, so it must run first.
			a.snapshotClosingStation(n)
			if err := pm.Clear(n.ID); err != nil {
				code := 500
				if pm.Conflict(err) {
					code = 409
				}
				http.Error(w, err.Error(), code)
				return
			}
			writeJSON(w, map[string]string{"status": "acknowledged"})
			return
		}
		if err := pm.Send(n.ID, delivered); err != nil {
			code := 500
			if pm.Conflict(err) {
				code = 409
			}
			http.Error(w, err.Error(), code)
			return
		}
		a.noteUsagePrompt(n.Agent)
		writeJSON(w, map[string]string{"status": "acknowledged"})
		return
	}
	a.mu.Lock()
	switch a.sendState[n.ID] {
	case "submitting":
		a.mu.Unlock()
		http.Error(w, "a send to this node is still in flight", 409)
		return
	case "unconfirmed":
		a.mu.Unlock()
		http.Error(w, "the previous send is unconfirmed — check the terminal, then recheck", 409)
		return
	}
	a.sendState[n.ID] = "submitting"
	a.mu.Unlock()

	// Transcript watermark before the send: a new user turn appearing is the
	// structured acknowledgement (slash commands may never enter the
	// transcript — for those the mechanical pane change has to carry it).
	turnsBefore := -1
	tl := a.tailerFor(n)
	if tl != nil {
		turnsBefore = len(tl.Poll())
	}
	acked, err := a.server.Session(n.ID).SendAck(delivered)
	if err != nil {
		a.mu.Lock()
		delete(a.sendState, n.ID)
		a.mu.Unlock()
		http.Error(w, err.Error(), 500)
		return
	}
	if !acked && tl != nil && len(tl.Poll()) > turnsBefore {
		acked = true
	}
	a.mu.Lock()
	if acked {
		delete(a.sendState, n.ID)
	} else {
		a.sendState[n.ID] = "unconfirmed"
	}
	a.mu.Unlock()
	// SendAck succeeded, so the prompt was delivered to the pane (acked or
	// unconfirmed — both consume budget). /clear is a page turn, not a turn.
	if strings.TrimSpace(body.Text) != "/clear" {
		a.noteUsagePrompt(n.Agent)
	}
	// /clear delivered through scimux is a *known* session rollover: Claude
	// Code starts a fresh session file and the linked transcript goes dead.
	// Retire the link right away — the UI degrades honestly to peek (the old
	// conversation is gone from the pane too), and the phase-end relink picks
	// up the new session file after the next turn. A /clear typed directly
	// into an attached pane still relies on the mtime heuristic.
	if acked && n.Agent == "claude" && strings.TrimSpace(body.Text) == "/clear" {
		a.retireTranscript(n)
	}
	if !acked {
		writeJSON(w, map[string]string{"status": "unconfirmed", "text": body.Text})
		return
	}
	writeJSON(w, map[string]string{"status": "acknowledged"})
}

// snapshotClosingStation freezes the label of the station a /clear is about to
// close, so a later rename of the active chat (which moves only the node's live
// Title/Description = the fresh head) can never rewrite the closed station's
// name on the map. The key is the closing station's start time: the last
// existing /clear seam, or the node's creation when this is the first page-turn.
// Call it BEFORE the clear seam is appended, so the key is read from the pre-clear
// stop chain. Best-effort: an append failure is logged, not fatal (the map
// degrades to the node title, the pre-feature behaviour). Guarded like the seam
// write — a node that never mirrored has no station to freeze.
func (a *app) snapshotClosingStation(n *Node) {
	if a.sessionsDir == "" {
		return
	}
	logPath := a.sessionLogPath(n.ID)
	if _, err := os.Stat(logPath); err != nil {
		return
	}
	key := n.CreatedAt
	if ct := a.segment(n).ClearTimes; len(ct) > 0 {
		key = ct[len(ct)-1]
	}
	if key == "" {
		return
	}
	w := &sessionlog.Writer{Path: logPath}
	if err := w.Append(sessionlog.NewStation(key, n.Title, n.Description)); err != nil {
		fmt.Fprintf(os.Stderr, "scimux: station snapshot for %s: %v\n", n.ID, err)
	}
}

// retireTranscript unlinks a node's transcript (and session id) after a known
// session rollover. Corrections are new records: a node record persists the
// cleared session id so a young node's discovery cannot resurrect the dead
// file, and an empty-path transcript record retires the link across restarts
// (the latest transcript record wins at replay). The node record goes first:
// if only one append lands, a cleared session id with a stale path is inert,
// while a cleared path with a live session id lets discovery relink the dead
// transcript. Record first, publish second, as everywhere.
func (a *app) retireTranscript(n *Node) {
	a.mu.Lock()
	if n.Transcript == "" && n.SessionID == "" {
		a.mu.Unlock()
		return
	}
	cp := *n
	cp.Transcript, cp.SessionID = "", ""
	a.mu.Unlock()
	if err := a.appendRecord(storeRecord{Type: "node", Node: &cp}); err != nil {
		fmt.Fprintf(os.Stderr, "scimux: retire session id for %s: %v\n", n.ID, err)
		return
	}
	if err := a.appendRecord(storeRecord{Type: "transcript", ID: n.ID, Path: ""}); err != nil {
		fmt.Fprintf(os.Stderr, "scimux: retire transcript for %s: %v\n", n.ID, err)
	}
	// Deliberate non-poller reset after durable retirement only: poller-owned
	// maps must drop the old file's tailer/progress/mirror so the next segment
	// cannot inherit them. Persist failure returns above without touching these.
	a.mu.Lock()
	n.Transcript, n.SessionID = "", ""
	delete(a.tailers, n.ID)
	delete(a.chatMark, n.ID)
	delete(a.staleChat, n.ID)
	// Drop the mirror's in-memory watermark too: it rebuilds from the log
	// (which now ends in the seam below), exactly like after a restart — so
	// a relink can never attribute pre-/clear turns to the fresh segment.
	delete(a.mirrors, n.ID)
	a.mu.Unlock()
	// A known rollover also turns the page in the session log: a path-less
	// "detached" seam makes the fresh chat surface immediate (the reader
	// renders since-last-source), while the prior conversation stays behind
	// it in the same file. The relink after the next turn appends the real
	// source seam with the new transcript path — two seams, both true. Only
	// an existing log gets one: a node that never mirrored has no page to
	// turn. Append failures are logged, not fatal: the UI degrades to peek
	// either way and the mirror's next seam still separates the segments.
	if a.sessionsDir != "" {
		logPath := a.sessionLogPath(n.ID)
		if _, err := os.Stat(logPath); err == nil {
			// Freeze the closing station's label before the seam turns the page,
			// so a later rename only moves the fresh head (spec A/B/C).
			a.snapshotClosingStation(n)
			w := &sessionlog.Writer{Path: logPath}
			if err := w.Append(sessionlog.NewClearSource("")); err != nil {
				fmt.Fprintf(os.Stderr, "scimux: clear seam for %s: %v\n", n.ID, err)
			}
		}
	}
}

// handleSendResolve is the supervisor's "I checked (or fixed) this in the
// terminal" acknowledgement: it clears an unconfirmed delivery so sending
// can resume. It never re-presses Enter.
func (a *app) handleSendResolve(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	a.mu.Lock()
	if a.sendState[n.ID] == "submitting" {
		a.mu.Unlock()
		http.Error(w, "a send is still in flight", 409)
		return
	}
	delete(a.sendState, n.ID)
	a.mu.Unlock()
	writeJSON(w, map[string]string{"ok": "resolved"})
}

func (a *app) handleSendInterrupt(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	if a.refuseEnded(w, n) {
		return
	}
	if pm := a.proc(n); pm != nil {
		if err := pm.Interrupt(n.ID); err != nil {
			code := 500
			if pm.Conflict(err) {
				code = 409
			}
			http.Error(w, err.Error(), code)
			return
		}
		writeJSON(w, map[string]string{"ok": "interrupted"})
		return
	}
	// tmux path: the interrupt is a remote keypress and follows the SendKey
	// contract — whitelisted key only (Escape is Claude Code's documented
	// turn interrupt; C-c on an idle pane clears input and a double press
	// exits the CLI), recorded in the store with pane evidence. Evidence
	// capture is a prerequisite, exactly as in handleKey.
	s := a.server.Session(n.ID)
	cap, err := s.Capture()
	if err != nil {
		http.Error(w, "refusing interrupt without pane evidence (capture failed): "+err.Error(), 500)
		return
	}
	excerpt := lastLines(cap, 12)
	if err := s.SendKey("Escape"); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if err := a.appendRecord(storeRecord{Type: "key", ID: n.ID, Key: "Escape",
		Excerpt: "interrupt: " + excerpt, Time: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		fmt.Fprintf(os.Stderr, "scimux: interrupt sent to %s but audit record failed: %v\n", n.ID, err)
		http.Error(w, "the interrupt was sent, but the audit record failed: "+err.Error(), 500)
		return
	}
	// Interrupting also withdraws an unconfirmed prior send — the supervisor
	// has taken over. A send still in flight ("submitting") keeps its state:
	// clearing it here would let a second send race the in-flight paste.
	a.mu.Lock()
	if a.sendState[n.ID] != "submitting" {
		delete(a.sendState, n.ID)
	}
	a.mu.Unlock()
	writeJSON(w, map[string]string{"ok": "interrupted"})
}

// handleChat renders the chat view for any node from its unified session log:
// the store is the read path for conversation history on every transport
// (phase 3 of the session-log consolidation). Transport machinery contributes
// only overlays — the tmux branch derives fallback/diagnostics/needs-input
// from the transcript tailer and pane mechanics; the structured branch adds
// the pending permission and last error from its manager. chat_started and
// prior_turns carry the /clear divider: a source seam in the log starts a
// fresh chat surface under the same activity, with the prior conversation
// preserved behind the seam in the same file.
func (a *app) handleChat(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	// ?history=1: the whole log as ordered surfaces — the on-demand read
	// behind the chat's "show earlier history" divider and the map's earlier
	// stops ("ride back through the journey"). A per-tap read, not a poll
	// path, so it is parsed fresh and uncached; the polled response below
	// stays segment-scoped.
	if r.URL.Query().Get("history") == "1" {
		segs := []sessionlog.HistorySegment{}
		assets := map[string]any{}
		if a.sessionsDir != "" {
			segs = sessionlog.ReadHistory(a.sessionLogPath(n.ID))
			for i := range segs {
				var segAssets map[string]any
				segs[i].Turns, segAssets = a.projectTurns(n.ID, segs[i].Turns)
				for id, v := range segAssets {
					assets[id] = v
				}
			}
		}
		resp := map[string]any{"segments": segs}
		if len(assets) > 0 {
			resp["assets"] = assets
		}
		writeJSON(w, resp)
		return
	}
	seg := a.segment(n)
	a.mu.Lock()
	var lastMS int64
	if t, ok := a.lastChg[n.ID]; ok {
		lastMS = t.UnixMilli()
	}
	a.mu.Unlock()
	turns, assets := a.projectTurns(n.ID, seg.Turns)
	resp := map[string]any{
		"turns": turns, "last_change": lastMS,
		"chat_started": seg.StartTime, "prior_turns": seg.PriorTurns,
	}
	if assets != nil {
		resp["assets"] = assets
	}
	if pm := a.proc(n); pm != nil {
		a.procChatInto(resp, n, pm, seg)
	} else {
		a.tmuxChatInto(resp, n, seg)
	}
	writeJSON(w, resp)
}

// projectTurns rewrites each turn's attachment markers (internal/asset.Project)
// and general agent-generated Markdown local-path references
// (internal/asset.ProjectAgentPaths, Phase 4) into scimux-asset:<id>
// references, and returns the projected turns plus the response's "assets"
// map, built from only the asset IDs actually referenced after projection —
// never the whole node's asset set. Read-time only: the stored log is never
// touched (upload-design.md "Render-Time Projection, Not Log Rewrite").
// ProjectAgentPaths runs even when byPath is empty — a Markdown path
// reference with no matching ingested asset still needs to become a defined
// "unavailable" chip rather than raw path text (Guaranteed Outcome), so it
// can't be skipped just because nothing has been ingested yet. turns is
// returned unmodified (assets nil) when the node has no session log at all.
func (a *app) projectTurns(nodeID string, turns []transcript.Turn) ([]transcript.Turn, map[string]any) {
	if a.sessionsDir == "" || len(turns) == 0 {
		return turns, nil
	}
	logPath := a.sessionLogPath(nodeID)
	byPath := sessionlog.ReadAssetsByPath(logPath)
	out := make([]transcript.Turn, len(turns))
	referenced := map[string]bool{}
	for i, t := range turns {
		t.Text = asset.Project(t.Text, byPath)
		t.Text = asset.ProjectAgentPaths(t.Text, byPath)
		for _, id := range asset.ReferencedIDs(t.Text) {
			referenced[id] = true
		}
		out[i] = t
	}
	if len(referenced) == 0 {
		return out, nil
	}
	idx := sessionlog.ReadAssets(logPath)
	assets := map[string]any{}
	for id := range referenced {
		if rec, ok := idx[id]; ok {
			assets[id] = a.assetSummary(nodeID, rec)
		}
	}
	return out, assets
}

// assetSummary builds one entry of the chat response's "assets" map (see
// upload-design.md "Rendering API"): inline assets carry their bytes as a
// data: URI so the client never round-trips to the download endpoint just
// to paint a thumbnail; blob assets carry the download URL instead.
func (a *app) assetSummary(nodeID string, rec sessionlog.AssetEvent) map[string]any {
	m := map[string]any{
		"name": rec.Name, "mime": rec.Mime, "size": rec.Size, "sha256": rec.SHA256,
	}
	if rec.Storage == "inline" {
		m["inline"] = true
		m["data"] = "data:" + rec.Mime + ";base64," + rec.Bytes
	} else {
		m["inline"] = false
		m["url"] = "/api/nodes/" + url.PathEscape(nodeID) + "/assets/" + url.PathEscape(rec.ID)
	}
	return m
}

// tmuxChatInto overlays the tmux-transport state onto the shared chat
// response: liveness, attention, delivery, and the fallback decision — the
// mechanics stay with the pane and the transcript tailer even though the
// turns themselves now come from the session log (the mirror keeps the log
// at most one poll tick behind the transcript).
func (a *app) tmuxChatInto(resp map[string]any, n *Node, seg sessionlog.Segment) {
	tl := a.tailerFor(n) // may reset staleness on a relink; read flags after
	a.mu.Lock()
	pending := n.Transcript == ""
	live := a.live[n.ID]
	stale := a.staleChat[n.ID]
	attn := a.attn[n.ID]
	delivery := a.sendState[n.ID]
	agent, model := n.Agent, n.Model
	a.mu.Unlock()
	turns := seg.Turns
	// fallback signals "the transcript is not (or no longer) making sense" —
	// missing, not yet populated (or not yet mirrored), unreadable,
	// format-incompatible from the start, structurally broken after valid
	// turns (Unparseable), or still growing through whole pane-activity
	// cycles without one recognizable chat record (staleChat: a typed future
	// format). A path string existing does not mean the transcript is usable;
	// the client must degrade to the pane snapshot in every one of these cases.
	fallback := len(turns) == 0 || (tl != nil && tl.Unparseable()) || stale

	// Diagnostics: where the chat content comes from and why attention (or
	// its absence) looks the way it does — so a missed question is
	// distinguishable from "no transcript" versus "no structured request".
	source := "transcript"
	switch {
	case agent == "pi" || agent == "opencode":
		source = "terminal_only" // supported via pane peek + send only
	case pending:
		source = "none"
	case fallback:
		source = "peek"
	}
	reason := ""
	switch {
	case live == "active":
		reason = "pane_active"
	case attn == "question":
		reason = "waiting_question"
	case attn == "approval":
		reason = "waiting_approval"
	case attn == "inspect":
		reason = "quiet_inspect"
	case pending:
		reason = "no_transcript"
	case fallback:
		reason = "no_structured_request"
	}
	var watermark int64
	var prog, pendCalls int
	var waiting string
	if tl != nil {
		watermark, prog = tl.Progress()
		pendCalls = tl.PendingCount()
		waiting, _ = tl.WaitingOn()
	}
	// Usage is segment-scoped: a /clear seam resets the gauge together with
	// the context it measures.
	ctxUsed := seg.Used
	ctxWindow := ctxWindowFor(seg.Used, seg.Size, model)
	ctxPct := ctxPctOf(ctxUsed, ctxWindow)
	resp["pending"] = pending
	resp["live"] = live
	resp["fallback"] = fallback
	resp["attention"] = attn
	resp["delivery"] = delivery
	resp["source"] = source
	resp["reason"] = reason
	resp["watermark"] = watermark
	resp["progress"] = prog
	resp["pending_calls"] = pendCalls
	resp["waiting_on"] = waiting
	resp["ctx_used"] = ctxUsed
	resp["ctx_window"] = ctxWindow
	resp["ctx_pct"] = ctxPct
}

// procChatInto overlays the structured-transport (ACP or codex app-server)
// state onto the shared chat response. No pane and no tailer: liveness and
// any pending permission come from the manager, while history and usage came
// from the same session log the manager writes. The tmux-only fields are
// neutralized (fallback false, delivery "", no watermark/pending_calls) so
// the web client's rendering path is shared; source "acp" selects the
// structured-node rendering path for both bridges; perm_* carries the pending
// approval and error a failed/empty turn.
func (a *app) procChatInto(resp map[string]any, n *Node, pm procManager, seg sessionlog.Segment) {
	live := pm.Live(n.ID)
	lastErr := pm.LastError(n.ID)
	permTitle, permOptions, hasPerm := pm.Pending(n.ID)
	attn := ""
	if hasPerm {
		attn = "approval"
	}
	reason := ""
	switch {
	case live == "active":
		reason = "turn_active"
	case attn == "approval":
		reason = "waiting_approval"
	case lastErr != "":
		reason = "turn_error"
	}
	ctxPct := ctxPctOf(seg.Used, seg.Size)
	resp["pending"] = false
	resp["live"] = live
	resp["fallback"] = false
	resp["attention"] = attn
	resp["delivery"] = ""
	resp["source"] = "acp"
	resp["reason"] = reason
	resp["watermark"] = int64(0)
	resp["progress"] = len(seg.Turns)
	resp["pending_calls"] = 0
	resp["waiting_on"] = permTitle
	resp["ctx_used"] = seg.Used
	resp["ctx_window"] = seg.Size
	resp["ctx_pct"] = ctxPct
	resp["error"] = lastErr
	resp["perm_title"] = permTitle
	resp["perm_options"] = permOptions
}

// handleKey presses one whitelisted key in the node's pane — answering an
// approval prompt or question menu remotely — and appends the pane's bottom
// lines plus the key to the store as decision evidence.
func (a *app) handleKey(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	if a.refuseEnded(w, n) {
		return
	}
	var body struct{ Key string }
	if err := decodeJSON(w, r, &body); err != nil || body.Key == "" {
		http.Error(w, "bad request", 400)
		return
	}
	if !tmuxsession.AllowedKey(body.Key) {
		http.Error(w, fmt.Sprintf("key %q not allowed", body.Key), 400)
		return
	}
	// Structured protocols (ACP, codex): the key answers a structured permission
	// request (there is no pane to press it into). Because resolution is
	// structured, the key can be mapped to an option and audited *before* the
	// agent is told the answer — unlike a tmux keypress, this need not be a
	// post-send best-effort write. Persist the decision first, then deliver, so
	// a store failure can never leave an unaudited permission answer already
	// acted on (finding 53). The tool title stands in for the pane excerpt as
	// decision evidence.
	if pm := a.proc(n); pm != nil {
		optID, evidence, err := pm.PrepareResolve(n.ID, body.Key)
		if err != nil {
			code := 400
			if pm.Conflict(err) {
				code = 409
			}
			http.Error(w, err.Error(), code)
			return
		}
		if err := a.appendRecord(storeRecord{Type: "key", ID: n.ID, Key: body.Key,
			Excerpt: evidence, Time: time.Now().UTC().Format(time.RFC3339)}); err != nil {
			http.Error(w, "refusing to answer without an audit record: "+err.Error(), 500)
			return
		}
		if err := pm.Deliver(n.ID, optID); err != nil {
			fmt.Fprintf(os.Stderr, "scimux: key %q audited on %s but delivery failed: %v\n", body.Key, n.ID, err)
			http.Error(w, "the decision was recorded, but delivering it to the agent failed: "+err.Error(), 500)
			return
		}
		writeJSON(w, map[string]string{"ok": "sent"})
		return
	}
	s := a.server.Session(n.ID)
	// Evidence capture is a prerequisite, not best-effort: a keypress whose
	// pane context cannot be photographed would produce an audit record that
	// cannot distinguish "empty pane" from "no evidence". Refuse and let the
	// supervisor retry (or attach) instead of acting unauditably.
	cap, err := s.Capture()
	if err != nil {
		http.Error(w, "refusing keypress without pane evidence (capture failed): "+err.Error(), 500)
		return
	}
	excerpt := lastLines(cap, 12)
	if err := s.SendKey(body.Key); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	// Deliberate non-poller write: successful key delivery clears attn/attnAt
	// now rather than waiting for the tool-call record to resolve (it only lands
	// when the approved tool completes, which for a long tool is minutes away —
	// R21.2). Failures before SendKey must not reach here. The mechanical
	// pipeline re-raises on the next tick if the dialog is still up, mirroring
	// what the structured path gets for free from pm.Attention.
	a.mu.Lock()
	a.attn[n.ID] = ""
	delete(a.attnAt, n.ID)
	a.mu.Unlock()
	// The audit record is part of the operation's success contract: a keypress
	// whose evidence cannot be persisted must not report plain success. The key
	// is already delivered (cannot be unsent), so say exactly that.
	if err := a.appendRecord(storeRecord{Type: "key", ID: n.ID, Key: body.Key,
		Excerpt: excerpt, Time: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		fmt.Fprintf(os.Stderr, "scimux: key %q sent to %s but audit record failed: %v\n", body.Key, n.ID, err)
		http.Error(w, "key was sent, but persisting the audit record failed: "+err.Error(), 500)
		return
	}
	writeJSON(w, map[string]string{"ok": "sent"})
}

// lastLines returns the last n lines of s with trailing blank lines removed —
// the visible bottom of a pane, where approval dialogs live.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, " \t\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// handlePeek returns the pane snapshot: the last 200 scrollback lines by
// default, or only the currently rendered screen with ?mode=visible — the
// decision view, where an approval dialog is not buried under history.
func (a *app) handlePeek(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	if pm := a.proc(n); pm != nil {
		// No pane to photograph: peek renders a tail of the raw event log.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, pm.Peek(n.ID))
		return
	}
	s := a.server.Session(n.ID)
	var cap string
	var err error
	if r.URL.Query().Get("mode") == "visible" {
		cap, err = s.CaptureVisible()
	} else {
		cap, err = s.Capture()
	}
	if err != nil {
		cap = "(session exited or unavailable)\n\n" + err.Error()
	} else {
		a.notePeekDialog(n, s)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, cap)
}

// notePeekDialog runs the corroborated dialog check when a human opens the
// terminal view. Peeking is exactly the manual gesture that catches a dialog
// the quiet gate cannot see (parallel calls queued behind it keep the pane
// animating), so automate it at the same moment: one visible capture,
// transcript corroboration required, attention only ever added — the poller
// keeps owning clearing. Deliberate non-poller raise: only unresolved
// structured evidence plus a visible dialog matcher may set attn/attnAt, and
// existing attention is never overwritten or restamped. Unlike the poller's
// tick path this is one-shot and human-triggered, so it skips the
// confined-animation gate.
func (a *app) notePeekDialog(n *Node, s *tmuxsession.Session) {
	tl := a.tailerFor(n)
	if tl == nil {
		return
	}
	tl.Poll()
	name, ok := tl.WaitingOn()
	if !ok {
		return
	}
	// Skip the visible-pane capture entirely when attention is already set — the
	// poller has classified this node and notePeekDialog only ever raises fresh
	// attention, never overwrites (efficiency, 2026-07-20 batch).
	a.mu.Lock()
	already := a.attn[n.ID] != ""
	a.mu.Unlock()
	if already {
		return
	}
	visible, err := s.CaptureVisible()
	if err != nil || !dialoghint.ClassifyVisible(visible) {
		return
	}
	a.mu.Lock()
	if a.attn[n.ID] == "" {
		a.attn[n.ID] = attentionKind(name)
		if a.attnAt == nil { // tests build app literals without the map
			a.attnAt = map[string]time.Time{}
		}
		a.attnAt[n.ID] = time.Now() // fresh evidence: start the preserve window (R21.2)
	}
	a.mu.Unlock()
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

// handleAgents reports the installed harnesses and their models — the
// new-activity dialog's source of truth (its built-in list is only the
// fallback for when this call fails).
func (a *app) handleAgents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, detectAgents())
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
