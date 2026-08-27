/* P1 — the pairing state machine.
 *
 * Pure: no DOM, no timers, no fetch. The adapter (P3) owns all three and
 * feeds this reducer events. Keeping the decisions here is what makes the
 * exhaustive screen x event table in web/test/pairing-state.test.js
 * affordable, and that table is the real specification of this flow.
 *
 * The flow, and why it has the shape it does:
 *
 *   pair-a-device -> authority-warning -> show-code -> compare-sas
 *                 -> awaiting-other-side -> succeeded
 *
 * The authority warning is on the laptop and comes BEFORE the code,
 * because this is the machine granting access and the grant is
 * SSH-equivalent. Warning after minting would disclose it after the fact.
 *
 * compare-sas is not ceremony. The QR carries the laptop's static X to
 * the device out-of-band, so the device holds an authentic laptop key --
 * but the device's own key Y comes back through the rendezvous, with
 * nothing visual behind it. The six digits are the only thing binding Y.
 * A compromised rendezvous cannot impersonate the laptop (it never sees
 * X) but it can offer its own Y' and be paired with as though it were the
 * phone. That is what the comparison catches, and it is what makes "the
 * rendezvous only relays" a mechanism instead of a promise.
 *
 * awaiting-other-side exists because FR-12 requires confirmation on both
 * ends and the laptop cannot speak for the phone. CONFIRM records the
 * laptop's half; only COMPLETED -- fired once the rendezvous reports the
 * device's own confirmation -- reaches succeeded.
 */
import { qrMatrix, qrSVG } from "./qr.js";

export const PAIRING_SCREENS = Object.freeze([
  "closed",
  "pair-a-device",
  "authority-warning",
  "show-code",
  "compare-sas",
  "awaiting-other-side",
  "succeeded",
  "cancelled",
  "failed",
  "expired",
]);

/* VISIBILITY and CLOSE are not part of the pairing protocol; they are the
 * two facts about the human that the machine has to know. VISIBILITY
 * drives the refresh pause (P2) -- a laptop nobody is looking at must not
 * keep a live credential cycling on screen. CLOSE is separate from CANCEL
 * because dismissing the sheet and rejecting a mismatch mean different
 * things and only one of them is evidence of a problem. */
export const PAIRING_EVENTS = Object.freeze([
  "OPEN",
  "BEGIN",
  "ACK_WARNING",
  "MINTED",
  "MINT_FAILED",
  "TICK",
  "OFFER",
  "DEVICE_CONFIRMED",
  "CONFIRM",
  "REJECT",
  "CANCEL",
  "COMPLETED",
  "HOSTED_BLOCKED",
  "VISIBILITY",
  "CLOSE",
]);

export function initialPairingState() {
  return {
    screen: "closed",
    code: "",
    rid: "",
    link: "",
    sas: "",
    expiresAt: 0,
    laptopConfirmed: false,
    deviceConfirmed: false,
    /* The adapter's cue to mint. The reducer cannot mint -- that is a
     * network call -- so expiry is expressed as a request rather than
     * performed. */
    refreshDue: false,
    visible: true,
    reason: "",
    error: "",
  };
}

/* Everything the code identifies, cleared together. Used on every exit
 * from a live pairing so a cancelled or failed screen cannot still be
 * holding a credential that a later render could put back on screen. */
function withoutCredential(s) {
  return { ...s, code: "", rid: "", link: "", sas: "", expiresAt: 0, refreshDue: false };
}

function closedFrom(s) {
  return { ...withoutCredential(s), screen: "closed", laptopConfirmed: false, deviceConfirmed: false, reason: "", error: "" };
}

function beginFrom(s) {
  return { ...closedFrom(s), screen: "authority-warning", visible: s.visible };
}

