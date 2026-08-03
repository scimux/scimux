package app

// Mirror tests drive syncMirror with synthetic Claude-format transcript files
// (never a real agent CLI) and assert on the resulting session log: the meta
// header, source seams, turn watermarks across restarts, /clear rollovers,
// and rotation. The log itself is the only durable state, so every scenario
// checks idempotence by replaying from disk.

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

func mirrorTestApp(t *testing.T) *app {
	t.Helper()
	return &app{
		byID:        map[string]*Node{},
		tailers:     map[string]*transcript.Tailer{},
		mirrors:     map[string]*mirror{},
		chatMark:    map[string]chatMark{},
		staleChat:   map[string]bool{},
		sessionsDir: t.TempDir(),
	}
}

func claudeTurn(role, text, ts string) string {
	return fmt.Sprintf(`{"type":%q,"timestamp":%q,"message":{"role":%q,"content":%q}}`,
		role, ts, role, text) + "\n"
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

func logEvents(t *testing.T, a *app, id string) []sessionlog.Event {
	t.Helper()
	return sessionlog.ReadEvents(filepath.Join(a.sessionsDir, id+".jsonl"))
}

func kinds(evs []sessionlog.Event) []string {
	var out []string
	for _, ev := range evs {
		out = append(out, ev.T)
	}
	return out
}

// contentEvents drops the mirror's "mark" watermark records — pure restart
// bookkeeping, orthogonal to the chat content these sequence assertions check.
func contentEvents(evs []sessionlog.Event) []sessionlog.Event {
	var out []sessionlog.Event
	for _, ev := range evs {
		if ev.T != "mark" {
			out = append(out, ev)
		}
	}
	return out
}

func TestMirrorBackfillAndIncrement(t *testing.T) {
	a := mirrorTestApp(t)
	tdir := t.TempDir()
	tp := filepath.Join(tdir, "sess-1.jsonl")
	appendFile(t, tp, claudeTurn("user", "question", "t1")+claudeTurn("assistant", "answer", "t2"))
	n := &Node{ID: "n1", Agent: "claude", Model: "opus", Dir: "/wd", Transcript: tp}

	a.syncMirror(n) // existing history is backfilled, not skipped
	evs := contentEvents(logEvents(t, a, "n1"))
	want := []string{"meta", "source", "user", "assistant"}
	if got := kinds(evs); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("after backfill: %v, want %v", got, want)
	}
	if evs[0].Meta == nil || evs[0].Meta.Agent != "claude" || evs[0].Meta.Model != "opus" {
		t.Fatalf("meta header wrong: %+v", evs[0].Meta)
	}
	if evs[1].Source == nil || evs[1].Source.Path != tp || evs[1].Source.SessionID != "sess-1" {
		t.Fatalf("source seam wrong: %+v", evs[1].Source)
	}
	if evs[2].Text != "question" || evs[2].Time != "t1" || evs[3].Text != "answer" {
		t.Fatalf("turns wrong: %+v", evs[2:])
	}

	a.syncMirror(n) // idle tick: nothing appended
	if got := len(contentEvents(logEvents(t, a, "n1"))); got != 4 {
		t.Fatalf("idle tick appended records: %d content events", got)
	}

	appendFile(t, tp, claudeTurn("user", "follow-up", "t3"))
	a.syncMirror(n)
	evs = contentEvents(logEvents(t, a, "n1"))
	if len(evs) != 5 || evs[4].T != "user" || evs[4].Text != "follow-up" {
		t.Fatalf("incremental turn missing: %v", kinds(evs))
	}
}

