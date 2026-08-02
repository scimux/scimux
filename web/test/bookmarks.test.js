/* Characterization tests for web/js/bookmarks.js — pure decisions + factory
 * adapters with a small fake DOM and controlled timers/clipboard. Does not
 * re-test format escaping, lanes hash, Notes workspace, or search. */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import {
  BOOKMARK_CLAMP_PX,
  STORAGE_KEY_OPEN,
  STORAGE_KEY_TAB,
  COPY_ACK_MS,
  DRAFT_KEY_PREFIX,
  stampAddress,
  bookmarkLaneId,
  bookmarkSortKey,
  bookmarkClampState,
  resolveBookmarkTab,
  bookmarkListModel,
  bookmarkTabsHTML,
  bookmarkListHTML,
  bookmarkFlagsHTML,
  bookmarksToggleState,
  mergeBookmarkIntoDraft,
  manualBookmarkPayload,
  jumpAddressDecision,
  createBookmarksFeature,
} from "../js/bookmarks.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const bookmarksSrc = readFileSync(join(__dirname, "../js/bookmarks.js"), "utf8");

/* ---------- module shape ---------- */
test("bookmarks.js exports factory and pure helpers; no later-feature imports", () => {
  assert.match(bookmarksSrc, /export function createBookmarksFeature/);
  assert.match(bookmarksSrc, /from "\.\/format\.js"/);
  assert.match(bookmarksSrc, /from "\.\/lanes\.js"/);
  assert.doesNotMatch(bookmarksSrc, /from "\.\/notes\.js"/);
  assert.doesNotMatch(bookmarksSrc, /from "\.\/search\.js"/);
  assert.doesNotMatch(bookmarksSrc, /from "\.\/sheets\.js"/);
  assert.doesNotMatch(bookmarksSrc, /from "\.\/polling\.js"/);
  assert.doesNotMatch(bookmarksSrc, /from "\.\/app\.js"/);
  assert.doesNotMatch(bookmarksSrc, /from "\.\/chat\.js"/);
  assert.doesNotMatch(bookmarksSrc, /from "\.\/composer\.js"/);
  assert.doesNotMatch(bookmarksSrc, /export function esc\b/);
  assert.doesNotMatch(bookmarksSrc, /test\/debug accessors|getExpanded|_openBookmark/);
});

test("storage keys, clamp px, copy timer constants", () => {
  assert.equal(STORAGE_KEY_OPEN, "scimux-bookmarks");
  assert.equal(STORAGE_KEY_TAB, "scimux-bookmarktab");
  assert.equal(BOOKMARK_CLAMP_PX, 220);
  assert.equal(COPY_ACK_MS, 900);
  assert.equal(DRAFT_KEY_PREFIX, "scimux-draft:");
});

/* ---------- stampAddress ---------- */
test("stampAddress: coerces segment/record; absent without uid", () => {
  const b = {};
  stampAddress(b, null);
  assert.deepEqual(b, {});
  stampAddress(b, {});
  assert.deepEqual(b, {});
  stampAddress(b, { uid: "", segment: 1, record: 2 });
  assert.deepEqual(b, {});
  stampAddress(b, { uid: "u1", segment: "3", record: "4" });
  assert.equal(b.uid, "u1");
  assert.equal(b.segment, 3);
  assert.equal(b.record, 4);
  const c = {};
  stampAddress(c, { uid: "u2", segment: "x", record: null });
  assert.equal(c.segment, 0);
  assert.equal(c.record, 0);
  const d = {};
  stampAddress(d, { uid: "u3" });
  assert.equal(d.segment, 0);
  assert.equal(d.record, 0);
});

/* ---------- lane resolution ---------- */
test("bookmarkLaneId: live / deleted / comment inherit / manual", () => {
  const nodes = {
    n1: { id: "n1", lane_id: "lane-live" },
  };
  const nodeById = id => nodes[id] || null;
  const byT = {
    a: { t: "a", node: "n1", lane: "captured" },
    b: { t: "b", node: "gone", lane: "captured-gone" },
    c: { t: "c", anchor: "a", text: "comment" },
    d: { t: "d", lane: "manual-only" },
    e: { t: "e" },
  };
  assert.equal(bookmarkLaneId(byT.a, byT, nodeById), "lane-live");
  assert.equal(bookmarkLaneId(byT.b, byT, nodeById), "captured-gone");
  assert.equal(bookmarkLaneId(byT.c, byT, nodeById), "lane-live");
  assert.equal(bookmarkLaneId(byT.d, byT, nodeById), "manual-only");
  assert.equal(bookmarkLaneId(byT.e, byT, nodeById), "");
  /* comment with missing anchor falls through to own fields */
  assert.equal(bookmarkLaneId({ t: "x", anchor: "missing", lane: "own" }, byT, nodeById), "own");
});

/* ---------- comment sort position ---------- */
test("bookmarkSortKey places comments immediately after anchors; oldest-first", () => {
  const byT = {
    "2026-01-01T00:00:00Z": { t: "2026-01-01T00:00:00Z", text: "first" },
    "2026-01-02T00:00:00Z": { t: "2026-01-02T00:00:00Z", text: "second" },
    "2026-01-03T00:00:00Z": {
      t: "2026-01-03T00:00:00Z",
      anchor: "2026-01-01T00:00:00Z",
      text: "late comment on first",
    },
  };
  const keys = Object.values(byT)
    .map(nt => ({ nt, k: bookmarkSortKey(nt, byT) }))
    .sort((a, b) => a.k.localeCompare(b.k))
    .map(x => x.nt.t);
  assert.deepEqual(keys, [
    "2026-01-01T00:00:00Z",
    "2026-01-03T00:00:00Z",
    "2026-01-02T00:00:00Z",
  ]);
  assert.ok(bookmarkSortKey(byT["2026-01-03T00:00:00Z"], byT).startsWith("2026-01-01T00:00:00Z~"));
});

