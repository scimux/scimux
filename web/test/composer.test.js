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
import { AWAITING_FORWARD_TTL_MS } from "../js/storage.js";
import * as composer from "../js/composer.js";

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
  const failed = stageChipHTML({key:2,name:'bad.txt',status:'err'}, {esc});
  assert.match(failed,/role="status"/);assert.match(failed,/Upload failed/);

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
  if (overrides.recovery) roots.recovery = el("div", {hidden:true});
  if (overrides.controls) {
    roots.destination = el("div", {hidden:true});
    roots.destinationPeople = el("button");
    roots.destinationAgent = el("button");
  }
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
  const uiOps = [];
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
    uiMutate: op => uiOps.push(op),
    setInterval: timers.setInterval,
    clearInterval: timers.clearInterval,
    setTimeout: timers.setTimeout,
    clearTimeout: timers.clearTimeout,
    now: () => now,
    ...overrides.deps,
  });

  return {
    feature, roots, storage, apiCalls, fetchCalls, revoked, created, alerts,
    echoes, paints, clears, uiOps, timers, doc, nodes,
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

test("pending Send-to cancels only after the empty destination is left", () => {
  const key = "scimux-sendto-pending:n1";
  const pending = JSON.stringify([{ node: "source", turnTime: "t", text: "source text" }]);
  const hold = makeFeature({ drafts: { [key]: pending, "scimux-draft:n1": "edited draft" } });
  hold.feature.onSelect();
  hold.setSel("n2");
  hold.feature.onSelect();
  assert.equal(hold.storage.getItem(key), pending, "nonempty draft holds across navigation");

  const cancel = makeFeature({ drafts: { [key]: pending, "scimux-draft:n1": "" } });
  cancel.feature.onSelect();
  assert.equal(cancel.storage.getItem(key), pending, "clear alone still holds while open");
  cancel.setSel("n2");
  cancel.feature.onSelect();
  assert.equal(cancel.storage.getItem(key), null, "empty plus leave cancels");

  const inFlight = makeFeature({ drafts: {
    [key]: pending,
    "scimux-draft:n1": "",
    "scimux-sendto-awaiting:n1": JSON.stringify({ text: "forwarded", afterTurns: 0, at: 1_000_000 }),
  } });
  inFlight.feature.onSelect();
  inFlight.setSel("n2");
  inFlight.feature.onSelect();
  assert.equal(inFlight.storage.getItem(key), pending,
    "a deferred initial prompt holds while its transcript turn is pending");
});

test("successful edited Send-to waits for the real transcript turn; failure keeps it pending", async () => {
  const key = "scimux-sendto-pending:n1";
  const source = { node: "source", uid: "u1", segment: 2, record: 7, text: "source text" };
  const success = makeFeature({ drafts: { [key]: JSON.stringify([source]), "scimux-draft:n1": "forwarded" } });
  success.feature.onSelect();
  success.roots.prompt.innerText = "edited before send";
  await success.feature.sendPrompt();
  assert.deepEqual(JSON.parse(success.storage.getItem(key)), [source]);
  assert.equal(success.uiOps.length, 0);
  assert.deepEqual(JSON.parse(success.storage.getItem("scimux-sendto-awaiting:n1")), {
    text: "edited before send", afterTurns: 1, at: 1_000_000,
  });

  const failed = makeFeature({
    drafts: { [key]: JSON.stringify([source]), "scimux-draft:n1": "forwarded" },
    sendHandler: async () => { throw new Error("offline"); },
  });
  failed.feature.onSelect();
  await failed.feature.sendPrompt();
  assert.equal(failed.uiOps.length, 0);
  assert.deepEqual(JSON.parse(failed.storage.getItem(key)), [source]);
});

test("unconfirmed Send-to remains pending and has no marker", async () => {
  const key = "scimux-sendto-pending:n1";
  const source = { node: "source", turnTime: "t", text: "source" };
  const ctx = makeFeature({
    drafts: { [key]: JSON.stringify([source]), "scimux-draft:n1": "forwarded" },
    sendHandler: async () => ({ status: "unconfirmed" }),
  });
  ctx.feature.onSelect();
  await ctx.feature.sendPrompt();
  assert.equal(ctx.uiOps.length, 0);
  assert.deepEqual(JSON.parse(ctx.storage.getItem(key)), [source]);
});

test("a later send cannot replace a Send-to turn already awaiting confirmation", async () => {
  const key = "scimux-sendto-pending:n1";
  const awaitingKey = "scimux-sendto-awaiting:n1";
  const source = { node: "source", turnTime: "t" };
  const awaiting = JSON.stringify({ text: "first forwarded send", afterTurns: 1, at: 1_000_000 });
  const ctx = makeFeature({ drafts: {
    [key]: JSON.stringify([source]),
    [awaitingKey]: awaiting,
    "scimux-draft:n1": "ordinary follow-up",
  } });
  ctx.feature.onSelect();
  await ctx.feature.sendPrompt();
  assert.equal(ctx.storage.getItem(awaitingKey), awaiting);
});

test("an expired delivery latch never blocks the next Send-to", async () => {
  const key = "scimux-sendto-pending:n1";
  const awaitingKey = "scimux-sendto-awaiting:n1";
  const source = { node: "source", turnTime: "t" };
  const ctx = makeFeature({ drafts: {
    [key]: JSON.stringify([source]),
    [awaitingKey]: JSON.stringify({ text: "never landed", afterTurns: 0, at: 1_000 }),
    "scimux-draft:n1": "forwarded",
  } });
  ctx.setNow(1_000 + AWAITING_FORWARD_TTL_MS + 1);
  ctx.feature.onSelect();
  ctx.roots.prompt.innerText = "sent much later";
  await ctx.feature.sendPrompt();
  assert.deepEqual(JSON.parse(ctx.storage.getItem(awaitingKey)), {
    text: "sent much later", afterTurns: 1, at: 1_000 + AWAITING_FORWARD_TTL_MS + 1,
  });
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

test("paste without readable clipboard text leaves the draft untouched", () => {
  const ctx = makeFeature(); ctx.feature.bind();
  let prevented = false;
  for (const clipboardData of [null, {files:[]}]) {
    ctx.roots.prompt.firstListener("paste")({clipboardData, preventDefault(){ prevented = true; }});
  }
  assert.equal(prevented, false);
  assert.equal(ctx.doc.execCommandCalls.length, 0);
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

test('a retired attachment remove callback cannot alter a replacement stage', async () => {
 const ctx=makeFeature({nodes:[{id:'n1',instance_id:'old'}]});ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);
 ctx.feature.addFiles([{name:'old.png',type:'image/png'}]);await Promise.resolve();
 const remove=ctx.roots.attstage._stageButtons[0].firstListener('click');ctx.nodes.splice(0,1,{id:'n1',instance_id:'new'});
 remove();assert.deepEqual(ctx.revoked,[]);ctx.feature.destroy();
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
  const node = { id: "n1" };
  const roots = makeRoots();
  const storage = makeStorage();
  const timers = makeTimers();
  let resolve;
  const feature = createComposerFeature({
    roots,
    document: { execCommand(){ return true; }, addEventListener(){}, removeEventListener(){} },
    storage,
    sel: () => "n1",
    nodeById: () => node,
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
  const nodes = { n1:{id:"n1"}, n2:{id:"n2"} };
  const feature = createComposerFeature({
    roots,
    document: { execCommand(){ return true; }, addEventListener(){}, removeEventListener(){} },
    storage,
    sel: () => sel,
    nodeById: id => nodes[id],
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

test("received selection cannot send, interrupt, or upload even through forged calls", async () => {
  const ctx = makeFeature({sel:"team/s/o/c/g"});
  ctx.feature.bind();
  ctx.feature.setPromptText("forged prompt");
  ctx.feature.setAttachAvail(true);
  ctx.feature.addFiles([{name:"x.txt",type:"text/plain"}]);
  await ctx.feature.sendPrompt();
  await ctx.feature.interruptPrompt();
  assert.deepEqual(ctx.apiCalls, []);
  assert.deepEqual(ctx.fetchCalls, []);
});

test("People destination sends only comments while agent is busy and preserves newer typing", async () => {
  let finish; const posts=[];
  const ctx=makeFeature({sel:"team/s/o/c/g",deps:{peopleContext:()=>({key:"s/o/c/g",writable:true}),sendPeople:(key,text)=>{posts.push({key,text}); return new Promise(resolve=>{finish=resolve});}}});
  ctx.feature.bind(); ctx.feature.onSelect(); ctx.feature.setComposerBusy(true);
  ctx.feature.setPromptText("hello people");
  const sending=ctx.feature.sendPrompt();
  assert.deepEqual(posts,[{key:"s/o/c/g",text:"hello people"}]);
  assert.equal(ctx.apiCalls.length,0); assert.equal(ctx.fetchCalls.length,0);
  ctx.feature.setPromptText("newer"); ctx.roots.prompt.firstListener("input")();
  finish(); await sending;
  assert.equal(ctx.feature.promptText(),"newer");
  assert.equal(ctx.storage.getItem("scimux-people-draft:team/s/o/c/g"),"newer");
});

test("owner Agent dispatch disarms immediately and retains failed draft", async () => {
  let reject;
  const ctx = makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true})},sendHandler:()=>new Promise((_resolve,fail)=>{reject=fail})});
  ctx.feature.bind(); ctx.feature.onSelect();
  ctx.feature.setPromptText('people'); ctx.roots.prompt.firstListener('input')();
  ctx.feature.setDestination('agent'); ctx.feature.setPromptText('agent'); ctx.roots.prompt.firstListener('input')();
  const pending = ctx.feature.sendPrompt();
  assert.equal(ctx.feature.destination(),'people');
  assert.equal(ctx.feature.promptText(),'people');
  reject(Error('failed')); await pending;
  assert.equal(ctx.storage.getItem('scimux-draft:n1'),'agent');
  assert.equal(ctx.feature.promptText(),'people');
});

test("owner validation retains Agent arm and later success keeps newer Agent draft", async () => {
  let resolve;
  const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true})},sendHandler:()=>new Promise(r=>{resolve=r})});
  ctx.feature.bind(); ctx.feature.onSelect(); ctx.feature.setDestination('agent');
  await ctx.feature.sendPrompt(); assert.equal(ctx.feature.destination(),'agent'); assert.equal(ctx.apiCalls.length,0);
  ctx.feature.setPromptText('first'); ctx.roots.prompt.firstListener('input')();
  const pending=ctx.feature.sendPrompt(); assert.equal(ctx.feature.destination(),'people');
  ctx.feature.setDestination('agent'); ctx.feature.setPromptText('newer'); ctx.roots.prompt.firstListener('input')();
  resolve({delivery:'ok'}); await pending;
  assert.equal(ctx.feature.destination(),'agent'); assert.equal(ctx.feature.promptText(),'newer');
  assert.equal(ctx.storage.getItem('scimux-draft:n1'),'newer');
});

