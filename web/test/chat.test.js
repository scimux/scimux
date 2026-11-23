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
  bubbleMarkersHTML,
  sentToMarkerModel,
  sharedBookmarkMarkerModel,
  keyRowHTML,
  peekBlockHTML,
  echoBubbleHTML,
  restoreDraftDecision,
  createChatFeature,
  elicitationStatusHTML,
  elicitationKey,
  isSafeElicitationURL,
  ELICITATION_OPEN_LABEL,
  hashTurnAttrs,
} from "../js/chat.js";
/* P6: ASSET_REF_RE + stripAssetRefs live in format.js (one home for every
   send-to strip). Mechanical import-path move from chat.js. */
import {
  ASSET_REF_RE,
  esc,
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
  assert.equal(DEC_LABELS.claude_unsupported, "Some chat features aren’t available.");
  assert.ok(RASTER_RE.test("photo.PNG"));
  assert.ok(ASSET_REF_RE.test("![x](scimux-asset:abc)"));
});

test("bookmark marker resolves durable turns and collapses duplicate section usages", () => {
  const turn = { uid: "u1", segment: 2, record: 7, time: "now", text: "answer" };
  const bookmark = { t: "b1", uid: "u1", segment: 2, record: 7, node: "lane-a", text: "answer" };
  const usages = [
    { note_id: "n1", note_title: "Research", section_id: "s1", section_title: "Findings", reference_id: "r1", source: { uid: "u1", segment: 2, record: 7 } },
    { note_id: "n1", note_title: "Research", section_id: "s1", section_title: "Findings", reference_id: "r2", source: { uid: "u1", segment: 2, record: 7 } },
    { note_id: "n1", note_title: "Research", section_id: "s2", section_title: "Risks", reference_id: "r3", source: { uid: "u1", segment: 2, record: 7 } },
  ];
  const model = chatmod.bookmarkMarkerModel(turn, "lane-a", [bookmark], usages);
  assert.equal(model.bookmark, bookmark);
  assert.deepEqual(model.destinations.map(x => x.section_id), ["s1", "s2"]);
});

test("bookmark marker supports legacy node/time/text bookmarks and note references", () => {
  const turn = { time: "2026-09-15T12:00:00Z", text: "legacy" };
  const bookmark = { t: "b1", node: "lane-a", turnTime: turn.time, text: turn.text };
  const usage = { note_id: "n1", section_id: "s1", reference_id: "r1", source: { node: "lane-a", turnTime: turn.time } };
  const other = { t: "nope", node: "lane-b", turnTime: turn.time, text: turn.text };
  const matched = chatmod.bookmarkMarkerModel(turn, "lane-a", [other, bookmark], [usage]);
  assert.equal(matched.bookmark, bookmark);
  assert.equal(matched.destinations.length, 1);
  assert.equal(matched.destinations[0], usage);
  const kept = chatmod.bookmarkMarkerModel(turn, "lane-a", [], [usage]);
  assert.ok(kept, "a legacy note reference keeps the marker after the bookmark is deleted");
  assert.equal(kept.bookmark, undefined);
  assert.equal(kept.destinations.length, 1);
  assert.equal(kept.destinations[0], usage);
  assert.equal(chatmod.bookmarkMarkerModel(turn, "lane-a", [], []), null);
  const wrongText = { t: "b2", node: "lane-a", turnTime: turn.time, text: "different" };
  assert.equal(chatmod.bookmarkMarkerModel(turn, "lane-a", [wrongText], []), null);
  const only = chatmod.bookmarkMarkerModel(turn, "lane-a", [bookmark], []);
  assert.equal(only.bookmark, bookmark);
  assert.deepEqual(only.destinations, []);
});

test("note references resolve without a bookmark and ignore unrelated addresses", () => {
  const turn = { uid: "u1", segment: 2, record: 7, time: "2026-10-01T00:00:00Z", text: "answer" };
  const durable = {
    note_id: "n1", section_id: "s1", reference_id: "durable",
    source: { uid: "u1", segment: 2, record: 7, node: "elsewhere", turnTime: "1999-01-01T00:00:00Z" },
  };
  const legacy = {
    note_id: "n2", section_id: "s2", reference_id: "legacy",
    source: { node: "lane-a", turnTime: turn.time },
  };
  const unrelated = [
    { note_id: "uid", section_id: "s", reference_id: "bad-uid", source: { uid: "other", segment: 2, record: 7, node: "lane-a", turnTime: turn.time } },
    { note_id: "seg", section_id: "s", reference_id: "bad-segment", source: { uid: "u1", segment: 9, record: 7, node: "lane-a", turnTime: turn.time } },
    { note_id: "rec", section_id: "s", reference_id: "bad-record", source: { uid: "u1", segment: 2, record: 8, node: "lane-a", turnTime: turn.time } },
    { note_id: "node", section_id: "s", reference_id: "bad-node", source: { node: "other", turnTime: turn.time } },
    { note_id: "time", section_id: "s", reference_id: "bad-time", source: { node: "lane-a", turnTime: "1999-01-01T00:00:00Z" } },
    null,
    {},
  ];
  const model = chatmod.bookmarkMarkerModel(turn, "lane-a", [], [durable, legacy, ...unrelated]);
  assert.ok(model, "durable and legacy note references survive bookmark deletion");
  assert.equal(model.bookmark, undefined);
  assert.deepEqual(model.destinations.map(d => d.reference_id), ["durable", "legacy"]);
  const bare = { text: "no clock" };
  const emptyTime = { note_id: "n0", section_id: "s0", reference_id: "empty-time", source: { node: "lane-a", turnTime: "" } };
  const stamped = { note_id: "nT", section_id: "sT", reference_id: "stamped", source: { node: "lane-a", turnTime: "2020-01-01T00:00:00Z" } };
  const durableOnBare = { note_id: "nD", section_id: "sD", reference_id: "has-uid", source: { uid: "u1", segment: 0, record: 0, node: "lane-a", turnTime: "" } };
  assert.deepEqual(
    chatmod.bookmarkMarkerModel(bare, "lane-a", [], [emptyTime, stamped, durableOnBare]).destinations.map(d => d.reference_id),
    ["empty-time"],
  );
});

test("duplicate section references collapse and distinct notes stay in order", () => {
  const turn = { uid: "u1", segment: 1, record: 1, time: "t", text: "x" };
  const source = { uid: "u1", segment: 1, record: 1 };
  const usages = [
    { note_id: "n1", note_title: "Alpha", section_id: "s1", section_title: "One", reference_id: "r1", source },
    { note_id: "n1", note_title: "Alpha", section_id: "s1", section_title: "One", reference_id: "r2", source },
    { note_id: "n2", note_title: "Beta", section_id: "s1", section_title: "One", reference_id: "r3", source },
    { note_id: "n1", note_title: "Alpha", section_id: "s2", section_title: "Two", reference_id: "r4", source },
  ];
  const model = chatmod.bookmarkMarkerModel(turn, "n", [], usages);
  assert.ok(model, "references alone are enough to build destinations");
  assert.deepEqual(model.destinations.map(d => d.reference_id), ["r1", "r3", "r4"]);
});

test("reference-only marker uses the generic bookmark icon", () => {
  const one = chatmod.bubbleMarkersHTML({ destinations: [{}] }, { iconBookmark: "BOOK" });
  const two = chatmod.bubbleMarkersHTML({ destinations: [{}, {}] }, { iconBookmark: "BOOK" });
  assert.match(one, /data-bmarker="bookmark"/);
  assert.match(one, />BOOK</);
  assert.match(one, /aria-label="Open bookmark destination"/);
  assert.doesNotMatch(one, /bmarkercount/);
  assert.doesNotMatch(one, /aria-haspopup/);
  assert.match(two, /aria-label="Open bookmark destination"/);
  assert.match(two, /aria-haspopup="menu"/);
  assert.match(two, /bmarkercount[^>]*>2</);
  const bookmarkOnly = chatmod.bubbleMarkersHTML({ bookmark: { t: "b1" } }, { iconBookmark: "BOOK" });
  assert.match(bookmarkOnly, /aria-label="Open bookmark"/);
  assert.match(bookmarkOnly, />BOOK</);
  assert.doesNotMatch(bookmarkOnly, /bmarkercount/);
  const withSend = chatmod.bubbleMarkersHTML({
    destinations: [{}], sentTo: [{ id: "l1" }],
  }, { iconBookmark: "BOOK", iconInto: "SEND" });
  assert.match(withSend, /data-bmarker="bookmark"/);
  assert.match(withSend, /data-bmarker="sendto"/);
  assert.match(withSend, />BOOK</);
  assert.match(withSend, /rot180[^>]*>SEND/);
});

test("bookmark marker HTML is compact and only counts multiple destinations", () => {
  const one = chatmod.bubbleMarkersHTML({ bookmark: { t: "b1" }, bookmarkDestinations: [{}] }, { iconBookmark: "BOOK" });
  const two = chatmod.bubbleMarkersHTML({ bookmark: { t: "b1" }, bookmarkDestinations: [{}, {}] }, { iconBookmark: "BOOK" });
  assert.match(one, /data-bmarker="bookmark"/);
  assert.match(one, /BOOK/);
  assert.doesNotMatch(one, /bmarkercount/);
  assert.match(two, /bmarkercount[^>]*>2</);
  assert.equal(chatmod.bubbleMarkersHTML({}, { iconBookmark: "BOOK" }), "");
});

test("Send-to marker follows the source turn and coexists with bookmark", () => {
  const turn = { uid: "u1", segment: 2, record: 7, time: "t", text: "source" };
  const links = [
    { id: "l1", source: { uid: "u1", segment: 2, record: 7 }, destination: { node: "d1", text: "edited" } },
    { id: "l2", source: { uid: "u1", segment: 2, record: 7 }, destination: { node: "d2", text: "other" } },
  ];
  assert.deepEqual(chatmod.sentToMarkerModel(turn, "source", links), links);
  const html = chatmod.bubbleMarkersHTML({
    bookmark: { t: "b1" }, bookmarkDestinations: [], sentTo: links,
  }, { iconBookmark: "BOOK", iconInto: "SEND" });
  assert.match(html, /data-bmarker="bookmark"/);
  assert.match(html, /data-bmarker="sendto"/);
  assert.match(html, /rot180[^>]*>SEND/);
  assert.match(html, /bmarkercount[^>]*>2/);
});

test("Send-to marker legacy match uses node and turn time", () => {
  const turn = { time: "t", text: "source" };
  const link = { id: "l1", source: { node: "source", turnTime: "t" }, destination: { node: "dest" } };
  assert.deepEqual(chatmod.sentToMarkerModel(turn, "source", [link]), [link]);
  assert.deepEqual(chatmod.sentToMarkerModel(turn, "other", [link]), []);
});

test("deferred new-chat Send-to commits on the matching transcript turn", async () => {
  const data = new Map([
    ["scimux-sendto-pending:n1", JSON.stringify([{ node: "source", turnTime: "old", text: "source" }])],
    ["scimux-sendto-awaiting:n1", JSON.stringify({ text: "edited prompt", afterTurns: 0, at: Date.now() })],
  ]);
  const storage = {
    getItem: key => data.has(key) ? data.get(key) : null,
    setItem: (key, value) => data.set(key, String(value)),
    removeItem: key => data.delete(key),
  };
  const ops = [];
  const ctx = makeFeature({
    storage,
    chatPayload: {
      turns: [{ role: "user", uid: "du", segment: 0, record: 1, time: "landed", text: "edited prompt" }],
      live: "quiet", delivery: "ok", source: "tmux", chat_started: "start", prior_turns: 0,
    },
    deps: { uiMutate: op => ops.push(op) },
  });
  await ctx.feature.render();
  assert.equal(ops.length, 1);
  assert.equal(ops[0].k, "forward-link-add");
  assert.equal(ops[0].link.destination.uid, "du");
  assert.equal(data.has("scimux-sendto-pending:n1"), false);
  assert.equal(data.has("scimux-sendto-awaiting:n1"), false);
});

test("Send-to confirmation ignores matching turns before its send boundary", () => {
  const oldTurn = { role: "user", time: "old", text: "same" };
  const newTurn = { role: "user", time: "new", text: "same" };
  assert.equal(chatmod.matchingForwardDestinationTurn(
    { text: "same", afterTurns: 1 }, [oldTurn, newTurn],
  ), newTurn);
});

/* The server records what it delivered, and an attachment makes that the
   prompt plus extendPrompt's appended reference. Equality alone would leave
   every attachment-bearing Send-to permanently unconfirmed. */
test("Send-to confirmation survives the attachment reference the server appends", () => {
  const turn = { role: "user", text: "carry this over\n\n[attached image: /tmp/a.png]" };
  assert.equal(chatmod.matchingForwardDestinationTurn({ text: "carry this over" }, [turn]), turn);
  assert.equal(chatmod.matchingForwardDestinationTurn({ text: "carry this" }, [turn]), null,
    "a bare prefix is a different prompt, not this latch's delivery");
});

test("truncated fallback addresses match a current turn by prefix", () => {
  assert.equal(chatmod.matchPendingJumpInTurns(
    { text: "forwarded prefix", textPrefix: true },
    [{ text: "other" }, { text: "forwarded prefix with the remaining body" }],
  ), 1);
});

