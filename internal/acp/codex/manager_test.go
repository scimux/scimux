package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newManagerWithMock builds a Manager whose SpawnFunc returns an in-process
// pipe-backed transport, so the whole launch/turn/approval path is exercised
// without the codex CLI and without tokens.
func newManagerWithMock(t *testing.T) (*Manager, *mockTransport, *mockServer) {
	t.Helper()
	mt, ms := newMockTransport()
	m := NewManagerWithSpawn(t.TempDir(), func(nodeID, dir string) (Transport, error) {
		return mt, nil
	})
	return m, mt, ms
}

// launch drives the initialize + thread/start handshake for a node, asserting
// the request order, and returns the session id Launch produced.
func launch(t *testing.T, m *Manager, ms *mockServer, nodeID, model, effort string) string {
	t.Helper()
	type res struct {
		sid string
		err error
	}
	rc := make(chan res, 1)
	go func() {
		sid, err := m.Launch(nodeID, "codex", "/w", model, effort)
		rc <- res{sid, err}
	}()
	if r := ms.nextReq(t); r.Method != "initialize" {
		t.Fatalf("first req = %q", r.Method)
	} else {
		ms.reply(t, r.ID, `{}`)
	}
	r := ms.nextReq(t)
	if r.Method != "thread/start" {
		t.Fatalf("second req = %q", r.Method)
	}
	if effort != "" && !strings.Contains(string(r.Params), `"model_reasoning_effort":"`+effort+`"`) {
		t.Fatalf("effort not forwarded to thread/start: %s", r.Params)
	}
	ms.reply(t, r.ID, threadStartResult)
	got := <-rc
	if got.err != nil {
		t.Fatalf("launch: %v", got.err)
	}
	if got.sid != "THREAD-1" {
		t.Fatalf("session id = %q, want thread id", got.sid)
	}
	return got.sid
}