test('unconfirmed owner dispatch reports uncertainty and preserves the Agent draft', async () => {
  const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true})},sendHandler:async()=>({status:'unconfirmed'})});
  ctx.feature.bind(); ctx.feature.onSelect(); ctx.feature.setDestination('agent');
  ctx.feature.setPromptText('review me'); ctx.roots.prompt.firstListener('input')(); await ctx.feature.sendPrompt();
  assert.equal(ctx.feature.destination(),'people'); assert.equal(ctx.storage.getItem('scimux-draft:n1'),'review me');
  assert.match(ctx.alerts[0],/unconfirmed/i); assert.doesNotMatch(ctx.alerts[0],/not delivered/i);
});

test('a shared owner Agent arm still disarms if People availability disappears before dispatch', async () => {
  let context={key:'s/o/c/g',writable:true,owner:true};const ctx=makeFeature({deps:{peopleContext:()=>context}});
  ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setDestination('agent');ctx.feature.setPromptText('owner prompt');ctx.roots.prompt.firstListener('input')();
  context=null;ctx.feature.contextChanged();await ctx.feature.sendPrompt();
  assert.equal(ctx.apiCalls.filter(call=>call.path.split('?')[0].endsWith('/send')).length,1);assert.equal(ctx.feature.destination(),'people');ctx.feature.destroy();
});

test('empty file selections and unconfirmed sends use the fallback refresh path', async () => {
  let ticks=0;const ctx=makeFeature({deps:{getChatSig:null,scheduleTick:null,tick:()=>ticks++},sendHandler:async()=>({status:'unconfirmed'})});
  ctx.feature.bind();ctx.feature.onSelect();ctx.feature.addFiles(null);assert.equal(ctx.fetchCalls.length,0);
  ctx.feature.setPromptText('review');await ctx.feature.sendPrompt();ctx.timers.runTimeouts(400);
  assert.equal(ticks,1);assert.match(ctx.alerts[0],/unconfirmed/i);ctx.feature.destroy();
  const success=makeFeature({deps:{getChatSig:null,scheduleTick:null,tick:()=>ticks++},sendHandler:async()=>({status:'acknowledged'})});
  success.feature.bind();success.feature.onSelect();success.feature.setPromptText('accepted');await success.feature.sendPrompt();success.timers.runTimeouts(400);
  assert.equal(ticks,2);success.feature.destroy();
});

test('owner upload-delayed dispatch preserves newer drafts and only disarms its original visit', async () => {
  let upload,finishSend; const source={uid:'u',record:1};
  const ctx=makeFeature({nodes:[{id:'n1'},{id:'n2'}],deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true}),canPreparePrompt:()=>true},
    uploadHandler:()=>new Promise(resolve=>{upload=resolve}),sendHandler:()=>new Promise(resolve=>{finishSend=resolve})});
  ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('people');ctx.roots.prompt.firstListener('input')();
  ctx.feature.preparePrompt('original',source);ctx.feature.setAttachAvail(true);ctx.feature.addFiles([{name:'first.txt',type:'text/plain'}]);
  const pending=ctx.feature.sendPrompt();
  ctx.feature.setPromptText('new agent');ctx.roots.prompt.firstListener('input')();
  ctx.setSel('n2');ctx.feature.onSelect();ctx.setSel('n1');ctx.feature.onSelect();ctx.feature.setDestination('agent');
  upload({ok:true,json:async()=>({attachments:[{path:'first-ref'}]})});await new Promise(r=>setImmediate(r));ctx.timers.runIntervals();
  await new Promise(r=>setImmediate(r));assert.equal(ctx.feature.destination(),'agent');assert.equal(ctx.feature.promptText(),'new agent');
  finishSend({status:'acknowledged',trace:{uid:'u',segment:0,record:2}});await pending;
  assert.equal(ctx.feature.destination(),'agent');assert.equal(ctx.feature.promptText(),'new agent');
  assert.equal(ctx.storage.getItem('scimux-people-draft:n1'),'people');
  assert.deepEqual(JSON.parse(ctx.storage.getItem('scimux-prompt-flights:n1'))[0].sources,[source]);
  ctx.feature.destroy();
});

test("People file paste gives feedback without disturbing either draft or staged Agent file", async () => {
  const ctx = makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true})}});
  ctx.feature.bind(); ctx.feature.onSelect(); ctx.feature.setDestination('agent');
  ctx.feature.setAttachAvail(true); ctx.feature.addFiles([{name:'agent.txt',type:'text/plain'}]);
  await new Promise(r=>setImmediate(r));
  ctx.feature.setDestination('people'); ctx.feature.setPromptText('people draft'); ctx.roots.prompt.firstListener('input')();
  ctx.roots.prompt.firstListener('paste')({clipboardData:{files:[{name:'no.txt',type:'text/plain'}]},preventDefault(){}});
  assert.match(ctx.alerts.at(-1),/people.*text/i);
  assert.equal(ctx.feature.destination(),'people'); assert.equal(ctx.feature.promptText(),'people draft');
  ctx.feature.setDestination('agent'); assert.match(ctx.roots.attstage.innerHTML,/agent.txt/);
});

test("owner Agent send retains an upload that failed while dispatching text", async () => {
  const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true})},uploadHandler:async()=>{throw Error('offline')}});
  ctx.feature.bind(); ctx.feature.onSelect(); ctx.feature.setDestination('agent'); ctx.feature.setAttachAvail(true);
  ctx.feature.addFiles([{name:'failed.txt',type:'text/plain'}]); await new Promise(r=>setImmediate(r));
  ctx.feature.setPromptText('text'); ctx.roots.prompt.firstListener('input')(); await ctx.feature.sendPrompt();
  ctx.feature.setDestination('agent');
  assert.match(ctx.roots.attstage.innerHTML,/failed.txt/);
  assert.equal(ctx.apiCalls.filter(call=>call.path.split('?')[0].endsWith('/send')).length,1);
});

test("People destination refuses readonly or ended sends and keeps its saved draft", async () => {
  let writable=false; let sends=0;
  const ctx=makeFeature({sel:"team/s/o/c/g",deps:{peopleContext:()=>({key:"s/o/c/g",writable}),sendPeople:()=>{sends++;}}});
  ctx.feature.bind(); ctx.feature.onSelect(); ctx.feature.setPromptText("keep"); ctx.roots.prompt.firstListener("input")();
  await ctx.feature.sendPrompt(); assert.equal(sends,0);
  assert.equal(ctx.storage.getItem("scimux-people-draft:team/s/o/c/g"),"keep");
  writable=true; await ctx.feature.sendPrompt(); assert.equal(sends,1);
  assert.equal(ctx.apiCalls.length,0);
});

test("owner switches People and Agent drafts without exposing attachments to People", async () => {
  let context={key:"s/o/c/g",writable:true,owner:true};
  const ctx=makeFeature({deps:{peopleContext:()=>context,sendPeople:async()=>{}}});
  ctx.storage.setItem("scimux-draft:n1","agent draft");
  ctx.feature.bind(); ctx.feature.onSelect();
  assert.equal(ctx.feature.destination(),"people");
  ctx.feature.setPromptText("people draft"); ctx.roots.prompt.firstListener("input")();
  ctx.feature.setDestination("agent"); assert.equal(ctx.feature.promptText(),"agent draft");
  ctx.feature.setAttachAvail(true); ctx.feature.addFiles([{name:"a.png",type:"image/png"}]);
  ctx.feature.setDestination("people"); assert.equal(ctx.feature.promptText(),"people draft");
  assert.equal(ctx.roots.attstage.hidden,true); assert.equal(ctx.roots.attadd.hidden,true);
  ctx.feature.setDestination("agent"); assert.equal(ctx.roots.attstage.hidden,false);
  assert.equal(ctx.feature.promptText(),"agent draft");
});

test("received chat rejects a forged Agent destination", () => {
  const ctx=makeFeature({sel:"team/s/o/c/g",deps:{peopleContext:()=>({key:"s/o/c/g",writable:true,owner:false})}});
  ctx.feature.bind(); ctx.feature.onSelect(); ctx.feature.setDestination("agent");
  assert.equal(ctx.feature.destination(),"people");
});

test("successful older Agent send does not erase newer Agent typing", async () => {
  let resolve; const ctx=makeFeature({sendHandler:()=>new Promise(r=>{resolve=r})});
  ctx.feature.bind(); ctx.feature.onSelect(); ctx.feature.setPromptText("old"); ctx.roots.prompt.firstListener("input")();
  const sent=ctx.feature.sendPrompt(); ctx.feature.setPromptText("new"); ctx.roots.prompt.firstListener("input")();
  resolve({delivery:"ok"}); await sent;
  assert.equal(ctx.storage.getItem("scimux-draft:n1"),"new");
  assert.equal(ctx.feature.promptText(),"new");
});

