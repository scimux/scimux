/* Characterization tests for web/js/chat.js — pure Chat decisions +
 * factory bind/destroy. Reuses format/lanes/map-model; does not re-test
 * markdown/escaping/time algorithms. */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import {
  ATTENTION_SUPPRESS_MS,
  SENT_ECHO_TIMEOUT_MS,
  SCROLL_NEAR_BOTTOM_PX,
  CHAT_LOAD_DELAY_MS,
  DEC_LABELS,
  RASTER_RE,
  chatRenderDecision,
  attentionIsSuppressed,
  suppressAttentionUntilTime,
  normalizeEchoText,
  retireSentEcho,
  chatActivityPolicy,
  priorSegsFromHistory,
  buildChatSignature,
  permOptionsKey,
  nearBottom,
  liveBk,
  histBk,
  turnRoleClass,
  histLoadHTML,
  pendingEmptyHTML,
  workPulseShouldRun,
  histScrollTargetIndex,
  matchPendingJumpInTurns,
  matchPendingJumpInHist,
  attTileHTML,
  refTilesHTML,
  assetTileHTML,
  splitAssetRefs,
  splitPermTitle,
  bubbleActionsHTML,
  keyRowHTML,
  peekBlockHTML,
  echoBubbleHTML,
  restoreDraftDecision,
  createChatFeature,
} from "../js/chat.js";
/* P6: ASSET_REF_RE + stripAssetRefs live in format.js (one home for every
   send-to strip). Mechanical import-path move from chat.js. */
import {
  ASSET_REF_RE,
  stripAssetRefs,
} from "../js/format.js";
/* Namespace import for symbols a red commit adds: a missing *named* import is a
   module-resolution error that takes the whole file down with it, which hides
   the ~100 tests that were meant to stay green. Through the namespace the same
   absence shows up as failures in exactly the cases that pin it. */
import * as chatmod from "../js/chat.js";
import { bubbleTitle, fmtBubbleTime } from "../js/format.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const chatSrc = readFileSync(join(__dirname, "../js/chat.js"), "utf8");
const chatCssSrc = readFileSync(join(__dirname, "../css/chat.css"), "utf8");

/* ---------- module shape / no later features ---------- */
test("chat.js exports factory and pure helpers; reuses format/lanes/map-model", () => {
  assert.match(chatSrc, /export function createChatFeature/);
  assert.match(chatSrc, /from "\.\/format\.js"/);
  assert.match(chatSrc, /from "\.\/lanes\.js"/);
  assert.match(chatSrc, /from "\.\/map-model\.js"/);
  assert.doesNotMatch(chatSrc, /from "\.\/composer\.js"/);
  assert.doesNotMatch(chatSrc, /from "\.\/polling\.js"/);
  assert.doesNotMatch(chatSrc, /from "\.\/app\.js"/);
  assert.doesNotMatch(chatSrc, /from "\.\/bookmarks\.js"/);
  assert.doesNotMatch(chatSrc, /from "\.\/notes\.js"/);
  assert.doesNotMatch(chatSrc, /from "\.\/search\.js"/);
  assert.doesNotMatch(chatSrc, /from "\.\/sheets\.js"/);
  // must not reimplement format algorithms
  assert.doesNotMatch(chatSrc, /export function esc\b/);
  assert.doesNotMatch(chatSrc, /export function md\b/);
  assert.doesNotMatch(chatSrc, /export function bubbleTitle\b/);
  assert.doesNotMatch(chatSrc, /export function hashStr\b/);
});

test("timing constants match live contracts", () => {
  assert.equal(ATTENTION_SUPPRESS_MS, 2500);
  assert.equal(SENT_ECHO_TIMEOUT_MS, 180000);
  assert.equal(SCROLL_NEAR_BOTTOM_PX, 80);
  assert.equal(CHAT_LOAD_DELAY_MS, 300);
  assert.ok(DEC_LABELS.waiting_approval);
  assert.ok(DEC_LABELS.turn_active);
  assert.ok(RASTER_RE.test("photo.PNG"));
  assert.ok(ASSET_REF_RE.test("![x](scimux-asset:abc)"));
});

test("attention evidence key changes only with kind or fresh evidence epoch", () => {
  assert.equal(chatmod.attentionEvidenceKey({ attention: "inspect", attention_at: 123 }), "inspect:123");
  assert.equal(chatmod.attentionEvidenceKey({ attention: "inspect", attention_at: 123, live: "active" }), "inspect:123");
  assert.notEqual(
    chatmod.attentionEvidenceKey({ attention: "inspect", attention_at: 123 }),
    chatmod.attentionEvidenceKey({ attention: "inspect", attention_at: 124 }),
  );
  assert.notEqual(
    chatmod.attentionEvidenceKey({ attention: "inspect", attention_at: 123 }),
    chatmod.attentionEvidenceKey({ attention: "approval", attention_at: 123 }),
  );
});

/* ---------- signature skip vs rebuild ---------- */
test("chatRenderDecision skip versus rebuild", () => {
  assert.equal(chatRenderDecision("a|b", "a|b"), "skip");
  assert.equal(chatRenderDecision("a|b", "a|c"), "rebuild");
  assert.equal(chatRenderDecision("", "x"), "rebuild");
});

test("buildChatSignature is stable and order-sensitive", () => {
  const base = {
    nodeId: "n1", live: "quiet", attention: "", attentionHidden: false,
    delivery: "ok", showPeek: false, termOpen: false, termFull: false,
    source: "tmux", permTitle: "", permOptionsKey: "", priorTurns: 0,
    chatStarted: "2026-01-01T00:00:00Z", echoHash: "", histKey: "",
    turnsHash: "t", peekHash: "p",
  };
  const a = buildChatSignature(base);
  /* live and peekHash deliberately do not appear — they belong to the pane,
     which has its own host and its own signature. */
  const b = buildChatSignature({ ...base, delivery: "unconfirmed" });
  assert.equal(a, buildChatSignature(base));
  assert.notEqual(a, b);
  // production includes kind so a kind change re-renders the keyrow
  assert.equal(permOptionsKey([{ key: "1", name: "Yes" }, { key: "2", name: "No" }]), "1Yes,2No");
  assert.equal(
    permOptionsKey([{ key: "1", name: "Allow", kind: "allow" }, { key: "2", name: "Reject", kind: "reject" }]),
    "1Allowallow,2Rejectreject",
  );
  assert.notEqual(
    permOptionsKey([{ key: "1", name: "Allow", kind: "allow" }]),
    permOptionsKey([{ key: "1", name: "Allow", kind: "reject" }]),
  );
});

test("buildChatSignature includes expanded; flip rebuilds", () => {
  const base = {
    nodeId: "n1", live: "quiet", attention: "approval", attentionHidden: false,
    delivery: "ok", showPeek: false, termOpen: false, termFull: false,
    source: "acp", permTitle: "Execute `ls`", permOptionsKey: "1Allowallow",
    priorTurns: 0, chatStarted: "2026-01-01T00:00:00Z", echoHash: "",
    histKey: "", turnsHash: "t", peekHash: "p", expanded: false,
  };
  const collapsed = buildChatSignature(base);
  const open = buildChatSignature({ ...base, expanded: true });
  assert.notEqual(collapsed, open, "expanded flip must change the signature");
  assert.equal(buildChatSignature(base), collapsed);
  assert.equal(buildChatSignature({ ...base, expanded: true }), open);
  assert.equal(chatRenderDecision(open, collapsed), "rebuild");
  assert.equal(chatRenderDecision(collapsed, open), "rebuild");
  assert.equal(chatRenderDecision(open, open), "skip");
});

/* ---------- bubble roles / titles / actions / assets ---------- */
test("turnRoleClass and bk keys cover live and history", () => {
  assert.equal(turnRoleClass("user"), "turn user");
  assert.equal(turnRoleClass("assistant", { hist: true }), "turn hist assistant");
  assert.equal(turnRoleClass("user", { media: true, echo: true }), "turn user media echo");
  assert.equal(liveBk(3), "i:3");
  assert.equal(histBk(1, 2), "h:1:2");
});

test("bubbleActionsHTML order: when, fork, sendto, desc, bookmark, copy", () => {
  const html = bubbleActionsHTML({ time: "2026-07-24T09:05:00" }, {
    iconBranch: "B", iconInto: "I", iconCopy: "C",
  });
  assert.match(html, /class="bubwhen"/);
  assert.match(html, /data-bact="fork"/);
  assert.match(html, /data-bact="sendto"/);
  assert.match(html, /data-bact="desc"/);
  assert.match(html, /data-bact="bookmark"/);
  assert.match(html, /data-bact="copy"/);
  assert.ok(html.indexOf("bubwhen") < html.indexOf('data-bact="fork"'));
  /* send-to sits next to fork: both take this text elsewhere, fork creates
     and send-to reuses */
  assert.ok(html.indexOf('data-bact="fork"') < html.indexOf('data-bact="sendto"'));
  assert.ok(html.indexOf('data-bact="sendto"') < html.indexOf('data-bact="desc"'));
  assert.ok(html.indexOf('data-bact="bookmark"') < html.indexOf('data-bact="copy"'));
  assert.doesNotMatch(html, /copybtn/);
});

