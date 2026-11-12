package team

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	helloID   = "0123456789abcdef0123456789abcdef"
	helloName = "Lab relay"
	regToken  = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	seatID    = "fedcba9876543210fedcba9876543210"
)

func helloBody(id, name string) string {
	return `{"protocol":1,"server":{"id":"` + id + `","name":"` + name + `"}}`
}

func registerBody(token, id, name string) string {
	return `{"token":"` + token + `","server":{"id":"` + id + `","name":"` + name + `"}}`
}

type capturedRequest struct {
	method string
	path   string
	body   string
	header http.Header
}

func serveTeam(t *testing.T, fn func(http.ResponseWriter, *http.Request, *capturedRequest)) (*httptest.Server, *[]capturedRequest) {
	t.Helper()
	var mu sync.Mutex
	var got []capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		cap := capturedRequest{method: r.Method, path: r.URL.Path, body: string(raw), header: r.Header.Clone()}
		mu.Lock()
		got = append(got, cap)
		mu.Unlock()
		fn(w, r, &cap)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func clientOn(srv *httptest.Server) *Client {
	c := NewClient()
	c.Transport = srv.Client().Transport
	c.timeout = 10 * time.Second
	return c
}

func TestNewClientTimeoutIsTenSeconds(t *testing.T) {
	c := NewClient()
	if c.timeout != 10*time.Second {
		t.Fatalf("timeout = %s, want 10s", c.timeout)
	}
}

func TestHelloSuccess(t *testing.T) {
	srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		if r.URL.Path != "/v1/hello" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, helloBody(helloID, helloName))
	})
	srvID, err := clientOn(srv).Hello(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if srvID.ID != helloID || srvID.Name != helloName {
		t.Fatalf("server = %+v", srvID)
	}
	if len(*got) != 1 || (*got)[0].method != http.MethodGet || (*got)[0].path != "/v1/hello" || (*got)[0].body != "" {
		t.Fatalf("request = %+v", *got)
	}
	if (*got)[0].header.Get("Accept") != "application/json" {
		t.Fatalf("Accept = %q", (*got)[0].header.Get("Accept"))
	}
}

func TestHelloNilContextStillReachesTheServer(t *testing.T) {
	srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, helloBody(helloID, helloName))
	})
	gotSrv, err := clientOn(srv).Hello(nil, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if gotSrv.ID != helloID || len(*got) != 1 {
		t.Fatalf("server = %+v requests = %d", gotSrv, len(*got))
	}
}

func TestHelloRejectsShape(t *testing.T) {
	longName := strings.Repeat("a", 65)
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"protocol zero", http.StatusOK, `{"protocol":0,"server":{"id":"` + helloID + `","name":"Lab relay"}}`, ErrProtocol},
		{"short id", http.StatusOK, helloBody("0123456789abcdef", helloName), ErrProtocol},
		{"uppercase id", http.StatusOK, helloBody("0123456789ABCDEF0123456789ABCDEF", helloName), ErrProtocol},
		{"non hex id", http.StatusOK, helloBody("0123456789abcdef0123456789abcdeg", helloName), ErrProtocol},
		{"empty name", http.StatusOK, helloBody(helloID, ""), ErrProtocol},
		{"long name", http.StatusOK, helloBody(helloID, longName), ErrProtocol},
		{"control name", http.StatusOK, helloBody(helloID, `Lab\nrelay`), ErrProtocol},
		{"name not a string", http.StatusOK, `{"protocol":1,"server":{"id":"` + helloID + `","name":1}}`, ErrProtocol},
		{"missing server", http.StatusOK, `{"protocol":1}`, ErrProtocol},
		{"not json", http.StatusOK, `not-json`, ErrProtocol},
		{"unknown field", http.StatusOK, `{"protocol":1,"extra":true,"server":{"id":"` + helloID + `","name":"Lab relay"}}`, ErrProtocol},
		{"trailing value", http.StatusOK, helloBody(helloID, helloName) + `{}`, ErrProtocol},
		{"created instead of ok", http.StatusCreated, helloBody(helloID, helloName), ErrProtocol},
		{"other error code", http.StatusInternalServerError, `{"error":"internal"}`, ErrProtocol},
		{"join refused", http.StatusForbidden, `{"error":"join_refused"}`, ErrJoinRefused},
		{"trailing error", http.StatusForbidden, `{"error":"join_refused"}{}`, ErrProtocol},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			_, err := clientOn(srv).Hello(context.Background(), srv.URL)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestHelloAcceptsA64ByteName(t *testing.T) {
	name := strings.Repeat("a", 64)
	srv, _ := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, helloBody(helloID, name))
	})
	got, err := clientOn(srv).Hello(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != name {
		t.Fatalf("name = %q", got.Name)
	}
}

