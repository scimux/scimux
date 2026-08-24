package remote

// Join 2 — an answer that keeps its peer.
//
// CreateSessionAnswer's contract is to produce a *value*: it mints a peer
// connection, gathers a description, and closes the connection on the way out.
// That is correct for what it is, and every accepted row that calls it means
// exactly that, so it is left alone. The live path needs the opposite — an
// answer whose peer is still there when the device dials it — and that is this
// function.
//
// Two things happen here that CreateSessionAnswer does not do. The first is
// FR-16: checkRelayedFingerprint compares the fingerprint the peer sealed
// against the one its SDP carries. checkInner only asserts the field is
// non-empty, so before this the live path applied descriptions whose DTLS
// identity nothing had verified, while the in-process harness path checked it
// properly. The second is retention: the peer, its data channel, and the codec
// server that binds the channel to the tunnel boundary all outlive the call.
//
// The data channel deliberately cannot be waited for here. It is the device
// that opens it, and the device cannot start until it has the answer this
// function returns — so waiting would deadlock the handshake it is part of.
// The Session is therefore returned not-yet-live and finishes assembling
// itself in the background, which is why ChannelLive exists as a separate
// question from "did the answer succeed".

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/pion/webrtc/v4"

	"codeberg.org/chrberger/scimux/internal/remote/codec"
)

// sessionArrivalDeadline bounds how long a laptop-side session waits for the
// device to finish connecting. Without it an offer from a peer that never
// dials would hold a peer connection and a goroutine until the process exits.
// It is generous because it covers ICE on a bad network, not a protocol step.
const sessionArrivalDeadline = 60 * time.Second

// stunServicePort is the UDP port the rendezvous Binding responder listens
// on (rendezvous-v1 §17). The HTTP origin's port is discarded; this is the
// only port the answering path ever gathers against.
const stunServicePort = "3478"

// iceServersFromOrigin derives the STUN server the answering path gathers
// against from the rendezvous origin the client is already configured to
// talk to. There is no ICE-server discovery endpoint: take the host, drop
// any HTTP port, and use UDP 3478. An origin that will not parse, or has
// no host, yields no servers rather than an error — a session that might
// still work over host candidates is better than one refused outright.
func iceServersFromOrigin(origin string) []webrtc.ICEServer {
	if origin == "" {
		origin = DefaultOrigin
	}
	u, err := url.Parse(origin)
	if err != nil {
		return nil
	}
	host := u.Hostname()
	if host == "" {
		return nil
	}
	return []webrtc.ICEServer{{
		URLs: []string{"stun:" + net.JoinHostPort(host, stunServicePort)},
	}}
}

// acceptSessionOffer answers a device's §12.1 offer and retains the peer.
//
// iceServers is the STUN configuration derived from the rendezvous origin;
// this is the only production peer connection that gathers against one,
// because it is the path that has to reach a device on another network.
//
// The returned Session is the laptop half only: the far end is the device, so
// there is no client-side peer and no RoundTrip. The returned inner is the
// answer to seal back through the rendezvous.
func acceptSessionOffer(ctx context.Context, offer SessionInner, handler http.Handler, iceServers []webrtc.ICEServer) (*Session, SessionInner, error) {
	if handler == nil {
		return nil, SessionInner{}, classError(ClassUnavailable, "accept",
			"this installation serves no tunnel, so a session offer cannot be answered")
	}
	if err := checkInner(offer, sessionOfferType); err != nil {
		return nil, SessionInner{}, err
	}
	// FR-16, on the path that carries traffic rather than beside it.
	if err := checkRelayedFingerprint(offer); err != nil {
		return nil, SessionInner{}, err
	}

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: iceServers})
	if err != nil {
		return nil, SessionInner{}, classErrorf(ClassHandshake, "accept", "could not create a peer connection", err)
	}
	fail := func(e error) (*Session, SessionInner, error) {
		_ = pc.Close()
		return nil, SessionInner{}, e
	}

	// The channel arrives through the callback, so the receiver has to be in
	// place before the remote description is applied.
	dcCh := make(chan *webrtc.DataChannel, 1)
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != tunnelChannelLabel {
			_ = dc.Close()
			return
		}
		select {
		case dcCh <- dc:
		default:
			_ = dc.Close()
		}
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, SDP: offer.SDP,
	}); err != nil {
		return fail(classErrorf(ClassHandshake, "accept", "the relayed offer was refused by the WebRTC stack", err))
	}
	answer, err := localDescription(ctx, pc, sessionAnswerType, func() (webrtc.SessionDescription, error) {
		return pc.CreateAnswer(nil)
	})
	if err != nil {
		return fail(err)
	}

	s := &Session{
		laptop:     pc,
		laptopOnly: true,
		serveDone:  make(chan struct{}),
		// Until the channel opens, the honest FR-24 answer is that ICE has not
		// succeeded. It is cleared on open and replaced if a live channel is
		// later lost.
		cause: CauseICEFailed,
	}
	// Detached from ctx on purpose: ctx is the establishment deadline for the
	// answer, and the session it describes must outlive it.
	serveCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.serveCancel = cancel

	// A peer that fails or goes away must not leave the session claiming to be
	// live. This is the same edge DisconnectPeer models in-process.
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		switch st {
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateDisconnected,
			webrtc.PeerConnectionStateClosed:
			s.mu.Lock()
			if s.laptopDC != nil {
				s.cause = CauseConnectedThenLost
			}
			s.mu.Unlock()
		default:
		}
	})

	go s.serveArrivingChannel(serveCtx, dcCh, handler)
	return s, answer, nil
}

// serveArrivingChannel assembles the laptop half once the device's data
// channel shows up, then serves FR-27 frames off it until the session closes.
//
// Every step re-checks whether the session was closed while it was waiting:
// Close can win the race with a device that connects late, and a channel wired
// in after teardown would be a live tunnel nobody can revoke.
func (s *Session) serveArrivingChannel(ctx context.Context, dcCh <-chan *webrtc.DataChannel, handler http.Handler) {
	defer close(s.serveDone)

	arrive, stop := context.WithTimeout(ctx, sessionArrivalDeadline)
	defer stop()

	var dc *webrtc.DataChannel
	select {
	case dc = <-dcCh:
	case <-arrive.Done():
		// The device never dialled. Tear the peer down rather than holding it
		// open for a session that will not happen.
		_ = s.Close()
		return
	}

	stream := newDCStream(dc)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = stream.Close()
		return
	}
	s.laptopDC, s.laptopStream = dc, stream
	s.mu.Unlock()

	if err := stream.waitOpen(ctx); err != nil {
		_ = s.Close()
		return
	}

	// The laptop is always the responder: it serves, the browser dials.
	srv := codec.NewConn(stream, stream, codec.RoleResponder)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = srv.Close()
		return
	}
	s.srv = srv
	s.cause = ""
	s.mu.Unlock()

	// The laptop is the side that outlives deployments, so it is the side
	// most likely to meet a peer of another major. Serve negotiates before
	// it dispatches anything, and a version verdict is recorded as this
	// session's FR-24 cause rather than dropped as a serve error.
	_ = s.noteTunnelError("serve", srv.Serve(ctx, &httpTunnelHandler{h: handler}))
}
