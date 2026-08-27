package remote

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A rendezvous rejection is not an ambiguous enrollment.
//
// rendezvous-v1 §7 makes every public failure that is not a rate limit one
// response: 404, `text/plain; charset=utf-8`, `nosniff`, and the ten bytes
// "not found\n". On /v1/enroll that answer is only ever written by a
// `reject(w)` return, and every one of those returns before `Bind` — so a
// §7 rejection is positive evidence that **nothing was bound**.
//
// That matters because of what the two outcomes cost. When rv may have bound
// the key and we never learned the handle, the invite is spent, the identity
// is stranded, and only an operator can recover it — enrollment is genuinely
// ambiguous and FR-02's delete applies. When rv said "no", nothing happened
// at all: the invite is untouched and the right next step is to run the same
// command again.
//
// Until now both landed in StateAmbiguous with the invite shredded, so the
// common failure (an expired, mistyped, already-redeemed or not-yet-issued
// code) destroyed a credential that was still good and left behind a state
// file that refuses to start.

// unissuedInvite is well-formed but was never issued, so the fake answers
// `constantReject` — the §7 dump byte for byte. newFakeRV pre-issues
// vectorInviteGrouped, so a rejection needs a code it has not heard of.
const unissuedInvite = "1C60-T3GF-0410-6105-0R3G-G28A"

// A clean rejection leaves the invite exactly where the user put it.
func TestAT_FR_02_f_CleanRejectionKeepsTheInvite(t *testing.T) {
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	cfg.InviteFile = writeInviteFile(t, t.TempDir(), unissuedInvite, 0o600)
	before, err := os.ReadFile(cfg.InviteFile)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := ctxTO(t)
	defer cancel()

	c := NewClient(cfg)
	err = c.Start(ctx)
	requireClass(t, err, ClassEnrollRejected)

	after, rerr := os.ReadFile(cfg.InviteFile)
	if rerr != nil {
		t.Fatalf("invite file gone after a rejection that bound nothing: %v", rerr)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("invite file mutated: %q -> %q", before, after)
	}
	if fake.EnrollCount() != 1 {
		t.Fatalf("enroll attempts = %d, want 1", fake.EnrollCount())
	}
}

// …and leaves no state the next start has to be talked out of.
func TestAT_FR_02_f_CleanRejectionLeavesNoAmbiguousState(t *testing.T) {
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	cfg.InviteFile = writeInviteFile(t, t.TempDir(), unissuedInvite, 0o600)

	ctx, cancel := ctxTO(t)
	defer cancel()

	c := NewClient(cfg)
	requireClass(t, c.Start(ctx), ClassEnrollRejected)

	st, raw, ok := readStateFile(t, c.StatePath())
	if !ok {
		return // nothing persisted at all is the cleanest possible outcome
	}
	if st.Status == StateAmbiguous {
		t.Fatalf("rejection persisted StateAmbiguous; rv answered, so nothing was bound: %s", raw)
	}
	if st.Pending != nil {
		t.Fatalf("rejection left a pending enrollment record: %s", raw)
	}
}

// The whole point: fix the cause, run the same command, be enrolled. No new
// flag, no new invite, no operator.
func TestAT_FR_02_f_CleanRejectionIsRetryableWithNoNewInput(t *testing.T) {
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	cfg.InviteFile = writeInviteFile(t, t.TempDir(), unissuedInvite, 0o600)

	ctx, cancel := ctxTO(t)
	defer cancel()

	c1 := NewClient(cfg)
	requireClass(t, c1.Start(ctx), ClassEnrollRejected)
	_ = c1.Close()

	// The operator issues the code (or the user stops mistyping it).
	fake.Issue(unissuedInvite)

	// Same config, same invite file, same data directory: a plain rerun.
	c2 := NewClient(cfg)
	if err := c2.Start(ctx); err != nil {
		t.Fatalf("rerun after a rejection: %v", err)
	}
	got, err := c2.State()
	if err != nil || got != StateEnrolled {
		t.Fatalf("state after rerun = %q (%v), want %s", got, err, StateEnrolled)
	}
	// And now it is confirmed, so FR-02's delete finally applies.
	if _, err := os.Stat(cfg.InviteFile); !os.IsNotExist(err) {
		t.Fatalf("invite survived a confirmed enrollment (stat err %v)", err)
	}
}

