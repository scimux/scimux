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
  cardsRenderDecision,
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
    pinned: () => [],
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

test("CARD_TIME_SWAP_MS is 30s; pin action markup order pin < archive < trash", () => {
  assert.equal(CARD_TIME_SWAP_MS, 30000);
  assert.match(cardsSrc, /data-pin-action/);
  assert.match(cardsSrc, /data-arch-action/);
  assert.match(cardsSrc, /data-trash/);
  assert.match(cardsSrc, /pinflag/);
  assert.match(cardsSrc, /if \(pinDragging\) return/);
  assert.match(cardsSrc, /k: "pin-order"/);
  assert.match(cardsSrc, /draggable="true"/);
  // pin button before archive/trash in template
  const pin = cardsSrc.indexOf("data-pin-action");
  const arch = cardsSrc.indexOf("data-arch-action");
  const trash = cardsSrc.indexOf("data-trash");
  assert.ok(pin > 0 && pin < arch && arch < trash);
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
