/* Characterization tests for web/js/map.js — pure Journeys decisions +
 * factory bind/destroy. Reuses lanes/map-model coverage; does not re-test
 * forkKind, laneColumnOrder, toggleMapSelection, or stop-chain algorithms. */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync, readdirSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import {
  MAP_TAB_KEY,
  MAP_FOLD_KEY,
  MAP_FOLD_KNOWN_KEY,
  MAP_FULL_KEY,
  MAP_FARE_KEY,
  ATTN_GLOW_DEF,
  mapIsVisible,
  resolveMapTab,
  mapRenderDecision,
  clearMapSelection,
  applyNewLaneFolds,
  unfoldFocusLane,
  toggleFoldId,
  chipTapPlan,
  selectedStopOf,
  laneChipStyle,
  mapTabsHTML,
  buildStackBlocks,
  stackMapSignature,
  wallMapSignature,
  attentionStationSVG,
  attentionHitSVG,
  terminalStationSVG,
  terminalCapSVG,
  forkCueHTML,
  stationRowHTML,
  createMapFeature,
  // V2-P3 lane heat
  TRACK_STROKE_BASE,
  TRACK_STROKE_MAX,
  HEAT_COST_CAP,
  heatEnabled,
  segmentTokenCost,
  heatStrokeWidth,
  heatOpacity,
  heatSegmentsFingerprint,
  gapTokenCost,
  wallLaneTrackSVG,
  // V2-P4 capsule + callout
  fareOverlayEnabled,
  fmtRideMs,
  fareCapsuleHTML,
  // V2-P5 fare ticket
  wallHeatOn,
  segmentNewTokens,
  gapSegmentRef,
  wallCapsuleSpots,
  ticketTokenRows,
  ticketSplitParts,
  journeySplitParts,
  journeyTicketTotals,
  fareTicketContext,
  fareTicketHTML,
} from "../js/map.js";
/* Namespace import so P1 red tests can assert on MAP_DOCK_KEY / dockArrangement
   before those names exist as named exports (a missing named import would abort
   the whole file at load time and hide the per-test failure reasons). */
import * as mapExports from "../js/map.js";
import { toggleMapSelection, headStopKey } from "../js/map-model.js";
import { forkKind, stopsOf, stopKey, newestFirst } from "../js/lanes.js";
import { menuPlacement } from "../js/menu.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const mapSrc = readFileSync(join(__dirname, "../js/map.js"), "utf8");
const mapCssSrc = readFileSync(join(__dirname, "../css/map.css"), "utf8");
const notesCssSrc = readFileSync(join(__dirname, "../css/notes.css"), "utf8");
const appSrc = readFileSync(join(__dirname, "../js/app.js"), "utf8");
const indexHtml = readFileSync(join(__dirname, "../index.html"), "utf8");
const layoutCss = readFileSync(join(__dirname, "../css/layout.css"), "utf8");
const accessCss = readFileSync(join(__dirname, "../css/accessibility.css"), "utf8");

/* ---------- module shape / no later features ---------- */
test("map.js exports factory and pure helpers; reuses lanes/map-model", () => {
  assert.match(mapSrc, /export function createMapFeature/);
  assert.match(mapSrc, /from "\.\/lanes\.js"/);
  assert.match(mapSrc, /from "\.\/map-model\.js"/);
  assert.match(mapSrc, /from "\.\/format\.js"/);
  assert.match(mapSrc, /from "\.\/cards\.js"/); // cardConfigText only
  assert.match(mapSrc, /from "\.\/menu\.js"/);
  assert.doesNotMatch(mapSrc, /from "\.\/chat\.js"/);
  assert.doesNotMatch(mapSrc, /from "\.\/composer\.js"/);
  assert.doesNotMatch(mapSrc, /from "\.\/polling\.js"/);
  assert.doesNotMatch(mapSrc, /from "\.\/app\.js"/);
  // must not reimplement accepted model algorithms (local wrappers that call
  // the imported helpers are fine; no second export/definition of the pure core)
  assert.doesNotMatch(mapSrc, /export function forkKind\b/);
  assert.doesNotMatch(mapSrc, /export function laneColumnOrder\b/);
  assert.doesNotMatch(mapSrc, /export function toggleMapSelection\b/);
  assert.doesNotMatch(mapSrc, /export function stopsOf\b/);
  assert.match(mapSrc, /forkKind as forkKindMod|forkKindMod/);
  assert.match(mapSrc, /toggleMapSelection/);
  assert.match(mapSrc, /laneColumnOrder/);
});

test("storage keys are the durable Journeys contracts", () => {
  assert.equal(MAP_TAB_KEY, "scimux-maptab");
  assert.equal(MAP_FOLD_KEY, "scimux-mapfold");
  assert.equal(MAP_FOLD_KNOWN_KEY, "scimux-mapfold-known");
  assert.equal(MAP_FULL_KEY, "scimux-mapfull");
  assert.equal(MAP_FARE_KEY, "scimux-fare");
});

/* ---------- visibility / tab / render decision ---------- */
test("mapIsVisible: phone uses level 3; desktop uses full or map-open", () => {
  assert.equal(mapIsVisible({ isDesktop: false, mapFull: true, mapOpen: true, level: 2 }), false);
  assert.equal(mapIsVisible({ isDesktop: false, mapFull: false, mapOpen: false, level: 3 }), true);
  assert.equal(mapIsVisible({ isDesktop: true, mapFull: true, mapOpen: false, level: 1 }), true);
  assert.equal(mapIsVisible({ isDesktop: true, mapFull: false, mapOpen: true, level: 1 }), true);
  assert.equal(mapIsVisible({ isDesktop: true, mapFull: false, mapOpen: false, level: 3 }), false);
});

test("resolveMapTab only resets invalid groups after uiLoaded", () => {
  assert.equal(resolveMapTab("g1", [], false), "g1");
  assert.equal(resolveMapTab("g1", [], true), "all");
  assert.equal(resolveMapTab("g1", [{ id: "g1", name: "A" }], true), "g1");
  assert.equal(resolveMapTab("all", [], true), "all");
});

test("mapRenderDecision skip vs rebuild", () => {
  assert.equal(mapRenderDecision("a", "a"), "skip");
  assert.equal(mapRenderDecision("a", "b"), "rebuild");
});

test("clearMapSelection wipes wall selection triple", () => {
  assert.deepEqual(clearMapSelection(), { mapSel: "", mapSelKey: "", mapSelStop: "" });
});

/* ---------- fold keys / chip plan ---------- */
test("applyNewLaneFolds defaults new lanes folded and prunes dead ids", () => {
  const { mapFold, mapFoldKnown } = applyNewLaneFolds(
    new Set(["old", "dead"]),
    new Set(["old", "dead"]),
    ["old", "fresh"],
  );
  assert.equal(mapFold.has("old"), true);
  assert.equal(mapFold.has("fresh"), true, "new lane starts folded");
  assert.equal(mapFold.has("dead"), false);
  assert.equal(mapFoldKnown.has("fresh"), true);
  assert.equal(mapFoldKnown.has("dead"), false);
});

test("unfoldFocusLane and toggleFoldId are pure set ops", () => {
  const f = new Set(["a", "b"]);
  assert.deepEqual([...unfoldFocusLane(f, "a")].sort(), ["b"]);
  assert.deepEqual([...unfoldFocusLane(f, "z")].sort(), ["a", "b"]);
  assert.deepEqual([...toggleFoldId(f, "a")].sort(), ["b"]);
  assert.deepEqual([...toggleFoldId(f, "c")].sort(), ["a", "b", "c"]);
  // original set not mutated
  assert.deepEqual([...f].sort(), ["a", "b"]);
});

test("chipTapPlan: second tap clear-scope+fold; first tap focus+unfold", () => {
  const clear = chipTapPlan({ laneFilter: "L1", id: "L1", mapFold: new Set() });
  assert.equal(clear.kind, "clear-scope");
  assert.equal(clear.mapFold.has("L1"), true);

  const focus = chipTapPlan({ laneFilter: "", id: "L1", mapFold: new Set(["L1"]) });
  assert.equal(focus.kind, "focus");
  assert.equal(focus.mapFold.has("L1"), false);

  const focus2 = chipTapPlan({ laneFilter: "other", id: "L1", mapFold: new Set() });
  assert.equal(focus2.kind, "focus");
  assert.equal(focus2.mapFold.has("L1"), false);
});

/* ---------- selection helpers (reuse toggleMapSelection, not retest matrix) ---------- */
test("selectedStopOf resolves earlier stop or head", () => {
  const n = {
    id: "a",
    created_at: "2026-01-01T00:00:00Z",
    stops: ["2026-01-02T00:00:00Z"],
    title: "T",
  };
  const head = selectedStopOf([n], "a", "");
  assert.equal(head.head, true);
  assert.equal(head.i, 1);
  const earlier = selectedStopOf([n], "a", "2026-01-01T00:00:00Z");
  assert.equal(earlier.head, false);
  assert.equal(earlier.time, "2026-01-01T00:00:00Z");
  assert.equal(selectedStopOf([n], "missing", ""), null);
  // accepted toggleMapSelection still owns second-tap deselection
  const next = toggleMapSelection({
    id: "a", key: headStopKey("a", [n]), stopTime: "", mapSelKey: "a#1", nodes: [n],
  });
  assert.equal(next.mapSel, "");
  assert.equal(next.mapSelKey, "");
});

/* ---------- pure HTML / style fragments ---------- */
test("laneChipStyle selected uses contrast text", () => {
  assert.equal(
    laneChipStyle("#007AFF", false, { escape: s => s }),
    "border-color:#007AFF;color:#007AFF",
  );
  assert.equal(
    laneChipStyle("#007AFF", true, { escape: s => s, contrastText: () => "#fff" }),
    "border-color:#007AFF;background:#007AFF;color:#fff",
  );
});

test("mapTabsHTML All / groups / add button order and ARIA", () => {
  const html = mapTabsHTML("g2", [
    { id: "g2", name: "Beta" },
    { id: "g1", name: "Alpha" },
  ], { escape: s => s });
  assert.match(html, /data-mt="all"/);
  assert.match(html, /class="on"/);
  // sorted by name: Alpha before Beta
  assert.ok(html.indexOf("Alpha") < html.indexOf("Beta"));
  assert.match(html, /data-mt="\+" class="add" aria-label="add map"/);
  assert.match(html, /data-rm="g1"/);
});

/* ---------- Phase 8: fare layer toggle + D1 tank (v1 meter retired in V2-P4) ---------- */

test("ctx_pct still rendered as tank, unchanged (D1)", () => {
  // Tank is the wall-map occupancy ring (D1) — distinct from the fare capsule.
  // Pin the SVG occupancy path byte-for-byte so a merge into the fare overlay is caught.
  // Re-pinned when the arc gained its repaint hooks and lost the frac > 0
  // guard (an arc that only exists above 0% cannot be patched upwards). The
  // geometry — R, the two concentric circles, the 80% amber flip, the -90
  // rotation — is unchanged and is what this pin is here to protect.
  // P5: also suppress the tank under a finished-turn ring (same reason as
  // attention — two concentric rings would fight). Geometry otherwise pinned.
  // hardAttention (not raw n.attention) so neutral inspect does not suppress the
  // tank the way a yellow ring would — inspect is not ringed (fixes-2 P1).
  const tankSnippet = `if (n.ctx_pct != null && !hardAttention(n) && !turnFinished(n)){
        const R = CTX_RING_R, C = 2 * Math.PI * R, frac = Math.max(0, Math.min(1, n.ctx_pct / 100));
        svg += \`<circle cx="\${dotX}" cy="\${yy}" r="\${R}" fill="none" stroke="\${col}" stroke-width="2" opacity="\${op * .2}"/>\`;`;
  assert.ok(mapSrc.includes(tankSnippet), "wall tank SVG occupancy ring must be unchanged (D1)");
  const arcSnippet = `svg += \`<circle data-ctx="\${skey}" data-col="\${col}" cx="\${dotX}" cy="\${yy}" r="\${R}" fill="none"
                stroke="\${n.ctx_pct >= 80 ? "var(--attn)" : col}" stroke-width="2"
                stroke-dasharray="\${frac * C} \${C}" stroke-linecap="round"
                transform="rotate(-90 \${dotX} \${yy})" opacity="\${op}"/>\`;`;
  assert.ok(mapSrc.includes(arcSnippet), "wall tank occupancy arc must be unchanged (D1)");
  assert.equal(mapExports.CTX_RING_R, 9, "the tank's radius is the pinned geometry");
  // Capsule/callout must not re-absorb occupancy as "% ctx"
  assert.doesNotMatch(mapSrc, /% ctx/);
});

test("wall signature changes when fare fields change (fareOn)", () => {
  const base = {
    id: "a", title: "T", description: "d", agent: "x", model: "m", effort: "",
    lane_id: "L", parent: "", ended_at: "", live: "quiet", attention: "",
    created_at: "2026-01-01T00:00:00Z", last_activity: 100, ctx_pct: 10,
    fare_total: 1000, fare_turns: 2, fare_cost_complete: false,
    stops: [],
  };
  const rows1 = stopsOf(base).sort(newestFirst);
  const w1 = wallMapSignature(rows1, ["L"], {
    focusLane: null, mapTab: "all", mapSel: "", mapSelKey: "", fareOn: true,
  });
  const grown = { ...base, fare_total: 2500, fare_turns: 4, fare_cost_complete: true };
  const rows2 = stopsOf(grown).sort(newestFirst);
  const w2 = wallMapSignature(rows2, ["L"], {
    focusLane: null, mapTab: "all", mapSel: "", mapSelKey: "", fareOn: true,
  });
  assert.notEqual(w1, w2, "fare fingerprint must re-render when fare fields change");
  // last_activity alone is not the proxy — same last_activity, different fare still changes
  assert.equal(base.last_activity, grown.last_activity);
});

test("wall signature unchanged when fareOff even if fare fields change", () => {
  const base = {
    id: "a", title: "T", description: "d", agent: "x", model: "m", effort: "",
    lane_id: "L", parent: "", ended_at: "", live: "quiet", attention: "",
    created_at: "2026-01-01T00:00:00Z", last_activity: 100, ctx_pct: 10,
    fare_total: 1000, fare_turns: 2, fare_cost_complete: false,
    stops: [],
  };
  const w1 = wallMapSignature(stopsOf(base).sort(newestFirst), ["L"], {
    focusLane: null, mapTab: "all", mapSel: "", mapSelKey: "", fareOn: false,
  });
  const grown = { ...base, fare_total: 99999, fare_turns: 99, fare_cost_complete: true };
  const w2 = wallMapSignature(stopsOf(grown).sort(newestFirst), ["L"], {
    focusLane: null, mapTab: "all", mapSel: "", mapSelKey: "", fareOn: false,
  });
  assert.equal(w1, w2, "fare fields must not affect signature when fare overlay is off");
});

test("forkCueHTML uses y-new vs origin lane color; empty without parent", () => {
  const lm = { color: id => `C(${id})` };
  const nodes = {
    p: { id: "p", title: "Parent", lane_id: "L0" },
    c: { id: "c", title: "Child", lane_id: "L1", parent: "p" },
  };
  assert.equal(forkCueHTML(nodes.c, lm, {
    escape: s => s,
    forkKind: () => null,
    nodeById: id => nodes[id],
  }), "");
  assert.equal(forkCueHTML(nodes.c, lm, {
    escape: s => s,
    forkKind: () => "y-new",
    nodeById: id => nodes[id],
  }), `<span class="forkcue" data-goorigin="p" style="color:C(L1)">from Parent</span>`);
  assert.equal(forkCueHTML(nodes.c, lm, {
    escape: s => s,
    forkKind: () => "s",
    nodeById: id => nodes[id],
  }), `<span class="forkcue" data-goorigin="p" style="color:C(L0)">from Parent</span>`);
  assert.equal(forkCueHTML(nodes.c, lm, {
    escape: s => s,
    forkKind: () => "y-new",
    nodeById: () => null,
  }), "");
});

test("stationRowHTML earlier stop vs head markup contracts", () => {
  const n = {
    id: "a", title: "Alpha", description: "D", agent: "claude",
    model: "m", effort: "high", live: "quiet", created_at: "2026-01-01T00:00:00Z",
  };
  const stop = { n, i: 0, time: "2026-01-01T00:00:00Z", head: false };
  const earlier = stationRowHTML(n, { color: () => "#f", name: () => "L" }, {
    padLeft: 10, stop, alt: true, dim: true,
    escape: s => s, stamp: t => `T(${t})`,
  });
  assert.match(earlier, /class="strow stoprow alt dimmed"/);
  assert.match(earlier, /data-skey="a#0"/);
  assert.match(earlier, /data-stop="2026-01-01T00:00:00Z"/);
  assert.match(earlier, /earlier stop/);
  assert.doesNotMatch(earlier, /agent-logo/);

  const headStop = { n, i: 1, time: "2026-01-02T00:00:00Z", head: true };
  const head = stationRowHTML(n, { color: id => `C${id}`, name: id => `N${id}` }, {
    padLeft: 20, stop: headStop, fork: true, golane: true, others: ["L2"],
    escape: s => s, stamp: t => `T(${t})`, status: () => "Quiet",
    configText: x => `${x.model}/${x.effort}`,
    forkCue: `<span class="forkcue">cue</span>`,
    agentLogo: "LOGO",
  });
  assert.match(head, /data-nid="a"/);
  assert.match(head, /data-skey="a#1"/);
  assert.match(head, /agent-logo/);
  assert.match(head, /forkcue/);
  assert.match(head, /data-golane="L2"/);
  // The status word sits in its own element so a poll can repaint it without
  // rewriting the caption; the caption's order is otherwise unchanged.
  // P5: state class st-<kind> rides next to .st so colour tracks the word.
  assert.match(head, /m\/high · <span class="st st-quiet">Quiet<\/span>/);
  // V2-P4: no always-on fare text row on station cards
  assert.doesNotMatch(head, /class="fare"/);
});

test("attentionStationSVG and ATTN_GLOW_DEF geometry classes", () => {
  assert.match(ATTN_GLOW_DEF, /id="attnglow"/);
  const svg = attentionStationSVG(10, 20, 0.5);
  assert.match(svg, /class="attnstation-glow"/);
  assert.match(svg, /class="attnstation-ring"/);
  assert.match(svg, /cx="10"/);
  assert.match(svg, /cy="20"/);
  assert.match(svg, /--attn-op:0\.5/);
});

/* ---------- P5 (arc2): status colour, running pulse, finished ring ---------- */

test("P5: station status span carries st-<kind> for every live state", () => {
  const lm = { color: () => "#f", name: () => "L" };
  const base = { id: "n", title: "T", agent: "claude", created_at: "2026-01-01T00:00:00Z" };
  const cases = [
    { n: { ...base, live: "quiet" }, kind: "quiet", word: "Quiet" },
    { n: { ...base, live: "active" }, kind: "running", word: "Running" },
    { n: { ...base, live: "quiet", turn_done: true }, kind: "finished", word: "Ready" },
    { n: { ...base, live: "quiet", attention: "approval" }, kind: "attention", word: "Waiting" },
    { n: { ...base, live: "quiet", attention: "inspect" }, kind: "inspect", word: "Quiet" },
    { n: { ...base, live: "exited" }, kind: "exited", word: "Exited" },
    { n: { ...base, live: "unavailable" }, kind: "unavailable", word: "Unavailable" },
    { n: { ...base, ended_at: "t", live: "quiet" }, kind: "closed", word: "Closed" },
  ];
  for (const c of cases) {
    const html = stationRowHTML(c.n, lm, { escape: s => s });
    assert.match(html, new RegExp(`<span class="st st-${c.kind}">[^<]*${c.word}`),
      `${c.kind}: class + word`);
  }
});

test("P5: running pulse is a compositor-animated ::after on .st-running only", () => {
  const lm = { color: () => "#f", name: () => "L" };
  const run = stationRowHTML(
    { id: "r", title: "R", agent: "claude", live: "active", created_at: "2026-01-01T00:00:00Z" },
    lm, { escape: s => s });
  const quiet = stationRowHTML(
    { id: "q", title: "Q", agent: "claude", live: "quiet", created_at: "2026-01-01T00:00:00Z" },
    lm, { escape: s => s });
  assert.match(run, /class="st st-running"/, "running carries st-running");
  assert.doesNotMatch(quiet, /st-running/, "quiet does not");

  // Pulse is CSS, not an extra HTML element — assert on the rule body.
  const rule = mapCssSrc.match(/\.st(?:\.st-running|\.st-running::after| st-running)[^{]*\{([^}]+)\}/);
  // Prefer the ::after rule that carries the pulse itself.
  const after = mapCssSrc.match(/\.st\.st-running::after\s*\{([^}]+)\}/);
  assert.ok(after, ".st.st-running::after pulse rule must exist");
  assert.match(after[1], /animation\s*:/, "pulse is animated");
  assert.match(after[1], /box-shadow\s*:/, "static halo like .lattn (not keyframed)");
  assert.doesNotMatch(after[1], /will-change:[^;]*box-shadow/,
    "will-change cannot composite a box-shadow");
  // Keyframes used by the pulse must not animate box-shadow.
  const animName = (after[1].match(/animation\s*:\s*([A-Za-z0-9_-]+)/) || [])[1];
  assert.ok(animName, "animation names a keyframe");
  const kf = mapCssSrc.match(new RegExp(`@keyframes\\s+${animName}\\s*\\{([\\s\\S]*?)\\n\\}`));
  assert.ok(kf, `@keyframes ${animName} must exist in map.css`);
  assert.doesNotMatch(kf[1], /box-shadow/, "pulse is opacity/transform only");
  assert.match(kf[1], /opacity/, "and it must still pulse");
  void rule;
});

test("P5: reduced-motion disables the running pulse (rule body in map.css)", () => {
  // Practice: reduced-motion lives next to the rules it disables (map.css),
  // not in accessibility.css (pane slides only).
  const blocks = mapCssSrc.match(/@media\s*\(prefers-reduced-motion:\s*reduce\)\s*\{[\s\S]*?\n\}/g) || [];
  assert.ok(blocks.length, "map.css must have a prefers-reduced-motion block");
  const hit = blocks.find(b => /\.st\.st-running::after|\.st-running/.test(b));
  assert.ok(hit, "reduced-motion must mention the running pulse");
  // Assert on the rule BODY, not class names alone.
  const body = hit.replace(/^@media[^{]+\{/, "").replace(/\}\s*$/, "");
  assert.match(body, /animation\s*:\s*none/, "stops the pulse");
  assert.match(body, /box-shadow\s*:\s*none/, "drops the static halo with the pulse");
});

test("P5: finished station ring uses --work, not --attn", () => {
  // attentionStationSVG is widened: default stays --attn; finished passes --work.
  const attn = attentionStationSVG(1, 2, 1);
  assert.match(attn, /var\(--attn\)/, "hard attention keeps yellow");
  assert.doesNotMatch(attn, /var\(--work\)/, "default path is not finished hue");

  const fin = attentionStationSVG(1, 2, 1, "var(--work)");
  assert.match(fin, /var\(--work\)/, "finished ring is success/--work");
  assert.doesNotMatch(fin, /var\(--attn\)/, "finished ring must not use --attn yellow");
  assert.match(fin, /class="attnstation-glow"/, "same paint path, not a fork");
  assert.match(fin, /class="attnstation-ring"/);

  // Call sites gate on attention OR turn_done (widened, not a parallel path).
  const gates = [...mapSrc.matchAll(/if\s*\(\s*n\.attention[^)]*\)\s*svg\s*\+=\s*attentionStationSVG/g)];
  // After the fix the condition includes turnFinished / turn_done.
  const paintCalls = [...mapSrc.matchAll(/attentionStationSVG\s*\(/g)];
  assert.ok(paintCalls.length >= 3, "definition + two map paint sites");
  // Source must pass --work for the finished case somewhere near the calls.
  assert.match(mapSrc, /attentionStationSVG\([^)]*var\(--work\)|turnFinished|turn_done/,
    "paint path must know about finished / --work");
  void gates;
});

test("P5: .st colour tokens per state in map.css", () => {
  // Colour is one channel next to the word — never the only one (HIG).
  const running = mapCssSrc.match(/\.st\.st-running\s*\{([^}]+)\}/);
  const attention = mapCssSrc.match(/\.st\.st-attention\s*\{([^}]+)\}/);
  const finished = mapCssSrc.match(/\.st\.st-finished\s*\{([^}]+)\}/);
  assert.ok(running, ".st.st-running colour rule");
  assert.ok(attention, ".st.st-attention colour rule");
  assert.ok(finished, ".st.st-finished colour rule");
  assert.match(running[1], /var\(--work\)/, "running uses --work");
  assert.match(attention[1], /var\(--attn\)/, "waiting uses --attn");
  assert.match(finished[1], /var\(--work\)/, "finished uses success/--work (not --attn)");
  // Quiet / closed / exited / unavailable stay --dim (or inherit the caption dim).
  for (const k of ["quiet", "closed", "exited", "unavailable"]) {
    const r = mapCssSrc.match(new RegExp(`\\.st\\.st-${k}\\s*\\{([^}]+)\\}`));
    if (r) assert.match(r[1], /var\(--dim\)/, `${k} uses --dim`);
  }
});

/* P1 — wall-map attention ring was unclickable: .lbody > svg and .strow both
   had z-index:auto, rows paint after the SVG, and padding-left inset means each
   .strow box still spans x=0 and covers the ring hit circle. Rule-body regexes
   only (never file-wide grep). */
test("P1: .lbody > svg is stacked above the station rows", () => {
  const svg = mapCssSrc.match(/\.lbody\s*>\s*svg\s*\{([^}]+)\}/);
  assert.ok(svg, ".lbody > svg rule must exist");
  const zi = svg[1].match(/z-index\s*:\s*(-?\d+)/);
  assert.ok(zi, ".lbody > svg must declare z-index (rows cover auto-stacked SVG)");
  assert.ok(Number(zi[1]) >= 1, `z-index ${zi[1]} must be >= 1`);
  // A z-index on .strow would re-cover the SVG in tree order (rows after SVG).
  const strow = mapCssSrc.match(/\.strow\s*\{([^}]+)\}/);
  assert.ok(strow, ".strow rule must exist");
  assert.doesNotMatch(strow[1], /z-index\s*:/,
    ".strow must not declare z-index (would silently re-break ring hit)");
});

test("P1: the ring hit target keeps its pointer-events opt-in", () => {
  // Raising the SVG must not turn the overlay into a click shield over rows.
  const svg = mapCssSrc.match(/\.lbody\s*>\s*svg\s*\{([^}]+)\}/);
  assert.ok(svg, ".lbody > svg rule");
  assert.match(svg[1], /pointer-events\s*:\s*none/,
    ".lbody > svg stays pointer-events:none");
  const hit = mapCssSrc.match(/\.lbody\s*>\s*svg\s+\.attnstation-hit\s*\{([^}]+)\}/);
  assert.ok(hit, ".lbody > svg .attnstation-hit rule");
  assert.match(hit[1], /pointer-events\s*:\s*all/,
    "hit circle re-enables pointer-events:all");
});

test("P1: the fare capsule still wins over the ring", () => {
  // Capsules sit at gap midpoints and can fall inside a ring's 22px radius.
  const svg = mapCssSrc.match(/\.lbody\s*>\s*svg\s*\{([^}]+)\}/);
  assert.ok(svg, ".lbody > svg rule");
  const svgZ = svg[1].match(/z-index\s*:\s*(-?\d+)/);
  assert.ok(svgZ, ".lbody > svg must declare z-index");
  const fare = mapCssSrc.match(/\.fare-capsule\s*\{([^}]+)\}/);
  assert.ok(fare, ".fare-capsule rule");
  const fareZ = fare[1].match(/z-index\s*:\s*(-?\d+)/);
  assert.ok(fareZ, ".fare-capsule must declare z-index");
  assert.ok(Number(fareZ[1]) > Number(svgZ[1]),
    `.fare-capsule z-index ${fareZ[1]} must be strictly greater than ` +
    `.lbody > svg z-index ${svgZ[1]}`);
});

