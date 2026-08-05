package transcript

// The fixtures in testdata/ were hand-written from known samples of both
// CLIs' log formats. Regenerate real ones with scripts/capture-fixtures.sh
// whenever a CLI update changes its format.

import (
	"os"
	"path/filepath"
	"reflect"
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

// --- Phase 3: Claude usage breakdown retention (fare-design.md §2.2, Phase 3) ---
// Synthetic assistant records only. The four billable fields must be retained
// alongside the occupancy sum; ctxUsed formula is unchanged.

func TestTailer_UsageBreakdownRetainsSplit(t *testing.T) {
	// §2.2 live shape: fresh input, cache-create, cache-read, output.
	// Occupancy sum must stay input+cache_read+cache_create+output (52310).
	p := filepath.Join(t.TempDir(), "s.jsonl")
	f, _ := os.Create(p)
	defer f.Close()
	tl := &Tailer{Path: p}
	f.WriteString(`{"type":"assistant","timestamp":"t1","message":{"id":"msg_split","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":10,"cache_read_input_tokens":50000,"cache_creation_input_tokens":2000,"output_tokens":300}}}` + "\n")
	tl.Poll()

	// Occupancy sum is byte-for-byte the pre-Phase-3 formula.
	if used, _ := tl.Usage(); used != 52310 {
		t.Fatalf("ctxUsed = %d, want 52310 (occupancy sum unchanged)", used)
	}
	bd := tl.UsageBreakdown()
	if bd.InputTokens != 10 {
		t.Errorf("InputTokens = %d, want 10 (fresh)", bd.InputTokens)
	}
	if bd.OutputTokens != 300 {
		t.Errorf("OutputTokens = %d, want 300", bd.OutputTokens)
	}
	if bd.CachedReadTokens != 50000 {
		t.Errorf("CachedReadTokens = %d, want 50000", bd.CachedReadTokens)
	}
	if bd.CacheCreationTokens != 2000 {
		t.Errorf("CacheCreationTokens = %d, want 2000", bd.CacheCreationTokens)
	}
}

func TestTailer_CacheCreationFlatShape(t *testing.T) {
	// Flat field: cache_creation_input_tokens:25437 (the common Claude shape).
	p := filepath.Join(t.TempDir(), "s.jsonl")
	f, _ := os.Create(p)
	defer f.Close()
	tl := &Tailer{Path: p}
	f.WriteString(`{"type":"assistant","timestamp":"t1","message":{"id":"msg_flat","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":2,"cache_creation_input_tokens":25437,"cache_read_input_tokens":0,"output_tokens":410}}}` + "\n")
	tl.Poll()
	bd := tl.UsageBreakdown()
	if bd.CacheCreationTokens != 25437 {
		t.Fatalf("CacheCreationTokens = %d, want 25437 (flat shape)", bd.CacheCreationTokens)
	}
	// Occupancy still uses the flat field in its sum: 2+0+25437+410 = 25849.
	if used, _ := tl.Usage(); used != 25849 {
		t.Fatalf("ctxUsed = %d, want 25849", used)
	}
}

func TestTailer_CacheCreationEphemeralShape(t *testing.T) {
	// Nested shape only: cache_creation:{ephemeral_5m:100, ephemeral_1h:20} → 120.
	// Flat field absent. Breakdown must sum nested; ctxUsed stays flat-only
	// (no cache-create contribution → 2+0+0+10 = 12), matching pre-Phase-3.
	p := filepath.Join(t.TempDir(), "s.jsonl")
	f, _ := os.Create(p)
	defer f.Close()
	tl := &Tailer{Path: p}
	f.WriteString(`{"type":"assistant","timestamp":"t1","message":{"id":"msg_eph","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":2,"cache_read_input_tokens":0,"output_tokens":10,"cache_creation":{"ephemeral_5m":100,"ephemeral_1h":20}}}}` + "\n")
	tl.Poll()
	bd := tl.UsageBreakdown()
	if bd.CacheCreationTokens != 120 {
		t.Fatalf("CacheCreationTokens = %d, want 120 (ephemeral sum)", bd.CacheCreationTokens)
	}
	if used, _ := tl.Usage(); used != 12 {
		t.Fatalf("ctxUsed = %d, want 12 (flat-only occupancy formula unchanged)", used)
	}
}

func TestTailer_TurnIDFromMessageAndRequest(t *testing.T) {
	// ccusage dedup identity: message id + request id (D3). Must be stable
	// across an independent re-read of the same record.
	p := filepath.Join(t.TempDir(), "s.jsonl")
	line := `{"type":"assistant","timestamp":"t1","requestId":"req-xyz","message":{"id":"msg-abc","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}` + "\n"
	if err := os.WriteFile(p, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	tl1 := &Tailer{Path: p}
	tl1.Poll()
	id1 := tl1.UsageBreakdown().TurnID
	if id1 == "" {
		t.Fatal("TurnID empty; want message id + request id")
	}
	if id1 != "msg-abc:req-xyz" {
		t.Fatalf("TurnID = %q, want %q", id1, "msg-abc:req-xyz")
	}
	// Independent re-read of the same file yields the same TurnID.
	tl2 := &Tailer{Path: p}
	tl2.Poll()
	id2 := tl2.UsageBreakdown().TurnID
	if id2 != id1 {
		t.Fatalf("TurnID not stable across re-read: %q vs %q", id1, id2)
	}
}
