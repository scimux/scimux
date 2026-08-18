/* Characterization tests for web/js/sheets.js — pure decisions + factory
 * adapters with a small fake DOM, timers, API, and storage.
 * No browser emulator; no mutable production test/debug accessors. */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync, readdirSync, statSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import {
  LAST_DIR_KEY,
  DEFAULT_MODELS,
  DEFAULT_EFFORTS,
  cloneDefaultModels,
  applyAgentsProbe,
  agentOptionsHTML,
  modelDefaultLabel,
  modelOptionsHTML,
  effortLevelsFor,
  effortOptionsHTML,
  preservedEffortValue,
  launchSeedFrom,
  lanePreselectForActivity,
  forkRequiresLane,
  seedForkTitleText,
  forkRationaleFromTurn,
  forkPromptFromTurn,
  buildCreatePayload,
  buildHeadEditBody,
  isNoOpEditBody,
  isNoOpStationEdit,
  buildStationLabelPayload,
  buildAdoptPayload,
  createErrorField,
  submitButtonLabel,
  submitButtonHTML,
  sheetHeadLabels,
  getLastDir,
  setLastDir,
  sheetActiveAttrs,
  createSheetsFeature,
} from "../js/sheets.js";
import { esc } from "../js/format.js";
import { stopsOf, stopLabel } from "../js/lanes.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const sheetsSrc = readFileSync(join(__dirname, "../js/sheets.js"), "utf8");

/* ---------- module shape ---------- */
test("sheets.js exports factory and pure helpers; no later-feature imports", () => {
  assert.match(sheetsSrc, /export function createSheetsFeature/);
  assert.match(sheetsSrc, /Packet 7H ownership inventory/);
  assert.match(sheetsSrc, /from "\.\/format\.js"/);
  assert.match(sheetsSrc, /from "\.\/lanes\.js"/);
  assert.doesNotMatch(sheetsSrc, /from "\.\/polling\.js"/);
  assert.doesNotMatch(sheetsSrc, /from "\.\/app\.js"/);
  assert.doesNotMatch(sheetsSrc, /from "\.\/search\.js"/);
  assert.doesNotMatch(sheetsSrc, /from "\.\/notes\.js"/);
  assert.doesNotMatch(sheetsSrc, /from "\.\/cards\.js"/);
  assert.doesNotMatch(sheetsSrc, /from "\.\/map\.js"/);
  assert.doesNotMatch(sheetsSrc, /from "\.\/chat\.js"/);
  assert.doesNotMatch(sheetsSrc, /getNcParent|_modelsForTest|test\/debug/);
});

test("constants: last-dir key and static catalogs", () => {
  assert.equal(LAST_DIR_KEY, "scimux-lastdir");
  assert.deepEqual(DEFAULT_MODELS.claude, ["", "fable", "opus", "sonnet", "haiku"]);
  assert.deepEqual(DEFAULT_MODELS.grok, ["", "grok-4.5"]);
  assert.deepEqual(DEFAULT_EFFORTS.claude, ["low", "medium", "high", "xhigh", "max"]);
  assert.deepEqual(DEFAULT_EFFORTS.codex, ["low", "medium", "high"]);
  assert.deepEqual(DEFAULT_EFFORTS.grok, ["low", "medium", "high"]);
});

test("Grok model/effort options and probe replacement", () => {
  const models = cloneDefaultModels();
  assert.ok(models.grok.includes("grok-4.5"));
  const html = modelOptionsHTML(models, "grok", null, esc);
  assert.match(html, /value="grok-4\.5"/);
  const efforts = effortLevelsFor("grok", "grok-4.5", {});
  assert.deepEqual(efforts.list, ["low", "medium", "high"]);
  // Probe replaces fallbacks wholesale, including Grok.
  const me = {};
  applyAgentsProbe(models, me, {
    grok: {
      models: ["grok-4.5", "grok-code-fast-1"],
      efforts: { "grok-4.5": { levels: ["low", "high"], default: "high" } },
    },
  });
  assert.deepEqual(models.grok, ["", "grok-4.5", "grok-code-fast-1"]);
  assert.deepEqual(
    effortLevelsFor("grok", "grok-4.5", me),
    { list: ["low", "high"], default: "high" },
  );
});

/* ---------- pure: model/effort options ---------- */
test("agentOptionsHTML lists model keys escaped", () => {
  const html = agentOptionsHTML({ claude: [], "x&y": [] }, esc);
  assert.match(html, /<option>claude<\/option>/);
  assert.match(html, /<option>x&amp;y<\/option>/);
});

test("modelDefaultLabel inherits only same-agent parent model", () => {
  assert.equal(modelDefaultLabel("claude", null, esc), "(default)");
  assert.equal(modelDefaultLabel("claude", { agent: "codex", model: "gpt" }, esc), "(default)");
  assert.equal(
    modelDefaultLabel("claude", { agent: "claude", model: "opus" }, esc),
    "(inherit: opus)",
  );
});

test("modelOptionsHTML empty value uses default label", () => {
  const html = modelOptionsHTML(
    { claude: ["", "opus"] },
    "claude",
    { agent: "claude", model: "sonnet" },
    esc,
  );
  assert.match(html, /value="">\(inherit: sonnet\)/);
  assert.match(html, /value="opus">opus/);
});

test("effortLevelsFor: per-model wins, then static, then common three", () => {
  const me = {
    codex: {
      "gpt-5.4": { levels: ["minimal", "low", "high"], default: "low" },
    },
  };
  assert.deepEqual(
    effortLevelsFor("codex", "gpt-5.4", me),
    { list: ["minimal", "low", "high"], default: "low" },
  );
  assert.deepEqual(
    effortLevelsFor("claude", "opus", me),
    { list: DEFAULT_EFFORTS.claude, default: undefined },
  );
  assert.deepEqual(
    effortLevelsFor("unknown", "", {}),
    { list: ["low", "medium", "high"], default: undefined },
  );
});

test("effortOptionsHTML marks default; preservedEffortValue", () => {
  const html = effortOptionsHTML(["low", "high"], "high", esc);
  assert.match(html, /value="">/);
  assert.match(html, /value="high">high \(default\)/);
  assert.equal(preservedEffortValue(["low", "high"], "high"), "high");
  assert.equal(preservedEffortValue(["low"], "high"), null);
});

test("applyAgentsProbe replaces catalogs; empty no-op", () => {
  const models = cloneDefaultModels();
  const me = {};
  assert.equal(applyAgentsProbe(models, me, null).changed, false);
  assert.equal(applyAgentsProbe(models, me, {}).changed, false);
  assert.ok(models.claude);
  const r = applyAgentsProbe(models, me, {
    pi: { models: ["fast"], efforts: { fast: { levels: ["low"], default: "low" } } },
  });
  assert.equal(r.changed, true);
  assert.equal(models.claude, undefined);
  assert.deepEqual(models.pi, ["", "fast"]);
  assert.deepEqual(me.pi.fast.levels, ["low"]);
});

/* ---------- pure: launch seed / lane ---------- */
test("launchSeedFrom and lane/fork requirements", () => {
  assert.deepEqual(launchSeedFrom(null), { agent: "", model: "", effort: "", dir: "", has: false });
  assert.deepEqual(
    launchSeedFrom({ agent: "claude", model: "opus", effort: "high", dir: "/x" }),
    { agent: "claude", model: "opus", effort: "high", dir: "/x", has: true },
  );
  assert.equal(lanePreselectForActivity(true, "lane-a"), "");
  assert.equal(lanePreselectForActivity(false, "lane-a"), "lane-a");
  assert.equal(lanePreselectForActivity(false, ""), "");
  assert.equal(forkRequiresLane("p1", ""), true);
  assert.equal(forkRequiresLane("p1", "lane-a"), false);
  assert.equal(forkRequiresLane("", ""), false);
});

/* ---------- pure: payloads ---------- */
test("buildCreatePayload: visible fields, parent/rationale, optional lane", () => {
  const p = buildCreatePayload({
    title: "T", description: "D", agent: "claude", model: "opus",
    effort: "high", dir: " /tmp ", parent: "p1", rationale: "ev", laneID: "L",
  });
  assert.deepEqual(p, {
    title: "T", description: "D", prompt: "D",
    agent: "claude", model: "opus", effort: "high", dir: "/tmp",
    parent: "p1", rationale: "follow-up on: ev", lane_id: "L",
  });
  const plain = buildCreatePayload({
    title: "T", description: "", agent: "claude", model: "",
    effort: "", dir: "", parent: "", rationale: "x", laneID: "",
  });
  assert.equal(plain.prompt, "T");
  assert.equal(plain.rationale, "");
  assert.equal(plain.parent, "");
  assert.equal("lane_id" in plain, false);
});

