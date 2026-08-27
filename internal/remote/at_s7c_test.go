package remote

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const s1FixtureECDHPub = "046a8620064ea5a5629fefc1762c3b84a2aba64bfacdbd50a5719f9383bb2bc8f488c9c7c4586f5c5d0523829105d90409ba6b56c208b69add3182430b9120a354"

// TestAT_S7c_DeviceECDHPubSurvivesRestart: persist.go must carry the
// device's static P-256 Y both ways. Without it a restart leaves the
// computer able to open a session offer and nowhere to seal the answer.
func TestAT_S7c_DeviceECDHPubSurvivesRestart(t *testing.T) {
	const at = "AT-S7c-ecdh-persist"
	c, _, ctx, cancel := s7Enrolled(t)
	defer cancel()

	_, yPub := s7MustP256(t)
	if hex.EncodeToString(yPub) == s1FixtureECDHPub {
		t.Fatalf("%s: generated Y collided with the S1 fixture", at)
	}

	rec, err := c.RegisterDevice(ctx, DeviceRecord{
		ID:      "phone",
		PubKey:  append([]byte(nil), testPhonePubKey...),
		ECDHPub: append([]byte(nil), yPub...),
	})
	if err != nil {
		t.Fatalf("%s: RegisterDevice: %v", at, err)
	}
	if rec.ID == "" || rec.RID == "" {
		t.Fatalf("%s: registered device missing id/rid: %+v", at, rec)
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

	var got []byte
	for _, d := range c2.devices {
		if d.ID == rec.ID && d.RID == rec.RID {
			got = d.ECDHPub
			break
		}
	}
	if !bytes.Equal(got, yPub) {
		t.Fatalf("%s: reloaded DeviceRecord.ECDHPub %x != registered %x", at, got, yPub)
	}
	if hex.EncodeToString(got) == s1FixtureECDHPub {
		t.Fatalf("%s: reloaded ECDHPub is the S1 fixture, not the registered live key", at)
	}
}

// TestAT_S7c_MismatchedPairingXFailsStart: pairing_x_priv and
// pairing_x_pub must be a corresponding valid P-256 pair. A mismatched
// file currently yields permanent SAS disagreement with no explanation.
func TestAT_S7c_MismatchedPairingXFailsStart(t *testing.T) {
	const at = "AT-S7c-pairing-x-mismatch"
	c, _, ctx, cancel := s7Enrolled(t)
	defer cancel()

	if _, err := c.PairingECDHPublic(); err != nil {
		t.Fatalf("%s: PairingECDHPublic: %v", at, err)
	}

	raw, err := os.ReadFile(c.StatePath())
	if err != nil {
		t.Fatalf("%s: read state: %v", at, err)
	}
	var st map[string]any
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("%s: state JSON: %v", at, err)
	}
	if st["pairing_x_priv"] == nil || st["pairing_x_pub"] == nil {
		t.Fatalf("%s: pairing X not on the identity file; cannot mismatch it", at)
	}
	_, otherPub := s7MustP256(t)
	st["pairing_x_pub"] = hex.EncodeToString(otherPub)
	patched, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}

	cfg := c.cfg
	cfg.InviteFile = ""
	cfg.InviteStdin = false
	cfg.InviteString = ""
	path := c.StatePath()
	if err := c.Close(); err != nil {
		t.Fatalf("%s: close: %v", at, err)
	}
	if err := os.WriteFile(path, patched, 0o600); err != nil {
		t.Fatal(err)
	}

	c2 := NewClient(cfg)
	err = c2.Start(ctx)
	if err == nil {
		t.Fatalf("%s: Start succeeded with mismatched pairing_x_priv/pairing_x_pub", at)
	}
	msg := err.Error()
	if !strings.Contains(msg, "pairing_x") {
		t.Fatalf("%s: Start error %q does not name pairing_x_priv/pairing_x_pub", at, msg)
	}
}