// The narrowing must not become an inversion. A lost response is still
// ambiguous, still burns the invite, and still needs an explicit recovery —
// AT-FR-02-e depends on all three.
func TestAT_FR_02_f_LostResponseStillBurnsTheInvite(t *testing.T) {
	fake := newFakeRV(t)
	fake.Issue(vectorInviteGrouped)
	fake.DropEnrollResponse(true)
	cfg := clientCfg(t, fake)
	cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)

	ctx, cancel := ctxTO(t)
	defer cancel()

	c := NewClient(cfg)
	err := c.Start(ctx)
	if err == nil {
		t.Fatal("Start succeeded after a dropped enroll response")
	}
	if classOf(err) == ClassEnrollRejected {
		t.Fatal("a dropped response was classified as a clean rejection; rv may have bound the key")
	}
	scanRootForInvite(t, cfg.DataDir, vectorInviteGrouped)

	st, raw, ok := readStateFile(t, c.StatePath())
	if !ok {
		t.Fatal("identity was not persisted before the lost response")
	}
	if st.Status != StateAmbiguous && st.Status != StatePartial {
		t.Fatalf("status = %q, want ambiguous evidence; bytes=%s", st.Status, raw)
	}
}

// A rejected rotate must not cost the user the enrollment they already had.
// enrollNew overwrites c.st before it posts, so without a rollback a bad
// invite typed at an enrolled installation would replace a working identity
// with a stranded keypair.
func TestAT_FR_02_f_RejectedRotateKeepsTheEnrolledIdentity(t *testing.T) {
	fake := newFakeRV(t)
	fake.Issue(vectorInviteGrouped)
	cfg := clientCfg(t, fake)
	cfg.InviteFile = writeInviteFile(t, t.TempDir(), vectorInviteGrouped, 0o600)

	ctx, cancel := ctxTO(t)
	defer cancel()

	c := NewClient(cfg)
	if err := c.Start(ctx); err != nil {
		t.Fatalf("first enrollment: %v", err)
	}
	enrolled, _, ok := readStateFile(t, c.StatePath())
	if !ok || enrolled.Status != StateEnrolled {
		t.Fatalf("setup did not enroll: %+v", enrolled)
	}

	_ = c.Close()

	// A second, unissued invite presented with an explicit rotate.
	rotate := cfg
	rotate.Rotate = true
	rotate.InviteFile = writeInviteFile(t, t.TempDir(), unissuedInvite, 0o600)

	c2 := NewClient(rotate)
	requireClass(t, c2.Start(ctx), ClassEnrollRejected)

	after, raw, ok := readStateFile(t, c2.StatePath())
	if !ok {
		t.Fatal("a rejected rotate deleted the state file")
	}
	if after.Status != StateEnrolled {
		t.Fatalf("status = %q after a rejected rotate, want %s; bytes=%s", after.Status, StateEnrolled, raw)
	}
	if after.PublicKey != enrolled.PublicKey || after.PrivateKey != enrolled.PrivateKey {
		t.Fatal("a rejected rotate replaced the working installation identity")
	}
	if after.Handle != enrolled.Handle {
		t.Fatalf("handle = %q after a rejected rotate, want %q", after.Handle, enrolled.Handle)
	}
	if _, err := os.Stat(rotate.InviteFile); err != nil {
		t.Fatalf("the rejected invite was destroyed: %v", err)
	}
}

// The message is the whole feature. It must say the invite survived and
// that a rerun is the fix, and it must not send the user looking for a
// recovery flag or an operator.
func TestAT_FR_02_f_RejectionGuidanceSaysRetry(t *testing.T) {
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	cfg.InviteFile = writeInviteFile(t, t.TempDir(), unissuedInvite, 0o600)

	ctx, cancel := ctxTO(t)
	defer cancel()

	err := NewClient(cfg).Start(ctx)
	requireClass(t, err, ClassEnrollRejected)

	g := strings.ToLower(guidanceOf(err))
	if !strings.Contains(g, "invite") {
		t.Errorf("guidance does not mention the invite: %q", guidanceOf(err))
	}
	if !strings.Contains(g, "again") {
		t.Errorf("guidance does not tell the user to try again: %q", guidanceOf(err))
	}
	for _, wrong := range []string{"ambiguous", "revoke", "explicit retry"} {
		if strings.Contains(g, wrong) {
			t.Errorf("guidance mentions %q, which is the other failure: %q", wrong, guidanceOf(err))
		}
	}
	// The invite is a secret; it must not be echoed back to the terminal.
	canon, perr := specParseInvite(unissuedInvite)
	if perr != nil {
		t.Fatal(perr)
	}
	if strings.Contains(err.Error(), unissuedInvite) || strings.Contains(err.Error(), canon) {
		t.Error("the rejection message echoes the invite plaintext")
	}
}

