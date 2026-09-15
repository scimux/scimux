/* Characterization tests for web/js/cards.js — pure card decisions + edit
 * preservation, with a small fake-DOM adapter where event/render seams need it.
 * Ordering/status/visibility stay covered by map-model.test.js (not duplicated). */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import {
  CARD_TIME_SWAP_MS,
  cardConfigText,
  cardTimeItems,
  cardTimeTitle,
  cardTimeText,
  cardTimeHTML,
  cardMetaHTML,
  resolvePinnedTabFallback,
  pinnedLiveCount,
  computeCardsSignature,
  cardShapeSignature,
  packCardsSig,
  splitCardsSig,
  cardsRenderDecision,
  cardsRenderPlan,
  reorderPlan,
  permuteSlots,
  cardStatusHTML,
  CARD_STATES,
  cardTabCount,
  emptyCardsHTML,
  attnFoldButtonHTML,
  cardTabsHTML,
  pinOrderAfterDrag,
  cardSwipeOpensActions,
  cardEditKey,
  withCardEditsPreserved,
  createCardsFeature,
} from "../js/cards.js";
import { orderedNodes, hardAttention, visibleCardLists } from "../js/map-model.js";
import { md as sharedMd, esc as sharedEsc } from "../js/format.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const cardsSrc = readFileSync(join(__dirname, "../js/cards.js"), "utf8");
const cardsCssSrc = readFileSync(join(__dirname, "../css/cards.css"), "utf8");

/* ---------- purity of pure helpers (no browser side effects in pure path) ---------- */
test("cards.js pure helpers do not reference app globals for decisions", () => {
  // Factory may use document/setInterval via injection; pure exports must stay pure.
  assert.match(cardsSrc, /export function cardConfigText/);
  assert.match(cardsSrc, /export function computeCardsSignature/);
  assert.match(cardsSrc, /export function withCardEditsPreserved/);
  assert.match(cardsSrc, /export function createCardsFeature/);
  assert.match(cardsSrc, /from "\.\/format\.js"/);
  assert.match(cardsSrc, /from "\.\/map-model\.js"/);
  // Must not import later Phase 7 features
  assert.doesNotMatch(cardsSrc, /from "\.\/map\.js"/);
  assert.doesNotMatch(cardsSrc, /from "\.\/chat\.js"/);
  assert.doesNotMatch(cardsSrc, /from "\.\/polling\.js"/);
  assert.doesNotMatch(cardsSrc, /from "\.\/app\.js"/);
});

/* ---------- config / time / meta ---------- */
test("cardConfigText joins model and optional effort", () => {
  assert.equal(cardConfigText({}), "default");
  assert.equal(cardConfigText({ model: "gpt" }), "gpt");
  assert.equal(cardConfigText({ model: "gpt", effort: "high" }), "gpt/high");
});

test("cardTimeItems: you always; seen when alone or >9s after you", () => {
  assert.deepEqual(cardTimeItems({}), []);
  assert.deepEqual(cardTimeItems({ last_interaction: 100 }), [{ key: "you", ms: 100 }]);
  assert.deepEqual(cardTimeItems({ last_activity: 50 }), [{ key: "seen", ms: 50 }]);
  // seen within 9s of you → only you
  assert.deepEqual(cardTimeItems({ last_interaction: 100, last_activity: 105 }), [{ key: "you", ms: 100 }]);
  // seen > 9s after you → both
  assert.deepEqual(cardTimeItems({ last_interaction: 100, last_activity: 11000 }), [
    { key: "you", ms: 100 },
    { key: "seen", ms: 11000 },
  ]);
});

test("cardTimeTitle and cardTimeText flip rotation", () => {
  const age = ms => `A${ms}`;
  assert.equal(cardTimeTitle({ last_interaction: 1, last_activity: 2 }, age),
    "Last interaction A1 · last seen A2");
  assert.equal(cardTimeTitle({ last_interaction: 1 }, age), "Last interaction A1");
  assert.equal(cardTimeTitle({}, age), "Last seen");
  assert.equal(cardTimeText({}, 0, age), "");
  const n = { last_interaction: 10, last_activity: 100000 };
  assert.equal(cardTimeText(n, 0, age), "you A10");
  assert.equal(cardTimeText(n, 1, age), "seen A100000");
  assert.equal(cardTimeText(n, 2, age), "you A10");
});

test("cardTimeHTML empty vs titled span", () => {
  assert.equal(cardTimeHTML({}, 0), `<span class="time" title=""></span>`);
  const html = cardTimeHTML({ last_interaction: 1000 }, 0, {
    escape: s => `E(${s})`,
    ageFn: () => "1m",
  });
  assert.match(html, /class="time"/);
  assert.match(html, /E\(Last interaction 1m\)/);
  assert.match(html, /E\(you 1m\)/);
});

test("cardMetaHTML projects config · status · time", () => {
  const html = cardMetaHTML({ model: "m", effort: "e", live: "active" }, 0, {
    escape: s => s,
    ageFn: () => "x",
    statusText: () => "Running",
  });
  assert.equal(html, `m/e · running · <span class="time" title=""></span>`);
});

test("computeCardsSignature changes when turn_done flips; shape does not", () => {
  const base = {
    list: [{ id: "a", title: "T", description: "d", lane_id: "", ended_at: "", live: "quiet", attention: "", model: "m", effort: "", last_interaction: 1, turn_done: false }],
    foldList: [],
    unadopted: [],
    sel: "a",
    expanded: new Set(),
    cardTab: "current",
    laneFilter: "",
    attnFoldOpen: false,
    bookmarksLen: 0,
    lanes: [],
    pinned: [],
    actionCard: "",
    editingDesc: "",
    editingTitle: "",
  };
  const quiet = computeCardsSignature(base);
  const readyList = [{ ...base.list[0], turn_done: true }];
  const ready = computeCardsSignature({ ...base, list: readyList });
  assert.notEqual(quiet, ready, "full sig must include turn_done so Ready patches");
  assert.equal(
    cardShapeSignature(base),
    cardShapeSignature({ ...base, list: readyList }),
    "shape omits turn_done so the card is patched, not rebuilt",
  );
  assert.equal(cardsRenderPlan({
    sig: ready,
    cardsSig: quiet,
    shapeSig: cardShapeSignature({ ...base, list: readyList }),
    cardsShapeSig: cardShapeSignature(base),
    pinDragging: false,
  }), "patch");
});

test("cardStatusHTML paints a static readydot only when displayReady", () => {
  const n = { id: "n", model: "opus", live: "quiet", turn_done: true, last_activity: 10 };
  const html = cardStatusHTML(n, 0, { escape: s => s, ageFn: () => "2m" });
  assert.match(html, /class="readydot"/);
  assert.match(html, /title="Ready to continue"/);
  assert.match(html, /aria-hidden="true"/);
  assert.doesNotMatch(html, /class="workdot"/);
  assert.match(html, /class="st-finished">Ready</);
  assert.doesNotMatch(html, / · ready · /);

  const selected = cardStatusHTML(n, 0, {
    escape: s => s, ageFn: () => "2m", selectedId: "n",
  });
  assert.doesNotMatch(selected, /class="readydot"/);
  assert.match(selected, / · quiet · /);

  const seen = cardStatusHTML(n, 0, {
    escape: s => s, ageFn: () => "2m", seenAt: { n: 10 },
  });
  assert.doesNotMatch(seen, /class="readydot"/);
  assert.match(seen, / · quiet · /);

  const working = cardStatusHTML(
    { id: "n", model: "opus", live: "active", turn_done: true },
    0, { escape: s => s, ageFn: () => "2m" },
  );
  assert.match(working, /class="workdot"/);
  assert.doesNotMatch(working, /class="readydot"/);

  const quiet = cardStatusHTML(
    { id: "n", model: "opus", live: "quiet" },
    0, { escape: s => s, ageFn: () => "2m" },
  );
  assert.doesNotMatch(quiet, /class="readydot"/);
  assert.doesNotMatch(quiet, /class="workdot"/);
  assert.match(quiet, / · quiet · /);
});

test("CARD_STATES stays the five mechanical tokens (no finished glow class)", () => {
  assert.deepEqual(CARD_STATES, ["closed", "attention", "working", "dead", "idle"]);
});

test("cards.css: .readydot is a static petrol disc; no finished card glow", () => {
  const dot = cardsCssSrc.match(/\.readydot\s*\{([^}]+)\}/);
  assert.ok(dot, ".readydot rule must exist");
  assert.match(dot[1], /var\(--work\)/, "disc uses petrol");
  assert.match(dot[1], /border-radius/, "it is a disc");
  assert.doesNotMatch(dot[1], /animation\s*:/, "Ready disc must not pulse");
  const word = cardsCssSrc.match(/\.card\s+\.status\s+\.st-finished\s*\{([^}]+)\}/)
    || cardsCssSrc.match(/\.st-finished\s*\{([^}]+)\}/);
  assert.ok(word, ".st-finished colour rule on the card status line");
  assert.match(word[1], /var\(--work\)/, "Ready word uses petrol");
  assert.doesNotMatch(cardsCssSrc, /\.card\.finished/, "no finished card glow class");
  const attn = cardsCssSrc.match(/\.card\.attention::after\s*\{/);
  assert.ok(attn, "attention glow stays the only card halo");
});

/* ---------- signature / render decision ---------- */
test("computeCardsSignature equality for age-only; change forces rebuild", () => {
  const base = {
    list: [{ id: "a", title: "T", description: "d", lane_id: "", ended_at: "", live: "quiet", attention: "", model: "m", effort: "", last_interaction: 1 }],
    foldList: [],
    unadopted: [],
    sel: "a",
    expanded: new Set(),
    cardTab: "current",
    laneFilter: "",
    attnFoldOpen: false,
    bookmarksLen: 0,
    lanes: [],
    pinned: [],
    actionCard: "",
    editingDesc: "",
    editingTitle: "",
  };
  const s1 = computeCardsSignature(base);
  const s2 = computeCardsSignature(base);
  assert.equal(s1, s2);
  assert.equal(cardsRenderDecision(s1, s1, false), "ages");
  assert.equal(cardsRenderDecision(s1, s1, true), "skip");
  const s3 = computeCardsSignature({ ...base, list: [{ ...base.list[0], live: "active" }] });
  assert.notEqual(s1, s3);
  assert.equal(cardsRenderDecision(s3, s1, false), "rebuild");
  // last_interaction truthiness is in sig (not the timestamp value alone via !!)
  const s4 = computeCardsSignature({
    ...base,
    list: [{ ...base.list[0], last_interaction: 0 }],
  });
  assert.notEqual(s1, s4);
});

