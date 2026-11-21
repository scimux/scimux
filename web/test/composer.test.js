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
import { AWAITING_FORWARD_TTL_MS, readRetiredDrafts } from "../js/storage.js";
import * as composer from "../js/composer.js";
import {createPeopleFileStore} from "../js/people-file-store.js";
import {parsePeoplePendingSnapshot} from "../js/people-local-review.js";

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
    keys(){ return [...map.keys()]; },
    _map: map,
  };
}

function renderedClassCount(html, token){
  const markup = html.replace(/<textarea\b[^>]*>[\s\S]*?<\/textarea\s*>/gi, "");
  return [...markup.matchAll(/<[a-z][^>]*>/gi)].filter(([tag]) => {
    const attr = tag.match(/\sclass\s*=\s*"([^"]*)"/);
    return attr && attr[1].split(/[\t\n\f\r ]+/).includes(token);
  }).length;
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
    if (sel === ".people-retry") {
      attstage._retryButtons = Array.from({length:renderedClassCount(attstage.innerHTML, "people-retry")}, () => el("button"));
      return attstage._retryButtons;
    }
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
  const peopleSaved = new Map();
  let peopleRecoveryRows = [{key:"allocator", epoch:"a".repeat(32), next:1}];
  const recoveryDriver = {read:async()=>[], readWrite:async fn=>fn([]).value, writePeople:async fn=>{const result=fn({files:[], recovery:peopleRecoveryRows});peopleRecoveryRows=result.recovery;return result.value;}};
  const recoveryStore = createPeopleFileStore(recoveryDriver);
  if (overrides.deps?.peopleFileStore && !overrides.deps.peopleFileStore.setStageRevision) overrides.deps.peopleFileStore.setStageRevision = async()=>{};
  if (overrides.deps?.peopleFileStore && !overrides.deps.peopleFileStore.removeStageFile) overrides.deps.peopleFileStore.removeStageFile = (...args)=>overrides.deps.peopleFileStore.remove(...args);
  let peopleFileNumber = 0;

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
    peopleFileStore: {
      savePeopleDraft:recoveryStore.savePeopleDraft, clearPeopleDraft:recoveryStore.clearPeopleDraft, listPeopleRecovery:recoveryStore.listPeopleRecovery,
      async save(scope,id,file){ peopleSaved.set(scope + "\0" + id,{id,file}); },
      async setStageRevision(scope,id,stageRevision){ peopleSaved.get(scope + "\0" + id).stageRevision=stageRevision; },
      async list(scope){ return [...peopleSaved.entries()].filter(([key])=>key.startsWith(scope + "\0")).map(([,value])=>value); },
      async markPublished(){},
      async markPublishedBatch(){},
      async remove(scope,id){ peopleSaved.delete(scope + "\0" + id); },
      async removeStageFile(scope,id){ peopleSaved.delete(scope + "\0" + id); },
    },
    randomBytes: () => new Uint8Array(peopleFileNumber++ === 0 ? [1,35,69,103,137,171,205,239] : [254,220,186,152,118,84,50,16]),
    finishPeople: () => true,
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
      if (typeof uploadHandler === "function") {
        const result = await uploadHandler(path, opts, fetchCalls);
        if (!overrides.preserveResponse && path.includes('/subscriptions/') && result.ok && result.json) {
          const original = result.json;
          result.json = async()=>{ const body = await original(); for (const attachment of body.attachments || []) if (attachment.id && !attachment.revision) attachment.revision = 'a'.repeat(64); return body; };
        }
        return result;
      }
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
    feature, roots, storage, recoveryStore, recoveryRows:()=>peopleRecoveryRows, apiCalls, fetchCalls, revoked, created, alerts,
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


// Synthetic transport around the real People store: publication admission and
// completion both commit through the same serialized test driver.
function transactionalPeopleFixture({context:provided, nodes, respond, sendHandler, depsExtra, afterDecision}={}) {
 const context=provided || {key:localReviewKey,copyInstance:'1'.repeat(32),binding:'current',owner:false,writable:true,attachments:true};
 let files=[],recovery=[{key:'allocator',epoch:'a'.repeat(32),next:1}],tail=Promise.resolve(),pending=null,failDecision=false,sendEntries=0,held=null,nextGate=null;const sends=[],sendAttempts=[],retries=[],events=[],readyWaiters=[],sendWaiters=[],draftWaiters=[],retryWaiters=[];
 const writePeople=fn=>{const gate=nextGate;nextGate=null;const operation=tail.then(async()=>{if(gate){gate.entered();await gate.wait;}const result=fn({files,recovery});if(failDecision&&result.recovery.some(row=>row.kind==='decision')&&!recovery.some(row=>row.kind==='decision'))throw Error('synthetic storage unavailable');files=result.files;recovery=result.recovery;return result.value;});tail=operation.then(()=>{},()=>{});return operation;};
 const driver={read:()=>tail.then(()=>files),readWrite:fn=>writePeople(({files,recovery})=>{const result=fn(files);return {files:result.rows,recovery,value:result.value};}),writePeople};
 const arm=point=>{if(held?.point===point){nextGate=held;held=null;}};
 const actual=createPeopleFileStore(driver),store={...actual,async savePeopleDraft(...args){const result=await actual.savePeopleDraft(...args);events.push('draft-ready');for(const resolve of draftWaiters.splice(0))resolve();return result;},async setStageRevision(...args){const result=await actual.setStageRevision(...args);events.push('stage-ready');for(const resolve of readyWaiters.splice(0))resolve();return result;},removeStageFile(...args){events.push('remove');arm('remove');return actual.removeStageFile(...args);}};
 const holdNext=point=>{let release,reject,entered;const wait=new Promise((resolve,fail)=>{release=resolve;reject=fail;}),ready=new Promise(resolve=>{entered=resolve;});held={point,wait,entered};return {ready,release,reject};};
 const waitReady=async count=>{while(events.filter(event=>event==='stage-ready').length<count)await new Promise(resolve=>readyWaiters.push(resolve));};
 const waitSend=async count=>{while(sends.length<count)await new Promise(resolve=>sendWaiters.push(resolve));};
 const waitDraft=async count=>{while(events.filter(event=>event==='draft-ready').length<count)await new Promise(resolve=>draftWaiters.push(resolve));};
 const waitRetry=async count=>{while(retries.length<count)await new Promise(resolve=>retryWaiters.push(resolve));};
 const deps={peopleContext:()=>context,peopleFileStore:store,pendingPeople:()=>pending,
  sendPeople:async(key,text,attachments,native,admission)=>{
   sendEntries++;
   sendAttempts.push({key,text,attachments:attachments&&[...attachments],native,requireDurableAdmission:admission?.requireDurableAdmission});
   assert.ok(admission);assert.equal(key,context.key);
   const requestKey=(2+sends.length).toString(16).repeat(16);
   if (!attachments) {
    const request={key:requestKey,text,anchor:0},identity={scope:admission.scope,peopleKey:key,copyInstance:context.copyInstance,binding:context.binding||'',native,head:admission.head,version:admission.version,request},admitted=await actual.beginPeopleTextPublication({...identity,current:admission.current});
    sends.push({key,text,native,request,publication:admitted.publication});for(const resolve of sendWaiters.splice(0))resolve();const result=respond?await respond(sends.at(-1)):{seq:7};
    return {...result,peopleTextDecision:await actual.finishPeopleTextPublication({scope:admission.scope,publication:admitted.publication,expected:identity,seq:result.seq,current:admission.current})};
   }
   const request={key:requestKey,text,anchor:0,attachments:[...attachments]};
   const admitted=await actual.beginPeoplePublication({scope:admission.scope,peopleKey:key,copyInstance:context.copyInstance,binding:context.binding||'',native,head:admission.head,version:admission.version,request,files:admission.files,current:admission.current});
   const snapshot={format:'transactional',scope:admission.scope,publication:admitted.publication,head:admission.head,version:admission.version,key,instance:native,copyInstance:context.copyInstance,binding:context.binding||'',storageKey:'idb:'+admitted.publication,value:JSON.stringify({instance:native,copyInstance:context.copyInstance,request}),publicationKey:request.key,text,attachments:[...attachments]};
   pending=snapshot;admission.onPrepared(snapshot);sends.push({key,text,attachments:[...attachments],native,request,snapshot});for(const resolve of sendWaiters.splice(0))resolve();
   const result=respond?await respond(sends.at(-1)): {seq:7};return {...result,peoplePending:snapshot};
  },
  finishPeople:async(receipt,options)=>{events.push('finish');arm('decision');const snapshot=receipt.peoplePending,result=await actual.decidePeoplePublication({scope:snapshot.scope,publication:snapshot.publication,expected:snapshot,proof:{state:'completed',seq:receipt.seq,text:snapshot.text,stage_ids:snapshot.attachments},current:options.current});if(pending===snapshot)pending=null;if(afterDecision)await afterDecision(result);return result;},
  retryPeople:async()=>{const snapshot=pending;assert.ok(snapshot);const request=JSON.parse(snapshot.value).request;retries.push({snapshot,request});for(const resolve of retryWaiters.splice(0))resolve();const result=respond?await respond(retries.at(-1)):{seq:7};return {...result,peoplePending:snapshot};},
 };
 const ctx=makeFeature({sel:context.owner?'n1':'team/'+context.key,nodes:nodes || (context.owner?[{id:'n1',instance_id:'owner-instance'}]:[]),deps:{...deps,...depsExtra},sendHandler,uploadHandler:async(_path,opts)=>({ok:true,json:async()=>({attachments:[{id:opts.body.parts[0].name==='new.txt'?'fedcba9876543210':'0123456789abcdef',revision:'a'.repeat(64)}]})})});
 return {ctx,context,actual,driver,store,sends,sendAttempts,retries,events,waitReady,waitSend,waitDraft,waitRetry,holdNext,sendEntries:()=>sendEntries,files:()=>files,recovery:()=>recovery,pending:()=>pending,setDecisionFailure:value=>{failDecision=value;}};
}

async function assertPeopleDrafts(ctx, scope, texts) {
  await new Promise(resolve => setImmediate(resolve));
  const state = await ctx.recoveryStore.listPeopleRecovery(scope);
  assert.deepEqual(state.heads.map(head => head.text).sort(), [...texts].sort());
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
  await assertPeopleDrafts(ctx,"people\0s/o/c/g\0",["newer"]); assert.equal(ctx.storage.getItem("scimux-people-draft:team/s/o/c/g"),null);
  const reopened=makeFeature({sel:"team/s/o/c/g",deps:{peopleContext:()=>({key:"s/o/c/g",writable:true}),peopleFileStore:ctx.recoveryStore}});reopened.feature.bind();reopened.feature.onSelect();await new Promise(r=>setImmediate(r));assert.equal(reopened.feature.promptText(),"newer");assert.equal(reopened.apiCalls.length,0);reopened.feature.destroy();
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

test("owner keeps Stop for the busy agent after disarming to an empty People draft", async () => {
  const ctx = makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true})}, sendHandler:async()=>({delivery:'ok'})});
  ctx.feature.bind(); ctx.feature.onSelect(); ctx.feature.setDestination('agent');
  ctx.feature.setPromptText('run it'); ctx.roots.prompt.firstListener('input')();
  await ctx.feature.sendPrompt();
  assert.equal(ctx.feature.destination(), 'people');
  ctx.feature.setComposerBusy(true);
  const btn = ctx.roots.sendbtn;
  assert.equal(btn.classList.contains('stop'), true);
  assert.equal(btn.innerHTML, 'X');
  assert.equal(btn.getAttribute('aria-label'), 'stop agent');
  const before = ctx.apiCalls.length;
  await btn.firstListener('click')();
  assert.equal(ctx.apiCalls.length, before + 1);
  assert.match(ctx.apiCalls.at(-1).path, /\/api\/nodes\/n1\/send\/interrupt/);
  // Typing a People message turns the control back into Send to people.
  ctx.feature.setPromptText('hello people'); ctx.roots.prompt.firstListener('input')();
  assert.equal(btn.classList.contains('stop'), false);
  assert.equal(btn.innerHTML, 'S');
  assert.equal(btn.getAttribute('aria-label'), 'Send message to people');
  // Clearing the People draft restores Stop while the agent is still busy.
  ctx.feature.setPromptText(''); ctx.roots.prompt.firstListener('input')();
  assert.equal(btn.classList.contains('stop'), true);
  ctx.feature.setComposerBusy(false);
  assert.equal(btn.classList.contains('stop'), false);
  assert.equal(btn.getAttribute('aria-label'), 'Send message to people');
});

test("busy chrome falls back to empty markup when icons are missing", () => {
  const ctx = makeFeature({deps:{icons:{}}});
  ctx.feature.bind();
  ctx.feature.setComposerBusy(true);
  assert.equal(ctx.roots.sendbtn.innerHTML, '');
  assert.equal(ctx.roots.sendbtn.classList.contains('stop'), true);
  ctx.feature.setComposerBusy(false);
  assert.equal(ctx.roots.sendbtn.innerHTML, '');
  assert.equal(ctx.roots.sendbtn.classList.contains('stop'), false);
});

test("recipients never get Stop for the sharer's busy agent", async () => {
  const ctx = makeFeature({sel:"team/s/o/c/g", deps:{peopleContext:()=>({key:"s/o/c/g",writable:true})}});
  ctx.feature.bind(); ctx.feature.onSelect(); ctx.feature.setComposerBusy(true);
  const btn = ctx.roots.sendbtn;
  assert.equal(btn.classList.contains('stop'), false);
  await btn.firstListener('click')();
  assert.equal(ctx.apiCalls.length, 0);
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
  await assertPeopleDrafts(ctx,'people\0s/o/c/g\0n1\0fallback:1',['people']);assert.equal(ctx.storage.getItem('scimux-people-draft:n1'),null);
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
  await assertPeopleDrafts(ctx,"people\0s/o/c/g\0",["keep"]);assert.equal(ctx.storage.getItem("scimux-people-draft:team/s/o/c/g"),null);
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
  let finish, selectedKey='s/o/a/g'; const ctx=makeFeature({sel:'team/s/o/a/g',deps:{peopleContext:()=>({key:selectedKey,writable:true}),sendPeople:()=>new Promise(r=>{finish=r})}});
  ctx.feature.bind(); ctx.feature.onSelect(); ctx.feature.setPromptText('original'); ctx.roots.prompt.firstListener('input')();
  const request=ctx.feature.sendPrompt();await new Promise(r=>setImmediate(r));selectedKey='s/o/b/g';ctx.setSel('team/s/o/b/g'); ctx.feature.onSelect();
  ctx.feature.setPromptText('different chat'); ctx.roots.prompt.firstListener('input')(); finish(); await request;
  await assertPeopleDrafts(ctx,'people\0s/o/a/g\0',[]);await assertPeopleDrafts(ctx,'people\0s/o/b/g\0',['different chat']);
  assert.equal(ctx.storage.getItem('scimux-people-draft:team/s/o/a/g'),null);assert.equal(ctx.storage.getItem('scimux-people-draft:team/s/o/b/g'),null);
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
  await assertPeopleDrafts(ctx,'people\0s/o/c/g\0',['keep']);assert.equal(ctx.storage.getItem('scimux-people-draft:team/s/o/c/g'),null); assert.deepEqual(ctx.alerts,['offline']);
});

test('People send accepts no second send while the first is in flight', async () => {
  let resolve, sends=0; const ctx=makeFeature({sel:'team/s/o/c/g',deps:{peopleContext:()=>({key:'s/o/c/g',writable:true}),sendPeople:()=>{sends++; return new Promise(r=>resolve=r)}}});
  ctx.feature.bind(); ctx.feature.onSelect(); ctx.feature.setPromptText('once'); ctx.roots.prompt.firstListener('input')();
  const pending=ctx.feature.sendPrompt(); await ctx.feature.sendPrompt(); await new Promise(r=>setImmediate(r)); assert.equal(sends,1); resolve(); await pending;
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
  const sent=ctx.feature.sendPrompt(); await new Promise(r=>setImmediate(r)); context=null; ctx.feature.contextChanged();
  ctx.feature.setPromptText('newer'); ctx.roots.prompt.firstListener('input')();
  ctx.setSel('n2'); ctx.feature.onSelect(); ctx.feature.setPromptText('other'); ctx.roots.prompt.firstListener('input')();
  finish(); await sent;
  assert.equal(ctx.feature.destination(),'agent'); assert.equal(ctx.feature.promptText(),'other');
  await assertPeopleDrafts(ctx,'people\0s/o/c/g\0n1\0fallback:1',['newer']);assert.equal(ctx.storage.getItem('scimux-people-draft:n1'),null);
  assert.equal(ctx.roots.destination.hidden,true); assert.equal(ctx.apiCalls.length,0);
});

test('owner prepares a shared source into the Agent draft without dispatch and clears its marker only on erasure', async () => {
  let context={key:'s/o/c/g',writable:true,owner:true};
  const ctx=makeFeature({deps:{peopleContext:()=>context,canPreparePrompt:()=>true}});
  ctx.storage.setItem('scimux-draft:n1','earlier'); ctx.feature.bind(); ctx.feature.onSelect();
  ctx.feature.setPromptText('people text'); ctx.roots.prompt.firstListener('input')();
  const source={uid:'u',segment:0,record:1};
  assert.equal(ctx.feature.preparePrompt('quoted',source),true);
  assert.equal(ctx.feature.destination(),'agent'); assert.equal(ctx.feature.promptText(),'earlier\n\nquoted');
  await assertPeopleDrafts(ctx,'people\0s/o/c/g\0n1\0fallback:1',['people text']);assert.equal(ctx.storage.getItem('scimux-people-draft:n1'),null);
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
 const sending=ctx.feature.sendPrompt();await new Promise(r=>setImmediate(r));ctx.nodes.splice(0);ctx.setSel('');ctx.feature.onSelect();
 generation='new';ctx.nodes.push({id:'n1',instance_id:'new'});ctx.setSel('n1');ctx.feature.onSelect();
 ctx.feature.setPromptText('same text');ctx.roots.prompt.firstListener('input')();
 finish();await sending;assert.equal(ctx.feature.promptText(),'same text');await assertPeopleDrafts(ctx,'people\0s/o/c/new\0n1\0new',['same text']);assert.equal(ctx.storage.getItem('scimux-people-draft:n1'),null);ctx.feature.destroy();
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

test("People attachment capability stages files without changing destination or dispatching", async () => {
  const ctx=makeFeature({deps:{peopleContext:()=>({key:"s/o/c/g",writable:true,owner:true,attachments:true}),sendPeople:async()=>{}}});
  ctx.feature.bind(); ctx.feature.onSelect(); ctx.feature.setPromptText("discussion draft");
  ctx.feature.addFiles([{name:"synthetic.txt",type:"text/plain",size:12}]);
  assert.equal(ctx.feature.destination(),"people");
  assert.equal(ctx.feature.promptText(),"discussion draft");
  assert.match(ctx.roots.attstage.innerHTML,/synthetic\.txt/);
  assert.equal(ctx.apiCalls.filter(call=>call.path.split("?")[0].endsWith("/send")).length,0);
});

test("People file validation keeps valid staged files and never changes Agent draft", async () => {
 const ctx=makeFeature({deps:{peopleContext:()=>({key:"s/o/c/g",writable:true,owner:true,attachments:true}),sendPeople:async()=>{}}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setDestination("agent");ctx.feature.setAttachAvail(true);ctx.feature.setPromptText("agent draft");ctx.roots.prompt.firstListener("input")();
 ctx.feature.setDestination("people");ctx.feature.setPromptText("people draft");ctx.roots.prompt.firstListener("input")();
 ctx.feature.addFiles([{name:"valid.txt",type:"text/plain",size:12},{name:"empty.txt",type:"text/plain",size:0},{name:"large.txt",type:"text/plain",size:(4<<20)+1}]);
 assert.match(ctx.roots.attstage.innerHTML,/valid\.txt/);assert.doesNotMatch(ctx.roots.attstage.innerHTML,/empty\.txt|large\.txt/);
 assert.match(ctx.alerts.join(" "),/empty|4\s*MiB/i);assert.equal(ctx.feature.promptText(),"people draft");
 ctx.feature.setDestination("agent");assert.equal(ctx.feature.promptText(),"agent draft");
});

test("People attachment feedback identifies unavailable local copy metadata", () => {
 const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true,attachments:false,copyIdentityMissing:true})}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('preserved discussion');ctx.roots.prompt.firstListener('input')();
 ctx.feature.addFiles([{name:'synthetic.txt',type:'text/plain',size:1}]);
 assert.match(ctx.alerts.join(' '),/copy identity.*unavailable/i);assert.equal(ctx.feature.promptText(),'preserved discussion');assert.equal(ctx.feature.destination(),'people');assert.equal(ctx.fetchCalls.length,0);ctx.feature.destroy();
});

test("People staged removal waits for scoped server removal", async () => {
 const ctx=makeFeature({deps:{peopleContext:()=>({key:"11111111111111111111111111111111/22222222222222222222222222222222/3333333333333333/4444444444444444",copyInstance:"55555555555555555555555555555555",writable:true,owner:true,attachments:true})},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:"0123456789abcdef",revision:'a'.repeat(64)}]})})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.addFiles([{name:"synthetic.txt",type:"text/plain",size:12}]);await new Promise(r=>setImmediate(r));
 ctx.roots.attstage._stageButtons[0].firstListener("click")();await new Promise(r=>setImmediate(r));
 assert.equal(ctx.apiCalls.filter(call=>call.opts?.method==="DELETE" && call.path.includes("/subscriptions/11111111111111111111111111111111/22222222222222222222222222222222/3333333333333333/4444444444444444/attachments/stage/0123456789abcdef")).length,1);
 assert.match(ctx.apiCalls.find(call=>call.opts?.method==='DELETE').path,/revision=a{64}/);
 assert.equal(ctx.roots.attstage.hidden,true);
});

test("People file-only send supplies the frozen stage identity without agent dispatch", async () => {
 const f=transactionalPeopleFixture(),ctx=f.ctx;
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.addFiles([{name:'synthetic.txt',type:'text/plain',size:12}]);await f.waitReady(1);await ctx.feature.sendPrompt();assert.equal(f.sends.length,1);assert.equal(f.sends[0].key,f.context.key);assert.equal(f.sends[0].text,'');assert.deepEqual(f.sends[0].attachments,['0123456789abcdef']);assert.equal(f.sends[0].request.key,'2'.repeat(16));assert.match(ctx.apiCalls.find(call=>call.opts?.method==='DELETE').path,/revision=a{64}/);assert.equal(ctx.apiCalls.filter(call=>call.path.split('?')[0].endsWith('/send')).length,0);assert.equal(f.recovery().find(row=>row.kind==='decision').seq,7);assert.equal(f.files().length,0);}finally{ctx.feature.destroy();}
});

test('legacy saved People file stays intact until its current stage is reviewed',async()=>{
 let removed=0, deleted=0;
 const store={list:async()=>[{id:'0123456789abcdef',file:{name:'legacy.txt',type:'text/plain',size:1}}],remove:async()=>{removed++}};
 const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true,attachments:true}),peopleFileStore:store,api:async()=>{deleted++}}});
 ctx.feature.bind();ctx.feature.onSelect();await new Promise(resolve=>setImmediate(resolve));
 ctx.roots.attstage._stageButtons[0].firstListener('click')();await new Promise(resolve=>setImmediate(resolve));
 assert.equal(deleted,0);assert.equal(removed,0);assert.match(ctx.alerts.join(' '),/review/i);assert.match(ctx.roots.attstage.innerHTML,/legacy\.txt/);ctx.feature.destroy();
});

test('confirmed legacy file remains saved with explicit review feedback',async()=>{
 const key=localReviewKey,copyInstance='1'.repeat(32),binding='current',scope=`people\0${key}\0binding:${binding}\0copy:${copyInstance}\0`,id='0123456789abcdef',file={name:'legacy.txt',type:'text/plain',size:1},value=JSON.stringify({instance:'',copyInstance,request:{key:'2'.repeat(16),text:'caption',anchor:0,attachments:[id]}}),storageKey=`scimux-people-file-pending:${key}::${binding}:copy:${copyInstance}`;let files=[{key:scope+'\0'+id,scope,id,file}],recovery=[{key:'allocator',epoch:'a'.repeat(32),next:1}],tail=Promise.resolve(),retries=0,finished=0;
 const writePeople=fn=>{const op=tail.then(()=>{const result=fn({files,recovery});files=result.files;recovery=result.recovery;return result.value;});tail=op.then(()=>{},()=>{});return op;},driver={read:()=>tail.then(()=>files),readWrite:fn=>writePeople(({files,recovery})=>{const result=fn(files);return {files:result.rows,recovery,value:result.value};}),writePeople},store=createPeopleFileStore(driver);let pending={format:'legacy',key,copyInstance,binding,instance:'',storageKey,value,publicationKey:'2'.repeat(16),text:'caption',attachments:[id]};
 const ctx=makeFeature({sel:'team/'+key,nodes:[],deps:{peopleContext:()=>({key,copyInstance,binding,owner:false,writable:true,attachments:true}),peopleFileStore:store,pendingPeople:()=>pending,retryPeople:async()=>{retries++;return {seq:1,peoplePending:pending};},finishPeople:async(receipt,options)=>{finished++;const result=await store.recordLegacyPeopleDecision({scope,snapshot:receipt.peoplePending,proof:{state:'completed',seq:receipt.seq,text:'caption',stage_ids:[id]},current:options.current});pending=null;return {state:result.state,seq:1,filesConfirmed:false,draftCleared:false};}}});
 try{ctx.storage.setItem(storageKey,value);ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(retries,0);ctx.roots.attstage._retryButtons[0].click();await localReviewTick();assert.equal(retries,1);assert.equal(finished,1);assert.equal(ctx.storage.getItem(storageKey),value);assert.equal(files.length,1);assert.equal(files[0].file,file);assert.equal(files[0].published,undefined);assert.equal(recovery.find(row=>row.kind==='legacy-decision').seq,1);assert.match(ctx.roots.attstage.innerHTML,/Sent at sequence 1; review/i);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('restored confirmed legacy file asks for review',async()=>{
 const store={list:async()=>[{id:'0123456789abcdef',published:1,file:{name:'legacy.txt',type:'text/plain',size:1}}]};
 const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true,attachments:true}),peopleFileStore:store}});
 ctx.feature.bind();ctx.feature.onSelect();await new Promise(resolve=>setImmediate(resolve));assert.match(ctx.roots.attstage.innerHTML,/Sent; review/i);ctx.feature.destroy();
});

test('restored People file keeps original revision if the same stage ID is replaced',async()=>{
 let removed=0; const paths=[];
 const store={list:async()=>[{id:'0123456789abcdef',stageRevision:'b'.repeat(64),file:{name:'original.txt',type:'text/plain',size:1}}],remove:async()=>{removed++}};
 const ctx=makeFeature({deps:{peopleContext:()=>({key:'11111111111111111111111111111111/22222222222222222222222222222222/3333333333333333/4444444444444444',copyInstance:'c'.repeat(32),writable:true,owner:true,attachments:true}),peopleFileStore:store,api:async(path)=>{paths.push(path);throw Error('stage changed')}}});
 ctx.feature.bind();ctx.feature.onSelect();await new Promise(resolve=>setImmediate(resolve));
 ctx.roots.attstage._stageButtons[0].firstListener('click')();await new Promise(resolve=>setImmediate(resolve));
 assert.match(paths[0],/revision=b{64}/);assert.equal(removed,0);assert.match(ctx.roots.attstage.innerHTML,/original\.txt/);ctx.feature.destroy();
});

test('People upload persists its original revision for removal after reload',async()=>{
 let rows=[];const driver={read:async()=>rows,readWrite:async fn=>{const result=fn(rows);rows=result.rows;return result.value}},store=createPeopleFileStore(driver);
 const context=()=>({key:'11111111111111111111111111111111/22222222222222222222222222222222/3333333333333333/4444444444444444',binding:'a'.repeat(64),copyInstance:'b'.repeat(32),writable:true,owner:true,attachments:true});
 const first=makeFeature({nodes:[{id:'n1',instance_id:'owner'}],deps:{peopleContext:context,peopleFileStore:store},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:'0123456789abcdef',revision:'c'.repeat(64)}]})})});
 first.feature.bind();first.feature.onSelect();first.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await new Promise(resolve=>setImmediate(resolve));first.feature.destroy();
 const paths=[],second=makeFeature({nodes:[{id:'n1',instance_id:'owner'}],deps:{peopleContext:context,peopleFileStore:store,api:async path=>{paths.push(path)}}});
 second.feature.bind();second.feature.onSelect();await new Promise(resolve=>setImmediate(resolve));second.roots.attstage._stageButtons[0].firstListener('click')();await new Promise(resolve=>setImmediate(resolve));
 assert.match(paths[0],/copy_instance=b{32}&revision=c{64}/);assert.equal(rows.length,0);second.feature.destroy();
});

test('People upload refuses a missing stage revision and retains the file',async()=>{
 const ctx=makeFeature({preserveResponse:true,deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true,attachments:true})},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:'0123456789abcdef'}]})})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.addFiles([{name:'original.txt',type:'text/plain',size:1}]);await new Promise(resolve=>setImmediate(resolve));
 assert.match(ctx.roots.attstage.innerHTML,/original\.txt/);assert.match(ctx.roots.attstage.innerHTML,/review/i);assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();
});

test('People recovery does not replace its saved revision on upload',async()=>{
 let sent=0;
 const store={list:async()=>[{id:'0123456789abcdef',stageRevision:'b'.repeat(64),file:{name:'original.txt',type:'text/plain',size:1}}]};
 const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true,attachments:true}),peopleFileStore:store,sendPeople:async()=>{sent++}},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:'0123456789abcdef',revision:'c'.repeat(64)}]})})});
 ctx.feature.bind();ctx.feature.onSelect();await new Promise(resolve=>setImmediate(resolve));const sending=ctx.feature.sendPrompt();await new Promise(resolve=>setImmediate(resolve));ctx.timers.runIntervals();await sending;
 assert.equal(sent,0);assert.match(ctx.roots.attstage.innerHTML,/original\.txt/);assert.match(ctx.roots.attstage.innerHTML,/review/i);ctx.feature.destroy();
});

test('failed revision persistence cannot adopt a replacement on retry',async()=>{
 let upload=0, sent=0;
 const store={save:async()=>{},list:async()=>[],setStageRevision:async()=>{throw Error('synthetic storage failure')}};
 const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true,attachments:true}),peopleFileStore:store,sendPeople:async()=>{sent++}},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:'0123456789abcdef',revision:(upload++ ? 'b' : 'a').repeat(64)}]})})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.addFiles([{name:'original.txt',type:'text/plain',size:1}]);await new Promise(resolve=>setImmediate(resolve));
 const sending=ctx.feature.sendPrompt();await new Promise(resolve=>setImmediate(resolve));ctx.timers.runIntervals();await sending;
 assert.equal(upload,2);assert.equal(sent,0);assert.match(ctx.roots.attstage.innerHTML,/original\.txt/);ctx.feature.destroy();
});

test("People file send preserves newer typing and staged files while pending", async () => {
 let release;const f=transactionalPeopleFixture({respond:()=>new Promise(resolve=>{release=resolve;})}),ctx=f.ctx;
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('old text');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'old.txt',type:'text/plain',size:3}]);await f.waitReady(1);const sending=ctx.feature.sendPrompt();await f.waitSend(1);assert.equal(f.sends.length,1);assert.equal(f.sends[0].request.text,'old text');assert.deepEqual(f.sends[0].request.attachments,['0123456789abcdef']);ctx.feature.setPromptText('new text');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'new.txt',type:'text/plain',size:3}]);await f.waitReady(2);release({seq:7});await sending;assert.equal(ctx.feature.promptText(),'new text');assert.match(ctx.roots.attstage.innerHTML,/new\.txt/);assert.doesNotMatch(ctx.roots.attstage.innerHTML,/old\.txt/);assert.equal(f.files().length,1);assert.equal(f.files()[0].file.name,'new.txt');assert.equal(f.files()[0].published,undefined);assert.equal(f.recovery().find(row=>row.kind==='decision').seq,7);assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);}finally{ctx.feature.destroy();}
});

test("People in-flight publication pins its original stage against removal", async () => {
 let release;const ctx=makeFeature({deps:{peopleContext:()=>({key:"s/o/c/g",writable:true,owner:true,attachments:true}),sendPeople:()=>new Promise(r=>{release=r})},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:"0123456789abcdef"}]})})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.addFiles([{name:"synthetic.txt",type:"text/plain",size:12}]);await new Promise(r=>setImmediate(r));const sending=ctx.feature.sendPrompt();await new Promise(r=>setImmediate(r));
 ctx.roots.attstage._stageButtons[0].firstListener("click")();await new Promise(r=>setImmediate(r));assert.match(ctx.roots.attstage.innerHTML,/synthetic\.txt/);assert.equal(ctx.apiCalls.filter(call=>call.opts?.method==="DELETE").length,0);
 release({seq:7});await sending;
});

test('confirmed People image releases its local preview after explicit send',async()=>{
 const f=transactionalPeopleFixture(),ctx=f.ctx;
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.addFiles([{name:'synthetic.png',type:'image/png',size:1}]);await f.waitReady(1);assert.deepEqual(ctx.created,['blob:synthetic.png']);assert.deepEqual(ctx.revoked,[]);assert.equal(f.sends.length,0);await ctx.feature.sendPrompt();assert.equal(f.sends.length,1);assert.deepEqual(ctx.revoked,['blob:synthetic.png']);assert.doesNotMatch(ctx.roots.attstage.innerHTML,/synthetic\.png/);assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);assert.equal(f.recovery().find(row=>row.kind==='decision').seq,7);assert.equal(f.files().length,0);}finally{ctx.feature.destroy();}
});

test("People file bytes are saved before upload and restored for the same share after reload", async () => {
 let release;const saved=new Map();const fileStore={save:async(scope,id,file)=>{await new Promise(r=>{release=r});saved.set(scope+"/"+id,{id,file})},list:async scope=>[...saved.entries()].filter(([key])=>key.startsWith(scope+"/")).map(([,value])=>value),remove:async(scope,id)=>saved.delete(scope+"/"+id)};
 const deps={peopleContext:()=>({key:"s/o/c/g",writable:true,owner:true,attachments:true}),peopleFileStore:fileStore,randomBytes:()=>new Uint8Array([1,2,3,4,5,6,7,8])};
 const ctx=makeFeature({deps,uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:"0102030405060708"}]})})});ctx.feature.bind();ctx.feature.onSelect();ctx.feature.addFiles([{name:"synthetic.txt",type:"text/plain",size:12}]);
 assert.equal(ctx.fetchCalls.length,0);release();await new Promise(r=>setImmediate(r));assert.equal(ctx.fetchCalls.length,1);assert.match(ctx.fetchCalls[0].path,/stage_id=0102030405060708/);
 const reloaded=makeFeature({deps});reloaded.feature.bind();reloaded.feature.onSelect();await new Promise(r=>setImmediate(r));assert.match(reloaded.roots.attstage.innerHTML,/synthetic\.txt/);
});

test("People confirmed response retains drafts if durable sent marker cannot be stored", async () => {
 const f=transactionalPeopleFixture(),ctx=f.ctx;
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('synthetic text');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'synthetic.txt',type:'text/plain',size:12}]);await f.waitReady(1);f.setDecisionFailure(true);await ctx.feature.sendPrompt();assert.equal(f.sends.length,1);assert.equal(f.events.filter(event=>event==='finish').length,1);assert.equal(ctx.feature.promptText(),'synthetic text');assert.match(ctx.roots.attstage.innerHTML,/synthetic\.txt/);assert.match(ctx.alerts.join(' '),/storage/i);assert.equal(f.recovery().filter(row=>row.kind==='publication').length,1);assert.equal(f.recovery().filter(row=>row.kind==='decision').length,0);assert.equal(f.files()[0].published,undefined);assert.equal(ctx.apiCalls.filter(call=>call.opts?.method==='DELETE').length,0);}finally{ctx.feature.destroy();}
});

test("a confirmed People file with failed cleanup stays marked sent and cannot be republished", async () => {
 const f=transactionalPeopleFixture({sendHandler:async(path,opts)=>{if(opts?.method==='DELETE'&&path.includes('/attachments/stage/'))throw Error('synthetic cleanup failure');return {delivery:'ok'};}}),ctx=f.ctx;
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await f.waitReady(1);await ctx.feature.sendPrompt();assert.equal(f.sendEntries(),1);assert.equal(f.sends.length,1);assert.equal(f.events.filter(event=>event==='finish').length,1);assert.match(ctx.roots.attstage.innerHTML,/Sent; remove this saved file to finish cleanup/);assert.equal(f.files()[0].published,7);assert.equal(f.recovery().find(row=>row.kind==='decision').seq,7);await ctx.feature.sendPrompt();assert.equal(f.sendEntries(),1);assert.equal(f.sends.length,1);assert.equal(f.events.filter(event=>event==='finish').length,1);assert.equal(f.files()[0].published,7);}finally{ctx.feature.destroy();}
});

test("People draft recovery restarts after a stale A-B-A visit", async () => {
 let release, calls=0;
 const fileStore={list:async()=>{calls++;if(calls===1)return new Promise(r=>{release=r});return [{id:"0123456789abcdef",file:{name:"saved.txt",type:"text/plain",size:1}}];}};
 const ctx=makeFeature({nodes:[{id:"n1"},{id:"n2"}],deps:{peopleContext:()=>({key:"s/o/c/g",writable:true,owner:true,attachments:true}),peopleFileStore:fileStore}});
 ctx.feature.bind();ctx.feature.onSelect();await new Promise(r=>setImmediate(r));
 ctx.setSel("n2");ctx.feature.onSelect();ctx.setSel("n1");ctx.feature.onSelect();
 release([]);await new Promise(r=>setImmediate(r));await new Promise(r=>setImmediate(r));
 assert.match(ctx.roots.attstage.innerHTML,/saved\.txt/);assert.ok(calls>=3);
});

test("Confirmed People caption clears its unchanged draft after switching to Agent", async()=>{
 for(const withFiles of [false,true]){
  let finish;const context={key:localReviewKey,copyInstance:'1'.repeat(32),binding:'current',owner:true,writable:true,attachments:withFiles},f=transactionalPeopleFixture({context,respond:()=>new Promise(resolve=>{finish=resolve;})}),ctx=f.ctx;
  try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('confirmed People caption');ctx.roots.prompt.firstListener('input')();if(withFiles){ctx.feature.addFiles([{name:'synthetic.txt',type:'text/plain',size:12}]);await f.waitReady(1);}const sending=ctx.feature.sendPrompt();await f.waitSend(1);assert.equal(typeof finish,'function');assert.equal(f.sends[0].request.text,'confirmed People caption');if(withFiles)assert.deepEqual(f.sends[0].request.attachments,['0123456789abcdef']);ctx.feature.setDestination('agent');ctx.feature.setPromptText('new Agent draft');ctx.roots.prompt.firstListener('input')();finish({seq:1});await sending;assert.equal(ctx.feature.promptText(),'new Agent draft');assert.equal(ctx.feature.destination(),'agent');ctx.feature.setDestination('people');assert.equal(ctx.feature.promptText(),'',`confirmed unchanged People caption remains armed after destination switch; withFiles=${withFiles}`);assert.equal(f.recovery().find(row=>row.kind===(withFiles?'decision':'text-decision')).seq,1);assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);}finally{ctx.feature.destroy();}
 }
});

test("confirmed People caption clears its captured draft after navigation without touching the new chat", async()=>{
 let finish;const context={key:localReviewKey,copyInstance:'1'.repeat(32),binding:'current',owner:true,writable:true,attachments:true},nodes=[{id:'n1',instance_id:'one'},{id:'n2',instance_id:'two'}],f=transactionalPeopleFixture({context,nodes,respond:()=>new Promise(resolve=>{finish=resolve;})}),ctx=f.ctx;
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('old caption');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'x.txt',type:'text/plain',size:1}]);await f.waitReady(1);const sending=ctx.feature.sendPrompt();await f.waitSend(1);assert.equal(f.sends[0].native,'one');assert.equal(f.sends[0].request.text,'old caption');ctx.setSel('n2');ctx.feature.onSelect();ctx.feature.setPromptText('new chat text');ctx.roots.prompt.firstListener('input')();finish({seq:1});await sending;assert.equal(ctx.feature.promptText(),'new chat text');ctx.setSel('n1');ctx.feature.onSelect();assert.equal(ctx.feature.promptText(),'');assert.equal(f.recovery().find(row=>row.kind==='decision').seq,1);assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);}finally{ctx.feature.destroy();}
});

test("People recovery shows the frozen caption and retries only by explicit button", async()=>{
 const id="0123456789abcdef",fileToken="11111111-1111-1111-1111-111111111111",stageRevision="a".repeat(64),publicationKey="2".repeat(16),value=JSON.stringify({request:{key:publicationKey,text:"old message",attachments:[id]}});let calls=0,finished=0;const old={key:"s/o/c/g",instance:"",text:"old message",attachments:[id],storageKey:'synthetic-slot',value};
 const fileStore={list:async()=>[{id:old.attachments[0],fileToken,stageRevision,file:{name:"saved.txt",type:"text/plain",size:1}}],markPublishedBatch:async()=>{},remove:async()=>{}};
 const ctx=makeFeature({deps:{peopleContext:()=>({key:old.key,writable:true,owner:true,attachments:true}),peopleFileStore:fileStore,pendingPeople:()=>old,retryPeople:async()=>{calls++;return {seq:1}},finishPeople:()=>{finished++;return true}}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.storage.setItem("scimux-people-pending-draft:people\0s/o/c/g\0n1\0fallback:1",JSON.stringify({binding:'s/o/c/g\0n1\0fallback:1',publicationKey,pendingValue:value,pendingStorageKey:'synthetic-slot',files:[{id,fileToken,stageRevision}]}));await new Promise(r=>setImmediate(r));
 assert.match(ctx.roots.attstage.innerHTML,/old message/);assert.match(ctx.roots.attstage.innerHTML,/saved\.txt/);assert.equal(calls,0);
 ctx.roots.attstage._retryButtons[0].firstListener("click")();await new Promise(r=>setImmediate(r));
 assert.equal(calls,1);assert.equal(finished,1);
});

test('headless People recovery still displays pending text without a DOM query adapter',async()=>{
 const pending={key:'s/o/c/g',instance:'',text:'saved caption',attachments:['0123456789abcdef']};let retries=0;
 const store={list:async()=>[{id:pending.attachments[0],file:{name:'saved.txt',type:'text/plain',size:1}}]};
 const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true,attachments:true}),peopleFileStore:store,pendingPeople:()=>pending,retryPeople:async()=>{retries++;return {seq:1}}}});
 ctx.roots.attstage.querySelectorAll=undefined;ctx.feature.bind();ctx.feature.onSelect();await new Promise(resolve=>setImmediate(resolve));
 assert.match(ctx.roots.attstage.innerHTML,/saved caption/);assert.match(ctx.roots.attstage.innerHTML,/Retry this message/);assert.equal(retries,0);assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();
});

test("an uncertain People file send immediately exposes the frozen retry control", async()=>{
 let pending=null;
 const ctx=makeFeature({deps:{peopleContext:()=>({key:"s/o/c/g",writable:true,owner:true,attachments:true}),pendingPeople:()=>pending,sendPeople:async()=>{pending={key:"s/o/c/g",text:"old caption",attachments:["0123456789abcdef"]};throw Error("unconfirmed")}},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:"0123456789abcdef"}]})})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText("old caption");ctx.roots.prompt.firstListener("input")();ctx.feature.addFiles([{name:"x.txt",type:"text/plain",size:1}]);await new Promise(r=>setImmediate(r));
 await ctx.feature.sendPrompt();assert.match(ctx.roots.attstage.innerHTML,/Retry this message/);
});

test("explicit People recovery clears only its unchanged confirmed caption",async()=>{
 const key=localReviewKey,copyInstance='1'.repeat(32),binding='current',scope=`people\0${key}\0binding:${binding}\0copy:${copyInstance}\0`,id='0123456789abcdef',revision='a'.repeat(64),events=[];let files=[],recovery=[{key:'allocator',epoch:'a'.repeat(32),next:1}];
 const driver={read:async()=>files,readWrite:async fn=>{const result=fn(files);files=result.rows;return result.value;},writePeople:async fn=>{const result=fn({files,recovery});files=result.files;recovery=result.recovery;return result.value;}},store=createPeopleFileStore(driver),file={name:'saved.txt',type:'text/plain',size:2},token=await store.save(scope,id,file);await store.setStageRevision(scope,id,revision,token);const draft=await store.savePeopleDraft({scope,text:'caption'}),request={key:'2'.repeat(16),text:'caption',anchor:0,attachments:[id]},admitted=await store.beginPeoplePublication({scope,peopleKey:key,head:draft.head,version:draft.version,copyInstance,binding,native:'',request,files:[{id,fileToken:token,stageRevision:revision}]});let pending={format:'transactional',scope,publication:admitted.publication,head:draft.head,version:draft.version,key,instance:'',copyInstance,binding,storageKey:'idb:'+admitted.publication,value:JSON.stringify({instance:'',copyInstance,request}),publicationKey:request.key,text:'caption',attachments:[id]};
 const ctx=makeFeature({sel:'team/'+key,nodes:[],deps:{peopleContext:()=>({key,copyInstance,binding,owner:false,writable:true,attachments:true}),peopleFileStore:store,pendingPeople:()=>pending,retryPeople:async()=>{events.push('retry');return {seq:7,peoplePending:pending};},finishPeople:async(receipt,options)=>{events.push('finish');const result=await store.decidePeoplePublication({scope,publication:receipt.peoplePending.publication,expected:receipt.peoplePending,proof:{state:'completed',seq:receipt.seq,text:receipt.peoplePending.text,stage_ids:receipt.peoplePending.attachments},current:options.current});pending=null;return result;},api:async(path,opts)=>{events.push(opts.method);return null;}}});
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(ctx.feature.promptText(),'caption');assert.equal(ctx.roots.attstage._retryButtons.length,1);ctx.roots.attstage._retryButtons[0].click();await localReviewTick();assert.equal(events.filter(value=>value==='retry').length,1);assert.ok(events.includes('finish'));assert.equal(ctx.feature.promptText(),'');assert.equal(recovery.find(row=>row.kind==='decision').state,'completed');assert.equal(ctx.apiCalls.length,0);assert.equal(ctx.feature.promptText(),"");}finally{ctx.feature.destroy();}
});

test('People retry preserves the draft when its old caption metadata is damaged',async()=>{
 let old={key:'s/o/c/g',instance:'',text:'old caption',attachments:['0123456789abcdef']},retries=0;
 const fileStore={list:async()=>[{id:old.attachments[0],file:{name:'saved.txt',type:'text/plain',size:1}}],markPublishedBatch:async()=>{},remove:async()=>{}};
 const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true,attachments:true}),peopleFileStore:fileStore,pendingPeople:()=>old,retryPeople:async()=>{retries++;return {seq:1}},finishPeople:()=>{old=null;return true}}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('newer editing');ctx.roots.prompt.firstListener('input')();
 ctx.storage.setItem('scimux-people-pending-draft:people\0s/o/c/g\0n1\0fallback:1','{damaged');await new Promise(resolve=>setImmediate(resolve));
 ctx.roots.attstage._retryButtons[0].firstListener('click')();await new Promise(resolve=>setImmediate(resolve));
 assert.equal(retries,1);assert.equal(ctx.feature.promptText(),'newer editing');assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);ctx.feature.destroy();
});

test("People recovery preserves newer editing even when its text is identical", async()=>{
 const id="0123456789abcdef", publicationKey="2".repeat(16), fileToken="11111111-1111-1111-1111-111111111111", stageRevision="a".repeat(64), value=JSON.stringify({request:{key:publicationKey,text:"old caption",attachments:[id]}});
 let release,old={key:"s/o/c/g",instance:"",text:"old caption",attachments:[id],storageKey:'synthetic-slot',value};
 const fileStore={list:async()=>[{id,fileToken,stageRevision,file:{name:"saved.txt",type:"text/plain",size:1}}],markPublishedBatch:async()=>{},remove:async()=>{}};
 const ctx=makeFeature({deps:{peopleContext:()=>({key:"s/o/c/g",writable:true,owner:true,attachments:true}),peopleFileStore:fileStore,pendingPeople:()=>old,retryPeople:()=>new Promise(r=>{release=r}),finishPeople:()=>{old=null;return true}}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText("old caption");ctx.roots.prompt.firstListener("input")();
 ctx.storage.setItem("scimux-people-pending-draft:people\0s/o/c/g\0n1\0fallback:1",JSON.stringify({binding:ctx.storage.getItem("scimux-people-draft:n1:binding"),text:"old caption",version:ctx.storage.getItem("scimux-people-draft:n1:version"),publicationKey,pendingValue:value,pendingStorageKey:'synthetic-slot',files:[{id,fileToken,stageRevision}]}));await new Promise(r=>setImmediate(r));
 ctx.roots.attstage._retryButtons[0].firstListener("click")();ctx.roots.prompt.firstListener("input")();release({seq:1});await new Promise(r=>setImmediate(r));assert.equal(ctx.feature.promptText(),"old caption");
});

test('People retry refuses missing saved files and preserves the pending publication',async()=>{
 let retries=0;const pending={key:'s/o/c/g',instance:'',text:'saved caption',attachments:['0123456789abcdef']};
 const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true,attachments:true}),peopleFileStore:{list:async()=>[]},pendingPeople:()=>pending,retryPeople:async()=>{retries++;return {seq:1}}}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('newer editing');ctx.roots.prompt.firstListener('input')();await new Promise(resolve=>setImmediate(resolve));
 ctx.roots.attstage._retryButtons[0].firstListener('click')();await new Promise(resolve=>setImmediate(resolve));
 assert.equal(retries,0);assert.equal(ctx.feature.promptText(),'newer editing');assert.match(ctx.alerts.at(-1),/saved files.*unavailable/i);assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();
});

test('People retry waits for explicitly recovered files and never replays while loading',async()=>{
 const id='0123456789abcdef',fileToken='11111111-1111-1111-1111-111111111111',stageRevision='a'.repeat(64),publicationKey='2'.repeat(16),value=JSON.stringify({request:{key:publicationKey,text:'saved caption',attachments:[id]}}),pending={key:'s/o/c/g',instance:'',text:'saved caption',attachments:[id],storageKey:'synthetic-slot',value};let release,retries=0,finished=0;
 const store={list:()=>new Promise(resolve=>{release=resolve}),markPublishedBatch:async()=>{},remove:async()=>{}};
 const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true,attachments:true}),peopleFileStore:store,pendingPeople:()=>pending,retryPeople:async()=>{retries++;return {seq:1}},finishPeople:()=>{finished++;return true}}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.storage.setItem('scimux-people-pending-draft:people\0s/o/c/g\0n1\0fallback:1',JSON.stringify({binding:'s/o/c/g\0n1\0fallback:1',publicationKey,pendingValue:value,pendingStorageKey:'synthetic-slot',files:[{id,fileToken,stageRevision}]}));ctx.feature.setPromptText('newer draft');
 ctx.roots.attstage._retryButtons[0].firstListener('click')();
 assert.match(ctx.alerts.at(-1),/saved files.*unavailable/i);assert.equal(retries,0);assert.equal(ctx.feature.promptText(),'newer draft');
 await new Promise(resolve=>setImmediate(resolve));release([{id:pending.attachments[0],fileToken,stageRevision,file:{name:'saved.txt',type:'text/plain',size:1}}]);await new Promise(resolve=>setImmediate(resolve));
 assert.match(ctx.roots.attstage.innerHTML,/saved\.txt/);assert.equal(retries,0);
 ctx.roots.attstage._retryButtons[0].firstListener('click')();await new Promise(resolve=>setImmediate(resolve));
 assert.equal(retries,1);assert.equal(finished,1);assert.equal(ctx.feature.promptText(),'newer draft');ctx.feature.destroy();
});

test('a stale People retry control cannot retry after switching to Agent',async()=>{
 const pending={key:'s/o/c/g',instance:'',text:'saved caption',attachments:['0123456789abcdef']};let retries=0;
 const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true,attachments:true}),peopleFileStore:{list:async()=>[]},pendingPeople:()=>pending,retryPeople:async()=>{retries++;return {seq:1}}}});
 ctx.feature.bind();ctx.feature.onSelect();await new Promise(resolve=>setImmediate(resolve));const oldClick=ctx.roots.attstage._retryButtons[0].firstListener('click');
 ctx.feature.setDestination('agent');ctx.feature.setPromptText('Agent draft');oldClick();await new Promise(resolve=>setImmediate(resolve));
 assert.equal(retries,0);assert.equal(ctx.feature.promptText(),'Agent draft');assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();
});

test('People retry reports an unconfirmed message when the failure has no detail',async()=>{
 const pending={key:'s/o/c/g',instance:'',text:'saved',attachments:['0123456789abcdef']};let retries=0;
 const store={list:async()=>[{id:pending.attachments[0],file:{name:'saved.txt',type:'text/plain',size:1}}]};
 const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true,attachments:true}),peopleFileStore:store,pendingPeople:()=>pending,retryPeople:async()=>{retries++;throw null}}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('new draft');await new Promise(resolve=>setImmediate(resolve));
 ctx.roots.attstage._retryButtons[0].firstListener('click')();await new Promise(resolve=>setImmediate(resolve));
 assert.equal(retries,1);assert.match(ctx.alerts.join(' '),/Message delivery is unconfirmed/);assert.equal(ctx.feature.promptText(),'new draft');assert.match(ctx.roots.attstage.innerHTML,/saved\.txt/);assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();
});

test('People stage removal fails closed when pending publication state is unreadable',async()=>{
 let unavailable=false, removals=0;
 const store={list:async()=>[],save:async()=>{},remove:async()=>{removals++}};
 const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true,attachments:true}),peopleFileStore:store,pendingPeople:()=>{if(unavailable)throw Error('synthetic pending storage failure');return null}},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:'0123456789abcdef'}]})})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.addFiles([{name:'review.txt',type:'text/plain',size:1}]);await new Promise(resolve=>setImmediate(resolve));
 assert.match(ctx.roots.attstage.innerHTML,/review\.txt/);unavailable=true;
 ctx.roots.attstage._stageButtons[0].firstListener('click')();await new Promise(resolve=>setImmediate(resolve));
 assert.match(ctx.alerts.join(' '),/pending People message/i);assert.match(ctx.roots.attstage.innerHTML,/review\.txt/);assert.equal(removals,0);assert.equal(ctx.apiCalls.filter(call=>call.options?.method==='DELETE').length,0);ctx.feature.destroy();
});

test('file-only pending recovery identifies missing files and cannot retry them',async()=>{
 let calls=0;
 const pending={key:'s/o/c/g',instance:'',text:'',attachments:['0123456789abcdef']};
 const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true,attachments:true}),peopleFileStore:{list:async()=>[]},pendingPeople:()=>pending,retryPeople:async()=>{calls++;return {seq:1}}}});
 ctx.feature.bind();ctx.feature.onSelect();await new Promise(resolve=>setImmediate(resolve));
 assert.match(ctx.roots.attstage.innerHTML,/File-only message/);assert.match(ctx.roots.attstage.innerHTML,/Saved file unavailable/);
 ctx.roots.attstage._retryButtons[0].firstListener('click')();await new Promise(resolve=>setImmediate(resolve));
 assert.equal(calls,0);assert.match(ctx.alerts.join(' '),/saved files.*unavailable/i);assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();
});

test('People retry keeps reviewable files when local receipt persistence fails',async()=>{
 let dispatch=0;const f=transactionalPeopleFixture({respond:async()=>{if(++dispatch===1)throw Error('synthetic offline');return {seq:1};}}),ctx=f.ctx;
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('saved caption');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await f.waitReady(1);await ctx.feature.sendPrompt();assert.equal(f.sends.length,1);assert.equal(f.retries.length,0);const snapshot=f.pending();assert.ok(snapshot);f.setDecisionFailure(true);ctx.roots.attstage._retryButtons[0].click();await f.waitRetry(1);await localReviewTick();assert.equal(f.retries.length,1);assert.deepEqual(f.retries[0].request,f.sends[0].request);assert.equal(f.events.filter(event=>event==='finish').length,1);assert.match(ctx.alerts.at(-1),/storage/i);assert.match(ctx.roots.attstage.innerHTML,/saved\.txt/);assert.match(ctx.roots.attstage.innerHTML,/Review unconfirmed/);assert.equal(f.recovery().find(row=>row.kind==='publication').publication,snapshot.publication);assert.equal(f.files()[0].published,undefined);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('People retry reports a known refusal and preserves files for a later explicit retry',async()=>{
 let dispatch=0;const f=transactionalPeopleFixture({respond:async()=>{dispatch++;if(dispatch===1)throw Error('synthetic offline');if(dispatch===2)throw Object.assign(Error('{"error":"relay_quota"}'),{status:507});return {seq:1};}}),ctx=f.ctx;
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('saved');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await f.waitReady(1);await ctx.feature.sendPrompt();const original=f.pending();assert.ok(original);ctx.feature.setPromptText('newer draft');ctx.roots.prompt.firstListener('input')();await f.waitDraft(2);ctx.roots.attstage._retryButtons[0].click();await f.waitRetry(1);await localReviewTick();assert.equal(f.retries.length,1);assert.deepEqual(f.retries[0].request,f.sends[0].request);assert.equal(f.files().length,1);assert.equal(f.files()[0].published,undefined);assert.match(ctx.alerts.join(' '),/quota/i);assert.match(ctx.roots.attstage.innerHTML,/saved\.txt/);assert.equal(ctx.feature.promptText(),'newer draft');ctx.roots.attstage._retryButtons[0].click();await f.waitRetry(2);await localReviewTick();assert.equal(f.retries.length,2);assert.deepEqual(f.retries[1].request,f.sends[0].request);assert.equal(f.files().length,0);assert.equal(f.recovery().find(row=>row.kind==='decision').seq,1);assert.equal(ctx.feature.promptText(),'newer draft');assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);}finally{ctx.feature.destroy();}
});

test('late People retry failure cannot alert or overwrite a newer visit',async()=>{
 let reject,context={key:'s/o/c/g',writable:true,owner:true,attachments:true};
 const pending={key:'s/o/c/g',instance:'',text:'saved',attachments:['0123456789abcdef']};
 const store={list:async()=>[{id:'0123456789abcdef',file:{name:'saved.txt',type:'text/plain',size:1}}],remove:async()=>assert.fail('failed retry must retain files')};
 const ctx=makeFeature({nodes:[{id:'n1'},{id:'n2'}],deps:{peopleContext:()=>context,peopleFileStore:store,pendingPeople:()=>context?pending:null,retryPeople:()=>new Promise((_,fail)=>{reject=fail})}});
 ctx.feature.bind();ctx.feature.onSelect();await new Promise(resolve=>setImmediate(resolve));ctx.roots.attstage._retryButtons[0].firstListener('click')();await new Promise(resolve=>setImmediate(resolve));
 context=null;ctx.setSel('n2');ctx.feature.onSelect();ctx.feature.setPromptText('new visit');reject(Error('old retry failed'));await new Promise(resolve=>setImmediate(resolve));
 assert.equal(ctx.feature.promptText(),'new visit');assert.deepEqual(ctx.alerts,[]);assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);ctx.feature.destroy();
});

test("prepared source association follows logical source files rather than identical unrelated bytes", async()=>{
 const source={server:"s",owner:"o",chat:"c",generation:"g",log:"comments",seq:1};
 let sourceUpdates=0;const ctx=makeFeature({deps:{peopleContext:()=>({key:"s/o/c/g",writable:true,owner:true}),canPreparePrompt:()=>true,promptSourcesChanged:()=>{sourceUpdates++}},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{path:"synthetic-ref"}]})})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);
 assert.equal(ctx.feature.preparePrompt("source text",source,[{name:"source.txt",type:"text/plain",size:1}]),true);
 ctx.feature.addFiles([{name:"other.txt",type:"text/plain",size:1}]);await new Promise(r=>setImmediate(r));
 ctx.feature.setPromptText("");ctx.roots.prompt.firstListener("input")();assert.notEqual(ctx.storage.getItem("scimux-prompt-sources:n1"),null);
 ctx.feature.setPromptText("unrelated new text");ctx.roots.prompt.firstListener("input")();const beforeRemoval=sourceUpdates;ctx.roots.attstage._stageButtons[0].firstListener("click")();await new Promise(r=>setImmediate(r));
 assert.equal(sourceUpdates,beforeRemoval+1,"removing the last source file refreshes source evidence");
 assert.equal(ctx.storage.getItem("scimux-prompt-sources:n1"),null);assert.match(ctx.roots.attstage.innerHTML,/other\.txt/);assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith("/send")).length,0);
});

test("explicit owner preparation stages source files without dispatching an agent", async()=>{
 const source={server:"a".repeat(32),owner:"b".repeat(32),chat:"c".repeat(16),generation:"d".repeat(16),log:"comments",seq:1};
 const key=[source.server,source.owner,source.chat,source.generation].join("/");const descriptor={id:"0123456789abcdef",sha256:"e".repeat(64),size:1,name:"source.txt",mime:"text/plain"};
 let prepared=0;const ctx=makeFeature({deps:{peopleContext:()=>({key,writable:true,owner:true,attachments:true}),canPreparePrompt:()=>true,preparePeopleFiles:async()=>{prepared++;return [{name:"source.txt",type:"text/plain",size:1}]}},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{path:"native-ref"}]})})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);assert.equal(await ctx.feature.prepareSharedPrompt("quoted",source,[descriptor]),true);await new Promise(r=>setImmediate(r));
 assert.equal(prepared,1);assert.equal(ctx.feature.destination(),"agent");assert.equal(ctx.feature.promptText(),"quoted");assert.match(ctx.roots.attstage.innerHTML,/source\.txt/);assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith("/send")).length,0);
});

test('file-only source preparation keeps existing Agent text and its file provenance',async()=>{
 const source={server:'a'.repeat(32),owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),log:'comments',seq:1};
 const descriptor={id:'0123456789abcdef',sha256:'e'.repeat(64),size:1,name:'source.txt',mime:'text/plain'};
 const store={list:async()=>[],save:async()=>{},remove:async()=>{}};
 const ctx=makeFeature({deps:{peopleContext:()=>null,canPreparePrompt:()=>true,peopleFileStore:store,preparePeopleFiles:async()=>[{name:'source.txt',type:'text/plain',size:1}]},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{path:'synthetic-ref'}]})})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);ctx.feature.setPromptText('existing draft');ctx.roots.prompt.firstListener('input')();
 assert.equal(await ctx.feature.prepareSharedPrompt('',source,[descriptor]),true);await new Promise(resolve=>setImmediate(resolve));
 assert.equal(ctx.feature.promptText(),'existing draft');assert.match(ctx.roots.attstage.innerHTML,/source\.txt/);assert.notEqual(ctx.storage.getItem('scimux-prompt-sources:n1'),null);assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);ctx.feature.destroy();
});

test('a failed prepared source upload blocks Send and preserves the source for review',async()=>{
 const source={server:'a'.repeat(32),owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),log:'comments',seq:1};
 const descriptor={id:'0123456789abcdef',sha256:'e'.repeat(64),size:1,name:'source.txt',mime:'text/plain'};
 let fail;const ctx=makeFeature({deps:{peopleContext:()=>null,canPreparePrompt:()=>true,preparePeopleFiles:async()=>[{name:'source.txt',type:'text/plain',size:1}]},uploadHandler:()=>new Promise((_,reject)=>{fail=reject})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);assert.equal(await ctx.feature.prepareSharedPrompt('source caption',source,[descriptor]),true);
 const send=ctx.feature.sendPrompt();fail(Error('synthetic upload failure'));await new Promise(resolve=>setImmediate(resolve));ctx.timers.runIntervals();await send;
 assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);assert.match(ctx.alerts.join(' '),/Review failed or unfinished source files/);assert.match(ctx.roots.attstage.innerHTML,/source\.txt/);assert.notEqual(ctx.storage.getItem('scimux-prompt-sources:n1'),null);ctx.feature.destroy();
});

test('failed removal of a prepared source file preserves its source association',async()=>{
 const source={server:'a'.repeat(32),owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),log:'comments',seq:1};
 const descriptor={id:'0123456789abcdef',sha256:'e'.repeat(64),size:1,name:'source.txt',mime:'text/plain'};
 const store={list:async()=>[],save:async()=>{},remove:async()=>{throw Error('synthetic storage failure')}};
 const ctx=makeFeature({deps:{peopleContext:()=>null,canPreparePrompt:()=>true,peopleFileStore:store,preparePeopleFiles:async()=>[{name:'source.txt',type:'text/plain',size:1}]},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{path:'synthetic-ref'}]})})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);assert.equal(await ctx.feature.prepareSharedPrompt('source caption',source,[descriptor]),true);await new Promise(resolve=>setImmediate(resolve));
 ctx.feature.setPromptText('');ctx.roots.prompt.firstListener('input')();ctx.roots.attstage._stageButtons[0].firstListener('click')();await new Promise(resolve=>setImmediate(resolve));
 assert.match(ctx.alerts.join(' '),/saved file removal failed/i);assert.match(ctx.roots.attstage.innerHTML,/source\.txt/);assert.notEqual(ctx.storage.getItem('scimux-prompt-sources:n1'),null);assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);ctx.feature.destroy();
});

test("late owner file preparation cannot switch destination after newer People editing", async()=>{
 const source={server:"a".repeat(32),owner:"b".repeat(32),chat:"c".repeat(16),generation:"d".repeat(16),log:"comments",seq:1};const key=[source.server,source.owner,source.chat,source.generation].join("/");
 let release;const ctx=makeFeature({deps:{peopleContext:()=>({key,writable:true,owner:true,attachments:true}),canPreparePrompt:()=>true,preparePeopleFiles:()=>new Promise(r=>{release=r})}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);const pending=ctx.feature.prepareSharedPrompt("old",source,[{id:"0123456789abcdef"}]);
 ctx.feature.setPromptText("newer People text");ctx.roots.prompt.firstListener("input")();release([{name:"x.txt",type:"text/plain",size:1}]);
 assert.equal(await pending,false);assert.equal(ctx.feature.destination(),"people");assert.equal(ctx.feature.promptText(),"newer People text");assert.equal(ctx.fetchCalls.length,0);assert.equal(ctx.apiCalls.length,0);
});

test("a received or wrong-generation source cannot prepare files into a local agent", async()=>{
 const source={server:"a".repeat(32),owner:"b".repeat(32),chat:"c".repeat(16),generation:"d".repeat(16),log:"comments",seq:1};let calls=0;
 for (const context of [{key:"other",writable:true,owner:true,attachments:true},{key:[source.server,source.owner,source.chat,source.generation].join("/"),writable:true,owner:false,attachments:true}]) {
  const ctx=makeFeature({deps:{peopleContext:()=>context,canPreparePrompt:()=>true,preparePeopleFiles:async()=>{calls++;return []}}});ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);
  assert.equal(await ctx.feature.prepareSharedPrompt("text",source,[{id:"0123456789abcdef"}]),false);assert.equal(ctx.apiCalls.length,0);
 }
 assert.equal(calls,0);
});

test("retiring a source action cancels its late file preparation", async()=>{
 const source={server:"a".repeat(32),owner:"b".repeat(32),chat:"c".repeat(16),generation:"d".repeat(16),log:"comments",seq:1};const key=[source.server,source.owner,source.chat,source.generation].join("/");let release,active=true;
 const ctx=makeFeature({deps:{peopleContext:()=>({key,writable:true,owner:true,attachments:true}),canPreparePrompt:()=>true,preparePeopleFiles:()=>new Promise(r=>{release=r})}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);const pending=ctx.feature.prepareSharedPrompt("old",source,[{id:"0123456789abcdef"}],()=>active);
 active=false;release([{name:"x.txt",type:"text/plain",size:1}]);assert.equal(await pending,false);assert.equal(ctx.feature.destination(),"people");assert.equal(ctx.fetchCalls.length,0);
});

test("prepared owner files survive reload for the same native instance", async()=>{
 const source={server:"a".repeat(32),owner:"b".repeat(32),chat:"c".repeat(16),generation:"d".repeat(16),log:"comments",seq:1};const key=[source.server,source.owner,source.chat,source.generation].join("/");
 const rows=new Map();const fileStore={save:async(scope,id,file,metadata)=>rows.set(scope+"/"+id,{id,file,metadata}),list:async scope=>[...rows.entries()].filter(([name])=>name.startsWith(scope+"/")).map(([,row])=>row),markPublishedBatch:async(scope,ids)=>{for(const id of ids)rows.get(scope+"/"+id).published=1},remove:async(scope,id)=>rows.delete(scope+"/"+id)};
 const deps={peopleContext:()=>({key,writable:true,owner:true,attachments:true}),canPreparePrompt:()=>true,peopleFileStore:fileStore,preparePeopleFiles:async()=>[{name:"source.txt",type:"text/plain",size:1}]};
 const nodes=[{id:"n1",instance_id:"same"}],ctx=makeFeature({nodes,deps,uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{path:"native-ref"}]})})});ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);
 assert.equal(await ctx.feature.prepareSharedPrompt("text",source,[{id:"fedcba9876543210"}]),true);await new Promise(r=>setImmediate(r));assert.equal(rows.size,1);
 const reload=makeFeature({nodes,storage:ctx.storage,deps,uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{path:"recovered-ref"}]})})});reload.feature.bind();reload.feature.onSelect();reload.feature.setAttachAvail(true);reload.feature.setDestination("agent");await new Promise(r=>setImmediate(r));assert.match(reload.roots.attstage.innerHTML,/source\.txt/);
 const sending=reload.feature.sendPrompt();await new Promise(r=>setImmediate(r));reload.timers.runIntervals();await sending;
 const sends=reload.apiCalls.filter(call=>call.path.split("?")[0].endsWith("/send"));assert.equal(sends.length,1);assert.deepEqual(JSON.parse(sends[0].opts.body).attachments,[{path:"recovered-ref"}]);
 assert.equal(rows.size,0,"accepted source file must not be offered again after reload");
});

test("Reload cannot associate unrelated text with vanished source-derived files", async()=>{
 const storage=makeStorage(),nodes=[{id:"n1",instance_id:"1".repeat(32)}];
 const source={server:"a".repeat(32),owner:"b".repeat(32),chat:"c".repeat(16),generation:"d".repeat(16),log:"comments",seq:1};
 const key=[source.server,source.owner,source.chat,source.generation].join("/");
 const deps={peopleContext:()=>({key,writable:true,owner:true}),canPreparePrompt:()=>true};
 const first=makeFeature({storage,nodes,deps,recovery:true,uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{path:"synthetic-ref"}]})})});
 first.feature.bind();first.feature.onSelect();first.feature.setAttachAvail(true);
 assert.equal(first.feature.preparePrompt("source caption",source,[{name:"source.txt",type:"text/plain",size:1}]),true);await new Promise(r=>setImmediate(r));
 first.feature.setPromptText("");first.roots.prompt.firstListener("input")();assert.notEqual(storage.getItem("scimux-prompt-sources:n1"),null);first.feature.destroy();
 let releaseSend;const restarted=makeFeature({storage,nodes,deps,recovery:true,sendHandler:()=>new Promise(resolve=>{releaseSend=resolve})});restarted.feature.bind();restarted.feature.onSelect();restarted.feature.setAttachAvail(true);restarted.feature.setDestination("agent");
 restarted.feature.setPromptText("unrelated new text");restarted.roots.prompt.firstListener("input")();
 const recoveredFile=!restarted.roots.attstage.hidden && restarted.roots.attstage.innerHTML.includes("source.txt");
 const explicitRecovery=!restarted.roots.recovery.hidden;
 if (!recoveredFile) {
  const pending=restarted.feature.sendPrompt();
  const flights=JSON.parse(storage.getItem("scimux-prompt-flights:n1") || "[]");
  if (typeof releaseSend === "function") {
   releaseSend({delivery:"ok"});await pending;
   assert.ok(flights.every(flight=>!flight.sources?.length),"unrelated Agent send inherited the source of vanished files and removed source text after reload");
  } else {await pending;assert.ok(explicitRecovery,"missing source content must be recovered, disassociated or explicitly blocked for review");}
 }
 restarted.feature.destroy();
});

test("empty source recovery removes only cleared associations before later sends", async()=>{
 const storage=makeStorage(),nodes=[{id:"n1",instance_id:"1".repeat(32)}],source={server:"a".repeat(32),owner:"b".repeat(32),chat:"c".repeat(16),generation:"d".repeat(16),log:"comments",seq:1};
 const key=[source.server,source.owner,source.chat,source.generation].join("/");const deps={peopleContext:()=>({key,writable:true,owner:true}),canPreparePrompt:()=>true};
 const first=makeFeature({storage,nodes,deps});first.feature.bind();first.feature.onSelect();first.feature.setAttachAvail(true);first.feature.preparePrompt("source",source,[{name:"x",type:"text/plain",size:1}]);first.feature.setPromptText("");first.roots.prompt.firstListener("input")();first.feature.destroy();
 const next=makeFeature({storage,nodes,deps,recovery:true});next.feature.bind();next.feature.onSelect();next.feature.setDestination("agent");await new Promise(r=>setImmediate(r));await new Promise(r=>setImmediate(r));
 assert.equal(storage.getItem("scimux-prompt-sources:n1"),null);next.feature.setPromptText("unrelated");next.roots.prompt.firstListener("input")();await next.feature.sendPrompt();
 const flights=JSON.parse(storage.getItem("scimux-prompt-flights:n1")||"[]");assert.ok(flights.every(flight=>flight.sources.length===0));
});

test("People staged files are isolated when the server configuration binding changes", async()=>{
 let binding="a".repeat(64);const ctx=makeFeature({deps:{peopleContext:()=>({key:"s/o/c/g",binding,writable:true,owner:true,attachments:true})},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:"0123456789abcdef"}]})})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.addFiles([{name:"old.txt",type:"text/plain",size:1}]);await new Promise(r=>setImmediate(r));assert.match(ctx.roots.attstage.innerHTML,/old\.txt/);
 binding="b".repeat(64);ctx.feature.contextChanged();assert.doesNotMatch(ctx.roots.attstage.innerHTML,/old\.txt/);
});

test("People text drafts follow the server binding and return when it is restored",()=>{
 let binding="a".repeat(64);const ctx=makeFeature({deps:{peopleContext:()=>({key:"s/o/c/g",binding,writable:true,owner:true,attachments:true})}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText("old server draft");ctx.roots.prompt.firstListener("input")();
 binding="b".repeat(64);ctx.feature.contextChanged();assert.equal(ctx.feature.promptText(),"");ctx.feature.setPromptText("new server draft");ctx.roots.prompt.firstListener("input")();
 binding="a".repeat(64);ctx.feature.contextChanged();assert.equal(ctx.feature.promptText(),"old server draft");
});

test("damaged source recovery metadata blocks an unrelated Agent send",async()=>{
 const source={server:"a".repeat(32),owner:"b".repeat(32),chat:"c".repeat(16),generation:"d".repeat(16),log:"comments",seq:1};
 const storage=makeStorage({"scimux-draft-owner:n1":"1".repeat(32),"scimux-prompt-sources:n1":JSON.stringify([source]),["scimux-cleared-source-text:n1\0"+"1".repeat(32)]:"{damaged"});
 const ctx=makeFeature({storage,nodes:[{id:"n1",instance_id:"1".repeat(32)}],recovery:true});ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setDestination("agent");
 ctx.feature.setPromptText("unrelated");ctx.roots.prompt.firstListener("input")();await ctx.feature.sendPrompt();
 assert.equal(ctx.apiCalls.filter(call=>call.path.split("?")[0].endsWith("/send")).length,0);assert.equal(ctx.roots.recovery.hidden,false);
});

test('legacy unbound People draft remains reviewable after capability binding appears',()=>{
 const storage=makeStorage({'scimux-people-draft:n1':'old discussion'});
 const ctx=makeFeature({storage,recovery:true,deps:{peopleContext:()=>({key:'s/o/c/g',binding:'a'.repeat(64),writable:true,owner:true,attachments:true})}});
 ctx.feature.bind();ctx.feature.onSelect();
 assert.equal(ctx.feature.promptText(),'');
 assert.equal(ctx.roots.recovery.hidden,false);
 assert.match(ctx.roots.recovery.innerHTML,/old discussion/);
 assert.equal(storage.getItem('scimux-people-draft:n1'),'old discussion');
 assert.equal(ctx.apiCalls.length,0);
});

test('invalid cleared-source metadata blocks unrelated sends without discarding the draft',async()=>{
 const source={server:'s',owner:'o',chat:'c',generation:'g',log:'comments',seq:1},identity='n1\0'+'1'.repeat(32);
 for(const invalid of ['{}','[1]']) {
  const storage=makeStorage({'scimux-draft-owner:n1':'1'.repeat(32),'scimux-prompt-sources:n1':JSON.stringify([source]),['scimux-cleared-source-text:'+identity]:invalid});
  const ctx=makeFeature({storage,nodes:[{id:'n1',instance_id:'1'.repeat(32)}],recovery:true});ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setDestination('agent');ctx.feature.setPromptText('new draft');ctx.roots.prompt.firstListener('input')();await ctx.feature.sendPrompt();
  assert.equal(ctx.apiCalls.filter(call=>call.path.split('?')[0].endsWith('/send')).length,0);assert.equal(ctx.feature.promptText(),'new draft');assert.equal(storage.getItem('scimux-cleared-source-text:'+identity),invalid);assert.equal(ctx.roots.recovery.hidden,false);ctx.feature.destroy();
 }
});

test('a People draft saved before copy identities stays reviewable',()=>{
 const binding='a'.repeat(64),oldScope='s/o/c/g\0binding:'+binding+'\0n1\0owner-instance';
 const key='scimux-people-draft:n1:scope:'+encodeURIComponent(oldScope),storage=makeStorage({[key]:'older scoped discussion'});
 const ctx=makeFeature({storage,recovery:true,nodes:[{id:'n1',instance_id:'owner-instance'}],deps:{peopleContext:()=>({key:'s/o/c/g',binding,copyInstance:'b'.repeat(32),writable:true,owner:true,attachments:true})}});
 ctx.feature.bind();ctx.feature.onSelect();
 assert.equal(ctx.feature.promptText(),'');assert.equal(ctx.roots.recovery.hidden,false);assert.match(ctx.roots.recovery.innerHTML,/older scoped discussion/);assert.equal(storage.getItem(key),'older scoped discussion');assert.equal(ctx.apiCalls.length,0);
});

test('an unresolved People publication from before copy identities stays reviewable',()=>{
 const binding='a'.repeat(64),key='scimux-people-file-pending:s/o/c/g:owner-instance:'+binding;
 const pending={instance:'owner-instance',request:{text:'unconfirmed older publication',anchor:0,key:'0123456789abcdef',attachments:['fedcba9876543210']}};
 const storage=makeStorage({[key]:JSON.stringify(pending)});
 const ctx=makeFeature({storage,recovery:true,nodes:[{id:'n1',instance_id:'owner-instance'}],deps:{peopleContext:()=>({key:'s/o/c/g',binding,copyInstance:'b'.repeat(32),writable:true,owner:true,attachments:true})}});
 ctx.feature.bind();ctx.feature.onSelect();assert.equal(ctx.roots.recovery.hidden,false);assert.match(ctx.roots.recovery.innerHTML,/unconfirmed older publication/);assert.match(ctx.roots.recovery.innerHTML,/unconfirmed/i);assert.equal(storage.getItem(key),JSON.stringify(pending));assert.equal(ctx.apiCalls.length,0);
});

test('a staged People file from before copy identities remains reviewable without upload',async()=>{
 const binding='a'.repeat(64),oldScope='people\0s/o/c/g\0binding:'+binding+'\0n1\0owner-instance';
 const store={list:async scope=>scope===oldScope?[{id:'0123456789abcdef',file:{name:'saved-synthetic.txt',type:'text/plain',size:1}}]:[]};
 const ctx=makeFeature({recovery:true,nodes:[{id:'n1',instance_id:'owner-instance'}],deps:{peopleFileStore:store,peopleContext:()=>({key:'s/o/c/g',binding,copyInstance:'b'.repeat(32),writable:true,owner:true,attachments:true})}});
 ctx.feature.bind();ctx.feature.onSelect();await new Promise(resolve=>setImmediate(resolve));
 assert.equal(ctx.roots.recovery.hidden,false);assert.match(ctx.roots.recovery.innerHTML,/saved-synthetic\.txt/);assert.match(ctx.roots.recovery.innerHTML,/download/i);
 assert.equal(ctx.fetchCalls.length,0);assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();assert.ok(ctx.revoked.includes('blob:saved-synthetic.txt'));
});

test('retired file review is cleared when its current copy identity disappears',async()=>{
 const binding='a'.repeat(64),oldScope='people\0s/o/c/g\0binding:'+binding+'\0n1\0owner-instance';let copyInstance='b'.repeat(32);
 const store={list:async scope=>scope===oldScope?[{id:'0123456789abcdef',file:{name:'saved-synthetic.txt',type:'text/plain',size:1}}]:[]};
 const ctx=makeFeature({recovery:true,nodes:[{id:'n1',instance_id:'owner-instance'}],deps:{peopleFileStore:store,peopleContext:()=>({key:'s/o/c/g',binding,copyInstance,writable:true,owner:true,attachments:!!copyInstance})}});
 ctx.feature.bind();ctx.feature.onSelect();await new Promise(resolve=>setImmediate(resolve));assert.match(ctx.roots.recovery.innerHTML,/saved-synthetic\.txt/);
 copyInstance='';ctx.feature.contextChanged();assert.doesNotMatch(ctx.roots.recovery.innerHTML,/saved-synthetic\.txt/);assert.ok(ctx.revoked.includes('blob:saved-synthetic.txt'));ctx.feature.destroy();
});

test('a replacement copy exposes earlier scoped files only for manual review',async()=>{
 const binding='a'.repeat(64),copyInstance='b'.repeat(32),prefix='people\0s/o/c/g\0binding:'+binding,owner='n1\0owner-instance',current=prefix+'\0copy:'+copyInstance+'\0'+owner;let requested;
 const store={list:async()=>[],listRetired:async(...args)=>{requested=args;return [{scope:prefix+'\0copy:'+'c'.repeat(32)+'\0'+owner,id:'0123456789abcdef',file:{name:'earlier.txt',type:'text/plain',size:1}}]}};
 const ctx=makeFeature({recovery:true,nodes:[{id:'n1',instance_id:'owner-instance'}],deps:{peopleFileStore:store,peopleContext:()=>({key:'s/o/c/g',binding,copyInstance,writable:true,owner:true,attachments:true})}});
 ctx.feature.bind();ctx.feature.onSelect();await new Promise(resolve=>setImmediate(resolve));
 assert.deepEqual(requested,[prefix,owner,current]);assert.match(ctx.roots.recovery.innerHTML,/Download earlier\.txt/);assert.equal(ctx.fetchCalls.length,0);assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();assert.ok(ctx.revoked.includes('blob:earlier.txt'));
});

test('a same-channel replacement switches to its own People draft',async()=>{
 const binding='a'.repeat(64);let copyInstance='b'.repeat(32);
 const ctx=makeFeature({recovery:true,nodes:[{id:'n1',instance_id:'owner-instance'}],deps:{peopleContext:()=>({key:'s/o/c/g',binding,copyInstance,writable:true,owner:true,attachments:true})}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('earlier draft');ctx.roots.prompt.firstListener('input')();
 copyInstance='c'.repeat(32);ctx.feature.contextChanged();assert.equal(ctx.feature.promptText(),'');ctx.feature.setPromptText('replacement draft');ctx.roots.prompt.firstListener('input')();
 copyInstance='b'.repeat(32);ctx.feature.contextChanged();assert.equal(ctx.feature.promptText(),'earlier draft');assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();
});

test('an earlier copy pending publication is reviewable without replay',async()=>{
 const binding='a'.repeat(64),copyInstance='b'.repeat(32),oldCopy='c'.repeat(32),prefix='people\0s/o/c/g\0binding:'+binding,owner='n1\0owner-instance';
 const key='scimux-people-file-pending:s/o/c/g:owner-instance:'+binding+':copy:'+oldCopy,pending={instance:'owner-instance',copyInstance:oldCopy,request:{text:'earlier pending text',anchor:0,key:'0123456789abcdef',attachments:['fedcba9876543210']}};
 const storage=makeStorage({[key]:JSON.stringify(pending)}),store={list:async()=>[],listRetired:async()=>[{scope:prefix+'\0copy:'+oldCopy+'\0'+owner,id:'fedcba9876543210',file:{name:'earlier.txt',type:'text/plain',size:1}}]};
 const ctx=makeFeature({storage,recovery:true,nodes:[{id:'n1',instance_id:'owner-instance'}],deps:{peopleFileStore:store,peopleContext:()=>({key:'s/o/c/g',binding,copyInstance,writable:true,owner:true,attachments:true})}});
 ctx.feature.bind();ctx.feature.onSelect();await new Promise(resolve=>setImmediate(resolve));
 assert.match(ctx.roots.recovery.innerHTML,/earlier pending text/);assert.match(ctx.roots.recovery.innerHTML,/unconfirmed/i);assert.equal(storage.getItem(key),JSON.stringify(pending));assert.equal(ctx.fetchCalls.length,0);assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();
});

test('an earlier copy text draft and unconfirmed publication remain reviewable without file rows',async()=>{
 const binding='a'.repeat(64),oldCopy='c'.repeat(32),copyInstance='b'.repeat(32),owner='n1\0owner-instance';
 const draftKey='scimux-people-draft:n1:scope:'+encodeURIComponent('s/o/c/g\0binding:'+binding+'\0copy:'+oldCopy+'\0'+owner);
 const pendingKey='scimux-people-file-pending:s/o/c/g:owner-instance:'+binding+':copy:'+oldCopy;
 const pending=JSON.stringify({instance:'owner-instance',copyInstance:oldCopy,request:{text:'unconfirmed prior message',attachments:['fedcba9876543210']}});
 const storage=makeStorage({[draftKey]:'old text draft',[pendingKey]:pending});
 const ctx=makeFeature({storage,recovery:true,nodes:[{id:'n1',instance_id:'owner-instance'}],deps:{peopleFileStore:{list:async()=>[],listRetired:async()=>[]},peopleContext:()=>({key:'s/o/c/g',binding,copyInstance,writable:true,owner:true,attachments:true})}});
 ctx.feature.bind();ctx.feature.onSelect();await new Promise(resolve=>setImmediate(resolve));
 assert.equal(ctx.feature.promptText(),'');assert.match(ctx.roots.recovery.innerHTML,/old text draft/);assert.match(ctx.roots.recovery.innerHTML,/unconfirmed prior message/);assert.match(ctx.roots.recovery.innerHTML,/unconfirmed/i);
 assert.equal(storage.getItem(draftKey),'old text draft');assert.equal(storage.getItem(pendingKey),pending);assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();
});

test('an earlier copy text draft remains reviewable when file storage is unavailable',async()=>{
 const binding='a'.repeat(64),oldCopy='c'.repeat(32),copyInstance='b'.repeat(32),owner='n1\0owner-instance';
 const key='scimux-people-draft:n1:scope:'+encodeURIComponent('s/o/c/g\0binding:'+binding+'\0copy:'+oldCopy+'\0'+owner),storage=makeStorage({[key]:'saved discussion without file store'});
 const ctx=makeFeature({storage,recovery:true,nodes:[{id:'n1',instance_id:'owner-instance'}],deps:{peopleFileStore:null,peopleContext:()=>({key:'s/o/c/g',binding,copyInstance,writable:true,owner:true,attachments:true})}});
 ctx.feature.bind();ctx.feature.onSelect();await new Promise(resolve=>setImmediate(resolve));
 assert.match(ctx.roots.recovery.innerHTML,/saved discussion without file store/);assert.match(ctx.roots.recovery.innerHTML,/could not be reviewed/);assert.equal(storage.getItem(key),'saved discussion without file store');assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();
});

test('an earlier copy text draft remains reviewable when file storage read fails',async()=>{
 const binding='a'.repeat(64),oldCopy='c'.repeat(32),copyInstance='b'.repeat(32),owner='n1\0owner-instance';
 const key='scimux-people-draft:n1:scope:'+encodeURIComponent('s/o/c/g\0binding:'+binding+'\0copy:'+oldCopy+'\0'+owner),storage=makeStorage({[key]:'saved discussion after storage error'});
 const ctx=makeFeature({storage,recovery:true,nodes:[{id:'n1',instance_id:'owner-instance'}],deps:{peopleFileStore:{list:async()=>[],listRetired:async()=>{throw Error('synthetic unavailable')}},peopleContext:()=>({key:'s/o/c/g',binding,copyInstance,writable:true,owner:true,attachments:true})}});
 ctx.feature.bind();ctx.feature.onSelect();await new Promise(resolve=>setImmediate(resolve));
 assert.match(ctx.roots.recovery.innerHTML,/saved discussion after storage error/);assert.match(ctx.roots.recovery.innerHTML,/could not be reviewed/);assert.equal(storage.getItem(key),'saved discussion after storage error');assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();
});

test('a late retired-file storage failure cannot change a newer chat recovery view',async()=>{
 let reject,selected='n1';const pending=new Promise((_resolve,fail)=>{reject=fail});
 const ctx=makeFeature({recovery:true,nodes:[{id:'n1',instance_id:'one'},{id:'n2',instance_id:'two'}],deps:{peopleFileStore:{list:async()=>[],listRetired:async prefix=>prefix.includes('first')?pending:[]},peopleContext:()=>({key:selected==='n1'?'first':'second',binding:'a'.repeat(64),copyInstance:'b'.repeat(32),writable:true,owner:true,attachments:true})}});
 ctx.feature.bind();ctx.feature.onSelect();await new Promise(resolve=>setImmediate(resolve));selected='n2';ctx.setSel('n2');ctx.feature.onSelect();await new Promise(resolve=>setImmediate(resolve));
 reject(Error('synthetic unavailable'));await new Promise(resolve=>setImmediate(resolve));assert.doesNotMatch(ctx.roots.recovery.innerHTML,/could not be reviewed/);assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();
});

test('a damaged earlier pending message stays stored and is labelled unavailable',async()=>{
 const binding='a'.repeat(64),oldCopy='c'.repeat(32),copyInstance='b'.repeat(32),key='scimux-people-file-pending:s/o/c/g:owner-instance:'+binding+':copy:'+oldCopy;
 const storage=makeStorage({[key]:'{damaged'}),ctx=makeFeature({storage,recovery:true,nodes:[{id:'n1',instance_id:'owner-instance'}],deps:{peopleFileStore:{list:async()=>[],listRetired:async()=>[]},peopleContext:()=>({key:'s/o/c/g',binding,copyInstance,writable:true,owner:true,attachments:true})}});
 ctx.feature.bind();ctx.feature.onSelect();await new Promise(resolve=>setImmediate(resolve));assert.match(ctx.roots.recovery.innerHTML,/unavailable/i);assert.match(ctx.roots.recovery.innerHTML,/unconfirmed/i);assert.equal(storage.getItem(key),'{damaged');assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();
});

test('a retired file-only pending message remains reviewable without replay',async()=>{
 const binding='a'.repeat(64),oldCopy='c'.repeat(32),copyInstance='b'.repeat(32),key='scimux-people-file-pending:s/o/c/g:owner-instance:'+binding+':copy:'+oldCopy;
 const pending=JSON.stringify({instance:'owner-instance',copyInstance:oldCopy,request:{text:'',attachments:['0123456789abcdef']}}),storage=makeStorage({[key]:pending});
 const ctx=makeFeature({storage,recovery:true,nodes:[{id:'n1',instance_id:'owner-instance'}],deps:{peopleFileStore:{list:async()=>[],listRetired:async()=>[]},peopleContext:()=>({key:'s/o/c/g',binding,copyInstance,writable:true,owner:true,attachments:true})}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('new draft');await new Promise(resolve=>setImmediate(resolve));
 assert.match(ctx.roots.recovery.innerHTML,/File-only People message/);assert.match(ctx.roots.recovery.innerHTML,/unconfirmed/i);assert.equal(storage.getItem(key),pending);assert.equal(ctx.feature.promptText(),'new draft');assert.equal(ctx.apiCalls.length,0);assert.equal(ctx.fetchCalls.length,0);ctx.feature.destroy();
});

test('recipient reload offers an earlier copy pending message for review without replay',async()=>{
 const binding='a'.repeat(64),old='c'.repeat(32),current='b'.repeat(32),key='scimux-people-file-pending:s/o/c/g::'+binding+':copy:'+old;
 const pending=JSON.stringify({instance:'',copyInstance:old,request:{text:'saved recipient caption',attachments:['0123456789abcdef']}}),storage=makeStorage({[key]:pending});
 const ctx=makeFeature({sel:'team/s/o/c/g',nodes:[],storage,recovery:true,deps:{peopleFileStore:{list:async()=>[],listRetired:async()=>[]},peopleContext:()=>({key:'s/o/c/g',binding,copyInstance:current,writable:true,owner:false,attachments:true})}});
 ctx.feature.bind();ctx.feature.onSelect();await new Promise(resolve=>setImmediate(resolve));
 const assertRetained=()=>{assert.match(ctx.roots.recovery.innerHTML,/saved recipient caption/);assert.match(ctx.roots.recovery.innerHTML,/unconfirmed/i);assert.equal(storage.getItem(key),pending);assert.equal(ctx.apiCalls.length,0);assert.equal(ctx.fetchCalls.length,0)};
 assertRetained();ctx.feature.contextChanged();await new Promise(resolve=>setImmediate(resolve));assertRetained();ctx.feature.destroy();
});

test('recipient can review an earlier pending copy when the server binding is unavailable',async()=>{
 const old='c'.repeat(32),current='b'.repeat(32),key='scimux-people-file-pending:s/o/c/g::copy:'+old;
 const pending=JSON.stringify({instance:'',copyInstance:old,request:{text:'saved offline caption',attachments:[]}}),storage=makeStorage({[key]:pending});
 const ctx=makeFeature({sel:'team/s/o/c/g',nodes:[],storage,recovery:true,deps:{peopleFileStore:{list:async()=>[],listRetired:async()=>[]},peopleContext:()=>({key:'s/o/c/g',copyInstance:current,writable:false,owner:false,attachments:false})}});
 ctx.feature.bind();ctx.feature.onSelect();await new Promise(resolve=>setImmediate(resolve));
 assert.match(ctx.roots.recovery.innerHTML,/saved offline caption/);assert.match(ctx.roots.recovery.innerHTML,/unconfirmed/i);assert.equal(storage.getItem(key),pending);assert.equal(ctx.apiCalls.length,0);assert.equal(ctx.fetchCalls.length,0);ctx.feature.destroy();
});

test('unchanged People context refresh preserves retired-file review without reloading it',async()=>{
 const binding='a'.repeat(64),oldCopy='c'.repeat(32),copyInstance='b'.repeat(32),owner='n1\0owner-instance',prefix='people\0s/o/c/g\0binding:'+binding;let reads=0;
 const store={list:async()=>[],listRetired:async()=>{reads++;return [{scope:prefix+'\0copy:'+oldCopy+'\0'+owner,id:'0123456789abcdef',file:{name:'earlier.txt',type:'text/plain',size:1}}]}};
 const ctx=makeFeature({recovery:true,nodes:[{id:'n1',instance_id:'owner-instance'}],deps:{peopleFileStore:store,peopleContext:()=>({key:'s/o/c/g',binding,copyInstance,writable:true,owner:true,attachments:true})}});
 ctx.feature.bind();ctx.feature.onSelect();await new Promise(resolve=>setImmediate(resolve));assert.equal(reads,1);assert.match(ctx.roots.recovery.innerHTML,/earlier\.txt/);
 ctx.feature.contextChanged();ctx.feature.contextChanged();await new Promise(resolve=>setImmediate(resolve));assert.equal(reads,1);assert.match(ctx.roots.recovery.innerHTML,/earlier\.txt/);assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();
});

test('People file recovery reports a storage failure without sending or clearing the draft',async()=>{
 const store={list:async()=>{throw Error('synthetic unavailable')},listRetired:async()=>[]};
 const ctx=makeFeature({nodes:[{id:'n1',instance_id:'owner-instance'}],deps:{peopleFileStore:store,peopleContext:()=>({key:'s/o/c/g',binding:'a'.repeat(64),copyInstance:'b'.repeat(32),writable:true,owner:true,attachments:true})}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setDestination('people');ctx.feature.setPromptText('keep this draft');
 await new Promise(resolve=>setImmediate(resolve));
 assert.match(ctx.alerts.join(' '),/Saved People files could not be recovered/);assert.equal(ctx.feature.promptText(),'keep this draft');assert.equal(ctx.apiCalls.length,0);assert.equal(ctx.fetchCalls.length,0);ctx.feature.destroy();
});

test('saved sent People images restore as cleanup-only tiles without dispatch',async()=>{
 const store={list:async()=>[{id:'0123456789abcdef',stageRevision:'a'.repeat(64),file:{name:'saved.png',type:'image/png',size:1},published:1}],listRetired:async()=>[]};
 const ctx=makeFeature({nodes:[{id:'n1',instance_id:'owner-instance'}],deps:{peopleFileStore:store,peopleContext:()=>({key:'s/o/c/g',binding:'a'.repeat(64),copyInstance:'b'.repeat(32),writable:true,owner:true,attachments:true})}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setDestination('people');await new Promise(resolve=>setImmediate(resolve));
 assert.match(ctx.roots.attstage.innerHTML,/saved\.png/);assert.match(ctx.roots.attstage.innerHTML,/Sent; remove this saved file/);assert.equal(ctx.apiCalls.length,0);assert.equal(ctx.fetchCalls.length,0);ctx.feature.destroy();
});

test('recipient reload restores a staged People file without owner identity or dispatch',async()=>{
 const store={list:async()=>[{id:'0123456789abcdef',file:{name:'recipient.txt',type:'text/plain',size:1}}],listRetired:async()=>[]};
 const ctx=makeFeature({sel:'team/s/o/c/g',nodes:[],deps:{peopleFileStore:store,peopleContext:()=>({key:'s/o/c/g',binding:'a'.repeat(64),copyInstance:'b'.repeat(32),writable:true,owner:false,attachments:true})}});
 ctx.feature.bind();ctx.feature.onSelect();await new Promise(resolve=>setImmediate(resolve));
 assert.match(ctx.roots.attstage.innerHTML,/recipient\.txt/);assert.match(ctx.roots.attstage.innerHTML,/Review and send to retry/);assert.equal(ctx.apiCalls.length,0);assert.equal(ctx.fetchCalls.length,0);ctx.feature.destroy();
});

test('retired People recovery reports a failed draft enumeration without dispatch',async()=>{
 const storage=makeStorage();storage.keys=()=>{throw Error('synthetic enumeration failure')};
 const ctx=makeFeature({storage,recovery:true,nodes:[{id:'n1',instance_id:'owner-instance'}],deps:{peopleFileStore:{list:async()=>[],listRetired:async()=>[]},peopleContext:()=>({key:'s/o/c/g',binding:'a'.repeat(64),copyInstance:'b'.repeat(32),writable:true,owner:true,attachments:true})}});
 ctx.feature.bind();ctx.feature.onSelect();await new Promise(resolve=>setImmediate(resolve));
 assert.match(ctx.roots.recovery.innerHTML,/could not be reviewed/i);assert.equal(ctx.apiCalls.length,0);assert.equal(ctx.fetchCalls.length,0);ctx.feature.destroy();
});

test('retired draft discovery refuses unrelated bindings and reports unavailable enumeration',async()=>{
 const binding='a'.repeat(64),other='d'.repeat(64),old='c'.repeat(32),current='b'.repeat(32),owner='n1\0owner-instance';
 const key='scimux-people-draft:n1:scope:'+encodeURIComponent('s/o/c/g\0binding:'+other+'\0copy:'+old+'\0'+owner);
 const storage=makeStorage({[key]:'other server private draft','scimux-people-draft:n1:scope:%bad':'damaged'});
 const deps={peopleFileStore:{list:async()=>[],listRetired:async()=>[]},peopleContext:()=>({key:'s/o/c/g',binding,copyInstance:current,writable:true,owner:true,attachments:true})};
 const ctx=makeFeature({storage,recovery:true,nodes:[{id:'n1',instance_id:'owner-instance'}],deps});
 ctx.feature.bind();ctx.feature.onSelect();await new Promise(resolve=>setImmediate(resolve));
 assert.doesNotMatch(ctx.roots.recovery.innerHTML,/other server private draft/);ctx.feature.destroy();
 storage.keys=()=>null;
 const retry=makeFeature({storage,recovery:true,nodes:[{id:'n1',instance_id:'owner-instance'}],deps});
 retry.feature.bind();retry.feature.onSelect();await new Promise(resolve=>setImmediate(resolve));
 assert.match(retry.roots.recovery.innerHTML,/could not be reviewed/);assert.equal(retry.apiCalls.length,0);retry.feature.destroy();
});

test('legacy People draft appears for review when capability arrives without replacing newer bound editing',()=>{
 let binding='';
 const storage=makeStorage({'scimux-people-draft:n1':'older <draft>'});
 const ctx=makeFeature({storage,recovery:true,deps:{peopleContext:()=>({key:'s/o/c/g',binding,writable:true,owner:true,attachments:!!binding})}});
 ctx.feature.bind();ctx.feature.onSelect();assert.equal(ctx.feature.promptText(),'');assert.match(ctx.roots.recovery.innerHTML,/older &lt;draft&gt;/);
 binding='a'.repeat(64);ctx.feature.contextChanged();assert.equal(ctx.feature.promptText(),'');
 assert.equal(ctx.roots.recovery.hidden,false);assert.match(ctx.roots.recovery.innerHTML,/older &lt;draft&gt;/);
 ctx.feature.setPromptText('newer');ctx.roots.prompt.firstListener('input')();
 assert.equal(storage.getItem('scimux-people-draft:n1'),'older <draft>');assert.equal(ctx.feature.promptText(),'newer');
});

test('owner file preparation rolls back saved files when its action is retired during persistence',async()=>{
 const source={server:'a'.repeat(32),owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),log:'comments',seq:1};const key=[source.server,source.owner,source.chat,source.generation].join('/');
 let release,active=true;const removed=[];const fileStore={save:()=>new Promise(resolve=>{release=resolve}),remove:async(scope,id)=>removed.push([scope,id]),list:async()=>[]};
 const ctx=makeFeature({deps:{peopleContext:()=>({key,writable:true,owner:true,attachments:true}),canPreparePrompt:()=>true,peopleFileStore:fileStore,preparePeopleFiles:async()=>[{name:'x.txt',type:'text/plain',size:1}]}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);const work=ctx.feature.prepareSharedPrompt('source',source,[{id:'0123456789abcdef'}],()=>active);
 await new Promise(r=>setImmediate(r));active=false;release();assert.equal(await work,false);assert.equal(removed.length,1);assert.equal(ctx.feature.destination(),'people');assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);
});

test('failed source-file recovery leaves owner preparation and existing People draft untouched',async()=>{
 const source={server:'a'.repeat(32),owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),log:'comments',seq:1};const key=[source.server,source.owner,source.chat,source.generation].join('/');
 const ctx=makeFeature({deps:{peopleContext:()=>({key,writable:true,owner:true,attachments:true}),canPreparePrompt:()=>true,peopleFileStore:null,preparePeopleFiles:async()=>[{name:'x.txt',type:'text/plain',size:1}]}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);ctx.feature.setPromptText('newer People draft');ctx.roots.prompt.firstListener('input')();
 assert.equal(await ctx.feature.prepareSharedPrompt('source',source,[{id:'0123456789abcdef'}]),false);assert.equal(ctx.feature.destination(),'people');assert.equal(ctx.feature.promptText(),'newer People draft');assert.match(ctx.alerts.at(-1),/recovery is unavailable/);assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);
});

test('People quota refusal displays actionable feedback while keeping its retry file identity',async()=>{
 let calls=0;const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true,attachments:true})},uploadHandler:async()=>{calls++;return {ok:false,status:507,text:async()=>JSON.stringify({error:'relay_quota',stage_id:'0123456789abcdef'})}}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.addFiles([{name:'synthetic.txt',type:'text/plain',size:1}]);await new Promise(r=>setImmediate(r));
 assert.match(ctx.roots.attstage.innerHTML,/relay.*quota.*full/i);assert.match(ctx.roots.attstage.innerHTML,/synthetic\.txt/);assert.equal(calls,1);assert.match(ctx.fetchCalls[0].path,/stage_id=0123456789abcdef/);assert.equal(ctx.apiCalls.length,0);
});

test('People files reject invalid random identities without disturbing the draft',async()=>{
 for(const value of [null,new Uint8Array(7),[0,1,2,3,4,5,6,256],[0,1,2,3,4,5,6,1.5]]) {
  const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true,attachments:true}),randomBytes:()=>value}});
  ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('keep this draft');ctx.feature.addFiles([{name:'unsafe.txt',type:'text/plain',size:1}]);
  assert.equal(ctx.feature.promptText(),'keep this draft');assert.match(ctx.alerts.join(' '),/file identity is unavailable/i);assert.equal(ctx.fetchCalls.length,0);assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();
 }
 const missing=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true,attachments:true}),randomBytes:undefined}});
 missing.feature.bind();missing.feature.onSelect();missing.feature.setPromptText('keep draft');missing.feature.addFiles([{name:'missing.txt',type:'text/plain',size:1}]);
 assert.match(missing.alerts.join(' '),/file identity is unavailable/i);assert.equal(missing.feature.promptText(),'keep draft');assert.equal(missing.fetchCalls.length,0);missing.feature.destroy();
});

test('People staging enforces the combined byte and file-count limits without discarding accepted files',async()=>{
 for(const files of [
  [...Array.from({length:4},(_,i)=>({name:`large-${i}.txt`,type:'text/plain',size:4*1024*1024})),{name:'extra.txt',type:'text/plain',size:1}],
  Array.from({length:17},(_,i)=>({name:`small-${i}.txt`,type:'text/plain',size:1})),
 ]) {
  const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true,attachments:true})},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:'0123456789abcdef'}]})})});
  ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('keep draft');ctx.feature.addFiles(files);await new Promise(resolve=>setImmediate(resolve));
  assert.equal(ctx.fetchCalls.length,files.length-1);assert.match(ctx.alerts.join(' '),/at most 16 files and 16 MiB total/i);assert.equal(ctx.feature.promptText(),'keep draft');assert.match(ctx.roots.attstage.innerHTML,new RegExp(files[0].name.replace('.','\\.')));assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();
 }
});

test('recipient file staging and removal remain bound to the selected People copy',async()=>{
 const paths=[];
 const ctx=makeFeature({sel:'team/11111111111111111111111111111111/22222222222222222222222222222222/3333333333333333/4444444444444444',nodes:[],deps:{peopleContext:()=>({key:'11111111111111111111111111111111/22222222222222222222222222222222/3333333333333333/4444444444444444',copyInstance:'f'.repeat(32),writable:true,owner:false,attachments:true}),api:async(path,options)=>{paths.push({path,options})}},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:'0123456789abcdef'}]})})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.addFiles([{name:'recipient.txt',type:'text/plain',size:1}]);await new Promise(resolve=>setImmediate(resolve));
 assert.equal(ctx.fetchCalls.length,1);assert.match(ctx.fetchCalls[0].path,/copy_instance=f{32}/);assert.doesNotMatch(ctx.fetchCalls[0].path,/chat_instance=/);
 ctx.roots.attstage._stageButtons[0].firstListener('click')();await new Promise(resolve=>setImmediate(resolve));
 assert.equal(paths.length,1);assert.equal(paths[0].options.method,'DELETE');assert.match(paths[0].path,/copy_instance=f{32}/);assert.doesNotMatch(paths[0].path,/chat_instance=/);assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();
});

test('owner file removal includes the captured native and copy identities',async()=>{
 const calls=[];const instance='a'.repeat(32),copyInstance='f'.repeat(32);
 const ctx=makeFeature({nodes:[{id:'n1',instance_id:instance}],deps:{peopleContext:()=>({key:'11111111111111111111111111111111/22222222222222222222222222222222/3333333333333333/4444444444444444',copyInstance,writable:true,owner:true,attachments:true}),api:async(path,options)=>calls.push({path,options})},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:'0123456789abcdef'}]})})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.addFiles([{name:'owner.txt',type:'text/plain',size:1}]);await new Promise(resolve=>setImmediate(resolve));
 ctx.roots.attstage._stageButtons[0].firstListener('click')();await new Promise(resolve=>setImmediate(resolve));
 assert.equal(calls.length,1);const url=new URL(calls[0].path,'http://local.invalid');assert.equal(calls[0].options.method,'DELETE');assert.equal(url.searchParams.get('copy_instance'),copyInstance);assert.equal(url.searchParams.get('chat_instance'),instance);assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();
});

test('People staging preserves the draft when durable file storage is unavailable',async()=>{
 const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true,attachments:true}),peopleFileStore:null}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('keep this draft');ctx.feature.addFiles([{name:'review.txt',type:'text/plain',size:1}]);await new Promise(resolve=>setImmediate(resolve));
 assert.equal(ctx.feature.promptText(),'keep this draft');assert.match(ctx.roots.attstage.innerHTML,/People file storage is unavailable/);assert.equal(ctx.fetchCalls.length,0);assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();
});

test('a damaged People draft version is repaired on the next edit without sending',async()=>{
 const context={key:localReviewKey,copyInstance:'1'.repeat(32),binding:'current',owner:true,writable:true,attachments:false},f=transactionalPeopleFixture({context}),ctx=f.ctx,scope=`people\0${context.key}\0binding:current\0copy:${context.copyInstance}\0n1\0owner-instance`;
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.storage.setItem('scimux-people-draft:n1:version','synthetic invalid version');ctx.feature.setPromptText('new local edit');ctx.roots.prompt.firstListener('input')();await f.waitDraft(1);const saved=await f.actual.listPeopleRecovery(scope);assert.equal(ctx.storage.getItem('scimux-people-draft:n1:version'),'synthetic invalid version');assert.equal(saved.heads.length,1);assert.equal(saved.heads[0].text,'new local edit');assert.match(saved.heads[0].version,/^[0-9a-f]{32}:[1-9][0-9]*$/);assert.equal(ctx.feature.promptText(),'new local edit');assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('People staging reports a storage failure without an error message',async()=>{
 const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true,attachments:true}),peopleFileStore:{list:async()=>[],save:async()=>{throw null}}}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('keep draft');ctx.feature.addFiles([{name:'review.txt',type:'text/plain',size:1}]);await new Promise(resolve=>setImmediate(resolve));
 assert.equal(ctx.feature.promptText(),'keep draft');assert.match(ctx.roots.attstage.innerHTML,/People file storage is unavailable/);assert.equal(ctx.fetchCalls.length,0);assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();
});

test('an unreadable People upload error remains unconfirmed and never invents a stage reference',async()=>{
 const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true,attachments:true})},uploadHandler:async()=>({ok:false,status:502,statusText:'unavailable',text:async()=>'{synthetic malformed'})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('keep draft');ctx.feature.addFiles([{name:'review.txt',type:'text/plain',size:1}]);await new Promise(resolve=>setImmediate(resolve));
 assert.match(ctx.roots.attstage.innerHTML,/review\.txt/);assert.match(ctx.roots.attstage.innerHTML,/unconfirmed/i);assert.equal(ctx.feature.promptText(),'keep draft');assert.equal(ctx.fetchCalls.length,1);
 ctx.roots.attstage._stageButtons[0].firstListener('click')();await new Promise(resolve=>setImmediate(resolve));assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();
});

test('failed People stage removal preserves the file until an explicit successful retry',async()=>{
 let fail=true,removals=0,localRemovals=0;
 const store={list:async()=>[],save:async()=>{},remove:async()=>{localRemovals++}};
 const ctx=makeFeature({deps:{peopleContext:()=>({key:'11111111111111111111111111111111/22222222222222222222222222222222/3333333333333333/4444444444444444',copyInstance:'f'.repeat(32),writable:true,owner:true,attachments:true}),peopleFileStore:store,api:async(path,options)=>{assert.match(path,/copy_instance=f{32}/);assert.equal(options.method,'DELETE');removals++;if(fail)throw Error('synthetic removal failure')}},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:'0123456789abcdef'}]})})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.addFiles([{name:'retain.txt',type:'text/plain',size:1}]);await new Promise(resolve=>setImmediate(resolve));
 ctx.roots.attstage._stageButtons[0].firstListener('click')();await new Promise(resolve=>setImmediate(resolve));
 assert.match(ctx.alerts.join(' '),/synthetic removal failure/);assert.match(ctx.roots.attstage.innerHTML,/retain\.txt/);assert.equal(localRemovals,0);
 fail=false;ctx.roots.attstage._stageButtons[0].firstListener('click')();await new Promise(resolve=>setImmediate(resolve));
 assert.equal(removals,2);assert.equal(localRemovals,1);assert.doesNotMatch(ctx.roots.attstage.innerHTML,/retain\.txt/);ctx.feature.destroy();
});

test('late People stage removal failure cannot alert or alter another visit',async()=>{
 let reject,context={key:'11111111111111111111111111111111/22222222222222222222222222222222/3333333333333333/4444444444444444',copyInstance:'f'.repeat(32),writable:true,owner:true,attachments:true};
 const store={list:async()=>[],save:async()=>{},remove:async()=>assert.fail('failed server removal must keep local file')};
 const ctx=makeFeature({nodes:[{id:'n1'},{id:'n2'}],deps:{peopleContext:()=>context,peopleFileStore:store,api:()=>new Promise((_,fail)=>{reject=fail})},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:'0123456789abcdef'}]})})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.addFiles([{name:'old.txt',type:'text/plain',size:1}]);await new Promise(resolve=>setImmediate(resolve));
 ctx.roots.attstage._stageButtons[0].firstListener('click')();await new Promise(resolve=>setImmediate(resolve));
 context=null;ctx.setSel('n2');ctx.feature.onSelect();ctx.feature.setPromptText('new visit draft');reject(Error('old failure'));await new Promise(resolve=>setImmediate(resolve));
 assert.equal(ctx.feature.promptText(),'new visit draft');assert.deepEqual(ctx.alerts,[]);assert.doesNotMatch(ctx.roots.attstage.innerHTML,/old\.txt/);ctx.feature.destroy();
});

test('overlapping removal clicks admit one People stage deletion',async()=>{
 let release,calls=0;
 const ctx=makeFeature({deps:{peopleContext:()=>({key:'11111111111111111111111111111111/22222222222222222222222222222222/3333333333333333/4444444444444444',copyInstance:'f'.repeat(32),writable:true,owner:true,attachments:true}),api:()=>{calls++;return new Promise(resolve=>{release=resolve})}},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:'0123456789abcdef'}]})})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.addFiles([{name:'one.txt',type:'text/plain',size:1}]);await new Promise(resolve=>setImmediate(resolve));
 const click=ctx.roots.attstage._stageButtons[0].firstListener('click');click();await new Promise(resolve=>setImmediate(resolve));click();await new Promise(resolve=>setImmediate(resolve));
 assert.equal(calls,1);assert.match(ctx.roots.attstage.innerHTML,/one\.txt/);release({});await new Promise(resolve=>setImmediate(resolve));assert.doesNotMatch(ctx.roots.attstage.innerHTML,/one\.txt/);ctx.feature.destroy();
});

test('unavailable People stage removal leaves its local file reviewable',async()=>{
 let localRemovals=0;
 const store={list:async()=>[],save:async()=>{},remove:async()=>{localRemovals++}};
 const ctx=makeFeature({deps:{peopleContext:()=>({key:'11111111111111111111111111111111/22222222222222222222222222222222/3333333333333333/4444444444444444',copyInstance:'f'.repeat(32),writable:true,owner:true,attachments:true}),peopleFileStore:store,api:undefined},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:'0123456789abcdef'}]})})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.addFiles([{name:'review.txt',type:'text/plain',size:1}]);await new Promise(resolve=>setImmediate(resolve));
 ctx.roots.attstage._stageButtons[0].firstListener('click')();await new Promise(resolve=>setImmediate(resolve));
 assert.match(ctx.alerts.join(' '),/stage removal is unavailable/i);assert.match(ctx.roots.attstage.innerHTML,/review\.txt/);assert.equal(localRemovals,0);assert.equal(ctx.fetchCalls.length,1);ctx.feature.destroy();
});

test('an unavailable pending-message check fails closed before removing a People file',async()=>{
 let removals=0;
 const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',copyInstance:'f'.repeat(32),writable:true,owner:true,attachments:true}),pendingPeople:()=>{throw Error('synthetic pending lookup failure')},api:async()=>{removals++}},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:'0123456789abcdef'}]})})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.addFiles([{name:'review.txt',type:'text/plain',size:1}]);await new Promise(resolve=>setImmediate(resolve));
 ctx.roots.attstage._stageButtons[0].firstListener('click')();await new Promise(resolve=>setImmediate(resolve));
 assert.match(ctx.alerts.join(' '),/part of a pending People message/i);assert.match(ctx.roots.attstage.innerHTML,/review\.txt/);assert.equal(removals,0);ctx.feature.destroy();
});

test('People stage removal keeps the file when the failure has no message',async()=>{
 let localRemovals=0;
 const store={list:async()=>[],save:async()=>{},remove:async()=>{localRemovals++}};
 const ctx=makeFeature({deps:{peopleContext:()=>({key:'11111111111111111111111111111111/22222222222222222222222222222222/3333333333333333/4444444444444444',copyInstance:'f'.repeat(32),writable:true,owner:true,attachments:true}),peopleFileStore:store,api:async()=>{throw null}},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:'0123456789abcdef'}]})})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.addFiles([{name:'review.txt',type:'text/plain',size:1}]);await new Promise(resolve=>setImmediate(resolve));
 ctx.roots.attstage._stageButtons[0].firstListener('click')();await new Promise(resolve=>setImmediate(resolve));
 assert.match(ctx.alerts.join(' '),/Stage removal failed/);assert.match(ctx.roots.attstage.innerHTML,/review\.txt/);assert.equal(localRemovals,0);ctx.feature.destroy();
});

test('an ordinary People file can be removed when no publication is pending',async()=>{
 let serverRemovals=0,localRemovals=0;
 const store={list:async()=>[],save:async()=>{},remove:async()=>{localRemovals++}};
 const ctx=makeFeature({deps:{peopleContext:()=>({key:'11111111111111111111111111111111/22222222222222222222222222222222/3333333333333333/4444444444444444',copyInstance:'f'.repeat(32),writable:true,owner:true,attachments:true}),peopleFileStore:store,pendingPeople:()=>null,api:async()=>{serverRemovals++}},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:'0123456789abcdef'}]})})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.addFiles([{name:'review.txt',type:'text/plain',size:1}]);await new Promise(resolve=>setImmediate(resolve));ctx.roots.attstage._stageButtons[0].firstListener('click')();await new Promise(resolve=>setImmediate(resolve));
 assert.equal(serverRemovals,1);assert.equal(localRemovals,1);assert.doesNotMatch(ctx.roots.attstage.innerHTML,/review\.txt/);assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();
});

test('a text-only pending People message does not pin an unrelated staged file',async()=>{
 let serverRemovals=0,localRemovals=0;
 const store={list:async()=>[],save:async()=>{},remove:async()=>{localRemovals++}};
 const ctx=makeFeature({deps:{peopleContext:()=>({key:'11111111111111111111111111111111/22222222222222222222222222222222/3333333333333333/4444444444444444',copyInstance:'f'.repeat(32),writable:true,owner:true,attachments:true}),peopleFileStore:store,pendingPeople:()=>({text:'earlier caption',attachments:[]}),api:async()=>{serverRemovals++}},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:'0123456789abcdef'}]})})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.addFiles([{name:'new.txt',type:'text/plain',size:1}]);await new Promise(resolve=>setImmediate(resolve));
 ctx.roots.attstage._stageButtons[0].firstListener('click')();await new Promise(resolve=>setImmediate(resolve));
 assert.equal(serverRemovals,1);assert.equal(localRemovals,1);assert.doesNotMatch(ctx.roots.attstage.innerHTML,/new\.txt/);assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();
});

test('attachment staging refuses missing selection and failed draft identity without uploading',()=>{
 const unselected=makeFeature({sel:''});unselected.feature.bind();unselected.feature.onSelect();unselected.feature.setAttachAvail(true);unselected.feature.addFiles([{name:'x.txt',type:'text/plain',size:1}]);assert.equal(unselected.fetchCalls.length,0);
 const storage=makeStorage();storage.setItem=()=>{throw Error('synthetic storage unavailable')};const failed=makeFeature({storage,nodes:[{id:'n1',instance_id:'1'.repeat(32)}]});failed.feature.bind();failed.feature.onSelect();failed.feature.setAttachAvail(true);failed.feature.addFiles([{name:'x.txt',type:'text/plain',size:1}]);assert.equal(failed.fetchCalls.length,0);assert.match(failed.alerts.join(' '),/storage/i);
});

test('prompt preparation refuses ineligible destinations and empty source content without dispatch',()=>{
 const source={server:'a'.repeat(32),owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),log:'comments',seq:1};
 for(const options of [{sel:''},{sel:'team/s/o/c/g'},{nodes:[{id:'n1',ended_at:'2026-01-01'}]},{nodes:[]},{deps:{canPreparePrompt:()=>false}}]) {
  const ctx=makeFeature(options);ctx.feature.bind();ctx.feature.onSelect();assert.equal(ctx.feature.preparePrompt('source',source),false);assert.equal(ctx.apiCalls.length,0);assert.equal(ctx.fetchCalls.length,0);
 }
 const ctx=makeFeature({deps:{canPreparePrompt:()=>true}});ctx.feature.bind();ctx.feature.onSelect();assert.equal(ctx.feature.preparePrompt('',source),false);assert.equal(ctx.feature.preparePrompt('text',{}),false);assert.equal(ctx.feature.preparePrompt('',source,[{name:'x',type:'text/plain',size:1}]),false);assert.equal(ctx.apiCalls.length,0);
});

test('common capture preparation preserves owner authority and draft for text and files',async()=>{
 const source={server:'a'.repeat(32),owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),log:'comments',seq:1};
 for(const attachments of [undefined,[],[{id:'0123456789abcdef'}]]) {
  const ctx=makeFeature({deps:{peopleContext:()=>null,canPreparePrompt:()=>true,preparePeopleFiles:async()=>[{name:'source.txt',type:'text/plain',size:1}]},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{path:'synthetic-native-ref'}]})})});
  ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);
  assert.equal(await ctx.feature.prepareCapturePrompt('quoted',source,attachments,()=>true),true);
  assert.equal(ctx.feature.promptText(),'quoted');assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);
  if(attachments?.length){await new Promise(r=>setImmediate(r));assert.match(ctx.roots.attstage.innerHTML,/source\.txt/)}
  ctx.feature.destroy();
 }
 const denied=makeFeature({deps:{canPreparePrompt:()=>false}});denied.feature.bind();denied.feature.onSelect();
 assert.equal(await denied.feature.prepareCapturePrompt('quoted',source,[],()=>true),false);assert.equal(denied.feature.promptText(),'');assert.equal(denied.apiCalls.length,0);
});

test('common capture preparation cannot apply files after its action is retired',async()=>{
 const source={server:'a'.repeat(32),owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),log:'comments',seq:1};let release,active=true;
 const ctx=makeFeature({deps:{peopleContext:()=>null,canPreparePrompt:()=>true,preparePeopleFiles:()=>new Promise(resolve=>{release=resolve})}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);ctx.feature.setPromptText('newer draft');
 const work=ctx.feature.prepareCapturePrompt('old source',source,[{id:'0123456789abcdef'}],()=>active);
 active=false;release([{name:'old.txt',type:'text/plain',size:1}]);
 assert.equal(await work,false);assert.equal(ctx.feature.promptText(),'newer draft');assert.doesNotMatch(ctx.roots.attstage.innerHTML,/old\.txt/);assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();
});

test('navigation and teardown abort in-flight source file preparation',async()=>{
 const source={server:'a'.repeat(32),owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),log:'comments',seq:1};
 for(const action of ['navigate','destroy']) {
  let release,signal;const ctx=makeFeature({nodes:[{id:'n1'},{id:'n2'}],deps:{peopleContext:()=>null,canPreparePrompt:()=>true,preparePeopleFiles:opts=>{signal=opts.signal;return new Promise(resolve=>{release=resolve})}}});
  ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);const pending=ctx.feature.prepareCapturePrompt('old',source,[{id:'0123456789abcdef'}],()=>true);
  if(action==='navigate'){ctx.setSel('n2');ctx.feature.onSelect();ctx.feature.setPromptText('new draft')}else ctx.feature.destroy();
  assert.equal(signal.aborted,true);release([{name:'old.txt',type:'text/plain',size:1}]);assert.equal(await pending,false);assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);
  if(action==='navigate'){assert.equal(ctx.feature.promptText(),'new draft');assert.doesNotMatch(ctx.roots.attstage.innerHTML,/old\.txt/);ctx.feature.destroy()}
 }
});

test('switching destinations aborts in-flight source preparation without replacing the Agent draft',async()=>{
 const source={server:'a'.repeat(32),owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),log:'comments',seq:1},key=[source.server,source.owner,source.chat,source.generation].join('/');
 let release,signal;const ctx=makeFeature({deps:{peopleContext:()=>({key,owner:true,writable:true,attachments:true}),canPreparePrompt:()=>true,preparePeopleFiles:opts=>{signal=opts.signal;return new Promise(resolve=>{release=resolve})}}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);ctx.feature.setDestination('agent');ctx.feature.setPromptText('existing Agent draft');ctx.roots.prompt.firstListener('input')();ctx.feature.setDestination('people');
 const pending=ctx.feature.prepareCapturePrompt('old source',source,[{id:'0123456789abcdef'}],()=>true);ctx.feature.setDestination('agent');
 assert.equal(signal.aborted,true);release([{name:'old.txt',type:'text/plain',size:1}]);assert.equal(await pending,false);assert.equal(ctx.feature.promptText(),'existing Agent draft');assert.doesNotMatch(ctx.roots.attstage.innerHTML,/old\.txt/);assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();
});

for (const retainedContext of [null, {key:["a".repeat(32),"b".repeat(32),"c".repeat(16),"d".repeat(16)].join("/"),owner:true,writable:false,attachments:true}]) test("retained owner file preparation after share ended " + (retainedContext ? "ended-context" : "no-current-share"), async()=> {
 const source={server:"a".repeat(32),owner:"b".repeat(32),chat:"c".repeat(16),generation:"d".repeat(16),log:"comments",seq:1};
 const descriptor={id:"0123456789abcdef",sha256:"e".repeat(64),size:1,name:"retained.txt",mime:"text/plain"};let prepared=0;
 const ctx=makeFeature({deps:{peopleContext:()=>retainedContext,canPreparePrompt:()=>true,preparePeopleFiles:async()=>{prepared++;return [{name:"retained.txt",type:"text/plain",size:1}]}},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{path:"synthetic-native-ref"}]})})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);
 assert.equal(ctx.feature.preparePrompt("retained text",source),true,"existing retained-text preparation remains permitted by verified owner source adapter");
 assert.equal(await ctx.feature.prepareSharedPrompt("retained file",source,[descriptor]),true,"explicit local owner preparation of retained file must not require People publication permission");
 await new Promise(r=>setImmediate(r));assert.equal(prepared,1);assert.match(ctx.roots.attstage.innerHTML,/retained\.txt/);assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith("/send")).length,0);ctx.feature.destroy();
});

test('retained file preparation cannot outlive the exact owned copy authority',async()=>{
 const source={server:'a'.repeat(32),owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),log:'comments',seq:1};const key=[source.server,source.owner,source.chat,source.generation].join('/');let valid=true,release;
 const ctx=makeFeature({deps:{peopleContext:()=>({key,writable:true,owner:true,attachments:true}),canPreparePrompt:()=>true,capturePromptAuthority:()=>({current:()=>valid}),preparePeopleFiles:()=>new Promise(resolve=>{release=resolve})}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);const pending=ctx.feature.prepareSharedPrompt('source',source,[{id:'0123456789abcdef'}]);valid=false;release([{name:'x.txt',type:'text/plain',size:1}]);
 assert.equal(await pending,false);assert.equal(ctx.feature.destination(),'people');assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);
});

test('owner file preparation requires a currently valid captured authority when provided',async()=>{
 const source={server:'a'.repeat(32),owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),log:'comments',seq:1};let reads=0;
 for(const authority of [null,{}, {current:()=>false}]) {
  const ctx=makeFeature({deps:{peopleContext:()=>null,canPreparePrompt:()=>true,capturePromptAuthority:()=>authority,preparePeopleFiles:async()=>{reads++;return []}}});ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);
  assert.equal(await ctx.feature.prepareSharedPrompt('text',source,[{id:'0123456789abcdef'}]),false);assert.equal(ctx.apiCalls.length,0);
 }
 assert.equal(reads,0);
});

test('failed source metadata preserves reviewable saved files even if rollback fails',async()=>{
 const source={server:'a'.repeat(32),owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),log:'comments',seq:1};const identity='n1\0'+'1'.repeat(32),saved=[];
 const storage=makeStorage({'scimux-draft-owner:n1':'1'.repeat(32),['scimux-cleared-source-text:'+identity]:'{damaged'});
 const fileStore={save:async(_scope,id)=>saved.push(id),list:async()=>[],remove:async()=>{throw Error('synthetic cleanup failure')}};
 const ctx=makeFeature({storage,nodes:[{id:'n1',instance_id:'1'.repeat(32)}],recovery:true,deps:{peopleContext:()=>null,canPreparePrompt:()=>true,peopleFileStore:fileStore,preparePeopleFiles:async()=>[{name:'x.txt',type:'text/plain',size:1}]}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);assert.equal(await ctx.feature.prepareSharedPrompt('text',source,[{id:'0123456789abcdef'}]),false);
 assert.equal(saved.length,1);assert.match(ctx.alerts.join(' '),/damaged|unavailable/i);assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);
});

test('a saved source file remains reviewable when its local removal fails',async()=>{
 const source={server:'a'.repeat(32),owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),log:'comments',seq:1};
 let fail=true,removals=0;
 const store={list:async()=>[],save:async()=>{},remove:async()=>{removals++;if(fail)throw Error('synthetic storage failure')}};
 const ctx=makeFeature({nodes:[{id:'n1',instance_id:'1'.repeat(32)}],deps:{peopleFileStore:store,peopleContext:()=>null,canPreparePrompt:()=>true,preparePeopleFiles:async()=>[{name:'source.txt',type:'text/plain',size:1}]}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);
 assert.equal(await ctx.feature.prepareSharedPrompt('source caption',source,[{id:'0123456789abcdef'}]),true);
 ctx.roots.attstage._stageButtons[0].firstListener('click')();await new Promise(resolve=>setImmediate(resolve));
 assert.match(ctx.alerts.join(' '),/saved file removal failed/i);assert.match(ctx.roots.attstage.innerHTML,/source\.txt/);
 assert.equal(ctx.feature.promptText(),'source caption');assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);
 fail=false;ctx.roots.attstage._stageButtons[0].firstListener('click')();await new Promise(resolve=>setImmediate(resolve));
 assert.equal(removals,2);assert.doesNotMatch(ctx.roots.attstage.innerHTML,/source\.txt/);assert.equal(ctx.feature.promptText(),'source caption');ctx.feature.destroy();
});

test('overlapping source file removals do not remove another staged file',async()=>{
 const source={server:'a'.repeat(32),owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),log:'comments',seq:1};
 const releases=[];const store={list:async()=>[],save:async()=>{},remove:()=>new Promise(resolve=>releases.push(resolve))};
 const ctx=makeFeature({nodes:[{id:'n1',instance_id:'1'.repeat(32)}],deps:{peopleFileStore:store,peopleContext:()=>null,canPreparePrompt:()=>true,preparePeopleFiles:async()=>[{name:'source.txt',type:'text/plain',size:1}]}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);
 assert.equal(await ctx.feature.prepareSharedPrompt('source caption',source,[{id:'0123456789abcdef'}]),true);
 ctx.feature.addFiles([{name:'other.txt',type:'text/plain',size:1}]);
 const click=ctx.roots.attstage._stageButtons[0].firstListener('click');click();click();await new Promise(resolve=>setImmediate(resolve));
 assert.equal(releases.length,2);releases.forEach(resolve=>resolve());await new Promise(resolve=>setImmediate(resolve));
 assert.doesNotMatch(ctx.roots.attstage.innerHTML,/source\.txt/);assert.match(ctx.roots.attstage.innerHTML,/other\.txt/);
 assert.equal(ctx.feature.promptText(),'source caption');assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);ctx.feature.destroy();
});

test('owner file preparation reports a generic failure without exposing an unknown error',async()=>{
 const source={server:'a'.repeat(32),owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),log:'comments',seq:1};
 const ctx=makeFeature({deps:{peopleContext:()=>null,canPreparePrompt:()=>true,preparePeopleFiles:async()=>{throw null}}});ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);
 assert.equal(await ctx.feature.prepareSharedPrompt('text',source,[{id:'0123456789abcdef'}]),false);assert.match(ctx.alerts.join(' '),/source files are unavailable/i);assert.equal(ctx.apiCalls.length,0);
});

test('a source file retained by failed rollback cannot lose provenance after restart',async()=>{
 const source={server:'a'.repeat(32),owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),log:'comments',seq:1};const key=[source.server,source.owner,source.chat,source.generation].join('/');const storage=makeStorage(),nodes=[{id:'n1',instance_id:'1'.repeat(32)}],rows=[];let release,active=true;
 const fileStore={save:async(scope,id,file,metadata)=>{await new Promise(resolve=>{release=resolve});rows.push({scope,id,file,metadata})},list:async scope=>rows.filter(row=>row.scope===scope),remove:async()=>{throw Error('synthetic removal failure')}};
 const deps={peopleContext:()=>({key,writable:true,owner:true,attachments:true}),canPreparePrompt:()=>true,peopleFileStore:fileStore,preparePeopleFiles:async()=>[{name:'source.txt',type:'text/plain',size:1}]};
 const first=makeFeature({nodes,storage,deps,recovery:true});first.feature.bind();first.feature.onSelect();first.feature.setAttachAvail(true);const pending=first.feature.prepareSharedPrompt('source',source,[{id:'0123456789abcdef'}],()=>active);await new Promise(resolve=>setImmediate(resolve));active=false;release();assert.equal(await pending,false);first.feature.destroy();
 let finish,changed=0;const restarted=makeFeature({nodes,storage,deps:{...deps,promptSourcesChanged:()=>changed++},recovery:true,uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{path:'synthetic-native-ref'}]})}),sendHandler:()=>new Promise(resolve=>{finish=resolve})});restarted.feature.bind();restarted.feature.onSelect();restarted.feature.setDestination('agent');await new Promise(resolve=>setImmediate(resolve));await new Promise(resolve=>setImmediate(resolve));
 assert.match(restarted.roots.attstage.innerHTML,/source\.txt/);const sources=JSON.parse(storage.getItem('scimux-prompt-sources:n1')||'[]');assert.ok(sources.some(item=>item.seq===1),'recovered source file retains its provenance');assert.ok(changed>0,'source markers must refresh when provenance is recovered');
 assert.equal(restarted.apiCalls.length,0);restarted.feature.setPromptText('unrelated');restarted.roots.prompt.firstListener('input')();const sending=restarted.feature.sendPrompt();await new Promise(resolve=>setImmediate(resolve));restarted.timers.runIntervals();await new Promise(resolve=>setImmediate(resolve));
 const flights=JSON.parse(storage.getItem('scimux-prompt-flights:n1')||'[]');assert.ok(flights.some(flight=>flight.sources.some(item=>item.seq===1)));finish({delivery:'ok'});await sending;restarted.feature.destroy();
});

test('recovered source files cannot send when provenance storage is unavailable',async()=>{
 const source={server:'a'.repeat(32),owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),log:'comments',seq:1};const storage=makeStorage(),set=storage.setItem;storage.setItem=(key,value)=>{if(key==='scimux-prompt-sources:n1')throw Error('synthetic storage failure');set(key,value)};
 const fileStore={list:async()=>[{id:'0123456789abcdef',file:{name:'source.txt',type:'text/plain',size:1},metadata:{source,sourceFileID:'fedcba9876543210'}}]};
 const ctx=makeFeature({storage,nodes:[{id:'n1',instance_id:'1'.repeat(32)}],recovery:true,deps:{peopleFileStore:fileStore}});ctx.feature.bind();ctx.feature.onSelect();await new Promise(resolve=>setImmediate(resolve));ctx.feature.setPromptText('unrelated');ctx.roots.prompt.firstListener('input')();await ctx.feature.sendPrompt();
 assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);assert.equal(ctx.roots.recovery.hidden,false);assert.match(ctx.alerts.join(' '),/recovered/i);ctx.feature.destroy();
});

test('source file recovery distinguishes accepted images from retryable files',async()=>{
 const source={server:'a'.repeat(32),owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),log:'comments',seq:1},file={name:'synthetic.png',type:'image/png',size:1};
 const rows=[{id:'0123456789abcdef',file,metadata:{source,sourceFileID:'fedcba9876543210'},published:7},{id:'1111111111111111',file:{name:'retry.txt',type:'text/plain',size:1},metadata:{source,sourceFileID:'2222222222222222'}},{id:'3333333333333333',file,metadata:{sourceFileID:'4444444444444444'}},{id:'5555555555555555',file:{name:'invalid.txt',type:'text/plain',size:1},metadata:{source:{unknown:'synthetic'},sourceFileID:'6666666666666666'}}];
 const fileStore={list:async()=>rows};const ctx=makeFeature({nodes:[{id:'n1',instance_id:'1'.repeat(32)}],deps:{peopleFileStore:fileStore},recovery:true});ctx.feature.bind();ctx.feature.onSelect();await new Promise(resolve=>setImmediate(resolve));
 assert.match(ctx.roots.attstage.innerHTML,/synthetic\.png/);assert.match(ctx.roots.attstage.innerHTML,/Accepted; remove this saved file/);assert.match(ctx.roots.attstage.innerHTML,/retry\.txt/);assert.match(ctx.roots.attstage.innerHTML,/Review and send to retry/);
 assert.doesNotMatch(ctx.roots.attstage.innerHTML,/invalid\.txt/);assert.equal(ctx.created.length,1);assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);ctx.feature.destroy();
});

test('a legacy accepted flight without attachment metadata does not mark recovered files sent',async()=>{
 const source={server:'a'.repeat(32),owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),log:'comments',seq:1},identity='n1\0'+'1'.repeat(32);
 const storage=makeStorage({'scimux-draft-owner:n1':'1'.repeat(32),'scimux-prompt-flights:n1':JSON.stringify([{identity,accepted:true,sources:[source],text:'earlier'}])});
 const store={list:async()=>[{id:'0123456789abcdef',file:{name:'review.txt',type:'text/plain',size:1},metadata:{source,sourceFileID:'fedcba9876543210'}}]};
 const ctx=makeFeature({storage,nodes:[{id:'n1',instance_id:'1'.repeat(32)}],deps:{peopleFileStore:store},recovery:true});ctx.feature.bind();ctx.feature.onSelect();await new Promise(resolve=>setImmediate(resolve));
 assert.match(ctx.roots.attstage.innerHTML,/review\.txt/);assert.match(ctx.roots.attstage.innerHTML,/Review and send to retry/);assert.doesNotMatch(ctx.roots.attstage.innerHTML,/Accepted; remove/);assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();
});

test('failed source file recovery blocks unrelated sends when cleared source content is unresolved',async()=>{
 const source={server:'a'.repeat(32),owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),log:'comments',seq:1};const identity='n1\0'+'1'.repeat(32);
 const storage=makeStorage({'scimux-draft-owner:n1':'1'.repeat(32),'scimux-prompt-sources:n1':JSON.stringify([source]),['scimux-cleared-source-text:'+identity]:JSON.stringify([JSON.stringify(["shared",source.server,source.owner,source.chat,source.generation,"comments",1])])});
 const ctx=makeFeature({storage,nodes:[{id:'n1',instance_id:'1'.repeat(32)}],recovery:true,deps:{peopleFileStore:{list:async()=>{throw Error('synthetic storage failure')}}}});ctx.feature.bind();ctx.feature.onSelect();await new Promise(resolve=>setImmediate(resolve));
 assert.match(ctx.alerts.join(' '),/could not be recovered/i);assert.equal(ctx.roots.recovery.hidden,false);ctx.feature.setPromptText('unrelated');ctx.roots.prompt.firstListener('input')();await ctx.feature.sendPrompt();assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);ctx.feature.destroy();
});

test('an already staged source file remains attributable while recovery is pending',async()=>{
 const source={server:'a'.repeat(32),owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),log:'comments',seq:1};let release;
 const store={list:()=>new Promise(resolve=>{release=resolve})};
 const ctx=makeFeature({nodes:[{id:'n1',instance_id:'1'.repeat(32)}],recovery:true,deps:{peopleFileStore:store,canPreparePrompt:()=>true},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{path:'synthetic-ref'}]})})});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setAttachAvail(true);assert.equal(ctx.feature.preparePrompt('source caption',source,[{name:'source.txt',type:'text/plain',size:1}]),true);await new Promise(resolve=>setImmediate(resolve));
 ctx.feature.setPromptText('');ctx.roots.prompt.firstListener('input')();ctx.feature.setPromptText('unrelated text');ctx.roots.prompt.firstListener('input')();await ctx.feature.sendPrompt();
 const sends=ctx.apiCalls.filter(call=>call.path.split('?')[0].endsWith('/send'));assert.equal(sends.length,1);assert.deepEqual(JSON.parse(ctx.storage.getItem('scimux-prompt-flights:n1'))[0].sources,[source]);
 release([]);await new Promise(resolve=>setImmediate(resolve));ctx.feature.destroy();
});

test('server-only local pending is reviewable without a browser pending row', async () => {
  const key = ['1'.repeat(32), '2'.repeat(32), '3'.repeat(16), '4'.repeat(16)].join('/');
  const calls = [];
  const ctx = makeFeature({sel:'team/' + key, nodes:[], deps:{
    peopleContext:()=>({key, copyInstance:'a'.repeat(32), binding:'synthetic', writable:false, attachments:false}),
    localReviewApi:async (path, options)=>{calls.push({path, options}); return {entries:[{kind:'pending',id:'5'.repeat(16),revision:'6'.repeat(64),text:'Synthetic orphan message',stage_ids:['7'.repeat(16)]}]};},
  }});
  try {
    ctx.feature.bind(); ctx.feature.onSelect();
    await new Promise(resolve=>setImmediate(resolve));
    assert.match(ctx.roots.attstage.innerHTML, /Synthetic orphan message/);
    assert.equal(calls.length, 1);
    assert.equal(calls[0].options.method, 'GET');
  } finally { ctx.feature.destroy(); }
});

const localReviewKey = ['a'.repeat(32), 'b'.repeat(32), 'c'.repeat(16), 'd'.repeat(16)].join('/');
const localReviewRow = (kind, id, fields = {}) => ({kind,id,revision:'e'.repeat(64),...fields});
const localReviewTick = () => new Promise(resolve=>setImmediate(resolve));

test('local review follows empty pages and displays escaped metadata in read-only view', async () => {
  const calls=[]; const pages=[{entries:[],next_cursor:'p'+'1'.repeat(16)},{entries:[localReviewRow('pending','2'.repeat(16),{text:'Saved <message>',stage_ids:['3'.repeat(16)]}),localReviewRow('stage','3'.repeat(16),{name:'file <one>.txt',mime:'text/plain',size:4,hash:'f'.repeat(64)})]}];
  const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance:'1'.repeat(32),binding:'one',writable:false,attachments:false}),localReviewApi:async(path,opts)=>{calls.push({path,opts});return pages.shift();}}});
  try {ctx.feature.bind();ctx.feature.onSelect();assert.match(ctx.roots.attstage.innerHTML,/role="status"/);await localReviewTick();
    assert.match(ctx.roots.attstage.innerHTML,/Saved &lt;message&gt;/);assert.match(ctx.roots.attstage.innerHTML,/file &lt;one&gt;.txt/);assert.equal(calls.length,2);ctx.feature.renderStage();ctx.feature.refreshDestination();await localReviewTick();assert.equal(calls.length,2);assert.ok(calls.every(c=>c.opts.method==='GET'));
  } finally {ctx.feature.destroy();}
});

test('local review shows stage-only, empty, missing identity and failure feedback', async () => {
  for (const scenario of ['stage','empty','missing','error']) {
    let calls=0; const context={key:localReviewKey,copyInstance:scenario==='missing'?'':'1'.repeat(32),writable:false,attachments:false};
    const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>context,localReviewApi:async()=>{calls++;if(scenario==='error')throw Error('offline');return {entries:scenario==='stage'?[localReviewRow('stage','3'.repeat(16),{name:'orphan.txt',mime:'text/plain',size:2,hash:'f'.repeat(64)})]:[]};}}});
    try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();if(scenario==='empty'){assert.equal(ctx.roots.attstage.hidden,true);assert.doesNotMatch(ctx.roots.attstage.innerHTML,/No saved local/);}else {assert.match(ctx.roots.attstage.innerHTML,scenario==='stage'?/orphan.txt/:/unavailable/i);if(scenario!=='stage')assert.match(ctx.roots.attstage.innerHTML,/role="status"/);}assert.equal(calls,scenario==='missing'?0:1);} finally {ctx.feature.destroy();}
  }
});

test('local review deduplicates only the saved durable pending key and preserves input', async () => {
  const pending={key:localReviewKey,text:'browser text',attachments:[],value:JSON.stringify({request:{key:'2'.repeat(16)}})};
  const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance:'1'.repeat(32),writable:true,attachments:true}),pendingPeople:()=>pending,localReviewApi:async()=>({entries:[localReviewRow('pending','2'.repeat(16),{text:'browser text',stage_ids:['3'.repeat(16)]}),localReviewRow('pending','4'.repeat(16),{text:'other key',stage_ids:['3'.repeat(16)]})]})}});
  try {ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('new draft');ctx.roots.prompt.firstListener('input')();await localReviewTick();assert.equal(ctx.feature.promptText(),'new draft');assert.match(ctx.roots.attstage.innerHTML,/browser text/);assert.doesNotMatch(ctx.roots.attstage.innerHTML,/same key/);assert.match(ctx.roots.attstage.innerHTML,/other key/);assert.match(ctx.roots.attstage.innerHTML,/Retry this message/);assert.equal((ctx.roots.attstage.innerHTML.match(/browser text/g)||[]).length,1);} finally {ctx.feature.destroy();}
});

test('local review invalidates delayed pages across navigation, identity changes and destroy', async () => {
  for (const change of ['chat','none','copy','binding','native','destination','destroy']) {
    let context={key:localReviewKey,copyInstance:'1'.repeat(32),binding:'one',owner:true,writable:false,attachments:false}; const releases=[], calls=[];
    const ctx=makeFeature({controls:true,nodes:[{id:'n1',instance_id:'9'.repeat(32)},{id:'n2',instance_id:'8'.repeat(32)}],deps:{peopleContext:()=>context,localReviewApi:(path,opts)=>{calls.push({path,opts});return new Promise(resolve=>releases.push(resolve));}}});
    try {ctx.feature.bind();ctx.feature.onSelect();assert.equal(calls.length,1);if(change==='chat'){ctx.setSel('n2');context=null;ctx.feature.onSelect();}else if(change==='none'){ctx.setSel('');context=null;ctx.feature.onSelect();}else if(change==='copy'){context={...context,copyInstance:'2'.repeat(32)};ctx.feature.contextChanged();}else if(change==='binding'){context={...context,binding:'two'};ctx.feature.contextChanged();}else if(change==='native'){ctx.nodes[0].instance_id='7'.repeat(32);ctx.feature.onSelect();}else if(change==='destination'){ctx.feature.setDestination('agent');}else ctx.feature.destroy();
      releases[0]({entries:[localReviewRow('pending','2'.repeat(16),{text:'stale secret',stage_ids:['3'.repeat(16)]})],next_cursor:'p'+'1'.repeat(16)});await localReviewTick();assert.doesNotMatch(ctx.roots.attstage.innerHTML,/stale secret/);assert.equal(calls.filter(c=>c.path.includes('cursor=')).length,0);assert.equal(calls[0].opts.signal.aborted,true);
    } finally {ctx.feature.destroy();}
  }
});

test('no selection or unshared context does not load local review', async () => {
  let calls=0;const ctx=makeFeature({sel:'',deps:{peopleContext:()=>null,localReviewApi:async()=>{calls++;}}});
  try {ctx.feature.bind();ctx.feature.onSelect();ctx.setSel('n1');ctx.feature.onSelect();await localReviewTick();assert.equal(calls,0);} finally {ctx.feature.destroy();}
});

test('local review from a destroyed lifetime cannot overwrite a rebound visit', async () => {
  const releases=[]; const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance:'1'.repeat(32),writable:false}),localReviewApi:()=>new Promise(resolve=>releases.push(resolve))}});
  try {ctx.feature.bind();ctx.feature.onSelect();ctx.feature.destroy();ctx.feature.bind();ctx.feature.onSelect();releases[0]({entries:[]});await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/Loading saved/);releases[1]({entries:[localReviewRow('pending','2'.repeat(16),{text:'new visit',stage_ids:['3'.repeat(16)]})]});await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/new visit/);} finally {ctx.feature.destroy();}
});

test('local review uses its injected read API without sending through composer mutation API', async () => {
  let reads=0, writes=0; const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance:'1'.repeat(32)}),api:async()=>{writes++;},localReviewApi:async(_path,opts)=>{reads++;assert.equal(opts.method,'GET');return {entries:[]};}}});
  try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(reads,1);assert.equal(writes,0);} finally {ctx.feature.destroy();}
});

test('inactive owner reviews each authoritative retained copy without switching destination', async () => {
  const other = localReviewKey.slice(0,-16) + '9'.repeat(16); const calls=[];
  const contexts=[{key:localReviewKey,copyInstance:'1'.repeat(32),binding:'one',owner:true},{key:other,copyInstance:'2'.repeat(32),binding:'two',owner:true}];
  const ctx=makeFeature({nodes:[{id:'n1',instance_id:'8'.repeat(32)}],deps:{peopleContext:()=>null,localReviewContexts:()=>contexts,localReviewApi:async(path,opts)=>{calls.push({path,opts});return {entries:[localReviewRow('pending','3'.repeat(16),{text:path.includes(other)?'second copy':'first copy',stage_ids:['4'.repeat(16)]})]};}}});
  try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(ctx.feature.destination(),'agent');assert.match(ctx.roots.attstage.innerHTML,/first copy/);assert.match(ctx.roots.attstage.innerHTML,/second copy/);assert.equal(calls.length,2);assert.ok(calls.every(c=>c.opts.method==='GET'));} finally {ctx.feature.destroy();}
});

test('local review refuses owner copies without the native chat incarnation', async () => {
  let calls=0;const ctx=makeFeature({nodes:[{id:'n1'}],deps:{requireChatIdentity:true,peopleContext:()=>({key:localReviewKey,copyInstance:'1'.repeat(32),owner:true,writable:false}),localReviewApi:async()=>{calls++;return {entries:[]};}}});
  try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(calls,0);assert.match(ctx.roots.attstage.innerHTML,/unavailable/i);} finally {ctx.feature.destroy();}
});

test('inventory review keeps the existing retry button bound after rendering', async () => {
  const pending={key:localReviewKey,text:'saved',attachments:[],value:JSON.stringify({request:{key:'2'.repeat(16)}})};
  const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance:'1'.repeat(32),writable:true,attachments:true}),pendingPeople:()=>pending,localReviewApi:async()=>({entries:[]})}});
  let html='', retry=[];Object.defineProperty(ctx.roots.attstage,'innerHTML',{get:()=>html,set:value=>{html=value;retry=/class="(?:[^"\s]+\s+)*people-retry(?:\s+[^"\s]+)*"/.test(html)?[el('button')]:[];}});ctx.roots.attstage.querySelectorAll=selector=>selector==='.people-retry'?retry:[];
  try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(retry.length,1);assert.equal(retry[0].listenerCount('click'),1);} finally {ctx.feature.destroy();}
});

test('returning to the same copy cannot let an aborted earlier visit overwrite it', async () => {
  let context={key:localReviewKey,copyInstance:'1'.repeat(32),writable:false};const releases=[];
  const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>context,localReviewApi:()=>new Promise(resolve=>releases.push(resolve))}});
  let paints=0, html='';Object.defineProperty(ctx.roots.attstage,'innerHTML',{get:()=>html,set:value=>{html=value;paints++;}});
  try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();context={...context,copyInstance:'2'.repeat(32)};ctx.feature.contextChanged();context={...context,copyInstance:'1'.repeat(32)};ctx.feature.contextChanged();await localReviewTick();assert.equal(releases.length,3);const before=paints;releases[0]({entries:[localReviewRow('pending','5'.repeat(16),{text:'stale copy',stage_ids:['6'.repeat(16)]})]});await localReviewTick();assert.equal(paints,before);assert.doesNotMatch(ctx.roots.attstage.innerHTML,/stale copy/);assert.match(ctx.roots.attstage.innerHTML,/Loading saved/);releases[2]({entries:[localReviewRow('pending','3'.repeat(16),{text:'current copy',stage_ids:['4'.repeat(16)]})]});await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/current copy/);assert.doesNotMatch(ctx.roots.attstage.innerHTML,/stale copy/);} finally {ctx.feature.destroy();}
});

test('local review never dispatches a received copy for a different selected chat', async () => {
  let calls=0;const ctx=makeFeature({sel:'team/'+localReviewKey.slice(0,-16)+'9'.repeat(16),nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance:'1'.repeat(32)}),localReviewApi:async()=>{calls++;return {entries:[]};}}});
  try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(calls,0);} finally {ctx.feature.destroy();}
});

test('local review does not use a received context on a native chat', async () => {
  let calls=0;const ctx=makeFeature({deps:{peopleContext:()=>({key:localReviewKey,copyInstance:'1'.repeat(32),owner:false}),localReviewApi:async()=>{calls++;return {entries:[]};}}});
  try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(calls,0);} finally {ctx.feature.destroy();}
});

test('local review retains staged files and uses truthful missing metadata', async () => {
  let release;const pending={key:localReviewKey,text:'browser',attachments:[],value:'invalid'};
  const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance:'1'.repeat(32),writable:true,attachments:true}),pendingPeople:()=>pending,localReviewApi:()=>new Promise(resolve=>{release=resolve;})},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:'0123456789abcdef'}]})})});
  try {ctx.feature.bind();ctx.feature.onSelect();ctx.feature.addFiles([{name:'new file.txt',type:'text/plain',size:1}]);ctx.feature.setPromptText('new text');ctx.roots.prompt.firstListener('input')();release({entries:[localReviewRow('pending','2'.repeat(16),{text:'',stage_ids:['3'.repeat(16)]}),localReviewRow('stage','4'.repeat(16),{})]});await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/new file.txt/);assert.equal(ctx.feature.promptText(),'new text');assert.match(ctx.roots.attstage.innerHTML,/Saved file metadata unavailable/);assert.match(ctx.roots.attstage.innerHTML,/File-only message/);assert.match(ctx.roots.attstage.innerHTML,/File metadata unavailable/);assert.match(ctx.roots.attstage.innerHTML,/Unknown type/);assert.match(ctx.roots.attstage.innerHTML,/Unknown size/);assert.match(ctx.roots.attstage.innerHTML,/Hash unavailable/);} finally {ctx.feature.destroy();}
});

test('local review reports missing pending file metadata', async () => {
  const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance:'1'.repeat(32)}),localReviewApi:async()=>({entries:[localReviewRow('pending','2'.repeat(16),{text:'saved'})]})}});
  try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/Saved file metadata unavailable/);} finally {ctx.feature.destroy();}
});

test('local review preserves multiline saved text for inspection', async () => {
  const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance:'1'.repeat(32)}),localReviewApi:async()=>({entries:[localReviewRow('pending','2'.repeat(16),{text:'line one\n  line two',stage_ids:['3'.repeat(16)]})]})}});
  try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/<textarea readonly>line one\n  line two<\/textarea>/);} finally {ctx.feature.destroy();}
});

test('received selection without context does not request local inventory', async () => {
  let calls=0;const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>null,localReviewApi:async()=>{calls++;return {entries:[]};}}});
  try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(calls,0);} finally {ctx.feature.destroy();}
});

test('local review ignores a result if context changes before its continuation', async () => {
  let context={key:localReviewKey,copyInstance:'1'.repeat(32)}, release;const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>context,localReviewApi:()=>new Promise(resolve=>{release=resolve;})}});
  try {ctx.feature.bind();ctx.feature.onSelect();release({entries:[localReviewRow('pending','2'.repeat(16),{text:'old result',stage_ids:['3'.repeat(16)]})]});queueMicrotask(()=>{context={...context,copyInstance:'2'.repeat(32)};});await localReviewTick();assert.doesNotMatch(ctx.roots.attstage.innerHTML,/old result/);} finally {ctx.feature.destroy();}
});

test('empty stage associations are reported as unavailable', async () => {
  const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance:'1'.repeat(32)}),localReviewApi:async()=>({entries:[localReviewRow('pending','2'.repeat(16),{text:'saved',stage_ids:[]})]})}});
  try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/Saved file metadata unavailable/);} finally {ctx.feature.destroy();}
});

test('destroyed composer does not restart local review from late callbacks', async () => {
  let calls=0; const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance:'1'.repeat(32),writable:false}),localReviewApi:async()=>{calls++;return {entries:[]};}}});
  try {
    ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(calls,1);
    ctx.feature.destroy();ctx.feature.destroy();ctx.feature.refreshDestination();ctx.feature.contextChanged();await localReviewTick();assert.equal(calls,1);
    ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(calls,2);
  } finally {ctx.feature.destroy();}
});

test('local review lifetime supports pre-bind selection but not callbacks after early destroy', async () => {
  let calls=0; const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance:'1'.repeat(32)}),localReviewApi:async()=>{calls++;return {entries:[]};}}});
  try {ctx.feature.onSelect();await localReviewTick();assert.equal(calls,1);ctx.feature.destroy();ctx.feature.refreshDestination();ctx.feature.contextChanged();await localReviewTick();assert.equal(calls,1);} finally {ctx.feature.destroy();}
  const stopped=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance:'1'.repeat(32)}),localReviewApi:async()=>{calls++;return {entries:[]};}}});
  try {stopped.feature.destroy();stopped.feature.destroy();stopped.feature.onSelect();stopped.feature.refreshDestination();stopped.feature.contextChanged();await localReviewTick();assert.equal(calls,1);stopped.feature.bind();stopped.feature.onSelect();await localReviewTick();assert.equal(calls,2);} finally {stopped.feature.destroy();}
});

test('late fulfilled and rejected local reviews cannot revive a destroyed lifetime', async () => {
  for (const outcome of ['resolve','reject']) {
    let settle, calls=0;const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance:'1'.repeat(32)}),localReviewApi:()=>{calls++;return new Promise((resolve,reject)=>{settle=outcome==='resolve'?()=>resolve({entries:[localReviewRow('pending','2'.repeat(16),{text:'stale',stage_ids:['3'.repeat(16)]})]}):()=>reject(Error('synthetic failure'));});}}});
    try {ctx.feature.bind();ctx.feature.onSelect();ctx.feature.destroy();settle();ctx.feature.refreshDestination();ctx.feature.contextChanged();await localReviewTick();assert.equal(calls,1);assert.doesNotMatch(ctx.roots.attstage.innerHTML,/stale|unavailable/i);} finally {ctx.feature.destroy();}
  }
});

test('destroyed unshared and empty selections do not start local review', async () => {
  let calls=0;const ctx=makeFeature({sel:'',deps:{peopleContext:()=>null,localReviewApi:async()=>{calls++;return {entries:[]};}}});
  try {ctx.feature.destroy();ctx.feature.refreshDestination();ctx.feature.contextChanged();ctx.setSel('n1');ctx.feature.onSelect();ctx.feature.refreshDestination();await localReviewTick();assert.equal(calls,0);} finally {ctx.feature.destroy();}
});

test('explicitly discarding a server-only pending review uses its exact local revision', async () => {
  const calls=[];const id='2'.repeat(16), revision='e'.repeat(64), copy='1'.repeat(32);
  const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance:copy,writable:false,attachments:false}),localReviewApi:async(path,options)=>{calls.push({path,options});return options.method==='GET'?{entries:[localReviewRow('pending',id,{text:'saved orphan',stage_ids:['3'.repeat(16)]})]}:{state:'locally_discarded'};}}});
  const original=ctx.roots.attstage.querySelectorAll;let buttons=[];
  ctx.roots.attstage.querySelectorAll=selector=>selector==='.people-local-discard'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-local-discard(?:\s+[^"\s]+)*" data-row="(\d+)" data-id="([0-9a-f]+)"/g)].map(m=>el('button',{dataset:{row:m[1],id:m[2]}}))):original(selector);
  try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/Discard local pending message/);assert.match(ctx.roots.attstage.innerHTML,/remote comment may still appear/i);assert.equal(buttons.length,1);buttons[0].click();await localReviewTick();assert.equal(calls.length,2);assert.equal(calls[1].path,`/api/team/subscriptions/${localReviewKey}/pending/${id}?copy_instance=${copy}&revision=${revision}`);assert.deepEqual(calls[1].options,{method:'DELETE'});assert.match(ctx.roots.attstage.innerHTML,/locally discarded/i);assert.doesNotMatch(ctx.roots.attstage.innerHTML,/saved orphan/);} finally {ctx.feature.destroy();}
});

function discardReviewFixture({result, pendingRows, context:initial, nodes, contexts} = {}) {
  let context=initial || {key:localReviewKey,copyInstance:'1'.repeat(32),writable:false,attachments:false};const calls=[];
  const rows=pendingRows || [localReviewRow('pending','2'.repeat(16),{text:'first saved',stage_ids:['3'.repeat(16)]})];
  const ctx=makeFeature({sel:contexts?'n1':'team/'+localReviewKey,nodes:nodes || (contexts?[{id:'n1',instance_id:'8'.repeat(32)}]:[]),deps:{peopleContext:()=>contexts?null:context,localReviewContexts:()=>contexts||[],localReviewApi:async(path,opts)=>{calls.push({path,opts});return opts.method==='GET'?{entries:rows}:result(path,opts);}}});
  const original=ctx.roots.attstage.querySelectorAll;let buttons=[];
  ctx.roots.attstage.querySelectorAll=selector=>selector==='.people-local-discard'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-local-discard(?:\s+[^"\s]+)*" data-row="(\d+)" data-id="([0-9a-f]+)"/g)].map(m=>el('button',{dataset:{row:m[1],id:m[2]}}))):original(selector);
  return {ctx,calls,buttons:()=>buttons,setContext:value=>{context=value;},context:()=>context};
}

test('local discard reports actual completion and preserves other pending files and new text', async () => {
  const f=discardReviewFixture({result:async()=>({state:'already_completed',seq:42}),pendingRows:[localReviewRow('pending','2'.repeat(16),{text:'first saved',stage_ids:['3'.repeat(16)]}),localReviewRow('pending','4'.repeat(16),{text:'other saved',stage_ids:['3'.repeat(16)]}),localReviewRow('stage','3'.repeat(16),{name:'kept.txt',mime:'text/plain',size:1,hash:'f'.repeat(64)})]});
  try {f.ctx.feature.bind();f.ctx.feature.onSelect();f.ctx.feature.setPromptText('new draft');f.ctx.roots.prompt.firstListener('input')();await localReviewTick();assert.equal(f.buttons().length,2);f.buttons()[0].click();await localReviewTick();assert.match(f.ctx.roots.attstage.innerHTML,/already completed.*42/i);assert.match(f.ctx.roots.attstage.innerHTML,/other saved/);assert.match(f.ctx.roots.attstage.innerHTML,/kept.txt/);assert.equal(f.ctx.feature.promptText(),'new draft');assert.equal(f.calls.filter(c=>c.opts.method==='DELETE').length,1);} finally {f.ctx.feature.destroy();}
});

test('failed or malformed local discard retains the reviewed value for explicit retry', async () => {
  for (const response of [null,{state:'already_completed',seq:0},Error('offline')]) {
    const f=discardReviewFixture({result:async()=>{if(response instanceof Error)throw response;return response;}});
    try {f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();f.buttons()[0]?.click();await localReviewTick();assert.match(f.ctx.roots.attstage.innerHTML,/first saved/);assert.match(f.ctx.roots.attstage.innerHTML,/role="status"[^>]*>[^<]*unavailable/i);assert.equal(f.buttons().length,1);assert.equal(f.calls.filter(c=>c.opts.method==='DELETE').length,1);f.buttons()[0].click();await localReviewTick();assert.equal(f.calls.filter(c=>c.opts.method==='DELETE').length,2);} finally {f.ctx.feature.destroy();}
  }
});

test('local discard prevents duplicate clicks and leaves other copy untouched', async () => {
  let release;const other=localReviewKey.slice(0,-16)+'9'.repeat(16),contexts=[{key:localReviewKey,copyInstance:'1'.repeat(32),owner:true},{key:other,copyInstance:'2'.repeat(32),owner:true}];
  const f=discardReviewFixture({contexts,result:()=>new Promise(resolve=>{release=resolve;})});
  try {f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();assert.equal(f.buttons().length,2);const original=f.buttons()[0];original.click();original.click();assert.equal(f.calls.filter(c=>c.opts.method==='DELETE').length,1);assert.match(f.ctx.roots.attstage.innerHTML,/role="status"[^>]*>[^<]*discarding/i);release({state:'locally_discarded'});await localReviewTick();assert.equal(f.buttons().length,1);assert.match(f.ctx.roots.attstage.innerHTML,/first saved/);assert.equal(f.calls.filter(c=>c.opts.method==='DELETE').length,1);} finally {f.ctx.feature.destroy();}
});

test('late local discard success or failure cannot affect a changed view', async () => {
  for (const change of ['selection','copy','binding','native','destination','destroy','rebind']) for (const outcome of ['resolve','reject']) {
    let settle;const f=discardReviewFixture({context:{key:localReviewKey,copyInstance:'1'.repeat(32),binding:'one',owner:true,writable:false},nodes:[{id:'n1',instance_id:'8'.repeat(32)}],result:()=>new Promise((resolve,reject)=>{settle=()=>outcome==='resolve'?resolve({state:'locally_discarded'}):reject(Error('offline'));})});
    f.ctx.setSel('n1');
    try {f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();f.buttons()[0]?.click();assert.equal(f.calls.filter(c=>c.opts.method==='DELETE').length,1);
      if(change==='selection'){f.ctx.setSel('');f.ctx.feature.onSelect();}else if(change==='copy'){f.setContext({...f.context(),copyInstance:'2'.repeat(32)});f.ctx.feature.contextChanged();}else if(change==='binding'){f.setContext({...f.context(),binding:'two'});f.ctx.feature.contextChanged();}else if(change==='native'){f.ctx.nodes[0].instance_id='9'.repeat(32);f.ctx.feature.onSelect();}else if(change==='destination')f.ctx.feature.setDestination('agent');else {f.ctx.feature.destroy();if(change==='rebind'){f.ctx.feature.bind();f.ctx.feature.onSelect();}}
      await localReviewTick();const before=f.ctx.roots.attstage.innerHTML;settle();await localReviewTick();assert.equal(f.ctx.roots.attstage.innerHTML,before);
    } finally {f.ctx.feature.destroy();}
  }
});

test('inventoried browser pending shares one review and keeps browser recovery after local discard', async () => {
  const id='2'.repeat(16), saved=JSON.stringify({request:{key:id,text:'same saved',attachments:['3'.repeat(16)]}}),pending={key:localReviewKey,text:'same saved',attachments:['3'.repeat(16)],value:saved};const calls=[];
  const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance:'1'.repeat(32),writable:false}),pendingPeople:()=>pending,localReviewApi:async(path,opts)=>{calls.push({path,opts});return opts.method==='GET'?{entries:[localReviewRow('pending',id,{text:'same saved',stage_ids:['3'.repeat(16)]})]}:{state:'locally_discarded'};}}});
  const original=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=selector=>selector==='.people-local-discard'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-local-discard(?:\s+[^"\s]+)*" data-row="(\d+)" data-id="([0-9a-f]+)"/g)].map(m=>el('button',{dataset:{row:m[1],id:m[2]}}))):original(selector);
  try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal((ctx.roots.attstage.innerHTML.match(/same saved/g)||[]).length,1);assert.equal(buttons.length,1);buttons[0].click();await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/same saved/);assert.equal(pending.value,saved);assert.equal(calls.filter(c=>c.opts.method==='DELETE').length,1);} finally {ctx.feature.destroy();}
});

test('stale and duplicate captured discard callbacks cannot dispatch', async () => {
  let release;const f=discardReviewFixture({result:()=>new Promise(resolve=>{release=resolve;})});
  try {f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();const click=f.buttons()[0].firstListener('click');click();click();assert.equal(f.calls.filter(c=>c.opts.method==='DELETE').length,1);f.ctx.setSel('');f.ctx.feature.onSelect();click();assert.equal(f.calls.filter(c=>c.opts.method==='DELETE').length,1);release({state:'locally_discarded'});await localReviewTick();} finally {f.ctx.feature.destroy();}
});

test('forged discard controls cannot address an uninventoried row', async () => {
  const f=discardReviewFixture({result:async()=>({state:'locally_discarded'})});const old=f.ctx.roots.attstage.querySelectorAll, forged=el('button',{dataset:{row:'99',id:'2'.repeat(16)}});
  f.ctx.roots.attstage.querySelectorAll=selector=>selector==='.people-local-discard'?[forged]:old(selector);
  try {f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();assert.equal(forged.listenerCount('click'),0);forged.click();assert.equal(f.calls.filter(c=>c.opts.method==='DELETE').length,0);f.ctx.roots.attstage.querySelectorAll=undefined;f.ctx.feature.renderStage();} finally {f.ctx.feature.destroy();}
});

test('discard completion is fenced if copy changes before composer continuation', async () => {
  let release;const f=discardReviewFixture({result:()=>new Promise(resolve=>{release=resolve;})});
  try {f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();f.buttons()[0].click();release({state:'locally_discarded'});queueMicrotask(()=>{f.setContext({...f.context(),copyInstance:'2'.repeat(32)});});await localReviewTick();assert.doesNotMatch(f.ctx.roots.attstage.innerHTML,/Locally discarded/);} finally {f.ctx.feature.destroy();}
});

test('explicit local discard never invokes agent, relay, send or receipt controls', async () => {
  let controls=0;const calls=[];const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance:'1'.repeat(32),writable:false}),api:async()=>{controls++;},sendPeople:async()=>{controls++;},finishPeople:()=>{controls++;},localReviewApi:async(path,opts)=>{calls.push({path,opts});return opts.method==='GET'?{entries:[localReviewRow('pending','2'.repeat(16),{text:'saved',stage_ids:['3'.repeat(16)]})]}:{state:'locally_discarded'};}}});
  const old=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-local-discard'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-local-discard(?:\s+[^"\s]+)*" data-row="(\d+)" data-id="([0-9a-f]+)"/g)].map(m=>el('button',{dataset:{row:m[1],id:m[2]}}))):old(s);
  try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(controls,0);buttons[0].click();await localReviewTick();assert.equal(controls,0);assert.deepEqual(calls.map(c=>c.opts.method),['GET','DELETE']);} finally {ctx.feature.destroy();}
});

test('discard result is ignored when context changes after adapter validation', async () => {
  let context={key:localReviewKey,copyInstance:'1'.repeat(32)}, schedule=false, calls=0;
  const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>{if(schedule){schedule=false;queueMicrotask(()=>{context={...context,copyInstance:'2'.repeat(32)};});}return context;},localReviewApi:async(_path,opts)=>{calls++;if(opts.method==='GET')return {entries:[localReviewRow('pending','2'.repeat(16),{text:'saved',stage_ids:['3'.repeat(16)]})]};schedule=true;return {state:'locally_discarded'};}}});
  const old=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-local-discard'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-local-discard(?:\s+[^"\s]+)*" data-row="(\d+)" data-id="([0-9a-f]+)"/g)].map(m=>el('button',{dataset:{row:m[1],id:m[2]}}))):old(s);
  try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();buttons[0].click();await localReviewTick();assert.equal(calls,2);assert.doesNotMatch(ctx.roots.attstage.innerHTML,/Locally discarded/);} finally {ctx.feature.destroy();}
});

test('discard leaves a newer staged browser file and draft untouched', async () => {
  let release;const calls=[];const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance:'1'.repeat(32),writable:true,attachments:true}),localReviewApi:async(path,opts)=>{calls.push(opts.method);return opts.method==='GET'?{entries:[localReviewRow('pending','2'.repeat(16),{text:'old pending',stage_ids:['3'.repeat(16)]})]}:new Promise(resolve=>{release=resolve;});}},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:'0123456789abcdef'}]})})});
  const old=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-local-discard'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-local-discard(?:\s+[^"\s]+)*" data-row="(\d+)" data-id="([0-9a-f]+)"/g)].map(m=>el('button',{dataset:{row:m[1],id:m[2]}}))):old(s);
  try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();buttons[0].click();ctx.feature.setPromptText('new draft');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'new local file.txt',type:'text/plain',size:1}]);await localReviewTick();release({state:'locally_discarded'});await localReviewTick();assert.equal(ctx.feature.promptText(),'new draft');assert.match(ctx.roots.attstage.innerHTML,/new local file.txt/);assert.deepEqual(calls,['GET','DELETE']);} finally {ctx.feature.destroy();}
});

test('browser pending conflict displays the actual inventoried text before discard', async () => {
  const id='2'.repeat(16),pending={key:localReviewKey,text:'browser draft',attachments:['3'.repeat(16)],value:JSON.stringify({request:{key:id}})};
  const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance:'1'.repeat(32)}),pendingPeople:()=>pending,localReviewApi:async()=>({entries:[localReviewRow('pending',id,{text:'actual saved text',stage_ids:['3'.repeat(16)]})]})}});
  try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/browser draft/);assert.match(ctx.roots.attstage.innerHTML,/actual saved text/);assert.match(ctx.roots.attstage.innerHTML,/Discard local pending message/);} finally {ctx.feature.destroy();}
});

test('browser review shows the inventoried file name for the exact pending record', async () => {
  const id='2'.repeat(16),pending={key:localReviewKey,text:'saved',attachments:['3'.repeat(16)],value:JSON.stringify({request:{key:id}})};
  const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance:'1'.repeat(32)}),pendingPeople:()=>pending,localReviewApi:async()=>({entries:[localReviewRow('pending',id,{text:'saved',stage_ids:['3'.repeat(16)]}),localReviewRow('stage','3'.repeat(16),{name:'actual.txt',mime:'text/plain',size:1,hash:'f'.repeat(64)})]})}});
  try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/Actual local files: actual.txt/);} finally {ctx.feature.destroy();}
});

test('browser review reports missing inventoried text and file metadata', async () => {
  for (const pair of [{browser:'draft',server:'',stage_ids:null},{browser:'',server:'saved',stage_ids:[]},{browser:'',server:'',stage_ids:['3'.repeat(16)]}]) {
    const id='2'.repeat(16),pending={key:localReviewKey,text:pair.browser,attachments:['3'.repeat(16)],value:JSON.stringify({request:{key:id}})};
    const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance:'1'.repeat(32)}),pendingPeople:()=>pending,localReviewApi:async()=>({entries:[localReviewRow('pending',id,{text:pair.server,stage_ids:pair.stage_ids})]})}});
    try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/Saved file metadata unavailable/);assert.match(ctx.roots.attstage.innerHTML,/Discard local pending message/);} finally {ctx.feature.destroy();}
  }
});

test('a revisited pending review can be explicitly retried after the earlier request settles', async () => {
  let release, requests=0;const f=discardReviewFixture({result:()=>++requests===1?new Promise(resolve=>{release=resolve;}):Promise.resolve({state:'locally_discarded'})});
  try {f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();f.buttons()[0].click();f.ctx.setSel('');f.ctx.feature.onSelect();f.ctx.setSel('team/'+localReviewKey);f.ctx.feature.onSelect();await localReviewTick();release({state:'locally_discarded'});await localReviewTick();assert.equal(f.buttons().length,1);f.buttons()[0].click();await localReviewTick();assert.equal(requests,2);} finally {f.ctx.feature.destroy();}
});


test('empty automatic review stays quiet while a new staged file and draft survive', async () => {
  let calls=0; const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance:'1'.repeat(32),writable:true,attachments:true}),localReviewApi:async()=>{calls++;return {entries:[]};}},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:'0123456789abcdef'}]})})});
  try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(ctx.roots.attstage.hidden,true);assert.doesNotMatch(ctx.roots.attstage.innerHTML,/No saved local/);ctx.feature.setPromptText('new synthetic draft');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'first.txt',type:'text/plain',size:1}]);await localReviewTick();assert.equal(ctx.roots.attstage.hidden,false);assert.match(ctx.roots.attstage.innerHTML,/first.txt/);assert.doesNotMatch(ctx.roots.attstage.innerHTML,/No saved local/);assert.equal(ctx.feature.promptText(),'new synthetic draft');assert.equal(calls,1);} finally {ctx.feature.destroy();}
});

test('empty copy rows are skipped without changing the reviewed action copy', async () => {
  const other=localReviewKey.slice(0,-16)+'9'.repeat(16),copy='2'.repeat(32),id='2'.repeat(16),calls=[];
  const contexts=[{key:localReviewKey,copyInstance:'1'.repeat(32),owner:true},{key:other,copyInstance:copy,owner:true}];
  const ctx=makeFeature({nodes:[{id:'n1',instance_id:'8'.repeat(32)}],deps:{peopleContext:()=>null,localReviewContexts:()=>contexts,localReviewApi:async(path,opts)=>{calls.push({path,opts});if(opts.method==='DELETE')return {state:'already_completed',seq:4};return {entries:path.includes(other)?[localReviewRow('pending',id,{text:'second copy',stage_ids:['3'.repeat(16)]})]:[]};}}});
  const old=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-local-discard'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-local-discard(?:\s+[^"\s]+)*" data-row="(\d+)" data-id="([0-9a-f]+)"/g)].map(m=>el('button',{dataset:{row:m[1],id:m[2]}}))):old(s);
  try {ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('draft preserved');ctx.roots.prompt.firstListener('input')();await localReviewTick();assert.doesNotMatch(ctx.roots.attstage.innerHTML,/No saved local/);assert.doesNotMatch(ctx.roots.attstage.innerHTML,new RegExp('Local copy '+localReviewKey));assert.match(ctx.roots.attstage.innerHTML,/second copy/);assert.equal(buttons.length,1);assert.equal(buttons[0].dataset.row,'1');buttons[0].click();await localReviewTick();assert.equal(calls.length,3);assert.equal(calls[2].path,`/api/team/subscriptions/${other}/pending/${id}?copy_instance=${copy}&revision=${'e'.repeat(64)}`);assert.match(ctx.roots.attstage.innerHTML,/role="status"[^>]*>Already completed at sequence 4/);assert.equal(ctx.feature.promptText(),'draft preserved');} finally {ctx.feature.destroy();}
});

test('browser pending state is read by publication key without writes', async () => {
  const publicationKey='2'.repeat(16),copyInstance='1'.repeat(32),storageKey='scimux-people-file-pending:'+localReviewKey+'::copy:'+copyInstance;
  const value=JSON.stringify({instance:'',copyInstance,request:{text:'saved synthetic',anchor:0,key:publicationKey,attachments:['3'.repeat(16)]}});
  const pending={key:localReviewKey,storageKey,value,text:'saved synthetic',attachments:['3'.repeat(16)],instance:''},calls=[];
  const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance,writable:false}),pendingPeople:()=>pending,localReviewApi:async(path,opts)=>{calls.push({path,opts});return path.includes('/pending/')?{state:'missing'}:{entries:[]};}}});
  try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(calls.length,2);assert.ok(calls.some(x=>x.path===`/api/team/subscriptions/${localReviewKey}/pending/${publicationKey}?copy_instance=${copyInstance}`));assert.ok(calls.every(x=>x.opts.method==='GET'));assert.match(ctx.roots.attstage.innerHTML,/no local publication/i);assert.match(ctx.roots.attstage.innerHTML,/saved synthetic/);} finally {ctx.feature.destroy();}
});

test('exact browser pending states show only actual evidence', async () => {
  const copyInstance='1'.repeat(32),id='2'.repeat(16),revision='e'.repeat(64),stage='3'.repeat(16);
  const value=JSON.stringify({instance:'',copyInstance,request:{text:'browser text',anchor:0,key:id,attachments:[stage]}});
  const pending={key:localReviewKey,storageKey:'scimux-people-file-pending:'+localReviewKey+'::copy:'+copyInstance,value,text:'browser text',attachments:[stage],instance:''};
  for(const [state,projection,expected] of [
    ['missing',{state:'missing'},/no local publication/i], ['absent_retired',{state:'absent_retired'},/absent.*retired/i],
    ['pending',{state:'pending',revision,text:'actual local text',stage_ids:[stage]},/actual local text/],
    ['retired',{state:'retired',revision,text:'actual local text',stage_ids:[stage]},/retired/i],
    ['completed',{state:'completed',revision,text:'actual local text',stage_ids:[stage],seq:9,pending_pinned:true},/sequence 9/],
    ['completed-unpinned',{state:'completed',revision,stage_ids:[stage],seq:10},/sequence 10/]
  ]) {
    const calls=[];const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance,writable:false}),pendingPeople:()=>pending,localReviewApi:async(path,opts)=>{calls.push(opts.method);return path.includes('/pending/')?projection:{entries:[]};}}});
    try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,expected,state);if(projection.stage_ids){assert.match(ctx.roots.attstage.innerHTML,new RegExp(stage));assert.match(ctx.roots.attstage.innerHTML,/actual local/i);}if(projection.pending_pinned)assert.match(ctx.roots.attstage.innerHTML,/pinned/i);assert.deepEqual(calls,['GET','GET']);} finally {ctx.feature.destroy();}
  }
});

test('exact state ignores invalid browser identity and fences delayed results and errors', async () => {
  const copyInstance='1'.repeat(32),id='2'.repeat(16),stage='3'.repeat(16),value=JSON.stringify({instance:'',copyInstance,request:{text:'saved',anchor:0,key:id,attachments:[stage]}});
  for(const mode of ['invalid','navigation','copy','binding','destroy','error']) {
    let context={key:localReviewKey,copyInstance,binding:'one',writable:false},resolve,reject;const calls=[];const pending={key:localReviewKey,storageKey:'scimux-people-file-pending:'+localReviewKey+'::one:copy:'+copyInstance,value:mode==='invalid'?'{}':value,text:'saved',attachments:[stage],instance:''};
    const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>context,pendingPeople:()=>pending,localReviewApi:(path,opts)=>{calls.push({path,opts});if(!path.includes('/pending/'))return Promise.resolve({entries:[]});return new Promise((a,b)=>{resolve=a;reject=b;});}}});
    try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();if(mode==='invalid'){assert.equal(calls.length,1);continue;}assert.equal(calls.length,2);const firstResolve=resolve;if(mode==='navigation'){ctx.setSel('');ctx.feature.onSelect();}if(mode==='copy'){context={...context,copyInstance:'4'.repeat(32)};ctx.feature.contextChanged();}if(mode==='binding'){context={...context,binding:'two'};ctx.feature.contextChanged();}if(mode==='destroy')ctx.feature.destroy();if(mode==='error'){reject(Error('offline'));await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/state.*unavailable/i);assert.equal(pending.value,value);continue;}firstResolve({state:'completed',revision:'e'.repeat(64),stage_ids:[stage],seq:77});await localReviewTick();assert.doesNotMatch(ctx.roots.attstage.innerHTML,/sequence 77/,mode);assert.equal(pending.value,value);} finally {ctx.feature.destroy();}
  }
});


test('exact read never adopts a pending record from an older binding', async () => {
  const copyInstance='1'.repeat(32),id='2'.repeat(16),stage='3'.repeat(16);let calls=0;
  const value=JSON.stringify({instance:'',copyInstance,request:{text:'saved',anchor:0,key:id,attachments:[stage]}});
  const pending={key:localReviewKey,storageKey:'scimux-people-file-pending:'+localReviewKey+'::old:copy:'+copyInstance,value,text:'saved',attachments:[stage],instance:''};
  const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance,binding:'new'}),pendingPeople:()=>pending,localReviewApi:async(path)=>{calls++;return path.includes('/pending/')?{state:'missing'}:{entries:[]};}}});
  try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(calls,1);assert.equal(pending.value,value);} finally {ctx.feature.destroy();}
});

test('exact state handles owner identity, absent values, and failed pending lookup safely', async () => {
  const copyInstance='1'.repeat(32),instance='8'.repeat(32),stage='3'.repeat(16),value=JSON.stringify({instance,copyInstance,request:{text:'saved',anchor:0,key:'2'.repeat(16),attachments:[stage]}});
  let pending={key:localReviewKey,storageKey:'scimux-people-file-pending:'+localReviewKey+':'+instance+':copy:'+copyInstance,value,text:'saved',attachments:[stage],instance},calls=[];
  const ctx=makeFeature({nodes:[{id:'n1',instance_id:instance}],deps:{peopleContext:()=>({key:localReviewKey,copyInstance,owner:true,writable:true}),pendingPeople:()=>pending,localReviewApi:async(path,opts)=>{calls.push(opts.method);return path.includes('/pending/')?{state:'missing'}:{entries:[]};}}});
  try {ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setDestination('people');await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/no local publication/i);assert.ok(calls.length>=2);} finally {ctx.feature.destroy();}
  for(const result of [null,{value:'',attachments:[]},'throw']) {
    let reads=0;const c=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance}),pendingPeople:()=>{if(result==='throw')throw Error('synthetic');return result;},localReviewApi:async()=>{reads++;return {entries:[]};}}});
    try {c.feature.bind();c.feature.onSelect();await localReviewTick();assert.equal(reads,1);} finally {c.feature.destroy();}
  }
});

test('exact state suppresses a response when pending lookup fails after dispatch', async () => {
  const copyInstance='1'.repeat(32),stage='3'.repeat(16),value=JSON.stringify({instance:'',copyInstance,request:{text:'saved',anchor:0,key:'2'.repeat(16),attachments:[stage]}});let fail=false,resolve;
  const pending={key:localReviewKey,storageKey:'scimux-people-file-pending:'+localReviewKey+'::copy:'+copyInstance,value,text:'saved',attachments:[stage],instance:''};
  const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance}),pendingPeople:()=>{if(fail)throw Error('synthetic');return pending;},localReviewApi:path=>path.includes('/pending/')?new Promise(r=>{resolve=r;}):Promise.resolve({entries:[]})}});
  try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();fail=true;resolve({state:'completed',revision:'e'.repeat(64),stage_ids:[stage],seq:77});await localReviewTick();assert.doesNotMatch(ctx.roots.attstage.innerHTML,/sequence 77/);} finally {ctx.feature.destroy();}
});

test('completed browser review separates publication references from current stages', async () => {
  const copyInstance='1'.repeat(32),stage='3'.repeat(16),value=JSON.stringify({instance:'',copyInstance,request:{text:'saved',anchor:0,key:'2'.repeat(16),attachments:[stage]}});
  const pending={key:localReviewKey,storageKey:'scimux-people-file-pending:'+localReviewKey+'::copy:'+copyInstance,value,text:'saved',attachments:[stage],instance:''};
  const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance}),pendingPeople:()=>pending,localReviewApi:async(path)=>path.includes('/pending/')?{state:'completed',revision:'e'.repeat(64),stage_ids:[stage,'4'.repeat(16)],seq:5}:{entries:[localReviewRow('stage',stage,{name:'actual.txt',mime:'text/plain',size:2,hash:'f'.repeat(64)})]}}});
  try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/sequence 5/);assert.match(ctx.roots.attstage.innerHTML,/Actual local text/);assert.match(ctx.roots.attstage.innerHTML,/Actual local stage IDs: 3333333333333333, 4444444444444444<\/p>/);assert.match(ctx.roots.attstage.innerHTML,/Review current staged files \(inventory snapshot\).*actual\.txt/);assert.doesNotMatch(ctx.roots.attstage.innerHTML,/3333333333333333 — actual\.txt/);} finally {ctx.feature.destroy();}
});

test('publication completion is fenced between adapter validation and rendering', async () => {
  const copyInstance='1'.repeat(32),stage='3'.repeat(16),value=JSON.stringify({instance:'',copyInstance,request:{text:'saved',anchor:0,key:'2'.repeat(16),attachments:[stage]}});let arm=false,changed=false;
  const pending={key:localReviewKey,storageKey:'scimux-people-file-pending:'+localReviewKey+'::copy:'+copyInstance,value,text:'saved',attachments:[stage],instance:''};
  const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance}),pendingPeople:()=>{if(arm&&!changed){arm=false;queueMicrotask(()=>{changed=true;});}return changed?null:pending;},localReviewApi:async(path)=>{if(path.includes('/pending/')){arm=true;return {state:'completed',revision:'e'.repeat(64),stage_ids:[stage],seq:77};}return {entries:[]};}}});
  try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(changed,true);assert.doesNotMatch(ctx.roots.attstage.innerHTML,/sequence 77/);} finally {ctx.feature.destroy();}
});

test('invalid browser snapshots cannot issue exact publication reads', async () => {
  const copyInstance='1'.repeat(32),stage='3'.repeat(16),request={text:'saved',anchor:0,key:'2'.repeat(16),attachments:[stage]},base={instance:'',copyInstance,request};
  const inputs=[null,{}, {...base,instance:'bad'}, {...base,copyInstance:'bad'}, {...base,request:{...request,key:'bad'}}, {...base,request:{...request,text:'é'.repeat(8193)}}, {...base,request:{...request,anchor:-1}}, {...base,request:{...request,attachments:[]}}, {...base,request:{...request,attachments:['invalid']}}, {...base,request:{...request,attachments:[stage,stage]}}];
  for(const saved of inputs){let exact=0;const value=JSON.stringify(saved),pending={key:localReviewKey,storageKey:'scimux-people-file-pending:'+localReviewKey+'::copy:'+copyInstance,value,text:'saved',attachments:[stage],instance:''};const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance}),pendingPeople:()=>pending,localReviewApi:async(path)=>{if(path.includes('/pending/'))exact++;return {entries:[]};}}});try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(exact,0);}finally{ctx.feature.destroy();}}
});

test('late exact-state failures cannot replace a newer visit', async () => {
  for (const change of ['navigation','destroy','rebind','destination','native']) {
    const copyInstance='1'.repeat(32),instance='8'.repeat(32),stage='3'.repeat(16),value=JSON.stringify({instance,copyInstance,request:{text:'saved',anchor:0,key:'2'.repeat(16),attachments:[stage]}}),nodes=[{id:'n1',instance_id:instance}];let reject;
    const pending={key:localReviewKey,storageKey:'scimux-people-file-pending:'+localReviewKey+':'+instance+':copy:'+copyInstance,value,text:'saved',attachments:[stage],instance};
    const ctx=makeFeature({nodes,deps:{peopleContext:()=>({key:localReviewKey,copyInstance,owner:true,writable:true}),pendingPeople:()=>pending,localReviewApi:path=>path.includes('/pending/')?new Promise((_,r)=>{if(!reject)reject=r;}):Promise.resolve({entries:[]})}});
    try {ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setDestination('people');await localReviewTick();assert.equal(typeof reject,'function');if(change==='navigation'){ctx.setSel('');ctx.feature.onSelect();}if(change==='destroy')ctx.feature.destroy();if(change==='rebind'){ctx.feature.destroy();ctx.feature.bind();ctx.feature.onSelect();}if(change==='destination')ctx.feature.setDestination('agent');if(change==='native'){nodes[0].instance_id='9'.repeat(32);ctx.feature.contextChanged();}reject(Error('synthetic offline'));await localReviewTick();assert.doesNotMatch(ctx.roots.attstage.innerHTML,/Local publication state is unavailable/);assert.equal(pending.value,value);} finally {ctx.feature.destroy();}
  }
});

test('new browser pending snapshot is inspected once during the existing visit', async () => {
  const copyInstance='1'.repeat(32),stage='3'.repeat(16),value=JSON.stringify({instance:'',copyInstance,request:{text:'saved',anchor:0,key:'2'.repeat(16),attachments:[stage]}});let pending=null,exact=0;
  const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance}),pendingPeople:()=>pending,localReviewApi:async(path)=>{if(path.includes('/pending/')){exact++;return {state:'missing'};}return {entries:[]};}}});
  try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();pending={key:localReviewKey,storageKey:'scimux-people-file-pending:'+localReviewKey+'::copy:'+copyInstance,value,text:'saved',attachments:[stage],instance:''};ctx.feature.renderStage();await localReviewTick();assert.equal(exact,1);assert.match(ctx.roots.attstage.innerHTML,/no local publication/i);ctx.feature.renderStage();await localReviewTick();assert.equal(exact,1);} finally {ctx.feature.destroy();}
});

test('returning to an identical browser value cannot revive its earlier exact read', async () => {
  const copyInstance='1'.repeat(32),stage='3'.repeat(16),key='2'.repeat(16),storageKey='scimux-people-file-pending:'+localReviewKey+'::copy:'+copyInstance,releases=[];
  const makePending=text=>({key:localReviewKey,storageKey,value:JSON.stringify({instance:'',copyInstance,request:{text,anchor:0,key,attachments:[stage]}}),text,attachments:[stage],instance:''});
  const first=makePending('first'),second=makePending('second');let pending=first;
  const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance}),pendingPeople:()=>pending,localReviewApi:path=>path.includes('/pending/')?new Promise(resolve=>releases.push(resolve)):Promise.resolve({entries:[]})}});
  try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();pending=second;ctx.feature.renderStage();pending=first;ctx.feature.renderStage();assert.equal(releases.length,3);releases[0]({state:'completed',revision:'e'.repeat(64),stage_ids:[stage],seq:77});await localReviewTick();assert.doesNotMatch(ctx.roots.attstage.innerHTML,/sequence 77/);releases[2]({state:'missing'});await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/no local publication/i);} finally {ctx.feature.destroy();}
});


test('publication stage references never attribute a same-ID replacement to an earlier record', async () => {
  const copyInstance='1'.repeat(32),stage='3'.repeat(16),value=JSON.stringify({instance:'',copyInstance,request:{text:'browser draft',anchor:0,key:'2'.repeat(16),attachments:[stage]}});
  const pending={key:localReviewKey,storageKey:'scimux-people-file-pending:'+localReviewKey+'::copy:'+copyInstance,value,text:'browser draft',attachments:[stage],instance:''};
  for(const projection of [{state:'completed',revision:'e'.repeat(64),stage_ids:[stage],text:'original text',seq:7},{state:'completed',revision:'e'.repeat(64),stage_ids:[stage],text:'original text',seq:8,pending_pinned:true},{state:'completed',revision:'e'.repeat(64),stage_ids:[stage],text:'original text',seq:9,pending_pinned:false},{state:'retired',revision:'e'.repeat(64),stage_ids:[stage],text:'original text'},{state:'pending',revision:'e'.repeat(64),stage_ids:[stage],text:'original text'}]) for(const present of [true,false]) {
    const calls=[];const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance}),pendingPeople:()=>pending,localReviewApi:async(path,opts)=>{calls.push(opts.method);return path.includes('/pending/')?projection:{entries:present?[localReviewRow('stage',stage,{name:'replacement.txt',mime:'text/plain',size:2,hash:'f'.repeat(64)})]:[]};}}});
    try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();const html=ctx.roots.attstage.innerHTML;assert.match(html,/original text/);assert.match(html,/Actual local stage IDs: 3333333333333333<\/p>/);assert.doesNotMatch(html,/3333333333333333 — replacement\.txt/);if(present)assert.match(html,/Review current staged files \(inventory snapshot\).*replacement\.txt/);else assert.doesNotMatch(html,/replacement\.txt/);if(projection.seq)assert.match(html,new RegExp('sequence '+projection.seq));if(projection.pending_pinned)assert.match(html,/pinned/);assert.deepEqual(calls,['GET','GET']);} finally {ctx.feature.destroy();}
  }
});

test('pending inventory file names are identified as current stage observations', async () => {
  const id='2'.repeat(16),stage='3'.repeat(16),pending={key:localReviewKey,text:'saved',attachments:[stage],value:JSON.stringify({request:{key:id}})};
  const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance:'1'.repeat(32)}),pendingPeople:()=>pending,localReviewApi:async()=>({entries:[localReviewRow('pending',id,{text:'saved',stage_ids:[stage]}),localReviewRow('stage',stage,{name:'current.txt',mime:'text/plain',size:1,hash:'f'.repeat(64)}),localReviewRow('pending','4'.repeat(16),{text:'other',stage_ids:[stage]})]})}});
  try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/Actual local files: current\.txt.*current stage inventory snapshot/i);assert.match(ctx.roots.attstage.innerHTML,/Current stage inventory snapshot \(IDs may be reused\): current\.txt/);} finally {ctx.feature.destroy();}
});

function retainedBrowserReviewFixture({copies, records, unavailable=false, pending=null, owner=true, writable=false, stateResponse, lookupFn}={}) {
  const instance='8'.repeat(32), copyInstance='1'.repeat(32), copy={key:localReviewKey,copyInstance,binding:'current',owner};
  const contexts=copies||[copy];let selectedContext=owner?null:{...copy,writable},reads=0; const calls=[], snapshots=records||[];
  const ctx=makeFeature({sel:owner?'n1':'team/'+localReviewKey,nodes:owner?[{id:'n1',instance_id:instance}]:[],deps:{peopleContext:()=>selectedContext,localReviewContexts:()=>contexts,pendingPeople:()=>pending,readRetainedPeoplePending:c=>{reads++;return lookupFn?lookupFn(c):{entries:snapshots.filter(x=>x.key===c.key&&x.copyInstance===c.copyInstance),unavailable};},localReviewApi:async(path,options)=>{calls.push({path,options});return path.includes('/pending/')?(stateResponse?stateResponse(path,options):{state:'missing'}):{entries:[]};}}});
  const original=ctx.roots.attstage.querySelectorAll;let details=[];
  ctx.roots.attstage.querySelectorAll=s=>s==='.people-local-snapshot'?(details=[...ctx.roots.attstage.innerHTML.matchAll(/class="people-local-snapshot" data-row="(\d+)" data-snapshot="(\d+)"/g)].map(m=>el('details',{dataset:{row:m[1],snapshot:m[2]}}))):original(s);
  return {ctx,calls,details:()=>details,reads:()=>reads,contexts,snapshots,setContext:v=>{selectedContext=v;}};
}
function retainedBrowserSnapshot({copyInstance='1'.repeat(32),binding='old',instance='8'.repeat(32),key=localReviewKey,text='retained synthetic',publicationKey='2'.repeat(16)}={}) {
  const attachments=['3'.repeat(16)],value=JSON.stringify({instance,copyInstance,request:{text,anchor:0,key:publicationKey,attachments}}),storageKey=`scimux-people-file-pending:${key}:${instance}${binding?':'+binding:''}:copy:${copyInstance}`;
  return {key,copyInstance,binding,instance,storageKey,value,publicationKey,text,attachments};
}

test('inactive owner reviews original browser binding lazily while Agent draft remains untouched', async () => {
  const record=retainedBrowserSnapshot(),f=retainedBrowserReviewFixture({records:[record]});
  try {f.ctx.feature.bind();f.ctx.feature.onSelect();f.ctx.feature.setPromptText('agent draft');f.ctx.roots.prompt.firstListener('input')();await localReviewTick();assert.equal(f.ctx.feature.destination(),'agent');assert.equal(f.ctx.feature.promptText(),'agent draft');assert.match(f.ctx.roots.attstage.innerHTML,/retained synthetic/);assert.match(f.ctx.roots.attstage.innerHTML,/old/);assert.doesNotMatch(f.ctx.roots.attstage.innerHTML,/people-retry/);assert.deepEqual(f.calls.map(x=>x.options.method),['GET']);assert.equal(f.details().length,1);const detail=f.details()[0];detail.open=true;detail.firstListener('toggle')();await localReviewTick();assert.equal(f.calls.length,2);assert.equal(f.calls[1].path,`/api/team/subscriptions/${localReviewKey}/pending/${record.publicationKey}?copy_instance=${record.copyInstance}`);assert.match(f.ctx.roots.attstage.innerHTML,/No local publication record/);assert.equal(f.ctx.feature.promptText(),'agent draft');assert.ok(f.calls.every(x=>x.options.method==='GET'));} finally {f.ctx.feature.destroy();}
});

test('retained snapshots preserve distinct bindings and copies without duplicating active snapshot', async () => {
  const copyInstance='1'.repeat(32),active=retainedBrowserSnapshot({instance:'',binding:'current',text:'active'}),old=retainedBrowserSnapshot({instance:'',binding:'old',text:'historical'}),f=retainedBrowserReviewFixture({owner:false,writable:true,records:[active,old],pending:active});
  try {f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();assert.equal((f.ctx.roots.attstage.innerHTML.match(/active/g)||[]).length,1);assert.match(f.ctx.roots.attstage.innerHTML,/historical/);assert.equal(f.details().length,1);assert.equal(f.calls.filter(c=>c.path.includes('/pending/')).length,1);assert.match(f.ctx.roots.attstage.innerHTML,/people-retry/);}finally{f.ctx.feature.destroy();}
});

test('retained lookup availability error preserves other captured rows',async()=>{
  const f=retainedBrowserReviewFixture({records:[retainedBrowserSnapshot()],unavailable:true});try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();assert.match(f.ctx.roots.attstage.innerHTML,/retained synthetic/);assert.match(f.ctx.roots.attstage.innerHTML,/saved.*unavailable/i);assert.equal(f.calls.length,1);}finally{f.ctx.feature.destroy();}
});

test('lazy retained reads fence stale success and failure across identity changes', async () => {
  for(const change of ['selection','copy','binding','native','destroy','rebind','raw']) for(const reject of [false,true]) {
    let settle;const record=retainedBrowserSnapshot(),f=retainedBrowserReviewFixture({records:[record],stateResponse:()=>new Promise((resolve,fail)=>{settle=()=>reject?fail(Error('synthetic offline')):resolve({state:'completed',revision:'e'.repeat(64),stage_ids:['3'.repeat(16)],seq:77});})});
    try {f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();const detail=f.details()[0];detail.open=true;detail.firstListener('toggle')();assert.equal(typeof settle,'function');if(change==='selection'){f.ctx.setSel('');f.ctx.feature.onSelect();}if(change==='copy'){f.contexts[0]={...f.contexts[0],copyInstance:'9'.repeat(32)};f.ctx.feature.contextChanged();}if(change==='binding'){f.contexts[0]={...f.contexts[0],binding:'new'};f.ctx.feature.contextChanged();}if(change==='native'){f.ctx.nodes[0].instance_id='9'.repeat(32);f.ctx.feature.contextChanged();}if(change==='destroy')f.ctx.feature.destroy();if(change==='rebind'){f.ctx.feature.destroy();f.ctx.feature.bind();f.ctx.feature.onSelect();}if(change==='raw')f.snapshots[0]={...record,value:record.value+' '};settle();await localReviewTick();assert.doesNotMatch(f.ctx.roots.attstage.innerHTML,/sequence 77|publication state is unavailable/i,change);assert.ok(f.calls.every(x=>x.options.method==='GET'));} finally {f.ctx.feature.destroy();}
  }
});

test('retained copies and original bindings remain distinct even for the same publication key', async () => {
  const other=localReviewKey.slice(0,-16)+'9'.repeat(16),copies=[{key:localReviewKey,copyInstance:'1'.repeat(32),binding:'new',owner:true},{key:other,copyInstance:'4'.repeat(32),binding:'new',owner:true}],records=[retainedBrowserSnapshot({binding:'old-one',text:'first saved'}),retainedBrowserSnapshot({binding:'old-two',text:'second saved'}),retainedBrowserSnapshot({key:other,copyInstance:'4'.repeat(32),binding:'old-three',text:'third saved'})],f=retainedBrowserReviewFixture({copies,records});
  try {f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();assert.equal(f.details().length,3);assert.match(f.ctx.roots.attstage.innerHTML,/first saved/);assert.match(f.ctx.roots.attstage.innerHTML,/second saved/);assert.match(f.ctx.roots.attstage.innerHTML,/third saved/);assert.equal(f.calls.length,2);const d=f.details()[2];d.open=true;d.firstListener('toggle')();await localReviewTick();assert.equal(f.calls[2].path,`/api/team/subscriptions/${other}/pending/${records[2].publicationKey}?copy_instance=${records[2].copyInstance}`);assert.equal(f.calls.length,3);}finally{f.ctx.feature.destroy();}
});

test('malformed retained row does not hide a valid row or dispatch state reads', async () => {
  const good=retainedBrowserSnapshot(),bad={...retainedBrowserSnapshot({binding:'broken'}),value:'{invalid'},f=retainedBrowserReviewFixture({records:[bad,good]});
  try {f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();assert.match(f.ctx.roots.attstage.innerHTML,/retained synthetic/);assert.match(f.ctx.roots.attstage.innerHTML,/saved.*unavailable/i);assert.equal(f.details().length,1);assert.equal(f.calls.length,1);}finally{f.ctx.feature.destroy();}
});

test('transactional retained review rejects missing draft identity without exact reads',async()=>{
 for(const missing of ['head','version']){const base=retainedBrowserSnapshot(),publication='a'.repeat(32)+':1',record={...base,format:'transactional',scope:`people\0${base.key}\0binding:${base.binding}\0copy:${base.copyInstance}\0n1\0${base.instance}`,publication,storageKey:'idb:'+publication,head:'head',version:'version'};delete record[missing];const f=retainedBrowserReviewFixture({records:[record]});
  try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();assert.equal(f.details().length,0);assert.match(f.ctx.roots.attstage.innerHTML,/saved.*unavailable/i);assert.equal(f.calls.length,1);assert.equal(f.ctx.apiCalls.length,0);}finally{f.ctx.feature.destroy();}}
});

test('retained review never loads hundreds of exact states until explicitly opened', async () => {
  const records=Array.from({length:128},(_,i)=>retainedBrowserSnapshot({binding:'old-'+i,text:'saved '+i})),f=retainedBrowserReviewFixture({records});
  try {f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();assert.equal(f.details().length,128);assert.equal(f.calls.length,1);const d=f.details()[17];d.open=true;d.firstListener('toggle')();await localReviewTick();assert.equal(f.calls.length,2);f.ctx.feature.renderStage();await localReviewTick();assert.equal(f.calls.length,2);}finally{f.ctx.feature.destroy();}
});

test('stale retained toggle cannot repaint a replacement visit', async () => {
  const f=retainedBrowserReviewFixture({records:[retainedBrowserSnapshot()]});let paints=0,html='';Object.defineProperty(f.ctx.roots.attstage,'innerHTML',{get:()=>html,set:v=>{html=v;paints++;}});
  try {f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();const detail=f.details()[0],oldToggle=detail.firstListener('toggle');f.ctx.setSel('');f.ctx.feature.onSelect();const before=paints;detail.open=true;oldToggle();assert.equal(paints,before);assert.equal(f.calls.length,1);}finally{f.ctx.feature.destroy();}
});

test('Agent staged file and draft remain intact during retained People review', async () => {
  const f=retainedBrowserReviewFixture({records:[retainedBrowserSnapshot()]});try {f.ctx.feature.bind();f.ctx.feature.onSelect();f.ctx.feature.setPromptText('agent synthetic');f.ctx.roots.prompt.firstListener('input')();f.ctx.feature.setAttachAvail(true);f.ctx.feature.addFiles([{name:'agent-file.txt',type:'text/plain',size:1}]);await localReviewTick();const apiBefore=f.ctx.apiCalls.length,fetchBefore=f.ctx.fetchCalls.length;assert.match(f.ctx.roots.attstage.innerHTML,/agent-file.txt/);const detail=f.details()[0];detail.open=true;detail.firstListener('toggle')();await localReviewTick();assert.match(f.ctx.roots.attstage.innerHTML,/agent-file.txt/);assert.equal(f.ctx.feature.promptText(),'agent synthetic');assert.equal(f.ctx.apiCalls.length,apiBefore);assert.equal(f.ctx.fetchCalls.length,fetchBefore);}finally{f.ctx.feature.destroy();}
});

test('retained lookup malformed results and failed exact reads report availability', async () => {
  for (const lookupFn of [()=>null,()=>({entries:null,unavailable:true}),()=>{throw Error('synthetic');},()=>({entries:[{...retainedBrowserSnapshot(),publicationKey:'4'.repeat(16)}],unavailable:false})]) {
    const f=retainedBrowserReviewFixture({lookupFn});try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();assert.match(f.ctx.roots.attstage.innerHTML,/saved.*unavailable/i);assert.equal(f.calls.length,1);}finally{f.ctx.feature.destroy();}
  }
  const f=retainedBrowserReviewFixture({records:[retainedBrowserSnapshot({binding:''})],stateResponse:async()=>{throw Error('synthetic');}});
  try {f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();assert.match(f.ctx.roots.attstage.innerHTML,/No saved binding/);const detail=f.details()[0];detail.open=true;detail.firstListener('toggle')();await localReviewTick();assert.match(f.ctx.roots.attstage.innerHTML,/Local publication state is unavailable/);assert.equal(f.calls.length,2);}finally{f.ctx.feature.destroy();}
});

test('forged retained toggle and changed snapshot before opening cannot issue requests', async () => {
  let valid=true;const record=retainedBrowserSnapshot(),f=retainedBrowserReviewFixture({lookupFn:()=>{if(!valid)throw Error('synthetic');return {entries:[record],unavailable:false};}});
  const old=f.ctx.roots.attstage.querySelectorAll,forged=el('details',{dataset:{row:'999',snapshot:'1'}});
  f.ctx.roots.attstage.querySelectorAll=s=>s==='.people-local-snapshot'?[...old(s),forged]:old(s);
  try {f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();assert.equal(forged.listenerCount('toggle'),0);const detail=f.details()[0];valid=false;detail.open=true;detail.firstListener('toggle')();await localReviewTick();assert.equal(f.calls.length,1);}finally{f.ctx.feature.destroy();}
});

test('retained review fences a replacement between adapter validation and display', async () => {
  let arm=false,changed=false;const record=retainedBrowserSnapshot();const f=retainedBrowserReviewFixture({lookupFn:()=>{if(arm&&!changed){arm=false;queueMicrotask(()=>{changed=true;});}return {entries:[changed?{...record,value:record.value+' '}:record],unavailable:false};},stateResponse:async()=>{arm=true;return {state:'completed',revision:'e'.repeat(64),stage_ids:['3'.repeat(16)],seq:77};}});
  try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();const detail=f.details()[0];detail.open=true;detail.firstListener('toggle')();await localReviewTick();assert.equal(changed,true);assert.doesNotMatch(f.ctx.roots.attstage.innerHTML,/sequence 77/);}finally{f.ctx.feature.destroy();}
});

test('retained exact read is not duplicated and can be retried by revisiting', async () => {
  let fail=true;const f=retainedBrowserReviewFixture({records:[retainedBrowserSnapshot()],stateResponse:async()=>{if(fail)throw Error('synthetic');return {state:'missing'};}});
  try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();const detail=f.details()[0],toggle=detail.firstListener('toggle');detail.open=true;toggle();toggle();await localReviewTick();assert.equal(f.calls.length,2);assert.match(f.ctx.roots.attstage.innerHTML,/state is unavailable/i);fail=false;f.ctx.setSel('');f.ctx.feature.onSelect();f.ctx.setSel('n1');f.ctx.feature.onSelect();await localReviewTick();const again=f.details()[0];again.open=true;again.firstListener('toggle')();await localReviewTick();assert.match(f.ctx.roots.attstage.innerHTML,/No local publication record/);assert.equal(f.calls.filter(x=>x.path.includes('/pending/')).length,2);}finally{f.ctx.feature.destroy();}
});

test('ended received copy shows retained retry without writable action', async () => {
  const record=retainedBrowserSnapshot({instance:'',binding:'prior'}),f=retainedBrowserReviewFixture({owner:false,records:[record]});
  try {f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();assert.match(f.ctx.roots.attstage.innerHTML,/retained synthetic/);assert.doesNotMatch(f.ctx.roots.attstage.innerHTML,/people-retry/);const detail=f.details()[0];detail.open=true;detail.firstListener('toggle')();await localReviewTick();assert.match(f.ctx.roots.attstage.innerHTML,/No local publication record/);assert.equal(f.calls.length,2);assert.ok(f.calls.every(x=>x.options.method==='GET'));}finally{f.ctx.feature.destroy();}
});

test('destination change fences a delayed retained result for an active owner', async () => {
  let resolve;const record=retainedBrowserSnapshot(),f=retainedBrowserReviewFixture({records:[record],stateResponse:()=>new Promise(r=>{resolve=r;})});f.setContext({...f.contexts[0],writable:true});
  try {f.ctx.feature.bind();f.ctx.feature.onSelect();f.ctx.feature.setDestination('people');await localReviewTick();const detail=f.details()[0];detail.open=true;detail.firstListener('toggle')();f.ctx.feature.setDestination('agent');resolve({state:'completed',revision:'e'.repeat(64),stage_ids:['3'.repeat(16)],seq:77});await localReviewTick();assert.doesNotMatch(f.ctx.roots.attstage.innerHTML,/sequence 77/);assert.equal(f.ctx.feature.destination(),'agent');}finally{f.ctx.feature.destroy();}
});

test('a retained snapshot of an unwritable current copy offers review without retry', async () => {
  const active=retainedBrowserSnapshot({instance:'',binding:'current',text:'saved current'}),f=retainedBrowserReviewFixture({owner:false,records:[active],pending:active});
  try {f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();assert.match(f.ctx.roots.attstage.innerHTML,/saved current/);assert.doesNotMatch(f.ctx.roots.attstage.innerHTML,/people-retry/);assert.equal(f.calls.filter(x=>x.path.includes('/pending/')).length,1);}finally{f.ctx.feature.destroy();}
});

test('captured Retry control refuses dispatch after current writability is lost', async () => {
  for(const writable of [false,undefined]) {
    let active=true,retries=0;const pending={key:localReviewKey,instance:'',text:'saved',attachments:[]};
    const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance:'1'.repeat(32),writable:active?true:writable,attachments:true}),pendingPeople:()=>pending,retryPeople:async()=>{retries++;return {seq:1};}}});
    try{ctx.feature.bind();ctx.feature.onSelect();const retry=ctx.roots.attstage._retryButtons[0].firstListener('click');active=false;retry();await localReviewTick();assert.equal(retries,0);assert.equal(ctx.apiCalls.length,0);assert.equal(ctx.alerts.length,0);}finally{ctx.feature.destroy();}
  }
});

test('unwritable or unspecified capability keeps active pending review visible without retry', async () => {
  for(const writable of [false,undefined]) {
    const record=retainedBrowserSnapshot({instance:'',binding:'current',text:'saved current'}),f=retainedBrowserReviewFixture({owner:false,writable,records:[record],pending:record});
    try{f.setContext({...f.contexts[0],writable});f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();assert.match(f.ctx.roots.attstage.innerHTML,/saved current/);assert.match(f.ctx.roots.attstage.innerHTML,/No local publication record/);assert.doesNotMatch(f.ctx.roots.attstage.innerHTML,/people-retry/);}finally{f.ctx.feature.destroy();}
  }
});

test('opening retained review does not write browser storage or send agent input', async () => {
  const f=retainedBrowserReviewFixture({records:[retainedBrowserSnapshot()]});let writes=0;const oldSet=f.ctx.storage.setItem,oldRemove=f.ctx.storage.removeItem;
  try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();f.ctx.storage.setItem=(...args)=>{writes++;return oldSet(...args);};f.ctx.storage.removeItem=(...args)=>{writes++;return oldRemove(...args);};const agentCalls=f.ctx.apiCalls.length,detail=f.details()[0];detail.open=true;detail.firstListener('toggle')();await localReviewTick();assert.equal(writes,0);assert.equal(f.ctx.apiCalls.length,agentCalls);assert.ok(f.calls.every(call=>call.options.method==='GET'));}finally{f.ctx.feature.destroy();}
});

test('destroyed People file finalization cannot affect a new flight after rebind',async()=>{
 for(const outcome of ['resolve','reject']){
  let settleOld,releaseNew,signalOld,decisions=0;const oldReady=new Promise(resolve=>{signalOld=resolve;}),f=transactionalPeopleFixture({respond:()=>f.sends.length===1?Promise.resolve({seq:7}):new Promise(resolve=>{releaseNew=resolve;}),afterDecision:()=>++decisions===1?new Promise((resolve,reject)=>{settleOld=()=>outcome==='resolve'?resolve():reject(Error('old completion failed'));signalOld();}):Promise.resolve()}),ctx=f.ctx;
  try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('old caption');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await f.waitReady(1);const first=ctx.feature.sendPrompt();await oldReady;assert.equal(f.recovery().find(row=>row.kind==='decision').seq,7);assert.equal(f.files()[0].published,7);ctx.feature.destroy();ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();ctx.feature.setPromptText('new caption');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'new.txt',type:'text/plain',size:1}]);await f.waitReady(2);const second=ctx.feature.sendPrompt();await Promise.race([f.waitSend(2),second]);assert.equal(f.sendEntries(),2);assert.equal(f.files().find(row=>row.file.name==='saved.txt').published,7);assert.equal(f.files().find(row=>row.file.name==='new.txt').published,undefined);assert.equal(ctx.feature.promptText(),'new caption');assert.deepEqual({attachments:f.sendAttempts[1].attachments,dispatches:f.sends.length},{attachments:['fedcba9876543210'],dispatches:2},'rebound send must exclude the already completed file and dispatch the new one');settleOld();await first;assert.equal(ctx.feature.promptText(),'new caption');assert.deepEqual(ctx.alerts,[]);assert.equal(ctx.apiCalls.filter(call=>call.opts?.method==='DELETE').length,0);assert.equal(f.events.filter(event=>event==='remove').length,0);assert.equal(f.recovery().find(row=>row.kind==='publication').publication,f.sends[1].snapshot.publication);const buttons=ctx.roots.attstage._stageButtons;buttons.at(-1).firstListener('click')();await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/new.txt/);await ctx.feature.sendPrompt();assert.equal(f.sendEntries(),2,'old finally must not release the newer flight');assert.equal(f.sends.length,2);releaseNew({seq:8});await second;}finally{if(settleOld)settleOld();if(releaseNew)releaseNew({seq:8});ctx.feature.destroy();}
 }
});

test('late People send response from a destroyed lifetime cannot finish or alert after rebind', async () => {
 for (const outcome of ['resolve','reject']) {
  let settle, calls=0, finished=0;
  const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true,attachments:true}),sendPeople:()=>{calls++;return new Promise((resolve,reject)=>{settle=()=>outcome==='resolve'?resolve({seq:7}):reject(Error('old network failure'));});},finishPeople:()=>{finished++;return true}},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:'0123456789abcdef'}]})})});
  try {ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('old');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await localReviewTick();const first=ctx.feature.sendPrompt();await localReviewTick();assert.equal(calls,1);ctx.feature.destroy();ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('new');ctx.roots.prompt.firstListener('input')();settle();await first;await localReviewTick();assert.equal(finished,0);assert.equal(ctx.feature.promptText(),'new');assert.match(ctx.roots.attstage.innerHTML,/saved.txt/);assert.deepEqual(ctx.alerts,[]);assert.equal(ctx.apiCalls.filter(c=>c.opts?.method==='DELETE').length,0);} finally {ctx.feature.destroy();}
 }
});

test('People file send cannot finalize after its binding or copy is replaced', async () => {
 for (const change of ['binding','copy']) {
  let context={key:'s/o/c/g',copyInstance:'1'.repeat(32),binding:'old',writable:true,owner:true,attachments:true},release,finished=0;
  const ctx=makeFeature({nodes:[{id:'n1',instance_id:'native'}],deps:{peopleContext:()=>context,sendPeople:()=>new Promise(resolve=>{release=resolve;}),finishPeople:()=>{finished++;return true}},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:'0123456789abcdef'}]})})});
  try {ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('old');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await localReviewTick();const first=ctx.feature.sendPrompt();await localReviewTick();context={...context,...(change==='binding'?{binding:'new'}:{copyInstance:'2'.repeat(32)})};ctx.feature.contextChanged();ctx.feature.setPromptText('new');ctx.roots.prompt.firstListener('input')();release({seq:7});await first;await localReviewTick();assert.equal(finished,0,change);assert.equal(ctx.feature.promptText(),'new');assert.equal(ctx.apiCalls.filter(c=>c.opts?.method==='DELETE').length,0);assert.deepEqual(ctx.alerts,[]);} finally {ctx.feature.destroy();}
 }
});

test('destroyed People retry cannot finalize an old receipt in a rebound composer', async () => {
 let release,finished=0;const pending={key:'s/o/c/g',text:'saved',attachments:['0123456789abcdef']};
 const store={list:async()=>[{id:pending.attachments[0],file:{name:'saved.txt',type:'text/plain',size:1}}],markPublishedBatch:async()=>{assert.fail('old retry must not mark');},remove:async()=>assert.fail('old retry must not remove')};
 const ctx=makeFeature({deps:{peopleContext:()=>({key:pending.key,writable:true,owner:true,attachments:true}),peopleFileStore:store,pendingPeople:()=>pending,retryPeople:()=>new Promise(resolve=>{release=resolve;}),finishPeople:()=>{finished++;return true}}});
 try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();ctx.roots.attstage._retryButtons[0].click();assert.equal(typeof release,'function');ctx.feature.destroy();ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('new');ctx.roots.prompt.firstListener('input')();release({seq:7});await localReviewTick();assert.equal(finished,0);assert.equal(ctx.feature.promptText(),'new');assert.deepEqual(ctx.alerts,[]);} finally {ctx.feature.destroy();}
});

test('People flight remains fenced after the selected binding changes away and back', async () => {
 let context={key:'s/o/c/g',binding:'old',writable:true,owner:true,attachments:true},release,finished=0;
 const ctx=makeFeature({deps:{peopleContext:()=>context,sendPeople:()=>new Promise(resolve=>{release=resolve;}),finishPeople:()=>{finished++;return true}},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:'0123456789abcdef'}]})})});
 try {ctx.feature.bind();ctx.feature.onSelect();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await localReviewTick();const first=ctx.feature.sendPrompt();await localReviewTick();context={...context,binding:'new'};ctx.feature.contextChanged();context={...context,binding:'old'};ctx.feature.contextChanged();release({seq:7});await first;assert.equal(finished,0);assert.equal(ctx.apiCalls.filter(c=>c.opts?.method==='DELETE').length,0);} finally {ctx.feature.destroy();}
});

test('destroy during a confirmed stage delete stops subsequent browser cleanup', async () => {
 let release,ready;const deleting=new Promise(resolve=>{ready=resolve;}),f=transactionalPeopleFixture({sendHandler:async(_path,opts)=>{if(opts?.method==='DELETE'){ready();return new Promise(resolve=>{release=resolve;});}return {};}}),ctx=f.ctx;
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('old');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await f.waitReady(1);const flight=ctx.feature.sendPrompt();await deleting;assert.equal(f.sendEntries(),1);assert.equal(f.sends.length,1);assert.equal(f.events.filter(event=>event==='finish').length,1);assert.equal(f.recovery().find(row=>row.kind==='decision').seq,7);assert.equal(f.files()[0].published,7);const saved=f.files()[0];ctx.feature.destroy();ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('new');ctx.roots.prompt.firstListener('input')();release({});await flight;assert.equal(f.files().length,1);assert.equal(f.files()[0].fileToken,saved.fileToken);assert.equal(f.files()[0].published,7);assert.equal(ctx.feature.promptText(),'new');assert.deepEqual(ctx.alerts,[]);assert.equal(ctx.apiCalls.filter(call=>call.opts?.method==='DELETE').length,1);}finally{if(release)release({});ctx.feature.destroy();}
});

test('People retirement and feature closure errors do not alert', async () => {
 for(const code of ['PEOPLE_LOCAL_RETIRED','PEOPLE_FEATURE_CLOSED']) {
  const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true}),sendPeople:async()=>{throw Object.assign(Error('retired'),{code});}}});
  try {ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('saved');ctx.roots.prompt.firstListener('input')();await ctx.feature.sendPrompt();assert.equal(ctx.feature.promptText(),'saved');assert.deepEqual(ctx.alerts,[]);} finally {ctx.feature.destroy();}
 }
});

test('native replacement fences a held People file send', async () => {
 let release,finished=0;const ctx=makeFeature({nodes:[{id:'n1',instance_id:'old'}],deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true,attachments:true}),sendPeople:()=>new Promise(resolve=>{release=resolve;}),finishPeople:()=>{finished++;return true}},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:'0123456789abcdef'}]})})});
 try {ctx.feature.bind();ctx.feature.onSelect();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await localReviewTick();const flight=ctx.feature.sendPrompt();await localReviewTick();ctx.nodes[0].instance_id='new';ctx.feature.onSelect();release({seq:7});await flight;assert.equal(finished,0);assert.equal(ctx.apiCalls.filter(c=>c.opts?.method==='DELETE').length,0);assert.deepEqual(ctx.alerts,[]);} finally {ctx.feature.destroy();}
});

test('a destroyed People flight cannot continue after a held file cleanup settles', async () => {
 for(const where of ['server','browser'])for(const outcome of ['resolve','reject']){
  let serverResolve,serverReject,entered;const serverReady=new Promise(resolve=>{entered=resolve;}),f=transactionalPeopleFixture({sendHandler:async(_path,opts)=>{if(opts?.method==='DELETE'&&where==='server'){entered();return new Promise((resolve,reject)=>{serverResolve=resolve;serverReject=reject;});}return {};}}),ctx=f.ctx,gate=where==='browser'?f.holdNext('remove'):null;
  try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('old');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await f.waitReady(1);const flight=ctx.feature.sendPrompt();await (gate?gate.ready:serverReady);assert.equal(f.sends.length,1);assert.equal(f.sendEntries(),1);assert.equal(f.events.filter(event=>event==='finish').length,1);assert.equal(f.recovery().find(row=>row.kind==='decision').seq,7);assert.equal(f.files()[0].published,7);const original=f.files()[0];ctx.feature.destroy();ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('new');ctx.roots.prompt.firstListener('input')();if(gate){if(outcome==='resolve')gate.release();else gate.reject(Error('cleanup failed'));}else if(outcome==='resolve')serverResolve({});else serverReject(Error('cleanup failed'));await flight;assert.equal(ctx.apiCalls.filter(call=>call.opts?.method==='DELETE').length,1);assert.equal(f.events.filter(event=>event==='remove').length,where==='browser'?1:0);const rows=await f.actual.list(original.scope);assert.equal(rows.length,1);assert.equal(rows[0].fileToken,original.fileToken);assert.equal(rows[0].published,7);assert.equal(ctx.feature.promptText(),'new');assert.deepEqual(ctx.alerts,[]);await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/saved.txt/);}finally{if(gate)gate.release();if(serverResolve)serverResolve({});ctx.feature.destroy();}
 }
});

test('synchronous People file marker failures preserve the draft and saved file', async () => {
 const f=transactionalPeopleFixture(),ctx=f.ctx;
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('saved');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await f.waitReady(1);f.setDecisionFailure(true);await ctx.feature.sendPrompt();assert.equal(f.sendEntries(),1);assert.equal(f.sends.length,1);assert.equal(f.events.filter(event=>event==='finish').length,1);assert.equal(ctx.feature.promptText(),'saved');assert.match(ctx.roots.attstage.innerHTML,/saved.txt/);assert.match(ctx.alerts.join(' '),/storage unavailable/);assert.equal(f.recovery().filter(row=>row.kind==='decision').length,0);assert.equal(f.recovery().filter(row=>row.kind==='publication').length,1);assert.equal(f.files()[0].published,undefined);assert.equal(ctx.apiCalls.filter(call=>call.opts?.method==='DELETE').length,0);}finally{ctx.feature.destroy();}
});

test('People retry preserves a replacement draft association written during file finalization', async () => {
 let dispatch=0;const f=transactionalPeopleFixture({respond:async()=>{if(++dispatch===1)throw Error('synthetic offline');return {seq:9};}}),ctx=f.ctx,key='synthetic-replacement-association';
 try {ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('same caption');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await f.waitReady(1);await ctx.feature.sendPrompt();const pending=f.pending(),file=f.files()[0],original=JSON.stringify({binding:f.context.binding,text:'same caption',version:pending.version,publicationKey:pending.publicationKey,pendingValue:pending.value,pendingStorageKey:pending.storageKey,files:[{id:file.id,fileToken:file.fileToken,stageRevision:file.stageRevision}],flight:'first'}),replacement=JSON.stringify({...JSON.parse(original),flight:'second'});ctx.storage.setItem(key,original);const gate=f.holdNext('decision');ctx.roots.attstage._retryButtons[0].click();await gate.ready;const release=gate.release;assert.equal(typeof release,'function');ctx.storage.setItem(key,replacement);ctx.feature.setPromptText('same caption');ctx.roots.prompt.firstListener('input')();release();await f.waitDraft(2);await localReviewTick();assert.equal(ctx.storage.getItem(key),replacement);assert.equal(ctx.feature.promptText(),'same caption');assert.equal(f.sendEntries(),1);assert.equal(f.retries.length,1);assert.equal(f.recovery().find(row=>row.publication===pending.publication).state,'completed');assert.ok(f.recovery().some(row=>row.kind==='draft'&&row.text==='same caption'&&row.version!==pending.version));} finally {ctx.feature.destroy();}
});

test('destroy queued after a People receipt prevents a queued file marker from starting', async () => {
 let release;const f=transactionalPeopleFixture({respond:()=>new Promise(resolve=>{release=resolve;})}),ctx=f.ctx;
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await f.waitReady(1);const flight=ctx.feature.sendPrompt();await f.waitSend(1);assert.equal(f.sendEntries(),1);assert.equal(f.sends.length,1);release({seq:7});queueMicrotask(()=>ctx.feature.destroy());await flight;assert.equal(f.events.filter(event=>event==='finish').length,0);assert.equal(f.recovery().filter(row=>row.kind==='decision').length,0);assert.equal(f.recovery().filter(row=>row.kind==='publication').length,1);assert.equal(f.files()[0].published,undefined);assert.equal(ctx.apiCalls.filter(call=>call.opts?.method==='DELETE').length,0);assert.deepEqual(ctx.alerts,[]);}finally{ctx.feature.destroy();}
});

test('unavailable People receipt recording retains its draft and file', async () => {
 for (const finish of [undefined,()=>false]) {
  const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true,attachments:true}),sendPeople:async()=>({seq:7}),finishPeople:finish},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:'0123456789abcdef'}]})})});
  try {ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('saved');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await localReviewTick();await ctx.feature.sendPrompt();assert.equal(ctx.feature.promptText(),'saved');assert.match(ctx.roots.attstage.innerHTML,/saved.txt/);assert.match(ctx.alerts.join(' '),/could not be recorded/i);} finally {ctx.feature.destroy();}
 }
});

test('People retry can be attempted again after draft association storage fails',async()=>{
 let dispatch=0;const f=transactionalPeopleFixture({respond:async()=>{if(++dispatch===1)throw Error('synthetic offline');return {seq:7};}}),ctx=f.ctx;
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('saved');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await f.waitReady(1);await ctx.feature.sendPrompt();assert.equal(f.sendEntries(),1);assert.equal(f.sends.length,1);const original=f.pending();f.setDecisionFailure(true);ctx.roots.attstage._retryButtons[0].click();await f.waitRetry(1);await localReviewTick();assert.equal(f.retries.length,1);assert.equal(f.recovery().filter(row=>row.kind==='decision').length,0);assert.equal(f.recovery().find(row=>row.kind==='publication').publication,original.publication);assert.equal(f.files()[0].published,undefined);assert.match(ctx.roots.attstage.innerHTML,/saved.txt/);assert.match(ctx.alerts.join(' '),/storage unavailable/);f.setDecisionFailure(false);ctx.feature.renderStage();ctx.roots.attstage._retryButtons[0].click();await f.waitRetry(2);await localReviewTick();assert.equal(f.retries.length,2);assert.deepEqual(f.retries[0].request,f.sends[0].request);assert.deepEqual(f.retries[1].request,f.sends[0].request);assert.equal(f.recovery().find(row=>row.kind==='decision').seq,7);assert.equal(f.files().length,0);assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);}finally{ctx.feature.destroy();}
});

test('People marker settling before queued destruction cannot continue finalization',async()=>{
 const f=transactionalPeopleFixture(),ctx=f.ctx,gate=f.holdNext('decision');
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('saved');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await f.waitReady(1);const flight=ctx.feature.sendPrompt();await gate.ready;assert.equal(f.sendEntries(),1);assert.equal(f.sends.length,1);assert.equal(f.events.filter(event=>event==='finish').length,1);gate.release();queueMicrotask(()=>{ctx.feature.destroy();ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('new');ctx.roots.prompt.firstListener('input')();});await flight;assert.equal(f.events.filter(event=>event==='remove').length,0);assert.equal(ctx.apiCalls.filter(call=>call.opts?.method==='DELETE').length,0);assert.equal(f.files().length,1);assert.equal(f.files()[0].published,7);assert.equal(f.recovery().find(row=>row.kind==='decision').seq,7);assert.equal(ctx.feature.promptText(),'new');assert.deepEqual(ctx.alerts,[]);}finally{gate.release();ctx.feature.destroy();}
});

test('People receipt callback destroying the composer stops subsequent stage cleanup', async () => {
 let f,ctx,finished=0;f=transactionalPeopleFixture({depsExtra:{finishPeople:async(receipt,options)=>{finished++;ctx.feature.destroy();ctx.feature.bind();ctx.feature.onSelect();const pending=receipt.peoplePending;return f.actual.decidePeoplePublication({scope:pending.scope,publication:pending.publication,expected:pending,proof:{state:'completed',seq:receipt.seq,text:pending.text,stage_ids:pending.attachments},current:options.current});}}});ctx=f.ctx;
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await f.waitReady(1);await ctx.feature.sendPrompt();await localReviewTick();assert.equal(finished,1);assert.equal(f.sendEntries(),1);assert.equal(f.sends.length,1);assert.equal(f.recovery().filter(row=>row.kind==='decision').length,0);assert.equal(f.recovery().filter(row=>row.kind==='publication').length,1);assert.equal(f.files()[0].published,undefined);assert.equal(ctx.apiCalls.filter(c=>c.opts?.method==='DELETE').length,0);assert.match(ctx.roots.attstage.innerHTML,/saved.txt/);assert.deepEqual(ctx.alerts,[]);}finally{ctx.feature.destroy();}
});

test('People browser removal settling before queued destruction cannot remove the new view row', async () => {
 const context={key:localReviewKey,copyInstance:'1'.repeat(32),binding:'current',writable:true,owner:true,attachments:true},f=transactionalPeopleFixture({context,nodes:[{id:'n1',instance_id:'8'.repeat(32)}]}),ctx=f.ctx,gate=f.holdNext('remove');
 try {ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('old');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await f.waitReady(1);const flight=ctx.feature.sendPrompt();await gate.ready;assert.equal(typeof gate.release,'function');gate.release();queueMicrotask(()=>queueMicrotask(()=>{ctx.feature.destroy();ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('new');ctx.roots.prompt.firstListener('input')();}));await flight;assert.equal(ctx.feature.promptText(),'new');assert.match(ctx.roots.attstage.innerHTML,/saved.txt/);assert.equal(f.sendEntries(),1);assert.equal(f.sends.length,1);assert.deepEqual(ctx.alerts,[]);} finally {gate.release();ctx.feature.destroy();}
});

test('People retry does not dispatch while its saved file removal is already in progress', async () => {
 let pending=null,release,retries=0;const id='0123456789abcdef',copyInstance='1'.repeat(32);
 const store={list:async()=>[{id,stageRevision:'a'.repeat(64),file:{name:'saved.txt',type:'text/plain',size:1}}],markPublishedBatch:async()=>{},remove:async()=>{}};
 const ctx=makeFeature({deps:{peopleContext:()=>({key:localReviewKey,copyInstance,writable:true,owner:true,attachments:true}),peopleFileStore:store,pendingPeople:()=>pending,retryPeople:async()=>{retries++;return {seq:7}}},sendHandler:async(_path,opts)=>opts.method==='DELETE'?new Promise(resolve=>{release=resolve;}):{}});
 try {ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();ctx.roots.attstage._stageButtons[0].click();await localReviewTick();assert.equal(typeof release,'function');pending={key:localReviewKey,text:'saved',attachments:[id]};ctx.feature.renderStage();ctx.roots.attstage._retryButtons[0].click();await localReviewTick();assert.equal(retries,0);release({});await localReviewTick();} finally {ctx.feature.destroy();}
});

for(const point of ['mark','remove']) test(`queued People ${point} update cannot mutate files after ownership changes`,async()=>{
 for(const change of ['destroy','copy','native','binding']){
  const context={key:localReviewKey,copyInstance:'1'.repeat(32),binding:'current',writable:true,owner:true,attachments:true},f=transactionalPeopleFixture({context,nodes:[{id:'n1',instance_id:'native-one'}]}),ctx=f.ctx,gate=f.holdNext(point==='mark'?'decision':'remove');
  try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('old');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await f.waitReady(1);const original=f.files()[0],flight=ctx.feature.sendPrompt();await gate.ready;assert.equal(f.sendEntries(),1);assert.equal(f.sends.length,1);if(change==='destroy'){ctx.feature.destroy();ctx.feature.bind();ctx.feature.onSelect();}else if(change==='copy'){context.copyInstance='2'.repeat(32);ctx.feature.contextChanged();}else if(change==='binding'){context.binding='other';ctx.feature.contextChanged();}else{ctx.nodes[0].instance_id='native-two';ctx.feature.onSelect();}gate.release();await flight;const rows=await f.actual.list(original.scope);assert.equal(rows.length,1,change);assert.equal(rows[0].file,original.file);assert.equal(rows[0].fileToken,original.fileToken);assert.equal(rows[0].published,point==='remove'?7:undefined);assert.equal(f.events.filter(event=>event==='finish').length,1);assert.equal(f.recovery().filter(row=>row.kind==='decision').length,point==='remove'?1:0);assert.equal(ctx.apiCalls.filter(call=>call.opts?.method==='DELETE').length,point==='remove'?1:0);assert.deepEqual(ctx.alerts,[]);context.attachments=false;ctx.feature.contextChanged();ctx.feature.setPromptText('new');ctx.roots.prompt.firstListener('input')();await ctx.feature.sendPrompt();assert.equal(f.sendEntries(),2);assert.equal(f.sends.length,2);assert.equal(f.sends[1].request.text,'new');assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);}finally{gate.release();ctx.feature.destroy();}
 }
});

function browserRetireFixture({state='missing',post,remove,clear=true,owner=false,retained=false,fenceState='locally_retired'}={}) {
 const record=retainedBrowserSnapshot({instance:owner?'8'.repeat(32):'',binding:retained?'old':'current'}),events=[];let raw=record.value,lookupThrow=false,clearAllowed=clear,retries=0;
 const context={key:record.key,copyInstance:record.copyInstance,binding:'current',owner,writable:!retained};
 const ctx=makeFeature({sel:owner?'n1':'team/'+localReviewKey,nodes:owner?[{id:'n1',instance_id:record.instance}]:[],deps:{peopleContext:()=>retained&&owner?null:context,localReviewContexts:()=>[context],pendingPeople:()=>{if(lookupThrow)throw Error('unavailable');return !retained&&raw?{...record,value:raw}:null;},retryPeople:async()=>{retries++;return {seq:1};},readRetainedPeoplePending:()=>({entries:retained&&raw?[{...record,value:raw}]:[],unavailable:false}),localReviewApi:async(path,opts)=>{events.push(opts.method);if(opts.method==='POST')return post?post(path,opts):{state:'locally_discarded'};if(opts.method==='DELETE')return remove?remove(path,opts):{state:'locally_discarded'};if(path.includes('/pending/')){const actual=typeof state==='function'?state():state;if(typeof actual==='object')return actual;return actual==='retired'||actual==='pending'?{state:actual,revision:'e'.repeat(64),text:'actual server text',stage_ids:record.attachments}:{state:actual};}return {entries:[]};},fencePeoplePending:snapshot=>{events.push('fence');assert.equal(snapshot.value,record.value);return {state:fenceState,cancelled:0};},clearPeoplePending:snapshot=>{events.push('clear');if(clearAllowed==='throw')throw Error('storage unavailable');if(clearAllowed==='review_conflict')throw Object.assign(Error('{"error":"review_required"}'),{status:409});if(!clearAllowed||raw!==snapshot.value)return false;raw=null;return true;}}});
 const original=ctx.roots.attstage.querySelectorAll;let buttons=[],details=[];
 ctx.roots.attstage.querySelectorAll=selector=>selector==='.people-browser-retire'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-browser-retire(?:\s+[^"\s]+)*" data-row="(\d+)" data-snapshot="([^"]+)"/g)].map(m=>el('button',{dataset:{row:m[1],snapshot:m[2]}}))):selector==='.people-local-snapshot'?(details=[...ctx.roots.attstage.innerHTML.matchAll(/class="people-local-snapshot" data-row="(\d+)" data-snapshot="(\d+)"/g)].map(m=>el('details',{dataset:{row:m[1],snapshot:m[2]}}))):original(selector);
 return {ctx,events,record,context,buttons:()=>buttons,details:()=>details,raw:()=>raw,setRaw:value=>{raw=value;},setLookupThrow:value=>{lookupThrow=value;},setClear:value=>{clearAllowed=value;},retries:()=>retries};
}

for(const state of ['missing','absent_retired','retired']) test(`explicit browser retry cleanup uses verified ${state} decision`,async()=>{
 const f=browserRetireFixture({state});try {f.ctx.feature.bind();f.ctx.feature.onSelect();f.ctx.feature.setPromptText('new draft');f.ctx.roots.prompt.firstListener('input')();await localReviewTick();assert.deepEqual(f.events,['GET','GET']);assert.equal(f.buttons().length,1);f.buttons()[0].click();await localReviewTick();assert.deepEqual(f.events,state==='retired'?['GET','GET','fence','clear']:['GET','GET','POST','fence','clear']);assert.equal(f.raw(),null);assert.equal(f.ctx.feature.promptText(),'new draft');assert.doesNotMatch(f.ctx.roots.attstage.innerHTML,/retained synthetic/);assert.equal(f.ctx.apiCalls.length,0);}finally{f.ctx.feature.destroy();}
});

test('retained readonly browser retry retirement preserves the Agent draft',async()=>{
 const f=browserRetireFixture({retained:true,owner:true});try{f.ctx.feature.bind();f.ctx.feature.onSelect();f.ctx.feature.setPromptText('agent draft');f.ctx.roots.prompt.firstListener('input')();await localReviewTick();const d=f.details()[0];d.open=true;d.firstListener('toggle')();await localReviewTick();assert.deepEqual(f.events,['GET','GET']);assert.equal(f.buttons().length,1);f.buttons()[0].click();await localReviewTick();assert.deepEqual(f.events,['GET','GET','POST','fence','clear']);assert.equal(f.raw(),null);assert.equal(f.ctx.feature.promptText(),'agent draft');assert.equal(f.ctx.feature.destination(),'agent');assert.equal(f.ctx.apiCalls.length,0);}finally{f.ctx.feature.destroy();}
});

test('browser retry cleanup preserves a newer raw value and reports failure',async()=>{
 let release;const f=browserRetireFixture({post:()=>new Promise(resolve=>{release=resolve;})});try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();assert.equal(f.buttons().length,1);f.buttons()[0].click();await localReviewTick();f.setRaw(f.record.value+' ');release({state:'locally_discarded'});await localReviewTick();assert.equal(f.raw(),f.record.value+' ');assert.deepEqual(f.events.slice(0,5),['GET','GET','POST','fence','clear']);assert.equal(f.events.filter(event=>event==='POST').length,1);}finally{f.ctx.feature.destroy();}
});

test('browser retry cleanup retains its raw record when local flight fencing fails',async()=>{
 const f=browserRetireFixture({fenceState:'unavailable'});try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();assert.equal(f.buttons().length,1);f.buttons()[0].click();await localReviewTick();assert.deepEqual(f.events,['GET','GET','POST','fence']);assert.equal(f.raw(),f.record.value);assert.match(f.ctx.roots.attstage.innerHTML,/cleanup is unconfirmed/i);assert.equal(f.ctx.apiCalls.length,0);}finally{f.ctx.feature.destroy();}
});

for(const response of [{state:'locally_discarded'},{state:'already_completed',seq:23}]) test(`reviewed active pending discard ${response.state} handles browser retry truthfully`,async()=>{
 const record=retainedBrowserSnapshot({instance:'',binding:'current'}),events=[];let raw=record.value;const context={key:record.key,copyInstance:record.copyInstance,binding:'current',owner:false,writable:true};
 const ctx=makeFeature({sel:'team/'+record.key,nodes:[],deps:{peopleContext:()=>context,pendingPeople:()=>raw?{...record,value:raw}:null,localReviewApi:async(path,opts)=>{events.push(opts.method);if(opts.method==='DELETE')return response;if(path.includes('/pending/'))return {state:'pending',revision:'e'.repeat(64),text:'actual',stage_ids:record.attachments};return {entries:[localReviewRow('pending',record.publicationKey,{text:'actual',stage_ids:record.attachments})]};},fencePeoplePending:()=>{events.push('fence');return {state:'locally_retired',cancelled:0};},clearPeoplePending:()=>{events.push('clear');raw=null;return true;}}});
 const old=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-local-discard'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-local-discard(?:\s+[^"\s]+)*" data-row="(\d+)" data-id="([0-9a-f]+)"/g)].map(m=>el('button',{dataset:{row:m[1],id:m[2]}}))):old(s);
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('new draft');ctx.roots.prompt.firstListener('input')();await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/actual/);assert.equal(buttons.length,1);buttons[0].click();await localReviewTick();assert.deepEqual(events,response.state==='locally_discarded'?['GET','GET','DELETE','fence','clear']:['GET','GET','DELETE']);assert.equal(raw,response.state==='locally_discarded'?null:record.value);assert.equal(ctx.feature.promptText(),'new draft');assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

for(const point of ['file-read','delete']) test(`People local discard protects another view while ${point} is pending`,async()=>{
 const record=retainedBrowserSnapshot({instance:'',binding:'current'}),context={key:record.key,copyInstance:record.copyInstance,binding:'current',owner:false,writable:true},events=[];let raw=record.value,hold=false,release;
 const store={list:async()=>{if(hold&&point==='file-read')await new Promise(resolve=>{release=resolve;});return [];}};
 const ctx=makeFeature({sel:'team/'+record.key,nodes:[],deps:{peopleContext:()=>context,peopleFileStore:store,pendingPeople:()=>raw?{...record,value:raw}:null,localReviewApi:async(path,opts)=>{events.push(opts.method);if(opts.method==='DELETE'&&point==='delete')return new Promise(resolve=>{release=()=>resolve({state:'locally_discarded'});});return path.includes('/pending/')?{state:'pending',revision:'e'.repeat(64),text:'actual',stage_ids:record.attachments}:{entries:[localReviewRow('pending',record.publicationKey,{text:'actual',stage_ids:record.attachments})]};},fencePeoplePending:()=>({state:'locally_retired'}),clearPeoplePending:snapshot=>{events.push('clear');assert.equal(snapshot.value,record.value);raw=null;return true;}}});
 const old=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-local-discard'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-local-discard(?:\s+[^"\s]+)*" data-row="(\d+)" data-id="([0-9a-f]+)"/g)].map(m=>el('button',{dataset:{row:m[1],id:m[2]}}))):old(s);
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(buttons.length,1);hold=true;buttons[0].click();await localReviewTick();assert.equal(typeof release,'function');ctx.setSel('');ctx.feature.onSelect();const currentHTML=ctx.roots.attstage.innerHTML;release();await localReviewTick();assert.equal(raw,point==='delete'?null:record.value);assert.equal(events.filter(event=>event==='DELETE').length,point==='delete'?1:0);assert.equal(events.includes('clear'),point==='delete');assert.equal(ctx.roots.attstage.innerHTML,currentHTML);assert.equal(ctx.apiCalls.length,0);}finally{if(release)release();ctx.feature.destroy();}
});

test('absent retirement conflict fetches actual content for new review without deleting',async()=>{
 let state='missing';const f=browserRetireFixture({state:()=>state,post:async()=>{state='pending';throw Object.assign(Error('{"error":"review_required"}'),{status:409});}});
 try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();assert.equal(f.buttons().length,1);f.buttons()[0].click();await localReviewTick();assert.deepEqual(f.events,['GET','GET','POST','GET']);assert.equal(f.raw(),f.record.value);assert.match(f.ctx.roots.attstage.innerHTML,/actual server text/);assert.match(f.ctx.roots.attstage.innerHTML,/3333333333333333/);assert.match(f.ctx.roots.attstage.innerHTML,new RegExp('e'.repeat(64)));assert.equal(f.buttons().length,0);assert.ok(!f.events.includes('DELETE'));assert.ok(!f.events.includes('clear'));}finally{f.ctx.feature.destroy();}
});

test('successful retained retry retirement removes only the reviewed row',async()=>{
 const key=localReviewKey,copyInstance='1'.repeat(32),instance='8'.repeat(32),context={key,copyInstance,binding:'current',owner:true,writable:false,attachments:true},scope=`people\0${key}\0binding:old\0copy:${copyInstance}\0n1\0${instance}`,id='3333333333333333',revision='a'.repeat(64),events=[];let snapshot=null,f;
 f=transactionalPeopleFixture({context,nodes:[{id:'n1',instance_id:instance}],depsExtra:{peopleContext:()=>null,localReviewContexts:()=>[context],pendingPeople:()=>null,readRetainedPeoplePending:()=>({entries:snapshot?[snapshot]:[],unavailable:false}),localReviewApi:async(path,opts)=>{events.push(opts.method);return opts.method==='POST'?{state:'locally_discarded'}:path.includes('/pending/')?{state:'missing'}:{entries:[]};},fencePeoplePending:()=>({state:'locally_retired',cancelled:0}),clearPeoplePending:async(record,options)=>{const result=await f.actual.decidePeoplePublication({scope,publication:record.publication,expected:record,proof:options.proof,current:options.current});snapshot=null;return result;}}});const ctx=f.ctx,file={name:'saved.txt',type:'text/plain',size:1},token=await f.actual.save(scope,id,file);await f.actual.setStageRevision(scope,id,revision,token);const draft=await f.actual.savePeopleDraft({scope,text:'retained synthetic'}),request={key:'2'.repeat(16),text:'retained synthetic',anchor:0,attachments:[id]},pub=await f.actual.beginPeoplePublication({scope,peopleKey:key,copyInstance,binding:'old',native:instance,...draft,request,files:[{id,fileToken:token,stageRevision:revision}]});snapshot={format:'transactional',scope,publication:pub.publication,head:draft.head,version:draft.version,key,instance,copyInstance,binding:'old',storageKey:'idb:'+pub.publication,value:JSON.stringify({instance,copyInstance,request}),publicationKey:request.key,text:request.text,attachments:request.attachments};
 const original=ctx.roots.attstage.querySelectorAll;let details=[],buttons=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-local-snapshot'?(details=[...ctx.roots.attstage.innerHTML.matchAll(/class="people-local-snapshot" data-row="(\d+)" data-snapshot="(\d+)"/g)].map(m=>el('details',{dataset:{row:m[1],snapshot:m[2]}}))):s==='.people-browser-retire'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-browser-retire(?:\s+[^"\s]+)*" data-row="(\d+)" data-snapshot="([^"]+)"/g)].map(m=>el('button',{dataset:{row:m[1],snapshot:m[2]}}))):original(s);
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();const d=details[0];d.open=true;d.firstListener('toggle')();await localReviewTick();buttons[0].click();await localReviewTick();assert.equal(snapshot,null);assert.doesNotMatch(ctx.roots.attstage.innerHTML,/retained synthetic/);assert.equal(ctx.roots.attstage.hidden,true);assert.equal(f.recovery().find(row=>row.publication===pub.publication).state,'retired');assert.equal((await f.actual.list(scope))[0].fileToken,token);assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('browser retirement failure keeps the saved retry reviewable',async()=>{
 for(const mode of ['clear','network']) {
  const f=browserRetireFixture(mode==='clear'?{clear:false}:{post:async()=>{throw Error('offline');}});
  try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();f.buttons()[0].click();await localReviewTick();assert.equal(f.raw(),f.record.value);assert.match(f.ctx.roots.attstage.innerHTML,/cleanup is unconfirmed/i);assert.equal(f.buttons().length,1);assert.equal(f.events.filter(x=>x==='POST').length,1);if(mode==='network')assert.ok(!f.events.includes('fence'));}finally{f.ctx.feature.destroy();}
 }
});

test('browser retirement is single flight and late acknowledgement stays in its original scope',async()=>{
 for(const change of ['selection','copy','binding','native','destroy','rebind']) {
  let release;const f=browserRetireFixture({owner:true,post:()=>new Promise(resolve=>{release=resolve;})});
  try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();const button=f.buttons()[0];assert.ok(button);button.click();button.click();await localReviewTick();assert.equal(f.events.filter(x=>x==='POST').length,1);
   if(change==='selection'){f.ctx.setSel('');f.ctx.feature.onSelect();}else if(change==='copy'){f.context.copyInstance='9'.repeat(32);f.ctx.feature.contextChanged();}else if(change==='binding'){f.context.binding='new';f.ctx.feature.contextChanged();}else if(change==='native'){f.ctx.nodes[0].instance_id='9'.repeat(32);f.ctx.feature.onSelect();}else{f.ctx.feature.destroy();if(change==='rebind'){f.ctx.feature.bind();f.ctx.feature.onSelect();}}
   await localReviewTick();const before=f.ctx.roots.attstage.innerHTML;release({state:'locally_discarded'});await localReviewTick();assert.equal(f.ctx.roots.attstage.innerHTML,before,change);assert.equal(f.raw(),change==='destroy'||change==='rebind'?f.record.value:null,change);assert.equal(f.events.filter(x=>x==='fence').length,change==='destroy'||change==='rebind'?0:1);assert.equal(f.ctx.apiCalls.length,0);
  }finally{f.ctx.feature.destroy();}
 }
});

test('retired browser retry is not offered as an ordinary send',async()=>{
 for(const state of ['retired','absent_retired']){const f=browserRetireFixture({state});try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();assert.equal(renderedClassCount(f.ctx.roots.attstage.innerHTML, "people-retry"),0);assert.equal(f.buttons().length,1);assert.ok(f.events.every(x=>x==='GET'));}finally{f.ctx.feature.destroy();}}
});

test('stale and duplicate browser retirement callbacks cannot dispatch',async()=>{
 let release;const f=browserRetireFixture({post:()=>new Promise(resolve=>{release=resolve;})});try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();const click=f.buttons()[0].firstListener('click');click();click();await localReviewTick();assert.equal(f.events.filter(x=>x==='POST').length,1);release({state:'locally_discarded'});await localReviewTick();click();assert.equal(f.events.filter(x=>x==='POST').length,1);}finally{f.ctx.feature.destroy();}
 const g=browserRetireFixture();try{g.ctx.feature.bind();g.ctx.feature.onSelect();await localReviewTick();const click=g.buttons()[0].firstListener('click');g.setRaw(g.record.value+' ');click();await localReviewTick();assert.ok(!g.events.includes('POST'));assert.ok(!g.events.includes('clear'));}finally{g.ctx.feature.destroy();}
});

test('retained retirement conflict refreshes the actual immutable record',async()=>{
 let state='missing';const f=browserRetireFixture({owner:true,retained:true,state:()=>state,post:async()=>{state='pending';throw Object.assign(Error('{"error":"review_required"}'),{status:409});}});
 try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();const d=f.details()[0];d.open=true;d.firstListener('toggle')();await localReviewTick();f.buttons()[0].click();await localReviewTick();assert.deepEqual(f.events,['GET','GET','POST','GET']);assert.match(f.ctx.roots.attstage.innerHTML,/actual server text/);assert.match(f.ctx.roots.attstage.innerHTML,new RegExp('e'.repeat(64)));assert.equal(f.raw(),f.record.value);assert.ok(!f.events.includes('DELETE'));}finally{f.ctx.feature.destroy();}
});

test('active pending discard reports unconfirmed raw cleanup and keeps the retry',async()=>{
 const record=retainedBrowserSnapshot({instance:'',binding:'current'}),events=[];let raw=record.value;
 const ctx=makeFeature({sel:'team/'+record.key,nodes:[],deps:{peopleContext:()=>({key:record.key,copyInstance:record.copyInstance,binding:'current',owner:false,writable:true}),pendingPeople:()=>({...record,value:raw}),localReviewApi:async(path,opts)=>{events.push(opts.method);if(opts.method==='DELETE')return {state:'locally_discarded'};return path.includes('/pending/')?{state:'pending',revision:'e'.repeat(64),text:'actual',stage_ids:record.attachments}:{entries:[localReviewRow('pending',record.publicationKey,{text:'actual',stage_ids:record.attachments})]};},fencePeoplePending:()=>{events.push('fence');return {state:'locally_retired',cancelled:0};},clearPeoplePending:()=>{events.push('clear');return false;}}});
 const old=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-local-discard'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-local-discard(?:\s+[^"\s]+)*" data-row="(\d+)" data-id="([0-9a-f]+)"/g)].map(m=>el('button',{dataset:{row:m[1],id:m[2]}}))):old(s);
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();buttons[0].click();await localReviewTick();assert.deepEqual(events,['GET','GET','DELETE','fence','clear']);assert.equal(raw,record.value);assert.match(ctx.roots.attstage.innerHTML,/cleanup is unconfirmed/i);assert.equal(buttons.length,0);assert.ok(renderedClassCount(ctx.roots.attstage.innerHTML, "people-browser-retire") > 0);}finally{ctx.feature.destroy();}
});

test('browser retirement rechecks its exact raw review immediately before dispatch',async()=>{
 const f=browserRetireFixture();let html='',arm=false;
 Object.defineProperty(f.ctx.roots.attstage,'innerHTML',{get:()=>html,set:value=>{html=value;if(arm && value.includes('Retiring local retry')){arm=false;f.setRaw(f.record.value+' ');}}});
 try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();const click=f.buttons()[0].firstListener('click');arm=true;click();await localReviewTick();assert.ok(!f.events.includes('POST'));assert.ok(!f.events.includes('fence'));assert.equal(f.raw(),f.record.value+' ');}finally{f.ctx.feature.destroy();}
});

test('retiring one reviewed binding keeps other same-key bindings and copies',async()=>{
 const copy='1'.repeat(32),other='4'.repeat(32),records=[retainedBrowserSnapshot({binding:'old-one'}),retainedBrowserSnapshot({binding:'old-two'}),retainedBrowserSnapshot({copyInstance:other,binding:'old-three'})],values=new Map(records.map(r=>[r.storageKey,r.value])),events=[];
 const contexts=[{key:localReviewKey,copyInstance:copy,binding:'current',owner:true},{key:localReviewKey,copyInstance:other,binding:'current',owner:true}];
 const ctx=makeFeature({nodes:[{id:'n1',instance_id:'8'.repeat(32)}],deps:{peopleContext:()=>null,localReviewContexts:()=>contexts,readRetainedPeoplePending:c=>({entries:records.filter(r=>r.copyInstance===c.copyInstance&&values.has(r.storageKey)),unavailable:false}),localReviewApi:async(path,opts)=>{events.push(opts.method);return opts.method==='POST'?{state:'locally_discarded'}:path.includes('/pending/')?{state:'missing'}:{entries:[]};},fencePeoplePending:s=>{events.push('fence');assert.equal(s.storageKey,records[0].storageKey);return {state:'locally_retired',cancelled:0};},clearPeoplePending:s=>{events.push('clear');if(values.get(s.storageKey)!==s.value)return false;values.delete(s.storageKey);return true;}}});
 const old=ctx.roots.attstage.querySelectorAll;let buttons=[],details=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-browser-retire'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-browser-retire(?:\s+[^"\s]+)*" data-row="(\d+)" data-snapshot="([^"]+)"/g)].map(m=>el('button',{dataset:{row:m[1],snapshot:m[2]}}))):s==='.people-local-snapshot'?(details=[...ctx.roots.attstage.innerHTML.matchAll(/class="people-local-snapshot" data-row="(\d+)" data-snapshot="(\d+)"/g)].map(m=>el('details',{dataset:{row:m[1],snapshot:m[2]}}))):old(s);
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('agent draft');ctx.roots.prompt.firstListener('input')();ctx.storage.setItem('synthetic-association','preserved');await localReviewTick();assert.equal(details.length,3);const d=details[0];d.open=true;d.firstListener('toggle')();await localReviewTick();assert.equal(buttons.length,1);buttons[0].click();await localReviewTick();assert.equal(values.has(records[0].storageKey),false);assert.equal(values.get(records[1].storageKey),records[1].value);assert.equal(values.get(records[2].storageKey),records[2].value);assert.equal(ctx.storage.getItem('synthetic-association'),'preserved');assert.equal(ctx.feature.promptText(),'agent draft');assert.equal(ctx.apiCalls.length,0);assert.deepEqual(events,['GET','GET','GET','POST','fence','clear']);}finally{ctx.feature.destroy();}
});

test('explicit retirement fences a held People file send before its late result can finalize',async()=>{
 for(const outcome of ['success','error']) {
  const copyInstance='1'.repeat(32),instance='8'.repeat(32),context={key:localReviewKey,copyInstance,binding:'current',owner:true,writable:true,attachments:true},events=[];let settle,retire,active=true,f;
  f=transactionalPeopleFixture({context,nodes:[{id:'n1',instance_id:instance}],respond:()=>{const transport=new Promise((resolve,reject)=>{settle=()=>outcome==='success'?resolve({seq:7}):reject(Error('late failure'));}),cancelled=new Promise((_,reject)=>{retire=()=>reject(Object.assign(Error('retired'),{code:'PEOPLE_LOCAL_RETIRED'}));});return Promise.race([transport,cancelled]);},depsExtra:{pendingPeople:()=>active?f.pending():null,localReviewApi:async(path,opts)=>{events.push(opts.method);return opts.method==='POST'?{state:'locally_discarded'}:path.includes('/pending/')?{state:'missing'}:{entries:[]};},fencePeoplePending:s=>{events.push('fence');assert.equal(s.value,f.pending().value);retire();return {state:'locally_retired',cancelled:1};},clearPeoplePending:async(s,options)=>{events.push('clear');const result=await f.actual.decidePeoplePublication({scope:s.scope,publication:s.publication,expected:s,proof:options.proof,current:options.current});active=false;return result;}}});const ctx=f.ctx;
  const old=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-browser-retire'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-browser-retire(?:\s+[^"\s]+)*" data-row="(\d+)" data-snapshot="([^"]+)"/g)].map(m=>el('button',{dataset:{row:m[1],snapshot:m[2]}}))):old(s);
  try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('old');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await f.waitReady(1);const flight=ctx.feature.sendPrompt();await f.waitSend(1);const association=f.pending(),file=f.files()[0];assert.ok(association);ctx.feature.renderStage();await localReviewTick();assert.equal(buttons.length,1);buttons[0].click();await flight;await localReviewTick();assert.deepEqual(events.slice(-3),['POST','fence','clear']);settle();await localReviewTick();assert.equal(f.files()[0].published,undefined);assert.equal(f.events.filter(event=>event==='finish').length,0);assert.match(ctx.roots.attstage.innerHTML,/saved.txt/);assert.equal(ctx.feature.promptText(),'old');const decision=f.recovery().find(row=>row.publication===association.publication);assert.equal(decision.state,'retired');assert.equal(decision.head,association.head);assert.equal(decision.version,association.version);assert.equal((await f.actual.list(association.scope))[0].fileToken,file.fileToken);assert.deepEqual(ctx.alerts,[]);assert.equal(ctx.apiCalls.filter(c=>c.path.endsWith('/send')).length,0);}finally{if(settle)settle();ctx.feature.destroy();}
 }
});

test('non-review conflict retains the saved retry and reports unavailability',async()=>{
 const f=browserRetireFixture({post:async()=>{throw Object.assign(Error('{"error":"copy_changed"}'),{status:409});}});try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();f.buttons()[0].click();await localReviewTick();assert.deepEqual(f.events,['GET','GET','POST']);assert.equal(f.raw(),f.record.value);assert.match(f.ctx.roots.attstage.innerHTML,/cleanup is unconfirmed/i);assert.equal(f.buttons().length,1);}finally{f.ctx.feature.destroy();}
});

test('browser retirement refuses a captured action when pending storage lookup fails',async()=>{
 const f=browserRetireFixture();try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();const click=f.buttons()[0].firstListener('click');f.setLookupThrow(true);click();await localReviewTick();assert.ok(!f.events.includes('POST'));assert.equal(f.raw(),f.record.value);}finally{f.ctx.feature.destroy();}
});

test('malformed conflict response keeps retry without a state refresh',async()=>{
 const f=browserRetireFixture({post:async()=>{throw Object.assign(Error('invalid response'),{status:409});}});try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();f.buttons()[0].click();await localReviewTick();assert.deepEqual(f.events,['GET','GET','POST']);assert.equal(f.raw(),f.record.value);assert.match(f.ctx.roots.attstage.innerHTML,/unconfirmed/i);}finally{f.ctx.feature.destroy();}
});

test('forged browser retirement button cannot address another row',async()=>{
 const f=browserRetireFixture(),old=f.ctx.roots.attstage.querySelectorAll,forged=el('button',{dataset:{row:'999',snapshot:'current'}});f.ctx.roots.attstage.querySelectorAll=s=>s==='.people-browser-retire'?[...old(s),forged]:old(s);
 try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();assert.equal(forged.listenerCount('click'),0);forged.click();assert.ok(!f.events.includes('POST'));}finally{f.ctx.feature.destroy();}
});

test('lifetime ending after an acknowledged retirement prevents later local cleanup',async()=>{
 let release;const f=browserRetireFixture({post:()=>new Promise(resolve=>{release=resolve;})});try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();f.buttons()[0].click();await localReviewTick();release({state:'locally_discarded'});queueMicrotask(()=>queueMicrotask(()=>queueMicrotask(()=>f.ctx.feature.destroy())));await localReviewTick();assert.equal(f.raw(),f.record.value);assert.ok(!f.events.includes('fence'));assert.ok(!f.events.includes('clear'));}finally{f.ctx.feature.destroy();}
});

test('lifetime ending after an acknowledged pending discard cannot repaint',async()=>{
 let release;const f=discardReviewFixture({result:()=>new Promise(resolve=>{release=resolve;})});try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();f.buttons()[0].click();await localReviewTick();release({state:'locally_discarded'});queueMicrotask(()=>queueMicrotask(()=>queueMicrotask(()=>f.ctx.feature.destroy())));await localReviewTick();assert.doesNotMatch(f.ctx.roots.attstage.innerHTML,/Locally discarded/);}finally{f.ctx.feature.destroy();}
});

test('readonly active retry can be locally retired without writable People capability',async()=>{
 const f=browserRetireFixture();f.context.writable=false;try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();assert.equal(renderedClassCount(f.ctx.roots.attstage.innerHTML, "people-retry"),0);assert.equal(f.buttons().length,1);f.buttons()[0].click();await localReviewTick();assert.equal(f.raw(),null);assert.deepEqual(f.events,['GET','GET','POST','fence','clear']);assert.equal(f.ctx.apiCalls.length,0);}finally{f.ctx.feature.destroy();}
});

test('unconfirmed raw cleanup can be explicitly retried after storage recovers',async()=>{
 const f=browserRetireFixture({clear:false});try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();f.buttons()[0].click();await localReviewTick();assert.equal(f.raw(),f.record.value);assert.match(f.ctx.roots.attstage.innerHTML,/cleanup is unconfirmed/i);f.setClear(true);f.buttons()[0].click();await localReviewTick();assert.equal(f.raw(),null);assert.equal(f.events.filter(x=>x==='POST').length,1);assert.equal(f.events.filter(x=>x==='clear').length,2);}finally{f.ctx.feature.destroy();}
});

test('the server-only pending discard shows the immutable revision it will send',async()=>{
 const f=discardReviewFixture({result:async()=>({state:'locally_discarded'})});try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();assert.match(f.ctx.roots.attstage.innerHTML,new RegExp('e'.repeat(64)));assert.equal(f.calls.filter(c=>c.opts.method==='DELETE').length,0);f.buttons()[0].click();await localReviewTick();assert.match(f.calls.at(-1).path,new RegExp('revision='+ 'e'.repeat(64)));}finally{f.ctx.feature.destroy();}
});

test('server-only discard rechecks its original review before dispatch',async()=>{
 const f=discardReviewFixture({result:async()=>({state:'locally_discarded'})});let html='',arm=false;
 Object.defineProperty(f.ctx.roots.attstage,'innerHTML',{get:()=>html,set:value=>{html=value;if(arm&&value.includes('Discarding locally')){arm=false;f.ctx.setSel('');}}});
 try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();const click=f.buttons()[0].firstListener('click');arm=true;click();await localReviewTick();assert.equal(f.calls.filter(c=>c.opts.method==='DELETE').length,0);assert.match(html,/first saved/);}finally{f.ctx.feature.destroy();}
});

test('captured People retry refuses dispatch after exact state proves retirement',async()=>{
 for(const state of ['retired','absent_retired']) for(const binding of ['current','']){
  const record=retainedBrowserSnapshot({instance:'',binding});let release,retries=0;
  const store={list:async()=>[{id:record.attachments[0],file:{name:'saved.txt',type:'text/plain',size:1}}],markPublishedBatch:async()=>{},remove:async()=>{}};
  const ctx=makeFeature({sel:'team/'+record.key,nodes:[],deps:{peopleContext:()=>({key:record.key,copyInstance:record.copyInstance,binding,owner:false,writable:true,attachments:true}),peopleFileStore:store,pendingPeople:()=>record,retryPeople:async()=>{retries++;return {seq:1}},localReviewApi:async(path)=>path.includes('/pending/')?new Promise(resolve=>{release=resolve;}):{entries:[]}}});
  try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();const click=ctx.roots.attstage._retryButtons[0].firstListener('click');release(state==='retired'?{state,revision:'e'.repeat(64),text:'actual',stage_ids:record.attachments}:{state});await localReviewTick();assert.equal(renderedClassCount(ctx.roots.attstage.innerHTML, "people-retry"),0);click();await localReviewTick();assert.equal(retries,0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
 }
});

test('missing retry retirement posts the captured copy and key without a body',async()=>{
 let request;const f=browserRetireFixture({post:async(path,options)=>{request={path,options};return {state:'locally_discarded'};}});try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();f.buttons()[0].click();await localReviewTick();assert.equal(request.path,`/api/team/subscriptions/${f.record.key}/pending/${f.record.publicationKey}/retire-absent?copy_instance=${f.record.copyInstance}`);assert.equal(request.options.method,'POST');assert.equal(Object.hasOwn(request.options,'body'),false);assert.equal(f.raw(),null);}finally{f.ctx.feature.destroy();}
});

for(const failure of [false,'throw']) test(`durable absent retirement hides publish retry when cleanup ${failure}`,async()=>{
 const f=browserRetireFixture({clear:failure});try{f.ctx.feature.bind();f.ctx.feature.onSelect();f.ctx.feature.setPromptText('draft preserved');f.ctx.roots.prompt.firstListener('input')();f.ctx.storage.setItem('synthetic-association','preserved');await localReviewTick();const retry=f.ctx.roots.attstage._retryButtons[0].firstListener('click');assert.equal(f.buttons().length,1);f.buttons()[0].click();await localReviewTick();assert.deepEqual(f.events,['GET','GET','POST','fence','clear']);assert.equal(f.raw(),f.record.value);assert.match(f.ctx.roots.attstage.innerHTML,/cleanup is unconfirmed/i);assert.equal(renderedClassCount(f.ctx.roots.attstage.innerHTML, "people-retry"),0);assert.doesNotMatch(f.ctx.roots.attstage.innerHTML,/No local publication record was found/);assert.equal(f.buttons().length,1);retry();await localReviewTick();assert.equal(f.retries(),0);assert.deepEqual(f.ctx.alerts,[]);assert.equal(f.ctx.feature.promptText(),'draft preserved');assert.equal(f.ctx.storage.getItem('synthetic-association'),'preserved');assert.equal(f.ctx.apiCalls.length,0);}finally{f.ctx.feature.destroy();}
});

for(const failure of [false,'throw']) test(`durable pending discard hides publish retry when cleanup ${failure}`,async()=>{
 const record=retainedBrowserSnapshot({instance:'',binding:'current'}),events=[];let raw=record.value,retries=0;
 const ctx=makeFeature({sel:'team/'+record.key,nodes:[],deps:{peopleContext:()=>({key:record.key,copyInstance:record.copyInstance,binding:'current',owner:false,writable:true}),pendingPeople:()=>({...record,value:raw}),retryPeople:async()=>{retries++;return {seq:1};},localReviewApi:async(path,opts)=>{events.push(opts.method);if(opts.method==='DELETE')return {state:'locally_discarded'};return path.includes('/pending/')?{state:'pending',revision:'e'.repeat(64),text:'actual',stage_ids:record.attachments}:{entries:[localReviewRow('pending',record.publicationKey,{text:'actual',stage_ids:record.attachments})]};},fencePeoplePending:()=>{events.push('fence');return {state:'locally_retired',cancelled:0};},clearPeoplePending:()=>{events.push('clear');if(failure==='throw')throw Error('storage unavailable');return false;}}});
 const old=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-local-discard'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-local-discard(?:\s+[^"\s]+)*" data-row="(\d+)" data-id="([0-9a-f]+)"/g)].map(m=>el('button',{dataset:{row:m[1],id:m[2]}}))):old(s);
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('draft preserved');ctx.roots.prompt.firstListener('input')();await localReviewTick();const retry=ctx.roots.attstage._retryButtons[0].firstListener('click');buttons[0].click();await localReviewTick();assert.deepEqual(events,['GET','GET','DELETE','fence','clear']);assert.equal(raw,record.value);assert.match(ctx.roots.attstage.innerHTML,/unconfirmed/i);assert.equal(renderedClassCount(ctx.roots.attstage.innerHTML, "people-retry"),0);retry();await localReviewTick();assert.equal(retries,0);assert.deepEqual(ctx.alerts,[]);assert.equal(ctx.feature.promptText(),'draft preserved');assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('cleanup error cannot replace an already proven retirement with a conflict refresh',async()=>{
 const f=browserRetireFixture({clear:'review_conflict'});try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();f.buttons()[0].click();await localReviewTick();assert.deepEqual(f.events,['GET','GET','POST','fence','clear']);assert.equal(f.raw(),f.record.value);assert.equal(renderedClassCount(f.ctx.roots.attstage.innerHTML, "people-retry"),0);assert.match(f.ctx.roots.attstage.innerHTML,/cleanup is unconfirmed/i);}finally{f.ctx.feature.destroy();}
});

for(const state of ['retired','absent_retired']) for(const failure of [false,'throw']) test(`verified ${state} never offers publish retry when cleanup ${failure}`,async()=>{
 const f=browserRetireFixture({state,clear:failure});try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();assert.equal(renderedClassCount(f.ctx.roots.attstage.innerHTML, "people-retry"),0);f.buttons()[0].click();await localReviewTick();assert.equal(f.raw(),f.record.value);assert.equal(renderedClassCount(f.ctx.roots.attstage.innerHTML, "people-retry"),0);assert.match(f.ctx.roots.attstage.innerHTML,/cleanup is unconfirmed/i);assert.equal(f.buttons().length,1);}finally{f.ctx.feature.destroy();}
});

test('retirement proof remains tied to its exact raw retry value',async()=>{
 const f=browserRetireFixture({clear:false});try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();f.buttons()[0].click();await localReviewTick();assert.equal(renderedClassCount(f.ctx.roots.attstage.innerHTML, "people-retry"),0);const next=JSON.parse(f.record.value);next.request.key='9'.repeat(16);f.setRaw(JSON.stringify(next));f.ctx.feature.renderStage();await localReviewTick();assert.equal(f.raw(),JSON.stringify(next));assert.ok(renderedClassCount(f.ctx.roots.attstage.innerHTML, "people-retry") > 0);assert.equal(f.events.filter(x=>x==='POST').length,1);}finally{f.ctx.feature.destroy();}
});

test('after a durable pending discard only local cleanup is retried',async()=>{
 const record=retainedBrowserSnapshot({instance:'',binding:'current'}),events=[];let raw=record.value,storageReady=false;
 const ctx=makeFeature({sel:'team/'+record.key,nodes:[],deps:{peopleContext:()=>({key:record.key,copyInstance:record.copyInstance,binding:'current',owner:false,writable:true}),pendingPeople:()=>raw?{...record,value:raw}:null,localReviewApi:async(path,opts)=>{events.push(opts.method);if(opts.method==='DELETE')return {state:'locally_discarded'};return path.includes('/pending/')?{state:'pending',revision:'e'.repeat(64),text:'actual',stage_ids:record.attachments}:{entries:[localReviewRow('pending',record.publicationKey,{text:'actual',stage_ids:record.attachments})]};},fencePeoplePending:()=>{events.push('fence');return {state:'locally_retired',cancelled:0};},clearPeoplePending:()=>{events.push('clear');if(!storageReady)return false;raw=null;return true;}}});
 const old=ctx.roots.attstage.querySelectorAll;let discards=[],cleanup=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-local-discard'?(discards=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-local-discard(?:\s+[^"\s]+)*" data-row="(\d+)" data-id="([0-9a-f]+)"/g)].map(m=>el('button',{dataset:{row:m[1],id:m[2]}}))):s==='.people-browser-retire'?(cleanup=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-browser-retire(?:\s+[^"\s]+)*" data-row="(\d+)" data-snapshot="([^"]+)"/g)].map(m=>el('button',{dataset:{row:m[1],snapshot:m[2]}}))):old(s);
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('draft');ctx.roots.prompt.firstListener('input')();await localReviewTick();discards[0].click();await localReviewTick();assert.equal(raw,record.value);assert.equal(cleanup.length,1);assert.equal(renderedClassCount(ctx.roots.attstage.innerHTML, "people-retry"),0);storageReady=true;cleanup[0].click();await localReviewTick();assert.equal(raw,null);assert.deepEqual(events,['GET','GET','DELETE','fence','clear','fence','clear']);assert.equal(ctx.feature.promptText(),'draft');assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('ended People copy explicitly removes the exact reviewed orphan stage',async()=>{
 const copyInstance='1'.repeat(32),id='2'.repeat(16),revision='e'.repeat(64),calls=[];
 const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance,writable:false,attachments:false}),localReviewApi:async(path,options)=>{calls.push({path,options});return options.method==='GET'?{entries:[localReviewRow('stage',id,{name:'orphan.txt',mime:'text/plain',size:7,hash:'f'.repeat(64)})]}:null;}}});
 const old=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-local-stage-remove'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-local-stage-remove(?:\s+[^"\s]+)*" data-row="(\d+)" data-id="([0-9a-f]+)"/g)].map(m=>el('button',{dataset:{row:m[1],id:m[2]}}))):old(s);
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('saved draft');ctx.roots.prompt.firstListener('input')();await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/orphan.txt/);assert.match(ctx.roots.attstage.innerHTML,new RegExp(revision));assert.equal(buttons.length,1);assert.deepEqual(calls.map(c=>c.options.method),['GET']);buttons[0].click();await localReviewTick();assert.equal(calls[1].path,`/api/team/subscriptions/${localReviewKey}/attachments/stage/${id}?copy_instance=${copyInstance}&revision=${revision}`);assert.equal(calls[1].options.method,'DELETE');assert.doesNotMatch(ctx.roots.attstage.innerHTML,/orphan.txt/);assert.equal(ctx.feature.promptText(),'saved draft');assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

function localStageReviewFixture({outcome,contexts}={}) {
 let context={key:localReviewKey,copyInstance:'1'.repeat(32),writable:false,attachments:false};const calls=[],stage=localReviewRow('stage','2'.repeat(16),{name:'saved.txt',mime:'text/plain',size:3,hash:'f'.repeat(64)});
 const ctx=makeFeature({sel:contexts?'n1':'team/'+localReviewKey,nodes:contexts?[{id:'n1',instance_id:'8'.repeat(32)}]:[],deps:{peopleContext:()=>contexts?null:context,localReviewContexts:()=>contexts||[],localReviewApi:async(path,opts)=>{calls.push({path,opts});return opts.method==='GET'?{entries:[stage]}:outcome(path,opts);}}});
 const old=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-local-stage-remove'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-local-stage-remove(?:\s+[^"\s]+)*" data-row="(\d+)" data-id="([0-9a-f]+)"/g)].map(m=>el('button',{dataset:{row:m[1],id:m[2]}}))):old(s);
 return {ctx,calls,stage,buttons:()=>buttons,setContext:value=>{context=value;},context:()=>context};
}

test('saved stage refusal preserves the reviewed record and permits an explicit retry',async()=>{
 for(const status of [409,403,507]){
  let count=0;const f=localStageReviewFixture({outcome:async()=>{if(++count===1)throw Object.assign(Error('unavailable'),{status});return null;}});
  try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();f.buttons()[0].click();await localReviewTick();assert.match(f.ctx.roots.attstage.innerHTML,/saved.txt/);assert.match(f.ctx.roots.attstage.innerHTML,/removal is unavailable/i);assert.equal(f.buttons().length,1);assert.equal(f.calls.filter(c=>c.opts.method==='DELETE').length,1);f.buttons()[0].click();await localReviewTick();assert.doesNotMatch(f.ctx.roots.attstage.innerHTML,/saved.txt/);assert.equal(f.calls.filter(c=>c.opts.method==='DELETE').length,2);assert.equal(f.ctx.apiCalls.length,0);}finally{f.ctx.feature.destroy();}
 }
});

test('saved stage action is single flight and late results cannot repaint another view',async()=>{
 for(const change of ['selection','copy','destroy','rebind']){
  let settle;const f=localStageReviewFixture({outcome:()=>new Promise(resolve=>{settle=resolve;})});
  try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();const click=f.buttons()[0].firstListener('click');click();click();assert.equal(f.calls.filter(c=>c.opts.method==='DELETE').length,1);
   if(change==='selection'){f.ctx.setSel('');f.ctx.feature.onSelect();}else if(change==='copy'){f.setContext({...f.context(),copyInstance:'2'.repeat(32)});f.ctx.feature.contextChanged();}else{f.ctx.feature.destroy();if(change==='rebind'){f.ctx.feature.bind();f.ctx.feature.onSelect();}}
   await localReviewTick();const before=f.ctx.roots.attstage.innerHTML;settle(null);await localReviewTick();assert.equal(f.ctx.roots.attstage.innerHTML,before);assert.equal(f.ctx.apiCalls.length,0);
  }finally{f.ctx.feature.destroy();}
 }
});

test('saved stage cleanup on an ended owner preserves other copies and Agent editing',async()=>{
 const contexts=[{key:localReviewKey,copyInstance:'1'.repeat(32),owner:true},{key:localReviewKey,copyInstance:'4'.repeat(32),owner:true}],f=localStageReviewFixture({contexts,outcome:async()=>null});
 try{f.ctx.feature.bind();f.ctx.feature.onSelect();f.ctx.feature.setPromptText('agent draft');f.ctx.roots.prompt.firstListener('input')();await localReviewTick();assert.equal(f.buttons().length,2);f.buttons()[0].click();await localReviewTick();assert.equal(f.buttons().length,1);assert.match(f.ctx.roots.attstage.innerHTML,/saved.txt/);assert.equal(f.ctx.feature.promptText(),'agent draft');assert.equal(f.ctx.feature.destination(),'agent');assert.equal(f.calls.filter(c=>c.opts.method==='DELETE').length,1);assert.equal(f.ctx.apiCalls.length,0);}finally{f.ctx.feature.destroy();}
});

test('saved stage action refuses stale and forged review controls before dispatch',async()=>{
 const f=localStageReviewFixture({outcome:async()=>null}),old=f.ctx.roots.attstage.querySelectorAll,forged=el('button',{dataset:{row:'999',id:'2'.repeat(16)}});f.ctx.roots.attstage.querySelectorAll=s=>s==='.people-local-stage-remove'?[...old(s),forged]:old(s);
 try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();assert.equal(forged.listenerCount('click'),0);const click=f.buttons()[0].firstListener('click');f.ctx.setSel('');f.ctx.feature.onSelect();click();forged.click();await localReviewTick();assert.equal(f.calls.filter(c=>c.opts.method==='DELETE').length,0);}finally{f.ctx.feature.destroy();}
 const g=localStageReviewFixture({outcome:async()=>null});let html='',arm=false;Object.defineProperty(g.ctx.roots.attstage,'innerHTML',{get:()=>html,set:value=>{html=value;if(arm&&value.includes('Removing saved file')){arm=false;g.ctx.setSel('');}}});
 try{g.ctx.feature.bind();g.ctx.feature.onSelect();await localReviewTick();const click=g.buttons()[0].firstListener('click');arm=true;click();await localReviewTick();assert.equal(g.calls.filter(c=>c.opts.method==='DELETE').length,0);}finally{g.ctx.feature.destroy();}
});


test('an inventoried stage replacement rejects the original revision and stays available',async()=>{
 const copyInstance='1'.repeat(32),id='2'.repeat(16),calls=[];let current=localReviewRow('stage',id,{name:'original.txt',mime:'text/plain',size:3,hash:'f'.repeat(64)}),browserRemovals=0;
 const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance,writable:false}),peopleFileStore:{list:async()=>[],removeStageFile:async()=>{browserRemovals++;}},localReviewApi:async(path,opts)=>{calls.push({path,opts});if(opts.method==='GET')return {entries:[current]};if(!path.includes('revision='+current.revision))throw Object.assign(Error('replaced'),{status:409});current=null;return null;}}});
 const old=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-local-stage-remove'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-local-stage-remove(?:\s+[^"\s]+)*" data-row="(\d+)" data-id="([0-9a-f]+)"/g)].map(m=>el('button',{dataset:{row:m[1],id:m[2]}}))):old(s);
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/original.txt/);current={...current,name:'replacement.txt',revision:'b'.repeat(64)};buttons[0].click();await localReviewTick();assert.equal(current.name,'replacement.txt');assert.match(calls.at(-1).path,new RegExp('revision='+ 'e'.repeat(64)));assert.match(ctx.roots.attstage.innerHTML,/removal is unavailable/i);assert.equal(browserRemovals,0);ctx.setSel('');ctx.feature.onSelect();ctx.setSel('team/'+localReviewKey);ctx.feature.onSelect();await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/replacement.txt/);assert.match(ctx.roots.attstage.innerHTML,new RegExp('b'.repeat(64)));assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('a confirmed People retry cannot consume an older association for a different publication',async()=>{
 const id='0123456789abcdef',token='11111111-1111-1111-1111-111111111111',revision='a'.repeat(64),value=JSON.stringify({request:{key:'2'.repeat(16),text:'same caption',attachments:[id]}});let pending={key:'s/o/c/g',instance:'',text:'same caption',attachments:[id],value},retries=0;
 const fileStore={list:async()=>[{id,fileToken:token,stageRevision:revision,file:{name:'saved.txt',type:'text/plain',size:1}}],markPublishedBatch:async()=>{},remove:async()=>{}};
 const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true,attachments:true}),peopleFileStore:fileStore,pendingPeople:()=>pending,retryPeople:async()=>{retries++;return {seq:7}},finishPeople:()=>{pending=null;return true}}});
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('same caption');ctx.roots.prompt.firstListener('input')();const associationKey='scimux-people-pending-draft:people\0s/o/c/g\0n1\0fallback:1',association=JSON.stringify({binding:ctx.storage.getItem('scimux-people-draft:n1:binding'),text:'same caption',version:ctx.storage.getItem('scimux-people-draft:n1:version'),publicationKey:'9'.repeat(16),pendingValue:value,files:[{id,fileToken:token,stageRevision:revision}]});ctx.storage.setItem(associationKey,association);await localReviewTick();assert.equal(retries,0);ctx.roots.attstage._retryButtons[0].click();await localReviewTick();assert.equal(retries,1);assert.equal(ctx.feature.promptText(),'same caption');assert.equal(ctx.storage.getItem(associationKey),association);assert.match(ctx.roots.attstage.innerHTML,/saved.txt/);assert.equal(ctx.apiCalls.filter(c=>c.path.endsWith('/send')).length,0);}finally{ctx.feature.destroy();}
});

test('legacy People caption association remains reviewable after explicit confirmed retry',async()=>{
 const key=localReviewKey,copyInstance='1'.repeat(32),binding='current',scope=`people\0${key}\0binding:${binding}\0copy:${copyInstance}\0`,id='0123456789abcdef',file={name:'saved.txt',type:'text/plain',size:1},value=JSON.stringify({instance:'',copyInstance,request:{key:'2'.repeat(16),text:'old caption',anchor:0,attachments:[id]}}),storageKey=`scimux-people-file-pending:${key}::${binding}:copy:${copyInstance}`,associationKey='scimux-people-pending-draft:'+scope,association=JSON.stringify({binding:'legacy binding',text:'old caption',version:'1'});let files=[{key:scope+'\0'+id,scope,id,file}],recovery=[{key:'allocator',epoch:'a'.repeat(32),next:1}],tail=Promise.resolve(),retries=0;
 const writePeople=fn=>{const op=tail.then(()=>{const result=fn({files,recovery});files=result.files;recovery=result.recovery;return result.value;});tail=op.then(()=>{},()=>{});return op;},driver={read:()=>tail.then(()=>files),readWrite:fn=>writePeople(({files,recovery})=>{const result=fn(files);return {files:result.rows,recovery,value:result.value};}),writePeople},store=createPeopleFileStore(driver);let pending={format:'legacy',key,copyInstance,binding,instance:'',storageKey,value,publicationKey:'2'.repeat(16),text:'old caption',attachments:[id]};
 const ctx=makeFeature({sel:'team/'+key,nodes:[],deps:{peopleContext:()=>({key,copyInstance,binding,owner:false,writable:true,attachments:true}),peopleFileStore:store,pendingPeople:()=>pending,retryPeople:async()=>{retries++;return {seq:1,peoplePending:pending};},finishPeople:async(receipt,options)=>{const result=await store.recordLegacyPeopleDecision({scope,snapshot:receipt.peoplePending,proof:{state:'completed',seq:receipt.seq,text:'old caption',stage_ids:[id]},current:options.current});pending={...pending,locallyRetired:true};return {state:result.state,seq:1,filesConfirmed:false,draftCleared:false};}}});
 try{ctx.storage.setItem(storageKey,value);ctx.storage.setItem(associationKey,association);ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('old caption');ctx.roots.prompt.firstListener('input')();await localReviewTick();assert.equal(retries,0);ctx.roots.attstage._retryButtons[0].click();await localReviewTick();assert.equal(retries,1);assert.equal(pending.value,value);assert.equal(ctx.storage.getItem(storageKey),value);assert.equal(ctx.feature.promptText(),'old caption');assert.equal(ctx.storage.getItem(associationKey),association);assert.equal(files[0].file,file);assert.equal(files[0].published,undefined);assert.equal(recovery.find(row=>row.kind==='legacy-decision').seq,1);assert.match(ctx.roots.attstage.innerHTML,/saved.txt/);assert.equal(ctx.apiCalls.filter(c=>c.path.endsWith('/send')).length,0);}finally{ctx.feature.destroy();}
});

test('People pending draft association records the actual publication and saved file identity',async()=>{
 let release;const context={key:localReviewKey,copyInstance:'1'.repeat(32),binding:'current',owner:true,writable:true,attachments:true},f=transactionalPeopleFixture({context,nodes:[{id:'n1',instance_id:'8'.repeat(32)}],respond:()=>new Promise(resolve=>{release=resolve;})}),ctx=f.ctx;
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('caption');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await f.waitReady(1);const flight=ctx.feature.sendPrompt();await f.waitSend(1);const snapshot=f.pending(),saved=await f.actual.listPeopleRecovery(snapshot.scope),row=saved.pending[0],file=f.files()[0];assert.equal(f.sendEntries(),1);assert.equal(f.sends.length,1);assert.equal(row.publication,snapshot.publication);assert.equal(row.request.key,snapshot.publicationKey);assert.equal(row.request.text,'caption');assert.deepEqual(row.request.attachments,['0123456789abcdef']);assert.equal(row.head,snapshot.head);assert.equal(row.version,snapshot.version);assert.equal(row.scope,snapshot.scope);assert.equal(row.peopleKey,context.key);assert.equal(row.copyInstance,context.copyInstance);assert.equal(row.binding,context.binding);assert.equal(row.native,'8'.repeat(32));assert.deepEqual(row.files,[{id:file.id,fileToken:file.fileToken,stageRevision:'a'.repeat(64)}]);assert.equal(ctx.storage.keys().filter(key=>key.startsWith('scimux-people-pending-draft:')).length,0);ctx.feature.destroy();release({seq:7});await flight;assert.equal(f.files()[0].fileToken,file.fileToken);assert.equal(f.files()[0].published,undefined);assert.equal(f.recovery().find(row=>row.kind==='publication').publication,snapshot.publication);}finally{if(release)release({seq:7});ctx.feature.destroy();}
});

test('a new People file send preserves an existing legacy association',async()=>{
 let release;const key='scimux-people-pending-draft:people\0s/o/c/g\0n1\0fallback:1',legacy=JSON.stringify({binding:'earlier binding',text:'older caption',version:'1'});
 const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true,attachments:true}),pendingPeople:()=>null,sendPeople:()=>new Promise(resolve=>{release=resolve;})},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:'0123456789abcdef'}]})})});
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.storage.setItem(key,legacy);ctx.feature.setPromptText('new caption');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'new.txt',type:'text/plain',size:1}]);await localReviewTick();const flight=ctx.feature.sendPrompt();await localReviewTick();assert.equal(ctx.storage.getItem(key),legacy);release({seq:4});await flight;assert.equal(ctx.storage.getItem(key),legacy);}finally{ctx.feature.destroy();}
});

test('a replacement file cannot be published through an older pending People request',async()=>{
 let sends=0,marks=0;const pending={key:'s/o/c/g',text:'caption',attachments:['0123456789abcdef']};
 const store={save:async()=>{},setStageRevision:async()=>{},list:async()=>[],markPublishedBatch:async()=>{marks++;},removeStageFile:async()=>assert.fail('replacement must remain')};
 const ctx=makeFeature({deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true,attachments:true}),peopleFileStore:store,pendingPeople:()=>pending,sendPeople:async()=>{sends++;return {seq:7}}},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id:'0123456789abcdef'}]})})});
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('caption');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'replacement.txt',type:'text/plain',size:1}]);await localReviewTick();await ctx.feature.sendPrompt();assert.equal(sends,0);assert.equal(marks,0);assert.match(ctx.roots.attstage.innerHTML,/replacement.txt/);assert.match(ctx.alerts.join(' '),/review/i);}finally{ctx.feature.destroy();}
});

test('People retry preserves files when saved association belongs to a different draft binding',async()=>{
 const key=localReviewKey,copyInstance='1'.repeat(32),binding='old',instance='8'.repeat(32),scope=`people\0${key}\0binding:${binding}\0copy:${copyInstance}\0n1\0${instance}`,id='0123456789abcdef',revision='a'.repeat(64),context={key,copyInstance,binding:'current',writable:true,owner:true,attachments:true};let pending=null,f;
 f=transactionalPeopleFixture({context,nodes:[{id:'n1',instance_id:instance}],depsExtra:{pendingPeople:()=>pending}});const ctx=f.ctx,file={name:'saved.txt',type:'text/plain',size:1},token=await f.actual.save(scope,id,file);await f.actual.setStageRevision(scope,id,revision,token);const draft=await f.actual.savePeopleDraft({scope,text:'caption'}),request={key:'2'.repeat(16),text:'caption',anchor:0,attachments:[id]},saved=await f.actual.beginPeoplePublication({scope,peopleKey:key,copyInstance,binding,native:instance,...draft,request,files:[{id,fileToken:token,stageRevision:revision}]});pending={format:'transactional',scope,publication:saved.publication,head:draft.head,version:draft.version,key,instance,copyInstance,binding,storageKey:'idb:'+saved.publication,value:JSON.stringify({instance,copyInstance,request}),publicationKey:request.key,text:request.text,attachments:request.attachments};
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('caption');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await f.waitReady(1);const current=f.files().find(row=>row.scope!==scope);assert.ok(current);await localReviewTick();ctx.roots.attstage._retryButtons[0].click();await localReviewTick();assert.equal(f.retries.length,0);assert.equal(f.events.filter(event=>event==='finish').length,0);assert.equal(f.recovery().find(row=>row.publication===saved.publication).state,'pending');assert.equal((await f.actual.list(scope))[0].fileToken,token);assert.equal((await f.actual.list(current.scope))[0].fileToken,current.fileToken);assert.equal(ctx.feature.promptText(),'caption');assert.match(ctx.roots.attstage.innerHTML,/saved.txt/);assert.equal(f.sendEntries(),0);}finally{ctx.feature.destroy();}
});

test('a destroyed People flight cannot write an association after a synchronous rebind',async()=>{
 const key=localReviewKey,copyInstance='1'.repeat(32),instance='8'.repeat(32),binding='current',id='0123456789abcdef',publicationKey='2'.repeat(16),fileToken='11111111-1111-1111-1111-111111111111',revision='a'.repeat(64);let pending=null;
 const store={save:async()=>fileToken,setStageRevision:async()=>{},list:async()=>[],markPublishedBatch:async()=>assert.fail('stale flight'),removeStageFile:async()=>assert.fail('stale flight')};let ctx;
 ctx=makeFeature({nodes:[{id:'n1',instance_id:instance}],deps:{peopleContext:()=>({key,copyInstance,binding,owner:true,writable:true,attachments:true}),peopleFileStore:store,pendingPeople:()=>pending,sendPeople:()=>{const value=JSON.stringify({instance,copyInstance,request:{text:'caption',anchor:0,key:publicationKey,attachments:[id]}});pending={key,instance,storageKey:`scimux-people-file-pending:${key}:${instance}:${binding}:copy:${copyInstance}`,value,text:'caption',attachments:[id]};ctx.feature.destroy();ctx.feature.bind();ctx.feature.onSelect();return Promise.resolve({seq:7});}},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id,revision}]})})});
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('caption');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await localReviewTick();await ctx.feature.sendPrompt();assert.equal(ctx.storage.keys().filter(k=>k.startsWith('scimux-people-pending-draft:')).length,0);assert.match(ctx.roots.attstage.innerHTML,/saved.txt/);}finally{ctx.feature.destroy();}
});

test('explicit retirement waits for an already-started file mark before its durable decision',async()=>{
 const context={key:localReviewKey,copyInstance:'1'.repeat(32),binding:'current',owner:true,writable:true,attachments:true},events=[];let f;
 f=transactionalPeopleFixture({context,nodes:[{id:'n1',instance_id:'8'.repeat(32)}],depsExtra:{localReviewApi:async(path,opts)=>{events.push(opts.method);if(opts.method==='POST')throw Object.assign(Error('{"error":"review_required"}'),{status:409});return path.includes('/pending/')?{state:'missing'}:{entries:[]};},fencePeoplePending:()=>{events.push('fence');return {state:'locally_retired',cancelled:0};},clearPeoplePending:()=>{events.push('clear');return true;}}});const ctx=f.ctx,gate=f.holdNext('decision');
 const old=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-browser-retire'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-browser-retire(?:\s+[^"\s]+)*" data-row="(\d+)" data-snapshot="([^"]+)"/g)].map(m=>el('button',{dataset:{row:m[1],snapshot:m[2]}}))):old(s);
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('caption');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await f.waitReady(1);const flight=ctx.feature.sendPrompt();await gate.ready;const releaseMark=gate.release;assert.equal(typeof releaseMark,'function');ctx.feature.renderStage();await localReviewTick();assert.equal(buttons.length,1);buttons[0].click();await localReviewTick();assert.equal(events.includes('POST'),false);releaseMark();await flight;await localReviewTick();assert.equal(events.filter(x=>x==='POST').length,1);assert.equal(f.recovery().filter(row=>row.kind==='decision').length,0);assert.ok(!events.includes('clear'));assert.equal(f.pending().text,'caption');assert.match(ctx.roots.attstage.innerHTML,/saved.txt/);assert.equal(f.files()[0].published,undefined);assert.equal(f.sendEntries(),1);assert.equal(f.sends.length,1);}finally{gate.release();ctx.feature.destroy();}
});

for(const pinned of [false,true]) test(`explicit completed People recovery releases the ${pinned?'pinned':'unpinned'} local retry`,async()=>{
 const projection={state:'completed',revision:'e'.repeat(64),text:'retained synthetic',stage_ids:['3'.repeat(16)],seq:17,...(pinned?{pending_pinned:true}:{})};
 const f=browserRetireFixture({state:projection,remove:async()=>({state:'already_completed',seq:17})});
 try{f.ctx.feature.bind();f.ctx.feature.onSelect();f.ctx.feature.setPromptText('new draft');f.ctx.roots.prompt.firstListener('input')();await localReviewTick();assert.match(f.ctx.roots.attstage.innerHTML,/sequence 17/);assert.equal(f.buttons().length,1);assert.ok(f.events.every(x=>x==='GET'));f.buttons()[0].click();await localReviewTick();assert.deepEqual(f.events,pinned?['GET','GET','DELETE','fence','clear']:['GET','GET','fence','clear']);assert.equal(f.raw(),null);assert.equal(f.ctx.feature.promptText(),'new draft');assert.equal(f.ctx.apiCalls.length,0);}finally{f.ctx.feature.destroy();}
});

test('completed publication with different content cannot clear a browser retry',async()=>{
 const f=browserRetireFixture({state:{state:'completed',revision:'e'.repeat(64),text:'other content',stage_ids:['3'.repeat(16)],seq:17}});
 try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();assert.match(f.ctx.roots.attstage.innerHTML,/other content/);assert.match(f.ctx.roots.attstage.innerHTML,/sequence 17/);assert.equal(f.buttons().length,0);assert.equal(f.raw(),f.record.value);assert.deepEqual(f.events,['GET','GET']);}finally{f.ctx.feature.destroy();}
});

for(const replacement of [false,true]) test(`completed recovery ${replacement?'preserves a replacement':'marks the proven saved file'} without removing bytes`,async()=>{
 const key=localReviewKey,copyInstance='1'.repeat(32),binding='current',scope=`people\0${key}\0binding:${binding}\0copy:${copyInstance}\0`,id='3333333333333333',revision='a'.repeat(64),events=[],context={key,copyInstance,binding,owner:false,writable:true,attachments:true};let pending=null,f;
 f=transactionalPeopleFixture({context,depsExtra:{pendingPeople:()=>pending,localReviewApi:async(path,opts)=>{events.push(opts.method);return path.includes('/pending/')?{state:'completed',revision:'e'.repeat(64),text:'retained synthetic',stage_ids:[id],seq:17}:{entries:[]};},fencePeoplePending:()=>{events.push('fence');return {state:'locally_retired',cancelled:0};},clearPeoplePending:async(record,options)=>{events.push('clear');const result=await f.actual.decidePeoplePublication({scope,publication:record.publication,expected:record,proof:options.proof,current:options.current});pending=null;return result;}}});const ctx=f.ctx,file={name:'saved.txt',type:'text/plain',size:1},token=await f.actual.save(scope,id,file);await f.actual.setStageRevision(scope,id,revision,token);const draft=await f.actual.savePeopleDraft({scope,text:'retained synthetic'}),request={key:'2'.repeat(16),text:'retained synthetic',anchor:0,attachments:[id]},saved=await f.actual.beginPeoplePublication({scope,peopleKey:key,copyInstance,binding,native:'',...draft,request,files:[{id,fileToken:token,stageRevision:revision}]});pending={format:'transactional',scope,publication:saved.publication,head:draft.head,version:draft.version,key,instance:'',copyInstance,binding,storageKey:'idb:'+saved.publication,value:JSON.stringify({instance:'',copyInstance,request}),publicationKey:request.key,text:request.text,attachments:request.attachments};let replacementToken;if(replacement){await f.actual.remove(scope,id);replacementToken=await f.actual.save(scope,id,file);await f.actual.setStageRevision(scope,id,revision,replacementToken);}
 const old=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-browser-retire'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-browser-retire(?:\s+[^"\s]+)*" data-row="(\d+)" data-snapshot="([^"]+)"/g)].map(m=>el('button',{dataset:{row:m[1],snapshot:m[2]}}))):old(s);
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(ctx.feature.promptText(),'retained synthetic');assert.equal(buttons.length,1);buttons[0].click();await localReviewTick();assert.equal(pending,null);const decision=f.recovery().find(row=>row.publication===saved.publication);assert.equal(decision.state,'completed');assert.equal(decision.filesConfirmed,!replacement);const rows=await f.actual.list(scope);assert.equal(rows.length,1);assert.equal(rows[0].fileToken,replacement?replacementToken:token);assert.equal(rows[0].published,replacement?undefined:17);assert.equal(ctx.feature.promptText(),replacement?'retained synthetic':'');assert.match(ctx.roots.attstage.innerHTML,/saved.txt/);if(!replacement){await ctx.feature.sendPrompt();assert.equal(f.sendEntries(),0);}assert.deepEqual(events,['GET','GET','fence','clear']);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('pending discard that discovers completion requires a new explicit bookkeeping action',async()=>{
 const record=retainedBrowserSnapshot({instance:'',binding:'current'}),events=[];let raw=record.value;
 const ctx=makeFeature({sel:'team/'+record.key,nodes:[],deps:{peopleContext:()=>({key:record.key,copyInstance:record.copyInstance,binding:'current',owner:false,writable:true}),pendingPeople:()=>raw?{...record,value:raw}:null,localReviewApi:async(path,opts)=>{events.push(opts.method);if(opts.method==='DELETE')return {state:'already_completed',seq:23};return path.includes('/pending/')?{state:'pending',revision:'e'.repeat(64),text:record.text,stage_ids:record.attachments}:{entries:[localReviewRow('pending',record.publicationKey,{text:record.text,stage_ids:record.attachments})]};},fencePeoplePending:()=>{events.push('fence');return {state:'locally_retired',cancelled:0};},clearPeoplePending:()=>{events.push('clear');raw=null;return true;}}});
 const old=ctx.roots.attstage.querySelectorAll;let discard=[],finish=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-local-discard'?(discard=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-local-discard(?:\s+[^"\s]+)*" data-row="(\d+)" data-id="([0-9a-f]+)"/g)].map(m=>el('button',{dataset:{row:m[1],id:m[2]}}))):s==='.people-browser-retire'?(finish=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-browser-retire(?:\s+[^"\s]+)*" data-row="(\d+)" data-snapshot="([^"]+)"/g)].map(m=>el('button',{dataset:{row:m[1],snapshot:m[2]}}))):old(s);
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();discard[0].click();await localReviewTick();assert.equal(raw,record.value);assert.deepEqual(events,['GET','GET','DELETE']);assert.match(ctx.roots.attstage.innerHTML,/sequence 23/);assert.equal(finish.length,1);finish[0].click();await localReviewTick();assert.equal(raw,null);assert.deepEqual(events,['GET','GET','DELETE','fence','clear']);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('confirmed People send removes only its exact staged inventory row',async()=>{
 const id='0123456789abcdef',revision='a'.repeat(64),other='fedcba9876543210',inventory=[localReviewRow('stage',id,{revision,name:'sent.txt',mime:'text/plain',size:1,hash:'f'.repeat(64)}),localReviewRow('stage',other,{name:'other.txt',mime:'text/plain',size:1,hash:'f'.repeat(64)})],f=transactionalPeopleFixture({depsExtra:{localReviewApi:async()=>({entries:inventory})}}),ctx=f.ctx;
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/sent.txt/);ctx.feature.addFiles([{name:'sent.txt',type:'text/plain',size:1}]);await f.waitReady(1);await ctx.feature.sendPrompt();await localReviewTick();assert.doesNotMatch(ctx.roots.attstage.innerHTML,/sent.txt/);assert.match(ctx.roots.attstage.innerHTML,/other.txt/);assert.equal(f.sends.length,1);assert.equal(f.recovery().find(row=>row.kind==='decision').seq,7);assert.equal(f.files().length,0);assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);}finally{ctx.feature.destroy();}
});

test('explicit People retry discard clears only its proven pending association',async()=>{
 const key=localReviewKey,copyInstance='1'.repeat(32),binding='current',scope=`people\0${key}\0binding:${binding}\0copy:${copyInstance}\0`,id='3333333333333333',revision='a'.repeat(64),events=[],context={key,copyInstance,binding,owner:false,writable:false,attachments:false};let pending=null,f;
 f=transactionalPeopleFixture({context,depsExtra:{pendingPeople:()=>pending,localReviewApi:async(path,opts)=>{events.push(opts.method);return opts.method==='POST'?{state:'locally_discarded'}:path.includes('/pending/')?{state:'missing'}:{entries:[]};},fencePeoplePending:()=>{events.push('fence');return {state:'locally_retired',cancelled:0};},clearPeoplePending:async(record,options)=>{events.push('clear');const result=await f.actual.decidePeoplePublication({scope,publication:record.publication,expected:record,proof:options.proof,current:options.current});pending=null;return result;}}});const ctx=f.ctx,token=await f.actual.save(scope,id,{name:'saved.txt',type:'text/plain',size:1});await f.actual.setStageRevision(scope,id,revision,token);const draft=await f.actual.savePeopleDraft({scope,text:'retained synthetic'}),request={key:'2'.repeat(16),text:'retained synthetic',anchor:0,attachments:[id]},saved=await f.actual.beginPeoplePublication({scope,peopleKey:key,copyInstance,binding,native:'',...draft,request,files:[{id,fileToken:token,stageRevision:revision}]});pending={format:'transactional',scope,publication:saved.publication,head:draft.head,version:draft.version,key,instance:'',copyInstance,binding,storageKey:'idb:'+saved.publication,value:JSON.stringify({instance:'',copyInstance,request}),publicationKey:request.key,text:request.text,attachments:request.attachments};
 const old=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-browser-retire'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-browser-retire(?:\s+[^"\s]+)*" data-row="(\d+)" data-snapshot="([^"]+)"/g)].map(m=>el('button',{dataset:{row:m[1],snapshot:m[2]}}))):old(s);
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('new draft');ctx.roots.prompt.firstListener('input')();ctx.storage.setItem('unrelated-association','preserved');await localReviewTick();buttons[0].click();await localReviewTick();assert.equal(pending,null);assert.equal(f.recovery().find(row=>row.publication===saved.publication).state,'retired');assert.equal(ctx.storage.getItem('unrelated-association'),'preserved');assert.equal(ctx.feature.promptText(),'new draft');assert.deepEqual(events,['GET','GET','POST','fence','clear']);assert.equal((await f.actual.list(scope))[0].fileToken,token);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

for(const response of [{state:'already_completed',seq:18},{state:'locally_discarded'}]) test(`pinned completion refuses contradictory release ${response.state}`,async()=>{
 const projection={state:'completed',revision:'e'.repeat(64),text:'retained synthetic',stage_ids:['3'.repeat(16)],seq:17,pending_pinned:true},f=browserRetireFixture({state:projection,remove:async()=>response});
 try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();f.buttons()[0].click();await localReviewTick();assert.equal(f.raw(),f.record.value);assert.deepEqual(f.events,['GET','GET','DELETE']);assert.match(f.ctx.roots.attstage.innerHTML,/sequence 17/);assert.match(f.ctx.roots.attstage.innerHTML,/cleanup is unconfirmed/i);assert.equal(f.buttons().length,1);assert.equal(f.ctx.apiCalls.length,0);}finally{f.ctx.feature.destroy();}
});

test('failed saved-file barrier preserves local retirement review without dispatch',async()=>{
 const record=retainedBrowserSnapshot({instance:'',binding:'current'}),events=[];let raw=record.value;
 const ctx=makeFeature({sel:'team/'+record.key,nodes:[],deps:{peopleContext:()=>({key:record.key,copyInstance:record.copyInstance,binding:'current',owner:false,writable:false}),peopleFileStore:{list:async()=>{throw Error('unavailable');}},pendingPeople:()=>({...record,value:raw}),localReviewApi:async(path,opts)=>{events.push(opts.method);return path.includes('/pending/')?{state:'missing'}:{entries:[]};},fencePeoplePending:()=>assert.fail('must not fence'),clearPeoplePending:()=>assert.fail('must not clear')}});
 const old=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-browser-retire'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-browser-retire(?:\s+[^"\s]+)*" data-row="(\d+)" data-snapshot="([^"]+)"/g)].map(m=>el('button',{dataset:{row:m[1],snapshot:m[2]}}))):old(s);
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();buttons[0].click();await localReviewTick();assert.equal(raw,record.value);assert.deepEqual(events,['GET','GET']);assert.match(ctx.roots.attstage.innerHTML,/cleanup is unconfirmed/i);assert.equal(buttons.length,1);}finally{ctx.feature.destroy();}
});

test('completed recovery cannot clear a newer same-text publication while its file mark is held',async()=>{
 const key=localReviewKey,copyInstance='1'.repeat(32),binding='current',scope=`people\0${key}\0binding:${binding}\0copy:${copyInstance}\0`,id='3333333333333333',revision='a'.repeat(64),events=[],context={key,copyInstance,binding,owner:false,writable:true,attachments:true};let pending=null,f,release,entered,held=false;const ready=new Promise(resolve=>{entered=resolve;});
 f=transactionalPeopleFixture({context,depsExtra:{pendingPeople:()=>pending,localReviewApi:async(path,opts)=>{events.push(opts.method);return path.includes('/pending/')?{state:'completed',revision:'e'.repeat(64),text:'retained synthetic',stage_ids:[id],seq:17}:{entries:[]};},fencePeoplePending:()=>{events.push('fence');return {state:'locally_retired',cancelled:0};},clearPeoplePending:()=>{events.push('clear');return false;}}});const ctx=f.ctx,file={name:'saved.txt',type:'text/plain',size:1},token=await f.actual.save(scope,id,file);await f.actual.setStageRevision(scope,id,revision,token);const draft=await f.actual.savePeopleDraft({scope,text:'retained synthetic'}),request={key:'2'.repeat(16),text:'retained synthetic',anchor:0,attachments:[id]},base={scope,peopleKey:key,copyInstance,binding,native:'',files:[{id,fileToken:token,stageRevision:revision}]},saved=await f.actual.beginPeoplePublication({...base,...draft,request});const snapshot=(publication,head,body)=>({format:'transactional',scope,publication,head:head.head,version:head.version,key,instance:'',copyInstance,binding,storageKey:'idb:'+publication,value:JSON.stringify({instance:'',copyInstance,request:body}),publicationKey:body.key,text:body.text,attachments:body.attachments});const original=snapshot(saved.publication,draft,request);pending=original;
 const list=f.store.list;f.store.list=async identity=>{const rows=await list(identity);if(held){held=false;entered();await new Promise(resolve=>{release=resolve;});}return rows;};const old=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-browser-retire'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-browser-retire(?:\s+[^"\s]+)*" data-row="(\d+)" data-snapshot="([^"]+)"/g)].map(m=>el('button',{dataset:{row:m[1],snapshot:m[2]}}))):old(s);
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(ctx.feature.promptText(),request.text);held=true;buttons[0].click();await ready;assert.equal(typeof release,'function');const newer=await f.actual.savePeopleDraft({scope,text:request.text}),newRequest={...request,key:'9'.repeat(16)},second=await f.actual.beginPeoplePublication({...base,...newer,request:newRequest});pending=snapshot(second.publication,newer,newRequest);release();await localReviewTick();assert.equal(pending.publication,second.publication);assert.equal(f.recovery().find(row=>row.publication===saved.publication).state,'pending');assert.equal(f.recovery().find(row=>row.publication===second.publication).state,'pending');assert.equal(ctx.feature.promptText(),request.text);assert.ok(!events.includes('clear'));assert.match(ctx.roots.attstage.innerHTML,/saved.txt/);assert.equal((await f.actual.list(scope))[0].fileToken,token);assert.equal(f.sendEntries(),0);}finally{if(release)release();ctx.feature.destroy();}
});

test('confirmed stage cleanup does not prune a different inventoried revision',async()=>{
 const id='0123456789abcdef',inventory=[localReviewRow('stage',id,{revision:'b'.repeat(64),name:'replacement.txt',mime:'text/plain',size:1,hash:'f'.repeat(64)})],f=transactionalPeopleFixture({depsExtra:{localReviewApi:async()=>({entries:inventory})}}),ctx=f.ctx;
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();ctx.feature.addFiles([{name:'sent.txt',type:'text/plain',size:1}]);await f.waitReady(1);await ctx.feature.sendPrompt();await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/replacement.txt/);assert.match(ctx.roots.attstage.innerHTML,new RegExp('b'.repeat(64)));assert.doesNotMatch(ctx.roots.attstage.innerHTML,/sent.txt/);assert.equal(f.recovery().find(row=>row.kind==='decision').seq,7);assert.equal(f.files().length,0);assert.equal(f.sends.length,1);}finally{ctx.feature.destroy();}
});

test('confirmed People publication still allows the existing explicit writable retry',async()=>{
 const f=browserRetireFixture({state:{state:'completed',revision:'e'.repeat(64),text:'retained synthetic',stage_ids:['3'.repeat(16)],seq:17}});
 try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();assert.match(f.ctx.roots.attstage.innerHTML,/sequence 17/);assert.ok(renderedClassCount(f.ctx.roots.attstage.innerHTML, "people-retry") > 0);assert.equal(f.buttons().length,1);assert.ok(f.events.every(x=>x==='GET'));}finally{f.ctx.feature.destroy();}
});

test('completed pinned release remains truthful when browser file bookkeeping fails',async()=>{
 const key=localReviewKey,copyInstance='1'.repeat(32),binding='current',scope=`people\0${key}\0binding:${binding}\0copy:${copyInstance}\0`,id='3333333333333333',revision='a'.repeat(64),events=[],context={key,copyInstance,binding,owner:false,writable:false,attachments:false};let pending=null,f;
 f=transactionalPeopleFixture({context,depsExtra:{pendingPeople:()=>pending,localReviewApi:async(path,opts)=>{events.push(opts.method);if(opts.method==='DELETE')return {state:'already_completed',seq:17};return path.includes('/pending/')?{state:'completed',revision:'e'.repeat(64),text:'retained synthetic',stage_ids:[id],seq:17,pending_pinned:true}:{entries:[]};},fencePeoplePending:()=>{events.push('fence');return {state:'locally_retired',cancelled:0};},clearPeoplePending:async(record,options)=>{events.push('decision');const result=await f.actual.decidePeoplePublication({scope,publication:record.publication,expected:record,proof:options.proof,current:options.current});pending=null;return result;}}});const ctx=f.ctx,token=await f.actual.save(scope,id,{name:'saved.txt',type:'text/plain',size:1});await f.actual.setStageRevision(scope,id,revision,token);const draft=await f.actual.savePeopleDraft({scope,text:'retained synthetic'}),request={key:'2'.repeat(16),text:'retained synthetic',anchor:0,attachments:[id]},saved=await f.actual.beginPeoplePublication({scope,peopleKey:key,copyInstance,binding,native:'',...draft,request,files:[{id,fileToken:token,stageRevision:revision}]});pending={format:'transactional',scope,publication:saved.publication,head:draft.head,version:draft.version,key,instance:'',copyInstance,binding,storageKey:'idb:'+saved.publication,value:JSON.stringify({instance:'',copyInstance,request}),publicationKey:request.key,text:request.text,attachments:request.attachments};const record=pending;
 const old=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-browser-retire'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-browser-retire(?:\s+[^"\s]+)*" data-row="(\d+)" data-snapshot="([^"]+)"/g)].map(m=>el('button',{dataset:{row:m[1],snapshot:m[2]}}))):old(s);
 try{ctx.feature.bind();ctx.feature.onSelect();f.setDecisionFailure(true);await localReviewTick();buttons[0].click();await localReviewTick();assert.equal(pending,record);assert.deepEqual(events,['GET','GET','DELETE','fence','decision']);assert.equal(f.recovery().find(row=>row.publication===saved.publication).state,'pending');assert.equal((await f.actual.list(scope))[0].fileToken,token);assert.match(ctx.roots.attstage.innerHTML,/sequence 17/);assert.doesNotMatch(ctx.roots.attstage.innerHTML,/remains pinned/i);assert.match(ctx.roots.attstage.innerHTML,/cleanup is unconfirmed/i);f.setDecisionFailure(false);buttons[0].click();await localReviewTick();assert.equal(pending,null);assert.deepEqual(events,['GET','GET','DELETE','fence','decision','fence','decision']);assert.equal(f.recovery().find(row=>row.publication===saved.publication).state,'completed');assert.equal((await f.actual.list(scope))[0].published,17);assert.equal(f.sendEntries(),0);}finally{ctx.feature.destroy();}
});

test('active People review keeps an explicitly opened disclosure through same-identity redraw',async()=>{
 const f=browserRetireFixture();const old=f.ctx.roots.attstage.querySelectorAll;let details=[];
 f.ctx.roots.attstage.querySelectorAll=s=>s==='.people-current-snapshot'?(details=[...f.ctx.roots.attstage.innerHTML.matchAll(/class="people-current-snapshot" data-row="(\d+)"(?: open)?/g)].map(m=>el('details',{dataset:{row:m[1]}}))):old(s);
 try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();assert.equal(details.length,1);details[0].open=true;details[0].firstListener('toggle')();f.ctx.feature.renderStage();assert.match(f.ctx.roots.attstage.innerHTML,/class="people-current-snapshot" data-row="0" open/);assert.equal(f.events.filter(x=>x==='POST').length,0);}finally{f.ctx.feature.destroy();}
});

test('stale or unavailable People disclosure callbacks cannot change the review',async()=>{
 const f=browserRetireFixture(),old=f.ctx.roots.attstage.querySelectorAll;let details=[];
 f.ctx.roots.attstage.querySelectorAll=s=>s==='.people-current-snapshot'?(details=[...f.ctx.roots.attstage.innerHTML.matchAll(/class="people-current-snapshot" data-row="(\d+)"(?: open)?/g)].map(m=>el('details',{dataset:{row:m[1]}}))):old(s);
 try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();const detail=details[0],toggle=detail.firstListener('toggle');detail.open=true;f.setLookupThrow(true);toggle();f.setLookupThrow(false);f.ctx.feature.renderStage();assert.doesNotMatch(f.ctx.roots.attstage.innerHTML,/class="people-current-snapshot" data-row="0" open/);const before=[...f.events];f.ctx.feature.destroy();toggle();await localReviewTick();assert.deepEqual(f.events,before);assert.equal(f.raw(),f.record.value);assert.equal(f.ctx.apiCalls.length,0);}finally{f.ctx.feature.destroy();}
});

test('active review disclosure does not carry an older raw snapshot into a newer one',async()=>{
 const f=browserRetireFixture(),old=f.ctx.roots.attstage.querySelectorAll;let details=[];f.ctx.roots.attstage.querySelectorAll=s=>s==='.people-current-snapshot'?(details=[...f.ctx.roots.attstage.innerHTML.matchAll(/class="people-current-snapshot" data-row="(\d+)"(?: open)?/g)].map(m=>el('details',{dataset:{row:m[1]}}))):old(s);
 try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();const detail=details[0],toggle=detail.firstListener('toggle');detail.open=true;toggle();const next=JSON.parse(f.record.value);next.request.key='9'.repeat(16);f.setRaw(JSON.stringify(next));f.ctx.feature.renderStage();await localReviewTick();assert.doesNotMatch(f.ctx.roots.attstage.innerHTML,/class="people-current-snapshot" data-row="0" open/);detail.open=true;toggle();f.ctx.feature.renderStage();assert.doesNotMatch(f.ctx.roots.attstage.innerHTML,/class="people-current-snapshot" data-row="0" open/);assert.ok(!f.events.includes('POST'));}finally{f.ctx.feature.destroy();}
});

test('completed recovery preserves a newer same-text publication installed during exact clear',async()=>{
 const key=localReviewKey,copyInstance='1'.repeat(32),binding='current',scope=`people\0${key}\0binding:${binding}\0copy:${copyInstance}\0`,id='3333333333333333',revision='a'.repeat(64),events=[],context={key,copyInstance,binding,owner:false,writable:true,attachments:true};let pending=null,f,second,token;
 const request={key:'2'.repeat(16),text:'retained synthetic',anchor:0,attachments:[id]},snapshot=(publication,head,body)=>({format:'transactional',scope,publication,head:head.head,version:head.version,key,instance:'',copyInstance,binding,storageKey:'idb:'+publication,value:JSON.stringify({instance:'',copyInstance,request:body}),publicationKey:body.key,text:body.text,attachments:body.attachments});
 f=transactionalPeopleFixture({context,depsExtra:{pendingPeople:()=>pending,localReviewApi:async(path,opts)=>{events.push(opts.method);return path.includes('/pending/')?{state:'completed',revision:'e'.repeat(64),text:request.text,stage_ids:[id],seq:17}:{entries:[]};},fencePeoplePending:()=>{events.push('fence');return {state:'locally_retired',cancelled:0};},clearPeoplePending:async(record,options)=>{events.push('clear');const newer=await f.actual.savePeopleDraft({scope,text:request.text}),newRequest={...request,key:'9'.repeat(16)},saved=await f.actual.beginPeoplePublication({scope,peopleKey:key,copyInstance,binding,native:'',...newer,request:newRequest,files:[{id,fileToken:token,stageRevision:revision}]});second=snapshot(saved.publication,newer,newRequest);pending=second;return f.actual.decidePeoplePublication({scope,publication:record.publication,expected:record,proof:options.proof,current:options.current});}}});const ctx=f.ctx;token=await f.actual.save(scope,id,{name:'saved.txt',type:'text/plain',size:1});await f.actual.setStageRevision(scope,id,revision,token);const draft=await f.actual.savePeopleDraft({scope,text:request.text}),saved=await f.actual.beginPeoplePublication({scope,peopleKey:key,copyInstance,binding,native:'',...draft,request,files:[{id,fileToken:token,stageRevision:revision}]});pending=snapshot(saved.publication,draft,request);
 const old=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-browser-retire'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-browser-retire(?:\s+[^"\s]+)*" data-row="(\d+)" data-snapshot="([^"]+)"/g)].map(m=>el('button',{dataset:{row:m[1],snapshot:m[2]}}))):old(s);
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(buttons.length,1);buttons[0].click();await localReviewTick();assert.ok(second);assert.equal(pending,second);assert.equal(f.recovery().find(row=>row.publication===saved.publication).state,'completed');assert.equal(f.recovery().find(row=>row.publication===second.publication).state,'pending');assert.equal(f.recovery().find(row=>row.publication===second.publication).request.key,'9'.repeat(16));assert.equal((await f.actual.list(scope))[0].fileToken,token);assert.equal((await f.actual.list(scope))[0].published,undefined);assert.equal(ctx.feature.promptText(),request.text);assert.match(ctx.roots.attstage.innerHTML,/saved.txt/);assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('captured People retry is refused while matching retirement waits for saved files',async()=>{
 const record=retainedBrowserSnapshot({instance:'',binding:'current'}),events=[];let raw=record.value,hold=false,release,retries=0;
 const store={list:async()=>{if(hold)await new Promise(resolve=>{release=resolve;});return [{id:record.attachments[0],fileToken:'11111111-1111-1111-1111-111111111111',stageRevision:'a'.repeat(64),file:{name:'synthetic.txt',type:'text/plain',size:1}}];}};
 const ctx=makeFeature({sel:'team/'+record.key,nodes:[],deps:{peopleContext:()=>({key:record.key,copyInstance:record.copyInstance,binding:'current',owner:false,writable:true,attachments:true}),peopleFileStore:store,pendingPeople:()=>raw?{...record,value:raw}:null,retryPeople:async()=>{retries++;return {seq:7};},localReviewApi:async(path,opts)=>{events.push(opts.method);return opts.method==='POST'?{state:'locally_discarded'}:path.includes('/pending/')?{state:'missing'}:{entries:[]};},fencePeoplePending:()=>({state:'locally_retired',cancelled:0}),clearPeoplePending:()=>{raw=null;return true;}}});
 const original=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-browser-retire'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-browser-retire(?:\s+[^"\s]+)*" data-row="(\d+)" data-snapshot="([^"]+)"/g)].map(m=>el('button',{dataset:{row:m[1],snapshot:m[2]}}))):original(s);
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();const retry=ctx.roots.attstage._retryButtons[0].firstListener('click');hold=true;buttons[0].click();await localReviewTick();assert.equal(typeof release,'function');assert.ok(!events.includes('POST'));retry();await localReviewTick();assert.equal(retries,0);release();await localReviewTick();assert.equal(events.filter(e=>e==='POST').length,1);assert.equal(ctx.apiCalls.length,0);}finally{if(release)release();ctx.feature.destroy();}
});

test('saved stage review explains that removing the local upload keeps the browser draft file',async()=>{
 const f=localStageReviewFixture({outcome:async()=>null});
 try{f.ctx.feature.bind();f.ctx.feature.onSelect();await localReviewTick();assert.match(f.ctx.roots.attstage.innerHTML,/Remove local upload/);assert.match(f.ctx.roots.attstage.innerHTML,/browser draft file.*kept/i);assert.equal(f.calls.filter(call=>call.opts.method==='DELETE').length,0);assert.equal(f.ctx.apiCalls.length,0);}finally{f.ctx.feature.destroy();}
});

test('new People edits preserve a legacy draft slot and save a distinct durable version',async()=>{
 const legacy='legacy synthetic caption';let recovery=[{key:'allocator',epoch:'a'.repeat(32),next:1}];const driver={read:async()=>[],readWrite:async fn=>fn([]).value,writePeople:async fn=>{const result=fn({files:[],recovery});recovery=result.recovery;return result.value;}},store=createPeopleFileStore(driver);
 const ctx=makeFeature({drafts:{'scimux-people-draft:n1':legacy},deps:{peopleContext:()=>({key:'s/o/c/g',writable:true,owner:true,attachments:true}),peopleFileStore:store}});
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('new synthetic caption');ctx.roots.prompt.firstListener('input')();await localReviewTick();assert.equal(ctx.storage.getItem('scimux-people-draft:n1'),legacy);const saved=recovery.filter(row=>row.kind==='draft');assert.equal(saved.length,1);assert.equal(saved[0].text,'new synthetic caption');assert.equal(ctx.feature.promptText(),'new synthetic caption');assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('a fresh People composer restores its durable draft without allocating on reload',async()=>{
 let recovery=[{key:'allocator',epoch:'a'.repeat(32),next:1}];const driver={read:async()=>[],readWrite:async fn=>fn([]).value,writePeople:async fn=>{const result=fn({files:[],recovery});recovery=result.recovery;return result.value;}},store=createPeopleFileStore(driver),context={key:localReviewKey,copyInstance:'1'.repeat(32),binding:'current',owner:false,writable:true,attachments:true};
 const make=()=>makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>context,peopleFileStore:store}}),first=make();let second;
 try{first.feature.bind();first.feature.onSelect();first.feature.setPromptText('durable synthetic');first.roots.prompt.firstListener('input')();await localReviewTick();const next=recovery.find(row=>row.key==='allocator').next;first.feature.destroy();second=make();second.feature.bind();second.feature.onSelect();await localReviewTick();assert.equal(second.feature.promptText(),'durable synthetic');assert.equal(recovery.find(row=>row.key==='allocator').next,next);assert.equal(recovery.filter(row=>row.kind==='head').length,1);assert.equal(second.apiCalls.length,0);}finally{first.feature.destroy();second?.feature.destroy();}
});

test('multiple durable People draft heads stay separate and reviewable',async()=>{
 let recovery=[{key:'allocator',epoch:'a'.repeat(32),next:1}];const driver={read:async()=>[],readWrite:async fn=>fn([]).value,writePeople:async fn=>{const result=fn({files:[],recovery});recovery=result.recovery;return result.value;}},store=createPeopleFileStore(driver),copy='1'.repeat(32),scope=`people\0${localReviewKey}\0binding:current\0copy:${copy}\0`;
 await store.savePeopleDraft({scope,text:'first synthetic draft'});await store.savePeopleDraft({scope,text:'second synthetic draft'});
 const ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>({key:localReviewKey,copyInstance:copy,binding:'current',owner:false,writable:true,attachments:true}),peopleFileStore:store}});
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(ctx.feature.promptText(),'');assert.match(ctx.roots.attstage.innerHTML,/first synthetic draft/);assert.match(ctx.roots.attstage.innerHTML,/second synthetic draft/);assert.equal(recovery.filter(row=>row.kind==='head').length,2);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('a preserved current-binding legacy People caption appears only in saved-draft review',async()=>{
 const copyInstance='1'.repeat(32),key=localReviewKey,binding=`${key}\0binding:current\0copy:${copyInstance}\0team/${key}`,draftKey='scimux-people-draft:team/'+key+':scope:'+encodeURIComponent(binding),legacy='legacy synthetic caption';
 const ctx=makeFeature({sel:'team/'+key,nodes:[],recovery:true,drafts:{[draftKey]:legacy},deps:{peopleContext:()=>({key,copyInstance,binding:'current',owner:false,writable:true,attachments:true})}});
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(ctx.feature.promptText(),'');assert.equal(ctx.storage.getItem(draftKey),legacy);assert.match(ctx.roots.recovery.innerHTML,/legacy synthetic caption/);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('confirmed text-only People send clears its exact durable draft version',async()=>{
 const context={key:localReviewKey,copyInstance:'1'.repeat(32),binding:'current',owner:false,writable:true,attachments:false},f=transactionalPeopleFixture({context}),ctx=f.ctx;
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('synthetic caption');ctx.roots.prompt.firstListener('input')();await f.waitDraft(1);await ctx.feature.sendPrompt();await localReviewTick();assert.equal(f.sends.length,1);assert.equal(f.sendEntries(),1);assert.equal(f.sendAttempts[0].requireDurableAdmission,true);assert.equal(f.sends[0].request.text,'synthetic caption');assert.equal(ctx.feature.promptText(),'');assert.equal(f.recovery().filter(row=>row.kind==='head').length,0);assert.equal(f.recovery().filter(row=>row.kind==='text-decision').length,1);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('explicit legacy retry retirement awaits its durable decision and labels the preserved raw value',async()=>{
 const record=retainedBrowserSnapshot({instance:'',binding:'current'}),events=[];let raw=record.value;
 const ctx=makeFeature({sel:'team/'+record.key,nodes:[],deps:{peopleContext:()=>({key:record.key,copyInstance:record.copyInstance,binding:'current',owner:false,writable:true}),pendingPeople:()=>({...record,value:raw}),localReviewApi:async(path,opts)=>{events.push(opts.method);return opts.method==='POST'?{state:'locally_discarded'}:path.includes('/pending/')?{state:'missing'}:{entries:[]};},fencePeoplePending:()=>{events.push('fence');return {state:'locally_retired',cancelled:0};},clearPeoplePending:async(snapshot,options)=>{events.push('record');assert.equal(snapshot.value,record.value);if(options)events.push(options.proof.state);return true;}}});
 const original=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-browser-retire'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-browser-retire(?:\s+[^"\s]+)*" data-row="(\d+)" data-snapshot="([^"]+)"/g)].map(m=>el('button',{dataset:{row:m[1],snapshot:m[2]}}))):original(s);
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();buttons[0].click();await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/legacy.*preserved/i);assert.deepEqual(events,['GET','GET','POST','fence','record','locally_discarded']);assert.equal(raw,record.value);assert.doesNotMatch(ctx.roots.attstage.innerHTML,/cleanup is unconfirmed/i);assert.equal(renderedClassCount(ctx.roots.attstage.innerHTML, "people-retry"),0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('a logically retired preserved legacy retry never renders a publish retry',async()=>{
 const record=retainedBrowserSnapshot({instance:'',binding:'current'}),ctx=makeFeature({sel:'team/'+record.key,nodes:[],deps:{peopleContext:()=>({key:record.key,copyInstance:record.copyInstance,binding:'current',owner:false,writable:true}),pendingPeople:()=>({...record,locallyRetired:true,decision:{state:'retired'}}),localReviewApi:async(path,opts)=>path.includes('/pending/')?{state:'missing'}:{entries:[]}}});
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(renderedClassCount(ctx.roots.attstage.innerHTML, "people-retry"),0);assert.match(ctx.roots.attstage.innerHTML,/legacy.*preserved/i);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('People review reloads durable retirement before offering a preserved legacy retry',async()=>{
 const record=retainedBrowserSnapshot({instance:'',binding:'current'});let loaded=false,posts=0;const store={list:async()=>[],listPeopleRecovery:async()=>({heads:[],pending:[],legacy:[]})};
 const ctx=makeFeature({sel:'team/'+record.key,nodes:[],deps:{peopleContext:()=>({key:record.key,copyInstance:record.copyInstance,binding:'current',owner:false,writable:true}),peopleFileStore:store,pendingPeople:()=>({...record,locallyRetired:loaded}),refreshPeopleRecovery:async()=>{loaded=true;return {heads:[],pending:[],legacy:[]};},retryPeople:async()=>{posts++;return {seq:7}},localReviewApi:async(path)=>path.includes('/pending/')?{state:'missing'}:{entries:[]}}});
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(loaded,true);assert.equal(renderedClassCount(ctx.roots.attstage.innerHTML, "people-retry"),0);assert.match(ctx.roots.attstage.innerHTML,/legacy.*preserved/i);assert.equal(posts,0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('People composer dispatches files only with durable publication and draft identity',async()=>{
 const key=localReviewKey,copyInstance='1'.repeat(32),binding='current',id='0123456789abcdef',revision='a'.repeat(64),events=[];let files=[],recovery=[{key:'allocator',epoch:'a'.repeat(32),next:1}],pending=null;
 const driver={read:async()=>files,readWrite:async fn=>{const result=fn(files);files=result.rows;return result.value;},writePeople:async fn=>{const result=fn({files,recovery});files=result.files;recovery=result.recovery;return result.value;}},store=createPeopleFileStore(driver),context={key,copyInstance,binding,owner:false,writable:true,attachments:true};
 const ctx=makeFeature({sel:'team/'+key,nodes:[],deps:{peopleContext:()=>context,peopleFileStore:store,pendingPeople:()=>pending,sendPeople:async(peopleKey,text,ids,instance,admission)=>{events.push('send');assert.ok(admission);const request={key:'2'.repeat(16),text,anchor:0,attachments:ids},admitted=await store.beginPeoplePublication({scope:admission.scope,peopleKey,head:admission.head,version:admission.version,copyInstance,binding,native:instance,request,files:admission.files,current:admission.current});pending={format:'transactional',scope:admission.scope,publication:admitted.publication,head:admission.head,version:admission.version,key:peopleKey,instance,copyInstance,binding,storageKey:'idb:'+admitted.publication,value:JSON.stringify({instance,copyInstance,request}),publicationKey:request.key,text,attachments:ids};admission.onPrepared(pending);return {seq:7,peoplePending:pending};},finishPeople:async(receipt,options)=>{events.push('finish');const result=await store.decidePeoplePublication({scope:receipt.peoplePending.scope,publication:receipt.peoplePending.publication,expected:receipt.peoplePending,proof:{state:'completed',seq:receipt.seq,text:receipt.peoplePending.text,stage_ids:receipt.peoplePending.attachments},current:options.current});pending=null;return result;},api:async(path,opts)=>{events.push(opts.method);return null;}},uploadHandler:async()=>({ok:true,json:async()=>({attachments:[{id,revision}]})})});
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('caption');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:2}]);await localReviewTick();await ctx.feature.sendPrompt();await localReviewTick();assert.equal(events.filter(event=>event==='send').length,1);assert.ok(events.includes('finish'));assert.equal(ctx.feature.promptText(),'');assert.equal(recovery.find(row=>row.kind==='decision').state,'completed');assert.equal(ctx.storage.keys().some(key=>key.startsWith('scimux-people-pending-draft:')),false);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('explicit transactional People retry clears only its confirmed durable caption',async()=>{
 const key=localReviewKey,copyInstance='1'.repeat(32),binding='current',scope=`people\0${key}\0binding:${binding}\0copy:${copyInstance}\0`,id='0123456789abcdef',revision='a'.repeat(64),events=[];let files=[],recovery=[{key:'allocator',epoch:'a'.repeat(32),next:1}];
 const driver={read:async()=>files,readWrite:async fn=>{const result=fn(files);files=result.rows;return result.value;},writePeople:async fn=>{const result=fn({files,recovery});files=result.files;recovery=result.recovery;return result.value;}},store=createPeopleFileStore(driver),file={name:'saved.txt',type:'text/plain',size:2},token=await store.save(scope,id,file);await store.setStageRevision(scope,id,revision,token);const draft=await store.savePeopleDraft({scope,text:'caption'}),request={key:'2'.repeat(16),text:'caption',anchor:0,attachments:[id]},admitted=await store.beginPeoplePublication({scope,peopleKey:key,head:draft.head,version:draft.version,copyInstance,binding,native:'',request,files:[{id,fileToken:token,stageRevision:revision}]});let pending={format:'transactional',scope,publication:admitted.publication,head:draft.head,version:draft.version,key,instance:'',copyInstance,binding,storageKey:'idb:'+admitted.publication,value:JSON.stringify({instance:'',copyInstance,request}),publicationKey:request.key,text:'caption',attachments:[id]};
 const ctx=makeFeature({sel:'team/'+key,nodes:[],deps:{peopleContext:()=>({key,copyInstance,binding,owner:false,writable:true,attachments:true}),peopleFileStore:store,pendingPeople:()=>pending,retryPeople:async()=>{events.push('retry');return {seq:7,peoplePending:pending};},finishPeople:async(receipt,options)=>{events.push('finish');const result=await store.decidePeoplePublication({scope,publication:receipt.peoplePending.publication,expected:receipt.peoplePending,proof:{state:'completed',seq:receipt.seq,text:receipt.peoplePending.text,stage_ids:receipt.peoplePending.attachments},current:options.current});pending=null;return result;},api:async(path,opts)=>{events.push(opts.method);return null;}}});
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(ctx.feature.promptText(),'caption');assert.equal(ctx.roots.attstage._retryButtons.length,1);ctx.roots.attstage._retryButtons[0].click();await localReviewTick();assert.equal(events.filter(value=>value==='retry').length,1);assert.ok(events.includes('finish'));assert.equal(ctx.feature.promptText(),'');assert.equal(recovery.find(row=>row.kind==='decision').state,'completed');assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('explicit confirmed legacy People retry preserves its raw value and records actual completion',async()=>{
 const record=retainedBrowserSnapshot({instance:'',binding:'current'}),scope=`people\0${record.key}\0binding:current\0copy:${record.copyInstance}\0`,id=record.attachments[0],fileToken='11111111-1111-1111-1111-111111111111',revision='a'.repeat(64);let recovery=[{key:'allocator',epoch:'a'.repeat(32),next:1}],events=[],pending={...record,format:'legacy'};
 const savedFile={key:scope+'\0'+id,scope,id,file:{name:'saved.txt',type:'text/plain',size:2},fileToken,stageRevision:revision},driver={read:async()=>[savedFile],readWrite:async fn=>fn([savedFile]).value,writePeople:async fn=>{const next=fn({files:[savedFile],recovery});recovery=next.recovery;return next.value;}},store=createPeopleFileStore(driver);
 const ctx=makeFeature({sel:'team/'+record.key,nodes:[],deps:{peopleContext:()=>({key:record.key,copyInstance:record.copyInstance,binding:'current',owner:false,writable:true,attachments:true}),peopleFileStore:store,pendingPeople:()=>pending,retryPeople:async()=>{events.push('retry');return {seq:7,peoplePending:pending};},finishPeople:async(receipt,options)=>{events.push('finish');await store.recordLegacyPeopleDecision({scope:options.scope,snapshot:pending,proof:{state:'completed',seq:receipt.seq,text:pending.text,stage_ids:pending.attachments},current:options.current});pending={...pending,locallyRetired:true,decision:{state:'completed',seq:7}};return {state:'completed',seq:7,filesConfirmed:false,draftCleared:false};}}});
 try{ctx.storage.setItem(record.storageKey,record.value);ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();ctx.roots.attstage._retryButtons[0].click();await localReviewTick();assert.deepEqual(events,['retry','finish']);assert.equal(ctx.storage.getItem(record.storageKey),record.value);assert.equal(recovery.find(row=>row.kind==='legacy-decision').seq,7);assert.match(ctx.roots.attstage.innerHTML,/legacy.*preserved/i);assert.equal((await store.list(scope))[0].file,savedFile.file);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('transactional People pending review retires only its captured publication',async()=>{
 const key=localReviewKey,copyInstance='1'.repeat(32),binding='current',scope=`people\0${key}\0binding:${binding}\0copy:${copyInstance}\0`,id='0123456789abcdef',revision='a'.repeat(64),events=[];let files=[],recovery=[{key:'allocator',epoch:'a'.repeat(32),next:1}];
 const driver={read:async()=>files,readWrite:async fn=>{const result=fn(files);files=result.rows;return result.value;},writePeople:async fn=>{const result=fn({files,recovery});files=result.files;recovery=result.recovery;return result.value;}},store=createPeopleFileStore(driver),file={name:'saved.txt',type:'text/plain',size:2},token=await store.save(scope,id,file);await store.setStageRevision(scope,id,revision,token);const draft=await store.savePeopleDraft({scope,text:'caption'}),request={key:'2'.repeat(16),text:'caption',anchor:0,attachments:[id]},admitted=await store.beginPeoplePublication({scope,peopleKey:key,head:draft.head,version:draft.version,copyInstance,binding,native:'',request,files:[{id,fileToken:token,stageRevision:revision}]});let pending={format:'transactional',scope,publication:admitted.publication,head:draft.head,version:draft.version,key,instance:'',copyInstance,binding,storageKey:'idb:'+admitted.publication,value:JSON.stringify({instance:'',copyInstance,request}),publicationKey:request.key,text:'caption',attachments:[id]};
 const ctx=makeFeature({sel:'team/'+key,nodes:[],deps:{peopleContext:()=>({key,copyInstance,binding,owner:false,writable:true,attachments:true}),peopleFileStore:store,pendingPeople:()=>pending,localReviewApi:async(path,opts)=>{events.push(opts.method);return opts.method==='POST'?{state:'locally_discarded'}:path.includes('/pending/')?{state:'missing'}:{entries:[]};},fencePeoplePending:()=>{events.push('fence');return {state:'locally_retired',cancelled:0};},clearPeoplePending:async(snapshot,options)=>{events.push('decision');const result=await store.decidePeoplePublication({scope,publication:snapshot.publication,expected:snapshot,proof:options.proof,current:options.current});pending=null;return result;}}});
 const original=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-browser-retire'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-browser-retire(?:\s+[^"\s]+)*" data-row="(\d+)" data-snapshot="([^"]+)"/g)].map(m=>el('button',{dataset:{row:m[1],snapshot:m[2]}}))):original(s);
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.deepEqual(events,['GET','GET']);assert.equal(buttons.length,1);buttons[0].click();await localReviewTick();assert.deepEqual(events,['GET','GET','POST','fence','decision']);assert.equal(pending,null);assert.equal(recovery.find(row=>row.publication===admitted.publication).state,'retired');assert.equal(recovery.find(row=>row.kind==='head').version,draft.version);assert.equal((await store.list(scope))[0].file,file);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('other transactional People publications remain in the common retained review',async()=>{
 const key=localReviewKey,copyInstance='1'.repeat(32),binding='current',scope=`people\0${key}\0binding:${binding}\0copy:${copyInstance}\0`,id='0123456789abcdef',records=[];for(const [n,publicationKey] of [['1','2'],['2','3']]){const request={key:publicationKey.repeat(16),text:'caption '+n,anchor:0,attachments:[id]};const recordBinding=n==='1'?binding:'prior',recordScope=n==='1'?scope:`people\0${key}\0binding:prior\0copy:${copyInstance}\0`;records.push({format:'transactional',scope:recordScope,publication:'a'.repeat(32)+':'+n,head:'a'.repeat(32)+':4',version:'a'.repeat(32)+':4',key,instance:'',copyInstance,binding:recordBinding,storageKey:'idb:'+'a'.repeat(32)+':'+n,value:JSON.stringify({instance:'',copyInstance,request}),publicationKey:request.key,text:request.text,attachments:[id]});}
 let loaded=false;const calls=[],ctx=makeFeature({sel:'team/'+key,nodes:[],deps:{peopleContext:()=>({key,copyInstance,binding,owner:false,writable:true,attachments:true}),pendingPeople:()=>loaded?records[0]:null,readRetainedPeoplePending:()=>({entries:loaded?records:[],unavailable:false}),refreshPeopleRecovery:async()=>{loaded=true;return {heads:[],pending:[],legacy:[]};},localReviewApi:async(path,opts)=>{calls.push({path,opts});return path.includes('/pending/')?{state:'missing'}:{entries:[]};}}});
 const original=ctx.roots.attstage.querySelectorAll;let details=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-local-snapshot'?(details=[...ctx.roots.attstage.innerHTML.matchAll(/class="people-local-snapshot" data-row="(\d+)" data-snapshot="(\d+)"/g)].map(m=>el('details',{dataset:{row:m[1],snapshot:m[2]}}))):original(s);
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(details.length,1);assert.match(ctx.roots.attstage.innerHTML,/caption 2/);assert.ok(calls.every(call=>call.opts.method==='GET'));assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('a stale People context cannot store a different selected chat as its draft',async()=>{
 let recovery=[{key:'allocator',epoch:'a'.repeat(32),next:1}],sends=0;const driver={read:async()=>[],readWrite:async fn=>fn([]).value,writePeople:async fn=>{const result=fn({files:[],recovery});recovery=result.recovery;return result.value;}},store=createPeopleFileStore(driver),ctx=makeFeature({sel:'team/s/o/other/g',nodes:[],deps:{peopleContext:()=>({key:'s/o/original/g',owner:false,writable:true}),peopleFileStore:store,sendPeople:async()=>{sends++;}}});
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('different chat');ctx.roots.prompt.firstListener('input')();await localReviewTick();assert.equal(ctx.feature.promptText(),'different chat');assert.equal(recovery.filter(row=>row.kind==='draft').length,0);await ctx.feature.sendPrompt();assert.equal(sends,0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('late text-only People completion preserves newer same-text durable editing',async()=>{
 let release;const context={key:localReviewKey,copyInstance:'1'.repeat(32),binding:'current',owner:false,writable:true,attachments:false},scope=`people\0${localReviewKey}\0binding:current\0copy:${context.copyInstance}\0`,f=transactionalPeopleFixture({context,respond:()=>new Promise(resolve=>{release=resolve;})}),ctx=f.ctx;
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('same');ctx.roots.prompt.firstListener('input')();await f.waitDraft(1);const flight=ctx.feature.sendPrompt();await f.waitSend(1);assert.equal(typeof release,'function');ctx.feature.setPromptText('same');ctx.roots.prompt.firstListener('input')();await f.waitDraft(2);release({seq:7});await flight;await localReviewTick();assert.equal(ctx.feature.promptText(),'same');const state=await f.actual.listPeopleRecovery(scope);assert.equal(state.heads.length,1);assert.equal(state.heads[0].text,'same');assert.equal(f.sends.length,1);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('a legacy text-only People retry remains visible for review without dispatch',async()=>{
 const key=localReviewKey,slot='scimux-comment-retry:'+key,value=JSON.stringify({text:'legacy text retry',anchor:0,key:'2'.repeat(16)}),ctx=makeFeature({sel:'team/'+key,nodes:[],recovery:true,drafts:{[slot]:value},deps:{peopleContext:()=>({key,copyInstance:'1'.repeat(32),binding:'current',owner:false,writable:true,attachments:false})}});
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(ctx.storage.getItem(slot),value);assert.match(ctx.roots.recovery.innerHTML,/legacy text retry/);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('text-only People composer supplies its durable draft to publication admission',async()=>{
 let recovery=[{key:'allocator',epoch:'a'.repeat(32),next:1}],calls=0;const driver={read:async()=>[],readWrite:async fn=>fn([]).value,writePeople:async fn=>{const next=fn({files:[],recovery});recovery=next.recovery;return next.value;}},store=createPeopleFileStore(driver),key=localReviewKey,copyInstance='1'.repeat(32),binding='current',context={key,copyInstance,binding,owner:false,writable:true,attachments:false};
 const ctx=makeFeature({sel:'team/'+key,nodes:[],deps:{peopleContext:()=>context,peopleFileStore:store,sendPeople:async(peopleKey,text,attachments,instance,admission)=>{calls++;assert.ok(admission);const input={scope:admission.scope,peopleKey,copyInstance,binding,native:instance,head:admission.head,version:admission.version,request:{key:'2'.repeat(16),text,anchor:0}},saved=await store.beginPeopleTextPublication(input),decision=await store.finishPeopleTextPublication({scope:input.scope,publication:saved.publication,expected:input,seq:7});return {seq:7,peopleTextDecision:decision};}}});
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('caption');ctx.roots.prompt.firstListener('input')();await localReviewTick();await ctx.feature.sendPrompt();await localReviewTick();assert.equal(calls,1);assert.equal(ctx.feature.promptText(),'');assert.equal(recovery.find(row=>row.kind==='text-decision').state,'completed');assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('an uncertain text-only People publication is reviewable without implicit dispatch',async()=>{
 const key=localReviewKey,ctx=makeFeature({sel:'team/'+key,nodes:[],deps:{peopleContext:()=>({key,copyInstance:'1'.repeat(32),binding:'current',owner:false,writable:true,attachments:false}),pendingPeopleText:()=>({request:{text:'uncertain synthetic',key:'2'.repeat(16)}}),sendPeople:async()=>assert.fail('must not send')}});
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/uncertain synthetic/);assert.match(ctx.roots.attstage.innerHTML,/unconfirmed/i);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('durable unconfirmed People text offers explicit local retirement without sending',async()=>{
 const context={key:localReviewKey,copyInstance:'1'.repeat(32),binding:'current',owner:false,writable:true,attachments:false},scope=`people\0${localReviewKey}\0binding:current\0copy:${context.copyInstance}\0`;let f;f=transactionalPeopleFixture({context,depsExtra:{pendingPeopleText:()=>f.recovery().find(row=>row.kind==='text-publication'&&row.state==='pending')}});const ctx=f.ctx;
 const draft=await f.actual.savePeopleDraft({scope,text:'unconfirmed text'}),request={key:'4'.repeat(16),text:'unconfirmed text',anchor:0};
 await f.actual.beginPeopleTextPublication({scope,peopleKey:context.key,copyInstance:context.copyInstance,binding:context.binding,native:'',...draft,request});
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/unconfirmed text/);assert.match(ctx.roots.attstage.innerHTML,/<button[^>]*>[^<]*(?:discard|retire)[^<]*<\/button>/i,'unconfirmed text needs an explicit local retirement action');assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('ended owner Agent view loads prior-binding transactional People recovery for review',async()=>{
 const key=localReviewKey,copyInstance='1'.repeat(32),scope=`people\0${key}\0binding:old\0copy:${copyInstance}\0n1\0native`,request={key:'2'.repeat(16),text:'retained synthetic',anchor:0,attachments:['3'.repeat(16)]},record={format:'transactional',scope,publication:'a'.repeat(32)+':1',head:'a'.repeat(32)+':2',version:'a'.repeat(32)+':2',key,instance:'native',copyInstance,binding:'old',storageKey:'idb:'+'a'.repeat(32)+':1',value:JSON.stringify({instance:'native',copyInstance,request}),publicationKey:request.key,text:request.text,attachments:request.attachments};let loaded=false,posts=0;
 const ctx=makeFeature({nodes:[{id:'n1',instance_id:'native',ended_at:'ended'}],deps:{peopleContext:()=>null,localReviewContexts:()=>[{key,copyInstance,binding:'current',owner:true}],readRetainedPeoplePending:()=>({entries:loaded?[record]:[],unavailable:false}),refreshRetainedPeopleRecovery:async()=>{loaded=true;},localReviewApi:async(path,opts)=>{if(opts.method!=='GET')posts++;return {entries:[]};}}});
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(ctx.feature.destination(),'agent');assert.equal(loaded,true);assert.match(ctx.roots.attstage.innerHTML,/retained synthetic/);assert.equal(posts,0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('unproven completed People files remain reviewable with the actual sequence after reload',async()=>{
 const key=localReviewKey,copyInstance='1'.repeat(32),binding='current',scope=`people\0${key}\0binding:${binding}\0copy:${copyInstance}\0`,id='0123456789abcdef',revision='a'.repeat(64);let files=[],recovery=[{key:'allocator',epoch:'a'.repeat(32),next:1}];const driver={read:async()=>files,readWrite:async fn=>{const next=fn(files);files=next.rows;return next.value;},writePeople:async fn=>{const next=fn({files,recovery});files=next.files;recovery=next.recovery;return next.value;}},store=createPeopleFileStore(driver),token=await store.save(scope,id,{name:'old.txt',type:'text/plain',size:1});await store.setStageRevision(scope,id,revision,token);const draft=await store.savePeopleDraft({scope,text:'caption'}),request={key:'2'.repeat(16),text:'caption',anchor:0,attachments:[id]},saved=await store.beginPeoplePublication({scope,peopleKey:key,head:draft.head,version:draft.version,copyInstance,binding,native:'',request,files:[{id,fileToken:token,stageRevision:revision}]});await store.remove(scope,id);const replacement=await store.save(scope,id,{name:'new.txt',type:'text/plain',size:2});await store.setStageRevision(scope,id,revision,replacement);const expected={key,copyInstance,binding,instance:'',head:draft.head,version:draft.version,publicationKey:request.key,text:request.text,attachments:request.attachments,value:JSON.stringify({instance:'',copyInstance,request})};await store.decidePeoplePublication({scope,publication:saved.publication,expected,proof:{state:'completed',seq:7,text:'caption',stage_ids:[id]}});
 const ctx=makeFeature({sel:'team/'+key,nodes:[],deps:{peopleContext:()=>({key,copyInstance,binding,owner:false,writable:true,attachments:true}),peopleFileStore:store}});
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/sequence 7/i);assert.match(ctx.roots.attstage.innerHTML,/caption/);assert.match(ctx.roots.attstage.innerHTML,/new.txt/);assert.equal((await store.list(scope))[0].published,undefined);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('explicit completed People recovery clears the confirmed durable caption after its pending row closes',async()=>{
 const key=localReviewKey,copyInstance='1'.repeat(32),binding='current',scope=`people\0${key}\0binding:${binding}\0copy:${copyInstance}\0`,id='0123456789abcdef',revision='a'.repeat(64),events=[];let files=[],recovery=[{key:'allocator',epoch:'a'.repeat(32),next:1}];const driver={read:async()=>files,readWrite:async fn=>{const next=fn(files);files=next.rows;return next.value;},writePeople:async fn=>{const next=fn({files,recovery});files=next.files;recovery=next.recovery;return next.value;}},store=createPeopleFileStore(driver),file={name:'saved.txt',type:'text/plain',size:2},token=await store.save(scope,id,file);await store.setStageRevision(scope,id,revision,token);const draft=await store.savePeopleDraft({scope,text:'caption'}),request={key:'2'.repeat(16),text:'caption',anchor:0,attachments:[id]},admitted=await store.beginPeoplePublication({scope,peopleKey:key,head:draft.head,version:draft.version,copyInstance,binding,native:'',request,files:[{id,fileToken:token,stageRevision:revision}]});let pending={format:'transactional',scope,publication:admitted.publication,head:draft.head,version:draft.version,key,instance:'',copyInstance,binding,storageKey:'idb:'+admitted.publication,value:JSON.stringify({instance:'',copyInstance,request}),publicationKey:request.key,text:'caption',attachments:[id]};
 const ctx=makeFeature({sel:'team/'+key,nodes:[],deps:{peopleContext:()=>({key,copyInstance,binding,owner:false,writable:false,attachments:false}),peopleFileStore:store,pendingPeople:()=>pending,localReviewApi:async(path,opts)=>{events.push(opts.method);return path.includes('/pending/')?{state:'completed',revision:'e'.repeat(64),text:'caption',stage_ids:[id],seq:7}:{entries:[]};},fencePeoplePending:()=>({state:'locally_retired',cancelled:0}),clearPeoplePending:async(snapshot,options)=>{const result=await store.decidePeoplePublication({scope,publication:snapshot.publication,expected:snapshot,proof:options.proof,current:options.current});pending=null;return result;}}});
 const original=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-browser-retire'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-browser-retire(?:\s+[^"\s]+)*" data-row="(\d+)" data-snapshot="([^"]+)"/g)].map(m=>el('button',{dataset:{row:m[1],snapshot:m[2]}}))):original(s);
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(ctx.feature.promptText(),'caption');assert.equal(buttons.length,1);buttons[0].click();await localReviewTick();assert.equal(pending,null);assert.equal(ctx.feature.promptText(),'');assert.equal((await store.list(scope))[0].file,file);assert.equal((await store.list(scope))[0].published,7);assert.deepEqual(events,['GET','GET']);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('completed People recovery preserves a newer same-text edit while its decision is held',async()=>{
 const key=localReviewKey,copyInstance='1'.repeat(32),binding='current',scope=`people\0${key}\0binding:${binding}\0copy:${copyInstance}\0`,id='0123456789abcdef',revision='a'.repeat(64),events=[];let release,files=[],recovery=[{key:'allocator',epoch:'a'.repeat(32),next:1}];const driver={read:async()=>files,readWrite:async fn=>{const next=fn(files);files=next.rows;return next.value;},writePeople:async fn=>{const next=fn({files,recovery});files=next.files;recovery=next.recovery;return next.value;}},store=createPeopleFileStore(driver),file={name:'saved.txt',type:'text/plain',size:2},token=await store.save(scope,id,file);await store.setStageRevision(scope,id,revision,token);const draft=await store.savePeopleDraft({scope,text:'caption'}),request={key:'2'.repeat(16),text:'caption',anchor:0,attachments:[id]},admitted=await store.beginPeoplePublication({scope,peopleKey:key,head:draft.head,version:draft.version,copyInstance,binding,native:'',request,files:[{id,fileToken:token,stageRevision:revision}]});let pending={format:'transactional',scope,publication:admitted.publication,head:draft.head,version:draft.version,key,instance:'',copyInstance,binding,storageKey:'idb:'+admitted.publication,value:JSON.stringify({instance:'',copyInstance,request}),publicationKey:request.key,text:'caption',attachments:[id]};
 const ctx=makeFeature({sel:'team/'+key,nodes:[],deps:{peopleContext:()=>({key,copyInstance,binding,owner:false,writable:false,attachments:false}),peopleFileStore:store,pendingPeople:()=>pending,localReviewApi:async(path,opts)=>{events.push(opts.method);return path.includes('/pending/')?{state:'completed',revision:'e'.repeat(64),text:'caption',stage_ids:[id],seq:7}:{entries:[]};},fencePeoplePending:()=>({state:'locally_retired',cancelled:0}),clearPeoplePending:async(snapshot,options)=>{await new Promise(resolve=>{release=resolve;});const result=await store.decidePeoplePublication({scope,publication:snapshot.publication,expected:snapshot,proof:options.proof,current:options.current});pending=null;return result;}}});
 const original=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-browser-retire'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-browser-retire(?:\s+[^"\s]+)*" data-row="(\d+)" data-snapshot="([^"]+)"/g)].map(m=>el('button',{dataset:{row:m[1],snapshot:m[2]}}))):original(s);
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(ctx.feature.promptText(),'caption');assert.equal(buttons.length,1);buttons[0].click();await localReviewTick();assert.equal(typeof release,'function');ctx.feature.setPromptText('caption');ctx.roots.prompt.firstListener('input')();await localReviewTick();release();await localReviewTick();assert.equal(pending,null);assert.equal(ctx.feature.promptText(),'caption');assert.equal((await store.listPeopleRecovery(scope)).heads[0].text,'caption');assert.equal((await store.list(scope))[0].file,file);assert.equal((await store.list(scope))[0].published,7);assert.deepEqual(events,['GET','GET']);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('a confirmed text-only People message preserves its captured draft if recovery storage became unavailable',async()=>{
 let recovery=[{key:'allocator',epoch:'a'.repeat(32),next:1}],calls=0,captured;const driver={read:async()=>[],readWrite:async fn=>fn([]).value,writePeople:async fn=>{const next=fn({files:[],recovery});recovery=next.recovery;return next.value;}},store=createPeopleFileStore(driver),key=localReviewKey,copyInstance='1'.repeat(32),binding='current',context={key,copyInstance,binding,owner:false,writable:true,attachments:false};
 const ctx=makeFeature({sel:'team/'+key,nodes:[],deps:{peopleContext:()=>context,peopleFileStore:store,sendPeople:async(_key,_text,_attachments,_instance,admission)=>{calls++;assert.ok(admission);captured=admission;return {seq:7,peopleTextStorageUnavailable:true};}}});
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('caption');ctx.roots.prompt.firstListener('input')();await localReviewTick();const original=recovery.find(row=>row.kind==='head');await ctx.feature.sendPrompt();await localReviewTick();assert.equal(calls,1);assert.equal(ctx.feature.promptText(),'caption');const heads=recovery.filter(row=>row.kind==='head');assert.equal(heads.length,2);assert.notEqual(heads[0].head,heads[1].head);assert.notEqual(heads[0].version,heads[1].version);assert.ok(heads.some(row=>row.head===original.head&&row.version===original.version));assert.ok(heads.some(row=>row.head===captured.head&&row.version===captured.version));assert.ok(heads.every(row=>recovery.find(draft=>draft.key==='draft:'+row.version)?.text==='caption'));assert.equal(recovery.find(row=>row.kind==='draft').text,'caption');assert.equal(recovery.filter(row=>row.kind==='text-decision').length,0);assert.equal(ctx.storage.keys().filter(key=>key.startsWith('scimux-people-pending-draft:')).length,0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('confirmed text-only People send without durable storage clears only its unchanged in-page input',async()=>{
 const key=localReviewKey,context={key,copyInstance:'1'.repeat(32),binding:'current',owner:false,writable:true,attachments:false};let calls=0;
 const ctx=makeFeature({sel:'team/'+key,nodes:[],deps:{peopleContext:()=>context,peopleFileStore:null,sendPeople:async(received,text)=>{calls++;assert.equal(received,key);assert.equal(text,'synthetic message');return {seq:7};}}});
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('synthetic message');ctx.roots.prompt.firstListener('input')();await ctx.feature.sendPrompt();assert.equal(calls,1);assert.equal(ctx.apiCalls.length,0);assert.equal(ctx.feature.promptText(),'');}finally{ctx.feature.destroy();}
});

function noHeadPeopleFixture(){
 const key=localReviewKey;let context={key,copyInstance:'1'.repeat(32),binding:'current',owner:true,writable:true,attachments:false},release,reject,startedResolve,calls=0;
 const started=new Promise(resolve=>{startedResolve=resolve;}),ctx=makeFeature({nodes:[{id:'n1',instance_id:'one'},{id:'n2',instance_id:'two'}],deps:{peopleContext:()=>context,peopleFileStore:null,sendPeople:(_key,_text,_attachments,_native,admission)=>{calls++;assert.equal(admission,null);startedResolve();return new Promise((resolve,fail)=>{release=resolve;reject=fail;});}}});
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('synthetic message');ctx.roots.prompt.firstListener('input')();
 return {ctx,started,calls:()=>calls,resolve:value=>release(value),reject:error=>reject(error),setContext:value=>{context={...context,...value};}};
}

for(const newer of ['synthetic message','newer text']) test(`confirmed no-head People send preserves newer editing: ${newer}`,async()=>{
 const f=noHeadPeopleFixture(),ctx=f.ctx;try{const work=ctx.feature.sendPrompt();await f.started;ctx.feature.setPromptText(newer);ctx.roots.prompt.firstListener('input')();f.resolve({seq:7});await work;assert.equal(f.calls(),1);assert.equal(ctx.feature.promptText(),newer);ctx.feature.setDestination('agent');ctx.feature.setDestination('people');assert.equal(ctx.feature.promptText(),newer);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

for(const result of [null,{}, {seq:0}, {seq:'7'}, {seq:-1}]) test(`unconfirmed no-head People result preserves input: ${JSON.stringify(result)}`,async()=>{
 const f=noHeadPeopleFixture(),ctx=f.ctx;try{const work=ctx.feature.sendPrompt();await f.started;f.resolve(result);await work;assert.equal(f.calls(),1);assert.equal(ctx.feature.promptText(),'synthetic message');assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('failed no-head People send preserves its in-page input',async()=>{
 const f=noHeadPeopleFixture(),ctx=f.ctx;try{const work=ctx.feature.sendPrompt();await f.started;f.reject(Error('synthetic offline'));await work;assert.equal(f.calls(),1);assert.equal(ctx.feature.promptText(),'synthetic message');assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('confirmed no-head People send clears its cached input after switching to Agent',async()=>{
 const f=noHeadPeopleFixture(),ctx=f.ctx;try{const work=ctx.feature.sendPrompt();await f.started;ctx.feature.setDestination('agent');ctx.feature.setPromptText('agent draft');ctx.roots.prompt.firstListener('input')();f.resolve({seq:7});await work;assert.equal(ctx.feature.destination(),'agent');assert.equal(ctx.feature.promptText(),'agent draft');ctx.feature.setDestination('people');assert.equal(ctx.feature.promptText(),'');assert.equal(f.calls(),1);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('confirmed no-head People send clears only its original cached input after navigation',async()=>{
 const f=noHeadPeopleFixture(),ctx=f.ctx;try{const work=ctx.feature.sendPrompt();await f.started;ctx.setSel('n2');ctx.feature.onSelect();ctx.feature.setPromptText('other chat text');ctx.roots.prompt.firstListener('input')();f.resolve({seq:7});await work;assert.equal(ctx.feature.promptText(),'other chat text');ctx.setSel('n1');ctx.feature.onSelect();assert.equal(ctx.feature.promptText(),'');assert.equal(f.calls(),1);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

for(const changed of [{binding:'new'}, {copyInstance:'2'.repeat(32)}]) test(`confirmed no-head People send preserves a changed scope: ${Object.keys(changed)[0]}`,async()=>{
 const f=noHeadPeopleFixture(),ctx=f.ctx;try{const work=ctx.feature.sendPrompt();await f.started;f.setContext(changed);ctx.feature.contextChanged();ctx.feature.setPromptText('new copy text');ctx.roots.prompt.firstListener('input')();f.resolve({seq:7});await work;assert.equal(ctx.feature.promptText(),'new copy text');assert.equal(f.calls(),1);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('confirmed no-head People send cannot clear a recreated composer lifetime',async()=>{
 const f=noHeadPeopleFixture(),ctx=f.ctx;try{const work=ctx.feature.sendPrompt();await f.started;ctx.feature.destroy();ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('new lifetime');ctx.roots.prompt.firstListener('input')();f.resolve({seq:7});await work;assert.equal(ctx.feature.promptText(),'new lifetime');assert.equal(f.calls(),1);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('People draft remains visible when an older file-only driver cannot write recovery',async()=>{
 let writes=0;const driver={read:async()=>[],readWrite:async fn=>{writes++;return fn([]).value;}},store=createPeopleFileStore(driver),context={key:localReviewKey,copyInstance:'1'.repeat(32),binding:'current',owner:false,writable:true,attachments:false},ctx=makeFeature({sel:'team/'+localReviewKey,nodes:[],deps:{peopleContext:()=>context,peopleFileStore:store}});
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('synthetic draft');ctx.roots.prompt.firstListener('input')();await localReviewTick();assert.equal(ctx.feature.promptText(),'synthetic draft');assert.match(ctx.roots.attstage.innerHTML,/draft.*(unavailable|saved)/i);assert.equal(writes,0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('rebound People file review does not infer delivery for a replacement file identity',async()=>{
 let release,signal;const reached=new Promise(resolve=>{signal=resolve;}),f=transactionalPeopleFixture({afterDecision:()=>new Promise(resolve=>{release=resolve;signal();})}),ctx=f.ctx;
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('original caption');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await f.waitReady(1);const flight=ctx.feature.sendPrompt();await reached;const old=f.files()[0];assert.equal(old.published,7);await f.actual.remove(old.scope,old.id);const replacement={name:'replacement.txt',type:'text/plain',size:2},token=await f.actual.save(old.scope,old.id,replacement);await f.actual.setStageRevision(old.scope,old.id,'b'.repeat(64),token);assert.notEqual(token,old.fileToken);ctx.feature.destroy();ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('new editing');ctx.roots.prompt.firstListener('input')();release();await flight;await localReviewTick();const rows=await f.actual.list(old.scope);assert.equal(rows.length,1);assert.equal(rows[0].file,replacement);assert.equal(rows[0].fileToken,token);assert.equal(rows[0].stageRevision,'b'.repeat(64));assert.equal(rows[0].published,undefined);assert.doesNotMatch(ctx.roots.attstage.innerHTML,/Sent; remove this saved file/);assert.equal(ctx.feature.promptText(),'new editing');assert.equal(f.sendEntries(),1);assert.equal(f.sends.length,1);await ctx.feature.sendPrompt();assert.equal(f.sendEntries(),2);assert.equal(f.sends.length,1);assert.match(ctx.alerts.join(' '),/file identity conflict/i);assert.equal((await f.actual.list(old.scope))[0].fileToken,token);assert.equal(ctx.apiCalls.filter(call=>call.opts?.method==='DELETE').length,0);}finally{if(release)release();ctx.feature.destroy();}
});

test('rebound People file review keeps an unpublished file eligible without implicit dispatch',async()=>{
 const f=transactionalPeopleFixture(),ctx=f.ctx;
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await f.waitReady(1);const original=f.files()[0];ctx.feature.destroy();ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();const rows=await f.actual.list(original.scope);assert.equal(rows.length,1);assert.equal(rows[0].fileToken,original.fileToken);assert.equal(rows[0].stageRevision,original.stageRevision);assert.equal(rows[0].published,undefined);assert.match(ctx.roots.attstage.innerHTML,/saved.txt/);assert.doesNotMatch(ctx.roots.attstage.innerHTML,/Sent; remove this saved file/);assert.equal(f.sendEntries(),0);assert.equal(f.sends.length,0);assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);}finally{ctx.feature.destroy();}
});

test('a late People file recovery read does not render into another copy after rebind',async()=>{
 const f=transactionalPeopleFixture(),ctx=f.ctx,scope=`people\0${f.context.key}\0binding:${f.context.binding}\0copy:${f.context.copyInstance}\0`,file={name:'old-scope.txt',type:'text/plain',size:1};await f.actual.save(scope,'0123456789abcdef',file);let release,entered,reads=0;const held=new Promise(resolve=>{entered=resolve;}),actualList=f.store.list;
 f.store.list=async identity=>{const rows=await actualList(identity);if(++reads===1){entered();await new Promise(resolve=>{release=resolve;});}return rows;};
 try{ctx.feature.bind();ctx.feature.onSelect();await held;ctx.feature.destroy();f.context.copyInstance='2'.repeat(32);ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('new copy input');ctx.roots.prompt.firstListener('input')();release();await localReviewTick();assert.equal(ctx.feature.promptText(),'new copy input');assert.doesNotMatch(ctx.roots.attstage.innerHTML,/old-scope.txt/);assert.deepEqual(ctx.alerts,[]);assert.equal(f.sendEntries(),0);assert.equal(f.sends.length,0);const rows=await f.actual.list(scope);assert.equal(rows.length,1);assert.equal(rows[0].file,file);}finally{if(release)release();ctx.feature.destroy();}
});

test('a failed old People file read cannot suppress recovery after rebind',async()=>{
 const f=transactionalPeopleFixture(),ctx=f.ctx,scope=`people\0${f.context.key}\0binding:${f.context.binding}\0copy:${f.context.copyInstance}\0`,file={name:'recoverable.txt',type:'text/plain',size:1};await f.actual.save(scope,'0123456789abcdef',file);let reject,entered,reads=0;const started=new Promise(resolve=>{entered=resolve;}),actualList=f.store.list;
 f.store.list=async identity=>{if(++reads===1){entered();return new Promise((_,fail)=>{reject=fail;});}return actualList(identity);};
 try{ctx.feature.bind();ctx.feature.onSelect();await started;ctx.feature.destroy();ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('new input');ctx.roots.prompt.firstListener('input')();reject(Error('synthetic old read failure'));await localReviewTick();assert.equal(ctx.feature.promptText(),'new input');assert.deepEqual(ctx.alerts,[]);assert.ok(reads>=2,'new lifetime must be able to recover its file');assert.match(ctx.roots.attstage.innerHTML,/recoverable.txt/);assert.equal(f.sendEntries(),0);assert.equal(f.sends.length,0);}finally{ctx.feature.destroy();}
});

test('rebound People file review does not transfer a published marker across a changed stage revision',async()=>{
 let release,signal;const reached=new Promise(resolve=>{signal=resolve;}),f=transactionalPeopleFixture({afterDecision:()=>new Promise(resolve=>{release=resolve;signal();})}),ctx=f.ctx;
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await f.waitReady(1);const flight=ctx.feature.sendPrompt();await reached;const old=f.files()[0];assert.equal(old.published,7);await f.driver.writePeople(({files,recovery})=>({files:files.map(row=>row.key===old.key?{...row,stageRevision:'b'.repeat(64)}:row),recovery,value:true}));ctx.feature.destroy();ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('new editing');ctx.roots.prompt.firstListener('input')();release();await flight;await localReviewTick();const row=(await f.actual.list(old.scope))[0];assert.equal(row.fileToken,old.fileToken);assert.equal(row.stageRevision,'b'.repeat(64));assert.equal(row.published,7);assert.doesNotMatch(ctx.roots.attstage.innerHTML,/Sent; remove this saved file/);assert.equal(ctx.feature.promptText(),'new editing');assert.equal(f.sendEntries(),1);assert.equal(ctx.apiCalls.filter(call=>call.opts?.method==='DELETE').length,0);}finally{if(release)release();ctx.feature.destroy();}
});

test('rebound People send waits for its saved-file reconciliation before capturing attachments',async()=>{
 let releaseOld,signalOld,releaseRead,signalRead,releaseNew,decisions=0;const oldReady=new Promise(resolve=>{signalOld=resolve;}),readReady=new Promise(resolve=>{signalRead=resolve;}),f=transactionalPeopleFixture({respond:()=>f.sends.length===1?Promise.resolve({seq:7}):new Promise(resolve=>{releaseNew=resolve;}),afterDecision:()=>++decisions===1?new Promise(resolve=>{releaseOld=resolve;signalOld();}):Promise.resolve()}),ctx=f.ctx;
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('old caption');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await f.waitReady(1);const first=ctx.feature.sendPrompt();await oldReady;assert.equal(f.files()[0].published,7);const list=f.store.list;let hold=true;f.store.list=async scope=>{const rows=await list(scope);if(hold){hold=false;signalRead();await new Promise(resolve=>{releaseRead=resolve;});}return rows;};ctx.feature.destroy();ctx.feature.bind();ctx.feature.onSelect();await readReady;ctx.feature.setPromptText('new caption');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'new.txt',type:'text/plain',size:1}]);await f.waitReady(2);const second=ctx.feature.sendPrompt();releaseRead();await Promise.race([f.waitSend(2),second]);assert.equal(f.sendEntries(),2);assert.deepEqual({attachments:f.sendAttempts[1].attachments,dispatches:f.sends.length},{attachments:['fedcba9876543210'],dispatches:2});releaseOld();await first;releaseNew({seq:8});await second;assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);}finally{if(releaseRead)releaseRead();if(releaseOld)releaseOld();if(releaseNew)releaseNew({seq:8});ctx.feature.destroy();}
});

async function heldRebindRecoveryFixture({failRead=false}={}){
 let releaseOld,signalOld,releaseRead,rejectRead,signalRead,decisions=0;const oldReady=new Promise(resolve=>{signalOld=resolve;}),readReady=new Promise(resolve=>{signalRead=resolve;}),f=transactionalPeopleFixture({respond:async()=>({seq:f.sends.length===1?7:8}),afterDecision:()=>++decisions===1?new Promise(resolve=>{releaseOld=resolve;signalOld();}):Promise.resolve()}),ctx=f.ctx;
 ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('old');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await f.waitReady(1);const first=ctx.feature.sendPrompt();await oldReady;const old=f.files()[0];assert.equal(old.published,7);const list=f.store.list;let held=true;f.store.list=async scope=>{const rows=await list(scope);if(held){held=false;signalRead();await new Promise((resolve,reject)=>{releaseRead=resolve;rejectRead=reject;});}return rows;};ctx.feature.destroy();ctx.feature.bind();ctx.feature.onSelect();await readReady;
 return {f,ctx,old,first,releaseOld,releaseRead:()=>failRead?rejectRead(Error('synthetic recovery unavailable')):releaseRead()};
}

test('lost ownership during held People recovery prevents a later send',async()=>{
 const x=await heldRebindRecoveryFixture();try{x.ctx.feature.setPromptText('new');x.ctx.roots.prompt.firstListener('input')();const second=x.ctx.feature.sendPrompt();x.ctx.feature.destroy();x.releaseRead();await second;x.releaseOld();await x.first;assert.equal(x.f.sendEntries(),1);assert.equal(x.f.sends.length,1);assert.equal(x.f.files()[0].fileToken,x.old.fileToken);assert.equal(x.f.files()[0].published,7);assert.equal(x.ctx.apiCalls.filter(call=>call.opts?.method==='DELETE').length,0);assert.deepEqual(x.ctx.alerts,[]);}finally{x.ctx.feature.destroy();}
});

test('failed current People file reconciliation keeps saved input and refuses dispatch',async()=>{
 const x=await heldRebindRecoveryFixture({failRead:true});try{x.ctx.feature.setPromptText('new');x.ctx.roots.prompt.firstListener('input')();const second=x.ctx.feature.sendPrompt();x.releaseRead();await second;x.releaseOld();await x.first;assert.equal(x.f.sendEntries(),1);assert.equal(x.f.sends.length,1);assert.equal(x.ctx.feature.promptText(),'new');assert.equal(x.f.files()[0].fileToken,x.old.fileToken);assert.equal(x.f.files()[0].published,7);assert.match(x.ctx.alerts.join(' '),/could not be recovered/i);assert.equal(x.ctx.apiCalls.filter(call=>call.opts?.method==='DELETE').length,0);}finally{x.ctx.feature.destroy();}
});

test('reconciled sent-only People selection sends only the newly typed text',async()=>{
 const x=await heldRebindRecoveryFixture();try{x.ctx.feature.setPromptText('new text');x.ctx.roots.prompt.firstListener('input')();const second=x.ctx.feature.sendPrompt();x.releaseRead();await second;x.releaseOld();await x.first;assert.equal(x.f.sendEntries(),2);assert.equal(x.f.sends.length,2);assert.equal(x.f.sendAttempts[1].attachments,undefined);assert.equal(x.f.sends[1].request.text,'new text');assert.equal(x.f.files()[0].fileToken,x.old.fileToken);assert.equal(x.f.files()[0].published,7);assert.equal(x.ctx.apiCalls.filter(call=>call.opts?.method==='DELETE').length,0);}finally{x.ctx.feature.destroy();}
});

test('reconciled sent-only People selection cannot dispatch an empty message',async()=>{
 const x=await heldRebindRecoveryFixture();try{x.ctx.feature.setPromptText('');const second=x.ctx.feature.sendPrompt();x.releaseRead();await second;x.releaseOld();await x.first;assert.equal(x.f.sendEntries(),1);assert.equal(x.f.sends.length,1);assert.equal(x.f.files()[0].fileToken,x.old.fileToken);assert.equal(x.f.files()[0].published,7);assert.equal(x.ctx.apiCalls.filter(call=>call.opts?.method==='DELETE').length,0);}finally{x.ctx.feature.destroy();}
});

test('text-only People send remains available while an empty file review is loading',async()=>{
 const f=transactionalPeopleFixture(),ctx=f.ctx,list=f.store.list;let release,entered;const ready=new Promise(resolve=>{entered=resolve;});f.store.list=async scope=>{const rows=await list(scope);entered();await new Promise(resolve=>{release=resolve;});return rows;};
 try{ctx.feature.bind();ctx.feature.onSelect();await ready;ctx.feature.setPromptText('text while loading');ctx.roots.prompt.firstListener('input')();await ctx.feature.sendPrompt();assert.equal(f.sendEntries(),1);assert.equal(f.sends.length,1);assert.equal(f.sends[0].request.text,'text while loading');assert.equal(f.sendAttempts[0].attachments,undefined);assert.equal(ctx.apiCalls.length,0);release();}finally{if(release)release();ctx.feature.destroy();}
});

test('a later explicit People send retries a failed saved-file reconciliation',async()=>{
 const x=await heldRebindRecoveryFixture({failRead:true});try{x.ctx.feature.setPromptText('new');x.ctx.roots.prompt.firstListener('input')();const firstAttempt=x.ctx.feature.sendPrompt();x.releaseRead();await firstAttempt;assert.equal(x.f.sendEntries(),1);assert.match(x.ctx.alerts.join(' '),/could not be recovered/i);x.f.store.list=x.f.actual.list;await x.ctx.feature.sendPrompt();assert.equal(x.f.sendEntries(),2);assert.equal(x.f.sends.length,2);assert.equal(x.f.sends[1].request.text,'new');assert.equal(x.f.sendAttempts[1].attachments,undefined);x.releaseOld();await x.first;assert.equal(x.f.files()[0].fileToken,x.old.fileToken);assert.equal(x.f.files()[0].published,7);assert.equal(x.ctx.apiCalls.filter(call=>call.opts?.method==='DELETE').length,0);}finally{x.releaseOld();x.ctx.feature.destroy();}
});

test('a held old People recovery read cannot block recovery in a rebound lifetime',async()=>{
 const f=transactionalPeopleFixture(),ctx=f.ctx,scope=`people\0${f.context.key}\0binding:${f.context.binding}\0copy:${f.context.copyInstance}\0`,file={name:'rebound.txt',type:'text/plain',size:1};await f.actual.save(scope,'0123456789abcdef',file);let release,entered,reads=0;const started=new Promise(resolve=>{entered=resolve;}),actualList=f.store.list;
 f.store.list=async identity=>{const rows=await actualList(identity);if(++reads===1){entered();await new Promise(resolve=>{release=resolve;});}return rows;};
 try{ctx.feature.bind();ctx.feature.onSelect();await started;ctx.feature.destroy();ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('new input');ctx.roots.prompt.firstListener('input')();await localReviewTick();assert.ok(reads>=2,'new lifetime must not wait for the old held read');assert.match(ctx.roots.attstage.innerHTML,/rebound.txt/);assert.equal(ctx.feature.promptText(),'new input');assert.deepEqual(ctx.alerts,[]);release();await localReviewTick();assert.equal(ctx.feature.promptText(),'new input');assert.equal(f.sendEntries(),0);assert.equal(f.sends.length,0);assert.equal((await f.actual.list(scope))[0].file,file);}finally{if(release)release();ctx.feature.destroy();}
});

test('settling an old People recovery read cannot release a newer held recovery',async()=>{
 const f=transactionalPeopleFixture(),ctx=f.ctx,scope=`people\0${f.context.key}\0binding:${f.context.binding}\0copy:${f.context.copyInstance}\0`,file={name:'saved-new-view.txt',type:'text/plain',size:1};await f.actual.save(scope,'0123456789abcdef',file);let reads=0,releaseOld,releaseNew,oldEntered,newEntered;const oldReady=new Promise(resolve=>{oldEntered=resolve;}),newReady=new Promise(resolve=>{newEntered=resolve;}),list=f.store.list;
 f.store.list=async identity=>{const rows=await list(identity);const number=++reads;if(number===1){oldEntered();await new Promise(resolve=>{releaseOld=resolve;});}if(number===2){newEntered();await new Promise(resolve=>{releaseNew=resolve;});}return rows;};
 try{ctx.feature.bind();ctx.feature.onSelect();await oldReady;ctx.feature.destroy();ctx.feature.bind();ctx.feature.onSelect();await newReady;releaseOld();await localReviewTick();ctx.feature.contextChanged();await localReviewTick();assert.equal(reads,2);releaseNew();await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/saved-new-view.txt/);assert.equal((await f.actual.list(scope))[0].file,file);assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);assert.deepEqual(ctx.alerts,[]);}finally{if(releaseOld)releaseOld();if(releaseNew)releaseNew();ctx.feature.destroy();}
});

test('held People recovery preserves captured file intent and one active send',async()=>{
 const x=await heldRebindRecoveryFixture();try{
  x.ctx.feature.setPromptText('captured text');x.ctx.roots.prompt.firstListener('input')();const pending=x.ctx.feature.sendPrompt();
  x.ctx.feature.addFiles([{name:'new.txt',type:'text/plain',size:1}]);await x.f.waitReady(2);await x.ctx.feature.sendPrompt();assert.equal(x.f.sendEntries(),1,'held Send owns the flight before admission');
  x.releaseRead();await pending;x.releaseOld();await x.first;
  assert.equal(x.f.sendEntries(),2);assert.equal(x.f.sends.length,2);assert.equal(x.f.sends[1].request.text,'captured text');assert.equal(x.f.sendAttempts[1].attachments,undefined);
  const newer=x.f.files().find(row=>row.file.name==='new.txt');assert.ok(newer);assert.equal(newer.published,undefined);assert.match(x.ctx.roots.attstage.innerHTML,/new.txt/);assert.equal(x.ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);
 }finally{x.releaseRead();x.releaseOld();x.ctx.feature.destroy();}
});

test('held People recovery preserves both submitted and newer draft text',async()=>{
 const x=await heldRebindRecoveryFixture();try{
  x.ctx.feature.setPromptText('captured submitted text');x.ctx.roots.prompt.firstListener('input')();await x.f.waitDraft(2);const pending=x.ctx.feature.sendPrompt();
  x.ctx.feature.setPromptText('newer draft text');x.ctx.roots.prompt.firstListener('input')();await x.f.waitDraft(3);x.releaseRead();await pending;x.releaseOld();await x.first;
  assert.equal(x.ctx.feature.promptText(),'newer draft text');assert.ok(x.f.recovery().some(row=>row.kind==='draft' && row.text==='newer draft text'));assert.ok(x.f.sendEntries()<=2);assert.ok(x.f.sends.length<=2);assert.ok(x.f.sendAttempts.slice(1).every(row=>row.text==='captured submitted text'));assert.equal(x.ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);
  const sent=x.f.sends.slice(1).some(row=>row.request.text==='captured submitted text'),kept=x.f.recovery().some(row=>row.text==='captured submitted text'||row.request?.text==='captured submitted text');
  assert.ok(sent||kept,'the submitted text must be delivered or remain recoverable when newer typing causes admission to fail');
 }finally{x.releaseRead();x.releaseOld();x.ctx.feature.destroy();}
});

test('another writer cannot erase submitted People text during held admission',async()=>{
 const x=await heldRebindRecoveryFixture();try{
  x.ctx.feature.setPromptText('captured cross-writer submission');x.ctx.roots.prompt.firstListener('input')();await x.f.waitDraft(2);
  const draft=x.f.recovery().find(row=>row.kind==='draft'&&row.text==='captured cross-writer submission'),head=x.f.recovery().find(row=>row.kind==='head'&&row.version===draft.version);assert.ok(head);
  const pending=x.ctx.feature.sendPrompt();const changed=await x.f.actual.savePeopleDraft({scope:head.scope,text:'another writer draft',head:head.head,expected:head.version});assert.ok(changed.version);
  x.releaseRead();await pending;x.releaseOld();await x.first;
  assert.ok(x.f.recovery().some(row=>row.kind==='draft'&&row.text==='another writer draft'));assert.equal(x.ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);
  const sent=x.f.sends.slice(1).some(row=>row.request.text==='captured cross-writer submission'),kept=x.f.recovery().some(row=>row.text==='captured cross-writer submission'||row.request?.text==='captured cross-writer submission');
  assert.ok(sent||kept,'submitted input must survive reload even when another writer edits the observed head before admission');
 }finally{x.releaseRead();x.releaseOld();x.ctx.feature.destroy();}
});

test('failed People submission freeze does not discard the captured draft when typing continues',async()=>{
 const x=await heldRebindRecoveryFixture();try{
  x.ctx.feature.setPromptText('submitted before storage failure');x.ctx.roots.prompt.firstListener('input')();await x.f.waitDraft(2);
  x.f.store.freezePeopleDraft=async()=>{throw Error('synthetic freeze unavailable');};const pending=x.ctx.feature.sendPrompt();
  x.ctx.feature.setPromptText('editing after storage failure');x.ctx.roots.prompt.firstListener('input')();await x.f.waitDraft(3);x.releaseRead();await pending;x.releaseOld();await x.first;
  const drafts=x.f.recovery().filter(row=>row.kind==='draft').map(row=>row.text);assert.ok(drafts.includes('submitted before storage failure'));assert.ok(drafts.includes('editing after storage failure'));assert.equal(x.ctx.feature.promptText(),'editing after storage failure');assert.equal(x.f.sends.length,1);assert.match(x.ctx.alerts.join(' '),/unavailable/i);assert.equal(x.ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);
 }finally{x.releaseRead();x.releaseOld();x.ctx.feature.destroy();}
});

test('confirmation of an older same-text retry cannot clear a newer submission editor',async()=>{
 let f,calls=0,earlier;const context={key:localReviewKey,copyInstance:'1'.repeat(32),binding:'current',owner:false,writable:true,attachments:false};
 f=transactionalPeopleFixture({context,depsExtra:{sendPeople:async(key,text,attachments,native,admission)=>{
  calls++;assert.equal(attachments,undefined);
  if(calls===1){const identity={scope:admission.scope,peopleKey:key,copyInstance:context.copyInstance,binding:context.binding,native,head:admission.head,version:admission.version,request:{key:'2'.repeat(16),text,anchor:0}},created=await f.actual.beginPeopleTextPublication({...identity,current:admission.current});earlier={...identity,publication:created.publication};throw Error('synthetic uncertain publication');}
  const decision=await f.actual.finishPeopleTextPublication({scope:earlier.scope,publication:earlier.publication,expected:earlier,seq:7,current:admission.current});return {seq:7,peopleTextDecision:decision};
 }}});const ctx=f.ctx;
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('same synthetic text');ctx.roots.prompt.firstListener('input')();await localReviewTick();await ctx.feature.sendPrompt();assert.equal(calls,1);assert.ok(earlier);ctx.feature.setPromptText('same synthetic text');ctx.roots.prompt.firstListener('input')();await localReviewTick();const editable=f.recovery().find(row=>row.kind==='head'&&!row.sealed),version=editable.version;await ctx.feature.sendPrompt();assert.equal(calls,2);assert.equal(f.recovery().find(row=>row.key===editable.key).version,version);assert.equal(ctx.feature.promptText(),'same synthetic text','a receipt for the prior frozen head must not clear the newer explicit submission editor');assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);}finally{ctx.feature.destroy();}
});

test('file-only People Send admits a unique frozen submission without an earlier text input',async()=>{
 let release;const f=transactionalPeopleFixture({respond:()=>new Promise(resolve=>{release=resolve;})}),ctx=f.ctx;
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.addFiles([{name:'synthetic.txt',type:'text/plain',size:1}]);await f.waitReady(1);const sending=ctx.feature.sendPrompt();await f.waitSend(1);const pending=f.pending(),head=f.recovery().find(row=>row.key==='head:'+pending.head);assert.ok(head?.sealed,'the explicit file-only send must use a frozen head');assert.equal(head.version,pending.version);assert.equal(f.recovery().find(row=>row.key==='draft:'+head.version).text,'');assert.equal(f.sends.length,1);assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);release({seq:7});await sending;}finally{if(release)release({seq:7});ctx.feature.destroy();}
});

test('People file confirmation without file identity keeps saved bytes for review',async()=>{
 const f=transactionalPeopleFixture({depsExtra:{finishPeople:async receipt=>({state:'completed',seq:receipt.seq,filesConfirmed:false,draftCleared:false})}}),ctx=f.ctx;
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('synthetic caption');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'review.txt',type:'text/plain',size:2}]);await f.waitReady(1);const before=JSON.stringify(f.files());await ctx.feature.sendPrompt();assert.equal(f.sends.length,1);assert.equal(JSON.stringify(f.files()),before);assert.match(ctx.roots.attstage.innerHTML,/review whether this saved file/i);assert.equal(ctx.apiCalls.filter(call=>call.opts?.method==='DELETE').length,0);assert.equal(ctx.feature.promptText(),'synthetic caption');}finally{ctx.feature.destroy();}
});

test('People file completion with no matching durable decision preserves the saved file',async()=>{
 for(const kind of ['missing','pending','different-sequence']){
  const f=transactionalPeopleFixture({depsExtra:{finishPeople:async receipt=>kind==='missing'?null:{state:kind==='pending'?'pending':'completed',seq:kind==='different-sequence'?receipt.seq+1:receipt.seq,filesConfirmed:true}}}),ctx=f.ctx;
  try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('synthetic caption');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'keep.txt',type:'text/plain',size:2}]);await f.waitReady(1);const before=JSON.stringify(f.files());await ctx.feature.sendPrompt();assert.equal(f.sends.length,1,kind);assert.equal(JSON.stringify(f.files()),before,kind);assert.equal(ctx.apiCalls.filter(call=>call.opts?.method==='DELETE').length,0,kind);assert.match(ctx.alerts.join(' '),/could not be recorded locally/i,kind);}finally{ctx.feature.destroy();}
 }
});

test('saved People review distinguishes a captured submission from an editable draft after reload',async()=>{
 const context={key:localReviewKey,copyInstance:'1'.repeat(32),binding:'current',owner:false,writable:true,attachments:false},f=transactionalPeopleFixture({context,respond:async()=>{throw Error('synthetic uncertain delivery');}}),ctx=f.ctx;
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('same text');ctx.roots.prompt.firstListener('input')();await f.waitDraft(1);await ctx.feature.sendPrompt();assert.equal(f.sendEntries(),1);assert.equal(f.sends.length,1);const frozen=f.recovery().find(row=>row.kind==='head'&&row.sealed),original=f.recovery().find(row=>row.kind==='head'&&!row.sealed);assert.ok(frozen);assert.ok(original);ctx.feature.destroy();ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/captured submission/i);assert.match(ctx.roots.attstage.innerHTML,/saved draft/i);assert.equal(f.sendEntries(),1);assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);}finally{ctx.feature.destroy();}
});

test('one editable People draft is restored beside an older sealed pending submission',async()=>{
 const context={key:localReviewKey,copyInstance:'1'.repeat(32),binding:'current',owner:false,writable:true,attachments:false},scope=`people\0${localReviewKey}\0binding:current\0copy:${context.copyInstance}\0`,f=transactionalPeopleFixture({context}),ctx=f.ctx;
 const original=await f.actual.savePeopleDraft({scope,text:'earlier message'}),frozen=await f.actual.freezePeopleDraft({scope,text:'earlier message',...original}),request={key:'4'.repeat(16),text:'earlier message',anchor:0};await f.actual.beginPeopleTextPublication({scope,peopleKey:context.key,copyInstance:context.copyInstance,binding:context.binding,native:'',...frozen,request});const newer=await f.actual.savePeopleDraft({scope,text:'newer editable input',head:original.head,expected:original.version});
 try{const before=await f.actual.listPeopleRecovery(scope);assert.equal(before.heads.length,2);assert.equal(before.heads.filter(head=>!head.sealed).length,1);assert.equal(before.heads.find(head=>!head.sealed).version,newer.version);assert.equal(before.textPending[0].request.key,request.key);ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/Captured submission/);assert.equal(ctx.feature.promptText(),'newer editable input');assert.deepEqual(await f.actual.listPeopleRecovery(scope),before);assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('People draft restoration selects only a unique editable head across sealed captures',async()=>{
 for(const editable of [0,1,2])for(const sealed of [0,1,2]){
  const context={key:localReviewKey,copyInstance:'1'.repeat(32),binding:'current',owner:false,writable:true,attachments:false},scope=`people\0${localReviewKey}\0binding:current\0copy:${context.copyInstance}\0`,f=transactionalPeopleFixture({context}),ctx=f.ctx;
  for(let i=0;i<editable;i++)await f.actual.savePeopleDraft({scope,text:'editable '+i});for(let i=0;i<sealed;i++){const draft=await f.actual.savePeopleDraft({scope,text:'capture '+i});await f.actual.freezePeopleDraft({scope,text:'capture '+i,...draft});assert.equal(await f.actual.clearPeopleDraft({scope,...draft}),true);}
  try{const before=await f.actual.listPeopleRecovery(scope);assert.equal(before.heads.filter(head=>!head.sealed).length,editable);assert.equal(before.heads.filter(head=>head.sealed).length,sealed);const raw=JSON.stringify(f.recovery());ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(ctx.feature.promptText(),editable===1?'editable 0':'',`${editable} editable, ${sealed} sealed`);for(let i=0;i<sealed;i++)assert.match(ctx.roots.attstage.innerHTML,new RegExp('capture '+i));assert.equal(JSON.stringify(f.recovery()),raw);assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
 }
});

test('late People draft restore cannot overwrite a newer in-page edit',async()=>{
 const context={key:localReviewKey,copyInstance:'1'.repeat(32),binding:'current',owner:false,writable:true,attachments:false},scope=`people\0${localReviewKey}\0binding:current\0copy:${context.copyInstance}\0`,f=transactionalPeopleFixture({context}),ctx=f.ctx;const old=await f.actual.savePeopleDraft({scope,text:'old editable'});await f.actual.freezePeopleDraft({scope,text:'old editable',...old});const list=f.store.listPeopleRecovery;let release,entered;const ready=new Promise(resolve=>{entered=resolve;});f.store.listPeopleRecovery=async scope=>{const value=await list(scope);entered();await new Promise(resolve=>{release=resolve;});return value;};
 try{ctx.feature.bind();ctx.feature.onSelect();await ready;ctx.feature.setPromptText('new in-page edit');ctx.roots.prompt.firstListener('input')();await localReviewTick();release();await localReviewTick();assert.equal(ctx.feature.promptText(),'new in-page edit');assert.ok(f.recovery().some(row=>row.kind==='draft'&&row.text==='new in-page edit'));assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{if(release)release();ctx.feature.destroy();}
});

test('People send saves and freezes input when restored review has no selected draft head',async()=>{
 const context={key:localReviewKey,copyInstance:'1'.repeat(32),binding:'current',owner:false,writable:true,attachments:false},scope=`people\0${localReviewKey}\0binding:current\0copy:${context.copyInstance}\0`,f=transactionalPeopleFixture({context}),ctx=f.ctx;
 await f.actual.savePeopleDraft({scope,text:'saved one'});await f.actual.savePeopleDraft({scope,text:'saved two'});
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/saved one/);ctx.feature.setPromptText('explicit text');await ctx.feature.sendPrompt();assert.equal(f.sendEntries(),1);assert.equal(f.sends[0].request.text,'explicit text');assert.equal(f.sendAttempts[0].requireDurableAdmission,true);const heads=(await f.actual.listPeopleRecovery(scope)).heads;assert.deepEqual(heads.map(row=>row.text).sort(),['saved one','saved two']);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('People send refuses dispatch when the captured draft cannot be stored',async()=>{
 const f=transactionalPeopleFixture(),ctx=f.ctx;f.store.savePeopleDraft=async()=>{throw Error('synthetic storage unavailable');};
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('unsaved text');ctx.roots.prompt.firstListener('input')();await localReviewTick();await ctx.feature.sendPrompt();assert.equal(f.sendEntries(),0);assert.equal(f.sends.length,0);assert.equal(ctx.feature.promptText(),'unsaved text');assert.match(ctx.alerts.join(' '),/could not be saved/i);assert.equal(ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);}finally{ctx.feature.destroy();}
});

test('unavailable text-pending lookup does not hide a completed People review',async()=>{
 const context={key:localReviewKey,copyInstance:'1'.repeat(32),binding:'current',owner:false,writable:true,attachments:false},f=transactionalPeopleFixture({context,depsExtra:{pendingPeopleText:()=>{throw Error('synthetic unavailable');}}}),ctx=f.ctx;
 f.store.listPeopleRecovery=async()=>({heads:[],pending:[],textPending:[],legacy:[],completed:[{seq:7,request:{text:'completed synthetic'}}]});
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/completed synthetic/);assert.match(ctx.roots.attstage.innerHTML,/sequence 7/);assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('unreadable People storage protects an older pending request before composer restoration',async()=>{
 const context={key:localReviewKey,copyInstance:'1'.repeat(32),binding:'current',owner:false,writable:true,attachments:false},f=transactionalPeopleFixture({context}),ctx=f.ctx,scope=`people\0${context.key}\0binding:current\0copy:${context.copyInstance}\0`;
 const doneDraft=await f.actual.savePeopleDraft({scope,text:'completed earlier'}),doneRequest={key:'5'.repeat(16),text:'completed earlier',anchor:0},doneIdentity={scope,peopleKey:context.key,copyInstance:context.copyInstance,binding:context.binding,native:'',...doneDraft,request:doneRequest},done=await f.actual.beginPeopleTextPublication(doneIdentity);await f.actual.finishPeopleTextPublication({scope,publication:done.publication,expected:doneIdentity,seq:8});
 const draft=await f.actual.savePeopleDraft({scope,text:'older message'}),request={key:'4'.repeat(16),text:'older message',anchor:0},identity={scope,peopleKey:context.key,copyInstance:context.copyInstance,binding:context.binding,native:'',...draft,request};
 const pending=await f.actual.beginPeopleTextPublication(identity);await f.actual.save(scope,'0123456789abcdef',{name:'kept.txt',type:'text/plain',size:3});
 const before=JSON.stringify({recovery:f.recovery(),files:f.files()}),legacyKey='scimux-people-file-pending:synthetic',legacyValue='unchanged synthetic legacy';ctx.storage.setItem(legacyKey,legacyValue);
 const fail=async()=>{throw Object.assign(Error('storage unavailable'),{code:'PEOPLE_STORAGE_UNAVAILABLE'});};f.store.listPeopleRecovery=fail;f.store.savePeopleDraft=fail;
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();ctx.feature.setPromptText('new visible text');ctx.roots.prompt.firstListener('input')();await localReviewTick();await ctx.feature.sendPrompt();assert.equal(f.sendEntries(),0);assert.equal(f.sends.length,0);assert.equal(ctx.feature.promptText(),'new visible text');assert.equal(JSON.stringify({recovery:f.recovery(),files:f.files()}),before);assert.ok(f.recovery().some(row=>row.kind==='text-publication'&&row.publication===pending.publication));assert.ok(f.recovery().some(row=>row.kind==='text-decision'&&row.publication===done.publication));assert.equal(ctx.storage.getItem(legacyKey),legacyValue);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('People text receipts with missing or mismatched publication identity cannot clear a frozen editor',async()=>{
 for(const mode of ['missing','scope','head','version','publication','key','state','sequence']){
  const context={key:localReviewKey,copyInstance:'1'.repeat(32),binding:'current',owner:false,writable:true,attachments:false};let f;
  f=transactionalPeopleFixture({context,depsExtra:{sendPeople:async(_key,_text,_files,_native,admission)=>{const identity={scope:admission.scope,head:admission.head,version:admission.version,publication:'a'.repeat(32)+':1',requestKey:'2'.repeat(16)};if(mode==='scope')identity.scope='other';if(mode==='head')identity.head='other';if(mode==='version')identity.version='other';if(mode==='publication')identity.publication='bad';if(mode==='key')identity.requestKey='bad';return {seq:7,peopleTextDecision:{state:mode==='state'?'pending':'completed',seq:mode==='sequence'?8:7,draftCleared:true,...(mode==='missing'?{}:{identity})}};}}});const ctx=f.ctx;
  try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('saved text');ctx.roots.prompt.firstListener('input')();await f.waitDraft(1);await ctx.feature.sendPrompt();assert.equal(ctx.feature.promptText(),'saved text',mode);assert.equal(f.recovery().filter(row=>row.kind==='head').length,2,mode);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
 }
});

test('a lone unadmitted frozen People submission stays labeled for review after reload',async()=>{
 const context={key:localReviewKey,copyInstance:'1'.repeat(32),binding:'current',owner:false,writable:true,attachments:false},scope=`people\0${localReviewKey}\0binding:current\0copy:${context.copyInstance}\0`,f=transactionalPeopleFixture({context}),ctx=f.ctx,original=await f.actual.savePeopleDraft({scope,text:'submitted'}),frozen=await f.actual.freezePeopleDraft({scope,text:'submitted',...original});assert.equal(await f.actual.clearPeopleDraft({scope,...original}),true);
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(ctx.feature.promptText(),'');assert.match(ctx.roots.attstage.innerHTML,/captured submission/i);assert.match(ctx.roots.attstage.innerHTML,/submitted/);assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('explicit captured People draft discard preserves editable text and saved files',async()=>{
 const context={key:localReviewKey,copyInstance:'1'.repeat(32),binding:'current',owner:false,writable:true,attachments:false},scope=`people\0${localReviewKey}\0binding:current\0copy:${context.copyInstance}\0`,f=transactionalPeopleFixture({context}),ctx=f.ctx,original=await f.actual.savePeopleDraft({scope,text:'editable note'}),captured=await f.actual.freezePeopleDraft({scope,text:'editable note',...original});await f.actual.save(scope,'0123456789abcdef',{name:'kept.txt',type:'text/plain',size:2});const before=JSON.stringify(f.files()),allocator=f.recovery().find(row=>row.key==='allocator').next;ctx.storage.setItem('synthetic-legacy','unchanged');
 const old=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=selector=>selector==='.people-captured-discard'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-captured-discard(?:\s+[^"\s]+)*" data-head="([^"]+)" data-version="([^"]+)"/g)].map(m=>el('button',{dataset:{head:m[1],version:m[2]}}))):old(selector);
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(ctx.feature.promptText(),'editable note');assert.ok(f.recovery().some(row=>row.kind==='head'&&row.head===captured.head&&row.sealed));assert.match(ctx.roots.attstage.innerHTML,/Captured submission/);assert.equal(buttons.length,1,'a reviewed captured submission must offer an explicit discard');buttons[0].click();await localReviewTick();assert.ok(!f.recovery().some(row=>row.kind==='head'&&row.head===captured.head));assert.ok(f.recovery().some(row=>row.kind==='head'&&row.head===original.head));assert.equal(f.recovery().find(row=>row.key==='allocator').next,allocator);assert.equal(JSON.stringify(f.files()),before);assert.equal(ctx.storage.getItem('synthetic-legacy'),'unchanged');assert.equal(ctx.feature.promptText(),'editable note');assert.match(ctx.roots.attstage.innerHTML,/does not retract/i);assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('People send with a durable draft refuses dispatch when freeze capability is missing',async()=>{
 const context={key:localReviewKey,copyInstance:'1'.repeat(32),binding:'current',owner:false,writable:true,attachments:false},f=transactionalPeopleFixture({context}),ctx=f.ctx;delete f.store.freezePeopleDraft;
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('saved input');ctx.roots.prompt.firstListener('input')();await f.waitDraft(1);await ctx.feature.sendPrompt();assert.equal(f.sendEntries(),0,'a saved draft must not be dispatched without its frozen identity');assert.equal(f.sends.length,0);assert.equal(ctx.feature.promptText(),'saved input');assert.ok(f.recovery().some(row=>row.kind==='draft'&&row.text==='saved input'));assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('full People draft capacity keeps submitted and newer input truthful without dispatch',async()=>{
 const x=await heldRebindRecoveryFixture();try{const scope=`people\0${x.f.context.key}\0binding:${x.f.context.binding}\0copy:${x.f.context.copyInstance}\0`;x.ctx.feature.setPromptText('submitted before capacity');x.ctx.roots.prompt.firstListener('input')();await x.f.waitDraft(2);for(let i=0;i<31;i++)await x.f.actual.savePeopleDraft({scope,text:'other '+i});assert.equal((await x.f.actual.listPeopleRecovery(scope)).heads.length,32);const pending=x.ctx.feature.sendPrompt();x.ctx.feature.setPromptText('new text at capacity');x.ctx.roots.prompt.firstListener('input')();await localReviewTick();x.releaseRead();await pending;x.releaseOld();await x.first;assert.equal(x.ctx.feature.promptText(),'new text at capacity');assert.ok((await x.f.actual.listPeopleRecovery(scope)).heads.some(row=>row.text==='submitted before capacity'));assert.equal((await x.f.actual.listPeopleRecovery(scope)).heads.length,32);assert.equal(x.f.sends.length,1);assert.match(x.ctx.alerts.join(' '),/limit|storage/i);assert.match(x.ctx.roots.attstage.innerHTML,/could not be saved/i);assert.equal(x.ctx.apiCalls.filter(call=>call.path.endsWith('/send')).length,0);}finally{x.releaseRead();x.releaseOld();x.ctx.feature.destroy();}
});

test('stale navigation restore cannot replace a newer in-page edit with the unique editable draft',async()=>{
 const context={key:localReviewKey,copyInstance:'1'.repeat(32),binding:'current',owner:false,writable:true,attachments:false},scope=`people\0${localReviewKey}\0binding:current\0copy:${context.copyInstance}\0`,f=transactionalPeopleFixture({context}),ctx=f.ctx;
 const draft=await f.actual.savePeopleDraft({scope,text:'old editable'});await f.actual.freezePeopleDraft({scope,text:'old editable',...draft});const before=await f.actual.listPeopleRecovery(scope);assert.equal(before.heads.filter(head=>!head.sealed).length,1);
 const list=f.store.listPeopleRecovery;let release,entered,first=true;const ready=new Promise(resolve=>{entered=resolve;});f.store.listPeopleRecovery=async value=>{const result=await list(value);if(first){first=false;entered();await new Promise(resolve=>{release=resolve;});}return result;};
 try{ctx.feature.bind();ctx.feature.onSelect();await ready;ctx.setSel('other');ctx.feature.onSelect();ctx.setSel('team/'+context.key);ctx.feature.onSelect();await localReviewTick();ctx.feature.setPromptText('new in-page edit');ctx.roots.prompt.firstListener('input')();await localReviewTick();release();await localReviewTick();assert.equal(ctx.feature.promptText(),'new in-page edit');assert.ok(f.recovery().some(row=>row.kind==='draft'&&row.text==='new in-page edit'));assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{if(release)release();ctx.feature.destroy();}
});

test('normal frozen text publication exposes an explicit local retirement action scoped to its review',async()=>{
 const context={key:localReviewKey,copyInstance:'1'.repeat(32),binding:'current',owner:false,writable:true,attachments:false},scope=`people\0${localReviewKey}\0binding:current\0copy:${context.copyInstance}\0`;let f;
 f=transactionalPeopleFixture({context,depsExtra:{pendingPeopleText:()=>f.recovery().find(row=>row.kind==='text-publication'&&row.state==='pending')}});const ctx=f.ctx;
 const draft=await f.actual.savePeopleDraft({scope,text:'frozen message'}),captured=await f.actual.freezePeopleDraft({scope,text:'frozen message',...draft}),request={key:'4'.repeat(16),text:'frozen message',anchor:0};const publication=await f.actual.beginPeopleTextPublication({scope,peopleKey:context.key,copyInstance:context.copyInstance,binding:context.binding,native:'',...captured,request});
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();const html=ctx.roots.attstage.innerHTML,review=html.match(/<details\b[^>]*>\s*<summary\b[^>]*>Review unconfirmed People text<\/summary>[\s\S]*?<\/details>/)?.[0];assert.ok(review,'the matching unconfirmed text review is rendered');assert.match(review,/frozen message/);assert.match(review,/<button[^>]*>[^<]*(?:discard|retire)[^<]*<\/button>/i,'the text review itself must offer explicit local retirement');assert.equal(f.recovery().find(row=>row.publication===publication.publication).state,'pending');assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

for(const loss of ['navigation','destroy']) test(`queued captured-draft discard refuses after ${loss}`,async()=>{
 const context={key:localReviewKey,copyInstance:'1'.repeat(32),binding:'current',owner:false,writable:true,attachments:false},scope=`people\0${localReviewKey}\0binding:current\0copy:${context.copyInstance}\0`,f=transactionalPeopleFixture({context}),ctx=f.ctx,draft=await f.actual.savePeopleDraft({scope,text:'saved'}),captured=await f.actual.freezePeopleDraft({scope,text:'saved',...draft});let release,entered;const ready=new Promise(resolve=>{entered=resolve;}),actual=f.store.discardPeopleCapture;
 f.store.discardPeopleCapture=async args=>{entered();await new Promise(resolve=>{release=resolve;});return actual(args);};const old=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=selector=>selector==='.people-captured-discard'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-captured-discard(?:\s+[^"\s]+)*" data-head="([^"]+)" data-version="([^"]+)"/g)].map(m=>el('button',{dataset:{head:m[1],version:m[2]}}))):old(selector);
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(buttons.length,1);buttons[0].click();await ready;if(loss==='destroy')ctx.feature.destroy();else{ctx.setSel('other');ctx.feature.onSelect();ctx.feature.setPromptText('new view');}release();await localReviewTick();assert.ok(f.recovery().some(row=>row.key==='head:'+captured.head));assert.ok(f.recovery().some(row=>row.key==='draft:'+captured.version));if(loss==='navigation')assert.equal(ctx.feature.promptText(),'new view');assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{if(release)release();ctx.feature.destroy();}
});

// This harness renders button nodes from the text review itself.  It delegates
// storage operations to the real store; it does not implement retirement.
// These minimal boundary adapters are not the actual guest integration.
function textReviewBlocks(html){return [...html.matchAll(/<details\b[^>]*>\s*<summary\b[^>]*>Review unconfirmed People text<\/summary>[\s\S]*?<\/details>/g)].map(match=>match[0]);}
async function textRetirementComposerFixture({mode='received',text='saved text',sealed=true,copyInstance:copy='1'.repeat(32),offline=false,depsExtra={}}={}) {
 const owner=mode.startsWith('owner'),ended=mode.includes('ended'),readonly=mode==='received-readonly',key=localReviewKey,copyInstance=copy,binding=mode==='owner-ended-prior'?'old':'current',native=owner?'native':'',context={key,copyInstance,binding:mode==='owner-ended-prior'?'replacement':binding,owner,writable:!readonly&&!ended,attachments:false,state:ended?'ended':'live'},scope=`people\0${key}\0binding:${binding}${copyInstance?'\0copy:'+copyInstance:''}\0${owner?'n1\0'+native:''}`;let f;const localRequests=[],textReads={current:0,retained:0};
 f=transactionalPeopleFixture({context,nodes:owner?[{id:'n1',instance_id:native}]:[],depsExtra:{peopleContext:()=>ended&&owner?null:context,localReviewContexts:()=>[context],pendingPeopleText:()=>{textReads.current++;return ended&&owner?null:f.recovery().find(row=>row.kind==='text-publication'&&row.state==='pending'&&row.peopleKey===context.key&&row.copyInstance===context.copyInstance&&row.binding===context.binding&&row.native===(owner?(f.ctx.nodes.find(node=>node.id==='n1')?.instance_id||''):''));},readRetainedPeopleTextPending:copy=>{textReads.retained++;return {entries:f.recovery().filter(row=>row.kind==='text-publication'&&row.state==='pending'&&row.peopleKey===copy.key&&row.copyInstance===copy.copyInstance&&row.native===(owner?(f.ctx.nodes.find(node=>node.id==='n1')?.instance_id||''):'')).map(row=>structuredClone(row)),unavailable:false};},refreshRetainedPeopleRecovery:async()=>{},retirePeopleText:(snapshot,options)=>f.store.retirePeopleTextPublication({scope:snapshot.scope,publication:snapshot.publication,expected:snapshot,current:options.current}),readRetainedPeoplePending:()=>({entries:[],unavailable:!copyInstance}),localReviewApi:async(path,opts)=>{localRequests.push({path,method:opts.method});assert.equal(opts.method,'GET');if(offline)throw Object.assign(Error('synthetic unavailable'),{status:404});return {entries:[]};},...depsExtra}});
 const ctx=f.ctx,draft=await f.actual.savePeopleDraft({scope,text}),capture=sealed?await f.actual.freezePeopleDraft({scope,text,...draft}):draft,request={key:'4'.repeat(16),text,anchor:0},expected={peopleKey:key,copyInstance,binding,native,...capture,request},saved=await f.actual.beginPeopleTextPublication({scope,...expected});
 await f.actual.save(scope,'3'.repeat(16),{name:'retained.txt',type:'text/plain',size:1});ctx.storage.setItem('synthetic-legacy-text','unchanged');
 const old=ctx.roots.attstage.querySelectorAll;let textButtons=[],textDetails=[];const injected={buttons:[],details:[]};
 ctx.roots.attstage.querySelectorAll=selector=>{
  const reviews=textReviewBlocks(ctx.roots.attstage.innerHTML);
  if(selector==='.people-text-review'){textDetails=reviews.map(review=>{const tag=review.match(/^<details\b([^>]*)>/)[1],row=tag.match(/data-row="([^"]+)"/)?.[1];const detail=el('details',{dataset:{row}});detail.open=/\sopen(?:\s|$)/.test(tag);return detail;});return [...textDetails,...injected.details];}
  if(!/^\.[\w-]+$/.test(selector)||!reviews.some(review=>review.includes('<button')))return old(selector);
  const className=selector.slice(1),matches=reviews.flatMap(review=>[...review.matchAll(/<button\b([^>]*)>([\s\S]*?)<\/button>/g)]).filter(m=>new RegExp('class="[^"]*\\b'+className+'\\b').test(m[1]));
  if(!matches.length)return old(selector);
  textButtons=matches.map(m=>{const dataset={};for(const a of m[1].matchAll(/data-([\w-]+)="([^"]*)"/g))dataset[a[1].replace(/-([a-z])/g,(_,c)=>c.toUpperCase())]=a[2];const button=el('button',{dataset,textContent:m[2]});for(const a of m[1].matchAll(/([\w-]+)="([^"]*)"/g))button.setAttribute(a[1],a[2]);return button;});return [...textButtons,...injected.buttons];
 };
 return {f,ctx,context,scope,draft,capture,request,expected,publication:saved.publication,buttons:()=>textButtons,details:()=>textDetails,injected,reviews:()=>textReviewBlocks(ctx.roots.attstage.innerHTML),localRequests,textReads};
}

for(const mode of ['received','received-readonly','received-ended','owner','owner-ended-prior']) for(const sealed of [false,true]) test(`explicit text retirement from ${mode} ${sealed?'frozen':'unsealed'} review preserves durable identity`,async()=>{
 const x=await textRetirementComposerFixture({mode,sealed}),{ctx,f,scope,publication,expected}=x;
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();const destination=ctx.feature.destination(),beforeFiles=JSON.stringify(f.files()),beforeLegacy=ctx.storage.getItem('synthetic-legacy-text'),beforeAllocator=JSON.stringify(f.recovery().find(row=>row.key==='allocator')),beforeRows=structuredClone(f.recovery()),pendingRow=structuredClone(f.recovery().find(row=>row.publication===publication));
  const review=ctx.roots.attstage.innerHTML.match(/<details\b[^>]*>\s*<summary\b[^>]*>Review unconfirmed People text<\/summary>[\s\S]*?<\/details>/)?.[0];assert.ok(review,'matching text publication must be reviewable');assert.match(review,/saved text/);assert.match(review,/<button[^>]*>[^<]*(?:discard|retire)[^<]*<\/button>/i,'only a button inside the matching text review qualifies');assert.equal(x.buttons().length,1,'the review action must be wired');x.buttons()[0].click();await localReviewTick();
  const decision=f.recovery().find(row=>row.kind==='text-decision'&&row.publication===publication);assert.equal(decision?.state,'retired');assert.deepEqual(decision,{...pendingRow,key:'text-decision:'+publication,kind:'text-decision',state:'retired'});assert.deepEqual(f.recovery().filter(row=>row!==decision),beforeRows.filter(row=>row.publication!==publication));assert.deepEqual(decision.request,expected.request);assert.ok(!f.recovery().some(row=>row.kind==='text-publication'&&row.publication===publication));assert.equal(JSON.stringify(f.files()),beforeFiles);assert.equal(JSON.stringify(f.recovery().find(row=>row.key==='allocator')),beforeAllocator);assert.equal(ctx.storage.getItem('synthetic-legacy-text'),beforeLegacy);assert.equal(ctx.feature.destination(),destination);assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);
 }finally{ctx.feature.destroy();}
});

for(const change of ['same','different','navigation','rebind','destroy','duplicate']) test(`text retirement action protects ${change} after the review was captured`,async()=>{
 const x=await textRetirementComposerFixture(),{ctx,f,scope,publication}=x;let entered,release;const ready=new Promise(resolve=>{entered=resolve;}),actual=f.actual.retirePeopleTextPublication;
 assert.equal(typeof actual,'function','future text retirement API is not implemented');f.store.retirePeopleTextPublication=async args=>{entered();await new Promise(resolve=>{release=resolve;});return actual(args);};
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(x.buttons().length,1);const button=x.buttons()[0],original=structuredClone(f.recovery()),originalFiles=JSON.stringify(f.files());button.click();await ready;if(change==='same'||change==='different'){ctx.feature.setPromptText(change==='same'?'saved text':'new input');ctx.roots.prompt.firstListener('input')();await localReviewTick();}if(change==='navigation'){ctx.setSel('other');ctx.feature.onSelect();ctx.feature.setPromptText('new view');}if(change==='rebind'){ctx.feature.destroy();ctx.feature.bind();ctx.feature.onSelect();}if(change==='destroy')ctx.feature.destroy();if(change==='duplicate')button.click();release();await localReviewTick();
  if(change==='same'||change==='different'){const text=change==='same'?'saved text':'new input';assert.equal(ctx.feature.promptText(),text);assert.ok(f.recovery().some(row=>row.kind==='draft'&&row.text===text));}
  if(change==='navigation')assert.equal(ctx.feature.promptText(),'new view');if(['navigation','rebind','destroy'].includes(change)){assert.deepEqual(f.recovery(),original);assert.equal(JSON.stringify(f.files()),originalFiles);}else assert.ok(f.recovery().some(row=>row.publication===publication));if(change==='duplicate')assert.equal(f.recovery().filter(row=>row.kind==='text-decision'&&row.publication===publication).length,1);assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);
 }finally{if(release)release();ctx.feature.destroy();}
});

for(const terminal of ['retired','completed']) test(`stale text retirement action observes already ${terminal} without clearing newer input`,async()=>{
 const x=await textRetirementComposerFixture(),{ctx,f,scope,publication,expected}=x;let entered,release;const ready=new Promise(resolve=>{entered=resolve;}),retire=f.actual.retirePeopleTextPublication;
 assert.equal(typeof retire,'function','future text retirement API is not implemented');f.store.retirePeopleTextPublication=async args=>{entered();await new Promise(resolve=>{release=resolve;});return retire(args);};
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(x.buttons().length,1);x.buttons()[0].click();await ready;ctx.feature.setPromptText('newer input');ctx.roots.prompt.firstListener('input')();await localReviewTick();const args={scope,publication,expected,current:()=>true};
  if(terminal==='retired')assert.equal((await retire(args)).state,'retired');else await f.actual.finishPeopleTextPublication({...args,seq:7});const before=JSON.stringify(f.recovery());release();await localReviewTick();assert.equal(JSON.stringify(f.recovery()),before);assert.equal(ctx.feature.promptText(),'newer input');const decision=f.recovery().find(row=>row.kind==='text-decision'&&row.publication===publication);assert.equal(decision.state,terminal);if(terminal==='completed')assert.equal(decision.seq,7);assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);
 }finally{if(release)release();ctx.feature.destroy();}
});

test('text retirement leaves owner prompt source association and agent draft unchanged',async()=>{
 const x=await textRetirementComposerFixture({mode:'owner'}),{ctx,f,publication}=x,source={server:'a'.repeat(32),owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),log:'comments',seq:1},association=JSON.stringify([source]);
 ctx.storage.setItem('scimux-prompt-sources:n1',association);ctx.storage.setItem('scimux-draft-owner:n1','native');
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setDestination('agent');ctx.feature.setPromptText('owner agent draft');ctx.roots.prompt.firstListener('input')();ctx.feature.setDestination('people');await localReviewTick();assert.equal(x.buttons().length,1);x.buttons()[0].click();await localReviewTick();assert.equal(f.recovery().find(row=>row.publication===publication).state,'retired');assert.equal(ctx.storage.getItem('scimux-prompt-sources:n1'),association);ctx.feature.setDestination('agent');assert.equal(ctx.feature.promptText(),'owner agent draft');assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);
 }finally{ctx.feature.destroy();}
});

test('text review action retires only its publication while another scope retains the same text and key',async()=>{
 const x=await textRetirementComposerFixture(),{ctx,f,scope,publication}=x,otherScope=scope+'other',otherDraft=await f.actual.savePeopleDraft({scope:otherScope,text:'saved text'}),other=await f.actual.beginPeopleTextPublication({scope:otherScope,peopleKey:x.context.key,copyInstance:'5'.repeat(32),binding:'other',native:'',...otherDraft,request:x.request});
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(x.buttons().length,1,'the matching text review action must be wired');x.buttons()[0].click();await localReviewTick();assert.equal(f.recovery().find(row=>row.publication===publication).state,'retired');assert.equal(f.recovery().find(row=>row.publication===other.publication).state,'pending');assert.ok(f.recovery().some(row=>row.key==='head:'+otherDraft.head&&row.scope===otherScope));assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

for(const change of ['navigation','rebind','destroy']) test(`committed text retirement cannot update ${change} view when its callback returns late`,async()=>{
 const x=await textRetirementComposerFixture(),{ctx,f,publication}=x,actual=f.actual.retirePeopleTextPublication;assert.equal(typeof actual,'function','future text retirement API is not implemented');let entered,release;const ready=new Promise(resolve=>{entered=resolve;});f.store.retirePeopleTextPublication=async args=>{const result=await actual(args);entered();await new Promise(resolve=>{release=resolve;});return result;};
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(x.buttons().length,1);x.buttons()[0].click();await ready;assert.equal(f.recovery().find(row=>row.publication===publication).state,'retired');if(change==='navigation'){ctx.setSel('other');ctx.feature.onSelect();ctx.feature.setPromptText('replacement view');}if(change==='rebind'){ctx.feature.destroy();ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('replacement view');}if(change==='destroy')ctx.feature.destroy();const before=JSON.stringify(f.recovery());release();await localReviewTick();assert.equal(JSON.stringify(f.recovery()),before);if(change!=='destroy')assert.equal(ctx.feature.promptText(),'replacement view');assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);
 }finally{if(release)release();ctx.feature.destroy();}
});

for(const failure of ['missing capability','throw','reject','unknown result']) test(`text retirement ${failure} keeps the exact publication and other durable rows`,async()=>{
 const x=await textRetirementComposerFixture(),{ctx,f,publication}=x;
 if(failure==='missing capability')delete f.store.retirePeopleTextPublication;
 if(failure==='throw')f.store.retirePeopleTextPublication=()=>{throw Error('synthetic storage failure');};
 if(failure==='reject')f.store.retirePeopleTextPublication=async()=>{throw Error('synthetic storage failure');};
 if(failure==='unknown result')f.store.retirePeopleTextPublication=async()=>({state:'unknown'});
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();const before=structuredClone(f.recovery()),files=JSON.stringify(f.files()),legacy=ctx.storage.getItem('synthetic-legacy-text');assert.equal(x.buttons().length,1);x.buttons()[0].click();await localReviewTick();assert.deepEqual(f.recovery(),before);assert.equal(f.recovery().find(row=>row.publication===publication).state,'pending');assert.equal(JSON.stringify(f.files()),files);assert.equal(ctx.storage.getItem('synthetic-legacy-text'),legacy);assert.match(ctx.roots.attstage.innerHTML,/unavailable|unconfirmed|could not|failed/i);assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

for(const outcome of ['success','failure']) test(`late ${outcome} after local text retirement cannot change its decision`,async()=>{
 let settle;const x=await textRetirementComposerFixture(),{ctx,f,scope,publication,expected}=x;
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();const response=new Promise((resolve,reject)=>{settle=()=>outcome==='success'?resolve({seq:7}):reject(Error('synthetic late failure'));});const flight=response.then(result=>f.actual.finishPeopleTextPublication({scope,publication,expected,seq:result.seq}));flight.catch(()=>{});assert.equal(x.buttons().length,1);x.buttons()[0].click();await localReviewTick();const decision=structuredClone(f.recovery().find(row=>row.publication===publication));assert.equal(decision.state,'retired');const before=structuredClone(f.recovery());settle();await assert.rejects(flight);assert.deepEqual(f.recovery(),before);assert.deepEqual(f.recovery().find(row=>row.publication===publication),decision);assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{if(settle)settle();ctx.feature.destroy();}
});

// The boundary is held around the real Store operation.  The composer supplies
// its own current predicate; the test changes the selected identity itself.
for(const field of ['server','owner','chat','generation','copyInstance','binding','native']) for(const order of ['before Store entry','after Store commit']) test(`text retirement ${order} survives ${field} replacement without affecting the replacement view`,async()=>{
 const x=await textRetirementComposerFixture({mode:field==='native'?'owner':'received'}),{ctx,f,context,publication}=x;
 const actual=f.actual.retirePeopleTextPublication;let entered,release,heldArgs,committed;
 const ready=new Promise(resolve=>{entered=resolve;}),gate=new Promise(resolve=>{release=resolve;});
 f.store.retirePeopleTextPublication=async args=>{
  heldArgs=structuredClone({...args,current:undefined});
  if(order==='before Store entry'){entered();await gate;return actual(args);}
  const result=await actual(args);committed=structuredClone(result);entered();await gate;return result;
 };
 const sourceKey='scimux-prompt-sources:'+(field==='native'?'n1':'team/'+context.key),source=JSON.stringify([{server:'a'.repeat(32),owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),log:'comments',seq:1}]);
 ctx.storage.setItem(sourceKey,source);if(field==='native')ctx.storage.setItem('scimux-draft-owner:n1','native');
 try{
  ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();
  const review=ctx.roots.attstage.innerHTML.match(/<details\b[^>]*>\s*<summary\b[^>]*>Review unconfirmed People text<\/summary>[\s\S]*?<\/details>/)?.[0];
  assert.ok(review,'original publication has its own text review');assert.match(review,/saved text/);assert.ok(review.includes(x.request.key));assert.match(review,/<button[^>]*>[^<]*(?:discard|retire)[^<]*<\/button>/i);assert.equal(x.buttons().length,1);
  const original=structuredClone(f.recovery().find(row=>row.kind==='text-publication'&&row.publication===publication));assert.ok(original);const originalRows=structuredClone(f.recovery().filter(row=>row.scope===original.scope&&row.publication!==publication)),originalFiles=JSON.stringify(f.files());
  x.buttons()[0].click();await ready;
  assert.equal(heldArgs.scope,original.scope);assert.equal(heldArgs.publication,publication);assert.deepEqual(heldArgs.expected,original);
  let decision;
  if(order==='after Store commit'){
   assert.equal(committed.state,'retired');decision=structuredClone(f.recovery().find(row=>row.publication===publication));
   assert.deepEqual(decision,{...original,key:'text-decision:'+publication,kind:'text-decision',state:'retired'});
  }else assert.deepEqual(f.recovery().find(row=>row.publication===publication),original);
  if(['server','owner','chat','generation'].includes(field)){
   const parts=context.key.split('/'),index=['server','owner','chat','generation'].indexOf(field);parts[index]='f'.repeat(parts[index].length);context.key=parts.join('/');ctx.setSel('team/'+context.key);
  }else if(field==='copyInstance')context.copyInstance='5'.repeat(32);
  else if(field==='binding')context.binding='replacement';
  else {const node=ctx.nodes.find(item=>item.id==='n1');assert.ok(node);assert.equal(node.instance_id,original.native);node.instance_id='replacement-native';}
  ctx.feature.contextChanged();ctx.feature.onSelect();await localReviewTick();
  if(field!=='native'){ctx.feature.setDestination('agent');assert.equal(ctx.feature.destination(),'people','a recipient cannot acquire Agent controls');}
  ctx.feature.setPromptText('replacement view input');ctx.roots.prompt.firstListener('input')();await localReviewTick();
  const before=structuredClone(f.recovery()),files=JSON.stringify(f.files()),view={text:ctx.feature.promptText(),destination:ctx.feature.destination(),stage:ctx.roots.attstage.innerHTML,hidden:ctx.roots.attstage.hidden,alerts:[...ctx.alerts],source:ctx.storage.getItem(sourceKey),retired:readRetiredDrafts(ctx.storage,'n1'),legacy:ctx.storage.getItem('synthetic-legacy-text')};
  assert.equal(view.text,'replacement view input');if(field==='native'){assert.notEqual(view.source,source);assert.ok(readRetiredDrafts(ctx.storage,'n1').some(item=>item.owner==='native'&&item.values['scimux-prompt-sources:']===source));}else assert.equal(view.source,source);assert.equal(view.legacy,'unchanged');assert.equal(files,originalFiles);for(const oldRow of originalRows)assert.deepEqual(before.find(row=>row.key===oldRow.key),oldRow,'old heads and drafts remain exact');
  if(order==='before Store entry')assert.deepEqual(before.find(row=>row.publication===publication),original);
  else assert.deepEqual(before.find(row=>row.publication===publication),decision);
  release();await localReviewTick();
  assert.deepEqual(f.recovery(),before,'the old callback cannot alter any durable row after replacement');assert.equal(JSON.stringify(f.files()),files);assert.equal(ctx.feature.promptText(),view.text);assert.equal(ctx.feature.destination(),view.destination);assert.equal(ctx.roots.attstage.innerHTML,view.stage);assert.equal(ctx.roots.attstage.hidden,view.hidden);assert.deepEqual(ctx.alerts,view.alerts);assert.equal(ctx.storage.getItem(sourceKey),view.source);assert.deepEqual(readRetiredDrafts(ctx.storage,'n1'),view.retired);assert.equal(ctx.storage.getItem('synthetic-legacy-text'),view.legacy);assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);assert.equal(ctx.fetchCalls.length,0);
 }finally{release();ctx.feature.destroy();}
});

function textAction(x, publication){
 const buttons=x.buttons().filter(button=>button.dataset.publication===publication);
 assert.equal(buttons.length,1,'one action must identify this immutable publication');
 assert.ok(!Object.values(buttons[0].dataset).some(value=>value.includes('\0')),'scope bytes must not be placed in attributes');
 return buttons[0];
}
async function addRetainedText(x,{binding='earlier',text='saved text',key='6'.repeat(16)}={}){
 const {f,context}=x,native=context.owner?'native':'',scope=`people\0${context.key}\0binding:${binding}${context.copyInstance?'\0copy:'+context.copyInstance:''}\0${context.owner?'n1\0'+native:''}`;
 const draft=await f.actual.savePeopleDraft({scope,text}),request={key,text,anchor:0},saved=await f.actual.beginPeopleTextPublication({scope,peopleKey:context.key,copyInstance:context.copyInstance,binding,native,...draft,request});
 return {scope,draft,request,publication:saved.publication};
}

test('current and retained references to one text publication share one review and action',async()=>{
 const x=await textRetirementComposerFixture(),{ctx,f,publication}=x;
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();const pending=structuredClone(f.recovery().find(row=>row.publication===publication)),before=structuredClone(f.recovery());assert.equal(x.reviews().length,1);assert.ok(x.textReads.current>0);assert.ok(x.textReads.retained>0);assert.match(x.reviews()[0],/saved text/);assert.ok(x.reviews()[0].includes(x.request.key));assert.equal(x.buttons().length,1);textAction(x,publication).click();await localReviewTick();assert.deepEqual(f.recovery().find(row=>row.publication===publication),{...pending,key:'text-decision:'+publication,kind:'text-decision',state:'retired'});assert.deepEqual(f.recovery().filter(row=>row.publication!==publication),before.filter(row=>row.publication!==publication));assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('equal retained texts with different publications retain separate opaque actions',async()=>{
 const x=await textRetirementComposerFixture({mode:'owner-ended-prior'}),{ctx,f,publication}=x,other=await addRetainedText(x),before=structuredClone(f.recovery()),files=JSON.stringify(f.files());
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(x.reviews().length,2);assert.equal(x.reviews().filter(review=>review.includes('saved text')).length,2);assert.ok(x.reviews().some(review=>review.includes(x.request.key)));assert.ok(x.reviews().some(review=>review.includes(other.request.key)));assert.equal(x.buttons().length,2);assert.notEqual(publication,other.publication);textAction(x,publication).click();await localReviewTick();const old=before.find(row=>row.publication===publication);assert.deepEqual(f.recovery().find(row=>row.publication===publication),{...old,key:'text-decision:'+publication,kind:'text-decision',state:'retired'});assert.deepEqual(f.recovery().find(row=>row.publication===other.publication),before.find(row=>row.publication===other.publication));assert.deepEqual(f.recovery().filter(row=>row.publication!==publication),before.filter(row=>row.publication!==publication));assert.equal(JSON.stringify(f.files()),files);assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('late committed retirement preserves a newly refreshed publication in the same view',async()=>{
 const x=await textRetirementComposerFixture(),{ctx,f,scope,publication,context}=x,actual=f.actual.retirePeopleTextPublication;let entered,release;const ready=new Promise(resolve=>{entered=resolve;}),gate=new Promise(resolve=>{release=resolve;});
 f.store.retirePeopleTextPublication=async args=>{const result=await actual(args);entered();await gate;return result;};
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();ctx.storage.setItem('scimux-prompt-sources:team/'+context.key,'synthetic association');textAction(x,publication).click();await ready;const decision=structuredClone(f.recovery().find(row=>row.publication===publication));assert.equal(decision.state,'retired');ctx.feature.setPromptText('new input');ctx.roots.prompt.firstListener('input')();await localReviewTick();const draft=await f.actual.savePeopleDraft({scope,text:'new pending'}),request={key:'7'.repeat(16),text:'new pending',anchor:0},newer=await f.actual.beginPeopleTextPublication({scope,peopleKey:context.key,copyInstance:context.copyInstance,binding:context.binding,native:'',...draft,request});ctx.feature.renderStage();await localReviewTick();assert.ok(x.reviews().some(review=>review.includes('new pending')&&review.includes(request.key)));textAction(x,newer.publication);const before=structuredClone(f.recovery()),files=JSON.stringify(f.files()),destination=ctx.feature.destination(),source=ctx.storage.getItem('scimux-prompt-sources:team/'+context.key);assert.equal(source,'synthetic association');release();await localReviewTick();assert.deepEqual(f.recovery(),before);assert.deepEqual(f.recovery().find(row=>row.publication===publication),decision);assert.equal(ctx.feature.promptText(),'new input');assert.equal(ctx.feature.destination(),destination);assert.equal(ctx.storage.getItem('scimux-prompt-sources:team/'+context.key),source);assert.equal(JSON.stringify(f.files()),files);assert.ok(x.reviews().some(review=>review.includes('new pending')&&review.includes(request.key)));textAction(x,newer.publication);assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{release();ctx.feature.destroy();}
});

test('owner switches People and Agent drafts and retires text from Agent with staged files intact',async()=>{
 const x=await textRetirementComposerFixture({mode:'owner'}),{ctx,f,publication}=x,source=JSON.stringify([{server:'a'.repeat(32),owner:'b'.repeat(32),chat:'c'.repeat(16),generation:'d'.repeat(16),log:'comments',seq:1}]);ctx.storage.setItem('scimux-draft-owner:n1','native');ctx.storage.setItem('scimux-prompt-sources:n1',source);
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('People draft');ctx.roots.prompt.firstListener('input')();await localReviewTick();ctx.feature.setDestination('agent');ctx.feature.setAttachAvail(true);ctx.feature.setPromptText('Agent draft');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'agent.txt',type:'text/plain',size:1}]);await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/agent\.txt/);ctx.feature.setDestination('people');assert.equal(ctx.feature.promptText(),'People draft');ctx.feature.setDestination('agent');await localReviewTick();assert.equal(ctx.feature.promptText(),'Agent draft');assert.match(ctx.roots.attstage.innerHTML,/agent\.txt/);const before=structuredClone(f.recovery()),files=JSON.stringify(f.files()),fetchCount=ctx.fetchCalls.length;const original=before.find(row=>row.publication===publication);textAction(x,publication).click();await localReviewTick();assert.deepEqual(f.recovery().find(row=>row.publication===publication),{...original,key:'text-decision:'+publication,kind:'text-decision',state:'retired'});assert.deepEqual(f.recovery().filter(row=>row.publication!==publication),before.filter(row=>row.publication!==publication));assert.equal(JSON.stringify(f.files()),files);assert.equal(ctx.feature.destination(),'agent');assert.equal(ctx.feature.promptText(),'Agent draft');assert.match(ctx.roots.attstage.innerHTML,/agent\.txt/);assert.equal(ctx.storage.getItem('scimux-prompt-sources:n1'),source);assert.equal(ctx.fetchCalls.length,fetchCount);assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

for(const variant of ['offline inventory','empty copy instance']) test(`live owner retains old-binding text review with ${variant}`,async()=>{
 const empty=variant==='empty copy instance',x=await textRetirementComposerFixture({mode:'owner-ended-prior',copyInstance:empty?'':'1'.repeat(32),offline:!empty}),{ctx,f,publication,context}=x;ctx.storage.setItem('scimux-draft-owner:n1','native');
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(ctx.nodes[0].ended_at,undefined);assert.equal(ctx.feature.destination(),'agent');assert.equal(context.binding,'replacement');const original=structuredClone(f.recovery().find(row=>row.publication===publication)),before=structuredClone(f.recovery()),files=JSON.stringify(f.files()),legacy=ctx.storage.getItem('synthetic-legacy-text');assert.equal(original.binding,'old');if(empty){assert.equal(original.copyInstance,'');assert.ok(!original.scope.includes('\0copy:'));}else assert.ok(x.localRequests.some(call=>call.method==='GET'));assert.match(ctx.roots.attstage.innerHTML,/unavailable|could not|failed/i,'file recovery limitation is reported');assert.equal(x.reviews().length,1);assert.match(x.reviews()[0],/saved text/);textAction(x,publication).click();await localReviewTick();assert.deepEqual(f.recovery().find(row=>row.publication===publication),{...original,key:'text-decision:'+publication,kind:'text-decision',state:'retired'});assert.deepEqual(f.recovery().filter(row=>row.publication!==publication),before.filter(row=>row.publication!==publication));assert.equal(JSON.stringify(f.files()),files);assert.equal(ctx.storage.getItem('synthetic-legacy-text'),legacy);assert.ok(x.localRequests.every(call=>call.method==='GET'));assert.equal(ctx.feature.destination(),'agent');assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('text retirement uses a native button and scoped status during pending work and errors',async()=>{
 const x=await textRetirementComposerFixture(),{ctx,f,publication}=x,actual=f.actual.retirePeopleTextPublication;let entered,release;const ready=new Promise(resolve=>{entered=resolve;}),gate=new Promise(resolve=>{release=resolve;});f.store.retirePeopleTextPublication=async args=>{entered();await gate;throw Error('synthetic failure');};
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();const action=textAction(x,publication);assert.equal(action.tagName,'BUTTON');assert.equal(action.getAttribute('type'),'button');const busy=ctx.roots.promptbar.classList.contains('busy'),sendDisabled=ctx.roots.sendbtn.disabled;action.click();await ready;await localReviewTick();assert.match(x.reviews()[0],/role="status"/);assert.match(x.reviews()[0],/retir|discard|saving|progress/i);assert.equal(ctx.roots.promptbar.classList.contains('busy'),busy);assert.equal(ctx.roots.sendbtn.disabled,sendDisabled);release();await localReviewTick();assert.match(x.reviews()[0],/role="status"/);assert.match(x.reviews()[0],/unavailable|failed|could not/i);assert.equal(f.recovery().find(row=>row.publication===publication).state,'pending');assert.equal(ctx.roots.promptbar.classList.contains('busy'),busy);assert.equal(ctx.roots.sendbtn.disabled,sendDisabled);assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{release();f.store.retirePeopleTextPublication=actual;ctx.feature.destroy();}
});

for(const loss of ['rerender','navigation','destroy']) test(`retired text action listener cannot act after ${loss} and a fresh action works`,async()=>{
 const x=await textRetirementComposerFixture(),{ctx,f,publication,context}=x,actual=f.actual.retirePeopleTextPublication;let calls=0;f.store.retirePeopleTextPublication=args=>{calls++;return actual(args);};
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();const old=textAction(x,publication),handler=old.firstListener('click');assert.equal(typeof handler,'function');assert.equal(old.listenerCount('click'),1);
  if(loss==='rerender')ctx.feature.renderStage();else if(loss==='navigation'){ctx.setSel('other');ctx.feature.onSelect();ctx.feature.setPromptText('other view');ctx.roots.prompt.firstListener('input')();}else ctx.feature.destroy();await localReviewTick();
  const before=structuredClone(f.recovery()),files=JSON.stringify(f.files()),view=ctx.feature.promptText(),destination=ctx.feature.destination(),stage=ctx.roots.attstage.innerHTML;assert.equal(old.listenerCount('click'),0);old.click();handler({type:'click',target:old,preventDefault(){},stopPropagation(){}});await localReviewTick();assert.equal(calls,0);assert.deepEqual(f.recovery(),before);assert.equal(JSON.stringify(f.files()),files);assert.equal(ctx.feature.promptText(),view);assert.equal(ctx.feature.destination(),destination);assert.equal(ctx.roots.attstage.innerHTML,stage);
  if(loss==='navigation'){ctx.setSel('team/'+context.key);ctx.feature.onSelect();}if(loss==='destroy'){ctx.feature.bind();ctx.feature.onSelect();}await localReviewTick();const fresh=textAction(x,publication);assert.notEqual(fresh,old);fresh.click();await localReviewTick();assert.equal(calls,1);assert.equal(f.recovery().find(row=>row.publication===publication).state,'retired');assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);
 }finally{ctx.feature.destroy();}
});

test('retained text enumeration failure leaves the current publication actionable without network effects',async()=>{
 const x=await textRetirementComposerFixture({depsExtra:{readRetainedPeopleTextPending:()=>{throw Error('synthetic lookup failure');}}}),{ctx,f,publication}=x;
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(x.reviews().length,1);const original=structuredClone(f.recovery().find(row=>row.publication===publication));textAction(x,publication).click();await localReviewTick();assert.deepEqual(f.recovery().find(row=>row.publication===publication),{...original,key:'text-decision:'+publication,kind:'text-decision',state:'retired'});assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);assert.ok(x.localRequests.every(call=>call.method==='GET'));}finally{ctx.feature.destroy();}
});

test('missing text retirement adapter preserves the exact review and all durable data',async()=>{
 const x=await textRetirementComposerFixture({depsExtra:{retirePeopleText:undefined}}),{ctx,f,publication}=x;let calls=0;const actual=f.store.retirePeopleTextPublication;f.store.retirePeopleTextPublication=(...args)=>{calls++;return actual(...args);};
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();const before=structuredClone(f.recovery()),files=JSON.stringify(f.files());textAction(x,publication).click();await localReviewTick();assert.equal(calls,0);assert.deepEqual(f.recovery(),before);assert.equal(JSON.stringify(f.files()),files);assert.equal(ctx.storage.getItem('synthetic-legacy-text'),'unchanged');assert.match(x.reviews()[0],/unavailable/i);assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

for(const variant of ['null record','missing request','bad text','bad request key type','missing publication','bad publication','bad scope','bad head','bad version','bad binding','bad native','wrong key','wrong copy','wrong native','wrong scope','bad request key','bad anchor']) test(`malformed retained text ${variant} cannot expose a retirement action`,async()=>{
 let retained=[];const x=await textRetirementComposerFixture({depsExtra:{pendingPeopleText:()=>null,readRetainedPeopleTextPending:()=>({entries:retained,unavailable:false})}}),{ctx,f,publication}=x;let calls=0;const actual=f.store.retirePeopleTextPublication;f.store.retirePeopleTextPublication=(...args)=>{calls++;return actual(...args);};
 let row=structuredClone(f.recovery().find(item=>item.publication===publication));
 if(variant==='null record')row=null;else if(variant==='missing request')delete row.request;else if(variant==='bad text')row.request.text=4;else if(variant==='bad request key type')row.request.key=4;else if(variant==='missing publication')delete row.publication;else if(variant==='bad publication')row.publication='invalid';else if(variant==='bad scope')row.scope=null;else if(variant==='bad head')row.head=null;else if(variant==='bad version')row.version=null;else if(variant==='bad binding')row.binding=null;else if(variant==='bad native')row.native=null;else if(variant==='wrong key')row.peopleKey='other';else if(variant==='wrong copy')row.copyInstance='5'.repeat(32);else if(variant==='wrong native')row.native='other';else if(variant==='wrong scope')row.scope+='other';else if(variant==='bad request key')row.request.key='wrong';else row.request.anchor='wrong';retained=[row];
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('newer local input');ctx.roots.prompt.firstListener('input')();await localReviewTick();const before=structuredClone(f.recovery()),files=JSON.stringify(f.files());assert.equal(x.buttons().length,0);assert.equal(x.reviews().length,0);ctx.feature.renderStage();await localReviewTick();assert.equal(calls,0);assert.deepEqual(f.recovery(),before);assert.equal(JSON.stringify(f.files()),files);assert.equal(ctx.feature.promptText(),'newer local input');assert.equal(ctx.storage.getItem('synthetic-legacy-text'),'unchanged');assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('missing current native identity cannot authorize another native text review',async()=>{
 let retained=[];const x=await textRetirementComposerFixture({mode:'owner',depsExtra:{pendingPeopleText:()=>null,readRetainedPeopleTextPending:()=>({entries:retained,unavailable:false})}}),{ctx,f,publication}=x;retained=[structuredClone(f.recovery().find(row=>row.publication===publication))];retained[0].native='';ctx.nodes[0].instance_id='';let calls=0;const actual=f.store.retirePeopleTextPublication;f.store.retirePeopleTextPublication=(...args)=>{calls++;return actual(...args);};
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();const before=structuredClone(f.recovery()),files=JSON.stringify(f.files());assert.equal(x.buttons().length,0);assert.equal(calls,0);assert.deepEqual(f.recovery(),before);assert.equal(JSON.stringify(f.files()),files);assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('a still-attached stale text action cannot enter the Store after selection changes',async()=>{
 const x=await textRetirementComposerFixture(),{ctx,f,publication}=x;let calls=0;const actual=f.store.retirePeopleTextPublication;f.store.retirePeopleTextPublication=(...args)=>{calls++;return actual(...args);};
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();const action=textAction(x,publication),before=structuredClone(f.recovery()),files=JSON.stringify(f.files());ctx.setSel('other');action.click();await localReviewTick();assert.equal(calls,0);assert.deepEqual(f.recovery(),before);assert.equal(JSON.stringify(f.files()),files);assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('a newly rendered duplicate action cannot start a second pending text retirement',async()=>{
 const x=await textRetirementComposerFixture(),{ctx,f,publication}=x,actual=f.actual.retirePeopleTextPublication;let calls=0,entered,release;const ready=new Promise(resolve=>{entered=resolve;}),gate=new Promise(resolve=>{release=resolve;});f.store.retirePeopleTextPublication=async args=>{calls++;entered();await gate;return actual(args);};
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();textAction(x,publication).click();await ready;const newer=textAction(x,publication);newer.click();await localReviewTick();assert.equal(calls,1);assert.equal(f.recovery().find(row=>row.publication===publication).state,'pending');release();await localReviewTick();assert.equal(f.recovery().find(row=>row.publication===publication).state,'retired');assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{release();ctx.feature.destroy();}
});

test('text disclosure toggle and rerender preserve its open state and remove old listeners',async()=>{
 const x=await textRetirementComposerFixture(),{ctx,f,publication}=x;
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();const detail=x.details()[0];assert.equal(detail.open,false);assert.equal(detail.listenerCount('toggle'),1);detail.open=true;detail.firstListener('toggle')();ctx.feature.renderStage();await localReviewTick();assert.equal(detail.listenerCount('toggle'),0);assert.equal(x.details()[0].open,true);assert.match(x.reviews()[0],/^<details\b[^>]*\sopen(?:\s|>)/);const current=x.details()[0];current.open=false;current.firstListener('toggle')();ctx.feature.renderStage();assert.equal(current.listenerCount('toggle'),0);assert.equal(x.details()[0].open,false);textAction(x,publication).click();await localReviewTick();assert.equal(f.recovery().find(row=>row.publication===publication).state,'retired');assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

for(const kind of ['wrong row','wrong publication','wrong detail row']) test(`mismatched text ${kind} receives no listener and cannot mutate data`,async()=>{
 const x=await textRetirementComposerFixture(),{ctx,f,publication}=x;const fake=kind==='wrong detail row'?el('details',{dataset:{row:'99'}}):el('button',{dataset:{row:kind==='wrong row'?'99':'0',publication:kind==='wrong publication'?'other':publication}});if(kind==='wrong detail row')x.injected.details.push(fake);else x.injected.buttons.push(fake);let calls=0;const actual=f.store.retirePeopleTextPublication;f.store.retirePeopleTextPublication=(...args)=>{calls++;return actual(...args);};
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();const before=structuredClone(f.recovery()),files=JSON.stringify(f.files());assert.equal(fake.listenerCount(kind==='wrong detail row'?'toggle':'click'),0);fake.click();await localReviewTick();assert.equal(calls,0);assert.deepEqual(f.recovery(),before);assert.equal(JSON.stringify(f.files()),files);textAction(x,publication);assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

for(const change of ['navigation','destroy']) test(`text disclosure removes its toggle listener on ${change}`,async()=>{
 const x=await textRetirementComposerFixture(),{ctx,f}=x;
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();const detail=x.details()[0];assert.equal(detail.listenerCount('toggle'),1);detail.open=true;detail.firstListener('toggle')();if(change==='navigation'){ctx.setSel('other');ctx.feature.onSelect();ctx.feature.setPromptText('different view');ctx.roots.prompt.firstListener('input')();}else ctx.feature.destroy();await localReviewTick();const before=structuredClone(f.recovery()),files=JSON.stringify(f.files()),view=ctx.feature.promptText();assert.equal(detail.listenerCount('toggle'),0);assert.deepEqual(f.recovery(),before);assert.equal(JSON.stringify(f.files()),files);assert.equal(ctx.feature.promptText(),view);assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('reviewed immutable text changed before Store entry is refused without touching newer data',async()=>{
 const x=await textRetirementComposerFixture(),{ctx,f,publication}=x,actual=f.actual.retirePeopleTextPublication;let entered,release;const ready=new Promise(resolve=>{entered=resolve;}),gate=new Promise(resolve=>{release=resolve;});f.store.retirePeopleTextPublication=async args=>{entered();await gate;return actual(args);};
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();textAction(x,publication).click();await ready;await f.driver.writePeople(({files,recovery})=>({files,recovery:recovery.map(row=>row.publication===publication?{...row,request:{...row.request,text:'different immutable request'}}:row),value:null}));ctx.feature.setPromptText('newer input');ctx.roots.prompt.firstListener('input')();await localReviewTick();const before=structuredClone(f.recovery()),files=JSON.stringify(f.files());release();await localReviewTick();assert.deepEqual(f.recovery(),before);assert.equal(f.recovery().find(row=>row.publication===publication).state,'pending');assert.equal(JSON.stringify(f.files()),files);assert.equal(ctx.feature.promptText(),'newer input');assert.equal(ctx.storage.getItem('synthetic-legacy-text'),'unchanged');assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{release();ctx.feature.destroy();}
});

async function capturedDiscardCoverageFixture(){
 const context={key:localReviewKey,copyInstance:'1'.repeat(32),binding:'current',owner:false,writable:true,attachments:false},scope=`people\0${context.key}\0binding:current\0copy:${context.copyInstance}\0`,f=transactionalPeopleFixture({context}),ctx=f.ctx,draft=await f.actual.savePeopleDraft({scope,text:'saved capture'}),capture=await f.actual.freezePeopleDraft({scope,text:'saved capture',...draft});await f.actual.save(scope,'3'.repeat(16),{name:'kept.txt',type:'text/plain',size:1});const old=ctx.roots.attstage.querySelectorAll;let buttons=[];
 ctx.roots.attstage.querySelectorAll=selector=>selector==='.people-captured-discard'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-captured-discard(?:\s+[^"\s]+)*" data-head="([^"]+)" data-version="([^"]+)"/g)].map(match=>el('button',{dataset:{head:match[1],version:match[2]}}))):old(selector);
 return {f,ctx,scope,draft,capture,buttons:()=>buttons};
}

for(const failure of ['missing capability','storage rejection','changed reviewed draft']) test(`captured draft ${failure} keeps local records and shows a truthful error`,async()=>{
 const x=await capturedDiscardCoverageFixture(),{f,ctx,capture}=x;
 if(failure==='missing capability')delete f.store.discardPeopleCapture;
 if(failure==='storage rejection')f.store.discardPeopleCapture=async()=>{throw Error('synthetic storage failure');};
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(x.buttons().length,1);if(failure==='changed reviewed draft')await f.driver.writePeople(({files,recovery})=>({files,recovery:recovery.map(row=>row.key==='draft:'+capture.version?{...row,text:'changed outside view'}:row),value:null}));const before=structuredClone(f.recovery()),files=JSON.stringify(f.files());x.buttons()[0].click();await localReviewTick();assert.deepEqual(f.recovery(),before);assert.equal(JSON.stringify(f.files()),files);assert.match(ctx.roots.attstage.innerHTML,/could not be discarded/i);assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('captured draft double activation invokes only one pending Store operation',async()=>{
 const x=await capturedDiscardCoverageFixture(),{f,ctx,capture}=x,actual=f.actual.discardPeopleCapture;let release,entered,calls=0;const gate=new Promise(resolve=>{release=resolve;}),ready=new Promise(resolve=>{entered=resolve;});f.store.discardPeopleCapture=async args=>{calls++;entered();await gate;return actual(args);};
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();const button=x.buttons()[0];button.click();await ready;button.click();await localReviewTick();assert.equal(calls,1);assert.ok(f.recovery().some(row=>row.key==='head:'+capture.head));release();await localReviewTick();assert.ok(!f.recovery().some(row=>row.key==='head:'+capture.head));assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{release();ctx.feature.destroy();}
});

test('captured draft stale click after selection loss never enters the Store',async()=>{
 const x=await capturedDiscardCoverageFixture(),{f,ctx}=x,actual=f.actual.discardPeopleCapture;let calls=0;f.store.discardPeopleCapture=(...args)=>{calls++;return actual(...args);};
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();const button=x.buttons()[0],before=structuredClone(f.recovery()),files=JSON.stringify(f.files());ctx.setSel('other');button.click();await localReviewTick();assert.equal(calls,0);assert.deepEqual(f.recovery(),before);assert.equal(JSON.stringify(f.files()),files);assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('saved retry remains reviewable when stage DOM queries are unavailable',async()=>{
 const f=browserRetireFixture({state:'missing'}),{ctx}=f;
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();const before=f.raw();ctx.roots.attstage.querySelectorAll=undefined;ctx.feature.renderStage();assert.match(ctx.roots.attstage.innerHTML,/retained synthetic/);assert.equal(f.raw(),before);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('invalid current retry disclosure row cannot attach a toggle listener',async()=>{
 const f=browserRetireFixture({state:'missing'}),{ctx}=f,old=ctx.roots.attstage.querySelectorAll,bad=el('details',{dataset:{row:'999'}});
 ctx.roots.attstage.querySelectorAll=selector=>selector==='.people-current-snapshot'?[bad]:old(selector);
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();const raw=f.raw();assert.equal(bad.listenerCount('toggle'),0);bad.open=true;ctx.feature.renderStage();assert.equal(bad.listenerCount('toggle'),0);assert.equal(f.raw(),raw);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('invalid captured-draft action identity cannot attach a discard listener',async()=>{
 const x=await capturedDiscardCoverageFixture(),{ctx,f}=x,old=ctx.roots.attstage.querySelectorAll,bad=el('button',{dataset:{head:'not-a-head',version:'not-a-version'}});ctx.roots.attstage.querySelectorAll=selector=>selector==='.people-captured-discard'?[...old(selector),bad]:old(selector);
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();const before=structuredClone(f.recovery()),files=JSON.stringify(f.files());assert.equal(bad.listenerCount('click'),0);bad.click();await localReviewTick();assert.deepEqual(f.recovery(),before);assert.equal(JSON.stringify(f.files()),files);assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('captured draft remains reviewable if stage DOM queries are unavailable',async()=>{
 const x=await capturedDiscardCoverageFixture(),{ctx,f,capture}=x;
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(x.buttons().length,1);const before=structuredClone(f.recovery()),files=JSON.stringify(f.files());ctx.roots.attstage.querySelectorAll=undefined;ctx.feature.renderStage();assert.match(ctx.roots.attstage.innerHTML,/Captured submission/);assert.ok(f.recovery().some(row=>row.key==='head:'+capture.head));assert.deepEqual(f.recovery(),before);assert.equal(JSON.stringify(f.files()),files);assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

for (const [html, expected] of [
 ['<button class="people-retry"></button>',1],
 ['<button class="people-retry btn-tinted"></button>',1],
 ['<button class="btn-tinted people-retry"></button>',1],
 ['<button class="  btn-tinted\tpeople-retry\n "></button>',1],
 ['<button></button>',0],
 ['<button class="people-retry-other"></button>',0],
 ['<button class="other-people-retry"></button>',0],
 ['<button class="btn-tinted"></button>',0],
]) test(`fake retry selection respects class tokens: ${JSON.stringify(html)}`,()=>{
 const stage=makeRoots().attstage;stage.innerHTML=html;
 assert.equal(stage.querySelectorAll('.people-retry').length,expected);
});

test('saved textarea text resembling a retry button is not a rendered retry action',()=>{
 const stage=makeRoots().attstage;
 stage.innerHTML='<textarea readonly>&lt;button class="people-retry"&gt;literal&lt;/button&gt;</textarea>';
 assert.equal(stage.querySelectorAll('.people-retry').length,0);
 assert.equal(renderedClassCount(stage.innerHTML,'people-retry'),0);
});

test('retry fixture ignores raw textarea content and data-class attributes',()=>{
 const stage=makeRoots().attstage;
 stage.innerHTML='<textarea><button class="people-retry">literal</button></textarea><button data-class="people-retry"></button>';
 assert.equal(stage.querySelectorAll('.people-retry').length,0);
 assert.equal(renderedClassCount(stage.innerHTML,'people-retry'),0);
});

test('a captured retry refuses dispatch when pending storage becomes unavailable',async()=>{
 const f=browserRetireFixture({state:'pending'}),{ctx}=f;
 try{
  ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();
  const button=ctx.roots.attstage._retryButtons[0];assert.ok(button);const before=f.raw(),events=[...f.events];
  f.setLookupThrow(true);button.click();await localReviewTick();
  assert.equal(f.retries(),0);assert.equal(f.raw(),before);assert.deepEqual(f.events,events);assert.equal(ctx.apiCalls.length,0);
 }finally{ctx.feature.destroy();}
});

test('a captured retry refuses dispatch after its pending record becomes locally retired',async()=>{
 const record=retainedBrowserSnapshot({instance:'',binding:'current'}),context={key:record.key,copyInstance:record.copyInstance,binding:'current',owner:false,writable:true};let retired=false,retries=0;
 const ctx=makeFeature({sel:'team/'+record.key,nodes:[],deps:{peopleContext:()=>context,pendingPeople:()=>({...record,locallyRetired:retired}),retryPeople:async()=>{retries++;return {seq:1};}}});
 try{
  ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();const button=ctx.roots.attstage._retryButtons[0];assert.ok(button);
  retired=true;button.click();await localReviewTick();assert.equal(retries,0);assert.equal(ctx.apiCalls.length,0);assert.equal(record.text,'retained synthetic');
 }finally{ctx.feature.destroy();}
});

test('unavailable saved-file review prevents retirement without touching the retry',async()=>{
 const record=retainedBrowserSnapshot({instance:'',binding:'current'}),events=[];let raw=record.value;
 const ctx=makeFeature({sel:'team/'+record.key,nodes:[],deps:{peopleContext:()=>({key:record.key,copyInstance:record.copyInstance,binding:'current',owner:false,writable:false}),peopleFileStore:{},pendingPeople:()=>({...record,value:raw}),localReviewApi:async(path,opts)=>{events.push(opts.method);return path.includes('/pending/')?{state:'missing'}:{entries:[]};},fencePeoplePending:()=>assert.fail('must not fence'),clearPeoplePending:()=>assert.fail('must not clear')}});
 const old=ctx.roots.attstage.querySelectorAll;let buttons=[];
 ctx.roots.attstage.querySelectorAll=s=>s==='.people-browser-retire'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-browser-retire(?:\s+[^"\s]+)*" data-row="(\d+)" data-snapshot="([^"]+)"/g)].map(m=>el('button',{dataset:{row:m[1],snapshot:m[2]}}))):old(s);
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(buttons.length,1);buttons[0].click();await localReviewTick();assert.equal(raw,record.value);assert.deepEqual(events,['GET','GET']);assert.match(ctx.roots.attstage.innerHTML,/cleanup is unconfirmed/i);assert.equal(ctx.apiCalls.length,0);}finally{ctx.feature.destroy();}
});

test('legacy retry receipt settling after composer teardown cannot alter the rebound draft',async()=>{
 const key=localReviewKey,copyInstance='1'.repeat(32),context={key,copyInstance,binding:'current',owner:false,writable:true,attachments:false};
 const pending={key,text:'saved legacy text',attachments:[],value:JSON.stringify({request:{key:'2'.repeat(16),text:'saved legacy text',anchor:0,attachments:[]}})};
 let release,entered,retries=0,finishes=0;const ready=new Promise(resolve=>{entered=resolve;}),gate=new Promise(resolve=>{release=resolve;});
 const ctx=makeFeature({sel:'team/'+key,nodes:[],deps:{peopleContext:()=>context,pendingPeople:()=>pending,retryPeople:async()=>{retries++;return {seq:7};},finishPeople:async()=>{finishes++;entered();await gate;return {state:'completed',seq:7};}}});
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();const button=ctx.roots.attstage._retryButtons[0];assert.ok(button);button.click();await ready;ctx.feature.destroy();ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('new input');ctx.roots.prompt.firstListener('input')();release();await localReviewTick();assert.equal(retries,1);assert.equal(finishes,1);assert.equal(ctx.feature.promptText(),'new input');assert.equal(pending.text,'saved legacy text');assert.equal(ctx.apiCalls.length,0);assert.deepEqual(ctx.alerts,[]);}finally{release();ctx.feature.destroy();}
});

test('confirmed retry preserves a replacement file when its captured file identity changed',async()=>{
 let attempts=0;const f=transactionalPeopleFixture({respond:async()=>{if(++attempts===1)throw Error('synthetic uncertain response');return {seq:7};}}),ctx=f.ctx;
 try{
  ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('saved caption');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await f.waitReady(1);await ctx.feature.sendPrompt();
  const original=f.files()[0],pending=f.pending();assert.ok(pending);await f.actual.remove(original.scope,original.id);const replacement=await f.actual.save(original.scope,original.id,{name:'replacement.txt',type:'text/plain',size:2});await f.actual.setStageRevision(original.scope,original.id,original.stageRevision,replacement);
  ctx.roots.attstage._retryButtons[0].click();await f.waitRetry(1);await localReviewTick();
  const decision=f.recovery().find(row=>row.publication===pending.publication);assert.equal(decision.state,'completed');assert.equal(decision.filesConfirmed,false);const rows=await f.actual.list(original.scope);assert.equal(rows.length,1);assert.equal(rows[0].fileToken,replacement);assert.equal(rows[0].published,undefined);assert.equal(ctx.feature.promptText(),'saved caption');assert.equal(ctx.apiCalls.filter(c=>c.opts?.method==='DELETE').length,0);
 }finally{ctx.feature.destroy();}
});

test('a People send waiting for draft persistence stops when its composer is destroyed',async()=>{
 const f=transactionalPeopleFixture(),ctx=f.ctx,save=f.store.savePeopleDraft;let release,entered;const ready=new Promise(resolve=>{entered=resolve;}),gate=new Promise(resolve=>{release=resolve;});
 f.store.savePeopleDraft=async args=>{entered();await gate;return save(args);};
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('held input');ctx.roots.prompt.firstListener('input')();await ready;const flight=ctx.feature.sendPrompt();ctx.feature.destroy();release();await flight;assert.equal(f.sendEntries(),0);assert.equal(f.sends.length,0);assert.equal(ctx.apiCalls.length,0);assert.deepEqual(ctx.alerts,[]);}finally{release();ctx.feature.destroy();}
});

test('a frozen People draft finishing after teardown never dispatches',async()=>{
 const f=transactionalPeopleFixture(),ctx=f.ctx,freeze=f.store.freezePeopleDraft;let release,entered;const ready=new Promise(resolve=>{entered=resolve;}),gate=new Promise(resolve=>{release=resolve;});
 f.store.freezePeopleDraft=async args=>{const result=await freeze(args);entered();await gate;return result;};
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('frozen input');ctx.roots.prompt.firstListener('input')();const flight=ctx.feature.sendPrompt();await ready;assert.ok(f.recovery().some(row=>row.kind==='draft'&&row.text==='frozen input'));ctx.feature.destroy();release();await flight;assert.equal(f.sendEntries(),0);assert.equal(f.sends.length,0);assert.ok(f.recovery().some(row=>row.kind==='draft'&&row.text==='frozen input'));assert.equal(ctx.apiCalls.length,0);assert.deepEqual(ctx.alerts,[]);}finally{release();ctx.feature.destroy();}
});

test('a held browser retry cleanup cannot update a rebound composer after teardown',async()=>{
 const record=retainedBrowserSnapshot({instance:'',binding:'current'}),events=[];let release,entered;const ready=new Promise(resolve=>{entered=resolve;}),gate=new Promise(resolve=>{release=resolve;});
 const ctx=makeFeature({sel:'team/'+record.key,nodes:[],deps:{peopleContext:()=>({key:record.key,copyInstance:record.copyInstance,binding:'current',owner:false,writable:false}),peopleFileStore:{list:async()=>[]},pendingPeople:()=>record,localReviewApi:async(path,opts)=>{events.push(opts.method);return opts.method==='POST'?{state:'locally_discarded'}:path.includes('/pending/')?{state:'missing'}:{entries:[]};},fencePeoplePending:()=>{events.push('fence');return {state:'locally_retired'};},clearPeoplePending:async()=>{events.push('clear');entered();await gate;return false;}}});
 const old=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-browser-retire'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-browser-retire(?:\s+[^"\s]+)*" data-row="(\d+)" data-snapshot="([^"]+)"/g)].map(m=>el('button',{dataset:{row:m[1],snapshot:m[2]}}))):old(s);
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(buttons.length,1);buttons[0].click();await ready;ctx.feature.destroy();ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('rebound draft');ctx.roots.prompt.firstListener('input')();release();await localReviewTick();assert.equal(ctx.feature.promptText(),'rebound draft');assert.deepEqual(events.slice(0,5),['GET','GET','POST','fence','clear']);assert.equal(record.text,'retained synthetic');assert.equal(ctx.apiCalls.length,0);assert.deepEqual(ctx.alerts,[]);}finally{release();ctx.feature.destroy();}
});

test('discarding a reviewed transactional pending publication clears only its matching browser review',async()=>{
 const key=localReviewKey,copyInstance='1'.repeat(32),binding='current',scope=`people\0${key}\0binding:${binding}\0copy:${copyInstance}\0`,id='3333333333333333',revision='a'.repeat(64),events=[],context={key,copyInstance,binding,owner:false,writable:true,attachments:true};let pending=null,f;
 f=transactionalPeopleFixture({context,depsExtra:{pendingPeople:()=>pending,localReviewApi:async(path,opts)=>{events.push(opts.method);if(opts.method==='DELETE')return {state:'locally_discarded'};return path.includes('/pending/')?{state:'pending',revision:'e'.repeat(64),text:'saved text',stage_ids:[id]}:{entries:[localReviewRow('pending','2'.repeat(16),{revision:'e'.repeat(64),text:'saved text',stage_ids:[id]})]};},fencePeoplePending:()=>{events.push('fence');return {state:'locally_retired'};},clearPeoplePending:async(record,options)=>{events.push('clear');const result=await f.actual.decidePeoplePublication({scope,publication:record.publication,expected:record,proof:options.proof,current:options.current});pending=null;return result;}}});
 const ctx=f.ctx,token=await f.actual.save(scope,id,{name:'kept.txt',type:'text/plain',size:1});await f.actual.setStageRevision(scope,id,revision,token);const draft=await f.actual.savePeopleDraft({scope,text:'saved text'}),capture=await f.actual.freezePeopleDraft({scope,text:'saved text',...draft}),newer=await f.actual.savePeopleDraft({scope,text:'newer draft'}),request={key:'2'.repeat(16),text:'saved text',anchor:0,attachments:[id]},admitted=await f.actual.beginPeoplePublication({scope,peopleKey:key,copyInstance,binding,native:'',...capture,request,files:[{id,fileToken:token,stageRevision:revision}]});
 const otherScope='people\0other synthetic scope\0',other=await f.actual.savePeopleDraft({scope:otherScope,text:'other draft'}),otherToken=await f.actual.save(otherScope,'4'.repeat(16),{name:'other.txt',type:'text/plain',size:1});
 pending={format:'transactional',scope,publication:admitted.publication,head:capture.head,version:capture.version,key,instance:'',copyInstance,binding,storageKey:'idb:'+admitted.publication,value:JSON.stringify({instance:'',copyInstance,request}),publicationKey:request.key,text:request.text,attachments:request.attachments};
 const old=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-local-discard'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-local-discard(?:\s+[^"\s]+)*" data-row="(\d+)" data-id="([0-9a-f]+)"/g)].map(m=>el('button',{dataset:{row:m[1],id:m[2]}}))):old(s);
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(buttons.length,1);assert.equal(events.filter(e=>e==='GET').length,2);buttons[0].click();await localReviewTick();assert.equal(pending,null);assert.deepEqual(events,['GET','GET','DELETE','fence','clear']);assert.equal(f.recovery().find(row=>row.publication===admitted.publication).state,'retired');assert.ok(f.recovery().some(row=>row.key==='head:'+newer.head&&row.version===newer.version));assert.ok(f.recovery().some(row=>row.key==='draft:'+newer.version&&row.text==='newer draft'));assert.equal((await f.actual.list(scope))[0].fileToken,token);assert.ok(f.recovery().some(row=>row.key==='draft:'+other.version&&row.text==='other draft'));assert.equal((await f.actual.list(otherScope))[0].fileToken,otherToken);assert.equal(f.sendEntries(),0);assert.equal(ctx.apiCalls.length,0);assert.equal(renderedClassCount(ctx.roots.attstage.innerHTML,'people-retry'),0);}finally{ctx.feature.destroy();}
});

test('a pending local discard response after teardown cannot change the rebound draft',async()=>{
 const key=localReviewKey,copyInstance='1'.repeat(32),events=[];let entered,release;const ready=new Promise(resolve=>{entered=resolve;}),gate=new Promise(resolve=>{release=resolve;});
 const ctx=makeFeature({sel:'team/'+key,nodes:[],deps:{peopleContext:()=>({key,copyInstance,binding:'current',owner:false,writable:true}),localReviewApi:async(_path,opts)=>{events.push(opts.method);if(opts.method==='DELETE'){entered();await gate;return {state:'locally_discarded'};}return {entries:[localReviewRow('pending','2'.repeat(16),{text:'saved text'})]};}}});
 const old=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-local-discard'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-local-discard(?:\s+[^"\s]+)*" data-row="(\d+)" data-id="([0-9a-f]+)"/g)].map(m=>el('button',{dataset:{row:m[1],id:m[2]}}))):old(s);
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.equal(buttons.length,1);buttons[0].click();await ready;ctx.feature.destroy();ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('new draft');ctx.roots.prompt.firstListener('input')();release();await localReviewTick();assert.equal(ctx.feature.promptText(),'new draft');assert.equal(events.filter(e=>e==='DELETE').length,1);assert.equal(ctx.apiCalls.length,0);assert.deepEqual(ctx.alerts,[]);}finally{release();ctx.feature.destroy();}
});

test('teardown queued after a confirmed receipt prevents a queued file decision from entering storage',async()=>{
 let release,dispatches=0,finishes=0;const response=new Promise(resolve=>{release=resolve;}),f=transactionalPeopleFixture({depsExtra:{sendPeople:()=>{dispatches++;return response;},finishPeople:async()=>{finishes++;return {state:'completed',seq:7,filesConfirmed:true};}}}),ctx=f.ctx;
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('saved input');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await f.waitReady(1);const flight=ctx.feature.sendPrompt();while(!dispatches)await localReviewTick();const file=f.files()[0];release({seq:7,peoplePending:{format:'transactional'}});queueMicrotask(()=>ctx.feature.destroy());await flight;assert.equal(dispatches,1);assert.equal(finishes,0);assert.equal((await f.actual.list(file.scope))[0].fileToken,file.fileToken);assert.equal(ctx.apiCalls.filter(c=>c.opts?.method==='DELETE').length,0);assert.deepEqual(ctx.alerts,[]);}finally{ctx.feature.destroy();}
});

async function storeBackedRetryFixture(){
 let cache=null,f,holdRelease;const requests=[],transportGate=new Promise(resolve=>{holdRelease=resolve;});
 const context={key:localReviewKey,copyInstance:'1'.repeat(32),binding:'current',owner:false,writable:true,attachments:true};
 const refresh=async scope=>{const rows=(await f.actual.listPeopleRecovery(scope)).pending;cache=rows.length?rows.map(row=>{const identity={key:row.peopleKey,copyInstance:row.copyInstance,binding:row.binding,instance:row.native},value=JSON.stringify({instance:row.native,copyInstance:row.copyInstance,request:row.request}),parsed=parsePeoplePendingSnapshot(value,identity);assert.deepEqual(row.files.map(file=>file.id),parsed.attachments);return {...parsed,format:'transactional',scope,publication:row.publication,head:row.head,version:row.version,storageKey:'idb:'+row.publication};})[0]:null;return cache;};
 f=transactionalPeopleFixture({context,respond:async()=>{throw Error('synthetic unconfirmed first send');},depsExtra:{pendingPeople:()=>cache,retryPeople:async()=>{const original=cache;if(!original)throw Error('Pending message recovery is unavailable.');await refresh(original.scope);if(!cache||cache.locallyRetired||cache.format!==original.format||cache.storageKey!==original.storageKey||cache.value!==original.value)throw Object.assign(Error('retired'),{code:'PEOPLE_LOCAL_RETIRED'});const current=cache,request=JSON.parse(current.value).request;requests.push(structuredClone(request));await transportGate;return {seq:7,peoplePending:current};}}});
 const ctx=f.ctx;ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('old synthetic caption');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await f.waitReady(1);await ctx.feature.sendPrompt();const old=f.pending();assert.ok(old);await refresh(old.scope);ctx.feature.renderStage();await localReviewTick();const button=ctx.roots.attstage._retryButtons[0];assert.ok(button);const oldClick=button.firstListener('click');assert.equal(typeof oldClick,'function');
 return {f,ctx,old,oldClick,requests,refresh,cache:()=>cache,release:holdRelease};
}

for(const transition of ['replacement','missing']) test(`stale captured retry is quiet after ${transition} of its stored publication`,async()=>{
 const x=await storeBackedRetryFixture(),{f,ctx,old,oldClick,requests}=x;
 try{
  const result=await f.actual.decidePeoplePublication({scope:old.scope,publication:old.publication,expected:old,proof:{state:'locally_discarded'}});assert.equal(result.state,'retired');const oldDecision=structuredClone(f.recovery().find(row=>row.publication===old.publication));let fresh;
  if(transition==='replacement'){
   const file=f.files()[0],request={key:'9'.repeat(16),text:'new synthetic caption',anchor:0,attachments:[file.id]},draft=await f.actual.savePeopleDraft({scope:old.scope,text:request.text}),capture=await f.actual.freezePeopleDraft({scope:old.scope,text:request.text,...draft});
   fresh=await f.actual.beginPeoplePublication({scope:old.scope,peopleKey:old.key,copyInstance:old.copyInstance,binding:old.binding,native:old.instance,...capture,request,files:[{id:file.id,fileToken:file.fileToken,stageRevision:file.stageRevision}]});assert.notEqual(fresh.publication,old.publication);
  }
  await x.refresh(old.scope);assert.equal(x.cache()?.publication,fresh?.publication);const before=structuredClone(f.recovery()),files=structuredClone(f.files()),alerts=[...ctx.alerts],source='synthetic source marker';ctx.storage.setItem('synthetic-source-marker',source);
  oldClick();await localReviewTick();
  assert.deepEqual(f.recovery(),before);assert.deepEqual(f.files(),files);assert.deepEqual(f.recovery().find(row=>row.publication===old.publication),oldDecision);assert.equal(ctx.storage.getItem('synthetic-source-marker'),source);assert.equal(ctx.apiCalls.length,0);assert.equal(requests.length,0,'an old control must not dispatch the current request');assert.deepEqual(ctx.alerts,alerts,'a stale control should be quiet');
 }finally{x.release();ctx.feature.destroy();}
});

test('an explicit retry of the unchanged stored publication posts its original key once',async()=>{
 const x=await storeBackedRetryFixture(),{f,ctx,old,oldClick,requests}=x;
 try{const before=JSON.parse(old.value).request;oldClick();await localReviewTick();assert.deepEqual(requests,[before]);assert.equal(requests[0].key,old.publicationKey);assert.equal(f.sendEntries(),1);assert.equal(ctx.apiCalls.length,0);x.release();await localReviewTick();assert.equal(f.recovery().find(row=>row.publication===old.publication).state,'completed');assert.equal(f.recovery().find(row=>row.publication===old.publication).seq,7);}finally{x.release();ctx.feature.destroy();}
});

test('stale retry refuses a same-publication request corrupted by another writer',async()=>{
 const x=await storeBackedRetryFixture(),{f,ctx,old,oldClick,requests}=x;
 try{
  await f.driver.writePeople(({files,recovery})=>({files,recovery:recovery.map(row=>row.publication===old.publication?{...row,request:{...row.request,text:'corrupted synthetic text'}}:row),value:null}));
  const current=await x.refresh(old.scope);assert.equal(current.storageKey,old.storageKey);assert.notEqual(current.value,old.value);assert.equal(current.publication,old.publication);assert.equal(current.publicationKey,old.publicationKey);assert.deepEqual(current.attachments,old.attachments);
  const before=structuredClone(f.recovery()),files=structuredClone(f.files()),alerts=[...ctx.alerts];oldClick();await localReviewTick();assert.deepEqual(f.recovery(),before);assert.deepEqual(f.files(),files);assert.equal(ctx.apiCalls.length,0);assert.equal(requests.length,0,'a stale control must not dispatch a changed frozen request');assert.deepEqual(ctx.alerts,alerts);
 }finally{x.release();ctx.feature.destroy();}
});

test('retirement prevents readmission of the same frozen file request key in its scope and copy',async()=>{
 const x=await storeBackedRetryFixture(),{f,ctx,old}=x;
 try{await f.actual.decidePeoplePublication({scope:old.scope,publication:old.publication,expected:old,proof:{state:'locally_discarded'}});const file=f.files()[0],request=JSON.parse(old.value).request,draft=await f.actual.savePeopleDraft({scope:old.scope,text:request.text}),capture=await f.actual.freezePeopleDraft({scope:old.scope,text:request.text,...draft}),before=structuredClone(f.recovery()),files=structuredClone(f.files());await assert.rejects(f.actual.beginPeoplePublication({scope:old.scope,peopleKey:old.key,copyInstance:old.copyInstance,binding:old.binding,native:old.instance,...capture,request,files:[{id:file.id,fileToken:file.fileToken,stageRevision:file.stageRevision}]}),/conflict/i);assert.deepEqual(f.recovery(),before);assert.deepEqual(f.files(),files);assert.equal(ctx.apiCalls.length,0);}finally{x.release();ctx.feature.destroy();}
});

test('a stale retry from an old binding cannot dispatch an identical request in the new binding',async()=>{
 const x=await storeBackedRetryFixture(),{f,ctx,old,oldClick,requests}=x;
 try{
  const oldFile=f.files().find(row=>row.scope===old.scope),retired=await f.actual.decidePeoplePublication({scope:old.scope,publication:old.publication,expected:old,proof:{state:'locally_discarded'}});assert.equal(retired.state,'retired');
  const binding='new-binding',scope=`people\0${old.key}\0binding:${binding}\0copy:${old.copyInstance}\0`,request=JSON.parse(old.value).request,token=await f.actual.save(scope,oldFile.id,oldFile.file);await f.actual.setStageRevision(scope,oldFile.id,oldFile.stageRevision,token);const draft=await f.actual.savePeopleDraft({scope,text:request.text}),capture=await f.actual.freezePeopleDraft({scope,text:request.text,...draft}),fresh=await f.actual.beginPeoplePublication({scope,peopleKey:old.key,copyInstance:old.copyInstance,binding,native:old.instance,...capture,request,files:[{id:oldFile.id,fileToken:token,stageRevision:oldFile.stageRevision}]});
  assert.notEqual(token,oldFile.fileToken);f.context.binding=binding;const current=await x.refresh(scope);assert.equal(current.value,old.value);assert.notEqual(current.storageKey,old.storageKey);assert.equal(current.publication,fresh.publication);assert.equal(current.key,old.key);assert.equal(current.copyInstance,old.copyInstance);assert.equal(current.instance,old.instance);assert.deepEqual(current.attachments,old.attachments);
  ctx.feature.contextChanged();await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/saved\.txt/);const before=structuredClone(f.recovery()),files=structuredClone(f.files()),alerts=[...ctx.alerts];oldClick();await localReviewTick();assert.deepEqual(f.recovery(),before);assert.deepEqual(f.files(),files);assert.equal(ctx.apiCalls.length,0);assert.equal(requests.length,0,'a stale control must not dispatch a different publication with identical request bytes');assert.deepEqual(ctx.alerts,alerts);
 }finally{x.release();ctx.feature.destroy();}
});

function scheduleTeardownAfter(promise, turns, action){
 promise.then(()=>{let next=Promise.resolve();for(let i=0;i<turns;i++)next=next.then(()=>{});next.then(action);});
}

for(const turns of Array.from({length:13},(_,i)=>i)) test(`pending discard completion preserves a rebound draft after ${turns} microtask turns`,async()=>{
 const key=localReviewKey,copyInstance='1'.repeat(32),events=[];let resolveDelete,entered,finished;const ready=new Promise(resolve=>{entered=resolve;}),teardown=new Promise(resolve=>{finished=resolve;});let deletesAtTeardown;
 const ctx=makeFeature({sel:'team/'+key,nodes:[],deps:{peopleContext:()=>({key,copyInstance,binding:'current',owner:false,writable:true}),localReviewApi:(_path,opts)=>{events.push(opts.method);if(opts.method==='DELETE'){const response=new Promise(resolve=>{resolveDelete=resolve;});scheduleTeardownAfter(response,turns,()=>{deletesAtTeardown=events.filter(e=>e==='DELETE').length;ctx.feature.destroy();ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('rebound draft');ctx.roots.prompt.firstListener('input')();finished();});entered();return response;}return Promise.resolve({entries:[localReviewRow('pending','2'.repeat(16),{text:'saved text'})]});}}});
 const original=ctx.roots.attstage.querySelectorAll;let buttons=[];ctx.roots.attstage.querySelectorAll=s=>s==='.people-local-discard'?(buttons=[...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-local-discard(?:\s+[^"\s]+)*" data-row="(\d+)" data-id="([0-9a-f]+)"/g)].map(m=>el('button',{dataset:{row:m[1],id:m[2]}}))):original(s);
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();ctx.storage.setItem('synthetic-source-marker','preserved');assert.equal(buttons.length,1);buttons[0].click();await ready;resolveDelete({state:'locally_discarded'});await teardown;await localReviewTick();assert.equal(events.filter(e=>e==='DELETE').length,deletesAtTeardown);assert.equal(deletesAtTeardown,1);assert.equal(ctx.feature.promptText(),'rebound draft');assert.equal(ctx.storage.getItem('synthetic-source-marker'),'preserved');assert.equal(ctx.apiCalls.length,0);assert.deepEqual(ctx.alerts,[]);}finally{if(resolveDelete)resolveDelete({state:'locally_discarded'});ctx.feature.destroy();}
});

for(const phase of ['decision','file-removal'])for(const turns of Array.from({length:13},(_,i)=>i))test(`People ${phase} completion preserves rebound data after ${turns} microtask turns`,async()=>{
 let f,finished,scheduled=false,removesAtTeardown,deletesAtTeardown;const teardown=new Promise(resolve=>{finished=resolve;});
 const after=promise=>{if(!scheduled){scheduled=true;scheduleTeardownAfter(promise,turns,()=>{removesAtTeardown=f.events.filter(e=>e==='remove').length;deletesAtTeardown=f.ctx.apiCalls.filter(c=>c.opts?.method==='DELETE').length;f.ctx.feature.destroy();f.ctx.feature.bind();f.ctx.feature.onSelect();f.ctx.feature.setPromptText('rebound draft');f.ctx.roots.prompt.firstListener('input')();finished();});}return promise;};
 f=transactionalPeopleFixture({depsExtra:phase==='decision'?{finishPeople:(receipt,options)=>{const p=receipt.peoplePending;return after(f.actual.decidePeoplePublication({scope:p.scope,publication:p.publication,expected:p,proof:{state:'completed',seq:receipt.seq,text:p.text,stage_ids:p.attachments},current:options.current}));}}:{}});const ctx=f.ctx;
 if(phase==='file-removal'){const remove=f.store.removeStageFile;f.store.removeStageFile=(...args)=>after(remove(...args));}
 const otherScope='people\0separate synthetic\0',otherToken=await f.actual.save(otherScope,'4'.repeat(16),{name:'other.txt',type:'text/plain',size:1});
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.storage.setItem('synthetic-source-marker','preserved');ctx.feature.setPromptText('original draft');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await f.waitReady(1);const flight=ctx.feature.sendPrompt();await teardown;await flight;await localReviewTick();assert.equal(f.sendEntries(),1);assert.equal(f.sends.length,1);assert.equal(f.events.filter(e=>e==='remove').length,removesAtTeardown);assert.equal(ctx.apiCalls.filter(c=>c.opts?.method==='DELETE').length,deletesAtTeardown);assert.equal((await f.actual.list(otherScope))[0].fileToken,otherToken);assert.equal(ctx.feature.promptText(),'rebound draft');assert.equal(ctx.storage.getItem('synthetic-source-marker'),'preserved');assert.deepEqual(ctx.alerts,[]);}finally{ctx.feature.destroy();}
});

test('confirmed retry preserves a repaired file when the restored view lacks its stage revision',async()=>{
 const f=transactionalPeopleFixture({respond:async()=>{throw Error('synthetic uncertain send');}}),first=f.ctx;
 first.feature.bind();first.feature.onSelect();first.feature.setPromptText('saved caption');first.roots.prompt.firstListener('input')();first.feature.addFiles([{name:'saved.txt',type:'text/plain',size:1}]);await f.waitReady(1);await first.feature.sendPrompt();const pending=f.pending(),original=f.files()[0];assert.ok(pending);assert.ok(original.stageRevision);first.feature.destroy();
 // Model damaged persisted metadata; a separate writer repairs the real store
 // after this new view has restored the older metadata.
 await f.driver.writePeople(({files,recovery})=>({files:files.map(row=>{if(row.key!==original.key)return row;const copy={...row};delete copy.stageRevision;return copy;}),recovery,value:null}));
 let retries=0;const context=f.context,ctx=makeFeature({sel:'team/'+context.key,nodes:[],deps:{peopleContext:()=>context,peopleFileStore:f.store,pendingPeople:()=>pending,retryPeople:async()=>{retries++;return {seq:7,peoplePending:pending};},finishPeople:(receipt,options)=>f.actual.decidePeoplePublication({scope:pending.scope,publication:pending.publication,expected:pending,proof:{state:'completed',seq:receipt.seq,text:pending.text,stage_ids:pending.attachments},current:options.current})}});
 try{ctx.feature.bind();ctx.feature.onSelect();await localReviewTick();assert.match(ctx.roots.attstage.innerHTML,/saved\.txt/);assert.equal((await f.actual.list(original.scope))[0].stageRevision,undefined);await f.actual.setStageRevision(original.scope,original.id,original.stageRevision,original.fileToken);ctx.feature.setPromptText('newer draft');ctx.roots.prompt.firstListener('input')();await localReviewTick();ctx.roots.attstage._retryButtons[0].click();await localReviewTick();const decision=f.recovery().find(row=>row.publication===pending.publication),file=(await f.actual.list(original.scope))[0];assert.equal(retries,1);assert.equal(decision.state,'completed');assert.equal(decision.seq,7);assert.equal(decision.filesConfirmed,true);assert.equal(file.fileToken,original.fileToken);assert.equal(file.stageRevision,original.stageRevision);assert.equal(file.published,7);assert.match(ctx.roots.attstage.innerHTML,/Sent; review the current local file before removing this saved file/);assert.equal(ctx.feature.promptText(),'newer draft');assert.equal(ctx.apiCalls.filter(c=>c.opts?.method==='DELETE').length,0);assert.deepEqual(ctx.alerts,[]);}finally{ctx.feature.destroy();first.feature.destroy();}
});

test('confirmed People image cleanup revokes its preview and preserves another copy file',async()=>{
 const f=transactionalPeopleFixture(),ctx=f.ctx,otherScope='people\0synthetic other copy\0',otherToken=await f.actual.save(otherScope,'4'.repeat(16),{name:'other.txt',type:'text/plain',size:1});
 try{ctx.feature.bind();ctx.feature.onSelect();ctx.feature.setPromptText('image caption');ctx.roots.prompt.firstListener('input')();ctx.feature.addFiles([{name:'image.png',type:'image/png',size:1}]);await f.waitReady(1);assert.deepEqual(ctx.created,['blob:image.png']);await ctx.feature.sendPrompt();await localReviewTick();assert.deepEqual(ctx.revoked,['blob:image.png']);assert.equal((await f.actual.list(otherScope))[0].fileToken,otherToken);assert.equal(f.recovery().find(row=>row.kind==='decision').seq,7);assert.equal(f.sends.length,1);assert.equal(ctx.apiCalls.filter(c=>c.path.endsWith('/send')).length,0);}finally{ctx.feature.destroy();}
});

test('late exact pending discard records legacy retirement without changing either view or raw storage', async () => {
  const record = retainedBrowserSnapshot({instance:'', binding:'current'});
  const scope = `people\0${record.key}\0binding:current\0copy:${record.copyInstance}\0`;
  const otherScope = 'people\0synthetic-other-copy\0';
  const context = {key:record.key, copyInstance:record.copyInstance, binding:'current', owner:false, writable:true};
  let files = [], recovery = [{key:'allocator', epoch:'a'.repeat(32), next:1}];
  const driver = {
    read: async () => files,
    readWrite: async fn => { const next = fn(files); files = next.rows; return next.value; },
    writePeople: async fn => { const next = fn({files, recovery}); files = next.files; recovery = next.recovery; return next.value; },
  };
  const store = createPeopleFileStore(driver);
  await store.save(scope, record.attachments[0], {name:'original.txt', type:'text/plain', size:2});
  await store.save(otherScope, '4'.repeat(16), {name:'newer.txt', type:'text/plain', size:3});
  const originalFiles = structuredClone(files);
  let releaseDelete, deleteStarted, deleted = false, cachedDecision;
  const started = new Promise(resolve => { deleteStarted = resolve; });
  const calls = [];
  let ctx, selected = 'team/'+record.key;
  // The guest adapter is synthetic; it parses raw localStorage and loads/records
  // the real Store decision with the same exact snapshot and proof contract.
  ctx = makeFeature({sel:'team/'+record.key, nodes:[{id:'n1', instance_id:'native', ended_at:''}], deps:{
    peopleContext: () => selected === 'team/'+record.key ? context : null,
    peopleFileStore: store,
    pendingPeople: () => {
      if (selected !== 'team/'+record.key) return null;
      const raw = ctx.storage.getItem(record.storageKey);
      if (!raw) return null;
      const parsed = parsePeoplePendingSnapshot(raw, {key:record.key, copyInstance:record.copyInstance, binding:record.binding, instance:record.instance});
      return {...parsed, format:'legacy', ...(cachedDecision ? {locallyRetired:true, decision:cachedDecision} : {})};
    },
    refreshPeopleRecovery: async identity => {
      const result = await store.listPeopleRecovery(identity);
      const row = result.legacy.find(item => item.snapshot.storageKey === record.storageKey && item.snapshot.value === record.value);
      cachedDecision = row ? {state:row.state} : undefined;
      return result;
    },
    localReviewApi: async (path, opts) => {
      calls.push({path, method:opts.method});
      if (opts.method === 'DELETE') {
        assert.equal(path, `/api/team/subscriptions/${record.key}/pending/${record.publicationKey}?copy_instance=${record.copyInstance}&revision=${'e'.repeat(64)}`);
        deleteStarted();
        return new Promise(resolve => { releaseDelete = () => { deleted = true; resolve({state:'locally_discarded'}); }; });
      }
      if (path.includes('/pending/')) return deleted ? {state:'retired', revision:'e'.repeat(64), text:'actual', stage_ids:record.attachments} : {state:'pending', revision:'e'.repeat(64), text:'actual', stage_ids:record.attachments};
      return {entries:deleted ? [] : [localReviewRow('pending', record.publicationKey, {text:'actual', stage_ids:record.attachments})]};
    },
    fencePeoplePending: snapshot => { assert.equal(snapshot.value, record.value); return {state:'locally_retired'}; },
    clearPeoplePending: async (snapshot, options) => {
      const result = await store.recordLegacyPeopleDecision({scope:options.scope, snapshot, proof:options.proof, current:options.current});
      if (!options.current()) return false;
      cachedDecision = {state:result.state};
      return true;
    },
  }});
  const originalQuery = ctx.roots.attstage.querySelectorAll;
  let buttons = [];
  ctx.roots.attstage.querySelectorAll = selector => selector === '.people-local-discard'
    ? (buttons = [...ctx.roots.attstage.innerHTML.matchAll(/class="(?:[^"\s]+\s+)*people-local-discard(?:\s+[^"\s]+)*" data-row="(\d+)" data-id="([0-9a-f]+)"/g)].map(match => el('button', {dataset:{row:match[1], id:match[2]}})))
    : originalQuery(selector);
  try {
    ctx.storage.setItem(record.storageKey, record.value);
    ctx.feature.bind(); ctx.feature.onSelect(); await localReviewTick();
    assert.equal(buttons.length, 1);
    buttons[0].click(); await started;
    assert.equal(recovery.filter(row => row.kind === 'legacy-decision').length, 0);
    selected = 'n1'; ctx.setSel(selected); ctx.feature.onSelect();
    ctx.feature.setPromptText('newer chat draft'); ctx.roots.prompt.firstListener('input')();
    ctx.storage.setItem('scimux-prompt-sources:n1', 'synthetic source association');
    await localReviewTick();
    const newer = {text:ctx.feature.promptText(), destination:ctx.feature.destination(), html:ctx.roots.attstage.innerHTML, source:ctx.storage.getItem('scimux-prompt-sources:n1')};
    releaseDelete(); await localReviewTick();
    const decisions = recovery.filter(row => row.kind === 'legacy-decision');
    assert.equal(decisions.length, 1);
    assert.equal(decisions[0].state, 'retired');
    assert.equal(decisions[0].scope, scope);
    assert.equal(decisions[0].snapshot.storageKey, record.storageKey);
    assert.equal(decisions[0].snapshot.value, record.value);
    assert.equal(ctx.storage.getItem(record.storageKey), record.value);
    assert.deepEqual(files, originalFiles);
    assert.deepEqual({text:ctx.feature.promptText(), destination:ctx.feature.destination(), html:ctx.roots.attstage.innerHTML, source:ctx.storage.getItem('scimux-prompt-sources:n1')}, newer);
    cachedDecision = undefined;
    selected = 'team/'+record.key; ctx.setSel(selected); ctx.feature.onSelect(); await localReviewTick();
    assert.equal(renderedClassCount(ctx.roots.attstage.innerHTML, 'people-retry'), 0);
    assert.match(ctx.roots.attstage.innerHTML, /legacy.*preserved/i);
    assert.equal(ctx.storage.getItem(record.storageKey), record.value);
    assert.equal(ctx.storage.getItem('scimux-prompt-sources:n1'), newer.source);
    assert.deepEqual(files, originalFiles);
    assert.equal(calls.filter(call => call.method === 'DELETE').length, 1);
    assert.equal(ctx.apiCalls.length, 0);
  } finally { if (releaseDelete && !deleted) releaseDelete(); ctx.feature.destroy(); }
});
