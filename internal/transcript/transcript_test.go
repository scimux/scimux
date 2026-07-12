package transcript

// The fixtures in testdata/ were hand-written from known samples of both
// CLIs' log formats. Regenerate real ones with scripts/capture-fixtures.sh
// whenever a CLI update changes its format.

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func parseFile(t *testing.T, path string) []Turn {
	t.Helper()
	tl := &Tailer{Path: path}
	return tl.Poll()
}

func TestParseClaudeFixture(t *testing.T) {
	turns := parseFile(t, "testdata/claude-session.jsonl")
	want := []Turn{
		{Role: "user", Text: "Hello agent, analyze run 42", Time: "2026-07-11T09:00:00.000Z"},
		{Role: "assistant", Text: "Looking at run 42 now.", Time: "2026-07-11T09:00:05.000Z"},
		{Role: "assistant", Text: "Run 42 shows 12% false positives on low-speed maneuvers.", Time: "2026-07-11T09:00:15.000Z"},
	}
	if !reflect.DeepEqual(turns, want) {
		t.Fatalf("turns = %#v\nwant %#v", turns, want)
	}
}

func TestParseCodexFixture(t *testing.T) {
	turns := parseFile(t, "testdata/codex-rollout.jsonl")
	want := []Turn{
		{Role: "user", Text: "Hello codex, sweep the detector thresholds", Time: "2026-07-11T10:00:02.000Z"},
		{Role: "assistant", Text: "Sweep started; 5 threshold values queued.", Time: "2026-07-11T10:00:06.000Z"},
	}
	if !reflect.DeepEqual(turns, want) {
		t.Fatalf("turns = %#v\nwant %#v", turns, want)
	}
}

// The real-* fixtures were captured from actual CLI runs (claude 2026-07,
// codex 0.139.0) via scripts/capture-fixtures.sh. They pin the parser to
// formats observed in the wild, including record types the hand-written
// fixtures don't model (queue-operation, ai-title, developer-role messages,
// thinking blocks, token_count events).

func TestParseRealClaudeFixture(t *testing.T) {
	if _, err := os.Stat("testdata/real-claude-session.jsonl"); err != nil {
		t.Skip("real fixture not captured yet (run scripts/capture-fixtures.sh)")
	}
	turns := parseFile(t, "testdata/real-claude-session.jsonl")
	if len(turns) != 2 {
		t.Fatalf("want 2 turns, got %d: %#v", len(turns), turns)
	}
	if turns[0].Role != "user" || turns[0].Text != "Reply with the single word: pong" {
		t.Errorf("user turn = %#v", turns[0])
	}
	if turns[1].Role != "assistant" || turns[1].Text != "pong" {
		t.Errorf("assistant turn = %#v", turns[1])
	}
}

func TestParseRealCodexFixture(t *testing.T) {
	if _, err := os.Stat("testdata/real-codex-rollout.jsonl"); err != nil {
		t.Skip("real fixture not captured yet (run scripts/capture-fixtures.sh)")
	}
	turns := parseFile(t, "testdata/real-codex-rollout.jsonl")
	if len(turns) != 2 {
		t.Fatalf("want 2 turns (env context and developer msg filtered), got %d: %#v", len(turns), turns)
	}
	if turns[0].Role != "user" || turns[0].Text != "Reply with the single word: pong" {
		t.Errorf("user turn = %#v", turns[0])
	}
	if turns[1].Role != "assistant" || turns[1].Text != "pong" {
		t.Errorf("assistant turn = %#v", turns[1])
	}
	// Discovery relies on session_meta carrying the cwd.
	if cwd := rolloutCwd("testdata/real-codex-rollout.jsonl"); cwd == "" {
		t.Error("rolloutCwd found no cwd in real session_meta")
	}
}

func TestGarbageInEmptyOutNoPanic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "garbage.jsonl")
	os.WriteFile(path, []byte("{{{{\n\x00\xff binary\n{\"type\":\"unknown_future_type\",\"payload\":{}}\n"), 0o644)
	if turns := parseFile(t, path); len(turns) != 0 {
		t.Fatalf("garbage produced turns: %#v", turns)
	}
	// Missing file: no turns, no error, no panic.
	missing := &Tailer{Path: filepath.Join(dir, "does-not-exist.jsonl")}
	if turns := missing.Poll(); len(turns) != 0 {
		t.Fatalf("missing file produced turns: %#v", turns)
	}
}

func TestTailerIncrementalAndPartialLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "live.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tl := &Tailer{Path: path}

	f.WriteString(`{"type":"user","timestamp":"t1","message":{"role":"user","content":"first"}}` + "\n")
	if turns := tl.Poll(); len(turns) != 1 || turns[0].Text != "first" {
		t.Fatalf("after first line: %#v", turns)
	}
	// Append half a line: must not be parsed yet…
	half := `{"type":"assistant","timestamp":"t2","message":{"role":"assist`
	f.WriteString(half)
	if turns := tl.Poll(); len(turns) != 1 {
		t.Fatalf("partial line parsed prematurely: %#v", turns)
	}
	// …until completed.
	f.WriteString(`ant","content":[{"type":"text","text":"second"}]}}` + "\n")
	turns := tl.Poll()
	if len(turns) != 2 || turns[1].Text != "second" {
		t.Fatalf("after completing line: %#v", turns)
	}
	// Idempotent when nothing new arrived.
	if turns := tl.Poll(); len(turns) != 2 {
		t.Fatalf("re-poll changed result: %#v", turns)
	}
}

func TestFindClaudeTranscript(t *testing.T) {
	home := t.TempDir()
	proj := filepath.Join(home, ".claude", "projects", "-data-exp1")
	os.MkdirAll(proj, 0o755)
	id := "11111111-2222-3333-4444-555555555555"
	os.WriteFile(filepath.Join(proj, id+".jsonl"), []byte("{}\n"), 0o644)
	os.WriteFile(filepath.Join(proj, "other.jsonl"), []byte("{}\n"), 0o644)

	got, ok := FindClaudeTranscript(home, id)
	if !ok || got != filepath.Join(proj, id+".jsonl") {
		t.Fatalf("got %q ok=%v", got, ok)
	}
	if _, ok := FindClaudeTranscript(home, "not-a-real-id"); ok {
		t.Fatal("found transcript for unknown session id")
	}
}

func TestFindClaudeNewestInDir(t *testing.T) {
	home := t.TempDir()
	proj := filepath.Join(home, ".claude", "projects", "-data-my-exp-1")
	os.MkdirAll(proj, 0o755)
	older := filepath.Join(proj, "aaaaaaaa-0000-0000-0000-000000000001.jsonl")
	newer := filepath.Join(proj, "bbbbbbbb-0000-0000-0000-000000000002.jsonl")
	os.WriteFile(older, []byte("{}\n"), 0o644)
	os.WriteFile(newer, []byte("{}\n"), 0o644)
	past := time.Now().Add(-2 * time.Hour)
	os.Chtimes(older, past, past)

	path, sid, ok := FindClaudeNewestInDir(home, "/data/my/exp_1")
	if !ok || path != newer || sid != "bbbbbbbb-0000-0000-0000-000000000002" {
		t.Fatalf("got %q sid=%q ok=%v", path, sid, ok)
	}
	if _, _, ok := FindClaudeNewestInDir(home, "/data/unknown"); ok {
		t.Fatal("unknown dir must not match")
	}
}

func TestFindCodexRollout(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "2026", "07", "11")
	os.MkdirAll(day, 0o755)
	meta := func(cwd string) string {
		return `{"timestamp":"x","type":"session_meta","payload":{"id":"i","cwd":"` + cwd + `"}}` + "\n"
	}
	old := filepath.Join(day, "rollout-old.jsonl")
	match := filepath.Join(day, "rollout-match.jsonl")
	otherCwd := filepath.Join(day, "rollout-other.jsonl")
	os.WriteFile(old, []byte(meta("/data/exp1")), 0o644)
	os.WriteFile(match, []byte(meta("/data/exp1")), 0o644)
	os.WriteFile(otherCwd, []byte(meta("/data/exp2")), 0o644)
	// Make "old" predate the search window.
	past := time.Now().Add(-1 * time.Hour)
	os.Chtimes(old, past, past)

	got, ok := FindCodexRollout(root, "/data/exp1", time.Now().Add(-time.Minute), nil)
	if !ok || got != match {
		t.Fatalf("got %q ok=%v, want %q", got, ok, match)
	}
	if _, ok := FindCodexRollout(root, "/data/nomatch", time.Now().Add(-time.Minute), nil); ok {
		t.Fatal("matched a rollout for the wrong cwd")
	}
}

