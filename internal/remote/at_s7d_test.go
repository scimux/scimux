package remote

// Packet S7d — join the pairing offer to the rendezvous.
//
// AcceptPairingOffer has no product caller. Minting a code sets waiterOn and
// returns a temporary RID, but liveRIDs only fans out over c.devices, so a
// sealed pair/offer has nowhere to land. These rows drive the rendezvous the
// way a device would: they do not call AcceptPairingOffer.

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func s7dEnrolledClient(t *testing.T, rv *sessionRV) *Client {
	t.Helper()
	cfg := r3ClientCfg(t, rv.URL(), rv.HTTPClient())
	cfg.Backoff = BackoffConfig{
		Initial:    20 * time.Millisecond,
		Max:        200 * time.Millisecond,
		Factor:     2,
		SuccessFor: 5 * time.Second,
	}
	pub, priv := newEd25519(t)
	writeState(t, NewClient(cfg), PersistedState{
		V:          1,
		Status:     StateEnrolled,
		Handle:     "ih_041061050R3GG28A",
		PublicKey:  hex.EncodeToString(pub),
		PrivateKey: hex.EncodeToString(priv),
		Origin:     DefaultOrigin,
	})
	ctx, cancel := ctxTO(t)
	t.Cleanup(cancel)
	c := NewClient(cfg)
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func s7dAwaitPairWaits(t *testing.T, rv *sessionRV, rid string, want int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if rv.PairWaitCount(rid) >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("pair/wait count for %s = %d, want >= %d (total waits %d)", rid, rv.PairWaitCount(rid), want, rv.WaitCount())
}

func s7dSealJSON(t *testing.T, inner any, recipientPub []byte, origin, rid string) []byte {
	t.Helper()
	plain, err := json.Marshal(inner)
	if err != nil {
		t.Fatal(err)
	}
	curve := ecdh.P256()
	pub, err := curve.NewPublicKey(recipientPub)
	if err != nil {
		t.Fatal(err)
	}
	eph, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := eph.ECDH(pub)
	if err != nil {
		t.Fatal(err)
	}
	ephPub := eph.PublicKey().Bytes()
	gcm, err := envelopeKey(shared, ephPub, pub.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, gcmNonceLen)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	out := append(append([]byte{}, ephPub...), nonce...)
	return gcm.Seal(out, nonce, plain, envelopeAD(origin, rid))
}

func s7dOpenJSON(t *testing.T, sealed, recipientPriv []byte, origin, rid string) map[string]any {
	t.Helper()
	curve := ecdh.P256()
	priv, err := curve.NewPrivateKey(recipientPriv)
	if err != nil {
		t.Fatalf("open: private key: %v", err)
	}
	if len(sealed) < p256UncompressedLen+gcmNonceLen+gcmTagLen {
		t.Fatalf("open: sealed blob too short (%d)", len(sealed))
	}
	ephPub := sealed[:p256UncompressedLen]
	eph, err := curve.NewPublicKey(ephPub)
	if err != nil {
		t.Fatalf("open: ephemeral: %v", err)
	}
	shared, err := priv.ECDH(eph)
	if err != nil {
		t.Fatalf("open: ecdh: %v", err)
	}
	gcm, err := envelopeKey(shared, ephPub, priv.PublicKey().Bytes())
	if err != nil {
		t.Fatal(err)
	}
	nonce := sealed[p256UncompressedLen : p256UncompressedLen+gcmNonceLen]
	plain, err := gcm.Open(nil, nonce, sealed[p256UncompressedLen+gcmNonceLen:], envelopeAD(origin, rid))
	if err != nil {
		t.Fatalf("device could not open the laptop's pairing reply: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(plain, &out); err != nil {
		t.Fatalf("pairing reply is not JSON: %v (%q)", err, plain)
	}
	return out
}

func s7dPairOfferInner(t *testing.T, yPub, signPub, offerN []byte, id, label string) map[string]any {
	t.Helper()
	return map[string]any{
		"v":               ProtocolVersion,
		"type":            "pair-offer",
		"device_pub":      hex.EncodeToString(yPub),
		"device_sign_pub": hex.EncodeToString(signPub),
		"offer_nonce":     hex.EncodeToString(offerN),
		"device_id":       id,
		"label":           label,
	}
}

// TestAT_S7d_MintRegistersPairWait is the first half of the join: minting a
// code must cause /v1/pair/wait for the temporary RID, for PairingTTL only.
func TestAT_S7d_MintRegistersPairWait(t *testing.T) {
	const at = "AT-S7d-mint-wait"
	rv := newSessionRV(t)
	c := s7dEnrolledClient(t, rv)
	ctx, cancel := ctxTO(t)
	defer cancel()

	code, err := c.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("%s: MintPairingCode: %v", at, err)
	}
	s7dAwaitPairWaits(t, rv, code.RID, 2, 3*time.Second)
	if rv.SessionWaitCount(code.RID) != 0 {
		t.Fatalf("%s: pairing RID was waited on /v1/wait; the pairing waiter is /v1/pair/wait", at)
	}
}

// TestAT_S7d_SealedPairOfferGetsSealedReply is the join. A device posts a
// sealed pair/offer; the laptop must open it via the wait loop, call into
// AcceptPairingOffer on the product path, and seal a pair-reply to Y.
func TestAT_S7d_SealedPairOfferGetsSealedReply(t *testing.T) {
	const at = "AT-S7d-pair-offer-reply"
	rv := newSessionRV(t)
	c := s7dEnrolledClient(t, rv)
	ctx, cancel := ctxTO(t)
	defer cancel()

	code, err := c.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("%s: mint: %v", at, err)
	}
	s7dAwaitPairWaits(t, rv, code.RID, 1, 3*time.Second)

	x, err := c.PairingECDHPublic()
	if err != nil {
		t.Fatalf("%s: laptop X: %v", at, err)
	}
	devPriv, yPub := deviceKeypair(t)
	signPub := s7MustSignPub(t)
	offerN := bytes.Repeat([]byte{0x41}, 32)
	sealed := s7dSealJSON(t, s7dPairOfferInner(t, yPub, signPub, offerN, "phone", "Phone"), x, DefaultOrigin, code.RID)
	rv.QueueEnvelope(code.RID, sealed)

	reply := awaitReply(t, rv, 8*time.Second)
	inner := s7dOpenJSON(t, reply, devPriv.Bytes(), DefaultOrigin, code.RID)
	gotType, _ := inner["type"].(string)
	if gotType != "pair-reply" {
		t.Fatalf("%s: reply type = %q, want pair-reply", at, gotType)
	}
	if inner["install_pub"] == nil || inner["reply_nonce"] == nil {
		t.Fatalf("%s: pair-reply missing install_pub or reply_nonce: %+v", at, inner)
	}

	st, err := c.PairingSession(code.Code)
	if err != nil {
		t.Fatalf("%s: PairingSession: %v", at, err)
	}
	if st.SAS == "" {
		t.Fatalf("%s: wait-loop AcceptPairingOffer did not derive SAS", at)
	}
	if hex.EncodeToString(st.ReplyNonce) != inner["reply_nonce"] {
		t.Fatalf("%s: pair-reply nonce %v != session nonce %s", at, inner["reply_nonce"], hex.EncodeToString(st.ReplyNonce))
	}
}

