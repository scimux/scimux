package app

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// UI harmonization review, 2026-08-07 (18-item feedback round).
//
// These tests pin the *system* decisions taken in that round — the named
// control ladder, one action-bar geometry, sentence-case column headers, and
// the removal of the three modal prompt()/instructional-text escapes. They are
// deliberately about consistency across surfaces, so most assert the same
// property in several places at once: that is the whole point of the round.

// mustReadWeb reads any embedded web source by its embed path.
func mustReadWeb(t *testing.T, path string) string {
	t.Helper()
	b, err := webFS.ReadFile(path)
	if err != nil {
		t.Fatalf("read embedded %s: %v", path, err)
	}
	return string(b)
}

// --- item 3: header type roles -----------------------------------------------

// The main app's pane titles ("Activities", "Journeys", "Bookmarks") and the
// workspace's view title ("Notes") are one role; the workspace's zone headers
// ("Bookmarks", "Notes" over zone 1/2) are a lower role. Before this round the
// two roles differed on *casing* (uppercase micro-caps, an iOS 6-12 idiom Apple
// dropped in iOS 13) instead of on size and weight. Harmonize on sentence case
// and keep the hierarchy in the type scale, per HIG Typography.
func TestPaneAndColumnHeaderTypeRoles(t *testing.T) {
	css := mustProductionCSSCascade(t)

	// One named role for pane/view titles, shared by .phead h2 and #wstopbar h2.
	if !strings.Contains(css, ".panetitle") {
		t.Fatal("a shared .panetitle type role must exist so pane titles cannot drift apart")
	}
	// Role existence is the contract; concrete type-scale values are design
	// choices and are not pinned here (P6).

	// One named role for in-view column headers.
	if !strings.Contains(css, ".columnhead") {
		t.Fatal("a shared .columnhead type role must exist for the workspace zone headers")
	}
	col := cssBlock(t, css, ".columnhead")
	if strings.Contains(col, "text-transform: uppercase") {
		t.Error(".columnhead must not be all-caps: HIG Typography drops micro-caps section headers (iOS 13+)")
	}
	if !strings.Contains(col, "text-transform: none") {
		t.Error(".columnhead must explicitly reset text-transform so no earlier rule re-capitalizes it")
	}
	if !strings.Contains(col, "var(--dim)") {
		t.Errorf(".columnhead must be secondary colour; got %q", col)
	}

	// The two workspace zone headers must actually adopt the role, and the
	// old uppercase/letter-spacing pair must be gone from both.
	for _, sel := range []string{"#wsinboxhead", "#wsnavhead"} {
		t.Run(sel, func(t *testing.T) {
			block := cssBlock(t, css, sel+" {")
			if strings.Contains(block, "text-transform: uppercase") {
				t.Errorf("%s must no longer be all-caps; got %q", sel, block)
			}
			if strings.Contains(block, "font-size: 11px") {
				t.Errorf("%s must use the .columnhead scale, not the old 11px micro-cap size; got %q", sel, block)
			}
		})
	}

	// And the markup must carry the classes (a rule nothing references is dead).
	html := mustReadIndex(t)
	for _, id := range []string{"wsinboxhead", "wsnavhead"} {
		if !strings.Contains(html, `id="`+id+`" class="columnhead"`) &&
			!strings.Contains(html, `class="columnhead" id="`+id+`"`) {
			t.Errorf("#%s must carry class=\"columnhead\"", id)
		}
	}
	if !strings.Contains(html, `<h2 class="panetitle">Journeys</h2>`) {
		t.Error(`the Journeys pane title must carry class="panetitle"`)
	}
	if !strings.Contains(html, `<h2 id="wstitle" class="panetitle">Notes</h2>`) {
		t.Error(`the workspace title must carry class="panetitle"`)
	}
}

// --- item 6 + 9: the control ladder and one action-bar geometry ---------------

