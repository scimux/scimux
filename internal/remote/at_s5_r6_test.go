package remote

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	r6WaitReleaseNoContent = 1
	r6WaitReleaseHangup    = 2
	r6WaitReleaseRevoked   = 3
)

func TestS5R6_F1_Wait404ThenFreshChallengeStaysEnrolled(t *testing.T) {
	srv := newR6WaitRV(t)
	cfg := r3ClientCfg(t, srv.URL, srv.Client)
	pub, priv := newEd25519(t)
	rid := strings.Repeat("ab", 32)
	writeState(t, NewClient(cfg), PersistedState{
		V:          1,
		Status:     StateEnrolled,
		Handle:     "ih_041061050R3GG28A",
		PublicKey:  hex.EncodeToString(pub),
		PrivateKey: hex.EncodeToString(priv),
		Origin:     DefaultOrigin,
		Devices: []PersistedDevice{{
			ID:     "phone",
			RID:    rid,
			PubKey: hex.EncodeToString(testPhonePubKey),
		}},
	})
	cfg.Backoff = BackoffConfig{Initial: 20 * time.Millisecond, Max: 200 * time.Millisecond, Factor: 2, Jitter: 0, SuccessFor: 5 * time.Second}
	ctx, cancel := ctxTO(t)
	defer cancel()
	c := NewClient(cfg)
	if err := c.Start(ctx); err != nil {
		t.Fatalf("F1: start: %v", err)
	}
	defer c.Close()

	w1 := srv.WaitHeld(t, 5*time.Second)
	rejectedChal := w1.Challenge
	nChal := srv.ChallengeCount()
	srv.Release(w1, r6WaitReleaseRevoked)

	deadline := time.Now().Add(5 * time.Second)
	var w2 *r6HeldWait
	for time.Now().Before(deadline) {
		assertPersistedNotRevoked(t, c)
		if st, _ := c.State(); st == StateRevoked {
			t.Fatal("F1: wait 404 persisted StateRevoked")
		}
		if srv.ChallengeCount() > nChal {
			select {
			case w2 = <-srv.held:
			default:
			}
			if w2 != nil {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if w2 == nil {
		t.Fatalf("F1: no resumed wait after wait 404; challenges %d -> %d, last hosted %q", nChal, srv.ChallengeCount(), c.HostedStatus())
	}
	defer srv.Release(w2, r6WaitReleaseNoContent)

	if srv.ChallengeCount() <= nChal {
		t.Fatalf("F1: ChallengeCount did not increase after rejected wait: %d -> %d", nChal, srv.ChallengeCount())
	}
	if w2.Challenge == "" || w2.Challenge == rejectedChal {
		t.Fatalf("F1: resumed wait reused rejected challenge %q", rejectedChal)
	}
	chal, err := hex.DecodeString(w2.Challenge)
	if err != nil || len(chal) != 32 {
		t.Fatalf("F1: resumed challenge %q", w2.Challenge)
	}
	sig, err := hex.DecodeString(w2.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		t.Fatalf("F1: resumed sig %q", w2.Sig)
	}
	msg := buildAuthMessage(DefaultOrigin, ProtocolVersion, "/v1/wait", "ih_041061050R3GG28A", chal)
	if !ed25519.Verify(pub, msg, sig) {
		t.Fatal("F1: resumed wait was not newly signed over the fresh challenge")
	}
	if w2.ID != rid {
		t.Fatalf("F1: resumed wait id = %q, want %q", w2.ID, rid)
	}
	st, raw := mustPersisted(t, c)
	if st.Status != StateEnrolled {
		t.Fatalf("F1: persisted status = %q, want enrolled; bytes=%s", st.Status, raw)
	}
}

func TestS5R6_F1_Wait404AloneNeverWritesRevoked(t *testing.T) {
	srv := newR6WaitRV(t)
	cfg := r3ClientCfg(t, srv.URL, srv.Client)
	pub, priv := newEd25519(t)
	writeState(t, NewClient(cfg), PersistedState{
		V:          1,
		Status:     StateEnrolled,
		Handle:     "ih_041061050R3GG28A",
		PublicKey:  hex.EncodeToString(pub),
		PrivateKey: hex.EncodeToString(priv),
		Origin:     DefaultOrigin,
		Devices: []PersistedDevice{{
			ID:     "phone",
			RID:    strings.Repeat("ab", 32),
			PubKey: hex.EncodeToString(testPhonePubKey),
		}},
	})
	sched := newDelaySched()
	cfg.Scheduler = sched
	cfg.Backoff = BackoffConfig{Initial: 50 * time.Millisecond, Max: 800 * time.Millisecond, Factor: 2, Jitter: 0, SuccessFor: 5 * time.Second}
	ctx, cancel := ctxTO(t)
	defer cancel()
	c := NewClient(cfg)
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	w1 := srv.WaitHeld(t, 5*time.Second)
	nChal := srv.ChallengeCount()
	srv.Release(w1, r6WaitReleaseRevoked)

	deadline := time.Now().Add(2 * time.Second)
	saw := false
	for time.Now().Before(deadline) {
		assertPersistedNotRevoked(t, c)
		if st, _ := c.State(); st == StateRevoked {
			t.Fatal("F1: wait 404 alone wrote StateRevoked")
		}
		if c.HostedStatus() == "revoked" {
			t.Fatal("F1: wait 404 alone hosted revoked")
		}
		if c.HostedStatus() == "unavailable" || sched.Pending() > 0 {
			saw = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !saw {
		t.Fatalf("F1: wait 404 was not processed as a retryable rejection; hosted=%q pending=%d", c.HostedStatus(), sched.Pending())
	}
	assertPersistedNotRevoked(t, c)
	if srv.ChallengeCount() != nChal {
		t.Fatalf("F1: wait 404 alone looked up a challenge before retry: %d -> %d", nChal, srv.ChallengeCount())
	}
	st, _ := c.State()
	if st != StateEnrolled {
		t.Fatalf("F1: in-memory status = %q, want enrolled", st)
	}
}

func TestS5R6_F1_Wait404ThenChallengeRevokeStopsEveryRID(t *testing.T) {
	srv := newR6WaitRV(t)
	cfg := r3ClientCfg(t, srv.URL, srv.Client)
	pub, priv := newEd25519(t)
	rid1 := strings.Repeat("ab", 32)
	rid2 := strings.Repeat("cd", 32)
	writeState(t, NewClient(cfg), PersistedState{
		V:          1,
		Status:     StateEnrolled,
		Handle:     "ih_041061050R3GG28A",
		PublicKey:  hex.EncodeToString(pub),
		PrivateKey: hex.EncodeToString(priv),
		Origin:     DefaultOrigin,
		Devices: []PersistedDevice{
			{ID: "phone", RID: rid1, PubKey: hex.EncodeToString(testPhonePubKey)},
			{ID: "tablet", RID: rid2, PubKey: hex.EncodeToString(testTabletPubKey)},
		},
	})
	cfg.Backoff = BackoffConfig{Initial: 20 * time.Millisecond, Max: 200 * time.Millisecond, Factor: 2, Jitter: 0, SuccessFor: 5 * time.Second}
	ctx, cancel := ctxTO(t)
	defer cancel()
	c := NewClient(cfg)
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	w1 := srv.WaitHeld(t, 5*time.Second)
	w2 := srv.WaitHeld(t, 5*time.Second)
	nChal := srv.ChallengeCount()
	nWait := srv.WaitArrived()
	srv.SetChallengeRevoke(true)
	srv.Release(w1, r6WaitReleaseRevoked)
	srv.Release(w2, r6WaitReleaseRevoked)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.HostedStatus() == "revoked" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if c.HostedStatus() != "revoked" {
		t.Fatalf("F1: hosted = %q, want revoked", c.HostedStatus())
	}
	st, raw := mustPersisted(t, c)
	if st.Status != StateRevoked {
		t.Fatalf("F1: persisted status = %q, want revoked; bytes=%s", st.Status, raw)
	}
	if srv.ChallengeCount() <= nChal {
		t.Fatalf("F1: challenge revocation did not follow wait rejection: chal %d -> %d", nChal, srv.ChallengeCount())
	}

	time.Sleep(80 * time.Millisecond)
	if srv.WaitArrived() > nWait+2 {
		t.Fatalf("F1: RID loops still reconnecting after challenge revoke: waits %d -> %d", nWait, srv.WaitArrived())
	}
	if srv.InFlight() != 0 {
		t.Fatalf("F1: %d waits still in flight after challenge revoke", srv.InFlight())
	}
}

func TestS5R6_F2_SuccessfulEnrollmentRemovesInvitePath(t *testing.T) {
	ctx, cancel := ctxTO(t)
	defer cancel()
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	path := writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	cfg.InviteFile = path
	orig, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer orig.Close()
	c, err := startClient(ctx, cfg)
	if err != nil {
		t.Fatalf("F2: enroll: %v", err)
	}
	defer c.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("F2: successful enrollment left original invite pathname: %v", err)
	}
	assertOriginalInviteGone(t, orig)
}

func TestS5R6_F2_ReplacementAfterLastIdentityCheckPreserved(t *testing.T) {
	ctx, cancel := ctxTO(t)
	defer cancel()
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	path := writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	cfg.InviteFile = path
	orig, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer orig.Close()
	replacement := []byte("r6-final-boundary-replacement\n")
	cfg.BeforeInviteUnlinkFinal = func() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Errorf("replace remove: %v", err)
		}
		if err := os.WriteFile(path, replacement, 0o600); err != nil {
			t.Errorf("replace write: %v", err)
		}
	}
	c, err := startClient(ctx, cfg)
	if err != nil {
		t.Fatalf("F2: enroll: %v", err)
	}
	defer c.Close()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("F2: replacement after last identity check was deleted: %v", err)
	}
	if !bytes.Equal(got, replacement) {
		t.Fatalf("F2: pathname after consume = %q, want replacement %q", got, replacement)
	}
	assertOriginalInviteGone(t, orig)
}

func TestS5R6_F2_PreNetworkFailurePreservesInvite(t *testing.T) {
	ctx, cancel := ctxTO(t)
	defer cancel()
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	path := writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	cfg.InviteFile = path
	cfg.FailWrite = &WriteFault{Step: WriteTempCreate}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	err = NewClient(cfg).Start(ctx)
	if err == nil {
		t.Fatal("F2: pre-network persist failure succeeded")
	}
	if fake.EnrollCount() != 0 {
		t.Fatalf("F2: pre-network failure contacted enroll (%d)", fake.EnrollCount())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("F2: pre-network failure removed invite: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("F2: pre-network failure mutated invite: %q -> %q", before, after)
	}
}

func TestS5R6_F2_CleanupFailureReportsGuidance(t *testing.T) {
	ctx, cancel := ctxTO(t)
	defer cancel()
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	path := writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	cfg.InviteFile = path
	fail := fmt.Errorf("injected invite cleanup failure")
	cfg.Unlinkat = func(int, string) error { return fail }
	cfg.Renameat = func(int, string, int, string) error { return fail }
	c := NewClient(cfg)
	err := c.Start(ctx)
	st, _ := c.State()
	if st != StateEnrolled {
		t.Fatalf("F2: cleanup failure undid enrollment: state=%q err=%v", st, err)
	}
	if fake.EnrollCount() != 1 {
		t.Fatalf("F2: cleanup failure retried enroll (%d)", fake.EnrollCount())
	}
	plaintextLeft := invitePlaintextAt(path)
	if err == nil && plaintextLeft {
		t.Fatal("F2: successful enrollment silently left invite plaintext")
	}
	if err == nil {
		t.Fatal("F2: cleanup failure was swallowed after confirmed enrollment")
	}
	requireClass(t, err, ClassInviteFile)
	g := strings.ToLower(err.Error() + guidanceOf(err))
	for _, w := range []string{"enrollment succeeded", "invite", "delete", "restart"} {
		if !strings.Contains(g, w) {
			t.Errorf("F2: cleanup guidance missing %q: %v", w, err)
		}
	}
	if strings.Contains(g, "retry") && !strings.Contains(g, "do not retry") && !strings.Contains(g, "without retry") && !strings.Contains(g, "not retry") {
		t.Errorf("F2: cleanup guidance must not invite a retry of confirmed enrollment: %v", err)
	}
	requireNoInviteLeak(t, vectorInviteGrouped, err.Error(), guidanceOf(err))

	pub, errPub := c.PublicKey()
	if errPub != nil {
		t.Fatal(errPub)
	}
	handle, errH := c.Handle()
	if errH != nil {
		t.Fatal(errH)
	}
	_ = c.Close()

	cfg2 := cfg
	cfg2.InviteFile = ""
	cfg2.Unlinkat = nil
	cfg2.Renameat = nil
	c2, err := startClient(ctx, cfg2)
	if err != nil {
		t.Fatalf("F2: restart without invite after cleanup failure: %v", err)
	}
	defer c2.Close()
	pub2, err := c2.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pub, pub2) {
		t.Fatal("F2: restart after cleanup failure rotated the identity")
	}
	h2, err := c2.Handle()
	if err != nil || h2 != handle {
		t.Fatalf("F2: restart handle = %q, want %q (%v)", h2, handle, err)
	}
	if fake.EnrollCount() != 1 {
		t.Fatalf("F2: restart re-sent enroll (%d)", fake.EnrollCount())
	}
}

func assertPersistedNotRevoked(t *testing.T, c *Client) {
	t.Helper()
	st, raw := mustPersisted(t, c)
	if st.Status == StateRevoked {
		t.Fatalf("persisted status = revoked; bytes=%s", raw)
	}
}

func mustPersisted(t *testing.T, c *Client) (PersistedState, []byte) {
	t.Helper()
	st, raw, ok := readStateFile(t, c.StatePath())
	if !ok {
		t.Fatal("state file missing")
	}
	return st, raw
}

func assertOriginalInviteGone(t *testing.T, orig *os.File) {
	t.Helper()
	if _, err := orig.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("F2: seek original object: %v", err)
	}
	got, err := io.ReadAll(orig)
	if err != nil {
		t.Fatalf("F2: read original object: %v", err)
	}
	if bytes.Contains(got, []byte(vectorInviteGrouped)) || bytes.Contains(got, []byte(vectorInviteCanon)) {
		t.Fatalf("F2: consumed plaintext still in the validated original object: %q", got)
	}
}

