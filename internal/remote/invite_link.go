package remote

import (
	"encoding/hex"
	"fmt"
	"net/url"
)

// The invite link (rendezvous-v1 §11.1).
//
// V1 has no typed pairing path. §11.2 records why: the SAS is
// ECDH(X, Y) and the pairing transcript binds the rendezvous ID, so a
// device that learned only a code has neither the key to derive the six
// digits nor the ID to address the reply. Closing that would mean
// carrying x_pub and rid in the pair-reply and unsealing that reply on
// the typed path — a protocol change, deferred rather than guessed at.
//
// So this is how a pairing starts: the laptop builds one link, the
// device opens it (tapped, or scanned as a QR of the same string), and
// the fragment carries everything §11's transcript needs.
//
// Everything rides in the fragment, never the query. A fragment is not
// sent to the server; a query would reach rv's access log, and §8
// forbids the rendezvous ID on that surface.

// InviteVersion is the fragment's `v`. The /p page refuses a version it
// does not read rather than guessing at the remaining parameters.
const InviteVersion = "1"

// PairingLink builds the invite for a freshly minted pairing code.
//
// The origin is this installation's configured rendezvous origin and is
// repeated in `o` so a link opened against the wrong deployment fails
// loudly at the device instead of pairing somewhere unintended.
func (c *Client) PairingLink(pc PairingCode) (string, error) {
	code, err := normalizePairingCode(pc.Code)
	if err != nil {
		return "", fmt.Errorf("remote: invite: %w", err)
	}
	if !validRID(pc.RID) {
		return "", fmt.Errorf("remote: invite: rendezvous id is not %d lowercase hex characters", RendezvousIDHexLen)
	}
	x, err := c.PairingECDHPublic()
	if err != nil {
		return "", err
	}
	// A device without X cannot compute the SAS, and an invite that
	// carried a truncated or compressed point would fail at the digits
	// rather than here, where there is something to say about it.
	if len(x) != p256UncompressedLen || x[0] != 0x04 {
		return "", fmt.Errorf("remote: invite: laptop key is not an uncompressed P-256 point")
	}

	frag := url.Values{}
	frag.Set("v", InviteVersion)
	frag.Set("o", c.origin())
	frag.Set("c", code)
	frag.Set("r", pc.RID)
	frag.Set("x", hex.EncodeToString(x))
	return c.origin() + "/p#" + frag.Encode(), nil
}
