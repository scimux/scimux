/* Characterization tests for web/js/notes.js — pure decisions + factory
 * adapters with a small fake DOM, controlled promises, and fake timers.
 * Does not re-test format escaping, bookmark lane/sort/clamp algorithms,
 * navigation swipe math, or search/archive. No mutable production test accessors. */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import {
  SAVE_DEBOUNCE_MS,
  STORAGE_KEY_FOLDS,
  STORAGE_KEY_INBOX_TAB,
  COPY_ACK_MS,
  parseNoteFolds,
  foldsAfterSet,
  provLabel,
  sectionSwapPlan,
  noteCardReorderPlan,
  cardLanesFromDoc,
  bookmarkSource,
  buildBookmarkSnapshot,
  createSaveEnqueue,
  sectionBodyInner,
  sectionHTML,
  referenceHTML,
  noteCardHTML,
  noteCardDragEnabled,
  noteCardRenamePlan,
  inboxLaneTabs,
  resolveInboxTab,
  inboxListModel,
  inboxTabsHTML,
  inboxItemHTML,
  applyNotesSwipeAction,
  createNotesFeature,
} from "../js/notes.js";
import { bookmarkLaneId, bookmarkSortKey, bookmarkClampState } from "../js/bookmarks.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const notesSrc = readFileSync(join(__dirname, "../js/notes.js"), "utf8");
const bookmarksSrc = readFileSync(join(__dirname, "../js/bookmarks.js"), "utf8");
const tokensSrc = readFileSync(join(__dirname, "../css/tokens.css"), "utf8");
const notesCssSrc = readFileSync(join(__dirname, "../css/notes.css"), "utf8");

