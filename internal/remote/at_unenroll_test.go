package remote

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

// F1, third part — the way out.
//
// Enrolling was one-way. Every other grant in this system can be handed
// back — a paired device is revoked from the burger, a node is deleted —
// but the computer's own enrollment had no such action, so a user trying
// scimux remote for the first time could only stop using it, never undo
// it. That is a bad trade to offer someone on the day you are asking them
// to try it.
//
// rendezvous-v1 §4.4 is the remote half: a route-scoped signature over a
// fresh challenge releases the installation. The local half is the one
// that must not be conditional on it — a computer that cannot reach the
// rendezvous is exactly the computer whose owner most wants to stop it
// trying, so the unlink completes either way and reports honestly which
// halves happened.
//
//	U1  TestAT_F1_m_UnlinkReleasesTheInstallationAndForgetsIt
//	U2  TestAT_F1_n_UnlinkSignsTheUnenrollRouteNotVerify
//	U3  TestAT_F1_o_AnUnreachableRendezvousStillUnlinksLocally
//	U4  TestAT_F1_p_ARevokedInstallationStillUnlinksLocally
//	U5  TestAT_F1_q_UnlinkingWhenNothingIsEnrolledContactsNobody
//	U6  TestAT_F1_r_AnUnlinkedComputerCanEnrollAgainWithANewInvite

func TestAT_F1_m_UnlinkReleasesTheInstallationAndForgetsIt(t *testing.T) {
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	cfg.InviteFile = writeInviteFile(t, t.TempDir(), vectorInviteGrouped, 0o600)

	ctx, cancel := ctxTO(t)
	defer cancel()

	c := NewClient(cfg)
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	handle := mustState(t, c.StatePath()).Handle
	if handle == "" {
		t.Fatal("setup did not bind a handle")
	}

	released, err := c.Unenroll(ctx)
	if err != nil {
		t.Fatalf("unenroll: %v", err)
	}
	if !released {
		t.Fatal("a reachable rendezvous did not confirm the release")
	}
	if got := fake.Released(); len(got) != 1 || got[0] != handle {
		t.Fatalf("rendezvous released %v, want [%s]", got, handle)
	}

	// Forgotten locally, not merely marked. A status record would leave the
	// private key on disk for an installation the rendezvous has already
	// let go of, which is a secret kept for nothing.
	if _, _, ok := readStateFile(t, c.StatePath()); ok {
		t.Fatal("unlink left the identity on disk")
	}
	if _, err := os.Stat(c.tempPath()); err == nil {
		t.Fatal("unlink left a temp state file behind")
	}
	st, err := c.State()
	if err != nil {
		t.Fatalf("state after unlink: %v", err)
	}
	if st != StateAbsent {
		t.Fatalf("state after unlink = %q, want %s", st, StateAbsent)
	}
	if h := c.HostedStatus(); h == "enrolled" {
		t.Fatalf("hosted = %q after unlink", h)
	}
}

