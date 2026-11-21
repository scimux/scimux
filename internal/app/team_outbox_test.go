package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/identity"
	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/team"
)

func TestInstalledShareOutboxOwnerInstanceTracksLiveNode(t *testing.T) {
	a := &app{byID: map[string]*Node{}}
	installShareOutbox(a, t.TempDir(), time.Now)
	h := a.teamHandler.(*teamHandler)
	if h.ownerInstance("local", "one") {
		t.Fatal("missing node authorized")
	}
	a.byID["local"] = &Node{ID: "local"}
	if h.ownerInstance("local", "") || h.ownerInstance("local", "one") {
		t.Fatal("node without an instance authorized")
	}
	a.byID["local"].InstanceID = "one"
	if !h.ownerInstance("local", "one") || h.ownerInstance("local", "two") {
		t.Fatal("current and wrong instance were not distinguished")
	}
	a.byID["local"].EndedAt = "2026-01-01T00:00:00Z"
	if h.ownerInstance("local", "one") {
		t.Fatal("ended node authorized")
	}
	a.byID["local"] = &Node{ID: "local", InstanceID: "two"}
	if h.ownerInstance("local", "one") || !h.ownerInstance("local", "two") {
		t.Fatal("replacement instance was not fenced")
	}
}

type outboxScript struct {
	helloStatus   int
	helloID       string
	putStatus     int
	deleteStatus  int
	deleteQueries []string
	mirrorStatus  int
	mirrorGen     string
	channel       string
	failPath      string
	owner         string
	mu            sync.Mutex
	// mirrorArrived is closed when a mirror call is inside the server.
	// mirrorGate holds that call until it is closed.
	mirrorArrived chan struct{}
	mirrorGate    chan struct{}
	mirrorOnce    sync.Once
	// kept is every mirror body this server stored.
	kept []string
}

func (s *outboxScript) set(fn func(*outboxScript)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s)
}

func (s *outboxScript) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	failPath := s.failPath
	helloStatus := s.helloStatus
	helloID := s.helloID
	putStatus := s.putStatus
	deleteStatus := s.deleteStatus
	mirrorStatus := s.mirrorStatus
	mirrorGen := s.mirrorGen
	channel := s.channel
	s.mu.Unlock()
	if failPath != "" && r.URL.Path == failPath {
		hj, ok := w.(http.Hijacker)
		if !ok {
			panic("outbox script: no hijack")
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
		status := helloStatus
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		if status == http.StatusOK {
			id := helloID
			if id == "" {
				id = teamServerID
			}
			_, _ = io.WriteString(w, helloJSON(id, "Lab relay"))
		} else {
			_, _ = io.WriteString(w, `{"error":"nope"}`)
		}
	case r.Method == http.MethodDelete:
		s.mu.Lock()
		s.deleteQueries = append(s.deleteQueries, r.URL.Query().Get("generation"))
		s.mu.Unlock()
		status := deleteStatus
		if status == 0 {
			status = http.StatusNoContent
		}
		w.WriteHeader(status)
	case r.Method == http.MethodPut:
		status := putStatus
		if strings.HasSuffix(r.URL.Path, "/members") && (status == 0 || status == http.StatusCreated) {
			status = http.StatusOK
		} else if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		if (status == http.StatusOK || status == http.StatusCreated) && channel != "" {
			_, _ = io.WriteString(w, channel)
		}
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/mirror"):
		status := mirrorStatus
		if status == 0 {
			status = http.StatusOK
		}
		raw, _ := io.ReadAll(r.Body)
		s.holdMirror()
		var req struct {
			Generation string `json:"generation"`
			Entries    []struct {
				Key string `json:"key"`
			} `json:"entries"`
		}
		_ = json.Unmarshal(raw, &req)
		if req.Generation != channelGeneration(channel) {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":"stale_generation"}`)
			return
		}
		w.WriteHeader(status)
		if status == http.StatusOK {
			s.noteKept(string(raw))
		}
		if status != http.StatusOK {
			return
		}
		gen := mirrorGen
		if gen == "" {
			gen = shareGen
		}
		results := make([]map[string]any, len(req.Entries))
		for i, e := range req.Entries {
			results[i] = map[string]any{"key": e.Key, "seq": i + 1, "duplicate": false}
		}
		enc := json.NewEncoder(w)
		_ = enc.Encode(struct {
			Generation string           `json:"generation"`
			Results    []map[string]any `json:"results"`
			Last       int64            `json:"last"`
		}{Generation: gen, Results: results, Last: int64(len(req.Entries))})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func channelGeneration(raw string) string {
	var doc struct {
		Generation string `json:"generation"`
	}
	if json.Unmarshal([]byte(raw), &doc) != nil || doc.Generation == "" {
		return shareGen
	}
	return doc.Generation
}

func (s *outboxScript) noteKept(raw string) {
	s.mu.Lock()
	s.kept = append(s.kept, raw)
	s.mu.Unlock()
}

func (s *outboxScript) keptMirrors() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.kept))
	copy(out, s.kept)
	return out
}

func recordOutbox(t *testing.T, script *outboxScript) (*httptest.Server, *[]shareCall) {
	t.Helper()
	mu := &sync.Mutex{}
	var calls []shareCall
	shareCallMus.Store(&calls, mu)
	t.Cleanup(func() { shareCallMus.Delete(&calls) })
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

func newShareBox(t *testing.T, script *outboxScript) (*shareOutbox, string, *[]shareCall, string) {
	t.Helper()
	srv, calls := recordOutbox(t, script)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "nodes.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := identity.LoadOrCreateInstallation(dir); err != nil {
		t.Fatal(err)
	}
	inst := installationOf(t, dir)
	script.owner = inst
	script.channel = channelJSON(inst, shareUID, "North", shareGen)
	seedTeam(t, dir, labEntry(teamServerID, srv.URL, teamToken))
	when := time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)
	box := &shareOutbox{
		dataDir: dir,
		now:     func() time.Time { return when },
		client:  clientFor(srv),
	}
	return box, dir, calls, inst
}

func appendOutbox(t *testing.T, dir string, ev sessionlog.Event) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	w := &sessionlog.Writer{Path: chatLogPath(dir, shareNode)}
	if err := w.Append(ev); err != nil {
		t.Fatal(err)
	}
}

func assertMirrorEqual(t *testing.T, body, want string) {
	t.Helper()
	var got, exp any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	if err := json.Unmarshal([]byte(want), &exp); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, exp) {
		t.Fatalf("mirror %s\nwant %s", body, want)
	}
}

func mirrorCall(t *testing.T, calls []shareCall) shareCall {
	t.Helper()
	for _, c := range calls {
		if strings.HasSuffix(c.path, "/mirror") {
			return c
		}
	}
	t.Fatalf("no mirror in %+v", calls)
	return shareCall{}
}

func TestOutboxCopiesTheFilteredLog(t *testing.T) {
	script := &outboxScript{}
	box, dir, calls, _ := newShareBox(t, script)
	appendNode(t, dir, shareNode, "North")
	events := []sessionlog.Event{
		{T: "meta", Time: "2026-11-12T09:30:00Z", Meta: &sessionlog.MetaEvent{Node: shareNode, UID: shareUID, Agent: "claude", Created: "2026-11-12T09:30:00Z"}},
		{T: "user", Time: "2026-11-12T09:31:00Z", Text: "one"},
		{T: "assistant", Time: "2026-11-12T09:32:00Z", Text: "two"},
		{T: "tool", Time: "2026-11-12T09:33:00Z"},
		{T: "usage", Time: "2026-11-12T09:34:00Z"},
		{T: "decision", Time: "2026-11-12T09:35:00Z"},
		{T: "mark", Time: "2026-11-12T09:36:00Z"},
		{T: "user", Time: "2026-11-12T09:37:00Z", Text: "  "},
		{T: "source", Time: "2026-11-12T09:38:00Z", Source: &sessionlog.SourceEvent{Reason: "clear"}},
		{T: "user", Time: "2026-11-12T09:39:00Z", Text: "three"},
		{T: "source", Time: "2026-11-12T09:40:00Z", Source: &sessionlog.SourceEvent{}},
		{T: "meta", Time: "2026-11-12T09:41:00Z", Meta: &sessionlog.MetaEvent{Node: shareNode, UID: shareUID, Agent: "codex", Created: "2026-11-12T09:41:00Z"}},
		{T: "assistant", Time: "2026-11-12T09:42:00Z", Text: "four", Prov: json.RawMessage(`{"k":1}`)},
	}
	for _, ev := range events {
		appendOutbox(t, dir, ev)
	}
	logBefore, err := os.ReadFile(chatLogPath(dir, shareNode))
	if err != nil {
		t.Fatal(err)
	}
	nodesBefore, err := os.ReadFile(filepath.Join(dir, "nodes.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, Scope: "all", Title: "North"})
	box.tick(context.Background())
	got := snapshotCalls(calls)
	if len(got) != 2 || got[0].path != "/v1/hello" || !strings.HasSuffix(got[1].path, "/mirror") {
		t.Fatalf("calls = %+v", got)
	}
	assertBearer(t, got[0], false)
	assertBearer(t, got[1], true)
	if strings.Contains(got[1].body, teamToken) || strings.Contains(got[1].body, "synthetic") {
		t.Fatalf("mirror leaked %s", got[1].body)
	}
	var req struct {
		Entries []struct {
			Key  string          `json:"key"`
			Body json.RawMessage `json:"body"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(got[1].body), &req); err != nil {
		t.Fatal(err)
	}
	want := []struct{ key, body string }{
		{"start", `{"v":1,"t":"start","scope":"all","time":"2026-11-12T09:30:00Z"}`},
		{"0/1", `{"v":1,"t":"user","time":"2026-11-12T09:31:00Z","text":"one"}`},
		{"0/2", `{"v":1,"t":"assistant","time":"2026-11-12T09:32:00Z","text":"two","agent":"claude"}`},
		{"0/8", `{"v":1,"t":"source","time":"2026-11-12T09:38:00Z","reason":"clear"}`},
		{"1/9", `{"v":1,"t":"user","time":"2026-11-12T09:39:00Z","text":"three"}`},
		{"1/10", `{"v":1,"t":"source","time":"2026-11-12T09:40:00Z"}`},
		{"2/12", `{"v":1,"t":"assistant","time":"2026-11-12T09:42:00Z","text":"four","agent":"codex","prov":true}`},
	}
	if len(req.Entries) != len(want) {
		t.Fatalf("entries = %+v", req.Entries)
	}
	for i, w := range want {
		if req.Entries[i].Key != w.key {
			t.Fatalf("key %d = %s", i, req.Entries[i].Key)
		}
		assertMirrorEqual(t, string(req.Entries[i].Body), w.body)
	}
	stored, ok := storedShare(t, dir, shareUID)
	if !ok || !stored.StartSent || stored.Next != 13 || stored.Generation != shareGen || stored.State != "active" || stored.Title != "North" {
		t.Fatalf("stored = %+v", stored)
	}
	logAfter, err := os.ReadFile(chatLogPath(dir, shareNode))
	if err != nil || !bytes.Equal(logBefore, logAfter) {
		t.Fatal("session log changed")
	}
	nodesAfter, err := os.ReadFile(filepath.Join(dir, "nodes.jsonl"))
	if err != nil || !bytes.Equal(nodesBefore, nodesAfter) {
		t.Fatal("node store changed")
	}
	raw, err := os.ReadFile(filepath.Join(dir, team.SharesFile))
	if err != nil || strings.Contains(string(raw), teamToken) || strings.Contains(string(raw), "one") {
		t.Fatalf("share file = %s", raw)
	}

	// A new outbox is a restart. The start entry is not sent again.
	again, calls2 := restartedBox(t, dir, script)
	again.tick(context.Background())
	if len(snapshotCalls(calls2)) != 0 {
		t.Fatalf("restart calls = %+v", snapshotCalls(calls2))
	}
}

// The owner's local attachment paths stay on the owner's computer: the relay
// receives only the file names, while the local session log keeps the full
// delivered text.
func TestOutboxSharesAttachmentNamesNotPaths(t *testing.T) {
	script := &outboxScript{}
	box, dir, calls, _ := newShareBox(t, script)
	appendNode(t, dir, shareNode, "North")
	local := filepath.Join(dir, "attachments", shareNode)
	userText := "compare these\n\n[attached file: " + local + "/0a1b2c3d-notes.txt]\n[attached image: " + local + "/deadbeef-plot.png]"
	agentText := "I read [attached file: " + local + "/0a1b2c3d-notes.txt]"
	for _, ev := range []sessionlog.Event{
		{T: "meta", Time: "2026-11-12T09:30:00Z", Meta: &sessionlog.MetaEvent{Node: shareNode, UID: shareUID, Agent: "codex", Created: "2026-11-12T09:30:00Z"}},
		{T: "user", Time: "2026-11-12T09:31:00Z", Text: userText},
		{T: "assistant", Time: "2026-11-12T09:32:00Z", Text: agentText},
	} {
		appendOutbox(t, dir, ev)
	}
	mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, Scope: "all", Title: "North"})
	box.tick(context.Background())
	sent := mirrorCall(t, snapshotCalls(calls))
	var req struct {
		Entries []struct {
			Key  string          `json:"key"`
			Body json.RawMessage `json:"body"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(sent.body), &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Entries) != 3 {
		t.Fatalf("entries = %+v", req.Entries)
	}
	assertMirrorEqual(t, string(req.Entries[1].Body), `{"v":1,"t":"user","time":"2026-11-12T09:31:00Z","text":"compare these\n\n[attached file: notes.txt]\n[attached image: plot.png]"}`)
	// Agent output is shared as the agent wrote it.
	var agent struct{ Text string }
	if err := json.Unmarshal(req.Entries[2].Body, &agent); err != nil || agent.Text != agentText {
		t.Fatalf("assistant body = %s", req.Entries[2].Body)
	}
	if strings.Contains(string(req.Entries[1].Body), dir) {
		t.Fatalf("user entry leaked a local path: %s", req.Entries[1].Body)
	}
	logged, err := os.ReadFile(chatLogPath(dir, shareNode))
	if err != nil || !strings.Contains(string(logged), local+"/0a1b2c3d-notes.txt") {
		t.Fatal("local session log lost the delivered attachment path")
	}
}

func restartedBox(t *testing.T, dir string, script *outboxScript) (*shareOutbox, *[]shareCall) {
	t.Helper()
	srv, calls := recordOutbox(t, script)
	entries, err := team.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	entries[0].URL = srv.URL
	if err := os.Remove(filepath.Join(dir, team.File)); err != nil {
		t.Fatal(err)
	}
	seedTeam(t, dir, entries[0])
	when := time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)
	return &shareOutbox{
		dataDir: dir,
		now:     func() time.Time { return when },
		client:  clientFor(srv),
	}, calls
}

func TestOutboxScopeSkipsEarlierRecords(t *testing.T) {
	script := &outboxScript{}
	box, dir, calls, _ := newShareBox(t, script)
	appendNode(t, dir, shareNode, "North")
	appendOutbox(t, dir, sessionlog.Event{T: "meta", Time: "2026-11-12T09:30:00Z", Meta: &sessionlog.MetaEvent{Node: shareNode, UID: shareUID, Agent: "claude", Created: "2026-11-12T09:30:00Z"}})
	appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:31:00Z", Text: "early"})
	appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:32:00Z", Text: "later"})
	mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, Scope: "from_now", Start: 2, Next: 2, Title: "North"})
	box.tick(context.Background())
	body := mirrorCall(t, snapshotCalls(calls)).body
	if strings.Contains(body, "early") || !strings.Contains(body, "later") {
		t.Fatalf("body = %s", body)
	}
	if !strings.Contains(body, `"scope":"from_now"`) {
		t.Fatalf("start = %s", body)
	}
	stored, ok := storedShare(t, dir, shareUID)
	if !ok || stored.Next != 3 || !stored.StartSent {
		t.Fatalf("stored = %+v", stored)
	}
}

