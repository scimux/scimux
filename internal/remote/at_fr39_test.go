package remote

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

// AT-FR-39-b: Simultaneous attacker and legitimate offers resolve to the
// legitimate one or to failure — never to the attacker — and
// role/reflection attacks are rejected.
func TestAT_FR_39_b_AttackerNeverWinsRoleAndReflectionRejected(t *testing.T) {
	const at = "AT-FR-39-b"
	c, _, ctx, cancel := s7Enrolled(t)
	defer cancel()

	code, err := c.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("%s: MintPairingCode: %v", at, err)
	}

	_, legitPub := s7MustP256(t)
	_, atkPub := s7MustP256(t)
	if bytes.Equal(legitPub, atkPub) {
		t.Fatalf("%s: fixture key material collided", at)
	}
	legitNonce := bytes.Repeat([]byte{0x11}, 32)
	atkNonce := bytes.Repeat([]byte{0x22}, 32)
	legitEnv, err := json.Marshal(map[string]any{
		"v": 1, "type": "pair-offer", "device_pub": hex.EncodeToString(legitPub),
	})
	if err != nil {
		t.Fatal(err)
	}
	atkEnv, err := json.Marshal(map[string]any{
		"v": 1, "type": "pair-offer", "device_pub": hex.EncodeToString(atkPub),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Same code, same device id and label; the offers differ only in key material.
	legit := PairingOffer{Code: code.Code, DeviceID: "phone", Label: "Phone", DevicePub: legitPub, SignPub: s7MustSignPub(t), OfferNonce: legitNonce, Envelope: legitEnv}
	attacker := PairingOffer{Code: code.Code, DeviceID: "phone", Label: "Phone", DevicePub: atkPub, SignPub: s7MustSignPub(t), OfferNonce: atkNonce, Envelope: atkEnv}

	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs[0] = c.AcceptPairingOffer(ctx, legit)
	}()
	go func() {
		defer wg.Done()
		errs[1] = c.AcceptPairingOffer(ctx, attacker)
	}()
	wg.Wait()
	for i, e := range errs {
		if errors.Is(e, ErrUnimplemented) {
			t.Fatalf("%s: AcceptPairingOffer[%d]: %v", at, i, e)
		}
	}

	dev, err := c.CompletePairing(ctx, code.Code, true, true)
	if err != nil {
		if errors.Is(err, ErrUnimplemented) {
			t.Fatalf("%s: CompletePairing: %v", at, err)
		}
		// Failure is allowed. Completing as the attacker is not.
	} else if bytes.Equal(dev.PubKey, atkPub) {
		t.Fatalf("%s: attacker key material won", at)
	} else if !bytes.Equal(dev.PubKey, legitPub) {
		t.Fatalf("%s: winner pub is not the legitimate key material", at)
	}

	x, err := c.PairingECDHPublic()
	if err != nil {
		t.Fatalf("%s: PairingECDHPublic: %v", at, err)
	}
	if len(x) == 0 {
		t.Fatalf("%s: laptop pairing public key is empty", at)
	}
	roleEnv, err := json.Marshal(map[string]any{
		"v": 1, "type": "pair-reply", "install_pub": hex.EncodeToString(x),
	})
	if err != nil {
		t.Fatal(err)
	}
	roleCode, err := c.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("%s: mint role-swap code: %v", at, err)
	}
	_, rolePub := s7MustP256(t)
	role := PairingOffer{Code: roleCode.Code, DeviceID: "phone", Label: "Phone", DevicePub: rolePub, OfferNonce: bytes.Repeat([]byte{0x31}, 32), Envelope: roleEnv}
	requireClass(t, c.AcceptPairingOffer(ctx, role), ClassPairRole)

	reflCode, err := c.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("%s: mint reflection code: %v", at, err)
	}
	refl := PairingOffer{Code: reflCode.Code, DeviceID: "phone", Label: "Phone", DevicePub: append([]byte(nil), x...), OfferNonce: bytes.Repeat([]byte{0x33}, 32), Envelope: []byte(`{"v":1,"type":"pair-offer"}`)}
	requireClass(t, c.AcceptPairingOffer(ctx, refl), ClassPairReflection)
}

