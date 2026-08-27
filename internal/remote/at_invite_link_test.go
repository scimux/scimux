package remote

import (
	"encoding/hex"
	"net/url"
	"strings"
	"testing"
)

// The invite link (rendezvous-v1 §11.1).
//
// V1 has no typed pairing path: §11.2 records that decision and its exit
// cost. A device therefore learns the code, the rendezvous ID and the
// computer's static X from a link or a QR of the same link — which is why
// this builder is the only way a pairing starts, and why what it emits is
// protocol rather than presentation.

func inviteFragment(t *testing.T, link string) url.Values {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("parse %q: %v", link, err)
	}
	q, err := url.ParseQuery(u.Fragment)
	if err != nil {
		t.Fatalf("fragment %q is not a query: %v", u.Fragment, err)
	}
	return q
}

func mintedInvite(t *testing.T) (*Client, PairingCode, string) {
	t.Helper()
	c, _, ctx, cancel := s7Enrolled(t)
	t.Cleanup(cancel)
	pc, err := c.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}
	link, err := c.PairingLink(pc)
	if err != nil {
		t.Fatalf("PairingLink: %v", err)
	}
	return c, pc, link
}

func TestInviteLinkCarriesEveryValueTheDeviceNeeds(t *testing.T) {
	c, pc, link := mintedInvite(t)

	q := inviteFragment(t, link)
	if got := q.Get("v"); got != "1" {
		t.Errorf("v = %q, want 1", got)
	}
	if got := q.Get("o"); got != c.origin() {
		t.Errorf("o = %q, want %q", got, c.origin())
	}
	if got := q.Get("c"); got != pc.Code {
		t.Errorf("c = %q, want the minted code %q", got, pc.Code)
	}
	if got := q.Get("r"); got != pc.RID {
		t.Errorf("r = %q, want the minted rid %q", got, pc.RID)
	}

	x, err := c.PairingECDHPublic()
	if err != nil {
		t.Fatalf("PairingECDHPublic: %v", err)
	}
	if got := q.Get("x"); got != hex.EncodeToString(x) {
		t.Errorf("x = %q, want the computer's static X", got)
	}
}

// §11's SAS is ECDH(X, Y) and the transcript binds the rid. A link that
// omitted either would produce a device that cannot derive the digits,
// which is the gap §11.2 exists to record.
func TestInviteLinkIsUselessWithoutXOrRID(t *testing.T) {
	_, _, link := mintedInvite(t)
	q := inviteFragment(t, link)
	for _, k := range []string{"c", "r", "x"} {
		if q.Get(k) == "" {
			t.Errorf("invite omits %q, so the device cannot compute the SAS", k)
		}
	}
}

// §8: the rendezvous ID must never reach a query string, a Referer, or
// rv's access log. A fragment is not sent to the server; a query is.
func TestInviteLinkKeepsTheSecretsOutOfThePathAndQuery(t *testing.T) {
	_, pc, link := mintedInvite(t)
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	if u.RawQuery != "" {
		t.Errorf("invite has a query string %q; §8 forbids the rid on that surface", u.RawQuery)
	}
	before := strings.SplitN(link, "#", 2)[0]
	for _, secret := range []string{pc.Code, pc.RID} {
		if strings.Contains(before, secret) {
			t.Errorf("invite leaks %q outside the fragment: %q", secret, before)
		}
	}
}

// §13: the page is served at /p and nowhere else, and the invite is a
// link to that page on this installation's own origin.
func TestInviteLinkPointsAtThePairingPageOnTheConfiguredOrigin(t *testing.T) {
	c, _, link := mintedInvite(t)
	want := c.origin() + "/p#"
	if !strings.HasPrefix(link, want) {
		t.Errorf("invite = %q, want it to start %q", link, want)
	}
}

// A link is single-use in the same sense the code is: two mints are two
// different invites. Reusing one would present a consumed code.
func TestTwoMintsProduceTwoInvites(t *testing.T) {
	c, _, ctx, cancel := s7Enrolled(t)
	defer cancel()
	seen := map[string]bool{}
	for range 3 {
		pc, err := c.MintPairingCode(ctx)
		if err != nil {
			t.Fatal(err)
		}
		link, err := c.PairingLink(pc)
		if err != nil {
			t.Fatal(err)
		}
		if seen[link] {
			t.Fatalf("invite repeated: %q", link)
		}
		seen[link] = true
	}
}