test("edit payloads: head diff, station, no-op", () => {
  const cur = { title: "A", description: "B" };
  assert.deepEqual(buildHeadEditBody(cur, "A", "B"), {});
  assert.deepEqual(buildHeadEditBody(cur, "C", "B"), { title: "C" });
  assert.deepEqual(buildHeadEditBody(cur, "A", "D"), { description: "D" });
  assert.equal(isNoOpEditBody({}), true);
  assert.equal(isNoOpEditBody({ title: "C" }), false);
  assert.equal(isNoOpStationEdit({ title: "X", desc: "Y" }, "X", "Y"), true);
  assert.equal(isNoOpStationEdit({ title: "X", desc: "" }, "X", ""), true);
  assert.equal(isNoOpStationEdit({ title: "X", desc: "Y" }, "X", "Z"), false);
  assert.deepEqual(
    buildStationLabelPayload("2020-01-01T00:00:00Z", "T", "D"),
    { station: "2020-01-01T00:00:00Z", title: "T", description: "D" },
  );
});

test("buildAdoptPayload trims fields", () => {
  assert.deepEqual(buildAdoptPayload({
    session: "s1", agent: "claude", session_id: "  id  ",
    transcript: " /p ", title: "  t ", prompt: "  p ",
  }), {
    session: "s1", agent: "claude", session_id: "id",
    transcript: "/p", title: "t", prompt: "p",
  });
});

test("createErrorField and submit/head labels", () => {
  assert.equal(createErrorField("bad directory"), "dir");
  assert.equal(createErrorField("unknown path"), "dir");
  assert.equal(createErrorField("title required"), "title");
  assert.equal(createErrorField("something else"), "prompt");
  assert.equal(submitButtonLabel({ editing: false, submitting: false }), "Start");
  assert.equal(submitButtonLabel({ editing: false, submitting: true }), "Starting...");
  assert.equal(submitButtonLabel({ editing: true, submitting: false }), "Save");
  assert.equal(submitButtonLabel({ editing: true, submitting: true }), "Saving...");
  assert.deepEqual(sheetHeadLabels({ editing: false }), { head: "New activity", start: "Start" });
  assert.deepEqual(sheetHeadLabels({ editing: true, editStop: "" }), { head: "Edit activity", start: "Save" });
  assert.deepEqual(sheetHeadLabels({ editing: true, editStop: "t" }), { head: "Edit station", start: "Save" });
});

test("seedForkTitleText / rationale / prompt", () => {
  assert.equal(seedForkTitleText("  Hello\nworld"), "Hello");
  assert.equal(seedForkTitleText(""), "Follow-up");
  assert.equal(seedForkTitleText("x".repeat(100)).length, 60);
  /* forkRationaleFromTurn: edge label only — first line, 140-char cap. */
  assert.equal(forkRationaleFromTurn("a\nb"), "a");
  assert.equal(forkRationaleFromTurn("a\nb").length, 1);
  assert.equal(forkRationaleFromTurn("line".repeat(50)).length, 140);
  assert.equal(forkRationaleFromTurn("a".repeat(200)), "a".repeat(140));
  /* forkPromptFromTurn: full turn body, every line quoted; no 140-char cap. */
  assert.equal(
    forkPromptFromTurn("a\nb\nc"),
    "Following up on:\n> a\n> b\n> c\n\n",
  );
  const long = "x".repeat(500);
  assert.equal(forkPromptFromTurn(long), "Following up on:\n> " + long + "\n\n");
  assert.ok(forkPromptFromTurn(long).includes(long));
  assert.equal(forkPromptFromTurn(""), "");
  assert.equal(forkPromptFromTurn("   \n  \t  "), "");
  /* Trailing whitespace trimmed — no dangling "> " line at the end. */
  assert.equal(forkPromptFromTurn("hello\n\n  "), "Following up on:\n> hello\n\n");
  assert.doesNotMatch(forkPromptFromTurn("hello\n\n  "), /> \n\n$/);
  /* Source guard: old excerpt helper is gone from sheets.js and all of web/.
     Name assembled so this test file itself is not a false hit. */
  const oldHelper = "forkPrompt" + "FromRationale";
  assert.equal(sheetsSrc.includes(oldHelper), false, "sheets.js must not define " + oldHelper);
  const webRoot = join(__dirname, "..");
  const hits = [];
  (function walk(dir){
    for (const name of readdirSync(dir)) {
      if (name === "node_modules") continue;
      const p = join(dir, name);
      if (statSync(p).isDirectory()) walk(p);
      else if (/\.(js|html|css|md)$/.test(name)) {
        if (readFileSync(p, "utf8").includes(oldHelper)) hits.push(p);
      }
    }
  })(webRoot);
  assert.deepEqual(hits, [], oldHelper + " must not be referenced under web/");
});

test("last-dir get/set preserves throw semantics", () => {
  const m = new Map();
  const storage = {
    getItem: k => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => { m.set(k, String(v)); },
  };
  assert.equal(getLastDir(storage), null);
  setLastDir(storage, "");
  assert.equal(getLastDir(storage), null);
  setLastDir(storage, "/work");
  assert.equal(getLastDir(storage), "/work");
  assert.throws(() => getLastDir({
    getItem(){ throw new Error("quota"); },
  }), /quota/);
  assert.throws(() => setLastDir({
    setItem(){ throw new Error("quota"); },
  }, "/x"), /quota/);
});

test("sheetActiveAttrs", () => {
  assert.deepEqual(sheetActiveAttrs(true), { openClass: true, inert: false, ariaHidden: "false" });
  assert.deepEqual(sheetActiveAttrs(false), { openClass: false, inert: true, ariaHidden: "true" });
});

/* ---------- fake DOM harness ---------- */

function el(tag, props = {}){
  const node = {
    tagName: tag.toUpperCase(),
    id: props.id || "",
    className: props.className || "",
    classList: null,
    hidden: !!props.hidden,
    disabled: !!props.disabled,
    value: props.value ?? "",
    textContent: props.textContent ?? "",
    innerHTML: props.innerHTML ?? "",
    dataset: Object.assign(Object.create(null), props.dataset || {}),
    options: props.options || [],
    children: [],
    parentNode: null,
    nextElementSibling: null,
    nextSibling: null,
    attributes: Object.create(null),
    _listeners: {},
    style: props.style || {},
    inert: false,
  };
  node.classList = {
    _set: new Set(String(node.className).split(/\s+/).filter(Boolean)),
    contains(c){ return this._set.has(c); },
    add(c){ this._set.add(c); node.className = [...this._set].join(" "); },
    remove(c){ this._set.delete(c); node.className = [...this._set].join(" "); },
    toggle(c, force){
      if (force === true) this.add(c);
      else if (force === false) this.remove(c);
      else if (this.contains(c)) this.remove(c);
      else this.add(c);
    },
  };
  node.setAttribute = (k, v) => { node.attributes[k] = String(v); };
  node.removeAttribute = k => { delete node.attributes[k]; };
  node.getAttribute = k => (k in node.attributes ? node.attributes[k] : null);
  node.toggleAttribute = (k, force) => {
    if (force) node.setAttribute(k, "");
    else node.removeAttribute(k);
    if (k === "inert") node.inert = !!force;
  };
  node.matches = sel => {
    if (sel === ".sheet") return node.classList.contains("sheet");
    if (sel && sel[0] === "#") return node.id === sel.slice(1);
    return false;
  };
  node.querySelector = sel => {
    if (sel.startsWith("option[value=")){
      const m = sel.match(/option\[value="([^"]*)"\]/);
      if (!m) return null;
      return node.options.find(o => o.value === m[1]) || null;
    }
    if (sel === "input, textarea, select") return null;
    return null;
  };
  node.querySelectorAll = sel => {
    if (sel === "input, textarea, select"){
      return node._fields || [];
    }
    return [];
  };
  node.addEventListener = (type, fn) => {
    (node._listeners[type] ||= []).push(fn);
  };
  node.removeEventListener = (type, fn) => {
    const a = node._listeners[type] || [];
    node._listeners[type] = a.filter(f => f !== fn);
  };
  node.dispatch = (type, ev = {}) => {
    for (const fn of (node._listeners[type] || []).slice()) fn({ target: node, ...ev });
  };
  node.focus = () => { node._focused = true; };
  node.select = () => { node._selected = true; };
  node.setSelectionRange = (a, b) => {
    node.selectionStart = a; node.selectionEnd = b; node._range = [a, b];
  };
  node.scrollIntoView = () => { node._scrolled = true; };
  node.insertAdjacentHTML = (pos, html) => {
    if (pos === "beforeend" && tag === "select"){
      const m = html.match(/value="([^"]*)"[^>]*>([^<]*)</) || html.match(/<option>([^<]*)<\/option>/);
      if (m){
        const value = m[1] !== undefined && html.includes("value=") ? m[1] : m[1];
        const text = m[2] !== undefined ? m[2] : m[1];
        node.options.push({ value: value ?? text, textContent: text });
      } else {
        const bare = html.match(/<option>([^<]*)<\/option>/);
        if (bare) node.options.push({ value: bare[1], textContent: bare[1] });
      }
    }
  };
  node.insertAdjacentElement = (pos, child) => {
    if (pos === "afterend"){
      node.nextElementSibling = child;
      node.nextSibling = child;
      child.parentNode = node.parentNode;
    }
  };
  node.appendChild = child => {
    node.children.push(child);
    child.parentNode = node;
    return child;
  };
  node.remove = () => {
    if (node.parentNode){
      node.parentNode.children = (node.parentNode.children || []).filter(c => c !== node);
    }
    if (node._owner && node._owner.nextElementSibling === node){
      node._owner.nextElementSibling = null;
      node._owner.nextSibling = null;
    }
  };
  Object.defineProperty(node, "options", {
    get(){ return this._options || []; },
    set(v){ this._options = v; },
    configurable: true,
  });
  node._options = props.options || [];
  /* re-parse options when innerHTML assigned for selects */
  Object.defineProperty(node, "innerHTML", {
    get(){ return this._innerHTML || ""; },
    set(html){
      this._innerHTML = html;
      if (tag === "select"){
        const opts = [];
        const re = /<option(?:\s+value="([^"]*)")?[^>]*>([^<]*)<\/option>/g;
        let m;
        while ((m = re.exec(html))){
          const value = m[1] !== undefined ? m[1] : m[2];
          opts.push({ value, textContent: m[2] });
        }
        this._options = opts;
      }
    },
    configurable: true,
  });
  if (props.innerHTML) node.innerHTML = props.innerHTML;
  return node;
}

