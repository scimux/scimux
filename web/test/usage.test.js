/* Pure usage-badge layout: weekly-only (Grok) vs two-window (Claude/Codex). */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import {
  usageAgentDisplayName, usageBadgeLayout, resetRemainingPercent, usageResetBars,
} from "../js/usage.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const appSrc = readFileSync(join(__dirname, "../js/app.js"), "utf8");
const indexSrc = readFileSync(join(__dirname, "../index.html"), "utf8");
const layoutCss = readFileSync(join(__dirname, "../css/layout.css"), "utf8");

test("usage.js is pure; app imports it; index has #sysgrok", () => {
  assert.match(appSrc, /from "\.\/usage\.js"/);
  assert.match(appSrc, /STATUS_PHASES = \["metrics", "claude", "codex", "grok"\]/);
  assert.match(appSrc, /case "grok":/);
  assert.match(appSrc, /#sysgrok/);
  assert.match(indexSrc, /id="sysgrok"/);
  assert.doesNotMatch(
    readFileSync(join(__dirname, "../js/usage.js"), "utf8"),
    /document\.|window\.|fetch\(/,
  );
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
  const badge = appSrc.slice(appSrc.indexOf("function usageBadge("), appSrc.indexOf("function renderUsage("));
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
