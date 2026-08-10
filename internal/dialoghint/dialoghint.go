package dialoghint

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	// Claude Code approval dialogs
	commandSubstitution = regexp.MustCompile(`(?is)\bcommand contains\b.*\bcommand substitution\b.*\besc to cancel\b`)
	proceedDialog       = regexp.MustCompile(`(?is)\bdo you want to proceed\b.*\besc to cancel\b`)
	dontAskAgain        = regexp.MustCompile(`(?is)\byes\b.*and don.?t ask again\b.*\besc to cancel\b`)
	allowDialog         = regexp.MustCompile(`(?is)\ballow\s+\S+\s+to\s+\w+.*\b\d\.\s*allow\b.*\besc to cancel\b`)

	// numberedOptionLine matches a dialog menu option: optional pointer glyph,
	// then "N." with N a positive integer. Shape-based (P1a) — not verb-based.
	numberedOptionLine = regexp.MustCompile(`(?i)^\s*[❯›>]?\s*(\d+)\.\s+\S`)
	escToCancel        = regexp.MustCompile(`(?i)\besc to cancel\b`)

	// Rate limit menus
	rateLimitMenu = regexp.MustCompile(`(?im)^\s*[❯›>]?\s*1\.\s*stop and wait for limit to reset\b`)
	rateLimitHit  = regexp.MustCompile(`(?i)\b(?:hit|reached)\s+your\s+(?:weekly|usage|session)\s+limit\b`)
	usageCommands = regexp.MustCompile(`(?i)\b/usage-credits\b|\bswitch\s+models?\s+with\s+/model\b`)
)

// optionGapLines: consecutive options may sit this many non-option lines apart
// (blank lines, chrome). escAnchorLines: "esc to cancel" may trail the last
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

// numberedOptionsDialog is the structural fallback for Claude Code approval
// menus whose verbs change (Write/Edit no longer say "proceed"/"allow"): a
// run of consecutively numbered options (1. then 2., …) with "esc to cancel"
// a few lines below the last option. Enumeration of verbs demonstrably
// breaks; shape does not.
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
		// Anchor: "esc to cancel" within escAnchorLines of the last option.
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
