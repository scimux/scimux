package remote

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
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type authRejectionCase struct {
	name, route, contentType, body string
	stale                          bool
}

func authRejectionCases() []authRejectionCase {
	var cases []authRejectionCase
	for _, route := range []string{"/v1/challenge", "/v1/verify"} {
		for _, response := range []struct{ name, ct, body string }{
			{"proxy", "text/html", "<html>route not found</html>"},
			{"protocol", "text/plain; charset=utf-8", "not found\n"},
			{"oversized", "text/html", strings.Repeat("x", httpBodyMax+1)},
		} {
			cases = append(cases, authRejectionCase{route + "/" + response.name, route, response.ct, response.body, false})
		}
	}
	return append(cases, authRejectionCase{"/v1/verify/stale", "/v1/verify", "text/plain; charset=utf-8", "not found\n", true})
}

func TestAuthRejectionClassification(t *testing.T) {
	for _, tc := range authRejectionCases() {
		t.Run(tc.name, func(t *testing.T) {
			rv, cfg := newAuthRejectionRV(t)
			c := NewClient(cfg)
			seedAuthRejectionIdentity(t, rv, c)
			if err := c.loadState(); err != nil {
				t.Fatal(err)
			}
			before := authStateBytes(t, c)
			rv.reject = &tc
			requireClass(t, c.authenticate(context.Background()), ClassUnavailable)
			if rv.accepted != 0 {
				t.Fatal("rejection authenticated the installation")
			}
			if tc.stale && rv.expired != 1 {
				t.Fatal("stale challenge was not expired by the fake clock")
			}
			if !bytes.Equal(before, authStateBytes(t, c)) {
				t.Fatal("authentication rejection changed durable identity")
			}
		})
	}
}

func TestAuthRejectionStartupAndRestart(t *testing.T) {
	for _, tc := range authRejectionCases() {
		t.Run(tc.name, func(t *testing.T) {
			rv, cfg := newAuthRejectionRV(t)
			c := NewClient(cfg)
			seeded := seedAuthRejectionIdentity(t, rv, c)
			before := authStateBytes(t, c)
			rv.reject = &tc
			ctx, cancel := ctxTO(t)
			defer cancel()
			if err := c.Start(ctx); err != nil {
				t.Fatalf("enrolled startup must remain recoverable: %v", err)
			}
			defer c.Close()
			if c.HostedStatus() != "unavailable" {
				t.Fatalf("hosted = %q, want unavailable", c.HostedStatus())
			}
			if !bytes.Equal(before, authStateBytes(t, c)) {
				t.Fatal("startup rejection rewrote enrollment, keys, handle or devices")
			}
			if rv.accepted != 0 {
				t.Fatal("server rejection allowed authentication")
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			rv.reject = nil
			restarted := NewClient(cfg)
			if err := restarted.Start(ctx); err != nil {
				t.Fatalf("restart without another invite: %v", err)
			}
			defer restarted.Close()
			if restarted.HostedStatus() != "enrolled" || rv.accepted != 1 {
				t.Fatal("restart did not authenticate successfully")
			}
			if got := mustState(t, restarted.StatePath()); !reflect.DeepEqual(got, seeded) {
				t.Fatal("restart changed persisted identity or devices")
			}
			devices, err := restarted.Devices()
			if err != nil || len(devices) != 2 {
				t.Fatalf("restart devices = %v, %v", devices, err)
			}
			if rv.fake.EnrollCount() != 0 {
				t.Fatal("restart spent an invite")
			}
		})
	}
}

func TestAuthRejectionAfterFirstEnrollment(t *testing.T) {
	for _, tc := range authRejectionCases() {
		t.Run(tc.name, func(t *testing.T) {
			rv, cfg := newAuthRejectionRV(t)
			cfg.InviteFile = writeInviteFile(t, cfg.DataDir, vectorInviteGrouped, 0o600)
			c := NewClient(cfg)
			rv.reject = &tc
			ctx, cancel := ctxTO(t)
			defer cancel()
			// First enrollment still returns the authentication error to its caller.
			requireClass(t, c.Start(ctx), ClassUnavailable)
			defer c.Close()
			seeded := mustState(t, c.StatePath())
			if seeded.Status != StateEnrolled || seeded.Handle == "" || c.HostedStatus() != "unavailable" {
				t.Fatalf("confirmed enrollment was lost: status=%q hosted=%q", seeded.Status, c.HostedStatus())
			}
			if rv.accepted != 0 || rv.fake.EnrollCount() != 1 || !rv.fake.Redeemed(vectorInviteGrouped) {
				t.Fatal("expected one enrollment and no authenticated success")
			}
			if _, err := os.Stat(cfg.InviteFile); !os.IsNotExist(err) {
				t.Fatalf("confirmed enrollment did not consume invite: %v", err)
			}
			before := authStateBytes(t, c)
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			cfg.InviteFile = ""
			rv.reject = nil
			restarted := NewClient(cfg)
			if err := restarted.Start(ctx); err != nil {
				t.Fatalf("restart required another invite: %v", err)
			}
			defer restarted.Close()
			if restarted.HostedStatus() != "enrolled" || rv.accepted != 1 {
				t.Fatal("restart did not authenticate")
			}
			if !bytes.Equal(before, authStateBytes(t, restarted)) || rv.fake.EnrollCount() != 1 {
				t.Fatal("restart replaced confirmed enrollment")
			}
		})
	}
}

func authStateBytes(t *testing.T, c *Client) []byte {
	t.Helper()
	raw, err := os.ReadFile(c.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func seedAuthRejectionIdentity(t *testing.T, rv *authRejectionRV, c *Client) PersistedState {
	t.Helper()
	st := enrolledFixture(t, "ih_041061050R3GG28A")
	st.Devices = []PersistedDevice{
		{ID: "phone", RID: strings.Repeat("ab", 32), PubKey: hex.EncodeToString(testPhonePubKey)},
		{ID: "tablet", RID: strings.Repeat("cd", 32), PubKey: hex.EncodeToString(testTabletPubKey)},
	}
	writeState(t, c, st)
	rv.fake.mu.Lock()
	rv.fake.installs[st.Handle] = &fakeInstall{handle: st.Handle, pubkey: st.PublicKey}
	rv.fake.mu.Unlock()
	return st
}

// authRejectionRV intercepts only admission authentication. The existing fake
// owns enrollment, including single-use invites. Authentication checks real
// signatures and consumes unique challenges with a deterministically driven TTL.
// These tests leave polling disabled, so all transport calls are synchronous.
type authRejectionRV struct {
	fake                   *fakeRV
	clock                  *fakeClock
	reject                 *authRejectionCase
	seq, accepted, expired int
	issued                 map[string]time.Time
}

func newAuthRejectionRV(t *testing.T) (*authRejectionRV, Config) {
	t.Helper()
	fake := newFakeRV(t)
	cfg := clientCfg(t, fake)
	rv := &authRejectionRV{fake: fake, clock: newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), issued: map[string]time.Time{}}
	base := cfg.HTTPClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	cfg.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/v1/challenge" && req.URL.Path != "/v1/verify" {
			return base.RoundTrip(req)
		}
		w := httptest.NewRecorder()
		rv.serve(w, req)
		return w.Result(), nil
	})}
	return rv, cfg
}

