package remote

import (
	"errors"
	"io"
	"strings"
	"testing"

	"codeberg.org/chrberger/scimux/internal/remote/codec"
)

// AT-FR-24-g (tunnel 2.0.0): a MAJOR protocol mismatch is a named FR-24
// state, never a decode error, a hang, or a blank page.
//
// This is the seventh cause. It exists because major 2 deliberately breaks
// every major-1 peer (tunnel-v2 §2.1): the break is by design, so the user
// has to be told *which side is behind*, not shown a malformed frame.

// wrongMajorPeer plays a peer that speaks a different major. It writes a
// well-formed §2.2.1 preamble carrying major, then falls silent — which is
// what an old scimux-rv actually does when its own decoder rejects ours.
func wrongMajorPeer(t *testing.T, major uint16) *codec.Conn {
	t.Helper()
	ourR, peerW := io.Pipe()
	peerR, ourW := io.Pipe()

	go func() {
		defer peerW.Close()
		// Magic, then the peer's major and minor, big-endian (§2.2.1).
		_, _ = peerW.Write([]byte{'S', 'C', 'M', 'X',
			byte(major >> 8), byte(major), 0x00, 0x00})
		// Then nothing. Drain whatever we send so our own write cannot
		// block against a peer that has stopped reading.
		_, _ = io.Copy(io.Discard, peerR)
	}()

	c := codec.NewConn(ourR, ourW, codec.RoleInitiator)
	t.Cleanup(func() { _ = c.Close(); _ = ourW.Close() })
	return c
}

// The verdict the wire really produces, obtained by running the real
// handshake — not a hand-built error value that could drift from it.
func realVersionError(t *testing.T, major uint16) error {
	t.Helper()
	ctx, cancel := ctxTO(t)
	defer cancel()
	err := wrongMajorPeer(t, major).Handshake(ctx)
	if err == nil {
		t.Fatalf("major %d handshake succeeded; want a version verdict", major)
	}
	var ve *codec.VersionError
	if !errors.As(err, &ve) {
		t.Fatalf("major %d handshake err = %v (%T), want *codec.VersionError", major, err, err)
	}
	return err
}

// The cause constant exists, is distinct from the other six, and — like
// every other pair — is string-identical to its class so a renamed class
// cannot silently keep an old cause (the AT-FR-24-a rule).
func TestAT_FR_24_g_TunnelVersionIsADistinctCause(t *testing.T) {
	const at = "AT-FR-24-g"
	if string(CauseTunnelVersionMismatch) != string(ClassTunnelVersion) {
		t.Fatalf("%s: cause %q does not match class %q",
			at, CauseTunnelVersionMismatch, ClassTunnelVersion)
	}
	for _, other := range s8Causes() {
		if other == CauseTunnelVersionMismatch {
			continue
		}
		if string(other) == string(CauseTunnelVersionMismatch) {
			t.Fatalf("%s: tunnel-version-mismatch collides with %q", at, other)
		}
	}
	if causeOfClass(ClassTunnelVersion) != CauseTunnelVersionMismatch {
		t.Fatalf("%s: causeOfClass(%q) = %q", at, ClassTunnelVersion,
			causeOfClass(ClassTunnelVersion))
	}
	// AT-FR-24-c: only ICE failure names the VPN fallback. Telling someone
	// whose binary is out of date to try WireGuard would be wrong advice.
	if CauseTunnelVersionMismatch.Guidance() != "" {
		t.Fatalf("%s: version mismatch carries ICE guidance: %q",
			at, CauseTunnelVersionMismatch.Guidance())
	}
}

// The error the codec really returns must classify. A version verdict that
// reached the UI as ClassMalformed would be exactly the decode error FR-24
// forbids.
func TestAT_FR_24_g_VersionVerdictClassifies(t *testing.T) {
	const at = "AT-FR-24-g"
	for _, major := range []uint16{1, 3, 512} {
		err := noteTunnelError("round-trip", realVersionError(t, major))
		requireClass(t, err, ClassTunnelVersion)
		if classOf(err) == ClassHandshake {
			t.Fatalf("%s: major %d surfaced as a generic handshake failure", at, major)
		}
	}
}