func TestOutboxBatchesEntries(t *testing.T) {
	script := &outboxScript{}
	box, dir, calls, _ := newShareBox(t, script)
	appendNode(t, dir, shareNode, "North")
	appendOutbox(t, dir, sessionlog.Event{T: "meta", Time: "2026-11-12T09:30:00Z", Meta: &sessionlog.MetaEvent{Node: shareNode, UID: shareUID, Agent: "claude", Created: "2026-11-12T09:30:00Z"}})
	for i := 0; i < 65; i++ {
		appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:31:00Z", Text: "n"})
	}
	mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, StartSent: true, Title: "North"})
	box.tick(context.Background())
	first := mirrorCall(t, snapshotCalls(calls))
	var req struct {
		Entries []struct {
			Key string `json:"key"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(first.body), &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Entries) != 64 || req.Entries[0].Key != "0/1" || req.Entries[63].Key != "0/64" {
		t.Fatalf("first = %+v", req.Entries)
	}
	stored, ok := storedShare(t, dir, shareUID)
	if !ok || stored.Next != 65 {
		t.Fatalf("stored = %+v", stored)
	}
	// The server was named in this process, so the second tick sends no Hello.
	box.tick(context.Background())
	got := snapshotCalls(calls)
	mirrors := 0
	hellos := 0
	for _, c := range got {
		if c.path == "/v1/hello" {
			hellos++
		}
		if strings.HasSuffix(c.path, "/mirror") {
			mirrors++
		}
	}
	if hellos != 1 || mirrors != 2 {
		t.Fatalf("calls = %+v", got)
	}
	stored, ok = storedShare(t, dir, shareUID)
	if !ok || stored.Next != 66 {
		t.Fatalf("stored = %+v", stored)
	}
}

func TestOutboxLimitsTheBatchBytes(t *testing.T) {
	script := &outboxScript{}
	box, dir, calls, _ := newShareBox(t, script)
	appendNode(t, dir, shareNode, "North")
	appendOutbox(t, dir, sessionlog.Event{T: "meta", Time: "2026-11-12T09:30:00Z", Meta: &sessionlog.MetaEvent{Node: shareNode, UID: shareUID, Agent: "claude", Created: "2026-11-12T09:30:00Z"}})
	text := strings.Repeat("x", 900000)
	for i := 0; i < 3; i++ {
		appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:31:00Z", Text: text})
	}
	mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, StartSent: true, Title: "North"})
	box.tick(context.Background())
	var req struct {
		Entries []struct {
			Key string `json:"key"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(mirrorCall(t, snapshotCalls(calls)).body), &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Entries) != 2 {
		t.Fatalf("entries = %d", len(req.Entries))
	}
	box.tick(context.Background())
	n := 0
	for _, c := range snapshotCalls(calls) {
		if strings.HasSuffix(c.path, "/mirror") {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("mirrors = %d", n)
	}
}

func TestOutboxOmitsAHugeRecord(t *testing.T) {
	script := &outboxScript{}
	box, dir, calls, _ := newShareBox(t, script)
	appendNode(t, dir, shareNode, "North")
	appendOutbox(t, dir, sessionlog.Event{T: "meta", Time: "2026-11-12T09:30:00Z", Meta: &sessionlog.MetaEvent{Node: shareNode, UID: shareUID, Agent: "claude", Created: "2026-11-12T09:30:00Z"}})
	huge := strings.Repeat("q", 1<<20+1)
	appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:31:00Z", Text: huge})
	mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, StartSent: true, Title: "North"})
	box.tick(context.Background())
	body := mirrorCall(t, snapshotCalls(calls)).body
	if strings.Contains(body, huge[:64]) || !strings.Contains(body, `"omitted":"too_large"`) || strings.Contains(body, `"text"`) {
		t.Fatalf("body leaked or omitted nothing")
	}
}

func TestOutboxFailedAppendSavesNothing(t *testing.T) {
	script := &outboxScript{mirrorStatus: http.StatusInternalServerError}
	box, dir, calls, _ := newShareBox(t, script)
	appendNode(t, dir, shareNode, "North")
	seedOneUser(t, dir)
	mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, Title: "North"})
	box.tick(context.Background())
	stored, ok := storedShare(t, dir, shareUID)
	if !ok || stored.Next != 0 || stored.StartSent || stored.State != "active" {
		t.Fatalf("stored = %+v", stored)
	}
	if box.delivery(teamServerID) != "changed" {
		t.Fatalf("delivery = %s", box.delivery(teamServerID))
	}
	first := mirrorCall(t, snapshotCalls(calls)).body
	box.tick(context.Background())
	if len(snapshotCalls(calls)) != 2 {
		t.Fatalf("backoff still called %+v", snapshotCalls(calls))
	}
	when := time.Date(2026, 11, 12, 9, 30, 4, 0, time.UTC)
	box.now = func() time.Time { return when }
	script.set(func(s *outboxScript) { s.mirrorStatus = http.StatusOK })
	box.tick(context.Background())
	got := snapshotCalls(calls)
	if len(got) < 4 || got[2].path != "/v1/hello" {
		t.Fatalf("calls = %+v", got)
	}
	assertBearer(t, got[2], false)
	second := mirrorCall(t, got[2:]).body
	if first != second {
		t.Fatalf("first %s\nsecond %s", first, second)
	}
}

func seedOneUser(t *testing.T, dir string) {
	t.Helper()
	appendOutbox(t, dir, sessionlog.Event{T: "meta", Time: "2026-11-12T09:30:00Z", Meta: &sessionlog.MetaEvent{Node: shareNode, UID: shareUID, Agent: "claude", Created: "2026-11-12T09:30:00Z"}})
	appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:31:00Z", Text: "one"})
}

func TestOutboxCrashDropsOneSave(t *testing.T) {
	script := &outboxScript{}
	box, dir, calls, _ := newShareBox(t, script)
	appendNode(t, dir, shareNode, "North")
	seedOneUser(t, dir)
	mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, Title: "North"})
	drops := 0
	box.save = func(dir string, s team.Share) error {
		drops++
		if drops == 1 {
			return nil
		}
		return team.SaveShare(dir, s)
	}
	box.tick(context.Background())
	stored, ok := storedShare(t, dir, shareUID)
	if !ok || stored.Next != 0 || stored.StartSent {
		t.Fatalf("stored = %+v", stored)
	}
	box.tick(context.Background())
	got := snapshotCalls(calls)
	mirrors := 0
	for _, c := range got {
		if strings.HasSuffix(c.path, "/mirror") {
			mirrors++
		}
	}
	if mirrors != 2 {
		t.Fatalf("calls = %+v", got)
	}
	stored, ok = storedShare(t, dir, shareUID)
	if !ok || stored.Next != 2 || !stored.StartSent {
		t.Fatalf("stored = %+v", stored)
	}
	box.tick(context.Background())
	if len(snapshotCalls(calls)) != len(got) {
		t.Fatalf("third tick called %+v", snapshotCalls(calls))
	}
}

func TestOutboxSendsANewTitle(t *testing.T) {
	script := &outboxScript{}
	box, dir, calls, inst := newShareBox(t, script)
	appendNode(t, dir, "keep", "Keep")
	appendStoreLine(t, dir, []byte("{"))
	appendStoreLine(t, dir, []byte(`{"type":"node"}`))
	appendNode(t, dir, shareNode, "old")
	appendDelete(t, dir, shareNode)
	appendNode(t, dir, shareNode, "South")
	seedOneUser(t, dir)
	script.set(func(s *outboxScript) { s.channel = channelJSON(inst, shareUID, "South", shareGen) })
	mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, Title: "North", StartSent: true, Next: 2})
	box.tick(context.Background())
	got := snapshotCalls(calls)
	if len(got) != 2 || got[0].path != "/v1/hello" || got[1].method != http.MethodPut {
		t.Fatalf("calls = %+v", got)
	}
	if !strings.Contains(got[1].body, `"title":"South"`) {
		t.Fatalf("put = %s", got[1].body)
	}
	stored, ok := storedShare(t, dir, shareUID)
	if !ok || stored.Title != "South" || stored.Next != 2 || stored.Generation != shareGen {
		t.Fatalf("stored = %+v", stored)
	}
}

func TestOutboxReadsTheNodeFileOnce(t *testing.T) {
	t.Run("zero", func(t *testing.T) {
		script := &outboxScript{}
		box, _, _, _ := newShareBox(t, script)
		if reads := countNodeReads(box); reads != 0 {
			t.Fatalf("reads = %d", reads)
		}
	})
	t.Run("one", func(t *testing.T) {
		assertNodeReads(t, 1)
	})
	t.Run("three", func(t *testing.T) {
		assertNodeReads(t, 3)
	})
	t.Run("unreadable", func(t *testing.T) {
		script := &outboxScript{}
		box, dir, calls, _ := newShareBox(t, script)
		armCountedShares(t, dir, 1)
		reads := 0
		box.readNodes = func(string) ([]byte, error) {
			reads++
			return nil, errors.New("nodes unreadable")
		}
		box.tick(context.Background())
		if reads != 1 {
			t.Fatalf("reads = %d", reads)
		}
		if _, ok := storedShare(t, dir, shareUID); !ok || len(snapshotCalls(calls)) != 0 {
			t.Fatal("read failure changed the share or sent a request")
		}
		box.readNodes = nil
		box.tick(context.Background())
		if _, ok := storedShare(t, dir, shareUID); !ok || len(snapshotCalls(calls)) == 0 {
			t.Fatal("readable retry did not resume delivery")
		}
	})
	t.Run("missing", func(t *testing.T) {
		box, dir, calls, _ := newShareBox(t, &outboxScript{})
		armCountedShares(t, dir, 1)
		path := filepath.Join(dir, "nodes.jsonl")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		box.tick(context.Background())
		if _, ok := storedShare(t, dir, shareUID); !ok || len(snapshotCalls(calls)) != 0 {
			t.Fatal("missing nodes changed the share or sent a request")
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		box.tick(context.Background())
		if _, ok := storedShare(t, dir, shareUID); !ok || len(snapshotCalls(calls)) == 0 {
			t.Fatal("readable retry did not resume delivery")
		}
	})
}

func assertNodeReads(t *testing.T, n int) {
	t.Helper()
	script := &outboxScript{}
	box, dir, _, _ := newShareBox(t, script)
	armCountedShares(t, dir, n)
	if reads := countNodeReads(box); reads != 1 {
		t.Fatalf("reads = %d", reads)
	}
	ids := []string{shareUID, shareUIDB, shareUIDC}
	for i := 0; i < n; i++ {
		stored, ok := storedShare(t, dir, ids[i])
		if !ok || stored.State != "active" {
			t.Fatalf("stored %s = %+v", ids[i], stored)
		}
	}
}

func countNodeReads(box *shareOutbox) int {
	reads := 0
	box.readNodes = func(dir string) ([]byte, error) {
		reads++
		return os.ReadFile(filepath.Join(dir, "nodes.jsonl"))
	}
	box.tick(context.Background())
	return reads
}

func armCountedShares(t *testing.T, dir string, n int) {
	t.Helper()
	appendStoreLine(t, dir, []byte("{"))
	appendStoreLine(t, dir, []byte(`{"type":"node"}`))
	appendStoreLine(t, dir, []byte(`{"type":"transcript"}`))
	appendNode(t, dir, "gone", "Gone")
	appendDelete(t, dir, "gone")
	nodes := []struct{ id, uid string }{
		{shareNode, shareUID},
		{"keep", shareUIDB},
		{"plain", shareUIDC},
	}
	for i := 0; i < n; i++ {
		appendNode(t, dir, nodes[i].id, "North")
		writeChatLog(t, dir, nodes[i].id, nodes[i].uid, 1)
		mustSaveShare(t, dir, team.Share{UID: nodes[i].uid, Node: nodes[i].id, Title: "North"})
	}
}

func TestOutboxDropsATitleThatOpensAChannel(t *testing.T) {
	const foreign = "1111222233334444"
	t.Run("created", func(t *testing.T) {
		assertTitleDoesNotResurrect(t, http.StatusCreated, shareGen, 0)
	})
	t.Run("other generation", func(t *testing.T) {
		assertTitleDoesNotResurrect(t, http.StatusOK, foreign, http.StatusInternalServerError)
	})
}

func assertTitleDoesNotResurrect(t *testing.T, putStatus int, gen string, deleteStatus int) {
	t.Helper()
	script := &outboxScript{}
	box, dir, calls, inst := newShareBox(t, script)
	appendNode(t, dir, shareNode, "South")
	seedOneUser(t, dir)
	script.set(func(s *outboxScript) {
		s.putStatus = putStatus
		s.deleteStatus = deleteStatus
		s.channel = channelJSON(inst, shareUID, "South", gen)
	})
	mustSaveShare(t, dir, team.Share{
		UID: shareUID, Node: shareNode, Title: "North", StartSent: true, Next: 2,
	})
	box.tick(context.Background())
	got := snapshotCalls(calls)
	wantCalls := 2
	if putStatus == http.StatusCreated {
		wantCalls = 3
	}
	if len(got) != wantCalls || got[0].path != "/v1/hello" || got[1].method != http.MethodPut {
		t.Fatalf("calls = %+v", got)
	}
	if !strings.Contains(got[1].body, `"title":"South"`) || (wantCalls == 3 && (got[2].path != channelBase(inst, shareUID) || len(script.deleteQueries) != 1 || script.deleteQueries[0] != gen)) {
		t.Fatalf("calls = %+v", got)
	}
	for _, c := range got {
		if strings.HasSuffix(c.path, "/mirror") {
			t.Fatalf("mirror = %+v", got)
		}
	}
	stored, ok := storedShare(t, dir, shareUID)
	if !ok || stored.State != "lost" || stored.Title != "North" || stored.Generation != shareGen || stored.Next != 2 || !stored.StartSent {
		t.Fatalf("stored = %+v", stored)
	}
	box.tick(context.Background())
	if len(snapshotCalls(calls)) != len(got) {
		t.Fatalf("second = %+v", snapshotCalls(calls))
	}
}

func TestOutboxDeletesWhenTheChatIsGone(t *testing.T) {
	t.Run("node deleted", func(t *testing.T) {
		script := &outboxScript{}
		box, dir, calls, _ := newShareBox(t, script)
		appendNode(t, dir, shareNode, "North")
		appendDelete(t, dir, shareNode)
		seedOneUser(t, dir)
		mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode})
		box.tick(context.Background())
		got := snapshotCalls(calls)
		if len(got) != 2 || got[1].method != http.MethodDelete {
			t.Fatalf("calls = %+v", got)
		}
		assertBearer(t, got[0], false)
		assertBearer(t, got[1], true)
		assertNoStoredShare(t, dir, shareUID)
	})
	t.Run("node reused", func(t *testing.T) {
		script := &outboxScript{}
		box, dir, _, _ := newShareBox(t, script)
		appendNode(t, dir, shareNode, "North")
		writeChatLog(t, dir, shareNode, shareUIDB, 0)
		mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode})
		box.tick(context.Background())
		assertNoStoredShare(t, dir, shareUID)
	})
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"already gone", http.StatusNotFound},
		{"refused", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := &outboxScript{deleteStatus: tc.status}
			box, dir, _, _ := newShareBox(t, script)
			mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, State: "stopping"})
			box.tick(context.Background())
			assertNoStoredShare(t, dir, shareUID)
		})
	}
}

