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

	// The two larger punched holes, one at each end.
	holes := cssBlock(t, css, ".actionbar.tear::after {")
	if strings.Count(holes, "radial-gradient") != 2 {
		t.Errorf("exactly two punched holes (left and right) belong on the tear line; got %q", holes)
	}
	if !strings.Contains(holes, "calc(100%") {
		t.Errorf("the second hole must be anchored to the right end, not a fixed offset; got %q", holes)
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
