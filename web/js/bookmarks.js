/* Bookmarks feature: pane, flags, clamp, composer, jump, longpress send-to.
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
 *   - Longpress send-to draft merge + sheet/navigation
 *
 * Timers / clipboard / scrolling / layout seams:
 *   - copy acknowledgement 900ms timer (restores ICON_COPY; clears sig)
 *   - copyText injected (secure clipboard or execCommand fallback)
 *   - post-render scrollTop = scrollHeight when bookmarksPin
 *   - open action row scrollIntoView nearest/smooth after unfold
 *   - body.bookmarks-open class toggle
 *
 * Injected node / lane / chat / Notes / sheet / navigation effects:
 *   - nodeById, orderedNodes, laneList, laneColor, laneModel
 *   - uiMutate, bookmarks getter (UI.bookmarks)
 *   - jump deps: setPendingJump, invalidateChat, select, openArchived,
 *     wsOpen, closeWorkspace, isDesktop
 *   - startPlacement (Notes use-in-note — Notes owns workspace; 7F)
 *   - openNotes lazy (Notes › / workspace)
 *   - openSheet / closeSheets, setLevel, restartWorkPulse
 *   - toast, copyText, longpress helper, prompt focus targets
 *   - storage (localStorage), document, setTimeout/clearTimeout, CSS.escape
 *   - esc, md, fmtWhen, hashStr, icons
 *
 * bind / destroy lifecycle:
 *   - createBookmarksFeature(deps) → { bind, destroy, render, setOpen,
 *     isOpen, invalidate, pinBottom, setBackLabel, jumpToChatAddress }
 *   - bind() is idempotent; destroy() removes every owned listener and
 *     longpress disposer exactly once; shell DOM roots stay in place
 *
 * Explicit non-ownership:
 *   - Notes workspace / inbox / reference projection / bookmarkSnapshot /
 *     bookmarkSource / startPlacement body (Packet 7F)
 *   - Document swipe decisions, #scrim, overlayOwned snapshot (shell /
 *     navigation.js)
 *   - Search, archived openArchived implementation, map station capture
 *     (addStationBookmark shell), chat bubble capture (chat.js)
 *   - Composer #promptbar (composer.js), generic sheets, polling tick body
 *   - Shared longpress helper implementation (shell injects)
 *   - state.js applyOp bookmark-add/del (reducer stays shared)
 *
 * Reuses (no algorithm duplication):
 *   format.js: esc, md, fmtWhen
 *   lanes.js: hashStr
 *
 * Pre-existing bug fixed in 7E (reported separately):
 *   Longpress resolved `bookmark` but checked/used undeclared `note`.
 *   Intended contract: use `bookmark` for the guard and draft text.
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

import { esc as escDefault, md as mdDefault, fmtWhen as fmtWhenDefault } from "./format.js";
import { hashStr as hashStrDefault } from "./lanes.js";

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
    nodeById, esc, md, fmtWhen, laneColor, icons = {}, workspaceOpen = false,
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
    return `
    <div class="bookmark ${nt.anchor ? "comment" : ""}" data-t="${e(nt.t)}">
      <div class="when">${e(when(nt.t))}${src}</div>
      <div class="nbubble" style="border-left-color:${color}">${renderMd(nt.text || "")}</div>
      <button class="nmore" data-nmore hidden></button>
      ${openBookmarkT === nt.t
        ? bookmarkActionsHTML(nt, { icons, context: "pane", workspaceOpen }) : ""}
    </div>`;
  }).join("");
}

/* The one action row a bookmark gets, wherever it is shown (UI review items
   5, 6, 8, 9). Both the compact Bookmarks pane and the Notes workspace inbox
   render this, so the two can no longer drift apart in geometry, order, or
   which actions exist at all — the inbox used to offer only "Use in note",
   which meant reaching the chat required first placing the bookmark into a
   section. `context` withholds exactly one action: comment is pane-only,
   because its reply composer (#bookmarkprompt) lives only there.

   "use in note" is withheld from the pane while the Notes workspace is closed
   (review 2, item 4): it arms placement mode, whose only targets are the
   section rows inside that workspace, so from the compact pane it armed a mode
   with nothing visible to complete it. The inbox lives inside the workspace and
   therefore always offers it. */
