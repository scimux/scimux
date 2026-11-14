package app

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/team"
)

const (
	shareNode  = "chat"
	shareUID   = "0123456789abcdef"
	shareUIDB  = "fedcba9876543210"
	shareUIDC  = "0000000000000001"
	shareGen   = "abcdef0123456789"
	shareSeat  = "00112233445566778899aabbccddeeff"
	shareSeatB = "ffeeddccbbaa99887766554433221100"
	shareText  = "synthetic prompt"
)

func TestShareRoutesAreDocumented(t *testing.T) {
	root := repoRootFromTest(t)
	api, err := os.ReadFile(filepath.Join(root, "docs", "http-api.md"))
	if err != nil {
		t.Fatal(err)
	}
	section := string(api)
	if i := strings.Index(section, "## Team\n"); i >= 0 {
		section = section[i:]
	}
	for _, needle := range []string{
		"GET /api/team/servers/{id}/seats",
		"GET /api/team/shares",
		"GET /api/team/shares/{node}",
		"PUT /api/team/shares/{node}",
		"DELETE /api/team/shares/{node}",
		"no_history",
		"share_fixed",
		"not_active",
		"too_many",
		"unknown_member",
		"`refused` | The team server answered 401",
		"Before the first attempt it is `ok`",
	} {
		if !strings.Contains(section, needle) {
			t.Errorf("docs/http-api.md Team section missing %q", needle)
		}
	}
	storage, err := os.ReadFile(filepath.Join(root, "docs", "invariants", "storage.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{
		"team-shares.json",
		"holds no chat content and no token",
	} {
		if !strings.Contains(string(storage), needle) {
			t.Errorf("docs/invariants/storage.md missing %q", needle)
		}
	}
}

type shareCall struct {
	method string
	path   string
	auth   string
	body   string
}

type shareScript struct {
	helloStatus  int
	helloID      string
	helloBody    string
	putStatuses  []int
	memberStatus int
	deleteStatus int
	seatsStatus  int
	seatsBody    string
	channel      string
	failPath     string
}

func (s *shareScript) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.failPath != "" && r.URL.Path == s.failPath {
		hj, ok := w.(http.Hijacker)
		if !ok {
			panic("share script: no hijack")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			panic(err)
		}
		_ = conn.Close()
		return
	}
	switch {
	case r.URL.Path == "/v1/hello":
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
			_, _ = io.WriteString(w, helloJSON(id, "Lab relay"))
			return
		}
		body := s.helloBody
		if body == "" {
			body = `{"error":"nope"}`
		}
		_, _ = io.WriteString(w, body)
	case r.URL.Path == "/v1/seats":
		status := s.seatsStatus
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		if s.seatsBody != "" {
			_, _ = io.WriteString(w, s.seatsBody)
		}
	case strings.HasSuffix(r.URL.Path, "/members"):
		status := s.memberStatus
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		if status == http.StatusOK && s.channel != "" {
			_, _ = io.WriteString(w, s.channel)
		}
	case r.Method == http.MethodDelete:
		status := s.deleteStatus
		if status == 0 {
			status = http.StatusNoContent
		}
		w.WriteHeader(status)
	case r.Method == http.MethodPut:
		status := http.StatusCreated
		if len(s.putStatuses) > 0 {
			status = s.putStatuses[0]
			s.putStatuses = s.putStatuses[1:]
		}
		w.WriteHeader(status)
		if (status == http.StatusOK || status == http.StatusCreated) && s.channel != "" {
			_, _ = io.WriteString(w, s.channel)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func recordShareServer(t *testing.T, script *shareScript) (*httptest.Server, *[]shareCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []shareCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		mu.Lock()
		calls = append(calls, shareCall{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"), body: string(raw)})
		mu.Unlock()
		script.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func snapshotCalls(calls *[]shareCall) []shareCall {
	out := make([]shareCall, len(*calls))
	copy(out, *calls)
	return out
}

func installationOf(t *testing.T, dir string) string {
	t.Helper()
	id, err := identity.LoadInstallation(dir)
	if err != nil {
		t.Fatal(err)
	}
	return string(id)
}

func appendStoreLine(t *testing.T, dir string, line []byte) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, "nodes.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func appendNode(t *testing.T, dir, id, title string) {
	t.Helper()
	raw, err := json.Marshal(storeRecord{Type: "node", Node: &Node{
		ID: id, Title: title, Agent: "claude", Dir: "/tmp/synthetic", CreatedAt: "2026-11-12T09:30:00Z",
	}})
	if err != nil {
		t.Fatal(err)
	}
	appendStoreLine(t, dir, raw)
}

func appendDelete(t *testing.T, dir, id string) {
	t.Helper()
	raw, err := json.Marshal(storeRecord{Type: "delete", ID: id})
	if err != nil {
		t.Fatal(err)
	}
	appendStoreLine(t, dir, raw)
}

func chatLogPath(dir, id string) string {
	return filepath.Join(dir, "sessions", id+".jsonl")
}

func writeChatLog(t *testing.T, dir, id, uid string, users int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	w := &sessionlog.Writer{Path: chatLogPath(dir, id)}
	if err := w.Append(sessionlog.Event{
		T: "meta", Time: "2026-11-12T09:30:00Z",
		Meta: &sessionlog.MetaEvent{Node: id, UID: uid, Agent: "claude", Created: "2026-11-12T09:30:00Z"},
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < users; i++ {
		if err := w.Append(sessionlog.Event{T: "user", Time: "2026-11-12T09:31:00Z", Text: shareText}); err != nil {
			t.Fatal(err)
		}
	}
}

func writeUserOnlyLog(t *testing.T, dir, id string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	w := &sessionlog.Writer{Path: chatLogPath(dir, id)}
	if err := w.Append(sessionlog.Event{T: "user", Time: "2026-11-12T09:31:00Z", Text: shareText}); err != nil {
		t.Fatal(err)
	}
}

func channelJSON(owner, chat, title, gen string) string {
	titleJSON, err := json.Marshal(title)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf(
		`{"owner":"%s","chat":"%s","title":%s,"generation":"%s","created_at":"2026-11-14T09:30:00.000Z","access":"owner","mirror_last":0,"comments_last":0,"members":[]}`,
		owner, chat, titleJSON, gen,
	)
}

func channelBase(inst, uid string) string {
	return "/v1/channels/" + inst + "/" + uid
}

func sharePutBody(server, scope string, members []team.Member) string {
	raw, err := json.Marshal(struct {
		Server  string        `json:"server"`
		Scope   string        `json:"scope"`
		Members []team.Member `json:"members"`
	}{Server: server, Scope: scope, Members: members})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

type shareView struct {
	UID      string        `json:"uid"`
	Node     string        `json:"node"`
	Server   string        `json:"server"`
	Scope    string        `json:"scope"`
	State    string        `json:"state"`
	Delivery string        `json:"delivery"`
	Members  []team.Member `json:"members"`
	SharedAt string        `json:"shared_at"`
}

func decodeShareView(t *testing.T, rec *httptest.ResponseRecorder) shareView {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	var got shareView
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("view %s: %v", rec.Body.String(), err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		t.Fatalf("trailing view: %v", err)
	}
	return got
}

func decodeShareList(t *testing.T, rec *httptest.ResponseRecorder) []shareView {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	var got struct {
		Shares []shareView `json:"shares"`
	}
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("list %s: %v", rec.Body.String(), err)
	}
	return got.Shares
}

func assertMembersEqual(t *testing.T, got, want []team.Member) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("members = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("members = %+v, want %+v", got, want)
		}
	}
	if want != nil && got == nil {
		t.Fatal("members is null")
	}
}

func storedShare(t *testing.T, dir, uid string) (team.Share, bool) {
	t.Helper()
	shares, err := team.LoadShares(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range shares {
		if s.UID == uid {
			return s, true
		}
	}
	return team.Share{}, false
}

func assertNoStoredShare(t *testing.T, dir, uid string) {
	t.Helper()
	if _, ok := storedShare(t, dir, uid); ok {
		t.Fatalf("share %s was stored", uid)
	}
}

func assertBearer(t *testing.T, call shareCall, want bool) {
	t.Helper()
	if want {
		if call.auth != "Bearer "+teamToken {
			t.Fatalf("%s %s auth = %q", call.method, call.path, call.auth)
		}
		return
	}
	if call.auth != "" {
		t.Fatalf("%s %s carried auth %q", call.method, call.path, call.auth)
	}
}

func countDeletes(calls []shareCall) int {
	n := 0
	for _, c := range calls {
		if c.method == http.MethodDelete {
			n++
		}
	}
	return n
}

func lockShareDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}

func readySurface(t *testing.T, script *shareScript) (http.Handler, string, *[]shareCall, string) {
	t.Helper()
	srv, calls := recordShareServer(t, script)
	h, dir := testTeamSurface(t, clientFor(srv))
	seedTeam(t, dir, labEntry(teamServerID, srv.URL, teamToken))
	return h, dir, calls, installationOf(t, dir)
}

func putShare(h http.Handler, node, body string) *httptest.ResponseRecorder {
	return doTeam(h, http.MethodPut, "/api/team/shares/"+node, body)
}

func TestSeatsReturnSelfInServerOrder(t *testing.T) {
	script := &shareScript{seatsBody: `{"seats":[{"id":"` + shareSeat + `","name":"Ada"},{"id":"` + shareSeatB + `","name":"Bea"}]}`}
	h, dir, calls, inst := readySurface(t, script)
	rec := doTeam(h, http.MethodGet, "/api/team/servers/"+teamServerID+"/seats", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	assertNoSecrets(t, rec.Body.String(), "", teamToken)
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	var got struct {
		Self  string `json:"self"`
		Seats []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"seats"`
	}
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("body %s: %v", rec.Body.String(), err)
	}
	if got.Self != inst {
		t.Fatalf("self = %q, want %s", got.Self, inst)
	}
	if len(got.Seats) != 2 || got.Seats[0].ID != shareSeat || got.Seats[0].Name != "Ada" || got.Seats[1].ID != shareSeatB || got.Seats[1].Name != "Bea" {
		t.Fatalf("seats = %+v", got.Seats)
	}
	gotCalls := snapshotCalls(calls)
	if len(gotCalls) != 2 || gotCalls[0].path != "/v1/hello" || gotCalls[1].path != "/v1/seats" {
		t.Fatalf("calls = %+v", gotCalls)
	}
	assertBearer(t, gotCalls[0], false)
	assertBearer(t, gotCalls[1], true)
	if _, err := os.Stat(filepath.Join(dir, team.SharesFile)); !os.IsNotExist(err) {
		t.Fatal("seats created a share file")
	}

	empty := &shareScript{seatsBody: `{"seats":[]}`}
	h, _, _, inst = readySurface(t, empty)
	rec = doTeam(h, http.MethodGet, "/api/team/servers/"+teamServerID+"/seats", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"seats":[]`) || !strings.Contains(rec.Body.String(), `"self":"`+inst+`"`) {
		t.Fatalf("empty = %d %s", rec.Code, rec.Body.String())
	}
}

func TestSeatsMapEveryError(t *testing.T) {
	t.Run("unknown server", func(t *testing.T) {
		h, _, calls, _ := readySurface(t, &shareScript{})
		rec := doTeam(h, http.MethodGet, "/api/team/servers/"+teamOtherID+"/seats", "")
		assertAPIError(t, rec, http.StatusNotFound, "not_found")
		assertNoSecrets(t, rec.Body.String(), "", teamToken)
		if len(snapshotCalls(calls)) != 0 {
			t.Fatalf("calls = %+v", snapshotCalls(calls))
		}
	})
	t.Run("damaged file", func(t *testing.T) {
		h, dir, calls, _ := readySurface(t, &shareScript{})
		if err := os.WriteFile(filepath.Join(dir, team.File), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		rec := doTeam(h, http.MethodGet, "/api/team/servers/"+teamServerID+"/seats", "")
		assertAPIError(t, rec, http.StatusInternalServerError, "store_unavailable")
		if len(snapshotCalls(calls)) != 0 {
			t.Fatalf("calls = %+v", snapshotCalls(calls))
		}
	})
	t.Run("identity", func(t *testing.T) {
		h, dir, calls, _ := readySurface(t, &shareScript{})
		if err := os.Remove(filepath.Join(dir, identity.InstallationFile)); err != nil {
			t.Fatal(err)
		}
		rec := doTeam(h, http.MethodGet, "/api/team/servers/"+teamServerID+"/seats", "")
		assertAPIError(t, rec, http.StatusInternalServerError, "identity_unavailable")
		if len(snapshotCalls(calls)) != 0 {
			t.Fatalf("calls = %+v", snapshotCalls(calls))
		}
	})
	t.Run("canceled", func(t *testing.T) {
		h, dir, _, _ := readySurface(t, &shareScript{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/team/servers/"+teamServerID+"/seats", nil).WithContext(ctx)
		req.Host = "127.0.0.1"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assertAPIError(t, rec, http.StatusBadGateway, "unreachable")
		assertNoStoredShare(t, dir, shareUID)
	})
	for _, tc := range []struct {
		name   string
		script *shareScript
		status int
		code   string
		seats  bool
	}{
		{"hello down", &shareScript{failPath: "/v1/hello"}, http.StatusBadGateway, "unreachable", false},
		{"hello other server", &shareScript{helloID: teamOtherID}, http.StatusBadGateway, "bad_server", false},
		{"hello bad", &shareScript{helloStatus: http.StatusInternalServerError}, http.StatusBadGateway, "bad_server", false},
		{"refused", &shareScript{seatsStatus: http.StatusUnauthorized}, http.StatusBadGateway, "refused", true},
		{"unknown member", &shareScript{seatsStatus: http.StatusUnprocessableEntity}, http.StatusUnprocessableEntity, "unknown_member", true},
		{"quota", &shareScript{seatsStatus: http.StatusInsufficientStorage}, http.StatusInsufficientStorage, "server_full", true},
		{"no channel", &shareScript{seatsStatus: http.StatusNotFound}, http.StatusBadGateway, "bad_server", true},
		{"protocol", &shareScript{seatsStatus: http.StatusInternalServerError}, http.StatusBadGateway, "bad_server", true},
		{"seats down", &shareScript{failPath: "/v1/seats"}, http.StatusBadGateway, "unreachable", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, calls, _ := readySurface(t, tc.script)
			rec := doTeam(h, http.MethodGet, "/api/team/servers/"+teamServerID+"/seats", "")
			assertAPIError(t, rec, tc.status, tc.code)
			assertNoSecrets(t, rec.Body.String(), "", teamToken)
			got := snapshotCalls(calls)
			if len(got) == 0 || got[0].path != "/v1/hello" {
				t.Fatalf("calls = %+v", got)
			}
			assertBearer(t, got[0], false)
			sawSeats := false
			for _, c := range got {
				if c.path == "/v1/seats" {
					sawSeats = true
					assertBearer(t, c, true)
				}
			}
			if sawSeats != tc.seats {
				t.Fatalf("seats call = %v, calls %+v", sawSeats, got)
			}
		})
	}
}

func TestSeatsDoNotHoldTheFileLock(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/seats" {
			once.Do(func() { close(entered) })
			<-release
		}
		switch r.URL.Path {
		case "/v1/hello":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, helloJSON(teamServerID, "Lab relay"))
		case "/v1/seats":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"seats":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(blocked.Close)
	t.Cleanup(letGo)
	h, dir := testTeamSurface(t, clientFor(blocked))
	otherToken := strings.Repeat("cd", 32)
	seedTeam(t, dir, labEntry(teamServerID, blocked.URL, teamToken))
	seedTeam(t, dir, labEntry(teamOtherID, "http://other.example", otherToken))
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doTeam(h, http.MethodGet, "/api/team/servers/"+teamServerID+"/seats", "")
	}()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case <-entered:
	case <-timer.C:
		t.Fatal("seats did not reach the team server")
	}
	listed := doTeam(h, http.MethodGet, "/api/team/servers", "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), teamOtherID) {
		t.Fatalf("list = %d %s", listed.Code, listed.Body.String())
	}
	assertNoSecrets(t, listed.Body.String(), "", teamToken)
	assertNoSecrets(t, listed.Body.String(), "", otherToken)
	forgotten := doTeam(h, http.MethodDelete, "/api/team/servers/"+teamOtherID, "")
	if forgotten.Code != http.StatusNoContent {
		t.Fatalf("forget = %d %s", forgotten.Code, forgotten.Body.String())
	}
	letGo()
	rec := recvTeam(t, done, "seats")
	if rec.Code != http.StatusOK {
		t.Fatalf("seats = %d %s", rec.Code, rec.Body.String())
	}
	assertNoSecrets(t, rec.Body.String(), "", teamToken)
}

func mustSaveShare(t *testing.T, dir string, s team.Share) {
	t.Helper()
	if s.Members == nil {
		s.Members = []team.Member{}
	}
	if s.Title == "" {
		s.Title = "North"
	}
	if s.Generation == "" {
		s.Generation = shareGen
	}
	if s.SharedAt == "" {
		s.SharedAt = "2026-11-12T09:30:00Z"
	}
	if s.State == "" {
		s.State = "active"
	}
	if s.Scope == "" {
		s.Scope = "from_now"
	}
	if s.Server == "" {
		s.Server = teamServerID
	}
	if err := team.SaveShare(dir, s); err != nil {
		t.Fatalf("save share: %v", err)
	}
}

func seedChat(t *testing.T, dir, node, title, uid string, users int) {
	t.Helper()
	appendNode(t, dir, node, title)
	writeChatLog(t, dir, node, uid, users)
}

func longShareTitle() (raw, want string) {
	raw = "A\nB" + strings.Repeat("x", 198) + "é"
	want = "AB" + strings.Repeat("x", 198)
	return
}

func assertNoShareLeak(t *testing.T, body, dir string) {
	t.Helper()
	assertNoSecrets(t, body, "", teamToken)
	if strings.Contains(body, shareText) {
		t.Fatalf("response contains chat text: %s", body)
	}
	raw, err := os.ReadFile(filepath.Join(dir, team.SharesFile))
	if err != nil {
		return
	}
	text := string(raw)
	if strings.Contains(text, teamToken) || strings.Contains(text, shareText) {
		t.Fatalf("share file contains a secret or chat text: %s", text)
	}
}

func TestShareListAndGetStayLocal(t *testing.T) {
	script := &shareScript{}
	h, dir, calls, _ := readySurface(t, script)
	rec := doTeam(h, http.MethodGet, "/api/team/shares", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"shares":[]`) {
		t.Fatalf("empty = %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, team.SharesFile)); !os.IsNotExist(err) {
		t.Fatal("listing created the share file")
	}
	if len(snapshotCalls(calls)) != 0 {
		t.Fatalf("list called the server: %+v", snapshotCalls(calls))
	}
	missing := doTeam(h, http.MethodGet, "/api/team/shares/missing", "")
	assertAPIError(t, missing, http.StatusNotFound, "not_found")

	const (
		uidA    = "aaaaaaaaaaaaaaaa"
		uidB    = "bbbbbbbbbbbbbbbb"
		serverO = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		serverR = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		serverT = "0123456789abcdeffedcba9876543210"
	)
	mustSaveShare(t, dir, team.Share{UID: shareUID, Node: "m"})
	mustSaveShare(t, dir, team.Share{UID: shareUIDB, Node: "m", Server: serverR})
	mustSaveShare(t, dir, team.Share{UID: shareUIDC, Node: "m", Server: serverT})
	mustSaveShare(t, dir, team.Share{UID: uidB, Node: "b", Server: serverO, Scope: "all"})
	mustSaveShare(t, dir, team.Share{UID: uidA, Node: "a", Server: teamOtherID, Scope: "all", State: "stopping"})
	plain := doTeam(h, http.MethodGet, "/api/team/shares/a", "")
	if plain.Code != http.StatusOK {
		t.Fatalf("plain = %d %s", plain.Code, plain.Body.String())
	}
	if got := decodeShareView(t, plain); got.Delivery != "ok" || got.UID != uidA {
		t.Fatalf("plain view = %+v", got)
	}
	asTeamHandler(t, h).shareDelivery = func(id string) string {
		switch id {
		case teamServerID:
			return "offline"
		case teamOtherID:
			return "changed"
		case serverR:
			return "refused"
		case serverO:
			return "ok"
		default:
			return "nope"
		}
	}
	listed := doTeam(h, http.MethodGet, "/api/team/shares", "")
	if listed.Code != http.StatusOK {
		t.Fatalf("list = %d %s", listed.Code, listed.Body.String())
	}
	assertNoShareLeak(t, listed.Body.String(), dir)
	if len(snapshotCalls(calls)) != 0 {
		t.Fatal("list contacted the server")
	}
	views := decodeShareList(t, listed)
	if len(views) != 5 {
		t.Fatalf("views = %+v", views)
	}
	wantOrder := []struct{ node, uid, server, delivery string }{
		{"a", uidA, teamOtherID, "changed"},
		{"b", uidB, serverO, "ok"},
		{"m", shareUIDC, serverT, "ok"},
		{"m", shareUID, teamServerID, "offline"},
		{"m", shareUIDB, serverR, "refused"},
	}
	for i, want := range wantOrder {
		if views[i].Node != want.node || views[i].UID != want.uid || views[i].Server != want.server || views[i].Delivery != want.delivery {
			t.Fatalf("view %d = %+v, want %+v", i, views[i], want)
		}
		if views[i].Members == nil {
			t.Fatalf("view %d members is null", i)
		}
	}

	one := doTeam(h, http.MethodGet, "/api/team/shares/a", "")
	if one.Code != http.StatusOK {
		t.Fatalf("get = %d %s", one.Code, one.Body.String())
	}
	view := decodeShareView(t, one)
	if view.UID != uidA || view.State != "stopping" || view.Scope != "all" || view.Delivery != "changed" || view.SharedAt != "2026-11-12T09:30:00Z" {
		t.Fatalf("view = %+v", view)
	}

	// No log: several shares on one node resolve to the least UID.
	pile := doTeam(h, http.MethodGet, "/api/team/shares/m", "")
	if pile.Code != http.StatusOK {
		t.Fatalf("pile = %d %s", pile.Code, pile.Body.String())
	}
	if got := decodeShareView(t, pile); got.UID != shareUIDC {
		t.Fatalf("pile uid = %s", got.UID)
	}

	// A live log UID wins over the least UID.
	writeChatLog(t, dir, "m", shareUIDB, 0)
	live := doTeam(h, http.MethodGet, "/api/team/shares/m", "")
	if live.Code != http.StatusOK {
		t.Fatalf("live = %d %s", live.Code, live.Body.String())
	}
	if got := decodeShareView(t, live); got.UID != shareUIDB || got.Delivery != "refused" {
		t.Fatalf("live view = %+v", got)
	}

	damaged := []byte("{")
	if err := os.WriteFile(filepath.Join(dir, team.SharesFile), damaged, 0o600); err != nil {
		t.Fatal(err)
	}
	badList := doTeam(h, http.MethodGet, "/api/team/shares", "")
	assertAPIError(t, badList, http.StatusInternalServerError, "store_unavailable")
	badOne := doTeam(h, http.MethodGet, "/api/team/shares/m", "")
	assertAPIError(t, badOne, http.StatusInternalServerError, "store_unavailable")
	after, err := os.ReadFile(filepath.Join(dir, team.SharesFile))
	if err != nil || !bytes.Equal(damaged, after) {
		t.Fatalf("damaged file changed to %q", after)
	}
}

func TestStartSharingSendsHelloThenChannelThenMembers(t *testing.T) {
	rawTitle, wantTitle := longShareTitle()
	script := &shareScript{}
	h, dir, calls, inst := readySurface(t, script)
	script.channel = channelJSON(inst, shareUID, wantTitle, shareGen)
	appendStoreLine(t, dir, []byte("{"))
	appendStoreLine(t, dir, []byte(`{"type":"node"}`))
	appendNode(t, dir, "keep", "Keep")
	appendNode(t, dir, "side", "Side")
	appendDelete(t, dir, "side")
	appendNode(t, dir, shareNode, "old")
	appendDelete(t, dir, shareNode)
	appendNode(t, dir, shareNode, rawTitle)
	writeChatLog(t, dir, shareNode, shareUID, 1)
	logPath := chatLogPath(dir, shareNode)
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\nnot-json\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	logBefore, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	nodesBefore, err := os.ReadFile(filepath.Join(dir, "nodes.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	members := []team.Member{{Seat: shareSeat, Access: "read"}, {Seat: shareSeatB, Access: "write"}}
	rec := putShare(h, shareNode, sharePutBody(teamServerID, "from_now", members))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	assertNoShareLeak(t, rec.Body.String(), dir)
	view := decodeShareView(t, rec)
	if view.UID != shareUID || view.Node != shareNode || view.Server != teamServerID || view.Scope != "from_now" || view.State != "active" || view.Delivery != "ok" || view.SharedAt != "2026-11-12T09:30:00Z" {
		t.Fatalf("view = %+v", view)
	}
	assertMembersEqual(t, view.Members, members)
	stored, ok := storedShare(t, dir, shareUID)
	if !ok {
		t.Fatal("share was not stored")
	}
	if stored.Start != 2 || stored.Next != 2 || stored.StartSent || stored.Title != wantTitle || stored.Generation != shareGen || stored.State != "active" || stored.Scope != "from_now" || stored.Server != teamServerID || stored.Node != shareNode || stored.SharedAt != "2026-11-12T09:30:00Z" {
		t.Fatalf("stored = %+v", stored)
	}
	assertMembersEqual(t, stored.Members, members)
	info, err := os.Stat(filepath.Join(dir, team.SharesFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v err %v", info, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".team-") {
			t.Fatalf("temporary file %s", e.Name())
		}
	}
	logAfter, err := os.ReadFile(logPath)
	if err != nil || !bytes.Equal(logBefore, logAfter) {
		t.Fatal("session log changed")
	}
	nodesAfter, err := os.ReadFile(filepath.Join(dir, "nodes.jsonl"))
	if err != nil || !bytes.Equal(nodesBefore, nodesAfter) {
		t.Fatal("node store changed")
	}
	got := snapshotCalls(calls)
	base := channelBase(inst, shareUID)
	if len(got) != 3 || got[0].path != "/v1/hello" || got[1].path != base || got[2].path != base+"/members" {
		t.Fatalf("calls = %+v", got)
	}
	if got[0].method != http.MethodGet || got[1].method != http.MethodPut || got[2].method != http.MethodPut {
		t.Fatalf("methods = %+v", got)
	}
	assertBearer(t, got[0], false)
	assertBearer(t, got[1], true)
	assertBearer(t, got[2], true)
	var title struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal([]byte(got[1].body), &title); err != nil || title.Title != wantTitle {
		t.Fatalf("title body %s err %v", got[1].body, err)
	}
	var sent struct {
		Members []team.Member `json:"members"`
	}
	if err := json.Unmarshal([]byte(got[2].body), &sent); err != nil {
		t.Fatal(err)
	}
	assertMembersEqual(t, sent.Members, members)

	t.Run("scope all and an empty title", func(t *testing.T) {
		script := &shareScript{}
		h, dir, calls, inst := readySurface(t, script)
		script.channel = channelJSON(inst, shareUID, "North", shareGen)
		seedChat(t, dir, "plain", "\n", shareUID, 2)
		rec := putShare(h, "plain", sharePutBody(teamServerID, "all", []team.Member{}))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
		}
		view := decodeShareView(t, rec)
		if view.Scope != "all" || view.Delivery != "ok" {
			t.Fatalf("view = %+v", view)
		}
		if !strings.Contains(rec.Body.String(), `"members":[]`) {
			t.Fatalf("members = %s", rec.Body.String())
		}
		stored, ok := storedShare(t, dir, shareUID)
		if !ok || stored.Start != 0 || stored.Next != 0 || stored.Title != "" || stored.Scope != "all" || len(stored.Members) != 0 {
			t.Fatalf("stored = %+v ok %v", stored, ok)
		}
		got := snapshotCalls(calls)
		if len(got) != 3 || !strings.Contains(got[2].body, `"members":[]`) || strings.Contains(got[2].body, `"members":null`) {
			t.Fatalf("calls = %+v", got)
		}
	})

	t.Run("a title keeps brackets", func(t *testing.T) {
		script := &shareScript{}
		h, dir, calls, inst := readySurface(t, script)
		const title = `North <relay> "a"`
		script.channel = channelJSON(inst, shareUID, title, shareGen)
		seedChat(t, dir, shareNode, title, shareUID, 0)
		rec := putShare(h, shareNode, sharePutBody(teamServerID, "from_now", []team.Member{}))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
		}
		got := snapshotCalls(calls)
		if len(got) < 2 || !strings.Contains(got[1].body, `<relay>`) || strings.Contains(got[1].body, `\u003c`) {
			t.Fatalf("put body = %s", got)
		}
		stored, ok := storedShare(t, dir, shareUID)
		if !ok || stored.Title != title || stored.Start != 1 || stored.Next != 1 {
			t.Fatalf("stored = %+v", stored)
		}
	})
}

func TestStartSharingReplacesALeftoverChannel(t *testing.T) {
	t.Run("delete then create", func(t *testing.T) {
		script := &shareScript{putStatuses: []int{http.StatusOK, http.StatusCreated}}
		h, dir, calls, inst := readySurface(t, script)
		script.channel = channelJSON(inst, shareUID, "North", shareGen)
		seedChat(t, dir, shareNode, "North", shareUID, 0)
		rec := putShare(h, shareNode, sharePutBody(teamServerID, "all", []team.Member{}))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
		}
		base := channelBase(inst, shareUID)
		got := snapshotCalls(calls)
		want := []shareCall{
			{method: http.MethodGet, path: "/v1/hello"},
			{method: http.MethodPut, path: base},
			{method: http.MethodDelete, path: base},
			{method: http.MethodPut, path: base},
			{method: http.MethodPut, path: base + "/members"},
		}
		if len(got) != len(want) {
			t.Fatalf("calls = %+v", got)
		}
		for i := range want {
			if got[i].method != want[i].method || got[i].path != want[i].path {
				t.Fatalf("calls = %+v", got)
			}
			assertBearer(t, got[i], i != 0)
		}
		if _, ok := storedShare(t, dir, shareUID); !ok {
			t.Fatal("share missing")
		}
	})
	t.Run("still present", func(t *testing.T) {
		script := &shareScript{putStatuses: []int{http.StatusOK, http.StatusOK}}
		h, dir, calls, inst := readySurface(t, script)
		script.channel = channelJSON(inst, shareUID, "North", shareGen)
		seedChat(t, dir, shareNode, "North", shareUID, 0)
		rec := putShare(h, shareNode, sharePutBody(teamServerID, "all", []team.Member{}))
		assertAPIError(t, rec, http.StatusBadGateway, "bad_server")
		assertNoShareLeak(t, rec.Body.String(), dir)
		assertNoStoredShare(t, dir, shareUID)
		if countDeletes(snapshotCalls(calls)) != 2 {
			t.Fatalf("calls = %+v", snapshotCalls(calls))
		}
	})
	t.Run("delete failed", func(t *testing.T) {
		script := &shareScript{putStatuses: []int{http.StatusOK}, deleteStatus: http.StatusInternalServerError}
		h, dir, calls, inst := readySurface(t, script)
		script.channel = channelJSON(inst, shareUID, "North", shareGen)
		seedChat(t, dir, shareNode, "North", shareUID, 0)
		rec := putShare(h, shareNode, sharePutBody(teamServerID, "all", []team.Member{}))
		assertAPIError(t, rec, http.StatusBadGateway, "bad_server")
		assertNoStoredShare(t, dir, shareUID)
		got := snapshotCalls(calls)
		if len(got) != 3 || got[2].method != http.MethodDelete {
			t.Fatalf("calls = %+v", got)
		}
	})
	t.Run("second create fails", func(t *testing.T) {
		script := &shareScript{putStatuses: []int{http.StatusOK, http.StatusInternalServerError}}
		h, dir, calls, inst := readySurface(t, script)
		script.channel = channelJSON(inst, shareUID, "North", shareGen)
		seedChat(t, dir, shareNode, "North", shareUID, 0)
		rec := putShare(h, shareNode, sharePutBody(teamServerID, "all", []team.Member{}))
		assertAPIError(t, rec, http.StatusBadGateway, "bad_server")
		assertNoStoredShare(t, dir, shareUID)
		if countDeletes(snapshotCalls(calls)) != 2 {
			t.Fatalf("calls = %+v", snapshotCalls(calls))
		}
	})
}

func TestStartSharingFailureStoresNothing(t *testing.T) {
	members := []team.Member{{Seat: shareSeat, Access: "read"}}
	body := sharePutBody(teamServerID, "from_now", members)
	type built struct {
		h      http.Handler
		dir    string
		calls  *[]shareCall
		inst   string
		script *shareScript
	}
	setup := func(t *testing.T, script *shareScript) built {
		t.Helper()
		h, dir, calls, inst := readySurface(t, script)
		script.channel = channelJSON(inst, shareUID, "North", shareGen)
		seedChat(t, dir, shareNode, "North", shareUID, 0)
		return built{h, dir, calls, inst, script}
	}
	assertClean := func(t *testing.T, b built, rec *httptest.ResponseRecorder, status int, code string, deletes int) {
		t.Helper()
		assertAPIError(t, rec, status, code)
		assertNoShareLeak(t, rec.Body.String(), b.dir)
		assertNoStoredShare(t, b.dir, shareUID)
		got := snapshotCalls(b.calls)
		if len(got) == 0 || got[0].path != "/v1/hello" {
			t.Fatalf("calls = %+v", got)
		}
		assertBearer(t, got[0], false)
		if countDeletes(got) != deletes {
			t.Fatalf("deletes = %d calls %+v", countDeletes(got), got)
		}
		for _, c := range got {
			if strings.Contains(c.body, teamToken) {
				t.Fatalf("token in body %s", c.body)
			}
		}
	}
	t.Run("hello down", func(t *testing.T) {
		b := setup(t, &shareScript{failPath: "/v1/hello"})
		rec := putShare(b.h, shareNode, body)
		assertClean(t, b, rec, http.StatusBadGateway, "unreachable", 0)
	})
	t.Run("hello other server", func(t *testing.T) {
		b := setup(t, &shareScript{helloID: teamOtherID})
		rec := putShare(b.h, shareNode, body)
		assertClean(t, b, rec, http.StatusBadGateway, "bad_server", 0)
		for _, c := range snapshotCalls(b.calls) {
			assertBearer(t, c, false)
		}
	})
	t.Run("put refused", func(t *testing.T) {
		b := setup(t, &shareScript{putStatuses: []int{http.StatusUnauthorized}})
		rec := putShare(b.h, shareNode, body)
		assertClean(t, b, rec, http.StatusBadGateway, "refused", 0)
	})
	t.Run("put quota", func(t *testing.T) {
		b := setup(t, &shareScript{putStatuses: []int{http.StatusInsufficientStorage}})
		rec := putShare(b.h, shareNode, body)
		assertClean(t, b, rec, http.StatusInsufficientStorage, "server_full", 0)
	})
	t.Run("put unknown member", func(t *testing.T) {
		b := setup(t, &shareScript{putStatuses: []int{http.StatusUnprocessableEntity}})
		rec := putShare(b.h, shareNode, body)
		assertClean(t, b, rec, http.StatusUnprocessableEntity, "unknown_member", 0)
	})
	t.Run("put protocol", func(t *testing.T) {
		b := setup(t, &shareScript{putStatuses: []int{http.StatusInternalServerError}})
		rec := putShare(b.h, shareNode, body)
		assertClean(t, b, rec, http.StatusBadGateway, "bad_server", 0)
	})
	t.Run("members unknown", func(t *testing.T) {
		b := setup(t, &shareScript{memberStatus: http.StatusUnprocessableEntity})
		rec := putShare(b.h, shareNode, body)
		assertClean(t, b, rec, http.StatusUnprocessableEntity, "unknown_member", 1)
	})
	t.Run("members quota", func(t *testing.T) {
		b := setup(t, &shareScript{memberStatus: http.StatusInsufficientStorage})
		rec := putShare(b.h, shareNode, body)
		assertClean(t, b, rec, http.StatusInsufficientStorage, "server_full", 1)
	})
	t.Run("members refused", func(t *testing.T) {
		b := setup(t, &shareScript{memberStatus: http.StatusUnauthorized})
		rec := putShare(b.h, shareNode, body)
		assertClean(t, b, rec, http.StatusBadGateway, "refused", 1)
	})
	t.Run("members down", func(t *testing.T) {
		b := setup(t, &shareScript{})
		b.script.failPath = channelBase(b.inst, shareUID) + "/members"
		rec := putShare(b.h, shareNode, body)
		assertClean(t, b, rec, http.StatusBadGateway, "unreachable", 1)
	})
	t.Run("members protocol and delete fails", func(t *testing.T) {
		b := setup(t, &shareScript{memberStatus: http.StatusInternalServerError, deleteStatus: http.StatusInternalServerError})
		rec := putShare(b.h, shareNode, body)
		assertClean(t, b, rec, http.StatusBadGateway, "bad_server", 1)
	})
	t.Run("share list full", func(t *testing.T) {
		b := setup(t, &shareScript{})
		for i := 0; i < 256; i++ {
			mustSaveShare(t, b.dir, team.Share{UID: fmt.Sprintf("a%015x", i), Node: "parked"})
		}
		rec := putShare(b.h, shareNode, body)
		assertClean(t, b, rec, http.StatusInsufficientStorage, "too_many", 1)
		left, err := team.LoadShares(b.dir)
		if err != nil || len(left) != 256 {
			t.Fatalf("left = %d err %v", len(left), err)
		}
	})
	t.Run("share file unwritable", func(t *testing.T) {
		b := setup(t, &shareScript{})
		lockShareDir(t, b.dir)
		rec := putShare(b.h, shareNode, body)
		assertAPIError(t, rec, http.StatusInternalServerError, "store_unavailable")
		assertNoSecrets(t, rec.Body.String(), "", teamToken)
		if countDeletes(snapshotCalls(b.calls)) != 1 {
			t.Fatalf("calls = %+v", snapshotCalls(b.calls))
		}
		_ = os.Chmod(b.dir, 0o700)
		assertNoStoredShare(t, b.dir, shareUID)
	})
	t.Run("reused node keeps the old share", func(t *testing.T) {
		b := setup(t, &shareScript{})
		mustSaveShare(t, b.dir, team.Share{UID: shareUIDB, Node: shareNode, State: "stopping"})
		// The live log UID is shareUID, so this starts a second share.
		rec := putShare(b.h, shareNode, body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
		}
		old, ok := storedShare(t, b.dir, shareUIDB)
		if !ok || old.State != "stopping" {
			t.Fatalf("old = %+v ok %v", old, ok)
		}
		if _, ok := storedShare(t, b.dir, shareUID); !ok {
			t.Fatal("new share missing")
		}
	})
}

func TestSharePutRejectsABadBody(t *testing.T) {
	h, dir, calls, inst := readySurface(t, &shareScript{})
	seedChat(t, dir, shareNode, "North", shareUID, 0)
	valid := sharePutBody(teamServerID, "all", []team.Member{{Seat: shareSeat, Access: "read"}})
	var tooMany []team.Member
	for i := 0; len(tooMany) < 201; i++ {
		id := fmt.Sprintf("%032x", i+1)
		if id == inst {
			continue
		}
		tooMany = append(tooMany, team.Member{Seat: id, Access: "read"})
	}
	cases := []struct{ name, body string }{
		{"empty", ""},
		{"broken", "{"},
		{"trailing", valid + "{}"},
		{"unknown", strings.TrimSuffix(valid, "}") + `,"extra":1}`},
		{"scope", sharePutBody(teamServerID, "none", []team.Member{})},
		{"access", sharePutBody(teamServerID, "all", []team.Member{{Seat: shareSeat, Access: "owner"}})},
		{"seat", sharePutBody(teamServerID, "all", []team.Member{{Seat: "abc", Access: "read"}})},
		{"upper", sharePutBody(teamServerID, "all", []team.Member{{Seat: "00112233445566778899AABBCCDDEEFF", Access: "read"}})},
		{"twice", sharePutBody(teamServerID, "all", []team.Member{{Seat: shareSeat, Access: "read"}, {Seat: shareSeat, Access: "write"}})},
		{"self", sharePutBody(teamServerID, "all", []team.Member{{Seat: inst, Access: "read"}})},
		{"201", sharePutBody(teamServerID, "all", tooMany)},
		{"null members", `{"server":"` + teamServerID + `","scope":"all","members":null}`},
		{"missing members", `{"server":"` + teamServerID + `","scope":"all"}`},
		{"oversize", `{"server":"` + teamServerID + `","scope":"all","members":[],"pad":"` + strings.Repeat("a", 70*1024) + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(snapshotCalls(calls))
			rec := putShare(h, shareNode, tc.body)
			assertAPIError(t, rec, http.StatusBadRequest, "bad_request")
			assertNoSecrets(t, rec.Body.String(), "", teamToken)
			if len(snapshotCalls(calls)) != before {
				t.Fatalf("calls grew: %+v", snapshotCalls(calls))
			}
			assertNoStoredShare(t, dir, shareUID)
		})
	}
}

func TestSharePutRequiresANodeHistoryAndServer(t *testing.T) {
	body := sharePutBody(teamServerID, "all", []team.Member{})
	t.Run("unknown node", func(t *testing.T) {
		h, _, calls, _ := readySurface(t, &shareScript{})
		rec := putShare(h, shareNode, body)
		assertAPIError(t, rec, http.StatusNotFound, "not_found")
		if len(snapshotCalls(calls)) != 0 {
			t.Fatal("called the server")
		}
	})
	t.Run("deleted node", func(t *testing.T) {
		h, dir, calls, _ := readySurface(t, &shareScript{})
		seedChat(t, dir, shareNode, "North", shareUID, 0)
		appendDelete(t, dir, shareNode)
		rec := putShare(h, shareNode, body)
		assertAPIError(t, rec, http.StatusNotFound, "not_found")
		if len(snapshotCalls(calls)) != 0 {
			t.Fatal("called the server")
		}
	})
	t.Run("no log", func(t *testing.T) {
		h, dir, calls, _ := readySurface(t, &shareScript{})
		appendNode(t, dir, shareNode, "North")
		rec := putShare(h, shareNode, body)
		assertAPIError(t, rec, http.StatusConflict, "no_history")
		if len(snapshotCalls(calls)) != 0 {
			t.Fatal("called the server")
		}
	})
	t.Run("no uid", func(t *testing.T) {
		h, dir, calls, _ := readySurface(t, &shareScript{})
		appendNode(t, dir, shareNode, "North")
		writeUserOnlyLog(t, dir, shareNode)
		rec := putShare(h, shareNode, body)
		assertAPIError(t, rec, http.StatusConflict, "no_history")
		if len(snapshotCalls(calls)) != 0 {
			t.Fatal("called the server")
		}
	})
	t.Run("two uids", func(t *testing.T) {
		h, dir, calls, _ := readySurface(t, &shareScript{})
		appendNode(t, dir, shareNode, "North")
		writeChatLog(t, dir, shareNode, shareUID, 0)
		w := &sessionlog.Writer{Path: chatLogPath(dir, shareNode)}
		if err := w.Append(sessionlog.Event{
			T: "meta", Time: "2026-11-12T09:32:00Z",
			Meta: &sessionlog.MetaEvent{Node: shareNode, UID: shareUIDB, Agent: "claude", Created: "2026-11-12T09:32:00Z"},
		}); err != nil {
			t.Fatal(err)
		}
		rec := putShare(h, shareNode, body)
		assertAPIError(t, rec, http.StatusConflict, "no_history")
		if len(snapshotCalls(calls)) != 0 {
			t.Fatal("called the server")
		}
	})
	t.Run("unknown server", func(t *testing.T) {
		h, dir, calls, _ := readySurface(t, &shareScript{})
		seedChat(t, dir, shareNode, "North", shareUID, 0)
		rec := putShare(h, shareNode, sharePutBody(teamOtherID, "all", []team.Member{}))
		assertAPIError(t, rec, http.StatusNotFound, "not_found")
		if len(snapshotCalls(calls)) != 0 {
			t.Fatal("called the server")
		}
	})
	t.Run("damaged servers", func(t *testing.T) {
		h, dir, calls, _ := readySurface(t, &shareScript{})
		seedChat(t, dir, shareNode, "North", shareUID, 0)
		if err := os.WriteFile(filepath.Join(dir, team.File), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		rec := putShare(h, shareNode, body)
		assertAPIError(t, rec, http.StatusInternalServerError, "store_unavailable")
		if len(snapshotCalls(calls)) != 0 {
			t.Fatal("called the server")
		}
	})
	t.Run("damaged shares", func(t *testing.T) {
		h, dir, calls, _ := readySurface(t, &shareScript{})
		seedChat(t, dir, shareNode, "North", shareUID, 0)
		before := []byte("{")
		if err := os.WriteFile(filepath.Join(dir, team.SharesFile), before, 0o600); err != nil {
			t.Fatal(err)
		}
		rec := putShare(h, shareNode, body)
		assertAPIError(t, rec, http.StatusInternalServerError, "store_unavailable")
		after, err := os.ReadFile(filepath.Join(dir, team.SharesFile))
		if err != nil || !bytes.Equal(before, after) || len(snapshotCalls(calls)) != 0 {
			t.Fatalf("file %q calls %+v err %v", after, snapshotCalls(calls), err)
		}
	})
	t.Run("nodes unreadable", func(t *testing.T) {
		h, dir, calls, _ := readySurface(t, &shareScript{})
		if err := os.Mkdir(filepath.Join(dir, "nodes.jsonl"), 0o700); err != nil {
			t.Fatal(err)
		}
		rec := putShare(h, shareNode, body)
		assertAPIError(t, rec, http.StatusInternalServerError, "store_unavailable")
		if len(snapshotCalls(calls)) != 0 {
			t.Fatal("called the server")
		}
	})
	t.Run("identity", func(t *testing.T) {
		h, dir, calls, _ := readySurface(t, &shareScript{})
		seedChat(t, dir, shareNode, "North", shareUID, 0)
		if err := os.Remove(filepath.Join(dir, identity.InstallationFile)); err != nil {
			t.Fatal(err)
		}
		rec := putShare(h, shareNode, body)
		assertAPIError(t, rec, http.StatusInternalServerError, "identity_unavailable")
		if len(snapshotCalls(calls)) != 0 {
			t.Fatal("called the server")
		}
	})
	t.Run("canceled", func(t *testing.T) {
		h, dir, _, _ := readySurface(t, &shareScript{})
		seedChat(t, dir, shareNode, "North", shareUID, 0)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req := httptest.NewRequest(http.MethodPut, "http://127.0.0.1/api/team/shares/"+shareNode, strings.NewReader(body)).WithContext(ctx)
		req.Host = "127.0.0.1"
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assertAPIError(t, rec, http.StatusBadGateway, "unreachable")
		assertNoStoredShare(t, dir, shareUID)
	})
}

func TestShareUpdateReplacesMembersOnly(t *testing.T) {
	script := &shareScript{}
	h, dir, calls, inst := readySurface(t, script)
	script.channel = channelJSON(inst, shareUID, "North", shareGen)
	seedChat(t, dir, shareNode, "North", shareUID, 0)
	old := []team.Member{{Seat: shareSeat, Access: "read"}}
	mustSaveShare(t, dir, team.Share{
		UID: shareUID, Node: shareNode, Start: 4, Next: 7, StartSent: true,
		Title: "North", Generation: shareGen, Members: old,
	})
	next := []team.Member{{Seat: shareSeatB, Access: "write"}}
	rec := putShare(h, shareNode, sharePutBody(teamServerID, "from_now", next))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	assertNoShareLeak(t, rec.Body.String(), dir)
	view := decodeShareView(t, rec)
	if view.State != "active" || view.Delivery != "ok" || view.Scope != "from_now" || view.SharedAt != "2026-11-12T09:30:00Z" {
		t.Fatalf("view = %+v", view)
	}
	assertMembersEqual(t, view.Members, next)
	stored, ok := storedShare(t, dir, shareUID)
	if !ok || stored.Start != 4 || stored.Next != 7 || !stored.StartSent || stored.Title != "North" || stored.Generation != shareGen {
		t.Fatalf("stored = %+v", stored)
	}
	assertMembersEqual(t, stored.Members, next)
	got := snapshotCalls(calls)
	base := channelBase(inst, shareUID)
	if len(got) != 2 || got[0].path != "/v1/hello" || got[1].path != base+"/members" || got[1].method != http.MethodPut {
		t.Fatalf("calls = %+v", got)
	}
	assertBearer(t, got[0], false)
	assertBearer(t, got[1], true)

	t.Run("set members fails", func(t *testing.T) {
		script := &shareScript{memberStatus: http.StatusUnprocessableEntity}
		h, dir, calls, inst := readySurface(t, script)
		script.channel = channelJSON(inst, shareUID, "North", shareGen)
		seedChat(t, dir, shareNode, "North", shareUID, 0)
		mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, Members: old})
		rec := putShare(h, shareNode, sharePutBody(teamServerID, "from_now", next))
		assertAPIError(t, rec, http.StatusUnprocessableEntity, "unknown_member")
		stored, ok := storedShare(t, dir, shareUID)
		if !ok || stored.State != "active" {
			t.Fatalf("stored = %+v", stored)
		}
		assertMembersEqual(t, stored.Members, old)
		if countDeletes(snapshotCalls(calls)) != 0 {
			t.Fatalf("calls = %+v", snapshotCalls(calls))
		}
	})
	t.Run("hello names another server", func(t *testing.T) {
		script := &shareScript{helloID: teamOtherID}
		h, dir, calls, inst := readySurface(t, script)
		script.channel = channelJSON(inst, shareUID, "North", shareGen)
		seedChat(t, dir, shareNode, "North", shareUID, 0)
		mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, Members: old})
		rec := putShare(h, shareNode, sharePutBody(teamServerID, "from_now", next))
		assertAPIError(t, rec, http.StatusBadGateway, "bad_server")
		stored, ok := storedShare(t, dir, shareUID)
		if !ok || stored.State != "active" {
			t.Fatalf("stored = %+v", stored)
		}
		assertMembersEqual(t, stored.Members, old)
		got := snapshotCalls(calls)
		if len(got) != 1 || got[0].path != "/v1/hello" {
			t.Fatalf("calls = %+v", got)
		}
		assertBearer(t, got[0], false)
	})
	t.Run("save fails", func(t *testing.T) {
		script := &shareScript{}
		h, dir, calls, inst := readySurface(t, script)
		script.channel = channelJSON(inst, shareUID, "North", shareGen)
		seedChat(t, dir, shareNode, "North", shareUID, 0)
		mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, Members: old})
		lockShareDir(t, dir)
		rec := putShare(h, shareNode, sharePutBody(teamServerID, "from_now", next))
		assertAPIError(t, rec, http.StatusInternalServerError, "store_unavailable")
		if countDeletes(snapshotCalls(calls)) != 0 {
			t.Fatalf("calls = %+v", snapshotCalls(calls))
		}
		_ = os.Chmod(dir, 0o700)
		stored, ok := storedShare(t, dir, shareUID)
		if !ok {
			t.Fatal("share missing")
		}
		assertMembersEqual(t, stored.Members, old)
	})
}