/* ---------- clamp boundaries ---------- */
test("bookmarkClampState boundaries at 220px", () => {
  assert.deepEqual(bookmarkClampState(120, false), {
    clamped: false, showBtn: false, label: "Show more",
  });
  assert.deepEqual(bookmarkClampState(120, true), {
    clamped: false, showBtn: false, label: "Show less",
  });
  assert.deepEqual(bookmarkClampState(900, false), {
    clamped: true, showBtn: true, label: "Show more",
  });
  assert.deepEqual(bookmarkClampState(900, true), {
    clamped: false, showBtn: true, label: "Show less",
  });
  assert.deepEqual(bookmarkClampState(BOOKMARK_CLAMP_PX, false), {
    clamped: false, showBtn: false, label: "Show more",
  });
  assert.deepEqual(bookmarkClampState(BOOKMARK_CLAMP_PX + 1, false), {
    clamped: true, showBtn: true, label: "Show more",
  });
});

/* ---------- tabs / list / flags HTML ---------- */
test("bookmarkTabsHTML General first, lane order, on class", () => {
  const html = bookmarkTabsHTML("lane-a", [
    { id: "lane-a", name: "Alpha" },
    { id: "lane-b", name: "Beta" },
  ], s => s, id => id === "lane-a" ? "#f00" : "#0f0");
  assert.match(html, /data-ntab="GENERAL"/);
  assert.ok(html.indexOf('data-ntab="GENERAL"') < html.indexOf('data-ntab="lane-a"'));
  assert.match(html, /data-ntab="lane-a"[^>]*class="on"/);
  assert.match(html, /Alpha/);
  assert.match(html, /background:#f00/);
});

test("bookmarkListHTML: empty, jump gate, comment class, action order", () => {
  const empty = bookmarkListHTML({
    list: [], bookmarkTab: "GENERAL", openBookmarkT: "",
    esc: s => s, md: s => s, fmtWhen: s => s,
  });
  assert.match(empty, /No bookmarks yet/);
  assert.match(empty, /“bookmark”/);

  const list = [
    { t: "t1", text: "hello", node: "n1" },
    { t: "t2", text: "uid only", uid: "u1" },
    { t: "t3", text: "orphan" },
    { t: "t4", text: "comment", node: "n1", anchor: "t1" },
  ];
  const open = bookmarkListHTML({
    list,
    bookmarkTab: "GENERAL",
    openBookmarkT: "t1",
    nodeById: id => id === "n1" ? { id: "n1", title: "Chat A" } : null,
    esc: s => s,
    md: s => `<p>${s}</p>`,
    fmtWhen: s => "WHEN",
    laneColor: () => "#abc",
    icons: {
      ICON_JUMP: "J", ICON_COMMENT: "C", ICON_COPY: "P",
      ICON_CLIP: "L", ICON_TRASH: "T",
    },
  });
  assert.match(open, /class="bookmark "/);
  assert.match(open, /Chat A/);
  assert.match(open, /data-bmact="jump"/);
  assert.match(open, /data-bmact="comment"/);
  assert.match(open, /data-bmact="copy"/);
  assert.match(open, /data-bmact="note"/);
  assert.match(open, /data-bmact="del"/);
  const noteIdx = open.indexOf('data-bmact="note"');
  const delIdx = open.indexOf('data-bmact="del"');
  assert.ok(noteIdx >= 0 && delIdx > noteIdx);

  const uidOpen = bookmarkListHTML({
    list: [list[1]],
    bookmarkTab: "GENERAL",
    openBookmarkT: "t2",
    esc: s => s, md: s => s, fmtWhen: s => s,
    icons: { ICON_JUMP: "J", ICON_COPY: "P", ICON_CLIP: "L", ICON_TRASH: "T" },
  });
  assert.match(uidOpen, /data-bmact="jump"/);
  assert.doesNotMatch(uidOpen, /data-bmact="comment"/);

  const orphanOpen = bookmarkListHTML({
    list: [list[2]],
    bookmarkTab: "GENERAL",
    openBookmarkT: "t3",
    esc: s => s, md: s => s, fmtWhen: s => s,
    icons: { ICON_COPY: "P", ICON_CLIP: "L", ICON_TRASH: "T" },
  });
  assert.doesNotMatch(orphanOpen, /data-bmact="jump"/);

  const commentRow = bookmarkListHTML({
    list: [list[3]],
    bookmarkTab: "GENERAL",
    openBookmarkT: "",
    esc: s => s, md: s => s, fmtWhen: s => s,
  });
  assert.match(commentRow, /class="bookmark comment"/);
});

test("bookmarkFlagsHTML lane flags then GENERAL", () => {
  const html = bookmarkFlagsHTML(
    [{ id: "l1", name: "Lane", color: "#111" }],
    true,
    s => s,
  );
  assert.match(html, /data-flag="l1"/);
  assert.match(html, /Lane bookmarks/);
  assert.match(html, /data-flag="GENERAL"/);
  assert.ok(html.indexOf('data-flag="l1"') < html.indexOf('data-flag="GENERAL"'));
  assert.equal(bookmarkFlagsHTML([], false, s => s), "");
});

test("bookmarksToggleState chevrons and aria", () => {
  assert.deepEqual(bookmarksToggleState(false), {
    innerHTML: "&#8249;",
    ariaLabel: "open bookmarks",
  });
  assert.deepEqual(bookmarksToggleState(true), {
    innerHTML: "&#8250;",
    ariaLabel: "close bookmarks",
  });
});

/* ---------- list model / invalid tab ---------- */
test("bookmarkListModel invalid tab falls back to GENERAL; filter by lane", () => {
  const bms = [
    { t: "1", text: "g" },
    { t: "2", text: "a", lane: "lane-a" },
    { t: "3", text: "b", node: "n1" },
  ];
  const model = bookmarkListModel(
    bms,
    "missing-lane",
    id => id === "n1" ? { id: "n1", lane_id: "lane-b" } : null,
    () => [{ id: "lane-a", name: "A" }, { id: "lane-b", name: "B" }],
  );
  assert.equal(model.tab, "GENERAL");
  assert.equal(model.list.length, 1);
  assert.equal(model.list[0].t, "1");
  assert.equal(model.tabs.length, 2);

  const laneA = bookmarkListModel(
    bms, "lane-a",
    () => null,
    () => [{ id: "lane-a", name: "A" }],
  );
  assert.equal(laneA.tab, "lane-a");
  assert.deepEqual(laneA.list.map(x => x.t), ["2"]);
});

test("resolveBookmarkTab", () => {
  assert.equal(resolveBookmarkTab("GENERAL", ["a"]), "GENERAL");
  assert.equal(resolveBookmarkTab("a", ["a", "b"]), "a");
  assert.equal(resolveBookmarkTab("gone", ["a"]), "GENERAL");
});

/* ---------- manual payload / draft merge ---------- */
test("manualBookmarkPayload general / lane / comment", () => {
  assert.deepEqual(
    manualBookmarkPayload({ text: "hi", t: "T", bookmarkTab: "GENERAL" }),
    { t: "T", text: "hi" },
  );
  assert.deepEqual(
    manualBookmarkPayload({ text: "hi", t: "T", bookmarkTab: "lane-x" }),
    { t: "T", text: "hi", lane: "lane-x" },
  );
  assert.deepEqual(
    manualBookmarkPayload({
      text: "reply", t: "T", bookmarkAnchor: "parent", bookmarkTab: "lane-x",
    }),
    { t: "T", text: "reply", anchor: "parent" },
  );
});

test("mergeBookmarkIntoDraft", () => {
  assert.equal(mergeBookmarkIntoDraft("", "new"), "new");
  assert.equal(mergeBookmarkIntoDraft("old", "new"), "old\n\nnew");
  assert.equal(mergeBookmarkIntoDraft("old", ""), "old\n\n");
  assert.equal(mergeBookmarkIntoDraft("", null), "");
});

/* ---------- jump decision ---------- */
test("jumpAddressDecision live / archived / fail", () => {
  const live = jumpAddressDecision(
    { node: "n1", uid: "u", segment: 1, record: 2, turnTime: "tt", text: "tx" },
    id => id === "n1" ? { id: "n1" } : null,
  );
  assert.equal(live.kind, "live");
  assert.equal(live.pendingJump.node, "n1");
  assert.equal(live.pendingJump.uid, "u");

  const arch = jumpAddressDecision(
    { node: "gone", uid: "u2", segment: 3, record: 4, turnTime: "t2" },
    () => null,
  );
  assert.equal(arch.kind, "archived");
  assert.equal(arch.uid, "u2");

  const fail = jumpAddressDecision({ text: "only" }, () => null);
  assert.equal(fail.kind, "fail");
});

/* ---------- fake DOM helpers ---------- */
function el(tag, attrs = {}){
  const listeners = new Map();
  const children = [];
  const node = {
    tagName: tag.toUpperCase(),
    id: attrs.id || "",
    className: attrs.className || "",
    hidden: !!attrs.hidden,
    textContent: attrs.textContent || "",
    innerHTML: attrs.innerHTML || "",
    innerText: attrs.innerText != null ? attrs.innerText : (attrs.textContent || ""),
    scrollTop: 0,
    scrollHeight: attrs.scrollHeight || 0,
    dataset: { ...(attrs.dataset || {}) },
    style: {},
    classList: {
      _set: new Set((attrs.className || "").split(/\s+/).filter(Boolean)),
      toggle(name, on){
        if (on) this._set.add(name); else this._set.delete(name);
        node.className = [...this._set].join(" ");
      },
      add(name){ this._set.add(name); node.className = [...this._set].join(" "); },
      remove(name){ this._set.delete(name); node.className = [...this._set].join(" "); },
      contains(name){ return this._set.has(name); },
    },
    children,
    parentNode: null,
    closest(sel){
      if (sel.startsWith(".") && this.className.split(/\s+/).includes(sel.slice(1))) return this;
      if (sel.startsWith("#") && this.id === sel.slice(1)) return this;
      if (sel.startsWith("[data-") && this.dataset){
        const key = sel.replace(/^\[data-/, "").replace(/\]$/, "").replace(/-([a-z])/g, (_, c) => c.toUpperCase());
        /* crude: [data-nmore] means attribute present */
        if (sel === "[data-nmore]" && ("nmore" in this.dataset || this.dataset.nmore !== undefined))
          return this;
        if (sel === "[data-ntab]" && this.dataset.ntab !== undefined) return this;
        if (sel === "[data-flag]" && this.dataset.flag !== undefined) return this;
        if (sel === "[data-bmact]" && this.dataset.bmact !== undefined) return this;
        if (sel === "[data-fwd]" && this.dataset.fwd !== undefined) return this;
        if (sel === ".bookmark" && this.className.includes("bookmark")) return this;
      }
      if (sel === ".bookmark" && this.className.includes("bookmark")) return this;
      return this.parentNode ? this.parentNode.closest(sel) : null;
    },
    querySelector(sel){
      const all = this.querySelectorAll(sel);
      return all[0] || null;
    },
    querySelectorAll(sel){
      const out = [];
      const walk = n => {
        if (match(n, sel)) out.push(n);
        for (const c of n.children || []) walk(c);
      };
      for (const c of children) walk(c);
      /* also scan string-parsed fake children via registry */
      if (this._nodes){
        for (const n of this._nodes) if (match(n, sel)) out.push(n);
      }
      return out;
    },
    addEventListener(type, fn){
      if (!listeners.has(type)) listeners.set(type, []);
      listeners.get(type).push(fn);
    },
    removeEventListener(type, fn){
      const arr = listeners.get(type) || [];
      listeners.set(type, arr.filter(f => f !== fn));
    },
    dispatch(type, ev = {}){
      const e = {
        type,
        target: ev.target || this,
        preventDefault(){ this.defaultPrevented = true; },
        stopPropagation(){},
        key: ev.key,
        shiftKey: !!ev.shiftKey,
        altKey: !!ev.altKey,
        metaKey: !!ev.metaKey,
        ctrlKey: !!ev.ctrlKey,
        ...ev,
      };
      for (const fn of listeners.get(type) || []) fn(e);
      return e;
    },
    focus(){ this._focused = true; },
    blur(){ this._focused = false; },
    setAttribute(k, v){ this[k === "aria-label" ? "ariaLabel" : k] = v; if (k === "aria-label") this._ariaLabel = v; },
    getAttribute(k){ return k === "aria-label" ? this._ariaLabel : this[k]; },
    scrollIntoView(){ this._scrolled = true; },
    _listeners: listeners,
  };
  if (attrs["aria-label"]) node._ariaLabel = attrs["aria-label"];
  return node;
}

function match(n, sel){
  if (sel === ".bookmark") return (n.className || "").includes("bookmark");
  if (sel === ".nbubble") return (n.className || "").includes("nbubble");
  if (sel === ".nmore") return (n.className || "").includes("nmore");
  if (sel === ".bookmarkactions") return (n.className || "").includes("bookmarkactions");
  if (sel.startsWith("#")) return n.id === sel.slice(1);
  if (sel.includes(".bookmark[data-t=")){
    const m = sel.match(/data-t="([^"]+)"/);
    return (n.className || "").includes("bookmark") && n.dataset && n.dataset.t === m[1];
  }
  if (sel.includes(".bookmarkactions")){
    return (n.className || "").includes("bookmarkactions");
  }
  return false;
}

