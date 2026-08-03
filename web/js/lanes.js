/* Pure lane / fork / stop / column-order model helpers.
 *
 * Extracted in Packet 6B and wired into the sole browser entry in Packet 6F.
 * Named exports are exercised directly by Node tests and used by production.
 *
 * Explicit inputs (no UI / nodes / sel / mapTab globals):
 * - lane configuration arrays are passed as `lanes`
 * - node collections are passed as `nodes`
 * - map grouping takes (mapTab, groups)
 * - laneColumnOrder already took explicit colLanes/stations/byId inline
 *
 * Reuses safeColor and stampMS from format.js — do not duplicate them here.
 *
 * No document, window, fetch, localStorage, navigator, timers, or app state.
 * No HTML/DOM helpers (lane select/options/chips), UI mutation, card ordering,
 * or map/SVG rendering.
 */

import { safeColor, stampMS } from "./format.js";

/* ---------- palette + deterministic hash (lane color fallback) ---------- */

export const LANE_COLORS = ["#007AFF","#5856D6","#AF52DE","#34C759","#FF9500","#FF2D55","#00C7BE"];

/* djb2-style signed 32-bit hash; used for palette index when no authored color. */
export function hashStr(s){
  let h = 5381;
  for (let i = 0; i < s.length; i++) h = ((h << 5) + h + s.charCodeAt(i)) | 0;
  return h;
}

/* ---------- lane list / ordering / lookup ---------- */

export function laneList(lanes){
  return Array.isArray(lanes) ? lanes : [];
}

/* Deterministic name/id ordering including ties: base-insensitive name, then
   base-insensitive id. Matches the inline byNameID comparator. */
export const byNameID = (a, b) =>
  (a.name || a.id || "").localeCompare(b.name || b.id || "", undefined, { sensitivity: "base" })
    || (a.id || "").localeCompare(b.id || "", undefined, { sensitivity: "base" });

export function sortedLaneList(lanes){
  return [...laneList(lanes)].sort(byNameID);
}

export function laneById(id, lanes){
  return laneList(lanes).find(l => l.id === id) || null;
}

export function laneName(id, lanes){
  return laneById(id, lanes)?.name || id || "unset";
}

/* Safe authored color, else --unlane for empty id, else palette wrap by hash. */
export function laneColor(id, lanes){
  const lane = laneById(id, lanes);
  const c = lane && safeColor(lane.color);
  if (c) return c;
  if (!id) return "var(--unlane)";
  return LANE_COLORS[Math.abs(hashStr(id)) % LANE_COLORS.length];
}

/* Stable ID from display name: lowercased, non-[a-z0-9._-] → "-", collision
   suffix -2, -3, … */
export function uniqueLaneID(name, lanes){
  const base = (name || "lane").toLowerCase()
    .replace(/[^a-z0-9._-]+/g, "-").replace(/^-+|-+$/g, "") || "lane";
  const used = new Set(laneList(lanes).map(l => l.id));
  if (!used.has(base)) return base;
  for (let i = 2; ; i++){
    const id = `${base}-${i}`;
    if (!used.has(id)) return id;
  }
}

export function nextLaneColor(lanes){
  return LANE_COLORS[laneList(lanes).length % LANE_COLORS.length];
}

export function makeLane(name, lanes){
  const clean = name.trim();
  return { id: uniqueLaneID(clean, lanes), name: clean, color: nextLaneColor(lanes) };
}

/* A thread rides exactly one lane; filter Boolean drops unset. */
export const servedLanes = n => [n.lane_id].filter(Boolean);

/* Shared journey structure: configured lanes that are used, plus synthetic
   entries for lane_ids present on nodes but absent from UI configuration.
   color/name close over the full configuration so lookups stay consistent. */
export function laneModel(nodes, lanes){
  const list = laneList(lanes);
  const nodeList = Array.isArray(nodes) ? nodes : [];
  const byId = {};
  nodeList.forEach(n => byId[n.id] = n);
  const used = new Set(nodeList.flatMap(servedLanes));
  const out = list.filter(l => used.has(l.id));
  for (const id of used){
    if (!out.some(l => l.id === id)) out.push({ id, name: id, color: laneColor(id, list) });
  }
  out.sort(byNameID);
  return {
    byId,
    lanes: out,
    color: id => laneColor(id, list),
    name: id => laneName(id, list),
  };
}

/* Pure map-tab grouping: "all" → null (no filter); otherwise the group's
   lane-id list, or null-ish empty when the tab id is unknown. */
