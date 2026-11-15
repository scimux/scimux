package team

import (
	"context"
	"errors"
	"io"
	"net"
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

const teamCallToken = "SYNTHETIC-TEAM-TOKEN"

func seatsBody() string {
	return `{"seats":[{"id":"` + seatID + `","name":"Ada"}]}`
}

func assertTokenAbsent(t *testing.T, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), teamCallToken) {
		t.Fatalf("error quotes the token: %v", err)
	}
}

func TestCheckAcceptsASeatsArray(t *testing.T) {
	bodies := []string{
		`{"seats":[]}`,
		seatsBody(),
		`{"seats":[ ],"extra":true}`,
	}
	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, body)
			})
			err := clientOn(srv).Check(context.Background(), srv.URL, teamCallToken)
			if err != nil {
				t.Fatal(err)
			}
			if len(*got) != 1 {
				t.Fatalf("requests = %d", len(*got))
			}
			req := (*got)[0]
			if req.method != http.MethodGet || req.path != "/v1/seats" || req.body != "" {
				t.Fatalf("request = %+v", req)
			}
			if req.header.Get("Authorization") != "Bearer "+teamCallToken {
				t.Fatalf("Authorization = %q", req.header.Get("Authorization"))
			}
			if req.header.Get("Content-Type") != "" {
				t.Fatalf("Content-Type = %q", req.header.Get("Content-Type"))
			}
			if strings.Contains(req.body, teamCallToken) || strings.Contains(srv.URL, teamCallToken) {
				t.Fatal("token left the Authorization header")
			}
		})
	}
}

func TestCheckRejectsTheAnswer(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"not json", http.StatusOK, "not-json", ErrProtocol},
		{"array", http.StatusOK, `[]`, ErrProtocol},
		{"missing seats", http.StatusOK, `{}`, ErrProtocol},
		{"seats object", http.StatusOK, `{"seats":{}}`, ErrProtocol},
		{"seats null", http.StatusOK, `{"seats":null}`, ErrProtocol},
		{"seats string", http.StatusOK, `{"seats":"[]"}`, ErrProtocol},
		{"seats number", http.StatusOK, `{"seats":0}`, ErrProtocol},
		{"trailing value", http.StatusOK, `{"seats":[]}{}`, ErrProtocol},
		{"unauthorized json", http.StatusUnauthorized, `{"error":"unauthorized"}`, ErrUnauthorized},
		{"unauthorized text", http.StatusUnauthorized, teamCallToken, ErrUnauthorized},
		{"not found", http.StatusNotFound, `{"error":"not_found"}`, ErrProtocol},
		{"server error", http.StatusInternalServerError, teamCallToken, ErrProtocol},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			err := clientOn(srv).Check(context.Background(), srv.URL, teamCallToken)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			assertTokenAbsent(t, err)
			if len(*got) != 1 || (*got)[0].body != "" || (*got)[0].header.Get("Authorization") != "Bearer "+teamCallToken {
				t.Fatalf("request = %+v", *got)
			}
		})
	}
}

func TestLeaveAcceptsNoContentAndARefusedToken(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusUnauthorized} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
				w.WriteHeader(status)
				if status == http.StatusUnauthorized {
					_, _ = io.WriteString(w, teamCallToken)
				}
			})
			err := clientOn(srv).Leave(context.Background(), srv.URL, teamCallToken)
			if status == http.StatusNoContent {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("error = %v", err)
			}
			assertTokenAbsent(t, err)
			if len(*got) != 1 {
				t.Fatalf("requests = %d", len(*got))
			}
			req := (*got)[0]
			if req.method != http.MethodDelete || req.path != "/v1/seat" || req.body != "" {
				t.Fatalf("request = %+v", req)
			}
			if req.header.Get("Authorization") != "Bearer "+teamCallToken {
				t.Fatalf("Authorization = %q", req.header.Get("Authorization"))
			}
			if req.header.Get("Content-Type") != "" {
				t.Fatalf("Content-Type = %q", req.header.Get("Content-Type"))
			}
		})
	}
}

func TestLeaveRejectsOtherAnswers(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"not found", http.StatusNotFound, `{"error":"not_found"}`},
		{"server error", http.StatusInternalServerError, teamCallToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			err := clientOn(srv).Leave(context.Background(), srv.URL, teamCallToken)
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("error = %v", err)
			}
			assertTokenAbsent(t, err)
		})
	}
}

func TestCheckAndLeaveAreUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	c := NewClient()
	c.timeout = time.Second
	err := c.Check(context.Background(), url, teamCallToken)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("Check error = %v", err)
	}
	assertTokenAbsent(t, err)
	err = c.Leave(context.Background(), url, teamCallToken)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("Leave error = %v", err)
	}
	assertTokenAbsent(t, err)
}

func TestCheckAndLeaveTimeOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	c := clientOn(srv)
	c.timeout = 30 * time.Millisecond
	if err := c.Check(context.Background(), srv.URL, teamCallToken); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("Check error = %v", err)
	}
	if err := c.Leave(context.Background(), srv.URL, teamCallToken); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("Leave error = %v", err)
	}
}

func TestCheckAndLeaveDoNotFollowRedirects(t *testing.T) {
	for _, path := range []string{"/v1/seats", "/v1/seat"} {
		t.Run(path, func(t *testing.T) {
			var paths []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				http.Redirect(w, r, "/v1/landed", http.StatusFound)
			}))
			t.Cleanup(srv.Close)
			c := clientOn(srv)
			var err error
			if path == "/v1/seats" {
				err = c.Check(context.Background(), srv.URL, teamCallToken)
			} else {
				err = c.Leave(context.Background(), srv.URL, teamCallToken)
			}
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("error = %v", err)
			}
			assertTokenAbsent(t, err)
			if len(paths) != 1 || paths[0] != path {
				t.Fatalf("paths = %v", paths)
			}
		})
	}
}

func TestCheckRejectsAnOversizedAnswer(t *testing.T) {
	body := teamCallToken + strings.Repeat("x", 64*1024)
	srv, _ := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	})
	err := clientOn(srv).Check(context.Background(), srv.URL, teamCallToken)
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("error = %v", err)
	}
	assertTokenAbsent(t, err)
}

func TestCheckNilContextStillReachesTheServer(t *testing.T) {
	srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"seats":[]}`)
	})
	if err := clientOn(srv).Check(nil, srv.URL, teamCallToken); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 || (*got)[0].path != "/v1/seats" {
		t.Fatalf("requests = %+v", *got)
	}
}

func TestCheckCanceledContextIsUnreachable(t *testing.T) {
	srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"seats":[]}`)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := clientOn(srv).Check(ctx, srv.URL, teamCallToken)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("error = %v", err)
	}
	assertTokenAbsent(t, err)
	if len(*got) != 0 {
		t.Fatalf("canceled call sent %#v", *got)
	}
}

func TestCheckAndLeaveRejectABadBaseURL(t *testing.T) {
	c := NewClient()
	c.timeout = time.Second
	base := "http://" + teamCallToken + "@relay.example"
	if err := c.Check(context.Background(), base, teamCallToken); !errors.Is(err, ErrProtocol) {
		t.Fatalf("Check error = %v", err)
	} else {
		assertTokenAbsent(t, err)
	}
	if err := c.Leave(context.Background(), "http://[", teamCallToken); !errors.Is(err, ErrProtocol) {
		t.Fatalf("Leave error = %v", err)
	} else {
		assertTokenAbsent(t, err)
	}
}

func TestCheckReadErrorIsUnreachable(t *testing.T) {
	c := NewClient()
	c.Transport = failTrip{}
	c.timeout = time.Second
	err := c.Check(context.Background(), "http://relay.example", teamCallToken)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("error = %v", err)
	}
	assertTokenAbsent(t, err)
}

func TestCheckUsesTheBaseAndTheHeader(t *testing.T) {
	trip := &headerTrip{status: http.StatusOK, body: `{"seats":[]}`}
	c := NewClient()
	c.Transport = trip
	c.timeout = time.Second
	if err := c.Check(context.Background(), "https://relay.example", teamCallToken); err != nil {
		t.Fatal(err)
	}
	if trip.method != http.MethodGet || trip.gotURL != "https://relay.example/v1/seats" {
		t.Fatalf("request = %s %s", trip.method, trip.gotURL)
	}
	if trip.auth != "Bearer "+teamCallToken || trip.gotBody != "" {
		t.Fatalf("auth = %q body = %q", trip.auth, trip.gotBody)
	}
}

type headerTrip struct {
	status  int
	body    string
	method  string
	gotURL  string
	auth    string
	gotBody string
}

func (s *headerTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	s.method = r.Method
	s.gotURL = r.URL.String()
	s.auth = r.Header.Get("Authorization")
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		s.gotBody = string(raw)
	}
	return &http.Response{
		StatusCode: s.status,
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Header:     make(http.Header),
		Request:    r,
	}, nil
}

