/* fixes-2 P4 — auto-approve toggle UI, lifecycle, and decision audit surfaces.
 * Pure helpers + factory behavior. Does not re-test Go policy. */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import {
  AUTO_APPROVE_HELP,
  AUTO_APPROVE_CLAUDE_HELP,
  AUTO_APPROVE_LABEL_FULL,
  AUTO_APPROVE_LABEL_NARROW,
  autoApproveAriaName,
  autoApproveChromeModel,
  autoApproveButtonInnerHTML,
  decisionsHash,
  mergeTimelineItems,
  decisionRowHTML,
  buildChatSignature,
  keyRowHTML,
  createChatFeature,
} from "../js/chat.js";
import { esc } from "../js/format.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const indexHtml = readFileSync(join(__dirname, "../index.html"), "utf8");
const chatCss = readFileSync(join(__dirname, "../css/chat.css"), "utf8");
const appJs = readFileSync(join(__dirname, "../js/app.js"), "utf8");
const chatJs = readFileSync(join(__dirname, "../js/chat.js"), "utf8");

/* ---------- Slice 1: static DOM order ---------- */

test("P4 static DOM: terminal → autoapprove → scrollend; composer outside polled regions", () => {
  const tools = indexHtml.match(/id="convtools"[\s\S]*?<\/div>\s*<div id="workpulse"/);
  assert.ok(tools, "convtools block present");
  const block = tools[0];
  const termAt = block.indexOf('id="termtoggle"');
  const aaAt = block.indexOf('id="autoapprove"');
  const scrollAt = block.indexOf('id="scrollend"');
  assert.ok(termAt >= 0 && aaAt > termAt && scrollAt > aaAt,
    "order is terminal → autoapprove → scrollend");
  // Composer is a sibling after workpulse, not inside convtools/msgs.
  assert.match(indexHtml, /id="promptbar"/);
  const msgsAt = indexHtml.indexOf('id="msgs"');
  const toolsAt = indexHtml.indexOf('id="convtools"');
  const promptAt = indexHtml.indexOf('id="promptbar"');
  assert.ok(msgsAt < toolsAt && toolsAt < promptAt,
    "msgs, then convtools, then promptbar (composer outside polled msgs)");
  assert.match(block, /aria-pressed="false"/);
  assert.match(block, /Auto-approve this turn|Auto-approve eligible tool requests/);
});

test("P4 icons are checked-in SVG constants, not a runtime Font Awesome load", () => {
  assert.match(appJs, /ICON_TOGGLE_OFF/);
  assert.match(appJs, /ICON_TOGGLE_ON/);
  assert.match(appJs, /ICON_WARN/);
  assert.doesNotMatch(appJs, /fontawesome\.com\/.*\.css/);
  assert.doesNotMatch(appJs, /kit\.fontawesome/);
  assert.doesNotMatch(indexHtml, /fontawesome/i);
  assert.match(appJs, /Font Awesome Free/);
  assert.match(appJs, /CC BY 4\.0/);
});

/* ---------- Slice 2: pure chrome model matrix ---------- */

test("P4 chrome model: off, primed, armed-zero, armed-count, Claude, error", () => {
  const off = autoApproveChromeModel({ supported: true, enabled: false, phase: "off", count: 0 });
  assert.equal(off.enabled, false);
  assert.equal(off.pressed, false);
  assert.equal(off.showWarning, false);
  assert.equal(off.showBadge, false);
  assert.equal(off.toggleIcon, "off");
  assert.equal(off.labelFull, AUTO_APPROVE_LABEL_FULL);
  assert.ok(off.classNames.includes("off"));

  const primed = autoApproveChromeModel({ supported: true, enabled: true, phase: "primed", count: 0 });
  assert.equal(primed.enabled, true);
  assert.equal(primed.pressed, true);
  assert.equal(primed.showWarning, true);
  assert.equal(primed.showBadge, false);
  assert.equal(primed.toggleIcon, "on");
  assert.ok(primed.classNames.includes("on"));

  const armed0 = autoApproveChromeModel({ supported: true, enabled: true, phase: "armed", count: 0 });
  assert.equal(armed0.showBadge, false);
  assert.equal(armed0.showWarning, true);

  const armed15 = autoApproveChromeModel({ supported: true, enabled: true, phase: "armed", count: 15 });
  assert.equal(armed15.showBadge, true);
  assert.equal(armed15.badgeText, "15");
  assert.equal(armed15.ariaLabel, "Auto-approve eligible tool requests for this turn; 15 approved.");
  assert.doesNotMatch(armed15.labelFull, /\(15\)/);
  assert.doesNotMatch(armed15.labelNarrow, /15/);

  const claude = autoApproveChromeModel(
    { supported: false, enabled: false, phase: "off", count: 0 },
    { agent: "claude" },
  );
  assert.equal(claude.disabled, true);
  assert.equal(claude.showWarning, false);
  assert.equal(claude.showBadge, false);
  assert.equal(claude.title, AUTO_APPROVE_CLAUDE_HELP);
  assert.equal(claude.ariaLabel, AUTO_APPROVE_CLAUDE_HELP);
  assert.ok(claude.classNames.includes("unsupported"));

  // Claude agent even if a buggy payload claims supported.
  const claudeHard = autoApproveChromeModel(
    { supported: true, enabled: true, phase: "armed", count: 3 },
    { agent: "claude" },
  );
  assert.equal(claudeHard.disabled, true);
  assert.equal(claudeHard.showBadge, false);

  const err = autoApproveChromeModel({
    supported: true, enabled: true, phase: "armed", count: 1,
    error: "auto-approve audit failed: disk full",
  });
  assert.equal(err.error, "auto-approve audit failed: disk full");
  assert.ok(err.classNames.includes("has-error"));
  assert.equal(err.enabled, true, "error retains authoritative enabled state");
});

