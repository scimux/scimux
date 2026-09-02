/* Pure usage-badge layout: weekly-only (Grok) vs two-window (Claude/Codex). */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import {
  usageAgentDisplayName, usageBadgeLayout, resetRemainingPercent, usageResetBars,
  agentLogo, sysMetricHTML, statusPhaseAt, STATUS_PHASES,
} from "../js/usage.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const appSrc = readFileSync(join(__dirname, "../js/app.js"), "utf8");
const usageSrc = readFileSync(join(__dirname, "../js/usage.js"), "utf8");
const indexSrc = readFileSync(join(__dirname, "../index.html"), "utf8");
const layoutCss = readFileSync(join(__dirname, "../css/layout.css"), "utf8");

test("usage.js is pure; app imports it; index has #sysgrok", () => {
  assert.match(appSrc, /from "\.\/usage\.js"/);
  assert.match(appSrc, /STATUS_PHASES/);
  assert.match(appSrc, /agentLogo as agentLogoMod|agentLogoMod/);
  assert.match(appSrc, /#sysgrok/);
  assert.match(indexSrc, /id="sysgrok"/);
  assert.doesNotMatch(usageSrc, /document\.|window\.|fetch\(/);
  assert.deepEqual(STATUS_PHASES, ["metrics", "claude", "codex", "grok"]);
});

test("agentLogo routes each known agent through assetURL", () => {
  const seen = [];
  const url = (p) => { seen.push(p); return `U:${p}`; };
  assert.match(agentLogo("claude", url), /U:\/assets\/agents\/claude\.svg/);
  assert.match(agentLogo("codex", url), /U:\/assets\/agents\/openai\.svg/);
  assert.match(agentLogo("openai", url), /U:\/assets\/agents\/openai\.svg/);
  assert.match(agentLogo("pi", url), /U:\/assets\/agents\/pi\.svg/);
  assert.match(agentLogo("opencode", url), /U:\/assets\/agents\/opencode\.svg/);
  assert.match(agentLogo("grok", url), /U:\/assets\/agents\/grok\.svg/);
  assert.match(agentLogo("unknown", url), /<svg/);
  assert.deepEqual(seen, [
    "/assets/agents/claude.svg",
    "/assets/agents/openai.svg",
    "/assets/agents/openai.svg",
    "/assets/agents/pi.svg",
    "/assets/agents/opencode.svg",
    "/assets/agents/grok.svg",
  ]);
});

test("sysMetricHTML: flat/up/down and crit at 90", () => {
  const flat = sysMetricHTML("M", "50%", 50, 50);
  assert.match(flat, /a-flat/);
  assert.match(flat, /→/);
  const up = sysMetricHTML("M", "60%", 60, 50);
  assert.match(up, /a-stress/);
  assert.match(up, /↑/);
  assert.match(up, /roll-dn/);
  const down = sysMetricHTML("M", "40%", 40, 50);
  assert.match(down, /a-good/);
  assert.match(down, /↓/);
  const crit = sysMetricHTML("M", "91%", 91, 80);
  assert.match(crit, /a-crit/);
  const first = sysMetricHTML("M", "10%", 10, null);
  assert.match(first, /a-flat/);
});

test("statusPhaseAt stays on metrics until a snapshot exists", () => {
  assert.equal(statusPhaseAt(0, null), "metrics");
  assert.equal(statusPhaseAt(1, null), "metrics");
  assert.equal(statusPhaseAt(1, {}), "claude");
  assert.equal(statusPhaseAt(2, {}), "codex");
  assert.equal(statusPhaseAt(3, {}), "grok");
  assert.equal(statusPhaseAt(4, {}), "metrics");
});

test("usageAgentDisplayName", () => {
  assert.equal(usageAgentDisplayName("claude"), "Claude");
  assert.equal(usageAgentDisplayName("codex"), "Codex");
  assert.equal(usageAgentDisplayName("grok"), "Grok");
  assert.equal(usageAgentDisplayName("other"), "other");
});

test("weekly-only Grok badge omits 5h half; plan in tip", () => {
  const g = usageBadgeLayout("grok", {
    available: true,
    weekly_remaining: 86,
    plan: "SuperGrok Lite",
  });
  assert.equal(g.available, true);
  assert.equal(g.has5h, false);
  assert.equal(g.hasW, true);
  assert.equal(g.name, "Grok");
  assert.match(g.tip, /weekly 86% left/);
  assert.doesNotMatch(g.tip, /5h/);
  assert.match(g.tip, /SuperGrok Lite/);
});

test("Claude/Codex two-window layout preserved", () => {
  const c = usageBadgeLayout("claude", {
    available: true,
    five_hour_remaining: 40,
    weekly_remaining: 70,
  });
  assert.equal(c.has5h, true);
  assert.equal(c.hasW, true);
  assert.match(c.tip, /5h 40% left/);
  assert.match(c.tip, /weekly 70% left/);

  const x = usageBadgeLayout("codex", {
    available: true,
    five_hour_remaining: 10,
    weekly_remaining: 50,
  });
  assert.equal(x.name, "Codex");
  assert.equal(x.has5h, true);
  assert.equal(x.hasW, true);
});

test("unavailable and empty windows", () => {
  const off = usageBadgeLayout("grok", { available: false });
  assert.equal(off.available, false);
  assert.match(off.tip, /usage unavailable/);

  const empty = usageBadgeLayout("grok", { available: true });
  assert.equal(empty.has5h, false);
  assert.equal(empty.hasW, false);
  assert.match(empty.tip, /usage unavailable/);
});

test("resetRemainingPercent is an accurate clamped determinate gauge", () => {
  const now = Date.parse("2026-08-14T12:00:00Z");
  assert.equal(resetRemainingPercent("2026-08-14T14:30:00Z", 300, now), 50);
  assert.equal(resetRemainingPercent("2026-08-19T00:00:00Z", 10080, now), 64.28571428571429);
  assert.equal(resetRemainingPercent("2026-08-14T11:00:00Z", 300, now), 0,
    "a stale reset drains to zero");
  assert.equal(resetRemainingPercent("2026-08-15T12:00:00Z", 300, now), 100,
    "an out-of-range future reset clamps to full");
  assert.equal(resetRemainingPercent("", 300, now), null);
  assert.equal(resetRemainingPercent("not-a-date", 300, now), null);
  assert.equal(resetRemainingPercent("2026-08-14T14:30:00Z", 0, now), null);
});

test("usageResetBars returns two labeled rails, but Grok only its weekly rail", () => {
  const now = Date.parse("2026-08-14T12:00:00Z");
  const two = usageResetBars({
    five_hour_remaining: 40,
    five_hour_reset: "2026-08-14T14:30:00Z",
    weekly_remaining: 70,
    weekly_reset: "2026-08-18T00:00:00Z",
  }, now);
  assert.deepEqual(two.map(x => x.key), ["5h", "W"]);
  assert.equal(two[0].remainingPercent, 50);

  const grok = usageResetBars({
    weekly_remaining: 86,
    weekly_reset: "2026-08-18T00:00:00Z",
  }, now);
  assert.deepEqual(grok.map(x => x.key), ["W"]);

  const unknown = usageResetBars({
    five_hour_remaining: 40,
    weekly_remaining: 70,
  }, now);
  assert.deepEqual(unknown, [], "unknown reset times emit no misleading empty rail");
});

test("phone badge owns minimal noninteractive reset rails and one VoiceOver summary", () => {
  /* The badge HTML moved from app.js into usage.js; this lock follows the code
     it locks. usageBadge is the last declaration in the module, so the slice
     runs to end of file. The behavioural assertions live in
     usage-badge.test.js — this one only pins the accessibility shape. */
  const start = usageSrc.indexOf("export function usageBadge(");
  assert.notEqual(start, -1, "usageBadge is declared in usage.js");
  const badge = usageSrc.slice(start);
  assert.match(badge, /usageResetBars\(/);
  assert.match(badge, /<span class="resetrail"[^>]*aria-hidden="true"/,
    "the visual rails are inert descendants, not controls");
  assert.doesNotMatch(badge, /<button[^>]*resetrail|resetrail[^`]*<button/);
  assert.match(badge, /class="ubadge"[^>]*role="img"[^>]*aria-label=/,
    "VoiceOver reads one concise badge summary, not separate micro-elements");
  assert.match(badge, /class="Labbr"[^>]*>[\s\S]*resetrail/,
    "rails live in the compact phone presentation");

  assert.match(layoutCss, /@media\s*\(max-width:\s*520px\)[\s\S]*\.Labbr\s*\{[^}]*display:\s*inline-flex/);
  assert.match(layoutCss, /\.resetrail\s*\{[^}]*height:\s*2px[^}]*background:/s);
  assert.match(layoutCss, /\.resetrail\s+i\s*\{[^}]*height:\s*100%[^}]*background:/s);
});
