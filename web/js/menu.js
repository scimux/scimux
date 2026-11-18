import { esc, stripAssetRefs } from "./format.js";

/* Shared popover menu: pure placement + open/close controller + item HTML.
 *
 * Lifted from notes.js openSectionMenu / openDeleteConfirm (P5). Both
 * consumers share one element slot and one closer; they differ only in
 * content, className, and the horizontal OFFSET baked into left.
 *
 * Host access: document arrives through createPopoverMenu(doc). No module-
 * level document/window — the suite is headless and every other web/js
 * module takes its host through parameters.
 */

/**
 * Pure placement decision relative to a panel.
 *   top  = anchor.bottom - panel.top + 4, unless the measured menu would
 *          cross the panel's bottom edge; then it opens above the anchor
 *   left = max(8, min(anchor.left - panel.left - offset, panel.width - 190))
 * offset is 150 for the section menu and 40 for the delete confirm — do not
 * bake either in; a wrong default silently moves the other popover 110px.
 */
export function menuPlacement(anchorRect, panelRect, { offset = 0, menuHeight = 0 } = {}){
  const r = anchorRect || {};
  const pr = panelRect || {};
  const off = Number(offset) || 0;
  const gap = 4;
  const inset = 8;
  const panelTop = Number(pr.top) || 0;
  const panelHeight = Number(pr.height) ||
    Math.max(0, (Number(pr.bottom) || 0) - panelTop);
  const height = Math.max(0, Number(menuHeight) || 0);
  const below = (Number(r.bottom) || 0) - panelTop + gap;
  let top = below;
  if (height > 0 && panelHeight > 0 && below + height > panelHeight - inset){
    top = Math.max(inset, (Number(r.top) || 0) - panelTop - height - gap);
  }
  const left = Math.max(
    8,
    Math.min(
      (Number(r.left) || 0) - (Number(pr.left) || 0) - off,
      (Number(pr.width) || 0) - 190,
    ),
  );
  return { top, left };
}

/** Separator row between ordinary items and a destructive action. */
export function menuSepHTML(){
  return `<div class="sep"></div>`;
}

/**
 * One menu button. `attrs` is a raw attribute string (e.g. data-mi="del");
 * `danger` adds class="danger"; `icon` is optional leading markup.
 */
export function menuButtonHTML({ attrs = "", label = "", danger = false, icon = "" } = {}){
  const cls = danger ? ` class="danger"` : "";
  const attrStr = attrs ? " " + attrs : "";
  const iconStr = icon || "";
  /* no forced type= — section menu buttons historically omit it; callers that
     need type="button" (delete confirm) put it in attrs */
  return `<button${attrStr}${cls}>${iconStr}<span>${label}</span></button>`;
}

/**
 * One live popover slot. open() closes any prior menu first (exactly one in
 * the document). close() removes it. shouldCloseForClick mirrors the notes
 * onDocClick predicate: dismiss on outside click, not on the menu itself or
 * an exclude selector (notes passes "[data-secmenu]"). Escape closes and
 * returns focus to the trigger element passed via open() options (P6) —
 * never document.activeElement at module scope. Escape is capture-phase so
 * an open menu outranks earlier bubble listeners (app.js full-screen ladder).
 */
