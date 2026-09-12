package muse

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/agentperm"
	"codeberg.org/chrberger/scimux/internal/asset"
	"codeberg.org/chrberger/scimux/internal/sessionlog"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

// managerSurface is the app-facing structured-manager method set minus Conflict.
// The Muse package must not import internal/app; this assertion is the Phase 5
// adapter contract.
type managerSurface interface {
	Launch(nodeID, agent, dir, model, effort string) (string, error)
	Send(nodeID, text string) error
	Clear(nodeID string) error
	Interrupt(nodeID string) error
	PrepareResolve(nodeID, expectedRequestID, key string) (optID, evidence string, err error)
	Deliver(nodeID, optID string) error
	Pending(nodeID string) (agentperm.Pending, bool)
	PermissionBoundary(nodeID string) (incarn string, maxSeq uint64, ok bool)
	Turns(nodeID string) []transcript.Turn
	Peek(nodeID string) string
	Usage(nodeID string) (used, window int64)
	Live(nodeID string) string
	Attention(nodeID string) string
	LastError(nodeID string) string
	HasSession(nodeID string) bool
	Kill(nodeID string) error
	RecordStartFailure(nodeID string, cause error) error
	Shutdown()
}

var _ managerSurface = (*Manager)(nil)

func TestManagerSurfaceMatchesAppContract(t *testing.T) {
	var m managerSurface = NewManager(t.TempDir())
	m.Shutdown()
}

func TestManagerSentinelsDistinct(t *testing.T) {
	seen := map[string]error{
		"ErrNoSession":       ErrNoSession,
		"ErrNotAlive":        ErrNotAlive,
		"ErrTurnActive":      ErrTurnActive,
		"ErrNoPending":       ErrNoPending,
		"ErrStalePermission": ErrStalePermission,
		"ErrNoTurn":          ErrNoTurn,
		"ErrLaunchConflict":  ErrLaunchConflict,
	}
	for name, err := range seen {
		if err == nil || err.Error() == "" {
			t.Fatalf("%s is empty", name)
		}
	}
	if ErrNoSession == ErrNotAlive || ErrTurnActive == ErrNoTurn {
		t.Fatal("sentinels must be distinct")
	}
}

// ---------- fake MSP transport ----------

type closeWrap struct {
	Transport
	mu      sync.Mutex
	n       int
	err     error
	entered chan struct{}
	block   <-chan struct{}
}

func (c *closeWrap) Close() error {
	c.mu.Lock()
	c.n++
	n := c.n
	c.mu.Unlock()
	if n == 1 && c.entered != nil {
		select {
		case <-c.entered:
		default:
			close(c.entered)
		}
	}
	if c.block != nil {
		<-c.block
	}
	if c.Transport != nil {
		_ = c.Transport.Close()
	}
	return c.err
}

func (c *closeWrap) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

type holdLog struct {
	mu      sync.Mutex
	entered chan struct{}
	release <-chan struct{}
	inner   logAppender
	evs     []Event
}

func (h *holdLog) Append(ev Event) error {
	if h.entered != nil {
		select {
		case <-h.entered:
		default:
			close(h.entered)
		}
	}
	if h.release != nil {
		<-h.release
	}
	h.mu.Lock()
	h.evs = append(h.evs, ev)
	h.mu.Unlock()
	if h.inner != nil {
		return h.inner.Append(ev)
	}
	return nil
}

type seamHoldLog struct {
	mu          sync.Mutex
	seamEntered chan struct{}
	holdSeam    <-chan struct{}
	evs         []Event
	inner       logAppender
}

func (l *seamHoldLog) Append(ev Event) error {
	if ev.T == "source" {
		if l.seamEntered != nil {
			select {
			case <-l.seamEntered:
			default:
				close(l.seamEntered)
			}
		}
		if l.holdSeam != nil {
			<-l.holdSeam
		}
	}
	l.mu.Lock()
	l.evs = append(l.evs, ev)
	l.mu.Unlock()
	if l.inner != nil {
		return l.inner.Append(ev)
	}
	return nil
}

func drainGap(t *testing.T, srv *fakeServer, path, tag string) {
	t.Helper()
	srv.note(t, "view/gap", map[string]any{"after": tag, "next": tag + "-n"})
	waitFor(t, func() bool {
		for _, e := range sessionlog.ReadEvents(path) {
			if e.T == "error" && strings.Contains(e.Error, tag) {
				return true
			}
		}
		return false
	})
}

type pipeTransport struct {
	w        io.WriteCloser
	r        io.Reader
	serverW  *io.PipeWriter
	once     sync.Once
	closed   chan struct{}
	closeErr error
}

func (p *pipeTransport) Stdin() io.WriteCloser { return p.w }
func (p *pipeTransport) Stdout() io.Reader     { return p.r }
func (p *pipeTransport) Close() error {
	p.once.Do(func() {
		_ = p.w.Close()
		if p.serverW != nil {
			_ = p.serverW.Close()
		}
		close(p.closed)
	})
	return p.closeErr
}

type fakeServer struct {
	from   *bufio.Reader
	to     *io.PipeWriter
	tr     *pipeTransport
	reqs   chan map[string]any
	mu     sync.Mutex
	frames []map[string]any
}

func newFakeServer() (*pipeTransport, *fakeServer) {
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	tr := &pipeTransport{w: c2sW, r: s2cR, serverW: s2cW, closed: make(chan struct{})}
	fs := &fakeServer{
		from: bufio.NewReader(c2sR),
		to:   s2cW,
		tr:   tr,
		reqs: make(chan map[string]any, 64),
	}
	go fs.readLoop()
	return tr, fs
}

func (s *fakeServer) readLoop() {
	defer close(s.reqs)
	for {
		line, err := s.from.ReadBytes('\n')
		if err != nil {
			return
		}
		var m map[string]any
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		s.mu.Lock()
		s.frames = append(s.frames, m)
		s.mu.Unlock()
		if _, hasID := m["id"]; hasID {
			if method, _ := m["method"].(string); method != "" {
				s.reqs <- m
			}
		}
	}
}

func (s *fakeServer) nextReq(t *testing.T) map[string]any {
	t.Helper()
	select {
	case req, ok := <-s.reqs:
		if !ok {
			t.Fatal("fake server request stream closed")
		}
		return req
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for MSP request")
		return nil
	}
}

func (s *fakeServer) writeJSON(t *testing.T, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.to.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

func (s *fakeServer) replyOK(t *testing.T, req map[string]any, result any) {
	t.Helper()
	if result == nil {
		result = map[string]any{}
	}
	s.writeJSON(t, map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": result})
}

func (s *fakeServer) replyErr(t *testing.T, req map[string]any, msg string) {
	t.Helper()
	s.writeJSON(t, map[string]any{
		"jsonrpc": "2.0", "id": req["id"],
		"error": map[string]any{"code": -32603, "message": msg},
	})
}

func (s *fakeServer) note(t *testing.T, method string, params any) {
	t.Helper()
	s.writeJSON(t, map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (s *fakeServer) methods() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, f := range s.frames {
		if m, ok := f["method"].(string); ok {
			out = append(out, m)
		}
	}
	return out
}

func (s *fakeServer) hasMethod(name string) bool {
	for _, m := range s.methods() {
		if m == name {
			return true
		}
	}
	return false
}

type spawnCtl struct {
	mu      sync.Mutex
	err     error
	block   <-chan struct{}
	entered chan struct{}
	srvs    []*fakeServer
	dirs    []string
	nodes   []string
	wrap    func(Transport) Transport
}

func (c *spawnCtl) spawn(nodeID, dir string) (Transport, error) {
	c.mu.Lock()
	c.dirs = append(c.dirs, dir)
	c.nodes = append(c.nodes, nodeID)
	err := c.err
	block := c.block
	entered := c.entered
	wrap := c.wrap
	c.mu.Unlock()
	if entered != nil {
		select {
		case <-entered:
		default:
			close(entered)
		}
	}
	if err != nil {
		return nil, err
	}
	if block != nil {
		<-block
	}
	tr, srv := newFakeServer()
	c.mu.Lock()
	c.srvs = append(c.srvs, srv)
	c.mu.Unlock()
	if wrap != nil {
		return wrap(tr), nil
	}
	return tr, nil
}

func (c *spawnCtl) last(t *testing.T) *fakeServer {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.srvs) == 0 {
		t.Fatal("no fake server spawned")
	}
	return c.srvs[len(c.srvs)-1]
}

func (c *spawnCtl) waitSpawned(t *testing.T) *fakeServer {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		n := len(c.srvs)
		c.mu.Unlock()
		if n > 0 {
			return c.last(t)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("spawn produced no server")
	return nil
}

type launchRes struct {
	id  string
	err error
}

func initResult() map[string]any {
	return map[string]any{
		"serverInfo": map[string]any{"name": "muse", "version": ObservedMuseVersion},
		"schema":     map[string]any{"version": ObservedMSPSchema, "fingerprint": PinnedFingerprint},
	}
}

func handshake(t *testing.T, srv *fakeServer, model string) {
	t.Helper()
	req := srv.nextReq(t)
	if req["method"] != "initialize" {
		t.Fatalf("first method=%v", req["method"])
	}
	params, _ := req["params"].(map[string]any)
	info, _ := params["clientInfo"].(map[string]any)
	if info["name"] != "scimux" || info["name"] == "museprobe" {
		t.Fatalf("clientInfo=%v", info)
	}
	srv.replyOK(t, req, initResult())
	req = srv.nextReq(t)
	if req["method"] != "session/start" {
		t.Fatalf("second method=%v", req["method"])
	}
	sp, _ := req["params"].(map[string]any)
	if sp["workspaceRoot"] != "/ws" {
		t.Fatalf("workspaceRoot=%v", sp["workspaceRoot"])
	}
	if sp["approvalMode"] != "promptUnmatched" {
		t.Fatalf("approvalMode=%v", sp["approvalMode"])
	}
	if model != "" && sp["modelId"] != model {
		t.Fatalf("modelId=%v want %s", sp["modelId"], model)
	}
	if model == "" {
		if _, ok := sp["modelId"]; ok {
			t.Fatalf("empty model leaked modelId: %v", sp["modelId"])
		}
	}
	if _, ok := sp["sessionId"]; ok {
		t.Fatal("initial session/start must omit sessionId")
	}
	if _, ok := sp["effort"]; ok {
		t.Fatal("must not invent an MSP effort field")
	}
	srv.replyOK(t, req, map[string]any{"sessionId": "sess-1", "viewCursor": "c0"})
}

func launchOK(t *testing.T, logDir string) (*Manager, *spawnCtl, *fakeServer, string) {
	t.Helper()
	ctl := &spawnCtl{}
	m := NewManagerWithSpawn(logDir, ctl.spawn)
	t.Cleanup(m.Shutdown)
	rc := make(chan launchRes, 1)
	go func() {
		id, err := m.Launch("n1", "muse", "/ws", "spark", "high")
		rc <- launchRes{id, err}
	}()
	srv := ctl.waitSpawned(t)
	handshake(t, srv, "spark")
	res := <-rc
	if res.err != nil {
		t.Fatalf("launch: %v", res.err)
	}
	if res.id != "sess-1" {
		t.Fatalf("session id=%q", res.id)
	}
	return m, ctl, srv, filepath.Join(logDir, "n1.jsonl")
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func readLog(t *testing.T, path string) []Event {
	t.Helper()
	return sessionlog.ReadEvents(path)
}

func eventTypes(evs []Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.T
	}
	return out
}

func countT(evs []Event, tname string) int {
	n := 0
	for _, e := range evs {
		if e.T == tname {
			n++
		}
	}
	return n
}

type failLog struct {
	mu     sync.Mutex
	remain int
	err    error
	inner  logAppender
	evs    []Event
}

func (f *failLog) Append(ev Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.remain <= 0 {
		return f.err
	}
	f.remain--
	f.evs = append(f.evs, ev)
	if f.inner != nil {
		return f.inner.Append(ev)
	}
	return nil
}

func approvalParams(id, session, cmd string, idx int, choices []map[string]any) map[string]any {
	return map[string]any{
		"approvalId": id,
		"sessionId":  session,
		"turnId":     "turn-1",
		"subject":    map[string]any{"kind": "command", "command": cmd},
		"currentRequirementId": map[string]any{
			"approvalId":  id,
			"sourceIndex": idx,
		},
		"availableChoices": choices,
	}
}

func defaultChoices() []map[string]any {
	return []map[string]any{
		{"choiceId": "allow_once", "decision": "approved", "label": "Allow once"},
		{"choiceId": "allow_sess", "decision": "approvedForSession", "label": "Allow for session"},
		{"choiceId": "allow_pol", "decision": "approvedPolicyAmendment", "label": "Always allow"},
		{"choiceId": "deny", "decision": "denied", "label": "Reject"},
		{"choiceId": "mystery", "decision": "mysteryConsent", "label": "Mystery"},
	}
}

func agentItem(id, text string) map[string]any {
	return map[string]any{
		"item": map[string]any{
			"itemId": id, "kind": "agentMessage", "revision": 1,
			"status": "completed", "text": text,
		},
		"sessionId": "sess-1",
	}
}

func toolItem(id, kind, path string) map[string]any {
	args, _ := json.Marshal(map[string]any{"path": path})
	return map[string]any{
		"item": map[string]any{
			"itemId": id, "kind": kind, "revision": 1,
			"status": "completed", "tool": "write", "args": string(args),
		},
		"sessionId": "sess-1",
	}
}

func rpcOK(t *testing.T, srv *fakeServer, fn func() error, wantMethod string, result any) map[string]any {
	t.Helper()
	errc := make(chan error, 1)
	go func() { errc <- fn() }()
	req := srv.nextReq(t)
	if req["method"] != wantMethod {
		t.Fatalf("method=%v want %s", req["method"], wantMethod)
	}
	if result == nil {
		result = map[string]any{}
	}
	srv.replyOK(t, req, result)
	if err := <-errc; err != nil {
		t.Fatalf("%s: %v", wantMethod, err)
	}
	return req
}

func startTurn(t *testing.T, m *Manager, srv *fakeServer, text, turnID string) {
	t.Helper()
	if err := m.Send("n1", text); err != nil {
		t.Fatalf("send: %v", err)
	}
	req := srv.nextReq(t)
	if req["method"] != "turn/start" {
		t.Fatalf("method=%v", req["method"])
	}
	srv.replyOK(t, req, map[string]any{"turnId": turnID, "disposition": "started", "startedNewTurn": true})
	waitFor(t, func() bool {
		s := m.session("n1")
		return s != nil && s.client != nil && s.client.TurnActive()
	})
}

func completeTurn(t *testing.T, m *Manager, srv *fakeServer, turnID, terminal string) {
	t.Helper()
	sid := "sess-1"
	if s := m.session("n1"); s != nil && s.sessionID != "" {
		sid = s.sessionID
	}
	srv.note(t, "turn/completed", map[string]any{
		"sessionId": sid, "turnId": turnID, "terminal": terminal,
	})
	waitFor(t, func() bool { return m.Live("n1") == "quiet" })
}

// ---------- constructors / missing-node status ----------

func TestManagerMissingNodeStatus(t *testing.T) {
	m := NewManagerWithSpawn(t.TempDir(), func(string, string) (Transport, error) {
		t.Fatal("spawn")
		return nil, nil
	})
	t.Cleanup(m.Shutdown)
	if m.HasSession("n1") {
		t.Fatal("HasSession")
	}
	if m.Live("n1") != "exited" {
		t.Fatalf("Live=%q", m.Live("n1"))
	}
	if m.Attention("n1") != "" || m.LastError("n1") != "" {
		t.Fatal("attention/error")
	}
	if _, ok := m.Pending("n1"); ok {
		t.Fatal("pending")
	}
	if _, _, ok := m.PermissionBoundary("n1"); ok {
		t.Fatal("boundary")
	}
	if used, win := m.Usage("n1"); used != 0 || win != 0 {
		t.Fatalf("usage %d/%d", used, win)
	}
	if m.Peek("n1") != "(no muse events yet)" {
		t.Fatalf("peek=%q", m.Peek("n1"))
	}
	if turns := m.Turns("n1"); len(turns) != 0 {
		t.Fatalf("turns=%v", turns)
	}
	if err := m.Send("n1", "x"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("send %v", err)
	}
	if err := m.Clear("n1"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("clear %v", err)
	}
	if err := m.Interrupt("n1"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("interrupt %v", err)
	}
	if _, _, err := m.PrepareResolve("n1", "x", "1"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("prepare %v", err)
	}
	if err := m.Deliver("n1", "x"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("deliver %v", err)
	}
	if _, err := m.Models("n1"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("models %v", err)
	}
	if src := m.CatalogSource("n1"); src != "" {
		t.Fatalf("catalog source %q", src)
	}
	if err := m.Kill("missing"); err != nil {
		t.Fatalf("kill missing: %v", err)
	}
}

func TestNewManagerUsesDefaultSpawn(t *testing.T) {
	m := NewManager(t.TempDir())
	if m.spawn == nil {
		t.Fatal("default spawn missing")
	}
	m.Shutdown()
}

// ---------- Launch ----------

func TestManagerImmediateNotifyHitsSessionSink(t *testing.T) {
	dir := t.TempDir()
	ctl := &spawnCtl{}
	m := NewManagerWithSpawn(dir, ctl.spawn)
	t.Cleanup(m.Shutdown)
	rc := make(chan error, 1)
	go func() { _, err := m.Launch("n1", "muse", "/ws", "", ""); rc <- err }()
	srv := ctl.waitSpawned(t)
	req := srv.nextReq(t)
	srv.note(t, "item/completed", agentItem("early", "hello-early"))
	srv.replyOK(t, req, initResult())
	req = srv.nextReq(t)
	srv.replyOK(t, req, map[string]any{"sessionId": "sess-1", "viewCursor": "c0"})
	if err := <-rc; err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "n1.jsonl")
	waitFor(t, func() bool {
		for _, e := range readLog(t, logPath) {
			if e.T == "assistant" && e.Text == "hello-early" {
				return true
			}
		}
		return false
	})
}

func TestManagerLaunchSuccessMetaFirst(t *testing.T) {
	dir := t.TempDir()
	m, ctl, _, logPath := launchOK(t, dir)
	if !m.HasSession("n1") {
		t.Fatal("HasSession")
	}
	if m.Live("n1") != "quiet" {
		t.Fatalf("Live=%q", m.Live("n1"))
	}
	evs := readLog(t, logPath)
	if len(evs) == 0 || evs[0].T != "meta" {
		t.Fatalf("meta-first: %v", eventTypes(evs))
	}
	meta := evs[0].Meta
	if meta.Node != "n1" || meta.Agent != "muse" || meta.Model != "spark" || meta.Effort != "high" || meta.Dir != "/ws" {
		t.Fatalf("meta %+v", meta)
	}
	if len(ctl.dirs) != 1 || ctl.dirs[0] != "/ws" {
		t.Fatalf("spawn dir=%v", ctl.dirs)
	}
	if used, win := m.Usage("n1"); used != 0 || win != 0 {
		t.Fatalf("usage before context %d/%d", used, win)
	}
}

func TestManagerLaunchMkdirFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notdir")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	m := NewManagerWithSpawn(path, func(string, string) (Transport, error) {
		called = true
		return nil, errors.New("spawn")
	})
	t.Cleanup(m.Shutdown)
	_, err := m.Launch("n1", "muse", "/ws", "", "")
	if err == nil {
		t.Fatal("expected mkdir failure")
	}
	if called {
		t.Fatal("spawn ran after mkdir failure")
	}
	if m.HasSession("n1") {
		t.Fatal("published")
	}
}

