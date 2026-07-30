package main

import (
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

// The whole UI is one embedded HTML file with inline scripts — a stray
// backtick or brace ships silently in the binary and only surfaces as a
// blank page on the iPad. Parse the scripts with node when it is available.
func TestWebIndexScriptsParse(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JS syntax check")
	}
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	blocks := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(string(b), -1)
	if len(blocks) == 0 {
		t.Fatal("no inline <script> blocks found in web/index.html")
	}
	var js strings.Builder
	for _, m := range blocks {
		js.WriteString(m[1])
		js.WriteString("\n;\n")
	}
	f := filepath.Join(t.TempDir(), "index.js")
	if err := os.WriteFile(f, []byte(js.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "--check", f).CombinedOutput(); err != nil {
		t.Fatalf("inline JS does not parse: %v\n%s", err, out)
	}
}

// Extract the pure markdown helpers (mdInline/md) from the embedded page and
// execute them under node with a DOM-free esc stub. Finding 97: paragraph
// lines must be escaped per line and then joined with a real <br>, never the
// other way around.
func TestWebMarkdownParagraphs(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JS execution check")
	}
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	start := strings.Index(html, "function mdInline(")
	if start < 0 {
		t.Fatal("could not locate mdInline in web/index.html")
	}
	end := strings.Index(html[start:], "/* ---------- statusbar")
	if end < 0 {
		t.Fatal("could not locate the end of the markdown helpers in web/index.html")
	}
	script := `function esc(s){ return String(s ?? "").replace(/&/g,"&amp;").replace(/</g,"&lt;").replace(/>/g,"&gt;"); }` + "\n" +
		html[start:start+end] + `
const assert = require("assert");
assert.strictEqual(md("line one\nline two"), "<p>line one<br>line two</p>");
assert.strictEqual(md("a <b> tag\nnext"), "<p>a &lt;b&gt; tag<br>next</p>");
assert.strictEqual(md("solo"), "<p>solo</p>");
assert.strictEqual(md("p1 l1\np1 l2\n\np2"), "<p>p1 l1<br>p1 l2</p><p>p2</p>");
`
	f := filepath.Join(t.TempDir(), "md.js")
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
func TestForkPayloadSendsVisibleLaunchConfig(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	for _, field := range []string{"agent", "model", "effort"} {
		if strings.Contains(html, field+`: ncParent ? "" :`) {
			t.Errorf("new-node payload field %q is still emptied in fork mode", field)
		}
		if !strings.Contains(html, field+`: $("#nc_`+field+`").value`) {
			t.Errorf("new-node payload field %q does not send the visible selector value", field)
		}
	}
	// The per-open re-seed that replaces the old empty-payload staleness guard.
	// (prepareLaunchConfig now takes an optional explicit config for archived
	// fork; the on-open rebuild — fillAgents/fillModels first — is unchanged.)
	if !strings.Contains(html, "function prepareLaunchConfig(cfg){\n  fillAgents(); fillModels();") {
		t.Error("prepareLaunchConfig no longer rebuilds the selectors on open — stale-config guard lost")
	}
}

// The effort selector offers agent-specific levels: claude's --effort takes
// five (low..max), and the list must follow the selected agent, seed from the
// parent on a fork, and rebuild on an agent switch.
func TestEffortLevelsPerAgent(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	if !strings.Contains(html, `claude: ["low", "medium", "high", "xhigh", "max"]`) {
		t.Error("EFFORTS is missing claude's five --effort levels")
	}
	if !strings.Contains(html, "function fillEfforts(){") {
		t.Error("fillEfforts() not defined")
	}
	// The effort list must follow an agent switch and a fork's parent seed.
	if !strings.Contains(html, `$("#nc_agent").addEventListener("change", () => { fillModels(); fillEfforts(); });`) {
		t.Error("agent change does not refill the effort list")
	}
	if !strings.Contains(html, "fillModels(); fillEfforts();   /* model + effort lists follow") {
		t.Error("prepareLaunchConfig does not refill efforts for the parent's agent")
	}
}

func TestNotesToggleDirectionMatchesPaneState(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	// the closed Bookmarks toggle announces opening bookmarks (glyph set by
	// renderBookmarksToggle, so the static button carries no chevron)
	if !strings.Contains(html, `id="bookmarksbtn" aria-label="open bookmarks"></button>`) {
		t.Fatal("closed bookmarks toggle should announce opening bookmarks")
	}
	for _, want := range []string{
		`btn.innerHTML = bookmarksOpen ? "&#8250;" : "&#8249;";`,
		`btn.setAttribute("aria-label", bookmarksOpen ? "close bookmarks" : "open bookmarks");`,
		"renderBookmarksToggle();\n  renderBookmarkFlags();",
	} {
		if !strings.Contains(html, want) {
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
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
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
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	if strings.Contains(html, "copybtn") {
		t.Fatal("chat bubbles should not render an always-visible copy button")
	}
	rowStart := strings.Index(html, "row.innerHTML =")
	if rowStart < 0 {
		t.Fatal("bubble action row renderer not found")
	}
	rowEnd := strings.Index(html[rowStart:], "turnEl.after(row);")
	if rowEnd < 0 {
		t.Fatal("bubble action row renderer end not found")
	}
	row := html[rowStart : rowStart+rowEnd]
	for _, want := range []string{
		`<div class="bubwhen">${esc(fmtBubbleTime(turn.time))}</div>`,
		`data-bact="fork"`,
		`data-bact="desc"`,
		`data-bact="bookmark"`,
		`data-bact="copy"`,
	} {
		if !strings.Contains(row, want) {
			t.Errorf("bubble action row missing %q", want)
		}
	}
	if !(strings.Index(row, `data-bact="bookmark"`) < strings.Index(row, `data-bact="copy"`)) {
		t.Fatal("copy should be the rightmost bubble action")
	}
	if !(strings.Index(row, `class="bubwhen"`) < strings.Index(row, `data-bact="fork"`)) {
		t.Fatal("timestamp should sit between the bubble and the action buttons")
	}
	if !strings.Contains(html, `if (ba.dataset.bact === "copy"){`) ||
		!strings.Contains(html, `copyText(turn.text || "");`) {
		t.Fatal("copy action row button is not wired to copy the selected turn")
	}
}

func TestBubbleTimestampFormatting(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JS execution check")
	}
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	start := strings.Index(html, "function fmtBubbleTime(")
	if start < 0 {
		t.Fatal("could not locate fmtBubbleTime in web/index.html")
	}
	end := strings.Index(html[start:], "/* forkFromTurn:")
	if end < 0 {
		t.Fatal("could not locate the end of fmtBubbleTime in web/index.html")
	}
	script := html[start:start+end] + `
const assert = require("assert");
const now = new Date("2026-07-24T15:30:00");
assert.strictEqual(fmtBubbleTime("2026-07-24T09:05:00", now), new Date("2026-07-24T09:05:00").toLocaleTimeString([], { timeStyle: "short" }));
assert.strictEqual(fmtBubbleTime("2026-07-23T22:10:00", now), "Yesterday, " + new Date("2026-07-23T22:10:00").toLocaleTimeString([], { timeStyle: "short" }));
assert.strictEqual(fmtBubbleTime("2026-07-20T08:15:00", now), new Date("2026-07-20T08:15:00").toLocaleString([], { dateStyle: "medium", timeStyle: "short" }));
assert.strictEqual(fmtBubbleTime("not-a-date", now), "not-a-date");
`
	f := filepath.Join(t.TempDir(), "bubble-time.js")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, f).CombinedOutput(); err != nil {
		t.Fatalf("bubble timestamp formatting broken: %v\n%s", err, out)
	}
}

// R18.1 altitude: a lane color is user-authored and flows into many style=""
// interpolations. safeColor must return only well-formed color syntax verbatim
// and reject anything that could break out of an attribute, so the accessor is
// the single guard rather than every call site's esc() wrap.
func TestWebSafeColorRejectsInjection(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JS execution check")
	}
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	start := strings.Index(html, "const SAFE_COLOR")
	end := strings.Index(html, "function laneColor(")
	if start < 0 || end < 0 || end < start {
		t.Fatal("could not locate the safeColor helpers in web/index.html")
	}
	script := html[start:end] + `
const assert = require("assert");
for (const ok of ["#abc", "#aabbcc", "#aabbccdd", "var(--work)", "rgb(1,2,3)", "rgba(1,2,3,.5)", "hsl(200, 50%, 40%)", "tomato", "  #fff  "]) {
  assert.ok(safeColor(ok) !== null, "should accept " + ok);
}
for (const bad of ['#fff"><script>', 'red;background:url(x)', 'expression(1)', '</style>', '', null, undefined, "var(--x); }"]) {
  assert.strictEqual(safeColor(bad), null, "should reject " + JSON.stringify(bad));
}
assert.strictEqual(safeColor("  #fff  "), "#fff", "trims");
`
	f := filepath.Join(t.TempDir(), "color.js")
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
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	start := strings.Index(html, "function hexRGB(")
	end := strings.Index(html, "function uniqueLaneID(")
	if start < 0 || end < 0 || end < start {
		t.Fatal("could not locate lane contrast helpers in web/index.html")
	}
	script := html[start:end] + `
const assert = require("assert");
assert.deepStrictEqual(hexRGB("#abc"), [170,187,204]);
assert.strictEqual(contrastText("#007AFF"), "#000", "system blue has stronger black contrast by WCAG ratio");
assert.strictEqual(contrastText("#5856D6"), "#fff", "system indigo needs white text");
assert.strictEqual(contrastText("#FF9500"), "#000", "system orange needs dark text");
assert.ok(contrastRatio(relLum(hexRGB("#FF9500")), relLum(hexRGB(contrastText("#FF9500")))) >= 4.5);
assert.ok(contrastRatio(relLum(hexRGB("#5856D6")), relLum(hexRGB(contrastText("#5856D6")))) >= 4.5);
`
	f := filepath.Join(t.TempDir(), "lane-contrast.js")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, f).CombinedOutput(); err != nil {
		t.Fatalf("lane chip contrast helpers broken: %v\n%s", err, out)
	}
}

