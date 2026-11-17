package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/identity"
	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/team"
)

const (
	subOwner  = "11111111111111111111111111111111"
	subOwnerB = "22222222222222222222222222222222"
	subOwnerC = "33333333333333333333333333333333"
	subChat   = "aaaaaaaaaaaaaaaa"
	subChatB  = "bbbbbbbbbbbbbbbb"
	subChatC  = "cccccccccccccccd"
	subGen    = "cccccccccccccccc"
	subGenB   = "dddddddddddddddd"
)

type subCall struct {
	method string
	path   string
	auth   string
}

type subScript struct {
	mu             sync.Mutex
	helloStatus    int
	helloID        string
	channelsStatus int
	mirrorStatus   int
	channelsBody   string
	seatsStatus    int
	seatsBody      string
	dropPath       string
	calls          []subCall
}

func (s *subScript) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.calls = append(s.calls, subCall{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization")})
	drop := s.dropPath != "" && r.URL.Path == s.dropPath
	helloStatus, helloID := s.helloStatus, s.helloID
	chStatus, chBody := s.channelsStatus, s.channelsBody
	seatStatus, seatBody := s.seatsStatus, s.seatsBody
	mirrorStatus := s.mirrorStatus
	s.mu.Unlock()
	if drop {
		hj, ok := w.(http.Hijacker)
		if !ok {
			panic("subscription script: no hijack")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			panic(err)
		}
		_ = conn.Close()
		return
	}
	switch r.URL.Path {
	case "/v1/hello":
		if helloStatus == 0 {
			helloStatus = http.StatusOK
		}
		w.WriteHeader(helloStatus)
		if helloStatus == http.StatusOK {
			if helloID == "" {
				helloID = teamServerID
			}
			_, _ = io.WriteString(w, helloJSON(helloID, "Lab relay"))
		}
	case "/v1/channels":
		if chStatus == 0 {
			chStatus = http.StatusOK
		}
		w.WriteHeader(chStatus)
		if chStatus == http.StatusOK {
			if chBody == "" {
				chBody = `{"channels":[]}`
			}
			_, _ = io.WriteString(w, chBody)
		}
	case "/v1/seats":
		if seatStatus == 0 {
			seatStatus = http.StatusOK
		}
		w.WriteHeader(seatStatus)
		if seatStatus == http.StatusOK {
			if seatBody == "" {
				seatBody = `{"seats":[]}`
			}
			_, _ = io.WriteString(w, seatBody)
		}
	default:
		if strings.HasSuffix(r.URL.Path, "/mirror") && mirrorStatus != 0 {
			w.WriteHeader(mirrorStatus)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *subScript) snapshot() []subCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]subCall, len(s.calls))
	copy(out, s.calls)
	return out
}

func countSubPath(calls []subCall, path string) int {
	n := 0
	for _, c := range calls {
		if c.path == path {
			n++
		}
	}
	return n
}

func guestChannel(owner, chat, title, gen, access string, mirror uint64) string {
	raw, err := json.Marshal(struct {
		Owner        string `json:"owner"`
		Chat         string `json:"chat"`
		Title        string `json:"title"`
		Generation   string `json:"generation"`
		CreatedAt    string `json:"created_at"`
		Access       string `json:"access"`
		MirrorLast   uint64 `json:"mirror_last"`
		CommentsLast uint64 `json:"comments_last"`
	}{owner, chat, title, gen, "2026-11-14T09:30:00.000Z", access, mirror, 0})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func channelsOf(items ...string) string {
	return `{"channels":[` + strings.Join(items, ",") + `]}`
}

func seatsOf(pairs ...string) string {
	if len(pairs)%2 != 0 {
		panic("seatsOf: id, name")
	}
	var b strings.Builder
	b.WriteString(`{"seats":[`)
	for i := 0; i < len(pairs); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":"%s","name":"%s"}`, pairs[i], pairs[i+1])
	}
	b.WriteString(`]}`)
	return b.String()
}

func joinLab(t *testing.T, dir, id, rawURL string) {
	t.Helper()
	e := labEntry(id, rawURL, teamToken)
	e.Name = "Lab One"
	seedTeam(t, dir, e)
}

func subSurface(t *testing.T, script *subScript) (http.Handler, string, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(script)
	t.Cleanup(srv.Close)
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	return h, dir, srv
}

func holdClock(t *testing.T, h http.Handler) *time.Time {
	t.Helper()
	cur := fixedTeamClock()()
	asTeamHandler(t, h).now = func() time.Time { return cur }
	return &cur
}

type subView struct {
	Server        string              `json:"server"`
	ServerName    string              `json:"server_name"`
	Owner         string              `json:"owner"`
	OwnerName     string              `json:"owner_name"`
	Chat          string              `json:"chat"`
	Generation    string              `json:"generation"`
	Title         string              `json:"title"`
	Access        string              `json:"access"`
	State         string              `json:"state"`
	MirrorLast    uint64              `json:"mirror_last"`
	SeenMirror    uint64              `json:"seen_mirror"`
	CommentsLast  uint64              `json:"comments_last"`
	SeenComments  uint64              `json:"seen_comments"`
	Unread        bool                `json:"unread"`
	FirstSeen     string              `json:"first_seen"`
	LocalNode     string              `json:"local_node,omitempty"`
	LocalOrder    *int                `json:"local_order,omitempty"`
	LocalFrontier *team.OwnerFrontier `json:"local_frontier,omitempty"`
}

type subServerView struct {
	ID       string `json:"id"`
	Delivery string `json:"delivery"`
}

type subListBody struct {
	Subscriptions []subView       `json:"subscriptions"`
	Servers       []subServerView `json:"servers"`
	Truncated     *bool           `json:"truncated"`
}

func readSubList(t *testing.T, h http.Handler) (subListBody, *httptest.ResponseRecorder) {
	t.Helper()
	rec := doTeam(h, http.MethodGet, "/api/team/subscriptions", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	assertNoSecrets(t, rec.Body.String(), "", teamToken)
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	var got subListBody
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("body %s: %v", rec.Body.String(), err)
	}
	if got.Truncated == nil {
		t.Fatal("truncated is missing")
	}
	return got, rec
}

func findSub(list subListBody, owner, chat, gen string) (subView, bool) {
	for _, s := range list.Subscriptions {
		if s.Owner == owner && s.Chat == chat && s.Generation == gen {
			return s, true
		}
	}
	return subView{}, false
}

func mustFindSub(t *testing.T, list subListBody, owner, chat, gen string) subView {
	t.Helper()
	s, ok := findSub(list, owner, chat, gen)
	if !ok {
		t.Fatalf("missing %s %s %s in %+v", owner, chat, gen, list.Subscriptions)
	}
	return s
}

func loadSubFile(t *testing.T, dir string) []team.Subscription {
	t.Helper()
	subs, err := team.LoadSubs(dir)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, team.SubsFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), teamToken) {
		t.Fatal("subscription file contains the token")
	}
	return subs
}

func seedSub(t *testing.T, dir string, s team.Subscription) {
	t.Helper()
	have, err := team.LoadSubs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := team.SaveSubs(dir, append(have, s)); err != nil {
		t.Fatal(err)
	}
}

func baseSub() team.Subscription {
	return team.Subscription{
		Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen,
		ServerName: "Lab One", OwnerName: "Ada", Title: "Synthetic title",
		Access: "read", State: "live", MirrorLast: 2, SeenMirror: 0,
		FirstSeen: "2026-11-12T09:30:00Z",
	}
}

func assertNoSubsFile(t *testing.T, dir string) {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, team.SubsFile))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("subs stat = %v", err)
	}
}

func TestSubListGateOffDoesNothing(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits++
	}))
	t.Cleanup(srv.Close)
	orig := newTeamClient
	t.Cleanup(func() { newTeamClient = orig })
	newTeamClient = func() *team.Client {
		c := orig()
		c.Transport = srv.Client().Transport
		return c
	}
	cmd, data, _, _ := startTeamCommand(t, false, true)
	h, err := newCoreMux(cmd.application)
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		unknown := recordedText(h, method, "/api/not-a-route")
		got := recordedText(h, method, "/api/team/subscriptions")
		if got != unknown {
			t.Fatalf("%s subscriptions\n%s\nwant the unknown-route response\n%s", method, got, unknown)
		}
	}
	assertNoTeamFile(t, data)
	assertNoSubsFile(t, data)
	if _, err := os.Stat(filepath.Join(data, team.CopiesDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("copies stat = %v", err)
	}
	if hits != 0 {
		t.Fatalf("gate off contacted a server %d times", hits)
	}
}

func TestSubListOtherMethodsDoNothing(t *testing.T) {
	h, dir := testTeamSurface(t, team.NewClient())
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := doTeam(h, method, "/api/team/subscriptions", `{"mirror":1}`)
		assertAPIError(t, rec, http.StatusNotFound, "not_found")
	}
	rec := doTeam(h, http.MethodGet, "/api/team/subscriptions/", "")
	assertAPIError(t, rec, http.StatusNotFound, "not_found")
	assertNoSubsFile(t, dir)
}

