package remote

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
)

// Unenroll releases this installation at the rendezvous (rendezvous-v1
// §4.4) and then forgets it locally. It is the way out of an enrollment,
// and the reason it exists is symmetry: every other grant in scimux can
// be handed back — a paired device is revoked from the same menu, a node
// is deleted — but the computer's own enrollment could only be abandoned.
//
// The two halves are deliberately not equal partners. The local half is
// unconditional, because the computer that cannot reach the rendezvous is
// exactly the one whose owner wants it to stop trying; making the escape
// hatch depend on the network would break it in the failure mode it is
// most needed in. released reports whether the remote half also happened,
// so the caller can tell the user the honest thing rather than a
// comfortable one: an installation this rendezvous still holds needs an
// operator to revoke it.
//
// §4.4 releases the installation and never the code, so re-enrolling
// afterwards takes a new invite — this leaves the data directory as a
// fresh install would find it, not in a state of its own.
func (c *Client) Unenroll(ctx context.Context) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	c.mu.Lock()
	if !c.loaded {
		// A corrupt or half-written identity is still something the user
		// is entitled to clear, so a load failure is not fatal here: what
		// could not be read simply yields no handle to release.
		_ = c.loadState()
	}
	handle := c.st.Handle
	priv := append(ed25519.PrivateKey(nil), c.priv...)
	origin := c.origin()
	c.mu.Unlock()

	released := false
	if handle != "" && len(priv) == ed25519.PrivateKeySize {
		released = c.postUnenroll(ctx, handle, priv, origin) == nil
	}

	if err := c.forgetInstallation(); err != nil {
		return released, err
	}
	return released, nil
}

// postUnenroll is the §4.4 call: a fresh challenge, then a signature over
// the /v1/unenroll route. The route segment is the whole security of the
// endpoint — a signature made for /v1/verify must not delete the
// installation that made it — and it is the only thing distinguishing
// this message from that one.
func (c *Client) postUnenroll(ctx context.Context, handle string, priv ed25519.PrivateKey, origin string) error {
	chal, err := c.postChallenge(ctx)
	if err != nil {
		return err
	}
	sig := ed25519.Sign(priv, buildAuthMessage(origin, ProtocolVersion, "/v1/unenroll", handle, chal))
	body, _ := json.Marshal(map[string]any{
		"v":         c.requestV(),
		"handle":    handle,
		"challenge": hex.EncodeToString(chal),
		"sig":       hex.EncodeToString(sig),
	})
	resp, err := c.postJSON(ctx, "/v1/unenroll", body)
	if err != nil {
		return classErrorf(ClassUnavailable, "unenroll", "rendezvous is unavailable", err)
	}
	// §4.4 answers the same empty 204 as verify: the release either
	// happened or the caller gets the constant rejection, and there is
	// nothing further to tell them either way.
	_, _ = readBounded(resp.Body, verifyBodyMax)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return classError(ClassUnavailable, "unenroll", "the rendezvous did not release this installation")
	}
	return nil
}

// forgetInstallation is the local half: no more loops, no more devices,
// no identity on disk. The private key goes with it — keeping a secret
// for an installation nobody intends to use again is a cost with no
// remaining benefit.
func (c *Client) forgetInstallation() error {
	_ = c.StopRendezvous()
	return c.withStateLock("unenroll", func() error {
		c.mu.Lock()
		for id, ch := range c.channels {
			if ch != nil {
				_ = ch.Close()
			}
			delete(c.channels, id)
		}
		for id := range c.pending {
			delete(c.pending, id)
			if c.hook().OnPendingErase != nil {
				c.hook().OnPendingErase(id)
			}
		}
		c.devices = c.devices[:0]
		c.st = PersistedState{}
		c.pub = nil
		c.priv = nil
		c.loaded = false
		c.hosted = ""
		c.mu.Unlock()
		c.kickWaiters()

		// The temp file first: a crash between the two removals must not
		// leave a complete enrolled record where the recovery path would
		// replay it back into the installation the user just released.
		if err := os.Remove(c.tempPath()); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remote: unenroll: remove pending identity: %w", err)
		}
		if err := os.Remove(c.StatePath()); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remote: unenroll: remove identity: %w", err)
		}
		return nil
	})
}
