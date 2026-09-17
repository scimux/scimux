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
 * until the sent text appears as a real destination transcript turn. */
export const PENDING_FORWARD_PREFIX = "scimux-sendto-pending:";
export const AWAITING_FORWARD_PREFIX = "scimux-sendto-awaiting:";
export const FORWARD_TEXT_FALLBACK_MAX = 256;
/* A latch only clears when its text lands as a destination turn, and both the
   cancel guard and the overwrite guard read a live latch as "still in flight".
   Any divergence the matcher cannot bridge would therefore freeze the node's
   Send-to state for good, so an unconfirmed latch is given a life, not a
   promise. A day is long enough to outlast a closed tab and short enough that
   a stuck node heals itself without the user knowing there was a latch. */
export const AWAITING_FORWARD_TTL_MS = 24 * 60 * 60 * 1000;

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

export function writeAwaitingForward(storage, nodeId, text, afterTurns = 0, at = Date.now()){
  const value = {
    text: String(text || ""),
    afterTurns: Math.max(0, Math.floor(Number(afterTurns) || 0)),
    at: Number(at) || 0,
  };
  try { storage && storage.setItem(awaitingForwardKey(nodeId), JSON.stringify(value)); }
  catch { /* same best-effort draft cache contract */ }
  return value;
}

export function readAwaitingForward(storage, nodeId, now = Date.now()){
  let raw = "";
  try { raw = storage && storage.getItem(awaitingForwardKey(nodeId)) || ""; }
  catch { return null; }
  if (!raw) return null;
  let value = null;
  try {
    const parsed = JSON.parse(raw);
    if (parsed && typeof parsed === "object" && typeof parsed.text === "string"){
      value = {
        text: parsed.text,
        afterTurns: Math.max(0, Math.floor(Number(parsed.afterTurns) || 0)),
        at: Number(parsed.at) || 0,
      };
    }
  } catch { /* latches before this shape stored the expected text directly */ }
  /* An unstamped latch is one that predates the TTL, and the nodes most
     likely to hold one are exactly the nodes already stuck. Expiring it on
     sight costs at most one in-flight confirmation and unsticks the rest. */
  if (!value || !value.at || (Number(now) || 0) - value.at >= AWAITING_FORWARD_TTL_MS){
    clearAwaitingForward(storage, nodeId);
    return null;
  }
  return value;
}

export function clearAwaitingForward(storage, nodeId){
  try { storage && storage.removeItem(awaitingForwardKey(nodeId)); }
  catch { /* same best-effort draft cache contract */ }
}

function compactAddress(value = {}){
  const out = { node: value.node || "" };
  if (value.uid){
    out.uid = value.uid;
    out.segment = Number(value.segment) || 0;
    out.record = Number(value.record) || 0;
  }
  if (value.turnTime) out.turnTime = value.turnTime;
  return out;
}

export function makeForwardLinkToTurn(source, destinationNode, turn = {}){
  const compactSource = compactAddress(source);
  const destination = compactAddress({
    node: destinationNode,
    uid: turn.uid,
    segment: turn.segment,
    record: turn.record,
    turnTime: turn.time,
  });
  /* Text is only a last-resort live-segment address. Exact and timestamped
     transcripts do not duplicate message bodies in the shared UI document. */
  if (!destination.uid && !destination.turnTime){
    const text = String(turn.text || "");
    destination.text = text.slice(0, FORWARD_TEXT_FALLBACK_MAX);
    if (text.length > FORWARD_TEXT_FALLBACK_MAX) destination.textPrefix = true;
  }
  const destinationKey = destination.uid || destination.turnTime
    ? sourceAddressKey(destination)
    : `n:${destination.node}:text:${destination.text || ""}`;
  return {
    id: `${sourceAddressKey(compactSource)}>${destinationKey}`,
    source: compactSource,
    destination,
    sent_at: turn.time || "",
  };
}
