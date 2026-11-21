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
import {
  readPendingForwards,
  clearPendingForwards,
  readAwaitingForward,
  writeAwaitingForward,
  bindDraftIdentity,
  archiveRetiredDraft,
  readRetiredDrafts,
  addPromptSource, clearPromptSources, promptSourceKey, readPromptSources, readPromptFlights, writePromptFlights,
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
  let ownerChoice = false;
  const peopleRevisions = new Map();
  const agentRevisions = new Map();
  const peopleSending = new Set();
  const sendingDestinations = new Set();
  const cleanups = [];
  /* dynamic remove buttons rebound on each renderStage */
  let stageRemoveCleanups = [];

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
  function peopleDraftKey(id){ return "scimux-people-draft:" + id; }
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
      if (attadd) attadd.hidden = !on || isPeople();
      return;
    }
    canAttach = on;
    if (attadd) attadd.hidden = !on || isPeople();
    if (!on) closeAttMenu();
  }

  function staged(key){
    return stage[key] || (stage[key] = []);
  }

  function clearStageRemoveListeners(){
    while (stageRemoveCleanups.length){
      try { stageRemoveCleanups.pop()(); } catch { /* ignore */ }
    }
  }

  function renderStage(){
    if (!attstage) return;
    const id = selId();
    const items = id && !isPeople() ? (stage[stageIdentity(id)] || []) : [];
    clearStageRemoveListeners();
    if (!items.length){
      attstage.hidden = true;
      attstage.innerHTML = "";
      return;
    }
    attstage.hidden = false;
    attstage.innerHTML = stageRowHTML(items, {
      esc: escFn,
      iconFile: icons.ICON_FILE || "",
    });
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

  function removeStaged(key){
    const id = selId();
    if (!id) return;
    const arr = stage[stageIdentity(id)] || [];
    const i = arr.findIndex(x => String(x.key) === String(key));
    if (i < 0) return;
    if (arr[i].preview) URLImpl.revokeObjectURL(arr[i].preview);
    arr.splice(i, 1);
    renderStage();
  }

  async function uploadOne(it, dest, identity, instance){
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
      const r = await fetchImpl(
        `/api/nodes/${encodeURIComponent(dest)}/attachments${instance ? `?chat_instance=${encodeURIComponent(instance)}` : ""}`,
        withCsrf({ method: "POST", body: fd, ...(instance ? {headers:{"X-Scimux-Chat-Instance":instance}} : {}) }),
      );
      if (!r.ok) throw new Error((await r.text()) || r.statusText);
      const body = await r.json();
      it.ref = body.attachments[0];
      it.status = "done";
    } catch (e) {
      it.status = "err";
      it.error = e.message;
    }
    if (selId() === dest && stageIdentity(dest) === identity) renderStage();
  }

  function addFiles(files, dest){
    if (!currentComposerIdentity()) return;
    dest = dest || selId();
    if (!identityReady) return;
    if (!files || !files.length) return;
    if (isPeople()) { alertFn("People messages currently support text only. Choose Agent to attach files."); return; }
    if (!canAttach || !dest || dest.startsWith("team/")) return;
    const identity = stageIdentity(dest);
    if (!identity) { alertFn("This chat's identity is unavailable. Reconnect before attaching files."); return; }
    const instance = nodeById(dest)?.instance_id || "";
    const arr = staged(identity);
    const list = Array.from(files);
    for (const f of list){
      const isImage = isImageFileType(f.type);
      const it = {
        key: ++stageKey,
        file: f,
        name: stagedFileName(f, isImage),
        isImage,
        preview: isImage ? URLImpl.createObjectURL(f) : "",
        ref: null,
        status: "up",
      };
      arr.push(it);
      uploadOne(it, dest, identity, instance);
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
    const entries = id ? [...readRetiredDrafts(storage, id), ...(retiredMemory.get(id) || [])] : [];
    roots.recovery.hidden = !entries.length;
    roots.recovery.innerHTML = entries.length
      ? `<details><summary>Review saved drafts</summary>${retiredDraftReviewHTML(entries)}</details>` : "";
  }

  function onSelect({preferAgent = false} = {}){
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
    contextKey = context?.key || "";
    ownerChoice = !!context?.owner && !id.startsWith("team/");
    explicitAgent = !!preferAgent && !id.startsWith("team/");
    destination = !explicitAgent && (id.startsWith("team/") || contextKey) ? "people" : "agent";
    setPromptText(identityReady ? (isPeople() ? (storage.getItem(peopleDraftKey(id)) || "") : savedDraft(id)) : "");
    if (!identityReady) alertFn("Draft storage is unavailable. Reconnect before using this chat's composer.");
    refreshDestination();
    setAttachAvail(false);
    renderStage();
  }

  function refreshDestination(){
    if (roots.destination) roots.destination.hidden = !ownerChoice;
    if (roots.destinationPeople) roots.destinationPeople.setAttribute("aria-pressed", String(isPeople()));
    if (roots.destinationAgent) roots.destinationAgent.setAttribute("aria-pressed", String(!isPeople()));
    const busy = composerBusy; const closed = composerClosed;
    composerBusy = null; composerClosed = null;
    setComposerBusy(busy); setComposerClosed(closed);
    if (attadd) attadd.hidden = !canAttach || isPeople();
    if (isPeople()) closeAttMenu();
    renderStage();
  }

  function contextChanged(){
    const context = peopleContext();
    const key = context?.key || "";
    if (context?.owner && !selId().startsWith("team/")) ownerChoice = true;
    if (key && key !== contextKey && !selId().startsWith("team/") && !explicitAgent) {
      if (!isPeople()) keepDraft(selId(), promptText());
      contextKey = key;
      setDestination("people");
    } else { contextKey = key; refreshDestination(); }
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
    destinationVisit++;
    destination = next;
    explicitAgent = next === "agent";
    setPromptText(isPeople() ? (storage.getItem(peopleDraftKey(selId())) || "") : savedDraft(selId()));
    refreshDestination();
  }

  function preparePrompt(text, source){
    if (!currentComposerIdentity()) return false;
    if (!identityReady) return false;
    const id = selId();
    if (!id || id.startsWith("team/") || !nodeById(id) || nodeById(id).ended_at || !promptSourceKey(source) ||
        typeof d.canPreparePrompt !== "function" || !d.canPreparePrompt(source) || !String(text || "").trim()) return false;
    const prior = isPeople() ? savedDraft(id) : promptText();
    const merged = (prior ? prior + "\n\n" : "") + text;
    keepDraft(id, merged);
    addPromptSource(storage, id, source);
    setDestination("agent");
    setPromptText(merged);
    if (prompt) focusAtEnd(prompt);
    if (typeof d.promptSourcesChanged === "function") d.promptSourcesChanged(id);
    return true;
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

  async function sendPeople(){
    const context = peopleContext(); const id = selId(); const text = promptText();
    const identity = id.startsWith("team/") ? id : (stageIdentity(id) || (context?.key ? "people/" + context.key : ""));
    const sameIdentity = () => id.startsWith("team/") || (identity.startsWith("people/")
      ? selId() === id && peopleContext()?.key === context.key : stageIdentity(id) === identity);
    if (!context || !context.writable || !identity || !text.trim() || new TextEncoder().encode(text).length > 16384 || peopleSending.has(identity) || typeof d.sendPeople !== "function") return;
    const revision = peopleRevisions.get(id) || 0;
    peopleSending.add(identity);
    try {
      await d.sendPeople(context.key, text);
      if (!sameIdentity()) return;
      if ((peopleRevisions.get(id) || 0) === revision && storage.getItem(peopleDraftKey(id)) === text) {
        storage.removeItem(peopleDraftKey(id));
        if (selId() === id && isPeople() && promptText() === text) clearPrompt();
      }
    } catch (err) { if (err && selId() === id && sameIdentity()) alertFn(err.message); }
    finally { peopleSending.delete(identity); }
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
    if (sendingDestinations.has(identity)) return;
    const instance = nodeById(dest)?.instance_id || "";
    const visit = destinationVisit;
    const armedOwner = ownerChoice;
    const draftRevision = agentRevisions.get(dest) || 0;
    const originalStored = savedDraft(dest);
    const items = (stage[identity] || []).slice();
    const sourceSnapshot = readPromptSources(storage, dest);
    const turns = typeof d.getLastTurns === "function" ? d.getLastTurns() : [];
    const last = turns?.length ? turns[turns.length-1] : null;
    const boundary = last ? {uid:last.uid, segment:last.segment ?? 0, record:last.record} : null;
    const flight = {id:newPromptFlightID(nowFn()), text, sources:sourceSnapshot, boundary, attachments:[], at:nowFn(), accepted:false};
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
    if (anyUploading(items)) await waitForUploads(items);
    if (stageIdentity(dest) !== identity) {
      // Never restore an old flight into the replacement chat's draft. Keep
      // a separate local recovery copy for the retired identity instead.
      retireFlight();
      return;
    }
    const refs = completedRefs(items);
    if (!hasSendPayload(text, refs)) { restoreSources(); sendingDestinations.delete(identity); return; }
    const destNode = nodeById(dest);
    if (destNode && String(destNode.agent || "").toLowerCase() === "claude" &&
        isClaudeNativeForkCommand(text)){
      restoreSources();
      sendingDestinations.delete(identity);
      alertFn("Claude /fork is not supported. Use scimux's Fork action to start a fresh chat that inherits launch configuration but not conversation history.");
      return;
    }
    flight.attachments = items.filter(item => item.status === "done" && item.ref).map(item => ({name:item.name, isImage:item.isImage, ref:item.ref}));
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

  function onPromptInput(){
    if (!currentComposerIdentity()) return;
    if (!identityReady) return;
    resizePrompt();
    const id = selId();
    if (id && isPeople()) { peopleRevisions.set(id, (peopleRevisions.get(id) || 0) + 1); storage.setItem(peopleDraftKey(id), promptText()); if (composerBusy) renderBusy(); }
    else if (id) { agentRevisions.set(id, (agentRevisions.get(id) || 0) + 1); keepDraft(id, promptText()); if (!promptText().trim()) { clearPromptSources(storage, id); if (typeof d.promptSourcesChanged === "function") d.promptSourcesChanged(id); } }
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