func TestRegisterSuccessSendsTheExactBody(t *testing.T) {
	const code = "ab-cd ef"
	const name = "A&B"
	srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, registerBody(regToken, helloID, helloName))
	})
	token, server, err := clientOn(srv).Register(context.Background(), srv.URL, code, seatID, name)
	if err != nil {
		t.Fatal(err)
	}
	if token != regToken || server.ID != helloID || server.Name != helloName {
		t.Fatalf("token = %q server = %+v", token, server)
	}
	if len(*got) != 1 {
		t.Fatalf("requests = %d", len(*got))
	}
	req := (*got)[0]
	if req.method != http.MethodPost || req.path != "/v1/register" {
		t.Fatalf("request = %s %s", req.method, req.path)
	}
	if req.header.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q", req.header.Get("Content-Type"))
	}
	if req.header.Get("Accept") != "application/json" {
		t.Fatalf("Accept = %q", req.header.Get("Accept"))
	}
	if req.header.Get("Authorization") != "" {
		t.Fatalf("Authorization = %q", req.header.Get("Authorization"))
	}
	want := `{"join_code":"ab-cd ef","installation_id":"` + seatID + `","name":"A&B"}`
	if req.body != want {
		t.Fatalf("body = %s, want %s", req.body, want)
	}
}

func TestRegisterMapsErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"join refused", http.StatusForbidden, `{"error":"join_refused"}`, ErrJoinRefused},
		{"already registered", http.StatusConflict, `{"error":"already_registered"}`, ErrAlreadyRegistered},
		{"quota", http.StatusInsufficientStorage, `{"error":"quota"}`, ErrServerFull},
		{"bad request", http.StatusBadRequest, `{"error":"bad_request"}`, ErrRejected},
		{"other code", http.StatusInternalServerError, `{"error":"internal"}`, ErrProtocol},
		{"not json", http.StatusBadRequest, `nope`, ErrProtocol},
		{"unknown field", http.StatusForbidden, `{"error":"join_refused","detail":"x"}`, ErrProtocol},
		{"ok instead of created", http.StatusOK, registerBody(regToken, helloID, helloName), ErrProtocol},
		{"short token", http.StatusCreated, registerBody("abcd", helloID, helloName), ErrProtocol},
		{"uppercase token", http.StatusCreated, registerBody(strings.ToUpper(regToken), helloID, helloName), ErrProtocol},
		{"non hex token", http.StatusCreated, registerBody(regToken[:63]+"g", helloID, helloName), ErrProtocol},
		{"bad server", http.StatusCreated, registerBody(regToken, "0123456789abcdef", helloName), ErrProtocol},
		{"empty server name", http.StatusCreated, registerBody(regToken, helloID, ""), ErrProtocol},
		{"control server name", http.StatusCreated, registerBody(regToken, helloID, `a\nb`), ErrProtocol},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			_, _, err := clientOn(srv).Register(context.Background(), srv.URL, "ab-cd ef", seatID, "Ada")
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestClientUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	c := NewClient()
	c.timeout = time.Second
	if _, err := c.Hello(context.Background(), url); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("Hello error = %v", err)
	}
	if _, _, err := c.Register(context.Background(), url, "code", seatID, "Ada"); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("Register error = %v", err)
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		http.Redirect(w, r, "/v1/landed", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	c := clientOn(srv)
	_, err := c.Hello(context.Background(), srv.URL)
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("error = %v", err)
	}
	if len(paths) != 1 || paths[0] != "/v1/hello" {
		t.Fatalf("paths = %v", paths)
	}
}

