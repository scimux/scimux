package app

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// The sticky-lease rules (2026-08-17). Enabling auto-approve expresses a
// standing intent, not a one-turn action: the toggle stays on until the human
// takes it back. Only three human acts and the hard resets end it.
//
//	a) interrupt ("stop")
//	b) manual un-toggle
//	c) /clear and /exit
//	+  process/session loss and node delete (non-negotiable, not human acts)
//
// A turn ending is *not* one of them. The lease degrades armed → primed at the
// turn boundary — marker retracted, fence cleared, Enabled still true — and the
// next prompt re-arms it. The pane-liveness disarm this replaces fired on any
// 8s static stretch (paneQuietAfter), which is exactly what an approval dialog
// looks like, so the lease died at the moment it was needed.

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

func TestTurnEndKeepsTmuxLeaseEnabledAsPrimed(t *testing.T) {
	// The lease survives the turn as a *primed* lease: still enabled to the
	// human, but inert to the hook until the next prompt arms it. Retracting
	// the marker at the boundary keeps the "marker only in the armed phase"
	// invariant exactly as the old disarm did.
	f := &fakeTmux{alive: map[string]bool{"t1": true}, list: []string{"t1"}, capture: "idle pane"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "t1", hookSIDOwn)

	a.mu.Lock()
	a.live[n.ID] = "active"
	a.mu.Unlock()
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)
	armedLease := leaseIDOf(a, n.ID)
	if _, ok := markerLease(t, bundle); !ok {
		t.Fatal("precondition: armed lease must publish a marker")
	}

	endTmuxTurn(t, a, n.ID)

	if got := phaseOf(a, n.ID); got != autoPhasePrimed {
		t.Fatalf("phase after the turn ended = %q, want primed (the toggle stays on)", got)
	}
	a.mu.Lock()
	view := a.autoApproveViewOf(n)
	a.mu.Unlock()
	if !view.Enabled {
		t.Fatal("turn end must not switch the toggle off; that is the annoyance this replaces")
	}
	if lease, ok := markerLease(t, bundle); ok {
		t.Fatalf("primed lease left the arm marker behind (%q); the hook must go inert", lease)
	}
	if got := leaseIDOf(a, n.ID); got == armedLease {
		t.Fatal("turn end must rotate the lease id: one lease is still one turn, so the next turn needs a fresh fence")
	}
}

func TestStickyTmuxLeaseRearmsOnNextPromptWithoutRetoggle(t *testing.T) {
	// The whole point: prompt → turn → prompt → turn, one toggle.
	f := &fakeTmux{alive: map[string]bool{"t2": true}, list: []string{"t2"}, capture: "idle pane", captureAfterEnter: "working"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "t2", hookSIDOwn)
	h := newTestHandler(t, a)

	if v := a.enableAutoApprove(n.ID, nil); !v.Enabled {
		t.Fatalf("enable: %+v", v)
	}
	// Two turns is the whole claim: the second one arms from the same enable.
	for turn := 1; turn <= 2; turn++ {
		rec := routeRequest(h, http.MethodPost, "/api/nodes/t2/send", `{"text":"go"}`, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("turn %d send: status = %d body %q", turn, rec.Code, rec.Body.String())
		}
		if got := phaseOf(a, n.ID); got != autoPhaseArmed {
			t.Fatalf("turn %d: phase after prompt = %q, want armed", turn, got)
		}
		lease, ok := markerLease(t, bundle)
		if !ok {
			t.Fatalf("turn %d: armed lease published no marker; the hook can never answer", turn)
		}
		if want := leaseIDOf(a, n.ID); lease != want {
			t.Fatalf("turn %d: marker lease = %q, want %q", turn, lease, want)
		}
		endTmuxTurn(t, a, n.ID)
		if got := phaseOf(a, n.ID); got != autoPhasePrimed {
			t.Fatalf("turn %d: phase after turn end = %q, want primed", turn, got)
		}
	}
}

func TestTurnEndClearsThePromptIDFence(t *testing.T) {
	// TurnPromptID latches the turn a lease has committed to and declines every
	// other turn's request. Under a sticky lease that latch must be released at
	// the turn boundary, or the second turn would be silently unapprovable —
	// stickiness in the UI and nothing behind it.
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
	if st == nil {
		t.Fatal("turn end must keep the lease")
	}
	if st.TurnPromptID != "" {
		t.Fatalf("TurnPromptID = %q after the turn ended, want cleared", st.TurnPromptID)
	}
	if st.Count != 0 {
		t.Fatalf("Count = %d after the turn ended, want 0 (the badge counts this turn)", st.Count)
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

func TestStructuredTurnEndKeepsLeasePrimed(t *testing.T) {
	// Same rule on the structured branch of the poller, for every ACP/codex
	// agent — the lease is transport-independent policy.
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

			if got := phaseOf(a, n.ID); got != autoPhasePrimed {
				t.Fatalf("phase after turn end = %q, want primed", got)
			}
			a.mu.Lock()
			enabled := a.autoApproveViewOf(n).Enabled
			a.mu.Unlock()
			if !enabled {
				t.Fatal("turn end must leave the toggle on")
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
	// Live probe case D, 2026-08-18. A pane that has gone quiet is not a
	// finished turn while a tool is still running: `sleep 150` draws nothing,
	// so the pane crossed paneQuietAfter (8s) at t=25s, the boundary parked the
	// lease, and when the tool finished 130s later the *next* call in the same
	// turn found no marker and escalated to a dialog. One audited approval
	// instead of three, and a human waiting on a turn they had already
	// authorized. Any tool call quiet for more than 8s did this — a build, a
	// test run, a fetch — so it was most real work, not an edge case.
	//
	// Pane quietness alone cannot tell "turn over" from "tool running"; the
	// transcript can, and already does for turn_done. An unresolved call means
	// the turn is still going, so the lease must stay armed.
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

func TestResolvedToolCallStillParksTmuxLease(t *testing.T) {
	// The other side of the same predicate: once the result is in, a quiet pane
	// is a finished turn and the boundary parks the lease as before. Without
	// this the fix above would simply never park.
	f := &fakeTmux{alive: map[string]bool{"t2": true}, list: []string{"t2"}, capture: "idle pane"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaudeWithTranscript(t, a, "t2", hookSIDOwn,
		`{"type":"user","timestamp":"t1","message":{"role":"user","content":"run it"}}`,
		runningToolCall,
		`{"type":"user","timestamp":"t3","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"done"}]}}`,
		`{"type":"assistant","timestamp":"t4","message":{"role":"assistant","content":[{"type":"text","text":"it is done"}]}}`)

	a.mu.Lock()
	a.live[n.ID] = "active"
	a.mu.Unlock()
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)

	endTmuxTurn(t, a, n.ID)

	if got := phaseOf(a, n.ID); got != autoPhasePrimed {
		t.Fatalf("phase after a completed turn = %q, want primed", got)
	}
	if lease, ok := markerLease(t, bundle); ok {
		t.Fatalf("completed turn left the arm marker behind (%q)", lease)
	}
}