// Two same-cwd nodes starting close together must resolve to two different
// rollouts: claimed paths are excluded, and the oldest-by-filename candidate
// wins, so discovery in node start order pairs each node with its own file
// (finding 3 of the 2026-07 review).
func TestFindCodexRolloutConcurrentSameDir(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "2026", "07", "12")
	os.MkdirAll(day, 0o755)
	meta := `{"timestamp":"x","type":"session_meta","payload":{"id":"i","cwd":"/data/exp1"}}` + "\n"
	first := filepath.Join(day, "rollout-2026-07-12T10-00-00-aaa.jsonl")
	second := filepath.Join(day, "rollout-2026-07-12T10-00-05-bbb.jsonl")
	os.WriteFile(first, []byte(meta), 0o644)
	os.WriteFile(second, []byte(meta), 0o644)
	since := time.Now().Add(-time.Minute)

	// Node A (started first, discovered first) takes the older rollout…
	gotA, ok := FindCodexRollout(root, "/data/exp1", since, nil)
	if !ok || gotA != first {
		t.Fatalf("node A got %q ok=%v, want %q", gotA, ok, first)
	}
	// …and node B, with A's path claimed, takes the remaining one.
	gotB, ok := FindCodexRollout(root, "/data/exp1", since, map[string]bool{gotA: true})
	if !ok || gotB != second {
		t.Fatalf("node B got %q ok=%v, want %q", gotB, ok, second)
	}
	// With both claimed, nothing is left to link.
	if _, ok := FindCodexRollout(root, "/data/exp1", since,
		map[string]bool{first: true, second: true}); ok {
		t.Fatal("returned a claimed rollout")
	}
}

// An older same-cwd rollout whose mtime advances after launch (another
// session outside scimux still appending to it) must not beat the node's
// own new rollout: the pre-launch snapshot excludes it regardless of mtime
// and of its older filename (finding 14 of the 2026-07 review).
func TestFindCodexRolloutExcludesPreexisting(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "2026", "07", "12")
	os.MkdirAll(day, 0o755)
	meta := `{"timestamp":"x","type":"session_meta","payload":{"id":"i","cwd":"/data/exp1"}}` + "\n"
	oldRollout := filepath.Join(day, "rollout-2026-07-12T08-00-00-old.jsonl")
	os.WriteFile(oldRollout, []byte(meta), 0o644)
	snapshot, complete := ListCodexRollouts(root) // taken at launch: only the old file exists
	if !complete || !snapshot[oldRollout] {
		t.Fatalf("snapshot must be complete and contain the pre-launch rollout (complete=%v)", complete)
	}
	newRollout := filepath.Join(day, "rollout-2026-07-12T10-00-00-new.jsonl")
	os.WriteFile(newRollout, []byte(meta), 0o644)
	// The outside session keeps appending: the old file's mtime is fresh.
	now := time.Now()
	os.Chtimes(oldRollout, now, now)
	got, ok := FindCodexRollout(root, "/data/exp1", now.Add(-time.Minute), snapshot)
	if !ok || got != newRollout {
		t.Fatalf("got %q ok=%v, want the newly appeared %q", got, ok, newRollout)
	}
}