test("resolvePinnedTabFallback when last pin disappears", () => {
  assert.equal(resolvePinnedTabFallback("pinned", 0), "current");
  assert.equal(resolvePinnedTabFallback("pinned", 1), "pinned");
  assert.equal(resolvePinnedTabFallback("archived", 0), "archived");
});

test("pinnedLiveCount only counts ids present in nodes", () => {
  assert.equal(pinnedLiveCount(["a", "b", "ghost"], [{ id: "a" }, { id: "c" }]), 1);
  assert.equal(pinnedLiveCount([], [{ id: "a" }]), 0);
});

/* ---------- tabs HTML / counts / empty / adopt / fold ---------- */
test("cardTabsHTML: All/Pinned/Archived order and clear-scope ARIA", () => {
  const noPin = cardTabsHTML({
    cardTab: "current", laneFilter: "", laneColor: "", laneName: "",
    currentCount: 3, archivedCount: 1, pinnedCount: 0,
  });
  assert.match(noPin, /data-tab="current"/);
  assert.match(noPin, /data-tab="archived"/);
  assert.doesNotMatch(noPin, /data-tab="pinned"/);
  assert.match(noPin, />All</);

  const withPin = cardTabsHTML({
    cardTab: "pinned", laneFilter: "L1", laneColor: "#f00", laneName: "Road",
    currentCount: 2, archivedCount: 0, pinnedCount: 3,
  });
  const cur = withPin.indexOf('data-tab="current"');
  const pin = withPin.indexOf('data-tab="pinned"');
  const arch = withPin.indexOf('data-tab="archived"');
  assert.ok(cur < pin && pin < arch);
  assert.match(withPin, /Pinned \(3\)/);
  assert.match(withPin, /data-clear-scope/);
  assert.match(withPin, /aria-label="show all activities"/);
  assert.match(withPin, /Road \(2\)/);
});

test("cardTabCount mirrors visible membership for badges", () => {
  const nodes = [
    { id: "a", lane_id: "L1", attention: "approval" },
    { id: "b", lane_id: "L2" },
    { id: "c", lane_id: "L1" },
    { id: "d", lane_id: "L1", attention: "question" }, // archived + hard attention
  ];
  const archived = ["c", "d"];
  // current + lane L1: a (live) + d (archived hard-attention); b out of lane; c archived quiet
  assert.equal(cardTabCount(nodes, {
    archivedTab: false, laneFilter: "L1", selectedId: "", archived,
  }), 2);
  // archived tab (all lanes): c and d
  assert.equal(cardTabCount(nodes, {
    archivedTab: true, laneFilter: "", selectedId: "", archived,
  }), 2);
});

test("emptyCardsHTML messages", () => {
  assert.match(emptyCardsHTML({ laneFilter: "L1", cardTab: "current" }), /No activities on this journey/);
  assert.match(emptyCardsHTML({ laneFilter: "L1", cardTab: "archived" }), /No archived activities on this journey/);
  assert.match(emptyCardsHTML({ laneFilter: "", cardTab: "archived" }), /Nothing archived yet/);
  assert.match(emptyCardsHTML({ laneFilter: "", cardTab: "current" }), /start the first one/);
});

test("stale unadopted state cannot render an external-session card", () => {
  const h = cardsActionHarness({ nodes: [], unadopted: ["foreign-pane"] });
  h.feature.render();
  assert.doesNotMatch(h.list.innerHTML, /foreign-pane|data-adopt|adoptable/i);
  assert.match(h.list.innerHTML, /start the first one/);
});

test("attnFoldButtonHTML aria-expanded and open class", () => {
  assert.equal(attnFoldButtonHTML(0, false), "");
  const open = attnFoldButtonHTML(2, true);
  assert.match(open, /data-attnfold/);
  assert.match(open, /aria-expanded="true"/);
  assert.match(open, /attnfold open/);
  assert.match(open, /Input needed \(2\)/);
  const closed = attnFoldButtonHTML(1, false);
  assert.match(closed, /aria-expanded="false"/);
  assert.doesNotMatch(closed, /attnfold open/);
});

/* ---------- pin drag order / swipe ---------- */
test("pinOrderAfterDrag preserves DOM order and appends survivors", () => {
  assert.deepEqual(pinOrderAfterDrag(["b", "a"], ["a", "b", "c"]), ["b", "a", "c"]);
  assert.deepEqual(pinOrderAfterDrag(["a"], ["a"]), ["a"]);
  assert.deepEqual(pinOrderAfterDrag([], ["x"]), ["x"]);
});

test("cardSwipeOpensActions threshold matches live swipe", () => {
  assert.equal(cardSwipeOpensActions(-50, 10), true);
  assert.equal(cardSwipeOpensActions(-40, 0), false);
  assert.equal(cardSwipeOpensActions(-50, 40), false); // |dx| not > 1.6*|dy|
  assert.equal(cardSwipeOpensActions(50, 0), false);  // wrong direction
});

/* ---------- withCardEditsPreserved ---------- */
function fakeField({ key, value, type = "text", active = false, matchesLane = false, cardLane = "" }) {
  const dataset = {};
  if (key.startsWith("title:")) dataset.titleInput = key.slice(6);
  if (key.startsWith("desc:")) dataset.descInput = key.slice(5);
  if (key.startsWith("lanenew:")) dataset.laneNew = key.slice(8);
  if (key.startsWith("lanesel:")) { /* lane select via matches */ }
  const el = {
    value,
    type,
    dataset,
    selectionStart: 1,
    selectionEnd: 2,
    focused: false,
    setSelectionRange(a, b) { this.selectionStart = a; this.selectionEnd = b; this.ranged = [a, b]; },
    focus() { this.focused = true; },
    matches(sel) {
      if (sel === "[data-lane-select]") return matchesLane || key.startsWith("lanesel:");
      return false;
    },
    closest(sel) {
      if (sel === "[data-card-lane]") return { dataset: { cardLane: cardLane || key.slice(8) } };
      return null;
    },
  };
  return el;
}

test("cardEditKey: only active description shadows; inactive desc ignored", () => {
  const title = fakeField({ key: "title:n1", value: "T" });
  assert.equal(cardEditKey(title, ""), "title:n1");
  const descActive = { dataset: { descInput: "n1" }, value: "x" };
  assert.equal(cardEditKey(descActive, "n1"), "desc:n1");
  assert.equal(cardEditKey(descActive, "other"), "");
  assert.equal(cardEditKey(descActive, ""), "");
  const laneNew = { dataset: { laneNew: "n2" }, value: "Road" };
  assert.equal(cardEditKey(laneNew, ""), "lanenew:n2");
  const laneSel = fakeField({ key: "lanesel:n3", value: "L1", matchesLane: true, cardLane: "n3" });
  assert.equal(cardEditKey(laneSel, ""), "lanesel:n3");
});

test("withCardEditsPreserved restores title/desc/lane values, focus, selection, lane picker", () => {
  const title = fakeField({ key: "title:n1", value: "Draft", active: true });
  title.selectionStart = 2;
  title.selectionEnd = 4;
  const inactiveDesc = { dataset: { descInput: "n1" }, value: "stale" }; // not editing → no key
  const activeDesc = { dataset: { descInput: "n2" }, value: "typing" };
  const laneNew = { dataset: { laneNew: "n3" }, value: "NewLane" };
  const laneSel = fakeField({ key: "lanesel:n3", value: "__new", matchesLane: true, cardLane: "n3" });
  let fields = [title, inactiveDesc, activeDesc, laneNew, laneSel];
  const root = {
    querySelectorAll() { return fields; },
  };
  const doc = { activeElement: title };
  let synced = 0;
  withCardEditsPreserved(root, "n2", () => {
    // simulate rebuild with "fresh server" values
    title.value = "ServerTitle";
    inactiveDesc.value = "fresh-desc-from-server";
    activeDesc.value = "server-desc";
    laneNew.value = "";
    laneSel.value = "L1";
    fields = [title, inactiveDesc, activeDesc, laneNew, laneSel];
  }, {
    document: doc,
    syncLanePicker() { synced++; },
  });
  assert.equal(title.value, "Draft");
  assert.equal(title.focused, true);
  assert.deepEqual(title.ranged, [2, 4]);
  // inactive desc must NOT be restored — server value wins
  assert.equal(inactiveDesc.value, "fresh-desc-from-server");
  // active desc restored
  assert.equal(activeDesc.value, "typing");
  assert.equal(laneNew.value, "NewLane");
  assert.equal(laneSel.value, "__new");
  assert.ok(synced >= 1);
});

/* ---------- factory smoke with fake roots ---------- */
test("createCardsFeature: skip mid-drag; ages on equal sig; rebuild on change", () => {
  let cardsSig = "";
  let cardTab = "current";
  let attnFoldOpen = false;
  let actionCard = "";
  let editingDesc = "";
  let editingTitle = "";
  const nodes = [
    { id: "a", title: "A", description: "", lane_id: "", live: "quiet", model: "m", last_interaction: 1 },
  ];
  const list = {
    innerHTML: "",
    querySelectorAll() { return []; },
    addEventListener() {},
    removeEventListener() {},
  };
  const tabs = {
    innerHTML: "",
    querySelectorAll() { return []; },
    addEventListener() {},
    removeEventListener() {},
  };
  const doc = {
    title: "",
    getElementById() { return null; },
    querySelector() { return null; },
  };
  const feature = createCardsFeature({
    roots: { tabs, list },
    document: doc,
    nodes: () => nodes,
    unadopted: () => [],
    sel: () => "a",
    cardTab: () => cardTab,
    setCardTab: v => { cardTab = v; },
    laneFilter: () => "",
    attnFoldOpen: () => attnFoldOpen,
    setAttnFoldOpen: v => { attnFoldOpen = v; },
    expanded: () => new Set(),
    actionCard: () => actionCard,
    setActionCard: v => { actionCard = v; },
    editingDesc: () => editingDesc,
    setEditingDesc: v => { editingDesc = v; },
    editingTitle: () => editingTitle,
    editingTitleScope: () => "",
    cardsSig: () => cardsSig,
    setCardsSig: v => { cardsSig = v; },
    pinned: () => ["a"],
    archived: () => [],
    lanes: () => [],
    bookmarks: () => [],
    agentLogo: () => "",
    laneSelectHTML: () => "<select data-lane-select></select>",
    laneColor: () => "#000",
    laneName: () => "",
    icons: { ICON_CHECK: "C", ICON_PIN: "P", ICON_UNPIN: "U", ICON_END: "E", ICON_ARCHIVE: "A", ICON_RESTORE: "R", ICON_TRASH: "T" },
    CSS: { escape: s => s },
    setInterval: () => 0,
    clearInterval: () => {},
  });
  feature.render();
  assert.match(list.innerHTML, /id="card-a"/);
  assert.match(list.innerHTML, /data-pin-action="a"/);
  assert.match(list.innerHTML, /class="pinflag"/);
  assert.match(list.innerHTML, /aria-label="unpin"/);
  const pinAction = list.innerHTML.indexOf("data-pin-action");
  const archiveAction = list.innerHTML.indexOf("data-arch-action");
  const trashAction = list.innerHTML.indexOf("data-trash");
  assert.ok(pinAction > 0 && pinAction < archiveAction && archiveAction < trashAction);
  assert.match(tabs.innerHTML, /data-tab="current"/);
  const firstHTML = list.innerHTML;
  const firstSig = cardsSig;
  assert.ok(firstSig);
  // equal sig → ages only (innerHTML unchanged)
  feature.render();
  assert.equal(list.innerHTML, firstHTML);
  assert.equal(cardsSig, firstSig);
  // structural change → rebuild
  nodes[0].title = "B";
  feature.render();
  assert.match(list.innerHTML, />B</);
  assert.notEqual(cardsSig, firstSig);
});

