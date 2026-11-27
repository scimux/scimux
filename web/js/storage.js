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
    keys(){
      if (!store || typeof store.key !== "function") return null;
      try {
        if (!Number.isSafeInteger(store.length) || store.length > 4096) return null;
        const keys = new Set();
        for (let i = 0; i < store.length; i++) {
          const key = store.key(i);
          if (typeof key === "string") keys.add(key);
        }
        for (const [key, value] of shadow) value === null ? keys.delete(key) : keys.add(key);
        return keys.size <= 4096 ? [...keys] : null;
      } catch { persistent = false; return null; }
    },
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

const draftSlugPrefixes = ["scimux-draft:", "scimux-people-draft:", "scimux-prompt-sources:", "scimux-prompt-flights:", "scimux-sendto-pending:", "scimux-sendto-awaiting:"];

// One-time cleanup of a retired-draft archive. An "unknown" owner's last
// string-valued prefix entry is copied into an absent live key. Every other
// owner is dropped. Damaged JSON is removed and never thrown.
function restoreUnknownDraft(storage, nodeId){
  let parsed;
  try { parsed = JSON.parse(storage.getItem("scimux-retired-drafts:" + nodeId + ":unknown")); }
  catch { return; }
  if (!Array.isArray(parsed) || !parsed.length) return;
  const last = parsed[parsed.length - 1];
  if (!last || typeof last !== "object" || Array.isArray(last)) return;
  for (const prefix of draftSlugPrefixes) {
    const value = last[prefix];
    if (typeof value !== "string") continue;
    const live = prefix + nodeId;
    if (storage.getItem(live) !== null) continue;
    storage.setItem(live, value);
  }
}

function dropUnindexedRetiredDrafts(storage, nodeId){
  if (typeof storage.keys !== "function") return;
  let keys;
  try { keys = storage.keys(); }
  catch { return; }
  if (!Array.isArray(keys)) return;
  const prefix = "scimux-retired-drafts:" + nodeId + ":";
  for (const key of keys) {
    if (typeof key === "string" && key.startsWith(prefix)) storage.removeItem(key);
  }
}

function clearRetiredDraftArchive(storage, nodeId){
  const indexKey = "scimux-retired-index:" + nodeId;
  const raw = storage.getItem(indexKey);
  if (raw === null) return;
  let owners = null;
  try {
    const parsed = JSON.parse(raw);
    if (Array.isArray(parsed)) owners = parsed;
  } catch { /* damaged index */ }
  if (owners) {
    if (owners.includes("unknown")) restoreUnknownDraft(storage, nodeId);
    for (const owner of owners) {
      if (typeof owner !== "string") continue;
      storage.removeItem("scimux-retired-drafts:" + nodeId + ":" + owner);
    }
  } else dropUnindexedRetiredDrafts(storage, nodeId);
  storage.removeItem(indexKey);
}

// Slug-keyed drafts follow scimux-draft-owner. No owner adopts them. A
// different owner drops them. Nothing is archived.
export function bindDraftIdentity(storage, nodeId, instance){
  if (!instance) return true;
  try {
    clearRetiredDraftArchive(storage, nodeId);
    const old = storage.getItem("scimux-draft-owner:" + nodeId);
    if (old === instance) return true;
    if (old !== null) {
      for (const prefix of draftSlugPrefixes) storage.removeItem(prefix + nodeId);
    }
    storage.setItem("scimux-draft-owner:" + nodeId, instance);
    return true;
  } catch { return false; }
}
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
  if (source.server && source.owner && source.chat && source.generation && source.log)
    return promptSourceKey(source);
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
  if (!source || (!source.uid && !source.node && !promptSourceKey(source))) return readPendingForwards(storage, nodeId);
  const list = readPendingForwards(storage, nodeId);
  const key = sourceAddressKey(source);
  if (!list.some(item => sourceAddressKey(item) === key)) list.push(source);
  return writePendingForwards(storage, nodeId, list);
}

export function clearPendingForwards(storage, nodeId){
  return writePendingForwards(storage, nodeId, []);
}

function awaitingAddress(value){
  return !!value && typeof value === "object" && typeof value.uid === "string" &&
    value.uid.length > 0 && Number.isInteger(value.segment) && value.segment >= 0 &&
    Number.isInteger(value.record) && value.record >= 0;
}

function awaitingLegacyCount(value){
  return Math.max(0, Math.floor(Number(value) || 0));
}

