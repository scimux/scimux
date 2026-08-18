package app

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// One-turn auto-approve for every agent, including Claude. Enabling expresses
// intent for the current turn only. The lease ends on:
//
//	a) interrupt ("stop")
//	b) manual un-toggle
//	c) /clear and /exit
//	d) turn completion (Claude Stop hook or transcript end_turn; ACP/Codex
//	   protocol edges)
//	+  process/session loss and node delete (non-negotiable, not human acts)
//
// Claude's turn boundary is not pane quietness. A permission wait is also
// quiet, so mid-turn silence must leave the lease armed until Stop / end_turn.

// leaseIDOf reads the current lease id ("" when off).
func leaseIDOf(a *app, id string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if st := a.autoApprove[id]; st != nil {
		return st.LeaseID
	}
	return ""
}

// endTmuxTurn drives one poll tick whose pane has gone static past
// paneQuietAfter — the mechanical active → quiet turn boundary.
func endTmuxTurn(t *testing.T, a *app, id string) {
	t.Helper()
	// Sync prevCap to whatever the pane currently shows, so the next tick sees
	// no change regardless of what the test did before (a send, say).
	a.poll()
	a.mu.Lock()
	a.live[id] = "active"
	a.lastChg[id] = time.Now().Add(-2 * paneQuietAfter)
	a.mu.Unlock()
	a.poll()
	a.mu.Lock()
	live := a.live[id]
	a.mu.Unlock()
	if live != "quiet" {
		t.Fatalf("live = %q, want quiet (test setup)", live)
	}
}

func TestTurnEndDisarmsTmuxLease(t *testing.T) {
	// Claude matches every other agent: a completed turn turns the toggle off.
	// Retracting the marker at the boundary keeps the hook inert.
	f := &fakeTmux{alive: map[string]bool{"t1": true}, list: []string{"t1"}, capture: "idle pane"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "t1", hookSIDOwn)

	a.mu.Lock()
	a.live[n.ID] = "active"
	a.mu.Unlock()
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)
	if _, ok := markerLease(t, bundle); !ok {
		t.Fatal("precondition: armed lease must publish a marker")
	}

	endTmuxTurn(t, a, n.ID)

	if got := phaseOf(a, n.ID); got != autoPhaseOff {
		t.Fatalf("phase after the turn ended = %q, want off", got)
	}
	a.mu.Lock()
	view := a.autoApproveViewOf(n)
	a.mu.Unlock()
	if view.Enabled {
		t.Fatal("turn end must switch the Claude toggle off, like every other agent")
	}
	if lease, ok := markerLease(t, bundle); ok {
		t.Fatalf("turn end left the arm marker behind (%q); the hook must go inert", lease)
	}
}

func TestTmuxTurnEndRequiresRetoggleForNextTurn(t *testing.T) {
	// prompt → turn end → next prompt does not re-arm. The human must enable
	// again, exactly as on grok/opencode/pi/codex.
	f := &fakeTmux{alive: map[string]bool{"t2": true}, list: []string{"t2"}, capture: "idle pane", captureAfterEnter: "working"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "t2", hookSIDOwn)
	h := newTestHandler(t, a)

	if v := a.enableAutoApprove(n.ID, nil); !v.Enabled {
		t.Fatalf("enable: %+v", v)
	}
	rec := routeRequest(h, http.MethodPost, "/api/nodes/t2/send", `{"text":"go"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("first send: status = %d body %q", rec.Code, rec.Body.String())
	}
	if got := phaseOf(a, n.ID); got != autoPhaseArmed {
		t.Fatalf("phase after first prompt = %q, want armed", got)
	}
	if _, ok := markerLease(t, bundle); !ok {
		t.Fatal("armed lease published no marker")
	}
	endTmuxTurn(t, a, n.ID)
	if got := phaseOf(a, n.ID); got != autoPhaseOff {
		t.Fatalf("phase after turn end = %q, want off", got)
	}

	rec = routeRequest(h, http.MethodPost, "/api/nodes/t2/send", `{"text":"again"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("second send: status = %d body %q", rec.Code, rec.Body.String())
	}
	if got := phaseOf(a, n.ID); got != autoPhaseOff {
		t.Fatalf("phase after next prompt without retoggle = %q, want off", got)
	}
	if lease, ok := markerLease(t, bundle); ok {
		t.Fatalf("next prompt re-armed the marker (%q) without a new enable", lease)
	}
}

func TestTurnEndClearsThePromptIDFence(t *testing.T) {
	// TurnPromptID latches the turn a lease has committed to. Turn end deletes
	// the lease, so the next enable starts with a fresh fence.
	f := &fakeTmux{alive: map[string]bool{"t3": true}, list: []string{"t3"}, capture: "idle pane"}
	a := newTestApp(t, f)
	n, _ := seedPermClaude(t, a, "t3", hookSIDOwn)

	a.mu.Lock()
	a.live[n.ID] = "active"
	a.mu.Unlock()
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)
	a.mu.Lock()
	a.autoApprove[n.ID].TurnPromptID = "prompt-1"
	a.autoApprove[n.ID].Count = 4
	a.mu.Unlock()

	endTmuxTurn(t, a, n.ID)

	a.mu.Lock()
	st := a.autoApprove[n.ID]
	a.mu.Unlock()
	if st != nil {
		t.Fatalf("turn end must delete the lease, still have %+v", st)
	}
}

