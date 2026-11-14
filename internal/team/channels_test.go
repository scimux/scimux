package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	chOwner = "0123456789abcdef0123456789abcdef"
	chOther = "fedcba9876543210fedcba9876543210"
	chThird = "00112233445566778899aabbccddeeff"
	chChat  = "0123456789abcdef"
	chToken = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	chGen   = "0123456789abcdef"
	chTitle = `North <relay> "a"`
	chText  = "synthetic prompt"
)

func chEntry() json.RawMessage {
	return json.RawMessage(`{"v":1,"t":"user","text":"` + chText + `"}`)
}

func channelNames() []string {
	return []string{"seats", "put", "delete", "members", "append"}
}

func runChannel(c *Client, ctx context.Context, base, name string) error {
	switch name {
	case "seats":
		_, err := c.Seats(ctx, base, chToken)
		return err
	case "put":
		_, _, err := c.PutChannel(ctx, base, chToken, chOwner, chChat, chTitle)
		return err
	case "delete":
		return c.DeleteChannel(ctx, base, chToken, chOwner, chChat)
	case "members":
		return c.SetMembers(ctx, base, chToken, chOwner, chChat, []Member{
			{Seat: chOther, Access: "read"},
			{Seat: chThird, Access: "write"},
		})
	case "append":
		_, _, err := c.AppendMirror(ctx, base, chToken, chOwner, chChat, chGen, []MirrorEntry{
			{Key: "0/1", Body: chEntry()},
		})
		return err
	default:
		panic(name)
	}
}

func assertNoChannelSecrets(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, secret := range []string{chToken, chTitle, chText} {
		if strings.Contains(msg, secret) {
			t.Fatalf("error %q contains %q", msg, secret)
		}
	}
}

func secretEcho() string {
	return "echo " + chToken + " " + chTitle + " " + chText
}

func channelObject() string {
	title, _ := json.Marshal("Synthetic title")
	return `{"owner":"` + chOwner + `","chat":"` + chChat + `","title":` + string(title) +
		`,"generation":"` + chGen + `","created_at":"2026-11-14T09:30:00.000Z","access":"owner",` +
		`"mirror_last":1,"comments_last":0,"members":[{"seat":"` + chOther + `","access":"read"}]}`
}

func appendObject() string {
	return `{"generation":"` + chGen + `","results":[{"key":"0/1","seq":43,"duplicate":false}],"last":43}`
}

func seatsObject() string {
	return `{"seats":[{"id":"` + chOther + `","name":"Ada"},{"id":"` + chThird + `","name":"Bea"}]}`
}

func writeChannelSuccess(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/seats":
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, seatsObject())
	case r.Method == http.MethodDelete && r.URL.Path == "/v1/channels/"+chOwner+"/"+chChat:
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut && r.URL.Path == "/v1/channels/"+chOwner+"/"+chChat:
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, channelObject())
	case r.Method == http.MethodPut && r.URL.Path == "/v1/channels/"+chOwner+"/"+chChat+"/members":
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, minimalChannel())
	case r.Method == http.MethodPost && r.URL.Path == "/v1/channels/"+chOwner+"/"+chChat+"/mirror":
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, appendObject())
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, secretEcho())
	}
}

func minimalChannel() string {
	return `{"owner":"` + chOwner + `","chat":"` + chChat + `","title":"","generation":"` + chGen +
		`","created_at":"2026-11-14T09:30:00.000Z","access":"write","mirror_last":0,"comments_last":0,"members":[]}`
}

