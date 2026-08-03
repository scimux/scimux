/* Pure spatial-navigation decisions (level clamp, scrim/peek, document swipe
 * routing, Notes workspace swipe-back).
 *
 * Extracted in Packet 6D and wired into the sole browser entry in Packet 6F.
 * Named exports are exercised directly by Node tests and used by production.
 *
 * Explicit inputs only — no document, window, fetch, localStorage, navigator,
 * timers, matchMedia, or implicit app state (level / bookmarksOpen / overlays).
 *
 * Action descriptors describe *what* to do; callers apply DOM, storage, render,
 * and listener side effects. setLevel(3)'s map invalidation/render/scroll stays
 * inline forever in this packet (not modeled here beyond the level value).
 *
 * Overlay ownership is snapshotted at touchstart and consulted immutably at
 * touchend. documentSwipeDecision takes no live-overlay callback and performs
 * no live recheck — if an overlay closes in its own bubbling handler before
 * document touchend, the original snapshot still suppresses app navigation.
 */

/* ---------- level clamp / peek controls ---------- */

/* Levels are phone spatial panes: 1 = chat, 2 = cards, 3 = map. */
export function clampLevel(n){
  return Math.max(1, Math.min(3, n));
}

/* Scrim tap steps exactly one pane back (3→2, 2→1), never a jump to chat. */
export function scrimStep(level){
  return { type: "setLevel", level: clampLevel(level - 1) };
}

/* Bookmarks left-peek tap closes only Bookmarks (step back toward Chat). */
export function bookmarkPeekClose(){
  return { type: "setBookmarksOpen", open: false };
}

/* ---------- document swipe: snapshot at start, decide at end ---------- */

/* Capture touchstart geometry and immutable overlay ownership.
   overlayOwned must already be the boolean
   (notesOpen || searchOpen || archivedOpen) — never re-read later. */
export function captureTouchStart({
  x, y, clientWidth, localCard, overlayOwned,
} = {}){
  const cw = clientWidth || 0;
  return {
    x: x || 0,
    y: y || 0,
    edge: (x || 0) < 28,
    edgeR: (x || 0) > cw - 28,
    localCard: !!localCard,
    overlayOwned: !!overlayOwned,
  };
}

const HORIZ_MIN = 60;

function isHorizontal(dx, dy){
  return Math.abs(dx) > HORIZ_MIN && Math.abs(dx) > 2 * Math.abs(dy);
}

/* Document-level touchend decision. `touch` is the captureTouchStart snapshot.
   `end` is { x, y } from changedTouches[0].
   `state` supplies live app flags needed for routing — but NOT live overlays:
     { level, bookmarksOpen, isDesktop, editingChatDesc, editingTitle, sel }
   Returns null when the gesture is a no-op (after clearing the snapshot).
   When touch.overlayOwned is true, returns { type: "suppress" } so the caller
   discards the gesture without any app-nav side effect. */
export function documentSwipeDecision(touch, end, state = {}){
  if (!touch) return null;
  /* Snapshot owns the gesture: do not accept or call any live-overlay check. */
  if (touch.overlayOwned) return { type: "suppress" };

  const x = end?.x ?? 0, y = end?.y ?? 0;
  const dx = x - touch.x, dy = y - touch.y;
  const horiz = isHorizontal(dx, dy);
  const {
    level = 1,
    bookmarksOpen = false,
    isDesktop = false,
    editingChatDesc = false,
    editingTitle = "",
    sel = "",
  } = state;

  if ((editingChatDesc || editingTitle === sel) && horiz && dx < 0){
    return { type: "cancelEditors" };
  }
  if (!isDesktop && horiz && dx < 0 && touch.edgeR && !bookmarksOpen){
    /* right-edge R→L: step an existing level first; Bookmarks opens only from Chat */
    if (level > 1) return { type: "setLevel", level: clampLevel(level - 1) };
    return { type: "setBookmarksOpen", open: true };
  }
  if (!isDesktop && horiz && dx < 0 && bookmarksOpen){
    /* Bookmarks R→L → Notes overview */
    return { type: "openWorkspace" };
  }
  if (!isDesktop && horiz && dx > 0 && bookmarksOpen){
    /* Bookmarks L→R → Chat */
    return { type: "setBookmarksOpen", open: false };
  }
  if (!touch.localCard && horiz && dx > 0 && touch.edge){
    /* left-edge L→R: open one level (cards/map) */
    return { type: "setLevel", level: clampLevel(level + 1) };
  }
  if (!touch.localCard && horiz && dx < 0 && level > 1){
    /* general R→L: close one level */
    return { type: "setLevel", level: clampLevel(level - 1) };
  }
  return null;
}

/* ---------- Notes workspace swipe-back (narrow only) ---------- */

/* Capture Notes-local touchstart. inEditable fences textarea/input caret drags. */
export function captureNotesTouchStart({ x, y, inEditable } = {}){
  return {
    x: x || 0,
    y: y || 0,
    inEditable: !!inEditable,
  };
}

/* Narrow Notes L→R back order: cancel placement → editor to list → close.
   Wide layout and editable starts are ignored (return null).
   `state`: { isNarrow, placing, noteOpen } — explicit, no matchMedia/DOM. */
export function notesSwipeBackDecision(start, end, state = {}){
  if (!start || start.inEditable) return null;
  if (!state.isNarrow) return null;
  const x = end?.x ?? 0, y = end?.y ?? 0;
  const dx = x - start.x, dy = y - start.y;
  if (!(dx > HORIZ_MIN && dx > 2 * Math.abs(dy))) return null; /* L→R only */
  if (state.placing) return { type: "endPlacement" };
  if (state.noteOpen) return { type: "noteToList" };
  return { type: "closeWorkspace" };
}
