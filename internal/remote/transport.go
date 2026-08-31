package remote

// S6 (remote refactor) — the WebRTC transport.
//
// This file is the *protocol* half: the §12.1 session-offer/session-answer
// inner JSON, the §12.2 sealed envelope, and the FR-16 refusal rules that
// decide whether a relayed session description may be used at all. The live
// half (peers, data channel, FR-27 frames) is transport_session.go.
//
// pion enters the tree here. It is dependency exception #2 (AGENTS.md,
// approved 2026-08-22) and is confined to this package;
// internal/app/remote_boundary_guard_test.go asserts that mechanically over
// the module import graph.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"

	"github.com/pion/webrtc/v4"
)

const (
	// ClassFingerprint is AT-FR-16-b: a hub-substituted DTLS fingerprint
	// is detected and the connection is refused.
	ClassFingerprint Class = "fingerprint"
	// ClassHandshake is AT-FR-16-a: a hub-altered SDP fails DTLS.
	ClassHandshake Class = "handshake"
	// ClassICEFailed is FR-24: ICE failed (no direct path on this network pair).
	ClassICEFailed Class = "ice-failed"
	// ClassLost is FR-24: connected-then-lost.
	ClassLost Class = "connected-then-lost"
)

// TransportCause is one FR-24 UI state. Tests match on the constant so a
// generic "unreachable" cannot satisfy a row.
type TransportCause string

const (
	CauseRendezvousUnavailable TransportCause = "rendezvous-unavailable"
	CauseComputerOffline       TransportCause = "computer-offline"
	CauseSignallingRejected    TransportCause = "signalling-rejected"
	CauseICEFailed             TransportCause = "ice-failed"
	CauseAuthFailed            TransportCause = "auth-failed"
	CauseConnectedThenLost     TransportCause = "connected-then-lost"
)

// SessionInner is protocol §12.1 session-offer / session-answer JSON.
type SessionInner struct {
	V           int    `json:"v"`
	Type        string `json:"type"`
	SDP         string `json:"sdp"`
	Fingerprint string `json:"fingerprint"`
}

// The two §12.1 inner types. Nothing else may be sealed as a session.
const (
	sessionOfferType  = "session-offer"
	sessionAnswerType = "session-answer"
)

// envelopeSaltV1 and envelopeSealLabel are the §12.2 HKDF salt and the
// info prefix. The vectors in testdata/vectors/constructions.json
// (envelope-seal-p256) are the authority; protocol_vectors_test.go executes
// them against an independent implementation of the same construction.
const (
	envelopeSaltV1    = "scimux-rv/envelope/v1"
	envelopeSealLabel = "seal"
)

// p256UncompressedLen is the length of an uncompressed P-256 point, which
// is also the length of the ephemeral public key prefixed to every sealed
// envelope. gcmNonceLen and gcmTagLen complete the minimum blob size.
const (
	p256UncompressedLen = 65
	gcmNonceLen         = 12
	gcmTagLen           = 16
)

// iceFallbackGuidance is AT-FR-24-c. Only the ICE-failure state may carry
// it: naming a VPN fallback for a revoked credential or an offline computer
// would be wrong advice, so this string must not be reused for another
// class.
const iceFallbackGuidance = "no direct path between this network pair; " +
	"reach the computer over a supported fallback instead — an SSH port-forward, " +
	"WireGuard, or Tailscale"

