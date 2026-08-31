package transcript

// The fixtures in testdata/ were hand-written from known samples of both
// CLIs' log formats. Regenerate real ones with scripts/capture-fixtures.sh
// whenever a CLI update changes its format.

import (
	"encoding/json"
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

// A Claude record's message.role is CLI-authored data, not a scimux constant.
// Only "user" and "assistant" are visible chat roles; anything else is an
// unknown shape and is silently ignored, exactly as the Codex branch already
// treats an unexpected payload.role. Found by FuzzParseLine.
//
// This matters beyond tidiness because mirror.go writes Turn.Role straight
// into sessionlog.Event.T, a closed set that includes "source" — the /clear
// page-turn marker and the mirror's dedupe watermark in an append-only store.
func TestParseLineRejectsUnknownClaudeRole(t *testing.T) {
	for _, line := range []string{
		`{"type":"user","message":{"role":"system","content":"hello"}}`,
		`{"type":"user","message":{"role":"source","content":"hello"}}`,
		`{"type":"assistant","message":{"role":"tool","content":"hello"}}`,
		`{"type":"user","message":{"role":"0","content":"0"}}`,
		// encoding/json matches field names case-insensitively, so the same
		// record reaches the Claude branch under mixed-case keys.
		`{"tYpe":"user","messAge":{"role":"0","Content":"0"}}`,
		// The scaffolding filter is keyed on role == "user"; an unknown role
		// must not smuggle an injected block through as a visible turn.
		`{"type":"user","message":{"role":"x","content":"<user_instructions>secret"}}`,
		// Both roles are individually recognized, but they contradict the
		// envelope. Real Claude output never disagrees (42,988 records across
		// 173 captured transcripts, checked 2026-08-31), so a disagreement is
		// an unknown shape. Accepting it would misattribute a turn — and,
		// because the scaffolding filter keys on role == "user", the second
		// form below would render an injected block as agent prose.
		`{"type":"assistant","message":{"role":"user","content":"I am the agent"}}`,
		`{"type":"user","message":{"role":"assistant","content":"<user_instructions>secret"}}`,
	} {
		if got, ok := ParseLine([]byte(line)); ok {
			t.Errorf("ParseLine(%s) = (%+v, true), want ignored", line, got)
		}
	}
	// The role fallback for a record that omits message.role stays intact:
	// the envelope type is already constrained to user/assistant.
	for _, tc := range []struct{ line, want string }{
		{`{"type":"user","message":{"content":"hello"}}`, "user"},
		{`{"type":"assistant","message":{"content":"hello"}}`, "assistant"},
	} {
		got, ok := ParseLine([]byte(tc.line))
		if !ok || got.Role != tc.want {
			t.Errorf("ParseLine(%s) = (%+v, %v), want role %q", tc.line, got, ok, tc.want)
		}
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

func TestFindClaudeTranscriptRequiresUniqueMatch(t *testing.T) {
	// AT-BIND-13: a known UUID with exactly one path still binds; two files
	// with the same UUID basename bind nothing.
	home := t.TempDir()
	id := "11111111-2222-3333-4444-555555555556"
	p1 := filepath.Join(home, ".claude", "projects", "-w-one")
	p2 := filepath.Join(home, ".claude", "projects", "-w-two")
	if err := os.MkdirAll(p1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p2, 0o755); err != nil {
		t.Fatal(err)
	}
	one := filepath.Join(p1, id+".jsonl")
	if err := os.WriteFile(one, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := FindClaudeTranscript(home, id)
	if !ok || got != one {
		t.Fatalf("unique match = %q ok=%v, want %q", got, ok, one)
	}
	if err := os.WriteFile(filepath.Join(p2, id+".jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if path, ok := FindClaudeTranscript(home, id); ok {
		t.Fatalf("duplicate UUID matches must bind nothing, got %q", path)
	}
}

func TestClaudeBridgeReady(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	id := "11111111-2222-3333-4444-555555555555"
	lines := []byte(
		`{"type":"system","subtype":"bridge_status","sessionId":"other","content":"active"}` + "\n" +
			`{"type":"system","subtype":"different","sessionId":"` + id + `"}` + "\n" +
			`not json` + "\n")
	if err := os.WriteFile(path, lines, 0o600); err != nil {
		t.Fatal(err)
	}
	if ClaudeBridgeReady(path, id) {
		t.Fatal("wrong-session and unknown records must not report readiness")
	}
	appendLines := `{"type":"system","subtype":"bridge_status","sessionId":"` + id + `","content":"localized text may change"}` + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(appendLines); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if !ClaudeBridgeReady(path, id) {
		t.Fatal("matching bridge_status must report readiness without matching prose")
	}
	if ClaudeBridgeReady(filepath.Join(dir, "missing.jsonl"), id) {
		t.Fatal("missing transcript must degrade to not ready")
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

// Delivered / lastRole is the sibling of Owing used for the map's "turn
// finished" state (P5 item 10). Every site that assigns t.owing must also
// assign lastRole so Delivered() is true only after a recognized assistant
// record — never before anything is recognized (Owing is also false then).
func TestLastRoleAndDelivered(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "role.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tl := &Tailer{Path: path}

	// Empty: not owing, not delivered — lastRole is the discriminator.
	if tl.Owing() || tl.Delivered() {
		t.Fatal("empty file: Owing/Delivered must both be false")
	}

	// Site: Claude user/assistant branch in notePending (and ParseLine).
	f.WriteString(`{"type":"user","timestamp":"t1","message":{"role":"user","content":"please edit hello.txt"}}` + "\n")
	tl.Poll()
	if !tl.Owing() || tl.Delivered() {
		t.Fatalf("after user: Owing=%v Delivered=%v, want true/false", tl.Owing(), tl.Delivered())
	}

	f.WriteString(`{"type":"assistant","timestamp":"t2","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}` + "\n")
	tl.Poll()
	if tl.Owing() || !tl.Delivered() {
		t.Fatalf("after assistant text: Owing=%v Delivered=%v, want false/true", tl.Owing(), tl.Delivered())
	}

	// Site: Claude assistant tool_use (owing cleared in notePending; no ParseLine turn).
	f.WriteString(`{"type":"assistant","timestamp":"t3","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{}}]}}` + "\n")
	tl.Poll()
	if tl.Owing() || !tl.Delivered() {
		t.Fatalf("after tool_use: Owing=%v Delivered=%v, want false/true", tl.Owing(), tl.Delivered())
	}
	if tl.PendingCount() == 0 {
		t.Fatal("tool_use should leave a pending call")
	}

	// Site: Claude user tool_result (owing true).
	f.WriteString(`{"type":"user","timestamp":"t4","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"ok"}]}}` + "\n")
	tl.Poll()
	if !tl.Owing() || tl.Delivered() {
		t.Fatalf("after tool_result: Owing=%v Delivered=%v, want true/false", tl.Owing(), tl.Delivered())
	}

	// Unknown typed record: leave lastRole/owing unchanged.
	f.WriteString(`{"type":"bridge-session","timestamp":"t5","payload":{}}` + "\n")
	tl.Poll()
	if !tl.Owing() || tl.Delivered() {
		t.Fatal("unknown record must leave lastRole/owing unchanged (still user/owing)")
	}

	// Site: Codex function_call → assistant (not owing, delivered).
	f.WriteString(`{"timestamp":"t6","type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"c1","arguments":"{}"}}` + "\n")
	tl.Poll()
	if tl.Owing() || !tl.Delivered() {
		t.Fatalf("after codex function_call: Owing=%v Delivered=%v, want false/true", tl.Owing(), tl.Delivered())
	}

	// Site: Codex function_call_output → user (owing, not delivered).
	f.WriteString(`{"timestamp":"t7","type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":"ok"}}` + "\n")
	tl.Poll()
	if !tl.Owing() || tl.Delivered() {
		t.Fatalf("after codex function_call_output: Owing=%v Delivered=%v, want true/false", tl.Owing(), tl.Delivered())
	}

	// Site: ParseLine visible Codex message (assistant).
	f.WriteString(`{"timestamp":"t8","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"all done"}]}}` + "\n")
	tl.Poll()
	if tl.Owing() || !tl.Delivered() {
		t.Fatalf("after codex assistant message: Owing=%v Delivered=%v, want false/true", tl.Owing(), tl.Delivered())
	}

	// Site: rotation (shrunken file) resets lastRole. This one must start from
	// *delivered* and rotate to empty: only then does the reset decide the
	// answer. Rotating into a file whose content sets a role would pass with
	// no reset at all, because replaying that content overwrites lastRole —
	// which is why the two rotation checks below cannot cover this site.
	// Concretely: without the reset, a /clear-rotated log keeps claiming the
	// pre-clear assistant turn, and the map calls the fresh chat "Ready".
	if !tl.Delivered() {
		t.Fatal("precondition: rotation check must start from a delivered tailer")
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	tl.Poll()
	if tl.Owing() || tl.Delivered() {
		t.Fatalf("after rotation from delivered to empty: Owing=%v Delivered=%v, want false/false",
			tl.Owing(), tl.Delivered())
	}

	// Rotation into fresh content: the replayed record decides the role.
	if err := os.WriteFile(path, []byte(`{"type":"user","timestamp":"r1","message":{"role":"user","content":"fresh after rotate"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tl.Poll()
	if !tl.Owing() || tl.Delivered() {
		t.Fatalf("after rotation+user: Owing=%v Delivered=%v, want true/false", tl.Owing(), tl.Delivered())
	}

	// Explicit empty-after-reset: truncate to empty.
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	tl.Poll()
	if tl.Owing() || tl.Delivered() {
		t.Fatalf("after rotation to empty: Owing=%v Delivered=%v, want false/false", tl.Owing(), tl.Delivered())
	}
}

func TestClaudeEndTurnBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "end-turn.jsonl")
	tl := &Tailer{Path: path}
	appendLine := func(line string) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(line + "\n"); err != nil {
			f.Close()
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		tl.Poll()
	}

	appendLine(`{"type":"user","message":{"role":"user","content":"hi"}}`)
	if tl.EndTurn() {
		t.Fatal("user turn must not be an end-turn boundary")
	}
	appendLine(`{"type":"assistant","message":{"role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","id":"t1","name":"Bash"}]}}`)
	if tl.EndTurn() {
		t.Fatal("tool_use stop must not release the composer")
	}
	appendLine(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1"}]}}`)
	appendLine(`{"type":"assistant","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"done"}]}}`)
	if !tl.EndTurn() {
		t.Fatal("end_turn boundary was not retained")
	}
	appendLine(`{"type":"user","message":{"role":"user","content":"next"}}`)
	if tl.EndTurn() {
		t.Fatal("a new user turn must withdraw the prior end-turn boundary")
	}
}

func TestClaudeInterruptIsCompletedBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "interrupt.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tl := &Tailer{Path: path}

	lines := []string{
		`{"type":"user","timestamp":"2026-08-18T14:05:01Z","message":{"role":"user","content":"do it"}}`,
		`{"type":"assistant","timestamp":"2026-08-18T14:05:02Z","message":{"role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{}}]}}`,
		`{"type":"user","timestamp":"2026-08-18T14:28:30Z","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user for tool use]"}]},"interruptedMessageId":"msg_1"}`,
	}
	for _, line := range lines {
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	tl.Poll()
	if tl.Owing() {
		t.Fatal("an explicit Claude interrupt must not leave the agent owing output")
	}
	if tl.PendingCount() != 0 {
		t.Fatalf("pending calls after interrupt = %d, want 0", tl.PendingCount())
	}
	if !tl.EndTurn() {
		t.Fatal("an explicit Claude interrupt must release the next-prompt gate")
	}
}

func TestClaudeInterruptTextFallbackIsExact(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    bool
	}{
		{name: "legacy", content: `"[Request interrupted by user]"`, want: true},
		{name: "tool use block", content: `[{"type":"text","text":"[Request interrupted by user for tool use]"}]`, want: true},
		{name: "ordinary prompt mentioning marker", content: `"[Request interrupted by user] is what Claude printed"`, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeInterruptedTurn("", json.RawMessage(tc.content)); got != tc.want {
				t.Fatalf("claudeInterruptedTurn(%s) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}

// NewestTurnTime dates the newest parsed turn from the CLI's own stamp, in
// memory, with no extra file read — the poller's freshness gate for the map's
// "Ready" prominence (P5 review). Zero means "cannot be dated", never "old".
func TestNewestTurnTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "when.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tl := &Tailer{Path: path}
	if ts, ok := tl.NewestTurnTime(); ok || !ts.IsZero() {
		t.Fatalf("empty tailer: got %v/%v, want zero/false", ts, ok)
	}

	want, err := time.Parse(time.RFC3339Nano, "2026-08-10T09:15:00.500Z")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"type":"assistant","timestamp":"2026-08-10T09:15:00.500Z","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}` + "\n")
	tl.Poll()
	ts, ok := tl.NewestTurnTime()
	if !ok || !ts.Equal(want) {
		t.Fatalf("after assistant turn: got %v/%v, want %v/true", ts, ok, want)
	}

	// An undated turn must not pretend to be "now" and must not erase the
	// last real stamp: the freshness gate reads absence as unknown.
	f.WriteString(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"no stamp"}]}}` + "\n")
	tl.Poll()
	if ts, ok := tl.NewestTurnTime(); !ok || !ts.Equal(want) {
		t.Fatalf("after undated turn: got %v/%v, want the previous stamp", ts, ok)
	}

	// Claude Code flushes some records late, so a trailing turn can carry an
	// *older* stamp than one already parsed (the P1 root cause). Newest wins:
	// a late flush must not age a fresh delivery out of the freshness window.
	f.WriteString(`{"type":"user","timestamp":"2026-08-10T09:14:00Z","message":{"role":"user","content":"flushed late"}}` + "\n")
	tl.Poll()
	if ts, ok := tl.NewestTurnTime(); !ok || !ts.Equal(want) {
		t.Fatalf("after late-flushed older turn: got %v/%v, want the newer stamp %v", ts, ok, want)
	}

	// Rotation resets it with the rest of the tailer state, so a /clear-ed log
	// cannot date a fresh chat by its pre-clear turn.
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	tl.Poll()
	if ts, ok := tl.NewestTurnTime(); ok || !ts.IsZero() {
		t.Fatalf("after rotation: got %v/%v, want zero/false", ts, ok)
	}
}

// Owing is the transcript half of the quiet-pane stall backstop (P1b): the
// agent owes the next output when the newest recognized record is a human
// prompt or a tool result. Unknown record types leave it unchanged.
func TestOwing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owe.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tl := &Tailer{Path: path}

	if tl.Owing() {
		t.Fatal("empty file: Owing = true, want false")
	}

	// Human prompt: agent owes a response.
	f.WriteString(`{"type":"user","timestamp":"t1","message":{"role":"user","content":"please edit hello.txt"}}` + "\n")
	tl.Poll()
	if !tl.Owing() {
		t.Fatal("after user prompt: Owing = false, want true")
	}

	// Assistant turn: agent has produced output; debt cleared.
	f.WriteString(`{"type":"assistant","timestamp":"t2","message":{"role":"assistant","content":[{"type":"text","text":"I will edit it"}]}}` + "\n")
	tl.Poll()
	if tl.Owing() {
		t.Fatal("after assistant turn: Owing = true, want false")
	}

	// Tool result (type:user with tool_result blocks): agent continues; owes again.
	f.WriteString(`{"type":"assistant","timestamp":"t3","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{}}]}}` + "\n")
	f.WriteString(`{"type":"user","timestamp":"t4","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"ok"}]}}` + "\n")
	tl.Poll()
	if !tl.Owing() {
		t.Fatal("after tool result: Owing = false, want true")
	}

	// Unknown typed record: leave owing unchanged (still true).
	f.WriteString(`{"type":"bridge-session","timestamp":"t5","payload":{}}` + "\n")
	tl.Poll()
	if !tl.Owing() {
		t.Fatal("unknown record type must leave Owing unchanged (true)")
	}

	// After an assistant record, owing is false; unknown still leaves it false.
	f.WriteString(`{"type":"assistant","timestamp":"t6","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}` + "\n")
	tl.Poll()
	if tl.Owing() {
		t.Fatal("after second assistant: Owing = true, want false")
	}
	f.WriteString(`{"type":"file-history-delta","timestamp":"t7"}` + "\n")
	tl.Poll()
	if tl.Owing() {
		t.Fatal("unknown record type must leave Owing unchanged (false)")
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

// --- V2-P2: tool stamps for mirror widen ---

func TestTailer_ToolStampsClaude(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"type":"assistant","timestamp":"2026-07-11T09:00:05.000Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}}` + "\n")
	f.WriteString(`{"type":"user","timestamp":"2026-07-11T09:00:06.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}` + "\n")
	f.Close()

	tl := &Tailer{Path: path}
	tl.Poll()
	stamps := tl.ToolStamps()
	if len(stamps) != 2 {
		t.Fatalf("ToolStamps = %d, want 2: %+v", len(stamps), stamps)
	}
	if stamps[0].ID != "t1" || stamps[0].Status != "pending" || stamps[0].Title != "Bash" {
		t.Errorf("call = %+v", stamps[0])
	}
	if stamps[0].Time != "2026-07-11T09:00:05.000Z" {
		t.Errorf("call time = %q", stamps[0].Time)
	}
	if stamps[1].ID != "t1" || stamps[1].Status != "completed" {
		t.Errorf("result = %+v", stamps[1])
	}
}

func TestTailer_ToolStampsCodex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"timestamp":"t1","type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"c1","arguments":"{}"}}` + "\n")
	f.WriteString(`{"timestamp":"t2","type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":"done"}}` + "\n")
	f.Close()

	tl := &Tailer{Path: path}
	tl.Poll()
	stamps := tl.ToolStamps()
	if len(stamps) != 2 {
		t.Fatalf("ToolStamps = %d, want 2: %+v", len(stamps), stamps)
	}
	if stamps[0].ID != "c1" || stamps[0].Status != "pending" || stamps[0].Title != "exec_command" {
		t.Errorf("call = %+v", stamps[0])
	}
	if stamps[1].ID != "c1" || stamps[1].Status != "completed" {
		t.Errorf("result = %+v", stamps[1])
	}
}

// P2b — NewestContentTime ignores metadata-only records and returns false
// when the file has no turns the parser would surface.
func TestNewestContentTime(t *testing.T) {
	dir := t.TempDir()

	t.Run("ignores_metadata_only_and_unknown", func(t *testing.T) {
		path := filepath.Join(dir, "with-bridge.jsonl")
		// Content at 07:28; trailing bridge-session at 08:04 must not count.
		body := `{"type":"user","timestamp":"2026-08-10T07:28:38.000Z","message":{"role":"user","content":"last real turn"}}` + "\n" +
			`{"type":"bridge-session","timestamp":"2026-08-10T08:04:55.000Z"}` + "\n" +
			`{"type":"summary","summary":"meta","leafUuid":"x"}` + "\n"
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		got, ok := NewestContentTime(path)
		if !ok {
			t.Fatal("NewestContentTime returned false, want the user turn's time")
		}
		want, err := time.Parse(time.RFC3339Nano, "2026-08-10T07:28:38.000Z")
		if err != nil {
			t.Fatal(err)
		}
		if !got.Equal(want) {
			t.Fatalf("NewestContentTime = %v, want %v (bridge-session must not count)", got, want)
		}
	})

	t.Run("false_when_no_turns", func(t *testing.T) {
		path := filepath.Join(dir, "empty-meta.jsonl")
		body := `{"type":"bridge-session","timestamp":"2026-08-10T08:04:55.000Z"}` + "\n" +
			`{"type":"summary","summary":"only meta"}` + "\n" +
			`{}` + "\n"
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, ok := NewestContentTime(path); ok {
			t.Fatal("metadata-only file must return ok=false")
		}
	})

	t.Run("missing_file", func(t *testing.T) {
		if _, ok := NewestContentTime(filepath.Join(dir, "nope.jsonl")); ok {
			t.Fatal("missing file must return ok=false")
		}
	})
}
