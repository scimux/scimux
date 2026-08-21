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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestS5R5_F1_ConcurrentRIDWaitsBarrier(t *testing.T) {
	srv := newBarrierWaitRV(t, 2)
	cfg := r3ClientCfg(t, srv.URL, srv.Client)
	cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	sched := newDelaySched()
	cfg.Scheduler = sched
	cfg.Backoff = BackoffConfig{Initial: 50 * time.Millisecond, Max: 800 * time.Millisecond, Factor: 2, Jitter: 0.2, SuccessFor: 5 * time.Second}
	ctx, cancel := ctxTO(t)
	defer cancel()
	c, err := startClient(ctx, cfg)
	if err != nil {
		t.Fatalf("F1: start: %v", err)
	}
	defer func() { srv.Release(); _ = c.Close() }()

	d1, err := c.RegisterDevice(ctx, DeviceRecord{ID: "phone", PubKey: append([]byte(nil), testPhonePubKey...)})
	if err != nil {
		t.Fatal(err)
	}
	d2, err := c.RegisterDevice(ctx, DeviceRecord{ID: "tablet", PubKey: append([]byte(nil), testTabletPubKey...)})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-srv.BothActive():
	case <-time.After(5 * time.Second):
		t.Fatalf("F1: both RID waits were not active concurrently; in-flight=%d recs=%v", srv.InFlight(), srv.IDs())
	}

	ids := srv.IDs()
	if len(ids) != 2 {
		t.Fatalf("F1: concurrent waits = %v, want 2", ids)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	if !seen[d1.RID] || !seen[d2.RID] {
		t.Fatalf("F1: concurrent wait ids = %v, want %q and %q", ids, d1.RID, d2.RID)
	}
	chals := srv.Challenges()
	if len(chals) != 2 || chals[0] == "" || chals[0] == chals[1] {
		t.Fatalf("F1: concurrent waits must present independent challenges, got %v", chals)
	}
	for _, rec := range srv.Held() {
		if rec.Reject != "" {
			t.Fatalf("F1: held wait rejected: %+v", rec)
		}
		if rec.Sig == "" || rec.Challenge == "" {
			t.Fatalf("F1: held wait missing auth: %+v", rec)
		}
		if rec.MaxMS != WaitDefaultMaxMS {
			t.Fatalf("F1: max_ms = %d, want protocol default %d", rec.MaxMS, WaitDefaultMaxMS)
		}
	}

	srv.Release()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if srv.ReleasedCount() >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if srv.ReleasedCount() < 2 {
		t.Fatalf("F1: released %d waits, want 2", srv.ReleasedCount())
	}
}

