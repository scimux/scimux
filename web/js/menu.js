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
 *   top  = anchor.bottom - panel.top + 4
 *   left = max(8, min(anchor.left - panel.left - offset, panel.width - 190))
 * offset is 150 for the section menu and 40 for the delete confirm — do not
 * bake either in; a wrong default silently moves the other popover 110px.
 */
export function menuPlacement(anchorRect, panelRect, { offset = 0 } = {}){
  const r = anchorRect || {};
  const pr = panelRect || {};
  const off = Number(offset) || 0;
  const top = (Number(r.bottom) || 0) - (Number(pr.top) || 0) + 4;
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
 * an exclude selector (notes passes "[data-secmenu]"). No Escape handling,
 * no focus management — those are out of scope for this pure refactor.
 */
export function createPopoverMenu(doc){
  let menuEl = null;

  function close(){
    if (menuEl){
      menuEl.remove();
      menuEl = null;
    }
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
   */
  function open({
    panel,
    anchor,
    offset = 0,
    className = "popmenu",
    html = "",
    onClick,
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
    const pos = menuPlacement(r, pr, { offset });
    m.style.top = pos.top + "px";
    m.style.left = pos.left + "px";
    menuEl = m;
    if (typeof onClick === "function") m.addEventListener("click", onClick);
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
