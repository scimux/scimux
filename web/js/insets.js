/* Safe-area inset refresh after a rotation round-trip.
 *
 * The whole shell is positioned off the top inset — the status bar grows by
 * env(safe-area-inset-top) and every fixed pane starts below it (layout.css,
 * notes.css). WebKit resolves those env() values against viewport constants it
 * does not re-apply to already-laid-out fixed elements when the phone rotates
 * back: portrait → landscape (top inset 0) → portrait leaves the bar 44px tall
 * with no top padding, i.e. drawn underneath the iOS clock/battery. Rotations
 * that don't change the top inset (portrait ↔ upside-down, landscape ↔
 * landscape) never show it.
 *
 * The fix is mechanical and layout-only: hide the affected elements, force one
 * layout read while they are out of the box tree, put them back. WebKit then
 * re-resolves env() with the current constants. Hiding and restoring happen in
 * the same task with a single read for the whole batch, so there is no frame
 * where the user sees a gap and no transition is retriggered.
 *
 * Explicit inputs only — window, element lookup, layout read, and the two
 * schedulers are injected, so the whole thing is exercised by Node tests. */

/* Every fixed element whose top edge is calc(var(--sbh) + env(safe-area-inset-top))
   or otherwise offset by the inset. Keep in sync with layout.css / notes.css. */
export const INSET_TARGETS = [
  "#statusbar",
  "#app",
  "#cards",
  "#map",
  "#bookmarkspane",
  "#bookmarkpeek",
  "#bookmarkflags",
  "#notesworkspace",
];

/* iOS reports the rotation before the inset settles: the frame pass catches the
   common case, this one catches the tail (rotation lock prompts, slow devices). */
export const INSET_REFRESH_DELAY_MS = 400;

/* Hide → read → restore. Returns how many elements were nudged. Inline display
   is saved and restored per element, so features that set it themselves
   (#bookmarkflags, #notesworkspace) keep whatever they had. */
export function nudgeInsets(els, read){
  const prior = [];
  for (const el of els || []){
    if (!el || !el.style) continue;
    prior.push([el, el.style.display]);
    el.style.display = "none";
  }
  if (prior.length && typeof read === "function") read();
  for (const [el, display] of prior) el.style.display = display;
  return prior.length;
}

/* Binds the rotation signals and returns a cleanup function.
   `orientationchange` is deprecated but is what iOS Safari reliably fires; the
   orientation media query is the standard signal. Both funnel into one pending
   guard so a rotation schedules exactly one frame pass and one settled pass. */
export function installInsetRefresh({
  win,
  query,
  read,
  raf,
  setTimeout: setTimer,
} = {}){
  if (!win || typeof win.addEventListener !== "function") return () => {};
  const frame = typeof raf === "function" ? raf : fn => fn();
  const timer = typeof setTimer === "function" ? setTimer : fn => fn();
  const targets = typeof query === "function" ? query : () => [];

  let pending = false;
  const pass = () => nudgeInsets(targets(), read);
  const schedule = () => {
    if (pending) return;
    pending = true;
    frame(() => { pending = false; pass(); });
    timer(pass, INSET_REFRESH_DELAY_MS);
  };

  win.addEventListener("orientationchange", schedule);
  const cleanups = [() => win.removeEventListener("orientationchange", schedule)];

  const mql = typeof win.matchMedia === "function"
    ? win.matchMedia("(orientation: portrait)") : null;
  if (mql && typeof mql.addEventListener === "function"){
    mql.addEventListener("change", schedule);
    cleanups.push(() => mql.removeEventListener("change", schedule));
  }

  return () => { for (const fn of cleanups) fn(); };
}
