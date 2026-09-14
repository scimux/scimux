package sessionlog

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"codeberg.org/chrberger/scimux/internal/transcript"
)

func TestMetaHeaderRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.jsonl")
	w := &Writer{Path: path}
	if err := w.Append(NewMeta("review-parser", "codex", "opus", "high", "/tmp/wd")); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(Event{T: "user", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	evs := ReadEvents(path)
	if len(evs) != 2 || evs[0].T != "meta" || evs[0].Meta == nil {
		t.Fatalf("want meta header first, got %+v", evs)
	}
	m := evs[0].Meta
	if m.Node != "review-parser" || m.Agent != "codex" || m.Model != "opus" ||
		m.Effort != "high" || m.Dir != "/tmp/wd" || m.UID == "" || m.Created == "" {
		t.Fatalf("meta fields incomplete: %+v", m)
	}
	if NewMeta("a", "b", "", "", "").Meta.UID == m.UID {
		t.Fatal("meta UIDs must differ between calls")
	}
}

func TestReadTurnsSkipsNonChatRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.jsonl")
	w := &Writer{Path: path}
	for _, ev := range []Event{
		NewMeta("n", "claude", "", "", ""),
		{T: "user", Text: "question"},
		{T: "tool", Tool: &ToolEvent{ID: "t1", Title: "ls", Status: "completed"}},
		{T: "usage", Usage: &UsageEvent{Used: 10, Size: 100}},
		{T: "assistant", Text: "answer"},
		{T: "stop", StopReason: "end_turn"},
		{T: "user", Text: "  "}, // blank text never becomes a turn
	} {
		if err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	turns := ReadTurns(path)
	if len(turns) != 2 || turns[0].Role != "user" || turns[1].Role != "assistant" {
		t.Fatalf("want 2 chat turns, got %+v", turns)
	}
	if used, size := LatestUsage(path); used != 10 || size != 100 {
		t.Fatalf("usage = %d/%d, want 10/100", used, size)
	}
}

func TestReadersAreDefensive(t *testing.T) {
	if got := ReadEvents(filepath.Join(t.TempDir(), "missing.jsonl")); got != nil {
		t.Fatalf("missing file: want nil, got %v", got)
	}
	path := filepath.Join(t.TempDir(), "n.jsonl")
	content := `{"t":"user","text":"ok"}` + "\n" +
		"not json at all\n" +
		`{"t":"future-type","x":1}` + "\n" +
		`{"t":"assistant","text":"torn` // no trailing newline, truncated JSON
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	turns := ReadTurns(path)
	if len(turns) != 1 || turns[0].Text != "ok" {
		t.Fatalf("want the one valid turn, got %+v", turns)
	}
}

func TestPeekLog(t *testing.T) {
	dir := t.TempDir()
	if got := PeekLog(filepath.Join(dir, "none.jsonl"), 10, "(empty)"); got != "(empty)" {
		t.Fatalf("empty peek = %q", got)
	}
	path := filepath.Join(dir, "n.jsonl")
	w := &Writer{Path: path}
	w.Append(NewMeta("n1", "codex", "gpt-5.5", "", "/wd"))
	w.Append(Event{T: "user", Text: "line one\nline two"})
	got := PeekLog(path, 10, "(empty)")
	if !strings.Contains(got, "session n1 (codex gpt-5.5)") {
		t.Fatalf("peek misses meta line: %q", got)
	}
	if !strings.Contains(got, "» line one line two") {
		t.Fatalf("peek misses flattened user line: %q", got)
	}
}

// A /clear appends a source seam and turns the page; the peek must show only
// the current segment, never resurface the dead conversation behind the seam
// (finding 90).
func TestPeekLogScopedToCurrentSegment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.jsonl")
	w := &Writer{Path: path}
	w.Append(NewMeta("n1", "opencode", "", "", "/wd"))
	w.Append(Event{T: "user", Text: "old question"})
	w.Append(Event{T: "assistant", Text: "old answer"})
	w.Append(NewSource("", "fresh-session"))
	got := PeekLog(path, 10, "(empty)")
	if strings.Contains(got, "old question") || strings.Contains(got, "old answer") {
		t.Fatalf("peek resurfaces pre-seam history: %q", got)
	}
	if !strings.Contains(got, "— source") {
		t.Fatalf("peek should keep the seam line as the boundary: %q", got)
	}
	w.Append(Event{T: "user", Text: "new question"})
	got = PeekLog(path, 10, "(empty)")
	if !strings.Contains(got, "» new question") || strings.Contains(got, "old answer") {
		t.Fatalf("post-seam peek wrong: %q", got)
	}
}

func TestSourceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.jsonl")
	w := &Writer{Path: path}
	w.Append(NewMeta("n", "claude", "", "", ""))
	w.Append(NewSource("/x/aaa.jsonl", "aaa"))
	w.Append(Event{T: "user", Text: "hi"})
	evs := ReadEvents(path)
	if len(evs) != 3 || evs[1].T != "source" || evs[1].Source == nil {
		t.Fatalf("events: %+v", evs)
	}
	if evs[1].Source.Path != "/x/aaa.jsonl" || evs[1].Source.SessionID != "aaa" {
		t.Fatalf("source fields: %+v", evs[1].Source)
	}
	if turns := ReadTurns(path); len(turns) != 1 {
		t.Fatalf("source records must not become turns: %+v", turns)
	}
	if peek := PeekLog(path, 10, ""); !strings.Contains(peek, "— source /x/aaa.jsonl") {
		t.Fatalf("peek misses source seam: %q", peek)
	}
}

// Separate Writer instances against one path (as /clear seams and start-failure
// records create) must serialize: concurrent appends stay whole JSONL lines,
// none lost or interleaved. Run with -race to catch the shared-fd hazard.
func TestConcurrentAppendsAcrossWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.jsonl")
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := &Writer{Path: path} // a fresh Writer per goroutine on purpose
			if err := w.Append(Event{T: "user", Text: strings.Repeat("x", 100+i)}); err != nil {
				t.Errorf("append: %v", err)
			}
		}(i)
	}
	wg.Wait()
	evs := ReadEvents(path)
	if len(evs) != n {
		t.Fatalf("read %d events, want %d (a lost or torn line)", len(evs), n)
	}
	for _, ev := range evs {
		if ev.T != "user" {
			t.Fatalf("torn/garbled record: %+v", ev)
		}
	}
}

func TestAppendFirstCreateSyncsParentDir(t *testing.T) {
	// The first Append creates the file and must sync the parent dir; a second
	// append to the existing file must not error either. We can't observe the
	// dirent fsync directly, but the code path (created vs existing) must run
	// cleanly and the records must survive.
	path := filepath.Join(t.TempDir(), "n.jsonl")
	w := &Writer{Path: path}
	if err := w.Append(Event{T: "user", Text: "first"}); err != nil {
		t.Fatalf("first append (create): %v", err)
	}
	if err := w.Append(Event{T: "user", Text: "second"}); err != nil {
		t.Fatalf("second append (existing): %v", err)
	}
	if evs := ReadEvents(path); len(evs) != 2 {
		t.Fatalf("read %d events, want 2", len(evs))
	}
}

func TestSyncParentDir(t *testing.T) {
	dir := t.TempDir()
	if err := SyncParentDir(filepath.Join(dir, "n.jsonl")); err != nil {
		t.Fatalf("SyncParentDir on existing dir: %v", err)
	}
	if err := SyncParentDir(filepath.Join(dir, "nope", "n.jsonl")); err == nil {
		t.Fatalf("SyncParentDir on missing parent: want error, got nil")
	}
}

// --- Phase 1: UsageEvent schema extension (fare-design.md §4.1) ---