// The builder refuses to emit an invite it knows is not one, rather than
// producing a link that fails at the device with nothing to say.
func TestInviteLinkRefusesAnIncompletePairingCode(t *testing.T) {
	c, _, ctx, cancel := s7Enrolled(t)
	defer cancel()
	good, err := c.MintPairingCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]PairingCode{
		"no code":     {RID: good.RID},
		"no rid":      {Code: good.Code},
		"short rid":   {Code: good.Code, RID: good.RID[:32]},
		"upper rid":   {Code: good.Code, RID: strings.ToUpper(good.RID)},
		"long code":   {Code: good.Code + "7", RID: good.RID},
		"short code":  {Code: good.Code[:7], RID: good.RID},
		"non-crock":   {Code: "UUUUUUUU", RID: good.RID},
		"empty pair":  {},
		"non-hex rid": {Code: good.Code, RID: strings.Repeat("z", RendezvousIDHexLen)},
	}
	for name, pc := range cases {
		if link, err := c.PairingLink(pc); err == nil {
			t.Errorf("%s: PairingLink returned %q, want an error", name, link)
		}
	}
}

// §11.1 allows a grouped or ungrouped code. The builder emits the
// normalised form: a QR is not read by a human, and every character
// dropped is capacity the payload does not need.
func TestInviteLinkEmitsTheNormalisedCode(t *testing.T) {
	_, pc, link := mintedInvite(t)
	q := inviteFragment(t, link)
	if strings.Contains(q.Get("c"), "-") {
		t.Errorf("c = %q, want the normalised code", q.Get("c"))
	}
	if len(q.Get("c")) != PairingCodeCrockfordLen {
		t.Errorf("c = %q, want %d characters", q.Get("c"), PairingCodeCrockfordLen)
	}
	if q.Get("c") != pc.Code {
		t.Errorf("c = %q, want %q", q.Get("c"), pc.Code)
	}
}

// The x parameter is an uncompressed P-256 point, lowercase, as §11.1
// specifies and as the /p page's parser requires.
func TestInviteLinkXIsAnUncompressedLowercasePoint(t *testing.T) {
	_, _, link := mintedInvite(t)
	x := inviteFragment(t, link).Get("x")
	if len(x) != 130 || !strings.HasPrefix(x, "04") {
		t.Errorf("x = %q, want 04 followed by 128 hex characters", x)
	}
	if x != strings.ToLower(x) {
		t.Errorf("x = %q, want lowercase", x)
	}
	if _, err := hex.DecodeString(x); err != nil {
		t.Errorf("x is not hex: %v", err)
	}
}

// The point-shape guard cannot be reached through a real client, whose X
// is always well-formed. It is still worth having: a truncated or
// compressed point would otherwise leave the computer and fail at the
// device on the six digits, which is the worst place to learn about it.
func TestInviteLinkRefusesAMalformedComputerKey(t *testing.T) {
	good, err := hex.DecodeString(strings.Repeat("ab", p256UncompressedLen))
	if err != nil {
		t.Fatal(err)
	}
	good[0] = 0x04

	cases := map[string][]byte{
		"truncated":  good[:33],
		"compressed": append([]byte{0x02}, good[1:33]...),
		"wrong tag":  append([]byte{0x03}, good[1:]...),
		"overlong":   append(append([]byte(nil), good...), 0x00),
	}
	for name, x := range cases {
		c, _, ctx, cancel := s7Enrolled(t)
		pc, err := c.MintPairingCode(ctx)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		r := pairingOf(c)
		r.mu.Lock()
		r.xPub = append([]byte(nil), x...)
		r.mu.Unlock()

		if link, err := c.PairingLink(pc); err == nil {
			t.Errorf("%s: PairingLink returned %q, want an error", name, link)
		}
		cancel()
	}
}
