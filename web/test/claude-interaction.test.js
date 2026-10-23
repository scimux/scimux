/* Claude structured interaction card. Fixtures are synthetic protocol values.
   They are not copies of a live Claude session. The card under test is the
   served module, mounted by createChatFeature. */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { createChatFeature, buildChatSignature, decisionsHash, decisionRowHTML } from "../js/chat.js";
import {
  interactionSignature,
  interactionBlocksNativeKeys,
  manualDecisionRowHTML,
  createClaudeInteractionCard,
} from "../js/claude-interaction.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const chatSrc = readFileSync(join(__dirname, "../js/chat.js"), "utf8");
const cardSrc = readFileSync(join(__dirname, "../js/claude-interaction.js"), "utf8");
const appSrc = readFileSync(join(__dirname, "../js/app.js"), "utf8");
const cssSrc = readFileSync(join(__dirname, "../css/chat.css"), "utf8");

const RECORDED = {
  status: "accepted",
  delivery: "recorded",
  emitted: false,
  phase: "recorded",
};

function el(tag, attrs = {}){
  const listeners = new Map();
  let className = attrs.className || "";
  const classList = {
    _s: new Set(className.split(/\s+/).filter(Boolean)),
    add(c){ this._s.add(c); className = [...this._s].join(" "); },
    remove(c){ this._s.delete(c); className = [...this._s].join(" "); },
    contains(c){ return this._s.has(c); },
    toggle(c, on){
      if (on === undefined) on = !this._s.has(c);
      if (on) this.add(c); else this.remove(c);
    },
  };
  const node = {
    tagName: String(tag || "div").toUpperCase(),
    classList,
    style: {},
    dataset: { ...(attrs.dataset || {}) },
    hidden: !!attrs.hidden,
    disabled: !!attrs.disabled,
    checked: !!attrs.checked,
    focused: false,
    value: attrs.value || "",
    selectionStart: 0,
    selectionEnd: 0,
    textContent: attrs.textContent || "",
    type: attrs.type || "",
    name: attrs.name || "",
    scrollTop: 0,
    scrollHeight: attrs.scrollHeight || 500,
    clientHeight: attrs.clientHeight || 400,
    children: [],
    parentNode: null,
    _attrs: { ...(attrs.attrs || {}) },
    setAttribute(k, v){ this._attrs[k] = String(v); },
    getAttribute(k){ return this._attrs[k]; },
    addEventListener(type, fn){
      if (!listeners.has(type)) listeners.set(type, []);
      listeners.get(type).push({ fn });
    },
    removeEventListener(type, fn){
      listeners.set(type, (listeners.get(type) || []).filter(x => x.fn !== fn));
    },
    querySelectorAll(sel){
      const out = [];
      const walk = n => {
        if (matches(n, sel)) out.push(n);
        for (const c of n.children) walk(c);
      };
      for (const c of this.children) walk(c);
      return out;
    },
    querySelector(sel){ return this.querySelectorAll(sel)[0] || null; },
    closest(sel){
      let n = this;
      while (n){
        if (matches(n, sel)) return n;
        n = n.parentNode;
      }
      return null;
    },
    appendChild(child){ this.children.push(child); child.parentNode = this; return child; },
    remove(){
      const p = this.parentNode;
      if (p && p.children){
        const i = p.children.indexOf(this);
        if (i >= 0) p.children.splice(i, 1);
      }
      this.parentNode = null;
    },
    focus(){ this.focused = true; fire(this, "focus"); },
    click(){ fire(this, "click"); },
    _listeners: listeners,
    listenerCount(type){ return (listeners.get(type) || []).length; },
  };
  if (attrs.id) node.id = attrs.id;
  Object.defineProperty(node, "className", {
    get(){ return className; },
    set(v){
      className = String(v || "");
      classList._s = new Set(className.split(/\s+/).filter(Boolean));
    },
  });
  Object.defineProperty(node, "innerHTML", {
    configurable: true,
    get(){ return node._html || ""; },
    set(v){ node._html = String(v); node.children.length = 0; },
  });
  return node;
}

function matches(node, sel){
  if (!node || !sel) return false;
  if (sel.startsWith(".")) return !!(node.classList && node.classList.contains(sel.slice(1)));
  if (sel.startsWith("#")) return node.id === sel.slice(1);
  return String(node.tagName || "").toUpperCase() === sel.toUpperCase();
}

function fire(node, type){
  for (const { fn } of [...(node._listeners.get(type) || [])]){
    fn({ target: node, currentTarget: node, preventDefault(){}, stopPropagation(){} });
  }
}

function makeRoots(){
  const msgs = el("div", { id: "msgs" });
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
    autoapprove: el("button", { id: "autoapprove" }),
    scrollend: el("button", { id: "scrollend" }),
    workpulse: el("div", { id: "workpulse" }),
  };
}

function radioQuestion(id = "req-letter"){
  return {
    request_id: id,
    phase: "pending",
    tool: "AskUserQuestion",
    digest: "digest-letter",
    answer_until: "2026-10-21T12:00:00Z",
    native_keys: false,
    questions: [{
      id: "q1",
      text: "Pick a letter",
      header: "Letter",
      multi: false,
      preview: "shown as text",
      options: [
        { id: "q1o1", label: "Alpha", description: "the first" },
        { id: "q1o2", label: "Beta", description: "the second" },
      ],
    }],
  };
}

function multiQuestion(){
  return {
    request_id: "req-multi",
    phase: "pending",
    tool: "AskUserQuestion",
    native_keys: false,
    questions: [{
      id: "q2",
      text: "Pick many",
      multi: true,
      options: [
        { id: "q2o1", label: "Red", description: "warm" },
        { id: "q2o2", label: "Blue", description: "cool" },
        { id: "q2o3", label: "Green", description: "mid" },
      ],
    }],
  };
}

function batchQuestion(){
  return {
    request_id: "req-batch",
    phase: "pending",
    tool: "AskUserQuestion",
    native_keys: false,
    questions: [
      radioQuestion().questions[0],
      multiQuestion().questions[0],
      {
        id: "q3",
        text: "Say it",
        multi: false,
        options: [{ id: "q3o1", label: "Alpha", description: "preset" }],
      },
    ],
  };
}