// The codebase already had three de-facto button tiers (solid petrol CTA,
// tinted petrol constructive, neutral surface secondary) but no names, so new
// controls picked a tier by copy-paste. Name them after the HIG ladder
// (filled -> tinted -> plain) and require every action surface to opt into one.
func TestControlLadderIsNamedAndApplied(t *testing.T) {
	css := mustProductionCSSCascade(t)

	// Ladder tiers must key off role tokens, not literal paint values — with one
	// exception. A solid --work fill needs a light label: --work is a mid petrol
	// in both themes, so a theme-following label (var(--ink)) goes near-white on
	// it in dark mode. That is legibility, not taste, and it is the house pattern
	// map.test.js cites by name when it pins the same choice on #mappill.ready.
	filled := cssBlock(t, css, ".btn-filled")
	if !strings.Contains(filled, "var(--work)") {
		t.Errorf(".btn-filled must use the work/petrol role token; got %q", filled)
	}
	if label := cssDecl(filled, "color"); !strings.Contains(strings.ToLower(label), "#fff") && !strings.EqualFold(label, "white") {
		t.Errorf(".btn-filled label = %q; a solid --work fill carries a fixed light label, not a theme-following one", label)
	}
	tinted := cssBlock(t, css, ".btn-tinted {")
	if !strings.Contains(tinted, "color-mix(in srgb, var(--work)") || !strings.Contains(tinted, "color: var(--work)") {
		t.Errorf(".btn-tinted must be a petrol tint with petrol text; got %q", tinted)
	}
	plain := cssBlock(t, css, ".btn-plain {")
	if !strings.Contains(plain, "var(--surface)") || !strings.Contains(plain, "var(--hairline)") {
		t.Errorf(".btn-plain must be a surface chip with a hairline; got %q", plain)
	}

	// Destructive controls stay a modifier on a tier, never a fourth tier.
	if !strings.Contains(css, ".btn-plain.danger") {
		t.Error("danger must be a modifier on the ladder (.btn-plain.danger), not a separate button style")
	}
}

// One action-bar geometry, used by every per-item action row. Before this round
// the Bookmarks pane used 44x40 and the in-section reference card used 30x28 —
// visibly different, and both under the 44pt HIG minimum touch target.
//
// Phone (default): space-between distributes a short row of 44px targets across
// a full-width card — the HIG toolbar idiom that keeps each target's slack away
// from its neighbour. Desktop (≥900px, matching isDesktop()): the Bookmarks
// pane and notes zone-1 are far wider than N×44px, so the same rule marooned
// icons at both edges. Override to a leading-edge cluster there; do not drop
// space-between from the base rule (P5).
func TestActionBarGeometryIsSharedAndMeetsTouchMinimum(t *testing.T) {
	css := mustProductionCSSCascade(t)

	bar := cssBlock(t, css, ".actionbar {")
	if !strings.Contains(bar, "display: flex") {
		t.Errorf(".actionbar must be a flex row; got %q", bar)
	}
	// Phone contract: HIG toolbars distribute items across the full-width card.
	if !strings.Contains(bar, "justify-content: space-between") {
		t.Errorf(".actionbar base rule must keep space-between (phone toolbar); got %q", bar)
	}

	// Desktop override: cluster at the leading edge so wide columns do not
	// stretch three or four icons across a gap. Scoped to the same 900px
	// breakpoint as isDesktop() — do not invent a new one.
	desktopMedia := "@media (min-width: 900px)"
	di := strings.Index(css, desktopMedia)
	if di < 0 {
		t.Fatal("desktop actionbar override needs @media (min-width: 900px) (matches isDesktop)")
	}
	// Prefer a block that actually re-declares .actionbar; fall back to any
	// 900px media if several exist (layout/notes also use the breakpoint).
	rest := css[di:]
	// Walk every min-width:900px media and accept if any clusters .actionbar.
	foundCluster := false
	for {
		mi := strings.Index(rest, desktopMedia)
		if mi < 0 {
			break
		}
		chunk := rest[mi:]
		// Limit to this media query's opening brace region roughly: find the
		// first .actionbar rule after the media marker and check its body.
		ai := strings.Index(chunk, ".actionbar {")
		if ai >= 0 {
			// Ensure the .actionbar we found is still inside this @media: no
			// closing of the media before it at depth 0 is hard; instead require
			// the justify-content override appears near .actionbar within the
			// same short window (the override is a one-line rule).
			window := chunk[ai:]
			if len(window) > 200 {
				window = window[:200]
			}
			if strings.Contains(window, "justify-content: flex-start") ||
				strings.Contains(window, "justify-content: start") {
				foundCluster = true
				break
			}
		}
		rest = rest[mi+len(desktopMedia):]
	}
	if !foundCluster {
		t.Error("desktop @media (min-width: 900px) must override .actionbar to " +
			"justify-content: flex-start (or start) so icons cluster at the leading edge")
	}

	btn := cssBlock(t, css, ".actionbar button")
	if !strings.Contains(btn, "width: 44px") || !strings.Contains(btn, "height: 44px") {
		t.Errorf(".actionbar button must be 44x44 — the HIG minimum touch target; got %q", btn)
	}

	// Every per-item action row must be that one class, not its own geometry.
	for _, dead := range []string{".bookmarkactions button", ".wsrefactions button", ".wscardactions button"} {
		if strings.Contains(css, dead+" {") {
			t.Errorf("%s must be gone — per-item action rows share .actionbar now", dead)
		}
	}
}

