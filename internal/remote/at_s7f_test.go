package remote

// Packet S7f — X is durable from the moment anything uses it.
//
// Three minters (PairingECDHPublic, MintPairingCode, sessionKeyPriv) can
// each put X in memory. The invariant is a property of the pairing runtime,
// not of any one entry point: any X that leaves the runtime — returned by
// PairingECDHPublic, or used by sessionKeyPriv to open an envelope — must
// already be on the identity file. A persist failure must leave no X in
// memory, so the next call mints and persists rather than handing back a
// stranded key. A per-call "did I mint it?" flag answers the wrong question.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const (
	s7fMinterPub     = "PairingECDHPublic"
	s7fMinterMint    = "MintPairingCode"
	s7fMinterSession = "sessionKeyPriv"
)

func s7fDiskX(t *testing.T, c *Client) []byte {
	t.Helper()
	raw, err := os.ReadFile(c.StatePath())
	if err != nil {
		t.Fatalf("read identity file: %v", err)
	}
	var extra struct {
		PairingXPub string `json:"pairing_x_pub"`
	}
	if err := json.Unmarshal(raw, &extra); err != nil {
		t.Fatalf("identity JSON: %v", err)
	}
	if extra.PairingXPub == "" {
		return nil
	}
	pub, err := hex.DecodeString(extra.PairingXPub)
	if err != nil {
		t.Fatalf("pairing_x_pub hex: %v", err)
	}
	return pub
}

func s7fLiveX(t *testing.T, c *Client) []byte {
	t.Helper()
	r := pairingOf(c)
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.xPub...)
}

func s7fCallMinter(t *testing.T, c *Client, ctx context.Context, at, name string) []byte {
	t.Helper()
	switch name {
	case s7fMinterPub:
		x, err := c.PairingECDHPublic()
		if err != nil {
			t.Fatalf("%s: %s: %v", at, name, err)
		}
		if len(x) == 0 {
			t.Fatalf("%s: %s returned empty X", at, name)
		}
		return x
	case s7fMinterMint:
		if _, err := c.MintPairingCode(ctx); err != nil {
			t.Fatalf("%s: %s: %v", at, name, err)
		}
		x := s7fLiveX(t, c)
		if len(x) == 0 {
			t.Fatalf("%s: %s left no live X", at, name)
		}
		return x
	case s7fMinterSession:
		priv, err := c.sessionKeyPriv()
		if err != nil {
			t.Fatalf("%s: %s: %v", at, name, err)
		}
		if len(priv) == 0 {
			t.Fatalf("%s: %s returned empty X", at, name)
		}
		return s7PubFromPriv(t, priv)
	default:
		t.Fatalf("%s: unknown minter %q", at, name)
		return nil
	}
}

func s7fCallMinterErr(t *testing.T, c *Client, ctx context.Context, name string) error {
	t.Helper()
	switch name {
	case s7fMinterPub:
		_, err := c.PairingECDHPublic()
		return err
	case s7fMinterMint:
		_, err := c.MintPairingCode(ctx)
		return err
	case s7fMinterSession:
		_, err := c.sessionKeyPriv()
		return err
	default:
		t.Fatalf("unknown minter %q", name)
		return nil
	}
}

func s7fRestart(t *testing.T, c *Client, ctx context.Context, at string) *Client {
	t.Helper()
	cfg := c.cfg
	cfg.InviteFile = ""
	cfg.InviteStdin = false
	cfg.InviteString = ""
	cfg.FailWrite = nil
	if err := c.Close(); err != nil {
		t.Fatalf("%s: close: %v", at, err)
	}
	c2 := NewClient(cfg)
	if err := c2.Start(ctx); err != nil {
		t.Fatalf("%s: restart Start: %v", at, err)
	}
	t.Cleanup(func() { _ = c2.Close() })
	return c2
}

func s7fMustOnDisk(t *testing.T, c *Client, at, step string, want []byte) {
	t.Helper()
	got := s7fDiskX(t, c)
	if len(got) == 0 {
		t.Fatalf("%s: %s left X only in memory; identity file has no pairing_x_pub", at, step)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s: %s persisted X does not match live X", at, step)
	}
	live := s7fLiveX(t, c)
	if !bytes.Equal(live, want) {
		t.Fatalf("%s: %s live X changed under the caller", at, step)
	}
}

func s7fMustEmpty(t *testing.T, c *Client, at, step string) {
	t.Helper()
	if live := s7fLiveX(t, c); len(live) != 0 {
		t.Fatalf("%s: %s left X in memory (%x)", at, step, live)
	}
	if disk := s7fDiskX(t, c); len(disk) != 0 {
		t.Fatalf("%s: %s left pairing_x_pub on disk (%x)", at, step, disk)
	}
}