export function createPopoverMenu(doc){
  let menuEl = null;
  let triggerEl = null;
  let keyHandler = null;
  let onCloseCb = null;

  function unbindKey(){
    if (keyHandler && doc && typeof doc.removeEventListener === "function"){
      try { doc.removeEventListener("keydown", keyHandler, true); } catch { /* ignore */ }
    }
    keyHandler = null;
  }

  function close(){
    unbindKey();
    const cb = onCloseCb;
    onCloseCb = null;
    const wasOpen = !!menuEl;
    if (menuEl){
      menuEl.remove();
      menuEl = null;
    }
    triggerEl = null;
    if (wasOpen && typeof cb === "function") cb();
  }

  function element(){
    return menuEl;
  }

  /**
   * @param {object} opts
   * @param {Element} [opts.panel]  append target (e.g. #wspanel)
   * @param {Element} [opts.anchor] positioning anchor
   * @param {number}  [opts.offset] horizontal offset (150 or 40)
   * @param {string}  [opts.className] defaults to "popmenu"
   * @param {string}  [opts.html] innerHTML
   * @param {function} [opts.onClick] click listener on the menu root
   * @param {Element} [opts.trigger] focus-return target on Escape; defaults
   *   to anchor when omitted
   * @param {function} [opts.onClose] called once when an open menu is
   *   dismissed (public close, Escape, replacement open, teardown). Cleared
   *   before the call so reentrant/repeated close cannot double-notify.
   *   Omitted on existing consumers — behaviour unchanged.
   */
  function open({
    panel,
    anchor,
    offset = 0,
    className = "popmenu",
    html = "",
    onClick,
    trigger,
    onClose,
  } = {}){
    close();
    if (!doc || typeof doc.createElement !== "function") return null;
    const m = doc.createElement("div");
    m.className = className || "popmenu";
    if (html != null) m.innerHTML = html;
    if (panel && typeof panel.appendChild === "function") panel.appendChild(m);
    const r = anchor && typeof anchor.getBoundingClientRect === "function"
      ? anchor.getBoundingClientRect()
      : { bottom: 0, left: 0 };
    const pr = panel && typeof panel.getBoundingClientRect === "function"
      ? panel.getBoundingClientRect()
      : { top: 0, left: 0, width: 400 };
    const mr = typeof m.getBoundingClientRect === "function"
      ? m.getBoundingClientRect()
      : { height: 0 };
    const pos = menuPlacement(r, pr, { offset, menuHeight: mr.height });
    m.style.top = pos.top + "px";
    m.style.left = pos.left + "px";
    menuEl = m;
    triggerEl = trigger || anchor || null;
    onCloseCb = typeof onClose === "function" ? onClose : null;
    if (typeof onClick === "function") m.addEventListener("click", onClick);
    /* Escape closes and restores focus to the open()-provided trigger.
       Capture phase so an open popover refuses the key before later bubble
       listeners (app.js full-screen ladder is registered first, on bubble). */
    if (typeof doc.addEventListener === "function"){
      keyHandler = (ev) => {
        if (!menuEl) return;
        if (ev.key !== "Escape" && ev.key !== "Esc") return;
        if (typeof ev.preventDefault === "function") ev.preventDefault();
        if (typeof ev.stopPropagation === "function") ev.stopPropagation();
        const returnTo = triggerEl;
        close();
        if (returnTo && typeof returnTo.focus === "function") returnTo.focus();
      };
      doc.addEventListener("keydown", keyHandler, true);
    }
    return m;
  }

  /**
   * True when a document click should dismiss the open menu.
   * Matches notes onDocClick: menu open + target present + not inside
   * .popmenu + not matching exclude (e.g. "[data-secmenu]").
   */
  function shouldCloseForClick(target, { exclude } = {}){
    if (!menuEl || !target) return false;
    if (typeof target.closest === "function"){
      if (target.closest(".popmenu")) return false;
      if (exclude && target.closest(exclude)) return false;
    }
    return true;
  }

  return { open, close, element, shouldCloseForClick };
}

/* Shared gesture boundary for ordinary and received conversation bubbles. */

