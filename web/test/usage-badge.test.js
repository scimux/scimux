/* Presentation half of the usage badge: fuel ring, remaining class,
 * reset labels, and the assembled HTML (escaping, rails, weekly-only). */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import {
  usageResetLabel, usageRemClass, fuelGauge, usageBadge,
} from "../js/usage.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const usageSrc = readFileSync(join(__dirname, "../js/usage.js"), "utf8");
const appSrc = readFileSync(join(__dirname, "../js/app.js"), "utf8");

const NOW = new Date(2026, 7, 14, 12, 0, 0); /* Fri Aug 14 2026, local */
const LOCALE = "en-GB";
const logo = (agent) => `LOGO:${agent}`;
const deps = { agentLogo: logo, now: NOW, locales: LOCALE };

const SAME_DAY_ISO = new Date(2026, 7, 14, 17, 42, 0).toISOString();
const OTHER_DAY_ISO = new Date(2026, 7, 11, 17, 42, 0).toISOString(); /* Tue */

const RING = "M5.89 15.66 A7 7 0 1 1 14.11 15.66";
const FILL50 = "M5.89 15.66 A7 7 0 0 1 10.00 3.00";

function pathCount(svg){
  return (svg.match(/<path /g) || []).length;
}
function fillStroke(svg){
  const m = [...svg.matchAll(/stroke="([^"]+)"/g)].map(x => x[1]);
  /* ring is currentColor; a fill path, if present, is the other stroke */
  return m.filter(c => c !== "currentColor")[0] || null;
}

