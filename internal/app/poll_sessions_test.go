package app

import (
	"testing"
	"time"
)

// countSubs returns how many times sub appears among the fake tmux calls.
func countSubs(f *fakeTmux, sub string) int {
	n := 0
	for _, s := range f.subcommands() {
		if s == sub {
			n++
		}
	}
	return n
}

// TestPollOneListSessionsZeroHasSession is the fork-storm fix: one poll over
// N tmux nodes must issue exactly one list-sessions and zero has-session.
// Liveness is answered from the name set, not N process forks.
func TestPollOneListSessionsZeroHasSession(t *testing.T) {
	f := &fakeTmux{
		list:    []string{"n0", "n1", "n2", "n3", "n4"},
		alive:   map[string]bool{"n0": true, "n1": true, "n2": true, "n3": true, "n4": true},
		capture: "pane",
	}
	a := newTestApp(t, f)
	const n = 5
	for i := 0; i < n; i++ {
		id := f.list[i]
		node := &Node{ID: id, Title: id, Agent: "claude", CreatedAt: time.Now().UTC().Format(time.RFC3339)}
		a.nodes = append(a.nodes, node)
		a.byID[id] = node
	}
	// Clear any launch-path tmux noise from newTestApp construction.
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()

	a.poll()

	listN := countSubs(f, "list-sessions")
	hasN := countSubs(f, "has-session")
	if listN != 1 {
		t.Errorf("list-sessions count = %d, want 1; subs=%v", listN, f.subcommands())
	}
	if hasN != 0 {
		t.Errorf("has-session count = %d, want 0; subs=%v", hasN, f.subcommands())
	}
	// Alive nodes still get Capture (one capture-pane each at minimum).
	capN := countSubs(f, "capture-pane")
	if capN < n {
		t.Errorf("capture-pane count = %d, want >= %d for alive nodes", capN, n)
	}
}

// TestPollLivenessFromSessionSet checks set membership, not has-session:
// present → Capture path (quiet/active/unavailable); absent → exited.
func TestPollLivenessFromSessionSet(t *testing.T) {
	f := &fakeTmux{
		list:    []string{"alive-one"},
		capture: "same pane text",
	}
	a := newTestApp(t, f)
	alive := &Node{ID: "alive-one", Title: "alive-one", Agent: "claude", CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	gone := &Node{ID: "gone-one", Title: "gone-one", Agent: "claude", CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	a.nodes = []*Node{alive, gone}
	a.byID = map[string]*Node{"alive-one": alive, "gone-one": gone}
	// Aged lastChg so an unchanged capture is quiet, not active.
	a.lastChg["alive-one"] = time.Now().Add(-10 * time.Second)
	a.prevCap["alive-one"] = "same pane text"

	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
	a.poll()

	if got := a.live["alive-one"]; got != "quiet" {
		t.Errorf("alive-one live = %q, want quiet", got)
	}
	if got := a.live["gone-one"]; got != "exited" {
		t.Errorf("gone-one live = %q, want exited", got)
	}
	if countSubs(f, "has-session") != 0 {
		t.Errorf("has-session must not be used; subs=%v", f.subcommands())
	}
	// Capture targets only the alive session.
	for _, c := range f.calls {
		if len(c) >= 3 && c[2] == "capture-pane" {
			target := lastArg(c)
			if target == "=gone-one" || target == "=gone-one:" {
				t.Errorf("captured exited node: %v", c)
			}
		}
	}
}

// TestPollListSessionsFailureLeavesLivenessUnchanged documents branch (a):
// a failed list-sessions snapshot must not mass-flip every node to exited.
// Transient tmux failures leave live[] alone for that tick.
func TestPollListSessionsFailureLeavesLivenessUnchanged(t *testing.T) {
	f := &fakeTmux{
		list:    []string{"keep-me"},
		capture: "pane",
	}
	a := newTestApp(t, f)
	n := &Node{ID: "keep-me", Title: "keep-me", Agent: "claude", CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	a.nodes = []*Node{n}
	a.byID = map[string]*Node{"keep-me": n}
	a.live["keep-me"] = "active"
	a.lastChg["keep-me"] = time.Now()
	a.prevCap["keep-me"] = "pane"

	// First tick establishes baseline from a good snapshot.
	a.poll()
	if got := a.live["keep-me"]; got != "active" && got != "quiet" {
		t.Fatalf("baseline live = %q, want active or quiet", got)
	}
	baseline := a.live["keep-me"]

	// Next tick: list-sessions fails. Liveness must not become exited.
	f.mu.Lock()
	f.listErr = true
	f.calls = nil
	f.mu.Unlock()
	a.poll()

	if got := a.live["keep-me"]; got != baseline {
		t.Errorf("after list-sessions failure live = %q, want unchanged %q (branch a: no mass flip)", got, baseline)
	}
	if countSubs(f, "list-sessions") != 1 {
		t.Errorf("list-sessions count = %d, want 1", countSubs(f, "list-sessions"))
	}
	if countSubs(f, "has-session") != 0 {
		t.Errorf("has-session count = %d, want 0", countSubs(f, "has-session"))
	}
}
