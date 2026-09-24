/* Bookmarks feature: pane, flags, clamp, composer, jump, send-to (explicit bar).
 *
 * Packet 7E ownership inventory
 * -----------------------------
 * Owned DOM roots / external controls:
 *   - #bookmarkspane (panel root; class open via body.bookmarks-open)
 *   - #bookmarktabs / #bookmarklist (polled signature-guarded rebuild)
 *   - #bookmarkbar / #bookmarkchip / #bookmarkchiptext / #bookmarkchipx
 *   - #bookmarkprompt / #bookmarksend (singleton composer outside #bookmarklist)
 *   - #bookmarkflags (chat-area lane flags)
 *   - #bookmarksbtn (chat-head open/close chevron)
 *   - #bookmarksback (back label; setBackLabel from chat selection)
 *   - #notesbtn (visible "Notes ›" inside Bookmarks root — listener only;
 *     openNotes is a lazy injected closure; never import notes.js)
 *   - #bookmarkpeek (externally passed tap-catcher; exactly one listener)
 *   - #sendto_title / #sendto_list (the one send-to dialogue; chat.js drives it
 *     through the injected openSendTo, never by touching these roots)
 *
 * Persisted state (localStorage, exact keys):
 *   - "scimux-bookmarks"     — "1" / "0" for pane open
 *   - "scimux-bookmarktab"   — active tab id or "GENERAL"
 *
 * Ephemeral view state (never persisted, never ui.json):
 *   - openBookmarkT          — unfolded action-row t
 *   - expandedBookmarks      — Set of t with Show less
 *   - bookmarksSig / flagsSig — signature skips
 *   - bookmarksPin           — scroll to bottom after next rebuild
 *   - bookmarkAnchor         — reply target t (composer chip)
 *
 * Durable source-address fields (stampAddress):
 *   - uid, segment, record stamped only when src.uid is present
 *   - Number coercion with || 0; legacy node/time/text remain fallbacks
 *
 * UI operations (via injected uiMutate — state.js applyOp):
 *   - { k: "bookmark-add", bookmark }
 *   - { k: "bookmark-del", t }
 *   - { k: "forward-link-add", link } (confirmation is minted by chat.js)
 *
 * Owned UI operations / pure decisions:
 *   - Lane derivation (live node.lane_id, captured-lane fallback, comment→anchor)
 *   - Comment-at-anchor sort keys (anchor.t + "~" + comment.t)
 *   - Tabs/list/flags HTML and ordering (oldest-first; comments after anchors)
 *   - 220px clamp decision + in-place Show more/less
 *   - Open/close/toggle, bottom pin, invalid-tab → GENERAL
 *   - Jump / copy / delete / comment / use-in-note action dispatch
 *   - Manual bookmark creation (general / lane / comment-anchor)
 *   - Reply chip, blur empty-cancel, Escape, chip dismiss
 *   - Send-to draft merge + sheet/navigation (explicit bar button; P6 removed
 *     the longpress-only route)
 *   - openSendTo: shared picker for bookmark bar, inbox, reference, and chat
 *     bubbles — live/open targets only, pinned block then recency; each
 *     row is lane swatch, then agent logo, then title (sendToItemHTML)
 *
 * Timers / clipboard / scrolling / layout seams:
 *   - copy acknowledgement 900ms timer (restores ICON_COPY; clears sig)
 *   - copyText injected (secure clipboard or execCommand fallback)
 *   - post-render scrollTop = scrollHeight when bookmarksPin
 *   - open action row scrollIntoView nearest/smooth after unfold
 *   - body.bookmarks-open class toggle
 *
 * Injected node / lane / chat / Notes / sheet / navigation effects:
 *   - nodeById, orderedNodes, pinned, laneList, laneColor, laneModel, agentLogo
 *   - uiMutate, bookmarks getter (UI.bookmarks)
 *   - jump deps: setPendingJump, invalidateChat, select, openPreview,
 *     wsOpen, closeWorkspace, isDesktop, singleZone
 *   - startPlacement (Notes use-in-note — Notes owns workspace; 7F)
 *   - openNotes lazy (Notes › / workspace)
 *   - openSheet / closeSheets, setLevel, restartWorkPulse
 *   - toast, copyText, prompt focus targets
 *   - storage (localStorage), document, setTimeout/clearTimeout, CSS.escape
 *   - esc, md, fmtWhen, hashStr, icons, stripAssetRefs (format.js)
 *   - menu.js createPopoverMenu for the overflow (comment/copy/Delete)
 *
 * bind / destroy lifecycle:
 *   - createBookmarksFeature(deps) → { bind, destroy, render, setOpen,
 *     isOpen, invalidate, pinBottom, setBackLabel, jumpToChatAddress }
 *   - bind() is idempotent; destroy() removes every owned listener and
 *     closes the overflow menu; shell DOM roots stay in place
 *
 * Explicit non-ownership:
 *   - Notes workspace / inbox / reference projection / bookmarkSnapshot /
 *     bookmarkSource / startPlacement body (Packet 7F)
 *   - Document swipe decisions, #scrim, overlayOwned snapshot (shell /
 *     navigation.js)
 *   - Search, the openPreview implementation, map station capture
 *     (addStationBookmark shell), chat bubble capture (chat.js)
 *   - Composer #promptbar (composer.js), generic sheets, polling tick body
 *   - state.js applyOp bookmark-add/del (reducer stays shared)
 *
 * Reuses (no algorithm duplication):
 *   format.js: esc, md, fmtWhen, stripAssetRefs
 *   lanes.js: hashStr
 *   map-model.js: sendableNodes (send-to eligibility + ranking)
 *   menu.js: createPopoverMenu, menuButtonHTML, menuSepHTML
 *
 * Contracts preserved:
 *   - Oldest-first list; comments immediately after anchors
 *   - Live-node lane with captured-lane fallback
 *   - Durable uid/segment/record before legacy fallbacks
 *   - Exact storage keys and UI-op shapes
 *   - Signature skip without losing expanded/openBookmarkT
 *   - 220px clamp; in-place Show more/less
 *   - Reply-anchor cancellation on empty blur / Escape / chip / tab change
 *   - Post-render bottom pinning
 *   - Existing IDs, classes, ARIA, data attributes, labels, source order
 */