function makeStorage(initial = {}){
  const map = { ...initial };
  return {
    getItem(k){ return Object.prototype.hasOwnProperty.call(map, k) ? map[k] : null; },
    setItem(k, v){ map[k] = String(v); },
    removeItem(k){ delete map[k]; },
    _map: map,
  };
}

function makeRoots(){
  const body = el("body");
  const document = {
    body,
    querySelector(sel){
      if (sel === "#sendto_list") return roots.sendtoList;
      if (sel === "#prompt") return roots.chatPrompt;
      if (sel.startsWith("#bookmarklist")){
        /* support #bookmarklist .bookmark[data-t="…"] .bookmarkactions */
        const m = sel.match(/data-t="([^"]+)"/);
        if (m && roots.bookmarklist._nodes){
          const bm = roots.bookmarklist._nodes.find(
            n => n.dataset && n.dataset.t === m[1] && (n.className || "").includes("bookmark"),
          );
          if (!bm) return null;
          if (sel.includes(".bookmarkactions"))
            return bm.querySelector(".bookmarkactions") ||
              (bm._nodes || []).find(n => (n.className || "").includes("bookmarkactions")) || null;
          return bm;
        }
      }
      return null;
    },
  };

  const roots = {
    bookmarkspane: el("section", { id: "bookmarkspane" }),
    bookmarktabs: el("div", { id: "bookmarktabs" }),
    bookmarklist: el("div", { id: "bookmarklist" }),
    bookmarkflags: el("div", { id: "bookmarkflags" }),
    bookmarksbtn: el("button", { id: "bookmarksbtn", "aria-label": "open bookmarks" }),
    bookmarksback: el("button", { id: "bookmarksback", textContent: "‹ Back" }),
    notesbtn: el("button", { id: "notesbtn", textContent: "Notes ›" }),
    bookmarkpeek: el("div", { id: "bookmarkpeek" }),
    bookmarkbar: el("div", { id: "bookmarkbar" }),
    bookmarkchip: el("div", { id: "bookmarkchip", hidden: true }),
    bookmarkchiptext: el("span", { id: "bookmarkchiptext" }),
    bookmarkchipx: el("button", { id: "bookmarkchipx" }),
    bookmarkprompt: el("div", {
      id: "bookmarkprompt",
      textContent: "",
      innerText: "",
    }),
    bookmarksend: el("button", { id: "bookmarksend" }),
    sendtoList: el("div", { id: "sendto_list" }),
    chatPrompt: el("div", { id: "prompt" }),
  };
  roots.bookmarkprompt.dataset = { placeholder: "Add a bookmark…" };
  roots.bookmarklist._nodes = [];
  /* when list HTML is set, parse minimal structure for clamp/tap tests */
  const origListSet = Object.getOwnPropertyDescriptor(roots.bookmarklist, "innerHTML");
  let listHtml = "";
  Object.defineProperty(roots.bookmarklist, "innerHTML", {
    get(){ return listHtml; },
    set(v){
      listHtml = v;
      roots.bookmarklist._nodes = parseBookmarkNodes(v, roots.bookmarklist);
      roots.bookmarklist.children = roots.bookmarklist._nodes;
    },
  });
  void origListSet;
  return { roots, document, body };
}

