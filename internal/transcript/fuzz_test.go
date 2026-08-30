package transcript

import (
	"bytes"
	"strings"
	"testing"
)

func FuzzParseLine(f *testing.F) {
	// Claude Code session-log shapes.
	f.Add([]byte(`{"type":"user","timestamp":"2026-07-11T09:00:00.000Z","uuid":"u1","message":{"role":"user","content":"Hello agent, analyze run 42"}}`))
	f.Add([]byte(`{"type":"assistant","timestamp":"2026-07-11T09:00:05.000Z","uuid":"a1","message":{"role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"Looking at run 42 now."},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}`))
	// Codex rollout shape.
	f.Add([]byte(`{"timestamp":"2026-07-11T10:00:02.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Hello codex, sweep the detector thresholds"}]}}`))
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

	f.Fuzz(func(t *testing.T, line []byte) {
		orig := bytes.Clone(line)
		got, ok := ParseLine(line)
		if !bytes.Equal(line, orig) {
			t.Fatalf("ParseLine mutated the caller's slice")
		}
		again, ok2 := ParseLine(line)
		if ok != ok2 || got != again {
			t.Fatalf("ParseLine is not deterministic: first (%+v, %v) second (%+v, %v)", got, ok, again, ok2)
		}
		if !ok {
			if got != (Turn{}) {
				t.Fatalf("ok=false must return the zero Turn, got %+v", got)
			}
			return
		}
		if got.Role != "user" && got.Role != "assistant" {
			t.Fatalf("ok=true Role = %q, want user or assistant", got.Role)
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
	})
}