func TestShareUpdateRejectsFixedOrInactive(t *testing.T) {
	script := &shareScript{}
	h, dir, calls, _ := readySurface(t, script)
	seedTeam(t, dir, labEntry(teamOtherID, "http://other.example", strings.Repeat("cd", 32)))
	seedChat(t, dir, shareNode, "North", shareUID, 0)
	mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, Scope: "from_now"})
	rec := putShare(h, shareNode, sharePutBody(teamOtherID, "from_now", []team.Member{}))
	assertAPIError(t, rec, http.StatusConflict, "share_fixed")
	rec = putShare(h, shareNode, sharePutBody(teamServerID, "all", []team.Member{}))
	assertAPIError(t, rec, http.StatusConflict, "share_fixed")
	if len(snapshotCalls(calls)) != 0 {
		t.Fatalf("calls = %+v", snapshotCalls(calls))
	}
	stored, ok := storedShare(t, dir, shareUID)
	if !ok || stored.Scope != "from_now" || stored.Server != teamServerID {
		t.Fatalf("stored = %+v", stored)
	}
	for _, state := range []string{"stopping", "lost", "full"} {
		t.Run(state, func(t *testing.T) {
			h, dir, calls, _ := readySurface(t, &shareScript{})
			seedChat(t, dir, shareNode, "North", shareUID, 0)
			mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, State: state})
			rec := putShare(h, shareNode, sharePutBody(teamServerID, "from_now", []team.Member{}))
			assertAPIError(t, rec, http.StatusConflict, "not_active")
			assertNoSecrets(t, rec.Body.String(), "", teamToken)
			if len(snapshotCalls(calls)) != 0 {
				t.Fatal("called the server")
			}
			got, ok := storedShare(t, dir, shareUID)
			if !ok || got.State != state {
				t.Fatalf("stored = %+v", got)
			}
		})
	}
}

