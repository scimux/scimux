package muse

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type pipeTr struct {
	w    io.WriteCloser
	r    io.Reader
	once sync.Once
}

func (p *pipeTr) Stdin() io.WriteCloser { return p.w }
func (p *pipeTr) Stdout() io.Reader     { return p.r }
func (p *pipeTr) Close() error {
	p.once.Do(func() { _ = p.w.Close() })
	return nil
}

type harness struct {
	t      *testing.T
	client *Client
	mu     sync.Mutex
	events []Event
	from   *bufio.Reader
	to     *io.PipeWriter
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	h := &harness{t: t, from: bufio.NewReader(c2sR), to: s2cW}
	tr := &pipeTr{w: c2sW, r: s2cR}
	h.client = NewClient(tr, func(e Event) {
		h.mu.Lock()
		h.events = append(h.events, e)
		h.mu.Unlock()
	}, nil)
	t.Cleanup(func() {
		_ = h.client.Close()
		_ = s2cW.Close()
		_ = c2sR.Close()
	})
	return h
}

func (h *harness) snap() []Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Event, len(h.events))
	copy(out, h.events)
	return out
}

func (h *harness) readJSON() map[string]any {
	h.t.Helper()
	line, err := h.from.ReadBytes('\n')
	if err != nil {
		h.t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		h.t.Fatalf("%s: %v", line, err)
	}
	return m
}

func (h *harness) writeRaw(s string) {
	h.t.Helper()
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	if _, err := io.WriteString(h.to, s); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) writeJSON(v any) {
	h.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		h.t.Fatal(err)
	}
	h.writeRaw(string(b))
}

func (h *harness) replyOK(req map[string]any, result any) {
	h.t.Helper()
	if result == nil {
		result = map[string]any{}
	}
	h.writeJSON(map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": result})
}

func TestSessionSinkInstalledBeforePeerStart(t *testing.T) {
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	got := make(chan Event, 1)
	payload := `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"itemId":"n1","kind":"agentMessage","revision":1,"status":"completed","text":"hello"}}}` + "\n"
	go func() { _, _ = io.WriteString(s2cW, payload) }()
	tr := &pipeTr{w: c2sW, r: s2cR}
	c := newClient(tr, nil, func(_ string, e Event) { got <- e }, nil)
	t.Cleanup(func() {
		_ = c.Close()
		_ = c2sR.Close()
		_ = s2cW.Close()
	})
	select {
	case e := <-got:
		if e.Text != "hello" {
			t.Fatalf("text=%q", e.Text)
		}
		e.Text = "mutated"
	case <-time.After(2 * time.Second):
		t.Fatal("first event missed session sink")
	}
	if _, err := io.WriteString(s2cW, `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"itemId":"n2","kind":"agentMessage","revision":1,"status":"completed","text":"hello"}}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-got:
		if e.Text != "hello" {
			t.Fatalf("defensive copy failed: %q", e.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second event missing")
	}
}

func TestNewClientOrdinarySinkReceivesEvents(t *testing.T) {
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	got := make(chan Event, 1)
	payload := `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"itemId":"n1","kind":"agentMessage","revision":1,"status":"completed","text":"plain"}}}` + "\n"
	go func() { _, _ = io.WriteString(s2cW, payload) }()
	tr := &pipeTr{w: c2sW, r: s2cR}
	c := NewClient(tr, func(e Event) { got <- e }, nil)
	t.Cleanup(func() {
		_ = c.Close()
		_ = c2sR.Close()
		_ = s2cW.Close()
	})
	select {
	case e := <-got:
		if e.Text != "plain" {
			t.Fatalf("text=%q", e.Text)
		}
		e.Text = "mutated"
	case <-time.After(2 * time.Second):
		t.Fatal("ordinary sink missed first event")
	}
	if _, err := io.WriteString(s2cW, `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"itemId":"n2","kind":"agentMessage","revision":1,"status":"completed","text":"plain"}}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-got:
		if e.Text != "plain" {
			t.Fatalf("ordinary sink copy failed: %q", e.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second ordinary event missing")
	}
}

func TestInitializeHandshakeAndCapabilities(t *testing.T) {
	h := newHarness(t)
	errc := make(chan error, 1)
	go func() { errc <- h.client.Initialize(context.Background(), "scimux", "0.1") }()
	req := h.readJSON()
	if req["method"] != "initialize" {
		t.Fatalf("method=%v", req["method"])
	}
	params := req["params"].(map[string]any)
	capsBlock, _ := params["capabilities"].(map[string]any)
	if capsBlock["userInputDialogs"] != false || capsBlock["experimentalApi"] != false {
		t.Fatalf("caps flags %+v", capsBlock)
	}
	caps, _ := capsBlock["requestedCapabilities"].([]any)
	if len(caps) != 1 || caps[0] != "userShell" {
		t.Fatalf("requestedCapabilities=%v", caps)
	}
	info, _ := params["clientInfo"].(map[string]any)
	if info["name"] != "scimux" {
		t.Fatalf("clientInfo=%v", info)
	}
	if info["name"] == "museprobe" {
		t.Fatal("museprobe identity is forbidden")
	}
	h.replyOK(req, map[string]any{
		"serverInfo":   map[string]any{"name": "muse", "version": ObservedMuseVersion},
		"schema":       map[string]any{"version": ObservedMSPSchema},
		"fingerprint":  PinnedFingerprint,
		"capabilities": map[string]any{"granted": []string{"userShell"}},
	})
	note := h.readJSON()
	if note["method"] != "initialized" {
		t.Fatalf("want initialized notification, got %v", note)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if h.client.ServerFingerprint() != PinnedFingerprint {
		t.Fatalf("fp=%q", h.client.ServerFingerprint())
	}
	if h.client.FingerprintMismatch() {
		t.Fatal("matching fingerprint must not warn")
	}
}

func TestInitializeNumericSchemaAndSchemaFingerprint(t *testing.T) {
	h := newHarness(t)
	errc := make(chan error, 1)
	go func() { errc <- h.client.Initialize(context.Background(), "scimux", "1") }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{
		"serverInfo": map[string]any{"name": "muse", "version": ObservedMuseVersion},
		"schema": map[string]any{
			"version":     1,
			"fingerprint": PinnedFingerprint,
		},
	})
	note := h.readJSON()
	if note["method"] != "initialized" {
		t.Fatalf("method=%v", note["method"])
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if h.client.ServerFingerprint() != PinnedFingerprint {
		t.Fatalf("fp=%q", h.client.ServerFingerprint())
	}
	if h.client.FingerprintMismatch() {
		t.Fatal("exact schema.fingerprint must not warn")
	}
}

func TestInitializeMissingFingerprintIsMismatch(t *testing.T) {
	h := newHarness(t)
	errc := make(chan error, 1)
	go func() { errc <- h.client.Initialize(context.Background(), "scimux", "1") }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{
		"schema": map[string]any{"version": 1},
	})
	_ = h.readJSON()
	if err := <-errc; err != nil {
		t.Fatalf("missing fingerprint must not fail init: %v", err)
	}
	if h.client.ServerFingerprint() != "" {
		t.Fatalf("fp=%q", h.client.ServerFingerprint())
	}
	if !h.client.FingerprintMismatch() {
		t.Fatal("missing fingerprint must report mismatch")
	}
}

func TestInitializeMalformedSchemaSendsNoInitialized(t *testing.T) {
	h := newHarness(t)
	errc := make(chan error, 1)
	go func() { errc <- h.client.Initialize(context.Background(), "scimux", "1") }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{
		"schema": map[string]any{"version": map[string]any{"x": 1}},
	})
	if err := <-errc; err == nil {
		t.Fatal("malformed schema version must fail initialization")
	}
	_ = h.client.Close()
	line, err := h.from.ReadBytes('\n')
	if err == nil && bytes.Contains(line, []byte("initialized")) {
		t.Fatalf("initialized was sent after malformed result: %s", line)
	}
}

func TestFingerprintPrefersSchemaOverAliases(t *testing.T) {
	h := newHarness(t)
	errc := make(chan error, 1)
	go func() { errc <- h.client.Initialize(context.Background(), "scimux", "1") }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{
		"schema": map[string]any{
			"version":     1,
			"fingerprint": PinnedFingerprint,
		},
		"fingerprint": "sha256:alias-top",
		"serverInfo":  map[string]any{"fingerprint": "sha256:alias-info"},
	})
	_ = h.readJSON()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if h.client.ServerFingerprint() != PinnedFingerprint {
		t.Fatalf("schema.fingerprint lost to alias: %q", h.client.ServerFingerprint())
	}
	if h.client.FingerprintMismatch() {
		t.Fatal("schema.fingerprint is the documented location")
	}
}

func TestInitializeRejectsBadClientName(t *testing.T) {
	h := newHarness(t)
	for _, name := range []string{"museprobe", "Scimux", "sci-mux", ""} {
		if err := h.client.Initialize(context.Background(), name, "1"); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
}

func TestFingerprintMismatchDoesNotFailInit(t *testing.T) {
	h := newHarness(t)
	errc := make(chan error, 1)
	go func() { errc <- h.client.Initialize(context.Background(), "scimux", "1") }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{"fingerprint": "sha256:deadbeef", "serverInfo": map[string]any{"version": "9"}})
	_ = h.readJSON() // initialized
	if err := <-errc; err != nil {
		t.Fatalf("mismatch must not fail init: %v", err)
	}
	if !h.client.FingerprintMismatch() {
		t.Fatal("mismatch must be observable")
	}
}

func TestAbsentCapabilitiesMeanCapableDurable(t *testing.T) {
	h := newHarness(t)
	errc := make(chan error, 1)
	go func() { errc <- h.client.Initialize(context.Background(), "scimux", "1") }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{"fingerprint": PinnedFingerprint})
	_ = h.readJSON()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if !h.client.userInputCapable() || !h.client.sessionDurable() {
		t.Fatal("absent caps must mean capable/durable")
	}
}

func TestSessionStartNestedViewCursorWhenTopLevelEmpty(t *testing.T) {
	h := primed(t)
	errc := make(chan error, 1)
	go func() { errc <- h.client.StartSession(context.Background(), StartParams{}) }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{
		"session": map[string]any{"sessionId": "nested-only", "viewCursor": "from-session"},
	})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if h.client.SessionID() != "nested-only" {
		t.Fatalf("session=%s", h.client.SessionID())
	}
	if h.client.ViewCursor() != "from-session" {
		t.Fatalf("cursor=%s, want nested session.viewCursor when the top-level field is omitted", h.client.ViewCursor())
	}
}

func TestSessionStartAndClearRollback(t *testing.T) {
	h := primed(t)
	errc := make(chan error, 1)
	go func() {
		errc <- h.client.StartSession(context.Background(), StartParams{Cwd: "/tmp/ws", Model: "spark"})
	}()
	req := h.readJSON()
	if req["method"] != "session/start" {
		t.Fatalf("method=%v", req["method"])
	}
	params := req["params"].(map[string]any)
	if _, ok := params["sessionId"]; ok {
		t.Fatal("initial start must omit sessionId")
	}
	h.replyOK(req, map[string]any{"sessionId": "sess-1", "viewCursor": "c0"})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if h.client.SessionID() != "sess-1" || h.client.ViewCursor() != "c0" {
		t.Fatalf("session %s cursor %s", h.client.SessionID(), h.client.ViewCursor())
	}

	go func() { errc <- h.client.Clear(context.Background(), StartParams{Cwd: "/tmp/ws"}) }()
	req = h.readJSON()
	if req["method"] != "session/start" {
		t.Fatalf("clear method=%v", req["method"])
	}
	h.writeJSON(map[string]any{"jsonrpc": "2.0", "id": req["id"], "error": map[string]any{"code": -32603, "message": "nope"}})
	if err := <-errc; err == nil {
		t.Fatal("clear failure must surface")
	}
	if h.client.SessionID() != "sess-1" {
		t.Fatalf("failed clear dropped session: %s", h.client.SessionID())
	}

	go func() { errc <- h.client.Clear(context.Background(), StartParams{Cwd: "/tmp/other"}) }()
	req = h.readJSON()
	h.replyOK(req, map[string]any{"sessionId": "sess-2", "viewCursor": "c1"})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if h.client.SessionID() != "sess-2" {
		t.Fatalf("after clear %s", h.client.SessionID())
	}
}

func TestModelListNullsAndEmpty(t *testing.T) {
	h := primedSession(t)
	errc := make(chan error, 1)
	var models []Model
	go func() {
		var err error
		models, err = h.client.Models(context.Background())
		errc <- err
	}()
	req := h.readJSON()
	if req["method"] != "model/list" {
		t.Fatalf("method=%v", req["method"])
	}
	params := req["params"].(map[string]any)
	if params["commandId"] != nil {
		t.Fatal("model/list must not use a command id")
	}
	if params["sessionId"] != "sess-1" {
		t.Fatalf("sessionId=%v", params["sessionId"])
	}
	h.replyOK(req, map[string]any{
		"source": "bundledCatalog",
		"models": []any{
			map[string]any{"id": "spark", "isDefault": false, "contextLimit": nil, "outputLimit": nil, "releaseDate": nil},
		},
	})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "spark" || models[0].IsDefault {
		t.Fatalf("%+v", models)
	}
	if models[0].ContextLimit != nil || models[0].OutputLimit != nil {
		t.Fatal("null limits must stay unknown")
	}
	if h.client.CatalogSource() != "bundledCatalog" {
		t.Fatalf("source=%q", h.client.CatalogSource())
	}
	before := len(h.snap())
	if before != 0 {
		t.Fatalf("model/list must not create a turn event, got %d", before)
	}

	go func() {
		models, err := h.client.Models(context.Background())
		if err != nil {
			errc <- err
			return
		}
		if len(models) != 0 {
			errc <- errors.New("empty catalog must be valid")
			return
		}
		errc <- nil
	}()
	req = h.readJSON()
	h.replyOK(req, map[string]any{"source": "bundledCatalog", "models": []any{}})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func TestStartTurnShapeAndLifecycle(t *testing.T) {
	h := primedSession(t)
	errc := make(chan error, 1)
	go func() { errc <- h.client.StartTurn(context.Background(), "hello") }()
	req := h.readJSON()
	if req["method"] != "turn/start" {
		t.Fatalf("method=%v", req["method"])
	}
	p := req["params"].(map[string]any)
	if p["ifBusy"] != "queue" {
		t.Fatalf("ifBusy=%v", p["ifBusy"])
	}
	if p["sessionId"] != "sess-1" {
		t.Fatalf("sessionId=%v", p["sessionId"])
	}
	if _, ok := p["commandId"].(string); !ok {
		t.Fatalf("commandId=%v", p["commandId"])
	}
	h.replyOK(req, map[string]any{"turnId": "turn-1"})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if !h.client.TurnActive() || h.client.ActiveTurnID() != "turn-1" {
		t.Fatal("turn/start is admission, turn must stay active")
	}

	h.writeRaw(`{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"itemId":"a1","kind":"agentMessage","revision":1,"status":"completed","text":"hi"}}}`)
	waitEvent(t, h, func(e Event) bool { return e.T == "assistant" && e.Text == "hi" })
	if !h.client.TurnActive() {
		t.Fatal("assistant completion must not end the turn")
	}

	h.writeRaw(`{"jsonrpc":"2.0","method":"turn/completed","params":{"turnId":"turn-1","terminal":"completed"}}`)
	deadline := time.Now().Add(2 * time.Second)
	for h.client.TurnActive() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if h.client.TurnActive() {
		t.Fatal("only turn/completed ends the turn")
	}
}

func TestTurnAckMalformedAndMissingID(t *testing.T) {
	h := primedSession(t)
	errc := make(chan error, 1)
	go func() { errc <- h.client.StartTurn(context.Background(), "x") }()
	req := h.readJSON()
	h.writeRaw(`{"jsonrpc":"2.0","id":` + rawIDFrom(t, req) + `,"result":"not-an-object"}`)
	if err := <-errc; err == nil {
		t.Fatal("malformed ack must fail")
	}
	if h.client.TurnActive() {
		t.Fatal("malformed ack activated a turn")
	}
	go func() { errc <- h.client.StartTurn(context.Background(), "y") }()
	req = h.readJSON()
	h.replyOK(req, map[string]any{"disposition": "started"})
	if err := <-errc; err == nil {
		t.Fatal("started ack without turnId must fail")
	}
	if h.client.TurnActive() {
		t.Fatal("missing turnId invented an id")
	}
}

func TestQueuedDispositionDoesNotOverwriteActiveTurn(t *testing.T) {
	h := primedSession(t)
	errc := make(chan error, 1)
	go func() { errc <- h.client.StartTurn(context.Background(), "a") }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{"turnId": "turn-a", "disposition": "started", "startedNewTurn": true})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	go func() { errc <- h.client.StartTurn(context.Background(), "b") }()
	req = h.readJSON()
	h.replyOK(req, map[string]any{"turnId": "turn-b", "disposition": "queued", "startedNewTurn": false, "status": "queued"})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if h.client.ActiveTurnID() != "turn-a" {
		t.Fatalf("queued overwrote active: %s", h.client.ActiveTurnID())
	}
}

