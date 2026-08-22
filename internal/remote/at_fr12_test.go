package remote

import (
	"bytes"
	"testing"
)

// AT-FR-12-a: Both ends derive the same SAS from the same transcript.
// Product derivation must match S1 vectors pairing-transcript-v1 and
// pairing-sas-6.
func TestAT_FR_12_a_BothEndsSameSASFromTranscript(t *testing.T) {
	const at = "AT-FR-12-a"
	trVec := s7Construction(t, "pairing-transcript-v1")
	sasVec := s7Construction(t, "pairing-sas-6")
	wantTR := s7Hex(t, trVec["transcript_hex"].(string))
	wantSAS, _ := sasVec["sas"].(string)
	if wantSAS == "" {
		t.Fatalf("%s: pairing-sas-6 has no sas", at)
	}

	origin := DefaultOrigin
	code := "04106105"
	rid := pairingTranscriptRID
	xPub := s7Hex(t, trVec["recipient_pub_hex"].(string))
	yPriv := s7Hex(t, trVec["sender_priv_hex"].(string))
	yPub := s7PubFromPriv(t, yPriv)
	install := s7Hex(t, "d04ab232742bb4ab3a1368bd4615e4e6d0224ab71a016baf8520a332c9778737")
	offerN := bytes.Repeat([]byte{'A'}, 32)
	replyN := bytes.Repeat([]byte{'B'}, 32)
	gotTR, err := BuildPairingTranscript(origin, code, rid, xPub, yPub, install, offerN, replyN)
	if err != nil {
		t.Fatalf("%s: BuildPairingTranscript: %v", at, err)
	}
	if !bytes.Equal(gotTR, wantTR) {
		t.Fatalf("%s: transcript does not match S1 pairing-transcript-v1", at)
	}

	// Device: ECDH(Ypriv, Xpub). Laptop: ECDH(Xpriv, Ypub) with Ypub
	// derived from sender_priv_hex. Xpriv is the S1 fixture private
	// bound to the same recipient_pub (envelope-seal-p256), not invented.
	seal := s7Construction(t, "envelope-seal-p256")
	if seal["recipient_pub_hex"] != trVec["recipient_pub_hex"] {
		t.Fatalf("%s: envelope-seal-p256 recipient_pub is not the pairing X", at)
	}
	xPriv := s7Hex(t, seal["recipient_priv_hex"].(string))
	deviceSAS, err := DerivePairingSAS(yPriv, xPub, wantTR)
	if err != nil {
		t.Fatalf("%s: device DerivePairingSAS: %v", at, err)
	}
	laptopSAS, err := DerivePairingSAS(xPriv, yPub, wantTR)
	if err != nil {
		t.Fatalf("%s: laptop DerivePairingSAS: %v", at, err)
	}
	if laptopSAS != deviceSAS {
		t.Fatalf("%s: laptop SAS %q != device SAS %q", at, laptopSAS, deviceSAS)
	}
	if laptopSAS != wantSAS {
		t.Fatalf("%s: SAS %q, want S1 vector %q", at, laptopSAS, wantSAS)
	}
	if len(laptopSAS) != 6 {
		t.Fatalf("%s: SAS %q is not 6 digits", at, laptopSAS)
	}
}

// AT-FR-12-b: A tampered transcript produces divergent SAS values.
func TestAT_FR_12_b_TamperedTranscriptDivergesSAS(t *testing.T) {
	const at = "AT-FR-12-b"
	sasVec := s7Construction(t, "pairing-sas-6")
	tr := append([]byte(nil), s7Hex(t, sasVec["transcript_hex"].(string))...)
	priv := s7Hex(t, sasVec["sender_priv_hex"].(string))
	pub := s7Hex(t, sasVec["recipient_pub_hex"].(string))

	good, err := DerivePairingSAS(priv, pub, tr)
	if err != nil {
		t.Fatalf("%s: SAS: %v", at, err)
	}
	tr[len(tr)/2] ^= 0xff
	bad, err := DerivePairingSAS(priv, pub, tr)
	if err != nil {
		t.Fatalf("%s: tampered SAS: %v", at, err)
	}
	if good == bad {
		t.Fatalf("%s: tampered transcript produced the same SAS %q", at, good)
	}
}

// AT-FR-12-c: Pairing does not complete if confirmation is withheld.
// SAS is never a password and never auto-confirmed.
func TestAT_FR_12_c_WithheldConfirmationDoesNotComplete(t *testing.T) {
	const at = "AT-FR-12-c"
	c, _, ctx, cancel := s7Enrolled(t)
	defer cancel()

	code, err := c.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("%s: MintPairingCode: %v", at, err)
	}
	if err := c.AcceptPairingOffer(ctx, PairingOffer{Code: code.Code, DeviceID: "phone", Label: "Phone"}); err != nil {
		t.Fatalf("%s: offer: %v", at, err)
	}

	rows := []struct {
		name                    string
		laptopConfirm, deviceOK bool
	}{
		{"neither", false, false},
		{"laptop-only", true, false},
		{"device-only", false, true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			_, err := c.CompletePairing(ctx, code.Code, row.laptopConfirm, row.deviceOK)
			requireClass(t, err, ClassPairUnconfirmed)
			list, lerr := c.PairedDevices()
			if lerr != nil {
				t.Fatalf("%s: PairedDevices: %v", at, lerr)
			}
			if len(list) != 0 {
				t.Fatalf("%s: withheld confirmation persisted a device: %+v", at, list)
			}
		})
	}

	dev, err := c.CompletePairing(ctx, code.Code, true, true)
	if err != nil {
		t.Fatalf("%s: both-sides confirm: %v", at, err)
	}
	if dev.ID == "" {
		t.Fatalf("%s: confirmed pairing returned empty device", at)
	}
}

// pairingTranscriptRID is the 64-hex RID bound into S1 pairing-transcript-v1.
const pairingTranscriptRID = "abababababababababababababababababababababababababababababababab"
