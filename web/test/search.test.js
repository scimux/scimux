/* Characterization tests for web/js/search.js — pure decisions + factory
 * adapters with a small fake DOM, controlled timers/fetch/AbortController.
 * No browser emulator; no mutable production test/debug accessors. */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import {
  SEARCH_MIN,
  SEARCH_MAX,
  SEARCH_DEBOUNCE_MS,
  RECENTS_KEY,
  RECENTS_CAP,
  RECENTS_SHOW,
  normalizeSearchQuery,
  isSearchQueryReady,
  loadRecents,
  recordRecent,
  clearRecents,
  recentsHTML,
  searchBlankHTML,
  searchHitHTML,
  searchGroupHTML,
  searchFeedHTML,
  buildPendingJump,
  createSearchFeature,
} from "../js/search.js";
import { esc } from "../js/format.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const searchSrc = readFileSync(join(__dirname, "../js/search.js"), "utf8");

/* ---------- module shape ---------- */
test("search.js exports factory and pure helpers; no later-feature imports", () => {
  assert.match(searchSrc, /export function createSearchFeature/);
  assert.match(searchSrc, /from "\.\/format\.js"/);
  assert.doesNotMatch(searchSrc, /from "\.\/bookmarks\.js"/);
  assert.doesNotMatch(searchSrc, /from "\.\/sheets\.js"/);
  assert.doesNotMatch(searchSrc, /from "\.\/polling\.js"/);
  assert.doesNotMatch(searchSrc, /from "\.\/app\.js"/);
  assert.doesNotMatch(searchSrc, /from "\.\/notes\.js"/);
  assert.doesNotMatch(searchSrc, /from "\.\/chat\.js"/);
  assert.doesNotMatch(searchSrc, /from "\.\/composer\.js"/);
  assert.doesNotMatch(searchSrc, /from "\.\/cards\.js"/);
  assert.doesNotMatch(searchSrc, /from "\.\/map\.js"/);
  assert.doesNotMatch(searchSrc, /test\/debug accessors|getSearchSeq|_openHit/);
});

test("constants: min/max/debounce/recents caps and labels", () => {
  assert.equal(SEARCH_MIN, 2);
  assert.equal(SEARCH_MAX, 128);
  assert.equal(SEARCH_DEBOUNCE_MS, 180);
  assert.equal(RECENTS_KEY, "scimux-search-recents");
  assert.equal(RECENTS_CAP, 8);
  assert.equal(RECENTS_SHOW, 5);
});

/* ---------- query clamp/gate ---------- */
test("normalizeSearchQuery trims and clamps to SEARCH_MAX", () => {
  assert.equal(normalizeSearchQuery("  ab  "), "ab");
  assert.equal(normalizeSearchQuery("a".repeat(SEARCH_MAX + 40)).length, SEARCH_MAX);
  assert.equal(normalizeSearchQuery(null), "");
  assert.equal(normalizeSearchQuery(undefined), "");
});

test("isSearchQueryReady gates on SEARCH_MIN after normalize", () => {
  assert.equal(isSearchQueryReady("a"), false);
  assert.equal(isSearchQueryReady("  a  "), false);
  assert.equal(isSearchQueryReady("ab"), true);
  assert.equal(isSearchQueryReady("  ab  "), true);
});

/* ---------- recents ---------- */
function makeStorage(init = {}){
  const m = new Map(Object.entries(init));
  return {
    getItem: k => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => { m.set(k, String(v)); },
    removeItem: k => { m.delete(k); },
    _map: m,
  };
}

test("loadRecents: empty, valid, corrupt, non-array, non-string filter", () => {
  assert.deepEqual(loadRecents(null), []);
  assert.deepEqual(loadRecents(makeStorage()), []);
  const s = makeStorage({ [RECENTS_KEY]: JSON.stringify(["a", "b"]) });
  assert.deepEqual(loadRecents(s), ["a", "b"]);
  assert.deepEqual(loadRecents(makeStorage({ [RECENTS_KEY]: "not-json" })), []);
  assert.deepEqual(loadRecents(makeStorage({ [RECENTS_KEY]: "null" })), []);
  assert.deepEqual(loadRecents(makeStorage({ [RECENTS_KEY]: JSON.stringify({ x: 1 }) })), []);
  assert.deepEqual(loadRecents(makeStorage({ [RECENTS_KEY]: JSON.stringify(["ok", 1, null, "b"]) })), ["ok", "b"]);
});