test("failed Agent send restores its draft without overwriting People during the request", async () => {
  let reject; const ctx=makeFeature({deps:{peopleContext:()=>({key:"s/o/c/g",writable:true,owner:true})},sendHandler:()=>new Promise((_r,j)=>{reject=j})});
  ctx.feature.bind(); ctx.feature.onSelect(); ctx.feature.setDestination("agent"); ctx.feature.setPromptText("agent"); ctx.roots.prompt.firstListener("input")();
  const sending=ctx.feature.sendPrompt(); ctx.feature.setDestination("people"); ctx.feature.setPromptText("people"); ctx.roots.prompt.firstListener("input")();
  reject(Error("failed")); await sending;
  assert.equal(ctx.feature.promptText(),"people"); assert.equal(ctx.storage.getItem("scimux-draft:n1"),"agent");
});

test("ending sharing keeps People visible and requires an explicit Agent choice", async () => {
  let context={key:"s/o/c/g",writable:true,owner:true}; let sends=0;
  const ctx=makeFeature({controls:true,deps:{peopleContext:()=>context,sendPeople:()=>{sends++}}});
  ctx.storage.setItem("scimux-draft:n1","agent draft"); ctx.feature.bind(); ctx.feature.onSelect();
  ctx.feature.setPromptText("people draft"); ctx.roots.prompt.firstListener("input")();
  context=null; ctx.feature.contextChanged();
  assert.equal(ctx.feature.destination(),"people"); assert.equal(ctx.feature.promptText(),"people draft");
  assert.equal(ctx.roots.destination.hidden,false); assert.equal(ctx.roots.prompt.getAttribute("contenteditable"),"false");
  await ctx.feature.sendPrompt(); assert.equal(sends,0); assert.equal(ctx.apiCalls.length,0);
  ctx.feature.setDestination("agent"); assert.equal(ctx.feature.promptText(),"agent draft");
  assert.equal(ctx.roots.sendbtn.getAttribute("aria-label"),"Send message to agent");
  ctx.feature.setDestination("people"); assert.equal(ctx.feature.promptText(),"people draft");
  assert.equal(ctx.roots.prompt.getAttribute("contenteditable"),"false");
});

test("ended recipient cannot select Agent or route a forged send", async () => {
  let context={key:"s/o/c/g",writable:true,owner:false}; let sends=0;
  const ctx=makeFeature({controls:true,sel:"team/s/o/c/g",deps:{peopleContext:()=>context,sendPeople:()=>{sends++}}});
  ctx.feature.bind(); ctx.feature.onSelect(); ctx.feature.setPromptText("draft"); ctx.roots.prompt.firstListener("input")();
  context=null; ctx.feature.contextChanged(); ctx.feature.setDestination("agent"); await ctx.feature.sendPrompt();
  assert.equal(ctx.feature.destination(),"people"); assert.equal(ctx.feature.promptText(),"draft");
  assert.equal(ctx.roots.destination.hidden,true); assert.equal(sends,0); assert.equal(ctx.apiCalls.length,0);
});

test("shared owner Send labels describe the chosen destination", () => {
  const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true})}});
  ctx.feature.bind(); ctx.feature.onSelect();
  assert.equal(ctx.roots.sendbtn.getAttribute('aria-label'),'Send message to people');
  ctx.feature.setDestination('agent');
  assert.equal(ctx.roots.sendbtn.getAttribute('aria-label'),'Send message to agent');
});

test("late owner share activation preserves an Agent draft and defaults to People", () => {
  let context=null, allowed=false;
  const ctx=makeFeature({deps:{peopleContext:()=>context,agentAllowed:()=>allowed}});
  ctx.storage.setItem('scimux-draft:n1','prior agent text'); ctx.feature.bind(); ctx.feature.onSelect();
  assert.equal(ctx.roots.prompt.getAttribute('contenteditable'),'false');
  context={key:'s/o/c/g',writable:true,owner:true}; allowed=true; ctx.feature.contextChanged();
  assert.equal(ctx.feature.destination(),'people'); assert.equal(ctx.roots.prompt.getAttribute('contenteditable'),'true');
  ctx.feature.setDestination('agent'); assert.equal(ctx.feature.promptText(),'prior agent text');
});

test("successful Agent send retains a draft written by another feature while awaiting delivery", async () => {
  let resolve; const ctx=makeFeature({sendHandler:()=>new Promise(r=>{resolve=r})});
  ctx.feature.bind(); ctx.feature.onSelect(); ctx.feature.setPromptText('old'); ctx.roots.prompt.firstListener('input')();
  const sent=ctx.feature.sendPrompt(); ctx.storage.setItem('scimux-draft:n1','draft from another feature');
  resolve({delivery:'ok'}); await sent;
  assert.equal(ctx.storage.getItem('scimux-draft:n1'),'draft from another feature');
});

test('local team node supports prompt, interrupt and attachment routes', async () => {
  const ctx=makeFeature({sel:'team',nodes:[{id:'team',title:'Team'}]});
  ctx.feature.bind(); ctx.feature.onSelect(); ctx.feature.setAttachAvail(true);
  ctx.feature.addFiles([{name:'example.txt',type:'text/plain'}]);
  await new Promise(r=>setImmediate(r));
  ctx.feature.setPromptText('hello'); await ctx.feature.sendPrompt();
  await ctx.feature.interruptPrompt();
  assert.ok(ctx.fetchCalls.some(call=>call.path==='/api/nodes/team/attachments'));
  assert.ok(ctx.apiCalls.some(call=>call.path==='/api/nodes/team/send'));
  assert.ok(ctx.apiCalls.some(call=>call.path==='/api/nodes/team/send/interrupt'));
});

test('People success clears only its original unchanged destination after editing another chat', async () => {
  let finish; const ctx=makeFeature({sel:'team/s/o/a/g',deps:{peopleContext:()=>({key:'s/o/a/g',writable:true}),sendPeople:()=>new Promise(r=>{finish=r})}});
  ctx.feature.bind(); ctx.feature.onSelect(); ctx.feature.setPromptText('original'); ctx.roots.prompt.firstListener('input')();
  const request=ctx.feature.sendPrompt(); ctx.setSel('team/s/o/b/g'); ctx.feature.onSelect();
  ctx.feature.setPromptText('different chat'); ctx.roots.prompt.firstListener('input')(); finish(); await request;
  assert.equal(ctx.storage.getItem('scimux-people-draft:team/s/o/a/g'),null);
  assert.equal(ctx.storage.getItem('scimux-people-draft:team/s/o/b/g'),'different chat');
  assert.equal(ctx.feature.promptText(),'different chat');
});

test('explicit Send-to Agent draft stays Agent when shared context arrives later', () => {
  let context=null;
  const ctx=makeFeature({deps:{peopleContext:()=>context}});
  ctx.storage.setItem('scimux-draft:n1','forwarded source');
  ctx.feature.bind(); ctx.feature.onSelect({preferAgent:true});
  context={key:'s/o/c/g',writable:true,owner:true}; ctx.feature.contextChanged();
  assert.equal(ctx.feature.destination(),'agent');
  assert.equal(ctx.feature.promptText(),'forwarded source');
});

test('People and Agent destination guards reject invalid choices and pending owner sends', async () => {
  let context=null, allowed=false, sends=0;
  const ctx=makeFeature({deps:{peopleContext:()=>context,agentAllowed:()=>allowed,sendPeople:()=>{sends++;}}});
  ctx.feature.bind(); ctx.feature.onSelect(); ctx.feature.setDestination('invalid'); ctx.feature.setDestination('people');
  ctx.feature.setPromptText('prompt'); await ctx.feature.sendPrompt(); assert.equal(ctx.apiCalls.length,0);
  context={key:'s/o/c/g',writable:true,owner:true}; allowed=true; ctx.feature.contextChanged(); ctx.feature.setDestination('people');
  ctx.feature.setPromptText('  '); await ctx.feature.sendPrompt(); ctx.feature.setPromptText('x'.repeat(16385)); await ctx.feature.sendPrompt(); assert.equal(sends,0);
});

test('People send failure preserves the draft and reports an error', async () => {
  const ctx=makeFeature({sel:'team/s/o/c/g',deps:{peopleContext:()=>({key:'s/o/c/g',writable:true}),sendPeople:async()=>{throw Error('offline')}}});
  ctx.feature.bind(); ctx.feature.onSelect(); ctx.feature.setPromptText('keep'); ctx.roots.prompt.firstListener('input')(); await ctx.feature.sendPrompt();
  assert.equal(ctx.storage.getItem('scimux-people-draft:team/s/o/c/g'),'keep'); assert.deepEqual(ctx.alerts,['offline']);
});

test('People send accepts no second send while the first is in flight', async () => {
  let resolve, sends=0; const ctx=makeFeature({sel:'team/s/o/c/g',deps:{peopleContext:()=>({key:'s/o/c/g',writable:true}),sendPeople:()=>{sends++; return new Promise(r=>resolve=r)}}});
  ctx.feature.bind(); ctx.feature.onSelect(); ctx.feature.setPromptText('once'); ctx.roots.prompt.firstListener('input')();
  const pending=ctx.feature.sendPrompt(); await ctx.feature.sendPrompt(); assert.equal(sends,1); resolve(); await pending;
});

test('an Agent destination requires an owned local node even with an owner-shaped context', () => {
 const ctx=makeFeature({sel:'missing',deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true})}});
 ctx.feature.bind(); ctx.feature.onSelect(); ctx.feature.setDestination('agent'); assert.equal(ctx.feature.destination(),'people');
});

