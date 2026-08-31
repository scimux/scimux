package remote

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestAT_S7e_CompletePairingPersistRaceIsAtomic: the FR-38 list and the
// live registry are one transaction in both directions. S7d's persist-failure
// row fails adopt so commit never runs. This row lets adopt persist, then
// races expiry the way a slow disk or a long flock wait does — by advancing
// the pairing clock inside BeforePersist — which is the split S7d created.
//
// After CompletePairing, either both registries hold the device and
// RevokePairedDevice can remove it, or CompletePairing failed and both are
// empty. A live, persisted, unrevocable ghost is the forbidden outcome.
func TestAT_S7e_CompletePairingPersistRaceIsAtomic(t *testing.T) {
	const at = "AT-S7e-adopt-then-commit"
	c, clk, ctx, cancel := s7Enrolled(t)
	defer cancel()

	_, yPub := s7MustP256(t)
	code, err := c.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("%s: mint: %v", at, err)
	}
	if err := c.AcceptPairingOffer(ctx, PairingOffer{
		Code:       code.Code,
		DeviceID:   "phone",
		Label:      "Phone",
		DevicePub:  yPub,
		SignPub:    s7MustSignPub(t),
		OfferNonce: bytes.Repeat([]byte{0x41}, 32),
	}); err != nil {
		t.Fatalf("%s: offer: %v", at, err)
	}

	c.cfg.BeforePersist = func() {
		clk.Advance(PairingTTL)
	}
	_, err = c.CompletePairing(ctx, code.Code, true, true)
	c.cfg.BeforePersist = nil

	paired, perr := c.PairedDevices()
	if perr != nil {
		t.Fatalf("%s: PairedDevices: %v", at, perr)
	}
	list, lerr := c.Devices()
	if lerr != nil {
		t.Fatalf("%s: Devices: %v", at, lerr)
	}
	live := c.liveRIDs()

	if err != nil {
		t.Fatalf("%s: CompletePairing: %v (PairedDevices=%d Devices=%d liveRIDs=%v); expiry during persist must not fail a pairing after the live record is adopted",
			at, err, len(paired), len(list), live)
	}
	if len(paired) != 1 {
		t.Fatalf("%s: FR-38 list after complete = %+v, want the paired device", at, paired)
	}
	if len(list) != 1 {
		t.Fatalf("%s: live registry after complete = %+v, want the paired device", at, list)
	}
	if paired[0].ID != "phone" || list[0].ID != "phone" {
		t.Fatalf("%s: registries disagree on id: paired=%+v live=%+v", at, paired[0], list[0])
	}
	if paired[0].RID != code.RID || list[0].RID != code.RID {
		t.Fatalf("%s: registries disagree on RID: paired=%s live=%s want %s", at, paired[0].RID, list[0].RID, code.RID)
	}
	if !live[code.RID] {
		t.Fatalf("%s: liveRIDs %v does not poll the paired RID %s", at, live, code.RID)
	}

	if err := c.RevokePairedDevice(ctx, "phone"); err != nil {
		t.Fatalf("%s: RevokePairedDevice: %v (live device is not on the FR-38 list)", at, err)
	}
	list, err = c.Devices()
	if err != nil {
		t.Fatalf("%s: Devices after revoke: %v", at, err)
	}
	if len(list) != 0 {
		t.Fatalf("%s: Devices after revoke = %+v, want empty", at, list)
	}
	paired, err = c.PairedDevices()
	if err != nil {
		t.Fatalf("%s: PairedDevices after revoke: %v", at, err)
	}
	if len(paired) != 0 {
		t.Fatalf("%s: FR-38 list after revoke = %+v, want empty", at, paired)
	}
	live = c.liveRIDs()
	if live[code.RID] {
		t.Fatalf("%s: RID %s still polled after revoke: %v", at, code.RID, live)
	}
}

// TestAT_S7e_PairingECDHPublicPersistFailureDoesNotStrandX: ensureX stores
// X before the write. A persist error must not leave that key in RAM with
// created=false, or the next PairingECDHPublic returns an X that a restart
// cannot open envelopes against. MintPairingCode happens to re-persist;
// PairingECDHPublic on its own must recover too.
func TestAT_S7e_PairingECDHPublicPersistFailureDoesNotStrandX(t *testing.T) {
	const at = "AT-S7e-pair-x-strand"
	c, _, ctx, cancel := s7Enrolled(t)
	defer cancel()

	c.cfg.FailWrite = &WriteFault{Step: WriteTempCreate}
	_, err := c.PairingECDHPublic()
	if err == nil {
		t.Fatalf("%s: PairingECDHPublic swallowed a persist failure", at)
	}
	if !strings.Contains(err.Error(), "persist") {
		t.Fatalf("%s: error %q does not name the persist failure", at, err)
	}
	c.cfg.FailWrite = nil

	x1, err := c.PairingECDHPublic()
	if err != nil {
		t.Fatalf("%s: retry PairingECDHPublic: %v", at, err)
	}
	if len(x1) == 0 {
		t.Fatalf("%s: retry returned an empty X", at)
	}

	raw, err := os.ReadFile(c.StatePath())
	if err != nil {
		t.Fatalf("%s: read state: %v", at, err)
	}
	var extra struct {
		PairingXPub string `json:"pairing_x_pub"`
	}
	if err := json.Unmarshal(raw, &extra); err != nil {
		t.Fatalf("%s: state JSON: %v", at, err)
	}
	if extra.PairingXPub == "" {
		t.Fatalf("%s: retry left X only in memory; identity file has no pairing_x_pub", at)
	}
	stored, err := hex.DecodeString(extra.PairingXPub)
	if err != nil {
		t.Fatalf("%s: pairing_x_pub hex: %v", at, err)
	}
	if !bytes.Equal(stored, x1) {
		t.Fatalf("%s: persisted X does not match the X PairingECDHPublic returned after retry", at)
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
	defer c2.Close()
	x2, err := c2.PairingECDHPublic()
	if err != nil {
		t.Fatalf("%s: restarted PairingECDHPublic: %v", at, err)
	}
	if !bytes.Equal(x1, x2) {
		t.Fatalf("%s: restart minted a new X; the retried persist did not land on disk", at)
	}
}
