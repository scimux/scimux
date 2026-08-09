/* Caret placement for editor entry. Pure decision + DOM apply.
 *
 * Entering an editor on existing content places the caret at the END and
 * selects nothing. A bare focus() is equally wrong: in WebKit it leaves the
 * caret at position 0. Select-all is reserved for the one HIG case
 * (seedForkTitle — a machine-generated guess the user replaces wholesale).
 *
 * Host access: read document/Selection off the element's ownerDocument (or
 * accept them via the element). Never reach for a module-level `document` —
 * the suite is headless and every other web/js module takes its host through
 * deps.
 */

/** Pure collapsed range at `length`. */
export function caretTarget(length){
  const n = Math.max(0, Number(length) || 0);
  return { start: n, end: n };
}

/**
 * Focus `el` and place a collapsed caret at the end of its content.
 * - input / textarea: setSelectionRange(len, len)
 * - contenteditable: collapsed Range on the last text node (or node contents)
 * Never calls select(). No-ops on a missing element.
 */
export function focusAtEnd(el){
  if (!el) return;
  if (typeof el.focus === "function") el.focus();

  /* Value-backed controls (input, textarea, and fakes that expose value +
     setSelectionRange). Prefer this path when available. */
  if (typeof el.setSelectionRange === "function" && typeof el.value === "string"){
    const { start, end } = caretTarget(el.value.length);
    try { el.setSelectionRange(start, end); } catch { /* ignore non-text inputs */ }
    return;
  }

  /* Contenteditable / freeform hosts (the composer #prompt). */
  const doc = el.ownerDocument;
  if (!doc || typeof doc.createRange !== "function") return;

  const range = doc.createRange();
  const lastText = lastTextNode(el);
  if (lastText){
    const len = (lastText.textContent || "").length;
    range.setStart(lastText, len);
    range.setEnd(lastText, len);
  } else if (typeof range.selectNodeContents === "function"){
    range.selectNodeContents(el);
    if (typeof range.collapse === "function") range.collapse(false);
  } else {
    return;
  }

  const sel = typeof doc.getSelection === "function"
    ? doc.getSelection()
    : (doc.defaultView && typeof doc.defaultView.getSelection === "function"
      ? doc.defaultView.getSelection()
      : null);
  if (!sel) return;
  if (typeof sel.removeAllRanges === "function") sel.removeAllRanges();
  if (typeof sel.addRange === "function") sel.addRange(range);
}

/** Deepest last text node under `root`, or null. */
function lastTextNode(root){
  if (!root) return null;
  if (root.nodeType === 3) return root; /* already a text node */
  let node = root;
  /* Prefer lastChild walk when the tree is real. */
  while (node && node.lastChild) node = node.lastChild;
  if (node && node !== root && node.nodeType === 3) return node;
  /* Fallback: childNodes array (headless fakes often only have this). */
  const kids = root.childNodes;
  if (kids && kids.length){
    for (let i = kids.length - 1; i >= 0; i--){
      const found = lastTextNode(kids[i]);
      if (found) return found;
    }
  }
  return null;
}
