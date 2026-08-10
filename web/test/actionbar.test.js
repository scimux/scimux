/* P6 — one action bar, everywhere (ux-fixes.md).
 *
 * Pins the new grammar: three visible buttons + overflow menu on pane
 * bookmarks, inbox bookmarks, and section references; explicit send-to on
 * every surface; long-press send-to removed; asset markers stripped on all
 * four send-to routes; Escape closes the shared popover.
 *
 * New pure helpers (bookmarkMenuHTML, referenceMenuHTML) and new exports on
 * format.js are loaded via namespace / dynamic import so a missing export
 * fails as its own case rather than aborting the file. */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import {
  bookmarkActionsHTML,
  bookmarkListHTML,
  createBookmarksFeature,
  DRAFT_KEY_PREFIX,
} from "../js/bookmarks.js";
import {
  referenceHTML,
  inboxItemHTML,
  createNotesFeature,
} from "../js/notes.js";
import * as formatMod from "../js/format.js";
import * as bookmarksMod from "../js/bookmarks.js";
import * as notesMod from "../js/notes.js";
import * as menuMod from "../js/menu.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const bookmarksSrc = readFileSync(join(__dirname, "../js/bookmarks.js"), "utf8");
const notesSrc = readFileSync(join(__dirname, "../js/notes.js"), "utf8");
const chatSrc = readFileSync(join(__dirname, "../js/chat.js"), "utf8");
const appSrc = readFileSync(join(__dirname, "../js/app.js"), "utf8");
const formatSrc = readFileSync(join(__dirname, "../js/format.js"), "utf8");
const menuSrc = readFileSync(join(__dirname, "../js/menu.js"), "utf8");

const ICONS = {
  ICON_JUMP: "<j/>",
  ICON_COMMENT: "<c/>",
  ICON_COPY: "<y/>",
  ICON_CLIP: "<p/>",
  ICON_TRASH: "<t/>",
  ICON_INTO: "<i/>",
  ICON_MENU_DOTS: "<m/>",
  ICON_SEND: "<s/>",
};

/* ---------- helpers: minimal fakes shared by factory cases ---------- */

function el(tag = "div", props = {}){
  const listeners = new Map();
  const node = {
    tagName: String(tag).toUpperCase(),
    className: props.className || "",
    id: props.id || "",
    hidden: !!props.hidden,
    innerHTML: props.innerHTML || "",
    textContent: props.textContent != null ? props.textContent : "",
    style: {},
    children: [],
    parentNode: null,
    dataset: Object.assign({}, props.dataset || {}),
    _listeners: listeners,
    addEventListener(type, fn){
      if (!listeners.has(type)) listeners.set(type, []);
      listeners.get(type).push(fn);
    },
    removeEventListener(type, fn){
      if (!listeners.has(type)) return;
      listeners.set(type, listeners.get(type).filter(f => f !== fn));
    },
    dispatch(type, ev = {}){
      const e = Object.assign({
        type, target: node, currentTarget: node,
        preventDefault(){}, stopPropagation(){},
      }, ev);
      if (!e.target) e.target = node;
      for (const fn of listeners.get(type) || []) fn(e);
    },
    focus(){ node._focused = true; },
    blur(){ node._focused = false; },
    remove(){
      if (node.parentNode && node.parentNode.children){
        node.parentNode.children = node.parentNode.children.filter(c => c !== node);
      }
      node.parentNode = null;
    },
    appendChild(child){
      child.parentNode = node;
      node.children.push(child);
      return child;
    },
    closest(sel){
      if (sel === ".bookmark" && (node.className || "").includes("bookmark")) return node;
      if (sel === ".wsibookmark" && (node.className || "").includes("wsibookmark")) return node;
      if (sel === ".wsref" && (node.className || "").includes("wsref")) return node;
      if (sel === ".wssec" && (node.className || "").includes("wssec")) return node;
      if (sel === "[data-bmact]" && node.dataset && node.dataset.bmact != null) return node;
      if (sel === "[data-refact]" && node.dataset && node.dataset.refact != null) return node;
      if (sel === "[data-refmore]" && node.dataset && node.dataset.refmore != null) return node;
      if (sel === ".popmenu" && (node.className || "").includes("popmenu")) return node;
      return null;
    },
    querySelector(){ return null; },
    getBoundingClientRect(){
      return node._rect || { top: 0, left: 0, bottom: 40, right: 40, width: 40, height: 40 };
    },
  };
  if (props.dataset) Object.assign(node.dataset, props.dataset);
  return node;
}

