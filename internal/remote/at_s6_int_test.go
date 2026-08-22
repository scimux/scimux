package remote

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// AT-NFR-06-a: An established data channel survives a full restart of
// the fake rendezvous.
func TestAT_NFR_06_a_EstablishedChannelSurvivesRendezvousRestart(t *testing.T) {
	const at = "AT-NFR-06-a"
	c, _, d1, _, _ := enrollTwoDevices(t)
	ctx, cancel := ctxTO(t)
	defer cancel()

	s := s6Establish(t, at, ctx, c, d1.ID)
	t.Cleanup(func() { _ = s.Close() })
	if !s.ChannelLive() {
		t.Fatalf("%s: data channel is not live after establish", at)
	}
	if err := s6GetState(t, at, s); err != nil {
		t.Fatalf("%s: GET /api/state before restart: %v", at, err)
	}

	if err := s.RestartRendezvous(); err != nil {
		t.Fatalf("%s: restart fake rendezvous: %v", at, err)
	}
	if !s.ChannelLive() {
		t.Fatalf("%s: established data channel did not survive rendezvous restart", at)
	}
	if err := s6GetState(t, at, s); err != nil {
		t.Fatalf("%s: GET /api/state after restart: %v", at, err)
	}
}

// NFR-07 client half: reconnection after a restart must not produce a
// synchronised stampede. With NFR-06 the established channel is the
// reason no new session-offer burst is required. AT-NFR-07-a itself is
// the rv row (no Retry-After on wait); this is the laptop half S6 owns.
func TestAT_NFR_07_ClientDoesNotStampedeAfterRendezvousRestart(t *testing.T) {
	const at = "NFR-07"
	c, _, d1, _, _ := enrollTwoDevices(t)
	ctx, cancel := ctxTO(t)
	defer cancel()

	s := s6Establish(t, at, ctx, c, d1.ID)
	t.Cleanup(func() { _ = s.Close() })
	before := s.OfferCount()
	if err := s.RestartRendezvous(); err != nil {
		t.Fatalf("%s: restart fake rendezvous: %v", at, err)
	}
	if !s.ChannelLive() {
		t.Fatalf("%s: channel dropped on restart, forcing a reconnect stampede", at)
	}
	if got := s.OfferCount(); got != before {
		t.Fatalf("%s: session-offer count %d → %d after rendezvous restart (stampede)", at, before, got)
	}
}

// AT-FR-26-c: Client disconnect propagates through Caddy to the hub and
// removes the waiter. The in-process fake hub is the Caddy+hub stand-in;
// no real reverse proxy and no wall-clock wait.
func TestAT_FR_26_c_ClientDisconnectRemovesWaiter(t *testing.T) {
	const at = "AT-FR-26-c"
	c, _, d1, _, _ := enrollTwoDevices(t)
	ctx, cancel := ctxTO(t)
	defer cancel()

	s := s6Establish(t, at, ctx, c, d1.ID)
	t.Cleanup(func() { _ = s.Close() })
	if n := s.SignallingWaiters(); n < 1 {
		t.Fatalf("%s: hub waiters = %d before disconnect, want at least 1", at, n)
	}
	if err := s.DisconnectPeer(); err != nil {
		t.Fatalf("%s: disconnect: %v", at, err)
	}
	if n := s.SignallingWaiters(); n != 0 {
		t.Fatalf("%s: hub waiters = %d after client disconnect, want 0", at, n)
	}
}

// AT-FR-36-b: Local device revocation (FR-13) does terminate the
// established channel, so the two guarantees (rendezvous-side revoke vs
// local revoke) are demonstrably different.
func TestAT_FR_36_b_LocalRevokeTerminatesEstablishedChannel(t *testing.T) {
	const at = "AT-FR-36-b"
	c, _, d1, d2, _ := enrollTwoDevices(t)
	ctx, cancel := ctxTO(t)
	defer cancel()

	s1 := s6Establish(t, at, ctx, c, d1.ID)
	s2 := s6Establish(t, at+" other", ctx, c, d2.ID)
	t.Cleanup(func() {
		_ = s1.Close()
		_ = s2.Close()
	})
	if !s1.ChannelLive() || !s2.ChannelLive() {
		t.Fatalf("%s: both channels must be live before local revoke", at)
	}

	if err := c.RevokeDevice(ctx, d1.ID); err != nil {
		t.Fatalf("%s: local revoke: %v", at, err)
	}
	if s1.ChannelLive() {
		t.Fatalf("%s: local revoke left the established channel live", at)
	}
	if !s2.ChannelLive() {
		t.Fatalf("%s: local revoke of one device terminated the other channel", at)
	}
}

func s6GetState(t *testing.T, at string, s *Session) error {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "/api/state", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := s.RoundTrip(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s: GET /api/state status %d body %q", at, resp.StatusCode, body)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("%s: GET /api/state JSON: %v (%q)", at, err, raw)
	}
	return nil
}
