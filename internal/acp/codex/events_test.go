package codex

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
)

func TestDecodeAgentDelta(t *testing.T) {
	if _, got := decodeAgentDelta([]byte(`{"delta":"pon"}`)); got != "pon" {
		t.Fatalf("delta = %q", got)
	}
	if _, got := decodeAgentDelta([]byte(`garbage`)); got != "" {
		t.Fatalf("garbage delta should be empty, got %q", got)
	}
}

func TestDecodeStatus(t *testing.T) {
	// Synthetic status envelope: status is an object, not a string.
	if got := decodeStatus([]byte(`{"threadId":"t","status":{"type":"active","activeFlags":[]}}`)); got != "active" {
		t.Fatalf("status = %q", got)
	}
	if got := decodeStatus([]byte(`{"status":{"type":"idle"}}`)); got != "idle" {
		t.Fatalf("status = %q", got)
	}
}

func TestDecodeTokenUsage(t *testing.T) {
	// Synthetic last-turn counters deliberately differ from cumulative counters.
	raw := []byte(`{"threadId":"t","turnId":"u","tokenUsage":{"total":{"totalTokens":360,"inputTokens":300,"cachedInputTokens":120,"outputTokens":60,"reasoningOutputTokens":0},"last":{"totalTokens":120,"inputTokens":100,"cachedInputTokens":40,"outputTokens":20,"reasoningOutputTokens":0},"modelContextWindow":1000}}`)
	u := decodeTokenUsage(raw, "")
	if u == nil {
		t.Fatal("nil usage")
	}
	if u.Used != 120 || u.Size != 1000 {
		t.Fatalf("Used/Size = %d/%d", u.Used, u.Size)
	}
	if u.InputTokens != 100 || u.OutputTokens != 20 || u.CachedReadTokens != 40 || u.TotalTokens != 120 {
		t.Fatalf("token breakdown wrong: %+v", u)
	}
	if decodeTokenUsage([]byte(`not json`), "") != nil {
		t.Fatal("garbage usage should be nil")
	}
}

// Phase 6 — Model + TurnID on codex UsageEvents (O4, D3, D6).
// Live wire shape carries turnId on tokenUsage/updated params; no per-turn
// model → sessionModel (meta.model) fallback.
func TestCodexEvents_ModelAndTurnID(t *testing.T) {
	// Synthetic identity envelope: turnId is at params top-level.
	raw := []byte(`{"threadId":"t","turnId":"u","tokenUsage":{"total":{"totalTokens":360,"inputTokens":300,"cachedInputTokens":120,"outputTokens":60,"reasoningOutputTokens":0},"last":{"totalTokens":120,"inputTokens":100,"cachedInputTokens":40,"outputTokens":20,"reasoningOutputTokens":0},"modelContextWindow":1000}}`)
	u := decodeTokenUsage(raw, "gpt-5.5")
	if u == nil {
		t.Fatal("nil usage")
	}
	if u.TurnID != "u" {
		t.Fatalf("TurnID = %q, want %q (params.turnId)", u.TurnID, "u")
	}
	// O4: no per-turn model on tokenUsage → session meta.model fallback.
	if u.Model != "gpt-5.5" {
		t.Fatalf("Model = %q, want %q (session meta.model fallback, O4)", u.Model, "gpt-5.5")
	}
	// Occupancy / breakdown unchanged (additive identity fields only).
	if u.Used != 120 || u.InputTokens != 100 || u.CachedReadTokens != 40 {
		t.Fatalf("breakdown/occupancy changed: %+v", u)
	}

	// Absent turnId → empty TurnID (seam fallback, D3); never invent one.
	noID := []byte(`{"tokenUsage":{"last":{"totalTokens":77,"inputTokens":60,"cachedInputTokens":0,"outputTokens":17},"modelContextWindow":128000}}`)
	u = decodeTokenUsage(noID, "codex-o4-mini")
	if u == nil {
		t.Fatal("nil usage for no-turnId shape")
	}
	if u.TurnID != "" {
		t.Fatalf("TurnID = %q, want empty when params.turnId absent", u.TurnID)
	}
	if u.Model != "codex-o4-mini" {
		t.Fatalf("Model = %q, want session fallback", u.Model)
	}

	// Empty session model → Model empty (degrade, never invent).
	u = decodeTokenUsage(raw, "")
	if u == nil || u.Model != "" {
		t.Fatalf("empty sessionModel: Model = %q, want empty", u.Model)
	}
}

