package app

import (
	"strings"
	"testing"
)

// --- one shape for a list row's verbs -----------------------------------------
//
// Two lists, two answers to the same gesture. The Activities pane floated a
// cluster of round buttons over a folded card's trailing edge and left the row
// exactly where it was. The Notes workspace's zone-2 list grew the row
// downwards to make space for a flat, rectangular bar pushed out to the row's
// two outer edges — so reaching for "delete" moved everything below it, and the
// two lists did not look like they belonged to the same application.
//
// The floating cluster wins: it is the platform idiom for row actions, it does
// not reflow the list under the pointer, and it is the one the maintainer
// wants. Shared as .roundactions so the two cannot drift apart again.
func TestListRowActionsShareTheRoundOverlay(t *testing.T) {
	css := mustProductionCSSCascade(t)

	cluster := cssBlock(t, css, ".roundactions {")
	for _, want := range []string{"position: absolute", "top: 50%", "display: none"} {
		if !strings.Contains(cluster, want) {
			t.Errorf(".roundactions must float over the row rather than grow it (%s); got %q", want, cluster)
		}
	}
	// Both lists opt in by class — the geometry is written once.
	for _, f := range []string{"web/js/cards.js", "web/js/notes.js"} {
		if !strings.Contains(mustReadWeb(t, f), "roundactions") {
			t.Errorf("%s must render its row actions as the shared round cluster", f)
		}
	}
	// And the notes list must let go of the flat footer bar it used to grow.
	notes := mustReadWeb(t, "web/js/notes.js")
	if strings.Contains(notes, `class="wscardactions actionbar`) {
		t.Error("the zone-2 note row must not carry .actionbar any more — that is the in-card footer idiom")
	}
	if strings.Contains(cssBlock(t, css, ".wscardactions {"), "padding-top") {
		t.Error("the zone-2 note row must not reserve vertical space for its actions any more")
	}
}

// The cluster sits at the trailing edge, which on an Activities card is already
// occupied: the unfold chevron lives there, and the trailing button of the
// cluster covered it exactly — hiding the one control that says "there is more
// inside this card". The cluster has to start left of the chevron's reach.
//
// The chevron is pulled outward by a negative trailing margin, so its reach is
// its own width less that pull, measured from the card's padding box.
func TestRowActionsClearTheUnfoldChevron(t *testing.T) {
	css := mustProductionCSSCascade(t)

	chev := cssBlock(t, css, ".card .chev")
	chevW := px(t, cssValue(t, chev, "width"))

	acts := cssBlock(t, css, ".card .actions {")
	inset := px(t, cssValue(t, acts, "--rowacts-inset"))
	if inset < chevW {
		t.Errorf("the actions cluster starts %gpx from the card edge but the %gpx chevron is already there — it covers the unfold control", inset, chevW)
	}
}

// On a pointer the cluster is revealed by hover, so covering the row for that
// moment is fine. The notes list has no long-press gesture, so on touch the
// cluster simply stands there — and then it must not sit on top of the title
// and the lane dots. The row reserves its width instead.
func TestTheStandingNotesClusterDoesNotCoverTheRow(t *testing.T) {
	css := mustProductionCSSCascade(t)

	btn := px(t, cssValue(t, cssBlock(t, css, ".roundactions button"), "width"))
	gutter := px(t, cssValue(t, cssBlock(t, css, ".wscard {"), "--rowacts-gutter"))
	if want := 2 * btn; gutter < want {
		t.Errorf("a touch note row reserves %gpx for a %gpx-wide two-button cluster; it will cover the row", gutter, want)
	}
	// Reserved only where the cluster actually stands. On a pointer it is
	// revealed by hover, and a permanent gutter there would be dead space.
	touch := afterMarker(mustReadWeb(t, "web/css/notes.css"), "@media (hover: none) {")
	if i := strings.Index(touch, "\n}"); i >= 0 {
		touch = touch[:i]
	}
	if !strings.Contains(touch, "var(--rowacts-gutter)") {
		t.Errorf("the reserved gutter belongs in the touch branch that stands the cluster up; got %q", touch)
	}
	// Renaming is the other moment the cluster stands: it swaps the title for a
	// full-width input, which the cluster would otherwise float across.
	if !strings.Contains(cssBlock(t, css, ".wscard.renaming {"), "var(--rowacts-gutter)") {
		t.Error("a renaming note row must reserve the cluster's width too — the cluster stays up over the input")
	}
}
