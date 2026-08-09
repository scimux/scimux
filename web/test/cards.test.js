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
  adoptCardsHTML,
  attnFoldButtonHTML,
  cardTabsHTML,
  pinOrderAfterDrag,
  cardSwipeOpensActions,
  cardEditKey,
  withCardEditsPreserved,
  createCardsFeature,
} from "../js/cards.js";
import { orderedNodes, hardAttention, visibleCardLists } from "../js/map-model.js";

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

test("emptyCardsHTML messages and adoptCardsHTML lane/tab gates", () => {
  assert.match(emptyCardsHTML({ laneFilter: "L1", cardTab: "current" }), /No activities on this journey/);
  assert.match(emptyCardsHTML({ laneFilter: "L1", cardTab: "archived" }), /No archived activities on this journey/);
  assert.match(emptyCardsHTML({ laneFilter: "", cardTab: "archived" }), /Nothing archived yet/);
  assert.equal(emptyCardsHTML({ laneFilter: "", cardTab: "current", hasAdoptCards: true }), "");
  assert.match(emptyCardsHTML({ laneFilter: "", cardTab: "current", hasAdoptCards: false }), /start the first one/);

  assert.equal(adoptCardsHTML(["s1"], { cardTab: "archived", laneFilter: "" }), "");
  assert.equal(adoptCardsHTML(["s1"], { cardTab: "current", laneFilter: "L1" }), "");
  const ad = adoptCardsHTML(["sess"], { cardTab: "current", laneFilter: "" });
  assert.match(ad, /class="card adoptable"/);
  assert.match(ad, /data-adopt="sess"/);
  assert.match(ad, /aria-label="adopt sess"/);
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

test("ordering remains map-model responsibility (pins → attention → fresh)", () => {
  // Do not re-test full matrix; one smoke that cards imports orderedNodes for counts only
  assert.match(cardsSrc, /orderedNodes/);
  const ids = orderedNodes([
    { id: "idle", last_activity: 1 },
    { id: "attn", attention: "approval" },
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
  /* live flips on the 2s poll for every working agent, and livenessTier turns
     that flip into a reorder — so folding either into the shape signature
     means the list is rebuilt from scratch for as long as anything is running. */
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
  /* The common case: an agent starts a turn, livenessTier promotes it. */
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
  assert.deepEqual(dom.children.map(c => c.id), ["attnfold", "card-b", "card-a"],
    "livenessTier promotes it, so the patch must move it — around the fold button, not over it");
  assert.deepEqual(dom.moves, [["card-b", "card-a"]], "with one insertBefore, not a re-append of each");
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
