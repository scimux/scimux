/* Top-displacement compensation after a rotation round-trip.
 *
 * The whole shell hangs off the top inset: the status bar grows by it and every
 * fixed pane starts below it (layout.css, notes.css, via var(--sat)).
 *
 * On iOS, rotating portrait → landscape → portrait leaves the *layout viewport*
 * at the landscape height while the window is resized back correctly. Fixed
 * elements are pinned to that stale box, so they render above the visible area
 * by exactly the status bar height and disappear under the OS clock. Measured
 * on a 375x667 Home Screen web app (temporary on-device readout):
 *
 *   healthy portrait  inner 375x647  client647  visual top0   bar top0
 *   landscape         inner 667x375  client375  visual top-20 bar top20
 *   broken portrait   inner 375x647  client667  visual top20  bar top-20
 *
 * env(safe-area-inset-top) is 0 in all three — with status-bar-style "default"
 * the OS keeps the web view clear of the status bar by sizing the window, so
 * there is no inset for CSS to read and nothing about env() to re-resolve. The
 * only signal is the disagreement between the two viewports:
 *
 *   displacement = documentElement.clientHeight - window.innerHeight
 *
 * which is 0 whenever the viewport is sane and equals the missing offset when
 * it is not. It is fed back as --vvtop, which --sat adds to the OS inset, so a
 * displaced viewport pushes the shell down by exactly what it lost.
 *
 * Explicit inputs only — window, document, the applier and both schedulers are
 * injected, so the whole thing is exercised by Node tests. The device is still
 * the only place the bug itself reproduces. */

/* A displacement is a status/nav bar height (20-60pt). Anything larger is a
   viewport we do not understand; compensating it would hang a blank band under
   the OS chrome, which is worse than the bug. */
export const MAX_TOP_DISPLACEMENT = 64;

/* iOS reports the rotation before the viewport settles: the frame pass catches
   the common case, this one catches the tail (slow devices, rotation prompts). */
export const INSET_REFRESH_DELAY_MS = 400;

/* How far above the visible area the fixed shell is pinned, in CSS px. */
export function topDisplacement({ innerHeight = 0, clientHeight = 0 } = {}){
  const d = Math.round((clientHeight || 0) - (innerHeight || 0));
  if (!Number.isFinite(d) || d <= 0) return 0;
  return Math.min(d, MAX_TOP_DISPLACEMENT);
}

/* Measures now, on every rotation signal, and whenever the visual viewport
   moves; returns a cleanup function. Measurement is idempotent and cheap (two
   layout reads), so passes always apply rather than diffing — a skipped apply
   after a missed event would leave the shell under the clock. */
export function installInsetRefresh({
  win,
  doc,
  apply,
  raf,
  setTimeout: setTimer,
} = {}){
  const w = win || {};
  const de = (doc && doc.documentElement) || {};
  const frame = typeof raf === "function" ? raf : fn => fn();
  const timer = typeof setTimer === "function" ? setTimer : fn => fn();
  const applyPx = typeof apply === "function" ? apply : () => {};

  const measure = () => applyPx(topDisplacement({
    innerHeight: w.innerHeight,
    clientHeight: de.clientHeight,
  }));

  let pending = false;
  const schedule = () => {
    if (pending) return;
    pending = true;
    frame(() => { pending = false; measure(); });
    timer(measure, INSET_REFRESH_DELAY_MS);
  };

  const cleanups = [];
  if (typeof w.addEventListener === "function"){
    /* orientationchange is deprecated but is what iOS reliably fires; the
       orientation media query is the standard signal. Both funnel into one
       pending guard, so a rotation schedules exactly one pair of passes. */
    w.addEventListener("orientationchange", schedule);
    cleanups.push(() => w.removeEventListener("orientationchange", schedule));

    const mql = typeof w.matchMedia === "function"
      ? w.matchMedia("(orientation: portrait)") : null;
    if (mql && typeof mql.addEventListener === "function"){
      mql.addEventListener("change", schedule);
      cleanups.push(() => mql.removeEventListener("change", schedule));
    }
  }
  const vv = w.visualViewport;
  if (vv && typeof vv.addEventListener === "function"){
    for (const ev of ["resize", "scroll"]){
      vv.addEventListener(ev, schedule);
      cleanups.push(() => vv.removeEventListener(ev, schedule));
    }
  }

  measure();   /* a page loaded straight into the broken state self-corrects */
  return () => { for (const fn of cleanups) fn(); };
}
