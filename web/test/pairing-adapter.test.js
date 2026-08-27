/* P3 — the adapter: the only part of pairing that touches a DOM, a clock
 * and the network. Everything it does is a translation, and every one of
 * those translations is a place a live pairing can be broken:
 *
 *   DOM   -> events   (taps, the ✕, action ids)
 *   clock -> events   (TICK, and the poll it drives)
 *   HTTP  -> events   (MINTED, OFFER, DEVICE_CONFIRMED, COMPLETED, failure)
 *   state -> DOM      (slots, the QR, the polite live region)
 *
 * The decisions themselves stay in the reducer and are specified by the
 * tables in pairing-state.test.js / pairing-refresh.test.js. Nothing here
 * re-asserts a transition; these tests assert only the four translations,
 * plus the two things that are pure adapter and exist nowhere else: the
 * timer's lifetime, and the cancel that must survive a code dying under it.
 */
import test from "node:test";
import assert from "node:assert/strict";

import { createPairingFeature } from "../js/pairing-ui.js";

/* ---------- fake host ---------- */

function makeEl(id) {
  const listeners = {};
  const attrs = {};
  const el = {
    id,
    className: "",
    innerHTML: "",
    textContent: "",
    hidden: false,
    dataset: {},
    attrs,
    setAttribute(k, v) {
      attrs[k] = String(v);
    },
    getAttribute(k) {
      return Object.prototype.hasOwnProperty.call(attrs, k) ? attrs[k] : null;
    },
    addEventListener(type, fn) {
      (listeners[type] || (listeners[type] = [])).push(fn);
    },
    removeEventListener(type, fn) {
      listeners[type] = (listeners[type] || []).filter((f) => f !== fn);
    },
    listenerCount(type) {
      return (listeners[type] || []).length;
    },
    /* A click on a descendant carrying data-pa, as delegation sees it. */
    click(action) {
      const target = {
        closest(sel) {
          return sel === "[data-pa]" && action ? { dataset: { pa: action } } : null;
        },
      };
      const ev = { type: "click", target, preventDefault() {} };
      for (const fn of listeners.click || []) fn(ev);
    },
  };
  return el;
}

const SLOTS = [
  "#pairsheet",
  "#pair_title",
  "#pair_body",
  "#pair_qr",
  "#pair_sas",
  "#pair_actions",
  "#pair_live",
  "#pair_x",
];

function makeDoc() {
  const els = new Map(SLOTS.map((sel) => [sel, makeEl(sel.slice(1))]));
  return {
    els,
    el(sel) {
      return els.get(sel);
    },
    querySelector(sel) {
      return els.get(sel) || null;
    },
  };
}

function makeTimers() {
  const t = { fns: new Map(), next: 1, cleared: [] };
  t.setInterval = (fn, ms) => {
    const id = t.next++;
    t.fns.set(id, { fn, ms });
    return id;
  };
  t.clearInterval = (id) => {
    t.cleared.push(id);
    t.fns.delete(id);
  };
  t.fire = () => {
    for (const { fn } of [...t.fns.values()]) fn();
  };
  t.live = () => t.fns.size;
  return t;
}

/* An api double: a queue of handlers keyed by "METHOD path", each call
 * recorded. Anything unrouted throws, so an unexpected request is a
 * failure rather than a silent undefined. */
function makeApi(routes = {}) {
  const calls = [];
  const fn = async (path, opts = {}) => {
    const method = (opts.method || "GET").toUpperCase();
    const key = `${method} ${path}`;
    calls.push({ key, path, method, body: opts.body ? JSON.parse(opts.body) : null });
    const h = routes[key];
    if (!h) throw new Error(`unrouted ${key}`);
    return typeof h === "function" ? h() : h;
  };
  fn.calls = calls;
  fn.keys = () => calls.map((c) => c.key);
  fn.routes = routes;
  return fn;
}

function httpError(status, message) {
  const e = new Error(message || `status ${status}`);
  e.status = status;
  return e;
}

const T0 = 1_000_000;
const CODE = "04106105";
const LINK = "https://my.scimux.eu/p#c=04106105";

function mintOK(over = {}) {
  return {
    code: CODE,
    rid: "a".repeat(64),
    link: LINK,
    expires_at: "2026-08-27T10:00:00Z",
    state: "pending",
    ...over,
  };
}

const MINT = "POST /api/remote/pairing";
const POLL = `GET /api/remote/pairing/${CODE}`;
const CONFIRM = `POST /api/remote/pairing/${CODE}/confirm`;
const CANCEL = `POST /api/remote/pairing/${CODE}/cancel`;