func TestCompletionOtherTurnDoesNotClearActive(t *testing.T) {
	h := primedSession(t)
	errc := make(chan error, 1)
	go func() { errc <- h.client.StartTurn(context.Background(), "a") }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{"turnId": "turn-b", "disposition": "started"})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	h.writeRaw(`{"jsonrpc":"2.0","method":"turn/completed","params":{"turnId":"turn-a","terminal":"failed","sessionId":"sess-1"}}`)
	time.Sleep(0)
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if !h.client.TurnActive() {
			t.Fatal("other turn completion cleared B")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if h.client.ActiveTurnID() != "turn-b" {
		t.Fatalf("active=%s", h.client.ActiveTurnID())
	}
	h.writeRaw(`{"jsonrpc":"2.0","method":"turn/completed","params":{"terminal":"failed","sessionId":"sess-1"}}`)
	deadline = time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if h.client.ActiveTurnID() != "turn-b" {
			t.Fatal("malformed terminal cleared active turn")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSessionStartCommitsBeforeLaterProtocolAndItems(t *testing.T) {
	h := primedSession(t)
	errc := make(chan error, 1)
	go func() { errc <- h.client.Clear(context.Background(), StartParams{Cwd: "/tmp"}) }()
	req := h.readJSON()
	if req["method"] != "session/start" {
		t.Fatalf("method=%v", req["method"])
	}
	id := rawIDFrom(t, req)
	payload := `{"jsonrpc":"2.0","id":` + id + `,"result":{"sessionId":"sess-2","viewCursor":"c-new"}}` + "\n" +
		`{"jsonrpc":"2.0","method":"turn/started","params":{"sessionId":"sess-2","turnId":"turn-new"}}` + "\n" +
		`{"jsonrpc":"2.0","method":"item/completed","params":{"sessionId":"sess-2","item":{"itemId":"n1","kind":"agentMessage","revision":1,"status":"completed","text":"post-start"}}}` + "\n" +
		`{"jsonrpc":"2.0","method":"session/contextUsage","params":{"sessionId":"sess-2","usedTokens":11,"windowTokens":110}}` + "\n"
	if _, err := io.WriteString(h.to, payload); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if h.client.SessionID() != "sess-2" {
		t.Fatalf("session=%s", h.client.SessionID())
	}
	waitEvent(t, h, func(e Event) bool { return e.T == "assistant" && e.Text == "post-start" })
	n := 0
	for _, e := range h.snap() {
		if e.T == "assistant" && e.Text == "post-start" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("assistant copies=%d", n)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.client.ActiveTurnID() == "turn-new" && h.client.Context().Used == 11 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("turn=%s occupancy=%+v", h.client.ActiveTurnID(), h.client.Context())
}

func TestOldSessionNotificationsIgnoredAfterClear(t *testing.T) {
	h := primedSession(t)
	errc := make(chan error, 1)
	go func() { errc <- h.client.Clear(context.Background(), StartParams{Cwd: "/tmp"}) }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{"sessionId": "sess-2", "viewCursor": "c-new"})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	before := len(h.snap())
	cur := h.client.ViewCursor()
	h.writeRaw(`{"jsonrpc":"2.0","method":"item/completed","params":{"sessionId":"sess-1","item":{"itemId":"old","kind":"agentMessage","revision":1,"status":"completed","text":"stale"}}}`)
	h.writeRaw(`{"jsonrpc":"2.0","method":"session/tokenUsage","params":{"sessionId":"sess-1","promptTokens":99,"totalTokens":99,"usage":{"outputTokens":1}}}`)
	h.writeRaw(`{"jsonrpc":"2.0","method":"approval/requested","params":{"sessionId":"sess-1","approvalId":"old-ap","subject":{"kind":"command","command":"x"},"availableChoices":[{"choiceId":"abort","decision":"abort"}]}}`)
	h.writeRaw(`{"jsonrpc":"2.0","method":"turn/completed","params":{"sessionId":"sess-1","turnId":"t-old","terminal":"failed"}}`)
	h.writeRaw(`{"jsonrpc":"2.0","params":{"sessionId":"sess-1","viewCursor":"c-old"},"method":"item/delta"}`)
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if h.client.ViewCursor() != cur {
		t.Fatalf("cursor moved to %s", h.client.ViewCursor())
	}
	if h.client.Spend().TotalTokens == 99 {
		t.Fatal("old-session spend applied")
	}
	if _, ok := h.client.PendingApproval(); ok {
		t.Fatal("old-session approval applied")
	}
	for _, e := range h.snap()[before:] {
		if e.T == "assistant" && e.Text == "stale" {
			t.Fatal("old-session chat emitted")
		}
	}
}

func TestSessionStartMissingIDLeavesState(t *testing.T) {
	h := primedSession(t)
	old := h.client.SessionID()
	errc := make(chan error, 1)
	go func() { errc <- h.client.StartSession(context.Background(), StartParams{Cwd: "/x"}) }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{"viewCursor": "no-id"})
	if err := <-errc; err == nil {
		t.Fatal("missing sessionId must be malformed")
	}
	if h.client.SessionID() != old {
		t.Fatalf("state changed to %s", h.client.SessionID())
	}
}

func TestTerminalBeforeAckDoesNotReactivate(t *testing.T) {
	h := primedSession(t)
	errc := make(chan error, 1)
	go func() { errc <- h.client.StartTurn(context.Background(), "hi") }()
	req := h.readJSON()
	h.writeRaw(`{"jsonrpc":"2.0","method":"turn/completed","params":{"sessionId":"sess-1","turnId":"turn-race","terminal":"failed"}}`)
	h.replyOK(req, map[string]any{"turnId": "turn-race", "disposition": "started", "startedNewTurn": true})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if h.client.TurnActive() {
		t.Fatal("ack reactivated an already completed turn")
	}
	n := 0
	h.writeRaw(`{"jsonrpc":"2.0","method":"turn/completed","params":{"sessionId":"sess-1","turnId":"turn-race","terminal":"failed"}}`)
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	for _, e := range h.snap() {
		if e.T == "error" {
			n++
		}
	}
	if n > 1 {
		t.Fatalf("duplicate terminal records: %d", n)
	}
}

func TestMatchingTerminalStillWorks(t *testing.T) {
	h := primedSession(t)
	errc := make(chan error, 1)
	go func() { errc <- h.client.StartTurn(context.Background(), "hi") }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{"turnId": "turn-ok", "disposition": "started"})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	h.writeRaw(`{"jsonrpc":"2.0","method":"turn/completed","params":{"sessionId":"sess-1","turnId":"turn-ok","terminal":"cancelled"}}`)
	deadline := time.Now().Add(2 * time.Second)
	for h.client.TurnActive() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if h.client.TurnActive() {
		t.Fatal("matching terminal did not end turn")
	}
	waitEvent(t, h, func(e Event) bool { return e.T == "stop" })
}

func rawIDFrom(t *testing.T, req map[string]any) string {
	t.Helper()
	b, err := json.Marshal(req["id"])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInterruptWithoutTurnFailsLocally(t *testing.T) {
	h := primedSession(t)
	if err := h.client.Interrupt(context.Background()); err == nil {
		t.Fatal("interrupt with no turn must fail locally")
	}
}

func TestInterruptSendsCurrentTurn(t *testing.T) {
	h := primedSession(t)
	go func() { _ = h.client.StartTurn(context.Background(), "x") }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{"turnId": "turn-9"})
	deadline := time.Now().Add(2 * time.Second)
	for !h.client.TurnActive() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	errc := make(chan error, 1)
	go func() { errc <- h.client.Interrupt(context.Background()) }()
	ireq := h.readJSON()
	if ireq["method"] != "turn/interrupt" {
		t.Fatalf("method=%v", ireq["method"])
	}
	p := ireq["params"].(map[string]any)
	if p["sessionId"] != "sess-1" || p["turnId"] != "turn-9" {
		t.Fatalf("interrupt params %+v", p)
	}
	h.replyOK(ireq, map[string]any{})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func TestUsageContextGapAndDeltas(t *testing.T) {
	h := primedSession(t)
	h.writeRaw(`{"jsonrpc":"2.0","method":"session/tokenUsage","params":{"promptTokens":4,"totalTokens":6,"usage":{"outputTokens":2},"modelId":"spark"}}`)
	waitEvent(t, h, func(e Event) bool { return e.T == "usage" && e.Usage != nil && e.Usage.InputTokens == 4 })
	sp := h.client.Spend()
	if sp.InputTokens != 4 || sp.Used != 0 || sp.Size != 0 {
		t.Fatalf("spend leaked occupancy: %+v", sp)
	}
	h.writeRaw(`{"jsonrpc":"2.0","method":"session/contextUsage","params":{"usedTokens":10,"windowTokens":100}}`)
	waitEvent(t, h, func(e Event) bool { return e.T == "usage" && e.Usage != nil && e.Usage.Used == 10 })
	ctx := h.client.Context()
	if ctx.Used != 10 || ctx.Size != 100 || ctx.InputTokens != 0 {
		t.Fatalf("occupancy %+v", ctx)
	}
	h.writeRaw(`{"jsonrpc":"2.0","method":"item/started","params":{"item":{"itemId":"a1","kind":"agentMessage","revision":1,"status":"inProgress"}}}`)
	h.writeRaw(`{"jsonrpc":"2.0","method":"item/delta","params":{"itemId":"a1","delta":"Hel"}}`)
	h.writeRaw(`{"jsonrpc":"2.0","method":"item/delta","params":{"itemId":"a1","field":"summary.0","delta":"NO"}}`)
	deadline := time.Now().Add(2 * time.Second)
	for h.client.Streaming() != "Hel" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if h.client.Streaming() != "Hel" {
		t.Fatalf("streaming=%q", h.client.Streaming())
	}
	h.writeRaw(`{"jsonrpc":"2.0","method":"view/gap","params":{"after":"c-a","next":"c-b"}}`)
	waitEvent(t, h, func(e Event) bool { return e.T == "error" && strings.Contains(e.Error, "c-a") })
	if h.client.Gaps() < 1 {
		t.Fatal("gap count")
	}
	if h.client.Streaming() != "" {
		t.Fatal("gap must clear streaming")
	}
}

func TestReasoningExcludedUnknownToolKept(t *testing.T) {
	h := primedSession(t)
	h.writeRaw(`{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"itemId":"r1","kind":"reasoning","revision":1,"status":"completed","text":"secret"}}}`)
	h.writeRaw(`{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"itemId":"u1","kind":"mysteryKind","revision":1,"status":"completed"}}}`)
	waitEvent(t, h, func(e Event) bool { return e.T == "tool" && e.Tool != nil && e.Tool.Kind == "mysteryKind" })
	for _, e := range h.snap() {
		if e.T == "assistant" && e.Text == "secret" {
			t.Fatal("reasoning rendered as chat")
		}
	}
}

func TestApprovalNotificationAndDecide(t *testing.T) {
	h := primedSession(t)
	got := make(chan Approval, 1)
	h.client.SetApprovalHandler(func(a Approval) { got <- a })
	h.writeRaw(`{"jsonrpc":"2.0","method":"approval/requested","params":{"approvalId":"ap1","sessionId":"sess-1","subject":{"kind":"command","command":"ls"},"availableChoices":[{"choiceId":"allow_once","decision":"approved"},{"choiceId":"abort","decision":"abort","acceptsFeedback":true}],"currentRequirementId":{"approvalId":"ap1","sourceIndex":0}}}`)
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("handler not called")
	}
	p, ok := h.client.PendingApproval()
	if !ok || p.ApprovalID != "ap1" {
		t.Fatal("pending")
	}
	if err := h.client.Decide(context.Background(), "ap1", p.Requirement, "nope", ""); !errors.Is(err, ErrNoChoice) {
		t.Fatalf("unknown choice: %v", err)
	}
	errc := make(chan error, 1)
	go func() { errc <- h.client.Decide(context.Background(), "ap1", p.Requirement, "abort", "because") }()
	req := h.readJSON()
	if req["method"] != "approval/decide" {
		t.Fatalf("method=%v", req["method"])
	}
	params := req["params"].(map[string]any)
	if params["choiceId"] != "abort" {
		t.Fatalf("%v", params)
	}
	h.replyOK(req, map[string]any{"terminal": false})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if _, ok := h.client.PendingApproval(); !ok {
		t.Fatal("nonterminal ack must not clear pending")
	}
	h.writeRaw(`{"jsonrpc":"2.0","method":"approval/resolved","params":{"approvalId":"ap1","decision":"abort","resolvedBy":"user"}}`)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := h.client.PendingApproval(); !ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("resolved did not clear pending")
}

func TestApprovalEventCarriesProvenance(t *testing.T) {
	h := primedSession(t)
	got := make(chan Approval, 1)
	h.client.SetApprovalHandler(func(a Approval) { got <- a })
	h.writeRaw(`{"jsonrpc":"2.0","method":"approval/requested","params":{"approvalId":"ap1","sessionId":"sess-1","subject":{"kind":"command","command":"ls"},"availableChoices":[{"choiceId":"abort","decision":"abort"}],"currentRequirementId":{"approvalId":"ap1","sourceIndex":0},"_meta":{"k":"req"}}}`)
	var a Approval
	select {
	case a = <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("handler")
	}
	assertProvHas(t, a.Prov, "notification", "_meta")
	copy(a.Prov, bytes.Repeat([]byte("z"), len(a.Prov)))
	p, _ := h.client.PendingApproval()
	assertProvHas(t, p.Prov, "notification", "_meta")
	if bytes.Contains(p.Prov, []byte("zzz")) {
		t.Fatal("callback mutation leaked")
	}
	waitEvent(t, h, func(e Event) bool {
		return e.T == "tool" && e.Tool != nil && e.Tool.Kind == "approval" && e.Tool.Status == "inProgress" && len(e.Prov) > 0
	})
	h.writeRaw(`{"jsonrpc":"2.0","method":"approval/updated","params":{"approvalId":"ap1","currentRequirementId":{"approvalId":"ap1","sourceIndex":1},"availableChoices":[{"choiceId":"abort","decision":"abort"}],"provenance":[1]}}`)
	waitEvent(t, h, func(e Event) bool {
		return e.T == "tool" && e.Tool != nil && e.Tool.Kind == "approval" && e.Tool.Status == "inProgress" && bytes.Contains(e.Prov, []byte(`"provenance"`))
	})
	p2, _ := h.client.PendingApproval()
	assertProvHas(t, p2.Prov, "notification", "_meta")
	assertProvHas(t, p2.Prov, "notification", "provenance")
	h.writeRaw(`{"jsonrpc":"2.0","method":"approval/resolved","params":{"approvalId":"ap1","decision":"abort","resolvedBy":"user","_meta":{"k":"res"}}}`)
	waitEvent(t, h, func(e Event) bool {
		return e.T == "tool" && e.Tool != nil && e.Tool.Status == "completed" && bytes.Contains(e.Prov, []byte(`"k"`))
	})
}

