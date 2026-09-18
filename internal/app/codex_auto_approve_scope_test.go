package app

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/acp/codex"
	"github.com/scimux/scimux/internal/sessionlog"
)

type scopedCodexFrame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

type scopedCodexServer struct {
	out   io.Writer
	mu    sync.Mutex
	reqs  chan scopedCodexFrame
	resps chan scopedCodexFrame
}

func newScopedCodexSpawn() (codex.SpawnFunc, <-chan *scopedCodexServer) {
	ready := make(chan *scopedCodexServer, 1)
	spawn := func(string, string) (codex.Transport, error) {
		serverR, clientW := io.Pipe()
		clientR, serverW := io.Pipe()
		server := &scopedCodexServer{
			out: serverW, reqs: make(chan scopedCodexFrame, 16), resps: make(chan scopedCodexFrame, 16),
		}
		go func() {
			defer serverW.Close()
			defer close(server.reqs)
			defer close(server.resps)
			scanner := bufio.NewScanner(serverR)
			for scanner.Scan() {
				var frame scopedCodexFrame
				if json.Unmarshal(scanner.Bytes(), &frame) != nil {
					continue
				}
				if frame.Method != "" {
					server.reqs <- frame
				} else {
					server.resps <- frame
				}
			}
		}()
		ready <- server
		return &fakeCodexTransport{stdin: clientW, stdout: clientR}, nil
	}
	return spawn, ready
}

func (s *scopedCodexServer) write(t *testing.T, raw string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := io.WriteString(s.out, raw+"\n"); err != nil {
		t.Fatalf("fake Codex write: %v", err)
	}
}

func (s *scopedCodexServer) reply(t *testing.T, req scopedCodexFrame, result string) {
	t.Helper()
	s.write(t, `{"id":`+string(req.ID)+`,"result":`+result+`}`)
}

func (s *scopedCodexServer) note(t *testing.T, method, params string) {
	t.Helper()
	s.write(t, `{"method":`+fmt.Sprintf("%q", method)+`,"params":`+params+`}`)
}

func (s *scopedCodexServer) request(t *testing.T, id, method, params string) {
	t.Helper()
	s.write(t, `{"id":`+fmt.Sprintf("%q", id)+`,"method":`+fmt.Sprintf("%q", method)+`,"params":`+params+`}`)
}

func nextScopedFrame(t *testing.T, ch <-chan scopedCodexFrame, what string) scopedCodexFrame {
	t.Helper()
	select {
	case frame, ok := <-ch:
		if !ok {
			t.Fatalf("fake Codex %s stream closed", what)
		}
		return frame
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for fake Codex %s", what)
		return scopedCodexFrame{}
	}
}

func (s *scopedCodexServer) barrier(t *testing.T, id string) {
	t.Helper()
	s.request(t, id, "currentTime/read", `{}`)
	resp := nextScopedFrame(t, s.resps, "barrier response")
	if string(resp.ID) != fmt.Sprintf("%q", id) {
		t.Fatalf("response before barrier %q had id %s", id, resp.ID)
	}
}

func awaitScopedPending(t *testing.T, m codexManager, nodeID string) PendingPermission {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if pending, ok := m.Pending(nodeID); ok {
			return pending
		}
		select {
		case <-deadline.C:
			t.Fatal("timed out waiting for Codex pending approval")
		case <-tick.C:
		}
	}
}

func awaitScopedLive(t *testing.T, m codexManager, nodeID, want string) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for m.Live(nodeID) != want {
		select {
		case <-deadline.C:
			t.Fatalf("Codex live = %q, want %q", m.Live(nodeID), want)
		case <-tick.C:
		}
	}
}

func launchScopedCodex(t *testing.T) (*app, *Node, codexManager, *scopedCodexServer) {
	t.Helper()
	a := newTestApp(t, &fakeTmux{})
	spawn, ready := newScopedCodexSpawn()
	m := codexManager{codex.NewManagerWithSpawn(a.sessionsDir, spawn)}
	a.codex = m
	t.Cleanup(m.Shutdown)
	n := &Node{ID: "codex-scope", Title: "Codex scope", Agent: "codex", Transport: "codex", Dir: a.home, CreatedAt: "2026-09-16T00:00:00Z"}
	a.nodes = append(a.nodes, n)
	a.byID[n.ID] = n

	launched := make(chan error, 1)
	go func() {
		_, err := m.Launch(n.ID, n.Agent, n.Dir, "", "")
		launched <- err
	}()
	server := <-ready
	initialize := nextScopedFrame(t, server.reqs, "initialize")
	if initialize.Method != "initialize" {
		t.Fatalf("request = %q, want initialize", initialize.Method)
	}
	server.reply(t, initialize, `{}`)
	thread := nextScopedFrame(t, server.reqs, "thread/start")
	if thread.Method != "thread/start" {
		t.Fatalf("request = %q, want thread/start", thread.Method)
	}
	server.reply(t, thread, `{"thread":{"id":"PARENT-THREAD","path":"/synthetic/rollout.jsonl"},"approvalPolicy":"on-request"}`)
	if err := <-launched; err != nil {
		t.Fatalf("launch: %v", err)
	}
	return a, n, m, server
}