func TestActivityCardShowsUserInteractionAgeAndHostConnectivity(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	for _, want := range []string{
		`<span class="host offline" id="host">scimux</span>`,
		`#statusbar .host::before`,
		`#statusbar .host.online::before { background: #34C759; }`,
		`function setHostOnline(ok){`,
		`setHostOnline(false); $("#sys").textContent = "server unreachable"; return;`,
		`setHostOnline(true);`,
		`let cardTimeFlip = 0;`,
		`const CARD_TIME_SWAP_MS = 30000;`,
		`function cardTimeItems(n){`,
		`function cardTimeHTML(n){`,
		`const you = n.last_interaction || 0;`,
		`Last interaction ${ageText(you)} · last seen ${ageText(seen)}`,
		`const it = items[cardTimeFlip % items.length];`,
		`return ` + "`${it.key} ${ageText(it.ms)}`" + `;`,
		`${cardTimeHTML(n)}`,
		`function updateCardAges(animate=false){`,
		`time.classList.add("roll-dn");`,
		`setInterval(() => {`,
		`cardTimeFlip++;`,
		`updateCardAges(true);`,
		`}, CARD_TIME_SWAP_MS);`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("activity card interaction/connectivity wiring missing %q", want)
		}
	}
	if strings.Contains(html, `<span class="age">${ageText(n.last_activity)}</span>`) {
		t.Fatal("folded activity cards should not label pane movement as the primary age")
	}
}

func TestActivityCardOrderPinsAttentionThenFreshCards(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JS execution check")
	}
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	start := strings.Index(html, "const hardAttention = ")
	if start < 0 {
		t.Fatal("could not locate hardAttention in web/index.html")
	}
	end := strings.Index(html[start:], "const LANE_COLORS")
	if end < 0 {
		t.Fatal("could not locate the end of orderedNodes in web/index.html")
	}
	script := `let nodes = [];
` + html[start:start+end] + `
const assert = require("assert");
nodes = [
  {id:"active-older", last_activity: 900, last_interaction: 200, created_at:"2026-01-04T00:00:00Z"},
  {id:"newer-fresh", created_at:"2026-01-06T00:00:00Z"},
  {id:"active-newer", last_activity: 300, last_interaction: 800, created_at:"2026-01-01T00:00:00Z"},
  {id:"never-touched", last_activity: 700, created_at:"2026-01-03T00:00:00Z"},
  {id:"older-fresh", created_at:"2026-01-05T00:00:00Z"},
  {id:"attn", attention:"approval", last_activity: 100, last_interaction: 100, created_at:"2026-01-02T00:00:00Z"},
];
assert.deepStrictEqual(
  orderedNodes().map(n => n.id),
  ["attn", "newer-fresh", "older-fresh", "active-newer", "active-older", "never-touched"]
);
`
	f := filepath.Join(t.TempDir(), "card-order.js")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, f).CombinedOutput(); err != nil {
		t.Fatalf("activity card ordering broken: %v\n%s", err, out)
	}
}

// A collapsed (docked column) metro map should let you edit a station's title
// and description without going full-screen: long-press / right-click a station
// opens the same activity editor the full-screen wall map reaches via its
// pencil. Earlier that gesture early-returned unless mapFull was set, so the
// docked map had no edit affordance at all.
func TestDockedMapStationLongPressEdits(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	start := strings.Index(html, `longpress($("#mapwrap"), ".strow"`)
	if start < 0 {
		t.Fatal("station long-press handler not found")
	}
	end := strings.Index(html[start:], "});")
	if end < 0 {
		t.Fatal("station long-press handler not terminated")
	}
	block := html[start : start+end]
	// The docked map must reach the editor — no unconditional mapFull bail-out.
	if strings.Contains(block, "if (!mapFull) return;") {
		t.Error("station long-press still bails out of the docked (collapsed) map")
	}
	// The full-screen-only selection move stays fenced behind mapFull.
	if !strings.Contains(block, "if (mapFull){") {
		t.Error("full-screen selection move should be gated on mapFull, not run in the docked map")
	}
	// Both maps open the editor scoped to the pressed station's stop.
	if !strings.Contains(block, "openActivityEditor(id, stop)") {
		t.Error("station long-press should open the activity editor scoped to the station's stop")
	}
}

func TestJourneyLaneFoldAndChipWiring(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	for _, want := range []string{
		`let mapFoldKnown = new Set(JSON.parse(localStorage.getItem("scimux-mapfold-known") || "[]"));`,
		`for (const id of live) if (!mapFoldKnown.has(id)){`,
		`mapFold.add(id);`,
		`mapFoldKnown.add(id);`,
		`localStorage.setItem("scimux-mapfold-known", JSON.stringify([...mapFoldKnown]));`,
		`if (focusLane && mapFold.has(focusLane)){`,
		`mapFold.delete(focusLane);`,
		`class="lanechip ${focusLane === l.id ? "selected" : ""}"`,
		`style="${laneChipStyle(lm.color(l.id), focusLane === l.id)}"`,
		`return ` + "`border-color:${c};background:${c};color:${esc(contrastText(color))}`" + `;`,
		`const groups = [...(UI.groups || [])].sort(byNameID);`,
		`lanes.sort(byNameID);`,
		`blocks.sort((a, b) => byNameID(a.lane, b.lane));`,
		`animation: mapAttentionDot 1.65s ease-in-out infinite;`,
		`class="attnstation-glow"`,
		`class="attnstation-ring"`,
		`.lhead .lattn, .attnstation-glow, .attnstation-ring { animation: none; }`,
		`mapFold.add(id);`,
		`setLaneFilter("");`,
		`mapFold.delete(id);`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("journey lane fold/chip wiring missing %q", want)
		}
	}
	chipCSSStart := strings.Index(html, ".lanechip {")
	chipCSSEnd := strings.Index(html[chipCSSStart:], ".lanechip.selected")
	if chipCSSStart < 0 || chipCSSEnd < 0 {
		t.Fatal("lane chip CSS not found")
	}
	chipCSS := html[chipCSSStart : chipCSSStart+chipCSSEnd]
	if strings.Contains(html, ".lanechip.focused") || strings.Contains(chipCSS, "opacity:") {
		t.Fatal("lane chips should no longer show selection by dimming unselected chips")
	}
	if !strings.Contains(html, `.lhead .chev { color: var(--dim); font-size: 15px; width: 16px;`) {
		t.Fatal("lane fold glyph should be enlarged enough to read as a disclosure control")
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
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	start := strings.Index(html, "function forkKind(n){")
	if start < 0 {
		t.Fatal("could not locate forkKind in web/index.html")
	}
	end := strings.Index(html[start:], "function renderMap(){")
	if end < 0 {
		t.Fatal("could not locate the end of forkKind in web/index.html")
	}
	script := `let nodes = [];
function nodeById(id){ return nodes.find(n => n.id === id); }
` + html[start:start+end] + `
const assert = require("assert");
// Y-new: destination lane had no station at fork time; a LATER station in that
// lane must not flip the historical fork to S.
nodes = [
  {id:"p",     lane_id:"lane-a", created_at:"2026-01-01T00:00:00Z"},
  {id:"c",     lane_id:"lane-b", parent:"p", created_at:"2026-01-02T00:00:00Z"},
  {id:"later", lane_id:"lane-b", created_at:"2026-01-03T00:00:00Z"},
];
assert.strictEqual(forkKind(nodeById("c")), "y-new", "grown lane must stay y-new");
// S: destination lane already had an older station when the child forked in.
nodes = [
  {id:"a0", lane_id:"lane-b", created_at:"2026-01-01T00:00:00Z"},
  {id:"p",  lane_id:"lane-a", created_at:"2026-01-02T00:00:00Z"},
  {id:"c",  lane_id:"lane-b", parent:"p", created_at:"2026-01-03T00:00:00Z"},
];
assert.strictEqual(forkKind(nodeById("c")), "s", "prior station in dest lane is a crossover");
// Y-stay: same lane as parent.
nodes = [
  {id:"p", lane_id:"lane-a", created_at:"2026-01-01T00:00:00Z"},
  {id:"c", lane_id:"lane-a", parent:"p", created_at:"2026-01-02T00:00:00Z"},
];
assert.strictEqual(forkKind(nodeById("c")), "y-stay", "same-lane fork is y-stay");
// Root and deleted-parent: no glyph.
assert.strictEqual(forkKind({id:"r", lane_id:"lane-a"}), null, "root has no fork");
nodes = [{id:"c", lane_id:"lane-b", parent:"ghost", created_at:"2026-01-02T00:00:00Z"}];
assert.strictEqual(forkKind(nodeById("c")), null, "deleted parent has no glyph");
`
	f := filepath.Join(t.TempDir(), "forkkind.js")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, f).CombinedOutput(); err != nil {
		t.Fatalf("forkKind classification broken: %v\n%s", err, out)
	}
}

