/* Claude auto-approve UI contracts. Claude reaches the toggle through the
 * PermissionRequest hook, which offers scimux no menu — so the chrome is the
 * same as every other agent's, and the audit surface has to be honest about
 * the missing option list rather than drawing an empty one. */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import {
  AUTO_APPROVE_HELP,
  AUTO_APPROVE_UNSUPPORTED_HELP,
  AUTO_APPROVE_CLAUDE_UNSUPPORTED_HELP,
  autoApproveChromeModel,
  decisionRowHTML,
} from "../js/chat.js";
import { esc } from "../js/format.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const chatJs = readFileSync(join(__dirname, "../js/chat.js"), "utf8");

test("Claude chrome follows the server's supported verdict, not the agent name", () => {
  const armed = autoApproveChromeModel(
    { supported: true, enabled: true, phase: "armed", count: 2 },
    { agent: "claude" },
  );
  assert.equal(armed.supported, true);
  assert.equal(armed.disabled, false);
  assert.equal(armed.pressed, true);
  assert.equal(armed.showWarning, true);
  assert.equal(armed.title, AUTO_APPROVE_HELP);
  assert.ok(!armed.classNames.includes("unsupported"));

  const primed = autoApproveChromeModel(
    { supported: true, enabled: true, phase: "primed", count: 0 },
    { agent: "Claude" },
  );
  assert.equal(primed.enabled, true, "case of the agent name must not matter");
  assert.equal(primed.disabled, false);

  // The hardcoded refusal is gone: nothing in the model may key off "claude".
  const modelStart = chatJs.indexOf("export function autoApproveChromeModel");
  const modelEnd = chatJs.indexOf("export function autoApproveButtonInnerHTML");
  const body = chatJs.slice(modelStart, modelEnd);
  assert.doesNotMatch(body, /agentL\s*===\s*"claude"/,
    "the chrome model must not decide support by agent name");
});

test("unsupported Claude says what would make it supported; others stay generic", () => {
  const claude = autoApproveChromeModel(
    { supported: false, enabled: false, phase: "off", count: 0 },
    { agent: "claude" },
  );
  assert.equal(claude.title, AUTO_APPROVE_CLAUDE_UNSUPPORTED_HELP);
  assert.equal(claude.ariaLabel, AUTO_APPROVE_CLAUDE_UNSUPPORTED_HELP);
  assert.match(AUTO_APPROVE_CLAUDE_UNSUPPORTED_HELP, /relaunch|fork/i,
    "an adopted or pre-feature pane needs a next step, not just a refusal");

  const other = autoApproveChromeModel(
    { supported: false, enabled: false, phase: "off", count: 0 },
    { agent: "codex" },
  );
  assert.equal(other.title, AUTO_APPROVE_UNSUPPORTED_HELP);
  assert.notEqual(AUTO_APPROVE_UNSUPPORTED_HELP, AUTO_APPROVE_CLAUDE_UNSUPPORTED_HELP);

  // Both stay disabled and marked, whatever the wording.
  for (const m of [claude, other]){
    assert.equal(m.disabled, true);
    assert.equal(m.showBadge, false);
    assert.ok(m.classNames.includes("unsupported"));
  }
});

test("decisionRowHTML states the missing menu instead of drawing an empty list", () => {
  const hookDecision = {
    record: 4,
    time: "2026-08-17T09:00:00Z",
    decision: {
      source: "auto",
      lease_id: "lease-1",
      request_id: "0f1e2d3c",
      agent: "claude",
      tool_kind: "execute",
      title: "Bash: ls -la",
      reason: "",
      options: [],
      selected: { name: "Allow once", kind: "allow" },
    },
  };
  const html = decisionRowHTML(hookDecision, { escape: esc, fmtTime: t => t });
  assert.doesNotMatch(html, /<ul class="decision-opts"><\/ul>/,
    "an empty option list reads as 'no options were offered to the human'");
  assert.match(html, /class="decision-none"/);
  assert.match(html, /no menu offered/i);
  // The decision itself is still fully auditable.
  assert.match(html, /Auto-approved/);
  assert.match(html, /Allow once/);
  assert.match(html, /0f1e2d3c/);
  assert.match(html, /lease-1/);
  assert.match(html, /class="decision-cmd"/);
  // A key-less selection must not render a bare empty <code>.
  assert.doesNotMatch(html, /<code><\/code>/);

  // A menu-bearing decision is unchanged.
  const menu = decisionRowHTML({
    record: 5,
    decision: {
      title: "go test ./...",
      options: [{ key: "2", name: "Allow once", kind: "allow" }],
      selected: { key: "2", name: "Allow once", kind: "allow" },
    },
  }, { escape: esc, fmtTime: t => t });
  assert.match(menu, /<ul class="decision-opts">/);
  assert.doesNotMatch(menu, /class="decision-none"/);
});
