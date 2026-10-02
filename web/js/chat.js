/* Chat feature: head/details, turns/bubbles, history, terminal, scroll, echo.
 *
 * Packet 7C ownership inventory
 * -----------------------------
 * Owned roots / controls:
 *   - #chathead (details open, title display, mission, agent meta, desc)
 *   - #chatdetails / #chatdesc / #chatdescinput / #infobtn / #chatdot /
 *     #chattitle / #chatmission / #chatagentlogo / #chatmeta
 *   - #msgwrap / #msgs / #chatloading
 *   - #gauge / #gaugefill (context gauge)
 *   - #keyrow (dialog answer buttons — presentation + key send)
 *   - #convtools / #termtoggle / #autoapprove / #scrollend
 *   - #workpulse
 *
 * Inputs (getters / injected deps — never implicit app globals):
 *   - nodes, selected id (sel), selGen
 *   - editingTitle / editingTitleScope (shared shell title editor)
 *   - hardAttention, laneColor, agentLogo, icons
 *   - pendingJump get/set (shell jump coordinator for notes/search)
 *   - bookmarks / noteUsages / forwardLinks / markerVersion / uiMutate
 *   - openBookmark / openChoiceList / openNoteUsage / jumpToChatAddress
 *   - composer effects: setComposerBusy / setComposerClosed / setAttachAvail
 *   - cards/map invalidation + re-render when live/attention chrome changes
 *   - api, hashStr, md, esc, fmtWhen, fmtBubbleTime, bubbleTitle
 *   - document/window timers, CSS.escape, longpress helper
 *
 * Outputs / owned state:
 *   - HTML into msgs / keyrow / chat head details / gauge / term toggle
 *   - chatSig, chatHist, termOpen, termFull, sentEcho, suppressAttentionUntil
 *   - tappedTurn, bubbleTurns, lastTurns, chatScrollBottom
 *   - chatDetailsOpen, editingChatDesc, chatDetailSavedUntil, chatCtxPct
 *   - tileBox (thumbnail size reservation across poll rebuilds)
 *   - workpulse .on class; chathead .compact / .details-open / .desc-editing
 *
 * Server calls owned here:
 *   - GET  /api/nodes/:id/chat
 *   - GET  /api/nodes/:id/chat?history=1
 *   - GET  /api/nodes/:id/peek[?mode=visible]
 *   - POST /api/nodes/:id/key
 *   - POST /api/nodes/:id/send/resolve
 *   - POST /api/nodes/:id/auto-approve
 *   - PATCH /api/nodes/:id  (description only, from details editor)
 *
 * Events owned after one idempotent bind():
 *   - #infobtn click (details toggle / save description)
 *   - #chathead click (header-row details toggle)
 *   - #msgs scroll (compact header)
 *   - #msgs load capture (thumbnail box + re-pin near bottom)
 *   - #msgs error capture (broken-thumbnail cleanup; no inline onerror)
 *   - #msgs click (histload, marker jumps/menus, bubble tap/actions)
 *   - #msgs touchend (double-tap zoom reset on empty background)
 *   - #keyrow click (dialog keys + attention collapse)
 *   - #termtoggle / #autoapprove / #scrollend click
 *   - longpress #chattitle / #chatdesc (title edit / desc edit)
 *   - dynamic #termfull / #decresolve (rebound each rebuild and explicitly
 *     detached before replacement or destroy)
 *
 * Not owned (stay shell / other features):
 *   - composer singleton (#prompt / #promptbar / #attstage / send / drafts)
 *   - #chatback (spatial setLevel) and #bookmarksbtn (bookmarks toggle)
 *   - document keydown/focusout title-edit handlers (shared with cards)
 *   - document Escape for map-full (app-level)
 *   - select() orchestration, polling tick, shell spatial gestures/scrim
 *   - forkFromTurn / openSheet / new-activity configuration (sheets)
 *   - openSendTo: the send-to picker and its target list (bookmarks owns
 *     #sendto; the bubble action only supplies text + the source node id)
 *   - jumpToChatAddress / pendingJump creation (bookmarks/search set them;
 *     chat invokes the injected jump for Send-to markers)
 *   - stampAddress pure helper (shell; bubble action injects it)
 *   - archived read-only surface (imports splitAssetRefs only)
 *   - shared startTitleEdit / commitTitleEdit
 *
 * Reuses (no algorithm duplication):
 *   format.js: esc, md, fmtWhen, fmtBubbleTime, bubbleTitle
 *   lanes.js: hashStr
 *   map-model.js: hardAttention
 *
 * Module size: above the ~500-line soft guide — one factory owns the complete
 * Chat surface (head + turns + history + terminal + echo + scroll + keys).
 */

import {
  esc, md, fmtWhen, fmtBubbleTime, bubbleTitle,
  ASSET_REF_RE, stripAssetRefs,
} from "./format.js";
import { hashStr, hashTurns } from "./lanes.js";
import { hardAttention as hardAttentionMod, canReceiveSend } from "./map-model.js";
import { focusAtEnd } from "./caret.js";
import {
  readPendingForwards,
  clearPendingForwards,
  readAwaitingForward,
  clearAwaitingForward,
  makeForwardLinkToTurn,
} from "./storage.js";

/* ---------- public constants ---------- */

export const ATTENTION_SUPPRESS_MS = 2500;
export const SENT_ECHO_TIMEOUT_MS = 180000;
export const SCROLL_NEAR_BOTTOM_PX = 80;
export const CHAT_LOAD_DELAY_MS = 300;
export const CHAT_DETAIL_SAVED_MS = 2000;
export const PENDING_JUMP_TTL_MS = 15000;
export const MUSE_SCHEMA_MISMATCH_WARNING =
  "Muse protocol compatibility warning: its schema fingerprint differs from the version scimux tested.";
export const MUSE_SCHEMA_MISMATCH_CODE = "muse_schema_fingerprint_mismatch";

/* Exact client twin of the server's inlineImageTypes allowlist: only these
   raster extensions preview as <img>. Recorded MIME is never consulted. */
export const RASTER_RE = /\.(png|jpe?g|gif|webp)$/i;

export const DEC_LABELS = {
  no_transcript: "No transcript yet \u2014 showing the terminal",
  no_structured_request: "Showing the terminal",
  pane_active: "Agent is working",
  waiting_question: "Agent is waiting for your answer",
  waiting_approval: "Agent needs your approval",
  quiet_inspect: "Quiet \u2014 inspect the terminal",
  /* ACP transport (pi/opencode/grok/cursor/dsh): no pane, so "terminal" reads as the event log */
  turn_active: "Agent is working",
  turn_error: "The agent finished without output \u2014 check the log",
  claude_starting: "Starting Claude \u2014 waiting for SessionStart",
  claude_unsupported: "Some chat features aren’t available.",
  claude_launch_error: "Claude did not start",
  claude_transcript_fault: "The transcript is not readable \u2014 send another prompt or fork",
};

const KEYS = { Up: "\u2191", Down: "\u2193", Enter: "\u23ce", Escape: "Esc" };
const HINTS = {
  /* Neutral diagnostic only — no remote keys (fixes-2 P1). */
  inspect: "No visible progress \u2014 inspect if needed.",
};

/* ---------- pure decisions ---------- */

export function chatRenderDecision(sig, prevSig){
  return sig === prevSig ? "skip" : "rebuild";
}

export function attentionIsSuppressed(id, untilMap, now = Date.now()){
  if (!id) return false;
  return (untilMap[id] || 0) > now;
}

export function suppressAttentionUntilTime(now = Date.now(), ms = ATTENTION_SUPPRESS_MS){
  return now + ms;
}

/* Stable identity of one attention claim. attention_at changes only when the
   server sees fresh evidence, not on each poll or spinner frame. */
export function attentionEvidenceKey(node = {}){
  return `${node.attention || ""}:${Number(node.attention_at) || 0}`;
}

export function normalizeEchoText(s){
  return (s || "").replace(/\s+/g, " ").trim();
}

/* Retire or update the optimistic sent echo against the latest turns.
   Returns { echo: nextEchoOrNull, retired: boolean }. Mutates nothing. */
export function retireSentEcho(sentEcho, turns, now = Date.now()){
  if (!sentEcho) return { echo: null, retired: false };
  const list = turns || [];
  const norm = normalizeEchoText;
  const iLast = list.reduce((a, t, i) => t.role === "user" ? i : a, -1);
  let landed = iLast >= 0 &&
    (norm(list[iLast].text) === norm(sentEcho.text) ||
     (sentEcho.seen != null && iLast >= sentEcho.seen));
  let next = sentEcho;
  if (!landed && sentEcho.seen == null){
    const users = list.reduce((a, t) => a + (t.role === "user" ? 1 : 0), 0);
    if (sentEcho.base == null) next = { ...sentEcho, base: users };
    else if (users > sentEcho.base) landed = true;
  }
  if (landed || now - sentEcho.at > SENT_ECHO_TIMEOUT_MS)
    return { echo: null, retired: true };
  return { echo: next, retired: false };
}

/* Decide pane/terminal chrome from poll payload + local termOpen flag. */
export function chatActivityPolicy({
  attention = "",
  attentionHidden = false,
  fallback = false,
  delivery = "",
  turnsLength = 0,
  source = "",
  priorTurns = 0,
  termOpen = false,
  fresh = false,
  supervision = "",
  autoApproveArmed = false,
  permDialogId = "",
} = {}){
  const unconfirmed = delivery === "unconfirmed";
  /* "delivering" is the first prompt pasted into a launch whose SessionStart
     arrived but whose lazily written transcript has not caught up. It is
     deliberately not `unconfirmed`: nothing has failed, so it must not force
     the pane, offer a resume button, or say to check the terminal. */
  const delivering = delivery === "delivering";
  const claudeGated = supervision === "claude_strict" ||
    supervision === "claude_unsupported" ||
    supervision === "claude_starting" ||
    supervision === "claude_failed";
  if (claudeGated){
    // Hooked/starting/unsupported Claude: never auto-open from inspect,
    // fallback, unconfirmed delivery, empty turns, owing, or elicitation.
    // The pane is visible only when the user opened it or a proven
    // permission-dialog epoch is waiting and auto-approve is not armed.
    const permDialog = supervision === "claude_strict" && !autoApproveArmed &&
      !attentionHidden && !!permDialogId;
    const mustShowPane = permDialog;
    const freshSurface = !turnsLength && !mustShowPane &&
                         (fresh || (source === "acp" && (priorTurns || 0) > 0));
    const forcePeek = mustShowPane;
    const showPeek = forcePeek || !!termOpen;
    return { unconfirmed, delivering, mustShowPane, freshSurface, forcePeek, showPeek };
  }
  // Server-reported fresh (zero-turn post-seam segment) means a deliberate
  // /clear: show "fresh chat — send a prompt" instead of treating empty+
  // fallback as a broken transcript that forces a terminal peek (P2c).
  // Attention and unconfirmed delivery force the pane until the user locally
  // acknowledges this exact evidence epoch.
  const mustShowPane = !attentionHidden && (
    !!attention || unconfirmed || (!!fallback && !fresh)
  );
  const freshSurface = !turnsLength && !mustShowPane &&
                       (fresh || (source === "acp" && (priorTurns || 0) > 0));
  const forcePeek = mustShowPane || (!turnsLength && !freshSurface);
  const showPeek = forcePeek || !!termOpen;
  return { unconfirmed, delivering, mustShowPane, freshSurface, forcePeek, showPeek };
}

export function priorSegsFromHistory(histSegs, chatStarted){
  const curT = Date.parse(chatStarted || "") || Infinity;
  if (!histSegs) return [];
  return histSegs.filter(s => (Date.parse(s.start || s.seam || "") || 0) < curT);
}

/* The transcript's signature. Deliberately blind to two things that move on
   the poll cadence: the terminal pane capture (which has its own host and its
   own signature below) and mechanical liveness, which never reaches this
   markup at all — it drives applyChatActivityChrome, outside the region. */
export function buildChatSignature(parts){
  const p = parts || {};
  return [
    p.nodeId,
    p.attention,
    p.attentionHidden,
    p.delivery,
    p.showPeek,
    p.termOpen,
    p.termFull,
    p.source,
    p.externalAttachments ? "1" : "0",
    /* Transport failures can arrive without a new chat turn. Include the
       inline message so the status is inserted (and announced) immediately. */
    p.error || "",
    p.permTitle,
    p.permReason || "",
    p.permOptionsKey,
    /* Request identity must participate: A→B with identical title/options
       still rebuilds the row so buttons carry B's data-request-id (P1 review). */
    p.permRequestId || "",
    p.permDialogId || "",
    p.permManual ? "1" : "0",
    p.priorTurns || 0,
    p.chatStarted || "",
    p.echoHash || "",
    p.histKey || "",
    p.turnsHash || "",
    p.turnAttrHash || "",
    p.markerHash || "",
    p.decisionsHash || "",
    p.expanded ? "1" : "0",
    p.compacting ? "1" : "0",
    p.elicitationKey || "",
  ].join("|");
}

function durableSourceKey(value = {}){
  return value.uid
    ? `u\u0000${value.uid}\u0000${Number(value.segment) || 0}\u0000${Number(value.record) || 0}`
    : "";
}

function legacySourceMatches(turn, nodeId, value = {}){
  return !durableSourceKey(value) && value.node === nodeId &&
    value.turnTime === (turn.time || "") &&
    (!Object.prototype.hasOwnProperty.call(value, "text") || value.text === (turn.text || ""));
}

export function bookmarkMarkerModel(turn, nodeId, bookmarks = [], usages = []){
  const key = durableSourceKey(turn);
  const bookmark = bookmarks.find(b =>
    (key && durableSourceKey(b) === key) || legacySourceMatches(turn, nodeId, b));
  if (!bookmark) return null;
  const bookmarkKey = durableSourceKey(bookmark);
  const seen = new Set();
  const destinations = usages.filter(u => {
    const source = (u && u.source) || {};
    return (bookmarkKey && durableSourceKey(source) === bookmarkKey) ||
      (!bookmarkKey && source.node === bookmark.node && source.turnTime === bookmark.turnTime);
  }).filter(u => {
    const k = `${u.note_id || ""}\u0000${u.section_id || ""}`;
    if (seen.has(k)) return false;
    seen.add(k);
    return true;
  });
  return { bookmark, destinations };
}

export function sentToMarkerModel(turn, nodeId, links = []){
  const key = durableSourceKey(turn);
  return (links || []).filter(link => {
    const source = (link && link.source) || {};
    return (key && durableSourceKey(source) === key) || legacySourceMatches(turn, nodeId, source);
  });
}