test("bubbleActionsHTML: send-to is labelled with an ellipsis and the 180° into glyph", () => {
  const html = bubbleActionsHTML({ time: "2026-07-24T09:05:00" }, {
    iconBranch: "B", iconInto: "I", iconCopy: "C",
  });
  const btn = html.match(/<button data-bact="sendto">([\s\S]*?)<\/button>/);
  assert.ok(btn, "send-to button present");
  /* HIG: an action that needs further input before anything happens takes an
     ellipsis. It must not be called "copy" — that label is the clipboard
     action two slots away. */
  assert.match(btn[1], /send to&#8230;|send to…/);
  assert.doesNotMatch(btn[1], /copy/i);
  assert.match(btn[1], /class="rot180"/);
});

test("chat.js send-to: strips asset refs and excludes the source chat", () => {
  /* scimux-asset: markers are node-scoped — the fork path already strips them
     (they would be dead markdown in the target). Send-to must too. */
  assert.match(chatSrc, /openSendTo\(/);
  const call = chatSrc.slice(chatSrc.indexOf('bact === "sendto"'));
  assert.match(call.slice(0, 400), /stripAssetRefs\s*\(/);
  assert.match(call.slice(0, 400), /exceptId/);
});

test("bubbleTitle from format is used for tooltips (accepted helper)", () => {
  const t = "2026-07-24T09:05:00";
  assert.match(bubbleTitle("user", t), /^you — sent /);
  assert.match(bubbleTitle("assistant", t), /^agent — sent /);
  assert.equal(bubbleTitle("user", ""), "you");
  assert.ok(fmtBubbleTime);
});

test("splitAssetRefs projects markers; missing asset is inert chip", () => {
  const assets = {
    a1: { name: "pic.png", mime: "image/png" },
  };
  const r = splitAssetRefs("hello ![x](scimux-asset:a1) world", "n1", assets, {
    iconFile: "F",
  });
  assert.match(r.clean, /hello/);
  assert.match(r.clean, /world/);
  assert.doesNotMatch(r.clean, /scimux-asset/);
  assert.match(r.html, /attthumb|attfile/);
  assert.match(r.html, /a1|pic\.png/);

  const miss = splitAssetRefs("![y](scimux-asset:gone)", "n1", {}, { iconFile: "FILEICON" });
  assert.match(miss.html, /unavailable/);
  assert.match(miss.html, /missing/);
  assert.match(miss.html, /FILEICON/);
});

test("refTilesHTML and attTileHTML for echo path", () => {
  const html = refTilesHTML(
    [{ path: "deadbeef-shot.png", mime: "image/png" }],
    "n1",
    { iconFile: "F" },
  );
  assert.match(html, /attrow/);
  assert.match(html, /attthumb/);
  assert.match(html, /attachments\/deadbeef-shot\.png/);

  const chip = attTileHTML("n1", "deadbeef-notes.md", false, { iconFile: "F" });
  assert.match(chip, /attfile/);
  assert.doesNotMatch(chip, /attthumb/);
});

test("assetTileHTML image vs file and inline data", () => {
  const img = assetTileHTML("n1", "id1", "alt", { name: "a.png", mime: "image/png", inline: true, data: "data:image/png;base64,xx" }, { iconFile: "F" });
  assert.match(img, /attthumb/);
  assert.match(img, /\/api\/nodes\/n1\/assets\/id1/);
  assert.doesNotMatch(img, /data:image\/png/);
  const file = assetTileHTML("n1", "id2", "doc", { name: "a.pdf", mime: "application/pdf" }, { iconFile: "F" });
  assert.match(file, /attfile/);
});

test("assetTileHTML ignores record data and derives URLs from encoded ids", () => {
  const html = assetTileHTML(
    "node/id",
    "a/1",
    "alt",
    { name: "shot.png", mime: "image/png", inline: true, data: "data:image/png;base64,EVIL" },
    { iconFile: "F" },
  );
  assert.match(html, /\/api\/nodes\/node%2Fid\/assets\/a%2F1/);
  assert.doesNotMatch(html, /data:image/);
  assert.doesNotMatch(html, /EVIL/);
  assert.match(html, /href="\/api\/nodes\/node%2Fid\/assets\/a%2F1"/);
  assert.match(html, /src="\/api\/nodes\/node%2Fid\/assets\/a%2F1"/);
});

test("assetTileHTML malicious MIME cannot add an attribute or handler", () => {
  const html = assetTileHTML("n1", "id1", "alt", {
    name: "a.png",
    mime: `image/png" onerror="alert(1)`,
    data: `data:image/png" onerror="alert(1)`,
  }, { iconFile: "F" });
  assert.doesNotMatch(html, /onerror=/);
  assert.doesNotMatch(html, /alert\(1\)/);
  assert.match(html, /\/api\/nodes\/n1\/assets\/id1/);
});

test("preview eligibility is the exact safe-raster allowlist, not /^image\\//", () => {
  const svg = assetTileHTML("n1", "s1", "pic", { name: "x.svg", mime: "image/svg+xml" }, { iconFile: "F" });
  assert.match(svg, /attfile/);
  assert.doesNotMatch(svg, /attthumb/);
  const htmlNamed = assetTileHTML("n1", "h1", "page", { name: "x.html", mime: "image/png" }, { iconFile: "F" });
  assert.match(htmlNamed, /attfile/);
  assert.doesNotMatch(htmlNamed, /attthumb/);
  const png = assetTileHTML("n1", "p1", "pic", { name: "x.PNG", mime: "application/octet-stream" }, { iconFile: "F" });
  assert.match(png, /attthumb/);
});

test("generated attachment and asset HTML has no inline onerror", () => {
  const asset = assetTileHTML("n1", "id1", "alt", { name: "a.png", mime: "image/png" }, { iconFile: "F" });
  const att = attTileHTML("n1", "deadbeef-shot.png", true, { iconFile: "F" });
  const refs = refTilesHTML([{ path: "deadbeef-shot.png", mime: "image/png" }], "n1", { iconFile: "F" });
  for (const html of [asset, att, refs]) {
    assert.doesNotMatch(html, /onerror=/);
  }
});

/* ---------- history surfaces ---------- */
test("priorSegsFromHistory drops the live segment and later seams", () => {
  const segs = [
    { start: "2026-01-01T00:00:00Z", seam: "2026-01-01T00:00:00Z", turns: [{ role: "user", text: "a" }] },
    { start: "2026-01-02T00:00:00Z", seam: "2026-01-02T00:00:00Z", turns: [{ role: "user", text: "b" }] },
    { start: "2026-01-03T00:00:00Z", seam: "2026-01-03T00:00:00Z", turns: [{ role: "user", text: "c" }] },
  ];
  const prior = priorSegsFromHistory(segs, "2026-01-03T00:00:00Z");
  assert.equal(prior.length, 2);
  assert.equal(prior[0].turns[0].text, "a");
  assert.equal(priorSegsFromHistory(null, "2026-01-03T00:00:00Z").length, 0);
});

test("histLoadHTML and pendingEmptyHTML empty/loading states", () => {
  assert.equal(histLoadHTML(0), "");
  assert.match(histLoadHTML(1), /1 turn</);
  assert.match(histLoadHTML(3), /3 turns/);
  assert.match(histLoadHTML(3), /histload/);
  assert.match(pendingEmptyHTML({ freshSurface: true }), /fresh chat/);
  assert.match(pendingEmptyHTML({ freshSurface: false, pending: true }), /waiting for the agent's transcript/);
  assert.match(pendingEmptyHTML({ freshSurface: false, pending: false }), /no readable transcript/);
});

test("histScrollTargetIndex finds earliest seam at-or-after stop time", () => {
  const seams = [
    { seam: "2026-01-01T00:00:00Z" },
    { seam: "2026-01-02T12:00:00Z" },
    { seam: "2026-01-03T00:00:00Z" },
  ];
  assert.equal(histScrollTargetIndex(seams, "seam"), -1);
  assert.equal(histScrollTargetIndex(seams, "2026-01-02T00:00:00Z"), 1);
  assert.equal(histScrollTargetIndex(seams, "2026-01-04T00:00:00Z"), -1);
});

/* ---------- terminal / peek policy ---------- */
test("chatActivityPolicy: forcePeek, showPeek, freshSurface, unconfirmed", () => {
  const quiet = chatActivityPolicy({ turnsLength: 2, termOpen: false });
  assert.equal(quiet.forcePeek, false);
  assert.equal(quiet.showPeek, false);

  const term = chatActivityPolicy({ turnsLength: 2, termOpen: true });
  assert.equal(term.showPeek, true);
  assert.equal(term.forcePeek, false);

  const attn = chatActivityPolicy({ attention: "approval", turnsLength: 2 });
  assert.equal(attn.mustShowPane, true);
  assert.equal(attn.forcePeek, true);
  assert.equal(attn.showPeek, true);
  const inspect = chatActivityPolicy({ attention: "inspect", turnsLength: 2 });
  assert.equal(inspect.mustShowPane, true, "a fresh inspect opens the terminal once");
  assert.equal(inspect.forcePeek, true);
  assert.equal(inspect.showPeek, true);
  const inspectDismissed = chatActivityPolicy({
    attention: "inspect", attentionHidden: true, turnsLength: 2,
  });
  assert.equal(inspectDismissed.mustShowPane, false);
  assert.equal(inspectDismissed.showPeek, false);

  const hidden = chatActivityPolicy({
    attention: "approval", attentionHidden: true, turnsLength: 2,
  });
  assert.equal(hidden.mustShowPane, false);

  const empty = chatActivityPolicy({ turnsLength: 0 });
  assert.equal(empty.forcePeek, true);

  const fresh = chatActivityPolicy({
    turnsLength: 0, source: "acp", priorTurns: 5,
  });
  assert.equal(fresh.freshSurface, true);
  assert.equal(fresh.forcePeek, false);

  const unconf = chatActivityPolicy({ delivery: "unconfirmed", turnsLength: 1 });
  assert.equal(unconf.unconfirmed, true);
  assert.equal(unconf.forcePeek, true);
});

test("chatActivityPolicy: Claude strict never auto-opens except a proven permission dialog", () => {
  const starting = chatActivityPolicy({
    supervision: "claude_starting", turnsLength: 0, fallback: true, delivery: "unconfirmed",
  });
  assert.equal(starting.forcePeek, false);
  assert.equal(starting.mustShowPane, false);
  assert.equal(starting.showPeek, false);

  const inspect = chatActivityPolicy({
    supervision: "claude_strict", attention: "inspect", turnsLength: 0,
  });
  assert.equal(inspect.mustShowPane, false);
  assert.equal(inspect.forcePeek, false);

  const missing = chatActivityPolicy({
    supervision: "claude_strict", fallback: true, turnsLength: 0,
  });
  assert.equal(missing.forcePeek, false);

  const perm = chatActivityPolicy({
    supervision: "claude_strict", attention: "approval", turnsLength: 2,
  });
  assert.equal(perm.mustShowPane, true);
  assert.equal(perm.forcePeek, true);

  const armed = chatActivityPolicy({
    supervision: "claude_strict", attention: "approval", autoApproveArmed: true, turnsLength: 2,
  });
  assert.equal(armed.mustShowPane, false);

  const unsupported = chatActivityPolicy({
    supervision: "claude_unsupported", attention: "inspect", fallback: true, turnsLength: 0,
  });
  assert.equal(unsupported.mustShowPane, false);
  assert.equal(unsupported.forcePeek, false);

  const manual = chatActivityPolicy({
    supervision: "claude_strict", termOpen: true, turnsLength: 2,
  });
  assert.equal(manual.showPeek, true);
  assert.equal(manual.forcePeek, false);
});

test("keyRowHTML: Claude permission bar binds the visible-dialog epoch, not a request id", () => {
  const html = keyRowHTML({
    attention: "approval",
    source: "transcript",
    permTitle: "Bash",
    permOptions: [{ key: "1", name: "Yes", kind: "allow" }, { key: "3", name: "No", kind: "reject" }],
    permDialogId: "epoch-9",
  });
  assert.match(html, /data-dialog-id="epoch-9"/);
  assert.doesNotMatch(html, /data-request-id/);
  assert.match(html, /data-key="1"/);
  assert.doesNotMatch(html, /Quiet/);
});

test("Claude permission click posts dialog_id without claiming request identity", async () => {
  const nodes = [{
    id: "n1", title: "A", agent: "claude", model: "sonnet", live: "quiet",
    attention: "approval", lane_id: "", description: "",
  }];
  const ctx = makeFeature({
    nodes,
    chatPayload: {
      turns: [{ role: "user", text: "q" }],
      live: "quiet", delivery: "", source: "transcript",
      chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
      attention: "approval",
      supervision: "claude_strict",
      perm_title: "Bash",
      perm_options: [
        { key: "1", name: "Yes", kind: "allow" },
        { key: "3", name: "No", kind: "reject" },
      ],
      perm_dialog_id: "epoch-click-7",
    },
  });
  ctx.feature.bind();
  await ctx.feature.render();
  assert.match(ctx.roots.keyrow.innerHTML, /data-dialog-id="epoch-click-7"/);
  assert.doesNotMatch(ctx.roots.keyrow.innerHTML, /data-request-id/);
  const btn = el("button", { dataset: { key: "1", dialogId: "epoch-click-7" } });
  firstListener(ctx.roots.keyrow, "click")({ target: btn });
  await new Promise(resolve => setImmediate(resolve));
  const keyCalls = ctx.apiCalls.filter(c => c.path.includes("/key"));
  assert.equal(keyCalls.length, 1);
  const body = JSON.parse(keyCalls[0].opts?.body || "{}");
  assert.equal(body.key, "1");
  assert.equal(body.dialog_id, "epoch-click-7");
  assert.equal(body.request_id, undefined);
  ctx.feature.destroy();
});

test("pending_prompt seeds a pale echo; restore_draft clears it", async () => {
  const restored = [];
  const ctx = makeFeature({
    chatPayload: {
      turns: [],
      live: "quiet",
      delivery: "unconfirmed",
      pending_prompt: "hello pale",
      supervision: "claude_starting",
      pending: true,
      chat_started: "2026-01-01T00:00:00Z",
      prior_turns: 0,
      assets: {},
    },
    restoreDraft: (id, text) => restored.push({ id, text }),
  });
  ctx.feature.bind();
  await ctx.feature.render();
  assert.match(ctx.roots.msgs.innerHTML, /hello pale/);
  assert.match(ctx.roots.msgs.innerHTML, /echo/);
  ctx.feature.destroy();

  const ctx2 = makeFeature({
    chatPayload: {
      turns: [],
      live: "quiet",
      delivery: "",
      restore_draft: "hello pale",
      error: "Claude did not start.",
      supervision: "claude_failed",
      pending: true,
      chat_started: "2026-01-01T00:00:00Z",
      prior_turns: 0,
      assets: {},
    },
    restoreDraft: (id, text) => restored.push({ id, text }),
  });
  ctx2.feature.setSentEcho({ node: "n1", text: "hello pale", atts: [], at: Date.now(), seen: null });
  ctx2.feature.bind();
  await ctx2.feature.render();
  assert.equal(restored.length, 1);
  assert.equal(restored[0].text, "hello pale");
  assert.doesNotMatch(ctx2.roots.msgs.innerHTML, /echo/);
  ctx2.feature.destroy();
});

test("restoreDraftDecision keeps newer composer input", () => {
  assert.equal(restoreDraftDecision({ original: "first prompt", current: "" }).action, "restore");
  assert.equal(restoreDraftDecision({ original: "first prompt", current: "first prompt" }).action, "restore");
  const typed = restoreDraftDecision({ original: "first prompt", current: "newer follow-up" });
  assert.equal(typed.action, "keep");
  assert.equal(typed.text, "newer follow-up");
});

test("restoreDraftDecision uses exact text equality", () => {
  const orig = "first prompt";
  const wsOnly = restoreDraftDecision({ original: orig, current: "   " });
  assert.equal(wsOnly.action, "keep");
  assert.equal(wsOnly.text, "   ");
  const lead = restoreDraftDecision({ original: orig, current: " first prompt" });
  assert.equal(lead.action, "keep");
  assert.equal(lead.text, " first prompt");
  const trail = restoreDraftDecision({ original: orig, current: "first prompt " });
  assert.equal(trail.action, "keep");
  assert.equal(trail.text, "first prompt ");
  const multi = restoreDraftDecision({ original: "line\n\n", current: "line\n \n" });
  assert.equal(multi.action, "keep");
  assert.equal(multi.text, "line\n \n");
  const newer = restoreDraftDecision({ original: orig, current: "rewritten" });
  assert.equal(newer.action, "keep");
  assert.equal(newer.text, "rewritten");
  const empty = restoreDraftDecision({ original: orig, current: "" });
  assert.equal(empty.action, "restore");
  assert.equal(empty.text, orig);
  const same = restoreDraftDecision({ original: orig, current: orig });
  assert.equal(same.action, "restore");
  assert.equal(same.text, orig);
});

test("restore_draft does not overwrite a draft typed during pending startup", async () => {
  const restored = [];
  const storage = {
    data: { "scimux-draft:n1": "typed while starting" },
    getItem(k){ return this.data[k] || null; },
    setItem(k, v){ this.data[k] = v; },
  };
  const ctx = makeFeature({
    chatPayload: {
      turns: [],
      live: "quiet",
      delivery: "",
      restore_draft: "hello pale",
      error: "Claude did not start.",
      supervision: "claude_failed",
      pending: true,
      chat_started: "2026-01-01T00:00:00Z",
      prior_turns: 0,
      assets: {},
    },
    storage,
    composerText: () => "typed while starting",
    restoreDraft: (id, text) => restored.push({ id, text }),
  });
  ctx.feature.bind();
  await ctx.feature.render();
  assert.equal(restored.length, 0, "must not call restoreDraft over newer input");
  assert.equal(storage.getItem("scimux-draft:n1"), "typed while starting");
  ctx.feature.destroy();
});

test("keyRowHTML: Claude question without proven options is manual + Open Terminal", () => {
  const html = keyRowHTML({
    attention: "question",
    source: "transcript",
    permTitle: "AskUserQuestion",
    permOptions: [],
    permDialogId: "epoch-q",
    permManual: true,
    permReason: "A dialog is visible but its options could not be read safely. Open Terminal to respond.",
  });
  assert.match(html, /Open Terminal/);
  assert.match(html, /data-open-terminal/);
  assert.doesNotMatch(html, /Yes, and don.?t ask again/);
  assert.doesNotMatch(html, /data-key="1"/);
  assert.doesNotMatch(html, /allow always/i);
});

test("keyRowHTML: epoch-bound Claude surface renders without map attention", () => {
  const html = keyRowHTML({
    attention: "",
    source: "transcript",
    permTitle: "AskUserQuestion",
    permOptions: [{ key: "1", name: "Yes", kind: "allow" }],
    permDialogId: "epoch-q",
    permReason: "Auto-approve is armed and will not answer this. Choose below, or open Terminal.",
  });
  assert.match(html, /data-dialog-id="epoch-q"/);
  assert.doesNotMatch(html, /data-key="y"/);
  assert.match(html, /Choose below/);
});

test("chatActivityPolicy: Claude question auto-opens only when auto-approve is off", () => {
  const off = chatActivityPolicy({
    supervision: "claude_strict", attention: "question", turnsLength: 2,
  });
  assert.equal(off.mustShowPane, true);
  const armed = chatActivityPolicy({
    supervision: "claude_strict", attention: "question", autoApproveArmed: true, turnsLength: 2,
  });
  assert.equal(armed.mustShowPane, false);
});

test("pendingEmptyHTML: Claude startup does not mention the raw terminal", () => {
  const html = pendingEmptyHTML({ pending: true, supervision: "claude_starting" });
  assert.match(html, /SessionStart/);
  assert.doesNotMatch(html, /raw terminal/i);
  const other = pendingEmptyHTML({ pending: true });
  assert.match(other, /raw terminal/i);
});

test("peekBlockHTML and keyRowHTML presentation contracts", () => {
  const peek = peekBlockHTML({
    forcePeek: true, label: "Needs your attention", termFull: false,
    peekText: "line1", unconfirmed: true,
  });
  assert.match(peek, /peekblock/);
  assert.match(peek, /Needs your attention/);
  assert.match(peek, /id="termfull"/);
  assert.match(peek, /decresolve/);
  assert.match(peek, /expand terminal/);

  const full = peekBlockHTML({
    forcePeek: false, termFull: true, peekText: "x", unconfirmed: false,
  });
  assert.match(full, /shrink terminal/);
  assert.match(full, /class="peek full"/);
  assert.match(full, />Terminal</);

  assert.equal(keyRowHTML({ attention: "", attentionHidden: false }), "");
  assert.equal(keyRowHTML({ attention: "q", attentionHidden: true }), "");
  const acp = keyRowHTML({
    attention: "approval", source: "acp",
    permTitle: "Edit", permOptions: [{ key: "1", name: "Allow" }],
  });
  assert.match(acp, /data-key="1"/);
  assert.match(acp, /Allow/);
  const tmux = keyRowHTML({ attention: "question" });
  assert.match(tmux, /data-key="y"/);
  assert.match(tmux, /data-key="Enter"/);
  assert.match(tmux, /class="permbtns keys"/, "tmux keys use .permbtns.keys wrap layout");
  const waiting = keyRowHTML({
    attention: "approval", source: "acp", permTitle: "X", permOptions: [],
  });
  assert.match(waiting, /Waiting for approval options/);
  assert.doesNotMatch(waiting, /permbtns/);
});

/* ---------- P3 approval reachability: pure helpers + keyrow structure ---------- */

test("splitPermTitle: verb+backtick shapes vs fallthrough", () => {
  assert.deepEqual(splitPermTitle("Execute `ls -la`"), { verb: "Execute", code: "ls -la" });
  assert.deepEqual(splitPermTitle("Read `/abs/path`"), { verb: "Read", code: "/abs/path" });
  assert.deepEqual(splitPermTitle("Edit `/abs/path`"), { verb: "Edit", code: "/abs/path" });
  assert.deepEqual(splitPermTitle("Search `foo.*bar`"), { verb: "Search", code: "foo.*bar" });
  // bare regex / prose / empty / null — never throw, never null
  assert.deepEqual(splitPermTitle("foo.*bar"), { verb: "", code: "foo.*bar" });
  assert.deepEqual(splitPermTitle("thinking hard about it"), { verb: "", code: "thinking hard about it" });
  assert.deepEqual(splitPermTitle(""), { verb: "", code: "" });
  assert.deepEqual(splitPermTitle(null), { verb: "", code: "" });
  assert.deepEqual(splitPermTitle(undefined), { verb: "", code: "" });
  // 6 KB multi-line payload survives byte-identical (the phone overflow case)
  const payload = "#!/bin/bash\n" + "x".repeat(6000) + "\n# heading\n* bullet\n| a | b |\necho done";
  const r = splitPermTitle("Execute `" + payload + "`");
  assert.equal(r.verb, "Execute");
  assert.equal(r.code, payload);
});

test("keyRowHTML: never md() on perm_title; payload escapes; structure and kinds", () => {
  // md() regression pin: title with heading/bullet/table lines must stay
  // verbatim inside <pre> — no <h1>/<li>/<table> from the markdown path.
  const poison = "# heading\n* bullet\n| a | b |\n<script>x</script>";
  const html = keyRowHTML({
    attention: "approval",
    source: "acp",
    permTitle: "Execute `" + poison + "`",
    permToolKind: "execute",
    permOptions: [
      { key: "1", name: "Allow once", kind: "allow" },
      { key: "2", name: "Allow always", kind: "allow_always" },
      { key: "3", name: "Reject once", kind: "reject" },
      { key: "4", name: "Reject always", kind: "reject_always" },
      { key: "5", name: "Maybe", kind: "" },
    ],
    expanded: false,
  });
  assert.doesNotMatch(html, /<h[1-3][\s>]/i, "no heading tags from md()");
  assert.doesNotMatch(html, /<li[\s>]/i, "no list items from md()");
  assert.doesNotMatch(html, /<table[\s>]/i, "no table from md()");
  assert.doesNotMatch(html, /<script>/i, "payload must be escaped");
  assert.match(html, /&lt;script&gt;/);
  assert.match(html, /# heading/);
  assert.match(html, /\* bullet/);
  assert.match(html, /\| a \| b \|/);
  assert.match(html, /<pre class="permcode">/);
  assert.match(html, /class="permverb"[^>]*>Execute</);
  assert.match(html, /class="permask clamped"/);
  assert.match(html, /class="permmore"/);
  assert.match(html, /show all/);

  // .permbtns: one button per option, agent order, digit prefixes, kind classes
  assert.match(html, /class="permbtns"/);
  const btnOrder = [...html.matchAll(/data-key="(\d+)"[^>]*class="([^"]+)"[^>]*>([^<]+)</g)];
  // class may come before or after data-key — match more loosely
  const labels = [...html.matchAll(/data-key="(\d+)"[^>]*>([^<]+)</g)].map(m => [m[1], m[2].trim()]);
  assert.deepEqual(labels.map(l => l[0]), ["1", "2", "3", "4", "5"]);
  assert.match(labels[0][1], /^1\.\s+Allow once$/);
  assert.match(labels[1][1], /^2\.\s+Allow always$/);
  assert.match(labels[2][1], /^3\.\s+Reject once$/);
  assert.match(labels[3][1], /^4\.\s+Reject always$/);
  assert.match(labels[4][1], /^5\.\s+Maybe$/);
  assert.match(html, /class="permbtn allow"/);
  assert.match(html, /class="permbtn allow-always"/);
  assert.match(html, /class="permbtn reject"/);
  assert.match(html, /class="permbtn reject-always"/);
  assert.match(html, /class="permbtn unknown"/);

  // expanded=true: no clamp, no show-all
  const open = keyRowHTML({
    attention: "approval", source: "acp",
    permTitle: "Execute `cmd`",
    permOptions: [{ key: "1", name: "Allow once", kind: "allow" }],
    expanded: true,
  });
  assert.doesNotMatch(open, /clamped/);
  assert.doesNotMatch(open, /permmore/);
});

/* P5b item 2: .permreason must sit outside .permask so a multi-line reason
 * does not consume the three-line clamp that belongs to the ask body. */
test("P5b: .permreason is not a descendant of .permask (clamp is body-only)", () => {
  const html = keyRowHTML({
    attention: "approval", source: "acp",
    permTitle: "Execute `curl example.com`",
    permToolKind: "execute",
    permReason: "Line one of the reason.\nLine two of the reason.\nLine three would steal the body.",
    permOptions: [{ key: "1", name: "Allow once", kind: "allow" }],
    expanded: false,
  });
  assert.match(html, /class="permreason"/, "reason is present");
  assert.match(html, /class="permask clamped"/, "mask is clamped when not expanded");
  const parts = permaskInnerAndRest(html);
  assert.ok(parts, "emits a .permask element");
  assert.doesNotMatch(parts.inner, /permreason/,
    ".permreason must not live inside .permask (would eat the 3-line clamp)");
  assert.match(parts.before, /class="permreason"/,
    ".permreason sits before .permask (with the hint)");
  assert.match(parts.inner, /permverb|permcode|permtext/,
    "verb/body stay inside the mask");
  /* document order: hint → reason → mask */
  const hintAt = html.search(/class="hint"/);
  const reasonAt = html.search(/class="permreason"/);
  const maskAt = html.search(/class="permask/);
  assert.ok(hintAt >= 0 && reasonAt > hintAt && maskAt > reasonAt,
    "order is hint → permreason → permask");
});

/* G1b: reason is a distinct dimmed line above the ask body (.permreason),
 * not mixed into .permverb or the code/prose body. Escaped, optional. */
test("G1b: keyRowHTML renders permReason above the body, escaped and distinct", () => {
  const html = keyRowHTML({
    attention: "approval", source: "acp",
    permTitle: "Execute `curl example.com`",
    permToolKind: "execute",
    permReason: "DNS failed. Allow <network>?",
    permOptions: [{ key: "1", name: "Allow once", kind: "allow" }],
    expanded: true,
  });
  assert.match(html, /class="permreason"/, "reason has its own class");
  assert.match(html, /DNS failed\. Allow &lt;network&gt;\?/,
    "reason must be HTML-escaped");
  assert.doesNotMatch(html, /Allow <network>/, "raw angle brackets must not land");
  // Order: reason, then verb, then body — not the same visual line.
  const reasonIdx = html.indexOf("permreason");
  const verbIdx = html.indexOf("permverb");
  const codeIdx = html.indexOf("permcode");
  assert.ok(reasonIdx >= 0 && verbIdx > reasonIdx && codeIdx > verbIdx,
    "order is reason → verb → body");
  // Verb and reason are separate elements.
  assert.match(html, /class="permreason"[^>]*>DNS failed/);
  assert.match(html, /class="permverb"[^>]*>Execute</);

  // Absent reason: no .permreason element.
  const noReason = keyRowHTML({
    attention: "approval", source: "acp",
    permTitle: "Execute `ls`",
    permToolKind: "execute",
    permOptions: [{ key: "1", name: "Allow once", kind: "allow" }],
    expanded: true,
  });
  assert.doesNotMatch(noReason, /permreason/,
    "empty/absent reason must not emit a dead line");
});

/* Cross-agent matrix (P1 deliverable): codex / grok / pi / opencode share one
 * keyRowHTML path. Pins body element choice, reachability, buttons, escaping
 * across tool kinds × title shapes × reason presence. */
test("P1 matrix: body element, reachability, buttons, escape across agents", () => {
  const CODE_KINDS = new Set(["execute", "edit", "read", "search"]);
  const kinds = ["execute", "edit", "read", "search", "think", "fetch", "other", ""];
  const titles = {
    short: "ls -la",
    long: "x".repeat(400),
    multi: "cat <<EOF\n  indented line\nEOF",
  };
  const reasons = {
    present: "DNS failed — allow <net>?",
    absent: "",
  };
  const opts = [
    { key: "1", name: "Allow once", kind: "allow" },
    { key: "2", name: "Reject once", kind: "reject" },
  ];
  let rows = 0;
  for (const kind of kinds){
    for (const [titleKey, title] of Object.entries(titles)){
      for (const [reasonKey, reason] of Object.entries(reasons)){
        rows++;
        const label = `kind=${kind||"(empty)"} title=${titleKey} reason=${reasonKey}`;
        const poisonTitle = title + "<script>x</script>";
        const html = keyRowHTML({
          attention: "approval", source: "acp",
          permTitle: poisonTitle,
          permToolKind: kind,
          permReason: reason,
          permOptions: opts,
          expanded: false,
        });
        const wantCode = CODE_KINDS.has(kind);
        if (wantCode){
          assert.match(html, /<pre class="permcode">/, `${label}: code body`);
          assert.doesNotMatch(html, /class="permtext"/, `${label}: not prose`);
        } else {
          assert.match(html, /class="permtext"/, `${label}: prose body`);
          assert.doesNotMatch(html, /<pre class="permcode">/, `${label}: not code`);
        }
        /* Full ask is in the DOM (clamp is CSS-only). Poison is escaped. */
        assert.match(html, /&lt;script&gt;/, `${label}: title escaped`);
        assert.doesNotMatch(html, /<script>/, `${label}: no raw script`);
        if (titleKey === "long"){
          assert.match(html, /x{400}/, `${label}: full long command present`);
        }
        if (titleKey === "multi"){
          assert.match(html, /indented line/, `${label}: multi-line body kept`);
          assert.match(html, /  indented/, `${label}: leading indent survives in HTML`);
        }
        if (reasonKey === "present"){
          assert.match(html, /class="permreason"/, `${label}: reason line`);
          assert.match(html, /allow &lt;net&gt;\?/, `${label}: reason escaped`);
        } else {
          assert.doesNotMatch(html, /permreason/, `${label}: no reason line`);
        }
        /* Collapsed always emits show-all control (measure pass may unhide). */
        assert.match(html, /class="permmore"/, `${label}: show all available`);
        assert.match(html, /class="permask clamped"/, `${label}: clamped mask`);
        /* Answer buttons with 44px targets still emitted. */
        assert.match(html, /class="permbtns"/, `${label}: buttons row`);
        assert.match(html, /data-key="1"/, `${label}: allow button`);
        assert.match(html, /data-key="2"/, `${label}: reject button`);
        assert.match(html, /class="permbtn allow"/, `${label}: allow class`);
        assert.match(html, /class="permbtn reject"/, `${label}: reject class`);
      }
    }
  }
  assert.equal(rows, kinds.length * Object.keys(titles).length * Object.keys(reasons).length,
    "matrix row count");
  /* Structural pin: .permcode wraps so long single-line can grow vertically. */
  const codeBody = permcodeRuleBody(chatCssSrc);
  assert.match(codeBody, /white-space\s*:\s*pre-wrap/);
  /* Short-viewport pin: .permbtns stays flex: 0 0 auto so 44px targets survive. */
  const btns = chatCssSrc.match(/\.permbtns\s*\{([^}]+)\}/);
  assert.ok(btns, ".permbtns rule");
  assert.match(btns[1], /flex\s*:\s*0\s+0\s+auto/);
});

test("keyRowHTML: code vs prose and codex label fallback", () => {
  // codex bare command + permToolKind execute → <pre class="permcode">
  const codex = keyRowHTML({
    attention: "approval", source: "acp",
    permTitle: "npm install left-pad",
    permToolKind: "execute",
    permOptions: [{ key: "1", name: "accept", kind: "allow" }],
    expanded: true,
  });
  assert.match(codex, /<pre class="permcode">/);
  assert.doesNotMatch(codex, /class="permverb"/, "no verb when title has no backticks");
  assert.match(codex, /npm install left-pad/);
  // enum-looking name + known kind → readable label; raw enum in title=
  assert.match(codex, /title="accept"/);
  assert.match(codex, />1\. Allow</);

  // think/prose → .permtext, not pre
  const think = keyRowHTML({
    attention: "approval", source: "acp",
    permTitle: "thinking about the plan",
    permToolKind: "think",
    permOptions: [{ key: "1", name: "Allow once", kind: "allow" }],
    expanded: true,
  });
  assert.match(think, /class="permtext"/);
  assert.doesNotMatch(think, /<pre class="permcode">/);

  // already-human Name (has a space) is left alone; no forced title=
  const human = keyRowHTML({
    attention: "approval", source: "acp",
    permTitle: "Edit `/x`",
    permToolKind: "edit",
    permOptions: [{ key: "1", name: "Allow once", kind: "allow" }],
    expanded: true,
  });
  assert.match(human, />1\. Allow once</);
  assert.doesNotMatch(human, /title="Allow once"/);

  // unknown kind + enum-looking name → show name as-is
  const unk = keyRowHTML({
    attention: "approval", source: "acp",
    permTitle: "x",
    permOptions: [{ key: "1", name: "declinePermissions", kind: "" }],
    expanded: true,
  });
  assert.match(unk, />1\. declinePermissions</);
  assert.match(unk, /class="permbtn unknown"/);

  // reject_always enum fallback
  const rej = keyRowHTML({
    attention: "approval", source: "acp",
    permTitle: "x",
    permOptions: [
      { key: "1", name: "cancel", kind: "reject" },
      { key: "2", name: "declinePermissions", kind: "reject_always" },
    ],
    expanded: true,
  });
  assert.match(rej, /title="cancel"/);
  assert.match(rej, />1\. Reject</);
  assert.match(rej, /title="declinePermissions"/);
  assert.match(rej, />2\. Reject always</);
});

test("stripAssetRefs: marker→alt, empty dropped, other markdown untouched", () => {
  assert.equal(stripAssetRefs("see ![shot](scimux-asset:a_1) here"), "see shot here");
  assert.equal(stripAssetRefs("x ![](scimux-asset:a_1) y"), "x  y");
  assert.equal(
    stripAssetRefs("keep [link](https://example.com) and ![img](https://example.com/a.png)"),
    "keep [link](https://example.com) and ![img](https://example.com/a.png)",
  );
  // blank lines left by a dropped marker collapse
  assert.equal(
    stripAssetRefs("before\n\n![](scimux-asset:z)\n\n\nafter"),
    "before\n\nafter",
  );
  const multi = stripAssetRefs("a ![one](scimux-asset:1) b [two](scimux-asset:2) c");
  assert.equal(multi, "a one b two c");
  assert.doesNotMatch(stripAssetRefs("![x](scimux-asset:a) ![y](scimux-asset:b)"), /scimux-asset:/);
  assert.equal(stripAssetRefs(""), "");
  assert.equal(stripAssetRefs(null), "");
  // fork call site must strip before handing text to sheets
  assert.match(chatSrc, /forkFromTurn\(\s*stripAssetRefs\s*\(/);
});

test("approval keyrow CSS pins .permbtns and meets 44px touch target", () => {
  const btns = chatCssSrc.match(/\.permbtns\s*\{([^}]+)\}/);
  assert.ok(btns, ".permbtns rule present");
  assert.match(btns[1], /flex:\s*0\s*0\s*auto/, ".permbtns is flex: 0 0 auto (the pin guarantee)");
  assert.match(btns[1], /flex-direction:\s*column/, ".permbtns keeps column stack for ACP labels");
  const btn = chatCssSrc.match(/\.permbtn\s*\{([^}]+)\}/);
  assert.ok(btn, ".permbtn rule present");
  const mh = btn[1].match(/min-height:\s*(\d+)px/);
  assert.ok(mh, ".permbtn declares min-height");
  assert.ok(Number(mh[1]) >= 44, "min-height at least 44px for touch");
});

test("tmux key labels centre in their button; ACP option labels stay left", () => {
  /* The ten tmux keys are content-sized with min-width: 44px, so a one-glyph
     label ("1", "y", "↑") fills only ~31px of the box. Inheriting .permbtn's
     text-align: left from the ACP option buttons pushes every short label
     off-centre by the leftover width. Centre the keys variant only — long ACP
     labels still read better flush left. */
  const keys = chatCssSrc.match(/\.permbtns\.keys\s+\.permbtn\s*\{([^}]+)\}/);
  assert.ok(keys, ".permbtns.keys .permbtn rule present");
  assert.match(keys[1], /text-align:\s*center/, "short key labels are centred");
  assert.match(keys[1], /justify-content:\s*center/, "centred along the flex axis too");
  assert.match(keys[1], /display:\s*(inline-)?flex/, "flex box so justify/align apply");
  assert.match(keys[1], /align-items:\s*center/, "and vertically inside the 44px target");
  assert.match(keys[1], /min-width:\s*44px/, "the 44px touch target survives the fix");

  const btn = chatCssSrc.match(/\.permbtn\s*\{([^}]+)\}/);
  assert.match(btn[1], /text-align:\s*left/, "ACP option buttons keep their left alignment");
});

/* ---------- P5: tmux keys wrap; show-all outside clamp; restore ACP hint ---------- */

/* Extract the .permask element body (between its open tag and its balanced
   closing </div>). Nested <div>s inside the mask (verb, prose) are handled
   by depth counting on div tags only. Returns null if no .permask. */
function permaskInnerAndRest(html){
  const open = /<div class="permask(?:\s+[^"]*)?">/.exec(html);
  if (!open) return null;
  let i = open.index + open[0].length;
  let depth = 1;
  while (i < html.length && depth > 0){
    const openAt = html.indexOf("<div", i);
    const closeAt = html.indexOf("</div>", i);
    if (closeAt < 0) return null;
    if (openAt >= 0 && openAt < closeAt){
      depth++;
      i = openAt + 4;
    } else {
      depth--;
      if (depth === 0){
        return {
          inner: html.slice(open.index + open[0].length, closeAt),
          after: html.slice(closeAt + "</div>".length),
          before: html.slice(0, open.index),
        };
      }
      i = closeAt + 6;
    }
  }
  return null;
}

test("keyRowHTML: tmux .permbtns has keys; ACP .permbtns does not", () => {
  const tmux = keyRowHTML({ attention: "question" });
  const tmuxBtns = tmux.match(/class="(permbtns[^"]*)"/);
  assert.ok(tmuxBtns, "tmux emits .permbtns");
  assert.match(tmuxBtns[1], /\bkeys\b/, "tmux container class list contains keys");

  const acp = keyRowHTML({
    attention: "approval", source: "acp",
    permTitle: "Edit `/x`",
    permOptions: [{ key: "1", name: "Allow once", kind: "allow" }],
  });
  const acpBtns = acp.match(/class="(permbtns[^"]*)"/);
  assert.ok(acpBtns, "ACP emits .permbtns");
  assert.doesNotMatch(acpBtns[1], /\bkeys\b/, "ACP .permbtns must not carry keys");
});

