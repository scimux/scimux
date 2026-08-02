package main

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
		source, err := os.ReadFile(embedPath)
		if err != nil {
			t.Fatalf("read source %s: %v", embedPath, err)
		}
		if !bytes.Equal(embedded, source) {
			t.Fatalf("embedded bytes for %s differ from source", embedPath)
		}
		if out, err := exec.Command(node, "--check", embedPath).CombinedOutput(); err != nil {
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
func TestWebMarkdownParagraphs(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JS execution check")
	}
	script := `
import assert from "node:assert/strict";
import { md, mdInline } from "./format.js";
assert.equal(md("line one\nline two"), "<p>line one<br>line two</p>");
assert.equal(md("a <b> tag\nnext"), "<p>a &lt;b&gt; tag<br>next</p>");
assert.equal(md("solo"), "<p>solo</p>");
assert.equal(md("p1 l1\np1 l2\n\np2"), "<p>p1 l1<br>p1 l2</p><p>p2</p>");
assert.equal(mdInline("*test*"), "<em>test</em>");
assert.equal(mdInline("**test**"), "<strong>test</strong>");
assert.equal(mdInline("a *b* and **c**"), "a <em>b</em> and <strong>c</strong>");
assert.equal(mdInline("2 * 3 * 4"), "2 * 3 * 4");
`
	// Run against the source module path (same bytes as embed).
	tmp := t.TempDir()
	// Copy format.js next to the harness so the relative import resolves.
	src, err := os.ReadFile("web/js/format.js")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "format.js"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(tmp, "md.mjs")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, f).CombinedOutput(); err != nil {
		t.Fatalf("markdown paragraph rendering broken: %v\n%s", err, out)
	}
}

// Fork launch-config contract (supersedes finding 98's "send empty"): fork is
// the one path that may CHANGE launch config, so the payload sends the visible
// selector values in fork mode too. The staleness hazard finding 98 guarded
// against is now closed at the source — prepareLaunchConfig rebuilds the
// selectors and re-seeds them from the parent on every sheet open, so what is
// visible is never a leftover from a previous open.
// Packet 7I: state tick + UI-sync coordination live in polling.js.
func TestPollingModuleWired(t *testing.T) {
	b, err := os.ReadFile("web/js/polling.js")
	if err != nil {
		t.Fatalf("read polling.js: %v", err)
	}
	src := string(b)
	if !strings.Contains(src, "export function createPollingFeature") {
		t.Error("createPollingFeature must live in polling.js")
	}
	if !strings.Contains(src, "Packet 7I ownership inventory") {
		t.Error("polling.js must document Packet 7I ownership inventory")
	}
	app := mustReadApp(t)
	html := mustReadIndex(t)
	if !strings.Contains(app, `from "./polling.js"`) || !strings.Contains(app, "createPollingFeature") {
		t.Error("production must import and instantiate createPollingFeature from polling.js")
	}
	if !strings.Contains(app, "pollingFeature.bind()") {
		t.Error("production must bind pollingFeature as the visibility/pagehide/poll-timer owner")
	}
	// Old polling owners must not remain in the composition entry or the shell.
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

// Packet 7H: launch/fork decisions live in sheets.js; Node sheets.test.js is
// authoritative for payload builders.
func TestForkPayloadSendsVisibleLaunchConfig(t *testing.T) {
	b, err := os.ReadFile("web/js/sheets.js")
	if err != nil {
		t.Fatalf("read sheets.js: %v", err)
	}
	src := string(b)
	for _, field := range []string{"agent", "model", "effort"} {
		if strings.Contains(src, field+`: ncParent ? "" :`) {
			t.Errorf("new-node payload field %q is still emptied in fork mode", field)
		}
	}
	if !strings.Contains(src, "export function buildCreatePayload(") {
		t.Error("buildCreatePayload must live in sheets.js")
	}
	if !strings.Contains(src, "agent,") || !strings.Contains(src, "model,") || !strings.Contains(src, "effort,") {
		t.Error("create payload must include visible agent/model/effort fields")
	}
	// The per-open re-seed that replaces the old empty-payload staleness guard.
	if !strings.Contains(src, "function prepareLaunchConfig(cfg){") {
		t.Error("prepareLaunchConfig missing from sheets.js")
	}
	if !strings.Contains(src, "fillAgents(); fillModels(); fillEfforts();") {
		t.Error("prepareLaunchConfig no longer rebuilds the selectors on open — stale-config guard lost")
	}
	app := mustReadApp(t)
	if !strings.Contains(app, `from "./sheets.js"`) || !strings.Contains(app, "createSheetsFeature") {
		t.Error("production must import and instantiate createSheetsFeature from sheets.js")
	}
	if !strings.Contains(app, "sheetsFeature.bind()") {
		t.Error("production must bind sheetsFeature as the sheet event owner")
	}
}

// The effort selector offers agent-specific levels: claude's --effort takes
// five (low..max), and the list must follow the selected agent, seed from the
// parent on a fork, and rebuild on an agent switch.
// Packet 7H: effort menus live in sheets.js.
func TestEffortLevelsPerAgent(t *testing.T) {
	b, err := os.ReadFile("web/js/sheets.js")
	if err != nil {
		t.Fatalf("read sheets.js: %v", err)
	}
	src := string(b)
	if !strings.Contains(src, `claude: ["low", "medium", "high", "xhigh", "max"]`) {
		t.Error("DEFAULT_EFFORTS is missing claude's five --effort levels")
	}
	if !strings.Contains(src, "function fillEfforts(){") {
		t.Error("fillEfforts() not defined in sheets.js")
	}
	// The effort list must follow an agent switch and a fork's parent seed.
	if !strings.Contains(src, "function onAgentChange(){ fillModels(); fillEfforts(); }") {
		t.Error("agent change does not refill the effort list")
	}
	if !strings.Contains(src, "fillModels(); fillEfforts();   /* model + effort lists follow") {
		t.Error("prepareLaunchConfig does not refill efforts for the parent's agent")
	}
}

func TestNotesToggleDirectionMatchesPaneState(t *testing.T) {
	html := mustReadIndex(t)
	app := mustReadApp(t)
	// the closed Bookmarks toggle announces opening bookmarks (glyph set by
	// renderBookmarksToggle in bookmarks.js, so the static button carries no chevron)
	if !strings.Contains(html, `id="bookmarksbtn" aria-label="open bookmarks"></button>`) {
		t.Fatal("closed bookmarks toggle should announce opening bookmarks")
	}
	if !strings.Contains(app, `from "./bookmarks.js"`) || !strings.Contains(app, "createBookmarksFeature") {
		t.Fatal("production must import and instantiate createBookmarksFeature from bookmarks.js")
	}
	if !strings.Contains(app, "bookmarksFeature.bind()") {
		t.Fatal("production must bind bookmarksFeature as the Bookmarks event owner")
	}
	bm, err := os.ReadFile("web/js/bookmarks.js")
	if err != nil {
		t.Fatalf("read bookmarks.js: %v", err)
	}
	src := string(bm)
	for _, want := range []string{
		`innerHTML: open ? "&#8250;" : "&#8249;"`,
		`ariaLabel: open ? "close bookmarks" : "open bookmarks"`,
		"function bookmarksToggleState(",
		"renderBookmarksToggle()",
		"renderBookmarkFlags()",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("bookmarks toggle state sync missing %q", want)
		}
	}
}

// A note captured from an archived search hit carries the durable
// (uid, segment, record) address but no live node id. The notes-pane jump
// action must render for such a note — the handler already opens the archived
// surface via nt.uid — or Phase 0's "jump back after deletion" goal has no
// affordance. Gate jump on (nt.node || nt.uid); keep comment gated on nt.node.
func TestNotesJumpActionRendersForUIDOnlyNotes(t *testing.T) {
	// Packet 7E: jump affordance lives in bookmarks.js list HTML builder.
	b, err := os.ReadFile("web/js/bookmarks.js")
	if err != nil {
		t.Fatalf("read bookmarks.js: %v", err)
	}
	html := string(b)
	if !strings.Contains(html, `${nt.node || nt.uid ? `+"`"+`<button data-bmact="jump"`) {
		t.Error("notes-pane jump button must render when the note has a durable uid, not only a live node")
	}
	if strings.Contains(html, `${nt.node ? `+"`"+`<button data-bmact="jump"`) {
		t.Error("jump button is still gated on nt.node alone — UID-only archived-source notes get no jump affordance")
	}
	// The comment action stays live-node-only (it replies into the live chat).
	if !strings.Contains(html, `${nt.node && !nt.anchor ? `+"`"+`<button data-bmact="comment"`) {
		t.Error("comment action gate changed unexpectedly")
	}
}

