/* Pairing is offered only where it could actually complete.
 *
 * "Pair a device" was unconditional markup: always rendered, always
 * clickable, on a computer with no enrollment at all. The mint is entirely
 * local — a code, a locally minted RID, a link built from the configured
 * origin — so nothing downstream said no either, and the user got a code, a
 * QR, and a wait that could only end in an expiry. What they needed to be
 * told was that this computer is not enrolled with a rendezvous.
 *
 * The button therefore ships hidden and is revealed by a status read, which
 * is the same shape the unlink control beside it already has. Two properties
 * carry the weight:
 *   - it fails closed. Before the first answer, and after a read that could
 *     not be made at all, there is no button or experimental section.
 *   - inside an active remote run, a hidden button says why. A control that
 *     vanishes without a word reads as a bug, and sends the user looking for
 *     the feature rather than for their enrollment.
 */
import test from "node:test";
import assert from "node:assert/strict";

import { createPairControl } from "../js/pairing-ui.js";

function makeEl(id) {
  return { id, innerHTML: "", hidden: false };
}

function makeDoc() {
  const els = new Map([
    ["#m_remote", makeEl("m_remote")],
    ["#m_pair", makeEl("m_pair")],
    ["#m_pair_note", makeEl("m_pair_note")],
  ]);
  return { el: (s) => els.get(s), querySelector: (s) => els.get(s) || null };
}

const STATUS = "GET /api/remote/status";

function setup(handler) {
  const doc = makeDoc();
  const calls = [];
  const api = async (path, opts = {}) => {
    const key = `${(opts.method || "GET").toUpperCase()} ${path}`;
    calls.push(key);
    return handler(key);
  };
  const f = createPairControl({ api, doc });
  f.bind();
  return {
    f,
    doc,
    calls,
    section: () => doc.el("#m_remote"),
    btn: () => doc.el("#m_pair"),
    note: () => doc.el("#m_pair_note"),
  };
}

const ok = (body) => (key) => {
  if (key !== STATUS) throw new Error(`unrouted ${key}`);
  return body;
};

test("the button is hidden until a status read says pairing is possible", async () => {
  const s = setup(ok({ hosted: "enrolled", can_pair: true }));
  assert.equal(s.section().hidden, true, "the experimental section ships closed");
  assert.equal(s.btn().hidden, true, "bind must not reveal the button on its own");
  await s.f.refresh();
  await s.f.settled();
  assert.equal(s.btn().hidden, false);
  assert.equal(s.section().hidden, false);
  assert.equal(s.note().hidden, true);
  assert.equal(s.note().innerHTML, "");
});

test("a refusal hides the button and says why, in the server's words", async () => {
  const s = setup(
    ok({
      hosted: "",
      can_pair: false,
      pair_refusal: "This installation is not enrolled with a rendezvous, so there is nowhere for a device to meet it.",
    }),
  );
  await s.f.refresh();
  await s.f.settled();
  assert.equal(s.btn().hidden, true);
  assert.equal(s.note().hidden, false);
  assert.match(s.note().innerHTML, /not enrolled with a rendezvous/);
});

test("a refusing status without a sentence still explains the hidden action", async () => {
  const s = setup(ok({ hosted: "enrolled", can_pair: false }));
  await s.f.refresh();
  await s.f.settled();
  assert.equal(s.section().hidden, false);
  assert.equal(s.btn().hidden, true);
  assert.equal(s.note().hidden, false);
  assert.match(s.note().innerHTML, /cannot pair a device right now/i);
});

test("a status route that is not there keeps the experimental section absent", async () => {
  const s = setup(() => {
    const e = new Error("remote pairing is not enabled");
    e.status = 404;
    throw e;
  });
  await s.f.refresh();
  await s.f.settled();
  assert.equal(s.btn().hidden, true, "remote is off; there is nothing to pair with");
  assert.equal(s.section().hidden, true, "an absent remote API keeps the whole section absent");
});

/* A read that failed is not evidence that anything changed. Hiding a working
   button on one bad answer would make the menu flicker between visits and
   teach the user that pairing comes and goes. */
test("a failed read after a good one keeps the last known answer", async () => {
  let fail = false;
  const s = setup((key) => {
    if (fail) throw new Error("network");
    if (key !== STATUS) throw new Error(`unrouted ${key}`);
    return { hosted: "enrolled", can_pair: true };
  });
  await s.f.refresh();
  await s.f.settled();
  assert.equal(s.btn().hidden, false);
  fail = true;
  await s.f.refresh();
  await s.f.settled();
  assert.equal(s.btn().hidden, false, "the button survives one unreadable status");
  assert.equal(s.note().hidden, true, "and claims nothing it did not learn");
});

/* can_pair is the server's word. Re-deriving it here from `hosted` would be
   a second copy of the policy the mint enforces, free to drift from it —
   "unavailable" is pairable and "revoked" is not, and only one place should
   know that. */
test("the control reads can_pair and does not second-guess it from hosted", async () => {
  const shown = setup(ok({ hosted: "unavailable", can_pair: true }));
  await shown.f.refresh();
  await shown.f.settled();
  assert.equal(shown.btn().hidden, false, "a transient outage still offers the mint that recovers it");

  const hidden = setup(ok({ hosted: "enrolled", can_pair: false, pair_refusal: "no." }));
  await hidden.f.refresh();
  await hidden.f.settled();
  assert.equal(hidden.btn().hidden, true);
});
