package dialoghint

import (
	"regexp"
	"strconv"
	"strings"
)

// cancelAnchorRE is the shared Claude dialog-footer fragment. It accepts both
// documented renderer spellings — ordinary TUI "Esc to cancel" and screen-
// reader "Escape to cancel" — and nothing broader (bare Escape, bare cancel,
// "esc to interrupt", "Escape to cancellation", …). Every matcher that
// anchors on this chrome must compose this fragment so both spellings stay
// in lockstep. HasCancelAnchor uses the same expression and remains
// corroboration-only: it never raises attention by itself.
const cancelAnchorRE = `\besc(?:ape)? to cancel\b`

var (
	// Claude Code approval dialogs
	commandSubstitution = regexp.MustCompile(`(?is)\bcommand contains\b.*\bcommand substitution\b.*` + cancelAnchorRE)
	proceedDialog       = regexp.MustCompile(`(?is)\bdo you want to proceed\b.*` + cancelAnchorRE)
	dontAskAgain        = regexp.MustCompile(`(?is)\byes\b.*and don.?t ask again\b.*` + cancelAnchorRE)
	allowDialog         = regexp.MustCompile(`(?is)\ballow\s+\S+\s+to\s+\w+.*\b\d\.\s*allow\b.*` + cancelAnchorRE)

	// numberedOptionLine matches a dialog menu option: optional pointer glyph,
	// then "N." with N a positive integer. Shape-based (P1a) — not verb-based.
	numberedOptionLine = regexp.MustCompile(`(?i)^\s*[❯›>]?\s*(\d+)\.\s+\S`)
	escToCancel        = regexp.MustCompile(`(?i)` + cancelAnchorRE)

	// Rate limit menus
	rateLimitMenu = regexp.MustCompile(`(?im)^\s*[❯›>]?\s*1\.\s*stop and wait for limit to reset\b`)
	rateLimitHit  = regexp.MustCompile(`(?i)\b(?:hit|reached)\s+your\s+(?:weekly|usage|session)\s+limit\b`)
	usageCommands = regexp.MustCompile(`(?i)\b/usage-credits\b|\bswitch\s+models?\s+with\s+/model\b`)
)

// optionGapLines: consecutive options may sit this many non-option lines apart
// (blank lines, chrome). escAnchorLines: the cancel anchor may trail the last
// option by this many lines (footer chrome).
const (
	optionGapLines = 3
	escAnchorLines = 4
)

// ClassifyVisible returns true if the visible pane content contains a recognized dialog pattern.
func ClassifyVisible(pane string) bool {
	stripped := stripANSI(pane)

	if commandSubstitution.MatchString(stripped) ||
		proceedDialog.MatchString(stripped) ||
		dontAskAgain.MatchString(stripped) ||
		allowDialog.MatchString(stripped) {
		return true
	}

	if numberedOptionsDialog(stripped) {
		return true
	}

	if rateLimitMenu.MatchString(stripped) ||
		(rateLimitHit.MatchString(stripped) && usageCommands.MatchString(stripped)) {
		return true
	}

	return false
}

// HasCancelAnchor reports whether the pane shows the modal-chrome phrase every
// Claude Code dialog carries. It is **corroboration only** and must never raise
// attention by itself: unlike the matchers above it has no structure to bound
// it, and the phrase occurs in ordinary agent prose — any session discussing
// dialoghint prints it (measured live). Its one sanctioned use is shortening a
// stall that already rests on mechanical evidence: quiet pane + the agent owing
// output. In 1190 captured frames the phrase and a real dialog coincided 519
// times with no chrome counterexample, which is why it sharpens the timing; the
// prose case is why it may not decide the outcome.
func HasCancelAnchor(pane string) bool {
	return escToCancel.MatchString(stripANSI(pane))
}

// numberedOptionsDialog is the structural fallback for Claude Code approval
// menus whose verbs change (Write/Edit no longer say "proceed"/"allow"): a
// run of consecutively numbered options (1. then 2., …) with a cancel anchor
// a few lines below the last option. Enumeration of verbs demonstrably
// breaks; shape does not. The anchor accepts both "Esc to cancel" and the
// screen-reader spelling "Escape to cancel".
func numberedOptionsDialog(s string) bool {
	lines := strings.Split(s, "\n")
	// Scan for a run starting at 1., collecting consecutive N, N+1, …
	for i := 0; i < len(lines); i++ {
		n, ok := optionNumber(lines[i])
		if !ok || n != 1 {
			continue
		}
		lastOpt := i
		expect := 2
		j := i + 1
		for j < len(lines) && j-lastOpt <= optionGapLines {
			if m, ok := optionNumber(lines[j]); ok {
				if m != expect {
					break
				}
				lastOpt = j
				expect++
				j++
				continue
			}
			j++
		}
		if expect < 3 {
			// Need at least 1. and 2.
			continue
		}
		// Anchor: cancel chrome within escAnchorLines of the last option.
		for k := lastOpt + 1; k < len(lines) && k-lastOpt <= escAnchorLines; k++ {
			if escToCancel.MatchString(lines[k]) {
				return true
			}
		}
	}
	return false
}

func optionNumber(line string) (int, bool) {
	m := numberedOptionLine.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func stripANSI(s string) string {
	return ansi.ReplaceAllString(s, "")
}