function makeSelect(id, options = [""]){
  const s = el("select", { id });
  s.innerHTML = options.map(o =>
    typeof o === "string"
      ? `<option value="${o}">${o}</option>`
      : `<option value="${o.value}">${o.label || o.value}</option>`).join("");
  return s;
}

function makeRoots(){
  const backdrop = el("div", { id: "backdrop" });
  const burger = el("button", { id: "burger" });
  const plusbtn = el("button", { id: "plusbtn" });

  const newchat = el("div", { id: "newchat", className: "sheet" });
  const nc_head = el("h3", { id: "nc_head", textContent: "New activity" });
  const nc_start = el("button", { id: "nc_start", textContent: "Start" });
  const nc_title = el("input", { id: "nc_title" });
  const nc_prompt = el("textarea", { id: "nc_prompt" });
  const nc_agent = makeSelect("nc_agent", ["claude", "codex"]);
  const nc_model = makeSelect("nc_model", [""]);
  const nc_effort = makeSelect("nc_effort", [""]);
  const nc_dir = el("input", { id: "nc_dir" });
  const nc_lane = makeSelect("nc_lane", ["", "lane-a"]);
  const nc_lane_new = el("input", { id: "nc_lane_new", hidden: true });
  const nc_lane_hint = el("div", { id: "nc_lane_hint", hidden: true });
  const nc_lane_swatch = el("span", { id: "nc_lane_swatch" });
  newchat._fields = [nc_title, nc_prompt, nc_agent, nc_model, nc_effort, nc_dir, nc_lane, nc_lane_new];

  const adopt = el("div", { id: "adopt", className: "sheet" });
  const ad_title = el("h3", { id: "ad_title", textContent: "Adopt session" });
  const ad_agent = makeSelect("ad_agent", ["claude", "pi", "opencode"]);
  ad_agent.value = "claude";
  const ad_sid = el("input", { id: "ad_sid" });
  const ad_path = el("input", { id: "ad_path" });
  const ad_titlein = el("input", { id: "ad_titlein" });
  const ad_prompt = el("textarea", { id: "ad_prompt" });
  const ad_go = el("button", { id: "ad_go", textContent: "Adopt" });
  adopt._fields = [ad_agent, ad_sid, ad_path, ad_titlein, ad_prompt];

  const menu = el("div", { id: "menu", className: "sheet" });
  menu._fields = [];

  const byId = {
    backdrop, burger, plusbtn, newchat, nc_head, nc_start, nc_title, nc_prompt,
    nc_agent, nc_model, nc_effort, nc_dir, nc_lane, nc_lane_new, nc_lane_hint, nc_lane_swatch,
    adopt, ad_title, ad_agent, ad_sid, ad_path, ad_titlein, ad_prompt, ad_go, menu,
  };

  const errNodes = new Map();
  const document = {
    getElementById(id){
      if (errNodes.has(id)) return errNodes.get(id);
      return byId[id] || null;
    },
    querySelector(sel){
      if (sel && sel[0] === "#") return document.getElementById(sel.slice(1));
      return null;
    },
    querySelectorAll(sel){
      if (sel === ".sheet") return [newchat, adopt, menu];
      return [];
    },
    createElement(tag){
      const n = el(tag);
      n.remove = () => {
        if (n.id) errNodes.delete(n.id);
        if (n._owner){
          if (n._owner.nextElementSibling === n){
            n._owner.nextElementSibling = null;
            n._owner.nextSibling = null;
          }
        }
      };
      /* track fielderr nodes by id when assigned */
      Object.defineProperty(n, "id", {
        get(){ return this._id || ""; },
        set(v){
          if (this._id) errNodes.delete(this._id);
          this._id = v;
          if (v) errNodes.set(v, n);
        },
        configurable: true,
      });
      const origInsert = null;
      return n;
    },
    contains(){ return true; },
  };

  /* fix fieldError insertAdjacentElement to register owner */
  for (const f of [...newchat._fields, ...adopt._fields]){
    f.insertAdjacentElement = (pos, child) => {
      if (pos === "afterend"){
        f.nextElementSibling = child;
        f.nextSibling = child;
        child._owner = f;
        child.parentNode = { children: [child] };
      }
    };
  }

  return {
    roots: byId,
    document,
    byId,
    sheets: [newchat, adopt, menu],
  };
}

function makeStorage(init = {}){
  const m = new Map(Object.entries(init));
  return {
    getItem: k => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => { m.set(k, String(v)); },
    removeItem: k => { m.delete(k); },
    _map: m,
  };
}

function createFeature(overrides = {}){
  const { roots, document, byId } = makeRoots();
  const storage = overrides.storage || makeStorage(overrides.storageInit || {});
  const timers = [];
  let now = 0;
  const setTimeoutFn = (fn, ms) => {
    const id = { fn, at: now + (ms || 0), cleared: false };
    timers.push(id);
    return id;
  };
  const clearTimeoutFn = id => { if (id) id.cleared = true; };
  const flush = (ms = 0) => {
    now += ms;
    for (const t of timers){
      if (!t.cleared && t.at <= now){
        t.cleared = true;
        t.fn();
      }
    }
  };

  const nodes = overrides.nodes || {
    p1: {
      id: "p1", title: "Parent", description: "Parent desc",
      agent: "claude", model: "opus", effort: "high", dir: "/parent",
      lane_id: "lane-a",
      created_at: "2020-01-01T00:00:00Z",
      stops: ["2020-01-02T00:00:00Z"],
      station_labels: {
        "2020-01-01T00:00:00Z": { title: "Frozen", desc: "Old" },
      },
    },
  };

  const effects = {
    select: [],
    setLevel: [],
    tick: 0,
    invalidateStateEtag: 0,
    invalidateCards: 0,
    invalidateChat: 0,
    invalidateMap: 0,
    renderCards: 0,
    renderChatHead: 0,
    renderMap: 0,
    updateLocal: [],
    uiMutate: [],
    alert: [],
    fillLane: [],
  };

  const apiCalls = [];
  let apiImpl = overrides.api || (async (path, opts = {}) => {
    apiCalls.push({ path, opts });
    if (path === "/api/agents") return overrides.agentsPayload || {};
    if (path === "/api/nodes" && opts.method === "POST")
      return { id: "created-1", title: JSON.parse(opts.body).title };
    if (path.startsWith("/api/nodes/") && opts.method === "PATCH"){
      const body = JSON.parse(opts.body);
      if (body.station) return { title: body.title, desc: body.description };
      return { id: path.split("/").pop(), ...body };
    }
    if (path === "/api/adopt") return { id: "adopted-1" };
    return {};
  });

  const feature = createSheetsFeature({
    roots,
    document,
    storage,
    setTimeout: setTimeoutFn,
    clearTimeout: clearTimeoutFn,
    createElement: tag => document.createElement(tag),
    esc,
    api: (...a) => apiImpl(...a),
    nodeById: id => nodes[id] || null,
    sel: () => overrides.sel ?? "p1",
    laneFilter: () => overrides.laneFilter ?? "lane-a",
    stopsOf,
    stopLabel,
    fillLaneSelect: (select, selected) => {
      effects.fillLane.push({ selected });
      if (select) select.value = selected;
    },
    readLaneChoice: (select, input) => {
      if (overrides.laneChoice) return overrides.laneChoice;
      const val = select?.value || "";
      if (val === "__new"){
        const name = input?.value.trim() || "";
        if (!name) return { error: "Enter a lane name." };
        return { laneID: "new-lane", lane: { id: "new-lane", name } };
      }
      return { laneID: val };
    },
    syncLanePicker: () => {},
    laneList: () => [{ id: "lane-a", name: "A" }],
    uiMutate: op => effects.uiMutate.push(op),
    updateLocalNode: n => effects.updateLocal.push(n),
    select: id => effects.select.push(id),
    setLevel: n => effects.setLevel.push(n),
    isDesktop: () => !!overrides.isDesktop,
    tick: async () => { effects.tick++; },
    invalidateStateEtag: () => { effects.invalidateStateEtag++; },
    invalidateCardsSig: () => { effects.invalidateCards++; },
    invalidateChat: () => { effects.invalidateChat++; },
    invalidateMap: () => { effects.invalidateMap++; },
    renderCards: () => { effects.renderCards++; },
    renderChatHead: () => { effects.renderChatHead++; },
    renderMap: () => { effects.renderMap++; },
    alert: msg => effects.alert.push(msg),
    ...overrides.deps,
  });

  return {
    feature, roots, document, byId, storage, effects, apiCalls, timers, flush, nodes,
    setApi(fn){ apiImpl = fn; },
  };
}

