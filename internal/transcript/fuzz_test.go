package transcript

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func FuzzParseLine(f *testing.F) {
	// Claude Code session-log shapes.
	f.Add([]byte(`{"type":"user","timestamp":"2026-07-11T09:00:00.000Z","uuid":"u1","message":{"role":"user","content":"Hello agent, analyze run 42"}}`))
	f.Add([]byte(`{"type":"user","timestamp":"2027-01-02T03:04:05.000Z","uuid":"u2","message":{"role":"user","content":"\n\n<pasted_content id=\"a1b2\">\nSynthetic pasted prompt\n</pasted_content id=\"a1b2\">\n"}}`))
	f.Add([]byte(`{"type":"assistant","timestamp":"2026-07-11T09:00:05.000Z","uuid":"a1","message":{"role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"Looking at run 42 now."},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}`))
	// Codex rollout shape.
	f.Add([]byte(`{"timestamp":"2026-07-11T10:00:02.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Hello codex, sweep the detector thresholds"}]}}`))
	// Envelope/role disagreement, both directions. Seeded rather than left to
	// the mutator: both halves have to be exact enum words, so random mutation
	// does not reach this shape (25s of fuzzing against the pre-fix parser
	// never produced one). As a seed it is checked on every plain `go test`.
	f.Add([]byte(`{"type":"user","message":{"role":"assistant","content":"<user_instructions>secret"}}`))
	f.Add([]byte(`{"type":"assistant","message":{"role":"user","content":"I am the agent"}}`))
	// Meta, tool-call, unknown type.
	f.Add([]byte(`{"type":"user","timestamp":"2026-07-11T09:00:07.000Z","uuid":"u3","isMeta":true,"message":{"role":"user","content":"meta line that must be skipped"}}`))
	f.Add([]byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}`))
	f.Add([]byte(`{"type":"session_meta","payload":{"id":"c0ffee00-0000-0000-0000-000000000001"}}`))
	// Ways the line goes wrong.
	f.Add([]byte(`{"type":"user","message":`))
	f.Add([]byte(``))
	f.Add([]byte(`null`))
	f.Add([]byte(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"ok","nested":{"a":{"b":{"c":{"d":1}}}}}]}}`))
	f.Add([]byte(`{"type":"user","message":"not-an-object"}`))
	f.Add([]byte(`{"type":"user","message":{"role":"user","content":"   \n\t  "}}`))
	// Synthetic provenance on recognized assistant messages (Phase 2).
	f.Add([]byte(`{"type":"assistant","timestamp":"2026-07-11T09:00:05.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Looking at run 42 now."}],"_meta":{"src":"synth-claude","k":1}}}`))
	f.Add([]byte(`{"timestamp":"2026-07-11T10:00:06.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Sweep started"}],"provenance":{"src":"synth-codex","k":2}}}`))
	f.Add([]byte(`{"type":"assistant","message":{"role":"assistant","content":"ok","_meta":"not-an-object"}}`))
	f.Add([]byte(`{"type":"future_type","_meta":{"src":"x"},"provenance":{"src":"y"},"message":{"role":"assistant","content":"nope"}}`))
	f.Add([]byte(`{"type":"assistant","_meta":{"from":"env"},"message":{"role":"assistant","content":"hi","provenance":{"from":"msg"}}}`))
	f.Add([]byte(`{"type":"response_item","_meta":{"e":1},"payload":{"type":"message","role":"assistant","content":"pong","_meta":{"p":2},"provenance":null}}`))
	f.Add([]byte(`{"type":"assistant","message":{"role":"assistant","content":"hi","Provenance":{"no":1},"_Meta":{"no":2}}}`))

	f.Fuzz(func(t *testing.T, line []byte) {
		orig := bytes.Clone(line)
		got, ok := ParseLine(line)
		if !bytes.Equal(line, orig) {
			t.Fatalf("ParseLine mutated the caller's slice")
		}
		again, ok2 := ParseLine(line)
		if ok != ok2 || !turnsEqual(got, again) {
			t.Fatalf("ParseLine is not deterministic: first (%+v, %v) second (%+v, %v)", got, ok, again, ok2)
		}
		if !ok {
			if !turnsEqual(got, Turn{}) {
				t.Fatalf("ok=false must return the zero Turn, got %+v", got)
			}
			return
		}
		if got.Role != "user" && got.Role != "assistant" {
			t.Fatalf("ok=true Role = %q, want user or assistant", got.Role)
		}
		// A Claude record states its role twice. Membership in the enum is not
		// enough: the rendered role must be the one the envelope declared, or a
		// contradicting record could misattribute a turn and slip an injected
		// block past the role == "user" scaffolding filter. Re-derive the
		// envelope with the same decoder production uses, so the check inherits
		// encoding/json's case-insensitive field matching rather than
		// second-guessing it.
		var env struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
		}
		if json.Unmarshal(line, &env) == nil && len(env.Message) > 0 &&
			(env.Type == "user" || env.Type == "assistant") && got.Role != env.Type {
			t.Fatalf("Claude record type %q yielded Role %q", env.Type, got.Role)
		}
		if got.Text == "" {
			t.Fatalf("ok=true with empty Text")
		}
		if got.Text != strings.TrimSpace(got.Text) {
			t.Fatalf("ok=true Text is not trimmed: %q", got.Text)
		}
		if got.Role == "user" && strings.HasPrefix(got.Text, "<") {
			t.Fatalf("ok=true user Text starts with injected scaffolding: %q", got.Text)
		}
		assertProvContract(t, line, got)
	})
}

