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
		// Screen-reader (AX) flat menus: documented "Escape to cancel" footer
		// with no separate abbreviated "Esc to cancel" line. Synthetic only.
		{
			name: "AX create file permission",
			pane: `Permission Required: Create file
hello.txt

  1. Yes
  2. Yes, and don't ask again for this session
  3. No

Enter selection [1-3], or Escape to cancel:`,
			match: true,
		},
		{
			name: "AX bash permission",
			pane: `Permission Required: Bash command
go test ./...

  1. Yes
  2. Yes, and don't ask again for this session
  3. No

Enter selection [1-3], or Escape to cancel:`,
			match: true,
		},
		{
			// Full-word path only: no abbreviated "Esc to cancel" anywhere.
			name: "AX AskUserQuestion Escape only",
			pane: `Which approach should we take?

  1. Keep the poller mechanical
  2. Parse the TUI
  3. Other
  4. Chat about this

Enter selection [1-4], or Escape to cancel:`,
			match: true,
		},
		{
			// Invalid-number retry re-prompts the range; still an active menu.
			name: "AX invalid number retry still dialog",
			pane: `Permission Required: Create file
notes.md

  1. Yes
  2. Yes, and don't ask again for this session
  3. No

Invalid selection. Enter a number from 1 to 3, or Escape to cancel:`,
			match: true,
		},
		{
			// Abbreviated footer still classifies (ordinary TUI compatibility).
			name: "AX-shaped menu with abbreviated Esc",
			pane: `Permission Required: Create file
hello.txt

  1. Yes
  2. Yes, and don't ask again for this session
  3. No

Enter selection [1-3], or Esc to cancel:`,
			match: true,
		},
		{
			name:  "prose Enter selection without options or cancel",
			pane:  "The form says Enter selection when ready, but this is just agent prose.",
			match: false,
		},
		{
			name: "completed flat tool output not a dialog",
			pane: `Wrote hello.txt (42 bytes)

✓ Done
$`,
			match: false,
		},
		{
			name: "Escape to cancellation near miss",
			pane: `Pick one:
  1. Yes
  2. No

Enter selection [1-2], or Escape to cancellation:`,
			match: false,
		},
		{
			name: "Escapement to cancel near miss",
			pane: `Pick one:
  1. Yes
  2. No

Escapement to cancel:`,
			match: false,
		},
		{
			name: "bare Escape with numbered options",
			pane: `Pick one:
  1. Yes
  2. No

Press Escape`,
			match: false,
		},
		{
			name: "bare cancel with numbered options",
			pane: `Pick one:
  1. Yes
  2. No

Type cancel to abort`,
			match: false,
		},
		{
			name: "nonconsecutive option numbers",
			pane: `Pick one:
  1. Yes
  3. No

Enter selection [1-3], or Escape to cancel:`,
			match: false,
		},
		{
			name: "malformed option without space after number",
			pane: `Pick one:
  1.Yes
  2.No

Enter selection [1-2], or Escape to cancel:`,
			match: false,
		},
		{
			// Ordinary prose discussing the screen-reader prompt must not
			// classify without consecutive options + cancel anchor structure.
			name: "prose discussing Escape to cancel",
			pane: "Screen-reader menus end with Escape to cancel after Enter selection. " +
				"dialoghint must not treat this sentence as a live dialog.",
			match: false,
		},
		{
			name:  "Permission Required alone is not a dialog",
			pane:  "Permission Required: Create file\nWaiting for input…",
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

// TestHasCancelAnchor pins the corroboration-only signal. It is deliberately
// loose — the phrase occurs in ordinary agent prose (a scimux session
// discussing dialoghint prints it), which is exactly why it may only shorten a
// stall that already has mechanical evidence behind it, never raise attention
// on its own. Both documented Claude spellings are accepted; near-misses are
// not.
func TestHasCancelAnchor(t *testing.T) {
	tests := []struct {
		name string
		pane string
		want bool
	}{
		{"approval dialog footer Esc", " 3. No\n\n Esc to cancel · Tab to amend", true},
		{"screen-reader footer Escape", "Enter selection [1-3], or Escape to cancel:", true},
		{"lowercase esc mid-line", "press esc to cancel the operation", true},
		{"lowercase escape mid-line", "press escape to cancel the operation", true},
		{"mixed case ESCAPE", "OR ESCAPE TO CANCEL:", true},
		{"mixed case ESC", "OR ESC TO CANCEL:", true},
		{"ansi coloured Esc footer", "\x1b[2m Esc to cancel\x1b[0m", true},
		{"ansi coloured Escape footer", "\x1b[2m Escape to cancel\x1b[0m", true},
		{"agent prose quoting esc", "the matcher requires esc to cancel below the options", true},
		{"agent prose quoting Escape", "Anthropic documents Escape to cancel on AX menus", true},
		{"esc to interrupt not cancel", "Working…\n esc to interrupt", false},
		{"Escape to cancellation", "Escape to cancellation of the request", false},
		{"Escapement to cancel", "Escapement to cancel is not chrome", false},
		{"bare Escape", "Press Escape when ready", false},
		{"bare cancel", "Type cancel to abort", false},
		{"empty pane", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasCancelAnchor(tt.pane); got != tt.want {
				t.Errorf("HasCancelAnchor() = %v, want %v", got, tt.want)
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