export function matchingForwardDestinationTurn(expected, turns = []){
  const latch = typeof expected === "string"
    ? { text: expected, afterTurns: 0 }
    : expected;
  if (!latch || !latch.text) return null;
  const boundary = Math.max(0, Math.floor(Number(latch.afterTurns) || 0));
  const start = boundary <= turns.length ? boundary : 0;
  /* Attachments make the recorded turn the prompt plus extendPrompt's
     appended reference, so the seam is matched, not a loose prefix: an
     unrelated turn that merely starts with this text is a different send. */
  const extended = `${latch.text}\n\n`;
  return turns.slice(start).find(turn =>
    turn && turn.role === "user" &&
    (turn.text === latch.text || String(turn.text || "").startsWith(extended))) || null;
}

export function bubbleMarkersHTML(model = {}, { iconBookmark = "", iconInto = "" } = {}){
  const buttons = [];
  if (model.bookmark){
    const n = (model.bookmarkDestinations || model.destinations || []).length;
    buttons.push(`<button type="button" class="bmarker" data-bmarker="bookmark" aria-label="${n ? "Open bookmark destination" : "Open bookmark"}"${n > 1 ? ` aria-haspopup="menu"` : ""}>${iconBookmark}${n > 1 ? `<span class="bmarkercount">${n}</span>` : ""}</button>`);
  }
  if (model.sentTo && model.sentTo.length){
    const n = model.sentTo.length;
    buttons.push(`<button type="button" class="bmarker" data-bmarker="sendto" aria-label="Open sent-to destination"${n > 1 ? ` aria-haspopup="menu"` : ""}><span class="rot180">${iconInto}</span>${n > 1 ? `<span class="bmarkercount">${n}</span>` : ""}</button>`);
  }
  return buttons.length ? `<div class="bubblemarkers">${buttons.join("")}</div>` : "";
}

/* Agent and provenance are not part of hashTurns (role+text only). Include
   them in the rebuild signature so a same-text turn whose attribution
   changes does not leave stale bubbleTurns. */
export function hashTurnAttrs(turns, hash = hashStr){
  let s = "";
  const list = turns || [];
  for (let i = 0; i < list.length; i++){
    const t = list[i] || {};
    s += "\n" + (t.agent || "") + "\0";
    const hasProv = Object.prototype.hasOwnProperty.call(t, "prov");
    s += hasProv ? "1" : "0";
    try { s += JSON.stringify(hasProv ? t.prov : null); }
    catch { s += "\0"; }
  }
  return hash(s);
}

function cloneJSONValue(v){
  if (v === undefined) return undefined;
  try { return JSON.parse(JSON.stringify(v)); }
  catch { return undefined; }
}

export function elicitationKey(data){
  if (!data || !data.elicitation_waiting) return "";
  const items = Array.isArray(data.elicitations) ? data.elicitations : [];
  return String(data.elicitation_count || items.length) + ":" +
    items.map(it => [it.server || "", it.message || "", it.mode || "", it.url || ""].join("\0")).join("\n");
}

export function isSafeElicitationURL(raw){
  if (!raw || typeof raw !== "string") return false;
  const trimmed = raw.trim();
  const lower = trimmed.toLowerCase();
  if (!lower.startsWith("http://") && !lower.startsWith("https://")) return false;
  try {
    const u = new URL(trimmed);
    return (u.protocol === "http:" || u.protocol === "https:") && !!u.host && !u.username;
  } catch {
    return false;
  }
}

export const ELICITATION_OPEN_LABEL = "Open Terminal to respond";

export function elicitationStatusHTML(data, { escape = esc } = {}){
  if (!data || !data.elicitation_waiting) return "";
  const items = Array.isArray(data.elicitations) ? data.elicitations : [];
  const count = Number(data.elicitation_count) || items.length;
  const heading = count > 1
    ? `${count} MCP input requests are waiting in Claude's dialog.`
    : "An MCP server needs input in Claude's dialog.";
  const rows = items.map(it => {
    const server = escape(String(it.server || "MCP"));
    const message = escape(String(it.message || ""));
    const mode = it.mode ? escape(String(it.mode)) : "";
    const modeLine = mode ? `<div class="elicitation-mode">${mode}</div>` : "";
    let urlLine = "";
    if (it.mode === "url" && isSafeElicitationURL(it.url)){
      const href = escape(String(it.url));
      urlLine = `<div class="elicitation-url"><a href="${href}" target="_blank" rel="noopener noreferrer">${href}</a></div>`;
    }
    return `<div class="elicitation-item"><div class="elicitation-server">${server}</div>` +
      `<div class="elicitation-message">${message}</div>${modeLine}${urlLine}</div>`;
  }).join("");
  const open = `<div class="permbtns"><button type="button" data-open-terminal="1" class="permbtn">${ELICITATION_OPEN_LABEL}</button></div>`;
  return `<div class="pending elicitation" role="status">${escape(heading)}${rows}${open}</div>`;
}

export const COMPACTING_STATUS =
  "Claude is compacting the conversation context. Responses will resume when it finishes.";

export function compactingStatusHTML(){
  return `<div class="pending compacting" role="status">${COMPACTING_STATUS}</div>`;
}

/* Everything peekBlockHTML renders. Its own signature so the pane can move on
   its own — a spinner changes the capture on every poll, and that must cost a
   pane-sized repaint, not a transcript-sized one. */
export function buildPeekSignature(parts){
  const p = parts || {};
  return [
    p.showPeek ? "1" : "0",
    p.peekHash || "",
    p.forcePeek ? "1" : "0",
    p.label || "",
    p.termFull ? "1" : "0",
    p.unconfirmed ? "1" : "0",
  ].join("|");
}

export function permOptionsKey(opts){
  return (opts || []).map(o => o.key + o.name + (o.kind || "")).join(",");
}

export function nearBottom(scrollHeight, scrollTop, clientHeight, slack = SCROLL_NEAR_BOTTOM_PX){
  return scrollHeight - scrollTop - clientHeight < slack;
}

/* Pure scroll decision for the chat scroller. `want` is the open-at-newest
   intent (chatScrollBottom); when the box has no layout the intent stays
   pending so a later frame / resize can re-assert. Overscroll always clamps.
   A reader who scrolled up (want false, scrollTop below max) is never yanked. */
export function pinDecision({ scrollTop = 0, scrollHeight = 0, clientHeight = 0, want = false } = {}){
  const st = scrollTop || 0;
  const sh = scrollHeight || 0;
  const ch = clientHeight || 0;
  const max = Math.max(0, sh - ch);
  if (want){
    if (ch === 0) return { action: "none", scrollTop: st, pending: true };
    return { action: "pin", scrollTop: max, pending: false };
  }
  if (st > max) return { action: "clamp", scrollTop: max, pending: false };
  if (st < 0) return { action: "clamp", scrollTop: 0, pending: false };
  return { action: "none", scrollTop: st, pending: false };
}

export function liveBk(i){ return `i:${i}`; }
export function histBk(si, ti){ return `h:${si}:${ti}`; }

export function turnRoleClass(role, { hist = false, media = false, echo = false } = {}){
  const base = role === "user" ? "user" : "assistant";
  const parts = ["turn"];
  if (hist) parts.push("hist");
  parts.push(base);
  if (media) parts.push("media");
  if (echo) parts.push("echo");
  return parts.join(" ");
}

export function histLoadHTML(priorTurns){
  const n = priorTurns || 0;
  if (n <= 0) return "";
  return `<div class="chatseam"><button class="histload">show earlier history &middot; ${n} turn${n === 1 ? "" : "s"}</button></div>`;
}

export function pendingEmptyHTML({
  freshSurface, pending, supervision = "", delivering = false, ended = false,
} = {}){
  if (freshSurface) return `<div class="pending">fresh chat \u2014 send a prompt</div>`;
  /* Ahead of the startup wording on purpose: by the time the prompt is being
     delivered, SessionStart has arrived, so "waiting for SessionStart" would
     name the one thing that already happened — which is how a healthy launch
     came to read as a stuck one. */
  if (delivering){
    return `<div class="pending">delivering the first prompt \u2014 waiting for Claude to log it</div>`;
  }
  if (supervision === "claude_starting"){
    return `<div class="pending">starting Claude \u2014 waiting for SessionStart</div>`;
  }
  if (supervision === "claude_strict" || supervision === "claude_failed"){
    return `<div class="pending">${pending
      ? "waiting for the first confirmed turn"
      : "no confirmed turns yet"}</div>`;
  }
  if (supervision === "claude_unsupported"){
    if (ended) return "";
    return `<div class="pending">this chat uses an older scimux setup \u2014 fork or relaunch it to use all features</div>`;
  }
  return `<div class="pending">${pending
    ? "waiting for the agent's transcript file \u2014 showing the raw terminal below"
    : "no readable transcript \u2014 showing the raw terminal below"}</div>`;
}

export function workPulseShouldRun(live, mustShowPane, replyReady = false){
  return live === "active" && !mustShowPane && !replyReady;
}

/* Pure scroll-target decision for history ride-back.
   seams = [{seam, el?}] earliest-first; returns index or -1 for curseam fallback. */
export function histScrollTargetIndex(seams, scrollTo){
  if (!scrollTo || scrollTo === "seam") return -1;
  const T = Date.parse(scrollTo) || 0;
  const i = (seams || []).findIndex(s => (Date.parse(s.seam || "") || 0) >= T);
  return i;
}

/* Match a pending jump against live turns. Returns index or -1. */
export function matchPendingJumpInTurns(pending, turns){
  if (!pending) return -1;
  const list = turns || [];
  let idx = -1;
  if (pending.uid)
    idx = list.findIndex(t => t.uid === pending.uid &&
      (t.segment || 0) === (pending.segment || 0) && (t.record || 0) === (pending.record || 0));
  if (idx < 0 && pending.turnTime) idx = list.findIndex(t => t.time === pending.turnTime);
  if (idx < 0 && pending.text) idx = list.findIndex(t =>
    t.text === pending.text || (pending.textPrefix && String(t.text || "").startsWith(pending.text)));
  return idx;
}

/* Match pending jump against history turn element datasets.
   hists = [{uid, segment, record, time, el?}] */
export function matchPendingJumpInHist(pending, hists){
  if (!pending) return null;
  const list = hists || [];
  let h = pending.uid
    ? list.find(t => t.uid === pending.uid &&
        (+t.segment || 0) === (pending.segment || 0) &&
        (+t.record || 0) === (pending.record || 0))
    : null;
  if (!h && pending.turnTime) h = list.find(t => t.time === pending.turnTime);
  return h || null;
}

/* ---------- asset / attachment tile pure builders ---------- */

export function tileStyle(tileBox, key){
  const b = tileBox && tileBox[key];
  return b ? ` style="width:${b.w}px;height:${b.h}px"` : "";
}

export function attTileHTML(nodeId, leaf, isImage, {
  escape = esc, iconFile = "", tileBox = {}, rasterRe = RASTER_RE,
  assetURL = (path) => path,
} = {}){
  const url = assetURL(`/api/nodes/${encodeURIComponent(nodeId)}/attachments/${encodeURIComponent(leaf)}`);
  const name = leaf.replace(/^[0-9a-f]{8}-/, "");
  isImage = isImage && rasterRe.test(name);
  return isImage
    ? `<a class="attthumb" data-asset-preview data-name="${escape(name)}" data-tkey="${escape(leaf)}"${tileStyle(tileBox, leaf)} href="${url}" target="_blank" rel="noopener" title="${escape(name)}">` +
      `<img src="${url}" alt="${escape(name)}" loading="lazy"></a>`
    : `<a class="attfile" href="${url}" target="_blank" rel="noopener" download="${escape(name)}" title="${escape(name)}">` +
      `${iconFile}<span>${escape(name)}</span></a>`;
}

export function refTilesHTML(refs, nodeId, deps = {}){
  if (!refs || !refs.length) return "";
  const rasterRe = deps.rasterRe || RASTER_RE;
  const tiles = refs.map(r => {
    const leaf = (r.path || "").split("/").pop();
    return attTileHTML(nodeId, leaf, rasterRe.test(leaf), deps);
  }).join("");
  return `<div class="attrow">${tiles}</div>`;
}

export function assetTileHTML(nodeId, id, alt, rec, {
  escape = esc, iconFile = "", tileBox = {}, rasterRe = RASTER_RE,
  assetURL = (path) => path,
} = {}){
  if (!rec){
    return `<span class="attfile missing" title="${escape(alt || id)} — unavailable">` +
           `${iconFile}<span>${escape(alt || "file")} (unavailable)</span></span>`;
  }
  const url = assetURL(`/api/nodes/${encodeURIComponent(nodeId)}/assets/${encodeURIComponent(id)}`);
  const name = rec.name || alt || "file";
  const isImage = rasterRe.test(name);
  return isImage
    ? `<a class="attthumb" data-asset-preview data-name="${escape(name)}" data-tkey="${escape(id)}"${tileStyle(tileBox, id)} href="${url}" target="_blank" rel="noopener" title="${escape(name)}">` +
      `<img src="${url}" alt="${escape(name)}" loading="lazy"></a>`
    : `<a class="attfile" href="${url}" target="_blank" rel="noopener" download="${escape(name)}" title="${escape(name)}">` +
      `${iconFile}<span>${escape(name)}</span></a>`;
}

const IMPORT_REF_RE = /(!?)\[([^\]]*)\]\(scimux-import:(\d+):(\d+):([a-z_]+)\)/g;

export function importTileHTML(nodeId, turn, occurrence, reason, alt, isImage, {
  escape = esc, iconFile = "", externalAllowed = false,
} = {}){
  const messages = {
    outside_workspace: ["Attachment outside workspace", "External attachments are disabled."],
    not_found: ["Attachment unavailable", "File no longer exists."],
    retry_ready: ["Attachment ready to import", "File is now available."],
    unreadable: ["Attachment unavailable", "File cannot be read."],
    not_regular: ["Attachment unavailable", "Unsupported filesystem object."],
    too_large: ["Attachment unavailable", "File exceeds the import limit."],
    storage: ["Attachment unavailable", "Storage budget or free-space limit prevented import."],
    chat_unavailable: ["Attachment unavailable", "Chat is unavailable for this operation."],
  };
  const copy = messages[reason] || ["Attachment unavailable", "Attachment could not be imported."];
  const retry = reason !== "outside_workspace" || externalAllowed;
  const actions = (reason === "outside_workspace" && !externalAllowed)
    ? `<button data-asset-settings>Open attachment settings</button>`
    : (retry ? `<button data-asset-retry data-node="${escape(nodeId)}" data-turn="${turn}" data-occurrence="${occurrence}">Retry import</button>` : "");
  return `<span class="assetblocked ${isImage ? "image" : "file"}" role="status">` +
    `${isImage ? "" : iconFile}<span><strong>${copy[0]}</strong><span>${copy[1]}</span>` +
    `<span class="assetblocked-name">${escape(alt || "file")}</span>${actions}</span></span>`;
}

