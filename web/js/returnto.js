/* returnto.js — the contextual return control (UI review item 18).
 *
 * A jump moves the user *sideways*: from the wall map, the search overlay, or
 * a note section straight into a chat. The ordinary "< Activities" back button
 * then lies — the chat's parent in the pane hierarchy is not where the user
 * came from, and the trip back was three taps.
 *
 * HIG has exactly one idiom for this: the navigation bar's leading back button
 * labelled with its *destination* (the system "< Messages" pill). So there is
 * no second control — #chatback simply renames itself while a return context
 * is live, and reverts the moment it stops being true.
 *
 * The context is deliberately fragile. It survives a jump and nothing else:
 * picking a different chat from Activities, tapping a sticky-note flag, or
 * starting a new thread all mean the user left that trajectory on purpose, and
 * a stale "< Search" would offer to restore a screen they had already
 * abandoned. Pure decisions only — the shell owns storage and DOM.
 */
import { esc } from "./format.js";

export const RETURN_MAP = "map";
export const RETURN_SEARCH = "search";
export const RETURN_NOTE = "note";

/* Longest note title we will put in the back button. Beyond this the label
   crowds the chat title next to it, so fall back to the generic noun — the
   same trade the system makes when a parent screen's title is too long. */
export const RETURN_LABEL_MAX = 24;

/* What #chatback reads with no return context: the chat's actual parent pane. */
export const DEFAULT_BACK_LABEL = "Activities";

const KINDS = [RETURN_MAP, RETURN_SEARCH, RETURN_NOTE];

const GENERIC_LABEL = {
  [RETURN_MAP]: "Map",
  [RETURN_SEARCH]: "Search",
  [RETURN_NOTE]: "Note",
};

/* makeReturnContext(kind, payload) -> a context, or null for an origin we do
   not know how to restore. Half a context is worse than none: it would render
   a back button that goes nowhere. The payload is whatever that origin needs
   to come back to the same place (map: node + stop; search: query; note: id). */
export function makeReturnContext(kind, payload){
  if (!kind || !KINDS.includes(kind)) return null;
  return { kind, ...(payload || {}) };
}

/* The destination's name, never a generic "Back". */
export function returnLabel(ctx){
  if (!ctx || !ctx.kind) return DEFAULT_BACK_LABEL;
  if (ctx.kind === RETURN_NOTE){
    const title = String(ctx.title || "").trim();
    if (title && title.length <= RETURN_LABEL_MAX) return title;
  }
  return GENERIC_LABEL[ctx.kind] || DEFAULT_BACK_LABEL;
}

/* The single decision behind #chatback: label, markup, assistive name, and
   whether the shell should treat a tap as "restore the origin" (hasReturn) or
   as the ordinary one-level-back. */
export function chatBackState(ctx){
  const label = returnLabel(ctx);
  return {
    hasReturn: !!(ctx && ctx.kind),
    label,
    html: "&#8249; " + esc(label),
    ariaLabel: "back to " + label,
  };
}

/* The invalidation rule. `how` is why the chat became selected: only "jump"
   keeps the context alive. Anything else — a card tap, a sticky-note flag, a
   freshly forked thread — is the user deviating from the trajectory, and the
   return offer goes with it. */
export function returnAfterSelection(ctx, how){
  if (!ctx) return null;
  return how === "jump" ? ctx : null;
}