func TestPaneExitDisarmsLeaseOutright(t *testing.T) {
	// Process/session loss is a hard reset, not a turn boundary: there is no
	// successor turn to prime for.
	f := &fakeTmux{alive: map[string]bool{}, list: []string{}}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "x1", hookSIDOwn)

	a.mu.Lock()
	a.live[n.ID] = "active"
	a.mu.Unlock()
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)

	a.poll()

	a.mu.Lock()
	live := a.live[n.ID]
	a.mu.Unlock()
	if live != "exited" {
		t.Fatalf("live = %q, want exited (test setup)", live)
	}
	if got := phaseOf(a, n.ID); got != autoPhaseOff {
		t.Fatalf("phase after process loss = %q, want off", got)
	}
	if lease, ok := markerLease(t, bundle); ok {
		t.Fatalf("process loss left the arm marker behind (%q)", lease)
	}
}

func TestStructuredTurnEndDisarmsLease(t *testing.T) {
	// Codex and every ACP agent are deliberately one-turn controls. Their
	// protocol turn boundary is exact, so completion turns the toggle off.
	for _, tc := range []struct{ agent, transport string }{
		{"grok", "acp"}, {"opencode", "acp"}, {"pi", "acp"}, {"codex", "codex"},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			a := newTestApp(t, &fakeTmux{})
			n := seedStructuredNode(t, a, "s-"+tc.agent, tc.agent, tc.transport)
			stub := &stubProc{live: "active", hasSession: true}
			a.testProc = stub
			a.mu.Lock()
			a.live[n.ID] = "active"
			a.mu.Unlock()
			a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)
			if phaseOf(a, n.ID) != autoPhaseArmed {
				t.Fatal("precondition: enable during an active turn arms")
			}

			stub.mu.Lock()
			stub.live = "quiet"
			stub.mu.Unlock()
			a.poll()

			if got := phaseOf(a, n.ID); got != autoPhaseOff {
				t.Fatalf("phase after turn end = %q, want off", got)
			}
			a.mu.Lock()
			enabled := a.autoApproveViewOf(n).Enabled
			a.mu.Unlock()
			if enabled {
				t.Fatal("turn end must switch the structured-agent toggle off")
			}
		})
	}
}

func TestStructuredExitDisarmsLease(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "sx", "grok", "acp")
	stub := &stubProc{live: "active", hasSession: true}
	a.testProc = stub
	a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)

	stub.mu.Lock()
	stub.live = "exited"
	stub.mu.Unlock()
	a.poll()

	if got := phaseOf(a, n.ID); got != autoPhaseOff {
		t.Fatalf("phase after process loss = %q, want off", got)
	}
}

func TestClearDisarmsLeaseForEveryStructuredAgent(t *testing.T) {
	// Rule (c), pinned per agent: /clear is a page turn and ends the lease on
	// grok, opencode, pi and codex alike. The shared handler already does this;
	// this test exists so it stays true per transport.
	for _, tc := range []struct{ agent, transport string }{
		{"grok", "acp"}, {"opencode", "acp"}, {"pi", "acp"}, {"codex", "codex"},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			a := newTestApp(t, &fakeTmux{})
			n := seedStructuredNode(t, a, "c-"+tc.agent, tc.agent, tc.transport)
			a.testProc = &stubProc{live: "quiet", hasSession: true}
			h := newTestHandler(t, a)
			a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)

			rec := routeRequest(h, http.MethodPost, "/api/nodes/"+n.ID+"/send", `{"text":"/clear"}`, true)
			if rec.Code != http.StatusOK {
				t.Fatalf("/clear: status = %d body %q", rec.Code, rec.Body.String())
			}
			if got := phaseOf(a, n.ID); got != autoPhaseOff {
				t.Fatalf("phase after /clear = %q, want off", got)
			}
		})
	}
}

