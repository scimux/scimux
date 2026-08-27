package remote

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// F1 — prove the path before spending the credential.
//
// An invite is single-use. Until now the client learned whether a
// rendezvous was reachable by posting the invite to it: enrollPosted was
// set one line before Do(req), and Do is where DNS, dial and TLS happen.
// So a typo'd URL, an offline laptop and a blocked port all arrived as
// StateAmbiguous — the invite erased, a stuck state persisted, and the
// only recovery an operator's.
//
// rendezvous-v1 §4.0 exists to remove that trade. GET /v1/hello is
// unauthenticated and stateless, so the client can prove reachability
// and version compatibility first and leave the invite exactly where the
// user put it when either fails.
//
// The rule these tests hold the code to: an invite is read, posted and
// erased *only* on evidence that the path works and the credential is
// well-formed. Every failure before that point writes nothing to disk.
//
//	F1-a  TestAT_F1_a_UnreachableRendezvousSpendsNothing
//	F1-b  TestAT_F1_b_AVersionWeCannotSpeakSpendsNothing
//	F1-c  TestAT_F1_c_ANewerRendezvousThatStillAcceptsUsEnrolls
//	F1-d  TestAT_F1_d_TheProbeComesBeforeTheInvite
//	F1-e  TestAT_F1_e_AReachableRendezvousEnrollsExactlyAsBefore
//	F1-f  TestAT_F1_f_SomethingElseAnsweringTheProbeSpendsNothing
//	F1-g  TestAT_F1_g_ARendezvousOlderThanTheProbeStillEnrolls

// inviteUntouched is the property every pre-flight failure must have:
// the user's only copy of the credential is still there, byte for byte,
// with the mode they gave it.
func inviteUntouched(t *testing.T, path string, before []byte, perm os.FileMode) {
	t.Helper()
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("invite file gone after a pre-flight failure: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("invite file mutated: %q -> %q", before, after)
	}
	if got := modePerm(t, path); got != perm {
		t.Fatalf("invite file mode %o, want %o", got, perm)
	}
}

// nothingPersisted is the other half. A failed probe must not leave a
// state file behind: the next run has to be a plain rerun of the same
// command, not a recovery.
func nothingPersisted(t *testing.T, c *Client) {
	t.Helper()
	if _, raw, ok := readStateFile(t, c.StatePath()); ok {
		t.Fatalf("a pre-flight failure persisted state: %s", raw)
	}
	if _, err := os.Stat(c.tempPath()); err == nil {
		t.Fatal("a pre-flight failure left a temp state file behind")
	}
}

func TestAT_F1_a_UnreachableRendezvousSpendsNothing(t *testing.T) {
	// No fake at all: clientCfg falls back to a transport that refuses
	// every request, which is what a typo'd host, an offline laptop and
	// a blocked port have in common.
	cfg := clientCfg(t, nil)
	path := writeInviteFile(t, t.TempDir(), vectorInviteGrouped, 0o600)
	cfg.InviteFile = path
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := ctxTO(t)
	defer cancel()

	c := NewClient(cfg)
	// Not ambiguous. Nothing was sent, so nothing is in doubt, and the
	// class has to say so or the guidance sends the user to an operator
	// for a problem their own network caused.
	requireClass(t, c.Start(ctx), ClassUnreachable)

	inviteUntouched(t, path, before, 0o600)
	nothingPersisted(t, c)
}

func TestAT_F1_b_AVersionWeCannotSpeakSpendsNothing(t *testing.T) {
	fake := newFakeRV(t)
	// A rendezvous that has moved past us: its floor is above what this
	// build speaks, so enrolling could only ever be refused.
	fake.SetHelloWindow(ProtocolVersion+1, ProtocolVersion+1)

	cfg := clientCfg(t, fake)
	path := writeInviteFile(t, t.TempDir(), vectorInviteGrouped, 0o600)
	cfg.InviteFile = path
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := ctxTO(t)
	defer cancel()

	c := NewClient(cfg)
	requireClass(t, c.Start(ctx), ClassVersionMismatch)

	inviteUntouched(t, path, before, 0o600)
	nothingPersisted(t, c)

	// And the probe was the whole conversation. Posting the invite to a
	// server that cannot accept it is the exact waste F1 removes.
	if fake.EnrollCount() != 0 {
		t.Fatalf("enroll attempts = %d, want 0 against an incompatible rendezvous", fake.EnrollCount())
	}
}