func TestSubListWithNoServer(t *testing.T) {
	h, dir := testTeamSurface(t, team.NewClient())
	got, rec := readSubList(t, h)
	if len(got.Subscriptions) != 0 || len(got.Servers) != 0 || *got.Truncated {
		t.Fatalf("list = %+v", got)
	}
	if !strings.Contains(rec.Body.String(), `"subscriptions":[]`) || !strings.Contains(rec.Body.String(), `"servers":[]`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
	assertNoSubsFile(t, dir)
}

func TestSubListWithNoChannel(t *testing.T) {
	script := &subScript{}
	h, dir, _ := subSurface(t, script)
	got, _ := readSubList(t, h)
	if len(got.Subscriptions) != 0 || len(got.Servers) != 1 || got.Servers[0].ID != teamServerID || got.Servers[0].Delivery != "ok" || *got.Truncated {
		t.Fatalf("list = %+v", got)
	}
	calls := script.snapshot()
	if countSubPath(calls, "/v1/hello") != 1 || countSubPath(calls, "/v1/channels") != 1 || countSubPath(calls, "/v1/seats") != 0 {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[0].path != "/v1/hello" || calls[0].auth != "" {
		t.Fatalf("hello = %+v", calls[0])
	}
	assertNoSubsFile(t, dir)
	if _, err := os.Stat(filepath.Join(dir, team.CopiesDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("copies stat = %v", err)
	}
}

func TestSubListIncludesOwnedChannels(t *testing.T) {
	script := &subScript{}
	srv := httptest.NewServer(script)
	t.Cleanup(srv.Close)
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	inst := installationOf(t, dir)
	script.channelsBody = channelsOf(
		channelJSON(inst, subChat, "Synthetic title", subGen),
		guestChannel(inst, subChatB, "Synthetic title", subGen, "read", 3),
		guestChannel(subOwner, subChatC, "North <relay>", subGen, "write", 4),
	)
	script.seatsBody = seatsOf(subOwner, "Ada")
	got, _ := readSubList(t, h)
	if len(got.Subscriptions) != 2 {
		t.Fatalf("subscriptions = %+v", got.Subscriptions)
	}
	one := mustFindSub(t, got, subOwner, subChatC, subGen)
	if one.State != "live" || one.Access != "write" || one.Title != "North <relay>" || one.OwnerName != "Ada" {
		t.Fatalf("kept = %+v", one)
	}
	if own, ok := findSub(got, inst, subChat, subGen); !ok || own.Access != "owner" {
		t.Fatal("owned channel was not stored")
	}
	if _, ok := findSub(got, inst, subChatB, subGen); ok {
		t.Fatal("a read channel this seat owns was stored")
	}
}

func TestSubListAddsASharedChannel(t *testing.T) {
	script := &subScript{
		channelsBody: channelsOf(
			guestChannel(subOwner, subChat, "North <relay>", subGen, "read", 4),
			guestChannel(subOwnerB, subChatB, "Synthetic title", subGen, "write", 0),
		),
		seatsBody: seatsOf(subOwner, "Ada", subOwnerB, "Bea"),
	}
	h, dir, _ := subSurface(t, script)
	got, _ := readSubList(t, h)
	if len(got.Subscriptions) != 2 || len(got.Servers) != 1 || got.Servers[0].Delivery != "ok" || *got.Truncated {
		t.Fatalf("list = %+v", got)
	}
	read := mustFindSub(t, got, subOwner, subChat, subGen)
	if read.Server != teamServerID || read.ServerName != "Lab One" || read.OwnerName != "Ada" || read.Access != "read" || read.State != "live" || read.MirrorLast != 4 || read.SeenMirror != 0 || !read.Unread || read.FirstSeen != "2026-11-12T09:30:00Z" || read.Title != "North <relay>" {
		t.Fatalf("read = %+v", read)
	}
	write := mustFindSub(t, got, subOwnerB, subChatB, subGen)
	if write.Access != "write" || write.OwnerName != "Bea" || write.MirrorLast != 0 || write.Unread || write.SeenMirror != 0 {
		t.Fatalf("write = %+v", write)
	}
	info, err := os.Stat(filepath.Join(dir, team.SubsFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	stored := loadSubFile(t, dir)
	if len(stored) != 2 {
		t.Fatalf("stored = %+v", stored)
	}
	calls := script.snapshot()
	if calls[0].auth != "" || calls[0].path != "/v1/hello" {
		t.Fatalf("first call = %+v", calls[0])
	}
	for _, c := range calls {
		if c.path == "/v1/hello" && c.auth != "" {
			t.Fatalf("hello carried a token: %+v", calls)
		}
		if (c.path == "/v1/channels" || c.path == "/v1/seats") && c.auth != "Bearer "+teamToken {
			t.Fatalf("authed call = %+v", c)
		}
	}
}

func TestSubListUpdatesALiveSubscription(t *testing.T) {
	script := &subScript{
		channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic title", subGen, "read", 1)),
		seatsBody:    seatsOf(subOwner, "Ada"),
	}
	h, dir, _ := subSurface(t, script)
	clock := holdClock(t, h)
	first, _ := readSubList(t, h)
	*clock = clock.Add(10 * time.Second)
	script.mu.Lock()
	script.channelsBody = channelsOf(guestChannel(subOwner, subChat, "North <relay>", subGen, "write", 8))
	script.mu.Unlock()
	second, _ := readSubList(t, h)
	one := mustFindSub(t, second, subOwner, subChat, subGen)
	if one.Title != "North <relay>" || one.Access != "write" || one.MirrorLast != 8 || one.SeenMirror != 0 || one.State != "live" || !one.Unread || one.FirstSeen != first.Subscriptions[0].FirstSeen || one.ServerName != "Lab One" || one.OwnerName != "Ada" {
		t.Fatalf("updated = %+v", one)
	}
	if countSubPath(script.snapshot(), "/v1/seats") != 1 {
		t.Fatalf("seats were asked again: %+v", script.snapshot())
	}
	if len(loadSubFile(t, dir)) != 1 {
		t.Fatal("update stored a second subscription")
	}
}

func TestSubListEndsTheOldGeneration(t *testing.T) {
	script := &subScript{
		channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic title", subGen, "read", 3)),
		seatsBody:    seatsOf(subOwner, "Ada"),
	}
	h, _, _ := subSurface(t, script)
	clock := holdClock(t, h)
	readSubList(t, h)
	*clock = clock.Add(10 * time.Second)
	script.mu.Lock()
	script.channelsBody = channelsOf(guestChannel(subOwner, subChat, "North <relay>", subGenB, "write", 0))
	script.mu.Unlock()
	got, _ := readSubList(t, h)
	if len(got.Subscriptions) != 2 {
		t.Fatalf("subscriptions = %+v", got.Subscriptions)
	}
	old := mustFindSub(t, got, subOwner, subChat, subGen)
	if old.State != "ended" || old.Title != "Synthetic title" || old.MirrorLast != 3 || old.Access != "read" || old.SeenMirror != 0 {
		t.Fatalf("old = %+v", old)
	}
	next := mustFindSub(t, got, subOwner, subChat, subGenB)
	if next.State != "live" || next.Title != "North <relay>" || next.Access != "write" || next.MirrorLast != 0 || next.SeenMirror != 0 || next.FirstSeen != "2026-11-12T09:30:10Z" {
		t.Fatalf("new = %+v", next)
	}
}

func TestSubListEndsAChannelThatIsGone(t *testing.T) {
	script := &subScript{
		channelsBody: channelsOf(
			guestChannel(subOwner, subChat, "Synthetic title", subGen, "read", 1),
			guestChannel(subOwner, subChatB, "Lab relay", subGen, "read", 1),
		),
		seatsBody: seatsOf(subOwner, "Ada"),
	}
	h, _, _ := subSurface(t, script)
	clock := holdClock(t, h)
	readSubList(t, h)
	*clock = clock.Add(10 * time.Second)
	script.mu.Lock()
	script.channelsBody = channelsOf(guestChannel(subOwner, subChat, "North <relay>", subGen, "write", 6))
	script.mu.Unlock()
	got, _ := readSubList(t, h)
	kept := mustFindSub(t, got, subOwner, subChat, subGen)
	gone := mustFindSub(t, got, subOwner, subChatB, subGen)
	if kept.State != "live" || kept.Title != "North <relay>" || kept.MirrorLast != 6 || kept.Access != "write" {
		t.Fatalf("kept = %+v", kept)
	}
	if gone.State != "ended" || gone.Title != "Lab relay" || gone.MirrorLast != 1 {
		t.Fatalf("gone = %+v", gone)
	}
}

func TestSubListRevivesSameGeneration(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic title", subGen, "read", 2)), seatsBody: seatsOf(subOwner, "Ada")}
	h, dir, _ := subSurface(t, script)
	clock := holdClock(t, h)
	sub := baseSub()
	sub.SeenMirror = 1
	seedSub(t, dir, sub)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	if err := team.AppendCopy(dir, id, team.MirrorLog, []team.MirrorRecord{{Seq: 1, Key: "k1", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: json.RawMessage(`{"v":1}`)}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, team.CopiesDir, teamServerID, subOwner, subChat, subGen, "mirror.jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	readSubList(t, h)
	*clock = clock.Add(10 * time.Second)
	script.mu.Lock()
	script.channelsBody = `{"channels":[]}`
	script.mu.Unlock()
	readSubList(t, h)
	*clock = clock.Add(10 * time.Second)
	script.mu.Lock()
	script.channelsBody = channelsOf(guestChannel(subOwner, subChat, "North <relay>", subGen, "write", 9))
	script.mu.Unlock()
	got, _ := readSubList(t, h)
	one := mustFindSub(t, got, subOwner, subChat, subGen)
	if one.State != "live" || one.Title != "North <relay>" || one.Access != "write" || one.MirrorLast != 9 || one.SeenMirror != 1 || one.FirstSeen != sub.FirstSeen {
		t.Fatalf("revived = %+v", one)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("revival changed copy")
	}
}

func TestSubListRevivesAfterReadRefusal(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusUnauthorized} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic title", subGen, "read", 2)), seatsBody: seatsOf(subOwner, "Ada")}
			h, dir, _ := subSurface(t, script)
			clock := holdClock(t, h)
			seedSub(t, dir, baseSub())
			script.mu.Lock()
			script.mirrorStatus = status
			script.mu.Unlock()
			decodeSubRead(t, doTeam(h, http.MethodGet, subReadPath(), ""))
			*clock = clock.Add(10 * time.Second)
			got, _ := readSubList(t, h)
			if s := mustFindSub(t, got, subOwner, subChat, subGen); s.State != "live" {
				t.Fatalf("subscription = %+v", s)
			}
		})
	}
}

func TestSubListAsksOncePerTenSeconds(t *testing.T) {
	script := &subScript{
		channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic title", subGen, "read", 1)),
		seatsBody:    seatsOf(subOwner, "Ada"),
	}
	h, _, _ := subSurface(t, script)
	clock := holdClock(t, h)
	readSubList(t, h)
	script.mu.Lock()
	script.channelsBody = channelsOf(guestChannel(subOwner, subChat, "North <relay>", subGen, "read", 5))
	script.mu.Unlock()
	cached, _ := readSubList(t, h)
	if mustFindSub(t, cached, subOwner, subChat, subGen).Title != "Synthetic title" {
		t.Fatalf("second call refreshed: %+v", cached.Subscriptions)
	}
	*clock = clock.Add(9 * time.Second)
	still, _ := readSubList(t, h)
	if mustFindSub(t, still, subOwner, subChat, subGen).Title != "Synthetic title" {
		t.Fatalf("call inside the window refreshed: %+v", still.Subscriptions)
	}
	if countSubPath(script.snapshot(), "/v1/channels") != 1 {
		t.Fatalf("asks = %+v", script.snapshot())
	}
	*clock = clock.Add(time.Second)
	fresh, _ := readSubList(t, h)
	if mustFindSub(t, fresh, subOwner, subChat, subGen).Title != "North <relay>" || mustFindSub(t, fresh, subOwner, subChat, subGen).MirrorLast != 5 {
		t.Fatalf("after the window = %+v", fresh.Subscriptions)
	}
	if countSubPath(script.snapshot(), "/v1/channels") != 2 || countSubPath(script.snapshot(), "/v1/hello") != 2 {
		t.Fatalf("asks = %+v", script.snapshot())
	}
}

func TestSubList401EndsTheServer(t *testing.T) {
	good := &subScript{
		channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic title", subGen, "read", 1)),
		seatsBody:    seatsOf(subOwner, "Ada"),
	}
	bad := &subScript{channelsStatus: http.StatusUnauthorized}
	goodSrv := httptest.NewServer(good)
	badSrv := httptest.NewServer(bad)
	t.Cleanup(goodSrv.Close)
	t.Cleanup(badSrv.Close)
	h, dir := testTeamSurface(t, clientFor(goodSrv))
	joinLab(t, dir, teamServerID, goodSrv.URL)
	joinLab(t, dir, teamOtherID, badSrv.URL)
	first := baseSub()
	first.Server = teamOtherID
	second := first
	second.Chat = subChatB
	second.Title = "Lab relay"
	seedSub(t, dir, first)
	seedSub(t, dir, second)
	live := baseSub()
	seedSub(t, dir, live)
	bad.helloID = teamOtherID
	got, _ := readSubList(t, h)
	if len(got.Servers) != 2 {
		t.Fatalf("servers = %+v", got.Servers)
	}
	for _, srv := range got.Servers {
		switch srv.ID {
		case teamServerID:
			if srv.Delivery != "ok" {
				t.Fatalf("good delivery = %s", srv.Delivery)
			}
		case teamOtherID:
			if srv.Delivery != "refused" {
				t.Fatalf("bad delivery = %s", srv.Delivery)
			}
		default:
			t.Fatalf("server %s", srv.ID)
		}
	}
	var endedA, endedB, liveState string
	for _, s := range got.Subscriptions {
		if s.Server == teamOtherID && s.Chat == subChat && s.Generation == subGen {
			endedA = s.State
		}
		if s.Server == teamOtherID && s.Chat == subChatB {
			endedB = s.State
			if s.Title != "Lab relay" {
				t.Fatalf("second ended = %+v", s)
			}
		}
		if s.Server == teamServerID && s.Chat == subChat {
			liveState = s.State
		}
	}
	if endedA != "ended" || endedB != "ended" || liveState != "live" {
		t.Fatalf("states ended=%s %s live=%s list=%+v", endedA, endedB, liveState, got.Subscriptions)
	}
	if countSubPath(bad.snapshot(), "/v1/seats") != 0 || countSubPath(bad.snapshot(), "/v1/channels") != 1 {
		t.Fatalf("bad calls = %+v", bad.snapshot())
	}
}

func TestSubListUnreachableChangesNothing(t *testing.T) {
	script := &subScript{dropPath: "/v1/channels"}
	h, dir, _ := subSurface(t, script)
	seedSub(t, dir, baseSub())
	before, err := os.ReadFile(filepath.Join(dir, team.SubsFile))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := readSubList(t, h)
	if len(got.Servers) != 1 || got.Servers[0].Delivery != "offline" {
		t.Fatalf("servers = %+v", got.Servers)
	}
	one := mustFindSub(t, got, subOwner, subChat, subGen)
	if one.State != "live" || one.Title != "Synthetic title" || one.MirrorLast != 2 {
		t.Fatalf("changed = %+v", one)
	}
	after, err := os.ReadFile(filepath.Join(dir, team.SubsFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("unreachable server rewrote the subscription file")
	}
	if countSubPath(script.snapshot(), "/v1/seats") != 0 {
		t.Fatalf("seats were called: %+v", script.snapshot())
	}
}

func TestSubListWrongHelloChangesNothing(t *testing.T) {
	script := &subScript{helloID: teamOtherID}
	h, dir, _ := subSurface(t, script)
	seedSub(t, dir, baseSub())
	before, err := os.ReadFile(filepath.Join(dir, team.SubsFile))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := readSubList(t, h)
	if got.Servers[0].Delivery != "changed" {
		t.Fatalf("delivery = %+v", got.Servers)
	}
	if mustFindSub(t, got, subOwner, subChat, subGen).State != "live" {
		t.Fatalf("list = %+v", got.Subscriptions)
	}
	after, err := os.ReadFile(filepath.Join(dir, team.SubsFile))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("wrong hello rewrote the subscription file")
	}
	for _, c := range script.snapshot() {
		if c.auth != "" || c.path != "/v1/hello" {
			t.Fatalf("call after a mismatched hello = %+v", script.snapshot())
		}
	}
}

func TestSubListBadProtocolChangesNothing(t *testing.T) {
	script := &subScript{channelsStatus: http.StatusOK, channelsBody: "nope"}
	h, dir, _ := subSurface(t, script)
	seedSub(t, dir, baseSub())
	before, err := os.ReadFile(filepath.Join(dir, team.SubsFile))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := readSubList(t, h)
	if got.Servers[0].Delivery != "changed" || mustFindSub(t, got, subOwner, subChat, subGen).MirrorLast != 2 {
		t.Fatalf("list = %+v", got)
	}
	after, err := os.ReadFile(filepath.Join(dir, team.SubsFile))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("a bad channels body rewrote the subscription file")
	}
}

func TestSubListForgetsAServerWithoutACall(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic title", subGen, "read", 1))}
	h, dir, _ := subSurface(t, script)
	orphan := baseSub()
	orphan.Server = teamOtherID
	orphan.Chat = subChatB
	seedSub(t, dir, orphan)
	got, _ := readSubList(t, h)
	one := mustFindSub(t, got, subOwner, subChatB, subGen)
	if one.State != "ended" || one.Server != teamOtherID {
		t.Fatalf("orphan = %+v", one)
	}
	if len(got.Servers) != 1 || got.Servers[0].ID != teamServerID {
		t.Fatalf("servers = %+v", got.Servers)
	}
	if mustFindSub(t, got, subOwner, subChat, subGen).State != "live" {
		t.Fatalf("joined = %+v", got.Subscriptions)
	}
}

func TestSubListOwnerNames(t *testing.T) {
	script := &subScript{
		channelsBody: channelsOf(
			guestChannel(subOwner, subChat, "Synthetic title", subGen, "read", 1),
			guestChannel(subOwnerB, subChatB, "Lab relay", subGen, "read", 1),
		),
		seatsBody: seatsOf(subOwner, "Ada", subOwnerB, "Bea"),
	}
	h, _, _ := subSurface(t, script)
	clock := holdClock(t, h)
	first, _ := readSubList(t, h)
	if mustFindSub(t, first, subOwner, subChat, subGen).OwnerName != "Ada" || mustFindSub(t, first, subOwnerB, subChatB, subGen).OwnerName != "Bea" {
		t.Fatalf("first names = %+v", first.Subscriptions)
	}
	*clock = clock.Add(10 * time.Second)
	script.mu.Lock()
	script.channelsBody = channelsOf(
		guestChannel(subOwner, subChat, "Synthetic title", subGen, "read", 1),
		guestChannel(subOwnerB, subChatB, "Lab relay", subGen, "read", 1),
		guestChannel(subOwnerC, subChatC, "Synthetic title", subGen, "read", 1),
	)
	script.seatsBody = seatsOf(subOwner, "Ada", subOwnerB, "Bea", subOwnerC, "Ada")
	script.mu.Unlock()
	second, _ := readSubList(t, h)
	if mustFindSub(t, second, subOwnerC, subChatC, subGen).OwnerName != "Ada" {
		t.Fatalf("new name = %+v", second.Subscriptions)
	}
	if countSubPath(script.snapshot(), "/v1/seats") != 2 {
		t.Fatalf("seats asks = %+v", script.snapshot())
	}
	*clock = clock.Add(10 * time.Minute)
	script.mu.Lock()
	script.seatsBody = seatsOf(subOwner, "Bea", subOwnerC, "Ada")
	script.mu.Unlock()
	third, _ := readSubList(t, h)
	if mustFindSub(t, third, subOwner, subChat, subGen).OwnerName != "Bea" {
		t.Fatalf("refreshed name = %+v", third.Subscriptions)
	}
	if mustFindSub(t, third, subOwnerB, subChatB, subGen).OwnerName != "Bea" {
		t.Fatalf("departed owner = %+v", third.Subscriptions)
	}
	if mustFindSub(t, third, subOwnerC, subChatC, subGen).OwnerName != "Ada" {
		t.Fatalf("owner missing from the later roster = %+v", third.Subscriptions)
	}
	if countSubPath(script.snapshot(), "/v1/seats") != 3 {
		t.Fatalf("seats asks = %+v", script.snapshot())
	}
}

func TestSubListOrdersTheAnswer(t *testing.T) {
	low := &subScript{
		helloID:      teamServerID,
		channelsBody: channelsOf(guestChannel(subOwnerB, subChat, "Synthetic title", subGen, "read", 1), guestChannel(subOwner, subChat, "Lab relay", subGen, "read", 1)),
		seatsBody:    seatsOf(subOwner, "Ada", subOwnerB, "Bea"),
	}
	high := &subScript{
		helloID:      teamOtherID,
		channelsBody: channelsOf(guestChannel(subOwner, subChatB, "Synthetic title", subGen, "read", 1)),
		seatsBody:    seatsOf(subOwner, "Ada"),
	}
	lowSrv := httptest.NewServer(low)
	highSrv := httptest.NewServer(high)
	t.Cleanup(lowSrv.Close)
	t.Cleanup(highSrv.Close)
	h, dir := testTeamSurface(t, clientFor(lowSrv))
	joinLab(t, dir, teamOtherID, highSrv.URL)
	joinLab(t, dir, teamServerID, lowSrv.URL)
	clock := holdClock(t, h)
	readSubList(t, h)
	*clock = clock.Add(10 * time.Second)
	low.mu.Lock()
	low.channelsBody = channelsOf(
		guestChannel(subOwnerB, subChat, "Synthetic title", subGen, "read", 1),
		guestChannel(subOwner, subChat, "North <relay>", subGenB, "read", 0),
	)
	low.mu.Unlock()
	got, _ := readSubList(t, h)
	want := []struct{ server, owner, chat, seen, state string }{
		{teamServerID, subOwner, subChat, "2026-11-12T09:30:00Z", "ended"},
		{teamServerID, subOwner, subChat, "2026-11-12T09:30:10Z", "live"},
		{teamServerID, subOwnerB, subChat, "2026-11-12T09:30:00Z", "live"},
		{teamOtherID, subOwner, subChatB, "2026-11-12T09:30:00Z", "live"},
	}
	if len(got.Subscriptions) != len(want) {
		t.Fatalf("subscriptions = %+v", got.Subscriptions)
	}
	for i, w := range want {
		s := got.Subscriptions[i]
		if s.Server != w.server || s.Owner != w.owner || s.Chat != w.chat || s.FirstSeen != w.seen || s.State != w.state {
			t.Fatalf("order[%d] = %+v", i, got.Subscriptions)
		}
	}
	if len(got.Servers) != 2 || got.Servers[0].ID != teamServerID || got.Servers[1].ID != teamOtherID {
		t.Fatalf("servers = %+v", got.Servers)
	}
}

func TestSubListUnreadFollowsTheCounts(t *testing.T) {
	script := &subScript{seatsBody: seatsOf(subOwner, "Ada")}
	var items []string
	seeds := []team.Subscription{}
	for _, tc := range []struct {
		chat   string
		mirror uint64
		seen   uint64
	}{
		{subChat, 5, 0},
		{subChatB, 5, 5},
		{subChatC, 5, 9},
	} {
		items = append(items, guestChannel(subOwner, tc.chat, "Synthetic title", subGen, "read", tc.mirror))
		s := baseSub()
		s.Chat = tc.chat
		s.SeenMirror = tc.seen
		s.MirrorLast = 1
		seeds = append(seeds, s)
	}
	script.channelsBody = channelsOf(items...)
	h, dir, _ := subSurface(t, script)
	for _, s := range seeds {
		seedSub(t, dir, s)
	}
	got, _ := readSubList(t, h)
	for _, tc := range []struct {
		chat string
		want bool
	}{{subChat, true}, {subChatB, false}, {subChatC, false}} {
		one := mustFindSub(t, got, subOwner, tc.chat, subGen)
		if one.Unread != tc.want || one.MirrorLast != 5 {
			t.Fatalf("chat %s = %+v", tc.chat, one)
		}
	}
	plain := mustFindSub(t, got, subOwner, subChatB, subGen)
	if plain.SeenMirror != 5 {
		t.Fatalf("seen changed: %+v", plain)
	}
	over := mustFindSub(t, got, subOwner, subChatC, subGen)
	if over.SeenMirror != 9 {
		t.Fatalf("seen shrank: %+v", over)
	}
}

func TestSubListSkipsAChatThatIsNotHex(t *testing.T) {
	script := &subScript{
		channelsBody: channelsOf(
			guestChannel(subOwner, subChat, "Synthetic title", subGen, "read", 1),
			guestChannel(subOwner, "lab-chat", "Lab relay", subGen, "read", 1),
			guestChannel(subOwner, "AAAAAAAAAAAAAAAB", "Lab relay", subGen, "write", 1),
		),
		seatsBody: seatsOf(subOwner, "Ada"),
	}
	h, dir, _ := subSurface(t, script)
	got, _ := readSubList(t, h)
	if len(got.Subscriptions) != 1 || mustFindSub(t, got, subOwner, subChat, subGen).State != "live" {
		t.Fatalf("list = %+v", got.Subscriptions)
	}
	for _, s := range loadSubFile(t, dir) {
		if s.Chat != subChat {
			t.Fatalf("stored %s", s.Chat)
		}
	}
}

func TestSubListReportsTruncatedAtTheCap(t *testing.T) {
	subs := make([]team.Subscription, 4096)
	for i := range subs {
		subs[i] = baseSub()
		subs[i].Chat = fmt.Sprintf("%016x", i+1)
		subs[i].OwnerName = "Ada"
		subs[i].SeenMirror = 1
		subs[i].MirrorLast = 1
	}
	script := &subScript{
		channelsBody: channelsOf(guestChannel(subOwnerB, subChat, "Synthetic title", subGen, "read", 1)),
		seatsBody:    `{"seats":[]}`,
	}
	h, dir, _ := subSurface(t, script)
	if err := team.SaveSubs(dir, subs); err != nil {
		t.Fatal(err)
	}
	got, _ := readSubList(t, h)
	if !*got.Truncated {
		t.Fatal("truncated = false")
	}
	cached, _ := readSubList(t, h)
	if !*cached.Truncated {
		t.Fatal("cached truncated = false")
	}
	stored := loadSubFile(t, dir)
	if len(stored) != 4096 {
		t.Fatalf("stored = %d", len(stored))
	}
	for _, s := range stored {
		if s.Chat == subChat && s.Owner == subOwnerB {
			t.Fatal("the subscription past the cap was stored")
		}
	}
}

func TestSubListStoreUnavailable(t *testing.T) {
	t.Run("damaged subscriptions", func(t *testing.T) {
		script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic title", subGen, "read", 1))}
		h, dir, _ := subSurface(t, script)
		path := filepath.Join(dir, team.SubsFile)
		if err := os.WriteFile(path, []byte("nope\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		rec := doTeam(h, http.MethodGet, "/api/team/subscriptions", "")
		assertAPIError(t, rec, http.StatusInternalServerError, "store_unavailable")
		assertNoSecrets(t, rec.Body.String(), "", teamToken)
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != "nope\n" {
			t.Fatalf("file = %q err %v", raw, err)
		}
		if len(script.snapshot()) != 0 {
			t.Fatalf("calls = %+v", script.snapshot())
		}
	})
	t.Run("damaged servers", func(t *testing.T) {
		script := &subScript{}
		h, dir, _ := subSurface(t, script)
		path := filepath.Join(dir, team.File)
		if err := os.WriteFile(path, []byte("nope\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		rec := doTeam(h, http.MethodGet, "/api/team/subscriptions", "")
		assertAPIError(t, rec, http.StatusInternalServerError, "store_unavailable")
		assertNoSubsFile(t, dir)
		if len(script.snapshot()) != 0 {
			t.Fatalf("calls = %+v", script.snapshot())
		}
	})
	t.Run("subscriptions cannot be written", func(t *testing.T) {
		script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "North <relay>", subGen, "read", 4))}
		h, dir, _ := subSurface(t, script)
		seedSub(t, dir, baseSub())
		before, err := os.ReadFile(filepath.Join(dir, team.SubsFile))
		if err != nil {
			t.Fatal(err)
		}
		asTeamHandler(t, h).saveSubs = func(string, []team.Subscription) error { return os.ErrPermission }
		rec := doTeam(h, http.MethodGet, "/api/team/subscriptions", "")
		assertAPIError(t, rec, http.StatusInternalServerError, "store_unavailable")
		after, err := os.ReadFile(filepath.Join(dir, team.SubsFile))
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("a failed write replaced the subscription file")
		}
		if strings.Contains(rec.Body.String(), "North") {
			t.Fatal("error text quotes the title")
		}
	})
	t.Run("identity", func(t *testing.T) {
		h, dir, _ := subSurface(t, &subScript{})
		if err := os.Remove(filepath.Join(dir, identity.InstallationFile)); err != nil {
			t.Fatal(err)
		}
		rec := doTeam(h, http.MethodGet, "/api/team/subscriptions", "")
		assertAPIError(t, rec, http.StatusInternalServerError, "identity_unavailable")
		assertNoSubsFile(t, dir)
	})
}

func TestSubListConcurrentCallsLeaveOneFile(t *testing.T) {
	script := &subScript{
		channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic title", subGen, "read", 1)),
		seatsBody:    seatsOf(subOwner, "Ada"),
	}
	h, dir, _ := subSurface(t, script)
	errc := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			rec := doTeam(h, http.MethodGet, "/api/team/subscriptions", "")
			if rec.Code != http.StatusOK {
				errc <- fmt.Errorf("status %d", rec.Code)
				return
			}
			errc <- nil
		}()
	}
	for i := 0; i < 2; i++ {
		if err := <-errc; err != nil {
			t.Fatal(err)
		}
	}
	subs := loadSubFile(t, dir)
	if len(subs) != 1 || subs[0].Chat != subChat || subs[0].State != "live" {
		t.Fatalf("stored = %+v", subs)
	}
}

func TestSubListDoesNotHoldTheLockDuringTheCall(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(letGo)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hello":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, helloJSON(teamServerID, "Lab relay"))
		case "/v1/channels":
			once.Do(func() { close(entered) })
			<-release
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, channelsOf(guestChannel(subOwner, subChat, "Synthetic title", subGen, "read", 1)))
		case "/v1/seats":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, seatsOf(subOwner, "Ada"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doTeam(h, http.MethodGet, "/api/team/subscriptions", "")
	}()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case <-entered:
	case <-timer.C:
		t.Fatal("the list did not reach the team server")
	}
	other := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		other <- doTeam(h, http.MethodGet, "/api/team/servers", "")
	}()
	rec := recvTeam(t, other, "server list")
	if rec.Code != http.StatusOK {
		t.Fatalf("servers while a list is in flight = %d %s", rec.Code, rec.Body.String())
	}
	letGo()
	list := recvTeam(t, done, "subscriptions")
	if list.Code != http.StatusOK {
		t.Fatalf("list = %d %s", list.Code, list.Body.String())
	}
}

func TestSubListDeliveryOkBeforeTheFirstAsk(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(letGo)
	var lateHits int
	late := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		lateHits++
	}))
	t.Cleanup(late.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hello":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, helloJSON(teamServerID, "Lab relay"))
		case "/v1/channels":
			once.Do(func() { close(entered) })
			<-release
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, channelsOf(guestChannel(subOwner, subChat, "Synthetic title", subGen, "read", 1)))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doTeam(h, http.MethodGet, "/api/team/subscriptions", "")
	}()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case <-entered:
	case <-timer.C:
		t.Fatal("the list did not reach the team server")
	}
	if err := team.Remove(dir, teamServerID); err != nil {
		t.Fatal(err)
	}
	joinLab(t, dir, teamOtherID, late.URL)
	letGo()
	rec := recvTeam(t, done, "subscriptions")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	var got subListBody
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("body %s: %v", rec.Body.String(), err)
	}
	if lateHits != 0 {
		t.Fatalf("the server joined during the call was asked %d times", lateHits)
	}
	if len(got.Servers) != 1 || got.Servers[0].ID != teamOtherID || got.Servers[0].Delivery != "ok" {
		t.Fatalf("servers = %+v", got.Servers)
	}
	if len(got.Subscriptions) != 0 {
		t.Fatalf("a removed server was still applied: %+v", got.Subscriptions)
	}
	assertNoSubsFile(t, dir)
}

func TestSubListHelloUnreachableChangesNothing(t *testing.T) {
	script := &subScript{dropPath: "/v1/hello"}
	h, dir, _ := subSurface(t, script)
	seedSub(t, dir, baseSub())
	before, err := os.ReadFile(filepath.Join(dir, team.SubsFile))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := readSubList(t, h)
	if got.Servers[0].Delivery != "offline" || mustFindSub(t, got, subOwner, subChat, subGen).State != "live" {
		t.Fatalf("list = %+v", got)
	}
	after, err := os.ReadFile(filepath.Join(dir, team.SubsFile))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("an unreachable hello rewrote the subscription file")
	}
	for _, c := range script.snapshot() {
		if c.path != "/v1/hello" || c.auth != "" {
			t.Fatalf("calls = %+v", script.snapshot())
		}
	}
}

func TestSubListSeatsFailureDoesNotDropAGoodList(t *testing.T) {
	t.Run("not protocol", func(t *testing.T) {
		script := &subScript{
			channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic title", subGen, "read", 1)),
			seatsBody:    "nope",
		}
		h, _, _ := subSurface(t, script)
		got, _ := readSubList(t, h)
		one := mustFindSub(t, got, subOwner, subChat, subGen)
		if got.Servers[0].Delivery != "ok" || one.State != "live" || one.OwnerName != "" || one.Title != "Synthetic title" {
			t.Fatalf("list = %+v", got)
		}
	})
	t.Run("unauthorized", func(t *testing.T) {
		script := &subScript{
			channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic title", subGen, "read", 1)),
			seatsStatus:  http.StatusUnauthorized,
		}
		h, _, _ := subSurface(t, script)
		got, _ := readSubList(t, h)
		one := mustFindSub(t, got, subOwner, subChat, subGen)
		if got.Servers[0].Delivery != "refused" || one.State != "ended" || one.Title != "Synthetic title" {
			t.Fatalf("list = %+v", got)
		}
	})
}

