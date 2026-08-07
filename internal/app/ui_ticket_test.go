package app

import (
	"strings"
	"testing"
)

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
	punch := cssBlock(t, css, ".bookmark, .wsibookmark, .wsref.open {")

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