// The stack/phone map is the primary map on small screens, so it must keep
// carrying fork topology (finding #3) even though it cannot draw the wall map's
// crossover curves: a per-station fork cue, its dot spur, and a tap-to-origin
// jump. Guard the wiring so it cannot silently regress while the wall map stays
// correct — and guard that the old population-based classifier is gone.
func TestWebStackMapKeepsForkCues(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	for _, want := range []string{
		"function forkCueHTML(",   // the caption builder
		"fork: true,",             // stack renderer opts in
		"data-goorigin=",          // tap-to-origin target
		"function gotoStation(",   // …and its handler
		`forkKind(n) === "y-new"`, // wall map classifies by history
	} {
		if !strings.Contains(html, want) {
			t.Errorf("stack fork-topology wiring missing: %q", want)
		}
	}
	if strings.Contains(html, "laneCount[n.lane_id]") {
		t.Error("old population-based Y-new/S classifier still present; must be replaced by forkKind")
	}
}

func TestStructuredApprovalDoesNotInventYN(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	i := strings.Index(html, `} else if (d.source === "acp") {`)
	if i < 0 {
		t.Fatal("structured approval branch not found")
	}
	j := strings.Index(html[i:], `} else {`)
	if j < 0 {
		t.Fatal("structured approval branch end not found")
	}
	branch := html[i : i+j]
	if strings.Contains(branch, `["y","n"]`) || strings.Contains(branch, `['y','n']`) {
		t.Fatal("structured approval branch must render protocol options only, not y/n fallback")
	}
}

// TestSearchOverlayShell asserts the G3 search overlay is a real Spotlight-style
// modal: a 🔍 trigger reachable from any view, an aria-modal dialog that blurs
// the app behind it (with a reduced-transparency fallback), Escape/backdrop/✕ to
// close, and focus returned to the trigger. The grouped feed itself is G4 — here
// #searchfeed exists but only carries the blank-state hint.
func TestSearchOverlayShell(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)

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
	if !strings.Contains(html, "backdrop-filter: blur") {
		t.Error("search scrim should blur the app behind it")
	}
	if !strings.Contains(html, "prefers-reduced-transparency: reduce") {
		t.Error("search overlay must honor prefers-reduced-transparency")
	}
	if !strings.Contains(html, "prefers-reduced-motion: reduce") {
		t.Error("search overlay must honor prefers-reduced-motion")
	}

	// Open/close plumbing: named functions, keyboard open, Escape close, and
	// focus returned to the trigger on close (focus trap / restore).
	for _, want := range []string{
		"function openSearch(",
		"function closeSearch(",
		`#searchbtn").addEventListener("click"`,
		"searchReturnFocus",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("search overlay behavior missing %q", want)
		}
	}
}

// TestSearchSlashShortcut asserts the "/" activation shortcut (spec: search opens
// on "/" when focus is not in a text field), alongside the existing Cmd/Ctrl-K.
func TestSearchSlashShortcut(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	// The global keydown handler must react to "/", guarded so it stays a literal
	// slash inside a text field and doesn't fire while the overlay is already open.
	i := strings.Index(html, `if ((e.metaKey || e.ctrlKey) && !e.altKey && (e.key === "k"`)
	if i < 0 {
		t.Fatal("global search shortcut handler not found")
	}
	handler := html[i : i+strings.Index(html[i:], "\n});")]
	if !strings.Contains(handler, `e.key === "/"`) {
		t.Error(`the "/" open shortcut is missing`)
	}
	if !strings.Contains(handler, "!searchOpen()") {
		t.Error(`the "/" shortcut must not fire while the overlay is already open`)
	}
	if !strings.Contains(handler, `/^(INPUT|TEXTAREA|SELECT)$/.test(e.target.tagName)`) ||
		!strings.Contains(handler, "!e.target.isContentEditable") {
		t.Error(`the "/" shortcut must be suppressed while a text field is focused`)
	}
}

// TestSearchHitCarriesIdentity asserts each rendered hit carries its stable
// identity in the DOM — role (so asset actions can be gated) and segment/record
// (so archived show-to-chat can anchor by ordinal, not timestamp).
func TestSearchHitCarriesIdentity(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	i := strings.Index(html, "function searchHitHTML(")
	if i < 0 {
		t.Fatal("searchHitHTML not found")
	}
	body := html[i : i+strings.Index(html[i:], "\n}\n")]
	for _, want := range []string{
		`data-role="${esc(h.role`,
		`data-segment="${h.segment`,
		`data-record="${h.record`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("hit markup missing stable-identity attribute %q", want)
		}
	}
	// toggleHitBar must feed the role into searchHitActions so an asset hit's
	// actions are gated.
	if !strings.Contains(html, "searchHitActions(el.dataset.kind, el.dataset.forkable === \"1\", el.dataset.role)") {
		t.Error("toggleHitBar must pass the hit role into searchHitActions")
	}
	// Archived show-to-chat must anchor by (segment, record), passed to openArchived.
	if !strings.Contains(html, "openArchived(hit.dataset.uid, hit.dataset.segment, hit.dataset.record, turn)") {
		t.Error("archived show-to-chat must anchor by seg/rec, not just timestamp")
	}
	if !strings.Contains(html, `"&seg=" + encodeURIComponent(seg`) ||
		!strings.Contains(html, `"&rec=" + encodeURIComponent(rec`) {
		t.Error("openArchived must send seg/rec to /api/archived")
	}
}

// TestSearchEarlierHistoryJump asserts finding #1's fix: a show-to-chat hit whose
// turn lives in an earlier segment (before a /clear) no longer dead-ends in a
// toast — consumePendingJump loads the full history and scrolls to the historical
// turn by time, which requires the history bubbles to carry data-time.
func TestSearchEarlierHistoryJump(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	i := strings.Index(html, "function consumePendingJump(")
	if i < 0 {
		t.Fatal("consumePendingJump not found")
	}
	body := html[i : i+strings.Index(html[i:], "\n}\n")]
	// It must fall through to history instead of only toasting on an earlier-segment hit.
	if !strings.Contains(body, "loadChatHistory(sel") {
		t.Error("consumePendingJump must load history when the turn is in an earlier segment")
	}
	if !strings.Contains(body, ".turn.hist") || !strings.Contains(body, "dataset.time === pendingJump.turnTime") {
		t.Error("consumePendingJump must scroll to the historical turn by time")
	}
	// The old behavior (a hard toast that abandons the jump on any prior history)
	// must be gone from the earlier-history branch.
	if strings.Contains(body, "earlier chat segment (before a /clear)") {
		t.Error("the earlier-segment dead-end toast should be replaced by a history load")
	}
	// History bubbles must carry data-time so the jump can target them.
	if !strings.Contains(html, `class="turn hist ${t.role === "user" ? "user" : "assistant"}${h.html && !h.clean ? " media" : ""}"`) ||
		!regexp.MustCompile(`class="turn hist [^\n]*?data-time="\$\{esc\(t\.time`).MatchString(html) {
		t.Error("history bubbles must carry data-time for jump targeting")
	}
	// loadChatHistory must no longer force a seam scroll for an empty target, or the
	// pending jump could never win the scroll.
	if !strings.Contains(html, `scrollTo: scrollTo || ""`) {
		t.Error("loadChatHistory must allow an empty scrollTo so a jump owns the scroll")
	}
}

// TestPinnedStoreModel asserts the pinned-cards store model: an ordered
// UI.pinned array carried through init + normUI, and op-merge cases for
// pin (new pin to the front → renders at top), unpin, and drag reorder.
func TestPinnedStoreModel(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	for _, want := range []string{
		`let UI = { groups: [], archived: [], bookmarks: [], lanes: [], pinned: [] };`,
		`pinned: []`, // in normUI defaults too
		`case "pin":`,
		`case "unpin":`,
		`case "pin-order":`,
		"const isPinned =",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("pinned store model missing %q", want)
		}
	}
	// A new pin goes to the FRONT (unshift) so it lands at the top of the tab.
	if !strings.Contains(html, "doc.pinned.unshift(op.id)") {
		t.Error("pin op should unshift (new pins land at the top)")
	}
	// pin-order (drag) replaces the whole ordered list, like groups/lanes; and
	// like arch/unarch it is not an always-replay idempotent op.
	if strings.Contains(html, `op.k === "pin"`) &&
		!strings.Contains(html, `idempotentOp = op => op.k === "bookmark-add" || op.k === "bookmark-del"`) {
		t.Error("pin ops must not be marked idempotent (they encode intent, replay on matching rev only)")
	}
}

// TestPinnedIcons asserts both glyphs are inlined SVGs (no FontAwesome webfont
// dependency) and carry no Pro/Commercial license artifact.
func TestPinnedIcons(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	if !strings.Contains(html, "const ICON_PIN =") || !strings.Contains(html, "const ICON_UNPIN =") {
		t.Error("ICON_PIN / ICON_UNPIN inlined glyphs missing")
	}
	if strings.Contains(html, "fa-thumbtack") || strings.Contains(html, `class="fa-`) {
		t.Error("icons must be inlined SVG, not FontAwesome webfont classes")
	}
	if strings.Contains(html, "Commercial License") || strings.Contains(html, "Font Awesome Pro") {
		t.Error("must not embed a Pro/Commercial-licensed glyph artifact")
	}
}