// A transcript that produced valid turns and then stops making sense must
// flag Unparseable so the UI can degrade to the pane snapshot — while
// benign unknown *typed* records never trip the signal (finding 15).
func TestTailerUnparseableAfterValidTurns(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	valid := `{"type":"user","timestamp":"t1","message":{"role":"user","content":"question"}}` + "\n" +
		`{"type":"assistant","timestamp":"t2","message":{"role":"assistant","content":"answer"}}` + "\n"
	os.WriteFile(p, []byte(valid), 0o644)
	tl := &Tailer{Path: p}
	if turns := tl.Poll(); len(turns) != 2 || tl.Unparseable() {
		t.Fatalf("valid prefix: turns=%d unparseable=%v", len(turns), tl.Unparseable())
	}
	append_ := func(line string, n int) {
		f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < n; i++ {
			f.WriteString(line + "\n")
		}
		f.Close()
	}
	// Unknown but typed records are normal defensive-parser territory.
	append_(`{"type":"future_record_kind","payload":{"x":1}}`, 30)
	tl.Poll()
	if tl.Unparseable() {
		t.Fatal("unknown typed records must not mark the transcript unparseable")
	}
	// A real format break: recent appended data is not interpretable at all.
	append_("this is no longer a transcript line", 12)
	if turns := tl.Poll(); len(turns) != 2 {
		t.Fatalf("earlier turns must survive, got %d", len(turns))
	}
	if !tl.Unparseable() {
		t.Fatal("garbage tail must mark the transcript unparseable")
	}
	// Recovery: recognizable data resets the streak.
	append_(`{"type":"assistant","timestamp":"t3","message":{"role":"assistant","content":"back"}}`, 1)
	tl.Poll()
	if tl.Unparseable() {
		t.Fatal("streak must reset once recognizable data resumes")
	}
}

// The session id embedded in a rollout filename is codex's own, so matching
// on it is deterministic — the safe correlation for adopted/resumed sessions
// where no pre-launch snapshot exists (finding 20).
func TestFindCodexRolloutBySession(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "2026", "07", "12")
	os.MkdirAll(day, 0o755)
	id := "00000000-0000-7000-8000-000000000001"
	want := filepath.Join(day, "rollout-2026-07-12T10-00-00-"+id+".jsonl")
	other := filepath.Join(day, "rollout-2026-07-12T09-00-00-ffffffff-0000-0000-0000-000000000000.jsonl")
	os.WriteFile(want, []byte("{}\n"), 0o644)
	os.WriteFile(other, []byte("{}\n"), 0o644)

	got, ok := FindCodexRolloutBySession(root, id)
	if !ok || got != want {
		t.Fatalf("got %q ok=%v, want %q", got, ok, want)
	}
	// Case-insensitive: ids extracted from a command line may be uppercased.
	if got, ok := FindCodexRolloutBySession(root, strings.ToUpper(id)); !ok || got != want {
		t.Fatalf("uppercase id: got %q ok=%v", got, ok)
	}
	if _, ok := FindCodexRolloutBySession(root, "11111111-2222-3333-4444-555555555555"); ok {
		t.Fatal("found a rollout for an unknown session id")
	}
}