function planView(){
  return {
    request_id: "req-plan",
    phase: "pending",
    tool: "ExitPlanMode",
    native_keys: false,
    plan: "Step <script>alert(1)</script>\npath /plans/not-read.md",
    modes: [
      { id: "restore", label: "Restore Claude's previous mode" },
      { id: "default", label: "Manual review (default)" },
      { id: "acceptEdits", label: "Accept edits for this session" },
    ],
  };
}

function hostileQuestion(){
  const view = radioQuestion("req-html");
  view.questions[0].text = `Choose <img src="/plans/not-read.md" onerror="alert(1)">`;
  view.questions[0].preview = `<script>alert(1)</script>`;
  view.questions[0].options[0].description = `<b>bold</b>`;
  return view;
}

function basePayload(interaction = radioQuestion()){
  return {
    turns: [{ role: "user", text: "hi", time: "t", record: 1 }],
    live: "quiet",
    delivery: "ok",
    source: "transcript",
    chat_started: "2026-10-21T00:00:00Z",
    prior_turns: 0,
    assets: {},
    perm_dialog_id: "epoch-1",
    perm_title: "Bash",
    perm_options: [{ key: "1", name: "Yes", kind: "allow" }],
    claude_interaction: interaction,
  };
}

function makeFeature(overrides = {}){
  const roots = makeRoots();
  const nodes = overrides.nodes || [{
    id: "c1", title: "Claude", agent: "claude", live: "quiet", attention: "",
  }];
  let sel = overrides.sel || "c1";
  let payload = overrides.chatPayload || basePayload();
  const posts = [];
  const calls = [];
  const api = async (path, opts) => {
    calls.push(path);
    if (String(path).includes("/claude-interaction")){
      const body = opts && opts.body ? JSON.parse(opts.body) : null;
      posts.push({ path, body });
      if (typeof overrides.onPost === "function") return overrides.onPost(body);
      return { ...RECORDED, request_id: body && body.request_id };
    }
    if (String(path).includes("/peek")) return "pane";
    if (String(path).includes("/chat")) return payload;
    return {};
  };
  const feature = createChatFeature({
    roots,
    document: overrides.document || { createElement: tag => el(tag), querySelector: () => null },
    window: {},
    CSS: { escape: s => String(s) },
    api,
    apiConditionalGet: async (path) => ({ status: 200, etag: '"e1"', data: await api(path) }),
    assetURL: p => p,
    nodes: () => nodes,
    sel: () => sel,
    selGen: () => 1,
    nodeById: id => nodes.find(n => n.id === id),
    hardAttention: () => false,
    laneColor: () => "#007AFF",
    agentLogo: () => "LOGO",
    icons: { ICON_TERM: "T", ICON_TOGGLE_OFF: "OFF", ICON_TOGGLE_ON: "ON" },
    setComposerBusy: () => {},
    setComposerClosed: () => {},
    setAttachAvail: () => {},
    getPendingJump: () => null,
    setPendingJump: () => {},
    invalidateCardsSig: () => {},
    invalidateMapSig: () => {},
    renderCards: () => {},
    renderMap: () => {},
    alert: () => {},
    tick: () => {},
    scheduleTick: () => {},
    isDesktop: () => false,
    now: () => 1_000_000,
    longpress: () => () => {},
  });
  return {
    feature, roots, posts, calls, nodes,
    setSel(v){ sel = v; },
    setPayload(v){ payload = v; },
    payload: () => payload,
  };
}

function mountCard(view, { nodeId = "c1", onPost } = {}){
  const parent = el("div", { id: "keyrow" });
  const posts = [];
  const card = createClaudeInteractionCard({
    document: { createElement: tag => el(tag) },
    api: async (path, opts) => {
      const body = opts && opts.body ? JSON.parse(opts.body) : null;
      posts.push({ path, body });
      if (onPost) return onPost(body);
      return { ...RECORDED, request_id: body && body.request_id };
    },
  });
  card.update(parent, view, { nodeId });
  return { parent, card, posts };
}

function opt(parent, optionId){
  return parent.querySelectorAll(".ci-opt").find(n => n.dataset.optionId === optionId);
}

test("interaction signature identifies the server view and blocks only structured ownership", () => {
  const view = radioQuestion();
  const sig = interactionSignature(view);
  assert.match(sig, /req-letter/);
  assert.match(sig, /q1o1/);
  assert.equal(sig, interactionSignature(view));
  assert.notEqual(sig, interactionSignature({ ...view, request_id: "req-next" }));
  assert.equal(interactionSignature(null), "");
  assert.notEqual(sig, interactionSignature({ ...view, native_keys: true }));
  assert.equal(interactionSignature({}), "");
  assert.match(interactionSignature(planView()), /req-plan/);
  const sparseSig = interactionSignature({
    request_id: "req-sparse",
    questions: [{ id: "q", options: [{ id: "o" }] }, { id: "bare" }],
    modes: null,
  });
  assert.match(sparseSig, /"phase":""/);
  assert.match(sparseSig, /"tool":""/);
  assert.match(sparseSig, /"header":""/);
  assert.match(sparseSig, /"preview":""/);
  assert.match(sparseSig, /"description":""/);
  assert.match(sparseSig, /"plan":""/);
  assert.match(sparseSig, /"delivery":""/);
  assert.match(interactionSignature({ request_id: "req-nolist", questions: null }), /req-nolist/);
  assert.equal(interactionBlocksNativeKeys(view), true);
  assert.equal(interactionBlocksNativeKeys({ ...view, native_keys: true }), false);
  assert.equal(interactionBlocksNativeKeys({ request_id: "req-letter" }), false);
  assert.equal(interactionBlocksNativeKeys({ native_keys: false }), false);
  assert.equal(interactionBlocksNativeKeys(null), false);
});

