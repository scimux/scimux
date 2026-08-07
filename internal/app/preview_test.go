package app

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

// previewResp mirrors the /api/preview JSON so tests read fields by name.
type previewResp struct {
	UID string `json:"uid"`
	// Node names the live node owning this log, empty when the chat is deleted.
	// It is the whole of the live/deleted distinction the client can see.
	Node            string         `json:"node"`
	Title           string         `json:"title"`
	Agent           string         `json:"agent"`
	Model           string         `json:"model"`
	Assets          map[string]any `json:"assets"`
	Anchor          int            `json:"anchor"`
	BeforeTruncated bool           `json:"before_truncated"`
	AfterTruncated  bool           `json:"after_truncated"`
	Turns           []struct {
		Role string `json:"role"`
		Text string `json:"text"`
		Time string `json:"time"`
	} `json:"turns"`
}

func doPreview(t *testing.T, a *app, uid, at string) (int, previewResp) {
	t.Helper()
	return doPreviewURL(t, a, "/api/preview?uid="+uid+"&at="+at)
}

func doPreviewSegRec(t *testing.T, a *app, uid string, seg, rec int, at string) (int, previewResp) {
	t.Helper()
	return doPreviewURL(t, a, fmt.Sprintf("/api/preview?uid=%s&seg=%d&rec=%d&at=%s", uid, seg, rec, at))
}

func doPreviewURL(t *testing.T, a *app, u string) (int, previewResp) {
	t.Helper()
	code, out, _ := doPreviewRaw(t, a, u)
	return code, out
}

// doPreviewRaw also hands back the undecoded body, for assertions about fields
// that must NOT be there — a struct can only see what it declares.
func doPreviewRaw(t *testing.T, a *app, u string) (int, previewResp, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	a.handlePreview(rec, httptest.NewRequest("GET", u, nil))
	var out previewResp
	if rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode preview body: %v (%s)", err, rec.Body.String())
		}
	}
	return rec.Code, out, rec.Body.String()
}

// A deleted chat's on-disk log serves a bounded window of turns around the hit,
// anchored by the hit's turn time, with truncation flags when turns fall outside
// the window. The response also carries the meta's launch config for fork.
func TestPreviewWindowsAroundHit(t *testing.T) {
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

	code, ar := doPreview(t, a, uid, at)
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
	// Agent and model ride along from the meta header — they are the subtitle,
	// the one thing the head can say about a chat that no longer exists.
	if ar.Agent != "codex" || ar.Model != "sonnet" {
		t.Errorf("launch config = %+v", ar)
	}
	// Deleted: there is no node to go into, and the head must not offer one.
	if ar.Node != "" {
		t.Errorf("a deleted chat named node %q", ar.Node)
	}
}

// The window is anchored by the hit's stable (seg, rec) ordinal, not its
// timestamp: when several turns share one timestamp, only the ordinal picks the
// right one. Time anchoring would land on the first turn with that time.
func TestPreviewAnchorsBySegRec(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)

	// Five turns all stamped with the same time; the needle is the fourth.
	evs := []sessionlog.Event{sessionlog.NewMeta("dead", "codex", "sonnet", "high", a.home)}
	dupTime := "2026-07-10T00:00:00Z"
	for i := 0; i < 5; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		text := fmt.Sprintf("shared-time line %d", i)
		if i == 3 {
			text = "the needle is here"
		}
		evs = append(evs, sessionlog.Event{T: role, Text: text, Time: dupTime})
	}
	appendLog(t, a, filepath.Join("archive", "dead.jsonl"), evs...)

	out := doSearch(t, a, "needle")
	if len(out.Groups) != 1 || len(out.Groups[0].Hits) != 1 {
		t.Fatalf("search setup: groups=%+v", out.Groups)
	}
	hit := out.Groups[0].Hits[0]
	uid := out.Groups[0].UID

	// Anchor by ordinal — must land on the needle (the 4th turn, index 3), even
	// though four other turns share its timestamp.
	code, ar := doPreviewSegRec(t, a, uid, hit.Segment, hit.Record, hit.Time)
	if code != 200 {
		t.Fatalf("archived code = %d", code)
	}
	if ar.Turns[ar.Anchor].Text != "the needle is here" {
		t.Errorf("seg/rec anchor = %q, want the needle line", ar.Turns[ar.Anchor].Text)
	}

	// The old time-only anchoring would land on the FIRST shared-time turn — prove
	// the ordinal path did something different (a real correctness gain, not a tie).
	_, atOnly := doPreview(t, a, uid, dupTime)
	if atOnly.Turns[atOnly.Anchor].Text == "the needle is here" {
		t.Fatal("test is not exercising the ordinal path: time anchoring already lands on the needle")
	}
}

