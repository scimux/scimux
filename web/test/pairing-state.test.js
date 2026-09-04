/* P1 — the pairing state machine, pure.
 *
 * No DOM, no timers, no API. Everything here is a function of (state,
 * event, now), which is what makes the exhaustive table below affordable:
 * it asserts every screen against every event, including the illegal
 * pairs that must be no-ops. A pairing UI that silently accepts an event
 * in the wrong state is exactly how a live SAS comparison gets swapped
 * out from under the human, so "nothing happens" is a real assertion and
 * not filler.
 */
import test from "node:test";
import assert from "node:assert/strict";

import {
  PAIRING_SCREENS,
  PAIRING_EVENTS,
  initialPairingState,
  nextPairing,
  pairingView,
  authorityWarningHTML,
} from "../js/pairing.js";

const T0 = 1_000_000;
const TTL = 60_000;

/* A state per screen, built by hand rather than driven through the
 * machine: a table that reached its own start states via the reducer
 * could not fail when the reducer is wrong. */
function at(screen, over = {}) {
  const base = {
    ...initialPairingState(),
    screen,
    visible: true,
  };
  if (screen === "show-code" || screen === "compare-sas" || screen === "awaiting-other-side") {
    base.code = "04106105";
    base.rid = "3d3a9f69";
    base.link = "https://my.scimux.eu/p#c=04106105";
    base.expiresAt = T0 + TTL;
  }
  if (screen === "compare-sas" || screen === "awaiting-other-side") {
    base.sas = "706990";
  }
  if (screen === "awaiting-other-side") {
    base.computerConfirmed = true;
  }
  return { ...base, ...over };
}

const MINTED = { type: "MINTED", code: "04106105", rid: "3d3a9f69", link: "https://x/p#c=1", expiresAt: T0 + TTL };
const OFFER = { type: "OFFER", sas: "706990" };

/* The table. Rows are screens, columns are events, cells are the screen
 * the machine must be on afterwards. Unlisted pairs are no-ops and are
 * generated as such, so adding an event without deciding its behaviour
 * everywhere fails rather than defaulting. */
const TRANSITIONS = {
  /* Opening the sheet is the request to pair. It lands on the warning:
     the screen it used to land on carried a title, no prose and one
     button repeating the menu item that had just been tapped, so it
     asked for a tap without offering a decision. */
  closed: { OPEN: "authority-warning" },
  /* Cancel on the first screen of the sheet closes the sheet. There is
     no longer a screen behind it to go back to. */
  "authority-warning": { ACK_WARNING: "show-code", CANCEL: "closed", CLOSE: "closed", MINT_FAILED: "failed", HOSTED_BLOCKED: "failed" },
  "show-code": {
    MINTED: "show-code",
    MINT_FAILED: "failed",
    HOSTED_BLOCKED: "failed",
    OFFER: "compare-sas",
    TICK: "show-code",
    VISIBILITY: "show-code",
    CANCEL: "cancelled",
    CLOSE: "closed",
  },
  "compare-sas": {
    CONFIRM: "awaiting-other-side",
    DEVICE_CONFIRMED: "compare-sas",
    REJECT: "cancelled",
    CANCEL: "cancelled",
    TICK: "compare-sas",
    VISIBILITY: "compare-sas",
    CLOSE: "closed",
  },
  "awaiting-other-side": {
    DEVICE_CONFIRMED: "awaiting-other-side",
    COMPLETED: "succeeded",
    CONFIRM_FAILED: "failed",
    REJECT: "cancelled",
    CANCEL: "cancelled",
    TICK: "awaiting-other-side",
    VISIBILITY: "awaiting-other-side",
    CLOSE: "closed",
  },
  succeeded: { BEGIN: "authority-warning", CLOSE: "closed" },
  cancelled: { BEGIN: "authority-warning", CLOSE: "closed" },
  failed: { BEGIN: "authority-warning", CLOSE: "closed" },
  expired: { BEGIN: "authority-warning", CLOSE: "closed", VISIBILITY: "show-code" },
};

