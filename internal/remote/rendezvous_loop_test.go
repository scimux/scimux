package remote

import "testing"

// The wait loop is what registers a pairing waiter with the rendezvous, so a
// client that never runs it mints codes no device can redeem: the rendezvous
// answers "the code is not in use" for a code the computer printed a second
// earlier, which reads to the user as a clock or typing problem rather than a
// computer that never announced itself. The guard is therefore behaviour, and
// a production caller states the intent by name rather than by remembering
// five durations.
func TestRunsRendezvousLoopGuardsTheWaitLoop(t *testing.T) {
	t.Run("production defaults run the loop", func(t *testing.T) {
		cfg := Config{Backoff: DefaultBackoff()}
		if !cfg.RunsRendezvousLoop() {
			t.Fatal("DefaultBackoff does not run the rendezvous loop")
		}
		c := NewClient(cfg)
		c.startRVLoop()
		defer c.stopRVLoop()
		c.mu.Lock()
		running := c.loopCancel != nil
		c.mu.Unlock()
		if !running {
			t.Fatal("startRVLoop refused a client configured with DefaultBackoff")
		}
	})

	t.Run("an unconfigured client stays off", func(t *testing.T) {
		// Tests construct clients that must not reach the network. That
		// silence is the whole trap: it is indistinguishable from a
		// production client whose config lost its backoff.
		cfg := Config{}
		if cfg.RunsRendezvousLoop() {
			t.Fatal("a zero config claims to run the rendezvous loop")
		}
		c := NewClient(cfg)
		c.startRVLoop()
		defer c.stopRVLoop()
		c.mu.Lock()
		running := c.loopCancel != nil
		c.mu.Unlock()
		if running {
			t.Fatal("startRVLoop started without backoff or a scheduler")
		}
	})

	t.Run("an injected scheduler runs the loop", func(t *testing.T) {
		// Backoff-free tests drive retries through a scheduler instead, so
		// the switch has always had two "on" positions.
		cfg := Config{Scheduler: newManualSched()}
		if !cfg.RunsRendezvousLoop() {
			t.Fatal("an injected scheduler does not run the rendezvous loop")
		}
	})
}