test("P4 chrome HTML: warning only when on; badge only when count > 0; no parenthesized counts", () => {
  const icons = { ICON_TOGGLE_OFF: "OFF", ICON_TOGGLE_ON: "ON", ICON_WARN: "WARN" };
  const offHTML = autoApproveButtonInnerHTML(
    autoApproveChromeModel({ supported: true, enabled: false, phase: "off", count: 0 }),
    icons,
  );
  assert.match(offHTML, /OFF/);
  assert.doesNotMatch(offHTML, /WARN/);
  assert.doesNotMatch(offHTML, /aa-badge/);
  assert.match(offHTML, /Auto-approve this turn/);
  assert.match(offHTML, /aa-label-narrow[^>]*>Auto-approve</);

  const onHTML = autoApproveButtonInnerHTML(
    autoApproveChromeModel({ supported: true, enabled: true, phase: "armed", count: 15 }),
    icons,
  );
  assert.match(onHTML, /ON/);
  assert.match(onHTML, /WARN/);
  assert.match(onHTML, /class="aa-badge"[^>]*>15</);
  assert.doesNotMatch(onHTML, /\(15\)/);
  assert.match(onHTML, /Auto-approve this turn/);

  const help = AUTO_APPROVE_HELP;
  assert.match(help, /sole one-time approval/);
  assert.match(help, /Resets when the turn finishes/);
});

test("P4 accessibility pure contracts: aria name, 44px CSS target, non-color state", () => {
  assert.equal(autoApproveAriaName({ enabled: false, count: 0, supported: true }),
    "Auto-approve eligible tool requests for this turn");
  assert.equal(autoApproveAriaName({ enabled: true, count: 15, supported: true }),
    "Auto-approve eligible tool requests for this turn; 15 approved.");
  assert.equal(autoApproveAriaName({ supported: false }), AUTO_APPROVE_CLAUDE_HELP);

  assert.match(chatCss, /button\.autoapprove[\s\S]*min-width:\s*44px/);
  assert.match(chatCss, /button\.autoapprove[\s\S]*min-height:\s*44px/);
  // State cues beyond color: border, warning glyph, aria-pressed, toggle geometry.
  assert.match(chatCss, /button\.autoapprove\.on[\s\S]*border-color/);
  assert.match(chatCss, /\.aa-warn/);
  assert.match(chatCss, /prefers-contrast:\s*more/);
  assert.match(chatCss, /prefers-color-scheme:\s*dark/);
  // Narrow label contraction.
  assert.match(chatCss, /max-width:\s*520px[\s\S]*aa-label-full[\s\S]*display:\s*none/);
  assert.match(chatCss, /aa-label-narrow[\s\S]*display:\s*inline/);
});

/* ---------- Slice 3: decision rendering ---------- */

const sampleDecision = {
  record: 5,
  time: "2026-08-11T14:20:31Z",
  decision: {
    source: "auto",
    lease_id: "lease-abc",
    request_id: "req-9",
    agent: "grok",
    tool_kind: "execute",
    title: "go test ./... && <script>alert(1)</script>",
    reason: "Run the suite with <b>flags</b>",
    options: [
      { key: "1", name: "Always allow", kind: "allow_always" },
      { key: "2", name: "Allow once", kind: "allow" },
      { key: "3", name: "Reject", kind: "reject" },
    ],
    selected: { key: "2", name: "Allow once", kind: "allow" },
  },
};