func TestStopSharingRemovesTheShare(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"deleted", http.StatusNoContent},
		{"already gone", http.StatusNotFound},
		{"refused", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := &shareScript{deleteStatus: tc.status}
			h, dir, calls, inst := readySurface(t, script)
			seedChat(t, dir, shareNode, "North", shareUID, 0)
			mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, Members: []team.Member{{Seat: shareSeat, Access: "read"}}})
			rec := doTeam(h, http.MethodDelete, "/api/team/shares/"+shareNode, "")
			if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
				t.Fatalf("status = %d body %q", rec.Code, rec.Body.String())
			}
			assertNoStoredShare(t, dir, shareUID)
			got := snapshotCalls(calls)
			base := channelBase(inst, shareUID)
			if len(got) != 2 || got[0].path != "/v1/hello" || got[1].method != http.MethodDelete || got[1].path != base {
				t.Fatalf("calls = %+v", got)
			}
			assertBearer(t, got[0], false)
			assertBearer(t, got[1], true)
			if strings.Contains(rec.Body.String(), teamToken) {
				t.Fatal("token in the response")
			}
		})
	}
	t.Run("live chat wins when the node is shared twice", func(t *testing.T) {
		script := &shareScript{}
		h, dir, _, _ := readySurface(t, script)
		writeChatLog(t, dir, shareNode, shareUIDB, 0)
		appendNode(t, dir, shareNode, "North")
		mustSaveShare(t, dir, team.Share{UID: shareUIDC, Node: shareNode})
		mustSaveShare(t, dir, team.Share{UID: shareUIDB, Node: shareNode})
		rec := doTeam(h, http.MethodDelete, "/api/team/shares/"+shareNode, "")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
		}
		if _, ok := storedShare(t, dir, shareUIDB); ok {
			t.Fatal("live share remains")
		}
		if _, ok := storedShare(t, dir, shareUIDC); !ok {
			t.Fatal("other share was removed")
		}
	})
	t.Run("the only share stops after the node is reused", func(t *testing.T) {
		h, dir, _, _ := readySurface(t, &shareScript{})
		appendNode(t, dir, shareNode, "North")
		writeChatLog(t, dir, shareNode, shareUIDB, 0)
		mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode})
		rec := doTeam(h, http.MethodDelete, "/api/team/shares/"+shareNode, "")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
		}
		assertNoStoredShare(t, dir, shareUID)
	})
	t.Run("already removed", func(t *testing.T) {
		h, dir, _, _ := readySurface(t, &shareScript{})
		seedChat(t, dir, shareNode, "North", shareUID, 0)
		mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode})
		asTeamHandler(t, h).removeShare = func(string, string) error { return team.ErrNotFound }
		rec := doTeam(h, http.MethodDelete, "/api/team/shares/"+shareNode, "")
		if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
			t.Fatalf("status = %d %q", rec.Code, rec.Body.String())
		}
	})
	t.Run("remove fails", func(t *testing.T) {
		h, dir, _, _ := readySurface(t, &shareScript{})
		seedChat(t, dir, shareNode, "North", shareUID, 0)
		mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode})
		asTeamHandler(t, h).removeShare = func(string, string) error { return os.ErrPermission }
		rec := doTeam(h, http.MethodDelete, "/api/team/shares/"+shareNode, "")
		assertAPIError(t, rec, http.StatusInternalServerError, "store_unavailable")
		got, ok := storedShare(t, dir, shareUID)
		if !ok || got.State != "stopping" {
			t.Fatalf("stored = %+v ok %v", got, ok)
		}
	})
}

