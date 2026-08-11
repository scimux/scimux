package app

import (
	"strconv"
	"strings"
	"testing"
)

// The one selector that punches a card. Both bookmark cards — the compact
// pane's and the workspace inbox's — render their action row only for the card
// the user tapped open, so the punches cannot key off the card: they key off
// the torn row actually being there.
const ticketPunchSelector = ":is(.bookmark, .wsibookmark):has(.actionbar.tear), .wsref.open"

// The ticket tear line. scimux's whole map vocabulary is a fare metaphor —
// journeys, lanes, stations, stops, a fare meter — and a captured turn is
// already shaped like a stub: the chat bubble on top, its actions torn off
// below along a seam. That seam was a plain hairline; it now reads as the
// perforation of a paper fare ticket (two punched holes at the ends, a run of
// small perforations between). Decoration only: it replaces a border, changes
// no geometry, and carries no meaning a user must decode.

func TestTicketTearLineIsASharedDecoration(t *testing.T) {
	css := mustProductionCSSCascade(t)

	tear := cssBlock(t, css, ".actionbar.tear {")
	if !strings.Contains(tear, "position: relative") {
		t.Errorf("the tear line's perforation strips are absolutely positioned against the row; got %q", tear)
	}
	if !strings.Contains(tear, "border-top: none") {
		t.Errorf("the perforation replaces the plain hairline seam, it does not sit on top of one; got %q", tear)
	}

	// The run of small perforations: one repeating dot, tiled horizontally.
	run := cssBlock(t, css, ".actionbar.tear::before {")
	if !strings.Contains(run, "radial-gradient") || !strings.Contains(run, "repeat-x") {
		t.Errorf("the perforation run must be a repeating radial-gradient dot; got %q", run)
	}

	// The row's height is the coordinate the card's punches are measured from,
	// so it cannot be left to drift with padding.
	if !strings.Contains(tear, "height: var(--tearrow") {
		t.Errorf("the torn row's height must be the shared constant the punches are placed against; got %q", tear)
	}

	// The end punches are cut, not drawn: nothing may paint circles on top.
	if strings.Contains(css, ".actionbar.tear::after") {
		t.Error("the end punches must remove material from the card, not paint discs over it")
	}
}

// The two larger punches are real bites out of the card's outer frame — half
// circles split on the border line, material missing the way a conductor's
// punch takes it out of the paper. Painting filled discs inside the frame
// (the first attempt) reads as decoration sitting ON the ticket; only removing
// the card's own background and border reads as paper that is gone.
func TestTicketPunchesRemoveMaterialFromTheCardEdge(t *testing.T) {
	css := mustProductionCSSCascade(t)
	punch := cssBlock(t, css, ticketPunchSelector+" {")

	// A mask cuts the element's own background AND border, which a pseudo
	// element inside the padding box can never reach.
	for _, want := range []string{"-webkit-mask-image", "mask-image"} {
		if !strings.Contains(punch, want) {
			t.Errorf("the punches must be masked out of the card (%s); got %q", want, punch)
		}
	}
	// One hole per end, each a transparent disc centred ON the edge so exactly
	// half of it falls outside the card.
	if strings.Count(punch, "at 0 var(--teary)") != 2 || strings.Count(punch, "at 100% var(--teary)") != 2 {
		t.Errorf("one punch must sit on the left edge and one on the right, both prefixed and not; got %q", punch)
	}
	if !strings.Contains(punch, "transparent") {
		t.Errorf("the punch must be a hole in the mask, not an opaque shape; got %q", punch)
	}
	// Two "everything but a hole" layers only leave two holes when intersected;
	// the default (add) would cover each other's hole and show no punch at all —
	// which is also the safe degradation on an engine without mask-composite.
	if !strings.Contains(punch, "mask-composite: intersect") {
		t.Errorf("the two mask layers must be intersected or neither hole survives; got %q", punch)
	}
	// Anchored to the bottom edge, because that is what the row height fixes.
	if !strings.Contains(punch, "calc(100% - var(--tearrow)") {
		t.Errorf("the punch line must be measured up from the card's bottom; got %q", punch)
	}

	// A reference card carries extra padding below its row, and shows the row
	// only when open — so it must restate the offset rather than inherit a
	// constant that would place its punches inside the card body.
	ref := cssBlock(t, css, ".wsref.open { --tearpad")
	if !strings.Contains(ref, "--tearpad") {
		t.Errorf("the reference card must declare its own padding below the torn row; got %q", ref)
	}
}

