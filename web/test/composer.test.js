/* Characterization tests for web/js/composer.js — pure decisions + factory
 * adapters with a small fake DOM and injected timers/fetch/FormData/URL.
 * Does not re-test format escaping or chat echo retirement policy. */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import {
  DRAFT_KEY_PREFIX,
  PROMPT_MAX_HEIGHT_PX,
  UPLOAD_WAIT_POLL_MS,
  UPLOAD_WAIT_TIMEOUT_MS,
  normalizePromptText,
  draftStorageKey,
  promptHeightPx,
  shouldSendOnEnter,
  busyChrome,
  closedChrome,
  isImageFileType,
  stagedFileName,
  attachmentInputKey,
  mergeFailedDraft,
  restoreStageOnFailure,
  hasSendPayload,
  completedRefs,
  anyUploading,
  buildSentEcho,
  stageChipHTML,
  stageRowHTML,
  createComposerFeature,
  isClaudeNativeForkCommand,
} from "../js/composer.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const composerSrc = readFileSync(join(__dirname, "../js/composer.js"), "utf8");

/* ---------- module shape ---------- */
test("composer.js exports factory and pure helpers; reuses format only", () => {
  assert.match(composerSrc, /export function createComposerFeature/);
  assert.match(composerSrc, /from "\.\/format\.js"/);
  assert.doesNotMatch(composerSrc, /from "\.\/chat\.js"/);
  assert.doesNotMatch(composerSrc, /from "\.\/polling\.js"/);
  assert.doesNotMatch(composerSrc, /from "\.\/app\.js"/);
  assert.doesNotMatch(composerSrc, /from "\.\/bookmarks\.js"/);
  assert.doesNotMatch(composerSrc, /from "\.\/notes\.js"/);
  assert.doesNotMatch(composerSrc, /from "\.\/search\.js"/);
  assert.doesNotMatch(composerSrc, /from "\.\/sheets\.js"/);
  assert.doesNotMatch(composerSrc, /from "\.\/cards\.js"/);
  assert.doesNotMatch(composerSrc, /from "\.\/map\.js"/);
  assert.doesNotMatch(composerSrc, /export function esc\b/);
  assert.doesNotMatch(composerSrc, /export function withCsrf\b/);
  assert.doesNotMatch(composerSrc, /export function api\b/);
  assert.doesNotMatch(composerSrc, /test\/debug accessors|getStage|_stage/);
});

test("timing and key constants match live contracts", () => {
  assert.equal(DRAFT_KEY_PREFIX, "scimux-draft:");
  assert.equal(PROMPT_MAX_HEIGHT_PX, 120);
  assert.equal(UPLOAD_WAIT_POLL_MS, 150);
  assert.equal(UPLOAD_WAIT_TIMEOUT_MS, 12000);
});

/* ---------- pure: NBSP / resize / Enter / paste helpers ---------- */
test("normalizePromptText converts NBSP to space", () => {
  assert.equal(normalizePromptText("a\u00a0b\u00a0c"), "a b c");
  assert.equal(normalizePromptText(""), "");
  assert.equal(normalizePromptText(null), "");
  assert.equal(normalizePromptText("plain"), "plain");
});

test("promptHeightPx caps at 120", () => {
  assert.equal(promptHeightPx(40), 40);
  assert.equal(promptHeightPx(120), 120);
  assert.equal(promptHeightPx(500), 120);
  assert.equal(promptHeightPx(0), 0);
});

test("desktop Enter modifier matrix and phone behavior", () => {
  assert.equal(shouldSendOnEnter({ key: "Enter" }), true);
  assert.equal(shouldSendOnEnter({ key: "Enter", shiftKey: true }), false);
  assert.equal(shouldSendOnEnter({ key: "Enter", altKey: true }), false);
  assert.equal(shouldSendOnEnter({ key: "Enter", metaKey: true }), false);
  assert.equal(shouldSendOnEnter({ key: "Enter", ctrlKey: true }), false);
  assert.equal(shouldSendOnEnter({ key: "a" }), false);
  assert.equal(shouldSendOnEnter({ key: "Enter", isPhoneTouch: true }), false);
  assert.equal(shouldSendOnEnter({
    key: "Enter", shiftKey: false, altKey: false, metaKey: false, ctrlKey: false,
    isPhoneTouch: false,
  }), true);
});

test("draftStorageKey is per-node", () => {
  assert.equal(draftStorageKey("n1"), "scimux-draft:n1");
  assert.equal(draftStorageKey("other"), "scimux-draft:other");
});

test("busy and closed chrome pure shapes", () => {
  assert.deepEqual(busyChrome(true), {
    busy: true, ariaLabel: "stop agent", title: "stop agent", stopClass: true,
  });
  assert.deepEqual(busyChrome(false), {
    busy: false, ariaLabel: "send", title: "send", stopClass: false,
  });
  const c = closedChrome(true);
  assert.equal(c.closed, true);
  assert.equal(c.contentEditable, "false");
  assert.match(c.placeholder, /Thread closed/);
  assert.equal(c.sendDisabled, true);
  const o = closedChrome(false);
  assert.equal(o.contentEditable, "true");
  assert.match(o.placeholder, /Message the agent/);
});

test("image type, staged name, attachment source key", () => {
  assert.equal(isImageFileType("image/png"), true);
  assert.equal(isImageFileType("application/pdf"), false);
  assert.equal(stagedFileName({ name: "a.png" }, true), "a.png");
  assert.equal(stagedFileName({}, true), "pasted-image.png");
  assert.equal(stagedFileName({}, false), "file");
  assert.equal(attachmentInputKey("image"), "image");
  assert.equal(attachmentInputKey("file"), "file");
  assert.equal(attachmentInputKey("other"), "file");
});

test("mergeFailedDraft and restoreStageOnFailure order", () => {
  assert.equal(mergeFailedDraft("hello", ""), "hello");
  assert.equal(mergeFailedDraft("hello", "hello"), "hello");
  assert.equal(mergeFailedDraft("hello", "extra"), "hello\n\nextra");
  assert.deepEqual(restoreStageOnFailure([], [{ key: 1 }]), [{ key: 1 }]);
  assert.deepEqual(
    restoreStageOnFailure([{ key: 2 }], [{ key: 1 }]),
    [{ key: 2 }, { key: 1 }],
  );
  assert.deepEqual(restoreStageOnFailure([{ key: 2 }], []), [{ key: 2 }]);
});

test("send payload and upload status helpers", () => {
  assert.equal(hasSendPayload("", []), false);
  assert.equal(hasSendPayload("  ", []), false);
  assert.equal(hasSendPayload("hi", []), true);
  assert.equal(hasSendPayload("", [{ path: "x" }]), true);
  assert.deepEqual(
    completedRefs([
      { status: "up" },
      { status: "done", ref: { path: "a" } },
      { status: "err" },
      { status: "done", ref: { path: "b" } },
    ]),
    [{ path: "a" }, { path: "b" }],
  );
  assert.equal(anyUploading([{ status: "done" }]), false);
  assert.equal(anyUploading([{ status: "up" }]), true);
});

