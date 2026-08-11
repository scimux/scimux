/* Notes workspace feature: open/close, inbox, cards, active note, saves,
 * sections, references, placement.
 *
 * Packet 7F ownership inventory
 * -----------------------------
 * Owned DOM roots / controls (all under #notesworkspace):
 *   - #notesworkspace (dialog overlay; .note-open / .placing classes)
 *   - #wsscrim / #wspanel / #wstopbar / #wstitle / #wsback / #wsclose
 *   - #wsplacepill / #wsplacetext / #wsplacecancel
 *   - #wsinboxtabs / #wsinboxlist (mirrored Bookmarks inbox; polled safe)
 *   - #wscards / #wsnewnote (sparse navigator; drag reorder)
 *   - #wszones / .wsdivider / #wsinbox / #wsnav / #wsnote (resizable columns)
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
 *   - the shared bookmark lane tab (bookmarks.js STORAGE_KEY_TAB): the
 *     compact pane and this inbox are one concept to the user, so they are
 *     one key — opening the workspace lands on the tab you were reading
 *   - "scimux-wslayout"    — { inboxW, navW, inboxCollapsed, navCollapsed }
 *
 * Ephemeral view state (never ui.json):
 *   - wsNotes / wsActiveId / wsActive / wsReturnFocus
 *   - wsSaveTimers / wsPending / wsSaveChains (per-field serialization)
 *   - wsInboxSig / wsInboxTab / expandedWsInbox / expandedRefs
 *   - wsCardDragging / wsCardDragEl / wsTouch / wsPlacing
 *   - wsSecTitleOrig / popMenu (shared popover slot)
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
  stripAssetRefs,
} from "./format.js";
import { hashStr as hashStrDefault } from "./lanes.js";
import {
  STORAGE_KEY_TAB,
  bookmarkLaneId as bookmarkLaneIdDefault,
  bookmarkSortKey as bookmarkSortKeyDefault,
  bookmarkClampState as bookmarkClampStateDefault,
  bookmarkActionsHTML as bookmarkActionsHTMLDefault,
  bookmarkMenuHTML as bookmarkMenuHTMLDefault,
} from "./bookmarks.js";
import {
  captureNotesTouchStart as captureNotesTouchStartDefault,
  notesSwipeBackDecision as notesSwipeBackDecisionDefault,
} from "./navigation.js";
import { focusAtEnd } from "./caret.js";
import {
  createPopoverMenu,
  menuButtonHTML,
  menuSepHTML,
} from "./menu.js";

/* ---------- public constants ---------- */

export const SAVE_DEBOUNCE_MS = 1100;
export const STORAGE_KEY_FOLDS = "scimux-notefolds";
/* The inbox does not own a tab key: it shares the Bookmarks pane's, so
   opening the workspace from the pane lands on the lane you were reading
   (UI review item 17). Re-exported under the old name only so callers that
   ask "which key is the inbox tab in" keep getting a truthful answer. */
export const STORAGE_KEY_INBOX_TAB = STORAGE_KEY_TAB;
export const STORAGE_KEY_WS_LAYOUT = "scimux-wslayout";
export const COPY_ACK_MS = 900;
/* Default mins match the clamp() lower ends in notes.css @media 768px;
 * peek matches the practical midpoint of --peek (clamp(44px, 12%, 60px)).
 *
 * Phone/tablet (<900px) keep inbox: 210 — that is the CSS clamp lower bound
 * and the overflow more-menu bar fits there.
 *
 * Desktop (≥900px) raises the inbox floor so the flat P5 action bar is never
 * clipped by #wsinboxlist { overflow-x: hidden }. Arithmetic from real CSS:
 *   flat row  = 5 × 44px buttons + 4 × 4px gap          = 236px
 *   chrome    = list pad 10+10 + border 1+1 + bar pad 6+6 =  34px
 *               (#wsinboxlist padding, .wsibookmark border, .wsibookmark .actionbar)
 *   zone min  = 236 + 34                                  = 270px
 * Persisted widths from an iPad drag at 210 must be raised on restore too —
 * a drag-only clamp misses that path (P5b). */
export const WS_LAYOUT_MINS = { inbox: 210, nav: 190, note: 280 };
export const WS_INBOX_FLAT_ROW_PX = 5 * 44 + 4 * 4;
export const WS_INBOX_FLAT_CHROME_PX = 10 + 10 + 1 + 1 + 6 + 6;
export const WS_INBOX_FLAT_ZONE_MIN = WS_INBOX_FLAT_ROW_PX + WS_INBOX_FLAT_CHROME_PX;
export const WS_LAYOUT_PEEK = 48;
export const WS_DIVIDER_NUDGE = 20;

/** Mins for dividerResizePlan / restore. Desktop raises inbox to the flat-row
 *  zone floor; phone keeps the CSS 210 clamp lower bound. */
export function wsLayoutMins({ isDesktop = false } = {}){
  return {
    inbox: isDesktop
      ? Math.max(WS_LAYOUT_MINS.inbox, WS_INBOX_FLAT_ZONE_MIN)
      : WS_LAYOUT_MINS.inbox,
    nav: WS_LAYOUT_MINS.nav,
    note: WS_LAYOUT_MINS.note,
  };
}

/** Raise a persisted expanded inboxW that would clip the flat bar. Collapsed
 *  rows keep their last width (peek is applied separately). Does not touch nav. */
export function clampWorkspaceLayout(layout, mins){
  const L = layout && typeof layout === "object"
    ? {
      inboxW: layout.inboxW == null ? null : layout.inboxW,
      navW: layout.navW == null ? null : layout.navW,
      inboxCollapsed: !!layout.inboxCollapsed,
      navCollapsed: !!layout.navCollapsed,
    }
    : { inboxW: null, navW: null, inboxCollapsed: false, navCollapsed: false };
  const floor = mins && Number.isFinite(Number(mins.inbox)) ? Number(mins.inbox) : 0;
  if (!L.inboxCollapsed && L.inboxW != null && floor > 0 && L.inboxW < floor)
    L.inboxW = floor;
  return L;
}

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