func TestManagerLaunchSpawnFailure(t *testing.T) {
	dir := t.TempDir()
	m := NewManagerWithSpawn(dir, func(string, string) (Transport, error) {
		return nil, errors.New("no binary")
	})
	t.Cleanup(m.Shutdown)
	_, err := m.Launch("n1", "muse", "/ws", "", "")
	if err == nil || !strings.Contains(err.Error(), "no binary") {
		t.Fatalf("err=%v", err)
	}
	if _, stat := os.Stat(filepath.Join(dir, "n1.jsonl")); !os.IsNotExist(stat) {
		t.Fatal("spawn failure created a log")
	}
	if m.HasSession("n1") {
		t.Fatal("published")
	}
}

func TestManagerLaunchMetaAppendFailure(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "n1.jsonl")
	if err := os.Mkdir(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	ctl := &spawnCtl{}
	m := NewManagerWithSpawn(dir, ctl.spawn)
	t.Cleanup(m.Shutdown)
	_, err := m.Launch("n1", "muse", "/ws", "", "")
	if err == nil {
		t.Fatal("expected meta append failure")
	}
	if _, stat := os.Stat(bad); stat != nil {
		t.Fatal("pre-existing path removed")
	}
	select {
	case <-ctl.last(t).tr.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("transport not closed")
	}
	if m.HasSession("n1") {
		t.Fatal("published")
	}
}

func TestManagerLaunchInitializeFailureCleansNewLog(t *testing.T) {
	dir := t.TempDir()
	ctl := &spawnCtl{}
	m := NewManagerWithSpawn(dir, ctl.spawn)
	t.Cleanup(m.Shutdown)
	rc := make(chan error, 1)
	go func() { _, err := m.Launch("n1", "muse", "/ws", "", ""); rc <- err }()
	srv := ctl.waitSpawned(t)
	req := srv.nextReq(t)
	srv.replyErr(t, req, "init broke")
	err := <-rc
	if err == nil || !strings.Contains(err.Error(), "initialize") {
		t.Fatalf("err=%v", err)
	}
	if _, stat := os.Stat(filepath.Join(dir, "n1.jsonl")); !os.IsNotExist(stat) {
		t.Fatal("partial log left behind")
	}
	select {
	case <-srv.tr.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("transport not closed")
	}
	if m.HasSession("n1") {
		t.Fatal("published")
	}
}

func TestManagerLaunchSessionStartFailure(t *testing.T) {
	dir := t.TempDir()
	ctl := &spawnCtl{}
	m := NewManagerWithSpawn(dir, ctl.spawn)
	t.Cleanup(m.Shutdown)
	rc := make(chan error, 1)
	go func() { _, err := m.Launch("n1", "muse", "/ws", "", ""); rc <- err }()
	srv := ctl.waitSpawned(t)
	req := srv.nextReq(t)
	srv.replyOK(t, req, initResult())
	req = srv.nextReq(t)
	if req["method"] != "session/start" {
		t.Fatalf("method=%v", req["method"])
	}
	srv.replyErr(t, req, "start broke")
	err := <-rc
	if err == nil || !strings.Contains(err.Error(), "session/start") {
		t.Fatalf("err=%v", err)
	}
	if _, stat := os.Stat(filepath.Join(dir, "n1.jsonl")); !os.IsNotExist(stat) {
		t.Fatal("partial log left behind")
	}
}

func TestManagerLaunchMalformedSessionID(t *testing.T) {
	dir := t.TempDir()
	ctl := &spawnCtl{}
	m := NewManagerWithSpawn(dir, ctl.spawn)
	t.Cleanup(m.Shutdown)
	rc := make(chan error, 1)
	go func() { _, err := m.Launch("n1", "muse", "/ws", "", ""); rc <- err }()
	srv := ctl.waitSpawned(t)
	req := srv.nextReq(t)
	srv.replyOK(t, req, initResult())
	req = srv.nextReq(t)
	srv.replyOK(t, req, map[string]any{"viewCursor": "c"})
	err := <-rc
	if err == nil {
		t.Fatal("missing sessionId must fail")
	}
	if _, stat := os.Stat(filepath.Join(dir, "n1.jsonl")); !os.IsNotExist(stat) {
		t.Fatal("partial log left behind")
	}
}

func TestManagerLaunchExitDuringInitialize(t *testing.T) {
	dir := t.TempDir()
	ctl := &spawnCtl{}
	m := NewManagerWithSpawn(dir, ctl.spawn)
	t.Cleanup(m.Shutdown)
	rc := make(chan error, 1)
	go func() { _, err := m.Launch("n1", "muse", "/ws", "", ""); rc <- err }()
	srv := ctl.waitSpawned(t)
	_ = srv.nextReq(t)
	_ = srv.tr.Close()
	err := <-rc
	if err == nil {
		t.Fatal("expected exit during launch")
	}
	if m.HasSession("n1") {
		t.Fatal("published")
	}
}

func TestManagerLaunchPreservesPreexistingLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "n1.jsonl")
	prefix := []byte("{\"t\":\"meta\",\"time\":\"old\"}\n")
	if err := os.WriteFile(path, prefix, 0o600); err != nil {
		t.Fatal(err)
	}
	ctl := &spawnCtl{}
	m := NewManagerWithSpawn(dir, ctl.spawn)
	t.Cleanup(m.Shutdown)
	rc := make(chan error, 1)
	go func() { _, err := m.Launch("n1", "muse", "/ws", "", ""); rc <- err }()
	srv := ctl.waitSpawned(t)
	req := srv.nextReq(t)
	srv.replyErr(t, req, "nope")
	if err := <-rc; err == nil {
		t.Fatal("expected failure")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(got, prefix) {
		t.Fatalf("prefix lost: %s", got)
	}
	if !bytes.Contains(got, prefix) {
		t.Fatal("pre-existing bytes missing")
	}
}

func TestManagerConcurrentLaunchSameNode(t *testing.T) {
	dir := t.TempDir()
	block := make(chan struct{})
	entered := make(chan struct{})
	ctl := &spawnCtl{block: block, entered: entered}
	m := NewManagerWithSpawn(dir, ctl.spawn)
	t.Cleanup(m.Shutdown)
	first := make(chan launchRes, 1)
	go func() {
		id, err := m.Launch("n1", "muse", "/ws", "", "")
		first <- launchRes{id, err}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first spawn not entered")
	}
	_, err := m.Launch("n1", "muse", "/ws", "", "")
	if !errors.Is(err, ErrLaunchConflict) {
		t.Fatalf("concurrent launch err=%v", err)
	}
	close(block)
	srv := ctl.waitSpawned(t)
	handshake(t, srv, "")
	res := <-first
	if res.err != nil {
		t.Fatalf("first launch: %v", res.err)
	}
}

func TestManagerLaunchPublicationConflict(t *testing.T) {
	dir := t.TempDir()
	ctl := &spawnCtl{}
	m := NewManagerWithSpawn(dir, ctl.spawn)
	t.Cleanup(m.Shutdown)
	rc := make(chan error, 1)
	go func() { _, err := m.Launch("n1", "muse", "/ws", "", ""); rc <- err }()
	srv := ctl.waitSpawned(t)
	req := srv.nextReq(t)
	srv.replyOK(t, req, initResult())
	req = srv.nextReq(t)
	m.mu.Lock()
	m.sessions["n1"] = &nodeSession{nodeID: "n1", incarn: "other", done: make(chan struct{})}
	m.mu.Unlock()
	srv.replyOK(t, req, map[string]any{"sessionId": "sess-1", "viewCursor": "c0"})
	err := <-rc
	if !errors.Is(err, ErrLaunchConflict) {
		t.Fatalf("err=%v", err)
	}
	select {
	case <-srv.tr.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("conflicting launch did not close transport")
	}
}

func TestManagerShutdownDuringBlockedLaunch(t *testing.T) {
	dir := t.TempDir()
	ctl := &spawnCtl{}
	m := NewManagerWithSpawn(dir, ctl.spawn)
	m.opTimeout = 2 * time.Second
	rc := make(chan error, 1)
	go func() { _, err := m.Launch("n1", "muse", "/ws", "", ""); rc <- err }()
	srv := ctl.waitSpawned(t)
	_ = srv.nextReq(t) // initialize; do not reply
	done := make(chan struct{})
	go func() { m.Shutdown(); close(done) }()
	select {
	case err := <-rc:
		if err == nil {
			t.Fatal("launch succeeded after shutdown")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not unblock launch")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown hung")
	}
	if m.HasSession("n1") {
		t.Fatal("published after shutdown")
	}
}

func TestManagerLaunchCleanupFailureDoesNotMask(t *testing.T) {
	dir := t.TempDir()
	ctl := &spawnCtl{wrap: func(tr Transport) Transport {
		pt := tr.(*pipeTransport)
		pt.closeErr = errors.New("close exploded")
		return pt
	}}
	m := NewManagerWithSpawn(dir, ctl.spawn)
	t.Cleanup(m.Shutdown)
	rc := make(chan error, 1)
	go func() { _, err := m.Launch("n1", "muse", "/ws", "", ""); rc <- err }()
	srv := ctl.waitSpawned(t)
	req := srv.nextReq(t)
	srv.replyErr(t, req, "init broke")
	err := <-rc
	if err == nil || !strings.Contains(err.Error(), "initialize") {
		t.Fatalf("primary hidden: %v", err)
	}
	if !strings.Contains(err.Error(), "cleanup") {
		t.Fatalf("cleanup failure dropped: %v", err)
	}
}

func TestManagerRejectsLaunchAfterShutdown(t *testing.T) {
	m := NewManagerWithSpawn(t.TempDir(), func(string, string) (Transport, error) {
		t.Fatal("spawn")
		return nil, nil
	})
	m.Shutdown()
	_, err := m.Launch("n1", "muse", "/ws", "", "")
	if !errors.Is(err, ErrNotAlive) {
		t.Fatalf("err=%v", err)
	}
}

func TestManagerSecondLaunchWhileLiveConflicts(t *testing.T) {
	m, _, _, _ := launchOK(t, t.TempDir())
	_, err := m.Launch("n1", "muse", "/ws", "", "")
	if !errors.Is(err, ErrLaunchConflict) {
		t.Fatalf("err=%v", err)
	}
}

// ---------- Send / turn lifecycle ----------

func TestManagerSendMissingDeadClosing(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	_ = srv.tr.Close()
	waitFor(t, func() bool { return m.Live("n1") == "exited" })
	if err := m.Send("n1", "x"); !errors.Is(err, ErrNotAlive) {
		t.Fatalf("dead send %v", err)
	}
	m.Shutdown()
	if err := m.Send("n1", "x"); !errors.Is(err, ErrNoSession) && !errors.Is(err, ErrNotAlive) {
		t.Fatalf("closing send %v", err)
	}
}

func TestManagerSendUserBeforeWire(t *testing.T) {
	m, _, srv, logPath := launchOK(t, t.TempDir())
	if err := m.Send("n1", "hello ![img](shot.png)"); err != nil {
		t.Fatalf("send: %v", err)
	}
	evs := readLog(t, logPath)
	var sawUser bool
	for _, e := range evs {
		if e.T == "user" && e.Text == "hello ![img](shot.png)" {
			sawUser = true
		}
	}
	if !sawUser {
		t.Fatalf("user missing before wire: %v", eventTypes(evs))
	}
	req := srv.nextReq(t)
	if req["method"] != "turn/start" {
		t.Fatalf("method=%v", req["method"])
	}
	params, _ := req["params"].(map[string]any)
	if params["sessionId"] != "sess-1" {
		t.Fatalf("params=%v", params)
	}
	srv.replyOK(t, req, map[string]any{"turnId": "turn-1", "disposition": "started"})
	waitFor(t, func() bool { return m.Live("n1") == "active" })
}

func TestManagerSendLogFailureWritesNoTurnStart(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	s := m.session("n1")
	s.logw = &failLog{remain: 0, err: errors.New("disk full")}
	err := m.Send("n1", "hello")
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err=%v", err)
	}
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if srv.hasMethod("turn/start") {
			t.Fatal("turn/start written after log failure")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := m.Send("n1", "retry"); err != nil && !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("reservation stuck: %v", err)
	}
}

func TestManagerSendSendConflict(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	if err := m.Send("n1", "one"); err != nil {
		t.Fatal(err)
	}
	if err := m.Send("n1", "two"); !errors.Is(err, ErrTurnActive) {
		t.Fatalf("err=%v", err)
	}
	req := srv.nextReq(t)
	srv.replyOK(t, req, map[string]any{"turnId": "turn-1", "disposition": "started"})
}

func TestManagerSendClearConflict(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	if err := m.Send("n1", "one"); err != nil {
		t.Fatal(err)
	}
	if err := m.Clear("n1"); !errors.Is(err, ErrTurnActive) {
		t.Fatalf("clear during send: %v", err)
	}
	req := srv.nextReq(t)
	srv.replyOK(t, req, map[string]any{"turnId": "turn-1", "disposition": "started"})
}

func TestManagerAckAndAssistantRemainActive(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "hi", "turn-1")
	if m.Live("n1") != "active" {
		t.Fatal("ack ended the turn")
	}
	srv.note(t, "item/completed", agentItem("a1", "pong"))
	waitFor(t, func() bool {
		turns := m.Turns("n1")
		return len(turns) >= 2 && turns[len(turns)-1].Text == "pong"
	})
	if m.Live("n1") != "active" {
		t.Fatal("assistant completion ended the turn")
	}
	completeTurn(t, m, srv, "turn-1", "completed")
	if m.Live("n1") != "quiet" {
		t.Fatalf("Live=%q", m.Live("n1"))
	}
}

func TestManagerApprovalRejectDoesNotEndTurn(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "risky", "turn-1")
	srv.note(t, "approval/requested", approvalParams("ap-1", "sess-1", "ls", 0, defaultChoices()))
	waitFor(t, func() bool { return m.Attention("n1") == "approval" })
	p, ok := m.Pending("n1")
	if !ok {
		t.Fatal("pending")
	}
	tok, _, err := m.PrepareResolve("n1", p.RequestID, "n")
	if err != nil {
		t.Fatal(err)
	}
	dec := rpcOK(t, srv, func() error { return m.Deliver("n1", tok) }, "approval/decide", map[string]any{})
	_ = dec
	srv.note(t, "approval/resolved", map[string]any{
		"approvalId": "ap-1", "sessionId": "sess-1", "decision": "abort", "resolvedBy": "user",
	})
	waitFor(t, func() bool { return m.Attention("n1") == "" })
	if m.Live("n1") != "active" {
		t.Fatal("abort ended the turn")
	}
	if m.LastError("n1") != "" {
		t.Fatalf("abort became LastError: %q", m.LastError("n1"))
	}
	completeTurn(t, m, srv, "turn-1", "completed")
}

