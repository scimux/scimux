package remote

// P3 (tunnel 2.0.0) — the seventh FR-24 cause.
//
// Major 2 breaks every major-1 peer on purpose (tunnel-v2 §2.1). A break by
// design is only acceptable if the user is told what happened, so the
// verdict the codec reaches at byte 8 has to arrive at the UI as a named
// state — never as a malformed frame, a hang, or a blank page. It also has
// to say *which side is behind*, because the remedy differs: update this
// binary, or wait for the rendezvous deployment to catch up.
//
// A MINOR difference never reaches here. Unknown frame types, unknown
// record tags and unknown rejection classes are skipped, so an additive
// change produces no error to classify.

import (
	"errors"
	"fmt"

	"github.com/scimux/scimux/internal/remote/codec"
)

// ClassTunnelVersion is the transport class for a MAJOR protocol mismatch.
// It is string-identical to its cause, the rule every other pair follows
// (AT-FR-24-a), so a renamed class cannot silently keep an old cause.
const ClassTunnelVersion Class = "tunnel-version-mismatch"

// CauseTunnelVersionMismatch is the matching FR-24 UI state.
const CauseTunnelVersionMismatch TransportCause = "tunnel-version-mismatch"

// noteTunnelError classifies a codec failure. Anything that is not a
// version verdict passes through untouched: relabelling a genuine malformed
// frame as "your binary is old" would send the user to fix the wrong thing.
func noteTunnelError(op string, err error) error {
	if err == nil {
		return nil
	}
	var ve *codec.VersionError
	if !errors.As(err, &ve) {
		return err
	}
	return classErrorf(ClassTunnelVersion, op, tunnelVersionGuidance(ve), err)
}

// tunnelVersionGuidance is the operator-facing sentence. It deliberately
// names no VPN fallback — that guidance belongs to ICE failure alone
// (AT-FR-24-c), and suggesting WireGuard to someone with a stale binary
// would be wrong advice.
func tunnelVersionGuidance(ve *codec.VersionError) string {
	switch {
	case ve == nil:
		return ""
	case !ve.HavePeer:
		return fmt.Sprintf(
			"the far end did not speak the scimux tunnel protocol (expected major %d); "+
				"the browser or the rendezvous deployment is not serving this protocol",
			codec.ProtocolMajor)
	case ve.PeerIsBehind():
		return fmt.Sprintf(
			"the browser speaks tunnel major %d and this scimux speaks major %d; "+
				"the rendezvous deployment is behind and has to be updated",
			ve.PeerMajor, codec.ProtocolMajor)
	default:
		return fmt.Sprintf(
			"the browser speaks tunnel major %d and this scimux speaks major %d; "+
				"update the scimux binary on this machine",
			ve.PeerMajor, codec.ProtocolMajor)
	}
}

// noteTunnelError records the cause on the session as well, so the FR-24
// projection the browser polls names the state instead of showing nothing.
func (s *Session) noteTunnelError(op string, err error) error {
	out := noteTunnelError(op, err)
	// causeOfClass is the one mapping from class to UI state, so the cause
	// stored here cannot drift from the one AT-FR-24-a pins. It is applied
	// narrowly on purpose: a round-trip that failed because the peer is
	// absent is already reported by the paths that own that state, and
	// widening this would let a transient request error overwrite them.
	if cause := causeOfClass(classOfErr(out)); cause == CauseTunnelVersionMismatch {
		s.mu.Lock()
		s.cause = cause
		s.mu.Unlock()
	}
	return out
}
