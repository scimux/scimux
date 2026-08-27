package remote

import (
	"encoding/hex"
	"os"
	"testing"
)

// F1, second half — revocation is a state, not a dead end.
//
// `StateRevoked` returned ClassRevoked unconditionally, before an invite
// was read, and the only branch that could accept a new one was gated on
// Config.Rotate — a field no flag set and no operator could reach. So the
// operator's own recovery ("I revoked that laptop; here is a new invite")
// had no path through the client at all: every start said "this
// installation has been revoked" and stopped.
//
// A new invite is the operator saying otherwise, and it is the only thing
// that can lift it. The old identity is dead at the rendezvous, so this
// is an ordinary first-time enrollment that happens to have wreckage on
// disk — new keypair, new handle, and the same probe-before-you-post rule
// as every other place a credential is spent.
//
//	F1-i  TestAT_F1_i_ARevokedInstallationSaysSoOnceAndContactsNobody
//	F1-j  TestAT_F1_j_ANewInviteLiftsRevocation
//	F1-k  TestAT_F1_k_ARejectedInviteLeavesTheInstallationRevoked
//	F1-l  TestAT_F1_l_AnUnreachableRendezvousDoesNotSpendTheLiftingInvite

// seedRevoked writes the installation an operator's revocation leaves
// behind: a complete identity, a bound handle, and a status saying the
// rendezvous will not talk to it again.
func seedRevoked(t *testing.T, c *Client) PersistedState {
	t.Helper()
	pub, priv := newEd25519(t)
	st := PersistedState{
		V:          1,
		Status:     StateRevoked,
		Handle:     "ih_041061050R3GG28A",
		PublicKey:  hex.EncodeToString(pub),
		PrivateKey: hex.EncodeToString(priv),
		Origin:     DefaultOrigin,
	}
	writeState(t, c, st)
	return st
}

func TestAT_F1_i_ARevokedInstallationSaysSoOnceAndContactsNobody(t *testing.T) {
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)

	c := NewClient(cfg)
	seeded := seedRevoked(t, c)

	ctx, cancel := ctxTO(t)
	defer cancel()

	// No invite: nothing has changed the operator's decision, so the
	// answer is the same one, and it is reached without a single request.
	// A revoked installation that phones home would be doing it with a
	// key the rendezvous has already refused.
	requireClass(t, c.Start(ctx), ClassRevoked)
	if n := fake.RequestCount(); n != 0 {
		t.Fatalf("a bare revoked start made %d requests: %v", n, fake.Paths())
	}
	if c.HostedStatus() != "revoked" {
		t.Fatalf("hosted = %q, want revoked", c.HostedStatus())
	}
	after := mustState(t, c.StatePath())
	if after.Status != StateRevoked || after.PublicKey != seeded.PublicKey {
		t.Fatalf("a bare revoked start rewrote the installation: %+v", after)
	}
}

func TestAT_F1_j_ANewInviteLiftsRevocation(t *testing.T) {
	fake := newFakeRV(t)
	invite := otherInvite()
	fake.Issue(invite)

	cfg := clientCfg(t, fake)
	c := NewClient(cfg)
	seeded := seedRevoked(t, c)

	path := writeInviteFile(t, t.TempDir(), invite, 0o600)
	cfg.InviteFile = path
	c2 := NewClient(cfg)

	ctx, cancel := ctxTO(t)
	defer cancel()

	if err := c2.Start(ctx); err != nil {
		t.Fatalf("a new invite did not lift the revocation: %v", err)
	}
	t.Cleanup(func() { _ = c2.Close() })

	st := mustState(t, c2.StatePath())
	if st.Status != StateEnrolled || st.Handle == "" {
		t.Fatalf("state after the lifting invite is %+v, want enrolled with a handle", st)
	}
	// A fresh identity, not the revoked one. Re-presenting a key the
	// rendezvous has struck off would be enrolling into an argument.
	if st.PublicKey == seeded.PublicKey || st.PrivateKey == seeded.PrivateKey {
		t.Fatal("the lifting enrollment reused the revoked identity")
	}
	if st.Handle == seeded.Handle {
		t.Fatal("the lifting enrollment kept the revoked handle")
	}
	if c2.HostedStatus() != "enrolled" {
		t.Fatalf("hosted = %q, want enrolled", c2.HostedStatus())
	}
	// FR-02 still applies: the credential really was redeemed this time.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the redeemed lifting invite survived: %v", err)
	}
}

func TestAT_F1_k_ARejectedInviteLeavesTheInstallationRevoked(t *testing.T) {
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	c := NewClient(cfg)
	seeded := seedRevoked(t, c)

	// An invite the rendezvous never issued. The attempt is refused, and
	// the installation must land back where it started rather than in the
	// half-built identity enrollNew writes before it posts.
	path := writeInviteFile(t, t.TempDir(), unissuedInvite, 0o600)
	cfg.InviteFile = path
	c2 := NewClient(cfg)

	ctx, cancel := ctxTO(t)
	defer cancel()

	requireClass(t, c2.Start(ctx), ClassEnrollRejected)

	st := mustState(t, c2.StatePath())
	if st.Status != StateRevoked {
		t.Fatalf("status = %q after a rejected lift, want %s", st.Status, StateRevoked)
	}
	if st.PublicKey != seeded.PublicKey || st.Handle != seeded.Handle {
		t.Fatalf("a rejected lift left a different installation behind: %+v", st)
	}
	// A §7 rejection proves nothing was redeemed, so the user keeps the
	// code they were given.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a rejected lift destroyed the invite: %v", err)
	}
}

func TestAT_F1_l_AnUnreachableRendezvousDoesNotSpendTheLiftingInvite(t *testing.T) {
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	c := NewClient(cfg)
	seedRevoked(t, c)

	cfg.HTTPClient = unreachableClient()
	cfg.RendezvousURL = "http://127.0.0.1:1"
	path := writeInviteFile(t, t.TempDir(), otherInvite(), 0o600)
	cfg.InviteFile = path
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := ctxTO(t)
	defer cancel()

	c2 := NewClient(cfg)
	requireClass(t, c2.Start(ctx), ClassUnreachable)

	inviteUntouched(t, path, before, 0o600)
	if st := mustState(t, c2.StatePath()); st.Status != StateRevoked {
		t.Fatalf("status = %q after an unreachable lift, want %s", st.Status, StateRevoked)
	}
}
