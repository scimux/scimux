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

	"codeberg.org/chrberger/scimux/internal/remote"
)

func TestS5R5_F2_HostedStatusWaitChallengeTransitions(t *testing.T) {
	srv := newTransitionRV(t)
	data := t.TempDir()
	writeEnrolledStateWithDevice(t, data)
	cmd := &Command{
		Args:   []string{"scimux", "--remote", "-data", data, "-addr", "127.0.0.1:0"},
		Stdout: io.Discard,
		Stderr: new(bytes.Buffer),
		Home:   t.TempDir(),
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
		t.Fatalf("F2: run: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Client() != nil {
			_ = cmd.Client().Close()
		}
	})

	w1 := srv.WaitHeld(t, 5*time.Second)
	assertHostedStatus(t, cmd, "enrolled")
	srv.Release(w1, waitReleaseNoContent)

	w2 := srv.WaitHeld(t, 5*time.Second)
	srv.Release(w2, waitReleaseHangup)
	waitHosted(t, cmd, "unavailable", 3*time.Second)

	w3 := srv.WaitHeld(t, 5*time.Second)
	assertHostedStatus(t, cmd, "unavailable")
	srv.Release(w3, waitReleaseNoContent)
	waitHosted(t, cmd, "enrolled", 3*time.Second)

	nChal := srv.ChallengeCount()
	nWait := srv.WaitArrived()
	w4 := srv.WaitHeld(t, 5*time.Second)
	assertHostedStatus(t, cmd, "enrolled")
	srv.SetRevoke(true)
	srv.Release(w4, waitReleaseRevoked)
	waitHosted(t, cmd, "revoked", 3*time.Second)

	raw, err := os.ReadFile(filepath.Join(data, remote.PrivateDirName, remote.StateFileName))
	if err != nil {
		t.Fatal(err)
	}
	var st remote.PersistedState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	if st.Status != remote.StateRevoked {
		t.Fatalf("F2: persisted status = %q, want revoked; bytes=%s", st.Status, raw)
	}

	time.Sleep(80 * time.Millisecond)
	if srv.WaitArrived() > nWait+1 {
		t.Fatalf("F2: still reconnecting after revoke: waits %d -> %d", nWait, srv.WaitArrived())
	}
	if srv.ChallengeCount() <= nChal {
		t.Fatalf("F2: wait rejection did not look up a fresh challenge: chal %d -> %d", nChal, srv.ChallengeCount())
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
		fmt.Fprintf(w, `{"challenge":"%s","v":1}`+"\n", strings.Repeat("22", 32))
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
		h := &heldWait{w: w, r: r, release: make(chan int, 1), done: make(chan struct{})}
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
