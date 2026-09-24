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
  museModelSelectable,
  museFreshDefaultId,
  museModelOptionsHTML,
  effortLevelsFor,
  effortOptionsHTML,
  preservedEffortValue,
  seededEffortValue,
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
    { list: ["low", "high"], default: "high", required: false },
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
    { list: ["minimal", "low", "high"], default: "low", required: false },
  );
  assert.deepEqual(
    effortLevelsFor("claude", "opus", me),
    { list: DEFAULT_EFFORTS.claude, default: undefined, required: false },
  );
  assert.deepEqual(
    effortLevelsFor("unknown", "", {}),
    { list: ["low", "medium", "high"], default: undefined, required: false },
  );
  /* Cursor's levels only exist per model, because the level is part of the
     model id. A row that has none (`auto`) must offer none: the generic
     low/medium/high would name three ids cursor would then refuse. */
  assert.deepEqual(
    effortLevelsFor("cursor", "auto", { cursor: {} }),
    { list: [], default: undefined, required: false },
  );
  assert.deepEqual(
    effortLevelsFor("cursor", "demo", { cursor: { demo: { levels: ["high", "xhigh"], default: "high" } } }),
    { list: ["high", "xhigh"], default: "high", required: false },
  );
});

test("effortOptionsHTML marks default; preservedEffortValue", () => {
  const html = effortOptionsHTML(["low", "high"], "high", esc);
  /* A required menu drops the empty option: there is no id it could mean. */
  const req = effortOptionsHTML(["low", "high"], "low", esc, true);
  assert.doesNotMatch(req, /<option value=""/);
  assert.match(req, /value="low">low \(default\)/);
  assert.match(html, /value="">/);
  assert.match(html, /value="high">high \(default\)/);
  assert.equal(preservedEffortValue(["low", "high"], "high"), "high");
  assert.equal(preservedEffortValue(["low"], "high"), null);
  /* A rebuild keeps a level the new menu still has, blank where blank is a
     real choice, and the default where it is not. */
  const menu = { list: ["low", "medium", "high"], dflt: "low" };
  assert.equal(seededEffortValue({ ...menu, required: true, want: "high" }), "high");
  assert.equal(seededEffortValue({ ...menu, required: true, want: "" }), "low");
  assert.equal(seededEffortValue({ ...menu, required: true, want: "max" }), "low");
  assert.equal(seededEffortValue({ ...menu, required: false, want: "" }), "");
  assert.equal(seededEffortValue({ ...menu, required: false, want: "max" }), "");
  /* Required with nothing published still has to name something on the menu. */
  assert.equal(seededEffortValue({ list: ["xhigh"], dflt: "", required: true, want: "" }), "xhigh");
  assert.equal(seededEffortValue({ list: [], dflt: "", required: true, want: "" }), "");
});

