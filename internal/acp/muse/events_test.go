package muse

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestPinnedFingerprintConstant(t *testing.T) {
	if PinnedFingerprint != "sha256:ab69549a7ebb423fce94068762da0b5ff3cdec1f8fc263dcc17248eda117f852" {
		t.Fatalf("PinnedFingerprint=%q", PinnedFingerprint)
	}
	if ObservedMuseVersion != "1.3.0-R3057.1" {
		t.Fatalf("ObservedMuseVersion=%q", ObservedMuseVersion)
	}
	if ObservedMSPSchema != 1 {
		t.Fatalf("ObservedMSPSchema=%v", ObservedMSPSchema)
	}
}

func TestExactlyOnceChatFromCompleted(t *testing.T) {
	f := newFold()
	var all []*Event
	collect := func(method, raw string) {
		all = append(all, f.apply(method, rawJSON(t, raw))...)
	}
	collect("item/started", `{"item":{"itemId":"a1","kind":"agentMessage","revision":1,"status":"inProgress","text":"early"},"_meta":{"s":1}}`)
	collect("item/updated", `{"item":{"itemId":"a1","kind":"agentMessage","revision":2,"status":"completed","text":"mid"},"_meta":{"s":2}}`)
	collect("item/completed", `{"item":{"itemId":"a1","kind":"agentMessage","revision":3,"status":"completed","text":"final","provenance":{"p":true}}}`)
	var asst []Event
	for _, e := range all {
		if e != nil && e.T == "assistant" {
			asst = append(asst, *e)
		}
	}
	if len(asst) != 1 {
		t.Fatalf("want exactly 1 assistant event, got %d: %+v", len(asst), asst)
	}
	if asst[0].Text != "final" {
		t.Fatalf("authoritative text=%q", asst[0].Text)
	}
	assertProvHas(t, asst[0].Prov, "notification", "_meta")
	assertProvHas(t, asst[0].Prov, "item", "provenance")
	if f.streaming() != "" {
		t.Fatal("completion must clear stream")
	}

	f = newFold()
	all = nil
	collect("item/started", `{"item":{"itemId":"u1","kind":"userMessage","revision":1,"status":"inProgress","text":"draft"}}`)
	collect("item/updated", `{"item":{"itemId":"u1","kind":"userMessage","revision":2,"status":"completed","text":"still-draft"}}`)
	collect("item/completed", `{"item":{"itemId":"u1","kind":"userMessage","revision":3,"status":"completed","text":"hello"}}`)
	var users []Event
	for _, e := range all {
		if e != nil && e.T == "user" {
			users = append(users, *e)
		}
	}
	if len(users) != 1 || users[0].Text != "hello" {
		t.Fatalf("want one user event, got %+v", users)
	}
}

func TestStartedUpdatedDoNotEmitChatEvenWithText(t *testing.T) {
	f := newFold()
	if evs := f.apply("item/started", rawJSON(t, `{"item":{"itemId":"a1","kind":"agentMessage","revision":1,"text":"nope"}}`)); chatCount(evs) != 0 {
		t.Fatalf("started emitted chat: %+v", evs)
	}
	if evs := f.apply("item/updated", rawJSON(t, `{"item":{"itemId":"a1","kind":"agentMessage","revision":2,"status":"completed","text":"nope"}}`)); chatCount(evs) != 0 {
		t.Fatalf("updated emitted chat: %+v", evs)
	}
	if evs := f.apply("item/started", rawJSON(t, `{"item":{"itemId":"r1","kind":"reasoning","revision":1,"text":"secret"}}`)); chatCount(evs) != 0 {
		t.Fatalf("reasoning started: %+v", evs)
	}
}

func TestToolRowsEmitOnStartedUpdatedCompleted(t *testing.T) {
	f := newFold()
	var tools []string
	for _, step := range []struct{ method, status string }{
		{"item/started", "inProgress"},
		{"item/updated", "inProgress"},
		{"item/completed", "completed"},
	} {
		evs := f.apply(step.method, rawJSON(t, `{"item":{"itemId":"t1","kind":"toolCall","revision":`+revFor(step.method)+`,"status":"`+step.status+`","tool":"ls"}}`))
		if len(evs) != 1 || evs[0].Tool == nil {
			t.Fatalf("%s: want tool row, got %+v", step.method, evs)
		}
		if evs[0].Tool.Status != step.status {
			t.Fatalf("%s status=%q", step.method, evs[0].Tool.Status)
		}
		tools = append(tools, evs[0].Tool.Status)
	}
	if len(tools) != 3 {
		t.Fatalf("tool rows=%v", tools)
	}
}

func chatCount(evs []*Event) int {
	n := 0
	for _, e := range evs {
		if e != nil && (e.T == "user" || e.T == "assistant") {
			n++
		}
	}
	return n
}

func revFor(method string) string {
	switch method {
	case "item/started":
		return "1"
	case "item/updated":
		return "2"
	default:
		return "3"
	}
}

