package dialoghint

import (
	"regexp"
)

var (
	// Claude Code approval dialogs
	commandSubstitution = regexp.MustCompile(`(?is)\bcommand contains\b.*\bcommand substitution\b.*\besc to cancel\b`)
	proceedDialog       = regexp.MustCompile(`(?is)\bdo you want to proceed\b.*\besc to cancel\b`)
	dontAskAgain        = regexp.MustCompile(`(?is)\byes\b.*and don.?t ask again\b.*\besc to cancel\b`)
	allowDialog         = regexp.MustCompile(`(?is)\ballow\s+\S+\s+to\s+\w+.*\b\d\.\s*allow\b.*\besc to cancel\b`)

	// Rate limit menus
	rateLimitMenu = regexp.MustCompile(`(?im)^\s*[❯›>]?\s*1\.\s*stop and wait for limit to reset\b`)
	rateLimitHit  = regexp.MustCompile(`(?i)\b(?:hit|reached)\s+your\s+(?:weekly|usage|session)\s+limit\b`)
	usageCommands = regexp.MustCompile(`(?i)\b/usage-credits\b|\bswitch\s+models?\s+with\s+/model\b`)
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

	if rateLimitMenu.MatchString(stripped) ||
		(rateLimitHit.MatchString(stripped) && usageCommands.MatchString(stripped)) {
		return true
	}

	return false
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func stripANSI(s string) string {
	return ansi.ReplaceAllString(s, "")
}