func TestTicketTearLineReplacesTheHairlineSeams(t *testing.T) {
	css := mustProductionCSSCascade(t)

	// The three surfaces where an action row is attached under a chat bubble.
	for _, sel := range []string{
		".bookmark .actionbar {",
		".wsibookmark .actionbar {",
		".wsref .actionbar {",
	} {
		block := cssBlock(t, css, sel)
		if strings.Contains(block, "border-top: 1px solid") {
			t.Errorf("%s must not draw both a hairline and a perforation; got %q", sel, block)
		}
	}
}

func TestTicketTearLineIsScopedToBubbleAttachedRows(t *testing.T) {
	bm := mustReadWeb(t, "web/js/bookmarks.js")
	notes := mustReadWeb(t, "web/js/notes.js")

	// Pane + workspace inbox (one shared builder) and the embedded reference.
	if !strings.Contains(bm, `class="actionbar tear"`) {
		t.Error("the shared bookmark action row is attached under a chat bubble and must carry the tear line")
	}
	ref := afterMarker(notes, "export function referenceHTML")
	if !strings.Contains(ref, `class="actionbar tear"`) {
		t.Error("an embedded chat reference is a stub too: its action row must carry the tear line")
	}

	// A note card's own action row is not attached to a chat bubble — it is a
	// document's toolbar, and a perforation there would claim a stub that is
	// not one. Same for the live chat's .bubactions, which float under the
	// bubble with no card seam to perforate.
	if strings.Contains(notes, `class="wscardactions actionbar tear"`) {
		t.Error("a note card's toolbar is not a torn-off ticket stub")
	}
	chat := mustReadWeb(t, "web/js/chat.js")
	if strings.Contains(chat, "actionbar tear") || strings.Contains(chat, "bubactions tear") {
		t.Error("the live chat's bubble actions have no card seam to perforate")
	}
}

// A punched hole without a perforation to lead into it is not a ticket, it is
// a damaged card. Both bookmark cards emit the torn row only for the card the
// user tapped open, so a card-wide punch left every closed card bitten at both
// edges with no seam between them. The mask must therefore be conditioned on
// the row's presence, not on the card's identity — `:has()` keeps the two from
// drifting apart the way a hand-set class would.
func TestTicketPunchesOnlyAppearWithTheTornRow(t *testing.T) {
	css := mustProductionCSSCascade(t)

	if !strings.Contains(css, ticketPunchSelector+" {") {
		t.Fatalf("the punch rule must be gated on the torn row: expected selector %q", ticketPunchSelector)
	}
	// A card class standing alone in a punch rule is the regression: it
	// punches every card, tapped open or not.
	for _, bad := range []string{
		".bookmark, .wsibookmark, .wsref.open {",
		".bookmark, .wsibookmark {",
		".bookmark:has(.actionbar.tear), .wsibookmark, .wsref.open {",
	} {
		if strings.Contains(css, bad) {
			t.Errorf("%q punches closed cards too — both bookmark rows are tap-to-reveal", bad)
		}
	}

	// The gate is only correct if both cards really are tap-to-reveal; if
	// either flips back to an always-visible row, this rule has to be revisited.
	bm := mustReadWeb(t, "web/js/bookmarks.js")
	if !strings.Contains(bm, "openBookmarkT === nt.t") {
		t.Error("the pane card's action row is expected to render only when that card is open")
	}
	notes := mustReadWeb(t, "web/js/notes.js")
	inbox := afterMarker(notes, "export function inboxItemHTML")
	if !strings.Contains(inbox, "deps.open") {
		t.Error("the workspace inbox card's action row must be conditional on the card being open")
	}
}

