/* storage.js — browser storage that cannot throw.
 *
 * localStorage is not a plain object with a plain contract. Reading it can
 * raise in private mode, writing it can raise when the origin's quota is
 * spent, and a policy change can turn a working store into a refusing one
 * between two keystrokes. Every one of those arrives as an exception in the
 * middle of whatever the user was doing, and the code it interrupts is
 * ordinarily the code holding their text.
 *
 * So the adapter never propagates: a refusal degrades to an in-memory shadow
 * that lasts as long as the tab. Drafts, the selected chat, the map fold --
 * all of it keeps working, and only "it will still be here tomorrow" is lost.
 * `persistent` says whether that promise still holds, so the shell can say so
 * once instead of every module guessing.
 *
 * The shadow wins reads, which is what makes a refused write recoverable and
 * a refused removal actually forgotten: the value the caller last said is the
 * value the caller reads back, whatever the store did with it.
 *
 * No document, window, fetch, navigator, timers, or app state -- the raw store
 * is injected, so the composition root is the only place that names a global.
 */

const PROBE_KEY = "scimux-storage-probe";

/* A store that cannot hold a value it was just given is not a store. Probing
 * once up front is what lets a denied browser start in memory rather than
 * discover it on the first draft. */
function writable(raw){
  if (!raw || typeof raw.getItem !== "function") return false;
  try {
    raw.setItem(PROBE_KEY, "1");
    raw.removeItem(PROBE_KEY);
    return true;
  } catch {
    return false;
  }
}

export function createStorage(raw){
  const shadow = new Map();      /* key -> string, or null for "removed" */
  const store = writable(raw) ? raw : null;
  let persistent = store !== null;
  return {
    get persistent(){ return persistent; },
    getItem(key){
      if (shadow.has(key)) return shadow.get(key);
      if (!store) return null;
      try {
        const v = store.getItem(key);
        return typeof v === "string" ? v : null;
      } catch {
        persistent = false;
        return null;
      }
    },
    setItem(key, value){
      const v = String(value);
      let saved = false;
      if (store){
        try {
          store.setItem(key, v);
          saved = true;
        } catch {
          persistent = false;
        }
      }
      /* A store that has refused once is not trusted to answer the read
         either, so from then on the caller reads back what it wrote. */
      if (saved && persistent) shadow.delete(key);
      else shadow.set(key, v);
    },
    removeItem(key){
      let removed = false;
      if (store){
        try {
          store.removeItem(key);
          removed = true;
        } catch {
          persistent = false;
        }
      }
      if (removed && persistent) shadow.delete(key);
      else shadow.set(key, null);
    },
  };
}

/* Send-to provenance follows draft lifetime: source intents stay device-local
 * until the destination send succeeds, then callers append shared UI links. */
export const PENDING_FORWARD_PREFIX = "scimux-sendto-pending:";
export const AWAITING_FORWARD_PREFIX = "scimux-sendto-awaiting:";

export function pendingForwardKey(nodeId){
  return PENDING_FORWARD_PREFIX + (nodeId || "");
}

export function awaitingForwardKey(nodeId){
  return AWAITING_FORWARD_PREFIX + (nodeId || "");
}

export function sourceAddressKey(source = {}){
  if (source.uid)
    return `u:${source.uid}:${Number(source.segment) || 0}:${Number(source.record) || 0}`;
  return `n:${source.node || ""}:${source.turnTime || ""}`;
}

export function readPendingForwards(storage, nodeId){
  try {
    const value = JSON.parse(storage && storage.getItem(pendingForwardKey(nodeId)) || "[]");
    return Array.isArray(value) ? value.filter(x => x && typeof x === "object") : [];
  } catch { return []; }
}

export function writePendingForwards(storage, nodeId, sources){
  const list = Array.isArray(sources) ? sources : [];
  try {
    if (!list.length) storage && storage.removeItem(pendingForwardKey(nodeId));
    else storage && storage.setItem(pendingForwardKey(nodeId), JSON.stringify(list));
  } catch { /* a refused local cache must not block Send-to */ }
  return list;
}

export function addPendingForward(storage, nodeId, source){
  if (!source || (!source.uid && !source.node)) return readPendingForwards(storage, nodeId);
  const list = readPendingForwards(storage, nodeId);
  const key = sourceAddressKey(source);
  if (!list.some(item => sourceAddressKey(item) === key)) list.push(source);
  return writePendingForwards(storage, nodeId, list);
}

export function clearPendingForwards(storage, nodeId){
  return writePendingForwards(storage, nodeId, []);
}

export function writeAwaitingForward(storage, nodeId, text){
  try { storage && storage.setItem(awaitingForwardKey(nodeId), String(text || "")); }
  catch { /* same best-effort draft cache contract */ }
}

export function readAwaitingForward(storage, nodeId){
  try { return storage && storage.getItem(awaitingForwardKey(nodeId)) || ""; }
  catch { return ""; }
}

export function clearAwaitingForward(storage, nodeId){
  try { storage && storage.removeItem(awaitingForwardKey(nodeId)); }
  catch { /* same best-effort draft cache contract */ }
}

export function makeForwardLink(source, destinationNode, sentText, sentAt){
  const when = sentAt || new Date().toISOString();
  return {
    id: `${sourceAddressKey(source)}>${destinationNode || ""}:${when}`,
    source: { ...(source || {}) },
    destination: { node: destinationNode || "", turnTime: when, text: sentText || "" },
    sent_at: when,
  };
}

export function makeForwardLinkToTurn(source, destinationNode, turn = {}){
  const link = makeForwardLink(source, destinationNode, turn.text || "", turn.time || undefined);
  link.destination = {
    node: destinationNode || "", text: turn.text || "", turnTime: turn.time || "",
    ...(turn.uid ? {
      uid: turn.uid, segment: Number(turn.segment) || 0, record: Number(turn.record) || 0,
    } : {}),
  };
  return link;
}
