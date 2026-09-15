/* Generic sheets + new-activity feature.
 *
 * Packet 7H ownership inventory
 * -----------------------------
 * Owned roots / generic controls:
 *   - #backdrop (open marker `.on`; click → close all)
 *   - every `.sheet` via setSheetActive: `.open`, `inert`, `aria-hidden`,
 *     and input/textarea/select disable-with-`data-sheetDisabledByClose`
 *   - #burger opens #menu only (generic open transition; menu contents stay shell)
 *   - openSheet(id) / closeSheets() public primitive used by other features
 *
 * New-activity roots (#newchat):
 *   - #nc_head, #nc_start, #nc_title, #nc_prompt
 *   - #nc_agent, #nc_model, #nc_effort, #nc_dir (create-only launch config)
 *   - #nc_lane_s / #nc_lane / #nc_lane_new / #nc_lane_hint / #nc_lane_swatch
 *   - #plusbtn (plain new activity)
 *
 * Explicit non-ownership:
 *   - #sendto contents/listeners (bookmarks.js)
 *   - #cardact contents/listeners (cards.js)
 *   - #tabsheet contents/listeners (map.js)
 *   - #menu update check / apply / license texts (shell)
 *   - Read-only chat preview (#previewview body)
 *   - Top-level gestures, toast implementation, polling/tick body
 *   - Shared lane-picker algorithms (fillLaneSelect / readLaneChoice / syncLanePicker)
 *   - app.js / polling.js
 *
 * Model / effort catalogs (module-local mutable, not ui.json):
 *   - MODELS — static fallback, replaced by /api/agents probe
 *   - EFFORTS — static per-agent fallback levels
 *   - MODEL_EFFORTS — per-agent per-model menus from probe
 *
 * Edit / fork state (ephemeral):
 *   - ncParent, ncRationale
 *   - ncEdit (node id when editing), ncEditStop (earlier station seam or "")
 *   - newActivitySubmitting (single-flight)
 *
 * Server calls (via injected api):
 *   - GET  /api/agents
 *   - POST /api/nodes
 *   - PATCH /api/nodes/:id  (head title/desc or station label)
 *
 * Storage:
 *   - "scimux-lastdir" get on plain new; set on successful create with dir
 *   - "scimux-sendto-pending:" + nodeId for new-chat source intents
 *   - "scimux-sendto-awaiting:" + nodeId for the expected initial turn
 *
 * Timers:
 *   - setTimeout(0) title focus/select after fork seed and edit open
 *
 * Injected effects / seams:
 *   - nodeById, sel, laneFilter, stopsOf, stopLabel
 *   - fillLaneSelect, readLaneChoice, syncLanePicker, clearLaneNew
 *   - laneList, uiMutate, updateLocalNode
 *   - select, setLevel, isDesktop, tick, invalidateStateEtag
 *   - invalidateCards/Chat/Map + renderCards/ChatHead/Map
 *   - esc, api, storage, document, querySelectorAll, setTimeout/clearTimeout
 *
 * Lifecycle:
 *   - bind() idempotent; owns backdrop, burger, plus, agent/model change,
 *     nc_start; initial closeSheets(); probes /api/agents once
 *   - destroy() removes listeners, clears title timer, strips dynamic fielderrs
 *
 * Contracts preserved:
 *   - Fork = fresh context; payload sends visible agent/model/effort/dir
 *   - Empty launch fields server-inherited only when visibly empty
 *   - Forks must consciously pick a lane; plain new may stay unlaned
 *   - Parent/rationale and same-lane/new-lane/crossover semantics
 *   - Agent→model→effort rebuild order; valid effort preserved
 *   - Per-model discovered menus/default labels; missing agents/models stay selectable
 *   - Edit mode: title/description (or frozen station label) only; launch/lane immutable
 *   - Single-flight submit; no-op edits close without request
 *   - Create success: last dir, tick refresh, select node, phone → Chat
 *   - Field-specific error mapping + focus/scroll
 *
 * Reuses (no algorithm duplication):
 *   format.js: esc (default)
 *   lanes.js: stopsOf / stopLabel (defaults; injectable)
 * Does not import polling.js, app.js, or later features.
 */

import { esc as escDefault } from "./format.js";
import { stopsOf as stopsOfDefault, stopLabel as stopLabelDefault } from "./lanes.js";
import { focusAtEnd } from "./caret.js";
import {
  writePendingForwards,
  writeAwaitingForward,
} from "./storage.js";

/* ---------- public constants ---------- */

export const LAST_DIR_KEY = "scimux-lastdir";

/* Static fallback catalogs when GET /api/agents fails or has not returned. */
export const DEFAULT_MODELS = {
  claude: ["", "fable", "opus", "sonnet", "haiku"],
  codex:  ["", "gpt-5.4", "gpt-5.4-mini"],
  // Static fallback when grok is not on PATH or /api/agents has not returned;
  // probe data replaces this wholesale when the harness is present.
  grok:   ["", "grok-4.5"],
};
export const DEFAULT_EFFORTS = {
  claude: ["low", "medium", "high", "xhigh", "max"],
  codex:  ["low", "medium", "high"],
  grok:   ["low", "medium", "high"],
};

/* ---------- pure: catalogs / options ---------- */