export function dispatchBubbleAction(action, capture, adapter) {
  if (!capture) return false;
  const controller = adapter.controller;
  let actionToken = controller?.beginAction();
  if (action === "copy" && typeof adapter.copy === "function") { adapter.copy(capture.text || ""); return true; }
  if (action === "fork" && typeof adapter.fork === "function") { adapter.fork(stripAssetRefs(capture.text || "")); return true; }
  if (action === "prompt" && typeof adapter.prepare === "function" && typeof adapter.canPrepare === "function" && adapter.canPrepare(capture)) {
    adapter.prepare(stripAssetRefs(capture.text || ""), capture.source); return true;
  }
  if (action === "desc" && typeof adapter.describe === "function") {
    if (typeof adapter.choose !== "function") { adapter.describe(capture.node || "", capture); return true; }
    const eligible = () => adapter.nodes().filter(n => n && n.id && n.instance_id && !n.id.startsWith("team/") && !n.ended_at);
    const destinations = new Map(eligible().map(n => [n.id, n.instance_id]));
    adapter.choose({title:"Use as description…", bubbleOwner:controller, choices:eligible().map(n => ({id:n.id,label:n.title || "Untitled chat"})), onChoose:choice => {
      const active = token => adapter.isCurrent() && (!controller || controller.isCurrentAction(token));
      const current = token => active(token) && destinations.has(choice?.id) && eligible().some(n => n.id === choice.id && n.instance_id === destinations.get(choice.id));
      if (!current(actionToken)) return;
      actionToken = controller?.beginAction();
      const choiceToken = actionToken;
      if (typeof adapter.verify !== "function") { adapter.describe(choice.id, capture); return; }
      Promise.resolve().then(() => adapter.verify(choice.id, destinations.get(choice.id))).then(ok => {
        if (ok && current(choiceToken)) adapter.describe(choice.id, capture);
        else if (!ok && active(choiceToken)) adapter.unavailable?.();
      }, () => { if (active(choiceToken)) adapter.unavailable?.(); });
    }});
    return true;
  }
  if (action === "bookmark" && typeof adapter.capture === "function" && typeof adapter.commit === "function") {
    if (adapter.exists(capture, "before")) { adapter.duplicate?.(); return true; }
    const commit = value => {
      if (!adapter.isCurrent() || adapter.exists(value, "after")) return;
      adapter.commit(value);
    };
    const fail = () => { if (adapter.isCurrent()) adapter.error?.(); };
    try {
      const value = adapter.capture(capture);
      if (value && typeof value.then === "function") { adapter.pending?.(); value.then(commit, fail); }
      else commit(value);
    } catch { fail(); }
    return true;
  }
  if (action !== "sendto" || typeof adapter.sendTo !== "function") return false;
  adapter.sendTo({
    text:stripAssetRefs(capture.text || ""), exceptId:capture.shared ? "" : (capture.exceptId || ""),
    title:capture.shared ? "Send to…" : "Send to chat…", role:capture.role || "",
    ...(capture.shared ? {who:capture.who, prov:capture.prov} : (capture.prov !== undefined ? {prov:capture.prov} : {})),
    source:capture.source, ...(capture.shared ? {allowNew:false} : {}),
  });
  return true;
}
export function renderBubbleTurn({ turnClass, turnAttributes, bubbleAttributes, markers, speakerName = "", speakerAttributes = "", text = "", formatText, suffix = "", indented = false }) {
  const turn = `<div class="${turnClass}" ${turnAttributes}>`;
  const speaker = speakerName ? `<div class="gwho"${speakerAttributes ? " " + speakerAttributes : ""}>${esc(speakerName)}</div>` : "";
  const body = formatText(text) + suffix;
  const bubble = `<div class="bubble" ${bubbleAttributes}>${markers}${speaker}${body}</div>`;
  return indented ? `\n      ${turn}\n\t\t${bubble}\n      </div>` : `${turn}${bubble}</div>`;
}

export function renderActionRow({doc, turn, html, shared = false}) {
  if (typeof turn.insertAdjacentHTML === "function") {
    turn.insertAdjacentHTML("afterend", `<div class="bubactions"${shared ? " data-gactions" : ""}>${html}</div>`);
    return turn.nextElementSibling;
  }
  const row = doc?.createElement?.("div");
  if (!row) return null;
  row.className = "bubactions";
  if (shared) row.dataset.gactions = "";
  row.innerHTML = html;
  if (typeof turn.after === "function") turn.after(row);
  else if (turn.parentNode) turn.parentNode.insertBefore(row, turn.nextSibling);
  return row;
}

