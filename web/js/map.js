/* Journeys map feature: tabs, lane chips, stack/wall render, selection, folds.
 *
 * Packet 7B ownership inventory
 * -----------------------------
 * Owned roots / controls:
 *   - #maptabs, #lanechips, #mapwrap, #mapscroll (scroll for toolbar)
 *   - #maptoolbar (floating wall-map selection bar)
 *   - #mapfullbtn, #farebtn (map chrome toggles)
 *   - map group sheet controls #tab_head/#tab_name/#tab_lanes/#tab_save/#tab_del
 *     (map-specific sheet body; generic sheet open/close stays injected)
 *
 * Inputs (getters / injected deps — never implicit app globals):
 *   - nodes, UI.groups / UI.lanes, laneFilter, level, uiLoaded
 *   - map-open body class (desktop column mode)
 *   - laneModel, laneColor/Name/List/ById, agentLogo, icons
 *   - storage (localStorage), document/window, CSS.escape
 *
 * Outputs:
 *   - HTML into maptabs / lanechips / mapwrap / maptoolbar
 *   - body.map-full class; mapfullbtn ARIA/glyph; farebtn.on + aria-pressed
 *     + on/off layer-group SVG (fare layer; revealed only under body.map-full)
 *   - localStorage: scimux-maptab, scimux-mapfold, scimux-mapfold-known,
 *     scimux-mapfull, scimux-fare
 *   - uiMutate groups/lanes; callbacks: select, setLevel, setLaneFilter,
 *     loadChatHistory, jumpChatToNow, openActivityEditor, forkFromStation,
 *     addStationBookmark, exitThread, openSheet/closeSheets, renderCards
 *
 * Events owned after one idempotent bind():
 *   - #maptabs click (select / second-tap edit / ✕ remove / + create)
 *   - #lanechips click + longpress rename
 *   - #mapwrap click (fold / station / golane / goorigin / legacy wall btns)
 *   - #mapwrap longpress station → editor (+ wall selection when full)
 *   - #maptoolbar click (open chat / edit / bookmark / fork / exit)
 *   - #mapscroll scroll + window resize → positionMapToolbar
 *   - #mapfullbtn / #farebtn click
 *   - #tab_save / #tab_del click (map group sheet)
 *
 * Not owned (stay shell / other features):
 *   - static Journeys phead order and “Activities ›” #mapclose label
 *   - #mapclose / #journeybtn / #scrim / document touch spatial navigation
 *   - emptyTap on #mapscroll / #cardlist (shell fold-back)
 *   - document Escape → setFull(false) (app-level key handler)
 *   - exitThread implementation (shared with cards; injected)
 *   - generic sheets, new-node/fork configuration sheets
 *   - cards, chat, composer, bookmarks, notes, search, polling
 *
 * Reuses (no algorithm duplication):
 *   lanes.js: groupLanes, servedLanes, forkKind, stopsOf, stopKey, stopLabel,
 *     newestFirst, parentStopIndex, laneColumnOrder, byNameID, stampMS
 *   map-model.js: statusText, headStopKey, toggleMapSelection
 *   format.js: esc, fmtStamp, stampMS, contrastText
 *   cards.js: cardConfigText (station caption only)
 *
 * Module size: above the ~500-line soft guide — one factory owns the complete
 * Journeys surface (stack + wall SVG + selection toolbar + fold/tab state).
 */

import { esc, fmtStamp, stampMS, contrastText as contrastTextPure } from "./format.js";
import {
  byNameID,
  groupLanes,
  servedLanes,
  forkKind as forkKindMod,
  stopsOf,
  stopKey,
  stopLabel,
  newestFirst,
  parentStopIndex,
  laneColumnOrder,
} from "./lanes.js";
import { statusText, headStopKey, toggleMapSelection } from "./map-model.js";
import { cardConfigText } from "./cards.js";

/* ---------- storage keys (public contract) ---------- */

export const MAP_TAB_KEY = "scimux-maptab";
export const MAP_FOLD_KEY = "scimux-mapfold";
export const MAP_FOLD_KNOWN_KEY = "scimux-mapfold-known";
export const MAP_FULL_KEY = "scimux-mapfull";
export const MAP_FARE_KEY = "scimux-fare";

export const ATTN_GLOW_DEF = `<defs><filter id="attnglow" x="-80%" y="-80%" width="260%" height="260%">
      <feGaussianBlur stdDeviation="3.2"/></filter></defs>`;

/* ---------- pure visibility / tab / fold / selection decisions ---------- */

/* full screen shows the map regardless of the column-mode map-open flag */
export function mapIsVisible({ isDesktop, mapFull, mapOpen, level } = {}){
  return isDesktop ? (!!mapFull || !!mapOpen) : level === 3;
}

/* validate the saved group only against loaded state — before loadUI
   resolves, groups is the empty default and would wrongly reset it */
export function resolveMapTab(mapTab, groups, uiLoaded){
  if (uiLoaded && mapTab !== "all" && !(groups || []).some(t => t.id === mapTab)) return "all";
  return mapTab || "all";
}

export function mapRenderDecision(sig, prevSig){
  return sig === prevSig ? "skip" : "rebuild";
}

export function clearMapSelection(){
  return { mapSel: "", mapSelKey: "", mapSelStop: "" };
}

/* New lanes start folded; prune deleted lanes from fold + known sets. */
export function applyNewLaneFolds(mapFold, mapFoldKnown, liveIds){
  const live = new Set(liveIds || []);
  const fold = new Set(mapFold || []);
  const known = new Set(mapFoldKnown || []);
  for (const id of live) if (!known.has(id)){
    fold.add(id);
    known.add(id);
  }
  for (const id of [...fold]) if (!live.has(id)) fold.delete(id);
  for (const id of [...known]) if (!live.has(id)) known.delete(id);
  return { mapFold: fold, mapFoldKnown: known };
}

/* Focusing a lane always unfolds its block so the chip tap is not a no-op. */
export function unfoldFocusLane(mapFold, focusLane){
  if (!focusLane || !mapFold || !mapFold.has(focusLane)) return new Set(mapFold || []);
  const next = new Set(mapFold);
  next.delete(focusLane);
  return next;
}

export function toggleFoldId(mapFold, id){
  const next = new Set(mapFold || []);
  if (next.has(id)) next.delete(id); else next.add(id);
  return next;
}

/* Lane chip: second tap clears scope and folds; first tap focuses and unfolds. */
export function chipTapPlan({ laneFilter, id, mapFold } = {}){
  if (laneFilter === id){
    const next = new Set(mapFold || []);
    next.add(id);
    return { kind: "clear-scope", mapFold: next };
  }
  const next = new Set(mapFold || []);
  if (next.has(id)) next.delete(id);
  return { kind: "focus", mapFold: next };
}

