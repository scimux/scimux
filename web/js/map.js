/* Journeys map feature: tabs, lane chips, stack/wall render, selection, folds.
 *
 * Packet 7B ownership inventory
 * -----------------------------
 * Owned roots / controls:
 *   - #maptabs, #lanechips, #mapwrap, #mapscroll (scroll for toolbar)
 *   - #maptoolbar (floating wall-map selection bar)
 *   - #mappill (off-screen attention pill; full-screen wall only)
 *   - #mapfullbtn, #layersbtn (map chrome; Layers opens the Fare menu)
 *   - #mapdivider (dock map|chat separator; desktop-only under map-full.map-dock)
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
 *   - HTML into maptabs / lanechips / mapwrap / maptoolbar / mappill
 *   - body.map-full / body.map-dock classes; mapfullbtn ARIA/glyph;
 *     layersbtn (stable layers glyph + Layers menu; Fare state lives in the
 *     menu; revealed only under body.map-full)
 *   - body style --dockmap (dock split; CSS variable write, never a re-render)
 *   - localStorage: scimux-maptab, scimux-mapfold, scimux-mapfold-known,
 *     scimux-mapfull, scimux-fare, scimux-mapdock, scimux-mapdockh,
 *     scimux-mapdockpeek
 *   - uiMutate groups/lanes; callbacks: select, setLevel, setLaneFilter,
 *     loadChatHistory, jumpChatToNow, openActivityEditor, forkFromStation,
 *     addStationBookmark, exitThread, openSheet/closeSheets, renderCards
 *
 * Events owned after one idempotent bind():
 *   - #maptabs click (select / second-tap edit / ✕ remove / + create)
 *   - #lanechips click + longpress rename
 *   - #mapwrap click (fold / station / golane / goorigin / ring open / legacy wall btns)
 *   - #mapwrap longpress station → editor (+ wall selection when full)
 *   - #maptoolbar click (open chat / edit / bookmark / fork / exit)
 *   - #mappill click (select off-screen station + scrollIntoView)
 *   - #mapscroll scroll + window resize + dock split → positionMapToolbar /
 *     positionMapPill (rAF-throttled; never a re-render)
 *   - #mapfullbtn / #layersbtn click (Layers menu + Fare item)
 *   - #mapdivider pointerdown/move/up/cancel + keydown (dock split)
 *   - #tab_save / #tab_del click (map group sheet)
 *
 * Not owned (stay shell / other features):
 *   - static Journeys phead order and “Activities ›” #mapclose label
 *   - #mapclose / #journeybtn / #scrim / document touch spatial navigation
 *   - emptyTap on #mapscroll / #cardlist (shell fold-back)
 *   - document Escape → escapeDockStep dispatch (app-level key handler)
 *   - exitThread implementation (shared with cards; injected)
 *   - generic sheets, new-node/fork configuration sheets
 *   - cards, chat, composer, bookmarks, notes, search, polling
 *
 * Reuses (no algorithm duplication):
 *   lanes.js: groupLanes, servedLanes, forkKind, stopsOf, stopKey, stopLabel,
 *     newestFirst, parentStopIndex, laneColumnOrder, byNameID, stampMS
 *   map-model.js: statusText, hardAttention, headStopKey, toggleMapSelection
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
import {
  statusText, statusKind, turnFinished, hardAttention, headStopKey, toggleMapSelection,
} from "./map-model.js";
import { cardConfigText } from "./cards.js";
import { createPopoverMenu } from "./menu.js";

/* ---------- storage keys (public contract) ---------- */

export const MAP_TAB_KEY = "scimux-maptab";
export const MAP_FOLD_KEY = "scimux-mapfold";
export const MAP_FOLD_KNOWN_KEY = "scimux-mapfold-known";
export const MAP_FULL_KEY = "scimux-mapfull";
export const MAP_FARE_KEY = "scimux-fare";
/* Dock (wall map on top + real chat below) is a separate flag from full-screen
   wall mode. Do not reuse MAP_FULL_KEY / MAP_FOLD_KEY / MAP_FARE_KEY. */
export const MAP_DOCK_KEY = "scimux-mapdock";
/* Map height FRACTION under the dock (not px). Distinct from MAP_DOCK_KEY. */
export const MAP_DOCK_H_KEY = "scimux-mapdockh";
/* Dock peek (chat collapsed to a title strip). Distinct from MAP_DOCK_KEY /
   MAP_DOCK_H_KEY — peek is a state, not a fraction value. */

export const DOCK_FRAC_DEFAULT = 0.55;
export const DOCK_FRAC_MIN = 0.30;
export const DOCK_FRAC_MAX = 0.75;
export const DOCK_FRAC_NUDGE = 0.05;

/* Uniform wall-row height — shared by SVG geometry (rowY / svgH) and scroll
   anchoring across poll rebuilds. Must match .strow height in map.css. */
export const WALL_ROW_H = 76;

/* Keep the selected station fixed on screen when a poll inserts/removes rows
   above it. Pure arithmetic on row indices — no layout measurement.
   Returns prevTop unchanged when either index is missing (no selection, or
   the selected stop vanished). Never goes below 0. */
export function anchoredScrollTop({ prevTop, prevIndex, nextIndex, rowHeight } = {}){
  if (prevIndex == null || nextIndex == null) return prevTop;
  const next = Number(prevTop) + (Number(nextIndex) - Number(prevIndex)) * Number(rowHeight);
  return next < 0 ? 0 : next;
}

/* Attention pill geometry source: one tick per ringed HEAD at its fraction of
   wall content height (matches attentionStationSVG's head-only paint). The tick
   set is the *ring* set — hardAttention (approval/question/dialog), plus a
   finished turn — so the pill locates stations that need a decision or show
   ready news. Neutral inspect is diagnostic only and does not ring, tick, or
   count as waiting (fixes-2 P1). Pure, DOM-free; ordered as the rows are.
   kind is "waiting" (hard attention) or "ready" (turn_done only). */
export function railTicks({ rows, rowHeight, offset } = {}){
  const list = rows || [];
  const n = list.length;
  if (!n) return [];
  const RH = Number(rowHeight);
  const OFF = Number(offset);
  const contentH = OFF + n * RH;
  if (!(contentH > 0)) return [];
  const out = [];
  for (let i = 0; i < n; i++){
    const s = list[i];
    if (!s || !s.head || !s.n) continue;
    let kind = null;
    if (hardAttention(s.n)) kind = "waiting";
    else if (turnFinished(s.n)) kind = "ready";
    if (!kind) continue;
    out.push({
      nodeId: s.n.id,
      stopKey: stopKey(s),
      frac: (OFF + i * RH + RH / 2) / contentH,
      kind,
    });
  }
  return out;
}

/* Off-screen attention pill decision core. Pure: given tick fracs and the
   scroll viewport, decide whether to show, how many, which way, and the label.
   A tick is off-screen when frac*contentH falls outside
   [scrollTop, scrollTop + viewportH]. Prefer the nearest below when both
   directions have off-screen ticks.

   The pill counts where the user's attention is needed to let the agent
   proceed: if any waiting station is off-screen the pill is about *those*, and
   the finished ones are not counted, not named and not targeted — they get the
   pill to themselves once the waiting ones are on-screen or answered. Counting
   the total while naming one kind would say "4 waiting" for one blocked agent,
   which is the meaning P5 reserved for --attn, and could scroll you to a
   station the label never mentioned. */
export function attentionPill({ ticks, scrollTop, viewportH, contentH } = {}){
  const list = ticks || [];
  const top = Number(scrollTop) || 0;
  const vh = Number(viewportH) || 0;
  const ch = Number(contentH) || 0;
  const bottom = top + vh;
  const off = [];
  for (const t of list){
    if (!t) continue;
    const y = Number(t.frac) * ch;
    if (!(y >= top && y <= bottom)) off.push(t);
  }
  if (!off.length){
    return {
      show: false, count: 0, dir: "down", kind: "waiting",
      nodeId: "", stopKey: "", label: "",
    };
  }
  /* One kind owns the pill: waiting if any is off-screen, else ready. Count,
     arrow and target all come from that set — never from the other. */
  const waiting = off.filter(t => t.kind === "waiting");
  const kind = waiting.length ? "waiting" : "ready";
  const named = waiting.length ? waiting : off;
  const below = [];
  const above = [];
  for (const t of named){
    const y = Number(t.frac) * ch;
    if (y > bottom) below.push(t);
    else above.push(t);
  }
  let dir, target;
  if (below.length){
    dir = "down";
    target = below.reduce((a, b) => (Number(a.frac) <= Number(b.frac) ? a : b));
  } else {
    dir = "up";
    target = above.reduce((a, b) => (Number(a.frac) >= Number(b.frac) ? a : b));
  }
  return {
    show: true,
    count: named.length,
    dir,
    kind,
    nodeId: target.nodeId || "",
    stopKey: target.stopKey || "",
    label: `${named.length} ${kind}`,
  };
}

export const ATTN_GLOW_DEF = `<defs><filter id="attnglow" x="-80%" y="-80%" width="260%" height="260%">
      <feGaussianBlur stdDeviation="3.2"/></filter></defs>`;

/* ---------- pure visibility / tab / fold / selection decisions ---------- */

/* full screen shows the map regardless of the column-mode map-open flag */
export function mapIsVisible({ isDesktop, mapFull, mapOpen, level } = {}){
  return isDesktop ? (!!mapFull || !!mapOpen) : level === 3;
}

/* Pure layout mode for the map/chat arrangement. Phone never docks (the wall
   itself is desktop-only); dock requires both full-screen and the dock flag. */
export function dockArrangement({ mapFull, mapDock, isDesktop } = {}){
  if (!isDesktop) return "stack";
  if (mapFull && mapDock) return "dock";
  if (mapFull) return "wall";
  return "stack";
}

/* Clamp the dock map-height fraction. Non-finite (NaN/undefined/…) → default
   0.55 so a hand-edited localStorage value can never produce an unusable split. */