test("buildSentEcho seen watermark depends on chatSig", () => {
  assert.deepEqual(
    buildSentEcho({ node: "n1", text: "t", atts: [], at: 9, chatSig: "sig", turnsLength: 3 }),
    { node: "n1", text: "t", atts: [], at: 9, seen: 3 },
  );
  assert.deepEqual(
    buildSentEcho({ node: "n1", text: "t", atts: [], at: 9, chatSig: "", turnsLength: 3 }),
    { node: "n1", text: "t", atts: [], at: 9, seen: null },
  );
});

test("stageChipHTML and stageRowHTML markup/status", () => {
  const esc = s => String(s).replace(/</g, "&lt;");
  const chip = stageChipHTML({
    key: 7, name: "a<b>.png", isImage: true, preview: "blob:1", status: "up",
  }, { esc, iconFile: "F" });
  assert.match(chip, /stagechip up/);
  assert.match(chip, /data-key="7"/);
  assert.match(chip, /a&lt;b/); // test esc only rewrites "<"
  assert.match(chip, /blob:1/);
  assert.match(chip, /class="spin"/);
  assert.match(chip, /class="sx"/);

  const fileChip = stageChipHTML({
    key: 1, name: "doc.pdf", isImage: false, preview: "", status: "done",
  }, { esc, iconFile: "FILE" });
  assert.match(fileChip, /fico/);
  assert.match(fileChip, /FILE/);
  assert.doesNotMatch(fileChip, /<img/);

  assert.equal(stageRowHTML([], { esc }), "");
  assert.match(stageRowHTML([{ key: 1, name: "x", status: "done" }], { esc, iconFile: "F" }), /stagechip/);
});

/* ---------- fake DOM / factory harness ---------- */

function el(tag, attrs = {}){
  const listeners = new Map();
  const classSet = new Set(
    (attrs.className || "").split(/\s+/).filter(Boolean),
  );
  const node = {
    tagName: String(tag).toUpperCase(),
    id: attrs.id || "",
    innerHTML: attrs.innerHTML || "",
    textContent: attrs.textContent || "",
    innerText: attrs.innerText != null ? attrs.innerText : (attrs.textContent || ""),
    value: attrs.value || "",
    hidden: !!attrs.hidden,
    disabled: !!attrs.disabled,
    title: attrs.title || "",
    scrollHeight: attrs.scrollHeight || 40,
    offsetHeight: attrs.offsetHeight || 80,
    style: attrs.style || {},
    dataset: Object.assign({}, attrs.dataset || {}),
    children: attrs.children || [],
    parentNode: null,
    classList: {
      _set: classSet,
      add(...xs){ xs.forEach(x => classSet.add(x)); },
      remove(...xs){ xs.forEach(x => classSet.delete(x)); },
      toggle(x, force){
        if (force === true) classSet.add(x);
        else if (force === false) classSet.delete(x);
        else if (classSet.has(x)) classSet.delete(x);
        else classSet.add(x);
        return classSet.has(x);
      },
      contains(x){ return classSet.has(x); },
    },
    setAttribute(k, v){
      this._attrs = this._attrs || {};
      this._attrs[k] = String(v);
      if (k === "contenteditable") this.contentEditable = String(v);
    },
    getAttribute(k){ return (this._attrs || {})[k]; },
    addEventListener(type, fn, opts){
      if (!listeners.has(type)) listeners.set(type, []);
      listeners.get(type).push({ fn, opts });
    },
    removeEventListener(type, fn){
      const arr = listeners.get(type) || [];
      listeners.set(type, arr.filter(x => x.fn !== fn));
    },
    getBoundingClientRect(){
      return attrs.rect || { left: 10, top: 200, width: 30, height: 30 };
    },
    querySelectorAll(sel){
      if (sel === ".sx") {
        return this.children.filter(c => c.classList && c.classList.contains("sx"));
      }
      return [];
    },
    querySelector(sel){
      if (sel.startsWith("#")) {
        const id = sel.slice(1);
        if (this.id === id) return this;
      }
      return null;
    },
    closest(sel){
      if (sel.includes("button[data-src]") && this.dataset && this.dataset.src)
        return this;
      if (sel.includes("button") && this.tagName === "BUTTON") return this;
      return this.parentNode ? this.parentNode.closest(sel) : null;
    },
    click(){
      const arr = listeners.get("click") || [];
      arr.forEach(({ fn }) => fn({ type: "click", target: this, preventDefault(){}, stopPropagation(){} }));
    },
    _listeners: listeners,
    listenerCount(type){ return (listeners.get(type) || []).length; },
    firstListener(type){ return (listeners.get(type) || [])[0]?.fn; },
  };
  // proxy textContent ↔ innerText for contenteditable
  Object.defineProperty(node, "textContent", {
    get(){ return this._text || ""; },
    set(v){ this._text = String(v || ""); this.innerText = this._text; },
    configurable: true,
  });
  node.textContent = attrs.textContent || "";
  if (attrs.id) node.id = attrs.id;
  return node;
}

function makeStorage(init = {}){
  const map = new Map(Object.entries(init));
  return {
    getItem(k){ return map.has(k) ? map.get(k) : null; },
    setItem(k, v){ map.set(k, String(v)); },
    removeItem(k){ map.delete(k); },
    _map: map,
  };
}

function makeRoots(){
  const attmenu = el("div", { id: "attmenu", hidden: true });
  attmenu.children = [
    el("button", { dataset: { src: "image" } }),
    el("button", { dataset: { src: "file" } }),
  ];
  attmenu.children.forEach(c => { c.parentNode = attmenu; });
  const attstage = el("div", { id: "attstage", hidden: true });
  attstage.querySelectorAll = sel => {
    if (sel !== ".sx") return [];
    const keys = [...attstage.innerHTML.matchAll(/class="sx" data-key="([^"]+)"/g)]
      .map(m => m[1]);
    attstage._stageButtons = keys.map(key => el("button", {
      className: "sx", dataset: { key },
    }));
    return attstage._stageButtons;
  };
  return {
    promptbar: el("div", { id: "promptbar" }),
    attstage,
    prompt: el("div", { id: "prompt", scrollHeight: 40 }),
    sendbtn: el("button", { id: "sendbtn" }),
    attadd: el("button", { id: "attadd", hidden: true }),
    attmenu,
    attimg: el("input", { id: "attimg" }),
    attfile: el("input", { id: "attfile" }),
  };
}

