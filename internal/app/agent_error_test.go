package app

import "testing"

// TestFindErrorMessage covers the recursive JSON message extractor used by
// embeddedErrorMessage. Ordering across unrelated map keys is not asserted.
func TestFindErrorMessage(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"direct message", map[string]any{"message": "boom"}, "boom"},
		{"direct message trimmed", map[string]any{"message": "  boom  "}, "boom"},
		{"nested error.message", map[string]any{"error": map[string]any{"message": "inner"}}, "inner"},
		{"nested object", map[string]any{"wrap": map[string]any{"message": "deep"}}, "deep"},
		{"nested array", map[string]any{"items": []any{map[string]any{"message": "from-array"}}}, "from-array"},
		{
			name: "error takes precedence over outer message",
			in:   map[string]any{"message": "outer", "error": map[string]any{"message": "inner"}},
			want: "inner",
		},
		{"blank message ignored", map[string]any{"message": "   "}, ""},
		{"non-string message ignored", map[string]any{"message": 12}, ""},
		{"no usable message", map[string]any{"code": 1, "detail": true}, ""},
		{"nil", nil, ""},
		{"scalar", "not-a-map", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := findErrorMessage(tc.in); got != tc.want {
				t.Fatalf("findErrorMessage() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUserFacingAgentError(t *testing.T) {
	tests := []struct {
		name, agent, raw, want string
	}{
		{
			name:  "grok ACP envelope",
			agent: "grok",
			raw:   `Internal error: {"message":"API error (status 402 Payment Required): Grok Build usage balance exhausted","http_status":402}`,
			want:  "Grok Build usage balance exhausted. Add credits or wait for the balance to reset, then try again.",
		},
		{
			name:  "codex app-server envelope",
			agent: "codex",
			raw:   `turn failed: {"error":{"message":"You've hit your usage limit. Try again at 9:34 PM.","codex_error_info":"usage_limit_exceeded"}}`,
			want:  "You've hit your usage limit. Try again at 9:34 PM.",
		},
		{
			name:  "claude reset copy",
			agent: "claude",
			raw:   "You've hit your session limit · resets 6pm (Europe/Berlin)",
			want:  "You've hit your session limit · resets 6pm (Europe/Berlin).",
		},
		{name: "unknown preserved", agent: "pi", raw: "transport closed", want: "transport closed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := userFacingAgentError(tt.agent, tt.raw); got != tt.want {
				t.Fatalf("userFacingAgentError() = %q, want %q", got, tt.want)
			}
		})
	}
}