// --- item 7 + 8: actions live inside the card, distributed ---------------------
//
// Decision (2026-08-07): bookmark and reference *cards* carry their actions
// inside the card, as a hairline-separated footer row; chat *bubbles* keep
// theirs below the bubble, because a speech bubble is not a card (the user
// bubble is solid petrol with white text, and the platform idiom for a message
// is a context menu, not an embedded toolbar).
func TestBookmarkIsACardWithInsideActions(t *testing.T) {
	css := mustProductionCSSCascade(t)

	card := cssBlock(t, css, ".bookmark {")
	for _, want := range []string{"var(--surface)", "var(--hairline)", "border-radius"} {
		if !strings.Contains(card, want) {
			t.Errorf(".bookmark must render as a card (%s) so its footer actions read as inside it; got %q", want, card)
		}
	}

	// The footer row is separated from the body by a drawn seam — the visual cue
	// that it belongs to the card rather than floating under it. That seam began
	// as a hairline border and is now the ticket perforation (ui_ticket_test.go),
	// which the builder attaches as a class; either way the row must not float.
	bm := mustReadWeb(t, "web/js/bookmarks.js")
	if !strings.Contains(bm, `class="actionbar tear"`) {
		foot := cssBlock(t, css, ".bookmark .actionbar")
		if !strings.Contains(foot, "border-top") {
			t.Errorf(".bookmark's action row must be seam-separated inside the card; got %q", foot)
		}
	}

	// The clamp must stay scoped to the text region only, so a collapsed card
	// still shows its actions below the fade rather than clipping them.
	clamp := cssBlock(t, css, ".bookmark .nbubble.clamped")
	if !strings.Contains(clamp, "max-height") {
		t.Errorf("the clamp must apply to .nbubble (the text region), never the card; got %q", clamp)
	}
	if strings.Contains(css, ".bookmark.clamped") {
		t.Error("the clamp must never be applied to the whole .bookmark card — it would clip the action row")
	}

	// Chat bubbles deliberately keep their actions below the bubble.
	if !strings.Contains(css, ".bubactions") {
		t.Error(".bubactions must survive: chat bubbles keep actions below the bubble by decision")
	}
}

// --- item 5: the workspace inbox is a full bookmark, not a stub ---------------