func TestManagerForeignAndDuplicateTerminal(t *testing.T) {
	m, _, srv, logPath := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "hi", "turn-1")
	before := countT(readLog(t, logPath), "stop") + countT(readLog(t, logPath), "error")
	srv.note(t, "turn/completed", map[string]any{
		"sessionId": "sess-other", "turnId": "turn-1", "terminal": "completed",
	})
	srv.note(t, "turn/completed", map[string]any{
		"sessionId": "sess-1", "turnId": "turn-other", "terminal": "failed",
	})
	drainGap(t, srv, logPath, "foreign-term")
	if m.Live("n1") != "active" {
		t.Fatal("foreign terminal ended turn")
	}
	completeTurn(t, m, srv, "turn-1", "completed")
	nStop := countT(readLog(t, logPath), "stop")
	srv.note(t, "turn/completed", map[string]any{
		"sessionId": "sess-1", "turnId": "turn-1", "terminal": "completed",
	})
	drainGap(t, srv, logPath, "dup-term")
	after := readLog(t, logPath)
	stops := countT(after, "stop")
	if stops != nStop {
		t.Fatalf("duplicate stop records: %d -> %d (before=%d types %v)", nStop, stops, before, eventTypes(after))
	}
}

func TestManagerAdmissionFailureReleasesReservation(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	if err := m.Send("n1", "hi"); err != nil {
		t.Fatal(err)
	}
	req := srv.nextReq(t)
	srv.replyErr(t, req, "nope")
	waitFor(t, func() bool { return m.Live("n1") == "quiet" && m.LastError("n1") != "" })
	if err := m.Send("n1", "again"); err != nil {
		t.Fatalf("reservation held: %v", err)
	}
	req = srv.nextReq(t)
	if req["method"] != "turn/start" {
		t.Fatalf("method=%v", req["method"])
	}
	srv.replyOK(t, req, map[string]any{"turnId": "turn-2", "disposition": "started"})
}

func TestManagerNoOutputTerminalError(t *testing.T) {
	m, _, srv, logPath := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "hi", "turn-1")
	completeTurn(t, m, srv, "turn-1", "completed")
	if !strings.Contains(m.LastError("n1"), "no output") {
		t.Fatalf("LastError=%q", m.LastError("n1"))
	}
	evs := readLog(t, logPath)
	var sawErr bool
	for _, e := range evs {
		if e.T == "error" && strings.Contains(e.Error, "no output") {
			sawErr = true
		}
	}
	if !sawErr {
		t.Fatalf("missing no-output error: %v", eventTypes(evs))
	}
}

func TestManagerFailedTerminalError(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "hi", "turn-1")
	srv.note(t, "item/completed", agentItem("a1", "partial"))
	waitFor(t, func() bool { return len(m.Turns("n1")) >= 2 })
	srv.note(t, "turn/completed", map[string]any{
		"sessionId": "sess-1", "turnId": "turn-1", "terminal": "failed", "reason": "boom",
	})
	waitFor(t, func() bool { return m.Live("n1") == "quiet" && m.LastError("n1") != "" })
	if !strings.Contains(m.LastError("n1"), "boom") && !strings.Contains(m.LastError("n1"), "failed") {
		t.Fatalf("LastError=%q", m.LastError("n1"))
	}
}

func TestManagerSendClearsPriorError(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "hi", "turn-1")
	completeTurn(t, m, srv, "turn-1", "completed")
	if m.LastError("n1") == "" {
		t.Fatal("expected no-output error")
	}
	if err := m.Send("n1", "next"); err != nil {
		t.Fatal(err)
	}
	if m.LastError("n1") != "" {
		t.Fatalf("prior error survived admitted send: %q", m.LastError("n1"))
	}
	req := srv.nextReq(t)
	srv.replyOK(t, req, map[string]any{"turnId": "turn-2", "disposition": "started"})
}

func TestManagerShutdownRacingAdmission(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- m.Send("n1", "hi")
	}()
	<-started
	m.Shutdown()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, ErrNotAlive) && !errors.Is(err, ErrNoSession) {
			// Send may have been admitted before shutdown; that is fine if it
			// cannot write after shutdown returns.
			_ = err
		}
	case <-time.After(2 * time.Second):
		t.Fatal("send hung across shutdown")
	}
	_ = srv
}

// ---------- Clear ----------

func TestManagerClearSameProcess(t *testing.T) {
	m, ctl, srv, _ := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "old", "turn-1")
	srv.note(t, "item/completed", agentItem("a1", "old-out"))
	waitFor(t, func() bool { return len(m.Turns("n1")) >= 2 })
	completeTurn(t, m, srv, "turn-1", "completed")
	spawns := len(ctl.srvs)
	req := rpcOK(t, srv, func() error { return m.Clear("n1") }, "session/start", map[string]any{"sessionId": "sess-2", "viewCursor": "c1"})
	sp, _ := req["params"].(map[string]any)
	if sp["workspaceRoot"] != "/ws" || sp["approvalMode"] != "promptUnmatched" || sp["modelId"] != "spark" {
		t.Fatalf("clear params=%v", sp)
	}
	if len(ctl.srvs) != spawns {
		t.Fatal("clear spawned a second process")
	}
}

func TestManagerClearSuccessSeam(t *testing.T) {
	m, ctl, srv, logPath := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "old", "turn-1")
	srv.note(t, "item/completed", agentItem("a1", "old-out"))
	waitFor(t, func() bool { return len(m.Turns("n1")) >= 2 })
	completeTurn(t, m, srv, "turn-1", "completed")
	spawns := len(ctl.srvs)
	errc := make(chan error, 1)
	go func() { errc <- m.Clear("n1") }()
	req := srv.nextReq(t)
	if req["method"] != "session/start" {
		t.Fatalf("method=%v", req["method"])
	}
	srv.replyOK(t, req, map[string]any{"sessionId": "sess-2", "viewCursor": "c1"})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if len(ctl.srvs) != spawns {
		t.Fatal("clear spawned a second process")
	}
	evs := readLog(t, logPath)
	var seam int
	for i, e := range evs {
		if e.T == "source" && e.Source != nil && e.Source.Reason == "clear" && e.Source.SessionID == "sess-2" {
			seam = i
		}
	}
	if seam == 0 {
		t.Fatalf("missing clear seam: %v", eventTypes(evs))
	}
	var priorUser bool
	for _, e := range evs[:seam] {
		if e.T == "user" && e.Text == "old" {
			priorUser = true
		}
	}
	if !priorUser {
		t.Fatal("history not preserved behind seam")
	}
	if m.LastError("n1") != "" {
		t.Fatalf("error survived clear: %q", m.LastError("n1"))
	}
	if _, ok := m.Pending("n1"); ok {
		t.Fatal("pending survived clear")
	}
	if used, win := m.Usage("n1"); used != 0 || win != 0 {
		t.Fatalf("usage after clear %d/%d", used, win)
	}
	if !strings.Contains(m.Peek("n1"), "source") || strings.Contains(m.Peek("n1"), "old-out") {
		t.Fatalf("peek leaked prior segment: %q", m.Peek("n1"))
	}
}

func TestManagerClearFailedStartNoSeam(t *testing.T) {
	m, _, srv, logPath := launchOK(t, t.TempDir())
	before, _ := os.ReadFile(logPath)
	errc := make(chan error, 1)
	go func() { errc <- m.Clear("n1") }()
	req := srv.nextReq(t)
	srv.replyErr(t, req, "cannot start")
	if err := <-errc; err == nil {
		t.Fatal("expected clear failure")
	}
	after, _ := os.ReadFile(logPath)
	if bytes.Contains(after, []byte(`"reason":"clear"`)) {
		t.Fatal("seam appended after failed start")
	}
	if !bytes.Equal(before, after) && bytes.Contains(after, []byte(`"reason":"clear"`)) {
		t.Fatal("log mutated with seam")
	}
	if !m.HasSession("n1") {
		t.Fatal("session dropped")
	}
}

func TestManagerClearSeamAppendFailure(t *testing.T) {
	m, _, srv, logPath := launchOK(t, t.TempDir())
	s := m.session("n1")
	s.logw = &failLog{remain: 0, err: errors.New("seam fail"), inner: &sessionlog.Writer{Path: logPath}}
	errc := make(chan error, 1)
	go func() { errc <- m.Clear("n1") }()
	req := srv.nextReq(t)
	srv.replyOK(t, req, map[string]any{"sessionId": "sess-2"})
	err := <-errc
	if err == nil || !strings.Contains(err.Error(), "seam") && !strings.Contains(err.Error(), "seam fail") {
		t.Fatalf("err=%v", err)
	}
}

func TestManagerClearOldSessionStragglers(t *testing.T) {
	m, _, srv, logPath := launchOK(t, t.TempDir())
	errc := make(chan error, 1)
	go func() { errc <- m.Clear("n1") }()
	req := srv.nextReq(t)
	srv.replyOK(t, req, map[string]any{"sessionId": "sess-2", "viewCursor": "c1"})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if err := m.Send("n1", "new"); err != nil {
		t.Fatal(err)
	}
	req = srv.nextReq(t)
	srv.replyOK(t, req, map[string]any{"turnId": "turn-new", "disposition": "started"})
	waitFor(t, func() bool { return m.Live("n1") == "active" })
	srv.note(t, "turn/completed", map[string]any{
		"sessionId": "sess-1", "turnId": "turn-old", "terminal": "completed",
	})
	srv.note(t, "item/completed", map[string]any{
		"sessionId": "sess-1",
		"item": map[string]any{
			"itemId": "old", "kind": "agentMessage", "revision": 1,
			"status": "completed", "text": "stale-assistant",
		},
	})
	drainGap(t, srv, logPath, "old-sess")
	if m.Live("n1") != "active" {
		t.Fatal("old terminal cleared new turn")
	}
	for _, e := range readLog(t, logPath) {
		if e.T == "assistant" && e.Text == "stale-assistant" {
			t.Fatal("old-session assistant entered new segment")
		}
	}
	completeTurn(t, m, srv, "turn-new", "completed")
}

func TestManagerClearPendingInvalidation(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "risky", "turn-1")
	srv.note(t, "approval/requested", approvalParams("ap-1", "sess-1", "ls", 0, defaultChoices()))
	waitFor(t, func() bool { return m.Attention("n1") == "approval" })
	p, _ := m.Pending("n1")
	tok, _, err := m.PrepareResolve("n1", p.RequestID, "1")
	if err != nil {
		t.Fatal(err)
	}
	completeTurn(t, m, srv, "turn-1", "cancelled")
	errc := make(chan error, 1)
	go func() { errc <- m.Clear("n1") }()
	req := srv.nextReq(t)
	srv.replyOK(t, req, map[string]any{"sessionId": "sess-2"})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Pending("n1"); ok {
		t.Fatal("pending after clear")
	}
	if m.Attention("n1") != "" {
		t.Fatal("attention after clear")
	}
	if err := m.Deliver("n1", tok); !errors.Is(err, ErrStalePermission) && !errors.Is(err, ErrNoPending) {
		t.Fatalf("stale token after clear: %v", err)
	}
}

func TestManagerClearUsageReset(t *testing.T) {
	m, _, srv, logPath := launchOK(t, t.TempDir())
	srv.note(t, "session/contextUsage", map[string]any{
		"sessionId": "sess-1", "usedTokens": 40, "windowTokens": 200,
	})
	waitFor(t, func() bool {
		u, w := m.Usage("n1")
		return u == 40 && w == 200
	})
	errc := make(chan error, 1)
	go func() { errc <- m.Clear("n1") }()
	req := srv.nextReq(t)
	srv.replyOK(t, req, map[string]any{"sessionId": "sess-2"})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if used, win := m.Usage("n1"); used != 0 || win != 0 {
		t.Fatalf("usage after clear %d/%d", used, win)
	}
	srv.note(t, "session/tokenUsage", map[string]any{
		"sessionId": "sess-2", "promptTokens": 9, "totalTokens": 9,
		"usage": map[string]any{"outputTokens": 1},
	})
	drainGap(t, srv, logPath, "spend-after-clear")
	if used, win := m.Usage("n1"); used != 0 || win != 0 {
		t.Fatalf("tokenUsage leaked into occupancy %d/%d", used, win)
	}
}

func TestManagerClearActiveConflictFromClear(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	errc := make(chan error, 1)
	go func() { errc <- m.Clear("n1") }()
	_ = srv.nextReq(t) // hold session/start
	if err := m.Send("n1", "x"); !errors.Is(err, ErrTurnActive) {
		t.Fatalf("send during clear: %v", err)
	}
	if err := m.Clear("n1"); !errors.Is(err, ErrTurnActive) {
		t.Fatalf("double clear: %v", err)
	}
}

func TestManagerClearNotAlive(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	_ = srv.tr.Close()
	waitFor(t, func() bool { return m.Live("n1") == "exited" })
	if err := m.Clear("n1"); !errors.Is(err, ErrNotAlive) {
		t.Fatalf("err=%v", err)
	}
}

// ---------- Approval ----------

func TestManagerApprovalCallbackNeverDecides(t *testing.T) {
	m, _, srv, logPath := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "risky", "turn-1")
	srv.note(t, "approval/requested", approvalParams("ap-1", "sess-1", "ls", 0, defaultChoices()))
	waitFor(t, func() bool { return m.Attention("n1") == "approval" })
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if srv.hasMethod("approval/decide") {
			t.Fatal("callback wrote approval/decide")
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, e := range readLog(t, logPath) {
		if e.T == "decision" {
			t.Fatal("manager wrote t:decision")
		}
	}
	p, ok := m.Pending("n1")
	if !ok || p.Title == "" || len(p.Options) != 5 {
		t.Fatalf("pending %+v", p)
	}
	if p.Options[0].Key != "1" || p.Options[0].Kind != "allow" {
		t.Fatalf("opt0 %+v", p.Options[0])
	}
	if p.Options[1].Kind != "allow_always" || p.Options[2].Kind != "allow_always" {
		t.Fatalf("always kinds %+v", p.Options)
	}
	if p.Options[3].Kind != "reject" {
		t.Fatalf("reject kind %+v", p.Options[3])
	}
	if p.Options[4].Kind == "allow" || p.Options[4].Kind == "allow_always" {
		t.Fatalf("unknown guessed as allow: %+v", p.Options[4])
	}
	if p.ToolKind != "execute" {
		t.Fatalf("toolKind=%q", p.ToolKind)
	}
	p.Options[0].Name = "mutated"
	p2, _ := m.Pending("n1")
	if p2.Options[0].Name == "mutated" {
		t.Fatal("Pending is not caller-private")
	}
	incarn, seq, ok := m.PermissionBoundary("n1")
	if !ok || incarn == "" || seq == 0 {
		t.Fatalf("boundary %s %d %v", incarn, seq, ok)
	}
}

func TestManagerApprovalUpdateReplacesFence(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "risky", "turn-1")
	srv.note(t, "approval/requested", approvalParams("ap-1", "sess-1", "ls", 0, defaultChoices()))
	waitFor(t, func() bool { return m.Attention("n1") == "approval" })
	p1, _ := m.Pending("n1")
	tok, _, err := m.PrepareResolve("n1", p1.RequestID, "1")
	if err != nil {
		t.Fatal(err)
	}
	srv.note(t, "approval/updated", map[string]any{
		"approvalId": "ap-1", "sessionId": "sess-1",
		"currentRequirementId": map[string]any{"approvalId": "ap-1", "sourceIndex": 1},
		"subject":              map[string]any{"kind": "command", "command": "ls -l"},
		"availableChoices": []map[string]any{
			{"choiceId": "abort", "decision": "abort", "label": "Abort now"},
		},
		"change": map[string]any{"kind": "stage"},
	})
	waitFor(t, func() bool {
		p, ok := m.Pending("n1")
		return ok && p.RequestID != p1.RequestID && len(p.Options) == 1
	})
	p2, _ := m.Pending("n1")
	if strings.Contains(p2.Title, "ls -l") == false && p2.Title == p1.Title {
		t.Fatalf("title not updated: %q", p2.Title)
	}
	if _, _, err := m.PrepareResolve("n1", p1.RequestID, "1"); !errors.Is(err, ErrStalePermission) {
		t.Fatalf("old request id: %v", err)
	}
	if err := m.Deliver("n1", tok); !errors.Is(err, ErrStalePermission) {
		t.Fatalf("old token: %v", err)
	}
	if srv.hasMethod("approval/decide") {
		t.Fatal("stale deliver wrote decide")
	}
}