test("chat signature and decision hash include the interaction and the manual record", () => {
  const bare = buildChatSignature({ nodeId: "n" });
  const keyed = buildChatSignature({ nodeId: "n", interactionKey: "req-letter|pending" });
  assert.notEqual(bare, keyed);
  assert.equal(buildChatSignature({ nodeId: "n", interactionKey: "" }), bare);
  const manual = (feedback) => decisionsHash([{
    record: 3,
    decision: {
      request_id: "req-letter",
      source: "manual",
      manual: {
        action: "answer",
        feedback,
        selections: [
          { question_id: "q1", option_ids: ["q1o1"] },
          { question_id: "q3", free_text: "note" },
        ],
      },
    },
  }]);
  assert.notEqual(manual("one"), manual("two"));
  const auto = decisionsHash([{
    record: 1,
    decision: { request_id: "r", selected: { key: "1" }, lease_id: "L" },
  }]);
  assert.equal(auto, decisionsHash([{
    record: 1,
    decision: { request_id: "r", selected: { key: "1" }, lease_id: "L" },
  }]));
  assert.notEqual(auto, manual("one"));
  const approved = decisionsHash([{
    record: 8,
    decision: { source: "manual", manual: { action: "approve", mode: "acceptEdits", feedback: "" } },
  }]);
  const noMode = decisionsHash([{
    record: 8,
    decision: { source: "manual", manual: { action: "approve", feedback: "" } },
  }]);
  assert.notEqual(approved, noMode);
});