func invitePlaintextAt(path string) bool {
	got, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return bytes.Contains(got, []byte(vectorInviteGrouped)) || bytes.Contains(got, []byte(vectorInviteCanon))
}

type r6HeldWait struct {
	w         http.ResponseWriter
	r         *http.Request
	release   chan int
	done      chan struct{}
	Challenge string
	Sig       string
	ID        string
}

type r6WaitRV struct {
	URL    string
	Client *http.Client

	mu              sync.Mutex
	pub             ed25519.PublicKey
	handle          string
	challenges      int
	waits           int
	held            chan *r6HeldWait
	inFlight        int
	challengeRevoke atomic.Bool
}

func newR6WaitRV(t *testing.T) *r6WaitRV {
	t.Helper()
	s := &r6WaitRV{held: make(chan *r6HeldWait, 8), handle: "ih_041061050R3GG28A"}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(func() {
		s.SetChallengeRevoke(true)
		for {
			select {
			case h := <-s.held:
				s.Release(h, r6WaitReleaseRevoked)
			default:
				srv.Close()
				return
			}
		}
	})
	s.URL = srv.URL
	s.Client = srv.Client()
	return s
}

func (s *r6WaitRV) SetChallengeRevoke(v bool) { s.challengeRevoke.Store(v) }

func (s *r6WaitRV) ChallengeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.challenges
}