func TestOutboxLeavesStoppingWhenDeleteFails(t *testing.T) {
	script := &outboxScript{deleteStatus: http.StatusInternalServerError}
	box, dir, calls, _ := newShareBox(t, script)
	mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, State: "stopping"})
	box.tick(context.Background())
	stored, ok := storedShare(t, dir, shareUID)
	if !ok || stored.State != "stopping" {
		t.Fatalf("stored = %+v", stored)
	}
	if box.delivery(teamServerID) != "changed" {
		t.Fatalf("delivery = %s", box.delivery(teamServerID))
	}
	box.tick(context.Background())
	if len(snapshotCalls(calls)) != 2 {
		t.Fatalf("calls = %+v", snapshotCalls(calls))
	}
}

func TestOutboxSkipsLostAndFull(t *testing.T) {
	for _, state := range []string{"lost", "full"} {
		t.Run(state, func(t *testing.T) {
			script := &outboxScript{}
			box, dir, calls, _ := newShareBox(t, script)
			appendNode(t, dir, shareNode, "North")
			seedOneUser(t, dir)
			mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, State: state})
			box.tick(context.Background())
			if len(snapshotCalls(calls)) != 0 {
				t.Fatalf("calls = %+v", snapshotCalls(calls))
			}
			stored, ok := storedShare(t, dir, shareUID)
			if !ok || stored.State != state {
				t.Fatalf("stored = %+v", stored)
			}
		})
	}
}

func TestOutboxForgetsAMissingServer(t *testing.T) {
	script := &outboxScript{}
	box, dir, calls, _ := newShareBox(t, script)
	mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode})
	if err := team.Remove(dir, teamServerID); err != nil {
		t.Fatal(err)
	}
	box.tick(context.Background())
	assertNoStoredShare(t, dir, shareUID)
	if len(snapshotCalls(calls)) != 0 {
		t.Fatalf("calls = %+v", snapshotCalls(calls))
	}
}

func TestOutboxHelloBeforeTheToken(t *testing.T) {
	t.Run("other server", func(t *testing.T) {
		script := &outboxScript{helloID: teamOtherID}
		box, dir, calls, _ := newShareBox(t, script)
		appendNode(t, dir, shareNode, "North")
		seedOneUser(t, dir)
		mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, Title: "North"})
		box.tick(context.Background())
		got := snapshotCalls(calls)
		if len(got) != 1 || got[0].path != "/v1/hello" {
			t.Fatalf("calls = %+v", got)
		}
		assertBearer(t, got[0], false)
		if box.delivery(teamServerID) != "changed" {
			t.Fatalf("delivery = %s", box.delivery(teamServerID))
		}
		stored, ok := storedShare(t, dir, shareUID)
		if !ok || stored.Next != 0 {
			t.Fatalf("stored = %+v", stored)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		script := &outboxScript{failPath: "/v1/hello"}
		box, dir, _, _ := newShareBox(t, script)
		appendNode(t, dir, shareNode, "North")
		seedOneUser(t, dir)
		mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, Title: "North"})
		box.tick(context.Background())
		if box.delivery(teamServerID) != "offline" {
			t.Fatalf("delivery = %s", box.delivery(teamServerID))
		}
	})
	t.Run("refused", func(t *testing.T) {
		script := &outboxScript{mirrorStatus: http.StatusUnauthorized}
		box, dir, calls, _ := newShareBox(t, script)
		appendNode(t, dir, shareNode, "North")
		seedOneUser(t, dir)
		mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, Title: "North"})
		box.tick(context.Background())
		got := snapshotCalls(calls)
		if len(got) != 2 {
			t.Fatalf("calls = %+v", got)
		}
		assertBearer(t, got[1], true)
		if box.delivery(teamServerID) != "refused" {
			t.Fatalf("delivery = %s", box.delivery(teamServerID))
		}
	})
}

func TestOutboxChannelErrors(t *testing.T) {
	t.Run("append missing", func(t *testing.T) {
		script := &outboxScript{mirrorStatus: http.StatusNotFound}
		box, dir, calls, _ := newShareBox(t, script)
		appendNode(t, dir, shareNode, "North")
		seedOneUser(t, dir)
		mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, Title: "North"})
		mustSaveShare(t, dir, team.Share{UID: shareUIDB, Node: "other", Title: "North"})
		appendNode(t, dir, "other", "North")
		writeChatLog(t, dir, "other", shareUIDB, 1)
		box.tick(context.Background())
		mirrors := 0
		for _, c := range snapshotCalls(calls) {
			if strings.HasSuffix(c.path, "/mirror") {
				mirrors++
			}
		}
		if mirrors != 2 {
			t.Fatalf("calls = %+v", snapshotCalls(calls))
		}
		for _, uid := range []string{shareUID, shareUIDB} {
			stored, ok := storedShare(t, dir, uid)
			if !ok || stored.State != "lost" {
				t.Fatalf("%s = %+v", uid, stored)
			}
		}
		if box.delivery(teamServerID) != "ok" {
			t.Fatalf("delivery = %s", box.delivery(teamServerID))
		}
	})
	t.Run("quota", func(t *testing.T) {
		script := &outboxScript{mirrorStatus: http.StatusInsufficientStorage}
		box, dir, _, _ := newShareBox(t, script)
		appendNode(t, dir, shareNode, "North")
		seedOneUser(t, dir)
		mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, Title: "North"})
		box.tick(context.Background())
		stored, ok := storedShare(t, dir, shareUID)
		if !ok || stored.State != "full" || stored.Next != 0 {
			t.Fatalf("stored = %+v", stored)
		}
		if box.delivery(teamServerID) != "ok" {
			t.Fatalf("delivery = %s", box.delivery(teamServerID))
		}
	})
	t.Run("generation differs", func(t *testing.T) {
		script := &outboxScript{mirrorGen: "0123456789abcdef"}
		box, dir, calls, _ := newShareBox(t, script)
		appendNode(t, dir, shareNode, "North")
		seedOneUser(t, dir)
		mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, Title: "North"})
		mustSaveShare(t, dir, team.Share{UID: shareUIDB, Node: "other", Title: "North"})
		appendNode(t, dir, "other", "North")
		writeChatLog(t, dir, "other", shareUIDB, 1)
		box.tick(context.Background())
		stored, ok := storedShare(t, dir, shareUID)
		if !ok || stored.State != "lost" || stored.Next != 0 {
			t.Fatalf("stored = %+v", stored)
		}
		mirrors := 0
		for _, c := range snapshotCalls(calls) {
			if strings.HasSuffix(c.path, "/mirror") {
				mirrors++
			}
		}
		if mirrors != 2 {
			t.Fatalf("calls = %+v", snapshotCalls(calls))
		}
	})
}

func TestOutboxBackoffIsPerServer(t *testing.T) {
	script := &outboxScript{failPath: "/v1/hello"}
	box, dir, calls, _ := newShareBox(t, script)
	other := &outboxScript{helloID: teamOtherID}
	srv, otherCalls := recordOutbox(t, other)
	seedTeam(t, dir, labEntry(teamOtherID, srv.URL, strings.Repeat("cd", 32)))
	appendNode(t, dir, shareNode, "North")
	seedOneUser(t, dir)
	appendNode(t, dir, "other", "North")
	writeChatLog(t, dir, "other", shareUIDB, 1)
	other.channel = channelJSON(installationOf(t, dir), shareUIDB, "North", shareGen)
	mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, Title: "North"})
	mustSaveShare(t, dir, team.Share{UID: shareUIDB, Node: "other", Server: teamOtherID, Title: "North"})
	when := time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)
	box.now = func() time.Time { return when }
	box.tick(context.Background())
	if len(snapshotCalls(otherCalls)) == 0 {
		t.Fatal("the other server was skipped")
	}
	if box.delivery(teamServerID) != "offline" || box.delivery(teamOtherID) != "ok" {
		t.Fatalf("delivery %s %s", box.delivery(teamServerID), box.delivery(teamOtherID))
	}
	// Each open still fails, so the wait doubles: 4s, 8s, 16s, then the cap.
	// A success at the end resets the next wait to 4s.
	base := len(snapshotCalls(calls))
	when = when.Add(3 * time.Second)
	box.tick(context.Background())
	if len(snapshotCalls(calls)) != base {
		t.Fatal("waited less than 4s")
	}
	when = when.Add(time.Second)
	box.tick(context.Background())
	if len(snapshotCalls(calls)) == base {
		t.Fatal("4s did not open the server")
	}
	mark := len(snapshotCalls(calls))
	when = when.Add(7 * time.Second)
	box.tick(context.Background())
	if len(snapshotCalls(calls)) != mark {
		t.Fatal("8s wait opened early")
	}
	when = when.Add(time.Second)
	box.tick(context.Background())
	if len(snapshotCalls(calls)) == mark {
		t.Fatal("8s did not open")
	}
	mark = len(snapshotCalls(calls))
	when = when.Add(15 * time.Second)
	box.tick(context.Background())
	if len(snapshotCalls(calls)) != mark {
		t.Fatal("16s wait opened early")
	}
	when = when.Add(time.Second)
	box.tick(context.Background())
	if len(snapshotCalls(calls)) == mark {
		t.Fatal("16s did not open")
	}
	for i := 0; i < 6; i++ {
		when = when.Add(5 * time.Minute)
		box.tick(context.Background())
	}
	mark = len(snapshotCalls(calls))
	when = when.Add(5*time.Minute - time.Second)
	box.tick(context.Background())
	if len(snapshotCalls(calls)) != mark {
		t.Fatal("cap opened early")
	}
	when = when.Add(time.Second)
	script.set(func(s *outboxScript) { s.failPath = "" })
	box.tick(context.Background())
	if box.delivery(teamServerID) != "ok" {
		t.Fatalf("delivery = %s", box.delivery(teamServerID))
	}
	// The success sent every record and kept the hello. The next record's
	// failed mirror is a new failure, so the wait returns to 4s.
	sent := len(snapshotCalls(calls))
	appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:33:00Z", Text: "more"})
	script.set(func(s *outboxScript) { s.mirrorStatus = http.StatusInternalServerError })
	box.tick(context.Background())
	if len(snapshotCalls(calls)) == sent {
		t.Fatal("reset failure did not reach the server")
	}
	mark = len(snapshotCalls(calls))
	when = when.Add(3 * time.Second)
	box.tick(context.Background())
	if len(snapshotCalls(calls)) != mark {
		t.Fatal("reset did not return to 4s")
	}
	when = when.Add(time.Second)
	box.tick(context.Background())
	if len(snapshotCalls(calls)) == mark {
		t.Fatal("4s after reset did not open")
	}
}