test("loadRecents: getItem throw returns []", () => {
  assert.deepEqual(loadRecents({
    getItem(){ throw new Error("quota"); },
  }), []);
});

test("recordRecent: gate, trim, clamp, dedupe move-to-front, cap", () => {
  const s = makeStorage();
  recordRecent("a", s);
  assert.deepEqual(loadRecents(s), []);
  recordRecent("auth", s);
  recordRecent(" bug ", s);
  recordRecent("auth", s);
  assert.deepEqual(loadRecents(s), ["auth", "bug"]);
  recordRecent("z".repeat(SEARCH_MAX + 50), s);
  assert.equal(loadRecents(s)[0].length, SEARCH_MAX);
  for (let i = 0; i < 20; i++) recordRecent("q" + i, s);
  assert.ok(loadRecents(s).length <= RECENTS_CAP);
  assert.equal(loadRecents(s)[0], "q19");
});

test("recordRecent: setItem failure is silent", () => {
  const s = {
    getItem: () => "[]",
    setItem(){ throw new Error("quota"); },
    removeItem(){},
  };
  assert.doesNotThrow(() => recordRecent("hello", s));
});

test("clearRecents empties store; removeItem failure silent", () => {
  const s = makeStorage({ [RECENTS_KEY]: JSON.stringify(["a"]) });
  clearRecents(s);
  assert.deepEqual(loadRecents(s), []);
  assert.doesNotThrow(() => clearRecents({
    getItem: () => null,
    setItem(){},
    removeItem(){ throw new Error("x"); },
  }));
});

test("recentsHTML: cap SHOW, escape, clear button; empty when none", () => {
  assert.equal(recentsHTML(makeStorage(), esc), "");
  const s = makeStorage();
  recordRecent("<img src=x>", s);
  recordRecent("hello", s);
  const htmlEsc = recentsHTML(s, esc);
  assert.match(htmlEsc, /class="recents"/);
  assert.match(htmlEsc, /id="recentsclear"/);
  assert.match(htmlEsc, /data-recent="hello"/);
  assert.ok(htmlEsc.includes("&lt;img src=x&gt;") && !htmlEsc.includes("<img src=x>"));
  for (let i = 0; i < 10; i++) recordRecent("r" + i, s);
  const html = recentsHTML(s, esc);
  const shown = (html.match(/class="recent"/g) || []).length;
  assert.ok(shown <= RECENTS_SHOW);
  assert.match(html, /data-recent="r9"/);
  assert.equal(searchBlankHTML(makeStorage(), esc),
    `<div class="empty">Search your current and past chats and notes.</div>`);
});

/* ---------- hit / group / feed HTML ---------- */
test("searchHitHTML escapes excerpt and stamps identity attributes", () => {
  const out = searchHitHTML({
    before: "a <b>", match: "x&y", after: "</b> z",
    time: "2026-07-26T10:00:00",
    role: "user", segment: 2, record: 3,
    _kind: "live", _id: "n1", _uid: "u1", _lane: "L",
  }, esc);
  assert.ok(out.includes("a &lt;b&gt;<mark>x&amp;y</mark>&lt;/b&gt; z"));
  assert.ok(!out.includes("<b>") && !out.includes("</b>"));
  assert.match(out, /data-role="user"/);
  assert.match(out, /data-segment="2"/);
  assert.match(out, /data-record="3"/);
  assert.doesNotMatch(out, /data-forkable/);
  assert.match(out, /data-kind="live"/);
  assert.match(out, /data-id="n1"/);
  assert.match(out, /data-uid="u1"/);
  assert.match(out, /data-lane="L"/);
});

