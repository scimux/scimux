/* Pure activity-card ordering/status and wall-map selection decisions.
 *
 * Extracted in Packet 6D and wired into the sole browser entry in Packet 6F.
 * Named exports are exercised directly by Node tests and used by production.
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

/* turn_done is a server-projected flag (quiet + assistant delivered + no
   pending tool call). Absent means UNKNOWN — ACP nodes never set it and must
   render as plain Quiet, not as a failed finished claim. */
export const turnFinished = n => !!(n && n.turn_done);

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

/* One precedence expression for the map status word and its class, so the
   word and st-${kind} can never disagree. Keys:
   closed | attention | inspect | running | finished | exited | unavailable | quiet.
   "Ready" is honest only when turn_done is provable (item 10); plain Quiet
   stays Quiet — it covers a finished-unknown, a crash, and a silent wait. */
function statusInfo(n){
  const node = n || {};
  if (node.ended_at) return { kind: "closed", text: "Closed" };
  if (node.attention === "inspect") return { kind: "inspect", text: "Quiet · check the terminal" };
  if (node.attention) return { kind: "attention", text: `Waiting · needs your ${node.attention}` };
  if (node.live === "active") return { kind: "running", text: "Running" };
  if (node.live === "exited") return { kind: "exited", text: "Exited" };
  if (node.live === "unavailable") return { kind: "unavailable", text: "Unavailable" };
  if (turnFinished(node)) return { kind: "finished", text: "Ready" };
  return { kind: "quiet", text: "Quiet" };
}

export function statusKind(n){
  return statusInfo(n).kind;
}

export function statusText(n){
  return statusInfo(n).text;
}

/* ---------- recency helpers used by ordering ---------- */

export function cardCreatedMS(n){
  const v = Date.parse(n.created_at || "");
  return isNaN(v) ? 0 : v;
}

/* Last human interaction, falling back to created_at for synthetic/malformed
   nodes that lack last_interaction. The state API already projects a creation
   or page-turn fallback for real nodes. */
export function cardInteractionMS(n){
  return (n && n.last_interaction) || cardCreatedMS(n) || 0;
}

/* ---------- ordering ---------- */

/* Current/All Activities order (fixes-2 P2):
   1. hard attention first (hardAttention — never inspect)
   2. newest last_interaction
   3. newest last_activity as a deterministic tie-breaker
   4. newest created_at as the final tie-breaker
   turn_done / active / quiet / exited / closed are visible on cards but are
   not separate sort tiers — they must not outrank the user's latest touch. */
export function orderedNodes(nodes){
  return [...(nodes || [])].sort((a, b) => {
    const aa = hardAttention(a) ? 1 : 0, ba = hardAttention(b) ? 1 : 0;
    if (aa !== ba) return ba - aa;
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

/* ---------- send-to targets (chat bubble / bookmark → another chat) ---------- */

/* Who can still receive text: a chat with a process left to type into.
   Closed (/exit) is a deliberate, immutable dead end and dead liveness means
   the tmux session is gone — neither can ever accept a prompt again, so they
   are omitted from the picker rather than shown disabled. A node with no poll
   state yet (freshly created) is not provably dead and stays eligible. */
export function canReceiveSend(n){
  if (!n || n.ended_at) return false;
  return n.live !== "exited" && n.live !== "unavailable";
}

/* Destination list for the send-to sheet, as { pinned, recent }.
   Pinned targets keep the user's manual pin order — deliberately NOT
   pinnedOrder, whose hard-attention float is right for the Pinned tab
   (supervision triage) and wrong for a picker: a list that reshuffles the
   moment an agent asks a question moves the row out from under your finger.
   Everything else ranks by the last *human* turn (cardInteractionMS answers
   "where was I typing"), not last_activity (agent churn). With nothing
   pinned this degenerates to pure recency — one rule, not two.
   Lane-agnostic on purpose, exactly as the Pinned tab ignores lane scope:
   a target that appeared only under some journey selections would be
   inexplicable to the user. */
export function sendableNodes(nodes, { exceptId = "", pinned = [] } = {}){
  const eligible = (nodes || []).filter(n => n && n.id !== exceptId && canReceiveSend(n));
  const order = pinned || [];
  const isPin = n => order.indexOf(n.id) >= 0;
  return {
    pinned: eligible.filter(isPin)
      .sort((a, b) => order.indexOf(a.id) - order.indexOf(b.id)),
    recent: eligible.filter(n => !isPin(n))
      .sort((a, b) => cardInteractionMS(b) - cardInteractionMS(a) ||
        cardCreatedMS(b) - cardCreatedMS(a)),
  };
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

/* Which turn a wall-map station bookmark should capture (UI review item 13).
   The map knows a node id and, for an earlier stop, that stop's time — but a
   bookmark needs a real turn so its "Open chat" can resolve later. The rule:
   a head stop bookmarks the newest live turn; an earlier stop bookmarks the
   newest turn of the history surface closest in time to that stop, because a
   stop *is* a seam and the seam's timestamp is the surface's start.
   History that yields nothing readable (back-to-back mechanical seams) falls
   back to the live segment; nothing readable anywhere returns null and the
   caller declines to file a bookmark. */
export function stationLatestTurn(segments, liveTurns, stopTime){
  const live = Array.isArray(liveTurns) ? liveTurns : [];
  const liveLast = live.length ? live[live.length - 1] : null;
  if (!stopTime) return liveLast;

  const segs = (Array.isArray(segments) ? segments : [])
    .filter(s => s && Array.isArray(s.turns) && s.turns.length);
  if (!segs.length) return liveLast;

  const target = Date.parse(stopTime);
  let best = segs[0], bestGap = Infinity;
  for (const s of segs){
    const start = Date.parse(s.start);
    /* an unparseable seam stamp cannot be compared; it stays a candidate of
       last resort rather than poisoning the choice with NaN */
    const gap = Number.isFinite(start) && Number.isFinite(target)
      ? Math.abs(start - target) : Infinity;
    if (gap < bestGap){ best = s; bestGap = gap; }
  }
  return best.turns[best.turns.length - 1];
}
