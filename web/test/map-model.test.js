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
  orderedNodes,
  pinnedOrder,
  canReceiveSend,
  sendableNodes,
  isArchived,
  isPinned,
  tabListForCards,
  inLaneScope,
  visibleCardLists,
  headStopKey,
  toggleMapSelection,
  stationLatestTurn,
} from "../js/map-model.js";
/* Namespace import so P5 red tests can assert on statusKind / turnFinished
   before those names exist as named exports (a missing named import would
   abort the whole file at load time). */
import * as mapModel from "../js/map-model.js";
import { stopTimes, servedLanes, stopKey } from "../js/lanes.js";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));
const mapSrc = readFileSync(join(__dirname, "../js/map-model.js"), "utf8");
const statusKind = (...a) => mapModel.statusKind(...a);
const turnFinished = (...a) => mapModel.turnFinished(...a);

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
    "hardAttention", "cardState", "statusText", "statusKind", "turnFinished",
    "cardCreatedMS", "cardInteractionMS",
    "orderedNodes", "pinnedOrder", "canReceiveSend", "sendableNodes",
    "isArchived", "isPinned", "tabListForCards", "inLaneScope", "visibleCardLists",
    "headStopKey", "toggleMapSelection",
  ]) {
    assert.ok(name in mod, name);
  }
  assert.equal("livenessTier" in mod, false,
    "livenessTier is removed — ordering is hard attention then interaction recency");
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

/* ---------- P5: statusKind / turnFinished / finished tier ---------- */

/* statusKind and statusText share one precedence so the word and the class
   can never disagree. kind → expected text for every fixture below. */
const STATUS_KIND_TEXT = {
  closed: "Closed",
  inspect: "Quiet · check the terminal",
  attention: null, // depends on attention value
  running: "Running",
  finished: "Ready",
  exited: "Exited",
  unavailable: "Unavailable",
  quiet: "Quiet",
};

const STATUS_FIXTURES = [
  { name: "closed beats everything", n: { ended_at: "t", attention: "approval", live: "active", turn_done: true }, kind: "closed" },
  { name: "hard attention waiting", n: { attention: "approval", live: "quiet" }, kind: "attention", text: "Waiting · needs your approval" },
  { name: "question attention", n: { attention: "question", live: "quiet" }, kind: "attention", text: "Waiting · needs your question" },
  { name: "inspect attention", n: { attention: "inspect", live: "quiet" }, kind: "inspect" },
  { name: "running agent", n: { live: "active" }, kind: "running" },
  { name: "finished turn (item 10)", n: { live: "quiet", turn_done: true }, kind: "finished" },
  { name: "quiet but not delivered", n: { live: "quiet" }, kind: "quiet" },
  { name: "quiet-but-stale (no turn_done)", n: { live: "quiet", turn_done: false }, kind: "quiet" },
  { name: "crashed / exited", n: { live: "exited" }, kind: "exited" },
  { name: "unavailable", n: { live: "unavailable" }, kind: "unavailable" },
  { name: "ACP node: turn_done absent = unknown, not finished", n: { live: "quiet", agent: "pi" }, kind: "quiet" },
  { name: "ACP with no field stays quiet even if live quiet", n: { live: "quiet", agent: "grok" }, kind: "quiet" },
  { name: "finished suppressed while running", n: { live: "active", turn_done: true }, kind: "running" },
  { name: "finished suppressed by hard attention", n: { attention: "approval", live: "quiet", turn_done: true }, kind: "attention", text: "Waiting · needs your approval" },
  { name: "finished suppressed by closed", n: { ended_at: "t", turn_done: true, live: "quiet" }, kind: "closed" },
];

test("turnFinished is true only when turn_done is set", () => {
  assert.equal(turnFinished({ turn_done: true }), true);
  assert.equal(turnFinished({ turn_done: false }), false);
  assert.equal(turnFinished({}), false);
  assert.equal(turnFinished({ turn_done: 1 }), true);
  // ACP / unknown: field absent must mean UNKNOWN, never "not finished" as a
  // positive claim — but the predicate itself is false so status stays Quiet.
  assert.equal(turnFinished({ live: "quiet", agent: "pi" }), false);
});

test("statusKind and statusText agree across the fixture matrix", () => {
  for (const fx of STATUS_FIXTURES) {
    const kind = statusKind(fx.n);
    const text = statusText(fx.n);
    assert.equal(kind, fx.kind, `${fx.name}: kind`);
    const wantText = fx.text ?? STATUS_KIND_TEXT[fx.kind];
    assert.equal(text, wantText, `${fx.name}: text for kind ${kind}`);
    // kind is a stable key from the allowed set
    assert.ok(kind in STATUS_KIND_TEXT, `${fx.name}: unexpected kind ${kind}`);
  }
});

test("hardAttention is unchanged by turn_done (regression lock)", () => {
  // hardAttention is attention && attention !== "inspect" — turn_done must
  // never become a new attention value or change this predicate.
  assert.equal(!!hardAttention({}), false);
  assert.equal(!!hardAttention({ turn_done: true }), false);
  assert.equal(hardAttention({ attention: "inspect", turn_done: true }), false);
  assert.equal(hardAttention({ attention: "approval", turn_done: true }), true);
  assert.equal(hardAttention({ attention: "question" }), true);
  assert.equal(hardAttention({ attention: "approval" }), true);
});