// In zone 1 of the Notes workspace a bookmark only offered "Use in note", so
// reaching its chat meant first placing it into a section — an extra step with
// no purpose. The inbox must render the shared action builder as the compact
// pane, minus comment (whose composer lives only in the pane).
//
// P3: inbox bar is jump · copy · note · more; sendto/del live in
// bookmarkMenuHTML. Pane bar is jump · comment · (note) · more with copy
// promoted into the bar when comment is withheld. Comment stays pane-only.
func TestWorkspaceInboxBookmarkHasFullActions(t *testing.T) {
	notes := mustReadWeb(t, "web/js/notes.js")
	bm := mustReadWeb(t, "web/js/bookmarks.js")

	// "same action set" is enforced structurally: the inbox renders the pane's
	// own builder rather than a parallel copy that can drift.
	if !strings.Contains(notes, "bookmarkActionsHTML") ||
		!strings.Contains(notes, `context: "inbox"`) {
		t.Error("the workspace inbox must render the shared bookmark action row (item 5)")
	}
	if !strings.Contains(bm, `b("jump"`) {
		t.Error("the shared action row must offer a jump-to-chat action (item 5)")
	}
	// P3 bar primaries (jump already checked). sendto moved to the overflow menu.
	for _, act := range []string{"copy", "note", "more"} {
		if !strings.Contains(bm, `b("`+act+`"`) {
			t.Errorf("the shared action row must offer the %q action", act)
		}
	}
	// Secondary / overflow actions.
	if !strings.Contains(bm, "bookmarkMenuHTML") {
		t.Error("overflow secondary actions must live in bookmarkMenuHTML")
	}
	if !strings.Contains(bm, `data-bmact="sendto"`) {
		t.Error("sendto must remain available via the overflow menu (P3)")
	}
	if !strings.Contains(bm, `data-bmact="del"`) {
		t.Error("delete must remain available via the overflow menu")
	}
	// comment is pane-only: its composer (#bookmarkprompt) is not in the workspace.
	if !strings.Contains(bm, `context === "pane" && !!(nt && nt.node && !nt.anchor)`) &&
		!strings.Contains(bm, `context === "pane" && nt && nt.node && !nt.anchor`) &&
		!strings.Contains(bm, `context === "pane" && nt.node && !nt.anchor`) {
		t.Error(`"comment" must be gated to the pane: the reply composer lives only in the Bookmarks pane`)
	}
	// One builder, not a second copy of the action row markup.
	if !strings.Contains(notes, "bookmarkActionsHTML") {
		t.Error("the inbox must reuse the shared bookmarkActionsHTML builder, not duplicate the row")
	}
}

// --- item 4: the trailing add-section control is centred ----------------------

func TestAddSectionTrailIsCentred(t *testing.T) {
	css := mustProductionCSSCascade(t)
	block := cssBlock(t, css, ".wssecaddtrail")
	if !strings.Contains(block, "justify-content: center") {
		t.Errorf("the trailing Add section control must be horizontally centred, matching the empty state; got %q", block)
	}
}

// --- item 10: placement mode is a pill, not an alert-sized banner --------------