function eventFor(type) {
  if (type === "MINTED") return MINTED;
  if (type === "OFFER") return OFFER;
  if (type === "TICK") return { type: "TICK", now: T0 };
  if (type === "VISIBILITY") return { type: "VISIBILITY", visible: true };
  return { type };
}

test("the screen list and the transition table cover each other exactly", () => {
  /* Guards the table itself. Without this, adding a screen to the module
   * and forgetting the table would leave the exhaustive test below
   * quietly asserting less than it appears to. */
  assert.deepEqual([...PAIRING_SCREENS].sort(), Object.keys(TRANSITIONS).sort());
});

test("every screen x event pair lands where the table says, and nowhere else", () => {
  for (const screen of PAIRING_SCREENS) {
    for (const type of PAIRING_EVENTS) {
      const want = TRANSITIONS[screen][type] ?? screen;
      const got = nextPairing(at(screen), eventFor(type), T0);
      assert.equal(
        got.screen,
        want,
        `${screen} + ${type} => ${got.screen}, want ${want}` +
          (TRANSITIONS[screen][type] ? "" : " (this pair is illegal and must be a no-op)"),
      );
    }
  }
});

test("an unknown or absent event never moves the machine", () => {
  for (const screen of PAIRING_SCREENS) {
    assert.equal(nextPairing(at(screen), { type: "NOT_AN_EVENT" }, T0).screen, screen);
    assert.equal(nextPairing(at(screen), null, T0).screen, screen);
    assert.equal(nextPairing(at(screen), {}, T0).screen, screen);
  }
});

test("leaving a live pairing clears the code, link and SAS from state", () => {
  /* A cancelled or failed screen that still holds the credential can put
   * it back on screen on the next render, and keeps a usable pairing code
   * in memory long after the human decided against it. */
  for (const [screen, event] of [
    ["compare-sas", { type: "CANCEL" }],
    ["compare-sas", { type: "REJECT" }],
    ["show-code", { type: "CANCEL" }],
    ["show-code", { type: "MINT_FAILED", error: "unreachable" }],
    ["awaiting-other-side", { type: "COMPLETED" }],
    ["show-code", { type: "CLOSE" }],
  ]) {
    const s = nextPairing(at(screen), event, T0);
    for (const field of ["code", "link", "sas"]) {
      assert.equal(s[field], "", `${screen} + ${event.type} kept ${field}`);
    }
    assert.equal(s.expiresAt, 0, `${screen} + ${event.type} kept the deadline`);
    assert.equal(pairingView(s).qr, "", `${screen} + ${event.type} still renders a QR`);
  }
});

test("acknowledging the warning is not confirming the pairing", () => {
  /* AT-FR-38-a's real content: the warning screen must not be able to
   * complete anything, or the grant is disclosed after the fact. */
  const s = nextPairing(at("authority-warning"), { type: "ACK_WARNING" }, T0);
  assert.equal(s.computerConfirmed, false);
  assert.equal(s.sas, "");
});

test("the authority warning names the SSH-equivalent grant before any code exists", () => {
  const html = authorityWarningHTML().toLowerCase();
  for (const marker of ["ssh", "anything you can do at this computer"]) {
    assert.ok(html.includes(marker), `warning does not name ${marker}`);
  }
  /* The grant lands on a person, not on a device: "a paired device can do
     anything you can do" invites the reader to picture a phone rather than
     whoever is holding it. */
  assert.ok(html.includes("user of the paired device"), "warning does not say who holds the grant");
  assert.ok(!html.includes("/api/"), "the warning is back to naming routes; that is jargon, not disclosure");
  const view = pairingView(at("authority-warning"));
  assert.equal(view.qr, "", "a code was rendered before the warning was acknowledged");
});

test("MINTED carries the code, link and deadline onto the state", () => {
  const s = nextPairing(at("show-code", { code: "", link: "", expiresAt: 0 }), MINTED, T0);
  assert.equal(s.code, MINTED.code);
  assert.equal(s.link, MINTED.link);
  assert.equal(s.expiresAt, MINTED.expiresAt);
});

