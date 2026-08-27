package remote

// S6 — the live session: two real peers, one data channel, FR-27 frames.
//
// The computer half of remote access never speaks HTTP over the wire. It
// speaks the FR-27 codec (internal/remote/codec) over a WebRTC data
// channel, and the codec's server side hands each request to the *tunnel*
// boundary from S3 — not the browser boundary. Session is the client end of
// that pair and is an http.RoundTripper, so a caller drives it with ordinary
// *http.Request values and never sees pion.

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"sync"

	"github.com/pion/webrtc/v4"

	"codeberg.org/chrberger/scimux/internal/remote/codec"
)

// tunnelChannelLabel is the single data channel every session opens. One
// session = one channel: the codec multiplexes requests by id, so a second
// channel would be a second, unframed way in.
const tunnelChannelLabel = "scimux"

// Session is one in-process WebRTC pair whose data channel carries FR-27
// frames. No pion type is exported.
type Session struct {
	mu     sync.Mutex
	closed bool

	client     *webrtc.PeerConnection
	computer   *webrtc.PeerConnection
	clientDC   *webrtc.DataChannel
	computerDC *webrtc.DataChannel

	clientStream   *dcStream
	computerStream *dcStream

	conn *codec.Conn // client side: RoundTrip
	srv  *codec.Conn // computer side: Serve

	serveCancel context.CancelFunc
	serveDone   chan struct{}

	hub      *signallingHub
	deviceID string
	offers   int

	// computerOnly marks a session whose far end is a real device rather than an
	// in-process client peer (acceptSessionOffer). Such a session has no
	// client half at all, so the questions "is the channel live" and "can this
	// round-trip" have different answers here than for InProcessTunnel — and
	// the flag says which shape is meant rather than inferring it from a nil,
	// which a half-built in-process session would also satisfy.
	computerOnly bool

	cause TransportCause
}

var _ http.RoundTripper = (*Session)(nil)
var _ Channel = (*Session)(nil)

// InProcessTunnel is two in-process peers (AT-int-a) whose data channel
// carries FR-27 frames to handler. handler is the tunnel boundary, not the
// browser boundary.
//
// The two peers negotiate through an in-process signalling hub that relays
// only sealed §12.2 envelopes, so the same seal/open and FR-16 fingerprint
// checks the real rendezvous path uses are exercised here rather than
// bypassed for convenience.
func InProcessTunnel(ctx context.Context, handler http.Handler) (*Session, error) {
	return inProcessTunnelVia(ctx, newSignallingHub(), handler)
}

// inProcessTunnelVia is InProcessTunnel over a caller-supplied hub. It
// exists so a test can hand in a hub that misbehaves; the exported entry
// point above always mints an honest one.
func inProcessTunnelVia(ctx context.Context, hub *signallingHub, handler http.Handler) (*Session, error) {
	if handler == nil {
		return nil, classError(ClassHandshake, "tunnel", "a tunnel needs a handler to serve")
	}
	pair, err := negotiatePair(ctx, webrtc.NewAPI(), hub)
	if err != nil {
		return nil, err
	}

	s := &Session{
		client:         pair.client,
		computer:       pair.computer,
		clientDC:       pair.clientDC,
		computerDC:     pair.computerDC,
		clientStream:   pair.clientStream,
		computerStream: pair.computerStream,
		hub:            hub,
		offers:         pair.offers,
		serveDone:      make(chan struct{}),
	}

	// The computer end serves; the client end dials. The serve context is
	// detached from ctx on purpose: ctx is the *establishment* deadline, and
	// a session must outlive the call that set it up.
	serveCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.serveCancel = cancel
	s.srv = codec.NewConn(pair.computerStream, pair.computerStream, codec.RoleResponder)
	s.conn = codec.NewConn(pair.clientStream, pair.clientStream, codec.RoleInitiator)
	go func() {
		defer close(s.serveDone)
		_ = s.srv.Serve(serveCtx, &httpTunnelHandler{h: handler})
	}()

	// The peer holds a wait slot on the hub for as long as it is connected;
	// that is what a disconnect has to remove (AT-FR-26-c).
	hub.addWaiter(tunnelChannelLabel)
	return s, nil
}