/* ---------- orderedNodes: hard attention, then human recency (P2) ----------
   Precedence is exactly:
     1. hardAttention (never inspect)
     2. newest last_interaction
     3. newest last_activity
     4. newest created_at
   turn_done / active / quiet / exited / closed are NOT separate sort tiers. */

test("orderedNodes: hard attention beats a newer ordinary card", () => {
  const nodes = [
    { id: "working", live: "active", last_activity: 999, last_interaction: 999 },
    { id: "needs", attention: "approval", last_activity: 1, last_interaction: 1 },
  ];
  assert.deepEqual(orderedNodes(nodes).map(n => n.id), ["needs", "working"]);
});

test("orderedNodes: inspect does not get hard-attention promotion", () => {
  const nodes = [
    { id: "quiet-new", live: "quiet", last_interaction: 500 },
    { id: "inspect-old", attention: "inspect", live: "quiet", last_interaction: 100 },
  ];
  assert.deepEqual(orderedNodes(nodes).map(n => n.id), ["quiet-new", "inspect-old"],
    "inspect sorts by interaction recency, never the hard-attention bucket");
});

test("orderedNodes: newer active interaction beats older turn_done card", () => {
  /* The reported bug: a finished "Full Code Review" sat above an actively
     used implementer card because turn_done shared the top liveness tier. */
  const nodes = [
    { id: "full-review", live: "quiet", turn_done: true, last_interaction: 100, last_activity: 200 },
    { id: "implementer", live: "active", last_interaction: 500, last_activity: 510 },
  ];
  assert.deepEqual(orderedNodes(nodes).map(n => n.id), ["implementer", "full-review"]);
});

test("orderedNodes: newer quiet interaction beats older active card", () => {
  const nodes = [
    { id: "active-old", live: "active", last_interaction: 100, last_activity: 900 },
    { id: "quiet-new", live: "quiet", last_interaction: 500, last_activity: 500 },
  ];
  assert.deepEqual(orderedNodes(nodes).map(n => n.id), ["quiet-new", "active-old"]);
});

test("orderedNodes: dead and closed participate by interaction recency", () => {
  const nodes = [
    { id: "closed-new", ended_at: "t", last_interaction: 900 },
    { id: "dead-mid", live: "exited", last_interaction: 500 },
    { id: "idle-old", live: "quiet", last_interaction: 100 },
    { id: "unavail-new", live: "unavailable", last_interaction: 700 },
  ];
  assert.deepEqual(orderedNodes(nodes).map(n => n.id),
    ["closed-new", "unavail-new", "dead-mid", "idle-old"],
    "closed/dead are not sunk below idle by a liveness tier");
});

test("orderedNodes: interaction ties fall through to activity then created_at", () => {
  const nodes = [
    { id: "a", last_activity: 10, last_interaction: 100, created_at: "2026-01-01T00:00:00Z" },
    { id: "b", last_activity: 20, last_interaction: 100, created_at: "2026-01-02T00:00:00Z" },
  ];
  assert.deepEqual(orderedNodes(nodes).map(n => n.id), ["b", "a"]);

  const tied = [
    { id: "x", last_activity: 10, last_interaction: 100, created_at: "2026-01-01T00:00:00Z" },
    { id: "y", last_activity: 10, last_interaction: 100, created_at: "2026-01-03T00:00:00Z" },
  ];
  assert.deepEqual(orderedNodes(tied).map(n => n.id), ["y", "x"]);
});

test("orderedNodes: missing interaction falls back to created_at", () => {
  const nodes = [
    { id: "older-created", created_at: "2026-01-01T00:00:00Z" },
    { id: "newer-created", created_at: "2026-01-03T00:00:00Z" },
    { id: "has-interaction", last_interaction: Date.parse("2026-01-02T00:00:00Z"),
      created_at: "2026-01-01T00:00:00Z" },
  ];
  assert.deepEqual(orderedNodes(nodes).map(n => n.id),
    ["newer-created", "has-interaction", "older-created"]);
});