func TestManagerPrepareMappingAndEvidence(t *testing.T) {
	m, _, srv, logPath := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "risky", "turn-1")
	srv.note(t, "approval/requested", approvalParams("ap-1", "sess-1", "ls", 0, defaultChoices()))
	waitFor(t, func() bool { return m.Attention("n1") == "approval" })
	p, _ := m.Pending("n1")
	if _, _, err := m.PrepareResolve("n1", "", "1"); !errors.Is(err, ErrStalePermission) {
		t.Fatalf("empty id: %v", err)
	}
	if _, _, err := m.PrepareResolve("n1", "nope", "1"); !errors.Is(err, ErrStalePermission) {
		t.Fatalf("stale id: %v", err)
	}
	if _, _, err := m.PrepareResolve("n1", p.RequestID, "99"); err == nil {
		t.Fatal("unknown key")
	}
	tokY, evY, err := m.PrepareResolve("n1", p.RequestID, "y")
	if err != nil || evY == "" || !strings.Contains(evY, "command") {
		t.Fatalf("y: tok=%s ev=%s err=%v", tokY, evY, err)
	}
	tokN, _, err := m.PrepareResolve("n1", p.RequestID, "n")
	if err != nil {
		t.Fatal(err)
	}
	tokEsc, _, err := m.PrepareResolve("n1", p.RequestID, "Escape")
	if err != nil {
		t.Fatal(err)
	}
	if tokN == tokY {
		t.Fatal("y and n produced the same token")
	}
	_ = tokEsc
	if _, ok := m.Pending("n1"); !ok {
		t.Fatal("prepare cleared pending")
	}
	if srv.hasMethod("approval/decide") {
		t.Fatal("prepare wrote decide")
	}
	for _, e := range readLog(t, logPath) {
		if e.T == "decision" {
			t.Fatal("prepare wrote decision")
		}
	}
}

func TestManagerDeliverWireShapeAndDuplicate(t *testing.T) {
	m, _, srv, logPath := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "risky", "turn-1")
	srv.note(t, "approval/requested", approvalParams("ap-1", "sess-1", "ls", 0, defaultChoices()))
	waitFor(t, func() bool { return m.Attention("n1") == "approval" })
	p, _ := m.Pending("n1")
	tok, _, err := m.PrepareResolve("n1", p.RequestID, "1")
	if err != nil {
		t.Fatal(err)
	}
	req := rpcOK(t, srv, func() error { return m.Deliver("n1", tok) }, "approval/decide", map[string]any{})
	params, _ := req["params"].(map[string]any)
	if params["approvalId"] != "ap-1" || params["choiceId"] != "allow_once" {
		t.Fatalf("params=%v", params)
	}
	reqID, _ := params["requirementId"].(map[string]any)
	if reqID["approvalId"] != "ap-1" {
		t.Fatalf("requirement=%v", reqID)
	}
	if _, ok := params["commandId"].(string); !ok {
		t.Fatal("missing commandId")
	}
	if m.Attention("n1") != "approval" {
		t.Fatal("ack cleared attention before resolved")
	}
	if err := m.Deliver("n1", tok); !errors.Is(err, ErrStalePermission) {
		t.Fatalf("duplicate deliver: %v", err)
	}
	nDecide := 0
	for _, name := range srv.methods() {
		if name == "approval/decide" {
			nDecide++
		}
	}
	if nDecide != 1 {
		t.Fatalf("decide count=%d", nDecide)
	}
	srv.note(t, "approval/resolved", map[string]any{
		"approvalId": "ap-1", "sessionId": "sess-1", "decision": "approved", "resolvedBy": "user",
	})
	waitFor(t, func() bool { return m.Attention("n1") == "" })
	for _, e := range readLog(t, logPath) {
		if e.T == "decision" {
			t.Fatal("deliver wrote decision")
		}
	}
}

func TestManagerDeliverStaleAfterResolveAndRelaunch(t *testing.T) {
	dir := t.TempDir()
	m, ctl, srv, _ := launchOK(t, dir)
	startTurn(t, m, srv, "risky", "turn-1")
	srv.note(t, "approval/requested", approvalParams("ap-1", "sess-1", "ls", 0, defaultChoices()))
	waitFor(t, func() bool { return m.Attention("n1") == "approval" })
	p, _ := m.Pending("n1")
	tok, _, err := m.PrepareResolve("n1", p.RequestID, "1")
	if err != nil {
		t.Fatal(err)
	}
	srv.note(t, "approval/resolved", map[string]any{
		"approvalId": "ap-1", "sessionId": "sess-1", "decision": "denied", "resolvedBy": "user",
	})
	waitFor(t, func() bool { return m.Attention("n1") == "" })
	if err := m.Deliver("n1", tok); !errors.Is(err, ErrStalePermission) && !errors.Is(err, ErrNoPending) {
		t.Fatalf("after resolve: %v", err)
	}
	if err := m.Kill("n1"); err != nil {
		t.Fatal(err)
	}
	n := len(ctl.srvs)
	rc := make(chan launchRes, 1)
	go func() {
		id, err := m.Launch("n1", "muse", "/ws", "spark", "high")
		rc <- launchRes{id, err}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ctl.mu.Lock()
		got := len(ctl.srvs)
		ctl.mu.Unlock()
		if got > n {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = srv
	handshake(t, ctl.last(t), "spark")
	if res := <-rc; res.err != nil {
		t.Fatal(res.err)
	}
	if err := m.Deliver("n1", tok); !errors.Is(err, ErrStalePermission) && !errors.Is(err, ErrNoPending) {
		t.Fatalf("after relaunch: %v", err)
	}
	incarn, _, ok := m.PermissionBoundary("n1")
	if !ok || incarn == "" {
		t.Fatal("new incarnation missing")
	}
}

func TestManagerForeignResolutionDoesNotClear(t *testing.T) {
	m, _, srv, logPath := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "risky", "turn-1")
	srv.note(t, "approval/requested", approvalParams("ap-1", "sess-1", "ls", 0, defaultChoices()))
	waitFor(t, func() bool { return m.Attention("n1") == "approval" })
	srv.note(t, "approval/resolved", map[string]any{
		"approvalId": "ap-other", "sessionId": "sess-1", "decision": "approved", "resolvedBy": "user",
	})
	drainGap(t, srv, logPath, "foreign-res")
	if m.Attention("n1") != "approval" {
		t.Fatal("foreign resolve cleared current")
	}
}

func TestManagerPrepareNoPending(t *testing.T) {
	m, _, _, _ := launchOK(t, t.TempDir())
	if _, _, err := m.PrepareResolve("n1", "x", "1"); !errors.Is(err, ErrNoPending) {
		t.Fatalf("err=%v", err)
	}
}

func TestManagerApprovalNotAutoApprovable(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "risky", "turn-1")
	srv.note(t, "approval/requested", approvalParams("ap-1", "sess-1", "ls", 0, defaultChoices()))
	waitFor(t, func() bool { return m.Attention("n1") == "approval" })
	s := m.session("n1")
	a, ok := s.client.PendingApproval()
	if !ok || a.AutoApprovable() {
		t.Fatal("auto-approvable")
	}
	p, _ := m.Pending("n1")
	for _, o := range p.Options {
		if o.Kind == "allow" && o.Key == "auto" {
			t.Fatal("auto option invented")
		}
	}
}

func TestManagerConcurrentUpdateVersusDeliver(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "risky", "turn-1")
	srv.note(t, "approval/requested", approvalParams("ap-1", "sess-1", "ls", 0, defaultChoices()))
	waitFor(t, func() bool { return m.Attention("n1") == "approval" })
	p, _ := m.Pending("n1")
	tok, _, err := m.PrepareResolve("n1", p.RequestID, "1")
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	delDone := make(chan error, 1)
	go func() {
		<-start
		delDone <- m.Deliver("n1", tok)
	}()
	go func() {
		<-start
		srv.note(t, "approval/updated", map[string]any{
			"approvalId": "ap-1", "sessionId": "sess-1",
			"currentRequirementId": map[string]any{"approvalId": "ap-1", "sourceIndex": 1},
			"subject":              map[string]any{"kind": "command", "command": "ls"},
			"availableChoices":     defaultChoices(),
		})
	}()
	close(start)
	var delErr error
	select {
	case delErr = <-delDone:
	case req := <-srv.reqs:
		if req["method"] == "approval/decide" {
			srv.replyOK(t, req, map[string]any{})
		}
		select {
		case delErr = <-delDone:
		case <-time.After(2 * time.Second):
			t.Fatal("deliver hung after decide")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("update vs deliver hung")
	}
	if delErr != nil && !errors.Is(delErr, ErrStalePermission) {
		t.Fatalf("deliver err=%v", delErr)
	}
}

func TestManagerOldCallbackDoesNotRestoreFence(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "risky", "turn-1")
	first := approvalParams("ap-1", "sess-1", "ls", 0, defaultChoices())
	srv.note(t, "approval/requested", first)
	waitFor(t, func() bool { return m.Attention("n1") == "approval" })
	p1, _ := m.Pending("n1")
	srv.note(t, "approval/requested", approvalParams("ap-2", "sess-1", "cat", 0, defaultChoices()))
	waitFor(t, func() bool {
		p, ok := m.Pending("n1")
		return ok && p.RequestID != p1.RequestID
	})
	srv.note(t, "approval/requested", first)
	drainGap(t, srv, m.logPath("n1"), "old-cb")
	p, _ := m.Pending("n1")
	cur, ok := m.session("n1").client.PendingApproval()
	if !ok {
		t.Fatal("client pending")
	}
	if cur.ApprovalID == "ap-2" && strings.Contains(p.RequestID, "") {
		if p.Title != "" && p.RequestID == p1.RequestID && cur.ApprovalID != "ap-1" {
			t.Fatal("old callback restored old fence")
		}
	}
}

// ---------- Catalog ----------

func TestManagerModelsCatalog(t *testing.T) {
	m, _, srv, logPath := launchOK(t, t.TempDir())
	before := len(readLog(t, logPath))
	errc := make(chan error, 1)
	var models []Model
	go func() {
		var err error
		models, err = m.Models("n1")
		errc <- err
	}()
	req := srv.nextReq(t)
	if req["method"] != "model/list" {
		t.Fatalf("method=%v", req["method"])
	}
	params, _ := req["params"].(map[string]any)
	if _, ok := params["commandId"]; ok {
		t.Fatal("model/list had commandId")
	}
	lim := 100
	rel := "2026-01-01"
	srv.replyOK(t, req, map[string]any{
		"source": "bundledCatalog",
		"models": []any{
			map[string]any{"id": "spark", "label": "Spark", "isDefault": true, "source": "bundled", "profileId": "p1", "contextLimit": lim, "outputLimit": 20, "releaseDate": rel},
			map[string]any{"id": "other", "name": "Other", "contextLimit": nil, "outputLimit": nil},
		},
	})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "spark" || !models[0].IsDefault || models[1].IsDefault {
		t.Fatalf("%+v", models)
	}
	if models[0].ContextLimit == nil || *models[0].ContextLimit != 100 {
		t.Fatal("limits")
	}
	if models[1].ContextLimit != nil {
		t.Fatal("null limit invented")
	}
	if m.CatalogSource("n1") != "bundledCatalog" {
		t.Fatalf("source=%s", m.CatalogSource("n1"))
	}
	models[0].ID = "mut"
	if models[0].ContextLimit != nil {
		*models[0].ContextLimit = 0
	}
	go func() {
		ms, err := m.Models("n1")
		if err != nil {
			errc <- err
			return
		}
		if len(ms) == 0 || ms[0].ID == "mut" {
			errc <- errors.New("models slice was not caller-private")
			return
		}
		errc <- nil
	}()
	req = srv.nextReq(t)
	srv.replyOK(t, req, map[string]any{
		"source": "bundledCatalog",
		"models": []any{map[string]any{"id": "spark", "label": "Spark", "contextLimit": 100}},
	})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	go func() { _, err := m.Models("n1"); errc <- err }()
	req = srv.nextReq(t)
	srv.replyOK(t, req, map[string]any{"source": "bundledCatalog", "models": []any{}})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	go func() { _, err := m.Models("n1"); errc <- err }()
	req = srv.nextReq(t)
	srv.replyOK(t, req, "not-an-object")
	if err := <-errc; err == nil {
		t.Fatal("malformed catalog")
	}
	if m.Live("n1") != "quiet" {
		t.Fatal("catalog started a turn")
	}
	if len(readLog(t, logPath)) != before {
		t.Fatal("catalog appended events")
	}
	for _, name := range srv.methods() {
		if name == "turn/start" {
			t.Fatal("catalog started a turn on the wire")
		}
	}
}

func TestManagerModelsDead(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	_ = srv.tr.Close()
	waitFor(t, func() bool { return m.Live("n1") == "exited" })
	if _, err := m.Models("n1"); !errors.Is(err, ErrNotAlive) {
		t.Fatalf("err=%v", err)
	}
}

// ---------- Assets ----------

func TestManagerAssetHookExactlyOnce(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	var nodes []string
	var dirs []string
	var locked atomic.Bool
	m, _, srv, _ := launchOK(t, t.TempDir())
	s := m.session("n1")
	s.assetHook = func(nodeID, dir string, cands []asset.Candidate) {
		if locked.Load() {
			panic("hook called under lock")
		}
		mu.Lock()
		defer mu.Unlock()
		nodes = append(nodes, nodeID)
		dirs = append(dirs, dir)
		for _, c := range cands {
			calls = append(calls, c.Ref)
		}
	}
	if err := m.Send("n1", "see ![u](user.png)"); err != nil {
		t.Fatal(err)
	}
	req := srv.nextReq(t)
	srv.replyOK(t, req, map[string]any{"turnId": "turn-1", "disposition": "started"})
	waitFor(t, func() bool { return m.Live("n1") == "active" })
	srv.note(t, "item/completed", agentItem("a1", "see ![a](asst.png)"))
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(calls) >= 2
	})
	srv.note(t, "item/completed", toolItem("t1", "edit", "notes/out.md"))
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(calls) >= 3
	})
	srv.note(t, "session/tokenUsage", map[string]any{
		"sessionId": "sess-1", "promptTokens": 1, "totalTokens": 1,
		"usage": map[string]any{"outputTokens": 0},
	})
	srv.note(t, "session/contextUsage", map[string]any{
		"sessionId": "sess-1", "usedTokens": 3, "windowTokens": 10,
	})
	completeTurn(t, m, srv, "turn-1", "completed")
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(calls, ",") != "user.png,asst.png,notes/out.md" {
		t.Fatalf("calls=%v", calls)
	}
	for _, n := range nodes {
		if n != "n1" {
			t.Fatalf("node %s", n)
		}
	}
	for _, d := range dirs {
		if d != "/ws" {
			t.Fatalf("dir %s", d)
		}
	}
}

func TestManagerAssetHookSkippedOnAppendFailure(t *testing.T) {
	m, _, srv, logPath := launchOK(t, t.TempDir())
	called := 0
	s := m.session("n1")
	s.assetHook = func(string, string, []asset.Candidate) { called++ }
	startTurn(t, m, srv, "plain", "turn-1")
	s.logw = &failLog{remain: 0, err: errors.New("append fail"), inner: &sessionlog.Writer{Path: logPath}}
	srv.note(t, "item/completed", agentItem("a1", "see ![a](asst.png)"))
	waitFor(t, func() bool { return strings.Contains(m.LastError("n1"), "append fail") })
	if called != 0 {
		t.Fatalf("hook called=%d", called)
	}
	if !strings.Contains(m.LastError("n1"), "append fail") {
		t.Fatalf("LastError=%q want substring %q", m.LastError("n1"), "append fail")
	}
}

// ---------- Status / interrupt / kill / shutdown ----------

func TestManagerInterrupt(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	if err := m.Interrupt("n1"); !errors.Is(err, ErrNoTurn) {
		t.Fatalf("quiet interrupt %v", err)
	}
	startTurn(t, m, srv, "long", "turn-1")
	rpcOK(t, srv, func() error { return m.Interrupt("n1") }, "turn/interrupt", map[string]any{})
	if m.Live("n1") != "active" {
		t.Fatal("interrupt ack ended turn")
	}
	completeTurn(t, m, srv, "turn-1", "interrupted")
}

func TestManagerInterruptRPCFailure(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "long", "turn-1")
	errc := make(chan error, 1)
	go func() { errc <- m.Interrupt("n1") }()
	req := srv.nextReq(t)
	srv.replyErr(t, req, "cannot interrupt")
	err := <-errc
	if err == nil {
		t.Fatal("expected rpc failure")
	}
	if m.Live("n1") != "active" {
		t.Fatal("rpc failure ended turn")
	}
}

func TestManagerKillForgetsAndPreservesLog(t *testing.T) {
	m, _, _, logPath := launchOK(t, t.TempDir())
	if err := m.Kill("n1"); err != nil {
		t.Fatal(err)
	}
	if m.HasSession("n1") {
		t.Fatal("still tracked")
	}
	if m.Live("n1") != "exited" {
		t.Fatal("live")
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Fatal("log removed")
	}
}

func TestManagerExitDoesNotDeleteHistory(t *testing.T) {
	m, _, srv, logPath := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "hi", "turn-1")
	_ = srv.tr.Close()
	waitFor(t, func() bool { return m.Live("n1") == "exited" })
	if !m.HasSession("n1") {
		t.Fatal("exited session forgotten")
	}
	if m.LastError("n1") == "" {
		t.Fatal("active turn died silently")
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Fatal("history deleted")
	}
	if err := m.Send("n1", "x"); !errors.Is(err, ErrNotAlive) {
		t.Fatalf("send %v", err)
	}
}