/* ---------- pure: workspace layout + divider resize (Phase 2) ---------- */

/** Default persisted layout: null widths → CSS clamp() fallbacks. */
export const DEFAULT_WS_LAYOUT = {
  inboxW: null,
  navW: null,
  inboxCollapsed: false,
  navCollapsed: false,
};

function wsPosNum(v){
  const n = Number(v);
  return Number.isFinite(n) && n > 0 ? n : null;
}

function wsNum(v, fallback){
  const n = Number(v);
  return Number.isFinite(n) ? n : fallback;
}

/* parseWorkspaceLayout reads {inboxW,navW,inboxCollapsed,navCollapsed};
 * absent/garbage → defaults (null widths = CSS clamp). */
export function parseWorkspaceLayout(raw){
  const def = {
    inboxW: null, navW: null,
    inboxCollapsed: false, navCollapsed: false,
  };
  try {
    const o = JSON.parse(raw == null || raw === "" ? "null" : raw);
    if (!o || typeof o !== "object" || Array.isArray(o)) return def;
    return {
      inboxW: wsPosNum(o.inboxW),
      navW: wsPosNum(o.navW),
      inboxCollapsed: !!o.inboxCollapsed,
      navCollapsed: !!o.navCollapsed,
    };
  } catch {
    return def;
  }
}

/* layoutAfterPlan merges a dividerResizePlan result into the persisted shape.
 * last.* stores the most recent non-collapsed width for restore. */
export function layoutAfterPlan(layout, plan){
  const prev = layout && typeof layout === "object" ? layout : DEFAULT_WS_LAYOUT;
  if (!plan || typeof plan !== "object"){
    return {
      inboxW: wsPosNum(prev.inboxW),
      navW: wsPosNum(prev.navW),
      inboxCollapsed: !!prev.inboxCollapsed,
      navCollapsed: !!prev.navCollapsed,
    };
  }
  const last = plan.last || {};
  const col = plan.collapsed || {};
  return {
    inboxW: wsPosNum(last.inbox) ?? wsPosNum(prev.inboxW),
    navW: wsPosNum(last.nav) ?? wsPosNum(prev.navW),
    inboxCollapsed: !!col.inbox,
    navCollapsed: !!col.nav,
  };
}

/**
 * dividerResizePlan — pure reallocation + collapse-to-peek for #wszones.
 *
 * boundary "inbox": divider between inbox|nav; pointerX = target inbox width
 *   (absolute from zones left). Reallocates those two; note stays put when
 *   possible, but yields down to mins.note so a collapsed peer can restore.
 * boundary "nav": divider between nav|note; pointerX = absolute x of the
 *   divider (inbox + nav). Note never collapses and always keeps ≥ mins.note.
 *
 * Collapsed collapsible zones snap to `peek` (always tappable). Both may be
 * collapsed at once; neither may go to width 0.
 *
 * @returns {{widths:{inbox,nav,note}, collapsed:{inbox,nav}, last:{inbox,nav}}}
 */
