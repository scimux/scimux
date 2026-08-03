"use strict";
/* Pure subscription-usage badge layout for the status slot.
 * Shell (app.js) owns rendering/fuel gauges and the 30s cadence; this module
 * only decides window presence, display name, and tooltip skeleton so Node
 * tests can cover weekly-only (Grok) vs two-window (Claude/Codex) without
 * loading the browser entry. */

export function usageAgentDisplayName(agent){
  if (agent === "codex") return "Codex";
  if (agent === "claude") return "Claude";
  if (agent === "grok") return "Grok";
  return String(agent || "");
}

/* Weekly-only providers omit the empty 5h half so the badge reads "W N%"
   rather than "-- · W N%". Plan text is appended to the tip when present. */
export function usageBadgeLayout(agent, v){
  const name = usageAgentDisplayName(agent);
  if (!v || !v.available){
    return { name, available: false, has5h: false, hasW: false, tip: name + ": usage unavailable" };
  }
  const fh = v.five_hour_remaining, wk = v.weekly_remaining;
  const has5h = fh != null, hasW = wk != null;
  const num = (x) => x == null ? "--" : Math.round(x);
  let tip = name + ":";
  if (has5h) tip += ` 5h ${num(fh)}% left`;
  if (has5h && hasW) tip += " ·";
  if (hasW) tip += ` weekly ${num(wk)}% left`;
  if (!has5h && !hasW) tip += " usage unavailable";
  if (v.plan) tip += ` (${v.plan})`;
  return { name, available: true, has5h, hasW, tip };
}