test("manual audit rows are not labeled Auto-approved; auto rows stay labeled", () => {
  const manual = {
    record: 4,
    time: "2026-10-21T00:00:00Z",
    decision: {
      source: "manual",
      request_id: "req-letter",
      title: "Pick a letter",
      manual: {
        action: "discuss",
        tool: "AskUserQuestion",
        feedback: "human <b>words</b>",
        app_context: "application context, not feedback",
        reviewed: "Pick a letter",
        mode: "restore",
        mode_label: "Restore Claude's previous mode",
        delivery: "recorded",
        selections: [{ question_id: "q1", question: "Pick a letter", labels: ["Alpha"], kind: "option" }],
      },
    },
  };
  const html = decisionRowHTML(manual, { escape: s => String(s), fmtTime: t => t });
  assert.match(html, /Manual/);
  assert.match(html, /human &lt;b&gt;words&lt;\/b&gt;/);
  assert.match(html, /Application context/);
  assert.match(html, /application context, not feedback/);
  assert.match(html, /Restore Claude's previous mode/);
  assert.doesNotMatch(html, /Auto-approved/);
  const direct = manualDecisionRowHTML(manual);
  assert.match(direct, /Manual/);
  assert.doesNotMatch(direct, /Auto-approved/);
  const failed = manualDecisionRowHTML({
    record: 5,
    decision: { source: "manual", request_id: "req-letter", manual: {
      action: "answer", delivery: "not_delivered",
      app_context: "Application delivery outcome: <rejected> before publication.",
    } },
  });
  assert.match(failed, /not delivered/);
  assert.match(failed, /&lt;rejected&gt;/);
  assert.doesNotMatch(failed, /Auto-approved/);
  const empty = manualDecisionRowHTML(undefined);
  assert.match(empty, /Manual/);
  assert.doesNotMatch(empty, /decision-action/);
  assert.doesNotMatch(empty, /Feedback/);
  assert.match(empty, /data-record=""/);
  assert.match(empty, /data-request=""/);
  const partial = manualDecisionRowHTML({
    decision: {
      manual: {
        selections: [
          { labels: "not-a-list", free_text: `typed <b>&"</b>`, question: "" },
          { labels: [], question: "Only question" },
          {},
        ],
      },
    },
  });
  assert.match(partial, /typed &lt;b&gt;&amp;&quot;&lt;\/b&gt;/);
  assert.match(partial, /Only question/);
  assert.doesNotMatch(partial, /Auto-approved/);
  assert.doesNotMatch(partial, /decision-action/);
  const implied = decisionRowHTML({
    record: 6,
    decision: { request_id: "req-implied", manual: { action: "cancel" } },
  }, { escape: s => String(s), fmtTime: t => t });
  assert.match(implied, /Manual/);
  assert.doesNotMatch(implied, /Auto-approved/);
  const auto = decisionRowHTML({
    record: 5,
    decision: {
      source: "auto",
      request_id: "auto-1",
      lease_id: "lease-1",
      title: "Bash: ls",
      options: [],
      selected: { name: "Allow once", kind: "allow" },
    },
  }, { escape: s => String(s), fmtTime: t => t });
  assert.match(auto, /Auto-approved/);
  assert.match(auto, /no menu offered/);
});

test("the served module is mounted from chat.js and does not import chat or app", () => {
  assert.match(chatSrc, /from "\.\/claude-interaction\.js"/);
  assert.doesNotMatch(cardSrc, /from "\.\/chat\.js"/);
  assert.doesNotMatch(cardSrc, /from "\.\/app\.js"/);
  assert.doesNotMatch(appSrc, /claude-interaction\.js/);
  assert.match(cssSrc, /#keyrow \.ci-card/);
  assert.match(cssSrc, /#keyrow \{/);
});

test("a structured question replaces native keys and escapes question text", async () => {
  const { feature, roots, calls } = makeFeature({ chatPayload: basePayload(hostileQuestion()) });
  feature.bind();
  await feature.render();
  const card = roots.keyrow.querySelector(".ci-card");
  assert.ok(card, "structured card is mounted in #keyrow");
  assert.equal(roots.keyrow.innerHTML, "");
  assert.doesNotMatch(roots.keyrow.innerHTML, /data-key/);
  assert.equal(roots.keyrow.querySelectorAll(".ci-text")[0].textContent,
    `Choose <img src="/plans/not-read.md" onerror="alert(1)">`);
  assert.equal(roots.keyrow.querySelector(".ci-preview").textContent, `<script>alert(1)</script>`);
  assert.equal(roots.keyrow.querySelector(".ci-desc").textContent, `<b>bold</b>`);
  assert.equal(roots.keyrow.querySelector("img"), null);
  assert.equal(roots.keyrow.querySelector("script"), null);
  assert.ok(!calls.some(p => String(p).includes("/plans/not-read.md")));
  feature.destroy();
});

test("native handoff and a missing interaction keep the native key path", async () => {
  const handed = radioQuestion();
  handed.native_keys = true;
  handed.phase = "handoff";
  const on = makeFeature({ chatPayload: basePayload(handed) });
  on.feature.bind();
  await on.feature.render();
  assert.equal(on.roots.keyrow.querySelector(".ci-card"), null);
  assert.match(on.roots.keyrow.innerHTML, /data-key/);
  on.feature.destroy();

  const off = makeFeature({
    chatPayload: { ...basePayload(), claude_interaction: undefined },
  });
  off.feature.bind();
  await off.feature.render();
  assert.equal(off.roots.keyrow.querySelector(".ci-card"), null);
  assert.match(off.roots.keyrow.innerHTML, /data-key/);
  off.feature.destroy();
});

test("unchanged polls keep the field and caret; a new turn restores them", async () => {
  const doc = faithfulDocument();
  const box = makeFeature({ document: doc });
  box.feature.bind();
  await box.feature.render();
  const field = box.roots.keyrow.querySelector(".ci-feedback");
  assert.ok(field);
  field.value = "keep this";
  field.selectionStart = 4;
  field.selectionEnd = 0;
  field.selectionDirection = "";
  field.focus();
  await box.feature.render();
  assert.equal(box.roots.keyrow.querySelector(".ci-feedback"), field);
  assert.equal(field.value, "keep this");
  assert.equal(field.selectionStart, 4);
  assert.equal(field.selectionEnd, 0);

  const payload = box.payload();
  payload.turns = payload.turns.concat([{ role: "assistant", text: "next", time: "t2", record: 2 }]);
  await box.feature.render();
  const again = box.roots.keyrow.querySelector(".ci-feedback");
  assert.notEqual(again, field);
  assert.equal(again.value, "keep this");
  assert.equal(again.selectionStart, 4);
  assert.equal(again.selectionEnd, 0);
  assert.equal(again.selectionDirection, "none");
  assert.equal(doc.activeElement, again);
  box.feature.destroy();
});

test("a new request id or node retires only the previous draft", async () => {
  const box = makeFeature();
  box.feature.bind();
  await box.feature.render();
  const field = box.roots.keyrow.querySelector(".ci-feedback");
  field.value = "old draft";
  fire(field, "input");
  const next = radioQuestion("req-next");
  next.questions[0].text = "Next question";
  box.setPayload(basePayload(next));
  await box.feature.render();
  const fresh = box.roots.keyrow.querySelector(".ci-feedback");
  assert.equal(fresh.value, "");
  assert.match(box.roots.keyrow.querySelector(".ci-text").textContent, /Next question/);

  fresh.value = "node draft";
  fire(fresh, "input");
  box.nodes.push({ id: "c2", title: "Other", agent: "claude", live: "quiet", attention: "" });
  box.feature.onSelectChange();
  box.setSel("c2");
  box.setPayload(basePayload(radioQuestion("req-letter")));
  await box.feature.render();
  assert.equal(box.roots.keyrow.querySelector(".ci-feedback").value, "");
  box.feature.destroy();
});

test("single, multi, and free-text answers post one complete typed batch", () => {
  const { parent, posts } = mountCard(batchQuestion());
  opt(parent, "q1o2").checked = true;
  fire(opt(parent, "q1o2"), "change");
  opt(parent, "q2o3").checked = true;
  fire(opt(parent, "q2o3"), "change");
  opt(parent, "q2o1").checked = true;
  fire(opt(parent, "q2o1"), "change");
  const free = parent.querySelectorAll(".ci-free").find(n => n.dataset.questionId === "q3");
  opt(parent, "q3o1").checked = true;
  fire(opt(parent, "q3o1"), "change");
  free.value = "  ";
  fire(free, "input");
  assert.equal(opt(parent, "q3o1").checked, true);
  free.value = "Alpha";
  free.selectionStart = 2;
  fire(free, "input");
  assert.equal(opt(parent, "q3o1").checked, false);
  parent.querySelector(".ci-answer").click();
  assert.equal(posts.length, 1);
  assert.deepEqual(posts[0].body, {
    request_id: "req-batch",
    action: "answer",
    answers: [
      { question_id: "q1", option_ids: ["q1o2"] },
      { question_id: "q2", option_ids: ["q2o1", "q2o3"] },
      { question_id: "q3", free_text: "Alpha" },
    ],
  });
  assert.equal(posts[0].path, "/api/nodes/c1/claude-interaction");
});

test("an incomplete batch and blank feedback do not post", () => {
  const incomplete = mountCard(batchQuestion());
  opt(incomplete.parent, "q1o1").checked = true;
  fire(opt(incomplete.parent, "q1o1"), "change");
  incomplete.parent.querySelector(".ci-answer").click();
  assert.equal(incomplete.posts.length, 0);
  assert.match(incomplete.parent.querySelector(".ci-status").textContent, /every question/i);

  const discuss = mountCard(radioQuestion());
  const feedback = discuss.parent.querySelector(".ci-feedback");
  feedback.value = "   ";
  fire(feedback, "input");
  discuss.parent.querySelector(".ci-discuss").click();
  assert.equal(discuss.posts.length, 0);
  assert.match(discuss.parent.querySelector(".ci-status").textContent, /feedback/i);
  feedback.value = "x".repeat(4097);
  fire(feedback, "input");
  discuss.parent.querySelector(".ci-discuss").click();
  assert.equal(discuss.posts.length, 0);
  feedback.value = "please revise";
  fire(feedback, "input");
  discuss.parent.querySelector(".ci-discuss").click();
  assert.deepEqual(discuss.posts[0].body, {
    request_id: "req-letter",
    action: "discuss",
    feedback: "please revise",
  });

  const long = mountCard(radioQuestion("req-long"));
  const typed = long.parent.querySelector(".ci-free");
  typed.value = "y".repeat(4097);
  fire(typed, "input");
  long.parent.querySelector(".ci-answer").click();
  assert.equal(long.posts.length, 0);
  typed.value = "y".repeat(4096);
  fire(typed, "input");
  long.parent.querySelector(".ci-answer").click();
  assert.equal(long.posts[0].body.answers[0].free_text.length, 4096);
});

test("Unicode feedback and free text use UTF-8 byte limits", () => {
  for (const [char, bytes] of [["界", 3], ["😀", 4]]) {
    for (const kind of ["feedback", "free"]) {
      const box = mountCard(radioQuestion(`utf8-${kind}-${bytes}`));
      const field = box.parent.querySelector(kind === "feedback" ? ".ci-feedback" : ".ci-free");
      const action = box.parent.querySelector(kind === "feedback" ? ".ci-discuss" : ".ci-answer");
      field.value = char.repeat(Math.floor(4096 / bytes) + 1);
      fire(field, "input");
      action.click();
      assert.equal(box.posts.length, 0, `${kind} accepted more than 4096 UTF-8 bytes`);
      assert.match(box.parent.querySelector(".ci-status").textContent, /too long/);
      field.value = char.repeat(Math.floor(4096 / bytes)) + "x".repeat(4096 % bytes);
      fire(field, "input");
      action.click();
      assert.equal(box.posts.length, 1, `${kind} rejected exactly 4096 UTF-8 bytes`);
    }
  }
});

test("plan approval, request changes, cancel, and terminal post only their fields", () => {
  const plan = mountCard(planView());
  assert.equal(plan.parent.querySelector(".ci-plan").textContent, planView().plan);
  assert.equal(plan.parent.querySelector("script"), null);
  const restore = plan.parent.querySelectorAll(".ci-mode").find(n => n.dataset.mode === "restore");
  const restoreLabel = restore.parentNode.querySelector(".ci-label").textContent;
  assert.match(restoreLabel, /Restore Claude's previous mode/);
  assert.doesNotMatch(restoreLabel, /Manual review/);
  plan.parent.querySelector(".ci-approve").click();
  assert.equal(plan.posts.length, 0);
  assert.match(plan.parent.querySelector(".ci-status").textContent, /mode/i);
  restore.checked = true;
  fire(restore, "change");
  plan.parent.querySelector(".ci-approve").click();
  assert.deepEqual(plan.posts[0].body, {
    request_id: "req-plan",
    action: "approve",
    mode: "restore",
  });
  assert.equal(JSON.stringify(plan.posts[0].body).includes("updatedPermissions"), false);
  assert.equal(JSON.stringify(plan.posts[0].body).includes("bypassPermissions"), false);

  const changes = mountCard(planView());
  const feedback = changes.parent.querySelector(".ci-feedback");
  feedback.value = "revise the second step";
  fire(feedback, "input");
  changes.parent.querySelector(".ci-changes").click();
  assert.deepEqual(changes.posts[0].body, {
    request_id: "req-plan",
    action: "request_changes",
    feedback: "revise the second step",
  });

  const cancel = mountCard(radioQuestion());
  cancel.parent.querySelector(".ci-cancel").click();
  assert.deepEqual(cancel.posts[0].body, { request_id: "req-letter", action: "cancel" });

  const term = mountCard(radioQuestion());
  term.parent.querySelector(".ci-terminal").click();
  assert.deepEqual(term.posts[0].body, { request_id: "req-letter", action: "terminal" });
});

async function settled(){
  for (let i = 0; i < 8; i++) await Promise.resolve();
}

test("duplicate clicks, 409, expiry, and connection errors stay distinct", async () => {
  let release;
  const gate = new Promise(r => { release = r; });
  const dup = mountCard(radioQuestion(), {
    onPost: () => gate.then(() => ({ ...RECORDED, request_id: "req-letter" })),
  });
  opt(dup.parent, "q1o1").checked = true;
  fire(opt(dup.parent, "q1o1"), "change");
  const answer = dup.parent.querySelector(".ci-answer");
  answer.click();
  answer.click();
  assert.equal(dup.posts.length, 1);
  assert.equal(answer.disabled, true);
  release();
  await settled();
  answer.click();
  assert.equal(dup.posts.length, 1);
  assert.match(dup.parent.querySelector(".ci-status").textContent, /Recorded/);
  assert.match(dup.parent.querySelector(".ci-status").textContent, /Not confirmed/);
  assert.doesNotMatch(dup.parent.querySelector(".ci-status").textContent, /Delivered/);

  const stale = mountCard(radioQuestion(), {
    onPost: () => { const err = new Error("stale interaction"); err.status = 409; throw err; },
  });
  opt(stale.parent, "q1o1").checked = true;
  fire(opt(stale.parent, "q1o1"), "change");
  stale.parent.querySelector(".ci-answer").click();
  await settled();
  assert.match(stale.parent.querySelector(".ci-status").textContent, /stale interaction/);
  stale.parent.querySelector(".ci-answer").click();
  assert.equal(stale.posts.length, 1);

  const expired = radioQuestion();
  expired.phase = "expired";
  expired.native_keys = true;
  const gone = mountCard(expired);
  assert.equal(gone.parent.querySelector(".ci-card"), null);

  const down = mountCard(radioQuestion(), {
    onPost: () => { throw new Error("connection reset"); },
  });
  const feedback = down.parent.querySelector(".ci-feedback");
  feedback.value = "still here";
  fire(feedback, "input");
  down.parent.querySelector(".ci-discuss").click();
  await settled();
  assert.match(down.parent.querySelector(".ci-status").textContent, /connection reset/);
  assert.equal(down.parent.querySelector(".ci-feedback").value, "still here");
  assert.equal(down.parent.querySelector(".ci-discuss").disabled, false);
});

test("recorded and unsupported views do not offer a second structured answer", () => {
  const recorded = radioQuestion();
  recorded.phase = "recorded";
  const box = mountCard(recorded);
  const answer = box.parent.querySelector(".ci-answer");
  assert.equal(answer.disabled, true);
  opt(box.parent, "q1o1").checked = true;
  answer.click();
  assert.equal(box.posts.length, 0);

  const bare = mountCard({
    request_id: "req-bare",
    phase: "pending",
    tool: "AskUserQuestion",
    native_keys: false,
    questions: [],
  });
  assert.equal(bare.parent.querySelector(".ci-answer"), null);
  bare.parent.querySelector(".ci-terminal").click();
  assert.deepEqual(bare.posts[0].body, { request_id: "req-bare", action: "terminal" });
});

test("mount, update, and destroy drop the card listeners", async () => {
  const box = makeFeature();
  box.feature.bind();
  await box.feature.render();
  const button = box.roots.keyrow.querySelector(".ci-cancel");
  assert.ok(button.listenerCount("click") > 0);
  box.feature.destroy();
  assert.equal(button.listenerCount("click"), 0);
  assert.equal(box.roots.keyrow.querySelector(".ci-card"), null);
});

test("the chat timeline renders a manual decision without the auto label", async () => {
  const payload = basePayload();
  delete payload.claude_interaction;
  delete payload.perm_options;
  delete payload.perm_dialog_id;
  payload.decisions = [{
    record: 2,
    time: "2026-10-21T00:00:00Z",
    decision: {
      source: "manual",
      request_id: "req-letter",
      manual: { action: "answer", feedback: "", selections: [{ question: "Pick a letter", labels: ["Alpha"] }] },
    },
  }];
  const box = makeFeature({ chatPayload: payload });
  box.feature.bind();
  await box.feature.render();
  assert.match(box.roots.msgs.innerHTML, /Manual/);
  assert.doesNotMatch(box.roots.msgs.innerHTML, /Auto-approved/);
  assert.equal(box.roots.keyrow.querySelector(".ci-card"), null);
  box.feature.destroy();
});

test("sparse questions and plans keep typed fallbacks and reject incomplete answers", () => {
  const sparse = mountCard({
    request_id: "req-sparse",
    phase: "pending",
    tool: "AskUserQuestion",
    native_keys: false,
    questions: [
      { id: "qm", multi: true, options: null },
      { id: "qs", multi: false, options: [{ id: "s1" }] },
      { id: "qf", multi: false, options: [] },
    ],
  });
  assert.equal(sparse.parent.querySelectorAll(".ci-text")[0].textContent, "");
  assert.equal(sparse.parent.querySelector(".ci-label").textContent, "");
  const free = sparse.parent.querySelector(".ci-free");
  free.value = "";
  fire(free, "input");
  sparse.parent.querySelector(".ci-discuss").click();
  assert.equal(sparse.posts.length, 0);
  assert.match(sparse.parent.querySelector(".ci-status").textContent, /Feedback is required/);
  sparse.parent.querySelector(".ci-answer").click();
  assert.equal(sparse.posts.length, 0);
  assert.match(sparse.parent.querySelector(".ci-status").textContent, /every question/i);

  const mutated = radioQuestion("req-mut");
  const changed = mountCard(mutated);
  mutated.questions = null;
  changed.parent.querySelector(".ci-answer").click();
  assert.equal(changed.posts.length, 0);
  assert.match(changed.parent.querySelector(".ci-status").textContent, /every question/i);

  const open = mountCard({
    request_id: "req-open",
    phase: "pending",
    tool: "AskUserQuestion",
    native_keys: false,
    questions: null,
  });
  assert.equal(open.parent.querySelector(".ci-answer"), null);
  assert.match(open.parent.querySelector(".ci-text").textContent, /Use the terminal/);
  open.card.capture();
  open.parent.querySelector(".ci-terminal").click();
  assert.deepEqual(open.posts[0].body, { request_id: "req-open", action: "terminal" });

  const plan = mountCard({
    request_id: "req-plan-sparse",
    phase: "claimed",
    tool: "ExitPlanMode",
    native_keys: false,
    modes: [{ id: "", label: "" }, { id: "default" }],
  });
  assert.equal(plan.parent.querySelector(".ci-plan").textContent, "");
  const blank = plan.parent.querySelectorAll(".ci-mode").find(n => n.dataset.mode === "");
  const manual = plan.parent.querySelectorAll(".ci-mode").find(n => n.dataset.mode === "default");
  assert.equal(blank.parentNode.querySelector(".ci-label").textContent, "");
  assert.equal(manual.parentNode.querySelector(".ci-label").textContent, "");
  blank.checked = true;
  fire(blank, "change");
  plan.card.capture();
  plan.parent.querySelector(".ci-approve").click();
  assert.equal(plan.posts.length, 0);
  assert.match(plan.parent.querySelector(".ci-status").textContent, /mode/i);
  manual.checked = true;
  fire(manual, "change");
  plan.parent.querySelector(".ci-approve").click();
  assert.deepEqual(plan.posts[0].body, {
    request_id: "req-plan-sparse",
    action: "approve",
    mode: "default",
  });

  const barePlan = mountCard({
    request_id: "req-plan-nomode",
    phase: "pending",
    tool: "ExitPlanMode",
    native_keys: false,
    modes: null,
  });
  assert.equal(barePlan.parent.querySelector(".ci-mode"), null);
  assert.equal(barePlan.parent.querySelector(".ci-plan").textContent, "");
  barePlan.parent.querySelector(".ci-approve").click();
  assert.equal(barePlan.posts.length, 0);
  assert.match(barePlan.parent.querySelector(".ci-status").textContent, /mode/i);
});

test("a card without a document or a current request does not mount", () => {
  const missing = createClaudeInteractionCard();
  const parent = el("div");
  missing.update(parent, radioQuestion(), { nodeId: "c1" });
  missing.update(null, radioQuestion());
  missing.capture();
  missing.unmount();
  missing.retire();
  missing.destroy();
  assert.equal(parent.querySelector(".ci-card"), null);

  const card = createClaudeInteractionCard({
    document: { createElement: tag => el(tag) },
    api: async () => ({ ...RECORDED }),
  });
  card.update(null, radioQuestion(), { nodeId: "c1" });
  card.update(parent, null, { nodeId: "c1" });
  card.update(parent, { native_keys: false }, { nodeId: "c1" });
  card.update(parent, { request_id: "req-native", native_keys: true }, { nodeId: "c1" });
  assert.equal(parent.children.length, 0);
  card.update(parent, radioQuestion("req-meta"));
  assert.equal(parent.querySelector(".ci-text").textContent, "Pick a letter");
});

test("the mounted chat card posts a typed answer through the chat request seam", async () => {
  const box = makeFeature();
  box.feature.bind();
  await box.feature.render();
  opt(box.roots.keyrow, "q1o1").checked = true;
  fire(opt(box.roots.keyrow, "q1o1"), "change");
  box.roots.keyrow.querySelector(".ci-answer").click();
  await settled();
  assert.deepEqual(box.posts[0].body, {
    request_id: "req-letter",
    action: "answer",
    answers: [{ question_id: "q1", option_ids: ["q1o1"] }],
  });
  assert.equal(box.posts[0].path, "/api/nodes/c1/claude-interaction");
  assert.match(box.roots.keyrow.querySelector(".ci-status").textContent, /Not confirmed/);
  box.feature.destroy();
});

test("a rebuilt card restores mode, focus, and only the original questions", async () => {
  const planBox = makeFeature({ chatPayload: basePayload(planView()) });
  planBox.feature.bind();
  await planBox.feature.render();
  const restore = planBox.roots.keyrow.querySelectorAll(".ci-mode").find(n => n.dataset.mode === "restore");
  restore.checked = true;
  fire(restore, "change");
  const planPayload = planBox.payload();
  planPayload.turns = planPayload.turns.concat([{ role: "assistant", text: "next", time: "t2", record: 2 }]);
  await planBox.feature.render();
  assert.equal(planBox.roots.keyrow.querySelectorAll(".ci-mode").find(n => n.dataset.mode === "restore").checked, true);
  assert.equal(planBox.roots.keyrow.querySelectorAll(".ci-mode").find(n => n.dataset.mode === "default").checked, false);
  planBox.feature.destroy();

  const askDoc = faithfulDocument();
  const ask = makeFeature({ document: askDoc });
  ask.feature.bind();
  await ask.feature.render();
  const free = ask.roots.keyrow.querySelector(".ci-free");
  const feedback = ask.roots.keyrow.querySelector(".ci-feedback");
  free.value = "Hi";
  free.selectionStart = 1;
  free.selectionEnd = 2;
  free.selectionDirection = "forward";
  free.focus();
  feedback.value = "";
  feedback.selectionStart = 0;
  feedback.selectionEnd = 0;
  opt(ask.roots.keyrow, "q1o1").checked = true;
  const grown = radioQuestion();
  grown.questions.push({ id: "q-extra", text: "", multi: false, options: [] });
  grown.questions.push({
    id: "q-opt",
    text: "Extra",
    multi: false,
    options: [{ id: "e1", label: "" }],
  });
  ask.setPayload({
    ...basePayload(grown),
    turns: basePayload().turns.concat([{ role: "assistant", text: "next", time: "t2", record: 2 }]),
  });
  await ask.feature.render();
  assert.equal(opt(ask.roots.keyrow, "q1o1").checked, true);
  const kept = ask.roots.keyrow.querySelectorAll(".ci-free").find(n => n.dataset.questionId === "q1");
  assert.equal(kept.value, "Hi");
  assert.equal(kept.selectionStart, 1);
  assert.equal(kept.selectionEnd, 2);
  assert.equal(kept.selectionDirection, "forward");
  assert.equal(askDoc.activeElement, kept);
  assert.equal(ask.roots.keyrow.querySelector(".ci-feedback").value, "");
  assert.equal(ask.roots.keyrow.querySelector(".ci-feedback").selectionStart, 0);
  assert.equal(ask.roots.keyrow.querySelector(".ci-feedback").selectionEnd, 0);
  const extra = ask.roots.keyrow.querySelectorAll(".ci-free").find(n => n.dataset.questionId === "q-extra");
  assert.equal(extra.value, "");
  assert.equal(opt(ask.roots.keyrow, "e1").checked, false);
  assert.equal(opt(ask.roots.keyrow, "e1").parentNode.querySelector(".ci-label").textContent, "");
  ask.feature.destroy();
});

test("a recorded status stays on the same request and clears on the next one", async () => {
  const view = radioQuestion("req-hold");
  const box = mountCard(view);
  opt(box.parent, "q1o1").checked = true;
  fire(opt(box.parent, "q1o1"), "change");
  box.parent.querySelector(".ci-answer").click();
  await settled();
  assert.match(box.parent.querySelector(".ci-status").textContent, /Not confirmed/);
  box.card.update(box.parent, radioQuestion("req-hold"), { nodeId: "c1" });
  assert.match(box.parent.querySelector(".ci-status").textContent, /Not confirmed/);
  box.card.update(box.parent, radioQuestion("req-next"), { nodeId: "c1" });
  assert.equal(box.parent.querySelector(".ci-status").textContent, "");
});

function faithfulDocument(){
  const doc = { activeElement: null, querySelector(){ return null; } };
  doc.createElement = tag => {
    const node = el(tag);
    const rawRemove = node.remove.bind(node);
    node.focus = () => {
      doc.activeElement = node;
      node.focused = false;
    };
    node.setSelectionRange = (start, end, direction) => {
      node.selectionStart = start;
      node.selectionEnd = end;
      node.selectionDirection = direction || "none";
    };
    node.remove = () => {
      const drop = current => {
        if (doc.activeElement === current) doc.activeElement = null;
        for (const child of current.children || []) drop(child);
      };
      drop(node);
      rawRemove();
    };
    return node;
  };
  return doc;
}

test("real DOM focus and selection survive a chat repaint", async () => {
  const doc = faithfulDocument();
  const box = makeFeature({ document: doc });
  box.feature.bind();
  await box.feature.render();
  const field = box.roots.keyrow.querySelector(".ci-free");
  field.value = "abcdef";
  field.selectionStart = 1;
  field.selectionEnd = 4;
  field.selectionDirection = "backward";
  field.focus();
  assert.equal(field.focused, false, "the fake must not invent a focused flag");
  assert.equal(doc.activeElement, field);
  const payload = box.payload();
  payload.turns = payload.turns.concat([{ role: "assistant", text: "next", time: "t2", record: 2 }]);
  await box.feature.render();
  const next = box.roots.keyrow.querySelector(".ci-free");
  assert.equal(doc.activeElement, next);
  assert.equal(next.selectionStart, 1);
  assert.equal(next.selectionEnd, 4);
  assert.equal(next.selectionDirection, "backward");
  assert.equal(next.value, "abcdef");
  box.feature.destroy();
});

test("an old response cannot mark a successor question recorded", async () => {
  let resolvePost;
  const pending = new Promise(resolve => { resolvePost = resolve; });
  const box = makeFeature({ onPost: () => pending });
  box.feature.bind();
  await box.feature.render();
  box.roots.keyrow.querySelector(".ci-opt").checked = true;
  box.roots.keyrow.querySelector(".ci-answer").click();
  box.nodes.push({ id: "c2", title: "Other", agent: "claude", live: "quiet", attention: "" });
  box.feature.onSelectChange();
  box.setSel("c2");
  box.setPayload(basePayload(radioQuestion("req-successor")));
  await box.feature.render();
  resolvePost({ status: "accepted", delivery: "recorded", emitted: false });
  await new Promise(r => setImmediate(r));
  assert.equal(box.roots.keyrow.querySelector(".ci-status").textContent, "");
  assert.equal(box.roots.keyrow.querySelector(".ci-answer").disabled, false);
  box.feature.destroy();
});

test("an old failure or 409 cannot change the successor card", async () => {
  let rejectPost;
  const pending = new Promise((_, reject) => { rejectPost = reject; });
  const box = makeFeature({ onPost: () => pending });
  box.feature.bind();
  await box.feature.render();
  box.roots.keyrow.querySelector(".ci-cancel").click();
  box.feature.onSelectChange();
  box.setPayload(basePayload(radioQuestion("req-after-failure")));
  await box.feature.render();
  const err = new Error("replaced");
  err.status = 409;
  rejectPost(err);
  await new Promise(r => setImmediate(r));
  assert.equal(box.roots.keyrow.querySelector(".ci-status").textContent, "");
  assert.equal(box.roots.keyrow.querySelector(".ci-cancel").disabled, false);
  box.feature.destroy();
});

test("a missing selection end stays on the caret during repaint", async () => {
  const doc = faithfulDocument();
  const box = makeFeature({ document: doc });
  box.feature.bind();
  await box.feature.render();
  const field = box.roots.keyrow.querySelector(".ci-free");
  field.value = "abcdef";
  field.selectionStart = 2;
  field.selectionEnd = null;
  field.selectionDirection = "forward";
  field.focus();
  const payload = box.payload();
  payload.turns = payload.turns.concat([{ role: "assistant", text: "next", time: "t2", record: 2 }]);
  await box.feature.render();
  const next = box.roots.keyrow.querySelector(".ci-free");
  assert.equal(next.selectionStart, 2);
  assert.equal(next.selectionEnd, 2);
  assert.equal(next.selectionDirection, "forward");
  assert.equal(next.value, "abcdef");
  box.feature.destroy();
});

test("leaving the structured card drops a late success for that request", async () => {
  let resolvePost;
  const pending = new Promise(resolve => { resolvePost = resolve; });
  const box = makeFeature({ onPost: () => pending });
  box.feature.bind();
  await box.feature.render();
  box.roots.keyrow.querySelector(".ci-opt").checked = true;
  box.roots.keyrow.querySelector(".ci-answer").click();
  const paused = box.payload();
  box.setPayload({ ...paused, claude_interaction: null });
  await box.feature.render();
  assert.equal(box.roots.keyrow.querySelector(".ci-card"), null);
  box.setPayload(paused);
  await box.feature.render();
  resolvePost({ status: "accepted", delivery: "recorded", emitted: false });
  await new Promise(r => setImmediate(r));
  assert.equal(box.roots.keyrow.querySelector(".ci-status").textContent, "");
  assert.equal(box.roots.keyrow.querySelector(".ci-answer").disabled, false);
  box.feature.destroy();
});

test("dismissing through update drops a late success", async () => {
  let resolvePost;
  const pending = new Promise(resolve => { resolvePost = resolve; });
  const doc = faithfulDocument();
  const parent = doc.createElement("div");
  const card = createClaudeInteractionCard({
    document: doc,
    api: () => pending,
  });
  const view = radioQuestion("req-dismiss");
  card.update(parent, view, { nodeId: "n1" });
  parent.querySelector(".ci-cancel").click();
  card.update(parent, { ...view, native_keys: true }, { nodeId: "n1" });
  assert.equal(parent.querySelector(".ci-card"), null);
  card.update(parent, view, { nodeId: "n1" });
  resolvePost({ status: "accepted" });
  await new Promise(r => setImmediate(r));
  assert.equal(parent.querySelector(".ci-status").textContent, "");
});

test("destroying the card drops a late success", async () => {
  let resolvePost;
  const pending = new Promise(resolve => { resolvePost = resolve; });
  const doc = faithfulDocument();
  const parent = doc.createElement("div");
  const card = createClaudeInteractionCard({
    document: doc,
    api: () => pending,
  });
  card.update(parent, radioQuestion("req-old"), { nodeId: "n1" });
  parent.querySelector(".ci-cancel").click();
  card.destroy();
  card.update(parent, radioQuestion("req-new"), { nodeId: "n2" });
  resolvePost({ status: "accepted" });
  await new Promise(r => setImmediate(r));
  assert.equal(parent.querySelector(".ci-status").textContent, "");
});

test("a reply without a message stays unconfirmed, and destroying the card does not throw", async () => {
  const quiet = mountCard(radioQuestion("req-quiet"), {
    onPost: () => { throw {}; },
  });
  quiet.parent.querySelector(".ci-cancel").click();
  await settled();
  assert.match(quiet.parent.querySelector(".ci-status").textContent, /could not be recorded/);
  assert.equal(quiet.parent.querySelector(".ci-cancel").disabled, false);
  quiet.parent.querySelector(".ci-cancel").click();
  await settled();
  assert.equal(quiet.posts.length, 2);

  let release;
  const gate = new Promise(resolve => { release = resolve; });
  const gone = mountCard(radioQuestion("req-gone"), {
    onPost: () => gate.then(() => ({ ...RECORDED, request_id: "req-gone" })),
  });
  gone.parent.querySelector(".ci-cancel").click();
  assert.equal(gone.posts.length, 1);
  gone.card.destroy();
  release();
  await settled();
  assert.equal(gone.parent.querySelector(".ci-card"), null);
});
