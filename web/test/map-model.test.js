/* Characterization tests for web/js/map-model.js — pure card/map decisions.
 * Imports the real module (not HTML substring extraction). */
import test from "node:test";
import assert from "node:assert/strict";
import {
  hardAttention,
  cardState,
  statusText,
  cardCreatedMS,
  cardInteractionMS,
  livenessTier,
  orderedNodes,
  pinnedOrder,
  isArchived,
  isPinned,
  tabListForCards,
  inLaneScope,
  visibleCardLists,
  headStopKey,
  toggleMapSelection,
  stationLatestTurn,
} from "../js/map-model.js";
import { stopTimes, servedLanes, stopKey } from "../js/lanes.js";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));
const mapSrc = readFileSync(join(__dirname, "../js/map-model.js"), "utf8");

/* ---------- purity / no browser globals ---------- */
test("map-model.js has no browser globals or implicit app state", () => {
  const code = mapSrc
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/\/\/.*$/gm, "");
  const forbidden = [
    /\bdocument\b/,
    /\bwindow\b/,
    /\bfetch\b/,
    /\blocalStorage\b/,
    /\bnavigator\b/,
    /\bsetTimeout\b/,
    /\bsetInterval\b/,
    /\brequestAnimationFrame\b/,
    /\bgetComputedStyle\b/,
    /\bcreateElement\b/,
    /\binnerHTML\b/,
    /\brenderCards\b/,
    /\brenderMap\b/,
    /\bcardMetaHTML\b/,
    /\bcardTimeHTML\b/,
    /\bnodeById\b/,
  ];
  for (const re of forbidden) {
    assert.equal(re.test(code), false, `map-model.js must not reference ${re}`);
  }
  assert.equal(/\bUI\./.test(code), false, "must not read UI.*");
  // No implicit globals: orderedNodes must take nodes; pinnedOrder takes pinned
  assert.match(mapSrc, /export function orderedNodes\(nodes\)/);
  assert.match(mapSrc, /export function pinnedOrder\(list, pinned\)/);
  // stopTimes must be imported, not reimplemented
  assert.match(mapSrc, /from\s+["']\.\/lanes\.js["']/);
  assert.equal(/\bfunction\s+stopTimes\b/.test(code), false);
  assert.equal(/\bconst\s+stopTimes\b/.test(code), false);
  assert.equal(/\bfunction\s+servedLanes\b/.test(code), false);
});

test("map-model.js exports named pure helpers", async () => {
  const mod = await import("../js/map-model.js");
  for (const name of [
    "hardAttention", "cardState", "statusText",
    "cardCreatedMS", "cardInteractionMS", "livenessTier",
    "orderedNodes", "pinnedOrder",
    "isArchived", "isPinned", "tabListForCards", "inLaneScope", "visibleCardLists",
    "headStopKey", "toggleMapSelection",
  ]) {
    assert.ok(name in mod, name);
  }
});

test("map-model.js reuses lanes.js stopTimes (no local copy)", () => {
  // headStopKey must agree with stopTimes length from the shared export
  const n = { id: "a", created_at: "2026-01-01T00:00:00Z", stops: ["2026-01-02T00:00:00Z"] };
  assert.equal(stopTimes(n).length, 2);
  assert.equal(headStopKey("a", [n]), "a#1");
  assert.equal(servedLanes({ lane_id: "L1" })[0], "L1");
});

/* ---------- hardAttention ---------- */
test("hardAttention: only non-inspect truthy attention", () => {
  // Live expression: n.attention && n.attention !== "inspect"
  // (returns undefined/"" when missing/empty — falsy, not strictly false)
  assert.equal(!!hardAttention({}), false);
  assert.equal(!!hardAttention({ attention: "" }), false);
  assert.equal(hardAttention({ attention: "inspect" }), false);
  assert.equal(hardAttention({ attention: "approval" }), true);
  assert.equal(hardAttention({ attention: "question" }), true);
  assert.equal(hardAttention({ attention: "input" }), true);
});

/* ---------- cardState / statusText precedence ---------- */
test("cardState precedence: closed > attention > working > dead > idle", () => {
  assert.equal(cardState({ ended_at: "2026-01-01T00:00:00Z", attention: "approval", live: "active" }), "closed");
  assert.equal(cardState({ attention: "approval", live: "active" }), "attention");
  assert.equal(cardState({ attention: "inspect", live: "active" }), "working"); // inspect is not hard
  assert.equal(cardState({ live: "active" }), "working");
  assert.equal(cardState({ live: "exited" }), "dead");
  assert.equal(cardState({ live: "unavailable" }), "dead");
  assert.equal(cardState({ live: "quiet" }), "idle");
  assert.equal(cardState({}), "idle");
});

test("statusText precedence: Closed > inspect > hard attention > live tiers > Quiet", () => {
  assert.equal(statusText({ ended_at: "t", attention: "approval", live: "active" }), "Closed");
  assert.equal(statusText({ attention: "inspect" }), "Quiet · check the terminal");
  assert.equal(statusText({ attention: "approval" }), "Waiting · needs your approval");
  assert.equal(statusText({ attention: "question" }), "Waiting · needs your question");
  assert.equal(statusText({ live: "active" }), "Running");
  assert.equal(statusText({ live: "exited" }), "Exited");
  assert.equal(statusText({ live: "unavailable" }), "Unavailable");
  assert.equal(statusText({ live: "quiet" }), "Quiet");
  assert.equal(statusText({}), "Quiet");
});

/* ---------- liveness tiers ---------- */
test("livenessTier mirrors cardState precedence numbers", () => {
  assert.equal(livenessTier({ ended_at: "t" }), 4);
  assert.equal(livenessTier({ attention: "approval" }), 0);
  assert.equal(livenessTier({ attention: "inspect", live: "active" }), 1); // inspect not hard
  assert.equal(livenessTier({ live: "active" }), 1);
  assert.equal(livenessTier({ live: "exited" }), 3);
  assert.equal(livenessTier({ live: "unavailable" }), 3);
  assert.equal(livenessTier({ live: "quiet" }), 2);
  assert.equal(livenessTier({}), 2);
});

/* ---------- orderedNodes: Go fixture + ties + fresh ---------- */
test("orderedNodes: attention then fresh then interaction recency (Go fixture)", () => {
  // Mirrors TestActivityCardOrderPinsAttentionThenFreshCards
  const nodes = [
    { id: "active-older", last_activity: 900, last_interaction: 200, created_at: "2026-01-04T00:00:00Z" },
    { id: "newer-fresh", created_at: "2026-01-06T00:00:00Z" },
    { id: "active-newer", last_activity: 300, last_interaction: 800, created_at: "2026-01-01T00:00:00Z" },
    { id: "never-touched", last_activity: 700, created_at: "2026-01-03T00:00:00Z" },
    { id: "older-fresh", created_at: "2026-01-05T00:00:00Z" },
    { id: "attn", attention: "approval", last_activity: 100, last_interaction: 100, created_at: "2026-01-02T00:00:00Z" },
  ];
  assert.deepEqual(
    orderedNodes(nodes).map(n => n.id),
    ["attn", "newer-fresh", "older-fresh", "active-newer", "active-older", "never-touched"],
  );
});

test("orderedNodes: hard attention outranks live active regardless of interaction age", () => {
  const nodes = [
    { id: "working", live: "active", last_activity: 999, last_interaction: 999 },
    { id: "needs", attention: "approval", last_activity: 1, last_interaction: 1 },
  ];
  assert.deepEqual(orderedNodes(nodes).map(n => n.id), ["needs", "working"]);
});

test("orderedNodes: closed sinks below dead and idle", () => {
  const nodes = [
    { id: "closed", ended_at: "t", last_interaction: 999 },
    { id: "dead", live: "exited", last_interaction: 1 },
    { id: "idle", live: "quiet", last_interaction: 2 },
  ];
  assert.deepEqual(orderedNodes(nodes).map(n => n.id), ["idle", "dead", "closed"]);
});

test("orderedNodes: fresh nodes (no last_activity) float above interacted same-tier", () => {
  const nodes = [
    { id: "touched", last_activity: 50, last_interaction: 50, created_at: "2026-01-01T00:00:00Z" },
    { id: "fresh", created_at: "2026-01-02T00:00:00Z" },
  ];
  assert.deepEqual(orderedNodes(nodes).map(n => n.id), ["fresh", "touched"]);
});

test("orderedNodes: among fresh nodes, newer created_at first", () => {
  const nodes = [
    { id: "old", created_at: "2026-01-01T00:00:00Z" },
    { id: "new", created_at: "2026-01-03T00:00:00Z" },
  ];
  assert.deepEqual(orderedNodes(nodes).map(n => n.id), ["new", "old"]);
});

test("orderedNodes: interaction ties fall through to last_activity then created_at", () => {
  const nodes = [
    { id: "a", last_activity: 10, last_interaction: 100, created_at: "2026-01-01T00:00:00Z" },
    { id: "b", last_activity: 20, last_interaction: 100, created_at: "2026-01-02T00:00:00Z" },
  ];
  // same interaction → higher last_activity wins
  assert.deepEqual(orderedNodes(nodes).map(n => n.id), ["b", "a"]);

  const tied = [
    { id: "x", last_activity: 10, last_interaction: 100, created_at: "2026-01-01T00:00:00Z" },
    { id: "y", last_activity: 10, last_interaction: 100, created_at: "2026-01-03T00:00:00Z" },
  ];
  assert.deepEqual(orderedNodes(tied).map(n => n.id), ["y", "x"]);
});

test("orderedNodes: does not mutate input array", () => {
  const nodes = [
    { id: "b", last_activity: 1 },
    { id: "a", last_activity: 2 },
  ];
  const copy = nodes.slice();
  orderedNodes(nodes);
  assert.deepEqual(nodes.map(n => n.id), copy.map(n => n.id));
});

test("cardCreatedMS / cardInteractionMS boundaries", () => {
  assert.equal(cardCreatedMS({}), 0);
  assert.equal(cardCreatedMS({ created_at: "not-a-date" }), 0);
  assert.equal(cardCreatedMS({ created_at: "2026-01-01T00:00:00.000Z" }), Date.parse("2026-01-01T00:00:00.000Z"));
  assert.equal(cardInteractionMS({}), 0);
  assert.equal(cardInteractionMS({ last_interaction: 42 }), 42);
});

/* ---------- pinnedOrder: attention-first then pin index ---------- */
test("pinnedOrder: hard attention floats above pin order", () => {
  const pinned = ["a", "b", "c"]; // a is first (top) in pin order
  const list = [
    { id: "a" },
    { id: "b", attention: "approval" },
    { id: "c" },
  ];
  assert.deepEqual(pinnedOrder(list, pinned).map(n => n.id), ["b", "a", "c"]);
});

test("pinnedOrder: without attention, preserves pin array index", () => {
  const pinned = ["c", "a", "b"]; // c was pinned most recently (unshift)
  const list = [{ id: "a" }, { id: "b" }, { id: "c" }];
  assert.deepEqual(pinnedOrder(list, pinned).map(n => n.id), ["c", "a", "b"]);
});

test("pinnedOrder: inspect does not float (not hard attention)", () => {
  const pinned = ["a", "b"];
  const list = [
    { id: "a" },
    { id: "b", attention: "inspect" },
  ];
  assert.deepEqual(pinnedOrder(list, pinned).map(n => n.id), ["a", "b"]);
});

/* ---------- visibility: current/archived/pinned + lane scope ---------- */
test("tabListForCards current: non-archived plus archived hard-attention", () => {
  const nodes = [
    { id: "live", last_activity: 1 },
    { id: "arch-quiet" },
    { id: "arch-attn", attention: "approval" },
    { id: "inspect-arch", attention: "inspect" },
  ];
  const archived = ["arch-quiet", "arch-attn", "inspect-arch"];
  const ids = tabListForCards(nodes, { cardTab: "current", archived }).map(n => n.id);
  assert.ok(ids.includes("live"));
  assert.ok(ids.includes("arch-attn"), "archived hard-attention must surface in current");
  assert.equal(ids.includes("arch-quiet"), false);
  assert.equal(ids.includes("inspect-arch"), false, "inspect is not hard attention");
});

test("tabListForCards archived: only archived (including hard attention)", () => {
  const nodes = [
    { id: "live" },
    { id: "arch", last_activity: 1 },
    { id: "arch-attn", attention: "approval" },
  ];
  const archived = ["arch", "arch-attn"];
  const ids = tabListForCards(nodes, { cardTab: "archived", archived }).map(n => n.id);
  assert.deepEqual(ids.sort(), ["arch", "arch-attn"].sort());
});

test("tabListForCards pinned: pinned only, attention-first", () => {
  const nodes = [
    { id: "p1" },
    { id: "p2", attention: "question" },
    { id: "other" },
  ];
  const pinned = ["p1", "p2"];
  const ids = tabListForCards(nodes, { cardTab: "pinned", pinned }).map(n => n.id);
  assert.deepEqual(ids, ["p2", "p1"]);
});

test("inLaneScope: empty filter is all; selectedId exempt; served lane membership", () => {
  const n = { id: "x", lane_id: "alpha" };
  assert.equal(inLaneScope(n, { laneFilter: "" }), true);
  assert.equal(inLaneScope(n, { laneFilter: null }), true);
  assert.equal(inLaneScope(n, { laneFilter: "alpha" }), true);
  assert.equal(inLaneScope(n, { laneFilter: "beta" }), false);
  assert.equal(inLaneScope(n, { laneFilter: "beta", selectedId: "x" }), true);
  assert.equal(inLaneScope(n, { laneFilter: "beta", selectedId: "other" }), false);
});

test("visibleCardLists: pinned ignores lane scope", () => {
  const nodes = [
    { id: "a", lane_id: "L1" },
    { id: "b", lane_id: "L2" },
  ];
  const { list, foldList } = visibleCardLists(nodes, {
    cardTab: "pinned",
    pinned: ["a", "b"],
    laneFilter: "L1",
    selectedId: "",
  });
  assert.deepEqual(list.map(n => n.id).sort(), ["a", "b"]);
  assert.equal(foldList.length, 0);
});

test("visibleCardLists: lane filter + selected exemption + attention fold", () => {
  const nodes = [
    { id: "in", lane_id: "L1", last_activity: 1 },
    { id: "out-quiet", lane_id: "L2", last_activity: 1 },
    { id: "out-attn", lane_id: "L2", attention: "approval" },
    { id: "selected-out", lane_id: "L2", last_activity: 2 },
  ];
  const { list, foldList } = visibleCardLists(nodes, {
    cardTab: "current",
    archived: [],
    laneFilter: "L1",
    selectedId: "selected-out",
  });
  const listIds = list.map(n => n.id);
  assert.ok(listIds.includes("in"));
  assert.ok(listIds.includes("selected-out"), "open chat always visible");
  assert.equal(listIds.includes("out-quiet"), false);
  assert.equal(listIds.includes("out-attn"), false, "out-of-scope attention goes to fold not list");
  assert.deepEqual(foldList.map(n => n.id), ["out-attn"]);
});

test("visibleCardLists: no fold when laneFilter empty or not current tab", () => {
  const nodes = [{ id: "a", lane_id: "L2", attention: "approval" }];
  assert.equal(visibleCardLists(nodes, { cardTab: "current", archived: [], laneFilter: "" }).foldList.length, 0);
  assert.equal(visibleCardLists(nodes, {
    cardTab: "archived", archived: ["a"], laneFilter: "L1",
  }).foldList.length, 0);
});

test("isArchived / isPinned helpers", () => {
  assert.equal(isArchived("a", ["a", "b"]), true);
  assert.equal(isArchived("c", ["a"]), false);
  assert.equal(isPinned("x", ["x"]), true);
  assert.equal(isPinned("y", []), false);
});

/* ---------- wall-map stop-scoped selection ---------- */
test("headStopKey: head index from stopTimes; missing node → #0", () => {
  const n = {
    id: "n1",
    created_at: "2026-01-01T00:00:00Z",
    stops: ["2026-01-02T00:00:00Z", "2026-01-03T00:00:00Z"],
  };
  assert.equal(stopTimes(n).length, 3);
  assert.equal(headStopKey("n1", [n]), "n1#2");
  assert.equal(headStopKey("missing", [n]), "missing#0");
  assert.equal(headStopKey("n1", []), "n1#0");
});

test("toggleMapSelection: select head, second tap deselects, clears stop time", () => {
  const nodes = [{ id: "n1", created_at: "t0", stops: ["t1"] }];
  const head = headStopKey("n1", nodes);
  assert.equal(head, "n1#1");

  const s1 = toggleMapSelection({ id: "n1", key: head, stopTime: "", mapSelKey: "", nodes });
  assert.deepEqual(s1, { mapSel: "n1", mapSelKey: "n1#1", mapSelStop: "" });

  const s2 = toggleMapSelection({ id: "n1", key: head, stopTime: "", mapSelKey: "n1#1", nodes });
  assert.deepEqual(s2, { mapSel: "", mapSelKey: "", mapSelStop: "" });
});

test("toggleMapSelection: earlier stop keeps stopTime; empty key falls back to head", () => {
  const nodes = [{ id: "n1", created_at: "t0", stops: ["t1", "t2"] }];
  // earlier stop index 0
  const key = stopKey({ n: nodes[0], i: 0 });
  assert.equal(key, "n1#0");

  const s1 = toggleMapSelection({
    id: "n1", key, stopTime: "t0", mapSelKey: "", nodes,
  });
  assert.deepEqual(s1, { mapSel: "n1", mapSelKey: "n1#0", mapSelStop: "t0" });

  // second tap on same earlier stop deselects and clears stop time
  const s2 = toggleMapSelection({
    id: "n1", key, stopTime: "t0", mapSelKey: "n1#0", nodes,
  });
  assert.deepEqual(s2, { mapSel: "", mapSelKey: "", mapSelStop: "" });

  // missing key → head
  const s3 = toggleMapSelection({ id: "n1", mapSelKey: "", nodes });
  assert.equal(s3.mapSelKey, "n1#2");
  assert.equal(s3.mapSelStop, "");
});

test("toggleMapSelection: switching stops replaces selection (no deselect)", () => {
  const nodes = [{ id: "n1", created_at: "t0", stops: ["t1"] }];
  const s = toggleMapSelection({
    id: "n1", key: "n1#0", stopTime: "t0", mapSelKey: "n1#1", nodes,
  });
  assert.deepEqual(s, { mapSel: "n1", mapSelKey: "n1#0", mapSelStop: "t0" });
});

test("toggleMapSelection: missing node still produces key via headStopKey fallback", () => {
  const s = toggleMapSelection({ id: "ghost", mapSelKey: "", nodes: [] });
  assert.deepEqual(s, { mapSel: "ghost", mapSelKey: "ghost#0", mapSelStop: "" });
});

/* ---------- stationLatestTurn (item 13: bookmark the station's newest bubble)
   The wall map knows only a node id and, for an earlier stop, that stop's
   time. A bookmark needs a real turn (text + uid/segment/record) so it can
   later jump back. This picks it without a server change: the live segment
   for a head stop, the temporally nearest history surface otherwise. */

const T = (role, text, uid, seg, rec, time) =>
  ({ role, text, uid, segment: seg, record: rec, time });

test("stationLatestTurn with no stop time takes the newest live turn", () => {
  const live = [T("user", "hi", "u1", 0, 1), T("assistant", "hello", "u2", 0, 2)];
  assert.deepEqual(stationLatestTurn([], live, ""), live[1]);
  assert.deepEqual(stationLatestTurn([], live), live[1]);
});

test("stationLatestTurn ignores history when the head stop is asked for", () => {
  const segs = [{ start: "2026-08-01T10:00:00Z", turns: [T("assistant", "old", "o1", 0, 9)] }];
  const live = [T("assistant", "new", "n1", 1, 3)];
  assert.equal(stationLatestTurn(segs, live, "").uid, "n1");
});

test("stationLatestTurn picks the segment nearest the stop time, newest turn in it", () => {
  const segs = [
    { start: "2026-08-01T10:00:00Z", turns: [T("user", "a", "a1", 0, 1), T("assistant", "b", "a2", 0, 2)] },
    { start: "2026-08-02T10:00:00Z", turns: [T("user", "c", "b1", 1, 1), T("assistant", "d", "b2", 1, 2)] },
    { start: "2026-08-03T10:00:00Z", turns: [T("user", "e", "c1", 2, 1)] },
  ];
  assert.equal(stationLatestTurn(segs, [], "2026-08-02T10:00:00Z").uid, "b2");
  // nearest wins even when the stamp is not an exact segment start
  assert.equal(stationLatestTurn(segs, [], "2026-08-02T23:00:00Z").uid, "c1");
  assert.equal(stationLatestTurn(segs, [], "2026-08-01T02:00:00Z").uid, "a2");
});

test("stationLatestTurn skips surfaces that render no turns", () => {
  const segs = [
    { start: "2026-08-02T10:00:00Z", turns: [] },
    { start: "2026-08-01T10:00:00Z", turns: [T("assistant", "kept", "k1", 0, 4)] },
  ];
  assert.equal(stationLatestTurn(segs, [], "2026-08-02T10:00:00Z").uid, "k1");
});

test("stationLatestTurn falls back to the live segment when history is unusable", () => {
  const live = [T("assistant", "live", "l1", 0, 1)];
  assert.equal(stationLatestTurn([], live, "2026-08-02T10:00:00Z").uid, "l1");
  assert.equal(stationLatestTurn(null, live, "2026-08-02T10:00:00Z").uid, "l1");
  assert.equal(stationLatestTurn([{ start: "x", turns: [] }], live, "2026-08-02T10:00:00Z").uid, "l1");
});

test("stationLatestTurn returns null when there is nothing to bookmark", () => {
  assert.equal(stationLatestTurn([], [], ""), null);
  assert.equal(stationLatestTurn(null, null, "2026-08-02T10:00:00Z"), null);
});

test("stationLatestTurn tolerates an unparseable segment start", () => {
  const segs = [
    { start: "not-a-date", turns: [T("assistant", "junk", "j1", 0, 1)] },
    { start: "2026-08-02T10:00:00Z", turns: [T("assistant", "good", "g1", 1, 1)] },
  ];
  assert.equal(stationLatestTurn(segs, [], "2026-08-02T09:00:00Z").uid, "g1");
});
