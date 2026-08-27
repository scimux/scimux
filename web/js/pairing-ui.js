/* P3 — the pairing adapter: the DOM, clock and network half of pairing.
 *
 * Packet S8 ownership inventory
 * -----------------------------
 * Owned roots (nothing else writes into these):
 *   - #pairsheet  (hidden marker; shown only while the flow is open)
 *   - #pair_title, #pair_body, #pair_qr, #pair_sas
 *   - #pair_actions (delegated click on [data-pa])
 *   - #pair_live  (aria-live="polite"; announcements only)
 *   - #pair_x     (cancel-and-close at every stage)
 *
 * Explicit non-ownership:
 *   - the burger's Remote access section and the paired-device list (P4)
 *   - the generic sheet backdrop; this sheet is opened and closed through
 *     open()/close(), and dismissable() is how the shell asks whether a
 *     backdrop tap or swipe may take it
 *
 * Server calls (via injected api):
 *   - POST /api/remote/pairing
 *   - GET  /api/remote/pairing/:code
 *   - POST /api/remote/pairing/:code/confirm
 *   - POST /api/remote/pairing/:code/cancel
 *
 * Timers: one setInterval while the sheet is open. It drives TICK (which
 * is what expires and refreshes a code) and the session poll.
 *
 * Everything that decides anything lives in pairing.js. This module only
 * translates, in four directions: taps to events, the clock to events,
 * HTTP to events, and state to slots. It holds no pairing state of its own
 * beyond the reducer's, the interval id, and the single-flight mint guard.
 */
import { esc } from "./format.js";
import { initialPairingState, nextPairing, pairingView } from "./pairing.js";

const PAIRING_ROOT = "/api/remote/pairing";
const TICK_MS = 1000;

/* Screens holding a live credential: the sheet must not be dismissed out
 * from under them, because that would abandon a code with no cancel
 * behind it. */
const LIVE_SCREENS = new Set(["show-code", "compare-sas", "awaiting-other-side"]);

function sessionURL(code, suffix = "") {
  return `${PAIRING_ROOT}/${encodeURIComponent(code)}${suffix}`;
}

function actionsHTML(actions) {
  return (actions || [])
    .map(
      (a) =>
        `<button type="button" data-pa="${esc(a.id)}"` +
        (a.primary ? ` class="primary"` : "") +
        `>${esc(a.label)}</button>`,
    )
    .join("");
}

