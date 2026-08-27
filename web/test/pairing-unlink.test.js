/* F1 — unlinking this laptop.
 *
 * Enrolling was one-way. Every other grant in scimux can be handed back
 * from this very menu — a paired device is revoked two rows up — but the
 * laptop's own enrollment could only be abandoned, which is a bad trade
 * to offer someone on the day you ask them to try remote access.
 *
 * Two properties carry the weight, and both are about honesty rather
 * than mechanism:
 *   - unlinking is two taps, because it ends every grant at once and
 *     costs a fresh invite to undo.
 *   - the local half always happens and the rendezvous half may not, so
 *     the message says which. A laptop the rendezvous still holds needs
 *     its operator, and a UI that hid that would send the user away
 *     believing something untrue.
 */
import test from "node:test";
import assert from "node:assert/strict";

import { createUnlinkControl } from "../js/pairing-ui.js";

function makeEl(id) {
  const listeners = {};
  return {
    id,
    innerHTML: "",
    hidden: false,
    className: "",
    addEventListener(type, fn) {
      (listeners[type] || (listeners[type] = [])).push(fn);
    },
    removeEventListener(type, fn) {
      listeners[type] = (listeners[type] || []).filter((f) => f !== fn);
    },
    listenerCount(type) {
      return (listeners[type] || []).length;
    },
    click() {
      for (const fn of listeners.click || []) fn({ type: "click", target: this, preventDefault() {} });
    },
  };
}

function makeDoc() {
  const els = new Map([
    ["#m_unlink", makeEl("m_unlink")],
    ["#m_unlink_note", makeEl("m_unlink_note")],
  ]);
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
  fn.routes = routes;
  return fn;
}

const UNLINK = "POST /api/remote/unenroll";
const STATUS = "GET /api/remote/status";

function setup(routes, opts = {}) {
  const doc = makeDoc();
  const api = makeApi(routes);
  const unlinked = [];
  const f = createUnlinkControl({
    api,
    doc,
    onUnlinked: () => unlinked.push(1),
    ...opts,
  });
  f.bind();
  return {
    f,
    doc,
    api,
    unlinked,
    btn: () => doc.el("#m_unlink"),
    note: () => doc.el("#m_unlink_note").innerHTML,
  };
}

test("the first tap reaches the network for nothing", async () => {
  /* Unlinking ends every grant this laptop has issued and costs a fresh
   * invite to undo. One tap is not enough intent for that. */
  const h = setup({ [UNLINK]: { released: true, hosted: "" } });
  h.btn().click();
  await h.f.settled();
  assert.deepEqual(h.api.calls, [], "one tap unlinked the laptop");
  assert.match(h.btn().innerHTML, /confirm/i, "the armed button does not say what the next tap does");
});

test("the second tap unlinks and reports the release", async () => {
  const h = setup({ [UNLINK]: { released: true, hosted: "" } });
  h.btn().click();
  await h.f.settled();
  h.btn().click();
  await h.f.settled();
  assert.deepEqual(h.api.calls, [UNLINK]);
  assert.equal(h.unlinked.length, 1, "the rest of the menu was not told the enrollment ended");
  assert.match(h.note(), /unlinked/i);
  assert.doesNotMatch(h.note(), /could not/i);
});

test("an installation the rendezvous still holds says so", async () => {
  /* released:false is the honest report, not a failure: the laptop is
   * unlinked either way, and only the operator can strike off the
   * installation the rendezvous never heard about. */
  const h = setup({ [UNLINK]: { released: false, hosted: "" } });
  h.btn().click();
  await h.f.settled();
  h.btn().click();
  await h.f.settled();
  assert.equal(h.unlinked.length, 1, "a local unlink was treated as no unlink at all");
  assert.match(h.note(), /unlinked/i, "the user was not told the local half happened");
  assert.match(h.note(), /revoke/i, "the user was not told who can finish the job");
});