export function nextPairing(state, event, now) {
  const s = state || initialPairingState();
  const type = event && event.type;

  /* CLOSE and BEGIN mean the same thing from every screen that accepts
   * them, so they are handled once rather than repeated per screen. Both
   * are refused mid-flow only where the table says so. */
  if (type === "CLOSE" && s.screen !== "closed") return closedFrom(s);
  if (type === "BEGIN" && (s.screen === "pair-a-device" || isFinished(s.screen))) return beginFrom(s);

  switch (s.screen) {
    case "closed":
      return type === "OPEN" ? { ...s, screen: "pair-a-device" } : s;

    case "pair-a-device":
      if (type === "HOSTED_BLOCKED") return { ...s, screen: "failed", reason: "hosted-blocked" };
      return s;

    case "authority-warning":
      /* Hosted refusal is discovered by the mint, and the mint happens
       * here and on show-code -- never on pair-a-device, which asks the
       * server nothing. Routing it to the generic failure instead would
       * tell a device-side user the network was down. */
      if (type === "HOSTED_BLOCKED") return { ...withoutCredential(s), screen: "failed", reason: "hosted-blocked" };
      /* Acknowledging is not confirming. The machine moves to show-code
       * with no code yet: the adapter mints on entry and MINTED fills it
       * in, so the human sees the sheet respond immediately rather than
       * waiting on a request before anything appears. */
      if (type === "ACK_WARNING") return { ...s, screen: "show-code", refreshDue: true };
      if (type === "CANCEL") return { ...closedFrom(s), screen: "pair-a-device", visible: s.visible };
      if (type === "MINT_FAILED") return failWith(s, event);
      return s;

    case "show-code":
      if (type === "MINTED") {
        return {
          ...s,
          code: String(event.code || ""),
          rid: String(event.rid || ""),
          link: String(event.link || ""),
          expiresAt: Number(event.expiresAt || 0),
          refreshDue: false,
        };
      }
      if (type === "MINT_FAILED") return failWith(s, event);
      if (type === "HOSTED_BLOCKED") return { ...withoutCredential(s), screen: "failed", reason: "hosted-blocked" };
      if (type === "OFFER") return { ...s, screen: "compare-sas", sas: String(event.sas || ""), refreshDue: false };
      if (type === "TICK") return tickShowCode(s, now);
      if (type === "VISIBILITY") return visibility(s, event, now);
      if (type === "CANCEL") return cancelWith(s, "cancelled");
      return s;

    case "compare-sas":
      if (type === "CONFIRM") return { ...s, screen: "awaiting-other-side", laptopConfirmed: true, refreshDue: false };
      if (type === "DEVICE_CONFIRMED") return { ...s, deviceConfirmed: true };
      if (type === "REJECT") return cancelWith(s, "sas-mismatch");
      if (type === "CANCEL") return cancelWith(s, "cancelled");
      /* Deliberately inert: TICK and VISIBILITY must not expire or
       * refresh a pairing a device has already offered on. Swapping the
       * code here would invalidate digits the human is mid-comparison
       * with, and the mismatch would look like an attack. */
      return s;

    case "awaiting-other-side":
      if (type === "DEVICE_CONFIRMED") return { ...s, deviceConfirmed: true };
      if (type === "COMPLETED") return { ...withoutCredential(s), screen: "succeeded" };
      if (type === "REJECT") return cancelWith(s, "sas-mismatch");
      if (type === "CANCEL") return cancelWith(s, "cancelled");
      return s;

    case "expired":
      /* Coming back to the tab is the resume: the code died while nobody
       * was looking, so mint immediately rather than showing a dead
       * screen the human has to poke. */
      if (type === "VISIBILITY" && event.visible) {
        return { ...s, screen: "show-code", visible: true, refreshDue: true };
      }
      if (type === "VISIBILITY") return { ...s, visible: false };
      return s;

    default:
      return s;
  }
}

function isFinished(screen) {
  return screen === "succeeded" || screen === "cancelled" || screen === "failed" || screen === "expired";
}

function failWith(s, event) {
  return { ...withoutCredential(s), screen: "failed", reason: "mint-failed", error: String((event && event.error) || "") };
}

function cancelWith(s, reason) {
  return { ...withoutCredential(s), screen: "cancelled", reason, laptopConfirmed: false, deviceConfirmed: false };
}

function expiredAt(s, now) {
  return s.expiresAt > 0 && now >= s.expiresAt;
}

function tickShowCode(s, now) {
  if (!expiredAt(s, now)) return s;
  /* Nobody is looking: stop cycling a live credential on an unattended
   * screen and wait to be come back to. */
  if (!s.visible) return { ...withoutCredential(s), screen: "expired" };
  return { ...s, refreshDue: true };
}

