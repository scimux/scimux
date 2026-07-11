// scimux — supervise tmux-wrapped agent chats (Claude Code, Codex) from a
// local web page. Each chat is a node in a research tree; prompts go in via
// tmux, replies come back from the transcript files the agent CLIs write.
package main

import (
	"crypto/rand"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"codeberg.org/chrberger/scimux/internal/tmuxsession"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

//go:embed web/index.html
var webFS embed.FS

// ---------- model ----------

type Node struct {
	ID         string `json:"id"`
	Parent     string `json:"parent,omitempty"`
	Title      string `json:"title"`
	Prompt     string `json:"prompt"`              // first prompt == the node's research question
	Rationale  string `json:"rationale,omitempty"` // why this fork exists (decision evidence)
	Agent      string `json:"agent"`               // "claude" | "codex"
	Model      string `json:"model,omitempty"`
	Effort     string `json:"effort,omitempty"` // codex reasoning effort; ignored for claude
	Dir        string `json:"dir"`
	SessionID  string `json:"session_id,omitempty"` // claude session uuid (minted by us)
	Transcript string `json:"transcript,omitempty"`
	CreatedAt  string `json:"created_at"`
}

// storeRecord is one line of the append-only store file. Node metadata is
// tiny; chat content lives in the agents' own transcript files.
type storeRecord struct {
	Type string `json:"type"` // "node" | "transcript"
	Node *Node  `json:"node,omitempty"`
	ID   string `json:"id,omitempty"`
	Path string `json:"path,omitempty"`
}

type app struct {
	mu      sync.Mutex
	nodes   []*Node
	byID    map[string]*Node
	live    map[string]string // node id -> "active"|"quiet"|"exited"
	prevCap map[string]string
	lastChg map[string]time.Time
	tailers map[string]*transcript.Tailer

	server    *tmuxsession.Server
	storePath string
	codexRoot string
	home      string
}

// ---------- store ----------

func (a *app) appendRecord(rec storeRecord) error {
	f, err := os.OpenFile(a.storePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

func (a *app) loadStore() error {
	b, err := os.ReadFile(a.storePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec storeRecord
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		switch {
		case rec.Type == "node" && rec.Node != nil:
			a.nodes = append(a.nodes, rec.Node)
			a.byID[rec.Node.ID] = rec.Node
		case rec.Type == "transcript":
			if n, ok := a.byID[rec.ID]; ok {
				n.Transcript = rec.Path
			}
		}
	}
	return nil
}

// ---------- node lifecycle ----------

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

var slugStrip = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func (a *app) uniqueID(title string) string {
	slug := strings.Trim(slugStrip.ReplaceAllString(title, "-"), "-.")
	if slug == "" || !tmuxsession.ValidName(slug) {
		slug = "chat"
	}
	if len(slug) > 40 {
		slug = slug[:40]
	}
	id := slug
	for i := 2; ; i++ {
		if _, taken := a.byID[id]; !taken {
			return id
		}
		id = fmt.Sprintf("%s-%d", slug, i)
	}
}

// agentCommand builds the launch command. The first prompt rides on the
// command line so prompt delivery and session start are atomic — no
// "is the TUI drawn yet" race, which only later turns (via paste) tolerate.
func agentCommand(n *Node) (string, error) {
	switch n.Agent {
	case "claude":
		parts := []string{"claude", "--session-id", n.SessionID}
		if n.Model != "" {
			parts = append(parts, "--model", shellQuote(n.Model))
		}
		return strings.Join(append(parts, shellQuote(n.Prompt)), " "), nil
	case "codex":
		parts := []string{"codex"}
		if n.Model != "" {
			parts = append(parts, "--model", shellQuote(n.Model))
		}
		if n.Effort != "" {
			parts = append(parts, "-c", shellQuote("model_reasoning_effort="+n.Effort))
		}
		return strings.Join(append(parts, shellQuote(n.Prompt)), " "), nil
	}
	return "", fmt.Errorf("unknown agent %q (want claude or codex)", n.Agent)
}

// createNode validates, starts the tmux session, and persists the node.
// Callers hold a.mu.
func (a *app) createNode(n *Node) error {
	if strings.TrimSpace(n.Prompt) == "" {
		return fmt.Errorf("prompt must not be empty")
	}
	if n.Parent != "" {
		p, ok := a.byID[n.Parent]
		if !ok {
			return fmt.Errorf("parent %q not found", n.Parent)
		}
		// Fresh-context fork: inherit the launch config, never the conversation.
		if n.Agent == "" {
			n.Agent = p.Agent
		}
		if n.Model == "" {
			n.Model = p.Model
		}
		if n.Effort == "" {
			n.Effort = p.Effort
		}
		if n.Dir == "" {
			n.Dir = p.Dir
		}
	}
	if n.Agent == "" {
		n.Agent = "claude"
	}
	if n.Dir == "" {
		n.Dir = a.home
	}
	abs, err := filepath.Abs(n.Dir)
	if err != nil {
		return err
	}
	n.Dir = abs
	if st, err := os.Stat(n.Dir); err != nil || !st.IsDir() {
		return fmt.Errorf("dir %q is not an existing directory", n.Dir)
	}
	if n.Title == "" {
		n.Title = firstWords(n.Prompt, 6)
	}
	n.ID = a.uniqueID(n.Title)
	if n.Agent == "claude" {
		n.SessionID = newUUID()
	}
	n.CreatedAt = time.Now().UTC().Format(time.RFC3339)

	cmd, err := agentCommand(n)
	if err != nil {
		return err
	}
	if _, err := a.server.NewSession(n.ID, n.Dir, cmd); err != nil {
		return err
	}
	a.nodes = append(a.nodes, n)
	a.byID[n.ID] = n
	return a.appendRecord(storeRecord{Type: "node", Node: n})
}

func firstWords(s string, n int) string {
	words := strings.Fields(s)
	if len(words) > n {
		words = words[:n]
	}
	return strings.Join(words, " ")
}

// ---------- background poller ----------

// poll refreshes liveness (mechanical only: pane changed recently, quiet, or
// session gone) and runs one-time transcript discovery for young nodes.
func (a *app) poll() {
	a.mu.Lock()
	nodes := make([]*Node, len(a.nodes))
	copy(nodes, a.nodes)
	a.mu.Unlock()

	for _, n := range nodes {
		s := a.server.Session(n.ID)
		state := "exited"
		if s.Alive() {
			cap, err := s.Capture()
			if err == nil {
				a.mu.Lock()
				if cap != a.prevCap[n.ID] {
					a.prevCap[n.ID] = cap
					a.lastChg[n.ID] = time.Now()
				}
				if time.Since(a.lastChg[n.ID]) < 8*time.Second {
					state = "active"
				} else {
					state = "quiet"
				}
				a.mu.Unlock()
			} else {
				state = "unavailable"
			}
		}
		a.mu.Lock()
		a.live[n.ID] = state
		a.mu.Unlock()
		a.discoverTranscript(n)
	}
}

func (a *app) discoverTranscript(n *Node) {
	a.mu.Lock()
	done := n.Transcript != ""
	a.mu.Unlock()
	if done {
		return
	}
	created, err := time.Parse(time.RFC3339, n.CreatedAt)
	if err != nil || time.Since(created) > 15*time.Minute {
		return // stop searching for stale nodes; peek remains available
	}
	var path string
	var ok bool
	switch n.Agent {
	case "claude":
		path, ok = transcript.FindClaudeTranscript(a.home, n.SessionID)
	case "codex":
		path, ok = transcript.FindCodexRollout(a.codexRoot, n.Dir, created)
	}
	if !ok {
		return
	}
	a.mu.Lock()
	n.Transcript = path
	a.mu.Unlock()
	a.appendRecord(storeRecord{Type: "transcript", ID: n.ID, Path: path})
}

// ---------- sysload ----------

func sysload() map[string]string {
	out := map[string]string{}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) >= 3 {
			out["load"] = strings.Join(f[:3], " ")
		}
	}
	var totalKB, availKB float64
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			var v float64
			if _, err := fmt.Sscanf(line, "MemTotal: %f kB", &v); err == nil {
				totalKB = v
			}
			if _, err := fmt.Sscanf(line, "MemAvailable: %f kB", &v); err == nil {
				availKB = v
			}
		}
	}
	if totalKB > 0 {
		out["mem"] = fmt.Sprintf("%.0f%% of %.1f GB used", 100*(totalKB-availKB)/totalKB, totalKB/1024/1024)
	}
	return out
}