test('People send handles a draft set by a preparation seam without an input event', async () => {
 let sent; const ctx=makeFeature({sel:'team/s/o/c/g',deps:{peopleContext:()=>({key:'s/o/c/g',writable:true}),sendPeople:async(_key,text)=>{sent=text}}});
 ctx.feature.bind(); ctx.feature.onSelect(); ctx.feature.setPromptText('prepared'); await ctx.feature.sendPrompt();
 assert.equal(sent,'prepared'); assert.equal(ctx.feature.promptText(),'prepared');
});

test('explicit Send-to Agent draft remains Agent when owner context is already known', () => {
 const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true})}});
 ctx.storage.setItem('scimux-draft:n1','forwarded'); ctx.feature.bind(); ctx.feature.onSelect({preferAgent:true});
 assert.equal(ctx.feature.destination(),'agent'); assert.equal(ctx.feature.promptText(),'forwarded');
});

test('permission downgrade keeps People draft and rejects busy sends', async () => {
  let context={key:'s/o/c/g',writable:true,owner:true}; let sends=0;
  const ctx=makeFeature({deps:{peopleContext:()=>context,sendPeople:()=>{sends++}}});
  ctx.feature.bind(); ctx.feature.onSelect(); ctx.feature.setComposerBusy(true);
  ctx.feature.setPromptText('keep'); ctx.roots.prompt.firstListener('input')();
  context={...context,writable:false}; ctx.feature.contextChanged();
  await ctx.feature.sendPrompt();
  assert.equal(ctx.feature.destination(),'people'); assert.equal(ctx.feature.promptText(),'keep');
  assert.equal(ctx.roots.prompt.getAttribute('contenteditable'),'false');
  assert.equal(sends,0); assert.equal(ctx.apiCalls.length,0);
});

test('late People completion after ending and navigation does not change the new chat', async () => {
  let context={key:'s/o/c/g',writable:true,owner:true}, finish;
  const ctx=makeFeature({controls:true,nodes:[{id:'n1'},{id:'n2'}],deps:{peopleContext:()=>context,sendPeople:()=>new Promise(r=>{finish=r})}});
  ctx.feature.bind(); ctx.feature.onSelect(); ctx.feature.setPromptText('old'); ctx.roots.prompt.firstListener('input')();
  const sent=ctx.feature.sendPrompt(); context=null; ctx.feature.contextChanged();
  ctx.feature.setPromptText('newer'); ctx.roots.prompt.firstListener('input')();
  ctx.setSel('n2'); ctx.feature.onSelect(); ctx.feature.setPromptText('other'); ctx.roots.prompt.firstListener('input')();
  finish(); await sent;
  assert.equal(ctx.feature.destination(),'agent'); assert.equal(ctx.feature.promptText(),'other');
  assert.equal(ctx.storage.getItem('scimux-people-draft:n1'),'newer');
  assert.equal(ctx.roots.destination.hidden,true); assert.equal(ctx.apiCalls.length,0);
});

test('owner prepares a shared source into the Agent draft without dispatch and clears its marker only on erasure', () => {
  let context={key:'s/o/c/g',writable:true,owner:true};
  const ctx=makeFeature({deps:{peopleContext:()=>context,canPreparePrompt:()=>true}});
  ctx.storage.setItem('scimux-draft:n1','earlier'); ctx.feature.bind(); ctx.feature.onSelect();
  ctx.feature.setPromptText('people text'); ctx.roots.prompt.firstListener('input')();
  const source={uid:'u',segment:0,record:1};
  assert.equal(ctx.feature.preparePrompt('quoted',source),true);
  assert.equal(ctx.feature.destination(),'agent'); assert.equal(ctx.feature.promptText(),'earlier\n\nquoted');
  assert.equal(ctx.storage.getItem('scimux-people-draft:n1'),'people text');
  assert.equal(ctx.apiCalls.length,0);
  assert.deepEqual(JSON.parse(ctx.storage.getItem('scimux-prompt-sources:n1')),[source]);
  ctx.feature.setPromptText('partly edited'); ctx.roots.prompt.firstListener('input')();
  ctx.feature.setDestination('people'); ctx.feature.setDestination('agent');
  assert.notEqual(ctx.storage.getItem('scimux-prompt-sources:n1'),null);
  ctx.feature.setPromptText(''); ctx.roots.prompt.firstListener('input')();
  ctx.feature.setPromptText('unrelated'); ctx.roots.prompt.firstListener('input')();
  assert.equal(ctx.storage.getItem('scimux-prompt-sources:n1'),null);
});

test('forged prompt preparation from a received chat or without owner capability does nothing', () => {
  const source={uid:'u',segment:0,record:1};
  for (const [id,allowed] of [['team/s/o/c/g',true],['n1',false]]) {
    const ctx=makeFeature({sel:id,deps:{peopleContext:()=>({key:'s/o/c/g',owner:false,writable:true}),canPreparePrompt:()=>allowed}});
    ctx.feature.bind(); ctx.feature.onSelect();
    assert.equal(ctx.feature.preparePrompt('text',source),false);
    assert.equal(ctx.apiCalls.length,0); assert.equal(ctx.storage.getItem('scimux-prompt-sources:'+id),null);
  }
});

test('in-flight prompt sources are isolated from a newer prepared draft and restored on failure', async () => {
  let reject; const ctx=makeFeature({deps:{canPreparePrompt:()=>true},sendHandler:()=>new Promise((_r,j)=>{reject=j})});
  const a={uid:'u',segment:0,record:1}, b={uid:'u',segment:0,record:2};
  ctx.feature.bind(); ctx.feature.onSelect(); ctx.feature.preparePrompt('first',a);
  const sending=ctx.feature.sendPrompt(); ctx.feature.preparePrompt('second',b);
  assert.deepEqual(JSON.parse(ctx.storage.getItem('scimux-prompt-sources:n1')),[b]);
  reject(Error('no delivery')); await sending;
  assert.deepEqual(JSON.parse(ctx.storage.getItem('scimux-prompt-sources:n1')),[b,a]);
  assert.match(ctx.feature.promptText(),/first/); assert.match(ctx.feature.promptText(),/second/);
});

test('prompt preparation rejects empty text and emits source-change callbacks', async () => {
 let changes=0; const source={uid:'u',segment:0,record:1};
 const ctx=makeFeature({deps:{canPreparePrompt:()=>true,promptSourcesChanged:()=>changes++}});
 ctx.feature.bind(); ctx.feature.onSelect(); assert.equal(ctx.feature.preparePrompt(' ',source),false); assert.equal(ctx.feature.preparePrompt(null,source),false);
 assert.equal(ctx.feature.preparePrompt('quoted',source),true); assert.equal(changes,1);
 ctx.feature.setPromptText(''); ctx.roots.prompt.firstListener('input')(); assert.equal(changes,2);
 ctx.feature.preparePrompt('delivered',source); await ctx.feature.sendPrompt();
 const flight=JSON.parse(ctx.storage.getItem('scimux-prompt-flights:n1'))[0];
 assert.equal(flight.accepted,true); assert.equal(flight.sources[0].uid,'u');
});

test('a successful send leaves another in-flight source snapshot intact', async () => {
 const ctx=makeFeature({deps:{canPreparePrompt:()=>true}}); const source={uid:'u',segment:0,record:1};
 ctx.storage.setItem('scimux-prompt-flights:n1',JSON.stringify([{id:4,text:'prior',sources:[source],accepted:true},{text:'legacy',sources:[source]}]));
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.preparePrompt('new',source);await ctx.feature.sendPrompt();
 const flights=JSON.parse(ctx.storage.getItem('scimux-prompt-flights:n1'));assert.equal(flights.length,3);assert.equal(flights[0].id,4);assert.equal(flights[2].accepted,true);
});

test('asynchronous rejection restores an Agent draft without overwriting People editing', () => {
 const context={key:'s/o/c/g',writable:true,owner:true};
 const ctx=makeFeature({deps:{peopleContext:()=>context}}); ctx.feature.bind();ctx.feature.onSelect();
 ctx.feature.setPromptText('people ongoing');ctx.roots.prompt.firstListener('input')();
 ctx.feature.recoverPromptDraft('n1','agent rejected');
 assert.equal(ctx.feature.promptText(),'people ongoing');assert.equal(ctx.storage.getItem('scimux-draft:n1'),'agent rejected');
 ctx.feature.setDestination('agent');assert.equal(ctx.feature.promptText(),'agent rejected');
});

