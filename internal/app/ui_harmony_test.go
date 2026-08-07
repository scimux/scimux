package app

import (
	"os"
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
	title := cssBlock(t, css, ".panetitle")
	for _, want := range []string{"font-size: 22px", "font-weight: 700"} {
		t.Run("panetitle/"+want, func(t *testing.T) {
			if !strings.Contains(title, want) {
				t.Errorf(".panetitle must carry %s; got %q", want, title)
			}
		})
	}

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
	if !strings.Contains(col, "font-weight: 600") {
		t.Errorf(".columnhead must be semibold so hierarchy comes from weight+size, not casing; got %q", col)
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

	filled := cssBlock(t, css, ".btn-filled")
	if !strings.Contains(filled, "var(--work)") || !strings.Contains(filled, "#fff") {
		t.Errorf(".btn-filled must be solid petrol on white — the one committing action; got %q", filled)
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
func TestActionBarGeometryIsSharedAndMeetsTouchMinimum(t *testing.T) {
	css := mustProductionCSSCascade(t)

	bar := cssBlock(t, css, ".actionbar {")
	if !strings.Contains(bar, "display: flex") {
		t.Errorf(".actionbar must be a flex row; got %q", bar)
	}
	// HIG toolbars distribute their items across the container rather than
	// clumping them left (the old behaviour) or centred (item 8's first guess).
	if !strings.Contains(bar, "justify-content: space-between") {
		t.Errorf(".actionbar must distribute its buttons across the card, the HIG toolbar idiom; got %q", bar)
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
// no purpose. The inbox must render the same action set as the compact pane,
// minus comment (whose composer lives only in the pane).
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
	for _, act := range []string{"copy", "note", "del"} {
		if !strings.Contains(bm, `b("`+act+`"`) {
			t.Errorf("the shared action row must offer the %q action", act)
		}
	}
	// comment is pane-only: its composer (#bookmarkprompt) is not in the workspace.
	if !strings.Contains(bm, `context === "pane" && nt.node && !nt.anchor`) {
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
	block := cssBlock(t, css, ".strow .lbl {")
	if !strings.Contains(block, "font-weight: 600") {
		t.Errorf(".strow .lbl must be semibold so the title separates from the description line; got %q", block)
	}
	// A past stop is a segment marker, not a chat title: it has no description
	// line under it and stays deliberately secondary. An EXITED thread is still a
	// chat title and keeps the weight — see TestStationTitleStaysBoldWhenTheThreadIsDead.
	stop := cssBlock(t, css, ".strow.stoprow .lbl")
	if !strings.Contains(stop, "font-weight: 400") {
		t.Errorf(".strow.stoprow .lbl must stay regular weight — past stops are deliberately secondary; got %q", stop)
	}
}

// --- items 12 + 14: one "Open chat" control everywhere ------------------------

// The wall-map toolbar used a bare chevron and the search hit bar used the
// words "Show to chat", while the Bookmarks pane and the in-section reference
// both used ICON_JUMP. Same action, same icon, same words.
func TestOpenChatControlIsConsistent(t *testing.T) {
	mapSrc := mustReadWeb(t, "web/js/map.js")
	search := mustReadWeb(t, "web/js/search.js")

	if strings.Contains(mapSrc, "&#8249; Open chat") {
		t.Error("the wall-map toolbar must not use a bare chevron for Open chat — it must use ICON_JUMP like every other surface")
	}
	jumpIdx := strings.Index(mapSrc, "data-jump=")
	if jumpIdx < 0 {
		t.Fatal("wall-map toolbar lost its data-jump control")
	}
	btn := mapSrc[jumpIdx:]
	if end := strings.Index(btn, "</button>"); end >= 0 {
		btn = btn[:end]
	}
	if !strings.Contains(btn, "ICON_JUMP") {
		t.Errorf("the wall-map Open chat button must render ICON_JUMP; got %q", btn)
	}
	if !strings.Contains(btn, "Open chat") {
		t.Errorf("the wall-map jump control must read exactly \"Open chat\"; got %q", btn)
	}

	if strings.Contains(search, "Show to chat") {
		t.Error(`the search hit bar must read "Open chat", the same words as the wall map (item 14)`)
	}
	if !strings.Contains(search, `show: "Open chat"`) {
		t.Error(`SACT_LABEL.show must be "Open chat"`)
	}
	if !strings.Contains(search, "ICON_JUMP") {
		t.Error("the search hit's Open chat button must carry ICON_JUMP, like the Bookmarks pane")
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
