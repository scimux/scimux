/* UI document reducer / merge / replay / storage foundation.
 *
 * Extracted in Packet 6C and wired into the sole browser entry in Packet 6F.
 * Named exports are exercised directly by Node tests and used by production.
 *
 * Explicit inputs (no UI / uiOps / uiRev / localStorage / fetch globals):
 * - document objects are passed as `doc`
 * - operations as `op` / `ops`
 * - revisions as `rev`
 * - storage via an injected adapter `{ getItem, setItem }` (localStorage shape)
 *
 * Mutation model (matches inline applyOp / uiMutate — do not redesign as
 * immutable): applyOp mutates `doc` in place and returns it. Nested list
 * objects are retained or replaced per-op as documented on each case. The
 * operation object itself is mutated when stampAndEnqueue writes `op.rev`.
 * pin-order / groups / lanes assign the op's array by reference (same object
 * identity retained); arch may push onto an existing archived array; unarch /
 * pin / unpin / bookmark-del replace the list with a new array.
 *
 * No document, window, fetch, localStorage, navigator, timers, or app state.
 * No ETag retry, CSRF, or server orchestration (Packet 6E).
 */

/* Storage keys — match the inline localStorage names. */
export const PENDING_OPS_KEY = "scimux-ui-ops";
export const CACHED_UI_KEY = "scimux-ui";

/* Fresh defaults object (new arrays each call — same shape as the inline
   Object.assign source literal evaluated on every normUI call). */
export function emptyUI() {
  return {
    groups: [],
    archived: [],
    bookmarks: [],
    lanes: [],
    pinned: [],
  };
}

/* Shallow-merge defaults over j. Unknown top-level fields on j are preserved.
   Nested arrays/objects from j are shared by reference (Object.assign shallow).
   null/undefined j → defaults only. */
export function normUI(j) {
  return Object.assign(emptyUI(), j || {});
}

/* Only bookmark add/delete are idempotent under R20.7: they dedupe on
   timestamp and must always replay across revision changes. Wholesale list
   snapshots (groups/lanes/pin-order) and arch/unarch/pin are not. */
export function idempotentOp(op) {
  return op.k === "bookmark-add" || op.k === "bookmark-del";
}

/* Apply one operation to doc IN PLACE. Returns the same doc reference.
   Unknown op.k is a no-op. Matches the inline switch body exactly. */
export function applyOp(doc, op) {
  switch (op.k) {
    case "arch":
      doc.archived ||= [];
      if (!doc.archived.includes(op.id)) doc.archived.push(op.id);
      break;
    case "unarch":
      doc.archived = (doc.archived || []).filter((x) => x !== op.id);
      break;
    case "bookmark-add":
      doc.bookmarks ||= [];
      if (!doc.bookmarks.some((n) => n.t === op.bookmark.t))
        doc.bookmarks.push(op.bookmark);
      break;
    case "bookmark-del":
      /* deleting a capture promotes its anchored comments to standalone
         notes — neither deleted with it nor stripped of context. They keep
         the anchor's activity/lane (so they stay on their tab and can still
         jump); they now sort at their own timestamp. */
      {
        const dead = (doc.bookmarks || []).find((n) => n.t === op.t);
        if (dead)
          for (const c of doc.bookmarks) {
            if (c.anchor !== op.t) continue;
            delete c.anchor;
            if (dead.node && !c.node) {
              c.node = dead.node;
              if (dead.turnTime) c.turnTime = dead.turnTime;
            }
            if (dead.lane && !c.lane) c.lane = dead.lane;
          }
      }
      doc.bookmarks = (doc.bookmarks || []).filter((n) => n.t !== op.t);
      break;
    case "pin":
      /* new pins land at the FRONT so a freshly pinned card shows at the top of
         the Pinned tab (Apple Notes idiom); dedupe keeps the array a set */
      doc.pinned = (doc.pinned || []).filter((x) => x !== op.id);
      doc.pinned.unshift(op.id);
      break;
    case "unpin":
      doc.pinned = (doc.pinned || []).filter((x) => x !== op.id);
      break;
    case "pin-order": /* drag reorder replaces the whole ordered pin list */
      doc.pinned = op.pinned;
      break;
    case "groups": /* map edits replace the whole tab list (rare, one device) */
      doc.groups = op.groups;
      break;
    case "lanes": /* lane create/rename replaces the compact lane list */
      doc.lanes = op.lanes;
      break;
  }
  return doc;
}

