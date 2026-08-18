package app

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// markerLease reads the arm marker a bundle currently publishes.
func markerLease(t *testing.T, bundle string) (string, bool) {
	t.Helper()
	return readClaudePermLease(filepath.Join(bundle, "perm"), time.Now())
}

func phaseOf(a *app, id string) autoApprovePhase {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.autoApprove[id]
	if st == nil {
		return autoPhaseOff
	}
	return st.Phase
}

func TestTmuxPromptArmsPrimedClaudeLease(t *testing.T) {
	// AT-CP-19: enabling while the pane is idle primes the lease and must not
	// publish a marker — the turn already on screen keeps meeting the human.
	// The next prompt is the arm boundary, and only then does the hook see a
	// marker at all.
	f := &fakeTmux{alive: map[string]bool{"n1": true}, captureAfterEnter: "working"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	h := newTestHandler(t, a)

	if view := a.enableAutoApprove(n.ID, nil); !view.Supported || !view.Enabled {
		t.Fatalf("enable: %+v, want a supported, enabled lease", view)
	}
	if got := phaseOf(a, n.ID); got != autoPhasePrimed {
		t.Fatalf("phase after idle enable = %q, want primed", got)
	}
	if lease, ok := markerLease(t, bundle); ok {
		t.Fatalf("primed lease published an arm marker (%q); the hook must stay inert", lease)
	}

	rec := routeRequest(h, http.MethodPost, "/api/nodes/n1/send", `{"text":"go"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("send: status = %d body %q", rec.Code, rec.Body.String())
	}
	if got := phaseOf(a, n.ID); got != autoPhaseArmed {
		t.Fatalf("phase after prompt = %q, want armed", got)
	}
	lease, ok := markerLease(t, bundle)
	if !ok {
		t.Fatal("armed lease published no marker; the hook can never answer")
	}
	a.mu.Lock()
	want := a.autoApprove[n.ID].LeaseID
	a.mu.Unlock()
	if lease != want {
		t.Fatalf("marker lease = %q, want the armed lease %q", lease, want)
	}
}

func TestTmuxClearDisarmsClaudeLease(t *testing.T) {
	// AT-CP-20: /clear is a page turn, not a turn. The lease and its marker go
	// with the closed page; the fresh surface must be armed deliberately.
	f := &fakeTmux{alive: map[string]bool{"c1": true}, captureAfterEnter: "cleared"}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "c1", hookSIDOwn)
	h := newTestHandler(t, a)

	a.mu.Lock()
	a.live[n.ID] = "active"
	a.mu.Unlock()
	a.setAutoApproveEnabled(n.ID, true, "active", "", 0)
	if _, ok := markerLease(t, bundle); !ok {
		t.Fatal("precondition: armed lease must publish a marker")
	}

	rec := routeRequest(h, http.MethodPost, "/api/nodes/c1/send", `{"text":"/clear"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("/clear: status = %d body %q", rec.Code, rec.Body.String())
	}
	if got := phaseOf(a, n.ID); got != autoPhaseOff {
		t.Fatalf("phase after /clear = %q, want off", got)
	}
	if lease, ok := markerLease(t, bundle); ok {
		t.Fatalf("/clear left the arm marker behind (%q)", lease)
	}
}

// AT-CP-21 (the *armed phase* cannot cross a turn) now lives in
// auto_approve_sticky_test.go as TestTurnEndDisarmsTmuxLease: active → quiet
// with an explicit turn boundary retracts the marker and turns the lease off.

func TestEnableDuringActiveTmuxTurnArmsImmediately(t *testing.T) {
	// AT-CP-22: a human toggles auto-approve *because* a turn is running. For
	// the structured transports the enable cutoff is the permission sequence;
	// for Claude it is the marker itself — a call that asked before the marker
	// existed wrote no request, so arming mid-turn cannot answer it.
	f := &fakeTmux{alive: map[string]bool{"n1": true}}
	a := newTestApp(t, f)
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)

	a.mu.Lock()
	a.live[n.ID] = "active"
	a.mu.Unlock()

	// No procManager: a tmux node's liveness lives in the poller's map, and
	// enable must read it there rather than defaulting to primed.
	view := a.enableAutoApprove(n.ID, nil)
	if !view.Enabled || view.Phase != string(autoPhaseArmed) {
		t.Fatalf("enable during an active tmux turn = %+v, want an armed lease", view)
	}
	if _, ok := markerLease(t, bundle); !ok {
		t.Fatal("mid-turn arm published no marker")
	}
}

func TestClaudePermissionLaneIdleWithoutArmedLease(t *testing.T) {
	// AT-CP-23: the lane runs ten times a second. With no armed lease it must
	// do no filesystem work at all — and in particular must never answer a
	// request left over from a turn whose lease is gone.
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	req := armedRequest(a, n, "nonce-1")
	req.Lease = "lease-gone"
	dropRequest(t, bundle, req)

	if got := a.resolveClaudePermissions(); got != 0 {
		t.Fatalf("lane delivered %d approvals with no lease armed", got)
	}
	if _, ok := answerFileFor(bundle, req.ID); ok {
		t.Fatal("lane answered a request belonging to no lease")
	}
	if _, err := os.Stat(filepath.Join(bundle, "perm", "req", req.ID+".json")); err != nil {
		t.Fatalf("lane touched the rendezvous while off: %v", err)
	}
}

func TestClaudePermissionLaneResolvesOnlyArmedNodes(t *testing.T) {
	// AT-CP-24: the lane is per-node. One armed node is served; its neighbour,
	// enabled but only primed, is not.
	a := newTestApp(t, &fakeTmux{})
	armed, armedBundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	primed, primedBundle := seedPermClaude(t, a, "n2", "00000000-0000-4000-8000-0000000000bb")

	a.setAutoApproveEnabled(armed.ID, true, "active", "", 0)
	a.setAutoApproveEnabled(primed.ID, true, "quiet", "", 0)

	armedReq := armedRequest(a, armed, "nonce-1")
	dropRequest(t, armedBundle, armedReq)
	primedReq := armedRequest(a, primed, "nonce-2")
	dropRequest(t, primedBundle, primedReq)

	if got := a.resolveClaudePermissions(); got != 1 {
		t.Fatalf("lane delivered %d approvals, want exactly the armed node's one", got)
	}
	ans, ok := answerFileFor(armedBundle, armedReq.ID)
	if !ok || ans.Decision != "allow" || ans.ID != armedReq.ID {
		t.Fatalf("armed node answer = %+v ok=%v, want an allow for its own nonce", ans, ok)
	}
	if _, ok := answerFileFor(primedBundle, primedReq.ID); ok {
		t.Fatal("a primed lease answered a request; only armed may")
	}
}

func TestRefreshClaudePermCapsSurvivesRestart(t *testing.T) {
	// AT-CP-25: a pane outlives scimux. Capability is proven from the bundle
	// on disk at startup, so a surviving pane keeps its toggle — and a bundle
	// written before this feature keeps not offering one.
	a := newTestApp(t, &fakeTmux{})
	n, _ := seedPermClaude(t, a, "n1", hookSIDOwn)
	legacy := seedOwnedClaude(t, a, "old", "00000000-0000-4000-8000-0000000000cc", "")
	if err := os.MkdirAll(filepath.Join(a.claudeHooksDir(), a.claudeHooks[legacy.ID]), 0o700); err != nil {
		t.Fatal(err)
	}

	a.mu.Lock()
	a.claudePermCap = nil // as after a restart, before anything is proven
	a.mu.Unlock()
	a.refreshClaudePermCaps()

	if !a.autoApproveSupportedFor(n) {
		t.Fatal("a permission-capable bundle must stay supported across a restart")
	}
	if a.autoApproveSupportedFor(legacy) {
		t.Fatal("a bundle without the permission layout must not become supported")
	}
}

func TestAutoApproveHandlerReadsSupportUnderLock(t *testing.T) {
	// AT-CP-26b: Claude's support predicate reads a.claudeHooks and the
	// capability map, both of which the hook drain mutates under a.mu. The
	// handler must take the lock for that read — under -race this fails
	// outright if it does not. (Run the suite with -race to get the signal.)
	f := &fakeTmux{alive: map[string]bool{"n1": true}}
	a := newTestApp(t, f)
	n, _ := seedPermClaude(t, a, "n1", hookSIDOwn)
	h := newTestHandler(t, a)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			a.mu.Lock()
			a.claudeHooks[n.ID] = a.claudeHooks[n.ID] // same value, still a write
			a.mu.Unlock()
		}
	}()
	for i := 0; i < 200; i++ {
		rec := routeRequest(h, http.MethodPost, "/api/nodes/n1/auto-approve", `{"enabled":false}`, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("disable: status = %d body %q", rec.Code, rec.Body.String())
		}
	}
	<-done
}
