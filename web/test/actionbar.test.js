/* P3 — action bars and the unrotated icon (ux-fixes-2.md item 2).
 *
 * Arc 1 P6 pinned a shared grammar of jump · sendto · (note) · more on every
 * surface. P3 deliberately re-composes the three bars (maintainer table) and
 * unscopes .rot180/.rot90l so the send-to glyph rotates outside .bubactions.
 *
 * Fences that survive from P6: data-refmore stays the clamp; menu items reuse
 * data-bmact/data-refact; bars stay tap-to-reveal; overflow menus use
 * menuButtonHTML + menuSepHTML; only the destructive item carries .danger;
 * bubbleActionsHTML is not restructured; an overflow-menu click runs once.
 *
 * Pure helpers and exports are loaded via namespace / dynamic import so a
 * missing export fails as its own case rather than aborting the file. */
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
const chatCssSrc = readFileSync(join(__dirname, "../css/chat.css"), "utf8");

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
    parentElement: null,
    dataset: Object.assign({}, props.dataset || {}),
    _listeners: listeners,
    _ariaLabel: "",
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
    setAttribute(k, v){
      if (k === "aria-label") node._ariaLabel = String(v);
      else node[k] = v;
    },
    focus(){ node._focused = true; },
    blur(){ node._focused = false; },
    remove(){
      if (node.parentNode && node.parentNode.children){
        node.parentNode.children = node.parentNode.children.filter(c => c !== node);
      }
      node.parentNode = null;
      node.parentElement = null;
    },
    appendChild(child){
      child.parentNode = node;
      child.parentElement = node;
      node.children.push(child);
      return child;
    },
    closest(sel){
      if (sel === ".bookmark" && (node.className || "").includes("bookmark")) return node;
      if (sel === ".wsibookmark" && (node.className || "").includes("wsibookmark")) return node;
      if (sel === ".wsref" && (node.className || "").includes("wsref")) return node;
      if (sel === ".wssec" && (node.className || "").includes("wssec")) return node;
      if (sel === ".wscard" && (node.className || "").includes("wscard")) return node;
      if (sel === "[data-bmact]" && node.dataset && node.dataset.bmact != null) return node;
      if (sel === "[data-refact]" && node.dataset && node.dataset.refact != null) return node;
      if (sel === "[data-refmore]" && node.dataset && node.dataset.refmore != null) return node;
      if (sel === "[data-wconfirm]" && node.dataset && node.dataset.wconfirm != null) return node;
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

/** Extract a CSS rule body by selector. Asserts the selector is present. */
function ruleBody(css, selectorRe){
  const m = css.match(selectorRe);
  assert.ok(m, `rule matching ${selectorRe} present`);
  return m[1];
}

/* ================================================================
 * P3 compositions — each surface's exact bar and menu, in order
 * ================================================================ */

test("P3 1: pane bookmark bar is jump · comment · note · more (default, comment available)", () => {
  const html = bookmarkActionsHTML(
    { t: "1", text: "x", node: "n1" },
    { icons: ICONS, context: "pane", workspaceOpen: true },
  );
  const acts = visibleBmActs(html);
  assert.deepEqual(acts, ["jump", "comment", "note", "more"]);
  for (const gone of ["copy", "sendto", "del"])
    assert.ok(!acts.includes(gone), `${gone} belongs in the overflow menu, not the pane bar`);
  assert.match(html, /class="actionbar tear"/);
});

test("P3 2: inbox bookmark bar is jump · copy · note · more", () => {
  const html = bookmarkActionsHTML(
    { t: "1", text: "x", node: "n1" },
    { icons: ICONS, context: "inbox" },
  );
  const acts = visibleBmActs(html);
  assert.deepEqual(acts, ["jump", "copy", "note", "more"]);
  for (const gone of ["comment", "sendto", "del"])
    assert.ok(!acts.includes(gone), `${gone} must not be visible on the inbox bar`);
});

test("P3 3: section reference bar is jump · copy · sendto · trash (no more)", () => {
  const html = referenceHTML(
    {
      id: "r1",
      snapshot: { lane: "#f00", station: "S", speaker: "you", time: "T", text: "hi" },
    },
    { esc: s => s, md: s => s, fmtWhen: s => s, icons: ICONS },
  );
  const acts = visibleRefActs(html);
  assert.deepEqual(acts, ["jump", "copy", "sendto", "trash"]);
  assert.ok(!acts.includes("more"), "embedded bubble has no overflow");
  assert.doesNotMatch(html, /data-refact="more"/);
  /* clamp trigger must remain a distinct attribute (collision fence) */
  assert.match(html, /data-refmore/);
  assert.doesNotMatch(html, /data-refact="more"[^>]*data-refmore|data-refmore[^>]*data-refact="more"/);
});

test("P3 4: pane menu is copy · sendto · del when comment is on the bar", () => {
  assert.equal(typeof bookmarksMod.bookmarkMenuHTML, "function",
    "bookmarkMenuHTML must be exported from bookmarks.js");
  const html = bookmarksMod.bookmarkMenuHTML(
    { t: "1", text: "x", node: "n1" },
    { icons: ICONS, context: "pane" },
  );
  const acts = visibleBmActs(html);
  assert.deepEqual(acts, ["copy", "sendto", "del"]);
  assert.ok(!acts.includes("comment"), "comment is on the bar, not the menu");
});

test("P3 5: inbox menu is sendto · del (no copy — copy is on the bar)", () => {
  assert.equal(typeof bookmarksMod.bookmarkMenuHTML, "function");
  const html = bookmarksMod.bookmarkMenuHTML(
    { t: "1", text: "x", node: "n1" },
    { icons: ICONS, context: "inbox" },
  );
  const acts = visibleBmActs(html);
  assert.deepEqual(acts, ["sendto", "del"]);
  assert.ok(!acts.includes("comment"), "comment composer is pane-only");
  assert.ok(!acts.includes("copy"), "copy is already on the inbox bar");
});

test("P3 6: when Comment is withheld, pane bar promotes Copy and drops it from the menu", () => {
  /* comment withheld: comment rows (nt.anchor) and bookmarks with no node */
  for (const nt of [
    { t: "2", text: "x", node: "n1", anchor: "1" },
    { t: "3", text: "orphan", uid: "u1" },
  ]){
    const bar = visibleBmActs(bookmarkActionsHTML(
      nt, { icons: ICONS, context: "pane", workspaceOpen: true }));
    assert.deepEqual(bar, ["jump", "copy", "note", "more"],
      `bar promotes copy when comment withheld (${nt.t})`);
    assert.ok(!bar.includes("comment"));

    const menu = visibleBmActs(bookmarksMod.bookmarkMenuHTML(
      nt, { icons: ICONS, context: "pane" }));
    assert.deepEqual(menu, ["sendto", "del"],
      `menu drops copy when it is on the bar (${nt.t})`);
    assert.ok(!menu.includes("copy"));
    assert.ok(!menu.includes("comment"));
  }
});

test("P3 7: embedded bubble emits no more button; referenceMenuHTML is gone", () => {
  const html = referenceHTML(
    { id: "r1", snapshot: { text: "hi", lane: "#0", station: "", speaker: "", time: "" } },
    { esc: s => s, md: s => s, fmtWhen: s => s, icons: ICONS },
  );
  assert.doesNotMatch(html, /data-refact="more"/);
  assert.doesNotMatch(html, /aria-label="more actions"/);
  assert.equal(typeof notesMod.referenceMenuHTML, "undefined",
    "referenceMenuHTML must be deleted — nothing calls it");
  assert.doesNotMatch(notesSrc, /export function referenceMenuHTML/);
  assert.doesNotMatch(notesSrc, /function openReferenceOverflow/);
  assert.doesNotMatch(notesSrc, /refact === "more"|dataset\.refact === "more"/);
});

test("P3 8: inline Remove is .danger on the reference bar", () => {
  const html = referenceHTML(
    { id: "r1", snapshot: { text: "hi", lane: "#0", station: "", speaker: "", time: "" } },
    { esc: s => s, md: s => s, fmtWhen: s => s, icons: ICONS },
  );
  assert.match(
    html,
    /data-refact="trash"[^>]*class="[^"]*danger|class="[^"]*danger[^"]*"[^>]*data-refact="trash"/,
    "danger must ride on the trash button itself",
  );
  /* only trash is red — no other bar button carries .danger */
  const red = [...html.matchAll(/<button([^>]*)>/g)]
    .filter(m => /class="[^"]*danger/.test(m[1]))
    .map(m => (m[1].match(/data-refact="([^"]+)"/) || [])[1]);
  assert.deepEqual(red, ["trash"]);
});