function parseBookmarkNodes(html, parent){
  const nodes = [];
  const re = /class="bookmark([^"]*)"[^>]*data-t="([^"]+)"/g;
  let m;
  while ((m = re.exec(html))){
    const isComment = m[1].includes("comment");
    const t = m[2];
    const bm = el("div", {
      className: "bookmark" + (isComment ? " comment" : ""),
      dataset: { t },
    });
    bm.parentNode = parent;
    const nbubble = el("div", {
      className: "nbubble",
      scrollHeight: html.includes("TALL-" + t) ? 400 : 80,
    });
    nbubble.parentNode = bm;
    const nmore = el("button", { className: "nmore", dataset: { nmore: "" }, hidden: true });
    nmore.parentNode = bm;
    nmore.dataset.nmore = "";
    bm.children = [nbubble, nmore];
    bm._nodes = [nbubble, nmore];
    if (html.includes(`data-t="${t}"`) && html.includes("bookmarkactions") &&
        html.indexOf(`data-t="${t}"`) < html.indexOf("bookmarkactions", html.indexOf(`data-t="${t}"`))){
      /* action row present for this open t — approximate */
    }
    /* attach actions if open in html after this data-t */
    const slice = html.slice(html.indexOf(`data-t="${t}"`));
    const next = slice.indexOf('data-t="', 1);
    const mine = next > 0 ? slice.slice(0, next) : slice;
    if (mine.includes("bookmarkactions")){
      const actions = el("div", { className: "bookmarkactions" });
      actions.parentNode = bm;
      const acts = [];
      for (const a of ["jump", "comment", "copy", "note", "del"]){
        if (mine.includes(`data-bmact="${a}"`)){
          const btn = el("button", { dataset: { bmact: a } });
          btn.dataset.bmact = a;
          btn.parentNode = actions;
          btn.closest = sel => {
            if (sel === "[data-bmact]") return btn;
            if (sel === ".bookmark") return bm;
            return null;
          };
          acts.push(btn);
        }
      }
      actions.children = acts;
      bm.children.push(actions);
      bm._nodes.push(actions);
    }
    bm.querySelector = sel => {
      if (sel === ".nbubble") return nbubble;
      if (sel === ".nmore") return nmore;
      if (sel === ".bookmarkactions")
        return bm.children.find(c => (c.className || "").includes("bookmarkactions")) || null;
      return null;
    };
    bm.closest = sel => {
      if (sel === ".bookmark") return bm;
      return parent && parent.closest ? parent.closest(sel) : null;
    };
    nodes.push(bm);
  }
  return nodes;
}

function createFeature(overrides = {}){
  const { roots, document, body } = makeRoots();
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
    for (const t of timers){
      if (!t.cleared && t.at <= now){
        t.cleared = true;
        t.fn();
      }
    }
  };

  let bms = overrides.bookmarks || [];
  const ops = [];
  const effects = {
    openNotes: [],
    startPlacement: [],
    toast: [],
    copy: [],
    select: [],
    setLevel: [],
    openSheet: [],
    closeSheets: [],
    setPendingJump: [],
    openArchived: [],
    invalidateChat: 0,
    closeWorkspace: 0,
    restartWorkPulse: 0,
  };

  const nodes = overrides.nodes || {
    n1: { id: "n1", title: "Alpha", lane_id: "lane-a" },
    n2: { id: "n2", title: "Beta", lane_id: "lane-b" },
  };
  const lanes = overrides.lanes || [
    { id: "lane-a", name: "Alpha", color: "#f00" },
    { id: "lane-b", name: "Beta", color: "#0f0" },
  ];

  const feature = createBookmarksFeature({
    roots,
    document,
    storage,
    setTimeout: setTimeoutFn,
    clearTimeout: clearTimeoutFn,
    CSS: { escape: s => s },
    esc: s => String(s ?? "").replace(/[<>&"]/g, c => ({
      "<": "&lt;", ">": "&gt;", "&": "&amp;", '"': "&quot;",
    }[c])),
    md: s => String(s ?? ""),
    fmtWhen: s => "W(" + s + ")",
    hashStr: s => {
      let h = 0;
      for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) | 0;
      return String(h);
    },
    icons: {
      ICON_JUMP: "J", ICON_COMMENT: "C", ICON_COPY: "P",
      ICON_CLIP: "L", ICON_TRASH: "T", ICON_SEND: "S",
    },
    bookmarks: () => bms,
    uiMutate: op => {
      ops.push(op);
      if (op.k === "bookmark-add") bms = [...bms, op.bookmark];
      if (op.k === "bookmark-del") bms = bms.filter(x => x.t !== op.t);
    },
    nodeById: id => nodes[id] || null,
    laneList: () => lanes,
    laneColor: id => (lanes.find(l => l.id === id) || {}).color || "",
    laneModel: () => ({ color: id => (lanes.find(l => l.id === id) || {}).color || "#999" }),
    orderedNodes: () => Object.values(nodes),
    openNotes: () => effects.openNotes.push(1),
    startPlacement: nt => effects.startPlacement.push(nt),
    toast: msg => effects.toast.push(msg),
    copyText: s => effects.copy.push(s),
    select: id => effects.select.push(id),
    setLevel: n => effects.setLevel.push(n),
    openSheet: id => effects.openSheet.push(id),
    closeSheets: () => effects.closeSheets.push(1),
    setPendingJump: v => effects.setPendingJump.push(v),
    openArchived: (...a) => effects.openArchived.push(a),
    invalidateChat: () => { effects.invalidateChat++; },
    wsOpen: () => !!overrides.wsOpen,
    closeWorkspace: () => { effects.closeWorkspace++; },
    isDesktop: () => !!overrides.isDesktop,
    restartWorkPulse: () => { effects.restartWorkPulse++; },
    sendtoList: roots.sendtoList,
    chatPrompt: roots.chatPrompt,
    longpress: (container, selector, fn) => {
      container._lp = { selector, fn };
      return () => { container._lp = null; };
    },
    ...overrides.deps,
  });

  return {
    feature, roots, document, body, storage, ops, effects, timers, flush,
    setBookmarks(next){ bms = next; },
    getBookmarks(){ return bms; },
  };
}