test("an offer carries the SAS and stops the code being refreshable", () => {
  /* The one rule that breaks a live pairing if it is wrong: once a device
   * has offered, the code must never be swapped. */
  const s = nextPairing(at("show-code"), OFFER, T0);
  assert.equal(s.sas, "706990");
  const ticked = nextPairing(s, { type: "TICK", now: T0 + TTL + 1 }, T0 + TTL + 1);
  assert.equal(ticked.screen, "compare-sas", "an offered pairing expired out from under the comparison");
  assert.equal(ticked.refreshDue, false, "an offered pairing was queued for a fresh code");
});

test("confirming waits for durable completion rather than succeeding optimistically", () => {
  /* CONFIRM starts the local completion request. Only COMPLETED, emitted
   * after the device record is durable, may report success. */
  const s = nextPairing(at("compare-sas"), { type: "CONFIRM" }, T0);
  assert.equal(s.screen, "awaiting-other-side");
  assert.equal(s.computerConfirmed, true);
  assert.equal(s.deviceConfirmed, false);
  const still = nextPairing(s, { type: "TICK", now: T0 + 1 }, T0 + 1);
  assert.equal(still.screen, "awaiting-other-side", "a timer completed pairing before the server did");
});

test("a failed completion is visible and releases the live credential", () => {
  const s = nextPairing(at("awaiting-other-side"), { type: "CONFIRM_FAILED" }, T0);
  assert.equal(s.screen, "failed");
  assert.equal(s.code, "");
  assert.match(pairingView(s).title, /could not finish/i);
  assert.match(pairingView(s).body, /nothing was paired/i);
});

test("a device confirming first does not skip the human's comparison", () => {
  const s = nextPairing(at("compare-sas"), { type: "DEVICE_CONFIRMED" }, T0);
  assert.equal(s.screen, "compare-sas", "the device's confirmation advanced the computer's screen");
  assert.equal(s.deviceConfirmed, true);
});

test("rejecting a mismatched SAS is distinguishable from cancelling", () => {
  /* Both land on cancelled, but a mismatch is the one outcome that means
   * something was wrong rather than that the human changed their mind. */
  const rejected = nextPairing(at("compare-sas"), { type: "REJECT" }, T0);
  const cancelled = nextPairing(at("compare-sas"), { type: "CANCEL" }, T0);
  assert.equal(rejected.screen, "cancelled");
  assert.equal(cancelled.screen, "cancelled");
  assert.notEqual(rejected.reason, cancelled.reason);
  assert.match(pairingView(rejected).body.toLowerCase(), /match|differ/);
});

test("the QR is shown while the code is scannable and nowhere else", () => {
  /* compare-sas and awaiting-other-side still hold the link -- the device
   * already scanned it -- so a view keyed on "is there a link" would keep
   * a live credential on screen for the whole comparison. */
  assert.match(pairingView(at("show-code")).qr, /^<svg /, "no QR was rendered for a minted code");
  for (const screen of ["compare-sas", "awaiting-other-side"]) {
    assert.equal(pairingView(at(screen)).qr, "", `${screen} still displays the pairing QR`);
  }
  assert.equal(pairingView(at("compare-sas")).sas, "706990");
});

test("every screen renders a title and at least one action, or is closed", () => {
  /* A screen with no way out is a trap; the sheet has no browser back. */
  for (const screen of PAIRING_SCREENS) {
    const view = pairingView(at(screen));
    assert.equal(typeof view.title, "string");
    if (screen === "closed") continue;
    assert.ok(view.title.length > 0, `${screen} has no title`);
    assert.ok(view.actions.length > 0, `${screen} offers no action`);
  }
});

test("initialPairingState is closed, unconfirmed and carries no credential", () => {
  const s = initialPairingState();
  assert.equal(s.screen, "closed");
  assert.equal(s.code, "");
  assert.equal(s.link, "");
  assert.equal(s.sas, "");
  assert.equal(s.computerConfirmed, false);
  assert.equal(s.deviceConfirmed, false);
});

test("nextPairing does not mutate the state it is given", () => {
  /* The adapter keeps the previous state to diff render regions against;
   * in-place mutation would make every diff report no change. */
  const before = at("show-code");
  const snapshot = JSON.stringify(before);
  nextPairing(before, OFFER, T0);
  assert.equal(JSON.stringify(before), snapshot);
});
