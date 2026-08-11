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
 *   - #convtools / #termtoggle / #scrollend
 *   - #workpulse
 *
 * Inputs (getters / injected deps — never implicit app globals):
 *   - nodes, selected id (sel), selGen
 *   - editingTitle / editingTitleScope (shared shell title editor)
 *   - hardAttention, laneColor, agentLogo, icons
 *   - pendingJump get/set (shell jump coordinator for notes/search)
 *   - bookmarks / uiMutate (bubble bookmark action only)
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
 *   - PATCH /api/nodes/:id  (description only, from details editor)
 *
 * Events owned after one idempotent bind():
 *   - #infobtn click (details toggle / save description)
 *   - #chathead click (header-row details toggle)
 *   - #msgs scroll (compact header)
 *   - #msgs load capture (thumbnail box + re-pin near bottom)
 *   - #msgs click (histload, bubble tap, bubble actions)
 *   - #msgs touchend (double-tap zoom reset on empty background)
 *   - #keyrow click (dialog keys + attention collapse)
 *   - #termtoggle / #scrollend click
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
 *   - jumpToChatAddress / pendingJump creation (bookmarks/search set them)
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
import { hardAttention as hardAttentionMod } from "./map-model.js";
import { focusAtEnd } from "./caret.js";

/* ---------- public constants ---------- */

export const ATTENTION_SUPPRESS_MS = 2500;
export const SENT_ECHO_TIMEOUT_MS = 180000;
export const SCROLL_NEAR_BOTTOM_PX = 80;
export const CHAT_LOAD_DELAY_MS = 300;
export const CHAT_DETAIL_SAVED_MS = 2000;
export const PENDING_JUMP_TTL_MS = 15000;

export const RASTER_RE = /\.(png|jpe?g|gif|webp)$/i;

export const DEC_LABELS = {
  no_transcript: "No transcript yet \u2014 showing the terminal",
  no_structured_request: "Showing the terminal",
  pane_active: "Agent is working",
  waiting_question: "Agent is waiting for your answer",
  waiting_approval: "Agent needs your approval",
  quiet_inspect: "Quiet \u2014 inspect the terminal",
  /* ACP transport (pi/opencode/grok): no pane, so "terminal" reads as the event log */
  turn_active: "Agent is working",
  turn_error: "The agent finished without output \u2014 check the log",
};

const KEYS = { Up: "\u2191", Down: "\u2193", Enter: "\u23ce", Escape: "Esc" };
const HINTS = {
  inspect: "The agent has gone quiet \u2014 check the terminal below; keys go straight to it:",
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
} = {}){
  const unconfirmed = delivery === "unconfirmed";
  // Server-reported fresh (zero-turn post-seam segment) means a deliberate
  // /clear: show "fresh chat — send a prompt" instead of treating empty+
  // fallback as a broken transcript that forces a terminal peek (P2c).
  // Attention and unconfirmed delivery still force the pane.
  const mustShowPane = !attentionHidden && (
    !!attention || unconfirmed || (!!fallback && !fresh)
  );
  const freshSurface = !turnsLength && !mustShowPane &&
                       (fresh || (source === "acp" && (priorTurns || 0) > 0));
  const forcePeek = mustShowPane || (!turnsLength && !freshSurface);
  const showPeek = forcePeek || !!termOpen;
  return { unconfirmed, mustShowPane, freshSurface, forcePeek, showPeek };
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
    p.permTitle,
    p.permReason || "",
    p.permOptionsKey,
    p.priorTurns || 0,
    p.chatStarted || "",
    p.echoHash || "",
    p.histKey || "",
    p.turnsHash || "",
    p.expanded ? "1" : "0",
  ].join("|");
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

export function pendingEmptyHTML({ freshSurface, pending }){
  if (freshSurface) return `<div class="pending">fresh chat \u2014 send a prompt</div>`;
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
  if (idx < 0) idx = list.findIndex(t => t.text === pending.text);
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
} = {}){
  const url = `/api/nodes/${encodeURIComponent(nodeId)}/attachments/${encodeURIComponent(leaf)}`;
  const name = leaf.replace(/^[0-9a-f]{8}-/, "");
  isImage = isImage && rasterRe.test(name);
  return isImage
    ? `<a class="attthumb" data-tkey="${escape(leaf)}"${tileStyle(tileBox, leaf)} href="${url}" target="_blank" rel="noopener" title="${escape(name)}">` +
      `<img src="${url}" alt="${escape(name)}" loading="lazy" onerror="this.closest('.attthumb').remove()"></a>`
    : `<a class="attfile" href="${url}" target="_blank" rel="noopener" download title="${escape(name)}">` +
      `${iconFile}<span>${escape(name)}</span></a>`;
}

