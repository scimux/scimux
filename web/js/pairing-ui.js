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
           further devices. It needs its own screen: "go to the laptop"
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
          body: JSON.stringify({ laptop_confirm: true, device_confirm: true }),
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