test("createCardsFeature: pinned fallback when last pin gone", () => {
  let cardsSig = "";
  let cardTab = "pinned";
  const list = { innerHTML: "", querySelectorAll: () => [], addEventListener() {}, removeEventListener() {} };
  const tabs = { innerHTML: "", querySelectorAll: () => [], addEventListener() {}, removeEventListener() {} };
  const feature = createCardsFeature({
    roots: { tabs, list },
    document: { title: "", getElementById: () => null },
    nodes: () => [{ id: "a", title: "A", live: "quiet" }],
    unadopted: () => [],
    sel: () => "",
    cardTab: () => cardTab,
    setCardTab: v => { cardTab = v; },
    laneFilter: () => "",
    attnFoldOpen: () => false,
    setAttnFoldOpen: () => {},
    expanded: () => new Set(),
    actionCard: () => "",
    setActionCard: () => {},
    editingDesc: () => "",
    setEditingDesc: () => {},
    editingTitle: () => "",
    editingTitleScope: () => "",
    cardsSig: () => cardsSig,
    setCardsSig: v => { cardsSig = v; },
    pinned: () => [], // no pins
    archived: () => [],
    lanes: () => [],
    bookmarks: () => [],
    agentLogo: () => "",
    laneSelectHTML: () => "",
    laneColor: () => "",
    laneName: () => "",
    icons: {},
    CSS: { escape: s => s },
    setInterval: () => 0,
    clearInterval: () => {},
  });
  feature.render();
  assert.equal(cardTab, "current");
  assert.doesNotMatch(tabs.innerHTML, /data-tab="pinned"/);
});

test("createCardsFeature: bind is singular; drag skips rebuild; destroy removes owned listeners", () => {
  let cardsSig = "";
  let cardTab = "pinned";
  const nodes = [{ id: "a", title: "A", live: "quiet" }];
  const handlers = { list: new Map(), tabs: new Map() };
  const removed = [];
  function eventRoot(kind){
    return {
      innerHTML: "",
      querySelectorAll: () => [],
      addEventListener(type, fn) {
        assert.equal(handlers[kind].has(type), false, `duplicate ${kind} ${type} listener`);
        handlers[kind].set(type, fn);
      },
      removeEventListener(type, fn) {
        assert.equal(handlers[kind].get(type), fn);
        handlers[kind].delete(type);
        removed.push(`${kind}:${type}`);
      },
    };
  }
  const list = eventRoot("list");
  const tabs = eventRoot("tabs");
  let intervalFn = null;
  let clearedTimer = null;
  let longpressBound = 0;
  let longpressCleaned = 0;
  const feature = createCardsFeature({
    roots: { tabs, list },
    document: { title: "", getElementById: () => null },
    nodes: () => nodes,
    unadopted: () => [],
    sel: () => "",
    cardTab: () => cardTab,
    setCardTab: v => { cardTab = v; },
    laneFilter: () => "",
    attnFoldOpen: () => false,
    setAttnFoldOpen: () => {},
    expanded: () => new Set(),
    actionCard: () => "",
    setActionCard: () => {},
    editingDesc: () => "",
    setEditingDesc: () => {},
    editingTitle: () => "",
    editingTitleScope: () => "",
    cardsSig: () => cardsSig,
    setCardsSig: v => { cardsSig = v; },
    pinned: () => ["a"],
    archived: () => [],
    lanes: () => [],
    bookmarks: () => [],
    agentLogo: () => "",
    laneSelectHTML: () => "",
    laneColor: () => "",
    laneName: () => "",
    icons: {},
    CSS: { escape: s => s },
    setInterval(fn) { intervalFn = fn; return 42; },
    clearInterval(id) { clearedTimer = id; },
    longpress() {
      longpressBound++;
      return () => { longpressCleaned++; };
    },
    uiMutate: () => {},
  });
  feature.render();
  assert.match(list.innerHTML, />A</);
  feature.bind();
  feature.bind(); // idempotent: never duplicate root or longpress listeners
  assert.equal(handlers.tabs.size, 1);
  assert.equal(handlers.list.size, 7);
  assert.equal(longpressBound, 3);
  assert.equal(typeof intervalFn, "function");

  const classes = new Set();
  const card = {
    classList: {
      contains: c => classes.has(c),
      add: c => classes.add(c),
      remove: c => classes.delete(c),
    },
    closest: sel => sel === ".card" ? card : null,
    querySelector: sel => sel === ".go" ? { dataset: { open: "a" } } : null,
  };
  handlers.list.get("dragstart")({
    target: card,
    dataTransfer: { setData() {}, effectAllowed: "" },
  });
  assert.equal(classes.has("dragging"), true);
  const before = list.innerHTML;
  nodes[0].title = "B";
  feature.render();
  assert.equal(list.innerHTML, before, "poll render rebuilt the list during drag");

  feature.destroy();
  assert.equal(handlers.tabs.size, 0);
  assert.equal(handlers.list.size, 0);
  assert.equal(removed.length, 8);
  assert.equal(longpressCleaned, 3);
  assert.equal(clearedTimer, 42);
});

test("CARD_TIME_SWAP_MS is 30s; drag implementation stays single-owned", () => {
  assert.equal(CARD_TIME_SWAP_MS, 30000);
  assert.match(cardsSrc, /if \(pinDragging\) return/);
  assert.match(cardsSrc, /k: "pin-order"/);
  assert.match(cardsSrc, /draggable="true"/);
});

test("archived hard-attention still surfaces via map-model visibleCardLists (not reimplemented)", () => {
  // Guard: cards.js must call visibleCardLists, not re-order itself
  assert.match(cardsSrc, /visibleCardLists/);
  assert.doesNotMatch(cardsSrc.replace(/from "\.\/map-model\.js"/, ""), /function orderedNodes/);
  const nodes = [
    { id: "live", live: "quiet" },
    { id: "arch-attn", attention: "question", live: "quiet" },
  ];
  const { list } = visibleCardLists(nodes, {
    cardTab: "current",
    archived: ["arch-attn"],
    pinned: [],
    laneFilter: "",
    selectedId: "",
  });
  assert.deepEqual(list.map(n => n.id).sort(), ["arch-attn", "live"]);
});

test("ordering remains map-model responsibility (hard attention → interaction)", () => {
  // Do not re-test full matrix; one smoke that cards imports orderedNodes for counts only
  assert.match(cardsSrc, /orderedNodes/);
  const ids = orderedNodes([
    { id: "idle", last_interaction: 900 },
    { id: "attn", attention: "approval", last_interaction: 1 },
  ]).map(n => n.id);
  assert.equal(ids[0], "attn");
  assert.equal(hardAttention({ attention: "approval" }), true);
});

/* ---------- the 304 path has to actually be cheap ----------
 * updateCardAges runs on both poll branches, including the ETag 304
 * short-circuit whose whole purpose is to make an unchanged fleet nearly
 * free. It looped every node in the fleet — CSS.escape, a regex, and two
 * getElementById each — to reach the handful of cards actually rendered,
 * and rewrote every card's title attribute unconditionally. */

function agedNode(id, over){
  return { id, title: id, live: "quiet", last_interaction: 1000, last_activity: 1000, ...over };
}

function renderedCard(id){
  const time = {
    textContent: "", _title: "", titleWrites: 0,
    get title(){ return this._title; },
    set title(v){ this.titleWrites++; this._title = v; },
    classList: { remove(){}, add(){} },
    offsetWidth: 0,
  };
  return { id: "card-" + id, _time: time, querySelector: s => (s === ".time" ? time : null) };
}

test("updateAges is proportional to the rendered cards, not to the fleet", () => {
  const cards = [renderedCard("n7"), renderedCard("n42")];
  let byIdCalls = 0;
  const list = {
    innerHTML: "", addEventListener(){}, removeEventListener(){},
    querySelectorAll(sel){ return sel === ".card" ? cards : []; },
  };
  const nodes = Array.from({ length: 120 }, (_, i) => agedNode("n" + i));
  const feature = createCardsFeature({
    roots: { tabs: { innerHTML: "", querySelectorAll: () => [], addEventListener(){}, removeEventListener(){} }, list },
    document: { title: "", getElementById(){ byIdCalls++; return null; }, querySelector: () => null },
    nodes: () => nodes,
    unadopted: () => [], sel: () => "", cardTab: () => "all", setCardTab(){},
    laneFilter: () => "", attnFoldOpen: () => false, setAttnFoldOpen(){},
    expanded: () => new Set(), actionCard: () => "", setActionCard(){},
    editingDesc: () => "", setEditingDesc(){}, editingTitle: () => "",
    editingTitleScope: () => "", cardsSig: () => "", setCardsSig(){},
    pinned: () => [], archived: () => [], lanes: () => [], bookmarks: () => [],
    agentLogo: () => "", laneSelectHTML: () => "", laneColor: () => "#000", laneName: () => "",
    icons: {}, ageText: ms => "T" + ms,
  });

  feature.updateAges();
  assert.equal(byIdCalls, 0,
    "the DOM already knows which cards exist — walking the fleet to find them is the bug");
  assert.equal(cards[0]._time.textContent, "you T1000");
  assert.equal(cards[1]._time.textContent, "you T1000");
  assert.equal(cards[0]._time.title, "Last interaction T1000 · last seen T1000");

  const writes = cards[0]._time.titleWrites;
  assert.ok(writes > 0, "the first pass must set the title");
  feature.updateAges();
  feature.updateAges();
  assert.equal(cards[0]._time.titleWrites, writes,
    "an unchanged title must not be rewritten on every poll");
  assert.equal(byIdCalls, 0);
});