func TestChannelCallsSendTheRequest(t *testing.T) {
	srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, cap *capturedRequest) {
		if strings.Contains(r.URL.String(), chToken) || strings.Contains(cap.body, chToken) {
			t.Errorf("token left the Authorization header: %s %s", r.URL.String(), cap.body)
		}
		writeChannelSuccess(w, r)
	})
	c := clientOn(srv)
	for _, name := range channelNames() {
		t.Run(name, func(t *testing.T) {
			before := len(*got)
			if err := runChannel(c, context.Background(), srv.URL, name); err != nil {
				t.Fatal(err)
			}
			if len(*got) != before+1 {
				t.Fatalf("requests = %d, want one new request", len(*got)-before)
			}
			req := (*got)[len(*got)-1]
			if req.header.Get("Accept") != "application/json" {
				t.Fatalf("Accept = %q", req.header.Get("Accept"))
			}
			if req.header.Get("Authorization") != "Bearer "+chToken {
				t.Fatalf("Authorization = %q", req.header.Get("Authorization"))
			}
			switch name {
			case "seats":
				if req.method != http.MethodGet || req.path != "/v1/seats" || req.body != "" {
					t.Fatalf("request = %s %s %q", req.method, req.path, req.body)
				}
				if req.header.Get("Content-Type") != "" {
					t.Fatalf("Content-Type = %q", req.header.Get("Content-Type"))
				}
			case "put":
				if req.method != http.MethodPut || req.path != "/v1/channels/"+chOwner+"/"+chChat {
					t.Fatalf("request = %s %s", req.method, req.path)
				}
				if req.body != `{"title":"North <relay> \"a\""}` {
					t.Fatalf("body = %s", req.body)
				}
				if req.header.Get("Content-Type") != "application/json" {
					t.Fatalf("Content-Type = %q", req.header.Get("Content-Type"))
				}
			case "delete":
				if req.method != http.MethodDelete || req.path != "/v1/channels/"+chOwner+"/"+chChat || req.body != "" {
					t.Fatalf("request = %s %s %q", req.method, req.path, req.body)
				}
				if req.header.Get("Content-Type") != "" {
					t.Fatalf("Content-Type = %q", req.header.Get("Content-Type"))
				}
			case "members":
				want := `{"members":[{"seat":"` + chOther + `","access":"read"},{"seat":"` + chThird + `","access":"write"}]}`
				if req.method != http.MethodPut || req.path != "/v1/channels/"+chOwner+"/"+chChat+"/members" || req.body != want {
					t.Fatalf("request = %s %s %s", req.method, req.path, req.body)
				}
			case "append":
				want := `{"generation":"` + chGen + `","entries":[{"key":"0/1","body":{"v":1,"t":"user","text":"synthetic prompt"}}]}`
				if req.method != http.MethodPost || req.path != "/v1/channels/"+chOwner+"/"+chChat+"/mirror" || req.body != want {
					t.Fatalf("request = %s %s %s", req.method, req.path, req.body)
				}
			}
		})
	}
}

