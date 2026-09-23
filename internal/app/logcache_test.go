package app

// Stage A — app-level LogCache wiring: one walk per unchanged poll cycle
// across /api/state and chat (sites that previously walked independently).

import (
	"bytes"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/sessionlog"
)

// A3. Zero walks on an unchanged file across a simulated poll cycle:
// two consecutive /api/state renders plus a chat render for an unchanged
// node parse the log at most once total.
func TestLogCache_UnchangedPollCycleOneWalk(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := &Node{
		ID: "walk-1", Title: "w", Agent: "claude", Model: "sonnet",
		Dir: "/tmp", CreatedAt: "2026-07-14T00:00:00Z",
	}
	a.nodes = []*Node{n}
	a.byID[n.ID] = n
	a.live[n.ID] = "quiet"

	writeSessionLog(t, a, n.ID, []sessionlog.Event{
		sessionlog.NewMeta(n.ID, "claude", "sonnet", "", a.home),
		{T: "user", Time: "2026-07-15T09:00:00Z", Text: "hello"},
		{T: "assistant", Time: "2026-07-15T09:00:01Z", Text: "[report](/outside/report.md)"},
		{T: "usage", Time: "2026-07-15T09:00:02Z", Usage: &sessionlog.UsageEvent{
			Used: 1000, Size: 200_000,
			InputTokens: 10, OutputTokens: 20, TurnID: "t1", CostAmount: 0.01,
		}},
		{T: "asset", Time: "2026-07-15T09:00:03Z", Asset: &sessionlog.AssetEvent{
			ID: "a1", Name: "x.png", Mime: "image/png", Size: 4,
			Storage: "inline", Bytes: "AAAA", SourceKind: "upload", SourcePath: "/tmp/x.png",
		}},
		sessionlog.NewAssetImport(sessionlog.AssetImportEvent{
			TurnRecord: 2, Occurrence: 0, Ref: "/outside/report.md", Alt: "report", Reason: "outside_workspace",
		}),
	})

	// Two state polls: segment + fare (+ rides) all share one walk.
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		a.handleState(rec, httptest.NewRequest("GET", "/api/state", nil))
		if rec.Code != 200 {
			t.Fatalf("state poll %d: code = %d body %q", i, rec.Code, rec.Body.String())
		}
	}

	// Two chat polls: segment + anchored assets + blocked imports + assets must
	// all hit the same cache. In particular, blocked imports must not call the
	// standalone full-log reader on every projection.
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/nodes/"+n.ID+"/chat", nil)
		r.SetPathValue("id", n.ID)
		a.handleChat(rec, r)
		if rec.Code != 200 {
			t.Fatalf("chat poll %d: code = %d body %q", i, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "scimux-import:2:0:outside_workspace") {
			t.Fatalf("chat poll %d omitted cached blocked import: %q", i, rec.Body.String())
		}
	}

	walks := logCacheWalks(a, n.ID)
	if walks != 1 {
		t.Fatalf("poll cycle Walks() = %d, want 1 (two state + two chat polls on unchanged log)", walks)
	}
}

// Preserving the cache identity while changing bytes is a test-only read spy:
// a caller that bypasses LogCache and reopens the file observes the replacement,
// while a caller using the unchanged cache snapshot observes the original. This
// proves blocked-import projection does not hide a second full-history scan
// behind an unchanged Walks count.
func TestProjectTurns_UnchangedPollDoesNotReopenAssetImports(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	path := a.sessionLogPath("walk-import")
	writeSessionLog(t, a, "walk-import", []sessionlog.Event{
		{T: "assistant", Text: "[report](/outside/report.md)"},
		sessionlog.NewAssetImport(sessionlog.AssetImportEvent{
			TurnRecord: 0, Occurrence: 0, Ref: "/outside/report.md", Reason: "outside_workspace",
		}),
	})
	turns := a.sessionLogCache("walk-import").Segment(path).Turns
	first, _ := a.projectTurns("walk-import", turns)
	if len(first) != 1 || !strings.Contains(first[0].Text, "outside_workspace") {
		t.Fatalf("first projection = %+v", first)
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rewritten := bytes.Replace(body, []byte("outside_workspace"), []byte("unreadable_______"), 1)
	if len(rewritten) != len(body) || bytes.Equal(rewritten, body) {
		t.Fatal("test replacement must change bytes without changing size")
	}
	if err := os.WriteFile(path, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}

	second, _ := a.projectTurns("walk-import", turns)
	if len(second) != 1 || !strings.Contains(second[0].Text, "outside_workspace") || strings.Contains(second[0].Text, "unreadable_______") {
		t.Fatalf("unchanged poll reopened session history: %+v", second)
	}
	if walks := logCacheWalks(a, "walk-import"); walks != 1 {
		t.Fatalf("unchanged projection Walks() = %d, want 1", walks)
	}
}