// TestAT_S7c_NoSignPubIsNotACompletedPairing: a pairing that cannot
// produce a live device is not a completed pairing. Missing SignPub used
// to return a PairedDevice and register nothing.
func TestAT_S7c_NoSignPubIsNotACompletedPairing(t *testing.T) {
	const at = "AT-S7c-no-signpub"
	c, _, ctx, cancel := s7Enrolled(t)
	defer cancel()

	code, err := c.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("%s: MintPairingCode: %v", at, err)
	}
	_, yPub := s7MustP256(t)
	if err := c.AcceptPairingOffer(ctx, PairingOffer{
		Code:       code.Code,
		DeviceID:   "phone",
		Label:      "Phone",
		DevicePub:  yPub,
		OfferNonce: bytes.Repeat([]byte{0x41}, 32),
	}); err != nil {
		t.Fatalf("%s: offer: %v", at, err)
	}

	dev, err := c.CompletePairing(ctx, code.Code, true, true)
	if err == nil {
		t.Fatalf("%s: CompletePairing succeeded without SignPub: %+v", at, dev)
	}
	paired, perr := c.PairedDevices()
	if perr != nil {
		t.Fatalf("%s: PairedDevices: %v", at, perr)
	}
	if len(paired) != 0 {
		t.Fatalf("%s: pairing list registered a device without SignPub: %+v", at, paired)
	}
	list, lerr := c.Devices()
	if lerr != nil {
		t.Fatalf("%s: Devices: %v", at, lerr)
	}
	if len(list) != 0 {
		t.Fatalf("%s: live registry registered a device without SignPub: %+v", at, list)
	}
	if len(c.devices) != 0 {
		t.Fatalf("%s: c.devices = %+v, want empty", at, c.devices)
	}
}

// TestAT_S7c_EmptySASIsNotConfirmable: SAS is the pairing's authentication.
// derivePairingSASLocked used to return silently, leaving s.sas == "" as
// something both sides could "confirm". An empty SAS must fail closed.
func TestAT_S7c_EmptySASIsNotConfirmable(t *testing.T) {
	const at = "AT-S7c-empty-sas"
	c, _, ctx, cancel := s7Enrolled(t)
	defer cancel()

	code, err := c.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("%s: MintPairingCode: %v", at, err)
	}
	if err := c.AcceptPairingOffer(ctx, PairingOffer{
		Code:       code.Code,
		DeviceID:   "phone",
		Label:      "Phone",
		SignPub:    s7MustSignPub(t),
		OfferNonce: bytes.Repeat([]byte{0x41}, 32),
	}); err != nil {
		t.Fatalf("%s: offer: %v", at, err)
	}
	st, err := c.PairingSession(code.Code)
	if err != nil {
		t.Fatalf("%s: PairingSession: %v", at, err)
	}
	if st.SAS != "" {
		t.Fatalf("%s: expected empty SAS without device Y, got %q", at, st.SAS)
	}

	dev, err := c.CompletePairing(ctx, code.Code, true, true)
	if err == nil {
		t.Fatalf("%s: CompletePairing confirmed an empty SAS: %+v", at, dev)
	}
	if !strings.Contains(err.Error(), "SAS") {
		t.Fatalf("%s: error %q does not name the SAS derivation failure", at, err)
	}
	paired, perr := c.PairedDevices()
	if perr != nil {
		t.Fatalf("%s: PairedDevices: %v", at, perr)
	}
	if len(paired) != 0 {
		t.Fatalf("%s: empty SAS persisted a pairing: %+v", at, paired)
	}
	list, lerr := c.Devices()
	if lerr != nil {
		t.Fatalf("%s: Devices: %v", at, lerr)
	}
	if len(list) != 0 {
		t.Fatalf("%s: empty SAS registered a live device: %+v", at, list)
	}
	consumed, cerr := c.PairingConsumed(code.Code)
	if cerr != nil {
		t.Fatalf("%s: PairingConsumed: %v", at, cerr)
	}
	if consumed {
		t.Fatalf("%s: empty SAS consumed the code", at)
	}
}

// TestAT_S7c_RevokePairedDeviceReportsPersistFailure: FR-29 is durable
// linearizable revocation; a persist failure must not be swallowed.
func TestAT_S7c_RevokePairedDeviceReportsPersistFailure(t *testing.T) {
	const at = "AT-S7c-revoke-persist"
	c, _, ctx, cancel := s7Enrolled(t)
	defer cancel()

	dev := s7cPair(t, c, ctx, "phone", "Phone")
	c.cfg.FailWrite = &WriteFault{Step: WriteTempCreate}
	err := c.RevokePairedDevice(ctx, dev.ID)
	if err == nil {
		t.Fatalf("%s: RevokePairedDevice swallowed a persist failure", at)
	}
}