func (s *r6WaitRV) WaitArrived() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waits
}

func (s *r6WaitRV) InFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inFlight
}

func (s *r6WaitRV) WaitHeld(t *testing.T, d time.Duration) *r6HeldWait {
	t.Helper()
	select {
	case h := <-s.held:
		return h
	case <-time.After(d):
		t.Fatalf("F1: no wait held within %s", d)
		return nil
	}
}

func (s *r6WaitRV) Release(h *r6HeldWait, how int) {
	if h == nil {
		return
	}
	select {
	case h.release <- how:
	case <-time.After(2 * time.Second):
	}
	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
	}
}

func (s *r6WaitRV) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 16<<10))
	switch r.URL.Path {
	case "/v1/enroll":
		var req struct {
			PubKey string `json:"pubkey"`
		}
		_ = json.Unmarshal(body, &req)
		raw, _ := hex.DecodeString(req.PubKey)
		s.mu.Lock()
		s.pub = raw
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		fmt.Fprintf(w, "{\"handle\":%q,\"v\":1}\n", s.handle)
	case "/v1/challenge":
		s.mu.Lock()
		s.challenges++
		n := s.challenges
		s.mu.Unlock()
		if s.challengeRevoke.Load() {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, "not found\n")
			return
		}
		chal := bytes.Repeat([]byte{byte(n)}, 32)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		fmt.Fprintf(w, "{\"challenge\":%q,\"v\":1}\n", hex.EncodeToString(chal))
	case "/v1/verify":
		w.WriteHeader(http.StatusNoContent)
	case "/v1/wait":
		s.handleWait(w, r, body)
	default:
		http.NotFound(w, r)
	}
}

