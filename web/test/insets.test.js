/* Top-displacement compensation after a rotation round-trip (insets.js).
 *
 * Measured on an iPhone (375x667, Home Screen web app, status-bar-style
 * "default") with a temporary on-device readout:
 *
 *   healthy portrait  inner 375x647  client647  visual top0   bar top0
 *   landscape         inner 667x375  client375  visual top-20 bar top20
 *   broken portrait   inner 375x647  client667  visual top20  bar top-20
 *
 * The window is inset correctly in all three; what goes stale on the way back
 * is the layout viewport, which keeps the landscape height (667). Fixed
 * elements are pinned to that stale box, so they render 20px — the status bar
 * height — above the visible area. clientHeight - innerHeight is exactly that
 * displacement, and zero whenever the viewport is sane.
 *
 * Fakes only; no real DOM, timers, or matchMedia. */
import test from "node:test";
import assert from "node:assert/strict";
import {
  MAX_TOP_DISPLACEMENT,
  INSET_REFRESH_DELAY_MS,
  topDisplacement,
  installInsetRefresh,
} from "../js/insets.js";
import { readFileSync } from "node:fs";

/* ---------- fakes ---------- */

function fakeWin({ innerHeight = 647, mql = null, vv = null } = {}){
  const listeners = new Map();
  return {
    innerHeight,
    visualViewport: vv,
    matchMedia: mql ? () => mql : undefined,
    listeners,
    addEventListener(ev, fn){
      if (!listeners.has(ev)) listeners.set(ev, new Set());
      listeners.get(ev).add(fn);
    },
    removeEventListener(ev, fn){ listeners.get(ev)?.delete(fn); },
    fire(ev){ for (const fn of [...(listeners.get(ev) || [])]) fn({ type: ev }); },
    count(ev){ return listeners.get(ev)?.size || 0; },
  };
}