// EstablishForDevice is InProcessTunnel bound to an enrolled device so
// local revoke and signalling disconnect can act on the live channel.
//
// Authorisation is checked before the handshake and again when the channel
// is attached: a device that is unknown, revoked, or disabled never gets a
// session, and the second check closes the window between them.
func EstablishForDevice(ctx context.Context, c *Client, deviceID string, handler http.Handler) (*Session, error) {
	if c == nil {
		return nil, classError(ClassUnauthorized, "establish", "no remote client")
	}
	c.mu.Lock()
	err := c.authDevice(deviceID)
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}

	s, err := InProcessTunnel(ctx, handler)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.deviceID = deviceID
	s.mu.Unlock()
	s.hub.replaceWaiter(tunnelChannelLabel, deviceID)

	if err := c.AttachChannel(deviceID, s); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// RoundTrip issues one origin-relative request over the data channel.
func (s *Session) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, classError(ClassHandshake, "round-trip", "no request")
	}
	s.mu.Lock()
	conn, closed, one := s.conn, s.closed, s.computerOnly
	s.mu.Unlock()
	if one {
		// This session serves; it does not dial. The requesting end is the
		// device, and there is no client-side codec conn to drive.
		return nil, classError(ClassUnavailable, "round-trip",
			"this session serves a remote device and has no outbound request path")
	}
	if closed || conn == nil {
		return nil, classError(ClassPeerAbsent, "round-trip", "the tunnel is closed")
	}
	if !s.ChannelLive() {
		return nil, classError(ClassPeerAbsent, "round-trip", "the data channel is not open")
	}

	ctx := req.Context()
	creq, err := codecRequest(req)
	if err != nil {
		return nil, err
	}
	resp, err := conn.RoundTrip(ctx, creq)
	if err != nil {
		// §7 step 0 runs inside RoundTrip, so a MAJOR mismatch arrives here
		// before any request byte was written. It has to become the named
		// FR-24 state rather than reaching the UI as a frame error.
		return nil, s.noteTunnelError("round-trip", err)
	}
	return httpResponse(req, resp), nil
}

// Close tears down the in-process peers. It is idempotent: revoke closes a
// session through the Channel interface, and the caller's own deferred
// Close must not then report a failure.
func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	cancel := s.serveCancel
	conn, srv := s.conn, s.srv
	client, computer := s.client, s.computer
	clientStream, computerStream := s.clientStream, s.computerStream
	device := s.deviceID
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if conn != nil {
		_ = conn.Close()
	}
	if srv != nil {
		_ = srv.Close()
	}
	if clientStream != nil {
		_ = clientStream.Close()
	}
	if computerStream != nil {
		_ = computerStream.Close()
	}
	if client != nil {
		_ = client.Close()
	}
	if computer != nil {
		_ = computer.Close()
	}
	if s.hub != nil {
		if device != "" {
			s.hub.removeWaiter(device)
		}
		s.hub.removeWaiter(tunnelChannelLabel)
	}
	return nil
}

// Closed reports whether this session has been torn down. It is the
// Channel half of Close and is what the revoke path reads.
func (s *Session) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Send is the Channel raw-message path, which a framed tunnel does not
// have: every byte on this data channel belongs to an FR-27 frame, and
// injecting an unframed message would desynchronise the codec. The S5
// delivery path uses it against fake channels; a real session refuses it
// rather than corrupting the stream.
func (s *Session) Send([]byte) error {
	return classError(ClassUnavailable, "send",
		"this channel carries framed tunnel requests and has no raw-message path")
}

// ChannelLive reports whether the data channel is open.
func (s *Session) ChannelLive() bool {
	s.mu.Lock()
	closed, one := s.closed, s.computerOnly
	clientDC, computerDC := s.clientDC, s.computerDC
	s.mu.Unlock()
	if closed || computerDC == nil {
		return false
	}
	if computerDC.ReadyState() != webrtc.DataChannelStateOpen {
		return false
	}
	if one {
		// The client end is the device, out of this process. The computer's own
		// channel being open is the whole of what can be observed here.
		return true
	}
	return clientDC != nil && clientDC.ReadyState() == webrtc.DataChannelStateOpen
}