function makeTimers(){
  let nextId = 1;
  const intervals = new Map();
  const timeouts = new Map();
  return {
    setInterval(fn, ms){
      const id = nextId++;
      intervals.set(id, { fn, ms });
      return id;
    },
    clearInterval(id){ intervals.delete(id); },
    setTimeout(fn, ms){
      const id = nextId++;
      timeouts.set(id, { fn, ms });
      return id;
    },
    clearTimeout(id){ timeouts.delete(id); },
    runIntervals(){
      for (const { fn } of [...intervals.values()]) fn();
    },
    runTimeouts(msFilter){
      for (const [id, t] of [...timeouts.entries()]) {
        if (msFilter == null || t.ms === msFilter) {
          timeouts.delete(id);
          t.fn();
        }
      }
    },
    intervalCount(){ return intervals.size; },
    timeoutCount(){ return timeouts.size; },
    hasTimeout(ms){
      return [...timeouts.values()].some(t => t.ms === ms);
    },
  };
}

function makeFeature(overrides = {}){
  const roots = makeRoots();
  const storage = overrides.storage || makeStorage(overrides.drafts || {});
  let sel = overrides.sel ?? "n1";
  const nodes = overrides.nodes || [{ id: "n1", title: "A", ended_at: "" }];
  const apiCalls = [];
  const fetchCalls = [];
  const revoked = [];
  const created = [];
  const alerts = [];
  const echoes = [];
  const paints = [];
  const clears = [];
  let chatSig = overrides.chatSig ?? "sig";
  let lastTurns = overrides.lastTurns ?? [{ role: "user", text: "x" }];
  const timers = makeTimers();
  let now = overrides.now ?? 1_000_000;
  let uploadHandler = overrides.uploadHandler;
  let sendHandler = overrides.sendHandler;
  let interruptHandler = overrides.interruptHandler;

  const doc = {
    execCommandCalls: [],
    execCommand(cmd, _a, text){
      this.execCommandCalls.push({ cmd, text });
      if (cmd === "insertText" && roots.prompt) {
        roots.prompt.textContent = (roots.prompt.textContent || "") + (text || "");
      }
      return true;
    },
    addEventListener(...args){ this._el = this._el || el("document"); return this._el.addEventListener(...args); },
    removeEventListener(...args){ return this._el && this._el.removeEventListener(...args); },
    listenerCount(type){ return this._el ? this._el.listenerCount(type) : 0; },
    firstListener(type){ return this._el ? this._el.firstListener(type) : null; },
  };
  // bind document listeners on a real-ish target
  const docTarget = el("document");
  doc.addEventListener = (...a) => docTarget.addEventListener(...a);
  doc.removeEventListener = (...a) => docTarget.removeEventListener(...a);
  doc.listenerCount = t => docTarget.listenerCount(t);
  doc.firstListener = t => docTarget.firstListener(t);

  const FormDataMock = function FormDataMock(){
    this.parts = [];
    this.append = (k, v, name) => { this.parts.push({ k, v, name }); };
  };

  const feature = createComposerFeature({
    roots,
    document: doc,
    storage,
    sel: () => sel,
    nodeById: id => nodes.find(n => n.id === id) || null,
    isPhoneTouch: () => !!overrides.isPhoneTouch,
    esc: s => String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;"),
    icons: {
      ICON_PLUS: "P", ICON_IMAGE: "I", ICON_FILE: "F",
      ICON_SEND: "S", ICON_STOP: "X",
    },
    FormData: FormDataMock,
    URL: {
      createObjectURL(f){ const u = "blob:" + (f && f.name || "x"); created.push(u); return u; },
      revokeObjectURL(u){ revoked.push(u); },
    },
    withCsrf: opts => ({ ...opts, csrf: true }),
    fetchImpl: async (path, opts) => {
      fetchCalls.push({ path, opts });
      if (typeof uploadHandler === "function") return uploadHandler(path, opts, fetchCalls);
      return {
        ok: true,
        status: 200,
        statusText: "OK",
        text: async () => "",
        json: async () => ({ attachments: [{ path: "deadbeef-" + (opts.body?.parts?.[0]?.name || "f"), mime: "image/png" }] }),
      };
    },
    api: async (path, opts) => {
      apiCalls.push({ path, opts });
      if (path.includes("/interrupt")) {
        if (typeof interruptHandler === "function") return interruptHandler(path, opts);
        return {};
      }
      if (typeof sendHandler === "function") return sendHandler(path, opts, apiCalls);
      return { delivery: "ok" };
    },
    setSentEcho: e => echoes.push(e),
    paintEcho: dest => paints.push(dest),
    clearSentEchoFor: id => clears.push(id),
    getChatSig: () => chatSig,
    getLastTurns: () => lastTurns,
    invalidateChat: () => { chatSig = ""; },
    refreshChat: () => {},
    tick: () => {},
    scheduleTick: () => {},
    alert: msg => alerts.push(msg),
    setInterval: timers.setInterval,
    clearInterval: timers.clearInterval,
    setTimeout: timers.setTimeout,
    clearTimeout: timers.clearTimeout,
    now: () => now,
    ...overrides.deps,
  });

  return {
    feature, roots, storage, apiCalls, fetchCalls, revoked, created, alerts,
    echoes, paints, clears, timers, doc, nodes,
    setSel: v => { sel = v; },
    setChatSig: v => { chatSig = v; },
    setLastTurns: v => { lastTurns = v; },
    setNow: v => { now = v; },
    setUploadHandler: fn => { uploadHandler = fn; },
    setSendHandler: fn => { sendHandler = fn; },
    setInterruptHandler: fn => { interruptHandler = fn; },
  };
}

/* ---------- bind / destroy ---------- */
test("bind is idempotent; destroy removes all owned listeners", () => {
  const ctx = makeFeature();
  ctx.feature.bind();
  ctx.feature.bind();
  assert.equal(ctx.roots.prompt.listenerCount("input"), 1);
  assert.equal(ctx.roots.prompt.listenerCount("paste"), 1);
  assert.equal(ctx.roots.prompt.listenerCount("keydown"), 1);
  assert.equal(ctx.roots.prompt.listenerCount("beforeinput"), 1);
  assert.equal(ctx.roots.sendbtn.listenerCount("click"), 1);
  assert.equal(ctx.roots.attadd.listenerCount("click"), 1);
  assert.equal(ctx.roots.attmenu.listenerCount("click"), 1);
  assert.equal(ctx.roots.attimg.listenerCount("change"), 1);
  assert.equal(ctx.roots.attfile.listenerCount("change"), 1);
  assert.equal(ctx.roots.promptbar.listenerCount("drop"), 1);
  assert.equal(ctx.roots.promptbar.listenerCount("dragover"), 1);
  assert.equal(ctx.doc.listenerCount("click"), 1);
  assert.equal(ctx.doc.listenerCount("keydown"), 1);
  // icons initialized
  assert.equal(ctx.roots.attadd.innerHTML, "P");
  assert.match(ctx.roots.attmenu.children[0].innerHTML, /Choose image/);
  assert.match(ctx.roots.attmenu.children[1].innerHTML, /Choose file/);
  assert.equal(ctx.roots.sendbtn.innerHTML, "S");
  assert.equal(ctx.roots.promptbar.classList.contains("busy"), false);
  assert.equal(ctx.roots.promptbar.classList.contains("closed"), false);

  ctx.feature.destroy();
  assert.equal(ctx.roots.prompt.listenerCount("input"), 0);
  assert.equal(ctx.roots.prompt.listenerCount("paste"), 0);
  assert.equal(ctx.roots.prompt.listenerCount("keydown"), 0);
  assert.equal(ctx.roots.prompt.listenerCount("beforeinput"), 0);
  assert.equal(ctx.roots.sendbtn.listenerCount("click"), 0);
  assert.equal(ctx.roots.attadd.listenerCount("click"), 0);
  assert.equal(ctx.roots.attmenu.listenerCount("click"), 0);
  assert.equal(ctx.doc.listenerCount("click"), 0);
  assert.equal(ctx.doc.listenerCount("keydown"), 0);

  ctx.feature.bind();
  assert.equal(ctx.roots.prompt.listenerCount("input"), 1);
  ctx.feature.destroy();
});