func startScopedTurn(t *testing.T, m codexManager, server *scopedCodexServer, nodeID, turnID string) {
	t.Helper()
	if err := m.Send(nodeID, "parent work"); err != nil {
		t.Fatal(err)
	}
	start := nextScopedFrame(t, server.reqs, "turn/start")
	if start.Method != "turn/start" {
		t.Fatalf("request = %q, want turn/start", start.Method)
	}
	server.reply(t, start, `{"turn":{"id":`+fmt.Sprintf("%q", turnID)+`}}`)
	awaitScopedLive(t, m, nodeID, "active")
}

func TestCodexChildLifecycleCannotDisarmAutoApprove(t *testing.T) {
	a, n, m, server := launchScopedCodex(t)
	startScopedTurn(t, m, server, n.ID, "parent-turn")
	a.mu.Lock()
	a.live[n.ID] = "active"
	a.mu.Unlock()
	view := a.enableAutoApprove(n.ID, m)
	if !view.Enabled || view.Phase != string(autoPhaseArmed) {
		t.Fatalf("enable = %+v", view)
	}

	server.request(t, "approval-1", "item/commandExecution/requestApproval",
		`{"threadId":"PARENT-THREAD","turnId":"parent-turn","command":"synthetic action","reason":"test","availableDecisions":["accept","cancel"]}`)
	pending := awaitScopedPending(t, m, n.ID)

	server.note(t, "turn/started", `{"threadId":"CHILD-THREAD","turn":{"id":"child-turn"}}`)
	server.note(t, "turn/completed", `{"threadId":"CHILD-THREAD","turn":{"id":"child-turn"}}`)
	server.note(t, "turn/failed", `{"threadId":"CHILD-THREAD","turnId":"child-turn","message":"child failed"}`)
	server.barrier(t, "child-events-processed")
	if got := m.Live(n.ID); got != "active" {
		t.Fatalf("child lifecycle changed manager live to %q", got)
	}
	if got, ok := m.Pending(n.ID); !ok || got.RequestID != pending.RequestID {
		t.Fatalf("child completion canceled pending approval: %+v, %v", got, ok)
	}
	a.mu.Lock()
	armed := a.autoApproveViewOf(n)
	a.mu.Unlock()
	if !armed.Enabled || armed.Phase != string(autoPhaseArmed) {
		t.Fatalf("child lifecycle disarmed lease: %+v", armed)
	}

	// The normal poll path sees the real manager's still-active parent and
	// resolves its existing queue through audit-before-delivery.
	a.poll()
	decision := nextScopedFrame(t, server.resps, "automatic approval response")
	if string(decision.ID) != `"approval-1"` || !strings.Contains(string(decision.Result), `"decision":"accept"`) {
		t.Fatalf("automatic reply = id:%s result:%s error:%s", decision.ID, decision.Result, decision.Error)
	}
	server.barrier(t, "approval-delivered")
	select {
	case extra := <-server.resps:
		t.Fatalf("more than one reply delivered for approval: %+v", extra)
	default:
	}

	var decisions []sessionlog.DecisionEvent
	for _, event := range sessionlog.ReadEvents(a.sessionLogPath(n.ID)) {
		if event.T == "decision" && event.Decision != nil && event.Decision.Source == "auto" {
			decisions = append(decisions, *event.Decision)
		}
	}
	if len(decisions) != 1 || decisions[0].RequestID != pending.RequestID || decisions[0].Selected.Kind != "allow" {
		t.Fatalf("automatic decision audit = %+v", decisions)
	}
	a.mu.Lock()
	armed = a.autoApproveViewOf(n)
	a.mu.Unlock()
	if armed.Count != 1 || armed.Phase != string(autoPhaseArmed) {
		t.Fatalf("lease after automatic decision = %+v", armed)
	}

	server.note(t, "item/completed", `{"item":{"type":"agentMessage","text":"parent done"}}`)
	server.note(t, "turn/completed", `{"threadId":"PARENT-THREAD","turnId":"parent-turn"}`)
	awaitScopedLive(t, m, n.ID, "quiet")
	a.poll()
	a.mu.Lock()
	off := a.autoApproveViewOf(n)
	a.mu.Unlock()
	if off.Enabled || off.Phase != string(autoPhaseOff) {
		t.Fatalf("actual parent completion did not disarm lease: %+v", off)
	}

	// A later parent turn starts with the old one-turn lease still off.
	startScopedTurn(t, m, server, n.ID, "next-parent-turn")
	server.request(t, "approval-next", "item/commandExecution/requestApproval",
		`{"threadId":"PARENT-THREAD","turnId":"next-parent-turn","command":"next action","availableDecisions":["accept","cancel"]}`)
	nextPending := awaitScopedPending(t, m, n.ID)
	a.poll()
	server.barrier(t, "next-turn-remains-manual")
	if got, ok := m.Pending(n.ID); !ok || got.RequestID != nextPending.RequestID {
		t.Fatalf("subsequent turn inherited old lease: %+v, %v", got, ok)
	}

	// Clean up the synthetic pending request through the unchanged manual path.
	token, _, err := m.PrepareResolve(n.ID, nextPending.RequestID, "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Deliver(n.ID, token); err != nil {
		t.Fatal(err)
	}
	manual := nextScopedFrame(t, server.resps, "manual approval response")
	if string(manual.ID) != `"approval-next"` {
		t.Fatalf("manual reply id = %s", manual.ID)
	}
	server.note(t, "item/completed", `{"item":{"type":"agentMessage","text":"next done"}}`)
	server.note(t, "turn/completed", `{"threadId":"PARENT-THREAD","turnId":"next-parent-turn"}`)
	awaitScopedLive(t, m, n.ID, "quiet")
}