func TestOutboxSkipsAnUnchangedLog(t *testing.T) {
	script := &outboxScript{}
	box, dir, calls, _ := newShareBox(t, script)
	appendNode(t, dir, shareNode, "North")
	seedOneUser(t, dir)
	mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, Title: "North"})
	reads := 0
	box.readLog = func(path string) []sessionlog.Event {
		reads++
		return sessionlog.ReadEvents(path)
	}
	box.tick(context.Background())
	if reads != 1 || len(snapshotCalls(calls)) == 0 {
		t.Fatalf("reads = %d calls %+v", reads, snapshotCalls(calls))
	}
	box.tick(context.Background())
	if reads != 1 {
		t.Fatalf("second tick read the log %d times", reads)
	}
	appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:33:00Z", Text: "more"})
	box.tick(context.Background())
	if reads != 2 {
		t.Fatalf("grown log reads = %d", reads)
	}
}

func TestOutboxReadsWhenTheSizeCheckFails(t *testing.T) {
	script := &outboxScript{}
	box, dir, _, _ := newShareBox(t, script)
	appendNode(t, dir, shareNode, "North")
	seedOneUser(t, dir)
	mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, Title: "North"})
	box.logSize = func(string) (int64, error) {
		return defaultLogSize(filepath.Join(t.TempDir(), "missing.jsonl"))
	}
	reads := 0
	box.readLog = func(path string) []sessionlog.Event {
		reads++
		return sessionlog.ReadEvents(path)
	}
	box.tick(context.Background())
	box.tick(context.Background())
	if reads != 2 {
		t.Fatalf("reads = %d", reads)
	}
}

func TestOutboxKeepsTheTitleWhenTheUpdateFails(t *testing.T) {
	script := &outboxScript{putStatus: http.StatusInternalServerError}
	box, dir, calls, _ := newShareBox(t, script)
	appendNode(t, dir, shareNode, "South")
	seedOneUser(t, dir)
	mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, Title: "North", StartSent: true, Next: 2})
	box.tick(context.Background())
	got := snapshotCalls(calls)
	if len(got) != 2 || got[1].method != http.MethodPut {
		t.Fatalf("calls = %+v", got)
	}
	stored, ok := storedShare(t, dir, shareUID)
	if !ok || stored.Title != "North" || stored.Next != 2 {
		t.Fatalf("stored = %+v", stored)
	}
	if box.delivery(teamServerID) != "changed" {
		t.Fatalf("delivery = %s", box.delivery(teamServerID))
	}
}

func TestOutboxLeavesStoppingWhenHelloFails(t *testing.T) {
	script := &outboxScript{failPath: "/v1/hello"}
	box, dir, calls, _ := newShareBox(t, script)
	mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, State: "stopping"})
	box.tick(context.Background())
	stored, ok := storedShare(t, dir, shareUID)
	if !ok || stored.State != "stopping" {
		t.Fatalf("stored = %+v", stored)
	}
	if box.delivery(teamServerID) != "offline" {
		t.Fatalf("delivery = %s", box.delivery(teamServerID))
	}
	for _, c := range snapshotCalls(calls) {
		if c.method == http.MethodDelete {
			t.Fatal("delete was sent without hello")
		}
	}
}