// TestPinnedTab asserts a conditional "Pinned" tab sits between the scope tab
// and "Archived", shows a live count, and appears only when something is pinned.
func TestPinnedTab(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	i := strings.Index(html, "function renderCardTabs()")
	if i < 0 {
		t.Fatal("renderCardTabs not found")
	}
	j := strings.Index(html[i:], "\n}")
	body := html[i : i+j]
	for _, want := range []string{
		`data-tab="pinned"`,
		"pinnedCount",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("renderCardTabs missing %q", want)
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
func TestPinnedFilterAndOrder(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	i := strings.Index(html, "function renderCards()")
	j := strings.Index(html[i:], "document.title =")
	body := html[i : i+j]
	if !strings.Contains(body, `cardTab === "pinned"`) {
		t.Error("renderCards must special-case the pinned tab")
	}
	if !strings.Contains(html, "function pinnedOrder(") {
		t.Error("a pinnedOrder helper (attention-first, then pin order) is expected")
	}
}

// TestPinnedActionAndFlag asserts the swipe/hover action row leads with a
// pin/unpin button (leftmost), and a folded card shows a pin flag when pinned.
func TestPinnedActionAndFlag(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	i := strings.Index(html, `<div class="actions">`)
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
	// Click handler wires the action.
	if !strings.Contains(html, "data-pin-action") ||
		!strings.Contains(html, `k: isPinned(id) ? "unpin" : "pin"`) {
		t.Error("pin action handler must toggle pin/unpin via uiMutate")
	}
}

// TestPinnedDragReorder asserts drag-to-reorder within the Pinned tab persists
// order via a pin-order op, and is poll-safe (a drag in progress suppresses the
// list rebuild).
func TestPinnedDragReorder(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	for _, want := range []string{
		`k: "pin-order"`,
		"pinDragging",      // the in-progress-drag guard
		`draggable="true"`, // pinned cards are draggable
	} {
		if !strings.Contains(html, want) {
			t.Errorf("pinned drag reorder missing %q", want)
		}
	}
	// Poll-safe: renderCards must bail while a pin drag is in progress.
	if !strings.Contains(html, "if (pinDragging) return;") {
		t.Error("renderCards must not rebuild the list mid-drag (poll-safe)")
	}
}

// TestSearchFeed asserts the G4 grouped feed wiring: the input drives a
// debounced fetch of /api/search with an AbortController (so a superseded query
// is dropped) and a client-side min-length gate, and the response renders as
// per-chat groups with headers (agent, title, lane swatch) and hits.
func TestSearchFeed(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	for _, want := range []string{
		"function runSearch(",
		"function renderSearchFeed(",
		"function searchGroupHTML(",
		"function searchHitHTML(",
		"new AbortController()",
		`fetch("/api/search?q=" + encodeURIComponent(`,
		`$("#searchinput").addEventListener("input"`,
		"const SEARCH_MIN =",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("search feed wiring missing %q", want)
		}
	}
	// Min-length gate (mirrors the server's minSearchQuery) and a superseded-
	// query guard so out-of-order responses never overwrite a newer render.
	if !strings.Contains(html, "< SEARCH_MIN") {
		t.Error("runSearch must gate on the client-side min query length")
	}
	// The client query-length cap must mirror the server's maxSearchQuery so a
	// pasted over-long query stores and fetches only the prefix the server searches.
	if want := fmt.Sprintf("const SEARCH_MAX = %d;", maxSearchQuery); !strings.Contains(html, want) {
		t.Errorf("SEARCH_MAX must mirror the server cap: expected %q", want)
	}
	if !strings.Contains(html, "searchSeq") {
		t.Error("a sequence guard is expected so a stale response can't clobber a newer one")
	}
	// The debounced input handler must not fetch on every keystroke.
	if !strings.Contains(html, "clearTimeout(searchDebounce)") {
		t.Error("the search input must be debounced (clearTimeout(searchDebounce))")
	}
	// G4 renders the feed only; the action bar + jumps are G5, so a hit is not
	// yet wired to a click handler.
	if strings.Contains(html, `dataset.searchHit` /* placeholder for a G5-only handler */) {
		t.Error("G4 should not wire hit clicks yet (action bar is G5)")
	}
}

// TestSearchExcerptEscaping runs searchHitHTML under node with an esc stub to
// prove the excerpt escapes before/after and wraps ONLY the match in <mark>.
// The server sends raw spans (never HTML-escaped) — a double-escape would show
// literal &lt; to the user and an unescaped span would be an injection.
func TestSearchExcerptEscaping(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JS execution check")
	}
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	start := strings.Index(html, "function searchHitHTML(")
	if start < 0 {
		t.Fatal("could not locate searchHitHTML in web/index.html")
	}
	end := strings.Index(html[start:], "function searchGroupHTML(")
	if end < 0 {
		t.Fatal("could not locate the end of searchHitHTML in web/index.html")
	}
	script := `function esc(s){ return String(s ?? "").replace(/&/g,"&amp;").replace(/</g,"&lt;").replace(/>/g,"&gt;"); }` + "\n" +
		html[start:start+end] + `
const assert = require("assert");
const out = searchHitHTML({ before: "a <b>", match: "x&y", after: "</b> z", time: "2026-07-26T10:00:00" });
assert.ok(out.includes("a &lt;b&gt;<mark>x&amp;y</mark>&lt;/b&gt; z"),
  "excerpt must escape before/after and wrap only the match in <mark>; got: " + out);
assert.ok(!out.includes("<b>") && !out.includes("</b>"),
  "raw HTML from the excerpt must never survive into the feed; got: " + out);
`
	f := filepath.Join(t.TempDir(), "searchhit.js")
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
// add-note keeps the overlay open.
func TestSearchActionBar(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	for _, want := range []string{
		"function searchHitActions(",
		"function doSearchAction(",
		"function toggleHitBar(",
		`$("#searchfeed").addEventListener("click"`,
		"data-sact",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("search action bar wiring missing %q", want)
		}
	}
	// fork is parented to the hit's node, not the currently-open chat — so
	// forkFromTurn must take an explicit parent (default stays sel for callers).
	if !strings.Contains(html, "function forkFromTurn(text, parent)") {
		t.Error("forkFromTurn must accept an explicit parent so search can fork from the hit's node")
	}
	if !strings.Contains(html, "ncParent = parent || sel;") {
		t.Error("forkFromTurn should default the parent to sel to preserve existing callers")
	}
	// show-to-chat's live/notes destination is the shared jumpToHitChat helper:
	// it leaves the overlay and jumps via pendingJump.
	j := strings.Index(html, "function jumpToHitChat(")
	if j < 0 {
		t.Fatal("jumpToHitChat not found")
	}
	jump := html[j : j+strings.Index(html[j:], "\n}\n")]
	if !strings.Contains(jump, "closeSearch()") || !strings.Contains(jump, "pendingJump =") {
		t.Error("show-to-chat must exit the overlay and jump to the turn")
	}
	// Isolate doSearchAction to assert per-action behaviour.
	i := strings.Index(html, "function doSearchAction(")
	if i < 0 {
		t.Fatal("doSearchAction not found")
	}
	body := html[i : i+strings.Index(html[i:], "\n}\n")]
	if !strings.Contains(body, "jumpToHitChat(") {
		t.Error("show action must route live/notes hits through jumpToHitChat")
	}
	// fork: leaves the overlay (before opening the main-view sheet) and reuses
	// forkFromTurn parented to the hit's node.
	if !strings.Contains(body, "closeSearch()") || !strings.Contains(body, "forkFromTurn(") {
		t.Error("fork action must exit the overlay and reuse forkFromTurn")
	}
	// add-note: files a note op and, being the "stays open" action, its branch
	// must NOT contain a closeSearch.
	nb := body[strings.Index(body, `a === "bookmark"`):]
	if !strings.Contains(nb, `k: "bookmark-add"`) {
		t.Error("add-note must file a note-add op")
	}
	if strings.Contains(nb, "closeSearch()") {
		t.Error("add-note must keep the overlay open (no closeSearch in its branch)")
	}
}