/* ---------- sheet inert / open / close / backdrop / burger ---------- */
test("sheet activation: open class, inert, aria-hidden, disable fields", () => {
  const { feature, byId } = createFeature();
  feature.bind();
  const sheet = byId.newchat;
  const field = byId.nc_title;
  assert.equal(sheet.classList.contains("open"), false);
  assert.equal(sheet.getAttribute("aria-hidden"), "true");
  assert.equal(field.disabled, true);
  assert.equal(field.dataset.sheetDisabledByClose, "1");

  feature.openSheet("#newchat");
  assert.equal(sheet.classList.contains("open"), true);
  assert.equal(sheet.getAttribute("aria-hidden"), "false");
  assert.equal(field.disabled, false);
  assert.equal(field.dataset.sheetDisabledByClose, undefined);
});

test("openSheet / closeSheets / backdrop / burger", () => {
  const { feature, byId } = createFeature();
  feature.bind();
  feature.openSheet("#newchat");
  assert.ok(byId.backdrop.classList.contains("on"));
  assert.ok(byId.newchat.classList.contains("open"));
  assert.equal(byId.menu.classList.contains("open"), false);

  feature.openSheet("#menu");
  assert.ok(byId.menu.classList.contains("open"));
  assert.equal(byId.newchat.classList.contains("open"), false);

  byId.backdrop.dispatch("click");
  assert.equal(byId.backdrop.classList.contains("on"), false);
  assert.equal(byId.menu.classList.contains("open"), false);

  byId.burger.dispatch("click");
  assert.ok(byId.menu.classList.contains("open"));
  assert.ok(byId.backdrop.classList.contains("on"));
});

/* ---------- agents probe / agent-model changes ---------- */
test("agents probe success rebuilds catalogs; failure is silent", async () => {
  const ctx = createFeature({
    agentsPayload: {
      pi: { models: ["fast"], efforts: { fast: { levels: ["low", "high"], default: "high" } } },
    },
  });
  ctx.feature.bind();
  await Promise.resolve();
  await Promise.resolve();
  assert.ok(ctx.apiCalls.some(c => c.path === "/api/agents"));
  assert.match(ctx.byId.nc_agent.innerHTML, /pi/);

  const ctx2 = createFeature();
  ctx2.setApi(async path => {
    if (path === "/api/agents") throw new Error("down");
    return {};
  });
  ctx2.feature.bind();
  await Promise.resolve();
  await Promise.resolve();
  assert.match(ctx2.byId.nc_agent.innerHTML, /claude/);
});

test("destroy prevents a delayed agents probe from mutating the feature", async () => {
  let release;
  const response = new Promise(resolve => { release = resolve; });
  const ctx = createFeature();
  ctx.setApi(path => path === "/api/agents" ? response : Promise.resolve({}));
  ctx.feature.bind();
  ctx.feature.destroy();
  release({ pi: { models: ["fast"] } });
  await Promise.resolve();
  await Promise.resolve();
  assert.doesNotMatch(ctx.byId.nc_agent.innerHTML, /pi/);
});

test("agent and model change rebuild model/effort menus", () => {
  const { feature, byId } = createFeature();
  feature.bind();
  byId.nc_agent.value = "codex";
  byId.nc_agent.dispatch("change");
  assert.match(byId.nc_model.innerHTML, /gpt-5\.4/);
  byId.nc_model.value = "gpt-5.4";
  byId.nc_effort.value = "medium";
  byId.nc_model.dispatch("change");
  assert.match(byId.nc_effort.innerHTML, /medium/);
  assert.equal(byId.nc_effort.value, "medium");
});

/* ---------- forks and new activity ---------- */
test("plain new activity seeds last dir and scoped lane", () => {
  const ctx = createFeature({ storageInit: { [LAST_DIR_KEY]: "/last" }, laneFilter: "lane-a" });
  ctx.feature.bind();
  ctx.byId.plusbtn.dispatch("click");
  assert.ok(ctx.byId.newchat.classList.contains("open"));
  assert.equal(ctx.byId.nc_dir.value, "/last");
  assert.equal(ctx.effects.fillLane[0].selected, "lane-a");
  assert.equal(ctx.byId.nc_lane_hint.hidden, true);
  assert.equal(ctx.byId.nc_head.textContent, "New activity");
});

test("live turn fork seeds parent, title select timer, prompt, conscious lane", () => {
  const ctx = createFeature();
  ctx.feature.bind();
  ctx.feature.forkFromTurn("Evidence line\nmore", "p1");
  assert.ok(ctx.byId.newchat.classList.contains("open"));
  assert.equal(ctx.byId.nc_title.value, "Evidence line");
  /* Full multi-line turn text lands in #nc_prompt (not a 140-char first-line excerpt). */
  assert.equal(
    ctx.byId.nc_prompt.value,
    "Following up on:\n> Evidence line\n> more\n\n",
  );
  assert.equal(ctx.byId.nc_agent.value, "claude");
  assert.equal(ctx.byId.nc_model.value, "opus");
  assert.equal(ctx.byId.nc_effort.value, "high");
  assert.equal(ctx.byId.nc_dir.value, "/parent");
  assert.equal(ctx.effects.fillLane.at(-1).selected, "");
  assert.equal(ctx.byId.nc_lane_hint.hidden, false);
  ctx.flush(0);
  assert.equal(ctx.byId.nc_title._focused, true);
  assert.equal(ctx.byId.nc_title._selected, true);
});

/* Fork seeds itself from a LIVE parent, and only from a live parent. The
   deleted-chat fork is gone with the search overlay's action bar: a chat you
   cannot open is a chat you cannot fork, and offering it was the one place the
   UI claimed otherwise. */
test("live station fork seeds from the parent node", () => {
  const ctx = createFeature();
  ctx.feature.bind();
  ctx.feature.forkFromStation("p1");
  assert.equal(ctx.byId.nc_prompt.value, "");
  assert.equal(ctx.byId.nc_dir.value, "/parent");
  assert.equal(ctx.effects.fillLane.at(-1).selected, "");
});

test("title selection timing uses setTimeout 0", () => {
  const ctx = createFeature();
  ctx.feature.bind();
  ctx.feature.forkFromTurn("Title", "p1");
  assert.equal(ctx.byId.nc_title._focused, undefined);
  assert.equal(ctx.timers.length, 1);
  ctx.flush(0);
  assert.equal(ctx.byId.nc_title._focused, true);
});

/* ---------- create success / validation / single-flight ---------- */
test("create success: payload, last dir, tick, select, phone level", async () => {
  const ctx = createFeature({ isDesktop: false });
  ctx.feature.bind();
  ctx.byId.plusbtn.dispatch("click");
  ctx.byId.nc_title.value = "Work";
  ctx.byId.nc_prompt.value = "Do it";
  ctx.byId.nc_agent.value = "claude";
  ctx.byId.nc_model.value = "opus";
  ctx.byId.nc_effort.value = "high";
  ctx.byId.nc_dir.value = "/proj";
  ctx.byId.nc_lane.value = "lane-a";
  await new Promise(r => {
    ctx.byId.nc_start.addEventListener("click", () => setTimeout(r, 0));
    ctx.byId.nc_start.dispatch("click");
  });
  /* start is async; wait microtasks */
  await Promise.resolve();
  await Promise.resolve();
  await Promise.resolve();
  const post = ctx.apiCalls.find(c => c.path === "/api/nodes" && c.opts.method === "POST");
  assert.ok(post);
  const body = JSON.parse(post.opts.body);
  assert.equal(body.title, "Work");
  assert.equal(body.prompt, "Do it");
  assert.equal(body.agent, "claude");
  assert.equal(body.model, "opus");
  assert.equal(body.effort, "high");
  assert.equal(body.dir, "/proj");
  assert.equal(body.lane_id, "lane-a");
  assert.equal(ctx.storage.getItem(LAST_DIR_KEY), "/proj");
  assert.equal(ctx.effects.tick, 1);
  assert.deepEqual(ctx.effects.select, ["created-1"]);
  assert.deepEqual(ctx.effects.setLevel, [1]);
  assert.equal(ctx.byId.newchat.classList.contains("open"), false);
});