// A launch that fails after the meta header was written must remove the
// meta-only log it just created, or the slug reads as taken-by-dead-history
// forever (R20.3).
func TestManagerLaunchFailureRemovesMetaOnlyLog(t *testing.T) {
	mt, ms := newMockTransport()
	dir := t.TempDir()
	m := NewManagerWithSpawn(dir, func(nodeID, d string) (Transport, error) { return mt, nil })
	errc := make(chan error, 1)
	go func() {
		_, err := m.Launch("n1", "codex", "/w", "", "")
		errc <- err
	}()
	r := ms.nextReq(t)
	if r.Method != "initialize" {
		t.Fatalf("first req = %q", r.Method)
	}
	idb, _ := json.Marshal(r.ID)
	ms.writeRaw(t, `{"id":`+string(idb)+`,"error":{"code":-1,"message":"broken binary"}}`)
	if err := <-errc; err == nil {
		t.Fatal("launch should have failed")
	}
	if _, err := os.Stat(filepath.Join(dir, "n1.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("meta-only log left behind by a failed launch: stat err = %v", err)
	}
}

// RecordStartFailure must not recreate a session log that no longer exists
// (node deleted, history archived, while the first prompt was in flight) —
// that would resurrect the dead slug as an orphan (R20.2).
func TestManagerRecordStartFailureSkipsMissingLog(t *testing.T) {
	dir := t.TempDir()
	m := NewManagerWithSpawn(dir, func(nodeID, d string) (Transport, error) {
		t.Fatal("spawn must not run")
		return nil, nil
	})
	if err := m.RecordStartFailure("gone", errors.New("subprocess killed by delete")); err != nil {
		t.Fatalf("RecordStartFailure on a deleted node: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "gone.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("RecordStartFailure recreated a deleted node's log: stat err = %v", err)
	}
}

func TestManagerLaunchAndTurn(t *testing.T) {
	m, _, ms := newManagerWithMock(t)
	launch(t, m, ms, "n1", "gpt-5.5", "high")

	if got := m.Live("n1"); got != "quiet" {
		t.Fatalf("post-launch live = %q, want quiet", got)
	}
	if err := m.Send("n1", "ping"); err != nil {
		t.Fatalf("send: %v", err)
	}
	r := ms.nextReq(t)
	if r.Method != "turn/start" || !strings.Contains(string(r.Params), `"text":"ping"`) {
		t.Fatalf("turn/start = %q %s", r.Method, r.Params)
	}
	ms.reply(t, r.ID, `{"turn":{}}`)
	ms.note(t, "item/completed", `{"item":{"type":"agentMessage","text":"pong"}}`)
	ms.note(t, "thread/tokenUsage/updated", `{"tokenUsage":{"last":{"totalTokens":42},"modelContextWindow":1000}}`)
	ms.note(t, "turn/completed", `{"turn":{}}`)

	waitFor(t, func() bool { return m.Live("n1") == "quiet" && len(m.Turns("n1")) == 2 })

	turns := m.Turns("n1")
	if turns[0].Role != "user" || turns[0].Text != "ping" {
		t.Fatalf("turn[0] = %+v", turns[0])
	}
	if turns[1].Role != "assistant" || turns[1].Text != "pong" {
		t.Fatalf("turn[1] = %+v", turns[1])
	}
	if used, window := m.Usage("n1"); used != 42 || window != 1000 {
		t.Fatalf("usage = %d/%d", used, window)
	}
	if m.LastError("n1") != "" {
		t.Fatalf("unexpected last error: %q", m.LastError("n1"))
	}
}

func TestManagerInterruptCancelsActiveTurn(t *testing.T) {
	m, _, ms := newManagerWithMock(t)
	launch(t, m, ms, "n1", "", "")
	if err := m.Send("n1", "long"); err != nil {
		t.Fatalf("send: %v", err)
	}
	r := ms.nextReq(t)
	if r.Method != "turn/start" {
		t.Fatalf("turn/start = %q", r.Method)
	}
	ms.reply(t, r.ID, `{"turn":{"id":"turn-1"}}`)
	waitFor(t, func() bool { return m.Live("n1") == "active" })
	if err := m.Interrupt("n1"); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	// The interrupt must reach the server as turn/interrupt for the live
	// turn — cancelling only the local wait would leave codex running the
	// turn server-side while scimux reports it as over.
	ir := ms.nextReq(t)
	if ir.Method != "turn/interrupt" {
		t.Fatalf("interrupt request = %q, want turn/interrupt", ir.Method)
	}
	if !strings.Contains(string(ir.Params), `"turnId":"turn-1"`) {
		t.Fatalf("turn/interrupt params = %s", ir.Params)
	}
	ms.reply(t, ir.ID, `{}`)
	ms.note(t, "turn/completed", `{"turn":{"id":"turn-1","status":"interrupted"}}`)
	waitFor(t, func() bool { return m.Live("n1") == "quiet" && m.LastError("n1") != "" })
	if got := m.LastError("n1"); !strings.Contains(got, "interrupted") {
		t.Fatalf("last error = %q, want interrupt notice", got)
	}
}

func TestManagerInterruptUnblocksPendingApproval(t *testing.T) {
	m, _, ms := newManagerWithMock(t)
	launch(t, m, ms, "n1", "", "")
	if err := m.Send("n1", "risky"); err != nil {
		t.Fatalf("send: %v", err)
	}
	r := ms.nextReq(t)
	ms.reply(t, r.ID, `{"turn":{"id":"turn-1"}}`)
	ms.serverRequest(t, 7, "item/commandExecution/requestApproval", syntheticApproval)
	waitFor(t, func() bool { return m.Attention("n1") == "approval" })

	if err := m.Interrupt("n1"); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	ir := ms.nextReq(t)
	if ir.Method != "turn/interrupt" {
		t.Fatalf("interrupt request = %q", ir.Method)
	}
	ms.reply(t, ir.ID, `{}`)
	ms.note(t, "turn/completed", `{"turn":{"id":"turn-1","status":"interrupted"}}`)

	// The dead approval is failed closed (client answers with an error
	// response) and attention clears instead of sticking to a moot question.
	dec := ms.nextResp(t)
	if len(dec.Error) == 0 {
		t.Fatalf("stale approval should be rejected, got result %s", dec.Result)
	}
	waitFor(t, func() bool { return m.Attention("n1") == "" && m.Live("n1") == "quiet" })
}

func TestManagerRejectsSecondTurn(t *testing.T) {
	m, _, ms := newManagerWithMock(t)
	launch(t, m, ms, "n1", "", "")
	if err := m.Send("n1", "first"); err != nil {
		t.Fatalf("send: %v", err)
	}
	r := ms.nextReq(t)
	ms.reply(t, r.ID, `{"turn":{}}`) // accepted, never completed
	if err := m.Send("n1", "second"); err != ErrTurnActive {
		t.Fatalf("want ErrTurnActive, got %v", err)
	}
}

func TestManagerApprovalResolve(t *testing.T) {
	m, _, ms := newManagerWithMock(t)
	launch(t, m, ms, "n1", "", "")
	if err := m.Send("n1", "fetch"); err != nil {
		t.Fatalf("send: %v", err)
	}
	ms.reply(t, ms.nextReq(t).ID, `{"turn":{}}`) // turn/start
	ms.serverRequest(t, 0, "item/commandExecution/requestApproval", syntheticApproval)

	// The approval becomes visible as pending attention with rendered options.
	waitFor(t, func() bool { return m.Attention("n1") == "approval" })
	title, opts, ok := m.Pending("n1")
	if !ok || len(opts) != 3 || title == "" {
		t.Fatalf("pending = %q %+v %v", title, opts, ok)
	}

	// Answer "1" → the first decision ("accept"): map, then deliver.
	optID, evidence, err := m.PrepareResolve("n1", "1")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !strings.HasPrefix(evidence, "permission: ") {
		t.Fatalf("evidence = %q", evidence)
	}
	if err := m.Deliver("n1", optID); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	dec := ms.nextResp(t)
	if !strings.Contains(string(dec.Result), `"decision":"accept"`) {
		t.Fatalf("decision result = %s", dec.Result)
	}
	ms.note(t, "turn/completed", `{"turn":{}}`)
	waitFor(t, func() bool { return m.Attention("n1") == "" && m.Live("n1") == "quiet" })
}

func TestManagerFileChangeApprovalResolve(t *testing.T) {
	m, _, ms := newManagerWithMock(t)
	launch(t, m, ms, "n1", "", "")
	if err := m.Send("n1", "edit"); err != nil {
		t.Fatalf("send: %v", err)
	}
	ms.reply(t, ms.nextReq(t).ID, `{"turn":{}}`) // turn/start
	ms.serverRequest(t, 0, "item/fileChange/requestApproval", `{"threadId":"THREAD-1","turnId":"turn-1","itemId":"patch-1","startedAtMs":1,"reason":"write file","grantRoot":"/w"}`)

	waitFor(t, func() bool { return m.Attention("n1") == "approval" })
	title, opts, ok := m.Pending("n1")
	if !ok || title != "item/fileChange/requestApproval" {
		t.Fatalf("pending = %q %+v %v", title, opts, ok)
	}
	if got := optionNames(opts); strings.Join(got, ",") != "accept,acceptForSession,decline,cancel" {
		t.Fatalf("file-change options = %v", got)
	}

	optID, _, err := m.PrepareResolve("n1", "2")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := m.Deliver("n1", optID); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	dec := ms.nextResp(t)
	if !strings.Contains(string(dec.Result), `"decision":"acceptForSession"`) {
		t.Fatalf("decision result = %s", dec.Result)
	}
}

func TestManagerQueuesConcurrentApprovals(t *testing.T) {
	m, _, ms := newManagerWithMock(t)
	launch(t, m, ms, "n1", "", "")
	if err := m.Send("n1", "two approvals"); err != nil {
		t.Fatalf("send: %v", err)
	}
	ms.reply(t, ms.nextReq(t).ID, `{"turn":{}}`) // turn/start
	ms.serverRequest(t, 1, "item/commandExecution/requestApproval", syntheticApproval)

	waitFor(t, func() bool {
		_, opts, ok := m.Pending("n1")
		return ok && len(opts) == 3
	})
	ms.serverRequest(t, 2, "item/fileChange/requestApproval", `{"threadId":"THREAD-1","turnId":"turn-1","itemId":"patch-1","startedAtMs":1}`)

	optID, _, err := m.PrepareResolve("n1", "1")
	if err != nil {
		t.Fatalf("prepare first: %v", err)
	}
	if err := m.Deliver("n1", optID); err != nil {
		t.Fatalf("deliver first: %v", err)
	}
	first := ms.nextResp(t)
	if !strings.Contains(string(first.Result), `"decision":"accept"`) {
		t.Fatalf("first decision = %s", first.Result)
	}

	waitFor(t, func() bool {
		title, opts, ok := m.Pending("n1")
		return ok && title == "item/fileChange/requestApproval" && len(opts) == 4
	})
	optID, _, err = m.PrepareResolve("n1", "4")
	if err != nil {
		t.Fatalf("prepare second: %v", err)
	}
	if err := m.Deliver("n1", optID); err != nil {
		t.Fatalf("deliver second: %v", err)
	}
	second := ms.nextResp(t)
	if !strings.Contains(string(second.Result), `"decision":"cancel"`) {
		t.Fatalf("second decision = %s", second.Result)
	}
}

func TestManagerNoPendingResolve(t *testing.T) {
	m, _, ms := newManagerWithMock(t)
	launch(t, m, ms, "n1", "", "")
	if _, _, err := m.PrepareResolve("n1", "1"); err != ErrNoPending {
		t.Fatalf("want ErrNoPending, got %v", err)
	}
	if _, _, err := m.PrepareResolve("missing", "1"); err != ErrNoSession {
		t.Fatalf("want ErrNoSession, got %v", err)
	}
}

func TestManagerExitedOnTransportClose(t *testing.T) {
	m, mt, ms := newManagerWithMock(t)
	launch(t, m, ms, "n1", "", "")
	// The subprocess dies: closing the transport ends the client read loop.
	_ = mt.Close()
	waitFor(t, func() bool { return m.Live("n1") == "exited" })
	if err := m.Send("n1", "x"); err != ErrNotAlive {
		t.Fatalf("want ErrNotAlive after exit, got %v", err)
	}
}

func TestManagerEmptyTurnFlagged(t *testing.T) {
	m, _, ms := newManagerWithMock(t)
	launch(t, m, ms, "n1", "", "")
	if err := m.Send("n1", "ping"); err != nil {
		t.Fatalf("send: %v", err)
	}
	ms.reply(t, ms.nextReq(t).ID, `{"turn":{}}`) // turn/start
	ms.note(t, "turn/completed", `{"turn":{}}`)  // completes with no assistant/tool output

	waitFor(t, func() bool { return m.Live("n1") == "quiet" && m.LastError("n1") != "" })
	if got := m.LastError("n1"); got != "agent produced no output this turn" {
		t.Fatalf("empty-turn error = %q", got)
	}
	if !strings.Contains(m.Peek("n1"), "error") {
		t.Fatalf("peek should surface the empty-turn error: %s", m.Peek("n1"))
	}
}

func TestManagerTurnFailed(t *testing.T) {
	m, _, ms := newManagerWithMock(t)
	launch(t, m, ms, "n1", "", "")
	if err := m.Send("n1", "ping"); err != nil {
		t.Fatalf("send: %v", err)
	}
	ms.reply(t, ms.nextReq(t).ID, `{"turn":{}}`)
	ms.note(t, "turn/failed", `{"message":"boom"}`)

	waitFor(t, func() bool { return m.Live("n1") == "quiet" && strings.Contains(m.LastError("n1"), "turn failed") })
}

func TestManagerKillForgetsSession(t *testing.T) {
	m, _, ms := newManagerWithMock(t)
	launch(t, m, ms, "n1", "", "")
	if !m.HasSession("n1") {
		t.Fatal("expected a live session after launch")
	}
	if err := m.Kill("n1"); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if m.HasSession("n1") {
		t.Fatal("session should be forgotten after Kill")
	}
	if m.Live("n1") != "exited" {
		t.Fatal("killed node should read exited")
	}
	// Kill of an unknown node is a no-op, and Shutdown is safe to call.
	if err := m.Kill("ghost"); err != nil {
		t.Fatalf("kill ghost: %v", err)
	}
	m.Shutdown()
}

func TestManagerRecordStartFailure(t *testing.T) {
	m, _, ms := newManagerWithMock(t)
	launch(t, m, ms, "n1", "", "")
	if err := m.RecordStartFailure("n1", context.DeadlineExceeded); err != nil {
		t.Fatalf("record start failure: %v", err)
	}
	if got := m.LastError("n1"); !strings.Contains(got, "first prompt not delivered") {
		t.Fatalf("last error = %q", got)
	}
	if !strings.Contains(m.Peek("n1"), "first prompt not delivered") {
		t.Fatalf("peek missing start failure: %s", m.Peek("n1"))
	}
}

func TestManagerSendNoSession(t *testing.T) {
	m := NewManagerWithSpawn(t.TempDir(), nil)
	if err := m.Send("ghost", "x"); err != ErrNoSession {
		t.Fatalf("want ErrNoSession, got %v", err)
	}
	if m.HasSession("ghost") {
		t.Fatal("ghost should have no session")
	}
	if m.Live("ghost") != "exited" {
		t.Fatal("no-session node should read exited")
	}
}

func optionNames(opts []PermOption) []string {
	out := make([]string, len(opts))
	for i, o := range opts {
		out[i] = o.Name
	}
	return out
}