func TestChannelCallsReportCreatedOrderAndLast(t *testing.T) {
	var putStatus int
	srv, _ := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		switch r.URL.Path {
		case "/v1/seats":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, seatsObject())
		case "/v1/channels/" + chOwner + "/" + chChat:
			w.WriteHeader(putStatus)
			_, _ = io.WriteString(w, channelObject())
		case "/v1/channels/" + chOwner + "/" + chChat + "/members":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, minimalChannel())
		case "/v1/channels/" + chOwner + "/" + chChat + "/mirror":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, appendObject())
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	c := clientOn(srv)
	putStatus = http.StatusCreated
	gen, created, err := c.PutChannel(context.Background(), srv.URL, chToken, chOwner, chChat, "Synthetic title")
	if err != nil || !created || gen != chGen {
		t.Fatalf("create gen = %q created = %v err = %v", gen, created, err)
	}
	putStatus = http.StatusOK
	gen, created, err = c.PutChannel(context.Background(), srv.URL, chToken, chOwner, chChat, "Synthetic title")
	if err != nil || created || gen != chGen {
		t.Fatalf("update gen = %q created = %v err = %v", gen, created, err)
	}
	seats, err := c.Seats(context.Background(), srv.URL, chToken)
	if err != nil {
		t.Fatal(err)
	}
	if len(seats) != 2 || seats[0].ID != chOther || seats[0].Name != "Ada" || seats[1].ID != chThird || seats[1].Name != "Bea" {
		t.Fatalf("seats = %+v", seats)
	}
	emptySrv, _ := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"seats":[]}`)
	})
	empty, err := clientOn(emptySrv).Seats(context.Background(), emptySrv.URL, chToken)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty seats = %#v err = %v", empty, err)
	}
	if err := c.SetMembers(context.Background(), srv.URL, chToken, chOwner, chChat, nil); err != nil {
		t.Fatal(err)
	}
	gen, last, err := c.AppendMirror(context.Background(), srv.URL, chToken, chOwner, chChat, chGen, []MirrorEntry{{Key: "0/1", Body: chEntry()}})
	if err != nil || gen != chGen || last != 43 {
		t.Fatalf("append gen = %q last = %d err = %v", gen, last, err)
	}
	if err := c.DeleteChannel(context.Background(), srv.URL, chToken, chOwner, chChat); err != nil {
		t.Fatal(err)
	}
}

func TestChannelCallsMapEveryStatus(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, ErrUnauthorized},
		{http.StatusNotFound, ErrNoChannel},
		{http.StatusRequestEntityTooLarge, ErrTooLarge},
		{http.StatusUnprocessableEntity, ErrUnknownMember},
		{http.StatusInsufficientStorage, ErrQuota},
		{http.StatusBadRequest, ErrProtocol},
		{http.StatusForbidden, ErrProtocol},
		{http.StatusInternalServerError, ErrProtocol},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d", tc.status), func(t *testing.T) {
			srv, _ := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, secretEcho())
			})
			c := clientOn(srv)
			for _, name := range channelNames() {
				err := runChannel(c, context.Background(), srv.URL, name)
				if !errors.Is(err, tc.want) {
					t.Fatalf("%s error = %v, want %v", name, err, tc.want)
				}
				assertNoChannelSecrets(t, err)
			}
		})
	}
}

func TestChannelCallsRejectAMalformedSuccess(t *testing.T) {
	seats := []string{
		`not-json`,
		`{"seats":null}`,
		`{"seats":[{"id":"short","name":"Ada"}]}`,
		`{"seats":[{"id":"` + strings.ToUpper(chOther) + `","name":"Ada"}]}`,
		`{"seats":[{"id":"` + chOther + `","name":""}]}`,
		`{"seats":[{"id":"` + chOther + `","name":"` + strings.Repeat("a", 65) + `"}]}`,
		`{"seats":[{"id":"` + chOther + `","name":"Ada` + "\n" + `"}]}`,
		`{"seats":[{"id":"` + chOther + `","name":" Ada"}]}`,
		`{"seats":[{"id":"` + chOther + `","name":"Ada"}],"extra":1}`,
		`{"seats":[{"id":"` + chOther + `","name":"Ada","extra":1}]}`,
		seatsObject() + `{"x":1}`,
	}
	channels := []string{
		`not-json`,
		`{"owner":"short","chat":"` + chChat + `","title":"Synthetic title","generation":"` + chGen + `","created_at":"2026-11-14T09:30:00.000Z","access":"owner","mirror_last":0,"comments_last":0,"members":[]}`,
		`{"owner":"` + chOwner + `","chat":"bad chat","title":"Synthetic title","generation":"` + chGen + `","created_at":"2026-11-14T09:30:00.000Z","access":"owner","mirror_last":0,"comments_last":0,"members":[]}`,
		`{"owner":"` + chOwner + `","chat":"` + chChat + `","title":"bad\n","generation":"` + chGen + `","created_at":"2026-11-14T09:30:00.000Z","access":"owner","mirror_last":0,"comments_last":0,"members":[]}`,
		`{"owner":"` + chOwner + `","chat":"` + chChat + `","title":"bad` + "\xff" + `","generation":"` + chGen + `","created_at":"2026-11-14T09:30:00.000Z","access":"owner","mirror_last":0,"comments_last":0,"members":[]}`,
		`{"owner":"` + chOwner + `","chat":"` + chChat + `","title":"` + strings.Repeat("a", 201) + `","generation":"` + chGen + `","created_at":"2026-11-14T09:30:00.000Z","access":"owner","mirror_last":0,"comments_last":0,"members":[]}`,
		`{"owner":"` + chOwner + `","chat":"` + chChat + `","title":"Synthetic title","generation":"short","created_at":"2026-11-14T09:30:00.000Z","access":"owner","mirror_last":0,"comments_last":0,"members":[]}`,
		`{"owner":"` + chOwner + `","chat":"` + chChat + `","title":"Synthetic title","generation":"` + strings.ToUpper(chGen) + `","created_at":"2026-11-14T09:30:00.000Z","access":"owner","mirror_last":0,"comments_last":0,"members":[]}`,
		`{"owner":"` + chOwner + `","chat":"` + chChat + `","title":"Synthetic title","generation":"` + chGen + `","created_at":"2026-11-14T09:30:00Z","access":"owner","mirror_last":0,"comments_last":0,"members":[]}`,
		`{"owner":"` + chOwner + `","chat":"` + chChat + `","title":"Synthetic title","generation":"` + chGen + `","created_at":"yesterday","access":"owner","mirror_last":0,"comments_last":0,"members":[]}`,
		`{"owner":"` + chOwner + `","chat":"` + chChat + `","title":"Synthetic title","generation":"` + chGen + `","created_at":"2026-11-14T09:30:00.000Z","access":"admin","mirror_last":0,"comments_last":0,"members":[]}`,
		`{"owner":"` + chOwner + `","chat":"` + chChat + `","title":"Synthetic title","generation":"` + chGen + `","created_at":"2026-11-14T09:30:00.000Z","access":"owner","mirror_last":0,"comments_last":0,"members":null}`,
		`{"owner":"` + chOwner + `","chat":"` + chChat + `","title":"Synthetic title","generation":"` + chGen + `","created_at":"2026-11-14T09:30:00.000Z","access":"owner","mirror_last":0,"comments_last":0}`,
		`{"owner":"` + chOwner + `","chat":"` + chChat + `","title":"Synthetic title","generation":"` + chGen + `","created_at":"2026-11-14T09:30:00.000Z","access":"owner","mirror_last":0,"comments_last":0,"members":[{"seat":"short","access":"read"}]}`,
		`{"owner":"` + chOwner + `","chat":"` + chChat + `","title":"Synthetic title","generation":"` + chGen + `","created_at":"2026-11-14T09:30:00.000Z","access":"owner","mirror_last":0,"comments_last":0,"members":[{"seat":"` + chOther + `","access":"owner"}]}`,
		`{"owner":"` + chOwner + `","chat":"` + chChat + `","title":"Synthetic title","generation":"` + chGen + `","created_at":"2026-11-14T09:30:00.000Z","access":"owner","mirror_last":0,"comments_last":0,"members":[{"seat":"` + chOther + `","access":"read"},{"seat":"` + chOther + `","access":"write"}]}`,
		channelObject() + `{"x":1}`,
		`{"owner":"` + chOwner + `","chat":"` + chChat + `","title":"Synthetic title","generation":"` + chGen + `","created_at":"2026-11-14T09:30:00.000Z","access":"owner","mirror_last":0,"comments_last":0,"members":[],"extra":1}`,
	}
	appends := []string{
		`not-json`,
		`{"generation":"short","results":[{"key":"0/1","seq":1,"duplicate":false}],"last":1}`,
		`{"generation":"` + chGen + `","results":null,"last":1}`,
		`{"generation":"` + chGen + `","last":1}`,
		`{"generation":"` + chGen + `","results":[{"key":"0/1","seq":1,"duplicate":false}]}`,
		`{"generation":"` + chGen + `","results":[{"key":"bad key","seq":1,"duplicate":false}],"last":1}`,
		`{"generation":"` + chGen + `","results":[{"key":"0/2","seq":1,"duplicate":false}],"last":1}`,
		`{"generation":"` + chGen + `","results":[],"last":1}`,
		appendObject() + `{"x":1}`,
		`{"generation":"` + chGen + `","results":[{"key":"0/1","seq":1,"duplicate":false}],"last":1,"extra":1}`,
	}
	// net/http discards a 204 body, so only a RoundTripper can present one.
	for _, tc := range []struct {
		name string
		body string
	}{
		{"body", secretEcho()},
		{"blank", "\n"},
	} {
		t.Run("delete "+tc.name, func(t *testing.T) {
			trip := &headerTrip{status: http.StatusNoContent, body: tc.body}
			c := NewClient()
			c.Transport = trip
			c.timeout = time.Second
			err := c.DeleteChannel(context.Background(), "http://relay.example", chToken, chOwner, chChat)
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("error = %v", err)
			}
			assertNoChannelSecrets(t, err)
			if trip.method != http.MethodDelete || trip.gotBody != "" {
				t.Fatalf("request = %s %q", trip.method, trip.gotBody)
			}
		})
	}
	t.Run("delete ok", func(t *testing.T) {
		delSrv, delGot := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
			w.WriteHeader(http.StatusOK)
		})
		err := clientOn(delSrv).DeleteChannel(context.Background(), delSrv.URL, chToken, chOwner, chChat)
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("error = %v", err)
		}
		assertNoChannelSecrets(t, err)
		if len(*delGot) != 1 {
			t.Fatalf("requests = %d", len(*delGot))
		}
	})

	check := func(name string, bodies []string, status int, call func(*Client, string) error) {
		t.Helper()
		for i, body := range bodies {
			srv, _ := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, body)
			})
			err := call(clientOn(srv), srv.URL)
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("%s case %d error = %v body %s", name, i, err, body)
			}
			assertNoChannelSecrets(t, err)
		}
	}
	check("seats", seats, http.StatusOK, func(c *Client, base string) error {
		_, err := c.Seats(context.Background(), base, chToken)
		return err
	})
	check("put", channels, http.StatusCreated, func(c *Client, base string) error {
		_, _, err := c.PutChannel(context.Background(), base, chToken, chOwner, chChat, chTitle)
		return err
	})
	check("members", channels, http.StatusOK, func(c *Client, base string) error {
		return c.SetMembers(context.Background(), base, chToken, chOwner, chChat, nil)
	})
	check("append", appends, http.StatusOK, func(c *Client, base string) error {
		_, _, err := c.AppendMirror(context.Background(), base, chToken, chOwner, chChat, chGen, []MirrorEntry{{Key: "0/1", Body: chEntry()}})
		return err
	})
}

func TestChannelCallsRejectAnOversizedAnswer(t *testing.T) {
	for _, name := range channelNames() {
		t.Run(name, func(t *testing.T) {
			limit := 64 * 1024
			if name == "seats" {
				limit = 1 << 20
			}
			body := chToken + chTitle + chText + strings.Repeat("x", limit)
			srv, _ := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, body)
			})
			err := runChannel(clientOn(srv), context.Background(), srv.URL, name)
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("error = %v", err)
			}
			assertNoChannelSecrets(t, err)
		})
	}
	// A seats document larger than the other calls' bound is still accepted.
	wide := manySeats(2000)
	if len(wide) <= 64*1024 || len(wide) > 1<<20 {
		t.Fatalf("fixture length = %d", len(wide))
	}
	srv, _ := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, wide)
	})
	seats, err := clientOn(srv).Seats(context.Background(), srv.URL, chToken)
	if err != nil || len(seats) != 2000 || seats[0].Name != "Ada" || seats[1999].ID != fmt.Sprintf("%032x", 2000) {
		t.Fatalf("seats = %d err = %v", len(seats), err)
	}
}

func manySeats(n int) string {
	var b strings.Builder
	b.WriteString(`{"seats":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":"%032x","name":"Ada"}`, i+1)
	}
	b.WriteString(`]}`)
	return b.String()
}