export function selectedStopOf(nodes, mapSel, mapSelStop){
  const n = (nodes || []).find(x => x.id === mapSel);
  if (!n) return null;
  const chain = stopsOf(n);
  if (mapSelStop) return chain.find(s => s.time === mapSelStop) || null;
  return chain.find(s => s.head) || null;
}

export function laneChipStyle(color, selected, { escape = esc, contrastText = contrastTextPure } = {}){
  const c = escape(color);
  if (!selected) return `border-color:${c};color:${c}`;
  return `border-color:${c};background:${c};color:${escape(contrastText(color))}`;
}

export function mapTabsHTML(mapTab, groups, { escape = esc } = {}){
  const sorted = [...(groups || [])].sort(byNameID);
  return (
    `<button data-mt="all" class="${mapTab === "all" ? "on" : ""}">All</button>` +
    sorted.map(t => `<button data-mt="${escape(t.id)}" class="${mapTab === t.id ? "on" : ""}">
       ${escape(t.name)}<span class="x" data-rm="${escape(t.id)}">&#10005;</span></button>`).join("") +
    `<button data-mt="+" class="add" aria-label="add map">+</button>`
  );
}

/* Stack map: one block per in-group lane with stops newest-first. */
export function buildStackBlocks(lanes, stations, { served = servedLanes, inGroup, stops = stopsOf, sortStops = newestFirst } = {}){
  return (lanes || []).filter(l => inGroup(l.id)).map(l => ({
    lane: l,
    rows: (stations || []).filter(n => served(n).includes(l.id))
      .flatMap(stops).sort(sortStops),
  })).filter(b => b.rows.length)
    .sort((a, b) => byNameID(a.lane, b.lane));
}

export function stackMapSignature(blocks, { mapFold, focusLane, mapTab, mapFull, lm } = {}){
  const hasAttn = b => b.rows.some(s => s.n.attention) ? 1 : 0;
  const fold = mapFold || new Set();
  return JSON.stringify((blocks || []).map(b => [b.lane.id, b.lane.name, lm.color(b.lane.id),
      fold.has(b.lane.id), hasAttn(b),
      b.rows.map(s => { const l = stopLabel(s); return [stopKey(s), s.time, s.head, l.title, l.desc,
        s.n.agent, s.n.model, s.n.effort, s.n.lane_id, s.n.parent || "", s.n.ended_at || "",
        s.n.live, s.n.attention]; })]))
    + "|" + focusLane + "|" + mapTab + "|" + mapFull;
}

/* Fare fingerprint for wall re-render when the overlay is on. Uses fare_*
   totals (not last_activity) so a token/cost poll update actually rebuilds
   the meter; fareOff keeps the field inert so idle polls stay skip-able. */
function fareFingerprint(n){
  if (!n) return "";
  return [
    n.fare_total ?? "",
    n.fare_turns ?? "",
    n.fare_cost_complete ? 1 : 0,
  ].join(",");
}

export function wallMapSignature(rows, cols, { focusLane, mapTab, mapSel, mapSelKey, fareOn } = {}){
  return "wall|" + (cols || []).join(",") + "|" + focusLane + "|" + mapTab + "|" +
    mapSel + "|" + mapSelKey + "|" + (fareOn ? "F" : "") + "|" +
    JSON.stringify((rows || []).map(s => { const l = stopLabel(s); return [stopKey(s), s.time, s.head, s.n.created_at,
      l.title, l.desc, s.n.agent,
      s.n.model, s.n.effort, s.n.lane_id, s.n.parent || "", s.n.ended_at || "", s.n.live,
      s.n.attention, s.n.ctx_pct == null ? -1 : s.n.ctx_pct,
      fareOn ? fareFingerprint(s.n) : 0]; }));
}

export function attentionStationSVG(x, y, op){
  return `<circle class="attnstation-glow" style="--attn-op:${op}" cx="${x}" cy="${y}" r="10.5" fill="var(--attn)" filter="url(#attnglow)"/>
          <circle class="attnstation-ring" style="--attn-op:${op}" cx="${x}" cy="${y}" r="9.5" fill="none" stroke="var(--attn)" stroke-width="2.5"/>`;
}

// terminalStationSVG marks an ended thread that sits *mid-lane* — the mainline
// continues north past it (newer stations above). The station's filled circle
// stays on the mainline (drawn by the caller); this adds the terminus spur — up
// out of the station (12 o'clock) then curving LEFT to a short vertical buffer
// bar (11–10 o'clock, convex bulge top-right). Left, because the fork cue hooks
// right (see the stack renderer): opposite sides keep a station that is both
// ended and a fork child uncluttered. Unlike the old crossbar "T", the mainline
// passes straight through the circle, so a lane continuing north is never
// crossed. col is the already-escaped lane colour. For an ended station that is
// the *north (top) terminus* of its lane — nothing continues past it — use
// terminalCapSVG instead: a straight buffer stop, not a sideways spur.
export function terminalStationSVG(x, y, op, col){
  return `<path d="M ${x} ${y} C ${x} ${y - 14} ${x - 6.3} ${y - 19} ${x - 15} ${y - 24}"
          fill="none" stroke="${col}" stroke-width="2.5" stroke-linecap="round" opacity="${op}"/>
          <line x1="${x - 15}" y1="${y - 29}" x2="${x - 15}" y2="${y - 19}" stroke="${col}" stroke-width="3" stroke-linecap="round" opacity="${op}"/>`;
}

// terminalCapSVG marks an ended thread that is the north (top) terminus of its
// lane — the topmost station, with no mainline continuing past it. Real metro
// lines end in a straight buffer stop, so this draws a short vertical stem
// straight up from the station (12 o'clock) capped by a horizontal buffer bar:
// a "T" sitting on top of the dot. Because nothing runs north through it, the
// crossbar is never crossed (the very failure mode that retired the old
// mid-lane crossbar). col is the already-escaped lane colour.
export function terminalCapSVG(x, y, op, col){
  return `<line x1="${x}" y1="${y}" x2="${x}" y2="${y - 13}" stroke="${col}" stroke-width="2.5" stroke-linecap="round" opacity="${op}"/>
          <line x1="${x - 9}" y1="${y - 13}" x2="${x + 9}" y2="${y - 13}" stroke="${col}" stroke-width="3" stroke-linecap="round" opacity="${op}"/>`;
}

export function forkCueHTML(n, lm, { escape = esc, forkKind, nodeById } = {}){
  const k = forkKind ? forkKind(n) : null;
  if (!k) return "";
  const p = nodeById ? nodeById(n.parent) : null;
  if (!p) return "";
  const col = escape(lm.color(k === "y-new" ? n.lane_id : p.lane_id));
  return `<span class="forkcue" data-goorigin="${escape(p.id)}" style="color:${col}">from ${escape(p.title)}</span>`;
}

