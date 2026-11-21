/* Composer feature: singleton prompt bar, drafts, staging, attachments, send.
 *
 * Packet 7D ownership inventory
 * -----------------------------
 * Owned roots / controls (outside every polled render region):
 *   - #promptbar (busy / closed / drop chrome)
 *   - #attstage (per-node staged attachment chips)
 *   - #promptrow / #prompt (contenteditable text + resize)
 *   - #sendbtn (send vs interrupt)
 *   - #attadd / #attmenu / #attimg / #attfile (source menu + pickers)
 *
 * Inputs (getters / injected deps — never implicit app globals):
 *   - selected id (sel), nodeById
 *   - isPhoneTouch (desktop Enter vs phone newline)
 *   - localStorage adapter for scimux-draft:<id>
 *   - api (JSON send / interrupt), withCsrf + fetch (multipart upload)
 *   - FormData / URL (createObjectURL / revokeObjectURL) seams
 *   - setInterval / clearInterval / setTimeout
 *   - document (execCommand, menu outside-click / Escape)
 *   - chat seams: setSentEcho, clearSentEchoFor, paintEcho, getChatSig,
 *     getLastTurns, invalidate, refreshChat
 *   - tick / scheduleTick (post-send poll nudge)
 *   - esc (format.js), icons (PLUS / IMAGE / FILE / SEND / STOP)
 *   - alert for non-409 failures
 *
 * Outputs / owned state:
 *   - #prompt textContent / height; #promptbar .busy .closed .drop
 *   - #sendbtn label / stop class / disabled
 *   - #attadd hidden / disabled / armed / aria-expanded
 *   - #attstage chip HTML and per-node in-memory stage map
 *   - draft keys scimux-draft:<node-id> (per device, local only)
 *   - scimux-sendto-pending:<node-id> (durable source intents, local only)
 *   - scimux-sendto-awaiting:<node-id> (expected turn, boundary, write stamp;
 *     local only, expires after AWAITING_FORWARD_TTL_MS)
 *   - composerBusy / composerClosed / canAttach edge-only caches
 *   - stageKey counter for chip identity
 *
 * Server calls owned here:
 *   - POST /api/nodes/:id/send              { text, attachments }
 *   - POST /api/nodes/:id/send/interrupt
 *   - POST /api/nodes/:id/attachments       multipart FormData files
 *
 * Events owned after one idempotent bind():
 *   - #prompt input / paste / keydown / beforeinput
 *   - #sendbtn click
 *   - #attadd click (menu open/place/close)
 *   - #attmenu click (image/file source)
 *   - #attimg / #attfile change
 *   - #promptbar dragover/dragenter/dragleave/dragend/drop
 *   - document click (close open attachment menu) — composer-owned
 *   - document keydown Escape (close open attachment menu) — composer-owned
 *   - dynamic .sx remove buttons rebound each renderStage
 *
 * Storage keys:
 *   - "scimux-draft:" + nodeId  — per-node text drafts (localStorage)
 *   - "scimux-sendto-pending:" + nodeId — unsent source intents
 *   - "scimux-sendto-awaiting:" + nodeId — delivered text awaiting a real turn,
 *     stamped so an unmatched latch expires instead of wedging the node
 *
 * Injected effects (chat / shell):
 *   - setSentEcho / paintEcho / clearSentEchoFor / invalidate / refreshChat
 *   - getChatSig / getLastTurns (echo seen watermark)
 *   - tick after successful send / interrupt
 *
 * Lifecycle:
 *   - createComposerFeature(deps) → { bind, destroy, setComposerBusy,
 *     setComposerClosed, setAttachAvail, setPromptText, clearPrompt,
 *     promptText, renderStage, onSelect, sendPrompt, interruptPrompt,
 *     addFiles, ... }
 *   - bind() is idempotent; destroy() removes every owned listener exactly
 *     once and leaves roots in place (singleton shell DOM)
 *
 * Not owned (stay shell / other features):
 *   - select() orchestration (calls onSelect for draft/stage restore)
 *   - chat head/turns/history/terminal/attention (chat.js)
 *   - notes long-press draft merge into localStorage (writes key directly)
 *   - bookmarks composer (#bookmarkbar), sheets, search, map, cards
 *   - app-level document gestures / title-edit keydown / polling tick body
 *   - CSRF token minting (api.js withCsrf / shell CSRF constant)
 *
 * Reuses (no algorithm duplication):
 *   format.js: esc
 *   api.js: withCsrf shapes via injected withCsrf/api
 *   chat.js: echo policy via setSentEcho / paintEcho / clearSentEchoFor
 *
 * Module size: above the ~500-line soft guide — one factory owns the complete
 * Composer surface (prompt text/resize, drafts, busy/closed chrome, staging,
 * uploads, attachment menu, send/interrupt, failure restore). Pure decisions
 * are exported separately; splitting further would fracture the singleton
 * lifecycle without a second feature packet.
 *
 * Contracts preserved:
 *   - Singleton outside every polled region; polls never rebuild it
 *   - Typing stays enabled while mechanically busy
 *   - Closed nodes disable prompt/send/upload
 *   - New chat is text-only until current segment has a turn
 *   - Drafts and staged files isolated per node
 *   - Destination captured before async upload/send work
 *   - Optimistic echo painted before POST
 *   - Failed sends merge original text before newer typing
 *   - Failed sends restore attachments without revoking retry previews
 *   - Successful sends revoke delivered previews
 *   - Unconfirmed delivery is still success
 *   - 409 restores quietly; other errors alert
 *   - Echo retraction is destination-scoped
 */

import { esc as escDefault } from "./format.js";
import { focusAtEnd } from "./caret.js";
import { peopleFileFailureText } from "./people-preparation.js";
import { removePeopleStage } from "./people-stage-removal.js";
import { loadPeopleLocalInventory, discardPeopleLocalPending, readPeopleLocalPublication, retireAbsentPeopleLocalPublication, parsePeoplePendingSnapshot } from "./people-local-review.js";
import {
  readPendingForwards,
  clearPendingForwards,
  readAwaitingForward,
  writeAwaitingForward,
  bindDraftIdentity,
  archiveRetiredDraft,
  readRetiredDrafts,
  addPromptSource, clearPromptSources, promptSourceKey, readPromptSources, writePromptSources, readPromptFlights, writePromptFlights,
} from "./storage.js";

export function retiredDraftReviewHTML(entries){
  return entries.map(({owner, values}) => {
    const fields = [["Agent draft", values["scimux-draft:"]], ["People draft", values["scimux-people-draft:"]]];
    let flights = [];
    try {
      const parsed = JSON.parse(values["scimux-prompt-flights:"] || "[]");
      if (Array.isArray(parsed)) flights = parsed.filter(item => item && typeof item === "object");
    } catch { /* keep other intact drafts visible */ }
    fields.push(...flights.map(flight => ["Pending Agent draft", flight.text]));
    const text = fields.filter(([, value]) => typeof value === "string" && value).map(([label, value]) =>
      `<label>${label}<textarea readonly>${escDefault(value)}</textarea></label>`).join("");
    const source = values["scimux-prompt-sources:"] || values["scimux-sendto-pending:"] || flights.some(flight => flight.sources?.length)
      ? '<p>Source references are retained with this saved draft.</p>' : "";
    const attachments = values["scimux-attachments:"] ? '<p>File references are retained with this saved draft; files may no longer be available.</p>' : "";
    const temporary = values["scimux-memory-only:"] ? '<p>Available only in this tab.</p>' : "";
    const delivery = values["scimux-delivery-status:"] === "accepted" ? '<p>Accepted by the earlier chat; confirmation is unavailable here.</p>'
      : values["scimux-delivery-status:"] === "unconfirmed" ? '<p>Delivery to the earlier chat is unconfirmed.</p>'
      : values["scimux-delivery-status:"] === "not-dispatched" ? '<p>Not sent.</p>' : "";
    const context = owner === "unknown" ? "Saved before chat identity could be verified." : "Saved from an earlier chat.";
    return `<section><p>${context}</p>${temporary}${delivery}${text}${source}${attachments}</section>`;
  }).join("");
}

/* ---------- public constants ---------- */

export const DRAFT_KEY_PREFIX = "scimux-draft:";
export const PROMPT_MAX_HEIGHT_PX = 120;
export const UPLOAD_WAIT_POLL_MS = 150;
export const UPLOAD_WAIT_TIMEOUT_MS = 12000;

/* ---------- pure decisions ---------- */

/** Normalize contenteditable text: NBSP → regular space. */
export function normalizePromptText(raw){
  return String(raw || "").replace(/\u00a0/g, " ");
}

/** Claude's native /fork command. Leading command only — prose that
 *  mentions "/fork" is not rejected. HTTP remains authoritative. */
export function isClaudeNativeForkCommand(text){
  const s = String(text || "").trim();
  if (s === "/fork") return true;
  if (!s.startsWith("/fork")) return false;
  const next = s.charAt(5);
  return next === " " || next === "\t" || next === "\n" || next === "\r";
}

/** Per-node localStorage key for drafts. */
export function draftStorageKey(nodeId){
  return DRAFT_KEY_PREFIX + (nodeId || "");
}

/** Cap contenteditable height at the production 120px max. */
export function promptHeightPx(scrollHeight, max = PROMPT_MAX_HEIGHT_PX){
  const h = Number(scrollHeight) || 0;
  return Math.min(h, max);
}

/**
 * Desktop bare Enter sends; Shift/Alt/Meta/Ctrl+Enter insert a newline.
 * Phone-touch always leaves Enter alone (software keyboard newline).
 */
export function shouldSendOnEnter({
  key, shiftKey, altKey, metaKey, ctrlKey, isPhoneTouch = false,
} = {}){
  if (isPhoneTouch) return false;
  if (key !== "Enter") return false;
  return !shiftKey && !altKey && !metaKey && !ctrlKey;
}

/** Busy chrome labels/classes (edge-only DOM application in the factory). */
export function busyChrome(active){
  const on = !!active;
  return {
    busy: on,
    ariaLabel: on ? "stop agent" : "send",
    title: on ? "stop agent" : "send",
    stopClass: on,
  };
}

/** Closed-thread chrome: disable send/upload and lock contenteditable. */
export function closedChrome(closed){
  const on = !!closed;
  return {
    closed: on,
    contentEditable: on ? "false" : "true",
    placeholder: on
      ? "Thread closed — fork to continue"
      : "Message the agent…",
    sendDisabled: on,
    attDisabled: on,
  };
}

/** Image MIME detector used when staging a File. */
export function isImageFileType(type){
  return /^image\//.test(type || "");
}

/** Default display name when a File has no name. */
export function stagedFileName(file, isImage){
  if (file && file.name) return file.name;
  return isImage ? "pasted-image.png" : "file";
}

/** Attachment menu source → file input id key. */
export function attachmentInputKey(dataSrc){
  return dataSrc === "image" ? "image" : "file";
}

/**
 * Merge original send text with anything typed while the POST was in flight.
 * Original leads; identical newer text is not duplicated.
 */
/* A distinct local token prevents another tab's replacement flight from
   consuming this request's eventual result. It is not delivery evidence. */
export function newPromptFlightID(now = Date.now(), random = Math.random){
  return `${now}:${random()}:${random()}`;
}

export function mergeFailedDraft(text, newer){
  const t = text || "";
  const n = newer || "";
  if (n && n !== t) return t + "\n\n" + n;
  return t;
}

/**
 * Restore staged items after a failed send. If the operator staged more
 * files while the send was pending, append the failed items after them;
 * otherwise replace with the failed set.
 */
export function restoreStageOnFailure(current, items){
  const cur = current || [];
  const failed = items || [];
  if (!failed.length) return cur.slice();
  return cur.length ? cur.concat(failed) : failed.slice();
}

/** True when there is nothing to send (empty text and no completed refs). */
export function hasSendPayload(text, refs){
  return !!(String(text || "").trim() || (refs && refs.length));
}

/** Collect completed attachment refs from staged items. */
export function completedRefs(items){
  return (items || []).filter(x => x.status === "done" && x.ref).map(x => x.ref);
}

/** True while any staged item is still uploading. */
export function anyUploading(items){
  return (items || []).some(x => x.status === "up");
}

/**
 * Echo watermark: when chat has rendered for this node (chatSig set),
 * pass the current turn count as `seen`; otherwise null and rely on text
 * match / timeout inside chat.js.
 */
export function buildSentEcho({ node, text, atts, at, chatSig, turnsLength }){
  return {
    node,
    text: text || "",
    atts: atts || [],
    at: at || 0,
    seen: chatSig ? (turnsLength || 0) : null,
  };
}

/** Stage chip HTML for one item (escaped name; optional image preview). */
export function stageChipHTML(it, { esc = escDefault, iconFile = "" } = {}){
  const disp = esc(it.name);
  const thumb = it.isImage && it.preview
    ? `<img src="${it.preview}" alt="${disp}">`
    : `<span class="fico">${iconFile}</span>`;
  const spin = it.status === "up" ? `<span class="spin"><i></i></span>` : "";
  const failure = it.status === "err" ? `<span class="stageerror" role="status">${esc(it.error || "Upload failed")}</span>` : "";
  return `<div class="stagechip ${it.status}" data-key="${it.key}" title="${disp}">
        ${thumb}${spin}<span class="snm">${disp}</span>
        <button class="sx" data-key="${it.key}" aria-label="Remove ${disp}">&times;</button>
        ${failure}
      </div>`;
}

/** Full stage row HTML, or empty string when no items. */
export function stageRowHTML(items, deps = {}){
  const list = items || [];
  if (!list.length) return "";
  return list.map(it => stageChipHTML(it, deps)).join("");
}

/* ---------- factory ---------- */

/**
 * Create the singleton composer feature.
 *
 * @param {object} deps
 * @param {object} deps.roots  promptbar, attstage, prompt, sendbtn, attadd,
 *                             attmenu, attimg, attfile
 * @param {function} [deps.sel] selected node id getter
 * @param {function} [deps.nodeById]
 * @param {function} [deps.isPhoneTouch]
 * @param {{getItem,setItem,removeItem}} [deps.storage]
 * @param {function} [deps.api]
 * @param {function} [deps.withCsrf]
 * @param {function} [deps.fetchImpl]
 * @param {function} [deps.FormData]
 * @param {{createObjectURL,revokeObjectURL}} [deps.URL]
 * @param {Document|object} [deps.document]
 * @param {function} [deps.esc]
 * @param {object} [deps.icons] ICON_PLUS, ICON_IMAGE, ICON_FILE, ICON_SEND, ICON_STOP
 * @param {function} [deps.setSentEcho]
 * @param {function} [deps.clearSentEchoFor]
 * @param {function} [deps.paintEcho]
 * @param {function} [deps.getChatSig]
 * @param {function} [deps.getLastTurns]
 * @param {function} [deps.invalidateChat]
 * @param {function} [deps.refreshChat]
 * @param {function} [deps.tick]
 * @param {function} [deps.scheduleTick]
 * @param {function} [deps.alert]
 * @param {function} [deps.setInterval]
 * @param {function} [deps.clearInterval]
 * @param {function} [deps.setTimeout]
 * @param {function} [deps.now]
 */
