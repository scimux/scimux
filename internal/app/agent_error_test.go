package app

import "testing"

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
