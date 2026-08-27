/* P2 — auto-refresh, at the level where it is a decision rather than a
 * timer. The reducer cannot mint (that is a network call), so expiry is
 * expressed as `refreshDue`: a request the adapter fulfils. Everything
 * about *when* a fresh code is wanted is therefore testable with no DOM,
 * no clock and no fetch.
 *
 * The timer that fires TICK, the aria-live announcement and the
 * ClassPairExpired swallow all live in the adapter and are tested in P3,
 * where they exist.
 */
import test from "node:test";
import assert from "node:assert/strict";

import { initialPairingState, nextPairing, pairingView } from "../js/pairing.js";

const T0 = 1_000_000;
const TTL = 60_000;
const LIVE = T0 + TTL; // deadline in the future at T0
const DEAD = T0 - 1; // deadline already passed at T0

function state(screen, over = {}) {
  return {
    ...initialPairingState(),
    screen,
    visible: true,
    code: "04106105",
    link: "https://my.scimux.eu/p#c=04106105",
    expiresAt: LIVE,
    ...over,
  };
}

/* The load-bearing rule, as one table. A fresh code is wanted only when
 * the sheet is showing a code nobody has scanned yet, the code has died,
 * and someone is actually looking at it. Every other combination must
 * leave the code alone -- swapping it mid-comparison breaks a pairing
 * that was working, and cycling one on an unattended screen leaves a live
 * credential on display for anyone walking past. */
const CASES = [
  // screen,                visible, expiresAt, want screen,        want refreshDue
  ["show-code", true, DEAD, "show-code", true],
  ["show-code", true, LIVE, "show-code", false],
  ["show-code", false, DEAD, "expired", false],
  ["show-code", false, LIVE, "show-code", false],
  ["compare-sas", true, DEAD, "compare-sas", false],
  ["compare-sas", false, DEAD, "compare-sas", false],
  ["awaiting-other-side", true, DEAD, "awaiting-other-side", false],
  ["awaiting-other-side", false, DEAD, "awaiting-other-side", false],
  ["pair-a-device", true, DEAD, "pair-a-device", false],
  ["authority-warning", true, DEAD, "authority-warning", false],
  ["succeeded", true, DEAD, "succeeded", false],
  ["cancelled", true, DEAD, "cancelled", false],
  ["failed", true, DEAD, "failed", false],
  ["expired", true, DEAD, "expired", false],
];

test("a tick asks for a fresh code only on an unscanned, expired, watched screen", () => {
  for (const [screen, visible, expiresAt, wantScreen, wantRefresh] of CASES) {
    const s = nextPairing(state(screen, { visible, expiresAt }), { type: "TICK", now: T0 }, T0);
    const label = `${screen} visible=${visible} ${expiresAt === DEAD ? "expired" : "live"}`;
    assert.equal(s.screen, wantScreen, `${label}: screen`);
    assert.equal(s.refreshDue, wantRefresh, `${label}: refreshDue`);
  }
});

test("a code with no deadline is never treated as expired", () => {
  /* The window between acknowledging the warning and MINTED arriving has
   * expiresAt 0. Treating that as "past" would mint in a loop. */
  const s = nextPairing(state("show-code", { expiresAt: 0, code: "", link: "" }), { type: "TICK", now: T0 }, T0);
  assert.equal(s.screen, "show-code");
  assert.equal(s.refreshDue, false);
});

test("expiry is inclusive of the deadline itself", () => {
  const exact = nextPairing(state("show-code", { expiresAt: T0 }), { type: "TICK", now: T0 }, T0);
  assert.equal(exact.refreshDue, true, "a code is dead at its deadline, not one millisecond after");
});

test("acknowledging the warning asks for the first code", () => {
  const s = nextPairing({ ...initialPairingState(), screen: "authority-warning" }, { type: "ACK_WARNING" }, T0);
  assert.equal(s.screen, "show-code");
  assert.equal(s.refreshDue, true, "the sheet opened on show-code without asking anyone to mint");
});

