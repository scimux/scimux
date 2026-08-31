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
	if !strings.Contains(previewFnBody(app, "closePreview"), "openSearch(") {
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

	fn := previewFnBody(app, "openPreviewChat")
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

// Coming back has the same problem in reverse, and BOTH of the overlay's entry
// animations cause it. searchpop would re-pop a panel the user watched stay put;
// searchfade would fade the backdrop blur up from transparent, which reads as
// the whole surface blinking out and back — the preview's scrim vanishes in the
// same frame, so for 160ms there is nothing behind the results. Suppressing only
// the panel left the flicker in place. Both are cancelled together.
func TestReturningToTheResultsDoesNotReplayTheEntryAnimations(t *testing.T) {
	css := mustProductionCSSCascade(t)
	app := mustReadWeb(t, "web/js/app.js")
	search := mustReadWeb(t, "web/js/search.js")

	for _, sel := range []string{"#searchpanel.nopop", "#searchscrim.nopop"} {
		if !strings.Contains(css, sel) {
			t.Fatalf("returning from the preview needs %s to open the overlay quietly", sel)
		}
		if !strings.Contains(cssBlock(t, css, sel), "animation: none") {
			t.Errorf("%s must cancel its entry animation", sel)
		}
	}

	// The shell asks; the module does it. search.js owns the overlay's roots, so
	// it is the only place allowed to put the class on them — and it must set the
	// flag on EVERY open, so a normal open after a return gets its pop back.
	closeFn := previewFnBody(app, "closePreview")
	if !strings.Contains(closeFn, "openSearch({ nopop: true })") {
		t.Error("the preview's return is the caller that asks for a quiet open")
	}
	if strings.Contains(closeFn, "classList") {
		t.Errorf("the shell must not reach into search.js's roots to set the class itself; got %q", closeFn)
	}
	if !strings.Contains(search, "function open(opts)") {
		t.Error("search.js open() must accept the quiet-open flag")
	}
	// Toggling — not adding — is what makes the suppression last exactly one
	// showing. Removing the class while the overlay is on screen would hand the
	// element a fresh animation and replay the very pop it was suppressing.
	if !strings.Contains(search, `classList.toggle("nopop"`) {
		t.Error("the class must be toggled per open, while the overlay is still hidden")
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

// previewFnBody returns the source of `function name()` up to, not including,
// its matching close brace. It used to stop at the first column-0 `\n}\n`,
// which is how a top-level function ended. createApp nests these handlers, so
// that sentinel is now the composition root's closer and the extracted "body"
// would swallow the rest of the file (and every later classList). Brace
// matching keeps the assertion scoped to the named function.
func previewFnBody(src, name string) string {
	body := afterMarker(src, "function "+name+"()")
	open := strings.Index(body, "{")
	if open < 0 {
		return body
	}
	if end := matchingBrace(body, open); end >= 0 {
		return body[:end]
	}
	return body
}

// matchingBrace returns the index of the `}` that closes s[open], skipping
// braces inside strings, template literals, and comments.
func matchingBrace(s string, open int) int {
	depth := 0
	var inStr byte
	inLineComment := false
	inBlockComment := false
	for i := open; i < len(s); i++ {
		c := s[i]
		if inLineComment {
			if c == '\n' {
				inLineComment = false
			}
			continue
		}
		if inBlockComment {
			if c == '*' && i+1 < len(s) && s[i+1] == '/' {
				inBlockComment = false
				i++
			}
			continue
		}
		if inStr != 0 {
			if c == '\\' {
				i++
				continue
			}
			if c == inStr {
				inStr = 0
			}
			continue
		}
		if c == '/' && i+1 < len(s) && s[i+1] == '/' {
			inLineComment = true
			i++
			continue
		}
		if c == '/' && i+1 < len(s) && s[i+1] == '*' {
			inBlockComment = true
			i++
			continue
		}
		if c == '"' || c == '\'' || c == '`' {
			inStr = c
			continue
		}
		if c == '{' {
			depth++
		} else if c == '}' {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}
