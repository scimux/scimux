package remote

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func r3ClientCfg(t *testing.T, url string, hc *http.Client) Config {
	t.Helper()
	return Config{
		DataDir:       t.TempDir(),
		Origin:        DefaultOrigin,
		Remote:        true,
		RendezvousURL: url,
		HTTPClient:    hc,
		Stdout:        io.Discard,
		Stderr:        new(bytes.Buffer),
	}
}

func TestS5R3_F2_ChallengeVerifySignedExactly(t *testing.T) {
	srv := newStrictAuthRV(t)
	cfg := r3ClientCfg(t, srv.URL, srv.Client)
	cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	ctx, cancel := ctxTO(t)
	defer cancel()
	c, err := startClient(ctx, cfg)
	if err != nil {
		t.Fatalf("F2: enroll/auth: %v", err)
	}
	if srv.VerifyCount() == 0 {
		t.Fatal("F2: Start never POSTed /v1/verify")
	}
	if !srv.LastVerifyOK() {
		t.Fatalf("F2: verify rejected: %s", srv.LastReject())
	}
	_ = c.Close()
}

func TestS5R3_F2_RejectsHandleOnlyAndWrongConstruction(t *testing.T) {
	rows := []struct {
		name string
		mut  func([]byte) []byte
	}{
		{"handle-only", func(msg []byte) []byte { return nil }},
		{"wrong-route", func(msg []byte) []byte {
			return bytes.Replace(msg, []byte("/v1/verify"), []byte("/v1/wait"), 1)
		}},
		{"wrong-origin", func(msg []byte) []byte {
			return bytes.Replace(msg, []byte(DefaultOrigin), []byte("https://evil.example"), 1)
		}},
		{"wrong-version", func(msg []byte) []byte {
			return bytes.Replace(msg, []byte{0, '1', 0}, []byte{0, '2', 0}, 1)
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			srv := newStrictAuthRV(t)
			srv.MutateSigned = row.mut
			cfg := r3ClientCfg(t, srv.URL, srv.Client)
			cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
			ctx, cancel := ctxTO(t)
			defer cancel()
			_, err := startClient(ctx, cfg)
			if err == nil && srv.LastVerifyOK() && row.name == "handle-only" {
				t.Fatal("F2: handle-only verify accepted")
			}
			if row.name != "handle-only" && srv.VerifyCount() > 0 && srv.LastVerifyOK() {
				t.Fatalf("F2: %s signature was accepted", row.name)
			}
		})
	}
}

func TestS5R3_F2_TransportErrorIsNotRevocation(t *testing.T) {
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	ctx, cancel := ctxTO(t)
	defer cancel()
	c, err := startClient(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	cfg2 := cfg
	cfg2.InviteFile = ""
	cfg2.HTTPClient = &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("no"))}, nil
		}),
	}
	c2 := NewClient(cfg2)
	err = c2.Start(ctx)
	st, _ := c2.State()
	if st == StateRevoked {
		t.Fatal("F2: 5xx persisted as revoked")
	}
	if err != nil && classOf(err) == ClassRevoked {
		t.Fatal("F2: 5xx classified as revoked")
	}
}

func TestS5R3_F4_DisableAllDoesNotOverwriteCorrupt(t *testing.T) {
	rows := []struct {
		name  string
		write func(*Client)
		class Class
	}{
		{"corrupt", func(c *Client) {
			os.MkdirAll(c.PrivateDir(), 0o700)
			os.WriteFile(c.StatePath(), []byte("this is not json {{{"), 0o600)
		}, ClassCorruptIdentity},
		{"partial", func(c *Client) {
			os.MkdirAll(c.PrivateDir(), 0o700)
			os.WriteFile(c.StatePath()+".tmp", []byte(`{"v":1,"status":"enrolled","public_key":"ab"`), 0o600)
		}, ClassPartialIdentity},
		{"key-missing", func(c *Client) {
			fix := enrolledFixture(t, "ih_041061050R3GG28A")
			writeState(t, c, PersistedState{V: 1, Status: StateEnrolled, Handle: fix.Handle, PublicKey: fix.PublicKey, Origin: DefaultOrigin})
		}, ClassKeyMissing},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			cfg := clientCfg(t, newFakeRV(t))
			c := NewClient(cfg)
			row.write(c)
			var before []byte
			if row.name == "partial" {
				before, _ = os.ReadFile(c.StatePath() + ".tmp")
			} else {
				before, _ = os.ReadFile(c.StatePath())
			}
			err := c.DisableAll(context.Background())
			requireClass(t, err, row.class)
			var after []byte
			if row.name == "partial" {
				after, _ = os.ReadFile(c.StatePath() + ".tmp")
			} else {
				after, _ = os.ReadFile(c.StatePath())
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("F4: DisableAll mutated %s evidence", row.name)
			}
		})
	}
}