export function clampDockFrac(frac){
  const n = Number(frac);
  if (!Number.isFinite(n)) return DOCK_FRAC_DEFAULT;
  if (n < DOCK_FRAC_MIN) return DOCK_FRAC_MIN;
  if (n > DOCK_FRAC_MAX) return DOCK_FRAC_MAX;
  return n;
}

/* Raw map-height fraction from a vertical drag over #app, then clamped.
   appHeight <= 0 (hidden / zero rect) → default rather than NaN/Infinity. */
export function dockFracFromDrag({ clientY, appTop, appHeight } = {}){
  if (!(appHeight > 0)) return DOCK_FRAC_DEFAULT;
  return clampDockFrac((Number(clientY) - Number(appTop || 0)) / appHeight);
}

/* What a tap on a wall row should do. The action bar is a selection-scoped
   command surface, and HIG dismisses that family (edit menu, contextual menu,
   popover) when a command is chosen — so a command closes the bar while the
   selection survives, being the row highlight, the scroll anchor, and what the
   docked chat is pointed at. That leaves one gap: with the bar closed and the
   row still selected, an ordinary toggle would read the second tap as
   "deselect" and the user would have to tap twice to get the menu back. */
export function stationTapAction({ id, key, barOpen, mapSel, mapSelKey } = {}){
  if (!barOpen && id === mapSel && key === mapSelKey) return "reopen";
  return "toggle";
}

/* Escape ladder under the wall map. Pure — app.js dispatches the result.
   dock open → out of the dock → out of full screen. The middle "peek" rung
   was removed after acceptance: a pop-out folded to its title strip is
   neither reading the chat nor reading the map, so Escape closes it outright.
   mapFull:false → "none" at every input (phone / not full). */