/* ---------- draft storage / busy closed ---------- */
test("per-node draft persistence on input and restore on select", () => {
  const ctx = makeFeature({ drafts: { "scimux-draft:n1": "saved one", "scimux-draft:n2": "saved two" } });
  ctx.feature.bind();
  ctx.feature.onSelect();
  assert.equal(ctx.feature.promptText(), "saved one");

  ctx.roots.prompt.innerText = "typed\u00a0now";
  ctx.roots.prompt.firstListener("input")();
  assert.equal(ctx.storage.getItem("scimux-draft:n1"), "typed now");

  ctx.setSel("n2");
  ctx.feature.onSelect();
  assert.equal(ctx.feature.promptText(), "saved two");
  // n1 draft untouched
  assert.equal(ctx.storage.getItem("scimux-draft:n1"), "typed now");
  ctx.feature.destroy();
});

test("busy/closed edge-only DOM changes", () => {
  const ctx = makeFeature();
  ctx.feature.bind();
  const bar = ctx.roots.promptbar;
  const btn = ctx.roots.sendbtn;
  const prompt = ctx.roots.prompt;

  ctx.feature.setComposerBusy(true);
  assert.equal(bar.classList.contains("busy"), true);
  assert.equal(btn.classList.contains("stop"), true);
  assert.equal(btn.innerHTML, "X");
  assert.equal(btn.getAttribute("aria-label"), "stop agent");
  // second identical call: edge-only — classList still true, no throw
  const html = btn.innerHTML;
  ctx.feature.setComposerBusy(true);
  assert.equal(btn.innerHTML, html);

  ctx.feature.setComposerBusy(false);
  assert.equal(bar.classList.contains("busy"), false);
  assert.equal(btn.innerHTML, "S");

  ctx.feature.setComposerClosed(true);
  assert.equal(bar.classList.contains("closed"), true);
  assert.equal(prompt.getAttribute("contenteditable"), "false");
  assert.match(prompt.dataset.placeholder, /Thread closed/);
  assert.equal(btn.disabled, true);
  assert.equal(ctx.roots.attadd.disabled, true);
  ctx.feature.setComposerClosed(true); // edge
  ctx.feature.setComposerClosed(false);
  assert.equal(prompt.getAttribute("contenteditable"), "true");
  assert.equal(btn.disabled, false);
  ctx.feature.destroy();
});

/* ---------- Enter / paste / drop ---------- */
test("desktop Enter sends; modifiers and phone do not", async () => {
  const ctx = makeFeature();
  ctx.feature.bind();
  ctx.roots.prompt.textContent = "hello";
  const prevented = [];
  const e = {
    key: "Enter", shiftKey: false, altKey: false, metaKey: false, ctrlKey: false,
    preventDefault(){ prevented.push(1); },
  };
  await ctx.roots.prompt.firstListener("keydown")(e);
  assert.equal(prevented.length, 1);
  assert.ok(ctx.apiCalls.some(c => c.path.includes("/send") && !c.path.includes("interrupt")));

  // shift
  ctx.roots.prompt.textContent = "again";
  const n = ctx.apiCalls.length;
  await ctx.roots.prompt.firstListener("keydown")({
    key: "Enter", shiftKey: true, altKey: false, metaKey: false, ctrlKey: false,
    preventDefault(){ prevented.push(1); },
  });
  assert.equal(ctx.apiCalls.length, n);

  // phone
  const ctx2 = makeFeature({ isPhoneTouch: true });
  ctx2.feature.bind();
  ctx2.roots.prompt.textContent = "phone";
  await ctx2.roots.prompt.firstListener("keydown")({
    key: "Enter", shiftKey: false, altKey: false, metaKey: false, ctrlKey: false,
    preventDefault(){},
  });
  assert.equal(ctx2.apiCalls.length, 0);
  ctx.feature.destroy();
  ctx2.feature.destroy();
});

test("plain-text paste inserts via execCommand; file paste stages", async () => {
  const ctx = makeFeature();
  ctx.feature.bind();
  ctx.feature.setAttachAvail(true);

  let prevented = false;
  ctx.roots.prompt.firstListener("paste")({
    clipboardData: {
      files: [],
      getData: t => t === "text/plain" ? "plain\u00a0text" : "",
    },
    preventDefault(){ prevented = true; },
  });
  assert.equal(prevented, true);
  assert.equal(ctx.doc.execCommandCalls.length, 1);
  assert.equal(ctx.doc.execCommandCalls[0].cmd, "insertText");
  assert.equal(ctx.doc.execCommandCalls[0].text, "plain\u00a0text");

  // file paste
  const file = { name: "pic.png", type: "image/png" };
  prevented = false;
  ctx.roots.prompt.firstListener("paste")({
    clipboardData: {
      files: [file],
      getData: () => "",
    },
    preventDefault(){ prevented = true; },
  });
  assert.equal(prevented, true);
  // allow upload microtask
  await Promise.resolve();
  assert.match(ctx.roots.attstage.innerHTML, /pic\.png/);
  ctx.feature.destroy();
});

test("beforeinput insertFromDrop forces plain text", () => {
  const ctx = makeFeature();
  ctx.feature.bind();
  let prevented = false;
  ctx.roots.prompt.firstListener("beforeinput")({
    inputType: "insertFromDrop",
    dataTransfer: { getData: () => "dropped" },
    preventDefault(){ prevented = true; },
  });
  assert.equal(prevented, true);
  assert.equal(ctx.doc.execCommandCalls[0].text, "dropped");
  // other input types ignored
  prevented = false;
  ctx.roots.prompt.firstListener("beforeinput")({
    inputType: "insertText",
    preventDefault(){ prevented = true; },
  });
  assert.equal(prevented, false);
  ctx.feature.destroy();
});

/* ---------- attachments: avail, menu, stage, isolation, upload ---------- */
test("attachment availability hides + and closes menu", () => {
  const ctx = makeFeature();
  ctx.feature.bind();
  assert.equal(ctx.roots.attadd.hidden, true);
  ctx.feature.setAttachAvail(true);
  assert.equal(ctx.roots.attadd.hidden, false);
  // open menu then disable
  ctx.roots.attmenu.hidden = false;
  ctx.roots.attadd.classList.add("armed");
  ctx.feature.setAttachAvail(false);
  assert.equal(ctx.roots.attadd.hidden, true);
  assert.equal(ctx.roots.attmenu.hidden, true);
  // edge: same value still forces hidden
  ctx.feature.setAttachAvail(false);
  assert.equal(ctx.roots.attadd.hidden, true);
  ctx.feature.destroy();
});