test("usage.js stays pure after the presentation move", () => {
  assert.doesNotMatch(usageSrc, /document\.|window\.|fetch\(/);
});

test("app.js no longer declares the four presentation functions; it imports them", () => {
  assert.doesNotMatch(appSrc, /^function usageResetLabel\s*\(/m);
  assert.doesNotMatch(appSrc, /^function usageRemClass\s*\(/m);
  assert.doesNotMatch(appSrc, /^function fuelGauge\s*\(/m);
  assert.doesNotMatch(appSrc, /^function usageBadge\s*\(/m);
  assert.match(appSrc, /from "\.\/usage\.js"/);
  assert.match(appSrc, /usageBadge/);
  assert.match(appSrc, /agentLogo as agentLogoMod|sysMetricHTML|STATUS_PHASES/);
});

test("usageRemClass at each threshold boundary", () => {
  assert.equal(usageRemClass(null), "");
  assert.equal(usageRemClass(undefined), "");
  assert.equal(usageRemClass(0), "u-crit");
  assert.equal(usageRemClass(9.99), "u-crit");
  assert.equal(usageRemClass(10), "u-warn");
  assert.equal(usageRemClass(19.99), "u-warn");
  assert.equal(usageRemClass(20), "");
  assert.equal(usageRemClass(100), "");
});

test("fuelGauge: null/0 unfilled; 100 full span; colours at boundaries; clamp", () => {
  const empty = fuelGauge(null);
  assert.equal(pathCount(empty), 1, "null is the unfilled grey ring only");
  assert.equal(fillStroke(empty), null);
  assert.match(empty, new RegExp(`d="${RING.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}"`));
  assert.match(empty, /class="fuel"/);
  assert.match(empty, /aria-hidden="true"/);

  const zero = fuelGauge(0);
  assert.equal(pathCount(zero), 1, "0 produces no fill path");
  assert.equal(fillStroke(zero), null);

  const full = fuelGauge(100);
  assert.equal(pathCount(full), 2);
  assert.match(full, new RegExp(`d="${RING.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}"`));
  assert.equal((full.match(new RegExp(RING.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "g")) || []).length, 2,
    "100 fills the whole 288° span — fill path matches the ring");
  assert.equal(fillStroke(full), "#34C759");

  const mid = fuelGauge(50);
  assert.match(mid, new RegExp(FILL50.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")));
  assert.equal(fillStroke(mid), "#34C759");

  assert.equal(fillStroke(fuelGauge(9.99)), "#FF3B30");
  assert.equal(fillStroke(fuelGauge(10)), "#FF9500");
  assert.equal(fillStroke(fuelGauge(19.99)), "#FF9500");
  assert.equal(fillStroke(fuelGauge(20)), "#34C759");

  assert.equal(pathCount(fuelGauge(-5)), 1, "below 0 clamps to empty, no broken arc");
  const over = fuelGauge(150);
  assert.equal(pathCount(over), 2);
  assert.equal((over.match(new RegExp(RING.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "g")) || []).length, 2,
    "above 100 clamps to a full span");
});

test("usageResetLabel: same day, other day, empty, unparseable", () => {
  assert.equal(usageResetLabel(SAME_DAY_ISO, NOW, LOCALE), "17:42");
  assert.equal(usageResetLabel(OTHER_DAY_ISO, NOW, LOCALE), "Tue 17:42");
  assert.equal(usageResetLabel(""), "");
  assert.equal(usageResetLabel(null), "");
  assert.equal(usageResetLabel("not-a-date", NOW, LOCALE), "");
});

test("usageBadge unavailable: u-off, both -- cells, injected logo", () => {
  const html = usageBadge("claude", { available: false }, deps);
  assert.match(html, /class="ubadge u-off"/);
  assert.match(html, /class="Lfull">--</);
  assert.match(html, /class="Labbr">--</);
  assert.match(html, /LOGO:claude/);
  assert.match(html, /role="img"/);
  assert.match(html, /title="Claude: usage unavailable"/);
  assert.match(html, /aria-label="Claude: usage unavailable"/);
  assert.doesNotMatch(html, /resetrail/);
});

test("usageBadge two-window Claude and weekly-only Grok", () => {
  const claude = usageBadge("claude", {
    available: true,
    five_hour_remaining: 40,
    weekly_remaining: 70,
    five_hour_reset: SAME_DAY_ISO,
    weekly_reset: OTHER_DAY_ISO,
  }, deps);
  assert.match(claude, /class="ubadge"/);
  assert.doesNotMatch(claude, /u-off/);
  assert.match(claude, /5h <b class="">40%<\/b>/);
  assert.match(claude, /W <b class="">70%<\/b>/);
  assert.match(claude, /class="Lfull"/);
  assert.match(claude, /class="Labbr"/);
  assert.match(claude, /class="uwindow"/);
  assert.match(claude, /LOGO:claude/);
  assert.equal((claude.match(/class="fuel"/g) || []).length, 4,
    "5h and W each appear in Lfull and Labbr");
  assert.match(claude, /17:42/);
  assert.match(claude, /Tue 17:42/);

  const grok = usageBadge("grok", {
    available: true,
    weekly_remaining: 86,
    plan: "SuperGrok Lite",
  }, deps);
  assert.doesNotMatch(grok, /5h/);
  assert.match(grok, /W <b class="">86%<\/b>/);
  assert.equal((grok.match(/class="fuel"/g) || []).length, 2, "weekly-only: W in Lfull and Labbr");
  assert.match(grok, /LOGO:grok/);
  assert.match(grok, /SuperGrok Lite/);
  assert.doesNotMatch(grok, /class="ubadge u-off"/);
});

test("usageBadge escapes plan in title and aria-label", () => {
  const plan = `free" onclick="alert(1)"><img src=x onerror=alert(1)>&more`;
  const html = usageBadge("codex", {
    available: true,
    five_hour_remaining: 40,
    weekly_remaining: 70,
    plan,
  }, deps);
  const title = html.match(/\stitle="([^"]*)"/);
  const aria = html.match(/\saria-label="([^"]*)"/);
  assert.ok(title, "title attribute is closed by quotes, not by the payload");
  assert.ok(aria, "aria-label attribute is closed by quotes, not by the payload");
  assert.match(title[1], /&quot;/);
  assert.match(title[1], /&lt;/);
  assert.match(title[1], /&amp;/);
  assert.match(aria[1], /&quot;/);
  assert.match(aria[1], /&lt;/);
  assert.match(aria[1], /&amp;/);
  assert.doesNotMatch(title[1], /"/);
  assert.doesNotMatch(html, /onclick="/);
  assert.doesNotMatch(html, /<img /);
  assert.doesNotMatch(html, /onerror="/);
  assert.doesNotMatch(html, />&more/);
});

test("reset rails appear only when the provider reported reset times", () => {
  const withReset = usageBadge("claude", {
    available: true,
    five_hour_remaining: 40,
    weekly_remaining: 70,
    five_hour_reset: SAME_DAY_ISO,
    weekly_reset: OTHER_DAY_ISO,
  }, deps);
  assert.match(withReset, /<span class="resetrail" aria-hidden="true"/);
  assert.match(withReset, /class="Labbr"[\s\S]*resetrail/,
    "rails live in the compact phone presentation");
  assert.doesNotMatch(withReset, /<button/);
  const full = withReset.match(/class="Lfull">([\s\S]*?)<\/span><span class="Labbr"/);
  assert.ok(full);
  assert.doesNotMatch(full[1], /resetrail/, "desktop full cells have no rails");

  const without = usageBadge("claude", {
    available: true,
    five_hour_remaining: 40,
    weekly_remaining: 70,
  }, deps);
  assert.doesNotMatch(without, /resetrail/);
});