export function dividerResizePlan(p = {}){
  const mins = {
    inbox: Math.max(0, wsNum(p.mins && p.mins.inbox, 0)),
    nav: Math.max(0, wsNum(p.mins && p.mins.nav, 0)),
    note: Math.max(0, wsNum(p.mins && p.mins.note, 0)),
  };
  const peek = Math.max(0, wsNum(p.peek, 0));
  const z = {
    inbox: Math.max(0, wsNum(p.zones && p.zones.inbox, 0)),
    nav: Math.max(0, wsNum(p.zones && p.zones.nav, 0)),
    note: Math.max(0, wsNum(p.zones && p.zones.note, 0)),
  };
  const total = Math.max(0, wsNum(p.total, z.inbox + z.nav + z.note));
  let colI = !!(p.collapsed && p.collapsed.inbox);
  let colN = !!(p.collapsed && p.collapsed.nav);
  let lastI = wsPosNum(p.last && p.last.inbox);
  let lastN = wsPosNum(p.last && p.last.nav);
  if (lastI == null) lastI = colI ? mins.inbox : (z.inbox || mins.inbox);
  if (lastN == null) lastN = colN ? mins.nav : (z.nav || mins.nav);
  if (lastI < mins.inbox) lastI = mins.inbox;
  if (lastN < mins.nav) lastN = mins.nav;

  let inboxW = colI ? peek : z.inbox;
  let navW = colN ? peek : z.nav;
  let noteW = Math.max(z.note, mins.note);

  const boundary = p.boundary;
  const px = wsNum(p.pointerX, NaN);

  function finish(){
    /* hard floors: collapsible zones never vanish; note never below min */
    if (colI) inboxW = peek; else if (inboxW < mins.inbox) inboxW = mins.inbox;
    if (colN) navW = peek; else if (navW < mins.nav) navW = mins.nav;
    if (noteW < mins.note) noteW = mins.note;
    /* re-normalize sum → total, preferring to adjust note then the non-primary */
    let sum = inboxW + navW + noteW;
    if (sum !== total){
      noteW += total - sum;
      if (noteW < mins.note){
        const d = mins.note - noteW;
        noteW = mins.note;
        if (boundary === "nav" && !colN) navW = Math.max(colN ? peek : mins.nav, navW - d);
        else if (!colI) inboxW = Math.max(colI ? peek : mins.inbox, inboxW - d);
        else if (!colN) navW = Math.max(mins.nav, navW - d);
        /* final sum fix */
        const s2 = inboxW + navW + noteW;
        if (s2 !== total) noteW += total - s2;
      }
    }
    if (!colI) lastI = Math.max(inboxW, mins.inbox);
    if (!colN) lastN = Math.max(navW, mins.nav);
    return {
      widths: { inbox: inboxW, nav: navW, note: noteW },
      collapsed: { inbox: colI, nav: colN },
      last: { inbox: lastI, nav: lastN },
    };
  }

  if (boundary !== "inbox" && boundary !== "nav"){
    return {
      widths: { inbox: z.inbox, nav: z.nav, note: z.note },
      collapsed: { inbox: colI, nav: colN },
      last: { inbox: lastI, nav: lastN },
    };
  }

  if (boundary === "inbox"){
    /* pointerX = target inbox width (zones-left origin) */
    let want = Number.isFinite(px) ? px : inboxW;
    if (colI){
      if (want < mins.inbox){
        inboxW = peek;
      } else {
        colI = false;
        inboxW = Math.max(want, mins.inbox);
      }
    } else if (want < mins.inbox){
      lastI = Math.max(z.inbox, mins.inbox);
      colI = true;
      inboxW = peek;
    } else {
      inboxW = want;
    }

    /* Partner nav + note share the remainder. Prefer keeping note stable;
       if nav is collapsed it stays at peek and note absorbs. */
    const rest = total - inboxW;
    const navFloor = colN ? peek : mins.nav;
    if (colN){
      navW = peek;
      noteW = rest - navW;
      if (noteW < mins.note){
        noteW = mins.note;
        /* cannot grow nav above peek while "collapsed" if note is floored —
           uncollapse only when there's room for mins.nav */
        const room = rest - mins.note;
        if (room >= mins.nav){
          colN = false;
          navW = room;
          noteW = mins.note;
        } else {
          navW = Math.max(peek, room);
          noteW = rest - navW;
          if (noteW < mins.note){ noteW = mins.note; navW = rest - noteW; }
          if (navW >= mins.nav) colN = false;
          else { colN = true; navW = peek; noteW = rest - navW; }
        }
      }
    } else {
      /* keep note as close to current as possible */
      const maxNote = rest - mins.nav;
      const preferNote = Math.min(Math.max(z.note, mins.note), Math.max(mins.note, maxNote));
      noteW = preferNote;
      navW = rest - noteW;
      if (navW < mins.nav){
        lastN = Math.max(z.nav, mins.nav);
        colN = true;
        navW = peek;
        noteW = rest - navW;
        if (noteW < mins.note){
          noteW = mins.note;
          navW = rest - noteW;
          if (navW >= mins.nav) colN = false;
          else { navW = peek; noteW = rest - navW; }
        }
      }
    }
    /* Cap expanded inbox so note ≥ min and nav ≥ floor */
    if (!colI){
      const navFl = colN ? peek : mins.nav;
      const maxInbox = total - mins.note - navFl;
      if (inboxW > maxInbox){
        inboxW = Math.max(mins.inbox, maxInbox);
        const r2 = total - inboxW;
        navW = colN ? peek : Math.max(mins.nav, r2 - mins.note);
        if (!colN && navW < mins.nav){
          colN = true; navW = peek;
        }
        noteW = total - inboxW - navW;
      }
    }
    return finish();
  }

  /* boundary === "nav": pointerX = absolute divider x (= inbox + nav) */
  inboxW = colI ? peek : z.inbox;
  /* Un-cascade: with both collapsed, rightward travel restores in reverse
     collapse order — inbox first, capped at its remembered width — so dragging
     back to where the gesture started returns the layout it started with. */
  if (Number.isFinite(px) && colI && colN){
    const wantInbox = Math.min(px - peek, lastI);
    if (wantInbox >= mins.inbox){ colI = false; inboxW = wantInbox; }
  }
  let wantNav = Number.isFinite(px) ? (px - inboxW) : navW;
  if (colN){
    if (wantNav < mins.nav){
      navW = peek;
    } else {
      colN = false;
      navW = Math.max(wantNav, mins.nav);
    }
  } else if (wantNav < mins.nav){
    lastN = Math.max(z.nav, mins.nav);
    colN = true;
    navW = peek;
  } else {
    navW = wantNav;
  }

  /* Cascade: nav is at peek and the pointer is still travelling left, so the
     inbox gives ground next (and collapses in its turn). Only ever shrinks —
     leftward travel must not widen the inbox. */
  if (colN && !colI && Number.isFinite(px)){
    const wantInbox = px - peek;
    if (wantInbox < inboxW){
      if (wantInbox < mins.inbox){
        lastI = Math.max(z.inbox, mins.inbox);
        colI = true;
        inboxW = peek;
      } else {
        inboxW = wantInbox;
      }
    }
  }

  /* note takes the rest; never below min; never collapsed */
  noteW = total - inboxW - navW;
  if (noteW < mins.note){
    noteW = mins.note;
    navW = total - inboxW - noteW;
    if (colN){
      /* expanding against note floor while collapsed: stay collapsed at peek
         only if nav would be below min; else the clamp un-collapsed us */
      if (navW >= mins.nav){
        colN = false;
      } else {
        navW = peek;
        noteW = total - inboxW - navW;
        /* if still tight, prefer note min and a peek nav */
        if (noteW < mins.note){
          noteW = mins.note;
          navW = total - inboxW - noteW;
          if (navW >= mins.nav) colN = false;
        }
      }
    } else {
      /* expanded nav hit note floor — stay expanded at the max that fits */
      if (navW < mins.nav){
        navW = Math.max(0, total - inboxW - mins.note);
        noteW = total - inboxW - navW;
        /* still expanded (not collapsed): collapse is only for drag-below-min
           on the nav side, not for note-floor pressure from the other side */
        if (navW < mins.nav){
          /* total too small for mins; keep note at min, nav gets remainder */
          noteW = mins.note;
          navW = total - inboxW - noteW;
        }
        colN = false;
      }
    }
  }
  return finish();
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
  const into = icons.ICON_INTO
    ? `<span class="rot180">${icons.ICON_INTO}</span>`
    : "";
  /* P3: jump · copy · sendto · trash (no overflow). data-refmore is the
     text-clamp trigger and must stay a distinct attribute. Remove is inline
     .danger and confirms before deleting (see referenceAction). */
  return `<div class="wsref" data-ref="${esc(r.id)}" style="border-left-color:${esc(color)}">
    <div class="wsrefhead"><span class="wsrefdot" style="background:${esc(color)}"></span><span class="wsrefprov">${label}</span></div>
    <div class="wsrefbody">${mdFn(snap.text || "")}</div>
    <button class="wsrefmore" data-refmore hidden></button>
    <div class="actionbar tear">
      <button class="btn-plain" data-refact="jump" aria-label="jump to chat">${icons.ICON_JUMP || ""}</button>
      <button class="btn-plain" data-refact="copy" aria-label="copy">${icons.ICON_COPY || ""}</button>
      <button class="btn-plain" data-refact="sendto" aria-label="send to\u2026">${into}</button>
      <button class="btn-plain danger" data-refact="trash" aria-label="remove">${icons.ICON_TRASH || ""}</button>
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
      <div class="wscardactions roundactions">
        <button type="button" data-wcact="rename" aria-label="rename note">${icons.ICON_PENCIL || ""}</button>
        <button type="button" class="danger" data-wcact="delete" aria-label="delete note">${icons.ICON_TRASH || ""}</button>
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

/* The inbox bookmark is the same card as the compact pane's, down to the
   action row (UI review item 5). It used to offer only "Use in note", so
   reaching the chat a bookmark came from meant first placing it into a
   section — an extra step that bought the user nothing.
   The row is tap-to-reveal, like the pane's card and the embedded reference:
   three surfaces showing the same bookmark must answer a tap the same way, and
   a card that only *sometimes* responds reads as broken. The extra step item 5
   removed was a mode change (place into a section, then chase the chat); one
   tap that unfolds in place is not that. */
export function inboxItemHTML(nt, color, deps = {}){
  const esc = deps.esc || (t => String(t ?? ""));
  const mdFn = deps.md || (t => String(t ?? ""));
  const whenFn = deps.fmtWhen || (t => String(t ?? ""));
  const icons = deps.icons || {};
  const actions = deps.bookmarkActionsHTML || bookmarkActionsHTMLDefault;
  const nodeById = deps.nodeById || (() => null);
  const nd = nt.node ? nodeById(nt.node) : null;
  const src = nd ? " · " + esc(nd.title) : "";
  return `<div class="wsibookmark" data-t="${esc(nt.t)}">
      <div class="wsiwhen">${esc(whenFn(nt.t))}${src}</div>
      <div class="wsibubble" style="border-left-color:${color}">${mdFn(nt.text || "")}</div>
      <button class="wsimore" data-wsimore hidden></button>
      ${deps.open ? actions(nt, {
        icons, context: "inbox",
        /* P5 width guard: phone keeps the more-menu bar (see bookmarkActionsHTML). */
        overflow: !!deps.overflow,
      }) : ""}
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
  const bookmarkMenuHTML = d.bookmarkMenuHTML || bookmarkMenuHTMLDefault;
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
  let wsInboxTab = (storage && storage.getItem(STORAGE_KEY_TAB)) || "GENERAL";
  /* the one card whose action row is unfolded — state, not a DOM class, because
     the list is polled and rebuilt wholesale whenever its signature changes */
  let wsInboxOpenT = "";
  const expandedWsInbox = new Set();
  const expandedRefs = new Set();
  let wsCardDragging = false, wsCardDragEl = null;
  let wsTouch = null;
  let wsPlacing = null;
  const wsSecTitleOrig = {};
  const popMenu = createPopoverMenu(doc);
  /* overflow-menu target identity when the menu lives outside the card */
  let menuInboxT = "";
  let menuRef = null; /* { secId, refId } */
  let bound = false;
  const cleanups = [];
  let copyTimer = null;
  let wsInboxCopyTimer = null;
  const bodyEditCleanups = [];
  /* Phase 2: device-scoped column layout (localStorage, never poll). */
  let wsLayout = readWsLayoutInitial();
  let wsDrag = null; /* { boundary, pointerId, startX, startZones, startCollapsed, startLast, moved } */
  let wsSuppressClick = false;

  function readWsLayoutInitial(){
    try {
      return parseWorkspaceLayout(storage ? storage.getItem(STORAGE_KEY_WS_LAYOUT) : null);
    } catch {
      return parseWorkspaceLayout(null);
    }
  }
  function readWsLayout(){
    try {
      return parseWorkspaceLayout(storage ? storage.getItem(STORAGE_KEY_WS_LAYOUT) : null);
    } catch {
      return parseWorkspaceLayout(null);
    }
  }
  function persistWsLayout(layout){
    wsLayout = layout || parseWorkspaceLayout(null);
    if (!storage) return;
    try { storage.setItem(STORAGE_KEY_WS_LAYOUT, JSON.stringify(wsLayout)); }
    catch { /* quota / private mode */ }
  }
  function setZoneWidthProp(zonesEl, prop, px, collapsed){
    if (!zonesEl || !zonesEl.style) return;
    if (collapsed || px == null || !(px > 0)){
      if (typeof zonesEl.style.removeProperty === "function")
        zonesEl.style.removeProperty(prop);
      else delete zonesEl.style[prop];
      return;
    }
    const v = Math.round(px) + "px";
    if (typeof zonesEl.style.setProperty === "function")
      zonesEl.style.setProperty(prop, v);
    else zonesEl.style[prop] = v;
  }
  function layoutIsDesktop(){
    return typeof d.isDesktop === "function" ? !!d.isDesktop() : false;
  }
  function currentLayoutMins(){
    return wsLayoutMins({ isDesktop: layoutIsDesktop() });
  }
  /* Apply persisted layout → custom props + .collapsed on zone rails.
     P5b: clamp a too-narrow stored inboxW on desktop so the flat bar fits
     (iPad-at-210 → desktop restore). Write the raised value back so the next
     open does not re-clip. */
  function applyWsLayout(layout){
    const raw = layout || wsLayout || parseWorkspaceLayout(null);
    const mins = currentLayoutMins();
    const L = clampWorkspaceLayout(raw, mins);
    const raised = !L.inboxCollapsed
      && raw && raw.inboxW != null
      && L.inboxW != null
      && L.inboxW !== raw.inboxW;
    wsLayout = L;
    if (raised) persistWsLayout(L);
    const zonesEl = root("wszones");
    const inbox = root("wsinbox");
    const nav = root("wsnav");
    if (zonesEl){
      /* collapsed: CSS .collapsed → var(--peek); still write peek px for measure */
      if (L.inboxCollapsed) setZoneWidthProp(zonesEl, "--wsinbox-w", WS_LAYOUT_PEEK, false);
      else if (L.inboxW) setZoneWidthProp(zonesEl, "--wsinbox-w", L.inboxW, false);
      else setZoneWidthProp(zonesEl, "--wsinbox-w", null, true);
      if (L.navCollapsed) setZoneWidthProp(zonesEl, "--wsnav-w", WS_LAYOUT_PEEK, false);
      else if (L.navW) setZoneWidthProp(zonesEl, "--wsnav-w", L.navW, false);
      else setZoneWidthProp(zonesEl, "--wsnav-w", null, true);
    }
    if (inbox && inbox.classList)
      inbox.classList.toggle("collapsed", !!L.inboxCollapsed);
    if (nav && nav.classList)
      nav.classList.toggle("collapsed", !!L.navCollapsed);
  }
  function applyPlanLive(plan){
    if (!plan || !plan.widths) return;
    const zonesEl = root("wszones");
    const inbox = root("wsinbox");
    const nav = root("wsnav");
    const col = plan.collapsed || {};
    if (zonesEl){
      setZoneWidthProp(zonesEl, "--wsinbox-w", plan.widths.inbox, false);
      setZoneWidthProp(zonesEl, "--wsnav-w", plan.widths.nav, false);
    }
    if (inbox && inbox.classList) inbox.classList.toggle("collapsed", !!col.inbox);
    if (nav && nav.classList) nav.classList.toggle("collapsed", !!col.nav);
    wsLayout = layoutAfterPlan(wsLayout, plan);
  }
  function measureZones(){
    const inbox = root("wsinbox");
    const nav = root("wsnav");
    const note = root("wsnote");
    const empty = { inbox: 0, nav: 0, note: 0 };
    if (!inbox || !nav || !note) return { widths: empty, total: 0 };
    const iw = (inbox.getBoundingClientRect && inbox.getBoundingClientRect().width) || 0;
    const nw = (nav.getBoundingClientRect && nav.getBoundingClientRect().width) || 0;
    const noteW = (note.getBoundingClientRect && note.getBoundingClientRect().width) || 0;
    /* fall back to last known / defaults when rects are zero (hidden / test) */
    const mins = currentLayoutMins();
    const widths = {
      inbox: iw > 0 ? iw : (wsLayout.inboxCollapsed ? WS_LAYOUT_PEEK : (wsLayout.inboxW || mins.inbox)),
      nav: nw > 0 ? nw : (wsLayout.navCollapsed ? WS_LAYOUT_PEEK : (wsLayout.navW || mins.nav)),
      note: noteW > 0 ? noteW : mins.note,
    };
    return { widths, total: widths.inbox + widths.nav + widths.note };
  }
  function planFromPointer(boundary, pointerX, base){
    return dividerResizePlan({
      boundary,
      pointerX,
      zones: base.zones,
      mins: currentLayoutMins(),
      peek: WS_LAYOUT_PEEK,
      total: base.total,
      collapsed: base.collapsed,
      last: base.last,
    });
  }
  function layoutBaseFromCurrent(){
    const m = measureZones();
    const mins = currentLayoutMins();
    return {
      zones: m.widths,
      total: m.total,
      collapsed: {
        inbox: !!(wsLayout && wsLayout.inboxCollapsed),
        nav: !!(wsLayout && wsLayout.navCollapsed),
      },
      last: {
        inbox: (wsLayout && wsLayout.inboxW) || m.widths.inbox || mins.inbox,
        nav: (wsLayout && wsLayout.navW) || m.widths.nav || mins.nav,
      },
    };
  }
  function restoreCollapsedZone(zone){
    if (zone !== "inbox" && zone !== "nav") return;
    const base = layoutBaseFromCurrent();
    if (zone === "inbox" && !base.collapsed.inbox) return;
    if (zone === "nav" && !base.collapsed.nav) return;
    const target = zone === "inbox"
      ? (base.last.inbox || WS_LAYOUT_MINS.inbox)
      : base.zones.inbox + (base.last.nav || WS_LAYOUT_MINS.nav);
    const boundary = zone === "inbox" ? "inbox" : "nav";
    const plan = planFromPointer(boundary, target, base);
    applyPlanLive(plan);
    persistWsLayout(wsLayout);
  }

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
  function openSendTo(opts){
    if (typeof d.openSendTo === "function") d.openSendTo(opts);
  }
  function uiMutate(op){
    if (typeof d.uiMutate === "function") d.uiMutate(op);
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

  /* Surfaces that render differently depending on whether this workspace is
     visible (the Bookmarks pane's "use in note") learn about it here — open()
     and close() are the one funnel every dismissal path goes through. */
  function onVisibilityChange(){
    if (typeof d.onVisibilityChange === "function") d.onVisibilityChange(isOpen());
  }

  function open(){
    if (isOpen()) return;
    wsReturnFocus = activeEl();
    const ws = root("notesworkspace");
    if (ws) ws.hidden = false;
    /* hydrate column widths/collapse from device-scoped localStorage */
    wsLayout = readWsLayout();
    applyWsLayout(wsLayout);
    wsLoadCards();
    adoptSharedBookmarkTab();
    renderInbox();
    rAF(() => {
      const btn = root("wsnewnote");
      if (btn && typeof btn.focus === "function") btn.focus();
    });
    onVisibilityChange();
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
    onVisibilityChange();
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
  /* item 17: the pane and the inbox share one key, so the inbox must re-read
     it on every open — the value it captured at feature-construction time is
     from before the user had picked any tab. */
  function adoptSharedBookmarkTab(){
    const t = storage && storage.getItem(STORAGE_KEY_TAB);
    if (t && t !== wsInboxTab){ wsInboxTab = t; wsInboxSig = ""; }
  }

  function renderInbox(){
    if (!isOpen()) return;
    const bms = bookmarks().slice();
    const { byT, laneOf, tabs } = inboxLaneTabs(bms, laneList(), bookmarkLaneId, nodeById);
    wsInboxTab = resolveInboxTab(wsInboxTab, tabs);
    const list = inboxListModel(bms, wsInboxTab, laneOf, byT, bookmarkSortKey);
    const tabsHtml = inboxTabsHTML(wsInboxTab, tabs, esc, laneColor);
    const color = wsInboxTab === "GENERAL" ? "var(--unlane)" : esc(laneColor(wsInboxTab));
    const listHtml = list.length
      ? list.map(nt => inboxItemHTML(nt, color, {
        esc, md, fmtWhen, icons, nodeById, open: nt.t === wsInboxOpenT,
        /* P5 width guard: flat row below isDesktop() keeps the more menu. */
        overflow: typeof d.isDesktop === "function" ? !d.isDesktop() : false,
      })).join("")
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
    const panel = root("wspanel");
    const tgt = anchor || null;
    popMenu.open({
      panel,
      anchor: tgt,
      offset: 40,
      className: "popmenu wsconfirm",
      html:
        `<div class="wsconfirmmsg">Delete note? It moves to the archive.</div>` +
        `<button type="button" data-wconfirm="cancel">Cancel</button>` +
        `<button type="button" data-wconfirm="ok" class="danger">Delete</button>`,
      onClick: async ev => {
        const btn = ev.target.closest && ev.target.closest("[data-wconfirm]");
        if (!btn) return;
        closeWsMenu();
        if (btn.dataset.wconfirm === "ok") await wsDeleteNote(noteId);
      },
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
    focusAtEnd(input);
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
    popMenu.close();
  }

  function openSectionMenu(btn){
    const secId = btn.closest(".wssec").dataset.sec;
    const panel = root("wspanel");
    popMenu.open({
      panel,
      anchor: btn,
      offset: 150,
      className: "popmenu",
      html:
        menuButtonHTML({ attrs: 'data-mi="add"', label: "Add section below", icon: icons.ICON_PLUS || "" }) +
        menuButtonHTML({ attrs: 'data-mi="edit"', label: "Edit", icon: icons.ICON_PENCIL || "" }) +
        menuButtonHTML({ attrs: 'data-mi="up"', label: "Move up", icon: icons.ICON_MOVE_UP || "" }) +
        menuButtonHTML({ attrs: 'data-mi="down"', label: "Move down", icon: icons.ICON_MOVE_DOWN || "" }) +
        menuSepHTML() +
        menuButtonHTML({ attrs: 'data-mi="del"', label: "Delete section", icon: icons.ICON_TRASH || "", danger: true }),
      onClick: async ev => {
        const mi = ev.target.closest("[data-mi]"); if (!mi) return;
        closeWsMenu();
        await sectionMenuAction(mi.dataset.mi, secId);
      },
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
    focusAtEnd(ta);
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
      // commit:true marks edit completion for optional git versioning; mid-edit
      // debounced autosaves (onInput) omit it so crash-safety writes do not
      // each become a commit.
      await wsPatchNow({ section: { id: secId, body: ta.value, commit: true } }, "secbody-" + secId);
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
    const refEl = btn.closest(".wsref");
    const secEl = btn.closest(".wssec");
    /* Confirm / prior menu path: fall back to the open-menu target. */
    const secId = (secEl && secEl.dataset && secEl.dataset.sec)
      || (menuRef && menuRef.secId) || "";
    const refId = (refEl && refEl.dataset && refEl.dataset.ref)
      || (menuRef && menuRef.refId) || "";
    const ref = findRef(secId, refId);
    if (!ref) return;
    if (btn.dataset.refact === "sendto"){
      const src = ref.source || {};
      openSendTo({
        text: stripAssetRefs((ref.snapshot && ref.snapshot.text) || ""),
        exceptId: src.node || "",
        title: "Send to chat\u2026",
      });
      return;
    }
    if (btn.dataset.refact === "copy"){
      closeWsMenu();
      copyText((ref.snapshot && ref.snapshot.text) || "");
      if (refEl){
        btn.innerHTML = "&#10003;";
        if (copyTimer) clearTimeoutFn(copyTimer);
        copyTimer = setTimeoutFn(() => { btn.innerHTML = icons.ICON_COPY || ""; }, COPY_ACK_MS);
      }
      return;
    }
    if (btn.dataset.refact === "jump"){
      const src = ref.source || {};
      if (!jumpToChatAddress({ node: src.node, uid: src.uid, segment: src.segment,
          record: src.record, turnTime: src.turnTime, text: (ref.snapshot || {}).text }))
        toast("Source unavailable — the captured snapshot is still shown here.");
      return;
    }
    if (btn.dataset.refact === "trash"){
      /* Inline Remove confirms first (HIG; no window.confirm — blocked in iOS PWA). */
      openRemoveRefConfirm(btn, secId, refId, refEl);
      return;
    }
  }

  /** Confirm before removing an embedded reference. Reuses the openDeleteConfirm
      shell (popMenu, className "popmenu wsconfirm", offset 40, data-wconfirm). */
  function openRemoveRefConfirm(anchor, secId, refId, refEl){
    if (!doc || !secId || !refId) return;
    menuRef = { secId, refId };
    menuInboxT = "";
    const panel = root("wspanel");
    popMenu.open({
      panel,
      anchor: anchor || null,
      trigger: anchor || null,
      offset: 40,
      className: "popmenu wsconfirm",
      html:
        `<div class="wsconfirmmsg">Remove this reference?</div>` +
        `<button type="button" data-wconfirm="cancel">Cancel</button>` +
        `<button type="button" data-wconfirm="ok" class="danger">Remove</button>`,
      onClick: async ev => {
        const btn = ev.target.closest && ev.target.closest("[data-wconfirm]");
        if (!btn) return;
        closeWsMenu();
        if (btn.dataset.wconfirm !== "ok") return;
        try {
          const docu = await api("/api/notes/" + encodeURIComponent(wsActiveId) +
            "/sections/" + encodeURIComponent(secId) + "/references/" + encodeURIComponent(refId),
            { method: "DELETE" });
          if (docu && docu.id === wsActiveId){ wsActive = docu; syncCardMeta(docu); }
        } catch { toast("Couldn't remove reference"); return; }
        if (refEl){
          const wrap = refEl.parentElement;
          refEl.remove();
          if (wrap && wrap.classList && wrap.classList.contains("wsrefs") && !wrap.children.length)
            wrap.remove();
        } else {
          renderNote();
        }
      },
    });
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
    if (storage) storage.setItem(STORAGE_KEY_TAB, wsInboxTab);
    wsInboxOpenT = "";   /* an unfolded row doesn't survive leaving its tab */
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
    /* the inbox card now carries the pane's own action row (item 5); "note"
       is what "Use in note" used to be, the rest are new here */
    const act = e.target.closest && e.target.closest("[data-bmact]");
    if (!act){
      /* anywhere else on the card folds its action row in or out, the same tap
         the compact pane and the embedded reference answer */
      const tapped = e.target.closest && e.target.closest(".wsibookmark");
      if (!tapped) return;
      wsInboxOpenT = wsInboxOpenT === tapped.dataset.t ? "" : tapped.dataset.t;
      wsInboxSig = "";
      renderInbox();
      revealInboxRow();
      return;
    }
    const card = act.closest(".wsibookmark");
    const t = (card && card.dataset && card.dataset.t) || menuInboxT;
    const nt = bookmarks().find(x => x.t === t);
    if (!nt) return;
    switch (act.dataset.bmact){
      case "more":
        openInboxOverflow(act, nt);
        return;
      case "sendto":
        openSendTo({
          text: stripAssetRefs(nt.text || ""),
          exceptId: nt.node || "",
          title: "Send bookmark to\u2026",
        });
        return;
      case "jump":
        /* jumpToChatAddress closes this workspace itself when it resolves */
        if (!(nt.node || nt.uid)) return;
        if (!jumpToChatAddress({
          node: nt.node, uid: nt.uid, segment: nt.segment,
          record: nt.record, turnTime: nt.turnTime, text: nt.text,
        })) toast("That chat is no longer available.");
        return;
      case "copy":
        closeWsMenu();
        copyText(nt.text || "");
        if (card){
          act.innerHTML = "&#10003;";
          if (wsInboxCopyTimer) clearTimeoutFn(wsInboxCopyTimer);
          wsInboxCopyTimer = setTimeoutFn(() => { wsInboxSig = ""; renderInbox(); }, COPY_ACK_MS);
        }
        return;
      case "note":
        startPlacement(nt);
        return;
      case "del": {
        closeWsMenu();
        /* Flat bar (toolbar): confirm first, like .wsref trash. Overflow menu
           already put Delete behind "more", so that path stays one-tap. */
        const onBar = !!(act.closest && act.closest(".actionbar"));
        if (onBar){
          openDeleteBookmarkConfirm(act, nt);
          return;
        }
        wsInboxOpenT = "";
        uiMutate({ k: "bookmark-del", t: nt.t });
        return;
      }
    }
  }

  /** Confirm before deleting a bookmark from the flat inbox toolbar (HIG;
      no window.confirm). Same shell as openRemoveRefConfirm / openDeleteConfirm. */
  function openDeleteBookmarkConfirm(anchor, nt){
    if (!nt) return;
    menuInboxT = nt.t;
    menuRef = null;
    const panel = root("wspanel");
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
        closeWsMenu();
        if (btn.dataset.wconfirm !== "ok") return;
        wsInboxOpenT = "";
        uiMutate({ k: "bookmark-del", t: nt.t });
      },
    });
  }

  function openInboxOverflow(trigger, nt){
    if (!nt) return;
    menuInboxT = nt.t;
    menuRef = null;
    const panel = root("wspanel");
    popMenu.open({
      panel,
      anchor: trigger,
      trigger,
      offset: 40,
      className: "popmenu",
      html: bookmarkMenuHTML(nt, { icons, context: "inbox" }),
      onClick: (ev) => {
        const btn = ev.target.closest && ev.target.closest("[data-bmact]");
        if (!btn) return;
        onInboxListClick({ target: btn, preventDefault(){}, stopPropagation(){} });
      },
    });
  }

  /* the unfolded row must be on screen after the tap that unfolded it — the
     card may sit at the bottom of the zone, and renderInbox has just rebuilt
     the list DOM, so the element has to be re-queried. */
  function revealInboxRow(){
    if (!wsInboxOpenT || !doc || typeof doc.querySelector !== "function") return;
    const CSSObj = (d.CSS || (typeof CSS !== "undefined" ? CSS : null)) || {};
    const t = CSSObj.escape ? CSSObj.escape(wsInboxOpenT) : wsInboxOpenT;
    const row = doc.querySelector(`#wsinboxlist .wsibookmark[data-t="${t}"] .actionbar`);
    if (row && typeof row.scrollIntoView === "function")
      row.scrollIntoView({ block: "nearest", behavior: "smooth" });
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
      // Mid-edit: debounced autosave without commit marker (crash-safety only).
      wsPatch({ section: { id: secId, title: t.value } }, "sectitle-" + secId);
    }
  }
  function onSectionsFocusIn(e){
    const t = e.target.closest && e.target.closest("[data-sectitle]");
    if (t){ wsSecTitleOrig[t.closest(".wssec").dataset.sec] = t.value; }
  }
  function onSectionsFocusOut(e){
    const t = e.target.closest && e.target.closest("[data-sectitle]");
    if (!t) return;
    const secId = t.closest(".wssec").dataset.sec;
    const s = wsActive && (wsActive.sections || []).find(x => x.id === secId);
    if (s) s.title = t.value;
    // commit:true marks edit completion for optional git versioning; mid-edit
    // debounced autosaves (onSectionsInput) omit it so keystroke saves do not
    // each become a commit. Enter already blurs into this path.
    wsPatchNow({ section: { id: secId, title: t.value, commit: true } }, "sectitle-" + secId);
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
    /* exclude every overflow trigger so the open-tap does not instantly dismiss */
    if (popMenu.shouldCloseForClick(e.target, {
      exclude: '[data-secmenu], [data-bmact="more"], [data-bmact="del"], [data-refact="trash"]',
    }))
      closeWsMenu();
  }

  /* ---- Phase 2: divider drag + collapse-to-peek (iPad/desktop only) ---- */
  function onDividerPointerDown(e){
    if (isNarrow()) return;
    const t = e.target;
    const handle = t && t.closest ? t.closest(".wsdivider") : null;
    if (!handle) return;
    const boundary = handle.dataset && handle.dataset.boundary;
    if (boundary !== "inbox" && boundary !== "nav") return;
    if (e.button != null && e.button !== 0) return;
    if (typeof e.preventDefault === "function") e.preventDefault();
    const base = layoutBaseFromCurrent();
    wsDrag = {
      boundary,
      pointerId: e.pointerId,
      startX: e.clientX,
      startZones: base.zones,
      startTotal: base.total,
      startCollapsed: base.collapsed,
      startLast: base.last,
      moved: false,
      handle,
    };
    if (handle.classList) handle.classList.add("dragging");
    if (typeof handle.setPointerCapture === "function" && e.pointerId != null){
      try { handle.setPointerCapture(e.pointerId); } catch { /* ignore */ }
    }
  }
  function onDividerPointerMove(e){
    if (!wsDrag) return;
    if (wsDrag.pointerId != null && e.pointerId != null && e.pointerId !== wsDrag.pointerId)
      return;
    const delta = (e.clientX || 0) - wsDrag.startX;
    if (Math.abs(delta) > 2) wsDrag.moved = true;
    const pointerX = wsDrag.boundary === "inbox"
      ? wsDrag.startZones.inbox + delta
      : wsDrag.startZones.inbox + wsDrag.startZones.nav + delta;
    const plan = planFromPointer(wsDrag.boundary, pointerX, {
      zones: wsDrag.startZones,
      total: wsDrag.startTotal,
      collapsed: wsDrag.startCollapsed,
      last: wsDrag.startLast,
    });
    applyPlanLive(plan);
  }
  function onDividerPointerUp(e){
    if (!wsDrag) return;
    if (wsDrag.pointerId != null && e.pointerId != null && e.pointerId !== wsDrag.pointerId)
      return;
    if (wsDrag.moved){
      persistWsLayout(wsLayout);
      wsSuppressClick = true;
    }
    const handle = wsDrag.handle;
    if (handle && handle.classList) handle.classList.remove("dragging");
    if (handle && typeof handle.releasePointerCapture === "function" && wsDrag.pointerId != null){
      try { handle.releasePointerCapture(wsDrag.pointerId); } catch { /* ignore */ }
    }
    wsDrag = null;
  }
  function onDividerKeydown(e){
    if (isNarrow()) return;
    const t = e.target;
    const handle = t && t.closest ? t.closest(".wsdivider") : (t && t.classList && t.classList.contains("wsdivider") ? t : null);
    if (!handle) return;
    if (e.key !== "ArrowLeft" && e.key !== "ArrowRight") return;
    if (typeof e.preventDefault === "function") e.preventDefault();
    const boundary = handle.dataset && handle.dataset.boundary;
    if (boundary !== "inbox" && boundary !== "nav") return;
    const base = layoutBaseFromCurrent();
    const delta = e.key === "ArrowLeft" ? -WS_DIVIDER_NUDGE : WS_DIVIDER_NUDGE;
    const pointerX = boundary === "inbox"
      ? base.zones.inbox + delta
      : base.zones.inbox + base.zones.nav + delta;
    const plan = planFromPointer(boundary, pointerX, base);
    applyPlanLive(plan);
    persistWsLayout(wsLayout);
  }
  function onZonesClick(e){
    if (isNarrow()) return;
    if (wsSuppressClick){ wsSuppressClick = false; return; }
    if (wsDrag) return;
    const t = e.target;
    if (!t) return;
    /* collapsed rail is a tap-catcher (bookmarkpeek idiom) */
    let zone = null;
    if (t.closest){
      const inbox = t.closest("#wsinbox");
      const nav = t.closest("#wsnav");
      if (inbox && inbox.classList && inbox.classList.contains("collapsed")) zone = "inbox";
      else if (nav && nav.classList && nav.classList.contains("collapsed")) zone = "nav";
    } else if (t.classList && t.classList.contains("collapsed")){
      if (t.id === "wsinbox") zone = "inbox";
      else if (t.id === "wsnav") zone = "nav";
    }
    if (zone) restoreCollapsedZone(zone);
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
    const zones = root("wszones");
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
    listen(root("wssections"), "focusout", onSectionsFocusOut);
    listen(root("wssections"), "keydown", onSectionsKeydown);
    listen(doc, "click", onDocClick);
    /* dividers: pointer drag + keyboard nudge; collapsed rails restore on tap */
    listen(zones, "pointerdown", onDividerPointerDown);
    listen(zones, "pointermove", onDividerPointerMove);
    listen(zones, "pointerup", onDividerPointerUp);
    listen(zones, "pointercancel", onDividerPointerUp);
    listen(zones, "keydown", onDividerKeydown);
    listen(zones, "click", onZonesClick);
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

  /* P5b: re-apply persisted layout under the current isDesktop mins (breakpoint
     cross). No-op when the workspace is closed. */
  function applyLayout(){
    if (!isOpen()) return;
    applyWsLayout(wsLayout || readWsLayout());
  }

  return {
    bind,
    destroy,
    open,
    close,
    isOpen,
    activeTitle: () => (wsActive && wsActive.title) || "",
    startPlacement,
    endPlacement,
    renderInbox,
    invalidateInbox,
    applyLayout,
  };
}
