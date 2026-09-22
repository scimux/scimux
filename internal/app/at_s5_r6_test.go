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
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/remote"
)

func TestS5R6_F1_HostedWait404ThenFreshChallengeStaysEnrolled(t *testing.T) {
	srv := newR6TransitionRV(t)
	data := t.TempDir()
	writeEnrolledStateWithDevice(t, data)
	cmd := &Command{
		ExperimentalRemote: true,
		Args:               []string{"scimux", "--remote", "-data", data, "-addr", "127.0.0.1:0"},
		Stdout:             io.Discard,
		Stderr:             new(bytes.Buffer),
		Home:               t.TempDir(),
		Config: remote.Config{
			DataDir:       data,
			RendezvousURL: srv.URL,
			HTTPClient:    srv.Client,
			NewTerminal:   noTestTerminal,
			Backoff: remote.BackoffConfig{
				Initial:    20 * time.Millisecond,
				Max:        200 * time.Millisecond,
				Factor:     2,
				Jitter:     0,
				SuccessFor: 5 * time.Second,
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := cmd.Run(ctx); err != nil {
		t.Fatalf("F1: run: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Client() != nil {
			_ = cmd.Client().Close()
		}
	})

	w1 := srv.WaitHeld(t, 5*time.Second)
	nChal := srv.ChallengeCount()
	assertHostedStatus(t, cmd, "enrolled")
	srv.Release(w1, waitReleaseRevoked)

	w2 := srv.WaitHeld(t, 5*time.Second)
	assertPersistedRemoteStatus(t, data, remote.StateEnrolled)
	if srv.ChallengeCount() <= nChal {
		t.Fatalf("F1: ChallengeCount did not increase after wait 404: %d -> %d", nChal, srv.ChallengeCount())
	}
	if w2.Challenge == "" || w2.Challenge == w1.Challenge {
		t.Fatalf("F1: resumed wait reused rejected challenge %q", w1.Challenge)
	}
	srv.Release(w2, waitReleaseNoContent)
	waitHosted(t, cmd, "enrolled", 3*time.Second)
}

func TestS5R6_F1_HostedWait404ThenChallengeRevokeStopsEveryRID(t *testing.T) {
	srv := newR6TransitionRV(t)
	data := t.TempDir()
	writeEnrolledStateWithTwoDevices(t, data)
	cmd := &Command{
		ExperimentalRemote: true,
		Args:               []string{"scimux", "--remote", "-data", data, "-addr", "127.0.0.1:0"},
		Stdout:             io.Discard,
		Stderr:             new(bytes.Buffer),
		Home:               t.TempDir(),
		Config: remote.Config{
			DataDir:       data,
			RendezvousURL: srv.URL,
			HTTPClient:    srv.Client,
			NewTerminal:   noTestTerminal,
			Backoff: remote.BackoffConfig{
				Initial:    20 * time.Millisecond,
				Max:        200 * time.Millisecond,
				Factor:     2,
				Jitter:     0,
				SuccessFor: 5 * time.Second,
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := cmd.Run(ctx); err != nil {
		t.Fatalf("F1: run: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Client() != nil {
			_ = cmd.Client().Close()
		}
	})

	w1 := srv.WaitHeld(t, 5*time.Second)
	w2 := srv.WaitHeld(t, 5*time.Second)
	nChal := srv.ChallengeCount()
	nWait := srv.WaitArrived()
	assertHostedStatus(t, cmd, "enrolled")
	srv.SetChallengeRevoke(true)
	srv.Release(w1, waitReleaseRevoked)
	srv.Release(w2, waitReleaseRevoked)
	waitHosted(t, cmd, "revoked", 5*time.Second)
	assertPersistedRemoteStatus(t, data, remote.StateRevoked)
	if srv.ChallengeCount() <= nChal {
		t.Fatalf("F1: wait rejection did not look up a challenge: %d -> %d", nChal, srv.ChallengeCount())
	}
	time.Sleep(80 * time.Millisecond)
	if srv.WaitArrived() > nWait+2 {
		t.Fatalf("F1: still reconnecting after challenge revoke: waits %d -> %d", nWait, srv.WaitArrived())
	}
}

func writeEnrolledStateWithTwoDevices(t *testing.T, data string) {
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
		Devices: []remote.PersistedDevice{
			{ID: "phone", RID: strings.Repeat("ab", 32), PubKey: hex.EncodeToString(bytes.Repeat([]byte{0x11}, ed25519.PublicKeySize))},
			{ID: "tablet", RID: strings.Repeat("cd", 32), PubKey: hex.EncodeToString(bytes.Repeat([]byte{0x22}, ed25519.PublicKeySize))},
		},
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.StatePath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertPersistedRemoteStatus(t *testing.T, data string, want remote.EnrollmentState) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(data, remote.PrivateDirName, remote.StateFileName))
	if err != nil {
		t.Fatal(err)
	}
	var st remote.PersistedState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	if st.Status != want {
		t.Fatalf("persisted status = %q, want %s; bytes=%s", st.Status, want, raw)
	}
}

type r6HeldWait struct {
	w         http.ResponseWriter
	r         *http.Request
	release   chan int
	done      chan struct{}
	Challenge string
}

type r6TransitionRV struct {
	URL    string
	Client *http.Client

	mu              sync.Mutex
	held            chan *r6HeldWait
	challengeRevoke atomic.Bool
	challenges      int
	waits           int
}

func newR6TransitionRV(t *testing.T) *r6TransitionRV {
	t.Helper()
	s := &r6TransitionRV{held: make(chan *r6HeldWait, 8)}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(func() {
		s.SetChallengeRevoke(true)
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

func (s *r6TransitionRV) SetChallengeRevoke(v bool) { s.challengeRevoke.Store(v) }

func (s *r6TransitionRV) ChallengeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.challenges
}

func (s *r6TransitionRV) WaitArrived() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waits
}

func (s *r6TransitionRV) WaitHeld(t *testing.T, d time.Duration) *r6HeldWait {
	t.Helper()
	select {
	case h := <-s.held:
		return h
	case <-time.After(d):
		t.Fatalf("F1: no wait held within %s", d)
		return nil
	}
}

func (s *r6TransitionRV) Release(h *r6HeldWait, how int) {
	select {
	case h.release <- how:
	case <-time.After(2 * time.Second):
	}
	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
	}
}

func (s *r6TransitionRV) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 16<<10))
	switch r.URL.Path {
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
		fmt.Fprintf(w, `{"challenge":"%s","v":1}`+"\n", hex.EncodeToString(chal))
	case "/v1/verify":
		w.WriteHeader(http.StatusNoContent)
	case "/v1/wait":
		s.mu.Lock()
		s.waits++
		s.mu.Unlock()
		var req struct {
			Challenge string `json:"challenge"`
		}
		_ = json.Unmarshal(body, &req)
		h := &r6HeldWait{w: w, r: r, release: make(chan int, 1), done: make(chan struct{}), Challenge: req.Challenge}
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