function setup(routes = {}, over = {}) {
  const doc = makeDoc();
  const timers = makeTimers();
  const api = makeApi(routes);
  let clock = T0;
  const f = createPairingFeature({
    api,
    doc,
    timers,
    now: () => clock,
    ...over,
  });
  f.bind();
  return {
    f,
    doc,
    timers,
    api,
    advance(ms) {
      clock += ms;
    },
    at(ms) {
      clock = ms;
    },
  };
}

/* Walk from closed to a freshly minted code on screen. */
async function toShowCode(h) {
  h.f.open();
  h.doc.el("#pair_actions").click("begin");
  h.doc.el("#pair_actions").click("ack");
  await h.f.settled();
}

/* ---------- clock -> events, and the timer's lifetime ---------- */

test("the ticker runs only while the sheet is open", async () => {
  const h = setup({ [MINT]: () => mintOK() });
  assert.equal(h.timers.live(), 0, "a closed sheet was already ticking");

  h.f.open();
  assert.equal(h.timers.live(), 1, "opening the sheet started no ticker");

  h.f.close();
  await h.f.settled();
  assert.equal(h.timers.live(), 0, "the ticker outlived the sheet");
});

test("opening an already-open sheet does not start a second ticker", async () => {
  /* Two intervals means two TICKs per second, which means two mints for
   * every expiry — and the second one silently replaces the code the
   * human is scanning. */
  const h = setup({ [MINT]: () => mintOK() });
  h.f.open();
  h.f.open();
  assert.equal(h.timers.live(), 1, "a second open leaked a ticker");
});

test("destroy stops the ticker and releases the listeners", async () => {
  const h = setup({ [MINT]: () => mintOK() });
  h.f.open();
  assert.equal(h.timers.live(), 1);
  assert.ok(h.doc.el("#pair_x").listenerCount("click") > 0, "the ✕ was never bound");

  h.f.destroy();
  await h.f.settled();
  assert.equal(h.timers.live(), 0, "destroy left a timer running");
  assert.equal(h.doc.el("#pair_x").listenerCount("click"), 0, "destroy left the ✕ bound");
  assert.equal(h.doc.el("#pair_actions").listenerCount("click"), 0, "destroy left the action row bound");
});

/* ---------- HTTP -> events ---------- */

test("acknowledging the warning mints exactly one code and shows it", async () => {
  const h = setup({ [MINT]: () => mintOK() });
  await toShowCode(h);

  assert.deepEqual(h.api.keys(), [MINT], "the refresh request was not fulfilled exactly once");
  assert.equal(h.f.screen(), "show-code");
  assert.match(h.doc.el("#pair_title").textContent, /scan this/i, "the screen's title never reached its slot");
  assert.match(h.doc.el("#pair_qr").innerHTML, /^<svg /, "no QR was rendered for the minted link");
});

test("a satisfied refresh is not re-requested on the next tick", async () => {
  const h = setup({ [MINT]: () => mintOK(), [POLL]: () => ({ state: "pending" }) });
  await toShowCode(h);
  h.timers.fire();
  await h.f.settled();
  assert.equal(h.api.calls.filter((c) => c.key === MINT).length, 1, "a live code was minted over");
});

test("an expired code is replaced on the tick that notices", async () => {
  let n = 0;
  const h = setup({
    [MINT]: () => mintOK({ code: CODE, expires_at: new Date(T0 + 60_000).toISOString() }),
    [POLL]: () => ({ state: "pending" }),
  });
  h.at(T0);
  await toShowCode(h);
  n = h.api.calls.filter((c) => c.key === MINT).length;
  assert.equal(n, 1);

  h.at(T0 + 60_000);
  h.timers.fire();
  await h.f.settled();
  assert.equal(
    h.api.calls.filter((c) => c.key === MINT).length,
    2,
    "the dead code was left on screen",
  );
});

test("ticks during an in-flight mint do not queue a second one", async () => {
  /* A mint is a network call and the ticker does not wait for it. Without
   * a single-flight guard a slow rendezvous collects one code per second,
   * and the last one to land is the one on screen — which is not the one
   * the human scanned. */
  let release;
  const gate = new Promise((r) => { release = r; });
  const h = setup({ [MINT]: async () => { await gate; return mintOK(); } });
  h.at(T0);
  h.f.open();
  h.doc.el("#pair_actions").click("begin");
  h.doc.el("#pair_actions").click("ack");

  h.timers.fire();
  h.timers.fire();
  release();
  await h.f.settled();

  assert.equal(h.api.calls.filter((c) => c.key === MINT).length, 1, "the mint was requested more than once");
});