func TestSubListRereadOfADamagedFileIsAnError(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(letGo)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/channels" {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, helloJSON(teamServerID, "Lab relay"))
			return
		}
		once.Do(func() { close(entered) })
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, channelsOf(guestChannel(subOwner, subChat, "Synthetic title", subGen, "read", 1)))
	}))
	t.Cleanup(srv.Close)
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	path := filepath.Join(dir, team.SubsFile)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doTeam(h, http.MethodGet, "/api/team/subscriptions", "")
	}()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case <-entered:
	case <-timer.C:
		t.Fatal("the list did not reach the team server")
	}
	if err := os.WriteFile(path, []byte("nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	letGo()
	rec := recvTeam(t, done, "subscriptions")
	assertAPIError(t, rec, http.StatusInternalServerError, "store_unavailable")
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "nope\n" {
		t.Fatalf("file = %q err %v", raw, err)
	}
}

func TestSubListDeliveryWhileFirstAskIsPending(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(letGo)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hello":
			_, _ = io.WriteString(w, helloJSON(teamServerID, "Lab relay"))
		case "/v1/channels":
			once.Do(func() { close(entered) })
			<-release
			_, _ = io.WriteString(w, `{"channels":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doTeam(h, http.MethodGet, "/api/team/subscriptions", "") }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the list did not reach the team server")
	}
	got, _ := readSubList(t, h)
	letGo()
	if len(got.Servers) != 1 || got.Servers[0].Delivery != "ok" {
		t.Fatalf("servers while the first ask is pending = %+v", got.Servers)
	}
	_ = recvTeam(t, done, "subscriptions")
}

func TestSubListSkipsInvalidIdentitiesAlongsideValidChannels(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(
		guestChannel(subOwner, subChat, "Synthetic title", subGen, "read", 1),
		guestChannel("bad", subChatB, "Synthetic title", subGen, "read", 1),
		guestChannel(subOwnerB, subChatB, "Synthetic title", "bad", "read", 1),
	), seatsBody: seatsOf(subOwner, "Ada")}
	h, dir, _ := subSurface(t, script)
	got, _ := readSubList(t, h)
	if len(got.Subscriptions) != 1 || mustFindSub(t, got, subOwner, subChat, subGen).State != "live" || *got.Truncated {
		t.Fatalf("list = %+v", got)
	}
	if len(loadSubFile(t, dir)) != 1 {
		t.Fatal("invalid identities were stored")
	}
	if _, err := os.Stat(filepath.Join(dir, team.CopiesDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("copies stat = %v", err)
	}
}

func TestSubListSaveFailureReportsUnavailable(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic title", subGen, "read", 1))}
	h, dir, _ := subSurface(t, script)
	asTeamHandler(t, h).saveSubs = func(string, []team.Subscription) error { return errors.New("synthetic failure") }
	rec := doTeam(h, http.MethodGet, "/api/team/subscriptions", "")
	assertAPIError(t, rec, http.StatusInternalServerError, "store_unavailable")
	assertNoSubsFile(t, dir)
}

func TestSubListUnauthorizedKeepsEndedSubscription(t *testing.T) {
	script := &subScript{channelsStatus: http.StatusUnauthorized}
	h, dir, _ := subSurface(t, script)
	sub := baseSub()
	sub.State = "ended"
	seedSub(t, dir, sub)
	got, _ := readSubList(t, h)
	if mustFindSub(t, got, subOwner, subChat, subGen).State != "ended" || got.Servers[0].Delivery != "refused" {
		t.Fatalf("list = %+v", got)
	}
}

func TestSubListRereadOfDamagedServersIsAnError(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/channels" {
			close(entered)
			<-release
			_, _ = io.WriteString(w, `{"channels":[]}`)
			return
		}
		_, _ = io.WriteString(w, helloJSON(teamServerID, "Lab relay"))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(letGo)
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doTeam(h, http.MethodGet, "/api/team/subscriptions", "") }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the list did not reach the team server")
	}
	if err := os.WriteFile(filepath.Join(dir, team.File), []byte("nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	letGo()
	assertAPIError(t, recvTeam(t, done, "subscriptions"), http.StatusInternalServerError, "store_unavailable")
}

func TestSubListForgetsServerDuringRosterRequest(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hello":
			_, _ = io.WriteString(w, helloJSON(teamServerID, "Lab relay"))
		case "/v1/channels":
			_, _ = io.WriteString(w, channelsOf(guestChannel(subOwner, subChat, "Synthetic title", subGen, "read", 1)))
		case "/v1/seats":
			close(entered)
			<-release
			_, _ = io.WriteString(w, seatsOf(subOwner, "Ada"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(letGo)
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doTeam(h, http.MethodGet, "/api/team/subscriptions", "") }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the list did not reach the roster")
	}
	if err := team.Remove(dir, teamServerID); err != nil {
		t.Fatal(err)
	}
	letGo()
	rec := recvTeam(t, done, "subscriptions")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	var got subListBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Servers) != 0 || len(got.Subscriptions) != 0 {
		t.Fatalf("list = %+v", got)
	}
	assertNoSubsFile(t, dir)
}

func subReadPath() string {
	return "/api/team/subscriptions/" + teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen
}

func subRecord(seq int, body string) string {
	return fmt.Sprintf(`{"seq":%d,"key":"k%d","author":"%s","at":"2026-11-14T09:30:00.000Z","body":%s}`, seq, seq, subOwner, body)
}

func subPage(gen string, last int, more bool, entries ...string) string {
	return fmt.Sprintf(`{"generation":"%s","entries":[%s],"last":%d,"more":%t}`, gen, strings.Join(entries, ","), last, more)
}

type readSubBody struct {
	View struct {
		subView
		Delivery string `json:"delivery"`
	} `json:"view"`
	Entries []map[string]any `json:"entries"`
	Last    uint64           `json:"last"`
	Next    uint64           `json:"next"`
	More    bool             `json:"more"`
}

func decodeSubRead(t *testing.T, rec *httptest.ResponseRecorder) readSubBody {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	assertNoSecrets(t, rec.Body.String(), "", teamToken)
	var body readSubBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func readSurface(t *testing.T, mirror func(http.ResponseWriter, *http.Request)) (http.Handler, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			_, _ = io.WriteString(w, helloJSON(teamServerID, "Lab relay"))
			return
		}
		mirror(w, r)
	}))
	t.Cleanup(srv.Close)
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	seedSub(t, dir, baseSub())
	return h, dir
}

func TestSubReadFetchesCopiesAndProjects(t *testing.T) {
	var queries []string
	bodies := []string{
		`{"v":1,"t":"start","scope":"from_now","time":"2026-11-14T09:30:00.000Z"}`,
		`{"v":1,"t":"user","time":"2026-11-14T09:30:00.000Z","text":"synthetic text"}`,
		`{"v":1,"t":"assistant","time":"2026-11-14T09:30:00.000Z","text":"synthetic output","agent":"grok","prov":true}`,
		`{"v":1,"t":"assistant","time":"2026-11-14T09:30:00.000Z","omitted":"too_large"}`,
		`{"v":1,"t":"source","time":"2026-11-14T09:30:00.000Z","reason":"clear"}`,
		`{"v":1,"t":"unknown"}`, `{"v":2,"t":"user","text":"ignored"}`, `42`,
	}
	var entries []string
	for i, body := range bodies {
		entries = append(entries, subRecord(i+1, body))
	}
	h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		if r.URL.Query().Get("after") == "0" {
			_, _ = io.WriteString(w, subPage(subGen, 8, false, entries...))
			return
		}
		_, _ = io.WriteString(w, subPage(subGen, 8, false))
	})
	first := decodeSubRead(t, doTeam(h, http.MethodGet, subReadPath(), ""))
	second := decodeSubRead(t, doTeam(h, http.MethodGet, subReadPath(), ""))
	if first.Last != 8 || first.More || first.Next != 8 || len(first.Entries) != 5 || first.View.Delivery != "ok" || !first.View.Unread {
		t.Fatalf("first = %+v", first)
	}
	if !reflect.DeepEqual(first.Entries, second.Entries) || second.Last != 8 || strings.Join(queries, ",") != "after=0,after=8" {
		t.Fatalf("second = %+v queries = %v", second, queries)
	}
	for i, entry := range first.Entries {
		if entry["seq"] != float64(i+1) || entry["key"] != nil || entry["author"] != nil {
			t.Fatalf("entry = %+v", entry)
		}
	}
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	stored, last, err := team.ReadCopy(dir, id, team.MirrorLog, 0, 256, 4<<20)
	if err != nil || last != 8 || len(stored) != 8 {
		t.Fatalf("copy last = %d count = %d err = %v", last, len(stored), err)
	}
	for i := range stored {
		if string(stored[i].Body) != bodies[i] {
			t.Fatalf("body %d changed", i)
		}
	}
	after := decodeSubRead(t, doTeam(h, http.MethodGet, subReadPath()+"?after=3", ""))
	if len(after.Entries) != 2 || after.Entries[0]["seq"] != float64(4) {
		t.Fatalf("after = %+v", after)
	}
}

func TestSubReadPageLimitAndResultLimit(t *testing.T) {
	var queries []string
	h, _ := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query().Get("after"))
		n, _ := strconv.Atoi(r.URL.Query().Get("after"))
		entries := make([]string, 0, 64)
		for i := n + 1; i <= n+64; i++ {
			entries = append(entries, subRecord(i, `{"v":1,"t":"user","time":"2026-11-14T09:30:00.000Z","text":"x"}`))
		}
		_, _ = io.WriteString(w, subPage(subGen, 1000, true, entries...))
	})
	got := decodeSubRead(t, doTeam(h, http.MethodGet, subReadPath(), ""))
	if len(queries) != 8 || got.Last != 512 || got.Next != 256 || len(got.Entries) != 256 || !got.More {
		t.Fatalf("queries %v result %+v", queries, got)
	}
}

func TestSubReadOutcomesKeepTheCopy(t *testing.T) {
	for _, tc := range []struct {
		name            string
		status          int
		page            string
		delivery, state string
	}{
		{"generation", 200, subPage(subGenB, 0, false), "ok", "ended"},
		{"missing", 404, "", "ok", "ended"},
		{"revoked", 401, "", "refused", "ended"},
		{"bad page", 200, "bad", "changed", "live"},
		{"gap", 200, subPage(subGen, 2, false, subRecord(2, `{"v":1,"t":"user"}`)), "changed", "live"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.page)
			})
			got := decodeSubRead(t, doTeam(h, http.MethodGet, subReadPath(), ""))
			if got.View.State != tc.state || got.View.Delivery != tc.delivery || got.Last != 0 || len(got.Entries) != 0 {
				t.Fatalf("read = %+v", got)
			}
		})
	}
}

func TestSubReadValidationAndSeen(t *testing.T) {
	h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, subPage(subGen, 2, false, subRecord(1, `{"v":1,"t":"user","time":"2026-11-14T09:30:00.000Z","text":"x"}`), subRecord(2, `{"v":1,"t":"user","time":"2026-11-14T09:30:00.000Z","text":"y"}`)))
	})
	decodeSubRead(t, doTeam(h, http.MethodGet, subReadPath(), ""))
	for _, body := range []string{`{}`, `{"mirror":-1}`, `{"mirror":1,"other":0}`, `{"mirror":1} {}`, strings.Repeat(" ", 1025)} {
		assertAPIError(t, doTeam(h, http.MethodPut, subReadPath()+"/seen", body), http.StatusBadRequest, "bad_request")
	}
	for _, n := range []int{1, 0, 100} {
		rec := doTeam(h, http.MethodPut, subReadPath()+"/seen", fmt.Sprintf(`{"mirror":%d}`, n))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("seen %d = %d %s", n, rec.Code, rec.Body.String())
		}
	}
	subs := loadSubFile(t, dir)
	if subs[0].SeenMirror != 2 {
		t.Fatalf("seen = %d", subs[0].SeenMirror)
	}
	assertAPIError(t, doTeam(h, http.MethodGet, subReadPath()+"?after=-1", ""), http.StatusBadRequest, "bad_request")
	assertAPIError(t, doTeam(h, http.MethodGet, subReadPath()+"x", ""), http.StatusNotFound, "not_found")
	assertAPIError(t, doTeam(h, http.MethodPut, subReadPath()+"x/seen", `{"mirror":1}`), http.StatusNotFound, "not_found")
}

func TestSubReadEndedUsesCopyWithoutServer(t *testing.T) {
	var hits int
	h, dir := readSurface(t, func(http.ResponseWriter, *http.Request) { hits++ })
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	entry := team.MirrorRecord{Seq: 1, Key: "k1", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: json.RawMessage(`{"v":1,"t":"user","time":"2026-11-14T09:30:00.000Z","text":"x"}`)}
	if err := team.AppendCopy(dir, id, team.MirrorLog, []team.MirrorRecord{entry}); err != nil {
		t.Fatal(err)
	}
	subs := loadSubFile(t, dir)
	subs[0].State = "ended"
	if err := team.SaveSubs(dir, subs); err != nil {
		t.Fatal(err)
	}
	got := decodeSubRead(t, doTeam(h, http.MethodGet, subReadPath(), ""))
	if hits != 0 || len(got.Entries) != 1 || got.Last != 1 || got.View.State != "ended" {
		t.Fatalf("read = %+v, hits = %d", got, hits)
	}
}

func TestSubReadOfflineAndDamagedCopy(t *testing.T) {
	h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			panic(err)
		}
		_ = c.Close()
	})
	got := decodeSubRead(t, doTeam(h, http.MethodGet, subReadPath(), ""))
	if got.View.Delivery != "offline" || got.View.State != "live" {
		t.Fatalf("read = %+v", got)
	}
	path := filepath.Join(dir, team.CopiesDir, teamServerID, subOwner, subChat, subGen, "mirror.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertAPIError(t, doTeam(h, http.MethodGet, subReadPath(), ""), 500, "copy_damaged")
}

func TestSubReadConcurrentStoresOnce(t *testing.T) {
	h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("after") == "0" {
			_, _ = io.WriteString(w, subPage(subGen, 1, false, subRecord(1, `{"v":1,"t":"user","time":"2026-11-14T09:30:00.000Z","text":"x"}`)))
			return
		}
		_, _ = io.WriteString(w, subPage(subGen, 1, false))
	})
	var wg sync.WaitGroup
	results := make(chan *httptest.ResponseRecorder, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- doTeam(h, http.MethodGet, subReadPath(), "") }()
	}
	wg.Wait()
	close(results)
	for rec := range results {
		got := decodeSubRead(t, rec)
		if got.Last != 1 || len(got.Entries) != 1 {
			t.Fatalf("read = %+v", got)
		}
	}
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	entries, last, err := team.ReadCopy(dir, id, team.MirrorLog, 0, 256, 4<<20)
	if err != nil || last != 1 || len(entries) != 1 {
		t.Fatalf("copy last %d entries %d err %v", last, len(entries), err)
	}
}

func TestSubReadRefusesBadEntries(t *testing.T) {
	cases := []string{
		strings.Replace(subRecord(1, `{}`), `"key":"k1"`, `"key":"!"`, 1),
		strings.Replace(subRecord(1, `{}`), `2026-11-14T09:30:00.000Z`, `yesterday`, 1),
		subRecord(1, `"`+strings.Repeat("x", 4<<20)+`"`),
	}
	for i, item := range cases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, subPage(subGen, 1, false, item))
			})
			got := decodeSubRead(t, doTeam(h, http.MethodGet, subReadPath(), ""))
			if got.View.Delivery != "changed" || got.View.State != "live" || got.Last != 0 {
				t.Fatalf("read = %+v", got)
			}
			if _, err := os.Stat(filepath.Join(dir, team.CopiesDir)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("copy stat = %v", err)
			}
		})
	}
}

func TestSubReadStorageBudgetAndTextLimit(t *testing.T) {
	t.Run("full", func(t *testing.T) {
		h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, subPage(subGen, 1, false, subRecord(1, `{"v":1,"t":"user","text":"x"}`)))
		})
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"storage_global_limit_bytes":1,"storage_min_free_bytes":0}`), 0o600); err != nil {
			t.Fatal(err)
		}
		assertAPIError(t, doTeam(h, http.MethodGet, subReadPath(), ""), 507, "storage_full")
	})
	t.Run("text", func(t *testing.T) {
		text := strings.Repeat("x", 3<<20)
		item := `{"v":1,"t":"user","time":"2026-11-14T09:30:00.000Z","text":"` + text + `"}`
		h, _ := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, subPage(subGen, 2, false, subRecord(1, item), subRecord(2, item)))
		})
		got := decodeSubRead(t, doTeam(h, http.MethodGet, subReadPath(), ""))
		if len(got.Entries) != 1 || got.Last != 2 || got.Next != 1 || !got.More {
			t.Fatalf("entries = %d last = %d more = %v", len(got.Entries), got.Last, got.More)
		}
	})
}

func TestSubReadUnknownServerAndStoreFailures(t *testing.T) {
	h, dir := readSurface(t, func(http.ResponseWriter, *http.Request) { t.Fatal("forgotten server contacted") })
	if err := team.Remove(dir, teamServerID); err != nil {
		t.Fatal(err)
	}
	got := decodeSubRead(t, doTeam(h, http.MethodGet, subReadPath(), ""))
	if got.Last != 0 {
		t.Fatalf("read = %+v", got)
	}
	assertAPIError(t, doTeam(h, http.MethodGet, strings.Replace(subReadPath(), subOwner, subOwnerB, 1), ""), 404, "not_found")
	if err := os.WriteFile(filepath.Join(dir, team.SubsFile), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertAPIError(t, doTeam(h, http.MethodGet, subReadPath(), ""), 500, "store_unavailable")
	assertAPIError(t, doTeam(h, http.MethodPut, subReadPath()+"/seen", `{"mirror":1}`), 500, "store_unavailable")
}

func TestSubReadAdditionalValidation(t *testing.T) {
	h, _ := readSurface(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, subPage(subGen, 0, false)) })
	for _, query := range []string{"?after=", "?after=1&after=2"} {
		assertAPIError(t, doTeam(h, http.MethodGet, subReadPath()+query, ""), 400, "bad_request")
	}
	for _, path := range []string{strings.Replace(subReadPath(), subOwner, strings.Repeat("z", 32), 1), subReadPath() + "/other"} {
		assertAPIError(t, doTeam(h, http.MethodGet, path, ""), 404, "not_found")
	}
	assertAPIError(t, doTeam(h, http.MethodPost, subReadPath(), ""), 404, "not_found")
	assertAPIError(t, doTeam(h, http.MethodPut, strings.Replace(subReadPath(), subOwner, subOwnerB, 1)+"/seen", `{"mirror":1}`), 404, "not_found")
}

func TestSubReadIgnoresMalformedBodyFields(t *testing.T) {
	var items []string
	for i, body := range []string{`null`, `{"v":1,"t":"assistant","prov":"wrong"}`, `{"v":1,"t":"user","text":42}`, `{"v":1,"t":false}`} {
		items = append(items, subRecord(i+1, body))
	}
	h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, subPage(subGen, 4, false, items...))
	})
	got := decodeSubRead(t, doTeam(h, http.MethodGet, subReadPath(), ""))
	if len(got.Entries) != 0 || got.Last != 4 {
		t.Fatalf("read = %+v", got)
	}
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	entries, _, err := team.ReadCopy(dir, id, team.MirrorLog, 0, 256, 4<<20)
	if err != nil || len(entries) != 4 {
		t.Fatalf("entries = %d err %v", len(entries), err)
	}
}

func TestSubSeenDamagedCopyAndSaveFailure(t *testing.T) {
	h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, subPage(subGen, 1, false, subRecord(1, `{"v":1,"t":"user","text":"x"}`)))
	})
	decodeSubRead(t, doTeam(h, http.MethodGet, subReadPath(), ""))
	asTeamHandler(t, h).saveSubs = func(string, []team.Subscription) error { return errors.New("synthetic failure") }
	assertAPIError(t, doTeam(h, http.MethodPut, subReadPath()+"/seen", `{"mirror":1}`), 500, "store_unavailable")
	asTeamHandler(t, h).saveSubs = nil
	path := filepath.Join(dir, team.CopiesDir, teamServerID, subOwner, subChat, subGen, "mirror.jsonl")
	if err := os.WriteFile(path, []byte("broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertAPIError(t, doTeam(h, http.MethodPut, subReadPath()+"/seen", `{"mirror":1}`), 500, "store_unavailable")
}

func TestSubReadEndingSaveFailure(t *testing.T) {
	h, _ := readSurface(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	asTeamHandler(t, h).saveSubs = func(string, []team.Subscription) error { return errors.New("synthetic failure") }
	assertAPIError(t, doTeam(h, http.MethodGet, subReadPath(), ""), 500, "store_unavailable")
}

func TestSubReadStoreChangesDuringFetch(t *testing.T) {
	for _, damage := range []bool{false, true} {
		t.Run(strconv.FormatBool(damage), func(t *testing.T) {
			var dir string
			h, d := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
				if damage {
					if err := os.WriteFile(filepath.Join(dir, team.SubsFile), []byte("broken"), 0o600); err != nil {
						t.Error(err)
					}
				} else {
					subs, err := team.LoadSubs(dir)
					if err != nil {
						t.Error(err)
					} else {
						subs[0].State = "ended"
						if err := team.SaveSubs(dir, subs); err != nil {
							t.Error(err)
						}
					}
				}
				w.WriteHeader(http.StatusNotFound)
			})
			dir = d
			rec := doTeam(h, http.MethodGet, subReadPath(), "")
			if damage {
				assertAPIError(t, rec, 500, "store_unavailable")
			} else {
				got := decodeSubRead(t, rec)
				if got.View.State != "ended" {
					t.Fatalf("read = %+v", got)
				}
			}
		})
	}
}

func TestSubReadCopyDamagedDuringFetch(t *testing.T) {
	for _, status := range []int{200, 404} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var dir string
			h, d := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
				path := filepath.Join(dir, team.CopiesDir, teamServerID, subOwner, subChat, subGen, "mirror.jsonl")
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Error(err)
				}
				if err := os.WriteFile(path, []byte("broken\n"), 0o600); err != nil {
					t.Error(err)
				}
				w.WriteHeader(status)
				if status == 200 {
					_, _ = io.WriteString(w, subPage(subGen, 1, false, subRecord(1, `{}`)))
				}
			})
			dir = d
			assertAPIError(t, doTeam(h, http.MethodGet, subReadPath(), ""), 500, "copy_damaged")
		})
	}
}

func TestSubReadChangedHelloSendsNoCredential(t *testing.T) {
	var authed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			authed = true
		}
		_, _ = io.WriteString(w, helloJSON(teamOtherID, "Lab relay"))
	}))
	t.Cleanup(srv.Close)
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	seedSub(t, dir, baseSub())
	got := decodeSubRead(t, doTeam(h, http.MethodGet, subReadPath(), ""))
	if got.View.Delivery != "changed" || authed {
		t.Fatalf("delivery = %s auth = %t", got.View.Delivery, authed)
	}
}

func TestSubReadReplacedGenerationSaveFailure(t *testing.T) {
	h, _ := readSurface(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, subPage(subGenB, 0, false)) })
	asTeamHandler(t, h).saveSubs = func(string, []team.Subscription) error { return errors.New("synthetic failure") }
	assertAPIError(t, doTeam(h, http.MethodGet, subReadPath(), ""), 500, "store_unavailable")
}

func TestSubSeenRequiresCSRF(t *testing.T) {
	cmd, data, _, _ := startTeamCommand(t, true, true)
	seedSub(t, data, baseSub())
	core, err := newCoreMux(cmd.application)
	if err != nil {
		t.Fatal(err)
	}
	web, err := newWebBackend(webBackendConfig{Web: webFS, Core: core, RequestPolicy: cmd.application.requestPolicy})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "http://127.0.0.1"+subReadPath()+"/seen", strings.NewReader(`{"mirror":1}`))
	req.Host = "127.0.0.1"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	web.local.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("without CSRF = %d", rec.Code)
	}
}

func TestSubSeenDoesNotContactServer(t *testing.T) {
	var hits int
	h, _ := readSurface(t, func(http.ResponseWriter, *http.Request) { hits++ })
	rec := doTeam(h, http.MethodPut, subReadPath()+"/seen", `{"mirror":1}`)
	if rec.Code != http.StatusNoContent || hits != 0 {
		t.Fatalf("seen = %d, hits = %d", rec.Code, hits)
	}
}