export function refTilesHTML(refs, nodeId, deps = {}){
  if (!refs || !refs.length) return "";
  const tiles = refs.map(r =>
    attTileHTML(nodeId, (r.path || "").split("/").pop(), /^image\//.test(r.mime || ""), deps)).join("");
  return `<div class="attrow">${tiles}</div>`;
}

export function assetTileHTML(nodeId, id, alt, rec, {
  escape = esc, iconFile = "", tileBox = {},
} = {}){
  if (!rec){
    return `<span class="attfile missing" title="${escape(alt || id)} — unavailable">` +
           `${iconFile}<span>${escape(alt || "file")} (unavailable)</span></span>`;
  }
  const url = `/api/nodes/${encodeURIComponent(nodeId)}/assets/${encodeURIComponent(id)}`;
  const name = rec.name || alt || "file";
  const isImage = /^image\//.test(rec.mime || "");
  const src = rec.inline && rec.data ? rec.data : url;
  return isImage
    ? `<a class="attthumb" data-tkey="${escape(id)}"${tileStyle(tileBox, id)} href="${url}" target="_blank" rel="noopener" title="${escape(name)}">` +
      `<img src="${src}" alt="${escape(name)}" loading="lazy" onerror="this.closest('.attthumb').remove()"></a>`
    : `<a class="attfile" href="${url}" target="_blank" rel="noopener" download title="${escape(name)}">` +
      `${iconFile}<span>${escape(name)}</span></a>`;
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

export function keyRowHTML({
  attention, attentionHidden, source, permTitle, permOptions,
  permToolKind = "", permReason = "", expanded = false,
  escape = esc,
} = {}){
  if (!attention || attentionHidden) return "";
  if (source === "acp"){
    const opts = permOptions || [];
    if (!opts.length){
      return `<span class="hint">Waiting for approval options from the agent...</span>`;
    }
    const { verb, code } = splitPermTitle(permTitle);
    const asCode = !!verb || PERM_CODE_KINDS.has(permToolKind || "");
    const body = asCode
      ? `<pre class="permcode">${escape(code)}</pre>`
      : `<div class="permtext">${escape(code)}</div>`;
    const clamp = expanded ? "" : " clamped";
    /* Start hidden; revealPermMoreIfNeeded unhides only when .permask overflows
       vertically. Single-line asks must not get a dead control. */
    const more = expanded ? "" : `<button class="permmore" hidden>show all</button>`;
    /* Reason is a distinct line above verb+body (G1b). Same dimmed chrome as
       .permverb so they cannot collapse into one visual line. Empty = omit. */
    const reason = (permReason && String(permReason)) || "";
    const reasonEl = reason
      ? `<div class="permreason">${escape(reason)}</div>` : "";
    const verbEl = verb ? `<div class="permverb">${escape(verb)}</div>` : "";
    /* .permmore is a sibling of .permask — inside the clamp it is clipped
       whenever the ask exceeds three lines (the exact moment it is needed).
       P5b: .permreason is a sibling too (like .hint). Inside the clamp it
       stole lines from the ask body; outside, the reason is always visible
       and revealPermMoreIfNeeded measures body overflow only. */
    const mask = `<div class="permask${clamp}">${verbEl}${body}</div>`;
    const btns = `<div class="permbtns">${opts.map(o => {
      const { label, title } = permOptionLabel(o);
      const cls = permOptionClass(o.kind);
      const titleAttr = title ? ` title="${escape(title)}"` : "";
      return `<button data-key="${escape(o.key)}" class="permbtn ${cls}"${titleAttr}>${escape(o.key)}. ${escape(label)}</button>`;
    }).join("")}</div>`;
    /* Hint outside the clamp; wording must not repeat the tool title — that
       is the ask body, rendered immediately below. Order:
       hint → permreason → permask(verb+body) → permmore → permbtns */
    const hint = `<span class="hint">The agent needs your approval \u2014 choose one:</span>`;
    return hint + reasonEl + mask + more + btns;
  }
  return `<span class="hint">${escape(HINTS[attention] || `The agent waits for your ${attention} \u2014 keys go straight to its terminal:`)}</span>` +
    `<div class="permbtns keys">` +
    ["1","2","3","4","y","n","Up","Down","Enter","Escape"].map(k =>
      `<button data-key="${k}" class="permbtn">${KEYS[k] || k}</button>`).join("") +
    `</div>`;
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

/* ---------- feature factory ---------- */

export function createChatFeature(deps){
  const d = deps || {};
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
  const scrollend = roots.scrollend;
  const workpulse = roots.workpulse;

  /* feature-owned state */
  let chatSig = "";
  /* Per-node chat poll validator (ETag). Keyed by node id so a tag from A is
     never sent for B; cleared on node switch (onSelectChange). */
  let chatETag = { node: "", etag: "" };
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

  function applyChatActivityChrome(live, mustShowPane, forcePeek, showPeek, replyReady){
    setWorkPulse(workPulseShouldRun(live, mustShowPane, replyReady));
    if (typeof d.setComposerBusy === "function")
      d.setComposerBusy(live === "active" && !replyReady);
    if (convtools) convtools.hidden = false;
    if (termtoggle){
      termtoggle.innerHTML = icons.ICON_TERM || "";
      if (termtoggle.classList) termtoggle.classList.toggle("on", showPeek);
      termtoggle.disabled = forcePeek;
      if (typeof termtoggle.setAttribute === "function")
        termtoggle.setAttribute("aria-label", showPeek ? "hide terminal" : "show terminal");
    }
  }

  function attentionSuppressed(id){
    if (!id) return false;
    if (attentionIsSuppressed(id, suppressAttentionUntil)) return true;
    delete suppressAttentionUntil[id];
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

  function onSelectChange(){
    chatSig = "";
    /* Drop the per-node chat ETag so a tag from node A is never sent for B. */
    chatETag = { node: "", etag: "" };
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

  async function loadHistory(id, scrollTo){
    try {
      const data = await api(`/api/nodes/${encodeURIComponent(id)}/chat?history=1`);
      if (g("sel", "") !== id) return;
      chatHist = { node: id, segs: data.segments || [], scrollTo: scrollTo || "", assets: data.assets || {} };
      chatScrollBottom = false;
      chatSig = "";
      await refreshChat();
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
      ? `context ${Math.round(chatCtxPct.pct)}%` : "";
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
    /* Only the tag for this node — never a sibling's. */
    const held = chatETag.node === n.id ? chatETag.etag : "";
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
    if (typeof d.setAttachAvail === "function") d.setAttachAvail(turns.length > 0);

    if (sentEcho && sentEcho.node === n.id){
      const now = typeof d.now === "function" ? d.now() : Date.now();
      const r = retireSentEcho(sentEcho, turns, now);
      sentEcho = r.echo;
    }
    const echo = sentEcho && sentEcho.node === n.id ? sentEcho : null;
    const assets = Object.assign({}, chatHist.assets || {}, data.assets || {});
    if (!n.attention) delete suppressAttentionUntil[n.id];
    const attentionHidden = !!n.attention && attentionSuppressed(n.id);
    const policy = chatActivityPolicy({
      attention: n.attention,
      attentionHidden,
      fallback: data.fallback,
      delivery: data.delivery,
      turnsLength: turns.length,
      source: data.source,
      priorTurns: data.prior_turns || 0,
      termOpen,
      fresh: !!data.fresh,
    });
    const { unconfirmed, mustShowPane, freshSurface, forcePeek, showPeek } = policy;

    let peekText = "";
    if (showPeek){
      try {
        peekText = await api(`/api/nodes/${encodeURIComponent(n.id)}/peek${mustShowPane ? "?mode=visible" : ""}`);
      } catch { /* ignore */ }
      if (gen !== g("selGen", 0) || g("sel", "") !== n.id) return;
    }

    if (gauge){
      if (data.ctx_window){
        gauge.hidden = false;
        if (gaugefill){
          gaugefill.style.width = Math.min(100, data.ctx_pct || 0) + "%";
          gaugefill.className = (data.ctx_pct || 0) >= 80 ? "high" : "";
        }
      } else {
        gauge.hidden = true;
      }
    }
    const pct = data.ctx_window ? Math.min(100, data.ctx_pct || 0) : null;
    if (chatCtxPct.node !== n.id || chatCtxPct.pct !== pct){
      chatCtxPct = { node: n.id, pct };
      renderChatHead();
    }
    // A successful /clear is ready for input immediately even if a tmux pane
    // redraw keeps mechanical liveness active for its debounce window.
    applyChatActivityChrome(
      data.live, mustShowPane, forcePeek, showPeek,
      !!data.reply_ready || freshSurface,
    );

    const hist = chatHist.node === n.id && chatHist.segs ? chatHist.segs : null;
    const priorSegs = priorSegsFromHistory(hist, data.chat_started);
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
      permTitle: data.perm_title,
      permReason: data.perm_reason || "",
      permOptionsKey: permOptionsKey(data.perm_options),
      priorTurns: data.prior_turns || 0,
      chatStarted: data.chat_started || "",
      echoHash: echo ? hash(echo.text) : "",
      histKey: hist ? "h" + priorSegs.length : "",
      turnsHash: hashTurns(turns),
      expanded,
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
        (s.turns || []).map((t, ti) => {
          const h = splitAssetRefs(t.text, n.id, assets, tileDeps());
          const bk = histBk(si, ti);
          bubbleTurns[bk] = t;
          return `
      <div class="${turnRoleClass(t.role, { hist: true, media: !!(h.html && !h.clean) })}" data-bk="${bk}" data-time="${escape(t.time || "")}" data-uid="${escape(t.uid || "")}" data-segment="${t.segment || 0}" data-record="${t.record || 0}">
        <div class="bubble" title="${escape(titleFn(t.role, t.time))}">${markdown(h.clean)}${h.html}</div>
      </div>`;
        }).join("")
      ).join("") +
      (data.chat_started
        ? `<div class="chatseam curseam"><span>chat started ${escape(whenFn(data.chat_started))}</span></div>` : "") +
      (!turns.length
        ? pendingEmptyHTML({ freshSurface, pending: data.pending })
        : "") +
      turns.map((t, i) => {
        const a = splitAssetRefs(t.text, n.id, assets, tileDeps());
        const bk = liveBk(i);
        bubbleTurns[bk] = t;
        return `
      <div class="${turnRoleClass(t.role, { media: !!(a.html && !a.clean) })}" data-i="${i}" data-bk="${bk}">
        <div class="bubble" title="${escape(titleFn(t.role, t.time))}">${markdown(a.clean)}${a.html}</div>
      </div>`;
      }).join("") +
      (echo ? echoBubbleHTML(echo.text, "", { markdown }) : "");

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
        expanded,
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
    const b = e.target.closest && e.target.closest("[data-key]");
    if (!b || !sel) return;
    const dest = sel;
    collapseAttentionUI(dest);
    (async () => {
      try {
        await api(`/api/nodes/${encodeURIComponent(dest)}/key`,
          { method: "POST", body: JSON.stringify({ key: b.dataset.key }) });
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
    const hl = e.target.closest && e.target.closest(".histload");
    if (hl){
      const sel = g("sel", "");
      if (sel) loadHistory(sel, "seam");
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
          if (typeof d.stampAddress === "function") d.stampAddress(bookmark, turn);
          if (typeof d.uiMutate === "function") d.uiMutate({ k: "bookmark-add", bookmark });
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
        });
      else if (ba.dataset.bact === "desc")
        useTurnAsDescription(turn.text);
      return;
    }
    const turnEl = e.target.closest && e.target.closest(".turn");
    if (turnEl && turnEl.dataset && turnEl.dataset.bk &&
        !(e.target.closest && e.target.closest("a, button"))){
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
    on(msgs, "click", onMsgsClick);
    on(msgs, "touchend", onMsgsTouchEnd, { passive: true });
    on(keyrow, "click", onKeyrowClick);
    on(termtoggle, "click", onTermToggle);
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