func TestFoldRejectsEmptyItemIDAndLowRevision(t *testing.T) {
	f := newFold()
	if evs := f.apply("item/started", rawJSON(t, `{"item":{"itemId":"","kind":"agentMessage","revision":1}}`)); len(evs) != 0 {
		t.Fatalf("empty id produced %v", evs)
	}
	if evs := f.apply("item/started", rawJSON(t, `{"item":{"itemId":"i1","kind":"agentMessage","revision":0}}`)); len(evs) != 0 {
		t.Fatalf("revision 0 produced %v", evs)
	}
	if evs := f.apply("item/started", rawJSON(t, `{"item":{"itemId":"i1","kind":"agentMessage","revision":-3}}`)); len(evs) != 0 {
		t.Fatalf("negative revision produced %v", evs)
	}
}

func TestFoldReplaceOnlyOnHigherRevision(t *testing.T) {
	f := newFold()
	f.apply("item/started", rawJSON(t, `{"item":{"itemId":"t1","kind":"toolCall","revision":1,"status":"inProgress","tool":"v1"}}`))
	evs := f.apply("item/updated", rawJSON(t, `{"item":{"itemId":"t1","kind":"toolCall","revision":1,"status":"inProgress","tool":"same"}}`))
	if toolTitle(evs) == "same" {
		t.Fatal("equal revision overwrote stored item")
	}
	evs = f.apply("item/updated", rawJSON(t, `{"item":{"itemId":"t1","kind":"toolCall","revision":0,"status":"inProgress","tool":"low"}}`))
	if toolTitle(evs) == "low" {
		t.Fatal("lower revision overwrote stored item")
	}
	evs = f.apply("item/updated", rawJSON(t, `{"item":{"itemId":"t1","kind":"toolCall","revision":2,"status":"inProgress","tool":"v2"}}`))
	if toolTitle(evs) != "v2" {
		t.Fatalf("higher revision title=%q", toolTitle(evs))
	}
}

func TestFoldCompletedWithoutStarted(t *testing.T) {
	f := newFold()
	evs := f.apply("item/completed", rawJSON(t, `{"item":{"itemId":"a1","kind":"agentMessage","revision":1,"status":"completed","text":"done"}}`))
	if len(evs) != 1 || evs[0].T != "assistant" || evs[0].Text != "done" {
		t.Fatalf("completed-without-started: %+v", evs)
	}
}

func TestDeltaDoesNotChangeRevision(t *testing.T) {
	f := newFold()
	f.apply("item/started", rawJSON(t, `{"item":{"itemId":"a1","kind":"agentMessage","revision":1,"status":"inProgress","text":""}}`))
	f.apply("item/delta", rawJSON(t, `{"itemId":"a1","delta":"Hi"}`))
	// A later revision-1 update must still be stale.
	evs := f.apply("item/updated", rawJSON(t, `{"item":{"itemId":"a1","kind":"agentMessage","revision":1,"status":"inProgress","text":"stale"}}`))
	for _, e := range evs {
		if e.T == "assistant" && e.Text == "stale" {
			t.Fatal("delta synthesized a revision so an equal update was applied")
		}
	}
}

func TestItemKindRendering(t *testing.T) {
	cases := []struct {
		name   string
		kind   string
		text   string
		status string
		wantT  string
		want   string
	}{
		{"user", "userMessage", "hello", "completed", "user", "hello"},
		{"user blank", "userMessage", "  ", "completed", "", ""},
		{"user retracted", "userMessage", "nope", "completed", "", ""},
		{"assistant", "agentMessage", "hi", "completed", "assistant", "hi"},
		{"assistant blank", "agentMessage", "", "completed", "", ""},
		{"reasoning", "reasoning", "secret", "completed", "", ""},
		{"tool", "toolCall", "", "completed", "tool", "toolCall"},
		{"shell", "userShell", "", "completed", "tool", "userShell"},
		{"subagent", "subagent", "", "completed", "tool", "subagent"},
		{"unknown kind", "reminderChild", "", "completed", "tool", "reminderChild"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFold()
			item := map[string]any{
				"itemId": "i-" + tc.name, "kind": tc.kind, "revision": 1,
				"status": tc.status, "text": tc.text,
			}
			if tc.name == "user retracted" {
				item["retracted"] = true
			}
			body, _ := json.Marshal(map[string]any{"item": item})
			evs := f.apply("item/completed", body)
			if tc.wantT == "" {
				for _, e := range evs {
					if e.T == "user" || e.T == "assistant" || e.T == "tool" {
						t.Fatalf("unexpected visible event %+v", e)
					}
				}
				return
			}
			if len(evs) == 0 {
				t.Fatal("expected an event")
			}
			e := evs[0]
			if e.T != tc.wantT {
				t.Fatalf("T=%q want %q", e.T, tc.wantT)
			}
			if tc.wantT == "tool" {
				if e.Tool == nil || e.Tool.Kind != tc.kind {
					t.Fatalf("tool=%+v", e.Tool)
				}
				if e.Tool.Title == "" {
					t.Fatal("unknown/tool kinds must keep an honest label")
				}
			} else if e.Text != tc.want {
				t.Fatalf("text=%q want %q", e.Text, tc.want)
			}
		})
	}
}