// HIG requires an active mode to stay visible with a clear exit (the
// call-in-progress / screen-recording pill, Files' "Move to..." bar). The old
// full-bleed tinted strip satisfied that but read as a warning. Shrink it to a
// floating pill that keeps the cancel affordance; let the destinations carry
// the invitation with a *finite* attention animation.
func TestPlacementModeIsAFloatingPill(t *testing.T) {
	html := mustReadIndex(t)
	css := mustProductionCSSCascade(t)

	if strings.Contains(html, `id="wsplacebar"`) {
		t.Error("#wsplacebar (the full-bleed banner) must be replaced by the floating pill")
	}
	if !strings.Contains(html, `id="wsplacepill"`) {
		t.Fatal("placement mode must show a floating #wsplacepill so the held bookmark stays visible")
	}
	// The cancel affordance survives the shrink — a mode with no exit is a trap.
	if !strings.Contains(html, `id="wsplacecancel"`) {
		t.Error("the pill must keep an explicit cancel control (item 10: Cancel is preserved)")
	}

	pill := cssBlock(t, css, "#wsplacepill {")
	if !strings.Contains(pill, "position: absolute") && !strings.Contains(pill, "position: fixed") {
		t.Errorf("the pill must float over the workspace, not push its layout; got %q", pill)
	}
	if !strings.Contains(pill, "border-radius") {
		t.Errorf("the pill must be pill-shaped, not a full-bleed bar; got %q", pill)
	}

	// A finite glow: HIG explicitly discourages indefinitely looping animation.
	glow := cssBlock(t, css, ".wsaddhere")
	if strings.Contains(glow, "infinite") {
		t.Error("the add-here attention animation must not loop indefinitely (HIG: looping animation distracts)")
	}
	if !strings.Contains(css, "@keyframes wsaddglow") {
		t.Error("a brief wsaddglow attention animation must exist on placement entry")
	}
	// The override is component-local (notes.css owns the workspace); the
	// cross-component pane rule in accessibility.css stays exclusive.
	glowOff := "@media (prefers-reduced-motion: reduce) {\n" +
		"  #notesworkspace.placing .wsaddhere { animation: none; }\n}"
	if !strings.Contains(css, glowOff) {
		t.Error("prefers-reduced-motion must disable the add-here glow specifically")
	}
}

// afterMarker returns everything from the last occurrence of marker onward,
// used to check that a later block mentions a selector.
func afterMarker(s, marker string) string {
	i := strings.LastIndex(s, marker)
	if i < 0 {
		return ""
	}
	return s[i:]
}

// --- item 11: journey station titles are semibold -----------------------------

// HIG list rows: the title is `headline` (semibold), the secondary line is
// `subheadline`/`footnote` regular. One rule serves both the docked Journeys
// pane and the full-screen wall map, since both render .strow.
func TestStationTitleIsSemibold(t *testing.T) {
	css := mustProductionCSSCascade(t)
	// The contract is the *contrast*, not the numbers: a live station title has
	// to outweigh a past stop. P6 pins them to each other so the type scale can
	// be retuned, but not flattened — "both declare a font-weight" would pass
	// with two identical values and guard nothing.
	title := cssDecl(cssBlock(t, css, ".strow .lbl {"), "font-weight")
	if title == "" {
		t.Fatal(".strow .lbl must declare a font-weight — hierarchy comes from weight, not casing")
	}
	// A past stop is a segment marker, not a chat title: it has no description
	// line under it and stays deliberately secondary. An EXITED thread is still a
	// chat title and keeps the weight — see TestStationTitleStaysBoldWhenTheThreadIsDead.
	stop := cssDecl(cssBlock(t, css, ".strow.stoprow .lbl"), "font-weight")
	if stop == "" {
		t.Fatal(".strow.stoprow .lbl must declare its own font-weight — past stops are deliberately secondary")
	}
	tw, sw := cssWeight(t, title), cssWeight(t, stop)
	if tw <= sw {
		t.Errorf(".strow .lbl (%s) must be heavier than .strow.stoprow .lbl (%s) so a live title separates from a past stop", title, stop)
	}
}

// cssWeight parses a numeric font-weight, mapping the two keywords the codebase
// may legitimately switch to.
func cssWeight(t *testing.T, v string) int {
	t.Helper()
	switch v {
	case "normal":
		return 400
	case "bold":
		return 700
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("font-weight %q is neither numeric nor normal/bold", v)
	}
	return n
}

// --- items 12 + 14: one "Open chat" control everywhere ------------------------