/* ---------- module shape ---------- */
test("notes.js exports factory and pure helpers; no later-feature imports", () => {
  assert.match(notesSrc, /export function createNotesFeature/);
  assert.match(notesSrc, /from "\.\/format\.js"/);
  assert.match(notesSrc, /from "\.\/bookmarks\.js"/);
  assert.match(notesSrc, /from "\.\/navigation\.js"/);
  assert.match(notesSrc, /from "\.\/lanes\.js"/);
  assert.doesNotMatch(notesSrc, /from "\.\/search\.js"/);
  assert.doesNotMatch(notesSrc, /from "\.\/sheets\.js"/);
  assert.doesNotMatch(notesSrc, /from "\.\/polling\.js"/);
  assert.doesNotMatch(notesSrc, /from "\.\/app\.js"/);
  assert.doesNotMatch(notesSrc, /from "\.\/chat\.js"/);
  assert.doesNotMatch(notesSrc, /from "\.\/composer\.js"/);
  assert.doesNotMatch(notesSrc, /test\/debug accessors|getWsActive|_wsNotes/);
  assert.doesNotMatch(bookmarksSrc, /from ["'].*notes/);
});

test("storage keys, debounce, copy ack constants", () => {
  assert.equal(STORAGE_KEY_FOLDS, "scimux-notefolds");
  assert.equal(STORAGE_KEY_INBOX_TAB, "scimux-wsinboxtab");
  assert.equal(SAVE_DEBOUNCE_MS, 1100);
  assert.equal(COPY_ACK_MS, 900);
});

/* ---------- Phase 1a/1b/1e: --danger token; destructive UI is red ---------- */
test("--danger token is defined in light and dark; distinct from --attn", () => {
  /* light :root block owns the first --danger; dark block redefines it. */
  const lightMatch = tokensSrc.match(/:root\s*\{([\s\S]*?)\n\}/);
  assert.ok(lightMatch, "tokens.css must declare a light :root block");
  assert.match(lightMatch[1], /--danger:\s*#FF3B30\s*;/);
  assert.match(lightMatch[1], /--attn:/);
  assert.doesNotMatch(lightMatch[1], /--danger:\s*var\(--attn\)/);

  const darkMatch = tokensSrc.match(/@media\s*\(prefers-color-scheme:\s*dark\)\s*\{\s*:root\s*\{([\s\S]*?)\n\s*\}/);
  assert.ok(darkMatch, "tokens.css must declare a dark :root block");
  assert.match(darkMatch[1], /--danger:\s*#FF453A\s*;/);
  assert.match(darkMatch[1], /--attn:/);

  /* semantic split: attention yellow must not be the destructive colour. */
  const lightAttn = lightMatch[1].match(/--attn:\s*([^;]+);/);
  const lightDanger = lightMatch[1].match(/--danger:\s*([^;]+);/);
  assert.ok(lightAttn && lightDanger);
  assert.notEqual(lightAttn[1].trim(), lightDanger[1].trim());
});

test(".wsmenu button.danger uses --danger, not --attn (covers Delete note + Delete section)", () => {
  /* Section menu data-mi=del and note menu data-si=del both use class="danger". */
  assert.match(notesSrc, /data-mi="del"[^>]*class="danger"|class="danger"[^>]*data-mi="del"/);
  assert.match(notesCssSrc, /\.wsmenu\s+button\.danger\s*\{[^}]*color:\s*var\(--danger\)/);
  assert.doesNotMatch(notesCssSrc, /\.wsmenu\s+button\.danger\s*\{[^}]*color:\s*var\(--attn\)/);
});

test("card delete action uses --danger (not --attn)", () => {
  assert.match(notesCssSrc, /\.wscardactions\s+button\.danger\s*\{[^}]*color:\s*var\(--danger\)/);
  assert.doesNotMatch(notesCssSrc, /\.wscardactions\s+button\.danger\s*\{[^}]*color:\s*var\(--attn\)/);
});

/* ---------- folds ---------- */
test("parseNoteFolds: empty, valid, corrupt", () => {
  assert.deepEqual(parseNoteFolds(null), {});
  assert.deepEqual(parseNoteFolds(""), {});
  assert.deepEqual(parseNoteFolds("{}"), {});
  assert.deepEqual(parseNoteFolds('{"s1":1}'), { s1: 1 });
  assert.deepEqual(parseNoteFolds("not-json"), {});
  assert.deepEqual(parseNoteFolds("null"), {});
});

test("foldsAfterSet set and clear", () => {
  assert.deepEqual(foldsAfterSet({}, "a", true), { a: 1 });
  assert.deepEqual(foldsAfterSet({ a: 1, b: 1 }, "a", false), { b: 1 });
  assert.deepEqual(foldsAfterSet({ a: 1 }, "a", true), { a: 1 });
});

/* ---------- provenance ---------- */
test("provLabel joins non-empty parts", () => {
  assert.equal(provLabel("Alpha", "agent", "Jul 28"), "Alpha · agent · Jul 28");
  assert.equal(provLabel("", "", "Jul 28"), "Jul 28");
  assert.equal(provLabel("Alpha", "", ""), "Alpha");
  assert.equal(provLabel("", "", ""), "");
});

/* ---------- section swap boundaries ---------- */
test("sectionSwapPlan: visual order, no wrap at boundaries", () => {
  const secs = [{ id: "b", order: 1 }, { id: "a", order: 0 }, { id: "c", order: 2 }];
  assert.equal(sectionSwapPlan(secs, "a", "up"), null);
  assert.equal(sectionSwapPlan(secs, "c", "down"), null);
  const up = sectionSwapPlan(secs, "b", "up");
  assert.deepEqual({ a: up.a, b: up.b }, { a: "b", b: "a" });
  assert.equal(up.ao, 0);
  assert.equal(up.bo, 1);
  const dn = sectionSwapPlan(secs, "b", "down");
  assert.deepEqual({ a: dn.a, b: dn.b }, { a: "b", b: "c" });
  assert.equal(sectionSwapPlan(secs, "zzz", "up"), null);
  assert.equal(sectionSwapPlan([], "a", "up"), null);
});

/* ---------- note-card reorder plan ---------- */
test("noteCardReorderPlan patches only changed orders", () => {
  const cards = [
    { id: "n1", order: 0, title: "A" },
    { id: "n2", order: 1, title: "B" },
    { id: "n3", order: 2, title: "C" },
  ];
  const plan = noteCardReorderPlan(cards, ["n3", "n1", "n2"]);
  assert.deepEqual(plan.cards.map(c => c.id), ["n3", "n1", "n2"]);
  assert.deepEqual(plan.patches, [
    { id: "n3", order: 0 },
    { id: "n1", order: 1 },
    { id: "n2", order: 2 },
  ]);
  const noop = noteCardReorderPlan(cards, ["n1", "n2", "n3"]);
  assert.deepEqual(noop.patches, []);
});

/* ---------- card lane dedupe / order ---------- */
test("cardLanesFromDoc dedupes first-seen lane colors in encounter order", () => {
  const doc = {
    sections: [
      {
        references: [
          { snapshot: { lane: "#f00" } },
          { snapshot: { lane: "#0f0" } },
          { snapshot: { lane: "#f00" } },
        ],
      },
      { references: [{ snapshot: { lane: "#00f" } }, { snapshot: {} }] },
    ],
  };
  assert.deepEqual(cardLanesFromDoc(doc), ["#f00", "#0f0", "#00f"]);
  assert.deepEqual(cardLanesFromDoc({ sections: [] }), []);
});

/* ---------- source / snapshot ---------- */
test("bookmarkSource durable fields and empties", () => {
  assert.deepEqual(bookmarkSource({
    uid: "u1", segment: 2, record: 3, node: "n1", turnTime: "T",
  }), { uid: "u1", segment: 2, record: 3, node: "n1", turnTime: "T" });
  assert.deepEqual(bookmarkSource({}), {
    uid: "", segment: 0, record: 0, node: "", turnTime: "",
  });
});

test("buildBookmarkSnapshot: self-contained; comment inherits parent lane", () => {
  const bookmarks = [
    { t: "1", lane: "lane-a", text: "parent" },
    { t: "2", anchor: "1", text: "comment", role: "assistant", turnTime: "TT" },
  ];
  const snap = buildBookmarkSnapshot(bookmarks[1], {
    bookmarks,
    nodeById: () => null,
    laneColor: id => (id ? "#c-" + id : ""),
    bookmarkLaneId,
  });
  assert.equal(snap.lane, "#c-lane-a");
  assert.equal(snap.speaker, "agent");
  assert.equal(snap.time, "TT");
  assert.equal(snap.text, "comment");
  assert.equal(snap.station, "");

  const withNode = buildBookmarkSnapshot(
    { t: "3", node: "n1", text: "hi", role: "user", lane: "lane-a" },
    {
      bookmarks: [{ t: "3", node: "n1", text: "hi", role: "user", lane: "lane-a" }],
      nodeById: id => (id === "n1" ? { id: "n1", title: "Alpha", lane_id: "lane-a" } : null),
      laneColor: id => (id === "lane-a" ? "#abc" : ""),
      bookmarkLaneId,
    },
  );
  assert.equal(withNode.station, "Alpha");
  assert.equal(withNode.speaker, "you");
  assert.equal(withNode.lane, "#abc");
});

/* ---------- save enqueue serialization ---------- */
test("createSaveEnqueue: same-field serializes edit then revert", async () => {
  const { enqueue } = createSaveEnqueue();
  const order = [];
  let resolveFirst;
  const first = enqueue("k", () => new Promise(r => {
    resolveFirst = () => { order.push("edit"); r(); };
  }));
  const revert = enqueue("k", () => { order.push("revert"); return Promise.resolve(); });
  /* first run is scheduled on a microtask via prev.then */
  await Promise.resolve();
  assert.equal(typeof resolveFirst, "function");
  assert.deepEqual(order, []);
  resolveFirst();
  await Promise.all([first, revert]);
  assert.deepEqual(order, ["edit", "revert"]);
});

test("createSaveEnqueue: independent fields do not block each other", async () => {
  const { enqueue } = createSaveEnqueue();
  const order = [];
  let resolveA;
  const a = enqueue("title", () => new Promise(r => {
    resolveA = () => { order.push("a"); r(); };
  }));
  const b = enqueue("body", () => { order.push("b"); return Promise.resolve(); });
  await b;
  assert.deepEqual(order, ["b"]);
  resolveA();
  await a;
  assert.deepEqual(order, ["b", "a"]);
});

/* ---------- HTML builders ---------- */
test("referenceHTML and sectionBodyInner preserve references", () => {
  const r = {
    id: "r1",
    snapshot: { lane: "#f00", station: "S", speaker: "you", time: "T1", text: "**hi**" },
  };
  const html = referenceHTML(r, {
    esc: s => s, md: s => `[md:${s}]`, fmtWhen: s => `W(${s})`,
    icons: { ICON_JUMP: "J", ICON_COPY: "C", ICON_TRASH: "T" },
  });
  assert.match(html, /data-ref="r1"/);
  assert.match(html, /data-refact="jump"/);
  assert.match(html, /data-refact="copy"/);
  assert.match(html, /data-refact="trash"/);
  assert.match(html, /S · you · W\(T1\)/);
  assert.match(html, /\[md:\*\*hi\*\*\]/);

  const body = sectionBodyInner(
    { body: "para", references: [r] },
    { esc: s => s, md: s => s, fmtWhen: s => s, icons: {} },
  );
  assert.match(body, /data-secrender/);
  assert.match(body, /class="wsrefs"/);
  assert.match(body, /data-ref="r1"/);
});

test("sectionHTML folded class and controls", () => {
  const html = sectionHTML({ id: "s1", title: "Sec", body: "x", references: [] }, true, {
    esc: s => s, md: s => s, fmtWhen: s => s,
    icons: { ICON_CHEV_RIGHT: "R", ICON_CHEV_DOWN: "D", ICON_PLUS: "+", ICON_MENU_DOTS: "M" },
  });
  assert.match(html, /class="wssec folded"/);
  assert.match(html, /data-sec="s1"/);
  assert.match(html, /data-addhere/);
  assert.match(html, /data-secmenu/);
  assert.match(html, />R</);
});

test("noteCardHTML active class, draggable, lanes", () => {
  const html = noteCardHTML(
    { id: "n1", title: "Hello", lanes: ["#f00", "#0f0"] },
    "n1",
    { esc: s => s, fmtNoteMeta: () => "1 section · now" },
  );
  assert.match(html, /class="wscard on"/);
  assert.match(html, /draggable="true"/);
  assert.match(html, /data-note="n1"/);
  assert.match(html, /wclanes/);
});

test("noteCardHTML carries Rename + Delete card actions; delete is danger", () => {
  const html = noteCardHTML(
    { id: "n1", title: "Hello", lanes: [] },
    "n1",
    { esc: s => s, fmtNoteMeta: () => "meta", icons: { ICON_PENCIL: "P", ICON_TRASH: "T" } },
  );
  assert.match(html, /class="wscardactions"/);
  assert.match(html, /data-wcact="rename"/);
  assert.match(html, /data-wcact="delete"/);
  assert.match(html, /data-wcact="delete"[^>]*class="[^"]*danger|class="[^"]*danger[^"]*"[^>]*data-wcact="delete"/);
  assert.match(html, /aria-label="rename note"/);
  assert.match(html, /aria-label="delete note"/);
  /* card must host buttons (not be a <button>) so nested actions stay valid HTML */
  assert.match(html, /<div class="wscard/);
  assert.doesNotMatch(html, /<button class="wscard/);
});

test("noteCardDragEnabled is false while renaming", () => {
  assert.equal(noteCardDragEnabled(false), true);
  assert.equal(noteCardDragEnabled(true), false);
});

test("noteCardRenamePlan: PATCH title; top bar only when active", () => {
  const active = noteCardRenamePlan({ noteId: "n1", activeId: "n1", title: "New" });
  assert.deepEqual(active.patch, { title: "New" });
  assert.equal(active.updateTopBar, true);
  assert.equal(active.topBarText, "New");
  assert.equal(active.updateActive, true);

  const other = noteCardRenamePlan({ noteId: "n2", activeId: "n1", title: "" });
  assert.deepEqual(other.patch, { title: "" });
  assert.equal(other.updateTopBar, false);
  assert.equal(other.topBarText, "Note");
  assert.equal(other.updateActive, false);
});

/* ---------- inbox lane / order / tabs / clamp reuse ---------- */
test("inbox lane tabs, resolve, list order, HTML", () => {
  const bms = [
    { t: "2", text: "later", lane: "lane-a" },
    { t: "1", text: "first", lane: "lane-a" },
    { t: "3", text: "gen" },
    { t: "4", anchor: "1", text: "comment" },
  ];
  const lanes = [{ id: "lane-a", name: "Alpha", color: "#f00" }];
  const nodeById = () => null;
  const { byT, laneOf, tabs } = inboxLaneTabs(bms, lanes, bookmarkLaneId, nodeById);
  assert.equal(tabs.length, 1);
  assert.equal(tabs[0].id, "lane-a");
  assert.equal(resolveInboxTab("missing", tabs), "GENERAL");
  assert.equal(resolveInboxTab("lane-a", tabs), "lane-a");

  const listA = inboxListModel(bms, "lane-a", laneOf, byT, bookmarkSortKey);
  assert.deepEqual(listA.map(n => n.t), ["1", "4", "2"]); /* comment after anchor */

  const th = inboxTabsHTML("GENERAL", tabs, s => s, id => "#f00");
  assert.match(th, /data-wsitab="GENERAL"[^>]*class="on"/);
  assert.match(th, /data-wsitab="lane-a"/);

  const item = inboxItemHTML({ t: "1", text: "hi", node: "n1" }, "var(--unlane)", {
    esc: s => s, md: s => s, fmtWhen: s => "W", nodeById: () => ({ title: "Alpha" }),
    icons: { ICON_CLIP: "L" },
  });
  assert.match(item, /data-wsiuse/);
  assert.match(item, /data-wsimore/);
  assert.match(item, /Alpha/);
});

test("bookmarkClampState reused at inbox/reference boundaries", () => {
  assert.deepEqual(bookmarkClampState(900, false), {
    clamped: true, showBtn: true, label: "Show more",
  });
  assert.deepEqual(bookmarkClampState(100, true), {
    clamped: false, showBtn: false, label: "Show less",
  });
});

/* ---------- swipe action applicator ---------- */
test("applyNotesSwipeAction maps decision types", () => {
  assert.equal(applyNotesSwipeAction(null), null);
  assert.equal(applyNotesSwipeAction({ type: "endPlacement" }), "endPlacement");
  assert.equal(applyNotesSwipeAction({ type: "noteToList" }), "noteToList");
  assert.equal(applyNotesSwipeAction({ type: "closeWorkspace" }), "closeWorkspace");
  assert.equal(applyNotesSwipeAction({ type: "other" }), null);
});

/* ---------- fake DOM helpers ---------- */
function classList(el){
  const set = new Set((el.className || "").split(/\s+/).filter(Boolean));
  return {
    contains: c => set.has(c),
    add: c => { set.add(c); el.className = [...set].join(" "); },
    remove: c => { set.delete(c); el.className = [...set].join(" "); },
    toggle: (c, force) => {
      if (force === true){ set.add(c); el.className = [...set].join(" "); return true; }
      if (force === false){ set.delete(c); el.className = [...set].join(" "); return false; }
      if (set.has(c)){ set.delete(c); el.className = [...set].join(" "); return false; }
      set.add(c); el.className = [...set].join(" "); return true;
    },
  };
}

function el(tag, props = {}){
  const listeners = {};
  const node = {
    tagName: tag.toUpperCase(),
    className: props.className || "",
    hidden: !!props.hidden,
    innerHTML: props.innerHTML || "",
    textContent: props.textContent || "",
    value: props.value || "",
    dataset: Object.assign({}, props.dataset || {}),
    style: {},
    children: [],
    parentNode: null,
    parentElement: null,
    nextSibling: null,
    scrollHeight: props.scrollHeight || 0,
    offsetHeight: props.offsetHeight || 0,
    clientHeight: props.clientHeight || 0,
    get classList(){ return classList(node); },
    addEventListener(type, fn){
      (listeners[type] || (listeners[type] = [])).push(fn);
    },
    removeEventListener(type, fn){
      if (!listeners[type]) return;
      listeners[type] = listeners[type].filter(f => f !== fn);
    },
    dispatch(type, ev = {}){
      const e = Object.assign({
        type, target: node, currentTarget: node, preventDefault(){}, stopPropagation(){},
      }, ev);
      if (!e.target) e.target = node;
      (listeners[type] || []).forEach(fn => fn(e));
    },
    focus(){ node._focused = true; documentRef.activeElement = node; },
    blur(){ node._focused = false; },
    select(){ node._selected = true; },
    click(){ node.dispatch("click"); },
    remove(){
      if (node.parentNode && node.parentNode.children){
        node.parentNode.children = node.parentNode.children.filter(c => c !== node);
      }
    },
    closest(sel){
      if (matchSel(node, sel)) return node;
      return node.parentNode && node.parentNode.closest
        ? node.parentNode.closest(sel) : null;
    },
    querySelector(sel){
      for (const c of walk(node)){
        if (matchSel(c, sel)) return c;
      }
      return null;
    },
    querySelectorAll(sel){
      return [...walk(node)].filter(c => matchSel(c, sel));
    },
    appendChild(child){
      child.parentNode = node;
      child.parentElement = node;
      node.children.push(child);
      return child;
    },
    insertBefore(child, ref){
      child.parentNode = node;
      child.parentElement = node;
      if (!ref){ node.children.push(child); return child; }
      const i = node.children.indexOf(ref);
      if (i < 0) node.children.push(child);
      else node.children.splice(i, 0, child);
      return child;
    },
    getBoundingClientRect(){
      return node._rect || { top: 0, left: 0, bottom: 20, height: 20, width: 100 };
    },
    setAttribute(k, v){ node[k] = v; },
    insertAdjacentHTML(pos, html){
      if (pos === "beforeend") node.innerHTML = (node.innerHTML || "") + html;
    },
    get _listeners(){ return listeners; },
  };
  if (props.id) node.id = props.id;
  return node;
}

function* walk(node){
  for (const c of node.children || []){
    yield c;
    yield* walk(c);
  }
}

function matchSel(node, sel){
  if (!sel) return false;
  if (sel.startsWith(".")) return (node.className || "").split(/\s+/).includes(sel.slice(1));
  if (sel.startsWith("#")) return node.id === sel.slice(1);
  if (sel.startsWith("[") && sel.endsWith("]")){
    const body = sel.slice(1, -1);
    if (body.startsWith("data-")){
      const [k, v] = body.split("=");
      const key = k.replace(/^data-/, "").replace(/-([a-z])/g, (_, c) => c.toUpperCase());
      /* dataset keys in HTML are camelCase of data-foo-bar → fooBar; our dataset uses short keys */
      const dk = k.replace(/^data-/, "");
      const camel = dk.replace(/-([a-z])/g, (_, c) => c.toUpperCase());
      const val = node.dataset[dk] ?? node.dataset[camel] ?? node.dataset[key];
      if (v == null) return val != null || node[k] != null;
      const want = v.replace(/^"|"$/g, "");
      return String(val) === want;
    }
  }
  if (sel.includes(".")){
    const [tag, cls] = sel.split(".");
    return node.tagName === tag.toUpperCase() && (node.className || "").includes(cls);
  }
  if (sel.includes("[")){
    const m = sel.match(/^([a-z0-9]+)(\[.+\])$/i);
    if (m) return matchSel(node, m[1]) && matchSel(node, m[2]);
  }
  return node.tagName === sel.toUpperCase();
}

let documentRef = { activeElement: null, contains: () => true };

function makeStorage(init = {}){
  const m = new Map(Object.entries(init));
  return {
    getItem: k => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => { m.set(k, String(v)); },
    removeItem: k => { m.delete(k); },
    _map: m,
  };
}

function makeRoots(){
  const notesworkspace = el("div", { id: "notesworkspace", hidden: true });
  const wsscrim = el("div", { id: "wsscrim" });
  const wspanel = el("div", { id: "wspanel" });
  const wstitle = el("h2", { id: "wstitle", textContent: "Notes" });
  const wsback = el("button", { id: "wsback", hidden: true });
  const wsclose = el("button", { id: "wsclose" });
  const wsplacetext = el("span", { id: "wsplacetext" });
  const wsplacecancel = el("button", { id: "wsplacecancel" });
  const wsinboxtabs = el("div", { id: "wsinboxtabs" });
  const wsinboxlist = el("div", { id: "wsinboxlist" });
  const wscards = el("div", { id: "wscards" });
  const wsnewnote = el("button", { id: "wsnewnote" });
  const wsnotehead = el("div", { id: "wsnotehead", hidden: true });
  const wsnotetitle = el("input", { id: "wsnotetitle" });
  const wsnotemenu = el("button", { id: "wsnotemenu" });
  const wssections = el("div", { id: "wssections" });
  const wsnoteempty = el("div", { id: "wsnoteempty" });
  const notesbtn = el("button", { id: "notesbtn" });

  notesworkspace.appendChild(wsscrim);
  notesworkspace.appendChild(wspanel);
  wspanel.appendChild(wstitle);
  wspanel.appendChild(wsback);
  wspanel.appendChild(wsclose);
  wspanel.appendChild(wsplacetext);
  wspanel.appendChild(wsplacecancel);
  wspanel.appendChild(wsinboxtabs);
  wspanel.appendChild(wsinboxlist);
  wspanel.appendChild(wscards);
  wspanel.appendChild(wsnewnote);
  wspanel.appendChild(wsnotehead);
  wspanel.appendChild(wsnotetitle);
  wspanel.appendChild(wsnotemenu);
  wspanel.appendChild(wssections);
  wspanel.appendChild(wsnoteempty);

  documentRef = {
    activeElement: null,
    contains: n => !!n,
    createElement: tag => el(tag),
    querySelector: () => null,
    getElementById: () => null,
    addEventListener: (...a) => { /* filled below */ },
    removeEventListener: (...a) => {},
    _listeners: {},
  };
  documentRef.addEventListener = (type, fn, opts) => {
    (documentRef._listeners[type] || (documentRef._listeners[type] = [])).push({ fn, opts });
  };
  documentRef.removeEventListener = (type, fn) => {
    if (!documentRef._listeners[type]) return;
    documentRef._listeners[type] = documentRef._listeners[type].filter(x => x.fn !== fn);
  };

  return {
    roots: {
      notesworkspace, wsscrim, wspanel, wstitle, wsback, wsclose,
      wsplacetext, wsplacecancel, wsinboxtabs, wsinboxlist, wscards,
      wsnewnote, wsnotehead, wsnotetitle, wsnotemenu, wssections, wsnoteempty,
    },
    document: documentRef,
    notesbtn,
  };
}

function createFeature(overrides = {}){
  const { roots, document, notesbtn } = makeRoots();
  const storage = makeStorage(overrides.storageInit || {});
  const timers = [];
  let now = 0;
  const setTimeoutFn = (fn, ms) => {
    const id = { fn, at: now + (ms || 0), cleared: false };
    timers.push(id);
    return id;
  };
  const clearTimeoutFn = id => { if (id) id.cleared = true; };
  const flush = ms => {
    now += ms || 0;
    const due = timers.filter(t => !t.cleared && t.at <= now);
    for (const t of due){
      t.cleared = true;
      t.fn();
    }
  };

  const apiLog = [];
  let notes = overrides.notes || [];
  const docs = overrides.docs || {};
  const api = async (url, opts = {}) => {
    apiLog.push({ url, method: opts.method || "GET", body: opts.body });
    if (typeof overrides.api === "function") return overrides.api(url, opts, { notes, docs, apiLog });
    if (url === "/api/notes" && (!opts.method || opts.method === "GET"))
      return { notes };
    if (url === "/api/notes" && opts.method === "POST"){
      const sh = { id: "new1", title: "Untitled", order: notes.length, sections: [] };
      notes = [...notes, sh];
      docs[sh.id] = { ...sh, sections: [{ id: "s1", title: "", body: "", order: 0, references: [] }] };
      return sh;
    }
    const m = url.match(/^\/api\/notes\/([^/]+)$/);
    if (m && (!opts.method || opts.method === "GET")){
      if (overrides.failOpen) throw new Error("open fail");
      return docs[decodeURIComponent(m[1])] || {
        id: decodeURIComponent(m[1]), title: "Note", sections: [],
      };
    }
    if (m && opts.method === "PATCH"){
      const id = decodeURIComponent(m[1]);
      const body = opts.body ? JSON.parse(opts.body) : {};
      const doc = docs[id] || { id, title: "Note", sections: [] };
      if (body.title != null) doc.title = body.title;
      if (body.order != null){
        const card = notes.find(c => c.id === id);
        if (card) card.order = body.order;
      }
      if (body.add_section != null){
        doc.sections = doc.sections || [];
        doc.sections.push({ id: "s" + (doc.sections.length + 1), title: "", body: "", order: doc.sections.length, references: [] });
      }
      if (body.section){
        const sec = (doc.sections || []).find(s => s.id === body.section.id);
        if (body.section.delete) doc.sections = (doc.sections || []).filter(s => s.id !== body.section.id);
        else if (sec){
          if (body.section.title != null) sec.title = body.section.title;
          if (body.section.body != null) sec.body = body.section.body;
          if (body.section.order != null) sec.order = body.section.order;
        }
      }
      doc.edited_at = "E" + apiLog.length;
      docs[id] = doc;
      const card = notes.find(c => c.id === id);
      if (card){
        card.title = doc.title;
        card.edited_at = doc.edited_at;
        card.section_count = (doc.sections || []).length;
      }
      return doc;
    }
    if (m && opts.method === "DELETE"){
      notes = notes.filter(c => c.id !== decodeURIComponent(m[1]));
      return {};
    }
    const refPost = url.match(/^\/api\/notes\/([^/]+)\/sections\/([^/]+)\/references$/);
    if (refPost && opts.method === "POST"){
      const id = decodeURIComponent(refPost[1]);
      const sid = decodeURIComponent(refPost[2]);
      const doc = docs[id];
      const sec = doc.sections.find(s => s.id === sid);
      const body = JSON.parse(opts.body);
      sec.references = sec.references || [];
      sec.references.push({ id: "r" + sec.references.length, source: body.source, snapshot: body.snapshot });
      docs[id] = doc;
      return doc;
    }
    const refDel = url.match(/^\/api\/notes\/([^/]+)\/sections\/([^/]+)\/references\/([^/]+)$/);
    if (refDel && opts.method === "DELETE"){
      const id = decodeURIComponent(refDel[1]);
      const sid = decodeURIComponent(refDel[2]);
      const rid = decodeURIComponent(refDel[3]);
      const doc = docs[id];
      const sec = doc.sections.find(s => s.id === sid);
      sec.references = (sec.references || []).filter(r => r.id !== rid);
      return doc;
    }
    return null;
  };

  let bms = overrides.bookmarks || [];
  const effects = {
    toast: [],
    copy: [],
    jump: [],
  };

  const feature = createNotesFeature({
    roots,
    document,
    storage,
    setTimeout: setTimeoutFn,
    clearTimeout: clearTimeoutFn,
    requestAnimationFrame: fn => setTimeoutFn(fn, 0),
    CSS: { escape: s => s },
    esc: s => String(s ?? "").replace(/[<>&"]/g, c => ({
      "<": "&lt;", ">": "&gt;", "&": "&amp;", '"': "&quot;",
    }[c])),
    md: s => String(s ?? ""),
    fmtWhen: s => "W(" + s + ")",
    fmtNoteMeta: c => `${c.section_count || 0} sections`,
    hashStr: s => {
      let h = 0;
      for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) | 0;
      return String(h);
    },
    icons: {
      ICON_PLUS: "+", ICON_MENU_DOTS: "M", ICON_CLIP: "L",
      ICON_JUMP: "J", ICON_COPY: "C", ICON_TRASH: "T",
      ICON_CHEV_RIGHT: "R", ICON_CHEV_DOWN: "D",
      ICON_PENCIL: "P", ICON_MOVE_UP: "U", ICON_MOVE_DOWN: "N",
    },
    api,
    bookmarks: () => bms,
    nodeById: id => (overrides.nodes || {})[id] || null,
    laneList: () => overrides.lanes || [],
    laneColor: id => ((overrides.lanes || []).find(l => l.id === id) || {}).color || "",
    toast: msg => effects.toast.push(msg),
    copyText: s => effects.copy.push(s),
    jumpToChatAddress: a => {
      effects.jump.push(a);
      return overrides.jumpOk !== false;
    },
    isNarrow: () => !!overrides.isNarrow,
    notesbtn: () => notesbtn,
    activeElement: () => document.activeElement,
    ...overrides.deps,
  });

  return {
    feature, roots, document, notesbtn, storage, apiLog, effects, timers, flush,
    setBookmarks(next){ bms = next; },
    setNotes(next){ notes = next; },
    docs,
  };
}

/* ---------- open/close focus ---------- */
test("open/close: focus return and notesbtn fallback", () => {
  const ctx = createFeature();
  const { feature, roots, notesbtn, document } = ctx;
  feature.bind();
  const prior = el("button");
  document.activeElement = prior;
  feature.open();
  assert.equal(feature.isOpen(), true);
  assert.equal(roots.notesworkspace.hidden, false);
  ctx.flush(0); /* rAF focus new note */
  assert.equal(roots.wsnewnote._focused, true);

  feature.close();
  assert.equal(feature.isOpen(), false);
  assert.equal(prior._focused, true);

  document.activeElement = null;
  /* second open with no prior focus → close focuses notesbtn */
  document.contains = () => false;
  feature.open();
  feature.close();
  assert.equal(notesbtn._focused, true);
});

test("open is idempotent when already open", () => {
  const ctx = createFeature();
  const { feature, roots } = ctx;
  feature.bind();
  feature.open();
  ctx.flush(0); /* drain first open's rAF focus */
  const focusCalls = [];
  roots.wsnewnote.focus = () => focusCalls.push(1);
  feature.open();
  ctx.flush(0);
  assert.equal(focusCalls.length, 0);
});

/* ---------- debounce / flush / serialize via factory ---------- */
async function settle(n = 8){
  for (let i = 0; i < n; i++) await Promise.resolve();
}

async function openNote(ctx, id){
  const card = el("button", { className: "wscard", dataset: { note: id } });
  card.dataset.note = id;
  card.closest = sel => (sel === ".wscard" ? card : null);
  ctx.roots.wscards.dispatch("click", { target: card });
  await settle();
}

test("save debounce 1100ms, same-field serialize, close flush", async () => {
  const ctx = createFeature({
    notes: [{ id: "n1", title: "T", order: 0 }],
    docs: {
      n1: { id: "n1", title: "T", sections: [{ id: "s1", title: "", body: "b", order: 0, references: [] }] },
    },
  });
  const { feature, roots, apiLog, flush } = ctx;
  feature.bind();
  feature.open();
  await settle();
  await openNote(ctx, "n1");
  assert.equal(roots.wsnotetitle.value, "T");

  roots.wsnotetitle.dispatch("focus");
  roots.wsnotetitle.value = "A";
  roots.wsnotetitle.dispatch("input", { target: roots.wsnotetitle });
  roots.wsnotetitle.value = "B";
  roots.wsnotetitle.dispatch("input", { target: roots.wsnotetitle });

  const patchesBefore = apiLog.filter(x => x.method === "PATCH");
  assert.equal(patchesBefore.length, 0, "debounced — no PATCH yet");

  flush(1100);
  await settle();
  const afterDebounce = apiLog.filter(x => x.method === "PATCH" && x.body && x.body.includes('"title"'));
  assert.ok(afterDebounce.length >= 1);
  const last = JSON.parse(afterDebounce[afterDebounce.length - 1].body);
  assert.equal(last.title, "B", "last debounced value wins");

  /* pending at close: type again and close before debounce */
  roots.wsnotetitle.value = "C";
  roots.wsnotetitle.dispatch("input", { target: roots.wsnotetitle });
  const beforeClose = apiLog.length;
  feature.close();
  await settle();
  const flushed = apiLog.slice(beforeClose).filter(x => x.method === "PATCH");
  assert.ok(flushed.length >= 1, "close flushes pending save");
  assert.equal(JSON.parse(flushed[flushed.length - 1].body).title, "C");
});

test("Escape title revert enqueues after in-flight via PatchNow path", async () => {
  const pending = [];
  const ctx = createFeature({
    notes: [{ id: "n1", title: "Orig", order: 0 }],
    docs: {
      n1: { id: "n1", title: "Orig", sections: [] },
    },
    api: async (url, opts) => {
      if (opts && opts.method === "PATCH"){
        return new Promise(r => {
          pending.push(() => {
            const body = JSON.parse(opts.body);
            r({ id: "n1", title: body.title, sections: [], edited_at: "e" });
          });
        });
      }
      if (url === "/api/notes") return { notes: [{ id: "n1", title: "Orig", order: 0 }] };
      if (url.includes("/api/notes/n1") && (!opts || !opts.method || opts.method === "GET"))
        return { id: "n1", title: "Orig", sections: [] };
      return null;
    },
  });
  const { feature, roots, flush } = ctx;
  feature.bind();
  feature.open();
  await settle();
  await openNote(ctx, "n1");
  assert.equal(roots.wsnotetitle.value, "Orig");

  roots.wsnotetitle.dispatch("focus");
  roots.wsnotetitle.value = "Edited";
  roots.wsnotetitle.dispatch("input", { target: roots.wsnotetitle });
  flush(1100);
  await settle();
  assert.equal(pending.length, 1, "in-flight edit");

  roots.wsnotetitle.dispatch("keydown", {
    key: "Escape", target: roots.wsnotetitle,
    preventDefault(){}, stopPropagation(){},
  });
  assert.equal(roots.wsnotetitle.value, "Orig");
  pending[0]();
  await settle();
  assert.ok(pending.length >= 2, "revert enqueued after in-flight");
  pending[1]();
  await settle();
});

/* ---------- note CRUD ---------- */
test("create note success path", async () => {
  const ctx = createFeature({ notes: [] });
  const { feature, roots, apiLog } = ctx;
  feature.bind();
  feature.open();
  await settle();
  assert.match(roots.wscards.innerHTML, /No notes yet/);
  roots.wsnewnote.dispatch("click");
  await settle(12);
  assert.ok(apiLog.some(x => x.url === "/api/notes" && x.method === "POST"));
  assert.equal(feature.isOpen(), true);
  assert.ok(roots.notesworkspace.classList.contains("note-open"));
});

test("create note error toasts", async () => {
  const ctx = createFeature({
    api: async () => { throw new Error("fail"); },
  });
  const { feature, roots, effects } = ctx;
  feature.bind();
  feature.open();
  roots.wsnewnote.dispatch("click");
  await settle(12);
  assert.ok(effects.toast.includes("Couldn't create note"));
});

test("delete note success clears editor", async () => {
  const ctx = createFeature({
    notes: [{ id: "n1", title: "T", order: 0 }],
    docs: { n1: { id: "n1", title: "T", sections: [] } },
  });
  const { feature, roots, apiLog } = ctx;
  feature.bind();
  feature.open();
  await settle();
  const card = el("button", { className: "wscard", dataset: { note: "n1" } });
  card.dataset.note = "n1";
  card.closest = sel => (sel === ".wscard" ? card : null);
  roots.wscards.dispatch("click", { target: card });
  await Promise.resolve();
  assert.ok(roots.notesworkspace.classList.contains("note-open"));

  /* card Delete → archive-aware confirm → ok */
  const delBtn = el("button", { className: "danger", dataset: { wcact: "delete" } });
  delBtn.dataset.wcact = "delete";
  delBtn.closest = sel => {
    if (sel === "[data-wcact]" || sel.includes("data-wcact")) return delBtn;
    if (sel === ".wscard") return card;
    return null;
  };
  delBtn.getBoundingClientRect = () => ({ top: 10, left: 10, bottom: 50, height: 40, width: 44 });
  roots.wspanel.getBoundingClientRect = () => ({ top: 0, left: 0, width: 400, height: 600 });
  roots.wscards.dispatch("click", {
    target: delBtn, stopPropagation(){}, preventDefault(){},
  });
  await settle();
  assert.equal(apiLog.some(x => x.method === "DELETE"), false, "await confirm");
  const confirm = roots.wspanel.children.find(c => (c.className || "").includes("wsmenu"));
  assert.ok(confirm);
  const ok = el("button", { dataset: { wconfirm: "ok" } });
  ok.dataset.wconfirm = "ok";
  ok.closest = sel => (sel === "[data-wconfirm]" || sel.includes("data-wconfirm") ? ok : null);
  confirm.dispatch("click", { target: ok });
  await settle();
  assert.ok(apiLog.some(x => x.method === "DELETE"));
  assert.equal(roots.notesworkspace.classList.contains("note-open"), false);
});

/* ---------- card drag suppresses rebuild ---------- */
test("renderCards skips while dragging", async () => {
  const ctx = createFeature({
    notes: [
      { id: "n1", title: "A", order: 0 },
      { id: "n2", title: "B", order: 1 },
    ],
  });
  const { feature, roots } = ctx;
  feature.bind();
  feature.open();
  await settle();
  const before = roots.wscards.innerHTML;
  const card = el("button", { className: "wscard", dataset: { note: "n1" } });
  card.dataset.note = "n1";
  card.closest = sel => (sel === ".wscard" ? card : null);
  roots.wscards.dispatch("dragstart", { target: card, dataTransfer: { setData(){}, effectAllowed: "" } });
  roots.wscards.innerHTML = "DIRTY";
  feature.close();
  feature.open(); /* starts the production card load while the drag guard is set */
  await settle();
  assert.equal(roots.wscards.innerHTML, "DIRTY", "mid-drag load must not rebuild");
  roots.wscards.dispatch("dragend");
  await Promise.resolve();
  await Promise.resolve();
  assert.notEqual(roots.wscards.innerHTML, "DIRTY");
  assert.ok(before.includes("wscard") || roots.wscards.innerHTML.includes("wscard") || roots.wscards.innerHTML.includes("A"));
});

/* ---------- inbox render + placement ---------- */
test("inbox tabs/list render; use-in-note starts placement", async () => {
  const ctx = createFeature({
    bookmarks: [
      { t: "1", text: "hello bookmark" }, /* General lane (no lane id) */
      { t: "2", text: "laned", lane: "lane-a" },
    ],
    lanes: [{ id: "lane-a", name: "Alpha", color: "#f00" }],
  });
  const { feature, roots } = ctx;
  feature.bind();
  feature.open();
  await settle();
  assert.match(roots.wsinboxtabs.innerHTML, /data-wsitab="GENERAL"/);
  assert.match(roots.wsinboxtabs.innerHTML, /data-wsitab="lane-a"/);
  assert.match(roots.wsinboxlist.innerHTML, /hello bookmark/);
  assert.match(roots.wsinboxlist.innerHTML, /data-wsiuse/);

  const use = el("button");
  use.dataset.wsiuse = "";
  const row = el("div", { className: "wsibookmark", dataset: { t: "1" } });
  row.dataset.t = "1";
  use.closest = sel => {
    if (sel === "[data-wsiuse]") return use;
    if (sel === ".wsibookmark") return row;
    return null;
  };
  roots.wsinboxlist.dispatch("click", { target: use });
  assert.ok(roots.notesworkspace.classList.contains("placing"));
  assert.equal(roots.wsplacetext.textContent, "hello bookmark");
});

test("placement payload uses bookmarkSource + snapshot; targeted path available", async () => {
  const ctx = createFeature({
    notes: [{ id: "n1", title: "Note", order: 0 }],
    docs: {
      n1: {
        id: "n1", title: "Note",
        sections: [{ id: "s1", title: "S", body: "body", order: 0, references: [] }],
      },
    },
    bookmarks: [{ t: "1", text: "cap", uid: "u1", segment: 1, record: 2, node: "nX", turnTime: "TT", lane: "lane-a" }],
    lanes: [{ id: "lane-a", name: "A", color: "#f00" }],
    nodes: { nX: { id: "nX", title: "Station", lane_id: "lane-a" } },
  });
  const { feature, roots, apiLog, effects } = ctx;
  feature.bind();
  feature.open();
  await settle();
  await openNote(ctx, "n1");

  feature.startPlacement({
    t: "1", text: "cap", uid: "u1", segment: 1, record: 2, node: "nX", turnTime: "TT", lane: "lane-a",
  });
  /* place via addhere */
  const add = el("button");
  add.dataset.addhere = "";
  const sec = el("div", { className: "wssec", dataset: { sec: "s1" } });
  sec.dataset.sec = "s1";
  const body = el("div", { className: "wssecbody" });
  sec.appendChild(body);
  add.closest = sel => {
    if (sel === "[data-addhere]") return add;
    if (sel === ".wssec") return sec;
    return null;
  };
  roots.wssections.dispatch("click", { target: add });
  await settle();
  const post = apiLog.find(x => x.method === "POST" && x.url.includes("/references"));
  assert.ok(post);
  const payload = JSON.parse(post.body);
  assert.deepEqual(payload.source, {
    uid: "u1", segment: 1, record: 2, node: "nX", turnTime: "TT",
  });
  assert.equal(payload.snapshot.text, "cap");
  assert.equal(payload.snapshot.station, "Station");
  assert.equal(payload.snapshot.lane, "#f00");
  assert.ok(effects.toast.some(t => t.startsWith("Added to")));
  assert.equal(roots.notesworkspace.classList.contains("placing"), false);
});

/* ---------- reference copy / jump / trash ---------- */
test("reference copy, jump, and in-place trash", async () => {
  const ctx = createFeature({
    notes: [{ id: "n1", title: "N", order: 0 }],
    docs: {
      n1: {
        id: "n1", title: "N",
        sections: [{
          id: "s1", title: "", body: "x", order: 0,
          references: [{
            id: "r1",
            source: { node: "nX", uid: "u", segment: 0, record: 0 },
            snapshot: { text: "snap text", lane: "#f00" },
          }],
        }],
      },
    },
  });
  const { feature, roots, effects, apiLog, flush } = ctx;
  feature.bind();
  feature.open();
  await settle();
  const card = el("button", { className: "wscard", dataset: { note: "n1" } });
  card.dataset.note = "n1";
  card.closest = sel => (sel === ".wscard" ? card : null);
  roots.wscards.dispatch("click", { target: card });
  await Promise.resolve();

  /* build minimal DOM for actions */
  const sec = el("div", { className: "wssec", dataset: { sec: "s1" } });
  sec.dataset.sec = "s1";
  const ref = el("div", { className: "wsref", dataset: { ref: "r1" } });
  ref.dataset.ref = "r1";
  const wrap = el("div", { className: "wsrefs" });
  wrap.appendChild(ref);
  sec.appendChild(wrap);
  roots.wssections.appendChild(sec);

  const copyBtn = el("button", { dataset: { refact: "copy" } });
  copyBtn.dataset.refact = "copy";
  copyBtn.closest = sel => {
    if (sel === "[data-refact]") return copyBtn;
    if (sel === ".wsref") return ref;
    if (sel === ".wssec") return sec;
    return null;
  };
  roots.wssections.dispatch("click", { target: copyBtn });
  assert.deepEqual(effects.copy, ["snap text"]);
  flush(900);

  const jumpBtn = el("button", { dataset: { refact: "jump" } });
  jumpBtn.dataset.refact = "jump";
  jumpBtn.closest = sel => {
    if (sel === "[data-refact]") return jumpBtn;
    if (sel === ".wsref") return ref;
    if (sel === ".wssec") return sec;
    return null;
  };
  roots.wssections.dispatch("click", { target: jumpBtn });
  assert.equal(effects.jump.length, 1);
  assert.equal(effects.jump[0].uid, "u");

  const trashBtn = el("button", { dataset: { refact: "trash" } });
  trashBtn.dataset.refact = "trash";
  trashBtn.closest = sel => {
    if (sel === "[data-refact]") return trashBtn;
    if (sel === ".wsref") return ref;
    if (sel === ".wssec") return sec;
    return null;
  };
  let removed = false;
  ref.remove = () => { removed = true; wrap.children = []; };
  roots.wssections.dispatch("click", { target: trashBtn });
  await Promise.resolve();
  await Promise.resolve();
  assert.ok(apiLog.some(x => x.method === "DELETE" && x.url.includes("/references/r1")));
  assert.equal(removed, true);
});

/* ---------- narrow swipe ---------- */
test("narrow swipe applies endPlacement / noteToList / close", () => {
  const ctx = createFeature({ isNarrow: true });
  const { feature, roots } = ctx;
  feature.bind();
  feature.open();
  feature.startPlacement({ t: "1", text: "x" });
  assert.ok(roots.notesworkspace.classList.contains("placing"));

  roots.notesworkspace.dispatch("touchstart", {
    touches: [{ clientX: 10, clientY: 100 }],
    target: roots.wscards,
  });
  roots.notesworkspace.dispatch("touchend", {
    changedTouches: [{ clientX: 100, clientY: 100 }],
  });
  assert.equal(roots.notesworkspace.classList.contains("placing"), false);

  roots.notesworkspace.classList.add("note-open");
  roots.wsback.hidden = false;
  let backClicks = 0;
  roots.wsback.click = () => { backClicks++; roots.wsback.dispatch("click"); };
  roots.notesworkspace.dispatch("touchstart", {
    touches: [{ clientX: 10, clientY: 100 }],
    target: roots.wscards,
  });
  roots.notesworkspace.dispatch("touchend", {
    changedTouches: [{ clientX: 100, clientY: 100 }],
  });
  assert.equal(backClicks, 1);
  assert.equal(roots.notesworkspace.classList.contains("note-open"), false);

  roots.notesworkspace.dispatch("touchstart", {
    touches: [{ clientX: 10, clientY: 100 }],
    target: roots.wscards,
  });
  roots.notesworkspace.dispatch("touchend", {
    changedTouches: [{ clientX: 100, clientY: 100 }],
  });
  assert.equal(feature.isOpen(), false);
});

/* ---------- bind/destroy ---------- */
test("bind is idempotent; destroy cleans document listener and timers", async () => {
  const ctx = createFeature({
    notes: [{ id: "n1", title: "T", order: 0 }],
    docs: { n1: { id: "n1", title: "T", sections: [] } },
  });
  const { feature, document, roots, timers, flush } = ctx;
  feature.bind();
  feature.bind();
  assert.equal((roots.wsclose._listeners.click || []).length, 1);
  assert.equal((document._listeners.click || []).length, 1);

  feature.open();
  flush(0); /* drain open rAF so only save timers remain pending later */
  await settle();
  await openNote(ctx, "n1");
  const timerCountBefore = timers.length;
  roots.wsnotetitle.dispatch("focus");
  roots.wsnotetitle.value = "X";
  roots.wsnotetitle.dispatch("input", { target: roots.wsnotetitle });
  const saveTimers = timers.slice(timerCountBefore).filter(t => !t.cleared);
  assert.ok(saveTimers.length >= 1, "debounce timer scheduled");
  feature.destroy();
  assert.equal((document._listeners.click || []).length, 0);
  assert.equal((roots.wsclose._listeners.click || []).length, 0);
  assert.ok(saveTimers.every(t => t.cleared), "destroy clears save timers");
});

/* ---------- fold storage with error semantics ---------- */
test("fold storage get/set errors are swallowed", async () => {
  const ctx = createFeature({
    notes: [{ id: "n1", title: "T", order: 0 }],
    docs: {
      n1: {
        id: "n1", title: "T",
        sections: [{ id: "s1", title: "S", body: "", order: 0, references: [] }],
      },
    },
  });
  const { feature, roots, storage } = ctx;
  feature.bind();
  feature.open();
  await settle();
  const card = el("button", { className: "wscard", dataset: { note: "n1" } });
  card.dataset.note = "n1";
  card.closest = sel => (sel === ".wscard" ? card : null);
  roots.wscards.dispatch("click", { target: card });
  await Promise.resolve();

  /* inject section DOM */
  const sec = el("div", { className: "wssec", dataset: { sec: "s1" } });
  sec.dataset.sec = "s1";
  const foldBtn = el("button");
  foldBtn.dataset.secfold = "";
  foldBtn.closest = sel => {
    if (sel === "[data-secfold]") return foldBtn;
    if (sel === ".wssec") return sec;
    return null;
  };
  roots.wssections.appendChild(sec);
  roots.wssections.dispatch("click", { target: foldBtn });
  assert.equal(storage.getItem(STORAGE_KEY_FOLDS), JSON.stringify({ s1: 1 }));

  let threw = false;
  storage.setItem = () => { threw = true; throw new Error("quota"); };
  roots.wssections.dispatch("click", { target: foldBtn });
  assert.equal(threw, true, "setItem was attempted");

  storage.getItem = () => { throw new Error("private mode"); };
  assert.doesNotThrow(() => roots.wssections.dispatch("click", { target: foldBtn }));
});

/* ---------- menu stopPropagation contract (note menu) ---------- */
test("note menu stopPropagation and offers rename/add/delete", () => {
  const ctx = createFeature();
  const { feature, roots } = ctx;
  feature.bind();
  let stopped = false;
  roots.wsnotemenu.dispatch("click", {
    target: roots.wsnotemenu,
    currentTarget: roots.wsnotemenu,
    stopPropagation(){ stopped = true; },
  });
  assert.equal(stopped, true);
  const menu = roots.wspanel.children.find(c => (c.className || "").includes("wsmenu"));
  assert.ok(menu);
  assert.match(menu.innerHTML, /data-si="rename"/);
  assert.match(menu.innerHTML, /data-si="add"/);
  assert.match(menu.innerHTML, /data-si="del"/);
  assert.match(menu.innerHTML, /Delete note/);
});

/* ---------- Phase 1c: Rename / Delete on zone-2 note cards ---------- */
test("card rename: not draggable while editing; commit PATCHes title and updates #wstitle", async () => {
  const ctx = createFeature({
    notes: [{ id: "n1", title: "Orig", order: 0 }],
    docs: { n1: { id: "n1", title: "Orig", sections: [] } },
  });
  const { feature, roots, apiLog } = ctx;
  feature.bind();
  feature.open();
  await settle();
  await openNote(ctx, "n1");
  assert.equal(roots.wstitle.textContent, "Orig");

  /* build a card with actions the way renderCards would (fake DOM has no HTML parse) */
  const card = el("div", { className: "wscard", dataset: { note: "n1" }, draggable: true });
  card.dataset.note = "n1";
  card.draggable = true;
  const title = el("div", { className: "wctitle", textContent: "Orig" });
  card.appendChild(title);
  const renameBtn = el("button", { dataset: { wcact: "rename" } });
  renameBtn.dataset.wcact = "rename";
  renameBtn.closest = sel => {
    if (sel === "[data-wcact]" || sel === "[data-wcact=\"rename\"]") return renameBtn;
    if (sel === ".wscard") return card;
    return null;
  };
  card.closest = sel => (sel === ".wscard" ? card : null);
  roots.wscards.appendChild(card);

  let stopped = false;
  roots.wscards.dispatch("click", {
    target: renameBtn,
    stopPropagation(){ stopped = true; },
    preventDefault(){},
  });
  assert.equal(stopped, true, "action click must not select/reorder");
  assert.equal(card.draggable, false, "card is not draggable while renaming");
  assert.equal(noteCardDragEnabled(true), false);

  const input = card.querySelector("input") || card.querySelector(".wctitleedit");
  assert.ok(input, "rename swaps title for an input");
  input.value = "Renamed";
  input.dispatch("keydown", {
    key: "Enter", target: input,
    preventDefault(){}, stopPropagation(){},
  });
  await settle();

  const patch = apiLog.find(x => x.method === "PATCH" && x.url.includes("/api/notes/n1"));
  assert.ok(patch, "rename commits a PATCH");
  assert.equal(JSON.parse(patch.body).title, "Renamed");
  assert.equal(roots.wstitle.textContent, "Renamed");
});

test("card delete opens archive-aware confirm; cancel skips DELETE", async () => {
  const ctx = createFeature({
    notes: [
      { id: "n1", title: "Keep", order: 0 },
      { id: "n2", title: "Gone", order: 1 },
    ],
    docs: {
      n1: { id: "n1", title: "Keep", sections: [] },
      n2: { id: "n2", title: "Gone", sections: [] },
    },
  });
  const { feature, roots, apiLog } = ctx;
  feature.bind();
  feature.open();
  await settle();
  await openNote(ctx, "n1");

  const card = el("div", { className: "wscard", dataset: { note: "n2" } });
  card.dataset.note = "n2";
  const delBtn = el("button", { className: "danger", dataset: { wcact: "delete" } });
  delBtn.dataset.wcact = "delete";
  delBtn.closest = sel => {
    if (sel === "[data-wcact]" || sel.includes("data-wcact")) return delBtn;
    if (sel === ".wscard") return card;
    return null;
  };
  delBtn.getBoundingClientRect = () => ({ top: 10, left: 10, bottom: 50, height: 40, width: 44 });
  card.closest = sel => (sel === ".wscard" ? card : null);
  roots.wscards.appendChild(card);
  roots.wspanel.getBoundingClientRect = () => ({ top: 0, left: 0, width: 400, height: 600 });

  roots.wscards.dispatch("click", {
    target: delBtn,
    stopPropagation(){},
    preventDefault(){},
  });
  await settle();

  assert.equal(
    apiLog.some(x => x.method === "DELETE"),
    false,
    "delete must not fire before confirm",
  );
  const menu = roots.wspanel.children.find(c => (c.className || "").includes("wsmenu"));
  assert.ok(menu, "confirm popover opens");
  assert.match(menu.innerHTML, /archive/i);
  assert.match(menu.innerHTML, /data-wconfirm="cancel"/);
  assert.match(menu.innerHTML, /data-wconfirm="ok"/);

  const cancel = el("button", { dataset: { wconfirm: "cancel" } });
  cancel.dataset.wconfirm = "cancel";
  cancel.closest = sel => (sel === "[data-wconfirm]" || sel.includes("data-wconfirm") ? cancel : null);
  menu.dispatch("click", { target: cancel });
  await settle();
  assert.equal(apiLog.some(x => x.method === "DELETE"), false, "cancel skips DELETE");
});

test("card delete confirm issues DELETE for that note id", async () => {
  const ctx = createFeature({
    notes: [
      { id: "n1", title: "Keep", order: 0 },
      { id: "n2", title: "Gone", order: 1 },
    ],
    docs: {
      n1: { id: "n1", title: "Keep", sections: [] },
      n2: { id: "n2", title: "Gone", sections: [] },
    },
  });
  const { feature, roots, apiLog } = ctx;
  feature.bind();
  feature.open();
  await settle();
  await openNote(ctx, "n1");

  const card = el("div", { className: "wscard", dataset: { note: "n2" } });
  card.dataset.note = "n2";
  const delBtn = el("button", { className: "danger", dataset: { wcact: "delete" } });
  delBtn.dataset.wcact = "delete";
  delBtn.closest = sel => {
    if (sel === "[data-wcact]" || sel.includes("data-wcact")) return delBtn;
    if (sel === ".wscard") return card;
    return null;
  };
  delBtn.getBoundingClientRect = () => ({ top: 10, left: 10, bottom: 50, height: 40, width: 44 });
  card.closest = sel => (sel === ".wscard" ? card : null);
  roots.wscards.appendChild(card);
  roots.wspanel.getBoundingClientRect = () => ({ top: 0, left: 0, width: 400, height: 600 });

  roots.wscards.dispatch("click", {
    target: delBtn,
    stopPropagation(){},
    preventDefault(){},
  });
  await settle();

  const menu = roots.wspanel.children.find(c => (c.className || "").includes("wsmenu"));
  assert.ok(menu);
  const ok = el("button", { dataset: { wconfirm: "ok" } });
  ok.dataset.wconfirm = "ok";
  ok.closest = sel => (sel === "[data-wconfirm]" || sel.includes("data-wconfirm") ? ok : null);
  menu.dispatch("click", { target: ok });
  await settle();

  const del = apiLog.find(x => x.method === "DELETE" && x.url.includes("/api/notes/n2"));
  assert.ok(del, "confirm issues DELETE for the card's note");
  assert.equal(roots.wstitle.textContent, "Keep");
});

/* ---------- title Enter/Escape contract in source ---------- */
test("source: debounce 1100, PatchNow reverts, sectionBodyInner on blur", () => {
  assert.match(notesSrc, /SAVE_DEBOUNCE_MS = 1100/);
  assert.match(notesSrc, /wsPatchNow\(\{ title: wsTitleOrig \}/);
  assert.match(notesSrc, /wsPatchNow\(\{ section: \{ id: secId, title: orig \}/);
  assert.match(notesSrc, /wsPatchNow\(\{ section: \{ id: secId, body: orig \}/);
  assert.match(notesSrc, /sectionBodyInner\(s/);
  assert.match(notesSrc, /scrollHeight/);
  assert.doesNotMatch(notesSrc, /wsPatch\(\{ title: wsTitleOrig \}\)/);
});

/* ---------- lazy Bookmarks callbacks without import cycle ---------- */
test("bookmarks does not import notes; notes may import bookmark pure helpers", () => {
  assert.doesNotMatch(bookmarksSrc, /from ["'].*notes/);
  assert.match(notesSrc, /bookmarkLaneId|bookmarkSortKey|bookmarkClampState/);
  assert.match(notesSrc, /startPlacement/);
});

/* ---------- poll/edit non-clobber decision (source + drag guard) ---------- */
test("renderNote is not invoked from poll path; drag guards cards", () => {
  assert.match(notesSrc, /if \(wsCardDragging\) return/);
  assert.match(notesSrc, /DOM text is authoritative|never rebuilds\s+focused editors|Editors never rebuilt by polling/i);
});

/* ---------- body edit preserves references via sectionBodyInner ---------- */
test("body edit Escape restores rendered body and references", async () => {
  const ctx = createFeature({
    notes: [{ id: "n1", title: "N", order: 0 }],
    docs: {
      n1: {
        id: "n1", title: "N",
        sections: [{
          id: "s1", title: "S", body: "original", order: 0,
          references: [{
            id: "r1",
            snapshot: { station: "Alpha", speaker: "you", text: "quoted" },
          }],
        }],
      },
    },
  });
  ctx.feature.bind();
  ctx.feature.open();
  await settle();
  await openNote(ctx, "n1");

  const sec = el("div", { className: "wssec", dataset: { sec: "s1" } });
  const body = el("div", { className: "wssecbody" });
  const render = el("div", { dataset: { secrender: "" } });
  sec.appendChild(body);
  body.appendChild(render);
  ctx.roots.wssections.appendChild(sec);
  ctx.roots.wssections.dispatch("click", { target: render });

  const textarea = body.querySelector("textarea");
  assert.ok(textarea, "body click must enter edit mode");
  textarea.value = "changed";
  let prevented = false;
  let stopped = false;
  textarea.dispatch("keydown", {
    key: "Escape",
    preventDefault(){ prevented = true; },
    stopPropagation(){ stopped = true; },
  });
  await settle();
  assert.equal(prevented, true);
  assert.equal(stopped, true);
  assert.match(body.innerHTML, /original/);
  assert.match(body.innerHTML, /data-ref="r1"/);
  assert.match(body.innerHTML, /quoted/);
});