func TestUnknownStatusPreservedAndNonInProgressIsTerminal(t *testing.T) {
	f := newFold()
	evs := f.apply("item/completed", rawJSON(t, `{"item":{"itemId":"t1","kind":"toolCall","revision":1,"status":"weirdStatus"}}`))
	if len(evs) != 1 || evs[0].Tool == nil || evs[0].Tool.Status != "weirdStatus" {
		t.Fatalf("unknown status dropped: %+v", evs)
	}
	if !toolStatusTerminal("weirdStatus") || !toolStatusTerminal("completed") || !toolStatusTerminal("failed") {
		t.Fatal("any status other than inProgress must be terminal for tool rows")
	}
	if toolStatusTerminal("inProgress") {
		t.Fatal("inProgress must not be terminal")
	}
}

func TestMalformedToolArgumentsPreserved(t *testing.T) {
	f := newFold()
	evs := f.apply("item/completed", rawJSON(t, `{"item":{"itemId":"t1","kind":"toolCall","revision":1,"status":"completed","args":"not-json {"}}`))
	if len(evs) != 1 || evs[0].Tool == nil {
		t.Fatalf("evs=%v", evs)
	}
	s, ok := evs[0].Tool.RawInput.(string)
	if !ok || s != "not-json {" {
		t.Fatalf("raw args=%#v, want exact string", evs[0].Tool.RawInput)
	}
}

func TestValidToolArgumentsParsed(t *testing.T) {
	f := newFold()
	evs := f.apply("item/completed", rawJSON(t, `{"item":{"itemId":"t1","kind":"toolCall","revision":1,"status":"completed","args":"{\"cmd\":\"ls\"}"}}`))
	raw := evs[0].Tool.RawInput
	m, ok := raw.(map[string]any)
	if !ok {
		b, _ := json.Marshal(raw)
		var got map[string]any
		if json.Unmarshal(b, &got) != nil || got["cmd"] != "ls" {
			t.Fatalf("parsed args=%#v", raw)
		}
		return
	}
	if m["cmd"] != "ls" {
		t.Fatalf("cmd=%v", m["cmd"])
	}
}

func TestDottedDeltasDoNotPolluteAssistantText(t *testing.T) {
	f := newFold()
	f.apply("item/started", rawJSON(t, `{"item":{"itemId":"a1","kind":"agentMessage","revision":1,"status":"inProgress"}}`))
	f.apply("item/delta", rawJSON(t, `{"itemId":"a1","delta":"Hello"}`))
	f.apply("item/delta", rawJSON(t, `{"itemId":"a1","field":"text","delta":" world"}`))
	f.apply("item/delta", rawJSON(t, `{"itemId":"a1","field":"summary.0","delta":"NOPE"}`))
	f.apply("item/delta", rawJSON(t, `{"itemId":"a1","field":"output","delta":"NOPE2"}`))
	if got := f.streaming(); got != "Hello world" {
		t.Fatalf("streaming=%q", got)
	}
	evs := f.apply("item/completed", rawJSON(t, `{"item":{"itemId":"a1","kind":"agentMessage","revision":2,"status":"completed","text":"Hello world"}}`))
	if len(evs) != 1 || evs[0].Text != "Hello world" {
		t.Fatalf("completion=%+v", evs)
	}
	if strings.Contains(evs[0].Text, "NOPE") {
		t.Fatal("dotted delta leaked into assistant text")
	}
	if f.streaming() != "" {
		t.Fatalf("streaming not cleared on completion: %q", f.streaming())
	}
}

func TestDeltaPromotionKeepsCompletedProvenance(t *testing.T) {
	f := newFold()
	f.apply("item/started", rawJSON(t, `{"item":{"itemId":"a1","kind":"agentMessage","revision":1,"status":"inProgress"},"_meta":{"n":1}}`))
	f.apply("item/delta", rawJSON(t, `{"itemId":"a1","delta":"Hi","_meta":{"n":2}}`))
	evs := f.apply("item/completed", rawJSON(t, `{"item":{"itemId":"a1","kind":"agentMessage","revision":2,"status":"completed","text":"Hi","provenance":{"p":true}}}`))
	if len(evs) != 1 || evs[0].T != "assistant" {
		t.Fatalf("evs=%v", evs)
	}
	assertProvHas(t, evs[0].Prov, "notification", "_meta")
	assertProvHas(t, evs[0].Prov, "item", "provenance")
}

