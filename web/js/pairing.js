/* P1 — the pairing state machine.
 *
 * Pure: no DOM, no timers, no fetch. The adapter (P3) owns all three and
 * feeds this reducer events. Keeping the decisions here is what makes the
 * exhaustive screen x event table in web/test/pairing-state.test.js
 * affordable, and that table is the real specification of this flow.
 *
 * The flow, and why it has the shape it does:
 *
 *   authority-warning -> show-code -> compare-sas
 *                     -> awaiting-other-side -> succeeded
 *
 * Opening the sheet is the request to pair, so the warning is the first
 * screen. It was preceded until 2026-09-04 by a pair-a-device screen
 * carrying a title, no prose and one button repeating the menu item that
 * had just been tapped: a tap that offered no decision.
 *
 * The authority warning is on the computer and comes BEFORE the code,
 * because this is the machine granting access and the grant is
 * SSH-equivalent. Warning after minting would disclose it after the fact.
 *
 * compare-sas is not ceremony. The QR carries the computer's static X to
 * the device out-of-band, so the device holds an authentic computer key --
 * but the device's own key Y comes back through the rendezvous, with
 * nothing visual behind it. The six digits are the only thing binding Y.
 * A compromised rendezvous cannot impersonate the computer (it never sees
 * X) but it can offer its own Y' and be paired with as though it were the
 * phone. That is what the comparison catches, and it is what makes "the
 * rendezvous only relays" a mechanism instead of a promise.
 *
 * The digits are READ on the device and TYPED here (FR-38, 2026-09-07).
 * Displaying them on both screens and asking "do these match?" is the
 * textbook construction, and it holds against a relay -- rv substituting
 * its own Y while the real device is also present, showing digits to
 * disagree with. It does not hold against a replacement: if the device
 * that answered is not yours, yours is showing nothing, there is no
 * second screen, and the affirmative button is a rubber stamp a hurried
 * human taps. Transcription changes nothing cryptographic -- the ECDH and
 * the derivation are identical -- but someone who cannot see the digits
 * cannot produce them, so the wrong device fails at 1 in 10^6 instead of
 * 1 in 1. The typing goes on the computer because the computer is the
 * side granting the authority and the side with a keyboard.
 *
 * There is deliberately no attempt limit. The adversary this defends
 * against is not at this keyboard -- if they were, they would not need to
 * pair -- and a legitimate human who mistypes twice must not be locked
 * out of their own pairing.
 *
 * awaiting-other-side is the in-flight completion call. The local UI sends
 * both explicit human confirmations only after the comparison; COMPLETED is
 * the server's durable acceptance, not a timer or optimistic transition.
 */
import { qrMatrix, qrSVG } from "./qr.js";