// CreateSessionOffer builds a WebRTC offer whose inner JSON includes the
// DTLS fingerprint and matches the SDP (FR-16).
//
// The offer is fully gathered before it returns: the envelope is sealed and
// posted once, so there is no trickle channel to carry late candidates.
//
// The peer connection minted here is closed before returning. This function
// produces the *value* that goes into a §12.2 envelope; a live session comes
// from InProcessTunnel or EstablishForDevice, which negotiate and keep their
// own peers.
func CreateSessionOffer(ctx context.Context) (SessionInner, error) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return SessionInner{}, classErrorf(ClassHandshake, "session-offer", "could not create a peer connection", err)
	}
	defer func() { _ = pc.Close() }()

	// The tunnel carries FR-27 frames over one data channel; declaring it
	// before the offer is what puts an m=application section in the SDP.
	if _, err := pc.CreateDataChannel(tunnelChannelLabel, nil); err != nil {
		return SessionInner{}, classErrorf(ClassHandshake, "session-offer", "could not create the data channel", err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return SessionInner{}, classErrorf(ClassHandshake, "session-offer", "could not create an offer", err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		return SessionInner{}, classErrorf(ClassHandshake, "session-offer", "could not apply the local offer", err)
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		return SessionInner{}, classErrorf(ClassHandshake, "session-offer", "candidate gathering did not finish", ctx.Err())
	}
	return innerFromDescription(sessionOfferType, pc.LocalDescription())
}

// CreateSessionAnswer builds a WebRTC answer to offer (FR-16). The offer is
// applied through the real stack, so an offer the hub altered is refused
// here rather than producing an answer to a description nobody sent.
func CreateSessionAnswer(ctx context.Context, offer SessionInner) (SessionInner, error) {
	if err := checkInner(offer, sessionOfferType); err != nil {
		return SessionInner{}, err
	}
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return SessionInner{}, classErrorf(ClassHandshake, "session-answer", "could not create a peer connection", err)
	}
	defer func() { _ = pc.Close() }()

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer.SDP}); err != nil {
		return SessionInner{}, classErrorf(ClassHandshake, "session-answer", "the relayed offer was refused by the WebRTC stack", err)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return SessionInner{}, classErrorf(ClassHandshake, "session-answer", "could not create an answer", err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		return SessionInner{}, classErrorf(ClassHandshake, "session-answer", "could not apply the local answer", err)
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		return SessionInner{}, classErrorf(ClassHandshake, "session-answer", "candidate gathering did not finish", ctx.Err())
	}
	return innerFromDescription(sessionAnswerType, pc.LocalDescription())
}

// innerFromDescription lifts a gathered local description into §12.1 form,
// carrying the DTLS fingerprint out of the SDP as its own field. That
// duplication is the point of FR-16: the fingerprint travels sealed, so a
// hub that rewrites the SDP copy is caught by the comparison.
func innerFromDescription(typ string, desc *webrtc.SessionDescription) (SessionInner, error) {
	if desc == nil || desc.SDP == "" {
		return SessionInner{}, classError(ClassHandshake, "session", "the peer connection produced no session description")
	}
	fp, ok := fingerprintFromSDP(desc.SDP)
	if !ok {
		return SessionInner{}, classError(ClassHandshake, "session", "the session description carries no DTLS fingerprint")
	}
	return SessionInner{
		V:           ProtocolVersion,
		Type:        typ,
		SDP:         desc.SDP,
		Fingerprint: fp,
	}, nil
}

// fingerprintFromSDP returns the a=fingerprint value, e.g.
// "sha-256 B4:FC:…". An SDP with no fingerprint line has no DTLS identity
// and is never usable.
func fingerprintFromSDP(sdp string) (string, bool) {
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimRight(line, "\r")
		const prefix = "a=fingerprint:"
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		if v == "" {
			return "", false
		}
		return v, true
	}
	return "", false
}

// sameFingerprint compares two fingerprint strings by their meaning rather
// than their bytes: case and inter-field spacing are not significant, the
// hash algorithm and the digest are.
func sameFingerprint(a, b string) bool {
	norm := func(s string) string {
		return strings.ToLower(strings.Join(strings.Fields(s), " "))
	}
	return norm(a) != "" && norm(a) == norm(b)
}

// checkInner is the structural admission of a §12.1 inner. Version and type
// are fail-closed: an unknown type is never treated as an offer.
func checkInner(in SessionInner, want string) error {
	switch {
	case in.V != ProtocolVersion:
		return classError(ClassHandshake, "session", "the session envelope has an unsupported version")
	case in.Type != want:
		return classError(ClassHandshake, "session", "the session envelope is not a "+want)
	case in.SDP == "":
		return classError(ClassHandshake, "session", "the session envelope carries no session description")
	case in.Fingerprint == "":
		return classError(ClassHandshake, "session", "the session envelope carries no DTLS fingerprint")
	}
	return nil
}

