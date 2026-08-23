package remote

// Join 1 test harness — a rendezvous that can actually hand the laptop a
// sealed session offer and capture what the laptop sends back.
//
// The existing S5 fakes (fakeRV, r6WaitRV) exercise /v1/wait as a long poll
// whose only interesting outcomes are 204, 404 and transport failure. That was
// the right scope for S5, which was about enrolment and lifecycle. It cannot
// express the case join 1 is about: a 200 whose octet-stream body is the
// device's sealed §12.2 envelope, and the `reply` field the laptop is supposed
// to post back on its next wait.
//
// This harness is deliberately separate rather than an extension of fakeRV.
// The S5 fakes are load-bearing for a dozen accepted assertions, and widening
// them to carry envelope state would put those assertions at risk for no gain.

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// sessionRV is a rendezvous that speaks exactly the three endpoints the wait
// loop touches: /v1/challenge, /v1/verify and /v1/wait. It holds one queued
// envelope per rid and records every reply the laptop posts.
type sessionRV struct {
	t   *testing.T
	srv *httptest.Server

	mu          sync.Mutex
	queued      map[string][]byte // rid -> sealed offer not yet delivered
	delivered   map[string]int    // rid -> how many times an envelope was served
	replies     [][]byte          // every decoded `reply` seen, in order
	waits       int
	waitRID     map[string]int // rid -> /v1/wait count
	pairWaitRID map[string]int // rid -> /v1/pair/wait count
}

func newSessionRV(t *testing.T) *sessionRV {
	t.Helper()
	f := &sessionRV{
		t:           t,
		queued:      map[string][]byte{},
		delivered:   map[string]int{},
		waitRID:     map[string]int{},
		pairWaitRID: map[string]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/challenge", f.handleChallenge)
	mux.HandleFunc("/v1/verify", f.handleVerify)
	mux.HandleFunc("/v1/wait", f.handleWait)
	mux.HandleFunc("/v1/pair/wait", f.handleWait)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *sessionRV) URL() string              { return f.srv.URL }
func (f *sessionRV) HTTPClient() *http.Client { return f.srv.Client() }

// QueueEnvelope makes sealed the next thing /v1/wait hands back for rid.
func (f *sessionRV) QueueEnvelope(rid string, sealed []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queued[rid] = append([]byte(nil), sealed...)
}

// Replies is every reply blob the laptop has posted so far.
func (f *sessionRV) Replies() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.replies))
	copy(out, f.replies)
	return out
}

// WaitCount is how many /v1/wait requests have been served. Tests use it to
// tell "the loop is running and saw no envelope" from "the loop never ran",
// which would otherwise both look like an empty Replies().
func (f *sessionRV) WaitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.waits
}

func (f *sessionRV) SessionWaitCount(rid string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.waitRID[rid]
}

func (f *sessionRV) PairWaitCount(rid string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pairWaitRID[rid]
}

// DeliveredCount is how many times an envelope was served for rid.
func (f *sessionRV) DeliveredCount(rid string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.delivered[rid]
}

func (f *sessionRV) handleChallenge(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"v":         ProtocolVersion,
		"challenge": strings.Repeat("22", 32),
	})
}

func (f *sessionRV) handleVerify(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Rv-Challenge", strings.Repeat("55", 32))
	w.WriteHeader(http.StatusNoContent)
}

func (f *sessionRV) handleWait(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		ID    string `json:"id"`
		Reply string `json:"reply"`
	}
	_ = json.Unmarshal(body, &req)

	f.mu.Lock()
	f.waits++
	if r.URL.Path == "/v1/pair/wait" {
		f.pairWaitRID[req.ID]++
	} else {
		f.waitRID[req.ID]++
	}
	// A reply rides in on an ordinary wait; the vectors show the signature is
	// byte-identical with and without it, so it is not signature-covered.
	if req.Reply != "" {
		if raw, err := hex.DecodeString(req.Reply); err == nil {
			f.replies = append(f.replies, raw)
		} else {
			f.replies = append(f.replies, []byte(req.Reply))
		}
	}
	sealed := f.queued[req.ID]
	if len(sealed) > 0 {
		delete(f.queued, req.ID)
		f.delivered[req.ID]++
	}
	f.mu.Unlock()

	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Rv-Challenge", strings.Repeat("55", 32))
	if len(sealed) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(sealed)
}