export function bookmarkActionsHTML(nt, opts){
  const { icons = {}, context = "pane", workspaceOpen = false } = opts || {};
  const canPlace = context === "inbox" || workspaceOpen;
  const b = (act, label, icon, extra = "") =>
    `<button class="btn-plain${extra}" data-bmact="${act}" aria-label="${label}">${icon || ""}</button>`;
  return `<div class="actionbar">
        ${nt.node || nt.uid ? b("jump", "open in activity", icons.ICON_JUMP) : ""}
        ${context === "pane" && nt.node && !nt.anchor
          ? b("comment", "comment on this bookmark", icons.ICON_COMMENT) : ""}
        ${b("copy", "copy bookmark", icons.ICON_COPY)}
        ${canPlace ? b("note", "use in note", icons.ICON_CLIP) : ""}
        ${b("del", "delete bookmark", icons.ICON_TRASH, " danger")}
      </div>`;
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

/** Longpress send-to: merge bookmark text into an existing per-node draft. */
export function mergeBookmarkIntoDraft(prev, text){
  return (prev ? prev + "\n\n" : "") + (text || "");
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
  let longpressDispose = null;
  let copyTimer = null;
  let sendtoListWithHandler = null;
  let sendtoClickHandler = null;

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
      /* "use in note" only exists while its placement targets do (item 4) */
      workspaceOpen: typeof d.wsOpen === "function" && d.wsOpen(),
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
      if (typeof d.openArchived === "function")
        d.openArchived(decision.uid, decision.segment, decision.record, decision.turnTime || "");
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
      const nt = bookmarks().find(x => x.t === act.closest(".bookmark")?.dataset.t);
      if (!nt) return;
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
        setBookmarkAnchor(nt.t);
        openBookmarkT = "";
        render();
        if (roots.bookmarkprompt && typeof roots.bookmarkprompt.focus === "function")
          roots.bookmarkprompt.focus();
      } else if (act.dataset.bmact === "copy"){
        if (typeof d.copyText === "function") d.copyText(nt.text || "");
        act.innerHTML = "&#10003;";
        if (copyTimer) clearTimeoutFn(copyTimer);
        copyTimer = setTimeoutFn(() => {
          act.innerHTML = icons.ICON_COPY || "";
          bookmarksSig = "";
          copyTimer = null;
        }, COPY_ACK_MS);
      } else if (act.dataset.bmact === "del"){
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

  function onLongpress(el){
    const bookmark = bookmarks().find(x => x.t === el.dataset.t);
    /* Pre-existing defect fixed in 7E: production resolved `bookmark` but
       checked/used undeclared `note`. Intended contract uses `bookmark`. */
    if (!bookmark) return;
    const lm = typeof d.laneModel === "function" ? d.laneModel() : { color: () => "" };
    const nodes = typeof d.orderedNodes === "function" ? d.orderedNodes() : [];
    const listEl = d.sendtoList || (doc && doc.querySelector && doc.querySelector("#sendto_list"));
    if (!listEl) return;
    listEl.innerHTML = nodes.map(n => `
    <button class="pos-item" data-fwd="${esc(n.id)}" style="width:100%;text-align:left">
      <span style="width:12px;height:12px;border-radius:6px;flex:none;align-self:center;background:${esc(lm.color(n.lane_id))}"></span>
      <span>${esc(n.title)}</span>
    </button>`).join("") || `<div style="color:var(--dim);font-size:14px">no activities yet</div>`;
    if (sendtoListWithHandler && sendtoListWithHandler !== listEl &&
        sendtoListWithHandler.onclick === sendtoClickHandler)
      sendtoListWithHandler.onclick = null;
    sendtoClickHandler = ev => {
      const b = ev.target.closest && ev.target.closest("[data-fwd]");
      if (!b) return;
      const tgt = b.dataset.fwd;
      const prev = (storage && storage.getItem(DRAFT_KEY_PREFIX + tgt)) || "";
      if (storage)
        storage.setItem(DRAFT_KEY_PREFIX + tgt, mergeBookmarkIntoDraft(prev, bookmark.text || ""));
      if (typeof d.closeSheets === "function") d.closeSheets();
      if (typeof d.select === "function") d.select(tgt);
      if (typeof d.isDesktop === "function" && !d.isDesktop() && typeof d.setLevel === "function")
        d.setLevel(1);
      const prompt = d.chatPrompt || (doc && doc.querySelector && doc.querySelector("#prompt"));
      if (prompt && typeof prompt.focus === "function") prompt.focus();
    };
    listEl.onclick = sendtoClickHandler;
    sendtoListWithHandler = listEl;
    if (typeof d.openSheet === "function") d.openSheet("#sendto");
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

    if (typeof d.longpress === "function" && roots.bookmarklist){
      longpressDispose = d.longpress(roots.bookmarklist, ".bookmark", onLongpress);
    }
  }

  function destroy(){
    while (cleanups.length){
      try { cleanups.pop()(); } catch { /* ignore */ }
    }
    if (typeof longpressDispose === "function"){
      try { longpressDispose(); } catch { /* ignore */ }
      longpressDispose = null;
    }
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
    setBookmarkAnchor,
    addManualBookmark,
  };
}