test("searchGroupHTML stamps group identity onto hits; kind labels", () => {
  const g = {
    kind: "archived", id: "gone", uid: "u9", lane_id: "lane1",
    agent: "claude", title: "Old <x>",
    hits: [{ before: "hi ", match: "there", after: "", turn_time: "T1", role: "assistant", segment: 1, record: 4 }],
  };
  const html = searchGroupHTML(g, {
    esc,
    laneColor: id => id === "lane1" ? "#f00" : "",
    agentLogo: a => `LOGO(${a})`,
  });
  assert.match(html, /class="searchgroup"/);
  assert.match(html, /sgkind">past</);
  assert.match(html, /LOGO\(claude\)/);
  assert.match(html, /background:#f00/);
  assert.match(html, /Old &lt;x&gt;/);
  assert.match(html, /data-kind="archived"/);
  assert.match(html, /data-uid="u9"/);
  const bm = searchGroupHTML({ kind: "bookmarks", id: "b", hits: [{ match: "n" }] }, { esc, laneColor: () => "", agentLogo: () => "" });
  assert.match(bm, /sgkind">bookmarks</);
});

/* A bookmark carries its own stamped address, and in the Bookmarks bucket there
   is no group address to inherit — the bucket is a bag of bookmarks from
   everywhere. Reading the uid off the group there threw the address away, which
   went unnoticed while the tap only ever opened the pane. Now the tap previews
   the source turn, so the hit's own address has to win. */
test("a hit's own address outranks its group's", () => {
  const bucket = searchGroupHTML({
    kind: "bookmarks", title: "Bookmarks",
    hits: [{ match: "n", uid: "u-own", segment: 2, record: 7 }],
  }, { esc, laneColor: () => "", agentLogo: () => "" });
  assert.match(bucket, /data-uid="u-own"/);
  assert.match(bucket, /data-segment="2"/);
  assert.match(bucket, /data-record="7"/);

  /* A log hit has no uid of its own and still inherits the group's. */
  const live = searchGroupHTML({
    kind: "live", id: "n1", uid: "u-group",
    hits: [{ match: "n", segment: 0, record: 1 }],
  }, { esc, laneColor: () => "", agentLogo: () => "" });
  assert.match(live, /data-uid="u-group"/);
});

test("searchFeedHTML: empty, groups, partial footer", () => {
  assert.equal(searchFeedHTML({ groups: [] }, { esc }),
    `<div class="empty">No matches.</div>`);
  const html = searchFeedHTML({
    groups: [{ kind: "live", id: "n1", title: "A", hits: [{ match: "x" }] }],
    partial: true,
  }, { esc, laneColor: () => "", agentLogo: () => "" });
  assert.match(html, /class="searchgroup"/);
  assert.match(html, /class="searchmore"/);
  assert.match(html, /Showing the most recent matches/);
});

/* ---------- no action bar at all ----------
   The per-hit bar was the whole confusion. It appeared only after the tap, and
   what it contained depended on facts the user could not see: three buttons for
   a live chat whose directory still existed, two if the directory was gone, two
   for a past chat, one for an asset. Nobody outside the project could explain
   why one hit offered "Fork from here" and its neighbour did not — and the
   explanation, once given, was about scimux's bookkeeping, not about the search.

   So the bar is gone, not rearranged. A hit does one thing. */
test("search.js builds no per-hit action bar", () => {
  assert.doesNotMatch(searchSrc, /data-sact/, "the action bar's contract is retired");
  assert.doesNotMatch(searchSrc, /hitbar/, "no bar element is constructed any more");
  assert.doesNotMatch(searchSrc, /SACT_LABEL|searchHitActions|hitBarHTML|showActionDecision/,
    "the adaptive-action vocabulary must not survive as dead exports");
});

/* Fork went with it, and not merely because the bar did. A deleted chat cannot
   be opened — the node is gone, which the UI states plainly — so offering to
   fork one was the same impossibility with a friendlier verb. Fork now lives
   exactly where the other per-turn actions live: inside a real chat. */
test("search.js neither forks nor files bookmarks", () => {
  assert.doesNotMatch(searchSrc, /forkFromTurn/, "forking is a chat action, not a search result action");
  assert.doesNotMatch(searchSrc, /buildSearchBookmark|uiMutate|stampAddress/,
    "filing a bookmark belongs to the chat the hit points at");
});

test("buildPendingJump durable address + clock", () => {
  assert.deepEqual(buildPendingJump("n1", "T", { uid: "u", segment: "2", record: "3" }, 99), {
    node: "n1", turnTime: "T", text: "", ts: 99,
    uid: "u", segment: 2, record: 3,
  });
  /* original: segment: addr && +addr.segment — null addr short-circuits to null */
  assert.deepEqual(buildPendingJump("n1", "", null, 1), {
    node: "n1", turnTime: "", text: "", ts: 1,
    uid: "", segment: null, record: null,
  });
});

/* ---------- fake DOM / factory harness ---------- */
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
    isContentEditable: !!props.isContentEditable,
    get classList(){
      return {
        add(c){
          const parts = new Set((node.className || "").split(/\s+/).filter(Boolean));
          parts.add(c);
          node.className = [...parts].join(" ");
        },
        remove(c){
          node.className = (node.className || "").split(/\s+/).filter(x => x && x !== c).join(" ");
        },
        contains(c){ return (node.className || "").split(/\s+/).includes(c); },
        toggle(c, force){
          if (force === true) this.add(c);
          else if (force === false) this.remove(c);
          else if (this.contains(c)) this.remove(c);
          else this.add(c);
        },
      };
    },
    addEventListener(type, fn){
      (listeners[type] || (listeners[type] = [])).push(fn);
    },
    removeEventListener(type, fn){
      if (!listeners[type]) return;
      listeners[type] = listeners[type].filter(f => f !== fn);
    },
    dispatch(type, ev = {}){
      const e = Object.assign({
        type, target: node, currentTarget: node,
        preventDefault(){}, stopPropagation(){},
        metaKey: false, ctrlKey: false, altKey: false, shiftKey: false, key: "",
      }, ev);
      if (!e.target) e.target = node;
      (listeners[type] || []).forEach(fn => fn(e));
    },
    focus(){ node._focused = true; documentRef.activeElement = node; },
    blur(){ node._focused = false; },
    remove(){
      if (node.parentNode && node.parentNode.children)
        node.parentNode.children = node.parentNode.children.filter(c => c !== node);
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
      if (matchSel(child, ".hitbar") || child.className === "hitbar")
        child.remove = () => {
          node.children = node.children.filter(c => c !== child);
        };
      return child;
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
      const [k] = body.split("=");
      const dk = k.replace(/^data-/, "");
      const camel = dk.replace(/-([a-z])/g, (_, c) => c.toUpperCase());
      const val = node.dataset[dk] ?? node.dataset[camel];
      if (!body.includes("=")) return val != null;
      const want = body.split("=")[1].replace(/^"|"$/g, "");
      return String(val) === want;
    }
  }
  return node.tagName === sel.toUpperCase();
}

let documentRef = { activeElement: null, contains: () => true, _listeners: {} };

function makeRoots(){
  const searchoverlay = el("div", { id: "searchoverlay", hidden: true });
  const searchscrim = el("div", { id: "searchscrim" });
  const searchinput = el("input", { id: "searchinput" });
  const searchclose = el("button", { id: "searchclose" });
  const searchfeed = el("div", { id: "searchfeed" });
  const searchbtn = el("button", { id: "searchbtn" });
  searchoverlay.appendChild(searchscrim);
  searchoverlay.appendChild(searchinput);
  searchoverlay.appendChild(searchclose);
  searchoverlay.appendChild(searchfeed);

  documentRef = {
    activeElement: null,
    contains: n => !!n,
    createElement: tag => el(tag),
    getElementById: () => null,
    querySelector: () => null,
    addEventListener(type, fn, opts){
      (documentRef._listeners[type] || (documentRef._listeners[type] = [])).push({ fn, opts });
    },
    removeEventListener(type, fn){
      if (!documentRef._listeners[type]) return;
      documentRef._listeners[type] = documentRef._listeners[type].filter(x => x.fn !== fn);
    },
    _listeners: {},
  };

  return {
    roots: { searchoverlay, searchscrim, searchinput, searchclose, searchfeed, searchbtn },
    document: documentRef,
  };
}

class FakeAbortError extends Error {
  constructor(){ super("aborted"); this.name = "AbortError"; }
}

function makeAbortController(){
  const c = {
    signal: { aborted: false },
    abort(){ c.signal.aborted = true; if (c._onAbort) c._onAbort(); },
  };
  return c;
}

function createFeature(overrides = {}){
  const { roots, document } = makeRoots();
  const storage = overrides.storage || makeStorage(overrides.storageInit || {});
  const timers = [];
  let clock = 0;
  const setTimeoutFn = (fn, ms) => {
    const id = { fn, at: clock + (ms || 0), cleared: false };
    timers.push(id);
    return id;
  };
  const clearTimeoutFn = id => { if (id) id.cleared = true; };
  const flush = ms => {
    clock += ms || 0;
    const due = timers.filter(t => !t.cleared && t.at <= clock).sort((a, b) => a.at - b.at);
    for (const t of due){
      t.cleared = true;
      t.fn();
    }
  };
  const rAF = fn => setTimeoutFn(fn, 0);

  const effects = {
    pendingJumps: [],
    selects: [],
    toasts: [],
    bookmarksOpen: [],
    preview: [],
    chatInvalidations: 0,
  };

  let fetchImpl = overrides.fetch || (async () => ({ ok: true, json: async () => ({ groups: [] }) }));
  const controllers = [];
  const AbortControllerFake = function(){
    const c = makeAbortController();
    controllers.push(c);
    return c;
  };

  const feature = createSearchFeature({
    roots,
    document,
    storage,
    setTimeout: setTimeoutFn,
    clearTimeout: clearTimeoutFn,
    requestAnimationFrame: rAF,
    fetch: (...a) => fetchImpl(...a),
    AbortController: AbortControllerFake,
    createElement: tag => el(tag),
    activeElement: () => document.activeElement,
    contains: n => document.contains(n),
    esc,
    now: () => overrides.now ?? 1000,
    nodeById: overrides.nodeById || (() => null),
    laneColor: overrides.laneColor || (() => "#00f"),
    agentLogo: overrides.agentLogo || (() => "LOGO"),
    select: id => effects.selects.push(id),
    setPendingJump: v => effects.pendingJumps.push(v),
    invalidateChat: () => { effects.chatInvalidations++; },
    setBookmarksOpen: o => effects.bookmarksOpen.push(o),
    openPreview: (...a) => effects.preview.push(a),
    toast: m => effects.toasts.push(m),
    isDesktop: overrides.isDesktop || (() => false),
    ...overrides.extraDeps,
  });

  return {
    feature, roots, document, storage, effects, timers, flush, controllers,
    setFetch(fn){ fetchImpl = fn; },
  };
}

function makeHit(dataset, text = "excerpt"){
  const hit = el("div", { className: "searchhit", dataset });
  const ex = el("span", { className: "ex", textContent: text });
  hit.appendChild(ex);
  return hit;
}

async function settle(n = 8){
  for (let i = 0; i < n; i++) await Promise.resolve();
}

async function inputSearch(h, q){
  h.roots.searchinput.value = q;
  h.roots.searchinput.dispatch("input", { target: h.roots.searchinput });
  h.flush(SEARCH_DEBOUNCE_MS);
  await settle();
}

/* ---------- open / close / focus ---------- */
test("open is idempotent focus; focus return on close; blank when short query", () => {
  const h = createFeature();
  h.feature.bind();
  const prev = el("button", { id: "prev" });
  h.document.activeElement = prev;
  h.feature.open();
  h.flush(0);
  assert.equal(h.roots.searchoverlay.hidden, false);
  assert.equal(h.document.activeElement, h.roots.searchinput);
  assert.match(h.roots.searchfeed.innerHTML, /Search your current and past chats/);
  /* second open just focuses */
  h.roots.searchinput._focused = false;
  h.feature.open();
  assert.equal(h.document.activeElement, h.roots.searchinput);
  h.feature.close();
  assert.equal(h.roots.searchoverlay.hidden, true);
  assert.equal(h.document.activeElement, prev);
});

test("results survive close when query is ready; recents refresh on short open", () => {
  const h = createFeature();
  h.feature.bind();
  h.roots.searchinput.value = "auth";
  h.roots.searchfeed.innerHTML = `<div class="searchgroup">kept</div>`;
  h.feature.open();
  assert.match(h.roots.searchfeed.innerHTML, /kept/);
  h.feature.close();
  h.roots.searchinput.value = "";
  recordRecent("prior", h.storage);
  h.feature.open();
  assert.match(h.roots.searchfeed.innerHTML, /data-recent="prior"/);
});

test("focus trap wraps Tab between input and close; Escape closes", () => {
  const h = createFeature();
  h.feature.bind();
  h.feature.open();
  h.flush(0);
  h.document.activeElement = h.roots.searchclose;
  h.roots.searchoverlay.dispatch("keydown", { key: "Tab", shiftKey: false, target: h.roots.searchclose });
  assert.equal(h.document.activeElement, h.roots.searchinput);
  h.document.activeElement = h.roots.searchinput;
  h.roots.searchoverlay.dispatch("keydown", { key: "Tab", shiftKey: true, target: h.roots.searchinput });
  assert.equal(h.document.activeElement, h.roots.searchclose);
  h.roots.searchoverlay.dispatch("keydown", { key: "Escape" });
  assert.equal(h.roots.searchoverlay.hidden, true);
});

test("Cmd/Ctrl-K opens; slash opens only outside fields; slash fenced when open", () => {
  const h = createFeature();
  h.feature.bind();
  const body = el("div");
  body.tagName = "DIV";
  h.document.activeElement = body;
  const docKey = h.document._listeners.keydown[0].fn;
  let prevented = false;
  docKey({
    key: "k", metaKey: true, ctrlKey: false, altKey: false,
    target: body, preventDefault(){ prevented = true; },
  });
  assert.equal(prevented, true);
  assert.equal(h.feature.isOpen(), true);
  h.feature.close();

  prevented = false;
  docKey({
    key: "/", metaKey: false, ctrlKey: false, altKey: false,
    target: body, preventDefault(){ prevented = true; },
  });
  assert.equal(prevented, true);
  assert.equal(h.feature.isOpen(), true);

  /* slash while open does not re-open path as prevent (already open fence) */
  prevented = false;
  docKey({
    key: "/", metaKey: false, ctrlKey: false, altKey: false,
    target: body, preventDefault(){ prevented = true; },
  });
  assert.equal(prevented, false);

  h.feature.close();
  const input = el("input");
  prevented = false;
  docKey({
    key: "/", metaKey: false, ctrlKey: false, altKey: false,
    target: input, preventDefault(){ prevented = true; },
  });
  assert.equal(prevented, false);
  assert.equal(h.feature.isOpen(), false);

  const editable = el("div");
  editable.isContentEditable = true;
  docKey({
    key: "/", metaKey: false, ctrlKey: false, altKey: false,
    target: editable, preventDefault(){ prevented = true; },
  });
  assert.equal(prevented, false);
});

/* ---------- debounce / abort / stale ---------- */
test("exact 180ms debounce; intermediate keystrokes reschedule", async () => {
  let calls = 0;
  const h = createFeature({
    fetch: async () => { calls++; return { ok: true, json: async () => ({ groups: [] }) }; },
  });
  h.feature.bind();
  h.roots.searchinput.dispatch("input", { target: h.roots.searchinput });
  h.roots.searchinput.value = "a";
  h.roots.searchinput.dispatch("input", { target: h.roots.searchinput });
  h.roots.searchinput.value = "ab";
  h.roots.searchinput.dispatch("input", { target: h.roots.searchinput });
  h.flush(179);
  assert.equal(calls, 0);
  h.flush(1);
  await Promise.resolve();
  await Promise.resolve();
  assert.equal(calls, 1);
});

test("short query aborts in-flight and returns to blank", async () => {
  let aborted = false;
  const h = createFeature();
  h.setFetch(async (_url, opts) => {
    await new Promise((resolve, reject) => {
      const t = setTimeout(resolve, 50);
      if (opts && opts.signal){
        const onAbort = () => { aborted = true; clearTimeout(t); reject(new FakeAbortError()); };
        opts.signal.addEventListener
          ? opts.signal.addEventListener("abort", onAbort)
          : (h.controllers[h.controllers.length - 1]._onAbort = onAbort);
      }
    });
    return { ok: true, json: async () => ({ groups: [{ kind: "live", id: "n", hits: [] }] }) };
  });
  h.feature.bind();
  await inputSearch(h, "hello");
  assert.ok(h.controllers.length >= 1);
  /* force abort path: short query */
  await inputSearch(h, "x");
  assert.match(h.roots.searchfeed.innerHTML, /Search your current|Recent searches|empty/);
  assert.ok(aborted || h.controllers.some(c => c.signal.aborted));
});

test("superseding request aborts older; stale response never renders", async () => {
  const resolvers = [];
  const h = createFeature();
  h.setFetch(() => new Promise((resolve, reject) => {
    resolvers.push({ resolve, reject });
  }));
  h.feature.bind();
  h.roots.searchinput.value = "alpha";
  h.roots.searchinput.dispatch("input", { target: h.roots.searchinput });
  h.flush(SEARCH_DEBOUNCE_MS);
  h.roots.searchinput.value = "beta";
  h.roots.searchinput.dispatch("input", { target: h.roots.searchinput });
  h.flush(SEARCH_DEBOUNCE_MS);
  assert.equal(h.controllers.length, 2);
  assert.equal(h.controllers[0].signal.aborted, true);
  /* resolve stale first with distinctive payload */
  resolvers[0].resolve({
    ok: true,
    json: async () => ({ groups: [{ kind: "live", id: "stale", title: "STALE", hits: [{ match: "s" }] }] }),
  });
  await settle();
  assert.ok(!h.roots.searchfeed.innerHTML.includes("STALE"));
  resolvers[1].resolve({
    ok: true,
    json: async () => ({ groups: [{ kind: "live", id: "new", title: "NEW", hits: [{ match: "n" }] }] }),
  });
  await settle();
  assert.match(h.roots.searchfeed.innerHTML, /NEW/);
  assert.ok(!h.roots.searchfeed.innerHTML.includes("STALE"));
});

test("AbortError is silent; current failure shows unavailable", async () => {
  const h = createFeature();
  h.setFetch(async () => { throw new FakeAbortError(); });
  h.feature.bind();
  h.roots.searchfeed.innerHTML = "prior";
  await inputSearch(h, "ab");
  assert.equal(h.roots.searchfeed.innerHTML, "prior"); /* aborted — no overwrite */

  h.setFetch(async () => ({ ok: false, status: 500 }));
  await inputSearch(h, "cd");
  assert.match(h.roots.searchfeed.innerHTML, /Search is unavailable right now/);
});

test("partial and no-match rendering", async () => {
  const h = createFeature();
  h.setFetch(async () => ({ ok: true, json: async () => ({ groups: [], partial: false }) }));
  h.feature.bind();
  await inputSearch(h, "zz");
  assert.equal(h.roots.searchfeed.innerHTML, `<div class="empty">No matches.</div>`);
  h.setFetch(async () => ({
    ok: true,
    json: async () => ({
      groups: [{ kind: "live", id: "n1", title: "T", hits: [{ match: "m" }] }],
      partial: true,
    }),
  }));
  await inputSearch(h, "mm");
  assert.match(h.roots.searchfeed.innerHTML, /searchmore/);
});

/* ---------- recents click / clear via feed ---------- */
test("recents click re-runs; clear wipes and blanks", async () => {
  let lastQ = "";
  const h = createFeature();
  recordRecent("auth", h.storage);
  h.setFetch(async url => {
    lastQ = String(url);
    return { ok: true, json: async () => ({ groups: [] }) };
  });
  h.feature.bind();
  h.feature.open();
  h.flush(0);
  const recentBtn = el("button", { dataset: { recent: "auth" } });
  recentBtn.dataset.recent = "auth";
  h.roots.searchfeed.appendChild(recentBtn);
  /* dispatch with closest path: put dataset on target */
  h.roots.searchfeed.dispatch("click", { target: recentBtn });
  await Promise.resolve();
  await Promise.resolve();
  assert.match(lastQ, /q=auth/);
  assert.equal(h.roots.searchinput.value, "auth");

  const clearBtn = el("button", { id: "recentsclear" });
  h.roots.searchfeed.appendChild(clearBtn);
  /* make closest find #recentsclear */
  clearBtn.id = "recentsclear";
  h.roots.searchfeed.dispatch("click", { target: clearBtn });
  assert.deepEqual(loadRecents(h.storage), []);
  assert.match(h.roots.searchfeed.innerHTML, /Search your current and past chats/);
});

/* ---------- one gesture ---------- */

/* A tap opens the surrounding conversation. That is the entire interaction, and
   it is the same interaction whether the chat is still running or was deleted
   last month — because the user cannot tell those apart from a result row, and
   should not have to before deciding whether to tap. */
test("tapping a hit opens the preview — live and past alike, same call", () => {
  const h = createFeature({ nodeById: id => id === "n1" ? { id: "n1" } : null });
  h.feature.bind();
  h.feature.open();
  h.roots.searchinput.value = "ab";

  const live = makeHit({ kind: "live", id: "n1", uid: "u1", segment: "1", record: "2", turn: "T1" });
  h.roots.searchfeed.appendChild(live);
  h.roots.searchfeed.dispatch("click", { target: live });
  assert.deepEqual(h.effects.preview[0], ["u1", "1", "2", "T1"]);
  /* The overlay steps aside; the preview is drawn in its place. */
  assert.equal(h.feature.isOpen(), false);
  /* No jump happened: reading a hit is not the same as leaving for the chat. */
  assert.equal(h.effects.selects.length, 0);
  assert.deepEqual(loadRecents(h.storage), ["ab"]);

  h.feature.open();
  const past = makeHit({ kind: "archived", id: "", uid: "u9", segment: "3", record: "4", turn: "TA" });
  h.roots.searchfeed.appendChild(past);
  h.roots.searchfeed.dispatch("click", { target: past });
  assert.deepEqual(h.effects.preview[1], ["u9", "3", "4", "TA"]);
  assert.equal(h.effects.preview.length, 2, "one code path, not two");
});

/* The one hit that cannot be previewed: a bookmark filed as free-standing
   commentary, which points at no turn in any log. Its home is the pane. */
test("an unaddressed bookmark hit still opens the Bookmarks pane", () => {
  const h = createFeature();
  h.feature.bind();
  h.feature.open();
  const bm = makeHit({ kind: "bookmarks", id: "", uid: "" });
  h.roots.searchfeed.appendChild(bm);
  h.roots.searchfeed.dispatch("click", { target: bm });
  assert.equal(h.effects.bookmarksOpen.at(-1), true);
  assert.equal(h.effects.preview.length, 0);
});

/* A live chat whose log predates the meta header has no uid to preview by. It
   is not unavailable — the chat is right there — so the tap goes straight in,
   which is exactly what it used to do for every live hit. */
test("a live hit with no log address jumps into the chat instead", () => {
  const h = createFeature({ nodeById: id => id === "n1" ? { id: "n1" } : null });
  h.feature.bind();
  h.feature.open();
  const hit = makeHit({ kind: "live", id: "n1", uid: "", turn: "T1" });
  h.roots.searchfeed.appendChild(hit);
  h.roots.searchfeed.dispatch("click", { target: hit });
  assert.equal(h.effects.preview.length, 0);
  assert.equal(h.effects.selects[0], "n1");
  assert.equal(h.effects.chatInvalidations, 1);
  assert.equal(h.feature.isOpen(), false);
});

test("a hit with neither an address nor a live chat says so", () => {
  const h = createFeature({ nodeById: () => null });
  h.feature.bind();
  h.feature.open();
  const gone = makeHit({ kind: "live", id: "missing", uid: "" });
  h.roots.searchfeed.appendChild(gone);
  h.roots.searchfeed.dispatch("click", { target: gone });
  assert.equal(h.effects.toasts.at(-1), "This chat is unavailable.");
  assert.equal(h.effects.preview.length, 0);
});

test("search.js has no prompt dependency left at all", () => {
  assert.equal(/\bprompt\b/.test(searchSrc), false,
    "item 15: the search overlay must not open a modal text prompt");
});

/* ---------- bind / destroy ---------- */
test("bind is idempotent; destroy cleans listeners, timer, abort", async () => {
  const h = createFeature();
  h.feature.bind();
  h.feature.bind();
  assert.equal(h.roots.searchbtn._listeners.click.length, 1);
  assert.equal(h.document._listeners.keydown.length, 1);
  h.roots.searchinput.value = "ab";
  h.roots.searchinput.dispatch("input", { target: h.roots.searchinput });
  assert.ok(h.timers.some(t => !t.cleared));

  let fetchStarted = false;
  let resolveIgnoredAbort;
  h.roots.searchfeed.innerHTML = "prior";
  h.setFetch(() => new Promise(resolve => {
    fetchStarted = true;
    resolveIgnoredAbort = resolve;
  }));
  h.roots.searchinput.value = "long";
  h.roots.searchinput.dispatch("input", { target: h.roots.searchinput });
  h.flush(SEARCH_DEBOUNCE_MS);
  assert.equal(fetchStarted, true);
  assert.equal(h.controllers.length, 1);

  h.feature.destroy();
  assert.equal(h.roots.searchbtn._listeners.click.length, 0);
  assert.equal(h.document._listeners.keydown.length, 0);
  assert.ok(h.timers.every(t => t.cleared));
  assert.equal(h.controllers[0].signal.aborted, true);
  resolveIgnoredAbort({
    ok: true,
    json: async () => ({ groups: [{ kind: "live", id: "late", title: "LATE", hits: [{ match: "x" }] }] }),
  });
  await settle();
  assert.equal(h.roots.searchfeed.innerHTML, "prior", "destroy invalidates a response even when fetch ignores abort");

  /* re-bind works */
  h.feature.bind();
  assert.equal(h.roots.searchbtn._listeners.click.length, 1);
});

test("searchbtn/scrim/close click open and close", () => {
  const h = createFeature();
  h.feature.bind();
  h.roots.searchbtn.dispatch("click");
  h.flush(0);
  assert.equal(h.feature.isOpen(), true);
  h.roots.searchscrim.dispatch("click");
  assert.equal(h.feature.isOpen(), false);
  h.roots.searchbtn.dispatch("click");
  h.flush(0);
  h.roots.searchclose.dispatch("click");
  assert.equal(h.feature.isOpen(), false);
});