// TestCodex_SubtractNormalizationEndToEnd: codex fixture with
// {inputTokens, cachedReadTokens} → after ReadFare, FreshIn == input − cache
// (clamped ≥0), proving §2.3 subtract flows through fare.Normalize for codex.
func TestCodex_SubtractNormalizationEndToEnd(t *testing.T) {
	// Same synthetic counters as TestDecodeTokenUsage: input=100, cached=40.
	raw := []byte(`{"threadId":"t","turnId":"turn-e2e","tokenUsage":{"last":{"totalTokens":120,"inputTokens":100,"cachedInputTokens":40,"outputTokens":20},"modelContextWindow":1000}}`)
	u := decodeTokenUsage(raw, "gpt-5.5")
	if u == nil {
		t.Fatal("nil usage")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "codex-fare.jsonl")
	w := &sessionlog.Writer{Path: path}
	if err := w.Append(sessionlog.NewMeta("c1", "codex", "gpt-5.5", "high", dir)); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(sessionlog.Event{T: "usage", Usage: u}); err != nil {
		t.Fatal(err)
	}

	f := sessionlog.ReadFare(path)
	wantFresh := 100 - 40 // 60
	if f.FreshIn != wantFresh {
		t.Errorf("FreshIn = %d, want %d (inputTokens − cachedReadTokens via fare.Normalize)", f.FreshIn, wantFresh)
	}
	if f.CacheRead != 40 {
		t.Errorf("CacheRead = %d, want 40", f.CacheRead)
	}
	if f.Out != 20 {
		t.Errorf("Out = %d, want 20", f.Out)
	}
	if f.Turns != 1 {
		t.Errorf("Turns = %d, want 1", f.Turns)
	}
	// D6: PerModel keyed by session-model fallback.
	pm, ok := f.PerModel["gpt-5.5"]
	if !ok {
		t.Fatal(`PerModel["gpt-5.5"] missing`)
	}
	if pm.FreshIn != wantFresh {
		t.Errorf("PerModel FreshIn = %d, want %d", pm.FreshIn, wantFresh)
	}
}

func TestDecodeItemUserMessage(t *testing.T) {
	raw := []byte(`{"item":{"type":"userMessage","id":"i","content":[{"type":"text","text":"hello"}]},"turnId":"u"}`)
	ev := decodeItem(raw, true)
	if ev == nil || ev.T != "user" || ev.Text != "hello" {
		t.Fatalf("user item decode: %+v", ev)
	}
}

func TestDecodeItemAgentMessage(t *testing.T) {
	// agentMessage carries final text directly.
	raw := []byte(`{"item":{"type":"agentMessage","id":"i","text":"pong"}}`)
	ev := decodeItem(raw, true)
	if ev == nil || ev.T != "assistant" || ev.Text != "pong" {
		t.Fatalf("agent item decode: %+v", ev)
	}
}

func TestDecodeItemReasoningIgnored(t *testing.T) {
	if ev := decodeItem([]byte(`{"item":{"type":"reasoning","id":"i"}}`), true); ev != nil {
		t.Fatalf("reasoning should be ignored, got %+v", ev)
	}
}

func TestDecodeItemToolFallback(t *testing.T) {
	raw := []byte(`{"item":{"type":"commandExecution","id":"call_x","command":"ls -la","status":"in_progress"}}`)
	ev := decodeItem(raw, false)
	if ev == nil || ev.T != "tool" || ev.Tool == nil {
		t.Fatalf("tool decode: %+v", ev)
	}
	if ev.Tool.Title != "ls -la" || ev.Tool.Kind != "commandExecution" || ev.Tool.Status != "in_progress" {
		t.Fatalf("tool fields: %+v", ev.Tool)
	}
	if ev.Tool.RawInput != "ls -la" {
		t.Fatalf("rawInput = %v", ev.Tool.RawInput)
	}
}

func TestDecodeItemToolDefaultStatus(t *testing.T) {
	// No status + completed=true → "completed"; completed=false → "started".
	done := decodeItem([]byte(`{"item":{"type":"x","id":"i"}}`), true)
	if done.Tool.Status != "completed" {
		t.Fatalf("completed status = %q", done.Tool.Status)
	}
	start := decodeItem([]byte(`{"item":{"type":"x","id":"i"}}`), false)
	if start.Tool.Status != "started" {
		t.Fatalf("started status = %q", start.Tool.Status)
	}
}

