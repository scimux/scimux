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
  bookmarkActionsHTML,
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
    workspaceOpen: true,
    icons: {
      ICON_JUMP: "J", ICON_COMMENT: "C", ICON_COPY: "P",
      ICON_CLIP: "L", ICON_TRASH: "T",
    },
  });
  assert.match(open, /class="bookmark "/);
  assert.match(open, /Chat A/);
  /* P5: pane flat bar is jump · comment · note · copy · sendto · del */
  assert.match(open, /data-bmact="jump"/);
  assert.match(open, /data-bmact="comment"/);
  assert.match(open, /data-bmact="note"/);
  assert.match(open, /data-bmact="copy"/);
  assert.match(open, /data-bmact="sendto"/);
  assert.match(open, /data-bmact="del"/);
  assert.doesNotMatch(open, /data-bmact="more"/);
  const jumpIdx = open.indexOf('data-bmact="jump"');
  const commentIdx = open.indexOf('data-bmact="comment"');
  const noteIdx = open.indexOf('data-bmact="note"');
  const delIdx = open.indexOf('data-bmact="del"');
  assert.ok(jumpIdx >= 0 && commentIdx > jumpIdx && noteIdx > commentIdx && delIdx > noteIdx);

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
    remove(){
      this._removed = true;
      const sibs = this.parentNode && this.parentNode.children;
      if (sibs){
        const i = sibs.indexOf(this);
        if (i >= 0) sibs.splice(i, 1);
      }
      this.parentNode = null;
    },
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
      if (sel === "#sendto_title") return roots.sendtoTitle;
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
    sendtoTitle: el("h3", { id: "sendto_title", textContent: "" }),
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
    openPreview: [],
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
    openPreview: (...a) => effects.openPreview.push(a),
    invalidateChat: () => { effects.invalidateChat++; },
    wsOpen: () => !!overrides.wsOpen,
    closeWorkspace: () => { effects.closeWorkspace++; },
    isDesktop: () => !!overrides.isDesktop,
    restartWorkPulse: () => { effects.restartWorkPulse++; },
    sendtoList: roots.sendtoList,
    sendtoTitle: roots.sendtoTitle,
    chatPrompt: roots.chatPrompt,
    pinned: () => overrides.pinned || [],
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

