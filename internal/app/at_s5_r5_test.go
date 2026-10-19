package app

import (
	"bytes"
	"context"
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

	"github.com/scimux/scimux/internal/remote"
)

func TestS5R5_F2_HostedStatusWaitChallengeTransitions(t *testing.T) {
	srv := newTransitionRV(t)
	data := t.TempDir()
	writeEnrolledStateWithDevice(t, data)
	before := readRemoteIdentity(t, data)
	sched := newHostedRetryScheduler()
	cmd := remoteCmd(t, data, srv.URL, srv.Client)
	cmd.Config.Scheduler = sched
	cmd.Config.Backoff = remote.BackoffConfig{Initial: 20 * time.Millisecond, Max: 200 * time.Millisecond, Factor: 2, SuccessFor: 5 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := cmd.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Client() != nil {
			_ = cmd.Client().Close()
		}
	})

	w1 := srv.WaitHeld(t, 3*time.Second)
	assertHostedStatus(t, cmd, "enrolled")
	srv.Release(w1, waitReleaseNoContent)
	retry := sched.next(t)
	sched.fire(t, retry)

	w2 := srv.WaitHeld(t, 3*time.Second)
	srv.Release(w2, waitReleaseHangup)
	retry = sched.next(t)
	assertHostedStatus(t, cmd, "unavailable")
	sched.fire(t, retry)

	w3 := srv.WaitHeld(t, 3*time.Second)
	assertHostedStatus(t, cmd, "unavailable")
	srv.Release(w3, waitReleaseNoContent)
	retry = sched.next(t)
	assertHostedStatus(t, cmd, "enrolled")
	sched.fire(t, retry)

	w4 := srv.WaitHeld(t, 3*time.Second)
	assertHostedStatus(t, cmd, "enrolled")
	srv.SetRevoke(true)
	srv.Release(w4, waitReleaseRevoked)
	retry = sched.next(t)
	nWait := srv.WaitArrived()
	for i := 0; i < 3; i++ {
		nChal := srv.ChallengeCount()
		sched.fire(t, retry)
		retry = sched.next(t)
		assertHostedStatus(t, cmd, "unavailable")
		if srv.ChallengeCount() != nChal+1 || srv.WaitArrived() != nWait {
			t.Fatal("rejected challenge did not block authentication and schedule a fresh attempt")
		}
		if !bytes.Equal(before, readRemoteIdentity(t, data)) {
			t.Fatal("rejection rewrote enrollment")
		}
	}
	// Observe failures before allowing recovery.
	srv.SetRevoke(false)
	sched.fire(t, retry)
	recovered := srv.WaitHeld(t, 3*time.Second)
	if recovered.Challenge == w4.Challenge {
		t.Fatal("recovery reused the rejected wait's challenge")
	}
	assertHostedWaitSignature(t, before, recovered.Challenge, recovered.Sig)
	assertHostedStatus(t, cmd, "unavailable")
	srv.Release(recovered, waitReleaseNoContent)
	_ = sched.next(t)
	assertHostedStatus(t, cmd, "enrolled")
	if !bytes.Equal(before, readRemoteIdentity(t, data)) {
		t.Fatal("recovery changed identity or devices")
	}
}

