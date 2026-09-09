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

/* ---------- which pane a fresh boot opens ---------- */

/* Device-scoped, like every other remembered pane choice (map tab, lane
   filter). Read by the shell; named here because the decision below is the
   only thing that interprets it. */
export const LEVEL_KEY = "scimux-level";

/* A phone that sleeps loses its tunnel, and the re-dial re-runs createApp() in
   a fresh module graph (lifecycle.js) — so "where I was" can only come from
   storage. The selection already survives; without the pane too, the phone
   reopened the remembered chat with the Activities list parked in front of it.

   Deliberately not time-boxed: the last pane the user chose is the last pane
   they chose, whether that was 15 minutes or a week ago.

   `stored` is the raw storage value (any type — it is user-writable). Anything
   that is not a pane number, and chat with no chat to show, mean the list. */
export function bootLevel({ stored, isDesktop, hasSelection } = {}){
  /* Desktop has no pane stack: setLevel is inert there, so 1 is the only
     honest answer and a phone-written value must not leak into it. */
  if (isDesktop) return 1;
  const n = Number(stored);
  if (!Number.isInteger(n) || n < 1 || n > 3) return 2;
  /* Level 1 without a remembered node is an empty chat pane. The map needs no
     selection — it renders the fleet, not a conversation. */
  if (n === 1 && !hasSelection) return 2;
  return clampLevel(n);
}

/* Injected adapter only (localStorage shape), never a global. Returns the raw
   value so bootLevel owns validation; a storage that denies reads is a device
   that simply does not remember. */
export function loadStoredLevel(storage){
  try {
    return storage.getItem(LEVEL_KEY);
  } catch {
    return null;
  }
}

/* Called from setLevel, i.e. on every swipe and scrim tap. A full or denied
   store must never break navigation, so the write is best-effort. */
export function saveLevel(storage, level){
  try {
    storage.setItem(LEVEL_KEY, String(clampLevel(level)));
  } catch { /* private mode, quota, disabled storage: forget instead of fail */ }
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

/* Chevron + aria for #journeybtn — the MIRROR of bookmarksToggleState, not a
   copy: Journeys sits left of Activities, so opening it moves content rightward
   (›) and closing it moves left (‹). The control
   must point at what the tap will do: it used to be a permanent left chevron
   even while the tap would close the pane (UI review item 18); copying the
   right-hand pane's mapping then read as flipped (review 2, item 2). On the phone the
   Journeys pane is a forward level rather than a toggle, so the shell simply
   never reports `open` there and the arrow stays "open journeys". */
export function journeyToggleState(open){
  return {
    innerHTML: open ? "&#8249;" : "&#8250;",
    ariaLabel: open ? "close journeys" : "open journeys",
  };
}