// ---------- HTTP ----------

type nodeView struct {
	*Node
	Live          string `json:"live"`
	HasTranscript bool   `json:"has_transcript"`
}

func (a *app) handleState(w http.ResponseWriter, r *http.Request) {
	sessions := a.server.Sessions()
	a.mu.Lock()
	views := make([]nodeView, 0, len(a.nodes))
	for _, n := range a.nodes {
		views = append(views, nodeView{Node: n, Live: a.live[n.ID], HasTranscript: n.Transcript != ""})
	}
	// Sessions on our socket that no node accounts for: candidates for
	// adoption (manually created, or migrated from another tmux server).
	unadopted := []string{}
	for _, s := range sessions {
		if _, known := a.byID[s]; !known {
			unadopted = append(unadopted, s)
		}
	}
	a.mu.Unlock()
	writeJSON(w, map[string]any{"nodes": views, "unadopted": unadopted, "sys": sysload(), "socket": a.server.Socket})
}

// handleAdopt registers an already-running tmux session (which scimux did
// not start) as a node. No tmux state is touched — this only writes the
// node record, so adoption is always safe for the running agent.
func (a *app) handleAdopt(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Session    string `json:"session"`
		Title      string `json:"title"`
		Prompt     string `json:"prompt"`
		Agent      string `json:"agent"`
		Model      string `json:"model"`
		Dir        string `json:"dir"`
		SessionID  string `json:"session_id"`
		Transcript string `json:"transcript"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Session == "" {
		http.Error(w, "bad request: need session", 400)
		return
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
	agent := body.Agent
	if agent == "" {
		agent = "claude"
	}
	n := &Node{ID: body.Session, Title: title, Prompt: body.Prompt, Agent: agent,
		Model: body.Model, Dir: dir, SessionID: body.SessionID, Transcript: body.Transcript,
		CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	// Adopted claude session without a known id: the newest session log in
	// the pane's working directory is almost certainly it (a migrated
	// --resume keeps appending to its original file). Best-effort — a wrong
	// guess still leaves peek + send working.
	if n.Agent == "claude" && n.SessionID == "" && n.Transcript == "" {
		if path, sid, ok := transcript.FindClaudeNewestInDir(a.home, n.Dir); ok {
			n.Transcript, n.SessionID = path, sid
		}
	}
	a.nodes = append(a.nodes, n)
	a.byID[n.ID] = n
	if err := a.appendRecord(storeRecord{Type: "node", Node: n}); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, n)
}

func (a *app) handleNewNode(w http.ResponseWriter, r *http.Request) {
	var n Node
	if err := json.NewDecoder(r.Body).Decode(&n); err != nil {
		http.Error(w, "bad request: "+err.Error(), 400)
		return
	}
	a.mu.Lock()
	err := a.createNode(&n)
	a.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, n)
}

func (a *app) node(r *http.Request) (*Node, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n, ok := a.byID[r.PathValue("id")]
	return n, ok
}

func (a *app) handleSend(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	var body struct{ Text string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Text) == "" {
		http.Error(w, "bad request", 400)
		return
	}
	if err := a.server.Session(n.ID).Send(body.Text); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]string{"ok": "sent"})
}

func (a *app) handleChat(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	a.mu.Lock()
	path := n.Transcript
	tl := a.tailers[n.ID]
	if tl == nil && path != "" {
		tl = &transcript.Tailer{Path: path}
		a.tailers[n.ID] = tl
	}
	live := a.live[n.ID]
	a.mu.Unlock()
	turns := []transcript.Turn{}
	if tl != nil {
		turns = tl.Poll()
	}
	writeJSON(w, map[string]any{"turns": turns, "pending": path == "", "live": live})
}

func (a *app) handlePeek(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	cap, err := a.server.Session(n.ID).Capture()
	if err != nil {
		cap = "(session exited or unavailable)\n\n" + err.Error()
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, cap)
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
	addr := flag.String("addr", "127.0.0.1:8787", "listen address (loopback only; use an SSH tunnel for remote access)")
	data := flag.String("data", filepath.Join(home, ".scimux"), "data directory for the node store")
	socket := flag.String("socket", "scimux", "tmux socket name (tmux -L) for the private server")
	codexRoot := flag.String("codex-sessions", filepath.Join(home, ".codex", "sessions"), "where Codex writes rollout logs")
	flag.Parse()

	if err := os.MkdirAll(*data, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "scimux:", err)
		os.Exit(1)
	}
	a := &app{
		byID:      map[string]*Node{},
		live:      map[string]string{},
		prevCap:   map[string]string{},
		lastChg:   map[string]time.Time{},
		tailers:   map[string]*transcript.Tailer{},
		server:    tmuxsession.NewServer(*socket),
		storePath: filepath.Join(*data, "nodes.jsonl"),
		codexRoot: *codexRoot,
		home:      home,
	}
	if err := a.loadStore(); err != nil {
		fmt.Fprintln(os.Stderr, "scimux: load store:", err)
		os.Exit(1)
	}

	go func() {
		for {
			a.poll()
			time.Sleep(2 * time.Second)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		b, _ := webFS.ReadFile("web/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// The UI is embedded in the binary and changes with every build;
		// a cached copy after a scimux upgrade is a recurring dogfooding
		// trap (especially iPad Safari). It's one small local page: always
		// fetch fresh.
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	})
	mux.HandleFunc("GET /api/state", a.handleState)
	mux.HandleFunc("POST /api/nodes", a.handleNewNode)
	mux.HandleFunc("POST /api/adopt", a.handleAdopt)
	mux.HandleFunc("POST /api/nodes/{id}/send", a.handleSend)
	mux.HandleFunc("GET /api/nodes/{id}/chat", a.handleChat)
	mux.HandleFunc("GET /api/nodes/{id}/peek", a.handlePeek)

	fmt.Printf("scimux: http://%s/  (tmux socket %q, store %s)\n", *addr, *socket, a.storePath)
	fmt.Printf("scimux: attach to a chat by hand: tmux -L %s attach -t <node-id>\n", *socket)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		fmt.Fprintln(os.Stderr, "scimux:", err)
		os.Exit(1)
	}
}