/* Neutral inspect uses the default terminal keypad plus a distinct Dismiss.
   Dismiss is local acknowledgement; Escape stays a remote key so a
   misclassified dialog can still be backed out of. */
test("keyRowHTML: inspect has the default action bar plus Dismiss", () => {
  const html = keyRowHTML({ attention: "inspect" });
  assert.ok(html, "inspect still renders a chat row");
  assert.match(html, /inspect if needed|No visible progress/i,
    "neutral diagnostic copy points at terminal inspection");
  assert.match(html, /class="permbtns keys"/, "inspect uses the yellow action bar");
  for (const k of ["1", "2", "3", "4", "y", "n", "Up", "Down", "Enter", "Escape"]){
    assert.match(html, new RegExp(`data-key="${k}"`), `inspect keeps remote key ${k}`);
  }
  assert.match(html, /data-dismiss-attention="inspect"/,
    "Dismiss is a local acknowledgement rather than a terminal key");
  assert.match(html, />Dismiss</, "the extra button is labelled Dismiss");
  assert.match(html, /aria-label="[^"]*does not send Escape/i,
    "Dismiss must say it does not send Escape");
  assert.match(html, /title="[^"]*do not send Escape/i,
    "Dismiss title must distinguish it from remote Escape");

  const ax = keyRowHTML({ attention: "inspect", axScreenReader: true });
  assert.match(ax, /data-dismiss-attention="inspect"/, "AX inspect still has Dismiss");
  assert.doesNotMatch(ax, /data-key=/,
    "AX inspect must not offer a remote keypad: 1 becomes 1+Enter and submits");

  // Verified terminal decision rows keep their remote Escape and no Dismiss.
  const approval = keyRowHTML({ attention: "approval" });
  assert.match(approval, /data-key="1"/, "approval retains digit keys");
  assert.match(approval, /permbtns/, "approval retains .permbtns");
  assert.doesNotMatch(approval, /data-dismiss-attention/,
    "classified attention is not locally dismissable");
  const question = keyRowHTML({ attention: "question" });
  assert.match(question, /data-key="y"/, "question retains y/n keys");
  assert.match(question, /data-key="Enter"/, "question retains Enter");
  assert.match(question, /data-key="Escape"/, "question retains remote Escape");
});

/* P5: classified dialog (lettered workspace-trust, no transcript) is the
   whole point of promoting inspect → dialog — the y/n/Enter keypad. */
test("dialog attention offers y/n/Enter", () => {
  const html = keyRowHTML({ attention: "dialog", source: "" });
  assert.match(html, /data-key="y"/);
  assert.match(html, /data-key="n"/);
  assert.match(html, /data-key="Enter"/);
});

test("CSS: .permbtns.keys wraps in a row; base .permbtns stays column pin", () => {
  // Base pin (column + non-shrink) — already asserted above; re-pin direction here.
  const base = chatCssSrc.match(/\.permbtns\s*\{([^}]+)\}/);
  assert.ok(base, ".permbtns rule present");
  assert.match(base[1], /flex-direction:\s*column/);
  assert.match(base[1], /flex:\s*0\s*0\s*auto/);

  const keys = chatCssSrc.match(/\.permbtns\.keys\s*\{([^}]+)\}/);
  assert.ok(keys, ".permbtns.keys rule present");
  assert.match(keys[1], /flex-direction:\s*row/, "tmux keys lay out in a row");
  assert.match(keys[1], /flex-wrap:\s*wrap/, "tmux keys wrap to multiple rows");

  const keyBtn = chatCssSrc.match(/\.permbtns\.keys\s+\.permbtn\s*\{([^}]+)\}/);
  assert.ok(keyBtn, ".permbtns.keys .permbtn rule present");
  assert.match(keyBtn[1], /width:\s*auto/, "key buttons are auto-width, not full-width stack");
  assert.match(keyBtn[1], /min-width:\s*44px/, "key buttons keep 44px touch target");
});

test("keyRowHTML: .permmore is a sibling of .permask, not inside it", () => {
  const html = keyRowHTML({
    attention: "approval", source: "acp",
    permTitle: "Execute `ls -la /very/long/path && echo done`",
    permToolKind: "execute",
    permOptions: [{ key: "1", name: "Allow once", kind: "allow" }],
    expanded: false,
  });
  const parts = permaskInnerAndRest(html);
  assert.ok(parts, "emits a .permask element");
  assert.doesNotMatch(parts.inner, /permmore/, ".permmore must not live inside the clamp");
  assert.match(parts.after, /permmore/, ".permmore must appear after .permask closes");
  // Order inside #keyrow: ask → show-all → buttons
  const moreAt = parts.after.indexOf("permmore");
  const btnsAt = parts.after.indexOf("permbtns");
  assert.ok(moreAt >= 0 && btnsAt > moreAt, ".permmore sits between .permask and .permbtns");
});

test("keyRowHTML: ACP branch restores the approval hint above .permask", () => {
  const html = keyRowHTML({
    attention: "approval", source: "acp",
    permTitle: "Execute `cmd`",
    permToolKind: "execute",
    permOptions: [{ key: "1", name: "Allow once", kind: "allow" }],
  });
  assert.match(html, /class="hint"/, "ACP approval emits a hint");
  assert.match(
    html,
    /The agent needs your approval\s*(?:&mdash;|\u2014|—)\s*choose one:/,
    "restored wording: The agent needs your approval — choose one:",
  );
  // Must not re-introduce the tool title into the hint (title is the ask below).
  assert.doesNotMatch(
    html,
    /class="hint"[^>]*>[^<]*Execute/,
    "hint must not mention the tool title",
  );
  const parts = permaskInnerAndRest(html);
  assert.ok(parts, "emits .permask");
  assert.match(parts.before, /class="hint"/, "hint appears before .permask");
  assert.doesNotMatch(parts.inner, /class="hint"/, "hint is outside the clamp");
});

test("source guard: keyrow call site reads per-node expanded, not hardcoded false", () => {
  // P3 left expanded: false with a P4 ownership comment; P4 must wire the flag.
  assert.doesNotMatch(
    chatSrc,
    /expanded:\s*false\s*,\s*\/\*\s*P4 owns the expanded flag/,
    "call site must not hardcode expanded: false with the P4 stub comment",
  );
  assert.match(
    chatSrc,
    /const\s+expanded\s*=\s*permExpanded\.node\s*===\s*n\.id\s*&&\s*permExpanded\.open/,
    "call site computes expanded from the per-node flag",
  );
  // keyRowHTML call receives that local (object shorthand), not a literal false
  assert.match(chatSrc, /keyRowHTML\(\{[\s\S]*?\bexpanded,[\s\S]*?\}\)/);
  assert.match(chatSrc, /let\s+permExpanded\s*=\s*\{\s*node:\s*""\s*,\s*open:\s*false\s*\}/);
});

/* ---------- P6: landscape height budget — wrap options; ask absorbs squeeze ---------- */

test("CSS: @media (max-height: 500px) wraps .permbtns; unmediated stays column pin", () => {
  // Base pin must survive — portrait ACP column stack is the phone-portrait shape.
  const base = chatCssSrc.match(/\.permbtns\s*\{([^}]+)\}/);
  assert.ok(base, ".permbtns rule present");
  assert.match(base[1], /flex-direction:\s*column/, "unmediated .permbtns stays column");
  assert.match(base[1], /flex:\s*0\s*0\s*auto/, "unmediated .permbtns stays flex: 0 0 auto");

  const mediaIdx = chatCssSrc.search(/@media\s*\(\s*max-height:\s*500px\s*\)/);
  assert.ok(mediaIdx >= 0, "@media (max-height: 500px) block exists");
  // Window after the query opener covers both .permbtns and .permbtn rules.
  const win = chatCssSrc.slice(mediaIdx, mediaIdx + 500);
  const mediaBtns = win.match(/\.permbtns\s*\{([^}]+)\}/);
  assert.ok(mediaBtns, "media block styles .permbtns");
  assert.match(mediaBtns[1], /flex-direction:\s*row/, "short viewports lay options in a row");
  assert.match(mediaBtns[1], /flex-wrap:\s*wrap/, "short viewports wrap options");
  const mediaBtn = win.match(/\.permbtns\s+\.permbtn\s*\{([^}]+)\}/);
  assert.ok(mediaBtn, "media block styles .permbtns .permbtn");
  assert.match(mediaBtn[1], /width:\s*auto/, "short-viewport .permbtn is width: auto");
});

test("CSS: #keyrow max-height; .permask flex absorbs the squeeze", () => {
  const keyrow = chatCssSrc.match(/#keyrow\s*\{([^}]+)\}/);
  assert.ok(keyrow, "#keyrow rule present");
  assert.match(keyrow[1], /max-height:/, "#keyrow declares max-height (the keyrow cap)");

  const mask = chatCssSrc.match(/\.permask\s*\{([^}]+)\}/);
  assert.ok(mask, ".permask rule present");
  assert.match(mask[1], /flex:\s*1\s*1\s*auto/, ".permask is flex: 1 1 auto under pressure");
  assert.match(mask[1], /min-height:\s*0/, ".permask min-height: 0 so it can shrink");
  // Cap stays — the ask scrolls internally; it does not grow unbound.
  assert.match(mask[1], /max-height:\s*30dvh/, ".permask keeps max-height: 30dvh");
});

test("keyRowHTML: ACP branch order is hint → .permask → .permmore → .permbtns", () => {
  // P6 is CSS-only — HTML order is the contract a later CSS-only phase must not
  // silently re-order around. Guard the full chain, not just pairwise siblings.
  const html = keyRowHTML({
    attention: "approval", source: "acp",
    permTitle: "Execute `cmd`",
    permToolKind: "execute",
    permOptions: [
      { key: "1", name: "Allow once", kind: "allow" },
      { key: "2", name: "Allow always", kind: "allow-always" },
      { key: "3", name: "Reject", kind: "reject" },
    ],
    expanded: false,
  });
  const hintAt = html.search(/class="hint"/);
  const maskAt = html.search(/class="permask/);
  const moreAt = html.search(/class="permmore"/);
  const btnsAt = html.search(/class="permbtns"/);
  assert.ok(hintAt >= 0, "emits hint");
  assert.ok(maskAt > hintAt, "hint before .permask");
  assert.ok(moreAt > maskAt, ".permask before .permmore");
  assert.ok(btnsAt > moreAt, ".permmore before .permbtns");
});

/* ---------- P7: reclaim optional chrome so the keyrow cap holds ---------- */

test("CSS: short viewports hide #keyrow .hint and tighten .permask; unmediated rules stay", () => {
  // Unmediated rules — portrait and desktop keep the hint and the 30dvh ask.
  assert.match(
    chatCssSrc,
    /#keyrow\s+\.hint\s*\{[^}]*font-size:/,
    "unmediated #keyrow .hint rule still exists",
  );
  const mask = chatCssSrc.match(/\.permask\s*\{([^}]+)\}/);
  assert.ok(mask, "unmediated .permask rule present");
  assert.match(mask[1], /max-height:\s*30dvh/, "unmediated .permask keeps max-height: 30dvh");

  const mediaIdx = chatCssSrc.search(/@media\s*\(\s*max-height:\s*500px\s*\)/);
  assert.ok(mediaIdx >= 0, "@media (max-height: 500px) block exists");
  const win = chatCssSrc.slice(mediaIdx, mediaIdx + 800);

  const mediaHint = win.match(/#keyrow\s+\.hint\s*\{([^}]+)\}/);
  assert.ok(mediaHint, "media block styles #keyrow .hint");
  assert.match(mediaHint[1], /display:\s*none/, "short viewports hide the approval hint");

  const mediaMask = win.match(/\.permask\s*\{([^}]+)\}/);
  assert.ok(mediaMask, "media block styles .permask");
  assert.match(mediaMask[1], /max-height:\s*20dvh/, "short viewports tighten .permask to 20dvh");
});

test("CSS: P6 pins survive P7 — .permbtns pin + short-viewport row/wrap", () => {
  const base = chatCssSrc.match(/\.permbtns\s*\{([^}]+)\}/);
  assert.ok(base, ".permbtns rule present");
  assert.match(base[1], /flex:\s*0\s*0\s*auto/, "unmediated .permbtns stays flex: 0 0 auto");

  const mediaIdx = chatCssSrc.search(/@media\s*\(\s*max-height:\s*500px\s*\)/);
  assert.ok(mediaIdx >= 0, "@media (max-height: 500px) block exists");
  const win = chatCssSrc.slice(mediaIdx, mediaIdx + 800);
  const mediaBtns = win.match(/\.permbtns\s*\{([^}]+)\}/);
  assert.ok(mediaBtns, "media block styles .permbtns");
  assert.match(mediaBtns[1], /flex-direction:\s*row/, "media block still sets row");
  assert.match(mediaBtns[1], /flex-wrap:\s*wrap/, "media block still sets wrap");
});

test("keyRowHTML: ACP still emits the hint unconditionally (CSS-only concealment)", () => {
  // Portrait and desktop keep the sentence; landscape only hides it via CSS.
  // Markup must not grow a max-height branch or drop the hint for "short".
  const html = keyRowHTML({
    attention: "approval", source: "acp",
    permTitle: "Execute `cmd`",
    permToolKind: "execute",
    permOptions: [
      { key: "1", name: "Allow once", kind: "allow" },
      { key: "2", name: "Allow always", kind: "allow-always" },
      { key: "3", name: "Reject", kind: "reject" },
      { key: "4", name: "Reject always", kind: "reject-always" },
      { key: "5", name: "Ask me", kind: "unknown" },
      { key: "6", name: "Skip", kind: "unknown" },
    ],
    expanded: false,
  });
  assert.match(html, /class="hint"/, "ACP with options still emits .hint in markup");
  assert.match(
    html,
    /The agent needs your approval\s*(?:&mdash;|\u2014|—)\s*choose one:/,
    "hint wording stays so portrait/desktop keep the sentence",
  );
  // Pure helper has no viewport signal — concealment is CSS-only.
  assert.doesNotMatch(chatSrc, /max-height.*hint|hint.*max-height|hideHint|shortViewport/,
    "keyRowHTML / chat.js must not branch on viewport height for the hint");
});

/* ---------- sentEcho ---------- */
test("retireSentEcho: text match, seen index, base growth, timeout", () => {
  const now = 1_000_000;
  const echo = { node: "n", text: "hello\nworld", at: now - 1000, seen: null };

  const match = retireSentEcho(echo, [
    { role: "assistant", text: "ok" },
    { role: "user", text: "hello world" },
  ], now);
  assert.equal(match.echo, null);
  assert.equal(match.retired, true);

  const bySeen = retireSentEcho(
    { ...echo, seen: 1, text: "reshaped beyond recognition" },
    [{ role: "user", text: "a" }, { role: "user", text: "b" }],
    now,
  );
  assert.equal(bySeen.retired, true);

  const base1 = retireSentEcho(
    { ...echo, seen: null, text: "reshaped" },
    [{ role: "user", text: "only" }],
    now,
  );
  assert.equal(base1.retired, false);
  assert.equal(base1.echo.base, 1);

  const base2 = retireSentEcho(
    { ...echo, seen: null, base: 1, text: "reshaped" },
    [{ role: "user", text: "a" }, { role: "user", text: "b" }],
    now,
  );
  assert.equal(base2.retired, true);

  const timeout = retireSentEcho(
    { ...echo, at: now - SENT_ECHO_TIMEOUT_MS - 1, text: "still waiting" },
    [],
    now,
  );
  assert.equal(timeout.retired, true);

  assert.equal(normalizeEchoText("a  \n b"), "a b");
});

test("echoBubbleHTML marks delivering user turn", () => {
  const html = echoBubbleHTML("hi there");
  assert.match(html, /turn user echo/);
  assert.match(html, /you — delivering/);
  assert.match(html, /hi there/);
});

/* ---------- attention suppression ---------- */
test("attentionIsSuppressed and suppress window", () => {
  const now = 5_000;
  assert.equal(attentionIsSuppressed("", { a: now + 100 }, now), false);
  assert.equal(attentionIsSuppressed("a", { a: now + 100 }, now), true);
  assert.equal(attentionIsSuppressed("a", { a: now - 1 }, now), false);
  assert.equal(suppressAttentionUntilTime(now), now + ATTENTION_SUPPRESS_MS);
});

/* ---------- work pulse ---------- */
test("workPulseShouldRun only when active and pane not forced", () => {
  assert.equal(workPulseShouldRun("active", false), true);
  assert.equal(workPulseShouldRun("active", true), false);
  assert.equal(workPulseShouldRun("active", false, true), false);
  assert.equal(workPulseShouldRun("quiet", false), false);
});

/* ---------- scroll near-bottom ---------- */
test("nearBottom uses 80px slack", () => {
  assert.equal(nearBottom(1000, 900, 100), true);  // 0 remaining
  assert.equal(nearBottom(1000, 800, 100), false); // 100 remaining
  assert.equal(nearBottom(1000, 921, 100), true);  // 21 remaining
  assert.equal(nearBottom(1000, 0, 100, 50), false);
});

/* ---------- pending jump matching ---------- */
test("matchPendingJumpInTurns prefers uid/segment/record then time then text", () => {
  const turns = [
    { uid: "u1", segment: 0, record: 1, time: "t1", text: "alpha" },
    { uid: "u2", segment: 1, record: 2, time: "t2", text: "beta" },
  ];
  assert.equal(matchPendingJumpInTurns({ uid: "u2", segment: 1, record: 2 }, turns), 1);
  assert.equal(matchPendingJumpInTurns({ turnTime: "t1" }, turns), 0);
  assert.equal(matchPendingJumpInTurns({ text: "beta" }, turns), 1);
  assert.equal(matchPendingJumpInTurns({ uid: "missing" }, turns), -1);
});