/* A 422 retry body is JSON text on Error.message (api.js decodeResponse).
   Only not_found retires the stale button; every other failure stays retryable. */
export function readRetryFailure(err){
  if (!err || Number(err.status) !== 422) return "";
  try {
    const body = JSON.parse(err.message);
    return body && typeof body.reason === "string" ? body.reason : "";
  } catch {
    return "";
  }
}

export function splitImportRefs(text, nodeId, deps = {}){
  IMPORT_REF_RE.lastIndex = 0;
  if (!IMPORT_REF_RE.test(text || "")) return { clean: text || "", html: "", missing: [] };
  const tiles = [];
  const missing = [];
  const escape = deps.escape || esc;
  let nextToken = 0;
  let priorMissingEnd = -1;
  const source = text || "";
  const clean = source.replace(IMPORT_REF_RE, (_m, bang, alt, turn, occurrence, reason, offset) => {
    if (reason === "not_found") {
      let name = alt || "file";
      try { name = decodeURIComponent(name); } catch { /* Keep malformed legacy labels as text. */ }
      let token;
      do { token = `\uE000${nextToken++}\uE001`; } while (source.includes(token));
      missing.push({ token, html: `<span class="missingfile">${escape(name)}</span>` });
      const separator = offset === priorMissingEnd ? " " : "";
      priorMissingEnd = offset + _m.length;
      return separator + token;
    }
    priorMissingEnd = -1;
    tiles.push(importTileHTML(nodeId, +turn, +occurrence, reason, alt, bang === "!", deps));
    return "";
  }).replace(/\n{3,}/g, "\n\n").trim();
  return { clean, html: tiles.length ? `<div class="attrow blockedrow">${tiles.join("")}</div>` : "", missing };
}

export function renderMissingImportText(rendered, missing = []){
  for (const item of missing) rendered = rendered.replace(item.token, () => item.html);
  return rendered;
}

export function splitAssetRefs(text, nodeId, assets, deps = {}){
  const re = deps.assetRefRe || ASSET_REF_RE;
  re.lastIndex = 0;
  if (!re.test(text || "")) return { clean: text || "", html: "" };
  const tiles = [];
  const clean = (text || "").replace(re, (_m, _bang, alt, id) => {
    tiles.push(assetTileHTML(nodeId, id, alt, (assets || {})[id], deps));
    return "";
  }).replace(/\n{3,}/g, "\n\n").trim();
  return { clean, html: `<div class="attrow">${tiles.join("")}</div>` };
}

/* splitPermTitle: pure. Parse "Verb `payload`" from ACP ToolCall.Title; anything
   else (codex bare commands, search regexes, think prose, future agents) falls
   through as { verb: "", code: <whole title> }. Never throws, never null. */
export function splitPermTitle(title){
  const m = /^([A-Za-z][\w ]{0,20})\s+`([\s\S]+)`\s*$/.exec(title || "");
  return m ? { verb: m[1], code: m[2] } : { verb: "", code: title || "" };
}

/* ---------- bubble / keyrow HTML ---------- */

export function bubbleActionsHTML(turn, {
  escape = esc, fmtTime = fmtBubbleTime,
  iconBranch = "", iconInto = "", iconCopy = "",
} = {}){
  return `<div class="bubwhen">${escape(fmtTime(turn && turn.time))}</div>` +
    `<button data-bact="fork"><span>${iconBranch}</span>fork from here</button>` +
    /* send-to sits beside fork — both take this text elsewhere, fork into a new
       chat, send-to into an existing one. The ellipsis is the HIG signal that a
       picker follows; "copy" would collide with the clipboard action below. */
    `<button data-bact="sendto"><span class="rot180">${iconInto}</span>send to&#8230;</button>` +
    `<button data-bact="desc"><span class="rot90l">${iconInto}</span>use as description</button>` +
    `<button data-bact="bookmark"><span>${iconInto}</span>bookmark</button>` +
    `<button data-bact="copy"><span>${iconCopy}</span>copy</button>`;
}

/* Kind → CSS class. Underscores become hyphens; empty → "unknown". */
const PERM_KIND_CLASS = {
  allow: "allow",
  allow_always: "allow-always",
  reject: "reject",
  reject_always: "reject-always",
};

/* Codex ships Decision.Key enums ("accept", "cancel"); ACP ships human labels
   ("Allow once"). When Name is a single token and kind is known, render a
   readable label and keep the raw enum in title= for inspection. */
const PERM_KIND_LABEL = {
  allow: "Allow",
  allow_always: "Allow always",
  reject: "Reject",
  reject_always: "Reject always",
};

const PERM_CODE_KINDS = new Set(["execute", "edit", "read", "search"]);

/* One rendering decision for an agent permission ask, shared by the live
   approval row and the decision audit so the audit can never read worse
   than the thing it audits. md() is escape-first (format.js), so this is
   still "never innerHTML of unescaped agent content". */
export function permBodyHTML(title, toolKind, {
  escape = esc, markdown = md, codeClass = "permcode", textClass = "permtext",
  verbClass = "permverb",
} = {}){
  const { verb, code } = splitPermTitle(title);
  const asCode = !!verb || PERM_CODE_KINDS.has(toolKind || "");
  const verbHTML = verb
    ? `<div class="${verbClass}">${escape(verb)}</div>` : "";
  const bodyHTML = asCode
    ? `<pre class="${codeClass}">${escape(code)}</pre>`
    : `<div class="${textClass}">${markdown(code)}</div>`;
  return { verbHTML, bodyHTML };
}

function permOptionLabel(o){
  const name = o && o.name != null ? String(o.name) : "";
  const kind = (o && o.kind) || "";
  const looksEnum = name.length > 0 && !/\s/.test(name);
  if (looksEnum && PERM_KIND_LABEL[kind]){
    return { label: PERM_KIND_LABEL[kind], title: name };
  }
  return { label: name, title: "" };
}

function permOptionClass(kind){
  return PERM_KIND_CLASS[kind] || "unknown";
}

/* Pure core for the approval "show all" measure pass. Vertical overflow only
   (scrollHeight vs clientHeight). .permcode uses white-space:pre-wrap and
   overflow-wrap:anywhere so a long single-line command wraps into line boxes
   under the 3-line clamp; that vertical growth is what unhides "show all"
   (G1a). Horizontal scroll is not the read path.

   Three states, not two. With no layout box — body.map-full hides #chat while
   polling keeps rendering it — the element measures 0x0, and reading that as
   "nothing to show" hides the only way to read a long ask. An unmeasured row
   is not a measured one; the caller waits for a box instead of deciding.
   Same rule as pinDecision's clientHeight-0 case. */
export const PERM_MORE_SHOW = "show";
export const PERM_MORE_HIDE = "hide";
export const PERM_MORE_UNKNOWN = "unknown";

export function permMoreDecision(scrollHeight, clientHeight){
  const ch = clientHeight || 0;
  if (ch <= 0) return PERM_MORE_UNKNOWN;
  return (scrollHeight || 0) > ch ? PERM_MORE_SHOW : PERM_MORE_HIDE;
}

export function restoreDraftDecision({ original = "", current = "" } = {}){
  const orig = String(original || "");
  const cur = String(current || "");
  if (cur === "" || cur === orig){
    return { action: "restore", text: orig };
  }
  return { action: "keep", text: cur };
}

export function keyRowHTML({
  attention, attentionHidden, source, permTitle, permOptions,
  permToolKind = "", permReason = "", permRequestId = "",
  permDialogId = "", permManual = false,
  elicitationWaiting = false,
  expanded = false, axScreenReader = false, escape = esc,
} = {}){
  const opts = permOptions || [];
  const epochBound = !!(permDialogId && opts.length);
  const epochManual = !!(permManual && (permDialogId || permReason));
  /* A live Claude dialog always uses the epoch-bound surface — never the
     generic unbound keypad — even when map attention is suppressed (armed
     auto-approve) or locally hidden. */
  if (epochBound || epochManual){
    attentionHidden = false;
  } else if (elicitationWaiting){
    /* Elicitation is a dedicated renderer in the transcript. Never fall
       through to guessed Yes/No, digits, or the generic tmux keypad. */
    return "";
  } else if (!attention || attentionHidden){
    return "";
  }
  if (epochManual && !epochBound){
    const reason = (permReason && String(permReason)) ||
      "A dialog is visible but its options could not be read safely. Open Terminal to respond.";
    const title = permTitle
      ? `<div class="permtext">${escape(permTitle)}</div>` : "";
    const open = `<button type="button" data-open-terminal="1" class="permbtn">Open Terminal</button>`;
    return `<span class="hint">${escape(reason)}</span>` + title +
      `<div class="permbtns">${open}</div>`;
  }
  /* Inspect is a neutral diagnostic. Non-AX gets the default terminal
     keypad so a misclassified dialog can be answered, plus a distinct
     Dismiss that never sends a key. AX inspect is Dismiss-only: a stray
     "1" would expand to 1+Enter and submit a prompt. */
  if (attention === "inspect" && !epochBound){
    const hint = `<span class="hint">${escape(HINTS.inspect)}</span>`;
    const dismiss = inspectDismissButton();
    if (axScreenReader){
      return hint + `<div class="permbtns keys">${dismiss}</div>`;
    }
    return hint + `<div class="permbtns keys">${tmuxKeyButtons()}${dismiss}</div>`;
  }
  if (source === "acp" || ((permRequestId || permDialogId) && opts.length)){
    if (!opts.length){
      return `<span class="hint">Waiting for approval options from the agent...</span>`;
    }
    const { verbHTML, bodyHTML } = permBodyHTML(permTitle, permToolKind, {
      escape, codeClass: "permcode", textClass: "permtext", verbClass: "permverb",
    });
    const clamp = expanded ? "" : " clamped";
    /* Start hidden; revealPermMoreIfNeeded unhides only when .permask overflows
       vertically. Single-line asks must not get a dead control. */
    const more = expanded ? "" : `<button class="permmore" hidden>show all</button>`;
    /* Reason is a distinct line above verb+body (G1b). Same dimmed chrome as
       .permverb so they cannot collapse into one visual line. Empty = omit. */
    const reason = (permReason && String(permReason)) || "";
    const reasonEl = reason
      ? `<div class="permreason">${escape(reason)}</div>` : "";
    /* .permmore is a sibling of .permask — inside the clamp it is clipped
       whenever the ask exceeds three lines (the exact moment it is needed).
       P5b: .permreason is a sibling too (like .hint). Inside the clamp it
       stole lines from the ask body; outside, the reason is always visible
       and revealPermMoreIfNeeded measures body overflow only. */
    const mask = `<div class="permask${clamp}">${verbHTML}${bodyHTML}</div>`;
    /* data-request-id is the opaque perm_request_id for structured (ACP/
       Codex) rows. data-dialog-id is a server-minted visible-dialog epoch
       for Claude: Notification has no PermissionRequest id, so this is not
       Claude's request identity. Captured at click so a later poll cannot
       retarget the decision. */
    const reqAttr = permRequestId
      ? ` data-request-id="${escape(String(permRequestId))}"` : "";
    const dialogAttr = permDialogId
      ? ` data-dialog-id="${escape(String(permDialogId))}"` : "";
    const btns = `<div class="permbtns">${opts.map(o => {
      const { label, title } = permOptionLabel(o);
      const cls = permOptionClass(o.kind);
      const titleAttr = title ? ` title="${escape(title)}"` : "";
      return `<button data-key="${escape(o.key)}"${reqAttr}${dialogAttr} class="permbtn ${cls}"${titleAttr}>${escape(o.key)}. ${escape(label)}</button>`;
    }).join("")}</div>`;
    /* Hint outside the clamp; wording must not repeat the tool title — that
       is the ask body, rendered immediately below. Order:
       hint → permreason → permask(verb+body) → permmore → permbtns */
    const hint = `<span class="hint">The agent needs your approval \u2014 choose one:</span>`;
    return hint + reasonEl + mask + more + btns;
  }
  return `<span class="hint">${escape(HINTS[attention] || `The agent waits for your ${attention} \u2014 keys go straight to its terminal:`)}</span>` +
    `<div class="permbtns keys">${tmuxKeyButtons()}</div>`;
}

function tmuxKeyButtons(){
  return ["1","2","3","4","y","n","Up","Down","Enter","Escape"].map(k =>
    `<button data-key="${k}" class="permbtn">${KEYS[k] || k}</button>`).join("");
}

function inspectDismissButton(){
  const title = "Dismiss \u2014 I looked; do not send Escape to the agent";
  const aria = "Dismiss inspect; does not send Escape to the agent";
  return `<button type="button" data-dismiss-attention="inspect" class="permbtn" title="${title}" aria-label="${aria}">Dismiss</button>`;
}

export function peekBlockHTML({
  forcePeek, label, termFull, peekText, unconfirmed, escape = esc,
} = {}){
  if (!peekText) return "";
  return `<div class="peekblock">
           <div class="peekhead">
             <span class="plabel">${forcePeek ? escape(label) : "Terminal"}</span>
             <button id="termfull" aria-label="${termFull ? "shrink terminal" : "expand terminal"}">${termFull ? "\u2921 shrink" : "\u2922 expand"}</button>
           </div>
           <div class="peek${termFull ? " full" : ""}">${escape(peekText)}</div>` +
         (unconfirmed
           ? `<div class="pnotice">Delivery to the terminal could not be confirmed.
                <button id="decresolve">I checked \u2014 resume sending</button></div>`
           : "") +
        `</div>`;
}

export function echoBubbleHTML(text, tilesHTML = "", { markdown = md } = {}){
  const media = tilesHTML && !text ? " media" : "";
  return `
      <div class="turn user echo${media}">
        <div class="bubble" title="you — delivering">${markdown(text || "")}${tilesHTML || ""}</div>
      </div>`;
}

/* ---------- auto-approve chrome + decision audit (fixes-2 P4) ---------- */

export const AUTO_APPROVE_HELP =
  "Automatically selects the sole one-time approval option for the current turn. Resets when the turn finishes, when you stop it, or on /clear or /exit.";
export const AUTO_APPROVE_CLAUDE_HELP = AUTO_APPROVE_HELP;
export const AUTO_APPROVE_UNSUPPORTED_HELP =
  "Auto-approval isn't available for this chat.";
/* Claude reaches auto-approve through a PermissionRequest hook that only a
   scimux-owned launch installs, so an adopted pane (or one launched before the
   feature) can never arm. Say what would change that rather than refusing. */
export const AUTO_APPROVE_CLAUDE_UNSUPPORTED_HELP =
  "Auto-approval needs a Claude chat scimux launched itself \u2014 relaunch or fork this chat to use it.";
export const AUTO_APPROVE_LABEL_FULL = "Auto-approve this turn";
export const AUTO_APPROVE_CLAUDE_LABEL_FULL = AUTO_APPROVE_LABEL_FULL;
export const AUTO_APPROVE_LABEL_NARROW = "Auto-approve";

