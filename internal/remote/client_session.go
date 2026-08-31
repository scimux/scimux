package remote

// Join 1 — the consumer the S5 wait loop never had.
//
// rvLoop long-polls /v1/wait for every live device. A 200 there carries the
// device's sealed §12.2 session offer; until now postWaitRID read that body,
// checked its content type, and dropped it. This file is what the body is
// handed to: open it under the computer's static P-256 key, negotiate an
// answer, and seal the answer back to the device so the rendezvous can hand
// it to the still-open POST /v1/envelope/<rid>.
//
// Join 2 completed that: the answer now comes from acceptSessionOffer, which
// applies the FR-16 fingerprint binding, keeps its peer connection, and serves
// the tunnel boundary over the data channel the device opens. The session is
// registered as the device's Channel before the answer is sent, so revocation
// severs live traffic instead of only editing a record.
//
// Still outside this file: what the tunnel boundary actually is. handler comes
// from Config, and the S3 boundary is wired into it by internal/app — join 3.

import (
	"context"
	"net/http"
)

// sessionKeyPriv is the computer's static P-256 private key X (§11).
//
// X is minted and owned by the pairing runtime, which is the right home for
// it: it is the same key the pairing transcript binds. This reads it rather
// than keeping a second copy, so there is exactly one X per installation and
// no way for the pairing SAS and the session envelope to disagree about which
// key the computer is. Opening an envelope uses X, so the key is durable
// before this returns.
func (c *Client) sessionKeyPriv() ([]byte, error) {
	if err := c.ensureDurableX(); err != nil {
		return nil, err
	}
	r := pairingOf(c)
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.xPriv) == 0 {
		return nil, classError(ClassHandshake, "session", "the computer has no static key to open the envelope with")
	}
	return append([]byte(nil), r.xPriv...), nil
}

// deviceForRID resolves a rendezvous id to the device's own id and its static
// P-256 public key Y.
//
// Authorisation is checked here rather than after the handshake: a revoked or
// disabled device must not cost a peer connection, and must certainly not
// receive an answer it could dial.
func (c *Client) deviceForRID(rid string) (string, []byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range c.devices {
		if d.RID != rid {
			continue
		}
		if err := c.authDevice(d.ID); err != nil {
			return "", nil, err
		}
		if len(d.ECDHPub) == 0 {
			return "", nil, classError(ClassHandshake, "session",
				"this device has no pairing key on record, so an answer cannot be sealed to it")
		}
		return d.ID, append([]byte(nil), d.ECDHPub...), nil
	}
	return "", nil, classError(ClassNotFound, "session", "no device is registered for this rendezvous id")
}

// answerSessionEnvelope opens a device's sealed session offer and returns the
// sealed answer to post back.
//
// Every failure returns an error and no blob, and the caller sends nothing.
// That is the point: improvising an answer for an envelope that did not
// authenticate would hand a session to whoever posted it, which is precisely
// the attack the §12.2 seal exists to stop. An unopenable envelope is a
// non-event — logged by the caller's ordinary error path, never answered.
func (c *Client) answerSessionEnvelope(ctx context.Context, rid string, sealed []byte) ([]byte, error) {
	priv, err := c.sessionKeyPriv()
	if err != nil {
		return nil, err
	}
	origin := c.origin()

	offer, err := OpenEnvelope(sealed, priv, origin, rid)
	if err != nil {
		return nil, err
	}
	// Resolve the recipient before spending a peer connection on an answer we
	// would have nowhere to send.
	deviceID, peer, err := c.deviceForRID(rid)
	if err != nil {
		return nil, err
	}
	handler := c.tunnelHandler(TunnelPeer{DeviceID: deviceID, RID: rid})
	if handler == nil {
		return nil, classError(ClassUnavailable, "session",
			"this installation serves no tunnel, so a session offer cannot be answered")
	}

	// Join 2: the answer keeps its peer. acceptSessionOffer applies the FR-16
	// fingerprint binding first, so a rendezvous that rewrote the SDP copy is
	// refused here rather than dialled.
	session, answer, err := acceptSessionOffer(ctx, offer, handler, iceServersFromOrigin(origin))
	if err != nil {
		return nil, err
	}
	blob, err := SealEnvelope(answer, peer, origin, rid)
	if err != nil {
		_ = session.Close()
		return nil, err
	}

	// Register before the answer goes out. AttachChannel re-checks
	// authorisation, so a device revoked during the handshake is refused here;
	// and once registered, RevokeDevice closes this session rather than only
	// editing a record.
	if err := c.attachSession(deviceID, session); err != nil {
		_ = session.Close()
		return nil, err
	}
	return blob, nil
}

// TunnelPeer names the device a tunnel boundary is being built for. Both
// fields are already proven by the time it is constructed: the RID carried the
// sealed offer, and the device is the one that RID resolves to.
type TunnelPeer struct {
	DeviceID string
	RID      string
}

// tunnelHandler is the S3 boundary a live session serves, or nil.
func (c *Client) tunnelHandler(peer TunnelPeer) http.Handler {
	c.mu.Lock()
	factory := c.cfg.TunnelHandlerFor
	fallback := c.cfg.TunnelHandler
	c.mu.Unlock()
	if factory != nil {
		return factory(peer)
	}
	return fallback
}

// attachSession registers s as the device's live channel, replacing and
// closing whatever was there.
//
// A device that reconnects — new network, reloaded page — sends a fresh offer
// while the old session may still look alive to us. Leaving the previous one
// registered would mean the next revoke closes the stale session and leaves
// the live one running, which is precisely the FR-29 failure this is meant to
// prevent.
func (c *Client) attachSession(deviceID string, s *Session) error {
	c.mu.Lock()
	prev := c.channels[deviceID]
	c.mu.Unlock()
	if prev != nil && prev != Channel(s) {
		_ = prev.Close()
	}
	return c.AttachChannel(deviceID, s)
}