// AT-FR-39-c: The single-use moment is exact: a code is consumed at the
// specified transition (laptop reply / both-sided confirmation, or TTL)
// and not before — not at offer, not at cancel — proven by driving a
// failure just either side of it.
func TestAT_FR_39_c_SingleUseMomentExact(t *testing.T) {
	const at = "AT-FR-39-c"
	c, clk, ctx, cancel := s7Enrolled(t)
	defer cancel()

	code, err := c.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("%s: MintPairingCode: %v", at, err)
	}

	if err := c.AcceptPairingOffer(ctx, PairingOffer{Code: code.Code, DeviceID: "phone"}); err != nil {
		t.Fatalf("%s: offer must not consume: %v", at, err)
	}
	consumed, err := c.PairingConsumed(code.Code)
	if err != nil {
		t.Fatalf("%s: PairingConsumed after offer: %v", at, err)
	}
	if consumed {
		t.Fatalf("%s: code consumed at offer; the transition is reply or TTL", at)
	}

	if err := c.CancelPairing(ctx, code.Code); err != nil {
		t.Fatalf("%s: cancel: %v", at, err)
	}
	consumed, err = c.PairingConsumed(code.Code)
	if err != nil {
		t.Fatalf("%s: PairingConsumed after cancel: %v", at, err)
	}
	if consumed {
		t.Fatalf("%s: code consumed at cancel", at)
	}

	if err := c.AcceptPairingOffer(ctx, PairingOffer{Code: code.Code, DeviceID: "phone", SignPub: s7MustSignPub(t), DevicePub: s7MustP256Pub(t)}); err != nil {
		t.Fatalf("%s: reusable after cancel: %v", at, err)
	}

	// Just before confirmation: still unconsumed.
	consumed, err = c.PairingConsumed(code.Code)
	if err != nil {
		t.Fatalf("%s: PairingConsumed before confirm: %v", at, err)
	}
	if consumed {
		t.Fatalf("%s: consumed before the reply/confirm transition", at)
	}

	_, err = c.CompletePairing(ctx, code.Code, true, true)
	if err != nil {
		t.Fatalf("%s: confirm: %v", at, err)
	}
	consumed, err = c.PairingConsumed(code.Code)
	if err != nil {
		t.Fatalf("%s: PairingConsumed after confirm: %v", at, err)
	}
	if !consumed {
		t.Fatalf("%s: code not consumed at the reply/confirm transition", at)
	}
	_, err = c.CompletePairing(ctx, code.Code, true, true)
	requireClass(t, err, ClassPairConsumed)

	// TTL path: a fresh code, advance the clock, consumed-by-expiry.
	code2, err := c.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("%s: mint 2: %v", at, err)
	}
	clk.Advance(PairingTTL)
	consumed, err = c.PairingConsumed(code2.Code)
	if err != nil {
		t.Fatalf("%s: PairingConsumed after TTL: %v", at, err)
	}
	if !consumed {
		t.Fatalf("%s: TTL did not consume the code", at)
	}
	err = c.AcceptPairingOffer(ctx, PairingOffer{Code: code2.Code, DeviceID: "late"})
	requireClass(t, err, ClassPairExpired)
}

// AT-FR-39-c (concurrency): single-use is a property of the code, not of the
// calling pattern. TestAT_FR_39_c_SingleUseMomentExact proves a *sequential*
// second CompletePairing is refused with ClassPairConsumed. That refusal is
// checked in prepare and the flag is set in commit, so two callers that both
// prepare before either commits used to both succeed — the code was single-use
// per call sequence rather than single-use.
//
// Nothing hostile gets in that way: the session binds one offer, so both
// callers publish the same device and the registries converge. But a caller
// that is told "consumed" in one interleaving and "here is your device" in
// another cannot be reasoned about, and the next reader of PairingConsumed
// would assume the stronger guarantee the sequential row appears to give.
func TestAT_FR_39_c_SingleUseIsExactUnderConcurrency(t *testing.T) {
	const at = "AT-FR-39-c-concurrent"
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
		DevicePub:  s7MustP256Pub(t),
		SignPub:    s7MustSignPub(t),
		OfferNonce: bytes.Repeat([]byte{0x41}, 32),
	}); err != nil {
		t.Fatalf("%s: offer: %v", at, err)
	}

	const callers = 2
	var wg sync.WaitGroup
	errs := make([]error, callers)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = c.CompletePairing(ctx, code.Code, true, true)
		}(i)
	}
	close(start)
	wg.Wait()

	ok, consumedRefusals := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case classOfErr(err) == ClassPairConsumed:
			consumedRefusals++
		default:
			t.Fatalf("%s: unexpected error from a concurrent complete: %v", at, err)
		}
	}
	if ok != 1 {
		t.Fatalf("%s: %d of %d concurrent completes succeeded, want exactly 1 (errs=%v); the code is not single-use under concurrency",
			at, ok, callers, errs)
	}
	if consumedRefusals != callers-1 {
		t.Fatalf("%s: %d concurrent completes refused as pair-consumed, want %d (errs=%v)",
			at, consumedRefusals, callers-1, errs)
	}

	consumed, err := c.PairingConsumed(code.Code)
	if err != nil {
		t.Fatalf("%s: PairingConsumed: %v", at, err)
	}
	if !consumed {
		t.Fatalf("%s: code not consumed after a successful concurrent complete", at)
	}

	// The winning complete must still be whole: both registries, one device.
	paired, err := c.PairedDevices()
	if err != nil {
		t.Fatalf("%s: PairedDevices: %v", at, err)
	}
	live, err := c.Devices()
	if err != nil {
		t.Fatalf("%s: Devices: %v", at, err)
	}
	if len(paired) != 1 || len(live) != 1 {
		t.Fatalf("%s: registries after concurrent complete: paired=%+v live=%+v, want one device in each", at, paired, live)
	}
	if paired[0].RID != code.RID || live[0].RID != code.RID {
		t.Fatalf("%s: registries disagree on RID: paired=%s live=%s want %s", at, paired[0].RID, live[0].RID, code.RID)
	}
}
