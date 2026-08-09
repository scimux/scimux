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

const __dirname = dirname(fileURLToPath(import.meta.url));
const mapSrc = readFileSync(join(__dirname, "../js/map.js"), "utf8");
const notesCssSrc = readFileSync(join(__dirname, "../css/notes.css"), "utf8");

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

/* ---------- Phase 8: fare layer toggle + D1 tank (v1 meter retired in V2-P4) ---------- */

test("ctx_pct still rendered as tank, unchanged (D1)", () => {
  // Tank is the wall-map occupancy ring (D1) — distinct from the fare capsule.
  // Pin the SVG occupancy path byte-for-byte so a merge into the fare overlay is caught.
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
  assert.match(head, /m\/high · Quiet/);
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
  // V2-P4: no always-on fare text row; heat + optional capsule are the overlay.
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
      mapwrap, fareticket, farebtn: fakeEl("farebtn"), mapfullbtn: fakeEl("mapfullbtn"),
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
    openSheet: id => opened.push(id),
    icons: {
      ICON_FARE_ON: `<svg data-fare="on"></svg>`,
      ICON_FARE_OFF: `<svg data-fare="off"></svg>`,
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
    closest(sel){ return sel === "[data-fare-capsule]" ? this : null; },
  };
  firstListener(mapwrap, "click")({
    target: { closest(sel){ return capsuleEl.closest(sel); } },
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
  const farebtn = fakeEl("farebtn");
  const feature = createMapFeature({
    roots: {
      mapwrap, fareticket: fakeEl("fare_ticket"), farebtn,
      mapfullbtn: fakeEl("mapfullbtn"), lanechips: fakeEl("chips"),
      maptabs: fakeEl("tabs"), maptoolbar: fakeEl("tb"),
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
    openSheet: () => {},
    icons: {
      ICON_FARE_ON: `<svg data-fare="on"></svg>`,
      ICON_FARE_OFF: `<svg data-fare="off"></svg>`,
    },
  });
  feature.bind();
  feature.restoreChrome();
  feature.render();
  assert.doesNotMatch(mapwrap.innerHTML, /fare-capsule|data-fare-capsule/,
    "no capsule when fareOff");
  assert.doesNotMatch(mapwrap.innerHTML, /class="fare"/);

  // turn fare on → capsule appears without any selection
  firstListener(farebtn, "click")();
  feature.render();
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
    selectNode: (id, why) => calls.push(["select", id, why]),
    loadChatHistory: (id, at) => calls.push(["history", id, at]),
    jumpChatToNow: () => calls.push(["now"]),
  });
  feature.bind();

  // Select a station so the toolbar has [data-jump].
  firstListener(mapwrap, "click")({
    target: {
      dataset: { nid: "a", skey: "a#0", stop },
      closest(selector){ return selector === "[data-nid]" ? this : null; },
    },
  });
  assert.match(maptoolbar.innerHTML, /data-jump="a"/);

  // Full → Open chat docks and stays full.
  firstListener(maptoolbar, "click")({
    target: {
      dataset: { jump: "a" },
      closest(selector){ return selector === "[data-jump]" ? this : null; },
    },
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
    target: {
      dataset: { jump: "a" },
      closest(selector){ return selector === "[data-jump]" ? this : null; },
    },
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
  });
  feature.bind();

  // Establish selection (mapSel=a, mapSelKey=a#0, mapSelStop=stop) via wall tap.
  firstListener(mapwrap, "click")({
    target: {
      dataset: { nid: "a", skey: "a#0", stop },
      closest(selector){ return selector === "[data-nid]" ? this : null; },
    },
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
    document: {
      body,
      querySelector(sel){
        if (sel === "#app") return app;
        if (sel === "#mapdivider") return mapdivider;
        if (sel === "#chat") return chat;
        return null;
      },
    },
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
    "pill is decoration; whole 12px strip stays the hit target");

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
  assert.match(desk[1], /height:\s*12px/, "12px grab inside 900px");
  assert.match(desk[1], /margin:\s*-5px\s+0/, "negative margin grab expand inside 900px");
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

test("P3: escapeDockStep full truth table", () => {
  const step = mapExports.escapeDockStep;
  assert.equal(typeof step, "function", "escapeDockStep is exported");

  // mapFull:false → "none" at every (mapDock, dockPeek) input (phone / not full).
  for (const mapDock of [false, true]){
    for (const dockPeek of [false, true]){
      assert.equal(step({ mapFull: false, mapDock, dockPeek }), "none",
        `mapFull:false → none (mapDock=${mapDock}, dockPeek=${dockPeek})`);
    }
  }

  // Ladder under full screen: dock open → peek → undock → exit-full.
  assert.equal(step({ mapFull: true, mapDock: true, dockPeek: false }), "peek");
  assert.equal(step({ mapFull: true, mapDock: true, dockPeek: true }), "undock");
  assert.equal(step({ mapFull: true, mapDock: false, dockPeek: false }), "exit-full");
  // Defensive: peek flag without dock is not a real state → exit full screen.
  assert.equal(step({ mapFull: true, mapDock: false, dockPeek: true }), "exit-full");
});

test("P3: MAP_DOCK_PEEK_KEY and DOCK_PEEK_SNAP constants", () => {
  assert.equal(mapExports.MAP_DOCK_PEEK_KEY, "scimux-mapdockpeek");
  assert.equal(mapExports.DOCK_PEEK_SNAP, 0.06);
  const existing = [
    MAP_TAB_KEY, MAP_FOLD_KEY, MAP_FOLD_KNOWN_KEY, MAP_FULL_KEY, MAP_FARE_KEY,
    mapExports.MAP_DOCK_KEY, mapExports.MAP_DOCK_H_KEY,
  ];
  for (const k of existing){
    assert.notEqual(mapExports.MAP_DOCK_PEEK_KEY, k,
      `MAP_DOCK_PEEK_KEY must not reuse ${k}`);
  }
  // Existing clamp constants must not change (P3 fence).
  assert.equal(mapExports.DOCK_FRAC_MIN, 0.30);
  assert.equal(mapExports.DOCK_FRAC_MAX, 0.75);
  assert.equal(mapExports.DOCK_FRAC_DEFAULT, 0.55);
  assert.equal(mapExports.DOCK_FRAC_NUDGE, 0.05);
});

test("P3: CSS .dock-peek #chat height from var(--peek); msgwrap/promptbar/keyrow hidden; #map takes remainder", () => {
  /* Geometry via extracted declarations (§0) — no layout engine. All rules live
     inside the brace-matched 900px media block (phone has no dock). */
  const mediaBody = notesDesktopMediaBody(notesCssSrc);
  assert.ok(mediaBody, "900px media block present");

  const chatRule = mediaBody.match(
    /body\.map-full\.map-dock\.dock-peek\s+#chat\s*\{([^}]+)\}/);
  assert.ok(chatRule, "dock-peek #chat rule inside desktop media");
  assert.match(chatRule[1], /var\(--peek\)/, "#chat height/flex basis from --peek");
  assert.match(chatRule[1], /overflow:\s*hidden/, "strip clips content");
  assert.match(chatRule[1], /cursor:\s*pointer/, "whole strip is the tap target");

  const mapRule = mediaBody.match(
    /body\.map-full\.map-dock\.dock-peek\s+#map\s*\{([^}]+)\}/);
  assert.ok(mapRule, "dock-peek #map rule inside desktop media");
  assert.match(mapRule[1], /flex:\s*1\s+1\s+auto/,
    "#map takes the remainder (not pinned to --dockmap)");

  // Hidden regions — display:none, never removed from the DOM.
  for (const id of ["#msgwrap", "#promptbar", "#keyrow", "#chatmission",
    "#chatdetails", "#gauge", "#convtools", "#workpulse"]){
    assert.match(mediaBody,
      new RegExp(`body\\.map-full\\.map-dock\\.dock-peek\\s+${id.replace("#", "#")}`),
      `${id} hidden under dock-peek`);
    const hide = mediaBody.match(
      new RegExp(`body\\.map-full\\.map-dock\\.dock-peek\\s+${id}\\s*\\{([^}]+)\\}`));
    if (hide){
      assert.match(hide[1], /display:\s*none/, `${id} is display:none under dock-peek`);
    }
  }
  // Comma-grouped selectors are fine: assert each id appears with display:none
  // in the same rule body via a broader scan.
  const peekHideBlock = mediaBody.match(
    /body\.map-full\.map-dock\.dock-peek\s+#msgwrap[\s\S]*?\{([^}]+)\}/);
  assert.ok(peekHideBlock, "peek hide rule covering #msgwrap present");
  assert.match(peekHideBlock[1], /display:\s*none/);

  // Containment: no dock-peek layout rules outside the 900px media block.
  const outside = notesDesktopMediaOutside(notesCssSrc);
  assert.ok(outside != null, "brace-matched outside of 900px media");
  assert.doesNotMatch(outside, /body\.map-full\.map-dock\.dock-peek\s+#chat\s*\{[^}]*flex/,
    "peek #chat flex geometry must not leak outside 900px");
  assert.doesNotMatch(outside, /body\.map-full\.map-dock\.dock-peek\s+#map\s*\{/,
    "peek #map rule must not leak outside 900px");
});

test("P3: tap on the peek strip restores the PERSISTED fraction (not default 55%)", () => {
  const hKey = () => mapExports.MAP_DOCK_H_KEY || "scimux-mapdockh";
  const peekKey = () => mapExports.MAP_DOCK_PEEK_KEY || "scimux-mapdockpeek";
  const storage = memoryStorage({
    [MAP_FULL_KEY]: "1",
    [mapExports.MAP_DOCK_KEY || "scimux-mapdock"]: "1",
    [hKey()]: "0.42",
    [peekKey()]: "1",
  });
  const body = fakeDockBody(["map-full", "map-dock", "dock-peek"]);
  const { feature, chat } = createDockDividerFeature({ storage, body });
  feature.bind();
  // Peek restore must re-open the user's 42% split, never the 55% default.
  assert.equal(body.style.getPropertyValue("--dockmap"), "42%",
    "bind restores the persisted fraction under --dockmap");
  assert.equal(body.classList.contains("dock-peek"), true,
    "bind restores dock-peek from storage");

  const click = firstListener(chat, "click");
  assert.ok(click, "click listener on static #chat for strip restore");
  click({ target: chat, preventDefault(){} });

  assert.equal(body.classList.contains("dock-peek"), false,
    "tap leaves peek");
  assert.equal(body.style.getPropertyValue("--dockmap"), "42%",
    "tap restores persisted 42%, not DOCK_FRAC_DEFAULT 55%");
  assert.equal(storage.getItem(hKey()), "0.42",
    "tap does not rewrite the stored fraction");
  feature.destroy();
});

test("P3: drag past MAX+SNAP enters peek; leaves scimux-mapdockh untouched", () => {
  /* dockFrac is the MAP height share. Dragging the divider DOWN grows the
     fraction; peek is entered past DOCK_FRAC_MAX + DOCK_PEEK_SNAP (0.81), not
     below MIN. Plan §3 P3 text about "below 0.30" is wrong — this pins the
     corrected direction. */
  const hKey = () => mapExports.MAP_DOCK_H_KEY || "scimux-mapdockh";
  const storage = memoryStorage({
    [MAP_FULL_KEY]: "1",
    [mapExports.MAP_DOCK_KEY || "scimux-mapdock"]: "1",
    [hKey()]: "0.42",
  });
  const { feature, body, mapdivider } = createDockDividerFeature({ storage });
  feature.bind();
  assert.equal(body.style.getPropertyValue("--dockmap"), "42%");
  assert.equal(body.classList.contains("dock-peek"), false);

  // appTop=100, appHeight=1000 → raw = (clientY-100)/1000.
  // MAX+SNAP = 0.81 → clientY >= 910 enters peek.
  firstListener(mapdivider, "pointerdown")({
    target: mapdivider, pointerId: 1, clientY: 520, button: 0,
    preventDefault(){},
  });
  firstListener(mapdivider, "pointermove")({
    target: mapdivider, pointerId: 1, clientY: 920, // raw 0.82 ≥ 0.81
  });
  assert.equal(body.classList.contains("dock-peek"), true,
    "raw fraction past MAX+SNAP sets dock-peek");
  assert.equal(storage.getItem(hKey()), "0.42",
    "entering peek must not overwrite scimux-mapdockh");

  firstListener(mapdivider, "pointerup")({
    target: mapdivider, pointerId: 1, clientY: 920,
  });
  // Still no persist of a clamped 0.75 — peek is distinct from fraction 0.95.
  assert.equal(storage.getItem(hKey()), "0.42",
    "pointerup in peek leaves scimux-mapdockh untouched");
  assert.equal(body.classList.contains("dock-peek"), true);
  // Not "pinning --dockmap at 75%" as the peek stand-in: after the gesture the
  // CSS var reflects the last committed split (0.42), not the clamp ceiling.
  assert.equal(body.style.getPropertyValue("--dockmap"), "42%",
    "peek does not pin --dockmap at 75%");
  feature.destroy();
});

test("P3: peek persists and restores across bind()", () => {
  const peekKey = () => mapExports.MAP_DOCK_PEEK_KEY || "scimux-mapdockpeek";
  const hKey = () => mapExports.MAP_DOCK_H_KEY || "scimux-mapdockh";
  const storage = memoryStorage({
    [MAP_FULL_KEY]: "1",
    [mapExports.MAP_DOCK_KEY || "scimux-mapdock"]: "1",
    [hKey()]: "0.60",
  });
  const { feature, body, mapdivider } = createDockDividerFeature({ storage });
  feature.bind();

  // Enter peek via keyboard parity: ArrowDown at MAX.
  // Nudge from 0.60 up to the clamp, then one more Down enters peek.
  const key = firstListener(mapdivider, "keydown");
  assert.ok(key, "keydown on #mapdivider");
  key({ target: mapdivider, key: "ArrowDown", preventDefault(){} }); // 0.65
  key({ target: mapdivider, key: "ArrowDown", preventDefault(){} }); // 0.70
  key({ target: mapdivider, key: "ArrowDown", preventDefault(){} }); // 0.75
  key({ target: mapdivider, key: "ArrowDown", preventDefault(){} }); // peek
  assert.equal(body.classList.contains("dock-peek"), true,
    "ArrowDown at MAX enters peek");
  assert.equal(storage.getItem(peekKey()), "1", "peek is persisted");
  assert.equal(storage.getItem(hKey()), "0.75",
    "fraction at MAX is the last non-peek value (still persisted)");
  feature.destroy();

  // New feature instance + bind (device reopen) restores peek.
  const again = createDockDividerFeature({
    storage,
    body: fakeDockBody(["map-full", "map-dock"]),
  });
  again.feature.bind();
  assert.equal(again.body.classList.contains("dock-peek"), true,
    "bind restores dock-peek from storage");
  again.feature.restoreChrome();
  assert.equal(again.body.classList.contains("dock-peek"), true,
    "restoreChrome keeps dock-peek");
  // ArrowUp from peek restores the split (keyboard parity).
  firstListener(again.mapdivider, "keydown")({
    target: again.mapdivider, key: "ArrowUp", preventDefault(){},
  });
  assert.equal(again.body.classList.contains("dock-peek"), false,
    "ArrowUp from peek leaves peek");
  assert.equal(again.storage.getItem(peekKey()), null,
    "leaving peek clears the peek key");
  again.feature.destroy();
});

test("P3: Escape ladder end-to-end — peek → undock → exit-full → none", () => {
  /* app.js owns the single document Escape site; it must dispatch via
     escapeDockStep (not bare setMapFull(false)). Behaviour is exercised
     through the same step + feature API the shell wires. */
  assert.match(appSrcForDock, /escapeDockStep/,
    "app.js imports/uses escapeDockStep for Escape");
  assert.match(appSrcForDock, /setPeek|isPeek/,
    "app.js reaches peek state through the map feature API");

  const hKey = () => mapExports.MAP_DOCK_H_KEY || "scimux-mapdockh";
  const peekKey = () => mapExports.MAP_DOCK_PEEK_KEY || "scimux-mapdockpeek";
  const storage = memoryStorage({
    [MAP_FULL_KEY]: "1",
    [mapExports.MAP_DOCK_KEY || "scimux-mapdock"]: "1",
    [hKey()]: "0.55",
  });
  const { feature, body } = createDockDividerFeature({ storage });
  feature.bind();
  assert.equal(feature.isFull(), true);
  assert.equal(feature.isDock(), true);
  assert.equal(typeof feature.isPeek, "function", "isPeek on factory API");
  assert.equal(feature.isPeek(), false);

  // 1st Escape: dock open → peek
  assert.equal(appEscapeDockDispatch(feature), "peek");
  assert.equal(feature.isPeek(), true);
  assert.equal(body.classList.contains("dock-peek"), true);
  assert.equal(feature.isFull(), true);
  assert.equal(feature.isDock(), true);

  // 2nd Escape: peek → undock (stay full)
  assert.equal(appEscapeDockDispatch(feature), "undock");
  assert.equal(feature.isDock(), false);
  assert.equal(feature.isPeek(), false, "undock clears peek");
  assert.equal(body.classList.contains("map-dock"), false);
  assert.equal(body.classList.contains("dock-peek"), false);
  assert.equal(feature.isFull(), true);
  assert.equal(storage.getItem(peekKey()), null);

  // 3rd Escape: full wall → exit full screen
  assert.equal(appEscapeDockDispatch(feature), "exit-full");
  assert.equal(feature.isFull(), false);
  assert.equal(body.classList.contains("map-full"), false);

  // 4th Escape: no-op
  assert.equal(appEscapeDockDispatch(feature), "none");
  assert.equal(feature.isFull(), false);
  feature.destroy();
});

test("P3: setMapFull(false) clears both map-dock and dock-peek", () => {
  const peekKey = () => mapExports.MAP_DOCK_PEEK_KEY || "scimux-mapdockpeek";
  const storage = memoryStorage({
    [MAP_FULL_KEY]: "1",
    [mapExports.MAP_DOCK_KEY || "scimux-mapdock"]: "1",
    [peekKey()]: "1",
  });
  const body = fakeDockBody(["map-full", "map-dock", "dock-peek"]);
  const { feature } = createDockDividerFeature({ storage, body });
  feature.bind();
  assert.equal(body.classList.contains("dock-peek"), true);
  assert.equal(body.classList.contains("map-dock"), true);

  feature.setFull(false);

  assert.equal(feature.isFull(), false);
  assert.equal(feature.isDock(), false);
  assert.equal(typeof feature.isPeek === "function" && feature.isPeek(), false);
  assert.equal(body.classList.contains("map-full"), false);
  assert.equal(body.classList.contains("map-dock"), false,
    "setMapFull(false) clears map-dock");
  assert.equal(body.classList.contains("dock-peek"), false,
    "setMapFull(false) clears dock-peek — no stale class into a non-dock arrangement");
  assert.equal(storage.getItem(mapExports.MAP_DOCK_KEY || "scimux-mapdock"), null);
  assert.equal(storage.getItem(peekKey()), null);
  feature.destroy();
});
