package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/sessionlog"
)

// The global total-hit budget caps hits ENTERING groups across files, not just
// per file: without trimming the boundary file's result, the response overshoots
// searchMaxHitsTotal by up to a full per-file batch. 27 chats × 19 hits = 513
// available; the budget must cap the total that enters groups at exactly 500. 27
// groups stays under searchMaxGroups and 19 under searchHitsPerGroup, so neither
// the group cap nor the per-group cap masks the total-hit trim.
func TestHandleSearchTotalHitCap(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	const files, per = 27, 19
	for i := 0; i < files; i++ {
		id := fmt.Sprintf("n%02d", i)
		liveNode(a, &Node{ID: id, Title: id, Agent: "claude", Dir: a.home,
			CreatedAt: fmt.Sprintf("2026-07-14T%02d:00:00Z", i)})
		evs := []sessionlog.Event{sessionlog.NewMeta(id, "claude", "", "", a.home)}
		for j := 0; j < per; j++ {
			evs = append(evs, sessionlog.Event{T: "user", Text: "widget line",
				Time: fmt.Sprintf("2026-07-14T%02d:%02d:00Z", i, j)})
		}
		appendLog(t, a, id+".jsonl", evs...)
	}
	out := doSearch(t, a, "widget")
	total := 0
	for _, g := range out.Groups {
		for _, h := range g.Hits {
			if h.Role != "bookmark" {
				total++
			}
		}
	}
	if total != searchMaxHitsTotal {
		t.Errorf("total log hits = %d, want the global cap %d (overshoots to %d without the trim)",
			total, searchMaxHitsTotal, files*per)
	}
	if !out.Partial {
		t.Error("partial = false, want true when the total-hit cap trims")
	}
}

// An aborted request stops the scan server-side, not just in the browser: with an
// already-cancelled context the handler returns a partial, empty result instead
// of scanning the catalog.
func TestHandleSearchAbortStopsScan(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	liveNode(a, &Node{ID: "alpha", Title: "Alpha", Agent: "claude", Dir: a.home})
	appendLog(t, a, "alpha.jsonl",
		sessionlog.NewMeta("alpha", "claude", "", "", a.home),
		sessionlog.Event{T: "user", Text: "widget", Time: "2026-07-14T00:00:00Z"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/search?q=widget", nil).WithContext(ctx)
	a.handleSearch(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code = %d, body %s", rec.Code, rec.Body.String())
	}
	var out searchResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Partial {
		t.Error("partial = false, want true for an aborted scan")
	}
	if len(out.Groups) != 0 {
		t.Errorf("groups = %d, want 0 (scan stopped before any file)", len(out.Groups))
	}
}

// searchResp mirrors the /api/search JSON so tests read fields by name.
type searchResp struct {
	Query   string `json:"query"`
	Partial bool   `json:"partial"`
	Groups  []struct {
		Kind     string `json:"kind"`
		ID       string `json:"id"`
		UID      string `json:"uid"`
		Title    string `json:"title"`
		LaneID   string `json:"lane_id"`
		Agent    string `json:"agent"`
		Forkable bool   `json:"forkable"`
		Hits     []struct {
			Role       string `json:"role"`
			Segment    int    `json:"segment"`
			Record     int    `json:"record"`
			UID        string `json:"uid"`
			BookmarkID string `json:"bookmark_id"`
			Time       string `json:"time"`
			TurnTime   string `json:"turn_time"`
			Before     string `json:"before"`
			Match      string `json:"match"`
			After      string `json:"after"`
		} `json:"hits"`
	} `json:"groups"`
}

func doSearch(t *testing.T, a *app, q string) searchResp {
	t.Helper()
	rec := httptest.NewRecorder()
	a.handleSearch(rec, httptest.NewRequest("GET", "/api/search?q="+q, nil))
	if rec.Code != 200 {
		t.Fatalf("search %q code = %d, body %s", q, rec.Code, rec.Body.String())
	}
	var out searchResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode search body: %v (%s)", err, rec.Body.String())
	}
	return out
}

func appendLog(t *testing.T, a *app, file string, evs ...sessionlog.Event) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(a.sessionsDir, file)), 0o700); err != nil {
		t.Fatal(err)
	}
	w := &sessionlog.Writer{Path: filepath.Join(a.sessionsDir, file)}
	for _, ev := range evs {
		if err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
}

func liveNode(a *app, n *Node) {
	a.nodes = append(a.nodes, n)
	a.byID[n.ID] = n
}

