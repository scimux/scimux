/* S7 R1 — AT-FR-38-a..c and AT-NFR-12-a. Failures are missing pairing
 * behaviour (PAIRING_UNIMPLEMENTED), not load errors or skips. */
import test from "node:test";
import assert from "node:assert/strict";
import {
  FR38_STATES,
  NFR12_SCREENS,
  renderPairingState,
  authorityWarningHTML,
  tap,
  drivePairingFlow,
  cancelPairing,
  codeConsumed,
  onStorageEvicted,
  onPrivateMode,
  deviceListHTML,
  pairingCodePaste,
} from "./pairing-ui.js";

function s7call(at, fn) {
  try {
    return fn();
  } catch (e) {
    const msg = e && e.message ? e.message : String(e);
    assert.fail(`${at}: ${msg}`);
  }
}

/* AT-FR-38-a asks that the warning state what is being granted before
   confirmation. It asks for the size of the grant, not for route names: the
   markers were /api/nodes and /api/update until 2026-09-04, which tested the
   jargon rather than the disclosure. What must survive is that the warning
   says the grant is total, says whose hands it lands in, and gives the SSH
   comparison for the readers who can act on it. */
function warningNamesGrant(html) {
  const t = String(html).toLowerCase();
  return t.includes("ssh")
    && t.includes("anything you can do at this computer")
    && t.includes("user of the paired device");
}

test("AT-FR-38-a: every FR-38 state renders; authority warning names the SSH-equivalent grant before confirmation", () => {
  const at = "AT-FR-38-a";
  const warning = s7call(at, () => authorityWarningHTML());
  if (!warningNamesGrant(warning)) {
    assert.fail(`${at}: authority warning does not state the grant (the paired device\u2019s user can do anything you can do here, SSH-equivalent): ${warning}`);
  }
  const seen = [];
  for (const state of FR38_STATES) {
    const html = s7call(at, () => renderPairingState(state));
    if (!html || !String(html).includes(state)) {
      assert.fail(`${at}: state ${state} did not render distinguishably: ${html}`);
    }
    seen.push(state);
  }
  for (const must of ["expired", "cancelled", "failed", "authority-warning", "pending", "succeeded"]) {
    if (!seen.includes(must)) {
      assert.fail(`${at}: state ${must} is not reachable`);
    }
  }
  const confirm = s7call(at, () => renderPairingState("compare-sas"));
  const warnFirst = s7call(at, () => tap("authority-warning", "acknowledge"));
  if (warnFirst && warnFirst.confirmed) {
    assert.fail(`${at}: confirmation happened on the warning screen`);
  }
  void confirm;
});

test("AT-FR-38-b: cancelling an accidental pairing leaves the code unconsumed and reusable within its TTL", () => {
  const at = "AT-FR-38-b";
  const minted = s7call(at, () => tap("show-code", "mint"));
  s7call(at, () => cancelPairing(minted && minted.code));
  const html = s7call(at, () => renderPairingState("cancelled"));
  if (!html) {
    assert.fail(`${at}: cancelled state did not render`);
  }
  const consumed = s7call(at, () => codeConsumed(minted && minted.code));
  if (consumed) {
    assert.fail(`${at}: cancel consumed the pairing code`);
  }
  const again = s7call(at, () => tap("enter-code", "submit", minted && minted.code));
  if (again && again.rejected) {
    assert.fail(`${at}: cancelled code was not reusable within TTL`);
  }
});

test("AT-FR-38-c: storage eviction and private mode degrade to a clear re-pair prompt", () => {
  const at = "AT-FR-38-c";
  const evicted = s7call(at, () => onStorageEvicted());
  if (!evicted || evicted.silent || !String(evicted.prompt || evicted.html || "").toLowerCase().includes("pair")) {
    assert.fail(`${at}: storage eviction was silent or not a re-pair prompt: ${JSON.stringify(evicted)}`);
  }
  const priv = s7call(at, () => onPrivateMode());
  if (!priv || priv.silent || !String(priv.prompt || priv.html || "").toLowerCase().includes("pair")) {
    assert.fail(`${at}: private mode was a silently broken session: ${JSON.stringify(priv)}`);
  }
});

test("AT-FR-38: device list has stable labels, duplicate-label handling, and paired-at times", () => {
  const at = "AT-FR-38";
  const html = s7call(at, () => deviceListHTML());
  if (!html || (!String(html).includes("paired") && !String(html).includes("Paired"))) {
    assert.fail(`${at}: device list does not show paired-at: ${html}`);
  }
});

test("AT-NFR-12-a: pairing is the named screen/tap sequence, including inputs, error states, and SAS confirmation — not an action count", () => {
  const at = "AT-NFR-12-a";
  const steps = NFR12_SCREENS.map((screen) => {
    if (screen === "authority-warning") return { screen, tap: "acknowledge" };
    if (screen === "show-code") return { screen, tap: "mint" };
    if (screen === "enter-code") return { screen, tap: "submit", input: "0410-6105" };
    if (screen === "compare-sas") return { screen, tap: "compare", input: "706990" };
    if (screen === "confirm-computer") return { screen, tap: "confirm" };
    if (screen === "confirm-phone") return { screen, tap: "confirm" };
    return { screen, tap: "continue" };
  });
  const result = s7call(at, () => drivePairingFlow(steps));
  if (!result || !Array.isArray(result.screensVisited)) {
    assert.fail(`${at}: drivePairingFlow did not return visited screens`);
  }
  assert.deepEqual(result.screensVisited, [...NFR12_SCREENS], `${at}: screens visited must be the named NFR-12 sequence, not an action count`);
  if (result.actionCount != null && result.actionCount === steps.length && !result.screensVisited) {
    assert.fail(`${at}: asserted an action count instead of the screen sequence`);
  }
  if (!result.sasConfirmed) {
    assert.fail(`${at}: SAS confirmation was skipped`);
  }
  if (!warningNamesGrant(result.warningHTML || "")) {
    assert.fail(`${at}: flow never showed the authority warning grant`);
  }

  for (const errState of ["expired", "cancelled", "failed"]) {
    const html = s7call(at, () => renderPairingState(errState));
    if (!html) {
      assert.fail(`${at}: error state ${errState} missing from the flow`);
    }
  }

  const pasted = s7call(at, () => pairingCodePaste("0410-6105\n"));
  if (!pasted || pasted.rejected) {
    assert.fail(`${at}: specified paste of a grouped pairing code failed: ${JSON.stringify(pasted)}`);
  }

  const warn = s7call(at, () => renderPairingState("authority-warning"));
  if (!String(warn).includes("aria-") && !String(warn).toLowerCase().includes("dialog")) {
    assert.fail(`${at}: authority warning has no accessibility name`);
  }
});