func TestS5R5_F1_CancelTerminatesEveryWaitAndRetry(t *testing.T) {
	srv := newBarrierWaitRV(t, 2)
	cfg := r3ClientCfg(t, srv.URL, srv.Client)
	cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	sched := newDelaySched()
	cfg.Scheduler = sched
	cfg.Backoff = BackoffConfig{Initial: 50 * time.Millisecond, Max: 800 * time.Millisecond, Factor: 2, Jitter: 0.2, SuccessFor: 5 * time.Second}
	ctx, cancel := ctxTO(t)
	defer cancel()
	c, err := startClient(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { srv.Release(); _ = c.Close() }()
	if _, err := c.RegisterDevice(ctx, DeviceRecord{ID: "phone", PubKey: append([]byte(nil), testPhonePubKey...)}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RegisterDevice(ctx, DeviceRecord{ID: "tablet", PubKey: append([]byte(nil), testTabletPubKey...)}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-srv.BothActive():
	case <-time.After(5 * time.Second):
		t.Fatal("F1: cancellation test never saw both waits")
	}
	n := srv.ArrivedCount()
	if err := c.StopRendezvous(); err != nil {
		t.Fatal(err)
	}
	sched.FireAll()
	time.Sleep(50 * time.Millisecond)
	if srv.ArrivedCount() > n {
		t.Fatalf("F1: wait retried after StopRendezvous: arrived %d -> %d", n, srv.ArrivedCount())
	}
	if srv.InFlight() != 0 {
		t.Fatalf("F1: %d waits still in flight after StopRendezvous", srv.InFlight())
	}
}

func TestS5R5_F3_SameClientLockOwnership(t *testing.T) {
	role := os.Getenv("SCIMUX_S5_R5_HELPER")
	if role != "" {
		runR5LockHelper(role)
		return
	}

	dir := t.TempDir()
	fake := newFakeRV(t)
	pub, priv := newEd25519(t)
	writeState(t, NewClient(Config{DataDir: dir}), PersistedState{
		V:          1,
		Status:     StateEnrolled,
		Handle:     "ih_041061050R3GG28A",
		PublicKey:  hex.EncodeToString(pub),
		PrivateKey: hex.EncodeToString(priv),
		Origin:     DefaultOrigin,
	})

	barrier := t.TempDir()
	startHelper := func(role string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestS5R5_F3_SameClientLockOwnership$", "-test.v=false", "-test.count=1")
		cmd.Env = append(os.Environ(),
			"SCIMUX_S5_R5_HELPER="+role,
			"SCIMUX_S5_STATE="+dir,
			"SCIMUX_S5_RV="+fake.URL(),
			"SCIMUX_S5_BARRIER="+barrier,
		)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd
	}
	a := startHelper("A")
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	doneA := make(chan error, 1)
	go func() { doneA <- a.Wait() }()

	waitFile(t, filepath.Join(barrier, "a-persist"), 8*time.Second)
	waitFile(t, filepath.Join(barrier, "b-held"), 8*time.Second)

	assertHelperStateLock(t, dir, fake.URL())
	if err := os.WriteFile(filepath.Join(barrier, "a-release"), []byte("go\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFile(t, filepath.Join(barrier, "b-persist"), 8*time.Second)
	assertHelperStateLock(t, dir, fake.URL())
	if err := os.WriteFile(filepath.Join(barrier, "b-release"), []byte("go\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-doneA:
		if err != nil {
			t.Fatalf("F3: overlapping mutations: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("F3: overlapping mutations did not finish")
	}
}

func waitFile(t *testing.T, path string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("F3: timeout waiting for %s", path)
}

func TestS5R5_F4_ReplacementAtFinalUnlinkPreserved(t *testing.T) {
	ctx, cancel := ctxTO(t)
	defer cancel()
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	path := writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
	cfg.InviteFile = path
	replacement := []byte("final-boundary-replacement\n")
	cfg.BeforeInviteUnlinkFinal = func() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Errorf("replace remove: %v", err)
		}
		if err := os.WriteFile(path, replacement, 0o600); err != nil {
			t.Errorf("replace write: %v", err)
		}
	}
	_, err := startClient(ctx, cfg)
	if err != nil {
		t.Fatalf("F4: enroll: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("F4: replacement at final Unlinkat boundary was deleted: %v", err)
	}
	if !bytes.Equal(got, replacement) {
		t.Fatalf("F4: pathname after Unlinkat = %q, want replacement %q", got, replacement)
	}
}

func TestS5R5_F5_EmptyDevicePublicKeyRejected(t *testing.T) {
	ctx, cancel := ctxTO(t)
	defer cancel()
	c, _, _, _, _ := enrollTwoDevices(t)

	t.Run("empty-key-mutation", func(t *testing.T) {
		assertUnchanged(t, c, func() error {
			_, err := c.RegisterDevice(ctx, DeviceRecord{ID: "watch"})
			return err
		}, ClassUnauthorized)
	})
	t.Run("nil-key-mutation", func(t *testing.T) {
		assertUnchanged(t, c, func() error {
			_, err := c.RegisterDevice(ctx, DeviceRecord{ID: "watch2", PubKey: nil})
			return err
		}, ClassUnauthorized)
	})
	t.Run("persisted-empty-key", func(t *testing.T) {
		cfg := clientCfg(t, newFakeRV(t))
		c2 := NewClient(cfg)
		pub, priv := newEd25519(t)
		st := PersistedState{
			V:          1,
			Status:     StateEnrolled,
			Handle:     "ih_041061050R3GG28A",
			PublicKey:  hex.EncodeToString(pub),
			PrivateKey: hex.EncodeToString(priv),
			Origin:     DefaultOrigin,
			Devices: []PersistedDevice{{
				ID:  "phone",
				RID: strings.Repeat("ab", 32),
			}},
		}
		writeState(t, c2, st)
		before, err := os.ReadFile(c2.StatePath())
		if err != nil {
			t.Fatal(err)
		}
		err = c2.Start(ctx)
		requireClass(t, err, ClassCorruptIdentity)
		after, err := os.ReadFile(c2.StatePath())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatalf("F5: persisted empty device key rewrote disk bytes")
		}
	})
}

func TestS5R5_F6_RestoreEchoFailureNotFalseSuccess(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	term := &ownerTerminal{f: w}
	err1 := term.RestoreEcho()
	if err1 == nil {
		t.Fatal("F6: RestoreEcho succeeded on a pipe")
	}
	if term.restored {
		t.Fatal("F6: RestoreEcho marked restored after ioctl failure")
	}
	err2 := term.RestoreEcho()
	if err2 == nil {
		t.Fatal("F6: second RestoreEcho returned nil after a failed ioctl (false success)")
	}
}

func TestS5R5_F7_ContentTypeSuffixSmuggling(t *testing.T) {
	handle := "ih_041061050R3GG28A"
	okBody := fmt.Sprintf("{\"handle\":%q,\"v\":1}\n", handle)
	chalBody := `{"challenge":"` + strings.Repeat("22", 32) + `","v":1}` + "\n"
	rows := []struct {
		name string
		path string
		ct   string
		body string
	}{
		{"enroll-json-evil", "/v1/enroll", "application/json-evil", okBody},
		{"challenge-json-evil", "/v1/challenge", "application/json-evil", chalBody},
		{"wait-octet-evil", "/v1/wait", "application/octet-stream-evil", "sealed"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/enroll":
					if row.path == "/v1/enroll" {
						w.Header().Set("Content-Type", row.ct)
						io.WriteString(w, row.body)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, okBody)
				case "/v1/challenge":
					if row.path == "/v1/challenge" {
						w.Header().Set("Content-Type", row.ct)
						io.WriteString(w, row.body)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, chalBody)
				case "/v1/verify":
					w.WriteHeader(http.StatusNoContent)
				case "/v1/wait":
					if row.path == "/v1/wait" {
						w.Header().Set("Content-Type", row.ct)
						w.Header().Set("X-Content-Type-Options", "nosniff")
						w.Header().Set("X-Rv-Challenge", strings.Repeat("33", 32))
						w.WriteHeader(http.StatusOK)
						io.WriteString(w, row.body)
						return
					}
					w.Header().Set("X-Content-Type-Options", "nosniff")
					w.Header().Set("X-Rv-Challenge", strings.Repeat("33", 32))
					w.WriteHeader(http.StatusNoContent)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			cfg := r3ClientCfg(t, srv.URL, srv.Client())
			cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
			if row.path == "/v1/wait" {
				sched := newDelaySched()
				cfg.Scheduler = sched
				cfg.Backoff = BackoffConfig{Initial: 50 * time.Millisecond, Max: 800 * time.Millisecond, Factor: 2, Jitter: 0.2, SuccessFor: 5 * time.Second}
			}
			c := NewClient(cfg)
			err := c.Start(context.Background())
			if row.path != "/v1/wait" {
				if err == nil {
					t.Fatalf("F7: %s accepted", row.name)
				}
				st, _ := c.State()
				if st == StateRevoked || classOf(err) == ClassRevoked {
					t.Fatalf("F7: %s persisted/classified revoked", row.name)
				}
				if row.path == "/v1/enroll" && st == StateEnrolled {
					t.Fatalf("F7: %s persisted enrolled", row.name)
				}
				return
			}
			if err != nil {
				t.Fatalf("F7: wait-row start: %v", err)
			}
			defer c.Close()
			if _, err := c.RegisterDevice(context.Background(), DeviceRecord{ID: "phone", PubKey: append([]byte(nil), testPhonePubKey...)}); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if c.HostedStatus() == "unavailable" {
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
			if c.HostedStatus() == "enrolled" {
				t.Fatal("F7: wait octet-stream-evil treated as a successful wait")
			}
			st, _ := c.State()
			if st == StateRevoked {
				t.Fatal("F7: wait suffix-smuggling revoked the installation")
			}
		})
	}
}

func assertHelperStateLock(t *testing.T, dir, rv string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestS5R5_F3_SameClientLockOwnership$", "-test.v=false", "-test.count=1")
	cmd.Env = append(os.Environ(),
		"SCIMUX_S5_R5_HELPER=C-register",
		"SCIMUX_S5_STATE="+dir,
		"SCIMUX_S5_RV="+rv,
	)
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	if code != 3 {
		t.Fatalf("F3: separate process exit %d, want ClassStateLock (3)", code)
	}
}

func runR5LockHelper(role string) {
	dir := os.Getenv("SCIMUX_S5_STATE")
	barrier := os.Getenv("SCIMUX_S5_BARRIER")
	cfg := Config{
		DataDir:       dir,
		Remote:        true,
		Origin:        DefaultOrigin,
		RendezvousURL: os.Getenv("SCIMUX_S5_RV"),
	}
	c := NewClient(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	switch role {
	case "A":
		runR5OverlapA(c, ctx, barrier)
		return
	case "C-register":
		_, err := c.RegisterDevice(ctx, DeviceRecord{ID: "other", PubKey: bytes.Repeat([]byte{0x33}, ed25519.PublicKeySize)})
		if classOf(err) == ClassStateLock {
			os.Exit(3)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "r5 helper C: %v\n", err)
			os.Exit(4)
		}
		os.Exit(0)
	default:
		os.Exit(4)
	}
}

func runR5OverlapA(c *Client, ctx context.Context, barrier string) {
	var holds, persistN atomic.Int32
	c.cfg.OnLockHeld = func() {
		if holds.Add(1) == 2 {
			_ = os.WriteFile(filepath.Join(barrier, "b-held"), []byte("ok\n"), 0o600)
		}
	}
	c.cfg.BeforePersist = func() {
		switch persistN.Add(1) {
		case 1:
			_ = os.WriteFile(filepath.Join(barrier, "a-persist"), []byte("ok\n"), 0o600)
			waitPath(filepath.Join(barrier, "b-held"))
			waitPath(filepath.Join(barrier, "a-release"))
		case 2:
			_ = os.WriteFile(filepath.Join(barrier, "b-persist"), []byte("ok\n"), 0o600)
			waitPath(filepath.Join(barrier, "b-release"))
		}
	}
	errA := make(chan error, 1)
	go func() {
		_, err := c.RegisterDevice(ctx, DeviceRecord{ID: "phone", PubKey: append([]byte(nil), testPhonePubKey...)})
		errA <- err
	}()
	waitPath(filepath.Join(barrier, "a-persist"))
	errB := make(chan error, 1)
	go func() {
		_, err := c.RegisterDevice(ctx, DeviceRecord{ID: "tablet", PubKey: append([]byte(nil), testTabletPubKey...)})
		errB <- err
	}()
	if err := <-errA; err != nil {
		fmt.Fprintf(os.Stderr, "r5 helper A mutation A: %v\n", err)
		os.Exit(4)
	}
	if err := <-errB; err != nil {
		fmt.Fprintf(os.Stderr, "r5 helper A mutation B: %v\n", err)
		os.Exit(4)
	}
	os.Exit(0)
}

func waitPath(path string) {
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type barrierWait struct {
	rec     waitRec
	release chan struct{}
	req     *http.Request
}

type barrierWaitRV struct {
	URL    string
	Client *http.Client

	need int

	mu       sync.Mutex
	pub      ed25519.PublicKey
	handle   string
	chalN    int
	held     []*barrierWait
	ids      []string
	chals    []string
	released int
	arrived  int
	both     chan struct{}
	bothOnce sync.Once
	allowGo  chan struct{}
	goOnce   sync.Once
}

func newBarrierWaitRV(t *testing.T, need int) *barrierWaitRV {
	t.Helper()
	s := &barrierWaitRV{
		need:    need,
		both:    make(chan struct{}),
		allowGo: make(chan struct{}),
	}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(func() {
		s.Release()
		srv.Close()
	})
	s.URL = srv.URL
	s.Client = srv.Client()
	return s
}

func (s *barrierWaitRV) BothActive() <-chan struct{} { return s.both }
func (s *barrierWaitRV) Release() {
	s.goOnce.Do(func() { close(s.allowGo) })
}

func (s *barrierWaitRV) InFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, h := range s.held {
		if h != nil {
			n++
		}
	}
	return n
}

func (s *barrierWaitRV) IDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]string(nil), s.ids...)
	return out
}

func (s *barrierWaitRV) Challenges() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.chals...)
}