func TestStopSharingAnswersStoppingWhenTheServerStays(t *testing.T) {
	assertStopping := func(t *testing.T, rec *httptest.ResponseRecorder, dir string) {
		t.Helper()
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
		}
		assertNoShareLeak(t, rec.Body.String(), dir)
		view := decodeShareView(t, rec)
		if view.State != "stopping" || view.UID != shareUID || view.Delivery != "ok" || view.SharedAt != "2026-11-12T09:30:00Z" {
			t.Fatalf("view = %+v", view)
		}
		got, ok := storedShare(t, dir, shareUID)
		if !ok || got.State != "stopping" {
			t.Fatalf("stored = %+v ok %v", got, ok)
		}
	}
	t.Run("hello down", func(t *testing.T) {
		h, dir, calls, _ := readySurface(t, &shareScript{failPath: "/v1/hello"})
		mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode})
		rec := doTeam(h, http.MethodDelete, "/api/team/shares/"+shareNode, "")
		assertStopping(t, rec, dir)
		if len(snapshotCalls(calls)) != 1 {
			t.Fatalf("calls = %+v", snapshotCalls(calls))
		}
	})
	t.Run("hello other server", func(t *testing.T) {
		h, dir, calls, _ := readySurface(t, &shareScript{helloID: teamOtherID})
		mustSaveShare(t, dir, shareNodeShare())
		rec := doTeam(h, http.MethodDelete, "/api/team/shares/"+shareNode, "")
		assertStopping(t, rec, dir)
		got := snapshotCalls(calls)
		if len(got) != 1 {
			t.Fatalf("calls = %+v", got)
		}
		assertBearer(t, got[0], false)
	})
	t.Run("hello bad", func(t *testing.T) {
		h, dir, _, _ := readySurface(t, &shareScript{helloStatus: http.StatusInternalServerError})
		mustSaveShare(t, dir, shareNodeShare())
		rec := doTeam(h, http.MethodDelete, "/api/team/shares/"+shareNode, "")
		assertAPIError(t, rec, http.StatusBadGateway, "bad_server")
		got, ok := storedShare(t, dir, shareUID)
		if !ok || got.State != "stopping" {
			t.Fatalf("stored = %+v", got)
		}
	})
	t.Run("delete down", func(t *testing.T) {
		script := &shareScript{}
		h, dir, calls, inst := readySurface(t, script)
		script.failPath = channelBase(inst, shareUID)
		mustSaveShare(t, dir, shareNodeShare())
		rec := doTeam(h, http.MethodDelete, "/api/team/shares/"+shareNode, "")
		assertStopping(t, rec, dir)
		if countDeletes(snapshotCalls(calls)) != 0 || len(snapshotCalls(calls)) != 2 {
			t.Fatalf("calls = %+v", snapshotCalls(calls))
		}
	})
	t.Run("delete protocol", func(t *testing.T) {
		h, dir, _, _ := readySurface(t, &shareScript{deleteStatus: http.StatusInternalServerError})
		mustSaveShare(t, dir, shareNodeShare())
		rec := doTeam(h, http.MethodDelete, "/api/team/shares/"+shareNode, "")
		assertAPIError(t, rec, http.StatusBadGateway, "bad_server")
		got, ok := storedShare(t, dir, shareUID)
		if !ok || got.State != "stopping" {
			t.Fatalf("stored = %+v", got)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		h, dir, calls, _ := readySurface(t, &shareScript{})
		rec := doTeam(h, http.MethodDelete, "/api/team/shares/"+shareNode, "")
		assertAPIError(t, rec, http.StatusNotFound, "not_found")
		if len(snapshotCalls(calls)) != 0 {
			t.Fatal("called the server")
		}
		if _, err := os.Stat(filepath.Join(dir, team.SharesFile)); !os.IsNotExist(err) {
			t.Fatal("created a share file")
		}
	})
	t.Run("damaged", func(t *testing.T) {
		h, dir, calls, _ := readySurface(t, &shareScript{})
		before := []byte("{")
		if err := os.WriteFile(filepath.Join(dir, team.SharesFile), before, 0o600); err != nil {
			t.Fatal(err)
		}
		rec := doTeam(h, http.MethodDelete, "/api/team/shares/"+shareNode, "")
		assertAPIError(t, rec, http.StatusInternalServerError, "store_unavailable")
		after, err := os.ReadFile(filepath.Join(dir, team.SharesFile))
		if err != nil || !bytes.Equal(before, after) || len(snapshotCalls(calls)) != 0 {
			t.Fatalf("file %q calls %+v", after, snapshotCalls(calls))
		}
	})
	t.Run("cannot save stopping", func(t *testing.T) {
		h, dir, calls, _ := readySurface(t, &shareScript{})
		mustSaveShare(t, dir, shareNodeShare())
		lockShareDir(t, dir)
		rec := doTeam(h, http.MethodDelete, "/api/team/shares/"+shareNode, "")
		assertAPIError(t, rec, http.StatusInternalServerError, "store_unavailable")
		if len(snapshotCalls(calls)) != 0 {
			t.Fatal("called the server")
		}
		_ = os.Chmod(dir, 0o700)
		got, ok := storedShare(t, dir, shareUID)
		if !ok || got.State != "active" {
			t.Fatalf("stored = %+v", got)
		}
	})
	t.Run("server forgotten", func(t *testing.T) {
		h, dir, calls, _ := readySurface(t, &shareScript{})
		mustSaveShare(t, dir, shareNodeShare())
		if err := team.Remove(dir, teamServerID); err != nil {
			t.Fatal(err)
		}
		rec := doTeam(h, http.MethodDelete, "/api/team/shares/"+shareNode, "")
		if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
			t.Fatalf("status = %d %q", rec.Code, rec.Body.String())
		}
		assertNoStoredShare(t, dir, shareUID)
		if len(snapshotCalls(calls)) != 0 {
			t.Fatalf("calls = %+v", snapshotCalls(calls))
		}
	})
	t.Run("identity", func(t *testing.T) {
		h, dir, calls, _ := readySurface(t, &shareScript{})
		mustSaveShare(t, dir, shareNodeShare())
		if err := os.Remove(filepath.Join(dir, identity.InstallationFile)); err != nil {
			t.Fatal(err)
		}
		rec := doTeam(h, http.MethodDelete, "/api/team/shares/"+shareNode, "")
		assertAPIError(t, rec, http.StatusInternalServerError, "identity_unavailable")
		got, ok := storedShare(t, dir, shareUID)
		if !ok || got.State != "active" {
			t.Fatalf("stored = %+v ok %v", got, ok)
		}
		if len(snapshotCalls(calls)) != 0 {
			t.Fatal("called the server")
		}
	})
	t.Run("servers damaged", func(t *testing.T) {
		h, dir, calls, _ := readySurface(t, &shareScript{})
		mustSaveShare(t, dir, shareNodeShare())
		if err := os.WriteFile(filepath.Join(dir, team.File), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		rec := doTeam(h, http.MethodDelete, "/api/team/shares/"+shareNode, "")
		assertAPIError(t, rec, http.StatusInternalServerError, "store_unavailable")
		got, ok := storedShare(t, dir, shareUID)
		if !ok || got.State != "active" {
			t.Fatalf("stored = %+v ok %v", got, ok)
		}
		if len(snapshotCalls(calls)) != 0 {
			t.Fatal("called the server")
		}
	})
}