// The snapshot's safety property needs a full walk: a traversal error must
// be reported as incomplete, while a missing root (no codex history at all)
// is a complete, empty snapshot (finding 22).
func TestListCodexRolloutsCompleteness(t *testing.T) {
	if snap, complete := ListCodexRollouts(filepath.Join(t.TempDir(), "absent")); !complete || len(snap) != 0 {
		t.Fatalf("missing root: complete=%v len=%d, want complete and empty", complete, len(snap))
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: permissions cannot make a directory unreadable")
	}
	root := t.TempDir()
	sealed := filepath.Join(root, "2026", "07", "12")
	os.MkdirAll(sealed, 0o755)
	os.WriteFile(filepath.Join(sealed, "rollout-hidden.jsonl"), []byte("{}\n"), 0o644)
	if err := os.Chmod(sealed, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(sealed, 0o755)
	if _, complete := ListCodexRollouts(root); complete {
		t.Fatal("unreadable subtree must mark the snapshot incomplete")
	}
}

// A future format that keeps emitting typed JSON objects but moves visible
// messages to a new shape must be detectable: bytes advance while the
// chat-record count stands still. Structural health (Unparseable) cannot see
// this case — that is exactly why Progress exists (finding 21).
func TestTailerProgressTypedFormatChange(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	os.WriteFile(p, []byte(
		`{"type":"user","timestamp":"t1","message":{"role":"user","content":"question"}}`+"\n"+
			`{"type":"assistant","timestamp":"t2","message":{"role":"assistant","content":"answer"}}`+"\n"), 0o644)
	tl := &Tailer{Path: p}
	tl.Poll()
	off1, prog1 := tl.Progress()
	if prog1 != 1 || off1 == 0 {
		t.Fatalf("valid prefix: offset=%d agentRecords=%d, want offset>0 and 1 record (the assistant turn; the user prompt is not agent progress)", off1, prog1)
	}
	append_ := func(lines ...string) {
		f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range lines {
			f.WriteString(l + "\n")
		}
		f.Close()
	}
	// The hypothetical next CLI release: still JSONL, still typed, but the
	// message shape moved — no turn, no tool call this parser recognizes.
	append_(`{"type":"message_v2","timestamp":"t3","body":{"speaker":"assistant","parts":[{"kind":"text","value":"new shape"}]}}`)
	tl.Poll()
	off2, prog2 := tl.Progress()
	if off2 <= off1 {
		t.Fatalf("offset did not advance: %d -> %d", off1, off2)
	}
	if prog2 != prog1 {
		t.Fatalf("typed future message counted as agent progress: %d -> %d", prog1, prog2)
	}
	if tl.Unparseable() {
		t.Fatal("typed records must not trip the structural health signal")
	}
	// Known agent-side shapes — including pure tool activity with no visible
	// message — do count as progress.
	append_(`{"type":"assistant","timestamp":"t4","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{}}]}}`)
	tl.Poll()
	if _, prog3 := tl.Progress(); prog3 != prog2+1 {
		t.Fatalf("tool call not counted as agent progress: %d -> %d", prog2, prog3)
	}
	// Benign side records (titles, token counts) are not progress — and that
	// alone must never mark anything unhealthy.
	append_(`{"type":"ai-title","title":"threshold sweep"}`)
	tl.Poll()
	if _, prog4 := tl.Progress(); prog4 != prog2+1 {
		t.Fatalf("side record counted as agent progress")
	}
}

// Progress must report the committed watermark, not raw bytes read: a
// partial trailing line buffered across polls is not data the parser failed
// on, and when the record completes later — perhaps while the pane is
// already quiet — it counts as progress at that moment (finding 24).
func TestTailerProgressPartialLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	full := `{"type":"assistant","timestamp":"t1","message":{"role":"assistant","content":"answer"}}` + "\n"
	os.WriteFile(p, []byte(full[:40]), 0o644)
	tl := &Tailer{Path: p}
	tl.Poll()
	if off, prog := tl.Progress(); off != 0 || prog != 0 {
		t.Fatalf("buffered partial line must not advance the watermark: off=%d prog=%d", off, prog)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(full[40:])
	f.Close()
	if turns := tl.Poll(); len(turns) != 1 {
		t.Fatalf("completed record must yield its turn, got %d", len(turns))
	}
	if off, prog := tl.Progress(); off != int64(len(full)) || prog != 1 {
		t.Fatalf("completed record must commit: off=%d prog=%d, want %d and 1", off, prog, len(full))
	}
}

// agentShaped is directional and strict: only understood *agent-side*
// records count — visible assistant text, trackable tool calls/results —
// never user prompts, meta records, or injected scaffolding, which would
// otherwise mask an assistant format change during the same phase
// (findings 24, 27).
func TestAgentShapedDirectional(t *testing.T) {
	yes := []string{
		`{"type":"assistant","message":{"role":"assistant","content":"a"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"a"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"1","name":"Bash","input":{}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"1","content":"ok"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}}`,
		`{"type":"response_item","payload":{"type":"function_call","call_id":"c1","name":"exec"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":"done"}}`,
	}
	for _, l := range yes {
		if !agentShaped([]byte(l)) {
			t.Errorf("understood agent-side record not counted: %s", l)
		}
	}
	no := []string{
		// User-side records must not vouch for the response format (finding 27):
		`{"type":"user","message":{"role":"user","content":"a real human prompt"}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"typed answer"}]}}`,
		`{"type":"user","isMeta":true,"message":{"role":"user","content":"<local-command-caveat>x</local-command-caveat>"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>…</environment_context>"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"sweep the thresholds"}]}}`,
		// Known outer type, moved inner shape — no claimed understanding:
		`{"type":"assistant","message":{"role":"assistant","content":{"v2_rich":"hi"}}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"kind":"txt","value":"x"}]}}`,
		`{"type":"assistant","message":{"role":"assistant"}}`,
		`{"type":"assistant","isMeta":true,"message":{"role":"assistant","content":"meta"}}`,
		`{"type":"response_item","payload":{"type":"message","content":[{"type":"output_text","text":"hi"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"tool","content":"x"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"kind":"v2","data":"x"}]}}`,
		`{"type":"response_item","payload":{"type":"function_call"}}`,
		`{"type":"ai-title","title":"threshold sweep"}`,
		`not json`,
	}
	for _, l := range no {
		if agentShaped([]byte(l)) {
			t.Errorf("record counted as agent progress but must not be: %s", l)
		}
	}
}

// FindCodexRolloutsForDir enumerates a cwd's rollouts for the migration
// workflow: JSON-parsed cwd matching (no raw substring games), session ids
// from the filename, newest first (finding 26).
func TestFindCodexRolloutsForDir(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "2026", "07", "12")
	os.MkdirAll(day, 0o755)
	meta := func(cwd string) string {
		// Whitespace after the colon: raw substring matching for "cwd":"…"
		// would miss this; JSON parsing must not.
		return `{"timestamp":"x","type":"session_meta","payload":{"id":"i", "cwd": "` + cwd + `"}}` + "\n"
	}
	idOld := "aaaaaaaa-0000-0000-0000-000000000001"
	idNew := "bbbbbbbb-0000-0000-0000-000000000002"
	older := filepath.Join(day, "rollout-2026-07-12T08-00-00-"+idOld+".jsonl")
	newer := filepath.Join(day, "rollout-2026-07-12T10-00-00-"+idNew+".jsonl")
	other := filepath.Join(day, "rollout-2026-07-12T09-00-00-cccccccc-0000-0000-0000-000000000003.jsonl")
	os.WriteFile(older, []byte(meta("/data/exp1")), 0o644)
	os.WriteFile(newer, []byte(meta("/data/exp1")), 0o644)
	os.WriteFile(other, []byte(meta("/data/exp2")), 0o644)
	past := time.Now().Add(-2 * time.Hour)
	os.Chtimes(older, past, past)

	got := FindCodexRolloutsForDir(root, "/data/exp1")
	if len(got) != 2 {
		t.Fatalf("want 2 candidates, got %d: %#v", len(got), got)
	}
	if got[0].Path != newer || got[0].SessionID != idNew {
		t.Errorf("newest first: got %q id %q", got[0].Path, got[0].SessionID)
	}
	if got[1].Path != older || got[1].SessionID != idOld {
		t.Errorf("older second: got %q id %q", got[1].Path, got[1].SessionID)
	}
	if extra := FindCodexRolloutsForDir(root, "/data/nomatch"); len(extra) != 0 {
		t.Errorf("unrelated cwd matched: %#v", extra)
	}
}