func TestChannelCallsDoNotFollowRedirects(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		http.Redirect(w, r, "/v1/landed", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	c := clientOn(srv)
	for _, name := range channelNames() {
		err := runChannel(c, context.Background(), srv.URL, name)
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("%s error = %v", name, err)
		}
		assertNoChannelSecrets(t, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != len(channelNames()) {
		t.Fatalf("paths = %v", paths)
	}
	for _, p := range paths {
		if p == "/v1/landed" {
			t.Fatalf("followed a redirect: %v", paths)
		}
	}
}

func TestChannelCallsTimeOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The server cancels the request only once the body has been read.
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	c := clientOn(srv)
	c.timeout = 30 * time.Millisecond
	for _, name := range channelNames() {
		err := runChannel(c, context.Background(), srv.URL, name)
		if !errors.Is(err, ErrUnreachable) {
			t.Fatalf("%s error = %v", name, err)
		}
		assertNoChannelSecrets(t, err)
	}
}

func TestChannelCallsHonorACanceledContext(t *testing.T) {
	srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		writeChannelSuccess(w, r)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := clientOn(srv)
	for _, name := range channelNames() {
		err := runChannel(c, ctx, srv.URL, name)
		if !errors.Is(err, ErrUnreachable) {
			t.Fatalf("%s error = %v", name, err)
		}
		assertNoChannelSecrets(t, err)
	}
	if len(*got) != 0 {
		t.Fatalf("canceled calls sent %#v", *got)
	}
}

func TestChannelCallsNilContextStillReachesTheServer(t *testing.T) {
	srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		writeChannelSuccess(w, r)
	})
	c := clientOn(srv)
	for _, name := range channelNames() {
		if err := runChannel(c, nil, srv.URL, name); err != nil {
			t.Fatalf("%s error = %v", name, err)
		}
	}
	if len(*got) != len(channelNames()) {
		t.Fatalf("requests = %d", len(*got))
	}
}