test("updateAges still updates a card whose age text moved on", () => {
  const cards = [renderedCard("a")];
  let clock = 1000;
  const list = {
    innerHTML: "", addEventListener(){}, removeEventListener(){},
    querySelectorAll(sel){ return sel === ".card" ? cards : []; },
  };
  const node = agedNode("a");
  const feature = createCardsFeature({
    roots: { tabs: { innerHTML: "", querySelectorAll: () => [], addEventListener(){}, removeEventListener(){} }, list },
    document: { title: "", getElementById: () => null, querySelector: () => null },
    nodes: () => [node],
    unadopted: () => [], sel: () => "", cardTab: () => "all", setCardTab(){},
    laneFilter: () => "", attnFoldOpen: () => false, setAttnFoldOpen(){},
    expanded: () => new Set(), actionCard: () => "", setActionCard(){},
    editingDesc: () => "", setEditingDesc(){}, editingTitle: () => "",
    editingTitleScope: () => "", cardsSig: () => "", setCardsSig(){},
    pinned: () => [], archived: () => [], lanes: () => [], bookmarks: () => [],
    agentLogo: () => "", laneSelectHTML: () => "", laneColor: () => "#000", laneName: () => "",
    icons: {}, ageText: () => "T" + clock,
  });
  feature.updateAges();
  assert.equal(cards[0]._time.textContent, "you T1000");
  const titleWrites = cards[0]._time.titleWrites;
  clock = 2000;
  feature.updateAges();
  assert.equal(cards[0]._time.textContent, "you T2000", "a moved age must still land");
  assert.ok(cards[0]._time.titleWrites > titleWrites, "and so must a moved title");
});

test("the attention card pulses a static shadow's opacity, not the shadow", () => {
  /* .card.attention::after is already a dedicated overlay layer, so the glow
     can sit still and the layer can fade. Animating the box-shadow itself
     repaints a blurred ring every frame for as long as the node asks. */
  const layer = cardsCssSrc.match(/\.card\.attention::after\s*\{([^}]+)\}/);
  assert.ok(layer, ".card.attention::after must still be the overlay");
  assert.match(layer[1], /box-shadow:/, "the glow lives on the overlay");
  assert.match(layer[1], /animation:\s*attentionPulse/);
  assert.doesNotMatch(layer[1], /will-change:[^;]*box-shadow/,
    "will-change cannot composite a box-shadow — the hint only costs memory");
  const kf = cardsCssSrc.match(/@keyframes\s+attentionPulse\s*\{([\s\S]*?)\n\}/);
  assert.ok(kf, "@keyframes attentionPulse must still exist");
  assert.doesNotMatch(kf[1], /box-shadow/, "the pulse must be opacity-only");
  assert.match(kf[1], /opacity/, "and it must still pulse");
});

/* ---------- A: a liveness flip patches the list, it does not rebuild it ---------- */

test("cardShapeSignature ignores liveness and card order", () => {
  /* live flips on the 2s poll for every working agent. It updates the status
     line in place but no longer reorders the list (order is hard attention +
     last_interaction). Folding live into the shape signature would still
     rebuild the list from scratch for as long as anything is running. */
  const base = { unadopted: [], sel: "a", expanded: new Set(), cardTab: "current",
    laneFilter: "", attnFoldOpen: false, bookmarksLen: 0, lanes: [], pinned: [],
    actionCard: "", editingDesc: "", editingTitle: "" };
  const a = { id: "a", title: "A", description: "d", lane_id: "l", live: "quiet", attention: "", model: "m" };
  const b = { id: "b", title: "B", description: "e", lane_id: "l", live: "quiet", attention: "", model: "m" };
  const sig = l => cardShapeSignature({ ...base, list: l, foldList: [] });

  assert.equal(sig([a, b]), sig([{ ...a, live: "active" }, b]), "liveness is not shape");
  assert.equal(sig([a, b]), sig([b, a]), "order is not shape");

  assert.notEqual(sig([a, b]), sig([{ ...a, title: "A2" }, b]), "a retitle is shape");
  assert.notEqual(sig([a, b]), sig([{ ...a, attention: "approval" }, b]),
    "attention is shape — it moves a card between the list and the fold");
  assert.notEqual(sig([a, b]), sig([{ ...a, ended_at: "t" }, b]), "closing is shape");
  assert.notEqual(sig([a, b]), sig([a]), "losing a card is shape");
  assert.notEqual(sig([a, b]), cardShapeSignature({ ...base, list: [a, b], foldList: [], sel: "b" }),
    "selection is shape");

  /* The full signature must still see everything the shape one drops, or the
     patch would never be triggered. */
  const full = l => computeCardsSignature({ ...base, list: l, foldList: [] });
  assert.notEqual(full([a, b]), full([{ ...a, live: "active" }, b]));
  assert.notEqual(full([a, b]), full([b, a]));
});

test("packCardsSig round-trips both halves through the one app-level slot", () => {
  /* app.js clears cardsSig directly in a dozen places. Carrying the shape half
     inside the same string is what makes those clears invalidate both. */
  const packed = packCardsSig("SHAPE", "FULL");
  assert.deepEqual(splitCardsSig(packed), { shape: "SHAPE", full: "FULL" });
  assert.deepEqual(splitCardsSig(""), { shape: "", full: "" },
    "a cleared slot must read as no shape and no full — never as a match");
  assert.deepEqual(splitCardsSig("legacy-unpacked"), { shape: "", full: "legacy-unpacked" },
    "an unpacked value has no shape half — it must never be mistaken for one");
  assert.notEqual(splitCardsSig("").shape, splitCardsSig(packed).shape);
});

test("cardsRenderPlan: skip, then ages, then patch, then rebuild", () => {
  const P = cardsRenderPlan;
  assert.equal(P({ sig: "s", cardsSig: "s", shapeSig: "h", cardsShapeSig: "h", pinDragging: true }), "skip");
  assert.equal(P({ sig: "s", cardsSig: "s", shapeSig: "h", cardsShapeSig: "h" }), "ages");
  assert.equal(P({ sig: "s2", cardsSig: "s", shapeSig: "h", cardsShapeSig: "h" }), "patch");
  assert.equal(P({ sig: "s2", cardsSig: "s", shapeSig: "h2", cardsShapeSig: "h" }), "rebuild");
  assert.equal(P({ sig: "s", cardsSig: "", shapeSig: "", cardsShapeSig: "" }), "rebuild",
    "an empty shape must never match an empty stored shape — nothing is in the DOM yet");
});

test("reorderPlan emits no move when the order already holds", () => {
  assert.deepEqual(reorderPlan(["a", "b", "c"], ["a", "b", "c"]), []);
  assert.deepEqual(reorderPlan([], []), []);
});

test("reorderPlan lifts one card to the front with a single insertBefore", () => {
  /* The common case: hard attention or a newer interaction promotes a card. */
  const plan = reorderPlan(["a", "b", "c", "d"], ["c", "a", "b", "d"]);
  assert.deepEqual(plan, [{ key: "c", before: "a" }]);
});

test("reorderPlan tolerates the id-less adoptable cards sharing a key", () => {
  /* Adoptable tmux sessions render as .card with no id, so several children
     can key as "". permuteSlots never moves them, so they hold the same slots
     in both lists — and the planner must read that as "already in place"
     rather than shuffling one arbitrary twin. */
  const plan = reorderPlan(["", "", "card-a", "card-b"], ["", "", "card-b", "card-a"]);
  assert.deepEqual(plan, [{ key: "card-b", before: "card-a" }]);
});

test("reorderPlan's moves actually produce the desired order", () => {
  const apply = (current, plan) => {
    const cur = current.slice();
    for (const { key, before } of plan){
      const at = cur.indexOf(key);
      if (at >= 0) cur.splice(at, 1);
      const b = before == null ? -1 : cur.indexOf(before);
      cur.splice(b < 0 ? cur.length : b, 0, key);
    }
    return cur;
  };
  const perms = [
    [["a", "b", "c"], ["c", "b", "a"]],
    [["a", "b", "c"], ["b", "c", "a"]],
    [["a", "b", "c", "d", "e"], ["e", "d", "a", "c", "b"]],
    [["a"], ["a"]],
    [["a", "b"], ["b", "a"]],
  ];
  for (const [cur, want] of perms){
    const plan = reorderPlan(cur, want);
    assert.deepEqual(apply(cur, plan), want, `plan for ${want.join("")} is wrong`);
    assert.ok(plan.length <= want.length, "a plan may never exceed one move per key");
  }
});

test("permuteSlots reorders one block and leaves every other child alone", () => {
  /* #cardlist also holds the attention-fold button, adoptable sessions and the
     empty state. Only the card blocks are tiered, so only their slots move. */
  const current = ["attnfold", "card-a", "card-b", "adopt-x", "card-c"];
  assert.deepEqual(permuteSlots(current, ["card-b", "card-a"]),
    ["attnfold", "card-b", "card-a", "adopt-x", "card-c"]);
  assert.deepEqual(permuteSlots(current, []), current, "an empty block changes nothing");
  /* A block that does not line up with its slots would otherwise write a key
     into a slot that is not its own — inventing a child that is not there, or
     dropping one that is. */
  assert.deepEqual(permuteSlots(["x", "card-a", "card-b"], ["card-zz", "card-a"]),
    ["x", "card-a", "card-b"],
    "a member that is not in the DOM must not displace one that is");
  assert.deepEqual(permuteSlots(["x", "card-a", "card-b"], ["card-b"]),
    ["x", "card-a", "card-b"],
    "a block shorter than its slots must not leave a hole");
});