// TestSearchHitActionsAdaptive runs searchHitActions under node to prove the
// per-hit action set is adaptive: every hit can be shown, only a live+forkable
// hit forks from the feed bar (archived fork lives inside the read-only view,
// where the launch config is), and notes never fork.
func TestSearchHitActionsAdaptive(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JS execution check")
	}
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	start := strings.Index(html, "function searchHitActions(")
	if start < 0 {
		t.Fatal("searchHitActions not found")
	}
	end := strings.Index(html[start:], "\n}\n")
	if end < 0 {
		t.Fatal("end of searchHitActions not found")
	}
	script := html[start:start+end] + `
}
const assert = require("assert");
const eq = (a, b) => assert.deepStrictEqual(a, b);
eq(searchHitActions("live", true),      ["show", "fork", "bookmark"]);
eq(searchHitActions("live", false),     ["show", "bookmark"]);
eq(searchHitActions("bookmarks", false),    ["show", "bookmark"]);
eq(searchHitActions("bookmarks", true),     ["show", "bookmark"]);     // notes never fork
eq(searchHitActions("archived", true),  ["show", "bookmark"]);     // fork is in the read-only view
eq(searchHitActions("archived", false), ["show", "bookmark"]);
// An asset filename hit is show-to-chat ONLY — no fork, no add-note — even in a
// live, forkable chat (spec §"Session assets").
eq(searchHitActions("live", true, "asset"),     ["show"]);
eq(searchHitActions("archived", true, "asset"), ["show"]);
// A normal (non-asset) role in a forkable live chat keeps the full set.
eq(searchHitActions("live", true, "user"),      ["show", "fork", "bookmark"]);
eq(searchHitActions("live", true, "assistant"), ["show", "fork", "bookmark"]);
`
	f := filepath.Join(t.TempDir(), "hitactions.js")
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
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	// A real modal, like the search overlay.
	for _, want := range []string{
		`id="archivedview"`,
		`role="dialog"`,
		"function openArchived(",
		"function closeArchived(",
		"function renderArchived(",
		`fetch("/api/archived?uid="`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("archived view missing %q", want)
		}
	}
	// Archived show-to-chat routes into the read-only surface (not a toast).
	i := strings.Index(html, "function doSearchAction(")
	body := html[i : i+strings.Index(html[i:], "\n}\n")]
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
	// Fork from the archived view seeds the launch config explicitly (there is
	// no live parent node), so prepareLaunchConfig must accept an explicit config.
	if !strings.Contains(html, "function forkFromArchived(") {
		t.Error("archived fork entry point missing")
	}
	if !strings.Contains(html, "function prepareLaunchConfig(cfg)") ||
		!strings.Contains(html, "const p = cfg || nodeById(ncParent);") {
		t.Error("prepareLaunchConfig must take an explicit config so archived fork can seed without a live node")
	}
	// The anchored turn is marked, and truncation is surfaced honestly.
	if !strings.Contains(html, "anchor") || !strings.Contains(html, "before_truncated") {
		t.Error("archived render must mark the anchor turn and honor truncation flags")
	}
}

// TestSearchRecents covers G6: recent searches are kept client-side (localStorage,
// like drafts), deduped most-recent-first, gated by the same min length as search,
// hard-capped, and only recorded when the supervisor acts on a hit. It runs the
// real recents helpers under node with a localStorage + esc stub.
func TestSearchRecents(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)

	// Wiring: a query is recorded on hit action (not per keystroke), tapping a
	// recent re-runs it, and Clear wipes the history.
	for _, want := range []string{
		"function recordRecent(",
		"function recentsHTML(",
		"function clearRecents(",
		`recordRecent($("#searchinput").value)`, // recorded inside doSearchAction
		`data-recent=`,
		`e.target.closest("#recentsclear")`,
		"const RECENTS_CAP =",
		"const RECENTS_SHOW =",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("recents wiring missing %q", want)
		}
	}
	// recordRecent must gate on the same minimum as search (no 1-char noise).
	if !strings.Contains(html, "if (query.length < SEARCH_MIN) return;") {
		t.Error("recordRecent must reuse the SEARCH_MIN gate")
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JS execution check")
	}
	start := strings.Index(html, "const RECENTS_KEY =")
	end := strings.Index(html, "const searchBlank =")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("could not locate the recents helper block in web/index.html")
	}
	script := `
const store = new Map();
const localStorage = {
  getItem: k => store.has(k) ? store.get(k) : null,
  setItem: (k, v) => store.set(k, String(v)),
  removeItem: k => store.delete(k),
};
function esc(s){ return String(s ?? "").replace(/&/g,"&amp;").replace(/</g,"&lt;").replace(/>/g,"&gt;"); }
const SEARCH_MIN = 2;
const SEARCH_MAX = 128;
` + html[start:end] + `
const assert = require("assert");

// query-length cap: a pasted over-long query is stored only up to SEARCH_MAX
// (the server searches just that prefix), never as a multi-KB string.
recordRecent("z".repeat(SEARCH_MAX + 50));
assert.strictEqual(loadRecents()[0].length, SEARCH_MAX,
  "an over-long recent must be capped to SEARCH_MAX; got " + loadRecents()[0].length);
store.clear();

// gate: a sub-minimum query is never recorded.
recordRecent("a");
assert.deepStrictEqual(loadRecents(), [], "1-char query must not be recorded");

// dedup + move-to-front, most-recent-first.
recordRecent("auth");
recordRecent(" bug ");            // trimmed
recordRecent("auth");             // repeat → jumps back to front, no duplicate
assert.deepStrictEqual(loadRecents(), ["auth", "bug"],
  "recents must dedup and move the repeat to the front; got " + JSON.stringify(loadRecents()));

// hard cap at RECENTS_CAP.
for (let i = 0; i < 20; i++) recordRecent("q" + i);
assert.ok(loadRecents().length <= RECENTS_CAP,
  "recents must be capped at " + RECENTS_CAP + "; got " + loadRecents().length);
assert.strictEqual(loadRecents()[0], "q19", "newest query must be first");

// blank-state render shows at most RECENTS_SHOW and escapes the query text.
store.clear();
recordRecent("<img src=x>");
recordRecent("hello");
const html2 = recentsHTML();
assert.ok(html2.includes("&lt;img src=x&gt;") && !html2.includes("<img src=x>"),
  "recent query text must be escaped; got " + html2);
assert.ok(html2.includes('data-recent="hello"'), "recent must carry its query for re-run");

// clear wipes everything → blank state renders nothing (falls back to the prompt).
clearRecents();
assert.deepStrictEqual(loadRecents(), [], "clearRecents must empty the history");
assert.strictEqual(recentsHTML(), "", "no recents → empty render (prompt fallback)");
`
	f := filepath.Join(t.TempDir(), "recents.js")
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
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JS execution check")
	}
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	start := strings.Index(html, "const BOOKMARK_CLAMP_PX")
	if start < 0 {
		t.Fatal("could not locate BOOKMARK_CLAMP_PX in web/index.html")
	}
	end := strings.Index(html[start:], "function renderBookmarksPane(")
	if end < 0 {
		t.Fatal("could not locate the end of the clamp core in web/index.html")
	}
	script := html[start:start+end] + `
const assert = require("assert");
// A short bubble (below the cap) never clamps and never shows the toggle,
// whatever its (irrelevant) expanded flag — this is the ~85% common case.
assert.deepStrictEqual(bookmarkClampState(120, false), { clamped: false, showBtn: false, label: "Show more" }, "short/collapsed: no clamp, no button");
assert.deepStrictEqual(bookmarkClampState(120, true),  { clamped: false, showBtn: false, label: "Show less" }, "short/expanded: still no clamp, no button");
// A tall bubble, collapsed (default), clamps and offers "Show more".
assert.deepStrictEqual(bookmarkClampState(900, false), { clamped: true,  showBtn: true,  label: "Show more" }, "tall/collapsed: clamp + Show more");
// The same tall bubble once the user expanded it: not clamped, toggle reads back.
assert.deepStrictEqual(bookmarkClampState(900, true),  { clamped: false, showBtn: true,  label: "Show less" }, "tall/expanded: no clamp, Show less");
// The boundary is exclusive: exactly at the cap is not "tall".
assert.deepStrictEqual(bookmarkClampState(BOOKMARK_CLAMP_PX, false), { clamped: false, showBtn: false, label: "Show more" }, "at the cap is not tall");
`
	f := filepath.Join(t.TempDir(), "noteclamp.js")
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
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	for _, want := range []string{
		"let expandedBookmarks = new Set();", // ephemeral, sibling of openBookmarkT
		"const BOOKMARK_CLAMP_PX",            // the threshold constant
		"function bookmarkClampState(",       // the decision core
		"data-nmore",                         // the per-bubble toggle button
		`class="nmore"`,                      // its markup in the note template
		".bookmark .nbubble.clamped",         // the CSS clamp
	} {
		if !strings.Contains(html, want) {
			t.Errorf("note collapse wiring missing %q", want)
		}
	}
	// The toggle handler is a distinct branch keyed on data-nmore. It must sit
	// in the #bookmarkspane click delegate BEFORE the generic ".bookmark" tap branch,
	// or clicking "Show more" would also open the action bar.
	dele := strings.Index(html, `$("#bookmarkspane").addEventListener("click"`)
	if dele < 0 {
		t.Fatal("could not locate the #bookmarkspane click delegate")
	}
	body := html[dele:]
	nmore := strings.Index(body, "[data-nmore]")
	noteTap := strings.Index(body, `closest(".bookmark")`)
	if nmore < 0 {
		t.Fatal("the notespane delegate has no data-nmore branch")
	}
	if !(nmore < noteTap) {
		t.Error("the data-nmore branch must precede the .bookmark tap branch so Show more does not open the action bar")
	}
	// Ephemerality: the expand state is never persisted. No localStorage key and
	// no ui op may touch expandedBookmarks.
	if regexp.MustCompile(`localStorage[^;\n]*expandedBookmarks|expandedBookmarks[^;\n]*localStorage`).MatchString(html) {
		t.Error("expandedBookmarks must stay ephemeral — no localStorage persistence")
	}
	// The toggle mutates the DOM directly and returns; it must not call
	// renderBookmarksPane (which would be a needless rebuild) inside its branch.
	branch := body[nmore:]
	if endB := strings.Index(branch, "return;"); endB > 0 {
		if strings.Contains(branch[:endB], "renderBookmarksPane(") {
			t.Error("the data-nmore toggle must not rebuild the pane — flip the class in place")
		}
	}
}