func TestProvenanceKeysAndValues(t *testing.T) {
	f := newFold()
	params := rawJSON(t, `{
		"_meta":{"k":1},
		"_Meta":{"ignored":true},
		"provenance":[1,2],
		"Provenance":"no",
		"item":{
			"itemId":"a1","kind":"agentMessage","revision":1,"status":"completed","text":"x",
			"provenance":{"ok":true},
			"_meta":null
		}
	}`)
	evs := f.apply("item/completed", params)
	entries := provEntries(t, evs[0].Prov)
	if len(entries) < 3 {
		t.Fatalf("entries=%s", evs[0].Prov)
	}
	assertProvHas(t, evs[0].Prov, "notification", "_meta")
	assertProvHas(t, evs[0].Prov, "notification", "provenance")
	assertProvHas(t, evs[0].Prov, "item", "provenance")
	assertProvHas(t, evs[0].Prov, "item", "_meta")
	for _, e := range entries {
		if e.Key != "_meta" && e.Key != "provenance" {
			t.Fatalf("lookalike key kept: %+v", e)
		}
	}
}

func TestProvenanceDedupAndOrder(t *testing.T) {
	f := newFold()
	evs := f.apply("item/completed", rawJSON(t, `{
		"_meta":{"k":1},
		"item":{"itemId":"a1","kind":"agentMessage","revision":1,"status":"completed","text":"x","_meta":{"k":1},"provenance":{"z":true}}
	}`))
	entries := provEntries(t, evs[0].Prov)
	if entries[0].Loc != "notification" || entries[0].Key != "_meta" {
		t.Fatalf("first-seen order wrong: %s", evs[0].Prov)
	}
	// equal repeated _meta may be deduplicated
	nMeta := 0
	for _, e := range entries {
		if e.Key == "_meta" && (bytes.Contains(e.V, []byte(`"k":1`)) || bytes.Contains(e.V, []byte(`"k": 1`))) {
			nMeta++
		}
	}
	if nMeta > 2 {
		t.Fatalf("duplicate contributions exploded: %s", evs[0].Prov)
	}
}

func TestProvenanceAnyJSONValue(t *testing.T) {
	values := []string{`{"a":1}`, `[1,2]`, `"s"`, `12`, `true`, `null`}
	for _, v := range values {
		f := newFold()
		evs := f.apply("item/completed", rawJSON(t, `{"_meta":`+v+`,"item":{"itemId":"a1","kind":"agentMessage","revision":1,"status":"completed","text":"x"}}`))
		if len(evs) != 1 || len(evs[0].Prov) == 0 {
			t.Fatalf("value %s dropped", v)
		}
		if bytes.Contains(evs[0].Prov, []byte(`"`+v)) && v != `"s"` {
			// must not stringify objects/arrays/numbers
		}
		entries := provEntries(t, evs[0].Prov)
		if string(bytes.TrimSpace(entries[0].V)) != v {
			t.Fatalf("v=%s got %s", v, entries[0].V)
		}
	}
}

func TestBlankMessageDoesNotInventChatEvent(t *testing.T) {
	f := newFold()
	evs := f.apply("item/completed", rawJSON(t, `{"_meta":{"p":1},"item":{"itemId":"a1","kind":"agentMessage","revision":1,"status":"completed","text":""}}`))
	for _, e := range evs {
		if e.T == "assistant" || e.T == "user" {
			t.Fatalf("blank+provenance invented chat: %+v", e)
		}
	}
}

func TestToolProvenanceSurvives(t *testing.T) {
	f := newFold()
	evs := f.apply("item/completed", rawJSON(t, `{"_meta":{"t":1},"item":{"itemId":"t1","kind":"mysteryKind","revision":1,"status":"completed","provenance":{"i":2}}}`))
	if len(evs) != 1 || evs[0].T != "tool" {
		t.Fatalf("unknown kind disappeared: %+v", evs)
	}
	assertProvHas(t, evs[0].Prov, "notification", "_meta")
	assertProvHas(t, evs[0].Prov, "item", "provenance")
}

func TestCallerMutationDoesNotAffectEvent(t *testing.T) {
	f := newFold()
	params := []byte(`{"_meta":{"k":"orig"},"item":{"itemId":"a1","kind":"agentMessage","revision":1,"status":"completed","text":"x"}}`)
	evs := f.apply("item/completed", params)
	params[bytes.Index(params, []byte("orig"))] = 'X'
	if bytes.Contains(evs[0].Prov, []byte("Xrig")) || !bytes.Contains(evs[0].Prov, []byte("orig")) {
		t.Fatalf("caller mutation leaked: %s", evs[0].Prov)
	}
	copy(evs[0].Prov, bytes.Repeat([]byte("z"), len(evs[0].Prov)))
	evs2 := f.apply("item/completed", rawJSON(t, `{"_meta":{"k":"orig"},"item":{"itemId":"a2","kind":"agentMessage","revision":1,"status":"completed","text":"y"}}`))
	if bytes.Contains(evs2[0].Prov, []byte("zzz")) {
		t.Fatal("mutating emitted event corrupted later state")
	}
}