func TestDecodeItemEmptyTextSkipped(t *testing.T) {
	if ev := decodeItem([]byte(`{"item":{"type":"agentMessage","text":"   "}}`), true); ev != nil {
		t.Fatalf("blank assistant text should be skipped, got %+v", ev)
	}
	if ev := decodeItem([]byte(`garbage`), true); ev != nil {
		t.Fatalf("garbage item should be nil")
	}
}

func assertItemProvKey(t *testing.T, ev *Event, key, inner string) {
	t.Helper()
	if ev == nil || len(ev.Prov) == 0 {
		t.Fatal("want retained provenance")
	}
	assertCodexEntry(t, ev.Prov, "item", key, inner)
}

func TestCopyRawEmptyIsNil(t *testing.T) {
	if copyRaw(nil) != nil || copyRaw(json.RawMessage{}) != nil {
		t.Fatal("empty copyRaw must return nil")
	}
}

func TestDecodeItemAgentMessageRetainsProvenance(t *testing.T) {
	inner := `{"src":"synth-codex-item","n":1}`
	raw := []byte(`{"item":{"type":"agentMessage","id":"i","text":"pong","provenance":{"src":"synth-codex-item","n":1}}}`)
	ev := decodeItem(raw, true)
	if ev == nil || ev.T != "assistant" || ev.Text != "pong" {
		t.Fatalf("agent item decode: %+v", ev)
	}
	assertItemProvKey(t, ev, "provenance", inner)
}

func TestDecodeItemAgentMessageRetainsMeta(t *testing.T) {
	inner := `{"src":"codex-meta"}`
	raw := []byte(`{"item":{"type":"agentMessage","text":"hi","_meta":{"src":"codex-meta"}}}`)
	ev := decodeItem(raw, true)
	if ev == nil || ev.Text != "hi" {
		t.Fatalf("decode: %+v", ev)
	}
	assertItemProvKey(t, ev, "_meta", inner)
}

func TestDecodeItemAgentMessageContentBlockWithProvenance(t *testing.T) {
	inner := `{"src":"content-block"}`
	raw := []byte(`{"item":{"type":"agentMessage","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}],"provenance":{"src":"content-block"}}}`)
	ev := decodeItem(raw, true)
	if ev == nil || ev.Text != "ab" {
		t.Fatalf("content join = %+v", ev)
	}
	assertItemProvKey(t, ev, "provenance", inner)
}

func TestDecodeItemWrongShapedProvenanceIgnored(t *testing.T) {
	raw := []byte(`{"item":{"type":"agentMessage","text":"kept","provenance":"nope"}}`)
	ev := decodeItem(raw, true)
	if ev == nil || ev.T != "assistant" || ev.Text != "kept" {
		t.Fatalf("message dropped: %+v", ev)
	}
	assertCodexEntry(t, ev.Prov, "item", "provenance", `"nope"`)
}

func TestDecodeItemAbsentProvenance(t *testing.T) {
	ev := decodeItem([]byte(`{"item":{"type":"agentMessage","text":"pong"}}`), true)
	if ev == nil || ev.Text != "pong" || len(ev.Prov) != 0 {
		t.Fatalf("absent provenance: %+v", ev)
	}
}

func TestDecodeItemUserAndReasoningUnchangedWithProvenance(t *testing.T) {
	u := decodeItem([]byte(`{"item":{"type":"userMessage","text":"hello","provenance":{"src":"user"}}}`), true)
	if u == nil || u.T != "user" || u.Text != "hello" {
		t.Fatalf("user item: %+v", u)
	}
	if ev := decodeItem([]byte(`{"item":{"type":"reasoning","id":"i","provenance":{"src":"x"}}}`), true); ev != nil {
		t.Fatalf("reasoning should stay ignored, got %+v", ev)
	}
	tool := decodeItem([]byte(`{"item":{"type":"commandExecution","id":"call_x","command":"ls","provenance":{"src":"x"}}}`), true)
	if tool == nil || tool.T != "tool" || tool.Tool == nil || tool.Tool.Title != "ls" {
		t.Fatalf("unknown/tool item behavior changed: %+v", tool)
	}
}

type codexProv struct {
	Loc string          `json:"loc"`
	Key string          `json:"key"`
	V   json.RawMessage `json:"v"`
}

func mustCodexProv(t *testing.T, raw json.RawMessage) []codexProv {
	t.Helper()
	if len(raw) == 0 {
		t.Fatal("Prov is empty")
	}
	var entries []codexProv
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("Prov is not an entry list: %v (%s)", err, raw)
	}
	return entries
}

