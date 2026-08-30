"use strict";
/* Pure subscription-usage badge: layout, reset rails, fuel gauges, and HTML.
 * Shell (app.js) owns the 30s cadence and the innerHTML write. */
import { esc } from "./format.js";

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

export function usageResetLabel(iso, now = new Date(), locales = undefined){
  if (!iso) return "";
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  const loc = locales === undefined ? [] : locales;
  const hhmm = d.toLocaleTimeString(loc, { hour: "2-digit", minute: "2-digit" });
  if (d.toDateString() === now.toDateString()) return hhmm;   /* same day: time only */
  return d.toLocaleDateString(loc, { weekday: "short" }) + " " + hhmm; /* else: weekday + time */
}
export function usageRemClass(rem){
  if (rem == null) return "";
  if (rem < 10) return "u-crit";
  if (rem < 20) return "u-warn";
  return "";
}
/* A fuel-gauge glyph: a 4/5 ring (288°) with the missing 1/5 as a gap at the
   bottom (6 o'clock), like a car fuel gauge. The arc fills from the lower-left
   (E) clockwise over the top to the lower-right (F) in proportion to remaining
   %, so a full ring == full tank. Colour tracks the same thresholds as the
   number: green healthy, orange <20%, red <10%. This makes "high == good" read
   correctly next to the "high == bad" MEM/SWAP metrics — the fill level is the
   shared meaning, not the raw number's direction. */
export function fuelGauge(rem){
  const R = 7, cx = 10, cy = 10, gap = 72, start = 90 + gap / 2, span = 360 - gap;
  const pt = (a) => { const r = a * Math.PI / 180;
    return [(cx + R * Math.cos(r)).toFixed(2), (cy + R * Math.sin(r)).toFixed(2)]; };
  const arc = (a0, a1) => { const [x0, y0] = pt(a0), [x1, y1] = pt(a1);
    return `M${x0} ${y0} A${R} ${R} 0 ${(a1 - a0) > 180 ? 1 : 0} 1 ${x1} ${y1}`; };
  const p = rem == null ? 0 : Math.max(0, Math.min(100, rem));
  const color = rem == null ? "currentColor" : p < 10 ? "#FF3B30" : p < 20 ? "#FF9500" : "#34C759";
  const fill = p > 0 ? `<path d="${arc(start, start + span * p / 100)}" fill="none" ` +
    `stroke="${color}" stroke-width="2.4" stroke-linecap="round"/>` : "";
  return `<svg class="fuel" viewBox="0 0 20 20" width="13" height="13" aria-hidden="true">` +
    `<path d="${arc(start, start + span)}" fill="none" stroke="currentColor" ` +
    `stroke-opacity=".2" stroke-width="2.4" stroke-linecap="round"/>${fill}</svg>`;
}
export function usageBadge(agent, v, deps = {}){
  const agentLogo = typeof deps.agentLogo === "function" ? deps.agentLogo : () => "";
  const now = deps.now instanceof Date ? deps.now : new Date();
  const locales = deps.locales;
  const logo = `<span class="agent-logo" aria-hidden="true">${agentLogo(agent)}</span>`;
  const layout = usageBadgeLayout(agent, v);
  if (!layout.available){
    return `<span class="ubadge u-off" role="img" title="${esc(layout.tip)}" aria-label="${esc(layout.tip)}">${logo}` +
           `<span class="Lfull">--</span><span class="Labbr">--</span></span>`;
  }
  const fh = v.five_hour_remaining, wk = v.weekly_remaining;
  const fhr = usageResetLabel(v.five_hour_reset, now, locales), wkr = usageResetLabel(v.weekly_reset, now, locales);
  const num = (x) => x == null ? "--" : Math.round(x);
  const fhc = usageRemClass(fh), wkc = usageRemClass(wk);
  const fg = fuelGauge(fh), wg = fuelGauge(wk);
  const resetBars = new Map(usageResetBars(v, now.getTime()).map(x => [x.key, x]));
  const resetRail = key => {
    const bar = resetBars.get(key);
    if (!bar) return "";
    return `<span class="resetrail" aria-hidden="true"><i style="width:${bar.remainingPercent}%"></i></span>`;
  };
  let fullInner = "", tip = layout.name + ":";
  const abbrCells = [];
  if (layout.has5h) {
    fullInner += `${fg}5h <b class="${fhc}">${num(fh)}%</b>${fhr ? " " + esc(fhr) : ""}`;
    abbrCells.push(`<span class="uwindow"><span class="uwindow-label">` +
      `${fg}5h <b class="${fhc}">${num(fh)}%</b></span>${resetRail("5h")}</span>`);
    tip += ` 5h ${num(fh)}% left${fhr ? ", resets " + fhr : ""}`;
  }
  if (layout.has5h && layout.hasW) { fullInner += " · "; tip += " ·"; }
  if (layout.hasW) {
    fullInner += `${wg}W <b class="${wkc}">${num(wk)}%</b>${wkr ? " " + esc(wkr) : ""}`;
    abbrCells.push(`<span class="uwindow"><span class="uwindow-label">` +
      `${wg}W <b class="${wkc}">${num(wk)}%</b></span>${resetRail("W")}</span>`);
    tip += ` weekly ${num(wk)}% left${wkr ? ", resets " + wkr : ""}`;
  }
  let abbrInner = abbrCells.join("");
  if (!layout.has5h && !layout.hasW) {
    fullInner = abbrInner = "--";
    tip += " usage unavailable";
  }
  if (v.plan) tip += ` (${v.plan})`;
  const full = `<span class="Lfull">${fullInner}</span>`;
  const abbr = `<span class="Labbr">${abbrInner}</span>`;
  return `<span class="ubadge" role="img" title="${esc(tip)}" aria-label="${esc(tip)}">${logo}${full}${abbr}</span>`;
}