func TestTeamOtherRoutesDoNotListChannels(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.URL.Path == "/v1/hello" {
			_, _ = io.WriteString(w, helloJSON(teamServerID, "Lab relay"))
			return
		}
		if r.URL.Path == "/v1/seats" {
			_, _ = io.WriteString(w, `{"seats":[]}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	for _, path := range []string{"/api/team/servers", "/api/team/shares", "/api/team/servers/" + teamServerID + "/seats"} {
		rec := doTeam(h, http.MethodGet, path, "")
		if rec.Code != 200 {
			t.Fatalf("%s = %d", path, rec.Code)
		}
	}
	for _, path := range paths {
		if path == "GET /v1/channels" {
			t.Fatal("other routes listed channels")
		}
	}
}

func TestSubListRosterAttemptThrottledRegardlessOfOutcome(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		ownerName string
	}{{"failed with name", 500, "Ada"}, {"failed without name", 500, ""}, {"missing owner", 200, ""}} {
		t.Run(tc.name, func(t *testing.T) {
			script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic title", subGen, "read", 2)), seatsStatus: tc.status}
			h, dir, _ := subSurface(t, script)
			sub := baseSub()
			sub.OwnerName = tc.ownerName
			seedSub(t, dir, sub)
			clock := holdClock(t, h)
			readSubList(t, h)
			*clock = clock.Add(11 * time.Second)
			readSubList(t, h)
			if n := countSubPath(script.snapshot(), "/v1/seats"); n != 1 {
				t.Fatalf("roster calls before ten minutes = %d", n)
			}
			*clock = clock.Add(10 * time.Minute)
			readSubList(t, h)
			if n := countSubPath(script.snapshot(), "/v1/seats"); n != 2 {
				t.Fatalf("roster calls after ten minutes = %d", n)
			}
		})
	}
}

func TestSubReadRejectsMalformedEscapesInAfter(t *testing.T) {
	h, _ := readSurface(t, func(w http.ResponseWriter, r *http.Request) { t.Error("malformed query contacted server") })
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+subReadPath(), nil)
	req.URL.RawQuery = "after=%zz"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assertAPIError(t, rec, 400, "bad_request")
}

func TestSubReadIgnoresNullTypedFields(t *testing.T) {
	items := []string{subRecord(1, `{"v":1,"t":"user","text":null}`), subRecord(2, `{"v":1,"t":"assistant","prov":null}`)}
	h, _ := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, subPage(subGen, 2, false, items...))
	})
	got := decodeSubRead(t, doTeam(h, http.MethodGet, subReadPath(), ""))
	if len(got.Entries) != 0 || got.Last != 2 {
		t.Fatalf("read = %+v", got)
	}
}

func TestSubEndedIsNeverUnread(t *testing.T) {
	h, dir := testTeamSurface(t, team.NewClient())
	sub := baseSub()
	sub.State = "ended"
	sub.MirrorLast = 7
	seedSub(t, dir, sub)
	list, _ := readSubList(t, h)
	if s := mustFindSub(t, list, subOwner, subChat, subGen); s.Unread {
		t.Fatalf("ended list = %+v", s)
	}
	view := decodeSubRead(t, doTeam(h, http.MethodGet, subReadPath(), ""))
	if view.View.Unread {
		t.Fatalf("ended view = %+v", view.View)
	}
}
func TestSubNewGenerationCopiesOwnerName(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic title", subGen, "read", 1)), seatsBody: seatsOf(subOwner, "Ada")}
	h, _, _ := subSurface(t, script)
	clock := holdClock(t, h)
	readSubList(t, h)
	*clock = clock.Add(10 * time.Second)
	script.mu.Lock()
	script.channelsBody = channelsOf(guestChannel(subOwner, subChat, "Synthetic title", subGenB, "read", 0))
	script.mu.Unlock()
	got, _ := readSubList(t, h)
	if s := mustFindSub(t, got, subOwner, subChat, subGenB); s.OwnerName != "Ada" {
		t.Fatalf("new generation = %+v", s)
	}
	if n := countSubPath(script.snapshot(), "/v1/seats"); n != 1 {
		t.Fatalf("roster calls = %d", n)
	}
}
func TestSubNewMissingOwnerAsksRosterOnlyOnce(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic title", subGen, "read", 1)), seatsBody: seatsOf(subOwner, "Ada")}
	h, _, _ := subSurface(t, script)
	clock := holdClock(t, h)
	readSubList(t, h)
	*clock = clock.Add(10 * time.Second)
	script.mu.Lock()
	script.channelsBody = channelsOf(guestChannel(subOwner, subChat, "Synthetic title", subGen, "read", 1), guestChannel(subOwnerB, subChatB, "Synthetic title", subGen, "read", 1))
	script.mu.Unlock()
	got, _ := readSubList(t, h)
	if s := mustFindSub(t, got, subOwnerB, subChatB, subGen); s.OwnerName != "" {
		t.Fatalf("unknown owner = %+v", s)
	}
	*clock = clock.Add(10 * time.Second)
	readSubList(t, h)
	if n := countSubPath(script.snapshot(), "/v1/seats"); n != 2 {
		t.Fatalf("roster calls = %d", n)
	}
}

func TestSubReadNextTracksUnknownEntries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		bodies   []string
		after    string
		wantNext uint64
		count    int
	}{
		{"middle", []string{`{"v":1,"t":"user","text":"x"}`, `{"v":2,"t":"user"}`, `{"v":1,"t":"user","text":"y"}`}, "", 3, 2},
		{"only", []string{`{"v":2,"t":"user"}`, `42`}, "", 2, 0},
		{"past end", []string{`{"v":2,"t":"user"}`}, "?after=99", 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) { t.Fatal("ended subscription contacted server") })
			subs := loadSubFile(t, dir)
			subs[0].State = "ended"
			if err := team.SaveSubs(dir, subs); err != nil {
				t.Fatal(err)
			}
			id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
			var entries []team.MirrorRecord
			for i, body := range tc.bodies {
				entries = append(entries, team.MirrorRecord{Seq: uint64(i + 1), Key: fmt.Sprintf("k%d", i+1), Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: json.RawMessage(body)})
			}
			if err := team.AppendCopy(dir, id, team.MirrorLog, entries); err != nil {
				t.Fatal(err)
			}
			got := decodeSubRead(t, doTeam(h, http.MethodGet, subReadPath()+tc.after, ""))
			if got.Next != tc.wantNext || got.More || len(got.Entries) != tc.count {
				t.Fatalf("read = %+v", got)
			}
		})
	}
}

func TestSubBranchInvalidPathPartsNeverReadState(t *testing.T) {
	h, dir := testTeamSurface(t, team.NewClient())
	path := filepath.Join(dir, team.SubsFile)
	if err := os.WriteFile(path, []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	serverPath := filepath.Join(dir, team.File)
	if err := os.WriteFile(serverPath, []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	good := []string{teamServerID, subOwner, subChat, subGen}
	for i := range good {
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			t.Run(fmt.Sprintf("part%d_%s", i, method), func(t *testing.T) {
				parts := append([]string(nil), good...)
				parts[i] = "bad"
				p := "/api/team/subscriptions/" + strings.Join(parts, "/")
				if method == http.MethodPut {
					p += "/seen"
				}
				assertAPIError(t, doTeam(h, method, p, `{"mirror":1}`), 404, "not_found")
			})
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "damaged" {
		t.Fatal("invalid path changed state")
	}
	raw, err = os.ReadFile(serverPath)
	if err != nil || string(raw) != "damaged" {
		t.Fatal("invalid path changed server state")
	}
}

func TestSubBranchUnknownFifthPartDoesNothing(t *testing.T) {
	h, dir := testTeamSurface(t, team.NewClient())
	seedSub(t, dir, baseSub())
	path := filepath.Join(dir, team.SubsFile)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	assertAPIError(t, doTeam(h, http.MethodPut, subReadPath()+"/other", `{"mirror":1}`), 404, "not_found")
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("unknown path changed state")
	}
}

func TestSubBranchUpdateOneFieldAtATime(t *testing.T) {
	for _, field := range []string{"access", "title"} {
		t.Run(field, func(t *testing.T) {
			script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic title", subGen, "read", 2)), seatsBody: seatsOf(subOwner, "Ada")}
			h, dir, _ := subSurface(t, script)
			seedSub(t, dir, baseSub())
			clock := holdClock(t, h)
			readSubList(t, h)
			*clock = clock.Add(10 * time.Second)
			title, access := "Synthetic title", "read"
			if field == "title" {
				title = "Other title"
			} else {
				access = "write"
			}
			script.mu.Lock()
			script.channelsBody = channelsOf(guestChannel(subOwner, subChat, title, subGen, access, 2))
			script.mu.Unlock()
			got, _ := readSubList(t, h)
			one := mustFindSub(t, got, subOwner, subChat, subGen)
			if one.Title != title || one.Access != access || one.MirrorLast != 2 {
				t.Fatalf("updated = %+v", one)
			}
			stored := loadSubFile(t, dir)
			if stored[0].Title != title || stored[0].Access != access {
				t.Fatal("update was not stored")
			}
		})
	}
}

func TestSubBranchSeenNeverShrinksOrRewrites(t *testing.T) {
	h, dir := testTeamSurface(t, team.NewClient())
	seedSub(t, dir, baseSub())
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	var recs []team.MirrorRecord
	for i := 1; i <= 5; i++ {
		recs = append(recs, team.MirrorRecord{Seq: uint64(i), Key: fmt.Sprintf("k%d", i), Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: json.RawMessage(`{}`)})
	}
	if err := team.AppendCopy(dir, id, team.MirrorLog, recs); err != nil {
		t.Fatal(err)
	}
	if r := doTeam(h, http.MethodPut, subReadPath()+"/seen", `{"mirror":5}`); r.Code != 204 {
		t.Fatalf("first = %d", r.Code)
	}
	path := filepath.Join(dir, team.SubsFile)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	asTeamHandler(t, h).saveSubs = func(string, []team.Subscription) error { writes++; return errors.New("unexpected write") }
	if r := doTeam(h, http.MethodPut, subReadPath()+"/seen", `{"mirror":2}`); r.Code != 204 {
		t.Fatalf("second = %d", r.Code)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) || writes != 0 {
		t.Fatal("seen file changed or was rewritten")
	}
	if s := loadSubFile(t, dir)[0]; s.SeenMirror != 5 {
		t.Fatalf("seen = %d", s.SeenMirror)
	}
}

func TestSubBranchRead401EndsOnlyItsServer(t *testing.T) {
	script := &subScript{mirrorStatus: 401}
	h, dir, srv := subSurface(t, script)
	joinLab(t, dir, teamOtherID, srv.URL)
	for _, server := range []string{teamServerID, teamOtherID} {
		for _, chat := range []string{subChat, subChatB} {
			s := baseSub()
			s.Server = server
			s.Chat = chat
			seedSub(t, dir, s)
		}
	}
	decodeSubRead(t, doTeam(h, http.MethodGet, subReadPath(), ""))
	for _, s := range loadSubFile(t, dir) {
		want := "live"
		if s.Server == teamServerID {
			want = "ended"
		}
		if s.State != want {
			t.Fatalf("server %s state %s", s.Server, s.State)
		}
	}
}

func TestSubBranchRosterOnlyForServerWithNewOwner(t *testing.T) {
	a := &subScript{}
	b := &subScript{helloID: teamOtherID, channelsBody: channelsOf(guestChannel(subOwnerB, subChatB, "Synthetic title", subGen, "read", 1)), seatsBody: seatsOf(subOwnerB, "Bea")}
	h, dir, _ := subSurface(t, a)
	bs := httptest.NewServer(b)
	t.Cleanup(bs.Close)
	joinLab(t, dir, teamOtherID, bs.URL)
	clock := holdClock(t, h)
	readSubList(t, h)
	*clock = clock.Add(10 * time.Second)
	a.mu.Lock()
	a.channelsBody = channelsOf(guestChannel(subOwner, subChat, "Synthetic title", subGen, "read", 1))
	a.seatsBody = seatsOf(subOwner, "Ada")
	a.mu.Unlock()
	got, _ := readSubList(t, h)
	if s := mustFindSub(t, got, subOwner, subChat, subGen); s.OwnerName != "Ada" {
		t.Fatalf("owner = %+v", s)
	}
	if na, nb := countSubPath(a.snapshot(), "/v1/seats"), countSubPath(b.snapshot(), "/v1/seats"); na != 1 || nb != 1 {
		t.Fatalf("roster counts = %d %d", na, nb)
	}
}

func TestSubBranchOwnerNameIsScopedToServer(t *testing.T) {
	a := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic title", subGen, "read", 1)), seatsBody: seatsOf(subOwner, "Ada")}
	b := &subScript{helloID: teamOtherID}
	h, dir, _ := subSurface(t, a)
	bs := httptest.NewServer(b)
	t.Cleanup(bs.Close)
	joinLab(t, dir, teamOtherID, bs.URL)
	clock := holdClock(t, h)
	readSubList(t, h)
	*clock = clock.Add(10 * time.Second)
	b.mu.Lock()
	b.channelsBody = channelsOf(guestChannel(subOwner, subChatB, "Synthetic title", subGen, "read", 1))
	b.mu.Unlock()
	got, _ := readSubList(t, h)
	found := false
	for _, s := range got.Subscriptions {
		if s.Server == teamOtherID && s.Owner == subOwner && s.Chat == subChatB {
			found = true
			if s.OwnerName != "" {
				t.Fatalf("wrong server name = %+v", s)
			}
		}
	}
	if !found {
		t.Fatal("missing subscription on second server")
	}
}

func TestSubBranchStorageFullReturnsExistingEntries(t *testing.T) {
	h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, subPage(subGen, 2, false, subRecord(2, `{"v":1,"t":"user","text":"y"}`)))
	})
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	if err := team.AppendCopy(dir, id, team.MirrorLog, []team.MirrorRecord{{Seq: 1, Key: "k1", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: json.RawMessage(`{"v":1,"t":"user","text":"x"}`)}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"storage_global_limit_bytes":1,"storage_min_free_bytes":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := decodeSubRead(t, doTeam(h, http.MethodGet, subReadPath(), ""))
	if got.Last != 1 || len(got.Entries) != 1 || got.Next != 1 {
		t.Fatalf("read = %+v", got)
	}
}

func TestSubBranchUnknownValidIdentityParts(t *testing.T) {
	h, dir := testTeamSurface(t, team.NewClient())
	seedSub(t, dir, baseSub())
	good := []string{teamServerID, subOwner, subChat, subGen}
	other := []string{teamOtherID, subOwnerB, subChatB, subGenB}
	for i := range good {
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			t.Run(fmt.Sprintf("part%d_%s", i, method), func(t *testing.T) {
				parts := append([]string(nil), good...)
				parts[i] = other[i]
				path := "/api/team/subscriptions/" + strings.Join(parts, "/")
				if method == http.MethodPut {
					path += "/seen"
				}
				assertAPIError(t, doTeam(h, method, path, `{"mirror":1}`), 404, "not_found")
			})
		}
	}
}

func TestSubBranchMalformedVersionAndKindAreOmitted(t *testing.T) {
	for i, body := range []string{`{"t":"user","text":"x"}`, `{"v":"one","t":"user"}`, `{"v":1}`, `{"v":1,"t":false}`} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			r := team.MirrorRecord{Seq: 1, At: "2026-11-14T09:30:00.000Z", Body: json.RawMessage(body)}
			if _, _, ok := projectSubRecord(r); ok {
				t.Fatal("malformed shape was projected")
			}
		})
	}
}

func TestSubBranchSkipsEmptyPriorOwnerName(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic title", subGen, "read", 1), guestChannel(subOwner, subChatB, "Synthetic title", subGen, "read", 1), guestChannel(subOwner, subChatC, "Synthetic title", subGen, "read", 1))}
	h, dir, _ := subSurface(t, script)
	a := baseSub()
	a.OwnerName = ""
	b := baseSub()
	b.Chat = subChatB
	b.OwnerName = "Ada"
	seedSub(t, dir, a)
	seedSub(t, dir, b)
	got, _ := readSubList(t, h)
	if s := mustFindSub(t, got, subOwner, subChatC, subGen); s.OwnerName != "Ada" {
		t.Fatalf("name = %q", s.OwnerName)
	}
}

func TestSubBranchSkipsNonMemberAccess(t *testing.T) {
	outcome := subOutcome{entry: team.Entry{ID: teamServerID, Name: "Lab One"}, channels: []team.Channel{{Owner: subOwner, Chat: subChat, Generation: subGen, Access: "owner"}}}
	subs, changed, truncated := applyChannels(nil, outcome, subOwnerB, fixedTeamClock()(), false)
	if len(subs) != 0 || changed || truncated {
		t.Fatalf("non-member access stored: %+v", subs)
	}
}

func TestSubBranchTruncationPersistsAcrossServers(t *testing.T) {
	h, dir := testTeamSurface(t, team.NewClient())
	joinLab(t, dir, teamServerID, "http://127.0.0.1:1")
	joinLab(t, dir, teamOtherID, "http://127.0.0.1:2")
	th := asTeamHandler(t, h)
	th.subAsks = map[string]subAsk{teamServerID: {truncated: true}, teamOtherID: {truncated: false}}
	view, err := th.commitSubs("", nil, nil, fixedTeamClock()())
	if err != nil {
		t.Fatal(err)
	}
	if !view.Truncated {
		t.Fatal("later server cleared truncation")
	}
}

func TestSubBranchGenerationReplacementStaysWithinIdentity(t *testing.T) {
	for _, field := range []string{"server", "owner", "chat"} {
		t.Run(field, func(t *testing.T) {
			a, b := baseSub(), baseSub()
			b.Generation = subGenB
			switch field {
			case "server":
				b.Server = teamOtherID
			case "owner":
				b.Owner = subOwnerB
			case "chat":
				b.Chat = subChatB
			}
			channel := func(s team.Subscription) team.Channel {
				return team.Channel{Owner: s.Owner, Chat: s.Chat, Generation: s.Generation, Title: s.Title, Access: s.Access, MirrorLast: s.MirrorLast}
			}
			outcome := subOutcome{entry: team.Entry{ID: teamServerID, Name: "Lab One"}, channels: []team.Channel{channel(a)}}
			if b.Server == teamServerID {
				outcome.channels = append(outcome.channels, channel(b))
			}
			got, changed, truncated := applyChannels([]team.Subscription{a, b}, outcome, strings.Repeat("f", 32), fixedTeamClock()(), false)
			if changed || truncated || len(got) != 2 || got[0].State != "live" || got[1].State != "live" {
				t.Fatalf("unrelated identity changed: %+v, changed %t", got, changed)
			}
		})
	}
}

func TestSubMoreWhenFetchLimitReachedAtCopyEnd(t *testing.T) {
	var pages int
	h, _ := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
		pages++
		after, err := strconv.Atoi(r.URL.Query().Get("after"))
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = io.WriteString(w, subPage(subGen, 9, true, subRecord(after+1, `{"v":1,"t":"user","text":"x"}`)))
	})
	got := decodeSubRead(t, doTeam(h, http.MethodGet, subReadPath()+"?after=8", ""))
	if pages != 8 || got.Last != 8 || got.Next != 8 || len(got.Entries) != 0 || !got.More {
		t.Fatalf("pages %d last %d next %d entries %d more %t", pages, got.Last, got.Next, len(got.Entries), got.More)
	}
}

func TestSubLiveForgottenServerReadsCopyWithoutAnyRequest(t *testing.T) {
	var serverHits, transportHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	client := team.NewClient()
	transport := srv.Client().Transport
	client.Transport = teamRoundTrip(func(r *http.Request) (*http.Response, error) { transportHits.Add(1); return transport.RoundTrip(r) })
	h, dir := testTeamSurface(t, client)
	joinLab(t, dir, teamServerID, srv.URL)
	seedSub(t, dir, baseSub())
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	if err := team.AppendCopy(dir, id, team.MirrorLog, []team.MirrorRecord{{Seq: 1, Key: "k1", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: json.RawMessage(`{"v":1,"t":"user","text":"x"}`)}}); err != nil {
		t.Fatal(err)
	}
	if err := team.Remove(dir, teamServerID); err != nil {
		t.Fatal(err)
	}
	got := decodeSubRead(t, doTeam(h, http.MethodGet, subReadPath(), ""))
	if got.View.State != "live" || got.View.Delivery != "ok" || got.Last != 1 || len(got.Entries) != 1 || got.Entries[0]["seq"] != float64(1) {
		t.Fatalf("view %+v last %d entries %d", got.View, got.Last, len(got.Entries))
	}
	if serverHits.Load() != 0 || transportHits.Load() != 0 {
		t.Fatalf("server hits %d transport hits %d", serverHits.Load(), transportHits.Load())
	}
}

func TestProjectSubRecordUserProvenance(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string
		valid bool
		want  any
	}{
		{"true", `,"prov":true`, true, true},
		{"false", `,"prov":false`, true, false},
		{"absent", ``, true, nil},
		{"null", `,"prov":null`, false, nil},
		{"wrong type", `,"prov":"yes"`, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := team.MirrorRecord{Seq: 1, Body: json.RawMessage(`{"v":1,"t":"user","text":"synthetic"` + tc.field + `}`)}
			got, _, ok := projectSubRecord(r)
			if ok != tc.valid {
				t.Fatalf("valid = %v, want %v", ok, tc.valid)
			}
			if ok {
				if got["prov"] != tc.want {
					t.Fatalf("prov = %v, want %v", got["prov"], tc.want)
				}
			}
		})
	}
}

func TestSubReadProjectsOmittedUser(t *testing.T) {
	h, _ := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, subPage(subGen, 1, false,
			subRecord(1, `{"v":1,"t":"user","time":"2026-11-14T09:30:00.000Z","omitted":"too_large"}`)))
	})
	body := decodeSubRead(t, doTeam(h, http.MethodGet, subReadPath(), ""))
	if len(body.Entries) != 1 {
		t.Fatalf("entries = %+v", body.Entries)
	}
	entry := body.Entries[0]
	if entry["omitted"] != "too_large" {
		t.Errorf("omitted = %v", entry["omitted"])
	}
	if _, present := entry["text"]; present {
		t.Errorf("unexpected text = %v", entry["text"])
	}
}

func TestProjectSubRecordUserOmittedType(t *testing.T) {
	for _, kind := range []string{"user", "assistant"} {
		t.Run(kind, func(t *testing.T) {
			r := team.MirrorRecord{Seq: 1, Body: json.RawMessage(`{"v":1,"t":"` + kind + `","omitted":42}`)}
			if _, _, ok := projectSubRecord(r); ok {
				t.Fatal("non-string omitted accepted")
			}
		})
	}
}

type subRemoveScript struct {
	helloStatus int
	helloID     string
	leaveStatus int
	drop        string
	helloCalls  atomic.Int64
	leaveCalls  atomic.Int64
}

func (s *subRemoveScript) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/hello" {
		s.helloCalls.Add(1)
		if s.drop == "hello" {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		status := s.helloStatus
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		if status == http.StatusOK {
			id := s.helloID
			if id == "" {
				id = teamServerID
			}
			_, _ = io.WriteString(w, helloJSON(id, "Synthetic"))
		}
		return
	}
	if r.URL.Path == "/v1/channels/"+subOwner+"/"+subChat+"/membership" {
		s.leaveCalls.Add(1)
		if s.drop == "leave" {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		if r.Method != http.MethodDelete {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+teamToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		status := s.leaveStatus
		if status == 0 {
			status = http.StatusNoContent
		}
		w.WriteHeader(status)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func seedRemovalCopy(t *testing.T, dir string, id team.CopyID) string {
	t.Helper()
	record := team.MirrorRecord{Seq: 1, Key: "k1", Author: id.Owner, At: "2026-11-14T09:30:00.000Z", Body: json.RawMessage(`{}`)}
	if err := team.AppendCopy(dir, id, team.MirrorLog, []team.MirrorRecord{record}); err != nil {
		t.Fatal("cannot seed synthetic copy")
	}
	return filepath.Join(dir, team.CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "mirror.jsonl")
}

func TestSubRemoveLiveAndServerOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		helloStatus int
		helloID     string
		leaveStatus int
		drop        string
		want        int
		code        string
		leaveCalls  int64
		removed     bool
	}{
		{"success", 0, "", 204, "", 204, "", 1, true},
		{"missing channel", 0, "", 404, "", 204, "", 1, true},
		{"unauthorized", 0, "", 401, "", 204, "", 1, true},
		{"hello offline", 0, "", 0, "hello", 502, "unreachable", 0, false},
		{"hello mismatch", 0, teamOtherID, 0, "", 502, "bad_server", 0, false},
		{"hello protocol", 500, "", 0, "", 502, "bad_server", 0, false},
		{"leave offline", 0, "", 0, "leave", 502, "unreachable", 1, false},
		{"leave protocol", 0, "", 500, "", 502, "bad_server", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := &subRemoveScript{helloStatus: tc.helloStatus, helloID: tc.helloID, leaveStatus: tc.leaveStatus, drop: tc.drop}
			srv := httptest.NewServer(script)
			t.Cleanup(srv.Close)
			h, dir := testTeamSurface(t, clientFor(srv))
			joinLab(t, dir, teamServerID, srv.URL)
			seedSub(t, dir, baseSub())
			id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
			path := seedRemovalCopy(t, dir, id)
			rec := doTeam(h, http.MethodDelete, subReadPath(), "")
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
			if tc.code != "" {
				assertAPIError(t, rec, tc.want, tc.code)
			}
			if script.helloCalls.Load() != 1 {
				t.Error("wrong hello request count")
			}
			if script.leaveCalls.Load() != tc.leaveCalls {
				t.Error("wrong leave request count")
			}
			subs := loadSubFile(t, dir)
			_, err := os.Stat(path)
			if tc.removed {
				if len(subs) != 0 {
					t.Error("subscription remains")
				}
				if !errors.Is(err, os.ErrNotExist) {
					t.Error("copy remains")
				}
			} else {
				if len(subs) != 1 {
					t.Error("subscription was removed")
				}
				if err != nil {
					t.Error("copy was removed")
				}
			}
		})
	}
}

func TestSubRemoveEndedAndUnjoinedDoNotCallServer(t *testing.T) {
	for _, mode := range []string{"ended", "unjoined"} {
		t.Run(mode, func(t *testing.T) {
			script := &subRemoveScript{}
			srv := httptest.NewServer(script)
			t.Cleanup(srv.Close)
			h, dir := testTeamSurface(t, clientFor(srv))
			joinLab(t, dir, teamServerID, srv.URL)
			sub := baseSub()
			if mode == "ended" {
				sub.State = "ended"
			}
			seedSub(t, dir, sub)
			if mode == "unjoined" {
				if err := team.Remove(dir, teamServerID); err != nil {
					t.Fatal("cannot forget synthetic server")
				}
			}
			id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
			path := seedRemovalCopy(t, dir, id)
			if rec := doTeam(h, http.MethodDelete, subReadPath(), ""); rec.Code != http.StatusNoContent {
				t.Errorf("status = %d", rec.Code)
			}
			if script.helloCalls.Load() != 0 {
				t.Error("unexpected hello")
			}
			if script.leaveCalls.Load() != 0 {
				t.Error("unexpected leave")
			}
			if len(loadSubFile(t, dir)) != 0 {
				t.Error("subscription remains")
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Error("copy remains")
			}
		})
	}
}

func TestSubRemoveRejectsUnknownAndMalformedIdentities(t *testing.T) {
	h, dir := testTeamSurface(t, team.NewClient())
	seedSub(t, dir, baseSub())
	parts := []string{teamServerID, subOwner, subChat, subGen}
	for i := range parts {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			bad := append([]string(nil), parts...)
			bad[i] = "bad"
			assertAPIError(t, doTeam(h, http.MethodDelete, "/api/team/subscriptions/"+strings.Join(bad, "/"), ""), 404, "not_found")
		})
	}
	other := []string{teamOtherID, subOwnerB, subChatB, subGenB}
	for i := range parts {
		unknown := append([]string(nil), parts...)
		unknown[i] = other[i]
		assertAPIError(t, doTeam(h, http.MethodDelete, "/api/team/subscriptions/"+strings.Join(unknown, "/"), ""), 404, "not_found")
	}
	if len(loadSubFile(t, dir)) != 1 {
		t.Error("unknown request changed subscriptions")
	}
}

func TestSubRemoveRequiresCSRF(t *testing.T) {
	cmd, data, _, _ := startTeamCommand(t, true, true)
	seedSub(t, data, baseSub())
	core, err := newCoreMux(cmd.application)
	if err != nil {
		t.Fatal("cannot create core mux")
	}
	web, err := newWebBackend(webBackendConfig{Web: webFS, Core: core, RequestPolicy: cmd.application.requestPolicy})
	if err != nil {
		t.Fatal("cannot create web boundary")
	}
	req := httptest.NewRequest(http.MethodDelete, "http://127.0.0.1"+subReadPath(), nil)
	req.Host = "127.0.0.1"
	rec := httptest.NewRecorder()
	web.local.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("without CSRF = %d", rec.Code)
	}
	if len(loadSubFile(t, data)) != 1 {
		t.Error("request without CSRF removed subscription")
	}
}

func TestSubRemoveStoreFailuresKeepSubscription(t *testing.T) {
	for _, mode := range []string{"lookup", "copy", "reload", "save"} {
		t.Run(mode, func(t *testing.T) {
			h, dir := testTeamSurface(t, team.NewClient())
			sub := baseSub()
			sub.State = "ended"
			seedSub(t, dir, sub)
			id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
			path := seedRemovalCopy(t, dir, id)
			previous := removeSubscriptionCopy
			t.Cleanup(func() { removeSubscriptionCopy = previous })
			switch mode {
			case "lookup":
				if err := os.WriteFile(filepath.Join(dir, team.SubsFile), []byte("broken"), 0o600); err != nil {
					t.Fatal("cannot damage synthetic store")
				}
			case "copy":
				removeSubscriptionCopy = func(string, team.CopyID) error { return errors.New("synthetic remove failure") }
			case "reload":
				removeSubscriptionCopy = func(data string, copyID team.CopyID) error {
					if err := previous(data, copyID); err != nil {
						return err
					}
					return os.WriteFile(filepath.Join(data, team.SubsFile), []byte("broken"), 0o600)
				}
			case "save":
				asTeamHandler(t, h).saveSubs = func(string, []team.Subscription) error { return errors.New("synthetic save failure") }
			}
			assertAPIError(t, doTeam(h, http.MethodDelete, subReadPath(), ""), 500, "store_unavailable")
			if mode == "copy" {
				if _, err := os.Stat(path); err != nil {
					t.Error("copy was lost on remove failure")
				}
			}
			if mode == "save" {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Error("copy remains after save failure")
				}
				if len(loadSubFile(t, dir)) != 1 {
					t.Error("entry was lost on save failure")
				}
			}
		})
	}
}

func TestSubRemovePreservesOtherServer(t *testing.T) {
	h, dir := testTeamSurface(t, team.NewClient())
	first := baseSub()
	first.State = "ended"
	second := first
	second.Server = teamOtherID
	second.Chat = subChatB
	seedSub(t, dir, first)
	seedSub(t, dir, second)
	id1 := team.CopyID{Server: first.Server, Owner: first.Owner, Chat: first.Chat, Generation: first.Generation}
	id2 := team.CopyID{Server: second.Server, Owner: second.Owner, Chat: second.Chat, Generation: second.Generation}
	path1, path2 := seedRemovalCopy(t, dir, id1), seedRemovalCopy(t, dir, id2)
	if rec := doTeam(h, http.MethodDelete, subReadPath(), ""); rec.Code != http.StatusNoContent {
		t.Errorf("status = %d", rec.Code)
	}
	if _, err := os.Stat(path1); !errors.Is(err, os.ErrNotExist) {
		t.Error("first copy remains")
	}
	if _, err := os.Stat(path2); err != nil {
		t.Error("other copy was lost")
	}
	subs := loadSubFile(t, dir)
	if len(subs) != 1 {
		t.Fatal("wrong subscription count")
	}
	if subs[0].Server != teamOtherID {
		t.Error("other server subscription was lost")
	}
}

func TestSubReadRacingRemovalDoesNotRestoreCopy(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var mirrorOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hello":
			_, _ = io.WriteString(w, helloJSON(teamServerID, "Synthetic"))
		case "/v1/channels/" + subOwner + "/" + subChat + "/mirror":
			mirrorOnce.Do(func() { close(entered) })
			<-release
			_, _ = io.WriteString(w, subPage(subGen, 1, false, subRecord(1, `{}`)))
		case "/v1/channels/" + subOwner + "/" + subChat + "/membership":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	seedSub(t, dir, baseSub())
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doTeam(h, http.MethodGet, subReadPath(), "") }()
	<-entered
	removed := doTeam(h, http.MethodDelete, subReadPath(), "")
	close(release)
	read := <-done
	if removed.Code != http.StatusNoContent {
		t.Errorf("remove status = %d", removed.Code)
	}
	assertAPIError(t, read, http.StatusNotFound, "not_found")
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	path := filepath.Join(dir, team.CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "mirror.jsonl")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("read restored the removed copy")
	}
	if len(loadSubFile(t, dir)) != 0 {
		t.Error("subscription was restored")
	}
}

func TestSubReadRechecksDamagedSubscriptionStore(t *testing.T) {
	var dir string
	h, data := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
		if err := os.WriteFile(filepath.Join(dir, team.SubsFile), []byte("broken"), 0o600); err != nil {
			t.Error("cannot damage synthetic store")
		}
		_, _ = io.WriteString(w, subPage(subGen, 1, false, subRecord(1, `{}`)))
	})
	dir = data
	assertAPIError(t, doTeam(h, http.MethodGet, subReadPath(), ""), 500, "store_unavailable")
}

func TestSubRemoveNetworkCallsHoldNoHandlerLock(t *testing.T) {
	var handler *teamHandler
	var held, earlyAuth atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handler.mu.TryLock() {
			handler.mu.Unlock()
		} else {
			held.Store(true)
		}
		if r.URL.Path == "/v1/hello" {
			if r.Header.Get("Authorization") != "" {
				earlyAuth.Store(true)
			}
			_, _ = io.WriteString(w, helloJSON(teamServerID, "Synthetic"))
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+teamToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	h, dir := testTeamSurface(t, clientFor(srv))
	handler = asTeamHandler(t, h)
	joinLab(t, dir, teamServerID, srv.URL)
	seedSub(t, dir, baseSub())
	if rec := doTeam(h, http.MethodDelete, subReadPath(), ""); rec.Code != http.StatusNoContent {
		t.Errorf("status = %d", rec.Code)
	}
	if held.Load() {
		t.Error("network request held the handler lock")
	}
	if earlyAuth.Load() {
		t.Error("hello carried authorization")
	}
}

func TestSubRemoveGateOffDoesNothing(t *testing.T) {
	cmd, data, _, _ := startTeamCommand(t, false, true)
	core, err := newCoreMux(cmd.application)
	if err != nil {
		t.Fatal("cannot create core mux")
	}
	if got, want := recordedText(core, http.MethodDelete, subReadPath()), recordedText(core, http.MethodDelete, "/api/not-a-route"); got != want {
		t.Error("gate-off route did not match the unknown route")
	}
	assertNoTeamFile(t, data)
	assertNoSubsFile(t, data)
	if _, err := os.Stat(filepath.Join(data, team.CopiesDir)); !errors.Is(err, os.ErrNotExist) {
		t.Error("gate-off route touched copies")
	}
}

func TestSubRemoveEntryVanishedDuringCopyRemoval(t *testing.T) {
	h, dir := testTeamSurface(t, team.NewClient())
	sub := baseSub()
	sub.State = "ended"
	seedSub(t, dir, sub)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	seedRemovalCopy(t, dir, id)
	previous := removeSubscriptionCopy
	t.Cleanup(func() { removeSubscriptionCopy = previous })
	calls := 0
	removeSubscriptionCopy = func(data string, copyID team.CopyID) error {
		calls++
		if err := previous(data, copyID); err != nil {
			return err
		}
		return team.SaveSubs(data, nil)
	}
	assertAPIError(t, doTeam(h, http.MethodDelete, subReadPath(), ""), 404, "not_found")
	if calls != 1 {
		t.Error("copy removal was not attempted")
	}
	if len(loadSubFile(t, dir)) != 0 {
		t.Error("subscription was restored")
	}
}

func TestSubListRemovalDuringRefresh(t *testing.T) {
	for _, tc := range []struct {
		name     string
		expire   bool
		advance  int64
		removeAt int64
	}{
		{name: "stale refresh"},
		{name: "removal follows ask", removeAt: 1},
		{name: "removal still held at 60 seconds", advance: 60},
		{name: "removal expires", expire: true, advance: 61},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var seats atomic.Int32
			var empty atomic.Bool
			var seconds atomic.Int64
			start := fixedTeamClock()()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/hello":
					_, _ = io.WriteString(w, helloJSON(teamServerID, "Synthetic"))
				case "/v1/channels":
					if empty.Load() {
						_, _ = io.WriteString(w, channelsOf())
					} else {
						_, _ = io.WriteString(w, channelsOf(guestChannel(subOwner, subChat, "Synthetic", subGen, "read", 2), guestChannel(subOwnerB, subChatB, "Synthetic", subGenB, "read", 1)))
					}
				case "/v1/seats":
					if seats.Add(1) == 1 {
						close(entered)
						<-release
					}
					_, _ = io.WriteString(w, seatsOf(subOwner, "Synthetic"))
				case "/v1/channels/" + subOwner + "/" + subChat + "/membership":
					w.WriteHeader(http.StatusNoContent)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			h, dir := testTeamSurface(t, clientFor(srv))
			asTeamHandler(t, h).now = func() time.Time { return start.Add(time.Duration(seconds.Load()) * time.Second) }
			joinLab(t, dir, teamServerID, srv.URL)
			seedSub(t, dir, baseSub())
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- doTeam(h, http.MethodGet, "/api/team/subscriptions", "") }()
			<-entered
			seconds.Store(tc.removeAt)
			removed := doTeam(h, http.MethodDelete, subReadPath(), "")
			if tc.advance != 0 {
				seconds.Store(tc.advance)
				empty.Store(true)
				refresh := doTeam(h, http.MethodGet, "/api/team/subscriptions", "")
				if refresh.Code != http.StatusOK {
					t.Errorf("refresh status %d", refresh.Code)
				}
			}
			close(release)
			listed := <-done
			if removed.Code != http.StatusNoContent {
				t.Errorf("remove status %d", removed.Code)
			}
			if listed.Code != http.StatusOK {
				t.Errorf("list status %d", listed.Code)
			}
			subs := loadSubFile(t, dir)
			foundA, foundB := false, false
			for _, sub := range subs {
				if sub.Chat == subChat {
					foundA = true
				}
				if sub.Chat == subChatB {
					foundB = true
				}
			}
			if foundA != tc.expire {
				t.Errorf("removed key present = %v, want %v", foundA, tc.expire)
			}
			if !foundB {
				t.Error("other key was not added")
			}
		})
	}
}

func TestSubListRefreshAfterRemovalAddsAgain(t *testing.T) {
	script := &subScript{channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic", subGen, "read", 2))}
	h, dir, _ := subSurface(t, script)
	clock := holdClock(t, h)
	sub := baseSub()
	sub.State = "ended"
	seedSub(t, dir, sub)
	removed := doTeam(h, http.MethodDelete, subReadPath(), "")
	*clock = clock.Add(time.Second)
	listed := doTeam(h, http.MethodGet, "/api/team/subscriptions", "")
	if removed.Code != http.StatusNoContent {
		t.Errorf("remove status %d", removed.Code)
	}
	if listed.Code != http.StatusOK {
		t.Errorf("list status %d", listed.Code)
	}
	subs := loadSubFile(t, dir)
	if len(subs) != 1 {
		t.Fatalf("got %d subscriptions", len(subs))
	}
	if subs[0].SeenMirror != 0 {
		t.Error("re-added subscription was not new")
	}
	if again := doTeam(h, http.MethodDelete, subReadPath(), ""); again.Code != http.StatusNoContent {
		t.Errorf("second remove status %d", again.Code)
	}
}

func TestSubListOwnerNameAndComments(t *testing.T) {
	script := &subScript{}
	h, dir, _ := subSurface(t, script)
	clock := holdClock(t, h)
	inst := installationOf(t, dir)
	entries, err := team.Load(dir)
	if err != nil || len(entries) != 1 {
		t.Fatal("server state unavailable")
	}
	own := channelJSON(inst, subChat, "Synthetic", subGen)
	own = strings.Replace(own, `"comments_last":0`, `"comments_last":3`, 1)
	script.channelsBody = channelsOf(own)
	first, _ := readSubList(t, h)
	got := mustFindSub(t, first, inst, subChat, subGen)
	if got.Access != "owner" || got.OwnerName != entries[0].SeatName || got.CommentsLast != 3 || got.SeenComments != 0 || !got.Unread {
		t.Error("owner subscription did not reflect server and seat")
	}
	if countSubPath(script.snapshot(), "/v1/seats") != 0 {
		t.Error("owner alone caused a roster call")
	}
	*clock = clock.Add(10 * time.Second)
	script.mu.Lock()
	script.channelsBody = channelsOf()
	script.mu.Unlock()
	ended, _ := readSubList(t, h)
	if got := mustFindSub(t, ended, inst, subChat, subGen); got.State != "ended" || got.Unread {
		t.Error("owner subscription was not ended")
	}
	*clock = clock.Add(10 * time.Second)
	script.mu.Lock()
	script.channelsBody = channelsOf(own)
	script.mu.Unlock()
	revived, _ := readSubList(t, h)
	if got := mustFindSub(t, revived, inst, subChat, subGen); got.State != "live" || !got.Unread {
		t.Error("owner subscription was not revived")
	}
}

func TestSubListUnreadIncludesCommentsByAccess(t *testing.T) {
	for _, tc := range []struct {
		name, access, state                        string
		mirror, seenMirror, comments, seenComments uint64
		want                                       bool
	}{
		{"guest mirror", "read", "live", 1, 0, 0, 0, true},
		{"guest comments", "write", "live", 0, 0, 1, 0, true},
		{"guest all seen", "read", "live", 1, 1, 1, 1, false},
		{"guest ended", "write", "ended", 1, 0, 1, 0, false},
		{"owner mirror", "owner", "live", 1, 0, 0, 0, false},
		{"owner comments", "owner", "live", 1, 0, 1, 0, true},
		{"owner ended", "owner", "ended", 1, 0, 1, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := testTeamSurface(t, team.NewClient())
			s := baseSub()
			s.Access, s.State = tc.access, tc.state
			s.MirrorLast, s.SeenMirror, s.CommentsLast, s.SeenComments = tc.mirror, tc.seenMirror, tc.comments, tc.seenComments
			view := asTeamHandler(t, h).subscriptionView([]team.Subscription{s}, nil, false).Subscriptions[0]
			if view.Unread != tc.want {
				t.Errorf("unread = %v, want %v", view.Unread, tc.want)
			}
			encoded, _ := json.Marshal(view)
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(encoded, &fields)
			if _, ok := fields["comments_last"]; !ok {
				t.Error("comments_last missing")
			}
			if _, ok := fields["seen_comments"]; !ok {
				t.Error("seen_comments missing")
			}
		})
	}
}

func TestSubListUpdatesOwnerNameAndCommentCount(t *testing.T) {
	s := baseSub()
	s.Access, s.OwnerName = "owner", "Old"
	outcome := subOutcome{entry: team.Entry{ID: teamServerID, SeatName: "New"}, channels: []team.Channel{{Owner: s.Owner, Chat: s.Chat, Generation: s.Generation, Title: s.Title, Access: "owner", MirrorLast: s.MirrorLast, CommentsLast: 4}}}
	got, changed, _ := applyChannels([]team.Subscription{s}, outcome, s.Owner, fixedTeamClock()(), false)
	if !changed || len(got) != 1 || got[0].OwnerName != "New" || got[0].CommentsLast != 4 || got[0].State != "live" {
		t.Error("owner update was lost")
	}
}

func TestSubListRosterDoesNotReplaceOwnSeatName(t *testing.T) {
	s := baseSub()
	s.Access, s.OwnerName = "owner", "Own"
	changed := applySeatNames([]team.Subscription{s}, teamServerID, []team.Seat{{ID: s.Owner, Name: "Other"}})
	if changed {
		t.Error("roster changed owner name")
	}
}

type commentsReadBody struct {
	Entries []struct {
		Seq    uint64 `json:"seq"`
		Author string `json:"author"`
		Mine   bool   `json:"mine"`
		Text   string `json:"text"`
		Anchor uint64 `json:"anchor"`
	} `json:"entries"`
	Names map[string]string `json:"names"`
	Last  uint64            `json:"last"`
	Next  uint64            `json:"next"`
	More  bool              `json:"more"`
}

func decodeCommentsRead(t *testing.T, rec *httptest.ResponseRecorder) commentsReadBody {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("comments status = %d", rec.Code)
	}
	var body commentsReadBody
	if json.Unmarshal(rec.Body.Bytes(), &body) != nil {
		t.Fatal("invalid comments response")
	}
	return body
}

func TestSubCommentsReadProjectionAndNames(t *testing.T) {
	var rosterCalls atomic.Int32
	var ownID string
	bodies := []string{
		`{"v":1,"t":"comment","text":"synthetic","anchor":4}`,
		`{"v":1,"t":"comment","text":"","anchor":1}`,
		`{"v":1,"t":"comment","anchor":1}`,
		`{"v":1,"t":"comment","text":1,"anchor":1}`,
		`{"v":1,"t":"comment","text":"synthetic"}`,
		`{"v":1,"t":"comment","text":"synthetic","anchor":-1}`,
		`{"v":1,"t":"other","text":"synthetic","anchor":1}`,
		`{"v":2,"t":"comment","text":"synthetic","anchor":1}`,
		`{"t":"comment","text":"synthetic","anchor":1}`,
		`{"v":1,"text":"synthetic","anchor":1}`,
		`{"v":1,"t":"comment","text":null,"anchor":1}`,
		`{"v":1,"t":"comment","text":"synthetic","anchor":null}`,
		`{"v":1,"t":"comment","text":"synthetic","anchor":2}`,
	}
	h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/seats":
			rosterCalls.Add(1)
			_, _ = io.WriteString(w, seatsOf(subOwner, "Ada"))
		case strings.HasSuffix(r.URL.Path, "/comments"):
			if r.URL.Query().Get("after") != "0" {
				_, _ = io.WriteString(w, subPage(subGen, len(bodies), false))
				return
			}
			entries := make([]string, 0, len(bodies))
			for i, body := range bodies {
				record := subRecord(i+1, body)
				if i == len(bodies)-1 {
					record = strings.Replace(record, subOwner, ownID, 1)
				}
				entries = append(entries, record)
			}
			_, _ = io.WriteString(w, subPage(subGen, len(bodies), false, entries...))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ownID = installationOf(t, dir)
	servers, err := team.Load(dir)
	if err != nil {
		t.Fatal("server state unavailable")
	}
	first := decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""))
	if first.Last != uint64(len(bodies)) || first.Next != first.Last || first.More || len(first.Entries) != 2 {
		t.Fatal("projection or cursor incorrect")
	}
	if first.Entries[0].Mine || !first.Entries[1].Mine || first.Entries[0].Anchor != 4 {
		t.Error("projected fields incorrect")
	}
	if first.Names[subOwner] != "Ada" || first.Names[ownID] != servers[0].SeatName {
		t.Error("author names incorrect")
	}
	if rosterCalls.Load() != 1 {
		t.Error("unexpected roster call count")
	}
	second := decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""))
	if len(second.Entries) != 2 || rosterCalls.Load() != 1 {
		t.Error("cached names or comments were not reused")
	}
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	_, last, err := team.ReadCopy(dir, id, team.CommentsLog, 0, 256, 4<<20)
	if err != nil || last != uint64(len(bodies)) {
		t.Error("comments copy did not retain skipped entries")
	}
}

func TestSubCommentsReadOutcomesAndValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		page   string
		state  string
	}{
		{"generation", 200, subPage(subGenB, 0, false), "ended"},
		{"missing", 404, "", "ended"},
		{"unauthorized", 401, "", "ended"},
		{"bad page", 200, "bad", "live"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.page)
			})
			got := decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""))
			if got.Last != 0 || len(got.Entries) != 0 {
				t.Error("unexpected comments")
			}
			if subs := loadSubFile(t, dir); subs[0].State != tc.state {
				t.Error("wrong subscription state")
			}
		})
	}
	h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, subPage(subGen, 0, false)) })
	assertAPIError(t, doTeam(h, http.MethodGet, subReadPath()+"/comments?after=-1", ""), 400, "bad_request")
	assertAPIError(t, doTeam(h, http.MethodGet, strings.Replace(subReadPath(), subOwner, subOwnerB, 1)+"/comments", ""), 404, "not_found")
	path := filepath.Join(dir, team.CopiesDir, teamServerID, subOwner, subChat, subGen, "comments.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal("cannot create copy")
	}
	if err := os.WriteFile(path, []byte("broken\n"), 0o600); err != nil {
		t.Fatal("cannot damage copy")
	}
	assertAPIError(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""), 500, "copy_damaged")
}

func TestSubCommentsReadPageAndBudgetLimits(t *testing.T) {
	t.Run("pages", func(t *testing.T) {
		calls := 0
		h, _ := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/seats" {
				_, _ = io.WriteString(w, seatsOf(subOwner, "Ada"))
				return
			}
			calls++
			after, _ := strconv.Atoi(r.URL.Query().Get("after"))
			_, _ = io.WriteString(w, subPage(subGen, 20, true, subRecord(after+1, `{"v":1,"t":"comment","text":"synthetic","anchor":0}`)))
		})
		got := decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""))
		if calls != 8 || got.Last != 8 || got.Next != 8 || !got.More {
			t.Error("page limit not enforced")
		}
	})
	t.Run("budget", func(t *testing.T) {
		h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, subPage(subGen, 1, false, subRecord(1, `{"v":1,"t":"comment","text":"synthetic","anchor":0}`)))
		})
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"storage_global_limit_bytes":1,"storage_min_free_bytes":0}`), 0o600); err != nil {
			t.Fatal("cannot set budget")
		}
		assertAPIError(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""), 507, "storage_full")
	})
}