func TestOutboxTwoTicksAtOnce(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/mirror") {
			raw, _ := io.ReadAll(r.Body)
			var req struct {
				Generation string `json:"generation"`
			}
			if json.Unmarshal(raw, &req) != nil || req.Generation != shareGen {
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"error":"stale_generation"}`)
				return
			}
			once.Do(func() { close(entered) })
			<-release
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if r.URL.Path == "/v1/hello" {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, helloJSON(teamServerID, "Lab relay"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(letGo)
	dir := t.TempDir()
	if _, err := identity.LoadOrCreateInstallation(dir); err != nil {
		t.Fatal(err)
	}
	seedTeam(t, dir, labEntry(teamServerID, srv.URL, teamToken))
	appendNode(t, dir, shareNode, "North")
	seedOneUser(t, dir)
	mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, Title: "North"})
	var hits int
	var mu sync.Mutex
	counting := clientFor(srv)
	base := counting.Transport
	counting.Transport = teamRoundTrip(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		hits++
		mu.Unlock()
		return base.RoundTrip(r)
	})
	box := &shareOutbox{
		dataDir: dir,
		now:     func() time.Time { return time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC) },
		client:  counting,
	}
	app := &app{shareOutbox: box}
	app.copySharedChats()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case <-entered:
	case <-timer.C:
		t.Fatal("tick did not reach the server")
	}
	mu.Lock()
	before := hits
	mu.Unlock()
	app.copySharedChats()
	mu.Lock()
	after := hits
	mu.Unlock()
	if after != before {
		t.Fatalf("second tick sent a request, hits %d to %d", before, after)
	}
	letGo()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		box.mu.Lock()
		running := box.running
		box.mu.Unlock()
		if !running {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("tick did not finish")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestOutboxDoesNothingForADamagedFile(t *testing.T) {
	t.Run("servers", func(t *testing.T) {
		box, dir, calls, _ := newShareBox(t, &outboxScript{})
		mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode})
		before := []byte("{")
		if err := os.WriteFile(filepath.Join(dir, team.File), before, 0o600); err != nil {
			t.Fatal(err)
		}
		box.tick(context.Background())
		after, err := os.ReadFile(filepath.Join(dir, team.File))
		if err != nil || !bytes.Equal(before, after) || len(snapshotCalls(calls)) != 0 {
			t.Fatalf("file %q calls %+v", after, snapshotCalls(calls))
		}
	})
	t.Run("shares", func(t *testing.T) {
		box, dir, calls, _ := newShareBox(t, &outboxScript{})
		before := []byte("{")
		if err := os.WriteFile(filepath.Join(dir, team.SharesFile), before, 0o600); err != nil {
			t.Fatal(err)
		}
		box.tick(context.Background())
		after, err := os.ReadFile(filepath.Join(dir, team.SharesFile))
		if err != nil || !bytes.Equal(before, after) || len(snapshotCalls(calls)) != 0 {
			t.Fatalf("file %q calls %+v", after, snapshotCalls(calls))
		}
	})
	t.Run("no shares", func(t *testing.T) {
		box, dir, calls, _ := newShareBox(t, &outboxScript{})
		box.tick(context.Background())
		if _, err := os.Stat(filepath.Join(dir, team.SharesFile)); !os.IsNotExist(err) {
			t.Fatal("tick created the share file")
		}
		if len(snapshotCalls(calls)) != 0 {
			t.Fatal("tick called the server")
		}
	})
}

func TestOutboxNeedsAnInstallation(t *testing.T) {
	box, dir, calls, _ := newShareBox(t, &outboxScript{})
	appendNode(t, dir, shareNode, "North")
	seedOneUser(t, dir)
	mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, Title: "North"})
	if err := os.Remove(filepath.Join(dir, identity.InstallationFile)); err != nil {
		t.Fatal(err)
	}
	reads := 0
	box.readNodes = func(string) ([]byte, error) { reads++; return nil, errors.New("unexpected nodes read") }
	box.tick(context.Background())
	if len(snapshotCalls(calls)) != 0 || reads != 0 {
		t.Fatalf("calls = %+v, node reads = %d", snapshotCalls(calls), reads)
	}
	stored, ok := storedShare(t, dir, shareUID)
	if !ok || stored.State != "active" {
		t.Fatalf("stored = %+v", stored)
	}
}

func TestCopySharedChatsIsWired(t *testing.T) {
	root := repoRootFromTest(t)
	raw, err := os.ReadFile(filepath.Join(root, "internal/app/main.go"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	i := strings.Index(text, "a.maybeRefreshUsageAsync()")
	if i < 0 {
		t.Fatal("poll loop lost the usage refresh")
	}
	window := text[i:]
	if n := strings.Index(window, "time.Sleep"); n >= 0 {
		window = window[:n]
	}
	if !strings.Contains(window, "a.copySharedChats()") {
		t.Fatal("poll loop does not copy shared chats")
	}

	on, _, _, _ := startTeamCommand(t, true, true)
	if on.application.shareOutbox == nil {
		t.Fatal("muxer gate on left the outbox nil")
	}
	h := asTeamHandler(t, on.application.teamHandler)
	if h.shareDelivery == nil {
		t.Fatal("delivery hook is nil")
	}
	if h.shareDelivery(teamServerID) != "ok" {
		t.Fatalf("delivery = %s", h.shareDelivery(teamServerID))
	}
	if on.application.shareOutbox.files != &h.mu {
		t.Fatal("routes and the outbox do not share the share-file lock")
	}
}

func TestCopySharedChatsStaysNilOutsideTheMuxer(t *testing.T) {
	off, _, _, _ := startTeamCommand(t, false, true)
	if off.application.shareOutbox != nil {
		t.Fatal("gate off constructed an outbox")
	}
	off.application.copySharedChats()

	outside, _, _, _ := startTeamCommand(t, true, false)
	if outside.application == nil || outside.application.shareOutbox != nil {
		t.Fatal("gate on outside the muxer constructed an outbox")
	}
}

func TestCopySharedChatsWithNoOutboxDoesNothing(t *testing.T) {
	a := &app{}
	a.copySharedChats()
}

func (s *outboxScript) holdMirror() {
	s.mu.Lock()
	arrived := s.mirrorArrived
	gate := s.mirrorGate
	s.mu.Unlock()
	if gate == nil {
		return
	}
	if arrived != nil {
		s.mirrorOnce.Do(func() { close(arrived) })
	}
	<-gate
}

func newRaceBox(t *testing.T, script *outboxScript) (*shareOutbox, http.Handler, string, string) {
	t.Helper()
	box, dir, _, inst := newShareBox(t, script)
	h := newTeamHandler(dir, box.now, box.client)
	box.files = &asTeamHandler(t, h).mu
	return box, h, dir, inst
}

func armActiveShare(t *testing.T, dir string, members []team.Member) {
	t.Helper()
	appendNode(t, dir, shareNode, "North")
	seedOneUser(t, dir)
	mustSaveShare(t, dir, team.Share{
		UID: shareUID, Node: shareNode, Scope: "all", Title: "North", Members: members,
	})
}

// overlapShare runs a tick and lets during finish while that tick is inside
// the mirror call, after the outbox has loaded the share and before it saves.
func overlapShare(t *testing.T, box *shareOutbox, script *outboxScript, during func()) {
	t.Helper()
	arrived := make(chan struct{})
	gate := make(chan struct{})
	script.set(func(s *outboxScript) {
		s.mirrorArrived = arrived
		s.mirrorGate = gate
		s.mirrorOnce = sync.Once{}
	})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate) }) }
	defer func() {
		release()
		script.set(func(s *outboxScript) {
			s.mirrorGate = nil
			s.mirrorArrived = nil
		})
	}()
	done := make(chan struct{})
	go func() {
		box.tick(context.Background())
		close(done)
	}()
	wait := time.NewTimer(3 * time.Second)
	defer wait.Stop()
	select {
	case <-arrived:
	case <-done:
		t.Fatal("tick finished before the mirror")
	case <-wait.C:
		t.Fatal("tick did not reach the mirror")
	}
	during()
	release()
	finish := time.NewTimer(3 * time.Second)
	defer finish.Stop()
	select {
	case <-done:
	case <-finish.C:
		t.Fatal("tick did not finish")
	}
}

func TestOutboxLeavesAStoppedShareGone(t *testing.T) {
	script := &outboxScript{}
	box, h, dir, _ := newRaceBox(t, script)
	armActiveShare(t, dir, nil)
	overlapShare(t, box, script, func() {
		rec := doTeam(h, http.MethodDelete, "/api/team/shares/"+shareNode, "")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
		}
	})
	assertNoStoredShare(t, dir, shareUID)
	box.tick(context.Background())
	assertNoStoredShare(t, dir, shareUID)
}

func TestOutboxLeavesAStoppingShare(t *testing.T) {
	script := &outboxScript{}
	box, h, dir, inst := newRaceBox(t, script)
	armActiveShare(t, dir, []team.Member{{Seat: shareSeat, Access: "read"}})
	overlapShare(t, box, script, func() {
		script.set(func(s *outboxScript) { s.failPath = channelBase(inst, shareUID) })
		rec := doTeam(h, http.MethodDelete, "/api/team/shares/"+shareNode, "")
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
		}
		view := decodeShareView(t, rec)
		if view.State != "stopping" {
			t.Fatalf("view = %+v", view)
		}
	})
	stored, ok := storedShare(t, dir, shareUID)
	if !ok || stored.State != "stopping" {
		t.Fatalf("stored = %+v ok %v", stored, ok)
	}
	script.set(func(s *outboxScript) { s.failPath = "" })
	box.tick(context.Background())
	assertNoStoredShare(t, dir, shareUID)
}

func TestOutboxKeepsMemberEdits(t *testing.T) {
	script := &outboxScript{}
	box, h, dir, _ := newRaceBox(t, script)
	old := []team.Member{{Seat: shareSeat, Access: "read"}}
	armActiveShare(t, dir, old)
	next := []team.Member{{Seat: shareSeatB, Access: "write"}}
	overlapShare(t, box, script, func() {
		rec := putShare(h, shareNode, sharePutBody(teamServerID, "all", next))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
		}
	})
	stored, ok := storedShare(t, dir, shareUID)
	if !ok || stored.State != "active" || !stored.StartSent || stored.Next != 2 || stored.Generation != shareGen {
		t.Fatalf("stored = %+v", stored)
	}
	assertMembersEqual(t, stored.Members, next)
}

func TestOutboxKeepsANewShareOfTheSameChat(t *testing.T) {
	script := &outboxScript{}
	box, h, dir, inst := newRaceBox(t, script)
	armActiveShare(t, dir, []team.Member{{Seat: shareSeat, Access: "read"}})
	const freshGen = "1111222233334444"
	members := []team.Member{{Seat: shareSeatB, Access: "write"}}
	overlapShare(t, box, script, func() {
		rec := doTeam(h, http.MethodDelete, "/api/team/shares/"+shareNode, "")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("stop = %d %s", rec.Code, rec.Body.String())
		}
		script.set(func(s *outboxScript) {
			s.putStatus = http.StatusCreated
			s.channel = channelJSON(inst, shareUID, "North", freshGen)
		})
		rec = putShare(h, shareNode, sharePutBody(teamServerID, "all", members))
		if rec.Code != http.StatusCreated {
			t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
		}
	})
	stored, ok := storedShare(t, dir, shareUID)
	if !ok || stored.State != "active" || stored.Generation != freshGen || stored.StartSent || stored.Next != 0 || stored.Scope != "all" || stored.Title != "North" {
		t.Fatalf("stored = %+v", stored)
	}
	assertMembersEqual(t, stored.Members, members)
}

func TestOutboxLeavesABrokenShareFile(t *testing.T) {
	script := &outboxScript{}
	box, dir, _, _ := newShareBox(t, script)
	armActiveShare(t, dir, nil)
	saves := 0
	box.save = func(dir string, s team.Share) error {
		saves++
		return team.SaveShare(dir, s)
	}
	broken := []byte("{")
	overlapShare(t, box, script, func() {
		if err := os.WriteFile(filepath.Join(dir, team.SharesFile), broken, 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if saves != 0 {
		t.Fatalf("saves = %d", saves)
	}
	raw, err := os.ReadFile(filepath.Join(dir, team.SharesFile))
	if err != nil || !bytes.Equal(raw, broken) {
		t.Fatalf("file = %q err %v", raw, err)
	}
}

func TestShareRoutesAndTicksRace(t *testing.T) {
	for i := 0; i < 4; i++ {
		script := &outboxScript{}
		box, h, dir, _ := newRaceBox(t, script)
		armActiveShare(t, dir, []team.Member{{Seat: shareSeat, Access: "read"}})
		overlapShare(t, box, script, func() {
			rec := doTeam(h, http.MethodDelete, "/api/team/shares/"+shareNode, "")
			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
			}
		})
		assertNoStoredShare(t, dir, shareUID)
	}
}

func TestKeepShareFieldsWritesOnlyRouteFields(t *testing.T) {
	dir := t.TempDir()
	h := asTeamHandler(t, newTeamHandler(dir, fixedTeamClock(), team.NewClient()))
	old := []team.Member{{Seat: shareSeat, Access: "read"}}
	mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, Next: 7, StartSent: true, Members: old})
	mustSaveShare(t, dir, team.Share{UID: shareUIDB, Node: "other", Next: 3})
	next := []team.Member{{Seat: shareSeatB, Access: "write"}}
	got, err := h.keepShareFields(shareUID, func(cur *team.Share) {
		cur.Members = next
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Next != 7 || !got.StartSent || got.State != "active" || got.Title != "North" || got.Generation != shareGen {
		t.Fatalf("got = %+v", got)
	}
	assertMembersEqual(t, got.Members, next)
	stored, ok := storedShare(t, dir, shareUID)
	if !ok || stored.Next != 7 || !stored.StartSent {
		t.Fatalf("stored = %+v", stored)
	}
	assertMembersEqual(t, stored.Members, next)
	other, ok := storedShare(t, dir, shareUIDB)
	if !ok || other.Next != 3 || len(other.Members) != 0 {
		t.Fatalf("other = %+v", other)
	}
	if _, err := h.keepShareFields(shareUIDC, func(*team.Share) {}); !errors.Is(err, team.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	shares, err := team.LoadShares(dir)
	if err != nil || len(shares) != 2 {
		t.Fatalf("shares = %+v err %v", shares, err)
	}
	damaged := []byte("{")
	if err := os.WriteFile(filepath.Join(dir, team.SharesFile), damaged, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.keepShareFields(shareUID, func(cur *team.Share) { cur.State = "stopping" }); !errors.Is(err, team.ErrSharesDamaged) {
		t.Fatalf("damaged = %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, team.SharesFile))
	if err != nil || !bytes.Equal(raw, damaged) {
		t.Fatalf("file = %q", raw)
	}
}

func TestOutboxResumesAGrownLog(t *testing.T) {
	script := &outboxScript{}
	box, dir, calls, _ := newShareBox(t, script)
	appendNode(t, dir, shareNode, "North")
	appendOutbox(t, dir, sessionlog.Event{T: "meta", Time: "2026-11-12T09:30:00Z", Meta: &sessionlog.MetaEvent{Node: shareNode, UID: shareUID, Agent: "claude", Created: "2026-11-12T09:30:00Z"}})
	appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:31:00Z", Text: "one"})
	base := saveResumeShare(t, dir)
	offsets := watchLogReads(box)
	box.tick(context.Background())
	size1 := logBytes(t, dir)
	appendOutbox(t, dir, sessionlog.Event{T: "source", Time: "2026-11-12T09:32:00Z", Source: &sessionlog.SourceEvent{Reason: "cut"}})
	appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:33:00Z", Text: "two"})
	box.tick(context.Background())
	size2 := logBytes(t, dir)
	appendOutbox(t, dir, sessionlog.Event{T: "assistant", Time: "2026-11-12T09:34:00Z", Text: "kept"})
	appendOutbox(t, dir, sessionlog.Event{T: "meta", Time: "2026-11-12T09:35:00Z", Meta: &sessionlog.MetaEvent{Node: shareNode, UID: shareUID, Agent: "muse", Created: "2026-11-12T09:35:00Z"}})
	appendOutbox(t, dir, sessionlog.Event{T: "assistant", Time: "2026-11-12T09:36:00Z", Text: "next"})
	box.tick(context.Background())
	if !reflect.DeepEqual(*offsets, []int64{0, size1, size2}) {
		t.Fatalf("offsets = %v want 0,%d,%d", *offsets, size1, size2)
	}
	assertMirrorsMatchFullRead(t, dir, base, snapshotCalls(calls))
}

func TestOutboxRereadsATornLine(t *testing.T) {
	script := &outboxScript{}
	box, dir, calls, _ := newShareBox(t, script)
	appendNode(t, dir, shareNode, "North")
	appendOutbox(t, dir, sessionlog.Event{T: "meta", Time: "2026-11-12T09:30:00Z", Meta: &sessionlog.MetaEvent{Node: shareNode, UID: shareUID, Agent: "claude", Created: "2026-11-12T09:30:00Z"}})
	appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:31:00Z", Text: "one"})
	base := saveResumeShare(t, dir)
	prefix := logBytes(t, dir)
	torn := []byte(`{"t":"user","time":"2026-11-12T09:32:00Z","text":"torn-pending"`)
	if _, err := appendRawLog(t, dir, torn); err != nil {
		t.Fatal(err)
	}
	offsets := watchLogReads(box)
	box.tick(context.Background())
	if strings.Contains(mirrorCall(t, snapshotCalls(calls)).body, "torn-pending") {
		t.Fatal("torn line was sent")
	}
	if _, err := appendRawLog(t, dir, []byte("}\n")); err != nil {
		t.Fatal(err)
	}
	box.tick(context.Background())
	if !reflect.DeepEqual(*offsets, []int64{0, prefix}) {
		t.Fatalf("offsets = %v want 0,%d", *offsets, prefix)
	}
	assertMirrorsMatchFullRead(t, dir, base, snapshotCalls(calls))
}

func TestOutboxRereadsAShorterLog(t *testing.T) {
	script := &outboxScript{}
	box, dir, calls, _ := newShareBox(t, script)
	appendNode(t, dir, shareNode, "North")
	appendOutbox(t, dir, sessionlog.Event{T: "meta", Time: "2026-11-12T09:30:00Z", Meta: &sessionlog.MetaEvent{Node: shareNode, UID: shareUID, Agent: "claude", Created: "2026-11-12T09:30:00Z"}})
	appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:31:00Z", Text: strings.Repeat("q", 4000)})
	saveResumeShare(t, dir)
	offsets := watchLogReads(box)
	box.tick(context.Background())
	before := logBytes(t, dir)
	if err := os.WriteFile(chatLogPath(dir, shareNode), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	appendOutbox(t, dir, sessionlog.Event{T: "meta", Time: "2026-11-12T09:30:00Z", Meta: &sessionlog.MetaEvent{Node: shareNode, UID: shareUID, Agent: "claude", Created: "2026-11-12T09:30:00Z"}})
	appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:31:00Z", Text: "a"})
	appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:32:00Z", Text: "b"})
	appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:33:00Z", Text: "c"})
	if logBytes(t, dir) >= before {
		t.Fatal("replacement is not shorter")
	}
	mark := len(snapshotCalls(calls))
	box.tick(context.Background())
	if !reflect.DeepEqual(*offsets, []int64{0, 0}) {
		t.Fatalf("offsets = %v", *offsets)
	}
	want, _, _ := mirrorBatch(storedBeforeNext(t, dir, 2), sessionlog.ReadEvents(chatLogPath(dir, shareNode)))
	got := mirrorEntries(t, snapshotCalls(calls)[mark:])
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entries = %s\nwant %s", entryKeys(got), entryKeys(want))
	}
}

func TestOutboxRereadsAReplacedLog(t *testing.T) {
	script := &outboxScript{}
	box, dir, calls, _ := newShareBox(t, script)
	appendNode(t, dir, shareNode, "North")
	appendOutbox(t, dir, sessionlog.Event{T: "meta", Time: "2026-11-12T09:30:00Z", Meta: &sessionlog.MetaEvent{Node: shareNode, UID: shareUID, Agent: "claude", Created: "2026-11-12T09:30:00Z"}})
	appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:31:00Z", Text: "one"})
	saveResumeShare(t, dir)
	offsets := watchLogReads(box)
	box.tick(context.Background())
	first, err := os.Stat(chatLogPath(dir, shareNode))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(chatLogPath(dir, shareNode)); err != nil {
		t.Fatal(err)
	}
	appendOutbox(t, dir, sessionlog.Event{T: "meta", Time: "2026-11-12T09:30:00Z", Meta: &sessionlog.MetaEvent{Node: shareNode, UID: shareUID, Agent: "claude", Created: "2026-11-12T09:30:00Z"}})
	appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:31:00Z", Text: "one"})
	appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:32:00Z", Text: "extra"})
	appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:33:00Z", Text: "more"})
	second, err := os.Stat(chatLogPath(dir, shareNode))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(first, second) || second.Size() <= first.Size() {
		t.Fatal("replacement did not change identity and grow")
	}
	mark := len(snapshotCalls(calls))
	box.tick(context.Background())
	if !reflect.DeepEqual(*offsets, []int64{0, 0}) {
		t.Fatalf("offsets = %v", *offsets)
	}
	want, _, _ := mirrorBatch(storedBeforeNext(t, dir, 2), sessionlog.ReadEvents(chatLogPath(dir, shareNode)))
	got := mirrorEntries(t, snapshotCalls(calls)[mark:])
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entries = %s\nwant %s", entryKeys(got), entryKeys(want))
	}
}

func TestOutboxRereadsAfterARestart(t *testing.T) {
	script := &outboxScript{}
	box, dir, calls, _ := newShareBox(t, script)
	appendNode(t, dir, shareNode, "North")
	appendOutbox(t, dir, sessionlog.Event{T: "meta", Time: "2026-11-12T09:30:00Z", Meta: &sessionlog.MetaEvent{Node: shareNode, UID: shareUID, Agent: "claude", Created: "2026-11-12T09:30:00Z"}})
	appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:31:00Z", Text: "one"})
	saveResumeShare(t, dir)
	first := watchLogReads(box)
	box.tick(context.Background())
	appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:32:00Z", Text: "more"})
	box2 := &shareOutbox{dataDir: dir, now: box.now, client: box.client}
	second := watchLogReads(box2)
	mark := len(snapshotCalls(calls))
	box2.tick(context.Background())
	if !reflect.DeepEqual(*first, []int64{0}) || !reflect.DeepEqual(*second, []int64{0}) {
		t.Fatalf("first %v second %v", *first, *second)
	}
	want, _, _ := mirrorBatch(storedBeforeNext(t, dir, 2), sessionlog.ReadEvents(chatLogPath(dir, shareNode)))
	got := mirrorEntries(t, snapshotCalls(calls)[mark:])
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entries = %s\nwant %s", entryKeys(got), entryKeys(want))
	}
}

func TestOutboxSendsNoRecordsWhenTheLogReadFails(t *testing.T) {
	script := &outboxScript{}
	box, dir, calls, _ := newShareBox(t, script)
	appendNode(t, dir, shareNode, "North")
	seedOneUser(t, dir)
	saveResumeShare(t, dir)
	offsets := []int64{}
	box.readFrom = func(string, int64) ([]byte, error) {
		offsets = append(offsets, 0)
		return nil, errors.New("log unreadable")
	}
	box.tick(context.Background())
	if !reflect.DeepEqual(offsets, []int64{0}) {
		t.Fatalf("offsets = %v", offsets)
	}
	for _, c := range snapshotCalls(calls) {
		if strings.Contains(c.body, "one") {
			t.Fatalf("sent the log: %+v", snapshotCalls(calls))
		}
	}
}

func TestOutboxKeepsAPartialBatchAtTheSameOffset(t *testing.T) {
	script := &outboxScript{}
	box, dir, calls, _ := newShareBox(t, script)
	appendNode(t, dir, shareNode, "North")
	appendOutbox(t, dir, sessionlog.Event{T: "meta", Time: "2026-11-12T09:30:00Z", Meta: &sessionlog.MetaEvent{Node: shareNode, UID: shareUID, Agent: "claude", Created: "2026-11-12T09:30:00Z"}})
	appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:31:00Z", Text: "one"})
	base := saveResumeShare(t, dir)
	offsets := watchLogReads(box)
	box.tick(context.Background())
	size1 := logBytes(t, dir)
	for i := 0; i < 80; i++ {
		appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:32:00Z", Text: "m" + strings.Repeat("x", 2) + string(rune('a'+i%26))})
	}
	box.tick(context.Background())
	stored, ok := storedShare(t, dir, shareUID)
	if !ok || stored.Next >= 82 {
		t.Fatalf("growth tick finished the log: %+v", stored)
	}
	box.tick(context.Background())
	if !reflect.DeepEqual(*offsets, []int64{0, size1, size1}) {
		t.Fatalf("offsets = %v want 0,%d,%d", *offsets, size1, size1)
	}
	assertMirrorsMatchFullRead(t, dir, base, snapshotCalls(calls))
}

func TestOutboxCountsSkippedLogLinesLikeAFullRead(t *testing.T) {
	t.Run("blank crlf and garbage", func(t *testing.T) {
		meta, err := json.Marshal(sessionlog.Event{T: "meta", Time: "2026-11-12T09:30:00Z", Meta: &sessionlog.MetaEvent{Node: shareNode, UID: shareUID, Agent: "claude", Created: "2026-11-12T09:30:00Z"}})
		if err != nil {
			t.Fatal(err)
		}
		raw := append(append([]byte{}, meta...), '\n')
		raw = append(raw, '\n')
		raw = append(raw, " \n"...)
		raw = append(raw, "{\n"...)
		raw = append(raw, "{\"t\":\"user\",\"time\":\"2026-11-12T09:31:00Z\",\"text\":\"crlf\"}\r\n"...)
		raw = append(raw, "{\"t\":\"user\",\"time\":\"2026-11-12T09:32:00Z\",\"text\":\"plain\"}\n"...)
		assertOneLogRead(t, raw)
	})
	t.Run("line across the buffer", func(t *testing.T) {
		script := &outboxScript{}
		box, dir, calls, _ := newShareBox(t, script)
		appendNode(t, dir, shareNode, "North")
		appendOutbox(t, dir, sessionlog.Event{T: "meta", Time: "2026-11-12T09:30:00Z", Meta: &sessionlog.MetaEvent{Node: shareNode, UID: shareUID, Agent: "claude", Created: "2026-11-12T09:30:00Z"}})
		appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:31:00Z", Text: strings.Repeat("q", 70<<10)})
		base := saveResumeShare(t, dir)
		offsets := watchLogReads(box)
		box.tick(context.Background())
		if !reflect.DeepEqual(*offsets, []int64{0}) {
			t.Fatalf("offsets = %v", *offsets)
		}
		assertMirrorsMatchFullRead(t, dir, base, snapshotCalls(calls))
	})
	t.Run("line over the record cap", func(t *testing.T) {
		meta, err := json.Marshal(sessionlog.Event{T: "meta", Time: "2026-11-12T09:30:00Z", Meta: &sessionlog.MetaEvent{Node: shareNode, UID: shareUID, Agent: "claude", Created: "2026-11-12T09:30:00Z"}})
		if err != nil {
			t.Fatal(err)
		}
		user, err := json.Marshal(sessionlog.Event{T: "user", Time: "2026-11-12T09:32:00Z", Text: "after"})
		if err != nil {
			t.Fatal(err)
		}
		raw := append(append([]byte{}, meta...), '\n')
		raw = append(raw, bytes.Repeat([]byte("x"), 8<<20+2*(64<<10)+1)...)
		raw = append(raw, '\n')
		raw = append(raw, user...)
		raw = append(raw, '\n')
		assertOneLogRead(t, raw)
	})
}

func TestOutboxReadsTheLogWhenTheSizeCheckFails(t *testing.T) {
	script := &outboxScript{}
	box, dir, calls, _ := newShareBox(t, script)
	appendNode(t, dir, shareNode, "North")
	seedOneUser(t, dir)
	saveResumeShare(t, dir)
	stats := 0
	box.statLog = func(string) (os.FileInfo, error) {
		stats++
		return nil, errors.New("stat failed")
	}
	reads := 0
	box.readFrom = func(string, int64) ([]byte, error) {
		reads++
		return nil, errors.New("read")
	}
	box.tick(context.Background())
	if stats != 1 || reads != 0 {
		t.Fatalf("stats %d reads %d", stats, reads)
	}
	if !strings.Contains(mirrorCall(t, snapshotCalls(calls)).body, "one") {
		t.Fatal("size-check failure dropped the log")
	}
}

func TestOutboxSendsNothingWhenTheLogVanishes(t *testing.T) {
	script := &outboxScript{}
	box, dir, calls, _ := newShareBox(t, script)
	appendNode(t, dir, shareNode, "North")
	seedOneUser(t, dir)
	saveResumeShare(t, dir)
	box.statLog = func(path string) (os.FileInfo, error) {
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
		return info, nil
	}
	box.tick(context.Background())
	for _, c := range snapshotCalls(calls) {
		if strings.Contains(c.body, "one") {
			t.Fatalf("sent a vanished log: %+v", snapshotCalls(calls))
		}
	}
}

func saveResumeShare(t *testing.T, dir string) team.Share {
	t.Helper()
	mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, Scope: "all", Title: "North"})
	stored, ok := storedShare(t, dir, shareUID)
	if !ok {
		t.Fatal("missing share")
	}
	return stored
}

func storedBeforeNext(t *testing.T, dir string, next int) team.Share {
	t.Helper()
	stored, ok := storedShare(t, dir, shareUID)
	if !ok {
		t.Fatal("missing share")
	}
	stored.Next = next
	stored.StartSent = true
	return stored
}

func watchLogReads(box *shareOutbox) *[]int64 {
	offsets := []int64{}
	box.readFrom = func(path string, offset int64) ([]byte, error) {
		offsets = append(offsets, offset)
		return readLogRange(path, offset)
	}
	return &offsets
}

func readLogRange(path string, offset int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if offset > st.Size() {
		return nil, io.ErrUnexpectedEOF
	}
	return io.ReadAll(io.NewSectionReader(f, offset, st.Size()-offset))
}

func logBytes(t *testing.T, dir string) int64 {
	t.Helper()
	st, err := os.Stat(chatLogPath(dir, shareNode))
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

func appendRawLog(t *testing.T, dir string, raw []byte) (int, error) {
	t.Helper()
	f, err := os.OpenFile(chatLogPath(dir, shareNode), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := f.Write(raw)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return n, err
}

func assertOneLogRead(t *testing.T, raw []byte) {
	t.Helper()
	script := &outboxScript{}
	box, dir, calls, _ := newShareBox(t, script)
	appendNode(t, dir, shareNode, "North")
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(chatLogPath(dir, shareNode), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	base := saveResumeShare(t, dir)
	offsets := watchLogReads(box)
	box.tick(context.Background())
	if !reflect.DeepEqual(*offsets, []int64{0}) {
		t.Fatalf("offsets = %v", *offsets)
	}
	assertMirrorsMatchFullRead(t, dir, base, snapshotCalls(calls))
}

func assertMirrorsMatchFullRead(t *testing.T, dir string, base team.Share, calls []shareCall) {
	t.Helper()
	got := mirrorEntries(t, calls)
	events := sessionlog.ReadEvents(chatLogPath(dir, base.Node))
	var want []team.MirrorEntry
	share := base
	for guard := 0; guard < 8; guard++ {
		batch, next, done := mirrorBatch(share, events)
		want = append(want, batch...)
		if done {
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("entries = %s\nwant %s", entryKeys(got), entryKeys(want))
			}
			return
		}
		if len(batch) == 0 {
			t.Fatal("oracle made no progress")
		}
		share.Next = next
		share.StartSent = true
	}
	t.Fatal("oracle did not finish")
}

func mirrorEntries(t *testing.T, calls []shareCall) []team.MirrorEntry {
	t.Helper()
	var out []team.MirrorEntry
	for _, c := range calls {
		if !strings.HasSuffix(c.path, "/mirror") {
			continue
		}
		var body struct {
			Entries []team.MirrorEntry `json:"entries"`
		}
		if err := json.Unmarshal([]byte(c.body), &body); err != nil {
			t.Fatal(err)
		}
		out = append(out, body.Entries...)
	}
	return out
}

func entryKeys(es []team.MirrorEntry) string {
	keys := make([]string, len(es))
	for i, e := range es {
		keys[i] = e.Key + ":" + string(e.Body)
	}
	return strings.Join(keys, "\n")
}

// userRecordN is one user line of n bytes, not counting the newline.
func userRecordN(n int) []byte {
	const pre = `{"t":"user","time":"2026-11-12T09:32:00Z","text":"`
	const suf = `"}`
	body := make([]byte, n+1)
	copy(body, pre)
	for i := len(pre); i < n-len(suf); i++ {
		body[i] = 'q'
	}
	copy(body[n-len(suf):n], suf)
	body[n] = '\n'
	return body
}

func TestOutboxKeepsARecordThatArrivesInTwoParts(t *testing.T) {
	script := &outboxScript{}
	box, dir, calls, _ := newShareBox(t, script)
	appendNode(t, dir, shareNode, "North")
	appendOutbox(t, dir, sessionlog.Event{T: "meta", Time: "2026-11-12T09:30:00Z", Meta: &sessionlog.MetaEvent{Node: shareNode, UID: shareUID, Agent: "claude", Created: "2026-11-12T09:30:00Z"}})
	appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:31:00Z", Text: "one"})
	base := saveResumeShare(t, dir)
	prefix := logBytes(t, dir)
	line := userRecordN(100_000)
	if len(line) != 100_001 || bytes.Contains(line[:80_000], []byte("\n")) {
		t.Fatalf("line = %d bytes", len(line))
	}
	offsets := watchLogReads(box)
	if _, err := appendRawLog(t, dir, line[:80_000]); err != nil {
		t.Fatal(err)
	}
	box.tick(context.Background())
	if _, err := appendRawLog(t, dir, line[80_000:]); err != nil {
		t.Fatal(err)
	}
	box.tick(context.Background())
	last, err := json.Marshal(sessionlog.Event{T: "user", Time: "2026-11-12T09:33:00Z", Text: "last"})
	if err != nil {
		t.Fatal(err)
	}
	last = append(last, '\n')
	if _, err := appendRawLog(t, dir, last); err != nil {
		t.Fatal(err)
	}
	box.tick(context.Background())
	end := prefix + int64(len(line))
	if !reflect.DeepEqual(*offsets, []int64{0, prefix, end}) {
		t.Fatalf("offsets = %v want 0,%d,%d", *offsets, prefix, end)
	}
	assertMirrorsMatchFullRead(t, dir, base, snapshotCalls(calls))
}

func TestOutboxSkipsASplitRecordOverTheCap(t *testing.T) {
	script := &outboxScript{}
	box, dir, calls, _ := newShareBox(t, script)
	appendNode(t, dir, shareNode, "North")
	appendOutbox(t, dir, sessionlog.Event{T: "meta", Time: "2026-11-12T09:30:00Z", Meta: &sessionlog.MetaEvent{Node: shareNode, UID: shareUID, Agent: "claude", Created: "2026-11-12T09:30:00Z"}})
	appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:31:00Z", Text: "one"})
	base := saveResumeShare(t, dir)
	prefix := logBytes(t, dir)
	head := bytes.Repeat([]byte("x"), maxLogRecord)
	tail := append(bytes.Repeat([]byte("x"), 64<<10), '\n')
	after, err := json.Marshal(sessionlog.Event{T: "user", Time: "2026-11-12T09:33:00Z", Text: "after"})
	if err != nil {
		t.Fatal(err)
	}
	after = append(after, '\n')
	offsets := watchLogReads(box)
	if _, err := appendRawLog(t, dir, head); err != nil {
		t.Fatal(err)
	}
	box.tick(context.Background())
	if _, err := appendRawLog(t, dir, append(tail, after...)); err != nil {
		t.Fatal(err)
	}
	box.tick(context.Background())
	if !reflect.DeepEqual(*offsets, []int64{0, prefix}) {
		t.Fatalf("offsets = %v want 0,%d", *offsets, prefix)
	}
	assertMirrorsMatchFullRead(t, dir, base, snapshotCalls(calls))
}

func TestOutboxSeededPartialWritesMatchAFullRead(t *testing.T) {
	raw := seededLog()
	nl := bytes.IndexByte(raw, '\n')
	if nl < 0 {
		t.Fatal("seed log has no meta line")
	}
	for _, seed := range []int64{1, 2, 3} {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			script := &outboxScript{}
			box, dir, calls, _ := newShareBox(t, script)
			appendNode(t, dir, shareNode, "North")
			if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(chatLogPath(dir, shareNode), raw[:nl+1], 0o600); err != nil {
				t.Fatal(err)
			}
			base := saveResumeShare(t, dir)
			box.tick(context.Background())
			rng := rand.New(rand.NewSource(seed))
			rest := raw[nl+1:]
			for len(rest) > 0 {
				n := 1 + rng.Intn(3000)
				if n > len(rest) {
					n = len(rest)
				}
				if _, err := appendRawLog(t, dir, rest[:n]); err != nil {
					t.Fatal(err)
				}
				rest = rest[n:]
				box.tick(context.Background())
			}
			assertMirrorKeysMatchFullRead(t, dir, base, snapshotCalls(calls))
		})
	}
}