test("a screen waiting on its first code polls no session", async () => {
  /* show-code is reached before MINTED lands. There is no session id yet,
   * so a poll here can only be a request for /api/remote/pairing/ — a
   * URL that identifies nothing. */
  let release;
  const gate = new Promise((r) => { release = r; });
  const h = setup({ [MINT]: async () => { await gate; return mintOK(); } });
  h.f.open();
  h.doc.el("#pair_actions").click("begin");
  h.doc.el("#pair_actions").click("ack");
  h.timers.fire();
  assert.equal(h.f.screen(), "show-code");
  assert.equal(h.f.code(), "", "the fixture already had a code");

  release();
  await h.f.settled();
  assert.deepEqual(h.api.keys(), [MINT], "a codeless screen polled for a session");
});

test("a mint that fails becomes a failed screen carrying the message", async () => {
  const h = setup({ [MINT]: () => { throw new Error("rendezvous unreachable"); } });
  await toShowCode(h);
  assert.equal(h.f.screen(), "failed");
  assert.match(h.doc.el("#pair_body").innerHTML, /rendezvous unreachable/);
});

test("a 409 from the mint is the hosted refusal, not a generic failure", async () => {
  /* A paired device asking to pair further devices. The distinction is the
   * whole point: one tells the human to go to the laptop, the other tells
   * them the network is down. */
  const h = setup({ [MINT]: () => { throw httpError(409, '{"hosted":"device"}'); } });
  await toShowCode(h);
  assert.equal(h.f.screen(), "failed");
  assert.match(h.doc.el("#pair_body").innerHTML, /laptop/i);
});

test("a polled SAS moves the sheet to the comparison and stops showing the code", async () => {
  const h = setup({
    [MINT]: () => mintOK(),
    [POLL]: () => ({ state: "pending", sas: "706990" }),
  });
  await toShowCode(h);
  h.timers.fire();
  await h.f.settled();

  assert.equal(h.f.screen(), "compare-sas");
  assert.equal(h.doc.el("#pair_sas").textContent, "706990");
  assert.equal(h.doc.el("#pair_qr").innerHTML, "", "the code stayed on screen after it was scanned");
});

test("the poll stops once there is nothing left to learn", async () => {
  const h = setup({ [MINT]: () => mintOK(), [POLL]: () => ({ state: "pending" }) });
  await toShowCode(h);
  h.timers.fire();
  await h.f.settled();
  const polled = h.api.calls.filter((c) => c.key === POLL).length;
  assert.equal(polled, 1, "the open sheet did not poll its own session");

  h.f.close();
  await h.f.settled();
  h.timers.fire();
  await h.f.settled();
  assert.equal(
    h.api.calls.filter((c) => c.key === POLL).length,
    polled,
    "a closed sheet kept polling a pairing session",
  );
});

test("confirming sends both halves and completes on success", async () => {
  const h = setup({
    [MINT]: () => mintOK(),
    [POLL]: () => ({ state: "pending", sas: "706990" }),
    [CONFIRM]: () => ({ id: "dev-1", rid: "b".repeat(64), label: "iPhone" }),
  });
  await toShowCode(h);
  h.timers.fire();
  await h.f.settled();

  h.doc.el("#pair_actions").click("confirm");
  await h.f.settled();

  const body = h.api.calls.find((c) => c.key === CONFIRM).body;
  assert.deepEqual(body, { laptop_confirm: true, device_confirm: true }, "FR-12 needs both halves");
  assert.equal(h.f.screen(), "succeeded");
});

test("a confirm the other side has not matched leaves the sheet waiting", async () => {
  /* 409 is ClassPairUnconfirmed: the laptop's half is recorded, the
   * device's is not. Treating that as a failure would throw away a
   * pairing that is one tap from finishing. */
  const h = setup({
    [MINT]: () => mintOK(),
    [POLL]: () => ({ state: "pending", sas: "706990" }),
    [CONFIRM]: () => { throw httpError(409, "pairing not confirmed on both sides"); },
  });
  await toShowCode(h);
  h.timers.fire();
  await h.f.settled();
  h.doc.el("#pair_actions").click("confirm");
  await h.f.settled();

  assert.equal(h.f.screen(), "awaiting-other-side");
  assert.equal(h.f.error(), "", "a routine wait was reported as an error");
});

