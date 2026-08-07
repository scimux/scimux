package app

import (
	"bytes"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Packet 7J: the sole browser entry is the external module web/js/app.js.
// Parse/check that entry and every production module, prove relative imports
// resolve offline (support graph only — app.js is not executed under bare Node),
// and prove web/test + package.json stay out of the embed. No classic scripts
// and no inline module implementation body remain in index.html.
func TestWebIndexScriptsParse(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JS syntax check")
	}
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)

	// Exactly one external module entry; no classic scripts; no inline body.
	allScriptOpens := regexp.MustCompile(`(?i)<script\b`).FindAllStringIndex(html, -1)
	moduleSrc := regexp.MustCompile(`(?i)<script\s+type="module"\s+src="/js/app\.js"\s*>\s*</script>`).FindAllStringIndex(html, -1)
	inlineModuleBodies := regexp.MustCompile(`(?s)<script\s+type="module">.+?</script>`).FindAllStringIndex(html, -1)
	if len(allScriptOpens) != 1 || len(moduleSrc) != 1 {
		t.Fatalf("script tags: total opens=%d external app.js module=%d; want exactly one external module entry",
			len(allScriptOpens), len(moduleSrc))
	}
	if len(inlineModuleBodies) != 0 {
		t.Fatalf("index.html must not retain an inline module body; found %d", len(inlineModuleBodies))
	}
	if !strings.Contains(html, `<script type="module" src="/js/app.js"></script>`) {
		t.Fatal(`index.html must load sole entry via <script type="module" src="/js/app.js"></script>`)
	}
	// No application-global bridge in the declarative shell.
	for _, banned := range []string{
		`window.scimux`,
		`window.app`,
		`globalThis.`,
	} {
		if strings.Contains(html, banned) {
			t.Fatalf("index.html must not define application global bridge %q", banned)
		}
	}

	app, err := webFS.ReadFile("web/js/app.js")
	if err != nil {
		t.Fatalf("read embedded web/js/app.js: %v", err)
	}
	appSrc := string(app)
	if !strings.Contains(appSrc, "Packet 7J") {
		t.Error("app.js must document Packet 7J composition ownership")
	}
	for _, banned := range []string{
		`window.scimux`,
		`window.app`,
		`globalThis.scimux`,
		`globalThis.app`,
	} {
		if strings.Contains(appSrc, banned) {
			t.Fatalf("app.js must not define application global bridge %q", banned)
		}
	}
	// Relative composition imports (support modules may be reached transitively).
	wantImports := []string{
		`from "./api.js"`,
		`from "./bookmarks.js"`,
		`from "./cards.js"`,
		`from "./chat.js"`,
		`from "./composer.js"`,
		`from "./format.js"`,
		`from "./lanes.js"`,
		`from "./map-model.js"`,
		`from "./map.js"`,
		`from "./navigation.js"`,
		`from "./notes.js"`,
		`from "./polling.js"`,
		`from "./search.js"`,
		`from "./sheets.js"`,
		`from "./usage.js"`,
	}
	for _, imp := range wantImports {
		if !strings.Contains(appSrc, imp) {
			t.Errorf("app.js missing import %q", imp)
		}
	}
	// Absolute /js/ imports must not reappear in the composition entry.
	if strings.Contains(appSrc, `from "/js/`) {
		t.Error(`app.js must use relative ./module.js imports, not absolute /js/ paths`)
	}

	// Enumerate production JS inventory: exact paths, exact embedded bytes, syntax.
	for _, embedPath := range productionJSModules {
		embedded, err := webFS.ReadFile(embedPath)
		if err != nil {
			t.Fatalf("production module %s missing from embed: %v", embedPath, err)
		}
		source, err := os.ReadFile(webSourcePath(embedPath))
		if err != nil {
			t.Fatalf("read source %s: %v", embedPath, err)
		}
		if !bytes.Equal(embedded, source) {
			t.Fatalf("embedded bytes for %s differ from source", embedPath)
		}
		if out, err := exec.Command(node, "--check", webSourcePath(embedPath)).CombinedOutput(); err != nil {
			t.Fatalf("node --check %s: %v\n%s", embedPath, err, out)
		}
	}

	// web/test and package.json remain excluded from the embed.
	for _, forbidden := range []string{"web/package.json", "web/test/smoke.test.js"} {
		if _, err := webFS.ReadFile(forbidden); err == nil {
			t.Fatalf("%s must not be embedded", forbidden)
		}
	}

	// Offline import smoke for the support-module graph only. app.js is the
	// browser composition root (document/localStorage/matchMedia); do not
	// execute it under bare Node.
	tmp := t.TempDir()
	jsDir := filepath.Join(tmp, "js")
	if err := os.MkdirAll(jsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, embedPath := range productionJSModules {
		data, err := webFS.ReadFile(embedPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(jsDir, filepath.Base(embedPath)), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	smoke := filepath.Join(tmp, "import-smoke.mjs")
	var smokeBody strings.Builder
	smokeBody.WriteString("const assert = (await import('node:assert/strict')).default;\n")
	for _, embedPath := range productionJSModules {
		base := filepath.Base(embedPath)
		if base == "app.js" {
			continue // composition root requires browser seams
		}
		smokeBody.WriteString("await import('./js/" + base + "');\n")
	}
	smokeBody.WriteString("assert.ok(true);\n")
	if err := os.WriteFile(smoke, []byte(smokeBody.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, smoke).CombinedOutput(); err != nil {
		t.Fatalf("production module import smoke failed: %v\n%s", err, out)
	}
}

// TestAppCompositionRootOwnership proves app.js is the only composition root
// and shell-level document-handler owner; polling retains visibility/pagehide,
// feature factories retain scoped listeners, and index.html has no implementation
// JS. Factories are constructed once from app.js and not reimplemented there.
func TestAppCompositionRootOwnership(t *testing.T) {
	html := mustReadIndex(t)
	app := mustReadApp(t)

	if strings.Count(html, `<script`) != 1 {
		t.Fatalf("index.html script tags = %d, want 1", strings.Count(html, `<script`))
	}
	if !strings.Contains(html, `src="/js/app.js"`) {
		t.Fatal("index.html must reference sole entry /js/app.js")
	}
	// No inline implementation leftovers in the shell.
	for _, frag := range []string{
		"function setLevel(",
		"createCardsFeature(",
		"createPollingFeature(",
		"document.addEventListener(",
		"localStorage.getItem(",
		"async function tick(){",
		"function startPolling(){",
		"from \"/js/",
		"from \"./",
	} {
		if strings.Contains(html, frag) {
			t.Errorf("index.html still contains implementation fragment %q", frag)
		}
	}

	// Composition root constructs every feature once.
	for _, want := range []string{
		"createCardsFeature(",
		"createMapFeature(",
		"createChatFeature(",
		"createComposerFeature(",
		"createBookmarksFeature(",
		"createNotesFeature(",
		"createSearchFeature(",
		"createSheetsFeature(",
		"createPollingFeature(",
		"cardsFeature.bind()",
		"mapFeature.bind()",
		"chatFeature.bind()",
		"composerFeature.bind()",
		"bookmarksFeature.bind()",
		"notesFeature.bind()",
		"searchFeature.bind()",
		"sheetsFeature.bind()",
		"pollingFeature.bind()",
		"setLevel(level)",
		"mapFeature.restoreChrome()",
		"renderMapTabs()",
		"pollingFeature.loadUI()",
		"pollingFeature.startPolling()",
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.js composition missing %q", want)
		}
	}
	// Document-wide shell listeners live only in app.js (features keep their own).
	for _, want := range []string{
		`document.addEventListener("touchstart"`,
		`document.addEventListener("touchend"`,
		`document.addEventListener("keydown"`,
		`document.addEventListener("focusout"`,
		`document.addEventListener("click"`,
		`document.addEventListener("change"`,
		`$("#scrim").addEventListener("click"`,
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.js document/shell listener missing %q", want)
		}
	}
	// Polling algorithm must remain in polling.js, not reimplemented in app.js.
	for _, banned := range []string{
		"async function tick(){",
		"function startPolling(){",
		"async function loadUI(){",
		"async function flushUI(){",
		"async function pollUI(){",
		`addEventListener("pagehide"`,
		`document.addEventListener("visibilitychange"`,
	} {
		if strings.Contains(app, banned) {
			t.Errorf("app.js must not reimplement polling owner %q", banned)
		}
	}
	// Soft-line exception: composition shell may exceed ~500 lines but must
	// remain free of extracted feature template owners.
	if !strings.Contains(app, "composition/shell exception") && !strings.Contains(app, "Composition/shell ownership") {
		t.Error("app.js must document composition/shell ownership when acting as the entry exception")
	}
}

// Markdown helpers live in web/js/format.js (Packet 6A/6F). Finding 97: paragraph
// lines must be escaped per line and then joined with a real <br>, never the
// other way around.

// Packet 7I: state tick + UI-sync coordination live in polling.js with
// executable Node coverage in web/test/polling.test.js. Keep ownership wiring:
// one import/bind owner; no reimplementation in shell or composition entry.
func TestPollingModuleWired(t *testing.T) {
	app := mustReadApp(t)
	html := mustReadIndex(t)
	if !strings.Contains(app, `from "./polling.js"`) || !strings.Contains(app, "createPollingFeature") {
		t.Error("production must import and instantiate createPollingFeature from polling.js")
	}
	if !strings.Contains(app, "pollingFeature.bind()") {
		t.Error("production must bind pollingFeature as the visibility/pagehide/poll-timer owner")
	}
	for _, frag := range []string{
		"async function tick(){",
		"function startPolling(){",
		"async function loadUI(){",
		"async function flushUI(){",
		"async function pollUI(){",
		`addEventListener("pagehide"`,
		`document.addEventListener("visibilitychange"`,
	} {
		if strings.Contains(app, frag) || strings.Contains(html, frag) {
			t.Errorf("composition/shell still contains old polling/UI-sync owner %q", frag)
		}
	}
}

// Shell ARIA for the closed Bookmarks toggle; toggle glyph/aria sync is
// executed by bookmarks.test.js (bookmarksToggleState). Wiring ownership stays.
func TestNotesToggleDirectionMatchesPaneState(t *testing.T) {
	html := mustReadIndex(t)
	app := mustReadApp(t)
	if !strings.Contains(html, `id="bookmarksbtn" aria-label="open bookmarks"></button>`) {
		t.Fatal("closed bookmarks toggle should announce opening bookmarks")
	}
	if !strings.Contains(app, `from "./bookmarks.js"`) || !strings.Contains(app, "createBookmarksFeature") {
		t.Fatal("production must import and instantiate createBookmarksFeature from bookmarks.js")
	}
	if !strings.Contains(app, "bookmarksFeature.bind()") {
		t.Fatal("production must bind bookmarksFeature as the Bookmarks event owner")
	}
}

// Packet 7D: the composer is a shell singleton outside every polled render
// region. Behavioral coverage lives in web/test/composer.test.js; this keeps
// the document placement contract and single production owner.
func TestComposerSingletonOutsidePolledRegions(t *testing.T) {
	htmlB, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	html := string(htmlB)
	for _, id := range []string{
		`id="promptbar"`, `id="attstage"`, `id="prompt"`,
		`id="sendbtn"`, `id="attadd"`, `id="attmenu"`,
		`id="attimg"`, `id="attfile"`,
	} {
		if !strings.Contains(html, id) {
			t.Errorf("shell missing composer root %s", id)
		}
	}
	msgsOpen := strings.Index(html, `id="msgs"`)
	chatLoading := strings.Index(html, `id="chatloading"`)
	promptbar := strings.Index(html, `id="promptbar"`)
	bookmarks := strings.Index(html, `id="bookmarklist"`)
	if msgsOpen < 0 || chatLoading < 0 || promptbar < 0 || bookmarks < 0 ||
		!(msgsOpen < chatLoading && chatLoading < promptbar && promptbar < bookmarks) {
		t.Fatal("#promptbar must follow #msgs in the chat shell (singleton after the message list)")
	}
	for _, id := range []string{"promptbar", "attstage", "prompt", "sendbtn", "attadd", "attmenu", "attimg", "attfile"} {
		if got := strings.Count(html, `id="`+id+`"`); got != 1 {
			t.Errorf("composer root #%s occurs %d times, want 1", id, got)
		}
	}
	if !strings.Contains(html, `id="prompt"`) || !strings.Contains(html, `contenteditable="true"`) {
		t.Fatal("#prompt must remain a contenteditable singleton in the shell")
	}
	app := mustReadApp(t)
	if !strings.Contains(app, `from "./composer.js"`) || !strings.Contains(app, "createComposerFeature") {
		t.Fatal("production must import and instantiate createComposerFeature from composer.js")
	}
	if !strings.Contains(app, "composerFeature.bind()") {
		t.Fatal("production must bind composerFeature exactly as the composer event owner")
	}
	for _, banned := range []string{
		"function promptText(",
		"function setComposerBusy(",
		"function setComposerClosed(",
		"function setAttachAvail(",
		"function sendPrompt(",
		"async function interruptPrompt(",
		"function renderStage(",
		"async function uploadOne(",
	} {
		if strings.Contains(html, banned) || strings.Contains(app, banned) {
			t.Errorf("shell/composition must not retain composer implementation: %s", banned)
		}
	}
}

// Host connectivity DOM chrome and CSS stay here; tick orchestration and
// online/offline/unreachable branches live in polling.js with executable Node
// coverage in web/test/polling.test.js. Card age flip lives in cards.js with
// web/test/cards.test.js (cardTimeItems/HTML, CARD_TIME_SWAP_MS, updateAges).
func TestActivityCardShowsUserInteractionAgeAndHostConnectivity(t *testing.T) {
	html := mustReadIndex(t)
	css := mustProductionCSSCascade(t)
	app := mustReadApp(t)
	if !strings.Contains(html, `<span class="host offline" id="host">scimux</span>`) {
		t.Error("activity host connectivity shell missing #host offline chrome")
	}
	for _, want := range []string{
		`createPollingFeature`,
		`import { createPollingFeature } from "./polling.js";`,
		`setHostOnline: ok =>`,
		`setServerUnreachable: () => { $("#sys").textContent = "server unreachable"; }`,
		`createCardsFeature`,
		`import { createCardsFeature } from "./cards.js";`,
		`function updateCardAges(animate=false){ cardsFeature.updateAges(animate); }`,
	} {
		if !strings.Contains(app, want) {
			t.Errorf("activity card interaction/connectivity wiring missing %q", want)
		}
	}
	for _, want := range []string{
		`#statusbar .host::before`,
		`#statusbar .host.online::before { background: #34C759; }`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("activity card connectivity CSS missing %q", want)
		}
	}
}

