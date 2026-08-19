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
	// numberedOptionCapture is the same shape plus the remainder of the line
	// (the label). Used only after NumberedOptions has validated the menu.
	numberedOptionCapture = regexp.MustCompile(`(?i)^\s*[❯›>]?\s*(\d+)\.\s+(\S.*)$`)
	escToCancel           = regexp.MustCompile(`(?i)` + cancelAnchorRE)
	escToInterrupt        = regexp.MustCompile(`(?i)\besc(?:ape)? to interrupt\b`)

	// letteredOptionLine: "y. Yes, I trust this folder" — a single-letter menu
	// key, the shape Claude Code uses for confirm/deny dialogs that predate the
	// session (workspace trust) and therefore have no transcript behind them.
	letteredOptionLine = regexp.MustCompile(`(?i)^\s*[❯›>]?\s*([a-z])\.\s+\S`)

	// enterPromptLine: the terminal input prompt that closes such a dialog,
	// e.g. "Enter y/n:". Anchoring here rather than on the cancel-chrome phrase
	// is deliberate: a lettered run is a much weaker shape than a numbered one,
	// and the cancel phrase appears in ordinary agent prose.
	enterPromptLine = regexp.MustCompile(`(?i)^\s*enter\s+([a-z](?:\s*/\s*[a-z])+)\s*:\s*$`)

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

	if numberedOptionsDialog(stripped) || letteredOptionsDialog(stripped) {
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

// HasInterruptAnchor reports Claude's ordinary working-state footer. It is a
// suppression hint only: it may prevent the neutral quiet/owing "inspect"
// fallback, but it must never feed liveness, retire a permission notice, or
// override a positively classified dialog. Agent prose can contain the phrase;
// a false match therefore costs only an unnecessary warning, never an answer.
func HasInterruptAnchor(pane string) bool {
	return escToInterrupt.MatchString(stripANSI(pane))
}

// NumberedOption is one item from a structurally validated numbered menu.
// N is the key Claude's AX prompt accepts; Label is the pane text after "N.".
type NumberedOption struct {
	N     int
	Label string
}

// NumberedOptions extracts a consecutively numbered menu (starting at 1)
// that is anchored by cancel chrome. It is description-only: callers must
// already know a dialog is visible (Notification or an equivalent proof).
// The pane text here never creates attention on its own.
func NumberedOptions(pane string) ([]NumberedOption, bool) {
	return extractNumberedOptions(stripANSI(pane))
}

// numberedOptionsDialog is the structural fallback for Claude Code approval
// menus whose verbs change (Write/Edit no longer say "proceed"/"allow"): a
// run of consecutively numbered options (1. then 2., …) with a cancel anchor
// a few lines below the last option. Enumeration of verbs demonstrably
// breaks; shape does not. The anchor accepts both "Esc to cancel" and the
// screen-reader spelling "Escape to cancel".
func numberedOptionsDialog(s string) bool {
	_, ok := extractNumberedOptions(s)
	return ok
}

func extractNumberedOptions(s string) ([]NumberedOption, bool) {
	lines := strings.Split(s, "\n")
	// Scan for a run starting at 1., collecting consecutive N, N+1, …
	for i := 0; i < len(lines); i++ {
		n, label, ok := optionNumberAndLabel(lines[i])
		if !ok || n != 1 {
			continue
		}
		opts := []NumberedOption{{N: n, Label: label}}
		lastOpt := i
		expect := 2
		j := i + 1
		for j < len(lines) && j-lastOpt <= optionGapLines {
			if m, lab, ok := optionNumberAndLabel(lines[j]); ok {
				if m != expect {
					break
				}
				opts = append(opts, NumberedOption{N: m, Label: lab})
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
				return opts, true
			}
		}
	}
	return nil, false
}

// letteredOptionsDialog is the structural fallback for Claude Code confirm/
// deny menus whose options are lettered (y. / n.) rather than numbered —
// notably the workspace-trust dialog that appears before any transcript
// exists. A run of ≥2 consecutive lettered option lines with distinct
// letters (same optionGapLines tolerance as the numbered scanner) plus an
// enterPromptLine within escAnchorLines of the last option whose letter set
// equals the options' letter set. The Enter prompt, not "esc to cancel", is
// the anchor: a lettered run is a weaker shape than a numbered one, and the
// cancel phrase occurs in ordinary agent prose.
func letteredOptionsDialog(s string) bool {
	lines := strings.Split(s, "\n")
	for i := 0; i < len(lines); i++ {
		letter, ok := optionLetter(lines[i])
		if !ok {
			continue
		}
		seen := map[string]bool{letter: true}
		lastOpt := i
		j := i + 1
		for j < len(lines) && j-lastOpt <= optionGapLines {
			if m, ok := optionLetter(lines[j]); ok {
				if seen[m] {
					break
				}
				seen[m] = true
				lastOpt = j
				j++
				continue
			}
			j++
		}
		if len(seen) < 2 {
			continue
		}
		for k := lastOpt + 1; k < len(lines) && k-lastOpt <= escAnchorLines; k++ {
			if prompt, ok := enterPromptLetters(lines[k]); ok && sameLetterSet(seen, prompt) {
				return true
			}
		}
	}
	return false
}

func optionLetter(line string) (string, bool) {
	m := letteredOptionLine.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	return strings.ToLower(m[1]), true
}

func enterPromptLetters(line string) (map[string]bool, bool) {
	m := enterPromptLine.FindStringSubmatch(line)
	if m == nil {
		return nil, false
	}
	set := make(map[string]bool)
	for _, p := range strings.Split(m[1], "/") {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		set[p] = true
	}
	if len(set) < 2 {
		return nil, false
	}
	return set, true
}

// LooksLikeWorkspaceTrust reports Claude Code's pre-session workspace-trust
// dialog. A generic lettered y/n menu is not enough: the pane must also
// carry trust-specific wording (the folder-trust option and/or the
// "Accessing workspace" permission title). This is a one-shot launch
// diagnosis, never an attention source and never a liveness input.
func LooksLikeWorkspaceTrust(pane string) bool {
	s := stripANSI(pane)
	if !letteredOptionsDialog(s) {
		return false
	}
	lower := strings.ToLower(s)
	hasTrustFolder := strings.Contains(lower, "trust this folder")
	hasAccessing := strings.Contains(lower, "accessing workspace")
	hasTrustCheck := strings.Contains(lower, "one you trust") ||
		strings.Contains(lower, "project you created")
	return hasTrustFolder && (hasAccessing || hasTrustCheck)
}

func sameLetterSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func optionNumber(line string) (int, bool) {
	n, _, ok := optionNumberAndLabel(line)
	return n, ok
}

func optionNumberAndLabel(line string) (int, string, bool) {
	m := numberedOptionCapture.FindStringSubmatch(line)
	if m == nil {
		return 0, "", false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 1 {
		return 0, "", false
	}
	return n, strings.TrimSpace(m[2]), true
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func stripANSI(s string) string {
	return ansi.ReplaceAllString(s, "")
}