export function writeAwaitingForward(storage, nodeId, text, after = 0, at = Date.now()){
  const value = {
    text: String(text || ""),
    ...(awaitingAddress(after)
      ? { after: { uid: after.uid, segment: after.segment, record: after.record } }
      : { afterTurns: typeof after === "number" ? awaitingLegacyCount(after) : 0 }),
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
        ...(Object.prototype.hasOwnProperty.call(parsed, "after") && awaitingAddress(parsed.after)
          ? { after: { uid: parsed.after.uid, segment: parsed.after.segment, record: parsed.after.record } }
          : { afterTurns: Object.prototype.hasOwnProperty.call(parsed, "after") ? 0 : awaitingLegacyCount(parsed.afterTurns) }),
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
  if (value.server && value.owner && value.chat && value.generation && value.log && promptSourceKey(value))
    return {server:value.server, owner:value.owner, chat:value.chat, generation:value.generation, log:value.log, seq:value.seq};
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

export function makePromptLink(source, destinationNode, address, submitted = ""){
  const destination = {node:destinationNode, uid:address.uid, segment:address.segment ?? 0, record:address.record};
  const link = {kind:"prompt", id:`prompt:${promptSourceKey(source)}>${promptSourceKey(destination)}`, source:{...source}, destination};
  const preview = Array.from(String(submitted || "").replace(/\s+/g, " ").trim()).slice(0, 100).join("");
  if (preview) link.preview = preview;
  return link;
}

/* Prompt preparation is independent of Send-to's legacy delivery latch. */
export function promptSourceKey(source = {}){
  if (source && typeof source.uid === "string" && source.uid &&
      Number.isSafeInteger(source.segment ?? 0) && (source.segment ?? 0) >= 0 &&
      Number.isSafeInteger(source.record) && source.record >= 0)
    return JSON.stringify(["local", source.uid, source.segment ?? 0, source.record]);
  if (source && [source.server, source.owner, source.chat, source.generation, source.log].every(x => typeof x === "string" && x) &&
      Number.isSafeInteger(source.seq) && source.seq > 0)
    return JSON.stringify(["shared", source.server, source.owner, source.chat, source.generation, source.log, source.seq]);
  return "";
}
export function readPromptSources(storage, node){
  try {
    const value = JSON.parse(storage.getItem("scimux-prompt-sources:" + node) || "[]");
    return Array.isArray(value) ? value.filter(source => promptSourceKey(source)) : [];
  } catch { return []; }
}
export function writePromptSources(storage, node, sources){
  try {
    if (sources.length) storage.setItem("scimux-prompt-sources:" + node, JSON.stringify(sources));
    else storage.removeItem("scimux-prompt-sources:" + node);
  } catch { /* draft cache is best effort */ }
}
export function addPromptSource(storage, node, source){
  const key = promptSourceKey(source);
  const sources = readPromptSources(storage, node);
  if (key && !sources.some(item => promptSourceKey(item) === key)) sources.push(source);
  writePromptSources(storage, node, sources);
}
export function clearPromptSources(storage, node){ writePromptSources(storage, node, []); }

export function readPromptFlights(storage, node){
  try {
    const value = JSON.parse(storage.getItem("scimux-prompt-flights:" + node) || "[]");
    return Array.isArray(value) ? value.filter(item => item && Array.isArray(item.sources) && item.sources.every(promptSourceKey)) : [];
  } catch { return []; }
}
export function writePromptFlights(storage, node, flights){
  try {
    if (flights.length) storage.setItem("scimux-prompt-flights:" + node, JSON.stringify(flights.slice(-32)));
    else storage.removeItem("scimux-prompt-flights:" + node);
  } catch { /* best effort, no replay */ }
}

export function recoverPromptFlights(storage, node, text){
  const flights = readPromptFlights(storage, node);
  const matches = flights.map((item, index) => item.text === text ? index : -1).filter(index => index >= 0);
  if (matches.length !== 1) return;
  const index = matches[0];
  const [flight] = flights.splice(index, 1);
  flight.sources.forEach(source => addPromptSource(storage, node, source));
  writePromptFlights(storage, node, flights);
}

/* Conservative recovery only: this never asserts delivery or dispatches. */
export function rejectedPromptFlight(flights, turns){
  if (!Array.isArray(flights) || flights.length !== 1) return null;
  const flight = flights[0], boundary = flight?.boundary;
  if (!flight.accepted || !promptSourceKey(boundary) || !flight.text) return null;
  const index = (turns || []).findIndex(turn => promptSourceKey(turn) === promptSourceKey(boundary));
  if (index < 0) return null;
  const prompt = turns[index + 1];
  if (!prompt || prompt.uid !== boundary.uid || (prompt.segment ?? 0) !== (boundary.segment ?? 0) ||
      prompt.role !== "user" || prompt.record <= boundary.record ||
      !(prompt.text === flight.text || String(prompt.text || "").startsWith(flight.text + "\n\n"))) return null;
  return flight;
}