// canonicalInner marshals a §12.1 inner with sorted keys, matching the
// envelope-inner vector. encoding/json sorts map keys, so the map is the
// canonicaliser — a struct would emit declaration order instead.
func canonicalInner(in SessionInner) ([]byte, error) {
	return json.Marshal(map[string]any{
		"v":           in.V,
		"type":        in.Type,
		"sdp":         in.SDP,
		"fingerprint": in.Fingerprint,
	})
}

// envelopeAD is the §12.2 associated data: the rendezvous origin and the
// rendezvous ID, NUL-separated. Binding both means an envelope resealed for
// a different origin or replayed onto a different RID will not open.
func envelopeAD(origin, rid string) []byte {
	ad := make([]byte, 0, len(origin)+1+len(rid))
	ad = append(ad, origin...)
	ad = append(ad, 0)
	ad = append(ad, rid...)
	return ad
}

// envelopeSealInfo is the HKDF info: the label, the ephemeral public key,
// and the recipient's static public key, NUL-separated. Both keys are in
// the info so a key-substituting hub derives a different key and the open
// fails.
func envelopeSealInfo(ephemeralPub, recipientStatic []byte) string {
	b := make([]byte, 0, len(envelopeSealLabel)+2+len(ephemeralPub)+len(recipientStatic))
	b = append(b, envelopeSealLabel...)
	b = append(b, 0)
	b = append(b, ephemeralPub...)
	b = append(b, 0)
	b = append(b, recipientStatic...)
	return string(b)
}

func envelopeKey(shared, ephemeralPub, recipientStatic []byte) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, shared, []byte(envelopeSaltV1), envelopeSealInfo(ephemeralPub, recipientStatic), 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// SealEnvelope seals inner to recipientPub under origin and rid as AD
// (protocol §12.2). The ephemeral key and the nonce are drawn fresh from
// crypto/rand on every call: the vector's fixed ephemeral is a fixture, not
// a minting rule, and reusing either would leak the plaintext.
func SealEnvelope(inner SessionInner, recipientPub []byte, origin, rid string) ([]byte, error) {
	plain, err := canonicalInner(inner)
	if err != nil {
		return nil, classErrorf(ClassHandshake, "seal", "could not encode the session inner", err)
	}
	return sealEnvelopeBytes(plain, recipientPub, origin, rid)
}

func sealEnvelopeBytes(plain, recipientPub []byte, origin, rid string) ([]byte, error) {
	if origin == "" || rid == "" {
		return nil, classError(ClassHandshake, "seal", "an envelope needs both an origin and a rendezvous id")
	}
	curve := ecdh.P256()
	pub, err := curve.NewPublicKey(recipientPub)
	if err != nil {
		return nil, classErrorf(ClassHandshake, "seal", "the recipient key is not a P-256 point", err)
	}
	eph, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, classErrorf(ClassHandshake, "seal", "could not draw an ephemeral key", err)
	}
	shared, err := eph.ECDH(pub)
	if err != nil {
		return nil, classErrorf(ClassHandshake, "seal", "the key agreement failed", err)
	}
	ephPub := eph.PublicKey().Bytes()
	gcm, err := envelopeKey(shared, ephPub, pub.Bytes())
	if err != nil {
		return nil, classErrorf(ClassHandshake, "seal", "could not derive the envelope key", err)
	}
	nonce := make([]byte, gcmNonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, classErrorf(ClassHandshake, "seal", "could not draw a nonce", err)
	}
	out := make([]byte, 0, len(ephPub)+len(nonce)+len(plain)+gcmTagLen)
	out = append(out, ephPub...)
	out = append(out, nonce...)
	return gcm.Seal(out, nonce, plain, envelopeAD(origin, rid)), nil
}