export function createPairingFeature({ api, doc, timers = {}, now = Date.now } = {}) {
  const setInterval_ = timers.setInterval || ((fn, ms) => globalThis.setInterval(fn, ms));
  const clearInterval_ = timers.clearInterval || ((id) => globalThis.clearInterval(id));

  let state = initialPairingState();
  let ticker = null;
  let minting = false;
  /* Work queue. Every effect is appended here so settled() has one thing
     to wait for, and so two effects can never interleave their dispatches. */
  let chain = Promise.resolve();
  const cleanups = [];

  const el = (sel) => doc.querySelector(sel);

  function enqueue(fn) {
    chain = chain.then(fn).catch(() => {});
    return chain;
  }

  function render() {
    const v = pairingView(state);
    const sheet = el("#pairsheet");
    if (sheet) sheet.hidden = state.screen === "closed";
    const title = el("#pair_title");
    if (title) title.textContent = v.title;
    const body = el("#pair_body");
    if (body) body.innerHTML = v.body;
    const qr = el("#pair_qr");
    if (qr) qr.innerHTML = v.qr;
    const sas = el("#pair_sas");
    if (sas) sas.textContent = v.sas;
    const actions = el("#pair_actions");
    if (actions) actions.innerHTML = actionsHTML(v.actions);
    /* Only ever written when there is something new to say: assigning ""
       on every render would clear the region before a screen reader had
       finished reading the digits out. */
    const live = el("#pair_live");
    if (live && v.announce) live.textContent = v.announce;
  }

  function dispatch(event) {
    state = nextPairing(state, event, now());
    render();
    react();
  }

  /* The reducer cannot mint; refreshDue is how it asks. */
  function react() {
    if (state.screen === "show-code" && state.refreshDue && !minting) mint();
  }

  function mint() {
    minting = true;
    enqueue(async () => {
      try {
        const r = await api(PAIRING_ROOT, { method: "POST" });
        dispatch({
          type: "MINTED",
          code: r && r.code,
          rid: r && r.rid,
          link: r && r.link,
          expiresAt: Date.parse((r && r.expires_at) || "") || 0,
        });
      } catch (e) {
        /* 409 is the hosted refusal — a paired device asking to pair
           further devices. It needs its own screen: "go to the computer"
           and "the rendezvous is unreachable" are different problems. */
        if (e && e.status === 409) dispatch({ type: "HOSTED_BLOCKED" });
        else dispatch({ type: "MINT_FAILED", error: (e && e.message) || "" });
      } finally {
        minting = false;
      }
    });
  }

  function applyStatus(st) {
    if (!st) return;
    /* Both are no-ops on any screen that has moved past them; the
       reducer owns that, and a second guard here would be a second
       authority on the same question. */
    if (st.sas) dispatch({ type: "OFFER", sas: st.sas });
    if (st.device_confirmed === true) dispatch({ type: "DEVICE_CONFIRMED" });
  }

  function tick() {
    dispatch({ type: "TICK" });
    /* Only a minted code has a session to ask about. show-code is reached
       before MINTED lands, so this is a real state and not defensive. */
    if (!state.code) return;
    const code = state.code;
    enqueue(async () => {
      /* A poll that fails is a poll that will happen again in a second;
         it is never evidence about the pairing itself. */
      try {
        applyStatus(await api(sessionURL(code), {}));
      } catch {
        /* transient */
      }
    });
  }

  /* Best-effort release of a code we are walking away from. Failure is
     never surfaced: 410 means the session is already in the state the
     cancel was asking for, and a genuine failure must not trap the human
     in a sheet they just dismissed. */
  function releaseCode(code) {
    if (!code) return;
    enqueue(async () => {
      try {
        await api(sessionURL(code, "/cancel"), { method: "POST" });
      } catch {
        /* already gone, or unreachable — either way the human is done */
      }
    });
  }

  function leave(event) {
    const code = state.code;
    dispatch(event);
    releaseCode(code);
  }

  function confirm() {
    const code = state.code;
    dispatch({ type: "CONFIRM" });
    enqueue(async () => {
      try {
        await api(sessionURL(code, "/confirm"), {
          method: "POST",
          body: JSON.stringify({ computer_confirm: true, device_confirm: true }),
        });
        dispatch({ type: "COMPLETED" });
      } catch {
        /* 409 is ClassPairUnconfirmed: the other side has not confirmed
           yet. That is the ordinary case for a human still reaching for
           their phone, so the sheet keeps waiting rather than tearing
           down a pairing that is one tap from finishing. */
      }
    });
  }

  const ACTIONS = {
    begin: () => dispatch({ type: "BEGIN" }),
    ack: () => dispatch({ type: "ACK_WARNING" }),
    cancel: () => leave({ type: "CANCEL" }),
    reject: () => leave({ type: "REJECT" }),
    confirm,
    close: () => close(),
  };

  function onActionClick(ev) {
    const hit = ev && ev.target && ev.target.closest && ev.target.closest("[data-pa]");
    const run = hit && ACTIONS[hit.dataset.pa];
    if (!run) return;
    if (ev.preventDefault) ev.preventDefault();
    run();
  }

  function onDismiss() {
    close();
  }

  function startTicker() {
    if (ticker === null) ticker = setInterval_(tick, TICK_MS);
  }

  function stopTicker() {
    if (ticker === null) return;
    clearInterval_(ticker);
    ticker = null;
  }

  function open() {
    dispatch({ type: "OPEN" });
    startTicker();
  }

  function close() {
    stopTicker();
    leave({ type: "CLOSE" });
  }

  function listen(node, type, fn) {
    if (!node) return;
    node.addEventListener(type, fn);
    cleanups.push(() => node.removeEventListener(type, fn));
  }

  function bind() {
    const live = el("#pair_live");
    if (live) live.setAttribute("aria-live", "polite");
    listen(el("#pair_actions"), "click", onActionClick);
    listen(el("#pair_x"), "click", onDismiss);
    render();
  }

  function destroy() {
    stopTicker();
    while (cleanups.length) cleanups.pop()();
  }

  return {
    bind,
    destroy,
    open,
    close,
    /* The shell asks this before letting a backdrop tap or a swipe take
       the sheet. A live credential says no; the ✕ is the way out. */
    dismissable: () => !LIVE_SCREENS.has(state.screen),
    isOpen: () => state.screen !== "closed",
    screen: () => state.screen,
    code: () => state.code,
    error: () => state.error,
    setVisible: (visible) => dispatch({ type: "VISIBILITY", visible: !!visible }),
    /* Test seam: the queue drains to quiescence, including the effects
       that earlier effects enqueued. */
    async settled() {
      let prev;
      do {
        prev = chain;
        await chain;
      } while (chain !== prev);
    },
  };
}