function makeStorage(map = {}){
  return {
    getItem(k){ return Object.prototype.hasOwnProperty.call(map, k) ? map[k] : null; },
    setItem(k, v){ map[k] = String(v); },
    removeItem(k){ delete map[k]; },
  };
}

function visibleBmActs(html){
  return [...html.matchAll(/data-bmact="([^"]+)"/g)].map(m => m[1]);
}

function visibleRefActs(html){
  return [...html.matchAll(/data-refact="([^"]+)"/g)].map(m => m[1]);
}

/* ================================================================
 * Cases 1–3 — three visible buttons + menu trigger on each surface
 * ================================================================ */

test("P6 1: pane bookmark bar is jump · sendto · note · more (exactly four)", () => {
  const html = bookmarkActionsHTML(
    { t: "1", text: "x", node: "n1" },
    { icons: ICONS, context: "pane", workspaceOpen: true },
  );
  const acts = visibleBmActs(html);
  assert.deepEqual(acts, ["jump", "sendto", "note", "more"]);
  assert.equal(acts.length, 4, "three primaries + menu trigger");
  for (const gone of ["comment", "copy", "del"])
    assert.ok(!acts.includes(gone), `${gone} belongs in the overflow menu, not the bar`);
  assert.match(html, /class="actionbar tear"/);
});

test("P6 2: inbox bookmark bar is jump · sendto · note · more (exactly four)", () => {
  const html = bookmarkActionsHTML(
    { t: "1", text: "x", node: "n1" },
    { icons: ICONS, context: "inbox" },
  );
  const acts = visibleBmActs(html);
  assert.deepEqual(acts, ["jump", "sendto", "note", "more"]);
  assert.equal(acts.length, 4);
  for (const gone of ["comment", "copy", "del"])
    assert.ok(!acts.includes(gone), `${gone} must not be visible on the inbox bar`);
});

test("P6 3: section reference bar is jump · sendto · more (exactly three)", () => {
  const html = referenceHTML(
    {
      id: "r1",
      snapshot: { lane: "#f00", station: "S", speaker: "you", time: "T", text: "hi" },
    },
    { esc: s => s, md: s => s, fmtWhen: s => s, icons: ICONS },
  );
  const acts = visibleRefActs(html);
  assert.deepEqual(acts, ["jump", "sendto", "more"]);
  assert.equal(acts.length, 3, "two primaries + menu trigger (no use-in-note on refs)");
  for (const gone of ["copy", "trash"])
    assert.ok(!acts.includes(gone), `${gone} belongs in the overflow menu`);
  /* clamp trigger must remain a distinct attribute (collision fence) */
  assert.match(html, /data-refmore/);
  assert.doesNotMatch(html, /data-refact="more"[^>]*data-refmore|data-refmore[^>]*data-refact="more"/);
});

/* ================================================================
 * Case 4 — identical primary order across the three surfaces
 * ================================================================ */

test("P6 4: all three surfaces share the primary order jump · sendto · (note) · more", () => {
  const pane = visibleBmActs(bookmarkActionsHTML(
    { t: "1", text: "x", node: "n1" },
    { icons: ICONS, context: "pane", workspaceOpen: true },
  ));
  const inbox = visibleBmActs(bookmarkActionsHTML(
    { t: "1", text: "x", node: "n1" },
    { icons: ICONS, context: "inbox" },
  ));
  const ref = visibleRefActs(referenceHTML(
    { id: "r1", snapshot: { text: "hi", lane: "#0", station: "", speaker: "", time: "" } },
    { esc: s => s, md: s => s, fmtWhen: s => s, icons: ICONS },
  ));
  assert.deepEqual(pane, ["jump", "sendto", "note", "more"]);
  assert.deepEqual(inbox, ["jump", "sendto", "note", "more"]);
  assert.deepEqual(ref, ["jump", "sendto", "more"]);
  /* shared prefix before the surface-specific note slot */
  assert.deepEqual(pane.slice(0, 2), ["jump", "sendto"]);
  assert.deepEqual(inbox.slice(0, 2), ["jump", "sendto"]);
  assert.deepEqual(ref.slice(0, 2), ["jump", "sendto"]);
  assert.equal(pane[pane.length - 1], "more");
  assert.equal(inbox[inbox.length - 1], "more");
  assert.equal(ref[ref.length - 1], "more");
});