import {
  esc as escDefault,
  md as mdDefault,
  fmtWhen as fmtWhenDefault,
  stripAssetRefs,
} from "./format.js";
import { hashStr as hashStrDefault } from "./lanes.js";
import { sendableNodes } from "./map-model.js";
import { focusAtEnd } from "./caret.js";
import { addPendingForward } from "./storage.js";
import { mediaTextAndTiles, legacyMediaTextAndTiles, hasSupportedImageMarker } from "./reference-media.js";
import {
  createPopoverMenu,
  menuButtonHTML,
  menuSepHTML,
} from "./menu.js";

/* ---------- public constants ---------- */

export const BOOKMARK_CLAMP_PX = 220;
export const STORAGE_KEY_OPEN = "scimux-bookmarks";
export const STORAGE_KEY_TAB = "scimux-bookmarktab";
export const COPY_ACK_MS = 900;
export const DRAFT_KEY_PREFIX = "scimux-draft:";

/* ---------- pure: durable address ---------- */

/* stampAddress copies a source turn's durable address (uid, segment, record)
   onto a captured note. src can be a chat turn (numeric segment/record) or a
   search hit's dataset (string values); coerce and stamp only when a real uid
   is present, so a legacy/tmux turn with no address leaves the note to its
   node+turnTime live hints rather than a bogus (uid:"", 0, 0) triple. */
export function stampAddress(bookmark, src){
  if (!src || !src.uid) return;
  bookmark.uid = src.uid;
  bookmark.segment = Number(src.segment) || 0;
  bookmark.record = Number(src.record) || 0;
}

/* ---------- pure: lane / sort ---------- */

/* a note's lane is derived live from its activity (activities gain lanes
   later; notes follow automatically); the capture-time snapshot is the
   fallback for deleted activities and the home of manual notes. Comments
   inherit their anchor's lane so the pair never separates. */
export function bookmarkLaneId(nt, byT, nodeById){
  if (nt.anchor && byT && byT[nt.anchor]) nt = byT[nt.anchor];
  if (!nt.node) return nt.lane || "";
  const nd = typeof nodeById === "function" ? nodeById(nt.node) : null;
  return nd ? (nd.lane_id || "") : (nt.lane || "");
}

/* effective sort key: a comment sorts at its anchor's position, right after
   it ("~" > every ISO-timestamp character); its own creation time is still
   what's displayed — revelations may come late, order shows where they belong */
export function bookmarkSortKey(nt, byT){
  if (nt.anchor && byT && byT[nt.anchor]) return byT[nt.anchor].t + "~" + nt.t;
  return nt.t + "~";
}

/* ---------- pure: clamp ---------- */

/* Long noted bubbles force a lot of scrolling; clamp the tall ~15% behind a
   "Show more" toggle and leave the short majority alone. bookmarkClampState is the
   DOM-free decision core — the measure loop feeds it a real scrollHeight —
   kept pure so it is unit-testable without layout. BOOKMARK_CLAMP_PX ≈ 12 lines,
   near the p90 assistant bubble. Expand state is ephemeral view state (like
   openBookmarkT): an in-memory Set, never persisted, never synced. */
export function bookmarkClampState(naturalPx, expanded){
  const tall = naturalPx > BOOKMARK_CLAMP_PX;
  return { clamped: tall && !expanded, showBtn: tall, label: expanded ? "Show less" : "Show more" };
}

/* ---------- pure: tab resolution / list model ---------- */

/** Fall back to GENERAL when the active non-GENERAL tab has no remaining lane. */
export function resolveBookmarkTab(tab, activeLaneIds){
  if (tab !== "GENERAL" && !(activeLaneIds || []).includes(tab)) return "GENERAL";
  return tab || "GENERAL";
}

/** Build byT map, laneOf map, tabs (lanes with at least one bookmark), filtered+sorted list. */
export function bookmarkListModel(bookmarks, bookmarkTab, nodeById, laneList){
  const list = Array.isArray(bookmarks) ? bookmarks : [];
  const byT = {};
  list.forEach(nt => { byT[nt.t] = nt; });
  const laneOf = {};
  list.forEach(nt => { laneOf[nt.t] = bookmarkLaneId(nt, byT, nodeById); });
  const lanes = typeof laneList === "function" ? laneList() : (laneList || []);
  const tabs = (lanes || []).filter(l => list.some(nt => laneOf[nt.t] === l.id));
  const tab = resolveBookmarkTab(bookmarkTab, tabs.map(l => l.id));
  const want = tab === "GENERAL" ? "" : tab;
  const filtered = list
    .filter(nt => (laneOf[nt.t] || "") === want)
    .sort((a, b) => bookmarkSortKey(a, byT).localeCompare(bookmarkSortKey(b, byT)));
  return { byT, laneOf, tabs, tab, list: filtered };
}

/* ---------- pure: HTML builders ---------- */

export function bookmarkTabsHTML(bookmarkTab, tabs, esc, laneColor){
  const e = esc || (s => String(s ?? ""));
  const lc = laneColor || (() => "");
  return (
    `<button data-ntab="GENERAL" class="${bookmarkTab === "GENERAL" ? "on" : ""}">General</button>` +
    (tabs || []).map(l => `<button data-ntab="${e(l.id)}" class="${bookmarkTab === l.id ? "on" : ""}"
      ><span class="tabdot" style="background:${e(lc(l.id))}"></span>${e(l.name)}</button>`).join("")
  );
}