func (s *barrierWaitRV) Held() []waitRec {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []waitRec
	for _, h := range s.held {
		if h != nil {
			out = append(out, h.rec)
		}
	}
	return out
}

func (s *barrierWaitRV) ReleasedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.released
}

func (s *barrierWaitRV) ArrivedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.arrived
}

func (s *barrierWaitRV) serve(w http.ResponseWriter, r *http.Request) {
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
		s.handle = "ih_041061050R3GG28A"
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		fmt.Fprintf(w, "{\"handle\":%q,\"v\":1}\n", "ih_041061050R3GG28A")
	case "/v1/challenge":
		s.mu.Lock()
		s.chalN++
		n := s.chalN
		s.mu.Unlock()
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

func (s *barrierWaitRV) handleWait(w http.ResponseWriter, r *http.Request, body []byte) {
	rec := waitRec{Body: string(body)}
	var req struct {
		V         json.Number `json:"v"`
		Handle    string      `json:"handle"`
		Challenge string      `json:"challenge"`
		Sig       string      `json:"sig"`
		ID        string      `json:"id"`
		MaxMS     json.Number `json:"max_ms"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		rec.Reject = "malformed"
		http.Error(w, "malformed", http.StatusBadRequest)
		return
	}
	rec.ID = req.ID
	rec.Challenge = req.Challenge
	rec.Sig = req.Sig
	if v, err := req.MaxMS.Int64(); err == nil {
		rec.MaxMS = int(v)
	}
	if req.Sig == "" || req.Challenge == "" || req.ID == "" {
		rec.Reject = "incomplete"
	}
	chal, err := hex.DecodeString(req.Challenge)
	if err != nil || len(chal) != 32 {
		rec.Reject = "bad-challenge"
	}
	sig, err := hex.DecodeString(req.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		rec.Reject = "bad-sig"
	}
	s.mu.Lock()
	pub := s.pub
	handle := s.handle
	s.mu.Unlock()
	if rec.Reject == "" {
		msg := buildAuthMessage(DefaultOrigin, ProtocolVersion, "/v1/wait", handle, chal)
		if !ed25519.Verify(pub, msg, sig) {
			rec.Reject = "verify-failed"
		}
	}

	h := &barrierWait{rec: rec, release: make(chan struct{}), req: r}
	s.mu.Lock()
	s.arrived++
	s.held = append(s.held, h)
	s.ids = append(s.ids, rec.ID)
	s.chals = append(s.chals, rec.Challenge)
	n := 0
	for _, x := range s.held {
		if x != nil {
			n++
		}
	}
	if n >= s.need {
		s.bothOnce.Do(func() { close(s.both) })
	}
	s.mu.Unlock()

	select {
	case <-s.allowGo:
	case <-r.Context().Done():
		s.dropHeld(h)
		return
	}

	next := make([]byte, 32)
	copy(next, chal)
	next[31]++
	s.mu.Lock()
	s.released++
	s.dropHeldLocked(h)
	s.mu.Unlock()
	if rec.Reject != "" {
		http.Error(w, rec.Reject, http.StatusNotFound)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Rv-Challenge", hex.EncodeToString(next))
	w.WriteHeader(http.StatusNoContent)
}

func (s *barrierWaitRV) dropHeld(h *barrierWait) {
	s.mu.Lock()
	s.dropHeldLocked(h)
	s.mu.Unlock()
}

func (s *barrierWaitRV) dropHeldLocked(h *barrierWait) {
	for i, x := range s.held {
		if x == h {
			s.held[i] = nil
		}
	}
}