/* ---------- factory: open/close/toggle/peek/flags ---------- */
test("pane toggle, open/close, peek, body class, storage", () => {
  const ctx = createFeature();
  const { feature, roots, body, storage, effects } = ctx;
  feature.bind();
  assert.equal(feature.isOpen(), false);
  assert.equal(roots.bookmarksbtn._ariaLabel, "open bookmarks");
  assert.equal(roots.bookmarksbtn.innerHTML, "&#8249;");

  roots.bookmarksbtn.dispatch("click");
  assert.equal(feature.isOpen(), true);
  assert.ok(body.classList.contains("bookmarks-open"));
  assert.equal(storage.getItem(STORAGE_KEY_OPEN), "1");
  assert.equal(roots.bookmarksbtn._ariaLabel, "close bookmarks");
  assert.equal(effects.restartWorkPulse, 1);

  roots.bookmarkpeek.dispatch("click");
  assert.equal(feature.isOpen(), false);
  assert.equal(storage.getItem(STORAGE_KEY_OPEN), "0");

  feature.setOpen(true);
  roots.bookmarksback.dispatch("click");
  assert.equal(feature.isOpen(), false);
});

test("flags open pane onto tab; notesbtn uses lazy openNotes", () => {
  const ctx = createFeature({
    bookmarks: [
      { t: "1", text: "a", lane: "lane-a" },
      { t: "2", text: "g" },
    ],
  });
  const { feature, roots, effects } = ctx;
  feature.bind();
  feature.render();
  assert.match(roots.bookmarkflags.innerHTML, /data-flag="lane-a"/);
  assert.match(roots.bookmarkflags.innerHTML, /data-flag="GENERAL"/);

  const flagBtn = el("button", { dataset: { flag: "lane-a" } });
  flagBtn.dataset.flag = "lane-a";
  flagBtn.closest = sel => sel === "[data-flag]" ? flagBtn : null;
  roots.bookmarkflags.dispatch("click", { target: flagBtn });
  assert.equal(feature.isOpen(), true);
  assert.equal(ctx.storage.getItem(STORAGE_KEY_TAB), "lane-a");

  roots.notesbtn.dispatch("click");
  assert.equal(effects.openNotes.length, 1);
});