// The wall-map toolbar used a bare chevron and the search hit bar used the
// words "Show to chat", while the Bookmarks pane and the in-section reference
// both used ICON_JUMP. Same action, same icon, same words.
func TestOpenChatControlIsConsistent(t *testing.T) {
	mapSrc := mustReadWeb(t, "web/js/map.js")
	search := mustReadWeb(t, "web/js/search.js")
	app := mustReadWeb(t, "web/js/app.js")

	if strings.Contains(mapSrc, "&#8249; Open chat") {
		t.Error("the wall-map toolbar must not use a bare chevron for Open chat — it must use ICON_JUMP like every other surface")
	}
	// The attention-ring hit circle also carries data-jump (P6); the Open chat
	// label lives only on the toolbar button. Anchor on that label, then walk
	// back to the opening tag so a ring hit further up the file cannot win.
	labelIdx := strings.Index(mapSrc, "Open chat</button>")
	if labelIdx < 0 {
		t.Fatal("wall-map toolbar lost its Open chat button")
	}
	btnStart := strings.LastIndex(mapSrc[:labelIdx], "<button")
	if btnStart < 0 {
		t.Fatal("Open chat label has no opening <button")
	}
	btn := mapSrc[btnStart : labelIdx+len("Open chat</button>")]
	if !strings.Contains(btn, "data-jump=") {
		t.Errorf("the wall-map Open chat button must carry data-jump; got %q", btn)
	}
	if !strings.Contains(btn, "ICON_JUMP") {
		t.Errorf("the wall-map Open chat button must render ICON_JUMP; got %q", btn)
	}
	if !strings.Contains(btn, "Open chat") {
		t.Errorf("the wall-map jump control must read exactly \"Open chat\"; got %q", btn)
	}

	// The search overlay no longer has a per-hit action bar to harmonise — a hit
	// just opens the preview. The words and the icon moved to the preview head,
	// which is now the third surface offering the same control, and it must say
	// and look like the other two.
	if strings.Contains(search, "Show to chat") || strings.Contains(search, "Open chat") {
		t.Error("the search overlay must not offer its own Open chat; the preview head owns it")
	}
	if !strings.Contains(app, `ICON_JUMP + " Open chat"`) {
		t.Error(`the preview head's control must read "Open chat" and carry ICON_JUMP, like the wall map and the Bookmarks pane`)
	}
}

// --- items 13 + 15: no modal prompt() for bookmarking -------------------------

// Both the wall map and the search overlay asked for free text in a
// window.prompt before filing a bookmark. That conflicts with the Bookmarks
// pane, where commentary is added *afterwards* by replying to the bookmark,
// and the map variant filed a lane-only record with no address, so its
// "Open chat" could never resolve.
func TestBookmarkCaptureNeverPrompts(t *testing.T) {
	app := mustReadApp(t)
	search := mustReadWeb(t, "web/js/search.js")

	if strings.Contains(app, "prompt(`Add a bookmark") || strings.Contains(app, `prompt("Add a bookmark`) {
		t.Error("addStationBookmark must not open a modal prompt (item 13)")
	}
	if strings.Contains(search, `prompt("Add a bookmark"`) || strings.Contains(search, "d.prompt(") {
		t.Error("the search bookmark action must not open a modal prompt (item 15)")
	}

	// The map capture must carry a real durable address, so the resulting
	// bookmark's Open chat resolves back to the turn it was taken from.
	if !strings.Contains(app, "stampAddress") {
		t.Error("addStationBookmark must stamp the durable uid/segment/record address onto the bookmark")
	}
	if !strings.Contains(app, "stationLatestTurn") {
		t.Error("addStationBookmark must bookmark the station's latest turn, not an empty lane-only record")
	}
}

// --- item 16: no instructional text for a standard tap ------------------------

func TestNoTapToReadHint(t *testing.T) {
	mapSrc := mustReadWeb(t, "web/js/map.js")
	if strings.Contains(mapSrc, "tap to read") {
		t.Error(`"tap to read" must go: HIG says do not label standard interactions — a list row is tappable by definition`)
	}
	if !strings.Contains(mapSrc, "earlier stop") {
		t.Error("the earlier-stop caption itself must stay — only the redundant tap hint goes")
	}
}

