/* Tests for web/js/lifecycle.js — the per-window app slot, the teardown
 * ledger and the timer books.
 *
 * Why the module exists: a remote handover fetches, verifies and re-mints
 * every module as a fresh blob: URL, so the successor's app.js runs in a
 * module registry that has never seen the predecessor. Module-scope state
 * therefore cannot retire the instance that came before it, and the discarded
 * document takes only its own element listeners with it — document, window,
 * matchMedia and visualViewport listeners, and every timer, survive. The one
 * identity both instances share is the window, so the slot lives there and
 * whoever claims it retires its predecessor. The two-module-graphs test below
 * is that condition, reproduced.
 */
import test from "node:test";
import assert from "node:assert/strict";
import {
  APP_SLOT,
  claimAppSlot,
  createTeardown,
  createTimerBook,
} from "../js/lifecycle.js";

/* A recording event target: it answers add/removeEventListener and reports
   which handlers are still registered. */
function fakeTarget() {
  const live = [];
  return {
    addEventListener(type, fn, opts) {
      live.push({ type, fn, opts });
    },
    removeEventListener(type, fn) {
      const i = live.findIndex(l => l.type === type && l.fn === fn);
      if (i >= 0) live.splice(i, 1);
    },
    live,
  };
}

/* ---------- createTeardown ---------- */

test("teardown: disposers run last-in-first-out", () => {
  const order = [];
  const t = createTeardown();
  t.add(() => order.push("first"));
  t.add(() => order.push("second"));
  t.add(() => order.push("third"));
  t.run();
  assert.deepEqual(order, ["third", "second", "first"],
    "a later registration may depend on an earlier one, so it must go first");
});

test("teardown: a disposer that throws does not strand the rest", () => {
  const order = [];
  const t = createTeardown();
  t.add(() => order.push("bottom"));
  t.add(() => { throw new Error("feature destroy blew up"); });
  t.add(() => order.push("top"));
  t.run();
  assert.deepEqual(order, ["top", "bottom"],
    "one broken feature must not leave the rest of the instance alive");
});

test("teardown: run is idempotent", () => {
  let calls = 0;
  const t = createTeardown();
  t.add(() => { calls++; });
  t.run();
  t.run();
  assert.equal(calls, 1, "retiring twice must not double-destroy");
});

test("teardown: a registration after run is disposed immediately", () => {
  let disposed = false;
  const t = createTeardown();
  t.run();
  t.add(() => { disposed = true; });
  assert.equal(disposed, true,
    "an async construction that lands after retirement must not leak");
});

test("teardown: own() destroys, returns the object, tolerates no destroy", () => {
  const t = createTeardown();
  let destroyed = 0;
  const feature = { destroy() { destroyed++; }, render() {} };
  assert.equal(t.own(feature), feature, "own is a pass-through so it can wrap a factory call");
  const plain = t.own({ render() {} });
  assert.deepEqual(Object.keys(plain), ["render"]);
  assert.equal(t.own(null), null);
  assert.equal(t.own(undefined), undefined);
  t.run();
  assert.equal(destroyed, 1);
});

test("teardown: on() registers and run() removes the same handler", () => {
  const t = createTeardown();
  const doc = fakeTarget();
  const handler = () => {};
  assert.equal(t.on(doc, "visibilitychange", handler), handler);
  assert.equal(doc.live.length, 1);
  assert.equal(doc.live[0].type, "visibilitychange");
  t.run();
  assert.deepEqual(doc.live, [],
    "a surviving target must not keep a retired instance's handler");
});

test("teardown: on() tolerates a target that cannot take listeners", () => {
  const t = createTeardown();
  const handler = () => {};
  assert.equal(t.on(null, "change", handler), handler);
  assert.equal(t.on({}, "change", handler), handler);
  t.run();
});

/* ---------- claimAppSlot ---------- */

test("claim: the first claim installs and reports no predecessor", () => {
  const host = {};
  const handle = { retire() {} };
  assert.equal(claimAppSlot(host, handle), null);
  assert.equal(host[APP_SLOT], handle);
});

test("claim: the second claim retires the first and hands it back", () => {
  const host = {};
  let retired = 0;
  const first = { retire() { retired++; } };
  const second = { retire() {} };
  claimAppSlot(host, first);
  assert.equal(claimAppSlot(host, second), first);
  assert.equal(retired, 1, "the predecessor is retired by its successor, not by itself");
  assert.equal(host[APP_SLOT], second);
});

test("claim: re-claiming with the same handle does not retire it", () => {
  const host = {};
  let retired = 0;
  const handle = { retire() { retired++; } };
  claimAppSlot(host, handle);
  claimAppSlot(host, handle);
  assert.equal(retired, 0);
  assert.equal(host[APP_SLOT], handle);
});

test("claim: a predecessor whose retire throws is still replaced", () => {
  const host = {};
  const broken = { retire() { throw new Error("stale instance"); } };
  const next = { retire() {} };
  claimAppSlot(host, broken);
  assert.equal(claimAppSlot(host, next), broken);
  assert.equal(host[APP_SLOT], next,
    "a boot must not be abandoned because the instance it replaces is broken");
});