test("attachment menu placement and source selection", () => {
  const ctx = makeFeature();
  ctx.feature.bind();
  ctx.feature.setAttachAvail(true);
  const imgClicks = [];
  const fileClicks = [];
  ctx.roots.attimg.click = () => imgClicks.push(1);
  ctx.roots.attfile.click = () => fileClicks.push(1);

  ctx.roots.attadd.firstListener("click")({ stopPropagation(){} });
  assert.equal(ctx.roots.attmenu.hidden, false);
  assert.equal(ctx.roots.attadd.classList.contains("armed"), true);
  assert.equal(ctx.roots.attadd.getAttribute("aria-expanded"), "true");
  assert.equal(ctx.roots.attmenu.style.left, "10px");
  assert.equal(ctx.roots.attmenu.style.top, (200 - 80 - 8) + "px");

  // image source
  ctx.roots.attmenu.firstListener("click")({
    target: ctx.roots.attmenu.children[0],
  });
  assert.equal(imgClicks.length, 1);
  assert.equal(ctx.roots.attmenu.hidden, true);

  // file source
  ctx.roots.attadd.firstListener("click")({ stopPropagation(){} });
  ctx.roots.attmenu.firstListener("click")({
    target: ctx.roots.attmenu.children[1],
  });
  assert.equal(fileClicks.length, 1);

  // document click / Escape close
  ctx.roots.attadd.firstListener("click")({ stopPropagation(){} });
  assert.equal(ctx.roots.attmenu.hidden, false);
  ctx.doc.firstListener("click")();
  assert.equal(ctx.roots.attmenu.hidden, true);
  ctx.roots.attadd.firstListener("click")({ stopPropagation(){} });
  ctx.doc.firstListener("keydown")({ key: "Escape" });
  assert.equal(ctx.roots.attmenu.hidden, true);
  ctx.feature.destroy();
});

test("stage isolation per node; remove revokes preview", async () => {
  const ctx = makeFeature();
  ctx.feature.bind();
  ctx.feature.setAttachAvail(true);
  ctx.feature.addFiles([{ name: "a.png", type: "image/png" }], "n1");
  await Promise.resolve();
  assert.match(ctx.roots.attstage.innerHTML, /a\.png/);
  assert.ok(ctx.created.length >= 1);

  ctx.setSel("n2");
  ctx.nodes.push({ id: "n2", title: "B" });
  ctx.feature.setAttachAvail(true);
  ctx.feature.addFiles([{ name: "b.pdf", type: "application/pdf" }], "n2");
  await Promise.resolve();
  assert.match(ctx.roots.attstage.innerHTML, /b\.pdf/);
  assert.doesNotMatch(ctx.roots.attstage.innerHTML, /a\.png/);

  ctx.setSel("n1");
  ctx.feature.renderStage();
  assert.match(ctx.roots.attstage.innerHTML, /a\.png|stagechip/);
  const preview = "blob:a.png";
  assert.equal(ctx.roots.attstage._stageButtons.length, 1);
  ctx.roots.attstage._stageButtons[0].firstListener("click")();
  assert.equal(ctx.roots.attstage.hidden, true);
  assert.ok(ctx.revoked.includes(preview));
  ctx.feature.destroy();
});

test("upload success and failure statuses", async () => {
  const ctx = makeFeature();
  ctx.feature.bind();
  ctx.feature.setAttachAvail(true);

  // success default
  ctx.feature.addFiles([{ name: "ok.png", type: "image/png" }]);
  await new Promise(r => setImmediate(r));
  await new Promise(r => setImmediate(r));
  assert.match(ctx.roots.attstage.innerHTML, /stagechip done/);

  // failure
  ctx.setUploadHandler(async () => ({
    ok: false, status: 400, statusText: "Bad",
    text: async () => "too large",
    json: async () => ({}),
  }));
  ctx.feature.addFiles([{ name: "bad.png", type: "image/png" }]);
  await new Promise(r => setImmediate(r));
  await new Promise(r => setImmediate(r));
  assert.match(ctx.roots.attstage.innerHTML, /stagechip err/);
  assert.match(ctx.roots.attstage.innerHTML, /bad\.png/);
  ctx.feature.destroy();
});

test("addFiles gated until attach available", async () => {
  const ctx = makeFeature();
  ctx.feature.bind();
  // canAttach false
  ctx.feature.addFiles([{ name: "x.png", type: "image/png" }]);
  assert.equal(ctx.fetchCalls.length, 0);
  ctx.feature.setAttachAvail(true);
  ctx.feature.addFiles([{ name: "x.png", type: "image/png" }]);
  await Promise.resolve();
  assert.equal(ctx.fetchCalls.length, 1);
  ctx.feature.destroy();
});

test("composer drag/drop stages files", async () => {
  const ctx = makeFeature();
  ctx.feature.bind();
  ctx.feature.setAttachAvail(true);
  const types = ["Files"];
  ctx.roots.promptbar.firstListener("dragover")({
    dataTransfer: { types },
    preventDefault(){},
  });
  assert.equal(ctx.roots.promptbar.classList.contains("drop"), true);
  ctx.roots.promptbar.firstListener("dragleave")();
  assert.equal(ctx.roots.promptbar.classList.contains("drop"), false);

  ctx.roots.promptbar.firstListener("drop")({
    dataTransfer: { files: [{ name: "drop.png", type: "image/png" }] },
    preventDefault(){},
  });
  await Promise.resolve();
  assert.match(ctx.roots.attstage.innerHTML, /drop\.png/);
  ctx.feature.destroy();
});

test("isClaudeNativeForkCommand is leading-command only", () => {
  assert.equal(isClaudeNativeForkCommand("/fork"), true);
  assert.equal(isClaudeNativeForkCommand("  /fork  "), true);
  assert.equal(isClaudeNativeForkCommand("/fork extra"), true);
  assert.equal(isClaudeNativeForkCommand("please use /fork"), false);
  assert.equal(isClaudeNativeForkCommand("/forked"), false);
  assert.equal(isClaudeNativeForkCommand("I ran /fork yesterday"), false);
});

