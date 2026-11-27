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
	"reflect"
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
		return deleteWithGeneration(c, ctx, base, chToken, chOwner, chChat, chGen)
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
	if err := deleteWithGeneration(c, context.Background(), srv.URL, chToken, chOwner, chChat, chGen); err != nil {
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
			err := deleteWithGeneration(c, context.Background(), "http://relay.example", chToken, chOwner, chChat, chGen)
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
		err := deleteWithGeneration(clientOn(delSrv), context.Background(), delSrv.URL, chToken, chOwner, chChat, chGen)
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
			return deleteWithGeneration(c, context.Background(), srv.URL, chToken, owner, chat, chGen)
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
	if err := deleteWithGeneration(clientOn(ok), context.Background(), ok.URL, chToken, chOwner, chat64, chGen); err != nil {
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

func guestChannel(owner, chat, access string, mirror, comments int) string {
	return `{"owner":"` + owner + `","chat":"` + chat + `","title":"Synthetic title","generation":"` + chGen +
		`","created_at":"2026-11-14T09:30:00.000Z","access":"` + access + `","mirror_last":` + fmt.Sprintf("%d", mirror) +
		`,"comments_last":` + fmt.Sprintf("%d", comments) + `}`
}

func mirrorLine(seq int, key, body string) string {
	return `{"seq":` + fmt.Sprintf("%d", seq) + `,"key":"` + key + `","author":"` + chOther +
		`","at":"2026-11-14T09:30:00.000Z","body":` + body + `}`
}

func mirrorPage(entries string, last int, more bool) string {
	flag := "false"
	if more {
		flag = "true"
	}
	return `{"generation":"` + chGen + `","entries":[` + entries + `],"last":` + fmt.Sprintf("%d", last) + `,"more":` + flag + `}`
}

func TestServerAnswersAcceptUnknownFields(t *testing.T) {
	seatBody := `{"seats":[{"id":"` + chOther + `","name":"Ada","extra":1}],"extra":1}`
	channelBody := `{"owner":"` + chOwner + `","chat":"` + chChat + `","title":"Synthetic title","generation":"` + chGen + `","created_at":"2026-11-14T09:30:00.000Z","access":"owner","mirror_last":0,"comments_last":0,"members":[{"seat":"` + chOther + `","access":"read","extra":1}],"extra":1}`
	appendBody := `{"generation":"` + chGen + `","results":[{"key":"0/1","seq":1,"duplicate":false,"extra":1}],"last":1,"extra":1}`
	listBody := `{"channels":[` + channelBody + `],"extra":1}`
	pageBody := `{"generation":"` + chGen + `","entries":[{"seq":1,"key":"0/1","author":"` + chOther + `","at":"2026-11-14T09:30:00.000Z","body":{"v":1},"extra":1}],"last":1,"more":false,"extra":1}`
	srv, _ := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		body := seatBody
		status := http.StatusOK
		switch {
		case r.URL.Path == "/v1/channels":
			body = listBody
		case strings.HasSuffix(r.URL.Path, "/members"):
			body = channelBody
		case strings.HasSuffix(r.URL.Path, "/mirror") && r.Method == http.MethodGet:
			body = pageBody
		case strings.HasSuffix(r.URL.Path, "/mirror"):
			body = appendBody
		case r.URL.Path != "/v1/seats":
			status = http.StatusCreated
			body = channelBody
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
	c := clientOn(srv)
	seats, err := c.Seats(context.Background(), srv.URL, chToken)
	if err != nil || len(seats) != 1 || seats[0].ID != chOther || seats[0].Name != "Ada" {
		t.Fatalf("seats = %+v err = %v", seats, err)
	}
	gen, created, err := c.PutChannel(context.Background(), srv.URL, chToken, chOwner, chChat, "Synthetic title")
	if err != nil || !created || gen != chGen {
		t.Fatalf("put gen = %q created = %v err = %v", gen, created, err)
	}
	if err := c.SetMembers(context.Background(), srv.URL, chToken, chOwner, chChat, []Member{{Seat: chOther, Access: "read"}}); err != nil {
		t.Fatal(err)
	}
	gotGen, last, err := c.AppendMirror(context.Background(), srv.URL, chToken, chOwner, chChat, chGen, []MirrorEntry{{Key: "0/1", Body: json.RawMessage(`{"v":1}`)}})
	if err != nil || gotGen != chGen || last != 1 {
		t.Fatalf("append gen = %q last = %d err = %v", gotGen, last, err)
	}
	list, err := c.Channels(context.Background(), srv.URL, chToken, false)
	if err != nil || len(list) != 1 || list[0].Chat != chChat || len(list[0].Members) != 1 || list[0].Members[0].Seat != chOther {
		t.Fatalf("channels = %+v err = %v", list, err)
	}
	page, err := c.ReadMirror(context.Background(), srv.URL, chToken, chOwner, chChat, 0)
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Seq != 1 || string(page.Entries[0].Body) != `{"v":1}` {
		t.Fatalf("page = %+v err = %v", page, err)
	}
}

func TestChannelsListsTheThreeAccessValues(t *testing.T) {
	body := `{"channels":[` + channelObject() + `,` + guestChannel(chOther, chChat, "read", 2, 0) + `,` + guestChannel(chThird, "fedcba9876543210", "write", 0, 4) + `]}`
	var query string
	srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, cap *capturedRequest) {
		query = r.URL.RawQuery
		if strings.Contains(r.URL.String(), chToken) || strings.Contains(cap.body, chToken) {
			t.Errorf("token left the header: %s", r.URL.String())
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	})
	list, err := clientOn(srv).Channels(context.Background(), srv.URL, chToken, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 || (*got)[0].method != http.MethodGet || (*got)[0].path != "/v1/channels" || (*got)[0].body != "" || query != "" {
		t.Fatalf("request = %+v query %q", *got, query)
	}
	req := (*got)[0]
	if req.header.Get("Authorization") != "Bearer "+chToken || req.header.Get("Content-Type") != "" {
		t.Fatalf("headers = %v", req.header)
	}
	if len(list) != 3 {
		t.Fatalf("channels = %+v", list)
	}
	if list[0].Owner != chOwner || list[0].Chat != chChat || list[0].Access != "owner" || list[0].MirrorLast != 1 || list[0].CommentsLast != 0 || list[0].Generation != chGen || list[0].Title != "Synthetic title" {
		t.Fatalf("owner = %+v", list[0])
	}
	if len(list[0].Members) != 1 || list[0].Members[0].Seat != chOther || list[0].Members[0].Access != "read" {
		t.Fatalf("members = %+v", list[0].Members)
	}
	if list[1].Owner != chOther || list[1].Access != "read" || list[1].MirrorLast != 2 || list[1].Members != nil {
		t.Fatalf("read = %+v", list[1])
	}
	if list[2].Owner != chThird || list[2].Chat != "fedcba9876543210" || list[2].Access != "write" || list[2].CommentsLast != 4 || list[2].Members != nil {
		t.Fatalf("write = %+v", list[2])
	}
}

func TestChannelsListsNothing(t *testing.T) {
	srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"channels":[]}`)
	})
	list, err := clientOn(srv).Channels(context.Background(), srv.URL, chToken, false)
	if err != nil || list == nil || len(list) != 0 {
		t.Fatalf("list = %#v err = %v", list, err)
	}
	if len(*got) != 1 {
		t.Fatalf("requests = %d", len(*got))
	}
}

func TestReadMirrorReadsAPage(t *testing.T) {
	kept := `{"v":1, "t":"user"}`
	page := mirrorPage(mirrorLine(1, "0/1", kept)+","+mirrorLine(2, "a.b_c:d/e-f", `{"v":1,"t":"source","reason":"clear"}`), 2, false)
	var query string
	srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, cap *capturedRequest) {
		query = r.URL.RawQuery
		if strings.Contains(r.URL.String(), chToken) || strings.Contains(cap.body, chToken) {
			t.Errorf("token left the header: %s", r.URL.String())
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, page)
	})
	gotPage, err := clientOn(srv).ReadMirror(context.Background(), srv.URL, chToken, chOwner, chChat, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 || (*got)[0].method != http.MethodGet || (*got)[0].path != "/v1/channels/"+chOwner+"/"+chChat+"/mirror" || (*got)[0].body != "" || query != "after=0" {
		t.Fatalf("request = %+v query %q", *got, query)
	}
	if (*got)[0].header.Get("Authorization") != "Bearer "+chToken || (*got)[0].header.Get("Content-Type") != "" {
		t.Fatalf("headers = %v", (*got)[0].header)
	}
	if gotPage.Generation != chGen || gotPage.More || gotPage.Last != 2 || len(gotPage.Entries) != 2 {
		t.Fatalf("page = %+v", gotPage)
	}
	if string(gotPage.Entries[0].Body) != kept || gotPage.Entries[0].Seq != 1 || gotPage.Entries[0].Key != "0/1" || gotPage.Entries[0].Author != chOther {
		t.Fatalf("first = %+v", gotPage.Entries[0])
	}
	if gotPage.Entries[1].Seq != 2 || gotPage.Entries[1].Key != "a.b_c:d/e-f" {
		t.Fatalf("second = %+v", gotPage.Entries[1])
	}
	moreBody := mirrorPage(mirrorLine(8, "0/8", `{"v":1}`), 40, true)
	moreSrv, _ := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		if r.URL.RawQuery != "after=7" {
			t.Errorf("query = %q", r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, moreBody)
	})
	more, err := clientOn(moreSrv).ReadMirror(context.Background(), moreSrv.URL, chToken, chOwner, chChat, 7)
	if err != nil || !more.More || more.Last != 40 || len(more.Entries) != 1 || more.Entries[0].Seq != 8 {
		t.Fatalf("more = %+v err = %v", more, err)
	}
	emptyBody := mirrorPage("", 5, false)
	emptySrv, _ := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, emptyBody)
	})
	empty, err := clientOn(emptySrv).ReadMirror(context.Background(), emptySrv.URL, chToken, chOwner, chChat, 5)
	if err != nil || empty.More || empty.Last != 5 || empty.Entries == nil || len(empty.Entries) != 0 {
		t.Fatalf("empty = %+v err = %v", empty, err)
	}
}

func TestGuestCallsMapEveryStatus(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, ErrUnauthorized},
		{http.StatusNotFound, ErrNoChannel},
		{http.StatusRequestEntityTooLarge, ErrTooLarge},
		{http.StatusUnprocessableEntity, ErrUnknownMember},
		{http.StatusInsufficientStorage, ErrQuota},
		{http.StatusConflict, ErrStaleGeneration},
		{http.StatusBadRequest, ErrProtocol},
		{http.StatusForbidden, ErrProtocol},
		{http.StatusInternalServerError, ErrProtocol},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d", tc.status), func(t *testing.T) {
			srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, secretEcho())
			})
			c := clientOn(srv)
			_, err := c.Channels(context.Background(), srv.URL, chToken, false)
			if !errors.Is(err, tc.want) {
				t.Fatalf("channels error = %v, want %v", err, tc.want)
			}
			assertNoChannelSecrets(t, err)
			_, err = c.ReadMirror(context.Background(), srv.URL, chToken, chOwner, chChat, 1)
			if !errors.Is(err, tc.want) {
				t.Fatalf("mirror error = %v, want %v", err, tc.want)
			}
			assertNoChannelSecrets(t, err)
			if len(*got) != 2 {
				t.Fatalf("requests = %d", len(*got))
			}
		})
	}
}

func TestGuestCallsRejectWhatIsNotTheProtocol(t *testing.T) {
	ownerWithWrite := `{"owner":"` + chOwner + `","chat":"` + chChat + `","title":"Synthetic title","generation":"` + chGen + `","created_at":"2026-11-14T09:30:00.000Z","access":"owner","mirror_last":0,"comments_last":0,"members":[{"seat":"` + chOther + `","access":"write"}]}`
	ownerNullMembers := `{"owner":"` + chOwner + `","chat":"` + chChat + `","title":"Synthetic title","generation":"` + chGen + `","created_at":"2026-11-14T09:30:00.000Z","access":"owner","mirror_last":0,"comments_last":0,"members":null}`
	lists := []string{
		`not-json ` + chToken,
		`{"channels":null}`,
		`{"channels":`,
		`{}`,
		`{"channels":[]}` + `{"x":1}`,
		`{"channels":["no"]}`,
		`{"channels":[` + guestChannel(chOwner, "bad chat", "read", 0, 0) + `]}`,
		`{"channels":[` + guestChannel("short", chChat, "read", 0, 0) + `]}`,
		`{"channels":[` + guestChannel(strings.ToUpper(chOwner), chChat, "read", 0, 0) + `]}`,
		`{"channels":[{"owner":"` + chOwner + `","chat":"` + chChat + `","title":"bad` + "\n" + `","generation":"` + chGen + `","created_at":"2026-11-14T09:30:00.000Z","access":"read","mirror_last":0,"comments_last":0}]}`,
		`{"channels":[{"owner":"` + chOwner + `","chat":"` + chChat + `","title":"bad` + "\xff" + `","generation":"` + chGen + `","created_at":"2026-11-14T09:30:00.000Z","access":"read","mirror_last":0,"comments_last":0}]}`,
		`{"channels":[` + strings.Replace(guestChannel(chOwner, chChat, "read", 0, 0), chGen, "short", 1) + `]}`,
		`{"channels":[` + strings.Replace(guestChannel(chOwner, chChat, "read", 0, 0), chGen, strings.ToUpper(chGen), 1) + `]}`,
		`{"channels":[{"owner":"` + chOwner + `","chat":"` + chChat + `","title":"Synthetic title","generation":"` + chGen + `","created_at":"yesterday","access":"read","mirror_last":0,"comments_last":0}]}`,
		`{"channels":[{"owner":"` + chOwner + `","chat":"` + chChat + `","title":"Synthetic title","generation":"` + chGen + `","created_at":"2026-11-14T09:30:00.000Z","access":"admin","mirror_last":0,"comments_last":0}]}`,
		`{"channels":[{"owner":"` + chOwner + `","chat":"` + chChat + `","title":"Synthetic title","generation":"` + chGen + `","created_at":"2026-11-14T09:30:00.000Z","access":"read","mirror_last":"1","comments_last":0}]}`,
		`{"channels":[` + guestChannel(chOwner, chChat, "read", 0, 0) + `,"extra"]}`,
		`{"channels":[` + ownerNullMembers + `]}`,
		`{"channels":[` + guestChannel(chOwner, chChat, "owner", 0, 0) + `]}`,
		`{"channels":[` + guestChannel(chOther, chChat, "read", 0, 0)[:len(guestChannel(chOther, chChat, "read", 0, 0))-1] + `,"members":[]}]}`,
		`{"channels":[` + guestChannel(chOther, chChat, "write", 0, 0)[:len(guestChannel(chOther, chChat, "write", 0, 0))-1] + `,"members":null}]}`,
		`{"channels":[{"owner":"` + chOwner + `","chat":"` + chChat + `","title":"Synthetic title","generation":"` + chGen + `","created_at":"2026-11-14T09:30:00.000Z","access":"owner","mirror_last":0,"comments_last":0,"members":[{"seat":"short","access":"read"}]}]}`,
		`{"channels":[{"owner":"` + chOwner + `","chat":"` + chChat + `","title":"Synthetic title","generation":"` + chGen + `","created_at":"2026-11-14T09:30:00.000Z","access":"owner","mirror_last":0,"comments_last":0,"members":[{"seat":"` + chOther + `","access":"owner"}]}]}`,
		`{"channels":[{"owner":"` + chOwner + `","chat":"` + chChat + `","title":"Synthetic title","generation":"` + chGen + `","created_at":"2026-11-14T09:30:00.000Z","access":"owner","mirror_last":0,"comments_last":0,"members":[{"seat":"` + chOther + `","access":"read"},{"seat":"` + chOther + `","access":"write"}]}]}`,
		`{"channels":[{"owner":"` + chOwner + `","chat":"` + strings.Repeat("a", 65) + `","title":"Synthetic title","generation":"` + chGen + `","created_at":"2026-11-14T09:30:00.000Z","access":"read","mirror_last":0,"comments_last":0}]}`,
		`{"channels":[{"owner":"` + chOwner + `","chat":"` + chChat + `","title":"` + strings.Repeat("a", 201) + `","generation":"` + chGen + `","created_at":"2026-11-14T09:30:00.000Z","access":"read","mirror_last":0,"comments_last":0}]}`,
	}
	for i, body := range lists {
		srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, body)
		})
		_, err := clientOn(srv).Channels(context.Background(), srv.URL, chToken, false)
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("list %d error = %v body %s", i, err, body)
		}
		assertNoChannelSecrets(t, err)
		if len(*got) != 1 {
			t.Fatalf("list %d requests = %d", i, len(*got))
		}
	}
	// A write member on an owned channel is accepted, and so is an empty member list.
	okBodies := []string{
		`{"channels":[` + ownerWithWrite + `]}`,
		`{"channels":[{"owner":"` + chOwner + `","chat":"` + chChat + `","title":"","generation":"` + chGen + `","created_at":"2026-11-14T09:30:00.000Z","access":"owner","mirror_last":0,"comments_last":0,"members":[]}]}`,
	}
	for i, body := range okBodies {
		srv, _ := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, body)
		})
		list, err := clientOn(srv).Channels(context.Background(), srv.URL, chToken, false)
		if err != nil || len(list) != 1 || list[0].Access != "owner" {
			t.Fatalf("ok %d list = %+v err = %v", i, list, err)
		}
		if i == 0 && (len(list[0].Members) != 1 || list[0].Members[0].Access != "write" || list[0].Members[0].Seat != chOther) {
			t.Fatalf("write member = %+v", list[0].Members)
		}
		if i == 1 && (list[0].Members == nil || len(list[0].Members) != 0 || list[0].Title != "") {
			t.Fatalf("empty members = %+v", list[0])
		}
	}

	lines := []string{
		`not-json ` + chToken,
		`{"generation":"` + chGen + `","entries":null,"last":0,"more":false}`,
		`{"generation":"` + chGen + `","last":0,"more":false}`,
		`{"generation":"` + chGen + `","entries":[],"more":false}`,
		`{"generation":"` + chGen + `","entries":[],"last":0}`,
		`{"generation":"short","entries":[],"last":0,"more":false}`,
		`{"generation":"` + strings.ToUpper(chGen) + `","entries":[],"last":0,"more":false}`,
		mirrorPage("", 1, true),
		mirrorPage(mirrorLine(1, "0/1", `{"v":1}`), 1, true),
		mirrorPage(mirrorLine(1, "0/1", `{"v":1}`), 0, false),
		mirrorPage(mirrorLine(1, "0/1", `{"v":1}`)+","+mirrorLine(1, "0/2", `{"v":1}`), 1, false),
		mirrorPage(mirrorLine(1, "0/1", `{"v":1}`)+","+mirrorLine(3, "0/3", `{"v":1}`)+","+mirrorLine(2, "0/2", `{"v":1}`), 2, false),
		mirrorPage(mirrorLine(5, "0/5", `{"v":1}`), 4, true),
		`{"generation":"` + chGen + `","entries":[{"seq":"1","key":"0/1","author":"` + chOther + `","at":"2026-11-14T09:30:00.000Z","body":{"v":1}}],"last":1,"more":false}`,
		`{"generation":"` + chGen + `","entries":[{"seq":1,"key":"bad key","author":"` + chOther + `","at":"2026-11-14T09:30:00.000Z","body":{"v":1}}],"last":1,"more":false}`,
		`{"generation":"` + chGen + `","entries":[{"seq":1,"key":"0/1","author":"short","at":"2026-11-14T09:30:00.000Z","body":{"v":1}}],"last":1,"more":false}`,
		`{"generation":"` + chGen + `","entries":[{"seq":1,"key":"0/1","author":"` + strings.ToUpper(chOther) + `","at":"2026-11-14T09:30:00.000Z","body":{"v":1}}],"last":1,"more":false}`,
		`{"generation":"` + chGen + `","entries":[{"seq":1,"key":"0/1","author":"` + chOther + `","at":"yesterday","body":{"v":1}}],"last":1,"more":false}`,
		`{"generation":"` + chGen + `","entries":[{"seq":1,"key":"0/1","author":"` + chOther + `","at":"2026-11-14T09:30:00.000Z"}],"last":1,"more":false}`,
		`{"generation":"` + chGen + `","entries":[{"key":"0/1","author":"` + chOther + `","at":"2026-11-14T09:30:00.000Z","body":{"v":1}}],"last":1,"more":false}`,
		`{"generation":"` + chGen + `","entries":[{"seq":1,"key":"0/1","author":"` + chOther + `","at":"2026-11-14T09:30:00.000Z","body":{"v":1}` + "\xff" + `}],"last":1,"more":false}`,
		mirrorPage("", 0, false) + `{"x":1}`,
		`{"generation":"` + chGen + `","entries":[],"last":"0","more":false}`,
		`{"generation":"` + chGen + `","entries":[],"last":0,"more":"false"}`,
	}
	for i, body := range lines {
		srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, body)
		})
		_, err := clientOn(srv).ReadMirror(context.Background(), srv.URL, chToken, chOwner, chChat, 0)
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("page %d error = %v body %s", i, err, body)
		}
		assertNoChannelSecrets(t, err)
		if len(*got) != 1 {
			t.Fatalf("page %d requests = %d", i, len(*got))
		}
	}
	// A sequence that is not above after is refused. 257 entries are refused.
	// 256 increasing entries are kept.
	stale, staleGot := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		if r.URL.RawQuery != "after=4" {
			t.Errorf("query = %q", r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, mirrorPage(mirrorLine(4, "0/4", `{"v":1}`), 4, false))
	})
	_, err := clientOn(stale).ReadMirror(context.Background(), stale.URL, chToken, chOwner, chChat, 4)
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("not above after: %v", err)
	}
	assertNoChannelSecrets(t, err)
	if len(*staleGot) != 1 {
		t.Fatalf("not above after requests = %d", len(*staleGot))
	}
	var many strings.Builder
	for i := 1; i <= 257; i++ {
		if i > 1 {
			many.WriteByte(',')
		}
		many.WriteString(mirrorLine(i, fmt.Sprintf("k%d", i), `{"v":1}`))
	}
	wide := mirrorPage(many.String(), 257, false)
	wideSrv, wideGot := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, wide)
	})
	_, err = clientOn(wideSrv).ReadMirror(context.Background(), wideSrv.URL, chToken, chOwner, chChat, 0)
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("257 error = %v", err)
	}
	assertNoChannelSecrets(t, err)
	if len(*wideGot) != 1 {
		t.Fatalf("257 requests = %d", len(*wideGot))
	}
	var full strings.Builder
	for i := 1; i <= 256; i++ {
		if i > 1 {
			full.WriteByte(',')
		}
		full.WriteString(mirrorLine(i, fmt.Sprintf("k%d", i), `null`))
	}
	fullBody := mirrorPage(full.String(), 256, false)
	fullSrv, _ := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, fullBody)
	})
	gotPage, err := clientOn(fullSrv).ReadMirror(context.Background(), fullSrv.URL, chToken, chOwner, chChat, 0)
	if err != nil || len(gotPage.Entries) != 256 || string(gotPage.Entries[255].Body) != "null" || gotPage.Entries[255].Seq != 256 {
		t.Fatalf("256 len = %d err = %v", len(gotPage.Entries), err)
	}
	over := mirrorPage(mirrorLine(1, "0/1", `"`+strings.Repeat("a", maxMirrorBody)+`"`), 1, false)
	overSrv, overGot := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, over)
	})
	_, err = clientOn(overSrv).ReadMirror(context.Background(), overSrv.URL, chToken, chOwner, chChat, 0)
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("oversize body error = %v", err)
	}
	assertNoChannelSecrets(t, err)
	if len(*overGot) != 1 {
		t.Fatalf("oversize requests = %d", len(*overGot))
	}
}

func TestGuestCallsKeepTheTokenInTheHeader(t *testing.T) {
	srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, cap *capturedRequest) {
		if strings.Contains(r.URL.String(), chToken) || strings.Contains(cap.body, chToken) {
			t.Errorf("token left the header: %s %s", r.URL.String(), cap.body)
		}
		if r.Header.Get("Authorization") != "Bearer "+chToken {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/v1/channels":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"channels":[]}`)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, mirrorPage("", 0, false))
		}
	})
	c := clientOn(srv)
	if _, err := c.Channels(context.Background(), srv.URL, chToken, false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadMirror(context.Background(), srv.URL, chToken, chOwner, chChat, 11); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 2 || (*got)[1].path != "/v1/channels/"+chOwner+"/"+chChat+"/mirror" {
		t.Fatalf("requests = %+v", *got)
	}
}

func TestGuestCallsReportUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := srv.URL
	srv.Close()
	c := NewClient()
	c.timeout = time.Second
	_, err := c.Channels(context.Background(), base, chToken, false)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("channels error = %v", err)
	}
	assertNoChannelSecrets(t, err)
	_, err = c.ReadMirror(context.Background(), base, chToken, chOwner, chChat, 0)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("mirror error = %v", err)
	}
	assertNoChannelSecrets(t, err)
	c.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed")
	})
	_, err = c.ReadMirror(context.Background(), "http://relay.example", chToken, chOwner, chChat, 2)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("dial error = %v", err)
	}
	assertNoChannelSecrets(t, err)
	c.Transport = failTrip{}
	_, err = c.Channels(context.Background(), "http://relay.example", chToken, false)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("read error = %v", err)
	}
	assertNoChannelSecrets(t, err)
}

func TestReadMirrorRejectsABadPath(t *testing.T) {
	srv, got := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		t.Errorf("request sent: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	})
	c := clientOn(srv)
	owners := []string{"", "0123", strings.Repeat("a", 31), strings.Repeat("a", 33), strings.ToUpper(chOwner), chOwner[:31] + "g"}
	chats := []string{"", strings.Repeat("a", 65), "bad chat", "bad/chat"}
	for _, owner := range owners {
		_, err := c.ReadMirror(context.Background(), srv.URL, chToken, owner, chChat, 0)
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("owner %q error = %v", owner, err)
		}
		assertNoChannelSecrets(t, err)
	}
	for _, chat := range chats {
		_, err := c.ReadMirror(context.Background(), srv.URL, chToken, chOwner, chat, 0)
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("chat %q error = %v", chat, err)
		}
		assertNoChannelSecrets(t, err)
	}
	if len(*got) != 0 {
		t.Fatalf("requests = %d", len(*got))
	}
}