// A past station opens its /clear-closed segment as history bubbles. Those must
// be tappable like live bubbles — reveal the timestamp row and the action bar —
// not inert. The tap machinery keys off a uniform data-bk addressed against a
// bubbleTurns map, so both live ("i:…") and history ("h:…") bubbles resolve.
func TestHistoryBubblesAreTappable(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	// Every rendered bubble carries the uniform tap key.
	if strings.Count(html, "data-bk=") < 2 {
		t.Error("both live and history bubble templates must carry a data-bk tap key")
	}
	// History bubbles feed the same turn store the action row reads from.
	if !strings.Contains(html, "bubbleTurns[") {
		t.Error("history bubbles must register their turn in the bubbleTurns lookup")
	}
	// The tap delegate must address bubbles by data-bk, not the live-only data-i.
	msgsDele := strings.Index(html, `$("#msgs").addEventListener("click"`)
	if msgsDele < 0 {
		t.Fatal("could not locate the #msgs click delegate")
	}
	body := html[msgsDele:]
	if !strings.Contains(body, "turnEl.dataset.bk") {
		t.Error("the bubble tap branch must key off data-bk so history bubbles resolve")
	}
	// The action-row renderer must resolve its turn from the shared store, not
	// the live-only lastTurns array, so it works for a history bubble too.
	rowStart := strings.Index(html, "function renderBubbleActions(")
	if rowStart < 0 {
		t.Fatal("renderBubbleActions not found")
	}
	rowEnd := strings.Index(html[rowStart:], "turnEl.after(row);")
	fn := html[rowStart : rowStart+rowEnd]
	if !strings.Contains(fn, "bubbleTurns[tappedTurn]") {
		t.Error("renderBubbleActions must resolve the tapped turn from bubbleTurns")
	}
	if strings.Contains(fn, `data-i="${tappedTurn}"`) {
		t.Error("renderBubbleActions must address the tapped bubble by data-bk, not data-i")
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
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	// The bubble templates must build their title from bubbleTitle, not a bare
	// ternary on the role.
	if strings.Contains(html, `title="${t.role === "user" ? "you" : "agent"}"`) {
		t.Error("bubble title should come from bubbleTitle(), not a bare role ternary")
	}
	start := strings.Index(html, "function fmtWhen(")
	if start < 0 {
		t.Fatal("could not locate fmtWhen in web/index.html")
	}
	btStart := strings.Index(html, "function bubbleTitle(")
	if btStart < 0 {
		t.Fatal("could not locate bubbleTitle in web/index.html")
	}
	btEnd := strings.Index(html[btStart:], "\n}")
	fmtEnd := strings.Index(html[start:], "\n}")
	script := html[start:start+fmtEnd+2] + "\n" + html[btStart:btStart+btEnd+2] + `
const assert = require("assert");
const t = "2026-07-24T09:05:00";
const w = fmtWhen(t);
assert.strictEqual(bubbleTitle("user", t), "you — sent " + w);
assert.strictEqual(bubbleTitle("assistant", t), "agent — sent " + w);
assert.strictEqual(bubbleTitle("user", ""), "you");
assert.strictEqual(bubbleTitle("assistant", null), "agent");
`
	f := filepath.Join(t.TempDir(), "bubble-title.js")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, f).CombinedOutput(); err != nil {
		t.Fatalf("bubbleTitle broken: %v\n%s", err, out)
	}
}

// --- Stage 2: bookmark inbox → "Use in note" → embedded references ---

// provLabel builds an embedded reference's provenance line from its snapshot
// parts, dropping the empty ones so a reference with only a timestamp still
// reads cleanly (design: an embedded reference retains lane/station/speaker/time
// provenance, but a manual capture may lack station or speaker).
func TestWebReferenceProvenanceLabel(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JS execution check")
	}
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	m := regexp.MustCompile(`function provLabel\([^)]*\)\{[^}]*\}`).FindString(html)
	if m == "" {
		t.Fatal("could not locate provLabel in web/index.html")
	}
	script := m + `
const assert = require("assert");
assert.strictEqual(provLabel("Alpha", "agent", "Jul 28"), "Alpha · agent · Jul 28");
assert.strictEqual(provLabel("", "", "Jul 28"), "Jul 28");
assert.strictEqual(provLabel("Alpha", "", ""), "Alpha");
assert.strictEqual(provLabel("", "", ""), "");
`
	f := filepath.Join(t.TempDir(), "provlabel.js")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, f).CombinedOutput(); err != nil {
		t.Fatalf("provLabel broken: %v\n%s", err, out)
	}
}

// A section must render its embedded references (provenance chip + snapshot text
// + jump/copy/trash actions) so a self-contained reference shows without any
// dependency on the source node still existing.
func TestWebSectionRendersReferences(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	for _, want := range []string{
		"function referenceHTML(", // per-reference builder
		`(s.references || [])`,    // sectionHTML iterates the section's references
		`data-refact="jump"`,      // jump back to the source chat
		`data-refact="copy"`,      // copy the snapshot text
		`data-refact="trash"`,     // trash removes only this reference
	} {
		if !strings.Contains(html, want) {
			t.Errorf("section reference rendering missing %q", want)
		}
	}
}

// "Use in note" is the capture→synthesis bridge. It must be an action on the
// compact Bookmark pane (the inbox on every device) and route through a shared
// placement path that posts the reference to the section endpoint.
func TestWebUseInNoteWiring(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	for _, want := range []string{
		`data-bmact="note"`,        // Use-in-note action in the Bookmark-pane action row
		"function startPlacement(", // enters placement mode holding the note
		`classList.add("placing")`, // placement mode drives the add-here affordances
		`data-addhere`,             // per-section "add capture here" target
		`/references`,              // posts to the section references endpoint
	} {
		if !strings.Contains(html, want) {
			t.Errorf("use-in-note wiring missing %q", want)
		}
	}
}