function fakeDoc(clientHeight = 647){
  return { documentElement: { clientHeight, scrollHeight: clientHeight } };
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

function fakeVV(){
  const listeners = new Map();
  return {
    width: 375, height: 647, offsetTop: 0, pageTop: 0, scale: 1,
    addEventListener(ev, fn){
      if (!listeners.has(ev)) listeners.set(ev, new Set());
      listeners.get(ev).add(fn);
    },
    removeEventListener(ev, fn){ listeners.get(ev)?.delete(fn); },
    fire(ev){ for (const fn of [...(listeners.get(ev) || [])]) fn({ type: ev }); },
    count(){ return [...listeners.values()].reduce((n, s) => n + s.size, 0); },
  };
}

function scheduler(){
  const queue = [];
  return {
    queue,
    raf(fn){ queue.push({ kind: "raf", fn }); return queue.length; },
    timer(fn, ms){ queue.push({ kind: "timer", ms, fn }); return queue.length; },
    flush(){ while (queue.length) queue.shift().fn(); },
    kinds(){ return queue.map(q => q.kind); },
  };
}

function harness({ innerHeight = 647, clientHeight = 647, mql, vv } = {}){
  const win = fakeWin({ innerHeight, mql, vv });
  const doc = fakeDoc(clientHeight);
  const s = scheduler();
  const applied = [];
  const cleanup = installInsetRefresh({
    win, doc,
    apply: px => applied.push(px),
    raf: s.raf,
    setTimeout: s.timer,
  });
  return { win, doc, s, applied, cleanup };
}

/* ---------- topDisplacement ---------- */

test("topDisplacement reads the stale layout viewport as the status bar height", () => {
  assert.equal(topDisplacement({ innerHeight: 647, clientHeight: 667 }), 20);
});

test("topDisplacement is zero whenever the viewport is sane", () => {
  assert.equal(topDisplacement({ innerHeight: 647, clientHeight: 647 }), 0, "healthy portrait");
  assert.equal(topDisplacement({ innerHeight: 375, clientHeight: 375 }), 0, "landscape");
  assert.equal(topDisplacement({ innerHeight: 852, clientHeight: 852 }), 0, "notched portrait");
});

test("topDisplacement never compensates a negative or absurd delta", () => {
  assert.equal(topDisplacement({ innerHeight: 700, clientHeight: 647 }), 0,
    "client smaller than window (desktop scrollbars) is not a displacement");
  assert.equal(topDisplacement({ innerHeight: 100, clientHeight: 900 }),
    MAX_TOP_DISPLACEMENT, "a wild delta is clamped, never a huge blank band");
  assert.equal(topDisplacement({}), 0);
  assert.equal(topDisplacement(), 0);
});

test("topDisplacement rounds sub-pixel viewport heights", () => {
  assert.equal(topDisplacement({ innerHeight: 646.5, clientHeight: 667 }), 21);
});

/* ---------- installInsetRefresh ---------- */

test("the displacement is measured once at boot, before any rotation", () => {
  const { applied } = harness({ innerHeight: 647, clientHeight: 667 });
  assert.deepEqual(applied, [20], "a page loaded into the broken state self-corrects");
});

test("a rotation re-measures on the next frame and once settled", () => {
  const { win, doc, s, applied } = harness();
  assert.deepEqual(applied, [0]);
  win.fire("orientationchange");
  assert.deepEqual(s.kinds(), ["raf", "timer"]);
  assert.equal(s.queue[1].ms, INSET_REFRESH_DELAY_MS);
  doc.documentElement.clientHeight = 667;   // iOS leaves the stale box behind
  s.flush();
  assert.deepEqual(applied, [0, 20, 20]);
});

test("an unchanged measurement is still applied idempotently, not skipped", () => {
  const { win, s, applied } = harness();
  win.fire("orientationchange");
  s.flush();
  assert.deepEqual(applied, [0, 0, 0]);
});

test("repeat rotation events while passes are pending do not stack", () => {
  const { win, s } = harness();
  win.fire("orientationchange");
  win.fire("orientationchange");
  win.fire("orientationchange");
  assert.deepEqual(s.kinds(), ["raf", "timer"]);
});

test("the orientation media query and visualViewport also re-measure", () => {
  const mql = fakeMQL(), vv = fakeVV();
  const { doc, s, applied } = harness({ mql, vv });
  doc.documentElement.clientHeight = 667;
  mql.fire();
  s.flush();
  assert.deepEqual(applied.slice(1), [20, 20]);
  vv.fire("resize");
  s.flush();
  assert.equal(applied.length, 5);
});

test("every listener is released on cleanup", () => {
  const mql = fakeMQL(), vv = fakeVV();
  const { win, mql: _m, cleanup } = { ...harness({ mql, vv }), mql };
  assert.equal(win.count("orientationchange"), 1);
  assert.equal(mql.count(), 1);
  assert.equal(vv.count(), 2, "visualViewport resize + scroll");
  cleanup();
  assert.equal(win.count("orientationchange"), 0);
  assert.equal(mql.count(), 0);
  assert.equal(vv.count(), 0);
});

test("installInsetRefresh degrades on a bare window", () => {
  const cleanup = installInsetRefresh({ win: {}, doc: fakeDoc() });
  assert.equal(typeof cleanup, "function");
  cleanup();
});

/* ---------- the CSS contract the fix depends on ---------- */

test("the shell offsets the top by --sat, not by env() alone", () => {
  const tokens = readFileSync(new URL("../css/tokens.css", import.meta.url), "utf8");
  assert.match(tokens, /--vvtop:\s*0px/,
    "the JS-measured displacement needs a declared default");
  assert.match(tokens, /--sat:\s*calc\(env\(safe-area-inset-top\)\s*\+\s*var\(--vvtop[^)]*\)\)/,
    "--sat must fold the OS inset and the measured displacement together");
  for (const file of ["../css/layout.css", "../css/notes.css"]) {
    const css = readFileSync(new URL(file, import.meta.url), "utf8");
    assert.doesNotMatch(css, /env\(safe-area-inset-top\)/,
      `${file} must offset the top with var(--sat) so compensation reaches it`);
    assert.match(css, /var\(--sat\)/, `${file} must use var(--sat)`);
  }
});

test("base.css keeps the document unscrollable on both axes", () => {
  const css = readFileSync(new URL("../css/base.css", import.meta.url), "utf8");
  const rule = css.match(/html,\s*body\s*\{[^}]*\}/);
  assert.ok(rule, "html, body rule not found");
  assert.match(rule[0], /overflow:\s*clip/);
});