function visibility(s, event, now) {
  const visible = !!event.visible;
  const moved = { ...s, visible };
  if (!visible) return moved;
  return expiredAt(moved, now) ? { ...moved, refreshDue: true } : moved;
}

/* The grant this warning describes is the whole reason pairing is
 * invite-only. It names the two routes that make it SSH-equivalent rather
 * than saying "full access", because a human can weigh "it can replace
 * the binary" and cannot weigh an adjective. */
export function authorityWarningHTML() {
  return (
    '<div class="pair-warn" role="group" aria-label="What a paired device may do">' +
    "<p>A paired device has <strong>SSH-equivalent authority</strong> on this laptop.</p>" +
    "<ul>" +
    "<li>It may <code>POST /api/nodes</code> to launch agents that run commands here.</li>" +
    "<li>It may <code>POST /api/update</code> to replace the scimux binary.</li>" +
    "</ul>" +
    "<p>Pair only a device you control, and only over a code you minted just now.</p>" +
    "</div>"
  );
}

const VIEWS = {
  closed: () => ({ title: "", body: "", actions: [] }),
  "pair-a-device": () => ({
    title: "Remote access",
    body: "Pair a phone or tablet to reach these sessions from another device.",
    actions: [{ id: "begin", label: "Pair a device", primary: true }],
  }),
  "authority-warning": () => ({
    title: "Before you pair",
    body: authorityWarningHTML(),
    actions: [
      { id: "ack", label: "I understand — show the code", primary: true },
      { id: "cancel", label: "Cancel" },
    ],
  }),
  "show-code": (s) => ({
    title: "Scan this with your device",
    body: s.code ? "The code is valid for a short time and refreshes itself while this stays open." : "Minting a pairing code…",
    actions: [{ id: "cancel", label: "Cancel" }],
    announce: s.code ? "New pairing code ready to scan." : "",
  }),
  "compare-sas": () => ({
    title: "Do these numbers match?",
    body: "Your device is showing six digits. They must match the ones here. If they differ, something is relaying your pairing — do not continue.",
    actions: [
      { id: "confirm", label: "They match", primary: true },
      { id: "reject", label: "They don't match" },
    ],
  }),
  "awaiting-other-side": (s) => ({
    title: "Waiting for your device",
    body: s.deviceConfirmed ? "Finishing up…" : "Confirm the same numbers on your device to finish pairing.",
    actions: [{ id: "cancel", label: "Cancel" }],
  }),
  succeeded: () => ({
    title: "Device paired",
    body: "It can now reach this laptop. Revoke it any time from Remote access.",
    actions: [{ id: "close", label: "Done", primary: true }],
  }),
  cancelled: (s) => ({
    title: s.reason === "sas-mismatch" ? "Numbers did not match" : "Pairing cancelled",
    body:
      s.reason === "sas-mismatch"
        ? "Nothing was paired. Digits that differ mean the device you scanned is not the device that answered — try again, and if they differ a second time, stop and investigate."
        : "Nothing was paired. The code was not used.",
    actions: [
      { id: "begin", label: "Try again", primary: true },
      { id: "close", label: "Close" },
    ],
  }),
  failed: (s) => ({
    title: s.reason === "hosted-blocked" ? "Pair from the laptop" : "Could not start pairing",
    body:
      s.reason === "hosted-blocked"
        ? "A paired device cannot pair further devices. Do this on the laptop running scimux."
        : s.error || "The rendezvous could not be reached.",
    actions: [
      { id: "begin", label: "Try again", primary: true },
      { id: "close", label: "Close" },
    ],
  }),
  expired: () => ({
    title: "Code expired",
    body: "Pairing codes are short-lived. Get a fresh one when you are ready to scan.",
    actions: [
      { id: "begin", label: "Get a new code", primary: true },
      { id: "close", label: "Close" },
    ],
  }),
};

export function pairingView(state) {
  const s = state || initialPairingState();
  const build = VIEWS[s.screen] || VIEWS.closed;
  const v = build(s);
  return {
    screen: s.screen,
    title: v.title,
    body: v.body,
    actions: v.actions,
    /* Only show-code renders a QR. Once a device has offered, the scan
     * has happened and a still-visible code is just another credential on
     * screen. */
    qr: s.screen === "show-code" && s.link ? qrSVG(qrMatrix(s.link)) : "",
    sas: s.sas,
    announce: v.announce || "",
  };
}