func TestGuestCallsRejectABadBase(t *testing.T) {
	c := NewClient()
	c.timeout = time.Second
	for _, base := range []string{"http://[", "/relative", "ftp://relay.example/join", "http://ada:secret@relay.example"} {
		_, err := c.Channels(context.Background(), base, chToken, false)
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("channels %s error = %v", base, err)
		}
		assertNoChannelSecrets(t, err)
		if strings.Contains(err.Error(), "secret") {
			t.Fatalf("%s quotes the input: %v", base, err)
		}
		_, err = c.ReadMirror(context.Background(), base, chToken, chOwner, chChat, 0)
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("mirror %s error = %v", base, err)
		}
		assertNoChannelSecrets(t, err)
	}
}

func TestGuestCallsLeaveNoConnectionOpen(t *testing.T) {
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
		if r.URL.Path == "/v1/channels" {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"channels":[]}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, mirrorPage("", 0, false))
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
	if _, err := c.Channels(context.Background(), srv.URL, chToken, false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadMirror(context.Background(), srv.URL, chToken, chOwner, chChat, 0); err != nil {
		t.Fatal(err)
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

func TestChannelsSubscriptionListSkipsInvalidIdentities(t *testing.T) {
	body := `{"channels":[` + guestChannel(chOwner, chChat, "read", 1, 0) + `,` + guestChannel("bad", chChat, "read", 1, 0) + `]}`
	srv, _ := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		_, _ = io.WriteString(w, body)
	})
	list, err := clientOn(srv).Channels(context.Background(), srv.URL, chToken, true)
	if err != nil || len(list) != 1 || list[0].Owner != chOwner {
		t.Fatalf("list = %+v, err = %v", list, err)
	}
}