func TestExitDisarmsLeaseForEveryStructuredAgent(t *testing.T) {
	// Rule (c), the other half.
	for _, tc := range []struct{ agent, transport string }{
		{"grok", "acp"}, {"opencode", "acp"}, {"pi", "acp"}, {"codex", "codex"},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			a := newTestApp(t, &fakeTmux{})
			n := seedStructuredNode(t, a, "e-"+tc.agent, tc.agent, tc.transport)
			a.testProc = &stubProc{live: "quiet", hasSession: true}
			h := newTestHandler(t, a)
			a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)

			rec := routeRequest(h, http.MethodPost, "/api/nodes/"+n.ID+"/exit", "", true)
			if rec.Code != http.StatusOK {
				t.Fatalf("/exit: status = %d body %q", rec.Code, rec.Body.String())
			}
			if got := phaseOf(a, n.ID); got != autoPhaseOff {
				t.Fatalf("phase after /exit = %q, want off", got)
			}
		})
	}
}

func TestInterruptDisarmsLease(t *testing.T) {
	// Rule (a): "stop" is the human taking the wheel back.
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "i1", "grok", "acp")
	a.testProc = &stubProc{live: "active", hasSession: true}
	h := newTestHandler(t, a)
	a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)

	rec := routeRequest(h, http.MethodPost, "/api/nodes/i1/send/interrupt", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("interrupt: status = %d body %q", rec.Code, rec.Body.String())
	}
	if got := phaseOf(a, n.ID); got != autoPhaseOff {
		t.Fatalf("phase after interrupt = %q, want off", got)
	}
}

func TestClaudeInterruptDisarmsStickyLease(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"ci1": true}, capture: "esc to interrupt"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "ci1", hookSIDOwn)
	a.mu.Lock()
	a.live[n.ID] = "active"
	a.mu.Unlock()
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)
	if _, ok := markerLease(t, bundle); !ok {
		t.Fatal("precondition: Claude lease marker was not armed")
	}

	h := newTestHandler(t, a)
	rec := routeRequest(h, http.MethodPost, "/api/nodes/ci1/send/interrupt", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("interrupt: status = %d body %q", rec.Code, rec.Body.String())
	}
	if got := phaseOf(a, n.ID); got != autoPhaseOff {
		t.Fatalf("Claude phase after interrupt = %q, want off", got)
	}
	if lease, ok := markerLease(t, bundle); ok {
		t.Fatalf("Claude interrupt left marker %q armed", lease)
	}
}

func TestManualDisableStillTurnsLeaseOff(t *testing.T) {
	// Rule (b): the toggle is still a toggle.
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "m1", "grok", "acp")
	a.testProc = &stubProc{live: "active", hasSession: true}
	a.setAutoApproveEnabled(n.ID, true, "active", "inc", 0)

	v := a.setAutoApproveEnabled(n.ID, false, "active", "", 0)
	if v.Enabled || v.Phase != string(autoPhaseOff) {
		t.Fatalf("manual disable = %+v, want off", v)
	}
}

// seedPermClaudeWithTranscript is seedPermClaude plus a bound transcript whose
// lines the caller supplies, so a test can put the node in a state the tailer
// can read (an unresolved tool call, say).
func seedPermClaudeWithTranscript(t *testing.T, a *app, id, sid string, lines ...string) (*Node, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), id+".jsonl")
	appendLines(t, path, lines...)
	n, bundle := seedPermClaude(t, a, id, sid)
	n.Transcript = path
	return n, bundle
}

// A tool call the agent has started and no result has come back for: the
// transcript state of `sleep 150 && date > d1.txt` while it runs.
const runningToolCall = `{"type":"assistant","timestamp":"t2","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{}}]}}`

