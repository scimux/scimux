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
    value: "",
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
const LINK = "https://my.scimux.com/p#c=04106105";

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
const SAS = "706990";

/* Confirming is now transcription: the digits are read off the device and
 * typed into the field before the button means anything. */
function typeAndConfirm(h, digits = SAS) {
  h.doc.el("#pair_sas").value = digits;
  h.doc.el("#pair_actions").click("confirm");
}

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
   * whole point: one tells the human to go to the computer, the other tells
   * them the network is down. */
  const h = setup({ [MINT]: () => { throw httpError(409, '{"hosted":"device"}'); } });
  await toShowCode(h);
  assert.equal(h.f.screen(), "failed");
  assert.match(h.doc.el("#pair_body").innerHTML, /computer/i);
});

/* The refusal grew: 409 is now also how a revoked, disabled or unenrolled
 * computer says no (internal/app/remote_pairing.go). Those need the server's
 * own sentence — telling someone at the computer to go to the computer is
 * worse than saying nothing, and "not enrolled" is the one fact that ends
 * the confusion. */
test("a 409 that carries a sentence shows that sentence, not the device wording", async () => {
  const refusal = "This installation is not enrolled with a rendezvous, so there is nowhere for a device to meet it.";
  const h = setup({
    [MINT]: () => { throw httpError(409, JSON.stringify({ hosted: "", error: refusal })); },
  });
  await toShowCode(h);
  assert.equal(h.f.screen(), "failed");
  assert.match(h.doc.el("#pair_body").innerHTML, /not enrolled with a rendezvous/);
  assert.doesNotMatch(h.doc.el("#pair_body").innerHTML, /pair further devices/);
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
  assert.equal(h.doc.el("#pair_sas").hidden, false, "nowhere to type the digits the device is showing");
  assert.equal(h.doc.el("#pair_sas").value, "", "the computer filled in the digits it is asking for");
  assert.ok(
    !h.doc.el("#pair_body").innerHTML.includes(SAS),
    "the computer put the digits on its own screen",
  );
  assert.equal(h.doc.el("#pair_qr").innerHTML, "", "the code stayed on screen after it was scanned");
});

test("the digits field is only on the screen that asks for them", async () => {
  const h = setup({ [MINT]: () => mintOK(), [POLL]: () => ({ state: "pending" }) });
  assert.equal(h.doc.el("#pair_sas").hidden, true, "a closed sheet showed the entry field");
  await toShowCode(h);
  assert.equal(h.doc.el("#pair_sas").hidden, true, "the code screen showed the entry field");
});

test("wrong digits stay on the comparison and never reach the server", async () => {
  /* The whole point of typing them: the completion call is not reachable
     from a screen where the human could not produce the digits. */
  const h = setup({
    [MINT]: () => mintOK(),
    [POLL]: () => ({ state: "pending", sas: SAS }),
    [CANCEL]: () => "",
  });
  await toShowCode(h);
  h.timers.fire();
  await h.f.settled();

  typeAndConfirm(h, "123456");
  await h.f.settled();

  assert.equal(h.f.screen(), "compare-sas");
  assert.equal(h.api.keys().filter((k) => k === CONFIRM).length, 0, "a wrong entry was sent to the server");
  assert.match(h.doc.el("#pair_body").innerHTML.toLowerCase(), /digits/);
  assert.equal(h.doc.el("#pair_sas").hidden, false, "the field went away after a mistype");
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

  typeAndConfirm(h);
  await h.f.settled();

  const body = h.api.calls.find((c) => c.key === CONFIRM).body;
  assert.deepEqual(body, { computer_confirm: true, device_confirm: true }, "FR-12 needs both halves");
  assert.equal(h.f.screen(), "succeeded");
});