test("applyAgentsProbe replaces catalogs; successful empty clears them", () => {
  const models = cloneDefaultModels();
  const me = {};
  assert.equal(applyAgentsProbe(models, me, null).changed, false);
  assert.ok(models.claude);
  const r = applyAgentsProbe(models, me, {
    pi: { models: ["fast"], efforts: { fast: { levels: ["low"], default: "low" } } },
  });
  assert.equal(r.changed, true);
  assert.equal(models.claude, undefined);
  assert.deepEqual(models.pi, ["", "fast"]);
  assert.deepEqual(me.pi.fast.levels, ["low"]);

  const empty = applyAgentsProbe(models, me, {});
  assert.equal(empty.changed, true);
  assert.deepEqual(models, {});
  assert.deepEqual(me, {});
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

test("createErrorField and submit/head labels", () => {
  assert.equal(createErrorField("bad directory"), "dir");
  assert.equal(createErrorField("unknown path"), "dir");
  assert.equal(createErrorField("title required"), "title");
  // A launch refused for the model or the effort points at the select that
  // chose it; parking it under the prompt sends the user to edit the one
  // field that was fine.
  assert.equal(createErrorField('cursor has no model "example-flash" at effort "max"', 400), "model");
  assert.equal(createErrorField("effort is not offered", 400), "effort");
  assert.equal(createErrorField("something else"), "prompt");
  /* A dsh launch refused on the wire names the knob it refused. Routing that
     to the prompt would ask the user to repair the one field that was fine.
     Only an HTTP 400 is that refusal: the server answers 400 exactly when the
     choice itself was wrong, and 500 when the same call failed for a reason
     the user's model and effort had nothing to do with (docs/http-api.md). */
  assert.equal(createErrorField(
    'acp: agent rejected the requested session configuration: model "ds/b" is not one this agent offers',
    400), "model");
  assert.equal(createErrorField(
    'acp: agent rejected the requested session configuration: model "ds/b" was chosen but this agent advertises no model option',
    400), "model");
  assert.equal(createErrorField(
    'acp: agent rejected the requested session configuration: effort "medium" is not one this agent offers',
    400), "effort");
  assert.equal(createErrorField(
    'acp: agent rejected the requested session configuration: agent refused effort "max": {"code":-32602}',
    400), "effort");
  /* The effort is the knob that failed even when the message names the model
     it failed for. */
  assert.equal(createErrorField('model "ds/b" has no effort level "medium"', 400), "effort");
  /* A broken agent (internal error, cancellation, dead transport) is a 500,
     and its message still names the model or effort the failed call carried.
     Blaming those selects would send the supervisor to correct a choice that
     was never the problem. */
  assert.equal(createErrorField(
    'agent failed to set model "ds/b": {"code":-32603,"message":"internal error"}',
    500), "prompt");
  assert.equal(createErrorField(
    'agent failed to set effort "max": write |1: broken pipe', 500), "prompt");
  /* No status at all is not a refusal either: a network failure, or a throw
     from the work that follows an accepted create, never classified one. */
  assert.equal(createErrorField('agent failed to set model "ds/b": boom'), "prompt");
  /* Directory and title name their own field in any answer that can carry
     them, so they stay status-independent. */
  assert.equal(createErrorField("bad directory path", 500), "dir");
  assert.equal(createErrorField("title is invalid", 500), "title");
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
          opts.push({
            value,
            textContent: m[2],
            disabled: /\sdisabled(?:\s|=|>|$)/.test(m[0]),
          });
        }
        this._options = opts;
        this.value = opts.length ? opts[0].value : "";
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
  const nc_muse_privacy = el("div", { id: "nc_muse_privacy", hidden: true });
  const nc_muse_ack = el("input", { id: "nc_muse_ack" });
  nc_muse_ack.checked = false;
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
  newchat._fields = [nc_muse_ack, nc_title, nc_prompt, nc_agent, nc_model, nc_effort, nc_dir, nc_lane, nc_lane_new];

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
    backdrop, burger, plusbtn, newchat, nc_head, nc_start, nc_muse_privacy, nc_muse_ack, nc_title, nc_prompt,
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

/* An api() rejection the way decodeResponse builds one: the HTTP status
   travels on the Error, and the sheet's classifier needs it to tell a wrong
   choice (400) from a broken agent (500). */
function apiError(msg, status){
  const e = new Error(msg);
  e.status = status;
  return e;
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

test("explicit agents refresh replaces a live model catalog without reload", async () => {
  let payload = {
    grok: { models: ["grok-4.6"], efforts: {} },
    opencode: { models: ["configured/old"] },
  };
  const ctx = createFeature();
  ctx.setApi(async path => path === "/api/agents" ? payload : {});
  ctx.feature.bind();
  await Promise.resolve();
  await Promise.resolve();

  ctx.byId.nc_agent.value = "grok";
  ctx.byId.nc_agent.dispatch("change");
  assert.match(ctx.byId.nc_model.innerHTML, /grok-4\.6/);

  payload = {
    grok: { models: ["grok-4.7"], efforts: {} },
    opencode: { models: ["configured/new"] },
    pi: { models: ["configured/fresh-model"] },
  };
  ctx.feature.applyAgents(payload);

  assert.equal(ctx.byId.nc_agent.value, "grok");
  assert.match(ctx.byId.nc_model.innerHTML, /grok-4\.7/);
  assert.doesNotMatch(ctx.byId.nc_model.innerHTML, /grok-4\.6/);
  assert.match(ctx.byId.nc_agent.innerHTML, /pi/);
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

/* A cursor row whose ids all carry a level has no id that means "no level":
   the server picks one at launch. The dialog must therefore choose it visibly,
   or the node is stored with effort "" while the process runs at "low". */
const cursorAgentsPayload = {
  cursor: {
    models: ["example-codex", "example-flash"],
    efforts: {
      /* Has a level-free id (`example-codex`), so blank effort is a real,
         launchable choice and stays on offer. */
      "example-codex": { levels: ["low", "high"] },
      /* No level-free id: blank resolves server-side to `example-flash-low`. */
      "example-flash": { levels: ["low", "medium", "high"], default: "low", required: true },
    },
  },
  codex: { models: ["gpt-5.4"], efforts: { "gpt-5.4": { levels: ["low", "medium", "high"], default: "medium" } } },
};

async function pickCursorModel(ctx, model){
  ctx.feature.bind();
  await Promise.resolve();
  await Promise.resolve();
  ctx.byId.plusbtn.dispatch("click");
  ctx.byId.nc_agent.value = "cursor";
  ctx.byId.nc_agent.dispatch("change");
  ctx.byId.nc_model.value = model;
  ctx.byId.nc_model.dispatch("change");
}

test("a fresh cursor row with no blank id selects and submits its default effort", async () => {
  const ctx = createFeature({ agentsPayload: cursorAgentsPayload });
  await pickCursorModel(ctx, "example-flash");

  /* The select shows the level the launch will use, before anything is sent. */
  assert.equal(ctx.byId.nc_effort.value, "low");
  /* And offers no empty choice, because there is no id that empty would mean. */
  assert.doesNotMatch(ctx.byId.nc_effort.innerHTML, /<option value=""/);

  ctx.byId.nc_title.value = "Work";
  ctx.byId.nc_prompt.value = "Do it";
  ctx.byId.nc_lane.value = "lane-a";
  ctx.byId.nc_start.dispatch("click");
  await Promise.resolve();
  await Promise.resolve();
  await Promise.resolve();
  const post = ctx.apiCalls.find(c => c.path === "/api/nodes" && c.opts.method === "POST");
  assert.ok(post, "no create request");
  const body = JSON.parse(post.opts.body);
  assert.equal(body.agent, "cursor");
  assert.equal(body.model, "example-flash");
  assert.equal(body.effort, "low", "the stored node must name the level the process gets");
});

test("a cursor fork names a level even when the parent record stored none", async () => {
  /* Forks seed the dialog from the parent, and the parent may predate this
     rule (or be a cursor node whose effort was resolved server-side), so the
     seeded menu has to answer the same question the fresh one does. */
  const cursorNodes = {
    "c-blank": {
      id: "c-blank", title: "Blank", agent: "cursor", model: "example-flash",
      effort: "", dir: "/w", lane_id: "lane-a",
      created_at: "2020-01-01T00:00:00Z", stops: ["2020-01-02T00:00:00Z"],
    },
    "c-high": {
      id: "c-high", title: "High", agent: "cursor", model: "example-flash",
      effort: "high", dir: "/w", lane_id: "lane-a",
      created_at: "2020-01-01T00:00:00Z", stops: ["2020-01-02T00:00:00Z"],
    },
  };
  const ctx = createFeature({ agentsPayload: cursorAgentsPayload, nodes: cursorNodes });
  ctx.feature.bind();
  await Promise.resolve();
  await Promise.resolve();
  ctx.feature.forkFromStation("c-blank");
  assert.equal(ctx.byId.nc_effort.value, "low");

  const ctx2 = createFeature({ agentsPayload: cursorAgentsPayload, nodes: cursorNodes });
  ctx2.feature.bind();
  await Promise.resolve();
  await Promise.resolve();
  ctx2.feature.forkFromStation("c-high");
  assert.equal(ctx2.byId.nc_effort.value, "high");
});

test("a chosen cursor effort survives a menu rebuild", async () => {
  const ctx = createFeature({ agentsPayload: cursorAgentsPayload });
  await pickCursorModel(ctx, "example-flash");
  ctx.byId.nc_effort.value = "high";
  ctx.byId.nc_model.dispatch("change");
  assert.equal(ctx.byId.nc_effort.value, "high");
});

test("blank effort still means blank where it names a real id", async () => {
  /* The cursor row that has a level-free id, and every other agent: blank is
     "let the harness decide", and preselecting a level there would pin today's
     default into a durable record. */
  const ctx = createFeature({ agentsPayload: cursorAgentsPayload });
  await pickCursorModel(ctx, "example-codex");
  assert.equal(ctx.byId.nc_effort.value, "");
  assert.match(ctx.byId.nc_effort.innerHTML, /<option value=""/);

  const ctx2 = createFeature({ agentsPayload: cursorAgentsPayload });
  ctx2.feature.bind();
  await Promise.resolve();
  await Promise.resolve();
  ctx2.byId.plusbtn.dispatch("click");
  ctx2.byId.nc_agent.value = "codex";
  ctx2.byId.nc_agent.dispatch("change");
  ctx2.byId.nc_model.value = "gpt-5.4";
  ctx2.byId.nc_model.dispatch("change");
  assert.equal(ctx2.byId.nc_effort.value, "");
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

test("Claude create with not_sent initial delivery preserves a recoverable composer draft", async () => {
  const ctx = createFeature({ isDesktop: false });
  ctx.setApi(async (path, opts = {}) => {
    if (path === "/api/agents") return {};
    if (path === "/api/nodes" && opts.method === "POST")
      return { id: "claude-pending", initial_delivery: "not_sent", initial_error: "Claude did not start." };
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
  assert.match(ctx.effects.alert.join(" "), /did not start|not delivered/i);
});

test("new-chat Send-to waits for the confirmed initial turn and keeps modified text", async () => {
  const source = { node: "source", uid: "u1", segment: 2, record: 7, text: "original" };
  const ctx = createFeature();
  ctx.feature.bind();
  ctx.feature.openNewActivity({ prompt: "forwarded", forwardSources: [source] });
  ctx.byId.nc_title.value = "Destination";
  ctx.byId.nc_prompt.value = "edited before creating";
  ctx.byId.nc_lane.value = "lane-a";
  ctx.byId.nc_start.dispatch("click");
  await Promise.resolve(); await Promise.resolve(); await Promise.resolve();
  assert.equal(ctx.effects.uiMutate.some(op => op.k === "forward-link-add"), false);
  assert.deepEqual(JSON.parse(ctx.storage.getItem("scimux-sendto-pending:created-1")), [source]);
  const created = JSON.parse(ctx.storage.getItem("scimux-sendto-awaiting:created-1"));
  assert.equal(created.text, "edited before creating");
  assert.equal(created.afterTurns, 0);
  assert.ok(created.at > 0, "the latch is stamped so it can expire");
});

test("new-chat cancel drops Send-to intent; not_sent transfers it to the recovery draft", async () => {
  const source = { node: "source", turnTime: "t", text: "original" };
  const cancel = createFeature();
  cancel.feature.bind();
  cancel.feature.openNewActivity({ prompt: "forwarded", forwardSources: [source] });
  cancel.feature.closeSheets();
  cancel.feature.openNewActivity();
  cancel.byId.nc_title.value = "Plain";
  cancel.byId.nc_lane.value = "lane-a";
  cancel.byId.nc_start.dispatch("click");
  await Promise.resolve(); await Promise.resolve(); await Promise.resolve();
  assert.equal(cancel.effects.uiMutate.some(op => op.k === "forward-link-add"), false);

  const held = createFeature();
  held.setApi(async (path, opts = {}) => {
    if (path === "/api/agents") return {};
    if (path === "/api/nodes" && opts.method === "POST")
      return { id: "pending-node", initial_delivery: "not_sent" };
    return {};
  });
  held.feature.bind();
  held.feature.openNewActivity({ prompt: "forwarded", forwardSources: [source] });
  held.byId.nc_title.value = "Destination";
  held.byId.nc_lane.value = "lane-a";
  held.byId.nc_start.dispatch("click");
  await Promise.resolve(); await Promise.resolve(); await Promise.resolve();
  assert.equal(held.effects.uiMutate.some(op => op.k === "forward-link-add"), false);
  assert.deepEqual(JSON.parse(held.storage.getItem("scimux-sendto-pending:pending-node")), [source]);
});

test("new-chat pending delivery waits for transcript confirmation", async () => {
  const source = { node: "source", turnTime: "t", text: "original" };
  const ctx = createFeature();
  ctx.setApi(async (path, opts = {}) => {
    if (path === "/api/agents") return {};
    if (path === "/api/nodes" && opts.method === "POST")
      return { id: "deferred-node", initial_delivery: "pending" };
    return {};
  });
  ctx.feature.bind();
  ctx.feature.openNewActivity({ prompt: "forwarded", forwardSources: [source] });
  ctx.byId.nc_title.value = "Destination";
  ctx.byId.nc_prompt.value = "edited before creating";
  ctx.byId.nc_lane.value = "lane-a";
  ctx.byId.nc_start.dispatch("click");
  await Promise.resolve(); await Promise.resolve(); await Promise.resolve();
  assert.equal(ctx.effects.uiMutate.some(op => op.k === "forward-link-add"), false);
  assert.deepEqual(JSON.parse(ctx.storage.getItem("scimux-sendto-pending:deferred-node")), [source]);
  const deferred = JSON.parse(ctx.storage.getItem("scimux-sendto-awaiting:deferred-node"));
  assert.equal(deferred.text, "edited before creating");
  assert.equal(deferred.afterTurns, 0);
  assert.ok(deferred.at > 0, "the latch is stamped so it can expire");
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
    ['cursor has no model "example-flash" at effort "max"', "nc_model", 400],
  ];
  for (const [msg, id, status] of cases){
    const ctx = createFeature();
    ctx.setApi(async path => {
      if (path === "/api/agents") return {};
      if (path === "/api/nodes") throw status ? apiError(msg, status) : new Error(msg);
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

test("a refused model or effort marks the field that was refused", async () => {
  /* The server answers 400 for a launch config the agent would not take
     (dsh applies both over the wire). The sheet has to put that error on the
     select the user must change, and focus it, or the only actionable part of
     a rejected launch is invisible. The status is part of that contract, so
     the double carries it the way decodeResponse does. */
  const cases = [
    ['acp: agent rejected the requested session configuration: model "ds/b" is not one this agent offers', "nc_model"],
    ['acp: agent rejected the requested session configuration: effort "medium" is not one this agent offers', "nc_effort"],
  ];
  for (const [msg, id] of cases){
    const ctx = createFeature();
    ctx.setApi(async path => {
      if (path === "/api/agents") return {};
      if (path === "/api/nodes") throw apiError(msg, 400);
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
    assert.equal(ctx.byId[id]._focused, true, msg);
    assert.notEqual(ctx.byId.nc_prompt.attributes["aria-invalid"], "true",
      "the prompt was not what the agent refused");
  }
});

test("a 500 that names a model or effort does not blame those selects", async () => {
  /* The failure text of a broken apply still quotes the knob it was applying.
     Only the status separates it from a refusal, so a 500 must land on the
     prompt with both selects left clean — otherwise an agent that crashed
     mid-launch reads as "your model is wrong". */
  const cases = [
    'agent failed to set model "ds/b": {"code":-32603,"message":"internal error"}',
    'agent failed to set effort "max": write |1: broken pipe',
  ];
  for (const msg of cases){
    const ctx = createFeature();
    ctx.setApi(async path => {
      if (path === "/api/agents") return {};
      if (path === "/api/nodes") throw apiError(msg, 500);
      return {};
    });
    ctx.feature.bind();
    ctx.byId.plusbtn.dispatch("click");
    ctx.byId.nc_title.value = "T";
    ctx.byId.nc_lane.value = "lane-a";
    ctx.byId.nc_start.dispatch("click");
    await Promise.resolve();
    await Promise.resolve();
    assert.equal(ctx.byId.nc_prompt.attributes["aria-invalid"], "true", msg);
    assert.equal(ctx.byId.nc_prompt._focused, true, msg);
    assert.notEqual(ctx.byId.nc_model.attributes["aria-invalid"], "true",
      "the model was not what failed");
    assert.notEqual(ctx.byId.nc_effort.attributes["aria-invalid"], "true",
      "the effort was not what failed");
  }
});

test("a refused field is cleared before the next submission", async () => {
  /* Otherwise a corrected second attempt still shows the first attempt's
     error on the effort select. */
  const ctx = createFeature();
  let fail = true;
  ctx.setApi(async (path, opts) => {
    if (path === "/api/agents") return {};
    if (path === "/api/nodes"){
      if (fail) throw apiError('acp: agent rejected the requested session configuration: effort "medium" is not one this agent offers', 400);
      return { id: "created-1", title: JSON.parse(opts.body).title };
    }
    return {};
  });
  ctx.feature.bind();
  ctx.byId.plusbtn.dispatch("click");
  ctx.byId.nc_title.value = "T";
  ctx.byId.nc_lane.value = "lane-a";
  ctx.byId.nc_start.dispatch("click");
  await Promise.resolve();
  await Promise.resolve();
  assert.equal(ctx.byId.nc_effort.attributes["aria-invalid"], "true");
  fail = false;
  ctx.byId.nc_start.dispatch("click");
  await Promise.resolve();
  await Promise.resolve();
  await Promise.resolve();
  assert.notEqual(ctx.byId.nc_effort.attributes["aria-invalid"], "true");
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

test("stale adoption controls have no sheet callback or request", async () => {
  const ctx = createFeature();
  ctx.feature.bind();
  assert.equal(ctx.feature.openAdopt, undefined);
  ctx.byId.ad_go.dispatch("click");
  await Promise.resolve();
  assert.equal(ctx.apiCalls.some(c => c.path === "/api/adopt"), false);
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

test("public factory API exposes only user actions and catalog refresh", () => {
  const { feature } = createFeature();
  const keys = Object.keys(feature).sort();
  assert.deepEqual(keys, [
    "applyAgents",
    "bind",
    "closeSheets",
    "destroy",
    "fieldError",
    "forkFromStation",
    "forkFromTurn",
    "openActivityEditor",
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

test("send-to agent logo is 16px in the picker (not the 20px card size)", () => {
  /* Bare `#sendto .agent-logo {` — never the img/svg/mask descendant rule. */
  const m = sheetsCssSrc.match(/#sendto\s+\.agent-logo\s*\{([^}]+)\}/);
  assert.ok(m, "#sendto .agent-logo rule present");
  assert.match(m[1], /width:\s*16px/);
  assert.match(m[1], /height:\s*16px/);
  assert.match(m[1], /flex:\s*none/);
  assert.doesNotMatch(m[1], /width:\s*20px/);
  assert.doesNotMatch(m[1], /height:\s*20px/);
});

test("send-to agent logo inner marks inherit the 16px size", () => {
  const m = sheetsCssSrc.match(/#sendto\s+\.agent-logo\s+img[\s\S]*?\{([^}]+)\}/);
  assert.ok(m, "#sendto .agent-logo img/svg/mask-logo override present");
  assert.match(m[1], /width:\s*16px/);
  assert.match(m[1], /height:\s*16px/);
  assert.match(sheetsCssSrc, /#sendto\s+\.agent-logo\s+svg/);
  assert.match(sheetsCssSrc, /#sendto\s+\.agent-logo\s+\.mask-logo/);
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
  for (const delivery of ["not_sent"]){
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

/* ---------- Phase 3 Packet E: edit validation / PATCH fail / single-flight / lane ---------- */

test("Packet E: edit empty title shows fieldError and issues no PATCH", async () => {
  const ctx = createFeature();
  ctx.feature.bind();
  ctx.feature.openActivityEditor("p1");
  ctx.byId.nc_title.value = "   ";
  const before = ctx.apiCalls.filter(c => c.opts && c.opts.method === "PATCH").length;
  ctx.byId.nc_start.dispatch("click");
  await Promise.resolve();
  await Promise.resolve();
  assert.equal(ctx.byId.nc_title.attributes["aria-invalid"], "true");
  assert.match(ctx.byId.nc_title.nextElementSibling.textContent, /Enter a title\./);
  assert.equal(ctx.byId.nc_title._focused, true);
  assert.equal(
    ctx.apiCalls.filter(c => c.opts && c.opts.method === "PATCH").length,
    before,
    "empty title must not PATCH",
  );
  assert.equal(ctx.byId.newchat.classList.contains("open"), true,
    "validation failure keeps the editor open");
});

test("Packet E: live-head PATCH failure shows fieldError and refocuses title", async () => {
  const ctx = createFeature();
  ctx.setApi(async (path, opts = {}) => {
    if (path === "/api/agents") return {};
    if (path.startsWith("/api/nodes/") && opts.method === "PATCH")
      throw new Error("head save refused");
    return {};
  });
  ctx.feature.bind();
  ctx.feature.openActivityEditor("p1");
  ctx.byId.nc_title.value = "Renamed";
  ctx.byId.nc_start.dispatch("click");
  for (let i = 0; i < 8; i++) await Promise.resolve();
  assert.equal(ctx.byId.nc_title.attributes["aria-invalid"], "true");
  assert.equal(ctx.byId.nc_title.nextElementSibling.textContent, "head save refused");
  assert.equal(ctx.byId.nc_title._focused, true);
  assert.equal(ctx.byId.newchat.classList.contains("open"), true,
    "failed head save must leave the draft open");
  assert.equal(ctx.byId.nc_title.value, "Renamed", "draft title retained after failure");
});

test("Packet E: station PATCH failure shows fieldError and refocuses title", async () => {
  const ctx = createFeature();
  ctx.setApi(async (path, opts = {}) => {
    if (path === "/api/agents") return {};
    if (path.startsWith("/api/nodes/") && opts.method === "PATCH")
      throw new Error("station save refused");
    return {};
  });
  ctx.feature.bind();
  ctx.feature.openActivityEditor("p1", "2020-01-01T00:00:00Z");
  ctx.byId.nc_title.value = "New frozen";
  ctx.byId.nc_start.dispatch("click");
  for (let i = 0; i < 8; i++) await Promise.resolve();
  assert.equal(ctx.byId.nc_title.attributes["aria-invalid"], "true");
  assert.equal(ctx.byId.nc_title.nextElementSibling.textContent, "station save refused");
  assert.equal(ctx.byId.nc_title._focused, true);
  assert.equal(ctx.byId.newchat.classList.contains("open"), true);
  assert.equal(ctx.byId.nc_title.value, "New frozen", "station draft retained");
  assert.equal(
    ctx.nodes.p1.station_labels["2020-01-01T00:00:00Z"].title,
    "Frozen",
    "failed station PATCH must not mutate local labels",
  );
});

test("Packet E: edit single-flight submission ignores re-entry", async () => {
  let release;
  const gate = new Promise(r => { release = r; });
  const ctx = createFeature();
  let patches = 0;
  ctx.setApi(async (path, opts = {}) => {
    if (path === "/api/agents") return {};
    if (path.startsWith("/api/nodes/") && opts.method === "PATCH"){
      patches++;
      await gate;
      return { id: "p1", title: "Renamed" };
    }
    return {};
  });
  ctx.feature.bind();
  ctx.feature.openActivityEditor("p1");
  ctx.byId.nc_title.value = "Renamed";
  ctx.byId.nc_start.dispatch("click");
  await Promise.resolve();
  assert.equal(ctx.byId.nc_start.disabled, true);
  assert.equal(ctx.byId.nc_start.getAttribute("aria-label"), "Saving...");
  ctx.byId.nc_start.dispatch("click");
  await Promise.resolve();
  assert.equal(patches, 1, "re-entry while saving must not issue a second PATCH");
  release();
  for (let i = 0; i < 8; i++) await Promise.resolve();
  assert.equal(patches, 1);
});

test("Packet E: create success with new lane uiMutates lanes", async () => {
  const ctx = createFeature({
    laneChoice: { laneID: "new-lane", lane: { id: "new-lane", name: "Fresh" } },
  });
  ctx.feature.bind();
  ctx.byId.plusbtn.dispatch("click");
  ctx.byId.nc_title.value = "Work";
  ctx.byId.nc_lane.value = "__new";
  ctx.byId.nc_start.dispatch("click");
  for (let i = 0; i < 8; i++) await Promise.resolve();
  assert.deepEqual(ctx.effects.uiMutate, [{
    k: "lanes",
    lanes: [{ id: "lane-a", name: "A" }, { id: "new-lane", name: "Fresh" }],
  }]);
  assert.deepEqual(ctx.effects.select, ["created-1"]);
});

/* ---------- Phase 6: structured Muse model catalog ---------- */

const MUSE_STD = {
  id: "synth-std",
  label: "Synth Standard",
  default: true,
  tier: "standard",
  launchable: true,
};
const MUSE_DISC = {
  id: "synth-disc",
  label: "Synth Discounted",
  default: false,
  tier: "discounted",
  launchable: true,
};
const MUSE_UNK = {
  id: "synth-unk",
  label: "Synth Unknown",
  default: false,
  tier: "unknown",
  launchable: false,
};

function museAgentsPayload(museModels, extras = {}){
  return {
    claude: { models: ["opus"] },
    muse: {
      models: (museModels || []).filter(r => r && r.launchable).map(r => r.id),
      muse_models: museModels,
      ...extras,
    },
  };
}

async function settle(){
  for (let i = 0; i < 8; i++) await Promise.resolve();
}

test("applyAgentsProbe stores a caller-private Muse catalog snapshot", () => {
  const row = {
    id: "synth-std",
    label: "Synth Standard",
    default: true,
    tier: "standard",
    launchable: true,
    extra: { secret: "no" },
    context_limit: 999,
  };
  const payload = {
    muse: { models: ["synth-std"], muse_models: [row] },
  };
  const r = applyAgentsProbe(cloneDefaultModels(), {}, payload);
  assert.equal(r.museModels.length, 1);
  assert.equal(r.museModels[0].id, "synth-std");
  assert.equal(r.museModels[0].extra, undefined);
  row.id = "mutated";
  row.label = "changed";
  row.tier = "discounted";
  row.default = false;
  row.launchable = false;
  assert.equal(r.museModels[0].id, "synth-std");
  assert.equal(r.museModels[0].label, "Synth Standard");
  assert.equal(r.museModels[0].tier, "standard");
  assert.equal(r.museModels[0].default, true);
  assert.equal(r.museModels[0].launchable, true);
  r.museModels[0].id = "from-return";
  r.museModels[0].tier = "unknown";
  assert.equal(payload.muse.muse_models[0].id, "mutated");
  assert.equal(payload.muse.muse_models[0].tier, "discounted");
  assert.equal(museModelSelectable(r.museModels[0]), false);
});

test("applyAgentsProbe Muse snapshot rejects empty ids and keeps fail-closed selectable rules", () => {
  const r = applyAgentsProbe(cloneDefaultModels(), {}, {
    muse: { muse_models: [
      { id: "  ", tier: "standard", launchable: true },
      { id: "", tier: "standard", launchable: true },
      { id: "ok", tier: "standard", launchable: true, label: "OK" },
    ] },
  });
  assert.deepEqual(r.museModels.map(x => x.id), ["ok"]);
  assert.equal(museFreshDefaultId(r.museModels), "");
});

test("applyAgentsProbe keeps structured Muse metadata out of the agent map", () => {
  const models = cloneDefaultModels();
  const me = {};
  const r = applyAgentsProbe(models, me, museAgentsPayload([MUSE_STD, MUSE_DISC, MUSE_UNK]));
  assert.equal(r.changed, true);
  assert.deepEqual(Object.keys(models).sort(), ["claude", "muse"]);
  assert.equal(models.tier, undefined);
  assert.equal(models.launchable, undefined);
  assert.equal(models["synth-std"], undefined);
  assert.equal(models.muse_models, undefined);
  assert.ok(Array.isArray(r.museModels));
  assert.equal(r.museModels.length, 3);
  assert.equal(r.museModels[0].id, "synth-std");
  assert.equal(r.museModels[0].tier, "standard");
  assert.equal(r.museModels[1].tier, "discounted");
  const html = agentOptionsHTML(models, esc);
  assert.match(html, />muse</);
  assert.doesNotMatch(html, />synth-std</);
  assert.doesNotMatch(html, />standard</);
});

test("applyAgentsProbe fail-closes when Muse structured metadata is absent or malformed", () => {
  const models = cloneDefaultModels();
  const missing = applyAgentsProbe(models, {}, {
    muse: { models: ["would-look-launchable"] },
  });
  assert.deepEqual(missing.museModels, []);
  assert.deepEqual(models.muse, ["", "would-look-launchable"]);

  const bad = applyAgentsProbe(cloneDefaultModels(), {}, {
    muse: { models: ["x"], muse_models: { id: "x", tier: "standard", launchable: true } },
  });
  assert.deepEqual(bad.museModels, []);

  const empty = applyAgentsProbe(cloneDefaultModels(), {}, null);
  assert.equal(empty.changed, false);
  assert.deepEqual(empty.museModels, []);
});

test("museModelSelectable is exact: launchable true and standard|discounted only", () => {
  assert.equal(museModelSelectable(MUSE_STD), true);
  assert.equal(museModelSelectable(MUSE_DISC), true);
  assert.equal(museModelSelectable(MUSE_UNK), false);
  assert.equal(museModelSelectable({
    id: "lie", launchable: true, tier: "unknown",
  }), false);
  assert.equal(museModelSelectable({
    id: "lie-std", launchable: true, tier: "Standard",
  }), false);
  assert.equal(museModelSelectable({
    id: "no-tier", launchable: true,
  }), false);
  assert.equal(museModelSelectable({
    id: "std-closed", launchable: false, tier: "standard",
  }), false);
  assert.equal(museModelSelectable({
    id: "std-yes", launchable: "true", tier: "standard",
  }), false);
  assert.equal(museModelSelectable(null), false);
});

test("museFreshDefaultId requires declared Standard default; never first-row or Discounted", () => {
  assert.equal(museFreshDefaultId([MUSE_STD, MUSE_DISC]), "synth-std");
  assert.equal(museFreshDefaultId([MUSE_DISC, MUSE_STD]), "synth-std");
  assert.equal(museFreshDefaultId([MUSE_DISC, MUSE_UNK]), "");
  assert.equal(museFreshDefaultId([{
    id: "first-disc", label: "Looks Standard", default: true,
    tier: "discounted", launchable: true,
  }, MUSE_STD]), "synth-std");
  assert.equal(museFreshDefaultId([{
    id: "cheap-first", tier: "discounted", launchable: true, default: false,
  }]), "");
  assert.equal(museFreshDefaultId([{
    id: "declared-but-unknown", default: true, tier: "unknown", launchable: true,
  }]), "");
  assert.equal(museFreshDefaultId([{
    id: "declared-unlaunchable", default: true, tier: "standard", launchable: false,
  }]), "");
  assert.equal(museFreshDefaultId(null), "");
});

test("museModelOptionsHTML uses ordinary names and omits unlaunchable rows", () => {
  const html = museModelOptionsHTML([
    MUSE_STD,
    MUSE_DISC,
    MUSE_UNK,
    { id: "no-label", label: "", tier: "standard", launchable: true },
    { id: "lie-launch", label: "Open", tier: "mystery", launchable: true },
    { id: "<xss>", label: "a<b>", tier: "standard", launchable: true },
    { id: "std-from-name", label: "standard-model", tier: "unknown", launchable: true },
    { id: "missing-tier", label: "Bare", launchable: true },
    null,
    "skip-me",
    { label: "no-id", tier: "standard", launchable: true },
  ], esc);

  assert.match(html, /value="synth-std">Synth Standard/);
  assert.match(html, /value="synth-disc">Synth Discounted/);
  assert.doesNotMatch(html, /synth-unk/);
  assert.match(html, /value="no-label">no-label/);
  assert.doesNotMatch(html, /lie-launch/);
  assert.match(html, /value="&lt;xss&gt;">a&lt;b&gt;/);
  assert.doesNotMatch(html, /std-from-name/);
  assert.doesNotMatch(html, /missing-tier/);
  assert.doesNotMatch(html, /skip-me/);
  assert.doesNotMatch(html, />no-id</);
  assert.doesNotMatch(html, /<xss>/);
  assert.doesNotMatch(html, /a<b>/);
  /* Never infer Standard from id, label, or position. */
  assert.doesNotMatch(html, /std-from-name"[^>]*>standard-model/);
});

test("non-Muse modelOptionsHTML is unchanged by a sibling Muse catalog", () => {
  const models = cloneDefaultModels();
  applyAgentsProbe(models, {}, {
    claude: { models: ["opus", "sonnet"] },
    muse: { models: ["synth-std"], muse_models: [MUSE_STD] },
  });
  const html = modelOptionsHTML(models, "claude", null, esc);
  assert.match(html, /value="">\(default\)/);
  assert.match(html, /value="opus">opus/);
  assert.doesNotMatch(html, /Standard|Discounted|tier unavailable/);
});

test("fresh Muse activity selects only the declared Standard default", async () => {
  const ctx = createFeature({
    agentsPayload: museAgentsPayload([
      { ...MUSE_DISC, id: "first-row" },
      MUSE_STD,
      MUSE_UNK,
    ]),
  });
  ctx.feature.bind();
  await settle();
  ctx.byId.plusbtn.dispatch("click");
  ctx.byId.nc_agent.value = "muse";
  ctx.byId.nc_agent.dispatch("change");
  assert.equal(ctx.byId.nc_model.value, "synth-std");
  assert.match(ctx.byId.nc_model.innerHTML, /Synth Standard/);
  assert.match(ctx.byId.nc_model.innerHTML, /Synth Discounted/);
  const unk = ctx.byId.nc_model.options.find(o => o.value === "synth-unk");
  assert.equal(unk, undefined);
});

test("fresh Muse create payload sends only the selected model id", async () => {
  const ctx = createFeature({
    agentsPayload: museAgentsPayload([MUSE_STD, MUSE_DISC]),
  });
  ctx.feature.bind();
  await settle();
  ctx.byId.plusbtn.dispatch("click");
  ctx.byId.nc_agent.value = "muse";
  ctx.byId.nc_agent.dispatch("change");
  ctx.byId.nc_title.value = "Muse work";
  ctx.byId.nc_lane.value = "lane-a";
  ctx.byId.nc_start.dispatch("click");
  await settle();
  const post = ctx.apiCalls.find(c => c.path === "/api/nodes" && c.opts.method === "POST");
  assert.ok(post);
  const body = JSON.parse(post.opts.body);
  assert.equal(body.agent, "muse");
  assert.equal(body.model, "synth-std");
  assert.equal("tier" in body, false);
  assert.equal("label" in body, false);
  assert.equal("launchable" in body, false);
  assert.equal("context_limit" in body, false);
  assert.equal("output_limit" in body, false);
  assert.equal("muse_models" in body, false);
});

test("fresh Muse with no Standard default does not select Discounted and blocks submit", async () => {
  const ctx = createFeature({
    agentsPayload: museAgentsPayload([MUSE_DISC, MUSE_UNK]),
  });
  ctx.feature.bind();
  await settle();
  ctx.byId.plusbtn.dispatch("click");
  ctx.byId.nc_agent.value = "muse";
  ctx.byId.nc_agent.dispatch("change");
  assert.equal(ctx.byId.nc_model.value, "");
  assert.doesNotMatch(ctx.byId.nc_model.innerHTML, /\u2014 (Standard|Discounted)/);
  assert.notEqual(ctx.byId.nc_model.value, "synth-disc");
  ctx.byId.nc_title.value = "T";
  ctx.byId.nc_lane.value = "lane-a";
  ctx.byId.nc_start.dispatch("click");
  await settle();
  assert.equal(ctx.byId.nc_model.attributes["aria-invalid"], "true");
  assert.match(ctx.byId.nc_model.nextElementSibling.textContent, /available Muse model/i);
  assert.equal(ctx.apiCalls.some(c => c.path === "/api/nodes"), false);
});

test("same-agent Muse fork inherits the parent model and its server-owned tier label", async () => {
  const ctx = createFeature({
    agentsPayload: museAgentsPayload([MUSE_STD, MUSE_DISC, MUSE_UNK]),
    nodes: {
      muse1: {
        id: "muse1", title: "Muse parent", description: "keep going",
        agent: "muse", model: "synth-disc", effort: "", dir: "/muse",
        lane_id: "lane-a", created_at: "2020-01-01T00:00:00Z",
      },
    },
    sel: "muse1",
  });
  ctx.feature.bind();
  await settle();
  ctx.feature.forkFromTurn("Follow this", "muse1");
  assert.equal(ctx.byId.nc_agent.value, "muse");
  assert.equal(ctx.byId.nc_model.value, "synth-disc");
  assert.match(ctx.byId.nc_model.innerHTML, /Synth Discounted/);
  ctx.byId.nc_title.value = "Fork";
  ctx.byId.nc_lane.value = "lane-a";
  assert.equal(ctx.byId.nc_muse_privacy.hidden, false);
  assert.equal(ctx.byId.nc_start.disabled, true);
  ctx.byId.nc_muse_ack.checked = true;
  ctx.byId.nc_muse_ack.dispatch("change");
  ctx.byId.nc_start.dispatch("click");
  await settle();
  const post = ctx.apiCalls.find(c => c.path === "/api/nodes" && c.opts.method === "POST");
  assert.ok(post);
  const body = JSON.parse(post.opts.body);
  assert.equal(body.model, "synth-disc");
  assert.equal("tier" in body, false);
  assert.equal(body.agent, "muse");
  assert.equal(body.muse_contributor_acknowledged, true);
});

test("stale or unlaunchable Muse parent model is not made selectable by fork", async () => {
  const ctx = createFeature({
    agentsPayload: museAgentsPayload([MUSE_STD, MUSE_UNK]),
    nodes: {
      muse1: {
        id: "muse1", title: "Stale", description: "",
        agent: "muse", model: "retired-id", effort: "", dir: "/muse",
        lane_id: "lane-a", created_at: "2020-01-01T00:00:00Z",
      },
    },
  });
  ctx.feature.bind();
  await settle();
  ctx.feature.forkFromTurn("x", "muse1");
  assert.doesNotMatch(ctx.byId.nc_model.innerHTML, /retired-id/);
  assert.notEqual(ctx.byId.nc_model.value, "retired-id");
  assert.equal(ctx.byId.nc_model.value, "synth-std");
});

test("unknown inherited Muse model is omitted and is not selected", async () => {
  const ctx = createFeature({
    agentsPayload: museAgentsPayload([MUSE_STD, MUSE_UNK]),
    nodes: {
      muse1: {
        id: "muse1", title: "Unk", description: "",
        agent: "muse", model: "synth-unk", effort: "", dir: "/muse",
        lane_id: "lane-a", created_at: "2020-01-01T00:00:00Z",
      },
    },
  });
  ctx.feature.bind();
  await settle();
  ctx.feature.forkFromTurn("x", "muse1");
  const unk = ctx.byId.nc_model.options.find(o => o.value === "synth-unk");
  assert.equal(unk, undefined);
  assert.equal(ctx.byId.nc_model.value, "synth-std");
});

test("changing away from Muse drops inherited model; switching back does not restore it", async () => {
  const ctx = createFeature({
    agentsPayload: museAgentsPayload([MUSE_STD, MUSE_DISC]),
    nodes: {
      muse1: {
        id: "muse1", title: "Muse parent", description: "",
        agent: "muse", model: "synth-disc", effort: "", dir: "/muse",
        lane_id: "lane-a", created_at: "2020-01-01T00:00:00Z",
      },
    },
  });
  ctx.feature.bind();
  await settle();
  ctx.feature.forkFromTurn("x", "muse1");
  assert.equal(ctx.byId.nc_model.value, "synth-disc");
  ctx.byId.nc_agent.value = "claude";
  ctx.byId.nc_agent.dispatch("change");
  assert.notEqual(ctx.byId.nc_model.value, "synth-disc");
  assert.doesNotMatch(ctx.byId.nc_model.innerHTML, /Discounted/);
  ctx.byId.nc_agent.value = "muse";
  ctx.byId.nc_agent.dispatch("change");
  assert.equal(ctx.byId.nc_model.value, "synth-std",
    "returning to Muse must follow the Standard default, not re-inherit");
});

test("cross-agent fork does not inherit a Muse model or tier", async () => {
  const ctx = createFeature({
    agentsPayload: museAgentsPayload([MUSE_STD, MUSE_DISC]),
    nodes: {
      p1: {
        id: "p1", title: "Claude parent", description: "Parent desc",
        agent: "claude", model: "opus", effort: "high", dir: "/parent",
        lane_id: "lane-a", created_at: "2020-01-01T00:00:00Z",
      },
    },
  });
  ctx.feature.bind();
  await settle();
  ctx.feature.forkFromTurn("x", "p1");
  assert.equal(ctx.byId.nc_agent.value, "claude");
  assert.equal(ctx.byId.nc_model.value, "opus");
  ctx.byId.nc_agent.value = "muse";
  ctx.byId.nc_agent.dispatch("change");
  assert.equal(ctx.byId.nc_model.value, "synth-std");
  assert.notEqual(ctx.byId.nc_model.value, "opus");
  assert.doesNotMatch(ctx.byId.nc_model.innerHTML, /opus/);
});

test("new-activity sheet has no redundant Muse supervision warning", () => {
  const indexSrc = readFileSync(join(__dirname, "../index.html"), "utf8");
	assert.doesNotMatch(indexSrc, /id="nc_muse_warn"/);
	assert.doesNotMatch(sheetsSrc, /museActivityWarningText|syncMuseActivityWarning/);
});

test("absent Muse structured catalog does not make legacy models launchable", async () => {
  const ctx = createFeature({
    agentsPayload: {
      claude: { models: ["opus"] },
      muse: { models: ["looks-open", "also-open"] },
    },
  });
  ctx.feature.bind();
  await settle();
  ctx.byId.plusbtn.dispatch("click");
  ctx.byId.nc_agent.value = "muse";
  ctx.byId.nc_agent.dispatch("change");
  assert.doesNotMatch(ctx.byId.nc_model.innerHTML, /looks-open|also-open/);
  assert.equal(ctx.byId.nc_model.value, "");
  ctx.byId.nc_title.value = "T";
  ctx.byId.nc_lane.value = "lane-a";
  ctx.byId.nc_model.value = "looks-open";
  ctx.byId.nc_start.dispatch("click");
  await settle();
  assert.equal(ctx.apiCalls.some(c => c.path === "/api/nodes"), false);
  assert.equal(ctx.byId.nc_model.attributes["aria-invalid"], "true");
});

function museParentNode(id, model){
  return {
    id, title: "Muse parent", description: "keep going",
    agent: "muse", model, effort: "medium", dir: "/muse",
    lane_id: "lane-a", created_at: "2020-01-01T00:00:00Z",
  };
}

test("delayed agents probe preserves an open same-agent Muse fork", async () => {
  let release;
  const pending = new Promise(resolve => { release = resolve; });
  const ctx = createFeature({
    nodes: { muse1: museParentNode("muse1", "synth-disc") },
    sel: "muse1",
  });
  ctx.setApi(async (path, opts = {}) => {
    ctx.apiCalls.push({ path, opts });
    if (path === "/api/agents") return pending;
    if (path === "/api/nodes" && opts.method === "POST")
      return { id: "created-1", title: JSON.parse(opts.body).title };
    return {};
  });
  ctx.feature.bind();
  ctx.feature.forkFromTurn("Follow this", "muse1");
  assert.equal(ctx.byId.nc_agent.value, "muse");
  assert.equal(ctx.byId.nc_dir.value, "/muse");
  assert.equal(ctx.byId.nc_effort.value, "medium");
  release({
    claude: { models: ["opus"] },
    muse: {
      models: ["synth-std", "synth-disc"],
      muse_models: [MUSE_STD, MUSE_DISC],
    },
  });
  await settle();
  assert.equal(ctx.byId.nc_agent.value, "muse");
  assert.equal(ctx.byId.nc_model.value, "synth-disc");
  assert.match(ctx.byId.nc_model.innerHTML, /Synth Discounted/);
  assert.equal(ctx.byId.nc_dir.value, "/muse");
  assert.equal(ctx.byId.nc_effort.value, "medium");
  ctx.byId.nc_title.value = "Fork";
  ctx.byId.nc_lane.value = "lane-a";
  ctx.byId.nc_muse_ack.checked = true;
  ctx.byId.nc_muse_ack.dispatch("change");
  ctx.byId.nc_start.dispatch("click");
  await settle();
  const post = ctx.apiCalls.find(c => c.path === "/api/nodes" && c.opts.method === "POST");
  assert.ok(post);
  const body = JSON.parse(post.opts.body);
  assert.equal(body.agent, "muse");
  assert.equal(body.model, "synth-disc");
  assert.equal(body.parent, "muse1");
  assert.equal("tier" in body, false);
  assert.equal("label" in body, false);
  assert.equal("launchable" in body, false);
});

/* Cursor is the strict version of the Muse case: its effort menu exists only
   per model in the catalog, so before the probe answers there is no menu at
   all, and a fork opened in that window has nothing to hold the parent's level
   with. What the parent chose has to survive the wait. */
function cursorParentNode(id, effort){
  return {
    id, title: "Cursor parent", description: "keep going",
    agent: "cursor", model: "example-flash", effort, dir: "/cur",
    lane_id: "lane-a", created_at: "2020-01-01T00:00:00Z",
  };
}

test("delayed agents probe preserves an open Cursor fork", async () => {
  for (const [effort, want] of [["high", "high"], ["", "low"]]){
    let release;
    const pending = new Promise(resolve => { release = resolve; });
    const ctx = createFeature({ nodes: { c1: cursorParentNode("c1", effort) } });
    ctx.setApi(async (path, opts = {}) => {
      ctx.apiCalls.push({ path, opts });
      if (path === "/api/agents") return pending;
      if (path === "/api/nodes" && opts.method === "POST")
        return { id: "created-1", title: JSON.parse(opts.body).title };
      return {};
    });
    ctx.feature.bind();
    ctx.feature.forkFromTurn("Follow this", "c1");
    assert.equal(ctx.byId.nc_agent.value, "cursor", effort);
    assert.equal(ctx.byId.nc_model.value, "example-flash", effort);

    release(cursorAgentsPayload);
    await settle();
    assert.equal(ctx.byId.nc_agent.value, "cursor", effort);
    assert.equal(ctx.byId.nc_model.value, "example-flash", effort);
    /* Blank is not a choice on this row, so a parent that stored none inherits
       the level the launch would use; a parent that stored one keeps it. */
    assert.equal(ctx.byId.nc_effort.value, want, effort);

    ctx.byId.nc_title.value = "Fork";
    ctx.byId.nc_lane.value = "lane-a";
    ctx.byId.nc_start.dispatch("click");
    await settle();
    const post = ctx.apiCalls.find(c => c.path === "/api/nodes" && c.opts.method === "POST");
    assert.ok(post, effort);
    const body = JSON.parse(post.opts.body);
    assert.equal(body.agent, "cursor", effort);
    assert.equal(body.model, "example-flash", effort);
    assert.equal(body.effort, want, effort);
    assert.equal(body.parent, "c1", effort);
  }
});

test("switching agents while the probe is pending drops the inherited level", async () => {
  /* Blank is a real choice on codex. Once the user has changed the agent, the
     cursor parent's level is not an unshown inheritance any more -- it is a
     value for a row that is no longer selected, and replaying it when the
     catalog lands would submit a level nobody picked. */
  let release;
  const pending = new Promise(resolve => { release = resolve; });
  const ctx = createFeature({ nodes: { c1: cursorParentNode("c1", "high") } });
  ctx.setApi(async (path, opts = {}) => {
    ctx.apiCalls.push({ path, opts });
    if (path === "/api/agents") return pending;
    if (path === "/api/nodes" && opts.method === "POST")
      return { id: "created-1", title: JSON.parse(opts.body).title };
    return {};
  });
  ctx.feature.bind();
  ctx.feature.forkFromTurn("Follow this", "c1");
  ctx.byId.nc_agent.value = "codex";
  ctx.byId.nc_agent.dispatch("change");
  assert.equal(ctx.byId.nc_effort.value, "");

  release(cursorAgentsPayload);
  await settle();
  assert.equal(ctx.byId.nc_agent.value, "codex");
  assert.equal(ctx.byId.nc_effort.value, "");

  ctx.byId.nc_title.value = "Fork";
  ctx.byId.nc_lane.value = "lane-a";
  ctx.byId.nc_start.dispatch("click");
  await settle();
  const post = ctx.apiCalls.find(c => c.path === "/api/nodes" && c.opts.method === "POST");
  assert.ok(post);
  const body = JSON.parse(post.opts.body);
  assert.equal(body.agent, "codex");
  assert.equal(body.effort, "");
});

test("delayed probe does not insert a stale inherited Muse model", async () => {
  let release;
  const pending = new Promise(resolve => { release = resolve; });
  const ctx = createFeature({
    nodes: { muse1: museParentNode("muse1", "retired-id") },
  });
  ctx.setApi(async path => path === "/api/agents" ? pending : {});
  ctx.feature.bind();
  ctx.feature.forkFromTurn("x", "muse1");
  release({
    claude: { models: ["opus"] },
    muse: { models: ["synth-std"], muse_models: [MUSE_STD, MUSE_UNK] },
  });
  await settle();
  assert.equal(ctx.byId.nc_agent.value, "muse");
  assert.doesNotMatch(ctx.byId.nc_model.innerHTML, /retired-id/);
  assert.notEqual(ctx.byId.nc_model.value, "retired-id");
  assert.equal(ctx.byId.nc_model.value, "synth-std");
});

test("delayed probe with no Standard default leaves Muse selected but blocks submit", async () => {
  let release;
  const pending = new Promise(resolve => { release = resolve; });
  const ctx = createFeature({
    nodes: { muse1: museParentNode("muse1", "retired-id") },
  });
  ctx.setApi(async (path, opts = {}) => {
    ctx.apiCalls.push({ path, opts });
    if (path === "/api/agents") return pending;
    if (path === "/api/nodes") return { id: "nope" };
    return {};
  });
  ctx.feature.bind();
  ctx.feature.forkFromTurn("x", "muse1");
  release({
    claude: { models: ["opus"] },
    muse: { models: ["synth-disc"], muse_models: [MUSE_DISC, MUSE_UNK] },
  });
  await settle();
  assert.equal(ctx.byId.nc_agent.value, "muse");
  assert.equal(ctx.byId.nc_model.value, "");
  ctx.byId.nc_title.value = "T";
  ctx.byId.nc_lane.value = "lane-a";
  ctx.byId.nc_start.dispatch("click");
  await settle();
  assert.equal(ctx.apiCalls.some(c => c.path === "/api/nodes"), false);
  assert.equal(ctx.byId.nc_model.attributes["aria-invalid"], "true");
});

test("delayed probe does not reopen a closed sheet or clobber a newer fork", async () => {
  let release;
  const pending = new Promise(resolve => { release = resolve; });
  const ctx = createFeature({
    nodes: {
      muse1: museParentNode("muse1", "synth-disc"),
      muse2: { ...museParentNode("muse2", "synth-std"), title: "Other", dir: "/other" },
    },
  });
  ctx.setApi(async path => path === "/api/agents" ? pending : {});
  ctx.feature.bind();
  ctx.feature.forkFromTurn("first", "muse1");
  ctx.feature.closeSheets();
  assert.equal(ctx.byId.newchat.classList.contains("open"), false);
  ctx.feature.forkFromTurn("second", "muse2");
  assert.equal(ctx.byId.nc_dir.value, "/other");
  release({
    claude: { models: ["opus"] },
    muse: { models: ["synth-std", "synth-disc"], muse_models: [MUSE_STD, MUSE_DISC] },
  });
  await settle();
  assert.equal(ctx.byId.newchat.classList.contains("open"), true);
  assert.equal(ctx.byId.nc_agent.value, "muse");
  assert.equal(ctx.byId.nc_model.value, "synth-std");
  assert.equal(ctx.byId.nc_dir.value, "/other");
});

test("fresh Muse activity after closing a Discounted selection restores the Standard default", async () => {
  const ctx = createFeature({
    agentsPayload: {
      muse: { models: ["synth-std", "synth-disc"], muse_models: [MUSE_STD, MUSE_DISC] },
    },
  });
  ctx.feature.bind();
  await settle();
  ctx.byId.plusbtn.dispatch("click");
  assert.equal(ctx.byId.nc_agent.value, "muse");
  assert.equal(ctx.byId.nc_model.value, "synth-std");
  ctx.byId.nc_model.value = "synth-disc";
  ctx.byId.nc_model.dispatch("change");
  assert.equal(ctx.byId.nc_model.value, "synth-disc");
  ctx.feature.closeSheets();
  ctx.byId.plusbtn.dispatch("click");
  assert.equal(ctx.byId.nc_model.value, "synth-std");
  assert.notEqual(ctx.byId.nc_model.value, "synth-disc");
});

test("openNewActivity send-to entry also restores the Standard default", async () => {
  const ctx = createFeature({
    agentsPayload: {
      muse: { models: ["synth-std", "synth-disc"], muse_models: [MUSE_STD, MUSE_DISC] },
    },
  });
  ctx.feature.bind();
  await settle();
  ctx.feature.openNewActivity({ prompt: "from send-to", focusTitle: true });
  ctx.byId.nc_model.value = "synth-disc";
  ctx.feature.closeSheets();
  ctx.feature.openNewActivity({ prompt: "again" });
  assert.equal(ctx.byId.nc_prompt.value, "again");
  assert.equal(ctx.byId.nc_model.value, "synth-std");
});

test("destroy drops a delayed Muse catalog so a closed form is not resurrected", async () => {
  let release;
  const pending = new Promise(resolve => { release = resolve; });
  const ctx = createFeature();
  ctx.setApi(async path => path === "/api/agents" ? pending : {});
  ctx.feature.bind();
  ctx.byId.plusbtn.dispatch("click");
  assert.equal(ctx.byId.newchat.classList.contains("open"), true);
  ctx.feature.closeSheets();
  ctx.feature.destroy();
  release({
    muse: { models: ["synth-std", "synth-disc"], muse_models: [MUSE_STD, MUSE_DISC] },
  });
  await settle();
  assert.equal(ctx.byId.newchat.classList.contains("open"), false);
  assert.notEqual(ctx.byId.nc_model.value, "synth-disc");
});

test("delayed catalog on an open fresh form selects Standard, not Discounted", async () => {
  let release;
  const pending = new Promise(resolve => { release = resolve; });
  const ctx = createFeature();
  ctx.setApi(async path => path === "/api/agents" ? pending : {});
  ctx.feature.bind();
  ctx.byId.plusbtn.dispatch("click");
  release({
    muse: { models: ["synth-std", "synth-disc"], muse_models: [MUSE_STD, MUSE_DISC] },
  });
  await settle();
  assert.equal(ctx.byId.newchat.classList.contains("open"), true);
  assert.equal(ctx.byId.nc_agent.value, "muse");
  assert.equal(ctx.byId.nc_model.value, "synth-std");
  ctx.byId.nc_model.value = "synth-disc";
  ctx.byId.nc_model.dispatch("change");
  assert.equal(ctx.byId.nc_model.value, "synth-disc",
    "an explicit live choice on the same open form is kept");
});

test("fresh Muse with no Standard default stays empty rather than Discounted", async () => {
  const ctx = createFeature({
    agentsPayload: {
      muse: { models: ["synth-disc"], muse_models: [MUSE_DISC] },
    },
  });
  ctx.feature.bind();
  await settle();
  ctx.byId.plusbtn.dispatch("click");
  assert.equal(ctx.byId.nc_model.value, "");
  ctx.byId.nc_title.value = "T";
  ctx.byId.nc_lane.value = "lane-a";
  ctx.byId.nc_start.dispatch("click");
  await settle();
  assert.equal(ctx.apiCalls.some(c => c.path === "/api/nodes"), false);
});

test("delayed probe while closed updates catalogs without leaking hidden Muse selection", async () => {
  let release;
  const pending = new Promise(resolve => { release = resolve; });
  const ctx = createFeature({
    nodes: { muse1: museParentNode("muse1", "synth-disc") },
  });
  ctx.setApi(async path => path === "/api/agents" ? pending : {});
  ctx.feature.bind();
  ctx.feature.forkFromTurn("x", "muse1");
  ctx.feature.closeSheets();
  release({
    muse: { models: ["synth-std", "synth-disc"], muse_models: [MUSE_STD, MUSE_DISC] },
  });
  await settle();
  assert.equal(ctx.byId.newchat.classList.contains("open"), false);
  ctx.feature.openNewActivity();
  assert.equal(ctx.byId.nc_agent.value, "muse");
  assert.equal(ctx.byId.nc_model.value, "synth-std",
    "fresh open after a closed delayed probe must use the Standard default");
  assert.notEqual(ctx.byId.nc_model.value, "synth-disc");
});

test("dsh reaches the new-chat dropdown from the probe, with no model menu of its own", () => {
  /* dsh has no static catalog and no list command: like pi and opencode it
     appears because the server probed it, and it launches on its profile's
     own default model. A DEFAULT_MODELS entry would invent a menu. */
  assert.equal(DEFAULT_MODELS.dsh, undefined);
  /* dsh advertises its thought levels per model over the wire (and a model
     whose route has no reasoning advertises none at all), so the static
     fallback must be empty: the generic low/medium/high menu would offer
     levels dsh rejects — "medium" is not one of its levels. */
  assert.deepEqual(DEFAULT_EFFORTS.dsh, []);
  assert.deepEqual(effortLevelsFor("dsh", "", {}), { list: [], default: undefined, required: false });
  assert.deepEqual(
    effortLevelsFor("dsh", "p/m", { dsh: { "p/m": { levels: ["off", "low", "high", "max"], default: "high" } } }),
    { list: ["off", "low", "high", "max"], default: "high", required: false });
  const models = cloneDefaultModels();
  const me = {};
  const r = applyAgentsProbe(models, me, { dsh: { models: [] } });
  assert.equal(r.changed, true);
  assert.deepEqual(Object.keys(models), ["dsh"]);
  assert.match(agentOptionsHTML(models, esc), />dsh</);
  /* The empty option is the only one, and it reads "(default)" — not a blank
     line the user has to guess at. */
  assert.deepEqual(models.dsh, [""]);
  assert.match(modelOptionsHTML(models, "dsh", null, esc), /value=""[^>]*>\(default\)</);
  /* An empty effort menu is a real answer, not a broken control: the one
     option reads "(default)" exactly as the model menu's does. */
  assert.equal(effortOptionsHTML([], undefined, esc), `<option value="">(default)</option>`);
  assert.equal(effortOptionsHTML(["low", "high"], "high", esc),
    `<option value=""></option><option value="low">low</option><option value="high">high (default)</option>`);
});


test("Contributor notice explains submitted context and the Standard alternative before acknowledgment", () => {
  const index = readFileSync(join(__dirname, "../index.html"), "utf8");
  const notice = index.match(/<p\b[^>]*id="nc_muse_privacy_note"[^>]*>([\s\S]*?)<\/p>/)?.[1];
  assert.ok(notice, "acknowledgment must have an accessible data-use notice");
  assert.match(notice, /retain[\s\S]*train/i);
  assert.match(notice, /confidential, sensitive, or personal information/);
  assert.match(notice, /prompts, files, and tool results/);
  assert.match(notice, /Choose a Standard model if your content must not be used for training/);
  assert.match(index, /id="nc_muse_ack"[^>]*aria-describedby="nc_muse_privacy_note"/);
  assert.doesNotMatch(index.match(/<input\b[^>]*id="nc_muse_ack"[^>]*>/)?.[0] || "", /\bchecked\b/);
});

test("Contributor acknowledgment gates creation, resets on model change and reopening", async () => {
  const ctx = createFeature({ agentsPayload: museAgentsPayload([MUSE_STD, MUSE_DISC]) });
  ctx.feature.bind();
  await settle();
  ctx.feature.openNewActivity();
  ctx.byId.nc_agent.value = "muse";
  ctx.byId.nc_agent.dispatch("change");
  assert.equal(ctx.byId.nc_muse_privacy.hidden, true);
  ctx.byId.nc_model.value = MUSE_DISC.id;
  ctx.byId.nc_model.dispatch("change");
  ctx.byId.nc_title.value = "Contributor work";
  ctx.byId.nc_lane.value = "lane-a";
  assert.equal(ctx.byId.nc_muse_privacy.hidden, false);
  assert.equal(ctx.byId.nc_muse_ack.checked, false);
  assert.equal(ctx.byId.nc_start.disabled, true);
  ctx.byId.nc_start.dispatch("click");
  await settle();
  assert.equal(ctx.apiCalls.some(c => c.path === "/api/nodes"), false);
  ctx.byId.nc_muse_ack.checked = true;
  ctx.byId.nc_muse_ack.dispatch("change");
  assert.equal(ctx.byId.nc_start.disabled, false);
  ctx.byId.nc_model.value = MUSE_STD.id;
  ctx.byId.nc_model.dispatch("change");
  assert.equal(ctx.byId.nc_muse_privacy.hidden, true);
  assert.equal(ctx.byId.nc_muse_ack.checked, false);
  ctx.byId.nc_model.value = MUSE_DISC.id;
  ctx.byId.nc_model.dispatch("change");
  assert.equal(ctx.byId.nc_start.disabled, true);
  ctx.byId.nc_muse_ack.checked = true;
  ctx.byId.nc_muse_ack.dispatch("change");
  ctx.feature.closeSheets();
  ctx.feature.openNewActivity();
  assert.equal(ctx.byId.nc_muse_ack.checked, false);
});

test("a catalog tier change revokes acknowledgment and a removed model cannot launch", async () => {
  const ctx = createFeature({ agentsPayload: museAgentsPayload([MUSE_STD, MUSE_DISC]) });
  ctx.feature.bind();
  await settle();
  ctx.feature.openNewActivity();
  ctx.byId.nc_agent.value = "muse";
  ctx.byId.nc_agent.dispatch("change");
  ctx.byId.nc_model.value = MUSE_DISC.id;
  ctx.byId.nc_model.dispatch("change");
  ctx.byId.nc_muse_ack.checked = true;
  ctx.byId.nc_muse_ack.dispatch("change");
  ctx.feature.applyAgents(museAgentsPayload([{ ...MUSE_DISC, tier: "unknown", launchable: false }]));
  assert.equal(ctx.byId.nc_model.value, "");
  assert.equal(ctx.byId.nc_muse_ack.checked, false);
  assert.equal(ctx.byId.nc_muse_privacy.hidden, true);
  assert.doesNotMatch(ctx.byId.nc_model.innerHTML, /synth-disc/);
});