export function escapeDockStep({ mapFull, mapDock } = {}){
  if (!mapFull) return "none";
  return mapDock ? "undock" : "exit-full";
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

/* Per-tab badge: one number, and the kind the hue will name. Reuses P6's
   pill rule (railTicks / attentionPill): hardAttention is waiting, turn_done
   without hard attention is ready, and a tab holding both badges the waiting
   count only. Neutral inspect never counts (fixes-2 P1). Never the sum: the
   number and the hue must always say the same thing.
   Membership mirrors renderMap: lane_id && served(n).some(inGroup), with the
   group's lanes read through groupLanes so a record with no lanes key means
   "no filter" in both places. A station served by several lanes counts once
   for All and once per intersecting group, never twice inside one tab.
   Pure; returns { all: {n, kind}, [groupId]: {n, kind} }. */
export function mapTabAttentionCounts(nodes, groups, { served = servedLanes } = {}){
  const list = Array.isArray(nodes) ? nodes : [];
  const gs = Array.isArray(groups) ? groups : [];
  const tabs = [["all", groupLanes("all", gs)], ...gs.map(g => [g.id, groupLanes(g.id, gs)])];
  const tally = {};
  for (const [id] of tabs) tally[id] = { waiting: 0, ready: 0 };
  for (const n of list){
    if (!n || !n.lane_id) continue;
    const kind = hardAttention(n) ? "waiting" : (turnFinished(n) ? "ready" : "");
    if (!kind) continue;
    const lanes = served(n) || [];
    for (const [id, grp] of tabs){
      if (lanes.some(laneID => !grp || grp.includes(laneID))) tally[id][kind]++;
    }
  }
  const out = {};
  for (const [id] of tabs){
    const t = tally[id];
    out[id] = t.waiting ? { n: t.waiting, kind: "waiting" } : { n: t.ready, kind: "ready" };
  }
  return out;
}

/* Signature for the map-tabs region: rebuild only when selection, group set
   (id+name), or badge counts change — kind included, so 1 waiting → 1 ready
   repaints instead of keeping the wrong hue. Counts alone turn a formerly static
   bar into a polled one; without this gate every tick would rewrite #maptabs. */
export function mapTabsSignature(mapTab, groups, counts){
  return JSON.stringify({
    mapTab: mapTab || "all",
    groups: (groups || []).map(g => [g.id, g.name]),
    counts: counts || {},
  });
}

export function mapTabsHTML(mapTab, groups, { escape = esc, counts = null } = {}){
  const c = counts || {};
  const badge = c => {
    const k = Number(c && c.n) || 0;
    if (k <= 0) return "";
    /* Marker only — the hue lives in CSS, same split as #mappill.ready. */
    const ready = c.kind === "ready" ? " ready" : "";
    return `<span class="tabcount${ready}">${k}</span>`;
  };
  const sorted = [...(groups || [])].sort(byNameID);
  return (
    `<button data-mt="all" class="${mapTab === "all" ? "on" : ""}">All${badge(c.all)}</button>` +
    sorted.map(t => `<button data-mt="${escape(t.id)}" class="${mapTab === t.id ? "on" : ""}">
       ${escape(t.name)}${badge(c[t.id])}<span class="x" data-rm="${escape(t.id)}">&#10005;</span></button>`).join("") +
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
  const hasAttn = b => b.rows.some(s => hardAttention(s.n)) ? 1 : 0;
  const fold = mapFold || new Set();
  return JSON.stringify((blocks || []).map(b => [b.lane.id, b.lane.name, lm.color(b.lane.id),
      fold.has(b.lane.id), hasAttn(b),
      b.rows.map(s => { const l = stopLabel(s); return [stopKey(s), s.time, s.head, l.title, l.desc,
        s.n.agent, s.n.model, s.n.effort, s.n.lane_id, s.n.parent || "", s.n.ended_at || "",
        s.n.live, s.n.attention, s.n.turn_done ? 1 : 0]; })]))
    + "|" + focusLane + "|" + mapTab + "|" + mapFull;
}

/* Fare fingerprint for wall re-render when the overlay is on. Uses fare_*
   totals + the per-segment new-token figures the capsules print (not
   last_activity) so a token/cost poll update rebuilds the capsule labels;
   fareOff keeps the field inert so idle polls stay skip-able. */
function fareFingerprint(n){
  if (!n) return "";
  return [
    n.fare_total ?? "",
    n.fare_turns ?? "",
    n.fare_cost_complete ? 1 : 0,
    fareSegmentsFingerprint(n),
  ].join(",");
}

/* ---------- repaint budget: shape rebuilds, state patches ----------
   The wall is one innerHTML write over ~170 rows and a 12,784px SVG, so it
   may only be rebuilt when its *shape* changes. Mechanical liveness and the
   context gauge move on the poll cadence — across a fleet that means every
   tick — so they are deliberately kept out of the signature and painted onto
   the existing nodes instead (patchStationVolatile). */

/* Only the liveness transitions that change what is drawn: exited and
   unavailable swap the filled dot for a hollow one and put .dead on the row.
   active vs quiet is one circle radius and one caption word — patchable. */
export function liveShape(live){
  return live === "exited" || live === "unavailable" ? live : "run";
}

export function wallMapSignature(rows, cols, { focusLane, mapTab, mapSel, mapSelKey, fareOn } = {}){
  return "wall|" + (cols || []).join(",") + "|" + focusLane + "|" + mapTab + "|" +
    mapSel + "|" + mapSelKey + "|" + (fareOn ? "F" : "") + "|" +
    JSON.stringify((rows || []).map(s => { const l = stopLabel(s); return [stopKey(s), s.time, s.head, s.n.created_at,
      l.title, l.desc, s.n.agent,
      s.n.model, s.n.effort, s.n.lane_id, s.n.parent || "", s.n.ended_at || "",
      liveShape(s.n.live),
      // Presence, not value: a gauge appearing from null has to create SVG
      // elements, which no attribute write can do. Its drift is patched.
      // turn_done is in the signature so the finished ring rebuilds (SVG),
      // not just the status word (patchable).
      s.n.attention, s.n.turn_done ? 1 : 0, s.n.ctx_pct == null ? 0 : 1,
      fareOn ? fareFingerprint(s.n) : 0]; }));
}

/* The per-station state the signature deliberately ignores. Earlier stops
   render "· earlier stop" — no status word, no live dot — so they have none. */
export function stationVolatile(n){
  if (!n) return "";
  return (n.live || "") + "|" + (n.ctx_pct == null ? "" : n.ctx_pct)
    + "|" + (n.turn_done ? 1 : 0) + "|" + (n.attention || "");
}

export function wallVolatile(rows){
  const out = {};
  (rows || []).forEach(s => { if (s && s.head && s.n) out[stopKey(s)] = stationVolatile(s.n); });
  return out;
}

/** Keys whose volatile state moved since the last frame. No previous frame
    means paint everything (the wall was just rebuilt from scratch). */
export function volatileDiff(prev, next){
  const out = [];
  Object.keys(next || {}).forEach(k => { if (!prev || prev[k] !== next[k]) out.push(k); });
  return out;
}

/** Radius of the context gauge ring. Shared by the renderer and the patch so
    the arc's circumference cannot drift between them. */
export const CTX_RING_R = 9;

/** Repaint one station's liveness and gauge on the nodes already in the DOM.
    Attribute and text writes only — an innerHTML write here would give back
    everything the skip bought. A station whose hooks are absent (an exited
    node has no patchable dot) is a silent no-op: this runs inside the poll
    tick and must never throw. */
export function patchStationVolatile(root, key, n, opts = {}){
  if (!root || typeof root.querySelector !== "function" || !n) return false;
  const cssEsc = opts.cssEscape || (s => String(s));
  const st = opts.status || statusText;
  const kindFn = opts.statusKind || statusKind;
  const k = cssEsc(key);
  const row = root.querySelector(`.strow[data-skey="${k}"]`);
  const cap = row && typeof row.querySelector === "function" ? row.querySelector(".cap .st") : null;
  if (cap){
    // Class carries colour + the running ::after pulse; text is the word.
    // Attribute/text writes only — never innerHTML (skip-path contract).
    const kind = kindFn(n);
    if ("className" in cap) cap.className = "st st-" + kind;
    cap.textContent = st(n);
  }
  const dot = root.querySelector(`circle[data-dot="${k}"]`);
  if (dot) dot.setAttribute("r", n.live === "active" ? 6.5 : 5.5);
  const arc = root.querySelector(`circle[data-ctx="${k}"]`);
  if (arc){
    const C = 2 * Math.PI * CTX_RING_R;
    const frac = Math.max(0, Math.min(1, (n.ctx_pct || 0) / 100));
    arc.setAttribute("stroke-dasharray", `${frac * C} ${C}`);
    arc.setAttribute("stroke", n.ctx_pct >= 80 ? "var(--attn)" : (arc.getAttribute("data-col") || ""));
  }
  return true;
}

/* ---------- V2-P3 lane heat (fare-design.md v2.3 / V2-P3) ----------
   Encode per-segment token cost as track stroke-width (+opacity). Thickness
   is the accessible channel — never colour alone. Always-on under
   mapFull && fareOn (wall-map eye-candy only). */

export const TRACK_STROKE_BASE = 3.5;
export const TRACK_STROKE_MAX = 11;
/** Cost (token total) at which stroke-width reaches TRACK_STROKE_MAX. */
export const HEAT_COST_CAP = 200_000;

/* Heat only on the full-screen wall when the fare layer is on. */
export function heatEnabled({ mapFull, fareOn } = {}){
  return !!(mapFull && fareOn);
}

/* V2-P5: heat is *parked in the UI*, not removed. Reviewed live, lane
   thickness read as noise — it tracks context size x turns (cache reads are
   ~99% of the token total on a long chat), not work spent. The maintainer
   wants to revisit the encoding before deleting the implementation, so the
   render gate is a single named constant instead of a code deletion. */
export const HEAT_IN_UI = false;
export function wallHeatOn(fareOn){
  return HEAT_IN_UI && !!fareOn;
}

/* Token heat magnitude from one fare_segments entry: the four canonical
   quantities (or shipped `total`). null = absent (≠ zero). Time parts
   (agent/tools/wait) never drive heat — those surface in V2-P4's callout. */
export function segmentTokenCost(seg){
  if (seg == null || typeof seg !== "object") return null;
  if (seg.total != null){
    const t = Number(seg.total);
    if (Number.isFinite(t)) return Math.max(0, t);
  }
  const keys = ["fresh_in", "cache_read", "cache_write", "out"];
  let any = false, sum = 0;
  for (const k of keys){
    if (seg[k] != null){
      const v = Number(seg[k]);
      if (Number.isFinite(v)){ any = true; sum += v; }
    }
  }
  return any ? Math.max(0, sum) : null;
}

/* Bounded monotonic stroke-width from a token cost. Absent/non-finite/≤0 →
   base track (never zero-width or missing). Huge costs clamp at max.
   Rounded to 2dp so SVG attributes stay stable under poll. */
export function heatStrokeWidth(cost){
  if (cost == null || !Number.isFinite(cost) || cost <= 0) return TRACK_STROKE_BASE;
  const t = Math.min(1, cost / HEAT_COST_CAP);
  const w = TRACK_STROKE_BASE + t * (TRACK_STROKE_MAX - TRACK_STROKE_BASE);
  return Math.round(w * 100) / 100;
}

/* Mild opacity ramp with cost (redundant channel; thickness is primary).
   Dim-lane baseOp is preserved as a multiplier. */
export function heatOpacity(cost, baseOp = 1){
  const base = Number(baseOp);
  const b = Number.isFinite(base) ? base : 1;
  if (cost == null || !Number.isFinite(cost) || cost <= 0) return b;
  const t = Math.min(1, cost / HEAT_COST_CAP);
  // 0.85 → 1.0 of baseOp so dim lanes stay dim and heat still reads.
  return b * (0.85 + 0.15 * t);
}

/* Per-segment heat fingerprint for wallMapSignature. */
export function heatSegmentsFingerprint(n){
  if (!n || !Array.isArray(n.fare_segments) || !n.fare_segments.length) return "";
  return n.fare_segments.map(s => {
    const c = segmentTokenCost(s);
    return c == null ? "" : String(c);
  }).join(";");
}

/* Token cost for the track gap between two stops. Same node + consecutive
   stop indices → fare_segments[min(i)]; otherwise absent. */
export function gapTokenCost(a, b){
  if (!a || !b || !a.n || !b.n || a.n.id !== b.n.id) return null;
  if (Math.abs(a.i - b.i) !== 1) return null;
  const i = Math.min(a.i, b.i);
  const segs = a.n.fare_segments;
  if (!Array.isArray(segs) || segs[i] == null) return null;
  return segmentTokenCost(segs[i]);
}

/* Vertical track SVG for one lane/branch line. stops: [{y, stop}, ...]
   with at least 2 points. When heatOn, each inter-station gap gets its own
   stroke-width from fare_segments; otherwise a single base-width line.
   Absent cost → base track (line still drawn). */
export function wallLaneTrackSVG(stops, { x, color, opacity = 1, heatOn = false } = {}){
  if (!stops || stops.length < 2) return "";
  if (!heatOn){
    const ys = stops.map(s => s.y);
    return `<line x1="${x}" y1="${Math.min(...ys)}" x2="${x}" y2="${Math.max(...ys)}"
            stroke="${color}" stroke-width="${TRACK_STROKE_BASE}" stroke-linecap="round"
            opacity="${opacity}"/>`;
  }
  const sorted = [...stops].sort((a, b) => a.y - b.y || 0);
  let out = "";
  for (let i = 0; i < sorted.length - 1; i++){
    const a = sorted[i], b = sorted[i + 1];
    if (a.y === b.y) continue;
    const cost = gapTokenCost(a.stop, b.stop);
    const sw = heatStrokeWidth(cost);
    const op = heatOpacity(cost, opacity);
    const heatAttr = cost != null ? ` data-heat="${cost}"` : "";
    out += `<line x1="${x}" y1="${a.y}" x2="${x}" y2="${b.y}"
            stroke="${color}" stroke-width="${sw}" stroke-linecap="round"
            opacity="${op}"${heatAttr}/>`;
  }
  return out;
}

/* Station ring/pulse for hard attention (default --attn yellow) and for a
   finished turn (caller passes var(--work)). Same paint path, distinct hue
   so yellow keeps exactly one meaning: blocked on you (P5).
   Glow+ring only — the transparent hit target is attentionHitSVG, emitted
   AFTER the solid station dot (and ctx ring) so SVG hit-testing reaches the
   centre of the station (paint order: later siblings win). */
export function attentionStationSVG(x, y, op, hue){
  const c = hue || "var(--attn)";
  return `<circle class="attnstation-glow" style="--attn-op:${op}" cx="${x}" cy="${y}" r="10.5" fill="${c}" filter="url(#attnglow)"/>
          <circle class="attnstation-ring" style="--attn-op:${op}" cx="${x}" cy="${y}" r="9.5" fill="none" stroke="${c}" stroke-width="2.5"/>`;
}

/* Transparent r=22 hit circle (44px HIG target) carrying data-jump so a tap
   on the ring/centre opens chat. Must be the last paint for that station. */
export function attentionHitSVG(x, y, nodeId){
  if (nodeId == null || nodeId === "") return "";
  return `<circle class="attnstation-hit" cx="${x}" cy="${y}" r="22" fill="transparent" pointer-events="all" data-jump="${esc(String(nodeId))}"/>`;
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

/* ---------- V2-P4/P5 centered capsule + fare ticket (fare-design.md v2.3)
   A capsule rides the midpoint of *every* priced inter-station gap; tapping
   one opens the fare ticket bottom sheet. The V2-P4 anchored callout is
   retired: centred on the leftmost lane's x it was clipped by the viewport,
   and a one-line dot-separated dump of eight quantities was unreadable. Same
   mapFull && fareOn gate as heat. */

/* Same gate as heat: full-screen wall + fare layer on. */
export function fareOverlayEnabled({ mapFull, fareOn } = {}){
  return !!(mapFull && fareOn);
}

/* Compact wall-clock duration for a ride (12ms / 45s / 1m 23s / 7h 48m /
   2d 5h). The unit pair climbs with the magnitude: a supervised chat runs for
   hours and a journey's cumulative time for days, and "1300m 51s" is a number
   nobody can relate to a clock. The smaller unit is dropped when it is zero,
   and each scale rounds before it is split so the seam can never print "60m"
   or "24h". */
export function fmtRideMs(ms){
  if (ms == null || ms === "") return "";
  const v = Number(ms);
  if (!Number.isFinite(v) || v < 0) return "";
  if (v < 1000) return Math.round(v) + "ms";
  if (v < 60_000){
    const s = v / 1000;
    if (s >= 10) return Math.round(s) + "s";
    return s.toFixed(1).replace(/\.0$/, "") + "s";
  }
  const pair = (big, small, ub, us) => small ? `${big}${ub} ${small}${us}` : `${big}${ub}`;
  const sec = Math.round(v / 1000);
  if (sec < 3600) return pair(Math.floor(sec / 60), sec % 60, "m", "s");
  const min = Math.round(v / 60_000);
  if (min < 1440) return pair(Math.floor(min / 60), min % 60, "h", "m");
  const hr = Math.round(v / 3_600_000);
  return pair(Math.floor(hr / 24), hr % 24, "d", "h");
}

/* The fare a ride actually cost: fresh input + reply. Cache reads are
   *carried*, not bought — they are re-sent context, and on a long chat they
   are ~99% of the token total, which is why the v1 headline (and the heat
   encoding) read as noise. null = neither side was ever reported (≠ zero:
   a reported 0 is a value). */
export function segmentNewTokens(seg){
  if (seg == null || typeof seg !== "object") return null;
  let any = false, sum = 0;
  for (const k of ["fresh_in", "out"]){
    if (seg[k] != null){
      const v = Number(seg[k]);
      if (Number.isFinite(v)){ any = true; sum += v; }
    }
  }
  return any ? Math.max(0, sum) : null;
}

/* Per-segment new-token fingerprint for wallMapSignature: what the capsules
   actually print, so a poll that only moves cache reads stays skip-able. */
function fareSegmentsFingerprint(n){
  if (!n || !Array.isArray(n.fare_segments) || !n.fare_segments.length) return "";
  return n.fare_segments.map(s => {
    const v = segmentNewTokens(s);
    return v == null ? "" : String(v);
  }).join(";");
}

/* The fare_segments entry bridging two adjacent stops of one node.
   fare_segments[i] spans stop i → stop i+1. Order-insensitive (wall rows are
   newest-first). null across nodes, across non-adjacent stops, or with no
   segment data. */
export function gapSegmentRef(a, b){
  if (!a || !b || !a.n || !b.n || a.n.id !== b.n.id) return null;
  if (Math.abs(a.i - b.i) !== 1) return null;
  const i = Math.min(a.i, b.i);
  const segs = a.n.fare_segments;
  if (!Array.isArray(segs) || segs[i] == null) return null;
  return { nodeId: a.n.id, segIdx: i, seg: segs[i] };
}

/* Capsule placements for one drawn line: the vertical midpoint of every gap
   that has a fare. pts: [{y, stop}, ...] in draw order. */
export function wallCapsuleSpots(pts, x){
  const out = [];
  if (!Array.isArray(pts) || pts.length < 2) return out;
  for (let k = 0; k < pts.length - 1; k++){
    const a = pts[k], b = pts[k + 1];
    const ref = gapSegmentRef(a.stop, b.stop);
    if (!ref || segmentNewTokens(ref.seg) == null) continue;
    out.push({ x, y: (a.y + b.y) / 2, ...ref });
  }
  return out;
}

/* Opaque-surface capsule at the (x, y) track midpoint — the ride's fare and
   nothing else. Carries its node + segment index so the tap can build the
   ticket. Position is fixed geometry so poll rebuilds update the label in
   place (no jump). Empty without a segment. */
export function fareCapsuleHTML(seg, opts = {}){
  if (!seg || typeof seg !== "object") return "";
  const escape = opts.escape || esc;
  const x = Number(opts.x), y = Number(opts.y);
  const px = Number.isFinite(x) ? x : 0;
  const py = Number.isFinite(y) ? y : 0;
  const tokens = segmentNewTokens(seg);
  const label = tokens == null ? "—" : fmtFareTokens(tokens);
  // translate(-50%,-50%) centres the capsule on the track midpoint; opaque
  // surface bg so the line reads as passing behind (Apple Maps style).
  return `<button type="button" class="fare-capsule" data-fare-capsule
    data-fare-node="${escape(opts.nodeId || "")}"
    data-fare-seg="${escape(String(opts.segIdx ?? ""))}"
    style="left:${px}px;top:${py}px"
    aria-label="fare ${escape(label)} new tokens — open ticket">
    <span class="fare-capsule-val">${escape(label)}</span>
  </button>`;
}

/* Layers popover body. Fare is a menuitemcheckbox; the decorative checkmark
   is hidden from assistive technology. Visible labels are escaped. */
export function layersMenuHTML({ fareOn = false, label = "Fare", escape = esc } = {}){
  const checked = !!fareOn;
  const text = escape(label);
  const mark = checked ? `<span class="check" aria-hidden="true">✓</span>` : "";
  return `<button type="button" role="menuitemcheckbox" aria-checked="${checked ? "true" : "false"}" data-layer="fare">${mark}<span>${text}</span></button>`;
}

/* ---------- V2-P5 fare ticket (the capsule's bottom sheet) ----------
   A vintage paper fare ticket: the tokens spent are the ticket's price, so
   they get the ticket's biggest type. Every block is data-conditional —
   a quantity the transport never reported hides its own row, bar or stamp
   rather than printing a zero (v2-D8 absent != zero). Perforations reuse the
   .actionbar.tear geometry. */

/* Breakdown rows. Zero is indistinguishable from "never reported" in the
   int-valued wire shape, so a 0 hides its row too (a codex ride genuinely
   has no cache write; printing "cache written 0" invents a fact). `counted`
   marks the two quantities that make up the fare. */
export function ticketTokenRows(seg){
  if (!seg || typeof seg !== "object") return [];
  const defs = [
    ["fresh_in", "new input", true],
    ["out", "reply", true],
    ["cache_read", "cache reused", false],
    ["cache_write", "cache written", false],
  ];
  const rows = [];
  for (const [k, label, counted] of defs){
    const v = Number(seg[k]);
    if (seg[k] == null || !Number.isFinite(v) || v <= 0) continue;
    rows.push({ key: k, label, counted, tokens: v, value: fmtFareTokens(v) });
  }
  return rows;
}

const SPLIT_DEFS = [
  ["agent", "agent_ms", "thinking", "work"],
  ["tools", "tools_ms", "tools", "tools"],
  ["wait", "wait_ms", "waiting for you", "attn"],
];

function splitPartsFrom(ms){
  const total = ms.reduce((a, v) => a + v, 0);
  if (!(total > 0)) return null;
  return SPLIT_DEFS.map(([key, , label, tone], i) => ({
    key, label, tone, ms: ms[i], frac: ms[i] / total,
  }));
}

/* Stacked-bar parts for one ride. Only when the transport stamped the split
   (v2-P2 records); a historical ride is real-only and a fabricated split
   would silently attribute approval waiting to tool time. */
export function ticketSplitParts(seg){
  if (!seg || typeof seg !== "object") return null;
  if (!SPLIT_DEFS.some(([, f]) => seg[f] != null)) return null;
  return splitPartsFrom(SPLIT_DEFS.map(([, f]) => {
    const v = Number(seg[f]);
    return Number.isFinite(v) && v > 0 ? v : 0;
  }));
}

/* Same partition for the whole line. Hidden outright when *any* ride lacks
   the split: summing only the split rides would under-report the journey and
   read as a complete picture. */
export function journeySplitParts(segs){
  if (!Array.isArray(segs) || !segs.length) return null;
  const sum = [0, 0, 0];
  for (const s of segs){
    const p = ticketSplitParts(s);
    if (!p) return null;
    p.forEach((part, i) => { sum[i] += part.ms; });
  }
  return splitPartsFrom(sum);
}

/* Whole-journey figures for the receipt foot. The whole point of the foot is
   that summing the capsules by hand is a waste of time. */
export function journeyTicketTotals(n){
  if (!n || typeof n !== "object") return null;
  const segs = Array.isArray(n.fare_segments) ? n.fare_segments : [];
  const newTokens = segmentNewTokens({ fresh_in: n.fare_fresh_in, out: n.fare_out });
  let realMS = null;
  for (const s of segs){
    if (!s || s.real_ms == null) continue;
    const v = Number(s.real_ms);
    if (Number.isFinite(v)) realMS = (realMS || 0) + v;
  }
  const rides = n.fare_turns == null ? null : Number(n.fare_turns);
  return {
    newTokens,
    realMS,
    // D8: a cost is printed only when the agent reported every turn's cost
    cost: n.fare_cost_complete && n.fare_cost != null ? Number(n.fare_cost) : null,
    rides: Number.isFinite(rides) ? rides : null,
    stops: 1 + (Array.isArray(n.stops) ? n.stops.length : 0),
    segments: segs,
  };
}

/* Everything the ticket prints, gathered from the node + stop chain.
   fare_segments carries no timestamps, so departure/arrival come from the
   stops the segment bridges. */
export function fareTicketContext(n, segIdx, opts = {}){
  if (!n || typeof n !== "object") return null;
  const i = Number(segIdx);
  const segs = Array.isArray(n.fare_segments) ? n.fare_segments : [];
  if (!Number.isInteger(i) || i < 0 || i >= segs.length || segs[i] == null) return null;
  const label = opts.stopLabel || stopLabel;
  const time = opts.timeLabel || fmtStamp;
  const chain = (opts.stopsOf || stopsOf)(n);
  if (chain.length < i + 2) return null;
  const station = k => {
    const st = chain[k];
    const l = label(st) || {};
    return { title: l.title || "", desc: l.desc || "", index: k + 1, time: st.time ? time(st.time) : "" };
  };
  const totals = journeyTicketTotals(n);
  return {
    nodeId: n.id, segIdx: i, seg: segs[i],
    from: station(i), to: station(i + 1),
    agent: n.agent || "", model: n.fare_model || n.model || "", effort: n.effort || "",
    totals, segments: segs,
  };
}

function ticketFigure(tokens, big){
  const label = tokens == null ? "—" : fmtFareTokens(tokens);
  return `<span class="tk-fig ${big ? "tk-fig-lg" : ""}">${label}</span>` +
    `<span class="tk-fig-unit">new tokens</span>`;
}

function ticketBarHTML(parts, escape){
  if (!parts) return "";
  const seg = parts.map(p =>
    `<i class="tk-part tk-${p.tone}" style="flex:${p.frac.toFixed(4)}"></i>`).join("");
  const key = parts.map(p =>
    `<span class="tk-key"><i class="tk-dot tk-${p.tone}"></i>${escape(p.label)} ${escape(fmtRideMs(p.ms))}</span>`).join("");
  return `<div class="ticket-bar">${seg}</div><div class="tk-legend">${key}</div>`;
}

/* Render one ticket. `ctx` comes from fareTicketContext (or a test double).
   Empty string without a segment — never a skeleton. */
export function fareTicketHTML(ctx, opts = {}){
  if (!ctx || !ctx.seg || typeof ctx.seg !== "object") return "";
  const escape = (opts && opts.escape) || esc;
  const seg = ctx.seg;
  const from = ctx.from || {}, to = ctx.to || {};
  const t = ctx.totals || {};
  const stopNo = s => s.index == null ? "" : "stop " + String(s.index).padStart(2, "0");
  const sub = (s, verb) => {
    const bits = [stopNo(s), s.time ? verb + " " + s.time : ""].filter(Boolean);
    return bits.length ? `<div class="tk-sub">${escape(bits.join(" · "))}</div>` : "";
  };
  const line = [ctx.agent, ctx.model, ctx.effort].filter(Boolean).join(" · ");
  const rows = ticketTokenRows(seg);
  const split = ticketSplitParts(seg);
  const jsplit = journeySplitParts(ctx.segments);
  const clock = [from.time, to.time].filter(Boolean).join(" → ");
  const cost = seg.cost_complete && seg.cost != null ? fmtFareCost(seg.cost) : "";
  const foot = [t.realMS == null ? "" : fmtRideMs(t.realMS),
    t.cost == null ? "" : fmtFareCost(t.cost)].filter(Boolean).join(" · ");
  const rides = [t.rides == null ? "" : t.rides + (t.rides === 1 ? " ride" : " rides"),
    t.stops == null ? "" : t.stops + (t.stops === 1 ? " stop" : " stops")]
    .filter(Boolean).join(" · ");
  return `<div class="fare-ticket" data-fare-node="${escape(ctx.nodeId || "")}"
    data-fare-seg="${escape(String(ctx.segIdx ?? ""))}">
  <div class="tk-head"><span>SCIMUX · FARE</span><span>${escape(rides)}</span></div>
  <div class="tk-rule"></div>
  <div class="tk-route">
    <div class="tk-stn">
      <div class="tk-cap">FROM</div>
      <div class="tk-name">${escape(from.title || "")}</div>
      ${sub(from, "dep")}
    </div>
    <div class="tk-arrow">→</div>
    <div class="tk-stn">
      <div class="tk-cap">TO</div>
      <div class="tk-name">${escape(to.title || "")}</div>
      ${sub(to, "arr")}
    </div>
  </div>
  ${line ? `<div class="tk-line">${escape(line)}</div>` : ""}
  <div class="tk-fare">
    <div class="tk-figrow">${ticketFigure(segmentNewTokens(seg), true)}</div>
    ${cost ? `<div class="tk-stamp"><b>${escape(cost)}</b><span>REPORTED</span></div>` : ""}
  </div>
  <div class="tk-note">fresh input + reply · cache excluded</div>
  <div class="tk-tear"></div>
  ${rows.length ? `<div class="tk-cap">FARE BREAKDOWN</div>
  <div class="tk-rows">${rows.map(r => `<div class="tk-row ${r.counted ? "" : "tk-off"}">
      <span class="tk-rlabel">${escape(r.label)}</span>
      <span class="tk-lead"></span>
      <span class="tk-rval">${escape(r.value)}</span>
    </div>`).join("")}</div>
  <div class="tk-note">cache reads are carried, not bought — they don't count toward the fare</div>
  <div class="tk-tear"></div>` : ""}
  ${seg.real_ms == null ? "" : `<div class="tk-caprow">
    <span class="tk-cap">JOURNEY TIME</span>${clock ? `<span class="tk-clock">${escape(clock)}</span>` : ""}
  </div>
  <div class="tk-dur">${escape(fmtRideMs(seg.real_ms))}${
    split ? "" : `<span class="tk-nosplit">tool timing not stamped for this ride</span>`}</div>
  ${ticketBarHTML(split, escape)}`}
  <div class="tk-foot">
    <div class="tk-rule tk-rule-b"></div>
    <div class="tk-cap">TOTAL · WHOLE JOURNEY</div>
    <div class="tk-totrow">
      <div class="tk-figrow">${ticketFigure(t.newTokens, false)}</div>
      ${foot ? `<div class="tk-totside">${escape(foot)}</div>` : ""}
    </div>
    ${jsplit ? ticketBarHTML(jsplit, escape)
      : `<div class="tk-note">split unavailable for some rides — total time only</div>`}
  </div>
</div>`;
}

export function stationRowHTML(n, lm, opts = {}){
  const {
    padLeft = 0, others = [], dim = false, golane = false, current = false,
    fork = false, stop = null, alt = false,
    escape = esc, stamp = fmtStamp, status = statusText, configText = cardConfigText,
    forkCue = "",
  } = opts;
  const label = stop ? stopLabel(stop) : { title: n.title || "", desc: n.description || n.prompt || "" };
  if (stop && !stop.head)
    return `
        <div class="strow stoprow ${alt ? "alt " : ""}${dim ? "dimmed" : ""}${current ? " current" : ""}" style="padding-left:${padLeft}px" data-nid="${escape(n.id)}" data-skey="${escape(stopKey(stop))}" data-stop="${escape(stop.time)}">
          <div class="lbl${label.desc ? " hasdesc" : ""}"><span class="t">${escape(label.title)}</span></div>
          <div class="cap">${escape(stamp(stop.time))} &middot; earlier stop</div>
          ${label.desc ? `<div class="desc">${escape(label.desc)}</div>` : ""}
        </div>`;
  const when = stop && stop.i > 0
    ? `${stamp(stop.time)} · stop ${stop.i + 1}` : stamp(n.created_at);
  const desc = label.desc;
  // kind from the same precedence as the word (statusKind), even when a test
  // injects a custom status() string — class and default word stay aligned.
  const kind = (opts.statusKind || statusKind)(n);
  return `
        <div class="strow ${alt ? "alt " : ""}${dim ? "dimmed" : ""} ${n.live === "exited" ? "dead" : ""}${current ? " current" : ""}"
             style="padding-left:${padLeft}px" data-nid="${escape(n.id)}"${stop ? ` data-skey="${escape(stopKey(stop))}"` : ""}>
          <div class="lbl${desc ? " hasdesc" : ""}"><span class="agent-logo" title="${escape(n.agent || "agent")}">${opts.agentLogo || ""}</span><span class="t">${escape(label.title)}</span>${fork ? forkCue : ""}</div>
          <div class="cap">${escape(when)} · ${escape(configText(n))} · <span class="st st-${kind}">${escape(status(n))}</span>${
            others.map(l => ` · <span class="xchip"${golane ? ` data-golane="${escape(l)}"` : ""} style="color:${escape(lm.color(l))}">&#8644; ${escape(lm.name(l))}</span>`).join("")}</div>
          ${desc ? `<div class="desc">${escape(desc)}</div>` : ""}
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
  const mappill = roots.mappill;
  const mapfullbtn = roots.mapfullbtn;
  const layersbtn = roots.layersbtn;
  const fareticket = roots.fareticket;
  const mapEl = roots.map;
  const mapdivider = roots.mapdivider;
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
  /* Dock: wall map on top + existing #chat below. Independent of map selection
     (the selection is what the dock points at). Cleared when leaving full. */
  let mapDock = !!(storage && storage.getItem(MAP_DOCK_KEY) === "1");
  /* Dock map-height fraction (0.30–0.75). Restored through clampDockFrac so a
     hand-edited "3" can never become a 300% map. Applied as --dockmap on body. */
  let dockFrac = (() => {
    const raw = storage && storage.getItem(MAP_DOCK_H_KEY);
    if (raw == null || raw === "") return DOCK_FRAC_DEFAULT;
    return clampDockFrac(Number(raw));
  })();
  /* Peek: chat collapsed to a title strip. Distinct from the fraction — leaving
     peek restores dockFrac / scimux-mapdockh, never a synthetic 0.95. Only
     meaningful under map-full.map-dock; cleared when either flag drops. */
  let dockDrag = null;
  let mapSel = "";
  let mapSelKey = "";
  let mapSelStop = "";
  /* Whether the station action bar is showing. Deliberately separate from the
     selection: a command dismisses the bar, the selection stays. */
  let mapBarOpen = false;
  /* Last wall selection key/row after a rebuild — used to anchor #mapscroll
     when a poll shifts the same selection's row index. Guard: only apply when
     mapSelKey === lastSelKey (a new selection's index delta is meaningless). */
  let lastSelKey = "";
  let lastSelRow = null;
  let mapSig = "";
  /* Last painted map-tabs signature (selected tab + group set + waiting
     counts). Live counts put #maptabs on the poll path; the gate keeps a
     steady bar from rewriting every tick. */
  let mapTabsSig = "";
  /* Last painted volatile frame, keyed by stop. Diffed on every skipped poll
     so liveness and the context gauge still move without a rebuild. */
  let wallVol = {};
  let editTab = null;
  let bound = false;
  const cleanups = [];
  const layersMenu = createPopoverMenu(doc);

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

  /* What was last written, so an unchanged fold set costs no storage write.
     localStorage.setItem is synchronous main-thread I/O and this runs on the
     render path, ahead of the signature guard, on every poll. */
  let foldWritten = null, foldKnownWritten = null;

  /* `model` is the caller's lane model. The render path already holds one;
     rebuilding it here is a second O(fleet) pass per tick (a third when a
     focused lane forces the second call). Omitted only by the handful of
     interaction paths that have none. */
  function saveMapFold(model){
    /* new journeys default folded (mapFoldKnown tracks every lane ever seen;
       mapFold records only the user's explicit open/closed choices). Prune lanes
       that no longer exist so a recreated id starts folded again. */
    if (uiLoaded() && (g("nodes", []) || []).length){
      const live = new Set((model || lm()).lanes.map(l => l.id));
      const next = applyNewLaneFolds(mapFold, mapFoldKnown, live);
      mapFold = next.mapFold;
      mapFoldKnown = next.mapFoldKnown;
    }
    const fold = JSON.stringify([...mapFold]);
    if (fold !== foldWritten){ storeSet(MAP_FOLD_KEY, fold); foldWritten = fold; }
    const known = JSON.stringify([...mapFoldKnown]);
    if (known !== foldKnownWritten){ storeSet(MAP_FOLD_KNOWN_KEY, known); foldKnownWritten = known; }
  }

  function setMapSel(id, key, stopTime){
    if (stationTapAction({ id, key, barOpen: mapBarOpen, mapSel, mapSelKey }) === "reopen"){
      mapBarOpen = true;
      renderMapToolbar();
      return;
    }
    const next = toggleMapSelection({ id, key, stopTime, mapSelKey, nodes: g("nodes", []) });
    mapSel = next.mapSel;
    mapSelKey = next.mapSelKey;
    mapSelStop = next.mapSelStop;
    mapBarOpen = !!next.mapSel;
    mapSig = "";
    renderMap();
  }

  /* Dismiss the bar without touching the selection (see stationTapAction). */
  function closeMapBar(){
    if (!mapBarOpen) return;
    mapBarOpen = false;
    renderMapToolbar();
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
    /* Leaving full screen also leaves the dock — the dock is only meaningful
       under body.map-full, and the CSS selector is the conjunction. */
    if (!mapFull){
      try { layersMenu.close(); } catch { /* ignore */ }
      mapDock = false;
      storeRemove(MAP_DOCK_KEY);
      if (doc && doc.body) doc.body.classList.toggle("map-dock", false);
    }
    const cleared = clearMapSelection();
    mapSel = cleared.mapSel; mapSelKey = cleared.mapSelKey; mapSelStop = cleared.mapSelStop;
    mapBarOpen = false;
    if (doc && doc.body) doc.body.classList.toggle("map-full", mapFull);
    syncMapFullBtn();
    mapSig = "";
    renderMap();
    renderMapToolbar();
  }

  /* Enter/leave the dock under an already-full wall map. Must NOT clear the
     map selection — the selection is what the docked chat is pointed at. */
  function setMapDock(on){
    mapDock = !!on;
    if (mapDock) storeSet(MAP_DOCK_KEY, "1");
    else storeRemove(MAP_DOCK_KEY);
    if (doc && doc.body) doc.body.classList.toggle("map-dock", mapDock);
    renderMapToolbar();
    /* The dock hands half the map's height to the chat, and that height is the
       pill's only input — so the verdict painted before the dock is stale the
       moment it opens. Cheap: pure geometry plus a few writes to the #mappill
       singleton, never a #mapwrap rebuild, so poll safety is untouched.
       Reading clientHeight here flushes layout, so the post-dock height is the
       one measured. */
    positionMapPill();
  }

  function dividerEl(){
    return mapdivider || (doc && doc.querySelector && doc.querySelector("#mapdivider"));
  }
  function appEl(){
    return roots.app || (doc && doc.querySelector && doc.querySelector("#app"));
  }

  /* Write --dockmap on body. CSS variable only — never renderMap, never mapSig.
     That is the poll-safety guarantee: resizing must not rebuild #mapwrap or
     disturb the composer. Differ from notes dividers only in axis (clientY /
     row-resize / horizontal pill) and in writing one fraction rather than two
     column widths. */
  function applyDockFrac(frac, { persist = false } = {}){
    dockFrac = clampDockFrac(frac);
    const body = doc && doc.body;
    if (body && body.style && typeof body.style.setProperty === "function"){
      /* Whole percents: nudge is 5% and the CSS var is a flex-basis percentage. */
      body.style.setProperty("--dockmap", Math.round(dockFrac * 100) + "%");
    }
    if (persist) storeSet(MAP_DOCK_H_KEY, String(dockFrac));
    /* Still no re-render — the split only moves the pill's viewport, which is
       chrome. rAF-throttled, so a drag costs one reposition per frame. */
    scheduleMapChrome();
  }

  /* ---- P2: dock divider — same handler shape as notes.js onDivider* ----
     P2a: no-op when !isDesktop (notes.js uses isNarrow the same way). CSS
     already hides the control on phone; the guard stops --dockmap writes if
     focus somehow reaches the element. */
  function onDockDividerPointerDown(e){
    if (!isDesktop()) return;
    const handle = dividerEl();
    if (!handle) return;
    const t = e.target;
    if (t && t !== handle && !(t.closest && t.closest("#mapdivider"))) return;
    if (e.button != null && e.button !== 0) return;
    if (typeof e.preventDefault === "function") e.preventDefault();
    const app = appEl();
    const rect = app && typeof app.getBoundingClientRect === "function"
      ? app.getBoundingClientRect()
      : { top: 0, height: 0 };
    dockDrag = {
      pointerId: e.pointerId,
      startY: e.clientY || 0,
      appTop: rect.top || 0,
      appHeight: rect.height || 0,
      moved: false,
      handle,
    };
    if (handle.classList) handle.classList.add("dragging");
    if (typeof handle.setPointerCapture === "function" && e.pointerId != null){
      try { handle.setPointerCapture(e.pointerId); } catch { /* ignore */ }
    }
  }
  function onDockDividerPointerMove(e){
    if (!dockDrag) return;
    if (dockDrag.pointerId != null && e.pointerId != null && e.pointerId !== dockDrag.pointerId)
      return;
    const y = e.clientY || 0;
    if (Math.abs(y - dockDrag.startY) > 2) dockDrag.moved = true;
    const raw = dockFracFromDrag({
      clientY: y,
      appTop: dockDrag.appTop,
      appHeight: dockDrag.appHeight,
    });
    applyDockFrac(raw, { persist: false });
  }
  function onDockDividerPointerUp(e){
    if (!dockDrag) return;
    if (dockDrag.pointerId != null && e.pointerId != null && e.pointerId !== dockDrag.pointerId)
      return;
    /* Persist on pointerup only — not per move (same as notes wsdivider). */
    if (dockDrag.moved) applyDockFrac(dockFrac, { persist: true });
    const handle = dockDrag.handle;
    if (handle && handle.classList) handle.classList.remove("dragging");
    if (handle && typeof handle.releasePointerCapture === "function" && dockDrag.pointerId != null){
      try { handle.releasePointerCapture(dockDrag.pointerId); } catch { /* ignore */ }
    }
    dockDrag = null;
  }
  function onDockDividerKeydown(e){
    if (!isDesktop()) return;
    const handle = dividerEl();
    if (!handle) return;
    const t = e.target;
    if (t && t !== handle && !(t.closest && t.closest("#mapdivider"))) return;
    if (e.key !== "ArrowUp" && e.key !== "ArrowDown") return;
    if (typeof e.preventDefault === "function") e.preventDefault();
    /* ArrowUp moves the separator up → smaller map; ArrowDown → larger map,
       each clamped by applyDockFrac. */
    const delta = e.key === "ArrowUp" ? -DOCK_FRAC_NUDGE : DOCK_FRAC_NUDGE;
    applyDockFrac(dockFrac + delta, { persist: true });
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
    const gs = groups();
    const counts = mapTabAttentionCounts(g("nodes", []), gs);
    const sig = mapTabsSignature(mapTab, gs, counts);
    if (mapRenderDecision(sig, mapTabsSig) === "skip") return;
    mapTabsSig = sig;
    maptabs.innerHTML = mapTabsHTML(mapTab, gs, { escape, counts });
  }

  function renderLaneChips(model, inGroup, focusLane){
    if (!lanechips) return;
    lanechips.innerHTML = model.lanes.filter(l => inGroup(l.id)).map(l => `
    <button class="lanechip ${focusLane === l.id ? "selected" : ""}"
          style="${laneChipStyle(model.color(l.id), focusLane === l.id, { escape, contrastText: contrastFn })}"
          data-chip="${escape(l.id)}">${escape(l.name || l.id)}</button>`).join("");
  }

  function stationHTML(n, model, opts){
    const forkCue = opts.fork ? forkCueHTML(n, model, {
      escape, forkKind, nodeById,
    }) : "";
    const agentLogo = typeof d.agentLogo === "function" ? d.agentLogo(n.agent) : "";
    return stationRowHTML(n, model, {
      ...opts,
      escape, stamp: stampFn, status: statusText, configText: cardConfigText,
      forkCue, agentLogo,
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

  /* rAF-throttled chrome reposition: toolbar + pill only (no server call).
     Driven by scroll, by window resize, and by the dock divider — the drag
     rewrites the map's height on every pointermove, and the pill reads that
     height. */
  let scrollRaf = 0;
  function scheduleMapChrome(){
    if (scrollRaf) return;
    const raf = (win && win.requestAnimationFrame)
      || (typeof requestAnimationFrame !== "undefined" ? requestAnimationFrame : null);
    if (!raf){
      positionMapToolbar();
      positionMapPill();
      return;
    }
    scrollRaf = raf(() => {
      scrollRaf = 0;
      positionMapToolbar();
      positionMapPill();
    });
  }

  function onMapResize(){
    positionMapToolbar();
    positionMapPill();
  }

  function renderMapToolbar(){
    const bar = maptoolbar;
    if (!bar) return;
    const n = (mapFull && mapSel && mapBarOpen) ? nodeById(mapSel) : null;
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
    <button class="sbtn iconbtn" data-jump="${escape(n.id)}">${icons.ICON_JUMP || ""} Open chat</button>
    <button class="sbtn iconbtn" data-medit="${escape(n.id)}" aria-label="edit title & description">${icons.ICON_PENCIL || ""} Edit</button>
    <button class="sbtn" data-forkbookmark="${escape(n.id)}">Add bookmark</button>
    <button class="sbtn" data-forkfrom="${escape(n.id)}">+ New</button>
    ${n.ended_at ? "" : `<button class="sbtn iconbtn" data-mapexit="${escape(n.id)}" aria-label="close thread">${icons.ICON_END || ""} End</button>`}`;
    positionMapToolbar();
  }

  /* Last wall-tick geometry for the off-screen attention pill. Recomputed on
     each wall paint; positionMapPill re-reads scroll/viewport only. */
  let pillTicks = [];
  let pillContentH = 0;
  const WALL_OFF = 16;

  function clearMapPill(){
    pillTicks = [];
    pillContentH = 0;
    if (!mappill) return;
    mappill.classList.toggle("on", false);
    mappill.textContent = "";
    mappill.dataset.nid = "";
    mappill.dataset.skey = "";
    if (typeof mappill.removeAttribute === "function"){
      mappill.removeAttribute("aria-label");
    }
  }

  /* Open chat at a station — shared by the toolbar "Open chat" button and the
     attention-ring hit circle. stop empty → head (jumpChatToNow). */
  function openStationChat(id, stop){
    if (!id) return;
    if (typeof d.selectNode === "function") d.selectNode(id, "jump");
    if (stop){
      if (typeof d.loadChatHistory === "function") d.loadChatHistory(id, stop);
    } else if (typeof d.jumpChatToNow === "function") d.jumpChatToNow();
    if (mapFull) setMapDock(true);
    else setMapFull(false);
  }

  function positionMapPill(){
    if (!mappill) return;
    if (!mapFull || !mapscroll){
      mappill.classList.toggle("on", false);
      mappill.textContent = "";
      return;
    }
    const decision = attentionPill({
      ticks: pillTicks,
      scrollTop: mapscroll.scrollTop || 0,
      viewportH: mapscroll.clientHeight || 0,
      contentH: pillContentH,
    });
    mappill.classList.toggle("on", !!decision.show);
    /* Hue follows the word: --attn only when the pill is about stations that
       are blocked on you (P5 reserves yellow for exactly that). */
    mappill.classList.toggle("ready", !!decision.show && decision.kind === "ready");
    if (!decision.show){
      mappill.textContent = "";
      mappill.dataset.nid = "";
      mappill.dataset.skey = "";
      return;
    }
    const arrow = decision.dir === "up" ? "↑" : "↓";
    mappill.textContent = `${decision.label} ${arrow}`;
    mappill.dataset.nid = decision.nodeId || "";
    mappill.dataset.skey = decision.stopKey || "";
    if (typeof mappill.setAttribute === "function"){
      mappill.setAttribute("aria-label", `Jump to ${decision.label} ${arrow}`);
    }
  }

  function updateMapPill(rows){
    const list = rows || [];
    pillTicks = railTicks({ rows: list, rowHeight: WALL_ROW_H, offset: WALL_OFF });
    pillContentH = WALL_OFF + list.length * WALL_ROW_H;
    positionMapPill();
  }

  /* The skipped-poll path. Nothing about the wall's shape changed, so only the
     stations whose liveness or gauge moved get touched — attribute and text
     writes on nodes that are already painted, no markup re-parsed. */
  function patchWallVolatile(rows){
    const next = wallVolatile(rows);
    const moved = volatileDiff(wallVol, next);
    if (!moved.length) return;
    if (mapwrap)
      moved.forEach(k => patchStationVolatile(mapwrap, k, headNodeAt(rows, k), {
        cssEscape: s => CSSRef.escape(s),
      }));
    wallVol = next;
  }

  const headNodeAt = (rows, key) => {
    const s = (rows || []).find(r => r && r.head && stopKey(r) === key);
    return s ? s.n : null;
  };

  function renderWallMap(model, grp, inGroup, served, stations, focusLane){
    const colLanes = model.lanes.filter(l => inGroup(l.id));
    const cols = laneColumnOrder(colLanes, stations, model.byId);
    const colIdx = {}; cols.forEach((id, c) => colIdx[id] = c);
    const rows = stations.flatMap(stopsOf).sort(newestFirst);

    const sig = wallMapSignature(rows, cols, {
      focusLane, mapTab, mapSel, mapSelKey, fareOn,
    });
    if (mapRenderDecision(sig, mapSig) === "skip") return patchWallVolatile(rows);
    mapSig = sig;
    /* The frame the next patch diffs against. Recorded before the rebuild,
       because the markup below is drawn from exactly this state. */
    wallVol = wallVolatile(rows);

    const RH = WALL_ROW_H, CW = 46, MX = 24, OFF = 16, GAP = 16;
    const nCols = cols.length, nRows = rows.length;
    const colX = c => MX + c * CW;
    const rowY = i => OFF + i * RH + RH / 2;
    let colsW = MX + Math.max(0, nCols - 1) * CW + MX;
    const svgH = OFF + nRows * RH;
    const dimLane = id => focusLane && id !== focusLane;
    const dimNode = n => focusLane && n.lane_id !== focusLane;

    if (!nRows){
      if (mapwrap) mapwrap.innerHTML = `<div class="empty">No journeys${grp ? " on this map" : ""} yet.</div>`;
      /* Empty wall: drop anchor state so a later refill with the same stop
         selected does not apply a stale row-index delta (P4 edge). */
      lastSelKey = "";
      lastSelRow = null;
      clearMapPill();
      renderLaneChips(model, inGroup, focusLane);
      return;
    }

    const rowIdx = {}; rows.forEach((s, i) => rowIdx[stopKey(s)] = i);
    /* Capture scroll + prior row before the DOM write (innerHTML can reset
       scrollTop). Applied only when the selection key is unchanged. */
    const prevTop = mapscroll ? mapscroll.scrollTop : 0;
    const prevKey = lastSelKey;
    const prevIndex = lastSelRow;
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

    // Per-line station points (y + stop) so heat can encode each inter-station
    // gap from fare_segments. Mainline vs y-stay branch (x offset) stay separate.
    const touchStops = {}; cols.forEach(id => touchStops[id] = []);
    const branchStops = {};
    rows.forEach((s, i) => {
      if (colIdx[s.n.lane_id] == null) return;
      const pt = { y: rowY(i), stop: s };
      if (xOff[s.n.id]) (branchStops[s.n.id] = branchStops[s.n.id] || []).push(pt);
      else touchStops[s.n.lane_id].push(pt);
    });

    let svg = ATTN_GLOW_DEF;
    // Wall is already mapFull; fareOn is the layer toggle (heatEnabled).
    cols.forEach(id => {
      const pts = touchStops[id];
      if (!pts || pts.length < 2) return;
      const x = colX(colIdx[id]);
      svg += wallLaneTrackSVG(pts, {
        x,
        color: escape(model.color(id)),
        opacity: dimLane(id) ? .22 : 1,
        heatOn: wallHeatOn(fareOn),
      });
    });
    Object.keys(branchStops).forEach(id => {
      const pts = branchStops[id], n = model.byId[id];
      if (!pts || pts.length < 2 || !n) return;
      const x = nodeX(n);
      svg += wallLaneTrackSVG(pts, {
        x,
        color: escape(model.color(n.lane_id)),
        opacity: dimNode(n) ? .22 : 1,
        heatOn: wallHeatOn(fareOn),
      });
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
      // Widen the existing ring path: hard attention keeps --attn; a finished
      // turn reuses the same SVG with --work. No parallel paint path (P5).
      // Neutral inspect does not ring (fixes-2 P1). Hit target is emitted AFTER
      // the solid dot (and ctx) so the centre is tappable — SVG paints later
      // siblings on top (P4).
      const ringed = !!(hardAttention(n) || turnFinished(n));
      if (hardAttention(n)) svg += attentionStationSVG(dotX, yy, op, null);
      else if (turnFinished(n)) svg += attentionStationSVG(dotX, yy, op, "var(--work)");
      /* data-dot / data-ctx are the repaint hooks. Only the live branch
         carries one: exited and unavailable are in the signature, so reaching
         them rebuilds rather than patches. */
      const skey = escape(stopKey(s));
      if (n.live === "exited" || n.live === "unavailable")
        svg += `<circle cx="${dotX}" cy="${yy}" r="5.5" fill="var(--bg)" stroke="${col}" stroke-width="2.5" opacity="${op * .55}"/>`;
      else
        svg += `<circle data-dot="${skey}" cx="${dotX}" cy="${yy}" r="${n.live === "active" ? 6.5 : 5.5}" fill="${col}" opacity="${op}"/>`;
      if (n.ctx_pct != null && !hardAttention(n) && !turnFinished(n)){
        const R = CTX_RING_R, C = 2 * Math.PI * R, frac = Math.max(0, Math.min(1, n.ctx_pct / 100));
        svg += `<circle cx="${dotX}" cy="${yy}" r="${R}" fill="none" stroke="${col}" stroke-width="2" opacity="${op * .2}"/>`;
        /* Emitted even at 0% — an invisible arc the patch can grow, since no
           attribute write can create a node that was never drawn. data-col
           remembers the lane colour the patch falls back to below 80%. */
        svg += `<circle data-ctx="${skey}" data-col="${col}" cx="${dotX}" cy="${yy}" r="${R}" fill="none"
                stroke="${n.ctx_pct >= 80 ? "var(--attn)" : col}" stroke-width="2"
                stroke-dasharray="${frac * C} ${C}" stroke-linecap="round"
                transform="rotate(-90 ${dotX} ${yy})" opacity="${op}"/>`;
      }
      if (ringed) svg += attentionHitSVG(dotX, yy, n.id);
    });

    /* V2-P5: a capsule on the midpoint of every priced gap — the fare layer
       is a map legend, not a selection detail. Tapping one opens the ticket
       sheet (progressive disclosure moved out of the clipped anchor). */
    let capsuleHTML = "";
    if (fareOn){
      const spots = [];
      cols.forEach(id => {
        const pts = touchStops[id];
        if (pts) spots.push(...wallCapsuleSpots(pts, colX(colIdx[id])));
      });
      Object.keys(branchStops).forEach(id => {
        const n = model.byId[id];
        if (n) spots.push(...wallCapsuleSpots(branchStops[id], nodeX(n)));
      });
      capsuleHTML = spots.map(sp => fareCapsuleHTML(sp.seg, {
        x: sp.x, y: sp.y, nodeId: sp.nodeId, segIdx: sp.segIdx, escape,
      })).join("");
    }

    if (mapwrap) mapwrap.innerHTML = `<div class="lbody wallbody">
    <svg width="${colsW}" height="${svgH}" viewBox="0 0 ${colsW} ${svgH}">${svg}</svg>
    ${capsuleHTML}
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
    /* Anchor only when the same stop stayed selected. A new mapSelKey's
       prev/next indices describe different stops — applying the delta yanks
       the viewport on every tap. Missing mapscroll is a no-op (no throw). */
    const nextIndex = mapSelKey && Object.prototype.hasOwnProperty.call(rowIdx, mapSelKey)
      ? rowIdx[mapSelKey]
      : null;
    if (mapscroll && mapSelKey === prevKey){
      mapscroll.scrollTop = anchoredScrollTop({
        prevTop, prevIndex, nextIndex, rowHeight: WALL_ROW_H,
      });
    }
    lastSelKey = mapSelKey;
    lastSelRow = nextIndex;
    renderLaneChips(model, inGroup, focusLane);
    renderMapToolbar();
    /* Pill follows the wall paint: signature already includes attention and
       turn_done, so a ring appearing/disappearing rebuilds (not a volatile
       patch) and the hit circle + pill ticks stay in lockstep. */
    updateMapPill(rows);
  }

  function renderMap(){
    if (!mapVisible()) return;
    /* Live tab badges: refresh before the wall/stack signature gate so a poll
       that only changes attention still updates counts even when the map body
       itself skips. Internally gated on mapTabsSig. */
    renderMapTabs();
    const model = lm();
    const grp = groupLanes(mapTab, groups());
    const inGroup = laneID => !grp || grp.includes(laneID);
    const served = servedLanes;
    const stations = (g("nodes", []) || []).filter(n => n.lane_id && served(n).some(inGroup));
    const focusLane = laneFilter() || null;
    if (mapFull && isDesktop()) return renderWallMap(model, grp, inGroup, served, stations, focusLane);

    let blocks = buildStackBlocks(model.lanes, stations, { served, inGroup });
    saveMapFold(model);
    if (focusLane && mapFold.has(focusLane)){
      mapFold = unfoldFocusLane(mapFold, focusLane);
      saveMapFold(model);
    }

    const sig = stackMapSignature(blocks, {
      mapFold, focusLane, mapTab, mapFull, lm: model,
    });
    if (mapRenderDecision(sig, mapSig) === "skip") return;
    mapSig = sig;
    /* Stack map has no attention pill — clear so a prior wall session cannot
       leave a floating "N waiting" over the column map. */
    clearMapPill();

    const RH = 76, LX = 22, LW = 56;
    const y = i => i * RH + RH / 2;
    const dimRow = n => focusLane && !served(n).includes(focusLane);
    const hasAttn = b => b.rows.some(s => hardAttention(s.n)) ? 1 : 0;

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
        // Hit after the solid dot (P4) — same paint-order rule as the wall.
        // Neutral inspect does not ring (fixes-2 P1).
        const ringed = !!(hardAttention(n) || turnFinished(n));
        if (hardAttention(n)) svg += attentionStationSVG(dotX, yy, op, null);
        else if (turnFinished(n)) svg += attentionStationSVG(dotX, yy, op, "var(--work)");
        if (n.live === "exited" || n.live === "unavailable")
          svg += `<circle cx="${dotX}" cy="${yy}" r="5.5" fill="var(--bg)" stroke="${col}" stroke-width="2.5" opacity="${op * .55}"/>`;
        else
          svg += `<circle cx="${dotX}" cy="${yy}" r="${n.live === "active" ? 6.5 : 5.5}" fill="${col}" opacity="${op}"/>`;
        if (ringed) svg += attentionHitSVG(dotX, yy, n.id);
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

  function openFareTicket(nodeId, segIdx){
    const n = nodeById(nodeId);
    const ctx = fareTicketContext(n, Number(segIdx), { timeLabel: stampFn });
    if (!ctx) return;
    if (fareticket) fareticket.innerHTML = fareTicketHTML(ctx, { escape });
    if (typeof d.openSheet === "function") d.openSheet("#faresheet");
  }

  function onMapWrapClick(e){
    /* V2-P5: capsule tap opens the fare ticket sheet. Handled before station
       rows so the button does not also re-toggle mapSel — and note it does
       *not* touch mapSig: the sheet is outside the polled render region, so
       the wall is never rebuilt (nor the composer disturbed) by a tap. */
    const cap = e.target.closest("[data-fare-capsule]");
    if (cap){
      if (typeof e.stopPropagation === "function") e.stopPropagation();
      if (typeof e.preventDefault === "function") e.preventDefault();
      openFareTicket(cap.dataset.fareNode, cap.dataset.fareSeg);
      return;
    }
    const f = e.target.closest("[data-fold]");
    if (f){ toggleMapFold(f.dataset.fold); return; }
    /* P6: ring hit circle (and any other data-jump in the wrap) opens chat —
       before the generic [data-nid] row branch so the ring wins over the row.
       Two closest() calls (not a comma selector) so fakes and older engines
       that only match a single simple selector still route correctly. */
    const j = e.target.closest(".attnstation-hit") || e.target.closest("[data-jump]");
    if (j){
      openStationChat(j.dataset.jump, j.dataset.stop || "");
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

  function onMapPillClick(e){
    /* Pill locates (select + scrollIntoView); the ring opens. Human tap, so
       smooth scroll is fine — P4's no-animation fence is about poll ticks. */
    const t = e.currentTarget || e.target;
    const nid = (t && t.dataset && t.dataset.nid) || "";
    const skey = (t && t.dataset && t.dataset.skey) || "";
    if (!nid) return;
    if (mapFull) setMapSel(nid, skey, "");
    const row = mapwrap && mapwrap.querySelector(
      skey
        ? `.strow[data-skey="${CSSRef.escape(skey)}"]`
        : `.strow[data-nid="${CSSRef.escape(nid)}"]`,
    );
    if (row && typeof row.scrollIntoView === "function")
      row.scrollIntoView({ behavior: "smooth", block: "center" });
  }

  function onToolbarClick(e){
    const j = e.target.closest("[data-jump]");
    if (j){
      openStationChat(j.dataset.jump, mapSelStop);
      closeMapBar();
      return;
    }
    const pe = e.target.closest("[data-medit]");
    if (pe){
      if (typeof d.openActivityEditor === "function") d.openActivityEditor(pe.dataset.medit, mapSelStop || "");
      closeMapBar();
      return;
    }
    const nt = e.target.closest("[data-forkbookmark]");
    if (nt){
      if (typeof d.addStationBookmark === "function")
        d.addStationBookmark(nt.dataset.forkbookmark, mapSelStop);
      closeMapBar();
      return;
    }
    const fk = e.target.closest("[data-forkfrom]");
    if (fk){
      if (typeof d.forkFromStation === "function") d.forkFromStation(fk.dataset.forkfrom);
      closeMapBar();
      return;
    }
    const ex = e.target.closest("[data-mapexit]");
    if (ex){
      if (typeof d.exitThread === "function") d.exitThread(ex.dataset.mapexit);
      closeMapBar();
      return;
    }
  }

  function onMapFullClick(){ setMapFull(!mapFull); }

  function syncLayersExpanded(){
    if (!layersbtn || typeof layersbtn.setAttribute !== "function") return;
    layersbtn.setAttribute("aria-expanded", layersMenu.element() ? "true" : "false");
  }

  function syncLayersBtn(){
    if (!layersbtn) return;
    const icons = d.icons || {};
    // One stable layers glyph (no FA webfont — TestPinnedIcons). Fare state
    // lives on the menu item, not on this button.
    layersbtn.innerHTML = icons.ICON_LAYERS || "";
    if (typeof layersbtn.setAttribute === "function"){
      layersbtn.setAttribute("aria-label", "Layers");
      layersbtn.setAttribute("title", "Layers");
      layersbtn.setAttribute("aria-haspopup", "menu");
    }
    syncLayersExpanded();
  }

  function onLayersClose(){
    syncLayersExpanded();
  }

  function layersPanel(){
    if (mapEl) return mapEl;
    if (doc && typeof doc.querySelector === "function") return doc.querySelector("#map");
    return null;
  }

  function openLayersMenu(){
    if (!layersbtn) return;
    const m = layersMenu.open({
      panel: layersPanel(),
      anchor: layersbtn,
      trigger: layersbtn,
      html: layersMenuHTML({ fareOn, escape }),
      onClick: onLayersMenuClick,
      onClose: onLayersClose,
    });
    if (m && typeof m.setAttribute === "function") m.setAttribute("role", "menu");
    syncLayersExpanded();
  }

  function onLayersClick(){
    if (layersMenu.element()){
      layersMenu.close();
      return;
    }
    openLayersMenu();
  }

  function onLayersMenuClick(e){
    const t = e && e.target;
    const item = t && typeof t.closest === "function" ? t.closest("[data-layer=\"fare\"]") : null;
    if (!item) return;
    fareOn = !fareOn;
    if (fareOn) storeSet(MAP_FARE_KEY, "1");
    else storeRemove(MAP_FARE_KEY);
    layersMenu.close();
    mapSig = "";
    renderMap();
  }

  function onLayersDocClick(e){
    if (layersMenu.shouldCloseForClick(e && e.target, { exclude: "#layersbtn" }))
      layersMenu.close();
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
    on(mappill, "click", onMapPillClick);
    on(mapscroll, "scroll", scheduleMapChrome, { passive: true });
    if (win && typeof win.addEventListener === "function"){
      win.addEventListener("resize", onMapResize);
      cleanups.push(() => win.removeEventListener("resize", onMapResize));
    }
    on(mapfullbtn, "click", onMapFullClick);
    on(layersbtn, "click", onLayersClick);
    on(doc, "click", onLayersDocClick);
    on(tabSave, "click", onTabSave);
    on(tabDel, "click", onTabDel);
    /* Dock divider: pointer drag + keyboard nudge. Listeners go through
       cleanups so destroy() drops them (same pattern as every other map root). */
    const div = dividerEl();
    on(div, "pointerdown", onDockDividerPointerDown);
    on(div, "pointermove", onDockDividerPointerMove);
    on(div, "pointerup", onDockDividerPointerUp);
    on(div, "pointercancel", onDockDividerPointerUp);
    on(div, "keydown", onDockDividerKeydown);
    /* Restore the split on bind (clamped). No render — CSS var only. */
    applyDockFrac(dockFrac, { persist: false });
    if (doc && doc.body){
      if (mapDock) doc.body.classList.toggle("map-dock", true);
    }
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
    syncLayersBtn();
    syncMapFullBtn();
  }

  function destroy(){
    while (cleanups.length){
      try { cleanups.pop()(); } catch { /* ignore */ }
    }
    try { layersMenu.close(); } catch { /* ignore */ }
    bound = false;
  }

  function invalidate(){ mapSig = ""; }

  function restoreChrome(){
    if (mapFull && isDesktop() && doc && doc.body){
      doc.body.classList.add("map-full");
      if (mapDock) doc.body.classList.add("map-dock");
    }
    applyDockFrac(dockFrac, { persist: false });
    syncLayersBtn();
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
    setDock: setMapDock,
    isDock: () => mapDock,
    restoreChrome,
    scrollMapToTop,
  };
}
