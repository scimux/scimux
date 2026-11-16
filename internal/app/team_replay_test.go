package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/identity"
)

type seatReplayRecord struct {
	N          int             `json:"n"`
	T          float64         `json:"t"`
	Seat       string          `json:"seat"`
	Method     string          `json:"method"`
	Path       string          `json:"path"`
	Auth       string          `json:"auth"`
	Req        json.RawMessage `json:"req"`
	Res        json.RawMessage `json:"res"`
	Status     int             `json:"status"`
	ResMissing bool            `json:"res_missing"`
}

// The local fake server has a different URL from the recorded proxy.
var seatReplayExceptions = []string{
	"The fake server URL replaces the recorded proxy URL.",
	"Channel DELETE checks the last observed generation and matches without its query because the recording predates conditional deletion.",
}

func seatReplayJSON(raw []byte) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v
}

func seatReplayDiff(want, got any, path string, url string) string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok || len(w) != len(g) {
			return path
		}
		keys := make([]string, 0, len(w))
		for k := range w {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v, ok := g[k]
			part := k
			if strings.Contains(path, ".body") {
				part = "<field>"
			}
			if !ok {
				return path + "." + part
			}
			if k == "url" && url != "" {
				if v != url {
					return path + ".url"
				}
				continue
			}
			if d := seatReplayDiff(w[k], v, path+"."+part, url); d != "" {
				return d
			}
		}
		return ""
	case []any:
		g, ok := got.([]any)
		if !ok || len(w) != len(g) {
			return path
		}
		for i := range w {
			if d := seatReplayDiff(w[i], g[i], fmt.Sprintf("%s[%d]", path, i), url); d != "" {
				return d
			}
		}
		return ""
	case string:
		g, ok := got.(string)
		if !ok {
			return path
		}
		if _, err := time.Parse(time.RFC3339Nano, w); err == nil {
			if _, err = time.Parse(time.RFC3339Nano, g); err != nil {
				return path
			}
			return ""
		}
		if w != g {
			return path
		}
		return ""
	default:
		if !reflect.DeepEqual(want, got) {
			return path
		}
		return ""
	}
}

func seatReplayRead(t *testing.T, path string) []seatReplayRecord {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal("cannot read local recording")
	}
	defer f.Close()
	var out []seatReplayRecord
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 4096), 16<<20)
	for scan.Scan() {
		var x seatReplayRecord
		if json.Unmarshal(scan.Bytes(), &x) != nil {
			t.Fatal("invalid local recording")
		}
		out = append(out, x)
	}
	if scan.Err() != nil {
		t.Fatal("cannot read local recording")
	}
	return out
}
func seatReplayPath(path string) string {
	p := strings.SplitN(path, "?", 2)[0]
	if strings.HasPrefix(p, "/api/team/shares/") {
		return "/api/team/shares/{node}"
	}
	if strings.HasPrefix(p, "/api/team/subscriptions/") {
		return "/api/team/subscriptions/{address}"
	}
	if strings.HasPrefix(p, "/v1/channels/") {
		return "/v1/channels/{address}"
	}
	return p
}

type seatReplayWire struct {
	mu          sync.Mutex
	records     []seatReplayRecord
	used        []bool
	cursor      int
	tokens      map[string]string
	errorText   string
	generations map[string]string
}

