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
  escape = esc,
  newLaneValue = NEW_LANE_VALUE,
  laneById = null,
} = {}){
  const list = Array.isArray(lanes) ? lanes : [];
  const byId = typeof laneById === "function"
    ? laneById
    : (id) => list.find(l => l && l.id === id);
  const unset = includeUnset
    ? `<option value="" ${!selected ? "selected" : ""}>unset</option>`
    : "";
  const missing = selected && !byId(selected)
    ? `<option value="${escape(selected)}" selected>${escape(selected)}</option>`
    : "";
  const opts = list.map(l =>
    `<option value="${escape(l.id)}" ${selected === l.id ? "selected" : ""}>${escape(l.name)}</option>`
  ).join("");
  const create = disabled ? "" : `<option value="${newLaneValue}">New lane...</option>`;
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
  newLaneValue = NEW_LANE_VALUE,
} = {}){
  const v = val || "";
  if (v === newLaneValue){
    const trimmed = (name || "").trim();
    if (!trimmed) return { error: "Enter a lane name." };
    if (typeof makeLane !== "function") {
      return { error: "Enter a lane name." };
    }
    const lane = makeLane(trimmed);
    return { laneID: lane.id, lane };
  }
  return { laneID: v };
}