test("terminalStationSVG: spur rises then curves LEFT to a vertical buffer bar", () => {
  const svg = terminalStationSVG(100, 200, 0.5, "#abc");
  // an arc (the spur) plus a straight buffer bar
  assert.match(svg, /<path\b/, "spur is a path");
  assert.match(svg, /<line\b/, "buffer bar is a line");
  assert.match(svg, /stroke="#abc"/, "uses the passed lane colour");
  assert.match(svg, /opacity="0\.5"/, "honours op");
  // spur starts exactly at the station (100,200) and leaves it vertically
  // (12 o'clock): the first control point shares the station's x.
  assert.match(svg, /d="M 100 200 C 100 /, "starts at the station, tangent up");
  // the tip and buffer bar sit to the LEFT of the station (x < 100) and ABOVE
  // it (y < 200) — the fork cue hooks right, so the terminus goes left.
  const bar = svg.match(/<line x1="([\d.]+)" y1="([\d.]+)" x2="([\d.]+)" y2="([\d.]+)"/);
  assert.ok(bar, "buffer bar coordinates present");
  const [, x1, y1, x2, y2] = bar.map(Number);
  assert.ok(x1 < 100 && x2 < 100, `buffer bar must be left of the station: x1=${x1} x2=${x2}`);
  assert.equal(x1, x2, "buffer bar is vertical");
  assert.ok(y1 < 200 && y2 < 200, `buffer bar must be above the station: y1=${y1} y2=${y2}`);
});

test("terminalCapSVG: straight buffer stop (T) centred on top of the station", () => {
  const svg = terminalCapSVG(100, 200, 0.5, "#abc");
  assert.match(svg, /stroke="#abc"/, "uses the passed lane colour");
  assert.match(svg, /opacity="0\.5"/, "honours op");
  // No curving spur — the terminus is straight lines only.
  assert.doesNotMatch(svg, /<path\b/, "cap must not draw a curved spur");
  const lines = [...svg.matchAll(/<line x1="([\d.-]+)" y1="([\d.-]+)" x2="([\d.-]+)" y2="([\d.-]+)"/g)]
    .map(m => m.slice(1).map(Number));
  assert.equal(lines.length, 2, "a vertical stem plus a horizontal bar");
  const stem = lines.find(([x1, , x2]) => x1 === x2);
  const bar = lines.find(([x1, , x2]) => x1 !== x2);
  assert.ok(stem, "vertical stem present");
  assert.ok(bar, "horizontal buffer bar present");
  // Stem rises straight up out of the station centre (x stays 100).
  assert.equal(stem[0], 100, "stem is centred on the station x");
  assert.ok(stem[1] === 200 && stem[3] < 200, "stem starts at the station and goes up");
  // Bar sits above the station, is horizontal, and is centred over it.
  assert.ok(bar[1] < 200 && bar[3] < 200, "bar sits above the station");
  assert.equal(bar[1], bar[3], "bar is horizontal");
  assert.ok(bar[0] < 100 && bar[2] > 100, "bar is centred over the station x");
});

/* ---------- stack model / signatures ---------- */
test("buildStackBlocks: group filter, newest-first stops, name order", () => {
  const lanes = [
    { id: "b", name: "Bravo" },
    { id: "a", name: "Alpha" },
  ];
  const stations = [
    { id: "n1", lane_id: "a", created_at: "2026-01-01T00:00:00Z", stops: ["2026-01-03T00:00:00Z"] },
    { id: "n2", lane_id: "b", created_at: "2026-01-02T00:00:00Z", stops: [] },
    { id: "n3", lane_id: "z", created_at: "2026-01-01T00:00:00Z", stops: [] },
  ];
  const blocks = buildStackBlocks(lanes, stations, {
    inGroup: id => id === "a" || id === "b",
  });
  assert.equal(blocks.length, 2);
  assert.equal(blocks[0].lane.id, "a", "Alpha before Bravo");
  assert.equal(blocks[1].lane.id, "b");
  assert.equal(blocks[0].rows.length, 2);
  assert.equal(blocks[0].rows[0].head, true, "newest first");
  assert.equal(blocks[0].rows[0].time, "2026-01-03T00:00:00Z");
  // accepted forkKind still classifies y-new stably (not reimplemented here)
  const child = { id: "c", lane_id: "b", parent: "n1", created_at: "2026-01-04T00:00:00Z", fork_kind: "y-new" };
  assert.equal(forkKind(child, [...stations, child]), "y-new");
});

test("stack and wall signatures change on fare/fold/selection fields", () => {
  const n = {
    id: "a", title: "T", description: "d", agent: "x", model: "m", effort: "",
    lane_id: "L", parent: "", ended_at: "", live: "quiet", attention: "",
    created_at: "2026-01-01T00:00:00Z", last_activity: 100, ctx_pct: 10,
    fare_total: 500, fare_turns: 1, fare_cost_complete: false,
    stops: [],
  };
  const rows = stopsOf(n).sort(newestFirst);
  const blocks = [{ lane: { id: "L", name: "Lane" }, rows }];
  const lm = { color: () => "#00f" };
  const s1 = stackMapSignature(blocks, {
    mapFold: new Set(), focusLane: null, mapTab: "all", mapFull: false, lm,
  });
  const s2 = stackMapSignature(blocks, {
    mapFold: new Set(["L"]), focusLane: null, mapTab: "all", mapFull: false, lm,
  });
  assert.notEqual(s1, s2);
  const s3 = stackMapSignature(blocks, {
    mapFold: new Set(), focusLane: null, mapTab: "all", mapFull: false, lm,
  });
  assert.equal(s1, s3);

  const w1 = wallMapSignature(rows, ["L"], {
    focusLane: null, mapTab: "all", mapSel: "", mapSelKey: "", fareOn: false,
  });
  const w2 = wallMapSignature(rows, ["L"], {
    focusLane: null, mapTab: "all", mapSel: "a", mapSelKey: stopKey(rows[0]), fareOn: false,
  });
  assert.notEqual(w1, w2);
  const w3 = wallMapSignature(rows, ["L"], {
    focusLane: null, mapTab: "all", mapSel: "", mapSelKey: "", fareOn: true,
  });
  assert.notEqual(w1, w3);
  assert.match(w1, /^wall\|/);
});

/* ---------- fake-DOM factory lifecycle ---------- */

/* B7/#11: stubs must not silently return null for selectors they do not
   implement — that green-washes production paths that query something the
   harness never modelled. Known forms below may return null/[] ("not found");
   anything else throws. Scope: this file only. */
function unsupportedSelector(method, selector){
  throw new Error(
    `map.test.js DOM stub ${method}(${JSON.stringify(String(selector))}): ` +
    `selector form not implemented. Implement the case, or fix production code ` +
    `that was only green because the stub returned null.`,
  );
}

/** closest() forms map.js is known to try (click routing + a few chrome paths). */
const KNOWN_CLOSEST = new Set([
  "[data-fare-capsule]",
  "[data-fold]",
  ".attnstation-hit",
  "[data-jump]",
  "[data-medit]",
  "[data-forkfrom]",
  "[data-golane]",
  "[data-goorigin]",
  "[data-nid]",
  "[data-rm]",
  "[data-mt]",
  "[data-chip]",
  "[data-forkbookmark]",
  "[data-mapexit]",
  "button, a, input",
  "#mapdivider",
  "[data-layer=\"fare\"]",
  "#layersbtn",
  // compound forms some tests assert production may use
  "circle.attnstation-hit",
  ".attnstation-hit[data-jump]",
]);

function isKnownQuerySelector(sel){
  const s = String(sel);
  return (
    s === "#mapfullbtn" || s === "#mapdivider" || s === "#app" || s === "#map" || s === "#chat" ||
    s === ".cap .st" || s === "input:checked" ||
    s.startsWith(".strow[data-skey=") ||
    s.startsWith(".strow[data-nid=") ||
    s.startsWith("circle[data-dot=") ||
    s.startsWith("circle[data-ctx=") ||
    s.startsWith(".lblock[data-lane=")
  );
}

function isKnownQuerySelectorAll(sel){
  return String(sel) === "input:checked" || isKnownQuerySelector(sel);
}

/** closest that returns matches[sel] when listed, null for other known forms,
    throws on unknown forms. matches values may be elements or null. */
function stubClosest(matches = {}){
  return function closest(sel){
    if (Object.prototype.hasOwnProperty.call(matches, sel)) return matches[sel];
    if (KNOWN_CLOSEST.has(sel)) return null;
    unsupportedSelector("closest", sel);
  };
}

/** Event target with dataset; each matchSelectors entry maps closest → this target. */
function fakeTarget(dataset, matchSelectors){
  const t = { dataset: dataset || {} };
  const sels = Array.isArray(matchSelectors) ? matchSelectors : [matchSelectors];
  const matches = {};
  for (const sel of sels) matches[sel] = t;
  t.closest = stubClosest(matches);
  return t;
}

function stubQuerySelector(impl){
  return function querySelector(sel){
    if (typeof impl === "function"){
      const r = impl(sel);
      // impl may return undefined to mean "I don't handle this form"
      if (r !== undefined) return r;
    }
    if (isKnownQuerySelector(sel)) return null;
    unsupportedSelector("querySelector", sel);
  };
}

function stubQuerySelectorAll(impl){
  return function querySelectorAll(sel){
    if (typeof impl === "function"){
      const r = impl(sel);
      if (r !== undefined) return r;
    }
    if (isKnownQuerySelectorAll(sel)) return [];
    unsupportedSelector("querySelectorAll", sel);
  };
}

function stubDocument(body, extra = {}){
  const { querySelector: qsImpl, querySelectorAll: qsaImpl, ...rest } = extra;
  return {
    body,
    ...rest,
    querySelector: stubQuerySelector(qsImpl),
    querySelectorAll: stubQuerySelectorAll(qsaImpl),
  };
}

function fakeEl(id){
  const listeners = new Map(); // event -> Set of handlers
  const attrs = {};
  return {
    id,
    innerHTML: "",
    classList: {
      _set: new Set(),
      toggle(name, on){ if (on) this._set.add(name); else this._set.delete(name); },
      contains(name){ return this._set.has(name); },
      add(name){ this._set.add(name); },
      remove(name){ this._set.delete(name); },
    },
    style: {},
    dataset: {},
    value: "",
    textContent: "",
    offsetHeight: 40,
    scrollTop: 0,
    setAttribute(k, v){ attrs[k] = String(v); },
    getAttribute(k){ return Object.prototype.hasOwnProperty.call(attrs, k) ? attrs[k] : null; },
    addEventListener(ev, fn, opts){
      if (!listeners.has(ev)) listeners.set(ev, new Set());
      listeners.get(ev).add(fn);
      this._lastOpts = opts;
    },
    removeEventListener(ev, fn){
      listeners.get(ev)?.delete(fn);
    },
    querySelector: stubQuerySelector(),
    querySelectorAll: stubQuerySelectorAll(),
    closest: stubClosest(),
    getBoundingClientRect(){ return { top: 0, bottom: 100, left: 0, right: 100 }; },
    _listeners: listeners,
    _listenerCount(ev){ return listeners.get(ev)?.size || 0; },
    _totalListeners(){
      let n = 0;
      for (const set of listeners.values()) n += set.size;
      return n;
    },
  };
}

function memoryStorage(seed = {}){
  const store = { ...seed };
  return {
    getItem(k){ return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
    setItem(k, v){ store[k] = String(v); },
    removeItem(k){ delete store[k]; },
    _raw: store,
  };
}

function firstListener(el, event){
  return [...(el._listeners.get(event) || [])][0];
}

function makeMenuNode(tag = "div"){
  const listeners = {};
  const attrs = {};
  const node = {
    tagName: String(tag).toUpperCase(),
    className: "",
    innerHTML: "",
    style: {},
    children: [],
    parentNode: null,
    dataset: {},
    getBoundingClientRect(){
      return { top: 0, left: 0, bottom: 0, right: 0, width: 0, height: 0 };
    },
    setAttribute(k, v){ attrs[k] = String(v); },
    getAttribute(k){ return Object.prototype.hasOwnProperty.call(attrs, k) ? attrs[k] : null; },
    addEventListener(type, fn){
      (listeners[type] || (listeners[type] = [])).push(fn);
    },
    removeEventListener(type, fn){
      if (!listeners[type]) return;
      listeners[type] = listeners[type].filter(f => f !== fn);
    },
    dispatch(type, ev = {}){
      const e = Object.assign({
        type, target: node, currentTarget: node,
        preventDefault(){}, stopPropagation(){},
      }, ev);
      for (const fn of listeners[type] || []) fn(e);
    },
    appendChild(child){
      child.parentNode = node;
      node.children.push(child);
      return child;
    },
    remove(){
      if (!node.parentNode) return;
      const kids = node.parentNode.children;
      const i = kids.indexOf(node);
      if (i >= 0) kids.splice(i, 1);
      node.parentNode = null;
    },
    closest(sel){
      if (sel === ".popmenu" && String(node.className || "").includes("popmenu")) return node;
      return null;
    },
  };
  return node;
}

function popMenus(panel){
  return (panel.children || []).filter(c => String(c.className || "").includes("popmenu"));
}

function fareMenuTarget(menu){
  return {
    closest(sel){
      const s = String(sel);
      if (s.includes("data-layer") && s.includes("fare")) return this;
      if (s === ".popmenu") return menu;
      return null;
    },
  };
}

function chooseFareItem(panel){
  const menus = popMenus(panel);
  assert.equal(menus.length, 1, "exactly one .popmenu must be open");
  menus[0].dispatch("click", { target: fareMenuTarget(menus[0]) });
  return menus[0];
}

function isCaptureOpt(opts){
  return opts === true || !!(opts && opts.capture);
}

function layersDoc(body, extra = {}){
  const listeners = new Map();
  const entries = new Map();
  return stubDocument(body, {
    addEventListener(ev, fn, opts){
      if (!listeners.has(ev)) listeners.set(ev, new Set());
      listeners.get(ev).add(fn);
      if (!entries.has(ev)) entries.set(ev, []);
      entries.get(ev).push({ fn, capture: isCaptureOpt(opts) });
    },
    removeEventListener(ev, fn, opts){
      listeners.get(ev)?.delete(fn);
      const list = entries.get(ev);
      if (!list) return;
      const capture = isCaptureOpt(opts);
      const i = list.findIndex(l => l.fn === fn && l.capture === capture);
      if (i >= 0) list.splice(i, 1);
    },
    createElement(tag){ return makeMenuNode(tag); },
    _listeners: listeners,
    _listenerEntries: entries,
    _listenerCount(ev){ return listeners.get(ev)?.size || 0; },
    _totalListeners(){
      let n = 0;
      for (const set of listeners.values()) n += set.size;
      return n;
    },
    ...extra,
  });
}

function dispatchDocKey(doc, ev = {}){
  const list = [...(doc._listenerEntries.get("keydown") || [])];
  const e = Object.assign({
    key: "Escape",
    target: { tagName: "BODY", isContentEditable: false },
    preventDefault(){},
    stopPropagation(){ e._stopped = true; },
  }, ev);
  for (const l of list.filter(x => x.capture)){
    l.fn(e);
    if (e._stopped) return e;
  }
  for (const l of list.filter(x => !x.capture)){
    l.fn(e);
    if (e._stopped) return e;
  }
  return e;
}

function appStyleEscape(feature, e){
  /* Mirror of app.js document keydown Escape ladder (bubble phase). */
  if (e.key !== "Escape" || !feature.isFull()) return;
  const t = e.target || {};
  if (/^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName) || t.isContentEditable) return;
  if (typeof e.preventDefault === "function") e.preventDefault();
  const step = mapExports.escapeDockStep({
    mapFull: feature.isFull(),
    mapDock: feature.isDock(),
  });
  if (step === "undock") feature.setDock(false);
  else if (step === "exit-full") feature.setFull(false);
}

function fullLayersHost(store = {}){
  const layersbtn = layersButton();
  const map = mapPanel();
  const mapwrap = fakeEl("mapwrap");
  const body = { classList: mapClassList(["map-full"]) };
  const doc = layersDoc(body);
  const storage = memoryStorage({
    [MAP_FULL_KEY]: "1",
    [MAP_FOLD_KNOWN_KEY]: JSON.stringify(["L"]),
    [MAP_FOLD_KEY]: JSON.stringify([]),
    ...store,
  });
  const feature = createMapFeature({
    roots: {
      layersbtn, map, mapwrap, mapfullbtn: fakeEl("mapfullbtn"),
      lanechips: fakeEl("chips"), maptabs: fakeEl("tabs"), maptoolbar: fakeEl("tb"),
    },
    document: doc,
    storage,
    isDesktop: () => true,
    mapOpen: () => true,
    level: () => 1,
    nodes: () => [],
    groups: () => [],
    laneFilter: () => "",
    uiLoaded: () => true,
    laneModel: () => ({ lanes: [], color: () => "", name: id => id, byId: {} }),
    icons: { ICON_LAYERS: ICON_LAYERS_TEST },
  });
  feature.bind();
  feature.restoreChrome();
  return { feature, layersbtn, map, doc, storage };
}

function layersButton(){
  const btn = fakeEl("layersbtn");
  btn.focus = () => { btn._focused = true; };
  btn.closest = function closest(sel){
    if (sel === "#layersbtn") return btn;
    if (sel === ".popmenu") return null;
    if (KNOWN_CLOSEST.has(sel)) return null;
    unsupportedSelector("closest", sel);
  };
  btn.getBoundingClientRect = () => ({
    top: 10, left: 200, bottom: 44, right: 234, width: 34, height: 34,
  });
  return btn;
}

function mapPanel(){
  const map = fakeEl("map");
  map.children = [];
  map.appendChild = function appendChild(child){
    child.parentNode = map;
    map.children.push(child);
    return child;
  };
  map.getBoundingClientRect = () => ({
    top: 0, left: 0, width: 400, height: 600, bottom: 600, right: 400,
  });
  return map;
}

const ICON_LAYERS_TEST = `<svg data-icon="layers" aria-hidden="true"></svg>`;

function mapClassList(seed = []){
  return {
    _set: new Set(seed),
    toggle(name, on){ if (on) this._set.add(name); else this._set.delete(name); },
    contains(name){ return this._set.has(name); },
    add(name){ this._set.add(name); },
  };
}

test("createMapFeature bind is idempotent; destroy removes all map-owned listeners", () => {
  const maptabs = fakeEl("maptabs");
  const lanechips = fakeEl("lanechips");
  const mapwrap = fakeEl("mapwrap");
  const mapscroll = fakeEl("mapscroll");
  const maptoolbar = fakeEl("maptoolbar");
  const mapfullbtn = fakeEl("mapfullbtn");
  const layersbtn = layersButton();
  const tabSave = fakeEl("tab_save");
  const tabDel = fakeEl("tab_del");
  const winListeners = new Map();
  const win = {
    addEventListener(ev, fn){
      if (!winListeners.has(ev)) winListeners.set(ev, new Set());
      winListeners.get(ev).add(fn);
    },
    removeEventListener(ev, fn){ winListeners.get(ev)?.delete(fn); },
  };
  const longpressCleanups = [];
  const doc = layersDoc({ classList: { contains: () => false, toggle(){}, add(){} } });
  const feature = createMapFeature({
    roots: {
      maptabs, lanechips, mapwrap, mapscroll, maptoolbar,
      mapfullbtn, layersbtn, tabSave, tabDel,
    },
    window: win,
    document: doc,
    storage: memoryStorage(),
    isDesktop: () => true,
    level: () => 1,
    nodes: () => [],
    groups: () => [],
    laneFilter: () => "",
    uiLoaded: () => true,
    laneModel: () => ({ lanes: [], color: () => "#000", name: id => id, byId: {} }),
    longpress: (container, sel, fn) => {
      const cleanup = () => { longpressCleanups.push([container.id, sel]); };
      // simulate registration side-effect owned by longpress helper
      container.addEventListener("lp-" + sel, fn);
      return () => {
        container.removeEventListener("lp-" + sel, fn);
        cleanup();
      };
    },
  });

  feature.bind();
  feature.bind(); // idempotent
  assert.equal(maptabs._listenerCount("click"), 1);
  assert.equal(lanechips._listenerCount("click"), 1);
  assert.equal(mapwrap._listenerCount("click"), 1);
  assert.equal(maptoolbar._listenerCount("click"), 1);
  assert.equal(mapscroll._listenerCount("scroll"), 1);
  assert.equal(mapfullbtn._listenerCount("click"), 1);
  assert.equal(layersbtn._listenerCount("click"), 1);
  assert.equal(doc._listenerCount("click"), 1, "document outside-click listener registered once");
  assert.equal(tabSave._listenerCount("click"), 1);
  assert.equal(tabDel._listenerCount("click"), 1);
  assert.equal(winListeners.get("resize")?.size || 0, 1);
  assert.equal(mapwrap._listenerCount("lp-.strow"), 1);
  assert.equal(lanechips._listenerCount("lp-.lanechip"), 1);

  const before = {
    tabs: maptabs._totalListeners(),
    wrap: mapwrap._totalListeners(),
    scroll: mapscroll._totalListeners(),
    win: winListeners.get("resize")?.size || 0,
  };
  assert.ok(before.tabs >= 1);
  feature.destroy();
  assert.equal(maptabs._totalListeners(), 0);
  assert.equal(lanechips._totalListeners(), 0);
  assert.equal(mapwrap._totalListeners(), 0);
  assert.equal(mapscroll._totalListeners(), 0);
  assert.equal(maptoolbar._totalListeners(), 0);
  assert.equal(mapfullbtn._totalListeners(), 0);
  assert.equal(layersbtn._totalListeners(), 0);
  assert.equal(doc._totalListeners(), 0, "document listeners removed on destroy");
  assert.equal(tabSave._totalListeners(), 0);
  assert.equal(tabDel._totalListeners(), 0);
  assert.equal(winListeners.get("resize")?.size || 0, 0);
  assert.equal(longpressCleanups.length, 2);

  // re-bind after destroy works
  feature.bind();
  assert.equal(maptabs._listenerCount("click"), 1);
  assert.equal(layersbtn._listenerCount("click"), 1);
  assert.equal(doc._listenerCount("click"), 1);
  feature.destroy();
});

test("setFull clears selection, persists key, toggles body class", () => {
  const body = {
    classList: {
      _set: new Set(),
      toggle(name, on){ if (on) this._set.add(name); else this._set.delete(name); },
      contains(name){ return this._set.has(name); },
      add(name){ this._set.add(name); },
    },
  };
  const storage = memoryStorage({ [MAP_FULL_KEY]: "1" });
  const mapfullbtn = fakeEl("mapfullbtn");
  const feature = createMapFeature({
    roots: { mapfullbtn, maptoolbar: fakeEl("tb"), mapwrap: fakeEl("w") },
    document: stubDocument(body),
    storage,
    isDesktop: () => true,
    mapOpen: () => true,
    level: () => 1,
    nodes: () => [{ id: "a", lane_id: "L", created_at: "2026-01-01T00:00:00Z", stops: [] }],
    groups: () => [],
    laneFilter: () => "",
    uiLoaded: () => true,
    laneModel: () => ({
      lanes: [{ id: "L", name: "Lane" }],
      color: () => "#00f",
      name: () => "Lane",
      byId: { a: { id: "a", lane_id: "L" } },
    }),
    agentLogo: () => "",
  });
  assert.equal(feature.isFull(), true);
  // force a selection via internal path: setFull(true) clears
  feature.setFull(true);
  assert.equal(feature.isFull(), true);
  assert.equal(storage.getItem(MAP_FULL_KEY), "1");
  assert.equal(body.classList.contains("map-full"), true);
  feature.setFull(false);
  assert.equal(feature.isFull(), false);
  assert.equal(storage.getItem(MAP_FULL_KEY), null);
  assert.equal(body.classList.contains("map-full"), false);
  assert.equal(mapfullbtn.getAttribute("aria-label"), "full-screen map");
});

test("earlier-stop toolbar opens history and docks without leaving full-screen", () => {
  /* P1: Open chat from the wall stays in full screen and docks the real chat
     under the map. loadChatHistory must still run against mapSelStop *before*
     any state change — same order pin as before, new destination is dock. */
  const mapwrap = fakeEl("mapwrap");
  const maptoolbar = fakeEl("maptoolbar");
  const storage = memoryStorage({
    [MAP_FULL_KEY]: "1",
    [MAP_FOLD_KNOWN_KEY]: JSON.stringify(["L"]),
    [MAP_FOLD_KEY]: JSON.stringify([]),
  });
  const body = {
    classList: {
      /* Seeded as map-full: storage restores mapFull, and restoreChrome (or a
         prior setFull) would have put the class on body in a real session. */
      _set: new Set(["map-full"]),
      toggle(name, on){ if (on) this._set.add(name); else this._set.delete(name); },
      contains(name){ return this._set.has(name); },
      add(name){ this._set.add(name); },
    },
  };
  const stop = "2026-01-01T00:00:00Z";
  const node = {
    id: "a", title: "Alpha", description: "", agent: "claude", model: "m",
    effort: "", lane_id: "L", parent: "", ended_at: "", live: "quiet",
    attention: "", created_at: "2026-01-02T00:00:00Z", stops: [stop],
  };
  const calls = [];
  const feature = createMapFeature({
    roots: { mapwrap, maptoolbar, lanechips: fakeEl("chips"), maptabs: fakeEl("tabs") },
    document: stubDocument(body),
    storage,
    isDesktop: () => true,
    mapOpen: () => true,
    level: () => 1,
    nodes: () => [node],
    groups: () => [],
    laneFilter: () => "",
    uiLoaded: () => true,
    laneModel: () => ({
      lanes: [{ id: "L", name: "Lane" }],
      color: () => "#00f",
      name: () => "Lane",
      byId: { a: node },
    }),
    laneColor: () => "#00f",
    agentLogo: () => "",
    selectNode: id => calls.push(["select", id]),
    loadChatHistory: (id, at) => calls.push(["history", id, at]),
    jumpChatToNow: () => calls.push(["now"]),
  });
  feature.bind();

  firstListener(mapwrap, "click")({
    target: fakeTarget({ nid: "a", skey: "a#0", stop }, "[data-nid]"),
  });
  assert.match(maptoolbar.innerHTML, /data-jump="a"/);
  assert.match(maptoolbar.innerHTML, /earlier stop/);

  firstListener(maptoolbar, "click")({
    target: fakeTarget({ jump: "a" }, "[data-jump]"),
  });
  assert.deepEqual(calls, [["select", "a"], ["history", "a", stop]]);
  assert.equal(feature.isFull(), true, "Open chat from the wall stays in full screen");
  assert.equal(storage.getItem(MAP_FULL_KEY), "1");
  assert.equal(body.classList.contains("map-full"), true);
  assert.equal(storage.getItem(mapExports.MAP_DOCK_KEY || "scimux-mapdock"), "1");
  assert.equal(body.classList.contains("map-dock"), true);
  feature.destroy();
});

test("new map sheet persists its checked lanes before selecting the group", () => {
  const maptabs = fakeEl("maptabs");
  const tabHead = fakeEl("tab_head");
  const tabName = fakeEl("tab_name");
  const tabLanes = fakeEl("tab_lanes");
  const tabSave = fakeEl("tab_save");
  const tabDel = fakeEl("tab_del");
  const storage = memoryStorage();
  let groups = [];
  const calls = [];
  tabLanes.querySelectorAll = stubQuerySelectorAll(selector =>
    selector === "input:checked" ? [{ value: "L1" }, { value: "L2" }] : undefined
  );
  const feature = createMapFeature({
    roots: {
      maptabs, tabHead, tabName, tabLanes, tabSave, tabDel,
      mapwrap: fakeEl("mapwrap"), lanechips: fakeEl("lanechips"),
    },
    document: stubDocument({ classList: { contains: () => false, toggle(){}, add(){} } }),
    storage,
    isDesktop: () => false,
    mapOpen: () => false,
    level: () => 1,
    nodes: () => [],
    groups: () => groups,
    laneFilter: () => "",
    uiLoaded: () => true,
    laneModel: () => ({
      lanes: [{ id: "L1", name: "One" }, { id: "L2", name: "Two" }],
      color: id => id === "L1" ? "#111" : "#222",
      name: id => id,
      byId: {},
    }),
    openSheet: selector => calls.push(["open", selector]),
    closeSheets: () => calls.push(["close"]),
    uiMutate: op => {
      calls.push(["mutate", op]);
      groups = op.groups;
    },
  });
  feature.bind();

  const add = { dataset: { mt: "+" } };
  add.closest = stubClosest({ "[data-rm]": null, "[data-mt]": add });
  firstListener(maptabs, "click")({ target: add });
  assert.deepEqual(calls, [["open", "#tabsheet"]]);
  assert.equal(tabHead.textContent, "New map");
  assert.equal(tabSave.textContent, "Add map");
  assert.equal(tabDel.style.display, "none");
  assert.match(tabLanes.innerHTML, /value="L1"/);

  tabName.value = "Roadmap";
  firstListener(tabSave, "click")();
  assert.equal(calls[1][0], "mutate");
  const op = calls[1][1];
  assert.equal(op.k, "groups");
  assert.equal(op.groups.length, 1);
  assert.match(op.groups[0].id, /^g\d+$/);
  assert.deepEqual(op.groups[0], {
    id: op.groups[0].id,
    name: "Roadmap",
    lanes: ["L1", "L2"],
  });
  assert.deepEqual(calls[2], ["close"]);
  assert.equal(storage.getItem(MAP_TAB_KEY), op.groups[0].id);
  assert.match(maptabs.innerHTML, new RegExp(`data-mt="${op.groups[0].id}"`));
  assert.match(maptabs.innerHTML, new RegExp(`data-mt="${op.groups[0].id}" class="on"`));
  feature.destroy();
});

test("syncMapFullBtn sets aria-label and out/in glyphs for enter and exit", () => {
  const mapfullbtn = fakeEl("mapfullbtn");
  const storage = memoryStorage();
  const expandIcon = "EXPAND_GLYPH";
  const contractIcon = "CONTRACT_GLYPH";
  const feature = createMapFeature({
    roots: { mapfullbtn, maptoolbar: fakeEl("tb"), mapwrap: fakeEl("w") },
    document: stubDocument({ classList: { toggle(){}, contains: () => false, add(){} } }),
    storage,
    isDesktop: () => false,
    mapOpen: () => false,
    level: () => 3,
    nodes: () => [],
    groups: () => [],
    laneFilter: () => "",
    uiLoaded: () => false,
    laneModel: () => ({ lanes: [], color: () => "", name: id => id, byId: {} }),
    icons: { ICON_MAP_EXPAND: expandIcon, ICON_MAP_CONTRACT: contractIcon },
  });
  feature.setFull(true);
  assert.equal(mapfullbtn.getAttribute("aria-label"), "exit full-screen map");
  assert.equal(mapfullbtn.innerHTML, contractIcon);
  feature.setFull(false);
  assert.equal(mapfullbtn.getAttribute("aria-label"), "full-screen map");
  assert.equal(mapfullbtn.innerHTML, expandIcon);
});

test("invalidate forces next render; signature skip prevents rebuild", () => {
  const mapwrap = fakeEl("mapwrap");
  const nodes = [{
    id: "a", title: "T", description: "", agent: "x", model: "m", effort: "",
    lane_id: "L", parent: "", ended_at: "", live: "quiet", attention: "",
    created_at: "2026-01-01T00:00:00Z", stops: [],
  }];
  // Pre-seed fold-known so L is not auto-folded as a "new" lane on first save.
  const storage = memoryStorage({
    [MAP_FOLD_KNOWN_KEY]: JSON.stringify(["L"]),
    [MAP_FOLD_KEY]: JSON.stringify([]),
  });
  const feature = createMapFeature({
    roots: { mapwrap, lanechips: fakeEl("chips"), maptabs: fakeEl("tabs") },
    document: stubDocument({ classList: { contains: () => true, toggle(){}, add(){} } }),
    storage,
    isDesktop: () => true,
    mapOpen: () => true,
    level: () => 1,
    nodes: () => nodes,
    groups: () => [],
    laneFilter: () => "",
    uiLoaded: () => true,
    laneModel: () => ({
      lanes: [{ id: "L", name: "Lane" }],
      color: () => "#00f",
      name: () => "Lane",
      byId: { a: nodes[0] },
    }),
    agentLogo: () => "",
  });
  feature.render();
  const html1 = mapwrap.innerHTML;
  assert.ok(html1.length > 0);
  assert.match(html1, /data-nid="a"/);
  mapwrap.innerHTML = "STALE";
  feature.render(); // same sig → skip
  assert.equal(mapwrap.innerHTML, "STALE");
  feature.invalidate();
  feature.render();
  assert.match(mapwrap.innerHTML, /data-nid="a"/);
});

test("stack renderer caps a top-terminus ended node with the straight buffer (T)", () => {
  const mapwrap = fakeEl("mapwrap");
  const nodes = [{
    id: "a", title: "T", description: "", agent: "x", model: "m", effort: "",
    lane_id: "L", parent: "", ended_at: "2026-01-02T00:00:00Z", live: "quiet",
    attention: "", created_at: "2026-01-01T00:00:00Z", stops: [],
  }];
  const storage = memoryStorage({
    [MAP_FOLD_KNOWN_KEY]: JSON.stringify(["L"]),
    [MAP_FOLD_KEY]: JSON.stringify([]),
  });
  const feature = createMapFeature({
    roots: { mapwrap, lanechips: fakeEl("chips"), maptabs: fakeEl("tabs") },
    document: stubDocument({ classList: { contains: () => true, toggle(){}, add(){} } }),
    storage,
    isDesktop: () => true,
    mapOpen: () => true,
    level: () => 1,
    nodes: () => nodes,
    groups: () => [],
    laneFilter: () => "",
    uiLoaded: () => true,
    laneModel: () => ({
      lanes: [{ id: "L", name: "Lane" }],
      color: () => "#00f",
      name: () => "Lane",
      byId: { a: nodes[0] },
    }),
    agentLogo: () => "",
  });
  feature.render();
  const html = mapwrap.innerHTML;
  // The single node is the topmost (north) station of its lane, so the ended
  // path must emit the straight buffer cap via the shared helper (stack
  // renderer: dotX=LX=22, first row y=38, op=1, lane colour #00f) — not the
  // sideways spur, which is reserved for mid-lane ended stations.
  assert.ok(
    html.includes(terminalCapSVG(22, 38, 1, "#00f")),
    "top-terminus ended node did not render the straight buffer cap",
  );
  assert.ok(
    !html.includes(terminalStationSVG(22, 38, 1, "#00f")),
    "top terminus must not render the sideways spur",
  );
  // The old crossbar — a horizontal line through the mainline at the dot's y —
  // must be gone (that was the "T" the spur change replaced).
  assert.doesNotMatch(html, /y1="38"[^>]*y2="38"/, "old crossbar still present");
});

test("stack renderer draws the sideways spur for a mid-lane ended node", () => {
  const mapwrap = fakeEl("mapwrap");
  // Two stations on lane L: a newer live one on top, an older ended one below.
  // The ended node is NOT the north terminus, so it keeps the sideways spur.
  const nodes = [
    {
      id: "top", title: "Top", description: "", agent: "x", model: "m", effort: "",
      lane_id: "L", parent: "", ended_at: "", live: "quiet",
      attention: "", created_at: "2026-01-03T00:00:00Z", stops: [],
    },
    {
      id: "end", title: "End", description: "", agent: "x", model: "m", effort: "",
      lane_id: "L", parent: "", ended_at: "2026-01-02T00:00:00Z", live: "quiet",
      attention: "", created_at: "2026-01-01T00:00:00Z", stops: [],
    },
  ];
  const storage = memoryStorage({
    [MAP_FOLD_KNOWN_KEY]: JSON.stringify(["L"]),
    [MAP_FOLD_KEY]: JSON.stringify([]),
  });
  const feature = createMapFeature({
    roots: { mapwrap, lanechips: fakeEl("chips"), maptabs: fakeEl("tabs") },
    document: stubDocument({ classList: { contains: () => true, toggle(){}, add(){} } }),
    storage,
    isDesktop: () => true,
    mapOpen: () => true,
    level: () => 1,
    nodes: () => nodes,
    groups: () => [],
    laneFilter: () => "",
    uiLoaded: () => true,
    laneModel: () => ({
      lanes: [{ id: "L", name: "Lane" }],
      color: () => "#00f",
      name: () => "Lane",
      byId: { top: nodes[0], end: nodes[1] },
    }),
    agentLogo: () => "",
  });
  feature.render();
  const html = mapwrap.innerHTML;
  // Rows are newest-first: "end" sits at the second row, y(1)=76+38=114.
  assert.ok(
    html.includes(terminalStationSVG(22, 114, 1, "#00f")),
    "mid-lane ended node did not render the sideways spur",
  );
  assert.ok(
    !html.includes(terminalCapSVG(22, 114, 1, "#00f")),
    "mid-lane ended node must not render the straight buffer cap",
  );
});

test("wall map caps a y-stay branch terminus with the straight buffer (T)", () => {
  const mapwrap = fakeEl("mapwrap");
  // Lane L, newest-first: a main-column root at top (T), a y-stay branch child
  // that dead-ends on its own x offset (must also get the T — the bug was that
  // the branch head, not being the lane's main-column top, fell to the spur),
  // and an older main-column root below the top (interior → keeps the spur).
  const nodes = [
    {
      id: "main-top", title: "Main top", description: "", agent: "x", model: "m",
      effort: "", lane_id: "L", parent: "", ended_at: "2026-01-05T00:00:00Z",
      live: "quiet", attention: "", created_at: "2026-01-05T00:00:00Z", stops: [],
    },
    {
      id: "branch", title: "Branch", description: "", agent: "x", model: "m",
      effort: "", lane_id: "L", parent: "main-top", fork_kind: "y-stay",
      ended_at: "2026-01-03T00:00:00Z", live: "quiet", attention: "",
      created_at: "2026-01-03T00:00:00Z", stops: [],
    },
    {
      id: "main-mid", title: "Main mid", description: "", agent: "x", model: "m",
      effort: "", lane_id: "L", parent: "", ended_at: "2026-01-01T00:00:00Z",
      live: "quiet", attention: "", created_at: "2026-01-01T00:00:00Z", stops: [],
    },
  ];
  const byId = { "main-top": nodes[0], branch: nodes[1], "main-mid": nodes[2] };
  const storage = memoryStorage({ [MAP_FULL_KEY]: "1" });
  const feature = createMapFeature({
    roots: { mapwrap, lanechips: fakeEl("chips"), maptabs: fakeEl("tabs") },
    document: stubDocument({ classList: { contains: () => true, toggle(){}, add(){} } }),
    storage,
    isDesktop: () => true,
    mapOpen: () => true,
    level: () => 1,
    nodes: () => nodes,
    groups: () => [],
    laneFilter: () => "",
    uiLoaded: () => true,
    laneModel: () => ({
      lanes: [{ id: "L", name: "Lane" }],
      color: () => "#0a0",
      name: () => "Lane",
      byId,
    }),
    agentLogo: () => "",
  });
  feature.render();
  const html = mapwrap.innerHTML;
  // Geometry: colX(0)=24, BR=13; rowY(i)=54+76i. Main column x=24, branch x=37.
  // Main-column top (row 0) → cap.
  assert.ok(html.includes(terminalCapSVG(24, 54, 1, "#0a0")),
    "main-column top terminus did not render the cap");
  // Branch head (row 1, x=37) is the top of its own line → cap, not spur.
  assert.ok(html.includes(terminalCapSVG(37, 130, 1, "#0a0")),
    "branch terminus did not render the straight buffer cap");
  assert.ok(!html.includes(terminalStationSVG(37, 130, 1, "#0a0")),
    "branch terminus must not render the sideways spur");
  // Interior main-column root (row 2) keeps the spur.
  assert.ok(html.includes(terminalStationSVG(24, 206, 1, "#0a0")),
    "interior main-column ended node did not render the spur");
});

/* ---------- Phase 8: Layers menu + wall re-render polling invariant ---------- */

test("layersMenuHTML: Fare is a menuitemcheckbox with a check only when enabled", () => {
  assert.equal(typeof mapExports.layersMenuHTML, "function", "layersMenuHTML must be exported");
  const off = mapExports.layersMenuHTML({ fareOn: false });
  assert.match(off, /role="menuitemcheckbox"/);
  assert.match(off, /aria-checked="false"/);
  assert.match(off, />Fare</);
  assert.doesNotMatch(off, /[✓✔]/);
  const on = mapExports.layersMenuHTML({ fareOn: true });
  assert.match(on, /role="menuitemcheckbox"/);
  assert.match(on, /aria-checked="true"/);
  assert.match(on, /aria-hidden="true"/);
  assert.match(on, /[✓✔]/);
  assert.match(on, />Fare</);
  const injected = mapExports.layersMenuHTML({ fareOn: false, label: `<img src=x>` });
  assert.doesNotMatch(injected, /<img/);
  assert.match(injected, /&lt;img/);
});

test("layersbtn is a stable Layers control, not an aria-pressed fare toggle", () => {
  const layersbtn = layersButton();
  const map = mapPanel();
  const storage = memoryStorage();
  const feature = createMapFeature({
    roots: {
      layersbtn, map, mapfullbtn: fakeEl("mapfullbtn"),
      maptoolbar: fakeEl("tb"), mapwrap: fakeEl("w"),
    },
    document: layersDoc({ classList: mapClassList() }),
    storage,
    isDesktop: () => true,
    mapOpen: () => true,
    level: () => 1,
    nodes: () => [],
    groups: () => [],
    laneFilter: () => "",
    uiLoaded: () => true,
    laneModel: () => ({ lanes: [], color: () => "", name: id => id, byId: {} }),
    icons: { ICON_LAYERS: ICON_LAYERS_TEST },
  });
  feature.bind();
  feature.restoreChrome();
  assert.equal(layersbtn.getAttribute("aria-label"), "Layers");
  assert.equal(layersbtn.getAttribute("title"), "Layers");
  assert.equal(layersbtn.getAttribute("aria-haspopup"), "menu");
  assert.equal(layersbtn.getAttribute("aria-expanded"), "false");
  assert.equal(layersbtn.getAttribute("aria-pressed"), null);
  assert.match(layersbtn.innerHTML, /data-icon="layers"/);
  assert.doesNotMatch(layersbtn.innerHTML, /data-fare=/);
  firstListener(layersbtn, "click")();
  assert.equal(layersbtn.getAttribute("aria-expanded"), "true");
  assert.equal(layersbtn.getAttribute("aria-pressed"), null);
  assert.match(layersbtn.innerHTML, /data-icon="layers"/);
  assert.doesNotMatch(layersbtn.innerHTML, /data-fare=/);
  feature.destroy();
});

test("layers menu: open, Fare checkbox, toggle, reopen, dismiss, destroy", () => {
  const layersbtn = layersButton();
  const map = mapPanel();
  const mapwrap = fakeEl("mapwrap");
  const node = {
    id: "a", title: "Alpha", description: "d", agent: "claude", model: "m",
    effort: "", lane_id: "L", parent: "", ended_at: "", live: "quiet",
    attention: "", created_at: "2026-01-01T00:00:00Z", last_activity: 100,
    ctx_pct: 30, stops: ["2026-01-02T00:00:00Z"],
    fare_total: 170, fare_turns: 2, fare_cost_complete: false,
    fare_segments: [{ total: 100, fresh_in: 80, out: 20, real_ms: 5000 }],
  };
  const storage = memoryStorage({
    [MAP_FULL_KEY]: "1",
    [MAP_FOLD_KNOWN_KEY]: JSON.stringify(["L"]),
    [MAP_FOLD_KEY]: JSON.stringify([]),
  });
  const doc = layersDoc({ classList: mapClassList(["map-full"]) });
  const feature = createMapFeature({
    roots: {
      layersbtn, map, mapwrap, mapfullbtn: fakeEl("mapfullbtn"),
      lanechips: fakeEl("chips"), maptabs: fakeEl("tabs"), maptoolbar: fakeEl("tb"),
    },
    document: doc,
    storage,
    isDesktop: () => true,
    mapOpen: () => true,
    level: () => 1,
    nodes: () => [node],
    groups: () => [],
    laneFilter: () => "",
    uiLoaded: () => true,
    laneModel: () => ({
      lanes: [{ id: "L", name: "Lane" }],
      color: () => "#00f",
      name: () => "Lane",
      byId: { a: node },
    }),
    agentLogo: () => "",
    icons: { ICON_LAYERS: ICON_LAYERS_TEST },
  });
  feature.bind();
  feature.restoreChrome();
  feature.render();
  assert.doesNotMatch(mapwrap.innerHTML, /fare-capsule|data-fare-capsule/);

  firstListener(layersbtn, "click")();
  assert.equal(popMenus(map).length, 1, "opening creates exactly one .popmenu");
  assert.equal(layersbtn.getAttribute("aria-expanded"), "true");
  const opened = popMenus(map)[0];
  assert.equal(opened.getAttribute("role"), "menu");
  assert.match(opened.innerHTML, /role="menuitemcheckbox"/);
  assert.match(opened.innerHTML, /aria-checked="false"/);
  assert.match(opened.innerHTML, />Fare</);
  assert.doesNotMatch(opened.innerHTML, /[✓✔]/);
  assert.match(opened.style.top || "", /^\d+(\.\d+)?px$/);
  assert.match(opened.style.left || "", /^\d+(\.\d+)?px$/);

  chooseFareItem(map);
  assert.equal(popMenus(map).length, 0, "choosing Fare dismisses the menu");
  assert.equal(layersbtn.getAttribute("aria-expanded"), "false");
  assert.equal(storage.getItem(MAP_FARE_KEY), "1");
  assert.match(mapwrap.innerHTML, /fare-capsule|data-fare-capsule/,
    "Fare toggle must invalidate the map signature and redraw the overlay");

  firstListener(layersbtn, "click")();
  const reopened = popMenus(map)[0];
  assert.ok(reopened, "reopening after toggle");
  assert.match(reopened.innerHTML, /aria-checked="true"/);
  assert.match(reopened.innerHTML, /[✓✔]/);

  const inside = { closest: sel => (sel === ".popmenu" ? reopened : null) };
  firstListener(doc, "click")({ target: inside });
  assert.equal(popMenus(map).length, 1, "click inside the menu must not close it");
  firstListener(doc, "click")({ target: layersbtn });
  assert.equal(popMenus(map).length, 1, "click on the Layers trigger must not prematurely close");
  const outside = { closest(){ return null; } };
  firstListener(doc, "click")({ target: outside });
  assert.equal(popMenus(map).length, 0, "outside click closes the menu");
  assert.equal(layersbtn.getAttribute("aria-expanded"), "false");

  firstListener(layersbtn, "click")();
  assert.equal(popMenus(map).length, 1);
  for (const fn of [...(doc._listeners.get("keydown") || [])]){
    fn({ key: "Escape", preventDefault(){}, stopPropagation(){} });
  }
  assert.equal(popMenus(map).length, 0, "Escape closes through the shared controller");
  assert.equal(layersbtn.getAttribute("aria-expanded"), "false");
  assert.equal(layersbtn._focused, true, "Escape restores trigger focus");

  firstListener(layersbtn, "click")();
  assert.equal(popMenus(map).length, 1);
  feature.destroy();
  assert.equal(popMenus(map).length, 0, "destroy removes any open popover");
  assert.equal(layersbtn._totalListeners(), 0);
  assert.equal(doc._totalListeners(), 0);
});

test("Escape with Layers open closes only the menu; map stays full-screen", () => {
  /* Reproduce app.js listener order: the dock-ladder handler is registered on
     document at load (bubble). The popover registers later, on open. A real
     keydown must let an open Layers menu refuse Escape before the ladder. */
  const { feature, layersbtn, map, doc } = fullLayersHost();
  assert.equal(feature.isFull(), true);
  doc.addEventListener("keydown", e => appStyleEscape(feature, e));

  firstListener(layersbtn, "click")();
  assert.equal(popMenus(map).length, 1);
  assert.equal(layersbtn.getAttribute("aria-expanded"), "true");

  dispatchDocKey(doc, { key: "Escape" });
  assert.equal(popMenus(map).length, 0, "first Escape dismisses Layers");
  assert.equal(feature.isFull(), true, "map must remain full-screen");
  assert.equal(layersbtn.getAttribute("aria-expanded"), "false");
  assert.equal(layersbtn._focused, true, "focus returns to Layers");

  dispatchDocKey(doc, { key: "Escape" });
  assert.equal(feature.isFull(), false, "second Escape may then exit full-screen");
  feature.destroy();
});

test("setFull(false) dismisses an open Layers menu and resets aria-expanded", () => {
  const { feature, layersbtn, map } = fullLayersHost();
  firstListener(layersbtn, "click")();
  assert.equal(popMenus(map).length, 1);
  assert.equal(layersbtn.getAttribute("aria-expanded"), "true");
  feature.setFull(false);
  assert.equal(popMenus(map).length, 0, "leaving full-screen must not orphan the popover");
  assert.equal(layersbtn.getAttribute("aria-expanded"), "false");
  feature.destroy();
});

test("layersbtn chrome CSS: full-screen-only reveal; reject old farebtn contract", () => {
  const chromeCss = layoutCss + "\n" + mapCssSrc;
  assert.match(chromeCss, /#layersbtn\s*\{[^}]*display:\s*none/s);
  assert.match(chromeCss, /body\.map-full\s+#layersbtn\s*\{[^}]*display:\s*inline-flex/s);
  assert.doesNotMatch(layoutCss, /#farebtn\b/);
  assert.doesNotMatch(mapCssSrc, /#farebtn\b/);
  assert.match(indexHtml, /id="layersbtn"[^>]*aria-label="Layers"/);
  assert.match(indexHtml, /id="layersbtn"[^>]*title="Layers"/);
  assert.match(indexHtml, /id="layersbtn"[^>]*aria-haspopup="menu"/);
  assert.match(indexHtml, /id="layersbtn"[^>]*aria-expanded="false"/);
  assert.doesNotMatch(indexHtml, /id="farebtn"/);
  assert.doesNotMatch(indexHtml, /id="layersbtn"[^>]*aria-pressed/);
  assert.doesNotMatch(indexHtml, /aria-label="toggle fare overlay"/);
  assert.doesNotMatch(mapSrc, /#farebtn\b|roots\.farebtn|ICON_FARE_ON|ICON_FARE_OFF|aria-pressed/);
  assert.match(mapSrc, /createPopoverMenu/);
  assert.match(appSrc, /ICON_LAYERS/);
  assert.match(appSrc, /layersbtn:\s*\$\("#layersbtn"\)/);
  assert.doesNotMatch(appSrc, /ICON_FARE_ON|ICON_FARE_OFF|#farebtn|farebtn:/);
});

/* ---------- P2: leading lane-row placement and responsive polish ---------- */

function stripCssComments(src){
  return String(src || "").replace(/\/\*[\s\S]*?\*\//g, "");
}

function cssRuleBodies(src, selectorRe){
  const text = stripCssComments(src);
  const bodies = [];
  const re = new RegExp("(?:" + String(selectorRe.source) + ")\\s*\\{([^}]+)\\}", "g");
  let m;
  while ((m = re.exec(text))) bodies.push(m[1]);
  return bodies;
}

function cssDecls(src, selectorRe){
  return cssRuleBodies(src, selectorRe).join("\n");
}

function declaredPx(decls, prop){
  const min = decls.match(new RegExp(`min-${prop}:\\s*([0-9.]+)px`));
  const abs = decls.match(new RegExp(`(?:^|[;{\\s])${prop}:\\s*([0-9.]+)px`));
  return Math.max(min ? Number(min[1]) : 0, abs ? Number(abs[1]) : 0);
}

function mapPanelHtml(){
  const m = indexHtml.match(/<section id="map"[^>]*>[\s\S]*?<\/section>/);
  assert.ok(m, "#map section must exist");
  return m[0];
}

function mapHeadHtml(section){
  const m = section.match(/<div class="phead">[\s\S]*?<\/div>/);
  assert.ok(m, "Journeys header (.phead) must exist");
  return m[0];
}

test("P2: Journeys regions are header, tabs, #mapcontrols, scroll; Layers leads chips", () => {
  const section = mapPanelHtml();
  const headAt = section.search(/<div class="phead">/);
  const tabsAt = section.search(/id="maptabs"/);
  const controlsAt = section.search(/id="mapcontrols"/);
  const scrollAt = section.search(/id="mapscroll"/);
  assert.ok(headAt >= 0, "header present");
  assert.ok(tabsAt > headAt, "#maptabs follows the header");
  assert.ok(controlsAt > tabsAt, "#mapcontrols follows #maptabs");
  assert.ok(scrollAt > controlsAt, "#mapscroll follows #mapcontrols");

  const controls = section.match(/id="mapcontrols"[^>]*>([\s\S]*?)id="mapscroll"/);
  assert.ok(controls, "#mapcontrols must sit immediately before #mapscroll");
  const inner = controls[1];
  assert.match(inner, /id="layersbtn"/, "#mapcontrols contains #layersbtn");
  assert.match(inner, /id="lanechips"/, "#mapcontrols contains #lanechips");
  assert.ok(inner.indexOf('id="layersbtn"') < inner.indexOf('id="lanechips"'),
    "#layersbtn is immediately before sibling #lanechips");
  assert.match(
    section,
    /id="mapcontrols"[^>]*>\s*<button[^>]*id="layersbtn"[\s\S]*?<\/button>\s*<div[^>]*id="lanechips"/,
    "#layersbtn and #lanechips are siblings; Layers is not a child of #lanechips",
  );
  assert.doesNotMatch(inner, /id="lanechips"[^>]*>[\s\S]*id="layersbtn"/,
    "renderLaneChips must not be able to erase Layers by nesting it in #lanechips");

  const head = mapHeadHtml(section);
  assert.match(head, /id="mapfullbtn"/, "header keeps #mapfullbtn");
  assert.match(head, /id="mapclose"/, "header keeps #mapclose");
  assert.doesNotMatch(head, /id="layersbtn"/, "Layers is not in the Journeys header");
});

test("P2: Layers hide is full-screen-only and leaves no leading gap or separator", () => {
  const css = layoutCss + "\n" + mapCssSrc;
  const hideTarget = cssDecls(css, /#layersbtn|#maplayers|#mapcontrols\s+\.maplayers/);
  assert.match(hideTarget, /display:\s*none/,
    "#layersbtn or its fixed wrapper is hidden by default");
  assert.match(css, /body\.map-full\s+(?:#layersbtn|#maplayers|#mapcontrols\s+\.maplayers)\s*\{[^}]*display:\s*inline-flex/s,
    "body.map-full reveals Layers");

  const controls = cssDecls(mapCssSrc, /#mapcontrols/);
  assert.ok(declaredPx(controls, "padding-left") < 44,
    "#mapcontrols must not reserve a Layers-sized leading hole when the button is hidden");
  assert.ok(declaredPx(controls, "min-width") < 44,
    "#mapcontrols min-width must not keep a Layers-sized gap when hidden");

  assert.doesNotMatch(indexHtml,
    /id="layersbtn"[\s\S]{0,240}class="(?:sep|mapsep|divider)"/,
    "no standalone separator element that would remain after Layers is hidden");
  const layersVisual = cssDecls(css, /#layersbtn(?::after)?|#maplayers(?::after)?/);
  const hasOwnedDivider = /border-right:|::after/.test(css) &&
    (/border-right:/.test(layersVisual) || /#layersbtn::after|#maplayers::after/.test(stripCssComments(css)));
  if (hasOwnedDivider){
    assert.match(hideTarget, /display:\s*none/,
      "separator is owned by the hidden Layers unit so it disappears atomically");
  }
});

test("P2: Layers has a 44×44 hit region, 16×16 glyph, focus, and press feedback", () => {
  const css = layoutCss + "\n" + mapCssSrc;
  const btn = cssDecls(css, /#layersbtn/);
  assert.ok(btn, "#layersbtn rule exists");
  assert.ok(declaredPx(btn, "width") >= 44 || declaredPx(btn, "min-width") >= 44,
    "Layers hit region is at least 44px wide");
  assert.ok(declaredPx(btn, "height") >= 44 || declaredPx(btn, "min-height") >= 44,
    "Layers hit region is at least 44px tall");
  assert.match(btn, /flex:\s*none/, "Layers group does not shrink");

  const glyph = cssDecls(css, /#layersbtn\s+svg/);
  assert.match(glyph, /width:\s*16px/, "glyph stays 16×16");
  assert.match(glyph, /height:\s*16px/, "glyph stays 16×16");

  assert.doesNotMatch(btn, /outline:\s*none/,
    "Layers must not suppress the shared :focus-visible ring");
  assert.match(accessCss, /:focus-visible\s*\{[^}]*outline:\s*2px\s+solid\s+var\(--work\)/s,
    "visible keyboard focus uses the shared focus token");

  const hover = cssDecls(css, /#layersbtn:hover/);
  const active = cssDecls(css, /#layersbtn:active/);
  assert.match(hover, /background:/, "hover feedback is present");
  assert.match(active, /background:/, "active/press feedback is present");
  assert.doesNotMatch(hover, /#[0-9a-fA-F]{3,8}/, "hover uses design tokens, not hex");
  assert.doesNotMatch(active, /#[0-9a-fA-F]{3,8}/, "active uses design tokens, not hex");
});

test("P2: #mapcontrols is a flex row; chips grow/wrap; Layers stays leading", () => {
  const controls = cssDecls(mapCssSrc, /#mapcontrols/);
  assert.match(controls, /display:\s*flex/, "#mapcontrols is a flex row");
  const dir = controls.match(/flex-direction:\s*(\w+)/);
  if (dir) assert.equal(dir[1], "row", "control row stays horizontal");
  assert.doesNotMatch(controls, /flex-wrap:\s*wrap/,
    "the row itself does not wrap; chips wrap on their side");
  assert.match(controls, /align-items:\s*center/,
    "lane chips align vertically with the Layers glyph");

  const btn = cssDecls(layoutCss + "\n" + mapCssSrc, /#layersbtn/);
  assert.match(btn, /flex:\s*none/, "Layers group does not shrink");

  const chipsScoped = cssDecls(mapCssSrc, /#mapcontrols\s+(?:#lanechips|\.lanechips)/);
  assert.match(chipsScoped, /flex:\s*1|flex-grow:\s*[1-9]/,
    "#lanechips consumes remaining width");
  const chips = chipsScoped + "\n" + cssDecls(mapCssSrc, /\.lanechips/);
  assert.match(chips, /flex-wrap:\s*wrap/, "lane chips wrap safely");
  assert.match(chips, /padding:\s*7px\s+16px/,
    "symmetric vertical padding keeps chip centers aligned while wrapping");

  const layersVisual = cssDecls(layoutCss + "\n" + mapCssSrc, /#layersbtn(?::after)?|#maplayers(?::after)?/);
  const gap = controls.match(/gap:\s*([0-9.]+)px/);
  const hasGap = !!(gap && Number(gap[1]) > 0);
  const hasDivider = /border-right:/.test(layersVisual) ||
    /#layersbtn::after|#maplayers::after/.test(stripCssComments(layoutCss + "\n" + mapCssSrc));
  assert.ok(hasGap || hasDivider,
    "a divider or gap distinguishes Layers from the lane-filter chips");

  const lanechips = fakeEl("lanechips");
  const feature = createMapFeature({
    roots: {
      lanechips,
      mapwrap: fakeEl("mapwrap"),
      maptabs: fakeEl("tabs"),
      maptoolbar: fakeEl("tb"),
      mapfullbtn: fakeEl("mapfullbtn"),
    },
    document: layersDoc({ classList: mapClassList(["map-full"]) }),
    storage: memoryStorage({
      [MAP_FULL_KEY]: "1",
      [MAP_FOLD_KNOWN_KEY]: JSON.stringify(["A", "B"]),
      [MAP_FOLD_KEY]: JSON.stringify([]),
    }),
    isDesktop: () => true,
    mapOpen: () => true,
    level: () => 1,
    nodes: () => [],
    groups: () => [],
    laneFilter: () => "B",
    uiLoaded: () => true,
    laneModel: () => ({
      lanes: [{ id: "A", name: "Alpha" }, { id: "B", name: "Beta" }],
      color: () => "#00f",
      name: id => id,
      byId: {},
    }),
  });
  feature.bind();
  feature.restoreChrome();
  feature.render();
  const html = lanechips.innerHTML;
  const aAt = html.indexOf('data-chip="A"');
  const bAt = html.indexOf('data-chip="B"');
  assert.ok(aAt >= 0 && bAt > aAt, "chip order follows the lane model");
  assert.match(html, /data-chip="B"[^>]*class="lanechip selected"|class="lanechip selected"[^>]*data-chip="B"/);
  assert.doesNotMatch(html, /data-chip="A"[^>]*selected|selected[^>]*data-chip="A"/);
  feature.destroy();
});

test("P2: Layers popover still anchors below the button and clamps in the map panel", () => {
  const wideAnchor = { top: 80, left: 16, bottom: 124, right: 60, width: 44, height: 44 };
  const widePanel = { top: 0, left: 0, width: 900, height: 600 };
  const wide = menuPlacement(wideAnchor, widePanel, { offset: 0 });
  assert.deepEqual(wide, { top: 128, left: 16 });
  assert.ok(wide.top > wideAnchor.bottom, "opens below the Layers button");
  assert.ok(wide.left >= 8 && wide.left <= widePanel.width - 190, "clamped inside a wide map");

  const narrowAnchor = { top: 80, left: 8, bottom: 124, right: 52, width: 44, height: 44 };
  const narrowPanel = { top: 0, left: 0, width: 280, height: 500 };
  const narrow = menuPlacement(narrowAnchor, narrowPanel, { offset: 0 });
  assert.equal(narrow.top, 128, "still unfolds below the button on a narrow map");
  assert.ok(narrow.left >= 8, "left edge clamps inside the panel");
  assert.ok(narrow.left <= narrowPanel.width - 190, "right edge clamps inside the panel");

  const layersbtn = layersButton();
  layersbtn.getBoundingClientRect = () => ({ ...wideAnchor });
  const map = mapPanel();
  map.getBoundingClientRect = () => ({
    ...widePanel, bottom: 600, right: 900,
  });
  const feature = createMapFeature({
    roots: {
      layersbtn, map,
      mapwrap: fakeEl("mapwrap"),
      mapfullbtn: fakeEl("mapfullbtn"),
      lanechips: fakeEl("chips"),
      maptabs: fakeEl("tabs"),
      maptoolbar: fakeEl("tb"),
    },
    document: layersDoc({ classList: mapClassList(["map-full"]) }),
    storage: memoryStorage({ [MAP_FULL_KEY]: "1" }),
    isDesktop: () => true,
    mapOpen: () => true,
    level: () => 1,
    nodes: () => [],
    groups: () => [],
    laneFilter: () => "",
    uiLoaded: () => true,
    laneModel: () => ({ lanes: [], color: () => "", name: id => id, byId: {} }),
    icons: { ICON_LAYERS: ICON_LAYERS_TEST },
  });
  feature.bind();
  firstListener(layersbtn, "click")();
  const opened = popMenus(map)[0];
  assert.ok(opened, "map consumer still opens one popover on the map panel");
  assert.equal(opened.style.top, "128px", "map consumer uses menuPlacement below Layers");
  assert.equal(opened.style.left, "16px", "leading-edge Layers opens toward the map interior");
  feature.destroy();

  const openCall = mapSrc.match(/layersMenu\.open\(\{[\s\S]*?\}\)/);
  assert.ok(openCall, "Layers still opens through the shared popover controller");
  assert.doesNotMatch(openCall[0], /offset:\s*(40|150)/,
    "Layers must not inherit notes-menu offsets that would pull it off the leading edge");
});

test("P2: map rerenders rewrite only #lanechips and #mapwrap; Layers and menu survive", () => {
  const layersbtn = layersButton();
  const lanechips = fakeEl("lanechips");
  const mapwrap = fakeEl("mapwrap");
  const map = mapPanel();
  const feature = createMapFeature({
    roots: {
      layersbtn, lanechips, mapwrap, map,
      mapfullbtn: fakeEl("mapfullbtn"),
      maptabs: fakeEl("tabs"),
      maptoolbar: fakeEl("tb"),
    },
    document: layersDoc({ classList: mapClassList(["map-full"]) }),
    storage: memoryStorage({
      [MAP_FULL_KEY]: "1",
      [MAP_FOLD_KNOWN_KEY]: JSON.stringify(["A", "B"]),
      [MAP_FOLD_KEY]: JSON.stringify([]),
    }),
    isDesktop: () => true,
    mapOpen: () => true,
    level: () => 1,
    nodes: () => [],
    groups: () => [],
    laneFilter: () => "",
    uiLoaded: () => true,
    laneModel: () => ({
      lanes: [{ id: "A", name: "Alpha" }, { id: "B", name: "Beta" }],
      color: () => "#00f",
      name: id => id,
      byId: {},
    }),
    icons: { ICON_LAYERS: ICON_LAYERS_TEST },
  });
  feature.bind();
  feature.restoreChrome();
  const glyph = layersbtn.innerHTML;
  assert.match(glyph, /data-icon="layers"/);

  firstListener(layersbtn, "click")();
  assert.equal(popMenus(map).length, 1);
  const menu = popMenus(map)[0];

  feature.render();
  assert.equal(layersbtn.innerHTML, glyph, "renderLaneChips must not rewrite the sibling Layers button");
  assert.equal(popMenus(map).length, 1, "an open Layers menu survives a poll/render");
  assert.equal(popMenus(map)[0], menu);
  assert.match(lanechips.innerHTML, /data-chip="A"/);
  assert.match(lanechips.innerHTML, /data-chip="B"/);
  assert.ok(lanechips.innerHTML.indexOf('data-chip="A"') < lanechips.innerHTML.indexOf('data-chip="B"'));
  assert.match(mapwrap.innerHTML, /empty|lblock|strow/);

  const chipFn = mapSrc.match(/function renderLaneChips\([\s\S]*?\n  \}/);
  assert.ok(chipFn, "renderLaneChips remains the chips-only rewrite");
  assert.match(chipFn[0], /lanechips\.innerHTML/);
  assert.doesNotMatch(chipFn[0], /layersbtn|mapcontrols|popmenu/,
    "renderLaneChips must not touch Layers or the open menu");
  assert.doesNotMatch(indexHtml, /id="lanechips"[^>]*>[\s\S]*id="layersbtn"/);
  feature.destroy();
});

test("wall re-render on fare change preserves external composer draft (polling invariant)", () => {
  // Composer is a singleton outside all polled render regions. A fare-driven
  // wall rebuild must only touch #mapwrap — never reset drafts or steal focus.
  // V2-P4: no always-on fare text row; heat + optional capsule are the overlay.
  const mapwrap = fakeEl("mapwrap");
  const composer = fakeEl("prompt");
  composer.value = "my draft stays";
  composer._focused = true;
  const node = {
    id: "a", title: "Alpha", description: "d", agent: "claude", model: "m",
    effort: "", lane_id: "L", parent: "", ended_at: "", live: "quiet",
    attention: "", created_at: "2026-01-01T00:00:00Z", last_activity: 100,
    ctx_pct: 30, stops: ["2026-01-02T00:00:00Z"],
    fare_fresh_in: 100, fare_out: 20, fare_cache_write: 0, fare_cache_read: 50,
    fare_total: 170, fare_turns: 2, fare_cost_complete: false,
    fare_segments: [
      { total: 100, fresh_in: 80, out: 20, real_ms: 1000 },
      { total: 70, fresh_in: 50, out: 20, real_ms: 500 },
    ],
  };
  const nodes = [node];
  const storage = memoryStorage({
    [MAP_FULL_KEY]: "1",
    [MAP_FARE_KEY]: "1",
    [MAP_FOLD_KNOWN_KEY]: JSON.stringify(["L"]),
    [MAP_FOLD_KEY]: JSON.stringify([]),
  });
  const layersbtn = layersButton();
  const map = mapPanel();
  const feature = createMapFeature({
    roots: {
      mapwrap, layersbtn, map, mapfullbtn: fakeEl("mapfullbtn"),
      lanechips: fakeEl("chips"), maptabs: fakeEl("tabs"), maptoolbar: fakeEl("tb"),
    },
    document: layersDoc({ classList: mapClassList(["map-full"]) }),
    storage,
    isDesktop: () => true,
    mapOpen: () => true,
    level: () => 1,
    nodes: () => nodes,
    groups: () => [],
    laneFilter: () => "",
    uiLoaded: () => true,
    laneModel: () => ({
      lanes: [{ id: "L", name: "Lane" }],
      color: () => "#00f",
      name: () => "Lane",
      byId: { a: node },
    }),
    agentLogo: () => "",
    icons: { ICON_LAYERS: ICON_LAYERS_TEST },
  });
  // fareOn must be restored from storage (no longer dormant)
  feature.bind();
  feature.restoreChrome();
  feature.render();
  // v1 always-on fare row is gone; wall track still draws under fareOn
  assert.doesNotMatch(mapwrap.innerHTML, /class="fare"/);
  assert.match(mapwrap.innerHTML, /<line\b/);
  assert.equal(composer.value, "my draft stays");
  assert.equal(composer._focused, true);

  // Poll-like fare growth: mutate segment costs, re-render
  node.fare_total = 9000;
  node.fare_turns = 5;
  node.fare_segments = [
    { total: 8000, fresh_in: 7000, out: 1000, real_ms: 1000 },
    { total: 1000, fresh_in: 900, out: 100, real_ms: 500 },
  ];
  feature.render(); // signature must notice fare fingerprint and rebuild
  assert.doesNotMatch(mapwrap.innerHTML, /class="fare"/);
  assert.match(mapwrap.innerHTML, /<line\b/);
  // composer singleton untouched
  assert.equal(composer.value, "my draft stays");
  assert.equal(composer._focused, true);

  // Toggle Fare through the Layers menu: still only #mapwrap, never the composer.
  firstListener(layersbtn, "click")();
  assert.equal(popMenus(map).length, 1);
  chooseFareItem(map);
  assert.equal(storage.getItem(MAP_FARE_KEY), null);
  assert.equal(composer.value, "my draft stays");
  assert.equal(composer._focused, true);

  // map never owns the composer
  assert.doesNotMatch(mapSrc, /from "\.\/composer\.js"/);
  assert.doesNotMatch(mapSrc, /#prompt\b|getElementById\(["']prompt/);
  feature.destroy();
});

/* ---------- V2-P3: lane heat overlay (fare-design.md v2.3 / V2-P3) ----------
 * Heat only. Capsule/callout/fareLineHTML retirement are V2-P4. */

test("V2-P3 heatEnabled only under mapFull && fareOn", () => {
  assert.equal(heatEnabled({ mapFull: true, fareOn: true }), true);
  assert.equal(heatEnabled({ mapFull: true, fareOn: false }), false, "off when !fareOn");
  assert.equal(heatEnabled({ mapFull: false, fareOn: true }), false, "off when not full-screen");
  assert.equal(heatEnabled({ mapFull: false, fareOn: false }), false);
});

test("V2-P3 segmentTokenCost uses token fare (four quantities / total)", () => {
  assert.equal(segmentTokenCost(null), null, "absent → null");
  assert.equal(segmentTokenCost(undefined), null);
  assert.equal(segmentTokenCost({}), null, "empty object is absent");
  // shipped total wins
  assert.equal(segmentTokenCost({
    total: 59810, fresh_in: 1, cache_read: 2, cache_write: 3, out: 4,
  }), 59810);
  // sum of four when total missing
  assert.equal(segmentTokenCost({
    fresh_in: 100, cache_read: 50, cache_write: 20, out: 30,
  }), 200);
  // partial fields still sum (zeros allowed; presence of any field counts)
  assert.equal(segmentTokenCost({ fresh_in: 0, out: 10 }), 10);
  // time parts must not drive heat magnitude
  assert.equal(segmentTokenCost({
    total: 100, real_ms: 99999, agent_ms: 1, tools_ms: 2, wait_ms: 3,
  }), 100);
});

test("V2-P3 heatStrokeWidth scales monotonically with cost; absent → base", () => {
  assert.equal(TRACK_STROKE_BASE, 3.5, "base track matches v1 wall stroke");
  assert.ok(TRACK_STROKE_MAX > TRACK_STROKE_BASE);
  assert.ok(HEAT_COST_CAP > 0);

  // Absent / non-finite / ≤0 → base track (never zero-width)
  assert.equal(heatStrokeWidth(null), TRACK_STROKE_BASE);
  assert.equal(heatStrokeWidth(undefined), TRACK_STROKE_BASE);
  assert.equal(heatStrokeWidth(NaN), TRACK_STROKE_BASE);
  assert.equal(heatStrokeWidth(0), TRACK_STROKE_BASE);
  assert.equal(heatStrokeWidth(-5), TRACK_STROKE_BASE);

  const wSmall = heatStrokeWidth(1_000);
  const wMid = heatStrokeWidth(50_000);
  const wBig = heatStrokeWidth(HEAT_COST_CAP);
  const wHuge = heatStrokeWidth(HEAT_COST_CAP * 10);

  assert.ok(wSmall > TRACK_STROKE_BASE, "positive cost thickens above base");
  assert.ok(wMid > wSmall, "mid > small (monotonic)");
  assert.ok(wBig > wMid, "cap cost > mid (monotonic)");
  assert.equal(wBig, TRACK_STROKE_MAX, "cost at cap hits max");
  assert.equal(wHuge, TRACK_STROKE_MAX, "huge cost clamps — cannot swamp the lane");
  // never zero or negative
  for (const w of [wSmall, wMid, wBig, wHuge, heatStrokeWidth(null)]) {
    assert.ok(w >= TRACK_STROKE_BASE, `width ${w} must be ≥ base`);
  }
});

test("V2-P3 heatOpacity is a mild ramp; thickness remains the channel", () => {
  // Colour is not the encoding channel — opacity may vary mildly with cost.
  const o0 = heatOpacity(null, 1);
  const o1 = heatOpacity(1_000, 1);
  const o2 = heatOpacity(HEAT_COST_CAP, 1);
  assert.equal(o0, 1, "absent keeps base opacity");
  assert.ok(o1 <= 1 && o1 > 0);
  assert.ok(o2 <= 1 && o2 >= o1, "opacity non-decreasing with cost");
  // dim-lane baseOp is multiplied through
  assert.ok(Math.abs(heatOpacity(HEAT_COST_CAP, 0.22) - 0.22 * o2) < 1e-9);
});

test("V2-P3 gapTokenCost maps consecutive same-node stops to fare_segments[i]", () => {
  const n = {
    id: "a",
    fare_segments: [
      { total: 1000, fresh_in: 800, out: 200 },
      { total: 5000, fresh_in: 4000, out: 1000 },
    ],
  };
  const s0 = { n, i: 0, time: "t0", head: false };
  const s1 = { n, i: 1, time: "t1", head: true };
  assert.equal(gapTokenCost(s0, s1), 1000, "gap 0→1 uses segments[0]");
  assert.equal(gapTokenCost(s1, s0), 1000, "order-independent");

  // Non-consecutive stops → absent
  const s2 = { n, i: 2, time: "t2", head: true };
  n.fare_segments.push({ total: 99 });
  assert.equal(gapTokenCost(s0, s2), null);

  // Different nodes → absent
  const other = { n: { id: "b", fare_segments: [{ total: 999 }] }, i: 0, time: "x", head: true };
  assert.equal(gapTokenCost(s0, other), null);

  // Missing segment entry → absent (not zero)
  const bare = { n: { id: "c", fare_segments: [] }, i: 0, time: "u", head: false };
  const bare1 = { n: bare.n, i: 1, time: "v", head: true };
  assert.equal(gapTokenCost(bare, bare1), null);

  // No fare_segments at all → absent
  const noSeg = { n: { id: "d" }, i: 0, time: "u", head: false };
  const noSeg1 = { n: noSeg.n, i: 1, time: "v", head: true };
  assert.equal(gapTokenCost(noSeg, noSeg1), null);
});

test("V2-P3 wallLaneTrackSVG: heat off → single base track; heat on → per-gap width", () => {
  const n = {
    id: "a",
    fare_segments: [
      { total: 1_000 },
      { total: HEAT_COST_CAP },
    ],
  };
  const stops = [
    { y: 10, stop: { n, i: 0, time: "t0", head: false } },
    { y: 50, stop: { n, i: 1, time: "t1", head: false } },
    { y: 90, stop: { n, i: 2, time: "t2", head: true } },
  ];

  // Heat off: one continuous base-width line (no per-gap thickening)
  const off = wallLaneTrackSVG(stops, { x: 24, color: "#00f", opacity: 1, heatOn: false });
  assert.match(off, /<line\b/);
  assert.match(off, /stroke-width="3\.5"/);
  assert.doesNotMatch(off, /data-heat=/);
  // single line covering full span
  assert.match(off, /y1="10"/);
  assert.match(off, /y2="90"/);
  const offLines = off.match(/<line\b/g) || [];
  assert.equal(offLines.length, 1, "heat off: one continuous track");

  // Heat on: two sub-segments with distinct widths
  const on = wallLaneTrackSVG(stops, { x: 24, color: "#00f", opacity: 1, heatOn: true });
  const onLines = on.match(/<line\b[^/]*\/>/g) || on.match(/<line\b[^>]*>/g) || [];
  assert.ok(onLines.length >= 2, `heat on: per-gap lines, got ${onLines.length}`);
  const wLow = heatStrokeWidth(1_000);
  const wHigh = heatStrokeWidth(HEAT_COST_CAP);
  assert.ok(wHigh > wLow);
  assert.match(on, new RegExp(`stroke-width="${String(wLow).replace(".", "\\.")}"`));
  assert.match(on, new RegExp(`stroke-width="${String(wHigh).replace(".", "\\.")}"`));
  // lane colour preserved (not colour-encoding cost)
  assert.match(on, /stroke="#00f"/);
});

test("V2-P3 wallLaneTrackSVG: absent cost → base track, never missing/zero-width", () => {
  // Two stations, no fare_segments → still a drawn base line when heatOn
  const n = { id: "a" };
  const stops = [
    { y: 20, stop: { n, i: 0, time: "t0", head: false } },
    { y: 80, stop: { n, i: 1, time: "t1", head: true } },
  ];
  const svg = wallLaneTrackSVG(stops, { x: 10, color: "#abc", opacity: 0.5, heatOn: true });
  assert.match(svg, /<line\b/, "line must still render");
  assert.match(svg, /stroke-width="3\.5"/, "absent cost uses base width");
  assert.doesNotMatch(svg, /stroke-width="0"/);
  assert.match(svg, /stroke="#abc"/);
  assert.match(svg, /opacity="0\.5"/);

  // empty / single stop → no line (nothing between)
  assert.equal(wallLaneTrackSVG([], { x: 0, color: "#x", heatOn: true }), "");
  assert.equal(wallLaneTrackSVG(stops.slice(0, 1), { x: 0, color: "#x", heatOn: true }), "");
});

test("V2-P3 heatSegmentsFingerprint and wallMapSignature reflect segment cost", () => {
  const base = {
    id: "a", title: "T", description: "d", agent: "x", model: "m", effort: "",
    lane_id: "L", parent: "", ended_at: "", live: "quiet", attention: "",
    created_at: "2026-01-01T00:00:00Z", last_activity: 100, ctx_pct: 10,
    fare_total: 6000, fare_turns: 2, fare_cost_complete: false,
    stops: ["2026-01-02T00:00:00Z"],
    fare_segments: [
      { total: 1000, fresh_in: 800, out: 200 },
      { total: 5000, fresh_in: 4000, out: 1000 },
    ],
  };
  const fp1 = heatSegmentsFingerprint(base);
  assert.ok(fp1.length > 0, "fingerprint non-empty when segments present");
  assert.match(fp1, /1000/);
  assert.match(fp1, /5000/);

  const grown = {
    ...base,
    fare_segments: [
      { total: 1000, fresh_in: 800, out: 200 },
      { total: 9000, fresh_in: 8000, out: 1000 }, // segment cost changed
    ],
  };
  // whole-journey totals unchanged — only segment heat differs
  assert.equal(base.fare_total, grown.fare_total);
  assert.notEqual(heatSegmentsFingerprint(base), heatSegmentsFingerprint(grown));

  const rows1 = stopsOf(base).sort(newestFirst);
  const rows2 = stopsOf(grown).sort(newestFirst);
  const w1 = wallMapSignature(rows1, ["L"], {
    focusLane: null, mapTab: "all", mapSel: "", mapSelKey: "", fareOn: true,
  });
  const w2 = wallMapSignature(rows2, ["L"], {
    focusLane: null, mapTab: "all", mapSel: "", mapSelKey: "", fareOn: true,
  });
  assert.notEqual(w1, w2, "signature must change when a segment's cost changes");

  // Unchanged poll → stable signature
  const w1b = wallMapSignature(rows1, ["L"], {
    focusLane: null, mapTab: "all", mapSel: "", mapSelKey: "", fareOn: true,
  });
  assert.equal(w1, w1b, "unchanged poll → stable signature");
  assert.equal(mapRenderDecision(w1, w1b), "skip");

  // fareOff: segment heat must not affect signature
  const wOff1 = wallMapSignature(rows1, ["L"], {
    focusLane: null, mapTab: "all", mapSel: "", mapSelKey: "", fareOn: false,
  });
  const wOff2 = wallMapSignature(rows2, ["L"], {
    focusLane: null, mapTab: "all", mapSel: "", mapSelKey: "", fareOn: false,
  });
  assert.equal(wOff1, wOff2, "fareOff: segment cost changes are inert");
});

test("V2-P3 wall heat re-render preserves composer (polling invariant)", () => {
  // Heat-driven signature change rebuilds #mapwrap only; composer is outside
  // every polled render region (AGENTS.md polling-never-clobbers-input).
  const mapwrap = fakeEl("mapwrap");
  const composer = fakeEl("prompt");
  composer.value = "draft across heat update";
  composer._focused = true;
  const body = {
    classList: {
      _set: new Set(["map-full"]),
      toggle(name, on){ if (on) this._set.add(name); else this._set.delete(name); },
      contains(name){ return this._set.has(name); },
      add(name){ this._set.add(name); },
    },
  };
  // Two stops so the wall draws a track that can carry heat
  const node = {
    id: "a", title: "Alpha", description: "d", agent: "claude", model: "m",
    effort: "", lane_id: "L", parent: "", ended_at: "", live: "quiet",
    attention: "", created_at: "2026-01-01T00:00:00Z", last_activity: 100,
    ctx_pct: 30, stops: ["2026-01-02T00:00:00Z"],
    fare_fresh_in: 100, fare_out: 20, fare_cache_write: 0, fare_cache_read: 50,
    fare_total: 170, fare_turns: 2, fare_cost_complete: false,
    fare_segments: [
      { total: 100, fresh_in: 80, out: 20 },
      { total: 70, fresh_in: 50, out: 20 },
    ],
  };
  const nodes = [node];
  const storage = memoryStorage({
    [MAP_FULL_KEY]: "1",
    [MAP_FARE_KEY]: "1",
    [MAP_FOLD_KNOWN_KEY]: JSON.stringify(["L"]),
    [MAP_FOLD_KEY]: JSON.stringify([]),
  });
  const feature = createMapFeature({
    roots: {
      mapwrap, layersbtn: fakeEl("layersbtn"), mapfullbtn: fakeEl("mapfullbtn"),
      lanechips: fakeEl("chips"), maptabs: fakeEl("tabs"), maptoolbar: fakeEl("tb"),
    },
    document: stubDocument(body),
    storage,
    isDesktop: () => true,
    mapOpen: () => true,
    level: () => 1,
    nodes: () => nodes,
    groups: () => [],
    laneFilter: () => "",
    uiLoaded: () => true,
    laneModel: () => ({
      lanes: [{ id: "L", name: "Lane" }],
      color: () => "#00f",
      name: () => "Lane",
      byId: { a: node },
    }),
    agentLogo: () => "",
    icons: {
      ICON_LAYERS: ICON_LAYERS_TEST,
    },
  });
  feature.bind();
  feature.restoreChrome();
  feature.render();
  assert.equal(composer.value, "draft across heat update");
  assert.equal(composer._focused, true);
  // Wall track present; v1 fare text row retired
  assert.match(mapwrap.innerHTML, /<line\b/);
  assert.doesNotMatch(mapwrap.innerHTML, /class="fare"/);

  // Poll: only segment heat grows (whole-journey fields can stay put)
  node.fare_segments = [
    { total: 100, fresh_in: 80, out: 20 },
    { total: 80_000, fresh_in: 70_000, out: 10_000 },
  ];
  feature.render();
  assert.equal(composer.value, "draft across heat update");
  assert.equal(composer._focused, true);
  assert.match(mapwrap.innerHTML, /<line\b/);
  assert.doesNotMatch(mapwrap.innerHTML, /class="fare"/);
  // map still does not own the composer
  assert.doesNotMatch(mapSrc, /from "\.\/composer\.js"/);
  feature.destroy();
});

test("V2-P3 wall render uses wallLaneTrackSVG under fareOn; base track when fareOff", () => {
  // Integration: wall path must call the heat track helper (not only pure unit).
  // A bare export is not enough — renderWallMap must invoke it.
  assert.match(mapSrc, /svg \+= wallLaneTrackSVG\(/,
    "renderWallMap must draw tracks via wallLaneTrackSVG");
  // V2-P5: the heat gate moved behind wallHeatOn() so the UI can be parked
  // without deleting the implementation. Call sites stay, gate is off.
  assert.match(mapSrc, /heatOn:\s*wallHeatOn\(fareOn\)/);
  // V2-P3 left fareLineHTML; V2-P4 retires it (see V2-P4 suite below).
  // Keep heat integration pin only — do not re-pin the v1 text line here.
});

/* ---------- V2-P4: centered capsule + tap callout; retire fareLineHTML
 * (fare-design.md v2.3 / V2-P4 / v2.5 ④⑤⑥). Red stubs fail until green. */

const SEG_FULL = {
  fresh_in: 32800,
  cache_read: 1200,
  cache_write: 25400,
  out: 410,
  total: 59810,
  turns: 3,
  real_ms: 125_000,
  agent_ms: 90_000,
  tools_ms: 25_000,
  wait_ms: 10_000,
  cost: 0.6215,
  cost_complete: true,
};

const SEG_NO_SPLIT = {
  fresh_in: 100,
  cache_read: 50,
  cache_write: 0,
  out: 20,
  total: 170,
  turns: 1,
  real_ms: 10_000,
  // agent/tools/wait intentionally absent (historical / codex-app-server)
};

const SEG_NO_COST = {
  ...SEG_FULL,
  cost: 0.99,
  cost_complete: false,
};

test("V2-P4 fareOverlayEnabled only under mapFull && fareOn", () => {
  assert.equal(fareOverlayEnabled({ mapFull: true, fareOn: true }), true);
  assert.equal(fareOverlayEnabled({ mapFull: true, fareOn: false }), false, "off when !fareOn");
  assert.equal(fareOverlayEnabled({ mapFull: false, fareOn: true }), false, "off when not full-screen");
  assert.equal(fareOverlayEnabled({ mapFull: false, fareOn: false }), false);
});

test("V2-P4 fmtRideMs compact wall-clock labels", () => {
  assert.equal(fmtRideMs(null), "");
  assert.equal(fmtRideMs(undefined), "");
  assert.equal(fmtRideMs(NaN), "");
  assert.match(fmtRideMs(12), /12\s*ms|0(\.0)?s/);
  assert.match(fmtRideMs(1_500), /1\.5\s*s|2\s*s/);
  assert.match(fmtRideMs(45_000), /45\s*s/);
  assert.match(fmtRideMs(125_000), /2\s*m|125\s*s/);
});

test("V2-P5 fareCapsuleHTML: new-token headline + segment identity; empty without segment", () => {
  assert.equal(fareCapsuleHTML(null, { x: 24, y: 50 }), "");
  assert.equal(fareCapsuleHTML(undefined, { x: 24, y: 50 }), "");

  const html = fareCapsuleHTML(SEG_FULL, {
    x: 24, y: 92, nodeId: "a", segIdx: 1, escape: s => s,
  });
  assert.match(html, /fare-capsule|data-fare-capsule/, "capsule marker");
  // headline is fresh + out (32800 + 410 = 33210) — cache excluded
  assert.match(html, /33\.?2?\s*k|33210/i);
  assert.doesNotMatch(html, /59\.?8\s*k|59810/, "must not headline the cache-inflated total");
  // the tap target must carry which segment it is, for the ticket
  assert.match(html, /data-fare-node="a"/);
  assert.match(html, /data-fare-seg="1"/);
  // at rest: no breakdown, no time split, and no anchored callout any more
  assert.doesNotMatch(html, /fresh/i);
  assert.doesNotMatch(html, /cache-wr|cache-rd/i);
  assert.doesNotMatch(html, /agent|tools|wait/i);
  assert.doesNotMatch(html, /fare-callout/);
  // position anchors (midpoint)
  assert.match(html, /24|top|left|translate/i);
});

test("V2-P5 wallMapSignature folds per-segment new tokens so idle poll skips", () => {
  const n = {
    id: "a", title: "T", description: "d", agent: "x", model: "m", effort: "",
    lane_id: "L", parent: "", ended_at: "", live: "quiet", attention: "",
    created_at: "2026-01-01T00:00:00Z", last_activity: 100, ctx_pct: 10,
    fare_total: 170, fare_turns: 1, fare_cost_complete: false,
    stops: ["2026-01-02T00:00:00Z"],
    fare_segments: [{ total: 170, fresh_in: 100, out: 20, real_ms: 1000 }],
  };
  const rows = stopsOf(n).sort(newestFirst);
  const base = { focusLane: null, mapTab: "all", mapSel: "a", mapSelKey: "a#1", fareOn: true };
  const before = wallMapSignature(rows, ["L"], base);
  assert.equal(wallMapSignature(rows, ["L"], base), before, "stable under unchanged poll");
  assert.equal(mapRenderDecision(before, before), "skip");

  // a ride that spends more must rebuild the wall (capsule label changes)
  n.fare_segments = [{ total: 170, fresh_in: 140, out: 30, real_ms: 1000 }];
  assert.notEqual(wallMapSignature(rows, ["L"], base), before);
});

test("V2-P4 v1 fareLineHTML fully retired — no export, no call site, no always-on fare row", () => {
  // ⑤ no orphaned always-on text row
  assert.doesNotMatch(mapSrc, /export function fareLineHTML\b/,
    "fareLineHTML export must be removed");
  assert.doesNotMatch(mapSrc, /fareLineHTML\s*\(/,
    "no fareLineHTML call sites");
  // stationHTML / wall must not inject class="fare" meter
  assert.doesNotMatch(mapSrc, /class="fare"/);
  // helpers that replace it must exist
  assert.match(mapSrc, /export function fareCapsuleHTML\b/);
});

test("V2-P5 anchored callout fully retired — the sheet replaces it", () => {
  assert.doesNotMatch(mapSrc, /export function fareCalloutHTML\b/,
    "fareCalloutHTML export must be removed");
  assert.doesNotMatch(mapSrc, /fareCalloutHTML\s*\(/, "no callout call sites");
  assert.doesNotMatch(mapSrc, /fareCalloutOpen/, "no callout open state");
  assert.doesNotMatch(mapSrc, /selectedSegmentRef/, "selection-only capsule path retired");
  assert.doesNotMatch(mapSrc, /fare-callout/, "no callout markup");
  const css = readFileSync(join(__dirname, "../css/map.css"), "utf8");
  assert.doesNotMatch(css, /\.fare-callout/, "callout CSS (the clipped anchor) must be gone");
  // the ticket sheet exists in its place
  assert.match(mapSrc, /export function fareTicketHTML\b/);
  const html = readFileSync(join(__dirname, "../index.html"), "utf8");
  assert.match(html, /id="faresheet"/, "ticket bottom sheet must exist");
  assert.match(html, /id="fare_ticket"/, "ticket body container must exist");
});

test("V2-P5 wall: a capsule on every segment; tap opens the ticket sheet", () => {
  const mapwrap = fakeEl("mapwrap");
  const fareticket = fakeEl("fare_ticket");
  const composer = fakeEl("prompt");
  composer.value = "draft stays for capsule";
  composer._focused = true;
  const opened = [];
  const body = {
    classList: {
      _set: new Set(["map-full"]),
      toggle(name, on){ if (on) this._set.add(name); else this._set.delete(name); },
      contains(name){ return this._set.has(name); },
      add(name){ this._set.add(name); },
    },
  };
  // creation + two /clear stops = three stations = two track gaps
  const node = {
    id: "a", title: "Alpha", description: "d", agent: "claude", model: "opus-5",
    effort: "high", lane_id: "L", parent: "", ended_at: "", live: "quiet",
    attention: "", created_at: "2026-01-01T00:00:00Z", last_activity: 100,
    ctx_pct: 30, stops: ["2026-01-02T00:00:00Z", "2026-01-03T00:00:00Z"],
    fare_fresh_in: 130, fare_out: 40, fare_cache_write: 0, fare_cache_read: 50,
    fare_total: 220, fare_turns: 4, fare_cost_complete: false,
    fare_segments: [
      { total: 100, fresh_in: 80, cache_read: 0, cache_write: 0, out: 20,
        real_ms: 5000, agent_ms: 3000, tools_ms: 1000, wait_ms: 1000, turns: 2 },
      { total: 70, fresh_in: 50, cache_read: 0, cache_write: 0, out: 20,
        real_ms: 2000, turns: 2 },
    ],
  };
  const nodes = [node];
  const storage = memoryStorage({
    [MAP_FULL_KEY]: "1",
    [MAP_FARE_KEY]: "1",
    [MAP_FOLD_KNOWN_KEY]: JSON.stringify(["L"]),
    [MAP_FOLD_KEY]: JSON.stringify([]),
  });
  const feature = createMapFeature({
    roots: {
      mapwrap, fareticket, layersbtn: fakeEl("layersbtn"), mapfullbtn: fakeEl("mapfullbtn"),
      lanechips: fakeEl("chips"), maptabs: fakeEl("tabs"), maptoolbar: fakeEl("tb"),
    },
    document: stubDocument(body),
    storage,
    isDesktop: () => true,
    mapOpen: () => true,
    level: () => 1,
    nodes: () => nodes,
    groups: () => [],
    laneFilter: () => "",
    uiLoaded: () => true,
    laneModel: () => ({
      lanes: [{ id: "L", name: "Lane" }],
      color: () => "#00f",
      name: () => "Lane",
      byId: { a: node },
    }),
    agentLogo: () => "",
    openSheet: id => opened.push(id),
    icons: {
      ICON_LAYERS: ICON_LAYERS_TEST,
    },
  });
  feature.bind();
  feature.restoreChrome();
  feature.render();

  // Both segments carry a capsule with no selection at all (that was V2-P4)
  const caps = mapwrap.innerHTML.match(/data-fare-capsule/g) || [];
  assert.equal(caps.length, 2, "one capsule per inter-station gap");
  assert.match(mapwrap.innerHTML, /data-fare-seg="0"/);
  assert.match(mapwrap.innerHTML, /data-fare-seg="1"/);
  // headline is fresh + out, not the cache-inflated total
  assert.match(mapwrap.innerHTML, /\b100\b|0\.1\s*k/i);
  // lane heat is parked: every track stroke stays at the base width
  assert.doesNotMatch(mapwrap.innerHTML, /data-heat=/);
  // no always-on fare text row, no anchored callout
  assert.doesNotMatch(mapwrap.innerHTML, /class="fare"/);
  assert.doesNotMatch(mapwrap.innerHTML, /fare-callout/);
  assert.equal(composer.value, "draft stays for capsule");
  assert.equal(composer._focused, true);

  // Tap a capsule → ticket rendered into the sheet body, sheet opened
  const capsuleEl = {
    dataset: { fareCapsule: "1", fareNode: "a", fareSeg: "0" },
  };
  capsuleEl.closest = stubClosest({ "[data-fare-capsule]": capsuleEl });
  firstListener(mapwrap, "click")({
    target: { closest: stubClosest({ "[data-fare-capsule]": capsuleEl }) },
    stopPropagation(){},
    preventDefault(){},
  });
  assert.deepEqual(opened, ["#faresheet"], "capsule tap opens the ticket sheet");
  const ticket = fareticket.innerHTML;
  assert.match(ticket, /fare-ticket/, "ticket markup");
  assert.match(ticket, /Alpha/, "station name, not just a stop number");
  assert.match(ticket, /claude/i);
  assert.match(ticket, /opus-5/i);
  assert.match(ticket, /high/i, "effort when the agent reports one");
  assert.match(ticket, /\b100\b|0\.1\s*k/, "segment new tokens");
  assert.match(ticket, /170|0\.17\s*k/, "cumulative journey total (130 + 40)");
  // the tapped segment has a split; the journey does not (segment 1 lacks it)
  assert.match(ticket, /ticket-bar/, "segment gauge");
  assert.equal((ticket.match(/ticket-bar/g) || []).length, 1,
    "no cumulative gauge while any ride lacks the split");
  // wall must not have been disturbed by opening the sheet
  assert.equal(composer.value, "draft stays for capsule");

  // Poll: tokens grow, capsule label follows, composer survives
  node.fare_segments[0] = {
    ...node.fare_segments[0], fresh_in: 8000, out: 1000, total: 9000,
  };
  feature.render();
  assert.match(mapwrap.innerHTML, /9\s*k|9000/i);
  assert.equal(composer.value, "draft stays for capsule");
  assert.equal(composer._focused, true);

  feature.destroy();
});

test("V2-P5 capsules gated off when !fareOn; heat call sites stay parked", () => {
  assert.match(mapSrc, /fareCapsuleHTML\s*\(/,
    "renderWallMap must draw capsules via fareCapsuleHTML");
  assert.match(mapSrc, /wallCapsuleSpots\s*\(/,
    "renderWallMap must place capsules from wallCapsuleSpots");
  assert.doesNotMatch(mapSrc, /export function fareLineHTML\b/);
  assert.doesNotMatch(mapSrc, /fareLineHTML\s*\(/);

  const mapwrap = fakeEl("mapwrap");
  const body = {
    classList: {
      _set: new Set(["map-full"]),
      toggle(name, on){ if (on) this._set.add(name); else this._set.delete(name); },
      contains(name){ return this._set.has(name); },
      add(name){ this._set.add(name); },
    },
  };
  const node = {
    id: "a", title: "Alpha", description: "d", agent: "claude", model: "m",
    effort: "", lane_id: "L", parent: "", ended_at: "", live: "quiet",
    attention: "", created_at: "2026-01-01T00:00:00Z", last_activity: 100,
    ctx_pct: 30, stops: ["2026-01-02T00:00:00Z"],
    fare_total: 170, fare_turns: 2, fare_cost_complete: false,
    fare_segments: [{ total: 100, fresh_in: 80, out: 20, real_ms: 5000 }],
  };
  const storage = memoryStorage({
    [MAP_FULL_KEY]: "1",
    // fare OFF
    [MAP_FOLD_KNOWN_KEY]: JSON.stringify(["L"]),
    [MAP_FOLD_KEY]: JSON.stringify([]),
  });
  const layersbtn = layersButton();
  const map = mapPanel();
  const feature = createMapFeature({
    roots: {
      mapwrap, fareticket: fakeEl("fare_ticket"), layersbtn, map,
      mapfullbtn: fakeEl("mapfullbtn"), lanechips: fakeEl("chips"),
      maptabs: fakeEl("tabs"), maptoolbar: fakeEl("tb"),
    },
    document: layersDoc(body),
    storage,
    isDesktop: () => true,
    mapOpen: () => true,
    level: () => 1,
    nodes: () => [node],
    groups: () => [],
    laneFilter: () => "",
    uiLoaded: () => true,
    laneModel: () => ({
      lanes: [{ id: "L", name: "Lane" }],
      color: () => "#00f",
      name: () => "Lane",
      byId: { a: node },
    }),
    agentLogo: () => "",
    openSheet: () => {},
    icons: {
      ICON_LAYERS: ICON_LAYERS_TEST,
    },
  });
  feature.bind();
  feature.restoreChrome();
  feature.render();
  assert.doesNotMatch(mapwrap.innerHTML, /fare-capsule|data-fare-capsule/,
    "no capsule when fareOff");
  assert.doesNotMatch(mapwrap.innerHTML, /class="fare"/);

  // turn fare on via the Layers menu → capsule appears without any selection
  firstListener(layersbtn, "click")();
  chooseFareItem(map);
  assert.match(mapwrap.innerHTML, /fare-capsule|data-fare-capsule/,
    "capsule when fareOn, no selection needed");
  assert.doesNotMatch(mapwrap.innerHTML, /data-heat=/, "heat stays parked");
  feature.destroy();
});


/* ---------- V2-P5: lane heat parked, capsule per segment, ticket sheet
 * (fare-design.md v2 follow-up). The anchored callout is retired; the
 * breakdown moves into a bottom sheet styled as a paper fare ticket, and
 * every block hides itself when its data was never reported. */

const SEG_TICKET = {
  fresh_in: 12300, cache_read: 130000, cache_write: 8000, out: 4100,
  total: 154400, turns: 3,
  real_ms: 200_000, agent_ms: 40_000, tools_ms: 120_000, wait_ms: 40_000,
  cost: 0.14, cost_complete: true,
};

const SEG_SPARSE = {
  // codex app-server: no cache write, no cost, no tool timing
  fresh_in: 7400, cache_read: 88000, cache_write: 0, out: 2400,
  total: 97800, turns: 2, real_ms: 291_000,
};

test("V2-P5 lane heat hidden in the UI, implementation kept", () => {
  assert.equal(wallHeatOn(true), false, "heat off even when the fare layer is on");
  assert.equal(wallHeatOn(false), false);
  // parked, not deleted — the maintainer wants to revisit it
  assert.match(mapSrc, /export function heatStrokeWidth\b/);
  assert.match(mapSrc, /export function heatEnabled\b/);
  assert.match(mapSrc, /export function heatOpacity\b/);
  assert.doesNotMatch(mapSrc, /heatOn:\s*fareOn\b/, "wall must not feed fareOn into heat");
});

test("V2-P5 segmentNewTokens = fresh + out; absent ≠ zero", () => {
  assert.equal(segmentNewTokens(SEG_TICKET), 16400);
  assert.equal(segmentNewTokens({ fresh_in: 0, out: 0 }), 0, "reported zero is a value");
  assert.equal(segmentNewTokens({ fresh_in: 5 }), 5, "one side present is enough");
  assert.equal(segmentNewTokens({ cache_read: 500 }), null, "cache alone is not a fare");
  assert.equal(segmentNewTokens(null), null);
  assert.equal(segmentNewTokens(undefined), null);
});

test("V2-P5 gapSegmentRef bridges consecutive stops of one node", () => {
  const n = {
    id: "a", created_at: "2026-01-01T00:00:00Z",
    stops: ["2026-01-02T00:00:00Z", "2026-01-03T00:00:00Z"],
    fare_segments: [{ fresh_in: 1, out: 1 }, { fresh_in: 2, out: 2 }],
  };
  const chain = stopsOf(n);
  assert.deepEqual(gapSegmentRef(chain[0], chain[1]),
    { nodeId: "a", segIdx: 0, seg: n.fare_segments[0] });
  assert.deepEqual(gapSegmentRef(chain[1], chain[2]),
    { nodeId: "a", segIdx: 1, seg: n.fare_segments[1] });
  // order-insensitive (wall rows are newest-first)
  assert.deepEqual(gapSegmentRef(chain[2], chain[1]),
    { nodeId: "a", segIdx: 1, seg: n.fare_segments[1] });
  // non-adjacent, cross-node, missing segment → null
  assert.equal(gapSegmentRef(chain[0], chain[2]), null);
  const other = { id: "b", created_at: "2026-01-01T00:00:00Z", stops: [] };
  assert.equal(gapSegmentRef(chain[0], stopsOf(other)[0]), null);
  assert.equal(gapSegmentRef(null, chain[0]), null);
});

test("V2-P5 wallCapsuleSpots: one midpoint per priced gap", () => {
  const n = {
    id: "a", created_at: "2026-01-01T00:00:00Z",
    stops: ["2026-01-02T00:00:00Z", "2026-01-03T00:00:00Z"],
    fare_segments: [{ fresh_in: 80, out: 20 }, { fresh_in: 50, out: 20 }],
  };
  const chain = stopsOf(n);
  const pts = [
    { y: 300, stop: chain[0] },
    { y: 200, stop: chain[1] },
    { y: 100, stop: chain[2] },
  ];
  const spots = wallCapsuleSpots(pts, 46);
  assert.equal(spots.length, 2);
  assert.deepEqual(spots.map(s => s.y).sort((a, b) => a - b), [150, 250],
    "vertically centred between the two stations");
  assert.ok(spots.every(s => s.x === 46));
  assert.deepEqual(spots.map(s => s.segIdx).sort(), [0, 1]);
  // fewer than two points, or no fare data → nothing
  assert.deepEqual(wallCapsuleSpots([pts[0]], 46), []);
  assert.deepEqual(wallCapsuleSpots([], 46), []);
  const bare = { id: "c", created_at: "2026-01-01T00:00:00Z", stops: ["2026-01-02T00:00:00Z"] };
  const bareChain = stopsOf(bare);
  assert.deepEqual(
    wallCapsuleSpots([{ y: 10, stop: bareChain[0] }, { y: 90, stop: bareChain[1] }], 46),
    [], "no fare_segments → no capsule");
});

test("V2-P5 ticketTokenRows hides quantities that were never reported", () => {
  const full = ticketTokenRows(SEG_TICKET);
  assert.deepEqual(full.map(r => r.label),
    ["new input", "reply", "cache reused", "cache written"]);
  assert.deepEqual(full.map(r => r.counted), [true, true, false, false],
    "only fresh + reply count toward the fare");
  assert.match(String(full[0].value), /12\.?3?\s*k|12300/);

  // codex: cache_write 0 → the row disappears rather than printing a zero
  const sparse = ticketTokenRows(SEG_SPARSE);
  assert.deepEqual(sparse.map(r => r.label), ["new input", "reply", "cache reused"]);

  assert.deepEqual(ticketTokenRows(null), []);
  assert.deepEqual(ticketTokenRows({}), []);
});

test("V2-P5 ticketSplitParts: bar only when the transport stamped the split", () => {
  const parts = ticketSplitParts(SEG_TICKET);
  assert.equal(parts.length, 3);
  assert.deepEqual(parts.map(p => p.key), ["agent", "tools", "wait"]);
  assert.deepEqual(parts.map(p => p.ms), [40_000, 120_000, 40_000]);
  // fractions partition the whole (v2-D1: parts never exceed real)
  const sum = parts.reduce((a, p) => a + p.frac, 0);
  assert.ok(Math.abs(sum - 1) < 1e-6, "fractions sum to 1");
  assert.match(parts[2].label, /wait/i);

  assert.equal(ticketSplitParts(SEG_SPARSE), null, "no split → no bar, never zeros");
  assert.equal(ticketSplitParts(null), null);
});

test("V2-P5 journeySplitParts hides the cumulative gauge on mixed journeys", () => {
  const split = journeySplitParts([SEG_TICKET, { ...SEG_TICKET, agent_ms: 1000, tools_ms: 2000, wait_ms: 3000 }]);
  assert.equal(split.length, 3);
  assert.deepEqual(split.map(p => p.ms), [41_000, 122_000, 43_000], "summed across rides");

  // one unsplit ride and the sum would silently under-report the journey
  assert.equal(journeySplitParts([SEG_TICKET, SEG_SPARSE]), null);
  assert.equal(journeySplitParts([]), null);
  assert.equal(journeySplitParts(null), null);
});

test("V2-P5 journeyTicketTotals sums the line, D8 cost, absent ≠ zero", () => {
  const n = {
    fare_fresh_in: 120000, fare_out: 18200, fare_turns: 7,
    fare_cost: 1.06, fare_cost_complete: true,
    stops: ["s1", "s2"],
    fare_segments: [SEG_TICKET, { ...SEG_SPARSE, real_ms: 100_000 }],
  };
  const t = journeyTicketTotals(n);
  assert.equal(t.newTokens, 138200);
  assert.equal(t.realMS, 300_000, "sum of ride durations, not wall clock");
  assert.equal(t.rides, 7);
  assert.equal(t.stops, 3, "creation + two /clear stops");
  assert.equal(t.cost, 1.06);

  // incomplete cost → omitted, never a partial sum passed off as the total
  assert.equal(journeyTicketTotals({ ...n, fare_cost_complete: false }).cost, null);
  // no fare at all
  assert.equal(journeyTicketTotals({}).newTokens, null);
  assert.equal(journeyTicketTotals(null), null);
});

test("V2-P5 fareTicketContext gathers stations, agent and totals", () => {
  const n = {
    id: "a", title: "Head", agent: "claude", model: "opus-5", effort: "high",
    created_at: "2026-01-01T09:00:00Z",
    stops: ["2026-01-01T10:00:00Z", "2026-01-01T11:00:00Z"],
    fare_fresh_in: 100, fare_out: 20, fare_turns: 3,
    fare_segments: [SEG_TICKET, SEG_SPARSE],
    station_labels: { "2026-01-01T09:00:00Z": { title: "First station" } },
  };
  const ctx = fareTicketContext(n, 0, {
    stopLabel: st => ({ title: st.i === 0 ? "First station" : "Second station" }),
    timeLabel: iso => "T" + iso.slice(11, 16),
  });
  assert.equal(ctx.from.title, "First station");
  assert.equal(ctx.to.title, "Second station");
  assert.equal(ctx.from.time, "T09:00");
  assert.equal(ctx.to.time, "T10:00");
  assert.equal(ctx.from.index, 1, "stations are numbered from 1 on the ticket");
  assert.equal(ctx.to.index, 2);
  assert.equal(ctx.agent, "claude");
  assert.equal(ctx.model, "opus-5");
  assert.equal(ctx.effort, "high");
  assert.equal(ctx.seg, n.fare_segments[0]);
  assert.equal(ctx.totals.rides, 3);

  // out-of-range or fare-less segment → no ticket
  assert.equal(fareTicketContext(n, 9, {}), null);
  assert.equal(fareTicketContext(null, 0, {}), null);
});

test("V2-P5 fareTicketHTML: full ticket", () => {
  const ctx = {
    nodeId: "a", segIdx: 0, seg: SEG_TICKET,
    from: { title: "Wiring the fare sheet", time: "14:32", index: 2 },
    to: { title: "Retiring the callout", time: "14:35", index: 3 },
    agent: "claude", model: "opus-5", effort: "high",
    totals: { newTokens: 138200, realMS: 2_472_000, cost: 1.06, rides: 7, stops: 3 },
    segments: [SEG_TICKET, { ...SEG_TICKET }],
  };
  const html = fareTicketHTML(ctx, { escape: s => s });
  assert.match(html, /fare-ticket/);
  // stations, not stop numbers alone
  assert.match(html, /Wiring the fare sheet/);
  assert.match(html, /Retiring the callout/);
  assert.match(html, /14:32/);
  assert.match(html, /14:35/);
  // agent · model · effort above the fare
  assert.match(html, /claude/i);
  assert.match(html, /opus-5/i);
  assert.match(html, /high/i);
  // headline = new tokens, cache excluded
  assert.match(html, /16\.?4?\s*k|16400/i);
  assert.match(html, /new tokens/i);
  // breakdown + the line that explains why cache is not in the fare
  assert.match(html, /new input/i);
  assert.match(html, /cache reused/i);
  assert.match(html, /cache written/i);
  // journey time: duration + split bar + legend
  assert.match(html, /ticket-bar/);
  assert.match(html, /waiting/i, "wait is spelled out, not time(1) jargon");
  // cost stamp (D8: reported)
  assert.match(html, /0\.14/);
  // cumulative foot
  assert.match(html, /138\.?2?\s*k|138200/i);
  assert.match(html, /7 rides|7\s*·\s*rides|rides/i);
  // both gauges present when the whole journey carries the split
  assert.equal((html.match(/ticket-bar/g) || []).length, 2);
});

test("V2-P5 fareTicketHTML: sparse ticket hides what was never reported", () => {
  const ctx = {
    nodeId: "b", segIdx: 0, seg: SEG_SPARSE,
    from: { title: "Codex probe", time: "09:14", index: 1 },
    to: { title: "Second turn", time: "09:19", index: 2 },
    agent: "codex", model: "gpt-5.3-codex", effort: "",
    totals: { newTokens: 61700, realMS: 1_323_000, cost: null, rides: 4, stops: 2 },
    segments: [SEG_SPARSE, SEG_SPARSE],
  };
  const html = fareTicketHTML(ctx, { escape: s => s });
  // no effort word when the agent reports none
  assert.doesNotMatch(html, /effort/i);
  // no cache-written row (0 / never reported)
  assert.doesNotMatch(html, /cache written/i);
  assert.match(html, /cache reused/i, "what was reported still shows");
  // no split → no bar anywhere, but the duration survives
  assert.doesNotMatch(html, /ticket-bar/);
  assert.doesNotMatch(html, /\bagent\b/i);
  assert.match(html, /4m 51s|291|duration|journey time/i);
  // no reported cost → no stamp, no dollar figure at all
  assert.doesNotMatch(html, /\$/);
  assert.doesNotMatch(html, /estimat/i);
  // cumulative tokens still shown
  assert.match(html, /61\.?7?\s*k|61700/i);

  assert.equal(fareTicketHTML(null), "");
  assert.equal(fareTicketHTML({ seg: null }), "");
});

/* ---------- V2-P5 review fixes (live iPad review, 2026-08-08) ---------- */

test("V2-P5 fmtRideMs climbs units — no 1300m 51s", () => {
  // short scales unchanged
  assert.match(fmtRideMs(45_000), /^45s$/);
  assert.equal(fmtRideMs(125_000), "2m 5s");
  assert.equal(fmtRideMs(600_000), "10m");

  // an hour and beyond: minutes, not a four-digit minute count
  assert.equal(fmtRideMs(3_600_000), "1h");
  assert.equal(fmtRideMs(3_599_999), "1h", "rounds up across the boundary, never 60m");
  assert.equal(fmtRideMs(28_063_000), "7h 48m", "was 467m 43s");
  assert.equal(fmtRideMs(78_051_000), "21h 41m", "was 1300m 51s");

  // a day and beyond (a journey's cumulative time reaches this)
  assert.equal(fmtRideMs(86_400_000), "1d");
  assert.equal(fmtRideMs(90_000_000), "1d 1h");
  assert.equal(fmtRideMs(8 * 86_400_000 + 5 * 3_600_000), "8d 5h");

  // the seam must never print a zero small unit
  assert.doesNotMatch(fmtRideMs(7_200_000), /0m/);
  assert.doesNotMatch(fmtRideMs(172_800_000), /0h/);
});

test("V2-P5 the ticket perforation IS the actionbar perforation", () => {
  /* Reviewed on an iPad: the ticket's punches read weaker than the bubble
     stub's in both themes. The ink and geometry were already identical — what
     differed was that .tk-tear also painted a solid --hairline rule *behind*
     the run, filling the gaps so the seam read as a bumpy line instead of a
     row of holes. .actionbar.tear sets `border-top: none` for exactly that
     reason. Same motif = same declaration, no ticket-only variant. */
  const tokens = readFileSync(join(__dirname, "../css/tokens.css"), "utf8");
  const css = readFileSync(join(__dirname, "../css/map.css"), "utf8");
  const base = readFileSync(join(__dirname, "../css/base.css"), "utf8");

  const punch = /radial-gradient\(circle, var\(--perf\) 0 2\.5px, transparent 2\.9px\) repeat-x;\s*\n?\s*background-size: 9px 6px;/;
  assert.match(base, punch, "the actionbar run is the reference");
  assert.match(css, punch, "the ticket must use the identical run");
  assert.doesNotMatch(tokens, /--fare-perf/, "no ticket-only punch token");
  assert.doesNotMatch(css, /--fare-perf/);

  // no continuous rule behind the punches (that was the whole defect)
  const tear = css.match(/\.fare-ticket \.tk-tear \{[^}]*\}/);
  assert.ok(tear, ".tk-tear rule present");
  assert.doesNotMatch(tear[0], /background:\s*var\(--hairline\)/,
    "a solid line through the run kills the perforation");
  assert.doesNotMatch(tear[0], /border-top:\s*1px/);

  // end bites match the card's --tearhole radius, and the run clears them by
  // the same 8px the actionbar uses
  assert.match(css, /\.tk-tear::after[\s\S]*?12px/, "12px end bites, as .tearcard");
  assert.match(css, /\.tk-tear::before[\s\S]*?left: 20px; right: 20px/,
    "run inset = tearhole 12px + 8px");

  /* The card's bite is a real hole (mask-image) with a --perf burr arc drawn
     around it — see notes.css. The ticket cannot mask its sheet (tear
     positions move with content), so it paints the bite; but a plain --bg
     disc on --surface is a 3% difference in light mode and vanished, which is
     why the seam lost the two anchors that sell the motif and why dark, where
     --bg is much darker than --surface, always looked fine. Painted bites
     therefore need both a hole-toned fill and the burr. */
  const bite = css.match(/\.fare-ticket \.tk-tear::after \{[^}]*\}/);
  assert.ok(bite, ".tk-tear::after rule present");
  assert.match(bite[0], /var\(--perf\)/, "burr arc around the bite, as on the card");
  assert.match(bite[0], /color-mix\([^)]*var\(--ink\)[^)]*var\(--bg\)/,
    "bite reads as a hole, not as page colour laid on the sheet");
});

/* ---------- P1: map dock shell (body.map-full.map-dock) ---------- */

/** Extract the @media (min-width: 900px) block body from notes.css by brace depth. */
function notesDesktopMediaBody(css){
  const m = /@media\s*\(\s*min-width:\s*900px\s*\)\s*\{/.exec(css);
  if (!m) return null;
  const open = m.index + m[0].length - 1; // position of '{'
  let depth = 0;
  for (let i = open; i < css.length; i++){
    const ch = css[i];
    if (ch === "{") depth++;
    else if (ch === "}"){
      depth--;
      if (depth === 0) return css.slice(open + 1, i);
    }
  }
  return null;
}

test("P1: MAP_DOCK_KEY is scimux-mapdock and distinct from every existing map key", () => {
  assert.equal(mapExports.MAP_DOCK_KEY, "scimux-mapdock");
  const existing = [MAP_TAB_KEY, MAP_FOLD_KEY, MAP_FOLD_KNOWN_KEY, MAP_FULL_KEY, MAP_FARE_KEY];
  for (const k of existing){
    assert.notEqual(mapExports.MAP_DOCK_KEY, k, `MAP_DOCK_KEY must not reuse ${k}`);
  }
});

test("P1: dockArrangement truth table; isDesktop:false never returns dock", () => {
  const dockArrangement = mapExports.dockArrangement;
  assert.equal(typeof dockArrangement, "function", "dockArrangement is exported");

  assert.equal(dockArrangement({ mapFull: false, mapDock: false, isDesktop: true }), "stack");
  assert.equal(dockArrangement({ mapFull: false, mapDock: true, isDesktop: true }), "stack");
  assert.equal(dockArrangement({ mapFull: true, mapDock: false, isDesktop: true }), "wall");
  assert.equal(dockArrangement({ mapFull: true, mapDock: true, isDesktop: true }), "dock");

  // Phone never docks — every (mapFull, mapDock) combo on !isDesktop is not "dock".
  for (const mapFull of [false, true]){
    for (const mapDock of [false, true]){
      const arr = dockArrangement({ mapFull, mapDock, isDesktop: false });
      assert.notEqual(arr, "dock",
        `isDesktop:false must never yield dock (mapFull=${mapFull}, mapDock=${mapDock})`);
    }
  }
});

test("P1: CSS body.map-full.map-dock #app sets flex-direction: column", () => {
  const rule = notesCssSrc.match(/body\.map-full\.map-dock\s+#app\s*\{([^}]+)\}/);
  assert.ok(rule, "body.map-full.map-dock #app rule present");
  assert.match(rule[1], /flex-direction:\s*column/, "#app stacks map over chat in the dock");
});

test("P1: CSS body.map-full.map-dock #chat sets display:flex AND min-height:0", () => {
  /* Load-bearing: without min-height:0 the flex item won't shrink below its
     children's intrinsic height (.panel height:100% + flex:none children) and
     the composer is pushed off-screen — same failure class as approval P7. */
  const rule = notesCssSrc.match(/body\.map-full\.map-dock\s+#chat\s*\{([^}]+)\}/);
  assert.ok(rule, "body.map-full.map-dock #chat rule present");
  assert.match(rule[1], /display:\s*flex/, "#chat is un-hidden in the dock");
  assert.match(rule[1], /min-height:\s*0/, "#chat can shrink so the composer stays in view");
});

test("P1: CSS body.map-full.map-dock #map sets flex:0 0 … and height:auto", () => {
  /* height:auto overrides .panel's height:100% so the map takes the dock's
     fixed top share instead of claiming the full viewport. */
  const rule = notesCssSrc.match(/body\.map-full\.map-dock\s+#map\s*\{([^}]+)\}/);
  assert.ok(rule, "body.map-full.map-dock #map rule present");
  assert.match(rule[1], /flex:\s*0\s+0\b/, "#map is a fixed flex basis (55% share)");
  assert.match(rule[1], /height:\s*auto/, "#map does not inherit .panel height:100%");
});

test("P1: CSS dock rules live only inside @media (min-width: 900px)", () => {
  /* Strengthened (P1a): containment is proven by brace-depth extraction of the
     media body, not by "the rule text appears after the @media line". A stray
     } can close the media block early while dock rules above the break still
     look "inside" to a naive after-media slice — this path uses the true match. */
  const mediaBody = notesDesktopMediaBody(notesCssSrc);
  assert.ok(mediaBody, "@media (min-width: 900px) block present in notes.css");
  assert.match(mediaBody, /body\.map-full\.map-dock\s+#app\s*\{/, "#app dock rule in desktop media");
  assert.match(mediaBody, /body\.map-full\.map-dock\s+#map\s*\{/, "#map dock rule in desktop media");
  assert.match(mediaBody, /body\.map-full\.map-dock\s+#chat\s*\{/, "#chat dock rule in desktop media");

  // Outside = everything not in the brace-matched media span (phone has no wall → no dock).
  const m = /@media\s*\(\s*min-width:\s*900px\s*\)\s*\{/.exec(notesCssSrc);
  assert.ok(m, "media query present");
  const open = m.index + m[0].length - 1;
  let depth = 0, end = -1;
  for (let i = open; i < notesCssSrc.length; i++){
    if (notesCssSrc[i] === "{") depth++;
    else if (notesCssSrc[i] === "}"){
      depth--;
      if (depth === 0){ end = i; break; }
    }
  }
  assert.ok(end > open, "media block has a matching close brace");
  // mediaBody must equal the slice between the matched braces (single source of truth).
  assert.equal(mediaBody, notesCssSrc.slice(open + 1, end),
    "notesDesktopMediaBody matches the brace-walked span exactly");
  const outside = notesCssSrc.slice(0, m.index) + notesCssSrc.slice(end + 1);
  assert.doesNotMatch(outside, /body\.map-full\.map-dock/,
    "dock rules must not appear outside the desktop media query");
});

test("P1: [data-jump] docks when full, setMapFull(false) when not", () => {
  const stop = "2026-01-01T00:00:00Z";
  const node = {
    id: "a", title: "Alpha", description: "", agent: "claude", model: "m",
    effort: "", lane_id: "L", parent: "", ended_at: "", live: "quiet",
    attention: "", created_at: "2026-01-02T00:00:00Z", stops: [stop],
  };
  const body = {
    classList: {
      _set: new Set(["map-full"]),
      toggle(name, on){ if (on) this._set.add(name); else this._set.delete(name); },
      contains(name){ return this._set.has(name); },
      add(name){ this._set.add(name); },
      remove(name){ this._set.delete(name); },
    },
  };
  const storage = memoryStorage({
    [MAP_FULL_KEY]: "1",
    [MAP_FOLD_KNOWN_KEY]: JSON.stringify(["L"]),
    [MAP_FOLD_KEY]: JSON.stringify([]),
  });
  const calls = [];
  const mapwrap = fakeEl("mapwrap");
  const maptoolbar = fakeEl("maptoolbar");
  const feature = createMapFeature({
    roots: { mapwrap, maptoolbar, lanechips: fakeEl("chips"), maptabs: fakeEl("tabs") },
    document: stubDocument(body),
    storage,
    isDesktop: () => true,
    mapOpen: () => true,
    level: () => 1,
    nodes: () => [node],
    groups: () => [],
    laneFilter: () => "",
    uiLoaded: () => true,
    laneModel: () => ({
      lanes: [{ id: "L", name: "Lane" }],
      color: () => "#00f",
      name: () => "Lane",
      byId: { a: node },
    }),
    laneColor: () => "#00f",
    agentLogo: () => "",
    selectNode: (id, why) => calls.push(["select", id, why]),
    loadChatHistory: (id, at) => calls.push(["history", id, at]),
    jumpChatToNow: () => calls.push(["now"]),
  });
  feature.bind();

  // Select a station so the toolbar has [data-jump].
  firstListener(mapwrap, "click")({
    target: fakeTarget({ nid: "a", skey: "a#0", stop }, "[data-nid]"),
  });
  assert.match(maptoolbar.innerHTML, /data-jump="a"/);

  // Full → Open chat docks and stays full.
  firstListener(maptoolbar, "click")({
    target: fakeTarget({ jump: "a" }, "[data-jump]"),
  });
  assert.ok(calls.some(c => c[0] === "select" && c[1] === "a"), "selectNode called");
  assert.equal(feature.isFull(), true, "mapFull stays true when docking");
  assert.equal(body.classList.contains("map-full"), true);
  assert.equal(body.classList.contains("map-dock"), true);
  assert.equal(storage.getItem(mapExports.MAP_DOCK_KEY || "scimux-mapdock"), "1");

  // Not full → Open chat still exits via setMapFull(false) (no dock).
  // Leave full (also clears dock per the leave-full-leaves-dock rule), then
  // fire a wrap-level [data-jump] as the non-full path.
  feature.setFull(false);
  assert.equal(feature.isFull(), false);
  assert.equal(body.classList.contains("map-dock"), false);
  calls.length = 0;
  firstListener(mapwrap, "click")({
    target: fakeTarget({ jump: "a" }, "[data-jump]"),
  });
  assert.ok(calls.some(c => c[0] === "select" && c[1] === "a"), "selectNode still called when not full");
  assert.equal(feature.isFull(), false, "setMapFull(false) path leaves mapFull false");
  assert.equal(body.classList.contains("map-dock"), false, "non-full jump does not dock");
  assert.equal(storage.getItem(MAP_FULL_KEY), null);
  feature.destroy();
});

test("P1: setMapDock(true) does not clear mapSel / mapSelKey / mapSelStop", () => {
  const stop = "2026-01-01T00:00:00Z";
  const node = {
    id: "a", title: "Alpha", description: "", agent: "claude", model: "m",
    effort: "", lane_id: "L", parent: "", ended_at: "", live: "quiet",
    attention: "", created_at: "2026-01-02T00:00:00Z", stops: [stop],
  };
  const body = {
    classList: {
      _set: new Set(["map-full"]),
      toggle(name, on){ if (on) this._set.add(name); else this._set.delete(name); },
      contains(name){ return this._set.has(name); },
      add(name){ this._set.add(name); },
      remove(name){ this._set.delete(name); },
    },
  };
  const storage = memoryStorage({
    [MAP_FULL_KEY]: "1",
    [MAP_FOLD_KNOWN_KEY]: JSON.stringify(["L"]),
    [MAP_FOLD_KEY]: JSON.stringify([]),
  });
  const mapwrap = fakeEl("mapwrap");
  const maptoolbar = fakeEl("maptoolbar");
  const feature = createMapFeature({
    roots: { mapwrap, maptoolbar, lanechips: fakeEl("chips"), maptabs: fakeEl("tabs") },
    document: stubDocument(body),
    storage,
    isDesktop: () => true,
    mapOpen: () => true,
    level: () => 1,
    nodes: () => [node],
    groups: () => [],
    laneFilter: () => "",
    uiLoaded: () => true,
    laneModel: () => ({
      lanes: [{ id: "L", name: "Lane" }],
      color: () => "#00f",
      name: () => "Lane",
      byId: { a: node },
    }),
    laneColor: () => "#00f",
    agentLogo: () => "",
  });
  feature.bind();

  // Establish selection (mapSel=a, mapSelKey=a#0, mapSelStop=stop) via wall tap.
  firstListener(mapwrap, "click")({
    target: fakeTarget({ nid: "a", skey: "a#0", stop }, "[data-nid]"),
  });
  assert.match(maptoolbar.innerHTML, /data-jump="a"/, "selection paints the toolbar");
  assert.match(maptoolbar.innerHTML, /earlier stop/, "mapSelStop is live in the toolbar");
  const htmlBefore = maptoolbar.innerHTML;

  assert.equal(typeof feature.setDock, "function", "setDock is on the factory API");
  feature.setDock(true);

  // Selection is what the dock points at — setMapDock must not clearMapSelection.
  assert.match(maptoolbar.innerHTML, /data-jump="a"/, "mapSel survives setMapDock");
  assert.match(maptoolbar.innerHTML, /earlier stop/, "mapSelStop survives setMapDock");
  assert.equal(maptoolbar.innerHTML, htmlBefore, "toolbar selection chrome is unchanged");
  assert.equal(body.classList.contains("map-dock"), true);
  assert.equal(storage.getItem(mapExports.MAP_DOCK_KEY || "scimux-mapdock"), "1");
  // Contrast: setFull clears selection (existing contract).
  feature.setFull(true); // still full, but clears selection
  assert.equal(maptoolbar.innerHTML, "", "setFull clears selection; setDock must not");
  feature.destroy();
});

/* ---------- P2: dock divider (drag, clamp, persist, keyboard) ---------- */

function fakeBodyStyle(){
  const props = {};
  return {
    setProperty(k, v){ props[k] = String(v); },
    getPropertyValue(k){ return Object.prototype.hasOwnProperty.call(props, k) ? props[k] : ""; },
    removeProperty(k){ delete props[k]; },
    _props: props,
  };
}

function fakeDockBody(seedClasses = ["map-full", "map-dock"]){
  return {
    classList: {
      _set: new Set(seedClasses),
      toggle(name, on){ if (on) this._set.add(name); else this._set.delete(name); },
      contains(name){ return this._set.has(name); },
      add(name){ this._set.add(name); },
      remove(name){ this._set.delete(name); },
    },
    style: fakeBodyStyle(),
  };
}

/** Brace-matched outside of notes.css @media (min-width: 900px). */
function notesDesktopMediaOutside(css){
  const m = /@media\s*\(\s*min-width:\s*900px\s*\)\s*\{/.exec(css);
  if (!m) return null;
  const open = m.index + m[0].length - 1;
  let depth = 0, end = -1;
  for (let i = open; i < css.length; i++){
    if (css[i] === "{") depth++;
    else if (css[i] === "}"){
      depth--;
      if (depth === 0){ end = i; break; }
    }
  }
  if (end < 0) return null;
  return css.slice(0, m.index) + css.slice(end + 1);
}

/** Minimal dock-divider harness: full+dock, #mapdivider, #app geometry, storage. */
function createDockDividerFeature(opts = {}){
  const body = opts.body || fakeDockBody();
  const storage = opts.storage || memoryStorage({
    [MAP_FULL_KEY]: "1",
    [mapExports.MAP_DOCK_KEY || "scimux-mapdock"]: "1",
  });
  const mapdivider = opts.mapdivider || fakeEl("mapdivider");
  mapdivider.setPointerCapture = () => {};
  mapdivider.releasePointerCapture = () => {};
  const app = opts.app || fakeEl("app");
  app.getBoundingClientRect = opts.appRect || (() => ({
    top: 100, height: 1000, bottom: 1100, left: 0, right: 800, width: 800,
  }));
  const mapwrap = opts.mapwrap || fakeEl("mapwrap");
  const maptoolbar = opts.maptoolbar || fakeEl("maptoolbar");
  /* #chat is a static singleton (composer lives here). Peek strip listens on it. */
  const chat = opts.chat || fakeEl("chat");
  const feature = createMapFeature({
    roots: {
      mapdivider, mapwrap, maptoolbar,
      lanechips: fakeEl("chips"), maptabs: fakeEl("tabs"),
      mapfullbtn: fakeEl("mapfullbtn"),
      chat,
    },
    document: stubDocument(body, {
      querySelector(sel){
        if (sel === "#app") return app;
        if (sel === "#mapdivider") return mapdivider;
        if (sel === "#chat") return chat;
        return undefined; // fall through to known-null / throw
      },
    }),
    storage,
    /* opts.isDesktop overrides the desktop default (P2a phone-guard tests). */
    isDesktop: typeof opts.isDesktop === "function" ? opts.isDesktop : () => true,
    mapOpen: () => true,
    level: () => 1,
    nodes: () => opts.nodes || [],
    groups: () => [],
    laneFilter: () => "",
    uiLoaded: () => true,
    laneModel: typeof opts.laneModel === "function"
      ? opts.laneModel
      : () => opts.laneModel || ({
          lanes: [], color: () => "#00f", name: id => id, byId: {},
        }),
    agentLogo: () => "",
    ...opts.deps,
  });
  return { feature, body, storage, mapdivider, app, mapwrap, maptoolbar, chat };
}

test("P2: clampDockFrac clamps to 0.30–0.75; non-finite → 0.55", () => {
  const clamp = mapExports.clampDockFrac;
  assert.equal(typeof clamp, "function", "clampDockFrac is exported");
  assert.equal(clamp(0.1), 0.30);
  assert.equal(clamp(0.9), 0.75);
  assert.equal(clamp(0.5), 0.5);
  assert.equal(clamp(NaN), 0.55);
  assert.equal(clamp(undefined), 0.55);
});

test("P2: dockFracFromDrag arithmetic; appHeight 0 → default", () => {
  const fromDrag = mapExports.dockFracFromDrag;
  assert.equal(typeof fromDrag, "function", "dockFracFromDrag is exported");
  // clientY 100px into a 1000px app starting at top 0 → 0.10 → clamp 0.30
  assert.equal(fromDrag({ clientY: 100, appTop: 0, appHeight: 1000 }), 0.30);
  // midpoint
  assert.equal(fromDrag({ clientY: 600, appTop: 100, appHeight: 1000 }), 0.50);
  // near bottom → clamp 0.75
  assert.equal(fromDrag({ clientY: 1000, appTop: 0, appHeight: 1000 }), 0.75);
  // no measurable app → default, not NaN/Infinity
  assert.equal(fromDrag({ clientY: 500, appTop: 0, appHeight: 0 }), 0.55);
  assert.equal(fromDrag({ clientY: 500, appTop: 0, appHeight: -10 }), 0.55);
});

test("P2: MAP_DOCK_H_KEY is scimux-mapdockh and distinct from every map key", () => {
  assert.equal(mapExports.MAP_DOCK_H_KEY, "scimux-mapdockh");
  const existing = [
    MAP_TAB_KEY, MAP_FOLD_KEY, MAP_FOLD_KNOWN_KEY, MAP_FULL_KEY, MAP_FARE_KEY,
    mapExports.MAP_DOCK_KEY,
  ];
  for (const k of existing){
    assert.notEqual(mapExports.MAP_DOCK_H_KEY, k, `MAP_DOCK_H_KEY must not reuse ${k}`);
  }
});

test("P2: drag writes --dockmap and does not call renderMap / touch mapSig", () => {
  /* Poll-safety guarantee: resize is a CSS variable write only. Rebuilding
     #mapwrap on drag would thrash the wall and can disturb the composer. */
  const node = {
    id: "a", title: "Alpha", description: "", agent: "claude", model: "m",
    effort: "", lane_id: "L", parent: "", ended_at: "", live: "quiet",
    attention: "", created_at: "2026-01-02T00:00:00Z", stops: [],
  };
  const { feature, body, mapdivider, mapwrap, storage } = createDockDividerFeature({
    storage: memoryStorage({
      [MAP_FULL_KEY]: "1",
      [mapExports.MAP_DOCK_KEY || "scimux-mapdock"]: "1",
      [MAP_FOLD_KNOWN_KEY]: JSON.stringify(["L"]),
      [MAP_FOLD_KEY]: JSON.stringify([]),
    }),
    nodes: [node],
    laneModel: () => ({
      lanes: [{ id: "L", name: "Lane" }],
      color: () => "#00f", name: () => "Lane", byId: { a: node },
    }),
  });
  feature.bind();
  feature.render();
  assert.ok(mapwrap.innerHTML.length > 0, "wall rendered once");
  mapwrap.innerHTML = "STALE";

  firstListener(mapdivider, "pointerdown")({
    target: mapdivider, pointerId: 1, clientY: 600, button: 0,
    preventDefault(){},
  });
  firstListener(mapdivider, "pointermove")({
    target: mapdivider, pointerId: 1, clientY: 600,
  });
  // appTop=100, appHeight=1000, clientY=600 → (600-100)/1000 = 0.50 → 50%
  assert.equal(body.style.getPropertyValue("--dockmap"), "50%");
  // mapSig untouched: a subsequent render with unchanged nodes must skip
  feature.render();
  assert.equal(mapwrap.innerHTML, "STALE",
    "drag must not clear mapSig (render skip still holds)");
  // and must not have rebuilt mid-drag either
  assert.equal(mapwrap.innerHTML, "STALE", "drag must not call renderMap");
  // move-only: nothing persisted yet
  assert.equal(storage.getItem(mapExports.MAP_DOCK_H_KEY || "scimux-mapdockh"), null);
  feature.destroy();
});

test("P2: dock height persists on pointerup only; restore clamps hand-edited storage", () => {
  const hKey = () => mapExports.MAP_DOCK_H_KEY || "scimux-mapdockh";
  const storage = memoryStorage({
    [MAP_FULL_KEY]: "1",
    [mapExports.MAP_DOCK_KEY || "scimux-mapdock"]: "1",
  });
  const { feature, body, mapdivider } = createDockDividerFeature({ storage });
  feature.bind();

  firstListener(mapdivider, "pointerdown")({
    target: mapdivider, pointerId: 1, clientY: 100, button: 0,
    preventDefault(){},
  });
  firstListener(mapdivider, "pointermove")({
    target: mapdivider, pointerId: 1, clientY: 700, // moved past threshold
  });
  assert.equal(storage.getItem(hKey()), null, "pointermove does not persist");
  firstListener(mapdivider, "pointerup")({
    target: mapdivider, pointerId: 1, clientY: 700,
  });
  // clientY 700, appTop 100, height 1000 → 0.60
  assert.equal(storage.getItem(hKey()), "0.6");
  assert.equal(body.style.getPropertyValue("--dockmap"), "60%");
  feature.destroy();

  // Hand-edited localStorage "3" must not produce a 300% map.
  const storage2 = memoryStorage({
    [MAP_FULL_KEY]: "1",
    [mapExports.MAP_DOCK_KEY || "scimux-mapdock"]: "1",
    [hKey()]: "3",
  });
  const again = createDockDividerFeature({ storage: storage2 });
  again.feature.bind();
  assert.equal(again.body.style.getPropertyValue("--dockmap"), "75%",
    "stored '3' clamps to max 0.75 → 75%, never 300%");
  again.feature.destroy();
});

test("P2: ArrowUp/ArrowDown nudge the split by 5% and clamp at both ends", () => {
  const hKey = () => mapExports.MAP_DOCK_H_KEY || "scimux-mapdockh";
  const storage = memoryStorage({
    [MAP_FULL_KEY]: "1",
    [mapExports.MAP_DOCK_KEY || "scimux-mapdock"]: "1",
    [hKey()]: "0.5",
  });
  const { feature, body, mapdivider, storage: st } = createDockDividerFeature({ storage });
  feature.bind();
  assert.equal(body.style.getPropertyValue("--dockmap"), "50%");

  const key = firstListener(mapdivider, "keydown");
  assert.ok(key, "keydown listener on #mapdivider");

  key({ target: mapdivider, key: "ArrowDown", preventDefault(){} });
  assert.equal(body.style.getPropertyValue("--dockmap"), "55%");
  assert.equal(st.getItem(hKey()), "0.55");

  key({ target: mapdivider, key: "ArrowUp", preventDefault(){} });
  assert.equal(body.style.getPropertyValue("--dockmap"), "50%");

  // Clamp at top (min map share 30%)
  st.setItem(hKey(), "0.30");
  feature.destroy();
  const lo = createDockDividerFeature({
    storage: memoryStorage({
      [MAP_FULL_KEY]: "1",
      [mapExports.MAP_DOCK_KEY || "scimux-mapdock"]: "1",
      [hKey()]: "0.30",
    }),
  });
  lo.feature.bind();
  firstListener(lo.mapdivider, "keydown")({
    target: lo.mapdivider, key: "ArrowUp", preventDefault(){},
  });
  assert.equal(lo.body.style.getPropertyValue("--dockmap"), "30%");
  lo.feature.destroy();

  // Clamp at bottom (max map share 75%)
  const hi = createDockDividerFeature({
    storage: memoryStorage({
      [MAP_FULL_KEY]: "1",
      [mapExports.MAP_DOCK_KEY || "scimux-mapdock"]: "1",
      [hKey()]: "0.75",
    }),
  });
  hi.feature.bind();
  firstListener(hi.mapdivider, "keydown")({
    target: hi.mapdivider, key: "ArrowDown", preventDefault(){},
  });
  assert.equal(hi.body.style.getPropertyValue("--dockmap"), "75%");
  hi.feature.destroy();
});

test("P2: CSS #mapdivider mirrors .wsdivider (row-resize, pill, hairline, focus)", () => {
  /* Sizing/appearance live inside the 900px block (global rule is display:none
     only — assert the desktop rule body, not the first #mapdivider match). */
  const mediaBody = notesDesktopMediaBody(notesCssSrc);
  assert.ok(mediaBody, "900px media block present");
  const rule = mediaBody.match(/(?:^|\n)\s*#mapdivider\s*\{([^}]+)\}/);
  assert.ok(rule, "#mapdivider sizing rule inside desktop media");
  assert.match(rule[1], /cursor:\s*row-resize/, "vertical drag cursor");
  assert.match(rule[1], /touch-action:\s*none/, "prevent scroll-while-drag");

  const pill = notesCssSrc.match(/#mapdivider::before\s*\{([^}]+)\}/);
  assert.ok(pill, "#mapdivider::before grab pill present");
  assert.match(pill[1], /pointer-events:\s*none/,
    "pill is decoration; the whole grab strip stays the hit target");

  const hair = notesCssSrc.match(/#mapdivider::after\s*\{([^}]+)\}/);
  assert.ok(hair, "#mapdivider::after hairline present");
  assert.match(hair[1], /background:\s*var\(--hairline\)/, "1px hairline via ::after");

  assert.match(notesCssSrc, /#mapdivider:focus-visible\s*\{/,
    ":focus-visible state for keyboard focus");
});

test("P2: CSS #mapdivider size/appearance inside 900px; outside only base hide", () => {
  /* P2a rewrite: the old assertion "no #mapdivider string outside the media
     block" was too strong. The correct contract is hide-global / size-scoped
     (same as .wsdivider and #maptoolbar): a global display:none is required so
     the index.html singleton leaves the phone tab order; sizing stays inside
     the brace-matched 900px block. Brace extraction, not text-order slicing. */
  const mediaBody = notesDesktopMediaBody(notesCssSrc);
  assert.ok(mediaBody, "900px media block present");

  const desk = mediaBody.match(/(?:^|\n)\s*#mapdivider\s*\{([^}]+)\}/);
  assert.ok(desk, "#mapdivider sizing rule inside desktop media");
  /* Thickness comes from --grab so the dock divider and both workspace
     dividers can never drift apart (12px was too fine to hit with a mouse —
     found in acceptance). The negative margin is derived from the same token
     so the visible footprint stays 2px however --grab changes. */
  assert.match(desk[1], /height:\s*var\(--grab\)/, "grab thickness from --grab");
  assert.match(desk[1], /margin:\s*calc\(\(var\(--grab\)\s*-\s*2px\)\s*\/\s*-2\)\s+0/,
    "negative margin derived from --grab, not a literal");
  assert.match(desk[1], /cursor:\s*row-resize/, "row-resize inside 900px");
  assert.match(desk[1], /touch-action:\s*none/, "touch-action inside 900px");
  assert.match(mediaBody, /body\.map-full\.map-dock\s+#mapdivider\s*\{[^}]*display:\s*block/,
    "reveal only under the dock, inside 900px");
  assert.match(mediaBody, /#mapdivider::before\s*\{/, "pill inside 900px");
  assert.match(mediaBody, /#mapdivider::after\s*\{/, "hairline inside 900px");
  assert.match(mediaBody, /#mapdivider:focus-visible\s*\{/, "focus-visible inside 900px");

  const outside = notesDesktopMediaOutside(notesCssSrc);
  assert.ok(outside != null, "outside slice via brace-matched media close");
  // Every outside #mapdivider rule may only hide — never size or show.
  const outsideRules = [...outside.matchAll(/#mapdivider(?![.\w-])[^{]*\{([^}]*)\}/g)];
  for (const m of outsideRules){
    const body = m[1];
    assert.doesNotMatch(body, /height\s*:/, "outside must not size height");
    assert.doesNotMatch(body, /margin\s*:/, "outside must not set margin");
    assert.doesNotMatch(body, /cursor\s*:/, "outside must not set cursor");
    if (/display\s*:/.test(body)){
      assert.match(body, /display:\s*none/, "outside display must be none only");
      assert.doesNotMatch(body, /display:\s*(?!none\b)[\w-]+/,
        "outside must not reveal the divider");
    }
  }
});

test("P2: destroy() removes the divider's listeners", () => {
  const { feature, mapdivider } = createDockDividerFeature();
  feature.bind();
  feature.bind(); // idempotent
  assert.equal(mapdivider._listenerCount("pointerdown"), 1);
  assert.equal(mapdivider._listenerCount("pointermove"), 1);
  assert.equal(mapdivider._listenerCount("pointerup"), 1);
  assert.equal(mapdivider._listenerCount("keydown"), 1);
  const n = mapdivider._totalListeners();
  assert.ok(n >= 4, "divider owns pointer + key listeners");
  feature.destroy();
  assert.equal(mapdivider._totalListeners(), 0, "destroy removes every divider listener");
  // re-bind after destroy works
  feature.bind();
  assert.equal(mapdivider._listenerCount("pointerdown"), 1);
  feature.destroy();
});

/* ---------- P2a: phone hide + viewport-guarded handlers ---------- */

test("P2a: global #mapdivider { display: none } exists outside @media 900px", () => {
  /* Mechanism for "not in the phone tab order": display:none is global, like
     #maptoolbar. Asserted via CSS source — no focus model in the suite (§0). */
  const outside = notesDesktopMediaOutside(notesCssSrc);
  assert.ok(outside != null, "brace-matched outside of 900px media");
  const base = outside.match(/(?:^|\n)\s*#mapdivider\s*\{([^}]+)\}/);
  assert.ok(base, "global #mapdivider rule outside the 900px media block");
  assert.match(base[1], /display:\s*none/, "global rule hides the divider");
  assert.doesNotMatch(base[1], /height\s*:/, "global rule is hide-only, not sized");
  assert.doesNotMatch(base[1], /cursor\s*:/, "global rule is hide-only, not interactive chrome");
});

test("P2a: divider handlers no-op when isDesktop is false", () => {
  /* Belt and braces with the global display:none — notes.js guards isNarrow()
     the same way. Phone must not write --dockmap or persist scimux-mapdockh. */
  const hKey = () => mapExports.MAP_DOCK_H_KEY || "scimux-mapdockh";
  const storage = memoryStorage({
    [MAP_FULL_KEY]: "1",
    [mapExports.MAP_DOCK_KEY || "scimux-mapdock"]: "1",
  });
  const { feature, body, mapdivider } = createDockDividerFeature({
    storage,
    isDesktop: () => false,
  });
  feature.bind();
  // Clear bind-time restore so a handler write is unambiguous.
  if (body.style.removeProperty) body.style.removeProperty("--dockmap");
  else body.style._props && delete body.style._props["--dockmap"];
  storage.removeItem(hKey());

  firstListener(mapdivider, "keydown")({
    target: mapdivider, key: "ArrowDown", preventDefault(){},
  });
  assert.equal(body.style.getPropertyValue("--dockmap"), "",
    "phone keydown must not write --dockmap");
  assert.equal(storage.getItem(hKey()), null,
    "phone keydown must not persist scimux-mapdockh");

  firstListener(mapdivider, "pointerdown")({
    target: mapdivider, pointerId: 1, clientY: 100, button: 0,
    preventDefault(){},
  });
  firstListener(mapdivider, "pointermove")({
    target: mapdivider, pointerId: 1, clientY: 700,
  });
  firstListener(mapdivider, "pointerup")({
    target: mapdivider, pointerId: 1, clientY: 700,
  });
  assert.equal(body.style.getPropertyValue("--dockmap"), "",
    "phone pointer drag must not write --dockmap");
  assert.equal(storage.getItem(hKey()), null,
    "phone pointer drag must not persist scimux-mapdockh");
  feature.destroy();
});

/* ---------- P3: dock peek + Escape ladder ---------- */

const appSrcForDock = readFileSync(join(__dirname, "../js/app.js"), "utf8");

/** Mirror of the app.js Escape dispatch (P3). Tests drive the ladder through
 *  the same pure step + feature API the shell wires — no second document listener. */
function appEscapeDockDispatch(feature){
  const step = mapExports.escapeDockStep({
    mapFull: feature.isFull(),
    mapDock: feature.isDock(),
    dockPeek: typeof feature.isPeek === "function" ? feature.isPeek() : false,
  });
  if (step === "peek") feature.setPeek(true);
  else if (step === "undock") feature.setDock(false);
  else if (step === "exit-full") feature.setFull(false);
  return step;
}

test("escapeDockStep full truth table", () => {
  const step = mapExports.escapeDockStep;
  assert.equal(typeof step, "function", "escapeDockStep is exported");

  // mapFull:false → "none" at either mapDock (phone / not full).
  for (const mapDock of [false, true]){
    assert.equal(step({ mapFull: false, mapDock }), "none", `mapFull:false → none (mapDock=${mapDock})`);
  }

  // Ladder under full screen: close the pop-out, then leave full screen.
  assert.equal(step({ mapFull: true, mapDock: true }), "undock");
  assert.equal(step({ mapFull: true, mapDock: false }), "exit-full");

  // A stale peek flag from an older build must not resurrect a third rung.
  assert.equal(step({ mapFull: true, mapDock: true, dockPeek: true }), "undock");
});

/* ---------- P4: scroll anchoring across poll rebuilds ---------- */

/** One stop per node (created_at only) so row indices are trivial: newest first. */
function p4Node(id, createdAt, title){
  return {
    id, title: title || id, description: "", agent: "claude", model: "m",
    effort: "", lane_id: "L", parent: "", ended_at: "", live: "quiet",
    attention: "", created_at: createdAt, stops: [],
  };
}

/** Wall-map harness with #mapscroll for P4 scrollTop assertions.
 *  P6 injects #mappill (sibling of #maptoolbar, never inside #mapwrap). */
function createWallScrollFeature(nodes, opts = {}){
  const mapwrap = fakeEl("mapwrap");
  const mapscroll = fakeEl("mapscroll");
  const maptoolbar = fakeEl("maptoolbar");
  const mappill = opts.mappill || fakeEl("mappill");
  /* Viewport geometry for attentionPill / positionMapPill — not real layout. */
  if (opts.clientHeight != null) mapscroll.clientHeight = opts.clientHeight;
  if (opts.scrollTop != null) mapscroll.scrollTop = opts.scrollTop;
  const body = {
    classList: {
      _set: new Set(["map-full"]),
      toggle(name, on){ if (on) this._set.add(name); else this._set.delete(name); },
      contains(name){ return this._set.has(name); },
      add(name){ this._set.add(name); },
      remove(name){ this._set.delete(name); },
    },
  };
  /* The wall's viewport height is not constant: docking hands half of it to the
     chat and a divider drag rewrites it continuously. There is no layout engine
     here, so the harness stands in for one —
       dockedClientHeight → the height returned while body carries .map-dock
       clientHeightRef    → a mutable { v } a test can rewrite mid-drag
     Both feed the same clientHeight the feature reads. */
  if (opts.dockedClientHeight != null || opts.clientHeightRef){
    Object.defineProperty(mapscroll, "clientHeight", {
      configurable: true,
      get(){
        if (opts.clientHeightRef) return opts.clientHeightRef.v;
        return body.classList.contains("map-dock")
          ? opts.dockedClientHeight
          : opts.clientHeight;
      },
    });
  }
  const storage = memoryStorage({
    [MAP_FULL_KEY]: "1",
    [MAP_FOLD_KNOWN_KEY]: JSON.stringify(["L"]),
    [MAP_FOLD_KEY]: JSON.stringify([]),
    ...(opts.storageSeed || {}),
  });
  const byId = {};
  for (const n of nodes) byId[n.id] = n;
  const feature = createMapFeature({
    roots: {
      mapwrap, mapscroll, maptoolbar, mappill,
      lanechips: fakeEl("chips"), maptabs: fakeEl("tabs"),
      mapfullbtn: fakeEl("mapfullbtn"),
      /* Optional: the dock divider drags the wall's own viewport height. */
      mapdivider: opts.mapdivider,
      app: opts.app,
    },
    document: stubDocument(body),
    storage,
    isDesktop: () => true,
    mapOpen: () => true,
    level: () => 1,
    nodes: () => nodes,
    groups: () => [],
    laneFilter: () => "",
    uiLoaded: () => true,
    laneModel: () => ({
      lanes: [{ id: "L", name: "Lane" }],
      color: () => "#00f",
      name: () => "Lane",
      byId,
    }),
    laneColor: () => "#00f",
    agentLogo: () => "",
    selectNode: opts.selectNode,
    loadChatHistory: opts.loadChatHistory,
    jumpChatToNow: opts.jumpChatToNow,
  });
  feature.bind();
  feature.restoreChrome();
  return {
    feature, mapwrap, mapscroll, maptoolbar, mappill, body, storage, nodes, byId,
    mapdivider: opts.mapdivider,
  };
}

function selectWallStop(mapwrap, { nid, skey, stop }){
  firstListener(mapwrap, "click")({
    target: fakeTarget({ nid, skey, stop: stop || "" }, "[data-nid]"),
  });
}

test("P4: anchoredScrollTop pure arithmetic — insert/remove/unchanged/null/clamp", () => {
  const fn = mapExports.anchoredScrollTop;
  assert.equal(typeof fn, "function", "anchoredScrollTop is exported");

  // One newer row inserted above: selection index 1 → 2, scroll +76.
  assert.equal(fn({ prevTop: 100, prevIndex: 1, nextIndex: 2, rowHeight: 76 }), 176);
  // One row removed above: index 2 → 1, scroll −76.
  assert.equal(fn({ prevTop: 176, prevIndex: 2, nextIndex: 1, rowHeight: 76 }), 100);
  // Unchanged index: no movement.
  assert.equal(fn({ prevTop: 50, prevIndex: 3, nextIndex: 3, rowHeight: 76 }), 50);
  // Missing selection / vanished stop → leave prevTop alone.
  assert.equal(fn({ prevTop: 40, prevIndex: null, nextIndex: 1, rowHeight: 76 }), 40);
  assert.equal(fn({ prevTop: 40, prevIndex: undefined, nextIndex: 1, rowHeight: 76 }), 40);
  assert.equal(fn({ prevTop: 40, prevIndex: 1, nextIndex: null, rowHeight: 76 }), 40);
  assert.equal(fn({ prevTop: 40, prevIndex: 1, nextIndex: undefined, rowHeight: 76 }), 40);
  // Would go negative → clamp to 0.
  assert.equal(fn({ prevTop: 10, prevIndex: 2, nextIndex: 0, rowHeight: 76 }), 0);
});

test("P4: WALL_ROW_H exported as 76; renderWallMap uses it (not a second literal)", () => {
  assert.equal(mapExports.WALL_ROW_H, 76, "WALL_ROW_H is the shared row height");
  // Wall geometry must share the constant; stack map keeps its own RH = 76.
  assert.match(mapSrc, /const RH = WALL_ROW_H\b/,
    "renderWallMap must set RH from WALL_ROW_H");
  assert.match(mapSrc, /const RH = 76\b/,
    "stack map keeps its own RH = 76 literal (do not widen the diff)");

  const a = p4Node("a", "2026-01-01T00:00:00Z", "Alpha");
  const b = p4Node("b", "2026-01-02T00:00:00Z", "Beta");
  const { feature, mapwrap } = createWallScrollFeature([a, b]);
  feature.render();
  // svgH = OFF(16) + nRows * WALL_ROW_H(76). Two rows → 16 + 152 = 168.
  const rh = mapExports.WALL_ROW_H || 76;
  const expectedH = 16 + 2 * rh;
  assert.match(mapwrap.innerHTML, new RegExp(`height="${expectedH}"`),
    "wall SVG height is OFF + nRows * WALL_ROW_H (geometry matches the constant)");
  assert.match(mapwrap.innerHTML, /class="strow/, "wall rows rendered");
  feature.destroy();
});

test("P4: re-render with one newer row anchors scrollTop by +76 (selection unchanged)", () => {
  const a = p4Node("a", "2026-01-01T00:00:00Z", "Alpha");
  const b = p4Node("b", "2026-01-02T00:00:00Z", "Beta");
  const nodes = [a, b];
  const { feature, mapwrap, mapscroll, maptoolbar, byId } = createWallScrollFeature(nodes);
  feature.render();
  // Select the older station (a#0). Newest-first: b at 0, a at 1.
  selectWallStop(mapwrap, { nid: "a", skey: "a#0", stop: a.created_at });
  assert.match(maptoolbar.innerHTML, /data-jump="a"/, "selection is live");
  mapscroll.scrollTop = 100;

  // Poll: a newer stop appears at the top → a moves from index 1 → 2.
  const c = p4Node("c", "2026-01-03T00:00:00Z", "Gamma");
  nodes.push(c);
  byId.c = c;
  feature.render();

  assert.equal(mapscroll.scrollTop, 176,
    "one row inserted above moves scrollTop by exactly WALL_ROW_H (76)");
  assert.match(maptoolbar.innerHTML, /data-jump="a"/,
    "selection key is unchanged across the poll rebuild");
  feature.destroy();
});

test("P4: selection change leaves scrollTop untouched", () => {
  const a = p4Node("a", "2026-01-01T00:00:00Z", "Alpha");
  const b = p4Node("b", "2026-01-02T00:00:00Z", "Beta");
  const { feature, mapwrap, mapscroll, maptoolbar } = createWallScrollFeature([a, b]);
  feature.render();
  selectWallStop(mapwrap, { nid: "a", skey: "a#0", stop: a.created_at });
  mapscroll.scrollTop = 100;

  // Tap a different station — prevIndex/nextIndex describe two different stops;
  // the delta is meaningless and must not yank the viewport.
  selectWallStop(mapwrap, { nid: "b", skey: "b#0", stop: b.created_at });
  assert.match(maptoolbar.innerHTML, /data-jump="b"/, "selection moved to b");
  assert.equal(mapscroll.scrollTop, 100,
    "changing mapSelKey must not apply anchor arithmetic");
  feature.destroy();
});

test("P4: no selection leaves scrollTop untouched on rebuild", () => {
  const a = p4Node("a", "2026-01-01T00:00:00Z", "Alpha");
  const b = p4Node("b", "2026-01-02T00:00:00Z", "Beta");
  const nodes = [a, b];
  const { feature, mapscroll, maptoolbar, byId } = createWallScrollFeature(nodes);
  feature.render();
  assert.equal(maptoolbar.innerHTML, "", "no selection");
  mapscroll.scrollTop = 55;

  const c = p4Node("c", "2026-01-03T00:00:00Z", "Gamma");
  nodes.push(c);
  byId.c = c;
  feature.render();

  assert.equal(mapscroll.scrollTop, 55,
    "without a selection, poll rebuild must not fight the user's scroll");
  feature.destroy();
});

/* ---------- P5 (arc1): attention rail (locator gutter on the wall map) ---------- */

const indexHtmlSrc = readFileSync(join(__dirname, "../index.html"), "utf8");

/** Wall-row fraction of content height — same formula as railTicks / renderWallMap. */
function p5Frac(i, nRows, rowHeight = 76, offset = 16){
  return (offset + i * rowHeight + rowHeight / 2) / (offset + nRows * rowHeight);
}

test("P5/P6: railTicks — hard-attention heads, frac arithmetic, empty cases", () => {
  const fn = mapExports.railTicks;
  assert.equal(typeof fn, "function", "railTicks is exported");

  // Five rows; hard-attention heads at indices 1 and 3. Non-head of an asking
  // node at index 0 must contribute nothing (attention is a node property; the
  // wall only marks the head stop — the pill geometry must match).
  const askA = {
    id: "a", title: "Alpha", attention: "approval",
    created_at: "t0", stops: [], live: "quiet", lane_id: "L",
  };
  const quiet = {
    id: "q", title: "Quiet", attention: "",
    created_at: "t1", stops: [], live: "quiet", lane_id: "L",
  };
  const askB = {
    id: "b", title: "Beta", attention: "question",
    created_at: "t2", stops: [], live: "quiet", lane_id: "L",
  };
  // Multi-stop asking node: non-head + head. Only the head tick.
  const multi = {
    id: "m", title: "Multi", attention: "approval",
    created_at: "2026-01-01T00:00:00Z",
    stops: ["2026-01-02T00:00:00Z"],
    live: "quiet", lane_id: "L",
  };
  const multiStops = stopsOf(multi); // [non-head i=0, head i=1]
  assert.equal(multiStops[0].head, false);
  assert.equal(multiStops[1].head, true);

  const rows = [
    multiStops[0], // 0: non-head of asking node — no tick
    { n: askA, i: 0, time: "t0", head: true },  // 1: asking head
    { n: quiet, i: 0, time: "t1", head: true }, // 2: quiet
    { n: askB, i: 0, time: "t2", head: true },  // 3: asking head
    multiStops[1], // 4: head of multi — tick
  ];
  const RH = 76, OFF = 16;
  const ticks = fn({ rows, rowHeight: RH, offset: OFF });
  assert.equal(ticks.length, 3, "one tick per hard-attention HEAD only (not per stop)");
  assert.deepEqual(ticks.map(t => t.nodeId), ["a", "b", "m"]);
  assert.deepEqual(ticks.map(t => t.stopKey), ["a#0", "b#0", "m#1"]);
  assert.deepEqual(ticks.map(t => t.kind), ["waiting", "waiting", "waiting"]);
  assert.equal(ticks[0].frac, p5Frac(1, 5, RH, OFF));
  assert.equal(ticks[1].frac, p5Frac(3, 5, RH, OFF));
  assert.equal(ticks[2].frac, p5Frac(4, 5, RH, OFF));
  // Ordering matches row order (top-down), not sorted by node id.
  assert.ok(ticks[0].frac < ticks[1].frac && ticks[1].frac < ticks[2].frac);

  // No attention / finished anywhere → [].
  const quietRows = rows.map(s => ({
    ...s, n: { ...s.n, attention: "", turn_done: false },
  }));
  assert.deepEqual(fn({ rows: quietRows, rowHeight: RH, offset: OFF }), []);

  // Empty rows → [].
  assert.deepEqual(fn({ rows: [], rowHeight: RH, offset: OFF }), []);
  assert.deepEqual(fn({ rows: null, rowHeight: RH, offset: OFF }), []);
});

test("P6: wall pill shows only when an asking/finished station is off-screen", () => {
  /* Replaces the P5 rail-tick button assertions: the pill is a single control
     whose show/label come from attentionPill, not one button per tick. */
  const nodes = [
    p4Node("n0", "2026-01-01T00:00:00Z", "N0"),
    p4Node("n1", "2026-01-02T00:00:00Z", "N1"),
    p4Node("n2", "2026-01-03T00:00:00Z", "N2"),
    p4Node("n3", "2026-01-04T00:00:00Z", "N3"),
    p4Node("n4", "2026-01-05T00:00:00Z", "N4"),
  ];
  nodes[1].attention = "approval"; // n1 → row index 3
  nodes[3].attention = "question"; // n3 → row index 1
  // Viewport shorter than content so both ticks are off-screen.
  // contentH = 16 + 5*76 = 396. Viewport height 80 → only the top of the list.
  const { feature, mappill, mapscroll } = createWallScrollFeature(nodes, {
    clientHeight: 80, scrollTop: 0,
  });
  feature.render();
  assert.ok(mappill.classList.contains("on"), "pill .on when ticks are off-screen");
  assert.match(mappill.textContent, /2 waiting/);
  assert.match(mappill.textContent, /↓|↑/);
  assert.equal(mappill.dataset.nid, "n3", "targets the nearest off-screen (top-side first below)");
  assert.equal(mappill.dataset.skey, "n3#0");
  assert.equal(mappill._listenerCount("click"), 1,
    "exactly one click listener on the singleton pill");

  // Scroll so both asking heads fall inside a tall viewport → hide.
  mapscroll.clientHeight = 500;
  mapscroll.scrollTop = 0;
  // Re-trigger geometry via the scroll handler (rAF-throttled; tests have no
  // rAF → falls through to the direct path).
  firstListener(mapscroll, "scroll")({});
  assert.equal(mappill.classList.contains("on"), false,
    "pill hides when every asking station is on-screen");
  feature.destroy();
});

test("P6: zero asking stations leaves pill without .on", () => {
  const quiet = [
    p4Node("a", "2026-01-01T00:00:00Z", "A"),
    p4Node("b", "2026-01-02T00:00:00Z", "B"),
  ];
  const { feature, mappill } = createWallScrollFeature(quiet, { clientHeight: 40 });
  feature.render();
  assert.equal(mappill.classList.contains("on"), false,
    "empty pill is not .on (must not swallow clicks)");
  assert.equal(mappill.textContent, "", "empty pill has no label");
  feature.destroy();
});

test("P6: tapping the pill selects the target and does not open chat", () => {
  /* The pill locates; the ring opens. Two gestures, two outcomes. */
  const a = p4Node("a", "2026-01-01T00:00:00Z", "Alpha");
  const b = p4Node("b", "2026-01-02T00:00:00Z", "Beta");
  a.attention = "approval";
  b.attention = "question";
  const calls = [];
  const { feature, mapwrap, maptoolbar, mappill } = createWallScrollFeature([a, b], {
    clientHeight: 40, scrollTop: 0,
    selectNode: (id, why) => calls.push(["select", id, why]),
    jumpChatToNow: () => calls.push(["now"]),
  });
  // Make row queryable for setMapSel's .current paint.
  mapwrap.querySelector = stubQuerySelector(sel => {
    if (String(sel).includes("a#0") || String(sel).includes('nid="a"')
      || String(sel).includes("a")) {
      return {
        dataset: { nid: "a", skey: "a#0", stop: "" },
        classList: { toggle(){}, contains(){ return false; } },
        scrollIntoView(){ calls.push(["scroll"]); },
      };
    }
    return null; // known strow form, not found / not matching
  });
  feature.render();
  assert.ok(mappill.classList.contains("on"), "pill visible for off-screen asking");
  assert.equal(maptoolbar.innerHTML, "", "no selection yet");

  firstListener(mappill, "click")({
    currentTarget: mappill,
    target: mappill,
  });
  assert.match(maptoolbar.innerHTML, /data-jump="a"|data-jump="b"/,
    "pill tap selects a station (toolbar paints for mapSel)");
  assert.ok(!calls.some(c => c[0] === "now"),
    "pill must not open chat (that is the ring's job)");
  assert.ok(!calls.some(c => c[0] === "select" && c[2] === "jump"),
    "pill must not call selectNode(..., 'jump')");
  feature.destroy();
});

test("P6: successive wall renders keep one pill listener (no re-bind)", () => {
  const nodes = [
    p4Node("a", "2026-01-01T00:00:00Z", "A"),
    p4Node("b", "2026-01-02T00:00:00Z", "B"),
    p4Node("c", "2026-01-03T00:00:00Z", "C"),
  ];
  nodes[0].attention = "approval";
  nodes[2].attention = "question";
  const { feature, mappill, byId } = createWallScrollFeature(nodes, {
    clientHeight: 40,
  });
  feature.render();
  assert.equal(mappill._listenerCount("click"), 1,
    "exactly one click listener on the singleton pill");

  byId.a.title = "A-renamed";
  nodes[0].title = "A-renamed";
  feature.render();
  assert.equal(mappill._listenerCount("click"), 1,
    "re-render must not re-bind click (no orphaned listeners)");
  feature.destroy();
});

/** A tap on the attention ring's transparent hit circle. */
function ringTapEvent(id){
  const target = {
    dataset: { jump: id },
    classList: { contains: n => n === "attnstation-hit" },
  };
  target.closest = stubClosest({
    ".attnstation-hit": target,
    "[data-jump]": target,
  });
  return { target };
}

test("P6: a ready-only pill wears --work, never the blocked-on-you yellow", () => {
  /* P5 settled that --attn means exactly one thing: blocked on you. A pill that
     says "2 ready" while painted yellow re-merges the two meanings the phase
     just separated — and points at stations painted --work. */
  const base = mapCssSrc.match(/(?:^|\n)\s*#mappill\s*\{([^}]+)\}/);
  assert.ok(base, "#mappill base rule exists in map.css");
  assert.match(base[1], /var\(--attn\)/, "the default pill is the attention hue");
  const ready = mapCssSrc.match(/#mappill\.ready\s*\{([^}]+)\}/);
  assert.ok(ready, "#mappill.ready rule exists");
  assert.match(ready[1], /var\(--work\)/, "a ready pill uses the finished hue");
  assert.doesNotMatch(ready[1], /var\(--attn\)/, "and drops --attn entirely");

  // And the class is applied only when the label actually says "ready".
  const nodes = [
    p4Node("a", "2026-01-01T00:00:00Z", "A"),
    p4Node("b", "2026-01-02T00:00:00Z", "B"),
    p4Node("c", "2026-01-03T00:00:00Z", "C"),
  ];
  nodes[0].turn_done = true; // oldest → bottom row, off-screen
  const { feature, mappill } = createWallScrollFeature(nodes, { clientHeight: 40 });
  feature.render();
  assert.ok(mappill.classList.contains("on"));
  assert.match(mappill.textContent, /1 ready/);
  assert.ok(mappill.classList.contains("ready"), "ready-only pill carries .ready");

  // One waiting station joins it: the pill goes back to waiting, and yellow.
  nodes[2].attention = "approval";
  feature.render();
  assert.match(mappill.textContent, /1 waiting/);
  assert.equal(mappill.classList.contains("ready"), false,
    "a waiting pill must not keep the ready hue");
  feature.destroy();
});

test("P8: the pill's label is legible on both of its fills, in both themes", () => {
  /* Observed live on the wall map: the pill rendered as a bare drop shadow with
     an invisible label. Two causes, both in these two rules.
     (1) The fill was color-mix(... var(--fg)) and --fg does not exist in this
         palette, so the whole background-color was invalid → transparent. The
         repo-wide guard in smoke.test.js now catches that class; this case pins
         the fix in place.
     (2) The label was color: var(--bg) — warm paper — which is invisible on a
         transparent fill and wrong on yellow anyway. --ink is no good either:
         it flips with the theme, and --attn stays bright yellow in dark mode,
         so a theme-following label goes near-white on yellow. Yellow needs a
         fixed dark label; the petrol .ready fill needs a light one (the house
         pattern for filled --work, cf. base.css .btn-primary). */
  const base = mapCssSrc.match(/(?:^|\n)\s*#mappill\s*\{([^}]+)\}/);
  assert.ok(base, "#mappill base rule exists in map.css");
  const ready = mapCssSrc.match(/#mappill\.ready\s*\{([^}]+)\}/);
  assert.ok(ready, "#mappill.ready rule exists");

  for (const [name, body] of [["#mappill", base[1]], ["#mappill.ready", ready[1]]]){
    assert.doesNotMatch(body, /var\(--fg\)/, `${name} must not use the non-existent --fg`);
    const bg = body.match(/background\s*:\s*([^;]+)/);
    assert.ok(bg, `${name} sets a background`);
    assert.doesNotMatch(bg[1], /var\(--(?!attn\b|work\b)[\w-]+\)/,
      `${name}'s fill may only reference the state hues, no other token`);
  }

  // Yellow fill → fixed dark label, not a theme-following one.
  const baseColor = base[1].match(/(?:^|;|\s)color\s*:\s*([^;]+)/);
  assert.ok(baseColor, "#mappill sets a label colour");
  assert.doesNotMatch(baseColor[1], /var\(--bg\)|var\(--ink\)|var\(--surface\)/,
    "a theme-following label is near-white on yellow in dark mode");
  assert.match(baseColor[1], /#[0-9a-fA-F]{3,6}|\brgb/,
    "the label on yellow is a fixed dark colour");

  // Petrol fill → light label, the same choice filled --work already makes.
  const readyColor = ready[1].match(/(?:^|;|\s)color\s*:\s*([^;]+)/);
  assert.ok(readyColor, "#mappill.ready sets its own label colour");
  assert.match(readyColor[1], /#fff\b|#ffffff\b|\bwhite\b/i,
    "a filled --work surface carries white text (base.css .btn-primary)");

  /* Added after the red commit, on measurement rather than on taste: dark-mode
     --work is #2FA7B4, where white lands at 2.9:1 — under AA for a 14px bold
     label — while near-black is 6.0:1. The pill has to be read at a glance
     from across the wall, so it overrides the house white in dark mode. */
  const dark = mapCssSrc.match(/@media\s*\(prefers-color-scheme:\s*dark\)\s*\{[\s\S]*?#mappill\.ready\s*\{([^}]+)\}/);
  assert.ok(dark, "#mappill.ready has a dark-scheme label override");
  assert.match(dark[1], /color\s*:\s*#1C1C1E/i, "dark-mode ready label is near-black");
});

test("P6: entering the dock recomputes the pill — the ring's own gesture shrinks the map", () => {
  /* The ring tap docks the chat, which hands half the map's height to it. The
     pill's whole input is that height, so the gesture P6 introduced invalidates
     the verdict P6 painted. Nothing else recomputes it: an idle wall skips
     renderMap on every poll, so a stale pill stays stale indefinitely. */
  const nodes = [
    p4Node("n0", "2026-01-01T00:00:00Z", "N0"),
    p4Node("n1", "2026-01-02T00:00:00Z", "N1"),
    p4Node("n2", "2026-01-03T00:00:00Z", "N2"),
    p4Node("n3", "2026-01-04T00:00:00Z", "N3"),
    p4Node("n4", "2026-01-05T00:00:00Z", "N4"),
  ];
  nodes[1].attention = "approval"; // row index 3 → y = 16 + 3*76 + 38 = 282
  const { feature, mapwrap, mappill, body } = createWallScrollFeature(nodes, {
    clientHeight: 500,       // undocked: content is 396 tall, everything fits
    dockedClientHeight: 80,  // docked: the wall keeps a sliver
    scrollTop: 0,
    selectNode: () => {},
    jumpChatToNow: () => {},
  });
  feature.render();
  assert.equal(mappill.classList.contains("on"), false,
    "undocked: the whole wall fits, so no pill");

  firstListener(mapwrap, "click")(ringTapEvent("n1"));
  assert.equal(body.classList.contains("map-dock"), true, "the ring docked the chat");
  assert.ok(mappill.classList.contains("on"),
    "docking pushed the asking station off-screen — the pill must say so");
  assert.match(mappill.textContent, /1 waiting/);
  feature.destroy();
});

test("P6: a divider drag recomputes the pill", () => {
  /* Same failure by the other route: applyDockFrac writes --dockmap and is
     documented as never re-rendering the map — correct, and the reason the pill
     has to be repositioned explicitly. */
  const nodes = [
    p4Node("a", "2026-01-01T00:00:00Z", "A"),
    p4Node("b", "2026-01-02T00:00:00Z", "B"),
    p4Node("c", "2026-01-03T00:00:00Z", "C"),
  ];
  nodes[0].attention = "approval"; // oldest → bottom row
  const viewport = { v: 500 };     // content is 16 + 3*76 = 244 tall
  const mapdivider = fakeEl("mapdivider");
  mapdivider.setPointerCapture = () => {};
  mapdivider.releasePointerCapture = () => {};
  const app = fakeEl("app");
  app.getBoundingClientRect = () => ({
    top: 0, height: 1000, bottom: 1000, left: 0, right: 800, width: 800,
  });
  const { feature, mappill } = createWallScrollFeature(nodes, {
    clientHeightRef: viewport, scrollTop: 0, mapdivider, app,
  });
  feature.render();
  assert.equal(mappill.classList.contains("on"), false, "tall map: nothing off-screen");

  firstListener(mapdivider, "pointerdown")({
    target: mapdivider, pointerId: 1, clientY: 600, button: 0,
    preventDefault(){},
  });
  viewport.v = 60; // the drag handed the height to the chat
  firstListener(mapdivider, "pointermove")({
    target: mapdivider, pointerId: 1, clientY: 300,
  });
  assert.ok(mappill.classList.contains("on"),
    "the drag changed the viewport — the pill must recompute");
  assert.match(mappill.textContent, /1 waiting/);
  feature.destroy();
});

/* ---------- P5a: chrome that assumed map and chat are never both visible ---------- */

test("P5a: the bookmarks chevron is hidden in full screen (no dead control)", () => {
  /* #bookmarksbtn toggles body.bookmarks-open. Under body.map-full the wall
     earns the whole width, so the pane it opens is hidden — leaving a control
     that appears to do nothing. Hide the control instead. */
  const mediaBody = notesDesktopMediaBody(notesCssSrc);
  assert.ok(mediaBody, "900px media block present");
  const rule = mediaBody.match(/body\.map-full\s+#bookmarksbtn\s*\{([^}]+)\}/);
  assert.ok(rule, "body.map-full #bookmarksbtn rule inside desktop media");
  assert.match(rule[1], /display:\s*none/, "chevron hidden in full screen");
});

test("P5a: bookmarks-open cannot reveal the pane over the wall map", () => {
  /* body.bookmarks-open #bookmarkspane { display: flex } (notes.css) and
     body.map-full #bookmarkspane { display: none } have equal specificity, so
     source order decided it and the pane won — sliding in over the full-screen
     wall. Pin an explicit higher-specificity rule so order can never decide. */
  const mediaBody = notesDesktopMediaBody(notesCssSrc);
  assert.ok(mediaBody, "900px media block present");
  assert.match(mediaBody, /body\.map-full\s+#bookmarkspane[^{]*\{[^}]*display:\s*none/,
    "full screen hides the bookmarks pane");
  const both = mediaBody.match(/body\.map-full\.bookmarks-open\s+#bookmarkspane\s*\{([^}]+)\}/);
  assert.ok(both, "explicit body.map-full.bookmarks-open #bookmarkspane rule");
  assert.match(both[1], /display:\s*none/,
    "an open bookmarks state must not reveal the pane in full screen");
});

test("P6: #mappill is a named button control (not a bare div)", () => {
  /* Replaces the P5a maprail landmark pin: the pill is a real <button> so it
     is announced without a role=navigation wrapper. */
  const html = readFileSync(join(__dirname, "../index.html"), "utf8");
  const tag = html.match(/<button[^>]*id="mappill"[^>]*>/);
  assert.ok(tag, "#mappill present as a button in index.html");
  assert.match(tag[0], /aria-label="[^"]+"/, "control is named");
  assert.match(tag[0], /type="button"/, "type=button so it never submits");
});

test("P5b: --grab is one token for all three dividers, comfortably mouse-sized", () => {
  /* Three divider instances, two idioms: #mapdivider (dock) and .wsdivider on
     the workspace's inbox|nav and nav|note boundaries. 12px was hard to point
     at with a mouse; the token keeps them in step and the derived margins keep
     the visible footprint at 2px so no layout moved. */
  const tokens = readFileSync(join(__dirname, "../css/tokens.css"), "utf8");
  const grab = tokens.match(/--grab:\s*([0-9]+)px/);
  assert.ok(grab, "--grab token defined in tokens.css");
  assert.ok(Number(grab[1]) >= 20,
    `--grab is ${grab[1]}px; a mouse-pointable divider needs >= 20px`);

  const ws = notesCssSrc.match(/(?:^|\n)\.wsdivider\s*\{([^}]+)\}/);
  assert.ok(ws, ".wsdivider base rule present");
  assert.match(ws[1], /width:\s*var\(--grab\)/, ".wsdivider width from --grab");
  assert.match(ws[1], /margin:\s*0\s+calc\(\(var\(--grab\)\s*-\s*2px\)\s*\/\s*-2\)/,
    ".wsdivider margin derived from --grab");
  assert.doesNotMatch(ws[1], /width:\s*12px/, "no stale 12px literal");
});

test("action bar: a command dismisses the bar, a tap on the still-selected row reopens it", () => {
  /* HIG treats a selection-scoped command surface (edit menu, contextual
     menu, popover) as dismissed by its own command. The bar was bound to
     mapSel alone, so "Open chat" left it floating over the wall, where on a
     pointer device it swallows the scroll gesture. The selection must stay:
     it is the row highlight, P4's scroll anchor, and what the docked chat is
     pointed at. So the bar needs its own open flag — and because the
     selection survives, a second tap on the same row must reopen the bar
     rather than toggle the selection off. */
  assert.equal(typeof mapExports.stationTapAction, "function", "stationTapAction exported");
  const sel = { mapSel: "a", mapSelKey: "a#0" };
  assert.equal(mapExports.stationTapAction({ id: "a", key: "a#0", barOpen: false, ...sel }),
    "reopen", "closed bar on the selected row reopens");
  assert.equal(mapExports.stationTapAction({ id: "a", key: "a#0", barOpen: true, ...sel }),
    "toggle", "open bar on the selected row toggles the selection off");
  assert.equal(mapExports.stationTapAction({ id: "b", key: "b#0", barOpen: false, ...sel }),
    "toggle", "a different row is an ordinary selection change");

  const stop = "2026-01-01T00:00:00Z";
  const node = {
    id: "a", title: "Alpha", description: "", agent: "claude", model: "m",
    effort: "", lane_id: "L", parent: "", ended_at: "", live: "quiet",
    attention: "", created_at: "2026-01-02T00:00:00Z", stops: [stop],
  };
  const body = {
    classList: {
      _set: new Set(["map-full"]),
      toggle(name, on){ if (on) this._set.add(name); else this._set.delete(name); },
      contains(name){ return this._set.has(name); },
      add(name){ this._set.add(name); },
      remove(name){ this._set.delete(name); },
    },
  };
  const mapwrap = fakeEl("mapwrap");
  const maptoolbar = fakeEl("maptoolbar");
  const feature = createMapFeature({
    roots: { mapwrap, maptoolbar, lanechips: fakeEl("chips"), maptabs: fakeEl("tabs") },
    document: stubDocument(body),
    storage: memoryStorage({
      [MAP_FULL_KEY]: "1",
      [MAP_FOLD_KNOWN_KEY]: JSON.stringify(["L"]),
      [MAP_FOLD_KEY]: JSON.stringify([]),
    }),
    isDesktop: () => true,
    mapOpen: () => true,
    level: () => 1,
    nodes: () => [node],
    groups: () => [],
    laneFilter: () => "",
    uiLoaded: () => true,
    laneModel: () => ({
      lanes: [{ id: "L", name: "Lane" }], color: () => "#00f",
      name: () => "Lane", byId: { a: node },
    }),
    laneColor: () => "#00f",
    agentLogo: () => "",
    selectNode: () => {},
    loadChatHistory: () => {},
    jumpChatToNow: () => {},
  });
  feature.bind();

  const rowTap = () => firstListener(mapwrap, "click")({
    target: fakeTarget({ nid: "a", skey: "a#0", stop }, "[data-nid]"),
  });

  rowTap();
  assert.match(maptoolbar.innerHTML, /data-jump="a"/, "row tap opens the bar");
  assert.match(mapwrap.innerHTML, /current/, "row tap marks the row current");

  firstListener(maptoolbar, "click")({
    target: fakeTarget({ jump: "a" }, "[data-jump]"),
  });
  assert.equal(maptoolbar.innerHTML, "", "the command dismisses the bar");
  assert.equal(maptoolbar.classList.contains("on"), false, "and drops .on so it cannot catch a scroll");
  assert.match(mapwrap.innerHTML, /current/, "but the selection is untouched");

  rowTap();
  assert.match(maptoolbar.innerHTML, /data-jump="a"/,
    "tapping the still-selected row reopens the bar instead of deselecting");
  assert.match(mapwrap.innerHTML, /current/, "and the row is still selected");
});

test("action bar: every command dismisses it, not just Open chat", () => {
  /* A menu that closes for one command and not the others is worse than one
     that never closes — the user cannot form a rule. */
  const src = readFileSync(join(__dirname, "../js/map.js"), "utf8");
  const fn = src.match(/function onToolbarClick\(e\)\{([\s\S]*?)\n  \}/);
  assert.ok(fn, "onToolbarClick found");
  for (const attr of ["data-jump", "data-medit", "data-forkbookmark", "data-forkfrom", "data-mapexit"]){
    const branch = fn[1].split(`closest("[${attr}]")`)[1];
    assert.ok(branch, `${attr} branch present`);
    const upTo = branch.split("return;")[0];
    assert.match(upTo, /closeMapBar\(\)/, `${attr} closes the action bar`);
  }
});

test("dock close: an × in the chat head, no fold-to-title-strip state", () => {
  /* Acceptance verdict: "I either edit and want to see the chat or I want to
     see the metro-map to navigate" — a pop-out folded to its title bar is a
     third state that serves neither. The divider still tunes the split; the ×
     closes the pop-out outright. It sits top-right in the chat head, where
     the bookmarks chevron used to be before P5a hid it there. */
  assert.match(indexHtmlSrc, /<button[^>]*id="dockclose"[^>]*>/, "#dockclose button present");
  const toprow = indexHtmlSrc.match(/<div class="toprow">([\s\S]*?)<\/div>/);
  assert.ok(toprow, "chat head toprow found");
  assert.match(toprow[1], /id="dockclose"/, "#dockclose lives in the chat head toprow");
  const ids = [...toprow[1].matchAll(/id="([a-z]+)"/g)].map(m => m[1]);
  assert.equal(ids[ids.length - 1], "dockclose", "and is last, i.e. trailing/top-right");
  assert.match(indexHtmlSrc, /id="dockclose"[^>]*aria-label="[^"]+"/, "#dockclose is named");

  assert.match(notesCssSrc, /(?:^|\n)[^{}\n]*#dockclose\s*\{[^}]*display:\s*none/,
    "hidden globally — it only means anything under the dock");
  const mediaBody = notesDesktopMediaBody(notesCssSrc);
  assert.match(mediaBody, /body\.map-full\.map-dock\s+[^{}\n]*#dockclose\s*\{[^}]*display:\s*flex/,
    "revealed only under body.map-full.map-dock");

  const appSrc = readFileSync(join(__dirname, "../js/app.js"), "utf8");
  assert.match(appSrc, /dockclose[\s\S]{0,200}setDock\(false\)/,
    "app.js wires #dockclose to leave the dock");
});

/* ---------- cascade: hide-global rules must actually win ---------- */

/* The link order in index.html — the tiebreaker when specificity ties. */
const CSS_LINK_ORDER = ["tokens", "base", "layout", "cards", "map", "chat",
  "sheets", "notes", "accessibility"];

/** [ids, classes+attrs+pseudo-classes, elements] for the selectors we compare. */
function cssSpecificity(sel){
  const s = String(sel).trim();
  const ids = (s.match(/#[\w-]+/g) || []).length;
  const cls = (s.match(/\.[\w-]+|\[[^\]]*\]|:(?!:)[\w-]+/g) || []).length;
  const els = (s.match(/(^|[\s>+~])[a-zA-Z][\w-]*/g) || []).length;
  return [ids, cls, els];
}

/** Does rule a beat rule b? Specificity first, then stylesheet order. */
function cascadeWins(a, b){
  const sa = cssSpecificity(a.sel), sb = cssSpecificity(b.sel);
  for (let i = 0; i < 3; i++) if (sa[i] !== sb[i]) return sa[i] > sb[i];
  return CSS_LINK_ORDER.indexOf(a.file) >= CSS_LINK_ORDER.indexOf(b.file);
}

/** The selector of the first rule in src that gives #id the given display. */
function displayRuleFor(src, id, value){
  const re = new RegExp(`(?:^|\\n|\\{)\\s*([^{}\\n]*#${id}(?:[^{}\\n]*)?)\\{[^}]*display:\\s*${value}`);
  const m = src.match(re);
  return m ? m[1].trim() : null;
}

test("cascadeWins ranks specificity first, stylesheet order only on a tie", () => {
  /* Specificity has to be decided before order, or a more specific rule in an
     earlier sheet reads as the loser — which is precisely the fight the dock ×
     lost. */
  assert.equal(cascadeWins({ sel: "#chathead #dockclose", file: "chat" },
                           { sel: "#chathead .headtoggle", file: "notes" }), true,
    "the more specific rule wins from an earlier sheet");
  assert.equal(cascadeWins({ sel: "#dockclose", file: "notes" },
                           { sel: "#chathead .headtoggle", file: "chat" }), false,
    "and loses from a later one");
  assert.equal(cascadeWins({ sel: "#a", file: "notes" }, { sel: "#b", file: "chat" }), true,
    "equal specificity: the later sheet wins");
  assert.equal(cascadeWins({ sel: "#a", file: "chat" }, { sel: "#b", file: "notes" }), false);
});

test("cssSpecificity counts ids, classes and elements", () => {
  assert.deepEqual(cssSpecificity("#dockclose"), [1, 0, 0]);
  assert.deepEqual(cssSpecificity("#chathead .headtoggle"), [1, 1, 0]);
  assert.deepEqual(cssSpecificity("#chathead #dockclose"), [2, 0, 0]);
  assert.deepEqual(cssSpecificity("body.map-full.map-dock #dockclose"), [1, 2, 1]);
});

test("the chat-head toggles that must hide actually outrank #chathead .headtoggle", () => {
  /* #chathead .headtoggle sets display:flex and carries an id *and* a class.
     A bare #dockclose { display: none } loses that fight, so the wall map's
     close × sat in the ordinary chat head next to the bookmarks chevron —
     a control for a pop-out that is not on screen. Matching the rule's text
     (as the test above does) cannot see this; only the cascade can. */
  const chatCss = readFileSync(join(__dirname, "../css/chat.css"), "utf8");
  const shown = { sel: (chatCss.match(/(?:^|\n)([^{}\n]*\.headtoggle)\s*\{[^}]*display:\s*flex/) || [])[1], file: "chat" };
  assert.ok(shown.sel, "the .headtoggle display:flex rule must still exist");

  const hide = { sel: displayRuleFor(notesCssSrc, "dockclose", "none"), file: "notes" };
  assert.ok(hide.sel, "#dockclose must still be hidden globally");
  assert.ok(cascadeWins(hide, shown),
    `"${hide.sel}" must beat "${shown.sel}" or the × is always visible`);

  const media = notesDesktopMediaBody(notesCssSrc);
  const reveal = { sel: displayRuleFor(media, "dockclose", "flex"), file: "notes" };
  assert.ok(reveal.sel, "the dock must still reveal it");
  assert.ok(cascadeWins(reveal, hide),
    `"${reveal.sel}" must beat its own hide "${hide.sel}"`);

  /* Same fight, same head: the bookmarks chevron under the full-screen map. */
  const bmHide = { sel: displayRuleFor(media, "bookmarksbtn", "none"), file: "notes" };
  assert.ok(bmHide.sel, "the full-screen map must still hide the bookmarks chevron");
  assert.ok(cascadeWins(bmHide, shown),
    `"${bmHide.sel}" must beat "${shown.sel}"`);
});

test("peek is gone: no third dock state in the exports, the CSS, or the ladder", () => {
  /* P3 built a peek strip; acceptance rejected it. Removing it must be total —
     a leftover storage key would restore a class no CSS answers, and a
     leftover ladder rung would make Escape take two presses to close. */
  assert.equal(mapExports.MAP_DOCK_PEEK_KEY, undefined, "peek storage key removed");
  assert.equal(mapExports.DOCK_PEEK_SNAP, undefined, "peek hysteresis constant removed");
  assert.doesNotMatch(notesCssSrc, /dock-peek/, "no .dock-peek rules survive");
  const src = readFileSync(join(__dirname, "../js/map.js"), "utf8");
  assert.doesNotMatch(src, /dockPeek|setDockPeek|onChatPeekClick/, "no peek state in map.js");

  assert.equal(mapExports.escapeDockStep({ mapFull: true, mapDock: true }), "undock",
    "Escape from the dock closes the pop-out in one press");
  assert.equal(mapExports.escapeDockStep({ mapFull: true, mapDock: false }), "exit-full");
  assert.equal(mapExports.escapeDockStep({ mapFull: false, mapDock: false }), "none");
  assert.equal(mapExports.escapeDockStep(), "none", "tolerates a missing argument");
});

/* ---------- wall repaint budget: shape rebuilds, state patches ----------
 * The wall was re-innerHTML'd on nearly every 2s poll because its signature
 * folded in two fields that move on the poll cadence: mechanical live
 * (active<->quiet flips whenever a pane stops animating) and ctx_pct (an
 * integer gauge that creeps). With ~170 stations the odds that *some* node
 * moved are ~1 per tick, so one agent's 1px dot rebuilt the whole wall and
 * discarded every painted layer — the "grey areas while scrolling".
 * The rule these tests pin: rebuild when the wall's *shape* changes; repaint
 * a station's *state* in place. */

function volNode(over){
  return {
    id: "a", title: "T", description: "d", agent: "x", model: "m", effort: "",
    lane_id: "L", parent: "", ended_at: "", live: "quiet", attention: "",
    created_at: "2026-01-01T00:00:00Z", ctx_pct: 10, stops: [], ...over,
  };
}
const volSigOpts = { focusLane: null, mapTab: "all", mapSel: "", mapSelKey: "", fareOn: false };
const volSig = n => wallMapSignature(stopsOf(n).sort(newestFirst), ["L"], volSigOpts);

test("liveShape keeps exited/unavailable but collapses the active<->quiet flicker", () => {
  // exited and unavailable swap a filled dot for a hollow one and put .dead on
  // the row — a shape change, worth a rebuild. active vs quiet is one circle
  // radius and one caption word, so both collapse to the same bucket.
  assert.equal(mapExports.liveShape("exited"), "exited");
  assert.equal(mapExports.liveShape("unavailable"), "unavailable");
  assert.equal(mapExports.liveShape("active"), mapExports.liveShape("quiet"));
  assert.notEqual(mapExports.liveShape("active"), "exited");
  assert.equal(mapExports.liveShape(""), mapExports.liveShape("quiet"),
    "an absent liveness is not its own shape");
});

test("wall signature ignores the active<->quiet flip", () => {
  assert.equal(volSig(volNode({ live: "active" })), volSig(volNode({ live: "quiet" })),
    "a turn starting or ending must not rebuild the wall");
  assert.equal(mapRenderDecision(volSig(volNode({ live: "active" })),
                                 volSig(volNode({ live: "quiet" }))), "skip");
});

test("wall signature ignores ctx_pct drift but not its first appearance", () => {
  assert.equal(volSig(volNode({ ctx_pct: 10 })), volSig(volNode({ ctx_pct: 74 })),
    "the gauge creeping must not rebuild the wall");
  assert.equal(volSig(volNode({ ctx_pct: 79 })), volSig(volNode({ ctx_pct: 81 })),
    "even the 80% colour flip is an attribute write, not a rebuild");
  // null -> number has to create SVG elements, so it is a shape change.
  assert.notEqual(volSig(volNode({ ctx_pct: null })), volSig(volNode({ ctx_pct: 0 })),
    "a ring appearing from nothing cannot be patched into existence");
});

test("wall signature still rebuilds for the liveness changes that change shape", () => {
  const running = volSig(volNode({ live: "active" }));
  assert.notEqual(running, volSig(volNode({ live: "exited" })), "hollow dot + .dead row");
  assert.notEqual(running, volSig(volNode({ live: "unavailable" })), "hollow dot");
  assert.notEqual(running, volSig(volNode({ attention: "approval" })),
    "attention adds the glow and a rail tick");
});

test("wallVolatile keys head stops only, and volatileDiff returns just the movers", () => {
  const a = volNode({ id: "a", live: "quiet", ctx_pct: 10 });
  const b = volNode({ id: "b", live: "quiet", ctx_pct: 20 });
  const rows = [...stopsOf(a), ...stopsOf(b)];
  const prev = mapExports.wallVolatile(rows);
  assert.deepEqual(Object.keys(prev).sort(), [stopKey(stopsOf(a)[0]), stopKey(stopsOf(b)[0])].sort());
  // Earlier stops render "· earlier stop" — no status word, no live dot.
  const withStops = volNode({ id: "c", stops: [{ time: "2026-01-02T00:00:00Z" }] });
  const stopRows = stopsOf(withStops);
  assert.ok(stopRows.length > 1, "fixture must produce a non-head stop");
  assert.equal(Object.keys(mapExports.wallVolatile(stopRows)).length, 1,
    "only head stops carry volatile state");

  const moved = [...stopsOf(volNode({ id: "a", live: "active", ctx_pct: 10 })), ...stopsOf(b)];
  const next = mapExports.wallVolatile(moved);
  assert.deepEqual(mapExports.volatileDiff(prev, next), [stopKey(stopsOf(a)[0])],
    "one node moving must not repaint its 169 neighbours");
  assert.deepEqual(mapExports.volatileDiff(next, next), [], "a still fleet patches nothing");
  assert.deepEqual(mapExports.volatileDiff(null, next).sort(), Object.keys(next).sort(),
    "no previous frame means paint everything");
});

test("patchStationVolatile writes attributes and text, never innerHTML", () => {
  const writes = [];
  const el = (tag, attrs = {}) => ({
    tag, attrs, _text: "",
    get textContent(){ return this._text; },
    set textContent(v){ this._text = v; writes.push([tag, "text", v]); },
    set innerHTML(v){ throw new Error("patch must not re-parse markup"); },
    getAttribute(k){ return this.attrs[k]; },
    setAttribute(k, v){ this.attrs[k] = String(v); writes.push([tag, k, String(v)]); },
  });
  const cap = el("st");
  const row = el("row");
  row.querySelector = stubQuerySelector(s => (s === ".cap .st" ? cap : undefined));
  const dot = el("dot", { r: "5.5" });
  const arc = el("arc", { "stroke-dasharray": "0 1", stroke: "C(L)", "data-col": "C(L)" });
  const root = {
    querySelector: stubQuerySelector(s => {
      if (s.includes(".strow")) return row;
      if (s.includes("data-dot")) return dot;
      if (s.includes("data-ctx")) return arc;
      return undefined;
    }),
  };

  mapExports.patchStationVolatile(root, "k", volNode({ live: "active", ctx_pct: 90 }));
  assert.equal(cap.textContent, "Running", "the caption word is the live signal on the wall");
  assert.equal(dot.attrs.r, "6.5", "active dot is the fatter one");
  assert.equal(arc.attrs.stroke, "var(--attn)", "the gauge turns amber past 80%");
  assert.notEqual(arc.attrs["stroke-dasharray"], "0 1", "the arc must actually move");

  mapExports.patchStationVolatile(root, "k", volNode({ live: "quiet", ctx_pct: 30 }));
  assert.equal(cap.textContent, "Quiet");
  assert.equal(dot.attrs.r, "5.5");
  assert.equal(arc.attrs.stroke, "C(L)", "below 80% it goes back to the lane colour it was drawn in");

  // A station whose hooks are absent (an exited node has no patchable dot)
  // must be a silent no-op, not a throw that kills the whole poll tick.
  assert.doesNotThrow(() => mapExports.patchStationVolatile({ querySelector: stubQuerySelector() }, "k", volNode({})));
  assert.doesNotThrow(() => mapExports.patchStationVolatile(null, "k", volNode({})));
});

test("the wall markup carries the hooks the patch needs", () => {
  const n = volNode({});
  const html = stationRowHTML(n, { color: () => "C", name: () => "N" },
    { stop: stopsOf(n)[0] });
  assert.match(html, /<span class="st st-quiet">Quiet<\/span>/,
    "the status word needs its own element or the patch has to rewrite the caption");
  // The live dot and the gauge arc need addressable hooks in the SVG, and the
  // arc must be emitted even at 0% so the patch never has to create a node.
  assert.match(mapSrc, /data-dot="/);
  assert.match(mapSrc, /data-ctx="/);
  assert.match(mapSrc, /data-col="/);
  assert.doesNotMatch(mapSrc, /if \(frac > 0\)/,
    "an arc that only exists above 0% cannot be patched from 0% upwards");
});

test("the wall's skip path patches volatile state instead of returning blind", () => {
  // Dropping live/ctx_pct from the signature without a repaint would leave the
  // wall lying about which agents are running.
  const skip = mapSrc.slice(mapSrc.indexOf("const sig = wallMapSignature("));
  const decision = skip.slice(0, skip.indexOf("\n", skip.indexOf('=== "skip"')) + 200);
  assert.match(decision, /=== "skip"\)\s*return patchWallVolatile\(/,
    "the skip branch must repaint the movers before it returns");
  assert.match(skip, /wallVol(atile)? *= *wallVolatile\(/,
    "a rebuild must record the frame the next patch diffs against");
});

/* ---------- poll-path cost: reconcile without paying for it ----------
 * saveMapFold reconciles fold state (new journeys default folded, deleted
 * lanes are pruned) and must keep running before the signature — the
 * signature reads mapFold. What must not run every tick is its *cost*: a
 * second full lane-model build over the whole fleet, and two synchronous
 * localStorage writes of byte-identical values. */

test("an unchanged fold set costs no storage write and no second lane model", () => {
  // Seeded with no known lanes, so "L" can only appear if the reconciliation
  // actually ran — seeding it here would make the last assertion vacuous.
  const storage = memoryStorage({
    [MAP_FOLD_KEY]: JSON.stringify([]),
    [MAP_FOLD_KNOWN_KEY]: JSON.stringify([]),
  });
  let writes = 0;
  const rawSet = storage.setItem.bind(storage);
  storage.setItem = (k, v) => { writes++; rawSet(k, v); };
  let models = 0;
  const feature = createMapFeature({
    roots: { mapwrap: fakeEl("mapwrap"), maptoolbar: fakeEl("tb"), lanechips: fakeEl("chips") },
    document: stubDocument({ classList: { contains: () => false, toggle(){}, add(){} } }),
    storage,
    isDesktop: () => false,
    mapOpen: () => true,
    level: () => 3,
    nodes: () => [{
      id: "a", title: "A", description: "", agent: "claude", model: "m", effort: "",
      lane_id: "L", parent: "", ended_at: "", live: "quiet", attention: "",
      created_at: "2026-01-01T00:00:00Z", stops: [],
    }],
    groups: () => [],
    laneFilter: () => "",
    uiLoaded: () => true,
    laneModel: () => {
      models++;
      return { lanes: [{ id: "L", name: "Lane" }], color: () => "#00f", name: () => "Lane", byId: {} };
    },
    agentLogo: () => "",
  });

  feature.render();
  assert.equal(models, 1, "one lane model per render — renderMap already built one");
  const afterFirst = writes;
  feature.render();
  feature.render();
  assert.equal(writes, afterFirst,
    "an unchanged fold set must not write localStorage on every poll");
  assert.equal(models, 3, "still exactly one lane model per render");

  // The reconciliation itself must still happen — a lane the user has never
  // seen defaults to folded, and that has to land before the signature reads it.
  assert.deepEqual(JSON.parse(storage.getItem(MAP_FOLD_KNOWN_KEY)), ["L"],
    "an unseen journey must be recorded, or it would unfold itself every session");
  assert.ok(afterFirst > 0, "the first render still has to persist that reconciliation");
});

test("saveMapFold reuses the caller's lane model on the render path", () => {
  const body = mapSrc.slice(mapSrc.indexOf("function saveMapFold("));
  assert.match(body.slice(0, body.indexOf("\n  }")), /saveMapFold\(model\)|\bmodel \|\| lm\(\)/,
    "the render path already holds a lane model; building a second is O(fleet) per tick");
  const render = mapSrc.slice(mapSrc.indexOf("let blocks = buildStackBlocks("));
  assert.doesNotMatch(render.slice(0, render.indexOf("const sig = stackMapSignature")), /saveMapFold\(\)/,
    "the per-tick calls must pass the model they already have");
});

test("the lane attention dot pulses without repainting its halo", () => {
  /* .lattn is its own element, so its opacity/transform animation already
     scales whatever shadow it carries — the halo can be static. */
  const dot = mapCssSrc.match(/\.lhead\s+\.lattn\s*\{([^}]+)\}/);
  assert.ok(dot, ".lhead .lattn must still carry the pulse");
  assert.match(dot[1], /box-shadow:/, "the halo lives on the element");
  assert.doesNotMatch(dot[1], /will-change:[^;]*box-shadow/,
    "will-change cannot composite a box-shadow — the hint only costs memory");
  const kf = mapCssSrc.match(/@keyframes\s+mapAttentionDot\s*\{([\s\S]*?)\n\}/);
  assert.ok(kf, "@keyframes mapAttentionDot must still exist");
  assert.doesNotMatch(kf[1], /box-shadow/, "the pulse must be opacity/transform only");
  assert.match(kf[1], /opacity/, "and it must still pulse");
});

test("reduced motion loses the halo with the pulse", () => {
  /* The halo used to live only at the pulse's peak, so stopping the animation
     removed it. Now that it is static, the reduced-motion rule that stops
     .lattn would otherwise freeze it at full glow — a brighter dot than these
     users ever saw. */
  const chatCss = readFileSync(join(__dirname, "../css/chat.css"), "utf8");
  const rm = chatCss.match(/@media\s*\(prefers-reduced-motion:\s*reduce\)\s*\{([\s\S]*?)\n\}/g) || [];
  const stops = rm.filter(b => /\.lhead\s+\.lattn/.test(b));
  assert.ok(stops.length, "the reduced-motion block must still stop .lattn");
  assert.ok(stops.some(b => /box-shadow:\s*none/.test(b)),
    "stopping the pulse must drop the static halo too");
});

test("the wall's title clamp is a class the builder sets, not a :has() scan", () => {
  /* :has() is matched per .strow, and the wall is ~170 of them; WebKit's
     invalidation for it is coarse, so the per-tick caption patch inside a row
     can drag the whole selector back through style recalc. The builder already
     knows whether the row has a description. */
  assert.doesNotMatch(mapCssSrc, /:has\(/,
    "map.css must not need a :has() scan on the wall's hottest selector");
  assert.match(mapCssSrc, /\.strow\s+\.lbl\.hasdesc\s+\.t\s*\{[^}]*-webkit-line-clamp:\s*1/,
    "the one-line clamp is keyed off the class instead");

  const n = { id: "n1", title: "Alpha", agent: "claude", created_at: "2026-01-01T00:00:00Z", live: "quiet" };
  const lm = { color: () => "#000", name: () => "L" };
  const withDesc = mapExports.stationRowHTML({ ...n, description: "why" }, lm);
  const without = mapExports.stationRowHTML(n, lm);
  assert.match(withDesc, /class="lbl hasdesc"/, "a described head row is marked");
  assert.match(withDesc, /<div class="desc">/, "and still renders the description");
  assert.doesNotMatch(without, /hasdesc/, "an undescribed row is not");

  /* Earlier stops share the row structure, so they need the same mark. */
  const stopNode = { ...n, description: "why", stops: [] };
  const stop = { time: "2026-01-01T00:00:00Z", head: false, i: 0, n: stopNode };
  const stopRow = mapExports.stationRowHTML(stopNode, lm, { stop });
  assert.match(stopRow, /stoprow/, "the earlier-stop branch was exercised");
  assert.match(stopRow, /class="lbl hasdesc"/, "an earlier stop with a description is marked too");
});

test("the dock's content column is one column — the approval row included", () => {
  /* The dock hides #cards, so #msgs and #promptbar are given back the width
     they have beside the Activities pane. #keyrow and #convtools are in the
     same column and were left out: an approval — the one moment the chat is
     asking you for something — arrived flush against the window edge while
     the transcript above it sat inset by half a pane. Chrome that spans (the
     gauge, the head's hairline, the composer's top border) is deliberate and
     stays out of the rule. */
  const mediaBody = notesDesktopMediaBody(notesCssSrc);
  assert.ok(mediaBody, "@media (min-width: 900px) block present in notes.css");
  const body = mediaBody.replace(/\/\*[\s\S]*?\*\//g, "");

  const rules = [...body.matchAll(/([^{}]+)\{([^{}]*)\}/g)]
    .filter(m => /padding-inline:\s*calc\(\s*var\(--pane\)\s*\/\s*2\s*\)/.test(m[2]));
  assert.equal(rules.length, 1, "one rule sets the dock column's inset");

  const selectors = rules[0][1].split(",").map(s => s.trim()).filter(Boolean);
  for (const sel of selectors){
    assert.match(sel, /^body\.map-full\.map-dock\s+#/,
      `the inset must stay scoped to the dock (${sel})`);
  }
  const ids = selectors.map(s => s.slice(s.lastIndexOf("#")));
  for (const id of ["#msgs", "#keyrow", "#convtools", "#promptbar"]){
    assert.ok(ids.includes(id), `${id} is content and must share the column`);
  }
  for (const id of ["#gauge", "#chathead"]){
    assert.ok(!ids.includes(id), `${id} is chrome and must still span`);
  }
});

/* ---------- P6: ring is the control; pill replaces the rail ---------- */

/** Pull the hit-circle tag out of attentionStationSVG (or wall/stack markup). */
function attnHitCircle(svg){
  const m = String(svg || "").match(/<circle\b[^>]*class="[^"]*attnstation-hit[^"]*"[^>]*\/?>|<circle\b[^>]*class='[^']*attnstation-hit[^']*'[^>]*\/?>/);
  if (m) return m[0];
  // Attribute order may put class after other attrs.
  const all = [...String(svg || "").matchAll(/<circle\b[^>]*>/g)].map(x => x[0]);
  return all.find(t => /attnstation-hit/.test(t)) || "";
}

function circleAttr(tag, name){
  const m = tag.match(new RegExp(`\\b${name}="([^"]*)"`));
  return m ? m[1] : null;
}

test("P6: attentionPill pure — nothing asking / all on-screen → show false", () => {
  const fn = mapExports.attentionPill;
  assert.equal(typeof fn, "function", "attentionPill is exported");

  const empty = fn({ ticks: [], scrollTop: 0, viewportH: 200, contentH: 800 });
  assert.equal(empty.show, false);
  assert.equal(empty.count, 0);

  const none = fn({ ticks: null, scrollTop: 0, viewportH: 200, contentH: 800 });
  assert.equal(none.show, false);
  assert.equal(none.count, 0);

  // One tick at the vertical centre of the viewport — on-screen.
  // y = frac * contentH = 0.25 * 800 = 200; viewport [0, 400] contains 200.
  const onScreen = fn({
    ticks: [{ nodeId: "a", stopKey: "a#0", frac: 0.25, kind: "waiting" }],
    scrollTop: 0, viewportH: 400, contentH: 800,
  });
  assert.equal(onScreen.show, false, "on-screen tick does not raise the pill");
  assert.equal(onScreen.count, 0);
});

test("P6: attentionPill pure — one below, one above, both (nearest below wins)", () => {
  const fn = mapExports.attentionPill;
  // contentH=1000, viewport [200, 500] (scrollTop=200, vh=300).
  // below: frac 0.8 → y=800 (>500); above: frac 0.05 → y=50 (<200).
  const below = fn({
    ticks: [{ nodeId: "b", stopKey: "b#0", frac: 0.8, kind: "waiting" }],
    scrollTop: 200, viewportH: 300, contentH: 1000,
  });
  assert.equal(below.show, true);
  assert.equal(below.count, 1);
  assert.equal(below.dir, "down");
  assert.equal(below.nodeId, "b");
  assert.equal(below.stopKey, "b#0");
  assert.match(below.label, /1 waiting/);

  const above = fn({
    ticks: [{ nodeId: "a", stopKey: "a#0", frac: 0.05, kind: "waiting" }],
    scrollTop: 200, viewportH: 300, contentH: 1000,
  });
  assert.equal(above.show, true);
  assert.equal(above.count, 1);
  assert.equal(above.dir, "up");
  assert.equal(above.nodeId, "a");
  assert.match(above.label, /1 waiting/);

  // Both directions: prefer nearest below even if an above tick is closer.
  // above at y=180 (20px above top), below at y=700 (200px below bottom).
  const both = fn({
    ticks: [
      { nodeId: "up", stopKey: "up#0", frac: 0.18, kind: "waiting" },
      { nodeId: "dn", stopKey: "dn#0", frac: 0.70, kind: "waiting" },
    ],
    scrollTop: 200, viewportH: 300, contentH: 1000,
  });
  assert.equal(both.show, true);
  assert.equal(both.count, 2);
  assert.equal(both.dir, "down", "prefer below when both directions have off-screen ticks");
  assert.equal(both.nodeId, "dn");
  assert.match(both.label, /2 waiting/);
});

test("P6: attentionPill pure — finished-only → ready; mixed → waiting; arrows", () => {
  const fn = mapExports.attentionPill;
  // Two finished ticks below the fold.
  const ready = fn({
    ticks: [
      { nodeId: "r1", stopKey: "r1#0", frac: 0.7, kind: "ready" },
      { nodeId: "r2", stopKey: "r2#0", frac: 0.9, kind: "ready" },
    ],
    scrollTop: 0, viewportH: 200, contentH: 1000,
  });
  assert.equal(ready.show, true);
  assert.equal(ready.count, 2);
  assert.equal(ready.dir, "down");
  assert.equal(ready.kind, "ready");
  assert.match(ready.label, /2 ready/);
  assert.doesNotMatch(ready.label, /waiting/);

  /* Mixed hard + finished off-screen. Maintainer decision 2026-08-10: the pill
     counts where the user's attention is needed to let the agent proceed — so
     the count is the *waiting* ticks alone, never the total. "2 waiting" with
     one waiting station would claim two agents are blocked on you, which is
     exactly the meaning P5 reserved for --attn. The finished station is not
     announced while a waiting one is off-screen; it gets the pill to itself
     once the waiting ones are on-screen or answered. */
  const mixed = fn({
    ticks: [
      { nodeId: "w", stopKey: "w#0", frac: 0.8, kind: "waiting" },
      { nodeId: "r", stopKey: "r#0", frac: 0.9, kind: "ready" },
    ],
    scrollTop: 0, viewportH: 200, contentH: 1000,
  });
  assert.equal(mixed.count, 1, "counts the waiting stations, not every off-screen tick");
  assert.equal(mixed.kind, "waiting");
  assert.match(mixed.label, /1 waiting/);
  assert.doesNotMatch(mixed.label, /ready/);

  /* The label names waiting, so the target must BE a waiting station: a pill
     that says "1 waiting ↓" and scrolls to a finished one is a lie about the
     destination. Nearest *among the named kind* — here the far waiting tick
     wins over the nearer ready one. */
  const nearer = fn({
    ticks: [
      { nodeId: "far", stopKey: "far#0", frac: 0.95, kind: "waiting" },
      { nodeId: "near", stopKey: "near#0", frac: 0.6, kind: "ready" },
    ],
    scrollTop: 0, viewportH: 200, contentH: 1000,
  });
  assert.equal(nearer.nodeId, "far", "the named kind picks the target");
  assert.equal(nearer.stopKey, "far#0");
  assert.equal(nearer.dir, "down");

  // Nearest of two below, both waiting: the smaller frac (closer to the fold).
  const twoBelow = fn({
    ticks: [
      { nodeId: "far", stopKey: "far#0", frac: 0.95, kind: "waiting" },
      { nodeId: "near", stopKey: "near#0", frac: 0.6, kind: "waiting" },
    ],
    scrollTop: 0, viewportH: 200, contentH: 1000,
  });
  assert.equal(twoBelow.nodeId, "near");
  assert.equal(twoBelow.count, 2);

  // Nearest of two above is the larger frac (closer to the viewport top).
  const nearerUp = fn({
    ticks: [
      { nodeId: "farUp", stopKey: "farUp#0", frac: 0.02, kind: "waiting" },
      { nodeId: "nearUp", stopKey: "nearUp#0", frac: 0.15, kind: "waiting" },
    ],
    scrollTop: 300, viewportH: 200, contentH: 1000,
  });
  assert.equal(nearerUp.dir, "up");
  assert.equal(nearerUp.nodeId, "nearUp");

  /* Direction follows the named kind too: a waiting station above and a
     finished one below must send the arrow up, to the one that needs you. */
  const waitingAbove = fn({
    ticks: [
      { nodeId: "w", stopKey: "w#0", frac: 0.1, kind: "waiting" },
      { nodeId: "r", stopKey: "r#0", frac: 0.9, kind: "ready" },
    ],
    scrollTop: 300, viewportH: 200, contentH: 1000,
  });
  assert.equal(waitingAbove.dir, "up");
  assert.equal(waitingAbove.nodeId, "w");
  assert.equal(waitingAbove.count, 1);
});

test("P6: railTicks excludes inspect from waiting — hardAttention only", () => {
  /* fixes-2 P1 inverted the 2026-08-10 contract: neutral inspect is diagnostic,
     not a verified wait. Rings, ticks, pills, and tab badges use hardAttention
     (approval/question/dialog), never truthy n.attention. turn_done stays ready. */
  const fn = mapExports.railTicks;
  const mk = (id, attention, done) => ({
    n: {
      id, title: id, attention, turn_done: !!done,
      created_at: "t", stops: [], live: "quiet", lane_id: "L",
    },
    i: 0, time: "t", head: true,
  });
  const rows = [
    mk("insp", "inspect"),
    mk("ask", "approval"),
    mk("done", "", true),
    mk("quiet", ""),
  ];
  const ticks = fn({ rows, rowHeight: 76, offset: 16 });
  assert.deepEqual(ticks.map(t => t.nodeId), ["ask", "done"],
    "inspect contributes zero waiting ticks; hard attention + ready remain");
  assert.deepEqual(ticks.map(t => t.kind), ["waiting", "ready"],
    "inspect is not a waiting kind");
  assert.ok(!ticks.some(t => t.nodeId === "insp"),
    "inspect station has no attention ring tick");
});

test("P6: railTicks folds finished heads and reports kind", () => {
  /* P5's rail only ticked attention heads. P6's pill counts hard attention
     AND turn_done, so railTicks must emit both with a kind field. */
  const fn = mapExports.railTicks;
  assert.equal(typeof fn, "function");
  const hard = {
    id: "h", title: "Hard", attention: "approval", turn_done: false,
    created_at: "t0", stops: [], live: "quiet", lane_id: "L",
  };
  const finished = {
    id: "f", title: "Fin", attention: "", turn_done: true,
    created_at: "t1", stops: [], live: "quiet", lane_id: "L",
  };
  const quiet = {
    id: "q", title: "Quiet", attention: "", turn_done: false,
    created_at: "t2", stops: [], live: "quiet", lane_id: "L",
  };
  const rows = [
    { n: hard, i: 0, time: "t0", head: true },
    { n: finished, i: 0, time: "t1", head: true },
    { n: quiet, i: 0, time: "t2", head: true },
  ];
  const ticks = fn({ rows, rowHeight: 76, offset: 16 });
  assert.equal(ticks.length, 2, "hard + finished heads only");
  assert.deepEqual(ticks.map(t => t.nodeId), ["h", "f"]);
  assert.equal(ticks[0].kind, "waiting");
  assert.equal(ticks[1].kind, "ready");
  assert.ok(typeof ticks[0].frac === "number");
  assert.ok(typeof ticks[0].stopKey === "string");
});

test("P6: attentionHitSVG r≥22, transparent, hit-testable; glow/ring stay decoration-only", () => {
  /* Decorative ring is r=9.5 (~19px). HIG wants ≥44pt; r=22 → 44px diameter.
     fill="none" is not hit-testable — must be transparent (or pointer-events=all).
     Hit is a separate emit (attentionHitSVG) so wall/stack can paint it AFTER
     the solid station dot — see P4 order tests. */
  const hit = attnHitCircle(attentionHitSVG(10, 20, "node-a"));
  assert.ok(hit, "hit circle with class attnstation-hit is present when nodeId is given");
  const r = Number(circleAttr(hit, "r"));
  assert.ok(r >= 22, `hit radius ${r} must be ≥ 22 (44px target)`);
  const fill = circleAttr(hit, "fill");
  assert.ok(fill === "transparent" || /pointer-events\s*=\s*["']?(all|auto)/.test(hit),
    "hit-testable: fill=transparent or pointer-events all/auto");
  assert.notEqual(fill, "none", "fill=none is not hit-testable");
  assert.match(hit, /data-jump="node-a"/, "hit carries routing data-jump");
  assert.match(hit, /pointer-events="all"/, "transparent fill needs pointer-events=all");

  // Glow/ring helper must not smuggle a hit circle (order is the caller's job).
  const deco = attentionStationSVG(10, 20, 0.5);
  assert.equal(attnHitCircle(deco), "", "attentionStationSVG is decoration-only");
  assert.equal(attentionHitSVG(1, 2, ""), "", "no hit without a node id");
  assert.equal(attentionHitSVG(1, 2, null), "", "null node id → empty");
});

test("P6: wall and stack SVG both carry the hit circle on asking/finished stations", () => {
  const ask = p4Node("ask", "2026-01-02T00:00:00Z", "Ask");
  ask.attention = "approval";
  const fin = p4Node("fin", "2026-01-01T00:00:00Z", "Fin");
  fin.turn_done = true;
  const quiet = p4Node("q", "2026-01-03T00:00:00Z", "Quiet");

  // Wall (map-full desktop).
  const wall = createWallScrollFeature([ask, fin, quiet]);
  wall.feature.render();
  const wallHits = [...wall.mapwrap.innerHTML.matchAll(/attnstation-hit/g)];
  assert.equal(wallHits.length, 2, "wall: hit on asking + finished only");
  assert.match(wall.mapwrap.innerHTML, /attnstation-hit[^>]*r="22"|r="22"[^>]*attnstation-hit/);
  assert.match(wall.mapwrap.innerHTML, /data-jump="ask"/);
  assert.match(wall.mapwrap.innerHTML, /data-jump="fin"/);
  assert.doesNotMatch(wall.mapwrap.innerHTML, /data-jump="q"/);
  wall.feature.destroy();

  // Stack (desktop column map — not full).
  const mapwrap = fakeEl("mapwrap");
  const body = {
    classList: {
      _set: new Set(),
      toggle(name, on){ if (on) this._set.add(name); else this._set.delete(name); },
      contains(name){ return this._set.has(name); },
      add(name){ this._set.add(name); },
      remove(name){ this._set.delete(name); },
    },
  };
  const byId = { ask, fin, q: quiet };
  const stack = createMapFeature({
    roots: {
      mapwrap, mapscroll: fakeEl("mapscroll"), maptoolbar: fakeEl("tb"),
      lanechips: fakeEl("chips"), maptabs: fakeEl("tabs"),
    },
    document: stubDocument(body),
    storage: memoryStorage({
      [MAP_FOLD_KNOWN_KEY]: JSON.stringify(["L"]),
      [MAP_FOLD_KEY]: JSON.stringify([]),
    }),
    isDesktop: () => true,
    mapOpen: () => true,
    level: () => 1,
    nodes: () => [ask, fin, quiet],
    groups: () => [],
    laneFilter: () => "",
    uiLoaded: () => true,
    laneModel: () => ({
      lanes: [{ id: "L", name: "Lane" }],
      color: () => "#00f", name: () => "Lane", byId,
    }),
    laneColor: () => "#00f",
    agentLogo: () => "",
  });
  stack.bind();
  stack.restoreChrome();
  stack.render();
  assert.match(mapwrap.innerHTML, /attnstation-hit/, "stack map also draws the hit circle");
  assert.match(mapwrap.innerHTML, /data-jump="ask"/);
  stack.destroy();
});

/* P4: hit circle must be LAST among co-located station paint (glow, ring, solid
   dot, ctx arc). SVG hit-tests later siblings first — a hit painted under the
   solid r=5.5/6.5 dot leaves the station centre dead. Assert on emitted markup
   order, never on a fake that declares its own class. */
function svgCircles(html){
  return [...String(html || "").matchAll(/<circle\b[^>]*\/?>/g)].map(m => m[0]);
}

/** Circles sharing the hit target's cx/cy, in document order. */
function coLocatedStationPaint(html, nodeId){
  const circles = svgCircles(html);
  const hit = circles.find(c =>
    /attnstation-hit/.test(c) && c.includes(`data-jump="${nodeId}"`));
  if (!hit) return { hit: null, peers: [], hitIndex: -1 };
  const cx = circleAttr(hit, "cx");
  const cy = circleAttr(hit, "cy");
  const peers = circles.filter(c =>
    circleAttr(c, "cx") === cx && circleAttr(c, "cy") === cy);
  return { hit, peers, hitIndex: peers.indexOf(hit) };
}

function assertHitIsTopmost(html, nodeId, label){
  const { hit, peers, hitIndex } = coLocatedStationPaint(html, nodeId);
  assert.ok(hit, `${label}: hit circle for ${nodeId} must be in the markup`);
  assert.equal(hitIndex, peers.length - 1,
    `${label}: hit for ${nodeId} must be the last co-located circle ` +
    `(paint order; centre tap hits later siblings first). peers=${peers.map(c =>
      (c.match(/class="([^"]*)"/) || c.match(/data-dot="([^"]*)"/) || ["", "?"])[1]
      || (c.includes("data-dot") ? "dot" : c.includes("data-ctx") ? "ctx" : "solid")
    ).join(",")}`);
  // Wall hooks: when present, hit must follow data-dot and data-ctx by string index.
  const skey = `${nodeId}#0`;
  const hitAt = html.indexOf(hit);
  const dotAt = html.search(new RegExp(`data-dot="${skey.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}"`));
  if (dotAt >= 0) assert.ok(dotAt < hitAt, `${label}: data-dot before hit for ${nodeId}`);
  const ctxAt = html.search(new RegExp(`data-ctx="${skey.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}"`));
  if (ctxAt >= 0) assert.ok(ctxAt < hitAt, `${label}: data-ctx before hit for ${nodeId}`);
}

function createStackMapFeature(nodes){
  const mapwrap = fakeEl("mapwrap");
  const body = {
    classList: {
      _set: new Set(),
      toggle(name, on){ if (on) this._set.add(name); else this._set.delete(name); },
      contains(name){ return this._set.has(name); },
      add(name){ this._set.add(name); },
      remove(name){ this._set.delete(name); },
    },
  };
  const byId = {};
  for (const n of nodes) byId[n.id] = n;
  const feature = createMapFeature({
    roots: {
      mapwrap, mapscroll: fakeEl("mapscroll"), maptoolbar: fakeEl("tb"),
      lanechips: fakeEl("chips"), maptabs: fakeEl("tabs"),
    },
    document: stubDocument(body),
    storage: memoryStorage({
      [MAP_FOLD_KNOWN_KEY]: JSON.stringify(["L"]),
      [MAP_FOLD_KEY]: JSON.stringify([]),
    }),
    isDesktop: () => true,
    mapOpen: () => true,
    level: () => 1,
    nodes: () => nodes,
    groups: () => [],
    laneFilter: () => "",
    uiLoaded: () => true,
    laneModel: () => ({
      lanes: [{ id: "L", name: "Lane" }],
      color: () => "#00f", name: () => "Lane", byId,
    }),
    laneColor: () => "#00f",
    agentLogo: () => "",
  });
  feature.bind();
  feature.restoreChrome();
  return { feature, mapwrap };
}

test("P4: wall hit circle is after station dot (attention and finished)", () => {
  const ask = p4Node("ask", "2026-01-02T00:00:00Z", "Ask");
  ask.attention = "approval";
  const fin = p4Node("fin", "2026-01-01T00:00:00Z", "Fin");
  fin.turn_done = true;
  const quiet = p4Node("q", "2026-01-03T00:00:00Z", "Quiet");
  quiet.ctx_pct = 40; // would draw a ctx ring; must not get a hit circle

  const { feature, mapwrap } = createWallScrollFeature([ask, fin, quiet]);
  feature.render();
  const html = mapwrap.innerHTML;

  assertHitIsTopmost(html, "ask", "wall attention");
  assertHitIsTopmost(html, "fin", "wall finished (turn_done / --work)");

  // Negative: quiet station emits no hit (attentionStationSVG never called).
  assert.equal(coLocatedStationPaint(html, "q").hit, null,
    "quiet station must not emit .attnstation-hit");
  assert.doesNotMatch(html, /data-jump="q"/);
  // Quiet does get a ctx ring — prove the hooks path still runs without a hit.
  assert.match(html, /data-ctx="q#0"/, "quiet still draws the ctx gauge");

  feature.destroy();
});

test("P4: stack hit circle is after station dot (attention and finished)", () => {
  const ask = p4Node("ask", "2026-01-02T00:00:00Z", "Ask");
  ask.attention = "approval";
  const fin = p4Node("fin", "2026-01-01T00:00:00Z", "Fin");
  fin.turn_done = true;
  const quiet = p4Node("q", "2026-01-03T00:00:00Z", "Quiet");

  const { feature, mapwrap } = createStackMapFeature([ask, fin, quiet]);
  feature.render();
  const html = mapwrap.innerHTML;

  assertHitIsTopmost(html, "ask", "stack attention");
  assertHitIsTopmost(html, "fin", "stack finished (turn_done / --work)");
  assert.equal(coLocatedStationPaint(html, "q").hit, null,
    "stack quiet station must not emit .attnstation-hit");
  assert.doesNotMatch(html, /data-jump="q"/);

  feature.destroy();
});

test("P6: tapping the ring reaches the same open path as toolbar [data-jump]", () => {
  /* Destination: selectNode(id,"jump") + jumpChatToNow (head) / loadChatHistory
     (earlier) + setMapDock(true) in full wall. Assert on injected calls, not
     "a function was called". */
  const head = p4Node("a", "2026-01-01T00:00:00Z", "Alpha");
  head.attention = "approval";
  const calls = [];
  const { feature, mapwrap, body, storage } = createWallScrollFeature([head], {
    selectNode: (id, why) => calls.push(["select", id, why]),
    loadChatHistory: (id, at) => calls.push(["history", id, at]),
    jumpChatToNow: () => calls.push(["now"]),
  });
  feature.render();
  assert.match(mapwrap.innerHTML, /attnstation-hit/, "ring hit is in the wall SVG");

  const ringTarget = {
    dataset: { jump: "a" },
    classList: { contains: n => n === "attnstation-hit" },
  };
  ringTarget.closest = stubClosest({
    ".attnstation-hit": ringTarget,
    "[data-jump]": ringTarget,
    "circle.attnstation-hit": ringTarget,
    ".attnstation-hit[data-jump]": ringTarget,
  });
  firstListener(mapwrap, "click")({ target: ringTarget });
  assert.deepEqual(calls, [
    ["select", "a", "jump"],
    ["now"],
  ], "head ring: selectNode(id, jump) then jumpChatToNow");
  assert.equal(body.classList.contains("map-dock"), true,
    "full wall ring opens docked chat (setMapDock)");
  assert.equal(storage.getItem(mapExports.MAP_DOCK_KEY || "scimux-mapdock"), "1");
  assert.equal(feature.isFull(), true, "mapFull stays true when docking from the ring");
  feature.destroy();
});

test("P6: ring tap wins over the generic [data-nid] row branch", () => {
  /* onMapWrapClick must handle the hit circle before [data-nid], so a tap on
     the ring opens chat rather than only selecting the row. */
  const head = p4Node("a", "2026-01-01T00:00:00Z", "Alpha");
  head.attention = "question";
  const calls = [];
  const { feature, mapwrap, maptoolbar } = createWallScrollFeature([head], {
    selectNode: (id, why) => calls.push(["select", id, why]),
    jumpChatToNow: () => calls.push(["now"]),
  });
  feature.render();

  // Target that would match BOTH .attnstation-hit and (if ordered wrong) a
  // parent [data-nid] — the hit branch must fire the open path.
  const bothTarget = {
    dataset: { jump: "a", nid: "a", skey: "a#0", stop: "" },
  };
  bothTarget.closest = stubClosest({
    ".attnstation-hit": bothTarget,
    "[data-jump]": bothTarget,
    "[data-nid]": bothTarget,
  });
  firstListener(mapwrap, "click")({ target: bothTarget });
  assert.ok(calls.some(c => c[0] === "select" && c[2] === "jump"),
    "ring path uses selectNode(..., 'jump'), not a bare row select");
  assert.ok(calls.some(c => c[0] === "now"), "and jumps chat to now");
  // Toolbar "Open chat" would also dock; ring must not stop at mere selection.
  void maptoolbar;
  feature.destroy();
});

test("P6: CSS #mappill hide-global / reveal-scoped; min-height ≥44; safe-area", () => {
  /* §0: assert on extracted declarations — not class names in an HTML string. */
  const base = mapCssSrc.match(/(?:^|\n)\s*#mappill\s*\{([^}]+)\}/);
  assert.ok(base, "#mappill base rule exists in map.css");
  assert.match(base[1], /display:\s*none/,
    "base #mappill is display:none (not focusable off-screen)");
  const mh = base[1].match(/min-height:\s*([0-9.]+)px/);
  assert.ok(mh, "min-height declared in px");
  assert.ok(Number(mh[1]) >= 44, `min-height ${mh[1]}px must be ≥ 44 (HIG)`);
  assert.match(base[1], /safe-area-inset-bottom/,
    "bottom offset uses env(safe-area-inset-bottom)");

  assert.match(mapCssSrc, /body\.map-full\s+#mappill\.on\s*\{([^}]+)\}/,
    "body.map-full #mappill.on reveal rule present");
  const reveal = mapCssSrc.match(/body\.map-full\s+#mappill\.on\s*\{([^}]+)\}/);
  assert.ok(reveal);
  assert.match(reveal[1], /display:\s*(block|flex)/,
    "reveal shows the pill under map-full + .on");
});

test("P6: #mappill is outside #mapwrap; sibling of #maptoolbar", () => {
  const html = readFileSync(join(__dirname, "../index.html"), "utf8");
  assert.match(html, /id="mappill"/, "#mappill present in index.html");
  assert.match(html, /id="maptoolbar"/);
  assert.match(html, /id="mapwrap"/);
  assert.doesNotMatch(html,
    /id="mapwrap"[^>]*>[^<]*<[^>]*id="mappill"/,
    "#mappill must not be a child of #mapwrap (poll-safe singleton)");
  // Sibling after mapscroll, near the toolbar — never nested in the polled region.
  assert.match(html,
    /id="mapscroll"><div id="mapwrap"><\/div><\/div>[\s\S]*id="mappill"/,
    "#mappill is a sibling after #mapscroll, not inside it");
  // Prefer a real button so it is a control, not a bare labelled div.
  assert.match(html, /<button[^>]*id="mappill"/, "#mappill is a <button>");
});

test("P6: standing guard — no maprail survives in production web sources", () => {
  /* Like arc 1's .dock-peek test: the rail is gone, not renamed in place. */
  const html = readFileSync(join(__dirname, "../index.html"), "utf8");
  const appSrc = readFileSync(join(__dirname, "../js/app.js"), "utf8");
  assert.doesNotMatch(html, /maprail/, "no maprail in index.html");
  assert.doesNotMatch(mapCssSrc, /maprail/, "no maprail in map.css");
  assert.doesNotMatch(mapSrc, /maprail/, "no maprail in map.js");
  assert.doesNotMatch(appSrc, /maprail/, "no maprail in app.js");

  // Broader: every production web file under web/js and web/css.
  for (const dir of ["js", "css"]) {
    const root = join(__dirname, "..", dir);
    for (const name of readdirSync(root)) {
      if (!/\.(js|css)$/.test(name)) continue;
      const src = readFileSync(join(root, name), "utf8");
      assert.doesNotMatch(src, /maprail/, `no maprail in web/${dir}/${name}`);
    }
  }
});

/* ---------- P7: thicker selection frame; tab attention counts ---------- */

test("P7: .strow.current selection frame is 2px at ~55% (rule body)", () => {
  /* Assert on the extracted rule body — width and mix percentage — not on a
     class name in an HTML string. Inset shadow: no layout shift. */
  const rule = mapCssSrc.match(/\.strow\.current\s*\{([^}]+)\}/);
  assert.ok(rule, ".strow.current rule must exist in map.css");
  const body = rule[1];
  assert.match(body, /box-shadow\s*:\s*inset\s+0\s+0\s+0\s+2px/,
    "selection frame is 2px inset (was 1px)");
  assert.match(body, /color-mix\(\s*in\s+srgb\s*,\s*var\(--work\)\s+55%\s*,\s*transparent\s*\)/,
    "selection frame is ~55% work mix (was 35%)");
  assert.doesNotMatch(body, /inset\s+0\s+0\s+0\s+1px/,
    "1px frame is retired");
});

test("P7: mapTabAttentionCounts mirrors the pill — one number, kind names it", () => {
  /* Reuses P6's kind split (railTicks / attentionPill): hardAttention is
     waiting; inspect contributes zero; turn_done without hard attention is
     ready. One badge per tab: if anything is waiting the badge counts waiting
     and wears that kind; otherwise it counts ready. Never the sum.
     Membership mirrors renderMap: lane_id && served(n).some(inGroup). */
  const fn = mapExports.mapTabAttentionCounts;
  assert.equal(typeof fn, "function", "mapTabAttentionCounts is exported");

  const nodes = [
    { id: "w", lane_id: "L1", attention: "approval", turn_done: false },
    { id: "insp", lane_id: "L1", attention: "inspect", turn_done: false },
    { id: "ready", lane_id: "L1", attention: "", turn_done: true },
    { id: "quiet", lane_id: "L1", attention: "", turn_done: false },
    { id: "b", lane_id: "L2", attention: "question", turn_done: false },
    /* multi: primary lane L3, also served on L1 via injected served() */
    { id: "multi", lane_id: "L3", attention: "approval", turn_done: false },
    { id: "nolane", lane_id: "", attention: "approval", turn_done: false },
    /* both flags set: waiting wins, exactly as railTicks resolves it */
    { id: "both", lane_id: "L4", attention: "approval", turn_done: true },
    { id: "done", lane_id: "L5", attention: "", turn_done: true },
    { id: "done2", lane_id: "L5", attention: "", turn_done: true },
  ];
  const groups = [
    { id: "g1", name: "One", lanes: ["L1"] },
    { id: "g2", name: "Two", lanes: ["L2"] },
    { id: "g3", name: "Empty", lanes: ["L99"] },
    { id: "g13", name: "One+Three", lanes: ["L1", "L3"] },
    { id: "g4", name: "Both", lanes: ["L4"] },
    { id: "g5", name: "Done", lanes: ["L5"] },
  ];
  const served = n => {
    if (n.id === "multi") return ["L3", "L1"];
    return n.lane_id ? [n.lane_id] : [];
  };
  const c = fn(nodes, groups, { served });

  // All: hard-attention stations with a lane — w, b, multi, both (inspect is
  // not waiting; ready ignored while anything waits; quiet/lane-less never).
  assert.deepEqual(c.all, { n: 4, kind: "waiting" },
    "All counts hard waiting only; inspect contributes zero");
  // g1 / L1: w + multi waiting; inspect does not count; ready not added.
  assert.deepEqual(c.g1, { n: 2, kind: "waiting" },
    "a mixed tab counts hard waiting, never inspect or the sum");
  assert.deepEqual(c.g2, { n: 1, kind: "waiting" });
  assert.deepEqual(c.g13, { n: 2, kind: "waiting" },
    "union tab: w + multi (inspect excluded; never double-counted)");
  assert.deepEqual(c.g4, { n: 1, kind: "waiting" },
    "hard attention + turn_done on one node resolves to waiting");
  // Ready-only tab: the finished state gets the same badge, a different kind.
  assert.deepEqual(c.g5, { n: 2, kind: "ready" },
    "a tab holding only finished turns badges ready");
  // Nothing at all: zero, and the kind is the quiet one.
  assert.deepEqual(c.g3, { n: 0, kind: "ready" },
    "tab whose lanes hold nothing is zero");

  // Empty inputs
  assert.deepEqual(fn([], [], {}), { all: { n: 0, kind: "ready" } });
  assert.deepEqual(fn(null, null), { all: { n: 0, kind: "ready" } });
});

test("P7: tab membership goes through groupLanes, not a private re-read", () => {
  /* renderMap filters with groupLanes(...) — `lanes ?? null`, where null means
     "no filter, show everything". A counter that reads g.lanes || [] instead
     would badge 0 on a group record with no lanes key while the map body shows
     every station under it. Same helper, same answer. */
  const fn = mapExports.mapTabAttentionCounts;
  const nodes = [
    { id: "w", lane_id: "L1", attention: "approval" },
    { id: "b", lane_id: "L2", attention: "question" },
  ];
  const served = n => (n.lane_id ? [n.lane_id] : []);

  const loose = fn(nodes, [{ id: "gx", name: "No lanes key" }], { served });
  assert.deepEqual(loose.gx, loose.all,
    "a group without a lanes key filters nothing — same count as All");

  // An explicitly empty list still means "matches nothing" in both places.
  const empty = fn(nodes, [{ id: "ge", name: "Empty", lanes: [] }], { served });
  assert.deepEqual(empty.ge, { n: 0, kind: "ready" },
    "lanes: [] matches nothing (unchanged)");
});

test("P7: mapTabsHTML carries the count badge and no animation class", () => {
  const counts = {
    all: { n: 2, kind: "waiting" },
    g1: { n: 1, kind: "waiting" },
    g2: { n: 0, kind: "waiting" },
    g3: { n: 4, kind: "ready" },
  };
  const html = mapTabsHTML("g1", [
    { id: "g1", name: "Alpha" },
    { id: "g2", name: "Beta" },
    { id: "g3", name: "Gamma" },
  ], { escape: s => s, counts });

  // All tab badge
  assert.match(html, /data-mt="all"[^>]*>All<span class="tabcount">2<\/span>/,
    "All tab shows its waiting count");
  // Selected group with count
  assert.match(html, /data-mt="g1"[^>]*>[\s\S]*?Alpha<span class="tabcount">1<\/span>/,
    "group tab shows its waiting count");
  // Ready kind carries the hue marker, mirroring #mappill.ready.
  assert.match(html, /data-mt="g3"[^>]*>[\s\S]*?Gamma<span class="tabcount ready">4<\/span>/,
    "a ready-kind badge is marked so the hue can name it");
  // Zero count: no badge (not a "0" pill)
  const beta = html.match(/data-mt="g2"[^>]*>([\s\S]*?)<\/button>/);
  assert.ok(beta, "Beta tab present");
  assert.doesNotMatch(beta[1], /tabcount/, "zero-count tab has no badge");

  // HIG: badge on the tab, pulse on the station — no motion on the tab itself.
  assert.doesNotMatch(html, /class="[^"]*(?:pulse|animat|glow|attn-ring)/i,
    "tab HTML carries no animation / pulse / glow class");
  assert.doesNotMatch(html, /tabcount[^"]*(?:pulse|anim)/i);

  // Without counts, still pure (existing order/ARIA contract intact).
  const bare = mapTabsHTML("all", [{ id: "g1", name: "Alpha" }], { escape: s => s });
  assert.doesNotMatch(bare, /tabcount/, "omitted counts → no badges");
  assert.match(bare, /data-mt="\+" class="add"/);
});

test("P7: a ready-kind tab badge wears --work, waiting keeps --attn", () => {
  /* Same two-hue contract as the ring and the pill: yellow means exactly one
     thing ("blocked on you"), finished turns get the success hue. */
  const layoutCss = readFileSync(join(__dirname, "../css/layout.css"), "utf8");
  const base = layoutCss.match(/\.tabs\s+\.tabcount\s*\{([^}]+)\}/);
  assert.ok(base, ".tabs .tabcount rule must exist");
  assert.match(base[1], /color\s*:\s*var\(--attn\)/, "waiting badge is --attn");

  const ready = layoutCss.match(/\.tabs\s+\.tabcount\.ready\s*\{([^}]+)\}/);
  assert.ok(ready, ".tabs .tabcount.ready rule must exist");
  assert.match(ready[1], /color\s*:\s*var\(--work\)/, "ready badge is --work");
  assert.match(ready[1], /background\s*:[^;]*var\(--work\)/,
    "ready badge background is mixed from --work, not --attn");
  assert.doesNotMatch(ready[1], /var\(--attn\)/,
    "the ready badge must not fall back to the waiting hue");
});

test("P7: the tabs signature separates a kind flip from a count change", () => {
  /* A tab that goes from 1 waiting to 1 ready keeps the same number and must
     still repaint — otherwise the badge keeps the wrong hue. */
  const groups = [{ id: "g1", name: "Alpha" }];
  const waiting = mapExports.mapTabsSignature("all", groups,
    { all: { n: 1, kind: "waiting" }, g1: { n: 1, kind: "waiting" } });
  const ready = mapExports.mapTabsSignature("all", groups,
    { all: { n: 1, kind: "ready" }, g1: { n: 1, kind: "ready" } });
  assert.notEqual(waiting, ready, "kind is part of the region signature");
});

test("P7: tab bar rebuilds only when counts (or tab set) change", () => {
  /* renderMapTabs is on the poll path via renderMap so live counts stay
     current — but the region must not rebuild every tick. Gate on a counts
     signature. Rename editor stays outside #maptabs (#tab_name in the sheet). */
  const maptabs = fakeEl("maptabs");
  const mapwrap = fakeEl("mapwrap");
  const nodes = [
    {
      id: "a", title: "Ask", description: "", agent: "x", model: "m", effort: "",
      lane_id: "L1", parent: "", ended_at: "", live: "quiet", attention: "approval",
      created_at: "2026-01-01T00:00:00Z", stops: [],
    },
    {
      id: "q", title: "Quiet", description: "", agent: "x", model: "m", effort: "",
      lane_id: "L1", parent: "", ended_at: "", live: "quiet", attention: "",
      created_at: "2026-01-02T00:00:00Z", stops: [],
    },
  ];
  let groups = [{ id: "g1", name: "Roadmap", lanes: ["L1"] }];
  const storage = memoryStorage({
    [MAP_FOLD_KNOWN_KEY]: JSON.stringify(["L1"]),
    [MAP_FOLD_KEY]: JSON.stringify([]),
  });
  const feature = createMapFeature({
    roots: {
      maptabs, mapwrap, lanechips: fakeEl("chips"),
      mapscroll: fakeEl("scroll"),
    },
    document: stubDocument({ classList: { contains: () => true, toggle(){}, add(){} } }),
    storage,
    isDesktop: () => true,
    mapOpen: () => true,
    level: () => 1,
    nodes: () => nodes,
    groups: () => groups,
    laneFilter: () => "",
    uiLoaded: () => true,
    laneModel: () => ({
      lanes: [{ id: "L1", name: "Lane" }],
      color: () => "#00f",
      name: () => "Lane",
      byId: Object.fromEntries(nodes.map(n => [n.id, n])),
    }),
    agentLogo: () => "",
  });

  feature.renderTabs();
  assert.match(maptabs.innerHTML, /tabcount">1</, "initial paint shows All/group count");
  const first = maptabs.innerHTML;

  // Same counts: no rebuild (poll-safety).
  maptabs.innerHTML = "STALE";
  feature.renderTabs();
  assert.equal(maptabs.innerHTML, "STALE",
    "renderTabs must not rewrite when counts and tab set are unchanged");

  // Poll path: renderMap also refreshes tabs, still gated.
  feature.render();
  assert.equal(maptabs.innerHTML, "STALE",
    "renderMap must not force a tab rewrite when counts are unchanged");

  // Count changes: rebuild. hardAttention (question) raises the waiting badge;
  // inspect must not (fixes-2 P1).
  nodes[1].attention = "inspect";
  feature.render();
  assert.equal(maptabs.innerHTML, "STALE",
    "inspect does not change waiting tab counts — no rebuild");
  nodes[1].attention = "question";
  feature.render();
  assert.notEqual(maptabs.innerHTML, "STALE", "hard-attention count change rebuilds");
  assert.match(maptabs.innerHTML, /tabcount">2</, "All count rose to 2");

  // Tab set change (rename) rebuilds even if counts are the same.
  maptabs.innerHTML = "STALE2";
  // Force a same-count recompute first so the sig is current, then rename.
  feature.renderTabs(); // may no-op if STALE2 left sig stale — call after real paint
  // Restore from last good counts: re-render once from real state.
  nodes[1].attention = "question";
  feature.invalidate?.();
  // Clear the tabs sig by changing groups.
  groups = [{ id: "g1", name: "Renamed", lanes: ["L1"] }];
  feature.renderTabs();
  assert.match(maptabs.innerHTML, /Renamed/, "group rename rebuilds the tab bar");
  assert.doesNotMatch(maptabs.innerHTML, /STALE/, "rename is not signature-skipped");

  feature.destroy();
  void first;
});