test("digits that do not match cancel the session on the server too", async () => {
  const h = setup({
    [MINT]: () => mintOK(),
    [POLL]: () => ({ state: "pending", sas: "706990" }),
    [CANCEL]: () => "",
  });
  await toShowCode(h);
  h.timers.fire();
  await h.f.settled();
  h.doc.el("#pair_actions").click("reject");
  await h.f.settled();

  assert.equal(h.f.screen(), "cancelled");
  assert.ok(h.api.keys().includes(CANCEL), "a rejected code was left live on the rendezvous");
});

test("the device's own confirmation reaches the waiting screen", async () => {
  /* FR-12's second half. Until it arrives the laptop cannot say the
   * pairing is nearly done, and the human is left reading "confirm on
   * your device" after they already have. */
  let confirmed = false;
  const h = setup({
    [MINT]: () => mintOK(),
    [POLL]: () => ({ state: "pending", sas: "706990", device_confirmed: confirmed }),
    [CONFIRM]: () => { throw httpError(409, "not both sides"); },
  });
  await toShowCode(h);
  h.timers.fire();
  await h.f.settled();
  h.doc.el("#pair_actions").click("confirm");
  await h.f.settled();
  assert.match(h.doc.el("#pair_body").innerHTML, /confirm the same numbers/i);

  confirmed = true;
  h.timers.fire();
  await h.f.settled();
  assert.equal(h.f.screen(), "awaiting-other-side", "the device's confirmation completed the pairing on its own");
  assert.match(h.doc.el("#pair_body").innerHTML, /finishing up/i);
});

/* ---------- DOM -> events ---------- */

test("cancelling before a code exists asks the server for nothing", async () => {
  const h = setup({ [MINT]: () => mintOK() });
  h.f.open();
  h.doc.el("#pair_actions").click("begin");
  h.doc.el("#pair_actions").click("cancel");
  await h.f.settled();
  assert.equal(h.f.screen(), "pair-a-device");
  assert.deepEqual(h.api.keys(), [], "backing out of the warning reached the network");
});

test("Done on the success screen closes without cancelling anything", async () => {
  const h = setup({
    [MINT]: () => mintOK(),
    [POLL]: () => ({ state: "pending", sas: "706990" }),
    [CONFIRM]: () => ({ id: "dev-1" }),
  });
  await toShowCode(h);
  h.timers.fire();
  await h.f.settled();
  h.doc.el("#pair_actions").click("confirm");
  await h.f.settled();
  assert.equal(h.f.screen(), "succeeded");

  h.doc.el("#pair_actions").click("close");
  await h.f.settled();
  assert.equal(h.f.screen(), "closed");
  assert.equal(h.f.isOpen(), false);
  assert.equal(
    h.api.keys().filter((k) => k === CANCEL).length,
    0,
    "closing a completed pairing tried to cancel the code it had just consumed",
  );
  assert.equal(h.timers.live(), 0, "the ticker outlived the finished flow");
});

test("the ✕ cancels at every live stage, and always tells the server", async () => {
  for (const stage of ["show-code", "compare-sas", "awaiting-other-side"]) {
    const h = setup({
      [MINT]: () => mintOK(),
      [POLL]: () => ({ state: "pending", sas: "706990" }),
      [CONFIRM]: () => { throw httpError(409, "not both sides"); },
      [CANCEL]: () => "",
    });
    await toShowCode(h);
    if (stage !== "show-code") {
      h.timers.fire();
      await h.f.settled();
    }
    if (stage === "awaiting-other-side") {
      h.doc.el("#pair_actions").click("confirm");
      await h.f.settled();
    }
    assert.equal(h.f.screen(), stage, `${stage}: setup`);

    h.doc.el("#pair_x").click();
    await h.f.settled();
    assert.equal(h.f.screen(), "closed", `${stage}: the ✕ did not dismiss the sheet`);
    assert.ok(h.api.keys().includes(CANCEL), `${stage}: the code was abandoned rather than cancelled`);
  }
});

test("cancelling a code that has already died is not an error", async () => {
  /* 410 is ClassPairExpired. The session the cancel was for is gone, which
   * is the state the cancel was asking for; surfacing it would put an
   * error on a screen the human just dismissed. */
  const h = setup({
    [MINT]: () => mintOK(),
    [CANCEL]: () => { throw httpError(410, "pairing code expired"); },
  });
  await toShowCode(h);
  h.doc.el("#pair_x").click();
  await h.f.settled();

  assert.equal(h.f.screen(), "closed");
  assert.equal(h.f.error(), "", "an expired cancel was reported to the human");
});

