package dialoghint

import "testing"

func TestClassifyVisible(t *testing.T) {
	tests := []struct {
		name  string
		pane  string
		match bool
	}{
		{
			name: "command substitution dialog",
			pane: `The command contains command substitution.
Do you want to proceed?
  1. Yes
  2. No
  Esc to cancel`,
			match: true,
		},
		{
			name: "generic proceed dialog",
			pane: `This will modify files.
Do you want to proceed?
  Esc to cancel`,
			match: true,
		},
		{
			name: "don't ask again dialog",
			pane: `Allow this action?
  1. Yes
  2. Yes and don't ask again
  3. No
  Esc to cancel`,
			match: true,
		},
		{
			name: "allow tool dialog",
			pane: `Allow WebFetch to fetch https://example.com?
  1. Allow once
  2. Allow for this session
  3. Deny
  Esc to cancel`,
			match: true,
		},
		{
			name: "rate limit menu",
			pane: `You've hit your usage limit.
  1. Stop and wait for limit to reset
  2. Switch model
  3. Continue anyway`,
			match: true,
		},
		{
			name: "rate limit with usage command",
			pane: `You've reached your weekly limit.
Check /usage-credits or switch models with /model`,
			match: true,
		},
		{
			name:  "ANSI codes stripped",
			pane:  "\x1b[1mDo you want to proceed?\x1b[0m\n  Esc to cancel",
			match: true,
		},
		{
			name:  "normal prose with proceed",
			pane:  "We should proceed with the implementation",
			match: false,
		},
		{
			name:  "numbered list without dialog chrome",
			pane:  "1. First step\n2. Second step",
			match: false,
		},
		{
			name:  "Esc mentioned without dialog",
			pane:  "Press Esc to exit vim",
			match: false,
		},
		{
			name:  "empty pane",
			pane:  "",
			match: false,
		},
		{
			name: "scrollback with old dialog",
			pane: `Agent working...
Agent working...
Old dialog that was already answered
Do you want to proceed? Esc to cancel
> Answered yes
Agent working...`,
			match: true, // This is intentional - we match if dialog text exists
		},
		// P1a — structural numbered-options shape (Write / Edit / AskUserQuestion).
		// Keys on 1. then 2. plus "esc to cancel", not on "proceed"/"allow" verbs.
		{
			name: "edit approval (numbered options shape)",
			pane: ` Do you want to make this edit to hello.txt?
 ❯ 1. Yes
   2. Yes, allow all edits during this session (shift+tab)
   3. No

 Esc to cancel · Tab to amend`,
			match: true,
		},
		{
			name: "create approval (numbered options shape)",
			pane: ` Do you want to create hello.txt?
 ❯ 1. Yes
   2. Yes, allow all edits during this session (shift+tab)
   3. No

 Esc to cancel · Tab to amend`,
			match: true,
		},
		{
			name: "AskUserQuestion numbered options shape",
			pane: ` Which approach should we take?
 ❯ 1. Keep the poller mechanical
   2. Parse the TUI
   3. Something else

 Esc to cancel`,
			match: true,
		},
		{
			name:  "numbered list in agent output is not a dialog",
			pane:  "Here is the plan:\n1. First step\n2. Second step\n3. Third step\nDone.",
			match: false,
		},
		{
			name:  "numbered options without esc to cancel anchor",
			pane:  "Pick one:\n  1. Yes\n  2. No\n  3. Maybe",
			match: false,
		},
		{
			name:  "esc to cancel with no numbered options",
			pane:  "Press Esc to cancel when ready\nWaiting…",
			match: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyVisible(tt.pane)
			if got != tt.match {
				t.Errorf("ClassifyVisible() = %v, want %v", got, tt.match)
			}
		})
	}
}

func TestStripANSI(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{
			input: "\x1b[1mBold\x1b[0m text",
			want:  "Bold text",
		},
		{
			input: "\x1b[31mRed\x1b[0m and \x1b[32mGreen\x1b[0m",
			want:  "Red and Green",
		},
		{
			input: "No ANSI codes here",
			want:  "No ANSI codes here",
		},
		{
			input: "",
			want:  "",
		},
	}

	for _, tt := range tests {
		got := stripANSI(tt.input)
		if got != tt.want {
			t.Errorf("stripANSI(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