func TestMirrorRestartResumes(t *testing.T) {
	a := mirrorTestApp(t)
	tdir := t.TempDir()
	tp := filepath.Join(tdir, "sess-1.jsonl")
	appendFile(t, tp, claudeTurn("user", "one", "t1"))
	n := &Node{ID: "n1", Agent: "claude", Transcript: tp}
	a.syncMirror(n)

	// "Restart": fresh app state over the same sessions dir, fresh tailer.
	b := mirrorTestApp(t)
	b.sessionsDir = a.sessionsDir
	a = nil
	b.syncMirror(n)
	evs := contentEvents(logEvents(t, b, "n1"))
	want := []string{"meta", "source", "user"}
	if got := kinds(evs); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("restart duplicated history: %v, want %v", got, want)
	}

	appendFile(t, tp, claudeTurn("assistant", "two", "t2"))
	b.syncMirror(n)
	evs = contentEvents(logEvents(t, b, "n1"))
	if len(evs) != 4 || evs[3].Text != "two" {
		t.Fatalf("post-restart increment wrong: %v", kinds(evs))
	}
}

func TestMirrorClearRolloverWritesNewSource(t *testing.T) {
	a := mirrorTestApp(t)
	tdir := t.TempDir()
	tp1 := filepath.Join(tdir, "sess-1.jsonl")
	appendFile(t, tp1, claudeTurn("user", "before clear", "t1"))
	n := &Node{ID: "n1", Agent: "claude", Transcript: tp1}
	a.syncMirror(n)

	// /clear: relink repoints the node at a fresh session file whose history
	// starts over. The old turns must stay, seamed by a second source record.
	tp2 := filepath.Join(tdir, "sess-2.jsonl")
	appendFile(t, tp2, claudeTurn("user", "after clear", "t2"))
	n.Transcript = tp2
	a.syncMirror(n)

	evs := contentEvents(logEvents(t, a, "n1"))
	want := []string{"meta", "source", "user", "source", "user"}
	if got := kinds(evs); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("rollover log: %v, want %v", got, want)
	}
	if evs[3].Source.SessionID != "sess-2" || evs[4].Text != "after clear" {
		t.Fatalf("second segment wrong: %+v %+v", evs[3].Source, evs[4])
	}
}

func TestMirrorRotationResources(t *testing.T) {
	a := mirrorTestApp(t)
	tdir := t.TempDir()
	tp := filepath.Join(tdir, "sess-1.jsonl")
	appendFile(t, tp, claudeTurn("user", "one", "t1")+claudeTurn("assistant", "two", "t2"))
	n := &Node{ID: "n1", Agent: "claude", Transcript: tp}
	a.syncMirror(n)

	// Rotation: same path, file rewritten shorter. The tailer resets; the
	// mirror opens a new source segment rather than silently dropping turns.
	if err := os.WriteFile(tp, []byte(claudeTurn("user", "rebuilt", "t3")), 0o600); err != nil {
		t.Fatal(err)
	}
	a.syncMirror(n)
	evs := contentEvents(logEvents(t, a, "n1"))
	want := []string{"meta", "source", "user", "assistant", "source", "user"}
	if got := kinds(evs); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("rotation log: %v, want %v", got, want)
	}
	if evs[5].Text != "rebuilt" {
		t.Fatalf("post-rotation turn wrong: %+v", evs[5])
	}
}

func TestMirrorUsageDedupe(t *testing.T) {
	a := mirrorTestApp(t)
	tdir := t.TempDir()
	tp := filepath.Join(tdir, "sess-1.jsonl")
	// A Claude assistant record carrying usage.
	appendFile(t, tp, `{"type":"assistant","timestamp":"t1","message":{"role":"assistant","content":"hi","usage":{"input_tokens":90,"output_tokens":10}}}`+"\n")
	n := &Node{ID: "n1", Agent: "claude", Transcript: tp}
	a.syncMirror(n)
	a.syncMirror(n) // unchanged usage must not append again

	var usages int
	for _, ev := range logEvents(t, a, "n1") {
		if ev.T == "usage" {
			usages++
			if ev.Usage.Used != 100 {
				t.Fatalf("usage used = %d, want 100", ev.Usage.Used)
			}
		}
	}
	if usages != 1 {
		t.Fatalf("usage events = %d, want 1", usages)
	}
}