// A rejection is only "nothing happened" when nothing had happened yet.
// Retrying out of an ambiguous enrollment and being rejected proves nothing
// about the *earlier* request — and a redeemed invite is rejected byte for
// byte like an unissued one — so the installation stays ambiguous and the
// invite is treated as spent.
func TestAT_FR_02_f_RejectionDuringExplicitRetryStaysAmbiguous(t *testing.T) {
	fake := newFakeRV(t)
	fake.Issue(vectorInviteGrouped)
	fake.DropEnrollResponse(true)
	cfg := clientCfg(t, fake)
	cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)

	ctx, cancel := ctxTO(t)
	defer cancel()

	c := NewClient(cfg)
	if err := c.Start(ctx); err == nil {
		t.Fatal("Start succeeded after a dropped enroll response")
	}
	_ = c.Close()

	// rv now answers, and rejects: the code it bound is redeemed.
	fake.DropEnrollResponse(false)
	retryDir := t.TempDir()
	retry := cfg
	retry.ExplicitRetry = true
	retry.InviteFile = writeInviteFile(t, retryDir, vectorInviteGrouped, 0o600)

	err := NewClient(retry).Start(ctx)
	requireClass(t, err, ClassAmbiguousEnrollment)
	if strings.Contains(strings.ToLower(guidanceOf(err)), "unchanged") {
		t.Errorf("guidance claims the invite is unchanged after an ambiguous retry: %q", guidanceOf(err))
	}
	scanRootForInvite(t, retryDir, vectorInviteGrouped)
}

// The genuinely ambiguous message names the only thing that can fix it,
// which is an operator — not a flag the binary does not expose.
func TestAT_FR_02_f_AmbiguousGuidanceNamesTheOperator(t *testing.T) {
	fake := newFakeRV(t)
	fake.Issue(vectorInviteGrouped)
	fake.DropEnrollResponse(true)
	cfg := clientCfg(t, fake)
	cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)

	ctx, cancel := ctxTO(t)
	defer cancel()

	c := NewClient(cfg)
	if err := c.Start(ctx); err == nil {
		t.Fatal("Start succeeded after a dropped enroll response")
	}
	_ = c.Close()

	// The next start is the one the human reads.
	nextStart := Config{
		DataDir:       cfg.DataDir,
		Remote:        true,
		Origin:        DefaultOrigin,
		RendezvousURL: fake.URL(),
		HTTPClient:    fake.Client(),
		Stdout:        io.Discard,
		Stderr:        new(bytes.Buffer),
	}
	err := NewClient(nextStart).Start(ctx)
	requireClass(t, err, ClassAmbiguousEnrollment)

	g := strings.ToLower(guidanceOf(err))
	if !strings.Contains(g, "revoke") && !strings.Contains(g, "new invite") {
		t.Errorf("ambiguous guidance names no operator action: %q", guidanceOf(err))
	}
}

// TestAT_FR_02_f_NearMissRejectionsStayAmbiguous fences the discriminator on
// the side that costs something to get wrong. `isConstantRejection` is what
// tells the user their invite is untouched, and the only thing entitling it
// to say that is rv's §7 dump — every `reject(w)` in handleEnroll returns
// before Bind, so *that exact answer* is proof no key was bound.
//
// A 404 from anything else carries no such proof. A CDN's branded error page,
// a captive portal, a reverse proxy that lost the backend after it forwarded
// the request — each can arrive with a 404 written *after* rv spent the
// invite. Treating those as clean would keep a spent credential on disk and
// tell the human to run the same command again, which is precisely the
// failure the ambiguous class exists to name.
//
// So the check is exact and every frozen part of the dump is load-bearing:
// the two subtests differ from §7 in one field each — the body, then the
// content type — and both must fall through to ambiguous with the invite
// erased. Neither goes through fakeRV: a knob for "answer wrongly" would
// weaken the fake for every other test that relies on it speaking protocol.
func TestAT_FR_02_f_NearMissRejectionsStayAmbiguous(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        string
	}{
		{
			name:        "wrong body",
			contentType: "text/plain; charset=utf-8",
			body:        "<html><title>404 Not Found</title></html>",
		},
		{
			name:        "wrong content type",
			contentType: "text/html; charset=utf-8",
			body:        "not found\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// The near miss is on enroll. §4.0 is answered
				// properly so the pre-flight probe passes and the
				// enroll path is reached at all — a server that
				// near-missed the probe too would never post the
				// invite, which is a different property (F1-f).
				if r.URL.Path == "/v1/hello" {
					helloOK(w)
					return
				}
				w.Header().Set("Content-Type", tc.contentType)
				w.Header().Set("X-Content-Type-Options", "nosniff")
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			cfg := clientCfg(t, nil)
			cfg.RendezvousURL = srv.URL
			cfg.HTTPClient = srv.Client()
			cfg.InviteFile = writeInviteFile(t, t.TempDir(), unissuedInvite, 0o600)

			ctx, cancel := ctxTO(t)
			defer cancel()
			c, err := startClient(ctx, cfg)
			if c != nil {
				defer c.Close()
			}
			if got := classOf(err); got != ClassAmbiguousEnrollment {
				t.Fatalf("error class = %q, want %q (err=%v)", got, ClassAmbiguousEnrollment, err)
			}
			// A near miss may have spent the invite, so the plaintext is
			// destroyed exactly as it is for any other non-200.
			scanRootForInvite(t, filepath.Dir(cfg.InviteFile), unissuedInvite)
			scanRootForInvite(t, cfg.DataDir, unissuedInvite)
		})
	}
}
