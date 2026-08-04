/* Notes workspace feature: open/close, inbox, cards, active note, saves,
 * sections, references, placement.
 *
 * Packet 7F ownership inventory
 * -----------------------------
 * Owned DOM roots / controls (all under #notesworkspace):
 *   - #notesworkspace (dialog overlay; .note-open / .placing classes)
 *   - #wsscrim / #wspanel / #wstopbar / #wstitle / #wsback / #wsclose
 *   - #wsplacebar / #wsplacetext / #wsplacecancel
 *   - #wsinboxtabs / #wsinboxlist (mirrored Bookmarks inbox; polled safe)
 *   - #wscards / #wsnewnote (sparse navigator; drag reorder)
 *   - #wssections / #wsnoteempty (active note; title lives only in #wstitle)
 *
 * Explicit non-ownership:
 *   - #notesbtn (Bookmarks-owned; lazy openNotes → this.open)
 *   - Bookmarks pane/list/flags/jump implementation (bookmarks.js)
 *   - Search, archived, generic sheets, polling tick body, top-level app
 *     gestures / #scrim / immutable overlayOwned snapshot (shell)
 *   - format.js escaping/Markdown/time, navigation.js swipe decisions,
 *     api.js request decoding, bookmarks.js pure lane/sort/clamp
 *
 * Persistent state (localStorage, exact keys):
 *   - "scimux-notefolds"   — { [secId]: 1 } fold map
 *   - "scimux-wsinboxtab"  — active inbox tab id or "GENERAL"
 *
 * Ephemeral view state (never ui.json):
 *   - wsNotes / wsActiveId / wsActive / wsReturnFocus
 *   - wsSaveTimers / wsPending / wsSaveChains (per-field serialization)
 *   - wsInboxSig / wsInboxTab / expandedWsInbox / expandedRefs
 *   - wsCardDragging / wsCardDragEl / wsTouch / wsPlacing
 *   - wsSecTitleOrig / wsMenuEl
 *
 * Server calls (via injected api):
 *   - GET    /api/notes
 *   - POST   /api/notes
 *   - GET    /api/notes/:id
 *   - PATCH  /api/notes/:id   (title, section, add_section, order)
 *   - DELETE /api/notes/:id
 *   - POST   /api/notes/:id/sections/:sid/references
 *   - DELETE /api/notes/:id/sections/:sid/references/:rid
 *
 * Save timers / serialization:
 *   - Debounce 1100ms per field key (sectitle-*, secbody-*); note title is
 *     card-rename with an immediate PATCH (no debounce key)
 *   - Per-key promise chain: issue order always; Escape reverts enqueue after
 *     in-flight; close flushes every pending debounce immediately
 *   - DOM text is authoritative while saves are pending; save response only
 *     folds edited_at/title/section_count into card meta (never rebuilds
 *     focused editors)
 *
 * Bookmark / reference inputs:
 *   - UI.bookmarks via injected bookmarks() for inbox + placement snapshot
 *   - Pure bookmarkLaneId / bookmarkSortKey / bookmarkClampState from
 *     bookmarks.js (no Bookmarks mutable state)
 *   - jumpToChatAddress injected from Bookmarks feature
 *
 * Injected Bookmarks / navigation / shell effects:
 *   - nodeById, laneList, laneColor, toast, copyText, jumpToChatAddress
 *   - captureNotesTouchStart / notesSwipeBackDecision (navigation.js)
 *   - isNarrow (matchMedia max-width 767px), requestAnimationFrame, focus
 *   - notesbtn focus fallback on close
 *
 * Listener / dynamic-editor lifecycle:
 *   - bind() idempotent; owns every #ws* control listener + document menu
 *     closer + workspace touch/keydown
 *   - destroy() removes bound listeners, clears save timers/pending, closes
 *     menu, ends placement; dynamic body-edit / menu handlers leave with DOM
 *
 * Contracts preserved:
 *   - Editors never rebuilt by polling (renderWsNote only on explicit actions)
 *   - Section move uses visual sorted order; no boundary wrap
 *   - Body edit restores via sectionBodyInner (references survive)
 *   - Placement snapshots self-contained; durable source + orphan fallback
 *   - Adding a reference prefers targeted DOM append on the section
 *   - Exact IDs, classes, ARIA, data attributes, payloads, statuses, keys
 *
 * Line-count exception: complete Notes workspace (inbox + cards + active note
 * + saves + placement + menus) lives in one factory so one owner owns the
 * non-clobber and save-serialization invariants. Pure decisions are exported
 * above the factory and unit-tested without DOM.
 *
 * Reuses (no algorithm duplication):
 *   format.js: esc, md, fmtWhen, fmtNoteMeta
 *   bookmarks.js: bookmarkLaneId, bookmarkSortKey, bookmarkClampState
 *   navigation.js: captureNotesTouchStart, notesSwipeBackDecision
 *   lanes.js: hashStr
 *   api.js: injected request behavior
 *
 * bookmarks.js must not import this module.
 */

import {
  esc as escDefault,
  md as mdDefault,
  fmtWhen as fmtWhenDefault,
  fmtNoteMeta as fmtNoteMetaDefault,
} from "./format.js";
import { hashStr as hashStrDefault } from "./lanes.js";
import {
  bookmarkLaneId as bookmarkLaneIdDefault,
  bookmarkSortKey as bookmarkSortKeyDefault,
  bookmarkClampState as bookmarkClampStateDefault,
} from "./bookmarks.js";
import {
  captureNotesTouchStart as captureNotesTouchStartDefault,
  notesSwipeBackDecision as notesSwipeBackDecisionDefault,
} from "./navigation.js";

/* ---------- public constants ---------- */

export const SAVE_DEBOUNCE_MS = 1100;
export const STORAGE_KEY_FOLDS = "scimux-notefolds";
export const STORAGE_KEY_INBOX_TAB = "scimux-wsinboxtab";
export const COPY_ACK_MS = 900;

/* ---------- pure: folds ---------- */

/* parseNoteFolds reads the fold map; corrupt JSON → {}. */
export function parseNoteFolds(raw){
  try { return JSON.parse(raw || "{}") || {}; } catch { return {}; }
}

/* foldsAfterSet returns a new fold map with secId set (1) or cleared. */
export function foldsAfterSet(folds, secId, folded){
  const f = Object.assign({}, folds || {});
  if (folded) f[secId] = 1; else delete f[secId];
  return f;
}

/* ---------- pure: provenance / section move / card reorder ---------- */

/* provLabel joins station · speaker · when, dropping empties. */
export function provLabel(station, speaker, when){
  return [station, speaker, when].filter(Boolean).join(" · ");
}

/* sectionSwapPlan: visual sorted order; null at boundaries (no wrap). */
export function sectionSwapPlan(sections, id, dir){
  const sorted = (sections || []).slice().sort((a, b) => (a.order || 0) - (b.order || 0));
  const vi = sorted.findIndex(s => s.id === id);
  if (vi < 0) return null;
  const j = vi + (dir === "up" ? -1 : 1);
  if (j < 0 || j >= sorted.length) return null;
  return { a: sorted[vi].id, ao: (sorted[j].order || 0), b: sorted[j].id, bo: (sorted[vi].order || 0) };
}