func TestAT_F1_c_ANewerRendezvousThatStillAcceptsUsEnrolls(t *testing.T) {
	fake := newFakeRV(t)
	// §3: minor versions are additive. A server ahead of us that still
	// accepts v1 is compatible, and refusing it would lock this build
	// out of every future deployment for no reason.
	fake.SetHelloWindow(ProtocolVersion+99, MinRequestV)

	cfg := clientCfg(t, fake)
	cfg.InviteFile = writeInviteFile(t, t.TempDir(), vectorInviteGrouped, 0o600)

	ctx, cancel := ctxTO(t)
	defer cancel()

	c := NewClient(cfg)
	if err := c.Start(ctx); err != nil {
		t.Fatalf("a newer but still-compatible rendezvous refused: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if fake.EnrollCount() != 1 {
		t.Fatalf("enroll attempts = %d, want 1", fake.EnrollCount())
	}
}

func TestAT_F1_d_TheProbeComesBeforeTheInvite(t *testing.T) {
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	cfg.InviteFile = writeInviteFile(t, t.TempDir(), vectorInviteGrouped, 0o600)

	ctx, cancel := ctxTO(t)
	defer cancel()

	c := NewClient(cfg)
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	reqs := fake.Requests()
	if len(reqs) == 0 {
		t.Fatal("no requests recorded")
	}
	// Ordering is the property, not merely presence: a probe that ran
	// after the enroll would prove nothing about whether the invite was
	// safe to spend.
	if reqs[0].Method != "GET" || reqs[0].Path != "/v1/hello" {
		t.Fatalf("first request was %s %s, want GET /v1/hello", reqs[0].Method, reqs[0].Path)
	}
	for _, r := range reqs {
		if r.Path == "/v1/enroll" {
			break
		}
		if len(r.Body) != 0 {
			t.Fatalf("%s %s carried a body before enrollment: %q", r.Method, r.Path, r.Body)
		}
	}
	// The probe is credential-free by construction, and this is the
	// check that keeps it that way: nothing sent before the enroll may
	// contain the code in any of its forms.
	norm, err := specParseInvite(vectorInviteGrouped)
	if err != nil {
		t.Fatal(err)
	}
	for _, form := range []string{vectorInviteGrouped, norm, specFormatInvite(norm)} {
		for _, r := range reqs {
			if r.Path == "/v1/enroll" {
				break
			}
			if bytes.Contains(r.Body, []byte(form)) {
				t.Fatalf("invite plaintext reached %s %s", r.Method, r.Path)
			}
		}
	}
}

func TestAT_F1_f_SomethingElseAnsweringTheProbeSpendsNothing(t *testing.T) {
	// A captive portal, a CDN error page, a proxy that lost its backend:
	// each answers, none is a rendezvous. The old code could only learn
	// that by posting the invite into it.
	answers := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "branded 404",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte("<html><title>404</title></html>"))
			},
		},
		{
			name: "captive portal 200",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = w.Write([]byte("<html>sign in to continue</html>"))
			},
		},
		{
			name: "json without a version window",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":"ok"}`))
			},
		},
	}
	for _, tc := range answers {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			cfg := clientCfg(t, nil)
			cfg.RendezvousURL = srv.URL
			cfg.HTTPClient = srv.Client()
			path := writeInviteFile(t, t.TempDir(), vectorInviteGrouped, 0o600)
			cfg.InviteFile = path
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := ctxTO(t)
			defer cancel()

			c := NewClient(cfg)
			requireClass(t, c.Start(ctx), ClassUnreachable)

			inviteUntouched(t, path, before, 0o600)
			nothingPersisted(t, c)
		})
	}
}

func TestAT_F1_g_ARendezvousOlderThanTheProbeStillEnrolls(t *testing.T) {
	// The one non-200 that is positive evidence: rv's own §7 constant
	// rejection. It proves this really is a rendezvous, reachable, that
	// simply predates §4.0 — and every rv that has ever existed accepts
	// v1. Refusing it would brick this build against a deployment that
	// has not been updated yet, for a probe that is only an optimisation.
	fake := newFakeRV(t)
	fake.SetHelloUnsupported(true)

	cfg := clientCfg(t, fake)
	cfg.InviteFile = writeInviteFile(t, t.TempDir(), vectorInviteGrouped, 0o600)

	ctx, cancel := ctxTO(t)
	defer cancel()

	c := NewClient(cfg)
	if err := c.Start(ctx); err != nil {
		t.Fatalf("a rendezvous predating the probe was refused: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if fake.EnrollCount() != 1 {
		t.Fatalf("enroll attempts = %d, want 1", fake.EnrollCount())
	}
}

func TestAT_F1_e_AReachableRendezvousEnrollsExactlyAsBefore(t *testing.T) {
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	path := writeInviteFile(t, t.TempDir(), vectorInviteGrouped, 0o600)
	cfg.InviteFile = path

	ctx, cancel := ctxTO(t)
	defer cancel()

	c := NewClient(cfg)
	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	// The happy path is unchanged: enrolled, handle bound, and FR-02's
	// erasure still happens once the credential really was redeemed.
	st, raw, ok := readStateFile(t, c.StatePath())
	if !ok {
		t.Fatal("a successful enrollment persisted nothing")
	}
	if st.Status != StateEnrolled || st.Handle == "" {
		t.Fatalf("state after enrollment is %s: %s", st.Status, raw)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a redeemed invite file survived: %v", err)
	}
}