// RestartRendezvous restarts the signalling hub without touching the
// established data channel (NFR-06).
//
// That the channel survives is not an accident of this implementation: once
// ICE, DTLS and SCTP are up, the peers talk to each other and the hub is
// out of the path. The restart drops every hub-side wait, and the still
// connected peer re-registers its wait against the new hub — which is
// exactly what it must *not* need a new session-offer to do (NFR-07).
func (s *Session) RestartRendezvous() error {
	s.mu.Lock()
	closed := s.closed
	hub, device := s.hub, s.deviceID
	s.mu.Unlock()
	if closed || hub == nil {
		return classError(ClassPeerAbsent, "restart", "the tunnel is closed")
	}
	hub.restart()
	if s.ChannelLive() {
		id := device
		if id == "" {
			id = tunnelChannelLabel
		}
		hub.addWaiter(id)
	}
	return nil
}

// DisconnectPeer drops the client so the hub must remove the waiter
// (AT-FR-26-c). The peer is really closed; the waiter is removed because
// the peer went away, not instead of it going away.
func (s *Session) DisconnectPeer() error {
	s.mu.Lock()
	hub, client, device := s.hub, s.client, s.deviceID
	s.mu.Unlock()
	if client != nil {
		if err := client.Close(); err != nil {
			return classErrorf(ClassPeerAbsent, "disconnect", "could not drop the peer", err)
		}
	}
	if hub != nil {
		if device != "" {
			hub.removeWaiter(device)
		}
		hub.removeWaiter(tunnelChannelLabel)
	}
	s.mu.Lock()
	s.cause = CauseConnectedThenLost
	s.mu.Unlock()
	return nil
}

// SignallingWaiters is the hub's live waiter count for this session.
func (s *Session) SignallingWaiters() int {
	s.mu.Lock()
	hub := s.hub
	s.mu.Unlock()
	if hub == nil {
		return 0
	}
	return hub.waiterCount()
}

// OfferCount is the number of session-offer envelopes this session posted.
func (s *Session) OfferCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.offers
}

// TransportCause is the current FR-24 cause, or empty if connected.
func (s *Session) TransportCause() (TransportCause, error) {
	if s.ChannelLive() {
		return "", nil
	}
	s.mu.Lock()
	cause := s.cause
	s.mu.Unlock()
	if cause == "" {
		return "", classError(ClassPeerAbsent, "transport-cause", "the tunnel is closed")
	}
	return cause, nil
}

// signallingHub is the in-process stand-in for my.scimux.eu: it relays
// sealed envelopes it cannot read and tracks who is waiting. It is not a
// model of the hub's HTTP surface — that is the rv repository's job — only
// of the two properties S6 must demonstrate: that a restart loses hub state
// and nothing else, and that a disconnected peer stops being a waiter.
type signallingHub struct {
	mu      sync.Mutex
	gen     int
	waiters map[string]struct{}
	mailbox map[string][]byte

	// tamper rewrites a relayed inner after it is opened. A real hub cannot
	// do this — §12.2 seals the inner to the recipient's static key, and the
	// hub has no key — so this models the wider case FR-16 actually defends
	// against: an inner that reaches the far side with a fingerprint that
	// does not match its SDP, whatever produced it. The check must not be
	// load-bearing on the seal alone.
	tamper func(SessionInner) SessionInner
}

func newSignallingHub() *signallingHub {
	return &signallingHub{
		waiters: make(map[string]struct{}),
		mailbox: make(map[string][]byte),
	}
}

func (h *signallingHub) post(slot string, sealed []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cp := make([]byte, len(sealed))
	copy(cp, sealed)
	h.mailbox[slot] = cp
}

func (h *signallingHub) take(slot string) ([]byte, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b, ok := h.mailbox[slot]
	if !ok {
		return nil, false
	}
	delete(h.mailbox, slot)
	return b, true
}

func (h *signallingHub) addWaiter(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.waiters[id] = struct{}{}
}