test("claim: a predecessor that reassigns the slot still loses it", () => {
  const host = {};
  const impostor = { retire() {} };
  const rogue = { retire() { host[APP_SLOT] = impostor; } };
  const next = { retire() {} };
  claimAppSlot(host, rogue);
  claimAppSlot(host, next);
  assert.equal(host[APP_SLOT], next);
});

test("claim: a predecessor without retire, and a missing host, are tolerated", () => {
  const host = {};
  claimAppSlot(host, { name: "no retire" });
  const next = { retire() {} };
  assert.deepEqual(claimAppSlot(host, next), { name: "no retire" });
  assert.equal(host[APP_SLOT], next);
  assert.equal(claimAppSlot(null, next), null);
  assert.equal(claimAppSlot(undefined, next), null);
});

test("claim: two module graphs over one window still see one instance", async () => {
  /* The handover condition: the successor's lifecycle.js is a different
     module object at a different blob: URL, sharing nothing but the window.
     A slot kept in module scope would report no predecessor here — which is
     exactly the leak that put two live app instances on one status bar. */
  const twin = await import("../js/lifecycle.js?graph=successor");
  assert.notEqual(twin.claimAppSlot, claimAppSlot, "the twin must be a separate module instance");
  assert.equal(twin.APP_SLOT, APP_SLOT, "both graphs must name the same slot");

  const host = {};
  let retired = 0;
  const first = { retire() { retired++; } };
  const second = { retire() {} };
  claimAppSlot(host, first);
  assert.equal(twin.claimAppSlot(host, second), first,
    "the successor graph must find and retire the predecessor it never imported");
  assert.equal(retired, 1);
  assert.equal(host[APP_SLOT], second);
});

/* ---------- createTimerBook ---------- */

function fakeClock() {
  const pending = new Map();
  let next = 0;
  const cleared = [];
  return {
    pending,
    cleared,
    set(fn, ms, ...rest) {
      const id = ++next;
      pending.set(id, { fn, ms, rest });
      return id;
    },
    clear(id) {
      cleared.push(id);
      pending.delete(id);
    },
    fire(id, ...args) {
      const t = pending.get(id);
      if (!t) throw new Error("fired a timer that is not pending: " + id);
      t.fn(...args);
    },
  };
}

test("timers: set delegates, returns the host id, and forwards its arguments", () => {
  const clock = fakeClock();
  const book = createTimerBook({ set: clock.set, clear: clock.clear, once: true });
  const seen = [];
  const id = book.set((...args) => seen.push(args), 2000, "a", "b");
  assert.equal(id, 1, "the caller gets the host's id, so unref and clear still work");
  assert.equal(clock.pending.get(id).ms, 2000);
  clock.fire(id, "a", "b");
  assert.deepEqual(seen, [["a", "b"]]);
});

test("timers: clearAll clears every live timer exactly once", () => {
  const clock = fakeClock();
  const book = createTimerBook({ set: clock.set, clear: clock.clear, once: false });
  const a = book.set(() => {}, 8000);
  const b = book.set(() => {}, 30000);
  book.clearAll();
  assert.deepEqual(clock.cleared.sort(), [a, b].sort(),
    "retiring an instance must stop every cadence it started");
  book.clearAll();
  assert.equal(clock.cleared.length, 2, "clearAll is idempotent");
});

test("timers: a one-shot that already fired is not tracked any more", () => {
  const clock = fakeClock();
  const book = createTimerBook({ set: clock.set, clear: clock.clear, once: true });
  const id = book.set(() => {}, 300);
  clock.fire(id);
  book.clearAll();
  assert.deepEqual(clock.cleared, [],
    "a long session schedules thousands of timeouts; the book must not grow by one per tick");
});

test("timers: an interval stays live after firing", () => {
  const clock = fakeClock();
  const book = createTimerBook({ set: clock.set, clear: clock.clear, once: false });
  let fired = 0;
  const id = book.set(() => { fired++; }, 8000);
  clock.fire(id);
  clock.fire(id);
  assert.equal(fired, 2, "an interval must keep its callback after a firing");
  book.clearAll();
  assert.deepEqual(clock.cleared, [id]);
});

test("timers: clear forgets the timer so clearAll does not clear it twice", () => {
  const clock = fakeClock();
  const book = createTimerBook({ set: clock.set, clear: clock.clear, once: true });
  const id = book.set(() => {}, 300);
  book.clear(id);
  assert.deepEqual(clock.cleared, [id]);
  book.clearAll();
  assert.deepEqual(clock.cleared, [id]);
});

test("timers: a one-shot whose host fires synchronously is never tracked", () => {
  /* insets.js and the Node harnesses pass immediate schedulers; the book must
     not end up holding a cell for a timer that is already over. */
  const cleared = [];
  const book = createTimerBook({
    set: fn => { fn(); return 7; },
    clear: id => cleared.push(id),
    once: true,
  });
  let ran = 0;
  assert.equal(book.set(() => { ran++; }, 0), 7);
  assert.equal(ran, 1);
  book.clearAll();
  assert.deepEqual(cleared, []);
});

test("timers: missing set/clear are tolerated", () => {
  const book = createTimerBook();
  book.set(() => {}, 100);
  book.clear(1);
  book.clearAll();
});