export function autoApproveCopy(){
  return {
    labelFull: AUTO_APPROVE_LABEL_FULL,
    help: AUTO_APPROVE_HELP,
    ariaBase: "Auto-approve eligible tool requests this turn",
  };
}

/* Which refusal to show. Only Claude has an actionable one. */
export function unsupportedHelp(agent){
  return String(agent || "").toLowerCase() === "claude"
    ? AUTO_APPROVE_CLAUDE_UNSUPPORTED_HELP
    : AUTO_APPROVE_UNSUPPORTED_HELP;
}

export function autoApproveViewNorm(view){
  const v = view || {};
  return {
    supported: !!v.supported,
    enabled: !!v.enabled,
    phase: v.phase || "off",
    count: Number(v.count) || 0,
    error: v.error ? String(v.error) : "",
  };
}

/* Accessible name for the toggle. Count is spoken only when enabled and > 0. */
export function autoApproveAriaName({ enabled = false, count = 0, supported = true, agent = "" } = {}){
  if (!supported) return unsupportedHelp(agent);
  const base = autoApproveCopy().ariaBase;
  if (enabled && (Number(count) || 0) > 0){
    return `${base}; ${Number(count)} approved.`;
  }
  return base;
}

/* Pure presentation model for the #autoapprove toggle.
   `node` is optional: with one, the control disappears for a chat that can
   never arm the lease. Auto-approve presupposes a turn to come, and a
   closed thread or dead liveness has none left — canReceiveSend is exactly
   that predicate ("a chat with a process left to type into"), so the two
   cannot drift apart. Hidden rather than disabled: a disabled control still
   claims the capability belongs here. A node we have not polled yet is not
   provably dead and stays visible. */
export function autoApproveChromeModel(view, { agent = "", node = null } = {}){
  const v = autoApproveViewNorm(view);
  const agentL = String(agent || "").toLowerCase();
  const copy = autoApproveCopy();
  const hidden = node ? !canReceiveSend(node) : false;
  // Support is the server's verdict alone (agent, transport, and — for Claude —
  // a permission-capable hook bundle). The UI never second-guesses it by name.
  if (!v.supported){
    return {
      hidden,
      supported: false,
      enabled: false,
      phase: "off",
      count: 0,
      error: "",
      pressed: false,
      disabled: true,
      showWarning: false,
      showBadge: false,
      badgeText: "",
      labelFull: copy.labelFull,
      labelNarrow: AUTO_APPROVE_LABEL_NARROW,
      title: unsupportedHelp(agentL),
      ariaLabel: unsupportedHelp(agentL),
      toggleIcon: "off",
      classNames: ["autoapprove", "unsupported"],
    };
  }
  const enabled = !!v.enabled && (v.phase === "primed" || v.phase === "armed");
  const count = enabled ? (Number(v.count) || 0) : 0;
  const classes = ["autoapprove", enabled ? "on" : "off"];
  if (v.error) classes.push("has-error");
  return {
    hidden,
    supported: true,
    enabled,
    phase: v.phase,
    count,
    error: v.error || "",
    pressed: enabled,
    disabled: hidden,
    showWarning: enabled,
    showBadge: enabled && count > 0,
    badgeText: enabled && count > 0 ? String(count) : "",
    labelFull: copy.labelFull,
    labelNarrow: AUTO_APPROVE_LABEL_NARROW,
    title: copy.help,
    ariaLabel: autoApproveAriaName({ enabled, count, supported: true, agent: agentL }),
    toggleIcon: enabled ? "on" : "off",
    classNames: classes,
  };
}

/* Inner HTML for the toggle. All dynamic text is escaped. */
export function autoApproveButtonInnerHTML(model, icons = {}, { escape = esc } = {}){
  const m = model || {};
  const icon = m.toggleIcon === "on"
    ? (icons.ICON_TOGGLE_ON || "")
    : (icons.ICON_TOGGLE_OFF || "");
  const warn = m.showWarning ? (icons.ICON_WARN || "") : "";
  const badge = m.showBadge
    ? `<span class="aa-badge" aria-hidden="true">${escape(m.badgeText)}</span>`
    : "";
  const err = m.error
    ? `<span class="aa-error" role="status">${escape(m.error)}</span>`
    : "";
  return `<span class="aa-icon" aria-hidden="true">${icon}</span>` +
    (warn ? `<span class="aa-warn" aria-hidden="true">${warn}</span>` : "") +
    `<span class="aa-label">` +
      `<span class="aa-label-full">${escape(m.labelFull || AUTO_APPROVE_LABEL_FULL)}</span>` +
      `<span class="aa-label-narrow">${escape(m.labelNarrow || AUTO_APPROVE_LABEL_NARROW)}</span>` +
    `</span>` +
    badge + err;
}

/* Stable hash of decision surfaces for the chat rebuild signature. */
export function decisionsHash(decisions){
  return (decisions || []).map(d => {
    const dec = (d && d.decision) || d || {};
    const sel = dec.selected || {};
    return [
      d && d.record != null ? d.record : "",
      dec.request_id || "",
      sel.key || "",
      dec.lease_id || "",
    ].join(":");
  }).join("|");
}

/* Merge turns and decision surfaces by durable record index. */
export function mergeTimelineItems(turns, decisions){
  const items = [];
  (turns || []).forEach((t, i) => {
    const rec = t && t.record != null ? Number(t.record) : i;
    items.push({ kind: "turn", record: rec, index: i, turn: t, ord: items.length });
  });
  (decisions || []).forEach(d => {
    const rec = d && d.record != null ? Number(d.record) : 0;
    items.push({ kind: "decision", record: rec, decision: d, ord: items.length });
  });
  items.sort((a, b) => {
    if (a.record !== b.record) return a.record - b.record;
    if (a.kind !== b.kind) return a.kind === "turn" ? -1 : 1;
    return a.ord - b.ord;
  });
  /* A lease is one turn. Number its durable decisions after timeline sorting
     so the last visible number remains the turn's approval total after the
     live toggle resets to off. Missing lease ids stay unnumbered: grouping
     unrelated legacy records would invent a turn boundary. */
  const byLease = new Map();
  items.forEach(item => {
    if (item.kind !== "decision") return;
    const lease = item.decision && item.decision.decision &&
      String(item.decision.decision.lease_id || "");
    if (!lease) return;
    const ordinal = (byLease.get(lease) || 0) + 1;
    byLease.set(lease, ordinal);
    item.ordinal = ordinal;
  });
  return items;
}

/* Audit surface for one automatic decision. The approved request is always
   visible and uses the exact same prose-vs-command renderer as the live ask;
   only mechanical audit metadata is collapsed. permBodyHTML / md() are
   escape-first, so this is still never innerHTML of unescaped agent content.
   No bubble actions. */
export function decisionRowHTML(surface, {
  escape = esc, markdown = md, fmtTime = fmtWhen, ordinal = 0,
} = {}){
  const s = surface || {};
  const d = s.decision || {};
  const sel = d.selected || {};
  const title = d.title != null ? String(d.title) : "";
  const selName = sel.name != null ? String(sel.name) : (sel.key != null ? String(sel.key) : "");
  const opts = Array.isArray(d.options) ? d.options : [];
  const optsHTML = opts.map(o => {
    const key = o && o.key != null ? String(o.key) : "";
    const name = o && o.name != null ? String(o.name) : "";
    const kind = o && o.kind != null ? String(o.kind) : "unknown";
    return `<li><code>${escape(key)}</code> ${escape(name)}` +
      ` <span class="aa-kind">(${escape(kind)})</span></li>`;
  }).join("");
  const when = s.time ? escape(fmtTime(s.time)) : "";
  const agent = d.agent != null ? String(d.agent) : "";
  const toolKind = d.tool_kind != null ? String(d.tool_kind) : "";
  const reason = d.reason != null ? String(d.reason) : "";
  const req = d.request_id != null ? String(d.request_id) : "";
  const lease = d.lease_id != null ? String(d.lease_id) : "";
  const selKey = sel.key != null ? String(sel.key) : "";
  const selKind = sel.kind != null ? String(sel.kind) : "";
  const { verbHTML, bodyHTML } = permBodyHTML(title, toolKind, {
    escape, markdown,
    codeClass: "decision-cmd", textClass: "decision-text", verbClass: "decision-verb",
  });
  const reasonHTML = reason
    ? `<div class="decision-row"><span class="k">Reason</span>` +
      ` <div class="decision-reason">${markdown(reason)}</div></div>`
    : "";
  const count = Number.isSafeInteger(Number(ordinal)) && Number(ordinal) > 0
    ? ` (#${Number(ordinal)})` : "";
  return `<div class="decision" data-record="${escape(String(s.record ?? ""))}"` +
    ` data-request="${escape(req)}">` +
    `<div class="decision-head"><span class="decision-state">Auto-approved${count}</span>` +
      (selName ? `<span class="decision-selected">\u2014 ${escape(selName)}</span>` : "") +
    `</div>` +
    `<div class="decision-approved">${verbHTML ? verbHTML : ""}${bodyHTML}</div>` +
    `<details class="decision-meta">` +
    `<summary class="decision-sum">Audit details</summary>` +
    `<div class="decision-body">` +
    (when ? `<div class="decision-row"><span class="k">Time</span> ${when}</div>` : "") +
    (agent ? `<div class="decision-row"><span class="k">Agent</span> ${escape(agent)}</div>` : "") +
    (toolKind ? `<div class="decision-row"><span class="k">Tool</span> ${escape(toolKind)}</div>` : "") +
    reasonHTML +
    `<div class="decision-row"><span class="k">Options</span>` +
      (opts.length
        ? `<ul class="decision-opts">${optsHTML}</ul>`
        : `<span class="decision-none">no menu offered \u2014 hook decision</span>`) +
      `</div>` +
    `<div class="decision-row"><span class="k">Selected</span>` +
      (selKey ? ` <code>${escape(selKey)}</code>` : "") +
      ` ${escape(selName)}` +
      ` <span class="aa-kind">(${escape(selKind)})</span></div>` +
    `<div class="decision-row"><span class="k">Request</span> <code>${escape(req)}</code></div>` +
    `<div class="decision-row"><span class="k">Lease</span> <code>${escape(lease)}</code></div>` +
    `</div></details></div>`;
}

/* ---------- feature factory ---------- */