test("a rejected completion is shown instead of waiting forever", async () => {
  const h = setup({
    [MINT]: () => mintOK(),
    [POLL]: () => ({ state: "pending", sas: "706990" }),
    [CONFIRM]: () => { throw httpError(409, "pairing not confirmed on both sides"); },
  });
  await toShowCode(h);
  h.timers.fire();
  await h.f.settled();
  typeAndConfirm(h);
  await h.f.settled();

  assert.equal(h.f.screen(), "failed");
  assert.match(h.doc.el("#pair_title").textContent, /could not finish/i);
  assert.match(h.doc.el("#pair_body").innerHTML, /nothing was paired/i);
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

test("a polled device confirmation never skips the computer comparison", async () => {
  const h = setup({
    [MINT]: () => mintOK(),
    [POLL]: () => ({ state: "pending", sas: "706990", device_confirmed: true }),
  });
  await toShowCode(h);
  h.timers.fire();
  await h.f.settled();
  assert.equal(h.f.screen(), "compare-sas");
  assert.match(h.doc.el("#pair_title").textContent, /type the digits/i);
});

/* ---------- DOM -> events ---------- */

test("cancelling before a code exists asks the server for nothing", async () => {
  const h = setup({ [MINT]: () => mintOK() });
  h.f.open();
  h.doc.el("#pair_actions").click("cancel");
  await h.f.settled();
  assert.equal(h.f.screen(), "closed");
  assert.deepEqual(h.api.keys(), [], "backing out of the warning reached the network");
});

/* Opening the sheet used to land on a screen with a title, no prose and
   a single button repeating the menu item that had just been tapped. It
   asked for a tap without offering a decision, so the first thing the
   sheet shows is now the screen that does. */
test("opening the sheet shows the authority warning, not a screen to tap through", async () => {
  const h = setup({ [MINT]: () => mintOK() });
  h.f.open();
  assert.equal(h.f.screen(), "authority-warning");
  assert.match(h.doc.el("#pair_body").innerHTML, /anything you can do at this computer/i);
  const actions = h.doc.el("#pair_actions").innerHTML;
  assert.doesNotMatch(actions, /data-pa="begin"/, "the sheet still opens on a screen that only begins");
  await h.f.settled();
  assert.deepEqual(h.api.keys(), [], "opening the sheet reached the network");
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
  typeAndConfirm(h);
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
    let finishConfirm;
    const h = setup({
      [MINT]: () => mintOK(),
      [POLL]: () => ({ state: "pending", sas: "706990" }),
      [CONFIRM]: () => new Promise((resolve) => { finishConfirm = resolve; }),
      [CANCEL]: () => "",
    });
    await toShowCode(h);
    if (stage !== "show-code") {
      h.timers.fire();
      await h.f.settled();
    }
    if (stage === "awaiting-other-side") {
      typeAndConfirm(h);
      await Promise.resolve();
      assert.equal(typeof finishConfirm, "function", "confirm request did not start");
    }
    assert.equal(h.f.screen(), stage, `${stage}: setup`);

    h.doc.el("#pair_x").click();
    if (finishConfirm) finishConfirm({ id: "dev-1" });
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
  const h = setup({
    [MINT]: () => mintOK(),
    [POLL]: () => ({ state: "pending", sas: SAS }),
    [CONFIRM]: () => ({ id: "dev-1" }),
  });
  const live = h.doc.el("#pair_live");
  assert.equal(live.getAttribute("aria-live"), "polite", "an assertive region interrupts mid-digit");

  h.f.open();
  assert.equal(live.textContent, "", "the opening screen announced something");

  h.doc.el("#pair_actions").click("begin");
  h.doc.el("#pair_actions").click("ack");
  await h.f.settled();
  assert.match(live.textContent, /pairing code/i, "the minted code was never announced");

  /* compare-sas does announce: it puts a field on screen and a screen
   * reader user has no other way to learn what goes in it. */
  h.timers.fire();
  await h.f.settled();
  assert.equal(h.f.screen(), "compare-sas");
  assert.match(live.textContent, /six digits/i, "a field appeared with no instruction to read");

  /* A render with nothing to say must leave the region alone: writing ""
   * into it truncates whatever is being read out. */
  typeAndConfirm(h);
  await h.f.settled();
  assert.equal(h.f.screen(), "succeeded");
  assert.match(live.textContent, /six digits/i, "a silent render wiped the live region");
});

test("every action the view offers is a button the delegation can reach", async () => {
  const h = setup({ [MINT]: () => mintOK() });
  h.f.open();
  const html = h.doc.el("#pair_actions").innerHTML;
  assert.match(html, /data-pa="ack"/, "the view's action id reached no attribute");
  assert.match(html, /<button/, "actions rendered as something unclickable");
  assert.match(html, /class="primary"/, "the view's primary action rendered as an ordinary button");
});

test("an unknown action id does nothing", async () => {
  const h = setup({ [MINT]: () => mintOK() });
  h.f.open();
  h.doc.el("#pair_actions").click("definitely-not-an-action");
  await h.f.settled();
  assert.equal(h.f.screen(), "authority-warning");
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
  /* The pause that keeps an unattended computer from cycling credentials is
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

/* ---------- clipboard -> events ---------- */

test("copy starts synchronously in the click with the exact QR link", async () => {
  const calls = [];
  const h = setup({ [MINT]: () => mintOK() }, {
    copy: (text) => { calls.push(text); return Promise.resolve(); },
  });
  await toShowCode(h);
  h.doc.el("#pair_actions").click("copy");
  assert.deepEqual(calls, [LINK], "the clipboard call waited beyond the click's user activation");
  await h.f.settled();
});

test("a successful copy is announced and leaves the QR available", async () => {
  const h = setup({ [MINT]: () => mintOK() }, { copy: () => Promise.resolve() });
  await toShowCode(h);
  const qr = h.doc.el("#pair_qr").innerHTML;
  h.doc.el("#pair_actions").click("copy");
  await h.f.settled();
  assert.match(h.doc.el("#pair_body").innerHTML, /Link copied\./);
  assert.equal(h.doc.el("#pair_live").textContent, "Pairing link copied.");
  assert.ok(h.doc.el("#pair_qr").innerHTML.length > 0);
  assert.equal(h.doc.el("#pair_qr").innerHTML, qr);
});

test("a rejected clipboard promise reports failure without an unhandled rejection", async () => {
  const h = setup({ [MINT]: () => mintOK() }, {
    copy: () => Promise.reject(new Error("denied")),
  });
  await toShowCode(h);
  h.doc.el("#pair_actions").click("copy");
  await h.f.settled();
  assert.match(h.doc.el("#pair_body").innerHTML, /Could not copy the link\./);
  assert.equal(h.doc.el("#pair_live").textContent, "Could not copy the pairing link.");
});

test("a synchronous clipboard throw reports failure", async () => {
  const h = setup({ [MINT]: () => mintOK() }, {
    copy: () => { throw new Error("denied"); },
  });
  await toShowCode(h);
  h.doc.el("#pair_actions").click("copy");
  await h.f.settled();
  assert.match(h.doc.el("#pair_body").innerHTML, /Could not copy the link\./);
  assert.equal(h.doc.el("#pair_live").textContent, "Could not copy the pairing link.");
});

test("a missing clipboard injection reports failure without throwing", async () => {
  const h = setup({ [MINT]: () => mintOK() });
  await toShowCode(h);
  assert.doesNotThrow(() => h.doc.el("#pair_actions").click("copy"));
  await h.f.settled();
  assert.match(h.doc.el("#pair_body").innerHTML, /Could not copy the link\./);
  assert.equal(h.doc.el("#pair_live").textContent, "Could not copy the pairing link.");
});

test("a copy tap before a link exists never calls the clipboard", async () => {
  const calls = [];
  const h = setup({ [MINT]: () => mintOK() }, { copy: (text) => { calls.push(text); } });
  h.f.open();
  h.doc.el("#pair_actions").click("copy");
  assert.deepEqual(calls, []);
  assert.equal(h.f.screen(), "authority-warning");
  await h.f.settled();
  assert.deepEqual(h.api.keys(), []);
});

test("a copy outcome queued behind a refresh is dropped for the replaced link", async () => {
  const LINK2 = "https://my.scimux.com/p#c=04106106";
  const calls = [];
  let release;
  let refreshing = false;
  const gate = new Promise((resolve) => { release = resolve; });
  const h = setup({
    [MINT]: () => mintOK({ expires_at: new Date(T0 + 60_000).toISOString() }),
    [POLL]: () => ({ state: "pending" }),
    "GET /api/remote/pairing/04106106": () => ({ state: "pending" }),
  }, { copy: (text) => { calls.push(text); return Promise.resolve(); } });
  await toShowCode(h);
  const oldQR = h.doc.el("#pair_qr").innerHTML;
  h.api.routes[MINT] = () => { refreshing = true; return gate; };
  h.at(T0 + 60_001);
  h.timers.fire();
  await Promise.resolve();
  assert.equal(refreshing, true, "the refresh mint must be in flight before the copy tap");
  h.doc.el("#pair_actions").click("copy");
  release(mintOK({ code: "04106106", link: LINK2 }));
  await h.f.settled();
  assert.deepEqual(calls, [LINK]);
  assert.doesNotMatch(h.doc.el("#pair_body").innerHTML, /Link copied\./);
  assert.notEqual(h.doc.el("#pair_qr").innerHTML, oldQR, "the QR still encodes the old link");
  const { pairingView, initialPairingState } = await import("../js/pairing.js");
  assert.equal(h.doc.el("#pair_qr").innerHTML, pairingView({
    ...initialPairingState(), screen: "show-code", code: "04106106", link: LINK2,
  }).qr);
  h.timers.fire();
  await h.f.settled();
  assert.ok(h.api.keys().includes("GET /api/remote/pairing/04106106"));
});

test("refresh clears the message about a successful copy of the earlier link", async () => {
  const h = setup({
    [MINT]: () => mintOK({ expires_at: new Date(T0 + 60_000).toISOString() }),
    [POLL]: () => ({ state: "pending" }),
    "GET /api/remote/pairing/04106106": () => ({ state: "pending" }),
  }, { copy: () => Promise.resolve() });
  await toShowCode(h);
  h.doc.el("#pair_actions").click("copy");
  await h.f.settled();
  assert.match(h.doc.el("#pair_body").innerHTML, /Link copied\./);
  h.api.routes[MINT] = () => mintOK({ code: "04106106", link: "https://my.scimux.com/p#c=04106106" });
  h.at(T0 + 60_001);
  h.timers.fire();
  await h.f.settled();
  assert.equal(h.doc.el("#pair_body").innerHTML, "The code is valid for a short time and refreshes itself while this stays open.");
  assert.equal(h.doc.el("#pair_live").textContent, "New pairing code ready to scan.");
});

test("the copy button is rendered only after minting supplies a link", async () => {
  let release;
  const gate = new Promise((resolve) => { release = resolve; });
  const h = setup({ [MINT]: () => gate });
  h.f.open();
  h.doc.el("#pair_actions").click("ack");
  await Promise.resolve();
  assert.equal(h.f.screen(), "show-code");
  assert.equal(h.f.code(), "");
  assert.doesNotMatch(h.doc.el("#pair_actions").innerHTML, /data-pa="copy"/);
  release(mintOK());
  await h.f.settled();
  assert.match(h.doc.el("#pair_actions").innerHTML, /data-pa="copy"/);
  assert.match(h.doc.el("#pair_actions").innerHTML, />Copy link</);
});

test("a clipboard function returning undefined counts as success", async () => {
  const calls = [];
  const h = setup({ [MINT]: () => mintOK() }, { copy: (text) => { calls.push(text); } });
  await toShowCode(h);
  h.doc.el("#pair_actions").click("copy");
  await h.f.settled();
  assert.deepEqual(calls, [LINK]);
  assert.equal(h.doc.el("#pair_live").textContent, "Pairing link copied.");
});

test("a rejected copy is handled immediately even behind a blocked poll", async () => {
  let release;
  const gate = new Promise((resolve) => { release = resolve; });
  const h = setup({ [MINT]: () => mintOK(), [POLL]: () => gate }, {
    copy: () => Promise.reject(new Error("denied")),
  });
  await toShowCode(h);
  h.timers.fire();
  await Promise.resolve();
  h.doc.el("#pair_actions").click("copy");
  /* A full event-loop turn lets Node surface an unhandled rejection while
     the work queue is still waiting on the poll. */
  await new Promise((resolve) => setImmediate(resolve));
  release({ state: "pending" });
  await h.f.settled();
  assert.equal(h.doc.el("#pair_live").textContent, "Could not copy the pairing link.");
});

/* ---------- stable action buttons ---------- */

function countHTMLWrites(element, readBack = (html) => html) {
  let stored = element.innerHTML;
  let writes = 0;
  Object.defineProperty(element, "innerHTML", {
    configurable: true,
    get: () => readBack(stored),
    set(html) {
      stored = html;
      writes++;
    },
  });
  return () => writes;
}

/* Replacing a pressed button makes the browser retarget the click to the
   actions container, which carries no data-pa for the delegation to find. */
function retargetingPointer(element, writes) {
  let action;
  let pressedAt;
  return {
    press(nextAction) {
      action = nextAction;
      pressedAt = writes();
    },
    release() {
      element.click(writes() === pressedAt ? action : null);
    },
  };
}

test("a tick that changes nothing keeps the show-code buttons", async () => {
  const h = setup({ [MINT]: () => mintOK(), [POLL]: () => ({ state: "pending" }) });
  const actions = h.doc.el("#pair_actions");
  const writes = countHTMLWrites(actions);
  await toShowCode(h);
  const shownAt = writes();
  for (let i = 0; i < 5; i++) {
    h.advance(1000);
    h.timers.fire();
    await h.f.settled();
  }
  assert.equal(writes(), shownAt, "unchanged ticks rebuilt the show-code buttons");
  assert.match(actions.innerHTML, /data-pa="copy"/);
  assert.match(actions.innerHTML, /data-pa="cancel"/);
});

test("a click pressed before a tick still lands after it", async () => {
  const calls = [];
  const h = setup({ [MINT]: () => mintOK(), [POLL]: () => ({ state: "pending" }) }, {
    copy: (text) => { calls.push(text); return Promise.resolve(); },
  });
  const actions = h.doc.el("#pair_actions");
  const writes = countHTMLWrites(actions);
  const pointer = retargetingPointer(actions, writes);
  await toShowCode(h);
  pointer.press("copy");
  h.timers.fire();
  await h.f.settled();
  pointer.release();
  assert.deepEqual(calls, [LINK], "a rebuild retargeted the released click and lost the copy");
  await h.f.settled();
});

test("the compare-sas buttons survive ticks and repeated offers", async () => {
  const h = setup({
    [MINT]: () => mintOK(),
    [POLL]: () => ({ state: "pending", sas: SAS }),
  });
  const actions = h.doc.el("#pair_actions");
  const writes = countHTMLWrites(actions);
  await toShowCode(h);
  h.timers.fire();
  await h.f.settled();
  assert.equal(h.f.screen(), "compare-sas");
  const shownAt = writes();
  for (let i = 0; i < 3; i++) {
    h.advance(1000);
    h.timers.fire();
    await h.f.settled();
  }
  assert.equal(writes(), shownAt, "ticks and repeated offers rebuilt the comparison buttons");
  assert.match(actions.innerHTML, /data-pa="confirm"/);
  assert.match(actions.innerHTML, /data-pa="reject"/);
});

test("a copy outcome changes the message but not the buttons", async () => {
  const h = setup({ [MINT]: () => mintOK() }, { copy: () => Promise.resolve() });
  const actions = h.doc.el("#pair_actions");
  const writes = countHTMLWrites(actions);
  await toShowCode(h);
  const shownAt = writes();
  actions.click("copy");
  await h.f.settled();
  assert.match(h.doc.el("#pair_body").innerHTML, /Link copied\./);
  assert.equal(writes(), shownAt, "copy feedback rebuilt unchanged buttons");
});

test("the buttons are still rebuilt when they change", async () => {
  let release;
  const gate = new Promise((resolve) => { release = resolve; });
  const h = setup({ [MINT]: () => gate, [CANCEL]: () => "" });
  const actions = h.doc.el("#pair_actions");
  const writes = countHTMLWrites(actions);

  h.f.open();
  assert.equal(writes(), 1);
  assert.match(actions.innerHTML, /data-pa="ack"/);

  actions.click("ack");
  await Promise.resolve();
  assert.equal(h.f.screen(), "show-code");
  assert.equal(h.f.code(), "");
  assert.deepEqual(h.api.keys(), [MINT], "the fixture must still be waiting on its mint");
  assert.equal(writes(), 2);
  assert.match(actions.innerHTML, /data-pa="cancel"/);
  assert.doesNotMatch(actions.innerHTML, /data-pa="copy"/);
  assert.deepEqual([...actions.innerHTML.matchAll(/data-pa="([^"]+)"/g)].map((m) => m[1]), ["cancel"]);

  release(mintOK());
  await h.f.settled();
  assert.equal(writes(), 3);
  assert.match(actions.innerHTML, /data-pa="copy"/);
  assert.match(actions.innerHTML, /data-pa="cancel"/);

  actions.click("cancel");
  await h.f.settled();
  assert.equal(h.f.screen(), "cancelled");
  assert.equal(writes(), 4);
  assert.match(actions.innerHTML, /data-pa="begin"/);
  assert.match(actions.innerHTML, /data-pa="close"/);

  actions.click("close");
  await h.f.settled();
  assert.equal(h.f.screen(), "closed");
  assert.equal(writes(), 5);
  assert.equal(actions.innerHTML, "");

  h.f.open();
  assert.equal(writes(), 6);
  assert.match(actions.innerHTML, /data-pa="ack"/);
});

test("a missing actions slot never throws and never marks buttons as shown", async () => {
  const h = setup({ [MINT]: () => mintOK(), [POLL]: () => ({ state: "pending" }) });
  /* Keep the bound element aside so the ack tap can reach the adapter while
     its queried slot is absent throughout open, ack and mint. */
  const actions = h.doc.el("#pair_actions");
  const writes = countHTMLWrites(actions);
  h.doc.els.delete("#pair_actions");
  assert.doesNotThrow(() => h.f.open());
  assert.doesNotThrow(() => actions.click("ack"));
  await h.f.settled();
  assert.equal(h.f.screen(), "show-code");
  assert.equal(h.f.code(), CODE);
  assert.equal(writes(), 0, "a missing slot should receive no writes");

  h.doc.els.set("#pair_actions", actions);
  h.timers.fire();
  await h.f.settled();
  assert.equal(writes(), 1, "the skipped markup was incorrectly remembered as shown");
  assert.match(actions.innerHTML, /data-pa="copy"/);
  assert.match(actions.innerHTML, /data-pa="cancel"/);
});

test("browser serialization does not cause unchanged actions to be rebuilt", async () => {
  const h = setup({ [MINT]: () => mintOK(), [POLL]: () => ({ state: "pending" }) });
  const actions = h.doc.el("#pair_actions");
  /* A valid re-serialization differing from the assigned markup makes a
     comparison against DOM read-back fail even though the buttons agree. */
  const writes = countHTMLWrites(actions, (html) => html.replace(/="([^"]*)"/g, "='$1'"));
  await toShowCode(h);
  assert.match(actions.innerHTML, /data-pa='copy'/);
  const shownAt = writes();
  h.timers.fire();
  await h.f.settled();
  assert.equal(writes(), shownAt, "render compared against re-serialized DOM markup");
});