test("matchPendingJumpInHist mirrors live addressing for history bubbles", () => {
  const hists = [
    { uid: "u1", segment: "0", record: "3", time: "t1" },
    { uid: "u2", segment: "1", record: "4", time: "t2" },
  ];
  assert.equal(matchPendingJumpInHist({ uid: "u2", segment: 1, record: 4 }, hists).uid, "u2");
  assert.equal(matchPendingJumpInHist({ turnTime: "t1" }, hists).time, "t1");
  assert.equal(matchPendingJumpInHist({ uid: "nope" }, hists), null);
});

/* ---------- fake DOM factory lifecycle ---------- */

function el(tag, attrs = {}){
  const listeners = new Map();
  const node = {
    tagName: tag.toUpperCase(),
    className: attrs.className || "",
    classList: {
      _s: new Set((attrs.className || "").split(/\s+/).filter(Boolean)),
      toggle(c, on){
        if (on === undefined) {
          if (this._s.has(c)) this._s.delete(c); else this._s.add(c);
        } else if (on) this._s.add(c); else this._s.delete(c);
        node.className = [...this._s].join(" ");
      },
      contains(c){ return this._s.has(c); },
      add(c){ this._s.add(c); node.className = [...this._s].join(" "); },
      remove(c){ this._s.delete(c); node.className = [...this._s].join(" "); },
    },
    style: {},
    dataset: { ...(attrs.dataset || {}) },
    hidden: !!attrs.hidden,
    disabled: !!attrs.disabled,
    textContent: attrs.textContent || "",
    _html: attrs.innerHTML || "",
    value: attrs.value || "",
    scrollTop: 0,
    scrollHeight: attrs.scrollHeight || 0,
    clientHeight: attrs.clientHeight || 0,
    offsetHeight: attrs.offsetHeight || 0,
    offsetWidth: attrs.offsetWidth || 0,
    children: [],
    parentNode: null,
    setAttribute(k, v){ this._attrs = this._attrs || {}; this._attrs[k] = v; },
    getAttribute(k){ return (this._attrs || {})[k]; },
    addEventListener(type, fn, opts){
      if (!listeners.has(type)) listeners.set(type, []);
      listeners.get(type).push({ fn, opts });
    },
    removeEventListener(type, fn){
      const arr = listeners.get(type) || [];
      listeners.set(type, arr.filter(x => x.fn !== fn));
    },
    querySelector(sel){
      if (sel.startsWith("#")) {
        const id = sel.slice(1);
        if (this.id === id) return this;
        for (const c of this.children) {
          const f = c.querySelector?.(sel);
          if (f) return f;
          if (c.id === id) return c;
        }
        return null;
      }
      if (sel.startsWith(".")) {
        const cls = sel.slice(1).split(/[\s.>]/)[0];
        if (this.classList.contains(cls)) return this;
        for (const c of this.children) {
          const f = c.querySelector?.(sel);
          if (f) return f;
        }
        return null;
      }
      // attribute selectors like .turn[data-bk="i:0"]
      const m = sel.match(/\[data-bk="([^"]+)"\]/);
      if (m) {
        const walk = n => {
          if (n.dataset && n.dataset.bk === m[1]) return n;
          for (const c of n.children || []) {
            const f = walk(c);
            if (f) return f;
          }
          return null;
        };
        return walk(this);
      }
      return null;
    },
    querySelectorAll(sel){
      const out = [];
      const walk = n => {
        if (sel.includes(".bubactions") && n.classList?.contains("bubactions")) out.push(n);
        if (sel.includes(".peekblock") && n.classList?.contains("peekblock")) out.push(n);
        if (sel.includes(".pending") && n.classList?.contains("pending")) out.push(n);
        if (sel.includes(".turn.echo") && n.classList?.contains("echo")) out.push(n);
        if (sel.includes(".histseam") && n.classList?.contains("histseam")) out.push(n);
        if (sel.includes(".turn.hist") && n.classList?.contains("hist")) out.push(n);
        if (sel.includes(".attthumb") && n.classList?.contains("attthumb")) out.push(n);
        for (const c of n.children || []) walk(c);
      };
      walk(this);
      return out;
    },
    closest(sel){
      if (sel.includes("button") && this.tagName === "BUTTON") return this;
      if (sel.includes(".histload") && this.classList.contains("histload")) return this;
      if (sel.includes(".permmore") && this.classList.contains("permmore")) return this;
      if (sel.includes(".permask") && this.classList.contains("permask")) return this;
      if (sel.includes("[data-bact]") && this.dataset?.bact) return this;
      if (sel.includes("[data-dismiss-attention]") && this.dataset?.dismissAttention) return this;
      if (sel.includes("[data-key]") && this.dataset?.key) return this;
      if (sel.includes(".turn") && this.classList.contains("turn")) return this;
      if (sel.includes(".attthumb") && this.classList.contains("attthumb")) return this;
      if (sel.includes("a, button") && (this.tagName === "A" || this.tagName === "BUTTON")) return this;
      return this.parentNode ? this.parentNode.closest(sel) : null;
    },
    after(sib){
      if (!this.parentNode) return;
      const i = this.parentNode.children.indexOf(this);
      this.parentNode.children.splice(i + 1, 0, sib);
      sib.parentNode = this.parentNode;
    },
    insertBefore(node, ref){
      if (!ref) { this.children.push(node); }
      else {
        const i = this.children.indexOf(ref);
        this.children.splice(i < 0 ? this.children.length : i, 0, node);
      }
      node.parentNode = this;
      return node;
    },
    appendChild(node){ this.children.push(node); node.parentNode = this; return node; },
    remove(){
      if (!this.parentNode) return;
      const i = this.parentNode.children.indexOf(this);
      if (i >= 0) this.parentNode.children.splice(i, 1);
      this.parentNode = null;
    },
    focus(){},
    select(){},
    setSelectionRange(){},
    scrollIntoView(){},
    scrollTo(opts){ if (opts && opts.top != null) this.scrollTop = opts.top; },
    _listeners: listeners,
    listenerCount(type){ return (listeners.get(type) || []).length; },
  };
  if (attrs.id) node.id = attrs.id;
  /* A real innerHTML write replaces the element's whole subtree. The fake keeps
     children in an array that a plain data property would leave untouched, so
     stale nodes (e.g. a previous #peekhost) would survive a rebuild here but
     not in a browser. */
  Object.defineProperty(node, "innerHTML", {
    configurable: true,
    get(){ return node._html; },
    set(v){ node._html = v; node.children.length = 0; },
  });
  return node;
}

/* keyrow harness: assign innerHTML → materialise .permask / .permmore children
   so scrollTop snapshot/restore, the P2 measure pass, and querySelector work
   the same way as a real DOM. _permMaskMetrics is overridable per test so the
   suite can drive overflow vs no-overflow without a layout engine. */
function makeKeyrow(){
  const node = el("div", { id: "keyrow" });
  node._permMaskMetrics = { scrollHeight: 1000, clientHeight: 200 };
  let html = "";
  Object.defineProperty(node, "innerHTML", {
    configurable: true,
    get(){ return html; },
    set(v){
      html = String(v);
      node.children = [];
      const m = html.match(/class="(permask(?:\s+clamped)?)"/);
      if (m){
        const mask = el("div", { className: m[1] });
        const met = node._permMaskMetrics || {};
        mask.scrollHeight = met.scrollHeight ?? 1000;
        mask.clientHeight = met.clientHeight ?? 200;
        mask.scrollTop = 0;
        node.appendChild(mask);
      }
      const moreTag = html.match(/<button\b[^>]*class="permmore"[^>]*>/);
      if (moreTag){
        const more = el("button", { className: "permmore" });
        more.hidden = /\bhidden\b/.test(moreTag[0]);
        node.appendChild(more);
      }
    },
  });
  // seed empty so later assignments always go through the setter
  node.innerHTML = "";
  return node;
}

function makeRoots(){
  const msgs = el("div", { id: "msgs", scrollHeight: 500, clientHeight: 400 });
  // make near-bottom true by default
  msgs.scrollTop = 100;
  return {
    chathead: el("div", { id: "chathead" }),
    chattitle: el("h2", { id: "chattitle" }),
    chatmission: el("div", { id: "chatmission" }),
    chatdetails: el("div", { id: "chatdetails", hidden: true }),
    chatdesc: el("div", { id: "chatdesc" }),
    chatdescinput: el("textarea", { id: "chatdescinput" }),
    chatdot: el("span", { id: "chatdot" }),
    chatmeta: el("span", { id: "chatmeta" }),
    chatagentlogo: el("span", { id: "chatagentlogo" }),
    infobtn: el("button", { id: "infobtn" }),
    msgs,
    msgwrap: el("div", { id: "msgwrap" }),
    chatloading: el("div", { id: "chatloading", hidden: true }),
    gauge: el("div", { id: "gauge", hidden: true }),
    gaugefill: el("i", { id: "gaugefill" }),
    keyrow: makeKeyrow(),
    convtools: el("div", { id: "convtools", hidden: true }),
    termtoggle: el("button", { id: "termtoggle" }),
    scrollend: el("button", { id: "scrollend" }),
    workpulse: el("div", { id: "workpulse" }),
  };
}

function acpApprovalPayload(overrides = {}){
  return {
    turns: [{ role: "user", text: "run it" }],
    live: "quiet",
    delivery: "ok",
    source: "acp",
    attention: "approval",
    perm_title: "Execute `#!/bin/bash\n" + "x".repeat(200) + "\necho done`",
    perm_options: [
      { key: "1", name: "Allow once", kind: "allow" },
      { key: "2", name: "Reject once", kind: "reject" },
    ],
    perm_tool_kind: "execute",
    chat_started: "2026-01-01T00:00:00Z",
    prior_turns: 0,
    assets: {},
    ...overrides,
  };
}

function acpApprovalNodes(id = "n1"){
  return [{
    id, title: "Grok", agent: "grok", model: "grok",
    live: "quiet", attention: "approval", lane_id: "", description: "",
  }];
}

function firstListener(node, type){
  return (node._listeners.get(type) || [])[0]?.fn;
}

function defaultChatPayload(overrides = {}){
  return overrides.chatPayload || {
    turns: [{ role: "user", text: "hello", time: "2026-01-01T00:00:00Z" },
            { role: "assistant", text: "hi", time: "2026-01-01T00:01:00Z" }],
    live: "quiet",
    delivery: "ok",
    source: "tmux",
    chat_started: "2026-01-01T00:00:00Z",
    prior_turns: 0,
    assets: {},
  };
}