test("cardStatusHTML is the one source the build and the patch share", () => {
  const deps = { escape: s => String(s), ageFn: ms => `A${ms}` };
  const working = { id: "a", title: "A", live: "active", model: "m", last_interaction: 5 };
  const idle = { ...working, live: "quiet" };
  assert.match(cardStatusHTML(working, 0, deps), /^<span class="workdot"><\/span>/,
    "a working card carries the pulse dot");
  assert.doesNotMatch(cardStatusHTML(idle, 0, deps), /workdot/);
  assert.match(cardStatusHTML(idle, 0, deps), /<span class="st">.*quiet.*<\/span>/);
  assert.match(cardsSrc, /<div class="status">\$\{cardStatusHTML\(/,
    "cardHTML must emit exactly what the patch writes back");
  assert.ok(CARD_STATES.includes("working") && CARD_STATES.includes("idle") &&
    CARD_STATES.includes("attention") && CARD_STATES.includes("closed") &&
    CARD_STATES.includes("dead"), "every cardState token must be swappable off the card");
});

/* A fake #cardlist that models the parts a patch touches: children in order,
   a classList per card, a .status and a .time, and insertBefore as a move. */
function fakeCardList(ids, leadIds = []){
  const mk = id => {
    const classes = new Set(["card", "idle"]);
    const status = { innerHTML: "" };
    const time = { textContent: "", title: "" };
    return {
      id: "card-" + id,
      classList: {
        add: (...c) => c.forEach(x => classes.add(x)),
        remove: (...c) => c.forEach(x => classes.delete(x)),
        contains: c => classes.has(c),
      },
      querySelector: sel => sel === ".status" ? status : sel === ".time" ? time : null,
      _classes: classes, _status: status, _time: time,
    };
  };
  /* Children that are not cards — the attention-fold button, an adoptable
     session, the empty state. They must come back out where they went in. */
  const leads = leadIds.map(id => ({ id, querySelector: () => null }));
  const children = leads.concat(ids.map(mk));
  const moves = [];
  return {
    innerHTML: "",
    children,
    moves,
    querySelectorAll(sel){
      return sel === ".card" ? children.filter(c => !leads.includes(c)) : [];
    },
    insertBefore(el, before){
      moves.push([el.id, before ? before.id : null]);
      const at = children.indexOf(el);
      if (at >= 0) children.splice(at, 1);
      const b = before ? children.indexOf(before) : -1;
      children.splice(b < 0 ? children.length : b, 0, el);
      return el;
    },
    appendChild(el){ return this.insertBefore(el, null); },
    addEventListener(){}, removeEventListener(){},
  };
}

function patchFeature(nodes, list, opts = {}){
  let cardsSig = "", cardTab = "current";
  let attnFoldOpen = !!opts.attnFoldOpen;
  return createCardsFeature({
    roots: { tabs: { innerHTML: "", querySelectorAll: () => [], addEventListener(){}, removeEventListener(){} }, list },
    document: { title: "", getElementById: () => null, querySelector: () => null },
    nodes: () => nodes,
    unadopted: () => [],
    sel: () => "",
    cardTab: () => cardTab,
    setCardTab: v => { cardTab = v; },
    laneFilter: () => opts.laneFilter || "",
    attnFoldOpen: () => attnFoldOpen,
    setAttnFoldOpen: v => { attnFoldOpen = v; },
    expanded: () => new Set(),
    actionCard: () => "",
    setActionCard: () => {},
    editingDesc: () => "",
    setEditingDesc: () => {},
    editingTitle: () => "",
    editingTitleScope: () => "",
    cardsSig: () => cardsSig,
    setCardsSig: v => { cardsSig = v; },
    pinned: () => [],
    archived: () => [],
    lanes: () => [],
    bookmarks: () => [],
    agentLogo: () => "",
    laneSelectHTML: () => "",
    laneColor: () => "#000",
    laneName: () => "",
    icons: {},
    CSS: { escape: s => s },
    ageText: ms => `A${ms}`,
    setInterval: () => 0,
    clearInterval: () => {},
  });
}

test("a liveness flip patches the cards in place instead of rebuilding them", () => {
  const nodes = [
    { id: "a", title: "A", description: "", lane_id: "l", live: "quiet", model: "m", last_interaction: 3, last_activity: 3 },
    { id: "b", title: "B", description: "", lane_id: "l", live: "quiet", model: "m", last_interaction: 2, last_activity: 2 },
  ];
  const list = fakeCardList([]);
  const feature = patchFeature(nodes, list);

  feature.render();
  const built = list.innerHTML;
  assert.match(built, /id="card-a"/, "the first render still builds the list");

  /* Hand the feature the DOM that render would have produced, with one child
     that is not a card sitting in front of them. */
  const dom = fakeCardList(["a", "b"], ["attnfold"]);
  Object.defineProperty(list, "children", { get: () => dom.children, configurable: true });
  list.querySelectorAll = sel => dom.querySelectorAll(sel);
  list.insertBefore = (el, before) => dom.insertBefore(el, before);
  let writes = 0;
  Object.defineProperty(list, "innerHTML", {
    configurable: true,
    get(){ return built; },
    set(){ writes++; },
  });

  nodes[1].live = "active";
  feature.render();

  assert.equal(writes, 0, "a liveness flip must not rewrite #cardlist");
  const byId = Object.fromEntries(dom.children.map(c => [c.id, c]));
  assert.ok(byId["card-b"]._classes.has("working"), "the started card takes the working class");
  assert.ok(!byId["card-b"]._classes.has("idle"), "and loses the state it had");
  assert.ok(byId["card-a"]._classes.has("idle"), "the quiet card keeps its own");
  assert.match(byId["card-b"]._status.innerHTML, /workdot/, "and its status line is repainted");
  assert.match(byId["card-b"]._status.innerHTML, /running/);
  /* live is not an ordering field — interaction recency still ranks a above b.
     Status updates in place; the list order is unchanged. */
  assert.deepEqual(dom.children.map(c => c.id), ["attnfold", "card-a", "card-b"],
    "a mechanical liveness change must not reorder cards");
  assert.deepEqual(dom.moves, [], "no insertBefore when order is unchanged");
  assert.equal(byId["card-b"]._time.textContent, "you A2",
    "the patch still owes the list its age refresh");
});

test("the attention fold is its own tiered block and reorders on its own", () => {
  /* With a journey filter on, out-of-scope cards that need input ride in the
     fold above the list. Both blocks are ordered, both can move under a patch,
     and neither may reach into the other's slots. */
  const nodes = [
    { id: "in", title: "In", description: "", lane_id: "l1", live: "quiet", model: "m", last_interaction: 9 },
    { id: "f1", title: "F1", description: "", lane_id: "l2", live: "quiet", attention: "approval", model: "m", last_interaction: 5, last_activity: 5 },
    { id: "f2", title: "F2", description: "", lane_id: "l2", live: "quiet", attention: "approval", model: "m", last_interaction: 4, last_activity: 4 },
  ];
  const list = fakeCardList([]);
  const feature = patchFeature(nodes, list, { laneFilter: "l1", attnFoldOpen: true });
  feature.render();
  assert.match(list.innerHTML, /data-attnfold/, "the fold button is built");

  const dom = fakeCardList(["f1", "f2", "in"], []);
  dom.children.splice(0, 0, { id: "attnfold", querySelector: () => null });
  Object.defineProperty(list, "children", { get: () => dom.children, configurable: true });
  list.querySelectorAll = sel => (sel === ".card" ? dom.children.filter(c => c.id !== "attnfold") : []);
  list.insertBefore = (el, before) => dom.insertBefore(el, before);
  let writes = 0;
  const built = list.innerHTML;
  Object.defineProperty(list, "innerHTML", {
    configurable: true, get(){ return built; }, set(){ writes++; },
  });

  /* f2 answers more recently than f1 — same tier, so recency reorders them. */
  nodes[2].last_interaction = 7;
  feature.render();

  assert.equal(writes, 0, "recency inside a tier is not a shape change");
  assert.deepEqual(dom.children.map(c => c.id), ["attnfold", "card-f2", "card-f1", "card-in"],
    "the fold block reorders and the main card keeps its slot");
});

test("a shape change still rebuilds, and a settled fleet still only ages", () => {
  const nodes = [
    { id: "a", title: "A", description: "", lane_id: "l", live: "quiet", model: "m", last_interaction: 3 },
  ];
  const list = fakeCardList([]);
  const feature = patchFeature(nodes, list);
  feature.render();

  let writes = 0;
  const built = list.innerHTML;
  Object.defineProperty(list, "innerHTML", {
    configurable: true,
    get(){ return built; },
    set(){ writes++; },
  });

  feature.render();
  assert.equal(writes, 0, "nothing changed — ages only");

  nodes[0].title = "renamed";
  feature.render();
  assert.equal(writes, 1, "a retitle is shape, so the list is rebuilt");
});

/* ---------- P3: caret at the end, never select-all (card description) ---------- */

test("P3 8: card description long-press focuses caret at end, never select-all", () => {
  let cardsSig = "";
  let editingDesc = "";
  let expanded = new Set();
  let actionCard = "";
  const nodes = [{
    id: "n1", title: "Alpha", description: "card desc body",
    lane_id: "", live: "quiet", model: "m", last_interaction: 1,
  }];
  const summaryFns = [];
  const timers = [];
  const descInput = {
    tagName: "TEXTAREA",
    value: "card desc body",
    dataset: { descInput: "n1" },
    _focused: false,
    _selected: false,
    _range: null,
    focus(){ this._focused = true; },
    select(){ this._selected = true; },
    setSelectionRange(a, b){
      this.selectionStart = a; this.selectionEnd = b; this._range = [a, b];
    },
  };
  const list = {
    innerHTML: "",
    querySelectorAll: () => [],
    addEventListener(){},
    removeEventListener(){},
  };
  const tabs = {
    innerHTML: "",
    querySelectorAll: () => [],
    addEventListener(){},
    removeEventListener(){},
  };
  const doc = {
    title: "",
    getElementById: () => null,
    querySelector(sel){
      if (typeof sel === "string" && sel.includes("data-desc-input")) return descInput;
      return null;
    },
  };
  assert.match(cardsSrc, /from "\.\/caret\.js"/, "cards imports caret.js");
  assert.match(cardsSrc, /focusAtEnd\s*\(/, "card description entry uses focusAtEnd");

  const feature = createCardsFeature({
    roots: { tabs, list },
    document: doc,
    nodes: () => nodes,
    unadopted: () => [],
    sel: () => "",
    cardTab: () => "current",
    setCardTab(){},
    laneFilter: () => "",
    attnFoldOpen: () => false,
    setAttnFoldOpen(){},
    expanded: () => expanded,
    actionCard: () => actionCard,
    setActionCard: v => { actionCard = v; },
    editingDesc: () => editingDesc,
    setEditingDesc: v => { editingDesc = v; },
    editingTitle: () => "",
    editingTitleScope: () => "",
    cardsSig: () => cardsSig,
    setCardsSig: v => { cardsSig = v; },
    pinned: () => [],
    archived: () => [],
    lanes: () => [],
    bookmarks: () => [],
    agentLogo: () => "",
    laneSelectHTML: () => "",
    laneColor: () => "#000",
    laneName: () => "",
    icons: {},
    CSS: { escape: s => s },
    setInterval: () => 0,
    clearInterval: () => {},
    setTimeout: (fn) => { timers.push(fn); return timers.length; },
    longpress(_root, selector, fn){
      if (selector === ".summary") summaryFns.push(fn);
      return () => {};
    },
  });
  feature.bind();
  assert.equal(summaryFns.length, 1, "summary longpress bound");
  const summaryEl = { dataset: { desc: "n1" } };
  summaryFns[0](summaryEl);
  assert.equal(editingDesc, "n1");
  for (const fn of timers) fn();

  assert.equal(descInput._focused, true, "description textarea focused");
  assert.equal(descInput._selected, false, "select() must not run");
  assert.deepEqual(descInput._range, [descInput.value.length, descInput.value.length],
    "caret at end of existing description");
  feature.destroy();
});

test("P3 12: withCardEditsPreserved still restores the original selection range", () => {
  /* Regression lock — not an entry point. Must pass at red and green. */
  const title = fakeField({ key: "title:n1", value: "Draft", active: true });
  title.selectionStart = 3;
  title.selectionEnd = 5;
  let fields = [title];
  const root = { querySelectorAll(){ return fields; } };
  const doc = { activeElement: title };
  withCardEditsPreserved(root, "", () => {
    title.value = "Server";
    title.selectionStart = 0;
    title.selectionEnd = 0;
    fields = [title];
  }, { document: doc });
  assert.equal(title.value, "Draft");
  assert.equal(title.focused, true);
  assert.deepEqual(title.ranged, [3, 5],
    "poll rebuild restores the original range unchanged — never forces end or select-all");
});

/* ---------- P2 Part B: expanded card descriptions use shared md() ---------- */

function descFeature(nodes, { expandedIds = [], editingDesc = "", cardsSig = "" } = {}){
  let sig = cardsSig;
  let edit = editingDesc;
  const expanded = new Set(expandedIds);
  const list = {
    innerHTML: "",
    querySelectorAll(){ return []; },
    addEventListener(){},
    removeEventListener(){},
  };
  const tabs = {
    innerHTML: "",
    querySelectorAll(){ return []; },
    addEventListener(){},
    removeEventListener(){},
  };
  const feature = createCardsFeature({
    roots: { tabs, list },
    document: { title: "", getElementById: () => null, querySelector: () => null },
    nodes: () => nodes,
    unadopted: () => [],
    sel: () => "",
    cardTab: () => "current",
    setCardTab(){},
    laneFilter: () => "",
    attnFoldOpen: () => false,
    setAttnFoldOpen(){},
    expanded: () => expanded,
    actionCard: () => "",
    setActionCard(){},
    editingDesc: () => edit,
    setEditingDesc: v => { edit = v; },
    editingTitle: () => "",
    editingTitleScope: () => "",
    cardsSig: () => sig,
    setCardsSig: v => { sig = v; },
    pinned: () => [],
    archived: () => [],
    lanes: () => [],
    bookmarks: () => [],
    agentLogo: () => "",
    laneSelectHTML: () => "",
    laneColor: () => "#000",
    laneName: () => "",
    icons: {},
    CSS: { escape: s => s },
    setInterval: () => 0,
    clearInterval: () => {},
  });
  return { feature, list, getEditingDesc: () => edit, setEditingDesc: v => { edit = v; },
    getSig: () => sig, setSig: v => { sig = v; }, expanded };
}

function summaryHTML(listHTML){
  const m = listHTML.match(/<div class="summary"[^>]*>([\s\S]*?)<\/div>\s*<textarea/);
  return m ? m[1] : "";
}

function descboxHTML(listHTML){
  const m = listHTML.match(/<textarea class="descbox"[^>]*>([\s\S]*?)<\/textarea>/);
  return m ? m[1] : null;
}

test("P2 md: cards.js imports shared md beside esc/ageText", () => {
  assert.match(cardsSrc, /import\s*\{[^}]*\bmd\b[^}]*\}\s*from\s*["']\.\/format\.js["']/,
    "cards must import md from format.js — no second Markdown dialect");
  assert.match(cardsSrc, /import\s*\{[^}]*\besc\b[^}]*\bageText\b[^}]*\}\s*from\s*["']\.\/format\.js["']/);
});

test("P2 md: expanded summary renders bold, italic, code, list, and link", () => {
  const desc = "Hello **bold** and *italic* with `code`\n\n- item one\n- item two\n\n[docs](https://example.com/x)";
  const { feature, list } = descFeature([{
    id: "n1", title: "T", description: desc, lane_id: "l", live: "quiet", model: "m",
    last_interaction: 1,
  }], { expandedIds: ["n1"] });
  feature.render();
  const sum = summaryHTML(list.innerHTML);
  assert.match(sum, /<strong>bold<\/strong>/);
  assert.match(sum, /<em>italic<\/em>/);
  assert.match(sum, /<code>code<\/code>/);
  assert.match(sum, /<ul><li>item one<\/li><li>item two<\/li><\/ul>/);
  assert.match(sum, /<a href="https:\/\/example\.com\/x" target="_blank" rel="noopener">docs<\/a>/);
  assert.equal(sum, sharedMd(desc), "card summary must equal shared md() for the same source");
});

test("P2 md: expanded summary renders fenced code and tables", () => {
  const desc = "```\nconst x = 1 < 2\n```\n\n| A | B |\n| --- | --- |\n| 1 | 2 |";
  const { feature, list } = descFeature([{
    id: "n1", title: "T", description: desc, lane_id: "l", live: "quiet", model: "m",
    last_interaction: 1,
  }], { expandedIds: ["n1"] });
  feature.render();
  const sum = summaryHTML(list.innerHTML);
  assert.match(sum, /<pre><code>const x = 1 &lt; 2<\/code><\/pre>/);
  assert.match(sum, /<table><tr><th>A<\/th><th>B<\/th><\/tr><tr><td>1<\/td><td>2<\/td><\/tr><\/table>/);
  assert.equal(sum, sharedMd(desc));
});

test("P2 md: script-like and attribute-shaped input stays escaped", () => {
  const desc = 'Click <script>alert(1)</script> and <img src=x onerror="alert(1)">';
  const { feature, list } = descFeature([{
    id: "n1", title: "T", description: desc, lane_id: "l", live: "quiet", model: "m",
    last_interaction: 1,
  }], { expandedIds: ["n1"] });
  feature.render();
  const sum = summaryHTML(list.innerHTML);
  // Escape-first: angle brackets become entities so no live tag/attribute runs.
  // The literal text "onerror=" may still appear inside escaped content.
  assert.doesNotMatch(sum, /<script[\s>]/i);
  assert.doesNotMatch(sum, /<img[\s>]/i);
  assert.match(sum, /&lt;script&gt;/);
  assert.match(sum, /&lt;img/);
  assert.match(sum, /onerror=&quot;alert\(1\)&quot;/);
  assert.equal(sum, sharedMd(desc));
});

test("P2 md: .summary is rendered HTML while .descbox holds raw Markdown", () => {
  const desc = "**keep raw** and a [link](https://example.com)";
  const { feature, list } = descFeature([{
    id: "n1", title: "T", description: desc, lane_id: "l", live: "quiet", model: "m",
    last_interaction: 1,
  }], { expandedIds: ["n1"] });
  feature.render();
  const sum = summaryHTML(list.innerHTML);
  const box = descboxHTML(list.innerHTML);
  assert.match(sum, /<strong>keep raw<\/strong>/);
  assert.match(sum, /<a href="https:\/\/example\.com"/);
  // textarea content is entity-escaped for safe HTML embedding of raw source
  assert.equal(box, sharedEsc(desc));
  assert.match(box, /\*\*keep raw\*\*/);
  assert.doesNotMatch(box, /<strong>/);
});

test("P2 md: empty description falls back to No description yet", () => {
  const { feature, list } = descFeature([{
    id: "n1", title: "T", description: "", prompt: "", lane_id: "l", live: "quiet", model: "m",
    last_interaction: 1,
  }], { expandedIds: ["n1"] });
  feature.render();
  assert.equal(summaryHTML(list.innerHTML), "No description yet.");
  assert.equal(descboxHTML(list.innerHTML), "");
});

test("P2 md: prompt is used when description is empty", () => {
  const { feature, list } = descFeature([{
    id: "n1", title: "T", description: "", prompt: "use **prompt**", lane_id: "l",
    live: "quiet", model: "m", last_interaction: 1,
  }], { expandedIds: ["n1"] });
  feature.render();
  assert.equal(summaryHTML(list.innerHTML), sharedMd("use **prompt**"));
});

test("P2 md: active description edit survives a poll rebuild after Markdown rendering", () => {
  const nodes = [{
    id: "n1", title: "T", description: "server **desc**", lane_id: "l",
    live: "quiet", model: "m", last_interaction: 1,
  }];
  let editingDesc = "n1";
  let cardsSig = "";
  const descInput = {
    value: "draft *markdown* still typing",
    dataset: { descInput: "n1" },
    type: "textarea",
    selectionStart: 6,
    selectionEnd: 15,
    focused: false,
    setSelectionRange(a, b){ this.selectionStart = a; this.selectionEnd = b; this.ranged = [a, b]; },
    focus(){ this.focused = true; },
  };
  let fields = [descInput];
  const list = {
    innerHTML: "",
    querySelectorAll(sel){
      if (sel === "input, textarea, select") return fields;
      return [];
    },
    addEventListener(){},
    removeEventListener(){},
  };
  const tabs = { innerHTML: "", querySelectorAll: () => [], addEventListener(){}, removeEventListener(){} };
  const doc = { title: "", activeElement: descInput, getElementById: () => null, querySelector: () => null };
  const feature = createCardsFeature({
    roots: { tabs, list },
    document: doc,
    nodes: () => nodes,
    unadopted: () => [],
    sel: () => "",
    cardTab: () => "current",
    setCardTab(){},
    laneFilter: () => "",
    attnFoldOpen: () => false,
    setAttnFoldOpen(){},
    expanded: () => new Set(["n1"]),
    actionCard: () => "",
    setActionCard(){},
    editingDesc: () => editingDesc,
    setEditingDesc: v => { editingDesc = v; },
    editingTitle: () => "",
    editingTitleScope: () => "",
    cardsSig: () => cardsSig,
    setCardsSig: v => { cardsSig = v; },
    pinned: () => [],
    archived: () => [],
    lanes: () => [],
    bookmarks: () => [],
    agentLogo: () => "",
    laneSelectHTML: () => "",
    laneColor: () => "#000",
    laneName: () => "",
    icons: {},
    CSS: { escape: s => s },
    setInterval: () => 0,
    clearInterval: () => {},
  });
  feature.render();
  // Simulate poll: server description changed, force rebuild while editor is open
  nodes[0].description = "server **changed**";
  cardsSig = ""; // invalidate like a structural poll
  // After rebuild, withCardEditsPreserved re-queries fields; keep the same fake
  // element so restored value/focus land where the test can observe them.
  feature.render();
  assert.equal(descInput.value, "draft *markdown* still typing",
    "active desc editor must keep the raw draft, never the rendered Markdown");
  assert.equal(descInput.focused, true);
  assert.deepEqual(descInput.ranged, [6, 15]);
  // Summary (read surface) still renders via md when not in the editing overlay —
  // but while .editing is set the CSS hides .summary; the HTML may still contain
  // the server's rendered description for the next read mode.
  assert.match(list.innerHTML, /class="card[^"]*editing/);
  assert.match(list.innerHTML, /data-desc-input="n1"/);
});

