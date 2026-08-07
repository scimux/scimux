package app

import (
	"strings"
	"testing"
)

// Getting back out of a search.
//
// The search overlay's "Open chat" has three destinations. A live hit jumps to
// the chat and #chatback becomes "‹ Return to Search" (item 18, already
// covered). A *past* hit cannot do that — the chat is deleted — so it opens the
// read-only archived window instead, and that surface is deliberately built
// from "the same Spotlight material as search": identical panel width, identical
// top offset. From the user's seat the search panel simply became a chat panel.
//
// Which makes its ✕ a trapdoor. The archived view is a modal *pushed by* the
// search overlay, and HIG's rule for that is unambiguous: dismissing a pushed
// surface returns to the one that presented it. Every exit — the button, the
// scrim, Escape — has to land back on the results, and the head has to say so
// before the user commits to the tap.
func TestArchivedChatReturnsToTheSearchThatOpenedIt(t *testing.T) {
	app := mustReadWeb(t, "web/js/app.js")

	// The presenter is passed in, not guessed: openArchived has two callers and
	// only one of them hides itself first.
	search := afterMarker(app, "createSearchFeature({")
	if !strings.Contains(search, "openArchived(uid, seg, rec, at, RETURN_SEARCH)") {
		t.Error("a search hit must tell the archived view which surface presented it")
	}
	// The Bookmarks pane stays open behind the modal, so its ✕ already lands
	// back on the pane; naming a return there would be a second control for
	// something that already works.
	bookmarks := afterMarker(app, "createBookmarksFeature({")
	if i := strings.Index(bookmarks, "openArchived("); i >= 0 {
		if strings.Contains(bookmarks[i:i+len("openArchived(uid, seg, rec, at, RETURN_SEARCH)")], "RETURN_SEARCH") {
			t.Error("the Bookmarks pane is still behind the modal — it is not the search overlay")
		}
	}

	// Every dismissal, not just the labelled one.
	closeFn := afterMarker(app, "function closeArchived()")
	if !strings.Contains(closeFn, "openSearch()") {
		t.Error("dismissing the archived view must restore the surface that presented it")
	}

	// And the label, so the destination is known before the tap.
	if !strings.Contains(app, "returnPillState") {
		t.Error("the archived head must render the shared return pill, not a hand-rolled label")
	}
}

func TestArchivedHeadCarriesALeadingReturnControl(t *testing.T) {
	html := mustReadWeb(t, "web/index.html")

	head := afterMarker(html, `<div id="archivedhead">`)
	if i := strings.Index(head, "</div>"); i >= 0 {
		head = head[:i]
	}
	if !strings.Contains(head, `id="archivedback"`) {
		t.Fatalf("the archived head needs a return control; got %q", head)
	}
	// Leading edge: HIG puts a back control before the title, never after it.
	if strings.Index(head, `id="archivedback"`) > strings.Index(head, `class="ahtext"`) {
		t.Error("the return control belongs at the leading edge, ahead of the title")
	}
	// Hidden by default — it only exists when a presenter can be named.
	back := head[strings.Index(head, `id="archivedback"`):]
	if i := strings.Index(back, ">"); i >= 0 && !strings.Contains(back[:i], "hidden") {
		t.Errorf("the return control must start hidden; got %q", back[:i])
	}
}

// One pill, one look. The chat's return and the modal's return are the same
// promise, so they share a rule rather than drifting into two near-identical
// ones.
func TestTheReturnPillIsSharedBetweenChatAndArchived(t *testing.T) {
	css := mustProductionCSSCascade(t)

	pill := cssBlock(t, css, "#chatback.hasreturn")
	if !strings.Contains(pill, "#archivedback") {
		t.Errorf("the archived return must reuse the chat's pill, not restate it; got %q", pill)
	}
	// The attribute has to win over the pill's display, or a hidden control
	// still takes up space in the head.
	if !strings.Contains(css, "#archivedback[hidden]") {
		t.Error("a display: on the pill overrides the hidden attribute unless it is restated")
	}
}
