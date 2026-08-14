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

/* Determinate time-to-reset gauge. The value is remaining runway (full just
   after a reset, empty at the next reset), matching the subscription fuel
   rings' high-is-good direction. Unknown/bad resets stay absent; callers must
   not turn missing provider data into a misleading empty rail. */
export function resetRemainingPercent(reset, windowMinutes, nowMs = Date.now()){
  const resetMs = reset instanceof Date ? reset.getTime() : Date.parse(reset || "");
  const durationMs = Number(windowMinutes) * 60_000;
  const now = Number(nowMs);
  if (!Number.isFinite(resetMs) || !(durationMs > 0) || !Number.isFinite(now)) return null;
  return Math.max(0, Math.min(100, 100 * (resetMs - now) / durationMs));
}

/* Phone reset rails in reading order. Window presence follows the quota
   surface: Grok has only W; Claude/Codex normally have 5h then W. A provider
   can report quota without a reset time, in which case that rail is omitted. */
export function usageResetBars(v, nowMs = Date.now()){
  const u = v || {};
  const out = [];
  if (u.five_hour_remaining != null){
    const remainingPercent = resetRemainingPercent(u.five_hour_reset, 300, nowMs);
    if (remainingPercent != null) out.push({ key: "5h", remainingPercent });
  }
  if (u.weekly_remaining != null){
    const remainingPercent = resetRemainingPercent(u.weekly_reset, 10_080, nowMs);
    if (remainingPercent != null) out.push({ key: "W", remainingPercent });
  }
  return out;
}