func TestS5R3_F4_ReenableRequiresCompleteIdentity(t *testing.T) {
	cfg := clientCfg(t, newFakeRV(t))
	c := NewClient(cfg)
	writeState(t, c, PersistedState{V: 1, Status: StateDisabled, Origin: DefaultOrigin})
	err := c.Reenable(context.Background())
	if err == nil {
		t.Fatal("F4: Reenable succeeded without enrolled identity/handle")
	}
}

func TestS5R3_F5_SemanticCorruption(t *testing.T) {
	pub, priv := newEd25519(t)
	pubHex := hex.EncodeToString(pub)
	privHex := hex.EncodeToString(priv)
	base := PersistedState{V: 1, Status: StateEnrolled, Handle: "ih_041061050R3GG28A", PublicKey: pubHex, PrivateKey: privHex, Origin: DefaultOrigin}
	rows := []struct {
		name string
		mut  func(*PersistedState)
	}{
		{"bad-schema-v", func(st *PersistedState) { st.V = 0 }},
		{"short-pub", func(st *PersistedState) { st.PublicKey = "ab" }},
		{"device-empty-id", func(st *PersistedState) {
			st.Devices = []PersistedDevice{{ID: "", RID: strings.Repeat("ab", 32), PubKey: pubHex}}
		}},
		{"rid-not-hex", func(st *PersistedState) {
			st.Devices = []PersistedDevice{{ID: "phone", RID: "not-a-rid", PubKey: pubHex}}
		}},
		{"duplicate-device-id", func(st *PersistedState) {
			rid1 := strings.Repeat("ab", 32)
			rid2 := strings.Repeat("cd", 32)
			st.Devices = []PersistedDevice{{ID: "phone", RID: rid1, PubKey: pubHex}, {ID: "phone", RID: rid2, PubKey: pubHex}}
		}},
		{"enrolled-with-pending", func(st *PersistedState) {
			st.Pending = &PendingEnrollment{PublicKey: pubHex}
		}},
		{"missing-origin", func(st *PersistedState) { st.Origin = "" }},
		{"mismatched-keypair", func(st *PersistedState) {
			pub2, _ := newEd25519(t)
			st.PublicKey = hex.EncodeToString(pub2)
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			cfg := clientCfg(t, newFakeRV(t))
			c := NewClient(cfg)
			st := base
			row.mut(&st)
			writeState(t, c, st)
			before, _ := os.ReadFile(c.StatePath())
			err := c.Start(context.Background())
			requireClass(t, err, ClassCorruptIdentity)
			after, _ := os.ReadFile(c.StatePath())
			if !bytes.Equal(before, after) {
				t.Fatal("F5: semantic corruption was rewritten")
			}
		})
	}
}

func TestS5R3_F6_SysPathnameSwapDoesNotTouchReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "invite")
	secret := []byte("unrelated-replacement-file\n")
	if err := os.WriteFile(path, secret, 0o600); err != nil {
		t.Fatal(err)
	}
	sys := newFakeSys(os.Geteuid())
	sys.add(path, vectorInviteGrouped+"\n", 0o600, os.Geteuid(), true)
	cfg := clientCfg(t, newFakeRV(t))
	cfg.DataDir = dir
	cfg.InviteFile = path
	cfg.Sys = sys
	cfg.Remote = true
	ctx, cancel := ctxTO(t)
	defer cancel()
	c := NewClient(cfg)
	_ = c.Start(ctx)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("F6: replacement file removed: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Fatalf("F6: replacement file overwritten: %q", got)
	}
}