func TestFoldClearsStreamingOnGapAndReset(t *testing.T) {
	f := newFold()
	f.apply("item/started", rawJSON(t, `{"item":{"itemId":"a1","kind":"agentMessage","revision":1,"status":"inProgress"}}`))
	f.apply("item/delta", rawJSON(t, `{"itemId":"a1","delta":"partial"}`))
	if f.streaming() == "" {
		t.Fatal("expected streaming text")
	}
	evs := f.apply("view/gap", rawJSON(t, `{"after":"c1","next":"c2"}`))
	if f.streaming() != "" {
		t.Fatal("gap must clear unfinished streaming")
	}
	if len(evs) != 1 || evs[0].T != "error" || !strings.Contains(evs[0].Error, "c1") || !strings.Contains(evs[0].Error, "c2") {
		t.Fatalf("gap event=%+v", evs)
	}
	f.apply("item/started", rawJSON(t, `{"item":{"itemId":"a2","kind":"agentMessage","revision":1,"status":"inProgress"}}`))
	f.apply("item/delta", rawJSON(t, `{"itemId":"a2","delta":"again"}`))
	f.reset()
	if f.streaming() != "" {
		t.Fatal("session reset must clear streaming")
	}
}

func TestOpaqueGapCursorNotParsed(t *testing.T) {
	f := newFold()
	evs := f.apply("view/gap", rawJSON(t, `{"after":"v:sess:1","next":"v:sess:9"}`))
	if strings.Contains(evs[0].Error, "parsed") {
		t.Fatal("must not parse v: cursors")
	}
	if !strings.Contains(evs[0].Error, "v:sess:1") || !strings.Contains(evs[0].Error, "v:sess:9") {
		t.Fatalf("cursors not retained: %q", evs[0].Error)
	}
}

func TestTokenUsageIsSpendNotOccupancy(t *testing.T) {
	u := decodeTokenUsage(rawJSON(t, `{
		"modelId":"spark","turnId":"t1",
		"promptTokens":10,"totalTokens":18,
		"usage":{"inputTokens":99,"outputTokens":5,"reasoningTokens":2,"cacheWriteTokens":1,"cachedTokens":3}
	}`))
	if u == nil {
		t.Fatal("nil usage")
	}
	if u.InputTokens != 10 {
		t.Fatalf("InputTokens=%d, want promptTokens 10 not usage.inputTokens", u.InputTokens)
	}
	if u.OutputTokens != 5 || u.ReasoningTokens != 2 || u.CacheCreationTokens != 1 || u.TotalTokens != 18 {
		t.Fatalf("spend fields: %+v", u)
	}
	if u.CachedReadTokens != 3 {
		t.Fatalf("cached fallback=%d", u.CachedReadTokens)
	}
	if u.Used != 0 || u.Size != 0 {
		t.Fatalf("spend must not populate occupancy: used=%d size=%d", u.Used, u.Size)
	}
	if u.Model != "spark" || u.TurnID != "t1" {
		t.Fatalf("identity %+v", u)
	}
}

func TestTokenUsageNullModelAndCachedReadSplit(t *testing.T) {
	u := decodeTokenUsage(rawJSON(t, `{
		"modelId":null,"promptTokens":4,"totalTokens":6,
		"usage":{"outputTokens":2,"cacheReadTokens":7,"cachedReadTokens":7}
	}`))
	if u.Model != "" {
		t.Fatalf("null model=%q", u.Model)
	}
	if u.CachedReadTokens != 7 {
		t.Fatalf("split cached-read=%d", u.CachedReadTokens)
	}
}

func TestSpendAccumulateOnly(t *testing.T) {
	var s Spend
	s = accumulateSpend(s, Spend{TotalTokens: 10, InputTokens: 8, OutputTokens: 2})
	s = accumulateSpend(s, Spend{TotalTokens: 7, InputTokens: 1, OutputTokens: 1})
	if s.TotalTokens != 10 {
		t.Fatalf("stale total reduced spend: %+v", s)
	}
	s = accumulateSpend(s, Spend{TotalTokens: 15, InputTokens: 11, OutputTokens: 4})
	if s.TotalTokens != 15 || s.InputTokens != 11 {
		t.Fatalf("max-style accumulate failed: %+v", s)
	}
	s = accumulateSpend(s, Spend{TotalTokens: 15, InputTokens: 99, OutputTokens: 0})
	if s.InputTokens != 11 || s.OutputTokens != 4 {
		t.Fatalf("equal total replaced component counts: %+v", s)
	}
}

func TestContextUsageIsOccupancy(t *testing.T) {
	u := decodeContextUsage(rawJSON(t, `{"usedTokens":40,"windowTokens":200,"pressure":"warning"}`))
	if u.Used != 40 || u.Size != 200 {
		t.Fatalf("occupancy=%+v", u)
	}
	if u.InputTokens != 0 || u.TotalTokens != 0 {
		t.Fatalf("context must not populate spend: %+v", u)
	}
	u2 := decodeContextUsage(rawJSON(t, `{"usedTokens":1,"windowTokens":null}`))
	if u2.Size != 0 {
		t.Fatalf("null window size=%d", u2.Size)
	}
}