// TestAT_S7f_XDurableThroughEveryMinterOrder: each of the three minters as
// the first caller, then each of the others, then a restart. X is unchanged
// and present in the identity file after every step — throughout, not only
// at the end. MintPairingCode happens to re-persist, so a check only after
// the full sequence would hide the ordering the current code gets wrong.
func TestAT_S7f_XDurableThroughEveryMinterOrder(t *testing.T) {
	minters := []string{s7fMinterPub, s7fMinterMint, s7fMinterSession}
	for i, first := range minters {
		for j, second := range minters {
			if j == i {
				continue
			}
			for k, third := range minters {
				if k == i || k == j {
					continue
				}
				order := []string{first, second, third}
				name := first + "," + second + "," + third
				t.Run(name, func(t *testing.T) {
					at := "AT-S7f-x-order-" + name
					c, _, ctx, cancel := s7Enrolled(t)
					defer cancel()

					var firstX []byte
					for n, m := range order {
						x := s7fCallMinter(t, c, ctx, at, m)
						if n == 0 {
							firstX = append([]byte(nil), x...)
						} else if !bytes.Equal(x, firstX) {
							t.Fatalf("%s: %s returned a different X than %s", at, m, order[0])
						}
						s7fMustOnDisk(t, c, at, m, firstX)
					}

					c2 := s7fRestart(t, c, ctx, at)
					x2, err := c2.PairingECDHPublic()
					if err != nil {
						t.Fatalf("%s: restarted PairingECDHPublic: %v", at, err)
					}
					if !bytes.Equal(firstX, x2) {
						t.Fatalf("%s: restart minted a new X", at)
					}
					s7fMustOnDisk(t, c2, at, "restart", firstX)
				})
			}
		}
	}
}

// TestAT_S7f_PersistFailureAtEachMinterLeavesNoX: a persist failure at
// the first call through each minter must leave no X in memory. The next
// call mints a different X, persists it, and a restart sees that one.
func TestAT_S7f_PersistFailureAtEachMinterLeavesNoX(t *testing.T) {
	nextOf := map[string]string{
		s7fMinterPub:     s7fMinterMint,
		s7fMinterMint:    s7fMinterSession,
		s7fMinterSession: s7fMinterPub,
	}
	for _, minter := range []string{s7fMinterPub, s7fMinterMint, s7fMinterSession} {
		t.Run(minter, func(t *testing.T) {
			at := "AT-S7f-x-persist-fail-" + minter
			c, _, ctx, cancel := s7Enrolled(t)
			defer cancel()

			c.cfg.FailWrite = &WriteFault{Step: WriteTempCreate}
			err := s7fCallMinterErr(t, c, ctx, minter)
			if err == nil {
				t.Fatalf("%s: %s swallowed a persist failure", at, minter)
			}
			if !strings.Contains(err.Error(), "persist") {
				t.Fatalf("%s: error %q does not name the persist failure", at, err)
			}
			s7fMustEmpty(t, c, at, minter+" persist failure")
			c.cfg.FailWrite = nil

			x1 := s7fCallMinter(t, c, ctx, at, nextOf[minter])
			s7fMustOnDisk(t, c, at, "retry "+nextOf[minter], x1)

			c2 := s7fRestart(t, c, ctx, at)
			x2, err := c2.PairingECDHPublic()
			if err != nil {
				t.Fatalf("%s: restarted PairingECDHPublic: %v", at, err)
			}
			if !bytes.Equal(x1, x2) {
				t.Fatalf("%s: restart did not see the X minted after the persist failure", at)
			}
		})
	}
}

// TestAT_S7f_SecondCallerPersistFailureDoesNotStrandX: the created-flag
// shape answers "did this call mint X?", not "is X on disk?". ensureX
// stores a key; PairingECDHPublic then sees created=false and a persist
// failure does not roll it back. That is the ordering the current code
// gets wrong. A persist failure at this second caller must leave no X
// in memory; the next call mints a different X and persists it.
func TestAT_S7f_SecondCallerPersistFailureDoesNotStrandX(t *testing.T) {
	const at = "AT-S7f-x-strand-second"
	c, _, ctx, cancel := s7Enrolled(t)
	defer cancel()

	r := pairingOf(c)
	r.mu.Lock()
	if _, err := r.ensureX(); err != nil {
		r.mu.Unlock()
		t.Fatalf("%s: ensureX: %v", at, err)
	}
	stranded := append([]byte(nil), r.xPub...)
	r.mu.Unlock()
	if len(stranded) == 0 {
		t.Fatalf("%s: ensureX stored no X", at)
	}
	if disk := s7fDiskX(t, c); len(disk) != 0 {
		t.Fatalf("%s: ensureX wrote X to disk; the strand setup is in-memory only", at)
	}

	c.cfg.FailWrite = &WriteFault{Step: WriteTempCreate}
	err := s7fCallMinterErr(t, c, ctx, s7fMinterPub)
	if err == nil {
		t.Fatalf("%s: PairingECDHPublic swallowed a persist failure for an X it did not mint", at)
	}
	if !strings.Contains(err.Error(), "persist") {
		t.Fatalf("%s: error %q does not name the persist failure", at, err)
	}
	s7fMustEmpty(t, c, at, "second-caller persist failure")
	c.cfg.FailWrite = nil

	x1 := s7fCallMinter(t, c, ctx, at, s7fMinterMint)
	if bytes.Equal(x1, stranded) {
		t.Fatalf("%s: retry handed back the stranded X that ensureX stored", at)
	}
	s7fMustOnDisk(t, c, at, "retry MintPairingCode", x1)

	c2 := s7fRestart(t, c, ctx, at)
	x2, err := c2.PairingECDHPublic()
	if err != nil {
		t.Fatalf("%s: restarted PairingECDHPublic: %v", at, err)
	}
	if !bytes.Equal(x1, x2) {
		t.Fatalf("%s: restart did not see the X minted after the second-caller persist failure", at)
	}
}
