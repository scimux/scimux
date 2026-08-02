/* Pure activity-card ordering/status and wall-map selection decisions.
 *
 * Packet 6D: DOM-free copy of the card/map-selection subset currently inlined
 * in web/index.html. Named exports are exercised by Node tests only; production
 * still uses the inline helpers until Packet 6F wires the sole module entry.
 *
 * Explicit inputs (no UI / nodes / sel / laneFilter / mapSel globals):
 * - node collections are passed as `nodes`
 * - pinned order as `pinned` (array of ids)
 * - archived ids as `archived`
 * - lane scope as `laneFilter` and selected chat as `selectedId`
 * - card tab as `cardTab` ("current" | "archived" | "pinned")
 * - map selection toggle takes current mapSelKey + optional stopTime/key
 *
 * Reuses stopTimes (and servedLanes for lane scope) from lanes.js — do not
 * duplicate stop-time derivation here.
 *
 * No document, window, fetch, localStorage, navigator, timers, or app state.
 * No card HTML/time rendering, renderCards, renderMap, or DOM mutation.
 */

import { stopTimes, servedLanes } from "./lanes.js";

/* ---------- attention / card state / status text ---------- */

/* "inspect" is the neutral attention: quiet pane, no structured transcript
   evidence — worth a look, but never the yellow "needs your decision" alarm */
export const hardAttention = n => n.attention && n.attention !== "inspect";

export function cardState(n){
  // A deliberately closed thread (/exit) is an immutable dead-end: it takes
  // precedence over mechanical liveness so a closed-but-adopted or
  // close-failed thread never renders as working/dead, and never shows a
  // workdot or attention glow on the card.
  if (n.ended_at) return "closed";
  if (hardAttention(n)) return "attention";
  if (n.live === "active") return "working";
  if (n.live === "exited" || n.live === "unavailable") return "dead";
  return "idle";
}

export function statusText(n){
  // Closed is a deliberate, durable cap — it wins over mechanical liveness and
  // any lingering attention flag.
  if (n.ended_at) return "Closed";
  if (n.attention === "inspect") return "Quiet · check the terminal";
  if (n.attention) return `Waiting · needs your ${n.attention}`;
  if (n.live === "active") return "Running";
  if (n.live === "exited") return "Exited";
  if (n.live === "unavailable") return "Unavailable";
  return "Quiet";
}

/* ---------- recency helpers used by ordering ---------- */

export function cardCreatedMS(n){
  const v = Date.parse(n.created_at || "");
  return isNaN(v) ? 0 : v;
}

export function cardInteractionMS(n){
  return n.last_interaction || 0;
}

/* The Activities list is tiered by liveness, then by recency within a tier.
   Liveness — not last_interaction — must decide the tier: last_interaction is
   the last *human* turn, so a chat where the agent has churned for minutes
   since you last typed would otherwise sink below an idle chat you touched more
   recently (the "active chats in the middle" bug). Tiers mirror cardState's
   precedence exactly (closed > attention > active > dead > idle) so a card's
   position and its rendered state never disagree. */
export function livenessTier(n){
  if (n.ended_at) return 4;                                       // closed — immutable dead-end, sinks
  if (hardAttention(n)) return 0;                                 // needs your decision — top
  if (n.live === "active") return 1;                              // working now
  if (n.live === "exited" || n.live === "unavailable") return 3;  // dead — sinks with closed
  return 2;                                                       // idle / quiet / fresh
}

/* ---------- ordering ---------- */

export function orderedNodes(nodes){
  return [...(nodes || [])].sort((a, b) => {
    const ta = livenessTier(a), tb = livenessTier(b);
    if (ta !== tb) return ta - tb;
    const freshA = !(a.last_activity || 0), freshB = !(b.last_activity || 0);
    if (freshA !== freshB) return freshA ? -1 : 1;
    if (freshA && freshB) return cardCreatedMS(b) - cardCreatedMS(a);
    return cardInteractionMS(b) - cardInteractionMS(a) ||
      (b.last_activity || 0) - (a.last_activity || 0) ||
      cardCreatedMS(b) - cardCreatedMS(a);
  });
}