func shareNodeShare() team.Share {
	return team.Share{UID: shareUID, Node: shareNode}
}

func TestShareRoutesRejectOtherMethods(t *testing.T) {
	h, _, _, _ := readySurface(t, &shareScript{})
	cases := []struct{ method, path string }{
		{http.MethodPost, "/api/team/shares"},
		{http.MethodPut, "/api/team/shares"},
		{http.MethodDelete, "/api/team/shares"},
		{http.MethodPost, "/api/team/shares/" + shareNode},
		{http.MethodGet, "/api/team/shares/" + shareNode + "/extra"},
		{http.MethodGet, "/api/team/shares/"},
		{http.MethodOptions, "/api/team/shares/" + shareNode},
		{http.MethodPost, "/api/team/servers/" + teamServerID + "/seats"},
		{http.MethodPut, "/api/team/servers/" + teamServerID + "/seats"},
		{http.MethodDelete, "/api/team/servers/" + teamServerID + "/seats"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := doTeam(h, tc.method, tc.path, "")
			assertAPIError(t, rec, http.StatusNotFound, "not_found")
		})
	}
}

func TestSharePutDoesNotHoldTheFileLock(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && !strings.HasSuffix(r.URL.Path, "/members") {
			once.Do(func() { close(entered) })
			<-release
		}
		switch {
		case r.URL.Path == "/v1/hello":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, helloJSON(teamServerID, "Lab relay"))
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/members"):
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"owner":"`+teamServerID+`","chat":"`+shareUID+`","title":"North","generation":"`+shareGen+`","created_at":"2026-11-14T09:30:00.000Z","access":"owner","mirror_last":0,"comments_last":0,"members":[]}`)
		case r.Method == http.MethodPut:
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"owner":"`+teamServerID+`","chat":"`+shareUID+`","title":"North","generation":"`+shareGen+`","created_at":"2026-11-14T09:30:00.000Z","access":"owner","mirror_last":0,"comments_last":0,"members":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(blocked.Close)
	t.Cleanup(letGo)
	h, dir := testTeamSurface(t, clientFor(blocked))
	seedTeam(t, dir, labEntry(teamServerID, blocked.URL, teamToken))
	seedChat(t, dir, shareNode, "North", shareUID, 0)
	mustSaveShare(t, dir, team.Share{UID: shareUIDB, Node: "other"})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- putShare(h, shareNode, sharePutBody(teamServerID, "all", []team.Member{}))
	}()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case <-entered:
	case <-timer.C:
		t.Fatal("put did not reach the team server")
	}
	listed := doTeam(h, http.MethodGet, "/api/team/shares", "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), shareUIDB) {
		t.Fatalf("list = %d %s", listed.Code, listed.Body.String())
	}
	one := doTeam(h, http.MethodGet, "/api/team/shares/other", "")
	if one.Code != http.StatusOK {
		t.Fatalf("get = %d %s", one.Code, one.Body.String())
	}
	letGo()
	rec := recvTeam(t, done, "put")
	if rec.Code != http.StatusCreated {
		t.Fatalf("put = %d %s", rec.Code, rec.Body.String())
	}
	assertNoSecrets(t, rec.Body.String(), "", teamToken)
}

func TestShareRoutesMatchTheSettingsBoundary(t *testing.T) {
	script := &shareScript{}
	srv, _ := recordShareServer(t, script)
	orig := newTeamClient
	t.Cleanup(func() { newTeamClient = orig })
	newTeamClient = func() *team.Client {
		c := orig()
		c.Transport = srv.Client().Transport
		return c
	}
	cmd, data, _, _ := startTeamCommand(t, true, true)
	if cmd.application == nil || cmd.application.teamHandler == nil {
		t.Fatal("muxer did not install the team handler")
	}
	core, err := newCoreMux(cmd.application)
	if err != nil {
		t.Fatal(err)
	}
	web, err := newWebBackend(webBackendConfig{Web: webFS, Core: core, RequestPolicy: cmd.application.requestPolicy})
	if err != nil {
		t.Fatal(err)
	}
	compare := func(method, teamPath, settingsPath, header string) {
		t.Helper()
		teamReq := httptest.NewRequest(method, "http://127.0.0.1"+teamPath, nil)
		settingsReq := httptest.NewRequest(method, "http://127.0.0.1"+settingsPath, nil)
		for _, req := range []*http.Request{teamReq, settingsReq} {
			req.Host = "127.0.0.1"
			if header != "" {
				req.Header.Set("Sec-Fetch-Site", header)
			}
		}
		teamRec := httptest.NewRecorder()
		settingsRec := httptest.NewRecorder()
		web.local.ServeHTTP(teamRec, teamReq)
		web.local.ServeHTTP(settingsRec, settingsReq)
		if dumpRecorder(teamRec) != dumpRecorder(settingsRec) {
			t.Fatalf("%s %s\n%s\nsettings\n%s", method, teamPath, dumpRecorder(teamRec), dumpRecorder(settingsRec))
		}
	}
	compare(http.MethodPut, "/api/team/shares/"+shareNode, "/api/settings", "")
	compare(http.MethodDelete, "/api/team/shares/"+shareNode, "/api/settings", "")
	compare(http.MethodGet, "/api/team/shares", "/api/settings", "cross-site")
	compare(http.MethodGet, "/api/team/servers/"+teamServerID+"/seats", "/api/settings", "cross-site")

	inst := installationOf(t, data)
	script.channel = channelJSON(inst, shareUID, "North", shareGen)
	seedTeam(t, data, labEntry(teamServerID, srv.URL, teamToken))
	seedChat(t, data, shareNode, "North", shareUID, 0)
	body := sharePutBody(teamServerID, "all", []team.Member{})
	req := httptest.NewRequest(http.MethodPut, "http://127.0.0.1/api/team/shares/"+shareNode, strings.NewReader(body))
	req.Host = "127.0.0.1"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Scimux-CSRF", csrfToken)
	rec := httptest.NewRecorder()
	web.local.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("put behind the boundary = %d %s", rec.Code, rec.Body.String())
	}
	assertNoSecrets(t, rec.Body.String(), "", teamToken)

	del := httptest.NewRequest(http.MethodDelete, "http://127.0.0.1/api/team/shares/"+shareNode, nil)
	del.Host = "127.0.0.1"
	del.Header.Set("X-Scimux-CSRF", csrfToken)
	delRec := httptest.NewRecorder()
	web.local.ServeHTTP(delRec, del)
	if delRec.Code != http.StatusNoContent || delRec.Body.Len() != 0 {
		t.Fatalf("delete behind the boundary = %d %q", delRec.Code, delRec.Body.String())
	}
	assertNoSecrets(t, delRec.Body.String(), "", teamToken)
}