export function cloneDefaultModels(){
  const out = {};
  for (const [k, v] of Object.entries(DEFAULT_MODELS)) out[k] = v.slice();
  return out;
}

function normalizeMuseModels(raw){
  if (!Array.isArray(raw)) return [];
  const out = [];
  for (const row of raw){
    if (!row || typeof row !== "object") continue;
    if (typeof row.id !== "string") continue;
    const id = row.id.trim();
    if (!id) continue;
    const copy = { id, launchable: row.launchable, tier: row.tier };
    if (typeof row.label === "string") copy.label = row.label;
    if (row.default === true) copy.default = true;
    out.push(copy);
  }
  return out;
}

/* Apply /api/agents payload into mutable catalogs. Empty/null payload is a no-op.
   Structured Muse rows stay on the return value — never as keys of `models`,
   which would make metadata or model ids appear as agents. */
export function applyAgentsProbe(models, modelEfforts, payload){
  if (!payload || !Object.keys(payload).length)
    return { models, modelEfforts, changed: false, museModels: [] };
  for (const k of Object.keys(models)) delete models[k];
  for (const k of Object.keys(modelEfforts)) delete modelEfforts[k];
  let museModels = [];
  for (const [agent, info] of Object.entries(payload)){
    models[agent] = ["", ...((info && info.models) || [])];
    modelEfforts[agent] = (info && info.efforts) || {};
    if (agent === "muse") museModels = normalizeMuseModels(info && info.muse_models);
  }
  return { models, modelEfforts, changed: true, museModels };
}

export function agentOptionsHTML(models, esc = escDefault){
  return Object.keys(models).map(a => `<option>${esc(a)}</option>`).join("");
}

/* Empty model option label: inherit parent's model when same-agent fork, else default. */
export function modelDefaultLabel(agent, parent, esc = escDefault){
  if (parent && parent.agent === agent && parent.model)
    return `(inherit: ${esc(parent.model)})`;
  return "(default)";
}

export function modelOptionsHTML(models, agent, parent, esc = escDefault){
  const dflt = modelDefaultLabel(agent, parent, esc);
  return (models[agent] || [""]).map(m =>
    `<option value="${esc(m)}">${esc(m) || dflt}</option>`).join("");
}

/* Muse options are server-owned structured rows. Tier is never inferred from
   id, label, limits, order, or the legacy string `models` list. */
export function museModelSelectable(row){
  return !!(row
    && typeof row.id === "string" && row.id
    && row.launchable === true
    && (row.tier === "standard" || row.tier === "discounted"));
}

export function museFreshDefaultId(rows){
  if (!Array.isArray(rows)) return "";
  for (const row of rows){
    if (row && row.default === true && museModelSelectable(row) && row.tier === "standard")
      return row.id;
  }
  return "";
}

function museTierSuffix(tier){
  if (tier === "standard") return " \u2014 Standard";
  if (tier === "discounted") return " \u2014 Discounted";
  return " \u2014 tier unavailable";
}

export function museModelOptionsHTML(rows, esc = escDefault){
  const list = Array.isArray(rows) ? rows : [];
  let html = `<option value=""></option>`;
  for (const row of list){
    if (!row || typeof row !== "object") continue;
    if (typeof row.id !== "string" || !row.id) continue;
    const label = (typeof row.label === "string" && row.label.trim()) ? row.label : row.id;
    const disabled = museModelSelectable(row) ? "" : " disabled";
    html += `<option value="${esc(row.id)}"${disabled}>${esc(label)}${museTierSuffix(row.tier)}</option>`;
  }
  return html;
}

/* Prefer per-model discovered menu; else static per-agent; else common three. */
export function effortLevelsFor(agent, model, modelEfforts, efforts = DEFAULT_EFFORTS){
  const perModel = (modelEfforts[agent] || {})[model];
  const list = (perModel && perModel.levels) || efforts[agent] || ["low", "medium", "high"];
  const dflt = perModel && perModel.default;
  return { list, default: dflt };
}

export function effortOptionsHTML(list, dflt, esc = escDefault){
  return `<option value=""></option>` +
    list.map(e => `<option value="${esc(e)}">${esc(e)}${e === dflt ? " (default)" : ""}</option>`).join("");
}

/* Preserve current effort across agent/model rebuild when still valid. */
export function preservedEffortValue(list, cur){
  return list.includes(cur) ? cur : null;
}

/* ---------- pure: launch seed / lane ---------- */

export function launchSeedFrom(cfg){
  if (!cfg) return { agent: "", model: "", effort: "", dir: "", has: false };
  return {
    agent: cfg.agent || "",
    model: cfg.model || "",
    effort: cfg.effort || "",
    dir: cfg.dir || "",
    has: true,
  };
}

/* Forks: no preselect (conscious lane choice). Plain new: scoped laneFilter. */
export function lanePreselectForActivity(forking, laneFilter){
  return forking ? "" : (laneFilter || "");
}

export function forkRequiresLane(parent, laneID){
  return !!(parent && !laneID);
}

/* ---------- pure: titles / prompts ---------- */

export function seedForkTitleText(src){
  const first = (src || "").split("\n").map(s => s.trim()).find(Boolean) || "Follow-up";
  return first.slice(0, 60);
}

export function forkRationaleFromTurn(text){
  return (text || "").split("\n")[0].slice(0, 140);
}

