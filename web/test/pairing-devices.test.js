/* P4 — the paired-device list, which is the other half of a grant.
 *
 * Pairing hands a device SSH-equivalent authority on this computer. A grant
 * nobody can see and nobody can take back is not a grant, it is a leak, so
 * the list and its revoke are part of the same feature rather than a
 * later nicety.
 *
 * Two properties carry the weight here and neither is cosmetic:
 *   - revoking is two taps, because the row next to the one you meant is
 *     the phone you are holding.
 *   - a failed read never blanks the list, because an empty list reads as
 *     "nothing is paired" — the single most dangerous thing this surface
 *     can say when it is not true.
 */
import test from "node:test";
import assert from "node:assert/strict";

import { createDeviceList } from "../js/pairing-ui.js";

function makeEl(id) {
  const listeners = {};
  return {
    id,
    innerHTML: "",
    textContent: "",
    addEventListener(type, fn) {
      (listeners[type] || (listeners[type] = [])).push(fn);
    },
    removeEventListener(type, fn) {
      listeners[type] = (listeners[type] || []).filter((f) => f !== fn);
    },
    listenerCount(type) {
      return (listeners[type] || []).length;
    },
    /* A click on a descendant carrying data-dev, as delegation sees it. */
    click(id_) {
      const target = {
        closest(sel) {
          return sel === "[data-dev]" && id_ ? { dataset: { dev: id_ } } : null;
        },
      };
      for (const fn of listeners.click || []) fn({ type: "click", target, preventDefault() {} });
    },
  };
}

function makeDoc() {
  const els = new Map([["#m_devices", makeEl("m_devices")]]);
  return { el: (s) => els.get(s), querySelector: (s) => els.get(s) || null };
}

function makeApi(routes) {
  const calls = [];
  const fn = async (path, opts = {}) => {
    const method = (opts.method || "GET").toUpperCase();
    const key = `${method} ${path}`;
    calls.push(key);
    const h = routes[key];
    if (!h) throw new Error(`unrouted ${key}`);
    return typeof h === "function" ? h() : h;
  };
  fn.calls = calls;
  return fn;
}

const LIST = "GET /api/remote/devices";
const REVOKE_A = "DELETE /api/remote/devices/dev-a";
const REVOKE_B = "DELETE /api/remote/devices/dev-b";

const TWO = {
  devices: [
    { id: "dev-a", label: "Christian's <b>iPhone</b>", paired_at: "2026-08-20T09:00:00Z" },
    { id: "dev-b", label: "iPad", paired_at: "2026-08-21T09:00:00Z" },
  ],
};

function setup(routes) {
  const doc = makeDoc();
  const api = makeApi(routes);
  const f = createDeviceList({ api, doc });
  f.bind();
  return { f, doc, api, html: () => doc.el("#m_devices").innerHTML };
}

test("the list names every paired device and offers each one a revoke", async () => {
  const h = setup({ [LIST]: TWO });
  await h.f.refresh();
  assert.match(h.html(), /Christian's &lt;b&gt;iPhone&lt;\/b&gt;/,
    "a device label the phone chose was rendered as markup, or not rendered at all");
  assert.match(h.html(), /iPad/);
  assert.match(h.html(), /data-dev="dev-a"/);
  assert.match(h.html(), /data-dev="dev-b"/);
});

test("a device with no label is still identifiable", async () => {
  /* A row that says nothing is a row nobody dares revoke. */
  const h = setup({ [LIST]: { devices: [{ id: "dev-a" }] } });
  await h.f.refresh();
  assert.match(h.html(), /dev-a/);
});

test("an empty list says so rather than showing nothing", async () => {
  const h = setup({ [LIST]: { devices: [] } });
  await h.f.refresh();
  assert.match(h.html(), /no devices/i);
  assert.doesNotMatch(h.html(), /data-dev=/);
});

test("the first tap on revoke reaches the network for nothing", async () => {
  /* The row next to the one you meant is the phone you are holding. */
  const h = setup({ [LIST]: TWO });
  await h.f.refresh();
  h.doc.el("#m_devices").click("dev-a");
  await h.f.settled();
  assert.deepEqual(h.api.calls, [LIST], "one tap revoked a device");
  assert.match(h.html(), /confirm/i, "the armed row does not say what the next tap does");
});

test("the second tap revokes that device and re-reads the list", async () => {
  const h = setup({ [LIST]: TWO, [REVOKE_A]: () => "" });
  await h.f.refresh();
  h.doc.el("#m_devices").click("dev-a");
  await h.f.settled();
  h.doc.el("#m_devices").click("dev-a");
  await h.f.settled();
  assert.deepEqual(h.api.calls, [LIST, REVOKE_A, LIST], "the revoke did not happen, or the list went stale after it");
});

test("arming one row disarms any other", async () => {
  /* Two armed rows means one stray tap revokes a device the human armed
   * a minute ago and then thought better of. */
  const h = setup({ [LIST]: TWO, [REVOKE_B]: () => "" });
  await h.f.refresh();
  h.doc.el("#m_devices").click("dev-a");
  await h.f.settled();
  h.doc.el("#m_devices").click("dev-b");
  await h.f.settled();
  assert.deepEqual(h.api.calls, [LIST], "arming a second row fired the first");

  h.doc.el("#m_devices").click("dev-a");
  await h.f.settled();
  assert.deepEqual(h.api.calls, [LIST], "the first row was still armed");
});