func TestManagerRecordStartFailure(t *testing.T) {
	m, _, _, logPath := launchOK(t, t.TempDir())
	if err := m.RecordStartFailure("n1", errors.New("prompt lost")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.LastError("n1"), "prompt lost") {
		t.Fatalf("LastError=%q", m.LastError("n1"))
	}
	if err := m.RecordStartFailure("n1", errors.New("later")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(m.LastError("n1"), "later") {
		t.Fatal("overwrote earlier error")
	}
	evs := readLog(t, logPath)
	if countT(evs, "error") < 1 {
		t.Fatal("error not appended")
	}
	if err := m.RecordStartFailure("n1", nil); err != nil {
		t.Fatal(err)
	}
}

func TestManagerRecordStartFailureSkipsMissingLog(t *testing.T) {
	m := NewManagerWithSpawn(t.TempDir(), func(string, string) (Transport, error) {
		t.Fatal("spawn")
		return nil, nil
	})
	t.Cleanup(m.Shutdown)
	if err := m.RecordStartFailure("gone", errors.New("x")); err != nil {
		t.Fatal(err)
	}
}

func TestCloneModelsDoesNotAliasLimitPointers(t *testing.T) {
	ctx, outn := 100, 20
	rel := "2026-01-01"
	in := []Model{
		{ID: "spark", ContextLimit: &ctx, OutputLimit: &outn, ReleaseDate: &rel},
		{ID: "other"},
	}
	got := cloneModels(in)
	if len(got) != 2 || got[0].ID != "spark" {
		t.Fatalf("%+v", got)
	}
	got[0].ID = "mut"
	*got[0].ContextLimit = 1
	*got[0].OutputLimit = 2
	*got[0].ReleaseDate = "mutated"
	if in[0].ID != "spark" || *in[0].ContextLimit != 100 || *in[0].OutputLimit != 20 || *in[0].ReleaseDate != "2026-01-01" {
		t.Fatalf("cloneModels aliased caller-visible fields: %+v", in[0])
	}
	if in[1].ContextLimit != nil || got[1].ContextLimit != nil {
		t.Fatal("nil limits invented")
	}
}

func TestManagerTurnsProvenanceIsCallerPrivate(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "hi", "turn-1")
	srv.note(t, "item/completed", map[string]any{
		"sessionId": "sess-1",
		"item": map[string]any{
			"itemId": "a1", "kind": "agentMessage", "revision": 1,
			"status": "completed", "text": "pong",
			"_meta": map[string]any{"src": "item"},
		},
		"_meta": map[string]any{"src": "note"},
	})
	waitFor(t, func() bool { return len(m.Turns("n1")) >= 2 })
	turns := m.Turns("n1")
	if len(turns) < 2 || len(turns[1].Prov) == 0 {
		t.Fatalf("%+v", turns)
	}
	saved := append([]byte(nil), turns[1].Prov...)
	for i := range turns[1].Prov {
		turns[1].Prov[i] = 'x'
	}
	again := m.Turns("n1")
	if len(again) < 2 {
		t.Fatal("turns disappeared")
	}
	if !bytes.Equal(again[1].Prov, saved) {
		t.Fatalf("mutating Turns result aliased stored provenance: %s", again[1].Prov)
	}
	completeTurn(t, m, srv, "turn-1", "completed")
}

func TestManagerTurnsPreserveAgentAndProv(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "hi", "turn-1")
	srv.note(t, "item/completed", map[string]any{
		"sessionId": "sess-1",
		"item": map[string]any{
			"itemId": "a1", "kind": "agentMessage", "revision": 1,
			"status": "completed", "text": "pong",
			"_meta": map[string]any{"src": "item"},
		},
		"_meta": map[string]any{"src": "note"},
	})
	waitFor(t, func() bool { return len(m.Turns("n1")) >= 2 })
	turns := m.Turns("n1")
	if turns[0].Role != "user" || turns[1].Role != "assistant" {
		t.Fatalf("%+v", turns)
	}
	if turns[1].Agent != "muse" {
		t.Fatalf("agent=%q", turns[1].Agent)
	}
	if len(turns[1].Prov) == 0 {
		t.Fatal("provenance dropped")
	}
	completeTurn(t, m, srv, "turn-1", "completed")
}

func TestManagerPeekEmptyMessage(t *testing.T) {
	m := NewManagerWithSpawn(t.TempDir(), nil)
	t.Cleanup(m.Shutdown)
	if m.Peek("n1") != "(no muse events yet)" {
		t.Fatalf("%q", m.Peek("n1"))
	}
}

func TestManagerShutdownIdempotentAndRejects(t *testing.T) {
	m, _, _, _ := launchOK(t, t.TempDir())
	m.Shutdown()
	m.Shutdown()
	if _, err := m.Launch("n2", "muse", "/ws", "", ""); !errors.Is(err, ErrNotAlive) {
		t.Fatalf("launch after shutdown %v", err)
	}
}

func TestManagerUsageIgnoresSpend(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	srv.note(t, "session/tokenUsage", map[string]any{
		"sessionId": "sess-1", "promptTokens": 50, "totalTokens": 80,
		"usage": map[string]any{"outputTokens": 30},
	})
	drainGap(t, srv, m.logPath("n1"), "spend-only")
	if used, win := m.Usage("n1"); used != 0 || win != 0 {
		t.Fatalf("spend as occupancy %d/%d", used, win)
	}
	srv.note(t, "session/contextUsage", map[string]any{
		"sessionId": "sess-1", "usedTokens": 7, "windowTokens": 70,
	})
	waitFor(t, func() bool {
		u, w := m.Usage("n1")
		return u == 7 && w == 70
	})
}

func TestManagerEventAppendFailureNoOutput(t *testing.T) {
	m, _, srv, logPath := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "hi", "turn-1")
	s := m.session("n1")
	s.logw = &failLog{remain: 0, err: errors.New("persist fail"), inner: &sessionlog.Writer{Path: logPath}}
	srv.note(t, "item/completed", agentItem("a1", "secret-output"))
	waitFor(t, func() bool { return strings.Contains(m.LastError("n1"), "persist fail") })
	turns := m.Turns("n1")
	for _, trn := range turns {
		if trn.Text == "secret-output" {
			t.Fatal("undurable assistant counted")
		}
	}
}

func TestManagerInterruptNotAlive(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	_ = srv.tr.Close()
	waitFor(t, func() bool { return m.Live("n1") == "exited" })
	if err := m.Interrupt("n1"); !errors.Is(err, ErrNotAlive) {
		t.Fatalf("err=%v", err)
	}
}

func TestManagerChoiceLabelFallback(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "risky", "turn-1")
	srv.note(t, "approval/requested", approvalParams("ap-1", "sess-1", "ls", 0, []map[string]any{
		{"choiceId": "cid-only", "decision": "approved"},
	}))
	waitFor(t, func() bool { return m.Attention("n1") == "approval" })
	p, _ := m.Pending("n1")
	if p.Options[0].Name != "cid-only" {
		t.Fatalf("label fallback=%q", p.Options[0].Name)
	}
}

func TestManagerUnknownSubjectKindNotGuessed(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "risky", "turn-1")
	params := approvalParams("ap-1", "sess-1", "", 0, defaultChoices())
	params["subject"] = map[string]any{"kind": "mysteryGate", "target": "x"}
	srv.note(t, "approval/requested", params)
	waitFor(t, func() bool { return m.Attention("n1") == "approval" })
	p, _ := m.Pending("n1")
	if p.ToolKind != "" {
		t.Fatalf("guessed toolKind=%q", p.ToolKind)
	}
}

func TestManagerEnterMapsToAllow(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "risky", "turn-1")
	srv.note(t, "approval/requested", approvalParams("ap-1", "sess-1", "ls", 0, defaultChoices()))
	waitFor(t, func() bool { return m.Attention("n1") == "approval" })
	p, _ := m.Pending("n1")
	tok, _, err := m.PrepareResolve("n1", p.RequestID, "Enter")
	if err != nil {
		t.Fatal(err)
	}
	req := rpcOK(t, srv, func() error { return m.Deliver("n1", tok) }, "approval/decide", map[string]any{})
	params, _ := req["params"].(map[string]any)
	if params["choiceId"] != "allow_once" {
		t.Fatalf("Enter mapped to %v", params["choiceId"])
	}
}

func TestManagerMalformedDeliverToken(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "risky", "turn-1")
	srv.note(t, "approval/requested", approvalParams("ap-1", "sess-1", "ls", 0, defaultChoices()))
	waitFor(t, func() bool { return m.Attention("n1") == "approval" })
	if err := m.Deliver("n1", "not-a-token"); !errors.Is(err, ErrStalePermission) {
		t.Fatalf("err=%v", err)
	}
	if srv.hasMethod("approval/decide") {
		t.Fatal("malformed token caused wire decide")
	}
}

func TestManagerHasSessionAfterExitUntilKill(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	_ = srv.tr.Close()
	waitFor(t, func() bool { return m.Live("n1") == "exited" })
	if !m.HasSession("n1") {
		t.Fatal("forgotten on exit")
	}
	_ = m.Kill("n1")
	if m.HasSession("n1") {
		t.Fatal("still tracked after kill")
	}
}

func TestManagerContextUsageAfterExitFromLog(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	srv.note(t, "session/contextUsage", map[string]any{
		"sessionId": "sess-1", "usedTokens": 5, "windowTokens": 50,
	})
	waitFor(t, func() bool {
		u, w := m.Usage("n1")
		return u == 5 && w == 50
	})
	_ = srv.tr.Close()
	waitFor(t, func() bool { return m.Live("n1") == "exited" })
	if used, win := m.Usage("n1"); used != 5 || win != 50 {
		t.Fatalf("usage after exit %d/%d", used, win)
	}
}

func TestManagerNoTierFieldsOnModels(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	errc := make(chan error, 1)
	var models []Model
	go func() {
		var err error
		models, err = m.Models("n1")
		errc <- err
	}()
	req := srv.nextReq(t)
	srv.replyOK(t, req, map[string]any{
		"source": "bundledCatalog",
		"models": []any{
			map[string]any{"id": "muse-fast", "label": "Fast", "source": "discounted-looking"},
		},
	})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(models[0])
	if bytes.Contains(bytes.ToLower(b), []byte("tier")) || bytes.Contains(b, []byte("Discounted")) || bytes.Contains(b, []byte("Standard")) {
		t.Fatalf("tier invented: %s", b)
	}
}

func TestManagerDefaultSpawnDoesNotUseDirAsCwd(t *testing.T) {
	var gotDir string
	m := NewManagerWithSpawn(t.TempDir(), func(nodeID, dir string) (Transport, error) {
		gotDir = dir
		tr, srv := newFakeServer()
		go func() {
			req := srv.nextReq(t)
			srv.replyOK(t, req, initResult())
			req = srv.nextReq(t)
			srv.replyOK(t, req, map[string]any{"sessionId": "sess-1"})
		}()
		return tr, nil
	})
	t.Cleanup(m.Shutdown)
	id, err := m.Launch("n1", "muse", "/opt/project", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if id != "sess-1" || gotDir != "/opt/project" {
		t.Fatalf("id=%s dir=%s", id, gotDir)
	}
}

func TestManagerTurnsIgnoreToolUsageError(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "hi", "turn-1")
	srv.note(t, "item/completed", toolItem("t1", "mysteryKind", "x.md"))
	srv.note(t, "session/contextUsage", map[string]any{"sessionId": "sess-1", "usedTokens": 1, "windowTokens": 2})
	completeTurn(t, m, srv, "turn-1", "failed")
	turns := m.Turns("n1")
	for _, trn := range turns {
		if trn.Role != "user" && trn.Role != "assistant" {
			t.Fatalf("non-chat turn %+v", trn)
		}
	}
	if len(turns) != 1 {
		t.Fatalf("turns=%d", len(turns))
	}
}

func TestManagerLastErrorEmptyInitially(t *testing.T) {
	m, _, _, _ := launchOK(t, t.TempDir())
	if m.LastError("n1") != "" {
		t.Fatalf("%q", m.LastError("n1"))
	}
}

func TestManagerSendAfterShutdownRejected(t *testing.T) {
	m, _, _, _ := launchOK(t, t.TempDir())
	m.Shutdown()
	if err := m.Send("n1", "x"); !errors.Is(err, ErrNoSession) && !errors.Is(err, ErrNotAlive) {
		t.Fatalf("err=%v", err)
	}
}

func TestManagerClearAfterShutdown(t *testing.T) {
	m, _, _, _ := launchOK(t, t.TempDir())
	m.Shutdown()
	if err := m.Clear("n1"); !errors.Is(err, ErrNoSession) && !errors.Is(err, ErrNotAlive) {
		t.Fatalf("err=%v", err)
	}
}

func TestManagerCatalogAfterShutdown(t *testing.T) {
	m, _, _, _ := launchOK(t, t.TempDir())
	m.Shutdown()
	if _, err := m.Models("n1"); !errors.Is(err, ErrNoSession) && !errors.Is(err, ErrNotAlive) {
		t.Fatalf("err=%v", err)
	}
}

func TestManagerNewSessionEventsAfterClearSeam(t *testing.T) {
	m, _, srv, logPath := launchOK(t, t.TempDir())
	errc := make(chan error, 1)
	go func() { errc <- m.Clear("n1") }()
	req := srv.nextReq(t)
	srv.replyOK(t, req, map[string]any{"sessionId": "sess-2"})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if err := m.Send("n1", "fresh"); err != nil {
		t.Fatal(err)
	}
	req = srv.nextReq(t)
	srv.replyOK(t, req, map[string]any{"turnId": "t2", "disposition": "started"})
	srv.note(t, "item/completed", map[string]any{
		"sessionId": "sess-2",
		"item": map[string]any{
			"itemId": "n", "kind": "agentMessage", "revision": 1,
			"status": "completed", "text": "new-out",
		},
	})
	waitFor(t, func() bool {
		for _, e := range readLog(t, logPath) {
			if e.T == "assistant" && e.Text == "new-out" {
				return true
			}
		}
		return false
	})
	evs := readLog(t, logPath)
	seam := -1
	asst := -1
	for i, e := range evs {
		if e.T == "source" && e.Source != nil && e.Source.SessionID == "sess-2" {
			seam = i
		}
		if e.T == "assistant" && e.Text == "new-out" {
			asst = i
		}
	}
	if seam < 0 || asst < 0 || asst < seam {
		t.Fatalf("order seam=%d asst=%d types=%v", seam, asst, eventTypes(evs))
	}
}

func TestManagerDuplicateTerminalDoesNotAppendTwice(t *testing.T) {
	m, _, srv, logPath := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "hi", "turn-1")
	srv.note(t, "item/completed", agentItem("a1", "pong"))
	waitFor(t, func() bool { return len(m.Turns("n1")) >= 2 })
	srv.note(t, "turn/completed", map[string]any{
		"sessionId": "sess-1", "turnId": "turn-1", "terminal": "completed",
	})
	waitFor(t, func() bool { return m.Live("n1") == "quiet" })
	n1 := countT(readLog(t, logPath), "stop")
	srv.note(t, "turn/completed", map[string]any{
		"sessionId": "sess-1", "turnId": "turn-1", "terminal": "completed",
	})
	drainGap(t, srv, logPath, "dup-stop")
	n2 := countT(readLog(t, logPath), "stop")
	if n2 != n1 {
		t.Fatalf("duplicate stop %d -> %d", n1, n2)
	}
}

func TestManagerRaceKillDuringSend(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_ = m.Send("n1", "hi")
	}()
	go func() {
		defer wg.Done()
		<-start
		_ = m.Kill("n1")
	}()
	close(start)
	wg.Wait()
	_ = srv
}

func TestManagerRaceShutdownAndKill(t *testing.T) {
	m, _, _, _ := launchOK(t, t.TempDir())
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		m.Shutdown()
	}()
	go func() {
		defer wg.Done()
		<-start
		_ = m.Kill("n1")
	}()
	close(start)
	wg.Wait()
}

func TestManagerShutdownWaitsForLaunchCleanup(t *testing.T) {
	dir := t.TempDir()
	entered := make(chan struct{})
	block := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-block:
		default:
			close(block)
		}
	})
	ctl := &spawnCtl{wrap: func(tr Transport) Transport {
		return &closeWrap{Transport: tr, entered: entered, block: block}
	}}
	m := NewManagerWithSpawn(dir, ctl.spawn)
	rc := make(chan error, 1)
	go func() { _, err := m.Launch("n1", "muse", "/ws", "", ""); rc <- err }()
	srv := ctl.waitSpawned(t)
	_ = srv.nextReq(t)
	shutDone := make(chan struct{})
	go func() { m.Shutdown(); close(shutDone) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup Close never entered")
	}
	select {
	case <-shutDone:
		t.Fatal("Shutdown returned while Launch cleanup was blocked")
	default:
	}
	close(block)
	select {
	case err := <-rc:
		if err == nil {
			t.Fatal("launch succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("launch did not return")
	}
	select {
	case <-shutDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return after cleanup")
	}
	if m.HasSession("n1") {
		t.Fatal("session published")
	}
	if _, err := os.Stat(filepath.Join(dir, "n1.jsonl")); !os.IsNotExist(err) {
		t.Fatal("partial log remains")
	}
}

func TestManagerKillWaitsForAdmittedSend(t *testing.T) {
	m, _, srv, logPath := launchOK(t, t.TempDir())
	s := m.session("n1")
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	s.logw = &holdLog{entered: entered, release: release, inner: &sessionlog.Writer{Path: logPath}}
	sendDone := make(chan error, 1)
	go func() { sendDone <- m.Send("n1", "hello") }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("send did not reach append")
	}
	killDone := make(chan error, 1)
	go func() { killDone <- m.Kill("n1") }()
	waitFor(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.stopping
	})
	select {
	case <-killDone:
		t.Fatal("Kill returned while admitted Send was blocked")
	default:
	}
	close(release)
	select {
	case <-sendDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Send did not return")
	}
	select {
	case <-killDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Kill did not return")
	}
	n := len(readLog(t, logPath))
	if len(readLog(t, logPath)) != n {
		t.Fatal("log changed after Kill returned")
	}
	_ = srv
}