// TestAT_S7d_UnopenablePairOfferDoesNotAnswerOrConsume: junk on the pairing
// wait must not be answered, must not consume the code, and must not wedge.
func TestAT_S7d_UnopenablePairOfferDoesNotAnswerOrConsume(t *testing.T) {
	const at = "AT-S7d-pair-junk"
	rv := newSessionRV(t)
	c := s7dEnrolledClient(t, rv)
	ctx, cancel := ctxTO(t)
	defer cancel()

	code, err := c.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("%s: mint: %v", at, err)
	}
	s7dAwaitPairWaits(t, rv, code.RID, 1, 3*time.Second)

	junk := make([]byte, 160)
	for i := range junk {
		junk[i] = byte(i + 3)
	}
	rv.QueueEnvelope(code.RID, junk)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if rv.DeliveredCount(code.RID) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if rv.DeliveredCount(code.RID) == 0 {
		t.Fatalf("%s: pairing wait never served the junk envelope", at)
	}
	served := rv.PairWaitCount(code.RID)
	time.Sleep(500 * time.Millisecond)
	if rs := rv.Replies(); len(rs) != 0 {
		t.Fatalf("%s: laptop answered an unopenable pair/offer: %d replies", at, len(rs))
	}
	if rv.PairWaitCount(code.RID) <= served {
		t.Fatalf("%s: pairing wait loop stopped after junk", at)
	}
	consumed, err := c.PairingConsumed(code.Code)
	if err != nil {
		t.Fatalf("%s: PairingConsumed: %v", at, err)
	}
	if consumed {
		t.Fatalf("%s: junk offer consumed the pairing code", at)
	}
}