test("composer refuses Claude /fork and keeps the draft", async () => {
  const ctx = makeFeature({
    nodes: [{ id: "n1", title: "A", agent: "claude", ended_at: "" }],
  });
  ctx.feature.bind();
  ctx.roots.prompt.textContent = "/fork";
  await ctx.feature.sendPrompt();
  assert.equal(ctx.apiCalls.length, 0);
  assert.match(ctx.alerts.join(" "), /Fork action/);
  assert.equal(ctx.feature.promptText(), "/fork");
  ctx.feature.destroy();

  const prose = makeFeature({
    nodes: [{ id: "n1", title: "A", agent: "claude", ended_at: "" }],
  });
  prose.feature.bind();
  prose.roots.prompt.textContent = "please use /fork later";
  await prose.feature.sendPrompt();
  assert.equal(prose.apiCalls.length, 1);
  prose.feature.destroy();

  const other = makeFeature({
    nodes: [{ id: "n1", title: "A", agent: "pi", ended_at: "" }],
  });
  other.feature.bind();
  other.roots.prompt.textContent = "/fork";
  await other.feature.sendPrompt();
  assert.equal(other.apiCalls.length, 1, "non-Claude /fork is not blocked in the GUI");
  other.feature.destroy();
});

/* ---------- send / interrupt ---------- */
test("text-only, files-only, combined, and empty sends", async () => {
  // empty
  const empty = makeFeature();
  empty.feature.bind();
  empty.roots.prompt.textContent = "   ";
  await empty.feature.sendPrompt();
  assert.equal(empty.apiCalls.length, 0);

  // text only
  const t = makeFeature();
  t.feature.bind();
  t.roots.prompt.textContent = "hello";
  await t.feature.sendPrompt();
  assert.equal(t.apiCalls.length, 1);
  const body = JSON.parse(t.apiCalls[0].opts.body);
  assert.equal(body.text, "hello");
  assert.deepEqual(body.attachments, []);
  assert.equal(t.storage.getItem("scimux-draft:n1"), null);
  assert.equal(t.feature.promptText(), "");

  // files only
  const f = makeFeature();
  f.feature.bind();
  f.feature.setAttachAvail(true);
  f.feature.addFiles([{ name: "a.png", type: "image/png" }]);
  await Promise.resolve();
  await Promise.resolve();
  f.roots.prompt.textContent = "";
  await f.feature.sendPrompt();
  assert.equal(f.apiCalls.length, 1);
  const b2 = JSON.parse(f.apiCalls[0].opts.body);
  assert.equal(b2.text, "");
  assert.equal(b2.attachments.length, 1);

  // combined
  const c = makeFeature();
  c.feature.bind();
  c.feature.setAttachAvail(true);
  c.feature.addFiles([{ name: "a.png", type: "image/png" }]);
  await Promise.resolve();
  await Promise.resolve();
  c.roots.prompt.textContent = "with file";
  await c.feature.sendPrompt();
  const b3 = JSON.parse(c.apiCalls[0].opts.body);
  assert.equal(b3.text, "with file");
  assert.equal(b3.attachments.length, 1);

  empty.feature.destroy();
  t.feature.destroy();
  f.feature.destroy();
  c.feature.destroy();
});

test("upload wait completion and 12s timeout bound", async () => {
  // hang upload in "up"
  const ctx = makeFeature({
    uploadHandler: () => new Promise(() => { /* never resolves */ }),
  });
  ctx.feature.bind();
  ctx.feature.setAttachAvail(true);
  ctx.feature.addFiles([{ name: "slow.png", type: "image/png" }]);
  // still uploading
  assert.match(ctx.roots.attstage.innerHTML, /stagechip up/);

  const sendP = ctx.feature.sendPrompt();
  // wait loop should have interval + 12s timeout
  assert.ok(ctx.timers.intervalCount() >= 1);
  assert.ok(ctx.timers.hasTimeout(12000));
  // fire timeout → proceeds with no completed refs and empty text → no send
  ctx.roots.prompt.textContent = ""; // ensure empty after wait
  // Actually text was captured at start - empty, refs empty after timeout
  ctx.timers.runTimeouts(12000);
  await sendP;
  // no completed refs, empty text → no api call
  assert.equal(ctx.apiCalls.length, 0);

  // completion path: status flips to done mid-wait
  let finishUpload;
  const ctx2 = makeFeature({
    uploadHandler: () => new Promise(res => { finishUpload = res; }),
  });
  ctx2.feature.bind();
  ctx2.feature.setAttachAvail(true);
  ctx2.feature.addFiles([{ name: "x.png", type: "image/png" }]);
  ctx2.roots.prompt.textContent = "go";
  const p2 = ctx2.feature.sendPrompt();
  // Complete the real upload promise, then let its status/ref update land.
  finishUpload({
    ok: true, status: 200, statusText: "OK",
    text: async () => "",
    json: async () => ({ attachments: [{ path: "done-x" }] }),
  });
  await new Promise(r => setImmediate(r));
  ctx2.timers.runIntervals();
  await p2;
  assert.equal(ctx2.apiCalls.length, 1);
  assert.deepEqual(JSON.parse(ctx2.apiCalls[0].opts.body).attachments, [{ path: "done-x" }]);
  ctx.feature.destroy();
  ctx2.feature.destroy();
});

test("selection change during in-flight send keeps destination", async () => {
  let resolveSend;
  const ctx = makeFeature({
    sendHandler: () => new Promise(res => { resolveSend = res; }),
  });
  ctx.feature.bind();
  ctx.roots.prompt.textContent = "dest capture";
  const p = ctx.feature.sendPrompt();
  assert.equal(typeof resolveSend, "function");
  // switch selection mid-flight
  ctx.setSel("other");
  ctx.nodes.push({ id: "other", title: "O" });
  resolveSend({ delivery: "ok" });
  await p;
  assert.ok(ctx.apiCalls[0].path.includes("/nodes/n1/send"));
  // stage for n1 cleared; other untouched
  ctx.feature.destroy();
});

test("echo is painted before POST (ordering)", async () => {
  const seq = [];
  const roots = makeRoots();
  const storage = makeStorage();
  const timers = makeTimers();
  let resolve;
  const feature = createComposerFeature({
    roots,
    document: { execCommand(){ return true; }, addEventListener(){}, removeEventListener(){} },
    storage,
    sel: () => "n1",
    nodeById: () => ({ id: "n1" }),
    isPhoneTouch: () => false,
    icons: { ICON_SEND: "S", ICON_STOP: "X", ICON_PLUS: "P", ICON_IMAGE: "I", ICON_FILE: "F" },
    api: async () => {
      seq.push("post");
      return new Promise(r => { resolve = () => r({}); });
    },
    setSentEcho: () => seq.push("echo"),
    paintEcho: () => seq.push("paint"),
    getChatSig: () => "s",
    getLastTurns: () => [],
    invalidateChat: () => {},
    scheduleTick: () => {},
    alert: () => {},
    FormData: function(){ this.append = () => {}; },
    URL: { createObjectURL: () => "", revokeObjectURL: () => {} },
    setInterval: timers.setInterval,
    clearInterval: timers.clearInterval,
    setTimeout: timers.setTimeout,
    clearTimeout: timers.clearTimeout,
    now: () => 1,
  });
  feature.bind();
  roots.prompt.textContent = "order";
  const p = feature.sendPrompt();
  assert.deepEqual(seq, ["echo", "paint", "post"]);
  resolve();
  await p;
  feature.destroy();
});