func assertMirrorKeysMatchFullRead(t *testing.T, dir string, base team.Share, calls []shareCall) {
	t.Helper()
	got := mirrorEntries(t, calls)
	events := sessionlog.ReadEvents(chatLogPath(dir, base.Node))
	var want []team.MirrorEntry
	share := base
	for guard := 0; guard < 8; guard++ {
		batch, next, done := mirrorBatch(share, events)
		want = append(want, batch...)
		if !done {
			if len(batch) == 0 {
				t.Fatal("oracle made no progress")
			}
			share.Next = next
			share.StartSent = true
			continue
		}
		if len(got) != len(want) {
			t.Fatalf("keys got %v want %v", mirrorKeyList(got), mirrorKeyList(want))
		}
		for i := range got {
			if got[i].Key != want[i].Key || !bytes.Equal(got[i].Body, want[i].Body) {
				t.Fatalf("entry %d got %s (%d bytes) want %s (%d bytes)", i, got[i].Key, len(got[i].Body), want[i].Key, len(want[i].Body))
			}
		}
		return
	}
	t.Fatal("oracle did not finish")
}

func mirrorKeyList(es []team.MirrorEntry) []string {
	keys := make([]string, len(es))
	for i, e := range es {
		keys[i] = e.Key
	}
	return keys
}