// Packet 7B: station long-press lives in map.js; Node covers bind/destroy.
// Structural: production wires createMapFeature; no residual inline longpress.
func TestDockedMapStationLongPressEdits(t *testing.T) {
	html := mustReadIndex(t)
	app := mustReadApp(t)
	if !strings.Contains(app, `from "./map.js"`) || !strings.Contains(app, "createMapFeature") {
		t.Fatal("production must wire map.js for station long-press")
	}
	if strings.Contains(html, `longpress($("#mapwrap"), ".strow"`) {
		t.Error("inline mapwrap station longpress must move into map.js")
	}
}

// Packet 7B: fold/chip algorithms and markup live in map.js; executable Node
// coverage is web/test/map.test.js (chipTapPlan, applyNewLaneFolds, laneChipStyle,
// attentionStationSVG). Keep production wiring + CSS structural contracts.
func TestJourneyLaneFoldAndChipWiring(t *testing.T) {
	html := mustReadIndex(t)
	app := mustReadApp(t)
	css := mustProductionCSSCascade(t)
	if !strings.Contains(app, `from "./map.js"`) || !strings.Contains(app, "createMapFeature") {
		t.Fatal("production must wire map.js for journey fold/chip behavior")
	}
	// Shell/composition must not retain duplicate fold/chip listeners or implementations.
	for _, dead := range []string{
		`longpress($("#lanechips")`,
		`$("#lanechips").addEventListener`,
		`let mapFoldKnown = new Set`,
		`function laneChipStyle(`,
	} {
		if strings.Contains(html, dead) || strings.Contains(app, dead) {
			t.Errorf("shell/composition map fold/chip code still present: %q", dead)
		}
	}
	for _, want := range []string{
		`animation: mapAttentionDot 1.65s ease-in-out infinite;`,
		`.lhead .lattn, .attnstation-glow, .attnstation-ring { animation: none; }`,
		`.lhead .chev { color: var(--dim); font-size: 15px; width: 16px;`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("journey lane fold/chip CSS missing %q", want)
		}
	}
	chipCSSStart := strings.Index(css, ".lanechip {")
	chipCSSEnd := strings.Index(css[chipCSSStart:], ".lanechip.selected")
	if chipCSSStart < 0 || chipCSSEnd < 0 {
		t.Fatal("lane chip CSS not found")
	}
	chipCSS := css[chipCSSStart : chipCSSStart+chipCSSEnd]
	if strings.Contains(css, ".lanechip.focused") || strings.Contains(chipCSS, "opacity:") {
		t.Fatal("lane chips should no longer show selection by dimming unselected chips")
	}
}