function makeFeature(overrides = {}){
  const roots = makeRoots();
  const nodes = overrides.nodes || [{
    id: "n1", title: "Alpha", agent: "claude", model: "sonnet",
    live: "quiet", attention: "", lane_id: "L1", description: "d",
  }];
  let sel = overrides.sel ?? "n1";
  let selGen = overrides.selGen ?? 1;
  let pendingJump = null;
  const apiCalls = [];
  const resolveApi = async (path, opts) => {
    if (typeof overrides.api === "function") return overrides.api(path, opts, apiCalls);
    if (path.includes("/chat?history=1")) {
      return {
        segments: overrides.histSegs || [],
        assets: {},
      };
    }
    if (path.includes("/peek")) return overrides.peekText || "pane text";
    if (path.includes("/chat")) return defaultChatPayload(overrides);
    return {};
  };
  const api = async (path, opts) => {
    apiCalls.push({ path, opts, kind: "api" });
    return resolveApi(path, opts);
  };
  /* Conditional chat poll: exposes status + ETag like production apiConditionalGet. */
  const apiConditionalGet = overrides.apiConditionalGet || (async (path, etag) => {
    const opts = { headers: etag ? { "If-None-Match": etag } : {} };
    apiCalls.push({ path, opts, etag: etag || "", kind: "conditional" });
    if (typeof overrides.chatConditional === "function")
      return overrides.chatConditional(path, etag, apiCalls);
    if (path.includes("/chat") && !path.includes("history")) {
      const data = await resolveApi(path, opts);
      return { status: 200, etag: overrides.stableChatETag || '"etag1"', data };
    }
    const data = await resolveApi(path, opts);
    return { status: 200, etag: "", data };
  });
  const effects = {
    setComposerBusy: [],
    setComposerClosed: [],
    setAttachAvail: [],
    bookmarksBack: [],
  };
  const feature = createChatFeature({
    roots,
    document: {
      createElement: tag => el(tag),
      querySelector: () => null,
    },
    window: {},
    CSS: { escape: s => String(s).replace(/"/g, '\\"') },
    api,
    apiConditionalGet,
    nodes: () => nodes,
    sel: () => sel,
    selGen: () => selGen,
    nodeById: id => nodes.find(n => n.id === id),
    hardAttention: () => false,
    laneColor: () => "#007AFF",
    agentLogo: () => "LOGO",
    icons: {
      ICON_TERM: "T", ICON_CHECK: "✓", ICON_CHEV_UP: "↑", ICON_CHEV_DOWN: "↓",
      ICON_BRANCH: "B", ICON_INTO: "I", ICON_COPY: "C", ICON_DOWNALL: "↓↓",
      ICON_FILE: "F",
    },
    setComposerBusy: v => effects.setComposerBusy.push(v),
    setComposerClosed: v => effects.setComposerClosed.push(v),
    setAttachAvail: v => effects.setAttachAvail.push(v),
    setBookmarksBackLabel: t => effects.bookmarksBack.push(t),
    getPendingJump: () => pendingJump,
    setPendingJump: v => { pendingJump = v; },
    invalidateCardsSig: () => {},
    invalidateMapSig: () => {},
    renderCards: () => {},
    renderMap: () => {},
    updateLocalNode: () => {},
    copyText: () => {},
    uiMutate: () => {},
    storage: overrides.storage,
    composerText: overrides.composerText,
    restoreDraft: overrides.restoreDraft,
    stampAddress: () => {},
    forkFromTurn: () => {},
    alert: () => {},
    toast: () => {},
    tick: () => {},
    scheduleTick: () => {},
    isDesktop: () => false,
    now: () => 1_000_000,
    longpress: () => () => {},
    ...overrides.deps,
  });
  return { feature, roots, nodes, apiCalls, effects, setSel: v => { sel = v; }, setSelGen: v => { selGen = v; }, getPending: () => pendingJump, setPending: v => { pendingJump = v; } };
}

test("factory bind is idempotent; destroy removes listeners", () => {
  let longpressCleanups = 0;
  const { feature, roots } = makeFeature({
    deps: { longpress: () => () => { longpressCleanups++; } },
  });
  feature.bind();
  feature.bind(); // second bind no-ops
  assert.equal(roots.infobtn.listenerCount("click"), 1);
  assert.equal(roots.chathead.listenerCount("click"), 1);
  assert.equal(roots.msgs.listenerCount("click"), 1);
  assert.equal(roots.msgs.listenerCount("scroll"), 1);
  assert.equal(roots.msgs.listenerCount("load"), 1);
  assert.equal(roots.msgs.listenerCount("error"), 1);
  assert.equal(roots.msgs.listenerCount("touchend"), 1);
  assert.equal(roots.termtoggle.listenerCount("click"), 1);
  assert.equal(roots.keyrow.listenerCount("click"), 1);
  assert.equal(roots.scrollend.listenerCount("click"), 1);
  feature.destroy();
  assert.equal(roots.infobtn.listenerCount("click"), 0);
  assert.equal(roots.chathead.listenerCount("click"), 0);
  assert.equal(roots.msgs.listenerCount("click"), 0);
  assert.equal(roots.msgs.listenerCount("scroll"), 0);
  assert.equal(roots.msgs.listenerCount("load"), 0);
  assert.equal(roots.msgs.listenerCount("error"), 0);
  assert.equal(roots.msgs.listenerCount("touchend"), 0);
  assert.equal(roots.keyrow.listenerCount("click"), 0);
  assert.equal(roots.termtoggle.listenerCount("click"), 0);
  assert.equal(roots.scrollend.listenerCount("click"), 0);
  assert.equal(longpressCleanups, 2);
  // rebind after destroy
  feature.bind();
  assert.equal(roots.infobtn.listenerCount("click"), 1);
  feature.destroy();
});

test("broken-thumbnail cleanup is one delegated error listener; rebuilds do not accumulate", async () => {
  const { feature, roots } = makeFeature({
    chatPayload: {
      turns: [{
        role: "user",
        text: "see ![pic](scimux-asset:a1)",
        time: "2026-01-01T00:00:00Z",
      }],
      live: "quiet",
      delivery: "ok",
      source: "tmux",
      chat_started: "2026-01-01T00:00:00Z",
      prior_turns: 0,
      assets: { a1: { name: "a.png", mime: "image/png" } },
    },
  });
  feature.bind();
  assert.equal(roots.msgs.listenerCount("error"), 1, "one delegated error listener");
  await feature.render();
  assert.match(roots.msgs.innerHTML, /attthumb/);
  assert.doesNotMatch(roots.msgs.innerHTML, /onerror=/);
  await feature.render(); // same sig skip
  feature.invalidate();
  await feature.render(); // rebuild
  assert.equal(roots.msgs.listenerCount("error"), 1, "rebuild must not add another error listener");
  feature.bind();
  assert.equal(roots.msgs.listenerCount("error"), 1);
  feature.destroy();
  assert.equal(roots.msgs.listenerCount("error"), 0);
});

test("refreshChat builds bubbles, signature-skips rebuild, preserves empty/loading", async () => {
  const { feature, roots, apiCalls, effects } = makeFeature();
  feature.bind();
  await feature.render();
  assert.ok(apiCalls.some(c => c.path.includes("/chat") && !c.path.includes("history")));
  assert.match(roots.msgs.innerHTML, /data-bk="i:0"/);
  assert.match(roots.msgs.innerHTML, /data-bk="i:1"/);
  assert.match(roots.msgs.innerHTML, /title="you — sent/);
  assert.match(roots.msgs.innerHTML, /hello/);
  assert.equal(effects.setAttachAvail.at(-1), true);
  assert.equal(effects.setComposerBusy.at(-1), false);
  assert.ok(effects.bookmarksBack.includes("Alpha"));

  const html1 = roots.msgs.innerHTML;
  const nCalls = apiCalls.length;
  await feature.render(); // same sig → skip DOM rebuild (still fetches)
  assert.ok(apiCalls.length > nCalls);
  assert.equal(roots.msgs.innerHTML, html1);

  // force rebuild via term toggle path
  firstListener(roots.termtoggle, "click")();
  await new Promise(resolve => setImmediate(resolve));
  /* the pane renders into its own host inside #msgs, never into the bubbles */
  const host = roots.msgs.children.find(c => c.id === "peekhost");
  assert.ok(host, "the pane host must exist after a rebuild");
  assert.match(host.innerHTML, /peekblock|Terminal/);
  feature.destroy();
});

/* ---------- P2: conditional chat poll (If-None-Match / 304) ---------- */

test("P2 refreshChat omits If-None-Match on first poll, then sends validator", async () => {
  const { feature, apiCalls } = makeFeature({ stableChatETag: '"v1"' });
  feature.bind();
  await feature.render();
  await feature.render();
  const polls = apiCalls.filter(c =>
    c.path.includes("/chat") && !c.path.includes("history") && c.kind === "conditional");
  assert.ok(polls.length >= 2, `want ≥2 conditional chat polls, got ${polls.length}`);
  const h0 = polls[0].opts?.headers || {};
  const h1 = polls[1].opts?.headers || {};
  assert.equal(h0["If-None-Match"] || "", "",
    "first request for a node must not send If-None-Match");
  assert.equal(h1["If-None-Match"], '"v1"',
    "second request must send the stored ETag");
  feature.destroy();
});

test("P2 refreshChat 304 performs no re-render", async () => {
  let n = 0;
  const payload = defaultChatPayload();
  const { feature, roots, apiCalls } = makeFeature({
    chatConditional: () => {
      n += 1;
      if (n === 1) return { status: 200, etag: '"e1"', data: payload };
      return { status: 304, etag: '"e1"', data: null };
    },
  });
  feature.bind();
  await feature.render();
  assert.match(roots.msgs.innerHTML, /hello/);
  // Marker proves the 304 path did not rebuild #msgs (signature skip alone
  // would still run after a 200 decode; 304 must not touch the render seam).
  roots.msgs.innerHTML = roots.msgs.innerHTML + "<!--no-rerender-->";
  const htmlMarked = roots.msgs.innerHTML;
  await feature.render();
  assert.equal(roots.msgs.innerHTML, htmlMarked,
    "304 must not re-render turns or rewrite #msgs");
  const polls = apiCalls.filter(c => c.kind === "conditional");
  assert.ok(polls.length >= 2);
  assert.equal(polls[1].opts?.headers?.["If-None-Match"], '"e1"');
  feature.destroy();
});

test("P2 refreshChat 304 leaves no stuck load or busy state", async () => {
  let n = 0;
  const timers = [];
  const payload = defaultChatPayload({
    chatPayload: {
      ...defaultChatPayload(),
      live: "quiet",
    },
  });
  const { feature, roots, effects } = makeFeature({
    chatConditional: () => {
      n += 1;
      if (n === 1) return { status: 200, etag: '"e1"', data: payload };
      return { status: 304, etag: '"e1"', data: null };
    },
    deps: {
      setTimeout: (fn) => { timers.push(fn); return timers.length; },
      clearTimeout: () => {},
    },
  });
  feature.bind();
  // Cold first load exercises beginChatLoad; endChatLoad must clear it.
  feature.onSelectChange();
  const p1 = feature.render();
  for (const fn of timers.splice(0)) fn();
  assert.equal(roots.msgwrap.classList.contains("loading"), true);
  await p1;
  assert.equal(roots.msgwrap.classList.contains("loading"), false);
  assert.equal(roots.chatloading.hidden, true);

  // Second poll is 304 — cold-load via invalidate (keeps per-node ETag).
  // beginChatLoad must still be balanced by endChatLoad on the 304 exit.
  feature.invalidate();
  const p2 = feature.render();
  for (const fn of timers.splice(0)) fn();
  assert.equal(roots.msgwrap.classList.contains("loading"), true,
    "cold 304 path still shows loading while request is in flight");
  await p2;
  assert.equal(roots.msgwrap.classList.contains("loading"), false,
    "304 must call endChatLoad so .loading does not stick");
  assert.equal(roots.chatloading.hidden, true);
  // Busy is not forced true on quiet payload; ensure last state is not stuck true.
  assert.notEqual(effects.setComposerBusy.at(-1), true,
    "304 must not leave composer stuck busy");
  // Prove the second request was actually a 304, not a silent 200 rebuild.
  assert.equal(n, 2, "chatConditional must have returned 304 on the second poll");
  feature.destroy();
});

test("P2 refreshChat drops previous node's validator on switch", async () => {
  const nodes = [
    { id: "A", title: "Alpha", agent: "claude", model: "sonnet",
      live: "quiet", attention: "", lane_id: "L1", description: "d" },
    { id: "B", title: "Beta", agent: "claude", model: "sonnet",
      live: "quiet", attention: "", lane_id: "L1", description: "d" },
  ];
  const tags = { A: '"etag-A"', B: '"etag-B"' };
  const { feature, apiCalls, setSel, setSelGen } = makeFeature({
    nodes,
    sel: "A",
    chatConditional: (path, etag) => {
      const id = path.includes("/A/") || path.endsWith("/A/chat") || path.includes("nodes/A/")
        ? "A"
        : path.includes("nodes/B/") ? "B" : (path.match(/nodes\/([^/]+)\/chat/) || [])[1];
      const nodeId = id || "A";
      return {
        status: 200,
        etag: tags[nodeId] || '"x"',
        data: {
          ...defaultChatPayload(),
          turns: [{ role: "user", text: "from-" + nodeId, time: "2026-01-01T00:00:00Z" }],
        },
      };
    },
  });
  feature.bind();
  await feature.render(); // A, store etag-A
  feature.onSelectChange();
  setSel("B");
  setSelGen(2);
  await feature.render(); // B must not send etag-A
  const polls = apiCalls.filter(c =>
    c.kind === "conditional" && c.path.includes("/chat") && !c.path.includes("history"));
  assert.ok(polls.length >= 2);
  const bPoll = polls.find(c => c.path.includes("nodes/B/"));
  assert.ok(bPoll, "expected a poll for node B");
  assert.equal(bPoll.opts?.headers?.["If-None-Match"] || "", "",
    "node switch must not send the previous node's validator");
  feature.destroy();
});

test("selection change clears an open terminal and loaded history", async () => {
  const segs = [{
    start: "2025-12-31T00:00:00Z", seam: "2025-12-31T00:00:00Z",
    turns: [{ role: "user", text: "old" }],
  }];
  const ctx = makeFeature({
    histSegs: segs,
    chatPayload: {
      turns: [{ role: "user", text: "now" }], live: "quiet", delivery: "ok",
      source: "tmux", chat_started: "2026-01-01T00:00:00Z", prior_turns: 1, assets: {},
    },
  });
  ctx.feature.bind();
  await ctx.feature.loadHistory("n1", "");
  assert.match(ctx.roots.msgs.innerHTML, /turn hist/);

  firstListener(ctx.roots.termtoggle, "click")();
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(ctx.roots.termtoggle.classList.contains("on"), true);

  ctx.feature.onSelectChange();
  await ctx.feature.render();
  assert.equal(ctx.roots.termtoggle.classList.contains("on"), false);
  assert.match(ctx.roots.msgs.innerHTML, /histload/);
  assert.doesNotMatch(ctx.roots.msgs.innerHTML, /turn hist/);
  ctx.feature.destroy();
});

test("sentEcho paint and retirement on refresh", async () => {
  let payload = {
    turns: [], live: "active", delivery: "ok", source: "tmux",
    chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
  };
  const ctx = makeFeature({
    api: async (path) => {
      if (path.includes("/peek")) return "p";
      if (path.includes("/chat")) return payload;
      return {};
    },
  });
  ctx.feature.setSentEcho({ node: "n1", text: "hello", at: 999_000, seen: null });
  ctx.feature.paintEcho("n1");
  assert.match(ctx.roots.msgs.innerHTML || ctx.roots.msgs.children.map?.(c => c.innerHTML).join("") || "", /echo|delivering/);

  // next poll lands the user turn
  payload = {
    turns: [{ role: "user", text: "hello" }],
    live: "quiet", delivery: "ok", source: "tmux",
    chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
  };
  ctx.feature.invalidate();
  await ctx.feature.render();
  assert.doesNotMatch(ctx.roots.msgs.innerHTML, /turn user echo/);
});

test("attention suppression collapses keyrow and hides attention chrome", async () => {
  const nodes = [{
    id: "n1", title: "A", agent: "claude", model: "s", live: "quiet",
    attention: "question", lane_id: "", description: "",
  }];
  const { feature, roots } = makeFeature({
    nodes,
    chatPayload: {
      turns: [{ role: "user", text: "q" }],
      live: "quiet", delivery: "ok", source: "tmux",
      chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
      attention: "question",
    },
  });
  feature.bind();
  await feature.render();
  assert.match(roots.keyrow.innerHTML, /data-key=/);
  const key = el("button", { dataset: { key: "Enter" } });
  firstListener(roots.keyrow, "click")({ target: key });
  assert.equal(roots.keyrow.innerHTML, "");
  feature.invalidate();
  await feature.render();
  // suppressed → keyrow empty and pane not forced solely by attention
  assert.equal(roots.keyrow.innerHTML, "");
  feature.destroy();
});

test("AX inspect renders Dismiss only so a stray 1 cannot submit", async () => {
  const nodes = [{
    id: "n1", title: "A", agent: "claude", model: "s", live: "quiet",
    attention: "inspect", attention_at: 100, lane_id: "", description: "",
    ax_screen_reader: true,
  }];
  const ctx = makeFeature({
    nodes,
    chatPayload: {
      turns: [{ role: "user", text: "q" }],
      live: "quiet", delivery: "ok", source: "tmux",
      chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
      attention: "inspect",
    },
  });
  ctx.feature.bind();
  await ctx.feature.render();
  assert.match(ctx.roots.keyrow.innerHTML, />Dismiss</);
  assert.doesNotMatch(ctx.roots.keyrow.innerHTML, /data-key=/,
    "AX inspect must not emit the keypad that tmuxKeySequence would confirm with Enter");
  ctx.feature.destroy();
});

test("inspect Esc is local and stays dismissed until fresh attention evidence", async () => {
  const nodes = [{
    id: "n1", title: "A", agent: "claude", model: "s", live: "quiet",
    attention: "inspect", attention_at: 100, lane_id: "", description: "",
  }];
  const payload = {
    turns: [{ role: "user", text: "q" }],
    live: "quiet", delivery: "ok", source: "tmux",
    chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
    attention: "inspect",
  };
  const ctx = makeFeature({
    nodes,
    chatPayload: payload,
  });
  ctx.feature.bind();
  await ctx.feature.render();
  assert.match(ctx.roots.keyrow.innerHTML, /data-dismiss-attention="inspect"/);
  assert.match(ctx.roots.keyrow.innerHTML, />Dismiss</);
  assert.match(ctx.roots.keyrow.innerHTML, /data-key="Enter"/,
    "inspect still offers the rest of the terminal action bar");
  assert.match(ctx.roots.keyrow.innerHTML, /data-key="Escape"/,
    "inspect Escape remains a remote key; Dismiss is the local ack");

  const beforeKeys = ctx.apiCalls.filter(c => c.path.includes("/key")).length;
  const dismiss = el("button", { dataset: { dismissAttention: "inspect" } });
  firstListener(ctx.roots.keyrow, "click")({ target: dismiss });
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(ctx.apiCalls.filter(c => c.path.includes("/key")).length, beforeKeys,
    "the acknowledgement must never send Escape to Claude");
  assert.equal(ctx.roots.keyrow.innerHTML, "");

  ctx.feature.invalidate();
  await ctx.feature.render();
  assert.equal(ctx.roots.keyrow.innerHTML, "",
    "the same evidence remains dismissed without a short timer");

  nodes[0].attention_at = 101;
  ctx.feature.invalidate();
  await ctx.feature.render();
  assert.match(ctx.roots.keyrow.innerHTML, /data-dismiss-attention="inspect"/,
    "fresh evidence makes inspection visible again");

  nodes[0].attention = "approval";
  payload.attention = "approval";
  ctx.feature.invalidate();
  await ctx.feature.render();
  assert.match(ctx.roots.keyrow.innerHTML, /data-key="Escape"/,
    "hard attention is never hidden by the neutral acknowledgement");
  ctx.feature.destroy();
});

test("inspect remote keys still send /key; Dismiss does not", async () => {
  const nodes = [{
    id: "n1", title: "A", agent: "claude", model: "s", live: "quiet",
    attention: "inspect", attention_at: 100, lane_id: "", description: "",
  }];
  const ctx = makeFeature({
    nodes,
    chatPayload: {
      turns: [{ role: "user", text: "q" }],
      live: "quiet", delivery: "ok", source: "tmux",
      chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
      attention: "inspect",
    },
  });
  ctx.feature.bind();
  await ctx.feature.render();

  const before = ctx.apiCalls.filter(c => c.path.includes("/key")).length;
  const enter = el("button", { dataset: { key: "Enter" } });
  firstListener(ctx.roots.keyrow, "click")({ target: enter });
  await new Promise(resolve => setImmediate(resolve));
  const keyCalls = ctx.apiCalls.filter(c => c.path.includes("/key"));
  assert.equal(keyCalls.length, before + 1, "inspect Enter is a remote terminal key");
  assert.equal(JSON.parse(keyCalls[keyCalls.length - 1].opts?.body || "{}").key, "Enter");

  const ctx2 = makeFeature({
    nodes,
    chatPayload: {
      turns: [{ role: "user", text: "q" }],
      live: "quiet", delivery: "ok", source: "tmux",
      chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
      attention: "inspect",
    },
  });
  ctx2.feature.bind();
  await ctx2.feature.render();
  const beforeEsc = ctx2.apiCalls.filter(c => c.path.includes("/key")).length;
  const esc = el("button", { dataset: { key: "Escape" } });
  firstListener(ctx2.roots.keyrow, "click")({ target: esc });
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(ctx2.apiCalls.filter(c => c.path.includes("/key")).length, beforeEsc + 1,
    "inspect Escape is a remote terminal key so a dialog can still be backed out of");
  ctx2.feature.destroy();
  ctx.feature.destroy();
});

/* Screen-reader AX: the browser stays semantic and single-shot. Server-side
 * translation may expand "1" into physical ["1","Enter"]; JS must not POST
 * Enter, wait, or inspect ax_screen_reader. */
test("tmux numbered key click is one semantic POST without delayed Enter", async () => {
  const nodes = [{
    id: "n1", title: "A", agent: "claude", model: "s", live: "quiet",
    attention: "question", lane_id: "", description: "",
  }];
  const scheduled = [];
  const ctx = makeFeature({
    nodes,
    chatPayload: {
      turns: [{ role: "user", text: "q" }],
      live: "quiet", delivery: "ok", source: "tmux",
      chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
      attention: "question",
    },
    deps: {
      // Capture delayed tick scheduling; must not schedule a second /key.
      scheduleTick: (ms) => { scheduled.push({ kind: "scheduleTick", ms }); },
      tick: () => { scheduled.push({ kind: "tick" }); },
    },
  });
  ctx.feature.bind();
  await ctx.feature.render();
  assert.match(ctx.roots.keyrow.innerHTML, /data-key="1"/);

  const before = ctx.apiCalls.filter(c => c.path.includes("/key")).length;
  const btn = el("button", { dataset: { key: "1" } });
  firstListener(ctx.roots.keyrow, "click")({ target: btn });
  await new Promise(resolve => setImmediate(resolve));

  const keyCalls = ctx.apiCalls.filter(c => c.path.includes("/key"));
  assert.equal(keyCalls.length, before + 1, "exactly one /key request");
  const call = keyCalls[keyCalls.length - 1];
  assert.match(call.path, /\/api\/nodes\/n1\/key$/);
  assert.equal(call.opts?.method, "POST");
  assert.equal(call.opts?.body, JSON.stringify({ key: "1" }),
    "body is the semantic choice only");
  // No second request for Enter, and no body that embeds Enter.
  assert.equal(
    keyCalls.filter(c => {
      try {
        const b = JSON.parse(c.opts?.body || "{}");
        return b.key === "Enter";
      } catch { return false; }
    }).length,
    0,
    "must not POST Enter as a separate confirmation",
  );
  // scheduleTick(400) for chat refresh is fine; it must not POST /key again.
  assert.ok(
    scheduled.every(s => s.kind !== "key"),
    "no delayed key confirmation scheduled",
  );
  const afterFlush = ctx.apiCalls.filter(c => c.path.includes("/key")).length;
  assert.equal(afterFlush, before + 1, "still exactly one /key after async settle");
  ctx.feature.destroy();
});

test("work-pulse start/stop via activity chrome", async () => {
  const { feature, roots } = makeFeature({
    chatPayload: {
      turns: [{ role: "user", text: "x" }],
      live: "active", delivery: "ok", source: "tmux",
      chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
    },
  });
  await feature.render();
  assert.equal(roots.workpulse.classList.contains("on"), true);

  // attention forces pane → pulse off (chat payload must carry attention;
  // refreshChat projects d.attention onto the node)
  const n2 = makeFeature({
    nodes: [{
      id: "n1", title: "A", agent: "c", model: "s", live: "active",
      attention: "approval", lane_id: "", description: "",
    }],
    chatPayload: {
      turns: [{ role: "user", text: "x" }],
      live: "active", attention: "approval", delivery: "ok", source: "tmux",
      chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
    },
  });
  await n2.feature.render();
  assert.equal(n2.roots.workpulse.classList.contains("on"), false);

  // Claude's explicit end_turn releases the main-chat scanner and composer
  // even while the mechanically debounced pane still reports active.
  const ready = makeFeature({
    chatPayload: {
      turns: [{ role: "assistant", text: "done" }],
      live: "active", reply_ready: true, delivery: "ok", source: "tmux",
      chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
    },
  });
  await ready.feature.render();
  assert.equal(ready.roots.workpulse.classList.contains("on"), false);
  assert.equal(ready.effects.setComposerBusy.at(-1), false);

  const cleared = makeFeature({
    chatPayload: {
      turns: [], live: "active", fresh: true, fallback: true,
      delivery: "ok", source: "none", chat_started: "2026-01-02T00:00:00Z",
      prior_turns: 2, assets: {},
    },
  });
  await cleared.feature.render();
  assert.equal(cleared.roots.workpulse.classList.contains("on"), false);
  assert.equal(cleared.effects.setComposerBusy.at(-1), false);
});

test("history expansion renders prior segs with data-bk hist keys", async () => {
  const segs = [{
    start: "2026-01-01T00:00:00Z",
    seam: "2026-01-01T00:00:00Z",
    reason: "clear",
    turns: [{ role: "user", text: "old", time: "2026-01-01T00:00:01Z", uid: "u0", segment: 0, record: 1 }],
  }];
  const { feature, roots } = makeFeature({
    chatPayload: {
      turns: [{ role: "user", text: "new", time: "2026-01-02T00:00:00Z" }],
      live: "quiet", delivery: "ok", source: "tmux",
      chat_started: "2026-01-02T00:00:00Z", prior_turns: 1, assets: {},
    },
  });
  await feature.render();
  assert.match(roots.msgs.innerHTML, /histload/);
  // simulate history load into feature state via loadHistory API
  const ctx = makeFeature({
    histSegs: segs,
    chatPayload: {
      turns: [{ role: "user", text: "new", time: "2026-01-02T00:00:00Z" }],
      live: "quiet", delivery: "ok", source: "tmux",
      chat_started: "2026-01-02T00:00:00Z", prior_turns: 1, assets: {},
    },
  });
  await ctx.feature.loadHistory("n1", "seam");
  assert.match(ctx.roots.msgs.innerHTML, /data-bk="h:0:0"/);
  assert.match(ctx.roots.msgs.innerHTML, /histseam|chat started/);
  assert.match(ctx.roots.msgs.innerHTML, /data-time=/);
});

test("pending jump into an earlier segment starts a full-history load", async () => {
  const ctx = makeFeature({
    chatPayload: {
      turns: [{ role: "user", text: "new", time: "2026-01-02T00:00:00Z" }],
      live: "quiet", delivery: "ok", source: "tmux",
      chat_started: "2026-01-02T00:00:00Z", prior_turns: 1, assets: {},
    },
  });
  ctx.setPending({
    node: "n1", uid: "old-uid", segment: 0, record: 1,
    turnTime: "2026-01-01T00:00:01Z", ts: 999_999,
  });
  await ctx.feature.render();
  await new Promise(resolve => setImmediate(resolve));
  assert.ok(ctx.apiCalls.some(c => c.path.includes("/chat?history=1")));
});

test("terminal toggle enable/disable and scrollend exist", async () => {
  const { feature, roots } = makeFeature({
    chatPayload: {
      turns: [], live: "quiet", delivery: "ok", source: "tmux",
      chat_started: "", prior_turns: 0, assets: {}, pending: true,
    },
  });
  feature.bind();
  await feature.render();
  // empty turns force peek → toggle disabled
  assert.equal(roots.termtoggle.disabled, true);
  assert.equal(roots.scrollend.innerHTML, "↓↓");
  feature.destroy();
});

test("jumpToNow with rendered chat pins bottom", async () => {
  const { feature, roots } = makeFeature();
  roots.msgs.scrollHeight = 1000;
  roots.msgs.clientHeight = 200;
  roots.msgs.scrollTop = 0;
  await feature.render();
  roots.msgs.scrollTop = 0;
  feature.jumpToNow();
  // pinDecision lands at max = scrollHeight - clientHeight (the browser clamps)
  assert.equal(roots.msgs.scrollTop, 800);
  assert.ok(nearBottom(roots.msgs.scrollHeight, roots.msgs.scrollTop, roots.msgs.clientHeight));
});

test("chat rebuild pins a near-bottom reader but preserves a reader scrolled up", async () => {
  const near = makeFeature();
  near.roots.msgs.scrollHeight = 1000;
  near.roots.msgs.clientHeight = 200;
  near.roots.msgs.scrollTop = 750; // 50px from bottom, inside the 80px fence
  await near.feature.render();
  assert.equal(near.roots.msgs.scrollTop, 800);

  const up = makeFeature();
  up.roots.msgs.scrollHeight = 1000;
  up.roots.msgs.clientHeight = 200;
  up.roots.msgs.scrollTop = 500; // 300px from bottom
  await up.feature.render();
  assert.equal(up.roots.msgs.scrollTop, 500);
});

test("switching to another chat lands at the newest bubble even if the reader was scrolled up", async () => {
  const { feature, roots } = makeFeature();
  roots.msgs.scrollHeight = 1000;
  roots.msgs.clientHeight = 200;
  roots.msgs.scrollTop = 500; // reading back in the chat we are leaving
  await feature.render();
  assert.equal(roots.msgs.scrollTop, 500, "reader scrolled up in the same chat stays put");

  feature.onSelectChange();          // select() calls this before refreshChat
  roots.msgs.scrollHeight = 2400;    // the other chat is a different length
  await feature.render();
  assert.equal(roots.msgs.scrollTop, 2200); // max = 2400 - 200
});

test("a jump-to-source outranks the open-at-newest pin of the selection change", async () => {
  const ctx = makeFeature({
    chatPayload: {
      turns: [{ role: "user", text: "new", time: "2026-01-02T00:00:00Z" }],
      live: "quiet", delivery: "ok", source: "tmux",
      chat_started: "2026-01-02T00:00:00Z", prior_turns: 1, assets: {},
    },
  });
  ctx.roots.msgs.scrollHeight = 1000;
  ctx.roots.msgs.clientHeight = 200;
  ctx.roots.msgs.scrollTop = 500;
  ctx.setPending({
    node: "n1", uid: "old-uid", segment: 0, record: 1,
    turnTime: "2026-01-01T00:00:01Z", ts: Date.now(),
  });
  ctx.feature.onSelectChange();   // notes/search jump: setPendingJump then select()
  await ctx.feature.render();
  await new Promise(resolve => setImmediate(resolve));
  assert.ok(ctx.apiCalls.some(c => c.path.includes("/chat?history=1")),
    "the jump still reaches for its segment instead of being pinned to the bottom");
  assert.equal(ctx.roots.msgs.scrollTop, 500, "no bottom pin while the jump is aiming");
});

test("loading/empty state when no selection", async () => {
  const { feature, effects } = makeFeature({ sel: "" });
  await feature.render();
  assert.equal(effects.setComposerBusy.at(-1), false);
});

test("details click and description longpress use real bound adapters", () => {
  let descPress = null;
  const { feature, roots } = makeFeature({
    deps: {
      longpress: (_root, selector, fn) => {
        if (selector === "#chatdesc") descPress = fn;
        return () => {};
      },
    },
  });
  feature.bind();
  feature.renderHead();
  firstListener(roots.infobtn, "click")();
  assert.equal(roots.chatdetails.hidden, false);
  descPress();
  assert.equal(feature.isEditingDesc(), true);
  feature.cancelDescEdit();
  assert.equal(feature.isEditingDesc(), false);
  feature.destroy();
});

test("poll-driven head renders preserve active title and description edits", () => {
  let descPress = null;
  const ctx = makeFeature({
    deps: {
      editingTitle: () => "n1",
      editingTitleScope: () => "chat",
      longpress: (_root, selector, fn) => {
        if (selector === "#chatdesc") descPress = fn;
        return () => {};
      },
    },
  });
  const titleEditor = el("input", { dataset: { titleInput: "n1" }, value: "draft title" });
  ctx.roots.chattitle.innerHTML = "KEEP-TITLE-EDITOR";
  ctx.roots.chattitle.querySelector = selector => selector.includes("data-title-input")
    ? titleEditor : null;
  ctx.feature.bind();
  ctx.feature.renderHead();
  assert.equal(ctx.roots.chattitle.innerHTML, "KEEP-TITLE-EDITOR");

  descPress();
  ctx.roots.chatdescinput.value = "draft description";
  ctx.roots.chatdescinput.dataset.node = "n1";
  ctx.nodes[0].description = "new server description";
  ctx.feature.renderHead();
  assert.equal(ctx.roots.chatdescinput.value, "draft description");
  ctx.feature.destroy();
});

test("same-node reselection clears the saved-description indicator", async () => {
  let descPress = null;
  const ctx = makeFeature({
    api: async path => path.endsWith("/n1")
      ? { id: "n1", title: "Alpha", agent: "claude", model: "sonnet", live: "quiet" }
      : {},
    deps: {
      setTimeout: () => 1,
      clearTimeout: () => {},
      longpress: (_root, selector, fn) => {
        if (selector === "#chatdesc") descPress = fn;
        return () => {};
      },
    },
  });
  ctx.feature.bind();
  ctx.feature.renderHead();
  descPress();
  firstListener(ctx.roots.infobtn, "click")();
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(ctx.roots.infobtn.classList.contains("saving"), true);
  ctx.feature.onReselect();
  ctx.feature.renderHead();
  assert.equal(ctx.roots.infobtn.classList.contains("saving"), false);
  ctx.feature.destroy();
});

test("render-owned terminal listeners are singular and removed on destroy", async () => {
  const termfull = el("button", { id: "termfull" });
  const decresolve = el("button", { id: "decresolve" });
  const ctx = makeFeature({
    chatPayload: {
      turns: [], live: "quiet", delivery: "unconfirmed", source: "tmux",
      chat_started: "", prior_turns: 0, assets: {}, pending: true,
    },
    deps: {
      document: {
        createElement: tag => el(tag),
        querySelector: selector => selector === "#termfull" ? termfull
          : selector === "#decresolve" ? decresolve : null,
      },
    },
  });
  await ctx.feature.render();
  assert.equal(termfull.listenerCount("click"), 1);
  assert.equal(decresolve.listenerCount("click"), 1);
  ctx.feature.invalidate();
  await ctx.feature.render();
  assert.equal(termfull.listenerCount("click"), 1);
  assert.equal(decresolve.listenerCount("click"), 1);
  ctx.feature.destroy();
  assert.equal(termfull.listenerCount("click"), 0);
  assert.equal(decresolve.listenerCount("click"), 0);
});

/* ---------- P4: expand ask in place + preserve .permask scrollTop ---------- */

test("click .permmore expands keyrow for that node without POSTing /key", async () => {
  const ctx = makeFeature({
    nodes: acpApprovalNodes(),
    chatPayload: acpApprovalPayload(),
  });
  ctx.feature.bind();
  await ctx.feature.render();
  assert.match(ctx.roots.keyrow.innerHTML, /class="permask clamped"/);
  assert.match(ctx.roots.keyrow.innerHTML, /class="permmore"/);
  assert.match(ctx.roots.keyrow.innerHTML, /class="permbtns"/);

  const beforeKeys = ctx.apiCalls.filter(c => c.path.includes("/key")).length;
  const more = el("button", { className: "permmore" });
  firstListener(ctx.roots.keyrow, "click")({ target: more });
  await new Promise(resolve => setImmediate(resolve));

  assert.doesNotMatch(ctx.roots.keyrow.innerHTML, /clamped/, "expanded drops .clamped");
  assert.doesNotMatch(ctx.roots.keyrow.innerHTML, /permmore/, "expanded omits show-all");
  assert.match(ctx.roots.keyrow.innerHTML, /class="permask"/);
  assert.match(ctx.roots.keyrow.innerHTML, /class="permbtn allow"/);
  assert.equal(
    ctx.apiCalls.filter(c => c.path.includes("/key")).length,
    beforeKeys,
    ".permmore must not POST /key",
  );
  // keyrow still painted — attention was not collapsed
  assert.notEqual(ctx.roots.keyrow.innerHTML, "");
  ctx.feature.destroy();
});

test("expand state clears on node switch and on attention leave", async () => {
  const nodes = [
    ...acpApprovalNodes("n1"),
    ...acpApprovalNodes("n2").map(n => ({ ...n, title: "Other" })),
  ];
  let payload = acpApprovalPayload();
  const ctx = makeFeature({
    nodes,
    api: async (path) => {
      if (path.includes("/peek")) return "p";
      if (path.includes("/chat")) return payload;
      return {};
    },
  });
  ctx.feature.bind();
  await ctx.feature.render();
  firstListener(ctx.roots.keyrow, "click")({ target: el("button", { className: "permmore" }) });
  await new Promise(resolve => setImmediate(resolve));
  assert.doesNotMatch(ctx.roots.keyrow.innerHTML, /clamped/);

  // switch away → collapsed when we come back
  ctx.setSel("n2");
  ctx.feature.onSelectChange();
  await ctx.feature.render();
  ctx.setSel("n1");
  ctx.feature.onSelectChange();
  await ctx.feature.render();
  assert.match(ctx.roots.keyrow.innerHTML, /class="permask clamped"/,
    "reselect after switch starts collapsed");
  assert.match(ctx.roots.keyrow.innerHTML, /permmore/);

  // expand again, then attention clears
  firstListener(ctx.roots.keyrow, "click")({ target: el("button", { className: "permmore" }) });
  await new Promise(resolve => setImmediate(resolve));
  assert.doesNotMatch(ctx.roots.keyrow.innerHTML, /clamped/);

  payload = acpApprovalPayload({ attention: "", perm_options: [], perm_title: "" });
  ctx.feature.invalidate();
  await ctx.feature.render();
  assert.equal(ctx.roots.keyrow.innerHTML, "", "no attention → empty keyrow");

  // attention returns → starts collapsed again
  payload = acpApprovalPayload();
  nodes[0].attention = "approval";
  ctx.feature.invalidate();
  await ctx.feature.render();
  assert.match(ctx.roots.keyrow.innerHTML, /class="permask clamped"/,
    "attention return after clear starts collapsed");
  ctx.feature.destroy();
});

test("expanded rebuild restores non-zero .permask scrollTop", async () => {
  const ctx = makeFeature({
    nodes: acpApprovalNodes(),
    chatPayload: acpApprovalPayload(),
  });
  ctx.feature.bind();
  await ctx.feature.render();
  firstListener(ctx.roots.keyrow, "click")({ target: el("button", { className: "permmore" }) });
  await new Promise(resolve => setImmediate(resolve));

  const mask = ctx.roots.keyrow.querySelector(".permask");
  assert.ok(mask, "expanded render materialises .permask");
  assert.equal(mask.classList.contains("clamped"), false);
  mask.scrollTop = 240;

  // poll-equivalent rebuild: clear sig, same payload
  ctx.feature.invalidate();
  await ctx.feature.render();
  const after = ctx.roots.keyrow.querySelector(".permask");
  assert.ok(after, "rebuild keeps .permask");
  assert.equal(after.classList.contains("clamped"), false);
  assert.equal(after.scrollTop, 240, "scrollTop survives keyrow.innerHTML reassignment");
  ctx.feature.destroy();
});

/* ---------- P2: the approval row — reject colour + conditional "show all" ----------
 * (a) .permbtn.reject must not shout --danger: declining is the safe branch;
 *     --attn is the attention surface; --danger stays for delete/archive.
 * (b) .permmore starts hidden and is revealed when .permask overflows
 *     vertically. A long single-line .permcode wraps (pre-wrap + word-break),
 *     so it grows line boxes and CAN unhide "show all" — G1a. Case 8 is the
 *     existing expanded:true test above — stays green. */

function permcodeRuleBody(css){
  const m = css.match(/\.permcode\s*\{([^}]+)\}/);
  assert.ok(m, ".permcode rule present");
  return m[1];
}

/* G1a: structural — a 400-char single-line command must produce multiple line
 * boxes, not one wide pre row. pre-wrap keeps leading whitespace (heredocs);
 * word-break / overflow-wrap lets unbroken tokens wrap inside the clamp. */
test("G1a: .permcode wraps long single-line commands (pre-wrap + break)", () => {
  const body = permcodeRuleBody(chatCssSrc);
  assert.match(body, /white-space\s*:\s*pre-wrap/,
    "pre-wrap: keep indentation, allow soft wraps at line ends");
  assert.ok(
    /overflow-wrap\s*:\s*(anywhere|break-word)/.test(body) ||
      /word-break\s*:\s*break-all/.test(body) ||
      /word-break\s*:\s*break-word/.test(body),
    "word-break or overflow-wrap so a 400-char token wraps into line boxes");
  assert.doesNotMatch(body, /white-space\s*:\s*pre\s*;/,
    "plain pre (no wrap) is the G1a bug — long commands stay one line box");
  assert.doesNotMatch(body, /overflow-x\s*:\s*auto/,
    "horizontal scroll is no longer the only way to read a long command");
  /* Keep the monospace shell-code chrome. */
  assert.match(body, /ui-monospace|monospace/);
  assert.match(body, /border-radius/);
  assert.match(body, /padding/);
  assert.match(body, /background/);
});

/* G1a: invert the stale "horizontal overflow must not unhide" contract.
 * A long single-line .permcode ask, once wrapped, overflows the 3-line clamp
 * vertically and the measure pass must be able to unhide "show all".
 * (makeKeyrow only stubs .permask/.permmore — body class is checked on HTML.) */
test("G1a: long single-line .permcode ask can unhide show all", async () => {
  const longCmd = "x".repeat(400);
  const ctx = makeFeature({
    nodes: acpApprovalNodes(),
    chatPayload: acpApprovalPayload({
      perm_title: longCmd,
      perm_tool_kind: "execute",
    }),
  });
  /* After wrapping, a 400-char command is many line boxes → vertical overflow. */
  ctx.roots.keyrow._permMaskMetrics = { scrollHeight: 400, clientHeight: 72 };
  ctx.feature.bind();
  await ctx.feature.render();
  assert.match(ctx.roots.keyrow.innerHTML, /class="permcode"/,
    "execute kind uses .permcode body for a bare long command");
  assert.match(ctx.roots.keyrow.innerHTML, new RegExp("x{400}"),
    "the full command is in the ask body");
  const more = ctx.roots.keyrow.querySelector(".permmore");
  assert.ok(more, ".permmore present for clamped long ask");
  assert.equal(more.hidden, false,
    "wrapped long single-line command must unhide show all (was G1a bug)");
  ctx.feature.destroy();
});

const baseCssSrc = readFileSync(join(__dirname, "../css/base.css"), "utf8");

function rejectRuleBody(css){
  /* Shared rule: .permbtn.reject, .permbtn.reject-always { … } */
  const m = css.match(/\.permbtn\.reject\s*,\s*\.permbtn\.reject-always\s*\{([^}]+)\}/);
  assert.ok(m, ".permbtn.reject, .permbtn.reject-always rule present");
  return m[1];
}

function allowRuleBody(css){
  const m = css.match(/\.permbtn\.allow\s*,\s*\.permbtn\.allow-always\s*\{([^}]+)\}/);
  assert.ok(m, ".permbtn.allow, .permbtn.allow-always rule present");
  return m[1];
}