// The restart fast path: a mark records the consumed transcript size, and on
// restart an unchanged (append-only) transcript is skipped without re-parsing.
// Proven behaviorally by substituting same-length-but-different content after
// the first sync — the size-keyed fast path must skip it, so the substituted
// turn is intentionally NOT mirrored. This documents the append-only tradeoff.
func TestMirrorFastPathSkipsUnchangedSize(t *testing.T) {
	a := mirrorTestApp(t)
	tdir := t.TempDir()
	tp := filepath.Join(tdir, "sess-1.jsonl")
	appendFile(t, tp, claudeTurn("user", "one", "t1"))
	n := &Node{ID: "n1", Agent: "claude", Transcript: tp}
	a.syncMirror(n)

	fi, err := os.Stat(tp)
	if err != nil {
		t.Fatal(err)
	}
	var markSize int64 = -1
	for _, ev := range logEvents(t, a, "n1") {
		if ev.T == "mark" && ev.Mark != nil {
			markSize = ev.Mark.Off
		}
	}
	if markSize != fi.Size() {
		t.Fatalf("mark size = %d, want file size %d", markSize, fi.Size())
	}

	// Restart over the same store, then overwrite with same-length content.
	b := mirrorTestApp(t)
	b.sessionsDir = a.sessionsDir
	same := claudeTurn("user", "XXX", "t1") // "XXX" is the same length as "one"
	if int64(len(same)) != fi.Size() {
		t.Fatalf("setup: substitute size %d != original %d", len(same), fi.Size())
	}
	if err := os.WriteFile(tp, []byte(same), 0o600); err != nil {
		t.Fatal(err)
	}
	b.syncMirror(n)
	evs := contentEvents(logEvents(t, b, "n1"))
	if got := kinds(evs); fmt.Sprint(got) != fmt.Sprint([]string{"meta", "source", "user"}) {
		t.Fatalf("fast path did not skip unchanged-size transcript: %v", got)
	}
	if evs[2].Text != "one" {
		t.Fatalf("skipped file was re-parsed: turn = %q, want unchanged %q", evs[2].Text, "one")
	}
}

// Phase 4: a Markdown image reference in mirrored (tmux) chat text must be
// ingested as a durable asset at turn-append time, the same "Ingestion
// Timing" rule the ACP/codex managers follow — never lazily from a later
// read, since a short-lived agent's temp file could vanish before then.
func TestMirrorIngestsAssetOnTurnAppend(t *testing.T) {
	a := mirrorTestApp(t)
	a.assetsDir = t.TempDir()
	a.assetHook = a.ingestAssetHook

	wd := t.TempDir()
	if err := os.WriteFile(filepath.Join(wd, "sketch.png"), []byte("PNGBYTES"), 0o600); err != nil {
		t.Fatal(err)
	}

	tdir := t.TempDir()
	tp := filepath.Join(tdir, "sess-1.jsonl")
	appendFile(t, tp, claudeTurn("assistant", "here: ![a sketch](sketch.png)", "t1"))
	n := &Node{ID: "n1", Agent: "claude", Model: "opus", Dir: wd, Transcript: tp}

	a.syncMirror(n)

	byPath := sessionlog.ReadAssetsByPath(filepath.Join(a.sessionsDir, "n1.jsonl"))
	ev, ok := byPath["sketch.png"]
	if !ok {
		t.Fatal("agent-referenced image not ingested at turn-append time")
	}
	if ev.SourceKind != "agent_path" {
		t.Errorf("sourceKind = %q, want agent_path", ev.SourceKind)
	}
}

func TestMirrorNoTranscriptIsNoop(t *testing.T) {
	a := mirrorTestApp(t)
	n := &Node{ID: "n1", Agent: "claude"}
	a.syncMirror(n)
	if _, err := os.Stat(filepath.Join(a.sessionsDir, "n1.jsonl")); !os.IsNotExist(err) {
		t.Fatal("mirror created a log for a node without a transcript")
	}
}