func TestEmbeddedAgentAssetsServe(t *testing.T) {
	assets, err := fs.Sub(webFS, "web/assets")
	if err != nil {
		t.Fatalf("sub web/assets: %v", err)
	}
	h := http.StripPrefix("/assets/", http.FileServer(http.FS(assets)))
	req := httptest.NewRequest("GET", "/assets/agents/openai.svg", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("asset code = %d body %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Font Awesome") {
		t.Fatal("openai asset does not look like the vendored Font Awesome SVG")
	}
}

// Fork kind is fixed at creation time (finding #2): a fork into a lane that did
// not yet exist is Y-new forever, and must NOT redraw as an origin-coloured S
// once that lane later gathers more stations. Execute forkKind under node
// against synthetic node sets to lock the chronological (not population-based)
// classification.

// Packet 7C: key-row HTML lives in chat.js; Node chat.test.js covers ACP vs tmux
// presentation. Retain the negative branch that ACP must not invent y/n keys
// (Node asserts positive protocol options only, not y/n absence).
func TestStructuredApprovalDoesNotInventYN(t *testing.T) {
	b, err := os.ReadFile(webSourcePath("web/js/chat.js"))
	if err != nil {
		t.Fatalf("read chat.js: %v", err)
	}
	src := string(b)
	i := strings.Index(src, `if (source === "acp")`)
	if i < 0 {
		t.Fatal("structured approval branch not found")
	}
	j := strings.Index(src[i:], `["1","2","3","4","y","n"`)
	acpEnd := strings.Index(src[i:], "return `<span class=\"hint\">${escape(HINTS")
	if acpEnd < 0 {
		acpEnd = j
	}
	if acpEnd < 0 {
		t.Fatal("structured approval branch end not found")
	}
	branch := src[i : i+acpEnd]
	if strings.Contains(branch, `["y","n"]`) || strings.Contains(branch, `['y','n']`) ||
		strings.Contains(branch, `["1","2","3","4","y","n"`) {
		t.Fatal("structured approval branch must render protocol options only, not y/n fallback")
	}
}

// Search overlay DOM/ARIA/CSS structural contracts. Open/close/focus-return
// behavior is executed by web/test/search.test.js.
func TestSearchOverlayShell(t *testing.T) {
	html := mustReadIndex(t)
	css := mustProductionCSSCascade(t)
	app := mustReadApp(t)

	for _, want := range []string{
		`id="searchbtn"`,
		`id="searchoverlay"`,
		`role="dialog"`,
		`aria-modal="true"`,
		`id="searchinput"`,
		`id="searchfeed"`,
		`id="searchclose"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("search overlay markup missing %q", want)
		}
	}
	if !strings.Contains(css, "backdrop-filter: blur") {
		t.Error("search scrim should blur the app behind it")
	}
	if !strings.Contains(css, "prefers-reduced-transparency: reduce") {
		t.Error("search overlay must honor prefers-reduced-transparency")
	}
	if !strings.Contains(css, "prefers-reduced-motion: reduce") {
		t.Error("search overlay must honor prefers-reduced-motion")
	}
	if !strings.Contains(app, `from "./search.js"`) || !strings.Contains(app, "createSearchFeature") {
		t.Error("production must import and instantiate createSearchFeature from search.js")
	}
	if !strings.Contains(app, "function openSearch(") || !strings.Contains(app, "function closeSearch(") {
		t.Error("app.js must retain thin openSearch/closeSearch wrappers for navigation")
	}
	if !strings.Contains(app, "searchFeature.bind()") {
		t.Error("production must bind searchFeature as the Search event owner")
	}
}

// Hit identity attributes are executed by search.test.js (searchHitHTML).
// Retain the shell connectivity that openPreview joins seg/rec to the preview API.
func TestSearchHitCarriesIdentity(t *testing.T) {
	app := mustReadApp(t)
	if !strings.Contains(app, `"&seg=" + encodeURIComponent(seg`) ||
		!strings.Contains(app, `"&rec=" + encodeURIComponent(rec`) {
		t.Error("openPreview must send seg/rec to /api/preview")
	}
}

// Packet 7C: consumePendingJump + history bubble templates live in chat.js;
// Node covers matchPendingJumpInHist and history expansion. Keep a thin wire
// check that the production chat owner is bound.
func TestSearchEarlierHistoryJump(t *testing.T) {
	app := mustReadApp(t)
	if !strings.Contains(app, `from "./chat.js"`) || !strings.Contains(app, "createChatFeature") {
		t.Fatal("production must wire createChatFeature for earlier-history jumps")
	}
	if !strings.Contains(app, "chatFeature.bind()") {
		t.Fatal("production must bind chatFeature as the chat event owner")
	}
}

func TestPinnedIcons(t *testing.T) {
	app := mustReadApp(t)
	html := mustReadIndex(t)
	if !strings.Contains(app, "const ICON_PIN =") || !strings.Contains(app, "const ICON_UNPIN =") {
		t.Error("ICON_PIN / ICON_UNPIN inlined glyphs missing from app.js")
	}
	if strings.Contains(app, "fa-thumbtack") || strings.Contains(app, `class="fa-`) ||
		strings.Contains(html, "fa-thumbtack") || strings.Contains(html, `class="fa-`) {
		t.Error("icons must be inlined SVG, not FontAwesome webfont classes")
	}
	if strings.Contains(app, "Commercial License") || strings.Contains(app, "Font Awesome Pro") ||
		strings.Contains(html, "Commercial License") || strings.Contains(html, "Font Awesome Pro") {
		t.Error("must not embed a Pro/Commercial-licensed glyph artifact")
	}
}

// TestPinnedTab asserts a conditional "Pinned" tab sits between the scope tab
// and "Archived", shows a live count, and appears only when something is pinned.
// Tab HTML generation lives in cards.js (Packet 7A); Node covers cardTabsHTML.

// Tab HTML order/count is executed by cards.test.js (cardTabsHTML). Keep the
// document shell root and cards wiring ownership.
func TestPinnedTab(t *testing.T) {
	html := mustReadIndex(t)
	app := mustReadApp(t)
	if !strings.Contains(html, `id="cardtabs"`) {
		t.Fatal("#cardtabs root missing from document shell")
	}
	if !strings.Contains(app, `from "./cards.js"`) || !strings.Contains(app, "createCardsFeature") {
		t.Fatal("production must wire cards.js for tab rendering")
	}
}

// Debounce/abort/min-length/feed rendering are executed by search.test.js.
// Retain the Go↔JS connectivity that SEARCH_MAX mirrors the server cap.
func TestSearchFeed(t *testing.T) {
	b, err := os.ReadFile(webSourcePath("web/js/search.js"))
	if err != nil {
		t.Fatalf("read search.js: %v", err)
	}
	src := string(b)
	if want := fmt.Sprintf("export const SEARCH_MAX = %d;", maxSearchQuery); !strings.Contains(src, want) {
		t.Errorf("SEARCH_MAX must mirror the server cap: expected %q", want)
	}
	if !strings.Contains(src, "export const SEARCH_MIN =") {
		t.Error("SEARCH_MIN must remain exported from search.js")
	}
}

// Adaptive hit actions and show/fork/bookmark branches are executed by
// search.test.js. Retain the cross-feature fork injection seam.
func TestSearchActionBar(t *testing.T) {
	app := mustReadApp(t)
	if !strings.Contains(app, `forkFromTurn: (text, parent) => sheetsFeature.forkFromTurn(text, parent)`) {
		t.Error("search/chat must inject sheetsFeature.forkFromTurn")
	}
	sheets, err := os.ReadFile(webSourcePath("web/js/sheets.js"))
	if err != nil {
		t.Fatalf("read sheets.js: %v", err)
	}
	if !strings.Contains(string(sheets), "function forkFromTurn(text, parent)") {
		t.Error("forkFromTurn must accept an explicit parent so search can fork from the hit's node")
	}
}

// Preview surface: shell DOM/ARIA, no composer, and its entry points.
// Hit routing and the head's two controls are covered by Node and by
// ui_search_preview_test.go.
func TestPreviewView(t *testing.T) {
	html := mustReadIndex(t)
	app := mustReadApp(t)
	for _, want := range []string{
		`id="previewview"`,
		`role="dialog"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("preview shell missing %q", want)
		}
	}
	for _, want := range []string{
		"function openPreview(",
		"function closePreview(",
		"function renderPreview(",
		`fetch("/api/preview?uid="`,
	} {
		if !strings.Contains(app, want) {
			t.Errorf("preview implementation missing %q", want)
		}
	}
	// The read-only surface must not host a chat composer.
	if vs := strings.Index(html, `<div id="previewview"`); vs >= 0 {
		ve := strings.Index(html[vs:], "\n</div>\n") + vs
		if ve > vs && strings.Contains(html[vs:ve], "<textarea") {
			t.Error("the read-only preview must not contain a composer/textarea")
		}
	}
	// A live chat's assets resolve here exactly as in the chat — same node, same
	// asset map — or the same turn would read "unavailable" one tap apart.
	if !strings.Contains(app, `splitAssetRefs(t.text || "", previewNode, d.assets || {}, { iconFile: ICON_FILE })`) {
		t.Error("preview asset projection must pass the node, its assets, and ICON_FILE")
	}
}

// Collapse decision matrix is executed by bookmarks.test.js. Keep CSS clamp
// contract and shell Bookmarks roots; ephemerality/order covered by Node.
func TestNoteCollapseWiring(t *testing.T) {
	html := mustReadIndex(t)
	css := mustProductionCSSCascade(t)
	if !strings.Contains(css, ".bookmark .nbubble.clamped") {
		t.Error("note-collapse CSS clamp is missing")
	}
	for _, id := range []string{"bookmarkspane", "bookmarklist", "bookmarkflags", "bookmarksbtn", "notesbtn", "bookmarkpeek"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("shell missing Bookmarks root #%s", id)
		}
	}
}

// Use-in-note placement behavior is executed by notes.test.js / bookmarks.test.js.
// Retain the composition injection seam Bookmarks → Notes.startPlacement.
func TestWebUseInNoteWiring(t *testing.T) {
	app := mustReadApp(t)
	if !strings.Contains(app, `from "./notes.js"`) || !strings.Contains(app, "createNotesFeature") {
		t.Error("production must import and instantiate createNotesFeature from notes.js")
	}
	if !strings.Contains(app, "notesFeature.startPlacement") && !strings.Contains(app, "startPlacement: nt => notesFeature.startPlacement") {
		t.Error("Bookmarks startPlacement injection must call notesFeature.startPlacement")
	}
	bm, err := os.ReadFile(webSourcePath("web/js/bookmarks.js"))
	if err != nil {
		t.Fatal(err)
	}
	// the action row is now one shared builder (bookmarkActionsHTML), so the
	// attribute is templated; the "note" action itself must still be wired.
	if !strings.Contains(string(bm), `data-bmact="${act}"`) ||
		!strings.Contains(string(bm), `b("note", "use in note"`) {
		t.Error(`use-in-note wiring missing the "note" action in bookmarks.js`)
	}
}

// Jump resolution branches are executed by bookmarks/chat/search Node suites.
// Retain the shared resolver wiring across app.js, bookmarks.js, and notes.js.
func TestWebSharedJumpResolver(t *testing.T) {
	app := mustReadApp(t)
	bm, err := os.ReadFile(webSourcePath("web/js/bookmarks.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(bm)
	notes, err := os.ReadFile(webSourcePath("web/js/notes.js"))
	if err != nil {
		t.Fatal(err)
	}
	nsrc := string(notes)
	if !strings.Contains(src, "function jumpToChatAddress(") {
		t.Error("shared jumpToChatAddress resolver not defined in bookmarks.js")
	}
	if !strings.Contains(app, "function jumpToChatAddress(") {
		t.Error("app.js must re-export jumpToChatAddress for Notes/search callers")
	}
	if !strings.Contains(nsrc, "jumpToChatAddress(") {
		t.Error("notes.js must call injected jumpToChatAddress for reference jump")
	}
}

func TestWorkspaceMatchesWallMap(t *testing.T) {
	css := mustProductionCSSCascade(t)
	ws := cssBlock(t, css, "#notesworkspace {")
	if !strings.Contains(ws, "var(--sbh)") {
		t.Errorf("#notesworkspace must offset below the status bar (var(--sbh)), like #map; got %q", ws)
	}
	panel := cssBlock(t, css, "#wspanel {")
	if !strings.Contains(panel, "var(--bg)") {
		t.Errorf("#wspanel must paint the metro-map background var(--bg); got %q", panel)
	}
	if strings.Contains(panel, "var(--surface)") {
		t.Errorf("#wspanel must not use var(--surface) (that differs from the wall map); got %q", panel)
	}
	if strings.Contains(panel, "margin: 3vh auto") {
		t.Error("#wspanel must not be a centred card with a vh margin")
	}
}

// Item 3 (2026-07-30 iPad review): on desktop/landscape the two left zones
// (Bookmarks inbox + Notes list) default to the app pane width (340px, same as
// #cards / #bookmarkspane) so the columns share one rhythm. Phase 2: widths are
// CSS custom props with a 340px fallback when the user has not resized.
func TestWorkspaceZoneWidthsMatchPanes(t *testing.T) {
	css := mustProductionCSSCascade(t)
	inbox := cssBlock(t, css, "#wsinbox { width: var(--wsinbox-w, 340px);")
	if !strings.Contains(inbox, "340px") {
		t.Errorf("#wsinbox default width must fall back to 340px (>=900px); got %q", inbox)
	}
	nav := cssBlock(t, css, "#wsnav { width: var(--wsnav-w, 340px);")
	if !strings.Contains(nav, "340px") {
		t.Errorf("#wsnav default width must fall back to 340px (>=900px); got %q", nav)
	}
	// Resizable dividers are part of the same three-zone desktop contract.
	if !strings.Contains(css, ".wsdivider") {
		t.Error("notes cascade must define .wsdivider for zone resize handles")
	}
	if !strings.Contains(css, "--wsinbox-w") || !strings.Contains(css, "--wsnav-w") {
		t.Error("notes cascade must expose --wsinbox-w / --wsnav-w custom props")
	}
}

// Follow-up (2026-07-30 iPhone): swipes must stay on the top-most layer. The
// document-level app-nav swipe (setLevel / setBookmarksOpen) must bail out while
// a full-screen overlay owns the screen, or a swipe on the workspace moves the
// activities/journeys/chat layers underneath while the workspace sits still.
func TestAppSwipeSuppressedUnderOverlays(t *testing.T) {
	app := mustReadApp(t)
	// the guard must be inside the document-level touchend handler, before nav.
	td := strings.Index(app, `document.addEventListener("touchend"`)
	if td < 0 {
		t.Fatal("could not locate the document-level touchend handler")
	}
	body := app[td:]
	// Packet 6F: overlayOwned is snapshotted at touchstart and fed to
	// documentSwipeDecision; suppress precedes applyNavAction / setLevel.
	// navigation.js encodes the suppress branch; production must call it.
	if !strings.Contains(app, "documentSwipeDecision") {
		t.Error("document touchend must use documentSwipeDecision for overlay-owned suppress")
	}
	if !strings.Contains(app, "overlayOwned") {
		t.Error("document touchend must bail on the overlay snapshot (overlayOwned) while a top overlay owns the gesture")
	}
	// applyNavAction is the only setLevel path from the document swipe.
	guard := strings.Index(body, "documentSwipeDecision")
	nav := strings.Index(body, "applyNavAction")
	if guard < 0 || nav < 0 || guard > nav {
		t.Error("the overlay decision must precede applyNavAction so swipes never move layers underneath")
	}
	// Live recheck of overlays at touchend remains forbidden.
	if strings.Contains(body, "if (wsOpen() || searchOpen() || previewOpen())") {
		t.Error("touchend still re-checks live overlay open state")
	}
}

// Follow-up (2026-07-30 iPhone): the workspace owns its own swipe-back gesture
// (L→R): note editor → notes list → close, mirroring the iOS edge-back. Without
// it the swipe fell through to the app underneath.
func TestWorkspaceHasSwipeBack(t *testing.T) {
	// Packet 7F: swipe-back listeners live in notes.js factory bind().
	notes, err := os.ReadFile(webSourcePath("web/js/notes.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(notes)
	if !strings.Contains(src, `listen(ws, "touchend"`) && !strings.Contains(src, `"touchend", onWorkspaceTouchEnd`) {
		t.Error("the workspace must handle its own touchend swipe-back in notes.js")
	}
	if !strings.Contains(src, "notesSwipeBackDecision") {
		t.Error("workspace swipe-back must use notesSwipeBackDecision")
	}
	// the two back steps: to the notes list (via #wsback) and out of the workspace.
	if !strings.Contains(src, "noteToList") || !strings.Contains(src, "closeWorkspace") {
		t.Error("workspace swipe-back must step note→list then list→closed")
	}
	app := mustReadApp(t)
	if !strings.Contains(app, "notesFeature.bind()") {
		t.Error("production must bind notesFeature as the Notes event owner")
	}
}

// Follow-up (2026-07-30 iPad review): the workspace title ("Notes") must render
// at the same size/weight as the app pane headings ("Activities", "Bookmarks").
func TestWorkspaceTitleMatchesPaneHeadings(t *testing.T) {
	css := mustProductionCSSCascade(t)
	pane := cssBlock(t, css, ".phead h2 {")
	ws := cssBlock(t, css, "#wstopbar h2 {")
	// margin:0 is essential — without it the UA heading margin inflates the bar's
	// height (flex items don't collapse margins), so the header renders taller.
	for _, want := range []string{"font-size: 22px", "font-weight: 700", "margin: 0"} {
		if !strings.Contains(pane, want) {
			t.Fatalf(".phead h2 baseline changed, expected %q; got %q", want, pane)
		}
		if !strings.Contains(ws, want) {
			t.Errorf("#wstopbar h2 must match the pane headings (%q); got %q", want, ws)
		}
	}
}

// Follow-up (2026-07-30 iPad review): the workspace header bar must be the same
// vertical height as the chat area header. Both rows are driven by 34px controls,
// so matching the top/bottom padding (10px, from #chathead) makes them identical.
func TestWorkspaceHeaderHeightMatchesChatHead(t *testing.T) {
	css := mustProductionCSSCascade(t)
	chat := cssBlock(t, css, "#chathead {")
	if !strings.Contains(chat, "padding: 10px 16px 10px") {
		t.Fatalf("#chathead vertical padding baseline changed; got %q", chat)
	}
	ws := cssBlock(t, css, "#wstopbar {")
	if !strings.Contains(ws, "padding: 10px ") {
		t.Errorf("#wstopbar must use 10px top/bottom padding to match #chathead; got %q", ws)
	}
}

// Follow-up (2026-07-30): the section body editor auto-grows to fit its content
// rather than relying on a manual drag handle. Apple HIG: iOS/iPadOS text views
// size to their content (Notes/Messages); CSS `resize` is ignored on iOS, so a
// grabber is not a real control there. It caps at a max-height, then scrolls.
func TestWorkspaceSectionEditorAutogrows(t *testing.T) {
	css := mustProductionCSSCascade(t)
	block := cssBlock(t, css, ".wssecedit {")
	if strings.Contains(block, "resize: vertical") {
		t.Error(".wssecedit must not depend on the manual resize handle (ignored on iOS) — auto-grow instead")
	}
	if !strings.Contains(block, "max-height") {
		t.Error(".wssecedit must cap growth with a max-height and scroll past it")
	}
	// Packet 7F: startBodyEdit lives in notes.js.
	notes, err := os.ReadFile(webSourcePath("web/js/notes.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(notes)
	start := strings.Index(src, "function startBodyEdit(")
	if start < 0 {
		t.Fatal("could not locate startBodyEdit in notes.js")
	}
	if !strings.Contains(src[start:start+800], "scrollHeight") {
		t.Error("startBodyEdit must auto-grow the textarea from its scrollHeight")
	}
}

// Phase 1g: zone-3 header removed; top bar #wstitle is the sole title home.
// Guard against the dead #wsnotehead / #wsnotetitle / #wsnotemenu chrome.
func TestWorkspaceNoteHeadRemoved(t *testing.T) {
	html := mustReadIndex(t)
	for _, id := range []string{"wsnotehead", "wsnotetitle", "wsnotemenu"} {
		if strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html must not contain #%s after Phase 1g", id)
		}
	}
	css := mustProductionCSSCascade(t)
	for _, sel := range []string{"#wsnotehead", "#wsnotetitle", "#wsnotemenu"} {
		if strings.Contains(css, sel) {
			t.Errorf("CSS cascade must not style %s after Phase 1g", sel)
		}
	}
}

// Phase 1h: zone-2 and zone-3 empty states share a vertical centre — both are
// absolute full-column overlays of equal-height sibling columns (#wsnav /
// #wsnote), not one pinned below its header.
func TestWorkspaceEmptyStatesCentered(t *testing.T) {
	css := mustProductionCSSCascade(t)
	for _, sel := range []string{"#wsnoteempty {", "#wscards .empty {"} {
		block := cssBlock(t, css, sel)
		if !strings.Contains(block, "align-items: center") || !strings.Contains(block, "justify-content: center") {
			t.Errorf("%s must centre on both axes (flex align+justify center); got %q", sel, block)
		}
		if !strings.Contains(block, "position: absolute") || !strings.Contains(block, "inset: 0") {
			t.Errorf("%s must be an absolute full-column overlay (position:absolute; inset:0); got %q", sel, block)
		}
	}
	nav := cssBlock(t, css, "#wsnav {")
	note := cssBlock(t, css, "#wsnote {")
	if !strings.Contains(nav, "position: relative") {
		t.Errorf("#wsnav must be the positioned ancestor for the zone-2 empty overlay; got %q", nav)
	}
	if !strings.Contains(note, "position: relative") {
		t.Errorf("#wsnote must be the positioned ancestor for #wsnoteempty; got %q", note)
	}
}

// Items 2 & 5: the left capture zone reuses the compact Notes UI — lane tabs and
// the same auto-collapse ("Show more"/"Show less") for tall bubbles.
func TestWorkspaceInboxHasTabsAndClamp(t *testing.T) {
	html := mustReadIndex(t)
	css := mustProductionCSSCascade(t)
	// Structural roots stay in the document.
	if !strings.Contains(html, `id="wsinboxtabs"`) {
		t.Error(`workspace inbox must keep id="wsinboxtabs" in markup`)
	}
	// Packet 7F: clamp/list logic lives in notes.js.
	notes, err := os.ReadFile(webSourcePath("web/js/notes.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(notes)
	for _, want := range []string{
		"data-wsitab",        // per-tab selector
		"data-wsimore",       // per-bubble show-more toggle
		"applyWsInboxClamps", // the measure/apply pass
		"expandedWsInbox",    // ephemeral expand state, sibling of expandedBookmarks
	} {
		if !strings.Contains(src, want) {
			t.Errorf("workspace inbox tabs/clamp wiring missing %q", want)
		}
	}
	if !strings.Contains(css, ".wsibubble.clamped") {
		t.Error("workspace inbox CSS clamp is missing")
	}
	// The show-more toggle branch must precede the bookmark-action branch so a
	// tap on Show more never enters placement mode.
	dele := strings.Index(src, "function onInboxListClick")
	if dele < 0 {
		t.Fatal("could not locate the inbox list click handler in notes.js")
	}
	body := src[dele:]
	more := strings.Index(body, "[data-wsimore]")
	use := strings.Index(body, "[data-bmact]")
	if more < 0 || use < 0 || more > use {
		t.Error("the data-wsimore toggle branch must precede the data-bmact branch")
	}
}

// Terminology (2026-07-29): the product language is Bookmarks (chat captures)
// and Notes (synthesis docs, formerly Memos), and the code identifiers now
// match — capture code is `bookmark`, synthesis code is `note`. This test
// guards the visible vocabulary; it anchors to rendered markup rather than bare
// words so it doesn't trip over the bottom-sheet `.sheet` homonym in comments.
func TestWorkspaceRenames(t *testing.T) {
	html := mustReadIndex(t)
	// stale visible wording that must not leak. Rendered UI forms only (anchor
	// to tags/aria/labels rather than bare words).
	for _, gone := range []string{
		">Use in sheet<", "No captures yet", "No sheets yet", "Couldn't create sheet",
		"take a note",              // the bubble action is now "bookmark"
		"Use in memo</span>",       // the inbox action label
		`aria-label="use in memo"`, // the note-row action
		"Delete memo</span>", "Couldn't create memo", "Couldn't open memo",
		"Couldn't add to memo", "No memos yet", "Pick a memo,", "New memo",
		`placeholder="Memo title"`, `aria-label="memo title"`, `aria-label="memo actions"`,
		`aria-label="Active memo"`,
		">Add note here</span>", // the held item is now a bookmark
		"<h2>Notes</h2>",        // the captures pane heading is now Bookmarks
	} {
		if strings.Contains(html, gone) {
			t.Errorf("stale visible wording %q must be renamed (Bookmarks/Notes vocabulary)", gone)
		}
	}
	// required new vocabulary
	chatJS, err := os.ReadFile(webSourcePath("web/js/chat.js"))
	if err != nil {
		t.Fatalf("read chat.js: %v", err)
	}
	chatSrc := string(chatJS)
	// bubble action label moved to chat.js (Packet 7C)
	if !strings.Contains(chatSrc, ">bookmark</button>") && !strings.Contains(chatSrc, "bookmark</button>") {
		t.Errorf("expected new vocabulary %q not found in chat.js", ">bookmark</button>")
	}
	notesJS, err := os.ReadFile(webSourcePath("web/js/notes.js"))
	if err != nil {
		t.Fatalf("read notes.js: %v", err)
	}
	notesSrc := string(notesJS)
	corpus := html + "\n" + notesSrc
	for _, want := range []string{
		`<h2 class="panetitle">Bookmarks</h2>`, // captures pane heading
		"Use in note", "Delete note",           // synthesis-doc action + archive confirm
		"Add bookmark here", "cancel placing bookmark",
		"New note", "rename note", "Pick a note, or make one with +",
		"No notes yet — make one with +", // notes (memo) card empty state
	} {
		if !strings.Contains(corpus, want) {
			t.Errorf("expected new vocabulary %q not found", want)
		}
	}
	// a short terminology note must remain, documenting the code==UI mapping and
	// the one live gotcha: the bottom-sheet `.sheet` primitive is NOT a Note.
	// Lives in CSS (now notes.css); read the assembled cascade, not raw HTML.
	css := mustProductionCSSCascade(t)
	if !strings.Contains(css, "TERMINOLOGY (code == UI") {
		t.Error("the terminology comment (code==UI mapping + bottom-sheet homonym warning) must be present")
	}
}

// The captures pane became "Bookmarks": the chat-area toggle uses a ‹/› chevron,
// and the "use in note" action (formerly the pen-to-square that read as "Edit")
// now uses a paperclip, sitting second-to-last in the note action row (before
// delete). Icons are inlined SVG per the repo convention — no FA dependency.
func TestBookmarkIconsAndActionOrder(t *testing.T) {
	html := mustReadIndex(t)
	app := mustReadApp(t)
	bm, err := os.ReadFile(webSourcePath("web/js/bookmarks.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(bm)
	if !strings.Contains(app, "const ICON_CLIP") {
		t.Error(`missing inlined icon constant "const ICON_CLIP"`)
	}
	// the chat-area toggle (#bookmarksbtn) is the ‹/› chevron mirroring pane state
	if strings.Contains(src, "ICON_BOOKMARK") {
		t.Error("the chat-area toggle (#bookmarksbtn) must be a chevron, not ICON_BOOKMARK")
	}
	if !strings.Contains(src, `"&#8249;"`) {
		t.Error("the chat-area toggle (#bookmarksbtn) must restore the ‹ chevron when closed")
	}
	// The Bookmarks pane's forward control opens the Notes pane (to its right in
	// the line), so it names that destination — "Notes ›", the mirror of the
	// Journeys pane's "Activities ›" — instead of a book-bookmark glyph that named
	// the pane you were already on. The dead ICON_BOOKMARK const is removed.
	if !strings.Contains(html, `id="notesbtn" aria-label="show notes" title="Notes">Notes &#8250;</button>`) {
		t.Error(`#notesbtn must read "Notes ›" (forward label + right chevron) to name the pane it reveals`)
	}
	if strings.Contains(html, "ICON_BOOKMARK") || strings.Contains(app, "ICON_BOOKMARK") || strings.Contains(src, "ICON_BOOKMARK") {
		t.Error("ICON_BOOKMARK is dead once #notesbtn is a text label — remove the const and its assignment")
	}
	// #notesbtn must not use .backbtn (it is a forward destination label)
	if strings.Contains(html, `id="notesbtn"`) {
		notesBtn := sliceBetween(t, html, `id="notesbtn"`, "</button>")
		if strings.Contains(notesBtn, "backbtn") {
			t.Error("#notesbtn must not carry .backbtn")
		}
	}
	// in the note action row, the paperclip (use-in-note) sits just before delete
	clip := strings.Index(src, `b("note",`)
	del := strings.Index(src, `b("del",`)
	if clip < 0 || del < 0 {
		t.Fatal("note action row must contain both the use-in-note and delete actions")
	}
	if !(clip < del) {
		t.Error("the use-in-note (paperclip) action must sit before delete (second-to-last)")
	}
	act := src[clip:]
	if end := strings.Index(act, ")"); end > 0 {
		if !strings.Contains(act[:end], "ICON_CLIP") {
			t.Error("the use-in-note action must render ICON_CLIP (paperclip)")
		}
		if strings.Contains(act[:end], "ICON_SHEETS") {
			t.Error("the use-in-note action must no longer render the pen-to-square ICON_SHEETS")
		}
	}
}

// Swipe R->L on the open Bookmarks pane opens the Notes overview (the workspace).
// This is the swipe-forward accelerator; a tappable path (#notesbtn) remains the
// discoverable primary per HIG.
func TestBookmarksSwipeOpensNotesOverview(t *testing.T) {
	app := mustReadApp(t)
	// find the global touchend gesture handler in the composition entry
	i := strings.Index(app, `document.addEventListener("touchend"`)
	if i < 0 {
		t.Fatal("could not locate the touchend gesture handler")
	}
	body := app[i : i+strings.Index(app[i:], "}, { passive: true });")]
	// Packet 6F: openWorkspace is applied via applyNavAction("openWorkspace")
	// when documentSwipeDecision returns that action (Bookmarks R→L).
	if !strings.Contains(body, "documentSwipeDecision") || !strings.Contains(app, `action.type === "openWorkspace"`) {
		t.Error("swipe R->L on the open Bookmarks pane must call openWorkspace() (Notes overview)")
	}
	nav, err := os.ReadFile(webSourcePath("web/js/navigation.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(nav), `type: "openWorkspace"`) {
		t.Error("navigation.js must emit openWorkspace for Bookmarks R→L")
	}
}

// Item 4: the capture list must not scroll horizontally (hard to use on touch);
// long code fences scroll inside their own <pre>, the list itself does not.
func TestWorkspaceInboxNoHorizontalScroll(t *testing.T) {
	css := mustProductionCSSCascade(t)
	block := cssBlock(t, css, "#wsinboxlist {")
	if !strings.Contains(block, "overflow-x: hidden") && !strings.Contains(block, "overflow-x: clip") {
		t.Errorf("#wsinboxlist must suppress horizontal scroll; got %q", block)
	}
}

// Item 6/9: Note cards reorder by drag-and-drop (the proven pinned-card gesture),
// not by no-op up/down arrows; the redundant reorder-mode toggle is gone.
func TestWorkspaceNoteCardsDragReorder(t *testing.T) {
	html := mustReadIndex(t)
	notes, err := os.ReadFile(webSourcePath("web/js/notes.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(notes)
	for _, gone := range []string{
		"data-wsmove",    // the old per-card up/down buttons
		`id="wsreorder"`, // the old reorder-mode toggle
		"function wsMove(",
	} {
		if strings.Contains(html, gone) || strings.Contains(src, gone) {
			t.Errorf("obsolete reorder affordance %q must be removed", gone)
		}
	}
	if !strings.Contains(src, `"dragstart"`) || !strings.Contains(src, "onCardsDragStart") {
		t.Error("Note cards must be reorderable by drag (dragstart handler in notes.js)")
	}
	// The card template must be draggable.
	if !strings.Contains(src, `draggable="true"`) {
		t.Error("the .wscard template must set draggable=\"true\"")
	}
}

// Item 10: section bodies, embedded references, and capture bubbles render
// Markdown with the same element styling the chat bubbles use — headings, lists,
// code, and quotes must be styled, not flat text.
func TestWorkspaceMarkdownStyled(t *testing.T) {
	css := mustProductionCSSCascade(t)
	// A grouped selector must style headings/lists inside the workspace md
	// containers (any of the three qualifies the rule).
	for _, want := range []string{
		".wssecrender h1",
		".wssecrender ul",
		".wssecrender pre",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("workspace markdown styling missing selector %q", want)
		}
	}
}

// Items 12 & 13: a section already at the top cannot move up, one at the bottom
// cannot move down — no wrap-around. sectionSwapPlan is the pure decision core,
// operating on the visually sorted order (not array position).
func TestWorkspaceReferenceClamp(t *testing.T) {
	css := mustProductionCSSCascade(t)
	notes, err := os.ReadFile(webSourcePath("web/js/notes.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(notes)
	for _, want := range []string{
		"data-refmore",
		"function applyRefClamps(",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("embedded-reference clamp wiring missing %q", want)
		}
	}
	if !strings.Contains(css, ".wsrefbody.clamped") {
		t.Error("workspace reference CSS clamp is missing")
	}
}

// Item 15: after editing a section's body, its embedded references must still
// render. The rendered-body rebuild must go through a helper that re-emits the
// references, not overwrite the body with the prose alone.
func TestWorkspaceWideLayout(t *testing.T) {
	css := mustProductionCSSCascade(t)
	if !strings.Contains(css, "min-width: 768px") {
		t.Error("multi-zone workspace breakpoint must reach iPad portrait (min-width: 768px)")
	}
	// #wsback is hidden in the wide layout (the X already closes the workspace).
	wide := css[strings.Index(css, "@media (min-width: 768px)"):]
	wide = wide[:strings.Index(wide, "\n}\n")+2]
	if !strings.Contains(wide, "#wsback") {
		t.Error("the wide layout must hide #wsback")
	}
}

// Review finding 2: Escape (revert) could lose a race to an already-started
// autosave — if the earlier edit's PATCH reached the server after the revert
// PATCH, the cancelled value would persist. The fix serializes every save for a
// given field: each PATCH is chained after the previous one for the same key, so
// the server always applies edits in issue order and a revert can never be
// overtaken. Behavioral coverage lives in web/test/notes.test.js (Packet 7F).
func sliceBetween(t *testing.T, html, start, end string) string {
	t.Helper()
	i := strings.Index(html, start)
	if i < 0 {
		t.Fatalf("could not locate %q", start)
	}
	rest := html[i:]
	j := strings.Index(rest, end)
	if j < 0 {
		t.Fatalf("could not locate %q after %q", end, start)
	}
	return rest[:j]
}

// iPhone feedback item 3: when a note has no sections, the "No sections yet."
// label duplicates the button beneath it. Drop the label and present just the
// Add section button, centered vertically and horizontally in the pane.
func TestWorkspaceEmptySectionStateIsCenteredButtonOnly(t *testing.T) {
	html := mustReadIndex(t)
	notes, err := os.ReadFile(webSourcePath("web/js/notes.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(notes)
	// the redundant caption is gone
	if strings.Contains(html, "No sections yet.") || strings.Contains(src, "No sections yet.") {
		t.Error(`the empty-section caption "No sections yet." must be dropped — it duplicates the Add section button`)
	}
	// Packet 7F: empty state HTML is rendered from notes.js.
	esi := strings.Index(src, "class=\"wssecempty\"")
	if esi < 0 {
		// template may use single-quoted class in a template literal class name only
		esi = strings.Index(src, "wssecempty")
	}
	if esi < 0 {
		t.Fatal("empty section state must render a .wssecempty centering wrapper")
	}
	// the wrapper must contain the add-section button (so wsAddSection still fires)
	wrap := src[esi:]
	if end := strings.Index(wrap, "Add section"); end < 0 || !strings.Contains(wrap[:end+20], "data-addsection") {
		t.Error(".wssecempty must contain the data-addsection button")
	}
	// the wrapper centers its button both axes and fills the pane so it sits
	// vertically centered, not pinned to the top
	css := mustProductionCSSCascade(t)
	block := cssBlock(t, css, ".wssecempty {")
	for _, want := range []string{"align-items: center", "justify-content: center"} {
		if !strings.Contains(block, want) {
			t.Errorf(".wssecempty must center its content (%s); got %q", want, block)
		}
	}
	if !strings.Contains(block, "min-height") {
		t.Errorf(".wssecempty must claim the pane height so the button centers vertically; got %q", block)
	}
}

// mustReadIndex returns the production index HTML. Use it for DOM markup,
// ARIA, stylesheet, and script-tag contracts only. Implementation JavaScript
// lives in web/js/app.js (mustReadApp) or the owning feature module.
// CSS declaration/media-query assertions must search the assembled cascade
// (mustProductionCSSCascade / mustCSSCascade) instead of assuming all CSS is
// inline in the HTML argument.
func mustReadIndex(t *testing.T) string {
	t.Helper()
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	return string(b)
}

// mustReadApp returns the sole browser composition entry (web/js/app.js).
// Prefer this for feature wiring, document listeners, boot order, and other
// implementation contracts that previously searched the inline module in index.
func mustReadApp(t *testing.T) string {
	t.Helper()
	b, err := webFS.ReadFile("web/js/app.js")
	if err != nil {
		t.Fatalf("read embedded web/js/app.js: %v", err)
	}
	return string(b)
}

// cssBlock returns the text of the first CSS rule whose selector line contains
// marker, from that line to the closing brace. cascade must be the assembled
// document CSS (inline <style> plus linked stylesheets in document order), not
// raw index HTML.
func cssBlock(t *testing.T, cascade, marker string) string {
	t.Helper()
	i := strings.Index(cascade, marker)
	if i < 0 {
		t.Fatalf("could not locate CSS rule %q", marker)
	}
	end := strings.Index(cascade[i:], "}")
	if end < 0 {
		t.Fatalf("unterminated CSS rule %q", marker)
	}
	return cascade[i : i+end]
}

// --- Pane gaps (pane-gap-design.md) ------------------------------------------

// P1: the peek is a deliberate token, not the leftover 14% of #cards' old
// min(86%,380px) sidebar width. It must clear the 44pt tap target on the
// narrowest phones (10% would not) yet stay capped so it never wastes width on
// large screens. Each phone pane is full-width-minus-peek, anchored to its home
// edge: left-side panes (Activities, Journeys) gap right; right-side panes
// (Bookmarks) gap left. Gaps must not pile up — every pane keeps its own
// absolute width, so the peek is uniform at any depth.
func TestPanePeekGapToken(t *testing.T) {
	css := mustProductionCSSCascade(t)

	if !strings.Contains(css, "--peek:") {
		t.Fatal("no --peek token defined; the peek width must be a deliberate token")
	}
	root := cssBlock(t, css, ":root {")
	// tolerate the block's column alignment (--peek:<pad>clamp(...)).
	if !strings.Contains(root, "--peek:") || !strings.Contains(root, "clamp(44px, 12%, 60px);") {
		t.Error("--peek must be clamp(44px, 12%, 60px): 44px floor keeps it tappable on the narrowest phone, 60px cap stops it wasting width on iPad/Pro Max")
	}

	// Activities: full-width-minus-peek, still left-anchored (gap on the right).
	cards := cssBlock(t, css, "#cards {")
	if !strings.Contains(cards, "width: calc(100% - var(--peek));") {
		t.Error("#cards should be width: calc(100% - var(--peek))")
	}
	if strings.Contains(cards, "min(86%, 380px)") {
		t.Error("#cards still carries the leftover min(86%,380px) sidebar width")
	}
	if !strings.Contains(cards, "left: 0;") {
		t.Error("#cards must stay left-anchored so its peek falls on the right")
	}

	// Journeys: was full-width (no gap); now gets the same peek, left-anchored.
	mp := cssBlock(t, css, "#map {")
	if !strings.Contains(mp, "width: calc(100% - var(--peek));") {
		t.Error("#map should be width: calc(100% - var(--peek)) (it was width:100% — no gap)")
	}
	if !strings.Contains(mp, "left: 0;") {
		t.Error("#map must stay left-anchored so its peek falls on the right")
	}

	// Bookmarks: was full-width (no gap); now peek on the LEFT, right-anchored.
	bm := cssBlock(t, css, "#bookmarkspane {")
	if !strings.Contains(bm, "width: calc(100% - var(--peek));") {
		t.Error("#bookmarkspane should be width: calc(100% - var(--peek)) (it was width:100% — no gap)")
	}
	if !strings.Contains(bm, "right: 0;") {
		t.Error("#bookmarkspane must stay right-anchored so its peek falls on the left")
	}
}

// P2: the right side is a spatial line — Chat · Bookmarks · Notes. Leaving Notes
// (L→R) must land on Bookmarks, not overshoot to Chat. The overshoot was a
// double-consumed gesture: the #notesworkspace touchend listener closes the
// workspace first (bubbling), flipping wsOpen() to false, so the document
// touchend guard — which re-checked wsOpen() live — no longer bailed and then
// processed the SAME L→R swipe as "Bookmarks → back → Chat". Fix: snapshot the
// overlay-open state at touchstart, and bail on that snapshot, so a gesture that
// began over an overlay can never be re-processed by the app-nav handler after
// the overlay's own handler consumes it.
func TestAppNavSwipeSnapshotsOverlayAtStart(t *testing.T) {
	app := mustReadApp(t)

	// touchstart must capture whether an overlay owned the gesture at its START
	// (immutable overlayOwned via captureTouchStart).
	if !strings.Contains(app, "captureTouchStart") {
		t.Error("touchstart must use captureTouchStart for the immutable overlay snapshot")
	}
	if !strings.Contains(app, "overlayOwned: wsOpen() || searchOpen() || previewOpen()") {
		t.Error("touchstart must snapshot overlay-open state (overlayOwned: wsOpen() || searchOpen() || previewOpen())")
	}
	// touchend consults documentSwipeDecision's suppress on that snapshot only.
	if !strings.Contains(app, "documentSwipeDecision") {
		t.Error("touchend must consult documentSwipeDecision with the touchstart snapshot")
	}
	if strings.Contains(app, "if (wsOpen() || searchOpen() || previewOpen()){ touch = null; return; }") {
		t.Error("touchend still re-checks live wsOpen() — racy: the overlay handler flips it before this bubbles, causing the Notes→L→R overshoot to Chat")
	}
	nav, err := os.ReadFile(webSourcePath("web/js/navigation.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(nav), `if (touch.overlayOwned) return { type: "suppress" }`) {
		t.Error("navigation.js must suppress app nav when overlayOwned is true")
	}
}

// P3: the exposed peek strip is a control — tapping it steps one pane back,
// mirroring the swipe (HIG: an exposed parent is tappable). Left panes reuse the
// scrim (now shown for any open left pane, stepping ONE level back, not jumping
// to level 1); the right pane (Bookmarks) gets a left-strip tap-catcher.
func TestPeekTapStepsBack(t *testing.T) {
	html := mustReadIndex(t)
	app := mustReadApp(t)
	css := mustProductionCSSCascade(t)

	// Left: scrim covers the peek for both open levels and steps exactly one back
	// via scrimStep → applyNavAction (never a jump to level 1).
	if !strings.Contains(app, `$("#scrim").classList.toggle("on", level >= 2);`) {
		t.Error("scrim must show for any open left pane (level >= 2), so the peek is tappable at level 3 too")
	}
	if !strings.Contains(app, `$("#scrim").addEventListener("click", () => applyNavAction(scrimStep(level)));`) {
		t.Error("scrim tap must step ONE level back via scrimStep, mirroring the swipe — not jump to level 1")
	}
	if strings.Contains(app, `$("#scrim").addEventListener("click", () => setLevel(1));`) {
		t.Error("scrim tap still jumps straight to level 1 instead of stepping one back")
	}
	nav, err := os.ReadFile(webSourcePath("web/js/navigation.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(nav), "export function scrimStep") ||
		!strings.Contains(string(nav), "clampLevel(level - 1)") {
		t.Error("navigation.js scrimStep must decrement one level")
	}

	// Right: a dedicated catcher over the Bookmarks left peek, closing bookmarks.
	if !strings.Contains(html, `id="bookmarkpeek"`) {
		t.Error("a #bookmarkpeek tap-catcher must exist over the Bookmarks left peek strip")
	}
	// Packet 7E: exactly one owner — bookmarksFeature.bind listens on #bookmarkpeek.
	if !strings.Contains(app, "bookmarksFeature.bind()") {
		t.Error("bookmarksFeature must bind as the sole #bookmarkpeek event owner")
	}
	if strings.Contains(app, `$("#bookmarkpeek").addEventListener`) {
		t.Error("app.js must not also attach a #bookmarkpeek listener (single-owner rule)")
	}
	bm, err := os.ReadFile(webSourcePath("web/js/bookmarks.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bm), "listen(roots.bookmarkpeek") &&
		!strings.Contains(string(bm), "listen(roots.bookmarkpeek,") {
		// accept either form
		if !strings.Contains(string(bm), "bookmarkpeek") || !strings.Contains(string(bm), "onPeekClick") {
			t.Error("bookmarks.js must own the #bookmarkpeek click handler")
		}
	}
	if !strings.Contains(string(nav), `type: "setBookmarksOpen", open: false`) {
		t.Error("bookmarkPeekClose must emit setBookmarksOpen false")
	}
	// It occupies only the peek strip, appears with the pane, and never on desktop.
	peek := cssBlock(t, css, "#bookmarkpeek {")
	if !strings.Contains(peek, "width: var(--peek);") || !strings.Contains(peek, "left: 0;") {
		t.Error("#bookmarkpeek must be a left-anchored strip exactly one peek wide")
	}
	if !strings.Contains(css, "body.bookmarks-open #bookmarkpeek { display: block; }") {
		t.Error("#bookmarkpeek must appear only while the bookmarks pane is open")
	}
	if !strings.Contains(css, "#bookmarkpeek, body.bookmarks-open #bookmarkpeek { display: none; }") {
		t.Error("#bookmarkpeek is phone/tablet-only: the exposed Bookmarks peek must stay unavailable on desktop")
	}
}

// P4: the back-label must point at its destination (HIG reversibility). Journeys
// is the leftmost pane; its parent Activities sits to the RIGHT and the return
// gesture is R→L. So the label reads "Activities ›" right-aligned, not the old
// left-pointing "‹ Activities". The Journeys title moves to the left edge.
// (#chatback's "‹ Activities" stays: Activities really is left of chat.)
func TestJourneyBackLabelPointsRight(t *testing.T) {
	html := mustReadIndex(t)

	mapStart := strings.Index(html, `<section id="map"`)
	if mapStart < 0 {
		t.Fatal("could not locate the #map (Journeys) section")
	}
	phead := html[mapStart : mapStart+strings.Index(html[mapStart:], "</div>")]

	if !strings.Contains(phead, `id="mapclose">Activities &#8250;`) {
		t.Error(`Journeys back-label must read "Activities ›" (right chevron &#8250;) to point at its right-side parent`)
	}
	if strings.Contains(phead, `&#8249; Activities</button>`) {
		t.Error(`Journeys back-label still reads "‹ Activities" — the left chevron contradicts the R→L return gesture`)
	}
	// Title left, back control right: Journeys h2 precedes the spacer, which
	// precedes the (now right-aligned) back button.
	title := strings.Index(phead, `>Journeys</h2>`)
	spacer := strings.Index(phead, `class="spacer"`)
	back := strings.Index(phead, `id="mapclose"`)
	if !(title >= 0 && spacer >= 0 && back >= 0 && title < spacer && spacer < back) {
		t.Errorf("Journeys header order must be title → spacer → back (got title=%d spacer=%d back=%d)", title, spacer, back)
	}
}

// P5: honour prefers-reduced-motion on the pane slides (the three sliding panes
// animate transform; nothing zeroed them), and guard that the selected-note
// detail stays a full-cover modal — the one leaf that takes NO peek.
func TestReducedMotionAndModalNoPeek(t *testing.T) {
	css := mustProductionCSSCascade(t)

	// The sliding panes must not animate under reduced motion.
	if !strings.Contains(css, "#cards, #map, #bookmarkspane { transition: none; }") {
		t.Error("prefers-reduced-motion must zero the #cards/#map/#bookmarkspane slide transitions")
	}
	rm := strings.LastIndex(css, "@media (prefers-reduced-motion: reduce)")
	// (presence check above is enough; ensure at least one such block exists)
	if rm < 0 {
		t.Error("no prefers-reduced-motion block found")
	}

	// The selected-note detail host full-covers and never carries a peek — it is a
	// modal push, not a member of the spatial line.
	ws := cssBlock(t, css, "#notesworkspace {")
	if !strings.Contains(ws, "left: 0; right: 0;") {
		t.Error("#notesworkspace (selected-note modal) must full-cover (left:0; right:0)")
	}
	if strings.Contains(ws, "var(--peek)") {
		t.Error("the selected-note modal must take NO peek — it is a full-cover leaf, not a line pane")
	}
}
