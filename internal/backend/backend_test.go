package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestUnixLinkRoundTripAndLifecycle(t *testing.T) {
	parent := t.TempDir()
	neighbor := filepath.Join(parent, "belongs-to-caller")
	if err := os.WriteFile(neighbor, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	var gotMethod, gotURI, gotBody, gotHeader string
	s, err := Listen(parent, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotURI = r.Method, r.URL.RequestURI()
		gotHeader = r.Header.Get("X-Test")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Add("X-Answer", "one")
		w.Header().Add("X-Answer", "two")
		w.Header().Set("Connection", "X-Hop")
		w.Header().Set("X-Hop", "private")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "answer")
	}))
	if err != nil {
		t.Fatal(err)
	}
	link := s.Link()
	if len(link.Token) != 64 || link.Socket == "" {
		t.Fatalf("link shape = %#v", link)
	}
	info, err := os.Stat(filepath.Dir(link.Socket))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("runtime directory mode = %o", info.Mode().Perm())
	}
	if info, err = os.Stat(link.Socket); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket stat = %v, %v", info, err)
	}

	c, err := NewClient(link)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	hello, err := c.Hello(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if hello.Major != ProtocolMajor || hello.Minor != ProtocolMinor || !hasCapability(hello.Capabilities, "http-proxy-v1") {
		t.Fatalf("hello = %#v", hello)
	}

	req := httptest.NewRequest(http.MethodPatch, "http://public.example/api/nodes/n1?q=a", strings.NewReader("payload"))
	req.Header.Set("X-Test", "preserved")
	req.Header.Set("Connection", "X-Request-Hop")
	req.Header.Set("X-Request-Hop", "drop")
	rec := httptest.NewRecorder()
	c.Proxy().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated || rec.Body.String() != "answer" {
		t.Fatalf("proxy response = %d %q", rec.Code, rec.Body.String())
	}
	if gotMethod != http.MethodPatch || gotURI != "/api/nodes/n1?q=a" || gotBody != "payload" || gotHeader != "preserved" {
		t.Fatalf("proxied request = %s %s %q %q", gotMethod, gotURI, gotBody, gotHeader)
	}
	if got := rec.Header().Values("X-Answer"); len(got) != 2 {
		t.Fatalf("multi-value response header = %v", got)
	}
	if rec.Header().Get("X-Hop") != "" || rec.Header().Get("Connection") != "" {
		t.Fatalf("hop response headers escaped: %v", rec.Header())
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, err := os.Stat(link.Socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket survives close: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(link.Socket)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime directory survives close: %v", err)
	}
	if got, err := os.ReadFile(neighbor); err != nil || string(got) != "keep" {
		t.Fatalf("caller neighbor changed: %q, %v", got, err)
	}
}

func TestProtocolAuthenticationMatrix(t *testing.T) {
	const token = "secret"
	called := 0
	h := authenticate(token, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called++
		w.WriteHeader(http.StatusNoContent)
	}))
	tests := []struct {
		name, tok, major, minor string
		want                    int
	}{
		{"current", token, "1", "0", http.StatusNoContent},
		{"future-minor", token, "1", "99", http.StatusNoContent},
		{"missing-token", "", "1", "0", http.StatusForbidden},
		{"wrong-token", "other", "1", "0", http.StatusForbidden},
		{"missing-major", token, "", "0", http.StatusUpgradeRequired},
		{"old-major", token, "0", "0", http.StatusUpgradeRequired},
		{"future-major", token, "2", "0", http.StatusUpgradeRequired},
		{"bad-minor", token, "1", "x", http.StatusBadRequest},
		{"negative-minor", token, "1", "-1", http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set(headerToken, tc.tok)
			req.Header.Set(headerMajor, tc.major)
			req.Header.Set(headerMinor, tc.minor)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			if tc.want == http.StatusUpgradeRequired && rec.Header().Get(headerMajor) != "1" {
				t.Fatal("major mismatch did not advertise muxer major")
			}
		})
	}
	if called != 2 {
		t.Fatalf("downstream called %d times, want 2", called)
	}
}