// WaitingOn is the transcript half of needs-input detection: a logged tool
// call whose result has not been logged yet. Verified against real session
// logs: both CLIs write the call record when the agent asks, and the result
// record only after the human answers (gaps of minutes to hours observed).

func TestWaitingOnClaude(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "w.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tl := &Tailer{Path: path}

	// Agent issues a Bash call: pending until the result lands.
	f.WriteString(`{"type":"assistant","timestamp":"t1","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"ls"}}]}}` + "\n")
	tl.Poll()
	if name, ok := tl.WaitingOn(); !ok || name != "Bash" {
		t.Fatalf("WaitingOn = %q,%v, want Bash,true", name, ok)
	}
	f.WriteString(`{"type":"user","timestamp":"t2","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"ok"}]}}` + "\n")
	tl.Poll()
	if name, ok := tl.WaitingOn(); ok {
		t.Fatalf("resolved call still pending: %q", name)
	}

	// An explicit question tool stays pending, keeping its name so the
	// caller can tell "question" from "approval".
	f.WriteString(`{"type":"assistant","timestamp":"t3","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu2","name":"AskUserQuestion","input":{}}]}}` + "\n")
	tl.Poll()
	if name, ok := tl.WaitingOn(); !ok || name != "AskUserQuestion" {
		t.Fatalf("WaitingOn = %q,%v, want AskUserQuestion,true", name, ok)
	}
	// A plain user turn (answer typed in the TUI, or an interrupt notice)
	// means the human already acted: nothing stays pending.
	f.WriteString(`{"type":"user","timestamp":"t4","message":{"role":"user","content":"[Request interrupted by user]"}}` + "\n")
	tl.Poll()
	if name, ok := tl.WaitingOn(); ok {
		t.Fatalf("user turn did not clear pending: %q", name)
	}
}