func TestSubCommentsKnownAuthorRefresh(t *testing.T) {
	for _, access := range []string{"owner", "read"} {
		t.Run(access, func(t *testing.T) {
			roster := 0
			name := "Bob"
			h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/seats" {
					roster++
					_, _ = io.WriteString(w, seatsOf(subOwner, name))
					return
				}
				if r.URL.Query().Get("after") == "0" {
					_, _ = io.WriteString(w, subPage(subGen, 1, false, subRecord(1, `{"v":1,"t":"comment","text":"synthetic","anchor":0}`)))
					return
				}
				_, _ = io.WriteString(w, subPage(subGen, 1, false))
			})
			subs := loadSubFile(t, dir)
			subs[0].Access = access
			if err := team.SaveSubs(dir, subs); err != nil {
				t.Fatal("cannot save subscription")
			}
			clock := holdClock(t, h)
			first := decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""))
			if first.Names[subOwner] != "Bob" || roster != 1 {
				t.Fatal("initial roster not cached")
			}
			name = "Bobby"
			*clock = clock.Add(60 * time.Second)
			second := decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""))
			if second.Names[subOwner] != "Bob" || roster != 1 {
				t.Fatal("roster refreshed too early")
			}
			*clock = clock.Add(time.Second)
			third := decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""))
			if third.Names[subOwner] != "Bobby" || roster != 2 {
				t.Fatal("known author was not refreshed")
			}
		})
	}
}