test("success cleanup revokes previews; unconfirmed is success", async () => {
  const ctx = makeFeature({
    sendHandler: async () => ({ delivery: "unconfirmed" }),
  });
  ctx.feature.bind();
  ctx.feature.setAttachAvail(true);
  ctx.feature.addFiles([{ name: "a.png", type: "image/png" }]);
  await Promise.resolve();
  await Promise.resolve();
  const preview = "blob:a.png";
  ctx.roots.prompt.textContent = "hi";
  await ctx.feature.sendPrompt();
  assert.ok(ctx.revoked.includes(preview));
  assert.equal(ctx.alerts.length, 0);
  assert.equal(ctx.storage.getItem("scimux-draft:n1"), null);
  assert.equal(ctx.clears.length, 0); // no echo retract on success
  ctx.feature.destroy();
});

/* A browser can refuse storage at any moment: private mode, a policy change,
 * a full quota. sendPrompt cleared the composer and dropped the saved draft
 * before its try block, so the refusal escaped with the text already gone and
 * no request ever made -- the prompt vanished and nothing happened. */
test("a storage that refuses must not swallow the prompt", async () => {
  const storage = makeStorage({ "scimux-draft:n1": "the only copy" });
  storage.removeItem = () => { throw new Error("SecurityError: access denied"); };
  const ctx = makeFeature({ storage });
  ctx.feature.bind();
  ctx.roots.prompt.textContent = "the only copy";
  await ctx.feature.sendPrompt();
  assert.equal(ctx.apiCalls.length, 1, "the send must still be attempted");
  assert.equal(ctx.apiCalls[0].path, "/api/nodes/n1/send");
  ctx.feature.destroy();
});

/* The recovery path has the same shape as the failure it recovers from: it
 * wrote the draft back before putting the text on screen, so a store that
 * refuses the write took the recovery down with it and the text was lost by
 * the very code that exists to keep it. */
test("a send that fails puts the text back even when the draft cannot be saved", async () => {
  const storage = makeStorage();
  storage.setItem = () => { throw new Error("QuotaExceededError"); };
  const ctx = makeFeature({
    storage,
    sendHandler: async () => { throw new Error("network down"); },
  });
  ctx.feature.bind();
  ctx.roots.prompt.textContent = "worth keeping";
  await ctx.feature.sendPrompt();
  assert.equal(ctx.feature.promptText(), "worth keeping", "the only copy left is the visible one");
  ctx.feature.destroy();
});

// P1d — an unacknowledged send must not look delivered; 409 must show the
// server's message instead of being swallowed.
test("P1d: 200 status unconfirmed is not-delivered and keeps draft", async () => {
  const ctx = makeFeature({
    sendHandler: async () => ({ status: "unconfirmed", text: "please approve" }),
  });
  ctx.feature.bind();
  ctx.roots.prompt.textContent = "please approve";
  await ctx.feature.sendPrompt();
  assert.ok(ctx.alerts.length >= 1, "must alert that send was not confirmed");
  assert.match(String(ctx.alerts[0]), /not delivered|unconfirmed|check the terminal/i);
  assert.equal(ctx.storage.getItem("scimux-draft:n1"), "please approve");
  assert.equal(ctx.feature.promptText(), "please approve");
  assert.ok(ctx.clears.includes("n1"), "echo must retract on unconfirmed");
  ctx.feature.destroy();
});

test("P1d: 409 surfaces server message on send", async () => {
  const ctx = makeFeature({
    sendHandler: async () => {
      const err = new Error("previous send unconfirmed — check the terminal");
      err.status = 409;
      throw err;
    },
  });
  ctx.feature.bind();
  ctx.roots.prompt.textContent = "next";
  await ctx.feature.sendPrompt();
  assert.deepEqual(ctx.alerts, ["previous send unconfirmed — check the terminal"]);
  assert.equal(ctx.storage.getItem("scimux-draft:n1"), "next");
  ctx.feature.destroy();
});

test("P1d: ordinary 200 acknowledged is unchanged success", async () => {
  const ctx = makeFeature({
    sendHandler: async () => ({ status: "acknowledged" }),
  });
  ctx.feature.bind();
  ctx.roots.prompt.textContent = "ok prompt";
  await ctx.feature.sendPrompt();
  assert.equal(ctx.alerts.length, 0);
  assert.equal(ctx.storage.getItem("scimux-draft:n1"), null);
  assert.equal(ctx.clears.length, 0);
  ctx.feature.destroy();
});

test("409 alerts with server message; draft merge and stage restore", async () => {
  // P1d: 409 must surface the server's message (was swallowed pre-P1).
  const ctx = makeFeature({
    sendHandler: async () => {
      const err = new Error("conflict");
      err.status = 409;
      throw err;
    },
  });
  ctx.feature.bind();
  ctx.feature.setAttachAvail(true);
  ctx.feature.addFiles([{ name: "a.png", type: "image/png" }]);
  await Promise.resolve();
  await Promise.resolve();
  ctx.roots.prompt.textContent = "original";
  await ctx.feature.sendPrompt();
  assert.deepEqual(ctx.alerts, ["conflict"]);

  // while "in flight", type newer draft after optimistic clear happens inside send —
  // inject by setting storage after clear via intercepting: use a custom send that sets newer mid-call
  const ctx2 = makeFeature({
    sendHandler: async () => {
      // after optimistic clear, operator typed more
      ctx2.storage.setItem("scimux-draft:n1", "newer typing");
      const err = new Error("conflict");
      err.status = 409;
      throw err;
    },
  });
  ctx2.feature.bind();
  ctx2.feature.setAttachAvail(true);
  ctx2.feature.addFiles([{ name: "a.png", type: "image/png" }]);
  await Promise.resolve();
  await Promise.resolve();
  const p2 = "blob:a.png";
  ctx2.roots.prompt.textContent = "original";
  await ctx2.feature.sendPrompt();
  assert.deepEqual(ctx2.alerts, ["conflict"]);
  assert.equal(ctx2.clears[0], "n1");
  assert.equal(ctx2.storage.getItem("scimux-draft:n1"), "original\n\nnewer typing");
  assert.equal(ctx2.feature.promptText(), "original\n\nnewer typing");
  assert.match(ctx2.roots.attstage.innerHTML, /a\.png/);
  assert.ok(!ctx2.revoked.includes(p2)); // not revoked on failure

  // non-409
  const ctx3 = makeFeature({
    sendHandler: async () => {
      const err = new Error("boom");
      err.status = 500;
      throw err;
    },
  });
  ctx3.feature.bind();
  ctx3.roots.prompt.textContent = "x";
  await ctx3.feature.sendPrompt();
  assert.deepEqual(ctx3.alerts, ["boom"]);
  assert.equal(ctx3.storage.getItem("scimux-draft:n1"), "x");

  // destination-scoped echo retraction
  const ctx4 = makeFeature({
    sendHandler: async () => {
      const err = new Error("c");
      err.status = 409;
      throw err;
    },
  });
  ctx4.feature.bind();
  ctx4.roots.prompt.textContent = "a";
  await ctx4.feature.sendPrompt();
  assert.deepEqual(ctx4.clears, ["n1"]);
  assert.deepEqual(ctx4.alerts, ["c"]);

  ctx.feature.destroy();
  ctx2.feature.destroy();
  ctx3.feature.destroy();
  ctx4.feature.destroy();
});

