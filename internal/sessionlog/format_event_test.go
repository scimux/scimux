package sessionlog

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestFormatEvent(t *testing.T) {
	longASCII := strings.Repeat("a", 210)
	longUnicode := strings.Repeat("世", 210) // multi-byte runes; truncation must not split one

	cases := []struct {
		name string
		ev   Event
		want string
	}{
		{"meta with payload", Event{T: "meta", Meta: &MetaEvent{Node: "n1", Agent: "claude", Model: "sonnet"}}, "— session n1 (claude sonnet)"},
		{"meta without payload", Event{T: "meta"}, "— session"},
		{"source with payload", Event{T: "source", Source: &SourceEvent{Path: "/tmp/t.jsonl"}}, "— source /tmp/t.jsonl"},
		{"source without payload", Event{T: "source"}, "— source"},
		{"tool with payload", Event{T: "tool", Tool: &ToolEvent{Title: "Edit", Status: "completed"}}, "· tool Edit [completed]"},
		{"tool without payload", Event{T: "tool"}, "· tool"},
		{"attention with payload", Event{T: "attention", Attention: &AttentionEvent{Kind: "approval", Status: "start"}}, "· wait approval [start]"},
		{"attention without payload", Event{T: "attention"}, "· wait"},
		{"usage with payload", Event{T: "usage", Usage: &UsageEvent{Used: 1, Size: 2}}, "· usage used=1 size=2"},
		{"usage without payload", Event{T: "usage"}, "· usage"},
		{"decision with payload", Event{T: "decision", Decision: &DecisionEvent{Title: "Run ls", Selected: DecOption{Name: "Allow"}}}, "· auto Run ls → Allow"},
		{"decision without payload", Event{T: "decision"}, "· decision"},
		{"user", Event{T: "user", Text: "hello"}, "» hello"},
		{"assistant", Event{T: "assistant", Text: "hi"}, "« hi"},
		{"stop", Event{T: "stop", StopReason: "end_turn"}, "— stop (end_turn)"},
		{"error", Event{T: "error", Error: "boom"}, "! error: boom"},
		{"unknown type", Event{T: "future"}, "· future"},
		{"multiline collapsed", Event{T: "user", Text: "line one\nline  two"}, "» line one line two"},
		{"ascii truncated", Event{T: "user", Text: longASCII}, "» " + strings.Repeat("a", 200) + "…"},
		{"unicode truncated without splitting rune", Event{T: "assistant", Text: longUnicode}, "« " + strings.Repeat("世", 200) + "…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatEvent(tc.ev)
			if got != tc.want {
				t.Fatalf("formatEvent() = %q, want %q", got, tc.want)
			}
			if tc.name == "unicode truncated without splitting rune" {
				body := strings.TrimPrefix(got, "« ")
				body = strings.TrimSuffix(body, "…")
				if !utf8.ValidString(body) {
					t.Fatal("truncated body is not valid UTF-8")
				}
				if utf8.RuneCountInString(body) != 200 {
					t.Fatalf("rune count = %d, want 200", utf8.RuneCountInString(body))
				}
			}
		})
	}
}

func TestParseEventTime(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"empty", "", false},
		{"RFC3339", "2024-06-01T12:00:00Z", true},
		{"RFC3339Nano", "2024-06-01T12:00:00.123456789Z", true},
		{"timezone offset", "2024-06-01T14:00:00+02:00", true},
		{"malformed", "not-a-time", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseEventTime(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (got %v)", ok, tc.ok, got)
			}
			if !tc.ok {
				if !got.IsZero() {
					t.Fatalf("want zero time on failure, got %v", got)
				}
				return
			}
			want, err := time.Parse(time.RFC3339Nano, tc.in)
			if err != nil {
				want, err = time.Parse(time.RFC3339, tc.in)
			}
			if err != nil {
				t.Fatalf("fixture parse: %v", err)
			}
			if !got.Equal(want) {
				t.Fatalf("instant = %v, want %v", got, want)
			}
		})
	}

	t.Run("preserves represented instant across offset forms", func(t *testing.T) {
		a, okA := parseEventTime("2024-06-01T12:00:00Z")
		b, okB := parseEventTime("2024-06-01T14:00:00+02:00")
		if !okA || !okB {
			t.Fatal("both forms must parse")
		}
		if !a.Equal(b) {
			t.Fatalf("instants differ: %v vs %v", a, b)
		}
	})
}
