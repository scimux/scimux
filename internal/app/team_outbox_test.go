package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

type outboxScript struct {
	helloStatus  int
	helloID      string
	putStatus    int
	deleteStatus int
	mirrorStatus int
	mirrorGen    string
	channel      string
	failPath     string
	owner        string
}

func (s *outboxScript) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.failPath != "" && r.URL.Path == s.failPath {
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
		} else {
			_, _ = io.WriteString(w, `{"error":"nope"}`)
		}
	case r.Method == http.MethodDelete:
		status := s.deleteStatus
		if status == 0 {
			status = http.StatusNoContent
		}
		w.WriteHeader(status)
	case r.Method == http.MethodPut:
		status := s.putStatus
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		if (status == http.StatusOK || status == http.StatusCreated) && s.channel != "" {
			_, _ = io.WriteString(w, s.channel)
		}
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/mirror"):
		status := s.mirrorStatus
		if status == 0 {
			status = http.StatusOK
		}
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Entries []struct {
				Key string `json:"key"`
			} `json:"entries"`
		}
		_ = json.Unmarshal(raw, &req)
		w.WriteHeader(status)
		if status != http.StatusOK {
			return
		}
		gen := s.mirrorGen
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
	script.mirrorStatus = http.StatusOK
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
	script.channel = channelJSON(inst, shareUID, "South", shareGen)
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
	script.failPath = ""
	box.tick(context.Background())
	if box.delivery(teamServerID) != "ok" {
		t.Fatalf("delivery = %s", box.delivery(teamServerID))
	}
	// The success sent every record. The reset is proved by the next record.
	sent := len(snapshotCalls(calls))
	appendOutbox(t, dir, sessionlog.Event{T: "user", Time: "2026-11-12T09:33:00Z", Text: "more"})
	script.failPath = "/v1/hello"
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
	box.tick(context.Background())
	if len(snapshotCalls(calls)) != 0 {
		t.Fatalf("calls = %+v", snapshotCalls(calls))
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