export function bookmarkListHTML(opts){
  const {
    list, bookmarkTab, openBookmarkT,
    nodeById, esc, md, fmtWhen, laneColor, icons = {},
    workspaceOpen = false, singleZone = false, overflow = false,
  } = opts || {};
  const e = esc || (s => String(s ?? ""));
  const renderMd = md || (s => String(s ?? ""));
  const when = fmtWhen || (s => String(s ?? ""));
  const lc = laneColor || (() => "");
  const color = bookmarkTab === "GENERAL" ? "var(--unlane)" : e(lc(bookmarkTab));
  if (!list || !list.length){
    return `<div class="empty">No bookmarks yet — tap a bubble and “bookmark” it.</div>`;
  }
  return list.map(nt => {
    const nd = nt.node && typeof nodeById === "function" ? nodeById(nt.node) : null;
    const src = nd ? " · " + e(nd.title) : "";
    const rendered = nt.media
      ? mediaTextAndTiles(nt.text || "", nt.media, { esc: e, assetURL: opts.assetURL })
      : (hasSupportedImageMarker(nt.text) ? legacyMediaTextAndTiles(nt, opts.legacyMedia, { esc:e, assetURL:opts.assetURL }) : { clean: nt.text || "", html: "" });
    return `
    <div class="bookmark ${nt.anchor ? "comment" : ""}" data-t="${e(nt.t)}">
      <div class="when">${e(when(nt.t))}${src}</div>
      <div class="nbubble" style="border-left-color:${color}">${renderMd(rendered.clean)}${rendered.html}</div>
      <button class="nmore" data-nmore hidden></button>
      ${openBookmarkT === nt.t
        ? bookmarkActionsHTML(nt, { icons, context: "pane", workspaceOpen, singleZone, overflow }) : ""}
    </div>`;
  }).join("");
}

/* The one action row a bookmark gets, wherever it is shown (UI review items
   5, 6, 8, 9 + P6 + P5). Both the compact Bookmarks pane and the Notes
   workspace inbox render this, so the two can no longer drift apart in
   geometry, order, or which actions exist at all.

   P5 grammar (icon-only, flat by default):
     pane:  [jump] [comment|copy] [note?] [copy?] [sendto] [del.danger]
     inbox: [jump] [copy] [note] [sendto] [del.danger]
   Former overflow-menu entries append rightward in menu order. Delete is
   last and .danger; the click handler confirms before firing (like .wsref
   trash). Comment is still pane-only and withheld from comments (no nesting);
   when withheld, Copy takes its primary slot and is not duplicated later.

   Width guard (`overflow: true`): max flat row is 6×44 + 5×4 gap = 284px.
   Phone Bookmarks pane content on a 320px SE is ~232px, so a 6-button row
   would clip inside overflow:hidden cards. Call sites pass overflow when
   !isDesktop() to keep today's more-menu bar on narrow widths. Desktop
   (`var(--pane)` 340px content ~296px) fits the flat row with flex-start.

   "use in note" is withheld from the pane while the Notes workspace is closed
   (review 2, item 4): it arms placement mode, whose only targets are the
   section rows inside that workspace, so from the compact pane it armed a mode
   with nothing visible to complete it. The inbox lives inside the workspace and
   therefore always offers it. `singleZone` is the phone exception: with the
   workspace's zones shown one at a time there is no inbox alongside the pane,
   so the pane's paperclip IS the route into placement (it opens the workspace
   in placing mode) and the armed mode is the next screen, not an invisible one. */
export function bookmarkActionsHTML(nt, opts){
  const {
    icons = {}, context = "pane", workspaceOpen = false, singleZone = false,
    overflow = false,
  } = opts || {};
  const canPlace = context === "inbox" || workspaceOpen || singleZone;
  /* Comment is pane-only and withheld on comment rows (no nesting) and when
     the bookmark has no node. When withheld, promote Copy into the primary
     slot so it never has a hole and no action appears twice. */
  const canComment = context === "pane" && !!(nt && nt.node && !nt.anchor);
  const b = (act, label, icon, extra = "") =>
    `<button class="btn-plain${extra}" data-bmact="${act}" aria-label="${label}">${icon || ""}</button>`;
  const into = icons.ICON_INTO
    ? `<span class="rot180">${icons.ICON_INTO}</span>`
    : "";

  /* Phone / narrow: keep the pre-P5 bar so 6×44 never clips the card. */
  if (overflow){
    return `<div class="actionbar tear">
        ${nt.node || nt.uid ? b("jump", "open in activity", icons.ICON_JUMP) : ""}
        ${canComment ? b("comment", "comment", icons.ICON_COMMENT) : b("copy", "copy", icons.ICON_COPY)}
        ${canPlace ? b("note", "use in note", icons.ICON_CLIP) : ""}
        ${b("more", "more actions", icons.ICON_MENU_DOTS)}
      </div>`;
  }

  /* Flat (default / desktop): primaries, then former menu entries in order. */
  let html = `<div class="actionbar tear">`;
  if (nt && (nt.node || nt.uid)) html += b("jump", "open in activity", icons.ICON_JUMP);
  if (canComment) html += b("comment", "comment", icons.ICON_COMMENT);
  else html += b("copy", "copy", icons.ICON_COPY);
  if (canPlace) html += b("note", "use in note", icons.ICON_CLIP);
  /* Topmost menu entry → next right: copy only when it was not the primary. */
  if (canComment) html += b("copy", "copy", icons.ICON_COPY);
  html += b("sendto", "send to\u2026", into);
  html += b("del", "delete", icons.ICON_TRASH, " danger");
  html += `</div>`;
  return html;
}

/** Overflow menu for a bookmark row when the bar uses overflow:true (phone).
    Menu items reuse data-bmact so the same dispatch handles bar and menu.
    Destructive Delete is last, .danger, after a separator (HIG). Copy is
    omitted when already on the bar (Comment withheld). */
export function bookmarkMenuHTML(nt, opts){
  const { icons = {}, context = "pane" } = opts || {};
  const canComment = context === "pane" && !!(nt && nt.node && !nt.anchor);
  /* Copy is on the bar whenever Comment is not — never both, never neither. */
  const copyOnBar = !canComment;
  const into = icons.ICON_INTO
    ? `<span class="rot180">${icons.ICON_INTO}</span>`
    : "";
  let html = "";
  if (!copyOnBar){
    html += menuButtonHTML({
      attrs: 'data-bmact="copy"',
      label: "Copy",
      icon: icons.ICON_COPY || "",
    });
  }
  html += menuButtonHTML({
    attrs: 'data-bmact="sendto"',
    label: "Send to\u2026",
    icon: into,
  });
  html += menuSepHTML();
  html += menuButtonHTML({
    attrs: 'data-bmact="del"',
    label: "Delete",
    icon: icons.ICON_TRASH || "",
    danger: true,
  });
  return html;
}

export function bookmarkFlagsHTML(flags, hasGeneral, esc){
  const e = esc || (s => String(s ?? ""));
  return (flags || []).map(f => `<button data-flag="${e(f.id)}" title="${e(f.name)} bookmarks"
      aria-label="${e(f.name)} bookmarks" style="background:${e(f.color || "")}"></button>`).join("")
    + (hasGeneral ? `<button data-flag="GENERAL" title="General bookmarks"
      aria-label="General bookmarks" style="background:var(--unlane)"></button>` : "");
}