/* Replay every op onto doc in order (in-place). Returns the same doc. */
export function replayOps(doc, ops) {
  for (const op of ops) applyOp(doc, op);
  return doc;
}

/* ---------- revision stamping + queue (uiMutate pure core) ---------- */

/* Stamp op.rev with the document revision this op was built against (R20.7),
   then push onto queue. Mutates both op and queue. Does not apply the op and
   does not touch storage/render — callers mirror the rest of uiMutate. */
export function stampAndEnqueue(op, rev, queue) {
  op.rev = rev;
  queue.push(op);
  return queue;
}

/* Local optimistic path: apply, stamp, enqueue. Matches uiMutate's first
   three pure steps. Mutates doc, op, and queue. */
export function queueLocalOp(doc, op, rev, queue) {
  applyOp(doc, op);
  stampAndEnqueue(op, rev, queue);
  return { doc, op, queue };
}

/* ---------- replay eligibility filters ---------- */

/* Initial-load filter (loadUI): idempotent always; non-idempotent only when
   there is no known revision (offline) or the op's rev still matches. */
export function eligibleForLoadReplay(op, rev) {
  return idempotentOp(op) || !rev || op.rev === rev;
}

/* Conflict / offline-adopt filter (flushUI 409 path and no-rev adopt):
   idempotent always; non-idempotent only when built against this exact rev. */
export function eligibleForConflictReplay(op, rev) {
  return idempotentOp(op) || op.rev === rev;
}

export function filterOpsForLoad(ops, rev) {
  return ops.filter((op) => eligibleForLoadReplay(op, rev));
}

export function filterOpsForConflict(ops, rev) {
  return ops.filter((op) => eligibleForConflictReplay(op, rev));
}

/* loadUI queue merge: persisted ops (filtered) ordered before this session's
   early ops (unfiltered — they were made against the in-memory document). */
export function mergeLoadOps(persisted, session, rev) {
  return filterOpsForLoad(persisted, rev).concat(session);
}

/* Remote document adoption + eligible local replay (flushUI conflict / no-rev
   adopt). Returns a new normalized outer doc with kept ops applied. normUI is
   deliberately shallow, so retained remote arrays can be mutated by replay,
   matching production. The ops array is a new filtered list (not in-place). */
export function adoptRemoteAndReplay(remote, ops, rev) {
  const doc = normUI(remote);
  const kept = filterOpsForConflict(ops, rev);
  replayOps(doc, kept);
  return { doc, ops: kept };
}

/* Initial-load path after a remote fetch (or empty remote): normalize remote,
   merge persisted+session with load filter, replay onto doc. */
export function loadDocumentWithReplay(remote, persisted, session, rev) {
  const doc = normUI(remote);
  const ops = mergeLoadOps(persisted, session, rev);
  replayOps(doc, ops);
  return { doc, ops };
}

/* ---------- storage adapter (injected; no implicit localStorage) ---------- */

/* Persist the pending-ops queue. Swallows setItem exceptions (quota, denied). */
export function savePendingOps(ops, storage) {
  try {
    storage.setItem(PENDING_OPS_KEY, JSON.stringify(ops));
  } catch {}
}

/* Load pending ops. Malformed JSON, null, and getItem exceptions → [].
   Non-array truthy parse results pass through (same as inline || []). */
export function loadPendingOps(storage) {
  try {
    return JSON.parse(storage.getItem(PENDING_OPS_KEY) || "[]") || [];
  } catch {
    return [];
  }
}

/* Mirror the current document for offline reads. Bare setItem like the
   success path of loadUI / uiMutate; exceptions are not swallowed here. */
export function saveCachedUI(doc, storage) {
  storage.setItem(CACHED_UI_KEY, JSON.stringify(doc));
}

/* Offline/cache fallback after a failed remote fetch. Returns a normalized
   document, or null when get/parse fails entirely (inline leaves UI unchanged).
   Empty/missing cache → normUI({}) defaults. */
export function loadCachedUI(storage) {
  try {
    return normUI(JSON.parse(storage.getItem(CACHED_UI_KEY) || "{}"));
  } catch {
    return null;
  }
}