test("orderedNodes: does not mutate input array", () => {
  const nodes = [
    { id: "b", last_interaction: 1 },
    { id: "a", last_interaction: 2 },
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
  // Missing last_interaction falls back to parsed created_at (state API does
  // this for real nodes; synthetic fixtures need the same key).
  assert.equal(cardInteractionMS({ created_at: "2026-01-01T00:00:00.000Z" }),
    Date.parse("2026-01-01T00:00:00.000Z"));
  assert.equal(cardInteractionMS({ last_interaction: 42, created_at: "2026-01-01T00:00:00.000Z" }), 42);
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

/* ---------- send-to targets: who can receive a chat bubble ---------- */

test("canReceiveSend: only a live, open chat can receive", () => {
  assert.equal(canReceiveSend({ id: "a", live: "quiet" }), true);
  assert.equal(canReceiveSend({ id: "a", live: "active" }), true);
  assert.equal(canReceiveSend({ id: "a", attention: "approval", live: "quiet" }), true);
  assert.equal(canReceiveSend({ id: "a", live: "exited" }), false);
  assert.equal(canReceiveSend({ id: "a", live: "unavailable" }), false);
  assert.equal(canReceiveSend({ id: "a", live: "quiet", ended_at: "2026-08-01T00:00:00Z" }), false);
  /* a node with no poll state yet (fresh) is not provably dead — allow it */
  assert.equal(canReceiveSend({ id: "a" }), true);
});

test("sendableNodes: drops dead, closed, and the source chat itself", () => {
  const nodes = [
    { id: "self", live: "quiet" },
    { id: "ok", live: "active" },
    { id: "dead", live: "exited" },
    { id: "gone", live: "unavailable" },
    { id: "closed", live: "quiet", ended_at: "2026-08-01T00:00:00Z" },
  ];
  const g = sendableNodes(nodes, { exceptId: "self" });
  assert.deepEqual(g.pinned.map(n => n.id), []);
  assert.deepEqual(g.recent.map(n => n.id), ["ok"]);
});

test("sendableNodes: pinned block first, in the user's pin order", () => {
  const nodes = [
    { id: "a", live: "quiet", last_interaction: 10 },
    { id: "b", live: "quiet", last_interaction: 90 },
    { id: "c", live: "quiet", last_interaction: 50 },
  ];
  const g = sendableNodes(nodes, { pinned: ["c", "a"] });
  assert.deepEqual(g.pinned.map(n => n.id), ["c", "a"]);
  assert.deepEqual(g.recent.map(n => n.id), ["b"]);
});

test("sendableNodes: attention never reorders the picker under the finger", () => {
  /* pinnedOrder floats hard attention for the Pinned *tab* (triage). A
     destination picker must stay positionally stable, or the chat you were
     about to tap moves the instant an agent asks a question. */
  const nodes = [
    { id: "a", live: "quiet" },
    { id: "b", live: "quiet", attention: "approval" },
  ];
  assert.deepEqual(sendableNodes(nodes, { pinned: ["a", "b"] }).pinned.map(n => n.id), ["a", "b"]);
});

test("sendableNodes: unpinned targets sort by last human interaction, newest first", () => {
  const nodes = [
    { id: "old", live: "quiet", last_interaction: 10, last_activity: 999 },
    { id: "new", live: "quiet", last_interaction: 90, last_activity: 1 },
  ];
  /* last_interaction (last human turn), not last_activity (agent churn) */
  assert.deepEqual(sendableNodes(nodes, {}).recent.map(n => n.id), ["new", "old"]);
});

test("sendableNodes: no pins degenerates to pure recency (no second rule)", () => {
  const nodes = [
    { id: "a", live: "quiet", last_interaction: 1 },
    { id: "b", live: "quiet", last_interaction: 2 },
  ];
  const g = sendableNodes(nodes, { pinned: [] });
  assert.deepEqual(g.pinned, []);
  assert.deepEqual(g.recent.map(n => n.id), ["b", "a"]);
});

test("sendableNodes: a pinned chat that died is still excluded", () => {
  const nodes = [
    { id: "pin-dead", live: "exited" },
    { id: "pin-closed", live: "quiet", ended_at: "2026-08-01T00:00:00Z" },
    { id: "pin-ok", live: "quiet" },
  ];
  const g = sendableNodes(nodes, { pinned: ["pin-dead", "pin-closed", "pin-ok"] });
  assert.deepEqual(g.pinned.map(n => n.id), ["pin-ok"]);
  assert.deepEqual(g.recent, []);
});

test("sendableNodes: lane-agnostic — no lane filter is applied or accepted", () => {
  /* Same rule as the Pinned tab (tabListForCards skips lane scope): a target
     that vanishes on some lane filters would be inexplicable to the user. */
  const nodes = [
    { id: "a", live: "quiet", lane_id: "L1", last_interaction: 2 },
    { id: "b", live: "quiet", lane_id: "L2", last_interaction: 1 },
  ];
  const g = sendableNodes(nodes, { pinned: ["b"], laneFilter: "L1" });
  assert.deepEqual(g.pinned.map(n => n.id), ["b"]);
  assert.deepEqual(g.recent.map(n => n.id), ["a"]);
  assert.equal(/laneFilter|inLaneScope|servedLanes/.test(
    mapSrc.slice(mapSrc.indexOf("export function sendableNodes"),
      mapSrc.indexOf("export function sendableNodes") + 900)), false,
    "sendableNodes must not consult lane scope");
});

test("sendableNodes: pure — never mutates or aliases the input array", () => {
  const nodes = [
    { id: "a", live: "quiet", last_interaction: 1 },
    { id: "b", live: "quiet", last_interaction: 2 },
  ];
  const before = nodes.map(n => n.id);
  const g = sendableNodes(nodes, {});
  assert.deepEqual(nodes.map(n => n.id), before);
  assert.notEqual(g.recent, nodes);
  assert.deepEqual(sendableNodes(null, {}), { pinned: [], recent: [] });
  assert.deepEqual(sendableNodes([], undefined), { pinned: [], recent: [] });
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