func (h *signallingHub) replaceWaiter(old, id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.waiters, old)
	h.waiters[id] = struct{}{}
}

func (h *signallingHub) removeWaiter(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.waiters, id)
}

// tamperInner applies the hub's rewrite, if it has one.
func (h *signallingHub) tamperInner(in SessionInner) SessionInner {
	h.mu.Lock()
	f := h.tamper
	h.mu.Unlock()
	if f == nil {
		return in
	}
	return f(in)
}

func (h *signallingHub) waiterCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.waiters)
}

// restart discards everything the hub held. A hub that kept its waiters
// across a restart would make AT-NFR-06-a pass without proving anything.
func (h *signallingHub) restart() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gen++
	h.waiters = make(map[string]struct{})
	h.mailbox = make(map[string][]byte)
}

// peerPair is one negotiated pair, before it is wrapped in a Session.
type peerPair struct {
	client         *webrtc.PeerConnection
	computer       *webrtc.PeerConnection
	clientDC       *webrtc.DataChannel
	computerDC     *webrtc.DataChannel
	clientStream   *dcStream
	computerStream *dcStream
	offers         int
}

// negotiatePair runs a real offer/answer through hub, sealing each side's
// §12.1 inner into a §12.2 envelope addressed to the other. hub may be nil,
// in which case the descriptions are handed over directly — the probes use
// that path, because they are testing ICE, not signalling.
func negotiatePair(ctx context.Context, api *webrtc.API, hub *signallingHub) (*peerPair, error) {
	client, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, classErrorf(ClassHandshake, "negotiate", "could not create the client peer", err)
	}
	computer, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		_ = client.Close()
		return nil, classErrorf(ClassHandshake, "negotiate", "could not create the computer peer", err)
	}
	fail := func(e error) (*peerPair, error) {
		_ = client.Close()
		_ = computer.Close()
		return nil, e
	}

	// The computer's inbound channel arrives through the callback, so the
	// stream that wraps it has to be built there, before it can open.
	computerDCCh := make(chan *webrtc.DataChannel, 1)
	computer.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != tunnelChannelLabel {
			_ = dc.Close()
			return
		}
		select {
		case computerDCCh <- dc:
		default:
			_ = dc.Close()
		}
	})

	clientDC, err := client.CreateDataChannel(tunnelChannelLabel, nil)
	if err != nil {
		return fail(classErrorf(ClassHandshake, "negotiate", "could not create the data channel", err))
	}
	clientStream := newDCStream(clientDC)

	offerInner, err := localDescription(ctx, client, sessionOfferType, func() (webrtc.SessionDescription, error) {
		return client.CreateOffer(nil)
	})
	if err != nil {
		return fail(err)
	}

	offers := 0
	answerInner, err := func() (SessionInner, error) {
		if hub == nil {
			if err := computer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offerInner.SDP}); err != nil {
				return SessionInner{}, classErrorf(ClassHandshake, "negotiate", "the offer was refused", err)
			}
			return localDescription(ctx, computer, sessionAnswerType, func() (webrtc.SessionDescription, error) {
				return computer.CreateAnswer(nil)
			})
		}
		return relayThroughHub(ctx, hub, computer, offerInner, &offers)
	}()
	if err != nil {
		return fail(err)
	}

	// FR-16 on the way back: the answer the hub handed us must carry the
	// fingerprint its own SDP does, or the connection is refused before the
	// description is ever applied.
	if err := HandshakeSession(ctx, offerInner, answerInner); err != nil {
		return fail(err)
	}
	if err := client.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answerInner.SDP}); err != nil {
		return fail(classErrorf(ClassHandshake, "negotiate", "the answer was refused", err))
	}

	var computerDC *webrtc.DataChannel
	select {
	case computerDC = <-computerDCCh:
	case <-ctx.Done():
		return fail(classErrorf(ClassHandshake, "negotiate", "the computer never saw the data channel", ctx.Err()))
	}
	computerStream := newDCStream(computerDC)

	if err := clientStream.waitOpen(ctx); err != nil {
		return fail(err)
	}
	if err := computerStream.waitOpen(ctx); err != nil {
		return fail(err)
	}

	return &peerPair{
		client:         client,
		computer:       computer,
		clientDC:       clientDC,
		computerDC:     computerDC,
		clientStream:   clientStream,
		computerStream: computerStream,
		offers:         offers,
	}, nil
}