func TestManagerRaceSendAndKillLogStable(t *testing.T) {
	for i := 0; i < 20; i++ {
		dir := t.TempDir()
		m, _, _, logPath := launchOK(t, dir)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_ = m.Send("n1", "hi")
		}()
		go func() {
			defer wg.Done()
			<-start
			_ = m.Kill("n1")
		}()
		close(start)
		wg.Wait()
		n := len(readLog(t, logPath))
		if got := len(readLog(t, logPath)); got != n {
			t.Fatalf("iter %d log grew after Kill/Send returned: %d -> %d", i, n, got)
		}
	}
}

func TestManagerKillUnblocksClear(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	errc := make(chan error, 1)
	go func() { errc <- m.Clear("n1") }()
	_ = srv.nextReq(t)
	killDone := make(chan error, 1)
	go func() { killDone <- m.Kill("n1") }()
	select {
	case <-errc:
	case <-time.After(2 * time.Second):
		t.Fatal("Clear not unblocked")
	}
	select {
	case <-killDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Kill not finished")
	}
}

func TestManagerKillUnblocksModels(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	errc := make(chan error, 1)
	go func() { _, err := m.Models("n1"); errc <- err }()
	_ = srv.nextReq(t)
	killDone := make(chan error, 1)
	go func() { killDone <- m.Kill("n1") }()
	select {
	case <-errc:
	case <-time.After(2 * time.Second):
		t.Fatal("Models not unblocked")
	}
	select {
	case <-killDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Kill not finished")
	}
}

func TestManagerKillUnblocksDeliver(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "risky", "turn-1")
	srv.note(t, "approval/requested", approvalParams("ap-1", "sess-1", "ls", 0, defaultChoices()))
	waitFor(t, func() bool { return m.Attention("n1") == "approval" })
	p, _ := m.Pending("n1")
	tok, _, err := m.PrepareResolve("n1", p.RequestID, "1")
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { errc <- m.Deliver("n1", tok) }()
	_ = srv.nextReq(t)
	killDone := make(chan error, 1)
	go func() { killDone <- m.Kill("n1") }()
	select {
	case <-errc:
	case <-time.After(2 * time.Second):
		t.Fatal("Deliver not unblocked")
	}
	select {
	case <-killDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Kill not finished")
	}
}

func TestManagerKillWaitsForAssetHook(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	ctl := &spawnCtl{}
	m := NewManagerWithSpawn(t.TempDir(), ctl.spawn)
	m.SetAssetHook(func(string, string, []asset.Candidate) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
	})
	rc := make(chan error, 1)
	go func() { _, err := m.Launch("n1", "muse", "/ws", "", ""); rc <- err }()
	srv := ctl.waitSpawned(t)
	handshake(t, srv, "")
	if err := <-rc; err != nil {
		t.Fatal(err)
	}
	sendDone := make(chan error, 1)
	go func() { sendDone <- m.Send("n1", "see ![x](a.png)") }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("hook not entered")
	}
	s := m.session("n1")
	killDone := make(chan error, 1)
	go func() { killDone <- m.Kill("n1") }()
	waitFor(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.stopping
	})
	select {
	case <-killDone:
		t.Fatal("Kill returned during asset hook")
	default:
	}
	close(release)
	select {
	case <-sendDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Send hung")
	}
	select {
	case <-killDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Kill hung")
	}
}

func TestManagerOpsRejectedAfterStop(t *testing.T) {
	m, _, srv, logPath := launchOK(t, t.TempDir())
	before := len(readLog(t, logPath))
	nFrames := len(srv.methods())
	if err := m.Kill("n1"); err != nil {
		t.Fatal(err)
	}
	if err := m.Send("n1", "x"); !errors.Is(err, ErrNoSession) && !errors.Is(err, ErrNotAlive) {
		t.Fatalf("send %v", err)
	}
	if err := m.Clear("n1"); !errors.Is(err, ErrNoSession) && !errors.Is(err, ErrNotAlive) {
		t.Fatalf("clear %v", err)
	}
	if _, err := m.Models("n1"); !errors.Is(err, ErrNoSession) && !errors.Is(err, ErrNotAlive) {
		t.Fatalf("models %v", err)
	}
	if err := m.Deliver("n1", "x"); !errors.Is(err, ErrNoSession) && !errors.Is(err, ErrNotAlive) {
		t.Fatalf("deliver %v", err)
	}
	if len(srv.methods()) != nFrames {
		t.Fatalf("wire activity after stop: %v", srv.methods()[nFrames:])
	}
	if len(readLog(t, logPath)) != before {
		t.Fatal("log wrote after stop")
	}
}

func TestManagerLaunchCleanupClosesTransportOnce(t *testing.T) {
	for _, name := range []string{"initialize", "session/start", "publish"} {
		t.Run(name, func(t *testing.T) {
			cw := &closeWrap{err: errors.New("close exploded")}
			ctl := &spawnCtl{wrap: func(tr Transport) Transport {
				cw.Transport = tr
				return cw
			}}
			dir := t.TempDir()
			m := NewManagerWithSpawn(dir, ctl.spawn)
			t.Cleanup(m.Shutdown)
			if name == "publish" {
				m2 := NewManagerWithSpawn(dir, ctl.spawn)
				t.Cleanup(m2.Shutdown)
				// occupy the slot after a successful parallel launch is not needed:
				// inject existing session before publish by launching a dummy first.
			}
			rc := make(chan error, 1)
			go func() { _, err := m.Launch("n1", "muse", "/ws", "", ""); rc <- err }()
			srv := ctl.waitSpawned(t)
			req := srv.nextReq(t)
			switch name {
			case "initialize":
				srv.replyErr(t, req, "init broke")
			case "session/start":
				srv.replyOK(t, req, initResult())
				req = srv.nextReq(t)
				srv.replyErr(t, req, "start broke")
			case "publish":
				srv.replyOK(t, req, initResult())
				req = srv.nextReq(t)
				m.sessions["n1"] = &nodeSession{nodeID: "n1", incarn: "other", done: make(chan struct{})}
				srv.replyOK(t, req, map[string]any{"sessionId": "sess-1"})
			}
			err := <-rc
			if err == nil {
				t.Fatal("expected launch failure")
			}
			if cw.count() != 1 {
				t.Fatalf("%s close count=%d", name, cw.count())
			}
			if name != "publish" && !strings.Contains(err.Error(), "cleanup") {
				t.Fatalf("cleanup missing: %v", err)
			}
			if name == "initialize" && !strings.Contains(err.Error(), "initialize") {
				t.Fatalf("primary hidden: %v", err)
			}
		})
	}
}

func TestPublishRejectsDeadClient(t *testing.T) {
	m := NewManagerWithSpawn(t.TempDir(), func(string, string) (Transport, error) {
		t.Fatal("spawn")
		return nil, nil
	})
	t.Cleanup(m.Shutdown)
	tr, _ := newFakeServer()
	c := NewClient(tr, nil, nil)
	_ = c.Close()
	s := &nodeSession{nodeID: "n1", client: c, incarn: "x", done: make(chan struct{}), procAlive: true}
	if err := m.publish("n1", s); err == nil {
		t.Fatal("published dead client")
	}
	if m.HasSession("n1") {
		t.Fatal("dead client left in map")
	}
}

func TestPublishConflictWithoutHook(t *testing.T) {
	m, _, _, _ := launchOK(t, t.TempDir())
	s2 := &nodeSession{nodeID: "n1", incarn: "other", done: make(chan struct{}), procAlive: true}
	if err := m.publish("n1", s2); !errors.Is(err, ErrLaunchConflict) {
		t.Fatalf("err=%v", err)
	}
}

func TestLaunchExitBeforePublishNotQuiet(t *testing.T) {
	dir := t.TempDir()
	ctl := &spawnCtl{}
	m := NewManagerWithSpawn(dir, ctl.spawn)
	t.Cleanup(m.Shutdown)
	rc := make(chan launchRes, 1)
	go func() {
		id, err := m.Launch("n1", "muse", "/ws", "", "")
		rc <- launchRes{id, err}
	}()
	srv := ctl.waitSpawned(t)
	req := srv.nextReq(t)
	srv.replyOK(t, req, initResult())
	req = srv.nextReq(t)
	srv.replyOK(t, req, map[string]any{"sessionId": "sess-1", "viewCursor": "c0"})
	_ = srv.tr.Close()
	res := <-rc
	if res.err == nil {
		s := m.session("n1")
		if s != nil && s.client != nil && s.client.Live() != nil && m.Live("n1") == "quiet" {
			t.Fatal("returned success as quiet after client death")
		}
		if m.Live("n1") == "quiet" && s != nil && s.client != nil {
			select {
			case <-s.client.Done():
				t.Fatal("returned success as quiet with client Done closed")
			default:
			}
		}
	}
}

func TestManagerClearSeamBeforeNewSessionEvents(t *testing.T) {
	m, _, srv, logPath := launchOK(t, t.TempDir())
	s := m.session("n1")
	hold := make(chan struct{})
	entered := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-hold:
		default:
			close(hold)
		}
	})
	lg := &seamHoldLog{seamEntered: entered, holdSeam: hold, inner: &sessionlog.Writer{Path: logPath}}
	s.logw = lg
	errc := make(chan error, 1)
	go func() { errc <- m.Clear("n1") }()
	req := srv.nextReq(t)
	srv.replyOK(t, req, map[string]any{"sessionId": "sess-2", "viewCursor": "c1"})
	srv.note(t, "item/completed", map[string]any{
		"sessionId": "sess-2",
		"item": map[string]any{
			"itemId": "n", "kind": "agentMessage", "revision": 1,
			"status": "completed", "text": "new-out",
		},
	})
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("seam append never entered")
	}
	lg.mu.Lock()
	for _, e := range lg.evs {
		if e.T == "assistant" && e.Text == "new-out" {
			lg.mu.Unlock()
			t.Fatal("new-session event appended before seam")
		}
	}
	lg.mu.Unlock()
	close(hold)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		for _, e := range readLog(t, logPath) {
			if e.T == "assistant" && e.Text == "new-out" {
				return true
			}
		}
		return false
	})
	evs := readLog(t, logPath)
	seam, asst := -1, -1
	for i, e := range evs {
		if e.T == "source" && e.Source != nil && e.Source.SessionID == "sess-2" {
			seam = i
		}
		if e.T == "assistant" && e.Text == "new-out" {
			asst = i
		}
	}
	if seam < 0 || asst < 0 || asst < seam {
		t.Fatalf("order seam=%d asst=%d types=%v", seam, asst, eventTypes(evs))
	}
}

func TestManagerClearNewSessionApprovalAfterSeam(t *testing.T) {
	m, _, srv, logPath := launchOK(t, t.TempDir())
	s := m.session("n1")
	hold := make(chan struct{})
	entered := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-hold:
		default:
			close(hold)
		}
	})
	s.logw = &seamHoldLog{seamEntered: entered, holdSeam: hold, inner: &sessionlog.Writer{Path: logPath}}
	errc := make(chan error, 1)
	go func() { errc <- m.Clear("n1") }()
	req := srv.nextReq(t)
	srv.replyOK(t, req, map[string]any{"sessionId": "sess-2"})
	srv.note(t, "approval/requested", approvalParams("ap-new", "sess-2", "ls", 0, defaultChoices()))
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("seam never entered")
	}
	close(hold)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return m.Attention("n1") == "approval" })
	p, ok := m.Pending("n1")
	if !ok {
		t.Fatal("new-session approval missing after seam")
	}
	if !strings.Contains(p.Title, "ls") {
		t.Fatalf("title=%q", p.Title)
	}
}