test("a failed unlink says so and stays offerable", async () => {
  /* The local half failing means the identity is still on disk, which
   * means this laptop is still enrolled. Saying otherwise would be the
   * one lie this surface must never tell. */
  const h = setup({ [UNLINK]: () => { throw new Error("identity file is locked"); } });
  h.btn().click();
  await h.f.settled();
  h.btn().click();
  await h.f.settled();
  assert.equal(h.unlinked.length, 0, "a failed unlink told the menu the enrollment had ended");
  assert.match(h.note(), /could not/i);
  assert.equal(h.btn().hidden, false, "a failed unlink removed the only way to retry it");
  assert.doesNotMatch(h.btn().innerHTML, /confirm/i, "a failed unlink left the button armed");
});

test("the button is gone once there is nothing left to unlink", async () => {
  const h = setup({ [UNLINK]: { released: true, hosted: "" } });
  h.btn().click();
  await h.f.settled();
  h.btn().click();
  await h.f.settled();
  assert.equal(h.btn().hidden, true, "an unlinked laptop still offers to unlink");
});

test("a second tap while the first is in flight does not unlink twice", async () => {
  let resolve;
  const gate = new Promise((r) => { resolve = () => r({ released: true, hosted: "" }); });
  const h = setup({ [UNLINK]: () => gate });
  h.btn().click();
  await h.f.settled();
  h.btn().click();
  h.btn().click();
  resolve();
  await h.f.settled();
  assert.deepEqual(h.api.calls, [UNLINK], "an impatient double tap unlinked twice");
});

test("the control can be shown and hidden with the enrollment", async () => {
  /* A laptop that was never enrolled has nothing to unlink, and an
   * action that cannot do anything is worse than no action. */
  const h = setup({ [UNLINK]: { released: true, hosted: "" } });
  h.f.setEnrolled(false);
  assert.equal(h.btn().hidden, true);
  h.f.setEnrolled(true);
  assert.equal(h.btn().hidden, false);
});

test("hiding the control disarms it", async () => {
  /* Otherwise the arming survives out of sight and the next tap, minutes
   * later in a different frame of mind, is the confirming one. */
  const h = setup({ [UNLINK]: { released: true, hosted: "" } });
  h.btn().click();
  await h.f.settled();
  h.f.setEnrolled(false);
  h.f.setEnrolled(true);
  h.btn().click();
  await h.f.settled();
  assert.deepEqual(h.api.calls, [], "arming survived the control being hidden");
});

test("a refresh shows the control exactly when there is an enrollment", async () => {
  /* The status is the only thing that knows whether this laptop has an
     identity on disk at all. revoked and unavailable still do — those are
     precisely the installations someone wants rid of. */
  for (const hosted of ["enrolled", "revoked", "unavailable"]) {
    const h = setup({ [STATUS]: { hosted, devices: [] } });
    await h.f.refresh();
    assert.equal(h.btn().hidden, false, `hosted=${hosted} hid the unlink`);
  }
  const none = setup({ [STATUS]: { hosted: "", devices: [] } });
  await none.f.refresh();
  assert.equal(none.btn().hidden, true, "a laptop with nothing enrolled was offered an unlink");
});

test("a failed status read does not invent an answer either way", async () => {
  /* Never enrolled, so nothing is shown; and a read that failed is not
     evidence that changed. */
  const h = setup({ [STATUS]: () => { throw new Error("network down"); } });
  await h.f.refresh();
  assert.equal(h.btn().hidden, true);

  const known = setup({ [STATUS]: { hosted: "enrolled", devices: [] } });
  await known.f.refresh();
  known.api.routes[STATUS] = () => { throw new Error("network down"); };
  await known.f.refresh();
  assert.equal(known.btn().hidden, false, "a failed read retired an enrollment that still exists");
});

test("destroy releases the listener", () => {
  const h = setup({ [UNLINK]: { released: true } });
  assert.ok(h.btn().listenerCount("click") > 0);
  h.f.destroy();
  assert.equal(h.btn().listenerCount("click"), 0);
});