/** Toggle chevron + aria for #bookmarksbtn. */
export function bookmarksToggleState(open){
  return {
    innerHTML: open ? "&#8250;" : "&#8249;",
    ariaLabel: open ? "close bookmarks" : "open bookmarks",
  };
}

/* Send-to is the one place scimux itself moves an agent's Output into another
   agent's Input. The receiving node records the arrival as an ordinary user
   turn, in an append-only store that is never rewritten — so without a marker
   it is *scimux*, not the user, asserting that a model wrote nothing here.
   SpaceXAI's Enterprise Terms bar misrepresenting Output as human-generated;
   its Consumer §4 imposes the disclosure affirmatively ("you agree that you
   will apply such disclosures"); Meta's AUP bars distributing Outputs without
   their provenance. The clipboard path is deliberately *not* marked: there the
   user copies, the user pastes and the user attributes, and scimux is upstream
   of their conduct the way it is of Meta §6.2.

   Marked on evidence only, never on suspicion. An unknown role is a legacy
   bookmark or a note the user typed themselves, and stamping "an AI wrote
   this" on the user's own words would be the same misattribution pointed the
   other way — the receiving agent would discount an instruction it should
   obey. Every path that knows the role now records it (chat.js and app.js at
   bookmark time, chat.js live from the turn), so "unknown" shrinks to the
   bookmarks that predate this rule.

   One line of plain text, then the Output verbatim. Plain because it is pasted
   into a terminal and read by a model, and verbatim because altering an Output
   is the neighbouring prohibition. */
export const AI_DISCLOSURE =
  "[AI-generated, quoted from another chat \u2014 not written by me:]";

/** Prefix an agent's Output with its disclosure; anything else passes through. */
export function discloseAgentOutput(text, role){
  const t = text || "";
  if (!t || role !== "assistant") return t;
  return AI_DISCLOSURE + "\n\n" + t;
}

/** Longpress send-to: merge bookmark text into an existing per-node draft. */
export function mergeBookmarkIntoDraft(prev, text){
  return (prev ? prev + "\n\n" : "") + (text || "");
}

/** One send-to destination row: lane swatch, agent logo, title.
    The logo is visual identity (HIG leading image). It is aria-hidden; the
    button's aria-label carries "title, agent" so VoiceOver can tell two
    like-named chats apart. Logo HTML is trusted (agentLogo returns SVG/mask
    markup) and is not re-escaped. */
export function sendToItemHTML(n, opts){
  if (!n || !n.id) return "";
  const d = opts || {};
  const e = d.esc || escDefault;
  const color = typeof d.laneColor === "function" ? d.laneColor(n.lane_id) : "";
  const logoFn = typeof d.agentLogo === "function" ? d.agentLogo : () => "";
  const title = n.title || "";
  const agent = n.agent || "agent";
  const logo = logoFn(n.agent);
  const aria = title ? `${title}, ${agent}` : agent;
  return `<button type="button" class="pos-item" data-fwd="${e(n.id)}" aria-label="${e(aria)}" style="width:100%;text-align:left">
      <span style="width:12px;height:12px;border-radius:6px;flex:none;align-self:center;background:${e(color)}"></span>
      <span class="agent-logo" title="${e(agent)}" aria-hidden="true">${logo}</span>
      <span>${e(title)}</span>
    </button>`;
}

/** Manual create payload shape (caller supplies ISO t). */
export function manualBookmarkPayload({ text, t, bookmarkAnchor, bookmarkTab }){
  const bookmark = { t, text };
  if (bookmarkAnchor) bookmark.anchor = bookmarkAnchor;
  else if (bookmarkTab && bookmarkTab !== "GENERAL") bookmark.lane = bookmarkTab;
  return bookmark;
}

/* ---------- pure: jump preparation decision ---------- */

/**
 * Decide how to resolve a jump address.
 * Returns { kind: "live"|"archived"|"fail", pendingJump?, uid?, segment?, record?, turnTime? }.
 */
export function jumpAddressDecision(a, nodeById){
  if (a && a.node && typeof nodeById === "function" && nodeById(a.node)){
    return {
      kind: "live",
      pendingJump: {
        node: a.node,
        turnTime: a.turnTime || "",
        text: a.text || "",
        uid: a.uid || "",
        segment: a.segment,
        record: a.record,
        ...(a.textPrefix ? { textPrefix: true } : {}),
      },
    };
  }
  if (a && a.uid){
    return {
      kind: "archived",
      uid: a.uid,
      segment: a.segment,
      record: a.record,
      turnTime: a.turnTime || "",
    };
  }
  return { kind: "fail" };
}

/* ---------- factory ---------- */