func (s *r6WaitRV) handleWait(w http.ResponseWriter, r *http.Request, body []byte) {
	var req struct {
		Challenge string `json:"challenge"`
		Sig       string `json:"sig"`
		ID        string `json:"id"`
	}
	_ = json.Unmarshal(body, &req)
	s.mu.Lock()
	s.waits++
	s.inFlight++
	s.mu.Unlock()
	h := &r6HeldWait{
		w: w, r: r, release: make(chan int, 1), done: make(chan struct{}),
		Challenge: req.Challenge, Sig: req.Sig, ID: req.ID,
	}
	s.held <- h
	how := r6WaitReleaseNoContent
	select {
	case how = <-h.release:
	case <-r.Context().Done():
		s.mu.Lock()
		s.inFlight--
		s.mu.Unlock()
		close(h.done)
		return
	}
	defer func() {
		s.mu.Lock()
		s.inFlight--
		s.mu.Unlock()
		close(h.done)
	}()
	switch how {
	case r6WaitReleaseHangup:
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "fail", http.StatusInternalServerError)
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			http.Error(w, "fail", http.StatusInternalServerError)
			return
		}
		conn.Close()
	case r6WaitReleaseRevoked:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "not found\n")
	default:
		next := bytes.Repeat([]byte{0x44}, 32)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Rv-Challenge", hex.EncodeToString(next))
		w.WriteHeader(http.StatusNoContent)
	}
}