func TestSubCommentsIncrementalNamesRefresh(t *testing.T) {
	roster := 0
	name := "Bob"
	h, _ := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/seats" {
			roster++
			_, _ = io.WriteString(w, seatsOf(subOwner, name))
			return
		}
		if r.URL.Query().Get("after") == "0" {
			_, _ = io.WriteString(w, subPage(subGen, 1, false, subRecord(1, `{"v":1,"t":"comment","text":"synthetic","anchor":0}`)))
			return
		}
		_, _ = io.WriteString(w, subPage(subGen, 1, false))
	})
	clock := holdClock(t, h)
	first := decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""))
	if first.Names[subOwner] != "Bob" || roster != 1 {
		t.Fatal("initial name missing")
	}
	name = "Bobby"
	*clock = clock.Add(61 * time.Second)
	next := decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments?after=1", ""))
	if len(next.Entries) != 0 || next.Names[subOwner] != "Bobby" || roster != 2 {
		t.Fatal("incremental read did not return refreshed name")
	}
}

func TestSubCommentsEmptyReadReturnsRosterAndOwnName(t *testing.T) {
	roster := 0
	var inst string
	h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/seats" {
			roster++
			_, _ = io.WriteString(w, seatsOf(subOwner, "Bob", subOwnerB, "Cy", inst, "Old"))
			return
		}
		_, _ = io.WriteString(w, subPage(subGen, 0, false))
	})
	inst = installationOf(t, dir)
	servers, err := team.Load(dir)
	if err != nil {
		t.Fatal("server state unavailable")
	}
	got := decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""))
	if len(got.Entries) != 0 || got.Names[subOwner] != "Bob" || got.Names[subOwnerB] != "Cy" || got.Names[inst] != servers[0].SeatName || roster != 1 {
		t.Fatal("empty read omitted roster or own name")
	}
	_ = decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""))
	if roster != 1 {
		t.Fatal("empty read made an extra roster call")
	}
	if err := team.Remove(dir, teamServerID); err != nil {
		t.Fatal("cannot forget server")
	}
	forgotten := decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""))
	if forgotten.Names[subOwner] != "Bob" || forgotten.Names[subOwnerB] != "Cy" || forgotten.Names[inst] != "Old" || roster != 1 {
		t.Fatal("unjoined read did not return cached names")
	}
}

func TestSubCommentsRosterFailureAndRefreshWindow(t *testing.T) {
	var roster int
	h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/seats" {
			roster++
			if roster == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = io.WriteString(w, seatsOf(subOwner, "Ada"))
			return
		}
		if r.URL.Query().Get("after") == "0" {
			_, _ = io.WriteString(w, subPage(subGen, 1, false, subRecord(1, `{"v":1,"t":"comment","text":"synthetic","anchor":0}`)))
			return
		}
		_, _ = io.WriteString(w, subPage(subGen, 1, false))
	})
	clock := holdClock(t, h)
	first := decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""))
	if _, ok := first.Names[subOwner]; ok || roster != 1 {
		t.Fatal("failed roster was used")
	}
	*clock = clock.Add(60 * time.Second)
	second := decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""))
	if _, ok := second.Names[subOwner]; ok || roster != 1 {
		t.Fatal("roster retried too early")
	}
	*clock = clock.Add(time.Second)
	third := decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""))
	if third.Names[subOwner] != "Ada" || roster != 2 {
		t.Fatal("roster did not refresh")
	}
	if err := team.Remove(dir, teamServerID); err != nil {
		t.Fatal("cannot forget server")
	}
	*clock = clock.Add(61 * time.Second)
	_ = decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""))
	if roster != 2 {
		t.Error("unjoined server was contacted")
	}
}

func TestSubCommentsReadBoundsAndMissingIdentity(t *testing.T) {
	t.Run("identity", func(t *testing.T) {
		h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, subPage(subGen, 0, false)) })
		if err := os.Remove(filepath.Join(dir, "installation.json")); err != nil {
			t.Fatal("cannot remove synthetic identity")
		}
		assertAPIError(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""), 500, "identity_unavailable")
	})
	t.Run("entry limit and after", func(t *testing.T) {
		h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/seats" {
				t.Error("ended subscription fetched comments")
				return
			}
			_, _ = io.WriteString(w, seatsOf(subOwner, "Ada"))
		})
		subs := loadSubFile(t, dir)
		subs[0].State = "ended"
		if err := team.SaveSubs(dir, subs); err != nil {
			t.Fatal("cannot save state")
		}
		id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
		entries := make([]team.MirrorRecord, 257)
		for i := range entries {
			entries[i] = team.MirrorRecord{Seq: uint64(i + 1), Key: fmt.Sprintf("k%d", i+1), Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: json.RawMessage(`{"v":1,"t":"comment","text":"synthetic","anchor":0}`)}
		}
		if err := team.AppendCopy(dir, id, team.CommentsLog, entries); err != nil {
			t.Fatal("cannot seed comments")
		}
		first := decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""))
		if len(first.Entries) != 256 || first.Next != 256 || first.Last != 257 || !first.More {
			t.Error("entry limit incorrect")
		}
		past := decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments?after=999", ""))
		if len(past.Entries) != 0 || past.Next != 257 || past.More {
			t.Error("after was not capped")
		}
	})
	t.Run("text limit", func(t *testing.T) {
		h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/seats" {
				t.Error("ended subscription fetched comments")
				return
			}
			_, _ = io.WriteString(w, seatsOf(subOwner, "Ada"))
		})
		subs := loadSubFile(t, dir)
		subs[0].State = "ended"
		if err := team.SaveSubs(dir, subs); err != nil {
			t.Fatal("cannot save state")
		}
		id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
		body, _ := json.Marshal(map[string]any{"v": 1, "t": "comment", "text": strings.Repeat("x", 3<<20), "anchor": 0})
		records := []team.MirrorRecord{{Seq: 1, Key: "k1", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: body}, {Seq: 2, Key: "k2", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: body}}
		if err := team.AppendCopy(dir, id, team.CommentsLog, records); err != nil {
			t.Fatal("cannot seed comments")
		}
		got := decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""))
		if len(got.Entries) != 1 || got.Next != 1 || got.Last != 2 || !got.More {
			t.Error("text limit incorrect")
		}
	})
}

func TestSubCommentsRosterNamesArePerServer(t *testing.T) {
	server := func(id, name string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/hello":
				_, _ = io.WriteString(w, helloJSON(id, "Synthetic"))
			case "/v1/seats":
				_, _ = io.WriteString(w, seatsOf(subOwner, name))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	a, b := server(teamServerID, "Ada"), server(teamOtherID, "Bea")
	h, dir := testTeamSurface(t, clientFor(a))
	joinLab(t, dir, teamServerID, a.URL)
	joinLab(t, dir, teamOtherID, b.URL)
	for _, serverID := range []string{teamServerID, teamOtherID} {
		s := baseSub()
		s.Server = serverID
		s.State = "ended"
		seedSub(t, dir, s)
		id := team.CopyID{Server: serverID, Owner: subOwner, Chat: subChat, Generation: subGen}
		record := team.MirrorRecord{Seq: 1, Key: "k1", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: json.RawMessage(`{"v":1,"t":"comment","text":"synthetic","anchor":0}`)}
		if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{record}); err != nil {
			t.Fatal("cannot seed comments")
		}
	}
	first := decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""))
	second := decodeCommentsRead(t, doTeam(h, http.MethodGet, strings.Replace(subReadPath(), teamServerID, teamOtherID, 1)+"/comments", ""))
	if first.Names[subOwner] != "Ada" || second.Names[subOwner] != "Bea" {
		t.Error("server names were mixed")
	}
}