// An empty `at` anchors at the start of the log (window from turn zero).
func TestPreviewNoAnchor(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	appendLog(t, a, filepath.Join("archive", "dead.jsonl"),
		sessionlog.NewMeta("dead", "claude", "", "", a.home),
		sessionlog.Event{T: "user", Text: "first needle", Time: "2026-07-10T00:00:00Z"},
		sessionlog.Event{T: "assistant", Text: "second line", Time: "2026-07-10T01:00:00Z"},
	)
	uid := doSearch(t, a, "needle").Groups[0].UID
	code, ar := doPreview(t, a, uid, "")
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
func TestPreviewLegacyConfined(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	appendLog(t, a, filepath.Join("archive", "ancient.jsonl"),
		sessionlog.Event{T: "user", Text: "legacy needle", Time: "2026-07-10T00:00:00Z"},
	)
	// The real legacy uid the search path emits for a header-less log: a path
	// relative to the archive dir, not the absolute on-disk path.
	legacyUID := "legacy:ancient.jsonl"
	if code, ar := doPreview(t, a, legacyUID, ""); code != 200 || len(ar.Turns) != 1 {
		t.Fatalf("legacy log should serve: code=%d turns=%d", code, len(ar.Turns))
	} else if ar.Node != "" {
		t.Errorf("a header-less archived log named node %q", ar.Node)
	}

	// A traversal attempt must not escape the archive directory.
	esc := "legacy:" + filepath.Join("..", "..", "secret.jsonl")
	if code, _ := doPreview(t, a, esc, ""); code != 404 {
		t.Errorf("path-escaping legacy uid code = %d, want 404", code)
	}
}

// An unknown uid is a clean 404, never a 500 or a leak.
// TestNoteAddressResolvesAfterDeletion is the load-bearing Phase 0d contract:
// the durable address a note stamps at capture time — read straight off the
// chat read path (a.segment), exactly as the client copies it — resolves the
// same source turn through /api/preview after the source node is deleted and
// its log archived. This is what lets a notes-pane jump survive node deletion,
// where the old node+turnTime scan gave up entirely. It also proves the read
// path (0a) and the archived resolver name a turn by the same (uid, seg, rec).
func TestNoteAddressResolvesAfterDeletion(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	n := &Node{ID: "alpha", Title: "Alpha", Agent: "claude", CreatedAt: "2026-07-14T00:00:00Z"}
	liveNode(a, n)
	appendLog(t, a, "alpha.jsonl",
		sessionlog.NewMeta("alpha", "claude", "", "", a.home),
		sessionlog.Event{T: "user", Text: "before the clear", Time: "2026-07-14T01:00:00Z"},
		sessionlog.Event{T: "assistant", Text: "old answer", Time: "2026-07-14T01:01:00Z"},
		sessionlog.NewClearSource("s2"),
		sessionlog.Event{T: "user", Text: "the captured turn", Time: "2026-07-15T09:00:00Z"},
	)

	// The address a note would stamp: read the current segment and take the turn
	// the researcher captured. It lives in segment 1 (past the /clear seam).
	seg := a.segment(n)
	var captured transcript.Turn
	for _, tn := range seg.Turns {
		if tn.Text == "the captured turn" {
			captured = tn
		}
	}
	if captured.UID == "" || captured.Segment != 1 {
		t.Fatalf("captured turn address = %+v, want a uid and segment 1", captured)
	}

	// The node is deleted: its log moves to sessions/archive, and the live node
	// is gone (a notes-pane jump discovers this via nodeById == nil).
	a.archiveSessionLog("alpha")

	// Resolving the stamped triple through the archived surface lands on the
	// exact captured turn — not the same-segment neighbour, not the pre-clear
	// turn — even though the live node no longer exists.
	code, ar := doPreviewSegRec(t, a, captured.UID, captured.Segment, captured.Record, captured.Time)
	if code != 200 {
		t.Fatalf("archived resolve code = %d", code)
	}
	if ar.Turns[ar.Anchor].Text != "the captured turn" {
		t.Errorf("archived anchor = %q, want %q", ar.Turns[ar.Anchor].Text, "the captured turn")
	}
}

// The surface is no longer "the deleted-chat window": every search hit opens it,
// so a LIVE chat's log must be previewable by uid too. What differs is the one
// fact the head needs — whether there is still a node to walk into — and it is
// reported as a field, not inferred by the client from a 404.
func TestPreviewServesALiveChatAndNamesItsNode(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	liveNode(a, &Node{ID: "alpha", Title: "Alpha study", Agent: "claude", CreatedAt: "2026-07-14T00:00:00Z"})
	appendLog(t, a, "alpha.jsonl",
		sessionlog.NewMeta("alpha", "claude", "opus", "", a.home),
		sessionlog.Event{T: "user", Text: "the live needle", Time: "2026-07-14T01:00:00Z"},
		sessionlog.Event{T: "assistant", Text: "an answer", Time: "2026-07-14T01:01:00Z"},
	)
	uid := readLogMeta(a.sessionLogPath("alpha")).UID

	code, pr := doPreview(t, a, uid, "2026-07-14T01:00:00Z")
	if code != 200 {
		t.Fatalf("live preview code = %d, want 200", code)
	}
	if len(pr.Turns) != 2 {
		t.Fatalf("live preview turns = %d, want 2", len(pr.Turns))
	}
	if pr.Node != "alpha" {
		t.Errorf("node = %q, want alpha — the head has no other way to offer Open chat", pr.Node)
	}
	// The node's *current* title, not the slug the meta header froze at creation:
	// chats get renamed, and the preview must agree with the chat it opens into.
	if pr.Title != "Alpha study" {
		t.Errorf("title = %q, want the node's current title", pr.Title)
	}
}

// A live chat's assets resolve in the preview exactly as they do in the chat.
// Without this the same turn reads "unavailable" here and fine one tap later,
// which would teach the user that the preview is lying to them.
func TestPreviewResolvesAssetsOnALiveChat(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	liveNode(a, &Node{ID: "alpha", Title: "Alpha", Agent: "claude", CreatedAt: "2026-07-14T00:00:00Z"})
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n"))
	appendLog(t, a, "alpha.jsonl",
		sessionlog.NewMeta("alpha", "claude", "", "", a.home),
		sessionlog.NewAsset(sessionlog.AssetEvent{
			ID: "a_1", Name: "chart.png", Mime: "image/png", Storage: "inline", Bytes: png,
		}),
		sessionlog.Event{T: "user", Text: "look at scimux-asset:a_1 needle", Time: "2026-07-14T01:00:00Z"},
	)
	uid := readLogMeta(a.sessionLogPath("alpha")).UID

	code, pr := doPreview(t, a, uid, "2026-07-14T01:00:00Z")
	if code != 200 || len(pr.Turns) == 0 {
		t.Fatalf("live preview code = %d turns = %d", code, len(pr.Turns))
	}
	if !strings.Contains(pr.Turns[pr.Anchor].Text, "scimux-asset:a_1") {
		t.Errorf("anchor text = %q, want the marker preserved for the client to render",
			pr.Turns[pr.Anchor].Text)
	}
	if pr.Assets["a_1"] == nil {
		t.Errorf("assets = %+v, want a_1 described so the chip is not inert", pr.Assets)
	}
}

// Fork is retired from this surface. The payload must not even describe it:
// a dir and an effort exist only to seed a fork sheet, and shipping them would
// invite the control back.
func TestPreviewOffersNoFork(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	appendLog(t, a, filepath.Join("archive", "dead.jsonl"),
		sessionlog.NewMeta("dead", "codex", "sonnet", "high", a.home),
		sessionlog.Event{T: "user", Text: "hello needle", Time: "2026-07-10T00:00:00Z"},
	)
	uid := doSearch(t, a, "needle").Groups[0].UID

	code, _, body := doPreviewRaw(t, a, "/api/preview?uid="+uid)
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	for _, gone := range []string{`"forkable"`, `"dir"`, `"effort"`} {
		if strings.Contains(body, gone) {
			t.Errorf("preview payload still carries %s: %s", gone, body)
		}
	}
}

func TestPreviewNotFound(t *testing.T) {
	f := &fakeTmux{}
	a := newTestApp(t, f)
	appendLog(t, a, filepath.Join("archive", "dead.jsonl"),
		sessionlog.NewMeta("dead", "claude", "", "", a.home),
		sessionlog.Event{T: "user", Text: "hello", Time: "2026-07-10T00:00:00Z"},
	)
	if code, _ := doPreview(t, a, "nope-not-a-real-uid", ""); code != 404 {
		t.Errorf("unknown uid code = %d, want 404", code)
	}
	if code, _ := doPreview(t, a, "", ""); code != 400 {
		t.Errorf("missing uid code = %d, want 400", code)
	}
}