// A live node's log hits surface under its group, groups order by newest match,
// hits within a group order newest-first, and Match carries the query casing.
func TestHandleSearchGroupsAndOrdering(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)

	liveNode(a, &Node{ID: "alpha", Title: "Alpha", Agent: "claude", Dir: a.home, LaneID: "lane-1", CreatedAt: "2026-07-14T00:00:00Z"})
	liveNode(a, &Node{ID: "beta", Title: "Beta", Agent: "codex", Dir: a.home, CreatedAt: "2026-07-14T00:00:00Z"})

	appendLog(t, a, "alpha.jsonl",
		sessionlog.NewMeta("alpha", "claude", "", "", a.home),
		sessionlog.Event{T: "user", Text: "how do WIDGETS work", Time: "2026-07-14T01:00:00Z"},
		sessionlog.Event{T: "assistant", Text: "widgets are simple", Time: "2026-07-14T02:00:00Z"},
	)
	appendLog(t, a, "beta.jsonl",
		sessionlog.NewMeta("beta", "codex", "", "", a.home),
		sessionlog.Event{T: "user", Text: "no widget here at all", Time: "2026-07-15T09:00:00Z"},
	)

	out := doSearch(t, a, "widget")
	if len(out.Groups) != 2 {
		t.Fatalf("groups = %d, want 2: %+v", len(out.Groups), out.Groups)
	}
	// beta's only match (07-15) is newer than alpha's newest (07-14) → beta first.
	if out.Groups[0].ID != "beta" || out.Groups[1].ID != "alpha" {
		t.Fatalf("group order = %s,%s want beta,alpha", out.Groups[0].ID, out.Groups[1].ID)
	}
	alpha := out.Groups[1]
	if alpha.Kind != "live" || alpha.Title != "Alpha" || alpha.LaneID != "lane-1" || alpha.Agent != "claude" {
		t.Errorf("alpha header = %+v", alpha)
	}
	if len(alpha.Hits) != 2 {
		t.Fatalf("alpha hits = %d, want 2", len(alpha.Hits))
	}
	// newest-first within the group
	if alpha.Hits[0].Time != "2026-07-14T02:00:00Z" || alpha.Hits[1].Time != "2026-07-14T01:00:00Z" {
		t.Errorf("alpha hit order = %s,%s", alpha.Hits[0].Time, alpha.Hits[1].Time)
	}
	// Match is the query-length span in the turn's original casing: "WIDGET".
	if alpha.Hits[1].Match != "WIDGET" {
		t.Errorf("match casing = %q, want WIDGET", alpha.Hits[1].Match)
	}
	if !alpha.Forkable {
		t.Errorf("live node with existing dir should be forkable")
	}
}

// Archived logs join the corpus; a header-less log becomes legacy: and non-forkable.
func TestHandleSearchIncludesArchivedAndLegacy(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)

	appendLog(t, a, filepath.Join("archive", "dead.jsonl"),
		sessionlog.NewMeta("dead", "claude", "", "", a.home),
		sessionlog.Event{T: "user", Text: "the frobnicate ritual", Time: "2026-07-10T00:00:00Z"},
	)
	// No meta header → legacy identity, non-forkable.
	appendLog(t, a, filepath.Join("archive", "ancient.jsonl"),
		sessionlog.Event{T: "user", Text: "frobnicate again", Time: "2026-07-11T00:00:00Z"},
	)

	out := doSearch(t, a, "frobnicate")
	if len(out.Groups) != 2 {
		t.Fatalf("groups = %d, want 2: %+v", len(out.Groups), out.Groups)
	}
	var dead, legacy bool
	for _, g := range out.Groups {
		if g.Kind != "archived" {
			t.Errorf("group kind = %q, want archived", g.Kind)
		}
		switch {
		case g.UID != "" && g.UID[:7] == "legacy:":
			legacy = true
			if g.Forkable {
				t.Errorf("legacy (no-uid) group must not be forkable")
			}
			// The legacy uid must be a path relative to the archive dir, never the
			// absolute on-disk path (that would leak the local data directory).
			if g.UID != "legacy:ancient.jsonl" {
				t.Errorf("legacy uid = %q, want relative %q", g.UID, "legacy:ancient.jsonl")
			}
			if strings.Contains(g.UID, a.sessionsDir) {
				t.Errorf("legacy uid %q leaks the absolute data dir", g.UID)
			}
		case g.Title == "dead":
			dead = true
			if g.UID == "" || g.UID[:7] == "legacy:" {
				t.Errorf("dead group should have a real uid, got %q", g.UID)
			}
			if !g.Forkable {
				t.Errorf("archived log with meta + existing dir should be forkable")
			}
		}
	}
	if !dead || !legacy {
		t.Errorf("expected both dead and legacy groups, got dead=%v legacy=%v", dead, legacy)
	}
}