/* noteCardReorderPlan aligns model cards to DOM id order; patches changed orders. */
export function noteCardReorderPlan(cards, orderedIds){
  const byId = {};
  (cards || []).forEach(c => { byId[c.id] = c; });
  const reordered = (orderedIds || []).map(id => byId[id]).filter(Boolean);
  const patches = [];
  reordered.forEach((c, i) => {
    if (c.order !== i) patches.push({ id: c.id, order: i });
  });
  return { cards: reordered, patches };
}

/* cardLanesFromDoc: first-seen lane colors from reference snapshots. */
export function cardLanesFromDoc(doc){
  const seen = {}, lanes = [];
  (doc && doc.sections || []).forEach(s => (s.references || []).forEach(r => {
    const c = r.snapshot && r.snapshot.lane;
    if (c && !seen[c]){ seen[c] = 1; lanes.push(c); }
  }));
  return lanes;
}

/* ---------- pure: reference source / snapshot ---------- */

export function bookmarkSource(nt){
  return {
    uid: (nt && nt.uid) || "",
    segment: (nt && nt.segment) || 0,
    record: (nt && nt.record) || 0,
    node: (nt && nt.node) || "",
    turnTime: (nt && nt.turnTime) || "",
  };
}

/* buildBookmarkSnapshot freezes display fields; comment inherits parent lane. */
export function buildBookmarkSnapshot(nt, deps = {}){
  const bookmarks = typeof deps.bookmarks === "function" ? deps.bookmarks() : (deps.bookmarks || []);
  const nodeById = typeof deps.nodeById === "function" ? deps.nodeById : (() => null);
  const laneColor = typeof deps.laneColor === "function" ? deps.laneColor : (() => "");
  const bookmarkLaneId = deps.bookmarkLaneId || bookmarkLaneIdDefault;
  const byT = {};
  (bookmarks || []).forEach(n => { byT[n.t] = n; });
  const laneId = bookmarkLaneId(nt, byT, nodeById);
  const nd = nt && nt.node ? nodeById(nt.node) : null;
  const speaker = nt && nt.role === "user" ? "you" : nt && nt.role === "assistant" ? "agent" : "";
  return {
    lane: laneId ? laneColor(laneId) : "",
    station: nd ? nd.title : "",
    speaker,
    time: (nt && (nt.turnTime || nt.t)) || "",
    text: (nt && nt.text) || "",
  };
}

/* ---------- pure: save enqueue ---------- */

/* createSaveEnqueue returns { enqueue } chaining per-key runs in issue order. */
export function createSaveEnqueue(){
  const chains = {};
  function enqueue(key, run){
    const prev = chains[key] || Promise.resolve();
    const next = prev.then(run, run);
    chains[key] = next.catch(() => {});
    return next;
  }
  return { enqueue, chains };
}

/* ---------- pure: HTML builders ---------- */

export function sectionBodyInner(s, { md, esc, fmtWhen, icons } = {}){
  const mdFn = md || (t => String(t ?? ""));
  const escFn = esc || (t => String(t ?? ""));
  const whenFn = fmtWhen || (t => String(t ?? ""));
  const ic = icons || {};
  const refs = (s.references || []).map(r => referenceHTML(r, { md: mdFn, esc: escFn, fmtWhen: whenFn, icons: ic })).join("");
  return `<div class="wssecrender" data-secrender>${mdFn(s.body || "")}</div>` +
    (refs ? `<div class="wsrefs">${refs}</div>` : "");
}

export function sectionHTML(s, folded, deps = {}){
  const esc = deps.esc || (t => String(t ?? ""));
  const icons = deps.icons || {};
  return `<div class="wssec ${folded ? "folded" : ""}" data-sec="${esc(s.id)}">
    <div class="wssechead">
      <button class="wssecfold" data-secfold aria-label="fold section">${folded ? (icons.ICON_CHEV_RIGHT || "") : (icons.ICON_CHEV_DOWN || "")}</button>
      <input class="wssectitle" data-sectitle value="${esc(s.title || "")}" aria-label="section title" placeholder="Section title">
      <button class="wsaddhere" data-addhere aria-label="add the held bookmark to this section">${icons.ICON_PLUS || ""}<span>Add bookmark here</span></button>
      <button class="wssecmenu" data-secmenu aria-label="section actions">${icons.ICON_MENU_DOTS || ""}</button>
    </div>
    <div class="wssecbody">${sectionBodyInner(s, deps)}</div>
  </div>`;
}

export function referenceHTML(r, deps = {}){
  const esc = deps.esc || (t => String(t ?? ""));
  const mdFn = deps.md || (t => String(t ?? ""));
  const whenFn = deps.fmtWhen || (t => String(t ?? ""));
  const icons = deps.icons || {};
  const snap = r.snapshot || {};
  const color = snap.lane || "var(--unlane)";
  const label = provLabel(esc(snap.station || ""), esc(snap.speaker || ""),
    snap.time ? esc(whenFn(snap.time)) : "");
  return `<div class="wsref" data-ref="${esc(r.id)}" style="border-left-color:${esc(color)}">
    <div class="wsrefhead"><span class="wsrefdot" style="background:${esc(color)}"></span><span class="wsrefprov">${label}</span></div>
    <div class="wsrefbody">${mdFn(snap.text || "")}</div>
    <button class="wsrefmore" data-refmore hidden></button>
    <div class="wsrefactions">
      <button data-refact="jump" aria-label="jump to chat">${icons.ICON_JUMP || ""}</button>
      <button data-refact="copy" aria-label="copy reference text">${icons.ICON_COPY || ""}</button>
      <button data-refact="trash" aria-label="remove reference">${icons.ICON_TRASH || ""}</button>
    </div>
  </div>`;
}

/** Card stays grab-draggable only when not mid-rename (drag would steal the gesture). */
export function noteCardDragEnabled(renaming){
  return !renaming;
}

/** Commit plan for an inline card title rename. */
export function noteCardRenamePlan({ noteId, activeId, title }){
  const t = title == null ? "" : String(title);
  const isActive = noteId === activeId;
  return {
    patch: { title: t },
    updateTopBar: isActive,
    topBarText: t || "Note",
    updateActive: isActive,
  };
}

/**
 * Keyboard activation for a focused .wscard (div role=button).
 * Enter/Space select the note — same as a native <button> — unless focus is on
 * the rename input, an action control, or the card is mid-rename.
 */
export function noteCardKeySelects({ key, inTitleEdit, onAction, renaming } = {}){
  if (inTitleEdit || onAction || renaming) return false;
  return key === "Enter" || key === " " || key === "Spacebar";
}

