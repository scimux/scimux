"use strict";
/* Pure lane-picker HTML and form-choice decisions shared by cards and sheets.
 * Lane *model* helpers stay in lanes.js (no HTML). DOM sync (fill/sync) stays
 * in the composition root. */
import { esc } from "./format.js";

export const NEW_LANE_VALUE = "__new";

/** Build <option> list for a lane <select>. */
export function laneOptionsHTML(selected, lanes, {
  includeUnset = true,
  disabled = false,
} = {}){
  const list = Array.isArray(lanes) ? lanes : [];
  const unset = includeUnset
    ? `<option value="" ${!selected ? "selected" : ""}>unset</option>`
    : "";
  const missing = selected && !list.find(l => l && l.id === selected)
    ? `<option value="${esc(selected)}" selected>${esc(selected)}</option>`
    : "";
  const opts = list.map(l =>
    `<option value="${esc(l.id)}" ${selected === l.id ? "selected" : ""}>${esc(l.name)}</option>`
  ).join("");
  const create = disabled ? "" : `<option value="${NEW_LANE_VALUE}">New lane...</option>`;
  return `${unset}${missing}${opts}${create}`;
}

/** Wrap laneOptionsHTML in a <select data-lane-select>. */
export function laneSelectHTML(selected, lanes, opts = {}){
  const disabled = !!opts.disabled;
  return `<select data-lane-select ${disabled ? "disabled" : ""}>${laneOptionsHTML(selected, lanes, opts)}</select>`;
}

/**
 * Pure form-choice from select value + new-lane name.
 * Returns { laneID } | { laneID, lane } | { error }.
 * `makeLane(name)` is injected so callers own id/color minting.
 */
export function readLaneChoiceFromValues(val, name, {
  makeLane,
} = {}){
  const v = val || "";
  if (v === NEW_LANE_VALUE){
    const trimmed = (name || "").trim();
    if (!trimmed) return { error: "Enter a lane name." };
    const lane = makeLane(trimmed);
    return { laneID: lane.id, lane };
  }
  return { laneID: v };
}

/** Counts the page uses for a lane share. Null when this computer has no such lane. */
export function laneShareFacts(id, lane, color, nodes, archivedIds){
  if (!lane) return null;
  const gone = new Set(Array.isArray(archivedIds) ? archivedIds : []);
  const archived = [];
  let open = 0;
  for (const n of (Array.isArray(nodes) ? nodes : [])) {
    if (!n || n.lane_id !== id) continue;
    if (gone.has(n.id)) archived.push(n.id);
    else open += 1;
  }
  return { name: lane.name || id, color: color || "", open, archived };
}

/** Notice under a lane picker. Empty when the lane is not shared. */
export function laneShareNotice(share, serverName){
  if (!share || share.state !== "active") return "";
  const n = Array.isArray(share.members) ? share.members.length : 0;
  const people = n === 1 ? "1 person" : n + " people";
  return "This lane is shared with " + people + " on " + serverName +
    ". This chat will be shared as soon as it starts.";
}

/** Write the notice into the picker. False when the element is absent. */
export function paintLaneShareNotice(root, text){
  if (!root || typeof root.querySelector !== "function") return false;
  const note = root.querySelector("[data-lane-share]");
  if (!note) return false;
  note.textContent = text;
  note.hidden = text === "";
  return true;
}