test("P2 1: .permbtn.reject rule body uses --attn, not --danger", () => {
  const body = rejectRuleBody(chatCssSrc);
  assert.match(body, /border-color\s*:\s*var\(--attn\)/,
    "reject border is the attention surface, not destructive red");
  assert.doesNotMatch(body, /--danger/,
    "declining a tool call is not delete/archive");
  assert.doesNotMatch(body, /(?:^|[^-])color\s*:/,
    "no color override — label inherits --ink like every other option");
});

test("P2 2: .permbtn.reject-always rule body uses --attn, not --danger", () => {
  /* Same shared rule as case 1 — both selectors must land on the non-danger body. */
  const body = rejectRuleBody(chatCssSrc);
  assert.match(body, /border-color\s*:\s*var\(--attn\)/);
  assert.doesNotMatch(body, /--danger/);
  assert.doesNotMatch(body, /(?:^|[^-])color\s*:/);
  assert.match(chatCssSrc, /\.permbtn\.reject-always/,
    "reject-always still co-selects the shared rule");
});

test("P2 3: .permbtn.allow rule body is unchanged", () => {
  const body = allowRuleBody(chatCssSrc);
  assert.match(body, /border-color\s*:\s*var\(--attn\)/);
  assert.doesNotMatch(body, /--danger/);
  /* allow does not set a color override either — inherits --ink from .permbtn */
  assert.doesNotMatch(body, /(?:^|[^-])color\s*:/);
});

test("P2 4: --danger still used by a delete/archive control elsewhere", () => {
  /* Stripping red from reject must not orphan the token. */
  const danger = baseCssSrc.match(/\.btn-plain\.danger\s*\{([^}]+)\}/);
  assert.ok(danger, ".btn-plain.danger is a delete/archive control");
  assert.match(danger[1], /color\s*:\s*var\(--danger\)/,
    "--danger remains live for destructive UI");
  assert.doesNotMatch(rejectRuleBody(chatCssSrc), /--danger/,
    "approval reject no longer claims the destructive token");
});

test("P2 5: keyRowHTML expanded:false emits .permmore with hidden", () => {
  const html = keyRowHTML({
    attention: "approval", source: "acp",
    permTitle: "Execute `echo hi`",
    permOptions: [{ key: "1", name: "Allow once", kind: "allow" }],
    expanded: false,
  });
  const more = html.match(/<button\b[^>]*class="permmore"[^>]*>/);
  assert.ok(more, ".permmore is rendered when collapsed");
  assert.match(more[0], /\bhidden\b/,
    "starts hidden; the measure pass reveals only when the ask overflows");
  assert.match(html, /show all/);
});

test("P2 6: measure pass keeps .permmore hidden when .permask does not overflow", async () => {
  const ctx = makeFeature({
    nodes: acpApprovalNodes(),
    chatPayload: acpApprovalPayload({
      /* short single-line command — vertical height fits the clamp */
      perm_title: "Execute `echo hi`",
    }),
  });
  /* No vertical overflow: scrollHeight == clientHeight. Short asks stay
     without a dead "show all" control; long wrapped ones use G1a metrics. */
  ctx.roots.keyrow._permMaskMetrics = { scrollHeight: 48, clientHeight: 48 };
  ctx.feature.bind();
  await ctx.feature.render();
  const more = ctx.roots.keyrow.querySelector(".permmore");
  assert.ok(more, ".permmore is in the tree so the measure pass can toggle it");
  assert.equal(more.hidden, true,
    "single-line / non-overflowing ask must not offer a useless control");
  ctx.feature.destroy();
});

test("P2 7: measure pass reveals .permmore when .permask overflows vertically", async () => {
  const ctx = makeFeature({
    nodes: acpApprovalNodes(),
    chatPayload: acpApprovalPayload(), /* multi-line title; harness defaults overflow */
  });
  ctx.roots.keyrow._permMaskMetrics = { scrollHeight: 1000, clientHeight: 200 };
  ctx.feature.bind();
  await ctx.feature.render();
  const more = ctx.roots.keyrow.querySelector(".permmore");
  assert.ok(more, ".permmore present");
  assert.equal(more.hidden, false,
    "vertical overflow unhides show all after the keyrow write");
  ctx.feature.destroy();
});

/* P2 8: expanded:true omits .permmore — covered by the existing test
 * "keyRowHTML: never md() on perm_title…" (assert.doesNotMatch(open, /permmore/)). */

/* ---------- P2b: a measurement that never happened is not a "no" ----------
 * revealPermMoreIfNeeded runs once, after the keyrow write. Polling keeps
 * rendering the chat while body.map-full hides #chat outright (notes.css:186),
 * so the keyrow can be built at 0x0 — and the poll's skip path never writes it
 * again, so nothing re-measures when the dock opens. A long approval then sits
 * clamped to three lines with no way to expand it. Same rule as P1's
 * pinDecision case 1: no box means "unknown", never "no". */

function roSpy(){
  const observers = [];
  class RO {
    constructor(cb){ this.cb = cb; this.targets = []; this.disconnected = false; observers.push(this); }
    observe(t){ this.targets.push(t); }
    unobserve(){}
    disconnect(){ this.disconnected = true; }
  }
  const forMask = () => observers.find(o =>
    o.targets.some(t => String(t && t.className || "").includes("permask")));
  return { RO, observers, forMask };
}

test("P2b 1: permMoreDecision cannot decide without a layout box", () => {
  assert.equal(chatmod.permMoreDecision(0, 0), chatmod.PERM_MORE_UNKNOWN,
    "display:none measures 0x0 — that is unknown, not 'nothing to show'");
  assert.equal(chatmod.permMoreDecision(1000, 0), chatmod.PERM_MORE_UNKNOWN);
  assert.equal(chatmod.permMoreDecision(0, undefined), chatmod.PERM_MORE_UNKNOWN);
});

test("P2b 2: permMoreDecision still decides normally once there is a box", () => {
  assert.equal(chatmod.permMoreDecision(1000, 200), chatmod.PERM_MORE_SHOW);
  assert.equal(chatmod.permMoreDecision(48, 48), chatmod.PERM_MORE_HIDE);
  assert.equal(chatmod.permMoreDecision(0, 200), chatmod.PERM_MORE_HIDE);
});

test("P2b 3: a keyrow built while the chat is hidden decides when the box arrives", async () => {
  const spy = roSpy();
  const ctx = makeFeature({
    nodes: acpApprovalNodes(),
    chatPayload: acpApprovalPayload(),        /* the long, multi-line ask */
    deps: { ResizeObserver: spy.RO, requestAnimationFrame: () => 0 },
  });
  ctx.roots.keyrow._permMaskMetrics = { scrollHeight: 0, clientHeight: 0 };
  ctx.feature.bind();
  await ctx.feature.render();                  /* body.map-full: #chat display:none */

  const obs = spy.forMask();
  assert.ok(obs, "an unmeasurable ask must arm an observer on .permask");
  const mask = ctx.roots.keyrow.querySelector(".permask");

  /* The user opens the dock / leaves full screen: #chat gains a box. Nothing
     invalidates chatSig, so the next poll takes the skip path — the observer is
     the only thing that can still decide. */
  mask.scrollHeight = 1000;
  mask.clientHeight = 200;
  obs.cb([]);
  assert.equal(ctx.roots.keyrow.querySelector(".permmore").hidden, false,
    "a long ask must become expandable once it can be measured");
  assert.equal(obs.disconnected, true, "decided — stop observing");

  await ctx.feature.render();                  /* skip path must not undo it */
  assert.equal(ctx.roots.keyrow.querySelector(".permmore").hidden, false);
  ctx.feature.destroy();
});

test("P2b 4: with no ResizeObserver, an unmeasurable ask fails open", async () => {
  const ctx = makeFeature({
    nodes: acpApprovalNodes(),
    chatPayload: acpApprovalPayload(),
  });
  ctx.roots.keyrow._permMaskMetrics = { scrollHeight: 0, clientHeight: 0 };
  ctx.feature.bind();
  await ctx.feature.render();
  assert.equal(ctx.roots.keyrow.querySelector(".permmore").hidden, false,
    "a dead control is a nuisance; an unreadable approval is a dead end");
  ctx.feature.destroy();
});

test("P2b 5: a pending measure observer is released on destroy", async () => {
  const spy = roSpy();
  const ctx = makeFeature({
    nodes: acpApprovalNodes(),
    chatPayload: acpApprovalPayload(),
    deps: { ResizeObserver: spy.RO, requestAnimationFrame: () => 0 },
  });
  ctx.roots.keyrow._permMaskMetrics = { scrollHeight: 0, clientHeight: 0 };
  ctx.feature.bind();
  await ctx.feature.render();
  const obs = spy.forMask();
  assert.ok(obs && !obs.disconnected, "observer is live while undecided");
  ctx.feature.destroy();
  assert.equal(obs.disconnected, true, "destroy must not leak the observer");
});

test("P2b 6: an observer firing on a still-empty box must not spin", async () => {
  const observers = [];
  class RO {
    constructor(cb){ this.cb = cb; this.disconnected = false; observers.push(this); }
    /* A real ResizeObserver delivers one callback on observe(). If the measure
       re-arms while the box is still 0x0, that initial delivery is recursion,
       not a retry — the guard is what makes waiting possible at all. */
    observe(t){ this.target = t; this.cb([]); }
    unobserve(){}
    disconnect(){ this.disconnected = true; }
  }
  const ctx = makeFeature({
    nodes: acpApprovalNodes(),
    chatPayload: acpApprovalPayload(),
    deps: { ResizeObserver: RO, requestAnimationFrame: () => 0 },
  });
  ctx.roots.keyrow._permMaskMetrics = { scrollHeight: 0, clientHeight: 0 };
  ctx.feature.bind();
  await ctx.feature.render();
  /* The bottom-pin observer shares the injected class — count only this one. */
  const onMask = observers.filter(o =>
    String(o.target && o.target.className || "").includes("permask"));
  assert.equal(onMask.length, 1,
    `armed once and waited; re-arming on an unmeasurable box is a spin (got ${onMask.length})`);
  const mask = ctx.roots.keyrow.querySelector(".permask");
  mask.scrollHeight = 1000;
  mask.clientHeight = 200;
  onMask[0].cb([]);
  assert.equal(ctx.roots.keyrow.querySelector(".permmore").hidden, false);
  ctx.feature.destroy();
});

/* ---------- the terminal peek is not part of the transcript ----------
 * peekText is a live tmux capture: while an agent animates a spinner it
 * changes on every poll, deterministically. It shared a render region with
 * every chat bubble, so a moving pane re-innerHTML'd the whole transcript
 * twice a second — re-running the markdown parser and building a fresh Intl
 * formatter per turn, for the live segment and every loaded history segment.
 * On a phone that is the poll loop's largest recurring cost.
 * The pane gets its own host: a moving pane repaints only the pane. */

import * as chatExports from "../js/chat.js";

function peekingFeature(peek){
  let peekText = peek;
  const made = makeFeature({
    nodes: [{
      id: "n1", title: "Alpha", agent: "claude", model: "sonnet",
      live: "quiet", attention: "approval", lane_id: "L1", description: "d",
    }],
    api: async (path) => {
      if (path.includes("/peek")) return peekText();
      if (path.includes("/chat")) return {
        turns: [{ role: "user", text: "hello", time: "2026-01-01T00:00:00Z" },
                { role: "assistant", text: "hi", time: "2026-01-01T00:01:00Z" }],
        live: "quiet", delivery: "ok", source: "tmux", attention: "approval",
        chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
      };
      return {};
    },
  });
  let writes = 0, html = "";
  Object.defineProperty(made.roots.msgs, "innerHTML", {
    configurable: true,
    get(){ return html; },
    /* a real innerHTML write discards the element's children — the fake node
       keeps an array, so model it here or a stale host would survive. */
    set(v){ writes++; html = v; made.roots.msgs.children.length = 0; },
  });
  return {
    ...made,
    msgsWrites: () => writes,
    msgsHTML: () => html,
    peekHost: () => (made.roots.msgs.children || []).find(c => c.id === "peekhost") || null,
  };
}