export function createBubbleActionController() {
  let selected = null;
  let action = {};
  const roots = new Map();
  let visit = {key:"", generation:0};
  function clear() {
    action = {};
    const previous = selected;
    selected = null;
    if (previous) previous.close();
  }
  const controller = {
    current: () => selected,
    clear,
    beginAction() { action = {}; return action; },
    cancelAction() { action = {}; },
    isCurrentAction: token => token === action,
    chooserOpened(owner) { if (owner !== controller) action = {}; },
    sheetClosed(owner) { if (owner !== controller) action = {}; },
    selectVisit(key) {
      clear();
      visit = {key, generation:visit.generation + 1};
      return visit.generation;
    },
    captureVisit: () => visit,
    isCurrentVisit: token => token === visit,
    captureViewport(body, {anchor = null, slack = 80} = {}) {
      return {body, anchor, top:anchor?.getBoundingClientRect?.().top,
        nearEnd:body.scrollHeight - body.scrollTop - body.clientHeight <= slack};
    },
    restoreViewport(snapshot, {anchor = null, follow = false} = {}) {
      if (anchor) {
        if (typeof snapshot.top === "number") snapshot.body.scrollTop += anchor.getBoundingClientRect().top - snapshot.top;
      } else if (follow && snapshot.nearEnd) snapshot.body.scrollTop = snapshot.body.scrollHeight;
    },
    listen(root, adapter) {
      let entry = roots.get(root);
      if (!entry) {
        const adapters = new Set();
        const click = event => { for (const item of adapters) item.click(event); };
        const keydown = event => { for (const item of adapters) item.keydown(event); };
        entry = {adapters, click, keydown};
        roots.set(root, entry);
        root.addEventListener("click", click);
        root.addEventListener("keydown", keydown);
      }
      entry.adapters.add(adapter);
      return () => {
        entry.adapters.delete(adapter);
        if (!entry.adapters.size) {
          root.removeEventListener("click", entry.click);
          root.removeEventListener("keydown", entry.keydown);
          roots.delete(root);
        }
      };
    },
    toggle(next) {
      const same = selected && selected.key === next.key && selected.target === next.target;
      clear();
      if (same) return false;
      selected = next;
      return true;
    },
  };
  return controller;
}

export function activateBubble({ target, doc, win, keyboard = false, activate }) {
  if (!target || typeof target.closest !== "function") return false;
  if (target.closest("a, button") || target.closest("a") || target.closest("button")) return false;
  if (!keyboard) {
    const selection = doc && typeof doc.getSelection === "function"
      ? doc.getSelection()
      : (win && typeof win.getSelection === "function" ? win.getSelection() : null);
    if (selection && !selection.isCollapsed && typeof selection.toString === "function" && selection.toString().trim()) return false;
  }
  activate();
  return true;
}

export function routeBubbleClick({target, actionSelector, findTurn, onAction, onTurn, doc, win}) {
  if (!target || typeof target.closest !== "function") return false;
  const action = target.closest(actionSelector);
  if (action) { onAction(action); return true; }
  const turn = findTurn(target);
  if (!turn) return false;
  return activateBubble({target, doc, win, activate:() => onTurn(turn)});
}

export function activateBubbleKey(event, {doc, win, activate}) {
  if (event.key !== "Enter" && event.key !== " ") return false;
  if (!event.target || typeof event.target.closest !== "function") return false;
  const bubble = event.target.closest(".bubble");
  if (bubble !== event.target) return false;
  const activated = activateBubble({target:bubble, doc, win, keyboard:true, activate:() => activate(bubble)});
  if (activated) event.preventDefault();
  return activated;
}
