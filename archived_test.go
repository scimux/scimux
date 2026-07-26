package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
)

// archivedResp mirrors the /api/archived JSON so tests read fields by name.
type archivedResp struct {
	UID             string `json:"uid"`
	Title           string `json:"title"`
	Agent           string `json:"agent"`
	Model           string `json:"model"`
	Effort          string `json:"effort"`
	Dir             string `json:"dir"`
	Forkable        bool   `json:"forkable"`
	Anchor          int    `json:"anchor"`
	BeforeTruncated bool   `json:"before_truncated"`
	AfterTruncated  bool   `json:"after_truncated"`
	Turns           []struct {
		Role string `json:"role"`
		Text string `json:"text"`
		Time string `json:"time"`
	} `json:"turns"`
}

func doArchived(t *testing.T, a *app, uid, at string) (int, archivedResp) {
	t.Helper()
	rec := httptest.NewRecorder()
	u := "/api/archived?uid=" + uid + "&at=" + at
	a.handleArchived(rec, httptest.NewRequest("GET", u, nil))
	var out archivedResp
	if rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode archived body: %v (%s)", err, rec.Body.String())
		}
	}
	return rec.Code, out
}

// A deleted chat's on-disk log serves a bounded window of turns around the hit,
// anchored by the hit's turn time, with truncation flags when turns fall outside
// the window. The response also carries the meta's launch config for fork.
func TestHandleArchivedWindowsAroundHit(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)

	evs := []sessionlog.Event{sessionlog.NewMeta("dead", "codex", "sonnet", "high", a.home)}
	for i := 0; i < 25; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		text := fmt.Sprintf("ordinary line %d", i)
		if i == 12 {
			text = "the needle is here"
		}
		evs = append(evs, sessionlog.Event{T: role, Text: text, Time: fmt.Sprintf("2026-07-10T%02d:00:00Z", i)})
	}
	appendLog(t, a, filepath.Join("archive", "dead.jsonl"), evs...)

	// Find the uid + the hit's turn time via the search path (the real caller).
	out := doSearch(t, a, "needle")
	if len(out.Groups) != 1 || len(out.Groups[0].Hits) != 1 {
		t.Fatalf("search setup: groups=%+v", out.Groups)
	}
	uid := out.Groups[0].UID
	at := out.Groups[0].Hits[0].Time
	if uid == "" {
		t.Fatal("archived group carried no uid")
	}

	code, ar := doArchived(t, a, uid, at)
	if code != 200 {
		t.Fatalf("archived code = %d", code)
	}
	// window = 8 before + anchor + 8 after → 17 turns, anchor at local index 8.
	if len(ar.Turns) != 17 {
		t.Fatalf("window size = %d, want 17", len(ar.Turns))
	}
	if ar.Anchor != 8 {
		t.Errorf("anchor local index = %d, want 8", ar.Anchor)
	}
	if ar.Turns[ar.Anchor].Text != "the needle is here" {
		t.Errorf("anchor turn = %q, want the needle line", ar.Turns[ar.Anchor].Text)
	}
	// turn 12 with 8 on each side leaves turns 0-3 before and 21-24 after unshown.
	if !ar.BeforeTruncated || !ar.AfterTruncated {
		t.Errorf("truncation flags = before %v after %v, want both true", ar.BeforeTruncated, ar.AfterTruncated)
	}
	// Launch config from the meta header rides along for the fork step — including
	// effort, so a fork from a deleted codex chat keeps its reasoning level (G5c).
	if ar.Agent != "codex" || ar.Model != "sonnet" || ar.Effort != "high" || ar.Dir != a.home {
		t.Errorf("launch config = %+v", ar)
	}
	if !ar.Forkable {
		t.Error("a meta with agent + existing dir should be forkable")
	}
}

// An empty `at` anchors at the start of the log (window from turn zero).
func TestHandleArchivedNoAnchor(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	appendLog(t, a, filepath.Join("archive", "dead.jsonl"),
		sessionlog.NewMeta("dead", "claude", "", "", a.home),
		sessionlog.Event{T: "user", Text: "first needle", Time: "2026-07-10T00:00:00Z"},
		sessionlog.Event{T: "assistant", Text: "second line", Time: "2026-07-10T01:00:00Z"},
	)
	uid := doSearch(t, a, "needle").Groups[0].UID
	code, ar := doArchived(t, a, uid, "")
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	if ar.Anchor != 0 || len(ar.Turns) != 2 || ar.BeforeTruncated {
		t.Errorf("no-anchor window = %+v", ar)
	}
}

// A header-less (legacy) log is servable when its path resolves inside the
// archive dir, but a legacy uid whose path escapes the archive dir is refused —
// the embedded path is never trusted raw (traversal guard).
func TestHandleArchivedLegacyConfined(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	appendLog(t, a, filepath.Join("archive", "ancient.jsonl"),
		sessionlog.Event{T: "user", Text: "legacy needle", Time: "2026-07-10T00:00:00Z"},
	)
	// The real legacy uid the search path emits for a header-less log.
	legacyUID := "legacy:" + filepath.Join(a.sessionsDir, "archive", "ancient.jsonl")
	if code, ar := doArchived(t, a, legacyUID, ""); code != 200 || len(ar.Turns) != 1 {
		t.Fatalf("legacy log should serve: code=%d turns=%d", code, len(ar.Turns))
	} else if ar.Forkable {
		t.Error("a header-less legacy log must be non-forkable")
	}

	// A traversal attempt must not escape the archive directory.
	esc := "legacy:" + filepath.Join(a.sessionsDir, "archive", "..", "..", "secret.jsonl")
	if code, _ := doArchived(t, a, esc, ""); code != 404 {
		t.Errorf("path-escaping legacy uid code = %d, want 404", code)
	}
}

// An unknown uid is a clean 404, never a 500 or a leak.
func TestHandleArchivedNotFound(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	appendLog(t, a, filepath.Join("archive", "dead.jsonl"),
		sessionlog.NewMeta("dead", "claude", "", "", a.home),
		sessionlog.Event{T: "user", Text: "hello", Time: "2026-07-10T00:00:00Z"},
	)
	if code, _ := doArchived(t, a, "nope-not-a-real-uid", ""); code != 404 {
		t.Errorf("unknown uid code = %d, want 404", code)
	}
	if code, _ := doArchived(t, a, "", ""); code != 400 {
		t.Errorf("missing uid code = %d, want 400", code)
	}
}