func (f *seatReplayWire) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	got := seatReplayJSON(body)
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if alias, ok := f.tokens[auth]; ok {
		auth = alias
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	requestPath := r.URL.RequestURI()
	if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/channels/") && len(strings.Split(r.URL.Path, "/")) == 5 && r.URL.RawQuery != "" {
		if r.URL.Query().Get("generation") != f.generations[r.URL.Path] || len(r.URL.Query()) != 1 {
			f.errorText = "incorrect delete generation"
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		requestPath = r.URL.Path
	}
	for i := f.cursor; i < len(f.records); i++ {
		x := f.records[i]
		if f.used[i] || x.Method != r.Method || x.Path != requestPath || x.Auth != auth {
			continue
		}
		if seatReplayDiff(seatReplayJSON(x.Req), got, "$", "") != "" {
			continue
		}
		f.used[i] = true
		// Keep the cursor at the first unused record: page polls can overlap.
		for f.cursor < len(f.records) && f.used[f.cursor] {
			f.cursor++
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(x.Status)
		res := seatReplayJSON(x.Res)
		if obj, ok := res.(map[string]any); ok {
			if f.generations == nil {
				f.generations = map[string]string{}
			}
			remember := func(ch map[string]any) {
				owner, a := ch["owner"].(string)
				chat, b := ch["chat"].(string)
				gen, c := ch["generation"].(string)
				if a && b && c {
					f.generations["/v1/channels/"+owner+"/"+chat] = gen
				}
			}
			remember(obj)
			if rows, ok := obj["channels"].([]any); ok {
				for _, item := range rows {
					if ch, ok := item.(map[string]any); ok {
						remember(ch)
					}
				}
			}
			if alias, ok := obj["token"].(string); ok {
				for token, a := range f.tokens {
					if a == alias {
						obj["token"] = token
						break
					}
				}
			}
		}
		if res != nil {
			raw, _ := json.Marshal(res)
			_, _ = w.Write(raw)
		}
		return
	}
	f.errorText = fmt.Sprintf("unmatched team request %s %s", r.Method, seatReplayPath(r.URL.Path))
	w.WriteHeader(http.StatusInternalServerError)
}

type seatReplaySeat struct {
	handler http.Handler
	dir     string
	now     *time.Time
	box     *shareOutbox
	code    string
}

func seatReplayTimes(body any, path string, logs map[string]map[float64]time.Time) bool {
	obj, ok := body.(map[string]any)
	if !ok {
		return false
	}
	entries, ok := obj["entries"].([]any)
	if !ok {
		return true
	}
	if logs[path] == nil {
		logs[path] = map[float64]time.Time{}
	}
	for _, entry := range entries {
		row, ok := entry.(map[string]any)
		if !ok {
			return false
		}
		seq, ok := row["seq"].(float64)
		if !ok {
			return false
		}
		stamp, ok := row["at"].(string)
		if !ok {
			return false
		}
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			return false
		}
		logs[path][seq] = at
	}
	seqs := make([]float64, 0, len(logs[path]))
	for seq := range logs[path] {
		seqs = append(seqs, seq)
	}
	sort.Float64s(seqs)
	for i := 1; i < len(seqs); i++ {
		if logs[path][seqs[i]].Before(logs[path][seqs[i-1]]) {
			return false
		}
	}
	return true
}