export const PAIRING_SCREENS = Object.freeze([
  "closed",
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
 * drives the refresh pause (P2) -- a computer nobody is looking at must not
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
  "CONFIRM_FAILED",
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
    computerConfirmed: false,
    deviceConfirmed: false,
    /* Set when the typed digits were not the device's, so the screen has
     * something to say. Never an error state: the human simply types
     * again. */
    sasMismatch: false,
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
  return { ...s, code: "", rid: "", link: "", sas: "", expiresAt: 0, refreshDue: false, sasMismatch: false };
}

function closedFrom(s) {
  return { ...withoutCredential(s), screen: "closed", computerConfirmed: false, deviceConfirmed: false, reason: "", error: "" };
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
  if (type === "BEGIN" && isFinished(s.screen)) return beginFrom(s);

  switch (s.screen) {
    case "closed":
      return type === "OPEN" ? beginFrom(s) : s;

    case "authority-warning":
      /* Hosted refusal is discovered by the mint, which happens here and
       * on show-code. Routing it to the generic failure instead would
       * tell a device-side user the network was down. */
      if (type === "HOSTED_BLOCKED") return blockedWith(s, event);
      /* Acknowledging is not confirming. The machine moves to show-code
       * with no code yet: the adapter mints on entry and MINTED fills it
       * in, so the human sees the sheet respond immediately rather than
       * waiting on a request before anything appears. */
      if (type === "ACK_WARNING") return { ...s, screen: "show-code", refreshDue: true };
      /* Cancel on the sheet's first screen closes the sheet; there is no
       * screen behind this one to return to. */
      if (type === "CANCEL") return closedFrom(s);
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
      if (type === "HOSTED_BLOCKED") return blockedWith(s, event);
      if (type === "OFFER") return { ...s, screen: "compare-sas", sas: String(event.sas || ""), refreshDue: false };
      if (type === "TICK") return tickShowCode(s, now);
      if (type === "VISIBILITY") return visibility(s, event, now);
      if (type === "CANCEL") return cancelWith(s, "cancelled");
      return s;

    case "compare-sas":
      if (type === "CONFIRM") {
        const typed = onlyDigits(event && event.value);
        const want = onlyDigits(s.sas);
        /* An empty field is not a wrong answer: leaving the instruction
         * standing beats telling someone who typed nothing that what they
         * typed was wrong. */
        if (want === "" || typed !== want) return { ...s, sasMismatch: typed !== "" };
        return { ...s, screen: "awaiting-other-side", computerConfirmed: true, refreshDue: false, sasMismatch: false };
      }
      if (type === "DEVICE_CONFIRMED") return { ...s, deviceConfirmed: true };
      if (type === "REJECT") return cancelWith(s, "sas-mismatch");
      if (type === "CANCEL") return cancelWith(s, "cancelled");
      /* Deliberately inert: TICK and VISIBILITY must not expire or
       * refresh a pairing a device has already offered on. Swapping the
       * code here would invalidate the digits the human is halfway
       * through typing, and the mismatch would look like an attack. */
      return s;

    case "awaiting-other-side":
      if (type === "DEVICE_CONFIRMED") return { ...s, deviceConfirmed: true };
      if (type === "COMPLETED") return { ...withoutCredential(s), screen: "succeeded" };
      if (type === "CONFIRM_FAILED") {
        return { ...withoutCredential(s), screen: "failed", reason: "completion-failed" };
      }
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

/* Digits only, so the spacing a phone renders and a human copies is not
 * treated as a mismatch. Both sides are normalised the same way, which is
 * why an absent SAS is refused explicitly at the call site: "" would
 * otherwise match an empty entry and pair a session that never received an
 * offer. */
function onlyDigits(v) {
  return String(v == null ? "" : v).replace(/\D+/g, "");
}

function isFinished(screen) {
  return screen === "succeeded" || screen === "cancelled" || screen === "failed" || screen === "expired";
}

/* The computer's own refusal, as opposed to the network's. It carries the
 * server's sentence when there is one: the reason may be a paired device, a
 * revoked or disabled installation, or no enrollment at all, and only the
 * side that answered knows which. */
function blockedWith(s, event) {
  return {
    ...withoutCredential(s),
    screen: "failed",
    reason: "hosted-blocked",
    error: String((event && event.error) || ""),
  };
}

function failWith(s, event) {
  return { ...withoutCredential(s), screen: "failed", reason: "mint-failed", error: String((event && event.error) || "") };
}

function cancelWith(s, reason) {
  return { ...withoutCredential(s), screen: "cancelled", reason, computerConfirmed: false, deviceConfirmed: false };
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
 * invite-only, so the warning must state the size of the grant before any
 * code exists.
 *
 * It named the two routes -- POST /api/nodes and POST /api/update -- from
 * 2026-08-27 until 2026-09-04, on the argument that a human can weigh "it
 * can replace the program" and cannot weigh an adjective. The routes went
 * because they are jargon: HIG asks for the consequence in the reader's
 * words, and a supervision UI aimed at people who run agents rather than
 * servers cannot assume a reader who parses a method and a path. What is
 * granted is now carried by the subject of the sentence -- the *user* of
 * the paired device, not the device -- and by the SSH comparison, which
 * says "everything" to the readers who can act on it.
 *
 * AT-FR-38-a asks that the warning state what is being granted before
 * confirmation. It does not ask for the route names; those were this
 * file's reading of it. */
export function authorityWarningHTML() {
  return (
    '<div class="pair-warn" role="group" aria-label="What a paired device may do">' +
    "<p>The user of the paired device <strong>can do anything you can do at this computer</strong>.</p>" +
    "<p>Pair only a device you control, and only with a code you just created.</p>" +
    '<p class="pair-warn-aside">Same risk as giving someone SSH access to this computer.</p>' +
    "</div>"
  );
}

const VIEWS = {
  closed: () => ({ title: "", body: "", actions: [] }),
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
  "compare-sas": (s) => ({
    title: "Type the digits from your device",
    body: s.sasMismatch
      ? "Those are not the digits this computer is expecting. Check your device and type them again. If your device is not showing any digits, stop — something else answered your code."
      : "Your device is showing six digits. Type them here. If your device is not showing any digits, stop — something else answered your code.",
    entry: true,
    actions: [
      { id: "confirm", label: "Pair this device", primary: true },
      { id: "reject", label: "My device shows nothing" },
    ],
    announce: s.sasMismatch ? "Those digits do not match. Try again." : "Type the six digits your device is showing.",
  }),
  "awaiting-other-side": () => ({
    title: "Finishing pairing",
    body: "Saving this device…",
    actions: [{ id: "cancel", label: "Cancel" }],
  }),
  succeeded: () => ({
    title: "Device paired",
    body: "It can now reach this computer. Revoke it any time from Remote access.",
    actions: [{ id: "close", label: "Done", primary: true }],
  }),
  cancelled: (s) => ({
    title: s.reason === "sas-mismatch" ? "Digits did not match" : "Pairing cancelled",
    body:
      s.reason === "sas-mismatch"
        ? "Nothing was paired. If your device was not showing those digits, the device that answered your code was not yours — find out why before you try again."
        : "Nothing was paired. The code was not used.",
    actions: [
      { id: "begin", label: "Try again", primary: true },
      { id: "close", label: "Close" },
    ],
  }),
  failed: (s) => {
    let title = "Could not start pairing";
    let body = s.error || "The rendezvous could not be reached.";
    if (s.reason === "hosted-blocked") {
      /* The default is the one refusal the computer cannot phrase for
         itself, because it is the device asking: this app is running on a
         paired device, and the computer is elsewhere. Every other 409 —
         revoked, disabled, not enrolled — arrives with the server's own
         sentence, which names a state this screen would only guess at. */
      title = s.error ? "Pairing is not available" : "Pair from the computer";
      body = s.error || "A paired device cannot pair further devices. Do this on the computer running scimux.";
    } else if (s.reason === "completion-failed") {
      title = "Could not finish pairing";
      body = "Nothing was paired. Start again with a new code.";
    }
    return {
      title,
      body,
      actions: [
        { id: "begin", label: "Try again", primary: true },
        { id: "close", label: "Close" },
      ],
    };
  },
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
    /* Whether to offer the digit field. The view never carries the digits
     * themselves: this is the screen asking to be told them. */
    entry: !!v.entry,
    announce: v.announce || "",
  };
}