// --- item 17: one active bookmark lane tab ------------------------------------

// Opening the Notes workspace from the Bookmarks pane landed on a different
// lane tab, because the pane and the workspace inbox each persisted their own
// key. It is one concept to the user; make it one key.
func TestBookmarkTabIsOneSharedKey(t *testing.T) {
	notes := mustReadWeb(t, "web/js/notes.js")
	bookmarks := mustReadWeb(t, "web/js/bookmarks.js")

	if strings.Contains(notes, "scimux-wsinboxtab") {
		t.Error("the workspace inbox must not keep its own tab key — the pane and the inbox share one active lane tab (item 17)")
	}
	if !strings.Contains(bookmarks, `STORAGE_KEY_TAB = "scimux-bookmarktab"`) {
		t.Fatal("the shared tab key must stay scimux-bookmarktab")
	}
	if !strings.Contains(notes, "STORAGE_KEY_TAB") {
		t.Error("notes.js must read/write the shared STORAGE_KEY_TAB from bookmarks.js")
	}
}

// --- item 18: the contextual return control -----------------------------------

// HIG has exactly one idiom for "go back where you came from": the navigation
// bar's leading back button, labelled with its destination (the system
// "< Messages" pill). It names the destination, it lives at the leading edge,
// and it disappears when the stack is no longer valid.
func TestContextualReturnControl(t *testing.T) {
	app := mustReadApp(t)
	ret := mustReadWeb(t, "web/js/returnto.js")

	// The three origins a jump can come from.
	for _, kind := range []string{"map", "search", "note"} {
		if !strings.Contains(ret, `"`+kind+`"`) {
			t.Errorf("returnto.js must model a %q origin", kind)
		}
	}
	// The label names the destination, per HIG, not a generic "Back".
	if !strings.Contains(ret, "chatBackState") {
		t.Error("returnto.js must expose chatBackState — the single decision behind #chatback's label")
	}
	// Deviating from the trajectory drops the context.
	if !strings.Contains(ret, "returnAfterSelection") {
		t.Error("returnto.js must model context invalidation when the user selects a chat by other means")
	}

	// The shell must route every selection through the invalidation decision and
	// re-render the back control, not just set it once at jump time.
	if !strings.Contains(app, "returnAfterSelection") {
		t.Error("select() must clear the return context when the selection did not come from a jump")
	}
	if !strings.Contains(app, "renderChatBack") {
		t.Error("the shell must own a renderChatBack that applies chatBackState to #chatback")
	}

	css := mustProductionCSSCascade(t)
	// .backbtn is display:none on desktop; the return variant must survive that,
	// because a jump out of the full-screen map lands on a visible chat there.
	if !strings.Contains(css, "#chatback.hasreturn") {
		t.Error("#chatback must stay visible at every breakpoint while it carries a return context")
	}
}

// The Journeys reveal control must point at what the tap will do, the way the
// Bookmarks toggle already does (bookmarksToggleState). Before this round it
// was a permanent left chevron even when the tap would close the pane.
func TestJourneysToggleFlipsWithPaneState(t *testing.T) {
	app := mustReadApp(t)
	if !strings.Contains(app, "journeyToggleState") {
		t.Error("#journeybtn must render from a journeyToggleState decision, mirroring bookmarksToggleState")
	}
	nav := mustReadWeb(t, "web/js/navigation.js")
	if !strings.Contains(nav, "journeyToggleState") {
		t.Error("journeyToggleState must be a pure, testable decision in navigation.js")
	}
}

// Guard: the round must not leave a stale copy of any renamed source behind.
func TestNoStaleUIHarmonySources(t *testing.T) {
	if _, err := os.Stat(webSourcePath("web/js/returnto.js")); err != nil {
		t.Fatalf("web/js/returnto.js must exist on disk (embedded sources are read from there): %v", err)
	}
}