test("echo retraction only for captured destination", async () => {
  const clears = [];
  let rejectSend;
  const timers = makeTimers();
  const roots = makeRoots();
  const storage = makeStorage();
  let sel = "n1";
  const feature = createComposerFeature({
    roots,
    document: { execCommand(){ return true; }, addEventListener(){}, removeEventListener(){} },
    storage,
    sel: () => sel,
    nodeById: id => ({ id }),
    icons: { ICON_SEND: "S", ICON_STOP: "X", ICON_PLUS: "P", ICON_IMAGE: "I", ICON_FILE: "F" },
    api: async () => {
      // switch selection while in flight
      sel = "n2";
      return new Promise((_, rej) => { rejectSend = rej; });
    },
    setSentEcho: () => {},
    paintEcho: () => {},
    clearSentEchoFor: id => clears.push(id),
    getChatSig: () => "s",
    getLastTurns: () => [],
    invalidateChat: () => {},
    alert: () => {},
    setInterval: timers.setInterval,
    clearInterval: timers.clearInterval,
    setTimeout: timers.setTimeout,
    clearTimeout: timers.clearTimeout,
    FormData: function(){ this.append = () => {}; },
    URL: { createObjectURL: () => "b", revokeObjectURL: () => {} },
    now: () => 1,
  });
  feature.bind();
  roots.prompt.textContent = "msg";
  const p = feature.sendPrompt();
  await new Promise(r => setImmediate(r));
  assert.equal(typeof rejectSend, "function");
  const err = new Error("c");
  err.status = 409;
  rejectSend(err);
  await p;
  assert.deepEqual(clears, ["n1"]); // not n2
  // draft restored under n1 key even though sel is n2
  assert.equal(storage.getItem("scimux-draft:n1"), "msg");
  // prompt not overwritten because sel !== dest
  assert.equal(feature.promptText(), ""); // cleared optimistically, not restored to DOM
  feature.destroy();
});

test("closed node refuses send without API call", async () => {
  const ctx = makeFeature({
    nodes: [{ id: "n1", ended_at: "2026-01-01T00:00:00Z" }],
  });
  ctx.feature.bind();
  ctx.roots.prompt.textContent = "nope";
  await ctx.feature.sendPrompt();
  assert.equal(ctx.apiCalls.length, 0);
  ctx.feature.destroy();
});

test("send button routes to interrupt when busy", async () => {
  const ctx = makeFeature();
  ctx.feature.bind();
  ctx.feature.setComposerBusy(true);
  await ctx.roots.sendbtn.firstListener("click")();
  assert.ok(ctx.apiCalls.some(c => c.path.includes("/interrupt")));
  ctx.feature.destroy();
});

test("interrupt 409 surfaces server message; other errors alert", async () => {
  // P1d: 409 must show the server's message (was swallowed pre-P1).
  const ctx = makeFeature({
    interruptHandler: async () => {
      const err = new Error("no turn");
      err.status = 409;
      throw err;
    },
  });
  ctx.feature.bind();
  await ctx.feature.interruptPrompt();
  assert.deepEqual(ctx.alerts, ["no turn"]);

  const ctx2 = makeFeature({
    interruptHandler: async () => {
      const err = new Error("down");
      err.status = 500;
      throw err;
    },
  });
  ctx2.feature.bind();
  await ctx2.feature.interruptPrompt();
  assert.deepEqual(ctx2.alerts, ["down"]);
  ctx.feature.destroy();
  ctx2.feature.destroy();
});

test("file input change stages and clears value", async () => {
  const ctx = makeFeature();
  ctx.feature.bind();
  ctx.feature.setAttachAvail(true);
  const input = ctx.roots.attimg;
  input.files = [{ name: "from-input.png", type: "image/png" }];
  input.firstListener("change")({ target: input });
  await Promise.resolve();
  assert.match(ctx.roots.attstage.innerHTML, /from-input\.png/);
  assert.equal(input.value, "");
  ctx.feature.destroy();
});

test("resizePrompt applies height cap", () => {
  const ctx = makeFeature();
  ctx.feature.bind();
  ctx.roots.prompt.scrollHeight = 500;
  ctx.feature.resizePrompt();
  assert.equal(ctx.roots.prompt.style.height, "120px");
  ctx.roots.prompt.scrollHeight = 50;
  ctx.feature.resizePrompt();
  assert.equal(ctx.roots.prompt.style.height, "50px");
  ctx.feature.destroy();
});

test("seen watermark null when chatSig empty", async () => {
  const ctx = makeFeature({ chatSig: "", lastTurns: [{}, {}, {}] });
  ctx.feature.bind();
  ctx.roots.prompt.textContent = "x";
  await ctx.feature.sendPrompt();
  assert.equal(ctx.echoes[0].seen, null);

  const ctx2 = makeFeature({ chatSig: "yes", lastTurns: [{}, {}] });
  ctx2.feature.bind();
  ctx2.roots.prompt.textContent = "y";
  await ctx2.feature.sendPrompt();
  assert.equal(ctx2.echoes[0].seen, 2);
  ctx.feature.destroy();
  ctx2.feature.destroy();
});

test("stage restore concatenates when operator staged more during flight", async () => {
  let rejectSend;
  const ctx = makeFeature({
    sendHandler: () => new Promise((_, rej) => { rejectSend = rej; }),
  });
  ctx.feature.bind();
  ctx.feature.setAttachAvail(true);
  ctx.feature.addFiles([{ name: "a.png", type: "image/png" }]);
  await new Promise(r => setImmediate(r));
  await new Promise(r => setImmediate(r));
  assert.match(ctx.roots.attstage.innerHTML, /stagechip done/);
  ctx.roots.prompt.textContent = "t";
  const p = ctx.feature.sendPrompt();
  // allow optimistic clear + POST start
  await new Promise(r => setImmediate(r));
  assert.equal(ctx.roots.attstage.hidden, true);
  // after optimistic clear, stage new file on same node
  ctx.feature.addFiles([{ name: "b.pdf", type: "application/pdf" }]);
  assert.match(ctx.roots.attstage.innerHTML, /b\.pdf/);
  const err = new Error("c");
  err.status = 409;
  rejectSend(err);
  await p;
  // b.pdf (new) then a.png (restored)
  const bAt = ctx.roots.attstage.innerHTML.indexOf("b.pdf");
  const aAt = ctx.roots.attstage.innerHTML.lastIndexOf("a.png");
  assert.ok(bAt >= 0 && aAt > bAt, ctx.roots.attstage.innerHTML);
  ctx.feature.destroy();
});