// OpenEnvelope opens a sealed session envelope. A blob that does not
// authenticate under this origin and rid is refused; there is no lenient
// path that returns a partially trusted inner.
func OpenEnvelope(sealed, recipientPriv []byte, origin, rid string) (SessionInner, error) {
	plain, err := openEnvelopeBytes(sealed, recipientPriv, origin, rid)
	if err != nil {
		return SessionInner{}, err
	}
	var inner SessionInner
	if err := json.Unmarshal(plain, &inner); err != nil {
		return SessionInner{}, classErrorf(ClassHandshake, "open", "the envelope contents are not session JSON", err)
	}
	if inner.Type != sessionOfferType && inner.Type != sessionAnswerType {
		return SessionInner{}, classError(ClassHandshake, "open", "the envelope is not a session offer or answer")
	}
	return inner, nil
}

func openEnvelopeBytes(sealed, recipientPriv []byte, origin, rid string) ([]byte, error) {
	if len(sealed) < p256UncompressedLen+gcmNonceLen+gcmTagLen {
		return nil, classError(ClassHandshake, "open", "the sealed envelope is too short to be one")
	}
	curve := ecdh.P256()
	priv, err := curve.NewPrivateKey(recipientPriv)
	if err != nil {
		return nil, classErrorf(ClassHandshake, "open", "the recipient key is not a P-256 scalar", err)
	}
	ephPub := sealed[:p256UncompressedLen]
	eph, err := curve.NewPublicKey(ephPub)
	if err != nil {
		return nil, classErrorf(ClassHandshake, "open", "the envelope prefix is not an ephemeral P-256 point", err)
	}
	shared, err := priv.ECDH(eph)
	if err != nil {
		return nil, classErrorf(ClassHandshake, "open", "the key agreement failed", err)
	}
	gcm, err := envelopeKey(shared, ephPub, priv.PublicKey().Bytes())
	if err != nil {
		return nil, classErrorf(ClassHandshake, "open", "could not derive the envelope key", err)
	}
	nonce := sealed[p256UncompressedLen : p256UncompressedLen+gcmNonceLen]
	plain, err := gcm.Open(nil, nonce, sealed[p256UncompressedLen+gcmNonceLen:], envelopeAD(origin, rid))
	if err != nil {
		return nil, classError(ClassHandshake, "open", "the sealed envelope did not authenticate for this origin and rendezvous id")
	}
	return plain, nil
}

// HandshakeSession admits a relayed session description for use with local.
//
// Two refusals, in this order, because they are different accusations:
//
//   - The fingerprint the peer sealed must be the fingerprint its own SDP
//     carries. A hub that swapped one for its own is caught here and the
//     connection is refused outright (ClassFingerprint) — no dial, no wait,
//     so the failure can never present as a timeout (AT-FR-16-b).
//   - Otherwise the description is handed to the real WebRTC stack. An SDP
//     the hub altered in flight no longer parses or no longer describes a
//     usable session, and pion says so (ClassHandshake, AT-FR-16-a).
//
// What this does *not* do is complete DTLS with a peer: local is a value
// that came out of an envelope, and the private key that would answer for
// it belongs to the peer connection that minted it. Establishment lives in
// EstablishForDevice / InProcessTunnel, which hold their own peers and run
// a real ICE + DTLS + SCTP handshake. This function is the admission gate
// those paths depend on.
func HandshakeSession(ctx context.Context, local, remote SessionInner) error {
	if err := checkSessionValue(local, "local"); err != nil {
		return err
	}
	if err := checkSessionValue(remote, "relayed"); err != nil {
		return err
	}

	if err := checkRelayedFingerprint(remote); err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		return classErrorf(ClassHandshake, "handshake", "the handshake was cancelled", err)
	}

	// The real stack is the arbiter of whether the relayed description is
	// still the one that was sealed. pion's own SDP parser goes first
	// because it applies to both sides: an answer cannot be handed to a
	// peer that has no offer outstanding, so a parse is the only judgement
	// available for one of the two types.
	desc := webrtc.SessionDescription{Type: sdpTypeOf(remote.Type), SDP: remote.SDP}
	if _, err := desc.Unmarshal(); err != nil {
		return classErrorf(ClassHandshake, "handshake",
			"the relayed session description was refused by the WebRTC stack", err)
	}
	if remote.Type == sessionOfferType {
		// An offer can be judged in full: a fresh peer applies it with
		// nothing of ours to fall back on.
		pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
		if err != nil {
			return classErrorf(ClassHandshake, "handshake", "could not create a peer connection", err)
		}
		defer func() { _ = pc.Close() }()
		if err := pc.SetRemoteDescription(desc); err != nil {
			return classErrorf(ClassHandshake, "handshake",
				"the relayed session description was refused by the WebRTC stack", err)
		}
	}

	// An offer cannot be handshaken against an offer. Reaching here with
	// matching types means the relay delivered our own side back to us.
	if local.Type == remote.Type {
		return classError(ClassHandshake, "handshake",
			"the relayed session description is the same side as ours, so there is no session to complete")
	}
	return nil
}