func TestChannelCallsLeaveNoConnectionOpen(t *testing.T) {
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
		if r.Header.Get("Authorization") != "Bearer "+chToken {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.URL.Path == "/v1/channels/"+chOwner+"/"+chChat && r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path == "/v1/channels/"+chOwner+"/"+chChat && r.Method == http.MethodPut {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, channelObject())
			return
		}
		writeChannelSuccess(w, r)
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
	for i := 0; i < 2; i++ {
		for _, name := range channelNames() {
			if err := runChannel(c, context.Background(), srv.URL, name); err != nil {
				t.Fatal(err)
			}
		}
	}
	mu.Lock()
	nOpened := opened
	mu.Unlock()
	if nOpened == 0 {
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
			if n != 0 {
				t.Fatalf("open connections = %d, want 0", n)
			}
			return
		}
	}
}

func TestChannelCallsAreUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := srv.URL
	srv.Close()
	c := NewClient()
	c.timeout = time.Second
	for _, name := range channelNames() {
		err := runChannel(c, context.Background(), base, name)
		if !errors.Is(err, ErrUnreachable) {
			t.Fatalf("%s error = %v", name, err)
		}
		assertNoChannelSecrets(t, err)
	}
	c.Transport = failTrip{}
	for _, name := range channelNames() {
		err := runChannel(c, context.Background(), "http://relay.example", name)
		if !errors.Is(err, ErrUnreachable) {
			t.Fatalf("%s read error = %v", name, err)
		}
		assertNoChannelSecrets(t, err)
	}
	c.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed " + chToken)
	})
	for _, name := range channelNames() {
		err := runChannel(c, context.Background(), "http://relay.example", name)
		if !errors.Is(err, ErrUnreachable) {
			t.Fatalf("%s dial error = %v", name, err)
		}
		assertNoChannelSecrets(t, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestChannelCallsRejectABadBaseURL(t *testing.T) {
	c := NewClient()
	c.timeout = time.Second
	bases := []string{
		"http://[",
		"/relative",
		"ftp://relay.example/join",
		"http:///no-host",
		"http://ada:secret@relay.example",
	}
	for _, base := range bases {
		for _, name := range channelNames() {
			err := runChannel(c, context.Background(), base, name)
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("%s %s error = %v", name, base, err)
			}
			assertNoChannelSecrets(t, err)
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), base) {
				t.Fatalf("%s quotes the input: %v", base, err)
			}
		}
	}
}