func TestTurnCompletedTerminalEvents(t *testing.T) {
	cases := []struct {
		kind string
		want string
	}{
		{"completed", "stop"},
		{"succeeded", "stop"},
		{"failed", "error"},
		{"error", "error"},
		{"cancelled", "stop"},
		{"interrupted", "stop"},
		{"mystery", "error"},
	}
	for _, tc := range cases {
		ev := decodeTurnCompleted(rawJSON(t, `{"turnId":"t1","terminal":"`+tc.kind+`"}`))
		if ev == nil || ev.T != tc.want {
			t.Fatalf("kind %s -> %+v, want T=%s", tc.kind, ev, tc.want)
		}
		if tc.kind == "mystery" && (ev.Error == "" && ev.StopReason == "") {
			t.Fatal("unknown terminal must be visible")
		}
	}
}

func TestSessionStartedIsTolerated(t *testing.T) {
	f := newFold()
	evs := f.apply("session/started", rawJSON(t, `{"mode":"denyUnmatched","source":"startup"}`))
	if len(evs) != 0 {
		t.Fatalf("session/started should be tolerated without a chat event: %v", evs)
	}
}

func TestAbsentCapabilities(t *testing.T) {
	c := decodeCapabilities(nil)
	if !c.UserInputCapable {
		t.Fatal("absent userInputDialogs means capable")
	}
	if !c.Durable {
		t.Fatal("absent sessionDurability means durable")
	}
	c = decodeCapabilities(rawJSON(t, `{"userInputDialogs":false,"sessionDurability":"ephemeral"}`))
	if c.UserInputCapable || c.Durable {
		t.Fatalf("explicit false/ephemeral: %+v", c)
	}
	c = decodeCapabilities(rawJSON(t, `{"userInputDialogs":true,"sessionDurability":"durable"}`))
	if !c.UserInputCapable || !c.Durable {
		t.Fatalf("explicit true/durable: %+v", c)
	}
}

func TestFoldUsageTurnAndTitles(t *testing.T) {
	f := newFold()
	if evs := f.apply("session/tokenUsage", rawJSON(t, `{"promptTokens":1,"totalTokens":1,"usage":{"outputTokens":0}}`)); len(evs) != 1 || evs[0].Usage == nil {
		t.Fatalf("tokenUsage %v", evs)
	}
	if evs := f.apply("session/contextUsage", rawJSON(t, `{"usedTokens":2,"windowTokens":3}`)); len(evs) != 1 || evs[0].Usage.Used != 2 {
		t.Fatalf("contextUsage %v", evs)
	}
	if evs := f.apply("turn/completed", rawJSON(t, `{"turnId":"t","terminal":"failed","reason":"nope"}`)); len(evs) != 1 || evs[0].T != "error" {
		t.Fatalf("failed turn %v", evs)
	}
	if evs := f.apply("nope", rawJSON(t, `{}`)); len(evs) != 0 {
		t.Fatal("unknown method")
	}
	if evs := f.apply("item/started", json.RawMessage(`{`)); len(evs) != 0 {
		t.Fatal("malformed item")
	}
	f.applyDelta(json.RawMessage(`{}`))
	f.addStreamProv("", namedProv(rawJSON(t, `{"_meta":{"k":1}}`), locNotification))
	if newFold().streaming() != "" {
		t.Fatal("empty stream")
	}
	var none *fold
	if none.streaming() != "" {
		t.Fatal("nil fold streaming")
	}
	if itemTitle(item{Kind: "userShell", CommandText: "ls"}) != "ls" {
		t.Fatal("shell title")
	}
	if itemTitle(item{Kind: "subagent", Objective: "do"}) != "do" {
		t.Fatal("subagent title")
	}
	if itemTitle(item{Kind: "x", FallbackText: "fb"}) != "fb" {
		t.Fatal("fallback title")
	}
	if itemTitle(item{Kind: "x", Tool: "t"}) != "t" {
		t.Fatal("tool fallback title")
	}
	if itemTitle(item{}) != "item" {
		t.Fatal("empty title")
	}
	if usageToSpend(nil).TotalTokens != 0 {
		t.Fatal("nil usage")
	}
	if decodeTokenUsage(json.RawMessage(`{`)) != nil || decodeContextUsage(json.RawMessage(`{`)) != nil {
		t.Fatal("malformed usage")
	}
	if decodeTurnCompleted(json.RawMessage(`{`)) != nil {
		t.Fatal("malformed turn")
	}
	if ev := decodeTurnCompleted(rawJSON(t, `{"terminal":"failed","error":{"kind":"x","message":"y"}}`)); ev == nil || !strings.Contains(ev.Error, "x") {
		t.Fatalf("error object %v", ev)
	}
	r := InitializeResult{ServerInfo: ServerInfo{Fingerprint: "from-info"}}
	if r.FingerprintValue() != "from-info" {
		t.Fatal("FingerprintValue fallback")
	}
	if decodeCapabilities(json.RawMessage(`"nope"`)).Durable != true {
		t.Fatal("bad caps json")
	}
	if namedProv(json.RawMessage(`[]`), locItem) != nil {
		t.Fatal("non-object namedProv")
	}
	if validJSONValue(nil) != nil || packProv() != nil {
		t.Fatal("empty prov")
	}
	id, field, _ := decodeItemDelta(json.RawMessage(`{`))
	if id != "" || field != "" {
		t.Fatal("bad delta")
	}
}