func TestCheckAndLeaveLeaveNoConnectionOpen(t *testing.T) {
	var mu sync.Mutex
	open := 0
	opened := 0
	var paths []string
	changed := make(chan struct{}, 1)
	notify := func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		auth := r.Header.Get("Authorization")
		mu.Unlock()
		if auth != "Bearer "+teamCallToken || r.ContentLength != 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/v1/seats":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"seats":[]}`)
		case "/v1/seat":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		mu.Lock()
		switch state {
		case http.StateNew:
			open++
			opened++
		case http.StateClosed:
			open--
		}
		mu.Unlock()
		notify()
	}
	srv.Start()
	t.Cleanup(srv.Close)

	c := NewClient()
	for i := 0; i < 3; i++ {
		if err := c.Check(context.Background(), srv.URL, teamCallToken); err != nil {
			t.Fatal(err)
		}
		if err := c.Leave(context.Background(), srv.URL, teamCallToken); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	seen := append([]string(nil), paths...)
	nOpened := opened
	mu.Unlock()
	if nOpened == 0 {
		t.Fatal("the server accepted no connection")
	}
	if len(seen) != 6 || seen[0] != "GET /v1/seats" || seen[1] != "DELETE /v1/seat" {
		t.Fatalf("paths = %v", seen)
	}

	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		mu.Lock()
		n := open
		mu.Unlock()
		if n == 0 {
			return
		}
		select {
		case <-changed:
		case <-deadline.C:
			mu.Lock()
			n = open
			mu.Unlock()
			if n == 0 {
				return
			}
			t.Fatalf("open connections = %d, want 0", n)
		}
	}
}

func TestClientLeavesNoConnectionOpen(t *testing.T) {
	var mu sync.Mutex
	open := 0
	opened := 0
	changed := make(chan struct{}, 1)
	notify := func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hello":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, helloBody(helloID, helloName))
		case "/v1/register":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, registerBody(regToken, helloID, helloName))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		mu.Lock()
		switch state {
		case http.StateNew:
			open++
			opened++
		case http.StateClosed:
			open--
		}
		mu.Unlock()
		notify()
	}
	srv.Start()
	t.Cleanup(srv.Close)

	c := NewClient()
	for i := 0; i < 4; i++ {
		if _, err := c.Hello(context.Background(), srv.URL); err != nil {
			t.Fatal(err)
		}
		if _, _, err := c.Register(context.Background(), srv.URL, "ab-cd", seatID, "Ada"); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	seen := opened
	mu.Unlock()
	if seen == 0 {
		t.Fatal("the server accepted no connection")
	}

	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		mu.Lock()
		n := open
		mu.Unlock()
		if n == 0 {
			return
		}
		select {
		case <-changed:
		case <-deadline.C:
			mu.Lock()
			n = open
			mu.Unlock()
			if n == 0 {
				return
			}
			t.Fatalf("open connections = %d, want 0", n)
		}
	}
}

func TestRenameSeatStatusAndRequest(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   error
	}{
		{"success", http.StatusOK, nil},
		{"unauthorized", http.StatusUnauthorized, ErrUnauthorized},
		{"bad request", http.StatusBadRequest, ErrRejected},
		{"unprocessable", http.StatusUnprocessableEntity, ErrRejected},
		{"unexpected", http.StatusInternalServerError, ErrProtocol},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) { w.WriteHeader(tc.status) })
			client := clientOn(srv)
			rename, ok := any(client).(interface {
				Rename(context.Context, string, string, string) error
			})
			if !ok {
				t.Fatal("Rename is missing")
			}
			err := rename.Rename(context.Background(), srv.URL, regToken, "Synthetic & Name")
			if !errors.Is(err, tc.want) {
				t.Fatalf("unexpected error class: %v", err)
			}
			if len(*got) != 1 || (*got)[0].method != http.MethodPut || (*got)[0].path != "/v1/seat" {
				t.Fatal("wrong request")
			}
			if (*got)[0].body != `{"name":"Synthetic & Name"}` {
				t.Fatal("wrong body")
			}
			if (*got)[0].header.Get("Authorization") != "Bearer "+regToken {
				t.Fatal("wrong authorization")
			}
		})
	}
}

func TestRenameSeatTransportErrors(t *testing.T) {
	client := NewClient()
	rename, ok := any(client).(interface {
		Rename(context.Context, string, string, string) error
	})
	if !ok {
		t.Fatal("Rename is missing")
	}
	if err := rename.Rename(context.Background(), "not a URL", regToken, "Synthetic"); !errors.Is(err, ErrProtocol) {
		t.Fatalf("invalid URL: %v", err)
	}
	client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") })
	if err := rename.Rename(context.Background(), "https://example.test", regToken, "Synthetic"); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("transport: %v", err)
	}
}