test("jump to a deleted source opens the preview; fail toast; desktop keeps pane open", () => {
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
  assert.deepEqual(arch.effects.openPreview[0], ["uid-1", 5, 6, ""]);
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

/* ---------- P6: send-to is a bar button (longpress removed) ---------- */
test("sendto action opens picker, merges draft, navigates", () => {
  const ctx = createFeature({
    bookmarks: [{ t: "t1", text: "send me" }],
    isDesktop: false,
  });
  const { feature, roots, effects, storage } = ctx;
  storage.setItem(DRAFT_KEY_PREFIX + "n2", "existing");
  feature.bind();
  feature.setOpen(true);

  /* longpress must not be bound after P6 */
  assert.ok(!roots.bookmarklist._lp, "longpress binding removed");

  const wrap = el("div", { className: "bookmark", dataset: { t: "t1" } });
  wrap.dataset.t = "t1";
  const btn = el("button", { dataset: { bmact: "sendto" } });
  btn.dataset.bmact = "sendto";
  btn.closest = sel => {
    if (sel === "[data-bmact]") return btn;
    if (sel === ".bookmark") return wrap;
    return null;
  };
  roots.bookmarkspane.dispatch("click", { target: btn });

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

/* ---------- send-to picker: one dialogue, shared by bubbles and bookmarks ---------- */

const sendtoNodes = {
  /* titles deliberately avoid the words "Pinned"/"Recent" so the group-label
     assertions below cannot pass on a node title */
  ok:     { id: "ok",     title: "Ok",     lane_id: "lane-a", live: "quiet",  last_interaction: 10 },
  recent: { id: "recent", title: "Newest", lane_id: "lane-b", live: "active", last_interaction: 90 },
  pin:    { id: "pin",    title: "Starred", lane_id: "lane-a", live: "quiet", last_interaction: 1 },
  dead:   { id: "dead",   title: "Dead",   lane_id: "lane-a", live: "exited" },
  closed: { id: "closed", title: "Closed", lane_id: "lane-a", live: "quiet", ended_at: "2026-08-01T00:00:00Z" },
  self:   { id: "self",   title: "Self",   lane_id: "lane-a", live: "quiet",  last_interaction: 99 },
};

test("openSendTo lists only chats that can receive, pinned first then recency", () => {
  const ctx = createFeature({ nodes: sendtoNodes, pinned: ["pin"] });
  const { feature, roots, effects } = ctx;
  feature.bind();

  feature.openSendTo({ text: "hello", exceptId: "self", title: "Send to chat…" });

  assert.equal(effects.openSheet[0], "#sendto");
  const html = roots.sendtoList.innerHTML;
  for (const gone of ["dead", "closed", "self"])
    assert.doesNotMatch(html, new RegExp(`data-fwd="${gone}"`), `${gone} must not be a target`);
  const order = [...html.matchAll(/data-fwd="([^"]+)"/g)].map(m => m[1]);
  assert.deepEqual(order, ["pin", "recent", "ok"]);
});

test("openSendTo labels the two groups only when both are populated", () => {
  const both = createFeature({ nodes: sendtoNodes, pinned: ["pin"] });
  both.feature.bind();
  both.feature.openSendTo({ text: "x", exceptId: "self" });
  assert.match(both.roots.sendtoList.innerHTML, /Pinned/);
  assert.match(both.roots.sendtoList.innerHTML, /Recent/);

  const noPins = createFeature({ nodes: sendtoNodes, pinned: [] });
  noPins.feature.bind();
  noPins.feature.openSendTo({ text: "x", exceptId: "self" });
  assert.doesNotMatch(noPins.roots.sendtoList.innerHTML, /Pinned/);
  assert.doesNotMatch(noPins.roots.sendtoList.innerHTML, /Recent/,
    "a single ungrouped list needs no header");
});

test("openSendTo sets the sheet title per caller and merges into the target draft", () => {
  const ctx = createFeature({ nodes: sendtoNodes, pinned: [], isDesktop: false });
  const { feature, roots, effects, storage } = ctx;
  storage.setItem(DRAFT_KEY_PREFIX + "ok", "existing");
  feature.bind();
  feature.openSendTo({ text: "carried over", exceptId: "self", title: "Send to chat…" });
  assert.equal(roots.sendtoTitle.textContent, "Send to chat…");

  const fwd = el("button", { dataset: { fwd: "ok" } });
  fwd.dataset.fwd = "ok";
  fwd.closest = sel => sel === "[data-fwd]" ? fwd : null;
  roots.sendtoList.onclick({ target: fwd });

  /* append, never clobber — the target may hold a half-typed draft */
  assert.equal(storage.getItem(DRAFT_KEY_PREFIX + "ok"), "existing\n\ncarried over");
  assert.deepEqual(effects.select, ["ok"]);
  assert.equal(roots.chatPrompt._focused, true);
});

test("openSendTo with no live targets still offers Start new chat… (P4)", () => {
  /* Replaces the pre-P4 dead-end message; case 2 is the full pin. */
  const ctx = createFeature({ nodes: { dead: sendtoNodes.dead }, pinned: [] });
  ctx.feature.bind();
  ctx.feature.openSendTo({ text: "x", exceptId: "" });
  assert.doesNotMatch(ctx.roots.sendtoList.innerHTML, /data-fwd=/);
  assert.doesNotMatch(ctx.roots.sendtoList.innerHTML, /no (running|live|open) chat/i);
  assert.match(ctx.roots.sendtoList.innerHTML, /data-newchat/);
  assert.match(ctx.roots.sendtoList.innerHTML, /Start new chat…|Start new chat\u2026/);
});

test("bookmark sendto reuses openSendTo — the crowded picker is filtered too", () => {
  const ctx = createFeature({
    nodes: sendtoNodes,
    pinned: ["pin"],
    bookmarks: [{ t: "t1", text: "send me" }],
  });
  const { feature, roots } = ctx;
  feature.bind();
  feature.setOpen(true);
  const wrap = el("div", { className: "bookmark", dataset: { t: "t1" } });
  wrap.dataset.t = "t1";
  const btn = el("button", { dataset: { bmact: "sendto" } });
  btn.dataset.bmact = "sendto";
  btn.closest = sel => {
    if (sel === "[data-bmact]") return btn;
    if (sel === ".bookmark") return wrap;
    return null;
  };
  roots.bookmarkspane.dispatch("click", { target: btn });

  const order = [...roots.sendtoList.innerHTML.matchAll(/data-fwd="([^"]+)"/g)].map(m => m[1]);
  assert.deepEqual(order, ["pin", "self", "recent", "ok"],
    "same filter and order as the bubble picker (no source chat to exclude here)");
  assert.match(roots.sendtoTitle.textContent, /bookmark/i);
});

test("sendto no-ops when bookmark t missing", () => {
  const ctx = createFeature({ bookmarks: [] });
  const { feature, roots, effects } = ctx;
  feature.bind();
  const wrap = el("div", { className: "bookmark", dataset: { t: "missing" } });
  wrap.dataset.t = "missing";
  const btn = el("button", { dataset: { bmact: "sendto" } });
  btn.dataset.bmact = "sendto";
  btn.closest = sel => {
    if (sel === "[data-bmact]") return btn;
    if (sel === ".bookmark") return wrap;
    return null;
  };
  roots.bookmarkspane.dispatch("click", { target: btn });
  assert.equal(effects.openSheet.length, 0);
});

/* ---------- bind/destroy ---------- */
test("bind is idempotent; destroy removes listeners and send-to owner (no longpress)", () => {
  const ctx = createFeature({ bookmarks: [{ t: "t1", text: "send me" }] });
  const { feature, roots } = ctx;
  feature.bind();
  feature.bind(); /* second bind no-op */
  const n1 = roots.bookmarksbtn._listeners.get("click").length;
  assert.equal(n1, 1);
  /* P6: longpress is no longer bound */
  assert.ok(!roots.bookmarklist._lp);

  /* open send-to via the feature API so destroy can clear the owner */
  feature.openSendTo({ text: "x", title: "t" });
  assert.equal(typeof roots.sendtoList.onclick, "function");

  feature.destroy();
  assert.equal(roots.bookmarksbtn._listeners.get("click").length, 0);
  assert.ok(!roots.bookmarklist._lp);
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

/* ---------- item 5/6/7/9: the shared action-bar builder ---------- */

const ICONS = {
  ICON_JUMP: "<svg id=j/>", ICON_COMMENT: "<svg id=c/>", ICON_COPY: "<svg id=y/>",
  ICON_CLIP: "<svg id=p/>", ICON_TRASH: "<svg id=t/>",
  ICON_INTO: "<svg id=i/>", ICON_MENU_DOTS: "<svg id=m/>",
};

test("bookmarkActionsHTML renders the pane's primary set in order (P3/P5)", () => {
  const html = bookmarkActionsHTML(
    { t: "1", text: "x", node: "n1" },
    /* the pane's FULL set is what it shows with the Notes workspace open;
       "use in note" is withheld while that workspace is closed (item 4) */
    { icons: ICONS, context: "pane", workspaceOpen: true });
  const order = [...html.matchAll(/data-bmact="([a-z]+)"/g)].map(m => m[1]);
  /* P5: former menu entries flattened onto the bar in menu order */
  assert.deepEqual(order, ["jump", "comment", "note", "copy", "sendto", "del"]);
});

test("bookmarkActionsHTML omits comment from the inbox bar — reply composer is pane-only (P3/P5)", () => {
  const html = bookmarkActionsHTML(
    { t: "1", text: "x", node: "n1" }, { icons: ICONS, context: "inbox" });
  const order = [...html.matchAll(/data-bmact="([a-z]+)"/g)].map(m => m[1]);
  assert.deepEqual(order, ["jump", "copy", "note", "sendto", "del"]);
  assert.ok(!order.includes("comment"));
});

test("bookmarkActionsHTML offers jump in the inbox whenever the bookmark is addressable (item 5)", () => {
  /* the whole point of item 5: reaching the chat must not require first
     placing the bookmark into a section */
  const live = bookmarkActionsHTML({ t: "1", text: "x", node: "n1" }, { icons: ICONS, context: "inbox" });
  assert.match(live, /data-bmact="jump"/);
  const archived = bookmarkActionsHTML({ t: "1", text: "x", uid: "u1" }, { icons: ICONS, context: "inbox" });
  assert.match(archived, /data-bmact="jump"/);
});

test("bookmarkActionsHTML drops jump only when the bookmark has no address at all", () => {
  const manual = bookmarkActionsHTML({ t: "1", text: "typed by hand" }, { icons: ICONS, context: "pane" });
  assert.ok(!manual.includes(`data-bmact="jump"`));
});

test("bookmarkActionsHTML withholds comment from a comment (no nesting)", () => {
  /* P3/P5: comment is on the pane bar when available; no nesting promotes copy */
  const html = bookmarkActionsHTML(
    { t: "2", text: "x", node: "n1", anchor: "1" },
    { icons: ICONS, context: "pane", workspaceOpen: true });
  assert.ok(!html.includes(`data-bmact="comment"`));
  assert.match(html, /data-bmact="copy"/, "copy promoted when comment withheld");
  assert.doesNotMatch(html, /data-bmact="more"/);
  assert.match(html, /data-bmact="del"/);
});

test("bookmarkActionsHTML uses the shared .actionbar/.btn-plain classes, not per-surface geometry", () => {
  const html = bookmarkActionsHTML({ t: "1", text: "x", node: "n1" }, { icons: ICONS });
  assert.match(html, /class="actionbar tear"/);
  assert.match(html, /class="btn-plain"/);
  assert.ok(!html.includes("bookmarkactions"),
    "the old per-surface class must be gone so pane and inbox cannot drift apart");
});

/* review 2, item 4: "use in note" starts placement mode, whose targets are the
   section rows INSIDE the Notes workspace. Offering it from the compact pane
   while that workspace is closed arms a mode the user cannot see or complete;
   the inbox (which only exists inside the workspace) always offers it. */
test("bookmarkActionsHTML withholds use-in-note from the pane while the workspace is closed", () => {
  const html = bookmarkActionsHTML(
    { t: "1", text: "x", node: "n1" }, { icons: ICONS, context: "pane", workspaceOpen: false });
  assert.ok(!html.includes(`data-bmact="note"`),
    "placement has no visible target while the Notes workspace is closed");
  assert.match(html, /data-bmact="jump"/, "the other pane actions must survive");
  /* P5: flat bar keeps del last; more is gone; note withheld as before */
  assert.doesNotMatch(html, /data-bmact="more"/);
  assert.match(html, /data-bmact="del"/);
});

test("bookmarkActionsHTML offers use-in-note from the pane once the workspace is open", () => {
  const html = bookmarkActionsHTML(
    { t: "1", text: "x", node: "n1" }, { icons: ICONS, context: "pane", workspaceOpen: true });
  assert.match(html, /data-bmact="note"/);
});

/* ...but the phone has no side-by-side workspace to reach the inbox in: its
   zones are shown one at a time, so the pane's paperclip is the direct route
   into placement (it opens the workspace in placing mode). The gate is about
   an invisible armed mode, and on a single-zone layout the mode is not
   invisible — it is the next screen. */
test("bookmarkActionsHTML keeps use-in-note in the pane on a single-zone (phone) layout", () => {
  const html = bookmarkActionsHTML(
    { t: "1", text: "x", node: "n1" },
    { icons: ICONS, context: "pane", workspaceOpen: false, singleZone: true });
  assert.match(html, /data-bmact="note"/);
});

test("the single-zone exception does not leak to the multi-zone layout", () => {
  const html = bookmarkActionsHTML(
    { t: "1", text: "x", node: "n1" },
    { icons: ICONS, context: "pane", workspaceOpen: false, singleZone: false });
  assert.ok(!html.includes(`data-bmact="note"`));
});

test("bookmarkListHTML carries the single-zone exception through to the row", () => {
  const html = bookmarkListHTML({
    list: [{ t: "1", text: "hello", node: "n1" }],
    bookmarkTab: "GENERAL",
    openBookmarkT: "1",
    nodeById: () => ({ id: "n1", title: "Act" }),
    icons: ICONS,
    workspaceOpen: false,
    singleZone: true,
  });
  assert.match(html, /data-bmact="note"/);
});

test("the inbox offers use-in-note regardless of the flag: it lives in the workspace", () => {
  const html = bookmarkActionsHTML(
    { t: "1", text: "x", node: "n1" }, { icons: ICONS, context: "inbox" });
  assert.match(html, /data-bmact="note"/);
});

test("bookmarkListHTML withholds use-in-note when the workspace is closed", () => {
  const html = bookmarkListHTML({
    list: [{ t: "1", text: "hello", node: "n1" }],
    bookmarkTab: "GENERAL",
    openBookmarkT: "1",
    nodeById: () => ({ id: "n1", title: "Act" }),
    icons: ICONS,
    workspaceOpen: false,
  });
  assert.ok(!html.includes(`data-bmact="note"`));
});

test("bookmarkListHTML passes an open workspace through to the action row", () => {
  const html = bookmarkListHTML({
    list: [{ t: "1", text: "hello", node: "n1" }],
    bookmarkTab: "GENERAL",
    openBookmarkT: "1",
    nodeById: () => ({ id: "n1", title: "Act" }),
    icons: ICONS,
    workspaceOpen: true,
  });
  assert.match(html, /data-bmact="note"/);
});

test("bookmarkListHTML emits the shared action bar for the open bookmark", () => {
  const html = bookmarkListHTML({
    list: [{ t: "1", text: "hello", node: "n1" }],
    bookmarkTab: "GENERAL",
    openBookmarkT: "1",
    nodeById: () => ({ id: "n1", title: "Act" }),
    icons: ICONS,
  });
  assert.match(html, /class="actionbar tear"/);
  assert.match(html, /data-bmact="jump"/);
});

/* ---------- P3: caret at the end after openSendTo merges into the composer ---------- */

test("P3 13: openSendTo places the caret after the merged draft in #prompt", () => {
  assert.match(bookmarksSrc, /from "\.\/caret\.js"/, "bookmarks imports caret.js");
  assert.match(bookmarksSrc, /focusAtEnd\s*\(/, "openSendTo uses focusAtEnd on #prompt");

  /* Production order (verified at 9c74086):
       1. storage.setItem(draft)            — bookmarks.js ~713
       2. d.select(tgt)                     — app.js select → composer.onSelect
          → setPromptText(storage draft)    — composer.js:357 paints #prompt
       3. prompt.focus()                    — bare today; must become focusAtEnd
     By focus time the text is already in the DOM. The unit harness mocks
     select, so the stub paints the draft onto #prompt the same way
     composer.onSelect would. */

  const ranges = [];
  const selection = {
    ranges,
    removeAllRanges(){ ranges.length = 0; },
    addRange(r){ ranges.push(r); },
  };
  const ownerDocument = {
    createRange(){
      return {
        startContainer: null, startOffset: 0,
        endContainer: null, endOffset: 0,
        collapsed: false,
        setStart(n, o){ this.startContainer = n; this.startOffset = o; },
        setEnd(n, o){
          this.endContainer = n; this.endOffset = o;
          this.collapsed = this.startContainer === n && this.startOffset === o;
        },
        selectNodeContents(n){
          this.startContainer = n; this.startOffset = 0;
          this.endContainer = n;
          this.endOffset = (n.textContent || "").length;
        },
        collapse(toStart){
          if (toStart){
            this.endContainer = this.startContainer;
            this.endOffset = this.startOffset;
          } else {
            this.startContainer = this.endContainer;
            this.startOffset = this.endOffset;
          }
          this.collapsed = true;
        },
      };
    },
    getSelection(){ return selection; },
  };

  let storageRef = null;
  let promptRef = null;
  const effects = { select: [], openSheet: [], closeSheets: [], setLevel: [] };

  const ctx = createFeature({
    nodes: sendtoNodes,
    pinned: [],
    isDesktop: false,
    deps: {
      select: id => {
        effects.select.push(id);
        /* Mirror composer.onSelect → setPromptText(draft from storage). */
        const draft = (storageRef && storageRef.getItem(DRAFT_KEY_PREFIX + id)) || "";
        if (promptRef) promptRef.textContent = draft;
      },
      closeSheets: () => { effects.closeSheets.push(1); },
      setLevel: n => { effects.setLevel.push(n); },
    },
  });
  const { feature, roots, storage } = ctx;
  storageRef = storage;
  storage.setItem(DRAFT_KEY_PREFIX + "ok", "existing");

  const prompt = roots.chatPrompt;
  promptRef = prompt;
  const textNode = { nodeType: 3, textContent: "", lastChild: null, parentNode: prompt };
  prompt.isContentEditable = true;
  prompt.contentEditable = "true";
  prompt.ownerDocument = ownerDocument;
  prompt.childNodes = [];
  prompt.lastChild = null;
  let _text = "";
  Object.defineProperty(prompt, "textContent", {
    configurable: true,
    get(){ return _text; },
    set(v){
      _text = String(v || "");
      textNode.textContent = _text;
      if (_text){
        prompt.childNodes = [textNode];
        prompt.lastChild = textNode;
      } else {
        prompt.childNodes = [];
        prompt.lastChild = null;
      }
    },
  });
  prompt._focused = false;
  prompt.focus = function(){ this._focused = true; };

  feature.bind();
  feature.openSendTo({ text: "carried over", exceptId: "self", title: "Send to chat…" });

  const fwd = el("button", { dataset: { fwd: "ok" } });
  fwd.dataset.fwd = "ok";
  fwd.closest = sel => sel === "[data-fwd]" ? fwd : null;
  roots.sendtoList.onclick({ target: fwd });

  const merged = "existing\n\ncarried over";
  assert.equal(storage.getItem(DRAFT_KEY_PREFIX + "ok"), merged);
  assert.deepEqual(effects.select, ["ok"]);
  assert.equal(prompt.textContent, merged,
    "composer has the merged draft before caret placement");
  assert.equal(prompt._focused, true, "#prompt focused after send-to");
  assert.equal(selection.ranges.length, 1, "contenteditable selection set");
  const r = selection.ranges[0];
  assert.equal(r.collapsed, true, "nothing pre-selected");
  assert.equal(r.startContainer, textNode, "caret on the draft text node");
  assert.equal(r.startOffset, merged.length, "caret AFTER the text you just sent");
});

/* ---------- P4: Start new chat… in the Send-to picker ---------- */

/** Click target that resolves [data-newchat] before [data-fwd] (handler order). */
function newChatClickTarget(){
  const btn = el("button", { dataset: { newchat: "" } });
  btn.dataset.newchat = "";
  btn.closest = sel => {
    if (sel === "[data-newchat]") return btn;
    if (sel === "[data-fwd]") return null;
    return null;
  };
  return btn;
}

/** Harness with openNewActivity spy + optional field stubs for cases 4–6. */
function createSendToWithNewChat(overrides = {}){
  const openNewActivityCalls = [];
  const forkFromTurnCalls = [];
  const nc_title = el("input", { id: "nc_title", value: "stale-title" });
  const nc_prompt = el("textarea", { id: "nc_prompt", value: "stale-prompt" });
  /* Simulate sheets.openNewActivity: clear parent/rationale, set prompt, focus title. */
  let ncParent = overrides.staleParent || "stale-parent";
  let ncRationale = overrides.staleRationale || "stale-rationale";
  const ctx = createFeature({
    ...overrides,
    deps: {
      ...(overrides.deps || {}),
      openNewActivity: (opts = {}) => {
        const { prompt = "", focusTitle = false } = opts;
        openNewActivityCalls.push(opts);
        /* Mirror the real function's clears — bookmarks only wins if it calls this. */
        ncParent = "";
        ncRationale = "";
        nc_title.value = "";
        nc_prompt.value = prompt;
        if (focusTitle){
          nc_title._focused = true;
        }
        ctx.effects.openSheet.push("#newchat");
      },
      forkFromTurn: (...a) => { forkFromTurnCalls.push(a); },
    },
  });
  return {
    ...ctx,
    openNewActivityCalls,
    forkFromTurnCalls,
    nc_title,
    nc_prompt,
    getNcParent: () => ncParent,
    getNcRationale: () => ncRationale,
  };
}

test("P4 case 1: with eligible targets, Start new chat… is the last entry", () => {
  const ctx = createFeature({ nodes: sendtoNodes, pinned: ["pin"] });
  ctx.feature.bind();
  ctx.feature.openSendTo({ text: "hello", exceptId: "self", title: "Send to chat…" });
  const html = ctx.roots.sendtoList.innerHTML;
  assert.match(html, /Start new chat…|Start new chat\u2026/,
    "trailing entry labelled Start new chat…");
  assert.match(html, /data-newchat/, "new-chat entry uses a distinct attribute");
  /* Last interactive entry is the new-chat button, after every data-fwd target. */
  const lastFwd = html.lastIndexOf("data-fwd=");
  const lastNew = Math.max(html.lastIndexOf("data-newchat"), html.lastIndexOf("Start new chat"));
  assert.ok(lastNew > lastFwd, "Start new chat… is after all eligible targets");
  /* Visually separated from the target list. */
  assert.ok(
    /sendto-new|sendto-sep|border-top|role="separator"/.test(html),
    "new-chat entry is visually separated",
  );
});

test("P4 case 2: no eligible targets — entry still present; dead-end message gone", () => {
  const ctx = createFeature({ nodes: { dead: sendtoNodes.dead }, pinned: [] });
  ctx.feature.bind();
  ctx.feature.openSendTo({ text: "x", exceptId: "" });
  const html = ctx.roots.sendtoList.innerHTML;
  assert.match(html, /data-newchat/);
  assert.match(html, /Start new chat…|Start new chat\u2026/);
  assert.doesNotMatch(html, /no (running|live|open) chat/i,
    "bare dead-end message is replaced by Start new chat…");
  assert.doesNotMatch(html, /data-fwd=/);
});

test("P4 case 3: tap closes #sendto and opens #newchat", () => {
  const ctx = createSendToWithNewChat({
    nodes: sendtoNodes, pinned: [], isDesktop: true,
  });
  ctx.feature.bind();
  ctx.feature.openSendTo({ text: "carried", exceptId: "self" });
  assert.equal(ctx.effects.openSheet[0], "#sendto");
  ctx.roots.sendtoList.onclick({ target: newChatClickTarget() });
  assert.ok(ctx.effects.closeSheets.length >= 1, "picker closes");
  assert.ok(ctx.effects.openSheet.includes("#newchat"), "#newchat opens");
  /* closeSheets before openNewActivity — never reverse. */
  const closeAt = 0; /* closeSheets is a count push; openNewActivity after it */
  assert.equal(ctx.effects.closeSheets.length, 1);
  assert.equal(ctx.openNewActivityCalls.length, 1);
});

test("P4 case 4: tap prefills nc_prompt from the carried text", () => {
  const ctx = createSendToWithNewChat({ nodes: sendtoNodes, pinned: [] });
  ctx.feature.bind();
  ctx.feature.openSendTo({ text: "bubble text here", exceptId: "self" });
  ctx.roots.sendtoList.onclick({ target: newChatClickTarget() });
  assert.equal(ctx.openNewActivityCalls.length, 1);
  assert.equal(ctx.openNewActivityCalls[0].prompt, "bubble text here");
  assert.equal(ctx.nc_prompt.value, "bubble text here");
});

test("P4 case 5: tap leaves nc_title empty and focused", () => {
  const ctx = createSendToWithNewChat({ nodes: sendtoNodes, pinned: [] });
  ctx.feature.bind();
  ctx.feature.openSendTo({ text: "x", exceptId: "self" });
  ctx.roots.sendtoList.onclick({ target: newChatClickTarget() });
  assert.equal(ctx.openNewActivityCalls[0].focusTitle, true);
  assert.equal(ctx.nc_title.value, "");
  assert.equal(ctx.nc_title._focused, true);
});

test("P4 case 6: tap leaves ncParent and ncRationale empty (no lineage)", () => {
  const ctx = createSendToWithNewChat({
    nodes: sendtoNodes, pinned: [],
    staleParent: "would-be-parent",
    staleRationale: "would-be-rationale",
  });
  ctx.feature.bind();
  ctx.feature.openSendTo({ text: "x", exceptId: "self" });
  assert.equal(ctx.getNcParent(), "would-be-parent", "precondition: stale parent");
  ctx.roots.sendtoList.onclick({ target: newChatClickTarget() });
  assert.equal(ctx.openNewActivityCalls.length, 1,
    "path goes through openNewActivity (structural no-parent)");
  assert.equal(ctx.getNcParent(), "");
  assert.equal(ctx.getNcRationale(), "");
  assert.equal(ctx.effects.select.length, 0, "no select — there is no node yet");
});

test("P4 case 8: Start new chat never calls forkFromTurn", () => {
  const ctx = createSendToWithNewChat({ nodes: sendtoNodes, pinned: [] });
  ctx.feature.bind();
  ctx.feature.openSendTo({ text: "from bubble", exceptId: "self", title: "Send to chat…" });
  ctx.roots.sendtoList.onclick({ target: newChatClickTarget() });
  assert.equal(ctx.forkFromTurnCalls.length, 0);
  assert.equal(ctx.openNewActivityCalls.length, 1);
  /* Product must not import sheets or call forkFromTurn from bookmarks. */
  assert.doesNotMatch(bookmarksSrc, /forkFromTurn/);
  assert.doesNotMatch(bookmarksSrc, /from "\.\/sheets\.js"/);
});

test("P4 case 9: existing targets keep order and filtering (regression — pass at red)", () => {
  /* sendableNodes is untouched; this locks the pre-P4 target list. */
  const ctx = createFeature({ nodes: sendtoNodes, pinned: ["pin"] });
  ctx.feature.bind();
  ctx.feature.openSendTo({ text: "hello", exceptId: "self", title: "Send to chat…" });
  const html = ctx.roots.sendtoList.innerHTML;
  for (const gone of ["dead", "closed", "self"])
    assert.doesNotMatch(html, new RegExp(`data-fwd="${gone}"`), `${gone} must not be a target`);
  const order = [...html.matchAll(/data-fwd="([^"]+)"/g)].map(m => m[1]);
  assert.deepEqual(order, ["pin", "recent", "ok"]);
});

test("P4 new-chat entry is a keyboard-reachable button with pos-item class", () => {
  const ctx = createFeature({ nodes: sendtoNodes, pinned: [] });
  ctx.feature.bind();
  ctx.feature.openSendTo({ text: "x", exceptId: "self" });
  const html = ctx.roots.sendtoList.innerHTML;
  assert.match(html, /<button\b[^>]*class="pos-item"[^>]*data-newchat/,
    "button.pos-item[data-newchat] — same 44px path as other .pos-item rows");
});

test("P4 phone path: Start new chat sets level 1 like send-to switch", () => {
  const ctx = createSendToWithNewChat({
    nodes: sendtoNodes, pinned: [], isDesktop: false,
  });
  ctx.feature.bind();
  ctx.feature.openSendTo({ text: "x", exceptId: "self" });
  ctx.roots.sendtoList.onclick({ target: newChatClickTarget() });
  assert.deepEqual(ctx.effects.setLevel, [1]);
});

test("P4 data-newchat branch runs before data-fwd (no phantom chat id)", () => {
  /* If the entry carried data-fwd, select() would open a chat named after it. */
  const ctx = createSendToWithNewChat({ nodes: sendtoNodes, pinned: [] });
  ctx.feature.bind();
  ctx.feature.openSendTo({ text: "x", exceptId: "self" });
  const html = ctx.roots.sendtoList.innerHTML;
  const newChatChunk = html.slice(html.indexOf("data-newchat") - 80);
  assert.doesNotMatch(newChatChunk, /data-fwd=/,
    "new-chat entry must not carry data-fwd");
  ctx.roots.sendtoList.onclick({ target: newChatClickTarget() });
  assert.equal(ctx.effects.select.length, 0);
  assert.equal(ctx.openNewActivityCalls.length, 1);
});


/* ---------- P6 review: the send-to route, and one click per tap ---------- */

/* P6's headline guarantee — every send-to route strips scimux-asset: markers —
   is pinned in actionbar.test.js only by the identifier appearing somewhere in
   bookmarks.js/notes.js, which the import statement satisfies on its own.
   Deleting the call survives the whole suite. These drive the real path. */
test("P6 review: pane send-to strips asset markers and excepts the source chat", () => {
  const ctx = createFeature({
    bookmarks: [{ t: "t1", node: "n1", text: "see ![shot](scimux-asset:a_1) here" }],
    isDesktop: true,
  });
  const { feature, roots, storage } = ctx;
  feature.bind();
  feature.setOpen(true);

  const wrap = el("div", { className: "bookmark", dataset: { t: "t1" } });
  wrap.dataset.t = "t1";
  const btn = el("button", { dataset: { bmact: "sendto" } });
  btn.dataset.bmact = "sendto";
  btn.closest = sel => {
    if (sel === "[data-bmact]") return btn;
    if (sel === ".bookmark") return wrap;
    return null;
  };
  roots.bookmarkspane.dispatch("click", { target: btn });

  /* the bookmark's own chat cannot be a target for its own text */
  assert.doesNotMatch(roots.sendtoList.innerHTML, /data-fwd="n1"/,
    "exceptId must withhold the source chat");
  assert.match(roots.sendtoList.innerHTML, /data-fwd="n2"/);

  const fwd = el("button", { dataset: { fwd: "n2" } });
  fwd.dataset.fwd = "n2";
  fwd.closest = sel => (sel === "[data-fwd]" ? fwd : null);
  roots.sendtoList.onclick({ target: fwd });

  assert.equal(storage.getItem(DRAFT_KEY_PREFIX + "n2"), "see shot here",
    "asset markers are stripped before the text reaches the target draft");
});

test("P6 review: an overflow-menu click runs its action once, not twice", () => {
  /* The popover is appended to #bookmarkspane — the very element that carries
     the delegated [data-bmact] handler — and the menu's own listener neither
     stops propagation nor clears the stored target. A real browser therefore
     delivers the click twice: once to the menu listener, once by bubbling to
     the pane. Headless fakes do not bubble, so the second delivery is
     simulated here exactly as the DOM would (skipped once propagation stops). */
  const created = [];
  const ctx = createFeature({
    bookmarks: [{ t: "t1", node: "n1", text: "copy me" }],
    isDesktop: true,
  });
  ctx.document.createElement = tag => {
    const n = el(tag);
    created.push(n);
    return n;
  };
  const { feature, roots, effects } = ctx;
  feature.bind();
  feature.setOpen(true);

  const wrap = el("div", { className: "bookmark", dataset: { t: "t1" } });
  wrap.dataset.t = "t1";
  const more = el("button", { dataset: { bmact: "more" } });
  more.dataset.bmact = "more";
  more.closest = sel => {
    if (sel === "[data-bmact]") return more;
    if (sel === ".bookmark") return wrap;
    return null;
  };
  roots.bookmarkspane.dispatch("click", { target: more });
  const menu = created[created.length - 1];
  assert.ok(menu, "overflow menu element created");

  const item = el("button", { dataset: { bmact: "copy" } });
  item.dataset.bmact = "copy";
  item.closest = sel => (sel === "[data-bmact]" ? item : null);

  let stopped = false;
  menu.dispatch("click", { target: item, stopPropagation(){ stopped = true; } });
  if (!stopped) roots.bookmarkspane.dispatch("click", { target: item });

  assert.deepEqual(effects.copy, ["copy me"],
    "the menu item's action must not also fire on the delegated pane handler");
});