// Reference jump-back and the notes-pane jump must share one resolver so both
// survive /clear seams, rotation, and node deletion identically (live node →
// pendingJump; deleted-and-archived → openArchived by uid).
func TestWebSharedJumpResolver(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	if !strings.Contains(html, "function jumpToChatAddress(") {
		t.Error("shared jumpToChatAddress resolver not defined")
	}
	// The notes pane must delegate to it rather than keep a private copy.
	if strings.Count(html, "jumpToChatAddress(") < 2 {
		t.Error("jumpToChatAddress must be called from both the notes pane and references")
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
	html := mustReadIndex(t)
	ws := cssBlock(t, html, "#notesworkspace {")
	if !strings.Contains(ws, "var(--sbh)") {
		t.Errorf("#notesworkspace must offset below the status bar (var(--sbh)), like #map; got %q", ws)
	}
	panel := cssBlock(t, html, "#wspanel {")
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
	html := mustReadIndex(t)
	css := cssBlock(t, html, "#wsinbox, #wsnav {")
	if !strings.Contains(css, "340px") {
		t.Errorf("#wsinbox, #wsnav must be 340px (>=900px) to match the app panes; got %q", css)
	}
}

// Follow-up (2026-07-30 iPad review): the workspace title ("Notes") must render
// at the same size/weight as the app pane headings ("Activities", "Bookmarks").
func TestWorkspaceTitleMatchesPaneHeadings(t *testing.T) {
	html := mustReadIndex(t)
	pane := cssBlock(t, html, ".phead h2 {")
	ws := cssBlock(t, html, "#wstopbar h2 {")
	for _, want := range []string{"font-size: 22px", "font-weight: 700"} {
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
	html := mustReadIndex(t)
	chat := cssBlock(t, html, "#chathead {")
	if !strings.Contains(chat, "padding: 10px 16px 10px") {
		t.Fatalf("#chathead vertical padding baseline changed; got %q", chat)
	}
	ws := cssBlock(t, html, "#wstopbar {")
	if !strings.Contains(ws, "padding: 10px ") {
		t.Errorf("#wstopbar must use 10px top/bottom padding to match #chathead; got %q", ws)
	}
}

// Item 4 (2026-07-30 iPad review): deleting the open note must clear the editor
// header so no stale title lingers. #wsnotehead sets display:flex, which beats
// the UA [hidden] rule, so an explicit hidden override is required for
// head.hidden = true to actually hide.
func TestWorkspaceNoteHeadHidesWhenEmpty(t *testing.T) {
	html := mustReadIndex(t)
	css := cssBlock(t, html, "#wsnotehead[hidden] {")
	if !strings.Contains(css, "display: none") {
		t.Errorf("#wsnotehead[hidden] must force display:none so a deleted note's title clears; got %q", css)
	}
}

// Items 5 & 6 (2026-07-30 iPad review): both empty states centre on both axes —
// the active-note placeholder inside #wsnote and the note-card empty state
// inside #wscards.
func TestWorkspaceEmptyStatesCentered(t *testing.T) {
	html := mustReadIndex(t)
	for _, sel := range []string{"#wsnoteempty {", "#wscards .empty {"} {
		css := cssBlock(t, html, sel)
		if !strings.Contains(css, "align-items: center") || !strings.Contains(css, "justify-content: center") {
			t.Errorf("%s must centre on both axes (flex align+justify center); got %q", sel, css)
		}
	}
}

// Items 2 & 5: the left capture zone reuses the compact Notes UI — lane tabs and
// the same auto-collapse ("Show more"/"Show less") for tall bubbles.
func TestWorkspaceInboxHasTabsAndClamp(t *testing.T) {
	html := mustReadIndex(t)
	for _, want := range []string{
		`id="wsinboxtabs"`,             // the lane-tab row exists in markup
		"data-wsitab",                  // per-tab selector
		"data-wsimore",                 // per-bubble show-more toggle
		".wsibubble.clamped",           // the CSS clamp on tall capture bubbles
		"function applyWsInboxClamps(", // the measure/apply pass
		"expandedWsInbox",              // ephemeral expand state, sibling of expandedBookmarks
	} {
		if !strings.Contains(html, want) {
			t.Errorf("workspace inbox tabs/clamp wiring missing %q", want)
		}
	}
	// The show-more toggle branch must precede the "Use in note" branch so a tap
	// on Show more never enters placement mode.
	dele := strings.Index(html, `$("#wsinboxlist").addEventListener("click"`)
	if dele < 0 {
		t.Fatal("could not locate the #wsinboxlist click delegate")
	}
	body := html[dele:]
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
	for _, want := range []string{
		">bookmark</button>",         // bubble action label
		"<h2>Bookmarks</h2>",         // captures pane heading
		"Use in note", "Delete note", // synthesis-doc action + menu
		"Add bookmark here", "Placing bookmark:",
		"New note", "Note title", "Pick a note, or make one with +",
		"No notes yet — make one with +", // notes (memo) card empty state
	} {
		if !strings.Contains(html, want) {
			t.Errorf("expected new vocabulary %q not found", want)
		}
	}
	// a short terminology note must remain, documenting the code==UI mapping and
	// the one live gotcha: the bottom-sheet `.sheet` primitive is NOT a Note.
	if !strings.Contains(html, "TERMINOLOGY (code == UI") {
		t.Error("the terminology comment (code==UI mapping + bottom-sheet homonym warning) must be present")
	}
}

// The captures pane became "Bookmarks": its toggle uses a book-bookmark glyph,
// and the "use in note" action (formerly the pen-to-square that read as "Edit")
// now uses a paperclip, sitting second-to-last in the note action row (before
// delete). Icons are inlined SVG per the repo convention — no FA dependency.
func TestBookmarkIconsAndActionOrder(t *testing.T) {
	html := mustReadIndex(t)
	for _, want := range []string{"const ICON_BOOKMARK", "const ICON_CLIP"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing inlined icon constant %q", want)
		}
	}
	// the chat-area toggle (#bookmarksbtn) is the ‹/› chevron mirroring pane state;
	// the bookmark glyph now lives on the Bookmarks pane's top-right button.
	tog := sliceBetween(t, html, "function renderBookmarksToggle(", "\n}\n")
	if strings.Contains(tog, "ICON_BOOKMARK") {
		t.Error("the chat-area toggle (#bookmarksbtn) must be a chevron, not ICON_BOOKMARK")
	}
	if !strings.Contains(tog, `"&#8249;"`) {
		t.Error("the chat-area toggle (#bookmarksbtn) must restore the ‹ chevron when closed")
	}
	if !strings.Contains(html, `$("#notesbtn").innerHTML = ICON_BOOKMARK;`) {
		t.Error("the Bookmarks pane top-right button (#notesbtn) must use ICON_BOOKMARK")
	}
	// in the note action row, the paperclip (use-in-note) sits just before delete
	row := sliceBetween(t, html, `<div class="bookmarkactions">`, "</div>")
	clip := strings.Index(row, `data-bmact="note"`)
	del := strings.Index(row, `data-bmact="del"`)
	if clip < 0 || del < 0 {
		t.Fatal("note action row must contain both the use-in-note and delete actions")
	}
	if !(clip < del) {
		t.Error("the use-in-note (paperclip) action must sit before delete (second-to-last)")
	}
	// and that action renders the paperclip, not the old pen-to-square (SHEETS)
	act := row[clip:]
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
	html := mustReadIndex(t)
	// find the global touchend gesture handler
	i := strings.Index(html, `document.addEventListener("touchend"`)
	if i < 0 {
		t.Fatal("could not locate the touchend gesture handler")
	}
	body := html[i : i+strings.Index(html[i:], "}, { passive: true });")]
	// a left-going swipe while the Bookmarks pane is open must open the workspace
	if !regexp.MustCompile(`dx < 0 && bookmarksOpen\)\s*openWorkspace\(\)`).MatchString(body) {
		t.Error("swipe R->L on the open Bookmarks pane must call openWorkspace() (Notes overview)")
	}
}

// Item 4: the capture list must not scroll horizontally (hard to use on touch);
// long code fences scroll inside their own <pre>, the list itself does not.
func TestWorkspaceInboxNoHorizontalScroll(t *testing.T) {
	html := mustReadIndex(t)
	css := cssBlock(t, html, "#wsinboxlist {")
	if !strings.Contains(css, "overflow-x: hidden") && !strings.Contains(css, "overflow-x: clip") {
		t.Errorf("#wsinboxlist must suppress horizontal scroll; got %q", css)
	}
}

// Item 6/9: Note cards reorder by drag-and-drop (the proven pinned-card gesture),
// not by no-op up/down arrows; the redundant reorder-mode toggle is gone.
func TestWorkspaceNoteCardsDragReorder(t *testing.T) {
	html := mustReadIndex(t)
	for _, gone := range []string{
		"data-wsmove",    // the old per-card up/down buttons
		`id="wsreorder"`, // the old reorder-mode toggle
		"function wsMove(",
	} {
		if strings.Contains(html, gone) {
			t.Errorf("obsolete reorder affordance %q must be removed", gone)
		}
	}
	if !strings.Contains(html, `$("#wscards").addEventListener("dragstart"`) {
		t.Error("Note cards must be reorderable by drag (dragstart handler on #wscards)")
	}
	// The card template must be draggable.
	tmpl := html[strings.Index(html, "function renderWsCards("):]
	tmpl = tmpl[:strings.Index(tmpl, "\n}")]
	if !strings.Contains(tmpl, `draggable="true"`) {
		t.Error("the .wscard template must set draggable=\"true\"")
	}
}

// Item 10: section bodies, embedded references, and capture bubbles render
// Markdown with the same element styling the chat bubbles use — headings, lists,
// code, and quotes must be styled, not flat text.
func TestWorkspaceMarkdownStyled(t *testing.T) {
	html := mustReadIndex(t)
	// A grouped selector must style headings/lists inside the workspace md
	// containers (any of the three qualifies the rule).
	for _, want := range []string{
		".wssecrender h1",
		".wssecrender ul",
		".wssecrender pre",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("workspace markdown styling missing selector %q", want)
		}
	}
}

// Items 12 & 13: a section already at the top cannot move up, one at the bottom
// cannot move down — no wrap-around. sectionSwapPlan is the pure decision core,
// operating on the visually sorted order (not array position).
func TestWorkspaceSectionMoveBoundary(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JS execution check")
	}
	html := mustReadIndex(t)
	fn := regexp.MustCompile(`(?s)function sectionSwapPlan\(.*?\n\}`).FindString(html)
	if fn == "" {
		t.Fatal("could not locate sectionSwapPlan in web/index.html")
	}
	script := fn + `
const assert = require("assert");
// order values deliberately out of array order to prove it sorts first
const secs = [ {id:"b",order:1}, {id:"a",order:0}, {id:"c",order:2} ];
assert.strictEqual(sectionSwapPlan(secs, "a", "up"), null, "top cannot move up");
assert.strictEqual(sectionSwapPlan(secs, "c", "down"), null, "bottom cannot move down");
const up = sectionSwapPlan(secs, "b", "up");
assert.deepStrictEqual({a:up.a, b:up.b}, {a:"b", b:"a"}, "middle up swaps with the one above");
const dn = sectionSwapPlan(secs, "b", "down");
assert.deepStrictEqual({a:dn.a, b:dn.b}, {a:"b", b:"c"}, "middle down swaps with the one below");
assert.strictEqual(sectionSwapPlan(secs, "zzz", "up"), null, "unknown id is a no-op");
`
	f := filepath.Join(t.TempDir(), "swapplan.js")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, f).CombinedOutput(); err != nil {
		t.Fatalf("sectionSwapPlan boundary logic broken: %v\n%s", err, out)
	}
}

// Item 14: an embedded capture inside a section auto-collapses like a Notes
// bubble, with its own Show more/less toggle.
func TestWorkspaceReferenceClamp(t *testing.T) {
	html := mustReadIndex(t)
	for _, want := range []string{
		"data-refmore",
		".wsrefbody.clamped",
		"function applyRefClamps(",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("embedded-reference clamp wiring missing %q", want)
		}
	}
}

// Item 15: after editing a section's body, its embedded references must still
// render. The rendered-body rebuild must go through a helper that re-emits the
// references, not overwrite the body with the prose alone.
func TestWorkspaceBodyEditPreservesReferences(t *testing.T) {
	html := mustReadIndex(t)
	if !strings.Contains(html, "function sectionBodyInner(") {
		t.Fatal("expected a sectionBodyInner helper that emits body + references together")
	}
	// startBodyEdit's blur must rebuild via the helper, never with a bare render div.
	be := html[strings.Index(html, "function startBodyEdit("):]
	be = be[:strings.Index(be, "\n}\n")]
	if !strings.Contains(be, "sectionBodyInner(") {
		t.Error("startBodyEdit must restore the body via sectionBodyInner so references survive an edit")
	}
}

// Item 16: the note's overflow (three-dots) menu must actually open — the
// document-level close handler was firing on the same click. The handler stops
// propagation, and the menu offers rename + delete.
func TestWorkspaceNoteMenuOpens(t *testing.T) {
	html := mustReadIndex(t)
	h := html[strings.Index(html, `$("#wsnotemenu").addEventListener("click"`):]
	h = h[:strings.Index(h, "\n});")]
	if !strings.Contains(h, "stopPropagation") {
		t.Error("#wsnotemenu handler must stopPropagation so the document close handler doesn't kill the menu")
	}
	if !strings.Contains(h, "data-si=\"del\"") {
		t.Error("Note menu must offer delete")
	}
}