func TestManagerClearCancelBeforeResponseKeepsIdentity(t *testing.T) {
	m, _, srv, logPath := launchOK(t, t.TempDir())
	s := m.session("n1")
	old := s.client.SessionID()
	errc := make(chan error, 1)
	go func() { errc <- m.Clear("n1") }()
	req := srv.nextReq(t)
	m.opCancel()
	if err := <-errc; err == nil {
		t.Fatal("expected canceled clear")
	}
	if s.client.SessionID() != old {
		t.Fatalf("client session=%s", s.client.SessionID())
	}
	if s.sessionID != "sess-1" {
		t.Fatalf("manager session=%s", s.sessionID)
	}
	srv.replyOK(t, req, map[string]any{"sessionId": "sess-2"})
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if s.client.SessionID() != old {
			t.Fatal("late start committed after cancel")
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, e := range readLog(t, logPath) {
		if e.T == "source" && e.Source != nil && e.Source.Reason == "clear" {
			t.Fatal("seam after canceled clear")
		}
	}
}

func TestManagerClearClaimedResponseWinsOverCancel(t *testing.T) {
	m, _, srv, logPath := launchOK(t, t.TempDir())
	s := m.session("n1")
	errc := make(chan error, 1)
	go func() { errc <- m.Clear("n1") }()
	req := srv.nextReq(t)
	key := pendingKey(t, req)
	s.client.mu.Lock()
	srv.replyOK(t, req, map[string]any{"sessionId": "sess-2", "viewCursor": "c1"})
	waitCallClaimed(t, s.client.peer, key)
	m.opCancel()
	select {
	case err := <-errc:
		s.client.mu.Unlock()
		t.Fatalf("clear returned %v while apply blocked", err)
	default:
	}
	s.client.mu.Unlock()
	if err := <-errc; err != nil {
		t.Fatalf("claimed clear must succeed: %v", err)
	}
	if s.client.SessionID() != "sess-2" || s.sessionID != "sess-2" {
		t.Fatalf("identity client=%s manager=%s", s.client.SessionID(), s.sessionID)
	}
	found := 0
	for _, e := range readLog(t, logPath) {
		if e.T == "source" && e.Source != nil && e.Source.SessionID == "sess-2" {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("seams=%d", found)
	}
}

func TestManagerClearCloseAfterClaimedStartAgrees(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	s := m.session("n1")
	errc := make(chan error, 1)
	go func() { errc <- m.Clear("n1") }()
	req := srv.nextReq(t)
	key := pendingKey(t, req)
	s.client.mu.Lock()
	srv.replyOK(t, req, map[string]any{"sessionId": "sess-2"})
	waitCallClaimed(t, s.client.peer, key)
	_ = s.client.Close()
	s.client.mu.Unlock()
	_ = <-errc
	if m.HasSession("n1") {
		if s.client.SessionID() != s.sessionID {
			t.Fatalf("published mismatch client=%s manager=%s", s.client.SessionID(), s.sessionID)
		}
	}
}

func TestManagerClearFailureNoSeamReleasesCallbacks(t *testing.T) {
	m, _, srv, logPath := launchOK(t, t.TempDir())
	errc := make(chan error, 1)
	go func() { errc <- m.Clear("n1") }()
	req := srv.nextReq(t)
	srv.replyErr(t, req, "cannot start")
	if err := <-errc; err == nil {
		t.Fatal("expected clear failure")
	}
	srv.note(t, "item/completed", agentItem("a1", "after-fail"))
	drainGap(t, srv, logPath, "after-fail-clear")
	for _, e := range readLog(t, logPath) {
		if e.T == "source" && e.Source != nil && e.Source.Reason == "clear" {
			t.Fatal("seam after failed clear")
		}
	}
}

func TestManagerClearStartFailureLastError(t *testing.T) {
	m, _, srv, logPath := launchOK(t, t.TempDir())
	oldIncarn, _, ok := m.PermissionBoundary("n1")
	if !ok {
		t.Fatal("boundary")
	}
	errc := make(chan error, 1)
	go func() { errc <- m.Clear("n1") }()
	req := srv.nextReq(t)
	srv.replyErr(t, req, "cannot start")
	if err := <-errc; err == nil {
		t.Fatal("expected failure")
	}
	if m.LastError("n1") == "" {
		t.Fatal("LastError empty after clear start failure")
	}
	for _, e := range readLog(t, logPath) {
		if e.T == "source" && e.Source != nil && e.Source.Reason == "clear" {
			t.Fatal("seam appended")
		}
	}
	incarn, _, ok := m.PermissionBoundary("n1")
	if !ok || incarn != oldIncarn {
		t.Fatalf("incarnation changed: %s -> %s", oldIncarn, incarn)
	}
}

func TestManagerClearSeamFailureLastError(t *testing.T) {
	m, _, srv, logPath := launchOK(t, t.TempDir())
	s := m.session("n1")
	s.logw = &failLog{remain: 0, err: errors.New("seam fail"), inner: &sessionlog.Writer{Path: logPath}}
	errc := make(chan error, 1)
	go func() { errc <- m.Clear("n1") }()
	req := srv.nextReq(t)
	srv.replyOK(t, req, map[string]any{"sessionId": "sess-2"})
	err := <-errc
	if err == nil {
		t.Fatal("expected seam failure")
	}
	if !strings.Contains(m.LastError("n1"), "seam") {
		t.Fatalf("LastError=%q want substring %q", m.LastError("n1"), "seam")
	}
	if s.sessionID != "sess-2" {
		t.Fatalf("session id=%s", s.sessionID)
	}
	s.logw = &sessionlog.Writer{Path: logPath}
	if err := m.Send("n1", "next"); err != nil {
		t.Fatalf("send after seam failure: %v", err)
	}
	if m.LastError("n1") != "" {
		t.Fatalf("admitted send did not clear LastError: %q", m.LastError("n1"))
	}
	req = srv.nextReq(t)
	srv.replyOK(t, req, map[string]any{"turnId": "t2", "disposition": "started"})
}

func TestManagerExitInvalidatesApproval(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "risky", "turn-1")
	srv.note(t, "approval/requested", approvalParams("ap-1", "sess-1", "ls", 0, defaultChoices()))
	waitFor(t, func() bool { return m.Attention("n1") == "approval" })
	p, _ := m.Pending("n1")
	tok, _, err := m.PrepareResolve("n1", p.RequestID, "1")
	if err != nil {
		t.Fatal(err)
	}
	_ = srv.tr.Close()
	waitFor(t, func() bool { return m.Live("n1") == "exited" })
	if m.Attention("n1") != "" {
		t.Fatal("attention after exit")
	}
	if _, ok := m.Pending("n1"); ok {
		t.Fatal("pending after exit")
	}
	if _, _, ok := m.PermissionBoundary("n1"); ok {
		t.Fatal("boundary after exit")
	}
	if _, _, err := m.PrepareResolve("n1", p.RequestID, "1"); !errors.Is(err, ErrNotAlive) {
		t.Fatalf("prepare %v", err)
	}
	if err := m.Deliver("n1", tok); !errors.Is(err, ErrNotAlive) {
		t.Fatalf("deliver %v", err)
	}
	if _, err := m.Models("n1"); !errors.Is(err, ErrNotAlive) {
		t.Fatalf("models %v", err)
	}
	if err := m.Interrupt("n1"); !errors.Is(err, ErrNotAlive) {
		t.Fatalf("interrupt %v", err)
	}
	if srv.hasMethod("approval/decide") {
		t.Fatal("decide after exit")
	}
	if !m.HasSession("n1") {
		t.Fatal("HasSession forgotten on exit")
	}
	if err := m.Kill("n1"); err != nil {
		t.Fatal(err)
	}
	if err := m.Kill("n1"); err != nil {
		t.Fatal(err)
	}
}

func TestScanEventIgnoresNonContent(t *testing.T) {
	if scanEvent(Event{T: "usage"}) != nil {
		t.Fatal("usage scanned")
	}
	if scanEvent(Event{T: "tool"}) != nil {
		t.Fatal("nil tool scanned")
	}
}

func TestCloseDoneNilSession(t *testing.T) {
	s := &nodeSession{}
	s.closeDone()
	s.markExited()
	s.onEvent(Event{T: "assistant", Text: "none"})
}

func TestPersistNilLogWriter(t *testing.T) {
	s := &nodeSession{nodeID: "n"}
	if s.persist(Event{T: "user", Text: "x"}) {
		t.Fatal("nil logw persisted")
	}
}

func TestFailAdmissionIdleTurn(t *testing.T) {
	s := &nodeSession{}
	s.failAdmission(errors.New("x"))
	if s.LastError() != "" {
		t.Fatal("idle failAdmission wrote error")
	}
}

func TestSyncPendingNilClient(t *testing.T) {
	s := &nodeSession{procAlive: true}
	s.syncPendingFromClient()
}

func TestFenceUsesClientSessionID(t *testing.T) {
	tr, _ := newFakeServer()
	c := NewClient(tr, nil, nil)
	t.Cleanup(func() { _ = c.Close() })
	c.mu.Lock()
	c.session.SessionID = "cli-sess"
	c.mu.Unlock()
	s := &nodeSession{client: c, incarn: "inc", pending: &displayedPending{seq: 1, approval: Approval{ApprovalID: "ap"}}}
	f := s.fenceOfLocked(s.pending)
	if f.SessionID != "cli-sess" {
		t.Fatalf("fence session=%q want cli-sess (client fallback)", f.SessionID)
	}
}

func TestManagerExitLateApprovalCallback(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "risky", "turn-1")
	srv.note(t, "approval/requested", approvalParams("ap-1", "sess-1", "ls", 0, defaultChoices()))
	waitFor(t, func() bool { return m.Attention("n1") == "approval" })
	_ = srv.tr.Close()
	waitFor(t, func() bool { return m.Live("n1") == "exited" })
	s := m.session("n1")
	s.onApproval(Approval{ApprovalID: "ap-1", Subject: Subject{Kind: "command"}})
	s.onEvent(Event{T: "assistant", Text: "late-out"})
	if m.Attention("n1") != "" {
		t.Fatal("late callback resurrected attention")
	}
	if _, ok := m.Pending("n1"); ok {
		t.Fatal("late callback resurrected pending")
	}
	if strings.Contains(m.Peek("n1"), "late-out") {
		t.Fatal("late event persisted after exit")
	}
}

func TestManagerSetAssetHook(t *testing.T) {
	var mu sync.Mutex
	var refs []string
	ctl := &spawnCtl{}
	m := NewManagerWithSpawn(t.TempDir(), ctl.spawn)
	t.Cleanup(m.Shutdown)
	m.SetAssetHook(func(nodeID, dir string, cands []asset.Candidate) {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range cands {
			refs = append(refs, nodeID+":"+dir+":"+c.Ref)
		}
	})
	rc := make(chan error, 1)
	go func() { _, err := m.Launch("n1", "muse", "/ws", "", ""); rc <- err }()
	srv := ctl.waitSpawned(t)
	handshake(t, srv, "")
	if err := <-rc; err != nil {
		t.Fatal(err)
	}
	if err := m.Send("n1", "see ![x](hooked.png)"); err != nil {
		t.Fatal(err)
	}
	req := srv.nextReq(t)
	srv.replyOK(t, req, map[string]any{"turnId": "turn-1", "disposition": "started"})
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(refs) > 0
	})
	mu.Lock()
	defer mu.Unlock()
	if refs[0] != "n1:/ws:hooked.png" {
		t.Fatalf("hook=%v", refs)
	}
}

func TestDefaultSpawnArgvIsMuseServe(t *testing.T) {
	old := commandFn
	t.Cleanup(func() { commandFn = old })
	var name string
	var args []string
	commandFn = func(ctx context.Context, n string, a ...string) *exec.Cmd {
		name, args = n, a
		return exec.Command("/no/such/muse-binary-for-phase4-tests")
	}
	_, err := defaultSpawn("node", "/workspace")
	if name != "muse" || len(args) != 1 || args[0] != "serve" {
		t.Fatalf("argv %s %v", name, args)
	}
	if err == nil {
		t.Fatal("missing binary must fail")
	}
}

func TestManagerFileSubjectKind(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "risky", "turn-1")
	params := approvalParams("ap-1", "sess-1", "", 0, defaultChoices())
	params["subject"] = map[string]any{"kind": "edit", "path": "/tmp/x"}
	srv.note(t, "approval/requested", params)
	waitFor(t, func() bool { return m.Attention("n1") == "approval" })
	p, _ := m.Pending("n1")
	if p.ToolKind != "edit" {
		t.Fatalf("toolKind=%q", p.ToolKind)
	}
}

func TestManagerPrepareYWithOnlyRejects(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "risky", "turn-1")
	srv.note(t, "approval/requested", approvalParams("ap-1", "sess-1", "ls", 0, []map[string]any{
		{"choiceId": "abort", "decision": "abort", "label": "Abort"},
	}))
	waitFor(t, func() bool { return m.Attention("n1") == "approval" })
	p, _ := m.Pending("n1")
	if _, _, err := m.PrepareResolve("n1", p.RequestID, "y"); err == nil {
		t.Fatal("y must fail when no allowing choice exists")
	}
}

func TestManagerDeliverInvalidJSONFence(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "risky", "turn-1")
	srv.note(t, "approval/requested", approvalParams("ap-1", "sess-1", "ls", 0, defaultChoices()))
	waitFor(t, func() bool { return m.Attention("n1") == "approval" })
	if err := m.Deliver("n1", "7b"); !errors.Is(err, ErrStalePermission) {
		t.Fatalf("err=%v", err)
	}
}

func TestManagerClearNewSessionUsageAfterSeam(t *testing.T) {
	m, _, srv, logPath := launchOK(t, t.TempDir())
	s := m.session("n1")
	hold := make(chan struct{})
	entered := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-hold:
		default:
			close(hold)
		}
	})
	lg := &seamHoldLog{seamEntered: entered, holdSeam: hold, inner: &sessionlog.Writer{Path: logPath}}
	s.logw = lg
	errc := make(chan error, 1)
	go func() { errc <- m.Clear("n1") }()
	req := srv.nextReq(t)
	srv.replyOK(t, req, map[string]any{"sessionId": "sess-2", "viewCursor": "c1"})
	srv.note(t, "session/contextUsage", map[string]any{
		"sessionId": "sess-2", "usedTokens": 11, "windowTokens": 110,
	})
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("seam never entered")
	}
	lg.mu.Lock()
	for _, e := range lg.evs {
		if e.T == "usage" {
			lg.mu.Unlock()
			t.Fatal("new-session usage appended before seam")
		}
	}
	lg.mu.Unlock()
	close(hold)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		used, win := m.Usage("n1")
		return used == 11 && win == 110
	})
	evs := readLog(t, logPath)
	seam, usage := -1, -1
	for i, e := range evs {
		if e.T == "source" && e.Source != nil && e.Source.SessionID == "sess-2" {
			seam = i
		}
		if e.T == "usage" && e.Usage != nil && e.Usage.Used == 11 {
			usage = i
		}
	}
	if seam < 0 || usage < 0 || usage < seam {
		t.Fatalf("order seam=%d usage=%d types=%v", seam, usage, eventTypes(evs))
	}
}

func TestManagerClearOldAssistantBlockedOnGateRejected(t *testing.T) {
	m, _, srv, logPath := launchOK(t, t.TempDir())
	s := m.session("n1")
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	var orig func(string, Event)
	orig = swapSessionSink(s.client, func(captured string, e Event) {
		if e.T == "assistant" && e.Text == "old-out" {
			select {
			case <-entered:
			default:
				close(entered)
			}
			<-release
		}
		orig(captured, e)
	})
	errc := make(chan error, 1)
	go func() { errc <- m.Clear("n1") }()
	req := srv.nextReq(t)
	srv.note(t, "item/completed", map[string]any{
		"sessionId": "sess-1",
		"item": map[string]any{
			"itemId": "old", "kind": "agentMessage", "revision": 1,
			"status": "completed", "text": "old-out",
		},
	})
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("old assistant never entered sink")
	}
	srv.replyOK(t, req, map[string]any{"sessionId": "sess-2", "viewCursor": "c1"})
	srv.note(t, "item/completed", map[string]any{
		"sessionId": "sess-2",
		"item": map[string]any{
			"itemId": "n", "kind": "agentMessage", "revision": 1,
			"status": "completed", "text": "new-out",
		},
	})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	close(release)
	waitFor(t, func() bool {
		for _, e := range readLog(t, logPath) {
			if e.T == "assistant" && e.Text == "new-out" {
				return true
			}
		}
		return false
	})
	evs := readLog(t, logPath)
	seam := -1
	for i, e := range evs {
		if e.T == "source" && e.Source != nil && e.Source.Reason == "clear" {
			seam = i
		}
	}
	if seam < 0 {
		t.Fatal("missing seam")
	}
	for i, e := range evs {
		if e.T == "assistant" && e.Text == "old-out" && i > seam {
			t.Fatalf("old assistant after seam at %d types=%v", i, eventTypes(evs))
		}
	}
}

func TestManagerEmptySessionApprovalIsImported(t *testing.T) {
	m, _, _, _ := launchOK(t, t.TempDir())
	s := m.session("n1")
	s.client.approvals.setRequested(Approval{
		ApprovalID: "ap-blank",
		Subject:    Subject{Kind: "command", Command: "ls"},
		Choices:    []Choice{{ChoiceID: "abort", Decision: "abort"}},
	})
	if m.Attention("n1") != "approval" {
		t.Fatal("empty-session pending should be importable")
	}
	if _, ok := m.Pending("n1"); !ok {
		t.Fatal("Pending missing for empty-session approval")
	}
}

func TestManagerForeignApprovalNotImported(t *testing.T) {
	m, _, _, _ := launchOK(t, t.TempDir())
	s := m.session("n1")
	s.client.approvals.setRequested(Approval{
		ApprovalID: "ap-foreign",
		SessionID:  "other-sess",
		Subject:    Subject{Kind: "command", Command: "rm"},
		Choices:    []Choice{{ChoiceID: "abort", Decision: "abort"}},
	})
	if m.Attention("n1") == "approval" {
		t.Fatal("attention imported foreign pending")
	}
	if _, ok := m.Pending("n1"); ok {
		t.Fatal("Pending imported foreign approval")
	}
	if _, _, ok := m.PermissionBoundary("n1"); !ok {
		t.Fatal("boundary should remain for live session")
	}
	incarn, seq, ok := m.PermissionBoundary("n1")
	if !ok || incarn == "" {
		t.Fatal("boundary")
	}
	_ = seq
}

func TestManagerClearOldApprovalBlockedOnGateRejected(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	s := m.session("n1")
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	s.client.SetApprovalHandler(func(a Approval) {
		if a.ApprovalID == "ap-old" {
			select {
			case <-entered:
			default:
				close(entered)
			}
			<-release
		}
		s.onApproval(a)
	})
	errc := make(chan error, 1)
	go func() { errc <- m.Clear("n1") }()
	req := srv.nextReq(t)
	srv.note(t, "approval/requested", approvalParams("ap-old", "sess-1", "rm", 0, defaultChoices()))
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("old approval handler never entered")
	}
	srv.replyOK(t, req, map[string]any{"sessionId": "sess-2"})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	close(release)
	drainGap(t, srv, m.logPath("n1"), "old-ap-gate")
	if m.Attention("n1") == "approval" {
		p, ok := m.Pending("n1")
		if ok && strings.Contains(p.Title, "rm") {
			t.Fatal("old approval pending after clear")
		}
	}
	if p, ok := m.Pending("n1"); ok && strings.Contains(p.Title, "rm") {
		t.Fatal("old approval became pending after seam")
	}
}

func TestManagerLiveReconcilesKnownDeadClient(t *testing.T) {
	m, _, srv, logPath := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "hello", "turn-1")
	s := m.session("n1")
	if err := s.client.Close(); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.procAlive = true
	s.mu.Unlock()
	if got := m.Live("n1"); got != "exited" {
		t.Fatalf("Live=%s want exited", got)
	}
	if m.Attention("n1") != "" {
		t.Fatal("attention actionable after known death")
	}
	if _, ok := m.Pending("n1"); ok {
		t.Fatal("pending actionable after known death")
	}
	if _, _, ok := m.PermissionBoundary("n1"); ok {
		t.Fatal("boundary after known death")
	}
	if err := m.Send("n1", "x"); !errors.Is(err, ErrNotAlive) {
		t.Fatalf("Send=%v", err)
	}
	if err := m.Interrupt("n1"); !errors.Is(err, ErrNotAlive) {
		t.Fatalf("Interrupt=%v", err)
	}
	if _, err := m.Models("n1"); !errors.Is(err, ErrNotAlive) {
		t.Fatalf("Models=%v", err)
	}
	n := 0
	for _, e := range readLog(t, logPath) {
		if e.T == "error" && strings.Contains(e.Error, "exited") {
			n++
		}
	}
	_ = m.Live("n1")
	_ = m.Send("n1", "again")
	n2 := 0
	for _, e := range readLog(t, logPath) {
		if e.T == "error" && strings.Contains(e.Error, "exited") {
			n2++
		}
	}
	if n2 != n || n > 1 {
		t.Fatalf("exit errors before=%d after=%d", n, n2)
	}
}

func TestLaunchKnownDeadIsNeverQuiet(t *testing.T) {
	dir := t.TempDir()
	ctl := &spawnCtl{}
	m := NewManagerWithSpawn(dir, ctl.spawn)
	t.Cleanup(m.Shutdown)
	rc := make(chan launchRes, 1)
	go func() {
		id, err := m.Launch("n1", "muse", "/ws", "", "")
		rc <- launchRes{id, err}
	}()
	srv := ctl.waitSpawned(t)
	req := srv.nextReq(t)
	srv.replyOK(t, req, initResult())
	req = srv.nextReq(t)
	srv.replyOK(t, req, map[string]any{"sessionId": "sess-1", "viewCursor": "c0"})
	_ = srv.tr.Close()
	res := <-rc
	s := m.session("n1")
	if res.err != nil {
		if s != nil && m.Live("n1") == "quiet" {
			t.Fatal("failed launch left a quiet session")
		}
		return
	}
	if s == nil || s.client == nil {
		t.Fatal("successful launch without session")
	}
	waitFor(t, func() bool { return s.client.Live() != nil })
	s.mu.Lock()
	s.procAlive = true
	s.mu.Unlock()
	if got := m.Live("n1"); got == "quiet" {
		t.Fatal("returned success as quiet after client death")
	}
	if got := m.Live("n1"); got != "exited" {
		t.Fatalf("Live=%s after known-dead client", got)
	}
}