export function noteCardHTML(c, activeId, deps = {}){
  const esc = deps.esc || (t => String(t ?? ""));
  const meta = deps.fmtNoteMeta || (() => "");
  const icons = deps.icons || {};
  const lanes = (c.lanes || []).map(col => `<i style="background:${esc(col)}"></i>`).join("");
  /* div (not button) so nested Rename/Delete action buttons are valid HTML. */
  return `<div class="wscard ${c.id === activeId ? "on" : ""}" draggable="true" data-note="${esc(c.id)}" role="button" tabindex="0">
      <div class="wctitle">${esc(c.title || "Untitled")}</div>
      <div class="wcmeta"><span>${esc(meta(c))}</span>${lanes ? `<span class="wclanes">${lanes}</span>` : ""}</div>
      <div class="wscardactions">
        <button type="button" data-wcact="rename" aria-label="rename note">${icons.ICON_PENCIL || ""}</button>
        <button type="button" data-wcact="delete" class="danger" aria-label="delete note">${icons.ICON_TRASH || ""}</button>
      </div>
    </div>`;
}

export function inboxLaneTabs(bookmarks, laneList, bookmarkLaneId, nodeById){
  const byT = {};
  (bookmarks || []).forEach(nt => { byT[nt.t] = nt; });
  const laneOf = {};
  (bookmarks || []).forEach(nt => { laneOf[nt.t] = bookmarkLaneId(nt, byT, nodeById); });
  const tabs = (laneList || []).filter(l => (bookmarks || []).some(nt => laneOf[nt.t] === l.id));
  return { byT, laneOf, tabs };
}

export function resolveInboxTab(tab, tabs){
  if (tab !== "GENERAL" && !(tabs || []).some(l => l.id === tab)) return "GENERAL";
  return tab || "GENERAL";
}

export function inboxListModel(bookmarks, tab, laneOf, byT, bookmarkSortKey){
  const sel = tab === "GENERAL" ? "" : tab;
  return (bookmarks || [])
    .filter(nt => (laneOf[nt.t] || "") === sel)
    .sort((a, b) => bookmarkSortKey(a, byT).localeCompare(bookmarkSortKey(b, byT)));
}

export function inboxTabsHTML(tab, tabs, esc, laneColor){
  return `<button data-wsitab="GENERAL" class="${tab === "GENERAL" ? "on" : ""}">General</button>` +
    (tabs || []).map(l => `<button data-wsitab="${esc(l.id)}" class="${tab === l.id ? "on" : ""}"
      ><span class="tabdot" style="background:${esc(laneColor(l.id))}"></span>${esc(l.name)}</button>`).join("");
}

export function inboxItemHTML(nt, color, deps = {}){
  const esc = deps.esc || (t => String(t ?? ""));
  const mdFn = deps.md || (t => String(t ?? ""));
  const whenFn = deps.fmtWhen || (t => String(t ?? ""));
  const icons = deps.icons || {};
  const nodeById = deps.nodeById || (() => null);
  const nd = nt.node ? nodeById(nt.node) : null;
  const src = nd ? " · " + esc(nd.title) : "";
  return `<div class="wsibookmark" data-t="${esc(nt.t)}">
      <div class="wsiwhen">${esc(whenFn(nt.t))}${src}</div>
      <div class="wsibubble" style="border-left-color:${color}">${mdFn(nt.text || "")}</div>
      <button class="wsimore" data-wsimore hidden></button>
      <button class="wsiuse" data-wsiuse>${icons.ICON_CLIP || ""}<span>Use in note</span></button>
    </div>`;
}

/* narrow swipe action applicator — pure decision → effect type names only. */
export function applyNotesSwipeAction(action){
  if (!action) return null;
  if (action.type === "endPlacement") return "endPlacement";
  if (action.type === "noteToList") return "noteToList";
  if (action.type === "closeWorkspace") return "closeWorkspace";
  return null;
}

/* ---------- factory ---------- */