export function forkPromptFromTurn(text){
  const body = (text || "").replace(/\s+$/, "");
  return body ? "Following up on:\n" +
    body.split("\n").map(l => "> " + l).join("\n") + "\n\n" : "";
}

/* ---------- pure: payloads ---------- */

export function buildCreatePayload({
  title, description, agent, model, effort, dir, parent, rationale, laneID,
}){
  const payload = {
    title,
    description,
    prompt: description || title,
    agent,
    model,
    effort,
    dir: (dir || "").trim(),
    parent: parent || "",
    rationale: parent ? "follow-up on: " + (rationale || "") : "",
  };
  if (laneID) payload.lane_id = laneID;
  return payload;
}

export function buildHeadEditBody(cur, title, description){
  const body = {};
  if (cur && title !== cur.title) body.title = title;
  if (cur && description !== (cur.description || "")) body.description = description;
  return body;
}

export function isNoOpEditBody(body){
  return !body || !Object.keys(body).length;
}

export function isNoOpStationEdit(prev, title, description){
  const p = prev || { title: "", desc: "" };
  return title === p.title && description === (p.desc || "");
}

export function buildStationLabelPayload(station, title, description){
  return { station, title, description };
}

/* ---------- pure: errors / submit chrome ---------- */

export function createErrorField(msg){
  const m = msg || "";
  if (/\b(dir|directory|path)\b/i.test(m)) return "dir";
  if (/\btitle\b/i.test(m)) return "title";
  return "prompt";
}

/* The CTA's *accessible* name. It carries the busy state because the visible
   title deliberately does not: a control's title is not a progress readout,
   and swapping "Start"→"Starting..." resizes the control mid-press. VoiceOver
   still hears the state; sighted users get the .ctadots indicator instead. */
export function submitButtonLabel({ editing, submitting }){
  if (editing) return submitting ? "Saving..." : "Save";
  return submitting ? "Starting..." : "Start";
}

/* The CTA's visible contents: a stable label plus, while submitting, three
   travelling dots. Indeterminate by construction — creating an activity has no
   knowable percentage, and a fake progress bar would be a lie. */
export function submitButtonHTML({ editing, submitting }){
  const label = `<span class="ctalabel">${editing ? "Save" : "Start"}</span>`;
  if (!submitting) return label;
  return label + `<span class="ctadots" aria-hidden="true"><i></i><i></i><i></i></span>`;
}

export function sheetHeadLabels({ editing, editStop }){
  if (editing){
    return {
      head: editStop ? "Edit station" : "Edit activity",
      start: "Save",
    };
  }
  return { head: "New activity", start: "Start" };
}

/* ---------- pure: last-dir storage (throws propagate) ---------- */

export function getLastDir(storage){
  if (!storage || typeof storage.getItem !== "function") return null;
  return storage.getItem(LAST_DIR_KEY);
}

export function setLastDir(storage, dir){
  if (!dir) return;
  if (!storage || typeof storage.setItem !== "function") return;
  storage.setItem(LAST_DIR_KEY, dir);
}

/* ---------- pure: sheet active markers (decision helpers for tests) ---------- */

export function sheetActiveAttrs(active){
  return {
    openClass: !!active,
    inert: !active,
    ariaHidden: active ? "false" : "true",
  };
}

/* ---------- factory ---------- */