// checkRelayedFingerprint is the FR-16 comparison: the fingerprint the peer
// sealed against the fingerprint its own SDP carries. The hub can rewrite
// the SDP copy but not the sealed one, so a mismatch names a hub that tried.
// It is a refusal, never a retry or a wait — a substitution must not be able
// to present itself as a timeout (AT-FR-16-b).
func checkRelayedFingerprint(in SessionInner) error {
	sdpFP, ok := fingerprintFromSDP(in.SDP)
	if !ok {
		return classError(ClassFingerprint, "handshake",
			"the relayed session description carries no DTLS fingerprint; refusing the connection")
	}
	if !sameFingerprint(in.Fingerprint, sdpFP) {
		return classError(ClassFingerprint, "handshake",
			"the relayed DTLS fingerprint does not match the session description it arrived with; refusing the connection")
	}
	return nil
}

func checkSessionValue(in SessionInner, which string) error {
	if in.Type != sessionOfferType && in.Type != sessionAnswerType {
		return classError(ClassHandshake, "handshake", "the "+which+" session is not an offer or an answer")
	}
	if err := checkInner(in, in.Type); err != nil {
		return err
	}
	return nil
}

func sdpTypeOf(typ string) webrtc.SDPType {
	if typ == sessionAnswerType {
		return webrtc.SDPTypeAnswer
	}
	return webrtc.SDPTypeOffer
}

// SimulateICEFailure induces FR-24 ICE failure. It is a real failure, not a
// constructed error: two peers are negotiated with every local interface
// filtered away and no ICE servers, so neither side can offer a candidate
// and the real agent gives up.
func SimulateICEFailure(ctx context.Context) error {
	state, err := probeTransport(ctx, transportProbeNoCandidates)
	if err != nil {
		return err
	}
	if state != probeICEFailed {
		return classError(ClassHandshake, "transport",
			"the ICE-failure probe did not fail as expected")
	}
	return classError(ClassICEFailed, "transport", iceFallbackGuidance)
}

// SimulateChannelLoss induces FR-24 connected-then-lost. The channel really
// is established first — ICE, DTLS and SCTP all complete and the data
// channel opens — and only then is the far peer dropped. That ordering is
// the whole distinction from ICE failure, so it must not be faked.
func SimulateChannelLoss(ctx context.Context) error {
	state, err := probeTransport(ctx, transportProbeDropAfterOpen)
	if err != nil {
		return err
	}
	if state != probeLostAfterOpen {
		return classError(ClassHandshake, "transport",
			"the channel-loss probe never established a channel to lose")
	}
	return classError(ClassLost, "transport",
		"the connection was established and then dropped; the computer or this device left the network")
}

// causeOfClass maps a transport failure class to its FR-24 UI state. The
// two constant families are kept string-identical on purpose (AT-FR-24-a
// asserts it) so a renamed class cannot silently keep an old cause.
func causeOfClass(c Class) TransportCause {
	switch c {
	case ClassICEFailed:
		return CauseICEFailed
	case ClassLost:
		return CauseConnectedThenLost
	case ClassUnavailable:
		return CauseRendezvousUnavailable
	case ClassPeerAbsent:
		return CauseComputerOffline
	case ClassRevoked, ClassUnauthorized:
		return CauseAuthFailed
	case ClassHandshake, ClassFingerprint:
		return CauseSignallingRejected
	case ClassTunnelVersion:
		return CauseTunnelVersionMismatch
	default:
		return ""
	}
}

var errNoTransportCause = errors.New("remote: no transport cause")