/* Pinned-tab order: hard-attention ("needs you") cards float to the very top —
   supervision precedence still wins in this view — then the user's pin order
   (pinned index; a new pin was unshifted to the front, so it lands topmost).
   Drag reorder rewrites pinned, so the array index IS the manual order. */
export function pinnedOrder(list, pinned){
  const order = pinned || [];
  return [...(list || [])].sort((a, b) => {
    const aa = hardAttention(a) ? 1 : 0, ba = hardAttention(b) ? 1 : 0;
    if (aa !== ba) return ba - aa;
    return order.indexOf(a.id) - order.indexOf(b.id);
  });
}

/* ---------- visibility (current / archived / pinned + lane scope) ---------- */

export function isArchived(id, archived){
  return (archived || []).includes(id);
}

export function isPinned(id, pinned){
  return (pinned || []).includes(id);
}

/* Tab membership before lane-scope filtering.
   Current: non-archived, plus archived nodes that hard-attention (surface
   pending decisions — archiving must never hide an approval/question).
   Archived: archived only.
   Pinned: pinned ids only, attention-first then pin order (lane-agnostic). */
export function tabListForCards(nodes, { cardTab, pinned, archived } = {}){
  const list = nodes || [];
  if (cardTab === "pinned"){
    return pinnedOrder(list.filter(n => isPinned(n.id, pinned)), pinned);
  }
  return orderedNodes(list).filter(n =>
    isArchived(n.id, archived) === (cardTab === "archived") ||
    (cardTab === "current" && hardAttention(n)));
}

/* A lane's scope is every thread on that lane (single membership) plus the
   open chat, which always shows. Empty laneFilter = all lanes in scope. */
export function inLaneScope(n, { laneFilter, selectedId } = {}){
  return !laneFilter || servedLanes(n).includes(laneFilter) || n.id === selectedId;
}

/* Visible main list + optional "Input needed" fold for out-of-scope hard
   attention on the current tab. Pinned ignores lane scope. Invariant: nothing
   that needs input is invisible — it is either in list or in fold. */
export function visibleCardLists(nodes, opts = {}){
  const { cardTab, laneFilter } = opts;
  const tabList = tabListForCards(nodes, opts);
  const list = cardTab === "pinned"
    ? tabList
    : tabList.filter(n => inLaneScope(n, opts));
  const foldList = laneFilter && cardTab === "current"
    ? tabList.filter(n => !inLaneScope(n, opts) && hardAttention(n))
    : [];
  return { tabList, list, foldList };
}

/* ---------- wall-map stop-scoped selection ---------- */

/* Head stop key for a node id. Missing node → id#0 (matches inline
   headStopKey when nodeById returns undefined). */
export function headStopKey(id, nodes){
  const n = (nodes || []).find(x => x.id === id);
  return id + "#" + (n ? stopTimes(n).length - 1 : 0);
}

/* Pure toggle for wall-map selection. Returns the next { mapSel, mapSelKey,
   mapSelStop } without DOM/render side effects.
   - missing/empty key falls back to headStopKey(id, nodes)
   - second tap on the same key deselects and clears stop time
   - selecting keeps stopTime only when truthy (head row → "")
   - mapSel is the node id (toolbar); mapSelKey is stop-scoped (nid#i) */
export function toggleMapSelection({ id, key, stopTime, mapSelKey, nodes } = {}){
  const resolvedKey = key || headStopKey(id, nodes);
  let nextSel, nextKey;
  if (mapSelKey === resolvedKey){
    nextSel = "";
    nextKey = "";
  } else {
    nextSel = id;
    nextKey = resolvedKey;
  }
  const mapSelStop = nextSel ? (stopTime || "") : "";
  return { mapSel: nextSel, mapSelKey: nextKey, mapSelStop };
}
