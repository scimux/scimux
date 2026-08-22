package remote

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
	"unicode"
)

// AT-FR-11-a: A pairing code expires after 60 seconds and is then rejected.
func TestAT_FR_11_a_PairingCodeExpiresAfter60s(t *testing.T) {
	const at = "AT-FR-11-a"
	c, clk, ctx, cancel := s7Enrolled(t)
	defer cancel()

	vec := s7Construction(t, "pairing-code-8")
	entropy := s7Hex(t, vec["entropy_hex"].(string))
	wantCode, _ := vec["code"].(string)
	if wantCode == "" {
		t.Fatalf("%s: pairing-code-8 has no code", at)
	}
	c.cfg.Rand = io.MultiReader(bytes.NewReader(entropy), bytes.NewReader(bytes.Repeat([]byte{0xab}, 32)))

	code, err := c.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("%s: MintPairingCode: %v", at, err)
	}
	if PairingTTL != 60*time.Second {
		t.Fatalf("%s: PairingTTL = %s, want 60s", at, PairingTTL)
	}
	norm := s7RequirePairCode(t, at, code.Code)
	if norm != wantCode {
		t.Fatalf("%s: mint-from-entropy = %q, want S1 pairing-code-8 %q", at, norm, wantCode)
	}
	if strings.ContainsRune(norm, 'U') {
		t.Fatalf("%s: code %q contains U; Crockford alphabet excludes U", at, norm)
	}
	if code.RID == "" {
		t.Fatalf("%s: minted code has no temporary rendezvous ID", at)
	}
	ridBits(t, code.RID)

	clk.Advance(PairingTTL - time.Second)
	err = c.AcceptPairingOffer(ctx, PairingOffer{Code: code.Code, DeviceID: "phone"})
	if err != nil && classOf(err) == ClassPairExpired {
		t.Fatalf("%s: code rejected as expired 1s before TTL", at)
	}
	if err != nil {
		t.Fatalf("%s: AcceptPairingOffer 1s before TTL: %v", at, err)
	}

	clk.Advance(time.Second)
	err = c.AcceptPairingOffer(ctx, PairingOffer{Code: code.Code, DeviceID: "phone"})
	requireClass(t, err, ClassPairExpired)
}

// AT-FR-11-b: A pairing code accepted once cannot be used a second time.
func TestAT_FR_11_b_PairingCodeSingleUse(t *testing.T) {
	const at = "AT-FR-11-b"
	c, _, ctx, cancel := s7Enrolled(t)
	defer cancel()

	code, err := c.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("%s: MintPairingCode: %v", at, err)
	}
	if err := c.AcceptPairingOffer(ctx, PairingOffer{Code: code.Code, DeviceID: "phone"}); err != nil {
		t.Fatalf("%s: AcceptPairingOffer: %v", at, err)
	}
	_, err = c.CompletePairing(ctx, code.Code, true, true)
	if err != nil {
		t.Fatalf("%s: first complete: %v", at, err)
	}
	consumed, err := c.PairingConsumed(code.Code)
	if err != nil {
		t.Fatalf("%s: PairingConsumed: %v", at, err)
	}
	if !consumed {
		t.Fatalf("%s: code not consumed after a successful pairing", at)
	}
	_, err = c.CompletePairing(ctx, code.Code, true, true)
	requireClass(t, err, ClassPairConsumed)
	err = c.AcceptPairingOffer(ctx, PairingOffer{Code: code.Code, DeviceID: "second"})
	requireClass(t, err, ClassPairConsumed)
}

// AT-FR-11-c: The temporary pairing rendezvous ID is unregistered when
// the code expires.
func TestAT_FR_11_c_TempRIDUnregisteredOnExpiry(t *testing.T) {
	const at = "AT-FR-11-c"
	c, clk, ctx, cancel := s7Enrolled(t)
	defer cancel()

	code, err := c.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("%s: MintPairingCode: %v", at, err)
	}
	if code.RID == "" {
		t.Fatalf("%s: no temporary RID", at)
	}
	ok, err := c.PairingWaiterRegistered(code.RID)
	if err != nil {
		t.Fatalf("%s: waiter before expiry: %v", at, err)
	}
	if !ok {
		t.Fatalf("%s: temporary RID was never registered", at)
	}

	clk.Advance(PairingTTL)
	ok, err = c.PairingWaiterRegistered(code.RID)
	if err != nil {
		t.Fatalf("%s: waiter after expiry: %v", at, err)
	}
	if ok {
		t.Fatalf("%s: temporary RID still registered after TTL", at)
	}
}

// s7RequirePairCode normalises a minted pairing code and fails the test if
// any rune is outside the Crockford alphabet (which excludes U).
func s7RequirePairCode(t *testing.T, at, s string) string {
	t.Helper()
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '-', ' ':
			continue
		case 'o', 'O':
			r = '0'
		case 'i', 'I', 'l', 'L':
			r = '1'
		default:
			r = unicode.ToUpper(r)
		}
		if strings.IndexRune(crockfordAlphabet, r) < 0 {
			t.Fatalf("%s: code %q has %q, which is outside the Crockford alphabet (U is excluded)", at, s, r)
		}
		b.WriteRune(r)
	}
	norm := b.String()
	if len(norm) != PairingCodeCrockfordLen {
		t.Fatalf("%s: code %q normalises to len %d, want 8 Crockford chars", at, s, len(norm))
	}
	if strings.ContainsRune(crockfordAlphabet, 'U') {
		t.Fatalf("%s: Crockford alphabet fixture includes U", at)
	}
	return norm
}
