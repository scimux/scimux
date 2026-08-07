/* Characterization tests for web/js/navigation.js — pure spatial navigation.
 * Imports the real module (not HTML substring extraction). */
import test from "node:test";
import assert from "node:assert/strict";
import {
  clampLevel,
  scrimStep,
  bookmarkPeekClose,
  captureTouchStart,
  documentSwipeDecision,
  captureNotesTouchStart,
  notesSwipeBackDecision,
  journeyToggleState,
} from "../js/navigation.js";
import { bookmarksToggleState } from "../js/bookmarks.js";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));
const navSrc = readFileSync(join(__dirname, "../js/navigation.js"), "utf8");

/* ---------- purity / no browser globals ---------- */
test("navigation.js has no browser globals or implicit app state", () => {
  const code = navSrc
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/\/\/.*$/gm, "");
  const forbidden = [
    /\bdocument\b/,
    /\bwindow\b/,
    /\bfetch\b/,
    /\blocalStorage\b/,
    /\bnavigator\b/,
    /\bsetTimeout\b/,
    /\bsetInterval\b/,
    /\brequestAnimationFrame\b/,
    /\bmatchMedia\b/,
    /\binnerWidth\b/,
    /\bgetComputedStyle\b/,
    /\bcreateElement\b/,
    /\binnerHTML\s*=/,   // writing the DOM is banned; returning markup as data is not
    /\bwsOpen\s*\(/,
    /\bsearchOpen\s*\(/,
    /\barchivedOpen\s*\(/,
    /\bsetBookmarksOpen\s*\(/,   // action type string is ok; no live calls
    /\bopenWorkspace\s*\(/,
    /\brenderMap\s*\(/,
    /\bmapSig\b/,
    /\bsetLevel\s*\(/,            // action type only; clamp is pure
  ];
  for (const re of forbidden) {
    assert.equal(re.test(code), false, `navigation.js must not reference ${re}`);
  }
  // No live-overlay recheck API
  assert.equal(/\bliveOverlay\b/.test(code), false);
  assert.equal(/\bisOverlayOpen\b/.test(code), false);
  // documentSwipeDecision must not take a callback for overlay
  assert.match(navSrc, /export function documentSwipeDecision\(touch, end, state/);
});

test("navigation.js exports named pure helpers", async () => {
  const mod = await import("../js/navigation.js");
  for (const name of [
    "clampLevel", "scrimStep", "bookmarkPeekClose",
    "captureTouchStart", "documentSwipeDecision",
    "captureNotesTouchStart", "notesSwipeBackDecision",
  ]) {
    assert.ok(name in mod, name);
  }
});

/* ---------- clampLevel ---------- */
test("clampLevel clamps to 1..3", () => {
  assert.equal(clampLevel(1), 1);
  assert.equal(clampLevel(2), 2);
  assert.equal(clampLevel(3), 3);
  assert.equal(clampLevel(0), 1);
  assert.equal(clampLevel(-5), 1);
  assert.equal(clampLevel(4), 3);
  assert.equal(clampLevel(99), 3);
  assert.equal(clampLevel(1.5), 1.5); // no integer coercion — matches Math.max/min only
});

/* ---------- scrim / bookmark peek ---------- */
test("scrimStep is exactly one level back (3→2, 2→1, 1 stays 1)", () => {
  assert.deepEqual(scrimStep(3), { type: "setLevel", level: 2 });
  assert.deepEqual(scrimStep(2), { type: "setLevel", level: 1 });
  assert.deepEqual(scrimStep(1), { type: "setLevel", level: 1 });
  // Invalid overshoot still clamps after the one-step subtraction.
  assert.deepEqual(scrimStep(4), { type: "setLevel", level: 3 });
});

test("scrimStep never jumps to chat from map (no setLevel(1) from 3)", () => {
  assert.notEqual(scrimStep(3).level, 1);
  assert.equal(scrimStep(3).level, 2);
});

test("bookmarkPeekClose closes only Bookmarks", () => {
  assert.deepEqual(bookmarkPeekClose(), { type: "setBookmarksOpen", open: false });
});

/* ---------- captureTouchStart snapshot ---------- */
test("captureTouchStart records edges, localCard, and immutable overlay", () => {
  const t = captureTouchStart({
    x: 10, y: 100, clientWidth: 390, localCard: true, overlayOwned: true,
  });
  assert.equal(t.edge, true);       // 10 < 28
  assert.equal(t.edgeR, false);
  assert.equal(t.localCard, true);
  assert.equal(t.overlayOwned, true);
  assert.equal(t.x, 10);
  assert.equal(t.y, 100);

  const r = captureTouchStart({
    x: 380, y: 50, clientWidth: 390, localCard: false, overlayOwned: false,
  });
  assert.equal(r.edge, false);
  assert.equal(r.edgeR, true);      // 380 > 390-28=362
  assert.equal(r.overlayOwned, false);
  assert.equal(r.localCard, false);
});

test("captureTouchStart freezes overlayOwned (boolean coercion)", () => {
  const t = captureTouchStart({ x: 100, y: 100, clientWidth: 400, overlayOwned: 1 });
  assert.equal(t.overlayOwned, true);
  // mutating the input later cannot affect the snapshot (primitive copy)
  let live = true;
  const snap = captureTouchStart({ x: 0, y: 0, clientWidth: 400, overlayOwned: live });
  live = false;
  assert.equal(snap.overlayOwned, true);
});

/* ---------- documentSwipeDecision: overlay snapshot, no live recheck ---------- */
test("documentSwipeDecision suppresses when snapshot.overlayOwned is true", () => {
  const touch = captureTouchStart({
    x: 50, y: 100, clientWidth: 390, overlayOwned: true,
  });
  // Even if state looks like "overlays closed now", decision must suppress
  const action = documentSwipeDecision(
    touch,
    { x: 200, y: 100 }, // L→R big swipe that would otherwise close bookmarks
    { level: 1, bookmarksOpen: true, isDesktop: false },
  );
  assert.deepEqual(action, { type: "suppress" });
});

test("documentSwipeDecision takes no live-overlay callback and performs no recheck", () => {
  // Prove by arity/source: function body must not call any open-check helpers
  const code = navSrc
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/\/\/.*$/gm, "");
  const fn = code.slice(code.indexOf("export function documentSwipeDecision"));
  const body = fn.slice(0, fn.indexOf("export function notesSwipeBackDecision") > 0
    ? fn.indexOf("export function notesSwipeBackDecision")
    : fn.length);
  assert.equal(/\bwsOpen\s*\(/.test(body), false);
  assert.equal(/\bsearchOpen\s*\(/.test(body), false);
  assert.equal(/\barchivedOpen\s*\(/.test(body), false);
  // only the immutable touchstart snapshot is consulted
  assert.match(body, /touch\.overlayOwned/);
});

test("overlay that 'closed' mid-gesture: original snapshot still suppresses", () => {
  // Simulate Notes closing itself in bubbling handler before document touchend:
  // at touchstart overlayOwned was true; by touchend live flags are false.
  const touch = captureTouchStart({
    x: 20, y: 200, clientWidth: 390, overlayOwned: true,
  });
  const action = documentSwipeDecision(touch, { x: 120, y: 200 }, {
    level: 1,
    bookmarksOpen: true, // would otherwise mean L→R closes bookmarks → Chat overshoot
    isDesktop: false,
  });
  assert.equal(action.type, "suppress");
});

test("documentSwipeDecision returns null without touch snapshot", () => {
  assert.equal(documentSwipeDecision(null, { x: 100, y: 100 }, {}), null);
});

/* ---------- horizontal threshold / fences ---------- */
test("documentSwipeDecision: sub-threshold or vertical motion is no-op", () => {
  const touch = captureTouchStart({ x: 100, y: 100, clientWidth: 390, overlayOwned: false });
  assert.equal(documentSwipeDecision(touch, { x: 140, y: 100 }, { level: 2, isDesktop: false }), null); // dx=40 < 60
  assert.equal(documentSwipeDecision(touch, { x: 200, y: 180 }, { level: 2, isDesktop: false }), null); // not horiz enough
});

/* Live cancel-editors guard is (editingChatDesc || editingTitle === sel).
   When both editingTitle and sel are "" the equality is true — matching the
   inline handler. Tests that exercise navigation must pass a non-empty sel
   (or explicit editing flags) so the cancel branch does not vacuously win. */
const baseState = {
  sel: "n1",
  editingTitle: "",
  editingChatDesc: false,
  bookmarksOpen: false,
  isDesktop: false,
};

test("documentSwipeDecision: localCard fences left-edge and general level moves", () => {
  const touch = captureTouchStart({
    x: 10, y: 100, clientWidth: 390, localCard: true, overlayOwned: false,
  });
  // left-edge L→R would open level, but localCard blocks
  assert.equal(
    documentSwipeDecision(touch, { x: 100, y: 100 }, { ...baseState, level: 1 }),
    null,
  );
  // general R→L from mid-card also blocked when localCard
  const mid = captureTouchStart({
    x: 150, y: 100, clientWidth: 390, localCard: true, overlayOwned: false,
  });
  assert.equal(
    documentSwipeDecision(mid, { x: 50, y: 100 }, { ...baseState, level: 2 }),
    null,
  );
});

test("documentSwipeDecision: editor cancel on R→L horiz when editing", () => {
  const touch = captureTouchStart({
    x: 200, y: 100, clientWidth: 390, overlayOwned: false,
  });
  assert.deepEqual(
    documentSwipeDecision(touch, { x: 100, y: 100 }, {
      ...baseState, level: 1, editingChatDesc: true,
    }),
    { type: "cancelEditors" },
  );
  assert.deepEqual(
    documentSwipeDecision(touch, { x: 100, y: 100 }, {
      ...baseState, level: 1, editingTitle: "n1",
    }),
    { type: "cancelEditors" },
  );
  // editing a different node's title does not cancel
  assert.notEqual(
    documentSwipeDecision(touch, { x: 100, y: 100 }, {
      ...baseState, level: 2, editingTitle: "other",
    })?.type,
    "cancelEditors",
  );
  // Live quirk: empty sel + empty editingTitle makes editingTitle === sel true
  assert.deepEqual(
    documentSwipeDecision(touch, { x: 100, y: 100 }, {
      level: 2, isDesktop: false, sel: "", editingTitle: "", editingChatDesc: false,
    }),
    { type: "cancelEditors" },
  );
});

/* ---------- right-edge / Bookmarks / level steps ---------- */
test("right-edge R→L: steps existing level before opening Bookmarks from Chat", () => {
  const edgeR = captureTouchStart({
    x: 380, y: 100, clientWidth: 390, overlayOwned: false,
  });
  assert.deepEqual(
    documentSwipeDecision(edgeR, { x: 280, y: 100 }, { ...baseState, level: 3 }),
    { type: "setLevel", level: 2 },
  );
  assert.deepEqual(
    documentSwipeDecision(edgeR, { x: 280, y: 100 }, { ...baseState, level: 2 }),
    { type: "setLevel", level: 1 },
  );
  assert.deepEqual(
    documentSwipeDecision(edgeR, { x: 280, y: 100 }, { ...baseState, level: 1 }),
    { type: "setBookmarksOpen", open: true },
  );
});

test("Bookmarks R→L opens Notes; Bookmarks L→R returns to Chat", () => {
  const mid = captureTouchStart({
    x: 200, y: 100, clientWidth: 390, overlayOwned: false,
  });
  assert.deepEqual(
    documentSwipeDecision(mid, { x: 100, y: 100 }, {
      ...baseState, level: 1, bookmarksOpen: true,
    }),
    { type: "openWorkspace" },
  );
  assert.deepEqual(
    documentSwipeDecision(mid, { x: 300, y: 100 }, {
      ...baseState, level: 1, bookmarksOpen: true,
    }),
    { type: "setBookmarksOpen", open: false },
  );
});

test("left-edge and general swipes move only one level", () => {
  const left = captureTouchStart({
    x: 10, y: 100, clientWidth: 390, overlayOwned: false,
  });
  assert.deepEqual(
    documentSwipeDecision(left, { x: 100, y: 100 }, { ...baseState, level: 1 }),
    { type: "setLevel", level: 2 },
  );
  assert.deepEqual(
    documentSwipeDecision(left, { x: 100, y: 100 }, { ...baseState, level: 2 }),
    { type: "setLevel", level: 3 },
  );
  assert.deepEqual(
    documentSwipeDecision(left, { x: 100, y: 100 }, { ...baseState, level: 3 }),
    { type: "setLevel", level: 3 },
  );

  const mid = captureTouchStart({
    x: 150, y: 100, clientWidth: 390, overlayOwned: false,
  });
  assert.deepEqual(
    documentSwipeDecision(mid, { x: 50, y: 100 }, { ...baseState, level: 3 }),
    { type: "setLevel", level: 2 },
  );
  assert.equal(
    documentSwipeDecision(mid, { x: 50, y: 100 }, { ...baseState, level: 1 }),
    null,
  );
});

test("desktop suppresses special Bookmarks routes but preserves general level decisions", () => {
  const edgeR = captureTouchStart({
    x: 380, y: 100, clientWidth: 390, overlayOwned: false,
  });
  assert.equal(
    documentSwipeDecision(edgeR, { x: 280, y: 100 }, {
      ...baseState, level: 1, isDesktop: true,
    }),
    null,
  );
  const mid = captureTouchStart({
    x: 200, y: 100, clientWidth: 390, overlayOwned: false,
  });
  assert.equal(
    documentSwipeDecision(mid, { x: 100, y: 100 }, {
      ...baseState, level: 1, bookmarksOpen: true, isDesktop: true,
    }),
    null,
  );
  const left = captureTouchStart({
    x: 10, y: 100, clientWidth: 390, overlayOwned: false,
  });
  assert.deepEqual(
    documentSwipeDecision(left, { x: 100, y: 100 }, {
      ...baseState, level: 1, isDesktop: true,
    }),
    { type: "setLevel", level: 2 },
    "the inline handler still updates level before setLevel returns on desktop",
  );
  assert.deepEqual(
    documentSwipeDecision(mid, { x: 100, y: 100 }, {
      ...baseState, level: 2, isDesktop: true,
    }),
    { type: "setLevel", level: 1 },
  );
  assert.deepEqual(
    documentSwipeDecision(mid, { x: 100, y: 100 }, {
      ...baseState, level: 1, isDesktop: true, editingChatDesc: true,
    }),
    { type: "cancelEditors" },
  );
});

/* ---------- Notes workspace swipe-back ---------- */
test("notesSwipeBackDecision: narrow back order placement → list → close", () => {
  const start = captureNotesTouchStart({ x: 50, y: 100, inEditable: false });
  const end = { x: 150, y: 100 }; // L→R

  assert.deepEqual(
    notesSwipeBackDecision(start, end, { isNarrow: true, placing: true, noteOpen: true }),
    { type: "endPlacement" },
  );
  assert.deepEqual(
    notesSwipeBackDecision(start, end, { isNarrow: true, placing: false, noteOpen: true }),
    { type: "noteToList" },
  );
  assert.deepEqual(
    notesSwipeBackDecision(start, end, { isNarrow: true, placing: false, noteOpen: false }),
    { type: "closeWorkspace" },
  );
});

test("notesSwipeBackDecision: editable and wide-layout gestures ignored", () => {
  const end = { x: 150, y: 100 };
  const editable = captureNotesTouchStart({ x: 50, y: 100, inEditable: true });
  assert.equal(
    notesSwipeBackDecision(editable, end, { isNarrow: true, placing: false, noteOpen: true }),
    null,
  );
  const normal = captureNotesTouchStart({ x: 50, y: 100, inEditable: false });
  assert.equal(
    notesSwipeBackDecision(normal, end, { isNarrow: false, placing: false, noteOpen: true }),
    null,
  );
});

test("notesSwipeBackDecision: non-L→R or sub-threshold is no-op", () => {
  const start = captureNotesTouchStart({ x: 100, y: 100, inEditable: false });
  assert.equal(
    notesSwipeBackDecision(start, { x: 50, y: 100 }, { isNarrow: true }), // R→L
    null,
  );
  assert.equal(
    notesSwipeBackDecision(start, { x: 130, y: 100 }, { isNarrow: true }), // dx=30
    null,
  );
  assert.equal(
    notesSwipeBackDecision(start, { x: 200, y: 180 }, { isNarrow: true }), // not horiz
    null,
  );
  assert.equal(notesSwipeBackDecision(null, { x: 200, y: 100 }, { isNarrow: true }), null);
});

/* ---------- journeys toggle (item 18: the chevron must flip) ---------- */

/* The chevron points the way the PANE travels, which is the mirror image of the
   right-hand bookmarks toggle: Journeys lives left of Activities, so opening it
   moves content rightward (›) and closing it moves left (‹). Copying the
   bookmarks mapping verbatim read as semantically flipped (review 2, item 2). */
test("journeyToggleState points the way the left-hand pane travels", () => {
  assert.deepEqual(journeyToggleState(false), {
    innerHTML: "&#8250;",
    ariaLabel: "open journeys",
  });
  assert.deepEqual(journeyToggleState(true), {
    innerHTML: "&#8249;",
    ariaLabel: "close journeys",
  });
});

test("journeyToggleState is the mirror of the right-hand bookmarks toggle", () => {
  assert.notEqual(journeyToggleState(true).innerHTML, journeyToggleState(false).innerHTML);
  assert.equal(journeyToggleState(true).innerHTML, bookmarksToggleState(false).innerHTML);
  assert.equal(journeyToggleState(false).innerHTML, bookmarksToggleState(true).innerHTML);
});

test("journeyToggleState treats a missing argument as closed", () => {
  assert.deepEqual(journeyToggleState(), journeyToggleState(false));
});
