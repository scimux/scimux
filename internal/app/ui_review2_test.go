package app

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Second UI review pass on the round-1 fixes (iPad/iPhone, 2026-08-07). Each
// test names the item it answers; the round-1 tests in ui_harmony_test.go stay
// as they are — these narrow them where the first reading was too generous.

// --- item 1: bold station titles are for EVERY chat, not just live ones ------

// Round 1 raised .strow .lbl to semibold, but .strow.dead .lbl (an exited
// thread) reset the weight to 400, so an ended chat's title stayed visually
// glued to its description line. De-emphasis for a dead thread is the --dim
// colour; weight is now carrying the title/description hierarchy and must not
// be spent twice.
func TestStationTitleStaysBoldWhenTheThreadIsDead(t *testing.T) {
	css := mustProductionCSSCascade(t)

	head := cssBlock(t, css, ".strow .lbl")
	if !strings.Contains(head, "font-weight: 600") {
		t.Fatalf("the station title must be semibold; got %q", head)
	}
	dead := cssBlock(t, css, ".strow.dead .lbl")
	if strings.Contains(dead, "font-weight") {
		t.Errorf("an exited thread's title must keep the shared weight (colour carries the de-emphasis); got %q", dead)
	}
	if !strings.Contains(dead, "var(--dim)") {
		t.Errorf("an exited thread must still be de-emphasised by colour; got %q", dead)
	}
}

// --- item 2: the Journeys chevron points the way the pane travels -----------

// Journeys sits LEFT of Activities, so it is the mirror of the right-hand
// bookmarks pane: opening moves content rightward, closing moves it left.
// Round 1 copied the bookmarks mapping verbatim, which read as flipped.
func TestJourneyChevronMirrorsTheRightHandPane(t *testing.T) {
	nav := mustReadWeb(t, "web/js/navigation.js")
	fn := afterMarker(nav, "export function journeyToggleState")
	if fn == "" {
		t.Fatal("journeyToggleState must live in navigation.js")
	}
	if end := strings.Index(fn, "\n}"); end > 0 {
		fn = fn[:end]
	}
	// open → ‹ (collapse leftward), closed → › (expand rightward).
	openIdx := strings.Index(fn, "&#8249;")
	closedIdx := strings.Index(fn, "&#8250;")
	if openIdx < 0 || closedIdx < 0 {
		t.Fatalf("journeyToggleState must render both chevrons; got %q", fn)
	}
	if openIdx > closedIdx {
		t.Errorf("open must yield ‹ and closed ›, mirroring the left-hand pane's travel; got %q", fn)
	}
}

// --- item 3: the return control has to be findable --------------------------

// "‹ Map" tucked beside the chat title was too quiet. The control now spells
// out its destination in a framed pill — HIG: a control that leaves the current
// context should be unmistakable, and its label names where it goes.
func TestReturnControlIsALabelledPill(t *testing.T) {
	ret := mustReadWeb(t, "web/js/returnto.js")
	if !strings.Contains(ret, "Return to ") {
		t.Error(`the return control must read "Return to <origin>", not a bare chevron + word`)
	}

	css := mustProductionCSSCascade(t)
	pill := cssBlock(t, css, "#chatback.hasreturn")
	if !strings.Contains(pill, "border-radius") {
		t.Errorf("the return control must be a rounded frame; got %q", pill)
	}
	if !strings.Contains(pill, "border:") {
		t.Errorf("the return control must carry a visible frame; got %q", pill)
	}
	if !strings.Contains(pill, "display: inline-flex") {
		t.Errorf("the return pill must survive the desktop .backbtn display:none; got %q", pill)
	}
}

// --- item 4: no armed mode the user cannot see ------------------------------

// "Use in note" arms placement mode, whose only targets are the section rows
// inside the Notes workspace. Offering it from the compact Bookmarks pane while
// that workspace is closed arms a mode with no visible target.
func TestUseInNoteIsGatedOnTheWorkspaceBeingVisible(t *testing.T) {
	bm := mustReadWeb(t, "web/js/bookmarks.js")
	if !strings.Contains(bm, "workspaceOpen") {
		t.Error("the shared action row must know whether the Notes workspace is open")
	}
	// The pane's own render must feed the live flag, not a constant.
	if !strings.Contains(bm, "wsOpen") {
		t.Error("the Bookmarks pane must read the workspace state (d.wsOpen) when rendering its rows")
	}

	// The workspace can be dismissed by the scrim, a swipe or Escape as well as
	// by the shell's own wrapper, so the notification has to come from the one
	// funnel every path goes through: the feature's open()/close().
	notes := mustReadWeb(t, "web/js/notes.js")
	for _, fn := range []string{"function open()", "function close()"} {
		body := afterMarker(notes, "  "+fn)
		if body == "" {
			t.Fatalf("notes.js must define %s", fn)
		}
		if end := strings.Index(body, "\n  }"); end > 0 {
			body = body[:end]
		}
		if !strings.Contains(body, "onVisibilityChange") {
			t.Errorf("notes.js %s must report the visibility change to the shell; got %q", fn, body)
		}
	}

	app := mustReadApp(t)
	// ...and the shell must drop the pane's cached signature when it hears it,
	// so the paperclip appears and disappears with the workspace.
	hook := afterMarker(app, "onVisibilityChange:")
	if hook == "" {
		t.Fatal("the shell must supply notes.js with an onVisibilityChange dependency")
	}
	if end := strings.Index(hook, "\n"); end > 0 {
		hook = hook[:end]
	}
	if !strings.Contains(hook, "invalidateBookmarks") && !strings.Contains(hook, "bookmarksFeature.invalidate") {
		t.Errorf("the workspace visibility hook must invalidate the Bookmarks pane; got %q", hook)
	}
}

// --- item 5: the confirmation toast must be visible from an overlay ---------

// A bookmark filed from the search overlay did toast, but #toast sat at
// z-index 60 — below the search overlay (120), the Notes workspace (110) and
// the archived view (110), so the confirmation was painted underneath the
// surface that triggered it. The toast is the topmost layer by definition:
// it confirms actions taken anywhere, including inside a full-screen overlay.
func TestToastPaintsAboveEveryOverlay(t *testing.T) {
	css := mustProductionCSSCascade(t)
	zre := regexp.MustCompile(`z-index:\s*(\d+)`)

	z := func(selector string) int {
		block := cssBlock(t, css, selector)
		m := zre.FindStringSubmatch(block)
		if m == nil {
			t.Fatalf("%s must declare a z-index; got %q", selector, block)
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("%s z-index %q: %v", selector, m[1], err)
		}
		return n
	}

	toast := z("#toast")
	for _, overlay := range []string{"#searchoverlay", "#notesworkspace", "#archivedview"} {
		if got := z(overlay); toast <= got {
			t.Errorf("#toast (z-index %d) must paint above %s (z-index %d): a confirmation raised from an overlay is invisible below it",
				toast, overlay, got)
		}
	}
}