func TestApprovalRequestFormAcksEmpty(t *testing.T) {
	h := primedSession(t)
	got := make(chan Approval, 1)
	h.client.SetApprovalHandler(func(a Approval) {
		time.Sleep(50 * time.Millisecond)
		got <- a
	})
	h.writeRaw(`{"jsonrpc":"2.0","id":"req-1","method":"approval/request","params":{"approvalId":"ap2","subject":{"kind":"command","command":"pwd"},"availableChoices":[{"choiceId":"abort","decision":"abort"}]}}`)
	resp := h.readJSON()
	if resp["id"] != "req-1" {
		t.Fatalf("id=%v", resp["id"])
	}
	if resp["error"] != nil {
		t.Fatalf("must not JSON-RPC-error the request form: %v", resp)
	}
	res, _ := json.Marshal(resp["result"])
	if strings.TrimSpace(string(res)) != "{}" && string(res) != "null" {
		if !bytes.Equal(bytes.TrimSpace(res), []byte(`{}`)) {
			t.Fatalf("want empty ack, got %s", res)
		}
	}
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("request form must still present")
	}
}

func TestOldUserInputRaceNotificationDroppedAfterClear(t *testing.T) {
	h := primedSession(t)
	orig := h.client.peer.onNotify
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	h.client.peer.onNotify = func(method string, params json.RawMessage, captured string, skip bool) {
		if method == "userInput/requested" {
			raw := copyRaw(params)
			close(entered)
			go func() {
				<-release
				if skip {
					return
				}
				h.client.declineUserInput(captured, raw)
			}()
			return
		}
		if orig != nil {
			orig(method, params, captured, skip)
		}
	}
	h.writeRaw(`{"jsonrpc":"2.0","method":"userInput/requested","params":{"userInputId":"u-old","sessionId":"sess-1","toolName":"ask"}}`)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("user input not paused")
	}
	errc := make(chan error, 1)
	go func() { errc <- h.client.Clear(context.Background(), StartParams{Cwd: "/tmp"}) }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{"sessionId": "sess-2"})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	before := len(h.snap())
	close(release)
	waitMarkerGap(t, h, "old-ui")
	for _, e := range h.snap()[before:] {
		if e.T == "error" && strings.Contains(strings.ToLower(e.Error), "user input") {
			t.Fatalf("old user-input error after clear: %q", e.Error)
		}
	}
	go func() {
		_ = h.client.Raw(context.Background(), "session/setApprovalMode", map[string]any{"mode": "onRequest"})
	}()
	next := h.readJSON()
	if next["method"] == "userInput/cancel" {
		p := next["params"].(map[string]any)
		if p["sessionId"] == "sess-2" {
			t.Fatal("old user input canceled on new session")
		}
		t.Fatal("old user input sent cancel after clear")
	}
	if next["method"] != "session/setApprovalMode" {
		t.Fatalf("method=%v", next["method"])
	}
	h.replyOK(next, map[string]any{})
}

func TestOldUserInputMissingSessionUsesCapturedNotLater(t *testing.T) {
	h := primedSession(t)
	orig := h.client.peer.onNotify
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	h.client.peer.onNotify = func(method string, params json.RawMessage, captured string, skip bool) {
		if method == "userInput/requested" {
			raw := copyRaw(params)
			close(entered)
			go func() {
				<-release
				if skip {
					return
				}
				h.client.declineUserInput(captured, raw)
			}()
			return
		}
		if orig != nil {
			orig(method, params, captured, skip)
		}
	}
	h.writeRaw(`{"jsonrpc":"2.0","method":"userInput/requested","params":{"userInputId":"u-ns"}}`)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("paused")
	}
	errc := make(chan error, 1)
	go func() { errc <- h.client.Clear(context.Background(), StartParams{Cwd: "/tmp"}) }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{"sessionId": "sess-2"})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	close(release)
	waitMarkerGap(t, h, "ui-ns")
	go func() {
		_ = h.client.Raw(context.Background(), "session/setApprovalMode", map[string]any{"mode": "onRequest"})
	}()
	next := h.readJSON()
	if next["method"] == "userInput/cancel" {
		p := next["params"].(map[string]any)
		if p["sessionId"] == "sess-2" {
			t.Fatal("missing inbound session used later session")
		}
	}
	if next["method"] != "session/setApprovalMode" {
		t.Fatalf("method=%v", next["method"])
	}
	h.replyOK(next, map[string]any{})
}

func TestOldUserInputCancelFailureDroppedAfterClear(t *testing.T) {
	h := primedSession(t)
	h.client.uiCancelWait = 30 * time.Millisecond
	orig := h.client.peer.onNotify
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	h.client.peer.onNotify = func(method string, params json.RawMessage, captured string, skip bool) {
		if method == "userInput/requested" {
			raw := copyRaw(params)
			close(entered)
			go func() {
				<-release
				if skip {
					return
				}
				h.client.declineUserInput(captured, raw)
			}()
			return
		}
		if orig != nil {
			orig(method, params, captured, skip)
		}
	}
	h.writeRaw(`{"jsonrpc":"2.0","method":"userInput/requested","params":{"userInputId":"hang","sessionId":"sess-1"}}`)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("paused")
	}
	errc := make(chan error, 1)
	go func() { errc <- h.client.Clear(context.Background(), StartParams{Cwd: "/tmp"}) }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{"sessionId": "sess-2"})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	before := len(h.snap())
	close(release)
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		for _, e := range h.snap()[before:] {
			if e.T == "error" && strings.Contains(e.Error, "userInput/cancel") {
				t.Fatal("cancel failure leaked into new session")
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestOldUserInputRequestFormAckThenInert(t *testing.T) {
	h := primedSession(t)
	orig := h.client.peer.onRequest
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	h.client.peer.onRequest = func(method string, params json.RawMessage, captured string, skip bool) (any, error) {
		res, err := orig(method, params, captured, skip)
		if method == "userInput/request" {
			if after, ok := res.(postAck); ok {
				return postAck{result: after.result, fn: func() {
					close(entered)
					<-release
					if after.fn != nil {
						after.fn()
					}
				}}, err
			}
		}
		return res, err
	}
	h.writeRaw(`{"jsonrpc":"2.0","id":8,"method":"userInput/request","params":{"userInputId":"u-old","sessionId":"sess-1"}}`)
	ack := h.readJSON()
	if ack["error"] != nil {
		t.Fatalf("request-form error: %v", ack)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("post-ack not entered")
	}
	errc := make(chan error, 1)
	go func() { errc <- h.client.Clear(context.Background(), StartParams{Cwd: "/tmp"}) }()
	req := h.readJSON()
	if req["method"] != "session/start" {
		t.Fatalf("method=%v", req["method"])
	}
	h.replyOK(req, map[string]any{"sessionId": "sess-2"})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	close(release)
	waitMarkerGap(t, h, "ui-req")
	go func() {
		_ = h.client.Raw(context.Background(), "session/setApprovalMode", map[string]any{"mode": "onRequest"})
	}()
	next := h.readJSON()
	if next["method"] == "userInput/cancel" {
		t.Fatal("old request-form still canceled after clear")
	}
	h.replyOK(next, map[string]any{})
}

func TestForeignRequestFormIsAckedAndSkipped(t *testing.T) {
	h := primedSession(t)
	h.writeRaw(`{"jsonrpc":"2.0","id":11,"method":"approval/request","params":{"approvalId":"ap-x","sessionId":"other","subject":{"kind":"command","command":"rm"},"availableChoices":[{"choiceId":"abort","decision":"abort"}]}}`)
	ack := h.readJSON()
	if ack["error"] != nil {
		t.Fatalf("must ack foreign request-form: %v", ack)
	}
	h.writeRaw(`{"jsonrpc":"2.0","id":12,"method":"userInput/request","params":{"userInputId":"u-x","sessionId":"other"}}`)
	ack2 := h.readJSON()
	if ack2["error"] != nil {
		t.Fatalf("must ack foreign user-input request-form: %v", ack2)
	}
	waitMarkerGap(t, h, "foreign-req")
	if _, ok := h.client.PendingApproval(); ok {
		t.Fatal("foreign request-form became pending")
	}
	go func() {
		_ = h.client.Raw(context.Background(), "session/setApprovalMode", map[string]any{"mode": "onRequest"})
	}()
	next := h.readJSON()
	if next["method"] == "userInput/cancel" {
		t.Fatal("foreign user-input request-form sent cancel")
	}
	h.replyOK(next, map[string]any{})
}

func TestOldApprovalRequestFormAckThenInert(t *testing.T) {
	h := primedSession(t)
	orig := h.client.peer.onRequest
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	h.client.peer.onRequest = func(method string, params json.RawMessage, captured string, skip bool) (any, error) {
		res, err := orig(method, params, captured, skip)
		if method == "approval/request" {
			if after, ok := res.(postAck); ok {
				return postAck{result: after.result, fn: func() {
					close(entered)
					<-release
					if after.fn != nil {
						after.fn()
					}
				}}, err
			}
		}
		return res, err
	}
	h.writeRaw(`{"jsonrpc":"2.0","id":9,"method":"approval/request","params":{"approvalId":"ap-old","sessionId":"sess-1","subject":{"kind":"command","command":"rm"},"availableChoices":[{"choiceId":"abort","decision":"abort"}]}}`)
	ack := h.readJSON()
	if ack["error"] != nil {
		t.Fatalf("ack error=%v", ack)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("post-ack not entered")
	}
	errc := make(chan error, 1)
	go func() { errc <- h.client.Clear(context.Background(), StartParams{Cwd: "/tmp"}) }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{"sessionId": "sess-2"})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	close(release)
	waitMarkerGap(t, h, "ap-req")
	if p, ok := h.client.PendingApproval(); ok && (p.SessionID == "sess-1" || p.ApprovalID == "ap-old") {
		t.Fatalf("old request-form pending: %+v", p)
	}
}

func TestUserInputCancelled(t *testing.T) {
	h := primedSession(t)
	h.writeRaw(`{"jsonrpc":"2.0","method":"userInput/requested","params":{"userInputId":"u1","sessionId":"sess-1","toolName":"ask","questions":[{"id":"q","prompt":"?"}]}}`)
	waitEvent(t, h, func(e Event) bool { return e.T == "error" && strings.Contains(strings.ToLower(e.Error), "user input") })
	req := h.readJSON()
	if req["method"] != "userInput/cancel" {
		t.Fatalf("method=%v", req["method"])
	}
	p := req["params"].(map[string]any)
	if p["sessionId"] != "sess-1" {
		t.Fatalf("sessionId=%v", p["sessionId"])
	}
	if p["userInputId"] != "u1" {
		t.Fatalf("userInputId=%v", p["userInputId"])
	}
	if _, ok := p["requestId"]; ok {
		t.Fatalf("outbound must not send requestId: %v", p)
	}
	if p["commandId"] == nil || p["reason"] == nil || p["reason"] == "" {
		t.Fatalf("missing commandId/reason: %v", p)
	}
	h.replyOK(req, map[string]any{})
}

func TestUserInputRequestFormAcksThenCancels(t *testing.T) {
	h := primedSession(t)
	h.writeRaw(`{"jsonrpc":"2.0","id":8,"method":"userInput/request","params":{"userInputId":"u2","questions":[{"id":"q","prompt":"?"}]}}`)
	resp := h.readJSON()
	if resp["error"] != nil {
		t.Fatalf("must not error request form: %v", resp)
	}
	if fmt.Sprint(resp["id"]) != "8" {
		t.Fatalf("ack id=%v", resp["id"])
	}
	cancel := h.readJSON()
	if cancel["method"] != "userInput/cancel" {
		t.Fatalf("cancel method=%v", cancel["method"])
	}
	cp := cancel["params"].(map[string]any)
	if cp["sessionId"] != "sess-1" {
		t.Fatal("must fall back to current session")
	}
	if cp["userInputId"] != "u2" {
		t.Fatalf("userInputId=%v", cp["userInputId"])
	}
	if _, ok := cp["requestId"]; ok {
		t.Fatal("outbound requestId must be omitted")
	}
}

func TestUserInputMissingIDSendsNothing(t *testing.T) {
	h := primedSession(t)
	h.writeRaw(`{"jsonrpc":"2.0","method":"userInput/requested","params":{"sessionId":"sess-1","toolName":"ask"}}`)
	waitEvent(t, h, func(e Event) bool { return e.T == "error" })
	go func() {
		_ = h.client.Raw(context.Background(), "session/setApprovalMode", map[string]any{"mode": "onRequest"})
	}()
	req := h.readJSON()
	if req["method"] == "userInput/cancel" {
		t.Fatal("missing userInputId must not send cancel")
	}
	if req["method"] != "session/setApprovalMode" {
		t.Fatalf("method=%v", req["method"])
	}
	h.replyOK(req, map[string]any{})
}

func TestUserInputCancelTimeoutReleasesWaiter(t *testing.T) {
	h := primedSession(t)
	h.client.uiCancelWait = 20 * time.Millisecond
	h.writeRaw(`{"jsonrpc":"2.0","method":"userInput/requested","params":{"userInputId":"hang","sessionId":"sess-1"}}`)
	req := h.readJSON()
	if req["method"] != "userInput/cancel" {
		t.Fatalf("method=%v", req["method"])
	}
	waitEvent(t, h, func(e Event) bool {
		return e.T == "error" && strings.Contains(e.Error, "userInput/cancel")
	})
	go func() {
		_ = h.client.Raw(context.Background(), "session/setApprovalMode", map[string]any{"mode": "onRequest"})
	}()
	next := h.readJSON()
	if next["method"] != "session/setApprovalMode" {
		t.Fatalf("pending cancel leaked; next=%v", next["method"])
	}
	h.replyOK(next, map[string]any{})
}

func TestUserInputCancelReleasedOnClose(t *testing.T) {
	h := primedSession(t)
	h.client.uiCancelWait = time.Hour
	h.writeRaw(`{"jsonrpc":"2.0","method":"userInput/requested","params":{"userInputId":"c","sessionId":"sess-1"}}`)
	req := h.readJSON()
	if req["method"] != "userInput/cancel" {
		t.Fatalf("method=%v", req["method"])
	}
	done := h.client.Done()
	_ = h.client.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("close did not finish while cancel was in flight")
	}
}

func TestForbiddenRawMethods(t *testing.T) {
	h := primedSession(t)
	for _, m := range []string{"session/fork", "session/compact"} {
		if err := h.client.Raw(context.Background(), m, map[string]any{}); !errors.Is(err, ErrForbiddenMethod) {
			t.Fatalf("%s err=%v", m, err)
		}
	}
	go func() {
		_ = h.client.Raw(context.Background(), "session/setApprovalMode", map[string]any{"mode": "onRequest"})
	}()
	req := h.readJSON()
	if req["method"] != "session/setApprovalMode" {
		t.Fatalf("allowed raw method not sent: %v", req["method"])
	}
	h.replyOK(req, map[string]any{})
}