func seededLog() []byte {
	var b bytes.Buffer
	write := func(ev sessionlog.Event) {
		raw, err := json.Marshal(ev)
		if err != nil {
			panic(err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	write(sessionlog.Event{T: "meta", Time: "2026-11-12T09:30:00Z", Meta: &sessionlog.MetaEvent{Node: shareNode, UID: shareUID, Agent: "claude", Created: "2026-11-12T09:30:00Z"}})
	b.WriteString("\n")
	b.WriteString(" \n")
	b.WriteString("{\n")
	crlf, err := json.Marshal(sessionlog.Event{T: "user", Time: "2026-11-12T09:31:00Z", Text: "crlf"})
	if err != nil {
		panic(err)
	}
	b.Write(crlf)
	b.WriteString("\r\n")
	write(sessionlog.Event{T: "source", Time: "2026-11-12T09:32:00Z", Source: &sessionlog.SourceEvent{Reason: "cut"}})
	write(sessionlog.Event{T: "assistant", Time: "2026-11-12T09:33:00Z", Text: "kept"})
	write(sessionlog.Event{T: "tool", Time: "2026-11-12T09:34:00Z", Tool: &sessionlog.ToolEvent{ID: "t1", Title: "read", Status: "done"}})
	b.Write(userRecordN(70 << 10))
	b.Write(userRecordN(80 << 10))
	write(sessionlog.Event{T: "user", Time: "2026-11-12T09:35:00Z", Text: "tail"})
	return b.Bytes()
}

type logSpan struct {
	offset int64
	n      int
}

func countLogReads(box *shareOutbox) *[]logSpan {
	spans := []logSpan{}
	box.readLogAt = func(path string, offset int64) ([]byte, error) {
		raw, err := readLogRange(path, offset)
		spans = append(spans, logSpan{offset: offset, n: len(raw)})
		return raw, err
	}
	return &spans
}

func TestOutboxUnchangedLogIsNotReadAgain(t *testing.T) {
	script := &outboxScript{}
	box, dir, calls, _ := newShareBox(t, script)
	appendNode(t, dir, shareNode, "North")
	seedOneUser(t, dir)
	saveResumeShare(t, dir)
	reads := countLogReads(box)
	box.tick(context.Background())
	if len(*reads) != 1 || (*reads)[0].offset != 0 || (*reads)[0].n != int(logBytes(t, dir)) {
		t.Fatalf("first look = %+v size %d", *reads, logBytes(t, dir))
	}
	sent := len(snapshotCalls(calls))
	if sent == 0 {
		t.Fatal("first look sent nothing")
	}
	box.tick(context.Background())
	box.tick(context.Background())
	if len(*reads) != 1 || len(snapshotCalls(calls)) != sent {
		t.Fatalf("reads %+v calls %d", *reads, len(snapshotCalls(calls)))
	}
}

func TestOutboxGrownLogIsReadOnce(t *testing.T) {
	script := &outboxScript{}
	box, dir, _, _ := newShareBox(t, script)
	appendNode(t, dir, shareNode, "North")
	seedOneUser(t, dir)
	saveResumeShare(t, dir)
	reads := countLogReads(box)
	box.tick(context.Background())
	if len(*reads) != 1 {
		t.Fatalf("first look = %+v", *reads)
	}
	size := logBytes(t, dir)
	appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:40:00Z", Text: "more"})
	box.tick(context.Background())
	if len(*reads) != 2 {
		t.Fatalf("reads = %+v", *reads)
	}
	got := (*reads)[1]
	grew := int(logBytes(t, dir) - size)
	if got.offset != size || got.n != grew {
		t.Fatalf("grown read %+v want offset %d len %d", got, size, grew)
	}
}

func TestOutboxGrownForeignUIDStopsTheShare(t *testing.T) {
	script := &outboxScript{}
	box, dir, calls, inst := newShareBox(t, script)
	appendNode(t, dir, shareNode, "North")
	seedOneUser(t, dir)
	saveResumeShare(t, dir)
	reads := countLogReads(box)
	box.tick(context.Background())
	size := logBytes(t, dir)
	n := len(*reads)
	sent := len(snapshotCalls(calls))
	appendOutbox(t, dir, sessionlog.Event{T: "meta", Time: "2026-11-12T09:40:00Z", Meta: &sessionlog.MetaEvent{Node: shareNode, UID: shareUIDB, Agent: "claude", Created: "2026-11-12T09:40:00Z"}})
	appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:41:00Z", Text: "foreign"})
	box.tick(context.Background())
	if len(*reads) != n+1 {
		t.Fatalf("reads = %+v", *reads)
	}
	got := (*reads)[n]
	grew := int(logBytes(t, dir) - size)
	if got.offset != size || got.n != grew {
		t.Fatalf("grown read %+v want offset %d len %d", got, size, grew)
	}
	fresh := snapshotCalls(calls)[sent:]
	if len(fresh) != 1 || fresh[0].method != http.MethodDelete || fresh[0].path != channelBase(inst, shareUID) {
		t.Fatalf("calls = %+v", fresh)
	}
	for _, c := range snapshotCalls(calls) {
		if strings.Contains(c.body, "foreign") {
			t.Fatal("foreign text was sent")
		}
	}
	assertNoStoredShare(t, dir, shareUID)
}

func TestOutboxReplacedLogWithAnotherUIDStopsTheShare(t *testing.T) {
	script := &outboxScript{}
	box, dir, calls, inst := newShareBox(t, script)
	appendNode(t, dir, shareNode, "North")
	seedOneUser(t, dir)
	saveResumeShare(t, dir)
	reads := countLogReads(box)
	box.tick(context.Background())
	first, err := os.Stat(chatLogPath(dir, shareNode))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(chatLogPath(dir, shareNode)); err != nil {
		t.Fatal(err)
	}
	writeChatLog(t, dir, shareNode, shareUIDB, 2)
	second, err := os.Stat(chatLogPath(dir, shareNode))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(first, second) {
		t.Fatal("replacement did not change the file")
	}
	n := len(*reads)
	sent := len(snapshotCalls(calls))
	box.tick(context.Background())
	if len(*reads) != n+1 {
		t.Fatalf("reads = %+v", *reads)
	}
	got := (*reads)[n]
	if got.offset != 0 || got.n != int(second.Size()) {
		t.Fatalf("replaced read %+v want 0,%d", got, second.Size())
	}
	fresh := snapshotCalls(calls)[sent:]
	if len(fresh) != 1 || fresh[0].method != http.MethodDelete || fresh[0].path != channelBase(inst, shareUID) {
		t.Fatalf("calls = %+v", fresh)
	}
	assertNoStoredShare(t, dir, shareUID)
}

func TestOutboxRestartReadsTheWholeLog(t *testing.T) {
	script := &outboxScript{}
	box, dir, calls, _ := newShareBox(t, script)
	appendNode(t, dir, shareNode, "North")
	seedOneUser(t, dir)
	saveResumeShare(t, dir)
	box.tick(context.Background())
	stored, ok := storedShare(t, dir, shareUID)
	if !ok {
		t.Fatal("missing share")
	}
	appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:40:00Z", Text: "more"})
	box2 := &shareOutbox{dataDir: dir, now: box.now, client: box.client}
	reads := countLogReads(box2)
	mark := len(snapshotCalls(calls))
	box2.tick(context.Background())
	size := logBytes(t, dir)
	if len(*reads) != 1 || (*reads)[0].offset != 0 || (*reads)[0].n != int(size) {
		t.Fatalf("restart read %+v want 0,%d", *reads, size)
	}
	box2.tick(context.Background())
	if len(*reads) != 1 {
		t.Fatalf("second tick read %+v", *reads)
	}
	want, _, _ := mirrorBatch(stored, sessionlog.ReadEvents(chatLogPath(dir, shareNode)))
	got := mirrorEntries(t, snapshotCalls(calls)[mark:])
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entries = %s\nwant %s", entryKeys(got), entryKeys(want))
	}
}

func TestOutboxLogIdentityMatchesAFullRead(t *testing.T) {
	meta := func(uid string) sessionlog.Event {
		return sessionlog.Event{T: "meta", Time: "2026-11-12T09:30:00Z", Meta: &sessionlog.MetaEvent{Node: shareNode, UID: uid, Agent: "claude", Created: "2026-11-12T09:30:00Z"}}
	}
	user := sessionlog.Event{T: "user", Time: "2026-11-12T09:31:00Z", Text: "kept"}
	for _, tc := range []struct {
		name string
		live bool
		raw  []byte
	}{
		{name: "no header", raw: mustLog(tLine(user))},
		{name: "other uid", raw: mustLog(tLine(meta(shareUIDB)), tLine(user))},
		{name: "two uids", raw: mustLog(tLine(meta(shareUID)), tLine(meta(shareUIDB)), tLine(user))},
		{name: "same uid twice", live: true, raw: mustLog(tLine(meta(shareUID)), tLine(meta(shareUID)), tLine(user))},
		{name: "blank uid then the chat", live: true, raw: mustLog([]byte("{\"t\":\"meta\"}\n"), []byte("{\"t\":\"meta\",\"meta\":{\"node\":\"chat\",\"uid\":\"\",\"agent\":\"claude\",\"created\":\"2026-11-12T09:30:00Z\"}}\n"), tLine(meta(shareUID)), tLine(user))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := &outboxScript{}
			box, dir, calls, _ := newShareBox(t, script)
			appendNode(t, dir, shareNode, "North")
			if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(chatLogPath(dir, shareNode), tc.raw, 0o600); err != nil {
				t.Fatal(err)
			}
			id := sessionlog.ReadLogIdentity(chatLogPath(dir, shareNode))
			wantLive := id.Status == sessionlog.IdentityOK && id.UID == shareUID
			if wantLive != tc.live {
				t.Fatalf("fixture live %v identity %+v", tc.live, id)
			}
			saveResumeShare(t, dir)
			reads := countLogReads(box)
			box.tick(context.Background())
			if len(*reads) != 1 || (*reads)[0].offset != 0 || (*reads)[0].n != len(tc.raw) {
				t.Fatalf("reads = %+v", *reads)
			}
			_, stored := storedShare(t, dir, shareUID)
			if stored != tc.live {
				t.Fatalf("stored %v want %v calls %+v", stored, tc.live, snapshotCalls(calls))
			}
			if tc.live && !strings.Contains(mirrorCall(t, snapshotCalls(calls)).body, "kept") {
				t.Fatal("live chat was not copied")
			}
		})
	}
}

func TestOutboxMissingLogRemovesTheShare(t *testing.T) {
	script := &outboxScript{}
	box, dir, _, _ := newShareBox(t, script)
	appendNode(t, dir, shareNode, "North")
	saveResumeShare(t, dir)
	box.tick(context.Background())
	assertNoStoredShare(t, dir, shareUID)
}

func tLine(ev sessionlog.Event) []byte {
	raw, err := json.Marshal(ev)
	if err != nil {
		panic(err)
	}
	return append(raw, '\n')
}

func mustLog(parts ...[]byte) []byte {
	return bytes.Join(parts, nil)
}

func appendTool(t *testing.T, dir, id string) {
	t.Helper()
	appendOutbox(t, dir, sessionlog.Event{T: "tool", Time: "2026-11-12T09:32:00Z", Tool: &sessionlog.ToolEvent{ID: id, Title: "read", Status: "done"}})
}

func shareFileBytes(t *testing.T, dir string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, team.SharesFile))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestOutboxToolGrowthReadsOnlyTheNewBytes(t *testing.T) {
	script := &outboxScript{}
	box, dir, calls, _ := newShareBox(t, script)
	appendNode(t, dir, shareNode, "North")
	seedOneUser(t, dir)
	base := saveResumeShare(t, dir)
	offsets := watchLogReads(box)
	box.tick(context.Background())
	want := []int64{0, logBytes(t, dir)}
	saved := shareFileBytes(t, dir)
	for i := 0; i < 4; i++ {
		appendTool(t, dir, fmt.Sprintf("tool-%d", i))
		box.tick(context.Background())
		if i < 3 {
			want = append(want, logBytes(t, dir))
		}
	}
	if !bytes.Equal(saved, shareFileBytes(t, dir)) {
		t.Fatal("tool growth wrote team-shares.json")
	}
	if !reflect.DeepEqual(*offsets, want) {
		t.Fatalf("offsets = %v want %v", *offsets, want)
	}
	appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:40:00Z", Text: "later"})
	box.tick(context.Background())
	assertMirrorsMatchFullRead(t, dir, base, snapshotCalls(calls))
}

func TestOutboxDroppedSaveBetweenToolGrowth(t *testing.T) {
	script := &outboxScript{}
	box, dir, calls, _ := newShareBox(t, script)
	appendNode(t, dir, shareNode, "North")
	seedOneUser(t, dir)
	base := saveResumeShare(t, dir)
	rewound := shareFileBytes(t, dir)
	offsets := watchLogReads(box)
	box.tick(context.Background())
	want := []int64{0, logBytes(t, dir)}
	appendTool(t, dir, "tool-1")
	box.tick(context.Background())
	want = append(want, logBytes(t, dir))
	saved := shareFileBytes(t, dir)
	appendTool(t, dir, "tool-2")
	box.tick(context.Background())
	if !bytes.Equal(saved, shareFileBytes(t, dir)) {
		t.Fatal("tool growth wrote team-shares.json")
	}
	if err := os.WriteFile(filepath.Join(dir, team.SharesFile), rewound, 0o600); err != nil {
		t.Fatal(err)
	}
	appendTool(t, dir, "tool-3")
	box.tick(context.Background())
	want = append(want, 0)
	after := logBytes(t, dir)
	appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:40:00Z", Text: "later"})
	box.tick(context.Background())
	want = append(want, after)
	if !reflect.DeepEqual(*offsets, want) {
		t.Fatalf("offsets = %v want %v", *offsets, want)
	}
	got := snapshotCalls(calls)
	// The copy before the rewind is already on the server. The mirrors
	// after it are what a full read of the rewound share sends.
	start := len(got)
	seen := 0
	for i, c := range got {
		if !strings.HasSuffix(c.path, "/mirror") {
			continue
		}
		seen++
		if seen == 2 {
			start = i
		}
	}
	assertMirrorsMatchFullRead(t, dir, base, got[start:])
}