func TestChannelCallsRejectInvalidOwnerAndChat(t *testing.T) {
	srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		t.Errorf("request sent: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	})
	c := clientOn(srv)
	owners := []string{
		"",
		"0123",
		strings.Repeat("a", 31),
		strings.Repeat("a", 33),
		strings.ToUpper(chOwner),
		chOwner[:31] + "g",
	}
	chats := []string{
		"",
		strings.Repeat("a", 65),
		"bad chat",
		"bad/chat",
		"bad.chat",
		"bad:chat",
		"bad chat",
	}
	calls := []func(owner, chat string) error{
		func(owner, chat string) error {
			_, _, err := c.PutChannel(context.Background(), srv.URL, chToken, owner, chat, chTitle)
			return err
		},
		func(owner, chat string) error {
			return c.DeleteChannel(context.Background(), srv.URL, chToken, owner, chat)
		},
		func(owner, chat string) error {
			return c.SetMembers(context.Background(), srv.URL, chToken, owner, chat, nil)
		},
		func(owner, chat string) error {
			_, _, err := c.AppendMirror(context.Background(), srv.URL, chToken, owner, chat, chGen, []MirrorEntry{{Key: "0/1", Body: chEntry()}})
			return err
		},
	}
	for _, owner := range owners {
		for _, call := range calls {
			err := call(owner, chChat)
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("owner %q error = %v", owner, err)
			}
			assertNoChannelSecrets(t, err)
		}
	}
	for _, chat := range chats {
		for _, call := range calls {
			err := call(chOwner, chat)
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("chat %q error = %v", chat, err)
			}
			assertNoChannelSecrets(t, err)
		}
	}
	chat64 := strings.Repeat("b", 64)
	ok, _ := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		if r.URL.Path != "/v1/channels/"+chOwner+"/"+chat64 {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	if err := clientOn(ok).DeleteChannel(context.Background(), ok.URL, chToken, chOwner, chat64); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 0 {
		t.Fatalf("invalid paths sent %d requests", len(*got))
	}
}

