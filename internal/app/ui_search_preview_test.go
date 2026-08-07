package app

import (
	"strings"
	"testing"
)

// The search panel, simplified.
//
// A result row used to answer a tap with an action bar whose contents depended
// on invisible facts: a live chat with a surviving directory offered three
// buttons, one without offered two, a deleted chat offered two, an asset one.
// The asymmetry was real — those things genuinely differ — but it was delivered
// as a menu that changed shape after the tap, which is the worst place to put a
// difference the user is supposed to reason about.
//
// So the asymmetry moves into the head of the surface the tap opens, where it
// reads as a sentence rather than a puzzle: every hit opens its surrounding
// conversation in the search overlay's own panel, and the head then says either
// "here is the chat, and here is the way into it" or, by leaving that control
// out, "there is no chat left to go into".
func TestPreviewHeadIsResultsTitleAndOpenChat(t *testing.T) {
	html := mustReadWeb(t, "web/index.html")

	head := afterMarker(html, `<div id="previewhead">`)
	if i := strings.Index(head, `<div id="previewbody"`); i >= 0 {
		head = head[:i]
	}
	for _, id := range []string{`id="previewback"`, `id="previewopen"`, `id="previewclose"`} {
		if !strings.Contains(head, id) {
			t.Fatalf("the preview head needs %s; got %q", id, head)
		}
	}
	// Leading edge, then the title, then the way onward — HIG's navigation bar
	// order, and the order the eye reads.
	back := strings.Index(head, `id="previewback"`)
	title := strings.Index(head, `class="phtext"`)
	open := strings.Index(head, `id="previewopen"`)
	if !(back < title && title < open) {
		t.Errorf("head order must be back, title, open chat; got back=%d title=%d open=%d", back, title, open)
	}
	// Both conditional controls start hidden: the return exists only when a
	// surface presented the modal, the jump only when a chat survives.
	for _, id := range []string{`id="previewback"`, `id="previewopen"`} {
		tag := head[strings.Index(head, id):]
		if i := strings.Index(tag, ">"); i >= 0 && !strings.Contains(tag[:i], "hidden") {
			t.Errorf("%s must start hidden; got %q", id, tag[:i])
		}
	}
}

// Fork is gone from the preview, and from the search overlay behind it. Nothing
// is offered on a deleted chat that a deleted chat cannot do.
func TestNoForkSurvivesOnTheSearchOrPreviewSurfaces(t *testing.T) {
	for _, f := range []string{"web/index.html", "web/js/app.js", "web/js/search.js", "web/js/sheets.js"} {
		src := mustReadWeb(t, f)
		for _, gone := range []string{"archivedforkbtn", "forkFromArchived", "getArchivedFork"} {
			if strings.Contains(src, gone) {
				t.Errorf("%s still carries %q — forking a deleted chat is retired", f, gone)
			}
		}
	}
}

// One gesture, one destination: the search overlay's only hit handler hands the
// address to the preview and names itself as the presenter.
func TestASearchHitOpensThePreview(t *testing.T) {
	app := mustReadWeb(t, "web/js/app.js")

	search := afterMarker(app, "createSearchFeature({")
	if !strings.Contains(search, "openPreview(uid, seg, rec, at, RETURN_SEARCH)") {
		t.Error("a search hit must open the preview and say which surface presented it")
	}
	// The Bookmarks pane stays open behind the modal, so its ✕ already lands
	// back on the pane; naming a return there would be a second control for
	// something that already works.
	bookmarks := afterMarker(app, "createBookmarksFeature({")
	if i := strings.Index(bookmarks, "openPreview("); i >= 0 {
		if strings.Contains(bookmarks[i:i+len("openPreview(uid, seg, rec, at, RETURN_SEARCH)")], "RETURN_SEARCH") {
			t.Error("the Bookmarks pane is still behind the modal — it is not the search overlay")
		}
	}

	// Every dismissal, not just the labelled one.
	closeFn := afterMarker(app, "function closePreview()")
	if !strings.Contains(closeFn, "openSearch()") {
		t.Error("dismissing the preview must restore the surface that presented it")
	}
	if !strings.Contains(app, "returnPillState") {
		t.Error("the preview head must render the shared return pill, not a hand-rolled label")
	}
}