// TestAT_S7d_CancelUnregistersPairWait: CancelPairing must stop the
// temporary wait loop.
func TestAT_S7d_CancelUnregistersPairWait(t *testing.T) {
	const at = "AT-S7d-cancel-wait"
	rv := newSessionRV(t)
	c := s7dEnrolledClient(t, rv)
	ctx, cancel := ctxTO(t)
	defer cancel()

	code, err := c.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("%s: mint: %v", at, err)
	}
	s7dAwaitPairWaits(t, rv, code.RID, 2, 3*time.Second)
	if err := c.CancelPairing(ctx, code.Code); err != nil {
		t.Fatalf("%s: cancel: %v", at, err)
	}
	n := rv.PairWaitCount(code.RID)
	time.Sleep(400 * time.Millisecond)
	if rv.PairWaitCount(code.RID) > n+1 {
		t.Fatalf("%s: pairing wait continued after cancel: %d → %d", at, n, rv.PairWaitCount(code.RID))
	}
	ok, err := c.PairingWaiterRegistered(code.RID)
	if err != nil {
		t.Fatalf("%s: PairingWaiterRegistered: %v", at, err)
	}
	if ok {
		t.Fatalf("%s: temporary waiter still registered after cancel", at)
	}
}

// TestAT_S7d_CompleteStopsPairWaitAndEnrollsDevice: after both-sides
// confirm, the pairing waiter is gone and the permanent RID is on the
// ordinary /v1/wait loop. Both must not run.
func TestAT_S7d_CompleteStopsPairWaitAndEnrollsDevice(t *testing.T) {
	const at = "AT-S7d-complete-handoff"
	rv := newSessionRV(t)
	c := s7dEnrolledClient(t, rv)
	ctx, cancel := ctxTO(t)
	defer cancel()

	code, err := c.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("%s: mint: %v", at, err)
	}
	s7dAwaitPairWaits(t, rv, code.RID, 1, 3*time.Second)
	x, err := c.PairingECDHPublic()
	if err != nil {
		t.Fatalf("%s: X: %v", at, err)
	}
	_, yPub := deviceKeypair(t)
	sealed := s7dSealJSON(t, s7dPairOfferInner(t, yPub, s7MustSignPub(t), bytes.Repeat([]byte{0x42}, 32), "phone", "Phone"), x, DefaultOrigin, code.RID)
	rv.QueueEnvelope(code.RID, sealed)
	_ = awaitReply(t, rv, 8*time.Second)

	pairN := rv.PairWaitCount(code.RID)
	if _, err := c.CompletePairing(ctx, code.Code, true, true); err != nil {
		t.Fatalf("%s: CompletePairing: %v", at, err)
	}
	list, err := c.Devices()
	if err != nil {
		t.Fatalf("%s: Devices: %v", at, err)
	}
	if len(list) != 1 || list[0].RID != code.RID {
		t.Fatalf("%s: live devices %+v, want the pairing RID %s", at, list, code.RID)
	}
	ok, err := c.PairingWaiterRegistered(code.RID)
	if err != nil {
		t.Fatalf("%s: PairingWaiterRegistered: %v", at, err)
	}
	if ok {
		t.Fatalf("%s: pairing waiter still running after complete", at)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if rv.SessionWaitCount(code.RID) >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if rv.SessionWaitCount(code.RID) < 1 {
		t.Fatalf("%s: permanent RID never appeared on /v1/wait", at)
	}
	time.Sleep(300 * time.Millisecond)
	if rv.PairWaitCount(code.RID) > pairN+1 {
		t.Fatalf("%s: pairing wait kept running alongside the device waiter", at)
	}
}

