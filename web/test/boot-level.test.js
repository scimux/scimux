/* Return to where the user left off after a sleep/wake re-boot.
 *
 * A sleeping phone drops the tunnel; rv re-dials and bootstrap.js calls
 * createApp() again in a fresh module graph (lifecycle.js), so every in-memory
 * variable restarts from its initializer. `scimux-sel` already survives in
 * storage, but the pane level did not — so the phone reopened the chat it
 * remembered with the Activities list parked in front of it.
 *
 * Pure decisions live in navigation.js and are tested directly; the shell
 * seams are locked by source assertions, the ready-seen.test.js pattern.
 */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { LEVEL_KEY, bootLevel, loadStoredLevel, saveLevel } from "../js/navigation.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const appSrc = readFileSync(join(__dirname, "../js/app.js"), "utf8");

/* A localStorage-shaped adapter that records writes. */
function fakeStorage(seed = {}) {
  const map = new Map(Object.entries(seed));
  return {
    map,
    getItem: k => (map.has(k) ? map.get(k) : null),
    setItem: (k, v) => { map.set(k, v); },
    removeItem: k => { map.delete(k); },
  };
}

/* ---------- bootLevel: which pane a fresh boot opens ---------- */

test("desktop always boots on chat, whatever the phone last stored", () => {
  for (const stored of [null, "1", "2", "3", "garbage"]) {
    assert.equal(bootLevel({ stored, isDesktop: true, hasSelection: true }), 1);
  }
});

test("a phone with nothing stored still opens the Activities list", () => {
  assert.equal(bootLevel({ stored: null, isDesktop: false, hasSelection: true }), 2);
  assert.equal(bootLevel({}), 2);
});

test("a phone that was in a chat comes back to that chat", () => {
  assert.equal(bootLevel({ stored: "1", isDesktop: false, hasSelection: true }), 1);
});

test("a phone that was on the map comes back to the map", () => {
  assert.equal(bootLevel({ stored: "3", isDesktop: false, hasSelection: true }), 3);
});

test("chat is not restored without a remembered selection", () => {
  /* Level 1 with no sel is an empty chat pane — the list is the honest start. */
  assert.equal(bootLevel({ stored: "1", isDesktop: false, hasSelection: false }), 2);
  /* The map does not need one: it renders the fleet, not a conversation. */
  assert.equal(bootLevel({ stored: "3", isDesktop: false, hasSelection: false }), 3);
});

test("a stored level that is not a pane falls back to the list", () => {
  for (const stored of ["", "0", "4", "-1", "1.5", "x", "NaN", undefined, {}, []]) {
    assert.equal(bootLevel({ stored, isDesktop: false, hasSelection: true }), 2,
      `stored=${JSON.stringify(stored)} must not restore a pane`);
  }
});

/* ---------- storage adapter (injected; never the global) ---------- */

test("loadStoredLevel reads the key and reports nothing as null", () => {
  assert.equal(loadStoredLevel(fakeStorage({ [LEVEL_KEY]: "3" })), "3");
  assert.equal(loadStoredLevel(fakeStorage()), null);
  assert.equal(loadStoredLevel(null), null);
});

test("loadStoredLevel survives a storage that throws", () => {
  assert.equal(loadStoredLevel({ getItem() { throw new Error("denied"); } }), null);
});

test("saveLevel writes the clamped pane number", () => {
  const s = fakeStorage();
  saveLevel(s, 3);
  assert.equal(s.map.get(LEVEL_KEY), "3");
  saveLevel(s, 9);
  assert.equal(s.map.get(LEVEL_KEY), "3", "out-of-range levels clamp, never store");
  saveLevel(s, 0);
  assert.equal(s.map.get(LEVEL_KEY), "1");
});

test("saveLevel never lets a full or denied store break navigation", () => {
  assert.doesNotThrow(() => saveLevel({ setItem() { throw new Error("quota"); } }, 2));
  assert.doesNotThrow(() => saveLevel(null, 2));
});

test("the storage key is the one the shell reads", () => {
  assert.equal(LEVEL_KEY, "scimux-level");
});

/* ---------- shell seams ---------- */

test("app.js derives its starting level from storage, not a hardcoded pane", () => {
  assert.match(appSrc, /let level = bootLevel\(/,
    "the boot pane must come from bootLevel()");
  assert.match(appSrc, /bootLevel\(\{[\s\S]{0,200}?stored: loadStoredLevel\(localStorage\)/);
  assert.match(appSrc, /bootLevel\(\{[\s\S]{0,200}?hasSelection: !!sel/,
    "chat is only restored when a remembered chat exists");
  assert.equal(/let level = matchMedia\([^)]*\)\.matches \? 1 : 2/.test(appSrc), false,
    "the old unconditional phone default must be gone");
  assert.match(appSrc, /setLevel\(level\)/, "boot still syncs the DOM to the level");
});

test("setLevel persists the pane, and only the phone's", () => {
  const start = appSrc.indexOf("function setLevel(n){");
  assert.ok(start >= 0, "setLevel() is a named function");
  const body = appSrc.slice(start, appSrc.indexOf("function applyNavAction", start));
  assert.match(body, /saveLevel\(localStorage, level\)/);
  assert.ok(body.indexOf("if (isDesktop()) return;") < body.indexOf("saveLevel("),
    "desktop returns before the write: the stored value is the phone's pane");
});

test("a remembered chat that no longer exists drops back to the list", () => {
  const start = appSrc.indexOf("ensureSelection: () => {");
  assert.ok(start >= 0, "ensureSelection is the selected-node fallback seam");
  const body = appSrc.slice(start, start + 900);
  assert.match(body, /setLevel\(2\)/,
    "substituting a different node must not leave the restored chat pane up");
  assert.match(body, /level === 1/,
    "only a restored chat pane steps back; cards and map are already honest");
});
