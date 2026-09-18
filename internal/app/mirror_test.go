package app

// Mirror tests drive syncMirror with synthetic Claude-format transcript files
// (never a real agent CLI) and assert on the resulting session log: the meta
// header, source seams, turn watermarks across restarts, /clear rollovers,
// and rotation. The log itself is the only durable state, so every scenario
// checks idempotence by replaying from disk.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/transcript"
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

func TestMirrorRestartRemirrorsUnchangedTranscriptAfterParserRecovery(t *testing.T) {
	a := mirrorTestApp(t)
	tdir := t.TempDir()
	tp := filepath.Join(tdir, "sid.jsonl")
	corruptFinal := `{"parentUuid":"p0","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu-lost","name":"Bash","input":{}}],"usage":{"iterations":[{"input_tokens":2},` +
		`{"parentUuid":"p1","message":{"id":"msg-final","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"recovered review"}]},"type":"assistant","uuid":"u-final","timestamp":"t2","session_id":"sid","sessionId":"sid"}` + "\n"
	transcriptText := claudeTurn("user", "initial", "t1") + corruptFinal +
		claudeTurn("user", "manual follow-up", "t3") + claudeTurn("assistant", "written", "t4")
	if err := os.WriteFile(tp, []byte(transcriptText), 0o600); err != nil {
		t.Fatal(err)
	}

	// Durable state produced by the old parser: it skipped the corrupt line,
	// then marked all transcript bytes consumed.
	logPath := filepath.Join(a.sessionsDir, "n1.jsonl")
	w := &sessionlog.Writer{Path: logPath}
	for _, ev := range []sessionlog.Event{
		sessionlog.NewMeta("n1", "claude", "opus", "", tdir),
		sessionlog.NewSource(tp, "sid"),
		{T: "user", Text: "initial", Time: "t1"},
		{T: "user", Text: "manual follow-up", Time: "t3"},
		{T: "assistant", Text: "written", Time: "t4"},
		sessionlog.NewMark(int64(len(transcriptText))),
	} {
		if err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}

	n := &Node{ID: "n1", Agent: "claude", Model: "opus", Dir: tdir, Transcript: tp}
	_ = a.tailerFor(n) // startup catch-up has the widened four-turn view
	a.syncMirror(n)
	seg := sessionlog.ReadSegment(logPath)
	if got := len(seg.Turns); got != 4 {
		t.Fatalf("remirrored current turns = %d, want 4", got)
	}
	if seg.Turns[1].Role != "assistant" || seg.Turns[1].Text != "recovered review" {
		t.Fatalf("recovered current segment = %#v", seg.Turns)
	}
	if history := sessionlog.ReadHistory(logPath); len(history) != 2 {
		t.Fatalf("segments = %d, want old history + corrected current", len(history))
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

// --- Phase 3: Claude breakdown projection into sessionlog (fare-design.md) ---

func TestMirror_WritesClaudeBreakdown(t *testing.T) {
	// §2.2 / §2.3: Claude input is fresh-direct; cache-creation is cache-write.
	// After projection, UsageEvent carries the split and ReadFare yields
	// FreshIn:2, CacheWrite:25437, CacheRead:0, Out:410.
	a := mirrorTestApp(t)
	tdir := t.TempDir()
	tp := filepath.Join(tdir, "sess-1.jsonl")
	appendFile(t, tp, `{"type":"assistant","timestamp":"t1","requestId":"req-xyz","message":{"id":"msg-abc","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":2,"cache_creation_input_tokens":25437,"cache_read_input_tokens":0,"output_tokens":410}}}`+"\n")
	n := &Node{ID: "n1", Agent: "claude", Model: "opus", Transcript: tp}
	a.syncMirror(n)

	var usage *sessionlog.UsageEvent
	for _, ev := range logEvents(t, a, "n1") {
		if ev.T == "usage" && ev.Usage != nil {
			usage = ev.Usage
		}
	}
	if usage == nil {
		t.Fatal("no usage event projected")
	}
	if usage.InputTokens != 2 {
		t.Errorf("InputTokens = %d, want 2", usage.InputTokens)
	}
	if usage.OutputTokens != 410 {
		t.Errorf("OutputTokens = %d, want 410", usage.OutputTokens)
	}
	if usage.CachedReadTokens != 0 {
		t.Errorf("CachedReadTokens = %d, want 0", usage.CachedReadTokens)
	}
	if usage.CacheCreationTokens != 25437 {
		t.Errorf("CacheCreationTokens = %d, want 25437", usage.CacheCreationTokens)
	}
	if usage.TurnID != "msg-abc:req-xyz" {
		t.Errorf("TurnID = %q, want %q", usage.TurnID, "msg-abc:req-xyz")
	}
	// Occupancy Used still the flat sum (2+0+25437+410).
	if usage.Used != 25849 {
		t.Errorf("Used = %d, want 25849 (occupancy sum)", usage.Used)
	}

	logPath := filepath.Join(a.sessionsDir, "n1.jsonl")
	fare := sessionlog.ReadFare(logPath)
	if fare.FreshIn != 2 {
		t.Errorf("ReadFare FreshIn = %d, want 2 (claude direct)", fare.FreshIn)
	}
	if fare.CacheWrite != 25437 {
		t.Errorf("ReadFare CacheWrite = %d, want 25437", fare.CacheWrite)
	}
	if fare.CacheRead != 0 {
		t.Errorf("ReadFare CacheRead = %d, want 0", fare.CacheRead)
	}
	if fare.Out != 410 {
		t.Errorf("ReadFare Out = %d, want 410", fare.Out)
	}
	if fare.Turns != 1 {
		t.Errorf("ReadFare Turns = %d, want 1", fare.Turns)
	}
}

func TestMirror_UsageChangeGateStillHolds(t *testing.T) {
	// Only emit on occupancy change (used/win). Unchanged usage must not
	// append extra records; a real change still produces exactly one new one.
	a := mirrorTestApp(t)
	tdir := t.TempDir()
	tp := filepath.Join(tdir, "sess-1.jsonl")
	appendFile(t, tp, `{"type":"assistant","timestamp":"t1","requestId":"req-1","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":90,"output_tokens":10,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`+"\n")
	n := &Node{ID: "n1", Agent: "claude", Transcript: tp}
	a.syncMirror(n)
	a.syncMirror(n) // idle: no second usage

	countUsage := func() int {
		n := 0
		for _, ev := range logEvents(t, a, "n1") {
			if ev.T == "usage" {
				n++
			}
		}
		return n
	}
	if got := countUsage(); got != 1 {
		t.Fatalf("after idle ticks: usage events = %d, want 1", got)
	}

	// Different occupancy → exactly one new usage event (with breakdown).
	appendFile(t, tp, `{"type":"assistant","timestamp":"t2","requestId":"req-2","message":{"id":"msg-2","role":"assistant","content":[{"type":"text","text":"more"}],"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`+"\n")
	a.syncMirror(n)
	if got := countUsage(); got != 2 {
		t.Fatalf("after usage change: usage events = %d, want 2", got)
	}
	a.syncMirror(n) // still settled
	if got := countUsage(); got != 2 {
		t.Fatalf("after second idle: usage events = %d, want 2 (gate held)", got)
	}
}

// TestMirror_EmitsOnTurnIDChangeAtSameOccupancy: Phase-3 gate gap fix.
// Claude's mirror used to emit only when used/win changed, so two consecutive
// turns with an identical Used snapshot dropped the second turn from fare.
// ReadFare dedups by TurnID, so more emissions are harmless — fire also when
// TurnID advances at the same occupancy.
func TestMirror_EmitsOnTurnIDChangeAtSameOccupancy(t *testing.T) {
	a := mirrorTestApp(t)
	tdir := t.TempDir()
	tp := filepath.Join(tdir, "sess-1.jsonl")
	// used = input+cache_read+cache_creation+output = 90+0+0+10 = 100 for both.
	appendFile(t, tp, `{"type":"assistant","timestamp":"t1","requestId":"req-1","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":90,"output_tokens":10,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`+"\n")
	n := &Node{ID: "n1", Agent: "claude", Transcript: tp}
	a.syncMirror(n)

	countUsage := func() int {
		n := 0
		for _, ev := range logEvents(t, a, "n1") {
			if ev.T == "usage" {
				n++
			}
		}
		return n
	}
	if got := countUsage(); got != 1 {
		t.Fatalf("after first turn: usage events = %d, want 1", got)
	}

	// Same occupancy (used still 100), different TurnID.
	appendFile(t, tp, `{"type":"assistant","timestamp":"t2","requestId":"req-2","message":{"id":"msg-2","role":"assistant","content":[{"type":"text","text":"again"}],"usage":{"input_tokens":80,"output_tokens":20,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`+"\n")
	a.syncMirror(n)
	if got := countUsage(); got != 2 {
		t.Fatalf("after same-occupancy new turn: usage events = %d, want 2 (TurnID gate)", got)
	}

	// Distinct turn IDs on the two usage records.
	var turnIDs []string
	for _, ev := range logEvents(t, a, "n1") {
		if ev.T == "usage" && ev.Usage != nil {
			turnIDs = append(turnIDs, ev.Usage.TurnID)
		}
	}
	if len(turnIDs) != 2 || turnIDs[0] == turnIDs[1] {
		t.Fatalf("turn IDs = %v, want two distinct ids", turnIDs)
	}
	if turnIDs[0] != "msg-1:req-1" || turnIDs[1] != "msg-2:req-2" {
		t.Errorf("turn IDs = %v, want [msg-1:req-1 msg-2:req-2]", turnIDs)
	}

	// Occupancy gate still held: idle re-sync does not emit again.
	a.syncMirror(n)
	if got := countUsage(); got != 2 {
		t.Fatalf("after idle: usage events = %d, want 2 (no spurious emission)", got)
	}
}

// --- V2-P2 / O6: tool call+result projection into sessionlog ---

func TestMirror_ProjectsClaudeToolCallAndResult(t *testing.T) {
	// Synthetic Claude shape from V2-P1 grounding: tool_use t1 @09:00:05,
	// tool_result @09:00:06. Mirror must project pending→completed with CLI times.
	a := mirrorTestApp(t)
	tdir := t.TempDir()
	tp := filepath.Join(tdir, "sess-1.jsonl")
	appendFile(t, tp,
		`{"type":"user","timestamp":"2026-07-11T09:00:00.000Z","message":{"role":"user","content":"Hello agent, analyze run 42"}}`+"\n"+
			`{"type":"assistant","timestamp":"2026-07-11T09:00:05.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Looking."},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}`+"\n"+
			`{"type":"user","timestamp":"2026-07-11T09:00:06.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"results.json"}]}}`+"\n"+
			`{"type":"assistant","timestamp":"2026-07-11T09:00:15.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Done."}]}}`+"\n")
	n := &Node{ID: "n1", Agent: "claude", Transcript: tp}
	a.syncMirror(n)

	var tools []sessionlog.Event
	turns := 0
	for _, ev := range logEvents(t, a, "n1") {
		switch ev.T {
		case "tool":
			tools = append(tools, ev)
		case "user", "assistant":
			turns++
		}
	}
	if len(tools) != 2 {
		t.Fatalf("tool events = %d, want 2 (call+result); events=%v", len(tools), kinds(logEvents(t, a, "n1")))
	}
	if tools[0].Tool == nil || tools[0].Tool.ID != "t1" || tools[0].Tool.Status != "pending" {
		t.Fatalf("call = %+v, want id=t1 status=pending", tools[0].Tool)
	}
	if tools[0].Tool.Title != "Bash" {
		t.Errorf("call title = %q, want Bash", tools[0].Tool.Title)
	}
	if tools[0].Time != "2026-07-11T09:00:05.000Z" {
		t.Errorf("call time = %q, want transcript timestamp", tools[0].Time)
	}
	if tools[1].Tool == nil || tools[1].Tool.ID != "t1" || tools[1].Tool.Status != "completed" {
		t.Fatalf("result = %+v, want id=t1 status=completed", tools[1].Tool)
	}
	if tools[1].Time != "2026-07-11T09:00:06.000Z" {
		t.Errorf("result time = %q, want transcript timestamp", tools[1].Time)
	}
	// Turn count: user + assistant(text) + assistant(done) = 3 visible turns
	// (tool_result-only user has no text → ParseLine drops it).
	if turns < 2 {
		t.Errorf("turns = %d, want ≥2 chat turns", turns)
	}
}

func TestMirror_ProjectsCodexFunctionCallAndOutput(t *testing.T) {
	a := mirrorTestApp(t)
	tdir := t.TempDir()
	tp := filepath.Join(tdir, "rollout-1.jsonl")
	appendFile(t, tp,
		`{"timestamp":"2026-07-11T10:00:02.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Hello codex"}]}}`+"\n"+
			`{"timestamp":"2026-07-11T10:00:04.000Z","type":"response_item","payload":{"type":"function_call","name":"shell","call_id":"c1","arguments":"{}"}}`+"\n"+
			`{"timestamp":"2026-07-11T10:00:05.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":"done"}}`+"\n"+
			`{"timestamp":"2026-07-11T10:00:06.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}}`+"\n")
	n := &Node{ID: "n1", Agent: "codex", Transcript: tp}
	a.syncMirror(n)

	var tools []sessionlog.Event
	for _, ev := range logEvents(t, a, "n1") {
		if ev.T == "tool" {
			tools = append(tools, ev)
		}
	}
	if len(tools) != 2 {
		t.Fatalf("tool events = %d, want 2; kinds=%v", len(tools), kinds(logEvents(t, a, "n1")))
	}
	if tools[0].Tool.ID != "c1" || tools[0].Tool.Status != "pending" || tools[0].Tool.Title != "shell" {
		t.Fatalf("call = %+v", tools[0].Tool)
	}
	if tools[0].Time != "2026-07-11T10:00:04.000Z" {
		t.Errorf("call time = %q", tools[0].Time)
	}
	if tools[1].Tool.ID != "c1" || tools[1].Tool.Status != "completed" {
		t.Fatalf("result = %+v", tools[1].Tool)
	}
}

func TestMirror_ToolsDoNotInflateTurnCounter(t *testing.T) {
	// After tools are projected, restart must not re-project turns.
	// replayMirrorState counts only user/assistant for the turn watermark.
	a := mirrorTestApp(t)
	tdir := t.TempDir()
	tp := filepath.Join(tdir, "sess-1.jsonl")
	appendFile(t, tp,
		`{"type":"user","timestamp":"t1","message":{"role":"user","content":"q"}}`+"\n"+
			`{"type":"assistant","timestamp":"t2","message":{"role":"assistant","content":[{"type":"text","text":"a"},{"type":"tool_use","id":"tu","name":"Bash","input":{}}]}}`+"\n"+
			`{"type":"user","timestamp":"t3","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu","content":"ok"}]}}`+"\n")
	n := &Node{ID: "n1", Agent: "claude", Transcript: tp}
	a.syncMirror(n)

	st := replayMirrorState(filepath.Join(a.sessionsDir, "n1.jsonl"))
	if st.mirrored != 2 { // user + assistant text; tool_result user has no text
		t.Fatalf("mirrored turns = %d, want 2 (tools must not inflate)", st.mirrored)
	}
	if st.tools != 2 {
		t.Fatalf("mirrored tools = %d, want 2", st.tools)
	}

	// Restart: no duplication.
	b := mirrorTestApp(t)
	b.sessionsDir = a.sessionsDir
	b.syncMirror(n)
	evs := contentEvents(logEvents(t, b, "n1"))
	toolN, turnN := 0, 0
	for _, ev := range evs {
		switch ev.T {
		case "tool":
			toolN++
		case "user", "assistant":
			turnN++
		}
	}
	if toolN != 2 {
		t.Errorf("after restart tools = %d, want 2 (no dup)", toolN)
	}
	if turnN != 2 {
		t.Errorf("after restart turns = %d, want 2 (no dup)", turnN)
	}
}

func TestMirror_ToolIncrementAndIdle(t *testing.T) {
	a := mirrorTestApp(t)
	tdir := t.TempDir()
	tp := filepath.Join(tdir, "sess-1.jsonl")
	appendFile(t, tp, claudeTurn("user", "q", "t1"))
	n := &Node{ID: "n1", Agent: "claude", Transcript: tp}
	a.syncMirror(n)
	a.syncMirror(n) // idle

	appendFile(t, tp,
		`{"type":"assistant","timestamp":"t2","message":{"role":"assistant","content":[{"type":"tool_use","id":"x","name":"Bash","input":{}}]}}`+"\n")
	a.syncMirror(n)
	tools := 0
	for _, ev := range logEvents(t, a, "n1") {
		if ev.T == "tool" {
			tools++
		}
	}
	if tools != 1 {
		t.Fatalf("tools after increment = %d, want 1", tools)
	}
	a.syncMirror(n)
	tools = 0
	for _, ev := range logEvents(t, a, "n1") {
		if ev.T == "tool" {
			tools++
		}
	}
	if tools != 1 {
		t.Fatalf("idle tick re-projected tools: %d", tools)
	}
}

func claudeAssistantWithMeta(text, ts, metaJSON string) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"role":"assistant","content":%q,"_meta":%s}}`,
		ts, text, metaJSON) + "\n"
}

func TestMirrorRetainsProvenanceAndAgent(t *testing.T) {
	a := mirrorTestApp(t)
	tdir := t.TempDir()
	tp := filepath.Join(tdir, "sess-1.jsonl")
	metaJSON := `{"src":"synth-mirror","n":1}`
	appendFile(t, tp, claudeTurn("user", "question", "t1")+claudeAssistantWithMeta("answer", "t2", metaJSON))

	line := []byte(strings.TrimSpace(claudeAssistantWithMeta("answer", "t2", metaJSON)))
	parsed, ok := transcript.ParseLine(line)
	if !ok || parsed.Role != "assistant" || parsed.Text != "answer" || parsed.Time != "t2" {
		t.Fatalf("ParseLine = (%+v, %v)", parsed, ok)
	}
	if len(parsed.Prov) == 0 {
		t.Fatal("ParseLine dropped provenance before the mirror")
	}

	n := &Node{ID: "n1", Agent: "claude", Model: "opus", Dir: "/wd", Transcript: tp}
	a.syncMirror(n)

	evs := contentEvents(logEvents(t, a, "n1"))
	wantKinds := []string{"meta", "source", "user", "assistant"}
	if got := kinds(evs); fmt.Sprint(got) != fmt.Sprint(wantKinds) {
		t.Fatalf("after initial mirror: %v, want %v", got, wantKinds)
	}
	if evs[0].Meta == nil || evs[0].Meta.Agent != "claude" {
		t.Fatalf("meta agent = %+v", evs[0].Meta)
	}
	asst := evs[3]
	if asst.T != "assistant" || asst.Text != "answer" || asst.Time != "t2" {
		t.Fatalf("mirrored assistant = %+v", asst)
	}
	if len(asst.Prov) == 0 || bytes.TrimSpace(asst.Prov)[0] == '"' {
		t.Fatalf("mirrored Prov missing or stringified: %s", asst.Prov)
	}
	var entries []struct {
		Loc string          `json:"loc"`
		Key string          `json:"key"`
		V   json.RawMessage `json:"v"`
	}
	if err := json.Unmarshal(asst.Prov, &entries); err != nil {
		t.Fatalf("Prov is not an entry list: %v (%s)", err, asst.Prov)
	}
	found := false
	for _, e := range entries {
		if e.Loc == "message" && e.Key == "_meta" {
			var got, want any
			_ = json.Unmarshal(e.V, &got)
			_ = json.Unmarshal([]byte(metaJSON), &want)
			gb, _ := json.Marshal(got)
			wb, _ := json.Marshal(want)
			if string(gb) == string(wb) {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("mirrored _meta = %s, want %s", asst.Prov, metaJSON)
	}

	seg := sessionlog.ReadSegment(filepath.Join(a.sessionsDir, "n1.jsonl"))
	if len(seg.Turns) != 2 {
		t.Fatalf("segment turns = %+v", seg.Turns)
	}
	if seg.Turns[0].Role != "user" || seg.Turns[0].Agent != "" {
		t.Errorf("user misattributed: %+v", seg.Turns[0])
	}
	if seg.Turns[1].Role != "assistant" || seg.Turns[1].Agent != "claude" || seg.Turns[1].Text != "answer" {
		t.Errorf("segment assistant = %+v", seg.Turns[1])
	}
	if !bytes.Equal(seg.Turns[1].Prov, asst.Prov) {
		t.Errorf("segment Prov = %s, event Prov = %s", seg.Turns[1].Prov, asst.Prov)
	}

	// Idle tick must not duplicate turns (provenance is not a dedupe key).
	a.syncMirror(n)
	if got := len(contentEvents(logEvents(t, a, "n1"))); got != 4 {
		t.Fatalf("idle tick appended records: %d content events", got)
	}

	// Restart / replay: fresh app over the same log, then incremental continuation.
	b := mirrorTestApp(t)
	b.sessionsDir = a.sessionsDir
	b.syncMirror(n)
	if got := kinds(contentEvents(logEvents(t, b, "n1"))); fmt.Sprint(got) != fmt.Sprint(wantKinds) {
		t.Fatalf("restart duplicated history: %v", got)
	}

	appendFile(t, tp, claudeAssistantWithMeta("follow-up", "t3", `{"src":"synth-mirror","n":2}`))
	b.syncMirror(n)
	evs = contentEvents(logEvents(t, b, "n1"))
	if len(evs) != 5 || evs[4].T != "assistant" || evs[4].Text != "follow-up" {
		t.Fatalf("incremental continuation: %v %+v", kinds(evs), evs[len(evs)-1])
	}
	if len(evs[4].Prov) == 0 {
		t.Fatal("incremental continuation dropped provenance")
	}
	seg = sessionlog.ReadSegment(filepath.Join(b.sessionsDir, "n1.jsonl"))
	if len(seg.Turns) != 3 || seg.Turns[2].Agent != "claude" || seg.Turns[2].Text != "follow-up" {
		t.Fatalf("post-restart segment = %+v", seg.Turns)
	}

	// Provenance must not change source-seam or turn counts.
	seams, users, assts := 0, 0, 0
	for _, ev := range evs {
		switch ev.T {
		case "source":
			seams++
		case "user":
			users++
		case "assistant":
			assts++
		}
	}
	if seams != 1 || users != 1 || assts != 2 {
		t.Fatalf("counts source=%d user=%d assistant=%d", seams, users, assts)
	}
}