func TestChannelsUsesExplicitSubscriptionMode(t *testing.T) {
	if reflect.TypeOf((*Client).Channels).IsVariadic() {
		t.Fatal("channel mode must be explicit")
	}
}

func TestSubscriptionChannelsRejectMalformedListedItem(t *testing.T) {
	srv, _ := serveTeam(t, func(w http.ResponseWriter, r *http.Request, _ *capturedRequest) {
		_, _ = io.WriteString(w, `{"channels":["not-an-object"]}`)
	})
	if _, err := clientOn(srv).Channels(context.Background(), srv.URL, chToken, true); !errors.Is(err, ErrProtocol) {
		t.Fatalf("error = %v", err)
	}
}

func TestLeaveChannelStatusAndRequest(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{http.StatusNoContent, nil},
		{http.StatusUnauthorized, ErrUnauthorized},
		{http.StatusNotFound, ErrNoChannel},
		{http.StatusRequestEntityTooLarge, ErrTooLarge},
		{http.StatusUnprocessableEntity, ErrUnknownMember},
		{http.StatusInsufficientStorage, ErrQuota},
		{http.StatusConflict, ErrStaleGeneration},
		{http.StatusInternalServerError, ErrProtocol},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			calls := 0
			c := &Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodDelete {
					t.Error("wrong request method")
				}
				if r.URL.Path != "/v1/channels/"+chOwner+"/"+chChat+"/membership" {
					t.Error("wrong request path")
				}
				if r.Header.Get("Authorization") != "Bearer "+chToken {
					t.Error("missing authorization")
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
			})}
			err := c.LeaveChannel(context.Background(), "http://127.0.0.1", chToken, chOwner, chChat)
			if !errors.Is(err, tc.want) {
				t.Error("wrong mapped status")
			}
			if calls != 1 {
				t.Errorf("request count = %d, want 1", calls)
			}
		})
	}
}