test("Claude create with unconfirmed initial delivery preserves a recoverable composer draft", async () => {
  const ctx = createFeature({ isDesktop: false });
  ctx.setApi(async (path, opts = {}) => {
    if (path === "/api/agents") return {};
    if (path === "/api/nodes" && opts.method === "POST")
      return { id: "claude-pending", initial_delivery: "unconfirmed" };
    return {};
  });
  ctx.feature.bind();
  ctx.byId.plusbtn.dispatch("click");
  ctx.byId.nc_title.value = "Remote Claude";
  ctx.byId.nc_prompt.value = "irreplaceable long prompt";
  ctx.byId.nc_agent.value = "claude";
  ctx.byId.nc_lane.value = "lane-a";
  ctx.byId.nc_start.dispatch("click");
  await Promise.resolve();
  await Promise.resolve();
  await Promise.resolve();
  assert.equal(
    ctx.storage.getItem("scimux-draft:claude-pending"),
    "irreplaceable long prompt",
  );
  assert.deepEqual(ctx.effects.select, ["claude-pending"]);
  assert.equal(ctx.byId.newchat.classList.contains("open"), false,
    "created node is selected instead of inviting a duplicate create");
  assert.match(ctx.effects.alert.join(" "), /not confirmed|not delivered/i);
});

test("create validation: missing title, fork lane required, new-lane error", async () => {
  const ctx = createFeature();
  ctx.feature.bind();
  ctx.feature.forkFromTurn("x", "p1");
  ctx.byId.nc_title.value = "";
  ctx.byId.nc_lane.value = "";
  ctx.byId.nc_start.dispatch("click");
  await Promise.resolve();
  /* fork without lane fails first only after title check order: title after lane in original?
     Original: laneChoice error → fork lane → title. Title empty still goes through lane first. */
  assert.ok(ctx.byId.nc_lane.attributes["aria-invalid"] === "true" ||
    ctx.byId.nc_title.attributes["aria-invalid"] === "true");

  /* explicit fork lane error */
  const ctx2 = createFeature();
  ctx2.feature.bind();
  ctx2.feature.forkFromTurn("x", "p1");
  ctx2.byId.nc_title.value = "T";
  ctx2.byId.nc_lane.value = "";
  ctx2.byId.nc_start.dispatch("click");
  await Promise.resolve();
  assert.equal(ctx2.byId.nc_lane.attributes["aria-invalid"], "true");
  assert.match(ctx2.byId.nc_lane.nextElementSibling.textContent, /Pick a lane for this fork/);

  const ctx3 = createFeature({
    laneChoice: { error: "Enter a lane name." },
  });
  ctx3.feature.bind();
  ctx3.byId.plusbtn.dispatch("click");
  ctx3.byId.nc_title.value = "T";
  ctx3.byId.nc_start.dispatch("click");
  await Promise.resolve();
  assert.equal(ctx3.byId.nc_lane_new.attributes["aria-invalid"], "true");
});

test("create error maps to dir/title/prompt fields", async () => {
  const cases = [
    ["bad directory path", "nc_dir"],
    ["title is invalid", "nc_title"],
    ["agent failed", "nc_prompt"],
  ];
  for (const [msg, id] of cases){
    const ctx = createFeature();
    ctx.setApi(async path => {
      if (path === "/api/agents") return {};
      if (path === "/api/nodes") throw new Error(msg);
      return {};
    });
    ctx.feature.bind();
    ctx.byId.plusbtn.dispatch("click");
    ctx.byId.nc_title.value = "T";
    ctx.byId.nc_lane.value = "lane-a";
    ctx.byId.nc_start.dispatch("click");
    await Promise.resolve();
    await Promise.resolve();
    assert.equal(ctx.byId[id].attributes["aria-invalid"], "true", msg);
    assert.equal(ctx.byId[id]._focused, true);
  }
});

test("malformed successful create response follows the original failure path", async () => {
  const ctx = createFeature();
  ctx.setApi(async path => {
    if (path === "/api/agents") return {};
    if (path === "/api/nodes") return null;
    return {};
  });
  ctx.feature.bind();
  ctx.byId.plusbtn.dispatch("click");
  ctx.byId.nc_title.value = "T";
  ctx.byId.nc_lane.value = "lane-a";
  ctx.byId.nc_start.dispatch("click");
  await Promise.resolve();
  await Promise.resolve();
  await Promise.resolve();
  assert.deepEqual(ctx.effects.select, []);
  assert.equal(ctx.byId.nc_prompt.attributes["aria-invalid"], "true");
});

test("single-flight submission ignores re-entry", async () => {
  let release;
  const gate = new Promise(r => { release = r; });
  const ctx = createFeature();
  let posts = 0;
  ctx.setApi(async (path, opts) => {
    if (path === "/api/agents") return {};
    if (path === "/api/nodes" && opts.method === "POST"){
      posts++;
      await gate;
      return { id: "n1" };
    }
    return {};
  });
  ctx.feature.bind();
  ctx.byId.plusbtn.dispatch("click");
  ctx.byId.nc_title.value = "T";
  ctx.byId.nc_lane.value = "lane-a";
  ctx.byId.nc_start.dispatch("click");
  await Promise.resolve();
  assert.equal(ctx.byId.nc_start.disabled, true);
  /* the busy state moved off the visible title and onto the accessible name +
     the .ctadots indicator (LI-B1..B3) */
  assert.equal(ctx.byId.nc_start.getAttribute("aria-label"), "Starting...");
  assert.match(ctx.byId.nc_start.innerHTML, /class="ctadots"/);
  ctx.byId.nc_start.dispatch("click");
  await Promise.resolve();
  assert.equal(posts, 1);
  release();
  await Promise.resolve();
  await Promise.resolve();
});

/* ---------- edit modes ---------- */
test("live-head edit patches title/description; no-op closes without request", async () => {
  const ctx = createFeature();
  ctx.feature.bind();
  ctx.feature.openActivityEditor("p1");
  assert.ok(ctx.byId.newchat.classList.contains("editing"));
  assert.equal(ctx.byId.nc_head.textContent, "Edit activity");
  assert.equal(ctx.byId.nc_title.value, "Parent");
  assert.equal(ctx.byId.nc_prompt.value, "Parent desc");
  ctx.flush(0);
  assert.equal(ctx.byId.nc_title._focused, true);

  /* no-op */
  const before = ctx.apiCalls.length;
  ctx.byId.nc_start.dispatch("click");
  await Promise.resolve();
  await Promise.resolve();
  assert.equal(ctx.apiCalls.filter(c => c.opts && c.opts.method === "PATCH").length, 0);
  assert.equal(ctx.byId.newchat.classList.contains("open"), false);

  ctx.feature.openActivityEditor("p1");
  ctx.byId.nc_title.value = "Renamed";
  ctx.byId.nc_start.dispatch("click");
  await Promise.resolve();
  await Promise.resolve();
  const patch = ctx.apiCalls.find(c => c.opts && c.opts.method === "PATCH");
  assert.ok(patch);
  assert.deepEqual(JSON.parse(patch.opts.body), { title: "Renamed" });
  assert.ok(ctx.effects.updateLocal.length);
});

test("frozen-station edit patches station label; no-op closes", async () => {
  const ctx = createFeature();
  ctx.feature.bind();
  ctx.feature.openActivityEditor("p1", "2020-01-01T00:00:00Z");
  assert.equal(ctx.byId.nc_head.textContent, "Edit station");
  assert.equal(ctx.byId.nc_title.value, "Frozen");
  assert.equal(ctx.byId.nc_prompt.value, "Old");
  /* no-op */
  ctx.byId.nc_start.dispatch("click");
  await Promise.resolve();
  assert.equal(ctx.apiCalls.filter(c => c.opts && c.opts.method === "PATCH").length, 0);

  ctx.feature.openActivityEditor("p1", "2020-01-01T00:00:00Z");
  ctx.byId.nc_title.value = "New frozen";
  ctx.byId.nc_start.dispatch("click");
  await Promise.resolve();
  await Promise.resolve();
  const patch = ctx.apiCalls.find(c => c.opts && c.opts.method === "PATCH");
  assert.ok(patch);
  const body = JSON.parse(patch.opts.body);
  assert.equal(body.station, "2020-01-01T00:00:00Z");
  assert.equal(body.title, "New frozen");
  assert.equal(ctx.nodes.p1.station_labels["2020-01-01T00:00:00Z"].title, "New frozen");
});

