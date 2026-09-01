package remote

import (
	"testing"
)

// AT-S8-cause: Client.TransportCause is the Session lookup the HTTP
// projection calls. It must return the exact FR-24 constant, not a
// generic "unreachable", and must not invent a cause when there is no
// attached Session (D2).

func s8Causes() []TransportCause {
	return []TransportCause{
		CauseRendezvousUnavailable,
		CauseComputerOffline,
		CauseSignallingRejected,
		CauseICEFailed,
		CauseAuthFailed,
		CauseConnectedThenLost,
		// The seventh, added with tunnel 2.0.0: a MAJOR mismatch has to be
		// a named state, because major 2 breaks every major-1 peer by design.
		CauseTunnelVersionMismatch,
	}
}

func TestAT_S8_Cause_ClassProjectionTable(t *testing.T) {
	tests := []struct {
		class Class
		want  TransportCause
	}{
		{ClassICEFailed, CauseICEFailed},
		{ClassLost, CauseConnectedThenLost},
		{ClassUnavailable, CauseRendezvousUnavailable},
		{ClassPeerAbsent, CauseComputerOffline},
		{ClassRevoked, CauseAuthFailed},
		{ClassUnauthorized, CauseAuthFailed},
		{ClassHandshake, CauseSignallingRejected},
		{ClassFingerprint, CauseSignallingRejected},
		{ClassTunnelVersion, CauseTunnelVersionMismatch},
		{Class("future-class"), ""},
	}
	for _, tt := range tests {
		t.Run(string(tt.class), func(t *testing.T) {
			if got := causeOfClass(tt.class); got != tt.want {
				t.Fatalf("causeOfClass(%q) = %q, want %q", tt.class, got, tt.want)
			}
		})
	}
}

func TestAT_S8_Cause_EachCauseRoundTrips(t *testing.T) {
	const at = "AT-S8-cause"
	c := NewClient(Config{})
	seen := map[TransportCause]bool{}
	for _, want := range s8Causes() {
		id := "dev-" + string(want)
		c.channels[id] = &Session{cause: want}
		got, err := c.TransportCause(id)
		if err != nil {
			t.Fatalf("%s: %s: TransportCause: %v", at, want, err)
		}
		if got != want {
			t.Fatalf("%s: cause = %q, want exact constant %q (not a generic unreachable)", at, got, want)
		}
		if got == "unreachable" {
			t.Fatalf("%s: generic unreachable satisfied a row", at)
		}
		seen[got] = true
	}
	if len(seen) != len(s8Causes()) {
		t.Fatalf("%s: saw %d distinct causes, want %d: %v", at, len(seen), len(s8Causes()), seen)
	}
}

func TestAT_S8_Cause_NoChannelIsNotACause(t *testing.T) {
	const at = "AT-S8-cause"
	c := NewClient(Config{})
	got, err := c.TransportCause("phone")
	if got == CauseComputerOffline {
		t.Fatalf("%s: never connected mapped onto computer-offline", at)
	}
	if got != "" {
		t.Fatalf("%s: no-channel cause = %q, want empty", at, got)
	}
	requireClass(t, err, ClassPeerAbsent)
}

func TestAT_S8_Cause_NonSessionChannelIsNotACause(t *testing.T) {
	const at = "AT-S8-cause"
	c := NewClient(Config{})
	c.channels["phone"] = &fakeChannel{}
	got, err := c.TransportCause("phone")
	if got == CauseComputerOffline {
		t.Fatalf("%s: a non-Session channel mapped onto computer-offline", at)
	}
	if got != "" {
		t.Fatalf("%s: non-session cause = %q, want empty (not a panic, not a cause)", at, got)
	}
	requireClass(t, err, ClassPeerAbsent)
}

func TestAT_S8_Cause_ClosedSessionWithNoCauseIsPeerAbsent(t *testing.T) {
	const at = "AT-S8-cause"
	c := NewClient(Config{})
	c.channels["phone"] = &Session{}
	got, err := c.TransportCause("phone")
	if got == CauseComputerOffline {
		t.Fatalf("%s: ClassPeerAbsent mapped onto computer-offline", at)
	}
	if got != "" {
		t.Fatalf("%s: cause = %q, want empty", at, got)
	}
	requireClass(t, err, ClassPeerAbsent)
}
