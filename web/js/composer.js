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
  return `<div class="stagechip ${it.status}" data-key="${it.key}" title="${disp}">
        ${thumb}${spin}<span class="snm">${disp}</span>
        <button class="sx" data-key="${it.key}" aria-label="Remove ${disp}">&times;</button>
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
  let stageKey = 0;

  let composerBusy = null;
  let composerClosed = null;
  let canAttach = false;
  let bound = false;
  const cleanups = [];
  /* dynamic remove buttons rebound on each renderStage */
  let stageRemoveCleanups = [];

  function selId(){ return g("sel", "") || ""; }
  function nodeById(id){
    return typeof d.nodeById === "function" ? d.nodeById(id) : null;
  }
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

  function setComposerBusy(active){
    if (composerBusy === !!active) return; /* poll-safe: no DOM churn per tick */
    composerBusy = !!active;
    const chrome = busyChrome(composerBusy);
    if (promptbar && promptbar.classList)
      promptbar.classList.toggle("busy", chrome.busy);
    if (sendbtn){
      if (sendbtn.classList) sendbtn.classList.toggle("stop", chrome.stopClass);
      sendbtn.innerHTML = composerBusy
        ? (icons.ICON_STOP || "")
        : (icons.ICON_SEND || "");
      if (typeof sendbtn.setAttribute === "function")
        sendbtn.setAttribute("aria-label", chrome.ariaLabel);
      sendbtn.title = chrome.title;
    }
  }

  function setComposerClosed(closed){
    if (composerClosed === !!closed) return;
    composerClosed = !!closed;
    const chrome = closedChrome(composerClosed);
    if (promptbar && promptbar.classList)
      promptbar.classList.toggle("closed", chrome.closed);
    if (prompt){
      if (typeof prompt.setAttribute === "function")
        prompt.setAttribute("contenteditable", chrome.contentEditable);
      if (prompt.dataset) prompt.dataset.placeholder = chrome.placeholder;
    }
    if (sendbtn) sendbtn.disabled = chrome.sendDisabled;
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
      if (attadd) attadd.hidden = !on;
      return;
    }
    canAttach = on;
    if (attadd) attadd.hidden = !on;
    if (!on) closeAttMenu();
  }

  function staged(id){
    return stage[id] || (stage[id] = []);
  }

  function clearStageRemoveListeners(){
    while (stageRemoveCleanups.length){
      try { stageRemoveCleanups.pop()(); } catch { /* ignore */ }
    }
  }

  function renderStage(){
    if (!attstage) return;
    const id = selId();
    const items = id ? (stage[id] || []) : [];
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
    const arr = stage[id] || [];
    const i = arr.findIndex(x => String(x.key) === String(key));
    if (i < 0) return;
    if (arr[i].preview) URLImpl.revokeObjectURL(arr[i].preview);
    arr.splice(i, 1);
    renderStage();
  }

  async function uploadOne(it, dest){
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
        `/api/nodes/${encodeURIComponent(dest)}/attachments`,
        withCsrf({ method: "POST", body: fd }),
      );
      if (!r.ok) throw new Error((await r.text()) || r.statusText);
      const body = await r.json();
      it.ref = body.attachments[0];
      it.status = "done";
    } catch (e) {
      it.status = "err";
      it.error = e.message;
    }
    if (selId() === dest) renderStage();
  }

  function addFiles(files, dest){
    dest = dest || selId();
    if (!canAttach || !dest || !files || !files.length) return;
    const arr = staged(dest);
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
      uploadOne(it, dest);
    }
    if (selId() === dest) renderStage();
  }

  function onSelect(){
    /* restore this node's draft and stage; hide + until chat confirms turns */
    const id = selId();
    setPromptText(storage.getItem(draftStorageKey(id)) || "");
    setAttachAvail(false);
    renderStage();
  }

  async function interruptPrompt(){
    const cur = selId();
    if (!cur || !nodeById(cur)) return;
    const dest = cur;
    try {
      if (typeof d.api === "function")
        await d.api(`/api/nodes/${encodeURIComponent(dest)}/send/interrupt`, { method: "POST" });
      if (typeof d.invalidateChat === "function") d.invalidateChat();
      if (typeof d.scheduleTick === "function") d.scheduleTick(250);
      else if (typeof d.tick === "function") setTimeoutFn(d.tick, 250);
    } catch (err) {
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
    const text = promptText().trim();
    const cur = selId();
    if (!cur) return;
    if (nodeById(cur)?.ended_at) return; /* closed thread: server would 409 */
    const dest = cur; /* capture: selection may change while send is in flight */
    const items = (stage[dest] || []).slice();
    if (anyUploading(items)) await waitForUploads(items);
    const refs = completedRefs(items);
    if (!hasSendPayload(text, refs)) return;
    clearPrompt();
    storage.removeItem(draftStorageKey(dest));
    stage[dest] = []; /* clear optimistically; restore on failure */
    if (selId() === dest) renderStage();
    /* optimistic echo, painted before the POST even resolves */
    const chatSig = typeof d.getChatSig === "function" ? d.getChatSig() : "";
    const turns = typeof d.getLastTurns === "function" ? d.getLastTurns() : [];
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
        res = await d.api(`/api/nodes/${encodeURIComponent(dest)}/send`, {
          method: "POST",
          body: JSON.stringify({ text, attachments: refs }),
        });
      }
      /* 200 with status:"unconfirmed" is not-delivered — keep the draft */
      if (res && res.status === "unconfirmed") {
        alertFn("not delivered — check the terminal");
        if (typeof d.clearSentEchoFor === "function") d.clearSentEchoFor(dest);
        const newer = storage.getItem(draftStorageKey(dest)) || "";
        const merged = mergeFailedDraft(text, newer);
        if (merged) storage.setItem(draftStorageKey(dest), merged);
        if (items.length)
          stage[dest] = restoreStageOnFailure(stage[dest] || [], items);
        if (selId() === dest){
          setPromptText(merged);
          renderStage();
        }
        if (typeof d.invalidateChat === "function") d.invalidateChat();
        if (typeof d.scheduleTick === "function") d.scheduleTick(400);
        else if (typeof d.tick === "function") setTimeoutFn(d.tick, 400);
        return;
      }
      if (typeof d.invalidateChat === "function") d.invalidateChat();
      items.forEach(x => {
        if (x.preview) URLImpl.revokeObjectURL(x.preview);
      });
      if (typeof d.scheduleTick === "function") d.scheduleTick(400);
      else if (typeof d.tick === "function") setTimeoutFn(d.tick, 400);
    } catch (err) {
      /* surface every failure, including 409 (unconfirmed / in flight / closed) */
      if (err) alertFn(err && err.message);
      /* retract only this destination's echo */
      if (typeof d.clearSentEchoFor === "function") d.clearSentEchoFor(dest);
      const newer = storage.getItem(draftStorageKey(dest)) || "";
      const merged = mergeFailedDraft(text, newer);
      if (merged) storage.setItem(draftStorageKey(dest), merged);
      if (items.length)
        stage[dest] = restoreStageOnFailure(stage[dest] || [], items);
      if (selId() === dest){
        setPromptText(merged);
        renderStage();
      }
    }
  }

  /* ---------- event adapters ---------- */

  function onPromptInput(){
    resizePrompt();
    const id = selId();
    if (id) storage.setItem(draftStorageKey(id), promptText());
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
    if (composerBusy) interruptPrompt();
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
    sendPrompt,
    interruptPrompt,
    addFiles,
    closeAttMenu,
  };
}