test("a successful revoke leaves nothing armed", async () => {
  const h = setup({ [LIST]: TWO, [REVOKE_A]: () => "" });
  await h.f.refresh();
  h.doc.el("#m_devices").click("dev-a");
  await h.f.settled();
  h.doc.el("#m_devices").click("dev-a");
  await h.f.settled();
  assert.doesNotMatch(h.html(), /confirm/i, "a row stayed armed across a re-read");
});

test("a failed read never blanks the devices already on screen", async () => {
  /* An empty list reads as "nothing is paired". That is the most
   * dangerous sentence this surface can say when it is not true. */
  let fail = false;
  const h = setup({
    [LIST]: () => {
      if (fail) throw new Error("network down");
      return TWO;
    },
  });
  await h.f.refresh();
  fail = true;
  await h.f.refresh();

  assert.match(h.html(), /data-dev="dev-a"/, "a failed read emptied the list of grants");
  assert.match(h.html(), /could not/i, "the failure was invisible, so the list looked current");
});

test("a first read that fails says so instead of claiming nothing is paired", async () => {
  const h = setup({ [LIST]: () => { throw new Error("network down"); } });
  await h.f.refresh();
  assert.doesNotMatch(h.html(), /no devices/i, "an unreachable list claimed the computer had no paired devices");
  assert.match(h.html(), /could not/i);
});

test("a revoke that fails says so and leaves the device listed", async () => {
  const h = setup({ [LIST]: TWO, [REVOKE_A]: () => { throw new Error("nope"); } });
  await h.f.refresh();
  h.doc.el("#m_devices").click("dev-a");
  await h.f.settled();
  h.doc.el("#m_devices").click("dev-a");
  await h.f.settled();
  assert.match(h.html(), /data-dev="dev-a"/, "a device vanished from the list without being revoked");
  assert.match(h.html(), /could not/i);
});

test("destroy releases the delegation", async () => {
  const h = setup({ [LIST]: TWO });
  assert.ok(h.doc.el("#m_devices").listenerCount("click") > 0);
  h.f.destroy();
  assert.equal(h.doc.el("#m_devices").listenerCount("click"), 0);
});

test("an unlabelled device is named by its id, not by nothing", async () => {
  /* `dev-a` also appears in data-dev, so this asserts the part a human
     reads rather than the part the click handler reads. */
  const h = setup({ [LIST]: { devices: [{ id: "dev-a" }] } });
  await h.f.refresh();
  assert.match(h.html(), /<span>dev-a<\/span>/,
    "the row's visible name is empty, so the human is asked to revoke a blank");
});

test("a device id is escaped into the attribute that carries it", async () => {
  /* The id is server-minted today. It lands in an HTML attribute, and the
     escape is what keeps that true of tomorrow's id too. */
  const h = setup({ [LIST]: { devices: [{ id: 'a"b', label: "Phone" }] } });
  await h.f.refresh();
  assert.doesNotMatch(h.html(), /data-dev="a"b"/, "a device id broke out of its attribute");
  assert.match(h.html(), /data-dev="a&quot;b"/);
});

test("the armed row is marked destructive, not merely relabelled", async () => {
  /* Colour is the part a thumb reads before the word does. */
  const h = setup({ [LIST]: TWO });
  await h.f.refresh();
  h.doc.el("#m_devices").click("dev-a");
  await h.f.settled();
  assert.match(h.html(), /class="danger"[^>]*>Confirm revoke|Confirm revoke/);
  assert.match(h.html(), /class="danger"/, "the armed revoke looks like every other button");
});

test("a read that succeeds after a failure clears the failure", async () => {
  /* A stale "could not read" sitting under a current list tells the human
     the list is stale when it is not. */
  let fail = true;
  const h = setup({
    [LIST]: () => {
      if (fail) throw new Error("network down");
      return TWO;
    },
  });
  await h.f.refresh();
  assert.match(h.html(), /could not/i);
  fail = false;
  await h.f.refresh();
  assert.doesNotMatch(h.html(), /could not/i, "a healed list still reports the old failure");
});

test("re-reading the list disarms whatever was armed", async () => {
  /* The menu is closed and reopened; nobody should find a loaded revoke
     waiting under their thumb from a minute ago. */
  const h = setup({ [LIST]: TWO, [REVOKE_A]: () => "" });
  await h.f.refresh();
  h.doc.el("#m_devices").click("dev-a");
  await h.f.settled();
  await h.f.refresh();
  assert.doesNotMatch(h.html(), /confirm/i, "a row stayed armed across a fresh read");

  h.doc.el("#m_devices").click("dev-a");
  await h.f.settled();
  assert.deepEqual(h.api.calls, [LIST, LIST], "the stale arming fired a revoke");
});

test("a tap that misses every row does nothing at all", async () => {
  const h = setup({ [LIST]: TWO });
  await h.f.refresh();
  h.doc.el("#m_devices").click("");
  await h.f.settled();
  assert.deepEqual(h.api.calls, [LIST]);
  assert.doesNotMatch(h.html(), /confirm/i);
});

test("a page without the list slot binds and reads without throwing", async () => {
  /* Every write is guarded, which is right for a browser: a renamed id
     must degrade to an empty section, never to a dead module that takes
     the rest of the shell down with it. */
  const doc = { querySelector: () => null };
  const api = makeApi({ [LIST]: TWO });
  const f = createDeviceList({ api, doc });
  f.bind();
  await f.refresh();
  f.destroy();
});
