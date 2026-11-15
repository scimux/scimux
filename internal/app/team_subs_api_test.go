package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/identity"
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
	Server     string `json:"server"`
	ServerName string `json:"server_name"`
	Owner      string `json:"owner"`
	OwnerName  string `json:"owner_name"`
	Chat       string `json:"chat"`
	Generation string `json:"generation"`
	Title      string `json:"title"`
	Access     string `json:"access"`
	State      string `json:"state"`
	MirrorLast uint64 `json:"mirror_last"`
	SeenMirror uint64 `json:"seen_mirror"`
	Unread     bool   `json:"unread"`
	FirstSeen  string `json:"first_seen"`
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

func TestSubListIgnoresOwnedChannels(t *testing.T) {
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
	if len(got.Subscriptions) != 1 {
		t.Fatalf("subscriptions = %+v", got.Subscriptions)
	}
	one := mustFindSub(t, got, subOwner, subChatC, subGen)
	if one.State != "live" || one.Access != "write" || one.Title != "North <relay>" || one.OwnerName != "Ada" {
		t.Fatalf("kept = %+v", one)
	}
	if _, ok := findSub(got, inst, subChat, subGen); ok {
		t.Fatal("owned channel was stored")
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
	if err := team.AppendCopy(dir, id, []team.MirrorRecord{{Seq: 1, Key: "k1", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: json.RawMessage(`{"v":1}`)}}); err != nil {
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
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		rec := doTeam(h, http.MethodGet, "/api/team/subscriptions", "")
		assertAPIError(t, rec, http.StatusInternalServerError, "store_unavailable")
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
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
	stored, last, err := team.ReadCopy(dir, id, 0, 256, 4<<20)
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
	if err := team.AppendCopy(dir, id, []team.MirrorRecord{entry}); err != nil {
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
	if err := os.WriteFile(path, []byte("broken"), 0o600); err != nil {
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
	entries, last, err := team.ReadCopy(dir, id, 0, 256, 4<<20)
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
	entries, _, err := team.ReadCopy(dir, id, 0, 256, 4<<20)
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
	if err := os.WriteFile(path, []byte("broken"), 0o600); err != nil {
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
				if err := os.WriteFile(path, []byte("broken"), 0o600); err != nil {
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
			if err := team.AppendCopy(dir, id, entries); err != nil {
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
	if err := team.AppendCopy(dir, id, recs); err != nil {
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
	if err := team.AppendCopy(dir, id, []team.MirrorRecord{{Seq: 1, Key: "k1", Author: subOwner, At: "2026-11-14T09:30:00.000Z", Body: json.RawMessage(`{"v":1,"t":"user","text":"x"}`)}}); err != nil {
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
