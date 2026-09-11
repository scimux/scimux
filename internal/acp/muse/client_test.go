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
	var a Approval
	select {
	case a = <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("handler not called")
	}
	if a.AutoApprovable() {
		t.Fatal("auto-approvable")
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
	h.client.peer.onNotify = func(method string, params json.RawMessage) {
		if method == "hold" {
			enterOnce.Do(func() { close(ent) })
			<-rel
			return
		}
		if orig != nil {
			orig(method, params)
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