func TestUsageEvent_RoundTripNewFields(t *testing.T) {
	in := UsageEvent{
		Used:                100,
		Size:                1000,
		InputTokens:         50,
		OutputTokens:        20,
		CachedReadTokens:    30,
		TotalTokens:         100,
		CacheCreationTokens: 25437,
		ReasoningTokens:     77,
		Model:               "claude-opus-4",
		TurnID:              "msg-abc:req-xyz",
		CostAmount:          0.0088,
		CostCurrency:        "USD",
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out UsageEvent
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.CacheCreationTokens != 25437 {
		t.Errorf("CacheCreationTokens = %d, want 25437", out.CacheCreationTokens)
	}
	if out.ReasoningTokens != 77 {
		t.Errorf("ReasoningTokens = %d, want 77", out.ReasoningTokens)
	}
	if out.Model != "claude-opus-4" {
		t.Errorf("Model = %q, want %q", out.Model, "claude-opus-4")
	}
	if out.TurnID != "msg-abc:req-xyz" {
		t.Errorf("TurnID = %q, want %q", out.TurnID, "msg-abc:req-xyz")
	}
	// Existing fields still round-trip unchanged.
	if out.Used != 100 || out.Size != 1000 || out.InputTokens != 50 ||
		out.OutputTokens != 20 || out.CachedReadTokens != 30 || out.TotalTokens != 100 ||
		out.CostAmount != 0.0088 || out.CostCurrency != "USD" {
		t.Errorf("existing fields corrupted: %+v", out)
	}
}

func TestUsageEvent_OmitEmptyKeepsOldShape(t *testing.T) {
	// Greppability invariant: minimal occupancy-only records must stay
	// exactly {"used":N} — new fields must not leak empty keys.
	b, err := json.Marshal(UsageEvent{Used: 36649})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"used":36649}`
	if string(b) != want {
		t.Errorf("marshal = %s, want exactly %s", b, want)
	}
}

func TestUsageEvent_ParsesLegacyRecord(t *testing.T) {
	// On-disk corpus shape before Phase 1 — must decode without migration.
	const legacy = `{"t":"usage","ts":"2026-08-01T12:00:00Z","usage":{"used":36649}}`
	var ev Event
	if err := json.Unmarshal([]byte(legacy), &ev); err != nil {
		t.Fatalf("legacy decode: %v", err)
	}
	if ev.T != "usage" || ev.Usage == nil {
		t.Fatalf("want usage event, got %+v", ev)
	}
	if ev.Usage.Used != 36649 {
		t.Errorf("Used = %d, want 36649", ev.Usage.Used)
	}
	// New fields zero/empty on legacy records.
	if ev.Usage.CacheCreationTokens != 0 || ev.Usage.ReasoningTokens != 0 ||
		ev.Usage.Model != "" || ev.Usage.TurnID != "" {
		t.Errorf("legacy should leave new fields zero: %+v", ev.Usage)
	}
}

func TestUsageEvent_StringIncludesModelWhenSet(t *testing.T) {
	// Empty Model must not change the pre-Phase-1 usage line shape.
	bare := UsageEvent{Used: 10, Size: 100}
	gotBare := bare.String()
	wantBare := "used=10 size=100"
	if gotBare != wantBare {
		t.Errorf("empty Model String() = %q, want %q", gotBare, wantBare)
	}
	// formatEvent prefix stays "· usage " + String().
	evBare := Event{T: "usage", Usage: &bare}
	if got := formatEvent(evBare); got != "· usage "+wantBare {
		t.Errorf("formatEvent empty model = %q", got)
	}

	withModel := UsageEvent{Used: 10, Size: 100, Model: "gpt-5.5"}
	got := withModel.String()
	if !strings.Contains(got, "model=gpt-5.5") {
		t.Errorf("String() missing model: %q", got)
	}
	if !strings.Contains(got, "used=10") || !strings.Contains(got, "size=100") {
		t.Errorf("String() dropped occupancy: %q", got)
	}
	ev := Event{T: "usage", Usage: &withModel}
	if got := formatEvent(ev); !strings.Contains(got, "model=gpt-5.5") {
		t.Errorf("formatEvent missing model: %q", got)
	}
}

// --- Phase 2: opaque provenance persistence and projections ---

func synthProvJSON(src string) json.RawMessage {
	return json.RawMessage(`[{"loc":"message","key":"_meta","v":{"src":"` + src + `"}}]`)
}

func assertRawEqual(t *testing.T, got, want json.RawMessage) {
	t.Helper()
	if bytes.Equal(got, want) {
		return
	}
	if len(got) == 0 && len(want) == 0 {
		return
	}
	var gv, wv any
	if json.Unmarshal(got, &gv) == nil && json.Unmarshal(want, &wv) == nil {
		g, _ := json.Marshal(gv)
		w, _ := json.Marshal(wv)
		if bytes.Equal(g, w) {
			return
		}
	}
	t.Errorf("prov = %s, want %s", got, want)
}

func TestAppendReadEventsRetainsProv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.jsonl")
	w := &Writer{Path: path}
	prov := synthProvJSON("sessionlog-append")
	if err := w.Append(NewMeta("n", "claude", "", "", "")); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(Event{T: "assistant", Text: "hello", Time: "2026-07-11T09:00:05Z", Prov: prov}); err != nil {
		t.Fatal(err)
	}
	evs := ReadEvents(path)
	if len(evs) != 2 || evs[1].T != "assistant" || evs[1].Text != "hello" {
		t.Fatalf("events = %+v", evs)
	}
	assertRawEqual(t, evs[1].Prov, prov)
	if len(evs[1].Prov) == 0 || bytes.TrimSpace(evs[1].Prov)[0] == '"' {
		t.Fatalf("Prov was stringified: %s", evs[1].Prov)
	}
}

func TestReadTurnsRetainsAgentAndProv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.jsonl")
	w := &Writer{Path: path}
	prov := synthProvJSON("read-turns")
	for _, ev := range []Event{
		NewMeta("n", "codex", "gpt", "", ""),
		{T: "user", Text: "question", Time: "t1"},
		{T: "assistant", Text: "answer", Time: "t2", Prov: prov},
	} {
		if err := w.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	turns := ReadTurns(path)
	if len(turns) != 2 {
		t.Fatalf("turns = %+v", turns)
	}
	if turns[0].Role != "user" || turns[0].Agent != "" || len(turns[0].Prov) != 0 {
		t.Errorf("user turn must not be agent-authored: %+v", turns[0])
	}
	if turns[1].Role != "assistant" || turns[1].Text != "answer" {
		t.Errorf("assistant turn = %+v", turns[1])
	}
	if turns[1].Agent != "codex" {
		t.Errorf("assistant Agent = %q, want codex from meta.agent", turns[1].Agent)
	}
	assertRawEqual(t, turns[1].Prov, prov)
}

func TestReadTurnsLegacyMissingProvAndAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.jsonl")
	// Hand-written old log: no prov field, still parses.
	body := `{"t":"meta","time":"t0","meta":{"node":"n","uid":"U","agent":"claude","created":"t0"}}` + "\n" +
		`{"t":"user","time":"t1","text":"old q"}` + "\n" +
		`{"t":"assistant","time":"t2","text":"old a"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	turns := ReadTurns(path)
	if len(turns) != 2 {
		t.Fatalf("legacy turns = %+v", turns)
	}
	if turns[0].Role != "user" || turns[0].Agent != "" || len(turns[0].Prov) != 0 {
		t.Errorf("legacy user = %+v", turns[0])
	}
	if turns[1].Role != "assistant" || turns[1].Agent != "claude" || len(turns[1].Prov) != 0 {
		t.Errorf("legacy assistant = %+v (agent from meta, empty prov)", turns[1])
	}
}

func TestReadEventsMalformedDoesNotShiftAndUnknownHarmless(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.jsonl")
	prov := synthProvJSON("torn")
	asst, err := json.Marshal(Event{T: "assistant", Text: "kept", Time: "t2", Prov: prov})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"t":"meta","time":"t0","meta":{"node":"n","uid":"U1","agent":"grok","created":"t0"}}` + "\n" +
		"\n" +
		"{ this is not json\n" +
		`{"t":"future-type","x":1,"prov":{"_meta":{"src":"ignored"}}}` + "\n" +
		string(asst) + "\n" +
		`{"t":"assistant","text":"torn` // torn tail, no newline close
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	evs := ReadEvents(path)
	// meta, unknown, assistant — blank/malformed/torn skipped, no ordinal shift
	// for readers that index by successfully-parsed records.
	if len(evs) != 3 || evs[0].T != "meta" || evs[1].T != "future-type" || evs[2].T != "assistant" {
		t.Fatalf("events = %+v", evs)
	}
	assertRawEqual(t, evs[2].Prov, prov)
	turns := ReadTurns(path)
	if len(turns) != 1 || turns[0].Text != "kept" || turns[0].Agent != "grok" {
		t.Fatalf("turns = %+v", turns)
	}
	assertRawEqual(t, turns[0].Prov, prov)
}

func TestSessionlogMultiContributionRoundTrip(t *testing.T) {
	prov := json.RawMessage(`[{"loc":"envelope","key":"_meta","v":{"from":"env"}},{"loc":"message","key":"provenance","v":[1,2]}]`)
	path := filepath.Join(t.TempDir(), "n.jsonl")
	w := &Writer{Path: path}
	if err := w.Append(NewMeta("n", "claude", "", "", "")); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(Event{T: "assistant", Text: "hello", Time: "t1", Prov: prov}); err != nil {
		t.Fatal(err)
	}
	mut := append(json.RawMessage(nil), prov...)
	for i := range prov {
		prov[i] = ' '
	}
	evs := ReadEvents(path)
	if len(evs) < 2 {
		t.Fatal("missing events")
	}
	assertRawEqual(t, evs[1].Prov, mut)
	turns := ReadTurns(path)
	if len(turns) != 1 || turns[0].Agent != "claude" {
		t.Fatalf("turns = %+v", turns)
	}
	assertRawEqual(t, turns[0].Prov, mut)
	seg := ReadSegment(path)
	if len(seg.Turns) != 1 || seg.Turns[0].Agent != "claude" {
		t.Fatalf("segment = %+v", seg.Turns)
	}
	assertRawEqual(t, seg.Turns[0].Prov, mut)
	hist := ReadHistory(path)
	if len(hist) != 1 || len(hist[0].Turns) != 1 {
		t.Fatalf("history = %+v", hist)
	}
	assertRawEqual(t, hist[0].Turns[0].Prov, mut)
	c := &LogCache{}
	cached := c.Segment(path)
	if !reflect.DeepEqual(cached.Turns, seg.Turns) {
		t.Fatalf("cache mismatch:\n got %+v\nwant %+v", cached.Turns, seg.Turns)
	}
	b, err := json.Marshal(turns[0])
	if err != nil {
		t.Fatal(err)
	}
	var again transcript.Turn
	if err := json.Unmarshal(b, &again); err != nil {
		t.Fatal(err)
	}
	assertRawEqual(t, again.Prov, mut)
	if again.Agent != "claude" {
		t.Errorf("json Agent = %q", again.Agent)
	}
}

func TestNewAttentionEdgeAndNewMarkShape(t *testing.T) {
	attn := NewAttentionEdge("approval", "start")
	if attn.T != "attention" || attn.Attention == nil || attn.Attention.Kind != "approval" || attn.Attention.Status != "start" {
		t.Fatalf("%+v", attn)
	}
	mark := NewMark(42)
	if mark.T != "mark" || mark.Mark == nil || mark.Mark.Off != 42 {
		t.Fatalf("%+v", mark)
	}
}

func TestReadTurnsCallerMutationDoesNotAliasStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.jsonl")
	w := &Writer{Path: path}
	if err := w.Append(NewMeta("n", "muse", "std-1", "", "/ws")); err != nil {
		t.Fatal(err)
	}
	prov := json.RawMessage(`[{"loc":"envelope","key":"_meta","v":{"src":"synth-muse"}}]`)
	if err := w.Append(Event{T: "assistant", Text: "pong", Time: "t1", Prov: prov}); err != nil {
		t.Fatal(err)
	}
	turns := ReadTurns(path)
	if len(turns) != 1 || turns[0].Agent != "muse" || len(turns[0].Prov) == 0 {
		t.Fatalf("%+v", turns)
	}
	saved := append([]byte(nil), turns[0].Prov...)
	for i := range turns[0].Prov {
		turns[0].Prov[i] = 'x'
	}
	again := ReadTurns(path)
	if !bytes.Equal(again[0].Prov, saved) {
		t.Fatalf("mutating ReadTurns result aliased stored provenance: %s", again[0].Prov)
	}
	if again[0].Agent != "muse" {
		t.Fatalf("agent=%q", again[0].Agent)
	}
}

// The same interrupted-tail contract as the node store: this file is the
// user's conversation history and is never rewritten, so a record appended
// after a truncated line must not be welded onto it and lost with it.
func TestAppendRecoversFromInterruptedTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n1.jsonl")
	if err := os.WriteFile(path, []byte(`{"t":"user","text":"first"}`+"\n"+`{"t":"assist`), 0o600); err != nil {
		t.Fatal(err)
	}
	w := &Writer{Path: path}
	if err := w.Append(Event{T: "assistant", Text: "after"}); err != nil {
		t.Fatalf("append after an interrupted tail: %v", err)
	}
	evs := ReadEvents(path)
	if len(evs) != 2 || evs[0].Text != "first" || evs[1].Text != "after" {
		t.Fatalf("replay after an interrupted tail = %+v, want the first and the new record", evs)
	}
}
