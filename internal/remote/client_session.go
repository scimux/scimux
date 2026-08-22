package remote

// Join 1 — the consumer the S5 wait loop never had.
//
// rvLoop long-polls /v1/wait for every live device. A 200 there carries the
// device's sealed §12.2 session offer; until now postWaitRID read that body,
// checked its content type, and dropped it. This file is what the body is
// handed to: open it under the laptop's static P-256 key, negotiate an
// answer, and seal the answer back to the device so the rendezvous can hand
// it to the still-open POST /v1/envelope/<rid>.
//
// Scope note, deliberately narrow: this closes the *signalling* path. The
// peer connection CreateSessionAnswer builds is not retained here, so the
// answer is genuine but nothing yet holds the session it describes. Keeping
// the connection alive, applying the FR-16 binding on the live path, and
// serving requests over the data channel is join 2. Splitting it this way
// keeps each half separately testable at the protocol boundary; it is not an
// oversight.

import (
	"context"
)

// sessionKeyPriv is the laptop's static P-256 private key X (§11).
//
// X is minted and owned by the pairing runtime, which is the right home for
// it: it is the same key the pairing transcript binds. This reads it rather
// than keeping a second copy, so there is exactly one X per installation and
// no way for the pairing SAS and the session envelope to disagree about which
// key the laptop is.
func (c *Client) sessionKeyPriv() ([]byte, error) {
	r := pairingOf(c)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.ensureX(); err != nil {
		return nil, err
	}
	if len(r.xPriv) == 0 {
		return nil, classError(ClassHandshake, "session", "the laptop has no static key to open the envelope with")
	}
	return append([]byte(nil), r.xPriv...), nil
}

// deviceECDHPub is the static P-256 public key Y of the device behind rid.
func (c *Client) deviceECDHPub(rid string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range c.devices {
		if d.RID != rid {
			continue
		}
		if len(d.ECDHPub) == 0 {
			return nil, classError(ClassHandshake, "session",
				"this device has no pairing key on record, so an answer cannot be sealed to it")
		}
		return append([]byte(nil), d.ECDHPub...), nil
	}
	return nil, classError(ClassNotFound, "session", "no device is registered for this rendezvous id")
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
	peer, err := c.deviceECDHPub(rid)
	if err != nil {
		return nil, err
	}
	answer, err := CreateSessionAnswer(ctx, offer)
	if err != nil {
		return nil, err
	}
	return SealEnvelope(answer, peer, origin, rid)
}