test("P2 md: .summary CSS covers paragraphs, lists, tables, blockquotes, code", () => {
  /* Minimal fit-inside-card rules only — no card geometry redesign. */
  for (const sel of [
    ".card .summary p",
    ".card .summary ul",
    ".card .summary ol",
    ".card .summary li",
    ".card .summary table",
    ".card .summary blockquote",
    ".card .summary pre",
    ".card .summary code",
  ]) {
    assert.ok(cardsCssSrc.includes(sel) || cardsCssSrc.includes(sel.replace(".card ", "")),
      `cards.css must style ${sel} for Markdown descendants`);
  }
});

/* ---------- Phase 3: card-list / tabs interaction dispatcher ---------- */

function actTarget(attrs, closestSels){
  const dataset = {};
  for (const [k, v] of Object.entries(attrs || {})){
    const camel = k.replace(/-([a-z])/g, (_, c) => c.toUpperCase());
    dataset[camel] = v;
  }
  const t = { dataset, classList: { contains: () => false } };
  const hit = new Set(Array.isArray(closestSels) ? closestSels : [closestSels].filter(Boolean));
  t.closest = (sel) => hit.has(sel) ? t : null;
  return t;
}

function cardsActionHarness(opts = {}){
  const state = {
    nodes: opts.nodes || [{ id: "n1", title: "Alpha", description: "old", live: "quiet", adopted: false }],
    cardTab: opts.cardTab || "current",
    actionCard: opts.actionCard || "",
    editingDesc: opts.editingDesc || "",
    attnFoldOpen: false,
    expanded: new Set(opts.expanded || []),
    pinned: [...(opts.pinned || [])],
    archived: [...(opts.archived || [])],
    sel: opts.sel || "",
    cardsSig: "x",
    laneFilter: opts.laneFilter || "",
  };
  const handlers = { list: new Map(), tabs: new Map() };
  const list = {
    innerHTML: "",
    querySelectorAll: () => [],
    addEventListener(type, fn){ handlers.list.set(type, fn); },
    removeEventListener(type){ handlers.list.delete(type); },
  };
  const tabs = {
    innerHTML: "",
    querySelectorAll: () => [],
    addEventListener(type, fn){ handlers.tabs.set(type, fn); },
    removeEventListener(type){ handlers.tabs.delete(type); },
  };
  const effects = {
    apiCalls: [],
    uiMutate: [],
    alerts: [],
    confirms: [],
    fieldErrors: [],
    focused: [],
    exitThread: [],
    openAdopt: [],
    selectNode: [],
    setLevel: [],
    setLaneFilter: [],
    updateLocalNode: [],
    removeNode: [],
    invalidateChatSig: 0,
    invalidateMapSig: 0,
    invalidateStateEtag: 0,
    renderChatHead: 0,
    renderMap: 0,
    scheduleTick: 0,
    startTitleEdit: [],
  };
  let apiImpl = opts.api || (async (url, init) => {
    effects.apiCalls.push({ url, init });
    return { id: "n1", title: "Alpha", description: "saved" };
  });
  let confirmImpl = opts.confirm !== undefined ? opts.confirm : () => true;
  const docEls = opts.docEls || {};
  const feature = createCardsFeature({
    roots: { tabs, list },
    document: {
      title: "",
      querySelector: (sel) => docEls[sel] || null,
      getElementById: () => null,
    },
    CSS: { escape: s => s },
    nodes: () => state.nodes,
    unadopted: () => opts.unadopted || [],
    sel: () => state.sel,
    setSel: v => { state.sel = v; },
    cardTab: () => state.cardTab,
    setCardTab: v => { state.cardTab = v; },
    laneFilter: () => state.laneFilter,
    setLaneFilter: v => { effects.setLaneFilter.push(v); state.laneFilter = v; },
    attnFoldOpen: () => state.attnFoldOpen,
    setAttnFoldOpen: v => { state.attnFoldOpen = v; },
    expanded: () => state.expanded,
    actionCard: () => state.actionCard,
    setActionCard: v => { state.actionCard = v; },
    editingDesc: () => state.editingDesc,
    setEditingDesc: v => { state.editingDesc = v; },
    editingTitle: () => "",
    editingTitleScope: () => "",
    cardsSig: () => state.cardsSig,
    setCardsSig: v => { state.cardsSig = v; },
    pinned: () => state.pinned,
    archived: () => state.archived,
    lanes: () => opts.lanes || [],
    laneList: () => opts.lanes || [],
    bookmarks: () => [],
    agentLogo: () => "",
    laneSelectHTML: () => "",
    laneColor: () => "#000",
    laneName: () => "",
    icons: {},
    setInterval: () => 0,
    clearInterval: () => {},
    api: (...a) => apiImpl(...a),
    uiMutate: (op) => { effects.uiMutate.push(op); },
    alert: (m) => { effects.alerts.push(m); },
    confirm: (m) => { effects.confirms.push(m); return typeof confirmImpl === "function" ? confirmImpl(m) : !!confirmImpl; },
    fieldError: (el, msg) => { effects.fieldErrors.push({ el, msg }); },
    readLaneChoice: opts.readLaneChoice || (() => ({})),
    updateLocalNode: (n) => { effects.updateLocalNode.push(n); },
    removeNode: (id) => {
      effects.removeNode.push(id);
      state.nodes = state.nodes.filter(n => n.id !== id);
    },
    exitThread: (id) => { effects.exitThread.push(id); },
    openAdopt: (s) => { effects.openAdopt.push(s); },
    selectNode: (id) => { effects.selectNode.push(id); },
    setLevel: (n) => { effects.setLevel.push(n); },
    isDesktop: () => opts.isDesktop !== undefined ? opts.isDesktop : true,
    invalidateChatSig: () => { effects.invalidateChatSig++; },
    invalidateMapSig: () => { effects.invalidateMapSig++; },
    invalidateStateEtag: () => { effects.invalidateStateEtag++; },
    renderChatHead: () => { effects.renderChatHead++; },
    renderMap: () => { effects.renderMap++; },
    scheduleTick: () => { effects.scheduleTick++; },
    startTitleEdit: (id, scope) => { effects.startTitleEdit.push([id, scope]); },
    longpress: opts.longpress || (() => () => {}),
  });
  feature.bind();
  return {
    state, effects, handlers, feature, list, tabs,
    setApi(fn){ apiImpl = fn; },
    setConfirm(v){ confirmImpl = v; },
    clickList(target){ return handlers.list.get("click")({ target, preventDefault(){} }); },
    clickTabs(target){ return handlers.tabs.get("click")({ target, preventDefault(){} }); },
  };
}