test("buildChatSignature does not carry the terminal pane", () => {
  const base = {
    nodeId: "n1", attention: "", delivery: "ok", showPeek: true,
    termOpen: false, termFull: false, source: "tmux", priorTurns: 0,
    turnsHash: 7,
  };
  assert.equal(
    buildChatSignature({ ...base, peekHash: 1 }),
    buildChatSignature({ ...base, peekHash: 2 }),
    "a moving pane must not invalidate the transcript");
  // live never appears in the #msgs markup — it drives applyChatActivityChrome,
  // which is outside the render region.
  assert.equal(
    buildChatSignature({ ...base, live: "active" }),
    buildChatSignature({ ...base, live: "quiet" }),
    "a turn starting or ending must not rebuild every bubble");
  assert.notEqual(buildChatSignature({ ...base, turnsHash: 8 }), buildChatSignature(base),
    "a real transcript change still rebuilds");
});

test("buildPeekSignature tracks everything the pane block renders", () => {
  const base = { showPeek: true, peekHash: 1, forcePeek: true, label: "L", termFull: false, unconfirmed: false };
  const sig = o => chatExports.buildPeekSignature({ ...base, ...o });
  assert.equal(sig({}), sig({}), "stable input, stable signature");
  assert.notEqual(sig({ peekHash: 2 }), sig({}), "new pane text");
  assert.notEqual(sig({ showPeek: false }), sig({}), "pane hidden");
  assert.notEqual(sig({ forcePeek: false }), sig({}), "heading switches to Terminal");
  assert.notEqual(sig({ label: "M" }), sig({}), "attention label");
  assert.notEqual(sig({ termFull: true }), sig({}), "expanded pane");
  assert.notEqual(sig({ unconfirmed: true }), sig({}), "the delivery notice and its button");
});

test("a moving pane repaints the pane, not the transcript", async () => {
  let pane = "frame one";
  const f = peekingFeature(() => pane);
  await f.feature.render();
  const afterFirst = f.msgsWrites();
  assert.ok(afterFirst > 0, "the first render must build the transcript");
  const host = f.peekHost();
  assert.ok(host, "the pane needs its own host inside #msgs");
  assert.match(host.innerHTML, /frame one/);
  assert.doesNotMatch(f.msgsHTML(), /frame one/,
    "the pane text must not be baked into the transcript markup");

  pane = "frame two";
  await f.feature.render();
  assert.equal(f.msgsWrites(), afterFirst,
    "a new pane frame must not rebuild a single bubble");
  assert.match(f.peekHost().innerHTML, /frame two/, "but the pane itself must move");
});

test("a real transcript change still rebuilds, and re-hosts the pane", async () => {
  let pane = "pane";
  let turns = [{ role: "user", text: "hello", time: "2026-01-01T00:00:00Z" }];
  const made = makeFeature({
    nodes: [{
      id: "n1", title: "Alpha", agent: "claude", model: "sonnet",
      live: "quiet", attention: "approval", lane_id: "L1", description: "d",
    }],
    api: async (path) => {
      if (path.includes("/peek")) return pane;
      if (path.includes("/chat")) return {
        turns, live: "quiet", delivery: "ok", source: "tmux", attention: "approval",
        chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
      };
      return {};
    },
  });
  await made.feature.render();
  const host1 = (made.roots.msgs.children || []).find(c => c.id === "peekhost");
  assert.ok(host1);
  turns = [...turns, { role: "assistant", text: "hi", time: "2026-01-01T00:01:00Z" }];
  await made.feature.render();
  const hosts = (made.roots.msgs.children || []).filter(c => c.id === "peekhost");
  assert.equal(hosts.length, 1, "a rebuild must not leave a second pane host behind");
  assert.match(hosts[0].innerHTML, /pane/,
    "the pane has to be repainted into the fresh host, or it vanishes on rebuild");
});

test("an empty pane host takes no room in the message column", () => {
  // #msgs is a flex column with a 12px gap, so an empty host would push a
  // phantom gap under the last bubble on every chat without a pane.
  assert.match(chatCssSrc, /#peekhost:empty\s*\{[^}]*display:\s*none/,
    "chat.css must collapse the empty host");
});

test("the chat signature hashes the turns without materialising them", () => {
  /* This runs before the skip check, so it is paid on every poll on both
     branches — the string it used to build was the entire conversation. */
  assert.match(chatSrc, /turnsHash:\s*hashTurns\(turns\)/,
    "the signature must use the streaming hash");
  assert.doesNotMatch(chatSrc, /turns\.map\([^)]*\)\.join\(/,
    "no full-transcript string may be built per tick");
  assert.match(chatSrc, /hashTurns[^;]*from "\.\/lanes\.js"|import \{[^}]*hashTurns[^}]*\} from "\.\/lanes\.js"/,
    "reuse the primitive, do not reimplement djb2 here");
});

/* ---------- P1: the chat that opens blank — scroller compositing + bottom pin ----------
 * Two independent faults on a node switch: (a) a filter/opacity transition on
 * #msgs leaves a stale WebKit layer after innerHTML mid-transition; (b) the
 * bottom pin is a one-shot assignment that is discarded when the box has no
 * layout, and nothing re-asserts when content settles or the peek host grows.
 * Pure pinDecision + CSS declaration tests + DOM refreshChat cases. */

/* Anchor the base #msgs rule (not body.cards-open, not .loading, not reduced-motion).
   It is the rule that immediately follows #msgwrap and carries overflow-y: auto. */
