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

/* Whether a wall-map selection should remember the map at all.

   A return control only earns its place when the origin actually left the
   screen. Under the dock (body.map-full.map-dock) the map keeps the top half,
   so "Return to Map" would point at what the user is already looking at — and
   because #chatback.hasreturn outspecifies .backbtn's desktop display:none, it
   re-appears there rather than staying hidden. A dock open is not a jump; the
   user never went anywhere. */
export function mapJumpReturnKind({ how, docked } = {}){
  return how === "jump" && !docked ? RETURN_MAP : null;
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
   as the ordinary one-level-back.

   A return control reads "Return to <origin>", not just the origin's name: sat
   beside the chat title, the bare "‹ Map" was too quiet to find (review 2,
   item 3), and the shell frames it as a pill. A note's origin is a name the
   user gave it, so it is quoted; Map and Search are places in the UI and are
   not. Without a context the control stays the plain parent-pane back button. */
export function chatBackState(ctx){
  const label = returnLabel(ctx);
  const hasReturn = !!(ctx && ctx.kind);
  if (!hasReturn){
    return { hasReturn, label, html: "&#8249; " + esc(label), ariaLabel: "back to " + label };
  }
  const named = ctx.kind === RETURN_NOTE && label !== GENERIC_LABEL[RETURN_NOTE];
  const shown = named ? "&#8220;" + esc(label) + "&#8221;" : esc(label);
  return {
    hasReturn,
    label,
    html: "&#8249; Return to " + shown,
    ariaLabel: "return to " + label,
  };
}

/* What the *inside* of a presenting surface is called. The chat preview is drawn
   in the search overlay's own panel — same width, same offset, no entry
   animation — so a tap does not read as "a dialog opened" but as "the results
   became a chat". One level up from there is not "Search" as a whole, it is the
   list of results, and that is the shortest true name for where the dismissal
   lands. */
const MODAL_LABEL = {
  [RETURN_MAP]: "Map",
  [RETURN_SEARCH]: "Results",
  [RETURN_NOTE]: "Note",
};

/* The same pill on a *pushed modal*, with two deliberate differences from
   #chatback.

   It drops the verb. HIG's plain rule — title the back button with the previous
   screen — works here because the trip is one level inside one panel; #chatback
   needs "Return to Search" only because it crosses screens and sits beside the
   chat's own title, where a bare word went unnoticed (review 2, item 3).

   And it has no parent-pane fallback. A chat always has "Activities" above it;
   a modal with no presenter to name has nothing above it but the surface it was
   dropped onto, and offering to "go back" there would send the user somewhere
   they never came from. So the control does not exist rather than lying. */
export function returnPillState(ctx){
  if (!ctx || !ctx.kind) return { hasReturn: false, label: "", html: "", ariaLabel: "" };
  /* Never the note's own title: the modal shows a chat, and two document names
     side by side in one head make the user work out which is which. */
  const label = MODAL_LABEL[ctx.kind] || DEFAULT_BACK_LABEL;
  return {
    hasReturn: true,
    label,
    html: "&#8249; " + esc(label),
    ariaLabel: "back to " + label.toLowerCase(),
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