/* ---------- the paired-device list ----------
 *
 * The other half of a grant. Pairing hands a device SSH-equivalent
 * authority on this computer, so the list of who holds it, and the way to
 * take it back, belong to the same feature rather than to a later phase.
 *
 * Owned root: #m_devices (delegated click on [data-dev]).
 * Server calls: GET /api/remote/devices, DELETE /api/remote/devices/:id.
 * No timers: the list is read when the menu opens and after a revoke,
 * never polled — a grant does not change behind the human's back.
 */
export function createDeviceList({ api, doc } = {}) {
  /* The id of the row whose next tap revokes. At most one, ever: two
     armed rows means a stray tap takes out a device the human armed a
     minute ago and then thought better of. */
  let armed = "";
  let devices = [];
  /* Whether `devices` is anything at all, as opposed to "we have never
     managed to read the list". The difference decides whether an empty
     render is allowed to say "no devices paired". */
  let known = false;
  let notice = "";
  let chain = Promise.resolve();
  const cleanups = [];

  const root = () => doc.querySelector("#m_devices");

  function enqueue(fn) {
    chain = chain.then(fn).catch(() => {});
    return chain;
  }

  function rowHTML(d) {
    const id = String((d && d.id) || "");
    /* A row that says nothing is a row nobody dares revoke, so an
       unlabelled device falls back to the only other thing that
       identifies it. */
    const name = (d && d.label) || id;
    const armedRow = armed === id;
    return (
      `<div class="item"><span>${esc(name)}</span>` +
      `<button type="button" data-dev="${esc(id)}"` +
      (armedRow ? ` class="danger"` : "") +
      `>${armedRow ? "Confirm revoke" : "Revoke"}</button></div>`
    );
  }

  function render() {
    const host = root();
    if (!host) return;
    let html = devices.map(rowHTML).join("");
    if (!html && known) html = `<div class="item note">No devices paired.</div>`;
    if (notice) html += `<div class="item err">${esc(notice)}</div>`;
    host.innerHTML = html;
  }

  async function refresh() {
    try {
      const r = await api("/api/remote/devices", {});
      devices = (r && r.devices) || [];
      known = true;
      notice = "";
    } catch (e) {
      /* Never blank the list on a failed read. An empty list reads as
         "nothing is paired", which is the most dangerous sentence this
         surface can say when it is not true — and on a first read it
         would be a claim made from no evidence at all. */
      notice = "Could not read the paired devices: " + ((e && e.message) || "unknown error");
    }
    armed = "";
    render();
  }

  function revoke(id) {
    enqueue(async () => {
      try {
        await api(`/api/remote/devices/${encodeURIComponent(id)}`, { method: "DELETE" });
        await refresh();
      } catch (e) {
        /* No disarming here: onClick cleared `armed` before it called us,
           so a second authority over the same field could only disagree. */
        notice = "Could not revoke that device: " + ((e && e.message) || "unknown error");
        render();
      }
    });
  }

  function onClick(ev) {
    const hit = ev && ev.target && ev.target.closest && ev.target.closest("[data-dev]");
    if (!hit) return;
    if (ev.preventDefault) ev.preventDefault();
    const id = hit.dataset.dev;
    if (armed === id) {
      armed = "";
      revoke(id);
      return;
    }
    armed = id;
    render();
  }

  function bind() {
    const host = root();
    if (!host) return;
    host.addEventListener("click", onClick);
    cleanups.push(() => host.removeEventListener("click", onClick));
  }

  return {
    bind,
    destroy() {
      while (cleanups.length) cleanups.pop()();
    },
    refresh: () => enqueue(refresh),
    async settled() {
      let prev;
      do {
        prev = chain;
        await chain;
      } while (chain !== prev);
    },
  };
}