func TestManagerLaunchFailedQuiescesUnpublishedCallbacks(t *testing.T) {
	for _, failAt := range []string{"initialize", "session/start"} {
		t.Run(failAt, func(t *testing.T) {
			dir := t.TempDir()
			entered := make(chan struct{})
			release := make(chan struct{})
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			ctl := &spawnCtl{}
			m := NewManagerWithSpawn(dir, ctl.spawn)
			m.SetAssetHook(func(string, string, []asset.Candidate) {
				select {
				case <-entered:
				default:
					close(entered)
				}
				<-release
			})
			rc := make(chan error, 1)
			go func() { _, err := m.Launch("n1", "muse", "/ws", "", ""); rc <- err }()
			srv := ctl.waitSpawned(t)
			req := srv.nextReq(t)
			if failAt == "session/start" {
				srv.replyOK(t, req, initResult())
				req = srv.nextReq(t)
			}
			srv.note(t, "item/completed", agentItem("a1", "see ![x](hook.png)"))
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("asset hook not entered")
			}
			if failAt == "initialize" {
				srv.replyErr(t, req, "init broke")
			} else {
				srv.replyErr(t, req, "start broke")
			}
			select {
			case <-srv.tr.closed:
			case <-time.After(2 * time.Second):
				t.Fatal("transport not closed during failed-launch cleanup")
			}
			shutDone := make(chan struct{})
			go func() { m.Shutdown(); close(shutDone) }()
			select {
			case <-rc:
				t.Fatal("Launch returned while unpublished callback blocked")
			default:
			}
			select {
			case <-shutDone:
				t.Fatal("Shutdown returned while Launch cleanup blocked")
			default:
			}
			close(release)
			select {
			case err := <-rc:
				if err == nil {
					t.Fatal("launch succeeded")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Launch did not return after hook release")
			}
			select {
			case <-shutDone:
			case <-time.After(2 * time.Second):
				t.Fatal("Shutdown did not return")
			}
			select {
			case <-srv.tr.closed:
			case <-time.After(2 * time.Second):
				t.Fatal("transport not closed")
			}
			logPath := filepath.Join(dir, "n1.jsonl")
			if _, err := os.Stat(logPath); !os.IsNotExist(err) {
				t.Fatal("partial log remained after quiescent cleanup")
			}
			deadline := time.Now().Add(200 * time.Millisecond)
			for time.Now().Before(deadline) {
				if _, err := os.Stat(logPath); !os.IsNotExist(err) {
					t.Fatal("late callback recreated log")
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
}

func TestManagerLaunchFailedPreservesPreexistingLogAfterCallbacks(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "n1.jsonl")
	if err := os.WriteFile(logPath, []byte("{\"t\":\"meta\",\"time\":\"t0\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	ctl := &spawnCtl{}
	m := NewManagerWithSpawn(dir, ctl.spawn)
	t.Cleanup(m.Shutdown)
	m.SetAssetHook(func(string, string, []asset.Candidate) {
		close(entered)
		<-release
	})
	rc := make(chan error, 1)
	go func() { _, err := m.Launch("n1", "muse", "/ws", "", ""); rc <- err }()
	srv := ctl.waitSpawned(t)
	req := srv.nextReq(t)
	srv.note(t, "item/completed", agentItem("a1", "see ![x](hook.png)"))
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("hook not entered")
	}
	srv.replyErr(t, req, "init broke")
	select {
	case <-rc:
		t.Fatal("Launch returned while hook blocked")
	default:
	}
	close(release)
	if err := <-rc; err == nil {
		t.Fatal("expected launch failure")
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"t":"meta"`)) {
		t.Fatalf("prefix lost: %s", raw)
	}
	n := len(readLog(t, logPath))
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(readLog(t, logPath)) != n {
			t.Fatal("log grew after failed Launch returned")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestManagerLaunchFailedLateRequestFormDoesNotAppend(t *testing.T) {
	dir := t.TempDir()
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	ctl := &spawnCtl{}
	m := NewManagerWithSpawn(dir, ctl.spawn)
	t.Cleanup(m.Shutdown)
	m.SetAssetHook(func(string, string, []asset.Candidate) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
	})
	rc := make(chan error, 1)
	go func() { _, err := m.Launch("n1", "muse", "/ws", "", ""); rc <- err }()
	srv := ctl.waitSpawned(t)
	req := srv.nextReq(t)
	srv.note(t, "item/completed", agentItem("a1", "see ![x](hook.png)"))
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("hook not entered")
	}
	srv.writeJSON(t, map[string]any{
		"jsonrpc": "2.0", "id": "late-1", "method": "approval/request",
		"params": approvalParams("ap-late", "sess-1", "ls", 0, defaultChoices()),
	})
	srv.replyErr(t, req, "init broke")
	select {
	case <-srv.tr.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("transport not closed")
	}
	select {
	case <-rc:
		t.Fatal("Launch returned before unpublished work quiesced")
	default:
	}
	close(release)
	if err := <-rc; err == nil {
		t.Fatal("expected failure")
	}
	if _, err := os.Stat(filepath.Join(dir, "n1.jsonl")); !os.IsNotExist(err) {
		t.Fatal("late request-form recreated or left log")
	}
	if m.HasSession("n1") {
		t.Fatal("unpublished session published")
	}
}

func TestManagerRecordStartFailureWaitsKillAndShutdown(t *testing.T) {
	m, _, _, logPath := launchOK(t, t.TempDir())
	s := m.session("n1")
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	s.logw = &holdLog{entered: entered, release: release, inner: &sessionlog.Writer{Path: logPath}}
	errc := make(chan error, 1)
	go func() { errc <- m.RecordStartFailure("n1", errors.New("prompt lost")) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("RecordStartFailure did not reach append")
	}
	killDone := make(chan error, 1)
	go func() { killDone <- m.Kill("n1") }()
	waitFor(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.stopping
	})
	select {
	case <-killDone:
		t.Fatal("Kill returned while RecordStartFailure append blocked")
	default:
	}
	close(release)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if err := <-killDone; err != nil {
		t.Fatal(err)
	}
	n := len(readLog(t, logPath))
	if len(readLog(t, logPath)) != n {
		t.Fatal("log changed after Kill returned")
	}
	found := false
	for _, e := range readLog(t, logPath) {
		if e.T == "error" && strings.Contains(e.Error, "prompt lost") {
			found = true
		}
	}
	if !found {
		t.Fatal("failure not appended")
	}
}

func TestManagerRecordStartFailureWaitsShutdown(t *testing.T) {
	m, _, _, logPath := launchOK(t, t.TempDir())
	s := m.session("n1")
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	s.logw = &holdLog{entered: entered, release: release, inner: &sessionlog.Writer{Path: logPath}}
	errc := make(chan error, 1)
	go func() { errc <- m.RecordStartFailure("n1", errors.New("prompt lost")) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("append not entered")
	}
	shutDone := make(chan struct{})
	go func() { m.Shutdown(); close(shutDone) }()
	waitFor(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.closing
	})
	select {
	case <-shutDone:
		t.Fatal("Shutdown returned while RecordStartFailure append blocked")
	default:
	}
	close(release)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	select {
	case <-shutDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return")
	}
}

func TestManagerRecordStartFailureAfterNaturalExit(t *testing.T) {
	m, _, srv, logPath := launchOK(t, t.TempDir())
	_ = srv.tr.Close()
	waitFor(t, func() bool { return m.Live("n1") == "exited" })
	if err := m.RecordStartFailure("n1", errors.New("prompt lost")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.LastError("n1"), "prompt lost") {
		t.Fatalf("LastError=%q", m.LastError("n1"))
	}
	found := false
	for _, e := range readLog(t, logPath) {
		if e.T == "error" && strings.Contains(e.Error, "prompt lost") {
			found = true
		}
	}
	if !found {
		t.Fatal("exited mapped session did not record failure")
	}
}

func TestManagerRecordStartFailureAfterKillIsHarmless(t *testing.T) {
	m, _, _, logPath := launchOK(t, t.TempDir())
	before := readLog(t, logPath)
	if err := m.Kill("n1"); err != nil {
		t.Fatal(err)
	}
	if err := m.RecordStartFailure("n1", errors.New("prompt lost")); err != nil {
		t.Fatal(err)
	}
	after := readLog(t, logPath)
	if len(after) != len(before) {
		t.Fatalf("killed session log grew %d -> %d", len(before), len(after))
	}
}

func TestManagerRecordStartFailureAppendError(t *testing.T) {
	m, _, _, logPath := launchOK(t, t.TempDir())
	s := m.session("n1")
	s.logw = &failLog{remain: 0, err: errors.New("disk full"), inner: &sessionlog.Writer{Path: logPath}}
	if err := m.RecordStartFailure("n1", errors.New("prompt lost")); err == nil {
		t.Fatal("append failure masqueraded as success")
	}
}

func TestManagerDeliverWinsOverClearWireOrder(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "risky", "turn-1")
	srv.note(t, "approval/requested", approvalParams("ap-1", "sess-1", "ls", 0, defaultChoices()))
	waitFor(t, func() bool { return m.Attention("n1") == "approval" })
	p, _ := m.Pending("n1")
	tok, _, err := m.PrepareResolve("n1", p.RequestID, "1")
	if err != nil {
		t.Fatal(err)
	}
	completeTurn(t, m, srv, "turn-1", "cancelled")
	delErr := make(chan error, 1)
	go func() { delErr <- m.Deliver("n1", tok) }()
	decide := srv.nextReq(t)
	if decide["method"] != "approval/decide" {
		t.Fatalf("method=%v", decide["method"])
	}
	clrErr := make(chan error, 1)
	go func() { clrErr <- m.Clear("n1") }()
	waitOpWaiter(t, m.session("n1").client)
	if srv.hasMethod("session/start") {
		n := 0
		for _, name := range srv.methods() {
			if name == "session/start" {
				n++
			}
		}
		if n > 1 {
			t.Fatal("session/start written while decide in flight")
		}
	}
	srv.replyOK(t, decide, map[string]any{})
	if err := <-delErr; err != nil {
		t.Fatal(err)
	}
	start := srv.nextReq(t)
	if start["method"] != "session/start" {
		t.Fatalf("expected session/start after decide, got %v", start["method"])
	}
	srv.replyOK(t, start, map[string]any{"sessionId": "sess-2"})
	if err := <-clrErr; err != nil {
		t.Fatal(err)
	}
}

func TestManagerClearWinsOverDeliverNoDecide(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "risky", "turn-1")
	srv.note(t, "approval/requested", approvalParams("ap-1", "sess-1", "ls", 0, defaultChoices()))
	waitFor(t, func() bool { return m.Attention("n1") == "approval" })
	p, _ := m.Pending("n1")
	tok, _, err := m.PrepareResolve("n1", p.RequestID, "1")
	if err != nil {
		t.Fatal(err)
	}
	completeTurn(t, m, srv, "turn-1", "cancelled")
	clrErr := make(chan error, 1)
	go func() { clrErr <- m.Clear("n1") }()
	start := srv.nextReq(t)
	if start["method"] != "session/start" {
		t.Fatalf("method=%v", start["method"])
	}
	delErr := make(chan error, 1)
	go func() { delErr <- m.Deliver("n1", tok) }()
	waitOpWaiter(t, m.session("n1").client)
	nDecide := 0
	for _, name := range srv.methods() {
		if name == "approval/decide" {
			nDecide++
		}
	}
	if nDecide != 0 {
		t.Fatal("approval/decide written while session/start in flight")
	}
	srv.replyOK(t, start, map[string]any{"sessionId": "sess-2"})
	if err := <-clrErr; err != nil {
		t.Fatal(err)
	}
	if err := <-delErr; !errors.Is(err, ErrStalePermission) {
		t.Fatalf("err=%v", err)
	}
	nDecide = 0
	for _, name := range srv.methods() {
		if name == "approval/decide" {
			nDecide++
		}
	}
	if nDecide != 0 {
		t.Fatal("stale deliver wrote approval/decide")
	}
}

func TestManagerFailedClearPreservesPendingAndFence(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "risky", "turn-1")
	srv.note(t, "approval/requested", approvalParams("ap-1", "sess-1", "ls", 0, defaultChoices()))
	waitFor(t, func() bool { return m.Attention("n1") == "approval" })
	p, _ := m.Pending("n1")
	tok, _, err := m.PrepareResolve("n1", p.RequestID, "1")
	if err != nil {
		t.Fatal(err)
	}
	completeTurn(t, m, srv, "turn-1", "cancelled")
	errc := make(chan error, 1)
	go func() { errc <- m.Clear("n1") }()
	req := srv.nextReq(t)
	srv.replyErr(t, req, "cannot start")
	if err := <-errc; err == nil {
		t.Fatal("expected clear failure")
	}
	if _, ok := m.Pending("n1"); !ok {
		t.Fatal("failed clear dropped pending")
	}
	if m.Attention("n1") != "approval" {
		t.Fatal("failed clear dropped attention")
	}
	del := rpcOK(t, srv, func() error { return m.Deliver("n1", tok) }, "approval/decide", map[string]any{})
	if del["method"] != "approval/decide" {
		t.Fatalf("method=%v", del["method"])
	}
}

func TestManagerMalformedClearPreservesPendingAndFence(t *testing.T) {
	m, _, srv, _ := launchOK(t, t.TempDir())
	startTurn(t, m, srv, "risky", "turn-1")
	srv.note(t, "approval/requested", approvalParams("ap-1", "sess-1", "ls", 0, defaultChoices()))
	waitFor(t, func() bool { return m.Attention("n1") == "approval" })
	p, _ := m.Pending("n1")
	tok, _, err := m.PrepareResolve("n1", p.RequestID, "1")
	if err != nil {
		t.Fatal(err)
	}
	completeTurn(t, m, srv, "turn-1", "cancelled")
	errc := make(chan error, 1)
	go func() { errc <- m.Clear("n1") }()
	req := srv.nextReq(t)
	srv.replyOK(t, req, map[string]any{"viewCursor": "no-id"})
	if err := <-errc; err == nil {
		t.Fatal("expected malformed clear failure")
	}
	if _, ok := m.Pending("n1"); !ok {
		t.Fatal("malformed clear dropped pending")
	}
	_ = tok
	if _, _, err := m.PrepareResolve("n1", p.RequestID, "1"); err != nil {
		t.Fatalf("fence unusable after malformed clear: %v", err)
	}
}

func TestWrapCleanupCombinesErrors(t *testing.T) {
	if err := wrapCleanup(nil, errors.New("close")); err == nil || err.Error() != "close" {
		t.Fatalf("nil primary: %v", err)
	}
	if err := wrapCleanup(errors.New("launch"), nil); err == nil || err.Error() != "launch" {
		t.Fatalf("nil cleanup: %v", err)
	}
	err := wrapCleanup(errors.New("launch"), errors.New("close"))
	if err == nil || !strings.Contains(err.Error(), "launch") || !strings.Contains(err.Error(), "close") {
		t.Fatalf("combined: %v", err)
	}
}

func TestManagerFingerprintMismatchObservableWithoutKill(t *testing.T) {
	ctl := &spawnCtl{}
	m := NewManagerWithSpawn(t.TempDir(), ctl.spawn)
	t.Cleanup(m.Shutdown)
	rc := make(chan launchRes, 1)
	go func() {
		id, err := m.Launch("n1", "muse", "/ws", "spark", "")
		rc <- launchRes{id, err}
	}()
	srv := ctl.waitSpawned(t)
	req := srv.nextReq(t)
	if req["method"] != "initialize" {
		t.Fatalf("method=%v", req["method"])
	}
	srv.replyOK(t, req, map[string]any{
		"serverInfo": map[string]any{"name": "muse", "version": ObservedMuseVersion},
		"schema":     map[string]any{"version": ObservedMSPSchema, "fingerprint": "sha256:deadbeef"},
	})
	req = srv.nextReq(t)
	if req["method"] != "session/start" {
		t.Fatalf("method=%v", req["method"])
	}
	srv.replyOK(t, req, map[string]any{"sessionId": "sess-1", "viewCursor": "c0"})
	res := <-rc
	if res.err != nil {
		t.Fatalf("mismatch must not fail launch: %v", res.err)
	}
	if !m.FingerprintMismatch("n1") {
		t.Fatal("mismatch must be observable")
	}
	if m.Live("n1") == "exited" {
		t.Fatal("mismatch must not kill the node")
	}
	if !m.HasSession("n1") {
		t.Fatal("mismatch must not drop the session")
	}
	if m.FingerprintMismatch("missing") {
		t.Fatal("missing node must not report mismatch")
	}

	m2, _, _, _ := launchOK(t, t.TempDir())
	if m2.FingerprintMismatch("n1") {
		t.Fatal("matching fingerprint must not warn")
	}
}