// Contrast. The punch is a hole, so on the light theme it shows --bg (warm
// paper) through a --surface (white) card: nearly nothing. The hairline the run
// borrowed is tuned for borders between adjacent surfaces, not for a mark that
// has to read as ink on paper. The perforation gets its own token — stronger
// than --hairline, well short of --ink — and the hole gets a punched edge drawn
// in it, so the bite reads even where card and page are near-identical.
func TestTicketPerforationHasItsOwnContrastToken(t *testing.T) {
	css := mustProductionCSSCascade(t)

	root := cssBlock(t, css, ":root {")
	if !strings.Contains(root, "--perf:") {
		t.Errorf("the perforation needs a token of its own, not --hairline; got %q", root)
	}
	// Scan every dark block, not just the text after the last one: afterMarker
	// is LastIndex-based, so any stylesheet that adds its own dark override
	// after tokens.css in the cascade would fail this for no reason.
	darkRestated := false
	for _, part := range strings.Split(css, "@media (prefers-color-scheme: dark)")[1:] {
		if strings.Contains(part, "--perf:") {
			darkRestated = true
			break
		}
	}
	if !darkRestated {
		t.Error("the perforation token must be restated for the dark theme — one value cannot serve both")
	}

	run := cssBlock(t, css, ".actionbar.tear::before {")
	if !strings.Contains(run, "var(--perf)") {
		t.Errorf("the perforation run must use the perforation token; got %q", run)
	}
	if strings.Contains(run, "var(--hairline)") {
		t.Errorf("the run must not fall back to the border hairline; got %q", run)
	}

	// The punched edge: a ring painted just outside the masked hole, so what
	// survives the mask is an arc around the bite.
	edge := cssBlock(t, css, ticketPunchSelector+" { background-image")
	if !strings.Contains(edge, "var(--perf)") || !strings.Contains(edge, "radial-gradient") {
		t.Errorf("the bite needs a drawn edge or it vanishes when card and page match; got %q", edge)
	}
	if !strings.Contains(edge, "var(--tearhole)") {
		t.Errorf("the punched edge must be placed on the same hole radius as the mask; got %q", edge)
	}
}

// Size. At a 5px hole and 2.2px dots the ticket read as a faint dotted rule.
// A conductor's punch is a coarse thing: the run's dots take the diameter the
// end punches used to have, and the end punches grow well past double.
func TestTicketPunchesAreCoarseEnoughToReadAsPaper(t *testing.T) {
	css := mustProductionCSSCascade(t)

	punch := cssBlock(t, css, ticketPunchSelector+" {")
	hole := cssValue(t, punch, "--tearhole")
	if px(t, hole) < 10 {
		t.Errorf("the end punches must be at least double the original 5px; got --tearhole: %s", hole)
	}

	run := cssBlock(t, css, ".actionbar.tear::before {")
	// The run's dots must clear the old 1.1px radius by a wide margin: the dot
	// diameter is now the size the end punch used to be.
	if strings.Contains(run, "1.1px") {
		t.Errorf("the run still uses the original hairline-thin dot; got %q", run)
	}
	if !strings.Contains(run, "2.5px") {
		t.Errorf("the run's dots should be 5px across (2.5px radius); got %q", run)
	}
	// And the run has to be inset past the bigger end punches instead of the
	// hard-coded 14px that cleared only a 5px hole.
	if !strings.Contains(run, "var(--tearhole") {
		t.Errorf("the run's inset must be derived from the punch radius, not hard-coded; got %q", run)
	}
}

// cssValue reads a single declaration out of an already-extracted rule body.
func cssValue(t *testing.T, block, prop string) string {
	t.Helper()
	i := strings.Index(block, prop+":")
	if i < 0 {
		t.Fatalf("no %s declaration in %q", prop, block)
	}
	rest := block[i+len(prop)+1:]
	if j := strings.Index(rest, ";"); j >= 0 {
		rest = rest[:j]
	}
	return strings.TrimSpace(rest)
}

// px parses a plain "12px" length; anything else fails the test loudly rather
// than silently comparing zero.
func px(t *testing.T, v string) float64 {
	t.Helper()
	n, err := strconv.ParseFloat(strings.TrimSuffix(v, "px"), 64)
	if err != nil {
		t.Fatalf("expected a plain px length, got %q", v)
	}
	return n
}
