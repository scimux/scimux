/* Unit tests for web/js/storage.js — the browser store that cannot throw.
 * Every case here is a browser saying no: private mode, a policy change, a
 * full quota, a value somebody else wrote. */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { createStorage } from "../js/storage.js";
import { installHost, identityAssetURL } from "./s2-helpers.js";

/* A localStorage-shaped object whose named methods throw the way a browser
 * refuses: a DOMException, not a plain Error. */
function refusing(parts, init = {}){
  const map = new Map(Object.entries(init));
  const refuse = () => { throw new Error("SecurityError: access denied"); };
  return {
    getItem: k => (parts.includes("getItem") ? refuse() : (map.has(k) ? map.get(k) : null)),
    setItem: (k, v) => (parts.includes("setItem") ? refuse() : void map.set(k, String(v))),
    removeItem: k => (parts.includes("removeItem") ? refuse() : void map.delete(k)),
    _map: map,
  };
}

test("a working store is used, and says it will survive the tab", () => {
  const raw = refusing([]);
  const s = createStorage(raw);
  s.setItem("k", "v");
  assert.equal(raw._map.get("k"), "v");
  assert.equal(s.getItem("k"), "v");
  s.removeItem("k");
  assert.equal(s.getItem("k"), null);
  assert.equal(s.persistent, true);
});

test("a store that denies access at all keeps working in memory", () => {
  const s = createStorage(refusing(["getItem", "setItem", "removeItem"]));
  s.setItem("k", "v");
  assert.equal(s.getItem("k"), "v");
  assert.equal(s.persistent, false, "the user is owed the truth about closing the tab");
});

test("a write the store refuses is still readable in this tab", () => {
  const s = createStorage(refusing(["setItem"]));
  s.setItem("scimux-draft:n1", "unsent text");
  assert.equal(s.getItem("scimux-draft:n1"), "unsent text");
  assert.equal(s.persistent, false);
});

test("a removal the store refuses still forgets the value", () => {
  const s = createStorage(refusing(["removeItem"], { "scimux-draft:n1": "sent already" }));
  s.removeItem("scimux-draft:n1");
  assert.equal(s.getItem("scimux-draft:n1"), null, "a draft that was sent must not come back");
});

test("a read that throws reads as absent, and writing still works after it", () => {
  const s = createStorage(refusing(["getItem"]));
  assert.equal(s.getItem("k"), null);
  s.setItem("k", "v");
  assert.equal(s.getItem("k"), "v");
});

test("a stored value that is not a string reads as absent", () => {
  const s = createStorage({
    getItem: () => ({ not: "a string" }),
    setItem: () => {},
    removeItem: () => {},
  });
  assert.equal(s.getItem("k"), null);
});

test("no store at all is a usable store", () => {
  const s = createStorage(null);
  s.setItem("k", "v");
  assert.equal(s.getItem("k"), "v");
  assert.equal(s.persistent, false);
});

/* ---------- shell seams ----------
 * app.js is the untested composition root, so the adapter's *behaviour* is
 * tested above and what is checked here is that the shell actually uses it:
 * one boot against a browser that refuses everything, plus a ratchet over the
 * call sites a boot does not reach (selection, pane, lane filter). */
const appSrc = readFileSync(
  join(dirname(fileURLToPath(import.meta.url)), "../js/app.js"), "utf8");

test("a browser that refuses storage still boots the shell, and says so", async () => {
  const host = installHost({});
  const denied = refusing(["getItem", "setItem", "removeItem"]);
  host.window.localStorage = denied;
  Object.defineProperty(globalThis, "localStorage",
    { configurable: true, writable: true, value: denied });

  const { createApp } = await import("../js/app.js");
  const app = await createApp({
    fetchImpl: host.trapFetch,
    assetURL: identityAssetURL,
    document: host.document,
    window: host.window,
  });
  for (let i = 0; i < 30; i++) await Promise.resolve();
  const toast = host.document.body.children.find(c => c.id === "toast");
  assert.ok(toast, "a store that cannot persist is worth one quiet line");
  assert.match(toast.textContent, /tab/i);
  app.retire();
});

test("the shell reaches the browser store only through the adapter", () => {
  assert.match(appSrc, /import \{ createStorage \} from "\.\/storage\.js";/);
  assert.match(appSrc, /const store = createStorage\(/,
    "one adapter, built once, handed to every feature");
  const raw = appSrc.split("\n")
    .map((line, i) => [i + 1, line])
    .filter(([, line]) => /\blocalStorage\b/.test(line) && !/createStorage\(/.test(line));
  assert.deepEqual(raw.map(([n]) => n), [],
    "these lines still touch localStorage directly, where a refusal throws");
});