// Notes join the corpus: one anchored to a live chat folds into that group; a
// note with no owning chat lands in the "Notes" group. Neither is forkable.
func TestHandleSearchFoldsNotes(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	a.uiPath = filepath.Join(a.home, "ui.json")

	liveNode(a, &Node{ID: "alpha", Title: "Alpha", Agent: "claude", Dir: a.home, CreatedAt: "2026-07-14T00:00:00Z"})
	appendLog(t, a, "alpha.jsonl",
		sessionlog.NewMeta("alpha", "claude", "", "", a.home),
		sessionlog.Event{T: "user", Text: "plain chat text", Time: "2026-07-14T01:00:00Z"},
	)
	ui := `{"bookmarks":[
		{"t":"2026-07-16T00:00:00Z","text":"remember the sprocket","node":"alpha","turnTime":"2026-07-14T01:00:00Z"},
		{"t":"2026-07-17T00:00:00Z","text":"loose sprocket idea"}
	]}`
	if err := os.WriteFile(a.uiPath, []byte(ui), 0o600); err != nil {
		t.Fatal(err)
	}

	out := doSearch(t, a, "sprocket")
	if len(out.Groups) != 2 {
		t.Fatalf("groups = %d, want 2: %+v", len(out.Groups), out.Groups)
	}
	var foundInChat, foundInNotes bool
	for _, g := range out.Groups {
		for _, h := range g.Hits {
			if h.Role != "bookmark" {
				continue
			}
			if g.ID == "alpha" {
				foundInChat = true
				if h.BookmarkID != "2026-07-16T00:00:00Z" || h.TurnTime != "2026-07-14T01:00:00Z" {
					t.Errorf("chat note hit = %+v", h)
				}
			}
			if g.Kind == "bookmarks" {
				foundInNotes = true
				if g.Forkable {
					t.Errorf("Notes group must not be forkable")
				}
				if h.BookmarkID != "2026-07-17T00:00:00Z" {
					t.Errorf("notes-group hit = %+v", h)
				}
			}
		}
	}
	if !foundInChat || !foundInNotes {
		t.Errorf("expected note in chat and in Notes group, got chat=%v notes=%v", foundInChat, foundInNotes)
	}
}

// An asset filename hit is emitted with Role "asset" (so the client can hide
// fork/add-note) and anchored on the OWNING turn's time, not the asset record's
// own — the asset record is never rendered as a turn, so its time would never
// resolve a jump.
func TestHandleSearchAssetHitOwningTurn(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	liveNode(a, &Node{ID: "alpha", Title: "Alpha", Agent: "claude", Dir: a.home, CreatedAt: "2026-07-14T00:00:00Z"})
	appendLog(t, a, "alpha.jsonl",
		sessionlog.NewMeta("alpha", "claude", "", "", a.home),
		sessionlog.Event{T: "assistant", Text: "here is the chart", Time: "2026-07-14T02:00:00Z"},
		sessionlog.Event{T: "asset", Time: "2026-07-14T02:00:05Z",
			Asset: &sessionlog.AssetEvent{ID: "a_1", Name: "revenue-widget.png", Storage: "inline"}},
	)
	out := doSearch(t, a, "revenue-widget")
	if len(out.Groups) != 1 || len(out.Groups[0].Hits) != 1 {
		t.Fatalf("groups = %+v, want one asset hit", out.Groups)
	}
	h := out.Groups[0].Hits[0]
	if h.Role != "asset" {
		t.Errorf("hit role = %q, want asset", h.Role)
	}
	if h.Time != "2026-07-14T02:00:00Z" {
		t.Errorf("asset hit time = %q, want the owning assistant turn's time", h.Time)
	}
}

