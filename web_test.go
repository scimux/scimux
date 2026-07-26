package main

import (
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
	if !strings.Contains(html, "function prepareLaunchConfig(){\n  fillAgents(); fillModels();") {
		t.Error("prepareLaunchConfig no longer rebuilds the selectors on open — stale-config guard lost")
	}
}

func TestNotesToggleDirectionMatchesPaneState(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("read embedded web/index.html: %v", err)
	}
	html := string(b)
	if !strings.Contains(html, `id="notesbtn" aria-label="open notes pane">&#8249;</button>`) {
		t.Fatal("closed notes toggle should point left and announce opening the notes pane")
	}
	for _, want := range []string{
		`btn.innerHTML = notesOpen ? "&#8250;" : "&#8249;";`,
		`btn.setAttribute("aria-label", notesOpen ? "close notes pane" : "open notes pane");`,
		"renderNotesToggle();\n  renderNoteFlags();",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("notes toggle state sync missing %q", want)
		}
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
		`data-bact="note"`,
		`data-bact="copy"`,
	} {
		if !strings.Contains(row, want) {
			t.Errorf("bubble action row missing %q", want)
		}
	}
	if !(strings.Index(row, `data-bact="note"`) < strings.Index(row, `data-bact="copy"`)) {
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
		`let UI = { groups: [], archived: [], notes: [], lanes: [], pinned: [] };`,
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
		!strings.Contains(html, `idempotentOp = op => op.k === "note-add" || op.k === "note-del"`) {
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