// "Open chat" is a jump, not a dismissal: it leaves the preview for the real
// chat, and the chat it lands in must still know it came from a search — that
// is the whole point of having a way onward rather than making the user close
// the preview, close the overlay, and find the chat themselves.
func TestOpenChatFromThePreviewJumpsAndKeepsTheSearchTrail(t *testing.T) {
	app := mustReadWeb(t, "web/js/app.js")

	fn := afterMarker(app, "function openPreviewChat()")
	if i := strings.Index(fn, "\n}\n"); i >= 0 {
		fn = fn[:i]
	}
	if fn == "" {
		t.Fatal("the preview head's Open chat needs a handler")
	}
	for _, want := range []string{"pendingJump", "chatFeature.invalidate()", `select(`, "RETURN_SEARCH"} {
		if !strings.Contains(fn, want) {
			t.Errorf("Open chat must %s; got %q", want, fn)
		}
	}
	// It must NOT reopen the search overlay on the way out — that is what the
	// return pill is for, and doing both would drop the user back where they
	// deliberately just left.
	if strings.Contains(fn, "openSearch()") {
		t.Error("Open chat leaves the search; it does not restore it")
	}
}

// The preview is presented *in the search overlay's own panel geometry*. That is
// not decoration: it is the reason a tap reads as "these results became a chat"
// rather than "a dialog appeared on top". If the two panels drift apart in width
// or position, the panel visibly jumps on every tap and the illusion — and with
// it the sense that ‹ Results goes back to something still there — breaks.
func TestThePreviewIsDrawnInTheSearchPanelsPlace(t *testing.T) {
	css := mustProductionCSSCascade(t)

	search := cssBlock(t, css, "#searchpanel")
	preview := cssBlock(t, css, "#previewpanel")
	for _, prop := range []string{"width", "margin-top", "max-height"} {
		s, p := cssValue(t, search, prop), cssValue(t, preview, prop)
		if s == "" || s != p {
			t.Errorf("%s: search panel %q vs preview panel %q — the panel must not move under the tap", prop, s, p)
		}
	}
	// And it must not re-animate in: the search panel already popped when the
	// overlay opened. A second pop would announce a new surface.
	if strings.Contains(preview, "animation:") {
		t.Errorf("the preview replaces a panel that is already on screen; got %q", preview)
	}
}

// Coming back has the same problem in reverse: reopening the overlay would
// replay searchpop, so the results would pop in over a panel the user watched
// stay put. The return suppresses it.
func TestReturningToTheResultsDoesNotReplayThePop(t *testing.T) {
	css := mustProductionCSSCascade(t)
	app := mustReadWeb(t, "web/js/app.js")

	if !strings.Contains(css, "#searchpanel.nopop") {
		t.Error("returning from the preview needs a way to open the overlay without the entry animation")
	}
	if !strings.Contains(cssBlock(t, css, "#searchpanel.nopop"), "animation: none") {
		t.Error("#searchpanel.nopop must cancel searchpop")
	}
	if !strings.Contains(afterMarker(app, "function closePreview()"), "nopop") {
		t.Error("the preview's return is the caller that suppresses the pop")
	}
}

// One pill, one look. The chat's return and the modal's return are the same
// promise, so they share a rule rather than drifting into two near-identical
// ones.
func TestTheReturnPillIsSharedBetweenChatAndPreview(t *testing.T) {
	css := mustProductionCSSCascade(t)

	pill := cssBlock(t, css, "#chatback.hasreturn")
	if !strings.Contains(pill, "#previewback") {
		t.Errorf("the preview return must reuse the chat's pill, not restate it; got %q", pill)
	}
	// The attribute has to win over the pill's display, or a hidden control
	// still takes up space in the head.
	if !strings.Contains(css, "#previewback[hidden]") {
		t.Error("a display: on the pill overrides the hidden attribute unless it is restated")
	}
}