func TestClientCloseIdempotentAndReleasesCalls(t *testing.T) {
	h := primed(t)
	errc := make(chan error, 1)
	go func() { errc <- h.client.StartSession(context.Background(), StartParams{}) }()
	_ = h.readJSON()
	if err := h.client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending call not released")
	}
}

func TestMalformedNotificationDoesNotKill(t *testing.T) {
	h := primedSession(t)
	h.writeRaw(`{not json`)
	h.writeRaw(`{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"itemId":"a1","kind":"agentMessage","revision":1,"status":"completed","text":"ok"}}}`)
	waitEvent(t, h, func(e Event) bool { return e.T == "assistant" && e.Text == "ok" })
}

func TestCallerMutationIsolationOnSink(t *testing.T) {
	h := primedSession(t)
	h.writeRaw(`{"jsonrpc":"2.0","method":"item/completed","params":{"_meta":{"k":"orig"},"item":{"itemId":"a1","kind":"agentMessage","revision":1,"status":"completed","text":"x"}}}`)
	waitEvent(t, h, func(e Event) bool { return e.T == "assistant" })
	evs := h.snap()
	copy(evs[0].Prov, bytes.Repeat([]byte("z"), len(evs[0].Prov)))
	h.writeRaw(`{"jsonrpc":"2.0","method":"item/completed","params":{"_meta":{"k":"orig"},"item":{"itemId":"a2","kind":"agentMessage","revision":1,"status":"completed","text":"y"}}}`)
	waitEvent(t, h, func(e Event) bool { return e.T == "assistant" && e.Text == "y" })
	for _, e := range h.snap() {
		if e.Text == "y" && bytes.Contains(e.Prov, []byte("zzz")) {
			t.Fatal("sink mutation corrupted later events")
		}
	}
}

func TestConcurrentClientAccess(t *testing.T) {
	h := primedSession(t)
	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); _ = h.client.SessionID(); _ = h.client.TurnActive(); _ = h.client.Spend() }()
	go func() { defer wg.Done(); _ = h.client.Context(); _ = h.client.Gaps(); _ = h.client.Streaming() }()
	go func() { defer wg.Done(); _, _ = h.client.PendingApproval() }()
	go func() {
		defer wg.Done()
		h.writeRaw(`{"jsonrpc":"2.0","method":"session/tokenUsage","params":{"promptTokens":1,"totalTokens":1,"usage":{"outputTokens":0}}}`)
	}()
	wg.Wait()
}

func TestClientDoneLiveAndNestedSession(t *testing.T) {
	h := primed(t)
	select {
	case <-h.client.Done():
		t.Fatal("done too early")
	default:
	}
	if err := h.client.Live(); err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { errc <- h.client.StartSession(context.Background(), StartParams{}) }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{
		"session":    map[string]any{"sessionId": "nested", "viewCursor": "vc"},
		"viewCursor": "vc2",
	})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if h.client.SessionID() != "nested" {
		t.Fatalf("session=%s", h.client.SessionID())
	}
	h.writeRaw(`{"jsonrpc":"2.0","method":"turn/started","params":{"turnId":"from-note"}}`)
	deadline := time.Now().Add(2 * time.Second)
	for h.client.ActiveTurnID() != "from-note" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if h.client.ActiveTurnID() != "from-note" {
		t.Fatal("turn/started ignored")
	}
	h.writeRaw(`{"jsonrpc":"2.0","method":"approval/updated","params":{"approvalId":"apx","change":{"kind":"x"},"availableChoices":[{"choiceId":"abort","decision":"abort"}],"subject":{"kind":"command","command":"x"}}}`)
	waitEvent(t, h, func(e Event) bool { return e.T == "tool" && e.Tool != nil && e.Tool.Kind == "approval" })
	h.writeRaw(`{"jsonrpc":"2.0","method":"approval/requested","params":{"subject":{"kind":"command"}}}`)
	waitEvent(t, h, func(e Event) bool { return e.T == "error" && strings.Contains(e.Error, "approval") })
	if err := h.client.Decide(context.Background(), "missing", RequirementRef{}, "x", ""); !errors.Is(err, ErrStaleApproval) && !errors.Is(err, ErrNoChoice) {
		t.Fatalf("decide missing: %v", err)
	}
	_ = h.client.Close()
	select {
	case <-h.client.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done not closed")
	}
	if err := h.client.Live(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Live after close: %v", err)
	}
}

func TestClearWithoutSessionAndUserInputNoSession(t *testing.T) {
	h := primed(t)
	if err := h.client.Clear(context.Background(), StartParams{}); !errors.Is(err, ErrNoSession) {
		t.Fatalf("clear: %v", err)
	}
	if err := h.client.StartTurn(context.Background(), "x"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("start: %v", err)
	}
	if err := h.client.Interrupt(context.Background()); !errors.Is(err, ErrNoSession) {
		t.Fatalf("interrupt: %v", err)
	}
	h.writeRaw(`{"jsonrpc":"2.0","method":"userInput/requested","params":{"requestId":"u","toolName":"ask","questions":[{"id":"q","prompt":"?"}]}}`)
	waitEvent(t, h, func(e Event) bool { return e.T == "error" && strings.Contains(e.Error, "no session") })
	h.writeRaw(`{"jsonrpc":"2.0","method":"userInput/requested","params":"}"}`)
}

func TestModelsModelIDAndInitializeDurability(t *testing.T) {
	h := newHarness(t)
	errc := make(chan error, 1)
	go func() { errc <- h.client.Initialize(context.Background(), "scimux", "") }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{
		"fingerprint":       PinnedFingerprint,
		"sessionDurability": "ephemeral",
		"userInputDialogs":  false,
		"serverInfo":        map[string]any{"fingerprint": "ignored"},
	})
	_ = h.readJSON()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if h.client.sessionDurable() {
		t.Fatal("ephemeral must not be durable")
	}
	if h.client.userInputCapable() {
		t.Fatal("explicit false dialogs")
	}
	go func() { errc <- h.client.StartSession(context.Background(), StartParams{ApprovalMode: "onRequest"}) }()
	req = h.readJSON()
	h.replyOK(req, map[string]any{"sessionId": "s", "viewCursor": "c"})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	go func() {
		ms, err := h.client.Models(context.Background())
		if err != nil {
			errc <- err
			return
		}
		if len(ms) != 1 || ms[0].ID != "mid" || ms[0].Label != "L" {
			errc <- fmt.Errorf("models %+v", ms)
			return
		}
		errc <- nil
	}()
	req = h.readJSON()
	h.replyOK(req, map[string]any{"source": "x", "models": []any{map[string]any{"modelId": "mid", "displayLabel": "L"}}})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func primed(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	errc := make(chan error, 1)
	go func() { errc <- h.client.Initialize(context.Background(), "scimux", "1") }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{"fingerprint": PinnedFingerprint, "capabilities": map[string]any{}})
	_ = h.readJSON()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	return h
}

func primedSession(t *testing.T) *harness {
	t.Helper()
	h := primed(t)
	errc := make(chan error, 1)
	go func() { errc <- h.client.StartSession(context.Background(), StartParams{Cwd: "/tmp"}) }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{"sessionId": "sess-1", "viewCursor": "c0"})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	return h
}