// Items 18-20: Enter commits a title edit; Escape cancels it (title, section
// title, section body) without collapsing the whole workspace. The editors stop
// Escape from bubbling to the workspace-close handler.
func TestWorkspaceEscCancelsEdit(t *testing.T) {
	html := mustReadIndex(t)
	// note-title keydown handles Enter (blur) and Escape (revert + stopPropagation)
	kd := html[strings.Index(html, `$("#wsnotetitle").addEventListener("keydown"`):]
	kd = kd[:strings.Index(kd, "\n});")]
	if !strings.Contains(kd, `"Enter"`) || !strings.Contains(kd, `"Escape"`) {
		t.Error("#wsnotetitle keydown must handle both Enter and Escape")
	}
	if !strings.Contains(kd, "stopPropagation") {
		t.Error("#wsnotetitle Escape must stopPropagation so the workspace does not close")
	}
	// section body editor cancels on Escape
	be := html[strings.Index(html, "function startBodyEdit("):]
	be = be[:strings.Index(be, "\n}\n")]
	if !strings.Contains(be, `"Escape"`) || !strings.Contains(be, "stopPropagation") {
		t.Error("startBodyEdit must cancel on Escape and stopPropagation")
	}
}

// Item 17: the wide (iPad/desktop) layout shows all three zones and hides the
// redundant back chevron. The multi-zone breakpoint reaches iPad portrait.
func TestWorkspaceWideLayout(t *testing.T) {
	html := mustReadIndex(t)
	if !strings.Contains(html, "min-width: 768px") {
		t.Error("multi-zone workspace breakpoint must reach iPad portrait (min-width: 768px)")
	}
	// #wsback is hidden in the wide layout (the X already closes the workspace).
	wide := html[strings.Index(html, "@media (min-width: 768px)"):]
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
// overtaken. Prove the core serialization guarantee under node.
func TestWorkspaceSaveSerializedPerField(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JS execution check")
	}
	html := mustReadIndex(t)
	start := strings.Index(html, "const wsSaveChains")
	if start < 0 {
		t.Fatal("could not locate wsSaveChains serialization core in web/index.html")
	}
	end := strings.Index(html[start:], "\nfunction wsEnqueue")
	if end < 0 {
		t.Fatal("could not locate wsEnqueue in web/index.html")
	}
	// grab through the end of wsEnqueue's body
	tail := html[start+end:]
	fnEnd := strings.Index(tail, "\n}\n")
	if fnEnd < 0 {
		t.Fatal("could not locate the end of wsEnqueue in web/index.html")
	}
	core := html[start : start+end+fnEnd+2]
	script := core + `
const assert = require("assert");
const order = [];
let resolveFirst;
// A slow first save on the field, then a revert on the SAME field. The revert
// thunk must not run until the first settles, and must complete last.
const first = wsEnqueue("k", () => new Promise(r => { resolveFirst = () => { order.push("edit"); r(); }; }));
const revert = wsEnqueue("k", () => { order.push("revert"); return Promise.resolve(); });
setTimeout(() => {
  assert.deepStrictEqual(order, [], "nothing may complete while the first save is still in flight — the revert must wait");
  resolveFirst();
  Promise.all([first, revert]).then(() => {
    assert.deepStrictEqual(order, ["edit", "revert"], "the revert must run only after the in-flight edit, so it lands last on the server");
    console.log("ok");
  }).catch(e => { console.error(e && e.message || e); process.exit(1); });
}, 10);
`
	f := filepath.Join(t.TempDir(), "wsserialize.js")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, f).CombinedOutput(); err != nil {
		t.Fatalf("per-field save serialization broken: %v\n%s", err, out)
	}
	// The revert sites must enqueue on the field's key (via wsPatchNow), not fire
	// an unkeyed immediate wsPatch that bypasses the chain.
	if !strings.Contains(html, "function wsPatchNow(") {
		t.Error("wsPatchNow (immediate keyed enqueue) must exist so reverts serialize behind in-flight saves")
	}
	for _, unkeyed := range []string{
		"wsPatch({ title: wsTitleOrig })",
		"wsPatch({ section: { id: secId, title: orig } })",
		"wsPatch({ section: { id: secId, body: orig } })",
	} {
		if strings.Contains(html, unkeyed) {
			t.Errorf("revert still uses an unkeyed wsPatch that bypasses per-field ordering: %q", unkeyed)
		}
	}
}

// Review finding 3: an anchored bookmark *comment* embedded into a note must
// inherit its parent bookmark's lane in the reference snapshot, exactly like the
// Bookmarks pane does. The snapshot path fed bookmarkLaneId an empty byT map, so
// an anchored comment lost its lane color and the note card missed that lane dot.
func TestWorkspaceReferenceSnapshotInheritsCommentLane(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping JS execution check")
	}
	html := mustReadIndex(t)
	laneFn := sliceBetween(t, html, "function bookmarkLaneId(", "\nfunction bookmarkSortKey(")
	snapFn := sliceBetween(t, html, "function bookmarkSnapshot(", "\nfunction bookmarkSource(")
	script := `
const UI = { bookmarks: [ { t: "1", lane: "lane-a" }, { t: "2", anchor: "1" } ] };
function nodeById(){ return null; }
function laneColor(id){ return id ? "#c-" + id : ""; }
` + laneFn + "\n" + snapFn + `
const assert = require("assert");
const comment = UI.bookmarks[1];   // anchored to note "1", carries no lane of its own
const snap = bookmarkSnapshot(comment);
assert.strictEqual(snap.lane, "#c-lane-a", "an anchored comment must inherit its parent note's lane color in the snapshot");
console.log("ok");
`
	f := filepath.Join(t.TempDir(), "wssnaplane.js")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, f).CombinedOutput(); err != nil {
		t.Fatalf("comment lane inheritance in reference snapshot broken: %v\n%s", err, out)
	}
}

// sliceBetween returns the text from the occurrence of start up to (but not
// including) the first occurrence of end after it.
// The new-activity effort menu is per-model where the CLI advertises it (codex
// via `codex debug models`): /api/agents now returns {models, efforts} per
// agent, the browser stashes the per-model effort menus, and fillEfforts uses
// the selected model's menu before the static per-agent EFFORTS fallback.
func TestPerModelEffortWiring(t *testing.T) {
	html := mustReadIndex(t)
	for _, want := range []string{
		"MODEL_EFFORTS", // per-agent → per-model effort store
		"info.efforts",  // populated from the /api/agents payload
		`$("#nc_model").addEventListener("change"`, // effort menu follows the model, not just the agent
	} {
		if !strings.Contains(html, want) {
			t.Errorf("per-model effort wiring missing %q", want)
		}
	}
	// fillEfforts must consult the selected model's menu first, then fall back
	// to the static per-agent EFFORTS list, and mark the model's default level.
	fe := sliceBetween(t, html, "function fillEfforts(", "\n}")
	if !strings.Contains(fe, "MODEL_EFFORTS") {
		t.Errorf("fillEfforts must consult per-model efforts: %q", fe)
	}
	if !strings.Contains(fe, "EFFORTS[") {
		t.Errorf("fillEfforts must keep the static per-agent EFFORTS fallback: %q", fe)
	}
	if !strings.Contains(fe, "(default)") {
		t.Errorf("fillEfforts should mark the model's default effort level: %q", fe)
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
	// the redundant caption is gone
	if strings.Contains(html, "No sections yet.") {
		t.Error(`the empty-section caption "No sections yet." must be dropped — it duplicates the Add section button`)
	}
	// the empty state is a centering wrapper holding the single add button
	esi := strings.Index(html, "class=\"wssecempty\"")
	if esi < 0 {
		t.Fatal("empty section state must render a .wssecempty centering wrapper")
	}
	// the wrapper must contain the add-section button (so wsAddSection still fires)
	wrap := html[esi:]
	end := strings.Index(wrap, "</div>")
	if end < 0 || !strings.Contains(wrap[:end+len("</div>")], "data-addsection") {
		t.Error(".wssecempty must contain the data-addsection button")
	}
	// the wrapper centers its button both axes and fills the pane so it sits
	// vertically centered, not pinned to the top
	css := cssBlock(t, html, ".wssecempty {")
	for _, want := range []string{"align-items: center", "justify-content: center"} {
		if !strings.Contains(css, want) {
			t.Errorf(".wssecempty must center its content (%s); got %q", want, css)
		}
	}
	if !strings.Contains(css, "min-height") {
		t.Errorf(".wssecempty must claim the pane height so the button centers vertically; got %q", css)
	}
}

// mustReadIndex / cssBlock: small helpers shared by the workspace tests.
func mustReadIndex(t *testing.T) string {
	t.Helper()
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	return string(b)
}

// cssBlock returns the text of the first CSS rule whose selector line contains
// marker, from that line to the closing brace.
func cssBlock(t *testing.T, html, marker string) string {
	t.Helper()
	i := strings.Index(html, marker)
	if i < 0 {
		t.Fatalf("could not locate CSS rule %q", marker)
	}
	end := strings.Index(html[i:], "}")
	if end < 0 {
		t.Fatalf("unterminated CSS rule %q", marker)
	}
	return html[i : i+end]
}