func TestWaitingOnCodex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tl := &Tailer{Path: path}

	f.WriteString(`{"timestamp":"t1","type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"c1","arguments":"{}"}}` + "\n")
	tl.Poll()
	if name, ok := tl.WaitingOn(); !ok || name != "exec_command" {
		t.Fatalf("WaitingOn = %q,%v, want exec_command,true", name, ok)
	}
	f.WriteString(`{"timestamp":"t2","type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":"done"}}` + "\n")
	tl.Poll()
	if name, ok := tl.WaitingOn(); ok {
		t.Fatalf("resolved call still pending: %q", name)
	}

	// task_complete / turn_aborted are explicit turn boundaries: whatever is
	// still unresolved (e.g. after an interrupt) is moot.
	f.WriteString(`{"timestamp":"t3","type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"c2","arguments":"{}"}}` + "\n")
	f.WriteString(`{"timestamp":"t4","type":"event_msg","payload":{"type":"turn_aborted","reason":"interrupted"}}` + "\n")
	tl.Poll()
	if name, ok := tl.WaitingOn(); ok {
		t.Fatalf("turn boundary did not clear pending: %q", name)
	}
}

func TestTailerUsageClaude(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	f, _ := os.Create(p)
	defer f.Close()
	tl := &Tailer{Path: p}
	if used, window := tl.Usage(); used != 0 || window != 0 {
		t.Fatalf("fresh tailer usage = %d/%d, want 0/0", used, window)
	}
	// Usage rides on every assistant record; the latest one wins.
	f.WriteString(`{"type":"assistant","timestamp":"t1","message":{"role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":10,"cache_read_input_tokens":50000,"cache_creation_input_tokens":2000,"output_tokens":300}}}` + "\n")
	tl.Poll()
	if used, window := tl.Usage(); used != 52310 || window != 0 {
		t.Fatalf("usage = %d/%d, want 52310/0 (claude logs no window)", used, window)
	}
	f.WriteString(`{"type":"assistant","timestamp":"t2","message":{"role":"assistant","content":[{"type":"text","text":"more"}],"usage":{"input_tokens":20,"cache_read_input_tokens":60000,"cache_creation_input_tokens":0,"output_tokens":100}}}` + "\n")
	tl.Poll()
	if used, _ := tl.Usage(); used != 60120 {
		t.Fatalf("usage after second turn = %d, want 60120 (latest wins)", used)
	}
}

func TestTailerUsageCodex(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	f, _ := os.Create(p)
	defer f.Close()
	tl := &Tailer{Path: p}
	f.WriteString(`{"type":"event_msg","timestamp":"t1","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":999999,"output_tokens":999},"last_token_usage":{"input_tokens":40000,"cached_input_tokens":30000,"output_tokens":500},"model_context_window":272000}}}` + "\n")
	tl.Poll()
	used, window := tl.Usage()
	if used != 70500 || window != 272000 {
		t.Fatalf("usage = %d/%d, want 70500/272000 (last request, not lifetime total)", used, window)
	}
	// token_count is a benign side record: it must not count as progress or
	// clear the health signals.
	if _, prog := tl.Progress(); prog != 0 {
		t.Fatalf("token_count counted as agent progress: %d", prog)
	}
}