func waitEvent(t *testing.T, h *harness, ok func(Event) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range h.snap() {
			if ok(e) {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("event not seen in %+v", h.snap())
}

func wrapHoldNotify(t *testing.T, h *harness) (entered <-chan struct{}, release func()) {
	t.Helper()
	orig := h.client.peer.onNotify
	ent := make(chan struct{})
	rel := make(chan struct{})
	var enterOnce, relOnce sync.Once
	h.client.peer.onNotify = func(method string, params json.RawMessage, captured string, skip bool) {
		if method == "hold" {
			enterOnce.Do(func() { close(ent) })
			<-rel
			return
		}
		if orig != nil {
			orig(method, params, captured, skip)
		}
	}
	release = func() { relOnce.Do(func() { close(rel) }) }
	t.Cleanup(release)
	return ent, release
}

func ackStartTurn(t *testing.T, h *harness, prompt, turnID string) {
	t.Helper()
	errc := make(chan error, 1)
	go func() { errc <- h.client.StartTurn(context.Background(), prompt) }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{"turnId": turnID, "disposition": "started", "startedNewTurn": true})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func writeTurnCompleted(h *harness, turnID string) {
	h.writeRaw(`{"jsonrpc":"2.0","method":"turn/completed","params":{"sessionId":"sess-1","turnId":"` + turnID + `","terminal":"failed"}}`)
}

func waitMarkerGap(t *testing.T, h *harness, tag string) {
	t.Helper()
	h.writeRaw(`{"jsonrpc":"2.0","method":"view/gap","params":{"after":"` + tag + `","next":"` + tag + `-n"}}`)
	waitEvent(t, h, func(e Event) bool {
		return e.T == "error" && strings.Contains(e.Error, tag)
	})
}

func extraTerminals(before []Event, after []Event) int {
	n := 0
	for _, e := range after[len(before):] {
		if e.T == "error" && strings.Contains(e.Error, "view gap") {
			continue
		}
		if e.T == "error" || e.T == "stop" {
			n++
		}
	}
	return n
}

func TestCompletionBeforeAckTurnInactive(t *testing.T) {
	for i := 0; i < 25; i++ {
		h := newHarness(t)
		entered, release := wrapHoldNotify(t, h)
		errc := make(chan error, 1)
		go func() { errc <- h.client.Initialize(context.Background(), "scimux", "1") }()
		req := h.readJSON()
		h.replyOK(req, map[string]any{"fingerprint": PinnedFingerprint, "capabilities": map[string]any{}})
		_ = h.readJSON()
		if err := <-errc; err != nil {
			t.Fatal(err)
		}
		go func() { errc <- h.client.StartSession(context.Background(), StartParams{Cwd: "/tmp"}) }()
		req = h.readJSON()
		h.replyOK(req, map[string]any{"sessionId": "sess-1", "viewCursor": "c0"})
		if err := <-errc; err != nil {
			t.Fatal(err)
		}

		h.writeRaw(`{"jsonrpc":"2.0","method":"hold"}`)
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("hold notify not entered")
		}

		go func() { errc <- h.client.StartTurn(context.Background(), "hi") }()
		req = h.readJSON()
		id := rawIDFrom(t, req)
		if _, err := io.WriteString(h.to, `{"jsonrpc":"2.0","method":"turn/completed","params":{"sessionId":"sess-1","turnId":"turn-race","terminal":"failed"}}`+"\n"+
			`{"jsonrpc":"2.0","id":`+id+`,"result":{"turnId":"turn-race","disposition":"started","startedNewTurn":true}}`+"\n"); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-errc:
			if err != nil {
				t.Fatalf("iter %d StartTurn: %v", i, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("iter %d StartTurn blocked", i)
		}
		if h.client.TurnActive() {
			t.Fatalf("iter %d: completion before ack left turn active id=%s", i, h.client.ActiveTurnID())
		}
		release()
	}
}

func TestResponseCannotOvertakeEarlierLifecycleState(t *testing.T) {
	h := newHarness(t)
	entered, release := wrapHoldNotify(t, h)
	defer release()
	errc := make(chan error, 1)
	go func() { errc <- h.client.Initialize(context.Background(), "scimux", "1") }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{"fingerprint": PinnedFingerprint})
	_ = h.readJSON()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	go func() { errc <- h.client.StartSession(context.Background(), StartParams{Cwd: "/tmp"}) }()
	req = h.readJSON()
	h.replyOK(req, map[string]any{"sessionId": "sess-1"})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	h.writeRaw(`{"jsonrpc":"2.0","method":"hold"}`)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("hold not entered")
	}
	go func() { errc <- h.client.StartTurn(context.Background(), "x") }()
	req = h.readJSON()
	id := rawIDFrom(t, req)
	if _, err := io.WriteString(h.to, `{"jsonrpc":"2.0","method":"turn/started","params":{"sessionId":"sess-1","turnId":"t-over"}}`+"\n"+
		`{"jsonrpc":"2.0","method":"turn/completed","params":{"sessionId":"sess-1","turnId":"t-over","terminal":"failed"}}`+"\n"+
		`{"jsonrpc":"2.0","id":`+id+`,"result":{"turnId":"t-over","disposition":"started","startedNewTurn":true}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if h.client.TurnActive() {
		t.Fatal("later start ack overtook earlier completed lifecycle")
	}
}

func TestCloseReleasesOrderedBarrierCall(t *testing.T) {
	h := primedSession(t)
	entered, release := wrapHoldNotify(t, h)
	h.writeRaw(`{"jsonrpc":"2.0","method":"hold"}`)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("hold not entered")
	}
	errc := make(chan error, 1)
	go func() { errc <- h.client.StartTurn(context.Background(), "x") }()
	_ = h.readJSON()
	if err := h.client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		if !errors.Is(err, ErrClosed) && !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ordered StartTurn was not released by Close")
	}
	release()
}

func TestClientLiveAfterPeerEOF(t *testing.T) {
	h := newHarness(t)
	_ = h.to.Close()
	waitFor(t, func() bool { return h.client.Live() != nil })
	if !errors.Is(h.client.Live(), ErrClosed) {
		t.Fatalf("Live=%v", h.client.Live())
	}
}

func TestClientCloseIdempotentAndReturnsTransportError(t *testing.T) {
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	base := &pipeTr{w: c2sW, r: s2cR}
	cw := &closeWrap{Transport: base, err: errors.New("boom")}
	c := NewClient(cw, nil, nil)
	t.Cleanup(func() { _ = c2sR.Close(); _ = s2cW.Close() })
	if err := c.Close(); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("first close err=%v", err)
	}
	if cw.count() != 1 {
		t.Fatalf("close count=%d", cw.count())
	}
	if err := c.Close(); cw.count() != 1 {
		t.Fatalf("second close invoked transport: n=%d err=%v", cw.count(), err)
	}
}

func TestClientCloseConcurrentOnce(t *testing.T) {
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	base := &pipeTr{w: c2sW, r: s2cR}
	cw := &closeWrap{Transport: base, err: errors.New("once")}
	c := NewClient(cw, nil, nil)
	t.Cleanup(func() { _ = c2sR.Close(); _ = s2cW.Close() })
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- c.Close()
		}()
	}
	wg.Wait()
	close(errs)
	if cw.count() != 1 {
		t.Fatalf("transport closed %d times", cw.count())
	}
}

func TestContextCancelReleasesOrderedBarrierWaiter(t *testing.T) {
	h := primedSession(t)
	entered, release := wrapHoldNotify(t, h)
	defer release()
	h.writeRaw(`{"jsonrpc":"2.0","method":"hold"}`)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("hold not entered")
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- h.client.StartTurn(ctx, "x") }()
	_ = h.readJSON()
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled StartTurn leaked")
	}
	h.client.peer.mu.Lock()
	n := len(h.client.peer.pend)
	h.client.peer.mu.Unlock()
	if n != 0 {
		t.Fatalf("pending waiter remains after cancel: %d", n)
	}
}

func TestTerminalReplayAfterLaterTurnIgnored(t *testing.T) {
	h := primedSession(t)
	ackStartTurn(t, h, "a", "turn-a")
	writeTurnCompleted(h, "turn-a")
	waitMarkerGap(t, h, "done-a")
	if h.client.TurnActive() {
		t.Fatal("A still active")
	}
	ackStartTurn(t, h, "b", "turn-b")
	writeTurnCompleted(h, "turn-b")
	waitMarkerGap(t, h, "done-b")
	if h.client.TurnActive() {
		t.Fatal("B still active")
	}
	before := h.snap()
	writeTurnCompleted(h, "turn-a")
	waitMarkerGap(t, h, "replay-a")
	if extraTerminals(before, h.snap()) != 0 {
		t.Fatalf("replayed A after B emitted again: %+v", h.snap()[len(before):])
	}
	if h.client.TurnActive() {
		t.Fatal("replayed A changed turn state")
	}
}

func TestStartedReplayAfterTerminalsIgnored(t *testing.T) {
	h := primedSession(t)
	ackStartTurn(t, h, "a", "turn-a")
	writeTurnCompleted(h, "turn-a")
	waitMarkerGap(t, h, "started-done-a")
	ackStartTurn(t, h, "b", "turn-b")
	writeTurnCompleted(h, "turn-b")
	waitMarkerGap(t, h, "started-done-b")
	h.writeRaw(`{"jsonrpc":"2.0","method":"turn/started","params":{"sessionId":"sess-1","turnId":"turn-a"}}`)
	waitMarkerGap(t, h, "started-replay")
	if h.client.TurnActive() {
		t.Fatalf("replayed turn/started reactivated %s", h.client.ActiveTurnID())
	}
}

func TestArbitraryIdleTerminalIgnored(t *testing.T) {
	h := primedSession(t)
	before := h.snap()
	h.writeRaw(`{"jsonrpc":"2.0","method":"turn/completed","params":{"sessionId":"sess-1","turnId":"turn-x","terminal":"failed"}}`)
	waitMarkerGap(t, h, "idle-x")
	if extraTerminals(before, h.snap()) != 0 {
		t.Fatalf("idle unseen terminal emitted: %+v", h.snap()[len(before):])
	}
	if h.client.TurnActive() {
		t.Fatal("idle terminal activated a turn")
	}
}

func TestTerminalForOtherTurnWhileActiveIgnored(t *testing.T) {
	h := primedSession(t)
	ackStartTurn(t, h, "y", "turn-y")
	before := h.snap()
	h.writeRaw(`{"jsonrpc":"2.0","method":"turn/completed","params":{"sessionId":"sess-1","turnId":"turn-x","terminal":"failed"}}`)
	waitMarkerGap(t, h, "other-x")
	if extraTerminals(before, h.snap()) != 0 {
		t.Fatalf("other-turn terminal emitted: %+v", h.snap()[len(before):])
	}
	if h.client.ActiveTurnID() != "turn-y" {
		t.Fatalf("active=%s", h.client.ActiveTurnID())
	}
}

func TestDuplicateTerminalDeliveryEmitsOnce(t *testing.T) {
	h := primedSession(t)
	ackStartTurn(t, h, "a", "turn-dup")
	before := h.snap()
	writeTurnCompleted(h, "turn-dup")
	waitMarkerGap(t, h, "dup-1")
	n := extraTerminals(before, h.snap())
	if n != 1 {
		t.Fatalf("first terminal records=%d, want 1", n)
	}
	mid := h.snap()
	writeTurnCompleted(h, "turn-dup")
	waitMarkerGap(t, h, "dup-2")
	if extraTerminals(mid, h.snap()) != 0 {
		t.Fatalf("duplicate terminal emitted again: %+v", h.snap()[len(mid):])
	}
}

func TestMalformedAckCleansInFlight(t *testing.T) {
	h := primedSession(t)
	errc := make(chan error, 1)
	go func() { errc <- h.client.StartTurn(context.Background(), "x") }()
	req := h.readJSON()
	h.writeRaw(`{"jsonrpc":"2.0","id":` + rawIDFrom(t, req) + `,"result":"not-an-object"}`)
	if err := <-errc; err == nil {
		t.Fatal("malformed ack must fail")
	}
	before := h.snap()
	h.writeRaw(`{"jsonrpc":"2.0","method":"turn/completed","params":{"sessionId":"sess-1","turnId":"turn-ghost","terminal":"failed"}}`)
	waitMarkerGap(t, h, "malformed-inflight")
	if extraTerminals(before, h.snap()) != 0 {
		t.Fatal("in-flight leaked after malformed ack; idle terminal was accepted")
	}
	if h.client.TurnActive() {
		t.Fatal("malformed ack left a turn active")
	}
}

func TestCancelledCallCleansInFlight(t *testing.T) {
	h := primedSession(t)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- h.client.StartTurn(ctx, "x") }()
	_ = h.readJSON()
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not release StartTurn")
	}
	before := h.snap()
	h.writeRaw(`{"jsonrpc":"2.0","method":"turn/completed","params":{"sessionId":"sess-1","turnId":"turn-cancel","terminal":"failed"}}`)
	waitMarkerGap(t, h, "cancel-inflight")
	if extraTerminals(before, h.snap()) != 0 {
		t.Fatal("in-flight leaked after cancel; idle terminal was accepted")
	}
}

func TestSuccessfulNewSessionResetsRecentTerminals(t *testing.T) {
	h := primedSession(t)
	ackStartTurn(t, h, "a", "turn-a")
	writeTurnCompleted(h, "turn-a")
	deadline := time.Now().Add(2 * time.Second)
	for h.client.TurnActive() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	errc := make(chan error, 1)
	go func() { errc <- h.client.StartSession(context.Background(), StartParams{Cwd: "/tmp"}) }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{"sessionId": "sess-2", "viewCursor": "c2"})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	ackStartTurn(t, h, "again", "turn-a")
	if !h.client.TurnActive() || h.client.ActiveTurnID() != "turn-a" {
		t.Fatal("successful new session must forget prior terminal history")
	}
}

func TestFailedSessionStartPreservesRecentTerminals(t *testing.T) {
	h := primedSession(t)
	ackStartTurn(t, h, "a", "turn-keep")
	writeTurnCompleted(h, "turn-keep")
	deadline := time.Now().Add(2 * time.Second)
	for h.client.TurnActive() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	old := h.client.SessionID()
	errc := make(chan error, 1)
	go func() { errc <- h.client.StartSession(context.Background(), StartParams{Cwd: "/x"}) }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{"viewCursor": "no-id"})
	if err := <-errc; err == nil {
		t.Fatal("missing sessionId must fail")
	}
	if h.client.SessionID() != old {
		t.Fatal("failed session/start mutated session")
	}
	h.writeRaw(`{"jsonrpc":"2.0","method":"turn/started","params":{"sessionId":"sess-1","turnId":"turn-keep"}}`)
	waitMarkerGap(t, h, "keep-recent")
	if h.client.TurnActive() {
		t.Fatal("failed session/start dropped terminal history")
	}
}

func TestBoundedTerminalHistoryCapacity(t *testing.T) {
	h := primedSession(t)
	const n = 8
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("old-%d", i)
		ackStartTurn(t, h, id, id)
		writeTurnCompleted(h, id)
		waitMarkerGap(t, h, "cap-done-"+id)
		if h.client.TurnActive() {
			t.Fatalf("%s still active", id)
		}
	}
	before := h.snap()
	writeTurnCompleted(h, "old-0")
	waitMarkerGap(t, h, "cap-replay")
	if extraTerminals(before, h.snap()) != 0 {
		t.Fatalf("early terminal replayed after later ones: %+v", h.snap()[len(before):])
	}

	h2 := primedSession(t)
	errc := make(chan error, 1)
	go func() { errc <- h2.client.StartTurn(context.Background(), "cap") }()
	req := h2.readJSON()
	id := rawIDFrom(t, req)
	var b strings.Builder
	for i := 0; i < 80; i++ {
		fmt.Fprintf(&b, `{"jsonrpc":"2.0","method":"turn/completed","params":{"sessionId":"sess-1","turnId":"cap-%d","terminal":"failed"}}`+"\n", i)
	}
	fmt.Fprintf(&b, `{"jsonrpc":"2.0","id":%s,"result":{"turnId":"turn-cap","disposition":"started","startedNewTurn":true}}`+"\n", id)
	if _, err := io.WriteString(h2.to, b.String()); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	h2.client.mu.Lock()
	got := h2.client.recent.count()
	h2.client.mu.Unlock()
	if got > recentTerminalCap {
		t.Fatalf("recent terminals=%d exceeds cap %d", got, recentTerminalCap)
	}
	if got != recentTerminalCap {
		t.Fatalf("recent terminals=%d, want bounded cap %d", got, recentTerminalCap)
	}
}

func TestTerminalHistoryAddEmptyDuplicateAndEmit(t *testing.T) {
	var h terminalHistory
	h.add("")
	if h.count() != 0 || h.has("") {
		t.Fatal("empty id must not enter history")
	}
	h.add("a")
	h.add("a")
	if h.count() != 1 {
		t.Fatalf("duplicate add count=%d", h.count())
	}
	if !h.markEmitted("a") {
		t.Fatal("first emit")
	}
	if h.markEmitted("a") {
		t.Fatal("second emit must be suppressed")
	}
	if h.markEmitted("missing") {
		t.Fatal("unseen id must not emit")
	}
	sess := primedSession(t)
	sess.client.handleTurnStarted(json.RawMessage(`{`))
	sess.client.handleTurnStarted(json.RawMessage(`{"turnId":""}`))
	ackStartTurn(t, sess, "y", "turn-y")
	sess.client.handleTurnStarted(json.RawMessage(`{"turnId":"turn-x"}`))
	if sess.client.ActiveTurnID() != "turn-y" {
		t.Fatalf("other started overwrote active: %s", sess.client.ActiveTurnID())
	}
}

func TestSessionStartFollowedByNewSessionApproval(t *testing.T) {
	h := primedSession(t)
	got := make(chan Approval, 1)
	h.client.SetApprovalHandler(func(a Approval) { got <- a })
	errc := make(chan error, 1)
	go func() { errc <- h.client.Clear(context.Background(), StartParams{Cwd: "/tmp"}) }()
	req := h.readJSON()
	id := rawIDFrom(t, req)
	payload := `{"jsonrpc":"2.0","id":` + id + `,"result":{"sessionId":"sess-2"}}` + "\n" +
		`{"jsonrpc":"2.0","method":"approval/requested","params":{"approvalId":"ap-new","sessionId":"sess-2","subject":{"kind":"command","command":"ls"},"availableChoices":[{"choiceId":"abort","decision":"abort"}],"currentRequirementId":{"approvalId":"ap-new","sourceIndex":0}}}` + "\n"
	if _, err := io.WriteString(h.to, payload); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-got:
		if a.ApprovalID != "ap-new" || a.SessionID != "sess-2" {
			t.Fatalf("approval=%+v", a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("new-session approval missing")
	}
	p, ok := h.client.PendingApproval()
	if !ok || p.ApprovalID != "ap-new" {
		t.Fatalf("pending=%v ok=%v", p.ApprovalID, ok)
	}
}

func TestOldNotifyDequeuedAfterStartIsDropped(t *testing.T) {
	h := primedSession(t)
	entered, release := wrapHoldNotify(t, h)
	h.writeRaw(`{"jsonrpc":"2.0","method":"hold"}`)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("hold not entered")
	}
	h.writeRaw(`{"jsonrpc":"2.0","method":"item/completed","params":{"sessionId":"sess-1","item":{"itemId":"old","kind":"agentMessage","revision":1,"status":"completed","text":"pre-clear"}}}`)
	errc := make(chan error, 1)
	go func() { errc <- h.client.Clear(context.Background(), StartParams{Cwd: "/tmp"}) }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{"sessionId": "sess-2", "viewCursor": "c-new"})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if h.client.SessionID() != "sess-2" {
		t.Fatalf("session=%s", h.client.SessionID())
	}
	release()
	waitMarkerGap(t, h, "after-old-drop")
	for _, e := range h.snap() {
		if e.T == "assistant" && e.Text == "pre-clear" {
			t.Fatal("old-session item processed after new session installed")
		}
	}
}

func TestFailedSessionStartKeepsOldSessionProcessing(t *testing.T) {
	h := primedSession(t)
	old := h.client.SessionID()
	errc := make(chan error, 1)
	go func() { errc <- h.client.StartSession(context.Background(), StartParams{Cwd: "/x"}) }()
	req := h.readJSON()
	h.writeJSON(map[string]any{
		"jsonrpc": "2.0", "id": req["id"],
		"error": map[string]any{"code": -32603, "message": "cannot start"},
	})
	if err := <-errc; err == nil {
		t.Fatal("expected start failure")
	}
	if h.client.SessionID() != old {
		t.Fatalf("session changed to %s", h.client.SessionID())
	}
	h.writeRaw(`{"jsonrpc":"2.0","method":"item/completed","params":{"sessionId":"sess-1","item":{"itemId":"keep","kind":"agentMessage","revision":1,"status":"completed","text":"still-old"}}}`)
	waitEvent(t, h, func(e Event) bool { return e.T == "assistant" && e.Text == "still-old" })
}

func TestMalformedSessionStartReleasesAndKeepsIdentity(t *testing.T) {
	h := primedSession(t)
	old := h.client.SessionID()
	cur := h.client.ViewCursor()
	errc := make(chan error, 1)
	go func() { errc <- h.client.StartSession(context.Background(), StartParams{Cwd: "/x"}) }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{"viewCursor": "no-id"})
	if err := <-errc; err == nil {
		t.Fatal("malformed start must fail")
	}
	if h.client.SessionID() != old || h.client.ViewCursor() != cur {
		t.Fatalf("identity session=%s cursor=%s", h.client.SessionID(), h.client.ViewCursor())
	}
	h.writeRaw(`{"jsonrpc":"2.0","method":"session/contextUsage","params":{"sessionId":"sess-1","usedTokens":3,"windowTokens":30}}`)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.client.Context().Used == 3 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("old-session usage not applied: %+v", h.client.Context())
}

func swapSessionSink(c *Client, fn func(string, Event)) func(string, Event) {
	c.sinkMu.Lock()
	defer c.sinkMu.Unlock()
	prev := c.sessionSink
	c.sessionSink = fn
	return prev
}

func pendingKey(t *testing.T, req map[string]any) string {
	t.Helper()
	b, err := json.Marshal(req["id"])
	if err != nil {
		t.Fatal(err)
	}
	return canonID(b)
}

func waitCallClaimed(t *testing.T, p *peer, key string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		_, ok := p.pend[key]
		p.mu.Unlock()
		if !ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("call was not claimed")
}

func TestSessionStartCancelBeforeResponseKeepsIdentity(t *testing.T) {
	h := primedSession(t)
	old := h.client.SessionID()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- h.client.StartSession(ctx, StartParams{Cwd: "/tmp"}) }()
	req := h.readJSON()
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	h.replyOK(req, map[string]any{"sessionId": "sess-2", "viewCursor": "c-new"})
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if h.client.SessionID() != old {
			t.Fatalf("late response committed session=%s", h.client.SessionID())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSessionStartClaimedResponseWinsOverCancel(t *testing.T) {
	h := primedSession(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- h.client.StartSession(ctx, StartParams{Cwd: "/tmp"}) }()
	req := h.readJSON()
	key := pendingKey(t, req)
	h.client.mu.Lock()
	h.replyOK(req, map[string]any{"sessionId": "sess-2", "viewCursor": "c-new"})
	waitCallClaimed(t, h.client.peer, key)
	cancel()
	select {
	case err := <-errc:
		h.client.mu.Unlock()
		t.Fatalf("cancel returned %v while apply blocked on session lock", err)
	default:
	}
	h.client.mu.Unlock()
	if err := <-errc; err != nil {
		t.Fatalf("claimed start must succeed: %v", err)
	}
	if h.client.SessionID() != "sess-2" {
		t.Fatalf("session=%s", h.client.SessionID())
	}
}

func TestClientLiveDuringBlockedClose(t *testing.T) {
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	base := &pipeTr{w: c2sW, r: s2cR}
	entered := make(chan struct{})
	block := make(chan struct{})
	cw := &closeWrap{Transport: base, entered: entered, block: block, err: errors.New("slow-close")}
	c := NewClient(cw, nil, nil)
	t.Cleanup(func() {
		select {
		case <-block:
		default:
			close(block)
		}
		_ = c2sR.Close()
		_ = s2cW.Close()
	})
	errc := make(chan error, 1)
	go func() { errc <- c.Close() }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("transport Close not entered")
	}
	livec := make(chan error, 1)
	go func() { livec <- c.Live() }()
	second := make(chan error, 1)
	go func() { second <- c.Close() }()
	select {
	case err := <-livec:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("Live=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Live blocked behind transport Close")
	}
	select {
	case <-second:
		t.Fatal("concurrent Close returned while first Close blocked")
	default:
	}
	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done not closed while Close blocked")
	}
	close(block)
	err := <-errc
	if err == nil || !strings.Contains(err.Error(), "slow-close") {
		t.Fatalf("Close=%v", err)
	}
	if err2 := <-second; err2.Error() != err.Error() {
		t.Fatalf("waiter Close %v vs %v", err2, err)
	}
	if cw.count() != 1 {
		t.Fatalf("transport closed %d times", cw.count())
	}
	if err2 := c.Close(); err2.Error() != err.Error() {
		t.Fatalf("stored Close %v vs %v", err2, err)
	}
}

func TestClientCloseNilPeerAndTransport(t *testing.T) {
	c := &Client{done: make(chan struct{})}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(c.Live(), ErrClosed) {
		t.Fatalf("Live=%v", c.Live())
	}
	select {
	case <-c.Done():
	default:
		t.Fatal("Done not closed")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestClientCloseConcurrentWaitersShareError(t *testing.T) {
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	base := &pipeTr{w: c2sW, r: s2cR}
	entered := make(chan struct{})
	block := make(chan struct{})
	cw := &closeWrap{Transport: base, entered: entered, block: block, err: errors.New("shared-boom")}
	c := NewClient(cw, nil, nil)
	t.Cleanup(func() {
		select {
		case <-block:
		default:
			close(block)
		}
		_ = c2sR.Close()
		_ = s2cW.Close()
	})
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	wg.Add(1)
	go func() {
		defer wg.Done()
		errs <- c.Close()
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("transport Close not entered")
	}
	for i := 0; i < 7; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- c.Close()
		}()
	}
	close(block)
	wg.Wait()
	close(errs)
	if cw.count() != 1 {
		t.Fatalf("transport closed %d times", cw.count())
	}
	var first error
	n := 0
	for err := range errs {
		n++
		if err == nil || !strings.Contains(err.Error(), "shared-boom") {
			t.Fatalf("err=%v", err)
		}
		if first == nil {
			first = err
		} else if first.Error() != err.Error() {
			t.Fatalf("divergent Close errors %v vs %v", first, err)
		}
	}
	if n != 8 {
		t.Fatalf("got %d close results", n)
	}
}

func TestMalformedApprovalUpdatedKeepsPending(t *testing.T) {
	h := primedSession(t)
	h.writeRaw(`{"jsonrpc":"2.0","method":"approval/requested","params":{"approvalId":"ap1","sessionId":"sess-1","subject":{"kind":"command","command":"ls"},"availableChoices":[{"choiceId":"abort","decision":"abort"}],"currentRequirementId":{"approvalId":"ap1","sourceIndex":0}}}`)
	waitEvent(t, h, func(e Event) bool {
		return e.T == "tool" && e.Tool != nil && e.Tool.Kind == "approval" && e.Tool.Status == "inProgress"
	})
	h.writeRaw(`{"jsonrpc":"2.0","method":"approval/updated","params":{}}`)
	waitEvent(t, h, func(e Event) bool { return e.T == "error" && strings.Contains(e.Error, "approvalId") })
	p, ok := h.client.PendingApproval()
	if !ok || p.ApprovalID != "ap1" {
		t.Fatalf("pending corrupted: ok=%v id=%s", ok, p.ApprovalID)
	}
}

func TestApprovalUpdateOtherIDDoesNotReplace(t *testing.T) {
	h := primedSession(t)
	h.writeRaw(`{"jsonrpc":"2.0","method":"approval/requested","params":{"approvalId":"ap1","sessionId":"sess-1","subject":{"kind":"command","command":"ls"},"availableChoices":[{"choiceId":"abort","decision":"abort"}],"currentRequirementId":{"approvalId":"ap1","sourceIndex":0}}}`)
	waitEvent(t, h, func(e Event) bool {
		return e.T == "tool" && e.Tool != nil && e.Tool.ID == "ap1"
	})
	h.writeRaw(`{"jsonrpc":"2.0","method":"approval/updated","params":{"approvalId":"ap-other","subject":{"kind":"command","command":"rm"},"availableChoices":[{"choiceId":"abort","decision":"abort"}],"currentRequirementId":{"approvalId":"ap-other","sourceIndex":9}}}`)
	waitMarkerGap(t, h, "other-update")
	p, ok := h.client.PendingApproval()
	if !ok || p.ApprovalID != "ap1" {
		t.Fatalf("pending=%v ok=%v", p.ApprovalID, ok)
	}
	if p.Subject.Command == "rm" {
		t.Fatal("foreign update hybridized subject")
	}
	if p.Requirement.SourceIndex == 9 {
		t.Fatal("foreign update replaced requirement")
	}
}

func TestMalformedApprovalResolvedKeepsPending(t *testing.T) {
	h := primedSession(t)
	h.writeRaw(`{"jsonrpc":"2.0","method":"approval/requested","params":{"approvalId":"ap1","sessionId":"sess-1","subject":{"kind":"command","command":"ls"},"availableChoices":[{"choiceId":"abort","decision":"abort"}],"currentRequirementId":{"approvalId":"ap1","sourceIndex":0}}}`)
	waitEvent(t, h, func(e Event) bool {
		return e.T == "tool" && e.Tool != nil && e.Tool.Kind == "approval"
	})
	h.writeRaw(`{"jsonrpc":"2.0","method":"approval/resolved","params":{"decision":"abort"}}`)
	waitEvent(t, h, func(e Event) bool { return e.T == "error" && strings.Contains(e.Error, "approvalId") })
	if _, ok := h.client.PendingApproval(); !ok {
		t.Fatal("malformed resolved cleared pending")
	}
}

func pauseNotifyAfterSnapshot(t *testing.T, h *harness, method string) (entered <-chan struct{}, release func()) {
	t.Helper()
	orig := h.client.peer.onNotify
	ent := make(chan struct{})
	rel := make(chan struct{})
	var enterOnce, relOnce sync.Once
	h.client.peer.onNotify = func(m string, params json.RawMessage, captured string, skip bool) {
		if m == method {
			raw := copyRaw(params)
			enterOnce.Do(func() { close(ent) })
			go func() {
				<-rel
				if skip {
					return
				}
				switch method {
				case "approval/requested":
					h.client.handleApprovalRequested(captured, raw)
				case "approval/updated":
					h.client.handleApprovalUpdated(captured, raw)
				case "approval/resolved":
					h.client.handleApprovalResolved(captured, raw)
				}
			}()
			return
		}
		if orig != nil {
			orig(m, params, captured, skip)
		}
	}
	release = func() { relOnce.Do(func() { close(rel) }) }
	t.Cleanup(release)
	return ent, release
}

func TestOldApprovalRequestedRaceNewSession(t *testing.T) {
	h := primedSession(t)
	entered, release := pauseNotifyAfterSnapshot(t, h, "approval/requested")
	h.writeRaw(`{"jsonrpc":"2.0","method":"approval/requested","params":{"approvalId":"ap-old","sessionId":"sess-1","subject":{"kind":"command","command":"rm"},"availableChoices":[{"choiceId":"abort","decision":"abort"}],"currentRequirementId":{"approvalId":"ap-old","sourceIndex":0}}}`)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("old request not paused")
	}
	errc := make(chan error, 1)
	go func() { errc <- h.client.Clear(context.Background(), StartParams{Cwd: "/tmp"}) }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{"sessionId": "sess-2"})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	release()
	waitMarkerGap(t, h, "old-ap-req")
	if p, ok := h.client.PendingApproval(); ok && p.SessionID == "sess-1" {
		t.Fatalf("old pending survived: %+v", p)
	}
	if p, ok := h.client.PendingApproval(); ok && p.ApprovalID == "ap-old" {
		t.Fatal("old approval id pending after clear")
	}
}

func TestOldApprovalUpdatedRaceNewSession(t *testing.T) {
	h := primedSession(t)
	h.writeRaw(`{"jsonrpc":"2.0","method":"approval/requested","params":{"approvalId":"ap1","sessionId":"sess-1","subject":{"kind":"command","command":"ls"},"availableChoices":[{"choiceId":"abort","decision":"abort"}],"currentRequirementId":{"approvalId":"ap1","sourceIndex":0}}}`)
	waitEvent(t, h, func(e Event) bool {
		return e.T == "tool" && e.Tool != nil && e.Tool.ID == "ap1"
	})
	entered, release := pauseNotifyAfterSnapshot(t, h, "approval/updated")
	h.writeRaw(`{"jsonrpc":"2.0","method":"approval/updated","params":{"approvalId":"ap1","sessionId":"sess-1","subject":{"kind":"command","command":"rm"},"availableChoices":[{"choiceId":"abort","decision":"abort"}],"currentRequirementId":{"approvalId":"ap1","sourceIndex":9}}}`)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("old update not paused")
	}
	errc := make(chan error, 1)
	go func() { errc <- h.client.Clear(context.Background(), StartParams{Cwd: "/tmp"}) }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{"sessionId": "sess-2"})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	h.writeRaw(`{"jsonrpc":"2.0","method":"approval/requested","params":{"approvalId":"ap1","sessionId":"sess-2","subject":{"kind":"command","command":"ls"},"availableChoices":[{"choiceId":"abort","decision":"abort"}],"currentRequirementId":{"approvalId":"ap1","sourceIndex":0}}}`)
	waitEvent(t, h, func(e Event) bool {
		return e.T == "tool" && e.Tool != nil && e.Tool.ID == "ap1" && e.Tool.Status == "inProgress"
	})
	release()
	waitMarkerGap(t, h, "old-ap-upd")
	p, ok := h.client.PendingApproval()
	if !ok {
		t.Fatal("new pending missing")
	}
	if p.SessionID != "sess-2" {
		t.Fatalf("session=%s", p.SessionID)
	}
	if p.Requirement.SourceIndex == 9 || p.Subject.Command == "rm" {
		t.Fatal("old update hybridized new approval")
	}
}

func TestOldApprovalResolvedRaceNewSession(t *testing.T) {
	h := primedSession(t)
	entered, release := pauseNotifyAfterSnapshot(t, h, "approval/resolved")
	h.writeRaw(`{"jsonrpc":"2.0","method":"approval/resolved","params":{"approvalId":"ap1","sessionId":"sess-1","decision":"abort","resolvedBy":"user"}}`)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("old resolved not paused")
	}
	errc := make(chan error, 1)
	go func() { errc <- h.client.Clear(context.Background(), StartParams{Cwd: "/tmp"}) }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{"sessionId": "sess-2"})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	h.writeRaw(`{"jsonrpc":"2.0","method":"approval/requested","params":{"approvalId":"ap1","sessionId":"sess-2","subject":{"kind":"command","command":"ls"},"availableChoices":[{"choiceId":"abort","decision":"abort"}],"currentRequirementId":{"approvalId":"ap1","sourceIndex":0}}}`)
	waitEvent(t, h, func(e Event) bool {
		return e.T == "tool" && e.Tool != nil && e.Tool.ID == "ap1" && e.Tool.Status == "inProgress"
	})
	release()
	waitMarkerGap(t, h, "old-ap-res")
	p, ok := h.client.PendingApproval()
	if !ok || p.SessionID != "sess-2" {
		t.Fatalf("new pending cleared by old resolve ok=%v sess=%s", ok, p.SessionID)
	}
}

func TestWithSessionRejectsStaleCaptured(t *testing.T) {
	h := primedSession(t)
	ran := false
	if h.client.withSession("other", func() { ran = true }) {
		t.Fatal("stale captured accepted")
	}
	if ran {
		t.Fatal("stale mutation ran")
	}
	if !h.client.withSession("sess-1", func() { ran = true }) || !ran {
		t.Fatal("current captured rejected")
	}
	if !h.client.withSession("", func() {}) {
		t.Fatal("empty captured rejected")
	}
}

func TestApprovalPendingCloneDoesNotAlias(t *testing.T) {
	h := primedSession(t)
	h.writeRaw(`{"jsonrpc":"2.0","method":"approval/requested","params":{"approvalId":"ap1","sessionId":"sess-1","subject":{"kind":"command","command":"ls"},"availableChoices":[{"choiceId":"abort","decision":"abort"}],"currentRequirementId":{"approvalId":"ap1","sourceIndex":0}}}`)
	waitEvent(t, h, func(e Event) bool { return e.T == "tool" && e.Tool != nil && e.Tool.ID == "ap1" })
	p, ok := h.client.PendingApproval()
	if !ok {
		t.Fatal("pending")
	}
	p.Subject.Command = "mutated"
	p.ApprovalID = "other"
	p2, _ := h.client.PendingApproval()
	if p2.Subject.Command != "ls" || p2.ApprovalID != "ap1" {
		t.Fatalf("alias leaked: %+v", p2)
	}
}

func TestApprovalResolvedMergesPresentationAndUpdateProv(t *testing.T) {
	h := primedSession(t)
	h.writeRaw(`{"jsonrpc":"2.0","method":"approval/requested","params":{"approvalId":"ap1","sessionId":"sess-1","subject":{"kind":"command","command":"ls"},"availableChoices":[{"choiceId":"abort","decision":"abort"}],"currentRequirementId":{"approvalId":"ap1","sourceIndex":0},"_meta":{"from":"req"}}}`)
	waitEvent(t, h, func(e Event) bool {
		return e.T == "tool" && e.Tool != nil && e.Tool.Status == "inProgress"
	})
	h.writeRaw(`{"jsonrpc":"2.0","method":"approval/updated","params":{"approvalId":"ap1","currentRequirementId":{"approvalId":"ap1","sourceIndex":1},"availableChoices":[{"choiceId":"abort","decision":"abort"}],"provenance":[1]}}`)
	waitEvent(t, h, func(e Event) bool {
		return e.T == "tool" && e.Tool != nil && e.Tool.Status == "inProgress" && bytes.Contains(e.Prov, []byte(`"provenance"`))
	})
	h.writeRaw(`{"jsonrpc":"2.0","method":"approval/resolved","params":{"approvalId":"ap1","decision":"abort","resolvedBy":"user","_meta":{"from":"res"}}}`)
	waitEvent(t, h, func(e Event) bool {
		return e.T == "tool" && e.Tool != nil && e.Tool.Status == "completed" &&
			bytes.Contains(e.Prov, []byte(`"from"`)) && bytes.Contains(e.Prov, []byte(`"provenance"`))
	})
}

func TestNotifyViewCursorAndMalformedUsage(t *testing.T) {
	h := primedSession(t)
	h.writeRaw(`{"jsonrpc":"2.0","method":"item/delta","params":{"sessionId":"sess-1","viewCursor":"c-cursor","item":{"itemId":"d1","kind":"agentMessage","revision":1,"delta":{"text":"x"}}}}`)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.client.ViewCursor() == "c-cursor" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if h.client.ViewCursor() != "c-cursor" {
		t.Fatalf("cursor=%s", h.client.ViewCursor())
	}
	before := len(h.snap())
	h.writeRaw(`{"jsonrpc":"2.0","method":"session/tokenUsage","params":"bad"}`)
	h.writeRaw(`{"jsonrpc":"2.0","method":"session/contextUsage","params":"bad"}`)
	h.writeRaw(`{"jsonrpc":"2.0","method":"session/tokenUsage","params":{"sessionId":"sess-1","promptTokens":1,"totalTokens":2,"cumulative":{"totalTokens":50},"usage":{"outputTokens":1}}}`)
	waitEvent(t, h, func(e Event) bool { return e.T == "usage" && e.Usage != nil && e.Usage.TotalTokens == 2 })
	if h.client.Spend().TotalTokens < 50 {
		t.Fatalf("cumulative spend not applied: %+v", h.client.Spend())
	}
	if len(h.snap()) < before {
		t.Fatal("events lost")
	}
}

func TestEmitIfCurrentDropsStaleSession(t *testing.T) {
	h := primedSession(t)
	h.client.emitIfCurrent("other-sess", Event{T: "assistant", Text: "stale"})
	h.client.emitIfCurrent("", Event{T: "assistant", Text: "blank-ok"})
	waitEvent(t, h, func(e Event) bool { return e.T == "assistant" && e.Text == "blank-ok" })
	for _, e := range h.snap() {
		if e.T == "assistant" && e.Text == "stale" {
			t.Fatal("stale captured emit leaked")
		}
	}
	h.client.handleApprovalRequested("other-sess", json.RawMessage(`{"approvalId":"ap-stale","sessionId":"other-sess","subject":{"kind":"command","command":"rm"},"availableChoices":[{"choiceId":"abort","decision":"abort"}]}`))
	if _, ok := h.client.PendingApproval(); ok {
		t.Fatal("stale present installed pending")
	}
}

func TestClientCloseNilDone(t *testing.T) {
	c := &Client{}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(c.Live(), ErrClosed) {
		t.Fatalf("Live=%v", c.Live())
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDecideWithoutSessionIDFailsLocally(t *testing.T) {
	h := newHarness(t)
	h.client.handleApprovalRequested("", json.RawMessage(`{"approvalId":"ap1","subject":{"kind":"command","command":"ls"},"availableChoices":[{"choiceId":"abort","decision":"abort"}],"currentRequirementId":{"approvalId":"ap1","sourceIndex":0}}`))
	p, ok := h.client.PendingApproval()
	if !ok {
		t.Fatal("pending missing")
	}
	err := h.client.Decide(context.Background(), "ap1", p.Requirement, "abort", "")
	if !errors.Is(err, ErrNoSession) {
		t.Fatalf("err=%v", err)
	}
	errc := make(chan error, 1)
	go func() { errc <- h.client.Raw(context.Background(), "ping", map[string]any{}) }()
	req := h.readJSON()
	if req["method"] != "ping" {
		t.Fatalf("Decide wrote RPC method=%v", req["method"])
	}
	h.replyOK(req, map[string]any{})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func holdNotifyThenStart(t *testing.T, h *harness, frame string) (before int) {
	t.Helper()
	entered, release := wrapHoldNotify(t, h)
	h.writeRaw(`{"jsonrpc":"2.0","method":"hold"}`)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("notifyLoop not blocked")
	}
	h.writeRaw(frame)
	errc := make(chan error, 1)
	go func() { errc <- h.client.Clear(context.Background(), StartParams{Cwd: "/tmp"}) }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{"sessionId": "sess-2", "viewCursor": "c-new"})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if h.client.SessionID() != "sess-2" {
		t.Fatalf("session=%s", h.client.SessionID())
	}
	before = len(h.snap())
	release()
	waitMarkerGap(t, h, "scope-hold")
	return before
}

func TestOldNotifyMissingSessionIdAssistantInertAfterStart(t *testing.T) {
	h := primedSession(t)
	before := holdNotifyThenStart(t, h, `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"itemId":"old","kind":"agentMessage","revision":1,"status":"completed","text":"pre-clear"}}}`)
	for _, e := range h.snap()[before:] {
		if e.T == "assistant" && e.Text == "pre-clear" {
			t.Fatal("old assistant rebound to new session")
		}
	}
}

func TestOldNotifyMissingSessionIdApprovalInertAfterStart(t *testing.T) {
	h := primedSession(t)
	got := make(chan Approval, 1)
	h.client.SetApprovalHandler(func(a Approval) { got <- a })
	before := holdNotifyThenStart(t, h, `{"jsonrpc":"2.0","method":"approval/requested","params":{"approvalId":"ap-old","subject":{"kind":"command","command":"rm"},"availableChoices":[{"choiceId":"abort","decision":"abort"}],"currentRequirementId":{"approvalId":"ap-old","sourceIndex":0}}}`)
	if p, ok := h.client.PendingApproval(); ok && p.ApprovalID == "ap-old" {
		t.Fatalf("old approval pending on new session: %+v", p)
	}
	select {
	case a := <-got:
		t.Fatalf("approval callback ran for old frame: %+v", a)
	default:
	}
	for _, e := range h.snap()[before:] {
		if e.T == "tool" && e.Tool != nil && e.Tool.ID == "ap-old" {
			t.Fatal("old approval tool event on new session")
		}
	}
}

func TestOldNotifyMissingSessionIdApprovalUpdateInertAfterStart(t *testing.T) {
	h := primedSession(t)
	h.writeRaw(`{"jsonrpc":"2.0","method":"approval/requested","params":{"approvalId":"ap1","sessionId":"sess-1","subject":{"kind":"command","command":"ls"},"availableChoices":[{"choiceId":"abort","decision":"abort"}],"currentRequirementId":{"approvalId":"ap1","sourceIndex":0}}}`)
	waitEvent(t, h, func(e Event) bool {
		return e.T == "tool" && e.Tool != nil && e.Tool.ID == "ap1"
	})
	holdNotifyThenStart(t, h, `{"jsonrpc":"2.0","method":"approval/updated","params":{"approvalId":"ap1","subject":{"kind":"command","command":"rm"},"availableChoices":[{"choiceId":"abort","decision":"abort"}],"currentRequirementId":{"approvalId":"ap1","sourceIndex":9}}}`)
	if p, ok := h.client.PendingApproval(); ok {
		t.Fatalf("update resurrected pending: %+v", p)
	}
}

func TestOldNotifyMissingSessionIdApprovalResolvedInertAfterStart(t *testing.T) {
	h := primedSession(t)
	before := holdNotifyThenStart(t, h, `{"jsonrpc":"2.0","method":"approval/resolved","params":{"approvalId":"ap1","decision":"abort","resolvedBy":"user"}}`)
	for _, e := range h.snap()[before:] {
		if e.T == "tool" && e.Tool != nil && e.Tool.ID == "ap1" {
			t.Fatal("old approval resolved on new session")
		}
	}
	h.writeRaw(`{"jsonrpc":"2.0","method":"approval/requested","params":{"approvalId":"ap1","sessionId":"sess-2","subject":{"kind":"command","command":"ls"},"availableChoices":[{"choiceId":"abort","decision":"abort"}],"currentRequirementId":{"approvalId":"ap1","sourceIndex":0}}}`)
	waitEvent(t, h, func(e Event) bool {
		return e.T == "tool" && e.Tool != nil && e.Tool.ID == "ap1" && e.Tool.Status == "inProgress"
	})
	p, ok := h.client.PendingApproval()
	if !ok || p.ApprovalID != "ap1" || p.SessionID != "sess-2" {
		t.Fatalf("new pending missing: %+v ok=%v", p, ok)
	}
}

func TestOldNotifyMissingSessionIdUsageInertAfterStart(t *testing.T) {
	h := primedSession(t)
	before := holdNotifyThenStart(t, h, `{"jsonrpc":"2.0","method":"session/tokenUsage","params":{"promptTokens":99,"totalTokens":99,"usage":{"outputTokens":1}}}`)
	if h.client.Spend().TotalTokens == 99 {
		t.Fatal("old token usage applied to new session")
	}
	for _, e := range h.snap()[before:] {
		if e.T == "usage" && e.Usage != nil && e.Usage.TotalTokens == 99 {
			t.Fatal("old usage event on new session")
		}
	}
	before = len(h.snap())
	entered, release := wrapHoldNotify(t, h)
	h.writeRaw(`{"jsonrpc":"2.0","method":"hold"}`)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("hold")
	}
	h.writeRaw(`{"jsonrpc":"2.0","method":"session/contextUsage","params":{"usedTokens":7,"windowTokens":70}}`)
	errc := make(chan error, 1)
	go func() { errc <- h.client.Clear(context.Background(), StartParams{Cwd: "/tmp"}) }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{"sessionId": "sess-3"})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	release()
	waitMarkerGap(t, h, "ctx-hold")
	if h.client.Context().Used == 7 {
		t.Fatal("old context usage applied to new session")
	}
	_ = before
}

func TestOldNotifyMissingSessionIdUserInputInertAfterStart(t *testing.T) {
	h := primedSession(t)
	before := holdNotifyThenStart(t, h, `{"jsonrpc":"2.0","method":"userInput/requested","params":{"userInputId":"u-old","toolName":"ask"}}`)
	for _, e := range h.snap()[before:] {
		if e.T == "error" && strings.Contains(strings.ToLower(e.Error), "user input") {
			t.Fatalf("old user-input error on new session: %q", e.Error)
		}
	}
	go func() {
		_ = h.client.Raw(context.Background(), "session/setApprovalMode", map[string]any{"mode": "onRequest"})
	}()
	next := h.readJSON()
	if next["method"] == "userInput/cancel" {
		t.Fatal("old user-input wrote cancel after start")
	}
	if next["method"] != "session/setApprovalMode" {
		t.Fatalf("method=%v", next["method"])
	}
	h.replyOK(next, map[string]any{})
}

func holdRequestThenStart(t *testing.T, h *harness, frame string) (ack map[string]any) {
	t.Helper()
	orig := h.client.peer.onRequest
	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce, relOnce sync.Once
	h.client.peer.onRequest = func(method string, params json.RawMessage, captured string, skip bool) (any, error) {
		enterOnce.Do(func() { close(entered) })
		<-release
		return orig(method, params, captured, skip)
	}
	t.Cleanup(func() { relOnce.Do(func() { close(release) }) })
	h.writeRaw(frame)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("request handler not blocked")
	}
	errc := make(chan error, 1)
	go func() { errc <- h.client.Clear(context.Background(), StartParams{Cwd: "/tmp"}) }()
	req := h.readJSON()
	if req["method"] != "session/start" {
		t.Fatalf("blocked request stalled start; method=%v", req["method"])
	}
	h.replyOK(req, map[string]any{"sessionId": "sess-2"})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	relOnce.Do(func() { close(release) })
	ack = h.readJSON()
	if ack["error"] != nil {
		t.Fatalf("request-form error: %v", ack)
	}
	waitMarkerGap(t, h, "req-scope")
	return ack
}

func TestOldApprovalRequestMissingSessionIdAckThenInert(t *testing.T) {
	h := primedSession(t)
	ack := holdRequestThenStart(t, h, `{"jsonrpc":"2.0","id":21,"method":"approval/request","params":{"approvalId":"ap-old","subject":{"kind":"command","command":"rm"},"availableChoices":[{"choiceId":"abort","decision":"abort"}]}}`)
	if fmt.Sprint(ack["id"]) != "21" {
		t.Fatalf("ack id=%v", ack["id"])
	}
	if p, ok := h.client.PendingApproval(); ok && p.ApprovalID == "ap-old" {
		t.Fatalf("old request became new-session approval: %+v", p)
	}
}

func TestOldUserInputRequestMissingSessionIdAckThenInert(t *testing.T) {
	h := primedSession(t)
	ack := holdRequestThenStart(t, h, `{"jsonrpc":"2.0","id":22,"method":"userInput/request","params":{"userInputId":"u-old"}}`)
	if fmt.Sprint(ack["id"]) != "22" {
		t.Fatalf("ack id=%v", ack["id"])
	}
	go func() {
		_ = h.client.Raw(context.Background(), "session/setApprovalMode", map[string]any{"mode": "onRequest"})
	}()
	next := h.readJSON()
	if next["method"] == "userInput/cancel" {
		p, _ := next["params"].(map[string]any)
		if p["sessionId"] == "sess-2" {
			t.Fatal("old request canceled with new session id")
		}
		t.Fatal("old request-form wrote cancel after start")
	}
	h.replyOK(next, map[string]any{})
}

func TestRequestAfterSessionStartUsesNewSession(t *testing.T) {
	h := primedSession(t)
	errc := make(chan error, 1)
	go func() { errc <- h.client.Clear(context.Background(), StartParams{Cwd: "/tmp"}) }()
	req := h.readJSON()
	id := rawIDFrom(t, req)
	payload := `{"jsonrpc":"2.0","id":` + id + `,"result":{"sessionId":"sess-2"}}` + "\n" +
		`{"jsonrpc":"2.0","id":31,"method":"approval/request","params":{"approvalId":"ap-new","subject":{"kind":"command","command":"ls"},"availableChoices":[{"choiceId":"abort","decision":"abort"}]}}` + "\n"
	if _, err := io.WriteString(h.to, payload); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	ack := h.readJSON()
	if fmt.Sprint(ack["id"]) != "31" {
		t.Fatalf("ack=%v", ack)
	}
	waitForClient(t, func() bool {
		p, ok := h.client.PendingApproval()
		return ok && p.ApprovalID == "ap-new"
	})
	p, _ := h.client.PendingApproval()
	if p.SessionID != "" && p.SessionID != "sess-2" {
		t.Fatalf("request after start scoped to %q", p.SessionID)
	}
}

func TestBlockedRequestDoesNotStallUnrelatedCall(t *testing.T) {
	h := primedSession(t)
	orig := h.client.peer.onRequest
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	h.client.peer.onRequest = func(method string, params json.RawMessage, captured string, skip bool) (any, error) {
		close(entered)
		<-release
		return orig(method, params, captured, skip)
	}
	h.writeRaw(`{"jsonrpc":"2.0","id":40,"method":"approval/request","params":{"approvalId":"ap-block","subject":{"kind":"command","command":"ls"},"availableChoices":[{"choiceId":"abort","decision":"abort"}]}}`)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handler not entered")
	}
	errc := make(chan error, 1)
	go func() { errc <- h.client.Raw(context.Background(), "ping", map[string]any{}) }()
	req := h.readJSON()
	if req["method"] != "ping" {
		t.Fatalf("blocked request stalled Call; method=%v", req["method"])
	}
	h.replyOK(req, map[string]any{})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	close(release)
	ack := h.readJSON()
	if fmt.Sprint(ack["id"]) != "40" {
		t.Fatalf("ack=%v", ack)
	}
}

func waitForClient(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met")
}

func installPending(t *testing.T, h *harness, id, session string) Approval {
	t.Helper()
	frame := `{"jsonrpc":"2.0","method":"approval/requested","params":{"approvalId":"` + id + `","sessionId":"` + session + `","subject":{"kind":"command","command":"ls"},"availableChoices":[{"choiceId":"abort","decision":"abort"}],"currentRequirementId":{"approvalId":"` + id + `","sourceIndex":0}}}`
	if session == "" {
		frame = `{"jsonrpc":"2.0","method":"approval/requested","params":{"approvalId":"` + id + `","subject":{"kind":"command","command":"ls"},"availableChoices":[{"choiceId":"abort","decision":"abort"}],"currentRequirementId":{"approvalId":"` + id + `","sourceIndex":0}}}`
	}
	h.writeRaw(frame)
	waitEvent(t, h, func(e Event) bool {
		return e.T == "tool" && e.Tool != nil && e.Tool.ID == id && e.Tool.Status == "inProgress"
	})
	p, ok := h.client.PendingApproval()
	if !ok {
		t.Fatal("pending missing")
	}
	return p
}

func waitOpWaiter(t *testing.T, c *Client) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.op.mu.Lock()
		waiting := c.op.wait != nil
		c.op.mu.Unlock()
		if waiting {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("session operation waiter not blocked")
}

func TestPendingApprovalNotCurrentAfterSessionStoreAdvances(t *testing.T) {
	h := primedSession(t)
	installPending(t, h, "ap1", "sess-1")
	h.client.mu.Lock()
	h.client.session = Session{SessionID: "sess-2"}
	h.client.sessionGen++
	h.client.mu.Unlock()
	if p, ok := h.client.PendingApproval(); ok {
		t.Fatalf("old approval visible as current: %+v", p)
	}
}

func TestPendingApprovalRejectsForeignSession(t *testing.T) {
	h := primedSession(t)
	h.client.handleApprovalRequested("sess-1", json.RawMessage(`{"approvalId":"ap-x","sessionId":"other","subject":{"kind":"command","command":"rm"},"availableChoices":[{"choiceId":"abort","decision":"abort"}],"currentRequirementId":{"approvalId":"ap-x","sourceIndex":0}}`))
	if p, ok := h.client.PendingApproval(); ok && p.SessionID == "other" {
		t.Fatalf("foreign approval returned as current: %+v", p)
	}
}

func TestPendingApprovalEmptyIDDoesNotSurviveGeneration(t *testing.T) {
	h := primedSession(t)
	h.client.handleApprovalRequested("sess-1", json.RawMessage(`{"approvalId":"ap-empty","subject":{"kind":"command","command":"ls"},"availableChoices":[{"choiceId":"abort","decision":"abort"}],"currentRequirementId":{"approvalId":"ap-empty","sourceIndex":0}}`))
	p, ok := h.client.PendingApproval()
	if !ok || p.ApprovalID != "ap-empty" {
		t.Fatalf("empty-session approval should be current in this generation: ok=%v id=%s", ok, p.ApprovalID)
	}
	h.client.mu.Lock()
	h.client.session = Session{SessionID: "sess-2"}
	h.client.sessionGen++
	h.client.mu.Unlock()
	if p, ok := h.client.PendingApproval(); ok {
		t.Fatalf("empty-session approval survived generation: %+v", p)
	}
}

func TestDecideWinsOverStartSessionWireOrder(t *testing.T) {
	h := primedSession(t)
	p := installPending(t, h, "ap1", "sess-1")
	decErr := make(chan error, 1)
	go func() {
		decErr <- h.client.Decide(context.Background(), p.ApprovalID, p.Requirement, "abort", "")
	}()
	decide := h.readJSON()
	if decide["method"] != "approval/decide" {
		t.Fatalf("method=%v", decide["method"])
	}
	startErr := make(chan error, 1)
	go func() {
		startErr <- h.client.StartSession(context.Background(), StartParams{Cwd: "/tmp"})
	}()
	waitOpWaiter(t, h.client)
	h.replyOK(decide, map[string]any{})
	if err := <-decErr; err != nil {
		t.Fatal(err)
	}
	start := h.readJSON()
	if start["method"] != "session/start" {
		t.Fatalf("expected session/start after decide finished, got %v", start["method"])
	}
	h.replyOK(start, map[string]any{"sessionId": "sess-2"})
	if err := <-startErr; err != nil {
		t.Fatal(err)
	}
}

func TestStartSessionWinsOverDecideNoDecideFrame(t *testing.T) {
	h := primedSession(t)
	p := installPending(t, h, "ap1", "sess-1")
	startErr := make(chan error, 1)
	go func() {
		startErr <- h.client.StartSession(context.Background(), StartParams{Cwd: "/tmp"})
	}()
	start := h.readJSON()
	if start["method"] != "session/start" {
		t.Fatalf("method=%v", start["method"])
	}
	decErr := make(chan error, 1)
	go func() {
		decErr <- h.client.Decide(context.Background(), p.ApprovalID, p.Requirement, "abort", "")
	}()
	waitOpWaiter(t, h.client)
	h.replyOK(start, map[string]any{"sessionId": "sess-2"})
	if err := <-startErr; err != nil {
		t.Fatal(err)
	}
	err := <-decErr
	if !errors.Is(err, ErrStaleApproval) {
		t.Fatalf("err=%v", err)
	}
	go func() {
		_ = h.client.Raw(context.Background(), "ping", map[string]any{})
	}()
	next := h.readJSON()
	if next["method"] == "approval/decide" {
		t.Fatal("session/start win still wrote approval/decide")
	}
	if next["method"] != "ping" {
		t.Fatalf("method=%v", next["method"])
	}
	h.replyOK(next, map[string]any{})
	if _, ok := h.client.PendingApproval(); ok {
		t.Fatal("old approval survived successful start")
	}
}

func TestFailedStartSessionKeepsPendingApproval(t *testing.T) {
	h := primedSession(t)
	p := installPending(t, h, "ap1", "sess-1")
	errc := make(chan error, 1)
	go func() { errc <- h.client.StartSession(context.Background(), StartParams{Cwd: "/x"}) }()
	req := h.readJSON()
	h.writeJSON(map[string]any{
		"jsonrpc": "2.0", "id": req["id"],
		"error": map[string]any{"code": -32603, "message": "nope"},
	})
	if err := <-errc; err == nil {
		t.Fatal("expected failure")
	}
	got, ok := h.client.PendingApproval()
	if !ok || got.ApprovalID != p.ApprovalID {
		t.Fatalf("pending lost after failed start: ok=%v id=%s", ok, got.ApprovalID)
	}
	decErr := make(chan error, 1)
	go func() { decErr <- h.client.Decide(context.Background(), p.ApprovalID, p.Requirement, "abort", "") }()
	decide := h.readJSON()
	if decide["method"] != "approval/decide" {
		t.Fatalf("method=%v", decide["method"])
	}
	h.replyOK(decide, map[string]any{})
	if err := <-decErr; err != nil {
		t.Fatal(err)
	}
}

func TestMalformedStartSessionKeepsPendingApproval(t *testing.T) {
	h := primedSession(t)
	installPending(t, h, "ap1", "sess-1")
	errc := make(chan error, 1)
	go func() { errc <- h.client.StartSession(context.Background(), StartParams{Cwd: "/x"}) }()
	req := h.readJSON()
	h.replyOK(req, map[string]any{"viewCursor": "no-id"})
	if err := <-errc; err == nil {
		t.Fatal("expected malformed start")
	}
	if _, ok := h.client.PendingApproval(); !ok {
		t.Fatal("malformed start cleared pending")
	}
}

func TestUserInputCancelWinsOverStartSession(t *testing.T) {
	h := primedSession(t)
	h.writeRaw(`{"jsonrpc":"2.0","method":"userInput/requested","params":{"userInputId":"u-win","sessionId":"sess-1"}}`)
	cancel := h.readJSON()
	if cancel["method"] != "userInput/cancel" {
		t.Fatalf("method=%v", cancel["method"])
	}
	if p := cancel["params"].(map[string]any); p["sessionId"] != "sess-1" {
		t.Fatalf("sessionId=%v", p["sessionId"])
	}
	startErr := make(chan error, 1)
	go func() { startErr <- h.client.StartSession(context.Background(), StartParams{Cwd: "/tmp"}) }()
	waitOpWaiter(t, h.client)
	h.replyOK(cancel, map[string]any{})
	start := h.readJSON()
	if start["method"] != "session/start" {
		t.Fatalf("session/start issued before cancel finished: %v", start["method"])
	}
	h.replyOK(start, map[string]any{"sessionId": "sess-2"})
	if err := <-startErr; err != nil {
		t.Fatal(err)
	}
}

func TestUserInputCancelMissingSessionWinsOverStartSession(t *testing.T) {
	h := primedSession(t)
	h.writeRaw(`{"jsonrpc":"2.0","method":"userInput/requested","params":{"userInputId":"u-ns"}}`)
	cancel := h.readJSON()
	if cancel["method"] != "userInput/cancel" {
		t.Fatalf("method=%v", cancel["method"])
	}
	if p := cancel["params"].(map[string]any); p["sessionId"] != "sess-1" {
		t.Fatalf("missing inbound sessionId used %v", p["sessionId"])
	}
	startErr := make(chan error, 1)
	go func() { startErr <- h.client.StartSession(context.Background(), StartParams{Cwd: "/tmp"}) }()
	waitOpWaiter(t, h.client)
	h.replyOK(cancel, map[string]any{})
	start := h.readJSON()
	if start["method"] != "session/start" {
		t.Fatalf("method=%v", start["method"])
	}
	h.replyOK(start, map[string]any{"sessionId": "sess-2"})
	if err := <-startErr; err != nil {
		t.Fatal(err)
	}
}

func TestStartSessionWinsOverUserInputCancel(t *testing.T) {
	h := primedSession(t)
	startErr := make(chan error, 1)
	go func() { startErr <- h.client.StartSession(context.Background(), StartParams{Cwd: "/tmp"}) }()
	start := h.readJSON()
	if start["method"] != "session/start" {
		t.Fatalf("method=%v", start["method"])
	}
	h.writeRaw(`{"jsonrpc":"2.0","method":"userInput/requested","params":{"userInputId":"u-lose","sessionId":"sess-1"}}`)
	waitOpWaiter(t, h.client)
	h.replyOK(start, map[string]any{"sessionId": "sess-2"})
	if err := <-startErr; err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = h.client.Raw(context.Background(), "ping", map[string]any{})
	}()
	next := h.readJSON()
	if next["method"] == "userInput/cancel" {
		t.Fatal("start-win still wrote userInput/cancel")
	}
	if next["method"] != "ping" {
		t.Fatalf("method=%v", next["method"])
	}
	h.replyOK(next, map[string]any{})
}

func TestStartSessionWinsOverUserInputCancelMissingSession(t *testing.T) {
	h := primedSession(t)
	startErr := make(chan error, 1)
	go func() { startErr <- h.client.StartSession(context.Background(), StartParams{Cwd: "/tmp"}) }()
	start := h.readJSON()
	h.writeRaw(`{"jsonrpc":"2.0","method":"userInput/requested","params":{"userInputId":"u-lose-ns"}}`)
	waitOpWaiter(t, h.client)
	h.replyOK(start, map[string]any{"sessionId": "sess-2"})
	if err := <-startErr; err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = h.client.Raw(context.Background(), "ping", map[string]any{})
	}()
	next := h.readJSON()
	if next["method"] == "userInput/cancel" {
		p, _ := next["params"].(map[string]any)
		if p["sessionId"] == "sess-2" {
			t.Fatal("missing inbound sessionId canceled with new session")
		}
		t.Fatal("start-win wrote cancel for missing sessionId")
	}
	h.replyOK(next, map[string]any{})
}

func TestUserInputRequestFormAckThenCancelWinsOverStart(t *testing.T) {
	h := primedSession(t)
	h.writeRaw(`{"jsonrpc":"2.0","id":51,"method":"userInput/request","params":{"userInputId":"u-req","sessionId":"sess-1"}}`)
	ack := h.readJSON()
	if fmt.Sprint(ack["id"]) != "51" || ack["error"] != nil {
		t.Fatalf("ack=%v", ack)
	}
	cancel := h.readJSON()
	if cancel["method"] != "userInput/cancel" {
		t.Fatalf("method=%v", cancel["method"])
	}
	startErr := make(chan error, 1)
	go func() { startErr <- h.client.StartSession(context.Background(), StartParams{Cwd: "/tmp"}) }()
	waitOpWaiter(t, h.client)
	h.replyOK(cancel, map[string]any{})
	start := h.readJSON()
	if start["method"] != "session/start" {
		t.Fatalf("method=%v", start["method"])
	}
	h.replyOK(start, map[string]any{"sessionId": "sess-2"})
	if err := <-startErr; err != nil {
		t.Fatal(err)
	}
}

func TestStartSessionWinsOverUserInputRequestFormCancel(t *testing.T) {
	h := primedSession(t)
	startErr := make(chan error, 1)
	go func() { startErr <- h.client.StartSession(context.Background(), StartParams{Cwd: "/tmp"}) }()
	start := h.readJSON()
	h.writeRaw(`{"jsonrpc":"2.0","id":52,"method":"userInput/request","params":{"userInputId":"u-req-lose"}}`)
	ack := h.readJSON()
	if fmt.Sprint(ack["id"]) != "52" || ack["error"] != nil {
		t.Fatalf("ack must precede cancel work: %v", ack)
	}
	waitOpWaiter(t, h.client)
	h.replyOK(start, map[string]any{"sessionId": "sess-2"})
	if err := <-startErr; err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = h.client.Raw(context.Background(), "ping", map[string]any{})
	}()
	next := h.readJSON()
	if next["method"] == "userInput/cancel" {
		t.Fatal("request-form cancel written after start won")
	}
	h.replyOK(next, map[string]any{})
}

func TestStartSessionAndDecideAfterClose(t *testing.T) {
	h := primedSession(t)
	p := installPending(t, h, "ap1", "sess-1")
	if err := h.client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.client.StartSession(context.Background(), StartParams{Cwd: "/tmp"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("StartSession after Close: %v", err)
	}
	if err := h.client.Decide(context.Background(), p.ApprovalID, p.Requirement, "abort", ""); !errors.Is(err, ErrClosed) {
		t.Fatalf("Decide after Close: %v", err)
	}
}

func TestDecideRejectsApprovalSessionMismatch(t *testing.T) {
	h := primedSession(t)
	h.client.handleApprovalRequested("sess-1", json.RawMessage(`{"approvalId":"ap-x","sessionId":"other","subject":{"kind":"command","command":"rm"},"availableChoices":[{"choiceId":"abort","decision":"abort"}],"currentRequirementId":{"approvalId":"ap-x","sourceIndex":0}}`))
	err := h.client.Decide(context.Background(), "ap-x", RequirementRef{ApprovalID: "ap-x", SourceIndex: 0}, "abort", "")
	if !errors.Is(err, ErrStaleApproval) {
		t.Fatalf("err=%v", err)
	}
	go func() {
		_ = h.client.Raw(context.Background(), "ping", map[string]any{})
	}()
	next := h.readJSON()
	if next["method"] == "approval/decide" {
		t.Fatal("mismatched session wrote approval/decide")
	}
	h.replyOK(next, map[string]any{})
}

func TestOpGateAcquireCancelAndIdempotentShutdown(t *testing.T) {
	var g opGate
	if err := g.acquire(nil, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- g.acquire(ctx, nil) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		g.mu.Lock()
		waiting := g.wait != nil
		g.mu.Unlock()
		if waiting {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled acquire leaked")
	}
	g.release()
	g.shutdown()
	g.shutdown()
	if err := g.acquire(context.Background(), nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("after shutdown: %v", err)
	}
}

func TestUserInputCancelAcquireTimeoutEmits(t *testing.T) {
	h := primedSession(t)
	h.client.uiCancelWait = 30 * time.Millisecond
	if err := h.client.op.acquire(context.Background(), h.client.Done()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.client.op.release)
	h.writeRaw(`{"jsonrpc":"2.0","method":"userInput/requested","params":{"userInputId":"u-to","sessionId":"sess-1"}}`)
	waitEvent(t, h, func(e Event) bool {
		return e.T == "error" && strings.Contains(e.Error, "userInput/cancel failed")
	})
}

func TestCloseReleasesSessionOpWaiter(t *testing.T) {
	h := primedSession(t)
	if err := h.client.op.acquire(context.Background(), h.client.Done()); err != nil {
		t.Fatal(err)
	}
	waitErr := make(chan error, 1)
	go func() {
		waitErr <- h.client.op.acquire(context.Background(), h.client.Done())
	}()
	waitOpWaiter(t, h.client)
	closed := make(chan error, 1)
	go func() { closed <- h.client.Close() }()
	select {
	case err := <-waitErr:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("waiter err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not release session op waiter")
	}
	h.client.op.release()
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}