test("P4 decisionRowHTML: compact summary, expandable details, full escape, no bubble actions", () => {
  const html = decisionRowHTML(sampleDecision, { escape: esc, fmtTime: t => t });
  assert.match(html, /class="decision"/);
  assert.match(html, /Auto-approved:.*go test/);
  assert.match(html, /Allow once/);
  assert.match(html, /<details/);
  assert.match(html, /lease-abc/);
  assert.match(html, /req-9/);
  assert.match(html, /tool_kind|Tool/);
  assert.match(html, /allow_always/);
  assert.match(html, /<code>2<\/code>/);
  // Escaping
  assert.match(html, /&lt;script&gt;/);
  assert.doesNotMatch(html, /<script>alert/);
  assert.match(html, /&lt;b&gt;flags&lt;\/b&gt;/);
  // No ordinary bubble actions
  assert.doesNotMatch(html, /data-bact=/);
  assert.doesNotMatch(html, /fork from here|bookmark|send to/);
  assert.doesNotMatch(html, /data-key=/);
  // Long command retained in details
  assert.match(html, /class="decision-cmd"/);
});

test("P4 mergeTimelineItems orders by durable record index; live/history parity helper", () => {
  const turns = [
    { role: "user", text: "a", record: 1 },
    { role: "assistant", text: "b", record: 3 },
    { role: "user", text: "c", record: 7 },
  ];
  const decisions = [
    { record: 5, decision: { title: "mid", selected: { key: "2", name: "Allow once" } } },
    { record: 2, decision: { title: "early", selected: { key: "1", name: "Allow once" } } },
  ];
  const items = mergeTimelineItems(turns, decisions);
  assert.deepEqual(items.map(i => [i.kind, i.record]), [
    ["turn", 1],
    ["decision", 2],
    ["turn", 3],
    ["decision", 5],
    ["turn", 7],
  ]);
  // Shared renderer path for current and historical (same function).
  assert.match(chatJs, /decisionRowHTML\(item\.decision/);
  assert.match(chatJs, /renderTimelineHTML/);
});

test("P4 decisionsHash changes rebuild signature; count alone is outside turns hash", () => {
  const base = {
    nodeId: "n1", attention: "", attentionHidden: false, delivery: "ok",
    showPeek: false, termOpen: false, termFull: false, source: "acp",
    permTitle: "", permOptionsKey: "", priorTurns: 0, chatStarted: "t",
    echoHash: "", histKey: "", turnsHash: "abc", decisionsHash: "",
  };
  const a = buildChatSignature(base);
  const b = buildChatSignature({ ...base, decisionsHash: "5:req:2:lease" });
  assert.notEqual(a, b, "new decision must change signature");
  assert.equal(buildChatSignature(base), a);
  assert.match(chatJs, /decisionsHash/);
  // Auto chrome is painted outside the signature skip path.
  assert.match(chatJs, /paintAutoApprove\(data\.auto_approve/);
});

/* ---------- fake DOM + factory ---------- */

function el(tag, attrs = {}){
  const listeners = new Map();
  const node = {
    tagName: tag.toUpperCase(),
    className: attrs.className || "",
    classList: {
      _s: new Set((attrs.className || "").split(/\s+/).filter(Boolean)),
      toggle(c, on){
        if (on === undefined) {
          if (this._s.has(c)) this._s.delete(c); else this._s.add(c);
        } else if (on) this._s.add(c); else this._s.delete(c);
        node.className = [...this._s].join(" ");
      },
      contains(c){ return this._s.has(c); },
      add(c){ this._s.add(c); node.className = [...this._s].join(" "); },
      remove(c){ this._s.delete(c); node.className = [...this._s].join(" "); },
    },
    style: {},
    dataset: { ...(attrs.dataset || {}) },
    hidden: !!attrs.hidden,
    disabled: !!attrs.disabled,
    textContent: attrs.textContent || "",
    title: attrs.title || "",
    _html: attrs.innerHTML || "",
    value: attrs.value || "",
    scrollTop: 0,
    scrollHeight: attrs.scrollHeight || 500,
    clientHeight: attrs.clientHeight || 400,
    children: [],
    parentNode: null,
    _attrs: { ...(attrs.attrs || {}) },
    setAttribute(k, v){ this._attrs[k] = String(v); },
    getAttribute(k){ return this._attrs[k]; },
    addEventListener(type, fn, opts){
      if (!listeners.has(type)) listeners.set(type, []);
      listeners.get(type).push({ fn, opts });
    },
    removeEventListener(type, fn){
      const arr = listeners.get(type) || [];
      listeners.set(type, arr.filter(x => x.fn !== fn));
    },
    querySelector(sel){
      if (sel.startsWith("#")) {
        const id = sel.slice(1);
        if (this.id === id) return this;
        for (const c of this.children) {
          if (c.id === id) return c;
          const f = c.querySelector?.(sel);
          if (f) return f;
        }
        return null;
      }
      if (sel.startsWith(".")) {
        const cls = sel.slice(1).split(/[\s.>]/)[0];
        if (this.classList.contains(cls)) return this;
        for (const c of this.children) {
          const f = c.querySelector?.(sel);
          if (f) return f;
        }
      }
      return null;
    },
    querySelectorAll(){ return []; },
    closest(){ return null; },
    after(){},
    insertBefore(node2, ref){
      if (!ref) this.children.push(node2);
      else {
        const i = this.children.indexOf(ref);
        this.children.splice(i < 0 ? this.children.length : i, 0, node2);
      }
      node2.parentNode = this;
      return node2;
    },
    appendChild(node2){ this.children.push(node2); node2.parentNode = this; return node2; },
    remove(){},
    focus(){},
    scrollIntoView(){},
    scrollTo(opts){ if (opts && opts.top != null) this.scrollTop = opts.top; },
    _listeners: listeners,
    listenerCount(type){ return (listeners.get(type) || []).length; },
  };
  if (attrs.id) node.id = attrs.id;
  Object.defineProperty(node, "innerHTML", {
    configurable: true,
    get(){ return node._html; },
    set(v){ node._html = v; node.children.length = 0; },
  });
  return node;
}

function firstListener(node, type){
  return (node._listeners.get(type) || [])[0]?.fn;
}

function makeRoots(){
  const msgs = el("div", { id: "msgs", scrollHeight: 500, clientHeight: 400 });
  msgs.scrollTop = 100;
  return {
    chathead: el("div", { id: "chathead" }),
    chattitle: el("h2", { id: "chattitle" }),
    chatmission: el("div", { id: "chatmission" }),
    chatdetails: el("div", { id: "chatdetails", hidden: true }),
    chatdesc: el("div", { id: "chatdesc" }),
    chatdescinput: el("textarea", { id: "chatdescinput" }),
    chatdot: el("span", { id: "chatdot" }),
    chatmeta: el("span", { id: "chatmeta" }),
    chatagentlogo: el("span", { id: "chatagentlogo" }),
    infobtn: el("button", { id: "infobtn" }),
    msgs,
    msgwrap: el("div", { id: "msgwrap" }),
    chatloading: el("div", { id: "chatloading", hidden: true }),
    gauge: el("div", { id: "gauge", hidden: true }),
    gaugefill: el("i", { id: "gaugefill" }),
    keyrow: el("div", { id: "keyrow" }),
    convtools: el("div", { id: "convtools", hidden: true }),
    termtoggle: el("button", { id: "termtoggle" }),
    autoapprove: el("button", {
      id: "autoapprove",
      className: "autoapprove off",
      attrs: { "aria-pressed": "false" },
    }),
    scrollend: el("button", { id: "scrollend" }),
    workpulse: el("div", { id: "workpulse" }),
  };
}

function makeFeature(overrides = {}){
  const roots = makeRoots();
  const nodes = overrides.nodes || [{
    id: "g1", title: "Grok", agent: "grok", model: "grok",
    live: "quiet", attention: "", lane_id: "", description: "",
  }];
  let sel = overrides.sel ?? "g1";
  let selGen = overrides.selGen ?? 1;
  const apiCalls = [];
  let chatPayload = overrides.chatPayload || {
    turns: [{ role: "user", text: "hi", time: "t", record: 1 }],
    live: "quiet",
    delivery: "ok",
    source: "acp",
    chat_started: "2026-08-11T00:00:00Z",
    prior_turns: 0,
    assets: {},
    auto_approve: { supported: true, enabled: false, phase: "off", count: 0 },
  };
  const resolveApi = async (path, opts) => {
    if (typeof overrides.api === "function") return overrides.api(path, opts, apiCalls);
    if (path.includes("/auto-approve")) {
      if (overrides.autoApproveError) throw Object.assign(new Error(overrides.autoApproveError), { status: 500 });
      const body = opts && opts.body ? JSON.parse(opts.body) : {};
      const view = body.enabled
        ? (overrides.enableView || { supported: true, enabled: true, phase: "primed", count: 0 })
        : { supported: true, enabled: false, phase: "off", count: 0 };
      return view;
    }
    if (path.includes("/chat?history=1")) {
      return { segments: overrides.histSegs || [], assets: {} };
    }
    if (path.includes("/peek")) return "pane";
    if (path.includes("/chat")) {
      return typeof chatPayload === "function" ? chatPayload() : chatPayload;
    }
    return {};
  };
  const api = async (path, opts) => {
    apiCalls.push({ path, opts, kind: "api" });
    return resolveApi(path, opts);
  };
  const apiConditionalGet = overrides.apiConditionalGet || (async (path, etag) => {
    apiCalls.push({ path, etag: etag || "", kind: "conditional",
      opts: { headers: etag ? { "If-None-Match": etag } : {} } });
    if (typeof overrides.chatConditional === "function")
      return overrides.chatConditional(path, etag, apiCalls);
    if (path.includes("/chat") && !path.includes("history")) {
      const data = await resolveApi(path, {});
      return { status: 200, etag: overrides.stableChatETag || '"etag1"', data };
    }
    return { status: 200, etag: "", data: await resolveApi(path, {}) };
  });
  const feature = createChatFeature({
    roots,
    document: { createElement: tag => el(tag), querySelector: () => null },
    window: {},
    CSS: { escape: s => String(s) },
    api,
    apiConditionalGet,
    nodes: () => nodes,
    sel: () => sel,
    selGen: () => selGen,
    nodeById: id => nodes.find(n => n.id === id),
    hardAttention: () => false,
    laneColor: () => "#007AFF",
    agentLogo: () => "LOGO",
    icons: {
      ICON_TERM: "T", ICON_CHECK: "✓", ICON_CHEV_UP: "↑", ICON_CHEV_DOWN: "↓",
      ICON_BRANCH: "B", ICON_INTO: "I", ICON_COPY: "C", ICON_DOWNALL: "↓↓",
      ICON_FILE: "F", ICON_TOGGLE_OFF: "OFF", ICON_TOGGLE_ON: "ON", ICON_WARN: "WARN",
    },
    setComposerBusy: () => {},
    setComposerClosed: () => {},
    setAttachAvail: () => {},
    setBookmarksBackLabel: () => {},
    getPendingJump: () => null,
    setPendingJump: () => {},
    invalidateCardsSig: () => {},
    invalidateMapSig: () => {},
    renderCards: () => {},
    renderMap: () => {},
    updateLocalNode: () => {},
    copyText: () => {},
    uiMutate: () => {},
    stampAddress: () => {},
    forkFromTurn: () => {},
    alert: () => {},
    toast: () => {},
    tick: () => {},
    scheduleTick: () => {},
    isDesktop: () => false,
    now: () => 1_000_000,
    longpress: () => () => {},
    ...overrides.deps,
  });
  return {
    feature, roots, nodes, apiCalls,
    setSel: v => { sel = v; },
    setSelGen: v => { selGen = v; },
    setChatPayload: v => { chatPayload = v; },
  };
}

/* ---------- Slice 4: factory paint + click behavior ---------- */

test("P4 factory paints off/primed/armed/count and Claude disabled", async () => {
  {
    const { feature, roots } = makeFeature();
    feature.bind();
    await feature.render();
    const btn = roots.autoapprove;
    assert.equal(btn.getAttribute("aria-pressed"), "false");
    assert.match(btn.innerHTML, /OFF/);
    assert.doesNotMatch(btn.innerHTML, /WARN/);
    assert.doesNotMatch(btn.innerHTML, /aa-badge/);
    assert.match(btn.innerHTML, /Auto-approve this turn/);
    feature.destroy();
  }
  {
    const { feature, roots } = makeFeature({
      chatPayload: {
        turns: [{ role: "user", text: "hi", record: 1 }],
        live: "quiet", delivery: "ok", source: "acp",
        chat_started: "t", prior_turns: 0, assets: {},
        auto_approve: { supported: true, enabled: true, phase: "primed", count: 0 },
      },
    });
    feature.bind();
    await feature.render();
    assert.equal(roots.autoapprove.getAttribute("aria-pressed"), "true");
    assert.match(roots.autoapprove.innerHTML, /ON/);
    assert.match(roots.autoapprove.innerHTML, /WARN/);
    assert.doesNotMatch(roots.autoapprove.innerHTML, /aa-badge/);
    feature.destroy();
  }
  {
    const { feature, roots } = makeFeature({
      chatPayload: {
        turns: [{ role: "user", text: "hi", record: 1 }],
        live: "active", delivery: "ok", source: "acp",
        chat_started: "t", prior_turns: 0, assets: {},
        auto_approve: { supported: true, enabled: true, phase: "armed", count: 15 },
      },
    });
    feature.bind();
    await feature.render();
    assert.equal(roots.autoapprove.getAttribute("aria-pressed"), "true");
    assert.match(roots.autoapprove.innerHTML, /aa-badge[^>]*>15</);
    assert.match(roots.autoapprove.getAttribute("aria-label"), /15 approved/);
    assert.doesNotMatch(roots.autoapprove.innerHTML, /\(15\)/);
    feature.destroy();
  }
  {
    const { feature, roots } = makeFeature({
      nodes: [{ id: "c1", title: "Claude", agent: "claude", live: "quiet", attention: "" }],
      sel: "c1",
      chatPayload: {
        turns: [{ role: "user", text: "hi", record: 1 }],
        live: "quiet", delivery: "ok", source: "tmux",
        chat_started: "t", prior_turns: 0, assets: {},
        auto_approve: { supported: false, enabled: false, phase: "off", count: 0 },
      },
    });
    feature.bind();
    await feature.render();
    assert.equal(roots.autoapprove.disabled, true);
    assert.equal(roots.autoapprove.getAttribute("aria-pressed"), "false");
    assert.match(roots.autoapprove.title, /Claude/);
    assert.doesNotMatch(roots.autoapprove.innerHTML, /WARN|aa-badge/);
    feature.destroy();
  }
});

test("P4 no optimistic enable; POST success paints authoritative; failure keeps off + error", async () => {
  {
    let resolvePost;
    const postPromise = new Promise(r => { resolvePost = r; });
    let aa = { supported: true, enabled: false, phase: "off", count: 0 };
    let etag = '"e1"';
    const chatBody = () => ({
      turns: [{ role: "user", text: "hi", record: 1 }],
      live: "quiet", delivery: "ok", source: "acp",
      chat_started: "t", prior_turns: 0, assets: {},
      auto_approve: { ...aa },
    });
    const { feature, roots, apiCalls } = makeFeature({
      api: async (path, opts) => {
        apiCalls.push({ path, opts });
        if (path.includes("/auto-approve")) {
          await postPromise;
          aa = { supported: true, enabled: true, phase: "primed", count: 0 };
          etag = '"e2"';
          return { ...aa };
        }
        if (path.includes("/chat")) return chatBody();
        return {};
      },
      apiConditionalGet: async (path) => {
        apiCalls.push({ path, kind: "conditional" });
        return { status: 200, etag, data: chatBody() };
      },
    });
    feature.bind();
    await feature.render();
    assert.equal(roots.autoapprove.getAttribute("aria-pressed"), "false");
    const click = firstListener(roots.autoapprove, "click");
    const p = click();
    // Still off while in flight — no optimistic enable.
    assert.equal(roots.autoapprove.getAttribute("aria-pressed"), "false");
    resolvePost();
    await p;
    assert.equal(roots.autoapprove.getAttribute("aria-pressed"), "true");
    assert.ok(apiCalls.some(c => c.path.includes("/auto-approve") &&
      c.opts && c.opts.body && c.opts.body.includes('"enabled":true')));
    feature.destroy();
  }
  {
    const { feature, roots } = makeFeature({
      autoApproveError: "not available",
    });
    feature.bind();
    await feature.render();
    await firstListener(roots.autoapprove, "click")();
    assert.equal(roots.autoapprove.getAttribute("aria-pressed"), "false");
    assert.match(roots.autoapprove.innerHTML, /aa-error|not available/);
    feature.destroy();
  }
});

test("P4 manual disable POSTs enabled:false; selected-node routing ignores stale response", async () => {
  // Part A: disable while armed.
  {
    let aa = { supported: true, enabled: true, phase: "armed", count: 2 };
    const { feature, roots, apiCalls } = makeFeature({
      api: async (path, opts) => {
        apiCalls.push({ path, opts });
        if (path.includes("/auto-approve")) {
          const body = opts && opts.body ? JSON.parse(opts.body) : {};
          aa = body.enabled
            ? { supported: true, enabled: true, phase: "armed", count: 0 }
            : { supported: true, enabled: false, phase: "off", count: 0 };
          return { ...aa };
        }
        return {
          turns: [{ role: "user", text: "hi", record: 1 }],
          live: "active", delivery: "ok", source: "acp",
          chat_started: "t", prior_turns: 0, assets: {},
          auto_approve: { ...aa },
        };
      },
      apiConditionalGet: async () => ({
        status: 200, etag: '"d1"',
        data: {
          turns: [{ role: "user", text: "hi", record: 1 }],
          live: "active", delivery: "ok", source: "acp",
          chat_started: "t", prior_turns: 0, assets: {},
          auto_approve: { ...aa },
        },
      }),
    });
    feature.bind();
    await feature.render();
    assert.equal(roots.autoapprove.getAttribute("aria-pressed"), "true");
    await firstListener(roots.autoapprove, "click")();
    assert.equal(roots.autoapprove.getAttribute("aria-pressed"), "false");
    assert.ok(apiCalls.some(c => c.path.includes("/auto-approve") &&
      c.opts && c.opts.body && c.opts.body.includes('"enabled":false')));
    feature.destroy();
  }
  // Part B: response for node A never repaints node B.
  {
    let postResolve;
    const postGate = new Promise(r => { postResolve = r; });
    const apiCalls = [];
    const ctx = makeFeature({
      nodes: [
        { id: "g1", title: "Grok", agent: "grok", live: "quiet", attention: "" },
        { id: "g2", title: "Other", agent: "pi", live: "quiet", attention: "" },
      ],
      sel: "g1",
      api: async (path, opts) => {
        apiCalls.push({ path, opts });
        if (path.includes("/auto-approve")) {
          await postGate;
          return { supported: true, enabled: true, phase: "primed", count: 0 };
        }
        return {
          turns: [{ role: "user", text: "hi", record: 1 }],
          live: "quiet", delivery: "ok", source: "acp",
          chat_started: "t", prior_turns: 0, assets: {},
          auto_approve: { supported: true, enabled: false, phase: "off", count: 0 },
        };
      },
      apiConditionalGet: async () => ({
        status: 200, etag: '"x"',
        data: {
          turns: [{ role: "user", text: "hi", record: 1 }],
          live: "quiet", delivery: "ok", source: "acp",
          chat_started: "t", prior_turns: 0, assets: {},
          auto_approve: { supported: true, enabled: false, phase: "off", count: 0 },
        },
      }),
    });
    ctx.feature.bind();
    await ctx.feature.render();
    const clickP = firstListener(ctx.roots.autoapprove, "click")();
    ctx.setSel("g2");
    postResolve();
    await clickP;
    assert.equal(ctx.roots.autoapprove.getAttribute("aria-pressed"), "false");
    ctx.feature.destroy();
  }
});

test("P4 poll: server reset turns UI off; count repaint; 304 no DOM rebuild; ETag isolation", async () => {
  let payload = {
    turns: [{ role: "user", text: "hi", record: 1 }],
    live: "active", delivery: "ok", source: "acp",
    chat_started: "t", prior_turns: 0, assets: {},
    auto_approve: { supported: true, enabled: true, phase: "armed", count: 3 },
  };
  let etag = '"v1"';
  const { feature, roots, apiCalls } = makeFeature({
    chatConditional: async (path, held) => {
      apiCalls.push({ path, etag: held || "", kind: "conditional" });
      if (held && held === etag) return { status: 304, etag, data: null };
      return { status: 200, etag, data: { ...payload } };
    },
  });
  feature.bind();
  await feature.render();
  assert.match(roots.autoapprove.innerHTML, />3</);
  const html1 = roots.msgs.innerHTML;

  // 304: no rebuild
  await feature.render();
  assert.equal(roots.msgs.innerHTML, html1);
  assert.ok(apiCalls.filter(c => c.kind === "conditional").length >= 2);

  // Server reset → off
  etag = '"v2"';
  payload = {
    ...payload,
    auto_approve: { supported: true, enabled: false, phase: "off", count: 0 },
  };
  await feature.render();
  assert.equal(roots.autoapprove.getAttribute("aria-pressed"), "false");
  assert.doesNotMatch(roots.autoapprove.innerHTML, /aa-badge/);

  // Count repaint without needing turns change
  etag = '"v3"';
  payload = {
    ...payload,
    auto_approve: { supported: true, enabled: true, phase: "armed", count: 7 },
  };
  await feature.render();
  assert.match(roots.autoapprove.innerHTML, />7</);

  // Node switch drops validator
  feature.onSelectChange();
  etag = '"other"';
  await feature.render();
  const afterSwitch = apiCalls.filter(c => c.kind === "conditional").at(-1);
  assert.equal(afterSwitch.etag, "", "must not reuse prior node's ETag");
  feature.destroy();
});

test("P4 decisions render in live segment order; history uses same path; no bubble actions", async () => {
  const { feature, roots } = makeFeature({
    chatPayload: {
      turns: [
        { role: "user", text: "run tests", record: 1 },
        { role: "assistant", text: "ok", record: 4 },
      ],
      decisions: [sampleDecision],
      live: "quiet", delivery: "ok", source: "acp",
      chat_started: "t", prior_turns: 0, assets: {},
      auto_approve: { supported: true, enabled: false, phase: "off", count: 0 },
    },
  });
  feature.bind();
  await feature.render();
  assert.match(roots.msgs.innerHTML, /class="decision"/);
  assert.match(roots.msgs.innerHTML, /Auto-approved:/);
  assert.match(roots.msgs.innerHTML, /&lt;script&gt;/);
  assert.doesNotMatch(roots.msgs.innerHTML, /data-bact=/);
  // Decision sits among turns by record order (record 5 after turn record 4).
  const html = roots.msgs.innerHTML;
  const userAt = html.indexOf("run tests");
  const asstAt = html.indexOf(">ok<") >= 0 ? html.indexOf(">ok<") : html.indexOf("ok");
  const decAt = html.indexOf("class=\"decision\"");
  assert.ok(userAt >= 0 && decAt > asstAt && asstAt > userAt,
    "order user → assistant → decision");
  feature.destroy();
});

test("P4 history segments render decisions; toggle off does not remove durable rows", async () => {
  const { feature, roots } = makeFeature({
    chatPayload: {
      turns: [{ role: "user", text: "now", record: 10 }],
      decisions: [{
        record: 11,
        time: "t2",
        decision: {
          source: "auto", lease_id: "L2", request_id: "R2",
          title: "live cmd", selected: { key: "1", name: "Allow once", kind: "allow" },
          options: [{ key: "1", name: "Allow once", kind: "allow" }],
        },
      }],
      live: "quiet", delivery: "ok", source: "acp",
      chat_started: "2026-08-11T12:00:00Z", prior_turns: 2, assets: {},
      auto_approve: { supported: true, enabled: false, phase: "off", count: 0 },
    },
    histSegs: [{
      start: "2026-08-11T10:00:00Z",
      seam: "2026-08-11T11:00:00Z",
      reason: "clear",
      turns: [{ role: "user", text: "old", record: 1, time: "t0" }],
      decisions: [{
        record: 2,
        time: "t1",
        decision: {
          source: "auto", lease_id: "L1", request_id: "R1",
          title: "hist cmd", selected: { key: "2", name: "Allow once", kind: "allow" },
          options: [
            { key: "1", name: "Always", kind: "allow_always" },
            { key: "2", name: "Allow once", kind: "allow" },
          ],
        },
      }],
    }],
  });
  feature.bind();
  await feature.render();
  // Load history
  await feature.loadHistory("g1", "");
  await feature.render();
  assert.match(roots.msgs.innerHTML, /hist cmd|live cmd/);
  assert.match(roots.msgs.innerHTML, /class="decision"/);
  // Count badge off does not remove decisions
  assert.equal(roots.autoapprove.getAttribute("aria-pressed"), "false");
  assert.match(roots.msgs.innerHTML, /Auto-approved:/);
  feature.destroy();
});

test("P4 regressions: terminal toggle, keyrow perm buttons still work; manual path intact", async () => {
  const { feature, roots } = makeFeature({
    nodes: [{
      id: "g1", title: "Grok", agent: "grok", live: "quiet", attention: "approval",
    }],
    chatPayload: {
      turns: [{ role: "user", text: "x", record: 1 }],
      live: "quiet", delivery: "ok", source: "acp",
      attention: "approval",
      perm_title: "Execute `ls`",
      perm_options: [
        { key: "1", name: "Always", kind: "allow_always" },
        { key: "2", name: "Allow once", kind: "allow" },
        { key: "3", name: "Reject", kind: "reject" },
      ],
      perm_tool_kind: "execute",
      chat_started: "t", prior_turns: 0, assets: {},
      auto_approve: {
        supported: true, enabled: true, phase: "armed", count: 0,
        error: "auto-approve audit failed: boom",
      },
    },
  });
  feature.bind();
  await feature.render();
  // Manual permission buttons remain despite auto-approve error.
  assert.match(roots.keyrow.innerHTML, /data-key="2"/);
  assert.match(roots.keyrow.innerHTML, /permbtn/);
  assert.match(roots.autoapprove.innerHTML, /aa-error|audit failed/);
  // Terminal toggle still binds
  assert.equal(roots.termtoggle.listenerCount("click"), 1);
  assert.equal(roots.scrollend.listenerCount("click"), 1);
  assert.equal(roots.autoapprove.listenerCount("click"), 1);
  // Pending request at enable is a server concern; UI still shows keyrow.
  assert.match(roots.keyrow.innerHTML, /Allow once/);
  feature.destroy();
});

test("P4 help title uses semantic sole-allow wording", () => {
  assert.match(AUTO_APPROVE_HELP, /sole one-time/);
  assert.doesNotMatch(AUTO_APPROVE_HELP, /first one-time/);
  assert.match(indexHtml, /sole one-time approval option/);
});

test("P4 label constants and narrow visual contraction", () => {
  assert.equal(AUTO_APPROVE_LABEL_FULL, "Auto-approve this turn");
  assert.equal(AUTO_APPROVE_LABEL_NARROW, "Auto-approve");
  const html = autoApproveButtonInnerHTML(
    autoApproveChromeModel({ supported: true, enabled: true, phase: "armed", count: 0 }),
    { ICON_TOGGLE_ON: "ON", ICON_WARN: "W" },
  );
  assert.match(html, /aa-label-full[^>]*>Auto-approve this turn</);
  assert.match(html, /aa-label-narrow[^>]*>Auto-approve</);
});