func TestLeaveChannelTransportFailure(t *testing.T) {
	calls := 0
	c := &Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("synthetic transport failure")
	})}
	if err := c.LeaveChannel(context.Background(), "http://127.0.0.1", chToken, chOwner, chChat); !errors.Is(err, ErrUnreachable) {
		t.Error("transport failure was not unreachable")
	}
	if calls != 1 {
		t.Errorf("request count = %d, want 1", calls)
	}
}

func TestLeaveChannelRejectsBadPathWithoutRequest(t *testing.T) {
	for _, tc := range []struct{ owner, chat string }{{"bad", chChat}, {chOwner, "bad/chat"}} {
		calls := 0
		c := &Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("unexpected request")
		})}
		if err := c.LeaveChannel(context.Background(), "http://127.0.0.1", chToken, tc.owner, tc.chat); !errors.Is(err, ErrProtocol) {
			t.Error("invalid path was not refused")
		}
		if calls != 0 {
			t.Errorf("request count = %d, want 0", calls)
		}
	}
}

func TestCommentsClientReadsAndAppends(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(fmt.Sprint(duplicate), func(t *testing.T) {
			calls := 0
			c := &Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Path != "/v1/channels/"+chOwner+"/"+chChat+"/comments" {
					t.Error("wrong comments path")
				}
				if r.Method == http.MethodGet {
					if r.URL.RawQuery != "after=7" {
						t.Error("wrong comments query")
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"generation":"` + chGen + `","entries":[],"last":7,"more":false}`)), Header: make(http.Header)}, nil
				}
				body, _ := io.ReadAll(r.Body)
				if string(body) != `{"generation":"`+chGen+`","entries":[{"key":"c/0123456789abcdef","body":{"v":1}}]}` {
					t.Error("wrong comments append body")
				}
				answer := fmt.Sprintf(`{"generation":"%s","results":[{"key":"c/0123456789abcdef","seq":3,"duplicate":%t}],"last":8}`, chGen, duplicate)
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(answer)), Header: make(http.Header)}, nil
			})}
			page, err := c.ReadComments(context.Background(), "http://127.0.0.1", chToken, chOwner, chChat, 7)
			if err != nil || page.Last != 7 {
				t.Error("comments page rejected")
			}
			seq, err := c.AppendComment(context.Background(), "http://127.0.0.1", chToken, chOwner, chChat, chGen, "c/0123456789abcdef", json.RawMessage(`{"v":1}`))
			if err != nil || seq != 3 {
				t.Error("comment result rejected")
			}
			if calls != 2 {
				t.Errorf("calls = %d", calls)
			}
		})
	}
}

func TestCommentsClientStatusAndValidation(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{401, ErrUnauthorized}, {403, ErrForbidden}, {404, ErrNoChannel}, {409, ErrStaleGeneration},
		{413, ErrTooLarge}, {422, ErrUnknownMember}, {507, ErrQuota}, {500, ErrProtocol},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			c := &Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
			})}
			_, readErr := c.ReadComments(context.Background(), "http://127.0.0.1", chToken, chOwner, chChat, 0)
			_, appendErr := c.AppendComment(context.Background(), "http://127.0.0.1", chToken, chOwner, chChat, chGen, "c/key", json.RawMessage(`{}`))
			if !errors.Is(readErr, tc.want) || !errors.Is(appendErr, tc.want) {
				t.Error("wrong comments status")
			}
		})
	}
	t.Run("bad input sends nothing", func(t *testing.T) {
		calls := 0
		c := &Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("network") })}
		if _, err := c.ReadComments(context.Background(), "http://127.0.0.1", chToken, "bad", chChat, 0); !errors.Is(err, ErrProtocol) {
			t.Error("bad read path accepted")
		}
		for _, tc := range []struct {
			owner, chat, generation, key string
			body                         json.RawMessage
		}{
			{"bad", chChat, chGen, "c/key", json.RawMessage(`{}`)}, {chOwner, "!", chGen, "c/key", json.RawMessage(`{}`)},
			{chOwner, chChat, "bad", "c/key", json.RawMessage(`{}`)}, {chOwner, chChat, chGen, "!", json.RawMessage(`{}`)},
			{chOwner, chChat, chGen, "c/key", json.RawMessage(`{`)},
		} {
			if _, err := c.AppendComment(context.Background(), "http://127.0.0.1", chToken, tc.owner, tc.chat, tc.generation, tc.key, tc.body); !errors.Is(err, ErrProtocol) {
				t.Error("bad append accepted")
			}
		}
		if calls != 0 {
			t.Errorf("invalid inputs sent %d requests", calls)
		}
	})
}

func TestChannelForbiddenIsProtocolForExistingCallers(t *testing.T) {
	c := &Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	for _, name := range channelNames() {
		err := runChannel(c, context.Background(), "http://127.0.0.1", name)
		if !errors.Is(err, ErrForbidden) || !errors.Is(err, ErrProtocol) {
			t.Errorf("%s did not retain protocol error", name)
		}
	}
	_, channelsErr := c.Channels(context.Background(), "http://127.0.0.1", chToken, false)
	_, mirrorErr := c.ReadMirror(context.Background(), "http://127.0.0.1", chToken, chOwner, chChat, 0)
	leaveErr := c.LeaveChannel(context.Background(), "http://127.0.0.1", chToken, chOwner, chChat)
	for _, err := range []error{channelsErr, mirrorErr, leaveErr} {
		if !errors.Is(err, ErrForbidden) || !errors.Is(err, ErrProtocol) {
			t.Error("existing caller did not retain protocol error")
		}
	}
}

func TestCommentsClientRejectsBadRepliesAndTransport(t *testing.T) {
	for _, answer := range []string{
		`{}`, `{"generation":"` + chGen + `","results":[],"last":0}`,
		`{"generation":"` + chGen + `","results":[{"key":"wrong","seq":1,"duplicate":false}],"last":1}`,
		`{"generation":"` + chGen + `","results":[{"key":"c/key","seq":0,"duplicate":false}],"last":1}`,
		`{"generation":"` + chGen + `","results":[{"key":"c/key","duplicate":false}],"last":1}`,
		`{"generation":"` + chGen + `","results":[{"key":"c/key","seq":1,"duplicate":false}],"last":0}`,
	} {
		c := &Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(answer)), Header: make(http.Header)}, nil
		})}
		if _, err := c.AppendComment(context.Background(), "http://127.0.0.1", chToken, chOwner, chChat, chGen, "c/key", json.RawMessage(`{}`)); !errors.Is(err, ErrProtocol) {
			t.Error("bad append reply accepted")
		}
		if _, err := c.ReadComments(context.Background(), "http://127.0.0.1", chToken, chOwner, chChat, 0); !errors.Is(err, ErrProtocol) {
			t.Error("bad read reply accepted")
		}
	}
	c := &Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("synthetic transport error") })}
	if _, err := c.ReadComments(context.Background(), "http://127.0.0.1", chToken, chOwner, chChat, 0); !errors.Is(err, ErrUnreachable) {
		t.Error("read transport error not mapped")
	}
	if _, err := c.AppendComment(context.Background(), "http://127.0.0.1", chToken, chOwner, chChat, chGen, "c/key", json.RawMessage(`{}`)); !errors.Is(err, ErrUnreachable) {
		t.Error("append transport error not mapped")
	}
}

// This adapter lets the regression commit build before the signature changes.
func deleteWithGeneration(c *Client, ctx context.Context, base, token, owner, chat, generation string) error {
	method := reflect.ValueOf(c).MethodByName("DeleteChannel")
	contextValue := reflect.ValueOf(ctx)
	if ctx == nil {
		contextValue = reflect.Zero(method.Type().In(0))
	}
	args := []reflect.Value{contextValue, reflect.ValueOf(base), reflect.ValueOf(token), reflect.ValueOf(owner), reflect.ValueOf(chat)}
	if method.Type().NumIn() == 6 {
		args = append(args, reflect.ValueOf(generation))
	}
	out := method.Call(args)[0]
	if out.IsNil() {
		return nil
	}
	return out.Interface().(error)
}

func TestDeleteChannelGenerationQuery(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Query().Get("generation") != chGen {
			t.Errorf("generation = %q", r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":"stale_generation"}`)
	}))
	defer srv.Close()
	c := clientOn(srv)
	for _, gen := range []string{"", "0123456789abcdeF", "bad"} {
		if err := deleteWithGeneration(c, context.Background(), srv.URL, chToken, chOwner, chChat, gen); !errors.Is(err, ErrProtocol) {
			t.Errorf("invalid %q: %v", gen, err)
		}
	}
	if hits != 0 {
		t.Fatalf("invalid generation sent %d requests", hits)
	}
	if err := deleteWithGeneration(c, context.Background(), srv.URL, chToken, chOwner, chChat, chGen); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("stale: %v", err)
	}
	if hits != 1 {
		t.Fatalf("requests: %d", hits)
	}
}