// The response layers global caps over ScanLog's per-file limits: an over-long
// query is clamped, and a single chat's hits are capped per group (marking the
// result partial) so one conversation can't flood the feed.
func TestHandleSearchGlobalCaps(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	a.uiPath = filepath.Join(a.home, "ui.json")

	liveNode(a, &Node{ID: "alpha", Title: "Alpha", Agent: "claude", Dir: a.home, CreatedAt: "2026-07-14T00:00:00Z"})
	evs := []sessionlog.Event{sessionlog.NewMeta("alpha", "claude", "", "", a.home)}
	// 15 log hits (below ScanLog's per-file cap of 20, so no per-file partial)…
	for i := 0; i < 15; i++ {
		evs = append(evs, sessionlog.Event{T: "user", Text: fmt.Sprintf("sprocket line %d", i),
			Time: fmt.Sprintf("2026-07-14T%02d:00:00Z", i)})
	}
	appendLog(t, a, "alpha.jsonl", evs...)
	// …plus 10 notes on the same node — together 25 hits fold into one group, over
	// the per-group cap of 20.
	notes := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		notes = append(notes, fmt.Sprintf(`{"t":"2026-07-15T%02d:00:00Z","text":"sprocket note %d","node":"alpha"}`, i, i))
	}
	if err := os.WriteFile(a.uiPath, []byte(`{"bookmarks":[`+strings.Join(notes, ",")+`]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	out := doSearch(t, a, "sprocket")
	if len(out.Groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(out.Groups))
	}
	if len(out.Groups[0].Hits) != searchHitsPerGroup {
		t.Errorf("group hits = %d, want the per-group cap %d", len(out.Groups[0].Hits), searchHitsPerGroup)
	}
	if !out.Partial {
		t.Error("partial = false, want true when the per-group cap truncates hits")
	}

	// Query-length clamp: a query longer than the cap is scanned as its prefix.
	long := ""
	for len(long) < maxSearchQuery+40 {
		long += "sprocket"
	}
	clamped := doSearch(t, a, long)
	if l := len([]rune(clamped.Query)); l != maxSearchQuery {
		t.Errorf("clamped query length = %d, want %d", l, maxSearchQuery)
	}
}

// Too-short / blank queries return an empty result, not an error — the field is
// searched live as the user types.
func TestHandleSearchShortQueryEmpty(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	liveNode(a, &Node{ID: "alpha", Title: "Alpha", Agent: "claude", Dir: a.home})
	appendLog(t, a, "alpha.jsonl",
		sessionlog.NewMeta("alpha", "claude", "", "", a.home),
		sessionlog.Event{T: "user", Text: "aaaa", Time: "2026-07-14T01:00:00Z"},
	)
	for _, q := range []string{"", "%20%20", "a"} {
		out := doSearch(t, a, q)
		if len(out.Groups) != 0 {
			t.Errorf("query %q returned %d groups, want 0", q, len(out.Groups))
		}
	}
}

// A captured note that stamped the durable source address (uid, segment,
// record) surfaces it on its search hit, so a jump can resolve the exact source
// turn across /clear seams and node deletion — not the fragile turnTime scan.
// The narrow defensive reader takes those fields and ignores the rest (Phase 0c).
func TestHandleSearchNoteCarriesDurableAddress(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	a.uiPath = filepath.Join(a.home, "ui.json")

	liveNode(a, &Node{ID: "alpha", Title: "Alpha", Agent: "claude", Dir: a.home, CreatedAt: "2026-07-14T00:00:00Z"})
	appendLog(t, a, "alpha.jsonl",
		sessionlog.NewMeta("alpha", "claude", "", "", a.home),
		sessionlog.Event{T: "user", Text: "plain chat text", Time: "2026-07-14T01:00:00Z"},
	)
	// A note with the full triple, plus one legacy note with none of it and an
	// unknown extra field the reader must ignore.
	ui := `{"bookmarks":[
		{"t":"2026-07-16T00:00:00Z","text":"remember the sprocket","node":"alpha","turnTime":"2026-07-14T01:00:00Z","uid":"abc123","segment":1,"record":4},
		{"t":"2026-07-17T00:00:00Z","text":"loose sprocket idea","bogus":true}
	]}`
	if err := os.WriteFile(a.uiPath, []byte(ui), 0o600); err != nil {
		t.Fatal(err)
	}

	out := doSearch(t, a, "sprocket")
	var addressed, legacy int
	for _, g := range out.Groups {
		for _, h := range g.Hits {
			if h.Role != "bookmark" {
				continue
			}
			switch h.BookmarkID {
			case "2026-07-16T00:00:00Z":
				addressed++
				if h.UID != "abc123" || h.Segment != 1 || h.Record != 4 {
					t.Errorf("addressed note hit = %+v, want uid abc123 seg 1 rec 4", h)
				}
			case "2026-07-17T00:00:00Z":
				legacy++
				if h.UID != "" || h.Segment != 0 || h.Record != 0 {
					t.Errorf("legacy note hit carried an address it never stored: %+v", h)
				}
			}
		}
	}
	if addressed != 1 || legacy != 1 {
		t.Fatalf("addressed=%d legacy=%d, want 1/1", addressed, legacy)
	}
}

// Malformed ui.json / unknown note shapes are ignored, never an error.
func TestHandleSearchDefensiveNotes(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	a.uiPath = filepath.Join(a.home, "ui.json")
	if err := os.WriteFile(a.uiPath, []byte(`{"bookmarks":"not an array","x":[1,2]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out := doSearch(t, a, "anything")
	if len(out.Groups) != 0 {
		t.Errorf("groups = %d, want 0", len(out.Groups))
	}
	// A missing ui.json is likewise fine.
	a.uiPath = filepath.Join(a.home, "nope.json")
	_ = doSearch(t, a, "anything")
}
