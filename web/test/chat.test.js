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
  ASSET_REF_RE,
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
  stripAssetRefs,
  splitPermTitle,
  bubbleActionsHTML,
  keyRowHTML,
  peekBlockHTML,
  echoBubbleHTML,
  createChatFeature,
} from "../js/chat.js";
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
  const b = buildChatSignature({ ...base, live: "active" });
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

test("bubbleActionsHTML order: when, fork, desc, bookmark, copy", () => {
  const html = bubbleActionsHTML({ time: "2026-07-24T09:05:00" }, {
    iconBranch: "B", iconInto: "I", iconCopy: "C",
  });
  assert.match(html, /class="bubwhen"/);
  assert.match(html, /data-bact="fork"/);
  assert.match(html, /data-bact="desc"/);
  assert.match(html, /data-bact="bookmark"/);
  assert.match(html, /data-bact="copy"/);
  assert.ok(html.indexOf("bubwhen") < html.indexOf('data-bact="fork"'));
  assert.ok(html.indexOf('data-bact="bookmark"') < html.indexOf('data-bact="copy"'));
  assert.doesNotMatch(html, /copybtn/);
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
  assert.match(img, /data:image\/png/);
  const file = assetTileHTML("n1", "id2", "doc", { name: "a.pdf", mime: "application/pdf" }, { iconFile: "F" });
  assert.match(file, /attfile/);
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
    innerHTML: attrs.innerHTML || "",
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
  return node;
}

/* keyrow harness: assign innerHTML → materialise .permask child so scrollTop
   snapshot/restore (and querySelector) work the same way as a real DOM. */
function makeKeyrow(){
  const node = el("div", { id: "keyrow" });
  let html = "";
  Object.defineProperty(node, "innerHTML", {
    configurable: true,
    get(){ return html; },
    set(v){
      html = String(v);
      node.children = [];
      const m = html.match(/class="(permask(?:\s+clamped)?)"/);
      if (!m) return;
      const mask = el("div", { className: m[1] });
      mask.scrollHeight = 1000;
      mask.clientHeight = 200;
      mask.scrollTop = 0;
      node.appendChild(mask);
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
  const api = async (path, opts) => {
    apiCalls.push({ path, opts });
    if (typeof overrides.api === "function") return overrides.api(path, opts, apiCalls);
    if (path.includes("/chat?history=1")) {
      return {
        segments: overrides.histSegs || [],
        assets: {},
      };
    }
    if (path.includes("/peek")) return overrides.peekText || "pane text";
    if (path.includes("/chat")) {
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
    return {};
  };
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
  assert.match(roots.msgs.innerHTML, /peekblock|Terminal/);
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
  assert.equal(roots.msgs.scrollTop, roots.msgs.scrollHeight);
});

test("chat rebuild pins a near-bottom reader but preserves a reader scrolled up", async () => {
  const near = makeFeature();
  near.roots.msgs.scrollHeight = 1000;
  near.roots.msgs.clientHeight = 200;
  near.roots.msgs.scrollTop = 750; // 50px from bottom, inside the 80px fence
  await near.feature.render();
  assert.equal(near.roots.msgs.scrollTop, 1000);

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
  assert.equal(roots.msgs.scrollTop, 2400);
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
