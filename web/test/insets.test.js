/* Safe-area inset refresh after rotation (insets.js).
 *
 * WebKit keeps the previous orientation's env(safe-area-inset-*) on fixed
 * elements after a portrait→landscape→portrait round-trip, which drops the
 * status bar under the iOS clock. These tests pin the mechanical contract of
 * the workaround: which elements get re-laid-out, that their inline display is
 * restored exactly, and that a rotation schedules passes without stacking.
 *
 * Fake window/elements only; no real DOM, timers, or matchMedia. */
import test from "node:test";
import assert from "node:assert/strict";
import {
  INSET_TARGETS,
  INSET_REFRESH_DELAY_MS,
  nudgeInsets,
  installInsetRefresh,
} from "../js/insets.js";

/* ---------- fakes ---------- */

function fakeEl(display = ""){
  return { style: { display }, hidden: false };
}

function fakeWin({ mql = null } = {}){
  const listeners = new Map();
  return {
    listeners,
    addEventListener(ev, fn){
      if (!listeners.has(ev)) listeners.set(ev, new Set());
      listeners.get(ev).add(fn);
    },
    removeEventListener(ev, fn){
      listeners.get(ev)?.delete(fn);
    },
    matchMedia: mql ? () => mql : undefined,
    fire(ev){
      for (const fn of [...(listeners.get(ev) || [])]) fn({ type: ev });
    },
    count(ev){
      return listeners.get(ev)?.size || 0;
    },
  };
}

function fakeMQL(){
  const listeners = new Set();
  return {
    matches: true,
    addEventListener(ev, fn){ if (ev === "change") listeners.add(fn); },
    removeEventListener(ev, fn){ if (ev === "change") listeners.delete(fn); },
    fire(){ for (const fn of [...listeners]) fn({ matches: true }); },
    count(){ return listeners.size; },
  };
}

/* Deferred scheduler: nothing runs until flush() is called, so a test can
   observe how many passes a single rotation queued. */
function scheduler(){
  const queue = [];
  return {
    queue,
    raf(fn){ queue.push({ kind: "raf", fn }); return queue.length; },
    timer(fn, ms){ queue.push({ kind: "timer", ms, fn }); return queue.length; },
    flush(){
      while (queue.length) queue.shift().fn();
    },
    kinds(){ return queue.map(q => q.kind); },
  };
}

function harness({ els, mql } = {}){
  const elements = els || [fakeEl(), fakeEl()];
  const s = scheduler();
  const reads = [];
  const win = fakeWin({ mql });
  const cleanup = installInsetRefresh({
    win,
    query: () => elements,
    read: () => reads.push(elements.map(e => e.style.display)),
    raf: s.raf,
    setTimeout: s.timer,
  });
  return { elements, s, reads, win, cleanup };
}

/* ---------- targets ---------- */

test("INSET_TARGETS covers every fixed element positioned off the top inset", () => {
  for (const sel of ["#statusbar", "#app", "#cards", "#map",
                     "#bookmarkspane", "#bookmarkpeek", "#bookmarkflags",
                     "#notesworkspace"]) {
    assert.ok(INSET_TARGETS.includes(sel), `${sel} missing from INSET_TARGETS`);
  }
});

/* ---------- nudgeInsets ---------- */

test("nudgeInsets hides every element, forces one read, restores inline display", () => {
  const a = fakeEl("");        // no inline display
  const b = fakeEl("flex");    // inline display set by feature code
  let seen = null;
  const n = nudgeInsets([a, b], () => { seen = [a.style.display, b.style.display]; });
  assert.equal(n, 2);
  assert.deepEqual(seen, ["none", "none"], "layout is read while hidden");
  assert.equal(a.style.display, "");
  assert.equal(b.style.display, "flex", "pre-existing inline display is restored");
});

test("nudgeInsets tolerates missing elements and a missing reader", () => {
  const a = fakeEl();
  assert.equal(nudgeInsets([null, undefined, {}, a]), 1);
  assert.equal(a.style.display, "");
});

/* ---------- installInsetRefresh ---------- */

test("installInsetRefresh listens for rotation and unbinds on cleanup", () => {
  const mql = fakeMQL();
  const { win, cleanup } = harness({ mql });
  assert.equal(win.count("orientationchange"), 1);
  assert.equal(mql.count(), 1, "orientation media query is a second, non-deprecated signal");
  cleanup();
  assert.equal(win.count("orientationchange"), 0);
  assert.equal(mql.count(), 0);
});

test("a rotation schedules a frame pass and a settled pass", () => {
  const { s, reads, win } = harness();
  win.fire("orientationchange");
  assert.deepEqual(s.kinds(), ["raf", "timer"]);
  assert.equal(s.queue[1].ms, INSET_REFRESH_DELAY_MS);
  s.flush();
  assert.equal(reads.length, 2, "both passes re-lay-out the targets");
  assert.deepEqual(reads[0], ["none", "none"]);
});

test("repeat rotation events while passes are pending do not stack", () => {
  const { s, win } = harness();
  win.fire("orientationchange");
  win.fire("orientationchange");
  win.fire("orientationchange");
  assert.deepEqual(s.kinds(), ["raf", "timer"]);
});

test("a rotation after the passes ran schedules a fresh pair", () => {
  const { s, reads, win } = harness();
  win.fire("orientationchange");
  s.flush();
  win.fire("orientationchange");
  s.flush();
  assert.equal(reads.length, 4);
});

test("the orientation media query triggers the same refresh", () => {
  const mql = fakeMQL();
  const { s, reads } = harness({ mql });
  mql.fire();
  s.flush();
  assert.equal(reads.length, 2);
});

test("installInsetRefresh degrades on a window without matchMedia", () => {
  const { s, reads, win, cleanup } = harness({ mql: null });
  win.fire("orientationchange");
  s.flush();
  assert.equal(reads.length, 2);
  cleanup();
});