func TestLongRunningToolCallDoesNotParkTmuxLease(t *testing.T) {
	// A quiet pane is not a finished turn while a call is unresolved. The case
	// this guards is an approval dialog with a second call queued behind it:
	// the dialog is static, the pane crosses paneQuietAfter (8s), and parking
	// there retracts the marker the queued call needs — the lease dies at the
	// moment it is wanted. Pane quietness alone cannot tell "turn over" from
	// "call pending"; the Stop hook and the transcript can.
	// the transcript can, and already does for turn_done.
	//
	// Scope honestly: a long *foreground* tool is not the motivating case.
	// Claude draws a ticking elapsed timer, so liveness stays "active" and this
	// branch never runs — see running_elapsed_time_stays_active_no_attention in
	// poller_test.go. This guard is reasoning, not a reproduced failure.
	//
	// Live probe case D (2026-08-18) was written to observe it and did not:
	// the probe host blocks foreground `sleep`, so its Claude backgrounded the
	// long command, the turn genuinely ended, and the pane went quiet with
	// nothing pending — parking was correct there. The probe's own error
	// (asserting a state it never reached) is recorded so the next attempt does
	// not repeat it. Until a probe reaches a queued call behind a dialog, this
	// test pins the intended rule, not a confirmed cure.
	f := &fakeTmux{alive: map[string]bool{"t1": true}, list: []string{"t1"}, capture: "idle pane"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaudeWithTranscript(t, a, "t1", hookSIDOwn,
		`{"type":"user","timestamp":"t1","message":{"role":"user","content":"run it"}}`,
		runningToolCall)

	a.mu.Lock()
	a.live[n.ID] = "active"
	a.mu.Unlock()
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)
	armedLease := leaseIDOf(a, n.ID)

	endTmuxTurn(t, a, n.ID)

	if got := phaseOf(a, n.ID); got != autoPhaseArmed {
		t.Fatalf("phase while a tool call is still unresolved = %q, want armed", got)
	}
	lease, ok := markerLease(t, bundle)
	if !ok {
		t.Fatal("the marker was retracted mid-turn; the turn's next call escalates to a dialog the human already authorized")
	}
	if lease != armedLease {
		t.Fatalf("marker lease = %q, want the unchanged armed lease %q", lease, armedLease)
	}
}

func TestResolvedToolCallDisarmsTmuxLease(t *testing.T) {
	// The other side of the same predicate: once the result is in, a quiet pane
	// with end_turn is a finished turn and the lease turns off. Without this
	// the mid-turn guard would simply never disarm.
	f := &fakeTmux{alive: map[string]bool{"t2": true}, list: []string{"t2"}, capture: "idle pane"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaudeWithTranscript(t, a, "t2", hookSIDOwn,
		`{"type":"user","timestamp":"t1","message":{"role":"user","content":"run it"}}`,
		runningToolCall,
		`{"type":"user","timestamp":"t3","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"done"}]}}`,
		`{"type":"assistant","timestamp":"t4","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"it is done"}]}}`)

	a.mu.Lock()
	a.live[n.ID] = "active"
	a.mu.Unlock()
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)

	endTmuxTurn(t, a, n.ID)

	if got := phaseOf(a, n.ID); got != autoPhaseOff {
		t.Fatalf("phase after a completed turn = %q, want off", got)
	}
	if lease, ok := markerLease(t, bundle); ok {
		t.Fatalf("completed turn left the arm marker behind (%q)", lease)
	}
}

func TestClaudeQuietMidTurnDoesNotParkStickyLease(t *testing.T) {
	// Reproduces the regression: a permission/question wait can make the pane
	// quiet before Claude flushes a call. User-role transcript state without an
	// explicit end_turn must keep the marker armed for later calls in this turn.
	f := &fakeTmux{alive: map[string]bool{"tq": true}, list: []string{"tq"}, capture: "waiting quietly"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaudeWithTranscript(t, a, "tq", hookSIDOwn,
		`{"type":"user","timestamp":"t1","message":{"role":"user","content":"do the work"}}`)
	a.mu.Lock()
	a.live[n.ID] = "active"
	a.mu.Unlock()
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)
	armedLease := leaseIDOf(a, n.ID)

	endTmuxTurn(t, a, n.ID)

	if got := phaseOf(a, n.ID); got != autoPhaseArmed {
		t.Fatalf("phase on a quiet mid-turn = %q, want armed", got)
	}
	if lease, ok := markerLease(t, bundle); !ok || lease != armedLease {
		t.Fatalf("quiet mid-turn marker = %q,%v, want unchanged %q", lease, ok, armedLease)
	}
}