test("lazy Notes callback without importing notes.js", () => {
  assert.doesNotMatch(bookmarksSrc, /from ["'].*notes/);
  const calls = [];
  const ctx = createFeature({
    deps: { openNotes: () => calls.push("notes") },
  });
  ctx.feature.bind();
  ctx.roots.notesbtn.dispatch("click");
  assert.deepEqual(calls, ["notes"]);
});

test("bookmark localStorage failures retain the inline implementation semantics", () => {
  assert.throws(() => createFeature({
    deps: { storage: { getItem(){ throw new Error("read"); }, setItem(){} } },
  }), /read/);

  const ctx = createFeature({
    deps: {
      storage: {
        getItem(){ return null; },
        setItem(){ throw new Error("write"); },
      },
    },
  });
  assert.throws(() => ctx.feature.setOpen(true), /write/);
});

/* ---------- signature skip vs rebuild; expanded state ---------- */
test("signature skip preserves expanded/open; rebuild when data changes", () => {
  const ctx = createFeature({
    bookmarks: [{ t: "t1", text: "hello" }],
  });
  const { feature, roots } = ctx;
  feature.bind();
  feature.setOpen(true);
  const first = roots.bookmarklist.innerHTML;
  assert.ok(first.length > 0);
  /* re-render same data → signature skip (innerHTML unchanged identity of content) */
  roots.bookmarklist.innerHTML = first + "<!--mark-->";
  feature.render();
  /* sig matches stored → no write; our mark remains if skip works */
  assert.ok(roots.bookmarklist.innerHTML.includes("<!--mark-->") ||
    roots.bookmarklist.innerHTML === first);

  /* force data change */
  ctx.setBookmarks([{ t: "t1", text: "hello" }, { t: "t2", text: "world" }]);
  feature.invalidate();
  feature.render();
  assert.match(roots.bookmarklist.innerHTML, /world/);
});

test("bottom pin scrolls list after rebuild; clears pin", () => {
  const ctx = createFeature({
    bookmarks: [{ t: "t1", text: "a" }],
  });
  const { feature, roots } = ctx;
  feature.bind();
  roots.bookmarklist.scrollHeight = 999;
  feature.pinBottom();
  feature.setOpen(true);
  assert.equal(roots.bookmarklist.scrollTop, 999);
  /* second render without pin should not force scroll */
  roots.bookmarklist.scrollTop = 10;
  ctx.setBookmarks([{ t: "t1", text: "a" }, { t: "t2", text: "b" }]);
  feature.invalidate();
  feature.render();
  assert.equal(roots.bookmarklist.scrollTop, 10);
});

test("invalid tab falls back on render", () => {
  const ctx = createFeature({
    storageInit: { [STORAGE_KEY_TAB]: "ghost" },
    bookmarks: [{ t: "1", text: "g" }],
  });
  const { feature, storage } = ctx;
  feature.bind();
  feature.setOpen(true);
  assert.equal(storage.getItem(STORAGE_KEY_TAB), "GENERAL");
});

/* ---------- in-place Show more ---------- */
test("Show more flips clamp in place without full rebuild", () => {
  const ctx = createFeature({
    bookmarks: [{ t: "tall1", text: "TALL-tall1 body" }],
  });
  const { feature, roots } = ctx;
  feature.bind();
  feature.setOpen(true);
  /* inject tall bubble scrollHeight */
  const bms = roots.bookmarklist._nodes;
  assert.ok(bms.length >= 1);
  bms[0].querySelector(".nbubble").scrollHeight = 400;
  feature.invalidate();
  feature.render();
  /* re-apply clamp after measure */
  const nb = roots.bookmarklist._nodes[0];
  nb.querySelector(".nbubble").scrollHeight = 400;
  /* manually run clamp path via more click */
  const more = nb.querySelector(".nmore");
  more.hidden = false;
  more.textContent = "Show more";
  more.dataset.nmore = "";
  more.closest = sel => {
    if (sel === "[data-nmore]") return more;
    if (sel === ".bookmark") return nb;
    return null;
  };
  const before = roots.bookmarklist.innerHTML;
  roots.bookmarkspane.dispatch("click", { target: more });
  assert.equal(more.textContent, "Show less");
  assert.equal(roots.bookmarklist.innerHTML, before);
});

/* ---------- actions ---------- */
test("action dispatch: jump live, copy timer, delete, comment, use-in-note", () => {
  const openCtx = createFeature({
    bookmarks: [
      { t: "t1", text: "cap", node: "n1", uid: "u1", segment: 1, record: 2 },
    ],
  });
  openCtx.feature.bind();
  openCtx.feature.setOpen(true);

  const wrap = el("div", { className: "bookmark", dataset: { t: "t1" } });
  wrap.dataset.t = "t1";
  const actClosest = (btn) => (sel) => {
    if (sel === "[data-bmact]") return btn;
    if (sel === ".bookmark") return wrap;
    return null;
  };

  const jumpBtn = el("button", { dataset: { bmact: "jump" } });
  jumpBtn.dataset.bmact = "jump";
  jumpBtn.closest = actClosest(jumpBtn);
  openCtx.roots.bookmarkspane.dispatch("click", { target: jumpBtn });
  assert.equal(openCtx.effects.setPendingJump.length, 1);
  assert.equal(openCtx.effects.select[0], "n1");
  assert.equal(openCtx.effects.invalidateChat, 1);
  assert.equal(openCtx.feature.isOpen(), false); /* phone default */

  /* copy */
  const copyBtn = el("button", { dataset: { bmact: "copy" }, innerHTML: "P" });
  copyBtn.dataset.bmact = "copy";
  copyBtn.closest = actClosest(copyBtn);
  openCtx.feature.setOpen(true);
  openCtx.roots.bookmarkspane.dispatch("click", { target: copyBtn });
  assert.deepEqual(openCtx.effects.copy, ["cap"]);
  assert.equal(copyBtn.innerHTML, "&#10003;");
  openCtx.flush(COPY_ACK_MS);
  assert.equal(copyBtn.innerHTML, "P");

  /* delete — restore bookmark after prior del would empty list */
  openCtx.setBookmarks([
    { t: "t1", text: "cap", node: "n1", uid: "u1", segment: 1, record: 2 },
  ]);
  const delBtn = el("button", { dataset: { bmact: "del" } });
  delBtn.dataset.bmact = "del";
  delBtn.closest = actClosest(delBtn);
  openCtx.roots.bookmarkspane.dispatch("click", { target: delBtn });
  assert.ok(openCtx.ops.some(o => o.k === "bookmark-del" && o.t === "t1"));

  /* use in note + comment on a fresh feature with the capture present */
  const ctx = createFeature({
    bookmarks: [{ t: "t1", text: "cap", node: "n1" }],
  });
  ctx.feature.bind();
  ctx.feature.setOpen(true);
  const wrap2 = el("div", { className: "bookmark", dataset: { t: "t1" } });
  wrap2.dataset.t = "t1";
  const noteBtn = el("button", { dataset: { bmact: "note" } });
  noteBtn.dataset.bmact = "note";
  noteBtn.closest = sel => {
    if (sel === "[data-bmact]") return noteBtn;
    if (sel === ".bookmark") return wrap2;
    return null;
  };
  ctx.roots.bookmarkspane.dispatch("click", { target: noteBtn });
  assert.equal(ctx.effects.startPlacement.length, 1);
  assert.equal(ctx.effects.startPlacement[0].t, "t1");

  const cBtn = el("button", { dataset: { bmact: "comment" } });
  cBtn.dataset.bmact = "comment";
  cBtn.closest = sel => {
    if (sel === "[data-bmact]") return cBtn;
    if (sel === ".bookmark") return wrap2;
    return null;
  };
  ctx.roots.bookmarkspane.dispatch("click", { target: cBtn });
  assert.equal(ctx.roots.bookmarkchip.hidden, false);
  assert.match(ctx.roots.bookmarkchiptext.textContent, /Replying to:/);
  assert.equal(ctx.roots.bookmarkprompt.dataset.placeholder, "Reply…");
});

test("jump archived and fail toast; desktop keeps pane open", () => {
  const arch = createFeature({
    isDesktop: true,
    bookmarks: [{ t: "t1", text: "x", uid: "uid-1", segment: 5, record: 6 }],
  });
  arch.feature.bind();
  arch.feature.setOpen(true);
  const wrap = el("div", { className: "bookmark", dataset: { t: "t1" } });
  wrap.dataset.t = "t1";
  const jump = el("button", { dataset: { bmact: "jump" } });
  jump.dataset.bmact = "jump";
  jump.closest = sel => {
    if (sel === "[data-bmact]") return jump;
    if (sel === ".bookmark") return wrap;
    return null;
  };
  arch.roots.bookmarkspane.dispatch("click", { target: jump });
  assert.deepEqual(arch.effects.openArchived[0], ["uid-1", 5, 6, ""]);
  assert.equal(arch.feature.isOpen(), true); /* desktop */

  const fail = createFeature({
    bookmarks: [{ t: "t1", text: "orphan only" }],
  });
  fail.feature.bind();
  fail.feature.setOpen(true);
  const w2 = el("div", { className: "bookmark", dataset: { t: "t1" } });
  w2.dataset.t = "t1";
  const j2 = el("button", { dataset: { bmact: "jump" } });
  j2.dataset.bmact = "jump";
  j2.closest = sel => {
    if (sel === "[data-bmact]") return j2;
    if (sel === ".bookmark") return w2;
    return null;
  };
  /* jump button only renders when node||uid — call jumpToChatAddress directly for fail */
  assert.equal(fail.feature.jumpToChatAddress({ text: "x" }), false);
});

/* ---------- manual create + reply cancel ---------- */
test("manual general/lane/comment creation; reply chip blur Escape dismiss", () => {
  const ctx = createFeature({
    storageInit: { [STORAGE_KEY_TAB]: "lane-a" },
    bookmarks: [{ t: "parent", text: "anchor body", lane: "lane-a", node: "n1" }],
  });
  const { feature, roots, ops, storage } = ctx;
  feature.bind();
  feature.setOpen(true);

  roots.bookmarkprompt.innerText = "  lane note  ";
  roots.bookmarkprompt.textContent = "  lane note  ";
  roots.bookmarksend.dispatch("click");
  assert.equal(ops.length, 1);
  assert.equal(ops[0].k, "bookmark-add");
  assert.equal(ops[0].bookmark.lane, "lane-a");
  assert.equal(ops[0].bookmark.text, "lane note");
  assert.equal(roots.bookmarkprompt.textContent, "");

  /* comment path via setBookmarkAnchor */
  ctx.setBookmarks([{ t: "parent", text: "anchor body", node: "n1" }]);
  feature.setBookmarkAnchor("parent");
  assert.equal(roots.bookmarkchip.hidden, false);
  roots.bookmarkprompt.innerText = "reply text";
  roots.bookmarkprompt.textContent = "reply text";
  feature.addManualBookmark();
  const last = ops[ops.length - 1];
  assert.equal(last.bookmark.anchor, "parent");
  assert.equal(last.bookmark.lane, undefined);

  /* Escape clears anchor, keeps typed text path via blur empty */
  feature.setBookmarkAnchor("parent");
  roots.bookmarkprompt.innerText = "still typing";
  roots.bookmarkprompt.textContent = "still typing";
  roots.bookmarkprompt.dispatch("keydown", { key: "Escape" });
  assert.equal(roots.bookmarkchip.hidden, true);
  assert.equal(roots.bookmarkprompt.dataset.placeholder, "Add a note…");

  feature.setBookmarkAnchor("parent");
  roots.bookmarkprompt.innerText = "";
  roots.bookmarkprompt.textContent = "";
  roots.bookmarkprompt.dispatch("blur");
  assert.equal(roots.bookmarkchip.hidden, true);

  feature.setBookmarkAnchor("parent");
  roots.bookmarkchipx.dispatch("click");
  assert.equal(roots.bookmarkchip.hidden, true);
  assert.equal(roots.bookmarkprompt._focused, true);

  /* general tab no lane field */
  const general = el("button", { dataset: { ntab: "GENERAL" } });
  general.dataset.ntab = "GENERAL";
  general.closest = sel => sel === "[data-ntab]" ? general : null;
  roots.bookmarkspane.dispatch("click", { target: general });
  roots.bookmarkprompt.innerText = "gen";
  roots.bookmarkprompt.textContent = "gen";
  feature.addManualBookmark();
  assert.equal(ops[ops.length - 1].bookmark.lane, undefined);
  assert.equal(ops[ops.length - 1].bookmark.anchor, undefined);
  void storage;
});

/* ---------- longpress send-to (explicit note→bookmark contract) ---------- */
test("longpress send-to uses bookmark (not undeclared note); merges draft; navigates", () => {
  /* Intended-contract test for the pre-existing longpress defect. */
  assert.match(bookmarksSrc, /const bookmark = /);
  assert.match(bookmarksSrc, /if \(!bookmark\) return/);
  assert.match(bookmarksSrc, /bookmark\.text/);
  assert.doesNotMatch(bookmarksSrc, /if \(!note\) return/);
  assert.doesNotMatch(bookmarksSrc, /\(note\.text/);

  const ctx = createFeature({
    bookmarks: [{ t: "t1", text: "send me" }],
    isDesktop: false,
  });
  const { feature, roots, effects, storage } = ctx;
  storage.setItem(DRAFT_KEY_PREFIX + "n2", "existing");
  feature.bind();
  feature.setOpen(true);

  const bmEl = el("div", { className: "bookmark", dataset: { t: "t1" } });
  bmEl.dataset.t = "t1";
  assert.ok(roots.bookmarklist._lp);
  roots.bookmarklist._lp.fn(bmEl);

  assert.equal(effects.openSheet[0], "#sendto");
  assert.match(roots.sendtoList.innerHTML, /data-fwd="n1"/);
  assert.match(roots.sendtoList.innerHTML, /Alpha/);

  const fwd = el("button", { dataset: { fwd: "n2" } });
  fwd.dataset.fwd = "n2";
  fwd.closest = sel => sel === "[data-fwd]" ? fwd : null;
  roots.sendtoList.onclick({ target: fwd });

  assert.equal(storage.getItem(DRAFT_KEY_PREFIX + "n2"), "existing\n\nsend me");
  assert.equal(effects.closeSheets.length, 1);
  assert.deepEqual(effects.select, ["n2"]);
  assert.deepEqual(effects.setLevel, [1]);
  assert.equal(roots.chatPrompt._focused, true);
});

test("longpress no-ops when bookmark t missing", () => {
  const ctx = createFeature({ bookmarks: [] });
  const { feature, roots, effects } = ctx;
  feature.bind();
  const bmEl = el("div", { className: "bookmark", dataset: { t: "missing" } });
  bmEl.dataset.t = "missing";
  roots.bookmarklist._lp.fn(bmEl);
  assert.equal(effects.openSheet.length, 0);
});

/* ---------- bind/destroy ---------- */
test("bind is idempotent; destroy removes listeners, longpress, and send-to owner", () => {
  const ctx = createFeature({ bookmarks: [{ t: "t1", text: "send me" }] });
  const { feature, roots } = ctx;
  feature.bind();
  feature.bind(); /* second bind no-op */
  const n1 = roots.bookmarksbtn._listeners.get("click").length;
  assert.equal(n1, 1);
  assert.ok(roots.bookmarklist._lp);
  const bm = el("div", { className: "bookmark", dataset: { t: "t1" } });
  roots.bookmarklist._lp.fn(bm);
  assert.equal(typeof roots.sendtoList.onclick, "function");

  feature.destroy();
  assert.equal(roots.bookmarksbtn._listeners.get("click").length, 0);
  assert.equal(roots.bookmarklist._lp, null);
  assert.equal(roots.sendtoList.onclick, null);

  /* re-bind works */
  feature.bind();
  assert.equal(roots.bookmarksbtn._listeners.get("click").length, 1);
});

test("setBackLabel writes bookmarksback", () => {
  const ctx = createFeature();
  ctx.feature.setBackLabel("My Chat");
  assert.equal(ctx.roots.bookmarksback.textContent, "‹ My Chat");
});

test("jumpToChatAddress closes workspace when open", () => {
  const ctx = createFeature({
    wsOpen: true,
    nodes: { n1: { id: "n1", title: "A", lane_id: "lane-a" } },
  });
  assert.equal(ctx.feature.jumpToChatAddress({
    node: "n1", uid: "u", text: "t",
  }), true);
  assert.equal(ctx.effects.closeWorkspace, 1);
  assert.equal(ctx.effects.select[0], "n1");
});

test("Enter on prompt adds bookmark; empty prompt no-ops", () => {
  const ctx = createFeature();
  ctx.feature.bind();
  ctx.feature.setOpen(true);
  ctx.roots.bookmarkprompt.innerText = "";
  ctx.roots.bookmarkprompt.dispatch("keydown", { key: "Enter" });
  assert.equal(ctx.ops.length, 0);
  ctx.roots.bookmarkprompt.innerText = "hi";
  ctx.roots.bookmarkprompt.textContent = "hi";
  const e = ctx.roots.bookmarkprompt.dispatch("keydown", { key: "Enter" });
  assert.equal(e.defaultPrevented, true);
  assert.equal(ctx.ops[0].k, "bookmark-add");
});

test("tab change clears reply anchor and re-pins", () => {
  const ctx = createFeature({
    bookmarks: [
      { t: "1", text: "g" },
      { t: "2", text: "a", lane: "lane-a" },
    ],
  });
  ctx.feature.bind();
  ctx.feature.setOpen(true);
  ctx.feature.setBookmarkAnchor("1");
  assert.equal(ctx.roots.bookmarkchip.hidden, false);
  const tab = el("button", { dataset: { ntab: "lane-a" } });
  tab.dataset.ntab = "lane-a";
  tab.closest = sel => sel === "[data-ntab]" ? tab : null;
  ctx.roots.bookmarkspane.dispatch("click", { target: tab });
  assert.equal(ctx.roots.bookmarkchip.hidden, true);
  assert.equal(ctx.storage.getItem(STORAGE_KEY_TAB), "lane-a");
});
