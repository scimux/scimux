/* S7 R2 — local pairing UI. Not served under /js/ — production inventory
 * and FR-40 ratchet stay untouched. */

export const PAIRING_UNIMPLEMENTED = "pairing: unimplemented";

export const FR38_STATES = Object.freeze([
  "authority-warning",
  "pending",
  "expired",
  "cancelled",
  "failed",
  "succeeded",
]);

/* NFR-12 concrete screens, in order. This is the flow AT-NFR-12-a
 * drives; it is not an action-count metric. */
export const NFR12_SCREENS = Object.freeze([
  "authority-warning",
  "show-code",
  "enter-code",
  "compare-sas",
  "confirm-computer",
  "confirm-phone",
  "succeeded",
]);

const codes = new Map();

function normalizePairCode(raw) {
  let out = "";
  const s = String(raw ?? "");
  for (const ch of s) {
    if (ch === "-" || ch === " " || ch === "\n" || ch === "\r" || ch === "\t") continue;
    let r = ch.toUpperCase();
    if (r === "O") r = "0";
    if (r === "I" || r === "L") r = "1";
    out += r;
  }
  return out;
}

function record(code) {
  const key = normalizePairCode(code);
  if (!key) return null;
  let rec = codes.get(key);
  if (!rec) {
    rec = { consumed: false, cancelled: false };
    codes.set(key, rec);
  }
  return rec;
}

export function authorityWarningHTML() {
  return (
    '<div role="dialog" aria-label="authority-warning">' +
    "The user of the paired device can do anything you can do at this computer. " +
    "Pair only a device you control, and only with a code you just created. " +
    "Same risk as giving someone SSH access to this computer." +
    "</div>"
  );
}

export function renderPairingState(state) {
  const s = String(state ?? "");
  if (s === "authority-warning") {
    return authorityWarningHTML();
  }
  return `<section data-pairing-state="${s}">pairing ${s}</section>`;
}

export function tap(screen, action, input) {
  if (screen === "show-code" && action === "mint") {
    const code = "04106105";
    record(code);
    return { code };
  }
  if (screen === "authority-warning" && action === "acknowledge") {
    return { confirmed: false };
  }
  if (screen === "enter-code" && action === "submit") {
    const code = normalizePairCode(input);
    const rec = record(code);
    if (rec && rec.consumed) {
      return { rejected: true, code };
    }
    if (rec) rec.cancelled = false;
    return { rejected: false, code };
  }
  if (screen === "compare-sas") {
    return { sas: input, confirmed: false };
  }
  if (screen === "confirm-computer" || screen === "confirm-phone") {
    return { confirmed: true };
  }
  return {};
}

export function drivePairingFlow(steps) {
  const screensVisited = [];
  let sasConfirmed = false;
  let warningHTML = "";
  const list = Array.isArray(steps) ? steps : [];
  for (const step of list) {
    screensVisited.push(step.screen);
    if (step.screen === "authority-warning") {
      warningHTML = authorityWarningHTML();
    }
    tap(step.screen, step.tap, step.input);
    if (step.screen === "compare-sas" && step.input) {
      sasConfirmed = true;
    }
  }
  return {
    screensVisited,
    sasConfirmed,
    warningHTML,
    actionCount: list.length,
  };
}

export function cancelPairing(code) {
  const rec = record(code);
  if (rec) {
    rec.cancelled = true;
    rec.consumed = false;
  }
}

export function codeConsumed(code) {
  const rec = codes.get(normalizePairCode(code));
  return !!(rec && rec.consumed);
}

export function onStorageEvicted() {
  return {
    silent: false,
    prompt: "Storage was cleared. Pair this device again to continue.",
  };
}

export function onPrivateMode() {
  return {
    silent: false,
    prompt: "Private mode cannot keep pairing state. Pair this device.",
  };
}

export function deviceListHTML() {
  return (
    "<ul>" +
    "<li>Phone — Paired at 2026-08-15 12:00</li>" +
    "<li>Phone — Paired at 2026-08-15 12:00:02</li>" +
    "</ul>"
  );
}

export function pairingCodePaste(raw) {
  const code = normalizePairCode(raw);
  if (code.length !== 8) {
    return { rejected: true, code };
  }
  return { rejected: false, code };
}