func waitHosted(t *testing.T, cmd *Command, want string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	var last string
	for time.Now().Before(deadline) {
		h := cmd.Handler()
		if h != nil {
			rec := routeRequest(h, http.MethodGet, "/api/state", "", false)
			if rec.Code == http.StatusOK {
				var payload map[string]any
				if json.Unmarshal(rec.Body.Bytes(), &payload) == nil {
					if remoteObj, ok := payload["remote"].(map[string]any); ok {
						last, _ = remoteObj["status"].(string)
						if last == want {
							return
						}
					}
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("F2: remote.status = %q, want %q", last, want)
}

func writeEnrolledStateWithDevice(t *testing.T, data string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := remote.NewClient(remote.Config{DataDir: data})
	if err := os.MkdirAll(c.PrivateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	st := remote.PersistedState{
		V:          1,
		Status:     remote.StateEnrolled,
		Handle:     "ih_041061050R3GG28A",
		PublicKey:  hex.EncodeToString(pub),
		PrivateKey: hex.EncodeToString(priv),
		Origin:     remote.DefaultOrigin,
		Devices: []remote.PersistedDevice{{
			ID:     "phone",
			RID:    strings.Repeat("ab", 32),
			PubKey: hex.EncodeToString(bytes.Repeat([]byte{0x11}, ed25519.PublicKeySize)),
		}},
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.StatePath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

const (
	waitReleaseNoContent = 1
	waitReleaseHangup    = 2
	waitReleaseRevoked   = 3
)

type heldWait struct {
	Challenge string
	Sig       string

	w       http.ResponseWriter
	r       *http.Request
	release chan int
	done    chan struct{}
}

type transitionRV struct {
	URL    string
	Client *http.Client

	mu         sync.Mutex
	held       chan *heldWait
	revoke     atomic.Bool
	challenges int
	waits      int
}

func newTransitionRV(t *testing.T) *transitionRV {
	t.Helper()
	s := &transitionRV{held: make(chan *heldWait, 8)}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(func() {
		s.SetRevoke(true)
		for {
			select {
			case h := <-s.held:
				s.Release(h, waitReleaseRevoked)
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

func (s *transitionRV) SetRevoke(v bool) { s.revoke.Store(v) }

func (s *transitionRV) ChallengeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.challenges
}

func (s *transitionRV) WaitArrived() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waits
}

func (s *transitionRV) WaitHeld(t *testing.T, d time.Duration) *heldWait {
	t.Helper()
	select {
	case h := <-s.held:
		return h
	case <-time.After(d):
		t.Fatalf("F2: no wait held within %s", d)
		return nil
	}
}

func (s *transitionRV) Release(h *heldWait, how int) {
	select {
	case h.release <- how:
	case <-time.After(2 * time.Second):
	}
	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
	}
}

func (s *transitionRV) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/challenge":
		s.mu.Lock()
		s.challenges++
		n := s.challenges
		s.mu.Unlock()
		if s.revoke.Load() {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, "not found\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		fmt.Fprintf(w, `{"challenge":"%s","v":1}`+"\n", hex.EncodeToString(bytes.Repeat([]byte{byte(n)}, 32)))
	case "/v1/verify":
		w.WriteHeader(http.StatusNoContent)
	case "/v1/wait":
		s.mu.Lock()
		s.waits++
		s.mu.Unlock()
		if s.revoke.Load() {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, "not found\n")
			return
		}
		var req struct{ Challenge, Sig string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		h := &heldWait{w: w, r: r, Challenge: req.Challenge, Sig: req.Sig, release: make(chan int, 1), done: make(chan struct{})}
		s.held <- h
		how := waitReleaseNoContent
		select {
		case how = <-h.release:
		case <-r.Context().Done():
			close(h.done)
			return
		}
		defer close(h.done)
		switch how {
		case waitReleaseHangup:
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
		case waitReleaseRevoked:
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, "not found\n")
		default:
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("X-Rv-Challenge", strings.Repeat("33", 32))
			w.WriteHeader(http.StatusNoContent)
		}
	default:
		http.NotFound(w, r)
	}
}

// Each event is installed after the client has processed a wait result. Tests
// fire only the selected retry, leaving other device loops parked.
type hostedRetry struct {
	delay     time.Duration
	fire      func()
	cancelled bool
}
type hostedRetryScheduler struct {
	mu    sync.Mutex
	ready chan *hostedRetry
}

func newHostedRetryScheduler() *hostedRetryScheduler {
	return &hostedRetryScheduler{ready: make(chan *hostedRetry, 32)}
}
func (s *hostedRetryScheduler) After(delay time.Duration, fn func()) func() {
	retry := &hostedRetry{delay: delay, fire: fn}
	s.ready <- retry
	return func() { s.mu.Lock(); retry.cancelled = true; s.mu.Unlock() }
}
func (s *hostedRetryScheduler) next(t *testing.T) *hostedRetry {
	t.Helper()
	select {
	case retry := <-s.ready:
		return retry
	case <-time.After(3 * time.Second):
		t.Fatal("device loop did not schedule a retry")
		return nil
	}
}
func (s *hostedRetryScheduler) fire(t *testing.T, retry *hostedRetry) {
	t.Helper()
	s.mu.Lock()
	cancelled := retry.cancelled
	s.mu.Unlock()
	if cancelled {
		t.Fatal("device retry was cancelled")
	}
	if retry.delay <= 0 || retry.delay > 200*time.Millisecond {
		t.Fatalf("retry delay = %s, outside configured backoff", retry.delay)
	}
	retry.fire()
}

func assertHostedWaitSignature(t *testing.T, identity []byte, challenge, signature string) {
	t.Helper()
	var st remote.PersistedState
	if err := json.Unmarshal(identity, &st); err != nil {
		t.Fatal(err)
	}
	pub, err := hex.DecodeString(st.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	chal, err := hex.DecodeString(challenge)
	if err != nil || len(chal) != 32 {
		t.Fatal("malformed recovery challenge")
	}
	sig, err := hex.DecodeString(signature)
	msg := append([]byte("scimux-rv/auth/v1\x00"+st.Origin+"\x001\x00/v1/wait\x00"+st.Handle+"\x00"), chal...)
	if err != nil || !ed25519.Verify(pub, msg, sig) {
		t.Fatal("recovered wait was not signed over the fresh challenge")
	}
}