func TestCodexAutoApproveQueueFencesRemainManual(t *testing.T) {
	a, n, m, server := launchScopedCodex(t)
	startScopedTurn(t, m, server, n.ID, "parent-turn")
	a.mu.Lock()
	a.live[n.ID] = "active"
	a.mu.Unlock()

	// A request already pending when enable occurs is below the manager's
	// sequence watermark and remains manual.
	server.request(t, "pre-enable", "item/commandExecution/requestApproval",
		`{"command":"already pending","availableDecisions":["accept","cancel"]}`)
	pre := awaitScopedPending(t, m, n.ID)
	if view := a.enableAutoApprove(n.ID, m); !view.Enabled || view.Phase != string(autoPhaseArmed) {
		t.Fatalf("enable = %+v", view)
	}
	a.poll()
	server.barrier(t, "pre-enable-still-manual")
	if got, ok := m.Pending(n.ID); !ok || got.RequestID != pre.RequestID {
		t.Fatalf("pre-enable request was not kept manual: %+v, %v", got, ok)
	}
	token, _, err := m.PrepareResolve(n.ID, pre.RequestID, "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Deliver(n.ID, token); err != nil {
		t.Fatal(err)
	}
	if resp := nextScopedFrame(t, server.resps, "pre-enable manual response"); string(resp.ID) != `"pre-enable"` {
		t.Fatalf("manual response id = %s", resp.ID)
	}

	// Persistent-only grants stay manual; they cannot consume the armed lease.
	server.request(t, "persistent", "item/commandExecution/requestApproval",
		`{"command":"persistent only","availableDecisions":["acceptForSession","cancel"]}`)
	persistent := awaitScopedPending(t, m, n.ID)
	a.poll()
	server.barrier(t, "persistent-still-manual")
	if got, ok := m.Pending(n.ID); !ok || got.RequestID != persistent.RequestID {
		t.Fatalf("persistent request was not kept manual: %+v, %v", got, ok)
	}
	token, _, err = m.PrepareResolve(n.ID, persistent.RequestID, "2")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Deliver(n.ID, token); err != nil {
		t.Fatal(err)
	}
	if resp := nextScopedFrame(t, server.resps, "persistent manual response"); string(resp.ID) != `"persistent"` || !strings.Contains(string(resp.Result), "cancel") {
		t.Fatalf("persistent response = id:%s result:%s", resp.ID, resp.Result)
	}

	// Two queued eligible requests retain exact identities and each receive one
	// independently prepared decision; the second cannot consume the first's.
	server.request(t, "queued-a", "item/commandExecution/requestApproval",
		`{"command":"queued a","availableDecisions":["accept","cancel"]}`)
	first := awaitScopedPending(t, m, n.ID)
	server.request(t, "queued-b", "item/commandExecution/requestApproval",
		`{"command":"queued b","availableDecisions":["acceptForSession","accept","cancel"]}`)
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		_, max, ok := m.PermissionBoundary(n.ID)
		if ok && max >= 4 {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("second queued request was not registered")
		case <-time.After(time.Millisecond):
		}
	}
	a.poll()
	respA := nextScopedFrame(t, server.resps, "queued-a response")
	if string(respA.ID) != `"queued-a"` || !strings.Contains(string(respA.Result), `"decision":"accept"`) {
		t.Fatalf("queued-a response = id:%s result:%s", respA.ID, respA.Result)
	}
	second := awaitScopedPending(t, m, n.ID)
	if second.RequestID == first.RequestID {
		t.Fatal("queue head did not advance to the second exact request")
	}
	a.poll()
	respB := nextScopedFrame(t, server.resps, "queued-b response")
	if string(respB.ID) != `"queued-b"` || !strings.Contains(string(respB.Result), `"decision":"accept"`) {
		t.Fatalf("queued-b response = id:%s result:%s", respB.ID, respB.Result)
	}
	server.barrier(t, "queued-deliveries-finished")
	select {
	case extra := <-server.resps:
		t.Fatalf("queued request received duplicate/crossed reply: %+v", extra)
	default:
	}

	server.note(t, "item/completed", `{"item":{"type":"agentMessage","text":"done"}}`)
	server.note(t, "turn/completed", `{"threadId":"PARENT-THREAD","turnId":"parent-turn"}`)
	awaitScopedLive(t, m, n.ID, "quiet")
}