// TestAT_S7c_RepairSameIDReplacesLiveRecord: re-pairing a live device id
// replaces the authorized record. The old RID is not polled; revoke-by-id
// hits the current pairing.
func TestAT_S7c_RepairSameIDReplacesLiveRecord(t *testing.T) {
	const at = "AT-S7c-repair-replace"
	c, _, ctx, cancel := s7Enrolled(t)
	defer cancel()

	_, y1 := s7MustP256(t)
	sign1 := s7MustSignPub(t)
	first := s7cPairOffer(t, c, ctx, "phone", "Phone", y1, sign1)
	if first.ID != "phone" {
		t.Fatalf("%s: first id = %q, want phone", at, first.ID)
	}

	_, y2 := s7MustP256(t)
	sign2 := s7MustSignPub(t)
	second := s7cPairOffer(t, c, ctx, "phone", "Phone", y2, sign2)
	if second.ID != "phone" {
		t.Fatalf("%s: second id = %q, want phone (replaced, not renamed)", at, second.ID)
	}
	if second.RID == "" || second.RID == first.RID {
		t.Fatalf("%s: second RID %q did not replace first RID %q", at, second.RID, first.RID)
	}

	list, err := c.Devices()
	if err != nil {
		t.Fatalf("%s: Devices: %v", at, err)
	}
	if len(list) != 1 {
		t.Fatalf("%s: live registry len %d, want 1 after re-pair: %+v", at, len(list), list)
	}
	if list[0].ID != "phone" || list[0].RID != second.RID {
		t.Fatalf("%s: live record %+v, want id=phone rid=%s", at, list[0], second.RID)
	}
	if bytes.Equal(list[0].PubKey, sign1) {
		t.Fatalf("%s: live PubKey is still the first pairing's identity", at)
	}
	if !bytes.Equal(list[0].PubKey, sign2) {
		t.Fatalf("%s: live PubKey is not the new pairing's ed25519 identity", at)
	}
	if !bytes.Equal(list[0].ECDHPub, y2) {
		t.Fatalf("%s: live ECDHPub is not the new pairing Y", at)
	}

	paired, err := c.PairedDevices()
	if err != nil {
		t.Fatalf("%s: PairedDevices: %v", at, err)
	}
	if len(paired) != 1 {
		t.Fatalf("%s: pairing list len %d, want 1 after re-pair: %+v", at, len(paired), paired)
	}
	if paired[0].ID != "phone" || paired[0].RID != second.RID {
		t.Fatalf("%s: pairing list %+v, want id=phone rid=%s", at, paired[0], second.RID)
	}

	live := c.liveRIDs()
	if live[first.RID] {
		t.Fatalf("%s: old RID %s is still polled after re-pair", at, first.RID)
	}
	if !live[second.RID] {
		t.Fatalf("%s: new RID %s is not polled after re-pair", at, second.RID)
	}
}

// TestAT_S7c_DevicesReturnsECDHPub: the public device list must carry Y,
// not just ID/RID/PubKey. Without it a consumer of Devices() has nowhere
// to seal a session answer.
func TestAT_S7c_DevicesReturnsECDHPub(t *testing.T) {
	const at = "AT-S7c-devices-ecdh"
	c, _, ctx, cancel := s7Enrolled(t)
	defer cancel()

	_, yPub := s7MustP256(t)
	if hex.EncodeToString(yPub) == s1FixtureECDHPub {
		t.Fatalf("%s: generated Y collided with the S1 fixture", at)
	}
	_, err := c.RegisterDevice(ctx, DeviceRecord{
		ID:      "phone",
		PubKey:  append([]byte(nil), testPhonePubKey...),
		ECDHPub: append([]byte(nil), yPub...),
	})
	if err != nil {
		t.Fatalf("%s: RegisterDevice: %v", at, err)
	}
	list, err := c.Devices()
	if err != nil {
		t.Fatalf("%s: Devices: %v", at, err)
	}
	if len(list) != 1 {
		t.Fatalf("%s: Devices len %d, want 1", at, len(list))
	}
	if !bytes.Equal(list[0].ECDHPub, yPub) {
		t.Fatalf("%s: Devices() dropped ECDHPub: got %x want %x", at, list[0].ECDHPub, yPub)
	}
	if hex.EncodeToString(list[0].ECDHPub) == s1FixtureECDHPub {
		t.Fatalf("%s: Devices() ECDHPub is the S1 fixture", at)
	}
}

func s7cPair(t *testing.T, c *Client, ctx context.Context, id, label string) PairedDevice {
	t.Helper()
	_, yPub := s7MustP256(t)
	return s7cPairOffer(t, c, ctx, id, label, yPub, s7MustSignPub(t))
}

func s7cPairOffer(t *testing.T, c *Client, ctx context.Context, id, label string, yPub, signPub []byte) PairedDevice {
	t.Helper()
	code, err := c.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}
	if err := c.AcceptPairingOffer(ctx, PairingOffer{
		Code:       code.Code,
		DeviceID:   id,
		Label:      label,
		DevicePub:  yPub,
		SignPub:    signPub,
		OfferNonce: bytes.Repeat([]byte{0x41}, 32),
	}); err != nil {
		t.Fatalf("AcceptPairingOffer: %v", err)
	}
	dev, err := c.CompletePairing(ctx, code.Code, true, true)
	if err != nil {
		t.Fatalf("CompletePairing: %v", err)
	}
	return dev
}
