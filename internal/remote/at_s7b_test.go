package remote

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

// TestAT_S7b_PairingXPersistedAndReloaded: once xPriv feeds SAS, a restart
// must not silently mint a new X and invalidate in-flight pairings.
func TestAT_S7b_PairingXPersistedAndReloaded(t *testing.T) {
	const at = "AT-S7b-persist-x"
	c, _, ctx, cancel := s7Enrolled(t)
	defer cancel()

	code, err := c.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("%s: MintPairingCode: %v", at, err)
	}
	if code.Code == "" {
		t.Fatalf("%s: empty pairing code", at)
	}
	x1, err := c.PairingECDHPublic()
	if err != nil {
		t.Fatalf("%s: PairingECDHPublic: %v", at, err)
	}
	if len(x1) == 0 {
		t.Fatalf("%s: live X is empty", at)
	}

	raw, err := os.ReadFile(c.StatePath())
	if err != nil {
		t.Fatalf("%s: read state: %v", at, err)
	}
	var extra struct {
		PairingXPriv string `json:"pairing_x_priv"`
		PairingXPub  string `json:"pairing_x_pub"`
	}
	if err := json.Unmarshal(raw, &extra); err != nil {
		t.Fatalf("%s: state JSON: %v", at, err)
	}
	if extra.PairingXPriv == "" || extra.PairingXPub == "" {
		t.Fatalf("%s: pairing X not persisted on the identity file (priv=%q pub=%q)", at, extra.PairingXPriv, extra.PairingXPub)
	}
	storedPub, err := hex.DecodeString(extra.PairingXPub)
	if err != nil {
		t.Fatalf("%s: pairing_x_pub hex: %v", at, err)
	}
	if !bytes.Equal(storedPub, x1) {
		t.Fatalf("%s: persisted X pub does not match live PairingECDHPublic", at)
	}
	if extra.PairingXPub == "046a8620064ea5a5629fefc1762c3b84a2aba64bfacdbd50a5719f9383bb2bc8f488c9c7c4586f5c5d0523829105d90409ba6b56c208b69add3182430b9120a354" {
		t.Fatalf("%s: persisted X is the S1 fixture, not the live key", at)
	}

	cfg := c.cfg
	cfg.InviteFile = ""
	cfg.InviteStdin = false
	cfg.InviteString = ""
	if err := c.Close(); err != nil {
		t.Fatalf("%s: close: %v", at, err)
	}
	c2 := NewClient(cfg)
	if err := c2.Start(ctx); err != nil {
		t.Fatalf("%s: restart Start: %v", at, err)
	}
	x2, err := c2.PairingECDHPublic()
	if err != nil {
		t.Fatalf("%s: restarted PairingECDHPublic: %v", at, err)
	}
	if !bytes.Equal(x1, x2) {
		t.Fatalf("%s: restart minted a new X (before %x after %x)", at, x1, x2)
	}
}