test("a cancel that genuinely fails still closes the sheet", async () => {
  const h = setup({
    [MINT]: () => mintOK(),
    [CANCEL]: () => { throw new Error("network down"); },
  });
  await toShowCode(h);
  h.doc.el("#pair_x").click();
  await h.f.settled();
  assert.equal(h.f.screen(), "closed", "a failed cancel trapped the human in the sheet");
});

test("the sheet resists dismissal exactly while a credential is live", async () => {
  /* The backdrop and the swipe close every other sheet. Losing this one
   * mid-comparison abandons a live code with no cancel behind it. */
  const h = setup({
    [MINT]: () => mintOK(),
    [POLL]: () => ({ state: "pending", sas: "706990" }),
    [CANCEL]: () => "",
  });
  h.f.open();
  assert.equal(h.f.dismissable(), true, "the opening screen holds nothing to protect");

  h.doc.el("#pair_actions").click("begin");
  h.doc.el("#pair_actions").click("ack");
  await h.f.settled();
  assert.equal(h.f.dismissable(), false, "a live code could be swiped away");

  h.timers.fire();
  await h.f.settled();
  assert.equal(h.f.dismissable(), false, "a comparison in progress could be swiped away");

  h.doc.el("#pair_x").click();
  await h.f.settled();
  assert.equal(h.f.dismissable(), true, "the closed sheet stayed undismissable");
});

/* ---------- state -> DOM ---------- */

test("the live region is polite and speaks only when there is something new", async () => {
  const h = setup({ [MINT]: () => mintOK(), [POLL]: () => ({ state: "pending", sas: "706990" }) });
  const live = h.doc.el("#pair_live");
  assert.equal(live.getAttribute("aria-live"), "polite", "an assertive region interrupts mid-digit");

  h.f.open();
  assert.equal(live.textContent, "", "the opening screen announced something");

  h.doc.el("#pair_actions").click("begin");
  h.doc.el("#pair_actions").click("ack");
  await h.f.settled();
  assert.match(live.textContent, /pairing code/i, "the minted code was never announced");

  /* A render with nothing to say must leave the region alone: writing ""
   * into it truncates whatever is being read out. compare-sas announces
   * nothing, because the digits are on screen to be compared, not read. */
  h.timers.fire();
  await h.f.settled();
  assert.equal(h.f.screen(), "compare-sas");
  assert.match(live.textContent, /pairing code/i, "a silent render wiped the live region");
});

test("every action the view offers is a button the delegation can reach", async () => {
  const h = setup({ [MINT]: () => mintOK() });
  h.f.open();
  const html = h.doc.el("#pair_actions").innerHTML;
  assert.match(html, /data-pa="begin"/, "the view's action id reached no attribute");
  assert.match(html, /<button/, "actions rendered as something unclickable");
  assert.match(html, /class="primary"/, "the view's primary action rendered as an ordinary button");
});

test("an unknown action id does nothing", async () => {
  const h = setup({ [MINT]: () => mintOK() });
  h.f.open();
  h.doc.el("#pair_actions").click("definitely-not-an-action");
  await h.f.settled();
  assert.equal(h.f.screen(), "pair-a-device");
  assert.deepEqual(h.api.keys(), [], "an unknown tap reached the network");
});

test("the sheet is hidden when closed and shown when open", async () => {
  const h = setup({ [MINT]: () => mintOK() });
  const sheet = h.doc.el("#pairsheet");
  assert.equal(sheet.hidden, true, "a closed sheet was left in the page");
  h.f.open();
  assert.equal(sheet.hidden, false);
  h.f.close();
  await h.f.settled();
  assert.equal(sheet.hidden, true);
});

test("visibility changes reach the reducer", async () => {
  /* The pause that keeps an unattended laptop from cycling credentials is
   * only real if the adapter forwards the event. */
  const h = setup({ [MINT]: () => mintOK({ expires_at: new Date(T0 + 60_000).toISOString() }) });
  h.at(T0);
  await toShowCode(h);

  h.f.setVisible(false);
  h.at(T0 + 60_000);
  h.timers.fire();
  await h.f.settled();

  assert.equal(h.f.screen(), "expired", "a hidden tab kept a dead code on screen");
  assert.equal(
    h.api.calls.filter((c) => c.key === MINT).length,
    1,
    "an unwatched screen minted a fresh credential",
  );

  h.f.setVisible(true);
  await h.f.settled();
  assert.equal(h.f.screen(), "show-code");
  assert.equal(
    h.api.calls.filter((c) => c.key === MINT).length,
    2,
    "returning to the tab left a dead screen to poke at",
  );
});