export function createComposerFeature(deps){
  const d = deps || {};
  const roots = d.roots || {};
  const promptbar = roots.promptbar;
  const attstage = roots.attstage;
  const prompt = roots.prompt;
  const sendbtn = roots.sendbtn;
  const attadd = roots.attadd;
  const attmenu = roots.attmenu;
  const attimg = roots.attimg;
  const attfile = roots.attfile;

  const g = (name, fallback) => {
    const v = d[name];
    return typeof v === "function" ? v() : (v !== undefined ? v : fallback);
  };
  const storage = d.storage || {
    getItem: () => null,
    setItem: () => {},
    removeItem: () => {},
  };
  const escFn = d.esc || escDefault;
  const icons = d.icons || {};
  const FormDataImpl = d.FormData || (typeof FormData !== "undefined" ? FormData : null);
  const URLImpl = d.URL || (typeof URL !== "undefined" ? URL : {
    createObjectURL: () => "",
    revokeObjectURL: () => {},
  });
  const doc = d.document || (typeof document !== "undefined" ? document : null);
  const setIntervalFn = d.setInterval || setInterval;
  const clearIntervalFn = d.clearInterval || clearInterval;
  const setTimeoutFn = d.setTimeout || setTimeout;
  const nowFn = () => (typeof d.now === "function" ? d.now() : Date.now());
  const alertFn = typeof d.alert === "function" ? d.alert : (typeof alert !== "undefined" ? alert : () => {});

  /* per-node stage map — in-memory only; reload resets */
  const stage = {};
  const retiredMemory = new Map();
  const fallbackIdentities = new WeakMap();
  let fallbackIdentity = 0;
  let stageKey = 0;

  let composerBusy = null;
  let composerClosed = null;
  let canAttach = false;
  let bound = false;
  let composerNode = "";
  let composerIdentity = "";
  let identityReady = true;
  let destination = "agent";
  let destinationVisit = 0;
  let explicitAgent = false;
  let contextKey = "";
  let contextDraftBinding = "";
  let ownerChoice = false;
  const peopleRevisions = new Map();
  const peopleDraftStates = new Map();
  const capturedDraftFlights = new Set();
  const lastPeopleDraftScopes = new Map();
  let unavailablePeopleDraft = null;
  const agentRevisions = new Map();
  const peopleSending = new Map();
  let peopleLifetime = 0;
  const sendingDestinations = new Set();
  const recoveredPeopleFiles = new Set();
  const recoveringPeopleFiles = new Map();
  let legacyPeopleReview = {identity:"", files:[], drafts:[], unavailable:false};
  const clearedSourceText = new Map();
  const damagedSourceMetadata = new Set();
  const recoveredSourceFiles = new Set(), recoveringSourceFiles = new Set();
  const preparationControllers = new Set();
  const cleanups = [];
  /* dynamic remove buttons rebound on each renderStage */
  let stageRemoveCleanups = [];
  let localReview = null;
  let localReviewControllers = [];
  let localReviewAlive = true;
  const localDiscardFlights = new Set();
  const localStageFlights = new Set();

  let textReview = null;
  const textRetirementFlights = new Set();

  function textReviewSnapshot(){
    const id = selId(), context = peopleContext();
    if (!id || (isPeople() && context?.key && (id.startsWith("team/") ? id !== "team/" + context.key : !context.owner))) return null;
    const contexts = isPeople() ? (context?.key ? [context] : []) : (typeof d.localReviewContexts === "function" ? d.localReviewContexts() : []);
    const copies = contexts.map(copy => ({key:copy.key, copyInstance:copy.copyInstance || "", binding:copy.binding || "", owner:!!copy.owner}));
    return {copies, address:JSON.stringify([id, destinationVisit, stageIdentity(id), copies])};
  }
  function collectTextReviews(pending){
    const view = textReviewSnapshot();
    if (!view) { textReview = null; return []; }
    if (textReview?.address !== view.address) textReview = {address:view.address, items:new Map(), notice:""};
    const results = [], seen = new Set();
    function add(record, copy){
      if (!record?.request || typeof record.request.text !== "string" || typeof record.request.key !== "string") return;
      if (!record.publication) { if (record === pending) results.push({snapshot:record}); return; }
      if (!/^[0-9a-f]{32}:[1-9][0-9]*$/.test(record.publication) || typeof record.scope !== "string" || typeof record.head !== "string" || typeof record.version !== "string" || typeof record.binding !== "string" || typeof record.native !== "string" || record.peopleKey !== copy.key || record.copyInstance !== copy.copyInstance || record.native !== (copy.owner ? (nodeById(selId())?.instance_id || "") : "") || record.scope !== peopleStageIdentity(selId(), {...copy, binding:record.binding}) || !/^[0-9a-f]{16}$/.test(record.request.key) || !Number.isSafeInteger(record.request.anchor)) return;
      const key = JSON.stringify([record.scope, record.publication]);
      if (seen.has(key)) return;
      seen.add(key);
      const saved = JSON.stringify(record), prior = textReview.items.get(key);
      const item = prior && prior.saved === saved ? prior : {key, saved, snapshot:JSON.parse(saved), open:false, error:false};
      textReview.items.set(key, item); results.push(item);
    }
    if (pending && view.copies[0]) add(pending, view.copies[0]);
    for (const copy of view.copies) {
      try {
        const retained = d.readRetainedPeopleTextPending?.(copy);
        if (Array.isArray(retained?.entries)) for (const record of retained.entries) add(record, copy);
      } catch { /* keep other reviews available */ }
    }
    return results;
  }
  async function retireTextReview(view, item){
    const current = () => localReviewAlive && peopleLifetime === view.lifetime && textReview === view.review && textReviewSnapshot()?.address === view.address;
    if (!current() || textRetirementFlights.has(item.key)) return;
    textRetirementFlights.add(item.key); item.error = false; renderStage();
    try {
      if (typeof d.retirePeopleText !== "function") throw new Error("People text recovery is unavailable.");
      const result = await d.retirePeopleText(item.snapshot, {current});
      if (!current()) return;
      if (result?.state === "retired") view.review.notice = "Local text retry retired; this does not retract a remote comment.";
      else if (result?.state === "completed" && Number.isSafeInteger(result.seq) && result.seq > 0) view.review.notice = "Already completed at sequence " + result.seq + ".";
      else throw new Error("People text recovery is unconfirmed.");
    } catch {
      if (current()) item.error = true;
    } finally {
      textRetirementFlights.delete(item.key);
      if (current()) renderStage();
    }
  }

  function localDiscardKey(copy, id){ return JSON.stringify([copy.key, copy.copyInstance, id]); }
  function localDiscardHTML(row, index, entry){
    const state = row.discards.get(entry.id);
    if (state?.state === "locally_discarded") return '<p role="status">Locally discarded. A remote comment may still appear.</p>';
    if (state?.state === "already_completed") return `<p role="status">Already completed at sequence ${state.seq}.</p>`;
    const notice = state === "error" ? '<p role="status">Local discard unavailable. If the saved record changed, reopen review before trying again.</p>' : "";
    const progress = localDiscardFlights.has(localDiscardKey(row.copy, entry.id)) ? '<p role="status">Discarding locally or the result is unavailable in this view.</p>' : "";
    return `${notice}${progress}<p>Reviewed local revision: ${escFn(entry.revision)}</p><p>A remote comment may still appear. Discard retires local retry; it does not retract a remote comment. Saved files remain.</p><button type="button" class="people-local-discard btn-tinted" data-row="${index}" data-id="${entry.id}">Discard local pending message</button>`;
  }
  async function discardLocalReview(visit, row, entry){
    const current = () => localReviewAlive && localReview === visit && localReviewAddress() === visit.address && row.entries.includes(entry);
    if (!current()) return;
    const item = row.browserState;
    const captured = item?.state === "ready" && item.result.state === "pending" && item.snapshot.publicationKey === entry.id && browserSnapshotCurrent(visit, row, item, true) ? item.snapshot : null;
    const copy = {...row.copy}, record = {kind:entry.kind, id:entry.id, revision:entry.revision};
    const flight = localDiscardKey(copy, record.id), lifetime = peopleLifetime, originalID = selId();
    if (localDiscardFlights.has(flight)) return;
    localDiscardFlights.add(flight);
    row.discards.delete(record.id);
    renderStage();
    const owned = () => localReviewAlive && peopleLifetime === lifetime;
    try {
      if (!current()) return;
      const saved = captured ? await fencePeopleFinalization(captured, row, originalID) : null;
      if (!current()) return;
      const result = await discardPeopleLocalPending({api:d.localReviewApi, key:copy.key, copyInstance:copy.copyInstance, record, current:owned});
      if (!owned()) return;
      let cleared = true;
      if (captured && result.state === "already_completed") item.result = {...item.result, state:"completed", seq:result.seq};
      if (captured && result.state === "locally_discarded") {
        item.retired = true; item.cleanupError = true;
        cleared = await clearRetiredBrowserSnapshot(captured, saved.identity, {state:"locally_discarded"}, owned);
        if (cleared) {
          item.cleanupError = false;
          if (captured.format === "transactional") row.browserState = null;
          else item.legacyRecorded = true;
        }
      }
      if (!current()) return;
      await Promise.resolve();
      if (!current()) return;
      row.discards.set(record.id, cleared ? result : "error");
    } catch {
      if (!current()) return;
      row.discards.set(record.id, "error");
    } finally {
      localDiscardFlights.delete(flight);
    }
    if (current()) renderStage();
  }

  async function removeLocalStage(visit, row, entry){
    const current = () => localReviewAlive && localReview === visit && localReviewAddress() === visit.address && row.entries.includes(entry);
    if (!current()) return;
    const copy = {...row.copy}, flight = localDiscardKey(copy, entry.id);
    if (localStageFlights.has(flight)) return;
    localStageFlights.add(flight); row.stageErrors.delete(entry.id); renderStage();
    try {
      if (!current()) return;
      await removePeopleStage(d.localReviewApi, {peopleKey:copy.key, copyInstance:copy.copyInstance, ref:{id:entry.id}, stageRevision:entry.revision});
      if (!current()) return;
      row.entries = row.entries.filter(item => item !== entry);
    } catch {
      if (current()) row.stageErrors.add(entry.id);
    } finally { localStageFlights.delete(flight); }
    if (current() || (localReview === visit && localReviewAddress() === visit.address)) renderStage();
  }
  function localStageHTML(row, index, entry){
    const flight = localDiscardKey(row.copy, entry.id);
    const error = row.stageErrors.has(entry.id) ? '<p role="status">Saved file removal is unavailable. Review the file and try again.</p>' : "";
    const progress = localStageFlights.has(flight) ? '<p role="status">Removing saved file…</p>' : "";
    return `<p>${escFn(entry.name || "File metadata unavailable")} — ${escFn(entry.mime || "Unknown type")} — ${escFn(entry.size ?? "Unknown size")} bytes — ${escFn(entry.hash || "Hash unavailable")}</p><p>Reviewed local revision: ${escFn(entry.revision)}</p><p>The browser draft file is kept.</p>${error}${progress}<button type="button" class="people-local-stage-remove btn-tinted" data-row="${index}" data-id="${entry.id}">Remove local upload</button>`;
  }

  function localReviewSnapshot(){
    const id = selId();
    if (typeof d.localReviewApi !== "function" || !id) return null;
    const context = peopleContext();
    if (isPeople() && context?.key && (id.startsWith("team/") ? id !== "team/" + context.key : !context.owner)) return null;
    const contexts = isPeople() ? (context?.key ? [context] : []) : (typeof d.localReviewContexts === "function" ? d.localReviewContexts() : []);
    if (!contexts.length) return null;
    const copies = contexts.map(item => ({key:item.key, copyInstance:item.copyInstance || "", binding:item.binding || "", owner:!!item.owner}));
    const incarnation = stageIdentity(id);
    return {copies, incarnation, address:JSON.stringify([id, destinationVisit, incarnation, copies])};
  }
  function localReviewAddress(){ return localReviewSnapshot()?.address || ""; }
  function stopLocalReview(){
    for (const controller of localReviewControllers) controller.abort();
    localReviewControllers = [];
    localReview = null;
  }
  function reviewedBrowserSnapshot(record, copy, instance) {
    let snapshot = parsePeoplePendingSnapshot(record.value, {key:copy.key, copyInstance:copy.copyInstance, binding:record.binding ?? copy.binding, instance});
    if (record.format === "transactional") {
      if (record.scope !== peopleStageIdentity(selId(), {...copy, binding:record.binding}) || typeof record.publication !== "string" || !/^[0-9a-f]{32}:[1-9][0-9]*$/.test(record.publication) || record.storageKey !== "idb:" + record.publication || typeof record.head !== "string" || typeof record.version !== "string") throw new Error("People recovery is unavailable.");
      snapshot = {...snapshot, format:"transactional", scope:record.scope, publication:record.publication, head:record.head, version:record.version, storageKey:record.storageKey};
    }
    if (record.storageKey !== snapshot.storageKey || record.key !== snapshot.key || record.instance !== instance || (record.publicationKey !== undefined && record.publicationKey !== snapshot.publicationKey) || record.text !== snapshot.text || JSON.stringify(record.attachments) !== JSON.stringify(snapshot.attachments)) throw new Error("People recovery is unavailable.");
    return snapshot;
  }
  function readBrowserPending(row, current){
    if (!isPeople() || typeof d.pendingPeople !== "function") return;
    let pending, snapshot;
    const copy = row.copy;
    const instance = copy.owner ? nodeById(selId())?.instance_id : "";
    try {
      pending = d.pendingPeople();
      if (!pending) return;
      snapshot = reviewedBrowserSnapshot(pending, copy, instance);
    } catch { return; }
    const value = pending.value, storageKey = pending.storageKey;
    if (row.browserState?.value !== value) row.activeOpen = false;
    const attempt = {value, snapshot, state:"loading"};
    const stillCurrent = () => {
      if (!localReviewAlive || !current() || !isPeople() || row.browserState !== attempt) return false;
      try { const latest = d.pendingPeople(); return latest?.value === value && latest.storageKey === storageKey; }
      catch { return false; }
    };
    const controller = new AbortController();
    localReviewControllers.push(controller);
    row.browserState = attempt;
    readPeopleLocalPublication({api:d.localReviewApi, key:copy.key, copyInstance:copy.copyInstance, publicationKey:snapshot.publicationKey, signal:controller.signal, current:stillCurrent}).then(result => {
      if (!stillCurrent()) return;
      row.browserState = {value, snapshot, state:"ready", result};
      renderStage();
    }).catch(() => {
      if (!stillCurrent()) return;
      row.browserState = {value, snapshot, state:"error"};
      renderStage();
    });
  }
  function loadRetainedPending(row){
    if (typeof d.readRetainedPeoplePending !== "function") return;
    try {
      const result = d.readRetainedPeoplePending(row.copy);
      row.retainedUnavailable = result?.unavailable !== false || !Array.isArray(result.entries);
      if (!Array.isArray(result?.entries)) return;
      const instance = row.copy.owner ? nodeById(selId())?.instance_id : "";
      const previous = row.retained, retained = [];
      for (const record of result.entries) {
        try {
          const snapshot = reviewedBrowserSnapshot(record, row.copy, instance);
          retained.push(previous.find(item => item.snapshot.storageKey === snapshot.storageKey && item.snapshot.value === snapshot.value) || {snapshot, open:false, state:"unread"});
        } catch { row.retainedUnavailable = true; }
      }
      row.retained = retained;
    } catch { row.retainedUnavailable = true; }
  }
  function readRetainedPending(visit, row, item){
    const snapshot = item.snapshot;
    const current = () => {
      if (!localReviewAlive || localReview !== visit || localReviewAddress() !== visit.address || !row.retained.includes(item)) return false;
      try {
        const result = d.readRetainedPeoplePending(row.copy);
        return Array.isArray(result?.entries) && result.entries.some(value => value.storageKey === snapshot.storageKey && value.value === snapshot.value);
      } catch { return false; }
    };
    if (!current()) return;
    item.state = "loading";
    const controller = new AbortController(); localReviewControllers.push(controller);
    readPeopleLocalPublication({api:d.localReviewApi, key:row.copy.key, copyInstance:row.copy.copyInstance, publicationKey:snapshot.publicationKey, signal:controller.signal, current}).then(result => {
      if (!current()) return;
      item.state = "ready"; item.result = result; renderStage();
    }).catch(() => {
      if (!current()) return;
      item.state = "error"; renderStage();
    });
  }
  function completedSnapshotMatches(item){
    return item?.state === "ready" && item.result.state === "completed" &&
      item.result.text === item.snapshot.text && JSON.stringify(item.result.stage_ids) === JSON.stringify(item.snapshot.attachments);
  }
  function browserRetireHTML(row, index, item, marker){
    if (item?.state !== "ready" || (!item.retired && !completedSnapshotMatches(item) && !["missing", "absent_retired", "retired"].includes(item.result.state))) return "";
    if (item.legacyRecorded) return '<p role="status">The legacy retry is preserved for review; its local decision is recorded.</p>';
    const flight = localDiscardKey(row.copy, item.snapshot.publicationKey);
    const status = item.cleanupError ? '<p role="status">Local retry cleanup is unconfirmed. Review and try again.</p>' : "";
    if (localDiscardFlights.has(flight)) return status + '<p role="status">Retiring local retry…</p>';
    const label = completedSnapshotMatches(item) ? "Finish local recovery" : "Clear local retry";
    return status + `<p>A remote comment may still appear. Saved files remain.</p><button type="button" class="people-browser-retire btn-tinted" data-row="${index}" data-snapshot="${marker}">${label}</button>`;
  }
  function browserSnapshotCurrent(visit, row, item, active){
    if (!localReviewAlive || localReview !== visit || localReviewAddress() !== visit.address || item.state !== "ready") return false;
    const snapshot = item.snapshot;
    try {
      if (active) {
        const pending = d.pendingPeople();
        return row.browserState === item && pending?.storageKey === snapshot.storageKey && pending.value === snapshot.value;
      }
      const records = d.readRetainedPeoplePending(row.copy);
      return row.retained.includes(item) && Array.isArray(records?.entries) && records.entries.some(value => value.storageKey === snapshot.storageKey && value.value === snapshot.value);
    } catch { return false; }
  }
  async function fencePeopleFinalization(snapshot, row, id) {
    for (const token of peopleSending.values()) {
      if (token.pending?.storageKey === snapshot.storageKey && token.pending.value === snapshot.value) {
        token.retiring = true;
        releasePeopleFlight(token);
      }
    }
    const identity = peopleStageIdentity(id, {key:snapshot.key, copyInstance:snapshot.copyInstance, binding:snapshot.binding, owner:row.copy.owner});
    if (!identity || typeof d.peopleFileStore?.list !== "function") throw new Error("Saved file review is unavailable.");
    const rows = await d.peopleFileStore.list(identity);
    return {identity, rows};
  }
  async function clearRetiredBrowserSnapshot(snapshot, scope, proof, current){
    const fenced = d.fencePeoplePending(snapshot);
    if (fenced?.state !== "locally_retired") return false;
    return await d.clearPeoplePending(snapshot, {scope, proof, current});
  }
  function isLocalReviewRequired(error){
    if (error?.status !== 409) return false;
    try { return JSON.parse(error.message)?.error === "review_required"; } catch { return false; }
  }
  async function retireBrowserSnapshot(visit, row, item, active){
    const snapshot = item.snapshot, result = item.result;
    const current = () => browserSnapshotCurrent(visit, row, item, active);
    if (!current() || (!item.retired && !completedSnapshotMatches(item) && !["missing", "absent_retired", "retired"].includes(result.state))) return;
    const flight = localDiscardKey(row.copy, snapshot.publicationKey), lifetime = peopleLifetime, originalID = selId(), originalRevision = peopleRevisions.get(selId()) || 0;
    if (localDiscardFlights.has(flight)) return;
    localDiscardFlights.add(flight); item.cleanupError = false; renderStage();
    const owned = () => localReviewAlive && peopleLifetime === lifetime;
    try {
      if (!current()) return;
      const saved = await fencePeopleFinalization(snapshot, row, originalID);
      if (!current()) return;
      const completed = completedSnapshotMatches(item);
      if (completed && result.pending_pinned && !item.completionReleased) {
        const released = await discardPeopleLocalPending({api:d.localReviewApi, key:snapshot.key, copyInstance:snapshot.copyInstance, record:{kind:"pending", id:snapshot.publicationKey, revision:result.revision}, current:owned});
        if (released.state !== "already_completed" || released.seq !== result.seq) throw new Error("Completed publication changed; review it again.");
        item.completionReleased = true;
      } else if (!completed && !item.retired && result.state !== "retired") await retireAbsentPeopleLocalPublication({api:d.localReviewApi, key:snapshot.key, copyInstance:snapshot.copyInstance, publicationKey:snapshot.publicationKey, current:owned});
      if (!owned()) return;
      if (!completed) item.retired = true;
      item.cleanupError = true;
      const proof = completed ? {state:"completed", seq:result.seq, text:result.text, stage_ids:result.stage_ids} : {state:"locally_discarded"};
      const cleared = await clearRetiredBrowserSnapshot(snapshot, saved.identity, proof, owned);
      if (!owned()) return;
      if (cleared) {
        item.cleanupError = false;
        if (snapshot.format === "transactional") {
          if (cleared.draftCleared) {
            const draft = peopleDraftStates.get(saved.identity);
            if (draft?.head === snapshot.head && draft.version === snapshot.version && (peopleRevisions.get(originalID) || 0) === originalRevision) {
              draft.head = undefined; draft.version = undefined; draft.text = "";
              if (localReview === visit && localReviewAddress() === visit.address && selId() === originalID && isPeople() && peopleStageIdentity(originalID) === saved.identity && promptText() === snapshot.text) clearPrompt();
            }
          }
          if (cleared.filesConfirmed) for (const file of saved.rows) {
            const staged = (stage[saved.identity] || []).find(value => value.localID === file.id && value.fileToken === file.fileToken && value.stageRevision === file.stageRevision);
            if (staged && snapshot.attachments.includes(file.id)) { staged.sent = true; staged.status = "err"; staged.error = "Sent; remove this saved file to finish cleanup."; }
          }
          if (localReview === visit && localReviewAddress() === visit.address) {
            if (active && row.browserState === item) row.browserState = null;
            else if (!active && row.retained.includes(item)) row.retained.splice(row.retained.indexOf(item), 1);
          }
        } else item.legacyRecorded = true;
      }
      else if (current()) item.cleanupError = true;
    } catch (error) {
      if (current()) {
        if (!item.retired && !item.completionReleased && isLocalReviewRequired(error)) {
          if (active) readBrowserPending(row, () => localReview === visit && localReviewAddress() === visit.address);
          else readRetainedPending(visit, row, item);
        } else item.cleanupError = true;
      }
    } finally { localDiscardFlights.delete(flight); }
    if (localReview === visit && localReviewAddress() === visit.address && owned()) renderStage();
  }
  function browserPendingStateHTML(browserState){
    if (browserState?.state === "loading") return '<p role="status">Checking local publication state…</p>';
    if (browserState?.state === "error") return '<p role="status">Local publication state is unavailable.</p>';
    if (browserState?.state !== "ready") return "";
    const result = browserState.result;
    if (browserState.retired) return '<p role="status">The local publication was retired.</p>';
    if (result.state === "missing") return '<p role="status">No local publication record was found.</p>';
    if (result.state === "absent_retired") return '<p role="status">The absent local publication key was retired.</p>';
    const status = result.state === "completed" ? `Completed at sequence ${result.seq}${result.pending_pinned && !browserState.completionReleased ? "; local pending record remains pinned" : ""}.` : `Local publication ${result.state}.`;
    return `<p role="status">${status}</p><label>Actual local text<textarea readonly>${escFn(result.text || "File-only message")}</textarea></label><p>Actual local stage IDs: ${result.stage_ids.map(escFn).join(", ")}</p><p>Actual local revision: ${escFn(result.revision)}</p>`;
  }
  function browserPendingReviewHTML(pending, names, stateHTML, action, attributes = "", retry = ""){
    return `<details${attributes}><summary>Review unconfirmed People message</summary><label>Saved text<textarea readonly>${escFn(pending.text || "File-only message")}</textarea></label><p>${names}</p>${stateHTML}${retry}${action}</details>`;
  }
  function refreshLocalReview(){
    if (!localReviewAlive) return;
    const snapshot = localReviewSnapshot();
    if (localReview?.address === snapshot?.address) return;
    stopLocalReview();
    if (!snapshot) return;
    const {address, copies, incarnation} = snapshot;
    const rows = copies.map(copy => ({copy, state:copy.owner && !incarnation ? "error" : "loading", entries:[], discards:new Map(), stageErrors:new Set(), retained:[], retainedUnavailable:false, activeOpen:false}));
    const visit = {address, rows};
    localReview = visit;
    const current = () => localReview === visit && localReviewAddress() === address;
    for (const row of rows) {
      if (row.state === "error") continue;
      loadRetainedPending(row);
      if (typeof d.refreshRetainedPeopleRecovery === "function") Promise.resolve().then(() => d.refreshRetainedPeopleRecovery(row.copy)).then(() => {
        if (!current()) return;
        loadRetainedPending(row); renderStage();
      }).catch(() => { if (current()) { row.retainedUnavailable = true; renderStage(); } });
      const controller = new AbortController();
      localReviewControllers.push(controller);
      loadPeopleLocalInventory({api:d.localReviewApi, key:row.copy.key, copyInstance:row.copy.copyInstance, signal:controller.signal, current}).then(entries => {
        if (!current()) return;
        row.state = "ready"; row.entries = entries;
        renderStage();
      }).catch(() => {
        if (!current()) return;
        row.state = "error";
        renderStage();
      });
      readBrowserPending(row, current);
    }
  }

  function selId(){ return g("sel", "") || ""; }
  function nodeById(id){
    return typeof d.nodeById === "function" ? d.nodeById(id) : null;
  }
  function stageIdentity(id){
    const node = nodeById(id);
    if (!node) return "";
    if (node.instance_id) return id + "\0" + node.instance_id;
    // Tests and older local adapters can provide stable object identities;
    // production requires the server-minted instance before dispatch.
    if (d.requireChatIdentity) return "";
    if (!fallbackIdentities.has(node)) fallbackIdentities.set(node, ++fallbackIdentity);
    return id + "\0fallback:" + fallbackIdentities.get(node);
  }
  function peopleContext(){
    return typeof d.peopleContext === "function" ? d.peopleContext() : null;
  }
  function peopleCanAttach(context = peopleContext()){
    return !!context && context.attachments === true && !!context.writable && !!context.key;
  }
  function peopleStageIdentity(id, context = peopleContext()){
    if (!id || !context?.key) return "";
    if (id.startsWith("team/") && id !== "team/" + context.key) return "";
    const ownerIdentity = context.owner ? stageIdentity(id) : "";
    if (context.owner && !ownerIdentity) return "";
    return "people\0" + context.key + (context.binding ? "\0binding:" + context.binding : "") + (context.copyInstance ? "\0copy:" + context.copyInstance : "") + "\0" + ownerIdentity;
  }
  function currentStageIdentity(id = selId()){
    return isPeople() ? peopleStageIdentity(id) : stageIdentity(id);
  }
  function attachmentAvailable(){ return isPeople() ? peopleCanAttach() : canAttach; }
  function peopleStageID(){
    const bytes = typeof d.randomBytes === "function" ? d.randomBytes(8) : null;
    if (!bytes || bytes.length !== 8 || Array.from(bytes).some(value => !Number.isInteger(value) || value < 0 || value > 255)) throw new Error("People file identity is unavailable.");
    return Array.from(bytes, value => value.toString(16).padStart(2, "0")).join("");
  }
  function sourceScope(identity){ return "agent-source\0" + identity; }
  function clearedSources(identity){
    if (clearedSourceText.has(identity)) return clearedSourceText.get(identity);
    let entries = [];
    try {
      const saved = JSON.parse(storage.getItem("scimux-cleared-source-text:" + identity) || "[]");
      if (!Array.isArray(saved) || saved.some(value => typeof value !== "string")) throw new Error("invalid source metadata");
      entries = saved;
    } catch { damagedSourceMetadata.add(identity); }
    const result = new Set(entries); clearedSourceText.set(identity, result); return result;
  }
  function saveClearedSources(identity, entries){
    clearedSourceText.set(identity, entries);
    if (!damagedSourceMetadata.has(identity)) storage.setItem("scimux-cleared-source-text:" + identity, JSON.stringify([...entries]));
  }
  function unresolvedSourceFiles(id, identity){
    if (!identity) return false;
    const cleared = clearedSources(identity), items = stage[identity] || [], sources = readPromptSources(storage, id);
    if (items.some(item => item.sourceKey && !item.sent && !sources.some(source => promptSourceKey(source) === item.sourceKey))) return true;
    if (damagedSourceMetadata.has(identity) && sources.length) return true;
    if (recoveredSourceFiles.has(identity)) return false;
    return sources.some(source => {
      const key = promptSourceKey(source);
      return cleared.has(key) && !items.some(item => item.sourceKey === key && !item.sent);
    });
  }
  function restoreSourceFiles() {
    const id = selId(), identity = stageIdentity(id), visit = destinationVisit;
    if (!id || id.startsWith("team/") || !identity || !d.peopleFileStore || recoveredSourceFiles.has(identity) || recoveringSourceFiles.has(identity)) return;
    recoveringSourceFiles.add(identity); let stale = false;
    Promise.resolve().then(() => d.peopleFileStore.list(sourceScope(identity))).then(rows => {
      if (selId() !== id || destinationVisit !== visit || stageIdentity(id) !== identity) { stale = true; return; }
      const arr = staged(identity);
      const previousSources = readPromptSources(storage, id);
      const accepted = new Set(readPromptFlights(storage, id).filter(flight => flight.identity === identity && flight.accepted).flatMap(flight => (flight.attachments || []).map(item => item.sourceLocalID).filter(Boolean)));
      for (const row of rows) {
        if (!row.metadata?.source || !row.metadata?.sourceFileID || arr.some(item => item.sourceLocalID === row.id)) continue;
        const sourceKey = promptSourceKey(row.metadata.source);
        if (!sourceKey) continue;
        const file = row.file, isImage = isImageFileType(file.type);
        const sent = !!row.published || accepted.has(row.id);
        if (!sent) addPromptSource(storage, id, row.metadata.source);
        arr.push({key:++stageKey, file, size:file.size, name:stagedFileName(file,isImage), isImage, preview:isImage ? URLImpl.createObjectURL(file) : "", ref:null, status:"err", error:sent ? "Accepted; remove this saved file to finish cleanup." : "Review and send to retry this file.", sourceKey, sourceLocalID:row.id, sourceFileID:row.metadata.sourceFileID, source:row.metadata.source, localSaved:true, sent, needsUpload:!sent});
      }
      recoveredSourceFiles.add(identity);
      const cleared = clearedSources(identity);
      const before = readPromptSources(storage, id);
      const after = before.filter(source => !cleared.has(promptSourceKey(source)) || arr.some(item => item.sourceKey === promptSourceKey(source) && !item.sent));
      if (after.length !== before.length) {
        writePromptSources(storage, id, after);
      }
      if (JSON.stringify(previousSources) !== JSON.stringify(after) && typeof d.promptSourcesChanged === "function") d.promptSourcesChanged(id);
      renderRecovery(id);
      if (!isPeople()) renderStage();
    }).catch(() => { if (selId() === id && destinationVisit === visit && stageIdentity(id) === identity) { renderRecovery(id); alertFn("Saved Agent source files could not be recovered."); } })
      .finally(() => { recoveringSourceFiles.delete(identity); if (stale && selId() === id && stageIdentity(id) === identity) restoreSourceFiles(); });
  }
  function restorePeopleFiles(){
    const id = selId(), context = peopleContext(), identity = peopleStageIdentity(id, context), visit = destinationVisit, lifetime = peopleLifetime;
    if (!isPeople() || !peopleCanAttach(context) || !identity || !d.peopleFileStore || recoveredPeopleFiles.has(identity) || recoveringPeopleFiles.has(identity)) return;
    let stale = false;
    const recovery = Promise.resolve().then(() => d.peopleFileStore.list(identity)).then(rows => {
      if (peopleLifetime !== lifetime || selId() !== id || destinationVisit !== visit || !isPeople() || peopleStageIdentity(id) !== identity) { stale = true; return; }
      const arr = staged(identity);
      for (const row of rows) {
        const existing = arr.find(item => item.localID === row.id);
        if (existing) {
          if (Number.isSafeInteger(row.published) && row.published > 0 && existing.fileToken && existing.fileToken === row.fileToken && existing.stageRevision && existing.stageRevision === row.stageRevision) {
            existing.sent = true; existing.needsUpload = false; existing.status = "err"; existing.error = "Sent; remove this saved file to finish cleanup.";
          }
          continue;
        }
        const file = row.file, isImage = isImageFileType(file.type);
        arr.push({key:++stageKey, file, size:file.size, name:stagedFileName(file, isImage), isImage,
          preview:isImage ? URLImpl.createObjectURL(file) : "", ref:{id:row.id}, localID:row.id, localSaved:true,
          peopleKey:context.key, copyInstance:context.copyInstance || "", instance:context.owner ? (nodeById(id)?.instance_id || "") : "",
          stageRevision:row.stageRevision, fileToken:row.fileToken,
          status:"err", error:row.published ? (row.stageRevision ? "Sent; remove this saved file to finish cleanup." : "Sent; review the current local file before removing this saved file.") : "Review and send to retry this file.",
          sent:!!row.published, needsUpload:!row.published});
      }
      recoveredPeopleFiles.add(identity);
      renderStage();
    }).catch(() => {
      if (peopleLifetime !== lifetime || selId() !== id || destinationVisit !== visit || !isPeople() || peopleStageIdentity(id) !== identity) stale = true;
      else alertFn("Saved People files could not be recovered.");
    }).finally(() => {
      if (recoveringPeopleFiles.get(identity) === recovery) recoveringPeopleFiles.delete(identity);
      if (stale && bound && selId() === id && isPeople() && peopleStageIdentity(id) === identity) restorePeopleFiles();
    });
    recoveringPeopleFiles.set(identity, recovery);
  }
  function clearLegacyPeopleReview(){
    for (const item of legacyPeopleReview.files) URLImpl.revokeObjectURL(item.url);
    legacyPeopleReview = {identity:"", files:[], drafts:[], unavailable:false};
  }
  function restoreLegacyPeopleFiles(){
    const id = selId(), context = peopleContext(), identity = peopleStageIdentity(id, context), visit = destinationVisit;
    if (!id || !context?.copyInstance || !identity) {
      if (legacyPeopleReview.identity) clearLegacyPeopleReview();
      return;
    }
    if (identity === legacyPeopleReview.identity) return;
    clearLegacyPeopleReview();
    legacyPeopleReview.identity = identity;
    const oldScope = peopleStageIdentity(id, {...context, copyInstance:""});
    const prefix = "people\0" + context.key + (context.binding ? "\0binding:" + context.binding : "");
    const ownerIdentity = context.owner ? stageIdentity(id) : "";
    const missingStore = !d.peopleFileStore;
    let failedStore = missingStore;
    Promise.resolve().then(() => {
      if (missingStore) return [];
      return typeof d.peopleFileStore.listRetired === "function" ? d.peopleFileStore.listRetired(prefix, ownerIdentity, identity) : d.peopleFileStore.list(oldScope);
    }).catch(() => { failedStore = true; return []; }).then(rows => {
      if (selId() !== id || destinationVisit !== visit || peopleStageIdentity(id) !== identity || legacyPeopleReview.identity !== identity) return;
      if (failedStore) legacyPeopleReview.unavailable = true;
      const priorCopies = new Set();
      for (const row of rows) {
        legacyPeopleReview.files.push({name:row.file.name, url:URLImpl.createObjectURL(row.file)});
        const marker = prefix + "\0copy:";
        if (typeof row.scope === "string" && row.scope.startsWith(marker)) {
          const copy = row.scope.slice(marker.length, marker.length + 32);
          if (/^[0-9a-f]{32}$/.test(copy)) priorCopies.add(copy);
        }
      }
      const native = context.owner ? (nodeById(id)?.instance_id || "") : "";
      if (typeof storage.keys === "function") {
        const keys = storage.keys();
        if (!Array.isArray(keys)) legacyPeopleReview.unavailable = true;
        else {
          const draftPrefix = "scimux-people-draft:" + id + ":scope:";
          const bindingPrefix = context.key + (context.binding ? "\0binding:" + context.binding : "") + "\0copy:";
          const bindingSuffix = "\0" + (context.owner ? stageIdentity(id) : id);
          const pendingPrefix = "scimux-people-file-pending:" + context.key + ":" + native + (context.binding ? ":" + context.binding : "") + ":copy:";
          for (const key of keys) {
            let copy = "";
            if (key.startsWith(draftPrefix)) {
              try {
                const scope = decodeURIComponent(key.slice(draftPrefix.length));
                if (scope.startsWith(bindingPrefix) && scope.endsWith(bindingSuffix)) copy = scope.slice(bindingPrefix.length, -bindingSuffix.length);
              } catch { /* malformed unrelated key */ }
            } else if (key.startsWith(pendingPrefix)) copy = key.slice(pendingPrefix.length);
            if (/^[0-9a-f]{32}$/.test(copy) && copy !== context.copyInstance) priorCopies.add(copy);
          }
        }
      }
      for (const copy of priorCopies) {
        const draft = storage.getItem(peopleDraftKey(id, {...context, copyInstance:copy}));
        if (draft) legacyPeopleReview.drafts.push({owner:"unknown", values:{"scimux-people-draft:":draft}});
        const pendingKey = "scimux-people-file-pending:" + context.key + ":" + native + (context.binding ? ":" + context.binding : "") + ":copy:" + copy;
        const pending = storage.getItem(pendingKey);
        if (pending) {
          let text = "Saved pending message is unavailable.";
          try { const parsed = JSON.parse(pending); if (typeof parsed?.request?.text === "string") text = parsed.request.text || "File-only People message"; } catch { /* retained for review */ }
          legacyPeopleReview.drafts.push({owner:"unknown", values:{"scimux-people-draft:":text, "scimux-attachments:":"saved", "scimux-delivery-status:":"unconfirmed"}});
        }
      }
      renderRecovery(id);
    }).catch(() => {
      if (selId() === id && destinationVisit === visit && peopleStageIdentity(id) === identity && legacyPeopleReview.identity === identity) {
        legacyPeopleReview.unavailable = true;
        renderRecovery(id);
      }
    });
  }
  function currentComposerIdentity(){
    const id = selId();
    if (!id || id !== composerNode || !composerIdentity || stageIdentity(id) === composerIdentity) return true;
    try {
      const key = "scimux-retired-visible:" + composerIdentity;
      const raw = storage.getItem(key);
      let saved = [];
      if (raw !== null) {
        try { saved = JSON.parse(raw); } catch { saved = raw; }
        if (!Array.isArray(saved)) saved = [saved];
      }
      storage.setItem(key, JSON.stringify([...saved, promptText()]));
      if (!archiveRetiredDraft(storage, id, composerIdentity.split("\0")[1], {
        [isPeople() ? "scimux-people-draft:" : "scimux-draft:"]: promptText(),
      })) throw new Error("draft archive unavailable");
    }
    catch { alertFn("This chat was replaced. Copy the visible draft before continuing."); return false; }
    onSelect();
    alertFn("This chat was replaced. Review the current draft before sending.");
    return false;
  }
  function peopleDraftKey(id, context = peopleContext()){
    const binding = context?.attachments && context.binding ? peopleDraftBinding(id, context) : "";
    return "scimux-people-draft:" + id + (binding ? ":scope:" + encodeURIComponent(binding) : "");
  }
  function peopleDraftBinding(id, context){
    if (!context?.key) return "";
    const native = context.owner ? stageIdentity(id) : id;
    return native ? context.key + (context.binding ? "\0binding:" + context.binding : "") + (context.copyInstance ? "\0copy:" + context.copyInstance : "") + "\0" + native : "";
  }
  function isPeople(){ return destination === "people"; }
  function isPhoneTouch(){
    return typeof d.isPhoneTouch === "function" ? !!d.isPhoneTouch() : false;
  }

  function promptText(){
    const raw = prompt ? (prompt.innerText || "") : "";
    return normalizePromptText(raw);
  }

  function resizePrompt(){
    if (!prompt || !prompt.style) return;
    prompt.style.height = "auto";
    const sh = prompt.scrollHeight;
    prompt.style.height = promptHeightPx(sh) + "px";
  }

  function setPromptText(s){
    if (!prompt) return;
    prompt.textContent = s || "";
    resizePrompt();
  }

  function clearPrompt(){ setPromptText(""); }

  /* Stop targets the local agent: in Agent mode while busy, and for the
     owner in People mode while busy as long as no People text is typed. */
  function stopAvailable(){
    return !!composerBusy && (!isPeople() || (ownerChoice && !promptText().trim()));
  }

  function setComposerBusy(active){
    if (composerBusy === !!active) return; /* poll-safe: no DOM churn per tick */
    composerBusy = !!active;
    renderBusy();
  }

  function renderBusy(){
    const stop = stopAvailable();
    const chrome = busyChrome(stop);
    if (promptbar && promptbar.classList)
      promptbar.classList.toggle("busy", chrome.busy);
    if (sendbtn){
      if (sendbtn.classList) sendbtn.classList.toggle("stop", chrome.stopClass);
      sendbtn.innerHTML = stop ? (icons.ICON_STOP || "") : (icons.ICON_SEND || "");
      if (typeof sendbtn.setAttribute === "function")
        sendbtn.setAttribute("aria-label", chrome.ariaLabel);
      sendbtn.title = chrome.title;
      if (!stop && isPeople() && ownerChoice) {
        sendbtn.setAttribute("aria-label", "Send message to people");
        sendbtn.title = "Send message to people";
      }
      if (!isPeople() && ownerChoice && !composerBusy) {
        sendbtn.setAttribute("aria-label", "Send message to agent");
        sendbtn.title = "Send message to agent";
      }
    }
  }

  function setComposerClosed(closed){
    if (composerClosed === !!closed) return;
    composerClosed = !!closed;
    const context = peopleContext();
    const chrome = closedChrome(isPeople() ? !(context && context.writable) : (composerClosed || (typeof d.agentAllowed === "function" && !d.agentAllowed())));
    if (promptbar && promptbar.classList)
      promptbar.classList.toggle("closed", chrome.closed);
    if (prompt){
      if (typeof prompt.setAttribute === "function")
        prompt.setAttribute("contenteditable", chrome.contentEditable);
      if (prompt.dataset) prompt.dataset.placeholder = isPeople() ? "Message people…" : chrome.placeholder;
    }
    if (sendbtn) {
      sendbtn.disabled = chrome.sendDisabled;
      if (isPeople()) { sendbtn.setAttribute("aria-label", "Send message to people"); sendbtn.title = "Send message to people"; }
    }
    if (attadd) attadd.disabled = chrome.attDisabled;
  }

  function closeAttMenu(){
    if (attmenu) attmenu.hidden = true;
    if (attadd){
      if (attadd.classList) attadd.classList.remove("armed");
      if (typeof attadd.setAttribute === "function")
        attadd.setAttribute("aria-expanded", "false");
    }
  }

  function setAttachAvail(on){
    /* production: when already equal, still force hidden to match; when
       flipping off, close the source menu. */
    if (canAttach === on){
      if (attadd) attadd.hidden = !attachmentAvailable();
      return;
    }
    canAttach = on;
    if (attadd) attadd.hidden = !attachmentAvailable();
    if (!attachmentAvailable()) closeAttMenu();
  }

  function staged(key){
    return stage[key] || (stage[key] = []);
  }

  function clearStageRemoveListeners(){
    while (stageRemoveCleanups.length){
      try { stageRemoveCleanups.pop()(); } catch { /* ignore */ }
    }
  }

  async function discardCapturedDraft(id, state, item){
    const scope = peopleStageIdentity(id), visit = destinationVisit, lifetime = peopleLifetime;
    const current = () => localReviewAlive && peopleLifetime === lifetime && destinationVisit === visit && selId() === id && isPeople() && peopleStageIdentity(id) === scope && peopleDraftStates.get(scope) === state && state.review?.includes(item);
    if (!scope || !current()) return;
    const flight = JSON.stringify([scope, item.head, item.version]);
    if (capturedDraftFlights.has(flight)) return;
    capturedDraftFlights.add(flight);
    try {
      if (typeof d.peopleFileStore?.discardPeopleCapture !== "function") throw new Error("People recovery is unavailable.");
      const discarded = await d.peopleFileStore.discardPeopleCapture({scope, head:item.head, version:item.version, text:item.text, current});
      if (!current()) return;
      if (discarded) state.review = state.review.filter(row => row !== item);
      state.captureNotice = discarded ? "discarded" : "error";
      renderStage();
    } catch {
      if (current()) { state.captureNotice = "error"; renderStage(); }
    } finally { capturedDraftFlights.delete(flight); }
  }

  function renderStage(){
    if (!attstage) return;
    const id = selId();
    const draftState = isPeople() ? peopleDraftStates.get(peopleStageIdentity(id)) : null;
    const draftError = draftState?.error || (unavailablePeopleDraft?.id === id && unavailablePeopleDraft.visit === destinationVisit), savedDrafts = draftState?.review || [], completedDrafts = draftState?.completed || [];
    const items = id ? (stage[currentStageIdentity(id)] || []) : [];
    let pending = null;
    try { if (isPeople() && typeof d.pendingPeople === "function") pending = d.pendingPeople(); } catch { /* recovery remains unavailable */ }
    let textPending = null;
    try { if (isPeople() && typeof d.pendingPeopleText === "function") textPending = d.pendingPeopleText(); } catch { /* recovery remains unavailable */ }
    clearStageRemoveListeners();
    const textItems = collectTextReviews(textPending);
    const review = localReview?.address === localReviewAddress() ? localReview : null;
    if (!items.length && !pending && !textItems.length && !textReview?.notice && !draftError && !savedDrafts.length && !completedDrafts.length && !draftState?.captureNotice && !review?.rows.some(row => row.state !== "ready" || row.entries.length || row.retained.length || row.retainedUnavailable)){
      attstage.hidden = true;
      attstage.innerHTML = "";
      return;
    }
    attstage.hidden = false;
    attstage.innerHTML = stageRowHTML(items, {
      esc: escFn,
      iconFile: icons.ICON_FILE || "",
    });
    if (draftError) attstage.innerHTML += '<p role="status">This People draft could not be saved. Keep this view open and review storage availability.</p>';
    if (savedDrafts.length) attstage.innerHTML += `<details><summary>Review saved People drafts</summary>${savedDrafts.map(item => `<label>${item.sealed ? "Captured submission" : "Saved draft"}<textarea readonly>${escFn(item.text)}</textarea></label>${item.sealed ? `<button type="button" class="people-captured-discard btn-tinted" data-head="${escFn(item.head)}" data-version="${escFn(item.version)}">Discard captured submission</button>` : ""}`).join("")}</details>`;
    if (draftState?.captureNotice) attstage.innerHTML += `<p role="status">${draftState.captureNotice === "discarded" ? "Captured submission discarded locally; this does not retract a remote comment." : "Captured submission could not be discarded. Review it again."}</p>`;
    if (completedDrafts.length) attstage.innerHTML += `<details><summary>Review completed People messages</summary>${completedDrafts.map(item => `<p>Completed at sequence ${escFn(item.seq)}; the saved file identity could not be confirmed.</p><label>Saved text<textarea readonly>${escFn(item.request.text)}</textarea></label>`).join("")}</details>`;
    for (const [index, item] of textItems.entries()) {
      const snapshot = item.snapshot, progress = item.key && textRetirementFlights.has(item.key);
      const status = progress ? '<p role="status">Retiring local text retry…</p>' : item.error ? '<p role="status">Local text retirement is unavailable. Review and try again.</p>' : "";
      const action = item.key ? `<button type="button" class="people-text-retire btn-tinted" data-row="${index}" data-publication="${escFn(snapshot.publication)}">Discard local text retry</button>` : "";
      attstage.innerHTML += `<details class="people-text-review" data-row="${index}"${item.open ? " open" : ""}><summary>Review unconfirmed People text</summary><label>Saved text<textarea readonly>${escFn(snapshot.request.text)}</textarea></label><p>A remote comment may still appear; this does not retract it.</p><p>Publication key: ${escFn(snapshot.request.key)}</p>${status}${action}</details>`;
    }
    if (textReview?.notice) attstage.innerHTML += `<p role="status">${escFn(textReview.notice)}</p>`;
    let pendingID = "";
    try { pendingID = JSON.parse(pending?.value || "null")?.request?.key || ""; } catch { /* retain both reviews */ }
    let pendingRow = null;
    if (pending) {
      const names = pending.attachments.map(fileID => items.find(item => item.localID === fileID)?.name || "Saved file unavailable");
      const context = peopleContext();
      const index = review ? review.rows.findIndex(row => row.copy.key === pending.key && row.copy.copyInstance === context?.copyInstance && row.copy.binding === (context?.binding || "")) : -1;
      const row = index < 0 ? null : review.rows[index];
      pendingRow = row;
      if (row && row.state !== "error" && row.browserState?.value !== pending.value) {
        row.activeOpen = false;
        readBrowserPending(row, () => localReview === review && localReviewAddress() === review.address);
      }
      const entry = row?.entries.find(item => item.kind === "pending" && item.id === pendingID);
      let action = entry ? localDiscardHTML(row, index, entry) : "";
      const actualText = entry && (entry.text || "") !== (pending.text || "") ? `<label>Actual local pending text<textarea readonly>${escFn(entry.text || "File-only message")}</textarea></label>` : "";
      const actualNames = entry ? (Array.isArray(entry.stage_ids) && entry.stage_ids.length ? entry.stage_ids.map(id => row.entries.find(item => item.kind === "stage" && item.id === id)?.name || "Saved file metadata unavailable") : ["Saved file metadata unavailable"]) : [];
      const actualFiles = entry ? `<p>Actual local files: ${actualNames.map(escFn).join(", ")} (current stage inventory snapshot; IDs may be reused)</p>` : "";
      const browserState = row?.browserState && row.browserState.value === pending.value ? row.browserState : null;
      const stateHTML = pending.locallyRetired ? '<p role="status">The legacy retry is preserved for review; its local decision is recorded.</p>' : browserPendingStateHTML(browserState);
      const retirement = row && !pending.locallyRetired ? browserRetireHTML(row, index, browserState, "current") : "";
      if (retirement) action = retirement;
      const terminal = pending.locallyRetired || (browserState?.state === "ready" && (browserState.legacyRecorded || browserState.retired || ["retired", "absent_retired"].includes(browserState.result.state)));
      const attributes = row ? ` class="people-current-snapshot" data-row="${index}"${row.activeOpen ? " open" : ""}` : "";
      const retiring = row && localDiscardFlights.has(localDiscardKey(row.copy, pendingID));
      attstage.innerHTML += browserPendingReviewHTML(pending, names.map(escFn).join(", "), actualText + actualFiles + stateHTML, action, attributes, context?.writable === true && !terminal && !retiring ? '<button type="button" class="people-retry btn-tinted">Retry this message</button>' : "");
    }
    if (review) for (const [index, row] of review.rows.entries()) {
      if (row.retainedUnavailable) attstage.innerHTML += '<p role="status">Some saved People review records are unavailable.</p>';
      for (const [snapshotIndex, item] of row.retained.entries()) {
        const snapshot = item.snapshot;
        if (pending && snapshot.storageKey === pending.storageKey && snapshot.value === pending.value) continue;
        const origin = `<p>Original binding: ${escFn(snapshot.binding || "No saved binding")}</p>`;
        const attributes = ` class="people-local-snapshot" data-row="${index}" data-snapshot="${snapshotIndex}"${item.open ? " open" : ""}`;
        attstage.innerHTML += browserPendingReviewHTML(snapshot, snapshot.attachments.map(escFn).join(", "), origin + browserPendingStateHTML(item), browserRetireHTML(row, index, item, snapshotIndex), attributes);
      }
      if (row.state === "ready" && !row.entries.length) continue;
      if (review.rows.length > 1) attstage.innerHTML += `<p>Local copy ${escFn(row.copy.key)}</p>`;
      if (row.state === "loading") attstage.innerHTML += '<p role="status">Loading saved local People review…</p>';
      else if (row.state === "error") attstage.innerHTML += '<p role="status">Saved local People review is unavailable.</p>';
      else {
        const stages = row.entries.filter(entry => entry.kind === "stage");
        const publications = row.entries.filter(entry => entry.kind === "pending" && (row !== pendingRow || entry.id !== pendingID));
        for (const entry of publications) {
          const names = Array.isArray(entry.stage_ids) && entry.stage_ids.length ? entry.stage_ids.map(id => stages.find(stage => stage.id === id)?.name || "Saved file metadata unavailable") : ["Saved file metadata unavailable"];
          const state = row.discards.get(entry.id);
          const content = state?.state === "locally_discarded" || state?.state === "already_completed" ? "" : `<label>Saved text<textarea readonly>${escFn(entry.text || "File-only message")}</textarea></label><p>Current stage inventory snapshot (IDs may be reused): ${names.map(escFn).join(", ")}</p>`;
          attstage.innerHTML += `<details><summary>Review unconfirmed local People message</summary>${content}${localDiscardHTML(row, index, entry)}</details>`;
        }
        if (stages.length) attstage.innerHTML += `<details><summary>Review current staged files (inventory snapshot)</summary>${stages.map(stage => localStageHTML(row, index, stage)).join("")}</details>`;
      }
    }
    const textView = {review:textReview, address:textReview?.address, lifetime:peopleLifetime};
    for (const detail of (typeof attstage.querySelectorAll === "function" ? attstage.querySelectorAll(".people-text-review") : [])) {
      const item = textItems[Number(detail.dataset?.row)];
      if (!item) continue;
      const onToggle = () => { if (textReview === textView.review && textReviewSnapshot()?.address === textView.address) item.open = !!detail.open; };
      detail.addEventListener("toggle", onToggle);
      stageRemoveCleanups.push(() => detail.removeEventListener("toggle", onToggle));
    }
    for (const button of (typeof attstage.querySelectorAll === "function" ? attstage.querySelectorAll(".people-text-retire") : [])) {
      const item = textItems[Number(button.dataset?.row)];
      if (!item || item.snapshot.publication !== button.dataset?.publication) continue;
      let active = true;
      const onClick = () => { if (active) retireTextReview(textView, item); };
      button.addEventListener("click", onClick);
      stageRemoveCleanups.push(() => { active = false; button.removeEventListener("click", onClick); });
    }
    if (review) {
      const details = typeof attstage.querySelectorAll === "function" ? attstage.querySelectorAll(".people-local-snapshot") : [];
      for (const detail of details) {
        const row = review.rows[Number(detail.dataset?.row)], item = row?.retained[Number(detail.dataset?.snapshot)];
        if (!item) continue;
        const onToggle = () => {
          if (!localReviewAlive || localReview !== review || localReviewAddress() !== review.address || !row.retained.includes(item)) return;
          item.open = !!detail.open;
          if (item.open && item.state === "unread") { readRetainedPending(review, row, item); renderStage(); }
        };
        detail.addEventListener("toggle", onToggle);
        stageRemoveCleanups.push(() => detail.removeEventListener("toggle", onToggle));
      }
    }
    if (review && pending) {
      const details = typeof attstage.querySelectorAll === "function" ? attstage.querySelectorAll(".people-current-snapshot") : [];
      for (const detail of details) {
        const row = review.rows[Number(detail.dataset?.row)];
        if (!row) continue;
        const onToggle = () => {
          if (!localReviewAlive || localReview !== review || localReviewAddress() !== review.address) return;
          try {
            const latest = d.pendingPeople();
            if (latest?.storageKey === pending.storageKey && latest.value === pending.value) row.activeOpen = !!detail.open;
          } catch { /* keep the last reviewed disclosure state */ }
        };
        detail.addEventListener("toggle", onToggle);
        stageRemoveCleanups.push(() => detail.removeEventListener("toggle", onToggle));
      }
    }
    if (draftState) for (const button of (typeof attstage.querySelectorAll === "function" ? attstage.querySelectorAll(".people-captured-discard") : [])) {
      const item = savedDrafts.find(row => row.sealed && row.head === button.dataset?.head && row.version === button.dataset?.version);
      if (!item) continue;
      const onClick = () => discardCapturedDraft(id, draftState, item);
      button.addEventListener("click", onClick);
      stageRemoveCleanups.push(() => button.removeEventListener("click", onClick));
    }
    if (pending) {
      const retryButtons = typeof attstage.querySelectorAll === "function" ? attstage.querySelectorAll(".people-retry") : [];
      for (const b of retryButtons) {
        const onClick = () => retryPeopleDraft(pending);
        b.addEventListener("click", onClick);
        stageRemoveCleanups.push(() => b.removeEventListener("click", onClick));
      }
    }
    if (review) {
      const retireButtons = typeof attstage.querySelectorAll === "function" ? attstage.querySelectorAll(".people-browser-retire") : [];
      for (const button of retireButtons) {
        const row = review.rows[Number(button.dataset?.row)];
        const active = button.dataset?.snapshot === "current";
        const item = active ? row?.browserState : row?.retained[Number(button.dataset?.snapshot)];
        if (!item) continue;
        const onClick = () => retireBrowserSnapshot(review, row, item, active);
        button.addEventListener("click", onClick);
        stageRemoveCleanups.push(() => button.removeEventListener("click", onClick));
      }
    }
    if (review) {
      const stageButtons = typeof attstage.querySelectorAll === "function" ? attstage.querySelectorAll(".people-local-stage-remove") : [];
      for (const button of stageButtons) {
        const row = review.rows[Number(button.dataset?.row)];
        const entry = row?.entries.find(item => item.kind === "stage" && item.id === button.dataset?.id);
        if (!entry) continue;
        const onClick = () => removeLocalStage(review, row, entry);
        button.addEventListener("click", onClick);
        stageRemoveCleanups.push(() => button.removeEventListener("click", onClick));
      }
    }
    if (review) {
      const discardButtons = typeof attstage.querySelectorAll === "function" ? attstage.querySelectorAll(".people-local-discard") : [];
      for (const b of discardButtons) {
        const row = review.rows[Number(b.dataset?.row)];
        const entry = row?.entries.find(item => item.kind === "pending" && item.id === b.dataset?.id);
        if (!entry) continue;
        const onClick = () => discardLocalReview(review, row, entry);
        b.addEventListener("click", onClick);
        stageRemoveCleanups.push(() => b.removeEventListener("click", onClick));
      }
    }
    /* rebind remove buttons */
    const buttons = typeof attstage.querySelectorAll === "function"
      ? attstage.querySelectorAll(".sx")
      : [];
    for (const b of buttons){
      const onClick = () => removeStaged(b.dataset && b.dataset.key);
      if (typeof b.addEventListener === "function"){
        b.addEventListener("click", onClick);
        stageRemoveCleanups.push(() => {
          if (typeof b.removeEventListener === "function")
            b.removeEventListener("click", onClick);
        });
      }
    }
  }

  async function removeStaged(key){
    const id = selId();
    if (!id) return;
    const identity = currentStageIdentity(id);
    const arr = stage[identity] || [];
    const i = arr.findIndex(x => String(x.key) === String(key));
    if (i < 0) return;
    const item = arr[i];
    if (item.removing) return;
    let frozen = false;
    try { frozen = !!item.peopleKey && !!d.pendingPeople?.()?.attachments?.includes(item.localID); } catch { frozen = true; }
    if (item.pending || frozen) { alertFn("This file is part of a pending People message. Review its delivery before removing it."); return; }
    if (item.peopleKey) {
      item.removing = true;
      try {
        if (item.uploadPromise) await item.uploadPromise;
        if (item.ref?.id) {
          await removePeopleStage(d.api, item);
        }
        if (item.localSaved) await d.peopleFileStore.removeStageFile(identity, item.localID, item.fileToken);
      } catch (err) {
        item.removing = false;
        if (selId() === id && currentStageIdentity(id) === identity) { alertFn(err?.message || "Stage removal failed"); renderStage(); }
        return;
      }
    }
    if (item.sourceLocalID && item.localSaved) {
      try { await d.peopleFileStore.remove(sourceScope(identity), item.sourceLocalID); }
      catch { alertFn("Saved file removal failed."); return; }
    }
    const index = arr.indexOf(item);
    if (index < 0) return;
    if (item.preview) URLImpl.revokeObjectURL(item.preview);
    arr.splice(index, 1);
    if (item.sourceKey && clearedSources(identity).has(item.sourceKey) && !arr.some(other => other.sourceKey === item.sourceKey)) {
      writePromptSources(storage, id, readPromptSources(storage, id).filter(source => promptSourceKey(source) !== item.sourceKey));
      if (typeof d.promptSourcesChanged === "function") d.promptSourcesChanged(id);
    }
    if (selId() === id && currentStageIdentity(id) === identity) renderStage();
  }

  async function uploadOne(it, dest, identity, instance, peopleKey = ""){
    if (!FormDataImpl){
      it.status = "err";
      it.error = "FormData unavailable";
      if (selId() === dest) renderStage();
      return;
    }
    const fd = new FormDataImpl();
    fd.append("files", it.file, it.name);
    try {
      const withCsrf = d.withCsrf || (opts => opts);
      const fetchImpl = d.fetchImpl || (typeof fetch !== "undefined" ? fetch : null);
      if (!fetchImpl) throw new Error("fetch unavailable");
      const target = peopleKey
        ? `/api/team/subscriptions/${peopleKey.split("/").map(encodeURIComponent).join("/")}/attachments?stage_id=${encodeURIComponent(it.localID)}&copy_instance=${encodeURIComponent(it.copyInstance || "")}${instance ? `&chat_instance=${encodeURIComponent(instance)}` : ""}`
        : `/api/nodes/${encodeURIComponent(dest)}/attachments${instance ? `?chat_instance=${encodeURIComponent(instance)}` : ""}`;
      const r = await fetchImpl(
        target,
        withCsrf({ method: "POST", body: fd, ...(instance ? {headers:{"X-Scimux-Chat-Instance":instance}} : {}) }),
      );
      if (!r.ok) {
        const message = (await r.text()) || r.statusText;
        if (peopleKey) {
          try { const result = JSON.parse(message); if (result?.stage_id === it.localID) it.ref = {id:result.stage_id}; } catch { /* preserve visible error */ }
        }
        throw new Error(message);
      }
      const body = await r.json();
      it.ref = body.attachments[0];
      if (peopleKey && it.ref?.id !== it.localID) { it.ref = null; throw new Error("People file identity did not match."); }
      if (peopleKey && it.newPeopleStage) {
        if (it.stageRevision && it.ref.revision !== it.stageRevision) throw new Error("Saved file changed; review it before removal.");
        if (!/^[0-9a-f]{64}$/.test(it.ref.revision || "")) throw new Error("Review the current local file before removing this saved file.");
        it.stageRevision = it.ref.revision;
        await d.peopleFileStore.setStageRevision(identity, it.localID, it.stageRevision, it.fileToken);
        it.newPeopleStage = false;
      } else if (peopleKey && it.stageRevision && it.ref.revision !== it.stageRevision) {
        throw new Error("Saved file changed; review it before removal.");
      }
      it.status = "done";
      if (peopleKey || it.sourceKey) it.needsUpload = false;
    } catch (e) {
      it.status = "err";
      it.error = peopleKey ? peopleFileFailureText(e.message) : e.message;
      if (peopleKey || it.sourceKey) it.needsUpload = true;
    }
    if (selId() === dest && currentStageIdentity(dest) === identity) renderStage();
  }

  function addFiles(files, dest, sourceKey = "", sourceRecords = []){
    if (!currentComposerIdentity()) return;
    dest = dest || selId();
    if (!identityReady) return;
    if (!files || !files.length) return;
    const people = isPeople();
    const context = people ? peopleContext() : null;
    if (people && !peopleCanAttach(context)) { alertFn(context?.copyIdentityMissing ? "The local copy identity is unavailable. Reconnect and review your saved files." : "People messages currently support text only. Choose Agent to attach files."); return; }
    if ((!people && (!canAttach || dest.startsWith("team/"))) || !dest) return;
    const identity = people ? peopleStageIdentity(dest, context) : stageIdentity(dest);
    if (!identity) { alertFn("This chat's identity is unavailable. Reconnect before attaching files."); return; }
    const instance = people && !context.owner ? "" : (nodeById(dest)?.instance_id || "");
    const arr = staged(identity);
    const list = Array.from(files);
    for (const [index, f] of list.entries()){
      if (people && (!Number.isSafeInteger(f.size) || f.size < 1 || f.size > 4 * 1024 * 1024 || arr.length >= 16 || arr.reduce((sum, item) => sum + item.size, 0) + f.size > 16 * 1024 * 1024)) {
        alertFn("People files must be non-empty, at most 4 MiB each, with at most 16 files and 16 MiB total.");
        continue;
      }
      const isImage = isImageFileType(f.type);
      let localID = "";
      if (people) {
        try { localID = peopleStageID(); } catch (err) { alertFn(err.message); continue; }
      }
      const it = {
        key: ++stageKey,
        file: f,
        size: f.size,
        name: stagedFileName(f, isImage),
        isImage,
        preview: isImage ? URLImpl.createObjectURL(f) : "",
        ref: null,
        status: "up",
        peopleKey: people ? context.key : "",
        copyInstance: people ? context.copyInstance || "" : "",
        instance: people ? instance : "",
        localID,
        newPeopleStage:people,
        sourceKey: people ? "" : sourceKey,
        ...(sourceRecords[index] || {}),
      };
      arr.push(it);
      if (people) {
        it.uploadPromise = (async () => {
          try {
            if (!d.peopleFileStore) throw new Error("People file storage is unavailable.");
            it.fileToken = await d.peopleFileStore.save(identity, localID, f);
            it.localSaved = true;
            await uploadOne(it, dest, identity, instance, it.peopleKey);
          } catch (err) {
            it.status = "err"; it.error = err?.message || "People file storage is unavailable.";
            if (selId() === dest && currentStageIdentity(dest) === identity) renderStage();
          }
        })();
      } else it.uploadPromise = uploadOne(it, dest, identity, instance);
    }
    if (selId() === dest) renderStage();
  }

  /* The three places this module touches storage, and the only ones. sendPrompt
     holds the user's only copy of a prompt, so it does not lean on the injected
     adapter's goodwill (storage.js): a browser that refuses costs the saved
     draft and nothing else -- not the send, not the text on screen. */
  function savedDraft(id){
    try { return storage.getItem(draftStorageKey(id)) || ""; } catch { return ""; }
  }
  function keepDraft(id, text){
    try { storage.setItem(draftStorageKey(id), text); } catch { /* not persisted */ }
  }
  function forgetDraft(id){
    try { storage.removeItem(draftStorageKey(id)); } catch { /* stays until it can be */ }
  }

  function renderRecovery(id){
    if (!roots.recovery) return;
    const entries = id ? [...readRetiredDrafts(storage, id), ...(retiredMemory.get(id) || []), ...legacyPeopleReview.drafts] : [];
    if (id && peopleContext()) {
      const currentLegacy = storage.getItem(peopleDraftKey(id));
      if (currentLegacy) entries.push({owner:"unknown", values:{"scimux-people-draft:":currentLegacy}});
    }
    if (id && peopleDraftKey(id) !== "scimux-people-draft:" + id) {
      const legacy = storage.getItem("scimux-people-draft:" + id);
      if (legacy) entries.push({owner:"unknown", values:{"scimux-people-draft:":legacy}});
    }
    const context = peopleContext();
    if (id && context?.key) {
      const legacyTextRetry = storage.getItem("scimux-comment-retry:" + context.key);
      if (legacyTextRetry) {
        let text = "Saved People retry is unavailable.";
        try { const parsed = JSON.parse(legacyTextRetry); if (typeof parsed?.text === "string") text = parsed.text || "Empty People message"; } catch { /* preserve damaged legacy content for review */ }
        entries.push({owner:"unknown", values:{"scimux-people-draft:":text, "scimux-delivery-status:":"unconfirmed"}});
      }
    }
    if (id && context?.copyInstance) {
      const priorKey = peopleDraftKey(id, {...context, copyInstance:""});
      if (priorKey !== "scimux-people-draft:" + id) {
        const prior = storage.getItem(priorKey);
        if (prior) entries.push({owner:"unknown", values:{"scimux-people-draft:":prior}});
      }
      const native = context.owner ? (nodeById(id)?.instance_id || "") : "";
      const pendingKey = "scimux-people-file-pending:" + context.key + ":" + native + (context.binding ? ":" + context.binding : "");
      const pending = storage.getItem(pendingKey);
      if (pending) {
        let text = "Saved pending message is unavailable.";
        try { const parsed = JSON.parse(pending); if (typeof parsed?.request?.text === "string") text = parsed.request.text || "File-only People message"; } catch { /* retained for review */ }
        entries.push({owner:"unknown", values:{"scimux-people-draft:":text, "scimux-attachments:":"saved", "scimux-delivery-status:":"unconfirmed"}});
      }
    }
    const pendingSources = id && unresolvedSourceFiles(id, stageIdentity(id));
    roots.recovery.hidden = !entries.length && !pendingSources && !legacyPeopleReview.files.length && !legacyPeopleReview.unavailable;
    roots.recovery.innerHTML = (entries.length ? `<details><summary>Review saved drafts</summary>${retiredDraftReviewHTML(entries)}</details>` : "") +
      (pendingSources ? '<p role="status">Review saved source files before sending; recovery is pending or unavailable.</p>' : "") +
      (legacyPeopleReview.files.length ? '<p>Files saved from an earlier copy are available for review.</p>' + legacyPeopleReview.files.map(item => `<a href="${escDefault(item.url)}" download="${escDefault(item.name)}">Download ${escDefault(item.name)}</a>`).join("") : "") +
      (legacyPeopleReview.unavailable ? '<p role="status">Saved People files could not be reviewed.</p>' : "");
  }

  function retireChangedPeopleFlights(id, context){
    for (const token of peopleSending.values()) {
      if (token.id === id && ((token.native && stageIdentity(id) !== token.native) ||
          (token.fileIdentity && peopleStageIdentity(id, context) !== token.fileIdentity))) releasePeopleFlight(token);
    }
  }

  function restorePeopleDraft(id, context){
    const remembered = lastPeopleDraftScopes.get(id), native = stageIdentity(id);
    const scope = context ? peopleStageIdentity(id, context) : (remembered && remembered.native === native ? remembered.scope : ""), visit = destinationVisit, lifetime = peopleLifetime, revision = peopleRevisions.get(id) || 0;
    if (context && scope) lastPeopleDraftScopes.set(id, {scope, native});
    const existing = peopleDraftStates.get(scope);
    setPromptText(existing?.text || "");
    const load = typeof d.refreshPeopleRecovery === "function" ? d.refreshPeopleRecovery : d.peopleFileStore?.listPeopleRecovery?.bind(d.peopleFileStore);
    if (!scope || !load) return;
    Promise.resolve().then(() => load(scope)).then(result => {
      if (peopleLifetime !== lifetime || destinationVisit !== visit || selId() !== id || !isPeople() || peopleStageIdentity(id) !== scope) return;
      if (localReview?.address === localReviewAddress()) for (const row of localReview.rows) if (peopleStageIdentity(id, row.copy) === scope) loadRetainedPending(row);
      const canRestore = !peopleDraftStates.has(scope) && (peopleRevisions.get(id) || 0) === revision && !promptText();
      const editable = result.heads.filter(head => !head.sealed);
      if (canRestore && editable.length === 1) {
        const head = editable[0];
        peopleDraftStates.set(scope, {head:head.head, version:head.version, tail:Promise.resolve(), text:head.text, error:false, ...(result.heads.length > 1 ? {review:result.heads} : {})});
        setPromptText(head.text);
      } else if (canRestore && result.heads.length) {
        peopleDraftStates.set(scope, {head:undefined, version:undefined, tail:Promise.resolve(), text:"", error:false, review:result.heads});
      }
      if (result.completed?.length) {
        let state = peopleDraftStates.get(scope);
        if (!state) { state = {head:undefined, version:undefined, tail:Promise.resolve(), text:"", error:false}; peopleDraftStates.set(scope, state); }
        state.completed = result.completed;
      }
      renderStage();
    }).catch(() => {
      if (peopleLifetime === lifetime && destinationVisit === visit && selId() === id && isPeople() && peopleStageIdentity(id) === scope && !peopleDraftStates.has(scope)) {
        peopleDraftStates.set(scope, {head:undefined, version:undefined, tail:Promise.resolve(), text:"", error:true});
        renderStage();
      }
    });
  }

  function onSelect({preferAgent = false} = {}){
    stopLocalReview();
    clearLegacyPeopleReview();
    for (const controller of preparationControllers) controller.abort();
    destinationVisit++;
    /* restore this node's draft and stage; hide + until chat confirms turns */
    const id = selId();
    if (composerNode && composerNode !== id && !promptText().trim() &&
        !sendingDestinations.has(composerIdentity) && !readAwaitingForward(storage, composerNode, nowFn()))
      clearPendingForwards(storage, composerNode);
    composerNode = id;
    composerIdentity = stageIdentity(id);
    identityReady = bindDraftIdentity(storage, id, nodeById(id)?.instance_id || "");
    renderRecovery(id);
    const context = peopleContext();
    retireChangedPeopleFlights(id, context);
    contextKey = context?.key || "";
    contextDraftBinding = peopleDraftBinding(id, context);
    ownerChoice = !!context?.owner && !id.startsWith("team/");
    explicitAgent = !!preferAgent && !id.startsWith("team/");
    destination = !explicitAgent && (id.startsWith("team/") || contextKey) ? "people" : "agent";
    if (identityReady && isPeople()) restorePeopleDraft(id, context);
    else setPromptText(identityReady ? savedDraft(id) : "");
    if (!identityReady) alertFn("Draft storage is unavailable. Reconnect before using this chat's composer.");
    refreshDestination();
    setAttachAvail(false);
    renderStage();
  }

  function refreshDestination(){
    refreshLocalReview();
    if (roots.destination) roots.destination.hidden = !ownerChoice;
    if (roots.destinationPeople) roots.destinationPeople.setAttribute("aria-pressed", String(isPeople()));
    if (roots.destinationAgent) roots.destinationAgent.setAttribute("aria-pressed", String(!isPeople()));
    const busy = composerBusy; const closed = composerClosed;
    composerBusy = null; composerClosed = null;
    setComposerBusy(busy); setComposerClosed(closed);
    if (attadd) attadd.hidden = !attachmentAvailable();
    if (!attachmentAvailable()) closeAttMenu();
    renderStage();
    restoreSourceFiles();
    restoreLegacyPeopleFiles();
    if (isPeople()) restorePeopleFiles();
  }

  function contextChanged(){
    const context = peopleContext();
    retireChangedPeopleFlights(selId(), context);
    const key = context?.key || "";
    const nextBinding = peopleDraftBinding(selId(), context);
    if (isPeople() && key === contextKey && nextBinding !== contextDraftBinding && (context?.binding || contextDraftBinding.includes("\0binding:"))) {
      destinationVisit++;
      restorePeopleDraft(selId(), context);
    }
    contextDraftBinding = nextBinding;
    if (context?.owner && !selId().startsWith("team/")) ownerChoice = true;
    if (key && key !== contextKey && !selId().startsWith("team/") && !explicitAgent) {
      if (!isPeople()) keepDraft(selId(), promptText());
      contextKey = key;
      setDestination("people");
    } else { contextKey = key; refreshDestination(); }
    renderRecovery(selId());
  }

  function setDestination(next){
    if (!currentComposerIdentity()) return;
    if (!identityReady) return;
    const context = peopleContext();
    if (next !== "agent" && next !== "people") return;
    if (next === "agent" && context && !context.owner) return;
    if (next === "agent" && (selId().startsWith("team/") || !nodeById(selId()))) return;
    if (next === "people" && !context && !ownerChoice) return;
    if (destination === next) { refreshDestination(); return; }
    for (const controller of preparationControllers) controller.abort();
    destinationVisit++;
    destination = next;
    explicitAgent = next === "agent";
    if (isPeople()) restorePeopleDraft(selId(), context);
    else setPromptText(savedDraft(selId()));
    refreshDestination();
  }

  function preparePrompt(text, source, files = [], sourceRecords = []){
    if (!currentComposerIdentity()) return false;
    if (!identityReady) return false;
    const id = selId();
    if (!id || id.startsWith("team/") || !nodeById(id) || nodeById(id).ended_at || !promptSourceKey(source) ||
        typeof d.canPreparePrompt !== "function" || !d.canPreparePrompt(source) || (!String(text || "").trim() && !files.length) || (files.length && !canAttach)) return false;
    const identity = stageIdentity(id);
    clearedSources(identity);
    if (damagedSourceMetadata.has(identity)) { alertFn("Saved source information is damaged; review it before preparing another prompt."); return false; }
    const prior = isPeople() ? savedDraft(id) : promptText();
    const merged = text ? (prior ? prior + "\n\n" : "") + text : prior;
    keepDraft(id, merged);
    addPromptSource(storage, id, source);
    const sourceKey = promptSourceKey(source);
    const cleared = clearedSources(identity);
    if (String(text || "").trim()) cleared.delete(sourceKey); else cleared.add(sourceKey);
    saveClearedSources(identity, cleared);
    setDestination("agent");
    setPromptText(merged);
    if (files.length) addFiles(files, id, sourceKey, sourceRecords);
    if (prompt) focusAtEnd(prompt);
    if (typeof d.promptSourcesChanged === "function") d.promptSourcesChanged(id);
    return true;
  }

  function prepareCapturePrompt(text, source, attachments, actionCurrent) {
    return attachments?.length ? prepareSharedPrompt(text, source, attachments, actionCurrent) : preparePrompt(text, source);
  }

  async function prepareSharedPrompt(text, source, attachments, actionCurrent = () => true) {
    const id = selId(), context = peopleContext(), identity = stageIdentity(id), visit = destinationVisit;
    const key = source && [source.server,source.owner,source.chat,source.generation].join("/");
    if (!identity || (context && (!context.owner || context.key !== key)) || !Array.isArray(attachments) || !attachments.length || !canAttach || typeof d.preparePeopleFiles !== "function" || typeof d.canPreparePrompt !== "function" || !d.canPreparePrompt(source)) return false;
    const authority = typeof d.capturePromptAuthority === "function" ? d.capturePromptAuthority(source, attachments) : null;
    if (typeof d.capturePromptAuthority === "function" && (!authority || typeof authority.current !== "function" || !authority.current())) return false;
    const agentRevision = agentRevisions.get(id) || 0, peopleRevision = peopleRevisions.get(id) || 0, agentDraft = savedDraft(id);
    const current = () => {
      const next = peopleContext();
      return actionCurrent() && selId() === id && destinationVisit === visit && stageIdentity(id) === identity && (context ? !!next?.owner && next.key === context.key && next.binding === context.binding : !next) && (!authority || authority.current()) && d.canPreparePrompt(source) && (agentRevisions.get(id) || 0) === agentRevision && (peopleRevisions.get(id) || 0) === peopleRevision && savedDraft(id) === agentDraft;
    };
    const controller = new AbortController(); preparationControllers.add(controller);
    const saved = [];
    try {
      const files = await d.preparePeopleFiles({source, attachments, copyInstance:authority?.copyInstance, current, signal:controller.signal});
      if (controller.signal.aborted || !current() || !Array.isArray(files) || files.length !== attachments.length) return false;
      if (!d.peopleFileStore) throw new Error("Source file recovery is unavailable.");
      for (let i = 0; i < files.length; i++) {
        const sourceLocalID = peopleStageID();
        await d.peopleFileStore.save(sourceScope(identity), sourceLocalID, files[i], {source, sourceFileID:attachments[i].id});
        saved.push({sourceLocalID, sourceFileID:attachments[i].id, source, localSaved:true});
        if (controller.signal.aborted || !current()) throw new Error("Source changed before preparation completed.");
      }
      preparationControllers.delete(controller);
      const prepared = preparePrompt(text, source, files, saved);
      if (!prepared) throw new Error("Source file preparation is unavailable.");
      return true;
    } catch (err) {
      for (const item of saved) { try { await d.peopleFileStore.remove(sourceScope(identity), item.sourceLocalID); } catch { /* retained for later review */ } }
      if (!controller.signal.aborted && current()) alertFn(err?.message || "Source files are unavailable.");
      return false;
    } finally { preparationControllers.delete(controller); }
  }

  function agentDraftText(id){
    return selId() === id && !isPeople() ? promptText() : savedDraft(id);
  }

  function recoverPromptDraft(id, text, attachments = []){
    const merged = mergeFailedDraft(text, savedDraft(id));
    if (merged) keepDraft(id, merged);
    const recovered = (Array.isArray(attachments) ? attachments : []).filter(item => item && item.ref && typeof item.name === "string")
      .map(item => ({key:++stageKey, name:item.name, isImage:!!item.isImage, preview:"", ref:item.ref, status:"done"}));
    const identity = stageIdentity(id);
    if (recovered.length && identity) stage[identity] = restoreStageOnFailure(stage[identity] || [], recovered);
    if (selId() === id && !isPeople()) { setPromptText(merged); renderStage(); }
  }

  function beginPeopleFlight(identity, id, context, files) {
    const token = {identity, id, native:context.owner ? stageIdentity(id) : "", fileIdentity:files.length ? peopleStageIdentity(id, context) : "", reviewAddress:localReview?.address, lifetime:peopleLifetime, files, barrier:null};
    peopleSending.set(identity, token);
    for (const item of files) item.pending = true;
    return token;
  }
  function ownsPeopleFlight(token) {
    return !token.retiring && token.lifetime === peopleLifetime && peopleSending.get(token.identity) === token &&
      (!token.native || stageIdentity(token.id) === token.native) &&
      (!token.fileIdentity || selId() !== token.id || !isPeople() || peopleStageIdentity(token.id) === token.fileIdentity);
  }
  function releasePeopleFlight(token) {
    if (peopleSending.get(token.identity) !== token) return;
    peopleSending.delete(token.identity);
    for (const item of token.files) item.pending = false;
  }
  async function peopleFileOperation(token, operation) {
    const barrier = Promise.resolve().then(() => ownsPeopleFlight(token) ? operation() : undefined);
    token.barrier = barrier;
    try { await barrier; } finally { if (token.barrier === barrier) token.barrier = null; }
    return ownsPeopleFlight(token);
  }
  function prunePeopleStageInventory(token, item) {
    const review = localReview;
    if (!localReviewAlive || !review || !token.reviewAddress || review.address !== token.reviewAddress || localReviewAddress() !== review.address || selId() !== token.id) return;
    for (const row of review.rows) {
      row.entries = row.entries.filter(entry => entry.kind !== "stage" || entry.id !== item.ref?.id || entry.revision !== item.stageRevision);
    }
  }
  async function finishPeopleFiles(token, fileIdentity, files, receipt) {
    if (receipt?.peoplePending?.format !== "transactional" || typeof d.finishPeople !== "function") throw new Error("The sent message could not be recorded locally. Review the saved files.");
    let decision;
    if (!await peopleFileOperation(token, async () => { decision = await d.finishPeople(receipt, {current:() => ownsPeopleFlight(token)}); })) return false;
    if (!decision || decision.state !== "completed" || decision.seq !== receipt.seq) throw new Error("The sent message could not be recorded locally. Review and retry it.");
    if (!decision.filesConfirmed) {
      for (const item of files) { item.status = "err"; item.error = `Sent at sequence ${receipt.seq}; review whether this saved file belongs to the confirmed message.`; }
      return false;
    }
    const arr = stage[fileIdentity];
    for (const item of files) {
      if (!ownsPeopleFlight(token)) return false;
      item.sent = true;
      try {
        if (!await peopleFileOperation(token, () => removePeopleStage(d.api, item))) return false;
        prunePeopleStageInventory(token, item);
        if (!await peopleFileOperation(token, () => d.peopleFileStore.removeStageFile(fileIdentity, item.localID, item.fileToken, () => ownsPeopleFlight(token), receipt.peoplePending.publication))) return false;
      } catch {
        if (!ownsPeopleFlight(token)) return false;
        item.status = "err";
        item.error = item.stageRevision ? "Sent; remove this saved file to finish cleanup." : "Sent; review the current local file before removing this saved file.";
        continue;
      }
      if (!ownsPeopleFlight(token)) return false;
      const index = arr.indexOf(item);
      if (item.preview) URLImpl.revokeObjectURL(item.preview);
      arr.splice(index, 1);
    }
    return decision;
  }
  function quietPeopleError(err) {
    return err?.code === "PEOPLE_LOCAL_RETIRED" || err?.code === "PEOPLE_FEATURE_CLOSED";
  }

  async function retryPeopleDraft(pending) {
    const id = selId(), context = peopleContext(), identity = peopleStageIdentity(id, context), visit = destinationVisit;
    const review = localReview?.address === localReviewAddress() ? localReview : null;
    const row = review?.rows.find(item => item.copy.key === context?.key && item.copy.copyInstance === context?.copyInstance && item.copy.binding === (context?.binding || ""));
    const observed = row?.browserState;
    try { const latest = d.pendingPeople?.(); if (!latest || latest.storageKey !== pending.storageKey || latest.value !== pending.value || latest.locallyRetired) return; } catch { return; }
    if (observed?.state === "ready" && observed.value === pending.value && (observed.retired || ["retired", "absent_retired"].includes(observed.result.state))) return;
    if (!isPeople() || !identity || context?.writable !== true || context.key !== pending.key || peopleSending.has(identity) || typeof d.retryPeople !== "function") return;
    let publicationKey;
    try { publicationKey = JSON.parse(pending.value)?.request?.key; } catch { /* pending validation remains with the existing retry path */ }
    if (localDiscardFlights.has(localDiscardKey({key:pending.key, copyInstance:context.copyInstance}, publicationKey))) return;
    const arr = stage[identity] || [];
    const files = pending.attachments.map(fileID => arr.find(item => item.localID === fileID));
    if (files.some(item => !item || !item.localSaved || item.removing)) { alertFn("Saved files for this message are unavailable."); return; }
    const token = beginPeopleFlight(identity, id, context, files);
    token.pending = {storageKey:pending.storageKey, value:pending.value};
    const inView = () => selId() === id && destinationVisit === visit && isPeople() && peopleStageIdentity(id) === identity;
    const draftState = peopleDraftStates.get(identity), revision = peopleRevisions.get(id) || 0;
    try {
      const receipt = await d.retryPeople();
      if (!ownsPeopleFlight(token)) return;
      if (pending.format !== "transactional") {
        let decision;
        if (!await peopleFileOperation(token, async () => { decision = await d.finishPeople(receipt, {scope:identity, current:() => ownsPeopleFlight(token)}); })) return;
        if (!decision || decision.state !== "completed" || decision.seq !== receipt.seq) throw new Error("The sent message could not be recorded locally. Review the saved files.");
        for (const item of files) { item.status = "err"; item.error = `Sent at sequence ${receipt.seq}; review whether this saved file belongs to the confirmed message.`; }
        if (inView()) renderStage();
        return;
      }
      const decision = await finishPeopleFiles(token, identity, files, receipt);
      if (!decision) return;
      if (decision.draftCleared && draftState?.head === pending.head && draftState.version === pending.version && (peopleRevisions.get(id) || 0) === revision) {
        draftState.head = undefined; draftState.version = undefined; draftState.text = "";
        if (inView() && promptText() === pending.text) clearPrompt();
      }
      if (inView()) renderStage();
    } catch (err) { if (ownsPeopleFlight(token) && inView() && !quietPeopleError(err)) alertFn(err?.status ? peopleFileFailureText(err.message) : (err?.message || "Message delivery is unconfirmed.")); }
    finally {
      const owned = ownsPeopleFlight(token);
      releasePeopleFlight(token);
      if (owned && inView()) renderStage();
    }
  }

  async function sendPeople(){
    const context = peopleContext(); const id = selId(); const text = promptText();
    const withFiles = peopleCanAttach(context);
    const fileIdentity = withFiles ? peopleStageIdentity(id, context) : "";
    const identity = withFiles ? fileIdentity : (id.startsWith("team/") ? id : (stageIdentity(id) || (context?.key ? "people/" + context.key : "")));
    const sameIdentity = () => withFiles ? (selId() === id && peopleStageIdentity(id) === identity) : (id.startsWith("team/") || (identity.startsWith("people/")
      ? selId() === id && peopleContext()?.key === context.key : stageIdentity(id) === identity));
    let files = withFiles ? (stage[fileIdentity] || []).filter(item => !item.sent) : [];
    if (!context || !context.writable || !identity || (id.startsWith("team/") && id !== "team/" + context.key) || (!text.trim() && !files.length) || new TextEncoder().encode(text).length > 16384 || peopleSending.has(identity) || typeof d.sendPeople !== "function") return;
    const revision = peopleRevisions.get(id) || 0;
    const draftScope = peopleStageIdentity(id, context); let draftState = peopleDraftStates.get(draftScope), draftTail = draftState?.tail;
    const visit = destinationVisit;
    const stillCurrent = () => selId() === id && destinationVisit === visit && isPeople() && peopleContext()?.key === context.key && (!draftScope || peopleStageIdentity(id) === draftScope);
    const token = beginPeopleFlight(identity, id, context, files);
    let originalDraft = null;
    if (!draftTail && typeof d.peopleFileStore?.freezePeopleDraft === "function") {
      savePeopleDraftInput(id, context, text);
      draftState = peopleDraftStates.get(draftScope); draftTail = draftState?.tail;
    }
    if (draftTail && typeof d.peopleFileStore?.freezePeopleDraft === "function") {
      const priorTail = draftTail;
      const frozen = priorTail.then(async draft => {
        if (!draft && !draftState.error) {
          draft = await d.peopleFileStore.savePeopleDraft({scope:draftScope,text,current:() => ownsPeopleFlight(token) && stillCurrent()});
          draftState.head = draft.head; draftState.version = draft.version;
        }
        if (!draft) throw new Error("The People draft could not be saved for recovery.");
        originalDraft = draft;
        return d.peopleFileStore.freezePeopleDraft({scope:draftScope, text, ...draft, current:() => ownsPeopleFlight(token) && stillCurrent()});
      });
      draftState.tail = frozen.then(() => priorTail, () => { draftState.forkAfterFreezeFailure = true; return priorTail; });
      draftTail = frozen;
    }
    try {
      if (files.length) {
        if (!recoveredPeopleFiles.has(fileIdentity)) restorePeopleFiles();
        let recovery = recoveringPeopleFiles.get(fileIdentity);
        while (recovery) {
          await recovery;
          if (!ownsPeopleFlight(token) || !stillCurrent()) return;
          recovery = recoveringPeopleFiles.get(fileIdentity);
        }
        if (!recoveredPeopleFiles.has(fileIdentity)) return;
        for (const item of files) if (item.sent) item.pending = false;
        files = files.filter(item => !item.sent);
        token.files = files;
        token.fileIdentity = files.length ? fileIdentity : "";
        if (!text.trim() && !files.length) return;
      }
      if (files.length) {
        for (const item of files) {
          if (item.needsUpload && item.localSaved && item.status !== "up") {
            item.status = "up";
            item.uploadPromise = uploadOne(item, id, fileIdentity, item.instance, item.peopleKey);
          }
        }
        await waitForUploads(files);
        if (!ownsPeopleFlight(token) || !stillCurrent() || !peopleCanAttach() || anyUploading(files)) return;
        if (files.some(item => item.status !== "done" || !item.ref?.id || item.removing)) { alertFn("Review failed or unfinished People files before sending."); return; }
      }
      const instance = context.owner ? (nodeById(id)?.instance_id || "") : "";
      let capturedDraft = draftTail ? await draftTail : null;
      if (!ownsPeopleFlight(token) || !stillCurrent()) return;
      if (capturedDraft && !originalDraft && typeof d.peopleFileStore?.beginPeopleTextPublication === "function") throw new Error("People draft recovery is unavailable.");
      let receipt;
      if (files.length) {
        const pending = d.pendingPeople?.();
        if (pending && !pending.locallyRetired) { alertFn("Review the previous unconfirmed People message before sending another."); return; }
        if (!capturedDraft) {
          savePeopleDraftInput(id, context, text); draftState = peopleDraftStates.get(draftScope); draftTail = draftState?.tail;
          capturedDraft = draftTail ? await draftTail : null;
        }
        if (!capturedDraft || (!originalDraft && typeof d.peopleFileStore?.beginPeoplePublication === "function") || !ownsPeopleFlight(token) || !stillCurrent()) throw new Error("The People draft could not be saved for recovery.");
        receipt = await d.sendPeople(context.key, text, files.map(item => item.ref.id), instance, {scope:draftScope, head:capturedDraft.head, version:capturedDraft.version, files:files.map(item => ({id:item.localID, fileToken:item.fileToken, stageRevision:item.stageRevision})), ...(originalDraft ? {requireDurableAdmission:true} : {}), current:() => ownsPeopleFlight(token) && stillCurrent(), onPrepared:snapshot => { if (ownsPeopleFlight(token) && stillCurrent()) token.pending = {storageKey:snapshot.storageKey, value:snapshot.value}; }});
      }
      else receipt = await d.sendPeople(context.key, text, undefined, instance, capturedDraft ? {scope:draftScope, head:capturedDraft.head, version:capturedDraft.version, current:() => ownsPeopleFlight(token), preflightCurrent:stillCurrent, ...(originalDraft ? {requireDurableAdmission:true} : {})} : null);
      if (!ownsPeopleFlight(token)) return;
      if (files.length) {
        const decision = await finishPeopleFiles(token, fileIdentity, files, receipt);
        if (!decision) return;
        const editableDraft = originalDraft || capturedDraft;
        if (decision.draftCleared && draftState?.head === editableDraft.head && draftState.version === editableDraft.version && (peopleRevisions.get(id) || 0) === revision) {
          draftState.head = undefined; draftState.version = undefined; draftState.text = "";
          if (stillCurrent() && promptText() === text) clearPrompt();
        }
        if (stillCurrent()) renderStage();
      }
      if (!files.length && capturedDraft && !receipt?.peopleTextStorageUnavailable && typeof d.peopleFileStore?.clearPeopleDraft === "function") {
        const identity = receipt?.peopleTextDecision?.identity;
        const exactDecision = receipt?.peopleTextDecision?.state === "completed" && Number.isSafeInteger(receipt?.seq) && receipt.seq > 0 && receipt.peopleTextDecision.seq === receipt.seq && identity && identity.scope === draftScope && identity.head === capturedDraft.head && identity.version === capturedDraft.version && typeof identity.publication === "string" && /^[0-9a-f]{32}:[1-9][0-9]*$/.test(identity.publication) && typeof identity.requestKey === "string" && /^[0-9a-f]{16}$/.test(identity.requestKey);
        const cleared = originalDraft ? !!(exactDecision && receipt?.peopleTextDecision?.draftCleared) : (receipt?.peopleTextDecision ? receipt.peopleTextDecision.draftCleared : await d.peopleFileStore.clearPeopleDraft({scope:draftScope, head:capturedDraft.head, version:capturedDraft.version, current:() => ownsPeopleFlight(token)}));
        const editableDraft = originalDraft || capturedDraft;
        if (cleared && ownsPeopleFlight(token) && draftState?.head === editableDraft.head && draftState.version === editableDraft.version && (peopleRevisions.get(id) || 0) === revision) {
          draftState.head = undefined; draftState.version = undefined; draftState.text = "";
          if (stillCurrent() && promptText() === text) clearPrompt();
        }
      }
      if (!files.length && !capturedDraft && Number.isSafeInteger(receipt?.seq) && receipt.seq > 0 && draftState && !draftState.head && peopleDraftStates.get(draftScope) === draftState && draftState.text === text && peopleRevisions.get(id) === revision) {
        draftState.text = "";
        if (stillCurrent() && peopleStageIdentity(id) === draftScope && promptText() === text) clearPrompt();
      }
    } catch (err) { if (err && ownsPeopleFlight(token) && stillCurrent() && sameIdentity() && !quietPeopleError(err)) alertFn(files.length && err.status ? peopleFileFailureText(err.message) : err.message); }
    finally {
      const owned = ownsPeopleFlight(token);
      releasePeopleFlight(token);
      if (owned && stillCurrent()) renderStage();
    }
  }

  async function interruptPrompt(){
    if (!currentComposerIdentity()) return;
    const cur = selId();
    if (!cur || !nodeById(cur)) return;
    const dest = cur;
    const identity = stageIdentity(dest);
    if (!identity) { alertFn("This chat's identity is unavailable. Reconnect before interrupting."); return; }
    const instance = nodeById(dest)?.instance_id || "";
    const visit = destinationVisit;
    const isCurrent = () => selId() === dest && destinationVisit === visit && stageIdentity(dest) === identity;
    try {
      if (typeof d.api === "function")
        await d.api(`/api/nodes/${encodeURIComponent(dest)}/send/interrupt${instance ? `?chat_instance=${encodeURIComponent(instance)}` : ""}`, { method: "POST", ...(instance ? {headers:{"X-Scimux-Chat-Instance":instance}} : {}) });
      if (!isCurrent()) return;
      if (typeof d.invalidateChat === "function") d.invalidateChat();
      if (typeof d.scheduleTick === "function") d.scheduleTick(250);
      else if (typeof d.tick === "function") setTimeoutFn(d.tick, 250);
    } catch (err) {
      if (!isCurrent()) return;
      /* surface every failure, including 409 (in flight / closed / unconfirmed) */
      if (err) alertFn(err.message);
      if (typeof d.invalidateChat === "function") d.invalidateChat();
      if (typeof d.refreshChat === "function") d.refreshChat();
    }
  }

  function waitForUploads(items){
    if (!anyUploading(items)) return Promise.resolve();
    return new Promise(res => {
      const t = setIntervalFn(() => {
        if (!anyUploading(items)){
          clearIntervalFn(t);
          res();
        }
      }, UPLOAD_WAIT_POLL_MS);
      setTimeoutFn(() => {
        clearIntervalFn(t);
        res();
      }, UPLOAD_WAIT_TIMEOUT_MS);
    });
  }

  async function sendPrompt(){
    if (!currentComposerIdentity()) return;
    if (!identityReady) return;
    if (isPeople()) return sendPeople();
    if (typeof d.agentAllowed === "function" && !d.agentAllowed()) return;
    const text = promptText().trim();
    const cur = selId();
    if (!cur || cur.startsWith("team/")) return;
    if (nodeById(cur)?.ended_at) return; /* closed thread: server would 409 */
    const dest = cur; /* capture: selection may change while send is in flight */
    const identity = stageIdentity(dest);
    if (!identity) { alertFn("This chat's identity is unavailable. Reconnect before sending."); return; }
    if (unresolvedSourceFiles(dest, identity)) { renderRecovery(dest); alertFn("Saved source files must be recovered before sending."); return; }
    if (sendingDestinations.has(identity)) return;
    const instance = nodeById(dest)?.instance_id || "";
    const visit = destinationVisit;
    const armedOwner = ownerChoice;
    const draftRevision = agentRevisions.get(dest) || 0;
    const originalStored = savedDraft(dest);
    const items = (stage[identity] || []).filter(item => !item.sent);
    const sourceSnapshot = readPromptSources(storage, dest);
    const turns = typeof d.getLastTurns === "function" ? d.getLastTurns() : [];
    const last = turns?.length ? turns[turns.length-1] : null;
    const boundary = last ? {uid:last.uid, segment:last.segment ?? 0, record:last.record} : null;
    const flight = {id:newPromptFlightID(nowFn()), identity, text, sources:sourceSnapshot, boundary, attachments:[], at:nowFn(), accepted:false};
    sendingDestinations.add(identity);
    writePromptFlights(storage, dest, [...readPromptFlights(storage, dest), flight]);
    clearPromptSources(storage, dest);
    if (sourceSnapshot.length && typeof d.promptSourcesChanged === "function") d.promptSourcesChanged(dest);
    function restoreSources(){
      sourceSnapshot.forEach(source => addPromptSource(storage, dest, source));
      writePromptFlights(storage, dest, readPromptFlights(storage, dest).filter(item => item.id !== flight.id));
      if (sourceSnapshot.length && typeof d.promptSourcesChanged === "function") d.promptSourcesChanged(dest);
    }
    function retireFlight(status = "not-dispatched", trace = null){
      try {
        const key = "scimux-retired-flight:" + identity;
        const prior = JSON.parse(storage.getItem(key) || "[]");
        const saved = Array.isArray(prior) ? prior : [prior];
        storage.setItem(key, JSON.stringify([...saved, {text, sources:sourceSnapshot, attachments:items.filter(item=>item.ref).map(item=>({name:item.name,ref:item.ref}))}]));
      } catch { /* in-memory stage still holds the files */ }
      const owner = identity.split("\0")[1];
      const recovered = {
        "scimux-draft:":text,
        "scimux-prompt-sources:":JSON.stringify(sourceSnapshot),
        "scimux-attachments:":JSON.stringify(items.filter(item=>item.ref).map(item=>({name:item.name,ref:item.ref}))),
        "scimux-delivery-status:":status,
        "scimux-delivery-trace:":JSON.stringify(trace),
      };
      if (!archiveRetiredDraft(storage, dest, owner, recovered)) {
        const previous = retiredMemory.get(dest) || [];
        retiredMemory.set(dest, [...previous, {owner, values:{...recovered, "scimux-memory-only:":true}}]);
      }
      if (selId() === dest) renderRecovery(dest);
      writePromptFlights(storage, dest, readPromptFlights(storage, dest).filter(item => item.id !== flight.id));
      sendingDestinations.delete(identity);
    }
    for (const item of items) {
      if (item.sourceKey && item.localSaved && item.needsUpload && item.status !== "up") {
        item.status = "up";
        item.uploadPromise = uploadOne(item, dest, identity, instance);
      }
    }
    if (anyUploading(items)) await waitForUploads(items);
    if (stageIdentity(dest) !== identity) {
      // Never restore an old flight into the replacement chat's draft. Keep
      // a separate local recovery copy for the retired identity instead.
      retireFlight();
      return;
    }
    const refs = completedRefs(items);
    if (items.some(item => item.sourceKey && item.status !== "done")) {
      restoreSources(); sendingDestinations.delete(identity);
      if (selId() === dest && !isPeople()) { alertFn("Review failed or unfinished source files before sending."); renderStage(); }
      return;
    }
    if (!hasSendPayload(text, refs)) { restoreSources(); sendingDestinations.delete(identity); return; }
    const destNode = nodeById(dest);
    if (destNode && String(destNode.agent || "").toLowerCase() === "claude" &&
        isClaudeNativeForkCommand(text)){
      restoreSources();
      sendingDestinations.delete(identity);
      alertFn("Claude /fork is not supported. Use scimux's Fork action to start a fresh chat that inherits launch configuration but not conversation history.");
      return;
    }
    flight.attachments = items.filter(item => item.status === "done" && item.ref).map(item => ({name:item.name, isImage:item.isImage, ref:item.ref, ...(item.sourceLocalID ? {sourceLocalID:item.sourceLocalID} : {})}));
    writePromptFlights(storage, dest, readPromptFlights(storage, dest).map(item => item.id === flight.id ? {...item, attachments:flight.attachments} : item));
    if (selId() === dest && !isPeople() && (agentRevisions.get(dest) || 0) === draftRevision) clearPrompt();
    const dispatchedItems = armedOwner ? items.filter(item => item.status === "done" && item.ref) : items;
    stage[identity] = (stage[identity] || []).filter(item => !dispatchedItems.includes(item)); /* retain unsent and newly staged files */
    if (selId() === dest) renderStage();
    /* optimistic echo, painted before the POST even resolves */
    const chatSig = typeof d.getChatSig === "function" ? d.getChatSig() : "";
    const echo = buildSentEcho({
      node: dest,
      text,
      atts: refs,
      at: nowFn(),
      chatSig,
      turnsLength: turns ? turns.length : 0,
    });
    if (typeof d.setSentEcho === "function") d.setSentEcho(echo);
    if (typeof d.paintEcho === "function") d.paintEcho(dest);
    try {
      let res = null;
      if (typeof d.api === "function"){
        if (armedOwner && destinationVisit === visit && selId() === dest && !isPeople()) setDestination("people");
        res = await d.api(`/api/nodes/${encodeURIComponent(dest)}/send${instance ? `?chat_instance=${encodeURIComponent(instance)}` : ""}`, {
          method: "POST",
          ...(instance ? {headers:{"X-Scimux-Chat-Instance":instance}} : {}),
          body: JSON.stringify({ text, attachments: refs, trace: true }),
        });
      }
      if (stageIdentity(dest) !== identity) { retireFlight(res?.status === "unconfirmed" ? "unconfirmed" : "accepted", res?.trace || null); return; }
      /* An unconfirmed outcome is not proof of delivery or non-delivery. */
      if (res && res.status === "unconfirmed") {
        restoreSources();
        alertFn("Delivery unconfirmed — check the terminal before retrying.");
        if (typeof d.clearSentEchoFor === "function") d.clearSentEchoFor(dest);
        const newer = savedDraft(dest);
        const merged = mergeFailedDraft(text, newer);
        if (dispatchedItems.length)
          stage[identity] = restoreStageOnFailure(stage[identity] || [], dispatchedItems);
        /* On screen first: the composer is the copy the user can see, and a
           storage that refuses must not be the reason they never get it back. */
        if (selId() === dest && !isPeople()){
          setPromptText(merged);
          renderStage();
        }
        if (merged) keepDraft(dest, merged);
        if (typeof d.invalidateChat === "function") d.invalidateChat();
        if (typeof d.scheduleTick === "function") d.scheduleTick(400);
        else if (typeof d.tick === "function") setTimeoutFn(d.tick, 400);
        sendingDestinations.delete(identity);
        return;
      }
      writePromptFlights(storage, dest, readPromptFlights(storage, dest).map(item => item.id === flight.id ? {...item, accepted:true, address:res?.trace} : item));
      const savedFiles = dispatchedItems.filter(item => item.sourceLocalID && item.localSaved);
      if (savedFiles.length) {
        try {
          await d.peopleFileStore.markPublishedBatch(sourceScope(identity), savedFiles.map(item => item.sourceLocalID), 1);
          for (const item of savedFiles) await d.peopleFileStore.remove(sourceScope(identity), item.sourceLocalID);
        } catch {
          for (const item of savedFiles) {
            item.sent = true; item.status = "err"; item.error = "Accepted; remove this saved file to finish cleanup.";
            if (!(stage[identity] || []).includes(item)) staged(identity).push(item);
          }
          if (selId() === dest && stageIdentity(dest) === identity) { alertFn("Accepted, but saved file cleanup needs review."); renderStage(); }
        }
      }
      if (sourceSnapshot.length && typeof d.promptSourcesChanged === "function") d.promptSourcesChanged(dest);
      /* Delivered, so the saved copy is finally redundant. Removing it any
         earlier -- as this did, before the request was even made -- destroys
         the one copy that outlives the tab while the send can still fail. */
      if ((agentRevisions.get(dest) || 0) === draftRevision && savedDraft(dest) === originalStored) forgetDraft(dest);
      const pending = readPendingForwards(storage, dest);
      /* A successful POST proves acceptance, not the destination address.
         chat.js mints the permanent link only after this exact text appears
         beyond the pre-send transcript boundary. */
      if (pending.length && text && !readAwaitingForward(storage, dest, nowFn()))
        writeAwaitingForward(storage, dest, text, turns?.length || 0, nowFn());
      sendingDestinations.delete(identity);
      if (typeof d.invalidateChat === "function") d.invalidateChat();
      dispatchedItems.forEach(x => {
        if (x.preview) URLImpl.revokeObjectURL(x.preview);
      });
      if (typeof d.scheduleTick === "function") d.scheduleTick(400);
      else if (typeof d.tick === "function") setTimeoutFn(d.tick, 400);
    } catch (err) {
      if (stageIdentity(dest) !== identity) { retireFlight("unconfirmed"); return; }
      restoreSources();
      /* surface every failure, including 409 (unconfirmed / in flight / closed) */
      if (err) alertFn(err && err.message);
      /* retract only this destination's echo */
      if (typeof d.clearSentEchoFor === "function") d.clearSentEchoFor(dest);
      const newer = savedDraft(dest);
      const merged = mergeFailedDraft(text, newer);
      if (dispatchedItems.length)
        stage[identity] = restoreStageOnFailure(stage[identity] || [], dispatchedItems);
      if (selId() === dest && !isPeople()){
        setPromptText(merged);
        renderStage();
      }
      if (merged) keepDraft(dest, merged);
      sendingDestinations.delete(identity);
    }
  }

  /* ---------- event adapters ---------- */

  function savePeopleDraftInput(id, context, text){
    const remembered = lastPeopleDraftScopes.get(id), currentNative = stageIdentity(id);
    const scope = context ? peopleStageIdentity(id, context) : (remembered?.native === currentNative ? remembered.scope : ""), lifetime = peopleLifetime, native = context?.owner || !context ? currentNative : "";
    if (!scope) { unavailablePeopleDraft = {id, visit:destinationVisit}; renderStage(); return; }
    unavailablePeopleDraft = null;
    lastPeopleDraftScopes.set(id, {scope, native:stageIdentity(id)});
    let state = peopleDraftStates.get(scope);
    if (!state) { state = {head:undefined, version:undefined, tail:Promise.resolve(), text:"", error:false}; peopleDraftStates.set(scope, state); }
    state.text = text;
    const current = () => peopleLifetime === lifetime && (!native || stageIdentity(id) === native);
    state.tail = state.tail.then(async () => {
      if (!current()) return;
      try {
        if (typeof d.peopleFileStore?.savePeopleDraft !== "function") throw new Error("People draft storage is unavailable.");
        const result = await d.peopleFileStore.savePeopleDraft({scope, text, ...(state.head && !state.forkAfterFreezeFailure ? {head:state.head, expected:state.version} : {}), current});
        if (!current()) return;
        state.head = result.head; state.version = result.version; state.error = false; state.forkAfterFreezeFailure = false;
        return result;
      } catch {
        if (current()) state.error = true;
      }
      if (current() && selId() === id && isPeople() && peopleStageIdentity(id) === scope) renderStage();
    });
  }

  function onPromptInput(){
    if (!currentComposerIdentity()) return;
    if (!identityReady) return;
    resizePrompt();
    const id = selId();
    if (id && isPeople()) {
      peopleRevisions.set(id, (peopleRevisions.get(id) || 0) + 1);
      savePeopleDraftInput(id, peopleContext(), promptText());
      if (composerBusy) renderBusy();
    }
    else if (id) {
      agentRevisions.set(id, (agentRevisions.get(id) || 0) + 1); keepDraft(id, promptText());
      if (!promptText().trim()) {
        const identity = stageIdentity(id), sources = readPromptSources(storage, id), cleared = clearedSources(identity);
        if (damagedSourceMetadata.has(identity)) { renderRecovery(id); return; }
        for (const source of sources) cleared.add(promptSourceKey(source));
        saveClearedSources(identity, cleared);
        const files = stage[identity] || [];
        writePromptSources(storage, id, sources.filter(source => files.some(item => item.sourceKey === promptSourceKey(source))));
        if (typeof d.promptSourcesChanged === "function") d.promptSourcesChanged(id);
      }
    }
  }

  function onPromptPaste(e){
    const files = e.clipboardData && e.clipboardData.files;
    if (files && files.length){
      e.preventDefault();
      addFiles(files);
      return;
    }
    const text = e.clipboardData && e.clipboardData.getData
      ? e.clipboardData.getData("text/plain")
      : null;
    if (text == null) return;
    e.preventDefault();
    if (doc && typeof doc.execCommand === "function")
      doc.execCommand("insertText", false, text);
  }

  function onPromptKeydown(e){
    if (shouldSendOnEnter({
      key: e.key,
      shiftKey: e.shiftKey,
      altKey: e.altKey,
      metaKey: e.metaKey,
      ctrlKey: e.ctrlKey,
      isPhoneTouch: isPhoneTouch(),
    })){
      e.preventDefault();
      sendPrompt();
    }
  }

  function onPromptBeforeInput(e){
    if (e.inputType !== "insertFromDrop") return;
    e.preventDefault();
    const text = e.dataTransfer && e.dataTransfer.getData
      ? e.dataTransfer.getData("text/plain")
      : "";
    if (text && doc && typeof doc.execCommand === "function")
      doc.execCommand("insertText", false, text);
  }

  function onSendClick(){
    if (stopAvailable()) interruptPrompt();
    else sendPrompt();
  }

  function onAttAddClick(e){
    e.stopPropagation();
    if (!attmenu || !attadd) return;
    if (!attmenu.hidden){ closeAttMenu(); return; }
    attmenu.hidden = false; /* unhide to measure, then place upward */
    if (typeof attadd.getBoundingClientRect === "function"){
      const r = attadd.getBoundingClientRect();
      if (attmenu.style){
        attmenu.style.left = r.left + "px";
        const h = attmenu.offsetHeight || 0;
        attmenu.style.top = (r.top - h - 8) + "px";
      }
    }
    if (attadd.classList) attadd.classList.add("armed");
    if (typeof attadd.setAttribute === "function")
      attadd.setAttribute("aria-expanded", "true");
  }

  function onAttMenuClick(e){
    const t = e.target;
    const b = t && typeof t.closest === "function"
      ? t.closest("button[data-src]")
      : (t && t.dataset && t.dataset.src ? t : null);
    if (!b) return;
    closeAttMenu();
    const key = attachmentInputKey(b.dataset && b.dataset.src);
    const input = key === "image" ? attimg : attfile;
    if (input && typeof input.click === "function") input.click();
  }

  function onAttImgChange(e){
    addFiles(e.target && e.target.files);
    if (e.target) e.target.value = "";
  }

  function onAttFileChange(e){
    addFiles(e.target && e.target.files);
    if (e.target) e.target.value = "";
  }

  function onBarDragOver(e){
    if (e.dataTransfer && e.dataTransfer.types){
      const types = Array.from(e.dataTransfer.types);
      if (types.includes("Files")){
        e.preventDefault();
        if (promptbar && promptbar.classList) promptbar.classList.add("drop");
      }
    }
  }

  function onBarDragLeave(){
    if (promptbar && promptbar.classList) promptbar.classList.remove("drop");
  }

  function onBarDrop(e){
    if (promptbar && promptbar.classList) promptbar.classList.remove("drop");
    if (e.dataTransfer && e.dataTransfer.files && e.dataTransfer.files.length){
      e.preventDefault();
      addFiles(e.dataTransfer.files);
    }
  }

  function onDocClick(){
    if (attmenu && !attmenu.hidden) closeAttMenu();
  }

  function onDocKeydown(e){
    if (e.key === "Escape" && attmenu && !attmenu.hidden) closeAttMenu();
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
    if (attadd) attadd.innerHTML = icons.ICON_PLUS || "";
    if (attmenu && attmenu.children){
      if (attmenu.children[0])
        attmenu.children[0].innerHTML = (icons.ICON_IMAGE || "") + "<span>Choose image…</span>";
      if (attmenu.children[1])
        attmenu.children[1].innerHTML = (icons.ICON_FILE || "") + "<span>Choose file…</span>";
    }
  }

  function bind(){
    if (bound) return;
    localReviewAlive = true;
    bound = true;
    initIcons();
    /* edge-only initial chrome */
    setComposerBusy(false);
    setComposerClosed(false);

    listen(roots.destinationPeople, "click", () => setDestination("people"));
    listen(roots.destinationAgent, "click", () => setDestination("agent"));
    listen(prompt, "input", onPromptInput);
    listen(prompt, "paste", onPromptPaste);
    listen(prompt, "keydown", onPromptKeydown);
    listen(prompt, "beforeinput", onPromptBeforeInput);
    listen(sendbtn, "click", onSendClick);
    listen(attadd, "click", onAttAddClick);
    listen(attmenu, "click", onAttMenuClick);
    listen(attimg, "change", onAttImgChange);
    listen(attfile, "change", onAttFileChange);
    listen(promptbar, "dragover", onBarDragOver);
    listen(promptbar, "dragenter", onBarDragOver);
    listen(promptbar, "dragleave", onBarDragLeave);
    listen(promptbar, "dragend", onBarDragLeave);
    listen(promptbar, "drop", onBarDrop);
    /* document-level menu dismissal — composer-owned, singular */
    listen(doc, "click", onDocClick);
    listen(doc, "keydown", onDocKeydown);
  }

  function destroy(){
    peopleLifetime++;
    recoveredPeopleFiles.clear();
    recoveringPeopleFiles.clear();
    peopleDraftStates.clear();
    lastPeopleDraftScopes.clear();
    unavailablePeopleDraft = null;
    for (const token of peopleSending.values()) releasePeopleFlight(token);
    localReviewAlive = false;
    textReview = null;
    stopLocalReview();
    clearLegacyPeopleReview();
    for (const controller of preparationControllers) controller.abort();
    while (cleanups.length){
      try { cleanups.pop()(); } catch { /* ignore */ }
    }
    clearStageRemoveListeners();
    closeAttMenu();
    bound = false;
  }

  return {
    bind,
    destroy,
    setComposerBusy,
    setComposerClosed,
    setAttachAvail,
    setPromptText,
    clearPrompt,
    promptText,
    resizePrompt,
    renderStage,
    onSelect,
    reconcileIdentity: currentComposerIdentity,
    preparePrompt,
    prepareCapturePrompt,
    prepareSharedPrompt,
    recoverPromptDraft,
    agentDraftText,
    setDestination,
    refreshDestination,
    contextChanged,
    destination: () => destination,
    sendPrompt,
    interruptPrompt,
    addFiles,
    closeAttMenu,
  };
}