/* F1 — unlinking this computer.
 *
 * The counterpart to the revoke two rows up. A paired device could always
 * be handed back; the computer's own enrollment could only be abandoned,
 * which is a bad trade to offer someone the day you ask them to try
 * remote access.
 *
 * The route underneath does the local unlink unconditionally and the
 * rendezvous release best-effort, so this control's real job is to report
 * which halves happened. released:false is not a failure — the computer is
 * unlinked either way — but it is something the user must be told, because
 * only the operator can strike off an installation the rendezvous still
 * holds. */
export function createUnlinkControl({ api, doc, onUnlinked } = {}) {
  let armed = false;
  let inFlight = false;
  /* Whether there is an enrollment to unlink at all. An action that
     cannot do anything is worse than no action. */
  let enrolled = true;
  /* Whether the status has ever been read. A read that failed is not
     evidence that anything changed, so it keeps the last known answer —
     but before there is one, nothing is claimed and nothing is offered. */
  let known = false;
  let done = false;
  let notice = "";
  /* Whether the notice is a failure. A message that reads the same
     whether the computer was unlinked or not is no message at all. */
  let noticeBad = false;
  let chain = Promise.resolve();
  const cleanups = [];

  const btn = () => doc.querySelector("#m_unlink");
  const note = () => doc.querySelector("#m_unlink_note");

  function enqueue(fn) {
    chain = chain.then(fn).catch(() => {});
    return chain;
  }

  function render() {
    const b = btn();
    if (b) {
      b.hidden = !enrolled || done;
      b.innerHTML = armed ? "Confirm unlink" : "Unlink this computer";
      b.className = armed ? "cta danger" : "cta";
    }
    const n = note();
    if (n) {
      n.innerHTML = notice
        ? `<div class="item ${noticeBad ? "err" : "note"}">${esc(notice)}</div>`
        : "";
      n.hidden = !notice;
    }
  }

  function unlink() {
    inFlight = true;
    enqueue(async () => {
      try {
        const r = await api("/api/remote/unenroll", { method: "POST" });
        done = true;
        noticeBad = false;
        notice = r && r.released
          ? "Unlinked. This computer is no longer enrolled."
          : "Unlinked locally, but the rendezvous could not be reached. " +
            "Ask whoever issued the invite to revoke this installation.";
        if (onUnlinked) onUnlinked();
      } catch (e) {
        /* The identity is still on disk, so this computer is still
           enrolled. Saying otherwise is the one lie this surface must
           never tell — the action stays offerable. */
        noticeBad = true;
        notice = "Could not unlink this computer: " + ((e && e.message) || "unknown error");
      } finally {
        inFlight = false;
        render();
      }
    });
  }

  function onClick(ev) {
    if (ev && ev.preventDefault) ev.preventDefault();
    if (inFlight || done || !enrolled) return;
    if (armed) {
      armed = false;
      unlink();
      render();
      return;
    }
    armed = true;
    render();
  }

  /* The status is the only thing that knows whether this computer has an
     identity on disk at all. revoked and unavailable still do, and those
     are precisely the installations someone wants rid of, so the test is
     "any enrollment" rather than "a working one". */
  async function refresh() {
    try {
      const r = await api("/api/remote/status", {});
      enrolled = !!(r && r.hosted);
      known = true;
      if (!enrolled) {
        armed = false;
        done = false;
        notice = "";
      }
    } catch {
      if (!known) enrolled = false;
    }
    render();
  }

  return {
    refresh: () => enqueue(refresh),
    bind() {
      const b = btn();
      if (!b) return;
      b.addEventListener("click", onClick);
      cleanups.push(() => b.removeEventListener("click", onClick));
      render();
    },
    /* Hiding disarms: otherwise the arming survives out of sight and the
       next tap, minutes later in a different frame of mind, is the
       confirming one. */
    setEnrolled(v) {
      enrolled = !!v;
      if (!enrolled) {
        armed = false;
        done = false;
        notice = "";
      }
      render();
    },
    destroy() {
      while (cleanups.length) cleanups.pop()();
    },
    async settled() {
      let prev;
      do {
        prev = chain;
        await chain;
      } while (chain !== prev);
    },
  };
}