func TestAppendMirrorRejectsEntriesBeforeARequest(t *testing.T) {
	srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		t.Errorf("request sent: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	})
	c := clientOn(srv)
	cases := [][]MirrorEntry{
		nil,
		{},
		{{Key: "bad key", Body: chEntry()}},
		{{Key: "", Body: chEntry()}},
		{{Key: strings.Repeat("a", 201), Body: chEntry()}},
		{{Key: "0/1", Body: json.RawMessage("{")}},
		make([]MirrorEntry, 257),
	}
	for i := range cases[len(cases)-1] {
		cases[len(cases)-1][i] = MirrorEntry{Key: fmt.Sprintf("k%d", i), Body: chEntry()}
	}
	dotted := []MirrorEntry{{Key: "a.b_c:d/e-f", Body: json.RawMessage(`{"ok":true}`)}}
	full := make([]MirrorEntry, 256)
	fullResults := make([]string, len(full))
	for i := range full {
		key := fmt.Sprintf("k%d", i)
		full[i] = MirrorEntry{Key: key, Body: json.RawMessage(`{"n":1}`)}
		fullResults[i] = `{"key":"` + key + `","seq":1,"duplicate":false}`
	}
	for i, entries := range cases {
		_, _, err := c.AppendMirror(context.Background(), srv.URL, chToken, chOwner, chChat, chGen, entries)
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("case %d error = %v", i, err)
		}
		assertNoChannelSecrets(t, err)
	}
	ok, one := serveTeam(t, func(w http.ResponseWriter, r *http.Request, cap *capturedRequest) {
		if cap.body != `{"generation":"`+chGen+`","entries":[{"key":"a.b_c:d/e-f","body":{"ok":true}}]}` {
			t.Errorf("body = %s", cap.body)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"generation":"`+chGen+`","results":[{"key":"a.b_c:d/e-f","seq":1,"duplicate":true}],"last":0}`)
	})
	gen, last, err := clientOn(ok).AppendMirror(context.Background(), ok.URL, chToken, chOwner, chChat, chGen, dotted)
	if err != nil || gen != chGen || last != 0 {
		t.Fatalf("dotted gen = %q last = %d err = %v", gen, last, err)
	}
	wide, wideGot := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"generation":"`+chGen+`","results":[`+strings.Join(fullResults, ",")+`],"last":256}`)
	})
	gen, last, err = clientOn(wide).AppendMirror(context.Background(), wide.URL, chToken, chOwner, chChat, chGen, full)
	if err != nil || gen != chGen || last != 256 {
		t.Fatalf("full gen = %q last = %d err = %v", gen, last, err)
	}
	if len(*got) != 0 || len(*one) != 1 || len(*wideGot) != 1 {
		t.Fatalf("rejected requests = %d dotted = %d full = %d", len(*got), len(*one), len(*wideGot))
	}
}