test("chat marker invalidation does not stringify marker corpora", () => {
  assert.doesNotMatch(chatSrc, /markerHash:\s*hash\(JSON\.stringify/);
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

test("buildChatSignature changes for an inline transport error", () => {
  const base = {
    nodeId: "n1", attention: "", delivery: "", showPeek: false,
    termOpen: false, source: "acp", turnsHash: 1,
  };
  assert.notEqual(
    buildChatSignature(base),
    buildChatSignature({ ...base, error: "Usage balance exhausted" }),
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
  assert.match(file, /download="a\.pdf"/);
  const unicode = assetTileHTML("n1", "id3", "misleading.svg", { name: "résumé final.csv", mime: "image/svg+xml" }, { iconFile: "F" });
  assert.match(unicode, /download="résumé final\.csv"/);
  assert.doesNotMatch(unicode, /download="misleading\.svg"/);
  assert.match(img, /data-asset-preview/);
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
    permDialogId: "epoch-1",
  });
  assert.equal(perm.mustShowPane, true);
  assert.equal(perm.forcePeek, true);

  const approvalOnly = chatActivityPolicy({
    supervision: "claude_strict", attention: "approval", turnsLength: 2,
  });
  assert.equal(approvalOnly.mustShowPane, false,
    "attention alone is not a proven permission dialog");

  const armed = chatActivityPolicy({
    supervision: "claude_strict", attention: "approval", autoApproveArmed: true, turnsLength: 2,
    permDialogId: "epoch-1",
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

test("unsupported Claude guidance is short and neutral", () => {
  const html = pendingEmptyHTML({ supervision: "claude_unsupported" });
  assert.match(html, /older scimux setup/i);
  assert.doesNotMatch(html, /hook bundle|permission dialogs|auto-approve/i);
  assert.match(chatSrc,
    /data\.reason === "claude_unsupported" \? "chatnotice" : "chaterr"/);
  assert.match(chatCssSrc, /\.pending\.chatnotice/);
  assert.equal(pendingEmptyHTML({ supervision: "claude_unsupported", ended: true }), "",
    "Exited is already visible; an ended chat needs no compatibility notice");
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

test('a rejected approval from an earlier visit cannot alter a later visit', async () => {
  let reject;const alerts=[];
  const ctx=makeFeature({nodes:acpApprovalNodes(),chatPayload:acpApprovalPayload(),
    api:path=>path.endsWith('/key')?new Promise((_resolve,fail)=>{reject=fail;}):Promise.resolve({}),deps:{alert:message=>alerts.push(message)}});
  ctx.feature.bind();await ctx.feature.render();const key=el('button',{dataset:{key:'1',requestId:'old'}});
  firstListener(ctx.roots.keyrow,'click')({target:key});
  ctx.feature.onSelectChange();ctx.setSel('team/s/o/c/g');ctx.setSelGen(2);
  ctx.feature.onSelectChange();ctx.setSel('n1');ctx.setSelGen(3);await ctx.feature.render();
  const before=ctx.roots.keyrow.innerHTML;reject(Error('old approval error'));await new Promise(r=>setImmediate(r));
  assert.deepEqual(alerts,[]);assert.equal(ctx.roots.keyrow.innerHTML,before);ctx.feature.destroy();
});

test('a successful approval from an earlier visit does not schedule work for the new visit', async () => {
  let resolve,ticks=0;
  const ctx=makeFeature({nodes:acpApprovalNodes(),chatPayload:acpApprovalPayload(),api:path=>path.endsWith('/key')?new Promise(done=>{resolve=done;}):Promise.resolve({}),deps:{scheduleTick:()=>ticks++}});
  ctx.feature.bind();await ctx.feature.render();firstListener(ctx.roots.keyrow,'click')({target:el('button',{dataset:{key:'1'}})});
  ctx.feature.onSelectChange();ctx.setSel('team/s/o/c/g');ctx.setSelGen(2);resolve({});await new Promise(r=>setImmediate(r));
  assert.equal(ticks,0);ctx.feature.destroy();
});

test('an accepted approval schedules the fallback tick for its current visit', async () => {
  let ticks=0;const timers=[];
  const ctx=makeFeature({nodes:acpApprovalNodes(),chatPayload:acpApprovalPayload(),deps:{scheduleTick:null,tick:()=>ticks++,setTimeout:(fn,ms)=>{timers.push({fn,ms});return timers.length;},clearTimeout:()=>{}}});
  ctx.feature.bind();await ctx.feature.render();firstListener(ctx.roots.keyrow,'click')({target:el('button',{dataset:{key:'1'}})});
  await new Promise(r=>setImmediate(r));const retry=timers.find(timer=>timer.ms===400);assert.ok(retry);retry.fn();assert.equal(ticks,1);ctx.feature.destroy();
});

test('a delayed approval tick cannot run in a different visit', async () => {
  let ticks=0;const timers=[];
  const ctx=makeFeature({nodes:acpApprovalNodes(),chatPayload:acpApprovalPayload(),deps:{scheduleTick:null,tick:()=>ticks++,setTimeout:(fn,ms)=>{timers.push({fn,ms});return timers.length;},clearTimeout:()=>{}}});
  ctx.feature.bind();await ctx.feature.render();firstListener(ctx.roots.keyrow,'click')({target:el('button',{dataset:{key:'1'}})});
  await new Promise(r=>setImmediate(r));const retry=timers.find(timer=>timer.ms===400);assert.ok(retry);
  ctx.feature.onSelectChange();ctx.setSel('team/s/o/c/g');ctx.setSelGen(2);retry.fn();assert.equal(ticks,0);ctx.feature.destroy();
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

test("chatActivityPolicy: Claude question auto-opens only with a proven permission epoch", () => {
  const off = chatActivityPolicy({
    supervision: "claude_strict", attention: "question", turnsLength: 2,
    permDialogId: "epoch-q",
  });
  assert.equal(off.mustShowPane, true);
  const elicitationOnly = chatActivityPolicy({
    supervision: "claude_strict", attention: "question", turnsLength: 2,
  });
  assert.equal(elicitationOnly.mustShowPane, false,
    "elicitation-only question attention must not auto-open the terminal");
  const armed = chatActivityPolicy({
    supervision: "claude_strict", attention: "question", autoApproveArmed: true, turnsLength: 2,
    permDialogId: "epoch-q",
  });
  assert.equal(armed.mustShowPane, false);
  const both = chatActivityPolicy({
    supervision: "claude_strict", attention: "question", turnsLength: 2,
    permDialogId: "epoch-q",
  });
  assert.equal(both.mustShowPane, true,
    "a coexisting proven permission dialog may still force the terminal");
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

test("CSS: the capped keyrow scrolls, and nothing shrinks inside it", () => {
  // The cap alone was the bug: #keyrow stopped at 55dvh with overflow visible,
  // so the tail of the answer stack painted outside the box, under #promptbar's
  // opaque background (which also swallowed the taps) or clipped away by
  // html,body { overflow: clip }. Nothing was scrollable but the ask itself.
  const keyrow = chatCssSrc.match(/#keyrow\s*\{([^}]+)\}/);
  assert.ok(keyrow, "#keyrow rule present");
  assert.match(keyrow[1], /max-height:/, "#keyrow declares max-height (the keyrow cap)");
  assert.match(keyrow[1], /overflow-y:\s*auto/, "#keyrow is the scroller, so every option is reachable");

  // Shrink was the other half: flex shrinking runs before the scroller does,
  // so a shrinkable ask is crushed toward nothing while the row could have
  // scrolled instead. Reachability now comes from the scroller, not from
  // taking height off the question.
  const mask = chatCssSrc.match(/\.permask\s*\{([^}]+)\}/);
  assert.ok(mask, ".permask rule present");
  assert.match(mask[1], /flex:\s*0\s*0\s*auto/, ".permask does not shrink (the ask keeps its lines)");
  assert.doesNotMatch(mask[1], /min-height:\s*0/, "no shrink floor to declare");
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
    classList: {
      _s: new Set((attrs.className || "").split(/\s+/).filter(Boolean)),
      toggle(c, on){
        if (on === undefined) {
          if (this._s.has(c)) this._s.delete(c); else this._s.add(c);
        } else if (on) this._s.add(c); else this._s.delete(c);
        _cn = [...this._s].join(" ");
      },
      contains(c){ return this._s.has(c); },
      add(c){ this._s.add(c); _cn = [...this._s].join(" "); },
      remove(c){ this._s.delete(c); _cn = [...this._s].join(" "); },
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
      /* Compound ".turn[data-bk=…]" must not treat the attribute as part of the
         class token — otherwise bubble-action taps never find the turn node. */
      const bkCombo = sel.match(/^\.([^.\s\[]+)\[data-bk="([^"]+)"\]$/);
      if (bkCombo) {
        const walk = n => {
          if (n.classList?.contains(bkCombo[1]) && n.dataset?.bk === bkCombo[2]) return n;
          for (const c of n.children || []) {
            const f = walk(c);
            if (f) return f;
          }
          return null;
        };
        return walk(this);
      }
      if (sel.startsWith(".")) {
        const cls = sel.slice(1).split(/[\s.>\[\]]/)[0];
        if (this.classList.contains(cls)) return this;
        for (const c of this.children) {
          const f = c.querySelector?.(sel);
          if (f) return f;
        }
        return null;
      }
      // attribute selectors like [data-bk="i:0"]
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
      if (sel.includes("[data-fold]") && this.dataset?.fold) return this;
      if (sel.includes("[data-bmarker]") && this.dataset?.bmarker) return this;
      if (sel.includes("[data-dismiss-attention]") && this.dataset?.dismissAttention) return this;
      if (sel.includes("[data-open-terminal]") && this.dataset?.openTerminal) return this;
      if (sel.includes("[data-asset-preview]") && this.dataset?.assetPreview) return this;
      if (sel.includes("[data-asset-settings]") && this.dataset?.assetSettings) return this;
      if (sel.includes("[data-asset-retry]") && this.dataset?.assetRetry) return this;
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
  /* Keep className ↔ classList in sync. Production often assigns className
     (e.g. bubactions rows); without this, classList.contains stays stale and
     bubble-action taps clear tappedTurn. */
  let _cn = attrs.className || "";
  Object.defineProperty(node, "className", {
    configurable: true,
    get(){ return _cn; },
    set(v){
      _cn = String(v);
      node.classList._s = new Set(_cn.split(/\s+/).filter(Boolean));
    },
  });
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
    chatfoldall: el("button", { id: "chatfoldall", className: "foldall", hidden: true }),
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
	muse_schema_warn: el("div", { id: "muse_schema_warn", className: "nc-hint", hidden: true }),
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
  const roots = overrides.roots || makeRoots();
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
    assetURL: path => path,
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

test("delegated attachment actions open preview/settings and retry without sending a turn", async () => {
  const previews = [], settings = [], alerts = [];
  let failRetry = false;
  const { feature, roots, apiCalls } = makeFeature({
    api: async (path, opts) => {
      if (path.includes("asset-imports/retry")) {
        const turn = JSON.parse(opts.body).turn_record;
        if (failRetry) throw turn === 8 ? new Error("still blocked") : "blocked string";
        return { status: turn === 7 ? "already_imported" : "imported" };
      }
      if (path.includes("/chat")) return defaultChatPayload({});
      return {};
    },
    deps: {
      openAssetPreview: value => previews.push(value),
      openAttachmentSettings: () => settings.push(true),
      alert: value => alerts.push(value),
    },
  });
  feature.bind();
  const click = target => roots.msgs._listeners.get("click")[0].fn({ target, preventDefault(){} });

  const image = el("a", { dataset: { assetPreview: "1", name: "photo.png" } });
  image.href = "blob:photo";
  click(image);
  assert.deepEqual(previews, [{ url: "blob:photo", name: "photo.png" }]);
  const titled = el("a", { dataset: { assetPreview: "1" } });
  titled.href = "blob:titled";
  titled.title = "title.png";
  click(titled);
  const unnamed = el("a", { dataset: { assetPreview: "1" } });
  unnamed.href = "blob:unnamed";
  click(unnamed);
  assert.deepEqual(previews.slice(1), [
    { url: "blob:titled", name: "title.png" },
    { url: "blob:unnamed", name: "image" },
  ]);

  click(el("button", { dataset: { assetSettings: "1" } }));
  assert.equal(settings.length, 1);

  const retry = el("button", { dataset: { assetRetry: "1", node: "n1", turn: "7", occurrence: "2" } });
  click(retry);
  for (let i = 0; i < 12; i++) await Promise.resolve();
  const call = apiCalls.find(c => c.path.includes("asset-imports/retry"));
  assert.deepEqual(JSON.parse(call.opts.body), { turn_record: 7, occurrence: 2 });
  assert.equal(retry.textContent, "Already imported");

  const imported = el("button", { dataset: { assetRetry: "1", node: "", turn: "9", occurrence: "0" } });
  click(imported);
  for (let i = 0; i < 12; i++) await Promise.resolve();
  assert.equal(imported.textContent, "Imported on retry");

  failRetry = true;
  const failed = el("button", { dataset: { assetRetry: "1", node: "n1", turn: "8", occurrence: "0" } });
  click(failed);
  for (let i = 0; i < 12; i++) await Promise.resolve();
  assert.equal(failed.disabled, false);
  assert.equal(failed.textContent, "Retry import");
  assert.deepEqual(alerts, ["still blocked"]);
  const failedString = el("button", { dataset: { assetRetry: "1", node: "n1", turn: "10", occurrence: "0" } });
  click(failedString);
  for (let i = 0; i < 12; i++) await Promise.resolve();
  assert.deepEqual(alerts, ["still blocked", "blocked string"]);
  assert.equal(apiCalls.some(c => c.path.endsWith("/send")), false);
  feature.destroy();

  const quiet = makeFeature({ deps: { openAssetPreview: null, openAttachmentSettings: null } });
  quiet.feature.bind();
  const quietClick = target => quiet.roots.msgs._listeners.get("click")[0].fn({ target, preventDefault(){} });
  quietClick(image);
  quietClick(el("button", { dataset: { assetSettings: "1" } }));
  quiet.feature.destroy();
});

test("external-attachment setting toggle rebuilds an existing blocked card", async () => {
  let externalAllowed = false;
  const blockedTurn = {
    role: "assistant",
    text: "![report](scimux-import:7:0:outside_workspace)",
    time: "2026-01-01T00:01:00Z",
  };
  const ctx = makeFeature({
    api: async path => {
      if (path.includes("/chat")) return defaultChatPayload({
        chatPayload: {
          turns: [blockedTurn], live: "quiet", delivery: "ok", source: "tmux",
          chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
          allow_external_attachments: externalAllowed,
        },
      });
      return {};
    },
  });

  await ctx.feature.render();
  assert.match(ctx.roots.msgs.innerHTML, /Open attachment settings/);
  assert.doesNotMatch(ctx.roots.msgs.innerHTML, /Retry import/);

  externalAllowed = true;
  await ctx.feature.render();
  assert.match(ctx.roots.msgs.innerHTML, /Retry import/,
    "a settings-only response change must rebuild the blocked card");
  assert.doesNotMatch(ctx.roots.msgs.innerHTML, /Open attachment settings/);
  ctx.feature.destroy();
});

test("successful retry refreshes loaded history without moving the reader", async () => {
  let imported = false;
  let historyReads = 0;
  const ctx = makeFeature({
    api: async (path) => {
      if (path.includes("asset-imports/retry")) {
        imported = true;
        return { status: "imported", asset_id: "a_report", name: "report.txt" };
      }
      if (path.includes("/chat?history=1")) {
        historyReads++;
        return {
          segments: [{
            start: "2025-12-31T00:00:00Z", seam: "2025-12-31T00:00:00Z",
            reason: "clear",
            turns: [{
              role: "assistant", time: "2025-12-31T00:01:00Z",
              text: imported
                ? "[report](scimux-asset:a_report)"
                : "[report](scimux-import:0:0:unreadable)",
            }],
          }],
          assets: imported ? { a_report: { name: "report.txt" } } : {},
        };
      }
      if (path.includes("/chat")) return {
        turns: [{ role: "assistant", text: "current", time: "2026-01-01T00:01:00Z" }],
        live: "quiet", delivery: "ok", source: "tmux",
        chat_started: "2026-01-01T00:00:00Z", prior_turns: 1, assets: {},
        allow_external_attachments: true,
      };
      return {};
    },
  });
  ctx.feature.bind();
  await ctx.feature.loadHistory("n1", "");
  assert.match(ctx.roots.msgs.innerHTML, /Retry import/);
  ctx.roots.msgs.scrollHeight = 2400;
  ctx.roots.msgs.clientHeight = 400;
  ctx.roots.msgs.scrollTop = 275;

  const retry = el("button", {
    dataset: { assetRetry: "1", node: "n1", turn: "0", occurrence: "0" },
  });
  firstListener(ctx.roots.msgs, "click")({ target: retry });
  for (let i = 0; i < 16; i++) await Promise.resolve();

  assert.equal(historyReads, 2, "retry must re-fetch the loaded historical segments");
  assert.match(ctx.roots.msgs.innerHTML, /report\.txt/);
  assert.doesNotMatch(ctx.roots.msgs.innerHTML, /Retry import/);
  assert.equal(ctx.roots.msgs.scrollTop, 275,
    "refreshing an earlier segment must preserve the reader's position");
  ctx.feature.destroy();
});

test("missing unimported files render as inert text in the live chat and earlier history", async () => {
  const ctx = makeFeature({
    api: async path => {
      if (path.includes("/chat?history=1")) {
        return {
          segments: [{
            start: "2025-12-31T00:00:00Z",
            seam: "2025-12-31T00:00:00Z",
            reason: "clear",
            turns: [{
              role: "assistant",
              time: "2025-12-31T00:01:00Z",
              text: 'old [<hist>.md](scimux-import:1:0:not_found)',
            }],
          }],
          assets: {},
          allow_external_attachments: true,
        };
      }
      if (path.includes("/chat")) {
        return {
          turns: [{
            role: "assistant",
            text: 'live [<now>.txt](scimux-import:4:0:not_found)',
            time: "2026-01-01T00:01:00Z",
          }],
          live: "quiet", delivery: "ok", source: "tmux",
          chat_started: "2026-01-01T00:00:00Z", prior_turns: 1, assets: {},
          allow_external_attachments: true,
        };
      }
      return {};
    },
  });
  ctx.feature.bind();
  await ctx.feature.loadHistory("n1", "");
  const html = ctx.roots.msgs.innerHTML;
  assert.match(html, /&lt;hist&gt;\.md/);
  assert.match(html, /&lt;now&gt;\.txt/);
  assert.match(html, /old <span class="missingfile">&lt;hist&gt;\.md<\/span>/);
  assert.match(html, /live <span class="missingfile">&lt;now&gt;\.txt<\/span>/);
  assert.doesNotMatch(html, /class="turn[^"]*\bmedia\b/);
  for (const markup of [
    "Retry import", "data-asset-retry", "data-asset-settings", "data-asset-preview",
    "attrow", "attfile", "attthumb", "download=", "Open attachment settings",
  ]) {
    assert.equal(html.includes(markup), false, markup);
  }
  ctx.feature.destroy();
});

function retryFailureError(reason = "not_found") {
  const err = new Error(JSON.stringify({
    status: "failed", reason, message: reason === "too_large" ? "File exceeds the import limit." : "File no longer exists.",
  }) + "\n");
  err.status = 422;
  return err;
}

for (const reason of ["not_found", "too_large"]) {
  test(`a ${reason} retry drops the stale button and refreshes history without moving the reader`, async () => {
    let historyReads = 0;
    const alerts = [];
    const order = [];
    const ctx = makeFeature({
      api: async path => {
        if (path.includes("asset-imports/retry")) {
          order.push("retry");
          throw retryFailureError(reason);
        }
        if (path.includes("/chat?history=1")) {
          historyReads++;
          order.push("history");
          return {
            segments: [{
              start: "2025-12-31T00:00:00Z",
              seam: "2025-12-31T00:00:00Z",
              reason: "clear",
              turns: [{
                role: "assistant",
                time: "2025-12-31T00:01:00Z",
                text: historyReads === 1
                  ? "[report.md](scimux-import:0:0:retry_ready)"
                  : `[report.md](scimux-import:0:0:${reason})`,
              }],
            }],
            assets: {},
            allow_external_attachments: true,
          };
        }
        if (path.includes("/chat")) {
          return {
            turns: [{ role: "assistant", text: "current chat", time: "2026-01-01T00:01:00Z" }],
            live: "quiet", delivery: "ok", source: "tmux",
            chat_started: "2026-01-01T00:00:00Z", prior_turns: 1, assets: {},
            allow_external_attachments: true,
          };
        }
        return {};
      },
      deps: { alert: value => alerts.push(value) },
    });
    ctx.feature.bind();
    await ctx.feature.loadHistory("n1", "");
    assert.match(ctx.roots.msgs.innerHTML, /Retry import/);
    ctx.roots.msgs.scrollHeight = 2400;
    ctx.roots.msgs.clientHeight = 400;
    ctx.roots.msgs.scrollTop = 275;
    const retry = el("button", {
      dataset: { assetRetry: "1", node: "n1", turn: "0", occurrence: "0" },
    });
    const host = el("span");
    host.appendChild(retry);
    retry.remove = () => {
      order.push("remove");
      if (retry.parentNode) {
        const i = retry.parentNode.children.indexOf(retry);
        if (i >= 0) retry.parentNode.children.splice(i, 1);
        retry.parentNode = null;
      }
    };
    firstListener(ctx.roots.msgs, "click")({ target: retry });
    for (let i = 0; i < 20; i++) await Promise.resolve();
    assert.equal(retry.parentNode, null);
    assert.ok(order.indexOf("remove") >= 0 && order.indexOf("remove") < order.lastIndexOf("history"));
    assert.equal(historyReads, 2);
    assert.match(ctx.roots.msgs.innerHTML, /report\.md/);
    assert.doesNotMatch(ctx.roots.msgs.innerHTML, /Retry import/);
    assert.equal(ctx.roots.msgs.scrollTop, 275);
    assert.match(ctx.roots.msgs.innerHTML, /current chat/);
    assert.deepEqual(alerts, []);
    ctx.feature.destroy();
  });

  test(`a ${reason} retry refreshes the live chat and leaves the selection in place`, async () => {
    let liveReads = 0;
    const ctx = makeFeature({
      api: async path => {
        if (path.includes("asset-imports/retry")) throw retryFailureError(reason);
        if (path.includes("/chat")) {
          liveReads++;
          return {
            turns: [{
              role: "assistant",
              text: liveReads === 1
                ? "[notes.md](scimux-import:2:0:retry_ready)"
                : `[notes.md](scimux-import:2:0:${reason})`,
              time: "2026-01-01T00:01:00Z",
            }],
            live: "quiet", delivery: "ok", source: "tmux",
            chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
            allow_external_attachments: true,
          };
        }
        return {};
      },
      deps: { alert: () => { throw new Error(`live ${reason} retry alerted`); } },
    });
    ctx.feature.bind();
    await ctx.feature.render();
    assert.match(ctx.roots.msgs.innerHTML, /Retry import/);
    const retry = el("button", {
      dataset: { assetRetry: "1", node: "n1", turn: "2", occurrence: "0" },
    });
    let removed = false;
    retry.remove = () => { removed = true; };
    firstListener(ctx.roots.msgs, "click")({ target: retry });
    for (let i = 0; i < 20; i++) await Promise.resolve();
    assert.equal(removed, true);
    assert.equal(liveReads, 2);
    assert.match(ctx.roots.msgs.innerHTML, /notes\.md/);
    assert.doesNotMatch(ctx.roots.msgs.innerHTML, /Retry import|data-asset-retry/);
    if (reason === "not_found") assert.doesNotMatch(ctx.roots.msgs.innerHTML, /attrow/);
    else assert.match(ctx.roots.msgs.innerHTML, /File exceeds the import limit/);
    assert.equal(ctx.roots.chattitle.textContent, "Alpha");
    ctx.feature.destroy();
  });

}

test("other retry failures keep the button and do not refresh the chat", async () => {
  let historyReads = 0;
  const alerts = [];
  assert.equal(chatmod.readRetryFailure(null), "");
  assert.equal(chatmod.readRetryFailure(undefined), "");
  const failures = [
    (() => {
      const err = new Error(JSON.stringify({ status: "failed", reason: "unreadable", message: "File cannot be read." }));
      err.status = 422;
      return err;
    })(),
    (() => {
      const err = new Error("not json");
      err.status = 422;
      return err;
    })(),
    (() => {
      const err = new Error("null");
      err.status = 422;
      return err;
    })(),
    (() => {
      const err = new Error(JSON.stringify({ status: "failed", reason: 7, message: "no" }));
      err.status = 422;
      return err;
    })(),
    (() => {
      const err = new Error(JSON.stringify({ status: "failed", reason: "not_found", message: "File no longer exists." }));
      err.status = 409;
      return err;
    })(),
  ];
  const ctx = makeFeature({
    api: async path => {
      if (path.includes("asset-imports/retry")) throw failures.shift();
      if (path.includes("/chat?history=1")) {
        historyReads++;
        return {
          segments: [{
            start: "2025-12-31T00:00:00Z",
            turns: [{ role: "assistant", text: "[report.md](scimux-import:0:0:unreadable)" }],
          }],
          assets: {},
        };
      }
      if (path.includes("/chat")) {
        return {
          turns: [{ role: "assistant", text: "current", time: "2026-01-01T00:01:00Z" }],
          live: "quiet", delivery: "ok", source: "tmux",
          chat_started: "2026-01-01T00:00:00Z", prior_turns: 1, assets: {},
        };
      }
      return {};
    },
    deps: { alert: value => alerts.push(value) },
  });
  ctx.feature.bind();
  await ctx.feature.loadHistory("n1", "");
  const clickRetry = () => {
    const retry = el("button", {
      dataset: { assetRetry: "1", node: "n1", turn: "0", occurrence: "0" },
      textContent: "Retry import",
    });
    retry.removed = false;
    retry.remove = () => { retry.removed = true; };
    firstListener(ctx.roots.msgs, "click")({ target: retry });
    return retry;
  };
  for (let n = 0; n < 5; n++) {
    const retry = clickRetry();
    for (let i = 0; i < 16; i++) await Promise.resolve();
    assert.equal(retry.disabled, false);
    assert.equal(retry.textContent, "Retry import");
    assert.equal(retry.removed, false);
  }
  assert.equal(historyReads, 1);
  assert.deepEqual(alerts, [
    JSON.stringify({ status: "failed", reason: "unreadable", message: "File cannot be read." }),
    "not json",
    "null",
    JSON.stringify({ status: "failed", reason: 7, message: "no" }),
    JSON.stringify({ status: "failed", reason: "not_found", message: "File no longer exists." }),
  ]);
  ctx.feature.destroy();
});

test("a not_found retry completion cannot repaint a newly selected chat", async () => {
  let finishRetry;
  let historyReads = 0;
  const retryDone = new Promise((_, reject) => { finishRetry = () => reject(retryFailureError()); });
  const ctx = makeFeature({
    nodes: [
      { id: "n1", title: "One", agent: "claude", live: "quiet", attention: "" },
      { id: "n2", title: "Two", agent: "claude", live: "quiet", attention: "" },
    ],
    api: async path => {
      if (path.includes("asset-imports/retry")) return retryDone;
      if (path.includes("/chat?history=1")) {
        historyReads++;
        return {
          segments: [{
            start: "2025-12-31T00:00:00Z",
            turns: [{ role: "assistant", text: "[old](scimux-import:0:0:unreadable)" }],
          }],
          assets: {},
        };
      }
      if (path.includes("/api/nodes/n2/chat")) return {
        turns: [{ role: "assistant", text: "selected chat two" }],
        live: "quiet", delivery: "ok", source: "tmux",
        chat_started: "2026-01-02T00:00:00Z", prior_turns: 0, assets: {},
      };
      if (path.includes("/chat")) return {
        turns: [{ role: "assistant", text: "chat one" }],
        live: "quiet", delivery: "ok", source: "tmux",
        chat_started: "2026-01-01T00:00:00Z", prior_turns: 1, assets: {},
      };
      return {};
    },
  });
  ctx.feature.bind();
  await ctx.feature.loadHistory("n1", "");
  const retry = el("button", {
    dataset: { assetRetry: "1", node: "n1", turn: "0", occurrence: "0" },
  });
  let removed = false;
  retry.remove = () => { removed = true; };
  firstListener(ctx.roots.msgs, "click")({ target: retry });
  ctx.setSel("n2");
  ctx.setSelGen(2);
  ctx.feature.onSelectChange();
  await ctx.feature.render();
  assert.match(ctx.roots.msgs.innerHTML, /selected chat two/);
  finishRetry();
  for (let i = 0; i < 16; i++) await Promise.resolve();
  assert.equal(removed, true);
  assert.equal(historyReads, 1);
  assert.match(ctx.roots.msgs.innerHTML, /selected chat two/);
  assert.doesNotMatch(ctx.roots.msgs.innerHTML, /old/);
  ctx.feature.destroy();
});

test("historical retry completion cannot repaint a newly selected chat", async () => {
  let finishRetry;
  let historyReads = 0;
  const retryDone = new Promise(resolve => { finishRetry = resolve; });
  const ctx = makeFeature({
    nodes: [
      { id: "n1", title: "One", agent: "claude", live: "quiet", attention: "" },
      { id: "n2", title: "Two", agent: "claude", live: "quiet", attention: "" },
    ],
    api: async path => {
      if (path.includes("asset-imports/retry")) return retryDone;
      if (path.includes("/chat?history=1")) {
        historyReads++;
        return {
          segments: [{
            start: "2025-12-31T00:00:00Z",
            turns: [{ role: "assistant", text: "[old](scimux-import:0:0:not_found)" }],
          }],
          assets: {},
        };
      }
      if (path.includes("/api/nodes/n2/chat")) return {
        turns: [{ role: "assistant", text: "selected chat two" }],
        live: "quiet", delivery: "ok", source: "tmux",
        chat_started: "2026-01-02T00:00:00Z", prior_turns: 0, assets: {},
      };
      if (path.includes("/chat")) return {
        turns: [{ role: "assistant", text: "chat one" }],
        live: "quiet", delivery: "ok", source: "tmux",
        chat_started: "2026-01-01T00:00:00Z", prior_turns: 1, assets: {},
      };
      return {};
    },
  });
  ctx.feature.bind();
  await ctx.feature.loadHistory("n1", "");
  const retry = el("button", {
    dataset: { assetRetry: "1", node: "n1", turn: "0", occurrence: "0" },
  });
  firstListener(ctx.roots.msgs, "click")({ target: retry });

  ctx.setSel("n2");
  ctx.setSelGen(2);
  ctx.feature.onSelectChange();
  await ctx.feature.render();
  assert.match(ctx.roots.msgs.innerHTML, /selected chat two/);

  finishRetry({ status: "imported", asset_id: "a_old", name: "old.txt" });
  for (let i = 0; i < 16; i++) await Promise.resolve();
  assert.equal(historyReads, 1, "stale retry must not load history into the new selection");
  assert.match(ctx.roots.msgs.innerHTML, /selected chat two/);
  assert.doesNotMatch(ctx.roots.msgs.innerHTML, /old\.txt/);
  ctx.feature.destroy();
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

test("switching from active local chat to received chat clears work pulse", async () => {
  const ctx = makeFeature({
    chatPayload: {
      turns: [{ role: "user", text: "synthetic" }],
      live: "active", delivery: "ok", source: "tmux",
      chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
    },
  });
  try {
    await ctx.feature.render();
    assert.equal(ctx.roots.workpulse.classList.contains("on"), true);
    // Match app.select: retire the old selection before selecting received;
    // received rendering does not call the native chat renderer.
    ctx.feature.onSelectChange();
    ctx.setSel("team/s/o/c/g");
    ctx.setSelGen(2);
    assert.equal(ctx.roots.workpulse.classList.contains("on"), false,
      "received chat must not inherit local activity");
  } finally {
    ctx.feature.destroy();
  }
});

test("history from an earlier visit cannot refresh the same node after A-B-A", async () => {
  let resolveHistory;
  const ctx = makeFeature({api:(path)=> path.includes('history=1')
    ? new Promise(resolve=>{resolveHistory=resolve;}) : Promise.resolve({turns:[],assets:{}})});
  const pending = ctx.feature.loadHistory('n1','');
  ctx.feature.onSelectChange(); ctx.setSel('team/s/o/c/g'); ctx.setSelGen(2);
  ctx.feature.onSelectChange(); ctx.setSel('n1'); ctx.setSelGen(3);
  resolveHistory({segments:[],assets:{}}); await pending;
  assert.equal(ctx.apiCalls.filter(call=>call.path.includes('/chat') && !call.path.includes('history=1')).length,0);
  ctx.feature.destroy();
});

test('history errors are shown only for the visit that requested them', async () => {
  let reject;const alerts=[];
  const ctx=makeFeature({api:()=>new Promise((_resolve,fail)=>{reject=fail;}),deps:{alert:message=>alerts.push(message)}});
  let pending=ctx.feature.loadHistory('n1','');reject(Error('current'));await pending;assert.deepEqual(alerts,['current']);
  pending=ctx.feature.loadHistory('n1','');ctx.feature.onSelectChange();ctx.setSel('team/s/o/c/g');ctx.setSelGen(2);
  ctx.feature.onSelectChange();ctx.setSel('n1');ctx.setSelGen(3);reject(Error('old'));await pending;
  assert.deepEqual(alerts,['current']);ctx.feature.destroy();
});

test("retiring a local selection clears its schema warning before a received chat opens", async () => {
  const ctx=makeFeature({nodes:[{id:'n1',agent:'muse',muse_schema_warning:'muse_schema_fingerprint_mismatch'}]});
  await ctx.feature.render(); assert.equal(ctx.roots.muse_schema_warn.hidden,false);
  ctx.feature.onSelectChange(); ctx.setSel('team/s/o/c/g'); ctx.setSelGen(2);
  assert.equal(ctx.roots.muse_schema_warn.hidden,true);
  ctx.feature.destroy();
});

test('cold loading and stale A response cannot repaint after A-received-A or no selection', async () => {
  let release;const timers=[];let calls=0;
  const ctx=makeFeature({chatConditional:()=>++calls===1?new Promise(resolve=>{release=resolve;}):({status:200,etag:'new',data:{...defaultChatPayload(),live:'quiet'}}),
    deps:{setTimeout:fn=>{timers.push(fn);return timers.length;},clearTimeout:()=>{}}});
  const old=ctx.feature.render();timers[0]();assert.equal(ctx.roots.chatloading.hidden,false);
  ctx.feature.onSelectChange();ctx.setSel('team/s/o/c/g');ctx.setSelGen(2);
  assert.equal(ctx.roots.chatloading.hidden,true);assert.equal(ctx.roots.convtools.hidden,true);timers[0]();assert.equal(ctx.roots.chatloading.hidden,true);
  ctx.feature.onSelectChange();ctx.setSel('n1');ctx.setSelGen(3);await ctx.feature.render();
  release({status:200,etag:'old',data:{...defaultChatPayload(),live:'active'}});await old;
  assert.equal(ctx.roots.workpulse.classList.contains('on'),false);
  ctx.feature.onSelectChange();ctx.setSel('');ctx.setSelGen(4);await ctx.feature.render();
  ctx.feature.renderHead();
  assert.equal(ctx.roots.workpulse.classList.contains('on'),false);assert.equal(ctx.roots.convtools.hidden,true);assert.equal(ctx.effects.setComposerClosed.at(-1),true);
  ctx.feature.destroy();
});

test('old peek response cannot repaint a later visit to the same node', async () => {
  let release,calls=0;const quiet={...defaultChatPayload(),live:'quiet'};
  const ctx=makeFeature({api:path=>path.includes('/peek')?new Promise(resolve=>{release=resolve;}):Promise.resolve({}),
    chatConditional:()=>({status:200,etag:'x',data:++calls===1?{...quiet,attention:'inspect',fallback:true}:quiet})});
  const old=ctx.feature.render();await new Promise(r=>setImmediate(r));assert.equal(typeof release,'function');
  ctx.feature.onSelectChange();ctx.setSel('team/s/o/c/g');ctx.setSelGen(2);
  ctx.feature.onSelectChange();ctx.setSel('n1');ctx.setSelGen(3);await ctx.feature.render();
  release('stale private pane');await old;assert.doesNotMatch(ctx.roots.msgs.innerHTML,/stale private pane/);
  assert.equal(ctx.roots.workpulse.classList.contains('on'),false);ctx.feature.destroy();
});

test('retiring a compacting chat for no selection removes its visible status and controls', async () => {
  const ctx=makeFeature({chatPayload:compactingPayload(true)});ctx.feature.bind();await ctx.feature.render();
  assert.match(ctx.roots.msgs.innerHTML,/compacting/);assert.equal(ctx.effects.setComposerBusy.at(-1),true);
  ctx.feature.onSelectChange();ctx.setSel('');ctx.setSelGen(2);await ctx.feature.render();
  assert.doesNotMatch(ctx.roots.msgs.innerHTML,/compacting/);assert.equal(ctx.effects.setComposerBusy.at(-1),false);
  assert.equal(ctx.roots.workpulse.classList.contains('on'),false);assert.equal(ctx.roots.convtools.hidden,true);ctx.feature.destroy();
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

  // A transcript end_turn can precede Claude's authoritative Stop hook. Keep
  // the composer interruptible until the server's turn lease is released.
	const ready = makeFeature({
	  chatPayload: {
		turns: [{ role: "assistant", text: "done" }],
		live: "active", reply_ready: true, turn_in_flight: true,
		delivery: "ok", source: "tmux",
		chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
	  },
	});
	await ready.feature.render();
	assert.equal(ready.roots.workpulse.classList.contains("on"), false);
	assert.equal(ready.effects.setComposerBusy.at(-1), true);

	const stopped = makeFeature({
	  chatPayload: {
		turns: [{ role: "assistant", text: "done" }],
		live: "active", reply_ready: true, turn_in_flight: false,
		delivery: "ok", source: "tmux",
		chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
	  },
	});
	await stopped.feature.render();
	assert.equal(stopped.effects.setComposerBusy.at(-1), false);

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

test('a retired approval observer cannot replace the current visit observer', async () => {
  const spy=roSpy();const ctx=makeFeature({nodes:acpApprovalNodes(),chatPayload:acpApprovalPayload(),deps:{ResizeObserver:spy.RO,requestAnimationFrame:()=>0}});
  ctx.roots.keyrow._permMaskMetrics={scrollHeight:0,clientHeight:0};ctx.feature.bind();await ctx.feature.render();
  const old=spy.forMask(),oldMask=old.targets[0];ctx.feature.onSelectChange();ctx.setSel('n1');ctx.setSelGen(2);await ctx.feature.render();
  const current=spy.observers.filter(o=>o.targets.some(t=>String(t.className||'').includes('permask'))).at(-1);
  assert.notEqual(current,old);oldMask.clientHeight=200;old.cb([]);assert.equal(current.disconnected,false);ctx.feature.destroy();
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

test('retired scroll callbacks cannot move a received chat', async () => {
  const rafs=[],observers=[];
  const ctx=makeFeature({deps:{requestAnimationFrame:fn=>{rafs.push(fn);return rafs.length;},ResizeObserver:class{constructor(cb){observers.push(cb);}observe(){}disconnect(){}}}});
  ctx.roots.msgs.scrollHeight=3000;ctx.roots.msgs.clientHeight=0;ctx.feature.onSelectChange();await ctx.feature.render();
  assert.ok(rafs.length);assert.ok(observers.length);
  ctx.feature.onSelectChange();ctx.setSel('team/s/o/c/g');ctx.setSelGen(2);
  ctx.roots.msgs.clientHeight=600;ctx.roots.msgs.scrollTop=100;
  for(const fn of rafs)fn();for(const fn of observers)fn([]);
  assert.equal(ctx.roots.msgs.scrollTop,100);ctx.feature.destroy();
});

test('missing chat roots and failing layout observers remain safe', async () => {
  const rafs=[],roots=makeRoots();roots.msgs=null;roots.chathead=null;
  const missing=makeFeature({roots,deps:{requestAnimationFrame:fn=>{rafs.push(fn);return rafs.length;}}});
  missing.feature.renderHead();await missing.feature.render();missing.feature.jumpToNow();assert.ok(rafs.length);rafs.forEach(fn=>fn());missing.feature.destroy();
  const broken=makeFeature({nodes:[{id:'n1',title:'Synthetic'}],deps:{ResizeObserver:class{constructor(){throw Error('observer unavailable');}},requestAnimationFrame:()=>0}});
  broken.roots.msgs.clientHeight=0;broken.feature.onSelectChange();await broken.feature.render();
  assert.equal(broken.roots.chatagentlogo.title,'agent');broken.feature.destroy();
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
  const doc = peekGrowingDocument(roots.msgs);
  const feature = createChatFeature({
    roots,
    document: doc,
    window: {},
    CSS: { escape: s => String(s) },
    assetURL: path => path,
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
    feature, roots, doc,
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

test("an unchanged poll completes a deferred explicit jump to the bottom", async () => {
  const ctx=makePeekSkipFeature();await ctx.feature.render();
  ctx.roots.msgs.clientHeight=0;ctx.feature.jumpToNow();
  await ctx.feature.render();
  ctx.roots.msgs.clientHeight=600;ctx.roots.msgs.scrollTop=100;
  await ctx.feature.render();
  assert.equal(ctx.roots.msgs.scrollTop,ctx.roots.msgs.scrollHeight-600);
  ctx.feature.destroy();
});

test("focused discussion editor keeps its anchor through a subsequent unchanged poll", async () => {
  const ctx=makePeekSkipFeature(); await ctx.feature.render();
  const turn={dataset:{gkey:'s/o/u/g',cseq:'1'},getBoundingClientRect:()=>({top:400})};
  const input={selectionStart:2,selectionEnd:4,selectionDirection:'forward'};
  const row={previousElementSibling:turn,contains:element=>element===input};
  input.closest=()=>row; ctx.doc.activeElement=input;
  const query=ctx.roots.msgs.querySelector.bind(ctx.roots.msgs);
  ctx.roots.msgs.querySelector=selector=>selector==='[data-grename]'?input:query(selector);
  const before=ctx.roots.msgs.scrollHeight-ctx.roots.msgs.clientHeight-76;
  ctx.roots.msgs.scrollTop=before; await ctx.feature.render();
  assert.equal(ctx.roots.msgs.scrollTop,before,'a later poll must not undo the editor anchor');
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

const COMPACTING_STATUS =
  "Claude is compacting the conversation context. Responses will resume when it finishes.";

test("buildChatSignature changes when compacting flips", () => {
  const base = {
    nodeId: "n1", attention: "", attentionHidden: false,
    delivery: "ok", showPeek: false, termOpen: false, termFull: false,
    source: "tmux", permTitle: "", permOptionsKey: "", priorTurns: 0,
    chatStarted: "2026-01-01T00:00:00Z", echoHash: "", histKey: "",
    turnsHash: "t",
  };
  const off = buildChatSignature(base);
  const on = buildChatSignature({ ...base, compacting: true });
  assert.notEqual(off, on, "compacting must participate in the chat signature");
  assert.equal(buildChatSignature({ ...base, compacting: false }), off);
  assert.equal(chatRenderDecision(on, off), "rebuild");
});

function compactingPayload(compacting, extra = {}){
  return {
    turns: [{ role: "user", text: "hello", time: "2026-01-01T00:00:00Z" },
            { role: "assistant", text: "hi", time: "2026-01-01T00:01:00Z" }],
    live: "quiet",
    delivery: "ok",
    source: "tmux",
    supervision: "claude_strict",
    compacting,
    compact_trigger: compacting ? "auto" : undefined,
    compact_summary: "SECRET SUMMARY",
    custom_instructions: "SECRET INSTRUCTIONS",
    chat_started: "2026-01-01T00:00:00Z",
    prior_turns: 0,
    assets: {},
    reply_ready: false,
    ...extra,
  };
}

test("compacting status is one inline status, busy chrome, no terminal or attention", async () => {
  let compacting = true;
  const ctx = makeFeature({
    chatConditional: () => ({
      status: 200,
      etag: compacting ? '"c1"' : '"c0"',
      data: compactingPayload(compacting),
    }),
  });
  ctx.feature.bind();
  await ctx.feature.render();
  const html = ctx.roots.msgs.innerHTML;
  assert.equal([...html.matchAll(/role="status"/g)].length, 1,
    "exactly one inline status in the live chat");
  assert.match(html, new RegExp(COMPACTING_STATUS.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")));
  assert.ok(html.indexOf("hi") < html.indexOf(COMPACTING_STATUS),
    "status renders after the live timeline");
  assert.doesNotMatch(html, /SECRET SUMMARY|SECRET INSTRUCTIONS|compact_summary|custom_instructions/);
  assert.equal(ctx.roots.workpulse.classList.contains("on"), true,
    "working pulse stays on while compacting even if live is quiet");
  assert.equal(ctx.effects.setComposerBusy.at(-1), true);
  assert.equal(ctx.roots.termtoggle.classList.contains("on"), false,
    "compaction must not force the terminal");
  assert.equal(ctx.roots.keyrow.innerHTML, "",
    "compaction must not create a dialog/action row");

  compacting = false;
  await ctx.feature.render();
  assert.doesNotMatch(ctx.roots.msgs.innerHTML, /Claude is compacting/,
    "status disappears after PostCompact without a transcript change");
  ctx.feature.destroy();
});

test("compacting status does not yank a scrolled-up reader, and pins a bottom reader", async () => {
  let compacting = false;
  const ctx = makeFeature({
    chatConditional: () => ({
      status: 200,
      etag: compacting ? '"c1"' : '"c0"',
      data: compactingPayload(compacting),
    }),
  });
  ctx.roots.msgs.scrollHeight = 3000;
  ctx.roots.msgs.clientHeight = 600;
  ctx.roots.msgs.scrollTop = 200;
  await ctx.feature.render();
  compacting = true;
  await ctx.feature.render();
  assert.equal(ctx.roots.msgs.scrollTop, 200,
    "a reader scrolled upward is not pulled to the bottom");

  const bottom = makeFeature({
    chatConditional: () => ({
      status: 200,
      etag: compactingBottom ? '"b1"' : '"b0"',
      data: compactingPayload(compactingBottom),
    }),
  });
  let compactingBottom = false;
  bottom.roots.msgs.scrollHeight = 3000;
  bottom.roots.msgs.clientHeight = 600;
  bottom.roots.msgs.scrollTop = 2400;
  await bottom.feature.render();
  compactingBottom = true;
  await bottom.feature.render();
  assert.ok(atBottom(bottom.roots.msgs),
    "a reader already at the bottom stays pinned when the status appears");
  compactingBottom = false;
  await bottom.feature.render();
  assert.ok(atBottom(bottom.roots.msgs),
    "a reader already at the bottom stays pinned when the status disappears");
  ctx.feature.destroy();
  bottom.feature.destroy();
});

function elicitationPayload(extra = {}){
  return {
    turns: [{ role: "user", text: "hello", time: "2026-01-01T00:00:00Z" },
            { role: "assistant", text: "hi", time: "2026-01-01T00:01:00Z" }],
    live: "quiet",
    delivery: "ok",
    source: "tmux",
    supervision: "claude_strict",
    attention: "question",
    elicitation_waiting: true,
    elicitation_count: 1,
    elicitations: [{
      server: "docs-server",
      message: "Please provide your credentials",
      mode: "form",
    }],
    chat_started: "2026-01-01T00:00:00Z",
    prior_turns: 0,
    assets: {},
    reply_ready: false,
    auto_approve: { phase: "off" },
    ...extra,
  };
}

test("isSafeElicitationURL accepts only absolute http(s)", () => {
  assert.equal(isSafeElicitationURL("https://auth.example.com/login"), true);
  assert.equal(isSafeElicitationURL("http://localhost:8080/x"), true);
  assert.equal(isSafeElicitationURL("javascript:alert(1)"), false);
  assert.equal(isSafeElicitationURL("data:text/html,<script>"), false);
  assert.equal(isSafeElicitationURL("/relative"), false);
  assert.equal(isSafeElicitationURL("//evil.example"), false);
  assert.equal(isSafeElicitationURL("https://user:pass@evil.example"), false);
});

test("elicitationStatusHTML escapes text and never uses Markdown", () => {
  const html = elicitationStatusHTML({
    elicitation_waiting: true,
    elicitation_count: 1,
    elicitations: [{
      server: "<script>alert(1)</script>",
      message: "**bold** <img src=x>",
      mode: "form",
    }],
  });
  assert.match(html, /class="pending elicitation"/);
  assert.match(html, /&lt;script&gt;alert\(1\)&lt;\/script&gt;/);
  assert.match(html, /\*\*bold\*\*/);
  assert.doesNotMatch(html, /<script>/);
  assert.doesNotMatch(html, /<img /);
  assert.doesNotMatch(html, /<strong>/);
  assert.match(html, new RegExp(ELICITATION_OPEN_LABEL));
  assert.match(html, /data-open-terminal="1"/);
  assert.doesNotMatch(html, /data-key=/);
});

test("elicitationStatusHTML URL mode renders only a validated http(s) link", () => {
  const ok = elicitationStatusHTML({
    elicitation_waiting: true,
    elicitation_count: 1,
    elicitations: [{
      server: "auth",
      message: "Please authenticate",
      mode: "url",
      url: "https://auth.example.com/login",
    }],
  });
  assert.match(ok, /href="https:\/\/auth\.example\.com\/login"/);
  assert.match(ok, /target="_blank"/);
  assert.match(ok, /rel="noopener noreferrer"/);

  const bad = elicitationStatusHTML({
    elicitation_waiting: true,
    elicitation_count: 1,
    elicitations: [{
      server: "auth",
      message: "Please authenticate",
      mode: "url",
      url: "javascript:alert(1)",
    }],
  });
  assert.doesNotMatch(bad, /href=/);
  assert.doesNotMatch(bad, /javascript:/);
});

test("keyRowHTML: elicitation-only question never shows a guessed keypad", () => {
  const html = keyRowHTML({
    attention: "question",
    elicitationWaiting: true,
    source: "tmux",
  });
  assert.equal(html, "");
  assert.doesNotMatch(html, /data-key=/);
});

test("buildChatSignature changes when elicitation flips", () => {
  const base = {
    nodeId: "n1", attention: "", attentionHidden: false,
    delivery: "ok", showPeek: false, termOpen: false, termFull: false,
    source: "tmux", permTitle: "", permOptionsKey: "", priorTurns: 0,
    chatStarted: "2026-01-01T00:00:00Z", echoHash: "", histKey: "",
    turnsHash: "t",
  };
  const off = buildChatSignature(base);
  const on = buildChatSignature({
    ...base,
    attention: "question",
    elicitationKey: elicitationKey(elicitationPayload()),
  });
  assert.notEqual(off, on, "elicitation must participate in the chat signature");
  assert.equal(chatRenderDecision(on, off), "rebuild");
});

test("elicitation status is dedicated, not permManual, and survives auto-approve", async () => {
  const payload = elicitationPayload({
    auto_approve: { phase: "armed", supported: true },
  });
  const nodes = [{
    id: "n1", title: "Alpha", agent: "claude", model: "sonnet",
    live: "quiet", attention: "question", lane_id: "L1", description: "d",
  }];
  const ctx = makeFeature({
    nodes,
    chatPayload: payload,
  });
  ctx.feature.bind();
  await ctx.feature.render();
  const html = ctx.roots.msgs.innerHTML;
  assert.match(html, /docs-server/);
  assert.match(html, /Please provide your credentials/);
  assert.match(html, /form/);
  assert.match(html, /data-open-terminal="1"/);
  assert.doesNotMatch(html, /data-key=/);
  assert.doesNotMatch(ctx.roots.keyrow.innerHTML, /data-key=/);
  assert.doesNotMatch(ctx.roots.keyrow.innerHTML, /Yes/);
  assert.equal(ctx.roots.termtoggle.classList.contains("on"), false,
    "elicitation must not auto-open the terminal");
  ctx.feature.destroy();
});

test("multiple elicitations show a bounded list", () => {
  const html = elicitationStatusHTML({
    elicitation_waiting: true,
    elicitation_count: 3,
    elicitations: [
      { server: "a", message: "one", mode: "form" },
      { server: "b", message: "two", mode: "url", url: "https://example.com" },
    ],
  });
  assert.match(html, /3 MCP input requests/);
  assert.match(html, />a</);
  assert.match(html, />b</);
});

test("elicitation Open Terminal sends no remote key", async () => {
  const payload = elicitationPayload();
  const nodes = [{
    id: "n1", title: "Alpha", agent: "claude", model: "sonnet",
    live: "quiet", attention: "question", lane_id: "L1", description: "d",
  }];
  const ctx = makeFeature({ nodes, chatPayload: payload });
  ctx.feature.bind();
  await ctx.feature.render();
  assert.match(ctx.roots.msgs.innerHTML, /data-open-terminal="1"/);
  assert.doesNotMatch(ctx.roots.msgs.innerHTML, /data-key=/);
  assert.match(chatSrc, /function onMsgsClick[\s\S]*data-open-terminal/);
  assert.match(chatSrc, /if \(openTerm\)\{\s*if \(!termOpen\) onTermToggle\(\);\s*return;/);
  ctx.feature.destroy();
});

test("elicitation rebuild preserves a scrolled-up reader and pins a bottom reader", async () => {
  let waiting = false;
  const ctx = makeFeature({
    nodes: [{
      id: "n1", title: "Alpha", agent: "claude", model: "sonnet",
      live: "quiet", attention: waiting ? "question" : "", lane_id: "L1", description: "d",
    }],
    chatConditional: () => ({
      status: 200,
      etag: waiting ? '"e1"' : '"e0"',
      data: waiting ? elicitationPayload() : {
        ...elicitationPayload(),
        elicitation_waiting: false,
        elicitation_count: 0,
        elicitations: [],
        attention: "",
      },
    }),
  });
  ctx.roots.msgs.scrollHeight = 3000;
  ctx.roots.msgs.clientHeight = 600;
  ctx.roots.msgs.scrollTop = 200;
  await ctx.feature.render();
  waiting = true;
  ctx.nodes[0].attention = "question";
  await ctx.feature.render();
  assert.equal(ctx.roots.msgs.scrollTop, 200,
    "a reader scrolled upward is not pulled to the bottom");
  ctx.feature.destroy();
});

/* ---------- Packet C: bubble actions, histload, open-term, thumb error, key rollback ----------
 * Harness does not materialise HTML buttons as children (same as /key click tests),
 * so actions click synthetic [data-bact] / .histload / [data-open-terminal] targets.
 * A live turn must be tapped first so bubbleTurns[tappedTurn] is the action subject.
 */

function tapBubbleTurn(ctx, bk = "i:0"){
  const turn = el("div", { className: "turn user", dataset: { bk } });
  ctx.roots.msgs.appendChild(turn);
  firstListener(ctx.roots.msgs, "click")({ target: turn });
  return turn;
}

test('ordinary human bubble exposes its action row to keyboard activation', async () => {
 const ctx=makeFeature({chatPayload:{turns:[{role:'user',text:'synthetic'}],live:'quiet'}});ctx.feature.bind();await ctx.feature.render();
 assert.match(ctx.roots.msgs.innerHTML,/class="bubble"[^>]*tabindex="0"/);
 const turn=el('div',{className:'turn user',dataset:{bk:'i:0'}}),bubble=el('div',{className:'bubble'});turn.appendChild(bubble);ctx.roots.msgs.appendChild(turn);
 const closest=bubble.closest.bind(bubble);bubble.closest=selector=>selector==='.bubble'?bubble:closest(selector);
 let prevented=false;firstListener(ctx.roots.msgs,'keydown')({key:'Enter',target:bubble,preventDefault(){prevented=true}});
 assert.equal(prevented,true);assert.equal(bubActionRows(ctx).length,1);ctx.feature.destroy();
});

/* A document whose selection reports what a click-drag would have left behind. */
function selectingDocument(text){
  return {
    createElement: tag => el(tag),
    querySelector: () => null,
    getSelection: () => ({
      isCollapsed: text === "",
      toString: () => text,
    }),
  };
}

function bubActionRows(ctx){
  return ctx.roots.msgs.querySelectorAll(".bubactions");
}

test("P3C: click-dragging a selection inside a bubble does not open the action bar", async () => {
  const ctx = makeFeature({
    chatPayload: {
      turns: [{ role: "user", text: "select me", time: "2026-01-01T00:00:00Z" }],
      live: "quiet", delivery: "ok", source: "tmux",
      chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
    },
    deps: { document: selectingDocument("select") },
  });
  ctx.feature.bind();
  await ctx.feature.render();
  tapBubbleTurn(ctx, "i:0");
  assert.equal(bubActionRows(ctx).length, 0,
    "marking text is not a request for the action bar");
  ctx.feature.destroy();
});

test("P3C: a selection does not close an action bar that is already open", async () => {
  let selected = "";
  const ctx = makeFeature({
    chatPayload: {
      turns: [{ role: "user", text: "select me", time: "2026-01-01T00:00:00Z" }],
      live: "quiet", delivery: "ok", source: "tmux",
      chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
    },
    deps: {
      document: {
        createElement: tag => el(tag),
        querySelector: () => null,
        getSelection: () => ({
          isCollapsed: selected === "",
          toString: () => selected,
        }),
      },
    },
  });
  ctx.feature.bind();
  await ctx.feature.render();
  const turn = tapBubbleTurn(ctx, "i:0");
  assert.equal(bubActionRows(ctx).length, 1, "a plain tap still opens it");
  selected = "select";
  firstListener(ctx.roots.msgs, "click")({ target: turn });
  assert.equal(bubActionRows(ctx).length, 1,
    "and dragging over the same bubble leaves it where it was");
  ctx.feature.destroy();
});

test("P3C: a collapsed selection leaves the bubble tap working", async () => {
  const ctx = makeFeature({
    chatPayload: {
      turns: [{ role: "user", text: "tap me", time: "2026-01-01T00:00:00Z" }],
      live: "quiet", delivery: "ok", source: "tmux",
      chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
    },
    deps: { document: selectingDocument("") },
  });
  ctx.feature.bind();
  await ctx.feature.render();
  const turn = tapBubbleTurn(ctx, "i:0");
  assert.equal(bubActionRows(ctx).length, 1, "opens");
  firstListener(ctx.roots.msgs, "click")({ target: turn });
  assert.equal(bubActionRows(ctx).length, 0, "and the second tap closes it");
  ctx.feature.destroy();
});

function clickBact(ctx, bact){
  const btn = el("button", { dataset: { bact } });
  firstListener(ctx.roots.msgs, "click")({ target: btn });
  return btn;
}

test('ordinary Send to preserves the exact source address and role', async () => {
 const sent=[],ctx=makeFeature({chatPayload:{turns:[{role:'assistant',text:'synthetic answer',uid:'log',segment:2,record:7}],live:'quiet'},deps:{openSendTo:value=>sent.push(value)}});
 ctx.feature.bind();await ctx.feature.render();tapBubbleTurn(ctx);clickBact(ctx,'sendto');
 assert.equal(sent.length,1);assert.equal(sent[0].role,'assistant');assert.deepEqual(sent[0].source,{node:'n1',turnTime:'',uid:'log',segment:2,record:7});ctx.feature.destroy();
});

test('an empty conditional response retires loading without painting stale chat', async () => {
 const ctx=makeFeature({chatConditional:async()=>({status:200,data:null})});ctx.feature.bind();
 await ctx.feature.render();assert.doesNotMatch(ctx.roots.msgs.innerHTML,/synthetic answer/);ctx.feature.destroy();
});

test('keyboard events outside a turn and an unavailable prompt adapter do not prepare text', async () => {
 const ctx=makeFeature({chatPayload:{turns:[{role:'user',text:'synthetic'}],live:'quiet'},deps:{canPreparePrompt:()=>true,preparePrompt:null}});
 ctx.feature.bind();await ctx.feature.render();firstListener(ctx.roots.msgs,'keydown')({key:'Enter',target:el('div'),preventDefault(){throw Error('unexpected activation')}});
 tapBubbleTurn(ctx);assert.doesNotThrow(()=>clickBact(ctx,'prompt'));ctx.feature.destroy();
});

test('bookmark capture uses empty optional fields and suppresses an exact duplicate arriving during capture', async () => {
 let finish;const bookmarks=[],ops=[];
 const ctx=makeFeature({nodes:[{id:'n1',title:'Local'}],chatPayload:{turns:[{role:'assistant'}],live:'quiet'},deps:{bookmarks:()=>bookmarks,captureBookmark:value=>new Promise(resolve=>{finish=()=>resolve({...value,uid:'u',segment:0,record:1})}),uiMutate:op=>ops.push(op)}});
 ctx.feature.bind();await ctx.feature.render();tapBubbleTurn(ctx);clickBact(ctx,'bookmark');
 bookmarks.push({uid:'u',segment:0,record:1});finish();await Promise.resolve();assert.deepEqual(ops,[]);ctx.feature.destroy();
});

test('bookmark capture tolerates an unavailable list and missing optional turn fields', async () => {
 let bookmarks=[];const saved=[],ctx=makeFeature({chatPayload:{turns:[{role:'user'}],live:'quiet'},deps:{bookmarks:()=>bookmarks,uiMutate:op=>saved.push(op)}});
 ctx.feature.bind();await ctx.feature.render();tapBubbleTurn(ctx);bookmarks=null;clickBact(ctx,'bookmark');
 assert.equal(saved.length,1);assert.equal(saved[0].bookmark.text,'');assert.equal(saved[0].bookmark.turnTime,'');ctx.feature.destroy();
});

test('bookmark duplicate comparison tolerates missing text and time', async () => {
 const ops=[],ctx=makeFeature({chatPayload:{turns:[{role:'user'}],live:'quiet'},deps:{bookmarks:()=>[{node:'n1',turnTime:'',text:''}],uiMutate:op=>ops.push(op)}});
 ctx.feature.bind();await ctx.feature.render();tapBubbleTurn(ctx);clickBact(ctx,'bookmark');assert.deepEqual(ops,[]);ctx.feature.destroy();
});

test("P3C: copy action calls copyText and shows copied ack", async () => {
  const copied = [];
  const ctx = makeFeature({
    chatPayload: {
      turns: [{ role: "user", text: "copy me", time: "2026-01-01T00:00:00Z" }],
      live: "quiet", delivery: "ok", source: "tmux",
      chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
    },
    deps: { copyText: s => copied.push(s) },
  });
  ctx.feature.bind();
  await ctx.feature.render();
  tapBubbleTurn(ctx, "i:0");
  const btn = clickBact(ctx, "copy");
  assert.deepEqual(copied, ["copy me"]);
  assert.match(btn.innerHTML, /copied/);
  ctx.feature.destroy();
});

test("P3C: bookmark add calls uiMutate bookmark-add", async () => {
  const ops = [];
  const stamped = [];
  const ctx = makeFeature({
    chatPayload: {
      turns: [{ role: "assistant", text: "note this", time: "2026-01-01T00:01:00Z" }],
      live: "quiet", delivery: "ok", source: "tmux",
      chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
    },
    deps: {
      bookmarks: () => [],
      uiMutate: op => ops.push(op),
      stampAddress: (bm, turn) => stamped.push({ bm, turn }),
    },
  });
  ctx.feature.bind();
  await ctx.feature.render();
  tapBubbleTurn(ctx, "i:0");
  const btn = clickBact(ctx, "bookmark");
  assert.equal(ops.length, 1);
  assert.equal(ops[0].k, "bookmark-add");
  assert.equal(ops[0].bookmark.text, "note this");
  assert.equal(ops[0].bookmark.node, "n1");
  assert.equal(ops[0].bookmark.turnTime, "2026-01-01T00:01:00Z");
  assert.equal(ops[0].bookmark.lane, "L1");
  assert.equal(stamped.length, 1);
  assert.match(btn.innerHTML, /noted/);
  ctx.feature.destroy();
});

test("image bookmark waits for capture and ignores a late response after selection changes", async () => {
  const ops = [];
  let finish;
  const ctx = makeFeature({
    chatPayload: {
      turns: [
        {role:"assistant",text:"![one](scimux-asset:a_1)",time:"T1",uid:"u",segment:0,record:1},
        {role:"assistant",text:"new selection",time:"T2",uid:"u",segment:0,record:2},
      ],
      live:"quiet",delivery:"ok",source:"acp",chat_started:"T0",prior_turns:0,
      assets:{a_1:{name:"one.png"}},
    },
    deps:{
      bookmarks:()=>[],
      uiMutate:op=>ops.push(op),
      captureBookmark:()=>new Promise(resolve=>{ finish=resolve; }),
    },
  });
  ctx.feature.bind(); await ctx.feature.render();
  tapBubbleTurn(ctx, "i:0");
  const button = clickBact(ctx, "bookmark");
  assert.equal(button.textContent, "capturing…");
  assert.equal(ops.length, 0);
  ctx.setSel("n2");
  finish({text:"durable",uid:"u",segment:0,record:1,media:{version:1,capture_id:"a".repeat(64),items:[]}});
  await Promise.resolve(); await Promise.resolve();
  assert.equal(ops.length, 0);
  ctx.feature.destroy();
});

test('bookmark capture from an earlier visit cannot commit after returning to the same chat', async () => {
 let finish;const ops=[];
 const ctx=makeFeature({chatPayload:{turns:[{role:'assistant',text:'synthetic',uid:'u',record:1}],live:'quiet'},deps:{bookmarks:()=>[],uiMutate:op=>ops.push(op),captureBookmark:()=>new Promise(resolve=>{finish=resolve})}});
 ctx.feature.bind();await ctx.feature.render();tapBubbleTurn(ctx,'i:0');clickBact(ctx,'bookmark');
 ctx.feature.onSelectChange();ctx.setSel('n2');ctx.setSelGen(2);ctx.feature.onSelectChange();ctx.setSel('n1');ctx.setSelGen(3);
 finish({text:'synthetic',uid:'u',segment:0,record:1});await Promise.resolve();await Promise.resolve();assert.deepEqual(ops,[]);ctx.feature.destroy();
});

test("image bookmark capture failure is retryable through the real bubble action", async () => {
  const ops = [], toasts = [];
  let attempts = 0;
  const turn = {role:"assistant",text:"![one](scimux-asset:a_1)",time:"T1",uid:"u",segment:0,record:1};
  const ctx = makeFeature({
    chatPayload:{turns:[turn],live:"quiet",delivery:"ok",source:"acp",chat_started:"T0",prior_turns:0,assets:{a_1:{name:"one.png"}}},
    deps:{bookmarks:()=>[],uiMutate:op=>ops.push(op),toast:x=>toasts.push(x),captureBookmark:async bm=>{
      attempts++;
      if (attempts === 1) throw new Error("budget");
      return {...bm,text:"durable",media:{version:1,capture_id:"b".repeat(64),items:[{id:"0",key:"asset:a_1",name:"one.png",state:"ready"}]}};
    }},
  });
  ctx.feature.bind(); await ctx.feature.render();
  tapBubbleTurn(ctx, "i:0"); clickBact(ctx, "bookmark");
  await Promise.resolve(); await Promise.resolve();
  assert.equal(ops.length, 0); assert.equal(toasts.length, 1);
  clickBact(ctx, "bookmark");
  await Promise.resolve(); await Promise.resolve();
  assert.equal(ops.length, 1); assert.equal(ops[0].bookmark.text, "durable");
  ctx.feature.destroy();
});

test("bubble bookmark handles synchronous capture success and failure", async () => {
  for (const failing of [true,false]) {
    const ops=[], toasts=[];
    const turn={role:"assistant",text:"![one](scimux-asset:a_1)",time:"T",uid:"u",segment:0,record:1};
    const ctx=makeFeature({chatPayload:{turns:[turn],live:"quiet",delivery:"ok",source:"acp",chat_started:"T0",prior_turns:0,assets:{a_1:{name:"one.png"}}},deps:{
      bookmarks:()=>[],uiMutate:op=>ops.push(op),toast:x=>toasts.push(x),
      captureBookmark:bm=>{if(failing) throw new Error("sync"); return {...bm,text:"sync durable"};},
    }});
    ctx.feature.bind(); await ctx.feature.render(); tapBubbleTurn(ctx,"i:0"); clickBact(ctx,"bookmark");
    assert.equal(ops.length,failing?0:1); assert.equal(toasts.length,failing?1:0);
    ctx.feature.destroy();
  }
});

test("hashTurnAttrs distinguishes absent prov from explicit null", () => {
  const absent = [{ role: "assistant", text: "same", agent: "grok" }];
  const explicitNull = [{ role: "assistant", text: "same", agent: "grok", prov: null }];
  assert.notEqual(hashTurnAttrs(absent), hashTurnAttrs(explicitNull),
    "absent prov and own prov:null must not share a hash");
  assert.equal(hashTurnAttrs(absent), hashTurnAttrs(absent));
  assert.equal(hashTurnAttrs(explicitNull), hashTurnAttrs(explicitNull));
});

test("absent-to-null prov rebuilds and bookmarks explicit null", async () => {
  const ops = [];
  const base = {
    role: "assistant", text: "same text", time: "2026-09-10T12:00:00Z",
    uid: "u1", segment: 0, record: 2, agent: "grok",
  };
  let payloadTurns = [{ ...base }];
  const ctx = makeFeature({
    chatConditional: async () => ({
      status: 200,
      etag: '"e1"',
      data: {
        turns: payloadTurns,
        live: "quiet", delivery: "ok", source: "acp",
        chat_started: "2026-09-10T11:00:00Z", prior_turns: 0, assets: {},
      },
    }),
    deps: {
      bookmarks: () => [],
      uiMutate: op => ops.push(op),
      stampAddress: (bm, turn) => {
        bm.uid = turn.uid;
        bm.segment = turn.segment;
        bm.record = turn.record;
      },
    },
  });
  ctx.feature.bind();
  await ctx.feature.render();
  payloadTurns = [{ ...base, prov: null }];
  await ctx.feature.render();
  tapBubbleTurn(ctx, "i:0");
  clickBact(ctx, "bookmark");
  assert.equal(ops.length, 1);
  assert.equal(Object.prototype.hasOwnProperty.call(ops[0].bookmark, "prov"), true);
  assert.equal(ops[0].bookmark.prov, null);
  ctx.feature.destroy();
});

test("null-to-absent prov rebuilds and omits bookmark prov", async () => {
  const ops = [];
  const base = {
    role: "assistant", text: "same text", time: "2026-09-10T12:00:00Z",
    uid: "u1", segment: 0, record: 2, agent: "grok",
  };
  let payloadTurns = [{ ...base, prov: null }];
  const ctx = makeFeature({
    chatConditional: async () => ({
      status: 200,
      etag: '"e1"',
      data: {
        turns: payloadTurns,
        live: "quiet", delivery: "ok", source: "acp",
        chat_started: "2026-09-10T11:00:00Z", prior_turns: 0, assets: {},
      },
    }),
    deps: {
      bookmarks: () => [],
      uiMutate: op => ops.push(op),
      stampAddress: (bm, turn) => {
        bm.uid = turn.uid;
        bm.segment = turn.segment;
        bm.record = turn.record;
      },
    },
  });
  ctx.feature.bind();
  await ctx.feature.render();
  payloadTurns = [{ ...base }];
  await ctx.feature.render();
  tapBubbleTurn(ctx, "i:0");
  clickBact(ctx, "bookmark");
  assert.equal(ops.length, 1);
  assert.equal(Object.prototype.hasOwnProperty.call(ops[0].bookmark, "prov"), false);
  ctx.feature.destroy();
});

test("same role/text turn with changed agent/prov rebuilds and bookmarks fresh values", async () => {
  const ops = [];
  let payloadTurns = [{
    role: "assistant", text: "same text", time: "2026-09-10T12:00:00Z",
    uid: "u1", segment: 0, record: 2,
    agent: "grok", prov: [{ loc: "chunk", key: "_meta", v: { src: "old" } }],
  }];
  const ctx = makeFeature({
    chatConditional: async () => ({
      status: 200,
      etag: '"e1"',
      data: {
        turns: payloadTurns,
        live: "quiet", delivery: "ok", source: "acp",
        chat_started: "2026-09-10T11:00:00Z", prior_turns: 0, assets: {},
      },
    }),
    deps: {
      bookmarks: () => [],
      uiMutate: op => ops.push(op),
      stampAddress: (bm, turn) => {
        bm.uid = turn.uid;
        bm.segment = turn.segment;
        bm.record = turn.record;
      },
    },
  });
  ctx.feature.bind();
  await ctx.feature.render();
  payloadTurns = [{
    role: "assistant", text: "same text", time: "2026-09-10T12:00:00Z",
    uid: "u1", segment: 0, record: 2,
    agent: "muse", prov: [{ loc: "chunk", key: "_meta", v: { src: "new" } }],
  }];
  await ctx.feature.render();
  tapBubbleTurn(ctx, "i:0");
  clickBact(ctx, "bookmark");
  assert.equal(ops.length, 1);
  assert.equal(ops[0].bookmark.agent, "muse");
  assert.deepEqual(ops[0].bookmark.prov, [{ loc: "chunk", key: "_meta", v: { src: "new" } }]);
  ctx.feature.destroy();
});

test("circular provenance is omitted from bookmark without crashing", async () => {
  const ops = [];
  const circ = { src: "circ" };
  circ.self = circ;
  const turn = {
    role: "assistant", text: "keep me", time: "2026-09-10T12:00:00Z",
    uid: "u1", segment: 1, record: 3, agent: "codex",
    prov: circ,
  };
  assert.doesNotThrow(() => hashTurnAttrs([turn]));
  const ctx = makeFeature({
    chatPayload: {
      turns: [turn], live: "quiet", delivery: "ok", source: "acp",
      chat_started: "t0", prior_turns: 0, assets: {},
    },
    deps: {
      bookmarks: () => [],
      uiMutate: op => ops.push(op),
      stampAddress: (bm, t) => {
        bm.uid = t.uid;
        bm.segment = t.segment;
        bm.record = t.record;
      },
    },
  });
  ctx.feature.bind();
  await ctx.feature.render();
  tapBubbleTurn(ctx, "i:0");
  clickBact(ctx, "bookmark");
  assert.equal(ops.length, 1);
  assert.equal(Object.prototype.hasOwnProperty.call(ops[0].bookmark, "prov"), false);
  assert.equal(ops[0].bookmark.role, "assistant");
  assert.equal(ops[0].bookmark.agent, "codex");
  assert.equal(ops[0].bookmark.text, "keep me");
  assert.equal(ops[0].bookmark.turnTime, "2026-09-10T12:00:00Z");
  assert.equal(ops[0].bookmark.uid, "u1");
  assert.equal(ops[0].bookmark.segment, 1);
  assert.equal(ops[0].bookmark.record, 3);
  ctx.feature.destroy();
});

test("function provenance is omitted from bookmark without crashing", async () => {
  const ops = [];
  const turn = {
    role: "assistant", text: "fn", time: "T",
    uid: "u", segment: 0, record: 1, agent: "grok",
    prov: function notJSON() { return 1; },
  };
  assert.doesNotThrow(() => hashTurnAttrs([turn]));
  const ctx = makeFeature({
    chatPayload: {
      turns: [turn], live: "quiet", delivery: "ok", source: "acp",
      chat_started: "t0", prior_turns: 0, assets: {},
    },
    deps: {
      bookmarks: () => [],
      uiMutate: op => ops.push(op),
      stampAddress: (bm, t) => { bm.uid = t.uid; },
    },
  });
  ctx.feature.bind();
  await ctx.feature.render();
  tapBubbleTurn(ctx, "i:0");
  clickBact(ctx, "bookmark");
  assert.equal(ops.length, 1);
  assert.equal(Object.prototype.hasOwnProperty.call(ops[0].bookmark, "prov"), false);
  assert.equal(ops[0].bookmark.role, "assistant");
  assert.equal(ops[0].bookmark.agent, "grok");
  ctx.feature.destroy();
});

test("bookmark capture deep-freezes nested provenance", async () => {
  const ops = [];
  const nested = { loc: "message", key: "_meta", v: { src: "live", arr: [1, { k: "v" }] } };
  const turn = {
    role: "assistant", text: "freeze me", time: "T",
    uid: "u", segment: 0, record: 1, agent: "codex",
    prov: [nested],
  };
  const ctx = makeFeature({
    chatPayload: {
      turns: [turn], live: "quiet", delivery: "ok", source: "acp",
      chat_started: "t0", prior_turns: 0, assets: {},
    },
    deps: {
      bookmarks: () => [],
      uiMutate: op => ops.push(op),
      stampAddress: (bm, t) => { bm.uid = t.uid; },
    },
  });
  ctx.feature.bind();
  await ctx.feature.render();
  tapBubbleTurn(ctx, "i:0");
  clickBact(ctx, "bookmark");
  assert.equal(ops.length, 1);
  nested.v.src = "mutated";
  nested.v.arr[1].k = "changed";
  turn.agent = "nope";
  assert.equal(ops[0].bookmark.agent, "codex");
  assert.equal(ops[0].bookmark.prov[0].v.src, "live");
  assert.equal(ops[0].bookmark.prov[0].v.arr[1].k, "v");
  ctx.feature.destroy();
});

test("bookmark captures role, agent, prov for Muse/Grok/Codex assistant turns", async () => {
  const examples = [
    {
      name: "muse",
      turn: {
        role: "assistant", agent: "muse", text: "synth muse reply",
        time: "2026-09-10T12:00:00Z", uid: "uid-muse", segment: 1, record: 4,
        prov: { _meta: { src: "synth-muse", n: 1 } },
      },
    },
    {
      name: "grok",
      turn: {
        role: "assistant", agent: "grok", text: "synth grok reply",
        time: "2026-09-10T12:00:01Z", uid: "uid-grok", segment: 0, record: 2,
        prov: { _meta: { src: "synth-grok", k: "v" } },
      },
    },
    {
      name: "codex",
      turn: {
        role: "assistant", agent: "codex", text: "synth codex reply",
        time: "2026-09-10T12:00:02Z", uid: "uid-codex", segment: 2, record: 9,
        prov: { provenance: { src: "synth-codex" } },
      },
    },
  ];
  for (const ex of examples) {
    const ops = [];
    const ctx = makeFeature({
      chatPayload: {
        turns: [ex.turn],
        live: "quiet", delivery: "ok", source: "acp",
        chat_started: "2026-09-10T11:00:00Z", prior_turns: 0, assets: {},
      },
      deps: {
        bookmarks: () => [],
        uiMutate: op => ops.push(op),
        stampAddress: (bm, turn) => {
          bm.uid = turn.uid;
          bm.segment = turn.segment;
          bm.record = turn.record;
        },
      },
    });
    ctx.feature.bind();
    await ctx.feature.render();
    tapBubbleTurn(ctx, "i:0");
    clickBact(ctx, "bookmark");
    assert.equal(ops.length, 1, ex.name);
    const bm = ops[0].bookmark;
    assert.equal(bm.role, "assistant", ex.name);
    assert.equal(bm.agent, ex.turn.agent, ex.name);
    assert.deepEqual(bm.prov, ex.turn.prov, ex.name);
    assert.equal(typeof bm.prov, "object", `${ex.name} prov must stay an object`);
    assert.notEqual(typeof bm.prov, "string", `${ex.name} must not stringify prov`);
    assert.equal(bm.text, ex.turn.text, ex.name);
    assert.equal(bm.turnTime, ex.turn.time, ex.name);
    assert.equal(bm.uid, ex.turn.uid, ex.name);
    assert.equal(bm.segment, ex.turn.segment, ex.name);
    assert.equal(bm.record, ex.turn.record, ex.name);
    ctx.feature.destroy();
  }
});

test("P3C: duplicate bookmark skips uiMutate", async () => {
  const ops = [];
  const turnText = "already noted";
  const turnTime = "2026-01-01T00:01:00Z";
  const ctx = makeFeature({
    chatPayload: {
      turns: [{ role: "assistant", text: turnText, time: turnTime }],
      live: "quiet", delivery: "ok", source: "tmux",
      chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
    },
    deps: {
      bookmarks: () => [{
        t: "2026-01-01T00:00:30Z", text: turnText, node: "n1",
        turnTime, lane: "L1",
      }],
      uiMutate: op => ops.push(op),
    },
  });
  ctx.feature.bind();
  await ctx.feature.render();
  tapBubbleTurn(ctx, "i:0");
  const btn = clickBact(ctx, "bookmark");
  assert.equal(ops.length, 0, "duplicate must not emit bookmark-add");
  assert.match(btn.innerHTML, /noted/, "ack still shows for a duplicate");
  ctx.feature.destroy();
});

test("P3C: fork click calls forkFromTurn with stripped text", async () => {
  const forks = [];
  const ctx = makeFeature({
    chatPayload: {
      turns: [{
        role: "user",
        text: "see ![shot](scimux-asset:a1) please",
        time: "2026-01-01T00:00:00Z",
      }],
      live: "quiet", delivery: "ok", source: "tmux",
      chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
    },
    deps: { forkFromTurn: t => forks.push(t) },
  });
  ctx.feature.bind();
  await ctx.feature.render();
  tapBubbleTurn(ctx, "i:0");
  clickBact(ctx, "fork");
  assert.equal(forks.length, 1);
  assert.equal(forks[0], stripAssetRefs("see ![shot](scimux-asset:a1) please"));
  assert.doesNotMatch(forks[0], /scimux-asset:/);
  ctx.feature.destroy();
});

test("P3C: send-to click opens openSendTo for the source chat", async () => {
  const sends = [];
  const ctx = makeFeature({
    chatPayload: {
      turns: [{
        role: "assistant",
        text: "carry ![pic](scimux-asset:x) over",
        time: "2026-01-01T00:01:00Z",
      }],
      live: "quiet", delivery: "ok", source: "tmux",
      chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
    },
    deps: { openSendTo: opts => sends.push(opts) },
  });
  ctx.feature.bind();
  await ctx.feature.render();
  tapBubbleTurn(ctx, "i:0");
  clickBact(ctx, "sendto");
  assert.equal(sends.length, 1);
  assert.equal(sends[0].exceptId, "n1");
  assert.equal(sends[0].title, "Send to chat…");
  assert.equal(sends[0].text, stripAssetRefs("carry ![pic](scimux-asset:x) over"));
  assert.deepEqual(sends[0].source, {
    node: "n1", turnTime: "2026-01-01T00:01:00Z",
  });
  assert.doesNotMatch(sends[0].text, /scimux-asset:/);
  ctx.feature.destroy();
});

test("Send-to marker jumps directly once and offers a chooser for several", async () => {
  const jumps = [], choices = [];
  let links = [{ id: "l1", source: { node: "n1", turnTime: "T" }, destination: { node: "d1", text: "sent" } }];
  const ctx = makeFeature({
    chatPayload: {
      turns: [{ role: "assistant", text: "source", time: "T" }],
      live: "quiet", delivery: "ok", source: "tmux", chat_started: "T0", prior_turns: 0,
    },
    deps: {
      forwardLinks: () => links,
      jumpToChatAddress: address => jumps.push(address),
      openChoiceList: opts => choices.push(opts),
    },
  });
  ctx.feature.bind();
  await ctx.feature.render();
  const turn = el("div", { className: "turn assistant", dataset: { bk: "i:0" } });
  const marker = el("button", { dataset: { bmarker: "sendto" } });
  turn.appendChild(marker); ctx.roots.msgs.appendChild(turn);
  firstListener(ctx.roots.msgs, "click")({ target: marker });
  assert.deepEqual(jumps, [{ node: "d1", text: "sent" }]);

  links = [...links, { id: "l2", source: { node: "n1", turnTime: "T" }, destination: { node: "d2" } }];
  firstListener(ctx.roots.msgs, "click")({ target: marker });
  assert.equal(choices.length, 1);
  assert.equal(choices[0].choices.length, 2);
});

function markerChunk(html, bk){
  const parts = String(html).split('data-bk="');
  return parts.find(part => part.startsWith(`${bk}"`)) || "";
}

function clickBubbleMarker(ctx, bk, kind){
  const turn = el("div", { className: "turn", dataset: { bk } });
  const marker = el("button", { dataset: { bmarker: kind } });
  turn.appendChild(marker);
  ctx.roots.msgs.appendChild(turn);
  firstListener(ctx.roots.msgs, "click")({ target: marker });
}

test("bookmark icon stays for note references after deletion and on loaded history", async () => {
  const liveTurn = {
    role: "assistant", text: "live answer", time: "2026-10-02T00:00:00Z",
    uid: "uid-live", segment: 3, record: 9,
  };
  const bookmarkOnlyTurn = { role: "user", text: "keep this", time: "2026-10-03T00:00:00Z" };
  const histTurn = { role: "assistant", text: "old answer", time: "2026-08-02T00:00:00Z" };
  const liveUsage = {
    note_id: "n1", note_title: "Research", section_id: "s1", section_title: "Findings",
    reference_id: "r-live",
    source: { uid: "uid-live", segment: 3, record: 9, node: "elsewhere", turnTime: "1999-01-01T00:00:00Z" },
  };
  const histUsage = {
    note_id: "n1", note_title: "Research", section_id: "s-old", section_title: "Earlier",
    reference_id: "r-hist", source: { node: "n1", turnTime: histTurn.time },
  };
  let bookmarks = [
    { t: "bm-live", uid: "uid-live", segment: 3, record: 9, node: "n1", turnTime: liveTurn.time, text: liveTurn.text },
    { t: "bm-hist", node: "n1", turnTime: histTurn.time, text: histTurn.text },
    { t: "bm-only", node: "n1", turnTime: bookmarkOnlyTurn.time, text: bookmarkOnlyTurn.text },
  ];
  let usages = [liveUsage, histUsage];
  const links = [{
    id: "l1", source: { uid: "uid-live", segment: 3, record: 9 },
    destination: { node: "d1", text: "sent" },
  }];
  let generation = 1;
  let usageVersion = 1;
  const bookmarkOpens = [];
  const usageOpens = [];
  const sheets = [];
  const jumps = [];
  const ctx = makeFeature({
    histSegs: [{
      start: "2026-08-01T00:00:00Z", seam: "2026-08-01T00:00:00Z", reason: "clear",
      turns: [histTurn],
    }],
    chatPayload: {
      turns: [liveTurn, bookmarkOnlyTurn],
      live: "quiet", delivery: "ok", source: "tmux",
      chat_started: "2026-10-01T00:00:00Z", prior_turns: 1, assets: {},
    },
    deps: {
      bookmarks: () => bookmarks,
      noteUsages: () => usages,
      forwardLinks: () => links,
      markerVersion: () => `${generation}:${usageVersion}`,
      openBookmark: t => bookmarkOpens.push(t),
      openNoteUsage: usage => usageOpens.push(usage),
      openChoiceList: opts => sheets.push(opts),
      jumpToChatAddress: address => jumps.push(address),
      icons: {
        ICON_TERM: "T", ICON_CHECK: "✓", ICON_CHEV_UP: "↑", ICON_CHEV_DOWN: "↓",
        ICON_BRANCH: "B", ICON_INTO: "SEND", ICON_COPY: "C", ICON_DOWNALL: "↓↓",
        ICON_FILE: "F", ICON_BUBBLE_BOOKMARK: "BOOK",
      },
    },
  });
  ctx.feature.bind();
  await ctx.feature.loadHistory("n1", "");
  const rendered = () => ctx.roots.msgs.innerHTML;
  assert.match(markerChunk(rendered(), "i:0"), /data-bmarker="bookmark"/);
  assert.match(markerChunk(rendered(), "i:0"), />BOOK</);
  assert.doesNotMatch(markerChunk(rendered(), "i:0"), /bmarkercount/);
  assert.match(markerChunk(rendered(), "i:0"), /data-bmarker="sendto"/);
  assert.match(markerChunk(rendered(), "h:0:0"), /data-bmarker="bookmark"/);
  assert.match(markerChunk(rendered(), "h:0:0"), />BOOK</);
  assert.match(markerChunk(rendered(), "i:1"), /aria-label="Open bookmark"/);

  clickBubbleMarker(ctx, "i:0", "bookmark");
  assert.equal(usageOpens.length, 1);
  assert.equal(usageOpens[0], liveUsage);
  assert.deepEqual(bookmarkOpens, []);
  clickBubbleMarker(ctx, "i:1", "bookmark");
  assert.deepEqual(bookmarkOpens, ["bm-only"]);
  clickBubbleMarker(ctx, "i:0", "sendto");
  assert.deepEqual(jumps, [{ node: "d1", text: "sent" }]);

  bookmarks = bookmarks.filter(b => b.t === "bm-only");
  generation += 1;
  ctx.feature.invalidate();
  await ctx.feature.render();
  assert.match(markerChunk(rendered(), "i:0"), /data-bmarker="bookmark"/,
    "deleting the bookmark keeps the icon while a note reference remains");
  assert.match(markerChunk(rendered(), "i:0"), />BOOK</);
  assert.match(markerChunk(rendered(), "i:0"), /data-bmarker="sendto"/);
  assert.match(markerChunk(rendered(), "h:0:0"), /data-bmarker="bookmark"/,
    "a loaded historical turn keeps the same reference marker");
  assert.match(markerChunk(rendered(), "i:1"), /data-bmarker="bookmark"/);

  usageOpens.length = 0;
  bookmarkOpens.length = 0;
  clickBubbleMarker(ctx, "i:0", "bookmark");
  assert.equal(usageOpens.length, 1);
  assert.equal(usageOpens[0].reference_id, "r-live");
  assert.deepEqual(bookmarkOpens, []);
  clickBubbleMarker(ctx, "h:0:0", "bookmark");
  assert.equal(usageOpens[1], histUsage);

  const second = {
    note_id: "n2", note_title: "Risks", section_id: "s2", section_title: "Open",
    reference_id: "r-live-2", source: { uid: "uid-live", segment: 3, record: 9 },
  };
  const duplicate = {
    note_id: "n1", note_title: "Research", section_id: "s1", section_title: "Findings",
    reference_id: "r-live-dup", source: { uid: "uid-live", segment: 3, record: 9 },
  };
  usages = [liveUsage, duplicate, second, histUsage];
  usageVersion += 1;
  ctx.feature.invalidate();
  await ctx.feature.render();
  assert.match(markerChunk(rendered(), "i:0"), /bmarkercount[^>]*>2</);
  assert.match(markerChunk(rendered(), "i:0"), /aria-haspopup="menu"/);
  clickBubbleMarker(ctx, "i:0", "bookmark");
  assert.equal(sheets.length, 1);
  assert.equal(sheets[0].title, "Used in…");
  assert.deepEqual(sheets[0].choices.map(choice => choice.label), [
    "Research › Findings", "Risks › Open",
  ]);
  usageOpens.length = 0;
  sheets[0].onChoose(sheets[0].choices[1]);
  assert.equal(usageOpens.length, 1);
  assert.equal(usageOpens[0], second);

  usages = [];
  usageVersion += 1;
  ctx.feature.invalidate();
  await ctx.feature.render();
  assert.doesNotMatch(markerChunk(rendered(), "i:0"), /data-bmarker="bookmark"/);
  assert.match(markerChunk(rendered(), "i:0"), /data-bmarker="sendto"/);
  assert.doesNotMatch(markerChunk(rendered(), "h:0:0"), /data-bmarker="bookmark"/);
  assert.match(markerChunk(rendered(), "i:1"), /data-bmarker="bookmark"/);
  bookmarkOpens.length = 0;
  usageOpens.length = 0;
  clickBubbleMarker(ctx, "i:1", "bookmark");
  assert.deepEqual(bookmarkOpens, ["bm-only"]);
  assert.deepEqual(usageOpens, []);
  ctx.feature.destroy();
});

test("Send-to marker explains when its destination no longer resolves", async () => {
  const toasts = [];
  const links = [{ id: "l1", source: { node: "n1", turnTime: "T" }, destination: { node: "gone" } }];
  const ctx = makeFeature({
    chatPayload: {
      turns: [{ role: "assistant", text: "source", time: "T" }],
      live: "quiet", delivery: "ok", source: "tmux", chat_started: "T0", prior_turns: 0,
    },
    deps: {
      forwardLinks: () => links,
      jumpToChatAddress: () => false,
      toast: message => toasts.push(message),
    },
  });
  ctx.feature.bind();
  await ctx.feature.render();
  const turn = el("div", { className: "turn assistant", dataset: { bk: "i:0" } });
  const marker = el("button", { dataset: { bmarker: "sendto" } });
  turn.appendChild(marker); ctx.roots.msgs.appendChild(turn);
  firstListener(ctx.roots.msgs, "click")({ target: marker });
  assert.deepEqual(toasts, ["This destination chat is no longer available."]);
});

test("P3C: use-as-description sets editor state from the turn", async () => {
  const timers = [];
  const ctx = makeFeature({
    chatPayload: {
      turns: [{ role: "user", text: "mission brief", time: "2026-01-01T00:00:00Z" }],
      live: "quiet", delivery: "ok", source: "tmux",
      chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
    },
    deps: {
      setTimeout: (fn) => { timers.push(fn); return timers.length; },
    },
  });
  const input = ctx.roots.chatdescinput;
  input._focused = false;
  input._range = null;
  input.focus = function(){ this._focused = true; };
  input.setSelectionRange = function(a, b){
    this.selectionStart = a; this.selectionEnd = b; this._range = [a, b];
  };
  ctx.feature.bind();
  await ctx.feature.render();
  tapBubbleTurn(ctx, "i:0");
  clickBact(ctx, "desc");
  assert.equal(ctx.feature.isEditingDesc(), true);
  assert.equal(input.value, "mission brief");
  assert.equal(input.dataset.node, "n1");
  assert.equal(ctx.roots.chatdetails.hidden, false);
  assert.equal(ctx.roots.chathead.classList.contains("desc-editing"), true);
  for (const fn of timers) fn();
  assert.equal(input._focused, true);
  assert.deepEqual(input._range, [input.value.length, input.value.length]);
  ctx.feature.destroy();
});

test("P3C: histload click fetches history API", async () => {
  const segs = [{
    start: "2025-12-31T00:00:00Z", seam: "2025-12-31T00:00:00Z",
    turns: [{ role: "user", text: "old", time: "2025-12-31T00:00:01Z" }],
  }];
  const chatPayload = {
    turns: [{ role: "user", text: "now", time: "2026-01-01T00:00:00Z" }],
    live: "quiet", delivery: "ok", source: "tmux",
    chat_started: "2026-01-01T00:00:00Z", prior_turns: 1, assets: {},
  };
  const ctx = makeFeature({
    histSegs: segs,
    chatPayload,
    /* Production returns 304 once the live segment's ETag is held. Loading
       history changes only client state, so its repaint must omit that tag
       and obtain a body to rebuild from. */
    chatConditional: (_path, etag) => etag
      ? { status: 304, etag, data: null }
      : { status: 200, etag: '"live"', data: chatPayload },
  });
  ctx.feature.bind();
  await ctx.feature.render();
  assert.match(ctx.roots.msgs.innerHTML, /histload/);
  const before = ctx.apiCalls.filter(c => c.path.includes("/chat?history=1")).length;
  const hl = el("button", { className: "histload" });
  firstListener(ctx.roots.msgs, "click")({ target: hl });
  await new Promise(resolve => setImmediate(resolve));
  await new Promise(resolve => setImmediate(resolve));
  const histCalls = ctx.apiCalls.filter(c => c.path.includes("/chat?history=1"));
  assert.equal(histCalls.length, before + 1);
  assert.match(histCalls[histCalls.length - 1].path, /\/api\/nodes\/n1\/chat\?history=1$/);
  const liveCalls = ctx.apiCalls.filter(c =>
    c.kind === "conditional" && c.path.includes("/chat") && !c.path.includes("history"));
  assert.equal(liveCalls.length, 2);
  assert.equal(liveCalls[1].etag, "", "local history expansion must bypass the held live ETag");
  assert.match(ctx.roots.msgs.innerHTML, /turn hist|data-bk="h:/);
  ctx.feature.destroy();
});

test("P3C: open-terminal when closed opens pane and never POSTs /key", async () => {
  const payload = elicitationPayload();
  const nodes = [{
    id: "n1", title: "Alpha", agent: "claude", model: "sonnet",
    live: "quiet", attention: "question", lane_id: "L1", description: "d",
  }];
  const ctx = makeFeature({ nodes, chatPayload: payload });
  ctx.feature.bind();
  await ctx.feature.render();
  assert.equal(ctx.roots.termtoggle.classList.contains("on"), false);
  const beforeKeys = ctx.apiCalls.filter(c => c.path.includes("/key")).length;
  const open = el("button", { dataset: { openTerminal: "1" } });
  firstListener(ctx.roots.msgs, "click")({ target: open });
  await new Promise(resolve => setImmediate(resolve));
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(ctx.roots.termtoggle.classList.contains("on"), true,
    "closed terminal must open via data-open-terminal");
  assert.equal(ctx.apiCalls.filter(c => c.path.includes("/key")).length, beforeKeys,
    "Open Terminal must never POST /key");
  assert.ok(ctx.apiCalls.some(c => c.path.includes("/peek")),
    "opening the terminal fetches peek");
  ctx.feature.destroy();
});

test("P3C: image error removes .attthumb", async () => {
  const ctx = makeFeature();
  ctx.feature.bind();
  const thumb = el("a", { className: "attthumb", dataset: { tkey: "a1" } });
  const img = el("img");
  thumb.appendChild(img);
  ctx.roots.msgs.appendChild(thumb);
  assert.equal(thumb.parentNode, ctx.roots.msgs);
  firstListener(ctx.roots.msgs, "error")({ target: img });
  assert.equal(thumb.parentNode, null, "broken thumbnail anchor is removed");
  assert.equal(ctx.roots.msgs.children.includes(thumb), false);
  ctx.feature.destroy();
});

test("P3C: key-submit API failure alerts and restores attention row", async () => {
  const alerts = [];
  const nodes = [{
    id: "n1", title: "A", agent: "claude", model: "s", live: "quiet",
    attention: "question", lane_id: "", description: "",
  }];
  const payload = {
    turns: [{ role: "user", text: "q" }],
    live: "quiet", delivery: "ok", source: "tmux",
    chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
    attention: "question",
  };
  const ctx = makeFeature({
    nodes,
    chatPayload: payload,
    api: async (path) => {
      if (path.includes("/key")) throw new Error("key transport failed");
      if (path.includes("/peek")) return "pane";
      if (path.includes("/chat")) return payload;
      return {};
    },
    deps: { alert: msg => alerts.push(msg) },
  });
  ctx.feature.bind();
  await ctx.feature.render();
  assert.match(ctx.roots.keyrow.innerHTML, /data-key=/);
  const btn = el("button", { dataset: { key: "y" } });
  firstListener(ctx.roots.keyrow, "click")({ target: btn });
  assert.equal(ctx.roots.keyrow.innerHTML, "",
    "optimistic collapse clears the row before the POST settles");
  await new Promise(resolve => setImmediate(resolve));
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(alerts, ["key transport failed"]);
  assert.match(ctx.roots.keyrow.innerHTML, /data-key=/,
    "failure clears suppress and refreshChat restores the attention row");
  ctx.feature.destroy();
});

/* The Claude CLI now writes its transcript lazily, so the first prompt can be
   pasted and still be unlogged for a while. That neutral wait is delivery
   "delivering", deliberately distinct from "unconfirmed": the latter tells the
   user the send could not be confirmed and offers a resume button, which would
   be a lie while the launch is provably healthy. */
test("delivering: the neutral first-prompt wait is not the unconfirmed state", () => {
  const p = chatActivityPolicy({ delivery: "delivering", turnsLength: 1 });
  assert.equal(p.unconfirmed, false,
    "delivering must not present as an unconfirmed send");
  assert.equal(p.delivering, true);
  assert.equal(p.mustShowPane, false,
    "a healthy launch waiting for its transcript must never force the terminal open");
  const u = chatActivityPolicy({ delivery: "unconfirmed", turnsLength: 1 });
  assert.equal(u.unconfirmed, true, "an ordinary unconfirmed send is unchanged");
  assert.equal(u.delivering, false);
});

test("chat shell hosts only the Muse schema warning outside #msgs", () => {
  const indexSrc = readFileSync(join(__dirname, "../index.html"), "utf8");
	assert.doesNotMatch(indexSrc, /id="muse_chat_warn"/);
	assert.equal((indexSrc.match(/id="muse_schema_warn"/g) || []).length, 1);
  const msgsOpen = indexSrc.indexOf(`id="msgs"`);
	const warnAt = indexSrc.indexOf(`id="muse_schema_warn"`);
  assert.ok(warnAt >= 0 && msgsOpen >= 0);
  const afterMsgs = indexSrc.slice(msgsOpen, indexSrc.indexOf("</div>", msgsOpen));
	assert.doesNotMatch(afterMsgs, /muse_schema_warn/);
	assert.match(indexSrc, /id="muse_schema_warn"[^>]*class="nc-hint"/);
});

test("Muse schema mismatch is additive, nonfatal, and not interpolated", async () => {
  const mismatch = chatmod.MUSE_SCHEMA_MISMATCH_WARNING;
  assert.match(mismatch, /schema fingerprint differs from the version scimux tested/);
  const nodes = [{
    id: "n1", title: "Muse", agent: "muse", model: "synth-std",
    live: "active", attention: "", lane_id: "", description: "",
    muse_schema_warning: "muse_schema_fingerprint_mismatch",
  }];
  const { feature, roots } = makeFeature({
    nodes,
    chatPayload: {
      turns: [{ role: "assistant", text: "still here", time: "2026-01-01T00:00:00Z" }],
      live: "active", delivery: "ok", source: "acp",
      chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
    },
  });
  feature.bind();
  feature.renderHead();
  await feature.render();
	assert.equal(roots.muse_schema_warn.hidden, false);
	assert.match(roots.muse_schema_warn.textContent, /schema fingerprint/);
	assert.doesNotMatch(roots.muse_schema_warn.textContent, /muse_schema_fingerprint_mismatch/);
  assert.match(roots.msgs.innerHTML, /still here/);
  assert.equal(roots.msgs.innerHTML.includes("peek") && /force/.test(roots.msgs.innerHTML), false);

  nodes[0].muse_schema_warning = "some_other_code<script>";
  feature.renderHead();
	assert.equal(roots.muse_schema_warn.hidden, true);
	assert.equal(roots.muse_schema_warn.textContent, "");
});

test("Muse chat meta says context unknown until a real window arrives", async () => {
  const nodes = [{
    id: "n1", title: "Muse", agent: "muse", model: "synth-std",
    live: "quiet", attention: "", lane_id: "", description: "",
  }];
  const { feature, roots } = makeFeature({
    nodes,
    chatPayload: {
      turns: [{ role: "user", text: "hi", time: "2026-01-01T00:00:00Z" }],
      live: "quiet", delivery: "ok", source: "acp",
      chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
    },
  });
  feature.bind();
  feature.renderHead();
  assert.match(roots.chatmeta.textContent, /context unknown/);
  assert.doesNotMatch(roots.chatmeta.textContent, /context 0%/);
  await feature.render();
  assert.equal(roots.gauge.hidden, true);
  assert.match(roots.chatmeta.textContent, /context unknown/);
  assert.doesNotMatch(roots.chatmeta.textContent, /0%/);
});

test("Muse zero ctx_window stays unknown; a real window can show genuine 0%", async () => {
  let payload = {
    turns: [{ role: "user", text: "hi", time: "2026-01-01T00:00:00Z" }],
    live: "quiet", delivery: "ok", source: "acp",
    chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
    ctx_window: 0, ctx_pct: 0,
  };
  const { feature, roots } = makeFeature({
    nodes: [{
      id: "n1", title: "Muse", agent: "muse", model: "synth-std",
      live: "quiet", attention: "", lane_id: "", description: "",
    }],
    api: async (path) => {
      if (path.includes("/chat") && !path.includes("history")) return payload;
      return {};
    },
  });
  feature.bind();
  await feature.render();
  assert.equal(roots.gauge.hidden, true);
  assert.match(roots.chatmeta.textContent, /context unknown/);
  assert.doesNotMatch(roots.chatmeta.textContent, /context 0%/);

  payload = { ...payload, ctx_window: 128000, ctx_pct: 0 };
  feature.invalidate();
  await feature.render();
  assert.equal(roots.gauge.hidden, false);
  assert.match(roots.chatmeta.textContent, /context 0%/);
  assert.doesNotMatch(roots.chatmeta.textContent, /context unknown/);
});

test("sparse ACP approval shows the command before the choice", () => {
  const html = keyRowHTML({
    attention: "approval",
    source: "acp",
    permTitle: "bash `git status --short`",
    permToolKind: "execute",
    permOptions: [
      { key: "allow-once", name: "Allow once", kind: "allow" },
      { key: "allow-always-session", name: "Allow for session", kind: "allow_always" },
      { key: "allow-always-project", name: "Allow for project", kind: "allow_always" },
      { key: "reject-once", name: "Reject", kind: "reject" },
    ],
  });
  const commandAt = html.indexOf("git status --short");
  const buttonsAt = html.indexOf('class="permbtns"');
  assert.ok(commandAt >= 0 && buttonsAt > commandAt, "the command is on the card before the choices");
  assert.match(html, /<pre class="permcode">git status --short<\/pre>/);
  assert.match(html, /class="permverb"[^>]*>bash</);
  assert.match(html, /data-key="allow-always-session"/);
  assert.match(html, /data-key="allow-always-project"/);
  assert.doesNotMatch(html, /data-auto|auto-approve/i);
});

test("an unknown tool stays visible and still requires a human choice", () => {
  const html = keyRowHTML({
    attention: "approval",
    source: "acp",
    permTitle: "Unknown tool",
    permToolKind: "",
    permOptions: [
      { key: "allow-once", name: "Allow once", kind: "allow" },
      { key: "reject-once", name: "Reject", kind: "reject" },
    ],
  });
  const titleAt = html.indexOf("Unknown tool");
  const buttonsAt = html.indexOf('class="permbtns"');
  assert.ok(titleAt >= 0 && buttonsAt > titleAt);
  assert.match(html, /class="permtext"/);
  assert.match(html, /data-key="allow-once"/);
  assert.match(html, /data-key="reject-once"/);
  assert.doesNotMatch(html, /<pre class="permcode">/);
  assert.doesNotMatch(html, /data-auto|auto-approve/i);
});

test("Vibe chat meta names occupancy, token spend, and reported cost separately", async () => {
  const nodes = [{
    id: "n1", title: "Vibe", agent: "vibe", model: "synth",
    live: "quiet", attention: "", lane_id: "", description: "",
    fare_total: 42, fare_cost: 1.5, fare_cost_complete: true,
  }];
  let payload = {
    turns: [{ role: "user", text: "hi", time: "2026-01-01T00:00:00Z" }],
    live: "quiet", delivery: "ok", source: "acp",
    chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
  };
  const { feature, roots } = makeFeature({
    nodes,
    api: async (path) => {
      if (path.includes("/chat") && !path.includes("history")) return payload;
      return {};
    },
  });
  feature.bind();
  feature.renderHead();
  assert.match(roots.chatmeta.textContent, /^Mistral Vibe · synth/);
  assert.match(roots.chatmeta.textContent, /context occupancy unknown/);
  assert.match(roots.chatmeta.textContent, /token spend 42/);
  assert.match(roots.chatmeta.textContent, /Vibe-reported cost \$1\.50/);
  assert.doesNotMatch(roots.chatmeta.textContent, /context occupancy \d+%/);

  payload = { ...payload, ctx_window: 4000, ctx_pct: 25 };
  feature.invalidate();
  await feature.render();
  assert.equal(roots.gauge.hidden, false);
  assert.match(roots.chatmeta.textContent, /context occupancy 25%/);
  assert.match(roots.chatmeta.textContent, /token spend 42/);
  assert.match(roots.chatmeta.textContent, /Vibe-reported cost \$1\.50/);
  assert.doesNotMatch(roots.chatmeta.textContent, /context occupancy unknown/);

  nodes[0].fare_cost_complete = false;
  nodes[0].fare_cost = 0;
  delete nodes[0].fare_total;
  feature.renderHead();
  assert.match(roots.chatmeta.textContent, /context occupancy 25%/);
  assert.match(roots.chatmeta.textContent, /token spend unknown/);
  assert.match(roots.chatmeta.textContent, /Vibe-reported cost unknown/);
  assert.doesNotMatch(roots.chatmeta.textContent, /\$/);
});

test("other agents keep their context percentage and do not use Vibe's metric names", () => {
  const { feature, roots } = makeFeature({
    nodes: [{
      id: "n1", title: "Pi", agent: "pi", model: "synth",
      live: "quiet", attention: "", lane_id: "", description: "",
      fare_total: 42, fare_cost: 1.5, fare_cost_complete: true,
    }],
  });
  feature.bind();
  feature.renderHead();
  assert.match(roots.chatmeta.textContent, /^pi · synth/);
  assert.doesNotMatch(roots.chatmeta.textContent, /token spend|Vibe-reported|context occupancy/);
});

test("non-Muse context meta omits unknown and still hides the gauge without a window", async () => {
  const { feature, roots } = makeFeature({
    nodes: [{
      id: "n1", title: "Claude", agent: "claude", model: "sonnet",
      live: "quiet", attention: "", lane_id: "", description: "",
    }],
    chatPayload: {
      turns: [{ role: "user", text: "hi", time: "2026-01-01T00:00:00Z" }],
      live: "quiet", delivery: "ok", source: "tmux",
      chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
    },
  });
  feature.bind();
  feature.renderHead();
  await feature.render();
  assert.doesNotMatch(roots.chatmeta.textContent, /context unknown/);
  assert.doesNotMatch(roots.chatmeta.textContent, /context 0%/);
  assert.equal(roots.gauge.hidden, true);
});

test("pendingEmptyHTML: delivering says the prompt is in flight, not that Claude is starting", () => {
  /* SessionStart has already arrived by then, so the startup wording would be
     stale — that exact mismatch is what read as a stuck launch. */
  const html = pendingEmptyHTML({ pending: true, supervision: "claude_starting", delivering: true });
  assert.match(html, /delivering/i);
  assert.doesNotMatch(html, /SessionStart/);
  assert.doesNotMatch(html, /raw terminal/i);
  const strict = pendingEmptyHTML({ pending: true, supervision: "claude_strict", delivering: true });
  assert.match(strict, /delivering/i);
});

test("retry button retirement follows the stable failure reason", () => {
  for (const reason of ["not_found", "too_large"]) {
    assert.equal(chatmod.retryFailureRetiresButton(reason), true, reason);
  }
  for (const reason of ["", "unreadable", "storage", "outside_workspace", "not_regular", "chat_unavailable", "unknown"]) {
    assert.equal(chatmod.retryFailureRetiresButton(reason), false, reason);
  }
});

/* Inline attachment labels are substituted only after Markdown rendering. */
import { md } from "../js/format.js";

function inlineAssetResult(text, assets = {}, deps = {}){
  const split = splitAssetRefs(text, "n1", assets, deps);
  return { ...split, body: chatmod.renderMissingImportText(md(split.clean), split.labels) };
}

const inlineAssets = {
  a: { name: "overview.md" },
  b: { name: "chart.png", mime: "image/png" },
};

test("splitAssetRefs inline: mid-sentence file keeps label and one tile", () => {
  const r = inlineAssetResult("Created the [research overview](scimux-asset:a), using", inlineAssets);
  assert.equal(r.body, "<p>Created the research overview, using</p>");
  assert.equal((r.html.match(/<a /g) || []).length, 1);
  assert.match(r.html, /overview\.md/);
});

test("splitAssetRefs inline: mid-sentence image keeps alt and thumbnail", () => {
  const r = inlineAssetResult("See ![the diagram](scimux-asset:b) here", inlineAssets);
  assert.equal(r.body, "<p>See the diagram here</p>");
  assert.match(r.html, /class="attthumb"/);
});

test("splitAssetRefs inline: image alone has no body", () => {
  const r = splitAssetRefs("![the diagram](scimux-asset:b)", "n1", inlineAssets);
  assert.equal(r.clean, "");
  assert.deepEqual(r.labels, []);
  assert.match(r.html, /attthumb/);
});

for (const [name, newline] of [["LF", "\n"], ["CR", "\r"], ["CRLF", "\r\n"]]) {
  test(`splitAssetRefs inline: user attachment on its own line (${name})`, () => {
    const r = inlineAssetResult(`look${newline}${newline}[report.pdf](scimux-asset:a)`, inlineAssets);
    assert.equal(r.body, "<p>look</p>");
    assert.doesNotMatch(r.body, /report\.pdf/);
    assert.match(r.html, /attfile/);
    assert.deepEqual(r.labels, []);
  });
}

test("splitAssetRefs inline: refs and whitespace alone are all dropped", () => {
  const r = splitAssetRefs(" \t[a.md](scimux-asset:a) \t ![chart](scimux-asset:b) \t", "n1", inlineAssets);
  assert.equal(r.clean, "");
  assert.deepEqual(r.labels, []);
  assert.equal((r.html.match(/<a /g) || []).length, 2);
});

test("splitAssetRefs inline: list marker counts as text", () => {
  const r = inlineAssetResult("- [a.md](scimux-asset:a)", inlineAssets);
  assert.equal(r.body, "<ul><li>a.md</li></ul>");
});

for (const [name, label, assets, expected] of [
  ["empty label", "", inlineAssets, "overview.md"],
  ["whitespace label", " \t ", inlineAssets, "overview.md"],
  ["missing record", "", undefined, "file"],
  ["empty record name", "", { a: { name: "" } }, "file"],
]) {
  test(`splitAssetRefs inline: fallback for ${name}`, () => {
    const r = inlineAssetResult(`See [${label}](scimux-asset:a).`, assets);
    assert.equal(r.body, `<p>See ${expected}.</p>`);
  });
}

for (const [label, expected] of [
  ['<b>"x"</b>', '&lt;b&gt;&quot;x&quot;&lt;/b&gt;'],
  ["*x*", "*x*"],
  ["`x`", "`x`"],
  [" scimux-import:1:0:not_found scimux-asset:a ", " scimux-import:1:0:not_found scimux-asset:a "],
]) {
  test(`splitAssetRefs inline: inert label ${label}`, () => {
    const r = inlineAssetResult(`See [${label}](scimux-asset:a).`, inlineAssets);
    assert.equal(r.body, `<p>See ${expected}.</p>`);
    assert.doesNotMatch(r.body, /<b>|<em>|<code>|missingfile|attrow/);
    assert.equal(r.labels.length, 1);
  });
}

test("splitAssetRefs inline: source tokens survive without collisions", () => {
  const original = "\uE0000\uE001 \uE0001\uE001";
  const source = `${original} [overview](scimux-asset:a) and [diagram](scimux-asset:b)`;
  const r = inlineAssetResult(source, inlineAssets);
  assert.equal(r.body, `<p>${original} overview and diagram</p>`);
  assert.equal(new Set(r.labels.map(item => item.token)).size, 2);
  for (const item of r.labels) assert.equal(source.includes(item.token), false);
});

test("splitAssetRefs inline: asset and missing import tokens coexist", () => {
  const a = splitAssetRefs("See [the overview](scimux-asset:a) and [missing%20notes.md](scimux-import:1:0:not_found).", "n1", inlineAssets);
  const blocked = chatmod.splitImportRefs(a.clean, "n1");
  const body = chatmod.renderMissingImportText(
    chatmod.renderMissingImportText(md(blocked.clean), blocked.missing), a.labels);
  assert.equal(body, '<p>See the overview and <span class="missingfile">missing notes.md</span>.</p>');
  assert.notEqual(a.labels[0].token, blocked.missing[0].token);
});

test("splitAssetRefs inline: repeated id keeps both labels and one tile", () => {
  const r = inlineAssetResult("See [first](scimux-asset:a) then [second](scimux-asset:a).", inlineAssets);
  assert.equal(r.body, "<p>See first then second.</p>");
  assert.equal((r.html.match(/<a /g) || []).length, 1);
});

test("splitAssetRefs inline: distinct tiles follow first appearance", () => {
  const r = inlineAssetResult("[first](scimux-asset:a), [diagram](scimux-asset:b), [again](scimux-asset:a)", inlineAssets);
  assert.equal(r.body, "<p>first, diagram, again</p>");
  assert.deepEqual([...r.html.matchAll(/href="[^"]*\/assets\/([^"/]+)"/g)].map(m => m[1]), ["a", "b"]);
});

for (const text of ["plain text", "", null]) {
  test(`splitAssetRefs inline: stable no-ref shape for ${JSON.stringify(text)}`, () => {
    assert.deepEqual(splitAssetRefs(text, "n1", {}), { clean: text || "", html: "", labels: [] });
  });
}

test("splitAssetRefs inline: injected regex and escape apply to label", () => {
  const r = splitAssetRefs("See [overview](scimux-asset:a)", "n1", inlineAssets, {
    assetRefRe: /(!?)\[([^\]]*)\]\(scimux-asset:([\w-]+)\)/g,
    escape: value => `ESC(${value})`,
  });
  assert.equal(chatmod.renderMissingImportText(md(r.clean), r.labels), "<p>See ESC(overview)</p>");
});

test("splitAssetRefs inline: live render keeps media only for image-only turn", async () => {
  const ctx = makeFeature({ chatPayload: {
    ...defaultChatPayload(),
    turns: [
      { role: "assistant", text: "![diagram](scimux-asset:b)" },
      { role: "assistant", text: "See [research overview](scimux-asset:a), using" },
    ],
    assets: inlineAssets,
  } });
  ctx.feature.bind();
  await ctx.feature.render();
  const html = ctx.roots.msgs.innerHTML;
  assert.match(html, /class="turn assistant media"/);
  assert.match(html, /class="turn assistant"/);
  assert.match(html, /See research overview, using/);
  ctx.feature.destroy();
});

test("splitAssetRefs inline: absent asset map uses file fallback", () => {
  const r = splitAssetRefs("See [](scimux-asset:a).", "n1", null);
  assert.equal(chatmod.renderMissingImportText(md(r.clean), r.labels), "<p>See file.</p>");
  assert.match(r.html, /missing/);
});

test("assetBubbleBodyHTML: default markdown renders inline labels and tiles", () => {
  assert.equal(typeof chatmod.assetBubbleBodyHTML, "function", "assetBubbleBodyHTML must be exported");
  const html = chatmod.assetBubbleBodyHTML("Created **the** [*overview*](scimux-asset:a).", "n1", inlineAssets);
  assert.match(html, /^<p>Created <strong>the<\/strong> \*overview\*\.<\/p><div class="attrow">/);
  assert.doesNotMatch(html, /<em>/);
  assert.match(html, /download="overview\.md"/);
});

test("assetBubbleBodyHTML: injected markdown receives clean tokens", () => {
  assert.equal(typeof chatmod.assetBubbleBodyHTML, "function", "assetBubbleBodyHTML must be exported");
  let input;
  const html = chatmod.assetBubbleBodyHTML("See [<diagram>](scimux-asset:b).", "n1", inlineAssets, {
    markdown: clean => { input = clean; return `<section>${clean}</section>`; },
  });
  assert.doesNotMatch(input, /<diagram>|scimux-asset/);
  assert.match(html, /^<section>See &lt;diagram&gt;\.<\/section><div class="attrow">/);
  assert.match(html, /class="attthumb"/);
});

for (const text of ["", null]) {
  test(`assetBubbleBodyHTML: handles ${JSON.stringify(text)} text`, () => {
    assert.equal(typeof chatmod.assetBubbleBodyHTML, "function", "assetBubbleBodyHTML must be exported");
    assert.equal(chatmod.assetBubbleBodyHTML(text, "n1", {}), "");
  });
}

test("assetBubbleBodyHTML: preview delegates its bubble body", () => {
  const appSrc = readFileSync(join(__dirname, "../js/app.js"), "utf8");
  const start = appSrc.indexOf("const rows = turns.map");
  const preview = appSrc.slice(start, appSrc.indexOf('$("#previewbody").innerHTML', start));
  assert.match(preview, /\$\{assetBubbleBodyHTML\(/, "preview bubble must call assetBubbleBodyHTML(");
  assert.doesNotMatch(preview, /md\(s\.clean\)/);
});


const fencedAssetLine = '- want := "see [the report](scimux-asset:a)"';
const fencedAssetText = `\`\`\`diff\n${fencedAssetLine}\n\`\`\``;

function fencedCodeContents(html){
  return [...html.matchAll(/<pre><code>([\s\S]*?)<\/code><\/pre>/g)].map(m => m[1]);
}

test("splitAssetRefs fenced: known id stays verbatim", () => {
  const r = splitAssetRefs(fencedAssetText, "n1", inlineAssets);
  assert.deepEqual(fencedCodeContents(chatmod.assetBubbleBodyHTML(fencedAssetText, "n1", inlineAssets)), [esc(fencedAssetLine)]);
  assert.equal(r.clean, fencedAssetText);
  assert.equal(r.html, "");
  assert.deepEqual(r.labels, []);
});

test("splitAssetRefs fenced: unknown id makes no unavailable row", () => {
  const text = fencedAssetText.replaceAll("scimux-asset:a", "scimux-asset:unknown");
  const html = chatmod.assetBubbleBodyHTML(text, "n1", inlineAssets);
  assert.doesNotMatch(html, /unavailable|attrow/);
  assert.deepEqual(fencedCodeContents(html), [esc(fencedAssetLine.replaceAll("scimux-asset:a", "scimux-asset:unknown"))]);
  assert.equal(splitAssetRefs(text, "n1", inlineAssets).html, "");
});

test("splitAssetRefs fenced: standalone marker line is retained", () => {
  const line = "[x](scimux-asset:a)";
  const text = `\`\`\`\n${line}\n\`\`\``;
  assert.equal(splitAssetRefs(text, "n1", inlineAssets).clean, text);
  assert.deepEqual(fencedCodeContents(chatmod.assetBubbleBodyHTML(text, "n1", inlineAssets)), [esc(line)]);
});

test("splitAssetRefs fenced: blank lines stay inside and collapse outside", () => {
  const lines = `${fencedAssetLine}\n\n\n\nend`;
  const fence = `\`\`\`diff\n${lines}\n\`\`\``;
  const r = inlineAssetResult(`before\n\n\n\nSee [report](scimux-asset:a).\n${fence}\n\n\n\nafter`, inlineAssets);
  assert.deepEqual(fencedCodeContents(r.body), [esc(lines)]);
  assert.ok(r.clean.startsWith("before\n\nSee "));
  assert.ok(r.clean.endsWith("```\n\nafter"));
});

test("splitAssetRefs fenced: only outside occurrences create tiles", () => {
  const lines = `${fencedAssetLine}\n![diagram](scimux-asset:b)`;
  const fence = `\`\`\`diff\n${lines}\n\`\`\``;
  const r = inlineAssetResult(`${fence}\nSee [outside report](scimux-asset:a).`, inlineAssets);
  assert.deepEqual(fencedCodeContents(r.body), [esc(lines)]);
  assert.match(r.body, /<p>See outside report\.<\/p>/);
  assert.equal((r.html.match(/<a /g) || []).length, 1);
  assert.doesNotMatch(r.html, /chart\.png/);
});

test("splitAssetRefs fenced: unclosed fence includes appended attachment", () => {
  const lines = `sample\n\n[report.pdf](scimux-asset:a)\n\n`;
  const text = `\`\`\`text\n${lines}`;
  const r = splitAssetRefs(text, "n1", inlineAssets);
  assert.equal(r.clean, text);
  assert.equal(r.html, "");
  assert.deepEqual(r.labels, []);
  assert.deepEqual(fencedCodeContents(chatmod.assetBubbleBodyHTML(text, "n1", inlineAssets)), [esc(lines)]);
});

test("splitAssetRefs fenced: indented info opener and opener marker are preserved", () => {
  const text = "  ```go [opener](scimux-asset:a)\nvalue\n  ``` [closer](scimux-asset:b)";
  const r = splitAssetRefs(text, "n1", inlineAssets);
  assert.equal(r.clean, text);
  assert.equal(r.html, "");
  assert.deepEqual(r.labels, []);
});

for (const [name, newline] of [["CR", "\r"], ["CRLF", "\r\n"]]) {
  test(`splitAssetRefs fenced: ${name} renders like LF`, () => {
    const text = fencedAssetText.replaceAll("\n", newline);
    assert.equal(chatmod.assetBubbleBodyHTML(text, "n1", inlineAssets), md(fencedAssetText));
    assert.equal(splitAssetRefs(text, "n1", inlineAssets).clean, text);
  });
}

test("splitAssetRefs fenced: verbatim property across synthetic fixtures", () => {
  const marker = "[x](scimux-asset:a)";
  const fixtures = [
    [fencedAssetText, [fencedAssetLine]],
    [`\`\`\`\n${marker}\n\`\`\``, [marker]],
    [`  \`\`\`go\n${marker}\n  \`\`\``, [marker]],
    [`\`\`\`text ${marker}\n<&>\n\`\`\``, ["<&>"]],
    [`\`\`\`\n${marker}\n\n\n\nend\n\`\`\``, [`${marker}\n\n\n\nend`]],
    [`\`\`\`\n${marker}\n\n`, [`${marker}\n\n`]],
    [fencedAssetText.replaceAll("\n", "\r"), [fencedAssetLine]],
    [fencedAssetText.replaceAll("\n", "\r\n"), [fencedAssetLine]],
    [`${fencedAssetText}\nprose\n\`\`\`text\n${marker}\n\`\`\``, [fencedAssetLine, marker]],
    [`${fencedAssetText}\n\`\`\`text\n${marker}\n\`\`\``, [fencedAssetLine, marker]],
  ];
  for (const [text, contents] of fixtures) {
    assert.deepEqual(fencedCodeContents(chatmod.assetBubbleBodyHTML(text, "n1", inlineAssets)), contents.map(esc), text);
  }
});

test("splitAssetRefs fenced: synthetic report reproduction has six quoted markers", () => {
  const diff = Array.from({ length: 4 }, (_, i) => `- want := "here: [sketch ${i}](scimux-asset:quote_${i})"`).join("\n");
  const plain = "[report](scimux-asset:quote_4)\n![diagram](scimux-asset:quote_5)";
  const text = `The synthetic checks preserve the quoted fixtures.\n\n\`\`\`diff\n${diff}\n\`\`\`\n\nResults are recorded below.\n\n\`\`\`text\n${plain}\n\`\`\``;
  const html = chatmod.assetBubbleBodyHTML(text, "n1", {});
  assert.doesNotMatch(html, /<a |unavailable|attrow/);
  assert.deepEqual(fencedCodeContents(html), [esc(diff), esc(plain)]);
});

test("splitAssetRefs fenced: live render has no media class or attachment row", async () => {
  const ctx = makeFeature({ chatPayload: {
    ...defaultChatPayload(),
    turns: [{ role: "assistant", text: fencedAssetText }],
    assets: inlineAssets,
  } });
  ctx.feature.bind();
  await ctx.feature.render();
  const html = ctx.roots.msgs.innerHTML;
  assert.doesNotMatch(html, /attrow|class="turn assistant media"/);
  assert.match(html, /class="turn assistant"/);
  assert.deepEqual(fencedCodeContents(html), [esc(fencedAssetLine)]);
  ctx.feature.destroy();
});

/* Folding intermediate agent bubbles. The chat feature renders one surface
 * at a time; these tests seed the session-storage record and read the HTML.
 * Formatters are injected so the byte comparison does not depend on locale. */

function foldMemory(seed = {}){
  const map = new Map(Object.entries(seed));
  return {
    getItem(key){ return map.has(key) ? map.get(key) : null; },
    setItem(key, value){ map.set(key, String(value)); },
    map,
  };
}

function foldRecord(uid, state){
  return { ["scimux:fold:v1:" + uid]: JSON.stringify(state) };
}

function foldBubble(role, record, text, segment = 0, uid = "log-1"){
  return {
    role, text, uid, segment, record,
    time: "2026-01-01T00:" + String(record).padStart(2, "0") + ":00Z",
  };
}

function foldTurnHTML(i, role, time, text){
  return '\n      <div class="turn ' + role + '" data-i="' + i + '" data-bk="i:' + i + '">'
    + '\n\t\t<div class="bubble" title="' + role + "|" + time + '" tabindex="0">' + text + "</div>"
    + "\n      </div>";
}

const FOLD_BASE_HTML =
  '<div class="chatseam curseam"><span>chat started T(2026-01-01T00:00:00Z)</span></div>'
  + foldTurnHTML(0, "user", "2026-01-01T00:01:00Z", "prompt-one")
  + foldTurnHTML(1, "assistant", "2026-01-01T00:02:00Z", "alpha")
  + foldTurnHTML(2, "assistant", "2026-01-01T00:03:00Z", "beta")
  + foldTurnHTML(3, "assistant", "2026-01-01T00:04:00Z", "gamma")
  + foldTurnHTML(4, "assistant", "2026-01-01T00:05:00Z", "delta")
  + '<div class="pending chaterr" role="status">channel down</div>'
  + '\n      <div class="turn user echo">\n        <div class="bubble" title="you — delivering">in flight</div>\n      </div>';

function foldShowHTML(count, key){
  const noun = count === 1 ? "bubble" : "bubbles";
  return '<div class="foldrow"><button type="button" class="foldtoggle" data-fold="open" data-foldkey="'
    + key + '" aria-expanded="false">Show ' + count + " intermediate " + noun + "</button></div>";
}

function foldCloseHTML(count, key){
  const noun = count === 1 ? "bubble" : "bubbles";
  return '<div class="foldrow"><button type="button" class="foldtoggle" data-fold="close" data-foldkey="'
    + key + '" aria-expanded="true">Fold ' + count + " intermediate " + noun + "</button></div>";
}

function foldAgents(n, segment = 0, uid = "log-1", textAt = null){
  const turns = [foldBubble("user", 1, "prompt-one", segment, uid)];
  for (let i = 0; i < n; i++){
    const record = i + 2;
    const text = textAt ? textAt(record) : "a" + record;
    turns.push(foldBubble("assistant", record, text, segment, uid));
  }
  return turns;
}

async function renderFolded({
  turns, decisions, uid = "log-1", state = null, deps = {}, histSegs,
  prior = 0, echo = true, chatConditional,
} = {}){
  const store = foldMemory(state ? foldRecord(uid, state) : {});
  const chatPayload = {
    turns,
    live: "quiet",
    delivery: "ok",
    source: "tmux",
    chat_started: "2026-01-01T00:00:00Z",
    prior_turns: prior,
    assets: {},
    error: "channel down",
  };
  if (decisions) chatPayload.decisions = decisions;
  const ctx = makeFeature({
    histSegs,
    chatPayload,
    chatConditional,
    deps: {
      fmtWhen: t => "T(" + t + ")",
      bubbleTitle: (role, time) => role + "|" + time,
      md: s => String(s ?? ""),
      foldStorage: store,
      ...deps,
    },
  });
  if (echo){
    ctx.feature.setSentEcho({ node: "n1", text: "in flight", atts: [], at: 1_000_000, seen: null });
  }
  await ctx.feature.render();
  return { ctx, store, html: () => ctx.roots.msgs.innerHTML };
}

function assertFoldChrome(html, userText = "prompt-one"){
  assert.ok(html.includes("chat started T(2026-01-01T00:00:00Z)"), "seam");
  assert.ok(html.includes(userText), "user bubble");
  assert.ok(html.includes(">channel down<"), "pending error");
  assert.ok(html.includes(">in flight<"), "echo");
}

test("nothing folded is the base transcript, byte for byte", async () => {
  const { ctx, html } = await renderFolded({
    turns: foldAgents(4, 0, "log-1", record => ["alpha", "beta", "gamma", "delta"][record - 2]),
    state: { all: false, turns: [], open: [] },
  });
  assert.equal(html(), FOLD_BASE_HTML);
  ctx.feature.destroy();
});

test("a folded turn shows the first and latest bubbles and one row for the middle", async () => {
  const { ctx, html } = await renderFolded({
    turns: foldAgents(4, 0, "log-1", record => ["alpha", "beta", "gamma", "delta"][record - 2]),
    state: { all: false, turns: ["0/2"], open: [] },
  });
  const page = html();
  assertFoldChrome(page);
  assert.ok(page.includes(">alpha<"));
  assert.ok(page.includes(">delta<"));
  assert.equal(page.includes(">beta<"), false);
  assert.equal(page.includes(">gamma<"), false);
  assert.ok(page.includes(foldShowHTML(2, "0/3")));
  assert.equal(page.includes("foldtoggle"), true);
  ctx.feature.destroy();
});

test("one of two ranges can be open, and folding it again hides it", async () => {
  const turns = [
    foldBubble("user", 1, "prompt-one"),
    foldBubble("assistant", 2, "first"),
    foldBubble("assistant", 3, "left-a"),
    foldBubble("assistant", 4, "left-b"),
    foldBubble("assistant", 5, "kept"),
    foldBubble("assistant", 6, "right-c"),
    foldBubble("assistant", 7, "right-d"),
    foldBubble("assistant", 8, "last"),
  ];
  const { ctx, store, html } = await renderFolded({
    turns,
    state: { all: false, turns: ["0/2"], open: ["0/6", "0/7"] },
    deps: {
      bookmarks: () => [{ uid: "log-1", segment: 0, record: 5, text: "kept" }],
      markerVersion: () => 1,
    },
  });
  let page = html();
  assertFoldChrome(page);
  assert.ok(page.includes(foldShowHTML(2, "0/3")));
  assert.equal(page.includes(">left-a<"), false);
  assert.equal(page.includes(">left-b<"), false);
  assert.ok(page.includes(">kept<"));
  assert.ok(page.includes(foldCloseHTML(2, "0/6")));
  assert.ok(page.includes(">right-c<"));
  assert.ok(page.includes(">right-d<"));
  assert.ok(page.includes(">last<"));
  assert.equal(page.split('data-fold="close"').length - 1, 1);

  store.setItem("scimux:fold:v1:log-1", JSON.stringify({
    all: false, turns: ["0/2"], open: [],
  }));
  await ctx.feature.render();
  page = html();
  assert.ok(page.includes(foldShowHTML(2, "0/3")));
  assert.ok(page.includes(foldShowHTML(2, "0/6")));
  assert.equal(page.includes(">right-c<"), false);
  assert.equal(page.includes(">right-d<"), false);
  assert.ok(page.includes(">kept<"));
  assert.ok(page.includes(">first<"));
  assert.ok(page.includes(">last<"));
  ctx.feature.destroy();
});

test("eight open bubbles repeat the Fold control under the range", async () => {
  const open = [];
  for (let record = 3; record <= 10; record++) open.push("0/" + record);
  const { ctx, html } = await renderFolded({
    turns: foldAgents(10),
    state: { all: false, turns: ["0/2"], open },
  });
  const page = html();
  assertFoldChrome(page);
  assert.equal(page.split(foldCloseHTML(8, "0/3")).length - 1, 2);
  assert.ok(page.includes(">a3<"));
  assert.ok(page.includes(">a10<"));
  assert.ok(page.includes(">a11<"));
  ctx.feature.destroy();
});

test("a bookmark keeps its bubble visible and splits the range", async () => {
  const turns = foldAgents(6);
  turns[3].text = "marked";
  const { ctx, html } = await renderFolded({
    turns,
    state: { all: false, turns: ["0/2"], open: [] },
    deps: {
      bookmarks: () => [{ uid: "log-1", segment: 0, record: 4, text: "marked" }],
      markerVersion: () => 1,
    },
  });
  const page = html();
  assertFoldChrome(page);
  assert.equal(page.includes(">a3<"), false);
  assert.ok(page.includes(">marked<"));
  assert.equal(page.includes(">a5<"), false);
  assert.ok(page.includes(foldShowHTML(1, "0/3")));
  assert.ok(page.includes(foldShowHTML(2, "0/5")));
  assert.ok(page.includes(">a2<"));
  assert.ok(page.includes(">a7<"));
  ctx.feature.destroy();
});

test("a note usage keeps its bubble visible and splits the range", async () => {
  const turns = foldAgents(6);
  turns[3].text = "noted";
  const { ctx, html } = await renderFolded({
    turns,
    state: { all: false, turns: ["0/2"], open: [] },
    deps: {
      noteUsages: () => [{
        note_id: "note-1", section_id: "sec-1",
        source: { uid: "log-1", segment: 0, record: 4 },
      }],
      markerVersion: () => 1,
    },
  });
  const page = html();
  assertFoldChrome(page);
  assert.equal(page.includes(">a3<"), false);
  assert.ok(page.includes(">noted<"));
  assert.equal(page.includes(">a5<"), false);
  assert.ok(page.includes(foldShowHTML(1, "0/3")));
  assert.ok(page.includes(foldShowHTML(2, "0/5")));
  ctx.feature.destroy();
});

test("a sent-to link keeps its bubble visible and splits the range", async () => {
  const turns = foldAgents(6);
  turns[3].text = "sent";
  const { ctx, html } = await renderFolded({
    turns,
    state: { all: false, turns: ["0/2"], open: [] },
    deps: {
      forwardLinks: () => [{
        source: { uid: "log-1", segment: 0, record: 4 },
        destination: { node: "other", text: "there" },
      }],
      markerVersion: () => 1,
    },
  });
  const page = html();
  assertFoldChrome(page);
  assert.equal(page.includes(">a3<"), false);
  assert.ok(page.includes(">sent<"));
  assert.equal(page.includes(">a5<"), false);
  assert.ok(page.includes(foldShowHTML(1, "0/3")));
  assert.ok(page.includes(foldShowHTML(2, "0/5")));
  ctx.feature.destroy();
});

test("adding a bookmark to a hidden bubble shows it on the next render", async () => {
  const turns = foldAgents(5);
  turns[2].text = "left";
  turns[3].text = "hidden-middle";
  turns[4].text = "right";
  let bookmarks = [];
  let markerVersion = 1;
  const { ctx, html } = await renderFolded({
    turns,
    state: { all: false, turns: ["0/2"], open: [] },
    deps: {
      bookmarks: () => bookmarks,
      markerVersion: () => markerVersion,
    },
  });
  assert.equal(html().includes(">hidden-middle<"), false);
  assert.ok(html().includes(foldShowHTML(3, "0/3")));
  bookmarks = [{ uid: "log-1", segment: 0, record: 4, text: "hidden-middle" }];
  markerVersion = 2;
  await ctx.feature.render();
  const page = html();
  assert.ok(page.includes(">hidden-middle<"));
  assert.equal(page.includes(">left<"), false);
  assert.equal(page.includes(">right<"), false);
  assert.ok(page.includes(foldShowHTML(1, "0/3")));
  assert.ok(page.includes(foldShowHTML(1, "0/5")));
  assertFoldChrome(page);
  ctx.feature.destroy();
});

test("a decision inside a folded range is hidden and a decision at the edge stays", async () => {
  const turns = [
    foldBubble("user", 1, "prompt-one"),
    foldBubble("assistant", 2, "first"),
    foldBubble("assistant", 4, "mid-a"),
    foldBubble("assistant", 6, "mid-b"),
    foldBubble("assistant", 8, "last"),
  ];
  const decision = token => ({
    record: token === "edge-keep" ? 3 : token === "inside-hide" ? 5 : 7,
    time: "2026-01-01T00:00:30Z",
    decision: {
      title: token,
      request_id: token,
      selected: { key: "1", name: "Allow", kind: "allow" },
    },
  });
  const { ctx, html } = await renderFolded({
    turns,
    decisions: [decision("edge-keep"), decision("inside-hide"), decision("edge-after")],
    state: { all: false, turns: ["0/2"], open: [] },
  });
  const page = html();
  assertFoldChrome(page);
  assert.ok(page.includes("edge-keep"));
  assert.ok(page.includes("edge-after"));
  assert.equal(page.includes("inside-hide"), false);
  assert.equal(page.includes(">mid-a<"), false);
  assert.equal(page.includes(">mid-b<"), false);
  assert.ok(page.includes(foldShowHTML(2, "0/4")));
  assert.ok(page.includes(">first<"));
  assert.ok(page.includes(">last<"));
  ctx.feature.destroy();
});

test("a poll that adds a bubble to a folded turn hides the bubble that was latest", async () => {
  const payload = {
    turns: foldAgents(3, 0, "log-1", record => ["alpha", "beta", "gamma"][record - 2]),
    live: "quiet",
    delivery: "ok",
    source: "tmux",
    chat_started: "2026-01-01T00:00:00Z",
    prior_turns: 0,
    assets: {},
    error: "channel down",
  };
  const store = foldMemory(foldRecord("log-1", { all: false, turns: ["0/2"], open: [] }));
  const ctx = makeFeature({
    chatPayload: payload,
    deps: {
      fmtWhen: t => "T(" + t + ")",
      bubbleTitle: (role, time) => role + "|" + time,
      md: s => String(s ?? ""),
      foldStorage: store,
    },
  });
  ctx.feature.setSentEcho({ node: "n1", text: "in flight", atts: [], at: 1_000_000, seen: null });
  await ctx.feature.render();
  assert.ok(ctx.roots.msgs.innerHTML.includes(">alpha<"));
  assert.equal(ctx.roots.msgs.innerHTML.includes(">beta<"), false);
  assert.ok(ctx.roots.msgs.innerHTML.includes(">gamma<"));
  assert.ok(ctx.roots.msgs.innerHTML.includes(foldShowHTML(1, "0/3")));
  payload.turns = payload.turns.concat([
    foldBubble("assistant", 5, "delta"),
  ]);
  await ctx.feature.render();
  const page = ctx.roots.msgs.innerHTML;
  assertFoldChrome(page);
  assert.ok(page.includes(">alpha<"));
  assert.ok(page.includes(">delta<"));
  assert.equal(page.includes(">beta<"), false);
  assert.equal(page.includes(">gamma<"), false);
  assert.ok(page.includes(foldShowHTML(2, "0/3")));
  ctx.feature.destroy();
});

test("a poll without changes does not rebuild a folded transcript", async () => {
  const { ctx, html } = await renderFolded({
    turns: foldAgents(4, 0, "log-1", record => ["alpha", "beta", "gamma", "delta"][record - 2]),
    state: { all: false, turns: ["0/2"], open: [] },
  });
  const page = html();
  assert.ok(page.includes(foldShowHTML(2, "0/3")));
  ctx.roots.msgs.innerHTML = page + "<!--hold-->";
  await ctx.feature.render();
  assert.ok(html().includes("<!--hold-->"));
  assert.ok(html().includes(foldShowHTML(2, "0/3")));
  ctx.feature.destroy();
});

test("a change of the fold state rebuilds the transcript", async () => {
  const { ctx, store, html } = await renderFolded({
    turns: foldAgents(4, 0, "log-1", record => ["alpha", "beta", "gamma", "delta"][record - 2]),
    state: { all: false, turns: [], open: [] },
  });
  assert.equal(html().includes("foldtoggle"), false);
  assert.ok(html().includes(">beta<"));
  ctx.roots.msgs.innerHTML = html() + "<!--hold-->";
  store.setItem("scimux:fold:v1:log-1", JSON.stringify({
    all: false, turns: ["0/2"], open: [],
  }));
  await ctx.feature.render();
  const page = html();
  assert.equal(page.includes("<!--hold-->"), false);
  assert.ok(page.includes(foldShowHTML(2, "0/3")));
  assert.equal(page.includes(">beta<"), false);
  assertFoldChrome(page);
  ctx.feature.destroy();
});

test("a loaded history surface folds on its own", async () => {
  const { ctx, html } = await renderFolded({
    turns: foldAgents(3, 1, "log-1", record => "live-" + record),
    histSegs: [
      { start: "2025-01-01T00:00:00Z", seam: "2025-01-01T00:00:00Z", reason: "clear" },
      {
        start: "2025-06-01T00:00:00Z",
        seam: "2025-06-01T00:00:00Z",
        reason: "clear",
        turns: foldAgents(3, 0, "log-1", record => "hist-" + record),
      },
    ],
    prior: 1,
    state: { all: false, turns: ["0/2"], open: [] },
  });
  await ctx.feature.loadHistory("n1", "");
  const page = html();
  assertFoldChrome(page);
  assert.equal(page.includes(">hist-3<"), false);
  assert.ok(page.includes(">hist-2<"));
  assert.ok(page.includes(">hist-4<"));
  assert.ok(page.includes(">live-3<"));
  assert.ok(page.includes(foldShowHTML(1, "0/3")));
  assert.equal(page.includes(foldShowHTML(1, "1/3")), false);
  ctx.feature.destroy();
});

test("the fold state survives a new chat feature on the same storage", async () => {
  const store = foldMemory(foldRecord("log-1", { all: false, turns: ["0/2"], open: [] }));
  const turns = foldAgents(4, 0, "log-1", record => ["alpha", "beta", "gamma", "delta"][record - 2]);
  const deps = {
    fmtWhen: t => "T(" + t + ")",
    bubbleTitle: (role, time) => role + "|" + time,
    md: s => String(s ?? ""),
    foldStorage: store,
  };
  const payload = {
    turns,
    live: "quiet", delivery: "ok", source: "tmux",
    chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
    error: "channel down",
  };
  const first = makeFeature({ chatPayload: payload, deps });
  first.feature.setSentEcho({ node: "n1", text: "in flight", atts: [], at: 1_000_000, seen: null });
  await first.feature.render();
  assert.ok(first.roots.msgs.innerHTML.includes(foldShowHTML(2, "0/3")));
  first.feature.destroy();
  const second = makeFeature({ chatPayload: payload, deps });
  second.feature.setSentEcho({ node: "n1", text: "in flight", atts: [], at: 1_000_000, seen: null });
  await second.feature.render();
  assert.ok(second.roots.msgs.innerHTML.includes(foldShowHTML(2, "0/3")));
  assert.equal(second.roots.msgs.innerHTML.includes(">beta<"), false);
  second.feature.destroy();
});

test("fold state is separate per chat uid", async () => {
  const store = foldMemory(foldRecord("log-a", { all: false, turns: ["0/2"], open: [] }));
  const deps = {
    fmtWhen: t => "T(" + t + ")",
    bubbleTitle: (role, time) => role + "|" + time,
    md: s => String(s ?? ""),
    foldStorage: store,
  };
  const payloadFor = uid => ({
    turns: foldAgents(4, 0, uid, record => uid + "-" + record),
    live: "quiet", delivery: "ok", source: "tmux",
    chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
    error: "channel down",
  });
  const folded = makeFeature({ chatPayload: payloadFor("log-a"), deps });
  await folded.feature.render();
  assert.equal(folded.roots.msgs.innerHTML.includes(">log-a-3<"), false);
  assert.ok(folded.roots.msgs.innerHTML.includes(foldShowHTML(2, "0/3")));
  folded.feature.destroy();
  const other = makeFeature({ chatPayload: payloadFor("log-b"), deps });
  await other.feature.render();
  const page = other.roots.msgs.innerHTML;
  assert.ok(page.includes(">log-b-3<"));
  assert.ok(page.includes(">log-b-4<"));
  assert.equal(page.includes("foldtoggle"), false);
  other.feature.destroy();
});

test("a chat without a uid shows no fold control and does not touch storage", async () => {
  let reads = 0;
  const store = {
    getItem(){ reads += 1; return '{"all":true,"turns":[],"open":[]}'; },
    setItem(){ reads += 1; },
  };
  const turns = [1, 2, 3, 4, 5].map(record => {
    const role = record === 1 ? "user" : "assistant";
    const text = record === 1 ? "prompt-one" : "body-" + record;
    return {
      role, text, segment: 0, record,
      time: "2026-01-01T00:0" + record + ":00Z",
    };
  });
  const { ctx, html } = await renderFolded({
    turns,
    deps: { foldStorage: store },
  });
  const page = html();
  assertFoldChrome(page);
  assert.ok(page.includes(">body-3<"));
  assert.equal(page.includes("foldtoggle"), false);
  assert.equal(reads, 0);
  ctx.feature.destroy();
});

/* Fold actions, the chat-head control, and the reading position.
 * Disclosure clicks are synthetic: the fake DOM does not parse innerHTML
 * into buttons, and closest() only recognises [data-fold] when the target
 * carries dataset.fold. Scroll positions are assigned, not laid out. */

const FOLD_ACTION_HTML =
  '<div class="bubwhen">TIME</div>' +
  '<button data-bact="fork"><span>B</span>fork from here</button>' +
  '<button data-bact="sendto"><span class="rot180">I</span>send to&#8230;</button>' +
  '<button data-bact="desc"><span class="rot90l">I</span>use as description</button>' +
  '<button data-bact="bookmark"><span>I</span>bookmark</button>' +
  '<button data-bact="copy"><span>C</span>copy</button>';

const FOLD_ICONS = {
  ICON_TERM: "T", ICON_CHECK: "✓", ICON_CHEV_UP: "↑", ICON_CHEV_DOWN: "↓",
  ICON_BRANCH: "B", ICON_INTO: "I", ICON_COPY: "C", ICON_DOWNALL: "↓↓",
  ICON_FILE: "F", ICON_FOLD: "<svg fold>",
};

function foldSettle(){
  return new Promise(resolve => setImmediate(resolve));
}

function twoRangeTurns(){
  return [
    foldBubble("user", 1, "prompt-one"),
    foldBubble("assistant", 2, "first"),
    foldBubble("assistant", 3, "left-a"),
    foldBubble("assistant", 4, "left-b"),
    foldBubble("assistant", 5, "kept"),
    foldBubble("assistant", 6, "right-c"),
    foldBubble("assistant", 7, "right-d"),
    foldBubble("assistant", 8, "last"),
  ];
}

function twoRangeDeps(){
  return {
    bookmarks: () => [{ uid: "log-1", segment: 0, record: 5, text: "kept" }],
    markerVersion: () => 1,
    icons: FOLD_ICONS,
  };
}

function readStoredFold(store, uid = "log-1"){
  const text = store.getItem("scimux:fold:v1:" + uid);
  return text ? JSON.parse(text) : null;
}

function armFoldScroll(msgs, top = 800){
  let html = msgs.innerHTML;
  Object.defineProperty(msgs, "innerHTML", {
    configurable: true,
    get(){ return html; },
    set(v){
      html = String(v);
      msgs.children.length = 0;
      const btnRe = /data-fold="(open|close)" data-foldkey="([^"]+)"/g;
      let m;
      while ((m = btnRe.exec(html))){
        const btn = el("button", {
          className: "foldtoggle",
          dataset: { fold: m[1], foldkey: m[2] },
        });
        btn.offsetTop = top;
        const row = el("div", { className: "foldrow" });
        row.appendChild(btn);
        msgs.appendChild(row);
      }
      const turnRe = /class="(turn [^"]*)"[^>]*data-bk="([^"]+)"/g;
      let t;
      while ((t = turnRe.exec(html))){
        const turn = el("div", { className: t[1], dataset: { bk: t[2] } });
        turn.offsetTop = top;
        msgs.appendChild(turn);
      }
    },
  });
}

function primeFoldScroll(ctx, scrollTop, replacementTop = 800){
  const msgs = ctx.roots.msgs;
  msgs.scrollHeight = 2000;
  msgs.clientHeight = 400;
  msgs.scrollTop = scrollTop;
  armFoldScroll(msgs, replacementTop);
}

function clickFoldRow(ctx, kind, key, offsetTop){
  const btn = el("button", { dataset: { fold: kind, foldkey: key } });
  if (offsetTop != null) btn.offsetTop = offsetTop;
  firstListener(ctx.roots.msgs, "click")({ target: btn });
  return btn;
}

test("bubbleActionsHTML without a fold option is unchanged", () => {
  const html = bubbleActionsHTML({ time: "2026-07-24T09:05:00" }, {
    fmtTime: () => "TIME",
    iconBranch: "B", iconInto: "I", iconCopy: "C",
  });
  assert.equal(html, FOLD_ACTION_HTML);
});

test("bubbleActionsHTML adds the fold action after copy when asked", () => {
  const html = bubbleActionsHTML({ time: "2026-07-24T09:05:00" }, {
    fmtTime: () => "TIME",
    iconBranch: "B", iconInto: "I", iconCopy: "C",
    fold: { icon: "<svg fold>" },
  });
  const bare = bubbleActionsHTML({ time: "2026-07-24T09:05:00" }, {
    fmtTime: () => "TIME",
    iconBranch: "B", iconInto: "I", iconCopy: "C",
    fold: {},
  });
  const button = '<button data-bact="fold" aria-label="Fold intermediate agent bubbles" ' +
    'title="Fold intermediate agent bubbles"><span><svg fold></span></button>';
  assert.equal(html, FOLD_ACTION_HTML + button);
  assert.ok(html.indexOf('data-bact="copy"') < html.indexOf('data-bact="fold"'));
  assert.equal(bare, FOLD_ACTION_HTML + button.replace("<svg fold>", ""));
});

test("the fold action is last and only on the first agent bubble of a foldable turn", async () => {
  const { ctx } = await renderFolded({
    turns: foldAgents(4),
    state: { all: false, turns: [], open: [] },
    echo: false,
    deps: { icons: FOLD_ICONS },
  });
  ctx.feature.bind();
  tapBubbleTurn(ctx, "i:1");
  const first = bubActionRows(ctx)[0].innerHTML;
  assert.match(first, /data-bact="fold"/);
  assert.ok(first.indexOf('data-bact="copy"') < first.indexOf('data-bact="fold"'));
  assert.match(first, /<span><svg fold><\/span>/);
  tapBubbleTurn(ctx, "i:1");
  tapBubbleTurn(ctx, "i:2");
  assert.doesNotMatch(bubActionRows(ctx)[0].innerHTML, /data-bact="fold"/);
  tapBubbleTurn(ctx, "i:2");
  tapBubbleTurn(ctx, "i:0");
  assert.doesNotMatch(bubActionRows(ctx)[0].innerHTML, /data-bact="fold"/);
  ctx.feature.destroy();
});

test("a short turn has no fold action and the head button stays hidden", async () => {
  const { ctx } = await renderFolded({
    turns: foldAgents(2),
    state: { all: false, turns: [], open: [] },
    echo: false,
  });
  ctx.feature.bind();
  tapBubbleTurn(ctx, "i:1");
  assert.doesNotMatch(bubActionRows(ctx)[0].innerHTML, /data-bact="fold"/);
  assert.equal(ctx.roots.chatfoldall.hidden, true);
  ctx.feature.destroy();
});

test("a fully folded turn has no fold action and the head button stays hidden", async () => {
  const { ctx, html } = await renderFolded({
    turns: foldAgents(4),
    state: { all: false, turns: ["0/2"], open: [] },
    echo: false,
  });
  ctx.feature.bind();
  tapBubbleTurn(ctx, "i:1");
  assert.equal(html().includes(">a3<"), false);
  assert.doesNotMatch(bubActionRows(ctx)[0].innerHTML, /data-bact="fold"/);
  assert.equal(ctx.roots.chatfoldall.hidden, true);
  ctx.feature.destroy();
});

test("a fold click on a bubble that cannot fold leaves the bar and the transcript", async () => {
  const { ctx, store, html } = await renderFolded({
    turns: foldAgents(4),
    echo: false,
  });
  ctx.feature.bind();
  tapBubbleTurn(ctx, "i:0");
  clickBact(ctx, "fold");
  await foldSettle();
  assert.equal(bubActionRows(ctx).length, 1);
  assert.ok(html().includes(">a3<"));
  assert.equal(readStoredFold(store), null);
  ctx.feature.destroy();
});

test("the fold action folds that turn, drops only its open keys, and keeps the bubble put", async () => {
  const turns = [
    foldBubble("user", 1, "prompt-one"),
    foldBubble("assistant", 2, "a2"),
    foldBubble("assistant", 3, "a3"),
    foldBubble("assistant", 4, "a4"),
    foldBubble("assistant", 5, "a5"),
    foldBubble("user", 10, "prompt-two"),
    foldBubble("assistant", 11, "b11"),
    foldBubble("assistant", 12, "b12"),
    foldBubble("assistant", 13, "b13"),
    foldBubble("assistant", 14, "b14"),
  ];
  const { ctx, store, html } = await renderFolded({
    turns,
    state: { all: false, turns: [], open: ["0/3", "0/12"] },
    echo: false,
    deps: { icons: FOLD_ICONS },
  });
  ctx.feature.bind();
  const turn = tapBubbleTurn(ctx, "i:1");
  turn.offsetTop = 500;
  primeFoldScroll(ctx, 40);
  clickBact(ctx, "fold");
  await foldSettle();
  const page = html();
  assert.equal(page.includes(">a3<"), false);
  assert.equal(page.includes(">a4<"), false);
  assert.ok(page.includes(">a2<"));
  assert.ok(page.includes(">a5<"));
  assert.ok(page.includes(">b12<"));
  assert.ok(page.includes(foldShowHTML(2, "0/3")));
  assert.equal(bubActionRows(ctx).length, 0);
  assert.deepEqual(readStoredFold(store), {
    all: false, turns: ["0/2"], open: ["0/12"],
  });
  assert.equal(ctx.roots.msgs.scrollTop, 340);
  ctx.feature.destroy();
});

test("a Show click opens that range only and keeps the row at the same distance", async () => {
  const { ctx, store, html } = await renderFolded({
    turns: twoRangeTurns(),
    state: { all: false, turns: ["0/2"], open: [] },
    echo: false,
    deps: twoRangeDeps(),
  });
  ctx.feature.bind();
  assert.equal(ctx.roots.chatfoldall.hidden, true);
  primeFoldScroll(ctx, 40);
  clickFoldRow(ctx, "open", "0/3", 500);
  await foldSettle();
  const page = html();
  assert.ok(page.includes(">left-a<"));
  assert.ok(page.includes(">left-b<"));
  assert.equal(page.includes(">right-c<"), false);
  assert.equal(page.includes(">right-d<"), false);
  assert.ok(page.includes(foldCloseHTML(2, "0/3")));
  assert.ok(page.includes(foldShowHTML(2, "0/6")));
  assert.deepEqual(readStoredFold(store).open, ["0/3", "0/4"]);
  assert.deepEqual(readStoredFold(store).turns, ["0/2"]);
  assert.equal(ctx.roots.msgs.scrollTop, 340);
  assert.equal(ctx.roots.chatfoldall.hidden, false);
  ctx.feature.destroy();
});

test("a Fold click hides that range and keeps the row at the same distance", async () => {
  const { ctx, store, html } = await renderFolded({
    turns: twoRangeTurns(),
    state: { all: false, turns: ["0/2"], open: ["0/3", "0/4"] },
    echo: false,
    deps: twoRangeDeps(),
  });
  ctx.feature.bind();
  primeFoldScroll(ctx, 0);
  clickFoldRow(ctx, "close", "0/3", 500);
  await foldSettle();
  const page = html();
  assert.equal(page.includes(">left-a<"), false);
  assert.equal(page.includes(">left-b<"), false);
  assert.equal(page.includes(">right-c<"), false);
  assert.ok(page.includes(foldShowHTML(2, "0/3")));
  assert.ok(page.includes(foldShowHTML(2, "0/6")));
  assert.deepEqual(readStoredFold(store).open, []);
  assert.equal(ctx.roots.msgs.scrollTop, 300);
  ctx.feature.destroy();
});

test("a reader at the bottom stays there when a range opens", async () => {
  const { ctx, html } = await renderFolded({
    turns: twoRangeTurns(),
    state: { all: false, turns: ["0/2"], open: [] },
    echo: false,
    deps: twoRangeDeps(),
  });
  ctx.feature.bind();
  primeFoldScroll(ctx, 1550, 100);
  clickFoldRow(ctx, "open", "0/3", 500);
  await foldSettle();
  assert.ok(html().includes(">left-a<"));
  assert.equal(ctx.roots.msgs.scrollTop, 1600);
  ctx.feature.destroy();
});

test("opening a range leaves the reader put when the new row has no position", async () => {
  const { ctx, html } = await renderFolded({
    turns: twoRangeTurns(),
    state: { all: false, turns: ["0/2"], open: [] },
    echo: false,
    deps: twoRangeDeps(),
  });
  ctx.feature.bind();
  ctx.roots.msgs.scrollHeight = 2000;
  ctx.roots.msgs.clientHeight = 400;
  ctx.roots.msgs.scrollTop = 40;
  clickFoldRow(ctx, "open", "0/3", 500);
  await foldSettle();
  assert.ok(html().includes(">left-a<"));
  assert.equal(ctx.roots.msgs.scrollTop, 40);
  ctx.feature.destroy();
});

test("the head button shows when a foldable bubble is visible", async () => {
  const { ctx } = await renderFolded({
    turns: foldAgents(4),
    state: { all: false, turns: [], open: [] },
    echo: false,
    deps: { icons: FOLD_ICONS },
  });
  assert.equal(ctx.roots.chatfoldall.hidden, false);
  assert.equal(ctx.roots.chatfoldall.innerHTML, "<svg fold>Fold all intermediate agent bubbles");
  ctx.feature.destroy();
});

test("the head button shows when only a loaded history surface can fold", async () => {
  const store = foldMemory();
  const ctx = makeFeature({
    histSegs: [{
      start: "2025-06-01T00:00:00Z",
      seam: "2025-06-01T00:00:00Z",
      reason: "clear",
      turns: foldAgents(4, 0, "log-1", record => "hist-" + record),
    }],
    chatPayload: {
      turns: foldAgents(2, 1, "log-1", record => "live-" + record),
      live: "quiet", delivery: "ok", source: "tmux",
      chat_started: "2026-01-01T00:00:00Z",
      prior_turns: 1, assets: {},
    },
    deps: {
      fmtWhen: t => "T(" + t + ")",
      bubbleTitle: (role, time) => role + "|" + time,
      md: s => String(s ?? ""),
      foldStorage: store,
      icons: FOLD_ICONS,
    },
  });
  await ctx.feature.loadHistory("n1", "");
  assert.equal(ctx.roots.chatfoldall.hidden, false);
  ctx.feature.destroy();
});

test("a chat without a uid keeps the head button hidden", async () => {
  const turns = [1, 2, 3, 4].map(record => ({
    role: record === 1 ? "user" : "assistant",
    text: record === 1 ? "prompt-one" : "body-" + record,
    segment: 0, record,
    time: "2026-01-01T00:0" + record + ":00Z",
  }));
  const { ctx } = await renderFolded({ turns, echo: false });
  assert.equal(ctx.roots.chatfoldall.hidden, true);
  ctx.feature.destroy();
});

test("folding the whole chat hides opened ranges and does not offer to open everything", async () => {
  const { ctx, store, html } = await renderFolded({
    turns: foldAgents(4),
    state: { all: false, turns: ["0/2"], open: ["0/3", "0/4"] },
    echo: false,
    deps: { icons: FOLD_ICONS },
  });
  ctx.feature.bind();
  assert.equal(ctx.roots.chatfoldall.hidden, false);
  const fn = firstListener(ctx.roots.chatfoldall, "click");
  assert.equal(typeof fn, "function");
  fn({ target: ctx.roots.chatfoldall });
  await foldSettle();
  const page = html();
  assert.equal(page.includes(">a3<"), false);
  assert.equal(page.includes(">a4<"), false);
  assert.ok(page.includes(foldShowHTML(2, "0/3")));
  assert.deepEqual(readStoredFold(store), { all: true, turns: [], open: [] });
  assert.equal(ctx.roots.chatfoldall.hidden, true);
  assert.doesNotMatch(ctx.roots.chatfoldall.innerHTML, /Unfold|Show all/);
  assert.doesNotMatch(page, /Unfold all|Show all intermediate/);
  ctx.feature.destroy();
});

test("the page has no control that opens every folded bubble", () => {
  assert.doesNotMatch(indexHtmlChat, /Unfold all|Show all intermediate|data-fold="all"/);
});

test("the chat head markup offers one fold-all button after the detail meta", () => {
  const details = indexHtmlChat.slice(
    indexHtmlChat.indexOf('id="chatdetails"'),
    indexHtmlChat.indexOf('id="chatdesc"'),
  );
  assert.match(details,
    /<button type="button" class="foldall" id="chatfoldall" hidden>Fold all intermediate agent bubbles<\/button>/);
});

test("the fold-all control stays hidden when its rule sets display", () => {
  assert.match(chatCssSrc, /\.foldall\[hidden\]\s*\{[^}]*display:\s*none/);
});

test("a jump to a hidden bubble opens its range and no other", async () => {
  const { ctx, store, html } = await renderFolded({
    turns: twoRangeTurns(),
    state: { all: false, turns: ["0/2"], open: [] },
    echo: false,
    deps: twoRangeDeps(),
  });
  ctx.setPending({
    node: "n1", uid: "log-1", segment: 0, record: 6, ts: 1_000_000,
  });
  await ctx.feature.render();
  const page = html();
  assert.ok(page.includes(">right-c<"));
  assert.ok(page.includes(">right-d<"));
  assert.equal(page.includes(">left-a<"), false);
  assert.equal(page.includes(">left-b<"), false);
  assert.deepEqual(readStoredFold(store), {
    all: false, turns: ["0/2"], open: ["0/6", "0/7"],
  });
  assert.equal(ctx.getPending(), null);
  ctx.feature.destroy();
});

test("a jump to a hidden history bubble opens that surface's range only", async () => {
  const histTurns = foldAgents(4, 0, "log-1", record => "hist-" + record);
  const liveTurns = foldAgents(4, 1, "log-1", record => "live-" + record);
  const store = foldMemory(foldRecord("log-1", {
    all: false, turns: ["0/2", "1/2"], open: [],
  }));
  const ctx = makeFeature({
    histSegs: [
      { start: "2025-01-01T00:00:00Z", seam: "2025-01-01T00:00:00Z", reason: "clear" },
      {
        start: "2025-06-01T00:00:00Z",
        seam: "2025-06-01T00:00:00Z",
        reason: "clear",
        turns: histTurns,
      },
    ],
    chatPayload: {
      turns: liveTurns,
      live: "quiet", delivery: "ok", source: "tmux",
      chat_started: "2026-01-01T00:00:00Z",
      prior_turns: 2, assets: {}, error: "channel down",
    },
    deps: {
      fmtWhen: t => "T(" + t + ")",
      bubbleTitle: (role, time) => role + "|" + time,
      md: s => String(s ?? ""),
      foldStorage: store,
    },
  });
  await ctx.feature.loadHistory("n1", "");
  ctx.setPending({
    node: "n1", uid: "log-1", segment: 0, record: 3, ts: 1_000_000,
  });
  await ctx.feature.render();
  const page = ctx.roots.msgs.innerHTML;
  assert.ok(page.includes(">hist-3<"));
  assert.ok(page.includes(">hist-4<"));
  assert.equal(page.includes(">live-3<"), false);
  assert.deepEqual(readStoredFold(store), {
    all: false, turns: ["0/2", "1/2"], open: ["0/3", "0/4"],
  });
  ctx.feature.destroy();
});

test("a jump to a visible bubble changes no fold state", async () => {
  const seed = { all: false, turns: ["0/2"], open: ["0/6", "0/7"] };
  const { ctx, store, html } = await renderFolded({
    turns: twoRangeTurns(),
    state: seed,
    echo: false,
    deps: twoRangeDeps(),
  });
  ctx.setPending({
    node: "n1", uid: "log-1", segment: 0, record: 6, ts: 1_000_000,
  });
  await ctx.feature.render();
  assert.ok(html().includes(">right-c<"));
  assert.deepEqual(readStoredFold(store), seed);
  ctx.feature.destroy();
});

test("a jump aimed at another chat does not change fold state", async () => {
  const seed = { all: false, turns: ["0/2"], open: [] };
  const { ctx, store, html } = await renderFolded({
    turns: twoRangeTurns(),
    state: seed,
    echo: false,
    deps: twoRangeDeps(),
  });
  ctx.setPending({
    node: "n2", uid: "log-1", segment: 0, record: 6, ts: 1_000_000,
  });
  await ctx.feature.render();
  assert.equal(html().includes(">right-c<"), false);
  assert.deepEqual(readStoredFold(store), seed);
  ctx.feature.destroy();
});

test("an expired jump does not open a hidden range", async () => {
  const seed = { all: false, turns: ["0/2"], open: [] };
  const { ctx, store, html } = await renderFolded({
    turns: twoRangeTurns(),
    state: seed,
    echo: false,
    deps: twoRangeDeps(),
  });
  ctx.setPending({
    node: "n1", uid: "log-1", segment: 0, record: 6, ts: 1,
  });
  await ctx.feature.render();
  assert.equal(html().includes(">right-c<"), false);
  assert.deepEqual(readStoredFold(store), seed);
  ctx.feature.destroy();
});

test("destroy removes the fold-all listener", () => {
  const { feature, roots } = makeFeature();
  feature.bind();
  assert.equal(roots.chatfoldall.listenerCount("click"), 1);
  feature.destroy();
  assert.equal(roots.chatfoldall.listenerCount("click"), 0);
});

/* Keyboard focus after a fold. The rebuild destroys the control that was
 * activated. When that control held the focus, the focus returns to what
 * replaced it, and focus() is called with preventScroll so the page is not
 * scrolled a second time. The fake DOM does not focus a button on click, so
 * each test sets activeElement itself. A control that did not hold the focus
 * is left alone, and a later poll does not move the focus. */

function foldFocusDoc(){
  const log = [];
  const doc = {
    activeElement: null,
    createElement(tag){ return el(tag); },
    querySelector(){ return null; },
  };
  function watch(node, mark){
    node.focus = opts => {
      log.push({ ...mark, opts, node });
      doc.activeElement = node;
    };
    return node;
  }
  const elsewhere = watch(el("button"), { role: "elsewhere" });
  doc.activeElement = elsewhere;
  return { log, doc, watch, elsewhere };
}

function installFoldFocus(msgs, doc, log, top){
  let html = msgs.innerHTML;
  Object.defineProperty(msgs, "innerHTML", {
    configurable: true,
    get(){ return html; },
    set(v){
      html = String(v);
      msgs.children.length = 0;
      const btnRe = /data-fold="(open|close)" data-foldkey="([^"]+)"/g;
      let m;
      while ((m = btnRe.exec(html))){
        const kind = m[1];
        const key = m[2];
        const btn = el("button", {
          className: "foldtoggle",
          dataset: { fold: kind, foldkey: key },
        });
        btn.offsetTop = top;
        btn.focus = opts => {
          log.push({ role: "fold", kind, key, opts, node: btn });
          doc.activeElement = btn;
        };
        const row = el("div", { className: "foldrow" });
        row.appendChild(btn);
        msgs.appendChild(row);
      }
      const turnRe = /class="(turn [^"]*)"[^>]*data-bk="([^"]+)"/g;
      let t;
      while ((t = turnRe.exec(html))){
        const turn = el("div", { className: t[1], dataset: { bk: t[2] } });
        turn.offsetTop = top;
        msgs.appendChild(turn);
      }
    },
  });
}

function revealFoldControls(msgs, doc, log, top){
  installFoldFocus(msgs, doc, log, top);
  msgs.innerHTML = msgs.innerHTML;
}

function foldToggleButtons(msgs){
  const out = [];
  const walk = n => {
    if (n.dataset && n.dataset.fold) out.push(n);
    for (const c of n.children || []) walk(c);
  };
  walk(msgs);
  return out;
}

function holdFoldScroll(msgs, scrollTop){
  msgs.scrollHeight = 2000;
  msgs.clientHeight = 400;
  msgs.scrollTop = scrollTop;
}

function wideFoldTurns(){
  const text = {
    2: "first", 3: "hid-a", 4: "hid-b", 5: "kept",
    6: "o6", 7: "o7", 8: "o8", 9: "o9", 10: "o10", 11: "o11", 12: "o12", 13: "o13",
    14: "last",
  };
  const turns = [foldBubble("user", 1, "prompt-one")];
  for (let record = 2; record <= 14; record++){
    turns.push(foldBubble("assistant", record, text[record]));
  }
  return turns;
}

function wideFoldState(){
  const open = [];
  for (let record = 6; record <= 13; record++) open.push("0/" + record);
  return { all: false, turns: ["0/2"], open };
}

function wideFoldDeps(doc){
  return {
    document: doc,
    icons: FOLD_ICONS,
    bookmarks: () => [{ uid: "log-1", segment: 0, record: 5, text: "kept" }],
    markerVersion: () => 1,
  };
}

function focusSplitTurns(){
  return [
    foldBubble("user", 1, "prompt-one"),
    foldBubble("assistant", 2, "a-first"),
    foldBubble("assistant", 3, "a-hid"),
    foldBubble("assistant", 4, "a-hid2"),
    foldBubble("assistant", 5, "a-keep"),
    foldBubble("assistant", 6, "a-open"),
    foldBubble("assistant", 7, "a-open2"),
    foldBubble("assistant", 8, "a-last"),
    foldBubble("user", 10, "prompt-two"),
    foldBubble("assistant", 11, "b-first"),
    foldBubble("assistant", 12, "b-left"),
    foldBubble("assistant", 13, "b-left2"),
    foldBubble("assistant", 14, "b-keep"),
    foldBubble("assistant", 15, "b-right"),
    foldBubble("assistant", 16, "b-right2"),
    foldBubble("assistant", 17, "b-last"),
  ];
}

function focusSplitDeps(doc){
  return {
    document: doc,
    icons: FOLD_ICONS,
    bookmarks: () => [
      { uid: "log-1", segment: 0, record: 5, text: "a-keep" },
      { uid: "log-1", segment: 0, record: 14, text: "b-keep" },
    ],
    markerVersion: () => 1,
  };
}

test("focused Show control returns focus to the Fold control of the same range", async () => {
  const focus = foldFocusDoc();
  const { ctx, html } = await renderFolded({
    turns: twoRangeTurns(),
    state: { all: false, turns: ["0/2"], open: [] },
    echo: false,
    deps: { ...twoRangeDeps(), document: focus.doc },
  });
  ctx.feature.bind();
  holdFoldScroll(ctx.roots.msgs, 40);
  revealFoldControls(ctx.roots.msgs, focus.doc, focus.log, 800);
  const show = foldToggleButtons(ctx.roots.msgs).find(btn =>
    btn.dataset.fold === "open" && btn.dataset.foldkey === "0/6");
  show.offsetTop = 500;
  focus.doc.activeElement = show;
  firstListener(ctx.roots.msgs, "click")({ target: show });
  await foldSettle();
  assert.ok(html().includes(">right-c<"));
  assert.equal(html().includes(">left-a<"), false);
  assert.equal(focus.log.length, 1);
  assert.equal(focus.log[0].role, "fold");
  assert.equal(focus.log[0].kind, "close");
  assert.equal(focus.log[0].key, "0/6");
  assert.deepEqual(focus.log[0].opts, { preventScroll: true });
  assert.equal(focus.doc.activeElement, focus.log[0].node);
  assert.equal(focus.doc.activeElement.dataset.foldkey, "0/6");
  assert.equal(ctx.roots.msgs.scrollTop, 340);
  ctx.feature.destroy();
});

test("Show control without focus does not move focus", async () => {
  const focus = foldFocusDoc();
  const { ctx, html } = await renderFolded({
    turns: twoRangeTurns(),
    state: { all: false, turns: ["0/2"], open: [] },
    echo: false,
    deps: { ...twoRangeDeps(), document: focus.doc },
  });
  ctx.feature.bind();
  holdFoldScroll(ctx.roots.msgs, 40);
  revealFoldControls(ctx.roots.msgs, focus.doc, focus.log, 800);
  const show = foldToggleButtons(ctx.roots.msgs).find(btn =>
    btn.dataset.fold === "open" && btn.dataset.foldkey === "0/6");
  show.offsetTop = 500;
  firstListener(ctx.roots.msgs, "click")({ target: show });
  await foldSettle();
  assert.ok(html().includes(">right-c<"));
  assert.equal(focus.log.length, 0);
  assert.equal(focus.doc.activeElement, focus.elsewhere);
  assert.equal(ctx.roots.msgs.scrollTop, 340);
  ctx.feature.destroy();
});

test("focused upper Fold control returns focus to the Show row of that range", async () => {
  const focus = foldFocusDoc();
  const { ctx, html } = await renderFolded({
    turns: wideFoldTurns(),
    state: wideFoldState(),
    echo: false,
    deps: wideFoldDeps(focus.doc),
  });
  ctx.feature.bind();
  holdFoldScroll(ctx.roots.msgs, 100);
  revealFoldControls(ctx.roots.msgs, focus.doc, focus.log, 800);
  const closes = foldToggleButtons(ctx.roots.msgs).filter(btn =>
    btn.dataset.fold === "close" && btn.dataset.foldkey === "0/6");
  assert.equal(closes.length, 2);
  closes[0].offsetTop = 200;
  focus.doc.activeElement = closes[0];
  firstListener(ctx.roots.msgs, "click")({ target: closes[0] });
  await foldSettle();
  assert.ok(html().includes(foldShowHTML(8, "0/6")));
  assert.ok(html().includes(foldShowHTML(2, "0/3")));
  assert.equal(focus.log.length, 1);
  assert.equal(focus.log[0].kind, "open");
  assert.equal(focus.log[0].key, "0/6");
  assert.deepEqual(focus.log[0].opts, { preventScroll: true });
  assert.equal(focus.doc.activeElement.dataset.fold, "open");
  assert.equal(focus.doc.activeElement.dataset.foldkey, "0/6");
  assert.equal(ctx.roots.msgs.scrollTop, 700);
  ctx.feature.destroy();
});

test("focused lower Fold control returns focus to the Show row of that range", async () => {
  const focus = foldFocusDoc();
  const { ctx, html } = await renderFolded({
    turns: wideFoldTurns(),
    state: wideFoldState(),
    echo: false,
    deps: wideFoldDeps(focus.doc),
  });
  ctx.feature.bind();
  holdFoldScroll(ctx.roots.msgs, 100);
  revealFoldControls(ctx.roots.msgs, focus.doc, focus.log, 800);
  const closes = foldToggleButtons(ctx.roots.msgs).filter(btn =>
    btn.dataset.fold === "close" && btn.dataset.foldkey === "0/6");
  assert.equal(closes.length, 2);
  closes[1].offsetTop = 500;
  focus.doc.activeElement = closes[1];
  firstListener(ctx.roots.msgs, "click")({ target: closes[1] });
  await foldSettle();
  assert.ok(html().includes(foldShowHTML(8, "0/6")));
  assert.ok(html().includes(foldShowHTML(2, "0/3")));
  assert.equal(focus.log.length, 1);
  assert.equal(focus.log[0].kind, "open");
  assert.equal(focus.log[0].key, "0/6");
  assert.deepEqual(focus.log[0].opts, { preventScroll: true });
  assert.equal(focus.doc.activeElement.dataset.fold, "open");
  assert.equal(focus.doc.activeElement.dataset.foldkey, "0/6");
  assert.equal(ctx.roots.msgs.scrollTop, 400);
  ctx.feature.destroy();
});

test("Fold control without focus does not move focus", async () => {
  const focus = foldFocusDoc();
  const { ctx, html } = await renderFolded({
    turns: wideFoldTurns(),
    state: wideFoldState(),
    echo: false,
    deps: wideFoldDeps(focus.doc),
  });
  ctx.feature.bind();
  holdFoldScroll(ctx.roots.msgs, 100);
  revealFoldControls(ctx.roots.msgs, focus.doc, focus.log, 800);
  const closes = foldToggleButtons(ctx.roots.msgs).filter(btn =>
    btn.dataset.fold === "close" && btn.dataset.foldkey === "0/6");
  closes[1].offsetTop = 500;
  firstListener(ctx.roots.msgs, "click")({ target: closes[1] });
  await foldSettle();
  assert.ok(html().includes(foldShowHTML(8, "0/6")));
  assert.equal(focus.log.length, 0);
  assert.equal(focus.doc.activeElement, focus.elsewhere);
  assert.equal(ctx.roots.msgs.scrollTop, 400);
  ctx.feature.destroy();
});

test("focused fold action returns focus to the first Show row of that turn", async () => {
  const focus = foldFocusDoc();
  const { ctx, store, html } = await renderFolded({
    turns: focusSplitTurns(),
    state: { all: false, turns: ["0/2"], open: ["0/6", "0/7"] },
    echo: false,
    deps: focusSplitDeps(focus.doc),
  });
  ctx.feature.bind();
  holdFoldScroll(ctx.roots.msgs, 40);
  installFoldFocus(ctx.roots.msgs, focus.doc, focus.log, 800);
  const anchor = el("div", { className: "turn assistant", dataset: { bk: "i:9" } });
  anchor.offsetTop = 500;
  ctx.roots.msgs.appendChild(anchor);
  firstListener(ctx.roots.msgs, "click")({ target: anchor });
  const btn = el("button", { dataset: { bact: "fold" } });
  focus.doc.activeElement = btn;
  firstListener(ctx.roots.msgs, "click")({ target: btn });
  await foldSettle();
  assert.deepEqual(readStoredFold(store), {
    all: false, turns: ["0/2", "0/11"], open: ["0/6", "0/7"],
  });
  assert.ok(html().includes(foldShowHTML(2, "0/12")));
  assert.ok(html().includes(foldShowHTML(2, "0/3")));
  assert.equal(focus.log.length, 1);
  assert.equal(focus.log[0].kind, "open");
  assert.equal(focus.log[0].key, "0/12");
  assert.deepEqual(focus.log[0].opts, { preventScroll: true });
  assert.equal(focus.doc.activeElement.dataset.foldkey, "0/12");
  assert.equal(ctx.roots.msgs.scrollTop, 340);
  ctx.feature.destroy();
});

test("fold action without focus does not move focus", async () => {
  const focus = foldFocusDoc();
  const { ctx, store } = await renderFolded({
    turns: focusSplitTurns(),
    state: { all: false, turns: ["0/2"], open: ["0/6", "0/7"] },
    echo: false,
    deps: focusSplitDeps(focus.doc),
  });
  ctx.feature.bind();
  holdFoldScroll(ctx.roots.msgs, 40);
  installFoldFocus(ctx.roots.msgs, focus.doc, focus.log, 800);
  const anchor = el("div", { className: "turn assistant", dataset: { bk: "i:9" } });
  anchor.offsetTop = 500;
  ctx.roots.msgs.appendChild(anchor);
  firstListener(ctx.roots.msgs, "click")({ target: anchor });
  const btn = el("button", { dataset: { bact: "fold" } });
  firstListener(ctx.roots.msgs, "click")({ target: btn });
  await foldSettle();
  assert.deepEqual(readStoredFold(store), {
    all: false, turns: ["0/2", "0/11"], open: ["0/6", "0/7"],
  });
  assert.equal(focus.log.length, 0);
  assert.equal(focus.doc.activeElement, focus.elsewhere);
  assert.equal(ctx.roots.msgs.scrollTop, 340);
  ctx.feature.destroy();
});

test("focused Fold all control returns focus to the info button", async () => {
  const focus = foldFocusDoc();
  const { ctx } = await renderFolded({
    turns: foldAgents(4),
    state: { all: false, turns: [], open: [] },
    echo: false,
    deps: { icons: FOLD_ICONS, document: focus.doc },
  });
  ctx.feature.bind();
  focus.watch(ctx.roots.infobtn, { role: "infobtn" });
  holdFoldScroll(ctx.roots.msgs, 120);
  installFoldFocus(ctx.roots.msgs, focus.doc, focus.log, 800);
  focus.doc.activeElement = ctx.roots.chatfoldall;
  firstListener(ctx.roots.chatfoldall, "click")({ target: ctx.roots.chatfoldall });
  await foldSettle();
  assert.equal(ctx.roots.chatfoldall.hidden, true);
  assert.equal(focus.log.length, 1);
  assert.equal(focus.log[0].role, "infobtn");
  assert.deepEqual(focus.log[0].opts, { preventScroll: true });
  assert.equal(focus.doc.activeElement, ctx.roots.infobtn);
  assert.equal(ctx.roots.msgs.scrollTop, 120);
  ctx.feature.destroy();
});

test("Fold all control without focus does not move focus", async () => {
  const focus = foldFocusDoc();
  const { ctx } = await renderFolded({
    turns: foldAgents(4),
    state: { all: false, turns: [], open: [] },
    echo: false,
    deps: { icons: FOLD_ICONS, document: focus.doc },
  });
  ctx.feature.bind();
  focus.watch(ctx.roots.infobtn, { role: "infobtn" });
  holdFoldScroll(ctx.roots.msgs, 120);
  installFoldFocus(ctx.roots.msgs, focus.doc, focus.log, 800);
  firstListener(ctx.roots.chatfoldall, "click")({ target: ctx.roots.chatfoldall });
  await foldSettle();
  assert.equal(ctx.roots.chatfoldall.hidden, true);
  assert.equal(focus.log.length, 0);
  assert.equal(focus.doc.activeElement, focus.elsewhere);
  assert.equal(ctx.roots.msgs.scrollTop, 120);
  ctx.feature.destroy();
});

test("a poll after a fold action does not move focus", async () => {
  const focus = foldFocusDoc();
  const turns = twoRangeTurns();
  const { ctx, html } = await renderFolded({
    turns,
    state: { all: false, turns: ["0/2"], open: [] },
    echo: false,
    deps: { ...twoRangeDeps(), document: focus.doc },
  });
  ctx.feature.bind();
  holdFoldScroll(ctx.roots.msgs, 40);
  revealFoldControls(ctx.roots.msgs, focus.doc, focus.log, 800);
  const show = foldToggleButtons(ctx.roots.msgs).find(btn =>
    btn.dataset.fold === "open" && btn.dataset.foldkey === "0/6");
  show.offsetTop = 500;
  focus.doc.activeElement = show;
  firstListener(ctx.roots.msgs, "click")({ target: show });
  await foldSettle();
  assert.ok(html().includes(">right-c<"));
  const held = focus.doc.activeElement;
  const calls = focus.log.length;
  turns.push(foldBubble("assistant", 9, "arrived"));
  await ctx.feature.render();
  assert.ok(html().includes(">arrived<"));
  assert.equal(focus.doc.activeElement, held);
  assert.equal(focus.log.length, calls);
  ctx.feature.destroy();
});

/* A real browser drops focus to body when innerHTML removes the focused
 * control. These helpers model that removal and document membership. */
function foldFocusPage(){
  const focus = foldFocusDoc();
  const body = el("body");
  const composer = el("textarea");
  const live = new Set([body, composer, focus.elsewhere]);
  const transcript = new Set();
  focus.doc.body = body;
  focus.doc.contains = node => live.has(node);
  return { ...focus, body, composer, live, transcript };
}

function installRemovingFoldFocus(msgs, focus, top){
  let html = msgs.innerHTML;
  Object.defineProperty(msgs, "innerHTML", {
    configurable: true,
    get(){ return html; },
    set(v){
      if (focus.transcript.has(focus.doc.activeElement)){
        focus.live.delete(focus.doc.activeElement);
        focus.transcript.delete(focus.doc.activeElement);
        focus.doc.activeElement = focus.body;
      }
      for (const old of focus.transcript) focus.live.delete(old);
      focus.transcript.clear();
      html = String(v);
      msgs.children.length = 0;
      const controls = [];
      const btnRe = /data-fold="(open|close)" data-foldkey="([^"]+)"/g;
      let m;
      while ((m = btnRe.exec(html))){
        const kind = m[1];
        const key = m[2];
        const btn = el("button", {
          className: "foldtoggle",
          dataset: { fold: kind, foldkey: key },
        });
        btn.offsetTop = top;
        btn.focus = opts => {
          focus.log.push({ role: "fold", kind, key, opts, node: btn });
          focus.doc.activeElement = btn;
        };
        const row = el("div", { className: "foldrow" });
        row.appendChild(btn);
        msgs.appendChild(row);
        controls.push(btn);
        focus.transcript.add(btn);
      }
      msgs._foldControls = controls;
      for (const control of controls) focus.live.add(control);
      if (!focus.doc.contains(focus.doc.activeElement)) focus.doc.activeElement = focus.body;
    },
  });
  msgs.innerHTML = msgs.innerHTML;
}

function deferred(){
  let resolve;
  const promise = new Promise(r => { resolve = r; });
  return { promise, resolve };
}

test("a poll rebuild uses pending fold focus before the action refresh resolves", async () => {
  const focus = foldFocusPage();
  const actionResponse = deferred();
  const turns = twoRangeTurns();
  const payload = {
    turns, live: "quiet", delivery: "ok", source: "tmux",
    chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
  };
  let overlap = false;
  let overlappingCalls = 0;
  const { ctx } = await renderFolded({
    turns,
    state: { all: false, turns: ["0/2"], open: [] },
    echo: false,
    chatConditional(){
      if (!overlap) return Promise.resolve({ status: 200, data: payload });
      overlappingCalls++;
      if (overlappingCalls === 1) return actionResponse.promise;
      return Promise.resolve({ status: 200, data: payload });
    },
    deps: { ...twoRangeDeps(), document: focus.doc },
  });
  ctx.feature.bind();
  installRemovingFoldFocus(ctx.roots.msgs, focus, 800);
  const show = ctx.roots.msgs._foldControls.find(btn =>
    btn.dataset.fold === "open" && btn.dataset.foldkey === "0/6");
  focus.live.add(show);
  focus.doc.activeElement = show;
  overlap = true;
  firstListener(ctx.roots.msgs, "click")({ target: show });
  await Promise.resolve();
  turns.push(foldBubble("assistant", 9, "poll-arrived"));
  payload.turns = turns;
  await ctx.feature.render();
  assert.equal(focus.doc.activeElement.dataset?.foldkey, "0/6");
  assert.equal(focus.log.length, 1);
  actionResponse.resolve({ status: 200, data: payload });
  await foldSettle();
  assert.equal(focus.log.length, 1, "the target is consumed by only one rebuild");
  ctx.feature.destroy();
});

test("a skipped fold render keeps its focus target for a later rebuild", async () => {
  const focus = foldFocusPage();
  const actionResponse = deferred();
  const turns = twoRangeTurns();
  const payload = {
    turns, live: "quiet", delivery: "ok", source: "tmux",
    chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
  };
  let overlap = false;
  let overlappingCalls = 0;
  const { ctx } = await renderFolded({
    turns,
    state: { all: false, turns: ["0/2"], open: [] },
    echo: false,
    chatConditional(){
      if (!overlap) return Promise.resolve({ status: 200, data: payload });
      overlappingCalls++;
      return overlappingCalls === 1
        ? actionResponse.promise
        : Promise.resolve({ status: 200, data: payload });
    },
    deps: { ...twoRangeDeps(), document: focus.doc },
  });
  ctx.feature.bind();
  installRemovingFoldFocus(ctx.roots.msgs, focus, 800);
  const show = ctx.roots.msgs._foldControls.find(btn =>
    btn.dataset.fold === "open" && btn.dataset.foldkey === "0/6");
  focus.live.add(show);
  focus.doc.activeElement = show;
  overlap = true;
  firstListener(ctx.roots.msgs, "click")({ target: show });
  await Promise.resolve();
  actionResponse.resolve({ status: 304 });
  await foldSettle();
  assert.equal(focus.log.length, 0, "a 304 does not use the pending target");
  turns.push(foldBubble("assistant", 9, "later-poll"));
  payload.turns = turns;
  await ctx.feature.render();
  assert.equal(focus.log.length, 1, "the next rebuilding poll uses the target");
  assert.equal(focus.doc.activeElement.dataset?.foldkey, "0/6");
  ctx.feature.destroy();
});

test("a fold answer leaves focus in the composer when the user moved there", async () => {
  const focus = foldFocusPage();
  const response = deferred();
  let hold = false;
  const { ctx } = await renderFolded({
    turns: twoRangeTurns(),
    state: { all: false, turns: ["0/2"], open: [] },
    echo: false,
    chatConditional(path, etag, calls){
      if (hold && calls.length > 1) return response.promise;
      return Promise.resolve({ status: 200, data: {
        turns: twoRangeTurns(), live: "quiet", delivery: "ok", source: "tmux",
        chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
      } });
    },
    deps: { ...twoRangeDeps(), document: focus.doc },
  });
  ctx.feature.bind();
  installRemovingFoldFocus(ctx.roots.msgs, focus, 800);
  const show = ctx.roots.msgs._foldControls.find(btn => btn.dataset.fold === "open");
  focus.live.add(show);
  focus.doc.activeElement = show;
  hold = true;
  firstListener(ctx.roots.msgs, "click")({ target: show });
  focus.doc.activeElement = focus.composer;
  response.resolve({ status: 200, data: {
    turns: twoRangeTurns(), live: "quiet", delivery: "ok", source: "tmux",
    chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
  } });
  await foldSettle();
  assert.equal(focus.doc.activeElement, focus.composer);
  assert.equal(focus.log.length, 0);
  ctx.feature.destroy();
});

test("a late fold answer preserves focus on another connected button", async () => {
  const focus = foldFocusPage();
  const response = deferred();
  let hold = false;
  const payload = {
    turns: twoRangeTurns(), live: "quiet", delivery: "ok", source: "tmux",
    chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
  };
  const { ctx } = await renderFolded({
    turns: payload.turns,
    state: { all: false, turns: ["0/2"], open: [] }, echo: false,
    chatConditional(path, etag, calls){
      if (hold && calls.length > 1) return response.promise;
      return Promise.resolve({ status: 200, data: payload });
    },
    deps: { ...twoRangeDeps(), document: focus.doc },
  });
  ctx.feature.bind();
  installRemovingFoldFocus(ctx.roots.msgs, focus, 800);
  const show = ctx.roots.msgs._foldControls.find(btn => btn.dataset.fold === "open");
  focus.live.add(show);
  focus.doc.activeElement = show;
  hold = true;
  firstListener(ctx.roots.msgs, "click")({ target: show });
  focus.doc.activeElement = focus.elsewhere;
  response.resolve({ status: 200, data: payload });
  await foldSettle();
  assert.equal(focus.doc.activeElement, focus.elsewhere);
  assert.equal(focus.log.length, 0);
  ctx.feature.destroy();
});

test("a fold answer restores focus from body or a removed transcript element", async t => {
  for (const startAt of ["body", "removed"]){
    await t.test(startAt, async () => {
      const focus = foldFocusPage();
      const response = deferred();
      let hold = false;
      const payload = {
        turns: twoRangeTurns(), live: "quiet", delivery: "ok", source: "tmux",
        chat_started: "2026-01-01T00:00:00Z", prior_turns: 0, assets: {},
      };
      const { ctx } = await renderFolded({
        turns: payload.turns,
        state: { all: false, turns: ["0/2"], open: [] }, echo: false,
        chatConditional(path, etag, calls){
          if (hold && calls.length > 1) return response.promise;
          return Promise.resolve({ status: 200, data: payload });
        },
        deps: { ...twoRangeDeps(), document: focus.doc },
      });
      ctx.feature.bind();
      installRemovingFoldFocus(ctx.roots.msgs, focus, 800);
      const show = ctx.roots.msgs._foldControls.find(btn => btn.dataset.fold === "open");
      focus.live.add(show);
      focus.doc.activeElement = show;
      hold = true;
      firstListener(ctx.roots.msgs, "click")({ target: show });
      if (startAt === "removed") focus.doc.activeElement = show;
      else focus.doc.activeElement = focus.body;
      response.resolve({ status: 200, data: payload });
      await foldSettle();
      assert.equal(focus.log.length, 1);
      assert.equal(focus.doc.activeElement.dataset?.foldkey, "0/3");
      ctx.feature.destroy();
    });
  }
});

test("an optimistic prompt survives a poll with no transcript turns", () => {
  const echo = { text:"synthetic prompt", at:1000, base:0 };
  const result = retireSentEcho(echo, null, 1100);
  assert.equal(result.retired, false);
  assert.equal(result.echo, echo);
});

test('history keeps an empty clear seam only when an owner comment anchors there', async () => {
 const {historyForDiscussion}=await import('../js/chat.js');
 const prior=[{segment_index:0,log_uid:'u',turns:[{uid:'u',segment:0,record:1}]}];
 const empty=[{segment_index:1,log_uid:'u',turns:[],start:'T'},{segment_index:2,log_uid:'u',turns:[],start:'T'}];
 const discussions=[{uid:'u',mirrors:[{seq:1,t:'source',local_segment:0,local_record:2}],comments:[{seq:1,anchor:1,text:'reply'}]}];
 assert.deepEqual(historyForDiscussion(prior,empty,3,'',discussions).map(s=>s.segment_index),[0,1]);
 assert.deepEqual(historyForDiscussion(prior,empty,3,'',[]).map(s=>s.segment_index),[0]);
});

test('loaded history renders a comment in its exact empty clear segment', async () => {
 const copy={uid:'u',key:'s/o/u/g',view:{server:'s'},names:{p:'Person'},people:{},mirrors:[{seq:1,t:'source',local_segment:0,local_record:2}],comments:[{seq:1,anchor:1,text:'between clears',author:'p',at:'T'}]};
 const ctx=makeFeature({deps:{ownerDiscussion:()=>[copy]},api:async path=>{
  if(path.includes('history=1')) return {segments:[{log_uid:'u',segment_index:0,start:'2026-01-01T00:00:00Z',turns:[{role:'assistant',uid:'u',segment:0,record:1,text:'before'}]}],empty_segments:[{log_uid:'u',segment_index:1,start:'2026-01-01T00:00:02Z',seam:'2026-01-01T00:00:02Z',reason:'clear',turns:[]}],assets:{}};
  if(path.includes('/chat')) return {log_uid:'u',segment_index:2,turns:[{role:'assistant',uid:'u',segment:2,record:5,text:'after'}],prior_turns:1,chat_started:'2026-01-01T00:00:04Z',live:'quiet',source:'codex'};
  return {};
 }});
 ctx.feature.bind(); await ctx.feature.loadHistory('n1',''); const body=ctx.roots.msgs.innerHTML;
 assert.ok(body.indexOf('before')<body.indexOf('between clears')); assert.ok(body.indexOf('between clears')<body.indexOf('after')); ctx.feature.destroy();
});

test('history falls back for older payloads and ignores invalid or current empty seams', async () => {
 const {historyForDiscussion}=await import('../js/chat.js');
 const old=[{start:'2020-01-01T00:00:00Z',turns:[]}];
 assert.deepEqual(historyForDiscussion(old,[],undefined,'2021-01-01T00:00:00Z',[]),old);
 const indexed=[{segment_index:0,turns:[]}], empty=[{segment_index:1,log_uid:'u',turns:[]},{segment_index:2,log_uid:'u',turns:[]},{log_uid:'u',turns:[]}];
 assert.deepEqual(historyForDiscussion(indexed,empty,2,'',[]),indexed);
});

test('history handles an empty server response and no discussion', async () => {
 const ctx=makeFeature({deps:{ownerDiscussion:()=>null},api:async path=>path.includes('history=1')?{}:{turns:[],live:'quiet',source:'codex'}});
 ctx.feature.bind(); await ctx.feature.loadHistory('n1',''); assert.ok(ctx.roots.msgs.innerHTML); ctx.feature.destroy();
 const {historyForDiscussion}=await import('../js/chat.js'); assert.deepEqual(historyForDiscussion([{segment_index:0}], [{segment_index:1,log_uid:'u'}],2,'',null).map(s=>s.segment_index),[0]);
});

test('received selection ignores forged native bubble controls', () => {
 let previews=0; const ctx=makeFeature({sel:'team/s/o/c/g',deps:{openAssetPreview:()=>previews++}});
 ctx.feature.bind(); const target={dataset:{},href:'forged',closest:selector=>selector==='[data-asset-preview]'?target:null};
 firstListener(ctx.roots.msgs,'click')({target,preventDefault(){}}); assert.equal(previews,0); assert.equal(ctx.apiCalls.length,0); ctx.feature.destroy();
});

test('received associated image uses the common preview controller',()=>{
 const opened=[];let prevented=0;const preview={href:'blob:verified',dataset:{name:'synthetic.png'}};
 const ctx=makeFeature({sel:'team/s/o/c/g',deps:{sharedPreview:node=>node===preview?{url:preview.href,name:preview.dataset.name}:null,openAssetPreview:value=>opened.push(value)}});
 ctx.feature.bind();firstListener(ctx.roots.msgs,'click')({target:{closest:selector=>selector==='[data-asset-preview]'?preview:null},preventDefault(){prevented++}});
 assert.deepEqual(opened,[{url:'blob:verified',name:'synthetic.png'}]);assert.equal(prevented,1);assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();
});

test('legacy owner comments without a recoverable frontier are visible with an honest label', async () => {
 const copy={uid:'u',key:'s/o/u/g',view:{server:'s'},names:{p:'Person'},people:{},mirrors:[{seq:1,t:'start'}],comments:[{seq:1,anchor:1,text:'retained unplaced',author:'p',at:'T'}]};
 const ctx=makeFeature({deps:{ownerDiscussion:()=>[copy]},api:async path=>path.includes('/chat')?{turns:[{role:'assistant',uid:'u',segment:0,record:2,text:'later local'}],log_uid:'u',segment_index:0,live:'quiet',source:'codex'}:{}});
 ctx.feature.bind();await ctx.feature.render();const body=ctx.roots.msgs.innerHTML; assert.match(body,/position unavailable/i);assert.equal((body.match(/retained unplaced/g)||[]).length,1);ctx.feature.destroy();
});

test('owner prompt action uses the direction opposite description', () => {
  const html=bubbleActionsHTML({time:''},{iconInto:'glyph',prompt:true});
  assert.match(html, /data-bact="prompt"><span class="rot90r">glyph<\/span>use as prompt/);
  assert.match(html, /data-bact="desc"><span class="rot90l">glyph<\/span>/);
  assert.doesNotMatch(bubbleActionsHTML({time:''},{iconInto:'glyph'}), /data-bact="prompt"/);
});

test('prepared prompt marker uses accessible opposite-facing prompt glyph', () => {
  const html=bubbleMarkersHTML({prepared:true},{iconInto:'glyph'});
  assert.match(html,/aria-label="Prepared as prompt"/);
  assert.match(html,/<span class="rot90r">glyph<\/span>/);
  assert.equal(bubbleMarkersHTML({}, {iconInto:'glyph'}),'');
});


test('native prompt bubble action checks capability again before preparation', async () => {
  const prepared=[]; let allowed=true;
  const turn={role:'user',text:'source',uid:'u',segment:0,record:1,prov:true};
  const ctx=makeFeature({chatPayload:{turns:[turn],live:'quiet',delivery:'ok',source:'acp',chat_started:'T0',prior_turns:0,assets:{}},deps:{canPreparePrompt:()=>allowed,preparePrompt:(...args)=>prepared.push(args)}});
  ctx.feature.bind(); await ctx.feature.render(); tapBubbleTurn(ctx,'i:0');
  assert.match(bubActionRows(ctx)[0].innerHTML,/data-bact="prompt"/);
  allowed=false; clickBact(ctx,'prompt'); assert.equal(prepared.length,0);
  allowed=true; clickBact(ctx,'prompt'); assert.equal(prepared.length,1);
  assert.equal(prepared[0][0].prov,true); assert.deepEqual(prepared[0][1],{uid:'u',segment:0,record:1}); ctx.feature.destroy();
});

test('owner discussion renders a prepared marker from the full shared address', async () => {
 const copy={uid:'u',key:'s/o/u/g',view:{server:'s',owner:'o',chat:'u',generation:'g'},names:{p:'Person'},people:{},mirrors:[{seq:1,t:'user',local_segment:0,local_record:1}],comments:[{seq:1,anchor:1,text:'reply',author:'p',at:'T'}]};
 const source={server:'s',owner:'o',chat:'u',generation:'g',log:'comments',seq:1};
 const storage={getItem:key=>key==='scimux-prompt-sources:n1'?JSON.stringify([source]):null,removeItem(){},setItem(){}};
 const ctx=makeFeature({storage,deps:{ownerDiscussion:()=>[copy],icons:{}},api:async path=>path.includes('/chat')?{turns:[{role:'user',uid:'u',segment:0,record:1,text:'turn'}],log_uid:'u',segment_index:0,live:'quiet',source:'codex'}:{}});
 ctx.feature.bind();await ctx.feature.render();assert.match(ctx.roots.msgs.innerHTML,/aria-label="Prepared as prompt"/);ctx.feature.destroy();
});

test('a local prompt source marker addresses the exact local record', async () => {
 const source={uid:'u',segment:0,record:1};
 const storage={getItem:key=>key==='scimux-prompt-sources:n1'?JSON.stringify([source]):null,removeItem(){},setItem(){}};
 const ctx=makeFeature({storage,chatPayload:{turns:[{...source,role:'assistant',text:'answer'}],live:'quiet',source:'acp'}});
 ctx.feature.bind();await ctx.feature.render();assert.match(ctx.roots.msgs.innerHTML,/aria-label="Prepared as prompt"/);ctx.feature.destroy();
});

test('an in-flight prompt source is marked awaiting delivery rather than confirmed', () => {
 const html=bubbleMarkersHTML({awaiting:true},{iconInto:'glyph'});
 assert.match(html,/aria-label="Awaiting prompt delivery"/);
 assert.doesNotMatch(html,/Sent as prompt/);
});

test('a uniquely addressed rejected prompt restores sources and the Agent draft', async () => {
 const source={uid:'u',segment:0,record:1}, flight={id:1,accepted:true,text:'draft',address:{uid:'u',segment:0,record:2},boundary:source,sources:[source]};
 const values=new Map([['scimux-prompt-flights:n1',JSON.stringify([flight])]]);
 const storage={getItem:k=>values.get(k)||null,setItem:(k,v)=>values.set(k,v),removeItem:k=>values.delete(k)};const recovered=[];
 const ctx=makeFeature({storage,chatPayload:{turns:[{...source,role:'assistant'},{uid:'u',record:2,role:'user',text:'draft'}],log_uid:'u',trace_outcome:'rejected',error:'dispatch rejected',turn_in_flight:false,live:'quiet',source:'acp'},deps:{recoverPromptDraft:(...args)=>recovered.push(args)}});
 ctx.feature.bind();await ctx.feature.render();assert.deepEqual(recovered,[['n1','draft',undefined]]);assert.equal(values.has('scimux-prompt-flights:n1'),false);assert.ok(values.has('scimux-prompt-sources:n1'));ctx.feature.destroy();
});

test('native prompt action recognizes an omitted zero segment', async () => {
 const prepared=[]; const turn={role:'user',text:'source',uid:'u',record:1};
 const ctx=makeFeature({chatPayload:{turns:[turn],live:'quiet',source:'acp'},deps:{canPreparePrompt:()=>true,preparePrompt:(...args)=>prepared.push(args)}});
 ctx.feature.bind();await ctx.feature.render();tapBubbleTurn(ctx);clickBact(ctx,'prompt');assert.equal(prepared[0][1].segment,0);ctx.feature.destroy();
});

test('in-flight source keeps its exact local marker while awaiting delivery', async () => {
 const source={uid:'u',segment:0,record:1}, flight={id:1,text:'queued',sources:[source]};
 const storage={getItem:key=>key==='scimux-prompt-flights:n1'?JSON.stringify([flight]):null,setItem(){},removeItem(){}};
 const ctx=makeFeature({storage,chatPayload:{turns:[{...source,role:'assistant',text:'answer'}],live:'quiet',source:'acp'}});
 ctx.feature.bind();await ctx.feature.render();assert.match(ctx.roots.msgs.innerHTML,/Awaiting prompt delivery/);ctx.feature.destroy();
});

test('an accepted prompt source has a distinct unconfirmed marker', async () => {
 const source={uid:'u',segment:0,record:1},flight={id:1,text:'accepted',accepted:true,sources:[source]};
 const storage={getItem:key=>key==='scimux-prompt-flights:n1'?JSON.stringify([flight]):null,setItem(){},removeItem(){}};
 const ctx=makeFeature({storage,chatPayload:{turns:[{...source,role:'assistant',text:'answer'}],live:'quiet',source:'acp'}});
 ctx.feature.bind();await ctx.feature.render();assert.match(ctx.roots.msgs.innerHTML,/Accepted; awaiting confirmation/);
 assert.doesNotMatch(ctx.roots.msgs.innerHTML,/Awaiting prompt delivery|Sent as prompt/);ctx.feature.destroy();
});

test('an accepted source with unavailable confirmation never claims a confirmed send', async () => {
 const source={uid:'u',segment:0,record:1},flight={id:1,text:'accepted',accepted:true,terminalUnknown:true,sources:[source]};
 const storage={getItem:key=>key==='scimux-prompt-flights:n1'?JSON.stringify([flight]):null,setItem(){},removeItem(){}};
 const ctx=makeFeature({storage,chatPayload:{turns:[{...source,role:'assistant',text:'answer'}],live:'quiet',source:'acp'}});
 ctx.feature.bind();await ctx.feature.render();assert.match(ctx.roots.msgs.innerHTML,/Accepted; confirmation unavailable/);
 assert.doesNotMatch(ctx.roots.msgs.innerHTML,/Sent as prompt/);ctx.feature.destroy();
});

test('prompt markers repaint the exact current local and discussion sources without a new poll', async () => {
 const local={uid:'u',segment:0,record:1},team={server:'s',owner:'o',chat:'c',generation:'g',log:'comments',seq:1};
 const values=new Map([['scimux-prompt-flights:n1',JSON.stringify([{id:1,text:'draft',accepted:true,sources:[local,team]}])]]);
 const storage={getItem:k=>values.get(k)||null,setItem:(k,v)=>values.set(k,v),removeItem:k=>values.delete(k)};
 const copy={key:'s/o/c/g',uid:'u',view:{...team,title:'Shared'},names:{o:'Owner'},people:{},mirrors:[],comments:[{seq:1,anchor:0,author:'o',mine:false,text:'comment'}]};
 const ctx=makeFeature({storage,chatPayload:{turns:[{...local,role:'assistant',text:'answer'}],log_uid:'u',segment_index:0,live:'quiet',source:'acp'},deps:{ownerDiscussion:()=>[copy]}});
 await ctx.feature.render();const updates=[];
 const bubble=label=>({querySelector:()=>({contains:()=>false,remove:()=>updates.push(label+' removed')}),insertAdjacentHTML:(_where,html)=>updates.push(label+' '+html)});
 const turn={dataset:{bk:'i:0'},querySelector:()=>bubble('local')},discussion={dataset:{gkey:'s/o/c/g',cseq:'1'},querySelector:()=>bubble('discussion')};
 ctx.roots.msgs.querySelectorAll=selector=>selector==='.turn[data-bk]'?[turn]:selector==='.gmsg[data-gkey][data-cseq]'?[discussion]:[];
 ctx.feature.refreshPromptMarkers('other');assert.deepEqual(updates,[]);
 ctx.feature.refreshPromptMarkers('n1');assert.equal(updates.length,4);assert.match(updates[1],/Accepted; awaiting confirmation/);assert.match(updates[3],/Accepted; awaiting confirmation/);
 values.clear();ctx.feature.refreshPromptMarkers('n1');assert.deepEqual(updates.slice(4),['local removed','discussion removed']);
 values.set('scimux-prompt-flights:n1',JSON.stringify([{id:2,accepted:true,sources:[local]}]));
 ctx.roots.msgs.querySelectorAll=selector=>selector==='.turn[data-bk]'?[{dataset:{bk:'missing'},querySelector:()=>null},{dataset:{bk:'i:0'},querySelector:()=>({querySelector:()=>null,insertAdjacentHTML:(_where,html)=>updates.push(html)})}]:[];
 ctx.feature.refreshPromptMarkers('n1');assert.match(updates.at(-1),/Accepted; awaiting confirmation/);
 ctx.roots.msgs.querySelectorAll=selector=>selector==='.turn[data-bk]'?[{dataset:{bk:'i:0'},querySelector:()=>null}]:[];
 ctx.feature.refreshPromptMarkers('n1');assert.equal(updates.length,7);
 ctx.feature.onSelectChange();ctx.setSel('team/s/o/c/g');ctx.setSelGen(2);ctx.feature.refreshPromptMarkers('n1');assert.equal(updates.length,7);ctx.feature.destroy();
});

test('refreshing prompt markers preserves focus on an existing bookmark control', async () => {
 const source={uid:'u',record:1},values=new Map([['scimux-prompt-flights:n1',JSON.stringify([{id:1,accepted:true,sources:[source]}])]]);
 const storage={getItem:k=>values.get(k)||null,setItem:(k,v)=>values.set(k,v),removeItem:k=>values.delete(k)};
 const focused={dataset:{bmarker:'bookmark'}};let restored=0;
 const doc={activeElement:focused,createElement:tag=>el(tag),querySelector:()=>null};
 const ctx=makeFeature({storage,chatPayload:{turns:[{...source,role:'assistant',text:'answer'}],live:'quiet',source:'acp'},deps:{document:doc}});
 await ctx.feature.render();const old={contains:x=>x===focused,remove(){}},replacement={focus:()=>restored++};
 const bubble={querySelector:selector=>selector==='.bubblemarkers'?old:selector==='[data-bmarker="bookmark"]'?replacement:null,insertAdjacentHTML(){}};
 ctx.roots.msgs.querySelectorAll=selector=>selector==='.turn[data-bk]'?[{dataset:{bk:'i:0'},querySelector:()=>bubble}]:[];
 ctx.feature.refreshPromptMarkers('n1');assert.equal(restored,1);ctx.feature.destroy();
});

test('owner shared timeline labels human prompts without a destination suffix', async () => {
 const copy={key:'s/o/c/g',uid:'u',view:{server:'s',owner:'o',chat:'c',generation:'g',title:'Shared'},names:{},people:{},mirrors:[],comments:[]};
 const ctx=makeFeature({chatPayload:{turns:[{uid:'u',record:1,role:'user',text:'human'}],log_uid:'u',live:'quiet',source:'acp'},deps:{ownerDiscussion:()=>[copy]}});
 await ctx.feature.render();assert.match(ctx.roots.msgs.innerHTML,/<div class="gwho">You<\/div>/);assert.doesNotMatch(ctx.roots.msgs.innerHTML,/You · Agent/);ctx.feature.destroy();
});

test('owner shared timeline names the actual assistant harness', async () => {
 const copy={key:'s/o/c/g',uid:'u',view:{server:'s',owner:'o',chat:'c',generation:'g',title:'Shared'},names:{},people:{},mirrors:[],comments:[]};
 const ctx=makeFeature({nodes:[{id:'n1',title:'Alpha',agent:'codex'}],chatPayload:{turns:[{uid:'u',record:1,role:'assistant',agent:'Codex',text:'answer'}],log_uid:'u',live:'quiet',source:'acp'},deps:{ownerDiscussion:()=>[copy]}});
 await ctx.feature.render();assert.match(ctx.roots.msgs.innerHTML,/<div class="gwho">Codex · agent<\/div>/);ctx.feature.destroy();
});

test('a non-human, non-assistant local record does not acquire a shared speaker label', async () => {
 const copy={key:'s/o/c/g',uid:'u',view:{server:'s',owner:'o',chat:'c',generation:'g',title:'Shared'},names:{},people:{},mirrors:[],comments:[]};
 const ctx=makeFeature({chatPayload:{turns:[{uid:'u',record:1,role:'system',text:'system record'}],log_uid:'u',live:'quiet'},deps:{ownerDiscussion:()=>[copy]}});
 await ctx.feature.render();assert.match(ctx.roots.msgs.innerHTML,/system record/);assert.doesNotMatch(ctx.roots.msgs.innerHTML,/class="gwho"/);ctx.feature.destroy();
});

test('retiring a chat with missing agent metadata leaves approval controls reset', async () => {
 const roots=makeRoots();roots.autoapprove=el('button');
 const ctx=makeFeature({roots,nodes:[{id:'n1',title:'Unknown'}],chatPayload:{turns:[],live:'quiet',source:'acp'}});
 await ctx.feature.render();ctx.feature.onSelectChange();
 assert.equal(ctx.roots.autoapprove.getAttribute('aria-pressed'),'false');ctx.feature.destroy();
});

test('chat navigation cancels description editing and retracts its optimistic echo', () => {
 const ctx=makeFeature();ctx.feature.setSentEcho({node:'n1',text:'queued'});ctx.feature.clearSentEchoFor('n1');ctx.feature.cancelEditorsForNav();ctx.feature.destroy();
});

test('poll restoration consults the Agent draft while People is selected', async () => {
 const restored=[];
 const ctx=makeFeature({chatPayload:{turns:[],live:'quiet',restore_draft:'failed Agent',error:'rejected',source:'acp'}, composerText:()=> 'People draft', deps:{agentDraftText:()=>'',restoreDraft:(id,text)=>restored.push([id,text])}});
 ctx.feature.bind(); await ctx.feature.render();
 assert.deepEqual(restored,[['n1','failed Agent']]); ctx.feature.destroy();
});

test('a positively evidenced trace persists the original source and exact destination', async () => {
 const source={uid:'u',segment:0,record:1}, address={uid:'u',segment:0,record:3};
 const values=new Map([['scimux-prompt-flights:n1',JSON.stringify([{id:1,text:'edited',sources:[source],address,accepted:true}])]]);
 const storage={getItem:k=>values.get(k)||null,setItem:(k,v)=>values.set(k,v),removeItem:k=>values.delete(k)};const ops=[];
 const ctx=makeFeature({storage,deps:{uiMutate:op=>ops.push(op)},api:async path=>path.includes('trace_uid=')?{turns:[{...address,role:'user',text:'edited'}],log_uid:'u',trace_outcome:'confirmed',live:'quiet',source:'acp'}:{}});
 ctx.feature.bind();await ctx.feature.render();
 assert.equal(ops.length,1);assert.equal(ops[0].link.kind,'prompt');assert.deepEqual(ops[0].link.source,source);assert.equal(ops[0].link.destination.record,3);
 assert.equal(values.has('scimux-prompt-flights:n1'),false);ctx.feature.destroy();
});

test('trace rejection restores only its captured sources and Agent text, preserving a newer source', async () => {
 const first={uid:'u',segment:0,record:1}, second={uid:'u',segment:0,record:4}, address={uid:'u',segment:0,record:2};
 const values=new Map([['scimux-prompt-sources:n1',JSON.stringify([second])],['scimux-prompt-flights:n1',JSON.stringify([{id:1,text:'first draft',sources:[first],address,accepted:true}])]]);
 const storage={getItem:k=>values.get(k)||null,setItem:(k,v)=>values.set(k,v),removeItem:k=>values.delete(k)};const recovered=[];
 const ctx=makeFeature({storage,chatPayload:{turns:[],log_uid:'u',trace_outcome:'rejected',source:'acp',live:'quiet'},deps:{recoverPromptDraft:(...args)=>recovered.push(args)}});
 ctx.feature.bind();await ctx.feature.render();assert.deepEqual(recovered,[['n1','first draft',undefined]]);assert.deepEqual(JSON.parse(values.get('scimux-prompt-sources:n1')),[second,first]);ctx.feature.destroy();
});

test('a stale log identity cannot confirm or recover an unrelated flight', async () => {
 const address={uid:'old',segment:0,record:1},values=new Map([['scimux-prompt-flights:n1',JSON.stringify([{id:1,text:'old draft',sources:[],address,accepted:true}])]]);
 const storage={getItem:k=>values.get(k)||null,setItem:(k,v)=>values.set(k,v),removeItem:k=>values.delete(k)};const effects=[];
 const ctx=makeFeature({storage,chatPayload:{turns:[],log_uid:'new',trace_outcome:'rejected',source:'acp',live:'quiet'},deps:{uiMutate:op=>effects.push(op),recoverPromptDraft:()=>effects.push('restore')}});
 ctx.feature.bind();await ctx.feature.render();assert.deepEqual(effects,[]);ctx.feature.destroy();
});

test('a normal jump works when no clock dependency is supplied', async () => {
 const ctx=makeFeature({chatPayload:{turns:[{uid:'u',segment:0,record:1,role:'user',text:'target'}],log_uid:'u',live:'quiet'},deps:{now:undefined}});
 ctx.setPending({node:'n1',uid:'u',segment:0,record:1,ts:Date.now()});ctx.feature.bind();await ctx.feature.render();ctx.feature.destroy();
});

test('a replaced log leaves its old flight unconfirmed and stops polling it', async () => {
 const address={uid:'old',segment:0,record:1},values=new Map([['scimux-prompt-flights:n1',JSON.stringify([{id:1,text:'draft',sources:[],address,accepted:true}])]]);
 const storage={getItem:k=>values.get(k)||null,setItem:(k,v)=>values.set(k,v),removeItem:k=>values.delete(k)};const effects=[];
 const ctx=makeFeature({storage,chatPayload:{turns:[],log_uid:'new',trace_outcome:'unknown',source:'acp',live:'quiet'},deps:{uiMutate:op=>effects.push(op),recoverPromptDraft:()=>effects.push('restore')}});
 ctx.feature.bind();await ctx.feature.render();assert.deepEqual(effects,[]);assert.equal(JSON.parse(values.get('scimux-prompt-flights:n1'))[0].terminalUnknown,true);ctx.feature.destroy();
});

test('prompt links keep full source identity and never appear as Send-to links', () => {
 const source={uid:'u',segment:0,record:2}, shared={server:'s',owner:'o',chat:'c',generation:'g',log:'comments',seq:1};
 const native={kind:'prompt',source,destination:{node:'n1',uid:'u',segment:0,record:5}};
 const human={kind:'prompt',source:shared,destination:{node:'n1',uid:'u',segment:0,record:6}};
 assert.deepEqual(chatmod.sentToMarkerModel(source,'n1',[native]),[]);
 assert.deepEqual(chatmod.promptLinksForSource(source,[native,human,null]),[native]);
 assert.deepEqual(chatmod.promptLinksForSource({...shared,generation:'other'},[human]),[]);
 assert.deepEqual(chatmod.promptLinksForSource({},[native]),[]);
  assert.match(bubbleMarkersHTML({promptLinks:[native]},{iconInto:'glyph'}),/aria-label="Sent as prompt"/);
});

test('native prompt markers jump to exact addresses and offer a chooser', async () => {
 const source={uid:'u',segment:0,record:2}, jumps=[], choices=[], toasts=[];
 let links=[{kind:'prompt',source,destination:{node:'n1',uid:'u',segment:1,record:9}}];
 const ctx=makeFeature({chatPayload:{turns:[{...source,role:'assistant',text:'answer'}],live:'quiet',source:'acp'},deps:{forwardLinks:()=>links,jumpToChatAddress:a=>{jumps.push(a);return false},toast:m=>toasts.push(m),openChoiceList:o=>choices.push(o)}});
 ctx.feature.bind();await ctx.feature.render();assert.match(ctx.roots.msgs.innerHTML,/Sent as prompt/);
 clickBubbleMarker(ctx,'i:0','prompt');assert.deepEqual(jumps,[links[0].destination]);assert.equal(toasts.length,1);
 links=[...links,{kind:'prompt',source,destination:{node:'gone',uid:'u',segment:2,record:7}}];
 clickBubbleMarker(ctx,'i:0','prompt');assert.equal(choices[0].choices.length,2);choices[0].onChoose(choices[0].choices[1]);assert.deepEqual(jumps[1],links[1].destination);ctx.feature.destroy();
});

test('an unknown trace preserves its source while ceasing trace polling', async () => {
 const source={uid:'u',record:1},address={uid:'u',record:2},values=new Map([['scimux-prompt-flights:n1',JSON.stringify([{id:1,text:'draft',sources:[source],address,accepted:true}])]]);
 const storage={getItem:k=>values.get(k)||null,setItem:(k,v)=>values.set(k,v),removeItem:k=>values.delete(k)};const ctx=makeFeature({storage,chatPayload:{turns:[],log_uid:'u',trace_outcome:'unknown',live:'quiet',source:'acp'}});
 ctx.feature.bind();await ctx.feature.render();assert.equal(JSON.parse(values.get('scimux-prompt-flights:n1'))[0].terminalUnknown,true);assert.deepEqual(JSON.parse(values.get('scimux-prompt-flights:n1'))[0].sources,[source]);ctx.feature.destroy();
});

test('confirmation of T1 never consumes a newer T2 source', async () => {
 const first={uid:'u',record:1,prov:true,role:'user'},second={uid:'u',record:3},address={uid:'u',record:2};
 const values=new Map([['scimux-prompt-sources:n1',JSON.stringify([second])],['scimux-prompt-flights:n1',JSON.stringify([{id:1,text:'edited',sources:[first],address,accepted:true}])]]);
 const storage={getItem:k=>values.get(k)||null,setItem:(k,v)=>values.set(k,v),removeItem:k=>values.delete(k)};const ops=[];
 const ctx=makeFeature({storage,chatPayload:{turns:[],log_uid:'u',trace_outcome:'confirmed',live:'quiet',source:'acp'},deps:{uiMutate:op=>ops.push(op)}});
 ctx.feature.bind();await ctx.feature.render();assert.deepEqual(ops[0].link.source,first);assert.equal(ops[0].link.preview,'edited');assert.deepEqual(ops[0].link.destination,{node:'n1',uid:'u',segment:0,record:2});assert.deepEqual(JSON.parse(values.get('scimux-prompt-sources:n1')),[second]);ctx.feature.destroy();
});

test('prompt marker matching tolerates absent links and unavailable destinations', async () => {
 const source={uid:'u',record:2};assert.deepEqual(chatmod.promptLinksForSource(source,null),[]);
 const links=[{kind:'prompt',source}];const opened=[];
 const ctx=makeFeature({chatPayload:{turns:[{...source,role:'assistant',text:'source'}],live:'quiet'},deps:{forwardLinks:()=>links,jumpToChatAddress:a=>{opened.push(a);return true}}});
 ctx.feature.bind();await ctx.feature.render();clickBubbleMarker(ctx,'i:0','prompt');assert.deepEqual(opened,[]);clickBubbleMarker(ctx,'missing','prompt');ctx.feature.destroy();
});

test('human prompt marker opens the matching shared source destination', async () => {
 const copy={uid:'u',key:'s/o/c/g',view:{server:'s',owner:'o',chat:'c',generation:'g'},names:{p:'Person'},people:{},mirrors:[{seq:1,t:'user',local_segment:0,local_record:1}],comments:[{seq:1,anchor:1,text:'reply',author:'p',at:'T'}]};
 const source={...copy.view,log:'comments',seq:1},dest={node:'n1',uid:'u',segment:0,record:3}, jumps=[];
 const ctx=makeFeature({deps:{ownerDiscussion:()=>[copy],forwardLinks:()=>[{kind:'prompt',source,destination:dest}],jumpToChatAddress:a=>jumps.push(a)},chatPayload:{turns:[{role:'user',uid:'u',record:1,text:'turn'}],log_uid:'u',live:'quiet'}});
 ctx.feature.bind();await ctx.feature.render();assert.match(ctx.roots.msgs.innerHTML,/Sent as prompt/);
 const turn=el('div',{className:'turn',dataset:{gkey:copy.key,cseq:'1'}}),marker=el('button',{dataset:{bmarker:'prompt'}});turn.appendChild(marker);ctx.roots.msgs.appendChild(turn);firstListener(ctx.roots.msgs,'click')({target:marker});assert.deepEqual(jumps,[dest]);ctx.feature.destroy();
});

test('legacy poll restoration reads stored Agent text if the visible composer is empty', async () => {
 const values=new Map([['scimux-draft:n1','newer']]);const storage={getItem:k=>values.get(k)||null,setItem:(k,v)=>values.set(k,v),removeItem:k=>values.delete(k)}, restored=[];
 const ctx=makeFeature({storage,composerText:()=>'',restoreDraft:(...args)=>restored.push(args),chatPayload:{turns:[],restore_draft:'old',live:'quiet'}});
 ctx.feature.bind();await ctx.feature.render();assert.deepEqual(restored,[]);assert.equal(values.get('scimux-draft:n1'),'newer');ctx.feature.destroy();
});

test('settling an unknown trace preserves other independent flights', async () => {
 for(const uid of ['u','different']){
  const address={uid:'u',record:2},other={id:2,text:'other',sources:[],address:{uid:'u',record:7},accepted:true};
  const values=new Map([['scimux-prompt-flights:n1',JSON.stringify([{id:1,text:'one',sources:[],address,accepted:true},other])]]);
  const storage={getItem:k=>values.get(k)||null,setItem:(k,v)=>values.set(k,v),removeItem:k=>values.delete(k)};
  const ctx=makeFeature({storage,chatPayload:{turns:[],log_uid:uid,trace_outcome:'unknown',live:'quiet'}});ctx.feature.bind();await ctx.feature.render();
  const flights=JSON.parse(values.get('scimux-prompt-flights:n1'));assert.equal(flights[0].terminalUnknown,true);assert.deepEqual(flights[1],other);ctx.feature.destroy();
 }
});

test('confirmed prompt link remains navigable on an earlier history bubble', async () => {
 const source={uid:'u',segment:0,record:1},dest={node:'n1',uid:'u',segment:1,record:4},jumps=[];
 const ctx=makeFeature({histSegs:[{segment_index:0,turns:[{...source,role:'assistant',text:'old answer'}]}],chatPayload:{turns:[{uid:'u',segment:1,record:4,role:'user',text:'edited'}],prior_turns:1,live:'quiet'},deps:{forwardLinks:()=>[{kind:'prompt',source,destination:dest}],jumpToChatAddress:a=>jumps.push(a)}});
 ctx.feature.bind();await ctx.feature.loadHistory('n1','');assert.match(markerChunk(ctx.roots.msgs.innerHTML,'h:0:0'),/Sent as prompt/);clickBubbleMarker(ctx,'h:0:0','prompt');assert.deepEqual(jumps,[dest]);ctx.feature.destroy();
});

test('prepared and confirmed prompt source bubbles remain visible when folded', async () => {
 const turns=twoRangeTurns(),source={uid:'log-1',segment:0,record:3},link={kind:'prompt',source,destination:{node:'n1',uid:'log-1',record:1}};
 const {ctx,html}=await renderFolded({turns,state:{all:true,turns:[],open:[]},echo:false,deps:{...twoRangeDeps(),forwardLinks:()=>[link]}});
 assert.match(html(),/>left-a</);assert.match(html(),/Sent as prompt/);ctx.feature.destroy();
});

test('correlated interval error passes captured attachments to Agent recovery', async () => {
 const attachments=[{name:'input.txt',ref:{path:'owned',mime:'text/plain'}}],values=new Map([['scimux-prompt-flights:n1',JSON.stringify([{id:1,text:'draft',sources:[],address:{uid:'u',record:1},accepted:true,attachments}])]]),recovered=[];
 const storage={getItem:k=>values.get(k)||null,setItem:(k,v)=>values.set(k,v),removeItem:k=>values.delete(k)};
 const ctx=makeFeature({storage,chatPayload:{turns:[],log_uid:'u',trace_outcome:'rejected',live:'quiet'},deps:{recoverPromptDraft:(...args)=>recovered.push(args)}});
 ctx.feature.bind();await ctx.feature.render();assert.deepEqual(recovered,[['n1','draft',attachments]]);ctx.feature.destroy();
});

test('a pending jump is consumed even when the transcript render is unchanged', async () => {
 const target={uid:'u',segment:0,record:1,role:'user',text:'destination'};
 const ctx=makeFeature({chatPayload:{turns:[target],log_uid:'u',live:'quiet'}});ctx.feature.bind();await ctx.feature.render();
 const element=el('div',{className:'turn user',dataset:{i:'0'}});ctx.roots.msgs.appendChild(element);
 ctx.setPending({node:'n1',uid:'u',segment:0,record:1,ts:1_000_000});await ctx.feature.render();
 assert.equal(ctx.getPending(),null);assert.ok(element.classList.contains('jumpflash'));ctx.feature.destroy();
});

test('a 304 racing a new jump triggers a fresh body rather than dropping the jump', async () => {
 let calls=0;const payload={turns:[{uid:'u',record:1,role:'user',text:'target'}],log_uid:'u',live:'quiet'};
 const ctx=makeFeature({chatConditional:async()=>{calls++;return calls===2?{status:304}:{status:200,etag:'tag',data:payload}}});
 ctx.feature.bind();await ctx.feature.render();ctx.setPending({node:'n1',uid:'u',record:1,ts:1_000_000});await ctx.feature.render();await new Promise(r=>setImmediate(r));
 assert.equal(calls,3);assert.equal(ctx.getPending(),null);ctx.feature.destroy();
});

test('a startup restore cannot consume an unrelated identical in-flight source', async () => {
 const source={uid:'old',record:1},values=new Map([['scimux-prompt-flights:n1',JSON.stringify([{id:1,text:'same text',sources:[source],accepted:true}])]]);
 const storage={getItem:k=>values.get(k)||null,setItem:(k,v)=>values.set(k,v),removeItem:k=>values.delete(k)};
 const ctx=makeFeature({storage,chatPayload:{turns:[],log_uid:'new',restore_draft:'same text',live:'quiet'}});ctx.feature.bind();await ctx.feature.render();
 assert.equal(values.has('scimux-prompt-sources:n1'),false);assert.ok(values.has('scimux-prompt-flights:n1'));ctx.feature.destroy();
});
// Independent coordinator regression: stale pending work must not starve a new log.
test('coordinator stale trace cannot starve the new log flight', async()=>{
 const old={id:1,text:'old draft',sources:[],address:{uid:'old',segment:0,record:1},accepted:true};
 const current={id:2,text:'new edited prompt',sources:[{uid:'new',segment:0,record:1}],address:{uid:'new',segment:0,record:3},accepted:true};
 const values=new Map([['scimux-prompt-flights:n1',JSON.stringify([old,current])]]);
 const storage={getItem:k=>values.get(k)||null,setItem:(k,v)=>values.set(k,v),removeItem:k=>values.delete(k)};
 const ops=[],restored=[],paths=[];
 const ctx=makeFeature({storage,deps:{uiMutate:op=>ops.push(op),recoverPromptDraft:(...args)=>restored.push(args)},api:async path=>{paths.push(path);return {turns:[{uid:'new',segment:0,record:3,role:'user',text:'new edited prompt'}],log_uid:'new',live:'quiet',source:'acp',trace_outcome:path.includes('trace_uid=new')?'confirmed':'unknown'};}});
 ctx.feature.bind();await ctx.feature.render();await ctx.feature.render();
 assert.deepEqual(restored,[],'stale flight must never recover into the new chat');
 assert.equal(ops.length,1,'a stale UID flight must not permanently prevent the current exact-address flight from confirming');
 assert.equal(ops[0].link.destination.uid,'new');ctx.feature.destroy();
});

test('coordinator stale mismatch result cannot retire a reused flight id', async()=>{
 const old={id:1,text:'old draft',sources:[],address:{uid:'old',segment:0,record:1},accepted:true};
 const current={id:1,text:'new edited prompt',sources:[{uid:'new',segment:0,record:1}],address:{uid:'new',segment:0,record:3},accepted:true};
 const values=new Map([['scimux-prompt-flights:n1',JSON.stringify([old])]]);
 const storage={getItem:k=>values.get(k)||null,setItem:(k,v)=>values.set(k,v),removeItem:k=>values.delete(k)};
 const ops=[],restored=[];let calls=0;
 const ctx=makeFeature({storage,deps:{uiMutate:op=>ops.push(op),recoverPromptDraft:(...args)=>restored.push(args)},api:async path=>{
  if(calls++===0)values.set('scimux-prompt-flights:n1',JSON.stringify([current]));
  return {turns:[{uid:'new',segment:0,record:3,role:'user',text:'new edited prompt'}],log_uid:'new',live:'quiet',source:'acp',trace_outcome:path.includes('trace_uid=new')?'confirmed':'unknown'};
 }});
 ctx.feature.bind();await ctx.feature.render();await ctx.feature.render();
 assert.deepEqual(restored,[]);assert.equal(ops.length,1,'a stale mismatched response must match address as well as reused id before retiring a flight');
 assert.equal(ops[0].link.destination.uid,'new');ctx.feature.destroy();
});

test('coordinator trace ETag is scoped to the exact queried resource', async()=>{
 const source={uid:'u',segment:0,record:1};
 const flights=[{id:1,text:'one',sources:[source],address:{uid:'u',segment:0,record:3},accepted:true},{id:2,text:'two',sources:[source],address:{uid:'u',segment:0,record:5},accepted:true}];
 const values=new Map([['scimux-prompt-flights:n1',JSON.stringify(flights)]]);
 const storage={getItem:k=>values.get(k)||null,setItem:(k,v)=>values.set(k,v),removeItem:k=>values.delete(k)};const ops=[];
 const payload={turns:[{uid:'u',segment:0,record:3,role:'user',text:'one'},{uid:'u',segment:0,record:5,role:'user',text:'two'}],log_uid:'u',live:'quiet',source:'acp',trace_outcome:'confirmed'};
 const ctx=makeFeature({storage,deps:{uiMutate:op=>ops.push(op),apiConditionalGet:async(path,held)=>held==='stable-body'?{status:304,etag:'stable-body'}:{status:200,etag:'stable-body',data:payload}}});
 ctx.feature.bind();await ctx.feature.render();await ctx.feature.render();await ctx.feature.render();
 assert.equal(ops.length,2,'an identical-body 304 across different trace addresses must not block the next confirmed flight');
 assert.deepEqual(ops.map(op=>op.link.destination.record),[3,5]);ctx.feature.destroy();
});

test('an active discussion editor survives a changed timeline and keeps its anchor', async () => {
 const {captureDiscussionEditor, restoreDiscussionEditor}=await import('../js/chat.js');
 let focused=false, selection=null;
 const input={selectionStart:3,selectionEnd:7,selectionDirection:'forward',focus(){focused=true},setSelectionRange(...v){selection=v}};
 const turn={dataset:{gkey:'s/o/u/g',cseq:'2'},getBoundingClientRect:()=>({top:120}),after(row){this.row=row}};
 const row={previousElementSibling:turn,contains:x=>x===input}; input.closest=()=>row;
 const doc={activeElement:input}; const body={scrollTop:80,querySelector:()=>input};
 const saved=captureDiscussionEditor(body,doc);
 const fresh={dataset:{gkey:'s/o/u/g',cseq:'2'},replaceWith(old){this.replaced=old}};
 body.querySelectorAll=()=>[fresh]; turn.getBoundingClientRect=()=>({top:150});
 assert.equal(restoreDiscussionEditor(body,saved),true);
 assert.equal(fresh.replaced,turn); assert.equal(turn.row,row); assert.equal(focused,true);
 assert.deepEqual(selection,[3,7,'forward']); assert.equal(body.scrollTop,110);
});

test('an open discussion action row survives a local timeline rebuild', async () => {
 const {captureDiscussionEditor,restoreDiscussionEditor}=await import('../js/chat.js');let focused=false;
 const turn={dataset:{gkey:'s/o/u/g',cseq:'2'},getBoundingClientRect:()=>({top:120}),after(row){this.row=row}};
 const button={focus(){focused=true}},row={previousElementSibling:turn,contains:x=>x===button};
 const body={scrollTop:80,querySelector:s=>s==='[data-gactions]'?row:null};
 const saved=captureDiscussionEditor(body,{activeElement:button});assert.ok(saved);
 const fresh={dataset:{...turn.dataset},replaceWith(old){this.replaced=old}};body.querySelectorAll=()=>[fresh];
 assert.equal(restoreDiscussionEditor(body,saved),true);assert.equal(fresh.replaced,turn);assert.equal(turn.row,row);assert.equal(focused,true);
});

test('retaining a discussion action row keeps newly rendered source markers', async () => {
 const {captureDiscussionEditor,restoreDiscussionEditor}=await import('../js/chat.js');
 const turn={dataset:{gkey:'s/o/u/g',cseq:'2'},innerHTML:'<div class="bubble">message</div>',getBoundingClientRect:()=>({top:120}),after(row){this.row=row}};
 const row={previousElementSibling:turn,contains:()=>false},body={scrollTop:0,querySelector:s=>s==='[data-gactions]'?row:null};
 const saved=captureDiscussionEditor(body,{}),fresh={dataset:{...turn.dataset},innerHTML:'<div class="bubble"><button data-bmarker="prompt">confirmed</button>message</div>',replaceWith(){}};
 body.querySelectorAll=()=>[fresh];assert.equal(restoreDiscussionEditor(body,saved),true);assert.match(turn.innerHTML,/confirmed/);
});

test('a focused discussion marker remains focused when its bubble refreshes', async () => {
 const {captureDiscussionEditor,restoreDiscussionEditor}=await import('../js/chat.js');let focused=false;
 const oldMarker={dataset:{bmarker:'prompt'}},newMarker={focus(options){focused=options.preventScroll}};
 const turn={dataset:{gkey:'s/o/u/g',cseq:'2'},innerHTML:'old',contains:value=>value===oldMarker,querySelector:s=>s==='[data-bmarker="prompt"]'?newMarker:null,getBoundingClientRect:()=>({top:120}),after(){}};
 const row={previousElementSibling:turn,contains:()=>false},body={scrollTop:0,querySelector:s=>s==='[data-gactions]'?row:null};
 const saved=captureDiscussionEditor(body,{activeElement:oldMarker}),fresh={dataset:{...turn.dataset},innerHTML:'new',replaceWith(){}};
 body.querySelectorAll=()=>[fresh];assert.equal(restoreDiscussionEditor(body,saved),true);assert.equal(focused,true);
});

test('a focused discussion bubble remains focused when its content refreshes', async () => {
 const {captureDiscussionEditor,restoreDiscussionEditor}=await import('../js/chat.js');let focused=false;
 const oldBubble={classList:{contains:name=>name==='bubble'}},newBubble={focus(options){focused=options.preventScroll}};
 const turn={dataset:{gkey:'s/o/u/g',cseq:'2'},innerHTML:'old',contains:value=>value===oldBubble,querySelector:s=>s==='.bubble'?newBubble:null,getBoundingClientRect:()=>({top:120}),after(){}};
 const row={previousElementSibling:turn,contains:()=>false},body={scrollTop:0,querySelector:s=>s==='[data-gactions]'?row:null};
 const saved=captureDiscussionEditor(body,{activeElement:oldBubble}),fresh={dataset:{...turn.dataset},innerHTML:'new',replaceWith(){}};
 body.querySelectorAll=()=>[fresh];assert.equal(restoreDiscussionEditor(body,saved),true);assert.equal(focused,true);
});

test('an exact source jump wins over an open editor anchor on a long rebuilt timeline', async () => {
 let turns = [{uid:'u',segment:0,record:1,role:'user',text:'destination'}];
 const doc = {activeElement:null, createElement:tag=>el(tag), querySelector:()=>null};
 const ctx = makeFeature({deps:{document:doc},api:async()=>({turns,log_uid:'u',live:'quiet'})});
 const body = ctx.roots.msgs; body.scrollHeight=10000; body.clientHeight=700;
 ctx.feature.bind(); await ctx.feature.render(); body.scrollTop=4700;
 let absoluteTop=5000, selected=null;
 const input={value:'Editing person',selectionStart:3,selectionEnd:7,selectionDirection:'forward',closest:()=>row,
   focus(){doc.activeElement=input},setSelectionRange(...args){selected=args}};
 const turn={dataset:{gkey:'s/o/u/g',cseq:'2'},getBoundingClientRect:()=>({top:absoluteTop-body.scrollTop}),after(value){this.row=value}};
 const row={previousElementSibling:turn,contains:node=>node===input}; doc.activeElement=input;
 const target=el('div',{className:'turn',dataset:{i:'0'}}); target.scrollIntoView=()=>{body.scrollTop=1000};
 const fresh={dataset:{...turn.dataset},replaceWith(value){this.old=value}};
 const query=body.querySelector.bind(body), all=body.querySelectorAll.bind(body);
 body.querySelector=selector=>selector==='[data-grename]'?input:selector==='.turn[data-i="0"]'?target:query(selector);
 body.querySelectorAll=selector=>selector==='[data-gkey][data-cseq]'?[fresh]:all(selector);
 ctx.setPending({node:'n1',uid:'u',segment:0,record:1,ts:1_000_000});
 turns=[...turns,{uid:'u',segment:0,record:2,role:'assistant',text:'new data'}]; absoluteTop+=100;
 await ctx.feature.render();
 assert.equal(ctx.getPending(),null); assert.equal(body.scrollTop,1000,'the explicitly requested destination stays in view');
 assert.equal(fresh.old,turn); assert.equal(turn.row,row); assert.equal(input.value,'Editing person');
 assert.equal(doc.activeElement,input); assert.deepEqual(selected,[3,7,'forward']); ctx.feature.destroy();
});

test('history navigation wins over an editor anchor, while polling and a different UID preserve it', async () => {
 for (const mode of ['history','poll','different-uid']) {
  let turns=[{uid:'u',segment:0,record:1,role:'user',text:'destination'}];
  const doc={activeElement:null,createElement:tag=>el(tag),querySelector:()=>null};
  const ctx=makeFeature({deps:{document:doc},api:async path=>path.includes('history=1')?{segments:[{segment_index:0,turns:[]}]}:{turns,log_uid:'u',live:'quiet'}});
  const body=ctx.roots.msgs;body.scrollHeight=10000;body.clientHeight=700;
  ctx.feature.bind();await ctx.feature.render();body.scrollTop=4700;
  let absoluteTop=5000, selection=null;
  const input={value:'unsaved',selectionStart:2,selectionEnd:5,selectionDirection:'forward',closest:()=>row,focus(){doc.activeElement=input},setSelectionRange(...v){selection=v}};
  const turn={dataset:{gkey:'s/o/u/g',cseq:'2'},getBoundingClientRect:()=>({top:absoluteTop-body.scrollTop}),after(r){this.row=r}};
  const row={previousElementSibling:turn,contains:n=>n===input};doc.activeElement=input;
  const fresh={dataset:{...turn.dataset},replaceWith(v){this.old=v}};
  const target=el('div',{className:'turn',dataset:{i:'0'}});target.scrollIntoView=()=>{body.scrollTop=1000};
  const query=body.querySelector.bind(body),all=body.querySelectorAll.bind(body);
  body.querySelector=s=>s==='[data-grename]'?input:s==='.turn[data-i="0"]'||s==='.curseam'?target:query(s);
  body.querySelectorAll=s=>s==='[data-gkey][data-cseq]'?[fresh]:all(s);
  const html=Object.getOwnPropertyDescriptor(body,'innerHTML');
  Object.defineProperty(body,'innerHTML',{configurable:true,get:html.get,set(value){absoluteTop+=100;html.set(value)}});
  if(mode==='history') await ctx.feature.loadHistory('n1','seam');
  else {
   if(mode==='different-uid')ctx.setPending({node:'n1',uid:'other',segment:0,record:1,ts:1_000_000});
   turns=[...turns,{uid:'u',segment:0,record:2,role:'assistant',text:'new'}];await ctx.feature.render();
  }
  assert.equal(body.scrollTop,mode==='history'?1000:4800,mode);
  assert.equal(fresh.old,turn);assert.equal(turn.row,row);assert.equal(input.value,'unsaved');
  assert.equal(doc.activeElement,input);assert.deepEqual(selection,[2,5,'forward']);
  if(mode==='different-uid')assert.equal(target.classList.contains('jumpflash'),false);
  ctx.feature.destroy();
 }
});

test('an active exact jump survives rebuild only for its own address and original deadline', async () => {
 const {activeJumpTarget}=await import('../js/chat.js');
 const active={node:'n1',uid:'u',segment:1,record:4,expires:1600};
 const turns=[{uid:'u',segment:1,record:3},{uid:'u',segment:1,record:4}];
 assert.equal(activeJumpTarget(active,'n1',turns,1599),1);
 assert.equal(activeJumpTarget(active,'n1',turns,1600),-1);
 assert.equal(activeJumpTarget(active,'n2',turns,100),-1);
 assert.equal(activeJumpTarget(active,'n1',[{uid:'other',segment:1,record:4}],100),-1);
 assert.equal(activeJumpTarget(active,'n1',turns,100,'new-uid'),-1);
});

test('discussion editor restoration ignores absent or replaced comments and preserves other focused actions', async () => {
 const {captureDiscussionEditor,restoreDiscussionEditor}=await import('../js/chat.js');
 assert.equal(captureDiscussionEditor({querySelector:()=>null},{}),null);
 assert.equal(restoreDiscussionEditor({},null),false);
 const button={focused:false,focus(){this.focused=true}};
 const turn={dataset:{gkey:'a',cseq:'4'},getBoundingClientRect:()=>({top:40}),after(){}};
 const row={previousElementSibling:turn,contains:x=>x===button};
 const input={closest:()=>row,selectionStart:1,selectionEnd:1,selectionDirection:'none'};
 const body={scrollTop:20,querySelector:()=>input,querySelectorAll:()=>[]};
 const saved=captureDiscussionEditor(body,{activeElement:button});
 assert.equal(restoreDiscussionEditor(body,saved),false);
 body.querySelectorAll=()=>[{dataset:{gkey:'b',cseq:'4'},replaceWith(){throw Error('wrong comment')}}];
 assert.equal(restoreDiscussionEditor(body,saved),false);
 body.querySelectorAll=()=>[{dataset:{gkey:'a',cseq:'4'},replaceWith(){}}];
 assert.equal(restoreDiscussionEditor(body,saved),true); assert.equal(button.focused,true);
 const noFocus=captureDiscussionEditor(body,{activeElement:{}});
 assert.equal(restoreDiscussionEditor(body,noFocus),true);
});

test('a changed timeline reapplies an exact active jump to its replacement turn', async () => {
 let turns=[{uid:'u',segment:0,record:1,role:'user',text:'target'}];
 const ctx=makeFeature({api:async path=>path.includes('/chat')?{turns,log_uid:'u',live:'quiet'}:{}});
 let target=el('div',{className:'turn',dataset:{i:'0'}});
 const originalQuery=ctx.roots.msgs.querySelector.bind(ctx.roots.msgs);
 ctx.roots.msgs.querySelector=selector=>selector==='.turn[data-i="0"]'?target:originalQuery(selector);
 ctx.feature.bind(); await ctx.feature.render();
 ctx.setPending({node:'n1',uid:'u',segment:0,record:1,ts:1_000_000}); await ctx.feature.render();
 assert.ok(target.classList.contains('jumpflash'));
 target=el('div',{className:'turn',dataset:{i:'0'}});
 turns=[...turns,{uid:'u',segment:0,record:2,role:'assistant',text:'new'}];
 await ctx.feature.render(); assert.ok(target.classList.contains('jumpflash'));
 ctx.feature.destroy();
});

test('discussion editor anchor accounts for the actual post-render scroll offset', async () => {
 const {captureDiscussionEditor,restoreDiscussionEditor}=await import('../js/chat.js');
 for (const [before, after, insertion] of [[80,120,30],[80,0,30],[80,200,-20]]) {
  let top=200, selection, focused=false;
  const body={scrollTop:before};
  const turn={dataset:{gkey:'s/o/u/g',cseq:'2'},getBoundingClientRect:()=>({top:top-body.scrollTop}),after(row){this.row=row}};
  const input={value:'Editing person',selectionStart:3,selectionEnd:7,selectionDirection:'forward',closest:()=>row,focus(){focused=true},setSelectionRange(...args){selection=args}};
  const row={previousElementSibling:turn,contains:x=>x===input};
  body.querySelector=()=>input;
  const visualTop=turn.getBoundingClientRect().top;
  const saved=captureDiscussionEditor(body,{activeElement:input});
  body.scrollTop=after; top+=insertion;
  const fresh={dataset:{...turn.dataset},replaceWith(value){this.replaced=value}};
  body.querySelectorAll=()=>[fresh];
  assert.equal(restoreDiscussionEditor(body,saved),true);
  assert.equal(body.scrollTop,before+insertion);
  assert.equal(turn.getBoundingClientRect().top,visualTop);
  assert.equal(fresh.replaced,turn); assert.equal(turn.row,row); assert.equal(row.previousElementSibling,turn);
  assert.equal(input.value,'Editing person'); assert.ok(focused); assert.deepEqual(selection,[3,7,'forward']);
 }
});

test('highlight ignores empty turn lists and a text-only jump is never retained', async () => {
 const {activeJumpTarget}=await import('../js/chat.js');
 assert.equal(activeJumpTarget({node:'n1',uid:'u',record:1,expires:10},'n1',null,1),-1);
 const ctx=makeFeature({chatPayload:{turns:[{role:'user',text:'target'}],live:'quiet'}});
 const target=el('div',{className:'turn',dataset:{i:'0'}}); const query=ctx.roots.msgs.querySelector.bind(ctx.roots.msgs);
 ctx.roots.msgs.querySelector=s=>s==='.turn[data-i="0"]'?target:query(s);
 ctx.feature.bind(); await ctx.feature.render(); ctx.setPending({node:'n1',text:'target',ts:1_000_000}); await ctx.feature.render();
 assert.ok(target.classList.contains('jumpflash')); ctx.feature.destroy();
});

test('history jumps retain their exact highlight through a timeline rebuild', async () => {
 let turns=[{uid:'u',segment:1,record:4,role:'user',text:'old'}];
 const ctx=makeFeature({histSegs:[{segment_index:0,turns:[{uid:'u',segment:0,record:1,role:'assistant',text:'old'}]}],api:async path=>path.includes('history=1')?{segments:[{segment_index:0,turns:[{uid:'u',segment:0,record:1,role:'assistant',text:'old'}]}]}:{turns,log_uid:'u',prior_turns:1,live:'quiet'}});
 let hist=el('div',{className:'turn hist',dataset:{uid:'u',segment:'0',record:'1'}});
 const all=ctx.roots.msgs.querySelectorAll.bind(ctx.roots.msgs);
 ctx.roots.msgs.querySelectorAll=s=>s==='.turn.hist'?[hist]:all(s);
 ctx.feature.bind(); await ctx.feature.loadHistory('n1',''); ctx.setPending({node:'n1',uid:'u',segment:0,record:1,ts:1_000_000}); await ctx.feature.render();
 assert.ok(hist.classList.contains('jumpflash'), 'same text in the live segment must not replace the exact history address');
 hist=el('div',{className:'turn hist',dataset:{uid:'u',segment:'0',record:'1'}});
 turns=[...turns,{uid:'u',segment:1,record:5,role:'assistant',text:'later'}]; await ctx.feature.render();
 assert.ok(hist.classList.contains('jumpflash'));ctx.feature.destroy();
});

test('expired, replaced and missing exact highlight targets are not revived', async () => {
 for (const scenario of ['expired','new-uid','missing','clock']) {
  let time=1_000_000, turns=[{uid:'u',segment:0,record:1,role:'user',text:'target'}], uid='u';
  const ctx=makeFeature({deps:{now:scenario==='clock'?null:()=>time},api:async path=>path.includes('/chat')?{turns,log_uid:uid,live:'quiet'}:{}});
  let target=el('div',{className:'turn',dataset:{i:'0'}}); const query=ctx.roots.msgs.querySelector.bind(ctx.roots.msgs);
  ctx.roots.msgs.querySelector=s=>s==='.turn[data-i="0"]'?target:query(s);
  ctx.feature.bind();await ctx.feature.render();
  ctx.setPending({node:'n1',uid:'u',segment:0,record:1,ts:scenario==='clock'?Date.now():time});await ctx.feature.render();
  if(scenario==='expired') time+=1601;
  if(scenario==='new-uid') uid='other';
  if(scenario==='missing') turns=[];
  if(scenario==='clock') turns=[...turns,{uid:'u',record:2,role:'assistant',text:'new'}];
  if(scenario==='new-uid') turns=[{uid:'other',record:1,role:'user',text:'replacement'}];
  if(scenario==='expired') turns=[...turns,{uid:'u',record:2,role:'assistant',text:'later'}];
  target=el('div',{className:'turn',dataset:{i:'0'}}); await ctx.feature.render();
  assert.equal(target.classList.contains('jumpflash'),scenario==='clock');ctx.feature.destroy();
 }
});

test('a time-only history jump flashes without retaining an exact address', async () => {
 const ctx=makeFeature({histSegs:[{segment_index:0,turns:[{role:'assistant',time:'old',text:'old'}]}],chatPayload:{turns:[{role:'user',text:'new'}],prior_turns:1,live:'quiet'}});
 const hist=el('div',{className:'turn hist',dataset:{uid:'',segment:'0',record:'0',time:'old'}});
 const all=ctx.roots.msgs.querySelectorAll.bind(ctx.roots.msgs);
 ctx.roots.msgs.querySelectorAll=s=>s==='.turn.hist'?[hist]:all(s);
 ctx.feature.bind();await ctx.feature.loadHistory('n1','');ctx.setPending({node:'n1',turnTime:'old',ts:1_000_000});await ctx.feature.render();
 assert.ok(hist.classList.contains('jumpflash'));assert.equal(ctx.getPending(),null);ctx.feature.destroy();
});

test('multiple prompt destinations advertise their count and dialog', () => {
  const one = bubbleMarkersHTML({promptLinks: [{}]}, {iconInto: 'glyph'});
  const two = bubbleMarkersHTML({promptLinks: [{}, {}]}, {iconInto: 'glyph'});
  assert.match(one, /rot90r[^>]*>glyph/);
  assert.doesNotMatch(one, /aria-haspopup|bmarkercount/);
  assert.match(two, /aria-haspopup="dialog"/);
  assert.match(two, /rot90r[^>]*>glyph/);
  assert.match(two, /bmarkercount[^>]*>2</);
});

test('sent-to markers match the complete shared source identity', () => {
 const source={server:'s',owner:'o',chat:'c',generation:'g',log:'comments',seq:1};
 const other={...source,seq:2};const links=[{source,destination:{node:'n1'}},{source:other,destination:{node:'n2'}}];
 assert.deepEqual(sentToMarkerModel(source,'',links),[links[0]]);
 assert.deepEqual(sentToMarkerModel({...source,generation:'other'},'',links),[]);
});

test('shared bookmark markers match the exact source rather than duplicate text', () => {
 const source={server:'s',owner:'o',chat:'c',generation:'g',log:'comments',seq:2};
 const bookmarks=[{t:'one',text:'same',team:{...source,seq:1}},{t:'two',text:'same',team:source}];
 assert.equal(sharedBookmarkMarkerModel(source,bookmarks).bookmark.t,'two');
 assert.equal(sharedBookmarkMarkerModel({...source,generation:'other'},bookmarks),null);
});

test('owner discussion renders the ordinary bookmark and send-to markers', async () => {
 const source={server:'s',owner:'o',chat:'u',generation:'g',log:'comments',seq:1};
 const copy={uid:'u',key:'s/o/u/g',view:{...source},names:{a:'A'},people:{},mirrors:[{seq:1,local_segment:0,local_record:1}],comments:[{seq:1,anchor:1,text:'comment',author:'a'}]};
 const ctx=makeFeature({deps:{ownerDiscussion:()=>[copy],bookmarks:()=>[{t:'one',team:source}],forwardLinks:()=>[{source,destination:{node:'n2'}}]},chatPayload:{turns:[{role:'assistant',uid:'u',record:1,text:'answer'}],log_uid:'u',live:'quiet'}});
 await ctx.feature.render();assert.match(ctx.roots.msgs.innerHTML,/aria-label="Open bookmark"/);assert.match(ctx.roots.msgs.innerHTML,/aria-label="Open sent-to destination"/);ctx.feature.destroy();
});

test('owner discussion bookmark marker opens the exact saved bookmark', async () => {
 const source={server:'s',owner:'o',chat:'u',generation:'g',log:'comments',seq:1},opened=[];
 const copy={uid:'u',key:'s/o/u/g',view:{...source},names:{a:'A'},people:{},mirrors:[{seq:1,local_segment:0,local_record:1}],comments:[{seq:1,anchor:1,text:'comment',author:'a'}]};
 const ctx=makeFeature({deps:{ownerDiscussion:()=>[copy],bookmarks:()=>[{t:'saved',team:source}],openBookmark:t=>opened.push(t)},chatPayload:{turns:[{role:'assistant',uid:'u',record:1,text:'answer'}],log_uid:'u',live:'quiet'}});
 ctx.feature.bind();await ctx.feature.render();const turn=el('div',{className:'turn',dataset:{gkey:'s/o/u/g',cseq:'1'}}),marker=el('button',{dataset:{bmarker:'bookmark'}});turn.appendChild(marker);ctx.roots.msgs.appendChild(turn);firstListener(ctx.roots.msgs,'click')({target:marker});
 assert.deepEqual(opened,['saved']);ctx.feature.destroy();
});

test('received shared markers open only their own bookmark and deliberate local destinations', () => {
 const source={server:'s',owner:'o',chat:'u',generation:'g',log:'comments',seq:2};
 const choices=[],jumps=[],opened=[],toasts=[];
 const links=[{source,destination:{node:'n1'},sent_at:'T'},{source,destination:{node:'missing'}}];
 const ctx=makeFeature({deps:{bookmarks:()=>[{t:'saved',team:source}],forwardLinks:()=>links,openBookmark:t=>opened.push(t),openChoiceList:o=>choices.push(o),jumpToChatAddress:a=>{jumps.push(a);return false},toast:m=>toasts.push(m)}});
 ctx.feature.openSharedMarker({...source,seq:1},'bookmark');assert.deepEqual(opened,[]);
 ctx.feature.openSharedMarker(source,'bookmark');assert.deepEqual(opened,['saved']);
 ctx.feature.openSharedMarker(source,'sendto');assert.equal(choices[0].choices.length,2);
 choices[0].onChoose(choices[0].choices[1]);assert.deepEqual(jumps,[{node:'missing'}]);assert.match(toasts[0],/no longer available/i);
 links.splice(0,links.length,{source});ctx.feature.openSharedMarker(source,'sendto');assert.deepEqual(jumps[1],{});assert.match(toasts[1],/no longer available/i);
 ctx.feature.destroy();
});

test('ordinary bookmark and sent-to markers handle missing destination details honestly', async () => {
 const source={uid:'u',segment:0,record:2},choices=[],opened=[];
 const usages=[{note_id:'n1',section_id:'s1',reference_id:'r1',source},{note_id:'n2',section_id:'s2',reference_id:'r2',source}];
 const links=[{source},{source,destination:{}}];
 const ctx=makeFeature({chatPayload:{turns:[{...source,role:'assistant',text:'answer'}],live:'quiet'},deps:{noteUsages:()=>usages,forwardLinks:()=>links,jumpToChatAddress:null,openNoteUsage:u=>opened.push(u),openChoiceList:o=>choices.push(o)}});
 ctx.feature.bind();await ctx.feature.render();clickBubbleMarker(ctx,'i:0','bookmark');
 assert.deepEqual(choices[0].choices.map(x=>x.label),['Note › Section','Note › Section']);choices[0].onChoose(choices[0].choices[0]);assert.deepEqual(opened,[usages[0]]);
 clickBubbleMarker(ctx,'i:0','sendto');assert.equal(choices[1].choices.length,2);choices[1].onChoose(choices[1].choices[0]);ctx.feature.destroy();
});

test('owner shared bookmark jump highlights the exact discussion sequence', async () => {
 const source={server:'s',owner:'o',chat:'u',generation:'g',log:'comments',seq:2};
 const copy={uid:'u',key:'s/o/u/g',view:{...source},names:{},people:{},mirrors:[{seq:1,local_segment:0,local_record:1}],comments:[{seq:1,anchor:1,text:'same',author:'a'},{seq:2,anchor:1,text:'same',author:'a'}]};
 const ctx=makeFeature({deps:{ownerDiscussion:()=>[copy]},chatPayload:{turns:[{role:'assistant',uid:'u',segment:0,record:1,text:'answer'}],log_uid:'u',live:'quiet'}});
 const target=el('div',{dataset:{gkey:'s/o/u/g',cseq:'2'}});target.scrollIntoView=()=>{target.scrolled=true};
 const query=ctx.roots.msgs.querySelector.bind(ctx.roots.msgs);ctx.roots.msgs.querySelector=s=>s==='[data-gkey="s/o/u/g"][data-cseq="2"]'?target:query(s);
 ctx.setPending({node:'n1',team:source,ts:1_000_000});await ctx.feature.render();assert.equal(target.scrolled,true);assert.equal(ctx.getPending(),null);ctx.feature.destroy();
});

test('owner discussion jump loads prior history without starting duplicate loads', async () => {
 const source={server:'s',owner:'o',chat:'u',generation:'g',log:'comments',seq:2};
 const ctx=makeFeature({chatPayload:{turns:[{role:'assistant',uid:'u',record:1,text:'answer'}],log_uid:'u',prior_turns:1,live:'quiet'}});
 ctx.setPending({node:'n1',team:source,ts:1_000_000});await ctx.feature.render();
 assert.equal(ctx.getPending().histLoading,true);assert.equal(ctx.apiCalls.filter(x=>x.path.includes('history=1')).length,1);
 await ctx.feature.render();assert.equal(ctx.apiCalls.filter(x=>x.path.includes('history=1')).length,1);ctx.feature.destroy();
});

test('owner mirror jump uses the authoritative local record mapping', async () => {
 const source={server:'s',owner:'o',chat:'u',generation:'g',log:'mirror',seq:7};
 const copy={uid:'u',key:'s/o/u/g',view:{...source},names:{},people:{},mirrors:[{seq:7,t:'assistant',local_segment:0,local_record:2}],comments:[]};
 const ctx=makeFeature({deps:{ownerDiscussion:()=>[copy]},chatPayload:{turns:[{role:'assistant',uid:'u',segment:0,record:1,text:'same'},{role:'assistant',uid:'u',segment:0,record:2,text:'same'}],log_uid:'u',live:'quiet'}});
 const target=el('div');target.scrollIntoView=()=>{target.scrolled=true};const query=ctx.roots.msgs.querySelector.bind(ctx.roots.msgs);ctx.roots.msgs.querySelector=s=>s==='.turn[data-i="1"]'?target:query(s);
 ctx.setPending({node:'n1',team:source,ts:1_000_000});await ctx.feature.render();assert.equal(target.scrolled,true);assert.equal(ctx.getPending(),null);ctx.feature.destroy();
});

test('owner mirror jump never invents a local history address for a missing mapping', async () => {
 const source={server:'s',owner:'o',chat:'u',generation:'g',log:'mirror',seq:7};
 for (const mirror of [null,{seq:7,t:'assistant',local_segment:-1,local_record:2},{seq:7,t:'assistant',local_segment:0,local_record:-1}]) {
  const warnings=[];
  const copy={uid:'u',key:'s/o/u/g',view:{...source},names:{},people:{},mirrors:mirror?[mirror]:[],comments:[]};
  const ctx=makeFeature({deps:{ownerDiscussion:()=>[copy],toast:message=>warnings.push(message)},chatPayload:{turns:[{role:'assistant',uid:'u',record:1,text:'same'}],log_uid:'u',live:'quiet'}});
  ctx.setPending({node:'n1',team:source,ts:1_000_000});await ctx.feature.render();
  if (mirror) {assert.match(warnings[0],/unavailable/i);assert.equal(ctx.getPending(),null);}
  else {assert.deepEqual(warnings,[]);assert.deepEqual(ctx.getPending().team,source);}
  ctx.feature.destroy();
 }
});

test('an owner shared jump from an earlier visit cannot scroll a later visit', async () => {
 const source={server:'s',owner:'o',chat:'u',generation:'g',log:'comments',seq:2};
 const ctx=makeFeature({chatPayload:{turns:[{role:'assistant',text:'answer'}],live:'quiet'}}),target=el('div');target.scrollIntoView=()=>{target.scrolled=true};
 const query=ctx.roots.msgs.querySelector.bind(ctx.roots.msgs);ctx.roots.msgs.querySelector=s=>s==='[data-gkey="s/o/u/g"][data-cseq="2"]'?target:query(s);
 ctx.setPending({node:'n1',team:source,visit:1,ts:1_000_000});ctx.setSelGen(3);await ctx.feature.render();assert.notEqual(target.scrolled,true);assert.equal(ctx.getPending(),null);ctx.feature.destroy();
});

test('shared owner timeline retains both the prompt and discussion with human labels', async () => {
 const copy={uid:'u',key:'s/o/u/g',view:{server:'s'},names:{},people:{},mirrors:[{seq:1,local_segment:0,local_record:1}],comments:[{seq:1,anchor:1,text:'human comment',author:'o',mine:true}]};
 const ctx=makeFeature({deps:{ownerDiscussion:()=>[copy]},chatPayload:{turns:[{role:'user',uid:'u',segment:0,record:1,text:'agent prompt'}],log_uid:'u',live:'quiet'}});
 ctx.feature.bind();await ctx.feature.loadHistory('n1','');
 assert.match(ctx.roots.msgs.innerHTML,/data-bk="i:0"[^]*?>You<\/div><p>agent prompt<\/p>/);assert.match(ctx.roots.msgs.innerHTML,/data-cseq="1"[^]*?>You<\/div><p>human comment<\/p>/);ctx.feature.destroy();
});

test('another shared log does not label an unrelated owner prompt', async () => {
 const copy={uid:'different',key:'s/o/d/g',view:{server:'s'},names:{},people:{},mirrors:[],comments:[]};
 const ctx=makeFeature({deps:{ownerDiscussion:()=>[copy]},chatPayload:{turns:[{role:'user',uid:'u',segment:0,record:1,text:'ordinary prompt'}],log_uid:'u',live:'quiet'}});
 ctx.feature.bind();await ctx.feature.loadHistory('n1','');assert.doesNotMatch(ctx.roots.msgs.innerHTML,/You · Agent/);ctx.feature.destroy();
});

test('ordinary unshared prompt has no shared destination caption', async () => {
 const ctx=makeFeature({deps:{ownerDiscussion:()=>null},chatPayload:{turns:[{role:'user',uid:'u',segment:0,record:1,text:'ordinary prompt'}],log_uid:'u',live:'quiet'}});
 ctx.feature.bind();await ctx.feature.loadHistory('n1','');assert.match(ctx.roots.msgs.innerHTML,/ordinary prompt/);assert.doesNotMatch(ctx.roots.msgs.innerHTML,/You · Agent/);ctx.feature.destroy();
});

test('same-source same-chat confirmed uses offer distinct edited prompt choices and exact addresses', async () => {
 const source={uid:'u',segment:0,record:2}, jumps=[],choices=[];
 const links=[{kind:'prompt',source,preview:'Edited first prompt',destination:{node:'n1',uid:'u',segment:0,record:5}},{kind:'prompt',source,preview:'Edited second prompt',destination:{node:'n1',uid:'u',segment:0,record:9}}];
 const ctx=makeFeature({chatPayload:{turns:[{...source,role:'assistant',text:'source'}],live:'quiet'},deps:{forwardLinks:()=>links,jumpToChatAddress:a=>jumps.push(a),openChoiceList:o=>choices.push(o)}});
 ctx.feature.bind();await ctx.feature.render();clickBubbleMarker(ctx,'i:0','prompt');
 assert.match(choices[0].choices[0].label,/Edited first prompt/);assert.match(choices[0].choices[1].label,/Edited second prompt/);
 choices[0].onChoose(choices[0].choices[0]);choices[0].onChoose(choices[0].choices[1]);assert.deepEqual(jumps,links.map(x=>x.destination));ctx.feature.destroy();
});

test('old prompt links get distinct honest labels and never fall back to text for navigation', async () => {
 const source={uid:'u',record:2},jumps=[],choices=[],toasts=[];
 const links=[{kind:'prompt',source,destination:{node:'n1',uid:'u',record:5}},{kind:'prompt',source,destination:{node:'n1',text:'do not match'}}];
 const ctx=makeFeature({chatPayload:{turns:[{...source,role:'assistant',text:'source'}],live:'quiet'},deps:{forwardLinks:()=>links,jumpToChatAddress:a=>jumps.push(a),toast:m=>toasts.push(m),openChoiceList:o=>choices.push(o)}});
 ctx.feature.bind();await ctx.feature.render();clickBubbleMarker(ctx,'i:0','prompt');
 assert.notEqual(choices[0].choices[0].label,choices[0].choices[1].label);assert.match(choices[0].choices[0].label,/^1\./);assert.match(choices[0].choices[1].label,/^2\./);
 choices[0].onChoose(choices[0].choices[0]);choices[0].onChoose(choices[0].choices[1]);assert.deepEqual(jumps,[{node:'n1',uid:'u',segment:0,record:5}]);assert.equal(toasts.length,1);ctx.feature.destroy();
});

test('early accepted owner comment in an empty previous segment stays visible', async () => {
 const copy={uid:'u',key:'server/owner/u/7',view:{server:'server',owner:'owner',chat:'u',generation:7},names:{p:'Person'},people:{},mirrors:[{seq:0,t:'start',local_segment:0,local_record:-1}],comments:[{seq:1,anchor:0,text:'early human reply',author:'p',at:'T'}]};
 const ctx=makeFeature({deps:{ownerDiscussion:()=>[copy]},api:async path=>path.includes('/chat')?{turns:[],log_uid:'u',segment_index:1,prior_turns:0,chat_started:'2026-01-01T00:00:00Z',live:'quiet',source:'claude'}:{}});
 ctx.feature.bind(); await ctx.feature.render(); const body=ctx.roots.msgs.innerHTML;
 assert.match(body,/early human reply/); assert.match(body,/Person/); assert.match(body,/server\/owner\/u\/7/); assert.equal((body.match(/early human reply/g)||[]).length,1); ctx.feature.destroy();
});

test('early discussion projection respects local identity and real history', async () => {
 const comment={seq:1,anchor:0,text:'older reply',author:'p',at:'T'};
 const makeCopy=uid=>({uid,key:'server/owner/'+uid+'/7',view:{server:'server'},names:{p:'Person'},people:{},mirrors:[{seq:0,t:'start',local_segment:0,local_record:-1}],comments:[comment]});
 for (const [uid,prior,visible] of [['other',0,false],['u',1,false],['u',0,true]]) {
  const ctx=makeFeature({deps:{ownerDiscussion:()=>[makeCopy(uid)]},api:async path=>path.includes('/chat')?{turns:[],log_uid:'u',segment_index:1,prior_turns:prior,live:'quiet',source:'claude'}:{}});
  ctx.feature.bind();await ctx.feature.render();assert.equal(ctx.roots.msgs.innerHTML.includes('older reply'),visible);if(prior)assert.match(ctx.roots.msgs.innerHTML,/histload/);ctx.feature.destroy();
 }
});

test('empty earlier history without discussion renders normally', async () => {
 const ctx=makeFeature({deps:{ownerDiscussion:()=>null},api:async path=>path.includes('/chat')?{turns:[],log_uid:'u',segment_index:1,prior_turns:0,live:'quiet',source:'claude'}:{}});
 ctx.feature.bind();await ctx.feature.render();assert.doesNotMatch(ctx.roots.msgs.innerHTML,/Earlier discussion/);ctx.feature.destroy();
});

test('a poll captures the focused editor scroll and selection after its response arrives', async () => {
 const response=deferred();let calls=0,selection;
 const doc={activeElement:null,createElement:tag=>el(tag),querySelector:()=>null};
 const ctx=makeFeature({deps:{document:doc},api:async path=>path.includes('/chat')?(++calls===1?{turns:[],log_uid:'u',live:'quiet'}:response.promise):{}});
 const body=ctx.roots.msgs;body.scrollHeight=10000;body.clientHeight=700;
 ctx.feature.bind();await ctx.feature.render();
 let absoluteTop=5000;
 const input={value:'latest edit',selectionStart:1,selectionEnd:2,selectionDirection:'forward',closest:()=>row,focus(){doc.activeElement=this},setSelectionRange(...v){selection=v}};
 const turn={dataset:{gkey:'s/o/u/g',cseq:'1'},getBoundingClientRect:()=>({top:absoluteTop-body.scrollTop}),after(){}};
 const row={previousElementSibling:turn,contains:x=>x===input};doc.activeElement=input;
 const fresh={dataset:{...turn.dataset},replaceWith(){absoluteTop+=100}};
 const query=body.querySelector.bind(body),all=body.querySelectorAll.bind(body);
 body.querySelector=s=>s==='[data-grename]'?input:query(s);body.querySelectorAll=s=>s==='[data-gkey][data-cseq]'?[fresh]:all(s);
 const pending=ctx.feature.render();body.scrollTop=4500;input.selectionStart=3;input.selectionEnd=6;
 response.resolve({turns:[{role:'assistant',text:'new data',uid:'u',segment:0,record:0}],log_uid:'u',live:'quiet'});await pending;
 assert.equal(body.scrollTop,4600);assert.deepEqual(selection,[3,6,'forward']);assert.equal(input.value,'latest edit');ctx.feature.destroy();
});

test("coordinator: a previous description save cannot end newer editing in another chat", async () => {
  let finish;
  const oldResponse = new Promise(resolve => { finish = resolve; });
  const ctx = makeFeature({nodes:[{id:"n1",title:"Original",agent:"codex",description:"old"},{id:"n2",title:"Other",agent:"codex",description:"other"}],
    api: async (path, options) => options?.method === "PATCH" ? oldResponse : {},
    deps:{setTimeout:()=>1,clearTimeout:()=>{}}});
  ctx.feature.bind();
  ctx.feature.useTurnAsDescription("Synthetic first description");
  firstListener(ctx.roots.infobtn,"click")();
  await new Promise(resolve=>setImmediate(resolve));
  assert.equal(ctx.apiCalls.filter(c=>c.opts?.method==="PATCH").length,1);
  ctx.feature.onSelectChange();ctx.setSel("n2");ctx.setSelGen(2);
  ctx.feature.useTurnAsDescription("Synthetic newer description");
  assert.equal(ctx.feature.isEditingDesc(),true);
  finish({id:"n1",title:"Original",agent:"codex",description:"Synthetic first description"});
  await new Promise(resolve=>setImmediate(resolve));
  console.log(JSON.stringify({coordinatorLateDescription:{editing:ctx.feature.isEditingDesc(),value:ctx.roots.chatdescinput.value}}));
  assert.equal(ctx.feature.isEditingDesc(),true,"an old save response must not close the new chat's active editor");
  assert.equal(ctx.roots.chatdescinput.value,"Synthetic newer description");
  ctx.feature.destroy();
});

test('description editor refuses a chat without durable identity when required', () => {
 const alerts=[];const ctx=makeFeature({nodes:[{id:'n1',title:'Legacy'}],deps:{requireChatIdentity:true,alert:value=>alerts.push(value)}});
 ctx.feature.useTurnAsDescription('synthetic');assert.equal(ctx.feature.isEditingDesc(),false);assert.match(alerts[0],/identity/i);ctx.feature.destroy();
});

test('description save preserves newer typing in the same editor', async () => {
 let finish; const pending=new Promise(resolve=>{finish=resolve}); const updated=[];
 const ctx=makeFeature({nodes:[{id:'n1',instance_id:'one',title:'One'}],api:async (_path,opts)=>opts?.method==='PATCH'?pending:{},deps:{setTimeout:()=>1,clearTimeout:()=>{},updateLocalNode:n=>updated.push(n)}});
 ctx.feature.bind();ctx.feature.useTurnAsDescription('first');firstListener(ctx.roots.infobtn,'click')();
 ctx.roots.chatdescinput.value='newer typing';finish({id:'n1',instance_id:'one',description:'first'});await new Promise(resolve=>setImmediate(resolve));
 assert.equal(ctx.feature.isEditingDesc(),true);assert.equal(ctx.roots.chatdescinput.value,'newer typing');assert.equal(updated.length,1);ctx.feature.destroy();
});

test('description editor cannot save into a recreated chat with the same id', async () => {
 const ctx=makeFeature({nodes:[{id:'n1',instance_id:'old',title:'Original'}],api:async()=>({}),deps:{setTimeout:()=>1,clearTimeout:()=>{}}});
 ctx.feature.bind();ctx.feature.useTurnAsDescription('old description');ctx.nodes.splice(0,1,{id:'n1',instance_id:'new',title:'Replacement'});
 firstListener(ctx.roots.infobtn,'click')();await new Promise(resolve=>setImmediate(resolve));
 assert.equal(ctx.apiCalls.filter(call=>call.opts?.method==='PATCH').length,0);ctx.feature.destroy();
});

test('out-of-order description saves do not roll back a newer save or rename', async () => {
 const resolvers=[];let ctx;
 ctx=makeFeature({nodes:[{id:'n1',instance_id:'one',title:'Old title',description:'initial'}],api:async (_path,opts)=>opts?.method==='PATCH'?new Promise(resolve=>resolvers.push(resolve)):{},deps:{setTimeout:()=>1,clearTimeout:()=>{},updateLocalNode:n=>Object.assign(ctx.nodes[0],n)}});
 ctx.feature.bind();ctx.feature.useTurnAsDescription('first');firstListener(ctx.roots.infobtn,'click')();
 ctx.feature.useTurnAsDescription('second');firstListener(ctx.roots.infobtn,'click')();ctx.nodes[0].title='Renamed';
 resolvers[1]({id:'n1',instance_id:'one',title:'Old title',description:'second'});await new Promise(resolve=>setImmediate(resolve));
 resolvers[0]({id:'n1',instance_id:'one',title:'Old title',description:'first'});await new Promise(resolve=>setImmediate(resolve));
 assert.equal(ctx.nodes[0].description,'second');assert.equal(ctx.nodes[0].title,'Renamed');ctx.feature.destroy();
});

test('earlier duplicate description save cannot close the editor of a pending newer save', async () => {
 const resolvers=[];const ctx=makeFeature({nodes:[{id:'n1',instance_id:'one',title:'One'}],api:async (_path,opts)=>opts?.method==='PATCH'?new Promise(resolve=>resolvers.push(resolve)):{},deps:{setTimeout:()=>1,clearTimeout:()=>{}}});
 ctx.feature.bind();ctx.feature.useTurnAsDescription('same');firstListener(ctx.roots.infobtn,'click')();firstListener(ctx.roots.infobtn,'click')();
 resolvers[0]({id:'n1',instance_id:'one',description:'same'});await new Promise(resolve=>setImmediate(resolve));assert.equal(ctx.feature.isEditingDesc(),true);
 resolvers[1]({id:'n1',instance_id:'one',description:'same'});await new Promise(resolve=>setImmediate(resolve));assert.equal(ctx.feature.isEditingDesc(),false);ctx.feature.destroy();
});

test('retired panel ignores pending description completion and focus callbacks', async () => {
 let finish;const timers=[],updates=[];const ctx=makeFeature({nodes:[{id:'n1',instance_id:'one',title:'One'}],api:async (_path,opts)=>opts?.method==='PATCH'?new Promise(resolve=>finish=resolve):{},deps:{setTimeout:fn=>{timers.push(fn);return timers.length},clearTimeout:()=>{},updateLocalNode:n=>updates.push(n)}});
 ctx.feature.bind();ctx.feature.useTurnAsDescription('draft');firstListener(ctx.roots.infobtn,'click')();ctx.feature.destroy();timers.forEach(fn=>fn());
 finish({id:'n1',instance_id:'one',description:'draft'});await new Promise(resolve=>setImmediate(resolve));assert.deepEqual(updates,[]);
});

test('description save failures and saved timers belong to their editing visit', async () => {
 const timers=[],alerts=[];let now=1000,fail=false;
 const ctx=makeFeature({nodes:[{id:'n1',instance_id:'one',title:'One'},{id:'n2',instance_id:'two',title:'Two'}],api:async (_path,opts)=>{if(opts?.method==='PATCH'){if(fail)throw Error('synthetic failure');return {id:'n1',instance_id:'one',description:''}}return {}},deps:{setTimeout:(fn,ms)=>{timers.push({fn,ms});return timers.length},clearTimeout:()=>{},now:()=>now,alert:msg=>alerts.push(msg)}});
 ctx.feature.bind();ctx.feature.useTurnAsDescription('');firstListener(ctx.roots.infobtn,'click')();await new Promise(resolve=>setImmediate(resolve));
 const saved=timers.find(x=>x.ms>0);assert.ok(saved);now+=10000;saved.fn();assert.equal(ctx.feature.isEditingDesc(),false);
 ctx.feature.useTurnAsDescription('next');fail=true;firstListener(ctx.roots.infobtn,'click')();await new Promise(resolve=>setImmediate(resolve));assert.match(alerts.at(-1),/synthetic failure/);assert.equal(ctx.feature.isEditingDesc(),true);
 ctx.feature.onSelectChange();ctx.setSel('n2');ctx.setSelGen(2);ctx.feature.useTurnAsDescription('new visit');saved.fn();assert.equal(ctx.feature.isEditingDesc(),true);assert.equal(ctx.roots.chatdescinput.value,'new visit');ctx.feature.destroy();
});

test('description long press and save reject lost identity', async () => {
 let press;const alerts=[];const ctx=makeFeature({nodes:[{id:'n1',instance_id:'one',title:'One'}],deps:{requireChatIdentity:true,alert:msg=>alerts.push(msg),longpress:(_root,selector,fn)=>{if(selector==='#chatdesc')press=fn;return ()=>{}},setTimeout:()=>1,clearTimeout:()=>{}}});
 ctx.feature.bind();press();ctx.nodes[0].instance_id='';firstListener(ctx.roots.infobtn,'click')();await new Promise(resolve=>setImmediate(resolve));assert.match(alerts.at(-1),/identity/i);assert.equal(ctx.apiCalls.length,0);
 ctx.feature.cancelDescEdit();press();assert.equal(ctx.feature.isEditingDesc(),false);assert.match(alerts.at(-1),/identity/i);ctx.feature.destroy();
});

test('description saved indicator supports the default clock', async () => {
 const timers=[];const ctx=makeFeature({nodes:[{id:'n1',instance_id:'one',title:'One'}],api:async (_path,opts)=>opts?.method==='PATCH'?{id:'n1',instance_id:'one',description:'saved'}:{},deps:{now:null,setTimeout:(fn,ms)=>{timers.push({fn,ms});return timers.length},clearTimeout:()=>{}}});
 ctx.feature.bind();ctx.feature.useTurnAsDescription('saved');firstListener(ctx.roots.infobtn,'click')();await new Promise(resolve=>setImmediate(resolve));
 const saved=timers.find(x=>x.ms>0);assert.ok(saved);saved.fn();ctx.feature.destroy();
});

test('destroy continues cleanup when a long-press disposer throws', () => {
 let called=0;const ctx=makeFeature({deps:{longpress:()=>()=>{called++;if(called===1)throw Error('synthetic cleanup failure')}}});
 ctx.feature.bind();assert.doesNotThrow(()=>ctx.feature.destroy());assert.equal(called,2);
});

test("ordinary chat render shows no recipient sharing notice", async () => {
  const ctx = makeFeature();
  ctx.feature.bind();
  await ctx.feature.render();
  assert.doesNotMatch(ctx.roots.msgs.innerHTML, /sharing-notice|sharing-disclosure/);
  assert.equal(ctx.roots.chathead.querySelector(".sharing-disclosure"), null);
  assert.doesNotMatch(ctx.roots.chatmission.textContent || "", /Shared by|Created with/);
  ctx.feature.destroy();
});

test('common chat file retry is scoped to the active visit and never dispatches an agent',async()=>{
 let release,clicked=0,replaced=0;const document={createElement:()=>({click(){clicked++}}),querySelector:()=>null};
 const ctx=makeFeature({sel:'team/s/o/c/g',deps:{document,retryAsset:()=>new Promise(resolve=>{release=resolve})}});ctx.feature.bind();
 const button={dataset:{attachmentRetry:'/api/team/subscriptions/s/o/c/g/attachments/1/id',name:'synthetic.txt'},isConnected:true,replaceWith(){replaced++}};
 firstListener(ctx.roots.msgs,'click')({target:{closest:selector=>selector==='[data-attachment-retry]'?button:null}});assert.equal(button.disabled,true);
 ctx.setSel('n1');ctx.setSelGen(2);ctx.setSel('team/s/o/c/g');ctx.setSelGen(3);release('blob:verified');await new Promise(resolve=>setImmediate(resolve));
 assert.equal(replaced,0);assert.equal(clicked,0);assert.equal(ctx.apiCalls.length,0);ctx.feature.destroy();
});