export function createChatFeature(deps){
  const d = deps || {};
  if (typeof d.assetURL !== "function") {
    throw new Error("createChatFeature: assetURL is required");
  }
  const roots = d.roots || {};
  const doc = d.document || (typeof document !== "undefined" ? document : null);
  const win = d.window || (typeof window !== "undefined" ? window : null);
  const CSSRef = d.CSS || (typeof CSS !== "undefined" ? CSS : { escape: s => String(s) });
  const escape = d.esc || esc;
  const markdown = d.md || md;
  const whenFn = d.fmtWhen || fmtWhen;
  const bubTimeFn = d.fmtBubbleTime || fmtBubbleTime;
  const titleFn = d.bubbleTitle || bubbleTitle;
  const hash = d.hashStr || hashStr;
  const setTimeoutFn = d.setTimeout || setTimeout;
  const clearTimeoutFn = d.clearTimeout || clearTimeout;
  const icons = d.icons || {};

  const chathead = roots.chathead;
  const chattitle = roots.chattitle;
  const chatmission = roots.chatmission;
  const chatdetails = roots.chatdetails;
  const chatdesc = roots.chatdesc;
  const chatdescinput = roots.chatdescinput;
  const chatdot = roots.chatdot;
  const chatmeta = roots.chatmeta;
  const chatagentlogo = roots.chatagentlogo;
  const infobtn = roots.infobtn;
  const msgs = roots.msgs;
  const msgwrap = roots.msgwrap;
  const chatloading = roots.chatloading;
  const gauge = roots.gauge;
  const gaugefill = roots.gaugefill;
  const keyrow = roots.keyrow;
  const convtools = roots.convtools;
  const termtoggle = roots.termtoggle;
  const autoapprove = roots.autoapprove;
  const scrollend = roots.scrollend;
  const workpulse = roots.workpulse;

  /* feature-owned state */
  let chatSig = "";
  /* Per-node chat poll validator (ETag). Keyed by node id so a tag from A is
     never sent for B; cleared on node switch (onSelectChange). */
  let chatETag = { node: "", etag: "" };
  /* Last authoritative auto-approve view for the selected node (server-owned). */
  let lastAutoView = null;
  /* Per-node: restore_draft applied once so a later poll cannot clobber typing. */
  const restoredDraft = {};
  /* In-flight POST node id — never repaint another node with a stale response. */
  let autoApproveInFlight = "";
  /* The pane's own render region: a host element inside #msgs that the
     transcript rebuild recreates, plus the signature of what is painted into
     it. Held as a reference rather than looked up, because the host is created
     by the rebuild and #msgs' markup is written wholesale. */
  let peekHost = null;
  let peekSig = "";
  const peekCleanups = [];
  let termOpen = false;
  let termFull = false;
  let sentEcho = null;
  let suppressAttentionUntil = {};
  let dismissedAttention = {};
  let chatHist = { node: "", segs: null, scrollTo: "", assets: {} };
  let chatScrollBottom = false;
  let lastTurns = [];
  let bubbleTurns = {};
  let tappedTurn = "";
  let chatDetailsOpen = false;
  let editingChatDesc = false;
  let chatDetailSavedUntil = 0;
  let chatCtxPct = { node: "", pct: null };
  /* Per-node "show all" for the approval ask. Only meaningful while this node
     still has a live acp permission row; cleared on select change or when
     attention leaves. Pure keyRowHTML stays pure — the call site computes
     expanded from this flag. */
  let permExpanded = { node: "", open: false };
  let chatLoadTimer = null;
  let chatLoadToken = 0;
  const tileBox = {};
  let lastBgTap = 0;
  let bound = false;
  const cleanups = [];
  const renderedCleanups = [];
  /* Bottom-pin re-assert: rAF + ResizeObserver, both via deps (no-op headless). */
  let pinObserver = null;
  /* Approval "show all": a second, separate observer — it waits for .permask to
     gain a box, which is a different question from the scroller's geometry. */
  let permMoreObserver = null;
  let pinRafQueued = false;

  function g(name, fallback){
    const v = d[name];
    return typeof v === "function" ? v() : (v !== undefined ? v : fallback);
  }
  function q(el, sel){
    return el && typeof el.querySelector === "function" ? el.querySelector(sel) : null;
  }
  function qa(el, sel){
    if (!el || typeof el.querySelectorAll !== "function") return [];
    return [...el.querySelectorAll(sel)];
  }
  function nodeById(id){
    if (typeof d.nodeById === "function") return d.nodeById(id);
    return (g("nodes", []) || []).find(n => n.id === id);
  }
  function hardAttention(n){
    if (typeof d.hardAttention === "function") return d.hardAttention(n);
    return hardAttentionMod(n);
  }
  function laneColor(id){
    return typeof d.laneColor === "function" ? d.laneColor(id) : "";
  }
  function agentLogo(agent){
    return typeof d.agentLogo === "function" ? d.agentLogo(agent) : "";
  }
  function api(path, opts){
    if (typeof d.api !== "function") throw new Error("chat feature requires api");
    return d.api(path, opts);
  }
  function tileDeps(){
    return {
      escape, iconFile: icons.ICON_FILE || "", tileBox, rasterRe: RASTER_RE,
      assetURL: d.assetURL,
    };
  }

  function setWorkPulse(active){
    if (workpulse && workpulse.classList)
      workpulse.classList.toggle("on", !!active);
  }

  function restartWorkPulse(){
    const isDesktop = typeof d.isDesktop === "function" ? d.isDesktop() : false;
    if (!isDesktop) return;
    if (!workpulse || !workpulse.classList || !workpulse.classList.contains("on")) return;
    workpulse.classList.remove("on");
    void workpulse.offsetWidth;
    workpulse.classList.add("on");
  }

  function clearChatLoad(){
    if (chatLoadTimer) clearTimeoutFn(chatLoadTimer);
    chatLoadTimer = null;
    if (msgwrap && msgwrap.classList) msgwrap.classList.remove("loading");
    if (chatloading) chatloading.hidden = true;
  }

  function clearRenderedListeners(){
    while (renderedCleanups.length){
      try { renderedCleanups.pop()(); } catch { /* ignore */ }
    }
    clearPeekListeners();
  }

  /* The pane's #termfull / #decresolve live and die with each pane repaint, so
     they get their own list — otherwise every poll would push another closure
     onto renderedCleanups, which is only drained on a transcript rebuild. */
  function clearPeekListeners(){
    while (peekCleanups.length){
      try { peekCleanups.pop()(); } catch { /* ignore */ }
    }
  }

  /* Repaint the terminal pane in place. Called on every refresh — including
     the transcript's skip path, which is the point. */
  function renderPeekHost(n, { showPeek, peekText, forcePeek, label, termFull: full, unconfirmed }){
    if (!peekHost) return;
    const sig = buildPeekSignature({
      showPeek, peekHash: hash(peekText || ""), forcePeek, label,
      termFull: full, unconfirmed,
    });
    if (sig === peekSig) return;
    peekSig = sig;
    clearPeekListeners();
    peekHost.innerHTML = showPeek && peekText
      ? peekBlockHTML({ forcePeek, label, termFull: full, peekText, unconfirmed, escape })
      : "";
    const pk = q(peekHost, ".peek");
    if (pk) pk.scrollTop = pk.scrollHeight;
    const onPeek = (node, event, handler) => {
      if (!node || typeof node.addEventListener !== "function") return;
      node.addEventListener(event, handler);
      peekCleanups.push(() => node.removeEventListener(event, handler));
    };
    const tf = q(peekHost, "#termfull") || (doc && doc.querySelector ? doc.querySelector("#termfull") : null);
    onPeek(tf, "click", () => { termFull = !termFull; peekSig = ""; refreshChat(); });
    const dr = q(peekHost, "#decresolve") || (doc && doc.querySelector ? doc.querySelector("#decresolve") : null);
    onPeek(dr, "click", async () => {
      try { await api(`/api/nodes/${encodeURIComponent(n.id)}/send/resolve`, { method: "POST" }); }
      catch { /* ignore */ }
      chatSig = "";
      if (typeof d.tick === "function") d.tick();
    });
  }

  function beginChatLoad(id, gen, cold){
    const token = ++chatLoadToken;
    clearChatLoad();
    if (cold) chatLoadTimer = setTimeoutFn(() => {
      if (token !== chatLoadToken || g("sel", "") !== id || g("selGen", 0) !== gen) return;
      if (msgwrap && msgwrap.classList) msgwrap.classList.add("loading");
      if (chatloading) chatloading.hidden = false;
    }, CHAT_LOAD_DELAY_MS);
    return () => {
      if (token !== chatLoadToken) return;
      clearChatLoad();
    };
  }

  function applyChatActivityChrome(live, mustShowPane, forcePeek, showPeek, replyReady, compacting, turnInFlight){
    setWorkPulse(workPulseShouldRun(live, mustShowPane, replyReady) || !!compacting);
    if (typeof d.setComposerBusy === "function")
      d.setComposerBusy(!!turnInFlight || !!compacting);
    if (convtools) convtools.hidden = false;
    if (termtoggle){
      termtoggle.innerHTML = icons.ICON_TERM || "";
      if (termtoggle.classList) termtoggle.classList.toggle("on", showPeek);
      termtoggle.disabled = forcePeek;
      if (typeof termtoggle.setAttribute === "function")
        termtoggle.setAttribute("aria-label", showPeek ? "hide terminal" : "show terminal");
    }
  }

  /* Paint the server-authoritative auto-approve toggle. Never optimistically
     claims enabled — only apply a view the server returned. */
  function paintAutoApprove(view, n){
    if (!autoapprove) return;
    const agent = (n && n.agent) || "";
    let v = view;
    if (v == null){
      v = String(agent).toLowerCase() === "claude"
        ? { supported: false, enabled: false, phase: "off", count: 0 }
        : { supported: true, enabled: false, phase: "off", count: 0 };
    }
    lastAutoView = v;
    const model = autoApproveChromeModel(v, { agent, node: n });
    autoapprove.hidden = !!model.hidden;
    autoapprove.disabled = !!model.disabled;
    if (typeof autoapprove.setAttribute === "function"){
      autoapprove.setAttribute("aria-pressed", model.pressed ? "true" : "false");
      autoapprove.setAttribute("aria-label", model.ariaLabel);
    }
    autoapprove.title = model.title;
    autoapprove.className = model.classNames.join(" ");
    if (autoapprove.classList && autoapprove.classList._s){
      autoapprove.classList._s = new Set(model.classNames);
    } else if (autoapprove.classList){
      ["on", "off", "unsupported", "has-error"].forEach(c => {
        if (typeof autoapprove.classList.toggle === "function")
          autoapprove.classList.toggle(c, model.classNames.includes(c));
      });
      if (typeof autoapprove.classList.add === "function")
        autoapprove.classList.add("autoapprove");
    }
    autoapprove.innerHTML = autoApproveButtonInnerHTML(model, icons, { escape });
  }

  function renderTurnHTML(t, { bk, hist = false, nodeId, assets, externalAllowed = false } = {}){
    const a = splitAssetRefs(t.text, nodeId, assets, tileDeps());
	const blocked = splitImportRefs(a.clean, nodeId, { ...tileDeps(), externalAllowed });
    const bookmarkMarker = bookmarkMarkerModel(t, nodeId, g("bookmarks", []), g("noteUsages", []));
    const sentTo = sentToMarkerModel(t, nodeId, g("forwardLinks", []));
    const marker = { ...(bookmarkMarker || {}), sentTo };
    const markers = (bookmarkMarker || sentTo.length) ? bubbleMarkersHTML(marker, {
      iconBookmark: icons.ICON_BUBBLE_BOOKMARK || "",
      iconInto: icons.ICON_INTO || "",
    }) : "";
    bubbleTurns[bk] = t;
    const cls = turnRoleClass(t.role, { hist, media: !!((a.html || blocked.html) && !blocked.clean) });
    const dataAttrs = hist
      ? `data-bk="${bk}" data-time="${escape(t.time || "")}" data-uid="${escape(t.uid || "")}" data-segment="${t.segment || 0}" data-record="${t.record || 0}"`
      : `data-i="${bk.startsWith("i:") ? bk.slice(2) : ""}" data-bk="${bk}"`;
    return `
      <div class="${cls}" ${dataAttrs}>
		<div class="bubble" title="${escape(titleFn(t.role, t.time))}">${markers}${renderMissingImportText(markdown(blocked.clean), blocked.missing)}${a.html}${blocked.html}</div>
      </div>`;
  }

  function confirmDeferredForward(nodeId, turns){
    const expected = readAwaitingForward(
      d.storage, nodeId, typeof d.now === "function" ? d.now() : Date.now());
    const turn = matchingForwardDestinationTurn(expected, turns);
    if (!turn) return;
    const pending = readPendingForwards(d.storage, nodeId);
    if (pending.length && typeof d.uiMutate === "function"){
      pending.forEach(source => d.uiMutate({
        k: "forward-link-add",
        link: makeForwardLinkToTurn(source, nodeId, turn),
      }));
      clearPendingForwards(d.storage, nodeId);
    }
    if (!pending.length || typeof d.uiMutate === "function")
      clearAwaitingForward(d.storage, nodeId);
  }

  function renderTimelineHTML(turns, decisions, { hist = false, segIndex = 0, nodeId, assets, externalAllowed = false } = {}){
    return mergeTimelineItems(turns, decisions).map(item => {
      if (item.kind === "decision"){
        return decisionRowHTML(item.decision, {
          escape, fmtTime: whenFn, ordinal: item.ordinal,
        });
      }
      const bk = hist ? histBk(segIndex, item.index) : liveBk(item.index);
      return renderTurnHTML(item.turn, { bk, hist, nodeId, assets, externalAllowed });
    }).join("");
  }

  function attentionSuppressed(id, node){
    if (!id) return false;
    if (attentionIsSuppressed(id, suppressAttentionUntil)) return true;
    delete suppressAttentionUntil[id];
    if (node && node.attention === "inspect" &&
        dismissedAttention[id] === attentionEvidenceKey(node)) return true;
    delete dismissedAttention[id];
    return false;
  }

  function collapseAttentionUI(id){
    suppressAttentionUntil[id] = suppressAttentionUntilTime();
    if (keyrow) keyrow.innerHTML = "";
    if (msgs) qa(msgs, ".peekblock, .pending").forEach(el => el.remove());
  }

  /* Apply a pinDecision to a scroller element. Returns the decision so callers
     can clear chatScrollBottom only on a verified "pin". */
  function applyScrollDecision(el, want){
    if (!el) return { action: "none", scrollTop: 0, pending: !!want };
    const decision = pinDecision({
      scrollTop: el.scrollTop || 0,
      scrollHeight: el.scrollHeight || 0,
      clientHeight: el.clientHeight || 0,
      want: !!want,
    });
    if (decision.action === "pin" || decision.action === "clamp")
      el.scrollTop = decision.scrollTop;
    return decision;
  }

  function pinChatBottom(el){
    return applyScrollDecision(el, true);
  }

  function clampChatScroll(el){
    return applyScrollDecision(el, false);
  }

  function ensurePinObserver(){
    const RO = d.ResizeObserver;
    if (!RO || !msgs || pinObserver) return;
    try {
      pinObserver = new RO(() => { reassertPin(); });
      pinObserver.observe(msgs);
    } catch {
      pinObserver = null;
    }
  }

  function schedulePinReassert(){
    ensurePinObserver();
    const raf = d.requestAnimationFrame;
    if (typeof raf !== "function" || pinRafQueued) return;
    pinRafQueued = true;
    raf(() => {
      pinRafQueued = false;
      reassertPin();
    });
  }

  function reassertPin(){
    if (!msgs) return;
    if (chatScrollBottom){
      const decision = pinChatBottom(msgs);
      if (decision.action === "pin") chatScrollBottom = false;
    } else {
      clampChatScroll(msgs);
    }
  }

  function clearPinObserver(){
    if (pinObserver && typeof pinObserver.disconnect === "function"){
      try { pinObserver.disconnect(); } catch { /* ignore */ }
    }
    pinObserver = null;
    pinRafQueued = false;
  }

  function invalidate(){ chatSig = ""; }

  function museSchemaWarnHost(){
    if (roots.muse_schema_warn) return roots.muse_schema_warn;
    if (doc && typeof doc.getElementById === "function")
      return doc.getElementById("muse_schema_warn");
    return null;
  }

  function syncMuseSchemaWarning(n){
    const host = museSchemaWarnHost();
    if (!host) return;
    if (!n || n.agent !== "muse" || n.muse_schema_warning !== MUSE_SCHEMA_MISMATCH_CODE){
      host.hidden = true;
      host.textContent = "";
      return;
    }
    host.textContent = MUSE_SCHEMA_MISMATCH_WARNING;
    host.hidden = false;
  }

  function onSelectChange(){
    chatSig = "";
    syncMuseSchemaWarning(nodeById(g("sel", "")));
    /* Drop the per-node chat ETag so a tag from node A is never sent for B. */
    chatETag = { node: "", etag: "" };
    lastAutoView = null;
    autoApproveInFlight = "";
    /* A new chat always opens at its newest bubble. #msgs is a singleton
       reused across nodes, so without this the rebuild inherits the scroll
       offset of the chat we just left: `atBottom` was measured on the old
       content, and a reader who had scrolled up (or a shorter/longer other
       chat) lands mid-history with the latest turns off-screen. */
    chatScrollBottom = true;
    termOpen = false;
    tappedTurn = "";
    chatHist = { node: "", segs: null, scrollTo: "", assets: {} };
    editingChatDesc = false;
    chatDetailSavedUntil = 0;
    permExpanded = { node: "", open: false };
  }

  function onReselect(){
    editingChatDesc = false;
    chatDetailSavedUntil = 0;
    chatSig = "";
  }

  function setSentEcho(echo){ sentEcho = echo; }
  function clearSentEchoFor(nodeId){
    if (sentEcho && sentEcho.node === nodeId){
      sentEcho = null;
      if (msgs) qa(msgs, ".turn.echo").forEach(x => x.remove());
    }
  }

  function jumpToNow(){
    chatHist.scrollTo = "";
    if (chatSig){
      const decision = pinChatBottom(msgs);
      /* No layout yet — keep the intent so a later re-assert can finish the job. */
      if (decision.pending) chatScrollBottom = true;
      if (decision.pending) schedulePinReassert();
    } else {
      chatScrollBottom = true;
    }
  }

  async function loadHistory(id, scrollTo, { preserveScroll = false } = {}){
    const savedScrollTop = preserveScroll && msgs ? msgs.scrollTop : null;
    try {
      const data = await api(`/api/nodes/${encodeURIComponent(id)}/chat?history=1`);
      if (g("sel", "") !== id) return;
      chatHist = { node: id, segs: data.segments || [], scrollTo: scrollTo || "", assets: data.assets || {} };
      chatScrollBottom = false;
      chatSig = "";
      await refreshChat();
      if (savedScrollTop != null && g("sel", "") === id && msgs){
        msgs.scrollTop = savedScrollTop;
        clampChatScroll(msgs);
      }
    } catch (err) {
      if (typeof d.alert === "function") d.alert(err.message);
    }
  }

  function renderChatHead(){
    if (!chathead) return;
    const toggle = infobtn;
    const details = chatdetails;
    if (chathead.classList){
      chathead.classList.toggle("details-open", chatDetailsOpen);
      chathead.classList.toggle("desc-editing", editingChatDesc);
    }
    if (details){
      details.hidden = !chatDetailsOpen;
      if (typeof details.setAttribute === "function")
        details.setAttribute("aria-hidden", details.hidden ? "true" : "false");
    }
    const n = nodeById(g("sel", ""));
    syncMuseSchemaWarning(n);
    const now = typeof d.now === "function" ? d.now() : Date.now();
    const saving = editingChatDesc || now < chatDetailSavedUntil;
    if (toggle) toggle.hidden = !n || (!chatDetailsOpen && !saving);
    if (n && g("editingTitle", "") === n.id && g("editingTitleScope", "") === "chat"){
      if (chattitle && !q(chattitle, `[data-title-input="${CSSRef.escape(n.id)}"]`))
        chattitle.innerHTML = `<input class="title-edit" data-title-input="${escape(n.id)}" value="${escape(n.title)}">`;
    } else if (chattitle) {
      chattitle.textContent = n ? n.title : "scimux";
    }
    if (chatmission) chatmission.textContent = "";
    if (typeof d.setBookmarksBackLabel === "function")
      d.setBookmarksBackLabel(n ? n.title : "Chat");
    if (chatdot){
      chatdot.className = "dot " + (n ? (hardAttention(n) ? "attention" : n.live) : "");
      chatdot.style.background = n && !hardAttention(n) && !["exited","unavailable"].includes(n.live)
        ? laneColor(n.lane_id) : "";
    }
    if (typeof d.setComposerClosed === "function")
      d.setComposerClosed(!!(n && n.ended_at));
    if (!n) return;
    const saved = now < chatDetailSavedUntil;
    if (toggle){
      toggle.innerHTML = editingChatDesc || saved
        ? (icons.ICON_CHECK || "")
        : (chatDetailsOpen ? (icons.ICON_CHEV_UP || "") : (icons.ICON_CHEV_DOWN || ""));
      if (toggle.classList) toggle.classList.toggle("saving", editingChatDesc || saved);
      if (typeof toggle.setAttribute === "function")
        toggle.setAttribute("aria-label",
          editingChatDesc ? "save description" : (chatDetailsOpen ? "hide details" : "show details"));
    }
    if (chatagentlogo){
      chatagentlogo.innerHTML = agentLogo(n.agent);
      chatagentlogo.title = n.agent || "agent";
    }
    const ctx = chatCtxPct.node === n.id && chatCtxPct.pct != null
      ? `context ${Math.round(chatCtxPct.pct)}%`
      : (n.agent === "muse" ? "context unknown" : "");
    if (chatmeta)
      chatmeta.textContent = [n.agent || "agent", n.model || "default", n.effort || "", ctx].filter(Boolean).join(" · ");
    const desc = n.description || n.prompt || "";
    /* Read state: same md() the bubbles and note references use — a description
       that is usually a multi-paragraph initial prompt must not render flat here
       and rich elsewhere. Editor stays raw (value), the standard rendered-read /
       plain-edit split. md() is escape-first; no extra sanitiser. */
    if (chatdesc) chatdesc.innerHTML = desc ? md(desc) : "No description yet.";
    const input = chatdescinput;
    if (input && (!editingChatDesc || input.dataset.node !== n.id)){
      input.value = desc;
      input.dataset.node = n.id;
    }
  }

  async function saveChatDescription(){
    const sel = g("sel", "");
    if (!sel || !nodeById(sel)) return;
    const id = sel;
    const description = (chatdescinput && chatdescinput.value || "").trim();
    try {
      const n = await api(`/api/nodes/${encodeURIComponent(id)}`, {
        method: "PATCH",
        body: JSON.stringify({ description }),
      });
      if (typeof d.updateLocalNode === "function") d.updateLocalNode(n);
      editingChatDesc = false;
      chatDetailsOpen = true;
      chatDetailSavedUntil = (typeof d.now === "function" ? d.now() : Date.now()) + CHAT_DETAIL_SAVED_MS;
      if (typeof d.invalidateCardsSig === "function") d.invalidateCardsSig();
      chatSig = "";
      if (typeof d.invalidateMapSig === "function") d.invalidateMapSig();
      if (typeof d.renderCards === "function") d.renderCards();
      renderChatHead();
      if (typeof d.renderMap === "function") d.renderMap();
      setTimeoutFn(() => {
        const now = typeof d.now === "function" ? d.now() : Date.now();
        if (now >= chatDetailSavedUntil){
          chatDetailSavedUntil = 0;
          renderChatHead();
        }
      }, CHAT_DETAIL_SAVED_MS);
    } catch (err) {
      if (typeof d.alert === "function") d.alert(err.message);
    }
  }

  function useTurnAsDescription(text){
    const n = nodeById(g("sel", ""));
    if (!n) return;
    chatDetailsOpen = true;
    editingChatDesc = true;
    if (chatdescinput){
      chatdescinput.value = text || "";
      chatdescinput.dataset.node = n.id;
    }
    chatSig = "";
    renderChatHead();
    setTimeoutFn(() => {
      if (!chatdescinput) return;
      focusAtEnd(chatdescinput);
    }, 0);
  }

  /* Whether this click was the end of a drag that selected text. A click-drag
     still fires `click` on mouseup, so without this the bubble tap toggles the
     action bar out from under someone who was only marking a passage to copy.
     A leftover selection cannot cause a false positive: pressing down collapses
     one, so anything still selected when the click arrives was selected by this
     very gesture. */
  function justSelectedText(){
    const sel = doc && typeof doc.getSelection === "function"
      ? doc.getSelection()
      : (win && typeof win.getSelection === "function" ? win.getSelection() : null);
    if (!sel || sel.isCollapsed) return false;
    const text = typeof sel.toString === "function" ? sel.toString() : "";
    return text.trim() !== "";
  }

  function renderBubbleActions(){
    if (!msgs) return;
    qa(msgs, ".bubactions").forEach(x => x.remove());
    if (!tappedTurn) return;
    const turnEl = q(msgs, `.turn[data-bk="${tappedTurn}"]`);
    const turn = bubbleTurns[tappedTurn];
    if (!turnEl || !turn){ tappedTurn = ""; return; }
    const row = (doc && doc.createElement) ? doc.createElement("div") : null;
    if (!row) return;
    row.className = "bubactions";
    row.innerHTML = bubbleActionsHTML(turn, {
      escape, fmtTime: bubTimeFn,
      iconBranch: icons.ICON_BRANCH || "",
      iconInto: icons.ICON_INTO || "",
      iconCopy: icons.ICON_COPY || "",
    });
    if (typeof turnEl.after === "function") turnEl.after(row);
    else if (turnEl.parentNode) turnEl.parentNode.insertBefore(row, turnEl.nextSibling);
  }

  function scrollAndFlash(target){
    if (!target) return;
    if (typeof target.scrollIntoView === "function")
      target.scrollIntoView({ block: "center" });
    if (target.classList){
      target.classList.add("jumpflash");
      setTimeoutFn(() => target.classList.remove("jumpflash"), 1600);
    }
  }

  function getPendingJump(){
    return typeof d.getPendingJump === "function" ? d.getPendingJump() : null;
  }
  function setPendingJump(v){
    if (typeof d.setPendingJump === "function") d.setPendingJump(v);
  }

  function consumePendingJump(el, turns, priorTurns){
    const pendingJump = getPendingJump();
    const sel = g("sel", "");
    if (!pendingJump || pendingJump.node !== sel) return false;
    const now = typeof d.now === "function" ? d.now() : Date.now();
    if (now - pendingJump.ts > PENDING_JUMP_TTL_MS){ setPendingJump(null); return false; }
    const idx = matchPendingJumpInTurns(pendingJump, turns);
    if (idx >= 0){
      setPendingJump(null);
      const target = q(el, `.turn[data-i="${idx}"]`);
      if (!target) return false;
      scrollAndFlash(target);
      return true;
    }
    if (priorTurns > 0 && (pendingJump.uid || pendingJump.turnTime)){
      const histReady = chatHist.node === sel && chatHist.segs;
      if (!histReady){
        if (!pendingJump.histLoading){
          pendingJump.histLoading = true;
          setPendingJump(pendingJump);
          loadHistory(sel, "");
        }
        return false;
      }
      const hists = qa(el, ".turn.hist").map(t => ({
        uid: t.dataset.uid,
        segment: t.dataset.segment,
        record: t.dataset.record,
        time: t.dataset.time,
        el: t,
      }));
      const h = matchPendingJumpInHist(pendingJump, hists);
      if (h){ setPendingJump(null); scrollAndFlash(h.el); return true; }
      setPendingJump(null);
      if (typeof d.toast === "function") d.toast("Couldn't find that message in this chat.");
      return false;
    }
    return false;
  }

  function paintEcho(dest){
    if (g("sel", "") !== dest || !sentEcho || !msgs) return;
    qa(msgs, ".turn.echo").forEach(x => x.remove());
    const div = doc && doc.createElement ? doc.createElement("div") : null;
    if (!div) return;
    const tiles = refTilesHTML(sentEcho.atts, sentEcho.node, tileDeps());
    div.className = "turn user echo" + (tiles && !sentEcho.text ? " media" : "");
    div.innerHTML = `<div class="bubble" title="you — delivering">${markdown(sentEcho.text)}${tiles}</div>`;
    const peek = q(msgs, ".peekblock");
    if (typeof msgs.insertBefore === "function")
      msgs.insertBefore(div, peek);
    else if (typeof msgs.appendChild === "function")
      msgs.appendChild(div);
    msgs.scrollTop = msgs.scrollHeight;
  }

  async function refreshChat(){
    const n = nodeById(g("sel", ""));
    if (!n){
      if (typeof d.setComposerBusy === "function") d.setComposerBusy(false);
      clearChatLoad();
      return;
    }
    renderChatHead();
    const gen = g("selGen", 0);
    const endChatLoad = beginChatLoad(n.id, gen, !chatSig);
    const chatPath = `/api/nodes/${encodeURIComponent(n.id)}/chat`;
    /* Use only this node's tag, and only while its local render remains valid.
       A local-state change can invalidate chatSig while the server's live
       segment stays unchanged (history expansion, terminal toggle, local
       attention state). A 304 has no body from which to rebuild that state. */
    const held = chatSig && chatETag.node === n.id ? chatETag.etag : "";
    let data;
    try {
      if (typeof d.apiConditionalGet === "function") {
        const result = await d.apiConditionalGet(chatPath, held);
        if (gen !== g("selGen", 0) || g("sel", "") !== n.id) return;
        /* 304: body unchanged — balance the load token and skip every render
           seam (turns, signature, chrome). Composer busy stays as last 200 set
           it; the payload that drove it has not changed. */
        if (result.status === 304) {
          endChatLoad();
          return;
        }
        if (!result.data) {
          endChatLoad();
          return;
        }
        chatETag = { node: n.id, etag: result.etag || "" };
        data = result.data;
      } else {
        data = await api(chatPath);
      }
    } catch { endChatLoad(); return; }
    if (gen !== g("selGen", 0) || g("sel", "") !== n.id) return;

    let chromeChanged = false;
    if (data.live && n.live !== data.live){
      n.live = data.live;
      chromeChanged = true;
    }
    if ((data.attention || "") !== (n.attention || "")){
      n.attention = data.attention || "";
      chromeChanged = true;
    }
    if (chromeChanged){
      if (typeof d.invalidateCardsSig === "function") d.invalidateCardsSig();
      if (typeof d.invalidateMapSig === "function") d.invalidateMapSig();
      if (typeof d.renderCards === "function") d.renderCards();
      renderChatHead();
      if (typeof d.renderMap === "function") d.renderMap();
    }

    const turns = data.turns || [];
    confirmDeferredForward(n.id, turns);
    if (data.restore_draft) clearAwaitingForward(d.storage, n.id);
    if (typeof d.setAttachAvail === "function") d.setAttachAvail(turns.length > 0);

    if (data.restore_draft && n.id && !restoredDraft[n.id]){
      restoredDraft[n.id] = true;
      if (sentEcho && sentEcho.node === n.id) sentEcho = null;
      const stored = (d.storage && typeof d.storage.getItem === "function")
        ? (d.storage.getItem("scimux-draft:" + n.id) || "") : "";
      const live = (typeof d.composerText === "function" && g("sel", "") === n.id)
        ? (d.composerText() || stored) : stored;
      const decision = restoreDraftDecision({ original: data.restore_draft, current: live });
      if (decision.action === "restore"){
        if (typeof d.restoreDraft === "function") d.restoreDraft(n.id, decision.text);
        else if (d.storage && typeof d.storage.setItem === "function"){
          d.storage.setItem("scimux-draft:" + n.id, decision.text);
        }
      }
    }
    if (!sentEcho && data.pending_prompt && n.id && !data.restore_draft){
      sentEcho = { node: n.id, text: data.pending_prompt, atts: [], at: Date.now(), seen: null };
    }
    if (sentEcho && sentEcho.node === n.id){
      const now = typeof d.now === "function" ? d.now() : Date.now();
      const r = retireSentEcho(sentEcho, turns, now);
      sentEcho = r.echo;
    }
    const echo = sentEcho && sentEcho.node === n.id ? sentEcho : null;
    const assets = Object.assign({}, chatHist.assets || {}, data.assets || {});
    if (!n.attention){
      delete suppressAttentionUntil[n.id];
      delete dismissedAttention[n.id];
    }
    const attentionHidden = !!n.attention && attentionSuppressed(n.id, n);
    const policy = chatActivityPolicy({
      attention: n.attention,
      attentionHidden,
      fallback: data.fallback,
      delivery: data.delivery,
      turnsLength: turns.length,
      source: data.source,
      externalAttachments: !!data.allow_external_attachments,
      priorTurns: data.prior_turns || 0,
      termOpen,
      fresh: !!data.fresh,
      supervision: data.supervision || n.supervision || "",
      autoApproveArmed: !!(data.auto_approve && data.auto_approve.phase === "armed"),
      permDialogId: data.perm_dialog_id || "",
    });
    const { unconfirmed, delivering, mustShowPane, freshSurface, forcePeek, showPeek } = policy;

    let peekText = "";
    if (showPeek){
      try {
        peekText = await api(`/api/nodes/${encodeURIComponent(n.id)}/peek${mustShowPane ? "?mode=visible" : ""}`);
      } catch { /* ignore */ }
      if (gen !== g("selGen", 0) || g("sel", "") !== n.id) return;
    }

    const windowKnown = Number(data.ctx_window) > 0;
    if (gauge){
      if (windowKnown){
        gauge.hidden = false;
        if (gaugefill){
          gaugefill.style.width = Math.min(100, data.ctx_pct || 0) + "%";
          gaugefill.className = (data.ctx_pct || 0) >= 80 ? "high" : "";
        }
      } else {
        gauge.hidden = true;
      }
    }
    const pct = windowKnown ? Math.min(100, data.ctx_pct || 0) : null;
    if (chatCtxPct.node !== n.id || chatCtxPct.pct !== pct){
      chatCtxPct = { node: n.id, pct };
      renderChatHead();
    }
    // A successful /clear is ready for input immediately even if a tmux pane
    // redraw keeps mechanical liveness active for its debounce window.
    applyChatActivityChrome(
      data.live, mustShowPane, forcePeek, showPeek,
      !!data.reply_ready || freshSurface,
      !!data.compacting,
      !!data.turn_in_flight,
    );
    /* Auto-approve chrome is outside the transcript signature so count/phase
       can repaint without clobbering the composer or rebuilding bubbles. */
    paintAutoApprove(data.auto_approve, n);

    const hist = chatHist.node === n.id && chatHist.segs ? chatHist.segs : null;
    const priorSegs = priorSegsFromHistory(hist, data.chat_started);
    const liveDecisions = data.decisions || [];
    /* Leave expand when this node no longer has a live acp approval row. */
    const acpOpts = (data.source === "acp" && (data.perm_options || []).length)
      ? data.perm_options : null;
    if (permExpanded.node === n.id &&
        (!n.attention || attentionHidden || !acpOpts)){
      permExpanded = { node: "", open: false };
    }
    const expanded = permExpanded.node === n.id && permExpanded.open;
    const sig = buildChatSignature({
      nodeId: n.id,
      attention: n.attention,
      attentionHidden,
      delivery: data.delivery,
      showPeek, termOpen, termFull,
      source: data.source,
      externalAttachments: !!data.allow_external_attachments,
      error: data.error || "",
      permTitle: data.perm_title,
      permReason: data.perm_reason || "",
      permOptionsKey: permOptionsKey(data.perm_options),
      permRequestId: data.perm_request_id || "",
      permDialogId: data.perm_dialog_id || "",
      permManual: !!data.perm_manual,
      priorTurns: data.prior_turns || 0,
      chatStarted: data.chat_started || "",
      echoHash: echo ? hash(echo.text) : "",
      histKey: hist ? "h" + priorSegs.length : "",
      turnsHash: hashTurns(turns),
      turnAttrHash: hashTurnAttrs(turns, hash),
      markerHash: String(g("markerVersion", "")),
      decisionsHash: decisionsHash(liveDecisions) +
        (hist ? "|" + priorSegs.map(s => decisionsHash(s.decisions)).join(";") : ""),
      expanded,
      compacting: !!data.compacting,
      elicitationKey: elicitationKey(data),
    });
    const label = unconfirmed
      ? "Send unconfirmed \u2014 check the terminal"
      : (DEC_LABELS[data.reason] || "Needs your attention");
    const paneState = { showPeek, peekText, forcePeek, label, termFull, unconfirmed };
    ensurePinObserver();
    if (chatRenderDecision(sig, chatSig) === "skip"){
      /* The transcript stands; only the pane may have moved. Measure atBottom
         before the peek write — a growing pane changes scrollHeight and used
         to push a bottom-reader off with no re-pin (0eee686 regression). */
      const el = msgs;
      const wasAtBottom = !!(el && nearBottom(el.scrollHeight, el.scrollTop, el.clientHeight));
      renderPeekHost(n, paneState);
      if (el){
        if (chatScrollBottom){
          const decision = pinChatBottom(el);
          if (decision.action === "pin") chatScrollBottom = false;
          else if (decision.pending) schedulePinReassert();
        } else if (wasAtBottom){
          pinChatBottom(el);
        } else {
          clampChatScroll(el);
        }
      }
      endChatLoad();
      return;
    }
    chatSig = sig;
    lastTurns = turns;
    bubbleTurns = {};

    const el = msgs;
    if (!el){ endChatLoad(); return; }
    const atBottom = nearBottom(el.scrollHeight, el.scrollTop, el.clientHeight);

    clearRenderedListeners();
    /* A jump-to-source aims at one bubble and outranks the open-at-newest pin.
       Resolved before the write so we neither zero the offset nor bottom-pin
       when a pendingJump is aiming at this node. */
    const jumpAimed = !!(getPendingJump() && getPendingJump().node === g("sel", ""));
    /* Zero any inherited offset before the write on a node switch so deferred
       momentum / a stale large scrollTop from the chat we left cannot land
       outside the new content's range. */
    if (chatScrollBottom && !jumpAimed) el.scrollTop = 0;
    el.innerHTML =
      (data.prior_turns > 0 && !hist ? histLoadHTML(data.prior_turns) : "") +
      priorSegs.map((s, si) =>
        `<div class="chatseam histseam" data-seam="${escape(s.seam || "")}"><span>${s.reason && s.reason !== "clear" ? "history from" : "chat started"} ${escape(whenFn(s.start))}</span></div>` +
        renderTimelineHTML(s.turns || [], s.decisions || [], {
          hist: true, segIndex: si, nodeId: n.id, assets,
          externalAllowed: !!data.allow_external_attachments,
        })
      ).join("") +
      (data.chat_started
        ? `<div class="chatseam curseam"><span>chat started ${escape(whenFn(data.chat_started))}</span></div>` : "") +
      (!turns.length && !liveDecisions.length
          ? pendingEmptyHTML({
            freshSurface, pending: data.pending, delivering,
            supervision: data.supervision || n.supervision || "",
            ended: !!n.ended_at,
          })
        : "") +
      renderTimelineHTML(turns, liveDecisions, {
        hist: false, nodeId: n.id, assets,
        externalAllowed: !!data.allow_external_attachments,
      }) +
      /* A transport error describes the just-finished turn. Keep it at the
         current end of the conversation where a bottom-pinned reader sees it,
         rather than above an arbitrarily long timeline. */
      (data.error
        ? `<div class="pending ${data.reason === "claude_unsupported" ? "chatnotice" : "chaterr"}" role="status">${escape(data.error)}</div>`
        : "") +
      (echo ? echoBubbleHTML(echo.text, "", { markdown }) : "") +
      (data.compacting ? compactingStatusHTML() : "") +
      elicitationStatusHTML(data, { escape });

    /* The pane's own region. Created rather than written into the markup so the
       feature can hold a live reference: #msgs is rewritten wholesale, and a
       string-built host would have to be re-found after every rebuild. */
    peekHost = doc && doc.createElement ? doc.createElement("div") : null;
    if (peekHost){
      peekHost.id = "peekhost";
      el.appendChild(peekHost);
    }
    /* The fresh host is empty and its listeners died with the old one, so the
       pane must be repainted unconditionally after a rebuild. */
    peekSig = "";
    renderPeekHost(n, paneState);

    renderBubbleActions();

    let histScrolled = false;
    if (hist && chatHist.scrollTo){
      const t = chatHist.scrollTo;
      let target = null;
      if (t !== "seam"){
        const seams = qa(el, ".histseam").map(s2 => ({ seam: s2.dataset.seam, el: s2 }));
        const i = histScrollTargetIndex(seams, t);
        target = i >= 0 ? seams[i].el : null;
      }
      if (!target) target = q(el, ".curseam") || q(el, ".histseam");
      if (target){
        if (typeof target.scrollIntoView === "function")
          target.scrollIntoView({ block: t === "seam" ? "end" : "start" });
        histScrolled = true;
      }
      chatHist.scrollTo = "";
    }
    /* jumpAimed was computed before the write (see above) */
    if (chatScrollBottom && !jumpAimed){
      const decision = pinChatBottom(el);
      if (decision.action === "pin"){
        chatScrollBottom = false;
        histScrolled = true;
      } else if (decision.pending){
        schedulePinReassert();
      }
    }
    if (!histScrolled && !consumePendingJump(el, turns, data.prior_turns || 0) && (atBottom || forcePeek))
      pinChatBottom(el);
    /* Unconditional clamp backstop after every rebuild — overscroll from a
       shrink or a stale offset never leaves a blank viewport. */
    clampChatScroll(el);
    const onRendered = (node, event, handler) => {
      if (!node || typeof node.addEventListener !== "function") return;
      node.addEventListener(event, handler);
      renderedCleanups.push(() => node.removeEventListener(event, handler));
    };
    if (keyrow){
      /* Preserve .permask scrollTop across poll rebuilds (expanded path only).
         Same spirit as withCardEditsPreserved: snapshot before innerHTML write,
         restore after if still the same node and still expanded. Clamped mode
         has overflow:hidden — no meaningful scroll. */
      let permScroll = null;
      const prevMask = q(keyrow, ".permask");
      if (prevMask && expanded){
        permScroll = {
          nodeId: n.id,
          scrollTop: prevMask.scrollTop || 0,
          expanded: true,
        };
      }
      keyrow.innerHTML = keyRowHTML({
        attention: n.attention,
        attentionHidden,
        source: data.source,
        permTitle: data.perm_title,
        permOptions: data.perm_options,
        permToolKind: data.perm_tool_kind || "",
        permReason: data.perm_reason || "",
        permRequestId: data.perm_request_id || "",
        permDialogId: data.perm_dialog_id || "",
        permManual: !!data.perm_manual,
        elicitationWaiting: !!data.elicitation_waiting,
        expanded,
        axScreenReader: !!n.ax_screen_reader,
        escape,
      });
      if (permScroll && permScroll.nodeId === n.id && expanded){
        const nextMask = q(keyrow, ".permask");
        if (nextMask){
          const max = Math.max(0, (nextMask.scrollHeight || 0) - (nextMask.clientHeight || 0));
          nextMask.scrollTop = Math.min(permScroll.scrollTop, max || permScroll.scrollTop);
        }
      }
      /* One-element measure pass: unhide .permmore only when .permask overflows
         its clamp vertically. Runs here only — not on the skip path, not a
         second render. Same pattern as bookmarkClampState + applyRefClamps. */
      revealPermMoreIfNeeded();
    }
    endChatLoad();
  }

  function revealPermMoreIfNeeded(){
    clearPermMoreObserver();
    if (!keyrow) return;
    const mask = q(keyrow, ".permask");
    const more = q(keyrow, ".permmore");
    if (!mask || !more) return;
    const decision = permMoreDecision(mask.scrollHeight, mask.clientHeight);
    if (decision === PERM_MORE_UNKNOWN){
      /* No box yet (the chat is hidden under the wall map). The keyrow is
         written once and the skip path never rewrites it, so an observer is
         the only thing left that can decide. Without one, fail open: a dead
         control is a nuisance, an unreadable approval is a dead end. */
      if (!armPermMoreObserver(mask)) more.hidden = false;
      return;
    }
    more.hidden = decision !== PERM_MORE_SHOW;
  }

  function armPermMoreObserver(mask){
    const RO = d.ResizeObserver;
    if (typeof RO !== "function") return false;
    try {
      const obs = new RO(() => {
        /* Still no box: keep waiting. Re-entering while unmeasurable would
           disconnect and re-observe, and observe() always delivers an initial
           callback — that is a spin, not a retry. */
        if (!(mask.clientHeight > 0)) return;
        revealPermMoreIfNeeded();
      });
      obs.observe(mask);
      permMoreObserver = obs;
      return true;
    } catch {
      permMoreObserver = null;
      return false;
    }
  }

  function clearPermMoreObserver(){
    if (permMoreObserver && typeof permMoreObserver.disconnect === "function"){
      try { permMoreObserver.disconnect(); } catch { /* ignore */ }
    }
    permMoreObserver = null;
  }

  /* ---- event handlers ---- */

  function onInfoClick(){
    if (editingChatDesc){ saveChatDescription(); return; }
    chatDetailsOpen = !chatDetailsOpen;
    renderChatHead();
  }

  function onHeadClick(e){
    if (!nodeById(g("sel", "")) || editingChatDesc) return;
    if (e.target.closest("button, input, textarea, a, #chatdetails")) return;
    if (!e.target.closest(".toprow, #chatmission")) return;
    chatDetailsOpen = !chatDetailsOpen;
    renderChatHead();
  }

  function onMsgsScroll(){
    if (!chathead || !msgs || !chathead.classList) return;
    chathead.classList.toggle("compact", msgs.scrollTop > 30);
  }

  function onMsgsLoad(e){
    const img = e.target;
    if (!img || img.tagName !== "IMG") return;
    const a = img.closest && img.closest(".attthumb");
    if (a && a.dataset && a.dataset.tkey && a.offsetHeight){
      tileBox[a.dataset.tkey] = { w: a.offsetWidth, h: a.offsetHeight };
    }
    if (!msgs) return;
    if (nearBottom(msgs.scrollHeight, msgs.scrollTop, msgs.clientHeight))
      msgs.scrollTop = msgs.scrollHeight;
  }

  function onMsgsError(e){
    const img = e.target;
    if (!img || img.tagName !== "IMG") return;
    const a = img.closest && img.closest(".attthumb");
    if (a) a.remove();
  }

  function onKeyrowClick(e){
    const sel = g("sel", "");
    /* "show all" expands the ask in place — no key send, no attention collapse. */
    const more = e.target.closest && e.target.closest(".permmore");
    if (more){
      if (!sel) return;
      permExpanded = { node: sel, open: true };
      chatSig = "";
      refreshChat();
      return;
    }
    const openTerm = e.target.closest && e.target.closest("[data-open-terminal]");
    if (openTerm){
      if (!termOpen) onTermToggle();
      return;
    }
    const dismiss = e.target.closest && e.target.closest("[data-dismiss-attention]");
    if (dismiss){
      const n = nodeById(sel);
      if (!sel || !n || n.attention !== "inspect") return;
      dismissedAttention[sel] = attentionEvidenceKey(n);
      termOpen = false;
      chatSig = "";
      peekSig = "";
      if (keyrow) keyrow.innerHTML = "";
      if (peekHost) peekHost.innerHTML = "";
      refreshChat();
      return;
    }
    const b = e.target.closest && e.target.closest("[data-key]");
    if (!b || !sel) return;
    /* Capture node, request id, and key from the rendered row BEFORE collapsing
       or refreshing the UI. Never re-read a newer global request id after the
       async action begins (P1: bind decision to the exact pending request). */
    const dest = sel;
    const key = b.dataset.key;
    const requestId = b.dataset.requestId || "";
    const dialogId = b.dataset.dialogId || "";
    collapseAttentionUI(dest);
    (async () => {
      try {
        const body = { key };
        if (requestId) body.request_id = requestId;
        if (dialogId) body.dialog_id = dialogId;
        await api(`/api/nodes/${encodeURIComponent(dest)}/key`,
          { method: "POST", body: JSON.stringify(body) });
        chatSig = "";
        if (typeof d.scheduleTick === "function") d.scheduleTick(400);
        else if (typeof d.tick === "function") setTimeoutFn(() => d.tick(), 400);
      } catch (err) {
        delete suppressAttentionUntil[dest];
        chatSig = "";
        refreshChat();
        if (typeof d.alert === "function") d.alert(err.message);
      }
    })();
  }

  function onMsgsClick(e){
    const preview = e.target.closest && e.target.closest("[data-asset-preview]");
    if (preview){
      e.preventDefault?.();
      if (typeof d.openAssetPreview === "function")
        d.openAssetPreview({ url: preview.href, name: preview.dataset?.name || preview.title || "image" });
      return;
    }
    const settings = e.target.closest && e.target.closest("[data-asset-settings]");
    if (settings){
      if (typeof d.openAttachmentSettings === "function") d.openAttachmentSettings();
      return;
    }
    const retry = e.target.closest && e.target.closest("[data-asset-retry]");
    if (retry){
      retry.disabled = true;
      retry.textContent = "Importing…";
      const node = retry.dataset.node || g("sel", "");
      (async () => {
        try {
          const result = await api(`/api/nodes/${encodeURIComponent(node)}/asset-imports/retry`, {
            method: "POST", body: JSON.stringify({
              turn_record: Number(retry.dataset.turn), occurrence: Number(retry.dataset.occurrence),
            }),
          });
          retry.textContent = result.status === "already_imported" ? "Already imported" : "Imported on retry";
          /* A retry can target a bubble in an on-demand historical segment.
             The ordinary chat poll only projects the current segment, so it
             cannot replace that segment's persisted blocked marker or stale
             asset map. Re-read loaded history while this same chat remains
             selected, and restore the reader's exact scroll position after
             the rebuild. A selection change during the POST owns the screen
             and must not be repainted by this stale completion. */
          if (g("sel", "") === node && chatHist.node === node && chatHist.segs){
            await loadHistory(node, "", { preserveScroll: true });
          } else if (g("sel", "") === node){
            chatSig = "";
            await refreshChat();
          }
        } catch (err) {
          if (readRetryFailure(err) === "not_found") {
            retry.remove();
            /* The file disappeared between render and click. Drop the stale
               action immediately, then rebuild whichever view is still selected. */
            if (g("sel", "") === node && chatHist.node === node && chatHist.segs){
              await loadHistory(node, "", { preserveScroll: true });
            } else if (g("sel", "") === node){
              chatSig = "";
              await refreshChat();
            }
            return;
          }
          retry.disabled = false;
          retry.textContent = "Retry import";
          if (typeof d.alert === "function") d.alert(err.message || String(err));
        }
      })();
      return;
    }
    const openTerm = e.target.closest && e.target.closest("[data-open-terminal]");
    if (openTerm){
      if (!termOpen) onTermToggle();
      return;
    }
    const hl = e.target.closest && e.target.closest(".histload");
    if (hl){
      const sel = g("sel", "");
      if (sel) loadHistory(sel, "seam");
      return;
    }
    const marker = e.target.closest && e.target.closest("[data-bmarker]");
    if (marker){
      const turnEl = marker.closest && marker.closest(".turn");
      const turn = turnEl && turnEl.dataset ? bubbleTurns[turnEl.dataset.bk] : null;
      const bookmarkModel = turn && bookmarkMarkerModel(
        turn, g("sel", ""), g("bookmarks", []), g("noteUsages", []));
      const sentTo = turn ? sentToMarkerModel(turn, g("sel", ""), g("forwardLinks", [])) : [];
      const model = { ...(bookmarkModel || {}), sentTo };
      if (marker.dataset.bmarker === "bookmark"){
        if (!bookmarkModel) return;
        if (!model.destinations.length){
          if (typeof d.openBookmark === "function") d.openBookmark(model.bookmark.t);
        } else if (model.destinations.length === 1){
          if (typeof d.openNoteUsage === "function") d.openNoteUsage(model.destinations[0]);
        } else if (typeof d.openChoiceList === "function"){
          d.openChoiceList({
            title: "Used in…",
            choices: model.destinations.map((u, i) => ({
              id: String(i), label: `${u.note_title || "Note"} › ${u.section_title || "Section"}`,
              usage: u,
            })),
            onChoose: choice => d.openNoteUsage(choice.usage),
          });
        }
      } else if (marker.dataset.bmarker === "sendto" && sentTo.length){
        const openLink = link => {
          if (typeof d.jumpToChatAddress !== "function") return;
          if (d.jumpToChatAddress(link.destination || {}) === false && typeof d.toast === "function")
            d.toast("This destination chat is no longer available.");
        };
        if (sentTo.length === 1) openLink(sentTo[0]);
        else if (typeof d.openChoiceList === "function"){
          d.openChoiceList({
            title: "Sent to…",
            choices: sentTo.map((link, i) => {
              const dest = (link && link.destination) || {};
              const node = nodeById(dest.node);
              return {
                id: String(i), link,
                label: `${(node && node.title) || "Chat"}${link.sent_at ? " · " + bubTimeFn(link.sent_at) : ""}`,
              };
            }),
            onChoose: choice => openLink(choice.link),
          });
        }
      }
      return;
    }
    const ba = e.target.closest && e.target.closest("[data-bact]");
    if (ba){
      const turn = bubbleTurns[tappedTurn];
      if (ba.dataset.bact === "copy"){
        if (turn && typeof d.copyText === "function") d.copyText(turn.text || "");
        ba.innerHTML = "&#10003; copied";
        setTimeoutFn(() => renderBubbleActions(), 900);
        return;
      }
      if (ba.dataset.bact === "bookmark"){
        if (!turn) return;
        const sel = g("sel", "");
        const bookmarks = g("bookmarks", []) || [];
        const dup = bookmarks.some(nt =>
          nt.node === sel && nt.turnTime === (turn.time || "") && nt.text === (turn.text || ""));
        if (!dup){
          const node = nodeById(sel);
          const bookmark = {
            t: new Date().toISOString(), text: turn.text || "",
            node: sel, lane: node?.lane_id || "", turnTime: turn.time || "",
          };
          /* Whose turn this was, recorded now because nothing downstream can
             recover it: the bookmark outlives the segment it was taken from.
             It is what lets send-to disclose an Output as AI-generated
             (bookmarks.js discloseAgentOutput) and what fills the speaker in a
             note's frozen snapshot. Written only when the transcript said so —
             a guessed role is worse than none. */
          if (turn.role === "user" || turn.role === "assistant")
            bookmark.role = turn.role;
          if (turn.agent) bookmark.agent = turn.agent;
          if (Object.prototype.hasOwnProperty.call(turn, "prov") && turn.prov !== undefined){
            const cloned = cloneJSONValue(turn.prov);
            if (cloned !== undefined) bookmark.prov = cloned;
          }
          if (typeof d.stampAddress === "function") d.stampAddress(bookmark, turn);
          const commit = captured => {
            if (g("sel", "") !== sel) return;
            const exists = (g("bookmarks", []) || []).some(nt =>
              captured.uid && nt.uid === captured.uid && nt.segment === captured.segment && nt.record === captured.record);
            if (exists) return;
            if (typeof d.uiMutate === "function") d.uiMutate({ k: "bookmark-add", bookmark: captured });
            chatSig = "";
            ba.innerHTML = "&#10003; noted";
            setTimeoutFn(() => { tappedTurn = ""; renderBubbleActions(); }, 700);
          };
          let captured = bookmark;
          try {
            if (typeof d.captureBookmark === "function") captured = d.captureBookmark(bookmark, turn);
          } catch (_err) {
            if (typeof d.toast === "function") d.toast("Bookmark capture failed — try again.");
            return;
          }
          if (captured && typeof captured.then === "function") {
            ba.textContent = "capturing…";
            captured.then(commit, () => {
              if (g("sel", "") === sel && typeof d.toast === "function") d.toast("Bookmark capture failed — try again.");
            });
            return;
          }
          commit(captured);
          return;
        }
        ba.innerHTML = "&#10003; noted";
        setTimeoutFn(() => { tappedTurn = ""; renderBubbleActions(); }, 700);
        return;
      }
      tappedTurn = "";
      renderBubbleActions();
      if (!turn) return;
      if (ba.dataset.bact === "fork" && typeof d.forkFromTurn === "function")
        d.forkFromTurn(stripAssetRefs(turn.text));
      else if (ba.dataset.bact === "sendto" && typeof d.openSendTo === "function")
        /* strip for the same reason fork does: scimux-asset: markers are
           node-scoped and would land as dead markdown in the target */
        d.openSendTo({
          text: stripAssetRefs(turn.text),
          exceptId: g("sel", ""),
          title: "Send to chat…",
          role: turn.role || "",
          source: {
            node: g("sel", ""), turnTime: turn.time || "",
            ...(turn.uid ? {
              uid: turn.uid, segment: Number(turn.segment) || 0, record: Number(turn.record) || 0,
            } : {}),
          },
        });
      else if (ba.dataset.bact === "desc")
        useTurnAsDescription(turn.text);
      return;
    }
    const turnEl = e.target.closest && e.target.closest(".turn");
    if (turnEl && turnEl.dataset && turnEl.dataset.bk &&
        !(e.target.closest && e.target.closest("a, button"))){
      if (justSelectedText()) return;
      const k = turnEl.dataset.bk;
      tappedTurn = tappedTurn === k ? "" : k;
      renderBubbleActions();
      if (tappedTurn){
        const row = q(msgs, ".bubactions");
        if (row && typeof row.scrollIntoView === "function")
          row.scrollIntoView({ block: "nearest", behavior: "smooth" });
      }
    }
  }

  function onMsgsTouchEnd(e){
    if (e.target !== e.currentTarget) return;
    const now = typeof d.now === "function" ? d.now() : Date.now();
    const scale = (win && win.visualViewport && win.visualViewport.scale) || 1;
    if (now - lastBgTap < 350 && scale > 1.01){
      const vp = doc && doc.querySelector ? doc.querySelector('meta[name="viewport"]') : null;
      if (vp){
        const orig = vp.content;
        vp.content = orig + ", maximum-scale=1";
        setTimeoutFn(() => { vp.content = orig; }, 120);
      }
      lastBgTap = 0;
      return;
    }
    lastBgTap = now;
  }

  function onTermToggle(){
    if (termtoggle && termtoggle.disabled) return;
    termOpen = !termOpen;
    chatSig = "";
    refreshChat();
    if (termOpen && msgs) msgs.scrollTop = 1e6;
  }

  /* Toggle auto-approve via POST. Capture node id before the request so a
     response for A never repaints B. No optimistic enable. */
  async function onAutoApproveClick(){
    if (!autoapprove || autoapprove.disabled) return;
    const id = g("sel", "");
    const n = nodeById(id);
    if (!id || !n) return;
    const pressed = typeof autoapprove.getAttribute === "function"
      ? autoapprove.getAttribute("aria-pressed") === "true"
      : false;
    const enable = !pressed;
    autoApproveInFlight = id;
    try {
      const view = await api(`/api/nodes/${encodeURIComponent(id)}/auto-approve`, {
        method: "POST",
        body: JSON.stringify({ enabled: enable }),
      });
      if (autoApproveInFlight !== id || g("sel", "") !== id) return;
      // Invalidate only this node's chat validator; never borrow another's ETag.
      if (chatETag.node === id) chatETag = { node: "", etag: "" };
      paintAutoApprove(view, n);
      chatSig = "";
      await refreshChat();
    } catch (err){
      if (autoApproveInFlight !== id || g("sel", "") !== id) return;
      // Retain last authoritative server state; surface a concise error.
      const msg = (err && err.message) ? String(err.message) : "auto-approve failed";
      const base = lastAutoView || { supported: true, enabled: false, phase: "off", count: 0 };
      paintAutoApprove({ ...base, error: msg.replace(/\s+/g, " ").trim().slice(0, 160) }, n);
    } finally {
      if (autoApproveInFlight === id) autoApproveInFlight = "";
    }
  }

  function onScrollEnd(){
    if (!msgs) return;
    if (typeof msgs.scrollTo === "function")
      msgs.scrollTo({ top: msgs.scrollHeight, behavior: "smooth" });
    else msgs.scrollTop = msgs.scrollHeight;
  }

  function onTitleLongpress(){
    if (typeof d.startTitleEdit === "function")
      d.startTitleEdit(g("sel", ""), "chat");
  }

  function onDescLongpress(){
    if (!g("sel", "") || !nodeById(g("sel", ""))) return;
    chatDetailsOpen = true;
    editingChatDesc = true;
    chatSig = "";
    renderChatHead();
    setTimeoutFn(() => {
      if (!chatdescinput) return;
      focusAtEnd(chatdescinput);
    }, 0);
  }

  function cancelDescEdit(){
    editingChatDesc = false;
    chatSig = "";
    renderChatHead();
  }

  function cancelEditorsForNav(){
    editingChatDesc = false;
    chatSig = "";
  }

  function bind(){
    if (bound) return;
    bound = true;
    const on = (el, event, handler, options) => {
      if (!el || typeof el.addEventListener !== "function") return;
      el.addEventListener(event, handler, options);
      cleanups.push(() => el.removeEventListener(event, handler, options));
    };
    on(infobtn, "click", onInfoClick);
    on(chathead, "click", onHeadClick);
    on(msgs, "scroll", onMsgsScroll);
    on(msgs, "load", onMsgsLoad, true);
    on(msgs, "error", onMsgsError, true);
    on(msgs, "click", onMsgsClick);
    on(msgs, "touchend", onMsgsTouchEnd, { passive: true });
    on(keyrow, "click", onKeyrowClick);
    on(termtoggle, "click", onTermToggle);
    on(autoapprove, "click", onAutoApproveClick);
    if (scrollend){
      scrollend.innerHTML = icons.ICON_DOWNALL || "";
      on(scrollend, "click", onScrollEnd);
    }
    if (typeof d.longpress === "function" && chathead){
      const c1 = d.longpress(chathead, "#chattitle", onTitleLongpress);
      if (typeof c1 === "function") cleanups.push(c1);
      const c2 = d.longpress(chathead, "#chatdesc", onDescLongpress);
      if (typeof c2 === "function") cleanups.push(c2);
    }
  }

  function destroy(){
    while (cleanups.length){
      try { cleanups.pop()(); } catch { /* ignore */ }
    }
    clearRenderedListeners();
    clearChatLoad();
    clearPinObserver();
    clearPermMoreObserver();
    bound = false;
  }

  return {
    render: refreshChat,
    renderHead: renderChatHead,
    loadHistory,
    jumpToNow,
    paintEcho,
    setSentEcho,
    clearSentEchoFor,
    invalidate,
    onSelectChange,
    onReselect,
    restartWorkPulse,
    cancelDescEdit,
    cancelEditorsForNav,
    bind,
    destroy,
    /* accessors required by shell send / navigation */
    getChatSig: () => chatSig,
    getLastTurns: () => lastTurns,
    isEditingDesc: () => editingChatDesc,
  };
}