export function groupLanes(mapTab, groups){
  return mapTab === "all" ? null
    : ((groups || []).find(t => t.id === mapTab)?.lanes ?? null);
}

/* Whether a lane id is included under a group filter (null group = all). */
export function laneInGroup(grp, laneID){
  return !grp || grp.includes(laneID);
}

/* Stations that have a lane and serve at least one in-group lane. */
export function stationsInGroup(nodes, grp){
  const nodeList = Array.isArray(nodes) ? nodes : [];
  return nodeList.filter(n => n.lane_id && servedLanes(n).some(id => laneInGroup(grp, id)));
}

/* ---------- fork kind (stored precedence + legacy fallback) ---------- */

/* What a station's fork did at creation time. null → no parent (root) or the
   parent is absent from the node set (orphaned). Stored n.fork_kind wins so
   sibling deletion cannot flip historical "s" → "y-new". Legacy records
   recompute: same lane → y-stay; older station already on dest → s; else y-new. */
export function forkKind(n, nodes){
  const nodeList = Array.isArray(nodes) ? nodes : [];
  const p = n.parent && nodeList.find(x => x.id === n.parent);
  if (!p) return null;
  if (n.fork_kind) return n.fork_kind;
  if (n.lane_id === p.lane_id) return "y-stay";
  const priorInDest = nodeList.some(x => x.id !== n.id && x.lane_id === n.lane_id &&
    (x.created_at || "") < (n.created_at || ""));
  return priorInDest ? "s" : "y-new";
}

/* ---------- stops ---------- */

export function stopTimes(n){ return [n.created_at || "", ...(n.stops || [])]; }

export function stopsOf(n){
  const ts = stopTimes(n);
  return ts.map((t, i) => ({ n, i, time: t, head: i === ts.length - 1 }));
}

export const stopKey = s => s.n.id + "#" + s.i;

/* Head reads live title/description; closed stops use station_labels[time]
   when present, else the same live fallback (legacy). */
export function stopLabel(s){
  const n = s.n;
  const fallback = { title: n.title || "", desc: n.description || n.prompt || "" };
  if (s.head) return fallback;
  const snap = n.station_labels && n.station_labels[s.time];
  return snap ? { title: snap.title || "", desc: snap.desc || "" } : fallback;
}

export const newestFirst = (a, b) => stampMS(b.time) - stampMS(a.time) ||
  (b.time || "").localeCompare(a.time || "");

/* Newest parent stop that already existed when the child was created.
   Missing/invalid childCreated keeps idx at 0 (creation stop). */
export function parentStopIndex(p, childCreated){
  const ts = stopTimes(p), c = stampMS(childCreated);
  let idx = 0;
  for (let i = 1; i < ts.length; i++)
    if (c && stampMS(ts[i]) <= c) idx = i;
  return idx;
}

/* ---------- wall-map column order ---------- */

/* Order lane columns so fork partners are adjacent (short crossovers): greedy
   chain over the cross-lane fork graph; isolated lanes appended by recency. */
export function laneColumnOrder(colLanes, stations, byId){
  const ids = colLanes.map(l => l.id);
  const inSet = new Set(ids);
  const w = {}, nbr = {}, rec = {};
  ids.forEach(id => { nbr[id] = new Set(); rec[id] = 0; });
  for (const n of stations){
    const t = Date.parse(n.created_at) || 0;
    if (inSet.has(n.lane_id) && t > rec[n.lane_id]) rec[n.lane_id] = t;
    const p = n.parent && byId[n.parent];
    if (p && n.lane_id !== p.lane_id && inSet.has(n.lane_id) && inSet.has(p.lane_id)){
      const k = [n.lane_id, p.lane_id].sort().join("|");
      w[k] = (w[k] || 0) + 1;
      nbr[n.lane_id].add(p.lane_id); nbr[p.lane_id].add(n.lane_id);
    }
  }
  const wt = (a, b) => w[[a, b].sort().join("|")] || 0;
  const total = a => [...nbr[a]].reduce((s, b) => s + wt(a, b), 0);
  const left = new Set(ids), out = [];
  const bestFree = () => [...left].sort((a, b) => total(b) - total(a) || rec[b] - rec[a])[0];
  while (left.size){
    let pick;
    if (out.length){
      const last = out[out.length - 1];
      let best = null, bs = -1;
      for (const c of left){
        const s = wt(last, c);
        if (s > bs || (s === bs && (best === null || rec[c] > rec[best]))){ best = c; bs = s; }
      }
      pick = bs > 0 ? best : bestFree();
    } else pick = bestFree();
    out.push(pick); left.delete(pick);
  }
  return out;
}