/* ---------- adoption ---------- */
test("adoption reset, payload, success, failure", async () => {
  const ctx = createFeature();
  ctx.feature.bind();
  ctx.feature.openAdopt("sess-1");
  assert.ok(ctx.byId.adopt.classList.contains("open"));
  assert.equal(ctx.byId.ad_title.textContent, `Adopt "sess-1"`);
  assert.equal(ctx.byId.ad_agent.value, "claude");
  assert.equal(ctx.byId.ad_sid.value, "");
  ctx.byId.ad_sid.value = " uuid ";
  ctx.byId.ad_path.value = " /t ";
  ctx.byId.ad_titlein.value = " Title ";
  ctx.byId.ad_prompt.value = " Work ";
  ctx.byId.ad_go.dispatch("click");
  await Promise.resolve();
  await Promise.resolve();
  const call = ctx.apiCalls.find(c => c.path === "/api/adopt");
  assert.ok(call);
  assert.deepEqual(JSON.parse(call.opts.body), {
    session: "sess-1",
    agent: "claude",
    session_id: "uuid",
    transcript: "/t",
    title: "Title",
    prompt: "Work",
  });
  assert.deepEqual(ctx.effects.select, ["adopted-1"]);
  assert.equal(ctx.effects.tick, 1);

  const ctx2 = createFeature();
  ctx2.setApi(async path => {
    if (path === "/api/agents") return {};
    if (path === "/api/adopt") throw new Error("nope");
    return {};
  });
  ctx2.feature.bind();
  ctx2.feature.openAdopt("s");
  ctx2.byId.ad_go.dispatch("click");
  await Promise.resolve();
  await Promise.resolve();
  assert.deepEqual(ctx2.effects.alert, ["nope"]);
});

test("malformed successful adoption response follows the original failure path", async () => {
  const ctx = createFeature();
  ctx.setApi(async path => {
    if (path === "/api/agents") return {};
    if (path === "/api/adopt") return null;
    return {};
  });
  ctx.feature.bind();
  ctx.feature.openAdopt("s");
  ctx.byId.ad_go.dispatch("click");
  await Promise.resolve();
  await Promise.resolve();
  await Promise.resolve();
  assert.deepEqual(ctx.effects.select, []);
  assert.equal(ctx.effects.alert.length, 1);
  assert.match(ctx.effects.alert[0], /null|id/i);
});

/* ---------- bind/destroy ---------- */
test("bind is idempotent; destroy cleans listeners, timer, field errors", async () => {
  const ctx = createFeature();
  ctx.feature.bind();
  ctx.feature.bind();
  assert.equal(ctx.byId.plusbtn._listeners.click.length, 1);
  assert.equal(ctx.byId.backdrop._listeners.click.length, 1);

  ctx.feature.forkFromTurn("x", "p1");
  assert.equal(ctx.timers.filter(t => !t.cleared).length, 1);

  ctx.feature.fieldError(ctx.byId.nc_title, "err");
  assert.ok(ctx.byId.nc_title.nextElementSibling);

  ctx.feature.destroy();
  assert.equal(ctx.byId.plusbtn._listeners.click.length, 0);
  assert.equal(ctx.byId.backdrop._listeners.click.length, 0);
  assert.equal(ctx.timers.every(t => t.cleared), true);
  assert.equal(ctx.byId.nc_title.nextElementSibling, null);

  /* re-bind works */
  ctx.feature.bind();
  assert.equal(ctx.byId.plusbtn._listeners.click.length, 1);
});

test("lazy cross-feature effects: select/tick/render after create", async () => {
  const ctx = createFeature({ isDesktop: true });
  ctx.feature.bind();
  ctx.byId.plusbtn.dispatch("click");
  ctx.byId.nc_title.value = "T";
  ctx.byId.nc_lane.value = "lane-a";
  ctx.byId.nc_start.dispatch("click");
  await Promise.resolve();
  await Promise.resolve();
  await Promise.resolve();
  assert.equal(ctx.effects.tick, 1);
  assert.equal(ctx.effects.invalidateStateEtag, 1);
  assert.deepEqual(ctx.effects.select, ["created-1"]);
  /* desktop does not setLevel(1) */
  assert.deepEqual(ctx.effects.setLevel, []);
});

test("fork create payload includes parent and rationale", async () => {
  const ctx = createFeature();
  ctx.feature.bind();
  ctx.feature.forkFromTurn("Evidence", "p1");
  ctx.byId.nc_title.value = "Child";
  ctx.byId.nc_lane.value = "lane-a";
  ctx.byId.nc_start.dispatch("click");
  await Promise.resolve();
  await Promise.resolve();
  const post = ctx.apiCalls.find(c => c.path === "/api/nodes" && c.opts.method === "POST");
  const body = JSON.parse(post.opts.body);
  assert.equal(body.parent, "p1");
  assert.equal(body.rationale, "follow-up on: Evidence");
  assert.equal(body.agent, "claude");
  assert.equal(body.model, "opus");
});

test("missing inherited agent/model remain selectable on seed", () => {
  const ctx = createFeature({
    nodes: {
      p1: {
        id: "p1", title: "P", agent: "rare-agent", model: "rare-model",
        effort: "", dir: "/d",
      },
    },
  });
  ctx.feature.bind();
  ctx.feature.forkFromStation("p1");
  assert.ok([...ctx.byId.nc_agent.options].some(o => o.value === "rare-agent" || o.textContent === "rare-agent"));
  assert.equal(ctx.byId.nc_agent.value, "rare-agent");
  assert.ok([...ctx.byId.nc_model.options].some(o => o.value === "rare-model"));
  assert.equal(ctx.byId.nc_model.value, "rare-model");
});

test("public factory API has no mutable test accessors", () => {
  const { feature } = createFeature();
  const keys = Object.keys(feature).sort();
  assert.deepEqual(keys, [
    "bind",
    "closeSheets",
    "destroy",
    "fieldError",
    "forkFromStation",
    "forkFromTurn",
    "openActivityEditor",
    "openAdopt",
    "openNewActivity",
    "openSheet",
  ].sort());
});

/* ---------- P3: caret at the end, never select-all (sheets call sites) ---------- */

test("P3 11: seedForkTitle still selects all — the sanctioned exception", () => {
  /* Regression lock — must pass at red and green. HIG reserves select-all for
     a machine-generated guess the user is expected to replace wholesale. */
  assert.match(sheetsSrc, /scheduleTitleFocus\s*\(\s*true\s*\)/,
    "seedForkTitle still requests select-all");
  const ctx = createFeature();
  ctx.feature.bind();
  ctx.feature.forkFromTurn("Evidence line\nmore", "p1");
  ctx.flush(0);
  assert.equal(ctx.byId.nc_title._focused, true);
  assert.equal(ctx.byId.nc_title._selected, true,
    "fork-seeded New Activity title keeps select-all");
  /* The selectAll parameter must survive — cleaning it up fails the phase. */
  assert.match(sheetsSrc, /function scheduleTitleFocus\s*\(\s*selectAll\s*\)/);
});

