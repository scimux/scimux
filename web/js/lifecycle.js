"use strict";
/* Instance lifecycle for the composition root: one app per window, and a
 * ledger that can take it back down.
 *
 * Why this is not module state. A remote boot (FR-40) fetches every module
 * over the tunnel, verifies its digest and re-mints it as a fresh `blob:` URL,
 * so the module graph is new on every handover: the successor's app.js runs in
 * a registry that has never seen its predecessor. A phone that sleeps takes
 * the peer connection with it, rv re-dials on wake and hands over again, and
 * `bootstrap.js` replaces head/body and calls `createApp()` a second time.
 *
 * What that swap does *not* take with it is everything not hanging off a
 * discarded element: listeners on `document`, `window`, a `MediaQueryList` or
 * `visualViewport`, and every timer. So a second boot used to leave the first
 * instance running — polling on its own 2s clock, and painting the status bar
 * through `document.querySelector`, which resolves against whichever document
 * is current. Its transport was the dead one (FR-41: after `cause` is set every
 * request is refused for good), so it reported "server unreachable" over a
 * connection the live instance was demonstrably using, and each sleep/wake
 * added another one.
 *
 * The one identity the two instances share is the window. Hence the slot lives
 * there and the *successor* retires its predecessor: an instance cannot be
 * asked to notice its own replacement, because nothing tells it. Retirement is
 * deliberately tolerant — a broken predecessor must not abort the boot that
 * replaces it, which would leave the page with no live instance at all.
 */

/* Protocol between two module graphs that never meet: both halves must name
   the same window property, so it is a constant here and read nowhere else. */
export const APP_SLOT = "__scimuxApp";

/**
 * Install `handle` as the window's one app instance, retiring whatever held
 * the slot before. Returns the predecessor (or null), so a caller can tell a
 * first boot from a handover.
 *
 * Must be called before the new instance registers anything, and synchronously
 * — `bootstrap.js` does not await `createApp`, so an instance that claimed the
 * slot after its first await would have already built half of itself.
 */
export function claimAppSlot(host, handle) {
  if (!host) return null;
  const prev = host[APP_SLOT] || null;
  host[APP_SLOT] = handle;
  if (prev && prev !== handle && typeof prev.retire === "function") {
    try {
      prev.retire();
    } catch (err) {
      globalThis.console?.debug?.("scimux: retiring the previous app instance failed", err);
    }
  }
  /* Retirement runs arbitrary feature teardown; if any of it touched the slot,
     the claim still wins — there is exactly one live instance and this is it. */
  host[APP_SLOT] = handle;
  return prev;
}

/**
 * A LIFO ledger of everything one instance owns.
 *
 * `add` takes a disposer, `own` adopts anything with a `destroy()` (every
 * feature factory already has one) and passes it straight back so a
 * construction can be wrapped in place, and `on` registers a listener
 * together with its own removal. `run` is the instance's `retire`.
 *
 * Order is last-in-first-out because a later registration may depend on an
 * earlier one. Each disposer is isolated: one broken feature must not strand
 * the rest of the teardown, which is the whole point of retiring at all.
 * Registering after `run` disposes immediately, so a construction that lands
 * after retirement (an awaited factory, a late callback) cannot leak.
 */
export function createTeardown() {
  const disposers = [];
  let ran = false;

  function dispose(fn) {
    try {
      fn();
    } catch (err) {
      globalThis.console?.debug?.("scimux: teardown step failed", err);
    }
  }

  function add(fn) {
    if (typeof fn !== "function") return fn;
    if (ran) dispose(fn);
    else disposers.push(fn);
    return fn;
  }

  function own(obj) {
    if (obj && typeof obj.destroy === "function") add(() => obj.destroy());
    return obj;
  }

  function on(target, type, handler, opts) {
    if (target && typeof target.addEventListener === "function") {
      target.addEventListener(type, handler, opts);
      add(() => target.removeEventListener(type, handler, opts));
    }
    return handler;
  }

  function run() {
    ran = true;
    while (disposers.length) dispose(disposers.pop());
  }

  return { add, own, on, run };
}

/**
 * A set of live timer ids over an injected scheduler, so retirement can stop
 * a cadence nobody kept a handle to.
 *
 * `once: true` is the timeout flavour: the book forgets a timer as it fires,
 * because a session that runs for hours schedules one per poll tick and a book
 * that only ever grew would be its own leak. `once: false` is the interval
 * flavour, live until cleared. Ids come from the host untouched — callers still
 * need them for `unref` and for their own `clear`.
 */
export function createTimerBook({ set, clear, once = false } = {}) {
  const setImpl = typeof set === "function" ? set : () => 0;
  const clearImpl = typeof clear === "function" ? clear : () => {};
  const live = new Set();

  function schedule(fn, ms, ...rest) {
    /* The cell is the identity, not the id: a host may recycle ids, and a
       scheduler that fires synchronously would run the callback before there
       is an id to record. */
    const cell = { id: undefined, fired: false };
    const body = once
      ? (...args) => {
          cell.fired = true;
          live.delete(cell);
          if (typeof fn === "function") fn(...args);
        }
      : fn;
    cell.id = setImpl(body, ms, ...rest);
    if (!cell.fired) live.add(cell);
    return cell.id;
  }

  function forget(id) {
    for (const cell of live) {
      if (cell.id === id) {
        live.delete(cell);
        break;
      }
    }
  }

  function drop(id) {
    forget(id);
    return clearImpl(id);
  }

  function clearAll() {
    const cells = [...live];
    live.clear();
    for (const cell of cells) {
      try {
        clearImpl(cell.id);
      } catch (err) {
        globalThis.console?.debug?.("scimux: clearing a timer failed", err);
      }
    }
  }

  return { set: schedule, clear: drop, clearAll };
}