func TestRealTeamE2EReplay(t *testing.T) {
	paths := []string{"real-team-e2e-page.jsonl", "real-team-e2e-wire.jsonl", "real-team-e2e-owner-log.jsonl", "real-team-e2e-manifest.json"}
	for _, p := range paths {
		_, err := os.Stat(filepath.Join("testdata", p))
		if os.IsNotExist(err) {
			t.Skip("local team acceptance recording absent")
		}
		if err != nil {
			t.Fatal("cannot access local recording")
		}
	}
	manifest, err := os.ReadFile(filepath.Join("testdata", paths[3]))
	if err != nil || !json.Valid(manifest) {
		t.Fatal("invalid local manifest")
	}
	page := seatReplayRead(t, filepath.Join("testdata", paths[0]))
	wire := seatReplayRead(t, filepath.Join("testdata", paths[1]))
	sort.SliceStable(page, func(i, j int) bool { return page[i].T < page[j].T })
	sort.SliceStable(wire, func(i, j int) bool { return wire[i].N < wire[j].N })
	f := &seatReplayWire{records: wire, used: make([]bool, len(wire)), tokens: map[string]string{}}
	firstJoin := 0.0
	for _, x := range page {
		if x.Method == http.MethodPost && x.Path == "/api/team/servers" {
			firstJoin = x.T
			break
		}
	}
	if firstJoin == 0 {
		t.Fatal("recording has no join")
	}
	// Before any seat has joined, only the acceptance's server probes run.
	for f.cursor < len(wire) && wire[f.cursor].T < firstJoin {
		probe := wire[f.cursor]
		if probe.Method != http.MethodGet || (probe.Path != "/" && probe.Path != "/v1/hello") || probe.Auth != "" {
			t.Fatal("unexpected pre-join wire call")
		}
		f.used[f.cursor] = true
		f.cursor++
	}
	for i := 1; i <= 3; i++ {
		alias := fmt.Sprintf("token-%d", i)
		sum := sha256.Sum256([]byte(alias))
		f.tokens[hex.EncodeToString(sum[:])] = alias
	}
	srv := httptest.NewServer(f)
	defer srv.Close()
	type registration struct{ id, code, name string }
	var regs []registration
	for _, x := range wire {
		if x.Method == http.MethodPost && x.Path == "/v1/register" {
			obj, _ := seatReplayJSON(x.Req).(map[string]any)
			id, _ := obj["installation_id"].(string)
			code, _ := obj["join_code"].(string)
			name, _ := obj["name"].(string)
			regs = append(regs, registration{id, code, name})
		}
	}
	if len(regs) != 3 {
		t.Fatal("recording registration count")
	}
	seats := map[string]*seatReplaySeat{}
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, s := range []string{"ann", "bob", "cy"} {
		var name string
		for _, x := range page {
			if x.Seat == s && x.Method == http.MethodPost && x.Path == "/api/team/servers" {
				obj, _ := seatReplayJSON(x.Req).(map[string]any)
				name, _ = obj["name"].(string)
				break
			}
		}
		var reg registration
		for _, r := range regs {
			if r.name == name {
				reg = r
				break
			}
		}
		if reg.id == "" || reg.code == "" {
			t.Fatal("cannot associate registered seat")
		}
		h, dir := testTeamSurface(t, clientFor(srv))
		now := epoch
		inst, _ := json.Marshal(map[string]any{"v": 1, "id": reg.id})
		if err := os.WriteFile(filepath.Join(dir, identity.InstallationFile), inst, 0600); err != nil {
			t.Fatal("cannot seed installation")
		}
		th := asTeamHandler(t, h)
		th.now = func() time.Time { return now }
		box := &shareOutbox{dataDir: dir, now: func() time.Time { return now }, client: clientFor(srv), files: &th.mu}
		th.shareDelivery = box.delivery
		seats[s] = &seatReplaySeat{handler: h, dir: dir, now: &now, box: box, code: reg.code}
	}
	ownerRaw, err := os.ReadFile(filepath.Join("testdata", paths[2]))
	if err != nil {
		t.Fatal("cannot read owner log")
	}
	var lines [][]byte
	var node, title string
	for _, line := range bytes.Split(bytes.TrimSpace(ownerRaw), []byte("\n")) {
		var ev map[string]any
		if json.Unmarshal(line, &ev) != nil {
			t.Fatal("invalid owner log")
		}
		if ev["t"] == "meta" {
			meta, _ := ev["meta"].(map[string]any)
			node, _ = meta["node"].(string)
		}
		lines = append(lines, append([]byte(nil), line...))
	}
	for _, x := range wire {
		if x.Method == http.MethodPut && strings.HasPrefix(x.Path, "/v1/channels/") && !strings.HasSuffix(x.Path, "/members") {
			req, _ := seatReplayJSON(x.Req).(map[string]any)
			title, _ = req["title"].(string)
			break
		}
	}
	if node == "" || title == "" {
		t.Fatal("missing owner node")
	}
	appendNode(t, seats["ann"].dir, node, title)
	if err := os.MkdirAll(filepath.Join(seats["ann"].dir, "sessions"), 0700); err != nil {
		t.Fatal("cannot seed sessions")
	}
	logFile, err := os.OpenFile(chatLogPath(seats["ann"].dir, node), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal("cannot seed session")
	}
	defer logFile.Close()
	applied := 0
	advance := func(at float64) {
		now := epoch.Add(time.Duration(at * float64(time.Second)))
		for _, s := range seats {
			*s.now = now
		}
		for applied < len(lines) {
			var ev struct {
				Time string `json:"time"`
			}
			_ = json.Unmarshal(lines[applied], &ev)
			stamp, e := time.Parse(time.RFC3339Nano, ev.Time)
			if e != nil {
				t.Fatal("invalid owner time")
			}
			if stamp.After(now) {
				break
			}
			if _, e = logFile.Write(append(lines[applied], '\n')); e != nil {
				t.Fatal("cannot seed session")
			}
			applied++
		}
	}
	solo, err := newCoreMux(&app{})
	if err != nil {
		t.Fatal("cannot build gate-off surface")
	}
	var mirrors []seatReplayRecord
	for _, x := range wire {
		if x.Method == http.MethodPost && strings.HasSuffix(strings.SplitN(x.Path, "?", 2)[0], "/mirror") {
			mirrors = append(mirrors, x)
		}
	}
	mi := 0
	recordedTimes := map[string]map[float64]time.Time{}
	liveTimes := map[string]map[float64]time.Time{}
	for _, x := range page {
		if !strings.HasPrefix(x.Path, "/api/team/") {
			continue
		}
		for mi < len(mirrors) && mirrors[mi].T <= x.T {
			advance(mirrors[mi].T)
			seats["ann"].box.tick(context.Background())
			mi++
		}
		advance(x.T)
		path := seatReplayPath(x.Path)
		fail := func(where string) { t.Fatalf("page %d %s %s %s differs at %s", x.N, x.Seat, x.Method, path, where) }
		if x.Seat == "solo" {
			got := doTeam(solo, x.Method, x.Path, "")
			if x.Status < 400 || got.Code != x.Status {
				fail("status")
			}
			continue
		}
		seat, ok := seats[x.Seat]
		if !ok {
			fail("seat")
		}
		body := seatReplayJSON(x.Req)
		if x.Method == http.MethodPost && x.Path == "/api/team/servers" {
			obj, ok := body.(map[string]any)
			if !ok {
				fail("request")
			}
			obj["link"] = srv.URL + "/join#" + seat.code
		}
		var raw string
		if body != nil {
			b, e := json.Marshal(body)
			if e != nil {
				fail("request")
			}
			raw = string(b)
		}
		got := doTeam(seat.handler, x.Method, x.Path, raw)
		f.mu.Lock()
		problem := f.errorText
		f.mu.Unlock()
		if problem != "" {
			t.Fatalf("page %d %s %s %s: %s", x.N, x.Seat, x.Method, path, problem)
		}
		if got.Code != x.Status {
			fail("status")
		}
		if x.Status != http.StatusNoContent && !x.ResMissing {
			expected, actual := seatReplayJSON(x.Res), seatReplayJSON(got.Body.Bytes())
			if d := seatReplayDiff(expected, actual, "$", srv.URL); d != "" {
				fail(d)
			}
			if x.Method == http.MethodGet && strings.HasPrefix(x.Path, "/api/team/subscriptions/") {
				key := strings.SplitN(x.Path, "?", 2)[0]
				if !seatReplayTimes(expected, key, recordedTimes) || !seatReplayTimes(actual, key, liveTimes) {
					fail("$.entries.at")
				}
			}
		}
	}
	if mi != len(mirrors) {
		t.Fatal("unreplayed outbox append")
	}
	var unused []string
	for i, x := range f.records {
		if !f.used[i] {
			unused = append(unused, fmt.Sprintf("%d %s %s", x.N, x.Method, seatReplayPath(x.Path)))
		}
	}
	if len(unused) > 0 {
		t.Fatalf("unused wire records: %s", strings.Join(unused, "; "))
	}
}
