package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/identity"
	"github.com/scimux/scimux/internal/team"
)

type seatReplayIdentitySeed struct {
	Seat       string `json:"seat"`
	Server     string `json:"server"`
	Owner      string `json:"owner"`
	Chat       string `json:"chat"`
	Generation string `json:"generation"`
	Identity   string `json:"identity"`
}

type seatReplayRecord struct {
	N              int             `json:"n"`
	T              float64         `json:"t"`
	Seat           string          `json:"seat"`
	Method         string          `json:"method"`
	Path           string          `json:"path"`
	Auth           string          `json:"auth"`
	Req            json.RawMessage `json:"req,omitempty"`
	Res            json.RawMessage `json:"res,omitempty"`
	ResBytes       json.RawMessage `json:"res_bytes,omitempty"`
	Status         int             `json:"status"`
	ResMissing     bool            `json:"res_missing"`
	Headers        http.Header     `json:"headers"`
	RequestHeaders http.Header     `json:"request_headers"`
	RequestHost    string          `json:"request_host"`
}

func seatReplayJSON(raw []byte) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var v any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&v) != nil {
		return nil
	}
	return v
}

func seatReplayDiff(want, got any, path string) string {
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
			if d := seatReplayDiff(w[k], v, path+"."+part); d != "" {
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
			if d := seatReplayDiff(w[i], g[i], fmt.Sprintf("%s[%d]", path, i)); d != "" {
				return d
			}
		}
		return ""
	case string:
		g, ok := got.(string)
		if !ok {
			return path
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

// selectReplayBody rejects incomplete or ambiguous recordings before comparing
// or serving them. Non-JSON bytes use the JSON base64 encoding of []byte.
func selectReplayBody(record seatReplayRecord) ([]byte, bool, error) {
	if record.ResMissing {
		return nil, false, fmt.Errorf("response missing")
	}
	jsonSet, rawSet := len(record.Res) > 0, len(record.ResBytes) > 0
	if record.Status == http.StatusNoContent || record.Status == http.StatusNotModified || (record.Status >= 100 && record.Status < 200) {
		if jsonSet || rawSet {
			return nil, false, fmt.Errorf("body forbidden")
		}
		return nil, false, nil
	}
	if jsonSet == rawSet {
		return nil, false, fmt.Errorf("response representation")
	}
	if jsonSet {
		if !json.Valid(record.Res) {
			return nil, false, fmt.Errorf("invalid JSON")
		}
		return record.Res, true, nil
	}
	if string(record.ResBytes) == "null" {
		return nil, false, fmt.Errorf("invalid raw bytes")
	}
	var b []byte
	if err := json.Unmarshal(record.ResBytes, &b); err != nil || b == nil {
		return nil, false, fmt.Errorf("invalid raw bytes")
	}
	return b, false, nil
}

func seatReplayResponseDiff(record seatReplayRecord, status int, body []byte) string {
	if status != record.Status {
		return "status"
	}
	expected, isJSON, err := selectReplayBody(record)
	if err != nil {
		return "response representation"
	}
	if !isJSON {
		if !bytes.Equal(expected, body) {
			return "body"
		}
		return ""
	}
	if !json.Valid(body) {
		return "invalid JSON"
	}
	return seatReplayDiff(seatReplayJSON(expected), seatReplayJSON(body), "$")
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
	errorText   string
	generations map[string]string
}

// seatReplayResponseHeaders restores end-to-end headers exactly. Framing is
// recomputed by net/http for the replayed body.
// Connection-nominated and hop-by-hop fields apply only to the recorded hop.
func seatReplayResponseHeaders(dst http.Header, recorded http.Header) {
	if recorded == nil {
		dst.Set("Content-Type", "application/json")
		return
	}
	excluded := map[string]bool{}
	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade", "Content-Length"} {
		excluded[http.CanonicalHeaderKey(name)] = true
	}
	for name, values := range recorded {
		if strings.EqualFold(name, "Connection") {
			for _, value := range values {
				for _, nominated := range strings.Split(value, ",") {
					excluded[http.CanonicalHeaderKey(strings.TrimSpace(nominated))] = true
				}
			}
		}
	}
	for name, values := range recorded {
		if !excluded[http.CanonicalHeaderKey(name)] {
			for _, value := range values {
				dst.Add(name, value)
			}
		}
	}
}

// The synthetic logical target never dials its advertised address; it can
// reach only this replay's own server. All other destinations and credentials
// fail before the underlying transport is called.
func seatReplayLogicalTransport(logical string, handler http.Handler, tokens map[string]bool) http.RoundTripper {
	return teamRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.User != nil || r.URL.Fragment != "" || r.URL.Scheme+"://"+r.URL.Host != logical {
			return nil, fmt.Errorf("replay target refused")
		}
		auth := r.Header.Values("Authorization")
		if len(auth) > 0 && (len(auth) != 1 || !strings.HasPrefix(auth[0], "Bearer ") || !tokens[strings.TrimPrefix(auth[0], "Bearer ")]) {
			return nil, fmt.Errorf("replay credential refused")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r.Clone(r.Context()))
		return w.Result(), nil
	})
}