test("tabs: clear-scope clears lane filter only", () => {
  const h = cardsActionHarness({ laneFilter: "L1" });
  h.clickTabs(actTarget({}, ["[data-clear-scope]"]));
  assert.deepEqual(h.effects.setLaneFilter, [""]);
  assert.equal(h.state.cardTab, "current");
});

test("tabs: switching tab clears actionCard and editingDesc", () => {
  const h = cardsActionHarness({ actionCard: "n1", editingDesc: "n1" });
  const btn = actTarget({ tab: "archived" }, ["[data-tab]"]);
  const siblings = [{ classList: { toggle(){} } }, btn];
  btn.classList = { toggle(name, on){ if (on) this._on = true; } };
  h.tabs.querySelectorAll = () => siblings;
  h.clickTabs(btn);
  assert.equal(h.state.cardTab, "archived");
  assert.equal(h.state.actionCard, "");
  assert.equal(h.state.editingDesc, "");
});

test("click: attn fold toggles open state", () => {
  /* foldList is non-empty only with a lane filter and out-of-scope hard attention;
     otherwise renderCards forces attnFoldOpen back to false. */
  const h = cardsActionHarness({
    laneFilter: "lane-a",
    nodes: [
      { id: "n1", title: "In", lane_id: "lane-a", live: "quiet" },
      { id: "n2", title: "Need", lane_id: "lane-b", live: "quiet", attention: "approval" },
    ],
  });
  h.clickList(actTarget({}, ["[data-attnfold]"]));
  assert.equal(h.state.attnFoldOpen, true);
  h.clickList(actTarget({}, ["[data-attnfold]"]));
  assert.equal(h.state.attnFoldOpen, false);
});

test("click: save-desc PATCH sends description body and clears editing", async () => {
  const ta = { value: "  new desc  " };
  const h = cardsActionHarness({
    editingDesc: "n1",
    docEls: { '[data-desc-input="n1"]': ta },
  });
  await h.clickList(actTarget({ "save-desc": "n1" }, ["[data-save-desc]"]));
  assert.equal(h.effects.apiCalls.length, 1);
  assert.equal(h.effects.apiCalls[0].url, "/api/nodes/n1");
  assert.equal(h.effects.apiCalls[0].init.method, "PATCH");
  assert.deepEqual(JSON.parse(h.effects.apiCalls[0].init.body), { description: "new desc" });
  assert.equal(h.state.editingDesc, "");
  assert.equal(h.effects.updateLocalNode.length, 1);
  assert.equal(h.effects.invalidateChatSig, 1);
  assert.equal(h.effects.renderChatHead, 1);
});