func TestSubCommentsBudgetKeepsSavedEntries(t *testing.T) {
	h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/seats" {
			_, _ = io.WriteString(w, seatsOf(subOwner, "Ada"))
			return
		}
		_, _ = io.WriteString(w, subPage(subGen, 2, false, subRecord(2, `{"v":1,"t":"comment","text":"synthetic","anchor":0}`)))
	})
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	record := team.MirrorRecord{Seq: 1, Key: "k1", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: json.RawMessage(`{"v":1,"t":"comment","text":"synthetic","anchor":0}`)}
	if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{record}); err != nil {
		t.Fatal("cannot seed comments")
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"storage_global_limit_bytes":1,"storage_min_free_bytes":0}`), 0o600); err != nil {
		t.Fatal("cannot set budget")
	}
	got := decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""))
	if got.Last != 1 || len(got.Entries) != 1 {
		t.Error("saved comments were not returned")
	}
}

func TestSubCommentPostServerOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name            string
		status          int
		wantStatus      int
		wantCode, state string
		duplicate       bool
	}{
		{"stored", 200, 200, "", "live", false}, {"duplicate", 200, 200, "", "live", true},
		{"stale", 409, 409, "not_active", "ended", false}, {"missing", 404, 409, "not_active", "ended", false},
		{"forbidden", 403, 403, "read_only", "live", false}, {"unauthorized", 401, 502, "refused", "ended", false},
		{"quota", 507, 507, "server_full", "live", false}, {"too large", 413, 413, "too_large", "live", false},
		{"other", 500, 502, "bad_server", "live", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const marker = "synthetic_comment_marker"
			var logs bytes.Buffer
			old := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(old)
			calls, hello := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/hello" {
					hello++
					_, _ = io.WriteString(w, helloJSON(teamServerID, "Synthetic"))
					return
				}
				calls++
				if r.URL.Path != "/v1/channels/"+subOwner+"/"+subChat+"/comments" || r.Method != http.MethodPost {
					t.Error("wrong append request")
				}
				body, _ := io.ReadAll(r.Body)
				want := `{"generation":"` + subGen + `","entries":[{"key":"c/0123456789abcdef","body":{"v":1,"t":"comment","text":" ` + marker + ` ","anchor":0}}]}`
				if string(body) != want {
					t.Error("append body changed")
				}
				w.WriteHeader(tc.status)
				if tc.status == 200 {
					_, _ = fmt.Fprintf(w, `{"generation":"%s","results":[{"key":"c/0123456789abcdef","seq":3,"duplicate":%t}],"last":5}`, subGen, tc.duplicate)
				}
			}))
			defer srv.Close()
			h, dir := testTeamSurface(t, clientFor(srv))
			joinLab(t, dir, teamServerID, srv.URL)
			sub := baseSub()
			sub.Access = "write"
			seedSub(t, dir, sub)
			other := sub
			other.Chat = subChatB
			seedSub(t, dir, other)
			rec := doTeam(h, http.MethodPost, subReadPath()+"/comments", `{"text":" `+marker+` ","anchor":0,"key":"0123456789abcdef"}`)
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d", rec.Code)
			}
			if tc.wantCode != "" {
				assertAPIError(t, rec, tc.wantStatus, tc.wantCode)
			} else {
				var got struct {
					Seq uint64 `json:"seq"`
				}
				if json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.Seq != 3 {
					t.Error("wrong sequence")
				}
			}
			if calls != 1 || hello != 1 {
				t.Error("wrong request count")
			}
			if subs := loadSubFile(t, dir); subs[0].State != tc.state {
				t.Error("wrong final state")
			}
			subs := loadSubFile(t, dir)
			otherState := "live"
			if tc.status == 401 {
				otherState = "ended"
			}
			if subs[1].State != otherState {
				t.Error("other subscription state incorrect")
			}
			if bytes.Contains(rec.Body.Bytes(), []byte(marker)) || bytes.Contains(logs.Bytes(), []byte(marker)) {
				t.Error("message leaked")
			}
			if _, err := os.Stat(filepath.Join(dir, team.CopiesDir)); !errors.Is(err, os.ErrNotExist) {
				t.Error("post wrote a copy")
			}
		})
	}
}

func TestSubCommentPostValidationAndInactive(t *testing.T) {
	h, dir := readSurface(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected server request") })
	subs := loadSubFile(t, dir)
	subs[0].Access = "write"
	if err := team.SaveSubs(dir, subs); err != nil {
		t.Fatal("cannot save state")
	}
	path := subReadPath() + "/comments"
	for _, body := range []string{
		`{}`, `{"text":"","anchor":0,"key":"0123456789abcdef"}`, `{"text":" \n ","anchor":0,"key":"0123456789abcdef"}`,
		`{"text":"x","key":"0123456789abcdef"}`, `{"text":"x","anchor":0}`,
		`{"text":"x","anchor":0,"key":"0123456789abcdeF"}`, `{"text":"x","anchor":0,"key":"0123"}`,
		`{"text":"x","anchor":-1,"key":"0123456789abcdef"}`, `{"text":"x","anchor":1,"key":"0123456789abcdef"}`,
		`{"text":"x","anchor":0,"key":"0123456789abcdef","extra":1}`, `{"text":"x","anchor":0,"key":"0123456789abcdef"} {}`,
		`{"text":"` + strings.Repeat("x", 16385) + `","anchor":0,"key":"0123456789abcdef"}`,
		`{"text":"` + string([]byte{0xff}) + `","anchor":0,"key":"0123456789abcdef"}`,
		strings.Repeat(" ", 65537),
	} {
		assertAPIError(t, doTeam(h, http.MethodPost, path, body), 400, "bad_request")
	}
	good := `{"text":"synthetic","anchor":0,"key":"0123456789abcdef"}`
	assertAPIError(t, doTeam(h, http.MethodPost, strings.Replace(subReadPath(), subOwner, subOwnerB, 1)+"/comments", good), 404, "not_found")
	subs[0].Access = "read"
	if err := team.SaveSubs(dir, subs); err != nil {
		t.Fatal("cannot save state")
	}
	assertAPIError(t, doTeam(h, http.MethodPost, path, good), 403, "read_only")
	subs[0].Access = "write"
	subs[0].State = "ended"
	if err := team.SaveSubs(dir, subs); err != nil {
		t.Fatal("cannot save state")
	}
	assertAPIError(t, doTeam(h, http.MethodPost, path, good), 409, "not_active")
	subs[0].State = "live"
	if err := team.SaveSubs(dir, subs); err != nil {
		t.Fatal("cannot save state")
	}
	if err := team.Remove(dir, teamServerID); err != nil {
		t.Fatal("cannot forget server")
	}
	assertAPIError(t, doTeam(h, http.MethodPost, path, good), 409, "not_active")
}

func TestSubCommentPostRequiresCSRF(t *testing.T) {
	cmd, data, _, _ := startTeamCommand(t, true, true)
	s := baseSub()
	s.Access = "write"
	seedSub(t, data, s)
	core, err := newCoreMux(cmd.application)
	if err != nil {
		t.Fatal("cannot create core mux")
	}
	web, err := newWebBackend(webBackendConfig{Web: webFS, Core: core, RequestPolicy: cmd.application.requestPolicy})
	if err != nil {
		t.Fatal("cannot create web boundary")
	}
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1"+subReadPath()+"/comments", strings.NewReader(`{"text":"synthetic","anchor":0,"key":"0123456789abcdef"}`))
	req.Host = "127.0.0.1"
	rec := httptest.NewRecorder()
	web.local.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("without CSRF = %d", rec.Code)
	}
}

func TestSubCommentPostHelloAndSaveFailures(t *testing.T) {
	for _, tc := range []struct {
		name       string
		helloID    string
		drop       bool
		status     int
		wantStatus int
		code       string
	}{
		{"hello mismatch", teamOtherID, false, 200, 502, "bad_server"},
		{"hello offline", "", true, 200, 502, "unreachable"},
		{"append offline", teamServerID, false, -1, 502, "unreachable"},
		{"state save", teamServerID, false, 409, 500, "store_unavailable"},
		{"unauthorized save", teamServerID, false, 401, 500, "store_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/hello" {
					if tc.drop {
						c, _, _ := w.(http.Hijacker).Hijack()
						_ = c.Close()
						return
					}
					_, _ = io.WriteString(w, helloJSON(tc.helloID, "Synthetic"))
					return
				}
				calls++
				if tc.status == -1 {
					c, _, _ := w.(http.Hijacker).Hijack()
					_ = c.Close()
					return
				}
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			h, dir := testTeamSurface(t, clientFor(srv))
			joinLab(t, dir, teamServerID, srv.URL)
			s := baseSub()
			s.Access = "write"
			seedSub(t, dir, s)
			if strings.HasSuffix(tc.name, "save") {
				asTeamHandler(t, h).saveSubs = func(string, []team.Subscription) error { return errors.New("synthetic failure") }
			}
			rec := doTeam(h, http.MethodPost, subReadPath()+"/comments", `{"text":"synthetic","anchor":0,"key":"0123456789abcdef"}`)
			assertAPIError(t, rec, tc.wantStatus, tc.code)
			if tc.helloID != teamServerID && calls != 0 {
				t.Error("append sent before confirmed hello")
			}
		})
	}
}

func TestSubCommentPostAnchorAndOwner(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			_, _ = io.WriteString(w, helloJSON(teamServerID, "Synthetic"))
			return
		}
		calls++
		_, _ = io.WriteString(w, `{"generation":"`+subGen+`","results":[{"key":"c/0123456789abcdef","seq":1,"duplicate":false}],"last":1}`)
	}))
	defer srv.Close()
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	s := baseSub()
	s.Access = "owner"
	seedSub(t, dir, s)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	record := team.MirrorRecord{Seq: 1, Key: "k1", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: json.RawMessage(`{}`)}
	if err := team.AppendCopy(dir, id, team.MirrorLog, []team.MirrorRecord{record}); err != nil {
		t.Fatal("cannot seed mirror")
	}
	good := `{"text":"` + strings.Repeat("x", 16384) + `","anchor":1,"key":"0123456789abcdef"}`
	if rec := doTeam(h, http.MethodPost, subReadPath()+"/comments", good); rec.Code != 200 {
		t.Errorf("owner post status = %d", rec.Code)
	}
	if calls != 1 {
		t.Error("owner post not sent")
	}
}

func TestSubCommentPostLocalFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		damage func(*testing.T, string)
		code   string
	}{
		{"state", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, team.SubsFile), []byte("bad"), 0o600); err != nil {
				t.Fatal("cannot damage state")
			}
		}, "store_unavailable"},
		{"copy", func(t *testing.T, dir string) {
			path := filepath.Join(dir, team.CopiesDir, teamServerID, subOwner, subChat, subGen, "mirror.jsonl")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal("cannot create copy")
			}
			if err := os.WriteFile(path, []byte("bad\n"), 0o600); err != nil {
				t.Fatal("cannot damage copy")
			}
		}, "copy_damaged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, dir := readSurface(t, func(http.ResponseWriter, *http.Request) { t.Error("local failure contacted server") })
			subs := loadSubFile(t, dir)
			subs[0].Access = "write"
			if err := team.SaveSubs(dir, subs); err != nil {
				t.Fatal("cannot save state")
			}
			tc.damage(t, dir)
			assertAPIError(t, doTeam(h, http.MethodPost, subReadPath()+"/comments", `{"text":"synthetic","anchor":0,"key":"0123456789abcdef"}`), 500, tc.code)
		})
	}
}

func TestSubCommentPostNetworkCallsHoldNoLock(t *testing.T) {
	var handler *teamHandler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !handler.mu.TryLock() {
			t.Error("network request held handler lock")
			w.WriteHeader(500)
			return
		}
		handler.mu.Unlock()
		if r.URL.Path == "/v1/hello" {
			_, _ = io.WriteString(w, helloJSON(teamServerID, "Synthetic"))
			return
		}
		_, _ = io.WriteString(w, `{"generation":"`+subGen+`","results":[{"key":"c/0123456789abcdef","seq":1,"duplicate":false}],"last":1}`)
	}))
	defer srv.Close()
	h, dir := testTeamSurface(t, clientFor(srv))
	handler = asTeamHandler(t, h)
	joinLab(t, dir, teamServerID, srv.URL)
	s := baseSub()
	s.Access = "write"
	seedSub(t, dir, s)
	if rec := doTeam(h, http.MethodPost, subReadPath()+"/comments", `{"text":"synthetic","anchor":0,"key":"0123456789abcdef"}`); rec.Code != 200 {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestSubSeenBothLogsCapsAndWritesOnce(t *testing.T) {
	h, dir := testTeamSurface(t, team.NewClient())
	s := baseSub()
	seedSub(t, dir, s)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	records := func(n int) []team.MirrorRecord {
		out := make([]team.MirrorRecord, n)
		for i := range out {
			out[i] = team.MirrorRecord{Seq: uint64(i + 1), Key: fmt.Sprintf("k%d", i+1), Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: json.RawMessage(`{}`)}
		}
		return out
	}
	if err := team.AppendCopy(dir, id, team.MirrorLog, records(2)); err != nil {
		t.Fatal("cannot seed mirror")
	}
	if err := team.AppendCopy(dir, id, team.CommentsLog, records(3)); err != nil {
		t.Fatal("cannot seed comments")
	}
	writes := 0
	asTeamHandler(t, h).saveSubs = func(data string, subs []team.Subscription) error { writes++; return team.SaveSubs(data, subs) }
	for _, tc := range []struct {
		body             string
		mirror, comments uint64
		writes           int
	}{
		{`{"mirror":1}`, 1, 0, 1}, {`{"comments":2}`, 1, 2, 2},
		{`{"mirror":99,"comments":99}`, 2, 3, 3}, {`{"mirror":1,"comments":1}`, 2, 3, 3},
		{`{"comments":3}`, 2, 3, 3},
	} {
		rec := doTeam(h, http.MethodPut, subReadPath()+"/seen", tc.body)
		if rec.Code != 204 {
			t.Errorf("seen status = %d", rec.Code)
			continue
		}
		got := loadSubFile(t, dir)[0]
		if got.SeenMirror != tc.mirror || got.SeenComments != tc.comments || writes != tc.writes {
			t.Error("seen counters or write count incorrect")
		}
	}
	for i, body := range []string{`{}`, `{"comments":null}`, `{"mirror":null,"comments":1}`, `{"mirror":1,"comments":null}`, `{"comments":-1}`, `{"comments":1,"extra":2}`, `{"comments":1} {}`} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			assertAPIError(t, doTeam(h, http.MethodPut, subReadPath()+"/seen", body), 400, "bad_request")
		})
	}
}

func TestSubRemoveOwnerLiveOrEnded(t *testing.T) {
	for _, state := range []string{"live", "ended"} {
		t.Run(state, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
			defer srv.Close()
			h, dir := testTeamSurface(t, clientFor(srv))
			joinLab(t, dir, teamServerID, srv.URL)
			s := baseSub()
			s.Access = "owner"
			s.State = state
			seedSub(t, dir, s)
			id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
			record := team.MirrorRecord{Seq: 1, Key: "k1", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: json.RawMessage(`{}`)}
			if err := team.AppendCopy(dir, id, team.MirrorLog, []team.MirrorRecord{record}); err != nil {
				t.Fatal("cannot seed mirror")
			}
			if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{record}); err != nil {
				t.Fatal("cannot seed comments")
			}
			rec := doTeam(h, http.MethodDelete, subReadPath(), "")
			if state == "live" {
				assertAPIError(t, rec, 409, "own_share")
				if len(loadSubFile(t, dir)) != 1 {
					t.Error("live owner state removed")
				}
				for _, log := range []string{team.MirrorLog, team.CommentsLog} {
					_, last, err := team.ReadCopy(dir, id, log, 0, 256, 4<<20)
					if err != nil || last != 1 {
						t.Error("live owner copy removed")
					}
				}
			} else {
				if rec.Code != 204 {
					t.Errorf("ended owner status = %d", rec.Code)
				}
				if len(loadSubFile(t, dir)) != 0 {
					t.Error("ended owner state retained")
				}
				for _, log := range []string{team.MirrorLog, team.CommentsLog} {
					_, last, err := team.ReadCopy(dir, id, log, 0, 256, 4<<20)
					if err != nil || last != 0 {
						t.Error("ended owner copy retained")
					}
				}
			}
			if calls != 0 {
				t.Error("owner removal contacted server")
			}
		})
	}
}

func TestSubSeenDamagedCommentsDoesNotSaveMirror(t *testing.T) {
	h, dir := testTeamSurface(t, team.NewClient())
	seedSub(t, dir, baseSub())
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	record := team.MirrorRecord{Seq: 1, Key: "k1", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: json.RawMessage(`{}`)}
	if err := team.AppendCopy(dir, id, team.MirrorLog, []team.MirrorRecord{record}); err != nil {
		t.Fatal("cannot seed mirror")
	}
	path := filepath.Join(dir, team.CopiesDir, teamServerID, subOwner, subChat, subGen, "comments.jsonl")
	if err := os.WriteFile(path, []byte("bad\n"), 0o600); err != nil {
		t.Fatal("cannot damage comments")
	}
	assertAPIError(t, doTeam(h, http.MethodPut, subReadPath()+"/seen", `{"mirror":1,"comments":1}`), 500, "store_unavailable")
	if got := loadSubFile(t, dir)[0]; got.SeenMirror != 0 || got.SeenComments != 0 {
		t.Error("partial read marks were saved")
	}
}

func TestSubRouteRejectsWrongPrefixAndCommentMethods(t *testing.T) {
	h, _ := testTeamSurface(t, team.NewClient())
	th := asTeamHandler(t, h)
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/", nil)
	req.URL.Path = teamServerID + "/" + subOwner + "/" + subChat + "/" + subGen
	rec := httptest.NewRecorder()
	th.serveSubscriptions(rec, req)
	assertAPIError(t, rec, 404, "not_found")
	for _, tc := range []struct{ method, suffix string }{{http.MethodPut, "comments"}, {http.MethodDelete, "comments"}, {http.MethodGet, "seen"}, {http.MethodPost, "seen"}, {http.MethodGet, "other"}} {
		assertAPIError(t, doTeam(h, tc.method, subReadPath()+"/"+tc.suffix, ""), 404, "not_found")
	}
}

func TestSubCommentsReadRacingRemovalDoesNotRestoreCopy(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hello":
			_, _ = io.WriteString(w, helloJSON(teamServerID, "Synthetic"))
		case "/v1/channels/" + subOwner + "/" + subChat + "/comments":
			close(entered)
			<-release
			_, _ = io.WriteString(w, subPage(subGen, 1, false, subRecord(1, `{}`)))
		case "/v1/channels/" + subOwner + "/" + subChat + "/membership":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	seedSub(t, dir, baseSub())
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doTeam(h, http.MethodGet, subReadPath()+"/comments", "") }()
	<-entered
	removed := doTeam(h, http.MethodDelete, subReadPath(), "")
	close(release)
	read := <-done
	if removed.Code != 204 {
		t.Errorf("remove status = %d", removed.Code)
	}
	assertAPIError(t, read, 404, "not_found")
	path := filepath.Join(dir, team.CopiesDir, teamServerID, subOwner, subChat, subGen, "comments.jsonl")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("removed comments returned")
	}
}

func TestSubCommentsRosterRefusesChangedServer(t *testing.T) {
	rosterCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hello" {
			_, _ = io.WriteString(w, helloJSON(teamOtherID, "Synthetic"))
			return
		}
		rosterCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	h, dir := testTeamSurface(t, clientFor(srv))
	joinLab(t, dir, teamServerID, srv.URL)
	s := baseSub()
	s.State = "ended"
	seedSub(t, dir, s)
	id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
	record := team.MirrorRecord{Seq: 1, Key: "k1", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: json.RawMessage(`{"v":1,"t":"comment","text":"synthetic","anchor":0}`)}
	if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{record}); err != nil {
		t.Fatal("cannot seed comments")
	}
	got := decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""))
	if _, ok := got.Names[subOwner]; ok || rosterCalls != 0 {
		t.Error("changed server received roster request")
	}
}

/* Wave 5a condition table (each mutation was caught by the named test):
| condition | test for each half |
| channel access != read && != write | TestSubListAddsASharedChannel: read; write |
| mirror missing && comments missing | TestSubSeenBothLogsCapsAndWritesOnce: mirror only; comments only |
| version != 1 && version != 2 | TestSubsVersionTwoAndLegacyMigration: legacy; version two |
| comments_last missing || seen_comments missing | TestSubsVersionTwoNeedsBothValidCounters: each missing field |
| access != read && != write && != owner | TestSubsRoundTripKeepsFieldsAndMode: read and write; TestSubsVersionTwoAndLegacyMigration: owner |
*/

func TestSubCommentsGateOffDoesNothing(t *testing.T) {
	cmd, data, _, _ := startTeamCommand(t, false, true)
	core, err := newCoreMux(cmd.application)
	if err != nil {
		t.Fatal("cannot create core mux")
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		if got, want := recordedText(core, method, subReadPath()+"/comments"), recordedText(core, method, "/api/not-a-route"); got != want {
			t.Error("gate-off comment route differs from unknown route")
		}
	}
	assertNoTeamFile(t, data)
	assertNoSubsFile(t, data)
	if _, err := os.Stat(filepath.Join(data, "team-people.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("gate-off route touched people storage")
	}
	if _, err := os.Stat(filepath.Join(data, team.CopiesDir)); !errors.Is(err, os.ErrNotExist) {
		t.Error("gate-off comment route touched copies")
	}
}

func TestSubCommentsReadMirrorErrorCases(t *testing.T) {
	t.Run("gap is refused", func(t *testing.T) {
		h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, subPage(subGen, 2, false, subRecord(2, `{}`)))
		})
		got := decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""))
		if got.Last != 0 || len(got.Entries) != 0 {
			t.Error("out-of-order page was stored")
		}
		if loadSubFile(t, dir)[0].State != "live" {
			t.Error("gap ended subscription")
		}
	})
	t.Run("offline keeps existing copy", func(t *testing.T) {
		h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
		})
		id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
		record := team.MirrorRecord{Seq: 1, Key: "k1", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: json.RawMessage(`{"v":1,"t":"comment","text":"synthetic","anchor":0}`)}
		if err := team.AppendCopy(dir, id, team.CommentsLog, []team.MirrorRecord{record}); err != nil {
			t.Fatal("cannot seed comments")
		}
		got := decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""))
		if got.Last != 1 || len(got.Entries) != 1 {
			t.Error("offline read lost saved comment")
		}
	})
	t.Run("ending save fails", func(t *testing.T) {
		h, _ := readSurface(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, subPage(subGenB, 0, false)) })
		asTeamHandler(t, h).saveSubs = func(string, []team.Subscription) error { return errors.New("synthetic failure") }
		assertAPIError(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""), 500, "store_unavailable")
	})
	t.Run("state changes during fetch", func(t *testing.T) {
		var dir string
		h, data := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
			if err := os.WriteFile(filepath.Join(dir, team.SubsFile), []byte("bad"), 0o600); err != nil {
				t.Error("cannot damage state")
			}
			_, _ = io.WriteString(w, subPage(subGen, 0, false))
		})
		dir = data
		assertAPIError(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""), 500, "store_unavailable")
	})
	t.Run("copy damaged during fetch", func(t *testing.T) {
		var dir string
		h, data := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
			path := filepath.Join(dir, team.CopiesDir, teamServerID, subOwner, subChat, subGen, "comments.jsonl")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Error("cannot create copy")
			}
			if err := os.WriteFile(path, []byte("bad\n"), 0o600); err != nil {
				t.Error("cannot damage copy")
			}
			_, _ = io.WriteString(w, subPage(subGen, 1, false, subRecord(1, `{}`)))
		})
		dir = data
		assertAPIError(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""), 500, "copy_damaged")
	})
}

func TestSubCommentsReadAfterValidation(t *testing.T) {
	h, _ := readSurface(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, subPage(subGen, 0, false)) })
	for _, query := range []string{"after=", "after=-1", "after=1&after=2", "after=%zz", "after=18446744073709551616"} {
		assertAPIError(t, doTeam(h, http.MethodGet, subReadPath()+"/comments?"+query, ""), 400, "bad_request")
	}
}

func TestCommentNamesSurviveRestartAndRename(t *testing.T) {
	roster := "Ada"
	h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/seats" {
			_, _ = io.WriteString(w, seatsOf(subOwner, roster))
			return
		}
		_, _ = io.WriteString(w, subPage(subGen, 1, false, subRecord(1, `{"v":1,"t":"comment","text":"synthetic","anchor":0}`)))
	})
	first := decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""))
	if first.Names[subOwner] != "Ada" {
		t.Fatal("initial roster name missing")
	}
	original := asTeamHandler(t, h)
	roster = "Bea"
	renamed := newTeamHandler(dir, fixedTeamClock(), original.client)
	updated := decodeCommentsRead(t, doTeam(renamed, http.MethodGet, subReadPath()+"/comments?after=1", ""))
	if updated.Names[subOwner] != "Bea" {
		t.Fatal("current roster did not win")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "team-people.json"))
	if err != nil || !bytes.Contains(raw, []byte(`"name":"Bea"`)) {
		t.Fatalf("rename not stored: %s %v", raw, err)
	}
	if err := team.Remove(dir, teamServerID); err != nil {
		t.Fatal(err)
	}
	restarted := newTeamHandler(dir, fixedTeamClock(), original.client)
	saved := decodeCommentsRead(t, doTeam(restarted, http.MethodGet, subReadPath()+"/comments", ""))
	if saved.Names[subOwner] != "Bea" {
		t.Fatalf("departed author name = %q", saved.Names[subOwner])
	}
}

func TestCommentNamesLoadAndSaveFailuresStaySilent(t *testing.T) {
	for _, damaged := range []bool{false, true} {
		t.Run(fmt.Sprint(damaged), func(t *testing.T) {
			h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/seats" {
					_, _ = io.WriteString(w, seatsOf(subOwner, "Ada"))
					return
				}
				_, _ = io.WriteString(w, subPage(subGen, 1, false, subRecord(1, `{"v":1,"t":"comment","text":"synthetic","anchor":0}`)))
			})
			path := filepath.Join(dir, "team-people.json")
			if damaged {
				if err := os.WriteFile(path, []byte("broken"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			got := decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""))
			if got.Names[subOwner] != "Ada" {
				t.Fatal("roster missing on storage failure")
			}
			if damaged {
				raw, _ := os.ReadFile(path)
				if string(raw) != "broken" {
					t.Fatal("damaged people file overwritten")
				}
			}
		})
	}
}

func TestCommentPeoplePrunedOnlyAfterLastSubscription(t *testing.T) {
	h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/seats" {
			_, _ = io.WriteString(w, seatsOf(subOwner, "Ada"))
			return
		}
		_, _ = io.WriteString(w, subPage(subGen, 0, false))
	})
	other := baseSub()
	other.Chat = subChatB
	other.State = "ended"
	seedSub(t, dir, other)
	subs := loadSubFile(t, dir)
	subs[0].State = "ended"
	if err := team.SaveSubs(dir, subs); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "team-people.json")
	raw := []byte(`{"v":1,"people":[{"server":"` + teamServerID + `","seat":"` + subOwner + `","name":"Ada"},{"server":"` + teamOtherID + `","seat":"` + subOwner + `","name":"Other"}]}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if rec := doTeam(h, http.MethodDelete, subReadPath(), ""); rec.Code != 204 {
		t.Fatalf("remove = %d", rec.Code)
	}
	first, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(first, []byte(`"Ada"`)) {
		t.Fatal("pruned while another subscription exists")
	}
	secondPath := strings.Replace(subReadPath(), subChat, subChatB, 1)
	if rec := doTeam(h, http.MethodDelete, secondPath, ""); rec.Code != 204 {
		t.Fatalf("remove second = %d", rec.Code)
	}
	last, err := os.ReadFile(path)
	if err != nil || bytes.Contains(last, []byte(`"Ada"`)) || !bytes.Contains(last, []byte(`"Other"`)) {
		t.Fatalf("prune: %s %v", last, err)
	}
}

func TestCommentPeopleCapStillUpdatesKnownNames(t *testing.T) {
	h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/seats" {
			_, _ = io.WriteString(w, seatsOf(subOwner, "Renamed", subOwnerB, "New"))
			return
		}
		record := strings.Replace(subRecord(1, `{"v":1,"t":"comment","text":"synthetic","anchor":0}`), subOwner, subOwnerB, 1)
		_, _ = io.WriteString(w, subPage(subGen, 1, false, record))
	})
	rows := make([]map[string]string, 0, 4096)
	rows = append(rows, map[string]string{"server": teamServerID, "seat": subOwner, "name": "Old"})
	for i := 0; i < 4095; i++ {
		rows = append(rows, map[string]string{"server": teamServerID, "seat": fmt.Sprintf("%032x", i), "name": "Person"})
	}
	raw, _ := json.Marshal(map[string]any{"v": 1, "people": rows})
	path := filepath.Join(dir, "team-people.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	got := decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""))
	if got.Names[subOwner] != "Renamed" || got.Names[subOwnerB] != "New" {
		t.Fatal("roster names missing")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		People []struct{ Seat, Name string } `json:"people"`
	}
	if err := json.Unmarshal(after, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.People) != 4096 {
		t.Fatalf("count %d", len(doc.People))
	}
	for _, p := range doc.People {
		if p.Seat == subOwner && p.Name != "Renamed" {
			t.Fatal("known name not updated")
		}
		if p.Seat == subOwnerB {
			t.Fatal("added name past cap")
		}
	}
}