func (f *seatReplayWire) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	got := seatReplayJSON(body)
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	defer f.mu.Unlock()
	requestPath := r.URL.RequestURI()
	if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/channels/") && len(strings.Split(r.URL.Path, "/")) == 5 && r.URL.RawQuery != "" {
		if r.URL.Query().Get("generation") != f.generations[r.URL.Path] || len(r.URL.Query()) != 1 {
			f.errorText = "incorrect delete generation"
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}
	for i := f.cursor; i < len(f.records); i++ {
		x := f.records[i]
		if f.used[i] || x.Method != r.Method || x.Path != requestPath || x.Auth != auth || x.RequestHost != r.Host || !seatReplayHeadersEqual(x.RequestHeaders, r.Header) {
			continue
		}
		if len(x.Req) == 0 || len(body) == 0 {
			if len(x.Req) != 0 || len(body) != 0 {
				continue
			}
		} else if !json.Valid(x.Req) || !json.Valid(body) || seatReplayDiff(seatReplayJSON(x.Req), got, "$") != "" {
			continue
		}
		responseBody, isJSON, bodyErr := selectReplayBody(x)
		if bodyErr != nil {
			f.errorText = "invalid recorded response"
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.used[i] = true
		// Keep the cursor at the first unused record: page polls can overlap.
		for f.cursor < len(f.records) && f.used[f.cursor] {
			f.cursor++
		}
		seatReplayResponseHeaders(w.Header(), x.Headers)
		w.WriteHeader(x.Status)
		var res any
		if isJSON {
			res = seatReplayJSON(responseBody)
		}
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

		}
		if len(responseBody) > 0 {
			_, _ = w.Write(responseBody)
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

// restoreReplayIdentity installs only a captured identity in an owned test
// directory, at the subscription persistence boundary; existing data wins.
func restoreReplayIdentity(root string, id team.CopyID, value string) error {
	if !subHex(id.Server, 32) || !subHex(id.Owner, 32) || !subHex(id.Chat, 16) || !subHex(id.Generation, 16) || !subHex(value, 32) {
		return fmt.Errorf("invalid replay identity")
	}
	existing, err := team.ReadPeopleCopyInstance(root, id)
	if err == nil {
		if existing != value {
			return fmt.Errorf("replay identity mismatch")
		}
		return nil
	}
	if !errors.Is(err, team.ErrPeopleStageMissing) {
		return err
	}
	dir := filepath.Join(root, team.CopiesDir, id.Server, id.Owner, id.Chat, id.Generation, "people-identity")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(value))
	record := team.PeopleStage{ID: "0000000000000000", Hash: hex.EncodeToString(sum[:]), Name: "identity", MIME: "text/plain", ConfigHash: strings.Repeat("0", 64), Data: []byte(value)}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "0000000000000000.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func seatReplayTimes(body any, path string, logs map[string]map[uint64]time.Time) bool {
	obj, ok := body.(map[string]any)
	if !ok {
		return false
	}
	entries, ok := obj["entries"].([]any)
	if !ok {
		return true
	}
	if logs[path] == nil {
		logs[path] = map[uint64]time.Time{}
	}
	for _, entry := range entries {
		row, ok := entry.(map[string]any)
		if !ok {
			return false
		}
		num, ok := row["seq"].(json.Number)
		if !ok {
			return false
		}
		seq, err := strconv.ParseUint(string(num), 10, 64)
		if err != nil {
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
	seqs := make([]uint64, 0, len(logs[path]))
	for seq := range logs[path] {
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for i := 1; i < len(seqs); i++ {
		if logs[path][seqs[i]].Before(logs[path][seqs[i-1]]) {
			return false
		}
	}
	return true
}

func TestRealTeamE2EReplay(t *testing.T) {
	paths := []string{"real-team-e2e-page.jsonl", "real-team-e2e-wire.jsonl", "real-team-e2e-owner-log.jsonl", "real-team-e2e-manifest.json"}
	missing := 0
	for _, p := range paths {
		_, err := os.Stat(filepath.Join("testdata", p))
		if os.IsNotExist(err) {
			missing++
			continue
		}
		if err != nil {
			t.Fatal("cannot access local recording")
		}
	}
	if missing == len(paths) {
		t.Skip("local team acceptance recording absent")
	}
	if missing != 0 {
		t.Fatal("local recording incomplete")
	}
	manifest, err := os.ReadFile(filepath.Join("testdata", paths[3]))
	var inventory struct {
		Version     int                      `json:"version"`
		PageRecords int                      `json:"page_records"`
		WireRecords int                      `json:"wire_records"`
		Identities  []seatReplayIdentitySeed `json:"copy_identities"`
	}
	if err != nil || json.Unmarshal(manifest, &inventory) != nil || inventory.Version != 2 {
		t.Fatal("invalid local manifest")
	}
	page := seatReplayRead(t, filepath.Join("testdata", paths[0]))
	wire := seatReplayRead(t, filepath.Join("testdata", paths[1]))
	if inventory.PageRecords < 1 || inventory.WireRecords < 1 || len(page) != inventory.PageRecords || len(wire) != inventory.WireRecords {
		t.Fatal("recording inventory mismatch")
	}
	for i, record := range page {
		if record.RequestHost == "" || record.RequestHeaders == nil || record.Headers == nil {
			t.Fatal("page header capture missing")
		}
		if record.N != i+1 {
			t.Fatal("page record order mismatch")
		}
	}
	for i, record := range wire {
		if record.RequestHost == "" || record.RequestHeaders == nil || record.Headers == nil {
			t.Fatal("wire header capture missing")
		}
		if record.N != i+1 {
			t.Fatal("wire record order mismatch")
		}
	}
	sort.SliceStable(page, func(i, j int) bool { return page[i].T < page[j].T })
	sort.SliceStable(wire, func(i, j int) bool { return wire[i].N < wire[j].N })
	f := &seatReplayWire{records: wire, used: make([]bool, len(wire))}
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
	// Every recorded wire request must be driven by a recorded page action or
	// outbox tick. Do not silently consume pre-join probes.
	if len(wire) > 0 && wire[0].T < firstJoin {
		t.Fatal("unreplayed pre-join wire record")
	}
	allowed := map[string]bool{}
	for i := 1; i <= 3; i++ {
		sum := sha256.Sum256([]byte(fmt.Sprintf("token-%d", i)))
		allowed[hex.EncodeToString(sum[:])] = true
	}
	const logicalURL = "http://127.0.0.1:23456"
	replayClient := func() *team.Client {
		c := team.NewClient()
		c.Transport = seatReplayLogicalTransport(logicalURL, f, allowed)
		return c
	}
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
	seeds := map[string]map[team.CopyID]string{}
	for _, seed := range inventory.Identities {
		if seed.Seat != "ann" && seed.Seat != "bob" && seed.Seat != "cy" {
			t.Fatal("invalid identity seat")
		}
		id := team.CopyID{Server: seed.Server, Owner: seed.Owner, Chat: seed.Chat, Generation: seed.Generation}
		if !subHex(id.Server, 32) || !subHex(id.Owner, 32) || !subHex(id.Chat, 16) || !subHex(id.Generation, 16) || !subHex(seed.Identity, 32) {
			t.Fatal("invalid identity seed")
		}
		if seeds[seed.Seat] == nil {
			seeds[seed.Seat] = map[team.CopyID]string{}
		}
		if _, exists := seeds[seed.Seat][id]; exists {
			t.Fatal("duplicate identity seed")
		}
		seeds[seed.Seat][id] = seed.Identity
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
		h, dir := testTeamSurface(t, replayClient())
		now := epoch
		inst, _ := json.Marshal(map[string]any{"v": 1, "id": reg.id})
		if err := os.WriteFile(filepath.Join(dir, identity.InstallationFile), inst, 0600); err != nil {
			t.Fatal("cannot seed installation")
		}
		th := asTeamHandler(t, h)
		th.now = func() time.Time { return now }
		box := &shareOutbox{dataDir: dir, now: func() time.Time { return now }, client: replayClient(), files: &th.mu}
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
	recordedTimes := map[string]map[uint64]time.Time{}
	liveTimes := map[string]map[uint64]time.Time{}
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
			got, host, headers := seatReplayPageRequest(solo, x.Method, x.Path, string(x.Req))
			if host != x.RequestHost || !seatReplayHeadersEqual(x.RequestHeaders, headers) {
				fail("request headers")
			}
			if !seatReplayHeadersEqual(x.Headers, seatReplayPageResponseHeaders(got)) {
				fail("response headers")
			}
			if x.Status < 400 {
				fail("status")
			}
			if d := seatReplayResponseDiff(x, got.Code, got.Body.Bytes()); d != "" {
				fail(d)
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
			obj["link"] = logicalURL + "/join#" + seat.code
		}
		var raw string
		if body != nil {
			b, e := json.Marshal(body)
			if e != nil {
				fail("request")
			}
			raw = string(b)
		}
		th := asTeamHandler(t, seat.handler)
		previousSave := th.saveSubs
		if x.Method == http.MethodGet && x.Path == "/api/team/subscriptions" && x.Status == http.StatusOK {
			identities := seeds[x.Seat]
			th.saveSubs = func(dir string, subs []team.Subscription) error {
				previous, err := team.LoadSubs(dir)
				if err != nil {
					return err
				}
				known := map[team.CopyID]bool{}
				for _, sub := range previous {
					known[team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}] = true
				}
				if err = team.SaveSubs(dir, subs); err != nil {
					return err
				}
				for _, sub := range subs {
					id := team.CopyID{Server: sub.Server, Owner: sub.Owner, Chat: sub.Chat, Generation: sub.Generation}
					if value, ok := identities[id]; ok && !known[id] {
						if err = restoreReplayIdentity(dir, id, value); err != nil {
							return err
						}
					}
				}
				return nil
			}
		}
		got, host, headers := seatReplayPageRequest(seat.handler, x.Method, x.Path, raw)
		th.saveSubs = previousSave
		if host != x.RequestHost || !seatReplayHeadersEqual(x.RequestHeaders, headers) {
			fail("request headers")
		}
		if !seatReplayHeadersEqual(x.Headers, seatReplayPageResponseHeaders(got)) {
			fail("response headers")
		}
		f.mu.Lock()
		problem := f.errorText
		f.mu.Unlock()
		if problem != "" {
			t.Fatalf("page %d %s %s %s: %s", x.N, x.Seat, x.Method, path, problem)
		}
		if d := seatReplayResponseDiff(x, got.Code, got.Body.Bytes()); d != "" {
			fail(d)
		}
		if x.Status != http.StatusNoContent {
			expected, actual := seatReplayJSON(x.Res), seatReplayJSON(got.Body.Bytes())
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

func seatReplayHeadersEqual(want, got http.Header) bool {
	if want == nil || got == nil {
		return want == nil && got == nil
	}
	normalize := func(in http.Header) (http.Header, bool) {
		out := http.Header{}
		for name, values := range in {
			key := http.CanonicalHeaderKey(name)
			if _, exists := out[key]; exists {
				return nil, false
			}
			out[key] = append([]string(nil), values...)
		}
		return out, true
	}
	a, ok := normalize(want)
	if !ok {
		return false
	}
	b, ok := normalize(got)
	return ok && reflect.DeepEqual(a, b)
}
func seatReplayPageRequest(h http.Handler, method, path, body string) (*httptest.ResponseRecorder, string, http.Header) {
	var host string
	var headers http.Header
	wrapper := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host = r.Host
		headers = r.Header.Clone()
		h.ServeHTTP(w, r)
	})
	return doTeam(wrapper, method, path, body), host, headers
}

func seatReplayPageResponseHeaders(w *httptest.ResponseRecorder) http.Header {
	return w.Result().Header.Clone()
}