func TestChannelMarshalFailureSendsNothing(t *testing.T) {
	orig := marshalChannelBody
	t.Cleanup(func() { marshalChannelBody = orig })
	marshalChannelBody = func(any) ([]byte, error) {
		return nil, errors.New("marshal failed " + chToken + " " + chTitle + " " + chText)
	}
	srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		t.Errorf("request sent: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	})
	c := clientOn(srv)
	for _, name := range []string{"put", "members", "append"} {
		err := runChannel(c, context.Background(), srv.URL, name)
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("%s error = %v", name, err)
		}
		assertNoChannelSecrets(t, err)
	}
	if len(*got) != 0 {
		t.Fatalf("requests = %d", len(*got))
	}
}

func TestSetMembersEncodesAnEmptyList(t *testing.T) {
	srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, channelObject())
	})
	if err := clientOn(srv).SetMembers(context.Background(), srv.URL, chToken, chOwner, chChat, nil); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 || (*got)[0].body != `{"members":[]}` {
		t.Fatalf("request = %+v", *got)
	}
}

func TestChannelCallsAcceptAnHTTPSBase(t *testing.T) {
	trip := &headerTrip{status: http.StatusOK, body: seatsObject()}
	c := NewClient()
	c.Transport = trip
	c.timeout = time.Second
	seats, err := c.Seats(context.Background(), "https://relay.example", chToken)
	if err != nil || len(seats) != 2 {
		t.Fatalf("seats = %+v err = %v", seats, err)
	}
	if trip.method != http.MethodGet || trip.gotURL != "https://relay.example/v1/seats" || trip.auth != "Bearer "+chToken {
		t.Fatalf("request = %s %s auth %q", trip.method, trip.gotURL, trip.auth)
	}
}

func TestAppendMirrorSendsTheGeneration(t *testing.T) {
	const gen = "fedcba9876543210"
	srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, cap *capturedRequest) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, appendObject())
		if cap.body != `{"generation":"`+gen+`","entries":[{"key":"0/1","body":{"v":1,"t":"user","text":"synthetic prompt"}}]}` {
			t.Errorf("body = %s", cap.body)
		}
	})
	gotGen, last, err := clientOn(srv).AppendMirror(context.Background(), srv.URL, chToken, chOwner, chChat, gen, []MirrorEntry{{Key: "0/1", Body: chEntry()}})
	if err != nil || gotGen != chGen || last != 43 {
		t.Fatalf("append gen = %q last = %d err = %v", gotGen, last, err)
	}
	if len(*got) != 1 {
		t.Fatalf("requests = %d", len(*got))
	}
}

func TestAppendMirrorRejectsABadGeneration(t *testing.T) {
	srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		t.Errorf("request sent: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	})
	c := clientOn(srv)
	entries := []MirrorEntry{{Key: "0/1", Body: chEntry()}}
	for _, gen := range []string{
		"",
		"0123456789abcde",
		"0123456789abcdef0",
		strings.ToUpper(chGen),
		"0123456789abcdeg",
	} {
		_, _, err := c.AppendMirror(context.Background(), srv.URL, chToken, chOwner, chChat, gen, entries)
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("generation %q error = %v", gen, err)
		}
		assertNoChannelSecrets(t, err)
	}
	if len(*got) != 0 {
		t.Fatalf("requests = %d", len(*got))
	}
}

func TestChannelCallsMapAStaleGeneration(t *testing.T) {
	srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, secretEcho())
	})
	c := clientOn(srv)
	for _, name := range channelNames() {
		err := runChannel(c, context.Background(), srv.URL, name)
		if !errors.Is(err, ErrStaleGeneration) {
			t.Fatalf("%s error = %v", name, err)
		}
		assertNoChannelSecrets(t, err)
	}
	if len(*got) != len(channelNames()) {
		t.Fatalf("requests = %d", len(*got))
	}
}