// turnsEqual is DeepEqual plus byte-aware Prov comparison. json.RawMessage
// makes Turn non-comparable, so fuzz determinism cannot use ==.
func turnsEqual(a, b Turn) bool {
	if a.Role != b.Role || a.Text != b.Text || a.Time != b.Time ||
		a.UID != b.UID || a.Segment != b.Segment || a.Record != b.Record ||
		a.Agent != b.Agent {
		return false
	}
	return bytes.Equal(a.Prov, b.Prov)
}

type oracleEntry struct {
	Loc string          `json:"loc"`
	Key string          `json:"key"`
	V   json.RawMessage `json:"v"`
}

func assertProvContract(t *testing.T, line []byte, got Turn) {
	t.Helper()
	want := expectedProvEntries(line)
	if len(got.Prov) == 0 {
		if len(want) > 0 {
			t.Fatalf("recognized visible record lost valid provenance: want %d entries from %s", len(want), line)
		}
		return
	}
	if !json.Valid(got.Prov) {
		t.Fatalf("retained Prov is not valid JSON: %s", got.Prov)
	}
	var entries []oracleEntry
	if err := json.Unmarshal(got.Prov, &entries); err != nil {
		t.Fatalf("Prov is not an entry list: %v (%s)", err, got.Prov)
	}
	if len(entries) == 0 {
		t.Fatalf("Prov fabricated an empty list: %s", got.Prov)
	}
	if len(want) == 0 {
		t.Fatalf("Prov invented provenance the source did not carry: %s", got.Prov)
	}
	if len(entries) != len(want) {
		t.Fatalf("Prov lost or invented contributions: got %d want %d (%s)", len(entries), len(want), got.Prov)
	}
	for i, e := range entries {
		if e.Key != "_meta" && e.Key != "provenance" {
			t.Fatalf("Prov fabricated key %q: %s", e.Key, got.Prov)
		}
		if e.Loc != "envelope" && e.Loc != "message" && e.Loc != "payload" {
			t.Fatalf("Prov fabricated loc %q: %s", e.Loc, got.Prov)
		}
		if !json.Valid(e.V) {
			t.Fatalf("Prov[%d].v is not valid JSON: %s", i, e.V)
		}
		w := want[i]
		if e.Loc != w.Loc || e.Key != w.Key || !jsonValueEqual(e.V, w.V) {
			t.Fatalf("Prov[%d] = loc=%s key=%s v=%s, want loc=%s key=%s v=%s",
				i, e.Loc, e.Key, e.V, w.Loc, w.Key, w.V)
		}
	}
}

// expectedProvEntries is a test-owned oracle: it does not call production
// namedProv/validJSONValue/packProv. Keys are matched case-sensitively.
func expectedProvEntries(line []byte) []oracleEntry {
	var env struct {
		Type    string          `json:"type"`
		Message json.RawMessage `json:"message"`
		Payload json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(line, &env) != nil {
		return nil
	}
	switch env.Type {
	case "user", "assistant":
		return append(oracleNamed(json.RawMessage(line), "envelope"), oracleNamed(env.Message, "message")...)
	case "response_item":
		return append(oracleNamed(json.RawMessage(line), "envelope"), oracleNamed(env.Payload, "payload")...)
	}
	return nil
}

func oracleNamed(raw json.RawMessage, loc string) []oracleEntry {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	var out []oracleEntry
	if v, ok := obj["_meta"]; ok {
		v = bytes.TrimSpace(v)
		if len(v) > 0 && json.Valid(v) {
			cp := make(json.RawMessage, len(v))
			copy(cp, v)
			out = append(out, oracleEntry{Loc: loc, Key: "_meta", V: cp})
		}
	}
	if v, ok := obj["provenance"]; ok {
		v = bytes.TrimSpace(v)
		if len(v) > 0 && json.Valid(v) {
			cp := make(json.RawMessage, len(v))
			copy(cp, v)
			out = append(out, oracleEntry{Loc: loc, Key: "provenance", V: cp})
		}
	}
	return out
}

func TestOracleNamedIndependentOfProduction(t *testing.T) {
	got := oracleNamed(json.RawMessage(`{"_meta":{"src":"m"},"provenance":null,"_Meta":{"no":1},"Provenance":{"no":2}}`), "message")
	if len(got) != 2 || got[0].Loc != "message" || got[0].Key != "_meta" || got[1].Key != "provenance" {
		t.Fatalf("both exact keys: %+v", got)
	}
	if !jsonValueEqual(got[0].V, json.RawMessage(`{"src":"m"}`)) || !jsonValueEqual(got[1].V, json.RawMessage(`null`)) {
		t.Fatalf("scalar/null values: %+v", got)
	}
	if oracleNamed(json.RawMessage(`{"_Meta":1,"Provenance":2}`), "envelope") != nil {
		t.Fatal("wrong-case variants must be ignored")
	}
	if oracleNamed(json.RawMessage(`not-object`), "envelope") != nil {
		t.Fatal("non-object source ignored")
	}
	if oracleNamed(json.RawMessage(`[]`), "envelope") != nil {
		t.Fatal("array source ignored")
	}
	if oracleNamed(json.RawMessage(`{"_meta":true}`), "payload")[0].Key != "_meta" {
		t.Fatal("boolean _meta must be recognized")
	}
}

func jsonValueEqual(a, b json.RawMessage) bool {
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return bytes.Equal(a, b)
	}
	return reflect.DeepEqual(av, bv)
}