func (rv *authRejectionRV) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	rv.fake.record(r, raw)
	var req struct{ Handle, Challenge, Sig string }
	_ = json.Unmarshal(raw, &req)
	rv.fake.mu.Lock()
	inst := rv.fake.installs[req.Handle]
	rv.fake.mu.Unlock()
	if inst == nil || inst.revoked {
		constantReject(w)
		return
	}
	if rv.reject != nil && r.URL.Path == rv.reject.route && !rv.reject.stale {
		w.Header().Set("Content-Type", rv.reject.contentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, rv.reject.body)
		return
	}
	if r.URL.Path == "/v1/challenge" {
		rv.seq++
		challenge := fmt.Sprintf("%064x", rv.seq)
		rv.issued[challenge] = rv.clock.Now().Add(30 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"challenge":%q,"v":1}`, challenge)
		return
	}
	if rv.reject != nil && rv.reject.stale {
		rv.clock.Advance(31 * time.Second)
	}
	expires, ok := rv.issued[req.Challenge]
	delete(rv.issued, req.Challenge)
	if ok && !rv.clock.Now().Before(expires) {
		rv.expired++
		constantReject(w)
		return
	}
	chal, _ := hex.DecodeString(req.Challenge)
	sig, _ := hex.DecodeString(req.Sig)
	pub, _ := hex.DecodeString(inst.pubkey)
	if !ok || !ed25519.Verify(pub, authMessage(DefaultOrigin, ProtocolVersion, r.URL.Path, req.Handle, chal), sig) {
		constantReject(w)
		return
	}
	rv.accepted++
	w.WriteHeader(http.StatusNoContent)
}

// authRetryScheduler announces each installed retry. Firing exactly one
// callback avoids sleeps and cross-device scheduling races in the regression.
type authRetry struct {
	delay     time.Duration
	fire      func()
	cancelled bool
}
type authRetryScheduler struct {
	mu    sync.Mutex
	ready chan *authRetry
}

func newAuthRetryScheduler() *authRetryScheduler {
	return &authRetryScheduler{ready: make(chan *authRetry, 64)}
}
func (s *authRetryScheduler) After(delay time.Duration, fn func()) func() {
	retry := &authRetry{delay: delay, fire: fn}
	s.ready <- retry
	return func() { s.mu.Lock(); retry.cancelled = true; s.mu.Unlock() }
}
func (s *authRetryScheduler) next(t *testing.T) *authRetry {
	t.Helper()
	select {
	case retry := <-s.ready:
		return retry
	case <-time.After(3 * time.Second):
		t.Fatal("device loop did not schedule a retry")
		return nil
	}
}
func (s *authRetryScheduler) fire(t *testing.T, retry *authRetry) {
	t.Helper()
	s.mu.Lock()
	cancelled := retry.cancelled
	s.mu.Unlock()
	if cancelled {
		t.Fatal("device retry was cancelled")
	}
	retry.fire()
}

// A 404 remains retryable even when a proxy truncates its error body.
func TestAuthRejectionUnreadableBody(t *testing.T) {
	for _, route := range []string{"/v1/challenge", "/v1/verify"} {
		t.Run(route, func(t *testing.T) {
			rv, cfg := newAuthRejectionRV(t)
			base := cfg.HTTPClient.Transport
			cfg.HTTPClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != route {
					return base.RoundTrip(r)
				}
				return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: io.NopCloser(authRejectionErrorReader{})}, nil
			})
			c := NewClient(cfg)
			seedAuthRejectionIdentity(t, rv, c)
			before := authStateBytes(t, c)
			ctx, cancel := ctxTO(t)
			defer cancel()
			if err := c.Start(ctx); err != nil || c.HostedStatus() != "unavailable" {
				t.Fatalf("truncated rejection: hosted=%q err=%v", c.HostedStatus(), err)
			}
			defer c.Close()
			if !bytes.Equal(before, authStateBytes(t, c)) || rv.accepted != 0 {
				t.Fatal("truncated rejection changed enrollment or authenticated")
			}
		})
	}
}

type authRejectionErrorReader struct{}

func (authRejectionErrorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// Recovery still requires valid authentication responses. A malformed 2xx
// after rejection must neither authorize the installation nor damage its state.
func TestAuthRejectionRecoveryRequiresValidResponses(t *testing.T) {
	for _, tc := range []struct {
		name, route, body, length string
		status                    int
	}{
		{"challenge-json", "/v1/challenge", "{", "", http.StatusOK},
		{"challenge-version", "/v1/challenge", `{"v":0,"challenge":"00"}`, "", http.StatusOK},
		{"challenge-nonce", "/v1/challenge", `{"v":1,"challenge":"00"}`, "", http.StatusOK},
		{"verify-status", "/v1/verify", "", "", http.StatusOK},
		{"verify-oversized", "/v1/verify", strings.Repeat("x", verifyBodyMax+1), "", http.StatusNoContent},
		{"verify-body", "/v1/verify", "x", "", http.StatusNoContent},
		{"verify-length", "/v1/verify", "", "invalid", http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rv, cfg := newAuthRejectionRV(t)
			base := cfg.HTTPClient.Transport
			malformed := false
			cfg.HTTPClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if !malformed || r.URL.Path != tc.route {
					return base.RoundTrip(r)
				}
				w := httptest.NewRecorder()
				w.Header().Set("Content-Type", "application/json")
				if tc.length != "" {
					w.Header().Set("Content-Length", tc.length)
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
				return w.Result(), nil
			})
			c := NewClient(cfg)
			seedAuthRejectionIdentity(t, rv, c)
			before := authStateBytes(t, c)
			ctx, cancel := ctxTO(t)
			defer cancel()
			rv.reject = &authRejectionCase{route: tc.route, contentType: "text/plain; charset=utf-8", body: "not found\n"}
			if err := c.Start(ctx); err != nil || c.HostedStatus() != "unavailable" {
				t.Fatalf("initial rejection: %v, hosted=%q", err, c.HostedStatus())
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			rv.reject = nil
			malformed = true
			c = NewClient(cfg)
			if err := c.Start(ctx); err != nil || c.HostedStatus() != "unavailable" {
				t.Fatalf("malformed recovery: %v, hosted=%q", err, c.HostedStatus())
			}
			if rv.accepted != 0 || !bytes.Equal(before, authStateBytes(t, c)) {
				t.Fatal("malformed recovery authenticated or changed identity")
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			malformed = false
			c = NewClient(cfg)
			defer c.Close()
			if err := c.Start(ctx); err != nil || c.HostedStatus() != "enrolled" || rv.accepted != 1 {
				t.Fatalf("valid recovery: %v, hosted=%q", err, c.HostedStatus())
			}
			if !bytes.Equal(before, authStateBytes(t, c)) || rv.fake.EnrollCount() != 0 {
				t.Fatal("valid recovery replaced enrollment")
			}
		})
	}
}

func TestAuthRejectionMessages(t *testing.T) {
	for _, tc := range []struct{ route, message string }{
		{"/v1/challenge", "rendezvous rejected the challenge request"},
		{"/v1/verify", "rendezvous rejected authentication"},
	} {
		t.Run(tc.route, func(t *testing.T) {
			rv, cfg := newAuthRejectionRV(t)
			c := NewClient(cfg)
			seedAuthRejectionIdentity(t, rv, c)
			if err := c.loadState(); err != nil {
				t.Fatal(err)
			}
			rv.reject = &authRejectionCase{route: tc.route, contentType: "text/plain; charset=utf-8", body: "not found\n"}
			err := c.authenticate(context.Background())
			requireClass(t, err, ClassUnavailable)
			if got := guidanceOf(err); got != tc.message {
				t.Fatalf("guidance = %q, want %q", got, tc.message)
			}
		})
	}
}
