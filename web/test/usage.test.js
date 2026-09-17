/* Pure usage-badge layout: weekly-only (Grok) vs two-window (Claude/Codex). */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import {
  usageAgentDisplayName, usageBadgeLayout, resetRemainingPercent, usageResetBars,
  agentLogo, usageBadge, sysMetricHTML, statusPhaseAt, STATUS_PHASES, setStatusUnreachable,
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
  assert.equal(Object.isFrozen(STATUS_PHASES), true, "exported phase inventory is immutable");
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
  assert.match(agentLogo("muse", url), /U:\/assets\/agents\/meta\.svg/);
  assert.match(agentLogo("dsh", url), /U:\/assets\/agents\/deepseek\.svg/);
  assert.match(agentLogo("unknown", url), /<svg/);
  assert.deepEqual(seen, [
    "/assets/agents/claude.svg",
    "/assets/agents/openai.svg",
    "/assets/agents/openai.svg",
    "/assets/agents/pi.svg",
    "/assets/agents/opencode.svg",
    "/assets/agents/grok.svg",
    "/assets/agents/meta.svg",
    "/assets/agents/deepseek.svg",
  ]);
});

test("dsh keeps its own lowercase name and stays out of the status rotation", () => {
  /* Vendor brands get their capital (Claude, Codex, Grok, Muse); a bare CLI
     name is its own spelling, as for pi and opencode. Renaming dsh to
     "DeepSeek" here would also be a lie about which model answers: this
     harness routes to whichever provider its profile names. */
  assert.equal(usageAgentDisplayName("dsh"), "dsh");
  /* The rotation is for harnesses with a quota API to read. dsh has none, so
     a dsh phase would be a gauge that is permanently unavailable. */
  assert.equal(STATUS_PHASES.includes("dsh"), false);
});

test("sysMetricHTML: flat/up/down and exact critical boundary", () => {
  const flat = sysMetricHTML("M", "50%", 50, 50);
  assert.match(flat, /a-flat/);
  assert.match(flat, /→/);
  assert.match(sysMetricHTML("M", "50%", 50.39, 50), /a-flat/);
  assert.match(sysMetricHTML("M", "50%", 50.41, 50), /a-stress/);
  assert.match(sysMetricHTML("M", "50%", 49.59, 50), /a-good/);
  const up = sysMetricHTML("M", "60%", 60, 50);
  assert.match(up, /a-stress/);
  assert.match(up, /↑/);
  assert.match(up, /roll-dn/);
  const down = sysMetricHTML("M", "40%", 40, 50);
  assert.match(down, /a-good/);
  assert.match(down, /↓/);
  const crit = sysMetricHTML("M", "90%", 90, 80);
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
});

test("usageAgentDisplayName", () => {
  assert.equal(usageAgentDisplayName("claude"), "Claude");
  assert.equal(usageAgentDisplayName("codex"), "Codex");
  assert.equal(usageAgentDisplayName("grok"), "Grok");
  assert.equal(usageAgentDisplayName("other"), "other");
});

test("usageAgentDisplayName names Muse without adding it to usage phases", () => {
  assert.equal(usageAgentDisplayName("muse"), "Muse");
  assert.deepEqual(STATUS_PHASES, ["metrics", "claude", "codex", "grok"]);
  assert.equal(STATUS_PHASES.includes("muse"), false);
  assert.doesNotMatch(usageSrc, /STATUS_PHASES[^\n]*muse/);
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

test("the offline message is a class on the status slot, never a write over its children", () => {
  /* `$("#sys").textContent = "server unreachable"` deleted the four .sysphase
     children permanently — nothing recreates them — so the next renderSys threw
     on a null #sysmetrics and took the poll loop down with it. The message owns
     its own element and the slot is only ever toggled. */
  assert.match(indexSrc, /id="sys">[\s\S]*id="sysoffline"/);
  assert.doesNotMatch(appSrc, /\$\("#sys"\)\.textContent/);
  assert.match(appSrc, /setStatusUnreachable/);

  const cls = new Set();
  const el = { classList: { toggle: (n, on) => (on ? cls.add(n) : cls.delete(n)) } };
  setStatusUnreachable(el, true);
  assert.ok(cls.has("offline"));
  setStatusUnreachable(el, false);
  assert.ok(!cls.has("offline"), "coming back online clears the message");
  setStatusUnreachable(null, true);   /* no element: no throw */

  /* The offline row has to outrank .sysphase.on, which the 8s phase cycler
     keeps re-asserting underneath it. */
  assert.match(layoutCss, /#statusbar #sysoffline\s*\{[^}]*display:\s*none/);
  assert.match(layoutCss, /#statusbar \.sys\.offline \.sysphase\s*\{[^}]*display:\s*none/);
  assert.match(layoutCss, /#statusbar \.sys\.offline #sysoffline\s*\{[^}]*display:\s*inline/);
});

/* A valid response can have a five-hour window and no weekly one at all. The
   badge must read as a session gauge, not as a two-window badge with half of
   it missing — and it must not say "weekly" when there is no weekly quota.
   Values in this test are independently synthetic. */
test("Claude session-only account omits the weekly half", () => {
  const c = usageBadgeLayout("claude", {
    available: true,
    five_hour_remaining: 73,
    five_hour_reset: "2030-01-02T03:04:05Z",
  });
  assert.equal(c.available, true);
  assert.equal(c.has5h, true);
  assert.equal(c.hasW, false);
  assert.match(c.tip, /5h 73% left/);
  assert.doesNotMatch(c.tip, /weekly/);
  assert.doesNotMatch(c.tip, /--/);
});

test("Claude session-only account gets one reset rail", () => {
  const bars = usageResetBars({
    five_hour_remaining: 73,
    five_hour_reset: new Date(Date.now() + 90 * 60_000).toISOString(),
  });
  assert.equal(bars.length, 1);
  assert.equal(bars[0].key, "5h");
});

test("a switched-off gauge is not a broken one", () => {
  /* "off" is the only dark gauge the user can fix by tapping, so it must not
     be spelled the same as "unavailable". The tip is where that difference
     becomes actionable. */
  const off = usageBadgeLayout("claude", { available: false, off: true, reason: "usage checks are off" });
  assert.equal(off.available, false);
  assert.equal(off.off, true);
  assert.match(off.tip, /off/i);
  assert.match(off.tip, /tap/i);

  const broken = usageBadgeLayout("claude", { available: false, reason: "usage unavailable: claude not installed" });
  assert.equal(broken.off, false);
  assert.doesNotMatch(broken.tip, /tap/i);
});

test("the off badge is reachable, the unavailable one is not", () => {
  /* A tappable badge must say so to the accessibility tree as well: an
     img-role span is not something a screen-reader user can act on. */
  const off = usageBadge("claude", { available: false, off: true }, {});
  assert.match(off, /data-usage-off="claude"/);
  assert.match(off, /role="button"/);
  const broken = usageBadge("claude", { available: false }, {});
  assert.doesNotMatch(broken, /data-usage-off/);
  assert.match(broken, /role="img"/);
});
