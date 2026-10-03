"use strict";
/* Pure subscription-usage badge: layout, reset rails, fuel gauges, and HTML.
 * Also owns agent logo markup and status-slot metric/phase presentation.
 * Shell (app.js) owns the cadence timers and the innerHTML writes. */
import { esc } from "./format.js";

/** Status-slot phase cycle: system metrics then each agent budget. */
export const STATUS_PHASES = Object.freeze(["metrics", "claude", "codex", "grok"]);

/**
 * Mark the status slot unreachable, or clear it. A class, never a write: the
 * slot is a container of phase elements and a textContent write over it
 * deleted them for good, so the next metrics render threw on a missing child.
 * The message has its own element inside the slot; CSS does the swap.
 */
export function setStatusUnreachable(el, unreachable){
  if (el && el.classList) el.classList.toggle("offline", !!unreachable);
}

/**
 * Resolve which status phase to show. Budget phases stay on metrics until a
 * usage snapshot has been read at least once (avoids an empty flip).
 */
export function statusPhaseAt(idx, usageSnap){
  const phase = STATUS_PHASES[idx];
  if (phase !== "metrics" && usageSnap == null) return "metrics";
  return phase;
}

/**
 * Agent logo HTML for cards/map/chat/status. `assetURL` is required so the
 * FR-42 transport seam stays explicit; the shell closes over its supplier and
 * keeps the historical one-argument `agentLogo(agent)` call sites.
 */
export function agentLogo(agent, assetURL){
  /* Call the parameter as assetURL(...) so AT-FR-42's seam scanner still
     classifies these sinks as seam-routed (wrapper name is the contract). */
  switch ((agent || "").toLowerCase()){
  case "claude":
    return `<img src="${assetURL('/assets/agents/claude.svg')}" alt="" aria-hidden="true">`;
  case "codex":
  case "openai":
    return `<img src="${assetURL('/assets/agents/openai.svg')}" alt="" aria-hidden="true">`;
  case "pi":
    return `<img src="${assetURL('/assets/agents/pi.svg')}" alt="" aria-hidden="true">`;
  case "opencode":
    return `<img src="${assetURL('/assets/agents/opencode.svg')}" alt="" aria-hidden="true">`;
  case "grok":
    return `<img src="${assetURL('/assets/agents/grok.svg')}" alt="" aria-hidden="true">`;
  case "muse":
    return `<img src="${assetURL('/assets/agents/meta.svg')}" alt="" aria-hidden="true">`;
  case "cursor":
    return `<img src="${assetURL('/assets/agents/cursor.svg')}" alt="" aria-hidden="true">`;
  case "dsh":
    return `<img src="${assetURL('/assets/agents/deepseek.svg')}" alt="" aria-hidden="true">`;
  case "vibe":
    return `<img src="${assetURL('/assets/agents/mistral.svg')}" alt="" aria-hidden="true">`;
  default:
    return `<svg viewBox="0 0 24 24" aria-hidden="true">
      <circle cx="12" cy="12" r="10" fill="var(--dim)"/>
      <circle cx="12" cy="12" r="3" fill="#fff"/>
    </svg>`;
  }
}

/**
 * One system-metric cell: label, display value, trend arrow from pct delta.
 * Crit at ≥90%; flat within ±0.4 of previous.
 */
export function sysMetricHTML(label, disp, pct, prevPct){
  const d = prevPct == null ? 0 : pct - prevPct;
  const dir  = Math.abs(d) < .4 ? 0 : d > 0 ? 1 : -1;
  const crit = pct >= 90;
  const aCls = crit ? "a-crit" : dir > 0 ? "a-stress" : dir < 0 ? "a-good" : "a-flat";
  const roll = dir > 0 ? "roll-dn" : dir < 0 ? "roll-up" : "";
  return `<span>${label} <b class="${roll} ${crit ? "a-crit" : ""}">${disp}</b>` +
         ` <span class="trend ${aCls}">${dir > 0 ? "↑" : dir < 0 ? "↓" : "→"}</span></span>`;
}