func TestS5R3_F7_InviteStdinBounded(t *testing.T) {
	cfg := clientCfg(t, newFakeRV(t))
	cfg.InviteStdin = true
	cfg.Stdin = neverEndingReader{}
	c := NewClient(cfg)
	done := make(chan error, 1)
	go func() { done <- c.Start(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("F7: endless invite stdin succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("F7: Start hung on unbounded invite stdin")
	}
}

func TestS5R3_F7_OversizedChallengeResponse(t *testing.T) {
	big := strings.Repeat("a", 1<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/enroll" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"handle":"ih_041061050R3GG28A","v":1}` + "\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"challenge":"`+big+`","v":1}`)
	}))
	t.Cleanup(srv.Close)
	cfg := r3ClientCfg(t, srv.URL, srv.Client())
	cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	done := make(chan error, 1)
	go func() { done <- NewClient(cfg).Start(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("F7: oversized challenge accepted")
		}
		if classOf(err) == ClassRevoked {
			t.Fatal("F7: oversized challenge persisted as revoked")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("F7: Start hung on oversized challenge body")
	}
}

func TestS5R3_F9_BackoffSchedulerCancel(t *testing.T) {
	sched := newManualSched()
	var waits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hello":
			helloOK(w)
		case "/v1/enroll":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"handle":"ih_041061050R3GG28A","v":1}` + "\n"))
		case "/v1/challenge":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"challenge":"` + strings.Repeat("22", 32) + `","v":1}` + "\n"))
		case "/v1/verify":
			w.WriteHeader(http.StatusNoContent)
		case "/v1/wait":
			waits.Add(1)
			http.Error(w, "fail", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	cfg := r3ClientCfg(t, srv.URL, srv.Client())
	cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	cfg.Scheduler = sched
	cfg.Clock = newClock(time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC))
	cfg.RNG = constRNG{v: 0.5}
	cfg.Backoff = BackoffConfig{Initial: 100 * time.Millisecond, Max: 1600 * time.Millisecond, Factor: 2, Jitter: 0.2, SuccessFor: 5 * time.Second}
	ctx, cancel := ctxTO(t)
	defer cancel()
	c, err := startClient(ctx, cfg)
	if err != nil {
		t.Fatalf("F9: start: %v", err)
	}
	if _, err := c.RegisterDevice(ctx, DeviceRecord{ID: "phone", PubKey: append([]byte(nil), testPhonePubKey...)}); err != nil {
		t.Fatalf("F9: register device: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for sched.Pending() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if sched.Pending() == 0 {
		t.Fatal("F9: no backoff retry was scheduled after wait failure")
	}
	n := waits.Load()
	if err := c.StopRendezvous(); err != nil {
		t.Fatal(err)
	}
	sched.FireAll()
	time.Sleep(20 * time.Millisecond)
	if waits.Load() > n+1 {
		t.Fatal("F9: wait retried after StopRendezvous")
	}
}

func TestS5R3_Authz_DisabledUnknownRevoked(t *testing.T) {
	c, _, d1, d2, _ := enrollTwoDevices(t)
	ctx, cancel := ctxTO(t)
	defer cancel()
	if err := c.RevokeDevice(ctx, d1.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.AttachChannel(d1.ID, &fakeChannel{}); err == nil {
		t.Fatal("attach revoked device succeeded")
	}
	if err := c.SetPending(d1.ID, PendingWork{Handshake: []byte("x")}); err == nil {
		t.Fatal("SetPending revoked succeeded")
	}
	if err := c.Deliver(ctx, d1.ID, []byte("x")); err == nil {
		t.Fatal("Deliver to revoked succeeded")
	}
	if err := c.DisableAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RegisterDevice(ctx, DeviceRecord{ID: "new"}); err == nil {
		t.Fatal("RegisterDevice while disabled succeeded")
	}
	if err := c.AddWaiter(&fakeWaiter{id: d2.RID}); err == nil {
		t.Fatal("AddWaiter while disabled succeeded")
	}
	_ = d2
}

func TestS5R3_DeliverLinearizableWithRevoke(t *testing.T) {
	c, _, d1, _, _ := enrollTwoDevices(t)
	ch := &blockingChannel{started: make(chan struct{}), release: make(chan struct{})}
	if err := c.AttachChannel(d1.ID, ch); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := ctxTO(t)
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- c.Deliver(ctx, d1.ID, []byte("payload")) }()
	select {
	case <-ch.started:
	case err := <-errc:
		t.Fatalf("Deliver returned before send boundary: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("Deliver never reached send boundary")
	}
	if err := c.RevokeDevice(ctx, d1.ID); err != nil {
		t.Fatal(err)
	}
	close(ch.release)
	if err := <-errc; err == nil {
		t.Fatal("Deliver succeeded after RevokeDevice returned")
	}
}

func TestS5R3_F3_LockHeldAfterSuccessfulStart(t *testing.T) {
	role := os.Getenv("SCIMUX_S5_R3_HELPER")
	if role == "A" || role == "B1" || role == "B2" {
		runR3LockHelper(role)
		return
	}
	fake := newFakeRV(t)
	dir := t.TempDir()
	invitePath := writeInviteFile(t, dir, vectorInviteGrouped, 0o600)
	started := filepath.Join(t.TempDir(), "started")
	release := filepath.Join(t.TempDir(), "release")
	startHelper := func(role string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestS5R3_F3_LockHeldAfterSuccessfulStart$", "-test.v=false", "-test.count=1")
		cmd.Env = append(os.Environ(),
			"SCIMUX_S5_R3_HELPER="+role,
			"SCIMUX_S5_STATE="+dir,
			"SCIMUX_S5_INVITE_FILE="+invitePath,
			"SCIMUX_S5_RV="+fake.URL(),
			"SCIMUX_S5_STARTED="+started,
			"SCIMUX_S5_RELEASE="+release,
		)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd
	}
	code := func(err error) int {
		if err == nil {
			return 0
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		return -1
	}
	a := startHelper("A")
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	doneA := make(chan error, 1)
	go func() { doneA <- a.Wait() }()
	announced := make(chan struct{})
	go func() {
		for {
			if _, err := os.Stat(started); err == nil {
				close(announced)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	select {
	case err := <-doneA:
		t.Fatalf("F3: process A exited before reporting Start complete: exit %d", code(err))
	case <-announced:
	case <-time.After(5 * time.Second):
		t.Fatal("F3: diagnostic timeout waiting for A Start")
	}
	b1 := startHelper("B1")
	errB := b1.Run()
	if code(errB) != 3 {
		t.Fatalf("F3: B while A held lock exit %d, want ClassStateLock (3)", code(errB))
	}
	if err := os.WriteFile(release, []byte("go\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := waitHelper(t, doneA, "process A"); code(err) != 0 {
		t.Fatalf("F3: A exit %d, want 0", code(err))
	}
	b2 := startHelper("B2")
	if err := b2.Run(); err != nil {
		t.Fatalf("F3: B after A close: %v", err)
	}
}

func runR3LockHelper(role string) {
	dir := os.Getenv("SCIMUX_S5_STATE")
	cfg := Config{
		DataDir:       dir,
		Remote:        true,
		Origin:        DefaultOrigin,
		InviteFile:    os.Getenv("SCIMUX_S5_INVITE_FILE"),
		RendezvousURL: os.Getenv("SCIMUX_S5_RV"),
	}
	if role != "A" {
		cfg.InviteFile = ""
	}
	c := NewClient(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := c.Start(ctx)
	if classOf(err) == ClassStateLock {
		os.Exit(3)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "r3 helper %s: %v\n", role, err)
		os.Exit(4)
	}
	if role == "A" {
		_ = os.WriteFile(os.Getenv("SCIMUX_S5_STARTED"), []byte("ok\n"), 0o600)
		release := os.Getenv("SCIMUX_S5_RELEASE")
		for {
			if _, err := os.Stat(release); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		_ = c.Close()
		os.Exit(0)
	}
	_ = c.Close()
	os.Exit(0)
}

type neverEndingReader struct{}

func (neverEndingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'A'
	}
	return len(p), nil
}

type blockingChannel struct {
	started, release chan struct{}
	closed           atomic.Bool
}

func (c *blockingChannel) Close() error { c.closed.Store(true); return nil }
func (c *blockingChannel) Closed() bool { return c.closed.Load() }
func (c *blockingChannel) Send(msg []byte) error {
	close(c.started)
	<-c.release
	if c.closed.Load() {
		return errors.New("closed")
	}
	return nil
}

type manualSched struct {
	mu      sync.Mutex
	pending []func()
}

func newManualSched() *manualSched { return &manualSched{} }

func (s *manualSched) After(d time.Duration, fn func()) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, fn)
	idx := len(s.pending) - 1
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if idx < len(s.pending) {
			s.pending[idx] = nil
		}
	}
}

func (s *manualSched) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, fn := range s.pending {
		if fn != nil {
			n++
		}
	}
	return n
}

func (s *manualSched) FireAll() {
	s.mu.Lock()
	fns := make([]func(), len(s.pending))
	copy(fns, s.pending)
	s.pending = nil
	s.mu.Unlock()
	for _, fn := range fns {
		if fn != nil {
			fn()
		}
	}
}

type strictAuthRV struct {
	URL          string
	Client       *http.Client
	MutateSigned func([]byte) []byte

	mu       sync.Mutex
	pub      ed25519.PublicKey
	handle   string
	chal     []byte
	verifies int
	verifyOK bool
	reject   string
	origin   string
}

func newStrictAuthRV(t *testing.T) *strictAuthRV {
	t.Helper()
	s := &strictAuthRV{origin: DefaultOrigin, chal: bytes.Repeat([]byte{0x22}, 32)}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	s.Client = srv.Client()
	return s
}

func (s *strictAuthRV) VerifyCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.verifies
}
func (s *strictAuthRV) LastVerifyOK() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.verifyOK
}
func (s *strictAuthRV) LastReject() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reject
}

func (s *strictAuthRV) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	switch r.URL.Path {
	case "/v1/hello":
		helloOK(w)
	case "/v1/enroll":
		var req struct {
			Code   string `json:"code"`
			PubKey string `json:"pubkey"`
		}
		_ = json.Unmarshal(body, &req)
		raw, _ := hex.DecodeString(req.PubKey)
		s.mu.Lock()
		s.pub = raw
		s.handle = "ih_041061050R3GG28A"
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		fmt.Fprintf(w, "{\"handle\":%q,\"v\":1}\n", "ih_041061050R3GG28A")
	case "/v1/challenge":
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		fmt.Fprintf(w, "{\"challenge\":%q,\"v\":1}\n", hex.EncodeToString(s.chal))
	case "/v1/verify":
		s.mu.Lock()
		s.verifies++
		s.mu.Unlock()
		var req struct {
			V         json.Number `json:"v"`
			Handle    string      `json:"handle"`
			Challenge string      `json:"challenge"`
			Sig       string      `json:"sig"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			s.rejectMsg(w, "malformed")
			return
		}
		if req.Sig == "" {
			s.rejectMsg(w, "handle-only")
			return
		}
		sig, err := hex.DecodeString(req.Sig)
		if err != nil || len(sig) != ed25519.SignatureSize {
			s.rejectMsg(w, "bad-sig")
			return
		}
		chal, err := hex.DecodeString(req.Challenge)
		if err != nil || len(chal) != 32 {
			s.rejectMsg(w, "bad-challenge")
			return
		}
		msg := authMessage(s.origin, 1, "/v1/verify", req.Handle, chal)
		if s.MutateSigned != nil {
			if mut := s.MutateSigned(msg); mut == nil {
				s.rejectMsg(w, "handle-only")
				return
			} else {
				msg = mut
			}
		}
		s.mu.Lock()
		pub := s.pub
		s.mu.Unlock()
		if !ed25519.Verify(pub, msg, sig) {
			s.rejectMsg(w, "verify-failed")
			return
		}
		s.mu.Lock()
		s.verifyOK = true
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func (s *strictAuthRV) rejectMsg(w http.ResponseWriter, why string) {
	s.mu.Lock()
	s.reject = why
	s.mu.Unlock()
	w.WriteHeader(http.StatusNotFound)
	io.WriteString(w, "not found\n")
}

func authMessage(origin string, version int, route, handle string, challenge []byte) []byte {
	var b []byte
	b = append(b, []byte("scimux-rv/auth/v1")...)
	b = append(b, 0)
	b = append(b, []byte(origin)...)
	b = append(b, 0)
	b = append(b, []byte(strconv.Itoa(version))...)
	b = append(b, 0)
	b = append(b, []byte(route)...)
	b = append(b, 0)
	b = append(b, []byte(handle)...)
	b = append(b, 0)
	b = append(b, challenge...)
	return b
}