test("P3 activity/station editor title places caret at end (scheduleTitleFocus false)", () => {
  assert.match(sheetsSrc, /from "\.\/caret\.js"/, "sheets imports caret.js");
  assert.match(sheetsSrc, /focusAtEnd\s*\(/, "false branch uses focusAtEnd");

  const ctx = createFeature();
  ctx.feature.bind();
  const title = ctx.byId.nc_title;
  title._focused = false;
  title._selected = false;
  title._range = null;
  title.setSelectionRange = function(a, b){
    this.selectionStart = a; this.selectionEnd = b; this._range = [a, b];
  };
  ctx.feature.openActivityEditor("p1");
  assert.equal(title.value, "Parent");
  ctx.flush(0);
  assert.equal(title._focused, true, "title focused on editor open");
  assert.equal(title._selected, false, "select-all only for seedForkTitle");
  assert.deepEqual(title._range, [title.value.length, title.value.length],
    "caret at end of existing activity title");
});

/* ---------- P4: openNewActivity options + no-parent create + plusbtn trap ---------- */

test("P4 case 7: create after openNewActivity carries no parent (even after a fork)", async () => {
  const ctx = createFeature();
  ctx.feature.bind();
  /* Seed parent via fork, then Start new chat must wipe lineage. */
  ctx.feature.forkFromTurn("Evidence", "p1");
  assert.ok(typeof ctx.feature.openNewActivity === "function",
    "openNewActivity is exported for bookmarks injection");
  ctx.feature.openNewActivity({ prompt: "from send-to picker", focusTitle: true });
  assert.equal(ctx.byId.nc_prompt.value, "from send-to picker");
  assert.equal(ctx.byId.nc_title.value, "");
  ctx.byId.nc_title.value = "Standalone";
  ctx.byId.nc_lane.value = "lane-a";
  ctx.byId.nc_start.dispatch("click");
  await Promise.resolve();
  await Promise.resolve();
  await Promise.resolve();
  const post = ctx.apiCalls.find(c => c.path === "/api/nodes" && c.opts.method === "POST");
  assert.ok(post, "create POSTed");
  const body = JSON.parse(post.opts.body);
  assert.equal(body.parent, "", "create payload carries no parent");
  assert.equal(body.rationale, "", "no fork rationale either");
  assert.equal(body.title, "Standalone");
  assert.equal(body.prompt, "from send-to picker");
});

test("P4 openNewActivity({ prompt, focusTitle }) prefills prompt and focuses title at end", () => {
  const ctx = createFeature();
  ctx.feature.bind();
  ctx.byId.nc_title.value = "old";
  ctx.byId.nc_prompt.value = "old";
  ctx.feature.openNewActivity({ prompt: "carried bubble text", focusTitle: true });
  assert.equal(ctx.byId.nc_prompt.value, "carried bubble text");
  assert.equal(ctx.byId.nc_title.value, "");
  assert.equal(ctx.byId.newchat.classList.contains("open"), true);
  ctx.flush(0);
  assert.equal(ctx.byId.nc_title._focused, true, "title focused via scheduleTitleFocus");
  assert.equal(ctx.byId.nc_title._selected, undefined,
    "focusTitle reuses caret-at-end path, not select-all");
});

test("P4 plusbtn click leaves nc_prompt empty (openNewActivity ignores the Event)", () => {
  /* Trap: on(plusbtn, "click", openNewActivity) would pass the MouseEvent as
     the first argument. With options destructuring that becomes a silent
     no-default object. Binding must be () => openNewActivity(). */
  assert.match(sheetsSrc, /on\(root\("plusbtn"\),\s*"click",\s*\(\)\s*=>\s*openNewActivity\(\)\)/,
    "plusbtn bound as () => openNewActivity() so the click Event is never options");
  assert.doesNotMatch(sheetsSrc, /on\(root\("plusbtn"\),\s*"click",\s*openNewActivity\)/,
    "must not pass openNewActivity bare as the click listener");
  const ctx = createFeature();
  ctx.feature.bind();
  ctx.byId.nc_prompt.value = "should be cleared";
  ctx.byId.nc_title.value = "should be cleared";
  /* dispatch passes { target: plusbtn } — same shape as a click Event. */
  ctx.byId.plusbtn.dispatch("click");
  assert.equal(ctx.byId.nc_prompt.value, "",
    "plusbtn must not feed the click Event into openNewActivity options");
  assert.equal(ctx.byId.nc_title.value, "");
  assert.equal(ctx.byId.newchat.classList.contains("open"), true);
});

/* ---------- P7: .sheet .pos-item is a 44px touch target ----------
 * Rule-body regexes only — never a file-wide min-height grep (that would
 * pass if the declaration landed on .sheet .pos-item .d or sendto-group).
 * Mutation check: move min-height into the neighbour rule and confirm red. */

const sheetsCssSrc = readFileSync(join(__dirname, "../css/sheets.css"), "utf8");

function posItemRuleBody(){
  const m = sheetsCssSrc.match(/\.sheet\s+\.pos-item\s*\{([^}]+)\}/);
  assert.ok(m, ".sheet .pos-item rule present");
  return m[1];
}

test("P7: .sheet .pos-item body has min-height: 44px", () => {
  const body = posItemRuleBody();
  assert.match(body, /min-height:\s*44px/,
    ".sheet .pos-item must declare min-height: 44px (hit-testable floor)");
});

test("P7: .sheet .pos-item body centres its items (not baseline)", () => {
  /* align-items: baseline in a 44px flex row pins the label to the top of
     the box. The lane dot already has inline align-self: center, so the
     hazard is the title, not the dot. */
  const body = posItemRuleBody();
  assert.match(body, /align-items:\s*center/,
    ".sheet .pos-item centres the label in the 44px row");
  assert.doesNotMatch(body, /align-items:\s*baseline/,
    "baseline would top-align the label once the row is 44px tall");
});

test("P7: neither 44px target fakes height with margin", () => {
  /* Margin is not hit-testable — the original .pos-item trap. Spacing
     margins (e.g. margin: 6px 0) may stay; a vertical margin that alone
     invents a ≥44px block must not stand in for min-height. */
  const posBody = posItemRuleBody();
  const menuCss = readFileSync(join(__dirname, "../css/menu.css"), "utf8");
  const pop = menuCss.match(/\.popmenu\s+button\s*\{([^}]+)\}/);
  assert.ok(pop, ".popmenu button rule present");
  const popBody = pop[1];
  for (const [name, body] of [[".sheet .pos-item", posBody], [".popmenu button", popBody]]) {
    assert.match(body, /min-height:\s*44px/,
      `${name}: height floor comes from min-height, not margin`);
    assert.doesNotMatch(body,
      /margin(?:-(?:top|bottom))?:\s*(?:4[4-9]|[5-9]\d|\d{3,})px/,
      `${name}: vertical margin must not invent the 44px target`);
    assert.doesNotMatch(body,
      /margin:\s*(?:4[4-9]|[5-9]\d|\d{3,})px\s+0\b/,
      `${name}: margin: Npx 0 with N≥44 would fake the target`);
  }
});

/* ---------- P2: modal sheet layer above every full-screen overlay ----------
 * Sheets and #backdrop lived at 60/55 while #notesworkspace and #previewview
 * sit at 110 and #searchoverlay at 120 — so any sheet opened from the
 * workspace or search painted behind them. Rule-body regexes only (never a
 * file-wide z-index grep). Mutation check: set .sheet back to 60 → red. */

const notesCssForSheetZ = readFileSync(join(__dirname, "../css/notes.css"), "utf8");
const layoutCssForSheetZ = readFileSync(join(__dirname, "../css/layout.css"), "utf8");