/* ================================================================
 * P3 9 — Remove routes through confirm (no window.confirm)
 * ================================================================ */

test("P3 9: inline Remove opens confirm; only ok fires the delete", async () => {
  /* Minimal notes harness: open a note with one reference, tap trash, assert
     the confirm popover, cancel does nothing, ok DELETEs. */
  const notesworkspace = el("div", { id: "notesworkspace" });
  const wsscrim = el("div", { id: "wsscrim" });
  const wspanel = el("div", { id: "wspanel" });
  wspanel.getBoundingClientRect = () =>
    ({ top: 0, left: 0, width: 400, height: 600, right: 400, bottom: 600 });
  const wstitle = el("h2", { id: "wstitle", textContent: "Notes" });
  const wsback = el("button", { id: "wsback", hidden: true });
  const wsclose = el("button", { id: "wsclose" });
  const wsplacetext = el("span", { id: "wsplacetext" });
  const wsplacecancel = el("button", { id: "wsplacecancel" });
  const wszones = el("div", { id: "wszones" });
  const wsinbox = el("div", { id: "wsinbox" });
  const wsnav = el("div", { id: "wsnav" });
  const wsnote = el("div", { id: "wsnote" });
  const wsinboxtabs = el("div", { id: "wsinboxtabs" });
  const wsinboxlist = el("div", { id: "wsinboxlist" });
  const wscards = el("div", { id: "wscards" });
  const wsnewnote = el("button", { id: "wsnewnote" });
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
  wspanel.appendChild(wszones);
  wszones.appendChild(wsinbox);
  wszones.appendChild(wsnav);
  wszones.appendChild(wsnote);
  wsinbox.appendChild(wsinboxtabs);
  wsinbox.appendChild(wsinboxlist);
  wsnav.appendChild(wscards);
  wsnav.appendChild(wsnewnote);
  wsnote.appendChild(wssections);
  wsnote.appendChild(wsnoteempty);

  const docListeners = {};
  const document = {
    activeElement: null,
    contains: () => true,
    createElement: tag => el(tag),
    querySelector: () => null,
    addEventListener(type, fn){
      (docListeners[type] || (docListeners[type] = [])).push(fn);
    },
    removeEventListener(type, fn){
      if (!docListeners[type]) return;
      docListeners[type] = docListeners[type].filter(f => f !== fn);
    },
  };

  const apiLog = [];
  const docs = {
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
  };
  let notes = [{ id: "n1", title: "N", order: 0 }];
  const api = async (url, opts = {}) => {
    apiLog.push({ url, method: opts.method || "GET", body: opts.body });
    if (url === "/api/notes" && (!opts.method || opts.method === "GET"))
      return { notes };
    const m = url.match(/^\/api\/notes\/([^/]+)$/);
    if (m && (!opts.method || opts.method === "GET"))
      return docs[decodeURIComponent(m[1])];
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

  const feature = createNotesFeature({
    roots: {
      notesworkspace, wsscrim, wspanel, wstitle, wsback, wsclose,
      wsplacetext, wsplacecancel, wszones, wsinbox, wsnav, wsnote,
      wsinboxtabs, wsinboxlist, wscards, wsnewnote, wssections, wsnoteempty,
    },
    document,
    storage: makeStorage(),
    setTimeout: () => 0,
    clearTimeout: () => {},
    requestAnimationFrame: fn => { fn(); return 0; },
    CSS: { escape: s => s },
    esc: s => String(s ?? ""),
    md: s => String(s ?? ""),
    fmtWhen: s => s,
    fmtNoteMeta: () => "",
    hashStr: s => s,
    icons: ICONS,
    api,
    bookmarks: () => [],
    nodeById: () => null,
    laneList: () => [],
    laneColor: () => "",
    toast: () => {},
    copyText: () => {},
    jumpToChatAddress: () => true,
    isNarrow: () => false,
    notesbtn: () => notesbtn,
    activeElement: () => null,
  });
  feature.bind();
  feature.open();
  await Promise.resolve();
  await Promise.resolve();

  /* open the note so wsActive carries the reference */
  const card = el("button", { className: "wscard", dataset: { note: "n1" } });
  card.dataset.note = "n1";
  card.closest = sel => (sel === ".wscard" ? card : null);
  wscards.dispatch("click", { target: card });
  await Promise.resolve();
  await Promise.resolve();

  const sec = el("div", { className: "wssec", dataset: { sec: "s1" } });
  sec.dataset.sec = "s1";
  const ref = el("div", { className: "wsref", dataset: { ref: "r1" } });
  ref.dataset.ref = "r1";
  const wrap = el("div", { className: "wsrefs" });
  wrap.appendChild(ref);
  sec.appendChild(wrap);
  wssections.appendChild(sec);

  const trashBtn = el("button", { dataset: { refact: "trash" } });
  trashBtn.dataset.refact = "trash";
  trashBtn.closest = sel => {
    if (sel === "[data-refact]") return trashBtn;
    if (sel === ".wsref") return ref;
    if (sel === ".wssec") return sec;
    return null;
  };
  trashBtn.getBoundingClientRect = () =>
    ({ top: 100, left: 100, bottom: 144, right: 144, width: 44, height: 44 });

  const deletesBefore = () => apiLog.filter(x => x.method === "DELETE").length;

  wssections.dispatch("click", { target: trashBtn });
  await Promise.resolve();

  const menu = wspanel.children.find(c => (c.className || "").includes("popmenu"));
  assert.ok(menu, "confirm popover must open on trash");
  assert.match(menu.className, /wsconfirm/, "reuse the openDeleteConfirm shell");
  assert.equal(deletesBefore(), 0, "DELETE must not fire before ok");

  /* cancel — still no delete */
  const cancel = el("button", { dataset: { wconfirm: "cancel" } });
  cancel.dataset.wconfirm = "cancel";
  cancel.closest = sel => (sel === "[data-wconfirm]" ? cancel : null);
  menu.dispatch("click", { target: cancel });
  await Promise.resolve();
  assert.equal(deletesBefore(), 0, "cancel must not remove the reference");

  /* open again and confirm */
  wssections.dispatch("click", { target: trashBtn });
  await Promise.resolve();
  const menu2 = wspanel.children.find(c => (c.className || "").includes("popmenu"));
  assert.ok(menu2, "confirm re-opens");
  const ok = el("button", { dataset: { wconfirm: "ok" } });
  ok.dataset.wconfirm = "ok";
  ok.closest = sel => (sel === "[data-wconfirm]" ? ok : null);
  menu2.dispatch("click", { target: ok });
  await Promise.resolve();
  await Promise.resolve();
  assert.ok(
    apiLog.some(x => x.method === "DELETE" && /\/references\/r1/.test(x.url)),
    "ok must DELETE the reference",
  );

  /* never window.confirm — blocked in the iOS PWA */
  assert.doesNotMatch(notesSrc, /window\.confirm/);
});

/* ================================================================
 * P3 10 — .rot180 / .rot90l element-level; duplicate gone
 * ================================================================ */

test("P3 10: .rot180 and .rot90l apply outside .bubactions; duplicate is gone", () => {
  /* Assert on the rule BODY extracted by regex, not on class names in HTML. */
  const rot180Bodies = [...chatCssSrc.matchAll(/\.rot180\s*\{([^}]*)\}/g)].map(m => m[1]);
  const rot90lBodies = [...chatCssSrc.matchAll(/\.rot90l\s*\{([^}]*)\}/g)].map(m => m[1]);
  assert.equal(rot180Bodies.length, 1, "exactly one .rot180 rule (duplicate dropped)");
  assert.equal(rot90lBodies.length, 1, "exactly one .rot90l rule");
  assert.match(rot180Bodies[0], /display:\s*inline-flex/);
  assert.match(rot180Bodies[0], /transform:\s*rotate\(180deg\)/);
  assert.match(rot90lBodies[0], /display:\s*inline-flex/);
  assert.match(rot90lBodies[0], /transform:\s*rotate\(-90deg\)/);

  /* selectors are element-level — not scoped under .bubactions */
  assert.match(chatCssSrc, /(?:^|[^.\w-])\.rot180\s*\{/m);
  assert.match(chatCssSrc, /(?:^|[^.\w-])\.rot90l\s*\{/m);
  assert.doesNotMatch(chatCssSrc, /\.bubactions\s+\.rot180/);
  assert.doesNotMatch(chatCssSrc, /\.bubactions\s+\.rot90l/);

  /* ownership stays in chat.css (cascade signature tests pin file contents) */
  assert.match(chatCssSrc, /\.rot180/);
  assert.match(chatCssSrc, /\.rot90l/);
});

/* ================================================================
 * Send-to still dispatches (composition moved it; behaviour unchanged)
 * ================================================================ */

test("P3 sendto: pane/inbox menus and reference bar still carry sendto dispatch", () => {
  const paneMenu = bookmarksMod.bookmarkMenuHTML(
    { t: "1", text: "x", node: "n1" }, { icons: ICONS, context: "pane" });
  assert.match(paneMenu, /data-bmact="sendto"/);
  const inboxMenu = bookmarksMod.bookmarkMenuHTML(
    { t: "1", text: "x", node: "n1" }, { icons: ICONS, context: "inbox" });
  assert.match(inboxMenu, /data-bmact="sendto"/);
  const ref = referenceHTML(
    { id: "r1", snapshot: { text: "ref body", lane: "#0", station: "", speaker: "", time: "" } },
    { esc: s => s, md: s => s, fmtWhen: s => s, icons: ICONS },
  );
  assert.match(ref, /data-refact="sendto"/);

  assert.match(bookmarksSrc, /bmact === "sendto"|dataset\.bmact === "sendto"/);
  assert.match(bookmarksSrc, /openSendTo\s*\(/);
  assert.match(notesSrc, /refact === "sendto"|dataset\.refact === "sendto"/);
  assert.match(notesSrc, /openSendTo/);
  assert.doesNotMatch(notesSrc, /from ["']\.\/chat\.js["']/);
  assert.match(appSrc, /openSendTo/);
});

test("P3 sendto: notes wires openSendTo via deps (never imports the picker)", () => {
  assert.match(appSrc, /openSendTo/);
  assert.match(notesSrc, /openSendTo/);
  assert.doesNotMatch(notesSrc, /from ["']\.\/chat\.js["']/);
  assert.doesNotMatch(notesSrc, /import\s*\{[^}]*openSendTo[^}]*\}\s*from\s*["']\.\/bookmarks\.js["']/);
});

/* ================================================================
 * Destructive is .danger, last, after .sep — bookmark menus
 * ================================================================ */

test("P3: pane/inbox Delete is .danger, last, after .sep (coupled to data-bmact=del)", () => {
  assert.equal(typeof bookmarksMod.bookmarkMenuHTML, "function");
  for (const context of ["pane", "inbox"]){
    const html = bookmarksMod.bookmarkMenuHTML(
      { t: "1", text: "x", node: "n1" },
      { icons: ICONS, context },
    );
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

/* ================================================================
 * Longpress removed; destroy stays idempotent
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
 * canPlace / context regression lock
 * ================================================================ */

test("P6 12: canPlace / context semantics unchanged (regression lock)", () => {
  const closed = bookmarkActionsHTML(
    { t: "1", text: "x", node: "n1" },
    { icons: ICONS, context: "pane", workspaceOpen: false },
  );
  assert.ok(!closed.includes('data-bmact="note"'));
  assert.match(closed, /data-bmact="jump"/);
  const open = bookmarkActionsHTML(
    { t: "1", text: "x", node: "n1" },
    { icons: ICONS, context: "pane", workspaceOpen: true },
  );
  assert.match(open, /data-bmact="note"/);
  const phone = bookmarkActionsHTML(
    { t: "1", text: "x", node: "n1" },
    { icons: ICONS, context: "pane", workspaceOpen: false, singleZone: true },
  );
  assert.match(phone, /data-bmact="note"/);
  const inbox = bookmarkActionsHTML(
    { t: "1", text: "x", node: "n1" },
    { icons: ICONS, context: "inbox" },
  );
  assert.match(inbox, /data-bmact="note"/);
  const multi = bookmarkActionsHTML(
    { t: "1", text: "x", node: "n1" },
    { icons: ICONS, context: "pane", workspaceOpen: false, singleZone: false },
  );
  assert.ok(!multi.includes('data-bmact="note"'));
});

test("P3: comment stays pane-only and is withheld from comments (bar + menu)", () => {
  /* With comment on the bar, the no-nesting rule lives in the bar builder too. */
  const paneBar = bookmarkActionsHTML(
    { t: "1", text: "x", node: "n1" }, { icons: ICONS, context: "pane", workspaceOpen: true });
  assert.match(paneBar, /data-bmact="comment"/);
  const commentBar = bookmarkActionsHTML(
    { t: "2", text: "x", node: "n1", anchor: "1" },
    { icons: ICONS, context: "pane", workspaceOpen: true });
  assert.doesNotMatch(commentBar, /data-bmact="comment"/);
  const inboxBar = bookmarkActionsHTML(
    { t: "1", text: "x", node: "n1" }, { icons: ICONS, context: "inbox" });
  assert.doesNotMatch(inboxBar, /data-bmact="comment"/);
  const inboxMenu = bookmarksMod.bookmarkMenuHTML(
    { t: "1", text: "x", node: "n1" }, { icons: ICONS, context: "inbox" });
  assert.doesNotMatch(inboxMenu, /data-bmact="comment"/);
});

/* ================================================================
 * Case 13 — inbox + reference send-to text + exclude source chat
 * ================================================================ */

test("P6 13: inbox and reference send-to carry text and exclude the source chat", () => {
  assert.match(notesSrc, /openSendTo\s*\(\s*\{/);
  assert.match(notesSrc, /exceptId\s*:/, "send-to from notes must exclude the source chat");
  assert.match(notesSrc, /stripAssetRefs\s*\(/, "send-to from notes strips asset markers");
  const calls = notesSrc.match(/openSendTo\s*\(\s*\{/g) || [];
  assert.ok(calls.length >= 2, `expected ≥2 openSendTo call sites, got ${calls.length}`);
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

  assert.match(formatSrc, /export const ASSET_REF_RE/);
  assert.match(formatSrc, /export function stripAssetRefs/);
  assert.doesNotMatch(chatSrc, /export const ASSET_REF_RE/);
  assert.doesNotMatch(chatSrc, /export function stripAssetRefs/);
  assert.match(chatSrc, /from ["']\.\/format\.js["']/);
  assert.match(chatSrc, /stripAssetRefs|ASSET_REF_RE/);

  assert.match(chatSrc, /bact === "sendto"[\s\S]{0,400}stripAssetRefs/);
  assert.match(chatSrc, /forkFromTurn\(\s*stripAssetRefs/);
  assert.match(bookmarksSrc, /stripAssetRefs/);
  assert.match(notesSrc, /stripAssetRefs/);
  assert.match(bookmarksSrc, /from ["']\.\/format\.js["']/);
  assert.doesNotMatch(bookmarksSrc, /from ["']\.\/chat\.js["']/);
  assert.doesNotMatch(notesSrc, /from ["']\.\/chat\.js["']/);
});

/* ================================================================
 * Name collision 1 — data-refmore (clamp) survives; overflow gone
 * ================================================================ */

test("P6 collision 1: data-refmore remains the clamp; no data-refact=more collision", () => {
  const html = referenceHTML(
    { id: "r1", snapshot: { text: "long", lane: "#0", station: "", speaker: "", time: "" } },
    { esc: s => s, md: s => s, fmtWhen: s => s, icons: ICONS },
  );
  assert.match(html, /data-refmore/, "clamp trigger kept");
  assert.doesNotMatch(html, /data-refact="more"/, "overflow trigger removed with the menu");
  assert.match(notesSrc, /data-refmore/);
  assert.doesNotMatch(notesSrc, /refact === "more"|dataset\.refact === "more"/);
});

/* ================================================================
 * Name collision 2 — menu items reuse data-bmact (bookmark menus)
 * ================================================================ */

test("P6 collision 2: overflow menu items reuse data-bmact for dispatch", () => {
  assert.equal(typeof bookmarksMod.bookmarkMenuHTML, "function");
  const bm = bookmarksMod.bookmarkMenuHTML(
    { t: "1", text: "x", node: "n1" }, { icons: ICONS, context: "pane" });
  assert.match(bm, /data-bmact="copy"/);
  assert.match(bm, /data-bmact="sendto"/);
  assert.match(bm, /data-bmact="del"/);
  /* reference has no menu; its bar actions still use data-refact */
  const ref = referenceHTML(
    { id: "r1", snapshot: { text: "x", lane: "#0", station: "", speaker: "", time: "" } },
    { esc: s => s, md: s => s, fmtWhen: s => s, icons: ICONS },
  );
  assert.match(ref, /data-refact="copy"/);
  assert.match(ref, /data-refact="trash"/);
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

  /* reference: closed by class toggle, bar is always in HTML but hidden via CSS */
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
  /* reference overflow is gone; bookmark menus (pane + inbox) still use the helpers */
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

/* ================================================================
 * Only the destructive item carries .danger
 * ================================================================ */

test("P6 review: only the destructive item carries .danger", () => {
  const bm = bookmarksMod.bookmarkMenuHTML(
    { t: "1", text: "x", node: "n1" }, { icons: ICONS, context: "pane" });
  const redBm = [...bm.matchAll(/<button([^>]*)class="danger"/g)]
    .map(m => (m[1].match(/data-bmact="([^"]+)"/) || [])[1]);
  assert.deepEqual(redBm, ["del"],
    "exactly one .danger item in the bookmark menu, and it is del");

  const ref = referenceHTML(
    { id: "r1", snapshot: { text: "x", lane: "#0", station: "", speaker: "", time: "" } },
    { esc: s => s, md: s => s, fmtWhen: s => s, icons: ICONS },
  );
  const redRef = [...ref.matchAll(/<button([^>]*)>/g)]
    .filter(m => /class="[^"]*danger/.test(m[1]))
    .map(m => (m[1].match(/data-refact="([^"]+)"/) || [])[1]);
  assert.deepEqual(redRef, ["trash"],
    "exactly one .danger item on the reference bar, and it is trash");
});

/* silence unused — DRAFT_KEY_PREFIX kept for parity with prior harness imports */
void DRAFT_KEY_PREFIX;
void ruleBody;
void menuSrc;