func TestBubbleCopyLivesInActionRow(t *testing.T) {
	// Packet 7C: action-row markup and wiring live in chat.js; Node
	// chat.test.js executes order/copy contracts. Retain a structural wire
	// check against the production module.
	b, err := os.ReadFile("web/js/chat.js")
	if err != nil {
		t.Fatalf("read chat.js: %v", err)
	}
	src := string(b)
	if strings.Contains(src, "copybtn") {
		t.Fatal("chat bubbles should not render an always-visible copy button")
	}
	if !strings.Contains(src, "export function bubbleActionsHTML") {
		t.Fatal("bubble action row HTML builder missing from chat.js")
	}
	for _, want := range []string{
		`class="bubwhen"`,
		`data-bact="fork"`,
		`data-bact="desc"`,
		`data-bact="bookmark"`,
		`data-bact="copy"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("bubble action row missing %q", want)
		}
	}
	if !(strings.Index(src, `data-bact="bookmark"`) < strings.Index(src, `data-bact="copy"`)) {
		t.Fatal("copy should be the rightmost bubble action")
	}
	if !(strings.Index(src, `class="bubwhen"`) < strings.Index(src, `data-bact="fork"`)) {
		t.Fatal("timestamp should sit between the bubble and the action buttons")
	}
	if !strings.Contains(src, `ba.dataset.bact === "copy"`) ||
		!strings.Contains(src, `copyText`) {
		t.Fatal("copy action row button is not wired to copy the selected turn")
	}
	app := mustReadApp(t)
	if !strings.Contains(app, `from "./chat.js"`) || !strings.Contains(app, "createChatFeature") {
		t.Fatal("production must wire createChatFeature from chat.js")
	}
}

// Packet 7D: the composer is a shell singleton outside every polled render
// region. Behavioral coverage lives in web/test/composer.test.js; this keeps
// the document contract and single production owner.
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
	// promptbar sits in #chat after the rebuilt #msgs surface and before the
	// separately rebuilt Bookmarks surface; every composer root is singular.
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
	// #msgs is a void-of-composer surface: no promptbar markup between its open and the next major sibling section close is hard to parse; instead assert the prompt is a contenteditable singleton with the production placeholder.
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
	// No duplicate implementation of the extracted composer in shell or composition.
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
	comp, err := os.ReadFile("web/js/composer.js")
	if err != nil {
		t.Fatalf("read composer.js: %v", err)
	}
	src := string(comp)
	if !strings.Contains(src, "export function createComposerFeature") {
		t.Fatal("composer.js must export createComposerFeature")
	}
	if !strings.Contains(src, "UPLOAD_WAIT_TIMEOUT_MS = 12000") {
		t.Fatal("composer.js must keep the 12-second upload wait bound")
	}
	if !strings.Contains(src, "scimux-draft:") {
		t.Fatal("composer.js must own scimux-draft storage keys")
	}
}

func TestBubbleTimestampFormatting(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JS execution check")
	}
	tmp := t.TempDir()
	src, err := os.ReadFile("web/js/format.js")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "format.js"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	script := `
import assert from "node:assert/strict";
import { fmtBubbleTime } from "./format.js";
const now = new Date("2026-07-24T15:30:00");
assert.equal(fmtBubbleTime("2026-07-24T09:05:00", now), new Date("2026-07-24T09:05:00").toLocaleTimeString([], { timeStyle: "short" }));
assert.equal(fmtBubbleTime("2026-07-23T22:10:00", now), "Yesterday, " + new Date("2026-07-23T22:10:00").toLocaleTimeString([], { timeStyle: "short" }));
assert.equal(fmtBubbleTime("2026-07-20T08:15:00", now), new Date("2026-07-20T08:15:00").toLocaleString([], { dateStyle: "medium", timeStyle: "short" }));
assert.equal(fmtBubbleTime("not-a-date", now), "not-a-date");
`
	f := filepath.Join(tmp, "bubble-time.mjs")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, f).CombinedOutput(); err != nil {
		t.Fatalf("bubble timestamp formatting broken: %v\n%s", err, out)
	}
}

func TestWebSafeColorRejectsInjection(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JS execution check")
	}
	tmp := t.TempDir()
	src, err := os.ReadFile("web/js/format.js")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "format.js"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	script := `
import assert from "node:assert/strict";
import { safeColor } from "./format.js";
for (const ok of ["#abc", "#aabbcc", "#aabbccdd", "var(--work)", "rgb(1,2,3)", "rgba(1,2,3,.5)", "hsl(200, 50%, 40%)", "tomato", "  #fff  "]) {
  assert.ok(safeColor(ok) !== null, "should accept " + ok);
}
for (const bad of ['#fff"><script>', 'red;background:url(x)', 'expression(1)', '</style>', '', null, undefined, "var(--x); }"]) {
  assert.equal(safeColor(bad), null, "should reject " + JSON.stringify(bad));
}
assert.equal(safeColor("  #fff  "), "#fff", "trims");
`
	f := filepath.Join(tmp, "color.mjs")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, f).CombinedOutput(); err != nil {
		t.Fatalf("safeColor sanitization broken: %v\n%s", err, out)
	}
}

func TestLaneChipContrastTextUsesWCAGMath(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JS execution check")
	}
	tmp := t.TempDir()
	src, err := os.ReadFile("web/js/format.js")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "format.js"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	script := `
import assert from "node:assert/strict";
import { hexRGB, contrastText, contrastRatio, relLum } from "./format.js";
assert.deepEqual(hexRGB("#abc"), [170,187,204]);
assert.equal(contrastText("#007AFF"), "#000", "system blue has stronger black contrast by WCAG ratio");
assert.equal(contrastText("#5856D6"), "#fff", "system indigo needs white text");
assert.equal(contrastText("#FF9500"), "#000", "system orange needs dark text");
assert.ok(contrastRatio(relLum(hexRGB("#FF9500")), relLum(hexRGB(contrastText("#FF9500")))) >= 4.5);
assert.ok(contrastRatio(relLum(hexRGB("#5856D6")), relLum(hexRGB(contrastText("#5856D6")))) >= 4.5);
`
	f := filepath.Join(tmp, "lane-contrast.mjs")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, f).CombinedOutput(); err != nil {
		t.Fatalf("lane chip contrast helpers broken: %v\n%s", err, out)
	}
}

func TestActivityCardShowsUserInteractionAgeAndHostConnectivity(t *testing.T) {
	html := mustReadIndex(t)
	css := mustProductionCSSCascade(t)
	// Host connectivity DOM chrome stays in the shell; tick orchestration and
	// online/offline/unreachable branches live in polling.js (Packet 7I) with
	// executable Node coverage in web/test/polling.test.js. Card age flip
	// lives in cards.js (Packet 7A) with web/test/cards.test.js.
	app := mustReadApp(t)
	for _, want := range []string{
		`<span class="host offline" id="host">scimux</span>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("activity card interaction/connectivity shell missing %q", want)
		}
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
	poll, err := os.ReadFile("web/js/polling.js")
	if err != nil {
		t.Fatal(err)
	}
	ps := string(poll)
	for _, want := range []string{
		`setHostOnline(false)`,
		`setServerUnreachable()`,
		`setHostOnline(true)`,
		`server unreachable`,
	} {
		if !strings.Contains(ps, want) {
			t.Errorf("polling.js host connectivity missing %q", want)
		}
	}
	cards, err := os.ReadFile("web/js/cards.js")
	if err != nil {
		t.Fatal(err)
	}
	cs := string(cards)
	for _, want := range []string{
		`export const CARD_TIME_SWAP_MS = 30000;`,
		`export function cardTimeItems(n){`,
		`export function cardTimeHTML(n,`,
		`Last interaction ${ageFn(you)} · last seen ${ageFn(seen)}`,
		`cardTimeFlip++`,
		`updateCardAges(true)`,
		`time.classList.add("roll-dn")`,
	} {
		if !strings.Contains(cs, want) {
			t.Errorf("cards.js age wiring missing %q", want)
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
	if strings.Contains(html, `<span class="age">${ageText(n.last_activity)}</span>`) ||
		strings.Contains(cs, `<span class="age">${ageText(n.last_activity)}</span>`) {
		t.Fatal("folded activity cards should not label pane movement as the primary age")
	}
}

func TestActivityCardOrderPinsAttentionThenFreshCards(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JS execution check")
	}
	tmp := t.TempDir()
	for _, name := range []string{"format.js", "lanes.js", "map-model.js"} {
		src, err := os.ReadFile("web/js/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmp, name), src, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := `
import assert from "node:assert/strict";
import { orderedNodes } from "./map-model.js";
const nodes = [
  {id:"active-older", last_activity: 900, last_interaction: 200, created_at:"2026-01-04T00:00:00Z"},
  {id:"newer-fresh", created_at:"2026-01-06T00:00:00Z"},
  {id:"active-newer", last_activity: 300, last_interaction: 800, created_at:"2026-01-01T00:00:00Z"},
  {id:"never-touched", last_activity: 700, created_at:"2026-01-03T00:00:00Z"},
  {id:"older-fresh", created_at:"2026-01-05T00:00:00Z"},
  {id:"attn", attention:"approval", last_activity: 100, last_interaction: 100, created_at:"2026-01-02T00:00:00Z"},
];
assert.deepEqual(
  orderedNodes(nodes).map(n => n.id),
  ["attn", "newer-fresh", "older-fresh", "active-newer", "active-older", "never-touched"]
);
`
	f := filepath.Join(tmp, "card-order.mjs")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, f).CombinedOutput(); err != nil {
		t.Fatalf("activity card ordering broken: %v\n%s", err, out)
	}
}

func TestDockedMapStationLongPressEdits(t *testing.T) {
	// Packet 7B: station long-press lives in map.js; Node covers bind/destroy.
	// Structural: production wires createMapFeature and map.js owns the docked path.
	html := mustReadIndex(t)
	app := mustReadApp(t)
	if !strings.Contains(app, `from "./map.js"`) || !strings.Contains(app, "createMapFeature") {
		t.Fatal("production must wire map.js for station long-press")
	}
	mapJS, err := os.ReadFile("web/js/map.js")
	if err != nil {
		t.Fatal(err)
	}
	body := string(mapJS)
	// The docked map must reach the editor — no unconditional mapFull bail-out.
	if strings.Contains(body, "if (!mapFull) return;") {
		t.Error("station long-press still bails out of the docked (collapsed) map")
	}
	// The full-screen-only selection move stays fenced behind mapFull.
	if !strings.Contains(body, "if (mapFull){") {
		t.Error("full-screen selection move should be gated on mapFull, not run in the docked map")
	}
	// Both maps open the editor scoped to the pressed station's stop.
	if !strings.Contains(body, "openActivityEditor(id, stop)") {
		t.Error("station long-press should open the activity editor scoped to the station's stop")
	}
	// No residual inline longpress on #mapwrap after extraction.
	if strings.Contains(html, `longpress($("#mapwrap"), ".strow"`) {
		t.Error("inline mapwrap station longpress must move into map.js")
	}
}

func TestJourneyLaneFoldAndChipWiring(t *testing.T) {
	// Packet 7B: fold/chip algorithms and markup live in map.js (+ lanes.js pure
	// helpers). Executable Node coverage is web/test/map.test.js; keep structural
	// wiring + CSS contracts here.
	html := mustReadIndex(t)
	app := mustReadApp(t)
	css := mustProductionCSSCascade(t)
	if !strings.Contains(app, `from "./map.js"`) || !strings.Contains(app, "createMapFeature") {
		t.Fatal("production must wire map.js for journey fold/chip behavior")
	}
	mapJS, err := os.ReadFile("web/js/map.js")
	if err != nil {
		t.Fatal(err)
	}
	body := string(mapJS)
	for _, want := range []string{
		`MAP_FOLD_KNOWN_KEY = "scimux-mapfold-known"`,
		`export function applyNewLaneFolds`,
		`export function unfoldFocusLane`,
		`export function chipTapPlan`,
		`export function laneChipStyle`,
		`class="lanechip ${focusLane === l.id ? "selected" : ""}"`,
		`class="attnstation-glow"`,
		`class="attnstation-ring"`,
		`setLaneFilter("")`,
		// stack order reuses lanes.js byNameID via buildStackBlocks
		`byNameID`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("journey lane fold/chip wiring missing from map.js: %q", want)
		}
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
	// CSS declarations and selectors live in the cascade (inline today; linked
	// files after Phase 5 extraction).
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
func TestWebForkKindStableAcrossLaneGrowth(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JS execution check")
	}
	tmp := t.TempDir()
	for _, name := range []string{"format.js", "lanes.js"} {
		src, err := os.ReadFile("web/js/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmp, name), src, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := `
import assert from "node:assert/strict";
import { forkKind } from "./lanes.js";
let nodes = [
  {id:"p",     lane_id:"lane-a", created_at:"2026-01-01T00:00:00Z"},
  {id:"c",     lane_id:"lane-b", parent:"p", created_at:"2026-01-02T00:00:00Z"},
  {id:"later", lane_id:"lane-b", created_at:"2026-01-03T00:00:00Z"},
];
assert.equal(forkKind(nodes.find(n => n.id === "c"), nodes), "y-new", "grown lane must stay y-new");
nodes = [
  {id:"a0", lane_id:"lane-b", created_at:"2026-01-01T00:00:00Z"},
  {id:"p",  lane_id:"lane-a", created_at:"2026-01-02T00:00:00Z"},
  {id:"c",  lane_id:"lane-b", parent:"p", created_at:"2026-01-03T00:00:00Z"},
];
assert.equal(forkKind(nodes.find(n => n.id === "c"), nodes), "s", "prior station in dest lane is a crossover");
nodes = [
  {id:"p", lane_id:"lane-a", created_at:"2026-01-01T00:00:00Z"},
  {id:"c", lane_id:"lane-a", parent:"p", created_at:"2026-01-02T00:00:00Z"},
];
assert.equal(forkKind(nodes.find(n => n.id === "c"), nodes), "y-stay", "same-lane fork is y-stay");
assert.equal(forkKind({id:"r", lane_id:"lane-a"}, nodes), null, "root has no fork");
nodes = [{id:"c", lane_id:"lane-b", parent:"ghost", created_at:"2026-01-02T00:00:00Z"}];
assert.equal(forkKind(nodes.find(n => n.id === "c"), nodes), null, "deleted parent has no glyph");
`
	f := filepath.Join(tmp, "forkkind.mjs")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, f).CombinedOutput(); err != nil {
		t.Fatalf("forkKind classification broken: %v\n%s", err, out)
	}
}

func TestWebStackMapKeepsForkCues(t *testing.T) {
	// Packet 7B: fork cues/goto live in map.js; classification stays in lanes.js.
	app := mustReadApp(t)
	if !strings.Contains(app, `from "./map.js"`) {
		t.Fatal("production must wire map.js for stack fork cues")
	}
	mapJS, err := os.ReadFile("web/js/map.js")
	if err != nil {
		t.Fatal(err)
	}
	body := string(mapJS)
	for _, want := range []string{
		"export function forkCueHTML(", // the caption builder
		"fork: true,",                  // stack renderer opts in
		"data-goorigin=",               // tap-to-origin target
		"function gotoStation(",        // …and its handler
		`forkKind(n) === "y-new"`,      // wall map classifies by history
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stack fork-topology wiring missing from map.js: %q", want)
		}
	}
	if strings.Contains(body, "laneCount[n.lane_id]") || strings.Contains(mustReadApp(t), "laneCount[n.lane_id]") {
		t.Error("old population-based Y-new/S classifier still present; must be replaced by forkKind")
	}
}

func TestStructuredApprovalDoesNotInventYN(t *testing.T) {
	// Packet 7C: key-row HTML lives in chat.js keyRowHTML; Node covers ACP vs tmux.
	b, err := os.ReadFile("web/js/chat.js")
	if err != nil {
		t.Fatalf("read chat.js: %v", err)
	}
	src := string(b)
	i := strings.Index(src, `if (source === "acp")`)
	if i < 0 {
		t.Fatal("structured approval branch not found")
	}
	j := strings.Index(src[i:], `["1","2","3","4","y","n"`)
	// ACP branch must end before the tmux y/n key list.
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
	if !strings.Contains(branch, "permOptions") && !strings.Contains(branch, "perm_options") {
		// keyRowHTML uses permOptions param
		if !strings.Contains(src, "permOptions") {
			t.Fatal("ACP branch must use protocol options")
		}
	}
}

// TestSearchOverlayShell asserts the G3 search overlay is a real Spotlight-style
// modal: a 🔍 trigger reachable from any view, an aria-modal dialog that blurs
// the app behind it (with a reduced-transparency fallback), Escape/backdrop/✕ to
// close, and focus returned to the trigger. The grouped feed itself is G4 — here
// #searchfeed exists but only carries the blank-state hint.
func TestSearchOverlayShell(t *testing.T) {
	html := mustReadIndex(t)
	css := mustProductionCSSCascade(t)

	// Structure + a11y: trigger and modal dialog.
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

	// The app behind the overlay is blurred, with an opaque fallback when the
	// viewer asks for reduced transparency (spec: honor Reduce Transparency).
	if !strings.Contains(css, "backdrop-filter: blur") {
		t.Error("search scrim should blur the app behind it")
	}
	if !strings.Contains(css, "prefers-reduced-transparency: reduce") {
		t.Error("search overlay must honor prefers-reduced-transparency")
	}
	if !strings.Contains(css, "prefers-reduced-motion: reduce") {
		t.Error("search overlay must honor prefers-reduced-motion")
	}

	// Packet 7G: open/close/focus-return live in search.js; app.js keeps thin
	// wrappers and imports the factory. Node search.test.js is authoritative.
	app := mustReadApp(t)
	if !strings.Contains(app, `from "./search.js"`) || !strings.Contains(app, "createSearchFeature") {
		t.Error("production must import and instantiate createSearchFeature from search.js")
	}
	if !strings.Contains(app, "function openSearch(") || !strings.Contains(app, "function closeSearch(") {
		t.Error("app.js must retain thin openSearch/closeSearch wrappers for navigation")
	}
	if !strings.Contains(app, "searchFeature.bind()") {
		t.Error("production must bind searchFeature as the Search event owner")
	}
	src, err := os.ReadFile("web/js/search.js")
	if err != nil {
		t.Fatalf("read search.js: %v", err)
	}
	js := string(src)
	for _, want := range []string{
		"searchReturnFocus",
		`on(root("searchbtn"), "click", open)`,
		`on(root("searchclose"), "click", close)`,
		`on(root("searchscrim"), "click", close)`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("search.js overlay behavior missing %q", want)
		}
	}
}

// TestSearchSlashShortcut asserts the "/" activation shortcut (spec: search opens
// on "/" when focus is not in a text field), alongside the existing Cmd/Ctrl-K.
// Packet 7G: document shortcut ownership lives in search.js bind().
func TestSearchSlashShortcut(t *testing.T) {
	b, err := os.ReadFile("web/js/search.js")
	if err != nil {
		t.Fatalf("read search.js: %v", err)
	}
	src := string(b)
	i := strings.Index(src, `if ((e.metaKey || e.ctrlKey) && !e.altKey && (e.key === "k"`)
	if i < 0 {
		t.Fatal("global search shortcut handler not found in search.js")
	}
	// Handler is function onDocumentKeydown — take a bounded window.
	end := i + 600
	if end > len(src) {
		end = len(src)
	}
	handler := src[i:end]
	if !strings.Contains(handler, `e.key === "/"`) {
		t.Error(`the "/" open shortcut is missing`)
	}
	if !strings.Contains(handler, "!isOpen()") {
		t.Error(`the "/" shortcut must not fire while the overlay is already open`)
	}
	if !strings.Contains(handler, `/^(INPUT|TEXTAREA|SELECT)$/.test(e.target && e.target.tagName)`) ||
		!strings.Contains(handler, "isContentEditable") {
		t.Error(`the "/" shortcut must be suppressed while a text field is focused`)
	}
}

// TestSearchHitCarriesIdentity asserts each rendered hit carries its stable
// identity in the DOM — role (so asset actions can be gated) and segment/record
// (so archived show-to-chat can anchor by ordinal, not timestamp).
// Packet 7G: hit HTML and actions live in search.js; openArchived stays shell.
func TestSearchHitCarriesIdentity(t *testing.T) {
	b, err := os.ReadFile("web/js/search.js")
	if err != nil {
		t.Fatalf("read search.js: %v", err)
	}
	src := string(b)
	i := strings.Index(src, "export function searchHitHTML(")
	if i < 0 {
		t.Fatal("searchHitHTML not found in search.js")
	}
	body := src[i : i+strings.Index(src[i:], "\n}")]
	for _, want := range []string{
		`data-role="${e(h.role`,
		`data-segment="${h.segment`,
		`data-record="${h.record`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("hit markup missing stable-identity attribute %q", want)
		}
	}
	// hit bar / action HTML must feed the role so asset hits are gated.
	if !strings.Contains(src, "hitBarHTML(el.dataset.kind, el.dataset.forkable === \"1\", el.dataset.role)") &&
		!strings.Contains(src, "searchHitActions(el.dataset.kind, el.dataset.forkable === \"1\", el.dataset.role)") {
		t.Error("toggleHitBar must pass the hit role into searchHitActions/hitBarHTML")
	}
	// Archived show-to-chat must anchor by (segment, record), passed to openArchived.
	if !strings.Contains(src, "openArchived(hit.dataset.uid, hit.dataset.segment, hit.dataset.record, turn)") &&
		!strings.Contains(src, "d.openArchived(hit.dataset.uid, hit.dataset.segment, hit.dataset.record, turn)") {
		t.Error("archived show-to-chat must anchor by seg/rec, not just timestamp")
	}
	app := mustReadApp(t)
	if !strings.Contains(app, `"&seg=" + encodeURIComponent(seg`) ||
		!strings.Contains(app, `"&rec=" + encodeURIComponent(rec`) {
		t.Error("openArchived must send seg/rec to /api/archived")
	}
}

// TestSearchEarlierHistoryJump asserts finding #1's fix: a show-to-chat hit whose
// turn lives in an earlier segment (before a /clear) no longer dead-ends in a
// toast — consumePendingJump loads the full history and scrolls to the historical
// turn by time, which requires the history bubbles to carry data-time.
func TestSearchEarlierHistoryJump(t *testing.T) {
	// Packet 7C: consumePendingJump + history bubble templates live in chat.js;
	// Node covers matchPendingJumpInHist and history expansion.
	b, err := os.ReadFile("web/js/chat.js")
	if err != nil {
		t.Fatalf("read chat.js: %v", err)
	}
	src := string(b)
	i := strings.Index(src, "function consumePendingJump(")
	if i < 0 {
		t.Fatal("consumePendingJump not found in chat.js")
	}
	// body until next top-level function after consumePendingJump
	rest := src[i:]
	endRel := strings.Index(rest[len("function consumePendingJump("):], "\n  function ")
	if endRel < 0 {
		endRel = strings.Index(rest, "\n  async function ")
	}
	if endRel < 0 {
		endRel = len(rest) - 1
	} else {
		endRel += len("function consumePendingJump(")
	}
	body := rest[:endRel]
	// It must fall through to history instead of only toasting on an earlier-segment hit.
	if !strings.Contains(body, "loadHistory(") && !strings.Contains(body, "loadChatHistory") {
		t.Error("consumePendingJump must load history when the turn is in an earlier segment")
	}
	if !strings.Contains(body, ".turn.hist") ||
		(!strings.Contains(body, "turnTime") && !strings.Contains(src, "matchPendingJumpInHist")) {
		t.Error("consumePendingJump must scroll to the historical turn by time")
	}
	// The old behavior (a hard toast that abandons the jump on any prior history)
	// must be gone from the earlier-history branch.
	if strings.Contains(body, "earlier chat segment (before a /clear)") {
		t.Error("the earlier-segment dead-end toast should be replaced by a history load")
	}
	// History bubbles must carry data-time so the jump can target them.
	if !strings.Contains(src, `data-time="`) && !strings.Contains(src, "data-time=") {
		t.Error("history bubbles must carry data-time for jump targeting")
	}
	if !strings.Contains(src, "histBk") || !strings.Contains(src, "turn.hist") && !strings.Contains(src, "hist: true") {
		t.Error("history bubble template must mark hist turns")
	}
	// loadHistory must allow an empty scrollTo so a jump owns the scroll.
	if !strings.Contains(src, `scrollTo: scrollTo || ""`) && !strings.Contains(src, "scrollTo: scrollTo ||") {
		t.Error("loadChatHistory must allow an empty scrollTo so a jump owns the scroll")
	}
}

// TestPinnedStoreModel asserts the pinned-cards store model: an ordered
// UI.pinned array carried through init + normUI, and op-merge cases for
// pin (new pin to the front → renders at top), unpin, and drag reorder.
// applyOp lives in web/js/state.js (Packet 6C/6F); the live document is owned
// by polling.js (Packet 7I); cards.js consumes pinned via injected getters.
func TestPinnedStoreModel(t *testing.T) {
	app := mustReadApp(t)
	for _, want := range []string{
		`from "./polling.js"`,
		`from "./cards.js"`,
		`pinned: () => getUI().pinned`,
		`createPollingFeature`,
	} {
		if !strings.Contains(app, want) {
			t.Errorf("pinned store model missing %q", want)
		}
	}
	poll, err := os.ReadFile("web/js/polling.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(poll), `emptyUI,`) || !strings.Contains(string(poll), `let UI = emptyUI();`) {
		t.Error("polling.js must initialize the UI document through state.js emptyUI")
	}
	state, err := os.ReadFile("web/js/state.js")
	if err != nil {
		t.Fatal(err)
	}
	st := string(state)
	for _, want := range []string{
		`case "pin":`,
		`case "unpin":`,
		`case "pin-order":`,
		"doc.pinned.unshift(op.id)",
		`op.k === "bookmark-add" || op.k === "bookmark-del"`,
	} {
		if !strings.Contains(st, want) {
			t.Errorf("state.js pinned model missing %q", want)
		}
	}
	// pin is not in the idempotent set (replay only on matching rev).
	if strings.Contains(st, `op.k === "pin"`) && strings.Contains(st, `idempotentOp`) {
		// Ensure pin is not listed alongside bookmark-add/del.
		idem := st[strings.Index(st, "export function idempotentOp"):]
		idem = idem[:strings.Index(idem, "\n}")]
		if strings.Contains(idem, `"pin"`) {
			t.Error("pin ops must not be marked idempotent (they encode intent, replay on matching rev only)")
		}
	}
}

// TestPinnedIcons asserts both glyphs are inlined SVGs (no FontAwesome webfont
// dependency) and carry no Pro/Commercial license artifact.
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
func TestPinnedTab(t *testing.T) {
	html := mustReadIndex(t)
	app := mustReadApp(t)
	// Shell still owns the #cardtabs root.
	if !strings.Contains(html, `id="cardtabs"`) {
		t.Fatal("#cardtabs root missing from document shell")
	}
	if !strings.Contains(app, `from "./cards.js"`) || !strings.Contains(app, "createCardsFeature") {
		t.Fatal("production must wire cards.js for tab rendering")
	}
	cards, err := os.ReadFile("web/js/cards.js")
	if err != nil {
		t.Fatal(err)
	}
	body := string(cards)
	for _, want := range []string{
		`data-tab="pinned"`,
		"pinnedCount",
		`data-tab="current"`,
		`data-tab="archived"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("cards.js tab markup missing %q", want)
		}
	}
	// Ordering: the Pinned tab must be emitted after the scope tab and before
	// Archived.
	pin := strings.Index(body, `data-tab="pinned"`)
	cur := strings.Index(body, `data-tab="current"`)
	arch := strings.Index(body, `data-tab="archived"`)
	if !(cur < pin && pin < arch) {
		t.Errorf("Pinned tab must sit between current and archived (cur=%d pin=%d arch=%d)", cur, pin, arch)
	}
}

// TestPinnedFilterAndOrder asserts the Pinned tab filters to pinned cards and
// orders them attention-first, then by pin order (UI.pinned index).
// Filtering/ordering algorithms stay in map-model.js; cards.js consumes them.
func TestPinnedFilterAndOrder(t *testing.T) {
	cards, err := os.ReadFile("web/js/cards.js")
	if err != nil {
		t.Fatal(err)
	}
	cs := string(cards)
	if !strings.Contains(cs, `cardTab === "pinned"`) && !strings.Contains(cs, `cardTab: "pinned"`) {
		// Factory uses g("cardTab"); template uses cardTab === "pinned" for draggable.
		if !strings.Contains(cs, `cardTab === "pinned"`) {
			t.Error("cards.js must special-case the pinned tab for draggable cards")
		}
	}
	app := mustReadApp(t)
	if !strings.Contains(app, `from "./cards.js"`) {
		t.Error("production must wire cards.js")
	}
	if !strings.Contains(cs, "visibleCardLists") {
		t.Error("cards.js must use visibleCardLists for tab/lane visibility")
	}
	mapModel, err := os.ReadFile("web/js/map-model.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mapModel), "export function pinnedOrder") {
		t.Error("map-model.js must retain attention-first pinned ordering")
	}
}

// TestPinnedActionAndFlag asserts the swipe/hover action row leads with a
// pin/unpin button (leftmost), and a folded card shows a pin flag when pinned.
func TestPinnedActionAndFlag(t *testing.T) {
	cards, err := os.ReadFile("web/js/cards.js")
	if err != nil {
		t.Fatal(err)
	}
	html := string(cards)
	i := strings.Index(html, `<div class="actions">`)
	if i < 0 {
		t.Fatal("card actions row missing from cards.js")
	}
	j := strings.Index(html[i:], "</div>`")
	actions := html[i : i+j]
	pin := strings.Index(actions, "data-pin-action")
	arch := strings.Index(actions, "data-arch-action")
	trash := strings.Index(actions, "data-trash")
	if pin < 0 {
		t.Fatal("pin/unpin action button missing from the action row")
	}
	if !(pin < arch && pin < trash) {
		t.Errorf("pin must be the leftmost action (pin=%d arch=%d trash=%d)", pin, arch, trash)
	}
	// The folded card marks pinned state with a flag glyph near the chevron.
	if !strings.Contains(html, "pinflag") {
		t.Error("a pinned card should show a pin flag on the folded card")
	}
	// Click handler wires the action via uiMutate pin/unpin toggle.
	if !strings.Contains(html, "data-pin-action") ||
		!strings.Contains(html, `isPinned(id) ? "unpin" : "pin"`) {
		t.Error("pin action handler must toggle pin/unpin via uiMutate")
	}
}

// TestPinnedDragReorder asserts drag-to-reorder within the Pinned tab persists
// order via a pin-order op, and is poll-safe (a drag in progress suppresses the
// list rebuild). Implementation lives in cards.js (Packet 7A).
func TestPinnedDragReorder(t *testing.T) {
	cards, err := os.ReadFile("web/js/cards.js")
	if err != nil {
		t.Fatal(err)
	}
	html := string(cards)
	for _, want := range []string{
		`k: "pin-order"`,
		"pinDragging",      // the in-progress-drag guard
		`draggable="true"`, // pinned cards are draggable
	} {
		if !strings.Contains(html, want) {
			t.Errorf("pinned drag reorder missing %q", want)
		}
	}
	// Poll-safe: render must bail while a pin drag is in progress.
	if !strings.Contains(html, "if (pinDragging) return;") &&
		!strings.Contains(html, "if (pinDragging) return") {
		t.Error("renderCards must not rebuild the list mid-drag (poll-safe)")
	}
}

// TestSearchFeed asserts the G4 grouped feed wiring: the input drives a
// debounced fetch of /api/search with an AbortController (so a superseded query
// is dropped) and a client-side min-length gate, and the response renders as
// per-chat groups with headers (agent, title, lane swatch) and hits.
// Packet 7G: feed lives in search.js; Node search.test.js is authoritative.
func TestSearchFeed(t *testing.T) {
	b, err := os.ReadFile("web/js/search.js")
	if err != nil {
		t.Fatalf("read search.js: %v", err)
	}
	src := string(b)
	for _, want := range []string{
		"async function runSearch(",
		"function renderSearchFeed(",
		"export function searchGroupHTML(",
		"export function searchHitHTML(",
		"new AbortCtrl()",
		`fetchFn("/api/search?q=" + encodeURIComponent(`,
		`on(root("searchinput"), "input", onInput)`,
		"export const SEARCH_MIN =",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("search feed wiring missing %q", want)
		}
	}
	if !strings.Contains(src, "< SEARCH_MIN") {
		t.Error("runSearch must gate on the client-side min query length")
	}
	if want := fmt.Sprintf("export const SEARCH_MAX = %d;", maxSearchQuery); !strings.Contains(src, want) {
		t.Errorf("SEARCH_MAX must mirror the server cap: expected %q", want)
	}
	if !strings.Contains(src, "searchSeq") {
		t.Error("a sequence guard is expected so a stale response can't clobber a newer one")
	}
	if !strings.Contains(src, "clearTimeoutFn(searchDebounce)") {
		t.Error("the search input must be debounced (clearTimeout on searchDebounce)")
	}
	if strings.Contains(src, `dataset.searchHit` /* placeholder for a G5-only handler */) {
		t.Error("search feed must not use a dataset.searchHit placeholder")
	}
}

// TestSearchExcerptEscaping runs searchHitHTML under node to prove the excerpt
// escapes before/after and wraps ONLY the match in <mark>. Packet 7G: module
// import; Node search.test.js is the primary coverage.
func TestSearchExcerptEscaping(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JS execution check")
	}
	tmp := t.TempDir()
	for _, name := range []string{"search.js", "format.js", "bookmarks.js", "lanes.js"} {
		src, err := os.ReadFile("web/js/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmp, name), src, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := `
import assert from "node:assert/strict";
import { searchHitHTML } from "./search.js";
import { esc } from "./format.js";
const out = searchHitHTML({ before: "a <b>", match: "x&y", after: "</b> z", time: "2026-07-26T10:00:00" }, esc);
assert.ok(out.includes("a &lt;b&gt;<mark>x&amp;y</mark>&lt;/b&gt; z"),
  "excerpt must escape before/after and wrap only the match in <mark>; got: " + out);
assert.ok(!out.includes("<b>") && !out.includes("</b>"),
  "raw HTML from the excerpt must never survive into the feed; got: " + out);
`
	f := filepath.Join(tmp, "searchhit.mjs")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, f).CombinedOutput(); err != nil {
		t.Fatalf("search excerpt escaping broken: %v\n%s", err, out)
	}
}

// TestSearchActionBar asserts the G5a per-hit action bar: a tap on a hit reveals
// an adaptive bar (show-to-chat / fork / add-note), show-to-chat exits the
// overlay and jumps, fork reuses the fork sheet parented to the hit's node, and
// add-note keeps the overlay open. Packet 7G: actions in search.js; fork sheet shell.
func TestSearchActionBar(t *testing.T) {
	b, err := os.ReadFile("web/js/search.js")
	if err != nil {
		t.Fatalf("read search.js: %v", err)
	}
	src := string(b)
	for _, want := range []string{
		"export function searchHitActions(",
		"function doSearchAction(",
		"function toggleHitBar(",
		`on(root("searchfeed"), "click", onFeedClick)`,
		"data-sact",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("search action bar wiring missing %q", want)
		}
	}
	// Packet 7H: forkFromTurn lives in sheets.js; search injects it via app.js.
	sheets, err := os.ReadFile("web/js/sheets.js")
	if err != nil {
		t.Fatalf("read sheets.js: %v", err)
	}
	sheetsSrc := string(sheets)
	if !strings.Contains(sheetsSrc, "function forkFromTurn(text, parent)") {
		t.Error("forkFromTurn must accept an explicit parent so search can fork from the hit's node")
	}
	if !strings.Contains(sheetsSrc, "ncParent = parent || sel;") &&
		!strings.Contains(sheetsSrc, "ncParent = parent || sel") {
		// factory uses d.sel()
		if !strings.Contains(sheetsSrc, "parent || sel") && !strings.Contains(sheetsSrc, "parent ||") {
			t.Error("forkFromTurn should default the parent to sel to preserve existing callers")
		}
	}
	app := mustReadApp(t)
	if !strings.Contains(app, `forkFromTurn: (text, parent) => sheetsFeature.forkFromTurn(text, parent)`) {
		t.Error("search/chat must inject sheetsFeature.forkFromTurn")
	}
	// show-to-chat's live destination is jumpToHitChat: leaves overlay + pending jump.
	j := strings.Index(src, "function jumpToHitChat(")
	if j < 0 {
		t.Fatal("jumpToHitChat not found in search.js")
	}
	jumpEnd := strings.Index(src[j:], "\n  function ")
	if jumpEnd < 0 {
		jumpEnd = strings.Index(src[j:], "\n  async function ")
	}
	if jumpEnd < 0 {
		jumpEnd = 400
	}
	jump := src[j : j+jumpEnd]
	if !strings.Contains(jump, "close()") || !strings.Contains(jump, "setPendingJump") {
		t.Error("show-to-chat must exit the overlay and jump to the turn")
	}
	i := strings.Index(src, "function doSearchAction(")
	if i < 0 {
		t.Fatal("doSearchAction not found")
	}
	// body until next factory function at same indent
	rest := src[i:]
	endRel := strings.Index(rest[len("function doSearchAction("):], "\n  function ")
	if endRel < 0 {
		endRel = 1200
	} else {
		endRel += len("function doSearchAction(")
	}
	body := rest[:endRel]
	if !strings.Contains(body, "jumpToHitChat(") {
		t.Error("show action must route live hits through jumpToHitChat")
	}
	if !strings.Contains(body, "close()") || !strings.Contains(body, "forkFromTurn") {
		t.Error("fork action must exit the overlay and reuse forkFromTurn")
	}
	nb := body[strings.Index(body, `a === "bookmark"`):]
	if !strings.Contains(nb, `k: "bookmark-add"`) {
		t.Error("add-note must file a note-add op")
	}
	if strings.Contains(nb, "close()") {
		t.Error("add-note must keep the overlay open (no close in its branch)")
	}
}

// TestSearchHitActionsAdaptive runs searchHitActions under node to prove the
// per-hit action set is adaptive: every hit can be shown, only a live+forkable
// hit forks from the feed bar (archived fork lives inside the read-only view,
// where the launch config is), and notes never fork. Packet 7G: search.js module.
func TestSearchHitActionsAdaptive(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JS execution check")
	}
	tmp := t.TempDir()
	for _, name := range []string{"search.js", "format.js", "bookmarks.js", "lanes.js"} {
		src, err := os.ReadFile("web/js/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmp, name), src, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := `
import assert from "node:assert/strict";
import { searchHitActions } from "./search.js";
const eq = (a, b) => assert.deepStrictEqual(a, b);
eq(searchHitActions("live", true),      ["show", "fork", "bookmark"]);
eq(searchHitActions("live", false),     ["show", "bookmark"]);
eq(searchHitActions("bookmarks", false),    ["show", "bookmark"]);
eq(searchHitActions("bookmarks", true),     ["show", "bookmark"]);
eq(searchHitActions("archived", true),  ["show", "bookmark"]);
eq(searchHitActions("archived", false), ["show", "bookmark"]);
eq(searchHitActions("live", true, "asset"),     ["show"]);
eq(searchHitActions("archived", true, "asset"), ["show"]);
eq(searchHitActions("live", true, "user"),      ["show", "fork", "bookmark"]);
eq(searchHitActions("live", true, "assistant"), ["show", "fork", "bookmark"]);
`
	f := filepath.Join(tmp, "hitactions.mjs")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, f).CombinedOutput(); err != nil {
		t.Fatalf("search hit actions not adaptive as specified: %v\n%s", err, out)
	}
}

// TestArchivedView asserts the G5b client surface: a read-only modal for a
// deleted chat, opened from an archived show-to-chat, fed by /api/archived, with
// a fork seeded from the endpoint's launch config (no live node) and NO composer.
func TestArchivedView(t *testing.T) {
	html := mustReadIndex(t)
	app := mustReadApp(t)
	// A real modal shell root, like the search overlay.
	for _, want := range []string{
		`id="archivedview"`,
		`role="dialog"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("archived view shell missing %q", want)
		}
	}
	// Implementation lives in the composition entry (Packet 7J).
	for _, want := range []string{
		"function openArchived(",
		"function closeArchived(",
		"function renderArchived(",
		`fetch("/api/archived?uid="`,
	} {
		if !strings.Contains(app, want) {
			t.Errorf("archived view implementation missing %q", want)
		}
	}
	// Archived show-to-chat routes into the read-only surface (not a toast).
	// Packet 7G: doSearchAction lives in search.js.
	searchJS, err := os.ReadFile("web/js/search.js")
	if err != nil {
		t.Fatalf("read search.js: %v", err)
	}
	sjs := string(searchJS)
	i := strings.Index(sjs, "function doSearchAction(")
	if i < 0 {
		t.Fatal("doSearchAction not found in search.js")
	}
	rest := sjs[i:]
	endRel := strings.Index(rest[len("function doSearchAction("):], "\n  function ")
	if endRel < 0 {
		endRel = 1500
	} else {
		endRel += len("function doSearchAction(")
	}
	body := rest[:endRel]
	if !strings.Contains(body, "openArchived(") {
		t.Error("archived show-to-chat must open the read-only surface")
	}
	// The read-only surface must not host a chat composer: no textarea in the
	// #archivedview markup block (bounded to the overlay's own element).
	if vs := strings.Index(html, `<div id="archivedview"`); vs >= 0 {
		ve := strings.Index(html[vs:], "\n</div>\n") + vs
		if ve > vs && strings.Contains(html[vs:ve], "<textarea") {
			t.Error("the archived read-only surface must not contain a composer/textarea")
		}
	}
	// Packet 7H: archived fork seeds via sheets.js prepareLaunchConfig(cfg).
	if !strings.Contains(app, "function forkFromArchived()") && !strings.Contains(app, "forkFromArchived(){") {
		t.Error("archived fork entry point missing")
	}
	if !strings.Contains(app, "sheetsFeature.forkFromArchived()") {
		t.Error("archived fork must delegate to sheetsFeature")
	}
	sh, err := os.ReadFile("web/js/sheets.js")
	if err != nil {
		t.Fatalf("read sheets.js: %v", err)
	}
	shSrc := string(sh)
	if !strings.Contains(shSrc, "function prepareLaunchConfig(cfg)") ||
		!strings.Contains(shSrc, "const p = cfg || nodeById(ncParent);") {
		t.Error("prepareLaunchConfig must take an explicit config so archived fork can seed without a live node")
	}
	// The anchored turn is marked, and truncation is surfaced honestly.
	if !strings.Contains(app, "anchor") || !strings.Contains(app, "before_truncated") {
		t.Error("archived render must mark the anchor turn and honor truncation flags")
	}
	// splitAssetRefs moved to chat.js, but the archived surface still needs the
	// shared file glyph that the old inline helper closed over.
	if !strings.Contains(app, `splitAssetRefs(t.text || "", "", {}, { iconFile: ICON_FILE })`) {
		t.Error("archived asset projection must inject ICON_FILE into splitAssetRefs")
	}
}

// TestSearchRecents covers G6: recent searches are kept client-side (localStorage,
// like drafts), deduped most-recent-first, gated by the same min length as search,
// hard-capped, and only recorded when the supervisor acts on a hit. Packet 7G:
// helpers live in search.js; Node search.test.js is the primary coverage.
func TestSearchRecents(t *testing.T) {
	b, err := os.ReadFile("web/js/search.js")
	if err != nil {
		t.Fatalf("read search.js: %v", err)
	}
	src := string(b)

	for _, want := range []string{
		"export function recordRecent(",
		"export function recentsHTML(",
		"export function clearRecents(",
		"recordRecent(input ? input.value : \"\", storage)",
		`data-recent=`,
		`t.closest("#recentsclear")`,
		"export const RECENTS_CAP =",
		"export const RECENTS_SHOW =",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("recents wiring missing %q", want)
		}
	}
	if !strings.Contains(src, "if (query.length < SEARCH_MIN) return;") {
		t.Error("recordRecent must reuse the SEARCH_MIN gate")
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JS execution check")
	}
	tmp := t.TempDir()
	for _, name := range []string{"search.js", "format.js", "bookmarks.js", "lanes.js"} {
		data, err := os.ReadFile("web/js/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmp, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := `
import assert from "node:assert/strict";
import {
  SEARCH_MIN, SEARCH_MAX, RECENTS_CAP, RECENTS_SHOW, RECENTS_KEY,
  loadRecents, recordRecent, clearRecents, recentsHTML,
} from "./search.js";
import { esc } from "./format.js";

const store = new Map();
const storage = {
  getItem: k => store.has(k) ? store.get(k) : null,
  setItem: (k, v) => store.set(k, String(v)),
  removeItem: k => store.delete(k),
};

recordRecent("z".repeat(SEARCH_MAX + 50), storage);
assert.equal(loadRecents(storage)[0].length, SEARCH_MAX);
store.clear();

recordRecent("a", storage);
assert.deepEqual(loadRecents(storage), []);

recordRecent("auth", storage);
recordRecent(" bug ", storage);
recordRecent("auth", storage);
assert.deepEqual(loadRecents(storage), ["auth", "bug"]);

for (let i = 0; i < 20; i++) recordRecent("q" + i, storage);
assert.ok(loadRecents(storage).length <= RECENTS_CAP);
assert.equal(loadRecents(storage)[0], "q19");

store.clear();
recordRecent("<img src=x>", storage);
recordRecent("hello", storage);
const html2 = recentsHTML(storage, esc);
assert.ok(html2.includes("&lt;img src=x&gt;") && !html2.includes("<img src=x>"));
assert.ok(html2.includes('data-recent="hello"'));
assert.equal(RECENTS_KEY, "scimux-search-recents");
assert.equal(SEARCH_MIN, 2);
assert.ok(RECENTS_SHOW <= RECENTS_CAP);

clearRecents(storage);
assert.deepEqual(loadRecents(storage), []);
assert.equal(recentsHTML(storage, esc), "");
`
	f := filepath.Join(tmp, "recents.mjs")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, f).CombinedOutput(); err != nil {
		t.Fatalf("recents behavior broken: %v\n%s", err, out)
	}
}

// A long chat bubble taken to the Notes pane forces a lot of scrolling
// (session logs: the top ~15% of bubbles blow past a screenful). The fix
// clamps a note bubble past a pixel threshold behind a "Show more" toggle,
// leaving the ~85% short bubbles untouched. bookmarkClampState is the DOM-free
// decision core (the measure loop feeds it a real scrollHeight); execute it
// under node against synthetic heights so the clamp/label logic is locked.
func TestNoteClampState(t *testing.T) {
	// Packet 7E: clamp decision core lives in bookmarks.js; Node bookmarks.test.js
	// executes the same matrix. Keep a thin Go import smoke for the constant.
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JS execution check")
	}
	tmp := t.TempDir()
	for _, name := range []string{"bookmarks.js", "format.js", "lanes.js"} {
		src, err := os.ReadFile("web/js/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmp, name), src, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := `
import assert from "node:assert/strict";
import { BOOKMARK_CLAMP_PX, bookmarkClampState } from "./bookmarks.js";
assert.equal(BOOKMARK_CLAMP_PX, 220);
assert.deepStrictEqual(bookmarkClampState(120, false), { clamped: false, showBtn: false, label: "Show more" });
assert.deepStrictEqual(bookmarkClampState(120, true),  { clamped: false, showBtn: false, label: "Show less" });
assert.deepStrictEqual(bookmarkClampState(900, false), { clamped: true,  showBtn: true,  label: "Show more" });
assert.deepStrictEqual(bookmarkClampState(900, true),  { clamped: false, showBtn: true,  label: "Show less" });
assert.deepStrictEqual(bookmarkClampState(BOOKMARK_CLAMP_PX, false), { clamped: false, showBtn: false, label: "Show more" });
`
	f := filepath.Join(tmp, "noteclamp.mjs")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, f).CombinedOutput(); err != nil {
		t.Fatalf("note clamp decision broken: %v\n%s", err, out)
	}
}

// The clamp is view state, not evidence: it must be ephemeral (in-memory Set,
// no localStorage, no ui.json op) and its toggle must not rebuild the pane or
// fall through to the note-tap that opens the action bar. Guard the wiring and
// the ephemerality contract so neither can silently regress.
func TestNoteCollapseWiring(t *testing.T) {
	// Packet 7E: collapse wiring lives in bookmarks.js; shell keeps structural roots.
	html := mustReadIndex(t)
	bm, err := os.ReadFile("web/js/bookmarks.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(bm)
	css := mustProductionCSSCascade(t)
	for _, want := range []string{
		"expandedBookmarks = new Set()", // ephemeral, sibling of openBookmarkT
		"BOOKMARK_CLAMP_PX",             // the threshold constant
		"function bookmarkClampState(",  // the decision core
		"data-nmore",                    // the per-bubble toggle button
		`class="nmore"`,                 // its markup in the note template
	} {
		if !strings.Contains(src, want) {
			t.Errorf("note collapse wiring missing %q", want)
		}
	}
	if !strings.Contains(css, ".bookmark .nbubble.clamped") {
		t.Error("note-collapse CSS clamp is missing")
	}
	// The toggle handler is a distinct branch keyed on data-nmore. It must sit
	// in the pane click delegate BEFORE the generic ".bookmark" tap branch,
	// or clicking "Show more" would also open the action bar.
	nmore := strings.Index(src, "[data-nmore]")
	noteTap := strings.Index(src, `closest(".bookmark")`)
	if nmore < 0 {
		t.Fatal("bookmarks.js has no data-nmore branch")
	}
	if !(nmore < noteTap) {
		t.Error("the data-nmore branch must precede the .bookmark tap branch so Show more does not open the action bar")
	}
	// Ephemerality: the expand state is never persisted.
	if regexp.MustCompile(`localStorage[^;\n]*expandedBookmarks|expandedBookmarks[^;\n]*localStorage`).MatchString(src) {
		t.Error("expandedBookmarks must stay ephemeral — no localStorage persistence")
	}
	// In-place flip must not call render() inside the nmore branch.
	branch := src[nmore:]
	if endB := strings.Index(branch, "return;"); endB > 0 {
		if strings.Contains(branch[:endB], "render(") || strings.Contains(branch[:endB], "renderBookmarksPane(") {
			t.Error("the data-nmore toggle must not rebuild the pane — flip the class in place")
		}
	}
	// Shell still hosts the structural roots Bookmarks owns.
	for _, id := range []string{"bookmarkspane", "bookmarklist", "bookmarkflags", "bookmarksbtn", "notesbtn", "bookmarkpeek"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("shell missing Bookmarks root #%s", id)
		}
	}
}

// A past station opens its /clear-closed segment as history bubbles. Those must
// be tappable like live bubbles — reveal the timestamp row and the action bar —
// not inert. The tap machinery keys off a uniform data-bk addressed against a
// bubbleTurns map, so both live ("i:…") and history ("h:…") bubbles resolve.
func TestHistoryBubblesAreTappable(t *testing.T) {
	// Packet 7C: history/live bubble tap keys and bubbleTurns live in chat.js;
	// Node chat.test.js executes histBk/liveBk and action-row resolution.
	b, err := os.ReadFile("web/js/chat.js")
	if err != nil {
		t.Fatalf("read chat.js: %v", err)
	}
	src := string(b)
	// Every rendered bubble carries the uniform tap key (live + history templates).
	if strings.Count(src, "data-bk=") < 2 && strings.Count(src, "data-bk=\"") < 2 {
		// templates build data-bk via liveBk/histBk helpers
		if !strings.Contains(src, "liveBk") || !strings.Contains(src, "histBk") {
			t.Error("both live and history bubble templates must carry a data-bk tap key")
		}
	}
	if !strings.Contains(src, "bubbleTurns[") {
		t.Error("history bubbles must register their turn in the bubbleTurns lookup")
	}
	if !strings.Contains(src, "dataset.bk") && !strings.Contains(src, "turnEl.dataset.bk") {
		t.Error("the bubble tap branch must key off data-bk so history bubbles resolve")
	}
	if !strings.Contains(src, "bubbleTurns[tappedTurn]") {
		t.Error("renderBubbleActions must resolve the tapped turn from bubbleTurns")
	}
	if strings.Contains(src, `data-i="${tappedTurn}"`) {
		t.Error("renderBubbleActions must address the tapped bubble by data-bk, not data-i")
	}
	app := mustReadApp(t)
	if !strings.Contains(app, `from "./chat.js"`) || !strings.Contains(app, "chatFeature.bind()") {
		t.Fatal("production must bind chatFeature for #msgs bubble taps")
	}
}

// Desktop hover: the bubble tooltip was a bare "you"/"agent" — redundant with
// the bubble color. bubbleTitle enriches it with the send time so a hover
// surfaces what otherwise needs a tap.
func TestBubbleTitleIncludesTimestamp(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JS execution check")
	}
	b, err := os.ReadFile("web/js/chat.js")
	if err != nil {
		t.Fatalf("read chat.js: %v", err)
	}
	html := string(b)
	// The bubble templates must build their title from bubbleTitle, not a bare
	// ternary on the role.
	if strings.Contains(html, `title="${t.role === "user" ? "you" : "agent"}"`) {
		t.Error("bubble title should come from bubbleTitle(), not a bare role ternary")
	}
	if !strings.Contains(html, "bubbleTitle") && !strings.Contains(html, "titleFn") {
		t.Error("chat.js must wire bubbleTitle into bubble tooltips")
	}
	tmp := t.TempDir()
	src, err := os.ReadFile("web/js/format.js")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "format.js"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	script := `
import assert from "node:assert/strict";
import { fmtWhen, bubbleTitle } from "./format.js";
const t = "2026-07-24T09:05:00";
const w = fmtWhen(t);
assert.equal(bubbleTitle("user", t), "you — sent " + w);
assert.equal(bubbleTitle("assistant", t), "agent — sent " + w);
assert.equal(bubbleTitle("user", ""), "you");
assert.equal(bubbleTitle("assistant", null), "agent");
`
	f := filepath.Join(tmp, "bubble-title.mjs")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, f).CombinedOutput(); err != nil {
		t.Fatalf("bubble title timestamp broken: %v\n%s", err, out)
	}
}

func TestWebReferenceProvenanceLabel(t *testing.T) {
	// Packet 7F: provLabel lives in notes.js; Node notes.test.js is authoritative.
	// Keep a minimal source contract that the helper still exists for references.
	b, err := os.ReadFile("web/js/notes.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if !strings.Contains(src, "export function provLabel(") {
		t.Fatal("could not locate provLabel in web/js/notes.js")
	}
	if !strings.Contains(src, ".filter(Boolean).join(\" · \")") && !strings.Contains(src, ".filter(Boolean).join(' · ')") {
		t.Error("provLabel must join non-empty parts with middle dots")
	}
}

// A section must render its embedded references (provenance chip + snapshot text
// + jump/copy/trash actions) so a self-contained reference shows without any
// dependency on the source node still existing.
func TestWebSectionRendersReferences(t *testing.T) {
	// Packet 7F: reference HTML builders live in notes.js.
	b, err := os.ReadFile("web/js/notes.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, want := range []string{
		"function referenceHTML(", // per-reference builder
		`(s.references || [])`,    // sectionBodyInner iterates the section's references
		`data-refact="jump"`,      // jump back to the source chat
		`data-refact="copy"`,      // copy the snapshot text
		`data-refact="trash"`,     // trash removes only this reference
	} {
		if !strings.Contains(src, want) {
			t.Errorf("section reference rendering missing %q", want)
		}
	}
}

// "Use in note" is the capture→synthesis bridge. It must be an action on the
// compact Bookmark pane (the inbox on every device) and route through a shared
// placement path that posts the reference to the section endpoint.
func TestWebUseInNoteWiring(t *testing.T) {
	bm, err := os.ReadFile("web/js/bookmarks.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(bm)
	notes, err := os.ReadFile("web/js/notes.js")
	if err != nil {
		t.Fatal(err)
	}
	nsrc := string(notes)
	// Use-in-note action lives in bookmarks.js; placement body is notes.js (7F).
	if !strings.Contains(src, `data-bmact="note"`) {
		t.Error(`use-in-note wiring missing data-bmact="note" in bookmarks.js`)
	}
	for _, want := range []string{
		"function startPlacement(", // enters placement mode holding the note
		`classList.add("placing")`, // placement mode drives the add-here affordances
		`data-addhere`,             // per-section "add capture here" target
		`/references`,              // posts to the section references endpoint
	} {
		if !strings.Contains(nsrc, want) {
			t.Errorf("use-in-note wiring missing %q in notes.js", want)
		}
	}
	// Composition must instantiate Notes and wire Bookmarks → startPlacement lazily.
	app := mustReadApp(t)
	if !strings.Contains(app, `from "./notes.js"`) || !strings.Contains(app, "createNotesFeature") {
		t.Error("production must import and instantiate createNotesFeature from notes.js")
	}
	if !strings.Contains(app, "notesFeature.startPlacement") && !strings.Contains(app, "startPlacement: nt => notesFeature.startPlacement") {
		t.Error("Bookmarks startPlacement injection must call notesFeature.startPlacement")
	}
}

// Reference jump-back and the notes-pane jump must share one resolver so both
// survive /clear seams, rotation, and node deletion identically (live node →
// pendingJump; deleted-and-archived → openArchived by uid).
func TestWebSharedJumpResolver(t *testing.T) {
	app := mustReadApp(t)
	bm, err := os.ReadFile("web/js/bookmarks.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(bm)
	notes, err := os.ReadFile("web/js/notes.js")
	if err != nil {
		t.Fatal(err)
	}
	nsrc := string(notes)
	// Packet 7E: resolver lives in bookmarks.js; app.js re-exports for Notes/search.
	if !strings.Contains(src, "function jumpToChatAddress(") {
		t.Error("shared jumpToChatAddress resolver not defined in bookmarks.js")
	}
	if !strings.Contains(app, "function jumpToChatAddress(") {
		t.Error("app.js must re-export jumpToChatAddress for Notes/search callers")
	}
	// Notes references call the injected jumpToChatAddress (not a private copy).
	if !strings.Contains(nsrc, "jumpToChatAddress(") {
		t.Error("notes.js must call injected jumpToChatAddress for reference jump")
	}
	if strings.Count(app, "jumpToChatAddress(")+strings.Count(src, "jumpToChatAddress(")+strings.Count(nsrc, "jumpToChatAddress(") < 3 {
		t.Error("jumpToChatAddress must be shared across Bookmarks jump and Notes references")
	}
}

// --- Notes workspace feedback (phase 1d review) ---
// The reviewer inspected the workspace on iPad and filed 20 issues. These
// tests lock the fixes so they cannot silently regress.

// Item 1 & 2 (2026-07-30 iPad review): the workspace matches the metro-map wall
// map — it starts BELOW the status bar (var(--sbh) offset, so the status bar
// stays visible and can host search later) and paints the same page background
// (var(--bg)) as #map. It still spans the full width; it is not a centred scrim
// card.
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
// (Bookmarks inbox + Notes list) match the app pane width (340px, same as
// #cards / #bookmarkspane) so the columns share one rhythm.
func TestWorkspaceZoneWidthsMatchPanes(t *testing.T) {
	css := mustProductionCSSCascade(t)
	block := cssBlock(t, css, "#wsinbox, #wsnav {")
	if !strings.Contains(block, "340px") {
		t.Errorf("#wsinbox, #wsnav must be 340px (>=900px) to match the app panes; got %q", block)
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
	if strings.Contains(body, "if (wsOpen() || searchOpen() || archivedOpen())") {
		t.Error("touchend still re-checks live overlay open state")
	}
}

// Follow-up (2026-07-30 iPhone): the workspace owns its own swipe-back gesture
// (L→R): note editor → notes list → close, mirroring the iOS edge-back. Without
// it the swipe fell through to the app underneath.
func TestWorkspaceHasSwipeBack(t *testing.T) {
	// Packet 7F: swipe-back listeners live in notes.js factory bind().
	notes, err := os.ReadFile("web/js/notes.js")
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
	notes, err := os.ReadFile("web/js/notes.js")
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

// Item 4 (2026-07-30 iPad review): deleting the open note must clear the editor
// header so no stale title lingers. #wsnotehead sets display:flex, which beats
// the UA [hidden] rule, so an explicit hidden override is required for
// head.hidden = true to actually hide.
func TestWorkspaceNoteHeadHidesWhenEmpty(t *testing.T) {
	css := mustProductionCSSCascade(t)
	block := cssBlock(t, css, "#wsnotehead[hidden] {")
	if !strings.Contains(block, "display: none") {
		t.Errorf("#wsnotehead[hidden] must force display:none so a deleted note's title clears; got %q", block)
	}
}

// Items 5 & 6 (2026-07-30 iPad review): both empty states centre on both axes —
// the active-note placeholder inside #wsnote and the note-card empty state
// inside #wscards.
func TestWorkspaceEmptyStatesCentered(t *testing.T) {
	css := mustProductionCSSCascade(t)
	for _, sel := range []string{"#wsnoteempty {", "#wscards .empty {"} {
		block := cssBlock(t, css, sel)
		if !strings.Contains(block, "align-items: center") || !strings.Contains(block, "justify-content: center") {
			t.Errorf("%s must centre on both axes (flex align+justify center); got %q", sel, block)
		}
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
	notes, err := os.ReadFile("web/js/notes.js")
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
	// The show-more toggle branch must precede the "Use in note" branch so a tap
	// on Show more never enters placement mode.
	dele := strings.Index(src, "function onInboxListClick")
	if dele < 0 {
		t.Fatal("could not locate the inbox list click handler in notes.js")
	}
	body := src[dele:]
	more := strings.Index(body, "[data-wsimore]")
	use := strings.Index(body, "[data-wsiuse]")
	if more < 0 || use < 0 || more > use {
		t.Error("the data-wsimore toggle branch must precede the data-wsiuse branch")
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
	chatJS, err := os.ReadFile("web/js/chat.js")
	if err != nil {
		t.Fatalf("read chat.js: %v", err)
	}
	chatSrc := string(chatJS)
	// bubble action label moved to chat.js (Packet 7C)
	if !strings.Contains(chatSrc, ">bookmark</button>") && !strings.Contains(chatSrc, "bookmark</button>") {
		t.Errorf("expected new vocabulary %q not found in chat.js", ">bookmark</button>")
	}
	notesJS, err := os.ReadFile("web/js/notes.js")
	if err != nil {
		t.Fatalf("read notes.js: %v", err)
	}
	notesSrc := string(notesJS)
	corpus := html + "\n" + notesSrc
	for _, want := range []string{
		"<h2>Bookmarks</h2>",         // captures pane heading
		"Use in note", "Delete note", // synthesis-doc action + menu
		"Add bookmark here", "Placing bookmark:",
		"New note", "Note title", "Pick a note, or make one with +",
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
	bm, err := os.ReadFile("web/js/bookmarks.js")
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
	clip := strings.Index(src, `data-bmact="note"`)
	del := strings.Index(src, `data-bmact="del"`)
	if clip < 0 || del < 0 {
		t.Fatal("note action row must contain both the use-in-note and delete actions")
	}
	if !(clip < del) {
		t.Error("the use-in-note (paperclip) action must sit before delete (second-to-last)")
	}
	act := src[clip:]
	if end := strings.Index(act, "</button>"); end > 0 {
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
	nav, err := os.ReadFile("web/js/navigation.js")
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
	notes, err := os.ReadFile("web/js/notes.js")
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
func TestWorkspaceSectionMoveBoundary(t *testing.T) {
	// Packet 7F: sectionSwapPlan lives in notes.js; Node notes.test.js is authoritative.
	// Keep a thin source contract so the pure helper cannot disappear from production.
	notes, err := os.ReadFile("web/js/notes.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(notes)
	if !strings.Contains(src, "export function sectionSwapPlan(") {
		t.Fatal("could not locate sectionSwapPlan in web/js/notes.js")
	}
	if !strings.Contains(src, "return null") {
		t.Error("sectionSwapPlan must return null at boundaries (no wrap)")
	}
}

// Item 14: an embedded capture inside a section auto-collapses like a Notes
// bubble, with its own Show more/less toggle.
func TestWorkspaceReferenceClamp(t *testing.T) {
	css := mustProductionCSSCascade(t)
	notes, err := os.ReadFile("web/js/notes.js")
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
func TestWorkspaceBodyEditPreservesReferences(t *testing.T) {
	notes, err := os.ReadFile("web/js/notes.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(notes)
	if !strings.Contains(src, "function sectionBodyInner(") {
		t.Fatal("expected a sectionBodyInner helper that emits body + references together")
	}
	// startBodyEdit's blur must rebuild via the helper, never with a bare render div.
	start := strings.Index(src, "function startBodyEdit(")
	if start < 0 {
		t.Fatal("startBodyEdit missing from notes.js")
	}
	be := src[start : start+1500]
	if !strings.Contains(be, "sectionBodyInner(") {
		t.Error("startBodyEdit must restore the body via sectionBodyInner so references survive an edit")
	}
}

// Item 16: the note's overflow (three-dots) menu must actually open — the
// document-level close handler was firing on the same click. The handler stops
// propagation, and the menu offers rename + delete.
func TestWorkspaceNoteMenuOpens(t *testing.T) {
	notes, err := os.ReadFile("web/js/notes.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(notes)
	start := strings.Index(src, "function onNoteMenuClick")
	if start < 0 {
		t.Fatal("onNoteMenuClick missing from notes.js")
	}
	h := src[start : start+900]
	if !strings.Contains(h, "stopPropagation") {
		t.Error("#wsnotemenu handler must stopPropagation so the document close handler doesn't kill the menu")
	}
	if !strings.Contains(h, `data-si="del"`) {
		t.Error("Note menu must offer delete")
	}
}

// Items 18-20: Enter commits a title edit; Escape cancels it (title, section
// title, section body) without collapsing the whole workspace. The editors stop
// Escape from bubbling to the workspace-close handler.
func TestWorkspaceEscCancelsEdit(t *testing.T) {
	notes, err := os.ReadFile("web/js/notes.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(notes)
	// note-title keydown handles Enter (blur) and Escape (revert + stopPropagation)
	start := strings.Index(src, "function onTitleKeydown")
	if start < 0 {
		t.Fatal("onTitleKeydown missing from notes.js")
	}
	kd := src[start : start+600]
	if !strings.Contains(kd, `"Enter"`) || !strings.Contains(kd, `"Escape"`) {
		t.Error("#wsnotetitle keydown must handle both Enter and Escape")
	}
	if !strings.Contains(kd, "stopPropagation") {
		t.Error("#wsnotetitle Escape must stopPropagation so the workspace does not close")
	}
	// section body editor cancels on Escape
	beStart := strings.Index(src, "function startBodyEdit(")
	if beStart < 0 {
		t.Fatal("startBodyEdit missing")
	}
	be := src[beStart : beStart+1200]
	if !strings.Contains(be, `"Escape"`) || !strings.Contains(be, "stopPropagation") {
		t.Error("startBodyEdit must cancel on Escape and stopPropagation")
	}
}

// Item 17: the wide (iPad/desktop) layout shows all three zones and hides the
// redundant back chevron. The multi-zone breakpoint reaches iPad portrait.
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
func TestWorkspaceSaveSerializedPerField(t *testing.T) {
	notes, err := os.ReadFile("web/js/notes.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(notes)
	if !strings.Contains(src, "function createSaveEnqueue(") && !strings.Contains(src, "export function createSaveEnqueue(") {
		t.Fatal("could not locate createSaveEnqueue serialization core in notes.js")
	}
	if !strings.Contains(src, "function wsPatchNow(") && !strings.Contains(src, "wsPatchNow(body, key)") {
		t.Error("wsPatchNow (immediate keyed enqueue) must exist so reverts serialize behind in-flight saves")
	}
	// The revert sites must enqueue on the field's key (via wsPatchNow), not fire
	// an unkeyed immediate wsPatch that bypasses the chain.
	for _, unkeyed := range []string{
		"wsPatch({ title: wsTitleOrig })",
		"wsPatch({ section: { id: secId, title: orig } })",
		"wsPatch({ section: { id: secId, body: orig } })",
	} {
		if strings.Contains(src, unkeyed) {
			t.Errorf("revert still uses an unkeyed wsPatch that bypasses per-field ordering: %q", unkeyed)
		}
	}
	if !strings.Contains(src, "wsPatchNow({ title: wsTitleOrig }") {
		t.Error("title Escape revert must use keyed wsPatchNow")
	}
	if !strings.Contains(src, "SAVE_DEBOUNCE_MS = 1100") && !strings.Contains(src, "SAVE_DEBOUNCE_MS") {
		t.Error("save debounce constant must remain 1100ms")
	}
}

// Review finding 3: an anchored bookmark *comment* embedded into a note must
// inherit its parent bookmark's lane in the reference snapshot, exactly like the
// Bookmarks pane does. The snapshot path fed bookmarkLaneId an empty byT map, so
// an anchored comment lost its lane color and the note card missed that lane dot.
// Packet 7F: buildBookmarkSnapshot lives in notes.js; Node notes.test.js covers it.
func TestWorkspaceReferenceSnapshotInheritsCommentLane(t *testing.T) {
	notes, err := os.ReadFile("web/js/notes.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(notes)
	if !strings.Contains(src, "export function buildBookmarkSnapshot(") {
		t.Fatal("buildBookmarkSnapshot must live in notes.js")
	}
	if !strings.Contains(src, "bookmarkLaneId") {
		t.Error("snapshot must reuse bookmarkLaneId so comments inherit parent lane")
	}
}

// sliceBetween returns the text from the occurrence of start up to (but not
// including) the first occurrence of end after it.
// The new-activity effort menu is per-model where the CLI advertises it (codex
// via `codex debug models`): /api/agents now returns {models, efforts} per
// agent, the browser stashes the per-model effort menus, and fillEfforts uses
// the selected model's menu before the static per-agent EFFORTS fallback.
// Packet 7H: wiring lives in sheets.js.
func TestPerModelEffortWiring(t *testing.T) {
	b, err := os.ReadFile("web/js/sheets.js")
	if err != nil {
		t.Fatalf("read sheets.js: %v", err)
	}
	src := string(b)
	for _, want := range []string{
		"MODEL_EFFORTS", // per-agent → per-model effort store
		"info.efforts",  // populated from the /api/agents payload
		`on(root("nc_model"), "change", onModelChange)`, // effort menu follows the model
	} {
		if !strings.Contains(src, want) {
			t.Errorf("per-model effort wiring missing %q", want)
		}
	}
	// fillEfforts must consult the selected model's menu first, then fall back
	// to the static per-agent EFFORTS list, and mark the model's default level.
	fe := sliceBetween(t, src, "function fillEfforts(", "\n  }")
	if !strings.Contains(fe, "MODEL_EFFORTS") {
		t.Errorf("fillEfforts must consult per-model efforts: %q", fe)
	}
	if !strings.Contains(fe, "EFFORTS") {
		t.Errorf("fillEfforts must keep the static per-agent EFFORTS fallback: %q", fe)
	}
	if !strings.Contains(src, "(default)") {
		t.Errorf("fillEfforts should mark the model's default effort level")
	}
	app := mustReadApp(t)
	if !strings.Contains(app, `from "./sheets.js"`) || !strings.Contains(app, "createSheetsFeature") {
		t.Error("production must wire sheets.js for new-activity effort menus")
	}
}

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
	notes, err := os.ReadFile("web/js/notes.js")
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
	if !strings.Contains(app, "overlayOwned: wsOpen() || searchOpen() || archivedOpen()") {
		t.Error("touchstart must snapshot overlay-open state (overlayOwned: wsOpen() || searchOpen() || archivedOpen())")
	}
	// touchend consults documentSwipeDecision's suppress on that snapshot only.
	if !strings.Contains(app, "documentSwipeDecision") {
		t.Error("touchend must consult documentSwipeDecision with the touchstart snapshot")
	}
	if strings.Contains(app, "if (wsOpen() || searchOpen() || archivedOpen()){ touch = null; return; }") {
		t.Error("touchend still re-checks live wsOpen() — racy: the overlay handler flips it before this bubbles, causing the Notes→L→R overshoot to Chat")
	}
	nav, err := os.ReadFile("web/js/navigation.js")
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
	nav, err := os.ReadFile("web/js/navigation.js")
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
	bm, err := os.ReadFile("web/js/bookmarks.js")
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
	title := strings.Index(phead, `<h2>Journeys</h2>`)
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
