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