export function usageAgentDisplayName(agent){
  if (agent === "codex") return "Codex";
  if (agent === "claude") return "Claude";
  if (agent === "grok") return "Grok";
  if (agent === "muse") return "Muse";
  if (agent === "cursor") return "Cursor";
  if (agent === "vibe") return "Mistral Vibe";
  return String(agent || "");
}

/* Dollar text for a positive Vibe-reported amount. Empty means unknown:
   a missing, zero, negative, or non-finite figure is not a bill, and a
   positive amount that rounds to $0.00 at two decimals keeps enough digits
   to stay visibly nonzero. */
function vibeCostText(cost, costComplete){
  if (costComplete !== true) return "";
  const v = Number(cost);
  if (!Number.isFinite(v) || !(v > 0)) return "";
  const cents = v.toFixed(2);
  if (cents !== "0.00") return "$" + cents;
  const precise = v.toFixed(6).replace(/0+$/, "").replace(/\.$/, "");
  if (precise === "0") return "";
  return "$" + precise;
}

/* Three different measurements. Context occupancy is the live context fill.
   Token spend is the summed per-turn delta. Cost is Vibe's own USD estimate
   when one was supplied. A missing figure stays the word unknown. */
export function vibeMetricLabels({
  occupancyPct = null, tokens = null, cost = null, costComplete = false,
} = {}){
  const occN = Number(occupancyPct);
  const occupancy = occupancyPct == null || !Number.isFinite(occN) || occN < 0
    ? "context occupancy unknown"
    : `context occupancy ${Math.round(occN)}%`;
  const tokN = Number(tokens);
  const spend = tokens == null || !Number.isFinite(tokN) || tokN < 0
    ? "token spend unknown"
    : `token spend ${Math.round(tokN)}`;
  const costText = vibeCostText(cost, costComplete);
  const reported = costText
    ? `Vibe-reported cost ${costText}`
    : "Vibe-reported cost unknown";
  return { occupancy, spend, reported, costText };
}

/* Weekly-only providers omit the empty 5h half so the badge reads "W N%"
   rather than "-- · W N%". Plan text is appended to the tip when present. */
export function usageBadgeLayout(agent, v){
  const name = usageAgentDisplayName(agent);
  if (!v || !v.available){
    /* Off is the one dark gauge a tap can fix, so it is the one that says so.
       Spelling it like every other failure would train the user to read a
       working switch as a broken feature. */
    const off = !!(v && v.off);
    return { name, available: false, off, has5h: false, hasW: false,
      tip: off ? name + ": usage checks are off — tap to turn on"
                : name + ": usage unavailable" };
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
  return { name, available: true, off: false, has5h, hasW, tip };
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
  const now = deps.now instanceof Date ? deps.now : new Date();
  const locales = deps.locales;
  // Render the abbreviation as text here: shrinking its SVG to status-bar
  // dimensions also shrinks the lettering far below a readable font size.
  const labels = { claude:"Cld", codex:"Cdx", openai:"Cdx", grok:"Grk", pi:"Pi", opencode:"OC", muse:"Mus", cursor:"Cur", dsh:"Dsh" };
  const label = labels[String(agent || "").toLowerCase()] || "?";
  const logo = `<span class="usage-agent-label" aria-hidden="true">${label}:</span>`;
  const layout = usageBadgeLayout(agent, v);
  if (!layout.available){
    /* Only the switched-off badge is a control. The rest describe a state the
       user cannot act on, and a button that does nothing is worse than a
       label — for a pointer and for the accessibility tree alike. */
    const act = layout.off
      ? ` role="button" tabindex="0" data-usage-off="${esc(agent || "")}"`
      : ` role="img"`;
    return `<span class="ubadge u-off"${act} title="${esc(layout.tip)}" aria-label="${esc(layout.tip)}">${logo}` +
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