func TestClaudeEnableDuringQuietOwingTurnArms(t *testing.T) {
	// "Auto-approve this turn" must apply to a turn whose approval dialog is
	// already up. The pane is quiet by design; owing (or a standing ask) is
	// the current-turn signal.
	f := &fakeTmux{alive: map[string]bool{"q1": true}, list: []string{"q1"}, capture: "waiting"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaudeWithTranscript(t, a, "q1", hookSIDOwn,
		`{"type":"user","timestamp":"t1","message":{"role":"user","content":"do the work"}}`)
	a.mu.Lock()
	a.live[n.ID] = "quiet"
	a.mu.Unlock()

	v := a.setAutoApproveEnabled(n.ID, true, "quiet", "", 0)
	if v.Phase != string(autoPhaseArmed) {
		t.Fatalf("phase during a quiet owing turn = %q, want armed", v.Phase)
	}
	if _, ok := markerLease(t, bundle); !ok {
		t.Fatal("quiet owing enable published no marker; the dialog cannot be answered")
	}
}

func TestClaudeEnableAfterEndTurnStaysPrimed(t *testing.T) {
	f := &fakeTmux{alive: map[string]bool{"q2": true}, list: []string{"q2"}, capture: "idle"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaudeWithTranscript(t, a, "q2", hookSIDOwn,
		`{"type":"user","timestamp":"t1","message":{"role":"user","content":"go"}}`,
		`{"type":"assistant","timestamp":"t2","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"done"}]}}`)
	a.mu.Lock()
	a.live[n.ID] = "quiet"
	a.mu.Unlock()

	v := a.setAutoApproveEnabled(n.ID, true, "quiet", "", 0)
	if v.Phase != string(autoPhasePrimed) {
		t.Fatalf("phase after a finished turn = %q, want primed", v.Phase)
	}
	if _, ok := markerLease(t, bundle); ok {
		t.Fatal("enable after end_turn must not publish an arm marker")
	}
}

func TestClaudeEnableDuringStandingAskArms(t *testing.T) {
	// The notice exists because Claude prints text and then asks before
	// flushing tool_use: newest record is assistant, Owing is false, and
	// PendingCount is 0. A tailer that returns there never sees the ask.
	f := &fakeTmux{alive: map[string]bool{"q3": true}, list: []string{"q3"}, capture: "dialog"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaudeWithTranscript(t, a, "q3", hookSIDOwn,
		`{"type":"user","timestamp":"t1","message":{"role":"user","content":"do the work"}}`,
		`{"type":"assistant","timestamp":"t2","message":{"role":"assistant","content":[{"type":"text","text":"I'll edit that"}]}}`)
	a.noteClaudeAskedCapability(a.claudeHookID(n.ID))
	askedNoticeAt(t, bundle, "ask1", "Bash", "deadbeefdeadbeef", time.Now().Add(-time.Minute))
	a.mu.Lock()
	a.live[n.ID] = "quiet"
	a.mu.Unlock()

	v := a.setAutoApproveEnabled(n.ID, true, "quiet", "", 0)
	if v.Phase != string(autoPhaseArmed) {
		t.Fatalf("phase with a standing ask = %q, want armed", v.Phase)
	}
	if _, ok := markerLease(t, bundle); !ok {
		t.Fatal("standing ask must publish the marker so the hook can answer")
	}
}

func TestClaudeEnableDuringAssistantTextWithoutAskStaysPrimed(t *testing.T) {
	// The same transcript without a notice is not a current-turn wait.
	f := &fakeTmux{alive: map[string]bool{"q4": true}, list: []string{"q4"}, capture: "idle"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaudeWithTranscript(t, a, "q4", hookSIDOwn,
		`{"type":"user","timestamp":"t1","message":{"role":"user","content":"do the work"}}`,
		`{"type":"assistant","timestamp":"t2","message":{"role":"assistant","content":[{"type":"text","text":"I'll edit that"}]}}`)
	a.mu.Lock()
	a.live[n.ID] = "quiet"
	a.mu.Unlock()

	v := a.setAutoApproveEnabled(n.ID, true, "quiet", "", 0)
	if v.Phase != string(autoPhasePrimed) {
		t.Fatalf("phase with assistant text and no ask = %q, want primed", v.Phase)
	}
	if _, ok := markerLease(t, bundle); ok {
		t.Fatal("assistant text alone must not publish an arm marker")
	}
}

func TestClaudeNextPromptDisarmsSurvivingArmedLease(t *testing.T) {
	// Belt path: if the poller missed end_turn, accepting the next idle prompt
	// is itself the definitive boundary. With one-turn leases that means off,
	// not a silent re-arm.
	f := &fakeTmux{alive: map[string]bool{"tb": true}, list: []string{"tb"}, capture: "idle", captureAfterEnter: "working"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "tb", hookSIDOwn)
	a.mu.Lock()
	a.live[n.ID] = "quiet"
	a.mu.Unlock()
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)

	acked, err := a.acceptTmuxPrompt(n, false, func() (bool, error) { return true, nil })
	if err != nil || !acked {
		t.Fatalf("accept prompt = %v,%v", acked, err)
	}
	if got := phaseOf(a, n.ID); got != autoPhaseOff {
		t.Fatalf("phase after next prompt = %q, want off", got)
	}
	if marker, ok := markerLease(t, bundle); ok {
		t.Fatalf("next prompt left marker %q armed", marker)
	}
}
