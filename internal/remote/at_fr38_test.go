package remote

import (
	"testing"
	"time"
)

// FR-38 computer half: cancelling an accidental pairing leaves the code
// unconsumed and reusable within its TTL. The web row AT-FR-38-b drives
// the same property through the UI; this pins the protocol transition
// (pair/cancel does not consume — §10.4 / §10.6).
func TestAT_FR_38_b_CancelDoesNotConsumeCode(t *testing.T) {
	const at = "AT-FR-38-b"
	c, _, ctx, cancel := s7Enrolled(t)
	defer cancel()

	code, err := c.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("%s: MintPairingCode: %v", at, err)
	}
	if err := c.AcceptPairingOffer(ctx, PairingOffer{Code: code.Code, DeviceID: "phone"}); err != nil {
		t.Fatalf("%s: first offer: %v", at, err)
	}
	if err := c.CancelPairing(ctx, code.Code); err != nil {
		t.Fatalf("%s: CancelPairing: %v", at, err)
	}
	consumed, err := c.PairingConsumed(code.Code)
	if err != nil {
		t.Fatalf("%s: PairingConsumed after cancel: %v", at, err)
	}
	if consumed {
		t.Fatalf("%s: cancel consumed the code", at)
	}
	if err := c.AcceptPairingOffer(ctx, PairingOffer{Code: code.Code, DeviceID: "phone"}); err != nil {
		t.Fatalf("%s: code not reusable after cancel: %v", at, err)
	}
	st, err := c.PairingState(code.Code)
	if err != nil {
		t.Fatalf("%s: PairingState: %v", at, err)
	}
	if st != PairStateCancelled && st != PairStatePending {
		t.Fatalf("%s: state after cancel = %q, want cancelled or pending (reusable)", at, st)
	}
}

// FR-38: device list with stable labels, duplicate-label handling, and
// paired-at times. A second device with the same label must not rename
// the first.
func TestAT_FR_38_DeviceListStableLabelsDuplicatesPairedAt(t *testing.T) {
	const at = "AT-FR-38"
	c, clk, ctx, cancel := s7Enrolled(t)
	defer cancel()

	code1, err := c.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("%s: mint 1: %v", at, err)
	}
	if err := c.AcceptPairingOffer(ctx, PairingOffer{Code: code1.Code, DeviceID: "phone", Label: "Phone", SignPub: s7MustSignPub(t), DevicePub: s7MustP256Pub(t)}); err != nil {
		t.Fatalf("%s: offer 1: %v", at, err)
	}
	d1, err := c.CompletePairing(ctx, code1.Code, true, true)
	if err != nil {
		t.Fatalf("%s: complete 1: %v", at, err)
	}
	if d1.Label != "Phone" {
		t.Fatalf("%s: first label = %q, want Phone", at, d1.Label)
	}
	if d1.PairedAt.IsZero() {
		t.Fatalf("%s: first paired-at is zero", at)
	}
	firstAt := d1.PairedAt

	clk.Advance(2 * time.Second)
	code2, err := c.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("%s: mint 2: %v", at, err)
	}
	if err := c.AcceptPairingOffer(ctx, PairingOffer{Code: code2.Code, DeviceID: "tablet", Label: "Phone", SignPub: s7MustSignPub(t), DevicePub: s7MustP256Pub(t)}); err != nil {
		t.Fatalf("%s: offer 2: %v", at, err)
	}
	d2, err := c.CompletePairing(ctx, code2.Code, true, true)
	if err != nil {
		t.Fatalf("%s: complete 2: %v", at, err)
	}
	if d2.Label != "Phone" {
		t.Fatalf("%s: duplicate label rewritten to %q", at, d2.Label)
	}
	if d1.ID == d2.ID {
		t.Fatalf("%s: duplicate labels collapsed to one device id", at)
	}

	list, err := c.PairedDevices()
	if err != nil {
		t.Fatalf("%s: PairedDevices: %v", at, err)
	}
	if len(list) != 2 {
		t.Fatalf("%s: device list len %d, want 2", at, len(list))
	}
	var seenFirst, seenSecond bool
	for _, d := range list {
		if d.ID == d1.ID {
			seenFirst = true
			if d.Label != "Phone" {
				t.Fatalf("%s: first device label mutated to %q after duplicate", at, d.Label)
			}
			if !d.PairedAt.Equal(firstAt) {
				t.Fatalf("%s: first paired-at moved from %s to %s", at, firstAt, d.PairedAt)
			}
		}
		if d.ID == d2.ID {
			seenSecond = true
			if d.PairedAt.Before(firstAt) {
				t.Fatalf("%s: second paired-at %s precedes first %s", at, d.PairedAt, firstAt)
			}
		}
	}
	if !seenFirst || !seenSecond {
		t.Fatalf("%s: list missing a paired device: %+v", at, list)
	}
}