/* Compact token count for the fare meter (32.8k / 1.2M). Pure; tests pin
   label presence, not exact glyphs. */
export function fmtFareTokens(n){
  const v = Number(n);
  if (!Number.isFinite(v)) return "0";
  const a = Math.abs(v);
  if (a >= 1_000_000){
    const s = (v / 1_000_000).toFixed(1);
    return s.replace(/\.0$/, "") + "M";
  }
  if (a >= 1000){
    const s = (v / 1000).toFixed(1);
    return s.replace(/\.0$/, "") + "k";
  }
  return String(Math.round(v));
}

function fmtFareCost(c){
  const v = Number(c);
  if (!Number.isFinite(v)) return "";
  return "$" + v.toFixed(2);
}

/* Journey token meter (fare-design.md Phase 8 / D1 / D8). Full-screen + fareOn
   only. Distinct from the ctx_pct tank ring on the wall SVG — never merges
   occupancy into this line. Cost only when fare_cost_complete (D8). Absent
   fare_* fields → empty (never 0·0·0). */
export function fareLineHTML(n, { mapFull, fareOn, escape = esc } = {}){
  if (!mapFull || !fareOn || !n) return "";
  const hasFare = n.fare_fresh_in != null || n.fare_out != null
    || n.fare_cache_write != null || n.fare_cache_read != null
    || n.fare_total != null || n.fare_turns != null;
  if (!hasFare) return "";
  const parts = [
    `fresh ${fmtFareTokens(n.fare_fresh_in ?? 0)}`,
    `out ${fmtFareTokens(n.fare_out ?? 0)}`,
    `cache-wr ${fmtFareTokens(n.fare_cache_write ?? 0)}`,
    `cache-rd ${fmtFareTokens(n.fare_cache_read ?? 0)}`,
  ];
  if (n.fare_model) parts.push(escape(n.fare_model));
  if (n.fare_turns != null) parts.push(`${Number(n.fare_turns)} turns`);
  // D8: reported-or-omitted — no $, no number, no estimate unless complete
  if (n.fare_cost_complete && n.fare_cost != null)
    parts.push(fmtFareCost(n.fare_cost));
  return `<div class="fare">${parts.join(" · ")}</div>`;
}