func TestClientRejectsOversizedBody(t *testing.T) {
	body := regToken + strings.Repeat("x", 64*1024)
	srv, _ := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	})
	_, err := clientOn(srv).Hello(context.Background(), srv.URL)
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), regToken) {
		t.Fatalf("error quotes the body: %v", err)
	}
}

func TestClientTimeoutIsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	c := clientOn(srv)
	c.timeout = 30 * time.Millisecond
	_, err := c.Hello(context.Background(), srv.URL)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("error = %v", err)
	}
}

func TestClientCanceledContextIsUnreachable(t *testing.T) {
	srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, helloBody(helloID, helloName))
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := clientOn(srv).Hello(ctx, srv.URL)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("error = %v", err)
	}
	if len(*got) != 0 {
		t.Fatalf("canceled call sent %#v", *got)
	}
}

func TestClientIgnoresProxyFromTheEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("https_proxy", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")
	srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, helloBody(helloID, helloName))
	})
	c := NewClient()
	c.timeout = 2 * time.Second
	gotSrv, err := c.Hello(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if gotSrv.ID != helloID || len(*got) != 1 {
		t.Fatalf("server = %+v requests = %d", gotSrv, len(*got))
	}
}

func TestClientErrorsOmitCodeAndToken(t *testing.T) {
	const code = "SECRET-JOIN-CODE"
	token := strings.Repeat("ab", 32)
	srv, _ := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, token)
	})
	_, _, err := clientOn(srv).Register(context.Background(), srv.URL, code, seatID, "Ada")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), code) || strings.Contains(err.Error(), token) {
		t.Fatalf("error quotes a secret: %v", err)
	}
}

func TestClientRejectsABadBaseURL(t *testing.T) {
	c := NewClient()
	c.timeout = time.Second
	cases := []string{
		"http://[",
		"/relative",
		"ftp://relay.example/join",
		"http:///no-host",
		"http://ada:secret@relay.example",
	}
	for _, base := range cases {
		_, err := c.Hello(context.Background(), base)
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("%s: error = %v", base, err)
		}
		if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), base) {
			t.Fatalf("%s: error quotes the input: %v", base, err)
		}
	}
}

type staticTrip struct {
	status int
	body   string
	gotURL string
}

func (s *staticTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	s.gotURL = r.URL.String()
	return &http.Response{
		StatusCode: s.status,
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Header:     make(http.Header),
		Request:    r,
	}, nil
}

func TestClientAcceptsAnHTTPSBase(t *testing.T) {
	trip := &staticTrip{status: http.StatusOK, body: helloBody(helloID, helloName)}
	c := NewClient()
	c.Transport = trip
	c.timeout = time.Second
	got, err := c.Hello(context.Background(), "https://relay.example")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != helloID || trip.gotURL != "https://relay.example/v1/hello" {
		t.Fatalf("server = %+v url = %s", got, trip.gotURL)
	}
}

type failBody struct{}

func (failBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (failBody) Close() error             { return nil }

type failTrip struct{}

func (failTrip) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: failBody{}, Header: make(http.Header)}, nil
}

func TestClientReadErrorIsUnreachable(t *testing.T) {
	c := NewClient()
	c.Transport = failTrip{}
	c.timeout = time.Second
	_, err := c.Hello(context.Background(), "http://relay.example")
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("error = %v", err)
	}
}