test("click: save-desc with new lane mutates lanes and includes lane_id", async () => {
  const lane = { id: "lane-x", name: "Trip", color: "#111" };
  const h = cardsActionHarness({
    editingDesc: "n1",
    lanes: [{ id: "l0", name: "Old" }],
    docEls: { '[data-desc-input="n1"]': { value: "d" }, '[data-card-lane="n1"]': {
      querySelector: (sel) => sel.includes("lane-select") ? {} : (sel.includes("lane-new") ? {} : null),
    } },
    readLaneChoice: () => ({ laneID: lane.id, lane }),
  });
  await h.clickList(actTarget({ "save-desc": "n1" }, ["[data-save-desc]"]));
  assert.deepEqual(JSON.parse(h.effects.apiCalls[0].init.body), { description: "d", lane_id: "lane-x" });
  assert.equal(h.effects.uiMutate.length, 1);
  assert.equal(h.effects.uiMutate[0].k, "lanes");
  assert.equal(h.effects.uiMutate[0].lanes.length, 2);
  assert.equal(h.effects.uiMutate[0].lanes[1].id, "lane-x");
});

test("click: save-desc lane validation error focuses field and skips API", async () => {
  const laneNew = { focus(){ this._focused = true; }, _focused: false };
  const laneBox = {
    querySelector: (sel) => sel.includes("lane-new") ? laneNew : (sel.includes("lane-select") ? {} : null),
  };
  const h = cardsActionHarness({
    editingDesc: "n1",
    docEls: {
      '[data-desc-input="n1"]': { value: "d" },
      '[data-card-lane="n1"]': laneBox,
    },
    readLaneChoice: () => ({ error: "Enter a lane name." }),
  });
  await h.clickList(actTarget({ "save-desc": "n1" }, ["[data-save-desc]"]));
  assert.equal(h.effects.apiCalls.length, 0);
  assert.equal(h.effects.fieldErrors.length, 1);
  assert.equal(h.effects.fieldErrors[0].msg, "Enter a lane name.");
  assert.equal(laneNew._focused, true);
  assert.equal(h.state.editingDesc, "n1", "editing preserved on validation failure");
});

test("click: save-desc API failure alerts and keeps editingDesc", async () => {
  const h = cardsActionHarness({
    editingDesc: "n1",
    docEls: { '[data-desc-input="n1"]': { value: "d" } },
    api: async () => { throw new Error("patch failed"); },
  });
  await h.clickList(actTarget({ "save-desc": "n1" }, ["[data-save-desc]"]));
  assert.deepEqual(h.effects.alerts, ["patch failed"]);
  assert.equal(h.state.editingDesc, "n1");
  assert.equal(h.effects.updateLocalNode.length, 0);
});

test("click: pin mutates pin and clears actionCard", () => {
  const h = cardsActionHarness({ actionCard: "n1", pinned: [] });
  h.clickList(actTarget({ "pin-action": "n1" }, ["[data-pin-action]"]));
  assert.deepEqual(h.effects.uiMutate, [{ k: "pin", id: "n1" }]);
  assert.equal(h.state.actionCard, "");
});

test("click: unpin when already pinned", () => {
  const h = cardsActionHarness({ pinned: ["n1"] });
  h.clickList(actTarget({ "pin-action": "n1" }, ["[data-pin-action]"]));
  assert.deepEqual(h.effects.uiMutate, [{ k: "unpin", id: "n1" }]);
});

test("click: exit clears actionCard and calls exitThread", () => {
  const h = cardsActionHarness({ actionCard: "n1" });
  h.clickList(actTarget({ exit: "n1" }, ["[data-exit]"]));
  assert.equal(h.state.actionCard, "");
  assert.deepEqual(h.effects.exitThread, ["n1"]);
});

test("click: archive and restore uiMutate", () => {
  const h = cardsActionHarness({ actionCard: "n1" });
  h.clickList(actTarget({ "arch-action": "n1" }, ["[data-arch-action]"]));
  assert.deepEqual(h.effects.uiMutate, [{ k: "arch", id: "n1" }]);
  assert.equal(h.state.actionCard, "");

  const h2 = cardsActionHarness({ archived: ["n1"] });
  h2.clickList(actTarget({ "arch-action": "n1" }, ["[data-arch-action]"]));
  assert.deepEqual(h2.effects.uiMutate, [{ k: "unarch", id: "n1" }]);
});

test("click: delete confirm owned removes node and clears selection", async () => {
  const h = cardsActionHarness({
    sel: "n1",
    actionCard: "n1",
    editingDesc: "n1",
    pinned: ["n1"],
    archived: ["n1"],
  });
  await h.clickList(actTarget({ trash: "n1" }, ["[data-trash]"]));
  assert.equal(h.effects.apiCalls[0].url, "/api/nodes/n1");
  assert.equal(h.effects.apiCalls[0].init.method, "DELETE");
  assert.ok(h.effects.confirms[0].includes("close its running session"));
  assert.deepEqual(h.effects.removeNode, ["n1"]);
  assert.equal(h.state.sel, "");
  assert.equal(h.state.actionCard, "");
  assert.equal(h.state.editingDesc, "");
  assert.ok(h.effects.uiMutate.some(op => op.k === "unarch"));
  assert.ok(h.effects.uiMutate.some(op => op.k === "unpin"));
  assert.equal(h.effects.scheduleTick, 1);
  assert.equal(h.effects.invalidateStateEtag, 1);
});

test("click: delete retired external confirmation preserves history and pane", async () => {
  const h = cardsActionHarness({
    nodes: [{ id: "n1", title: "Alpha", adopted: true, live: "quiet" }],
  });
  await h.clickList(actTarget({ trash: "n1" }, ["[data-trash]"]));
  assert.match(h.effects.confirms[0], /saved history will be archived/);
  assert.match(h.effects.confirms[0], /external tmux session will keep running/);
});

test("click: delete cancel skips API and removeNode", async () => {
  const h = cardsActionHarness({ confirm: () => false });
  await h.clickList(actTarget({ trash: "n1" }, ["[data-trash]"]));
  assert.equal(h.effects.apiCalls.length, 0);
  assert.equal(h.effects.removeNode.length, 0);
  assert.equal(h.state.nodes.length, 1);
});

test("click: delete API failure alerts and keeps the card", async () => {
  const h = cardsActionHarness({
    api: async () => { throw new Error("delete failed"); },
  });
  await h.clickList(actTarget({ trash: "n1" }, ["[data-trash]"]));
  assert.deepEqual(h.effects.alerts, ["delete failed"]);
  assert.equal(h.effects.removeNode.length, 0);
  assert.equal(h.state.nodes.length, 1);
});

test("click: dismiss actions-open clears actionCard without selecting", () => {
  const go = { dataset: { open: "n1" } };
  const card = {
    classList: { contains: () => false },
    querySelector: (sel) => sel === ".go" ? go : null,
    closest: (sel) => sel === ".card.actions-open" ? card : null,
  };
  const h = cardsActionHarness({ actionCard: "n1" });
  h.clickList(card);
  assert.equal(h.state.actionCard, "");
  assert.equal(h.effects.selectNode.length, 0);
});

test("click: expand toggles expanded set and clears actionCard", () => {
  const h = cardsActionHarness({ actionCard: "n1" });
  h.clickList(actTarget({ x: "n1" }, ["[data-x]"]));
  assert.equal(h.state.expanded.has("n1"), true);
  assert.equal(h.state.actionCard, "");
  h.clickList(actTarget({ x: "n1" }, ["[data-x]"]));
  assert.equal(h.state.expanded.has("n1"), false);
});

test("click: stale adoption target has no callback", () => {
  const h = cardsActionHarness();
  h.clickList(actTarget({ adopt: "sess-1" }, ["[data-adopt]"]));
  assert.deepEqual(h.effects.openAdopt, []);
});

test("click: open on desktop selects without setLevel", () => {
  const h = cardsActionHarness({
    actionCard: "n1",
    editingDesc: "n1",
    isDesktop: true,
  });
  h.clickList(actTarget({ open: "n1" }, ["[data-open]"]));
  assert.deepEqual(h.effects.selectNode, ["n1"]);
  assert.equal(h.effects.setLevel.length, 0);
  assert.equal(h.state.actionCard, "");
  assert.equal(h.state.editingDesc, "");
});

test("click: open on mobile selects and setLevel(1)", () => {
  const h = cardsActionHarness({ isDesktop: false });
  h.clickList(actTarget({ open: "n1" }, ["[data-open]"]));
  assert.deepEqual(h.effects.selectNode, ["n1"]);
  assert.deepEqual(h.effects.setLevel, [1]);
});

test("touch: swipe opens actionCard; expanded card ignores swipe", () => {
  const h = cardsActionHarness();
  const go = { dataset: { open: "n1" } };
  const card = {
    classList: { contains: (c) => false },
    querySelector: (sel) => sel === ".go" ? go : null,
    closest: (sel) => sel === ".card" ? card : null,
  };
  h.handlers.list.get("touchstart")({ target: card, touches: [{ clientX: 200, clientY: 10 }] });
  h.handlers.list.get("touchend")({
    target: card,
    changedTouches: [{ clientX: 100, clientY: 12 }], /* dx=-100, |dx| > 1.6*|dy| */
  });
  assert.equal(h.state.actionCard, "n1");

  const h2 = cardsActionHarness({ actionCard: "" });
  const expanded = {
    classList: { contains: (c) => c === "expanded" },
    querySelector: (sel) => sel === ".go" ? go : null,
    closest: (sel) => sel === ".card" ? expanded : null,
  };
  h2.handlers.list.get("touchstart")({ target: expanded, touches: [{ clientX: 200, clientY: 10 }] });
  h2.handlers.list.get("touchend")({
    target: expanded,
    changedTouches: [{ clientX: 100, clientY: 12 }],
  });
  assert.equal(h2.state.actionCard, "", "expanded card must not open actions via swipe");
});

test("longpress: title starts title edit; card toggles actionCard", () => {
  const longpressFns = [];
  const h = cardsActionHarness({
    longpress: (_root, sel, fn) => { longpressFns.push({ sel, fn }); return () => {}; },
  });
  const titleLP = longpressFns.find(x => x.sel === ".card .title");
  const cardLP = longpressFns.find(x => x.sel === ".card");
  assert.ok(titleLP && cardLP);
  titleLP.fn({ dataset: { title: "n1" } });
  assert.deepEqual(h.effects.startTitleEdit, [["n1", "cards"]]);

  const cardEl = {
    classList: { contains: () => false },
    querySelector: (sel) => sel === ".go" ? { dataset: { open: "n1" } } : null,
    closest: () => null,
  };
  cardLP.fn(cardEl, { target: cardEl });
  assert.equal(h.state.actionCard, "n1");
  cardLP.fn(cardEl, { target: cardEl });
  assert.equal(h.state.actionCard, "");
});