export function stationRowHTML(n, lm, opts = {}){
  const {
    padLeft = 0, others = [], dim = false, golane = false, current = false,
    fork = false, stop = null, alt = false,
    escape = esc, stamp = fmtStamp, status = statusText, configText = cardConfigText,
    fareHTML = "", forkCue = "",
  } = opts;
  const label = stop ? stopLabel(stop) : { title: n.title || "", desc: n.description || n.prompt || "" };
  if (stop && !stop.head)
    return `
        <div class="strow stoprow ${alt ? "alt " : ""}${dim ? "dimmed" : ""}${current ? " current" : ""}" style="padding-left:${padLeft}px" data-nid="${escape(n.id)}" data-skey="${escape(stopKey(stop))}" data-stop="${escape(stop.time)}">
          <div class="lbl"><span class="t">${escape(label.title)}</span></div>
          <div class="cap">${escape(stamp(stop.time))} &middot; earlier stop &middot; tap to read</div>
          ${label.desc ? `<div class="desc">${escape(label.desc)}</div>` : ""}
        </div>`;
  const when = stop && stop.i > 0
    ? `${stamp(stop.time)} · stop ${stop.i + 1}` : stamp(n.created_at);
  const desc = label.desc;
  return `
        <div class="strow ${alt ? "alt " : ""}${dim ? "dimmed" : ""} ${n.live === "exited" ? "dead" : ""}${current ? " current" : ""}"
             style="padding-left:${padLeft}px" data-nid="${escape(n.id)}"${stop ? ` data-skey="${escape(stopKey(stop))}"` : ""}>
          <div class="lbl"><span class="agent-logo" title="${escape(n.agent || "agent")}">${opts.agentLogo || ""}</span><span class="t">${escape(label.title)}</span>${fork ? forkCue : ""}</div>
          <div class="cap">${escape(when)} · ${escape(configText(n))} · ${escape(status(n))}${
            others.map(l => ` · <span class="xchip"${golane ? ` data-golane="${escape(l)}"` : ""} style="color:${escape(lm.color(l))}">&#8644; ${escape(lm.name(l))}</span>`).join("")}</div>
          ${desc ? `<div class="desc">${escape(desc)}</div>` : ""}
          ${fareHTML}
        </div>`;
}

/* ---------- feature factory ---------- */

export function createMapFeature(deps){
  const d = deps || {};
  const roots = d.roots || {};
  const maptabs = roots.maptabs;
  const lanechips = roots.lanechips;
  const mapwrap = roots.mapwrap;
  const mapscroll = roots.mapscroll;
  const maptoolbar = roots.maptoolbar;
  const mapfullbtn = roots.mapfullbtn;
  const farebtn = roots.farebtn;
  const mapEl = roots.map;
  const tabHead = roots.tabHead;
  const tabName = roots.tabName;
  const tabLanes = roots.tabLanes;
  const tabSave = roots.tabSave;
  const tabDel = roots.tabDel;
  const doc = d.document || (typeof document !== "undefined" ? document : null);
  const win = d.window || (typeof window !== "undefined" ? window : null);
  const CSSRef = d.CSS || (typeof CSS !== "undefined" ? CSS : { escape: s => String(s) });
  const storage = d.storage || (typeof localStorage !== "undefined" ? localStorage : null);
  const escape = d.esc || esc;
  const stampFn = d.fmtStamp || fmtStamp;
  const contrastFn = d.contrastText || contrastTextPure;

  let mapTab = (storage && storage.getItem(MAP_TAB_KEY)) || "all";
  let mapFold = new Set(JSON.parse((storage && storage.getItem(MAP_FOLD_KEY)) || "[]"));
  let mapFoldKnown = new Set(JSON.parse((storage && storage.getItem(MAP_FOLD_KNOWN_KEY)) || "[]"));
  let mapFull = !!(storage && storage.getItem(MAP_FULL_KEY) === "1");
  /* fare overlay: restored from localStorage; the control itself is CSS-hidden
     until body.map-full, so a persisted-on flag never shows a button where the
     overlay can't render. */
  let fareOn = !!(storage && storage.getItem(MAP_FARE_KEY) === "1");
  let mapSel = "";
  let mapSelKey = "";
  let mapSelStop = "";
  let mapSig = "";
  let editTab = null;
  let bound = false;
  const cleanups = [];

  function g(name, fallback){
    const v = d[name];
    return typeof v === "function" ? v() : (v !== undefined ? v : fallback);
  }
  function nodeById(id){
    if (typeof d.nodeById === "function") return d.nodeById(id);
    return (g("nodes", []) || []).find(n => n.id === id);
  }
  function lm(){
    if (typeof d.laneModel === "function") return d.laneModel();
    return { lanes: [], color: () => "", name: id => id, byId: {} };
  }
  function forkKind(n){ return forkKindMod(n, g("nodes", [])); }
  function groups(){ return g("groups", []) || []; }
  function laneFilter(){ return g("laneFilter", "") || ""; }
  function uiLoaded(){ return !!g("uiLoaded", false); }
  function isDesktop(){ return typeof d.isDesktop === "function" ? d.isDesktop() : false; }
  function mapOpen(){
    if (typeof d.mapOpen === "function") return !!d.mapOpen();
    return !!(doc && doc.body && doc.body.classList.contains("map-open"));
  }
  function level(){ return g("level", 1); }
  function storeSet(key, val){ if (storage) storage.setItem(key, val); }
  function storeRemove(key){ if (storage) storage.removeItem(key); }

  function saveMapFold(){
    /* new journeys default folded (mapFoldKnown tracks every lane ever seen;
       mapFold records only the user's explicit open/closed choices). Prune lanes
       that no longer exist so a recreated id starts folded again. */
    if (uiLoaded() && (g("nodes", []) || []).length){
      const live = new Set(lm().lanes.map(l => l.id));
      const next = applyNewLaneFolds(mapFold, mapFoldKnown, live);
      mapFold = next.mapFold;
      mapFoldKnown = next.mapFoldKnown;
    }
    storeSet(MAP_FOLD_KEY, JSON.stringify([...mapFold]));
    storeSet(MAP_FOLD_KNOWN_KEY, JSON.stringify([...mapFoldKnown]));
  }

  function setMapSel(id, key, stopTime){
    const next = toggleMapSelection({ id, key, stopTime, mapSelKey, nodes: g("nodes", []) });
    mapSel = next.mapSel;
    mapSelKey = next.mapSelKey;
    mapSelStop = next.mapSelStop;
    mapSig = "";
    renderMap();
  }

  function syncMapFullBtn(){
    const b = mapfullbtn || (doc && doc.querySelector && doc.querySelector("#mapfullbtn"));
    if (b){
      const icons = d.icons || {};
      /* Distinct out (expand) vs in (contract) FA solids — not a rotated twin. */
      b.innerHTML = mapFull
        ? (icons.ICON_MAP_CONTRACT || "")
        : (icons.ICON_MAP_EXPAND || "");
      const label = mapFull ? "exit full-screen map" : "full-screen map";
      if (typeof b.setAttribute === "function") b.setAttribute("aria-label", label);
      else b["aria-label"] = label;
    }
  }

  function setMapFull(on){
    mapFull = !!on;
    if (mapFull) storeSet(MAP_FULL_KEY, "1");
    else storeRemove(MAP_FULL_KEY);
    const cleared = clearMapSelection();
    mapSel = cleared.mapSel; mapSelKey = cleared.mapSelKey; mapSelStop = cleared.mapSelStop;
    if (doc && doc.body) doc.body.classList.toggle("map-full", mapFull);
    syncMapFullBtn();
    mapSig = "";
    renderMap();
    renderMapToolbar();
  }

  function selectedStop(){
    return selectedStopOf(g("nodes", []), mapSel, mapSelStop);
  }

  function mapVisible(){
    return mapIsVisible({ isDesktop: isDesktop(), mapFull, mapOpen: mapOpen(), level: level() });
  }

  function renderMapTabs(){
    mapTab = resolveMapTab(mapTab, groups(), uiLoaded());
    if (!maptabs) return;
    maptabs.innerHTML = mapTabsHTML(mapTab, groups(), { escape });
  }

  function renderLaneChips(model, inGroup, focusLane){
    if (!lanechips) return;
    lanechips.innerHTML = model.lanes.filter(l => inGroup(l.id)).map(l => `
    <button class="lanechip ${focusLane === l.id ? "selected" : ""}"
          style="${laneChipStyle(model.color(l.id), focusLane === l.id, { escape, contrastText: contrastFn })}"
          data-chip="${escape(l.id)}">${escape(l.name || l.id)}</button>`).join("");
  }

  function stationHTML(n, model, opts){
    const fareHTML = fareLineHTML(n, { mapFull, fareOn, escape });
    const forkCue = opts.fork ? forkCueHTML(n, model, {
      escape, forkKind, nodeById,
    }) : "";
    const agentLogo = typeof d.agentLogo === "function" ? d.agentLogo(n.agent) : "";
    return stationRowHTML(n, model, {
      ...opts,
      escape, stamp: stampFn, status: statusText, configText: cardConfigText,
      fareHTML, forkCue, agentLogo,
    });
  }

  function positionMapToolbar(){
    const bar = maptoolbar;
    if (!bar || !bar.classList.contains("on")) return;
    const wrap = mapwrap;
    const scroll = mapscroll;
    const mapRoot = mapEl || (doc && doc.querySelector && doc.querySelector("#map"));
    if (!wrap || !scroll){ bar.style.display = "none"; return; }
    const row = wrap.querySelector(`.strow[data-skey="${CSSRef.escape(mapSelKey)}"]`)
      || wrap.querySelector(`.strow[data-nid="${CSSRef.escape(mapSel)}"]`);
    if (!row || !mapRoot){ bar.style.display = "none"; return; }
    const rr = row.getBoundingClientRect(), sr = scroll.getBoundingClientRect(), mr = mapRoot.getBoundingClientRect();
    if (rr.bottom < sr.top + 8 || rr.top > sr.bottom - 8){ bar.style.display = "none"; return; }
    bar.style.display = "flex";
    const padLeft = parseFloat((d.getComputedStyle || (typeof getComputedStyle !== "undefined" ? getComputedStyle : () => ({ paddingLeft: "0" })))(row).paddingLeft) || 0;
    let top = rr.bottom - mr.top - 4;
    const maxTop = sr.bottom - mr.top - bar.offsetHeight - 4;
    if (top > maxTop) top = rr.top - mr.top - bar.offsetHeight + 4;
    bar.style.top = Math.max(4, top) + "px";
    bar.style.left = Math.max(8, rr.left - mr.left + padLeft) + "px";
  }

  function renderMapToolbar(){
    const bar = maptoolbar;
    if (!bar) return;
    const n = (mapFull && mapSel) ? nodeById(mapSel) : null;
    bar.classList.toggle("on", !!n);
    if (!n){ bar.innerHTML = ""; bar.style.display = ""; return; }
    const ss = selectedStop();
    const label = ss ? stopLabel(ss) : { title: n.title || "" };
    const icons = d.icons || {};
    const laneColor = typeof d.laneColor === "function" ? d.laneColor(n.lane_id) : "";
    bar.innerHTML = `
    <span class="mtb-node">
      <span class="mtb-swatch" style="background:${escape(laneColor)}"></span>
      <span class="mtb-title">${escape(label.title)}</span>
      ${mapSelStop ? `<span class="mtb-stopcue">earlier stop &middot; ${escape(stampFn(mapSelStop))}</span>` : ""}
    </span>
    <span class="mtb-sep"></span>
    <button class="sbtn" data-jump="${escape(n.id)}">&#8249; Open chat</button>
    <button class="sbtn iconbtn" data-medit="${escape(n.id)}" aria-label="edit title & description">${icons.ICON_PENCIL || ""} Edit</button>
    <button class="sbtn" data-forkbookmark="${escape(n.id)}">Add bookmark</button>
    <button class="sbtn" data-forkfrom="${escape(n.id)}">+ New</button>
    ${n.ended_at ? "" : `<button class="sbtn iconbtn" data-mapexit="${escape(n.id)}" aria-label="close thread">${icons.ICON_END || ""} End</button>`}`;
    positionMapToolbar();
  }

  function renderWallMap(model, grp, inGroup, served, stations, focusLane){
    const colLanes = model.lanes.filter(l => inGroup(l.id));
    const cols = laneColumnOrder(colLanes, stations, model.byId);
    const colIdx = {}; cols.forEach((id, c) => colIdx[id] = c);
    const rows = stations.flatMap(stopsOf).sort(newestFirst);

    const sig = wallMapSignature(rows, cols, {
      focusLane, mapTab, mapSel, mapSelKey, fareOn,
    });
    if (mapRenderDecision(sig, mapSig) === "skip") return;
    mapSig = sig;

    const RH = 76, CW = 46, MX = 24, OFF = 16, GAP = 16;
    const nCols = cols.length, nRows = rows.length;
    const colX = c => MX + c * CW;
    const rowY = i => OFF + i * RH + RH / 2;
    let colsW = MX + Math.max(0, nCols - 1) * CW + MX;
    const svgH = OFF + nRows * RH;
    const dimLane = id => focusLane && id !== focusLane;
    const dimNode = n => focusLane && n.lane_id !== focusLane;

    if (!nRows){
      if (mapwrap) mapwrap.innerHTML = `<div class="empty">No journeys${grp ? " on this map" : ""} yet.</div>`;
      renderLaneChips(model, inGroup, focusLane);
      return;
    }

    const rowIdx = {}; rows.forEach((s, i) => rowIdx[stopKey(s)] = i);
    const BR = 13;
    const xOff = {};
    {
      const slots = {};
      stations.slice().sort((a, b) => stampMS(a.created_at) - stampMS(b.created_at))
        .forEach(n => {
          const p = n.parent && model.byId[n.parent];
          if (p && n.lane_id === p.lane_id && colIdx[n.lane_id] != null){
            slots[n.lane_id] = (slots[n.lane_id] || 0) + 1;
            xOff[n.id] = slots[n.lane_id] * BR;
          }
        });
    }
    const nodeX = n => colX(colIdx[n.lane_id]) + (xOff[n.id] || 0);
    colsW += Math.max(0, ...Object.values(xOff));

    const touch = {}; cols.forEach(id => touch[id] = []);
    const branchYs = {};
    rows.forEach((s, i) => {
      if (colIdx[s.n.lane_id] == null) return;
      if (xOff[s.n.id]) (branchYs[s.n.id] = branchYs[s.n.id] || []).push(rowY(i));
      else touch[s.n.lane_id].push(rowY(i));
    });

    let svg = ATTN_GLOW_DEF;
    cols.forEach(id => {
      const ys = touch[id];
      if (ys.length < 2) return;
      const x = colX(colIdx[id]);
      svg += `<line x1="${x}" y1="${Math.min(...ys)}" x2="${x}" y2="${Math.max(...ys)}"
            stroke="${escape(model.color(id))}" stroke-width="3.5" stroke-linecap="round"
            opacity="${dimLane(id) ? .22 : 1}"/>`;
    });
    Object.keys(branchYs).forEach(id => {
      const ys = branchYs[id], n = model.byId[id];
      if (ys.length < 2 || !n) return;
      const x = nodeX(n);
      svg += `<line x1="${x}" y1="${Math.min(...ys)}" x2="${x}" y2="${Math.max(...ys)}"
            stroke="${escape(model.color(n.lane_id))}" stroke-width="3.5" stroke-linecap="round"
            opacity="${dimNode(n) ? .22 : 1}"/>`;
    });
    rows.forEach((s, ci) => {
      if (s.i !== 0) return;
      const n = s.n;
      const p = n.parent && model.byId[n.parent];
      if (!p) return;
      const cc = colIdx[n.lane_id], cp = colIdx[p.lane_id],
        pi = rowIdx[p.id + "#" + parentStopIndex(p, n.created_at)];
      if (cc == null || cp == null || pi == null) return;
      const xC = nodeX(n), yC = rowY(ci), xP = nodeX(p), yP = rowY(pi);
      const ym = (yP + yC) / 2, op = dimNode(n) ? .25 : 1;
      if (xC === xP){
        const col = escape(model.color(n.lane_id)), bx = xC + 13;
        svg += `<path d="M ${xC} ${yP} C ${bx} ${ym} ${bx} ${ym} ${xC} ${yC}"
              fill="none" stroke="${col}" stroke-width="2.5" opacity="${op}"/>`;
      } else {
        const col = escape(model.color(forkKind(n) === "y-new" ? n.lane_id : p.lane_id));
        svg += `<path d="M ${xP} ${yP} C ${xP} ${ym} ${xC} ${ym} ${xC} ${yC}"
              fill="none" stroke="${col}" stroke-width="2.5" opacity="${op}"/>`;
      }
    });
    // North (top) terminus per *drawn line*: the smallest row index (rows are
    // newest-first, so index 0 is topmost) among stations sharing an x. Keying
    // by nodeX, not lane_id, keeps each y-stay branch spur (its own x offset)
    // separate from the lane's main column, so a branch that dead-ends is
    // recognised as its own line-end and gets the straight buffer cap.
    const lineTop = {};
    rows.forEach((s, i) => {
      if (colIdx[s.n.lane_id] == null) return;
      const kx = nodeX(s.n);
      if (lineTop[kx] == null) lineTop[kx] = i;
    });
    rows.forEach((s, i) => {
      const n = s.n;
      const a = n.lane_id; if (colIdx[a] == null) return;
      const yy = rowY(i), col = escape(model.color(n.lane_id));
      const op = dimNode(n) ? .25 : 1;
      let dotX = nodeX(n);
      if (!s.head){
        svg += `<circle cx="${dotX}" cy="${yy}" r="4" fill="${col}" opacity="${op * .8}"/>`;
        return;
      }
      if (n.ended_at)
        svg += i === lineTop[dotX] ? terminalCapSVG(dotX, yy, op, col)
                                   : terminalStationSVG(dotX, yy, op, col);
      if (n.attention) svg += attentionStationSVG(dotX, yy, op);
      if (n.live === "exited" || n.live === "unavailable")
        svg += `<circle cx="${dotX}" cy="${yy}" r="5.5" fill="var(--bg)" stroke="${col}" stroke-width="2.5" opacity="${op * .55}"/>`;
      else
        svg += `<circle cx="${dotX}" cy="${yy}" r="${n.live === "active" ? 6.5 : 5.5}" fill="${col}" opacity="${op}"/>`;
      if (n.ctx_pct != null && !n.attention){
        const R = 9, C = 2 * Math.PI * R, frac = Math.max(0, Math.min(1, n.ctx_pct / 100));
        svg += `<circle cx="${dotX}" cy="${yy}" r="${R}" fill="none" stroke="${col}" stroke-width="2" opacity="${op * .2}"/>`;
        if (frac > 0)
          svg += `<circle cx="${dotX}" cy="${yy}" r="${R}" fill="none"
                stroke="${n.ctx_pct >= 80 ? "var(--attn)" : col}" stroke-width="2"
                stroke-dasharray="${frac * C} ${C}" stroke-linecap="round"
                transform="rotate(-90 ${dotX} ${yy})" opacity="${op}"/>`;
      }
    });

    if (mapwrap) mapwrap.innerHTML = `<div class="lbody wallbody">
    <svg width="${colsW}" height="${svgH}" viewBox="0 0 ${colsW} ${svgH}">${svg}</svg>
    <div style="height:${OFF}px"></div>
    ${rows.map((s, i) => stationHTML(s.n, model, {
      padLeft: colsW + GAP,
      others: [],
      dim: dimNode(s.n),
      golane: false,
      current: stopKey(s) === mapSelKey,
      fork: true,
      stop: s,
      alt: i % 2 === 1,
    })).join("")}
  </div>`;
    renderLaneChips(model, inGroup, focusLane);
    renderMapToolbar();
  }

  function renderMap(){
    if (!mapVisible()) return;
    const model = lm();
    const grp = groupLanes(mapTab, groups());
    const inGroup = laneID => !grp || grp.includes(laneID);
    const served = servedLanes;
    const stations = (g("nodes", []) || []).filter(n => n.lane_id && served(n).some(inGroup));
    const focusLane = laneFilter() || null;
    if (mapFull && isDesktop()) return renderWallMap(model, grp, inGroup, served, stations, focusLane);

    let blocks = buildStackBlocks(model.lanes, stations, { served, inGroup });
    saveMapFold();
    if (focusLane && mapFold.has(focusLane)){
      mapFold = unfoldFocusLane(mapFold, focusLane);
      saveMapFold();
    }

    const sig = stackMapSignature(blocks, {
      mapFold, focusLane, mapTab, mapFull, lm: model,
    });
    if (mapRenderDecision(sig, mapSig) === "skip") return;
    mapSig = sig;

    const RH = 76, LX = 22, LW = 56;
    const y = i => i * RH + RH / 2;
    const dimRow = n => focusLane && !served(n).includes(focusLane);
    const hasAttn = b => b.rows.some(s => s.n.attention) ? 1 : 0;

    if (mapwrap) mapwrap.innerHTML = blocks.map(b => {
      const col = escape(model.color(b.lane.id));
      const folded = !mapFull && mapFold.has(b.lane.id);
      const dimBlock = focusLane && b.lane.id !== focusLane;
      const head = `
      <button class="lhead ${dimBlock ? "dimmed" : ""}" ${mapFull ? "" : `data-fold="${escape(b.lane.id)}"`}>
        ${mapFull ? "" : `<span class="chev">${folded ? "&#9656;" : "&#9662;"}</span>`}
        <span class="lswatch" style="background:${col}"></span>
        <span class="lname">${escape(b.lane.name || b.lane.id)}</span>
        <span class="lcount">${new Set(b.rows.map(s => s.n.id)).size}</span>
        ${hasAttn(b) ? `<span class="lattn"></span>` : ""}
      </button>`;
      if (folded) return `<div class="lblock" data-lane="${escape(b.lane.id)}">${head}</div>`;

      let svg = "";
      if (b.rows.length > 1)
        svg += `<line x1="${LX}" y1="${y(0)}" x2="${LX}" y2="${y(b.rows.length - 1)}"
              stroke="${col}" stroke-width="3.5" stroke-linecap="round" opacity="${dimBlock ? .22 : 1}"/>`;
      b.rows.forEach((s, i) => {
        const n = s.n;
        const yy = y(i);
        const op = dimRow(n) ? .25 : 1;
        const dotX = LX;
        const fk = s.i === 0 && forkKind(n);
        if (fk){
          const fcol = escape(model.color(fk === "y-new" ? n.lane_id : (nodeById(n.parent)?.lane_id || b.lane.id)));
          svg += `<path d="M ${dotX} ${yy} q 9 -2 14 -9" fill="none" stroke="${fcol}" stroke-width="2.5" stroke-linecap="round" opacity="${op}"/>`;
        }
        if (!s.head){
          svg += `<circle cx="${dotX}" cy="${yy}" r="4" fill="${col}" opacity="${op * .8}"/>`;
          return;
        }
        if (n.ended_at)
          svg += i === 0 ? terminalCapSVG(dotX, yy, op, col)
                         : terminalStationSVG(dotX, yy, op, col);
        if (n.attention) svg += attentionStationSVG(dotX, yy, op);
        if (n.live === "exited" || n.live === "unavailable")
          svg += `<circle cx="${dotX}" cy="${yy}" r="5.5" fill="var(--bg)" stroke="${col}" stroke-width="2.5" opacity="${op * .55}"/>`;
        else
          svg += `<circle cx="${dotX}" cy="${yy}" r="${n.live === "active" ? 6.5 : 5.5}" fill="${col}" opacity="${op}"/>`;
      });

      return `<div class="lblock" data-lane="${escape(b.lane.id)}">${head}<div class="lbody">
      <svg width="${LW}" height="${b.rows.length * RH}" viewBox="0 0 ${LW} ${b.rows.length * RH}">${ATTN_GLOW_DEF}${svg}</svg>
      ${b.rows.map((s, i) => stationHTML(s.n, model, {
        padLeft: LW + 12,
        others: [],
        dim: dimRow(s.n),
        golane: true,
        fork: true,
        stop: s,
        alt: i % 2 === 1,
      })).join("")}
    </div></div>`;
    }).join("") || `<div class="empty">No journeys${grp ? " on this map" : ""} yet.</div>`;

    renderLaneChips(model, inGroup, focusLane);
  }

  function removeMapTab(id){
    const i = groups().findIndex(t => t.id === id);
    if (i < 0) return;
    const nextGroups = groups().filter(t => t.id !== id);
    if (mapTab === id) mapTab = "all";
    storeSet(MAP_TAB_KEY, mapTab);
    if (typeof d.uiMutate === "function") d.uiMutate({ k: "groups", groups: nextGroups });
    renderMapTabs(); mapSig = ""; renderMap();
  }

  function openTabSheet(t){
    editTab = t || null;
    if (tabHead) tabHead.textContent = t ? "Edit map" : "New map";
    if (tabName) tabName.value = t ? t.name : "";
    const model = lm();
    if (tabLanes) tabLanes.innerHTML = model.lanes.map(l => `
    <label style="display:flex;gap:10px;align-items:center;margin:8px 0;font-size:15px">
      <input type="checkbox" value="${escape(l.id)}" style="width:auto;margin:0"
        ${t && t.lanes.includes(l.id) ? "checked" : ""}>
      <span style="width:12px;height:12px;border-radius:6px;flex:none;background:${escape(model.color(l.id))}"></span>
      ${escape(l.name || l.id)}</label>`).join("")
      || `<div style="color:var(--dim);font-size:14px">no journeys yet — an empty map is fine</div>`;
    if (tabSave) tabSave.textContent = t ? "Save map" : "Add map";
    if (tabDel) tabDel.style.display = t ? "block" : "none";
    if (typeof d.openSheet === "function") d.openSheet("#tabsheet");
  }

  function toggleMapFold(id){
    mapFold = toggleFoldId(mapFold, id);
    saveMapFold();
    mapSig = "";
    renderMap();
  }

  function gotoStation(id){
    const n = nodeById(id);
    if (!n) return;
    if (mapTab !== "all" &&
        !(mapwrap && mapwrap.querySelector(`.strow[data-nid="${CSSRef.escape(id)}"]`))){
      mapTab = "all";
      storeSet(MAP_TAB_KEY, mapTab);
      mapSig = ""; renderMapTabs(); renderMap();
    }
    if (n.lane_id && mapFold.has(n.lane_id)){
      mapFold = unfoldFocusLane(mapFold, n.lane_id);
      saveMapFold(); mapSig = ""; renderMap();
    }
    const row = mapwrap && mapwrap.querySelector(`.strow[data-nid="${CSSRef.escape(id)}"]`);
    if (row){
      row.scrollIntoView({ behavior: "smooth", block: "center" });
      row.classList.add("originflash");
      const t = d.setTimeout || setTimeout;
      t(() => row.classList.remove("originflash"), 1200);
    }
  }

  function gotoLaneBlock(id){
    if (mapTab !== "all" &&
        !(mapwrap && mapwrap.querySelector(`.lblock[data-lane="${CSSRef.escape(id)}"]`))){
      mapTab = "all";
      storeSet(MAP_TAB_KEY, mapTab);
      mapSig = ""; renderMapTabs(); renderMap();
    }
    if (mapFold.has(id)){
      mapFold = unfoldFocusLane(mapFold, id);
      saveMapFold(); mapSig = ""; renderMap();
    }
    mapwrap && mapwrap.querySelector(`.lblock[data-lane="${CSSRef.escape(id)}"]`)
      ?.scrollIntoView({ behavior: "smooth", block: "start" });
  }

  /* ----- event handlers ----- */

  function onMapTabsClick(e){
    const rm = e.target.closest("[data-rm]");
    if (rm){ e.stopPropagation(); removeMapTab(rm.dataset.rm); return; }
    const b = e.target.closest("[data-mt]");
    if (!b) return;
    if (b.dataset.mt === "+"){ openTabSheet(null); return; }
    if (b.dataset.mt === mapTab && mapTab !== "all"){
      openTabSheet(groups().find(t => t.id === mapTab));
      return;
    }
    mapTab = b.dataset.mt;
    storeSet(MAP_TAB_KEY, mapTab);
    renderMapTabs(); mapSig = ""; renderMap();
  }

  function onTabSave(){
    const name = (tabName && tabName.value.trim()) || "";
    if (!name) return;
    const checked = tabLanes
      ? [...tabLanes.querySelectorAll("input:checked")].map(i => i.value)
      : [];
    if (editTab){ editTab.name = name; editTab.lanes = checked; }
    else {
      const id = "g" + Date.now();
      const next = [...groups(), { id, name, lanes: checked }];
      if (typeof d.uiMutate === "function") d.uiMutate({ k: "groups", groups: next });
      mapTab = id;
      storeSet(MAP_TAB_KEY, mapTab);
      renderMapTabs(); mapSig = ""; renderMap();
      if (typeof d.closeSheets === "function") d.closeSheets();
      return;
    }
    if (typeof d.uiMutate === "function") d.uiMutate({ k: "groups", groups: groups() });
    renderMapTabs(); mapSig = ""; renderMap();
    if (typeof d.closeSheets === "function") d.closeSheets();
  }

  function onTabDel(){
    if (editTab) removeMapTab(editTab.id);
    if (typeof d.closeSheets === "function") d.closeSheets();
  }

  function onLaneChipsClick(e){
    const c = e.target.closest("[data-chip]");
    if (!c) return;
    const id = c.dataset.chip;
    const plan = chipTapPlan({ laneFilter: laneFilter(), id, mapFold });
    mapFold = plan.mapFold;
    saveMapFold();
    if (plan.kind === "clear-scope"){
      if (typeof d.setLaneFilter === "function") d.setLaneFilter("");
      return;
    }
    const nodes = g("nodes", []) || [];
    const first = [...nodes]
      .filter(n => n.lane_id === id)
      .sort((a, b) => (b.created_at || "").localeCompare(a.created_at || ""))[0];
    if (first && typeof d.selectNode === "function") d.selectNode(first.id);
    if (typeof d.setLaneFilter === "function") d.setLaneFilter(id);
    mapwrap && mapwrap.querySelector(`.lblock[data-lane="${CSSRef.escape(id)}"]`)
      ?.scrollIntoView({ behavior: "smooth", block: "start" });
  }

  function onMapWrapClick(e){
    const f = e.target.closest("[data-fold]");
    if (f){ toggleMapFold(f.dataset.fold); return; }
    const j = e.target.closest("[data-jump]");
    if (j){
      if (typeof d.selectNode === "function") d.selectNode(j.dataset.jump);
      setMapFull(false);
      return;
    }
    const pe = e.target.closest("[data-medit]");
    if (pe){
      if (typeof d.openActivityEditor === "function") d.openActivityEditor(pe.dataset.medit);
      return;
    }
    const fk = e.target.closest("[data-forkfrom]");
    if (fk){
      if (typeof d.forkFromStation === "function") d.forkFromStation(fk.dataset.forkfrom);
      return;
    }
    const gl = e.target.closest("[data-golane]");
    if (gl){ gotoLaneBlock(gl.dataset.golane); return; }
    const go = e.target.closest("[data-goorigin]");
    if (go){ gotoStation(go.dataset.goorigin); return; }
    const r = e.target.closest("[data-nid]");
    if (r){
      const st = r.dataset.stop || "";
      if (mapFull){ setMapSel(r.dataset.nid, r.dataset.skey || "", st); }
      else {
        if (typeof d.selectNode === "function") d.selectNode(r.dataset.nid);
        if (st){
          if (typeof d.loadChatHistory === "function") d.loadChatHistory(r.dataset.nid, st);
        } else if (typeof d.jumpChatToNow === "function") d.jumpChatToNow();
        if (!isDesktop() && typeof d.setLevel === "function") d.setLevel(1);
      }
    }
  }

  function onToolbarClick(e){
    const j = e.target.closest("[data-jump]");
    if (j){
      const st = mapSelStop;
      if (typeof d.selectNode === "function") d.selectNode(j.dataset.jump);
      if (st){
        if (typeof d.loadChatHistory === "function") d.loadChatHistory(j.dataset.jump, st);
      } else if (typeof d.jumpChatToNow === "function") d.jumpChatToNow();
      setMapFull(false);
      return;
    }
    const pe = e.target.closest("[data-medit]");
    if (pe){
      if (typeof d.openActivityEditor === "function") d.openActivityEditor(pe.dataset.medit, mapSelStop || "");
      return;
    }
    const nt = e.target.closest("[data-forkbookmark]");
    if (nt){
      if (typeof d.addStationBookmark === "function") d.addStationBookmark(nt.dataset.forkbookmark);
      return;
    }
    const fk = e.target.closest("[data-forkfrom]");
    if (fk){
      if (typeof d.forkFromStation === "function") d.forkFromStation(fk.dataset.forkfrom);
      return;
    }
    const ex = e.target.closest("[data-mapexit]");
    if (ex){
      if (typeof d.exitThread === "function") d.exitThread(ex.dataset.mapexit);
      return;
    }
  }

  function onMapFullClick(){ setMapFull(!mapFull); }

  function syncFareBtn(){
    if (!farebtn) return;
    const icons = d.icons || {};
    // Inlined SVG glyphs (no FA webfont classes — TestPinnedIcons). On/off
    // state is .on + aria-pressed + data-fare marker on the glyph.
    farebtn.innerHTML = fareOn
      ? (icons.ICON_FARE_ON || "")
      : (icons.ICON_FARE_OFF || "");
    farebtn.classList.toggle("on", fareOn);
    if (typeof farebtn.setAttribute === "function"){
      farebtn.setAttribute("aria-pressed", fareOn ? "true" : "false");
      farebtn.setAttribute("aria-label", "toggle fare overlay");
    }
  }

  function onFareClick(){
    fareOn = !fareOn;
    if (fareOn) storeSet(MAP_FARE_KEY, "1");
    else storeRemove(MAP_FARE_KEY);
    syncFareBtn();
    mapSig = "";
    renderMap();
  }

  function onLaneChipLongpress(el){
    const id = el.dataset.chip;
    const lane = typeof d.laneById === "function" ? d.laneById(id) : null;
    if (!lane) return;
    const prompt = d.prompt || ((typeof globalThis !== "undefined" && globalThis.prompt) || (() => null));
    const name = prompt("Rename lane", lane.name || id);
    if (name == null) return;
    const clean = name.trim();
    if (!clean || clean === lane.name) return;
    const lanes = (typeof d.laneList === "function" ? d.laneList() : []).map(l =>
      l.id === id ? { ...l, name: clean } : l);
    if (typeof d.uiMutate === "function") d.uiMutate({ k: "lanes", lanes });
    mapSig = "";
    if (typeof d.invalidateCardsSig === "function") d.invalidateCardsSig();
    if (typeof d.renderCards === "function") d.renderCards();
    renderMapTabs();
    renderMap();
  }

  function onStationLongpress(el, ev){
    if (ev?.target?.closest("button, a, input")) return;
    const id = el.dataset.nid;
    if (!id) return;
    const stop = el.dataset.stop || "";
    if (mapFull){
      const key = el.dataset.skey || headStopKey(id, g("nodes", []));
      if (mapSelKey !== key){
        mapSel = id; mapSelKey = key; mapSelStop = stop;
        mapSig = ""; renderMap();
      }
    }
    if (typeof d.openActivityEditor === "function") d.openActivityEditor(id, stop);
  }

  function bind(){
    if (bound) return;
    bound = true;
    const on = (el, event, handler, options) => {
      if (!el || typeof el.addEventListener !== "function") return;
      el.addEventListener(event, handler, options);
      cleanups.push(() => el.removeEventListener(event, handler, options));
    };
    on(maptabs, "click", onMapTabsClick);
    on(lanechips, "click", onLaneChipsClick);
    on(mapwrap, "click", onMapWrapClick);
    on(maptoolbar, "click", onToolbarClick);
    on(mapscroll, "scroll", positionMapToolbar, { passive: true });
    if (win && typeof win.addEventListener === "function"){
      win.addEventListener("resize", positionMapToolbar);
      cleanups.push(() => win.removeEventListener("resize", positionMapToolbar));
    }
    on(mapfullbtn, "click", onMapFullClick);
    on(farebtn, "click", onFareClick);
    on(tabSave, "click", onTabSave);
    on(tabDel, "click", onTabDel);
    if (typeof d.longpress === "function"){
      if (lanechips){
        const c = d.longpress(lanechips, ".lanechip", onLaneChipLongpress);
        if (typeof c === "function") cleanups.push(c);
      }
      if (mapwrap){
        const c = d.longpress(mapwrap, ".strow", onStationLongpress);
        if (typeof c === "function") cleanups.push(c);
      }
    }
    syncFareBtn();
    syncMapFullBtn();
  }

  function destroy(){
    while (cleanups.length){
      try { cleanups.pop()(); } catch { /* ignore */ }
    }
    bound = false;
  }

  function invalidate(){ mapSig = ""; }

  function restoreChrome(){
    if (mapFull && isDesktop() && doc && doc.body) doc.body.classList.add("map-full");
    syncFareBtn();
    syncMapFullBtn();
  }

  function scrollMapToTop(){
    if (mapscroll) mapscroll.scrollTop = 0;
  }

  return {
    render: renderMap,
    renderTabs: renderMapTabs,
    bind,
    destroy,
    invalidate,
    setFull: setMapFull,
    isFull: () => mapFull,
    restoreChrome,
    scrollMapToTop,
  };
}