export function createBookmarksFeature(deps){
  const d = deps || {};
  const roots = d.roots || {};
  const doc = d.document || (typeof document !== "undefined" ? document : null);
  const storage = d.storage || null;
  const esc = d.esc || escDefault;
  const md = d.md || mdDefault;
  const fmtWhen = d.fmtWhen || fmtWhenDefault;
  const hashStr = d.hashStr || hashStrDefault;
  const icons = d.icons || {};
  const setTimeoutFn = d.setTimeout || setTimeout;
  const clearTimeoutFn = d.clearTimeout || clearTimeout;
  const CSSObj = d.CSS || (typeof CSS !== "undefined" ? CSS : { escape: s => String(s).replace(/"/g, '\\"') });

  const g = (name, fallback) => {
    if (typeof d[name] === "function") return d[name]();
    if (d[name] !== undefined) return d[name];
    return fallback;
  };

  /* ---- view state ---- */
  let bookmarksOpen = storage ? storage.getItem(STORAGE_KEY_OPEN) === "1" : false;
  let bookmarkTab = (storage && storage.getItem(STORAGE_KEY_TAB)) || "GENERAL";
  let openBookmarkT = "";
  let expandedBookmarks = new Set();
  let bookmarksSig = "";
  let bookmarksPin = true;
  let flagsSig = "";
  let bookmarkAnchor = "";
  let bound = false;
  const cleanups = [];
  let copyTimer = null;
  let sendtoListWithHandler = null;
  let sendtoClickHandler = null;
  const popMenu = createPopoverMenu(doc);
  let menuBookmarkT = "";
  let menuContext = "pane";

  function bookmarks(){
    return g("bookmarks", []) || [];
  }

  function nodeById(id){
    return typeof d.nodeById === "function" ? d.nodeById(id) : null;
  }

  function laneList(){
    if (typeof d.laneList === "function") return d.laneList() || [];
    return [];
  }

  function laneColor(id){
    return typeof d.laneColor === "function" ? d.laneColor(id) : "";
  }

  function setStorage(key, val){
    if (!storage) return;
    storage.setItem(key, val);
  }

  function isOpen(){ return bookmarksOpen; }

  function setBackLabel(t){
    if (roots.bookmarksback)
      roots.bookmarksback.textContent = "‹ " + t;
  }

  function invalidate(){
    bookmarksSig = "";
    flagsSig = "";
  }

  function pinBottom(){
    bookmarksPin = true;
  }

  function renderBookmarksToggle(){
    const btn = roots.bookmarksbtn;
    if (!btn) return;
    const st = bookmarksToggleState(bookmarksOpen);
    btn.innerHTML = st.innerHTML;
    btn.setAttribute("aria-label", st.ariaLabel);
  }

  function renderBookmarkFlags(){
    const bms = bookmarks();
    const byT = {};
    bms.forEach(nt => { byT[nt.t] = nt; });
    const laneIds = new Set(bms.map(nt => bookmarkLaneId(nt, byT, nodeById)));
    const flags = laneList().filter(l => laneIds.has(l.id));
    const html = bookmarkFlagsHTML(flags, laneIds.has(""), esc);
    if (html === flagsSig) return;
    flagsSig = html;
    if (roots.bookmarkflags) roots.bookmarkflags.innerHTML = html;
  }

  function applyBookmarkClamps(){
    const listEl = roots.bookmarklist;
    if (!listEl || typeof listEl.querySelectorAll !== "function") return;
    listEl.querySelectorAll(".bookmark").forEach(el => {
      const b = el.querySelector(".nbubble"), btn = el.querySelector(".nmore");
      if (!b || !btn) return;
      const st = bookmarkClampState(b.scrollHeight, expandedBookmarks.has(el.dataset.t));
      if (b.classList && typeof b.classList.toggle === "function")
        b.classList.toggle("clamped", st.clamped);
      else if (st.clamped) b.className = (b.className || "") + " clamped";
      btn.hidden = !st.showBtn;
      btn.textContent = st.label;
    });
  }

  function render(){
    if (doc && doc.body && doc.body.classList)
      doc.body.classList.toggle("bookmarks-open", bookmarksOpen);
    renderBookmarksToggle();
    renderBookmarkFlags();
    if (!bookmarksOpen) return;

    const model = bookmarkListModel(bookmarks(), bookmarkTab, nodeById, laneList);
    if (model.tab !== bookmarkTab){
      bookmarkTab = model.tab;
      setStorage(STORAGE_KEY_TAB, bookmarkTab);
    }
    const tabsHtml = bookmarkTabsHTML(bookmarkTab, model.tabs, esc, laneColor);
    const listHtml = bookmarkListHTML({
      list: model.list,
      bookmarkTab,
      openBookmarkT,
      nodeById,
      esc,
      md,
      fmtWhen,
      laneColor,
      icons,
      assetURL: d.assetURL,
      legacyMedia: d.legacyMedia,
      /* "use in note" only exists while its placement targets do (item 4),
         except on the single-zone phone layout where it is the way in */
      workspaceOpen: typeof d.wsOpen === "function" && d.wsOpen(),
      singleZone: typeof d.singleZone === "function" && d.singleZone(),
      /* P5 width guard: flat 6×44 row does not fit phone pane content;
         keep the more-menu bar below the isDesktop() breakpoint. */
      overflow: typeof d.isDesktop === "function" ? !d.isDesktop() : false,
    });
    const sig = hashStr(tabsHtml + "|" + listHtml);
    if (sig === bookmarksSig) return;
    bookmarksSig = sig;
    if (roots.bookmarktabs) roots.bookmarktabs.innerHTML = tabsHtml;
    if (roots.bookmarklist) roots.bookmarklist.innerHTML = listHtml;
    applyBookmarkClamps();
    if (bookmarksPin && roots.bookmarklist){
      roots.bookmarklist.scrollTop = roots.bookmarklist.scrollHeight;
      bookmarksPin = false;
    }
  }

  function setOpen(open){
    bookmarksOpen = !!open;
    setStorage(STORAGE_KEY_OPEN, open ? "1" : "0");
    bookmarksSig = "";
    bookmarksPin = true;
    render();
    if (typeof d.restartWorkPulse === "function") d.restartWorkPulse();
  }

  function openBookmark(t){
    const all = bookmarks();
    const target = all.find(nt => nt.t === t);
    if (!target) return false;
    const byT = {};
    all.forEach(nt => { byT[nt.t] = nt; });
    bookmarkTab = bookmarkLaneId(target, byT, nodeById) || "GENERAL";
    setStorage(STORAGE_KEY_TAB, bookmarkTab);
    openBookmarkT = t;
    setOpen(true);
    setTimeoutFn(() => {
      const list = roots.bookmarklist;
      const selector = `.bookmark[data-t="${CSSObj.escape(t)}"]`;
      const el = list && typeof list.querySelector === "function" ? list.querySelector(selector) : null;
      if (!el) return;
      if (el.classList) el.classList.add("jump-target");
      if (typeof el.scrollIntoView === "function") el.scrollIntoView({ block: "center", behavior: "smooth" });
      setTimeoutFn(() => { if (el.classList) el.classList.remove("jump-target"); }, 1400);
    }, 0);
    return true;
  }

  /* jumpToChatAddress — the one resolver behind both a note's "jump to" and an
     embedded reference's "jump to chat". Leaving any full-screen overlay is
     part of the jump — the destination must show. */
  function jumpToChatAddress(a){
    const decision = jumpAddressDecision(a, nodeById);
    if (decision.kind === "live"){
      const pj = { ...decision.pendingJump, ts: Date.now() };
      if (typeof d.setPendingJump === "function") d.setPendingJump(pj);
      if (typeof d.invalidateChat === "function") d.invalidateChat();
      if (typeof d.wsOpen === "function" && d.wsOpen() && typeof d.closeWorkspace === "function")
        d.closeWorkspace();
      if (typeof d.isDesktop === "function" && !d.isDesktop()) setOpen(false);
      if (typeof d.select === "function") d.select(a.node, "jump");
      return true;
    }
    if (decision.kind === "archived"){
      if (typeof d.wsOpen === "function" && d.wsOpen() && typeof d.closeWorkspace === "function")
        d.closeWorkspace();
      if (typeof d.openPreview === "function")
        d.openPreview(decision.uid, decision.segment, decision.record, decision.turnTime || "");
      return true;
    }
    return false;
  }

  function setBookmarkAnchor(t){
    const target = t ? bookmarks().find(x => x.t === t) : null;
    bookmarkAnchor = target ? t : "";
    const prompt = roots.bookmarkprompt;
    if (prompt && prompt.dataset)
      prompt.dataset.placeholder = bookmarkAnchor ? "Reply…" : "Add a note…";
    const chip = roots.bookmarkchip;
    if (bookmarkAnchor){
      if (roots.bookmarkchiptext)
        roots.bookmarkchiptext.textContent =
          "Replying to: " + (target.text || "").replace(/\s+/g, " ").trim();
      if (chip) chip.hidden = false;
    } else if (chip){
      chip.hidden = true;
    }
  }

  function addManualBookmark(){
    const prompt = roots.bookmarkprompt;
    const text = prompt && (prompt.innerText != null ? prompt.innerText : prompt.textContent || "");
    const clean = String(text || "").trim();
    if (!clean) return;
    const bookmark = manualBookmarkPayload({
      text: clean,
      t: new Date().toISOString(),
      bookmarkAnchor,
      bookmarkTab,
    });
    if (typeof d.uiMutate === "function") d.uiMutate({ k: "bookmark-add", bookmark });
    if (prompt) prompt.textContent = "";
    setBookmarkAnchor("");
    bookmarksPin = true;
    render();
  }

  /* ---- event handlers ---- */

  function onToggleClick(){
    setOpen(!bookmarksOpen);
  }

  function onBackClick(){
    setOpen(false);
  }

  function onNotesClick(){
    if (typeof d.openNotes === "function") d.openNotes();
  }

  function onPeekClick(){
    setOpen(false);
  }

  function onFlagsClick(e){
    const f = e.target && e.target.closest && e.target.closest("[data-flag]");
    if (!f) return;
    bookmarkTab = f.dataset.flag;
    setStorage(STORAGE_KEY_TAB, bookmarkTab);
    setOpen(true);
  }

  function onPaneClick(e){
    const more = e.target && e.target.closest && e.target.closest("[data-nmore]");
    if (more){
      /* ephemeral view-state flip: mutate the DOM in place, no rebuild, no
         persistence. Precedes the .bookmark tap branch so it never opens the
         action bar. */
      const el = more.closest(".bookmark"), t = el.dataset.t;
      expandedBookmarks.has(t) ? expandedBookmarks.delete(t) : expandedBookmarks.add(t);
      const b = el.querySelector(".nbubble");
      const st = bookmarkClampState(b.scrollHeight, expandedBookmarks.has(t));
      if (b.classList && typeof b.classList.toggle === "function")
        b.classList.toggle("clamped", st.clamped);
      more.textContent = st.label;
      return;
    }
    const tb = e.target.closest && e.target.closest("[data-ntab]");
    if (tb){
      bookmarkTab = tb.dataset.ntab;
      setStorage(STORAGE_KEY_TAB, bookmarkTab);
      setBookmarkAnchor("");   /* comment mode doesn't survive leaving its tab */
      openBookmarkT = "";
      bookmarksSig = "";
      bookmarksPin = true;
      render();
      return;
    }
    const act = e.target.closest && e.target.closest("[data-bmact]");
    if (act){
      /* Menu items live outside the card; fall back to the open-menu target. */
      const card = act.closest(".bookmark");
      const t = (card && card.dataset && card.dataset.t) || menuBookmarkT;
      const nt = bookmarks().find(x => x.t === t);
      if (!nt) return;
      if (act.dataset.bmact === "more"){
        openBookmarkOverflow(act, nt, "pane");
        return;
      }
      if (act.dataset.bmact === "sendto"){
        openSendTo({
          text: stripAssetRefs(nt.text || ""),
          exceptId: nt.node || "",
          title: "Send bookmark to\u2026",
          role: nt.role || "",
          source: (nt.node || nt.uid) ? {
            node: nt.node || "", turnTime: nt.turnTime || "",
            ...(nt.uid ? { uid: nt.uid, segment: Number(nt.segment) || 0, record: Number(nt.record) || 0 } : {}),
          } : null,
        });
        return;
      }
      if (act.dataset.bmact === "jump" && (nt.node || nt.uid)){
        if (!jumpToChatAddress({
          node: nt.node, uid: nt.uid, segment: nt.segment,
          record: nt.record, turnTime: nt.turnTime, text: nt.text,
        })){
          if (typeof d.toast === "function")
            d.toast("This note's activity was deleted — the text is still here to read or copy.");
        }
      } else if (act.dataset.bmact === "note"){
        if (typeof d.startPlacement === "function") d.startPlacement(nt);
      } else if (act.dataset.bmact === "comment"){
        popMenu.close();
        setBookmarkAnchor(nt.t);
        openBookmarkT = "";
        render();
        if (roots.bookmarkprompt && typeof roots.bookmarkprompt.focus === "function")
          roots.bookmarkprompt.focus();
      } else if (act.dataset.bmact === "copy"){
        popMenu.close();
        if (typeof d.copyText === "function") d.copyText(nt.text || "");
        /* bar button gets the checkmark ack; menu items just close */
        if (card){
          act.innerHTML = "&#10003;";
          if (copyTimer) clearTimeoutFn(copyTimer);
          copyTimer = setTimeoutFn(() => {
            act.innerHTML = icons.ICON_COPY || "";
            bookmarksSig = "";
            copyTimer = null;
          }, COPY_ACK_MS);
        }
      } else if (act.dataset.bmact === "del"){
        popMenu.close();
        /* Flat bar (toolbar): confirm first, like .wsref trash. Overflow menu
           already put Delete behind "more", so that path stays one-tap. */
        const onBar = !!(act.closest && act.closest(".actionbar"));
        if (onBar){
          openDeleteBookmarkConfirm(act, nt);
          return;
        }
        openBookmarkT = "";
        if (typeof d.uiMutate === "function") d.uiMutate({ k: "bookmark-del", t: nt.t });
      }
      return;
    }
    const bookmarkEl = e.target.closest && e.target.closest(".bookmark");
    if (bookmarkEl){
      openBookmarkT = openBookmarkT === bookmarkEl.dataset.t ? "" : bookmarkEl.dataset.t;
      render();
      /* same reveal as chat bubbles: the unfolded action row must be visible
         after the tap (re-query — the render above rebuilt the list DOM) */
      if (openBookmarkT && doc && typeof doc.querySelector === "function"){
        const escT = CSSObj.escape ? CSSObj.escape(openBookmarkT) : openBookmarkT;
        const row = doc.querySelector(
          `#bookmarklist .bookmark[data-t="${escT}"] .actionbar`,
        );
        if (row && typeof row.scrollIntoView === "function")
          row.scrollIntoView({ block: "nearest", behavior: "smooth" });
      }
    }
  }

  function onSendClick(){
    addManualBookmark();
  }

  function onPromptKeydown(e){
    if (e.key === "Enter" && !e.shiftKey && !e.altKey && !e.metaKey && !e.ctrlKey){
      e.preventDefault();
      addManualBookmark();
    }
    if (e.key === "Escape"){
      setBookmarkAnchor("");
      if (e.target && typeof e.target.blur === "function") e.target.blur();
    }
  }

  function onPromptBlur(){
    const prompt = roots.bookmarkprompt;
    const text = prompt && (prompt.innerText != null ? prompt.innerText : prompt.textContent || "");
    if (!String(text || "").trim()) setBookmarkAnchor("");
  }

  function onChipXClick(){
    setBookmarkAnchor("");
    if (roots.bookmarkprompt && typeof roots.bookmarkprompt.focus === "function")
      roots.bookmarkprompt.focus();
  }

  /* The one send-to dialogue. Both entry points — a chat bubble's "send to…"
     and a bookmark long-press — land here, so the target filter and order can
     never drift apart between them. Only the sheet title differs. */
  function openChoiceList({ title = "Choose…", choices = [], onChoose } = {}){
    const listEl = d.sendtoList || (doc && doc.querySelector && doc.querySelector("#sendto_list"));
    if (!listEl) return;
    const titleEl = d.sendtoTitle || (doc && doc.querySelector && doc.querySelector("#sendto_title"));
    if (titleEl) titleEl.textContent = title;
    listEl.innerHTML = choices.map((choice, i) =>
      `<button type="button" class="pos-item" data-choice="${i}" style="width:100%;text-align:left">${esc(choice.label || "Destination")}</button>`
    ).join("");
    if (sendtoListWithHandler && sendtoListWithHandler !== listEl &&
        sendtoListWithHandler.onclick === sendtoClickHandler)
      sendtoListWithHandler.onclick = null;
    sendtoClickHandler = ev => {
      const button = ev.target.closest && ev.target.closest("[data-choice]");
      if (!button) return;
      const choice = choices[Number(button.dataset.choice)];
      if (typeof d.closeSheets === "function") d.closeSheets();
      if (choice && typeof onChoose === "function") onChoose(choice);
    };
    listEl.onclick = sendtoClickHandler;
    sendtoListWithHandler = listEl;
    if (typeof d.openSheet === "function") d.openSheet("#sendto");
  }

  function openSendTo({ text = "", exceptId = "", title = "", role = "", source = null } = {}){
    /* Disclose once, here, rather than at each of the four callers: the two
       branches below (seed a new chat, merge into a live draft) both carry the
       text onward, and a caller that forgot would fail silently. */
    const body = discloseAgentOutput(text, role);
    const lm = typeof d.laneModel === "function" ? d.laneModel() : { color: () => "" };
    const nodes = typeof d.orderedNodes === "function" ? d.orderedNodes() : [];
    const pins = typeof d.pinned === "function" ? d.pinned() : [];
    const listEl = d.sendtoList || (doc && doc.querySelector && doc.querySelector("#sendto_list"));
    if (!listEl) return;
    const titleEl = d.sendtoTitle ||
      (doc && doc.querySelector && doc.querySelector("#sendto_title"));
    if (titleEl && title) titleEl.textContent = title;
    const groups = sendableNodes(nodes, { exceptId, pinned: pins });
    const item = n => sendToItemHTML(n, {
      esc,
      laneColor: id => lm.color(id),
      agentLogo: d.agentLogo,
    });
    /* Label the blocks only when both exist: with one block the order needs no
       explanation, with two an unlabelled split looks arbitrary. */
    const labelled = groups.pinned.length > 0 && groups.recent.length > 0;
    const section = (label, list) =>
      (list.length && labelled ? `<div class="sendto-group">${label}</div>` : "") +
      list.map(item).join("");
    const targets = section("Pinned", groups.pinned) + section("Recent", groups.recent);
    /* Always offer "Start new chat…" — including when nothing live can receive.
       Distinct data-newchat (never data-fwd) so the handler cannot treat it as
       a node id. No parent/lineage: openNewActivity clears ncParent. */
    const newChat =
      `<div class="sendto-new" role="separator"></div>` +
      `<button type="button" class="pos-item" data-newchat style="width:100%;text-align:left">` +
      `Start new chat\u2026</button>`;
    listEl.innerHTML = targets + newChat;
    if (sendtoListWithHandler && sendtoListWithHandler !== listEl &&
        sendtoListWithHandler.onclick === sendtoClickHandler)
      sendtoListWithHandler.onclick = null;
    sendtoClickHandler = ev => {
      /* data-newchat before data-fwd — a shared attribute would open a phantom chat. */
      const startNew = ev.target.closest && ev.target.closest("[data-newchat]");
      if (startNew){
        if (typeof d.closeSheets === "function") d.closeSheets();
        if (typeof d.openNewActivity === "function")
          d.openNewActivity({ prompt: body, focusTitle: true, forwardSources: source ? [source] : [] });
        if (typeof d.isDesktop === "function" && !d.isDesktop() && typeof d.setLevel === "function")
          d.setLevel(1);
        return;
      }
      const b = ev.target.closest && ev.target.closest("[data-fwd]");
      if (!b) return;
      const tgt = b.dataset.fwd;
      const prev = (storage && storage.getItem(DRAFT_KEY_PREFIX + tgt)) || "";
      if (storage)
        storage.setItem(DRAFT_KEY_PREFIX + tgt, mergeBookmarkIntoDraft(prev, body));
      if (source) addPendingForward(storage, tgt, source);
      if (typeof d.closeSheets === "function") d.closeSheets();
      if (typeof d.select === "function") d.select(tgt);
      if (typeof d.isDesktop === "function" && !d.isDesktop() && typeof d.setLevel === "function")
        d.setLevel(1);
      /* Draft is already in storage (above) and select() has run
         composer.onSelect → setPromptText, so #prompt holds the merged text
         before we place the caret. focusAtEnd on empty content is a no-op
         lookalike of bare focus. */
      const prompt = d.chatPrompt || (doc && doc.querySelector && doc.querySelector("#prompt"));
      if (prompt) focusAtEnd(prompt);
    };
    listEl.onclick = sendtoClickHandler;
    sendtoListWithHandler = listEl;
    if (typeof d.openSheet === "function") d.openSheet("#sendto");
  }

  function openBookmarkOverflow(trigger, nt, context){
    if (!nt) return;
    menuBookmarkT = nt.t;
    menuContext = context || "pane";
    const panel = roots.bookmarkspane || (doc && doc.body);
    popMenu.open({
      panel,
      anchor: trigger,
      trigger,
      offset: 40,
      className: "popmenu",
      html: bookmarkMenuHTML(nt, { icons, context: menuContext }),
      onClick: (ev) => {
        const btn = ev.target.closest && ev.target.closest("[data-bmact]");
        if (!btn) return;
        /* The popover lives inside #bookmarkspane, which carries the delegated
           [data-bmact] handler, so this click would reach onPaneClick a second
           time by bubbling — and menuBookmarkT still resolves the same
           bookmark, so the action would run twice. Stop here and dispatch
           once, deliberately. (Notes' popover goes to #wspanel, which is not
           an ancestor of its list handlers, so it has no such seam.) */
        if (typeof ev.stopPropagation === "function") ev.stopPropagation();
        /* Re-dispatch through the pane handler with the stored target so
           menu items reuse data-bmact dispatch without living inside .bookmark. */
        onPaneClick({ target: btn, preventDefault(){}, stopPropagation(){} });
      },
    });
  }

  /** Confirm before deleting from the flat toolbar (HIG; no window.confirm —
      blocked in iOS PWA). Reuses the openDeleteConfirm shell (popMenu,
      className "popmenu wsconfirm", offset 40, data-wconfirm). */
  function openDeleteBookmarkConfirm(anchor, nt){
    if (!nt) return;
    menuBookmarkT = nt.t;
    const panel = roots.bookmarkspane || (doc && doc.body);
    popMenu.open({
      panel,
      anchor: anchor || null,
      trigger: anchor || null,
      offset: 40,
      className: "popmenu wsconfirm",
      html:
        `<div class="wsconfirmmsg">Delete bookmark?</div>` +
        `<button type="button" data-wconfirm="cancel">Cancel</button>` +
        `<button type="button" data-wconfirm="ok" class="danger">Delete</button>`,
      onClick: ev => {
        const btn = ev.target.closest && ev.target.closest("[data-wconfirm]");
        if (!btn) return;
        popMenu.close();
        if (btn.dataset.wconfirm !== "ok") return;
        openBookmarkT = "";
        if (typeof d.uiMutate === "function") d.uiMutate({ k: "bookmark-del", t: nt.t });
      },
    });
  }

  function onDocClick(e){
    if (popMenu.shouldCloseForClick(e.target, {
      exclude: '[data-bmact="more"], [data-bmact="del"]',
    }))
      popMenu.close();
  }

  function listen(target, type, fn, opts){
    if (!target || typeof target.addEventListener !== "function") return;
    target.addEventListener(type, fn, opts);
    cleanups.push(() => {
      if (typeof target.removeEventListener === "function")
        target.removeEventListener(type, fn, opts);
    });
  }

  function initIcons(){
    if (roots.bookmarksend)
      roots.bookmarksend.innerHTML = icons.ICON_SEND || "";
  }

  function bind(){
    if (bound) return;
    bound = true;
    initIcons();
    /* initial chrome from restored open state */
    if (doc && doc.body && doc.body.classList)
      doc.body.classList.toggle("bookmarks-open", bookmarksOpen);
    renderBookmarksToggle();

    listen(roots.bookmarksbtn, "click", onToggleClick);
    listen(roots.bookmarksback, "click", onBackClick);
    listen(roots.notesbtn, "click", onNotesClick);
    listen(roots.bookmarkpeek, "click", onPeekClick);
    listen(roots.bookmarkflags, "click", onFlagsClick);
    listen(roots.bookmarkspane, "click", onPaneClick);
    listen(roots.bookmarksend, "click", onSendClick);
    listen(roots.bookmarkprompt, "keydown", onPromptKeydown);
    listen(roots.bookmarkprompt, "blur", onPromptBlur);
    listen(roots.bookmarkchipx, "click", onChipXClick);
    if (doc) listen(doc, "click", onDocClick);
  }

  function destroy(){
    while (cleanups.length){
      try { cleanups.pop()(); } catch { /* ignore */ }
    }
    try { popMenu.close(); } catch { /* ignore */ }
    menuBookmarkT = "";
    if (copyTimer){
      clearTimeoutFn(copyTimer);
      copyTimer = null;
    }
    if (sendtoListWithHandler && sendtoListWithHandler.onclick === sendtoClickHandler)
      sendtoListWithHandler.onclick = null;
    sendtoListWithHandler = null;
    sendtoClickHandler = null;
    bound = false;
  }

  return {
    bind,
    destroy,
    render,
    setOpen,
    isOpen,
    invalidate,
    pinBottom,
    setBackLabel,
    jumpToChatAddress,
    openBookmark,
    openChoiceList,
    openSendTo,
    setBookmarkAnchor,
    addManualBookmark,
  };
}