func TestOutboxDropsAReplacedChannel(t *testing.T) {
	const fresh = "1111222233334444"
	script := &outboxScript{}
	box, dir, calls, inst := newShareBox(t, script)
	h := newTeamHandler(dir, box.now, box.client)
	box.files = &asTeamHandler(t, h).mu
	script.set(func(s *outboxScript) {
		s.channel = channelJSON(inst, shareUID, "North", fresh)
	})
	old := []team.Member{{Seat: shareSeat, Access: "read"}}
	armActiveShare(t, dir, old)
	next := []team.Member{{Seat: shareSeatB, Access: "write"}}
	overlapShare(t, box, script, func() {
		rec := putShare(h, shareNode, sharePutBody(teamServerID, "all", next))
		if rec.Code != http.StatusOK {
			t.Fatalf("members = %d %s", rec.Code, rec.Body.String())
		}
	})
	var sent struct {
		Generation string `json:"generation"`
	}
	if err := json.Unmarshal([]byte(mirrorCall(t, snapshotCalls(calls)).body), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Generation != shareGen {
		t.Fatalf("generation = %q", sent.Generation)
	}
	if len(script.keptMirrors()) != 0 {
		t.Fatalf("stored mirrors = %d", len(script.keptMirrors()))
	}
	stored, ok := storedShare(t, dir, shareUID)
	if !ok || stored.State != "lost" || stored.Next != 0 || stored.StartSent || stored.Generation != shareGen || stored.Title != "North" {
		t.Fatalf("stored = %+v", stored)
	}
	assertMembersEqual(t, stored.Members, next)
	if box.confirmed(teamServerID) || box.waiting(teamServerID) || box.delivery(teamServerID) != "ok" {
		t.Fatalf("confirmed %v waiting %v delivery %s", box.confirmed(teamServerID), box.waiting(teamServerID), box.delivery(teamServerID))
	}
	mark := len(snapshotCalls(calls))
	appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:45:00Z", Text: "later"})
	box.now = func() time.Time { return time.Date(2026, 11, 12, 9, 40, 0, 0, time.UTC) }
	box.tick(context.Background())
	if len(snapshotCalls(calls)) != mark {
		t.Fatalf("second = %+v", snapshotCalls(calls)[mark:])
	}
}

func TestOutboxAppendsWithTheTitleGeneration(t *testing.T) {
	const titleGen = "fedcba9876543210"
	script := &outboxScript{}
	box, dir, calls, inst := newShareBox(t, script)
	appendNode(t, dir, shareNode, "South")
	seedOneUser(t, dir)
	appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:32:00Z", Text: "two"})
	script.set(func(s *outboxScript) {
		s.channel = channelJSON(inst, shareUID, "South", titleGen)
		s.mirrorGen = titleGen
	})
	mustSaveShare(t, dir, team.Share{
		UID: shareUID, Node: shareNode, Title: "North", Generation: titleGen, StartSent: true, Next: 2,
	})
	box.tick(context.Background())
	got := snapshotCalls(calls)
	if len(got) != 3 || got[0].path != "/v1/hello" || got[1].method != http.MethodPut || !strings.HasSuffix(got[2].path, "/mirror") {
		t.Fatalf("calls = %+v", got)
	}
	if !strings.Contains(got[1].body, `"title":"South"`) {
		t.Fatalf("put = %s", got[1].body)
	}
	var sent struct {
		Generation string `json:"generation"`
		Entries    []struct {
			Key string `json:"key"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(got[2].body), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Generation != titleGen || len(sent.Entries) != 1 || sent.Entries[0].Key != "0/2" {
		t.Fatalf("mirror = %s", got[2].body)
	}
	if len(script.keptMirrors()) != 1 {
		t.Fatalf("stored mirrors = %d", len(script.keptMirrors()))
	}
	stored, ok := storedShare(t, dir, shareUID)
	if !ok || stored.State != "active" || stored.Title != "South" || stored.Generation != titleGen || stored.Next != 3 || !stored.StartSent {
		t.Fatalf("stored = %+v", stored)
	}
}

func TestOutboxLeavesAStoppedShareThatChanged(t *testing.T) {
	script := &outboxScript{}
	box, dir, _, _ := newShareBox(t, script)
	armActiveShare(t, dir, []team.Member{{Seat: shareSeat, Access: "read"}})
	wantMembers := []team.Member{{Seat: shareSeatB, Access: "write"}}
	overlapShare(t, box, script, func() {
		stored, ok := storedShare(t, dir, shareUID)
		if !ok {
			t.Fatal("missing share")
		}
		stored.State = "stopping"
		stored.Title = "Halted"
		stored.Next = 9
		stored.Members = wantMembers
		if err := team.SaveShare(dir, stored); err != nil {
			t.Fatal(err)
		}
	})
	stored, ok := storedShare(t, dir, shareUID)
	if !ok || stored.State != "stopping" || stored.Title != "Halted" || stored.Next != 9 || stored.StartSent || stored.Generation != shareGen {
		t.Fatalf("stored = %+v", stored)
	}
	assertMembersEqual(t, stored.Members, wantMembers)
}

func TestOutboxOldStopDoesNotDeleteNewGeneration(t *testing.T) {
	script := &outboxScript{}
	box, h, dir, inst := newRaceBox(t, script)
	armActiveShare(t, dir, nil)
	old, ok := storedShare(t, dir, shareUID)
	if !ok {
		t.Fatal("missing share")
	}
	old.State = "stopping"
	if err := team.SaveShare(dir, old); err != nil {
		t.Fatal(err)
	}
	original := *box.client
	arrived, release := make(chan struct{}), make(chan struct{})
	box.client = &team.Client{Transport: teamRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v1/hello" {
			close(arrived)
			<-release
		}
		if r.Method == http.MethodDelete {
			if r.URL.Query().Get("generation") != shareGen {
				t.Errorf("old stop generation = %q", r.URL.RawQuery)
			}
			script.mu.Lock()
			current := channelGeneration(script.channel)
			script.mu.Unlock()
			if r.URL.Query().Get("generation") == current {
				return original.Transport.RoundTrip(r)
			}
			return &http.Response{StatusCode: http.StatusConflict, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"stale_generation"}`)), Request: r}, nil
		}
		return original.Transport.RoundTrip(r)
	})}
	entries, err := team.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); box.one(context.Background(), entries, inst, old, nil) }()
	select {
	case <-arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("stop did not arrive")
	}
	if rec := doTeam(h, http.MethodDelete, "/api/team/shares/"+shareNode, ""); rec.Code != http.StatusNoContent {
		close(release)
		<-done
		t.Fatalf("stop=%d", rec.Code)
	}
	const fresh = "1111222233334444"
	script.set(func(s *outboxScript) {
		s.putStatus = http.StatusCreated
		s.channel = channelJSON(inst, shareUID, "North", fresh)
	})
	if rec := putShare(h, shareNode, sharePutBody(teamServerID, "all", []team.Member{})); rec.Code != http.StatusCreated {
		close(release)
		<-done
		t.Fatalf("reshare=%d", rec.Code)
	}
	close(release)
	<-done
	stored, ok := storedShare(t, dir, shareUID)
	if !ok || stored.Generation != fresh {
		t.Fatal("older stop deleted new share")
	}
}

func TestOutboxStopFencesStoredGeneration(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusConflict, http.StatusUnauthorized, http.StatusNotFound} {
		for _, replace := range []bool{false, true} {
			t.Run(fmt.Sprintf("status_%d_replace_%t", status, replace), func(t *testing.T) {
				script := &outboxScript{deleteStatus: status}
				box, dir, _, inst := newShareBox(t, script)
				mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, State: "stopping"})
				old, _ := storedShare(t, dir, shareUID)
				original := *box.client
				box.client = &team.Client{Transport: teamRoundTrip(func(r *http.Request) (*http.Response, error) {
					if r.Method == http.MethodDelete && replace {
						fresh := old
						fresh.Generation = "1111222233334444"
						if err := team.SaveShare(dir, fresh); err != nil {
							t.Error(err)
						}
					}
					return original.Transport.RoundTrip(r)
				})}
				entries, err := team.Load(dir)
				if err != nil {
					t.Fatal(err)
				}
				box.stop(context.Background(), entries[0], inst, old)
				stored, ok := storedShare(t, dir, shareUID)
				if ok != replace || (replace && stored.Generation != "1111222233334444") {
					t.Fatalf("stored: %+v, present=%t", stored, ok)
				}
				if len(script.deleteQueries) != 1 || script.deleteQueries[0] != shareGen {
					t.Fatalf("delete generations: %v", script.deleteQueries)
				}
			})
		}
	}
}

func TestOutboxStopLeavesDamagedShareFile(t *testing.T) {
	script := &outboxScript{}
	box, dir, _, inst := newShareBox(t, script)
	mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, State: "stopping"})
	old, _ := storedShare(t, dir, shareUID)
	original := *box.client
	box.client = &team.Client{Transport: teamRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodDelete {
			if err := os.WriteFile(filepath.Join(dir, team.SharesFile), []byte("broken"), 0o600); err != nil {
				t.Error(err)
			}
		}
		return original.Transport.RoundTrip(r)
	})}
	entries, err := team.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	box.stop(context.Background(), entries[0], inst, old)
	raw, err := os.ReadFile(filepath.Join(dir, team.SharesFile))
	if err != nil || string(raw) != "broken" {
		t.Fatalf("damaged file changed: %q %v", raw, err)
	}
}

func TestOutboxStopKeepsDifferentShareUID(t *testing.T) {
	box, dir, _, inst := newShareBox(t, &outboxScript{})
	mustSaveShare(t, dir, team.Share{UID: shareUIDB, Node: shareNode, State: "stopping"})
	old, _ := storedShare(t, dir, shareUIDB)
	old.UID = shareUID
	entries, err := team.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	box.stop(context.Background(), entries[0], inst, old)
	if _, ok := storedShare(t, dir, shareUIDB); !ok {
		t.Fatal("unrelated share removed")
	}
}

func TestOutboxStopDoesNotMatchAnotherSharesGeneration(t *testing.T) {
	box, dir, _, inst := newShareBox(t, &outboxScript{})
	mustSaveShare(t, dir, team.Share{UID: shareUID, Node: shareNode, Generation: "1111222233334444"})
	mustSaveShare(t, dir, team.Share{UID: shareUIDB, Node: "other", Generation: shareGen})
	old, ok := storedShare(t, dir, shareUID)
	if !ok {
		t.Fatal("missing replacement share")
	}
	old.Generation = shareGen
	entries, err := team.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	box.stop(context.Background(), entries[0], inst, old)
	stored, ok := storedShare(t, dir, shareUID)
	if !ok || stored.Generation != "1111222233334444" {
		t.Fatalf("replacement share removed or changed: %+v, present %t", stored, ok)
	}
	other, ok := storedShare(t, dir, shareUIDB)
	if !ok || other.Generation != shareGen {
		t.Fatalf("unrelated share removed or changed: %+v, present %t", other, ok)
	}
}

func TestOutboxRetainsOwnerFrontierBeforeDeletingGeneration(t *testing.T) {
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
	box := &shareOutbox{dataDir: dir}
	box.deleteStoredGeneration(share)
	subs, err := team.LoadSubs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if subs[0].OwnerFrontier == nil || subs[0].OwnerFrontier.Record != 1 {
		t.Fatalf("frontier %+v", subs[0].OwnerFrontier)
	}
}

func TestOutboxFrontierFailureKeepsShare(t *testing.T) {
	for _, kind := range []string{"subs_damaged", "identity_missing", "frontier_unavailable", "already_retained"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			inst, err := identity.LoadOrCreateInstallation(dir)
			if err != nil {
				t.Fatal(err)
			}
			sub := baseSub()
			sub.Owner = string(inst)
			sub.Access = "owner"
			if kind == "already_retained" {
				sub.OwnerFrontier = &team.OwnerFrontier{Node: "local", Segment: 0, Record: 1}
			}
			seedSub(t, dir, sub)
			share := team.Share{UID: sub.Chat, Node: "local", Server: sub.Server, Generation: sub.Generation, Start: 2, Next: 2}
			mustSaveShare(t, dir, share)
			switch kind {
			case "subs_damaged":
				if err := os.WriteFile(filepath.Join(dir, team.SubsFile), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "identity_missing":
				if err := os.Remove(filepath.Join(dir, identity.InstallationFile)); err != nil {
					t.Fatal(err)
				}
			}
			box := &shareOutbox{dataDir: dir}
			err = box.preserveOwnerFrontiers(share)
			if kind == "already_retained" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("missing error")
			}
			if kind != "already_retained" {
				box.deleteStoredGeneration(share)
				if _, err := team.LoadShares(dir); err != nil {
					t.Fatal(err)
				}
				shares, _ := team.LoadShares(dir)
				if len(shares) != 1 {
					t.Fatal("failed frontier removed the share")
				}
			}
		})
	}
}

func TestOutboxDirectRetirementKeepsDamagedMetadata(t *testing.T) {
	for _, kind := range []string{"shares", "subs"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			box := &shareOutbox{dataDir: dir}
			if kind == "shares" {
				if err := os.WriteFile(filepath.Join(dir, team.SharesFile), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				share := team.Share{UID: shareUID, Node: shareNode, Server: teamServerID, Generation: shareGen}
				mustSaveShare(t, dir, share)
				if err := os.WriteFile(filepath.Join(dir, team.SubsFile), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			box.deleteStored(shareUID)
			if kind == "subs" {
				shares, err := team.LoadShares(dir)
				if err != nil || len(shares) != 1 {
					t.Fatalf("share not retained: %v %v", shares, err)
				}
			}
		})
	}
}

func TestOutboxWaitsForShareMutation(t *testing.T) {
	var lock sync.Mutex
	o := &shareOutbox{dataDir: t.TempDir(), shareOps: &lock}
	lock.Lock()
	done := make(chan struct{})
	go func() { o.one(context.Background(), nil, "", team.Share{UID: shareUID}, nil); close(done) }()
	select {
	case <-done:
		lock.Unlock()
		t.Fatal("outbox passed an in-flight share mutation")
	case <-time.After(100 * time.Millisecond):
	}
	lock.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("outbox did not resume")
	}
}