func TestConstructionAndUnavailableErrors(t *testing.T) {
	if _, err := Listen(t.TempDir(), nil); err == nil {
		t.Fatal("Listen accepted nil handler")
	}
	if _, err := Listen(filepath.Join(t.TempDir(), "missing", "parent"), http.NotFoundHandler()); err == nil {
		t.Fatal("Listen accepted missing parent")
	}
	if _, err := NewClient(Link{Token: "x"}); err == nil {
		t.Fatal("NewClient accepted empty socket")
	}
	if _, err := NewClient(Link{Socket: "/tmp/x"}); err == nil {
		t.Fatal("NewClient accepted empty token")
	}
	var nilServer *Server
	if nilServer.Link() != (Link{}) || nilServer.Close() != nil {
		t.Fatal("nil Server methods are not inert")
	}
	var nilClient *Client
	if _, err := nilClient.Hello(context.Background()); err == nil {
		t.Fatal("nil client Hello succeeded")
	}
	rec := httptest.NewRecorder()
	nilClient.Proxy().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("nil proxy = %d", rec.Code)
	}
	if err := nilClient.Close(); err != nil {
		t.Fatal(err)
	}

	c, err := NewClient(Link{Socket: filepath.Join(t.TempDir(), "absent.sock"), Token: "x"})
	if err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	c.Proxy().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("absent muxer proxy = %d", rec.Code)
	}
}

func TestHelloRejectsBadPeerDescriptions(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"status", http.StatusForbidden, `{}`},
		{"json", http.StatusOK, `{`},
		{"major", http.StatusOK, `{"major":2,"minor":0,"capabilities":["http-proxy-v1"]}`},
		{"capability", http.StatusOK, `{"major":1,"minor":0,"capabilities":["future"]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{http: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: tc.status,
					Status:     strconv.Itoa(tc.status),
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(tc.body)),
					Request:    req,
				}, nil
			})}, link: Link{Token: "x"}}
			if _, err := c.Hello(context.Background()); err == nil {
				t.Fatal("Hello accepted bad peer")
			}
		})
	}

	c := &Client{http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial")
	})}, link: Link{Token: "x"}}
	if _, err := c.Hello(context.Background()); err == nil {
		t.Fatal("Hello accepted transport failure")
	}
}

func TestHelloAcceptsAdditivePeerDescription(t *testing.T) {
	c := &Client{http: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header),
			Body:    io.NopCloser(strings.NewReader(`{"major":1,"minor":99,"capabilities":["http-proxy-v1","future-capability"]}`)),
			Request: req,
		}, nil
	})}, link: Link{Token: "x"}}
	hello, err := c.Hello(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if hello.Minor != 99 || !hasCapability(hello.Capabilities, "future-capability") {
		t.Fatalf("additive hello = %#v", hello)
	}
}

func TestProxyDoesNotFollowRedirects(t *testing.T) {
	s, err := Listen(t.TempDir(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/first" {
			http.Redirect(w, r, "/second", http.StatusFound)
			return
		}
		t.Fatal("proxy followed redirect")
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, _ := NewClient(s.Link())
	rec := httptest.NewRecorder()
	c.Proxy().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/first", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/second" {
		t.Fatalf("redirect response = %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestPublishStatus(t *testing.T) {
	remoteStatus := "enrolled"
	var got Status
	c := &Client{link: Link{Token: "cap"}, http: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.Path != "/_scimux/status" || req.Header.Get(headerToken) != "cap" {
			t.Fatalf("status request = %s %s headers %v", req.Method, req.URL.Path, req.Header)
		}
		if err := json.NewDecoder(req.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusNoContent, Status: "204 No Content", Header: make(http.Header), Body: http.NoBody}, nil
	})}}
	want := Status{Generation: 7, Version: "v7", Remote: &remoteStatus}
	if err := c.PublishStatus(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	if got.Generation != want.Generation || got.Version != want.Version || got.Remote == nil || *got.Remote != remoteStatus {
		t.Fatalf("published = %#v", got)
	}

	c.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadRequest, Status: "400 Bad Request", Header: make(http.Header), Body: http.NoBody}, nil
	})
	if err := c.PublishStatus(context.Background(), want); err == nil {
		t.Fatal("accepted rejected status report")
	}
	c.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("dial") })
	if err := c.PublishStatus(context.Background(), want); err == nil {
		t.Fatal("accepted status transport failure")
	}
}

func FuzzProtocolHeaders(f *testing.F) {
	f.Add("secret", "1", "0")
	f.Add("", "2", "-1")
	f.Fuzz(func(t *testing.T, token, major, minor string) {
		called := false
		h := authenticate("secret", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set(headerToken, token)
		req.Header.Set(headerMajor, major)
		req.Header.Set(headerMinor, minor)
		h.ServeHTTP(httptest.NewRecorder(), req)
		want := token == "secret" && major == "1"
		if n, err := strconv.Atoi(minor); err != nil || n < 0 {
			want = false
		}
		if called != want {
			t.Fatalf("downstream=%v, want %v", called, want)
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHelloJSONShape(t *testing.T) {
	// A small explicit shape assertion prevents accidental field renames from
	// becoming a silent old-muxer/new-web incompatibility.
	b, err := json.Marshal(Hello{Major: 1, Minor: 2, Capabilities: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, []byte(`{"major":1,"minor":2,"capabilities":["x"]}`)) {
		t.Fatalf("hello JSON = %s", b)
	}
}
