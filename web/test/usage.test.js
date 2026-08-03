/* Pure usage-badge layout: weekly-only (Grok) vs two-window (Claude/Codex). */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { usageAgentDisplayName, usageBadgeLayout } from "../js/usage.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const appSrc = readFileSync(join(__dirname, "../js/app.js"), "utf8");
const indexSrc = readFileSync(join(__dirname, "../index.html"), "utf8");

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