function baseMsgsRuleBody(css){
  const m = css.match(/#msgwrap\s*\{[^}]*\}\s*#msgs\s*\{([^}]+)\}/);
  assert.ok(m, "base #msgs rule must follow #msgwrap");
  assert.match(m[1], /overflow-y\s*:\s*auto/, "base #msgs is the overflow scroller");
  return m[1];
}

function loadingMsgsRuleBody(css){
  const m = css.match(/#msgwrap\.loading\s+#msgs\s*\{([^}]+)\}/);
  assert.ok(m, "#msgwrap.loading #msgs rule present");
  return m[1];
}

/* nearBottom alone cannot see the symptom P1 exists to fix: its
   scrollHeight - scrollTop - clientHeight goes *negative* when the reader is
   stranded past the end, which reads as "well within the fence". A reader
   marooned 5000px below the last bubble would satisfy it. So bound the offset
   from above as well — at the bottom means at the bottom, not beyond it. */
function atBottom(el){
  const max = Math.max(0, (el.scrollHeight || 0) - (el.clientHeight || 0));
  return nearBottom(el.scrollHeight, el.scrollTop, el.clientHeight, SCROLL_NEAR_BOTTOM_PX)
    && (el.scrollTop || 0) <= max;
}

test("P1 A1: base #msgs has no -webkit-overflow-scrolling", () => {
  const body = baseMsgsRuleBody(chatCssSrc);
  assert.doesNotMatch(body, /-webkit-overflow-scrolling/,
    "momentum scrolling is the default since iOS 13; the prefix forces a layer");
  // siblings stay put
  assert.match(chatCssSrc, /body\.cards-open\s+#msgs\s*\{[^}]*overflow:\s*hidden/,
    "body.cards-open #msgs is untouched");
  assert.match(chatCssSrc, /#peekhost:empty\s*\{[^}]*display:\s*none/,
    "#peekhost:empty is untouched");
});

test("P1 A2: base #msgs transition does not name filter or opacity", () => {
  const body = baseMsgsRuleBody(chatCssSrc);
  const tr = body.match(/transition\s*:\s*([^;]+)/);
  if (tr){
    assert.doesNotMatch(tr[1], /\bfilter\b/, "filter must not transition on the scroller");
    assert.doesNotMatch(tr[1], /\bopacity\b/, "opacity must not transition on the scroller");
  }
  // a bare "filter" somewhere else in the file is not a free pass — the base body is the hazard
  assert.doesNotMatch(body, /transition\s*:[^;]*\bfilter\b/);
  assert.doesNotMatch(body, /transition\s*:[^;]*\bopacity\b/);
});

test("P1 A3: #msgwrap.loading #msgs keeps pointer-events:none, drops filter/opacity", () => {
  const body = loadingMsgsRuleBody(chatCssSrc);
  assert.match(body, /pointer-events\s*:\s*none/,
    "pointer-events: none is load-bearing and stays");
  assert.doesNotMatch(body, /\bfilter\s*:/,
    "cold-load dimming must not put a filter on the scroller");
  assert.doesNotMatch(body, /\bopacity\s*:/,
    "cold-load dimming must not put opacity on the scroller");
});

test("P1 A4: #chatloading overlay still present and [hidden] by default", () => {
  assert.match(chatCssSrc, /#chatloading\s*\{/,
    "the load affordance lives on #chatloading, not on the scroller");
  const hidden = chatCssSrc.match(/#chatloading\[hidden\]\s*\{([^}]+)\}/);
  assert.ok(hidden, "#chatloading[hidden] rule present");
  assert.match(hidden[1], /display\s*:\s*none/);
  // Backdrop tint/blur still belongs to the overlay and never to the scroller.
  // It sits on the overlay's own .scrim layer rather than the shell — see
  // LI-A3 for why the indicator must not be inside a filtered box.
  const scrim = chatCssSrc.match(/#chatloading\s+\.scrim\s*\{([^}]+)\}/);
  assert.ok(scrim, "#chatloading .scrim rule present");
  assert.match(scrim[1], /backdrop-filter|background/,
    "the overlay still tints/blurs the backdrop on its own");
  assert.doesNotMatch(baseMsgsRuleBody(chatCssSrc), /backdrop-filter|filter\s*:/,
    "and the dimming never migrates onto the scroller");
});

test("P1 pure 1: no layout box keeps intent pending", () => {
  assert.equal(typeof chatExports.pinDecision, "function", "pinDecision must be exported");
  const r = chatExports.pinDecision({
    scrollTop: 400, scrollHeight: 3000, clientHeight: 0, want: true,
  });
  assert.equal(r.action, "none");
  assert.equal(r.pending, true);
});

test("P1 pure 2: want pin with a real box lands at max scrollTop", () => {
  const r = chatExports.pinDecision({
    scrollTop: 0, scrollHeight: 3000, clientHeight: 600, want: true,
  });
  assert.equal(r.action, "pin");
  assert.equal(r.scrollTop, 2400);
  assert.equal(r.pending, false);
});

test("P1 pure 3: over-scrolled clamps back to max", () => {
  const r = chatExports.pinDecision({
    scrollTop: 9000, scrollHeight: 3000, clientHeight: 600, want: false,
  });
  assert.equal(r.action, "clamp");
  assert.equal(r.scrollTop, 2400);
});

test("P1 pure 4: already at bottom, no want → none", () => {
  const r = chatExports.pinDecision({
    scrollTop: 2400, scrollHeight: 3000, clientHeight: 600, want: false,
  });
  assert.equal(r.action, "none");
  assert.equal(r.pending, false);
});

test("P1 pure 5: deliberate scroll-up is never yanked down", () => {
  const r = chatExports.pinDecision({
    scrollTop: 200, scrollHeight: 3000, clientHeight: 600, want: false,
  });
  assert.equal(r.action, "none");
  assert.equal(r.scrollTop, 200);
});

test("P1 pure 6: content shorter than the box never goes negative", () => {
  const pin = chatExports.pinDecision({
    scrollTop: 50, scrollHeight: 200, clientHeight: 600, want: true,
  });
  assert.equal(pin.scrollTop, 0);
  assert.ok(pin.scrollTop >= 0);
  const clamp = chatExports.pinDecision({
    scrollTop: 50, scrollHeight: 200, clientHeight: 600, want: false,
  });
  assert.equal(clamp.action, "clamp");
  assert.equal(clamp.scrollTop, 0);
});

test("P1 DOM 7: node switch with a stale large scrollTop ends near the bottom", async () => {
  const { feature, roots } = makeFeature();
  roots.msgs.scrollHeight = 3000;
  roots.msgs.clientHeight = 600;
  roots.msgs.scrollTop = 9000; // inherited from a taller previous chat
  feature.onSelectChange();
  await feature.render();
  assert.ok(atBottom(roots.msgs),
    `expected near bottom, got scrollTop=${roots.msgs.scrollTop} of max=${roots.msgs.scrollHeight - roots.msgs.clientHeight}`);
  feature.destroy();
});

test("P1 DOM 8: pin while clientHeight is 0 re-asserts once the box gains height", async () => {
  const rafs = [];
  let roCb = null;
  const { feature, roots } = makeFeature({
    deps: {
      requestAnimationFrame: (fn) => { rafs.push(fn); return rafs.length; },
      ResizeObserver: class {
        constructor(cb){ roCb = cb; }
        observe(){}
        disconnect(){}
        unobserve(){}
      },
    },
  });
  roots.msgs.scrollHeight = 3000;
  roots.msgs.clientHeight = 0; // display:none / map-full — no layout box
  roots.msgs.scrollTop = 0;
  feature.onSelectChange();
  await feature.render();
  // intent must survive: the pin assignment with no box is a no-op
  assert.equal(roots.msgs.clientHeight, 0);
  // box appears (user left the wall map / chat became visible)
  roots.msgs.clientHeight = 600;
  // re-assert via rAF and/or ResizeObserver — both are injected through deps
  assert.ok(rafs.length > 0 || typeof roCb === "function",
    "pending pin must schedule rAF and/or attach a ResizeObserver via deps");
  for (const fn of rafs.splice(0)) fn();
  if (typeof roCb === "function") roCb([]);
  assert.ok(atBottom(roots.msgs),
    `after layout, expected near bottom, got scrollTop=${roots.msgs.scrollTop}`);
  feature.destroy();
});

test("P1 DOM 9: ResizeObserver re-clamps when content shrinks after a pin", async () => {
  let roCb = null;
  const { feature, roots } = makeFeature({
    deps: {
      requestAnimationFrame: () => 0,
      ResizeObserver: class {
        constructor(cb){ roCb = cb; }
        observe(){}
        disconnect(){}
        unobserve(){}
      },
    },
  });
  roots.msgs.scrollHeight = 3000;
  roots.msgs.clientHeight = 600;
  roots.msgs.scrollTop = 0;
  feature.onSelectChange();
  await feature.render();
  assert.ok(atBottom(roots.msgs), "initial pin lands at bottom");
  // content shrinks (history collapsed, tiles reflowed) leaving scrollTop past max
  roots.msgs.scrollHeight = 1000;
  roots.msgs.scrollTop = 2400; // was max of the old height
  assert.ok(typeof roCb === "function", "ResizeObserver must be installed via deps");
  roCb([]);
  assert.ok(roots.msgs.scrollTop <= Math.max(0, roots.msgs.scrollHeight - roots.msgs.clientHeight),
    "shrink must re-clamp; blank viewport is the bug");
  assert.equal(roots.msgs.scrollTop, 400);
  feature.destroy();
});

test("P1 DOM 10: verified pin clears intent exactly once; later ticks do not re-pin a reader who scrolled up", async () => {
  const { feature, roots } = makeFeature();
  roots.msgs.scrollHeight = 3000;
  roots.msgs.clientHeight = 600;
  roots.msgs.scrollTop = 0;
  feature.onSelectChange();
  await feature.render();
  assert.ok(atBottom(roots.msgs), "first pin");
  // reader scrolls up deliberately after the pin cleared
  roots.msgs.scrollTop = 200;
  feature.invalidate();
  await feature.render();
  assert.equal(roots.msgs.scrollTop, 200,
    "intent was cleared on the verified pin; a later rebuild must not yank them down");
  feature.destroy();
});

test("P1 DOM 11: reported repro — A→B→A with stale scrollTop and changing clientHeight", async () => {
  const nodes = [
    { id: "A", title: "Operator A", agent: "claude", model: "s", live: "quiet", attention: "", lane_id: "", description: "" },
    { id: "B", title: "Operator B", agent: "claude", model: "s", live: "quiet", attention: "", lane_id: "", description: "" },
  ];
  let sel = "A";
  const payload = (id) => ({
    turns: [
      { role: "user", text: `hello ${id}`, time: "2026-01-01T00:00:00Z" },
      { role: "assistant", text: `hi ${id}`, time: "2026-01-01T00:01:00Z" },
    ],
    live: "quiet", delivery: "ok", source: "tmux",
    chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
  });
  const { feature, roots, setSel } = makeFeature({
    nodes,
    sel: "A",
    api: async (path) => {
      if (path.includes("/peek")) return "";
      if (path.includes("/chat")) return payload(sel);
      return {};
    },
  });
  // sit in A at the bottom
  roots.msgs.scrollHeight = 3000;
  roots.msgs.clientHeight = 600;
  roots.msgs.scrollTop = 2400;
  await feature.render();
  assert.ok(atBottom(roots.msgs), "A starts at bottom");

  // send to… B: draft written, select(B), render B
  feature.onSelectChange();
  setSel("B");
  sel = "B";
  roots.msgs.scrollHeight = 1800;
  roots.msgs.clientHeight = 500;
  await feature.render();
  assert.ok(atBottom(roots.msgs), "B opens at its newest bubble");

  // switch back to A — #msgs still carries B's offset / a large stale scrollTop,
  // and the clientHeight changes between the two renders (orientation / chrome)
  feature.onSelectChange();
  setSel("A");
  sel = "A";
  roots.msgs.scrollTop = 9000;
  roots.msgs.scrollHeight = 4000;
  roots.msgs.clientHeight = 700;
  await feature.render();
  assert.ok(atBottom(roots.msgs),
    `A must end near its own bottom after A→B→A; scrollTop=${roots.msgs.scrollTop} max=${4000 - 700}`);
  feature.destroy();
});

test("P1 DOM 12: cold-load class toggle still honours chatScrollBottom", async () => {
  const timers = [];
  const { feature, roots } = makeFeature({
    deps: {
      setTimeout: (fn) => { timers.push(fn); return timers.length; },
      clearTimeout: () => {},
      requestAnimationFrame: () => 0,
      ResizeObserver: class { constructor(){} observe(){} disconnect(){} },
    },
  });
  roots.msgs.scrollHeight = 3000;
  roots.msgs.clientHeight = 600;
  roots.msgs.scrollTop = 100;
  // cold path: onSelectChange clears chatSig so beginChatLoad schedules .loading
  feature.onSelectChange();
  const renderP = feature.render();
  // fire the delayed load affordance while the fetch is in flight
  for (const fn of timers.splice(0)) fn();
  assert.equal(roots.msgwrap.classList.contains("loading"), true,
    "cold load adds .loading after CHAT_LOAD_DELAY_MS");
  assert.equal(roots.chatloading.hidden, false);
  await renderP;
  // endChatLoad clears .loading regardless of class-toggle ordering vs pin
  assert.equal(roots.msgwrap.classList.contains("loading"), false);
  assert.equal(roots.chatloading.hidden, true);
  assert.ok(atBottom(roots.msgs),
    "final scroll position is the bottom regardless of the class-toggle ordering");
  feature.destroy();
});

/* Peek growth on the skip path: each distinct capture lengthens #msgs by a
   fixed step large enough that an unre-pinned reader falls outside the
   SCROLL_NEAR_BOTTOM_PX slack (cause b4). Length-proportional growth is too
   small — the old pin assigned scrollTop=scrollHeight (overscrolled), so a
   few dozen extra pixels still looked "near bottom". */
function peekGrowingDocument(msgs, step = 800){
  let frames = 0;
  return {
    createElement(tag){
      const node = el(tag);
      let html = "";
      Object.defineProperty(node, "innerHTML", {
        configurable: true,
        get(){ return html; },
        set(v){
          html = String(v);
          node.children.length = 0;
          if (node.id === "peekhost" && html){
            frames += 1;
            msgs.scrollHeight = 3000 + frames * step;
          }
        },
      });
      return node;
    },
    querySelector: () => null,
  };
}

function makePeekSkipFeature(){
  let paneText = "frame-one";
  const roots = makeRoots();
  roots.msgs.scrollHeight = 3000;
  roots.msgs.clientHeight = 600;
  roots.msgs.scrollTop = 2400;
  const nodes = [{
    id: "n1", title: "Alpha", agent: "claude", model: "sonnet",
    live: "quiet", attention: "approval", lane_id: "L1", description: "d",
  }];
  const feature = createChatFeature({
    roots,
    document: peekGrowingDocument(roots.msgs),
    window: {},
    CSS: { escape: s => String(s) },
    api: async (path) => {
      if (path.includes("/peek")) return paneText;
      if (path.includes("/chat")) return {
        turns: [
          { role: "user", text: "hello", time: "2026-01-01T00:00:00Z" },
          { role: "assistant", text: "hi", time: "2026-01-01T00:01:00Z" },
        ],
        live: "quiet", delivery: "ok", source: "tmux", attention: "approval",
        chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
      };
      return {};
    },
    nodes: () => nodes,
    sel: () => "n1",
    selGen: () => 1,
    nodeById: id => nodes.find(n => n.id === id),
    icons: {
      ICON_TERM: "T", ICON_CHECK: "✓", ICON_CHEV_UP: "↑", ICON_CHEV_DOWN: "↓",
      ICON_BRANCH: "B", ICON_INTO: "I", ICON_COPY: "C", ICON_DOWNALL: "↓↓", ICON_FILE: "F",
    },
    setComposerBusy: () => {},
    setComposerClosed: () => {},
    setAttachAvail: () => {},
    setBookmarksBackLabel: () => {},
    getPendingJump: () => null,
    setPendingJump: () => {},
    invalidateCardsSig: () => {},
    invalidateMapSig: () => {},
    renderCards: () => {},
    renderMap: () => {},
    isDesktop: () => false,
    now: () => 1_000_000,
    longpress: () => () => {},
    requestAnimationFrame: () => 0,
    ResizeObserver: class { constructor(){} observe(){} disconnect(){} },
  });
  return {
    feature, roots,
    setPane: (v) => { paneText = v; },
  };
}

test("P1 DOM 13: skip-path peek growth re-pins a reader who was at the bottom", async () => {
  const ctx = makePeekSkipFeature();
  await ctx.feature.render();
  // land exactly at the real max (not overscrolled) so a large growth pushes us off
  ctx.roots.msgs.scrollTop = ctx.roots.msgs.scrollHeight - ctx.roots.msgs.clientHeight;
  assert.ok(atBottom(ctx.roots.msgs), "reader is at the bottom before the skip tick");
  const htmlBefore = ctx.roots.msgs.innerHTML;
  const stBefore = ctx.roots.msgs.scrollTop;
  const shBefore = ctx.roots.msgs.scrollHeight;
  // only the peek capture moves — transcript signature is unchanged → skip path
  ctx.setPane("frame-two-much-longer-capture-while-agent-works");
  await ctx.feature.render();
  assert.equal(ctx.roots.msgs.innerHTML, htmlBefore,
    "skip path stays a skip: no transcript rebuild");
  assert.ok(ctx.roots.msgs.scrollHeight > shBefore + SCROLL_NEAR_BOTTOM_PX,
    "fixture must grow past the near-bottom slack");
  assert.ok(atBottom(ctx.roots.msgs),
    `peek growth must not push a bottom-reader off; was st=${stBefore} sh=${shBefore}, now st=${ctx.roots.msgs.scrollTop} sh=${ctx.roots.msgs.scrollHeight}`);
  ctx.feature.destroy();
});

test("P1 DOM 14: skip-path peek growth does not yank a reader who scrolled up", async () => {
  const ctx = makePeekSkipFeature();
  await ctx.feature.render();
  // deliberate scroll-up before the pane moves
  ctx.roots.msgs.scrollTop = 200;
  const held = ctx.roots.msgs.scrollTop;
  ctx.setPane("frame-two-much-longer-capture-while-agent-works");
  await ctx.feature.render();
  assert.equal(ctx.roots.msgs.scrollTop, held,
    "a reader who scrolled up is never pulled back down by peek re-pin");
  ctx.feature.destroy();
});

/* ---------- P3: caret at the end, never select-all (chat call sites) ---------- */

test("P3 6: description long-press places caret at end; never select-all", () => {
  let descPress = null;
  const timers = [];
  const ctx = makeFeature({
    deps: {
      setTimeout: (fn) => { timers.push(fn); return timers.length; },
      longpress: (_root, selector, fn) => {
        if (selector === "#chatdesc") descPress = fn;
        return () => {};
      },
    },
  });
  const input = ctx.roots.chatdescinput;
  input.value = "existing description text";
  input._focused = false;
  input._selected = false;
  input._range = null;
  input.focus = function(){ this._focused = true; };
  input.select = function(){ this._selected = true; };
  input.setSelectionRange = function(a, b){
    this.selectionStart = a; this.selectionEnd = b; this._range = [a, b];
  };

  ctx.feature.bind();
  ctx.feature.renderHead();
  assert.equal(typeof descPress, "function", "longpress on #chatdesc is bound");
  descPress();
  assert.equal(ctx.feature.isEditingDesc(), true);
  /* focus is deferred via setTimeout(0) */
  for (const fn of timers) fn();

  assert.equal(input._focused, true, "description input is focused");
  assert.equal(input._selected, false, "select() must not run — no select-all");
  assert.deepEqual(input._range, [input.value.length, input.value.length],
    "caret at end of existing content");
  assert.match(chatSrc, /from "\.\/caret\.js"/, "chat routes focus through caret.js");
  assert.match(chatSrc, /focusAtEnd\s*\(/, "onDescLongpress uses focusAtEnd");
  ctx.feature.destroy();
});

test("P3 7: use-as-description places caret at end via focusAtEnd", () => {
  /* Already caret-at-end by hand at red; the conversion is moving it onto the
     helper so there is one behaviour. Source pin is the load-bearing check —
     case 6 already covers focusAtEnd on this same #chatdescinput element
     behaviourally (focus + collapsed range + never select). */
  assert.match(chatSrc, /from "\.\/caret\.js"/);
  assert.doesNotMatch(chatSrc, /chatdescinput\.setSelectionRange\s*\(/,
    "hand-rolled setSelectionRange is retired in favour of focusAtEnd");
  const useFn = chatSrc.match(
    /function useTurnAsDescription\s*\([^)]*\)\s*\{[\s\S]*?\n  \}/,
  );
  assert.ok(useFn, "useTurnAsDescription still defined");
  assert.match(useFn[0], /focusAtEnd\s*\(\s*chatdescinput\s*\)/,
    "useTurnAsDescription routes focus through focusAtEnd");
  assert.doesNotMatch(useFn[0], /\.select\s*\(/);
  assert.doesNotMatch(useFn[0], /setSelectionRange\s*\(/);
  assert.match(useFn[0], /setTimeoutFn\s*\(/,
    "focus stays deferred so the editor is in the document first");
});

/* P2c — cleared Claude chat: server fresh:true yields freshSurface even when
   source is not "acp" and fallback would otherwise force a peek. */
test("chatActivityPolicy: server fresh trusts over source===acp and fallback", () => {
  // Cleared tmux/Claude node: empty turns, fallback/source=none, prior history,
  // server says fresh.
  const cleared = chatActivityPolicy({
    turnsLength: 0,
    source: "none",
    fallback: true,
    priorTurns: 5,
    fresh: true,
  });
  assert.equal(cleared.freshSurface, true, "server fresh must yield freshSurface for non-ACP");
  assert.equal(cleared.forcePeek, false, "fresh surface must not force peek");
  assert.equal(cleared.mustShowPane, false, "fallback alone must not force pane when fresh");

  // Same shape without server fresh: still peeks (cleared Claude today).
  const noFresh = chatActivityPolicy({
    turnsLength: 0,
    source: "none",
    fallback: true,
    priorTurns: 5,
    fresh: false,
  });
  assert.equal(noFresh.freshSurface, false);
  assert.equal(noFresh.forcePeek, true);

  // Attention still forces the pane even when fresh.
  const attn = chatActivityPolicy({
    turnsLength: 0,
    fresh: true,
    fallback: true,
    priorTurns: 3,
    attention: "approval",
  });
  assert.equal(attn.mustShowPane, true);
  assert.equal(attn.freshSurface, false);
});

/* ---------- P4: description read pane is height-bounded (scroll, not grow) ----------
 * #chathead is flex:none in the panel column. An unbounded .chatdesc pushes the
 * transcript off-screen. Reading flows one axis (same house rule as #msgs):
 * the container clips x; code blocks scroll internally. */

/** Extract a CSS rule body by bare-class selector (not descendants). */
function chatdescRuleBody(css){
  const m = css.match(/\.chatdesc\s*\{([^}]+)\}/);
  assert.ok(m, ".chatdesc bare rule present");
  return m[1];
}
function chatdescPreRuleBody(css){
  const m = css.match(/\.chatdesc\s+pre\s*\{([^}]+)\}/);
  assert.ok(m, ".chatdesc pre rule present");
  return m[1];
}
function chatdescboxRuleBody(css){
  const m = css.match(/\.chatdescbox\s*\{([^}]+)\}/);
  assert.ok(m, ".chatdescbox bare rule present");
  return m[1];
}

test("P4: .chatdesc is height-bounded and scrolls vertically", () => {
  /* Bare class only — .chatdesc p / .chatdesc pre must not satisfy this. */
  const body = chatdescRuleBody(chatCssSrc);
  assert.match(body, /max-height\s*:\s*[\d.]+dvh/,
    ".chatdesc must cap height in dvh so #chathead (flex:none) cannot grow unbound");
  assert.match(body, /overflow-y\s*:\s*auto/,
    "long descriptions scroll inside the cap, not by expanding the head");
  assert.match(body, /overscroll-behavior\s*:\s*contain/,
    "a flick inside the description must not chain-scroll the transcript underneath");
});

test("P4: long unbreakable content wraps instead of widening the head", () => {
  const body = chatdescRuleBody(chatCssSrc);
  assert.match(body, /overflow-wrap\s*:\s*(break-word|anywhere)/,
    "unbreakable tokens wrap; the head must not become a sideways scroller");
});

test("P4: code inside the description scrolls on its own axis", () => {
  const preBody = chatdescPreRuleBody(chatCssSrc);
  assert.match(preBody, /overflow-x\s*:\s*auto/,
    ".chatdesc pre scrolls long code lines internally");
  /* House rule from #msgs (chat.css): reading flows one axis; the container
     clips x and code blocks scroll internally. Absence of overflow-x:auto is
     not enough to state that: overflow-y:auto silently computes overflow-x to
     auto, so the clip must be declared or the cap re-creates the sideways
     scroller it was added to prevent. */
  const descBody = chatdescRuleBody(chatCssSrc);
  assert.doesNotMatch(descBody, /overflow-x\s*:\s*auto/,
    ".chatdesc itself must not be a horizontal scroller (same rule as #msgs)");
  assert.match(descBody, /overflow-x\s*:\s*clip/,
    "overflow-y:auto computes overflow-x to auto unless x is explicitly clipped");
});

test("P4: the editor textarea is unaffected", () => {
  const body = chatdescboxRuleBody(chatCssSrc);
  assert.match(body, /resize\s*:\s*vertical/,
    "editor keeps resize:vertical for free-form composition");
  assert.doesNotMatch(body, /max-height\s*:/,
    "editor must not inherit the reading cap — editing is free height");
});

/* ---------- P7: Markdown in the unfolded description ---------- */

test("P7: description read state is md() HTML; editor keeps raw; markup escaped", async () => {
  /* Same content must not be rich in one pane and flat in another. Notes
     already uses rendered-read / plain-edit; the chat description follows.
     md() is escape-first — no new sanitiser. */
  const { md } = await import("../js/format.js");
  const raw = "**hello** and <script>alert(1)</script>\n\nsecond paragraph";
  const ctx = makeFeature({
    nodes: [{
      id: "n1", title: "Alpha", agent: "claude", model: "sonnet",
      live: "quiet", attention: "", lane_id: "L1", description: raw,
    }],
  });
  ctx.feature.bind();
  ctx.feature.renderHead();

  const read = ctx.roots.chatdesc;
  assert.equal(read.innerHTML, md(raw),
    "read state is exactly md(description) into innerHTML");
  assert.match(read.innerHTML, /<strong>hello<\/strong>/, "markdown bold renders");
  assert.match(read.innerHTML, /&lt;script&gt;/, "raw markup is escaped by md()");
  assert.doesNotMatch(read.innerHTML, /<script>/i, "no injected script tags");
  assert.match(read.innerHTML, /<p>/, "multi-paragraph description is structured");

  // Editor path stays raw plain text (value), never the rendered HTML.
  assert.equal(ctx.roots.chatdescinput.value, raw,
    "editor receives the raw description text");
  assert.doesNotMatch(ctx.roots.chatdescinput.value, /<strong>/,
    "editor is not pre-filled with rendered HTML");

  // Empty description: still a readable placeholder (not md of empty → "").
  ctx.nodes[0].description = "";
  ctx.nodes[0].prompt = "";
  ctx.feature.renderHead();
  assert.match(ctx.roots.chatdesc.innerHTML || ctx.roots.chatdesc.textContent || "",
    /No description yet/, "empty description shows the placeholder");

  ctx.feature.destroy();
});

/* ---------- P1 security: structured permission request identity ---------- */

test("keyRowHTML: structured buttons carry data-request-id from permRequestId", () => {
  const html = keyRowHTML({
    attention: "approval", source: "acp",
    permTitle: "Edit",
    permOptions: [
      { key: "1", name: "Allow once", kind: "allow" },
      { key: "2", name: "Reject", kind: "reject" },
    ],
    permRequestId: "req-opaque-42",
  });
  assert.match(html, /data-request-id="req-opaque-42"/);
  assert.match(html, /data-key="1"/);
  assert.match(html, /data-key="2"/);
  // Every option button on the row carries the same request id.
  const ids = [...html.matchAll(/data-request-id="([^"]+)"/g)].map(m => m[1]);
  assert.equal(ids.length, 2);
  assert.ok(ids.every(id => id === "req-opaque-42"));
});

test("keyRowHTML: tmux keys do not invent data-request-id", () => {
  const html = keyRowHTML({ attention: "question" });
  assert.doesNotMatch(html, /data-request-id/);
  assert.match(html, /data-key="y"/);
});

test("structured permission click posts captured request_id with key", async () => {
  const nodes = [{
    id: "n1", title: "A", agent: "grok", model: "g", live: "quiet",
    attention: "approval", lane_id: "", description: "",
  }];
  const ctx = makeFeature({
    nodes,
    chatPayload: {
      turns: [{ role: "user", text: "q" }],
      live: "quiet", delivery: "ok", source: "acp",
      chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
      attention: "approval",
      perm_title: "go test",
      perm_options: [
        { key: "1", name: "Allow once", kind: "allow" },
        { key: "2", name: "Reject", kind: "reject" },
      ],
      perm_request_id: "req-click-7",
    },
  });
  ctx.feature.bind();
  await ctx.feature.render();
  // Rendered HTML includes the request id on each option button.
  assert.match(ctx.roots.keyrow.innerHTML, /data-request-id="req-click-7"/);
  assert.match(ctx.roots.keyrow.innerHTML, /data-key="1"/);

  const before = ctx.apiCalls.filter(c => c.path.includes("/key")).length;
  // Harness does not materialize buttons from HTML; click a synthetic button
  // that mirrors the rendered data-key + data-request-id attributes.
  const btn = el("button", { dataset: { key: "1", requestId: "req-click-7" } });
  firstListener(ctx.roots.keyrow, "click")({ target: btn });
  await new Promise(resolve => setImmediate(resolve));

  const keyCalls = ctx.apiCalls.filter(c => c.path.includes("/key"));
  assert.equal(keyCalls.length, before + 1, "exactly one /key request");
  const call = keyCalls[keyCalls.length - 1];
  assert.match(call.path, /\/api\/nodes\/n1\/key$/);
  const body = JSON.parse(call.opts?.body || "{}");
  assert.equal(body.key, "1");
  assert.equal(body.request_id, "req-click-7",
    "must post the request id rendered with the permission row");
  ctx.feature.destroy();
});

test("permission click captures node and request_id before async refresh", async () => {
  const nodes = [{
    id: "n1", title: "A", agent: "pi", model: "p", live: "quiet",
    attention: "approval", lane_id: "", description: "",
  }];
  let releaseKey;
  const keyGate = new Promise(r => { releaseKey = r; });
  const ctx = makeFeature({
    nodes,
    chatPayload: {
      turns: [{ role: "user", text: "q" }],
      live: "quiet", delivery: "ok", source: "acp",
      chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
      attention: "approval",
      perm_title: "rm -rf",
      perm_options: [{ key: "1", name: "Allow once", kind: "allow" }],
      perm_request_id: "req-A",
    },
    api: async (path, opts) => {
      if (path.includes("/key")) {
        await keyGate;
        return { ok: "sent" };
      }
      if (path.includes("/chat")) {
        return {
          turns: [{ role: "user", text: "q" }],
          live: "quiet", delivery: "ok", source: "acp",
          chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
          attention: "approval",
          perm_title: "rm -rf",
          perm_options: [{ key: "1", name: "Allow once", kind: "allow" }],
          perm_request_id: "req-A",
        };
      }
      return {};
    },
  });
  ctx.feature.bind();
  await ctx.feature.render();

  const btn = el("button", { dataset: { key: "1", requestId: "req-A" } });
  firstListener(ctx.roots.keyrow, "click")({ target: btn });
  // While the POST is held, change selection and invent a newer request id.
  ctx.setSel("n2");
  // Release the held /key — body must still target the click-time capture.
  releaseKey();
  await new Promise(resolve => setImmediate(resolve));
  await new Promise(resolve => setImmediate(resolve));

  const keyCalls = ctx.apiCalls.filter(c => c.path.includes("/key"));
  assert.ok(keyCalls.length >= 1, "expected a /key call");
  const body = JSON.parse(keyCalls[keyCalls.length - 1].opts?.body || "{}");
  assert.equal(body.request_id, "req-A",
    "must not retarget to a newer request id that appeared after click");
  assert.equal(body.key, "1");
  assert.match(keyCalls[keyCalls.length - 1].path, /\/api\/nodes\/n1\/key$/,
    "must target the node captured at click, not a later selection");
  ctx.feature.destroy();
});

test("buildChatSignature includes permRequestId so A→B rebuilds the row", () => {
  const base = {
    nodeId: "n1", attention: "approval", attentionHidden: false,
    delivery: "ok", showPeek: false, termOpen: false, termFull: false,
    source: "acp", permTitle: "go test",
    permOptionsKey: "1Allow onceallow,2Rejectreject",
    permRequestId: "inc:1",
    priorTurns: 0, chatStarted: "t", echoHash: "", histKey: "",
    turnsHash: "1", decisionsHash: "", expanded: false,
  };
  const a = buildChatSignature(base);
  const b = buildChatSignature({ ...base, permRequestId: "inc:2" });
  assert.notEqual(a, b,
    "identical title/options with different request ids must change the signature");
  assert.equal(buildChatSignature(base), a, "stable for same request id");
  assert.equal(
    buildChatSignature({ ...base, permRequestId: "" }),
    buildChatSignature({ ...base, permRequestId: undefined }),
    "missing and empty request id are equivalent",
  );
});

/* ---------- Loading indicators P1: the chat overlay spinner ----------
 * Reported from an iPhone: on a slow connection the chat dims and a spinner
 * appears, but it does not turn. Two independent causes, both fixed here:
 *   (a) Reduce Motion switched the animation off. Apple's own
 *       UIActivityIndicatorView / ProgressView keep spinning under Reduce
 *       Motion — that setting targets large-scale parallax and slide/zoom
 *       transitions, not a small in-place indeterminate indicator. HIG is
 *       explicit that a progress indicator which stops moving reads as
 *       stalled, which is the opposite of the message.
 *   (b) The spinner lived *inside* an element carrying backdrop-filter. On
 *       WebKit that box rasterizes as a unit and an animating descendant can
 *       stop repainting — the same layer-staleness family already documented
 *       for #msgs. The dim/blur is now its own sibling layer behind the
 *       indicator, so the indicator is never in a filtered subtree.
 */
const indexHtmlChat = readFileSync(join(__dirname, "../index.html"), "utf8");

function chatLoadingMarkup(html){
  const m = html.match(/<div id="chatloading"[^>]*>[\s\S]*?<\/div>\s*<\/div>/);
  assert.ok(m, "#chatloading element present in index.html");
  return m[0];
}

/* Brace-matched extraction: a naive non-greedy regex stops at the first inner
   "}" and would silently exclude the very declarations under test. */
function reducedMotionBlocks(css){
  const out = [];
  const re = /@media\s*\(prefers-reduced-motion:\s*reduce\)\s*\{/g;
  let m;
  while ((m = re.exec(css))){
    let depth = 1, i = re.lastIndex;
    while (i < css.length && depth > 0){
      if (css[i] === "{") depth++;
      else if (css[i] === "}") depth--;
      i++;
    }
    assert.equal(depth, 0, "unbalanced prefers-reduced-motion block");
    out.push(css.slice(m.index, i));
  }
  assert.ok(out.length, "the stylesheet must still have reduced-motion blocks");
  return out;
}

test("LI-A1: the chat spinner keeps animating under Reduce Motion", () => {
  const rule = chatCssSrc.match(/#chatloading\s+\.spin\s*\{([^}]+)\}/);
  assert.ok(rule, "#chatloading .spin rule present");
  assert.match(rule[1], /animation\s*:\s*spin\s/,
    "the indicator is driven by the shared spin keyframes");
  for (const block of reducedMotionBlocks(chatCssSrc)){
    assert.doesNotMatch(block, /#chatloading\s+\.spin/,
      "Reduce Motion must not stop the chat spinner: a frozen indicator reads " +
      "as stalled, and a small in-place indicator is not the motion class the " +
      "setting exists to suppress");
  }
});

test("LI-A2: no indeterminate progress indicator is frozen by Reduce Motion", () => {
  /* The same rule as A1, applied to every spinner idiom in the sheet: the
     upload chip's ring and the sheet CTA's dots. */
  for (const block of reducedMotionBlocks(chatCssSrc)){
    assert.doesNotMatch(block, /\.stagechip\s+\.spin/,
      "the upload chip's ring is an indeterminate indicator too");
  }
});

test("LI-A3: the blur/dim is a sibling scrim, never the spinner's ancestor", () => {
  const shell = chatCssSrc.match(/#chatloading\s*\{([^}]+)\}/);
  assert.ok(shell, "#chatloading rule present");
  assert.doesNotMatch(shell[1], /backdrop-filter/,
    "a backdrop-filter box rasterizes as a unit on WebKit; an animating " +
    "descendant can stop repainting inside it");
  assert.doesNotMatch(shell[1], /(^|[^-])filter\s*:/,
    "no filter on the ancestor of the indicator either");
  const scrim = chatCssSrc.match(/#chatloading\s+\.scrim\s*\{([^}]+)\}/);
  assert.ok(scrim, "#chatloading .scrim carries the dim + blur");
  assert.match(scrim[1], /-webkit-backdrop-filter\s*:\s*blur/, "keep the WebKit prefix");
  assert.match(scrim[1], /(^|[^-])backdrop-filter\s*:\s*blur/, "and the unprefixed property");
  assert.match(scrim[1], /position\s*:\s*absolute/, "the scrim covers the region on its own layer");

  const markup = chatLoadingMarkup(indexHtmlChat);
  const scrimAt = markup.indexOf('class="scrim"');
  const spinAt = markup.indexOf('class="spin"');
  assert.ok(scrimAt >= 0, "scrim element present");
  assert.ok(spinAt >= 0, "spin element present");
  assert.ok(scrimAt < spinAt, "scrim paints first, the indicator sits above it");
  assert.doesNotMatch(markup, /class="scrim"[^>]*>\s*<div class="spin"/,
    "the spinner must be the scrim's sibling, not its child");
});

test("LI-A4: the overlay announces itself as a live status region", () => {
  const markup = chatLoadingMarkup(indexHtmlChat);
  assert.match(markup, /role="status"/, "VoiceOver needs the status role");
  assert.match(markup, /aria-live="polite"/, "polite: it must not interrupt");
  assert.match(markup, /aria-label="loading chat"/);
});

test("LI-A5: the indicator's arc carries the contrast, not the track", () => {
  const rule = chatCssSrc.match(/#chatloading\s+\.spin\s*\{([^}]+)\}/);
  assert.match(rule[1], /border-top-color\s*:\s*var\(--work\)/,
    "the moving arc is a non-text UI component and takes the full accent");
});