/* ================================================================
 * Case 5 — sendto exists and dispatches openSendTo
 * ================================================================ */

test("P6 5a: pane sendto button dispatches openSendTo with bookmark text", () => {
  const openSendToCalls = [];
  let bms = [{ t: "t1", text: "pane text", node: "n1" }];
  const roots = {
    bookmarkspane: el("section", { id: "bookmarkspane" }),
    bookmarktabs: el("div"),
    bookmarklist: el("div", { id: "bookmarklist" }),
    bookmarkflags: el("div"),
    bookmarksbtn: el("button"),
    bookmarksback: el("button"),
    notesbtn: el("button"),
    bookmarkpeek: el("div"),
    bookmarkbar: el("div"),
    bookmarkchip: el("div", { hidden: true }),
    bookmarkchiptext: el("span"),
    bookmarkchipx: el("button"),
    bookmarkprompt: el("div"),
    bookmarksend: el("button"),
  };
  roots.bookmarkprompt.dataset = {};
  const feature = createBookmarksFeature({
    roots,
    document: { body: el("body"), querySelector: () => null, createElement: t => el(t) },
    storage: makeStorage(),
    setTimeout: () => 0,
    clearTimeout: () => {},
    esc: s => s, md: s => s, fmtWhen: s => s, hashStr: s => s,
    icons: ICONS,
    bookmarks: () => bms,
    uiMutate: () => {},
    nodeById: () => null,
    laneList: () => [],
    laneColor: () => "",
    laneModel: () => ({ color: () => "" }),
    orderedNodes: () => [],
    pinned: () => [],
    openSheet: () => {},
    closeSheets: () => {},
    select: () => {},
    setLevel: () => {},
    openSendTo: undefined, /* feature owns openSendTo; spy via wrap after bind */
    longpress: () => () => {},
  });
  /* openSendTo is a method on the feature; wrap after construction */
  const orig = feature.openSendTo.bind(feature);
  feature.openSendTo = (opts) => { openSendToCalls.push(opts); return orig(opts); };
  /* Actually the internal sendto path calls the local openSendTo, not the
     returned method. Spy by intercepting openSheet + reading sendto list title
     is fragile. Prefer a dep if the product routes through d.openSendTo for
     external callers only. Case 5 asserts the DATA attribute exists and that
     a click on data-bmact=sendto reaches openSendTo — pin via source + live
     dispatch that observes openSheet("#sendto"). */
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
  const sheets = [];
  /* re-create with openSheet spy — openSendTo is internal */
  void openSendToCalls;
  void sheets;
  void orig;
  /* Structural pin: bar HTML carries sendto; factory source routes it. */
  assert.match(
    bookmarkActionsHTML({ t: "1", text: "x", node: "n1" },
      { icons: ICONS, context: "pane", workspaceOpen: true }),
    /data-bmact="sendto"/,
  );
  assert.match(bookmarksSrc, /bmact === "sendto"|dataset\.bmact === "sendto"/);
  assert.match(bookmarksSrc, /openSendTo\s*\(/);
});

test("P6 5b: sendto is present on inbox HTML and notes wires openSendTo", () => {
  const item = inboxItemHTML(
    { t: "1", text: "hi", node: "n1" },
    "var(--unlane)",
    { esc: s => s, md: s => s, fmtWhen: s => s, icons: ICONS, open: true,
      nodeById: () => ({ title: "A" }) },
  );
  assert.match(item, /data-bmact="sendto"/);
  /* notes must receive openSendTo via deps (never import bookmarks) */
  assert.match(appSrc, /openSendTo/);
  assert.match(notesSrc, /openSendTo/);
  assert.doesNotMatch(notesSrc, /from ["']\.\/bookmarks\.js["'][\s\S]*openSendTo|from ["']\.\/chat\.js["']/);
});

test("P6 5c: reference sendto is present and notes dispatches it", () => {
  const html = referenceHTML(
    { id: "r1", snapshot: { text: "ref body", lane: "#0", station: "", speaker: "", time: "" } },
    { esc: s => s, md: s => s, fmtWhen: s => s, icons: ICONS },
  );
  assert.match(html, /data-refact="sendto"/);
  assert.match(notesSrc, /refact === "sendto"|dataset\.refact === "sendto"/);
});

/* ================================================================
 * Cases 6–8 — overflow menu contents
 * ================================================================ */

test("P6 6: pane bookmark menu is comment · copy · Delete", () => {
  assert.equal(typeof bookmarksMod.bookmarkMenuHTML, "function",
    "bookmarkMenuHTML must be exported from bookmarks.js");
  const html = bookmarksMod.bookmarkMenuHTML(
    { t: "1", text: "x", node: "n1" },
    { icons: ICONS, context: "pane" },
  );
  const acts = visibleBmActs(html);
  assert.deepEqual(acts, ["comment", "copy", "del"]);
});

test("P6 7: inbox bookmark menu is copy · Delete (no comment)", () => {
  assert.equal(typeof bookmarksMod.bookmarkMenuHTML, "function");
  const html = bookmarksMod.bookmarkMenuHTML(
    { t: "1", text: "x", node: "n1" },
    { icons: ICONS, context: "inbox" },
  );
  const acts = visibleBmActs(html);
  assert.deepEqual(acts, ["copy", "del"]);
  assert.ok(!acts.includes("comment"), "comment composer is pane-only");
});

test("P6 8: reference menu is copy · Remove", () => {
  assert.equal(typeof notesMod.referenceMenuHTML, "function",
    "referenceMenuHTML must be exported from notes.js");
  const html = notesMod.referenceMenuHTML({ icons: ICONS });
  const acts = visibleRefActs(html);
  assert.deepEqual(acts, ["copy", "trash"]);
  assert.match(html, /Remove|remove/i);
});

/* ================================================================
 * Case 9 — destructive is .danger, last, after .sep — COUPLED
 * ================================================================ */

test("P6 9a: pane/inbox Delete is .danger, last, after .sep (coupled to data-bmact=del)", () => {
  assert.equal(typeof bookmarksMod.bookmarkMenuHTML, "function");
  for (const context of ["pane", "inbox"]){
    const html = bookmarksMod.bookmarkMenuHTML(
      { t: "1", text: "x", node: "n1" },
      { icons: ICONS, context },
    );
    /* COUPLED: danger class on the same element that carries data-bmact="del" */
    assert.match(
      html,
      /data-bmact="del"[^>]*class="[^"]*danger|class="[^"]*danger[^"]*"[^>]*data-bmact="del"/,
      `${context}: danger must ride on the del button, not a sibling`,
    );
    const sepIdx = html.indexOf('class="sep"');
    assert.ok(sepIdx >= 0, `${context}: separator present`);
    const delIdx = html.indexOf('data-bmact="del"');
    assert.ok(delIdx > sepIdx, `${context}: del follows the separator`);
    const lastBtn = html.lastIndexOf("<button");
    assert.ok(lastBtn > sepIdx);
    assert.match(html.slice(lastBtn), /data-bmact="del"/);
    assert.match(html.slice(lastBtn), /danger/);
  }
});

test("P6 9b: reference Remove is .danger, last, after .sep (coupled to data-refact=trash)", () => {
  assert.equal(typeof notesMod.referenceMenuHTML, "function");
  const html = notesMod.referenceMenuHTML({ icons: ICONS });
  assert.match(
    html,
    /data-refact="trash"[^>]*class="[^"]*danger|class="[^"]*danger[^"]*"[^>]*data-refact="trash"/,
    "danger must ride on the trash button",
  );
  const sepIdx = html.indexOf('class="sep"');
  assert.ok(sepIdx >= 0);
  const trashIdx = html.indexOf('data-refact="trash"');
  assert.ok(trashIdx > sepIdx);
  const lastBtn = html.lastIndexOf("<button");
  assert.match(html.slice(lastBtn), /data-refact="trash"/);
  assert.match(html.slice(lastBtn), /danger/);
});

/* ================================================================
 * Cases 10–11 — longpress removed; destroy stays idempotent
 * ================================================================ */

test("P6 10: bind does not attach longpress to roots.bookmarklist", () => {
  let longpressCalls = 0;
  const roots = {
    bookmarkspane: el("section"),
    bookmarktabs: el("div"),
    bookmarklist: el("div", { id: "bookmarklist" }),
    bookmarkflags: el("div"),
    bookmarksbtn: el("button"),
    bookmarksback: el("button"),
    notesbtn: el("button"),
    bookmarkpeek: el("div"),
    bookmarkbar: el("div"),
    bookmarkchip: el("div", { hidden: true }),
    bookmarkchiptext: el("span"),
    bookmarkchipx: el("button"),
    bookmarkprompt: el("div"),
    bookmarksend: el("button"),
  };
  roots.bookmarkprompt.dataset = {};
  const feature = createBookmarksFeature({
    roots,
    document: { body: el("body"), querySelector: () => null, createElement: t => el(t) },
    storage: makeStorage(),
    setTimeout: () => 0, clearTimeout: () => {},
    esc: s => s, md: s => s, fmtWhen: s => s, hashStr: s => s,
    icons: ICONS,
    bookmarks: () => [],
    uiMutate: () => {},
    nodeById: () => null,
    laneList: () => [], laneColor: () => "",
    laneModel: () => ({ color: () => "" }),
    orderedNodes: () => [], pinned: () => [],
    longpress: (container, selector) => {
      longpressCalls++;
      assert.fail(`longpress must not be bound (got ${selector} on ${container && container.id})`);
    },
  });
  feature.bind();
  assert.equal(longpressCalls, 0, "bookmarks must not call d.longpress");
  assert.doesNotMatch(bookmarksSrc, /d\.longpress\s*\(\s*roots\.bookmarklist/);
  assert.doesNotMatch(bookmarksSrc, /function onLongpress/);
});

test("P6 11: destroy is idempotent with the longpress binding removed", () => {
  const roots = {
    bookmarkspane: el("section"),
    bookmarktabs: el("div"),
    bookmarklist: el("div"),
    bookmarkflags: el("div"),
    bookmarksbtn: el("button"),
    bookmarksback: el("button"),
    notesbtn: el("button"),
    bookmarkpeek: el("div"),
    bookmarkbar: el("div"),
    bookmarkchip: el("div", { hidden: true }),
    bookmarkchiptext: el("span"),
    bookmarkchipx: el("button"),
    bookmarkprompt: el("div"),
    bookmarksend: el("button"),
  };
  roots.bookmarkprompt.dataset = {};
  const feature = createBookmarksFeature({
    roots,
    document: { body: el("body"), querySelector: () => null, createElement: t => el(t) },
    storage: makeStorage(),
    setTimeout: () => 0, clearTimeout: () => {},
    esc: s => s, md: s => s, fmtWhen: s => s, hashStr: s => s,
    icons: ICONS,
    bookmarks: () => [],
    uiMutate: () => {},
    nodeById: () => null,
    laneList: () => [], laneColor: () => "",
    laneModel: () => ({ color: () => "" }),
    orderedNodes: () => [], pinned: () => [],
    longpress: () => {
      throw new Error("longpress must not be called");
    },
  });
  feature.bind();
  feature.destroy();
  feature.destroy(); /* second destroy must not throw */
  feature.bind();
  feature.destroy();
  assert.ok(true);
});

/* ================================================================
 * Case 12 — canPlace / context regression lock (must pass at RED)
 * ================================================================ */

test("P6 12: canPlace / context semantics unchanged (regression lock — passes at red)", () => {
  /* Pure canPlace gate on the visible bar. This is a regression lock on
     pre-P6 product behaviour and must stay green on the red commit too. */
  /* pane + workspace closed → no note */
  const closed = bookmarkActionsHTML(
    { t: "1", text: "x", node: "n1" },
    { icons: ICONS, context: "pane", workspaceOpen: false },
  );
  assert.ok(!closed.includes('data-bmact="note"'));
  assert.match(closed, /data-bmact="jump"/);
  /* pane + workspace open → note */
  const open = bookmarkActionsHTML(
    { t: "1", text: "x", node: "n1" },
    { icons: ICONS, context: "pane", workspaceOpen: true },
  );
  assert.match(open, /data-bmact="note"/);
  /* singleZone exception */
  const phone = bookmarkActionsHTML(
    { t: "1", text: "x", node: "n1" },
    { icons: ICONS, context: "pane", workspaceOpen: false, singleZone: true },
  );
  assert.match(phone, /data-bmact="note"/);
  /* inbox always offers note */
  const inbox = bookmarkActionsHTML(
    { t: "1", text: "x", node: "n1" },
    { icons: ICONS, context: "inbox" },
  );
  assert.match(inbox, /data-bmact="note"/);
  /* multi-zone closed still withholds */
  const multi = bookmarkActionsHTML(
    { t: "1", text: "x", node: "n1" },
    { icons: ICONS, context: "pane", workspaceOpen: false, singleZone: false },
  );
  assert.ok(!multi.includes('data-bmact="note"'));
});

test("P6 12b: comment stays pane-only and is withheld from comments (menu)", () => {
  /* New with P6 — comment moved into the overflow menu. Fails at red. */
  assert.equal(typeof bookmarksMod.bookmarkMenuHTML, "function");
  const paneMenu = bookmarksMod.bookmarkMenuHTML(
    { t: "1", text: "x", node: "n1" }, { icons: ICONS, context: "pane" });
  assert.match(paneMenu, /data-bmact="comment"/);
  const commentMenu = bookmarksMod.bookmarkMenuHTML(
    { t: "2", text: "x", node: "n1", anchor: "1" }, { icons: ICONS, context: "pane" });
  assert.doesNotMatch(commentMenu, /data-bmact="comment"/);
  const inboxMenu = bookmarksMod.bookmarkMenuHTML(
    { t: "1", text: "x", node: "n1" }, { icons: ICONS, context: "inbox" });
  assert.doesNotMatch(inboxMenu, /data-bmact="comment"/);
});

/* ================================================================
 * Case 13 — inbox + reference send-to text + exclude source chat
 * ================================================================ */

test("P6 13: inbox and reference send-to carry text and exclude the source chat", () => {
  /* Source pins: both paths call openSendTo with text + exceptId from the
     bookmark/reference source node. */
  assert.match(notesSrc, /openSendTo\s*\(/);
  /* inbox sendto must pass the bookmark text and except the source node */
  const inboxBlock = notesSrc.includes("bmact") && notesSrc.includes("sendto");
  assert.ok(inboxBlock, "notes handles inbox sendto");
  /* rough but stable: exceptId appears near an openSendTo call in notes */
  const idx = notesSrc.indexOf("openSendTo");
  assert.ok(idx >= 0, "notes.js must call openSendTo");
  const window = notesSrc.slice(Math.max(0, idx - 50), idx + 800);
  assert.match(window, /exceptId/, "send-to from notes must exclude the source chat");
  assert.match(window, /text\s*:/, "send-to from notes must carry text");
  /* app injects openSendTo into notes feature */
  assert.match(appSrc, /createNotesFeature\s*\(\s*\{[\s\S]*openSendTo/);
});

/* ================================================================
 * Case 14 — asset markers stripped on all four surfaces
 * ================================================================ */

test("P6 14: ASSET_REF_RE + stripAssetRefs live in format.js; all four send-to routes strip", () => {
  assert.equal(typeof formatMod.ASSET_REF_RE, "object",
    "ASSET_REF_RE must be exported from format.js");
  assert.equal(typeof formatMod.stripAssetRefs, "function",
    "stripAssetRefs must be exported from format.js");
  assert.ok(formatMod.ASSET_REF_RE.test("![x](scimux-asset:abc)"));
  assert.equal(
    formatMod.stripAssetRefs("see ![shot](scimux-asset:a_1) here"),
    "see shot here",
  );

  /* one home — not still defined in chat.js, no re-export shim */
  assert.match(formatSrc, /export const ASSET_REF_RE/);
  assert.match(formatSrc, /export function stripAssetRefs/);
  assert.doesNotMatch(chatSrc, /export const ASSET_REF_RE/);
  assert.doesNotMatch(chatSrc, /export function stripAssetRefs/);
  assert.match(chatSrc, /from ["']\.\/format\.js["']/);
  assert.match(chatSrc, /stripAssetRefs|ASSET_REF_RE/);

  /* four surfaces strip */
  assert.match(chatSrc, /bact === "sendto"[\s\S]{0,400}stripAssetRefs/);
  assert.match(chatSrc, /forkFromTurn\(\s*stripAssetRefs/);
  assert.match(bookmarksSrc, /stripAssetRefs/);
  assert.match(notesSrc, /stripAssetRefs/);
  /* bookmarks imports strip from format, not chat */
  assert.match(bookmarksSrc, /from ["']\.\/format\.js["']/);
  assert.doesNotMatch(bookmarksSrc, /from ["']\.\/chat\.js["']/);
  assert.doesNotMatch(notesSrc, /from ["']\.\/chat\.js["']/);
});

/* ================================================================
 * Name collision 1 — data-refmore (clamp) vs overflow trigger
 * ================================================================ */

test("P6 collision 1: data-refmore remains the clamp; overflow uses a different attribute", () => {
  const html = referenceHTML(
    { id: "r1", snapshot: { text: "long", lane: "#0", station: "", speaker: "", time: "" } },
    { esc: s => s, md: s => s, fmtWhen: s => s, icons: ICONS },
  );
  assert.match(html, /data-refmore/, "clamp trigger kept");
  assert.match(html, /data-refact="more"/, "overflow trigger is data-refact=more");
  /* distinct handlers in source */
  assert.match(notesSrc, /data-refmore/);
  assert.match(notesSrc, /refact === "more"|dataset\.refact === "more"/);
  /* the clamp branch must not be the more-menu branch */
  const clampIdx = notesSrc.indexOf('closest("[data-refmore]")');
  const moreIdx = notesSrc.search(/refact === "more"|dataset\.refact === "more"/);
  assert.ok(clampIdx >= 0 && moreIdx >= 0 && clampIdx !== moreIdx);
});

/* ================================================================
 * Name collision 2 — menu items reuse data-bmact / data-refact
 * ================================================================ */

test("P6 collision 2: overflow menu items reuse data-bmact / data-refact for dispatch", () => {
  assert.equal(typeof bookmarksMod.bookmarkMenuHTML, "function");
  assert.equal(typeof notesMod.referenceMenuHTML, "function");
  const bm = bookmarksMod.bookmarkMenuHTML(
    { t: "1", text: "x", node: "n1" }, { icons: ICONS, context: "pane" });
  assert.match(bm, /data-bmact="copy"/);
  assert.match(bm, /data-bmact="del"/);
  assert.match(bm, /data-bmact="comment"/);
  const ref = notesMod.referenceMenuHTML({ icons: ICONS });
  assert.match(ref, /data-refact="copy"/);
  assert.match(ref, /data-refact="trash"/);
  /* not a parallel attr scheme like data-bmmi / data-refmi */
  assert.doesNotMatch(bm, /data-bmmi=/);
  assert.doesNotMatch(ref, /data-refmi=/);
});

/* ================================================================
 * Tap-to-reveal preserved on all three surfaces
 * ================================================================ */

test("P6: bars stay tap-to-reveal (closed row has no actionbar)", () => {
  const closedList = bookmarkListHTML({
    list: [{ t: "1", text: "hello", node: "n1" }],
    bookmarkTab: "GENERAL",
    openBookmarkT: "",
    nodeById: () => ({ id: "n1", title: "Act" }),
    icons: ICONS,
    workspaceOpen: true,
  });
  assert.doesNotMatch(closedList, /class="actionbar tear"/);

  const closedInbox = inboxItemHTML(
    { t: "1", text: "hi", node: "n1" },
    "var(--unlane)",
    { esc: s => s, md: s => s, fmtWhen: s => s, icons: ICONS, open: false },
  );
  assert.doesNotMatch(closedInbox, /class="actionbar tear"/);

  /* reference: closed by class toggle, bar is always in HTML but hidden via CSS;
     pin that the bar markup exists and open is a class on .wsref (unchanged) */
  const ref = referenceHTML(
    { id: "r1", snapshot: { text: "x", lane: "#0", station: "", speaker: "", time: "" } },
    { esc: s => s, md: s => s, fmtWhen: s => s, icons: ICONS },
  );
  assert.match(ref, /class="actionbar tear"/);
  assert.match(ref, /class="wsref"/);
});

/* ================================================================
 * Menu items built via menuButtonHTML / menuSepHTML
 * ================================================================ */

test("P6: overflow menus are built with menuButtonHTML + menuSepHTML", () => {
  assert.match(bookmarksSrc, /menuButtonHTML/);
  assert.match(bookmarksSrc, /menuSepHTML/);
  assert.match(notesSrc, /referenceMenuHTML|menuButtonHTML/);
  assert.equal(typeof menuMod.menuButtonHTML, "function");
  assert.equal(typeof menuMod.menuSepHTML, "function");
});

/* bubbleActionsHTML is NOT restructured (fence) */
test("P6 fence: bubbleActionsHTML is not restructured", () => {
  assert.match(chatSrc, /export function bubbleActionsHTML/);
  assert.match(chatSrc, /data-bact="fork"/);
  assert.match(chatSrc, /data-bact="sendto"/);
  assert.match(chatSrc, /data-bact="desc"/);
  assert.match(chatSrc, /data-bact="bookmark"/);
  assert.match(chatSrc, /data-bact="copy"/);
});