test("MINTED satisfies the request", () => {
  const asked = nextPairing(state("show-code", { expiresAt: DEAD }), { type: "TICK", now: T0 }, T0);
  assert.equal(asked.refreshDue, true);
  const filled = nextPairing(asked, { type: "MINTED", code: "11112222", rid: "ab", link: "https://x/p#c=2", expiresAt: LIVE }, T0);
  assert.equal(filled.refreshDue, false, "the adapter would mint again on the next tick");
  assert.equal(filled.link, "https://x/p#c=2");
});

test("hiding the tab stops the refresh; returning to it resumes and re-mints", () => {
  /* An unattended laptop cycling a live pairing credential on screen is a
   * standing invitation. Pause on the condition actually being guarded --
   * nobody is looking -- rather than on a duration. */
  const watched = state("show-code", { expiresAt: DEAD });
  const hidden = nextPairing(watched, { type: "VISIBILITY", visible: false }, T0);
  assert.equal(hidden.visible, false);
  assert.equal(hidden.refreshDue, false, "a hidden tab kept asking for fresh codes");

  const died = nextPairing(hidden, { type: "TICK", now: T0 }, T0);
  assert.equal(died.screen, "expired");
  assert.equal(died.code, "", "an expired code was left in state while nobody was watching");

  const back = nextPairing(died, { type: "VISIBILITY", visible: true }, T0);
  assert.equal(back.screen, "show-code");
  assert.equal(back.refreshDue, true, "returning to the tab left a dead screen to poke at");
});

test("returning to a tab whose code is still alive does not churn it", () => {
  const hidden = nextPairing(state("show-code", { expiresAt: LIVE }), { type: "VISIBILITY", visible: false }, T0);
  const back = nextPairing(hidden, { type: "VISIBILITY", visible: true }, T0);
  assert.equal(back.screen, "show-code");
  assert.equal(back.refreshDue, false, "a perfectly good code was thrown away on refocus");
  assert.equal(back.link, hidden.link);
});

test("hiding the tab during a comparison changes nothing", () => {
  /* The human may well be looking at their phone. */
  for (const screen of ["compare-sas", "awaiting-other-side"]) {
    const hidden = nextPairing(state(screen, { expiresAt: DEAD }), { type: "VISIBILITY", visible: false }, T0);
    assert.equal(hidden.screen, screen);
    assert.equal(hidden.refreshDue, false);
    const back = nextPairing(hidden, { type: "VISIBILITY", visible: true }, T0);
    assert.equal(back.screen, screen, `${screen} was interrupted by a visibility change`);
    assert.equal(back.refreshDue, false);
  }
});

test("an offer that lands on an already-queued refresh cancels it", () => {
  /* The narrow race that breaks a working pairing: the code expires, a
   * refresh is queued, and the device's offer arrives before the adapter
   * has minted. If the request survives into compare-sas the adapter
   * mints anyway, and the digits the human is comparing belong to a code
   * that no longer exists. */
  const queued = nextPairing(state("show-code", { expiresAt: DEAD }), { type: "TICK", now: T0 }, T0);
  assert.equal(queued.refreshDue, true);
  const offered = nextPairing(queued, { type: "OFFER", sas: "706990" }, T0);
  assert.equal(offered.screen, "compare-sas");
  assert.equal(offered.refreshDue, false, "a code was queued for replacement while its SAS was being compared");
});

test("the rendered QR follows the link, so a refresh cannot leave a stale image", () => {
  const first = state("show-code");
  const second = nextPairing(first, { type: "MINTED", code: "99998888", rid: "cd", link: "https://my.scimux.eu/p#c=99998888", expiresAt: LIVE }, T0);
  const a = pairingView(first).qr;
  const b = pairingView(second).qr;
  assert.match(a, /^<svg /);
  assert.match(b, /^<svg /);
  assert.notEqual(a, b, "a fresh code rendered the previous code's QR");
});

test("the freshly minted code is announced, and the waiting state is not", () => {
  /* Announced politely, and only when there is something new to read: an
   * assertive live region interrupts a screen reader mid-digit. The
   * politeness itself is a DOM property and is asserted in P3. */
  assert.match(pairingView(state("show-code")).announce, /pairing code/i);
  assert.equal(pairingView(state("show-code", { code: "", link: "" })).announce, "");
});
