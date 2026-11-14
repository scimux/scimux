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

func TestSubListKeepsAnEndedSubscriptionEnded(t *testing.T) {
	script := &subScript{
		channelsBody: channelsOf(guestChannel(subOwner, subChat, "Synthetic title", subGen, "read", 2)),
		seatsBody:    seatsOf(subOwner, "Ada"),
	}
	h, dir, _ := subSurface(t, script)
	clock := holdClock(t, h)
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
	if len(got.Subscriptions) != 1 || one.State != "ended" || one.Title != "Synthetic title" || one.Access != "read" || one.MirrorLast != 2 || one.SeenMirror != 0 {
		t.Fatalf("revived = %+v list %+v", one, got.Subscriptions)
	}
	path := filepath.Join(dir, team.SubsFile)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(10 * time.Second)
	script.mu.Lock()
	script.channelsBody = `{"channels":[]}`
	script.mu.Unlock()
	again, _ := readSubList(t, h)
	if mustFindSub(t, again, subOwner, subChat, subGen).State != "ended" {
		t.Fatalf("ended twice = %+v", again.Subscriptions)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("ending an ended subscription rewrote the file")
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
	script.seatsBody = seatsOf(subOwner, "Bea")
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