// relayThroughHub posts the sealed offer, opens it on the computer side, and
// returns the computer's sealed-and-reopened answer. The hub only ever holds
// ciphertext: that is the property FR-16 exists to keep true, so the
// in-process path proves it rather than assuming it.
func relayThroughHub(ctx context.Context, hub *signallingHub, computer *webrtc.PeerConnection, offerInner SessionInner, offers *int) (SessionInner, error) {
	curve := ecdh.P256()
	computerKey, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return SessionInner{}, classErrorf(ClassHandshake, "negotiate", "could not mint the computer session key", err)
	}
	clientKey, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return SessionInner{}, classErrorf(ClassHandshake, "negotiate", "could not mint the client session key", err)
	}
	rid, err := randomHex(RendezvousIDHexLen / 2)
	if err != nil {
		return SessionInner{}, classErrorf(ClassHandshake, "negotiate", "could not mint a rendezvous id", err)
	}

	sealedOffer, err := SealEnvelope(offerInner, computerKey.PublicKey().Bytes(), DefaultOrigin, rid)
	if err != nil {
		return SessionInner{}, err
	}
	hub.post("offer", sealedOffer)
	*offers++

	blob, ok := hub.take("offer")
	if !ok {
		return SessionInner{}, classError(ClassUnavailable, "negotiate", "the hub lost the session offer")
	}
	opened, err := OpenEnvelope(blob, computerKey.Bytes(), DefaultOrigin, rid)
	if err != nil {
		return SessionInner{}, err
	}
	opened = hub.tamperInner(opened)
	// The computer has no local description yet, so the FR-16 check available
	// on this side is the fingerprint binding — which is the check that
	// catches a substituting hub. The SDP itself is judged by the stack on
	// the next line.
	if err := checkRelayedFingerprint(opened); err != nil {
		return SessionInner{}, err
	}
	if err := computer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: opened.SDP}); err != nil {
		return SessionInner{}, classErrorf(ClassHandshake, "negotiate", "the relayed offer was refused", err)
	}
	answerInner, err := localDescription(ctx, computer, sessionAnswerType, func() (webrtc.SessionDescription, error) {
		return computer.CreateAnswer(nil)
	})
	if err != nil {
		return SessionInner{}, err
	}
	sealedAnswer, err := SealEnvelope(answerInner, clientKey.PublicKey().Bytes(), DefaultOrigin, rid)
	if err != nil {
		return SessionInner{}, err
	}
	hub.post("answer", sealedAnswer)
	blob, ok = hub.take("answer")
	if !ok {
		return SessionInner{}, classError(ClassUnavailable, "negotiate", "the hub lost the session answer")
	}
	return OpenEnvelope(blob, clientKey.Bytes(), DefaultOrigin, rid)
}

// localDescription creates a description, applies it, waits for gathering
// to finish, and lifts it into §12.1 form.
func localDescription(ctx context.Context, pc *webrtc.PeerConnection, typ string, create func() (webrtc.SessionDescription, error)) (SessionInner, error) {
	desc, err := create()
	if err != nil {
		return SessionInner{}, classErrorf(ClassHandshake, "negotiate", "could not create the "+typ, err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(desc); err != nil {
		return SessionInner{}, classErrorf(ClassHandshake, "negotiate", "could not apply the "+typ, err)
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		return SessionInner{}, classErrorf(ClassHandshake, "negotiate", "candidate gathering did not finish", ctx.Err())
	}
	return innerFromDescription(typ, pc.LocalDescription())
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// onceCloser closes a signal channel exactly once. pion fires state
// callbacks from more than one goroutine, so a bare close() would panic on
// the second edge.
type onceCloser struct{ once sync.Once }

func (o *onceCloser) close(ch chan struct{}) {
	o.once.Do(func() { close(ch) })
}

var _ io.ReadWriteCloser = (*dcStream)(nil)