export function createNotesFeature(deps){
  const d = deps || {};
  const roots = d.roots || {};
  const doc = d.document || (typeof document !== "undefined" ? document : null);
  const storage = d.storage || null;
  const esc = d.esc || escDefault;
  const md = d.md || mdDefault;
  const fmtWhen = d.fmtWhen || fmtWhenDefault;
  const fmtNoteMeta = d.fmtNoteMeta || fmtNoteMetaDefault;
  const hashStr = d.hashStr || hashStrDefault;
  const bookmarkLaneId = d.bookmarkLaneId || bookmarkLaneIdDefault;
  const bookmarkSortKey = d.bookmarkSortKey || bookmarkSortKeyDefault;
  const bookmarkClampState = d.bookmarkClampState || bookmarkClampStateDefault;
  const captureNotesTouchStart = d.captureNotesTouchStart || captureNotesTouchStartDefault;
  const notesSwipeBackDecision = d.notesSwipeBackDecision || notesSwipeBackDecisionDefault;
  const icons = d.icons || {};
  const setTimeoutFn = d.setTimeout || setTimeout;
  const clearTimeoutFn = d.clearTimeout || clearTimeout;
  const rAF = d.requestAnimationFrame || (typeof requestAnimationFrame !== "undefined"
    ? requestAnimationFrame : fn => setTimeoutFn(fn, 0));
  const CSSObj = d.CSS || (typeof CSS !== "undefined" ? CSS : null);
  const api = typeof d.api === "function" ? d.api : async () => null;

  const htmlDeps = () => ({ esc, md, fmtWhen, fmtNoteMeta, icons });

  /* ---- view state ---- */
  let wsNotes = [];
  let wsActiveId = null;
  let wsActive = null;
  let wsReturnFocus = null;
  const wsSaveTimers = {};
  const wsPending = {};
  const { enqueue: wsEnqueue } = createSaveEnqueue();
  let wsInboxSig = "";
  let wsInboxTab = (storage && storage.getItem(STORAGE_KEY_INBOX_TAB)) || "GENERAL";
  const expandedWsInbox = new Set();
  const expandedRefs = new Set();
  let wsCardDragging = false, wsCardDragEl = null;
  let wsTouch = null;
  let wsPlacing = null;
  const wsSecTitleOrig = {};
  let wsMenuEl = null;
  let bound = false;
  const cleanups = [];
  let copyTimer = null;
  const bodyEditCleanups = [];

  function $(sel){
    if (roots[sel]) return roots[sel];
    /* roots keyed by id without # */
    const id = sel.startsWith("#") ? sel.slice(1) : sel;
    if (roots[id]) return roots[id];
    if (doc && typeof doc.querySelector === "function") return doc.querySelector(sel);
    return null;
  }

  function root(name){
    return roots[name] || (doc && doc.getElementById ? doc.getElementById(name) : null);
  }

  function bookmarks(){
    if (typeof d.bookmarks === "function") return d.bookmarks() || [];
    return d.bookmarks || [];
  }
  function nodeById(id){
    return typeof d.nodeById === "function" ? d.nodeById(id) : null;
  }
  function laneList(){
    return typeof d.laneList === "function" ? (d.laneList() || []) : [];
  }
  function laneColor(id){
    return typeof d.laneColor === "function" ? d.laneColor(id) : "";
  }
  function toast(msg){
    if (typeof d.toast === "function") d.toast(msg);
  }
  function copyText(s){
    if (typeof d.copyText === "function") d.copyText(s);
  }
  function jumpToChatAddress(a){
    return typeof d.jumpToChatAddress === "function" ? d.jumpToChatAddress(a) : false;
  }
  function isNarrow(){
    if (typeof d.isNarrow === "function") return !!d.isNarrow();
    if (typeof d.matchMedia === "function")
      return !!d.matchMedia("(max-width: 767px)").matches;
    return false;
  }
  function activeEl(){
    if (typeof d.activeElement === "function") return d.activeElement();
    return doc && doc.activeElement;
  }
  function notesbtn(){
    return typeof d.notesbtn === "function" ? d.notesbtn() : (d.notesbtn || root("notesbtn"));
  }

  function cssEsc(s){
    if (CSSObj && CSSObj.escape) return CSSObj.escape(s);
    return String(s).replace(/["\\]/g, "\\$&");
  }

  function isOpen(){
    const ws = root("notesworkspace");
    return !!(ws && !ws.hidden);
  }

  function wsFolds(){
    try {
      const raw = storage ? storage.getItem(STORAGE_KEY_FOLDS) : null;
      return parseNoteFolds(raw);
    } catch {
      return {};
    }
  }
  function wsSetFold(secId, folded){
    const f = foldsAfterSet(wsFolds(), secId, folded);
    if (!storage) return;
    try { storage.setItem(STORAGE_KEY_FOLDS, JSON.stringify(f)); } catch { /* quota / private mode */ }
  }

  function open(){
    if (isOpen()) return;
    wsReturnFocus = activeEl();
    const ws = root("notesworkspace");
    if (ws) ws.hidden = false;
    wsLoadCards();
    renderInbox();
    rAF(() => {
      const btn = root("wsnewnote");
      if (btn && typeof btn.focus === "function") btn.focus();
    });
  }

  function close(){
    if (!isOpen()) return;
    wsFlushPendingSaves();
    endPlacement();
    const ws = root("notesworkspace");
    if (ws) ws.hidden = true;
    const back = wsReturnFocus; wsReturnFocus = null;
    if (back && doc && typeof doc.contains === "function" && doc.contains(back) && back.focus)
      back.focus();
    else {
      const nb = notesbtn();
      if (nb && typeof nb.focus === "function") nb.focus();
    }
  }

  function endPlacement(){
    wsPlacing = null;
    const ws = root("notesworkspace");
    if (ws && ws.classList) ws.classList.remove("placing");
  }

  function startPlacement(nt){
    if (!nt) return;
    wsPlacing = nt;
    if (!isOpen()) open();
    const ws = root("notesworkspace");
    if (ws && ws.classList) ws.classList.add("placing");
    const txt = root("wsplacetext");
    if (txt) txt.textContent = (nt.text || "").replace(/\s+/g, " ").trim();
  }

  /* --- inbox --- */
  function renderInbox(){
    if (!isOpen()) return;
    const bms = bookmarks().slice();
    const { byT, laneOf, tabs } = inboxLaneTabs(bms, laneList(), bookmarkLaneId, nodeById);
    wsInboxTab = resolveInboxTab(wsInboxTab, tabs);
    const list = inboxListModel(bms, wsInboxTab, laneOf, byT, bookmarkSortKey);
    const tabsHtml = inboxTabsHTML(wsInboxTab, tabs, esc, laneColor);
    const color = wsInboxTab === "GENERAL" ? "var(--unlane)" : esc(laneColor(wsInboxTab));
    const listHtml = list.length
      ? list.map(nt => inboxItemHTML(nt, color, { esc, md, fmtWhen, icons, nodeById })).join("")
      : `<div class="empty">No bookmarks yet — tap a bubble and “bookmark” it.</div>`;
    const sig = hashStr(tabsHtml + "|" + listHtml);
    if (sig === wsInboxSig) return;
    wsInboxSig = sig;
    const tabsEl = root("wsinboxtabs");
    const listEl = root("wsinboxlist");
    if (tabsEl) tabsEl.innerHTML = tabsHtml;
    if (listEl) listEl.innerHTML = listHtml;
    applyWsInboxClamps();
  }

  function applyWsInboxClamps(){
    const listEl = root("wsinboxlist");
    if (!listEl || !listEl.querySelectorAll) return;
    listEl.querySelectorAll(".wsibookmark").forEach(el => {
      const b = el.querySelector(".wsibubble"), btn = el.querySelector(".wsimore");
      if (!b || !btn) return;
      const st = bookmarkClampState(b.scrollHeight, expandedWsInbox.has(el.dataset.t));
      b.classList.toggle("clamped", st.clamped);
      btn.hidden = !st.showBtn;
      btn.textContent = st.label;
    });
  }

  /* --- cards --- */
  async function wsLoadCards(){
    try {
      const resp = await api("/api/notes");
      wsNotes = (resp && resp.notes) || [];
    } catch { wsNotes = []; }
    renderCards();
  }

  function renderCards(){
    if (wsCardDragging) return;
    const host = root("wscards");
    if (!host) return;
    if (!wsNotes.length){
      host.innerHTML = `<div class="empty">No notes yet — make one with +</div>`;
      return;
    }
    host.innerHTML = wsNotes.map(c => noteCardHTML(c, wsActiveId, htmlDeps())).join("");
  }

  async function wsCreate(){
    try {
      const sh = await api("/api/notes", { method: "POST" });
      await wsLoadCards();
      wsSelect(sh.id);
    } catch { toast("Couldn't create note"); }
  }

  async function wsSelect(id){
    try {
      wsActive = await api("/api/notes/" + encodeURIComponent(id));
    } catch { toast("Couldn't open note"); return; }
    wsActiveId = id;
    const ws = root("notesworkspace");
    if (ws && ws.classList) ws.classList.add("note-open");
    const back = root("wsback");
    if (back) back.hidden = false;
    renderNote();
    renderCards();
  }

  function renderNote(){
    const empty = root("wsnoteempty"), secWrap = root("wssections");
    const h2 = root("wstitle");
    if (!wsActive){
      if (empty) empty.hidden = false;
      if (secWrap) secWrap.innerHTML = "";
      if (h2) h2.textContent = "Notes";
      return;
    }
    if (empty) empty.hidden = true;
    if (h2) h2.textContent = wsActive.title || "Note";
    const folds = wsFolds();
    const secs = (wsActive.sections || []).slice().sort((a, b) => (a.order || 0) - (b.order || 0));
    if (secWrap){
      if (!secs.length){
        /* zero sections: centred empty-state variant (button markup inlined so
           characterization tests can see data-addsection inside .wssecempty) */
        secWrap.innerHTML =
          `<div class="wssecempty"><button type="button" class="wssecaddempty" data-addsection>${icons.ICON_PLUS || ""}<span>Add section</span></button></div>`;
      } else {
        /* always-present trailing control — not only when empty (Phase 1f) */
        secWrap.innerHTML = secs.map(s => sectionHTML(s, !!folds[s.id], htmlDeps())).join("") +
          `<div class="wssecaddtrail"><button type="button" class="wssecaddempty" data-addsection>${icons.ICON_PLUS || ""}<span>Add section</span></button></div>`;
      }
      applyRefClamps(secWrap);
    }
  }

  function applyRefClamps(scope){
    const base = scope || doc;
    if (!base || !base.querySelectorAll) return;
    base.querySelectorAll(".wsref").forEach(el => {
      const b = el.querySelector(".wsrefbody"), btn = el.querySelector(".wsrefmore");
      if (!b || !btn) return;
      const st = bookmarkClampState(b.scrollHeight, expandedRefs.has(el.dataset.ref));
      b.classList.toggle("clamped", st.clamped);
      btn.hidden = !st.showBtn;
      btn.textContent = st.label;
    });
  }

  /* --- persistence --- */
  function wsSend(body){
    return api("/api/notes/" + encodeURIComponent(wsActiveId),
      { method: "PATCH", body: JSON.stringify(body) })
      .then(doc => {
        if (doc && doc.id === wsActiveId){
          if (wsActive) wsActive.edited_at = doc.edited_at;
          const card = wsNotes.find(c => c.id === doc.id);
          if (card){
            card.edited_at = doc.edited_at; card.title = doc.title;
            card.section_count = (doc.sections || []).length; renderCards();
          }
        }
        return doc;
      })
      .catch(() => { toast("Save failed — will retry on next edit"); });
  }

  function wsPatch(body, key){
    if (key){
      clearTimeoutFn(wsSaveTimers[key]);
      const fire = () => wsEnqueue(key, () => wsSend(body));
      wsPending[key] = fire;
      return new Promise(res => {
        wsSaveTimers[key] = setTimeoutFn(() => { delete wsPending[key]; res(fire()); }, SAVE_DEBOUNCE_MS);
      });
    }
    return wsSend(body);
  }
  function wsCancelPending(key){ clearTimeoutFn(wsSaveTimers[key]); delete wsPending[key]; }
  function wsPatchNow(body, key){ wsCancelPending(key); return wsEnqueue(key, () => wsSend(body)); }
  function wsFlushPendingSaves(){
    Object.keys(wsSaveTimers).forEach(k => clearTimeoutFn(wsSaveTimers[k]));
    const pend = Object.values(wsPending);
    for (const k in wsPending) delete wsPending[k];
    pend.forEach(fn => { try { fn(); } catch { /* ignore */ } });
  }

  function syncCardMeta(doc){
    const card = wsNotes.find(c => c.id === doc.id);
    if (!card) return;
    card.edited_at = doc.edited_at; card.title = doc.title;
    card.section_count = (doc.sections || []).length;
    card.lanes = cardLanesFromDoc(doc);
    renderCards();
  }

  async function wsAddSection(){
    const doc = await wsPatch({ add_section: "" });
    if (doc){ wsActive = doc; renderNote(); }
  }

  async function wsDeleteNote(noteId){
    const id = noteId || wsActiveId;
    if (!id) return;
    try { await api("/api/notes/" + encodeURIComponent(id), { method: "DELETE" }); }
    catch { toast("Delete failed"); return; }
    if (id === wsActiveId){
      wsActiveId = null; wsActive = null;
      const ws = root("notesworkspace");
      if (ws && ws.classList) ws.classList.remove("note-open");
      const back = root("wsback");
      if (back) back.hidden = true;
      renderNote();
    }
    await wsLoadCards();
  }

  /** In-workspace confirm before archive-delete (no window.confirm — blocked in iOS PWA). */
  function openDeleteConfirm(anchor, noteId){
    if (!doc || !noteId) return;
    closeWsMenu();
    const m = doc.createElement("div");
    m.className = "wsmenu wsconfirm";
    m.innerHTML =
      `<div class="wsconfirmmsg">Delete note? It moves to the archive.</div>` +
      `<button type="button" data-wconfirm="cancel">Cancel</button>` +
      `<button type="button" data-wconfirm="ok" class="danger">Delete</button>`;
    const panel = root("wspanel");
    if (panel) panel.appendChild(m);
    const tgt = anchor || null;
    const r = tgt && tgt.getBoundingClientRect
      ? tgt.getBoundingClientRect() : { bottom: 0, left: 0 };
    const pr = panel && panel.getBoundingClientRect
      ? panel.getBoundingClientRect() : { top: 0, left: 0, width: 400 };
    m.style.top = (r.bottom - pr.top + 4) + "px";
    m.style.left = Math.max(8, Math.min(r.left - pr.left - 40, pr.width - 190)) + "px";
    wsMenuEl = m;
    m.addEventListener("click", async ev => {
      const btn = ev.target.closest && ev.target.closest("[data-wconfirm]");
      if (!btn) return;
      closeWsMenu();
      if (btn.dataset.wconfirm === "ok") await wsDeleteNote(noteId);
    });
  }

  function startCardRename(card){
    if (!card || !doc || card.classList.contains("renaming")) return;
    const noteId = card.dataset.note;
    const titleEl = card.querySelector(".wctitle");
    if (!titleEl || !noteId) return;
    const cardMeta = wsNotes.find(c => c.id === noteId);
    const orig = (cardMeta && cardMeta.title) || titleEl.textContent || "";
    card.classList.add("renaming");
    card.draggable = noteCardDragEnabled(true); /* false while renaming */
    const input = doc.createElement("input");
    input.className = "wctitleedit";
    input.value = orig;
    input.setAttribute("aria-label", "rename note");
    if (titleEl.parentNode){
      titleEl.parentNode.insertBefore(input, titleEl);
      titleEl.remove();
    }
    if (typeof input.focus === "function") input.focus();
    if (typeof input.select === "function") input.select();
    let done = false;
    const finish = async (commit) => {
      if (done) return;
      done = true;
      const next = commit ? input.value : orig;
      const plan = noteCardRenamePlan({ noteId, activeId: wsActiveId, title: next });
      try {
        if (commit && next !== orig){
          const docu = await api("/api/notes/" + encodeURIComponent(noteId),
            { method: "PATCH", body: JSON.stringify(plan.patch) });
          const cardRow = wsNotes.find(c => c.id === noteId);
          if (cardRow) cardRow.title = plan.patch.title;
          if (plan.updateActive && wsActive) wsActive.title = plan.patch.title;
          if (plan.updateTopBar){
            const h2 = root("wstitle");
            if (h2) h2.textContent = plan.topBarText;
          }
          if (docu && docu.edited_at && cardRow) cardRow.edited_at = docu.edited_at;
        }
      } catch { toast("Save failed — will retry on next edit"); }
      card.classList.remove("renaming");
      card.draggable = noteCardDragEnabled(false); /* true again */
      /* rebuild this card's title node; full renderCards is fine (not mid-drag) */
      renderCards();
    };
    const onKey = ev => {
      if (ev.key === "Enter"){
        ev.preventDefault();
        finish(true);
      } else if (ev.key === "Escape"){
        ev.preventDefault();
        ev.stopPropagation();
        finish(false);
      }
    };
    const onBlur = () => { finish(true); };
    input.addEventListener("keydown", onKey);
    input.addEventListener("blur", onBlur);
  }

  function closeWsMenu(){
    if (wsMenuEl){ wsMenuEl.remove(); wsMenuEl = null; }
  }

  function openSectionMenu(btn){
    closeWsMenu();
    const secId = btn.closest(".wssec").dataset.sec;
    const m = doc.createElement("div");
    m.className = "wsmenu";
    m.innerHTML =
      `<button data-mi="add">${icons.ICON_PLUS || ""}<span>Add section below</span></button>` +
      `<button data-mi="edit">${icons.ICON_PENCIL || ""}<span>Edit</span></button>` +
      `<button data-mi="up">${icons.ICON_MOVE_UP || ""}<span>Move up</span></button>` +
      `<button data-mi="down">${icons.ICON_MOVE_DOWN || ""}<span>Move down</span></button>` +
      `<div class="sep"></div>` +
      `<button data-mi="del" class="danger">${icons.ICON_TRASH || ""}<span>Delete section</span></button>`;
    const panel = root("wspanel");
    if (panel) panel.appendChild(m);
    const r = btn.getBoundingClientRect(), pr = panel ? panel.getBoundingClientRect() : { top: 0, left: 0, width: 400 };
    m.style.top = (r.bottom - pr.top + 4) + "px";
    m.style.left = Math.max(8, Math.min(r.left - pr.left - 150, pr.width - 190)) + "px";
    wsMenuEl = m;
    m.addEventListener("click", async ev => {
      const mi = ev.target.closest("[data-mi]"); if (!mi) return;
      closeWsMenu();
      await sectionMenuAction(mi.dataset.mi, secId);
    });
  }

  async function sectionMenuAction(action, secId){
    const secs = (wsActive && wsActive.sections) || [];
    const idx = secs.findIndex(s => s.id === secId);
    if (idx < 0) return;
    if (action === "edit"){
      const secWrap = root("wssections");
      const sec = secWrap && secWrap.querySelector
        ? secWrap.querySelector(`.wssec[data-sec="${cssEsc(secId)}"]`) : null;
      if (sec){ sec.classList.remove("folded"); wsSetFold(secId, false); startBodyEdit(sec); }
      return;
    }
    if (action === "add"){ await wsAddSection(); return; }
    if (action === "del"){
      const doc = await wsPatch({ section: { id: secId, delete: true } });
      if (doc){ wsActive = doc; renderNote(); } return;
    }
    if (action === "up" || action === "down"){
      const plan = sectionSwapPlan(secs, secId, action);
      if (!plan) return;
      const a = secs.find(s => s.id === plan.a), b = secs.find(s => s.id === plan.b);
      a.order = plan.ao; b.order = plan.bo;
      await Promise.all([
        wsPatch({ section: { id: a.id, order: a.order } }),
        wsPatch({ section: { id: b.id, order: b.order } }),
      ]);
      renderNote();
    }
  }

  function startBodyEdit(sec){
    const secId = sec.dataset.sec;
    const s = wsActive && (wsActive.sections || []).find(x => x.id === secId);
    if (!s) return;
    const orig = s.body || "";
    const bodyWrap = sec.querySelector(".wssecbody");
    if (!bodyWrap || !doc) return;
    const ta = doc.createElement("textarea");
    ta.className = "wssecedit"; ta.value = orig; ta.setAttribute("aria-label", "section body (Markdown)");
    bodyWrap.innerHTML = ""; bodyWrap.appendChild(ta);
    const grow = () => {
      ta.style.height = "auto";
      ta.style.height = (ta.scrollHeight + ta.offsetHeight - ta.clientHeight) + "px";
    };
    if (typeof ta.focus === "function") ta.focus();
    grow();
    let cancelled = false;
    const onInput = () => {
      grow(); s.body = ta.value;
      wsPatch({ section: { id: secId, body: ta.value } }, "secbody-" + secId);
    };
    const onKey = ev => {
      if (ev.key !== "Escape") return;
      ev.preventDefault(); ev.stopPropagation();
      cancelled = true;
      s.body = orig;
      wsPatchNow({ section: { id: secId, body: orig } }, "secbody-" + secId);
      bodyWrap.innerHTML = sectionBodyInner(s, htmlDeps());
      applyRefClamps(bodyWrap);
    };
    const onBlur = async () => {
      if (cancelled) return;
      s.body = ta.value;
      await wsPatchNow({ section: { id: secId, body: ta.value } }, "secbody-" + secId);
      bodyWrap.innerHTML = sectionBodyInner(s, htmlDeps());
      applyRefClamps(bodyWrap);
    };
    ta.addEventListener("input", onInput);
    ta.addEventListener("keydown", onKey);
    ta.addEventListener("blur", onBlur);
    bodyEditCleanups.push(() => {
      try { ta.removeEventListener("input", onInput); } catch { /* ignore */ }
      try { ta.removeEventListener("keydown", onKey); } catch { /* ignore */ }
      try { ta.removeEventListener("blur", onBlur); } catch { /* ignore */ }
    });
  }

  function findRef(secId, refId){
    const sec = wsActive && (wsActive.sections || []).find(s => s.id === secId);
    return sec && (sec.references || []).find(r => r.id === refId);
  }

  async function referenceAction(btn){
    const refEl = btn.closest(".wsref"), secEl = btn.closest(".wssec");
    const secId = secEl.dataset.sec, refId = refEl.dataset.ref;
    const ref = findRef(secId, refId);
    if (!ref) return;
    const act = btn.dataset.refact;
    if (act === "copy"){
      copyText((ref.snapshot && ref.snapshot.text) || "");
      btn.innerHTML = "&#10003;";
      if (copyTimer) clearTimeoutFn(copyTimer);
      copyTimer = setTimeoutFn(() => { btn.innerHTML = icons.ICON_COPY || ""; }, COPY_ACK_MS);
      return;
    }
    if (act === "jump"){
      const src = ref.source || {};
      if (!jumpToChatAddress({ node: src.node, uid: src.uid, segment: src.segment,
          record: src.record, turnTime: src.turnTime, text: (ref.snapshot || {}).text }))
        toast("Source unavailable — the captured snapshot is still shown here.");
      return;
    }
    if (act === "trash"){
      try {
        const docu = await api("/api/notes/" + encodeURIComponent(wsActiveId) +
          "/sections/" + encodeURIComponent(secId) + "/references/" + encodeURIComponent(refId),
          { method: "DELETE" });
        if (docu && docu.id === wsActiveId){ wsActive = docu; syncCardMeta(docu); }
      } catch { toast("Couldn't remove reference"); return; }
      const wrap = refEl.parentElement;
      refEl.remove();
      if (wrap && wrap.classList && wrap.classList.contains("wsrefs") && !wrap.children.length)
        wrap.remove();
    }
  }

  async function placeHeldBookmark(secId){
    if (!wsPlacing || !wsActiveId) return;
    const body = {
      source: bookmarkSource(wsPlacing),
      snapshot: buildBookmarkSnapshot(wsPlacing, {
        bookmarks, nodeById, laneColor, bookmarkLaneId,
      }),
    };
    let docu;
    try {
      docu = await api("/api/notes/" + encodeURIComponent(wsActiveId) +
        "/sections/" + encodeURIComponent(secId) + "/references",
        { method: "POST", body: JSON.stringify(body) });
    } catch { toast("Couldn't add to note"); return; }
    endPlacement();
    if (docu && docu.id === wsActiveId){
      wsActive = docu; syncCardMeta(docu);
      const sec = (docu.sections || []).find(s => s.id === secId);
      const secWrap = root("wssections");
      const secEl = secWrap && secWrap.querySelector
        ? secWrap.querySelector(`.wssec[data-sec="${cssEsc(secId)}"]`) : null;
      const newRef = sec && (sec.references || [])[sec.references.length - 1];
      if (secEl && newRef){
        let wrap = secEl.querySelector(".wsrefs");
        if (!wrap){
          wrap = doc.createElement("div"); wrap.className = "wsrefs";
          const bodyEl = secEl.querySelector(".wssecbody");
          if (bodyEl) bodyEl.appendChild(wrap);
        }
        if (wrap.insertAdjacentHTML)
          wrap.insertAdjacentHTML("beforeend", referenceHTML(newRef, htmlDeps()));
        else if (wrap)
          wrap.innerHTML = (wrap.innerHTML || "") + referenceHTML(newRef, htmlDeps());
        applyRefClamps(secEl);
      } else {
        renderNote();
      }
      toast("Added to " + (docu.title || "note"));
    }
  }

  /* --- event handlers --- */
  function onCloseClick(){ close(); }
  function onBackClick(){
    const ws = root("notesworkspace");
    if (ws && ws.classList) ws.classList.remove("note-open");
    const back = root("wsback");
    if (back) back.hidden = true;
  }
  function onNewNote(){ wsCreate(); }
  function onPlaceCancel(){ endPlacement(); }

  function onWorkspaceKeydown(e){
    if (e.key === "Escape"){
      e.preventDefault(); e.stopPropagation();
      if (wsPlacing){ endPlacement(); return; }
      const ws = root("notesworkspace");
      if (ws && ws.classList && ws.classList.contains("note-open") && isNarrow()){
        const back = root("wsback");
        if (back && typeof back.click === "function") back.click();
        else onBackClick();
      } else close();
    }
  }

  function onWorkspaceTouchStart(e){
    const t = e.touches && e.touches[0];
    if (!t) return;
    const target = e.target;
    const inEditable = !!(target && target.closest && target.closest("textarea, input"));
    wsTouch = captureNotesTouchStart({
      x: t.clientX, y: t.clientY, inEditable,
    });
  }

  function onWorkspaceTouchEnd(e){
    const start = wsTouch; wsTouch = null;
    if (!start) return;
    const t = e.changedTouches && e.changedTouches[0];
    if (!t) return;
    const ws = root("notesworkspace");
    const action = notesSwipeBackDecision(start, { x: t.clientX, y: t.clientY }, {
      isNarrow: isNarrow(),
      placing: !!wsPlacing,
      noteOpen: !!(ws && ws.classList && ws.classList.contains("note-open")),
    });
    const kind = applyNotesSwipeAction(action);
    if (!kind) return;
    if (kind === "endPlacement") endPlacement();
    else if (kind === "noteToList"){
      const back = root("wsback");
      if (back && typeof back.click === "function") back.click();
      else onBackClick();
    } else if (kind === "closeWorkspace") close();
  }

  function onInboxTabsClick(e){
    const tb = e.target.closest && e.target.closest("[data-wsitab]");
    if (!tb) return;
    wsInboxTab = tb.dataset.wsitab;
    if (storage) storage.setItem(STORAGE_KEY_INBOX_TAB, wsInboxTab);
    wsInboxSig = "";
    renderInbox();
  }

  function onInboxListClick(e){
    const more = e.target.closest && e.target.closest("[data-wsimore]");
    if (more){
      const el = more.closest(".wsibookmark"), t = el.dataset.t;
      expandedWsInbox.has(t) ? expandedWsInbox.delete(t) : expandedWsInbox.add(t);
      const b = el.querySelector(".wsibubble");
      const st = bookmarkClampState(b.scrollHeight, expandedWsInbox.has(t));
      b.classList.toggle("clamped", st.clamped);
      more.textContent = st.label;
      return;
    }
    const use = e.target.closest && e.target.closest("[data-wsiuse]");
    if (!use) return;
    const t = use.closest(".wsibookmark").dataset.t;
    const nt = bookmarks().find(x => x.t === t);
    if (nt) startPlacement(nt);
  }

  function onCardsClick(e){
    const act = e.target.closest && e.target.closest("[data-wcact]");
    if (act){
      if (typeof e.stopPropagation === "function") e.stopPropagation();
      if (typeof e.preventDefault === "function") e.preventDefault();
      const card = act.closest(".wscard");
      if (!card) return;
      if (act.dataset.wcact === "rename") startCardRename(card);
      else if (act.dataset.wcact === "delete") openDeleteConfirm(act, card.dataset.note);
      return;
    }
    if (e.target.closest && e.target.closest(".wctitleedit")) return;
    const card = e.target.closest && e.target.closest(".wscard");
    if (card && !card.classList.contains("renaming")) wsSelect(card.dataset.note);
  }
  function onCardsKeydown(e){
    const tgt = e.target;
    const card = tgt && tgt.closest && tgt.closest(".wscard");
    if (!card) return;
    const inTitleEdit = !!(tgt.closest && tgt.closest(".wctitleedit"));
    const onAction = !!(tgt.closest && tgt.closest("[data-wcact]"));
    if (!noteCardKeySelects({
      key: e.key,
      inTitleEdit,
      onAction,
      renaming: card.classList.contains("renaming"),
    })) return;
    if (typeof e.preventDefault === "function") e.preventDefault();
    wsSelect(card.dataset.note);
  }
  function onCardsDragStart(e){
    const card = e.target.closest && e.target.closest(".wscard");
    if (!card) return;
    if (!noteCardDragEnabled(card.classList.contains("renaming")) || card.draggable === false){
      if (typeof e.preventDefault === "function") e.preventDefault();
      return;
    }
    wsCardDragging = true; wsCardDragEl = card;
    if (card.classList) card.classList.add("dragging");
    try {
      if (e.dataTransfer){
        e.dataTransfer.effectAllowed = "move";
        e.dataTransfer.setData("text/plain", "");
      }
    } catch { /* ignore */ }
  }
  function onCardsDragOver(e){
    if (!wsCardDragging || !wsCardDragEl) return;
    e.preventDefault();
    const over = e.target.closest && e.target.closest(".wscard");
    if (!over || over === wsCardDragEl) return;
    const r = over.getBoundingClientRect();
    const after = e.clientY > r.top + r.height / 2;
    if (over.parentNode && over.parentNode.insertBefore)
      over.parentNode.insertBefore(wsCardDragEl, after ? over.nextSibling : over);
  }
  function onCardsDrop(e){ if (wsCardDragging) e.preventDefault(); }
  async function onCardsDragEnd(){
    if (!wsCardDragging) return;
    const host = root("wscards");
    const ids = host && host.querySelectorAll
      ? [...host.querySelectorAll(".wscard")].map(c => c.dataset.note) : [];
    if (wsCardDragEl && wsCardDragEl.classList) wsCardDragEl.classList.remove("dragging");
    wsCardDragEl = null; wsCardDragging = false;
    const plan = noteCardReorderPlan(wsNotes, ids);
    wsNotes = plan.cards;
    await Promise.all(plan.patches.map(async p => {
      const c = wsNotes.find(x => x.id === p.id);
      if (c) c.order = p.order;
      try {
        await api("/api/notes/" + encodeURIComponent(p.id),
          { method: "PATCH", body: JSON.stringify({ order: p.order }) });
      } catch { /* ignore */ }
    }));
    renderCards();
  }

  function onSectionsClick(e){
    const addSec = e.target.closest && e.target.closest("[data-addsection]");
    if (addSec){ wsAddSection(); return; }
    const addHere = e.target.closest && e.target.closest("[data-addhere]");
    if (addHere){ placeHeldBookmark(addHere.closest(".wssec").dataset.sec); return; }
    const refMore = e.target.closest && e.target.closest("[data-refmore]");
    if (refMore){
      const el = refMore.closest(".wsref"), id = el.dataset.ref;
      expandedRefs.has(id) ? expandedRefs.delete(id) : expandedRefs.add(id);
      const b = el.querySelector(".wsrefbody");
      const st = bookmarkClampState(b.scrollHeight, expandedRefs.has(id));
      b.classList.toggle("clamped", st.clamped);
      refMore.textContent = st.label;
      return;
    }
    const refBtn = e.target.closest && e.target.closest("[data-refact]");
    if (refBtn){ referenceAction(refBtn); return; }
    const refEl = e.target.closest && e.target.closest(".wsref");
    if (refEl){ refEl.classList.toggle("open"); return; }
    const foldBtn = e.target.closest && e.target.closest("[data-secfold]");
    if (foldBtn){
      const sec = foldBtn.closest(".wssec");
      const folded = sec.classList.toggle("folded");
      wsSetFold(sec.dataset.sec, folded);
      foldBtn.innerHTML = folded ? (icons.ICON_CHEV_RIGHT || "") : (icons.ICON_CHEV_DOWN || "");
      return;
    }
    const menuBtn = e.target.closest && e.target.closest("[data-secmenu]");
    if (menuBtn){ openSectionMenu(menuBtn); return; }
    const render = e.target.closest && e.target.closest("[data-secrender]");
    if (render && !render.closest(".wssec").classList.contains("folded")){
      startBodyEdit(render.closest(".wssec"));
    }
  }

  function onSectionsInput(e){
    const t = e.target.closest && e.target.closest("[data-sectitle]");
    if (t){
      const secId = t.closest(".wssec").dataset.sec;
      const s = wsActive && (wsActive.sections || []).find(x => x.id === secId);
      if (s) s.title = t.value;
      wsPatch({ section: { id: secId, title: t.value } }, "sectitle-" + secId);
    }
  }
  function onSectionsFocusIn(e){
    const t = e.target.closest && e.target.closest("[data-sectitle]");
    if (t){ wsSecTitleOrig[t.closest(".wssec").dataset.sec] = t.value; }
  }
  function onSectionsKeydown(e){
    const t = e.target.closest && e.target.closest("[data-sectitle]");
    if (!t) return;
    if (e.key === "Enter"){ e.preventDefault(); t.blur(); return; }
    if (e.key === "Escape"){
      e.preventDefault(); e.stopPropagation();
      const secId = t.closest(".wssec").dataset.sec;
      const s = wsActive && (wsActive.sections || []).find(x => x.id === secId);
      const orig = wsSecTitleOrig[secId];
      if (s && orig != null){
        t.value = orig; s.title = orig;
        wsPatchNow({ section: { id: secId, title: orig } }, "sectitle-" + secId);
      }
      t.blur();
    }
  }

  function onDocClick(e){
    if (wsMenuEl && e.target && !e.target.closest(".wsmenu") && !e.target.closest("[data-secmenu]"))
      closeWsMenu();
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
    const nn = root("wsnewnote");
    if (nn) nn.innerHTML = icons.ICON_PLUS || "";
  }

  function bind(){
    if (bound) return;
    bound = true;
    initIcons();
    const ws = root("notesworkspace");
    listen(root("wsclose"), "click", onCloseClick);
    listen(root("wsscrim"), "click", onCloseClick);
    listen(root("wsback"), "click", onBackClick);
    listen(root("wsnewnote"), "click", onNewNote);
    listen(root("wsplacecancel"), "click", onPlaceCancel);
    listen(ws, "keydown", onWorkspaceKeydown);
    listen(ws, "touchstart", onWorkspaceTouchStart, { passive: true });
    listen(ws, "touchend", onWorkspaceTouchEnd, { passive: true });
    listen(root("wsinboxtabs"), "click", onInboxTabsClick);
    listen(root("wsinboxlist"), "click", onInboxListClick);
    listen(root("wscards"), "click", onCardsClick);
    listen(root("wscards"), "keydown", onCardsKeydown);
    listen(root("wscards"), "dragstart", onCardsDragStart);
    listen(root("wscards"), "dragover", onCardsDragOver);
    listen(root("wscards"), "drop", onCardsDrop);
    listen(root("wscards"), "dragend", onCardsDragEnd);
    listen(root("wssections"), "click", onSectionsClick);
    listen(root("wssections"), "input", onSectionsInput);
    listen(root("wssections"), "focusin", onSectionsFocusIn);
    listen(root("wssections"), "keydown", onSectionsKeydown);
    listen(doc, "click", onDocClick);
  }

  function destroy(){
    while (cleanups.length){
      try { cleanups.pop()(); } catch { /* ignore */ }
    }
    while (bodyEditCleanups.length){
      try { bodyEditCleanups.pop()(); } catch { /* ignore */ }
    }
    Object.keys(wsSaveTimers).forEach(k => clearTimeoutFn(wsSaveTimers[k]));
    for (const k in wsSaveTimers) delete wsSaveTimers[k];
    for (const k in wsPending) delete wsPending[k];
    if (copyTimer){ clearTimeoutFn(copyTimer); copyTimer = null; }
    closeWsMenu();
    endPlacement();
    bound = false;
  }

  function invalidateInbox(){ wsInboxSig = ""; }

  return {
    bind,
    destroy,
    open,
    close,
    isOpen,
    startPlacement,
    endPlacement,
    renderInbox,
    invalidateInbox,
  };
}