test("P2: the modal sheet layer sits above every full-screen overlay", () => {
  /* Bare `.sheet {` only — never `.sheet.open` / `.sheet .pos-item`. */
  const sheetRule = sheetsCssSrc.match(/(?:^|\n)\.sheet\s*\{([^}]+)\}/);
  assert.ok(sheetRule, ".sheet rule present");
  const sheetZi = sheetRule[1].match(/z-index\s*:\s*(-?\d+)/);
  assert.ok(sheetZi, ".sheet declares z-index");
  const sheetZ = Number(sheetZi[1]);

  const backdropRule = sheetsCssSrc.match(/#backdrop\s*\{([^}]+)\}/);
  assert.ok(backdropRule, "#backdrop rule present");
  const backdropZi = backdropRule[1].match(/z-index\s*:\s*(-?\d+)/);
  assert.ok(backdropZi, "#backdrop declares z-index");
  const backdropZ = Number(backdropZi[1]);

  const wsRule = notesCssForSheetZ.match(/#notesworkspace\s*\{([^}]+)\}/);
  assert.ok(wsRule, "#notesworkspace rule present");
  const wsZi = wsRule[1].match(/z-index\s*:\s*(-?\d+)/);
  assert.ok(wsZi, "#notesworkspace declares z-index");
  const wsZ = Number(wsZi[1]);

  const toastRule = notesCssForSheetZ.match(/#toast\s*\{([^}]+)\}/);
  assert.ok(toastRule, "#toast rule present");
  const toastZi = toastRule[1].match(/z-index\s*:\s*(-?\d+)/);
  assert.ok(toastZi, "#toast declares z-index");
  const toastZ = Number(toastZi[1]);

  const searchRule = layoutCssForSheetZ.match(/#searchoverlay\s*\{([^}]+)\}/);
  assert.ok(searchRule, "#searchoverlay rule present");
  const searchZi = searchRule[1].match(/z-index\s*:\s*(-?\d+)/);
  assert.ok(searchZi, "#searchoverlay declares z-index");
  const searchZ = Number(searchZi[1]);

  const previewRule = layoutCssForSheetZ.match(/#previewview\s*\{([^}]+)\}/);
  assert.ok(previewRule, "#previewview rule present");
  const previewZi = previewRule[1].match(/z-index\s*:\s*(-?\d+)/);
  assert.ok(previewZi, "#previewview declares z-index");
  const previewZ = Number(previewZi[1]);

  /* Ladder: sheet > backdrop > searchoverlay > notesworkspace == previewview,
     and toast > sheet. Target: backdrop 125, sheet 130, search 120, ws/preview
     110, toast 200. */
  assert.ok(sheetZ > backdropZ,
    `.sheet (${sheetZ}) must sit above #backdrop (${backdropZ})`);
  assert.ok(backdropZ > searchZ,
    `#backdrop (${backdropZ}) must sit above #searchoverlay (${searchZ})`);
  assert.ok(searchZ > wsZ,
    `#searchoverlay (${searchZ}) must sit above #notesworkspace (${wsZ})`);
  assert.equal(wsZ, previewZ,
    `#notesworkspace (${wsZ}) and #previewview (${previewZ}) share a tier`);
  assert.ok(toastZ > sheetZ,
    `#toast (${toastZ}) must outrank .sheet (${sheetZ})`);
});

/* ---------- Loading indicators P2/P3: the Start button and the launch echo ----------
 * Two gaps reported from an iPhone against one moment — pressing Start:
 *   P2  the button goes disabled and its label swaps to "Starting..." with no
 *       motion at all, so a slow backend is indistinguishable from a dead one.
 *   P3  the first prompt travels *with the launch config*, not through the
 *       composer, so nothing ever called setSentEcho for it. On a cold ACP
 *       model load the chat then sat empty for minutes — not even the human's
 *       own prompt was on screen.
 * HIG shape for P2: the visible label stays put (a control's title is not a
 * progress readout, and a changing title resizes the control); the motion is a
 * separate indeterminate indicator; the *accessible* name still carries the
 * state, so submitButtonLabel keeps its wording and becomes the aria-label.
 */

test("LI-B1: the visible CTA label is stable; the dots are a separate indicator", () => {
  assert.equal(submitButtonHTML({ editing: false, submitting: false }),
    `<span class="ctalabel">Start</span>`);
  assert.equal(submitButtonHTML({ editing: true, submitting: false }),
    `<span class="ctalabel">Save</span>`);
  const busy = submitButtonHTML({ editing: false, submitting: true });
  assert.match(busy, /<span class="ctalabel">Start<\/span>/,
    "the word does not change under the user — only an indicator is added");
  assert.match(busy, /class="ctadots"/);
  assert.match(submitButtonHTML({ editing: true, submitting: true }),
    /<span class="ctalabel">Save<\/span>[\s\S]*class="ctadots"/);
});

test("LI-B2: three dots, decorative to VoiceOver", () => {
  const busy = submitButtonHTML({ editing: false, submitting: true });
  assert.equal((busy.match(/<i><\/i>/g) || []).length, 3, "three dots");
  assert.match(busy, /class="ctadots" aria-hidden="true"/,
    "the accessible name carries the state; the dots must not be read out");
});

test("LI-B3: pressing Start paints dots, aria-busy and the busy accessible name", async () => {
  let release;
  const gate = new Promise(r => { release = r; });
  const ctx = createFeature();
  ctx.setApi(async (path, opts) => {
    if (path === "/api/agents") return {};
    if (path === "/api/nodes" && opts.method === "POST"){ await gate; return { id: "n1" }; }
    return {};
  });
  ctx.feature.bind();
  ctx.byId.plusbtn.dispatch("click");
  assert.match(ctx.byId.nc_start.innerHTML, /<span class="ctalabel">Start<\/span>/);
  assert.equal(ctx.byId.nc_start.getAttribute("aria-busy"), "false");

  ctx.byId.nc_title.value = "T";
  ctx.byId.nc_lane.value = "lane-a";
  ctx.byId.nc_start.dispatch("click");
  await Promise.resolve();
  assert.equal(ctx.byId.nc_start.disabled, true);
  assert.match(ctx.byId.nc_start.innerHTML, /class="ctadots"/, "the wait is animated");
  assert.match(ctx.byId.nc_start.innerHTML, /<span class="ctalabel">Start<\/span>/,
    "the label does not resize the button mid-press");
  assert.equal(ctx.byId.nc_start.getAttribute("aria-busy"), "true");
  assert.equal(ctx.byId.nc_start.getAttribute("aria-label"), "Starting...",
    "the state lives in the accessible name, not the visible title");
  release();
  await Promise.resolve(); await Promise.resolve(); await Promise.resolve();
});

test("LI-B4: the editor's Save button uses the same busy chrome", () => {
  const ctx = createFeature();
  ctx.feature.bind();
  ctx.feature.openActivityEditor("p1");
  assert.match(ctx.byId.nc_start.innerHTML, /<span class="ctalabel">Save<\/span>/,
    "resetCreateChrome/openActivityEditor must not write bare text over the structure");
});

test("LI-B5: dots animate, survive Reduce Motion, and busy is not disabled-dim", () => {
  assert.match(sheetsCssSrc, /@keyframes\s+ctadot\b/, "the dots have their own keyframes");
  const dots = sheetsCssSrc.match(/\.ctadots\s+i\s*\{([^}]+)\}/);
  assert.ok(dots, ".ctadots i rule present");
  assert.match(dots[1], /animation\s*:\s*ctadot\s/);
  assert.match(sheetsCssSrc, /\.ctadots\s+i:nth-child\(2\)\s*\{[^}]*animation-delay/,
    "staggered delays are what make it read as travelling dots");
  assert.match(sheetsCssSrc, /\.ctadots\s+i:nth-child\(3\)\s*\{[^}]*animation-delay/);
  const rm = sheetsCssSrc.match(/@media\s*\(prefers-reduced-motion:\s*reduce\)[\s\S]*?\n\}/g) || [];
  for (const b of rm)
    assert.doesNotMatch(b, /\.ctadots/,
      "an indeterminate indicator that stops moving reads as stalled");
  assert.match(sheetsCssSrc, /\.cta\[aria-busy="true"\]/,
    "busy must not look the same as unavailable: lift the :disabled dimming");
});

test("LI-C1: a started activity echoes its launch prompt before the chat opens", async () => {
  const echoes = [];
  const ctx = createFeature({
    deps: { setSentEcho: e => echoes.push(e), now: () => 1234 },
  });
  ctx.feature.bind();
  ctx.byId.plusbtn.dispatch("click");
  ctx.byId.nc_title.value = "Cold model";
  ctx.byId.nc_prompt.value = "explain the fare layer";
  ctx.byId.nc_lane.value = "lane-a";
  ctx.byId.nc_start.dispatch("click");
  for (let i = 0; i < 8; i++) await Promise.resolve();

  assert.equal(echoes.length, 1, "exactly one optimistic echo for the launch prompt");
  assert.deepEqual(echoes[0], {
    node: "created-1", text: "explain the fare layer", atts: [], at: 1234, seen: null,
  }, "same record shape composer.buildSentEcho makes; seen:null — there is no prior surface");
  assert.deepEqual(ctx.effects.select, ["created-1"]);
});

test("LI-C2: the echo is set before the selection, so the first render already has it", async () => {
  const order = [];
  const ctx = createFeature({
    deps: {
      setSentEcho: () => order.push("echo"),
      select: id => order.push("select:" + id),
    },
  });
  ctx.feature.bind();
  ctx.byId.plusbtn.dispatch("click");
  ctx.byId.nc_title.value = "T";
  ctx.byId.nc_prompt.value = "p";
  ctx.byId.nc_lane.value = "lane-a";
  ctx.byId.nc_start.dispatch("click");
  for (let i = 0; i < 8; i++) await Promise.resolve();
  assert.deepEqual(order, ["echo", "select:created-1"],
    "select() runs refreshChat; the echo must already be set or the first " +
    "paint is empty and the bubble pops in a poll later");
});

test("LI-C3: an undelivered launch prompt is never echoed as delivered", async () => {
  for (const delivery of ["not_sent", "unconfirmed"]){
    const echoes = [];
    const ctx = createFeature({ deps: { setSentEcho: e => echoes.push(e) } });
    ctx.setApi(async (path, opts) => {
      if (path === "/api/agents") return {};
      if (path === "/api/nodes" && opts.method === "POST")
        return { id: "n1", initial_delivery: delivery };
      return {};
    });
    ctx.feature.bind();
    ctx.byId.plusbtn.dispatch("click");
    ctx.byId.nc_title.value = "T";
    ctx.byId.nc_prompt.value = "p";
    ctx.byId.nc_lane.value = "lane-a";
    ctx.byId.nc_start.dispatch("click");
    for (let i = 0; i < 8; i++) await Promise.resolve();
    assert.equal(echoes.length, 0,
      `${delivery}: the prompt was restored to the draft — an echo would claim ` +
      "a delivery that did not happen");
    assert.equal(ctx.storage.getItem("scimux-draft:n1"), "p", "recovery draft still kept");
  }
});

test("LI-C4: an edit or a rejected start never mints an echo", async () => {
  const echoes = [];
  const ctx = createFeature({ deps: { setSentEcho: e => echoes.push(e) } });
  ctx.feature.bind();
  ctx.feature.openActivityEditor("p1");
  ctx.byId.nc_title.value = "Renamed";
  ctx.byId.nc_start.dispatch("click");
  for (let i = 0; i < 8; i++) await Promise.resolve();
  assert.equal(echoes.length, 0, "editing a node is not a prompt");

  ctx.feature.openNewActivity();
  ctx.byId.nc_title.value = "";
  ctx.byId.nc_start.dispatch("click");
  for (let i = 0; i < 8; i++) await Promise.resolve();
  assert.equal(echoes.length, 0, "a rejected form never launched anything");
});

test("LI-C5: sheets still imports no later feature module for the echo", () => {
  assert.doesNotMatch(sheetsSrc, /from "\.\/composer\.js"/,
    "the echo record is built as a literal here, like the draft-key contract");
  assert.match(sheetsSrc, /setSentEcho/);
});