func TestOpenEnumsSurviveInitializeShapes(t *testing.T) {
	var r InitializeResult
	if err := json.Unmarshal([]byte(`{"serverInfo":{"name":"muse","version":"9.9.9-x"},"schema":{"version":99,"fingerprint":"sha256:dead"}}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.ServerInfo.Version != "9.9.9-x" || r.Schema.Version != 99 || r.Schema.Fingerprint != "sha256:dead" {
		t.Fatalf("open values dropped: %+v", r)
	}
}

func rawJSON(t *testing.T, s string) json.RawMessage {
	t.Helper()
	if !json.Valid([]byte(s)) {
		t.Fatalf("invalid fixture JSON: %s", s)
	}
	return json.RawMessage(s)
}

func toolTitle(evs []*Event) string {
	for _, e := range evs {
		if e != nil && e.Tool != nil {
			return e.Tool.Title
		}
	}
	return ""
}

type provE struct {
	Loc string          `json:"loc"`
	Key string          `json:"key"`
	V   json.RawMessage `json:"v"`
}

func provEntries(t *testing.T, raw json.RawMessage) []provE {
	t.Helper()
	if len(raw) == 0 {
		t.Fatal("empty prov")
	}
	var entries []provE
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("prov %s: %v", raw, err)
	}
	return entries
}

func assertProvHas(t *testing.T, raw json.RawMessage, loc, key string) {
	t.Helper()
	for _, e := range provEntries(t, raw) {
		if e.Loc == loc && e.Key == key {
			return
		}
	}
	t.Fatalf("missing loc=%s key=%s in %s", loc, key, raw)
}

func TestStreamingIgnoresKnownNonAgentKinds(t *testing.T) {
	for _, kind := range []string{"userMessage", "reasoning", "toolCall", "mysteryKind"} {
		t.Run(kind, func(t *testing.T) {
			f := newFold()
			f.apply("item/started", rawJSON(t, `{"item":{"itemId":"i1","kind":"`+kind+`","revision":1,"status":"inProgress"}}`))
			f.apply("item/delta", rawJSON(t, `{"itemId":"i1","delta":"secret"}`))
			if got := f.streaming(); got != "" {
				t.Fatalf("kind %s leaked into Streaming: %q", kind, got)
			}
		})
	}
}

func TestDeltaBeforeKindInitiallyInvisible(t *testing.T) {
	f := newFold()
	f.apply("item/delta", rawJSON(t, `{"itemId":"a1","delta":"Hi"}`))
	if got := f.streaming(); got != "" {
		t.Fatalf("delta before kind visible: %q", got)
	}
}

func TestDeltaBeforeKindFlushedOnAgentMessage(t *testing.T) {
	f := newFold()
	f.apply("item/delta", rawJSON(t, `{"itemId":"a1","delta":"Hel"}`))
	f.apply("item/delta", rawJSON(t, `{"itemId":"a1","delta":"lo"}`))
	if got := f.streaming(); got != "" {
		t.Fatalf("pre-kind text leaked: %q", got)
	}
	f.apply("item/started", rawJSON(t, `{"item":{"itemId":"a1","kind":"agentMessage","revision":1,"status":"inProgress"}}`))
	if got := f.streaming(); got != "Hello" {
		t.Fatalf("flushed streaming=%q, want Hello", got)
	}
	f.apply("item/delta", rawJSON(t, `{"itemId":"a1","delta":"!"}`))
	if got := f.streaming(); got != "Hello!" {
		t.Fatalf("post-identify streaming=%q", got)
	}
}

func TestDeltaBeforeKindDiscardedOnNonAgent(t *testing.T) {
	for _, kind := range []string{"userMessage", "reasoning", "toolCall"} {
		t.Run(kind, func(t *testing.T) {
			f := newFold()
			f.apply("item/delta", rawJSON(t, `{"itemId":"x","delta":"nope"}`))
			f.apply("item/started", rawJSON(t, `{"item":{"itemId":"x","kind":"`+kind+`","revision":1,"status":"inProgress"}}`))
			if got := f.streaming(); got != "" {
				t.Fatalf("%s kept hidden text %q", kind, got)
			}
		})
	}
}

func TestUnknownKindDeltaNeverStreams(t *testing.T) {
	f := newFold()
	f.apply("item/started", rawJSON(t, `{"item":{"itemId":"u1","kind":"reminderChild","revision":1,"status":"inProgress"}}`))
	f.apply("item/delta", rawJSON(t, `{"itemId":"u1","delta":"hidden"}`))
	if got := f.streaming(); got != "" {
		t.Fatalf("unknown kind streamed %q", got)
	}
	f.apply("item/updated", rawJSON(t, `{"item":{"itemId":"u1","kind":"reminderChild","revision":2,"status":"inProgress"}}`))
	f.apply("item/delta", rawJSON(t, `{"itemId":"u1","delta":"still"}`))
	if got := f.streaming(); got != "" {
		t.Fatalf("updated unknown kind streamed %q", got)
	}
}

func TestInterleavedAgentAndNonAgentDeltas(t *testing.T) {
	f := newFold()
	f.apply("item/started", rawJSON(t, `{"item":{"itemId":"a1","kind":"agentMessage","revision":1,"status":"inProgress"}}`))
	f.apply("item/started", rawJSON(t, `{"item":{"itemId":"u1","kind":"userMessage","revision":1,"status":"inProgress"}}`))
	f.apply("item/started", rawJSON(t, `{"item":{"itemId":"r1","kind":"reasoning","revision":1,"status":"inProgress"}}`))
	f.apply("item/delta", rawJSON(t, `{"itemId":"a1","delta":"A"}`))
	f.apply("item/delta", rawJSON(t, `{"itemId":"u1","delta":"U"}`))
	f.apply("item/delta", rawJSON(t, `{"itemId":"r1","delta":"R"}`))
	f.apply("item/delta", rawJSON(t, `{"itemId":"a1","delta":"B"}`))
	if got := f.streaming(); got != "AB" {
		t.Fatalf("interleaved streaming=%q, want AB", got)
	}
}

func TestItemCompletionClearsEveryPerItemEntry(t *testing.T) {
	f := newFold()
	f.apply("item/delta", rawJSON(t, `{"itemId":"a1","delta":"Hi","_meta":{"n":1}}`))
	f.apply("item/started", rawJSON(t, `{"item":{"itemId":"a1","kind":"agentMessage","revision":1,"status":"inProgress"}}`))
	f.apply("item/delta", rawJSON(t, `{"itemId":"a1","delta":"!"}`))
	if f.streaming() == "" {
		t.Fatal("expected visible agent text before completion")
	}
	evs := f.apply("item/completed", rawJSON(t, `{"item":{"itemId":"a1","kind":"agentMessage","revision":2,"status":"completed","text":"Hi!"}}`))
	if len(evs) != 1 || evs[0].T != "assistant" {
		t.Fatalf("completed=%+v", evs)
	}
	if f.streaming() != "" {
		t.Fatalf("streaming after complete=%q", f.streaming())
	}
	if len(f.stream) != 0 || len(f.streamIDs) != 0 || len(f.streamProv) != 0 {
		t.Fatalf("stream maps remain: stream=%v ids=%v prov=%v", f.stream, f.streamIDs, f.streamProv)
	}
	if len(f.kinds) != 0 || len(f.hidden) != 0 {
		t.Fatalf("kind/hidden remain kinds=%v hidden=%v", f.kinds, f.hidden)
	}
	f.apply("item/delta", rawJSON(t, `{"itemId":"a1","delta":"late"}`))
	if got := f.streaming(); got != "" {
		t.Fatalf("same id after complete should be hidden again: %q", got)
	}
}

func TestDottedFieldsStillExcludedAfterKindGate(t *testing.T) {
	f := newFold()
	f.apply("item/started", rawJSON(t, `{"item":{"itemId":"a1","kind":"agentMessage","revision":1,"status":"inProgress"}}`))
	f.apply("item/delta", rawJSON(t, `{"itemId":"a1","delta":"Hello"}`))
	f.apply("item/delta", rawJSON(t, `{"itemId":"a1","field":"summary.0","delta":"NOPE"}`))
	f.apply("item/delta", rawJSON(t, `{"itemId":"a1","field":"output","delta":"NOPE2"}`))
	if got := f.streaming(); got != "Hello" {
		t.Fatalf("streaming=%q", got)
	}
}

func TestAgentMessageDeltaProvenanceSurvivesKindGate(t *testing.T) {
	f := newFold()
	f.apply("item/delta", rawJSON(t, `{"itemId":"a1","delta":"Hi","_meta":{"n":1}}`))
	f.apply("item/started", rawJSON(t, `{"item":{"itemId":"a1","kind":"agentMessage","revision":1,"status":"inProgress"},"_meta":{"n":2}}`))
	evs := f.apply("item/completed", rawJSON(t, `{"item":{"itemId":"a1","kind":"agentMessage","revision":2,"status":"completed","text":"Hi","provenance":{"p":true}}}`))
	if len(evs) != 1 || evs[0].T != "assistant" {
		t.Fatalf("evs=%v", evs)
	}
	assertProvHas(t, evs[0].Prov, "notification", "_meta")
	assertProvHas(t, evs[0].Prov, "item", "provenance")
}