func TestCommentNamesOwnNameOverridesStoredAndRoster(t *testing.T) {
	var inst string
	h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/seats" {
			_, _ = io.WriteString(w, seatsOf(inst, "Roster"))
			return
		}
		_, _ = io.WriteString(w, subPage(subGen, 0, false))
	})
	inst = installationOf(t, dir)
	path := filepath.Join(dir, "team-people.json")
	raw := []byte(`{"v":1,"people":[{"server":"` + teamServerID + `","seat":"` + inst + `","name":"Stored"}]}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	servers, err := team.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""))
	if got.Names[inst] != servers[0].SeatName {
		t.Fatalf("own name = %q", got.Names[inst])
	}
}

func TestCommentPeopleSaveFailureKeepsReadAvailable(t *testing.T) {
	h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/seats" {
			_, _ = io.WriteString(w, seatsOf(subOwner, "Renamed"))
			return
		}
		_, _ = io.WriteString(w, subPage(subGen, 1, false, subRecord(1, `{"v":1,"t":"comment","text":"synthetic","anchor":0}`)))
	})
	rows := make([]map[string]string, 0, 4096)
	rows = append(rows, map[string]string{"server": teamServerID, "seat": subOwner, "name": "Old"})
	for i := 0; i < 4095; i++ {
		rows = append(rows, map[string]string{"server": teamServerID, "seat": fmt.Sprintf("%032x", i), "name": strings.Repeat("<", 64)})
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]any{"v": 1, "people": rows}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "team-people.json")
	before := buf.Bytes()
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	got := decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""))
	if got.Names[subOwner] != "Renamed" {
		t.Fatal("roster unavailable after save failed")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed save changed file")
	}
}

func TestCommentNamesDoNotStoreRosterMembersWithoutComments(t *testing.T) {
	h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/seats" {
			_, _ = io.WriteString(w, seatsOf(subOwner, "Writer", subOwnerB, "Reader"))
			return
		}
		_, _ = io.WriteString(w, subPage(subGen, 1, false, subRecord(1, `{"v":1,"t":"comment","text":"synthetic","anchor":0}`)))
	})
	got := decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""))
	if got.Names[subOwnerB] != "Reader" {
		t.Fatalf("roster name missing from answer: %+v", got.Names)
	}
	people, err := team.LoadPeople(dir)
	if err != nil {
		t.Fatal(err)
	}
	foundWriter := false
	for _, person := range people {
		if person.Server != teamServerID {
			continue
		}
		if person.Seat == subOwnerB {
			t.Fatal("stored a roster member who wrote no comment")
		}
		if person.Seat == subOwner && person.Name == "Writer" {
			foundWriter = true
		}
	}
	if !foundWriter {
		t.Fatal("comment author's name was not stored")
	}
}

func TestCommentUnnamedAuthorLearnedLaterSurvivesRestart(t *testing.T) {
	for _, otherSubscription := range []bool{false, true} {
		t.Run(fmt.Sprintf("other_subscription_%t", otherSubscription), func(t *testing.T) {
			rosterCalls := 0
			left := false
			h, dir := readSurface(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/seats" {
					rosterCalls++
					if rosterCalls == 1 {
						w.WriteHeader(http.StatusServiceUnavailable)
					} else if left {
						_, _ = io.WriteString(w, seatsOf())
					} else {
						_, _ = io.WriteString(w, seatsOf(subOwner, "Writer"))
					}
					return
				}
				if strings.Contains(r.URL.Path, "/"+subChatB+"/") {
					_, _ = io.WriteString(w, subPage(subGen, 0, false))
				} else if r.URL.Query().Get("after") == "0" {
					_, _ = io.WriteString(w, subPage(subGen, 1, false, subRecord(1, `{"v":1,"t":"comment","text":"synthetic","anchor":0}`)))
				} else {
					_, _ = io.WriteString(w, subPage(subGen, 1, false))
				}
			})
			if otherSubscription {
				other := baseSub()
				other.Chat = subChatB
				seedSub(t, dir, other)
			}
			clock := holdClock(t, h)
			first := decodeCommentsRead(t, doTeam(h, http.MethodGet, subReadPath()+"/comments", ""))
			if len(first.Entries) != 1 || first.Names[subOwner] != "" || rosterCalls != 1 {
				t.Fatalf("initial read: %+v, roster calls %d", first, rosterCalls)
			}
			*clock = clock.Add(61 * time.Second)
			path := subReadPath() + "/comments?after=1"
			if otherSubscription {
				path = strings.Replace(subReadPath(), subChat, subChatB, 1) + "/comments"
			}
			next := decodeCommentsRead(t, doTeam(h, http.MethodGet, path, ""))
			if len(next.Entries) != 0 || next.Names[subOwner] != "Writer" || rosterCalls != 2 {
				t.Fatalf("later read: %+v, roster calls %d", next, rosterCalls)
			}
			people, err := team.LoadPeople(dir)
			if err != nil || len(people) != 1 || people[0].Server != teamServerID || people[0].Seat != subOwner || people[0].Name != "Writer" {
				t.Fatalf("later name not stored: %+v, %v", people, err)
			}
			left = true
			original := asTeamHandler(t, h)
			restarted := newTeamHandler(dir, original.now, original.client)
			saved := decodeCommentsRead(t, doTeam(restarted, http.MethodGet, subReadPath()+"/comments", ""))
			if len(saved.Entries) != 1 || saved.Names[subOwner] != "Writer" {
				t.Fatalf("restart lost author name: %+v", saved)
			}
		})
	}
}

func TestCommentUnnamedAuthorsBoundAndRetry(t *testing.T) {
	handler, dir := testTeamSurface(t, team.NewClient())
	h := asTeamHandler(t, handler)
	ctx := context.Background()
	first := make([]map[string]any, 2048)
	second := make([]map[string]any, 2048)
	for i := 0; i < 2048; i++ {
		first[i] = map[string]any{"author": fmt.Sprintf("%032x", i)}
		second[i] = map[string]any{"author": fmt.Sprintf("%032x", i+2048)}
	}
	h.commentNames(ctx, teamServerID, team.Entry{}, false, "", first)
	h.commentNames(ctx, teamServerID, team.Entry{}, false, "", first)
	h.commentNames(ctx, teamOtherID, team.Entry{}, false, "", second)
	const overflow = "ffffffffffffffffffffffffffffffff"
	h.commentNames(ctx, teamOtherID, team.Entry{}, false, "", []map[string]any{{"author": overflow}})
	h.storeRoster(teamOtherID, []team.Seat{{ID: overflow, Name: "Overflow"}})
	h.commentNames(ctx, teamOtherID, team.Entry{}, false, "", nil)
	people, err := team.LoadPeople(dir)
	if err != nil || len(people) != 0 {
		t.Fatalf("overflow author was retained: %+v, %v", people, err)
	}
	known := fmt.Sprintf("%032x", 0)
	full := make([]team.Person, 4096)
	for i := range full {
		full[i] = team.Person{Server: teamOtherID, Seat: fmt.Sprintf("%032x", i), Name: "Existing"}
	}
	if err := team.SavePeople(dir, full); err != nil {
		t.Fatal(err)
	}
	h.storeRoster(teamServerID, []team.Seat{{ID: known, Name: "Known"}})
	h.commentNames(ctx, teamServerID, team.Entry{}, false, "", nil)
	if err := team.SavePeople(dir, nil); err != nil {
		t.Fatal(err)
	}
	h.commentNames(ctx, teamServerID, team.Entry{}, false, "", nil)
	people, err = team.LoadPeople(dir)
	if err != nil || len(people) != 1 || people[0].Server != teamServerID || people[0].Seat != known || people[0].Name != "Known" {
		t.Fatalf("capped author not retried: %+v, %v", people, err)
	}
	if err := team.SavePeople(dir, nil); err != nil {
		t.Fatal(err)
	}
	h.commentNames(ctx, teamServerID, team.Entry{}, false, "", nil)
	people, err = team.LoadPeople(dir)
	if err != nil || len(people) != 0 {
		t.Fatalf("saved author stayed unnamed: %+v, %v", people, err)
	}
	const later = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	h.commentNames(ctx, teamOtherID, team.Entry{}, false, "", []map[string]any{{"author": later}})
	h.storeRoster(teamOtherID, []team.Seat{{ID: later, Name: "Later"}})
	h.commentNames(ctx, teamOtherID, team.Entry{}, false, "", nil)
	people, err = team.LoadPeople(dir)
	if err != nil || len(people) != 1 || people[0].Seat != later {
		t.Fatalf("freed unnamed slot was not reused: %+v, %v", people, err)
	}
}

func TestCommentUnnamedAuthorRetriesStorageFailures(t *testing.T) {
	for _, mode := range []string{"load", "save"} {
		t.Run(mode, func(t *testing.T) {
			handler, dir := testTeamSurface(t, team.NewClient())
			h := asTeamHandler(t, handler)
			ctx := context.Background()
			h.commentNames(ctx, teamServerID, team.Entry{}, false, "", []map[string]any{{"author": subOwner}})
			path := filepath.Join(dir, team.PeopleFile)
			if mode == "load" {
				if err := os.WriteFile(path, []byte("broken"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				rows := make([]team.Person, 4095)
				for i := range rows {
					rows[i] = team.Person{Server: teamOtherID, Seat: fmt.Sprintf("%032x", i), Name: strings.Repeat("<", 64)}
				}
				var buf bytes.Buffer
				enc := json.NewEncoder(&buf)
				enc.SetEscapeHTML(false)
				if err := enc.Encode(map[string]any{"v": 1, "people": rows}); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			h.storeRoster(teamServerID, []team.Seat{{ID: subOwner, Name: "Writer"}})
			names := h.commentNames(ctx, teamServerID, team.Entry{}, false, "", nil)
			if names[subOwner] != "Writer" {
				t.Fatal("roster name lost on storage failure")
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "load" && string(before) != "broken" {
				t.Fatal("damaged file overwritten")
			}
			if mode == "save" && !bytes.Contains(before, []byte(strings.Repeat("<", 64))) {
				t.Fatal("failed save replaced the file")
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			h.commentNames(ctx, teamServerID, team.Entry{}, false, "", nil)
			people, err := team.LoadPeople(dir)
			if err != nil || len(people) != 1 || people[0].Seat != subOwner || people[0].Name != "Writer" {
				t.Fatalf("failed storage not retried: %+v, %v", people, err)
			}
		})
	}
}

func TestCommentUnnamedAuthorsPrunedPerServer(t *testing.T) {
	handler, dir := testTeamSurface(t, team.NewClient())
	h := asTeamHandler(t, handler)
	for _, item := range []struct{ server, chat string }{{teamServerID, subChat}, {teamServerID, subChatB}, {teamOtherID, subChat}} {
		sub := baseSub()
		sub.Server, sub.Chat, sub.State = item.server, item.chat, "ended"
		seedSub(t, dir, sub)
	}
	ctx := context.Background()
	h.commentNames(ctx, teamServerID, team.Entry{}, false, "", []map[string]any{{"author": subOwner}, {"author": subOwnerB}})
	h.commentNames(ctx, teamOtherID, team.Entry{}, false, "", []map[string]any{{"author": subOwnerC}})
	if rec := doTeam(handler, http.MethodDelete, subReadPath(), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("first remove: %d", rec.Code)
	}
	h.storeRoster(teamServerID, []team.Seat{{ID: subOwner, Name: "First"}})
	h.commentNames(ctx, teamServerID, team.Entry{}, false, "", nil)
	people, err := team.LoadPeople(dir)
	if err != nil || len(people) != 1 || people[0].Seat != subOwner {
		t.Fatalf("another subscription did not retain authors: %+v, %v", people, err)
	}
	second := strings.Replace(subReadPath(), subChat, subChatB, 1)
	if rec := doTeam(handler, http.MethodDelete, second, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("last remove: %d", rec.Code)
	}
	h.storeRoster(teamServerID, []team.Seat{{ID: subOwnerB, Name: "Dropped"}})
	h.storeRoster(teamOtherID, []team.Seat{{ID: subOwnerC, Name: "Other"}})
	h.commentNames(ctx, teamServerID, team.Entry{}, false, "", nil)
	h.commentNames(ctx, teamOtherID, team.Entry{}, false, "", nil)
	people, err = team.LoadPeople(dir)
	if err != nil || len(people) != 1 || people[0].Server != teamOtherID || people[0].Seat != subOwnerC {
		t.Fatalf("pruned incorrect server: %+v, %v", people, err)
	}
}

func TestCommentStoredAuthorIsNotUnnamed(t *testing.T) {
	handler, dir := testTeamSurface(t, team.NewClient())
	h := asTeamHandler(t, handler)
	if err := team.SavePeople(dir, []team.Person{{Server: teamServerID, Seat: subOwner, Name: "Known"}}); err != nil {
		t.Fatal(err)
	}
	h.commentNames(context.Background(), teamServerID, team.Entry{}, false, "", []map[string]any{{"author": subOwner}})
	if err := team.SavePeople(dir, nil); err != nil {
		t.Fatal(err)
	}
	h.storeRoster(teamServerID, []team.Seat{{ID: subOwner, Name: "Renamed"}})
	h.commentNames(context.Background(), teamServerID, team.Entry{}, false, "", nil)
	people, err := team.LoadPeople(dir)
	if err != nil || len(people) != 0 {
		t.Fatalf("already known author treated as unnamed: %+v, %v", people, err)
	}
}

func TestCommentNamedAuthorRetriesFailedInitialSave(t *testing.T) {
	handler, dir := testTeamSurface(t, team.NewClient())
	h := asTeamHandler(t, handler)
	path := filepath.Join(dir, team.PeopleFile)
	if err := os.WriteFile(path, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.commentNames(context.Background(), teamServerID, team.Entry{}, false, "", nil)
	h.storeRoster(teamServerID, []team.Seat{{ID: subOwner, Name: "Writer"}})
	names := h.commentNames(context.Background(), teamServerID, team.Entry{}, false, "", []map[string]any{{"author": subOwner}})
	if names[subOwner] != "Writer" {
		t.Fatal("roster name missing")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	h.commentNames(context.Background(), teamServerID, team.Entry{}, false, "", nil)
	people, err := team.LoadPeople(dir)
	if err != nil || len(people) != 1 || people[0].Seat != subOwner {
		t.Fatalf("named author not retried after failed load: %+v, %v", people, err)
	}
}

func TestCommentNamedAuthorRetriesAfterPeopleCap(t *testing.T) {
	handler, dir := testTeamSurface(t, team.NewClient())
	h := asTeamHandler(t, handler)
	people := make([]team.Person, 4096)
	for i := range people {
		people[i] = team.Person{Server: teamOtherID, Seat: fmt.Sprintf("%032x", i), Name: "Existing"}
	}
	if err := team.SavePeople(dir, people); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h.commentNames(ctx, teamServerID, team.Entry{}, false, "", nil)
	h.storeRoster(teamServerID, []team.Seat{{ID: subOwner, Name: "Writer"}})
	h.commentNames(ctx, teamServerID, team.Entry{}, false, "", []map[string]any{{"author": subOwner}})
	if err := team.SavePeople(dir, people[:len(people)-1]); err != nil {
		t.Fatal(err)
	}
	h.commentNames(ctx, teamServerID, team.Entry{}, false, "", nil)
	got, err := team.LoadPeople(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, person := range got {
		if person.Server == teamServerID && person.Seat == subOwner && person.Name == "Writer" {
			return
		}
	}
	t.Fatal("named author was not stored after a people slot became free")
}

func TestCommentNamedAuthorRetriesAfterSaveFailure(t *testing.T) {
	handler, dir := testTeamSurface(t, team.NewClient())
	h := asTeamHandler(t, handler)
	rows := make([]team.Person, 4095)
	for i := range rows {
		rows[i] = team.Person{Server: teamOtherID, Seat: fmt.Sprintf("%032x", i), Name: strings.Repeat("<", 64)}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]any{"v": 1, "people": rows}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, team.PeopleFile)
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h.commentNames(ctx, teamServerID, team.Entry{}, false, "", nil)
	h.storeRoster(teamServerID, []team.Seat{{ID: subOwner, Name: "Writer"}})
	h.commentNames(ctx, teamServerID, team.Entry{}, false, "", []map[string]any{{"author": subOwner}})
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, buf.Bytes()) {
		t.Fatalf("initial save did not fail without changing the file: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	h.commentNames(ctx, teamServerID, team.Entry{}, false, "", nil)
	people, err := team.LoadPeople(dir)
	if err != nil || len(people) != 1 || people[0].Seat != subOwner || people[0].Name != "Writer" {
		t.Fatalf("named author not retried after failed save: %+v, %v", people, err)
	}
}

func TestCommentStoredAuthorRecoveryReleasesUnnamedSlot(t *testing.T) {
	handler, dir := testTeamSurface(t, team.NewClient())
	h := asTeamHandler(t, handler)
	ctx := context.Background()
	if err := team.SavePeople(dir, []team.Person{{Server: teamServerID, Seat: subOwner, Name: "Known"}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, team.PeopleFile)
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	others := make([]map[string]any, 4095)
	for i := range others {
		others[i] = map[string]any{"author": fmt.Sprintf("%032x", i)}
	}
	h.commentNames(ctx, teamOtherID, team.Entry{}, false, "", others)
	h.commentNames(ctx, teamServerID, team.Entry{}, false, "", []map[string]any{{"author": subOwner}})
	if err := os.WriteFile(path, saved, 0o600); err != nil {
		t.Fatal(err)
	}
	h.commentNames(ctx, teamServerID, team.Entry{}, false, "", nil)
	const later = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	h.commentNames(ctx, teamOtherID, team.Entry{}, false, "", []map[string]any{{"author": later}})
	h.storeRoster(teamOtherID, []team.Seat{{ID: later, Name: "Later"}})
	h.commentNames(ctx, teamOtherID, team.Entry{}, false, "", nil)
	people, err := team.LoadPeople(dir)
	if err != nil || len(people) != 2 {
		t.Fatalf("recovered stored author did not release unnamed slot: %+v, %v", people, err)
	}
	for _, person := range people {
		if person.Server == teamOtherID && person.Seat == later && person.Name == "Later" {
			return
		}
	}
	t.Fatalf("new author was not retained after recovery: %+v", people)
}

func TestOwnedMirrorLocalAddress(t *testing.T) {
	rec := team.MirrorRecord{Seq: 7, Key: "2/37", Body: json.RawMessage(`{"v":1,"t":"user","text":"synthetic"}`)}
	for _, tc := range []struct {
		access string
		want   bool
	}{{"owner", true}, {"read", false}, {"write", false}} {
		got, _, ok := projectSubRecordForAccess(rec, tc.access)
		if !ok {
			t.Fatal("entry rejected")
		}
		segment, present := got["local_segment"]
		if present != tc.want {
			t.Fatalf("access %s address presence = %v", tc.access, present)
		}
		if tc.want && (segment != 2 || got["local_record"] != 37) {
			t.Fatalf("address = %v", got)
		}
	}
	for _, key := range []string{"start", "-1/2", "1/-2", "a/2", "1/2/3"} {
		rec.Key = key
		got, _, ok := projectSubRecordForAccess(rec, "owner")
		if !ok {
			t.Fatal("entry rejected")
		}
		if _, exists := got["local_record"]; exists {
			t.Fatalf("invalid key %q exposed", key)
		}
	}
}

func TestSubReadLocalAddressOwnerOnly(t *testing.T) {
	for _, access := range []string{"owner", "read", "write"} {
		t.Run(access, func(t *testing.T) {
			h, dir := readSurface(t, func(http.ResponseWriter, *http.Request) {})
			subs, err := team.LoadSubs(dir)
			if err != nil {
				t.Fatal(err)
			}
			subs[0].Access = access
			subs[0].State = "ended"
			if err := team.SaveSubs(dir, subs); err != nil {
				t.Fatal(err)
			}
			id := team.CopyID{Server: teamServerID, Owner: subOwner, Chat: subChat, Generation: subGen}
			record := team.MirrorRecord{Seq: 1, Key: "2/37", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: json.RawMessage(`{"v":1,"t":"user","text":"synthetic"}`)}
			if err := team.AppendCopy(dir, id, team.MirrorLog, []team.MirrorRecord{record}); err != nil {
				t.Fatal(err)
			}
			result := decodeSubRead(t, doTeam(h, http.MethodGet, subReadPath(), ""))
			if len(result.Entries) != 1 {
				t.Fatalf("entries = %v", result.Entries)
			}
			_, present := result.Entries[0]["local_record"]
			if present != (access == "owner") {
				t.Fatalf("access=%s entry=%v", access, result.Entries[0])
			}
		})
	}
}

func TestOwnerStartPositionTracksSeamsAndFromNow(t *testing.T) {
	events := []sessionlog.Event{{T: "meta"}, {T: "user"}, {T: "source"}, {T: "assistant"}}
	for _, tc := range []struct {
		start, segment, record int
		ok                     bool
	}{{0, 0, -1, true}, {2, 0, 1, true}, {3, 1, 2, true}, {4, 1, 3, true}, {5, 0, 0, false}, {-1, 0, 0, false}} {
		segment, record, ok := ownerStartPosition(events, tc.start)
		if ok != tc.ok || (ok && (segment != tc.segment || record != tc.record)) {
			t.Fatalf("start %d: %d,%d,%v", tc.start, segment, record, ok)
		}
	}
}

func TestShareViewCarriesGenerationForOwnerSubscriptionMatching(t *testing.T) {
	h := &teamHandler{}
	got := h.shareAPI(team.Share{UID: "aaaaaaaaaaaaaaaa", Node: "local", Server: teamServerID, Generation: subGen})
	if got.Generation != subGen {
		t.Fatalf("generation=%q", got.Generation)
	}
}

func TestOwnerFrontierSurvivesShareRetirementAndRestart(t *testing.T) {
	dir := t.TempDir()
	inst, err := identity.LoadOrCreateInstallation(dir)
	if err != nil {
		t.Fatal(err)
	}
	sub := baseSub()
	sub.Owner = string(inst)
	sub.Access = "owner"
	seedSub(t, dir, sub)
	writeChatLog(t, dir, "local", sub.Chat, 2)
	mustSaveShare(t, dir, team.Share{UID: sub.Chat, Node: "local", Server: sub.Server, Generation: sub.Generation, Start: 2, Next: 2})
	id := team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}
	h := &teamHandler{dataDir: dir}
	seg, rec, ok := h.ownerStartAddress(id)
	if !ok || seg != 0 || rec != 1 {
		t.Fatalf("live frontier %d/%d %v", seg, rec, ok)
	}
	if err := team.RemoveShare(dir, sub.Chat); err != nil {
		t.Fatal(err)
	}
	h = &teamHandler{dataDir: dir}
	seg, rec, ok = h.ownerStartAddress(id)
	if !ok || seg != 0 || rec != 1 {
		t.Fatalf("retained frontier %d/%d %v", seg, rec, ok)
	}
}

func TestStopRetainsOwnerFrontierBeforeDeletingShare(t *testing.T) {
	dir := t.TempDir()
	inst, err := identity.LoadOrCreateInstallation(dir)
	if err != nil {
		t.Fatal(err)
	}
	sub := baseSub()
	sub.Owner = string(inst)
	sub.Access = "owner"
	seedSub(t, dir, sub)
	writeChatLog(t, dir, "local", sub.Chat, 2)
	share := team.Share{UID: sub.Chat, Node: "local", Server: sub.Server, Generation: sub.Generation, Start: 2, Next: 2}
	mustSaveShare(t, dir, share)
	h := &teamHandler{dataDir: dir}
	rec := httptest.NewRecorder()
	h.finishStop(rec, share)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d", rec.Code)
	}
	saved, err := team.LoadSubs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if saved[0].OwnerFrontier == nil || saved[0].OwnerFrontier.Record != 1 {
		t.Fatalf("frontier %+v", saved[0].OwnerFrontier)
	}
}

func TestOwnerLocalNodeRequiresMatchingLogIdentity(t *testing.T) {
	dir := t.TempDir()
	h := &teamHandler{dataDir: dir}
	sub := baseSub()
	sub.Access = "owner"
	sub.OwnerFrontier = &team.OwnerFrontier{Node: "local", Segment: 0, Record: 1}
	writeChatLog(t, dir, "local", sub.Chat, 1)
	got := h.subscriptionView([]team.Subscription{sub}, nil, false)
	if got.Subscriptions[0].LocalNode != "local" || got.Subscriptions[0].LocalFrontier == nil {
		t.Fatalf("view %+v", got.Subscriptions[0])
	}
	sub.Chat = subChatB
	got = h.subscriptionView([]team.Subscription{sub}, nil, false)
	if got.Subscriptions[0].LocalNode != "" {
		t.Fatalf("reused node exposed %+v", got.Subscriptions[0])
	}
	sub.Chat = subChat
	sub.Access = "read"
	got = h.subscriptionView([]team.Subscription{sub}, nil, false)
	if got.Subscriptions[0].LocalNode != "" || got.Subscriptions[0].LocalFrontier != nil {
		t.Fatalf("recipient node exposed %+v", got.Subscriptions[0])
	}
}

func TestCaptureOwnerFrontierOnSubscriptionRefresh(t *testing.T) {
	dir := t.TempDir()
	inst, err := identity.LoadOrCreateInstallation(dir)
	if err != nil {
		t.Fatal(err)
	}
	sub := baseSub()
	sub.Owner = string(inst)
	sub.Access = "owner"
	writeChatLog(t, dir, "local", sub.Chat, 2)
	share := team.Share{UID: sub.Chat, Node: "local", Server: sub.Server, Generation: sub.Generation, Start: 2, Next: 2}
	mustSaveShare(t, dir, share)
	h := &teamHandler{dataDir: dir}
	subs := []team.Subscription{sub}
	if !h.captureOwnerFrontiers(subs, string(inst)) || subs[0].OwnerFrontier == nil || subs[0].OwnerFrontier.Record != 1 {
		t.Fatalf("frontier %+v", subs[0].OwnerFrontier)
	}
	if h.captureOwnerFrontiers(subs, string(inst)) {
		t.Fatal("unchanged frontier reported changed")
	}
}

func TestOwnerFrontierFailurePaths(t *testing.T) {
	for _, kind := range []string{"identity_missing", "wrong_owner", "subs_damaged", "other_subscription", "shares_damaged", "other_share", "missing_log", "cursor_beyond_log", "save_failure", "no_share"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			inst, err := identity.LoadOrCreateInstallation(dir)
			if err != nil {
				t.Fatal(err)
			}
			sub := baseSub()
			sub.Owner = string(inst)
			sub.Access = "owner"
			seedSub(t, dir, sub)
			writeChatLog(t, dir, "local", sub.Chat, 2)
			share := team.Share{UID: sub.Chat, Node: "local", Server: sub.Server, Generation: sub.Generation, Start: 2, Next: 2}
			if kind == "other_share" {
				share.Generation = subGenB
			}
			if kind == "cursor_beyond_log" {
				share.Start = 99
				share.Next = 99
			}
			if kind != "no_share" {
				mustSaveShare(t, dir, share)
			}
			id := team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}
			h := &teamHandler{dataDir: dir}
			switch kind {
			case "identity_missing":
				if err := os.Remove(filepath.Join(dir, "installation.json")); err != nil {
					t.Fatal(err)
				}
			case "wrong_owner":
				id.Owner = subOwnerB
			case "subs_damaged":
				if err := os.WriteFile(filepath.Join(dir, team.SubsFile), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "other_subscription":
				id.Generation = subGenB
			case "shares_damaged":
				if err := os.WriteFile(filepath.Join(dir, team.SharesFile), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing_log":
				if err := os.Remove(chatLogPath(dir, "local")); err != nil {
					t.Fatal(err)
				}
			case "save_failure":
				h.saveSubs = func(string, []team.Subscription) error { return errors.New("synthetic failure") }
			}
			if _, _, ok := h.ownerStartAddress(id); ok {
				t.Fatalf("%s unexpectedly resolved", kind)
			}
		})
	}
}

func TestCaptureOwnerFrontiersRejectsDamagedOrUnrelatedState(t *testing.T) {
	dir := t.TempDir()
	inst, err := identity.LoadOrCreateInstallation(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := &teamHandler{dataDir: dir}
	sub := baseSub()
	sub.Owner = string(inst)
	sub.Access = "owner"
	subs := []team.Subscription{sub}
	if err := os.WriteFile(filepath.Join(dir, team.SharesFile), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if h.captureOwnerFrontiers(subs, string(inst)) {
		t.Fatal("damaged shares captured")
	}
	if err := os.Remove(filepath.Join(dir, team.SharesFile)); err != nil {
		t.Fatal(err)
	}
	mustSaveShare(t, dir, team.Share{UID: sub.Chat, Node: "local", Server: sub.Server, Generation: subGenB})
	if h.captureOwnerFrontiers(subs, string(inst)) {
		t.Fatal("different generation captured")
	}
	mustSaveShare(t, dir, team.Share{UID: sub.Chat, Node: "local", Server: sub.Server, Generation: sub.Generation})
	if h.captureOwnerFrontiers(subs, string(inst)) {
		t.Fatal("missing log captured")
	}
}

func TestStopKeepsShareWhenOwnerFrontierCannotBeSaved(t *testing.T) {
	for _, kind := range []string{"bad_subs", "missing_log"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			inst, err := identity.LoadOrCreateInstallation(dir)
			if err != nil {
				t.Fatal(err)
			}
			sub := baseSub()
			sub.Owner = string(inst)
			sub.Access = "owner"
			seedSub(t, dir, sub)
			share := team.Share{UID: sub.Chat, Node: "local", Server: sub.Server, Generation: sub.Generation}
			mustSaveShare(t, dir, share)
			if kind == "bad_subs" {
				if err := os.WriteFile(filepath.Join(dir, team.SubsFile), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			h := &teamHandler{dataDir: dir}
			rec := httptest.NewRecorder()
			h.finishStop(rec, share)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status %d", rec.Code)
			}
			shares, err := team.LoadShares(dir)
			if err != nil || len(shares) != 1 {
				t.Fatalf("share not retained: %v %v", shares, err)
			}
		})
	}
}