// FR-24 requires the state to say which side is behind, because the user's
// remedy differs: update the laptop binary, or wait for the deployment.
func TestAT_FR_24_g_VerdictNamesWhichSideIsBehind(t *testing.T) {
	const at = "AT-FR-24-g"
	behind := noteTunnelError("round-trip", realVersionError(t, 1)).Error()
	ahead := noteTunnelError("round-trip", realVersionError(t, 3)).Error()

	if !strings.Contains(strings.ToLower(behind), "browser") &&
		!strings.Contains(strings.ToLower(behind), "rendezvous") {
		t.Fatalf("%s: an older peer is not named: %q", at, behind)
	}
	if !strings.Contains(strings.ToLower(ahead), "scimux") {
		t.Fatalf("%s: a newer peer does not point at this binary: %q", at, ahead)
	}
	if behind == ahead {
		t.Fatalf("%s: both directions produce the same sentence: %q", at, behind)
	}
	if s6GuidanceHasFallback(behind + ahead) {
		t.Fatalf("%s: version mismatch named the ICE fallback", at)
	}
}

// Everything that is not a version verdict must pass through untouched.
// A translator that swallowed ordinary errors would relabel a genuine
// malformed frame as "your binary is old".
func TestAT_FR_24_g_OtherErrorsAreNotRelabelled(t *testing.T) {
	const at = "AT-FR-24-g"
	if got := noteTunnelError("round-trip", nil); got != nil {
		t.Fatalf("%s: nil became %v", at, got)
	}
	plain := classError(ClassPeerAbsent, "round-trip", "gone")
	if got := noteTunnelError("round-trip", plain); got != error(plain) {
		t.Fatalf("%s: a peer-absent error was rewritten to %v", at, got)
	}
	other := errors.New("some codec failure")
	if got := noteTunnelError("round-trip", other); got != other {
		t.Fatalf("%s: an unclassified error was rewritten to %v", at, got)
	}
}

// The Session must report it as its FR-24 cause, so the HTTP projection
// the browser polls names the state rather than showing nothing.
func TestAT_FR_24_g_SessionReportsTheCause(t *testing.T) {
	const at = "AT-FR-24-g"
	s := &Session{}
	if err := s.noteTunnelError("round-trip", realVersionError(t, 1)); err == nil {
		t.Fatalf("%s: no error returned", at)
	}
	got, err := s.TransportCause()
	if err != nil {
		t.Fatalf("%s: TransportCause: %v", at, err)
	}
	if got != CauseTunnelVersionMismatch {
		t.Fatalf("%s: cause = %q, want %q", at, got, CauseTunnelVersionMismatch)
	}

	c := NewClient(Config{})
	c.channels["phone"] = s
	if got, err = c.TransportCause("phone"); err != nil || got != CauseTunnelVersionMismatch {
		t.Fatalf("%s: client cause = %q, %v", at, got, err)
	}
}

// The verdict must be reached before a request is on the wire (§7 step 0).
// A mismatch discovered halfway through a body would have already sent
// bytes a major-1 peer cannot parse.
func TestAT_FR_24_g_NothingIsSentBeforeTheVerdict(t *testing.T) {
	const at = "AT-FR-24-g"
	ctx, cancel := ctxTO(t)
	defer cancel()

	conn := wrongMajorPeer(t, 1)
	_, err := conn.RoundTrip(ctx, &codec.Request{
		ID:     "ver-0001",
		Method: "GET",
		Path:   "/api/state",
	})
	var ve *codec.VersionError
	if !errors.As(err, &ve) {
		t.Fatalf("%s: RoundTrip err = %v, want a version verdict before any request", at, err)
	}
	if !ve.PeerIsBehind() {
		t.Fatalf("%s: major 1 peer not reported as behind", at)
	}
}