// TestAT_S7d_CompletePairingPersistFailureIsAtomic: the FR-38 list and the
// live registry are one transaction. A persist failure must not consume the
// code or report succeeded with an empty Devices() list.
func TestAT_S7d_CompletePairingPersistFailureIsAtomic(t *testing.T) {
	const at = "AT-S7d-complete-atomic"
	c, _, ctx, cancel := s7Enrolled(t)
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

	c.cfg.FailWrite = &WriteFault{Step: WriteTempCreate}
	_, err = c.CompletePairing(ctx, code.Code, true, true)
	if err == nil {
		t.Fatalf("%s: CompletePairing succeeded despite persist failure", at)
	}
	c.cfg.FailWrite = nil

	paired, err := c.PairedDevices()
	if err != nil {
		t.Fatalf("%s: PairedDevices: %v", at, err)
	}
	if len(paired) != 0 {
		t.Fatalf("%s: persist failure left FR-38 list populated: %+v", at, paired)
	}
	list, err := c.Devices()
	if err != nil {
		t.Fatalf("%s: Devices: %v", at, err)
	}
	if len(list) != 0 {
		t.Fatalf("%s: persist failure left live registry populated: %+v", at, list)
	}
	st, err := c.PairingState(code.Code)
	if err != nil {
		t.Fatalf("%s: PairingState: %v", at, err)
	}
	if st == PairStateSucceeded {
		t.Fatalf("%s: persist failure marked the session succeeded", at)
	}
	consumed, err := c.PairingConsumed(code.Code)
	if err != nil {
		t.Fatalf("%s: PairingConsumed: %v", at, err)
	}
	if consumed {
		t.Fatalf("%s: persist failure consumed the code so it cannot be retried", at)
	}
	if _, err := c.CompletePairing(ctx, code.Code, true, true); err != nil {
		t.Fatalf("%s: retry CompletePairing: %v", at, err)
	}
	list, err = c.Devices()
	if err != nil {
		t.Fatalf("%s: Devices after retry: %v", at, err)
	}
	if len(list) != 1 {
		t.Fatalf("%s: retry did not enroll a live device: %+v", at, list)
	}
}

// TestAT_S7d_PersistPairingXTakesStateLock: every other identity writer
// goes through withStateLock; persistPairingX must too.
func TestAT_S7d_PersistPairingXTakesStateLock(t *testing.T) {
	const at = "AT-S7d-pair-x-lock"
	c, _, ctx, cancel := s7Enrolled(t)
	defer cancel()

	held := 0
	c.cfg.OnLockHeld = func() { held++ }
	if _, err := c.MintPairingCode(ctx); err != nil {
		t.Fatalf("%s: mint: %v", at, err)
	}
	if held == 0 {
		t.Fatalf("%s: MintPairingCode persisted pairing X without withStateLock", at)
	}
}

// TestAT_S7d_PersistPairingXReportsWriteFailure: a discarded persist error
// leaves X only in memory, so a restart mints a new SAS identity.
func TestAT_S7d_PersistPairingXReportsWriteFailure(t *testing.T) {
	const at = "AT-S7d-pair-x-persist-err"
	c, _, ctx, cancel := s7Enrolled(t)
	defer cancel()

	c.cfg.FailWrite = &WriteFault{Step: WriteTempCreate}
	_, err := c.MintPairingCode(ctx)
	if err == nil {
		t.Fatalf("%s: MintPairingCode swallowed a persist failure", at)
	}
	if !strings.Contains(err.Error(), "persist") {
		t.Fatalf("%s: error %q does not name the persist failure", at, err)
	}
}