func TestAT_F1_n_UnlinkSignsTheUnenrollRouteNotVerify(t *testing.T) {
	// The route segment of the signed message is the whole security of
	// §4.4: without it, any signature this installation ever produced for
	// /v1/verify would delete it. The fake verifies the unenroll message,
	// so reaching a 204 already proves the right route went in — this
	// test proves the *other* half, that the same bytes are not a valid
	// verify signature, which is what would be true if the route were
	// dropped from the message entirely.
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	cfg.InviteFile = writeInviteFile(t, t.TempDir(), vectorInviteGrouped, 0o600)

	ctx, cancel := ctxTO(t)
	defer cancel()

	c := NewClient(cfg)
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	pubHex := mustState(t, c.StatePath()).PublicKey
	pub, err := hex.DecodeString(pubHex)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.Unenroll(ctx); err != nil {
		t.Fatalf("unenroll: %v", err)
	}

	var body []byte
	for _, r := range fake.Requests() {
		if r.Path == "/v1/unenroll" {
			body = r.Body
		}
	}
	if body == nil {
		t.Fatal("no /v1/unenroll request was made")
	}
	var req struct {
		Handle    string `json:"handle"`
		Challenge string `json:"challenge"`
		Sig       string `json:"sig"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unenroll body is not JSON: %v", err)
	}
	chal, cerr := hex.DecodeString(req.Challenge)
	sig, serr := hex.DecodeString(req.Sig)
	if cerr != nil || serr != nil {
		t.Fatalf("unenroll body is not hex: %v %v", cerr, serr)
	}
	if ed25519.Verify(pub, buildAuthMessage(DefaultOrigin, ProtocolVersion, "/v1/verify", req.Handle, chal), sig) {
		t.Fatal("the unenroll signature also authenticates /v1/verify: the route is not in the signed message")
	}
}

func TestAT_F1_o_AnUnreachableRendezvousStillUnlinksLocally(t *testing.T) {
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	cfg.InviteFile = writeInviteFile(t, t.TempDir(), vectorInviteGrouped, 0o600)

	ctx, cancel := ctxTO(t)
	defer cancel()

	c := NewClient(cfg)
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	// The computer that cannot reach the rendezvous is precisely the one
	// whose owner wants it to stop trying. Blocking the unlink on the
	// release would make the failure mode the reason the escape hatch
	// does not work.
	c.cfg.HTTPClient = unreachableClient()
	c.cfg.RendezvousURL = "http://127.0.0.1:1"

	released, err := c.Unenroll(ctx)
	if err != nil {
		t.Fatalf("unlink failed because the rendezvous was unreachable: %v", err)
	}
	if released {
		t.Fatal("claimed the rendezvous released an installation it never heard about")
	}
	if _, _, ok := readStateFile(t, c.StatePath()); ok {
		t.Fatal("an unreachable rendezvous left the identity on disk")
	}
}

func TestAT_F1_p_ARevokedInstallationStillUnlinksLocally(t *testing.T) {
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	c := NewClient(cfg)
	seedRevoked(t, c)

	ctx, cancel := ctxTO(t)
	defer cancel()

	// The rendezvous answers the §7 constant rejection to everything this
	// handle asks. There is nothing left to release, and the local
	// wreckage is the only thing the unlink can still clear — which is
	// exactly what the user wants it to clear.
	released, err := c.Unenroll(ctx)
	if err != nil {
		t.Fatalf("unlink of a revoked installation: %v", err)
	}
	if released {
		t.Fatal("claimed a release the rendezvous rejected")
	}
	if _, _, ok := readStateFile(t, c.StatePath()); ok {
		t.Fatal("unlink left the revoked identity on disk")
	}
}

func TestAT_F1_q_UnlinkingWhenNothingIsEnrolledContactsNobody(t *testing.T) {
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)

	ctx, cancel := ctxTO(t)
	defer cancel()

	c := NewClient(cfg)
	released, err := c.Unenroll(ctx)
	if err != nil {
		t.Fatalf("unlink with nothing enrolled: %v", err)
	}
	if released {
		t.Fatal("claimed a release with no handle to release")
	}
	// No handle means no signature is possible, so a request could only be
	// an unauthenticated one — noise at best, and a probe of the
	// rendezvous at worst.
	if n := fake.RequestCount(); n != 0 {
		t.Fatalf("unlink with nothing enrolled made %d requests: %v", n, fake.Paths())
	}
}

func TestAT_F1_r_AnUnlinkedComputerCanEnrollAgainWithANewInvite(t *testing.T) {
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	cfg.InviteFile = writeInviteFile(t, t.TempDir(), vectorInviteGrouped, 0o600)

	ctx, cancel := ctxTO(t)
	defer cancel()

	c := NewClient(cfg)
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	first := mustState(t, c.StatePath())
	if _, err := c.Unenroll(ctx); err != nil {
		t.Fatalf("unenroll: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// A clean slate, not a special case: the same data directory enrolls
	// from absent, exactly as a fresh install would. §4.4 releases the
	// installation and never the code, so it takes a new invite.
	next := otherInvite()
	fake.Issue(next)
	again := cfg
	again.InviteFile = writeInviteFile(t, t.TempDir(), next, 0o600)

	c2 := NewClient(again)
	if err := c2.Start(ctx); err != nil {
		t.Fatalf("re-enrollment after unlink: %v", err)
	}
	t.Cleanup(func() { _ = c2.Close() })

	st := mustState(t, c2.StatePath())
	if st.Status != StateEnrolled || st.Handle == "" {
		t.Fatalf("re-enrollment left %+v", st)
	}
	if st.PublicKey == first.PublicKey {
		t.Fatal("re-enrollment reused the released identity")
	}
}