export function createSheetsFeature(deps = {}){
  const d = deps;
  const roots = d.roots || {};
  const doc = d.document || (typeof document !== "undefined" ? document : null);
  const esc = typeof d.esc === "function" ? d.esc : escDefault;
  const storage = d.storage || null;
  const api = typeof d.api === "function" ? d.api : null;
  const setTimeoutFn = typeof d.setTimeout === "function" ? d.setTimeout
    : (typeof setTimeout !== "undefined" ? setTimeout : (() => 0));
  const clearTimeoutFn = typeof d.clearTimeout === "function" ? d.clearTimeout
    : (typeof clearTimeout !== "undefined" ? clearTimeout : (() => {}));
  const nowFn = typeof d.now === "function" ? d.now : (() => Date.now());
  const stopsOf = typeof d.stopsOf === "function" ? d.stopsOf : stopsOfDefault;
  const stopLabel = typeof d.stopLabel === "function" ? d.stopLabel : stopLabelDefault;

  /* mutable catalogs — same semantics as former module-level MODELS/EFFORTS */
  const MODELS = cloneDefaultModels();
  const EFFORTS = { ...DEFAULT_EFFORTS };
  for (const k of Object.keys(EFFORTS)) EFFORTS[k] = EFFORTS[k].slice();
  const MODEL_EFFORTS = {};
  let MUSE_MODELS = [];

  let ncParent = "";
  let ncRationale = "";
  let ncMuseInherit = false;
  let ncEdit = "";
  let ncEditStop = "";
  let ncForwardSources = [];
  let newActivitySubmitting = false;
  let bound = false;
  let probed = false;
  let probeGeneration = 0;
  let titleTimer = null;
  const cleanups = [];
  const fieldErrorCleanups = new Set();

  function root(name){
    if (roots[name]) return roots[name];
    if (roots["#" + name]) return roots["#" + name];
    if (doc && typeof doc.getElementById === "function") return doc.getElementById(name);
    if (doc && typeof doc.querySelector === "function") return doc.querySelector("#" + name);
    return null;
  }

  function q(sel){
    if (typeof d.querySelector === "function") return d.querySelector(sel);
    if (doc && typeof doc.querySelector === "function") return doc.querySelector(sel);
    if (sel && sel[0] === "#") return root(sel.slice(1));
    return null;
  }

  function allSheets(){
    if (typeof d.querySelectorAll === "function") return [...d.querySelectorAll(".sheet")];
    if (doc && typeof doc.querySelectorAll === "function") return [...doc.querySelectorAll(".sheet")];
    return Object.keys(roots)
      .map(k => roots[k])
      .filter(el => el && (el.classList?.contains?.("sheet") ||
        (typeof el.className === "string" && /\bsheet\b/.test(el.className))));
  }

  function nodeById(id){
    return typeof d.nodeById === "function" ? d.nodeById(id) : null;
  }

  function setSheetActive(sheet, active){
    if (!sheet) return;
    const a = sheetActiveAttrs(active);
    if (sheet.classList){
      sheet.classList.toggle("open", a.openClass);
    } else {
      const cls = String(sheet.className || "").replace(/\bopen\b/g, "").trim();
      sheet.className = a.openClass ? (cls + " open").trim() : cls;
    }
    if (typeof sheet.toggleAttribute === "function")
      sheet.toggleAttribute("inert", a.inert);
    else if (a.inert) sheet.inert = true;
    else {
      try { delete sheet.inert; } catch { sheet.inert = false; }
    }
    if (typeof sheet.setAttribute === "function")
      sheet.setAttribute("aria-hidden", a.ariaHidden);
    const fields = typeof sheet.querySelectorAll === "function"
      ? sheet.querySelectorAll("input, textarea, select") : [];
    for (const el of fields){
      if (active){
        if (el.dataset && el.dataset.sheetDisabledByClose) el.disabled = false;
        if (el.dataset) delete el.dataset.sheetDisabledByClose;
      } else if (!el.disabled){
        el.disabled = true;
        if (el.dataset) el.dataset.sheetDisabledByClose = "1";
      }
    }
  }

  function openSheet(id){
    const backdrop = root("backdrop");
    if (backdrop && backdrop.classList) backdrop.classList.add("on");
    else if (backdrop) backdrop.className = ((backdrop.className || "") + " on").trim();
    for (const s of allSheets()){
      const match = typeof s.matches === "function" ? s.matches(id)
        : (s.id && id === "#" + s.id);
      setSheetActive(s, match);
    }
  }

  function closeSheets(){
    ncForwardSources = [];
    const backdrop = root("backdrop");
    if (backdrop && backdrop.classList) backdrop.classList.remove("on");
    else if (backdrop)
      backdrop.className = String(backdrop.className || "").replace(/\bon\b/g, "").trim();
    for (const s of allSheets()) setSheetActive(s, false);
  }

  function clearFieldError(el){
    if (!el) return;
    if (typeof el.removeAttribute === "function"){
      el.removeAttribute("aria-invalid");
      el.removeAttribute("aria-describedby");
    }
    const id = el.id ? el.id + "-err" : "";
    if (!id) return;
    let err = null;
    if (doc && typeof doc.getElementById === "function") err = doc.getElementById(id);
    if (!err && el.nextElementSibling && el.nextElementSibling.id === id)
      err = el.nextElementSibling;
    if (err && typeof err.remove === "function") err.remove();
  }

  function fieldError(el, msg){
    if (!el) return;
    clearFieldError(el);
    const id = el.id + "-err";
    if (typeof el.setAttribute === "function"){
      el.setAttribute("aria-invalid", "true");
      el.setAttribute("aria-describedby", id);
    }
    let err;
    if (typeof d.createElement === "function") err = d.createElement("div");
    else if (doc && typeof doc.createElement === "function") err = doc.createElement("div");
    if (!err) return;
    err.className = "fielderr";
    err.id = id;
    err.textContent = msg;
    if (typeof el.insertAdjacentElement === "function")
      el.insertAdjacentElement("afterend", err);
    else if (el.parentNode && typeof el.parentNode.insertBefore === "function"){
      if (el.nextSibling) el.parentNode.insertBefore(err, el.nextSibling);
      else el.parentNode.appendChild(err);
    }
    const clear = () => {
      clearFieldError(el);
      if (typeof el.removeEventListener === "function") el.removeEventListener("input", clear);
      fieldErrorCleanups.delete(clear);
    };
    if (typeof el.addEventListener === "function") el.addEventListener("input", clear);
    fieldErrorCleanups.add(clear);
  }

  function fillAgents(){
    const ag = root("nc_agent");
    if (ag) ag.innerHTML = agentOptionsHTML(MODELS, esc);
  }

  function museRowById(id){
    if (!id) return null;
    return MUSE_MODELS.find(r => r && r.id === id) || null;
  }

  function applyMuseModelValue(mo, { prev = "", preserveSelection = false } = {}){
    const p = nodeById(ncParent);
    const inherited = ncMuseInherit && p && p.agent === "muse" ? (p.model || "") : "";
    if (preserveSelection && prev && museModelSelectable(museRowById(prev))){
      mo.value = prev;
      return;
    }
    if (inherited && museModelSelectable(museRowById(inherited))){
      mo.value = inherited;
      return;
    }
    mo.value = museFreshDefaultId(MUSE_MODELS) || "";
  }

  function fillModels(opts = {}){
    const mo = root("nc_model");
    if (!mo) return;
    const prev = mo.value;
    const agentEl = root("nc_agent");
    const agent = agentEl ? agentEl.value : "";
    const p = nodeById(ncParent);
    if (agent === "muse"){
      mo.innerHTML = museModelOptionsHTML(MUSE_MODELS, esc);
      applyMuseModelValue(mo, { prev, preserveSelection: !!opts.preserveSelection });
    } else {
      mo.innerHTML = modelOptionsHTML(MODELS, agent, p, esc);
    }
  }

  function newchatIsOpen(){
    const sheet = root("newchat");
    return !!(sheet && sheet.classList && sheet.classList.contains("open"));
  }

  function applyOpenSheetAfterCatalog(){
    const live = !ncEdit && newchatIsOpen();
    const ag = root("nc_agent");
    const prevAgent = live && ag ? ag.value : "";
    fillAgents();
    if (live && ag && prevAgent && Object.prototype.hasOwnProperty.call(MODELS, prevAgent))
      ag.value = prevAgent;
    fillModels({ preserveSelection: live });
    fillEfforts();
  }

  function fillEfforts(){
    const ef = root("nc_effort");
    if (!ef) return;
    const cur = ef.value;
    const agentEl = root("nc_agent");
    const modelEl = root("nc_model");
    const agent = agentEl ? agentEl.value : "";
    const model = modelEl ? modelEl.value : "";
    const { list, default: dflt } = effortLevelsFor(agent, model, MODEL_EFFORTS, EFFORTS);
    ef.innerHTML = effortOptionsHTML(list, dflt, esc);
    if (list.includes(cur)) ef.value = cur;
  }

  function prepareLaunchConfig(cfg){
    fillAgents(); fillModels(); fillEfforts();
    const p = cfg || nodeById(ncParent);
    if (p){
      const ag = root("nc_agent");
      if (ag){
        if (p.agent && ![...ag.options].some(o => o.value === p.agent))
          if (typeof ag.insertAdjacentHTML === "function")
            ag.insertAdjacentHTML("beforeend", `<option>${esc(p.agent)}</option>`);
          else {
            const opt = (doc && doc.createElement) ? doc.createElement("option") : null;
            if (opt){ opt.textContent = p.agent; opt.value = p.agent; ag.appendChild(opt); }
          }
        if (p.agent) ag.value = p.agent;
      }
      fillModels(); fillEfforts();   /* model + effort lists follow the (possibly re-set) agent */
      const mo = root("nc_model");
      const agentNow = ag ? ag.value : "";
      if (mo && agentNow !== "muse"){
        if (p.model && ![...mo.options].some(o => o.value === p.model))
          if (typeof mo.insertAdjacentHTML === "function")
            mo.insertAdjacentHTML("beforeend", `<option value="${esc(p.model)}">${esc(p.model)}</option>`);
          else {
            const opt = (doc && doc.createElement) ? doc.createElement("option") : null;
            if (opt){ opt.textContent = p.model; opt.value = p.model; mo.appendChild(opt); }
          }
        mo.value = p.model || "";
      }
      const ef = root("nc_effort");
      if (ef){
        const want = p.effort || "";
        ef.value = (typeof ef.querySelector === "function"
          ? ef.querySelector(`option[value="${esc(want)}"]`) : null)
          ? want : "";
      }
      const dir = root("nc_dir");
      if (dir) dir.value = p.dir || "";
    } else {
      const ef = root("nc_effort");
      if (ef) ef.value = "";
      const dir = root("nc_dir");
      if (dir) dir.value = "";
    }
    for (const id of ["nc_agent", "nc_model", "nc_effort", "nc_dir"]){
      const el = root(id);
      if (!el) continue;
      el.disabled = false;
      if (el.dataset) delete el.dataset.sheetDisabledByClose;
    }
  }

  function prepareNewActivityLane(){
    const forking = !!ncParent;
    const laneFilter = typeof d.laneFilter === "function" ? d.laneFilter() : "";
    if (typeof d.fillLaneSelect === "function")
      d.fillLaneSelect(root("nc_lane"), lanePreselectForActivity(forking, laneFilter), true, false);
    const hint = root("nc_lane_hint");
    if (hint) hint.hidden = !forking;
    const laneNew = root("nc_lane_new");
    if (laneNew) laneNew.value = "";
    clearFieldError(root("nc_lane_new"));
    clearFieldError(root("nc_lane"));
    if (typeof d.syncLanePicker === "function") d.syncLanePicker(root("nc_lane"));
  }

  /* Single writer for the CTA's contents. Every caller goes through here so
     nothing writes bare textContent over the label/dots structure. */
  function renderStartButton({ editing, submitting }){
    const btn = root("nc_start");
    if (!btn) return;
    btn.innerHTML = submitButtonHTML({ editing, submitting });
    if (typeof btn.setAttribute === "function"){
      btn.setAttribute("aria-busy", submitting ? "true" : "false");
      btn.setAttribute("aria-label", submitButtonLabel({ editing, submitting }));
    }
  }

  function setNewActivitySubmitting(active){
    newActivitySubmitting = !!active;
    const btn = root("nc_start");
    if (!btn) return;
    btn.disabled = newActivitySubmitting;
    renderStartButton({ editing: !!ncEdit, submitting: newActivitySubmitting });
  }

  function scheduleTitleFocus(selectAll){
    if (titleTimer != null) clearTimeoutFn(titleTimer);
    titleTimer = setTimeoutFn(() => {
      titleTimer = null;
      const t = root("nc_title");
      if (!t) return;
      /* selectAll survives only for seedForkTitle — a machine-generated guess
         the user is expected to replace wholesale (HIG select-all case). */
      if (selectAll){
        if (typeof t.focus === "function") t.focus();
        if (typeof t.select === "function") t.select();
        return;
      }
      focusAtEnd(t);
    }, 0);
  }

  function seedForkTitle(src){
    const t = root("nc_title");
    if (t) t.value = seedForkTitleText(src);
    scheduleTitleFocus(true);
  }

  function resetCreateChrome(){
    const labels = sheetHeadLabels({ editing: false });
    const sheet = root("newchat");
    if (sheet && sheet.classList) sheet.classList.remove("editing");
    const head = root("nc_head");
    if (head) head.textContent = labels.head;
    renderStartButton({ editing: false, submitting: false });
  }

  function forkFromTurn(text, parent){
    ncForwardSources = [];
    const sel = typeof d.sel === "function" ? d.sel() : "";
    ncParent = parent || sel;
    ncMuseInherit = !!(nodeById(ncParent) && nodeById(ncParent).agent === "muse");
    ncRationale = forkRationaleFromTurn(text);
    setNewActivitySubmitting(false);
    seedForkTitle(text);
    const prompt = root("nc_prompt");
    if (prompt) prompt.value = forkPromptFromTurn(text);
    prepareLaunchConfig();
    prepareNewActivityLane();
    openSheet("#newchat");
  }

  function forkFromStation(id){
    const n = nodeById(id);
    if (!n) return;
    ncForwardSources = [];
    ncEdit = ""; ncEditStop = "";
    ncParent = id;
    ncMuseInherit = n.agent === "muse";
    ncRationale = n.title || "";
    setNewActivitySubmitting(false);
    resetCreateChrome();
    seedForkTitle(n.description || n.title);
    const prompt = root("nc_prompt");
    if (prompt) prompt.value = "";
    prepareLaunchConfig();
    prepareNewActivityLane();
    openSheet("#newchat");
  }

  function openActivityEditor(id, stopTime){
    const n = nodeById(id);
    if (!n) return;
    ncForwardSources = [];
    ncEdit = id;
    ncEditStop = stopTime || "";
    ncParent = "";
    setNewActivitySubmitting(false);
    const sheet = root("newchat");
    if (sheet && sheet.classList) sheet.classList.add("editing");
    const stop = ncEditStop ? stopsOf(n).find(s => s.time === ncEditStop) : null;
    const label = stop ? stopLabel(stop) : { title: n.title || "", desc: n.description || "" };
    const labels = sheetHeadLabels({ editing: true, editStop: ncEditStop });
    const head = root("nc_head");
    if (head) head.textContent = labels.head;
    renderStartButton({ editing: true, submitting: false });
    clearFieldError(root("nc_title"));
    clearFieldError(root("nc_prompt"));
    const title = root("nc_title");
    if (title) title.value = label.title;
    const prompt = root("nc_prompt");
    if (prompt) prompt.value = label.desc;
    openSheet("#newchat");
    scheduleTitleFocus(false);
  }

  /* prompt/focusTitle serve Send-to "Start new chat…" (bookmarks openNewActivity
     dep). Defaults keep plain "+" identical. Never bind this bare as a click
     listener — the Event would be destructured as options; use () => openNewActivity(). */
  function openNewActivity({ prompt = "", focusTitle = false, forwardSources = [] } = {}){
    ncForwardSources = Array.isArray(forwardSources) ? forwardSources.slice() : [];
    ncParent = ""; ncRationale = ""; ncEdit = ""; ncEditStop = "";
    ncMuseInherit = false;
    resetCreateChrome();
    setNewActivitySubmitting(false);
    const title = root("nc_title");
    if (title) title.value = "";
    const promptEl = root("nc_prompt");
    if (promptEl) promptEl.value = prompt || "";
    prepareLaunchConfig();
    prepareNewActivityLane();
    const lastDir = getLastDir(storage);
    const dir = root("nc_dir");
    if (lastDir && dir && !dir.value) dir.value = lastDir;
    openSheet("#newchat");
    if (focusTitle) scheduleTitleFocus(false);
  }

  function afterNodeMutation(){
    if (typeof d.invalidateCardsSig === "function") d.invalidateCardsSig();
    if (typeof d.invalidateChat === "function") d.invalidateChat();
    if (typeof d.invalidateMap === "function") d.invalidateMap();
    if (typeof d.renderCards === "function") d.renderCards();
    if (typeof d.renderChatHead === "function") d.renderChatHead();
    if (typeof d.renderMap === "function") d.renderMap();
  }

  async function onStartClick(){
    if (newActivitySubmitting) return;
    clearFieldError(root("nc_title"));
    clearFieldError(root("nc_prompt"));
    clearFieldError(root("nc_dir"));
    clearFieldError(root("nc_lane"));
    clearFieldError(root("nc_model"));
    const titleEl = root("nc_title");
    const promptEl = root("nc_prompt");
    const title = (titleEl && titleEl.value || "").trim();
    const description = (promptEl && promptEl.value || "").trim();

    if (ncEdit){
      if (!title){
        fieldError(titleEl, "Enter a title.");
        if (titleEl && typeof titleEl.focus === "function") titleEl.focus();
        return;
      }
      const id = ncEdit, cur = nodeById(id);
      if (ncEditStop){
        const stop = cur && stopsOf(cur).find(s => s.time === ncEditStop);
        const prev = stop ? stopLabel(stop) : { title: "", desc: "" };
        if (isNoOpStationEdit(prev, title, description)){ closeSheets(); return; }
        setNewActivitySubmitting(true);
        try {
          const lbl = api
            ? await api(`/api/nodes/${encodeURIComponent(id)}`, {
              method: "PATCH",
              body: JSON.stringify(buildStationLabelPayload(ncEditStop, title, description)),
            })
            : { title, desc: description };
          if (cur){
            (cur.station_labels ||= {})[ncEditStop] = {
              title: (lbl && lbl.title) || title,
              desc: (lbl && lbl.desc) || description,
            };
          }
          closeSheets();
          if (typeof d.invalidateMap === "function") d.invalidateMap();
          if (typeof d.renderMap === "function") d.renderMap();
        } catch (err) {
          fieldError(titleEl, (err && err.message) || "Could not save.");
          if (titleEl && typeof titleEl.focus === "function") titleEl.focus();
        } finally { setNewActivitySubmitting(false); }
        return;
      }
      const body = buildHeadEditBody(cur, title, description);
      if (isNoOpEditBody(body)){ closeSheets(); return; }
      setNewActivitySubmitting(true);
      try {
        const n = api
          ? await api(`/api/nodes/${encodeURIComponent(id)}`, {
            method: "PATCH", body: JSON.stringify(body),
          })
          : { ...cur, ...body };
        if (typeof d.updateLocalNode === "function") d.updateLocalNode(n);
        closeSheets();
        afterNodeMutation();
      } catch (err) {
        fieldError(titleEl, (err && err.message) || "Could not save.");
        if (titleEl && typeof titleEl.focus === "function") titleEl.focus();
      } finally { setNewActivitySubmitting(false); }
      return;
    }

    const laneChoice = typeof d.readLaneChoice === "function"
      ? d.readLaneChoice(root("nc_lane"), root("nc_lane_new"))
      : { laneID: root("nc_lane") ? root("nc_lane").value : "" };
    if (laneChoice.error){
      const laneNew = root("nc_lane_new");
      fieldError(laneNew, laneChoice.error);
      if (laneNew && typeof laneNew.focus === "function") laneNew.focus();
      if (laneNew && typeof laneNew.scrollIntoView === "function")
        laneNew.scrollIntoView({ block: "center" });
      return;
    }
    if (forkRequiresLane(ncParent, laneChoice.laneID)){
      const lane = root("nc_lane");
      fieldError(lane, "Pick a lane for this fork (or use /clear to continue the thread).");
      if (lane && typeof lane.focus === "function") lane.focus();
      if (lane && typeof lane.scrollIntoView === "function")
        lane.scrollIntoView({ block: "center" });
      return;
    }
    const agent = root("nc_agent") ? root("nc_agent").value : "";
    const model = root("nc_model") ? root("nc_model").value : "";
    if (agent === "muse" && !museModelSelectable(museRowById(model))){
      const mo = root("nc_model");
      fieldError(mo, "No launchable Standard Muse model is selected. Pick a Standard or Discounted row.");
      if (mo && typeof mo.focus === "function") mo.focus();
      if (mo && typeof mo.scrollIntoView === "function")
        mo.scrollIntoView({ block: "center" });
      return;
    }
    const payload = buildCreatePayload({
      title,
      description,
      agent,
      model,
      effort: root("nc_effort") ? root("nc_effort").value : "",
      dir: root("nc_dir") ? root("nc_dir").value : "",
      parent: ncParent,
      rationale: ncRationale,
      laneID: laneChoice.laneID,
    });
    if (!payload.title){
      fieldError(titleEl, "Enter a title before starting.");
      if (titleEl && typeof titleEl.focus === "function") titleEl.focus();
      if (titleEl && typeof titleEl.scrollIntoView === "function")
        titleEl.scrollIntoView({ block: "center" });
      return;
    }
    setNewActivitySubmitting(true);
    try {
      const n = api
        ? await api("/api/nodes", { method: "POST", body: JSON.stringify(payload) })
        : { id: "new" };
      const echoLaunchPrompt = !(n && (n.initial_delivery === "not_sent"));
      if (n && n.id && !echoLaunchPrompt){
        /* Cross-feature storage contract with composer.js, kept as a literal
           here so sheets does not import a later feature module. The server
           already created this exact node; select it and preserve recovery
           rather than leaving Start able to create a duplicate node. */
        if (storage) storage.setItem("scimux-draft:" + n.id, payload.prompt);
        if (payload.prompt && ncForwardSources.length)
          writePendingForwards(storage, n.id, ncForwardSources);
        if (typeof d.alert === "function") d.alert(
          n.initial_error ||
          "Claude did not start. The initial prompt was not delivered and has been restored as a draft."
        );
      }
      /* Every accepted launch waits for the transcript turn. Even an
         acknowledged create response has no durable turn address itself. */
      if (n && n.id && echoLaunchPrompt && payload.prompt && ncForwardSources.length){
        writePendingForwards(storage, n.id, ncForwardSources);
        writeAwaitingForward(storage, n.id, payload.prompt, 0);
      }
      if (laneChoice.lane && typeof d.uiMutate === "function"){
        const list = typeof d.laneList === "function" ? d.laneList() : [];
        d.uiMutate({ k: "lanes", lanes: [...list, laneChoice.lane] });
      }
      setLastDir(storage, payload.dir);
      if (promptEl) promptEl.value = "";
      if (titleEl) titleEl.value = "";
      ncForwardSources = [];
      closeSheets();
      if (typeof d.invalidateStateEtag === "function") d.invalidateStateEtag();
      if (typeof d.tick === "function") await d.tick();
      /* Optimistic echo for the launch prompt. composer.js owns this for every
         later turn, but the first prompt travels with the launch config and so
         had no echo at all: the chat stayed empty until the transport wrote the
         user turn — minutes on a cold local model (pi/ACP, reported 2026-08-18)
         — and the human's own words were not even on screen. Set *before*
         select(), because select() runs refreshChat and the first paint should
         already carry the bubble. Not painted directly: #msgs still holds the
         previous node until that refresh lands.

         Skipped when the server reported the prompt undelivered — it has been
         restored to the recovery draft above, and an echo titled "delivering"
         would claim something that did not happen.

         The record is a literal, not composer.buildSentEcho: sheets does not
         import a later feature module (same contract as the draft key above).
         seen:null is the no-prior-surface case — a brand-new node has no turns,
         so retireSentEcho's user-turn count settles it. */
      if (echoLaunchPrompt && payload.prompt && n && n.id &&
          typeof d.setSentEcho === "function"){
        d.setSentEcho({
          node: n.id, text: payload.prompt, atts: [], at: nowFn(), seen: null,
        });
      }
      if (typeof d.select === "function") d.select(n.id);
      const desktop = typeof d.isDesktop === "function" ? d.isDesktop() : false;
      if (!desktop && typeof d.setLevel === "function") d.setLevel(1);
    } catch (err) {
      const msg = (err && err.message) || "Could not start this activity.";
      const field = createErrorField(msg);
      const target = field === "dir" ? root("nc_dir")
        : field === "title" ? root("nc_title") : root("nc_prompt");
      fieldError(target, msg);
      if (target && typeof target.focus === "function") target.focus();
      if (target && typeof target.scrollIntoView === "function")
        target.scrollIntoView({ block: "center" });
    } finally {
      setNewActivitySubmitting(false);
    }
  }

  function onAgentChange(){
    ncMuseInherit = false;
    const mo = root("nc_model");
    if (mo) mo.value = "";
    fillModels(); fillEfforts();
  }
  function onModelChange(){
    fillEfforts();
  }

  function probeAgents(){
    if (probed || !api) return;
    probed = true;
    const generation = ++probeGeneration;
    Promise.resolve(api("/api/agents")).then(j => {
      if (!bound || generation !== probeGeneration) return;
      const r = applyAgentsProbe(MODELS, MODEL_EFFORTS, j);
      if (r.changed){
        MUSE_MODELS = Array.isArray(r.museModels) ? r.museModels : [];
        applyOpenSheetAfterCatalog();
      }
    }).catch(() => {});
  }

  function on(el, type, fn, opts){
    if (!el || typeof el.addEventListener !== "function") return;
    el.addEventListener(type, fn, opts);
    cleanups.push(() => {
      if (typeof el.removeEventListener === "function")
        el.removeEventListener(type, fn, opts);
    });
  }

  function bind(){
    if (bound) return;
    bound = true;
    fillAgents(); fillModels(); fillEfforts();
    closeSheets();
    on(root("backdrop"), "click", closeSheets);
    on(root("burger"), "click", () => openSheet("#menu"));
    on(root("plusbtn"), "click", () => openNewActivity());
    on(root("nc_agent"), "change", onAgentChange);
    on(root("nc_model"), "change", onModelChange);
    on(root("nc_start"), "click", () => { onStartClick(); });
    probeAgents();
  }

  function destroy(){
    if (!bound) return;
    bound = false;
    probeGeneration++;
    for (const c of cleanups.splice(0)) c();
    if (titleTimer != null){ clearTimeoutFn(titleTimer); titleTimer = null; }
    for (const clear of [...fieldErrorCleanups]) clear();
    fieldErrorCleanups.clear();
    /* strip any remaining fielderr nodes under owned sheets */
    for (const id of ["nc_title", "nc_prompt", "nc_dir", "nc_lane", "nc_lane_new", "nc_model"])
      clearFieldError(root(id));
  }

  return {
    bind,
    destroy,
    openSheet,
    closeSheets,
    openActivityEditor,
    openNewActivity,
    forkFromTurn,
    forkFromStation,
    fieldError,
  };
}