func assertCodexEntry(t *testing.T, raw json.RawMessage, loc, key, wantJSON string) {
	t.Helper()
	entries := mustCodexProv(t, raw)
	for _, e := range entries {
		if e.Loc != loc || e.Key != key {
			continue
		}
		var got, want any
		if err := json.Unmarshal(e.V, &got); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
			t.Fatal(err)
		}
		gb, _ := json.Marshal(got)
		wb, _ := json.Marshal(want)
		if string(gb) != string(wb) {
			t.Fatalf("%s/%s = %s, want %s", loc, key, e.V, wantJSON)
		}
		return
	}
	t.Fatalf("missing loc=%s key=%s in %s", loc, key, raw)
}

func TestDecodeItemBothMetaAndProvenance(t *testing.T) {
	raw := []byte(`{"item":{"type":"agentMessage","text":"pong","_meta":{"from":"meta"},"provenance":{"from":"prov"}}}`)
	ev := decodeItem(raw, true)
	if ev == nil || ev.Text != "pong" {
		t.Fatalf("%+v", ev)
	}
	assertCodexEntry(t, ev.Prov, "item", "_meta", `{"from":"meta"}`)
	assertCodexEntry(t, ev.Prov, "item", "provenance", `{"from":"prov"}`)
	if len(mustCodexProv(t, ev.Prov)) != 2 {
		t.Fatalf("want 2 contributions: %s", ev.Prov)
	}
}

func TestDecodeItemNotificationAndItemLocations(t *testing.T) {
	raw := []byte(`{"_meta":{"from":"notif"},"provenance":{"from":"nprov"},"item":{"type":"agentMessage","text":"pong","_meta":{"from":"item"}}}`)
	ev := decodeItem(raw, true)
	if ev == nil {
		t.Fatal("nil")
	}
	assertCodexEntry(t, ev.Prov, "notification", "_meta", `{"from":"notif"}`)
	assertCodexEntry(t, ev.Prov, "notification", "provenance", `{"from":"nprov"}`)
	assertCodexEntry(t, ev.Prov, "item", "_meta", `{"from":"item"}`)
}

func TestDecodeItemAllJSONValueTypes(t *testing.T) {
	cases := []string{`{"src":"o"}`, `[1]`, `"s"`, `4`, `false`, `null`}
	for _, v := range cases {
		raw := []byte(`{"item":{"type":"agentMessage","text":"pong","_meta":` + v + `}}`)
		ev := decodeItem(raw, true)
		if ev == nil || ev.Text != "pong" {
			t.Errorf("value %s dropped message: %+v", v, ev)
			continue
		}
		assertCodexEntry(t, ev.Prov, "item", "_meta", v)
	}
}

func TestDecodeItemExactKeysOnly(t *testing.T) {
	raw := []byte(`{"item":{"type":"agentMessage","text":"pong","_Meta":{"no":1},"Provenance":{"no":2}}}`)
	ev := decodeItem(raw, true)
	if ev == nil || ev.Text != "pong" {
		t.Fatalf("%+v", ev)
	}
	if len(ev.Prov) != 0 {
		t.Fatalf("case variants retained: %s", ev.Prov)
	}
}

func TestDecodeItemDoesNotAliasParams(t *testing.T) {
	raw := []byte(`{"item":{"type":"agentMessage","text":"pong","provenance":{"src":"buf"}}}`)
	ev := decodeItem(raw, true)
	if ev == nil || len(ev.Prov) == 0 {
		t.Fatal("want prov")
	}
	saved := append(json.RawMessage(nil), ev.Prov...)
	for i := range raw {
		raw[i] = ' '
	}
	if !bytes.Equal(ev.Prov, saved) {
		t.Fatalf("aliased caller buffer: %s", ev.Prov)
	}
}

func TestDecodeItemContentBlockBothKeys(t *testing.T) {
	raw := []byte(`{"item":{"type":"agentMessage","content":[{"type":"text","text":"ab"}],"_meta":{"m":1},"provenance":{"p":2}}}`)
	ev := decodeItem(raw, true)
	if ev == nil || ev.Text != "ab" {
		t.Fatalf("%+v", ev)
	}
	assertCodexEntry(t, ev.Prov, "item", "_meta", `{"m":1}`)
	assertCodexEntry(t, ev.Prov, "item", "provenance", `{"p":2}`)
}