func TestCodexParentFailureAndInterruptStillDisarmAutoApprove(t *testing.T) {
	t.Run("failure", func(t *testing.T) {
		a, n, m, server := launchScopedCodex(t)
		startScopedTurn(t, m, server, n.ID, "failed-parent")
		a.mu.Lock()
		a.live[n.ID] = "active"
		a.mu.Unlock()
		if view := a.enableAutoApprove(n.ID, m); view.Phase != string(autoPhaseArmed) {
			t.Fatalf("enable = %+v", view)
		}
		server.note(t, "turn/failed", `{"threadId":"PARENT-THREAD","turnId":"failed-parent","message":"synthetic failure"}`)
		awaitScopedLive(t, m, n.ID, "quiet")
		a.poll()
		a.mu.Lock()
		view := a.autoApproveViewOf(n)
		a.mu.Unlock()
		if view.Enabled || view.Phase != string(autoPhaseOff) {
			t.Fatalf("parent failure left lease enabled: %+v", view)
		}
	})

	t.Run("interrupt", func(t *testing.T) {
		a, n, m, server := launchScopedCodex(t)
		startScopedTurn(t, m, server, n.ID, "interrupted-parent")
		a.mu.Lock()
		a.live[n.ID] = "active"
		a.mu.Unlock()
		if view := a.enableAutoApprove(n.ID, m); view.Phase != string(autoPhaseArmed) {
			t.Fatalf("enable = %+v", view)
		}
		if err := m.Interrupt(n.ID); err != nil {
			t.Fatal(err)
		}
		interrupt := nextScopedFrame(t, server.reqs, "turn/interrupt")
		if interrupt.Method != "turn/interrupt" || !strings.Contains(string(interrupt.Params), `"threadId":"PARENT-THREAD"`) ||
			!strings.Contains(string(interrupt.Params), `"turnId":"interrupted-parent"`) {
			t.Fatalf("interrupt = %q %s", interrupt.Method, interrupt.Params)
		}
		server.reply(t, interrupt, `{}`)
		server.note(t, "turn/completed", `{"threadId":"PARENT-THREAD","turnId":"interrupted-parent","status":"interrupted"}`)
		awaitScopedLive(t, m, n.ID, "quiet")
		a.poll()
		a.mu.Lock()
		view := a.autoApproveViewOf(n)
		a.mu.Unlock()
		if view.Enabled || view.Phase != string(autoPhaseOff) {
			t.Fatalf("parent interrupt left lease enabled: %+v", view)
		}
	})
}