test('Agent recovery updates the selected Agent composer and failure notifies source changes', async () => {
 let changes=0;const ctx=makeFeature({deps:{canPreparePrompt:()=>true,promptSourcesChanged:()=>changes++},sendHandler:async()=>{throw Error('rejected')}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.recoverPromptDraft('n1','recovered');assert.equal(ctx.feature.promptText(),'recovered');
 ctx.feature.preparePrompt('source',{uid:'u',segment:0,record:1});await ctx.feature.sendPrompt();assert.ok(changes>=3);
});

test('blocked or empty Agent sends preserve prepared source identities', async () => {
 const source={uid:'u',record:1};const ctx=makeFeature({nodes:[{id:'n1',agent:'claude'}],deps:{canPreparePrompt:()=>true}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.preparePrompt('/fork',source);await ctx.feature.sendPrompt();
 assert.equal(ctx.apiCalls.length,0);assert.ok(ctx.storage.getItem('scimux-prompt-sources:n1'));
 ctx.feature.setPromptText('');await ctx.feature.sendPrompt();assert.ok(ctx.storage.getItem('scimux-prompt-sources:n1'));
});

test('Agent draft lookup follows destination and node without changing People text', () => {
 const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',owner:true,writable:true})}});
 ctx.storage.setItem('scimux-draft:n1','saved Agent');
 ctx.storage.setItem('scimux-draft:n2','other Agent');
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('People');
 assert.equal(ctx.feature.agentDraftText('n1'),'saved Agent');
 assert.equal(ctx.feature.agentDraftText('n2'),'other Agent');
 assert.equal(ctx.feature.promptText(),'People');
 ctx.feature.setDestination('agent');ctx.feature.setPromptText('new Agent');
 assert.equal(ctx.feature.agentDraftText('n1'),'new Agent');
 assert.equal(ctx.feature.agentDraftText('n2'),'other Agent');
 ctx.feature.destroy();
});

test('manual Agent send records an exact trace even without prompt sources', async () => {
 const ctx=makeFeature({sendHandler:async()=>({status:'acknowledged',trace:{uid:'u',segment:0,record:4}})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('manual');await ctx.feature.sendPrompt();
 const request=JSON.parse(ctx.apiCalls[0].opts.body);assert.equal(request.trace,true);
 const flights=JSON.parse(ctx.storage.getItem('scimux-prompt-flights:n1'));
 assert.deepEqual(flights[0].address,{uid:'u',segment:0,record:4});assert.deepEqual(flights[0].sources,[]);
 ctx.feature.destroy();
});

test('late Agent recovery preserves blank People and newer Agent editing across chat selection', () => {
 const ctx=makeFeature({nodes:[{id:'n1'},{id:'n2'}],deps:{peopleContext:()=>({key:'s/o/c/g',owner:true,writable:true})}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setDestination('agent');ctx.feature.setPromptText('newer Agent');ctx.roots.prompt.firstListener('input')();
 ctx.feature.setDestination('people');ctx.feature.setPromptText('');ctx.roots.prompt.firstListener('input')();ctx.feature.recoverPromptDraft('n1','failed Agent');
 assert.equal(ctx.feature.destination(),'people');assert.equal(ctx.feature.promptText(),'');assert.equal(ctx.storage.getItem('scimux-draft:n1'),'failed Agent\n\nnewer Agent');
 ctx.setSel('n2');ctx.feature.onSelect();ctx.feature.setPromptText('other People');ctx.feature.recoverPromptDraft('n1','failed Agent');
 assert.equal(ctx.feature.promptText(),'other People');assert.equal(ctx.feature.destination(),'people');assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();
});

test('late Agent error restores captured attachments alongside newer files without painting People', async () => {
 const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',owner:true,writable:true})}});ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setDestination('agent');ctx.feature.setAttachAvail(true);
 ctx.feature.addFiles([{name:'newer.txt',type:'text/plain'}]);await Promise.resolve();await Promise.resolve();
 ctx.feature.setDestination('people');ctx.feature.setPromptText('human text');
 ctx.feature.recoverPromptDraft('n1','failed',[{name:'old.txt',isImage:false,ref:{path:'old-ref',mime:'text/plain'}}]);
 assert.equal(ctx.feature.promptText(),'human text');assert.equal(ctx.feature.destination(),'people');
 ctx.feature.setDestination('agent');assert.match(ctx.roots.attstage.innerHTML,/newer.txt/);assert.match(ctx.roots.attstage.innerHTML,/old.txt/);ctx.feature.destroy();
});

test('traced send snapshots uploaded attachment references for asynchronous recovery', async () => {
 const ctx=makeFeature({sendHandler:async()=>({status:'acknowledged',trace:{uid:'u',segment:0,record:4}})});ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);ctx.feature.addFiles([{name:'input.txt',type:'text/plain'}]);await Promise.resolve();await Promise.resolve();ctx.feature.setPromptText('prompt');await ctx.feature.sendPrompt();
 const flight=JSON.parse(ctx.storage.getItem('scimux-prompt-flights:n1'))[0];assert.equal(flight.attachments[0].name,'input.txt');assert.equal(flight.attachments[0].ref.path,'deadbeef-input.txt');assert.equal(flight.attachments[0].isImage,false);ctx.feature.destroy();
});

test('recovery ignores malformed stored attachment lists', () => {
 const ctx=makeFeature();ctx.feature.bind();ctx.feature.onSelect();ctx.feature.recoverPromptDraft('n1','recover',null);assert.equal(ctx.feature.promptText(),'recover');ctx.feature.destroy();
});

test('a recovered attachment is available after recreating the composer', () => {
 const ctx=makeFeature();ctx.feature.bind();ctx.feature.onSelect();ctx.feature.recoverPromptDraft('n1','restored',[{name:'report.txt',ref:{path:'owned',mime:'text/plain'}}]);
 assert.match(ctx.roots.attstage.innerHTML,/report.txt/);assert.equal(ctx.feature.promptText(),'restored');ctx.feature.destroy();
});

test('an upload-delayed send preserves newer Agent text and staged attachment', async () => {
 let finish;const ctx=makeFeature({uploadHandler:()=>new Promise(resolve=>{finish=resolve})});ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);
 ctx.feature.addFiles([{name:'first.txt',type:'text/plain'}]);ctx.feature.setPromptText('first');ctx.roots.prompt.firstListener('input')();const pending=ctx.feature.sendPrompt();
 ctx.feature.setPromptText('newer');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'newer.txt',type:'text/plain'}]);
 finish({ok:true,json:async()=>({attachments:[{path:'newer-ref'}]})});await new Promise(r=>setImmediate(r));ctx.timers.runTimeouts(12000);await pending;
 assert.equal(ctx.feature.promptText(),'newer');assert.match(ctx.roots.attstage.innerHTML,/newer.txt/);ctx.feature.destroy();
});

test('overlapping tabs cannot attach an old response to a replacement flight', async () => {
 const storage=makeStorage();let finishFirst,finishSecond;
 const a=makeFeature({storage,sendHandler:()=>new Promise(resolve=>{finishFirst=resolve})});
 const b=makeFeature({storage,sendHandler:()=>new Promise(resolve=>{finishSecond=resolve})});
 a.feature.bind();a.feature.onSelect();a.feature.setPromptText('first');const one=a.feature.sendPrompt();
 const first=JSON.parse(storage.getItem('scimux-prompt-flights:n1'))[0];storage.removeItem('scimux-prompt-flights:n1');
 b.feature.bind();b.feature.onSelect();b.feature.setPromptText('second');const two=b.feature.sendPrompt();const second=JSON.parse(storage.getItem('scimux-prompt-flights:n1'))[0];
 assert.notEqual(first.id,second.id);
 finishFirst({status:'acknowledged',trace:{uid:'u',segment:0,record:1}});await one;
 assert.equal(JSON.parse(storage.getItem('scimux-prompt-flights:n1'))[0].accepted,false);
 finishSecond({status:'acknowledged',trace:{uid:'u',segment:0,record:2}});await two;a.feature.destroy();b.feature.destroy();
});
test('coordinator selected source remains associated while send waits for upload', async()=>{
 const source={uid:'u',segment:0,record:1};
 const ctx=makeFeature({uploadHandler:()=>new Promise(()=>{}),deps:{canPreparePrompt:()=>true}});
 ctx.feature.bind();ctx.feature.onSelect();assert.equal(ctx.feature.preparePrompt('selected source',source),true);ctx.feature.setAttachAvail(true);
 ctx.feature.addFiles([{name:'slow.txt',type:'text/plain'}]);const sending=ctx.feature.sendPrompt();
 try{
  const prepared=JSON.parse(ctx.storage.getItem('scimux-prompt-sources:n1')||'[]');
  const inFlight=JSON.parse(ctx.storage.getItem('scimux-prompt-flights:n1')||'[]').flatMap(f=>f.sources);
  assert.equal([...prepared,...inFlight].some(s=>s.uid===source.uid&&s.record===source.record),true,'nonblank selected source cannot disappear from both prepared and in-flight associations during upload wait');
 }finally{ctx.timers.runTimeouts(12000);await sending;ctx.feature.destroy();}
});

test('upload wait reserves one send and keeps T1 sources separate from newer edits and navigation', async () => {
 let finish; const first={uid:'u',record:1}, second={uid:'u',record:2};
 const ctx=makeFeature({uploadHandler:()=>new Promise(resolve=>{finish=resolve}),deps:{canPreparePrompt:()=>true}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.preparePrompt('first',first);ctx.feature.setAttachAvail(true);
 ctx.feature.addFiles([{name:'slow.txt',type:'text/plain'}]);const pending=ctx.feature.sendPrompt();
 await ctx.feature.sendPrompt();assert.equal(ctx.apiCalls.length,0);
 ctx.feature.setPromptText('');ctx.roots.prompt.firstListener('input')();ctx.feature.preparePrompt('second',second);
 ctx.setSel('n2');ctx.feature.onSelect();
 finish({ok:true,json:async()=>({attachments:[{path:'first-ref'}]})});await new Promise(r=>setImmediate(r));ctx.timers.runIntervals();await pending;
 assert.equal(ctx.apiCalls.length,1);assert.deepEqual(JSON.parse(ctx.apiCalls[0].opts.body).attachments,[{path:'first-ref'}]);
 const flights=JSON.parse(ctx.storage.getItem('scimux-prompt-flights:n1'));assert.deepEqual(flights[0].sources,[first]);assert.deepEqual(flights[0].attachments[0].ref,{path:'first-ref'});
 assert.deepEqual(JSON.parse(ctx.storage.getItem('scimux-prompt-sources:n1')),[second]);ctx.setSel('n1');ctx.feature.onSelect();assert.match(ctx.feature.promptText(),/second/);ctx.feature.destroy();
});

test('upload wait releases reservation on empty timeout and blocked Claude fork', async () => {
 const ctx=makeFeature({uploadHandler:()=>new Promise(()=>{}),nodes:[{id:'n1',agent:'claude'}]});ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);
 ctx.feature.addFiles([{name:'slow.txt',type:'text/plain'}]);let pending=ctx.feature.sendPrompt();ctx.timers.runTimeouts(12000);await pending;
 assert.equal(ctx.apiCalls.length,0);assert.equal(ctx.storage.getItem('scimux-prompt-flights:n1'),null);
 ctx.feature.setPromptText('/fork');pending=ctx.feature.sendPrompt();ctx.timers.runTimeouts(12000);await pending;
 assert.equal(ctx.apiCalls.length,0);assert.equal(ctx.storage.getItem('scimux-prompt-flights:n1'),null);
 ctx.feature.setPromptText('ordinary');pending=ctx.feature.sendPrompt();ctx.timers.runTimeouts(12000);await pending;assert.equal(ctx.apiCalls.length,1);ctx.feature.destroy();
});

test('the upload reservation retains the exact pre-send boundary', async () => {
 const ctx=makeFeature({lastTurns:[{uid:'log',segment:2,record:7,role:'assistant'}],uploadHandler:()=>new Promise(()=>{})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);ctx.feature.addFiles([{name:'slow.txt',type:'text/plain'}]);ctx.feature.setPromptText('intent');
 const pending=ctx.feature.sendPrompt();const flight=JSON.parse(ctx.storage.getItem('scimux-prompt-flights:n1'))[0];assert.deepEqual(flight.boundary,{uid:'log',segment:2,record:7});
 ctx.timers.runTimeouts(12000);await pending;ctx.feature.destroy();
});

test('failed upload releases the captured send without losing its selected source', async () => {
 let fail;const source={uid:'u',record:3};const ctx=makeFeature({uploadHandler:()=>new Promise((_,reject)=>{fail=reject}),deps:{canPreparePrompt:()=>true}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.preparePrompt('selected',source);ctx.feature.setAttachAvail(true);ctx.feature.addFiles([{name:'bad.txt',type:'text/plain'}]);
 const pending=ctx.feature.sendPrompt();assert.equal(ctx.apiCalls.length,0);fail(new Error('upload failed'));await new Promise(r=>setImmediate(r));ctx.timers.runIntervals();await pending;
 assert.equal(ctx.apiCalls.length,1);assert.deepEqual(JSON.parse(ctx.apiCalls[0].opts.body).attachments,[]);assert.deepEqual(JSON.parse(ctx.storage.getItem('scimux-prompt-flights:n1'))[0].sources,[source]);ctx.feature.destroy();
});

test('a first prompt has no pre-send boundary', async () => {
 const ctx=makeFeature({lastTurns:[]});ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('first');await ctx.feature.sendPrompt();
 assert.equal(JSON.parse(ctx.storage.getItem('scimux-prompt-flights:n1'))[0].boundary,null);ctx.feature.destroy();
});

test('a composer without a transcript reader still captures a first prompt', async () => {
 const ctx=makeFeature({deps:{getLastTurns:undefined}});ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('first');await ctx.feature.sendPrompt();
 assert.equal(JSON.parse(ctx.storage.getItem('scimux-prompt-flights:n1'))[0].boundary,null);ctx.feature.destroy();
});


test('coordinator: a recreated local node does not inherit the old node attachment stage', async () => {
 const ctx=makeFeature({nodes:[{id:'n1',title:'Old synthetic chat',created_at:'2026-01-01T00:00:00Z'}]});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);
 ctx.feature.addFiles([{name:'old-synthetic.png',type:'image/png'}]);
 await Promise.resolve();await Promise.resolve();
 assert.match(ctx.roots.attstage.innerHTML,/old-synthetic/);
 ctx.nodes.splice(0);ctx.setSel('');ctx.feature.onSelect();
 ctx.nodes.push({id:'n1',title:'Fresh synthetic chat',created_at:'2026-01-02T00:00:00Z'});
 ctx.setSel('n1');ctx.feature.onSelect();ctx.feature.setAttachAvail(true);
 assert.doesNotMatch(ctx.roots.attstage.innerHTML,/old-synthetic/);
 ctx.feature.destroy();
});


test('coordinator: an old upload wait cannot dispatch into a recreated owner chat', async () => {
 let finishUpload;let generation='old';
 const ctx=makeFeature({controls:true,nodes:[{id:'n1',title:'Synthetic old',created_at:'2026-01-01T00:00:00Z'}],
  uploadHandler:()=>new Promise(resolve=>{finishUpload=resolve}),
  deps:{peopleContext:()=>({key:'s/o/c/'+generation,owner:true,writable:true})}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setDestination('agent');ctx.feature.setAttachAvail(true);
 ctx.feature.addFiles([{name:'old-synthetic.txt',type:'text/plain'}]);ctx.roots.prompt.textContent='Synthetic old prompt';
 const pending=ctx.feature.sendPrompt();assert.ok(ctx.timers.intervalCount()>0);
 ctx.nodes.splice(0);ctx.setSel('');ctx.feature.onSelect();generation='replacement';
 ctx.nodes.push({id:'n1',title:'Synthetic replacement',created_at:'2026-01-02T00:00:00Z'});
 ctx.setSel('n1');ctx.feature.onSelect();
 finishUpload({ok:true,status:200,statusText:'OK',text:async()=>'',json:async()=>({attachments:[{path:'synthetic-old-ref'}]})});
 await new Promise(resolve=>setImmediate(resolve));ctx.timers.runIntervals();await pending;
 assert.deepEqual(ctx.apiCalls.filter(c=>c.path.split('?')[0].endsWith('/send')),[],'old pending input must never be sent to the replacement agent');
 ctx.feature.destroy();
});

test('a recreated chat with the same timestamp cannot inherit a staged file', async () => {
 const ctx=makeFeature({nodes:[{id:'n1',instance_id:'old',created_at:'same'}]});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);ctx.feature.addFiles([{name:'old.txt',type:'text/plain'}]);
 await Promise.resolve();await Promise.resolve();ctx.nodes.splice(0);ctx.setSel('');ctx.feature.onSelect();
 ctx.nodes.push({id:'n1',instance_id:'new',created_at:'same'});ctx.setSel('n1');ctx.feature.onSelect();ctx.feature.setAttachAvail(true);
 assert.doesNotMatch(ctx.roots.attstage.innerHTML,/old.txt/);ctx.feature.destroy();
});

test('a recreated chat does not inherit either destination draft or pending sources', () => {
 const ctx=makeFeature({controls:true,nodes:[{id:'n1',instance_id:'old'}],deps:{peopleContext:()=>({key:'s/o/c/g',owner:true,writable:true}),canPreparePrompt:()=>true}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('old people');ctx.roots.prompt.firstListener('input')();
 ctx.feature.setDestination('agent');ctx.feature.preparePrompt('old agent',{uid:'u',record:1});
 ctx.nodes.splice(0);ctx.setSel('');ctx.feature.onSelect();ctx.nodes.push({id:'n1',instance_id:'new'});ctx.setSel('n1');ctx.feature.onSelect();
 assert.equal(ctx.feature.promptText(),'');ctx.feature.setDestination('agent');assert.equal(ctx.feature.promptText(),'');
 assert.deepEqual(JSON.parse(ctx.storage.getItem('scimux-prompt-sources:n1')||'[]'),[]);ctx.feature.destroy();
});

test('same-selection replacement cannot send the retired visible draft', async () => {
 const ctx=makeFeature({nodes:[{id:'n1',instance_id:'old'}]});ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('retired draft');ctx.roots.prompt.firstListener('input')();
 ctx.nodes.splice(0,1,{id:'n1',instance_id:'new'});await ctx.feature.sendPrompt();
 assert.equal(ctx.apiCalls.filter(c=>c.path.split('?')[0].endsWith('/send')).length,0);assert.doesNotMatch(ctx.feature.promptText(),/retired draft/);ctx.feature.destroy();
});

test('a corrupt old archive blocks drafting operations without dropping visible text', async () => {
 const storage=makeStorage({'scimux-draft-owner:n1':'old','scimux-draft:n1':'saved','scimux-retired-drafts:n1:old':'{}'});
 const ctx=makeFeature({storage,controls:true,nodes:[{id:'n1',instance_id:'new'}],deps:{peopleContext:()=>({key:'s/o/c/g',owner:true,writable:true}),canPreparePrompt:()=>true}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('copy me');
 ctx.feature.setDestination('agent');ctx.feature.setAttachAvail(true);ctx.feature.addFiles([{name:'a.txt',type:'text/plain'}]);
 assert.equal(ctx.feature.preparePrompt('source',{uid:'u',record:1}),false);ctx.roots.prompt.firstListener('input')();await ctx.feature.sendPrompt();
 assert.equal(ctx.feature.promptText(),'copy me');assert.equal(ctx.apiCalls.length,0);assert.match(ctx.alerts.join(' '),/storage is unavailable/i);ctx.feature.destroy();
});

test('a replacement cannot retire visible text when its recovery archive is damaged', async () => {
 const ctx=makeFeature({nodes:[{id:'n1',instance_id:'old'}]});ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('visible text');
 ctx.storage.setItem('scimux-retired-index:n1','{}');ctx.nodes.splice(0,1,{id:'n1',instance_id:'new'});
 await ctx.feature.sendPrompt();ctx.feature.addFiles([{name:'x.txt',type:'text/plain'}]);ctx.feature.setDestination('agent');
 assert.equal(ctx.feature.preparePrompt('source',{uid:'u',record:1}),false);ctx.roots.prompt.firstListener('input')();
 assert.equal(ctx.feature.promptText(),'visible text');assert.equal(ctx.apiCalls.length,0);assert.match(ctx.alerts.join(' '),/Copy the visible draft/);ctx.feature.destroy();
});

test('interrupt binds its request to the actual selected chat identity', async () => {
 const ctx=makeFeature({nodes:[{id:'n1',instance_id:'old'}]});ctx.feature.bind();ctx.feature.onSelect();
 await ctx.feature.interruptPrompt();
 const call=ctx.apiCalls.find(c=>c.path==='/api/nodes/n1/send/interrupt?chat_instance=old');
 assert.equal(call.opts.headers['X-Scimux-Chat-Instance'],'old');ctx.feature.destroy();
});

test('a server without chat identity cannot receive an attachment or interrupt from the current browser', async () => {
 const ctx=makeFeature({nodes:[{id:'n1'}],deps:{requireChatIdentity:true}});ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);
 ctx.feature.addFiles([{name:'file.txt',type:'text/plain'}]);await ctx.feature.interruptPrompt();
 assert.equal(ctx.fetchCalls.length,0);assert.equal(ctx.apiCalls.length,0);assert.equal(ctx.alerts.length,2);assert.match(ctx.alerts.join(' '),/identity is unavailable/i);ctx.feature.destroy();
});

test('attachment staging reports unavailable browser upload facilities', async () => {
 const original=globalThis.FormData;
 try {
  globalThis.FormData=undefined;
  const ctx=makeFeature({deps:{FormData:undefined}});ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);
  ctx.feature.addFiles([{name:'file.txt',type:'text/plain'}]);await Promise.resolve();
  assert.match(ctx.roots.attstage.innerHTML,/FormData unavailable/);ctx.feature.destroy();
 } finally {globalThis.FormData=original;}
});

test('attachment staging retains files when upload fetch is unavailable or rejects', async () => {
 const original=globalThis.fetch;
 try {
  globalThis.fetch=undefined;
  const ctx=makeFeature({deps:{fetchImpl:undefined,withCsrf:undefined}});ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);
  ctx.feature.addFiles([{name:'first.txt',type:'text/plain'}]);await Promise.resolve();
  assert.match(ctx.roots.attstage.innerHTML,/fetch unavailable/);ctx.feature.destroy();
 } finally {globalThis.fetch=original;}
 const failed=makeFeature({uploadHandler:async()=>({ok:false,statusText:'synthetic rejection',text:async()=>''}),deps:{withCsrf:undefined}});
 failed.feature.bind();failed.feature.onSelect();failed.feature.setAttachAvail(true);failed.feature.addFiles([{name:'second.txt',type:'text/plain'}]);
 await new Promise(resolve=>setImmediate(resolve));assert.match(failed.roots.attstage.innerHTML,/synthetic rejection/);failed.feature.destroy();
});

test('attachment staging uses the browser fetch when no adapter is supplied', async () => {
 const original=globalThis.fetch;let requests=0;
 try {
  globalThis.fetch=async()=>{requests++;return {ok:true,json:async()=>({attachments:[{path:'synthetic-ref'}]})}};
  const ctx=makeFeature({deps:{fetchImpl:undefined,withCsrf:undefined}});ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);
  ctx.feature.addFiles([{name:'file.txt',type:'text/plain'}]);await new Promise(resolve=>setImmediate(resolve));
  assert.equal(requests,1);assert.match(ctx.roots.attstage.innerHTML,/stagechip done/);ctx.feature.destroy();
 } finally {globalThis.fetch=original;}
});

test('a late interrupt response does not refresh a replacement chat', async () => {
 let finish;let refreshes=0;
 const ctx=makeFeature({nodes:[{id:'n1',instance_id:'old'}],interruptHandler:()=>new Promise(resolve=>{finish=resolve}),deps:{invalidateChat:()=>refreshes++}});
 ctx.feature.bind();ctx.feature.onSelect();const pending=ctx.feature.interruptPrompt();
 ctx.nodes.splice(0,1,{id:'n1',instance_id:'new'});finish({});await pending;
 assert.equal(refreshes,0);ctx.feature.destroy();
});

test('interrupt refuses a replaced visible chat before dispatch', async () => {
 const ctx=makeFeature({nodes:[{id:'n1',instance_id:'old'}]});ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('keep');
 ctx.nodes.splice(0,1,{id:'n1',instance_id:'new'});await ctx.feature.interruptPrompt();
 assert.equal(ctx.apiCalls.length,0);assert.equal(ctx.feature.promptText(),'');assert.match(ctx.alerts.join(' '),/replaced/i);ctx.feature.destroy();
});

test('a missing current People context never dispatches', async () => {
 let context={key:'s/o/c/g',owner:true,writable:true};const sent=[];
 const ctx=makeFeature({controls:true,nodes:[{id:'n1'}],deps:{requireChatIdentity:true,peopleContext:()=>context,sendPeople:(...a)=>sent.push(a)}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('keep');context=null;await ctx.feature.sendPrompt();
 assert.deepEqual(sent,[]);assert.equal(ctx.feature.promptText(),'keep');ctx.feature.destroy();
});

test('a late interrupt failure does not alert or refresh a different visit', async () => {
 let reject;let refreshes=0;
 const ctx=makeFeature({nodes:[{id:'n1',instance_id:'old'},{id:'n2',instance_id:'other'}],interruptHandler:()=>new Promise((_,fail)=>{reject=fail}),deps:{invalidateChat:()=>refreshes++,refreshChat:()=>refreshes++}});
 ctx.feature.bind();ctx.feature.onSelect();const pending=ctx.feature.interruptPrompt();ctx.setSel('n2');ctx.feature.onSelect();reject(new Error('old failure'));await pending;
 assert.deepEqual(ctx.alerts,[]);assert.equal(refreshes,0);ctx.feature.destroy();
});

test('late failed send cannot restore an old draft into a recreated chat', async () => {
 let rejectSend;
 const ctx=makeFeature({nodes:[{id:'n1',instance_id:'old'}],sendHandler:()=>new Promise((_,reject)=>{rejectSend=reject})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('old text');const pending=ctx.feature.sendPrompt();
 ctx.nodes.splice(0);ctx.setSel('');ctx.feature.onSelect();ctx.nodes.push({id:'n1',instance_id:'new'});ctx.setSel('n1');ctx.feature.onSelect();ctx.feature.setPromptText('new draft');
 rejectSend(new Error('offline'));await pending;assert.equal(ctx.feature.promptText(),'new draft');ctx.feature.destroy();
});

test('an accepted send retired after replacement remains labelled accepted for review', async () => {
 let finish;const ctx=makeFeature({recovery:true,nodes:[{id:'n1',instance_id:'old'}],sendHandler:()=>new Promise(resolve=>{finish=resolve})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('old accepted');const pending=ctx.feature.sendPrompt();
 ctx.nodes.splice(0);ctx.setSel('');ctx.feature.onSelect();ctx.nodes.push({id:'n1',instance_id:'new'});ctx.setSel('n1');ctx.feature.onSelect();
 finish({status:'acknowledged',trace:{uid:'u',record:1}});await pending;
 const saved=JSON.parse(ctx.storage.getItem('scimux-retired-drafts:n1:old')).find(item=>item['scimux-draft:']==='old accepted');
 assert.equal(saved['scimux-delivery-status:'],'accepted');assert.deepEqual(JSON.parse(saved['scimux-delivery-trace:']),{uid:'u',record:1});assert.match(ctx.roots.recovery.innerHTML,/accepted/i);ctx.feature.destroy();
});

test('an uncertain retired send stays labelled unconfirmed', async () => {
 let finish;const ctx=makeFeature({recovery:true,nodes:[{id:'n1',instance_id:'old'}],sendHandler:()=>new Promise(resolve=>{finish=resolve})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('uncertain');const pending=ctx.feature.sendPrompt();
 ctx.nodes.splice(0);ctx.setSel('');ctx.feature.onSelect();ctx.nodes.push({id:'n1',instance_id:'new'});ctx.setSel('n1');ctx.feature.onSelect();
 finish({status:'unconfirmed'});await pending;
 const saved=JSON.parse(ctx.storage.getItem('scimux-retired-drafts:n1:old')).find(item=>item['scimux-draft:']==='uncertain');
 assert.equal(saved['scimux-delivery-status:'],'unconfirmed');assert.match(ctx.roots.recovery.innerHTML,/unconfirmed/i);ctx.feature.destroy();
});

test('a retired upload wait does not block an explicit send in the replacement chat', async () => {
 let finish;
 const ctx=makeFeature({nodes:[{id:'n1',instance_id:'old'}],uploadHandler:()=>new Promise(resolve=>{finish=resolve})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);ctx.feature.addFiles([{name:'old.txt',type:'text/plain'}]);ctx.feature.setPromptText('old');const pending=ctx.feature.sendPrompt();
 ctx.nodes.splice(0);ctx.setSel('');ctx.feature.onSelect();ctx.nodes.push({id:'n1',instance_id:'new'});ctx.setSel('n1');ctx.feature.onSelect();ctx.feature.setPromptText('new');await ctx.feature.sendPrompt();
 assert.equal(ctx.apiCalls.filter(c=>c.path==='/api/nodes/n1/send?chat_instance=new').length,1);assert.equal(JSON.parse(ctx.apiCalls[0].opts.body).text,'new');
 finish({ok:true,json:async()=>({attachments:[{path:'old-ref'}]})});await new Promise(r=>setImmediate(r));ctx.timers.runIntervals();await pending;
 assert.equal(ctx.apiCalls.filter(c=>c.path==='/api/nodes/n1/send?chat_instance=new').length,1);
 const retired=JSON.parse(ctx.storage.getItem('scimux-retired-drafts:n1:old')).find(item=>item['scimux-draft:']==='old');
 assert.equal(retired['scimux-delivery-status:'],'not-dispatched');ctx.feature.destroy();
});

test('an old People publication cannot clear an identical draft in a replacement chat', async () => {
 let finish;let generation='old';
 const ctx=makeFeature({controls:true,nodes:[{id:'n1',instance_id:'old'}],deps:{peopleContext:()=>({key:'s/o/c/'+generation,owner:true,writable:true}),sendPeople:()=>new Promise(resolve=>{finish=resolve})}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('same text');ctx.roots.prompt.firstListener('input')();
 const sending=ctx.feature.sendPrompt();ctx.nodes.splice(0);ctx.setSel('');ctx.feature.onSelect();
 generation='new';ctx.nodes.push({id:'n1',instance_id:'new'});ctx.setSel('n1');ctx.feature.onSelect();
 ctx.storage.setItem('scimux-people-draft:n1','same text');ctx.feature.setPromptText('same text');
 finish();await sending;assert.equal(ctx.feature.promptText(),'same text');assert.equal(ctx.storage.getItem('scimux-people-draft:n1'),'same text');ctx.feature.destroy();
});

test('an old People publication does not block a replacement chat publication', async () => {
 let finish;let generation='old';const sent=[];
 const ctx=makeFeature({controls:true,nodes:[{id:'n1',instance_id:'old'}],deps:{peopleContext:()=>({key:'s/o/c/'+generation,owner:true,writable:true}),sendPeople:(key,text)=>{sent.push([key,text]);return key.endsWith('/old')?new Promise(resolve=>{finish=resolve}):Promise.resolve()}}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('old');const first=ctx.feature.sendPrompt();
 ctx.nodes.splice(0);ctx.setSel('');ctx.feature.onSelect();generation='new';ctx.nodes.push({id:'n1',instance_id:'new'});ctx.setSel('n1');ctx.feature.onSelect();ctx.feature.setPromptText('new');
 await ctx.feature.sendPrompt();assert.deepEqual(sent,[['s/o/c/old','old'],['s/o/c/new','new']]);finish();await first;ctx.feature.destroy();
});

test('separate retired in-flight sends retain both texts and source associations', async () => {
 const storage=makeStorage(), old={id:'n1',instance_id:'old'}, replacement={id:'n1',instance_id:'new'};
 let rejectA, rejectB;
 const a=makeFeature({storage,nodes:[old],sendHandler:()=>new Promise((_,reject)=>{rejectA=reject}),deps:{canPreparePrompt:()=>true}});
 const b=makeFeature({storage,nodes:[old],sendHandler:()=>new Promise((_,reject)=>{rejectB=reject}),deps:{canPreparePrompt:()=>true}});
 a.feature.bind();b.feature.bind();a.feature.onSelect();b.feature.onSelect();
 a.feature.preparePrompt('first',{uid:'a',record:1});const first=a.feature.sendPrompt();
 b.feature.preparePrompt('second',{uid:'b',record:2});const second=b.feature.sendPrompt();
 a.nodes.splice(0,1,replacement);b.nodes.splice(0,1,replacement);
 rejectA(new Error('offline'));await first;rejectB(new Error('offline'));await second;
 const saved=JSON.parse(storage.getItem('scimux-retired-flight:n1\0old'));
 assert.deepEqual(saved.map(x=>x.text),['first','second']);
 assert.deepEqual(saved.map(x=>x.sources[0].uid),['a','b']);
 const review=JSON.parse(storage.getItem('scimux-retired-drafts:n1:old'));
 assert.deepEqual(review.map(x=>x['scimux-draft:']),['first','second']);
 assert.deepEqual(review.map(x=>JSON.parse(x['scimux-prompt-sources:'])[0].uid),['a','b']);
 a.feature.destroy();b.feature.destroy();
});

test('retired sends preserve a pre-existing older recovery record', async () => {
 let rejectSend;const storage=makeStorage({'scimux-retired-flight:n1\0old':JSON.stringify({text:'previous',sources:[],attachments:[]})});
 const ctx=makeFeature({storage,nodes:[{id:'n1',instance_id:'old'}],sendHandler:()=>new Promise((_,reject)=>{rejectSend=reject})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('later');const pending=ctx.feature.sendPrompt();
 ctx.nodes.splice(0,1,{id:'n1',instance_id:'new'});rejectSend(new Error('offline'));await pending;
 assert.deepEqual(JSON.parse(storage.getItem('scimux-retired-flight:n1\0old')).map(item=>item.text),['previous','later']);ctx.feature.destroy();
});

test('a damaged older flight record does not prevent recovery of a newer retired send', async () => {
 let rejectSend;const storage=makeStorage({'scimux-retired-flight:n1\0old':'damaged'});
 const ctx=makeFeature({storage,nodes:[{id:'n1',instance_id:'old'}],sendHandler:()=>new Promise((_,reject)=>{rejectSend=reject})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('recover this');const pending=ctx.feature.sendPrompt();
 ctx.nodes.splice(0,1,{id:'n1',instance_id:'new'});rejectSend(new Error('offline'));await pending;
 assert.equal(storage.getItem('scimux-retired-flight:n1\0old'),'damaged');
 assert.equal(JSON.parse(storage.getItem('scimux-retired-drafts:n1:old'))[0]['scimux-draft:'],'recover this');ctx.feature.destroy();
});

test('a failed recovery archive leaves retired send text reviewable in the current tab', async () => {
 let rejectSend;const storage=makeStorage(),ctx=makeFeature({storage,recovery:true,nodes:[{id:'n1',instance_id:'old'}],sendHandler:()=>new Promise((_,reject)=>{rejectSend=reject})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('recover in memory');const pending=ctx.feature.sendPrompt();
 ctx.nodes.splice(0);ctx.setSel('');ctx.feature.onSelect();ctx.nodes.push({id:'n1',instance_id:'new'});ctx.setSel('n1');ctx.feature.onSelect();
 storage.setItem('scimux-retired-index:n1','{}');rejectSend(new Error('offline'));await pending;
 assert.equal(ctx.roots.recovery.hidden,false);assert.match(ctx.roots.recovery.innerHTML,/recover in memory/);ctx.feature.destroy();
});

test('replacement preserves visible drafts retired by two tabs separately', () => {
 const storage=makeStorage(), old={id:'n1',instance_id:'old'}, replacement={id:'n1',instance_id:'new'};
 const a=makeFeature({storage,nodes:[old]}), b=makeFeature({storage,nodes:[old]});
 a.feature.bind();b.feature.bind();a.feature.onSelect();b.feature.onSelect();
 a.feature.setPromptText('first visible');b.feature.setPromptText('second visible');
 a.nodes.splice(0,1,replacement);b.nodes.splice(0,1,replacement);
 a.feature.reconcileIdentity();b.feature.reconcileIdentity();
 assert.deepEqual(JSON.parse(storage.getItem('scimux-retired-visible:n1\0old')),['first visible','second visible']);
 assert.deepEqual(JSON.parse(storage.getItem('scimux-retired-drafts:n1:old')).map(x=>x['scimux-draft:']),['first visible','second visible']);
 a.feature.destroy();b.feature.destroy();
});

test('a visible People draft is archived under its own destination on same-selection replacement', () => {
 const ctx=makeFeature({controls:true,nodes:[{id:'n1',instance_id:'old'}],deps:{peopleContext:()=>({key:'s/o/c/g',owner:true,writable:true})}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('people draft');ctx.nodes.splice(0,1,{id:'n1',instance_id:'new'});
 assert.equal(ctx.feature.reconcileIdentity(),false);
 const saved=JSON.parse(ctx.storage.getItem('scimux-retired-drafts:n1:old'));
 assert.ok(saved.some(value=>value['scimux-people-draft:']==='people draft'));assert.equal(ctx.feature.promptText(),'');ctx.feature.destroy();
});

test('retired draft review shows escaped text and retained source context without preparing a send', () => {
 const html=composer.retiredDraftReviewHTML([{owner:'unknown',values:{'scimux-draft:':'old <script>','scimux-people-draft:':'discussion','scimux-prompt-sources:':'[{"uid":"u","record":1}]'}}]);
 assert.match(html,/old &lt;script&gt;/);assert.match(html,/discussion/);
 assert.match(html,/source/i);assert.match(html,/readonly/);assert.doesNotMatch(html,/<script>/);
});

test('a never-dispatched retired send is clearly labelled in review', () => {
 const html=composer.retiredDraftReviewHTML([{owner:'old',values:{'scimux-draft:':'draft','scimux-delivery-status:':'not-dispatched'}}]);
 assert.match(html,/Not sent/);
});

test('recovery review exposes text captured in an in-flight prompt', () => {
 const html=composer.retiredDraftReviewHTML([{owner:'old',values:{'scimux-prompt-flights:':'[{"text":"waiting <prompt>","sources":[{"uid":"u","record":2}]}]'}}]);
 assert.match(html,/waiting &lt;prompt&gt;/);assert.match(html,/source/i);
});

test('damaged flight details do not hide the saved text or file-reference notice', () => {
 const values={'scimux-draft:':'text kept','scimux-prompt-flights:':'not json','scimux-attachments:':'[]'};
 const html=composer.retiredDraftReviewHTML([{owner:'old',values}]);
 assert.match(html,/text kept/);assert.match(html,/File references/);
 const malformed=composer.retiredDraftReviewHTML([{owner:'old',values:{'scimux-prompt-flights:':'{}'}}]);
 assert.doesNotMatch(malformed,/Pending Agent draft/);
});

test('an old unattributed draft is available for review without entering the replacement composer', () => {
 const ctx=makeFeature({recovery:true,nodes:[{id:'n1',instance_id:'new'}],drafts:{'scimux-draft:n1':'legacy words'}});
 ctx.feature.bind();ctx.feature.onSelect();
 assert.equal(ctx.feature.promptText(),'');assert.equal(ctx.roots.recovery.hidden,false);assert.match(ctx.roots.recovery.innerHTML,/legacy words/);
 assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();
});
