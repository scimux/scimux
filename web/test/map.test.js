/* Characterization tests for web/js/map.js — pure Journeys decisions +
 * factory bind/destroy. Reuses lanes/map-model coverage; does not re-test
 * forkKind, laneColumnOrder, toggleMapSelection, or stop-chain algorithms. */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
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
  terminalStationSVG,
  terminalCapSVG,
  forkCueHTML,
  fareLineHTML,
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
  selectedSegmentRef,
  fmtRideMs,
  fareCapsuleHTML,
  fareCalloutHTML,
} from "../js/map.js";
import { toggleMapSelection, headStopKey } from "../js/map-model.js";
import { forkKind, stopsOf, stopKey, newestFirst } from "../js/lanes.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const mapSrc = readFileSync(join(__dirname, "../js/map.js"), "utf8");

/* ---------- module shape / no later features ---------- */
test("map.js exports factory and pure helpers; reuses lanes/map-model", () => {
  assert.match(mapSrc, /export function createMapFeature/);
  assert.match(mapSrc, /from "\.\/lanes\.js"/);
  assert.match(mapSrc, /from "\.\/map-model\.js"/);
  assert.match(mapSrc, /from "\.\/format\.js"/);
  assert.match(mapSrc, /from "\.\/cards\.js"/); // cardConfigText only
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

/* ---------- Phase 8: fare meter (journey tokens) + full-screen toggle ---------- */

const FARE_NODE = {
  fare_fresh_in: 32800,
  fare_out: 410,
  fare_cache_write: 25400,
  fare_cache_read: 1200,
  fare_total: 59810,
  fare_turns: 3,
  fare_cost: 0.6215,
  fare_cost_complete: true,
  fare_model: "claude-sonnet",
  ctx_pct: 42, // tank lives on the wall SVG ring, not the meter
};

test("fareLineHTML empty unless mapFull && fareOn", () => {
  assert.equal(fareLineHTML(FARE_NODE, { mapFull: true, fareOn: false }), "");
  assert.equal(fareLineHTML(FARE_NODE, { mapFull: false, fareOn: true }), "");
  assert.equal(fareLineHTML(FARE_NODE, { mapFull: false, fareOn: false }), "");
  const html = fareLineHTML(FARE_NODE, { mapFull: true, fareOn: true });
  assert.match(html, /class="fare"/);
});

test("fareLineHTML shows cumulative tokens", () => {
  const html = fareLineHTML(FARE_NODE, { mapFull: true, fareOn: true, escape: s => s });
  assert.match(html, /class="fare"/);
  // four canonical quantities labeled — pin presence/label, not exact glyphs
  assert.match(html, /fresh/i);
  assert.match(html, /out/i);
  assert.match(html, /cache-wr|cache.?wr|wr/i);
  assert.match(html, /cache-rd|cache.?rd|rd/i);
  // values appear in some form (compact k-suffix ok)
  assert.match(html, /32\.?8?\s*k|32800/i);
  assert.match(html, /410/);
  assert.match(html, /25\.?4?\s*k|25400/i);
  assert.match(html, /1\.?2?\s*k|1200/i);
  // optional annotations
  assert.match(html, /claude-sonnet|sonnet/i);
  assert.match(html, /3/);
});

test("fareLineHTML omits cost when not reported", () => {
  const incomplete = { ...FARE_NODE, fare_cost_complete: false, fare_cost: 0.99 };
  const noCost = fareLineHTML(incomplete, { mapFull: true, fareOn: true, escape: s => s });
  assert.doesNotMatch(noCost, /\$/);
  assert.doesNotMatch(noCost, /0\.99|0\.621/);
  // no estimate language either
  assert.doesNotMatch(noCost, /estimat/i);

  const complete = fareLineHTML(
    { ...FARE_NODE, fare_cost_complete: true, fare_cost: 0.6215 },
    { mapFull: true, fareOn: true, escape: s => s },
  );
  assert.match(complete, /\$/);
  assert.match(complete, /0\.62/);
});

test("fareLineHTML absent when no fare fields", () => {
  const bare = { id: "n", title: "T", ctx_pct: 50, created_at: "2026-01-01T00:00:00Z" };
  const html = fareLineHTML(bare, { mapFull: true, fareOn: true });
  // nothing, or a subtle dash — never zeroed quantities
  assert.ok(html === "" || /class="fare">\s*(—|&mdash;|–|-)\s*<\/div>/.test(html),
    `want empty or subtle dash, got ${JSON.stringify(html)}`);
  assert.doesNotMatch(html, /0\s*·\s*0\s*·\s*0/);
  assert.doesNotMatch(html, /fresh[^<]*\b0\b.*out[^<]*\b0\b/i);
});

test("ctx_pct still rendered as tank, unchanged", () => {
  // Tank is the wall-map occupancy ring (D1) — distinct from the fare meter.
  // Pin the SVG occupancy path byte-for-byte so a merge into fareLineHTML is caught.
  const tankSnippet = `if (n.ctx_pct != null && !n.attention){
        const R = 9, C = 2 * Math.PI * R, frac = Math.max(0, Math.min(1, n.ctx_pct / 100));
        svg += \`<circle cx="\${dotX}" cy="\${yy}" r="\${R}" fill="none" stroke="\${col}" stroke-width="2" opacity="\${op * .2}"/>\`;
        if (frac > 0)
          svg += \`<circle cx="\${dotX}" cy="\${yy}" r="\${R}" fill="none"
                stroke="\${n.ctx_pct >= 80 ? "var(--attn)" : col}" stroke-width="2"
                stroke-dasharray="\${frac * C} \${C}" stroke-linecap="round"
                transform="rotate(-90 \${dotX} \${yy})" opacity="\${op}"/>\`;
      }`;
  assert.ok(mapSrc.includes(tankSnippet), "wall tank SVG occupancy ring must be unchanged (D1)");
  // Meter must not re-absorb occupancy as "% ctx"
  const meter = fareLineHTML(FARE_NODE, { mapFull: true, fareOn: true });
  assert.doesNotMatch(meter, /% ctx/);
  assert.doesNotMatch(meter, /class="high"/);
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
    fareHTML: `<div class="fare">F</div>`,
  });
  assert.match(head, /data-nid="a"/);
  assert.match(head, /data-skey="a#1"/);
  assert.match(head, /agent-logo/);
  assert.match(head, /forkcue/);
  assert.match(head, /data-golane="L2"/);
  assert.match(head, /m\/high · Quiet/);
  assert.match(head, /class="fare"/);
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
    querySelector(){ return null; },
    querySelectorAll(){ return []; },
    closest(){ return null; },
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

test("createMapFeature bind is idempotent; destroy removes all map-owned listeners", () => {
  const maptabs = fakeEl("maptabs");
  const lanechips = fakeEl("lanechips");
  const mapwrap = fakeEl("mapwrap");
  const mapscroll = fakeEl("mapscroll");
  const maptoolbar = fakeEl("maptoolbar");
  const mapfullbtn = fakeEl("mapfullbtn");
  const farebtn = fakeEl("farebtn");
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
  const feature = createMapFeature({
    roots: {
      maptabs, lanechips, mapwrap, mapscroll, maptoolbar,
      mapfullbtn, farebtn, tabSave, tabDel,
    },
    window: win,
    document: { body: { classList: { contains: () => false, toggle(){}, add(){} } }, querySelector: () => null },
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
  assert.equal(farebtn._listenerCount("click"), 1);
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
  assert.equal(farebtn._totalListeners(), 0);
  assert.equal(tabSave._totalListeners(), 0);
  assert.equal(tabDel._totalListeners(), 0);
  assert.equal(winListeners.get("resize")?.size || 0, 0);
  assert.equal(longpressCleanups.length, 2);

  // re-bind after destroy works
  feature.bind();
  assert.equal(maptabs._listenerCount("click"), 1);
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
    document: { body, querySelector: () => null },
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

test("earlier-stop toolbar opens history before full-map selection is cleared", () => {
  const mapwrap = fakeEl("mapwrap");
  const maptoolbar = fakeEl("maptoolbar");
  const storage = memoryStorage({
    [MAP_FULL_KEY]: "1",
    [MAP_FOLD_KNOWN_KEY]: JSON.stringify(["L"]),
    [MAP_FOLD_KEY]: JSON.stringify([]),
  });
  const body = {
    classList: {
      _set: new Set(),
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
    document: { body, querySelector: () => null },
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

  const row = {
    dataset: { nid: "a", skey: "a#0", stop },
    closest(selector){ return selector === "[data-nid]" ? this : null; },
  };
  firstListener(mapwrap, "click")({ target: row });
  assert.match(maptoolbar.innerHTML, /data-jump="a"/);
  assert.match(maptoolbar.innerHTML, /earlier stop/);

  const jump = {
    dataset: { jump: "a" },
    closest(selector){ return selector === "[data-jump]" ? this : null; },
  };
  firstListener(maptoolbar, "click")({ target: jump });
  assert.deepEqual(calls, [["select", "a"], ["history", "a", stop]]);
  assert.equal(feature.isFull(), false);
  assert.equal(storage.getItem(MAP_FULL_KEY), null);
  assert.equal(body.classList.contains("map-full"), false);
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
  tabLanes.querySelectorAll = selector => selector === "input:checked"
    ? [{ value: "L1" }, { value: "L2" }]
    : [];
  const feature = createMapFeature({
    roots: {
      maptabs, tabHead, tabName, tabLanes, tabSave, tabDel,
      mapwrap: fakeEl("mapwrap"), lanechips: fakeEl("lanechips"),
    },
    document: {
      body: { classList: { contains: () => false, toggle(){}, add(){} } },
      querySelector: () => null,
    },
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

  const add = {
    dataset: { mt: "+" },
    closest(selector){
      if (selector === "[data-rm]") return null;
      return selector === "[data-mt]" ? this : null;
    },
  };
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
    document: { body: { classList: { toggle(){}, contains: () => false, add(){} } }, querySelector: () => null },
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
    document: { body: { classList: { contains: () => true, toggle(){}, add(){} } }, querySelector: () => null },
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
    document: { body: { classList: { contains: () => true, toggle(){}, add(){} } }, querySelector: () => null },
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
    document: { body: { classList: { contains: () => true, toggle(){}, add(){} } }, querySelector: () => null },
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
    document: { body: { classList: { contains: () => true, toggle(){}, add(){} } }, querySelector: () => null },
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

/* ---------- Phase 8: farebtn icon + wall re-render polling invariant ---------- */

test("farebtn is icon toggle with aria-pressed and on/off glyph state", () => {
  // Production uses inlined SVG (no FA webfont classes — TestPinnedIcons).
  // On/off = .on + aria-pressed + distinct glyph (data-fare marker).
  const farebtn = fakeEl("farebtn");
  const storage = memoryStorage();
  const body = {
    classList: {
      _set: new Set(),
      toggle(name, on){ if (on) this._set.add(name); else this._set.delete(name); },
      contains(name){ return this._set.has(name); },
      add(name){ this._set.add(name); },
    },
  };
  const feature = createMapFeature({
    roots: {
      farebtn, mapfullbtn: fakeEl("mapfullbtn"),
      maptoolbar: fakeEl("tb"), mapwrap: fakeEl("w"),
    },
    document: { body, querySelector: () => null },
    storage,
    isDesktop: () => true,
    mapOpen: () => true,
    level: () => 1,
    nodes: () => [],
    groups: () => [],
    laneFilter: () => "",
    uiLoaded: () => true,
    laneModel: () => ({ lanes: [], color: () => "", name: id => id, byId: {} }),
    icons: {
      ICON_FARE_ON: `<svg data-fare="on" aria-hidden="true">ON</svg>`,
      ICON_FARE_OFF: `<svg data-fare="off" aria-hidden="true">OFF</svg>`,
    },
  });
  feature.bind();
  feature.restoreChrome();
  assert.equal(farebtn.getAttribute("aria-label"), "toggle fare overlay");
  assert.equal(farebtn.getAttribute("aria-pressed"), "false");
  assert.equal(farebtn.classList.contains("on"), false);
  assert.match(farebtn.innerHTML, /data-fare="off"/);
  assert.doesNotMatch(farebtn.innerHTML, /data-fare="on"/);

  firstListener(farebtn, "click")();
  assert.equal(farebtn.getAttribute("aria-pressed"), "true");
  assert.equal(farebtn.classList.contains("on"), true);
  assert.equal(storage.getItem(MAP_FARE_KEY), "1");
  assert.match(farebtn.innerHTML, /data-fare="on"/);

  firstListener(farebtn, "click")();
  assert.equal(farebtn.getAttribute("aria-pressed"), "false");
  assert.equal(farebtn.classList.contains("on"), false);
  assert.equal(storage.getItem(MAP_FARE_KEY), null);
  assert.match(farebtn.innerHTML, /data-fare="off"/);
  feature.destroy();
});

test("farebtn chrome CSS: full-screen-only reveal, mapfullbtn sizing", () => {
  const layoutCss = readFileSync(join(__dirname, "../css/layout.css"), "utf8");
  assert.match(layoutCss, /#farebtn\s*\{[^}]*display:\s*none/s);
  assert.match(layoutCss, /body\.map-full\s+#farebtn\s*\{[^}]*display:\s*inline-flex/s);
  // sized like #mapfullbtn
  assert.match(layoutCss, /#farebtn\s*\{[^}]*width:\s*34px/s);
  assert.match(layoutCss, /#farebtn\s*\{[^}]*height:\s*34px/s);
  assert.match(layoutCss, /#farebtn\s*\{[^}]*border-radius:\s*9px/s);
  const indexHtml = readFileSync(join(__dirname, "../index.html"), "utf8");
  // farebtn immediately left of mapfullbtn
  assert.match(indexHtml, /id="farebtn"[^>]*>[\s\S]*?id="mapfullbtn"/);
  assert.match(indexHtml, /aria-label="toggle fare overlay"/);
});

test("wall re-render on fare change preserves external composer draft (polling invariant)", () => {
  // Composer is a singleton outside all polled render regions. A fare-driven
  // wall rebuild must only touch #mapwrap — never reset drafts or steal focus.
  const mapwrap = fakeEl("mapwrap");
  const composer = fakeEl("prompt");
  composer.value = "my draft stays";
  composer._focused = true;
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
    ctx_pct: 30, stops: [],
    fare_fresh_in: 100, fare_out: 20, fare_cache_write: 0, fare_cache_read: 50,
    fare_total: 170, fare_turns: 1, fare_cost_complete: false,
  };
  const nodes = [node];
  const storage = memoryStorage({
    [MAP_FULL_KEY]: "1",
    [MAP_FARE_KEY]: "1",
    [MAP_FOLD_KNOWN_KEY]: JSON.stringify(["L"]),
    [MAP_FOLD_KEY]: JSON.stringify([]),
  });
  const farebtn = fakeEl("farebtn");
  const feature = createMapFeature({
    roots: {
      mapwrap, farebtn, mapfullbtn: fakeEl("mapfullbtn"),
      lanechips: fakeEl("chips"), maptabs: fakeEl("tabs"), maptoolbar: fakeEl("tb"),
    },
    document: { body, querySelector: () => null },
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
      ICON_FARE_ON: `<svg data-fare="on"></svg>`,
      ICON_FARE_OFF: `<svg data-fare="off"></svg>`,
    },
  });
  // fareOn must be restored from storage (no longer dormant)
  feature.bind();
  feature.restoreChrome();
  feature.render();
  assert.match(mapwrap.innerHTML, /class="fare"/);
  assert.match(mapwrap.innerHTML, /fresh/i);
  assert.equal(composer.value, "my draft stays");
  assert.equal(composer._focused, true);

  // Poll-like fare growth: mutate node fare fields, re-render
  node.fare_total = 9000;
  node.fare_turns = 5;
  node.fare_fresh_in = 8000;
  feature.render(); // signature must notice fare fingerprint and rebuild
  assert.match(mapwrap.innerHTML, /class="fare"/);
  assert.match(mapwrap.innerHTML, /8\s*k|8000/i);
  // composer singleton untouched
  assert.equal(composer.value, "my draft stays");
  assert.equal(composer._focused, true);
  // map never owns the composer
  assert.doesNotMatch(mapSrc, /from "\.\/composer\.js"/);
  assert.doesNotMatch(mapSrc, /#prompt\b|getElementById\(["']prompt/);
  feature.destroy();
});

/* ---------- V2-P3: lane heat overlay (fare-design.md v2.3 / V2-P3) ----------
 * Heat only. No capsule, no callout, fareLineHTML stays (V2-P4). */

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
      mapwrap, farebtn: fakeEl("farebtn"), mapfullbtn: fakeEl("mapfullbtn"),
      lanechips: fakeEl("chips"), maptabs: fakeEl("tabs"), maptoolbar: fakeEl("tb"),
    },
    document: { body, querySelector: () => null },
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
      ICON_FARE_ON: `<svg data-fare="on"></svg>`,
      ICON_FARE_OFF: `<svg data-fare="off"></svg>`,
    },
  });
  feature.bind();
  feature.restoreChrome();
  feature.render();
  assert.equal(composer.value, "draft across heat update");
  assert.equal(composer._focused, true);
  // Wall track present
  assert.match(mapwrap.innerHTML, /<line\b/);
  // fareLineHTML still present (V2-P4 retires it — not this phase)
  assert.match(mapwrap.innerHTML, /class="fare"/);

  // Poll: only segment heat grows (whole-journey fields can stay put)
  node.fare_segments = [
    { total: 100, fresh_in: 80, out: 20 },
    { total: 80_000, fresh_in: 70_000, out: 10_000 },
  ];
  feature.render();
  assert.equal(composer.value, "draft across heat update");
  assert.equal(composer._focused, true);
  assert.match(mapwrap.innerHTML, /<line\b/);
  assert.match(mapwrap.innerHTML, /class="fare"/);
  // map still does not own the composer
  assert.doesNotMatch(mapSrc, /from "\.\/composer\.js"/);
  feature.destroy();
});

test("V2-P3 wall render uses wallLaneTrackSVG under fareOn; base track when fareOff", () => {
  // Integration: wall path must call the heat track helper (not only pure unit).
  // A bare export is not enough — renderWallMap must invoke it.
  assert.match(mapSrc, /svg \+= wallLaneTrackSVG\(/,
    "renderWallMap must draw tracks via wallLaneTrackSVG");
  // heatOn gated on fareOn inside the wall path (wall is already mapFull)
  assert.match(mapSrc, /heatOn:\s*fareOn/);
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

test("V2-P4 selectedSegmentRef maps wall stop selection to fare_segments[i]", () => {
  const n = {
    id: "a",
    created_at: "2026-01-01T00:00:00Z",
    stops: ["2026-01-02T00:00:00Z", "2026-01-03T00:00:00Z"],
    fare_segments: [
      { total: 1000, fresh_in: 800, out: 200 },
      { total: 5000, fresh_in: 4000, out: 1000 },
    ],
  };
  const chain = stopsOf(n);
  // stop i=0 creation → outbound segment 0
  assert.deepEqual(
    selectedSegmentRef(chain[0]),
    { nodeId: "a", segIdx: 0, seg: n.fare_segments[0] },
  );
  // stop i=1 intermediate → arrival segment 0
  assert.deepEqual(
    selectedSegmentRef(chain[1]),
    { nodeId: "a", segIdx: 0, seg: n.fare_segments[0] },
  );
  // stop i=2 head → arrival segment 1
  assert.deepEqual(
    selectedSegmentRef(chain[2]),
    { nodeId: "a", segIdx: 1, seg: n.fare_segments[1] },
  );

  // single-stop node: no track gap → null
  const lone = { id: "b", created_at: "2026-01-01T00:00:00Z", stops: [], fare_segments: [SEG_FULL] };
  assert.equal(selectedSegmentRef(stopsOf(lone)[0]), null);

  // no fare_segments → null
  const bare = { id: "c", created_at: "2026-01-01T00:00:00Z", stops: ["2026-01-02T00:00:00Z"] };
  assert.equal(selectedSegmentRef(stopsOf(bare)[1]), null);

  // null / missing stop
  assert.equal(selectedSegmentRef(null), null);
  assert.equal(selectedSegmentRef(undefined), null);
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

test("V2-P4 fareCapsuleHTML: headline tokens only; empty without segment", () => {
  assert.equal(fareCapsuleHTML(null, { x: 24, y: 50 }), "");
  assert.equal(fareCapsuleHTML(undefined, { x: 24, y: 50 }), "");

  const html = fareCapsuleHTML(SEG_FULL, { x: 24, y: 92, escape: s => s });
  assert.match(html, /fare-capsule|data-fare-capsule/, "capsule marker");
  // headline is the segment token fare (compact k ok)
  assert.match(html, /59\.?8?\s*k|59810/i);
  // at rest: no four-quantity breakdown, no time split in the capsule itself
  assert.doesNotMatch(html, /fresh/i);
  assert.doesNotMatch(html, /cache-wr|cache-rd/i);
  assert.doesNotMatch(html, /agent|tools|wait/i);
  // position anchors (midpoint)
  assert.match(html, /24|top|left|translate/i);
  // opaque surface — class or fill using surface/bg token
  assert.match(html, /fare-capsule|surface|--bg|--surface/i);
});

test("V2-P4 fareCalloutHTML: four tokens + real (agent·tools·wait); absent≠zero; D8 cost", () => {
  const full = fareCalloutHTML(SEG_FULL, { escape: s => s });
  assert.match(full, /fare-callout|data-fare-callout/);
  // four canonical quantities
  assert.match(full, /fresh/i);
  assert.match(full, /out/i);
  assert.match(full, /cache-wr|cache.?wr|wr/i);
  assert.match(full, /cache-rd|cache.?rd|rd/i);
  assert.match(full, /32\.?8?\s*k|32800/i);
  assert.match(full, /410/);
  // time: real with split
  assert.match(full, /real/i);
  assert.match(full, /agent/i);
  assert.match(full, /tools/i);
  assert.match(full, /wait/i);
  // cost when complete
  assert.match(full, /\$/);
  assert.match(full, /0\.62/);

  // no split → real alone, never fabricated agent/tools/wait zeros
  const noSplit = fareCalloutHTML(SEG_NO_SPLIT, { escape: s => s });
  assert.match(noSplit, /real/i);
  assert.doesNotMatch(noSplit, /\bagent\b/i);
  assert.doesNotMatch(noSplit, /\btools\b/i);
  assert.doesNotMatch(noSplit, /\bwait\b/i);
  // still shows tokens
  assert.match(noSplit, /fresh/i);

  // incomplete cost → no $
  const noCost = fareCalloutHTML(SEG_NO_COST, { escape: s => s });
  assert.doesNotMatch(noCost, /\$/);
  assert.doesNotMatch(noCost, /0\.99|estimat/i);

  assert.equal(fareCalloutHTML(null), "");
});

test("V2-P4 fareCapsuleHTML with calloutOpen includes callout body", () => {
  const closed = fareCapsuleHTML(SEG_FULL, { x: 10, y: 20, calloutOpen: false, escape: s => s });
  assert.doesNotMatch(closed, /fare-callout|data-fare-callout/);

  const open = fareCapsuleHTML(SEG_FULL, { x: 10, y: 20, calloutOpen: true, escape: s => s });
  assert.match(open, /fare-capsule|data-fare-capsule/);
  assert.match(open, /fare-callout|data-fare-callout/);
  assert.match(open, /fresh/i);
  assert.match(open, /real/i);
});

test("V2-P4 wallMapSignature folds callout-open so idle poll skips", () => {
  const n = {
    id: "a", title: "T", description: "d", agent: "x", model: "m", effort: "",
    lane_id: "L", parent: "", ended_at: "", live: "quiet", attention: "",
    created_at: "2026-01-01T00:00:00Z", last_activity: 100, ctx_pct: 10,
    fare_total: 170, fare_turns: 1, fare_cost_complete: false,
    stops: ["2026-01-02T00:00:00Z"],
    fare_segments: [SEG_NO_SPLIT, { total: 50, fresh_in: 40, out: 10, real_ms: 1000 }],
  };
  const rows = stopsOf(n).sort(newestFirst);
  const base = {
    focusLane: null, mapTab: "all", mapSel: "a", mapSelKey: "a#1", fareOn: true,
  };
  const wClosed = wallMapSignature(rows, ["L"], { ...base, fareCalloutOpen: false });
  const wOpen = wallMapSignature(rows, ["L"], { ...base, fareCalloutOpen: true });
  assert.notEqual(wClosed, wOpen, "callout open/closed must change signature");
  // stable under unchanged poll
  assert.equal(
    wallMapSignature(rows, ["L"], { ...base, fareCalloutOpen: true }),
    wOpen,
  );
  assert.equal(mapRenderDecision(wOpen, wOpen), "skip");
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
  assert.match(mapSrc, /export function fareCalloutHTML\b/);
  assert.match(mapSrc, /export function selectedSegmentRef\b/);
});

test("V2-P4 wall: capsule only on selected segment; none when nothing selected", () => {
  const mapwrap = fakeEl("mapwrap");
  const composer = fakeEl("prompt");
  composer.value = "draft stays for capsule";
  composer._focused = true;
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
    fare_fresh_in: 100, fare_out: 20, fare_cache_write: 0, fare_cache_read: 50,
    fare_total: 170, fare_turns: 2, fare_cost_complete: false,
    fare_segments: [
      { total: 100, fresh_in: 80, cache_read: 0, cache_write: 0, out: 20,
        real_ms: 5000, agent_ms: 3000, tools_ms: 1000, wait_ms: 1000 },
      { total: 70, fresh_in: 50, cache_read: 0, cache_write: 0, out: 20,
        real_ms: 2000 },
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
      mapwrap, farebtn: fakeEl("farebtn"), mapfullbtn: fakeEl("mapfullbtn"),
      lanechips: fakeEl("chips"), maptabs: fakeEl("tabs"), maptoolbar: fakeEl("tb"),
    },
    document: { body, querySelector: () => null },
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
      ICON_FARE_ON: `<svg data-fare="on"></svg>`,
      ICON_FARE_OFF: `<svg data-fare="off"></svg>`,
    },
  });
  feature.bind();
  feature.restoreChrome();
  feature.render();

  // Nothing selected → heat may be on, but no capsule and no always-on fare text row
  assert.doesNotMatch(mapwrap.innerHTML, /fare-capsule|data-fare-capsule/);
  assert.doesNotMatch(mapwrap.innerHTML, /class="fare"/);
  assert.equal(composer.value, "draft stays for capsule");
  assert.equal(composer._focused, true);

  // Select head stop via wall click simulation (data-nid + data-skey)
  const headKey = "a#1";
  const fakeRow = {
    dataset: { nid: "a", skey: headKey, stop: "" },
    closest(sel){
      if (sel === "[data-nid]") return this;
      if (sel === "[data-fold]" || sel === "[data-jump]" || sel === "[data-medit]"
        || sel === "[data-forkfrom]" || sel === "[data-golane]" || sel === "[data-goorigin]"
        || sel === "[data-fare-capsule]") return null;
      return null;
    },
  };
  const clickEv = { target: { closest(sel){ return fakeRow.closest(sel); } } };
  firstListener(mapwrap, "click")(clickEv);

  assert.match(mapwrap.innerHTML, /fare-capsule|data-fare-capsule/,
    "capsule appears on selected segment");
  // headline tokens for the arrival segment of head (seg 0: total 100)
  assert.match(mapwrap.innerHTML, /\b100\b|0\.1\s*k/i);
  // no always-on fare text meter
  assert.doesNotMatch(mapwrap.innerHTML, /class="fare"/);
  // callout closed at rest
  assert.doesNotMatch(mapwrap.innerHTML, /fare-callout|data-fare-callout/);

  // Tap capsule → callout opens with breakdown
  const capsuleEl = {
    dataset: { fareCapsule: "1" },
    closest(sel){
      if (sel === "[data-fare-capsule]") return this;
      return null;
    },
  };
  firstListener(mapwrap, "click")({
    target: { closest(sel){ return capsuleEl.closest(sel); } },
    stopPropagation(){},
    preventDefault(){},
  });
  assert.match(mapwrap.innerHTML, /fare-callout|data-fare-callout/);
  assert.match(mapwrap.innerHTML, /fresh/i);
  assert.match(mapwrap.innerHTML, /real/i);
  assert.match(mapwrap.innerHTML, /agent/i);

  // Poll: segment tokens grow; capsule text updates; composer + callout state survive
  node.fare_segments = [
    { total: 9000, fresh_in: 8000, cache_read: 0, cache_write: 0, out: 1000,
      real_ms: 5000, agent_ms: 3000, tools_ms: 1000, wait_ms: 1000 },
    { total: 70, fresh_in: 50, cache_read: 0, cache_write: 0, out: 20, real_ms: 2000 },
  ];
  feature.render();
  assert.match(mapwrap.innerHTML, /fare-capsule|data-fare-capsule/);
  assert.match(mapwrap.innerHTML, /9\s*k|9000/i);
  assert.match(mapwrap.innerHTML, /fare-callout|data-fare-callout/,
    "callout open state survives poll rebuild");
  assert.equal(composer.value, "draft stays for capsule");
  assert.equal(composer._focused, true);

  feature.destroy();
});

test("V2-P4 gated off when !fareOn or not full-screen; no dangling fareLineHTML", () => {
  // Pure gate already covered; pin wall path wiring + retirement.
  assert.match(mapSrc, /fareCapsuleHTML\s*\(/,
    "renderWallMap must draw the capsule via fareCapsuleHTML");
  assert.match(mapSrc, /selectedSegmentRef\s*\(/,
    "segment selection must use selectedSegmentRef from wall selection");
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
    fare_segments: [
      { total: 100, fresh_in: 80, out: 20, real_ms: 5000 },
      { total: 70, fresh_in: 50, out: 20, real_ms: 2000 },
    ],
  };
  const storage = memoryStorage({
    [MAP_FULL_KEY]: "1",
    // fare OFF
    [MAP_FOLD_KNOWN_KEY]: JSON.stringify(["L"]),
    [MAP_FOLD_KEY]: JSON.stringify([]),
  });
  const farebtn = fakeEl("farebtn");
  const feature = createMapFeature({
    roots: {
      mapwrap, farebtn, mapfullbtn: fakeEl("mapfullbtn"),
      lanechips: fakeEl("chips"), maptabs: fakeEl("tabs"), maptoolbar: fakeEl("tb"),
    },
    document: { body, querySelector: () => null },
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
    icons: {
      ICON_FARE_ON: `<svg data-fare="on"></svg>`,
      ICON_FARE_OFF: `<svg data-fare="off"></svg>`,
    },
  });
  feature.bind();
  feature.restoreChrome();
  feature.render();
  // select a stop while fare is off
  const fakeRow = {
    dataset: { nid: "a", skey: "a#1", stop: "" },
    closest(sel){
      if (sel === "[data-nid]") return this;
      if (sel === "[data-fare-capsule]") return null;
      return null;
    },
  };
  firstListener(mapwrap, "click")({
    target: { closest(sel){ return fakeRow.closest(sel); } },
  });
  assert.doesNotMatch(mapwrap.innerHTML, /fare-capsule|data-fare-capsule/,
    "no capsule when fareOff");
  assert.doesNotMatch(mapwrap.innerHTML, /class="fare"/);

  // turn fare on → capsule appears for selection
  firstListener(farebtn, "click")();
  feature.render();
  assert.match(mapwrap.innerHTML, /fare-capsule|data-fare-capsule/,
    "capsule when fareOn + selection");
  feature.destroy();
});
