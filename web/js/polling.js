/* State tick + UI document sync coordination.
 *
 * Packet 7I ownership inventory
 * -----------------------------
 * Owned state:
 *   - stateEtag          /api/state conditional GET revision
 *   - UI / uiRev / uiOps  shared ui.json document, ETag, pending local ops
 *   - uiLoaded / uiSaving write gates; saveGate / tickGate stall takeover
 *   - uiTimer            300ms coalesced flush timer
 *   - tickGate           non-overlap gate for direct/fast ticks
 *   - pollTimer / pollGen visible-tab 2s generation/cancellation loop
 *   - bound / destroyed  lifecycle flags
 *
 * Owned requests (orchestration only — HTTP shapes live in api.js):
 *   - GET /api/state with optional If-None-Match: stateEtag
 *   - loadUIState / flushUIState / pollUIState / pagehidePutRequest
 *
 * Owned timers:
 *   - pollTimer   (2s after tick completion, same generation only)
 *   - uiTimer     (exact 300ms coalesce for flushUI)
 *
 * Owned event targets (injected; single owner):
 *   - document "visibilitychange"  pause when hidden; startPolling when visible
 *   - window   "pagehide"          keepalive PUT when gate passes
 *
 * Storage (injected adapter; keys owned by state.js):
 *   - pending ops + cached UI via queueLocalOp / savePendingOps / saveCachedUI
 *     and the api.js load/flush/poll helpers — never reimplemented here
 *
 * Injected transition helpers / feature effects:
 *   - setHostOnline, setServerUnreachable, setHostname, setVersion
 *   - publishState ({ nodes, unadopted }) — shell owns the arrays
 *   - renderSys(sys) — metrics HTML stays shell-owned
 *   - ensureSelection() — selected-node fallback (sel/nodeById/select stay shell)
 *   - updateCardAges, renderCards, renderMap, renderMapTabs, renderBookmarksPane
 *   - renderChatHead, refreshChat (async), renderWsInbox
 *   - invalidateCardsSig, invalidateMap, invalidateBookmarks, invalidateChat
 *   - onStartPolling — usage prime only (badge HTML + 30s cadence stay shell)
 *   - onPageVisibility(visible) — sole fan-out of the document's
 *     visibilitychange to features that must pause when the page hides
 *   - syncwarnEl — exact pending/saved warning text
 *
 * Explicit non-ownership:
 *   - feature HTML / feature signature algorithms
 *   - selection implementation, title editing
 *   - archived / update / license surfaces
 *   - spatial gestures, shell controls, toast
 *   - usage badge HTML, status-phase flip, renderSys markup
 *   - app.js / boot composition (Packet 7J)
 *   - HTTP/reducer/conflict/replay algorithms (api.js + state.js)
 *
 * Contracts preserved:
 *   - Network failure → offline + "server unreachable"; back online clears it
 *   - 304 → online, ages, await chat, fire-and-forget UI poll (no structural rebuild)
 *   - non-OK → offline
 *   - 200 → ETag/nodes/unadopted/hostname/version/sys → selection fallback
 *           → Cards → Map → Bookmarks → await Chat → UI poll
 *   - UI poll never awaited by the state tick
 *   - Direct fast ticks non-overlapping; finally always releases the gate,
 *     and a holder stalled past STALLED_REQUEST_MS is taken over
 *   - A tick whose render effects throw still reschedules the loop
 *   - Stale generations cannot reschedule; 2s delay only after tick completes
 *   - Hidden pause + immediate visible resume via startPolling
 *   - Exact sync-warning: "syncing…" / "changes not saved yet" / clear
 *   - Persisted-before-session replay, no If-Match: *, max four conflicts
 *   - Remote publication invalidation/render order
 *   - Pagehide gate; thrown fetch silenced
 *   - Polls never rebuild composer or clobber active editors (feature sig APIs)
 *
 * Lifecycle:
 *   - bind() idempotent; installs visibility + pagehide listeners once
 *   - destroy() removes listeners, clears timers, bumps generation, ignores
 *     late fetch/timer results
 *   - startPolling / stop (via hidden) / tick / loadUI / uiMutate public
 */

import {
  pagehidePutRequest,
  loadUIState,
  flushUIState,
  pollUIState,
} from "./api.js";
import {
  emptyUI,
  queueLocalOp,
  savePendingOps,
  saveCachedUI,
} from "./state.js";

const FLUSH_COALESCE_MS = 300;
const POLL_INTERVAL_MS = 2000;

/* How long a request may hold its gate before the next one takes over.
 * A gate is released in `finally`, so a request that neither resolves nor
 * rejects would hold one for the life of the page — polling or saving stops
 * dead while the connection itself is healthy. Nothing else bounds that:
 * the tunnel has no per-request timeout by design, and the "disconnected"
 * state a sleeping phone sits in is deliberately treated as recoverable
 * rather than lost. Long enough that a slow host is never overtaken. */
export const STALLED_REQUEST_MS = 20000;

/* A latch a stalled request cannot hold shut. The token identifies the current
 * holder, so an abandoned one can neither free its successor's gate nor act on
 * its own late answer. That second half is not tidiness: a taken-over request
 * settles as lost long after the successor succeeded, and letting it run the
 * failure path repaints "server unreachable" over a working connection. */
function makeGate(now) {
  let at = 0;
  let seq = 0;
  let held = false;
  return {
    /** Token to act and release with, or 0 when another holder is inside. */
    enter() {
      if (held && now() - at < STALLED_REQUEST_MS) return 0;
      held = true;
      at = now();
      return ++seq;
    },
    /** Whether this token is still the current holder — check after an await. */
    holds(token) {
      return token === seq;
    },
    /** Releases only for the current holder; reports whether it did. */
    release(token) {
      if (token !== seq) return false;
      held = false;
      return true;
    },
  };
}

const SYNC_PENDING = "syncing…";
const SYNC_FAILED = "changes not saved yet";

/** Exact sync-warning text for a given pending/failed state. */
export function syncWarningText({ pending, failed } = {}) {
  if (failed) return SYNC_FAILED;
  if (pending) return SYNC_PENDING;
  return "";
}

/** Build /api/state request options from the current state ETag. */
export function stateGetRequest(stateEtag) {
  return {
    path: "/api/state",
    opts: { headers: stateEtag ? { "If-None-Match": stateEtag } : {} },
  };
}

/**
 * Create the polling + UI-sync coordinator.
 * All DOM, network, timer, and feature effects are injected.
 */
export function createPollingFeature(deps = {}) {
  if (typeof deps.fetchImpl !== "function") {
    throw new Error("createPollingFeature: fetchImpl is required");
  }
  const {
    document: doc = globalThis.document,
    window: win = globalThis.window,
    storage,
    fetchImpl,
    csrf = "",
    setTimeout: setTimeoutImpl = globalThis.setTimeout?.bind(globalThis),
    clearTimeout: clearTimeoutImpl = globalThis.clearTimeout?.bind(globalThis),
    now = () => Date.now(),
    syncwarnEl = null,
    setHostOnline = () => {},
    setServerUnreachable = () => {},
    setHostname = () => {},
    setVersion = () => {},
    publishState = () => {},
    renderSys = () => {},
    ensureSelection = () => {},
    updateCardAges = () => {},
    renderCards = () => {},
    renderMap = () => {},
    renderMapTabs = () => {},
    renderBookmarksPane = () => {},
    renderChatHead = () => {},
    refreshChat = async () => {},
    renderWsInbox = () => {},
    invalidateCardsSig = () => {},
    invalidateMap = () => {},
    invalidateBookmarks = () => {},
    invalidateChat = () => {},
    onStartPolling = () => {},
    onPageVisibility = () => {},
  } = deps;

  let stateEtag = "";
  let UI = emptyUI();
  let uiRev = "";
  let uiOps = [];
  let uiLoaded = false;
  let uiSaving = false;
  let uiTimer = null;
  const saveGate = makeGate(now);
  const tickGate = makeGate(now);
  let pollTimer = null;
  let pollGen = 0;
  let bound = false;
  let destroyed = false;

  const onVisibility = () => {
    if (destroyed) return;
    /* Told before we act on it ourselves: this listener is the document's
       single owner of visibilitychange, so anything else that must pause
       when the page goes away hears about it here rather than adding a
       second listener with its own idea of what "hidden" means. */
    onPageVisibility(!doc.hidden);
    if (doc.hidden) {
      if (pollTimer) clearTimeoutImpl(pollTimer);
      pollTimer = null;
    } else {
      startPolling();
    }
  };

  const onPagehide = () => {
    if (destroyed) return;
    const req = pagehidePutRequest(UI, {
      loaded: uiLoaded,
      ops: uiOps,
      rev: uiRev,
      csrf,
    });
    if (!req) return;
    try {
      /* Fire-and-forget: preserve the original sync-throw-only catch. */
      fetchImpl(req.path, req.opts);
    } catch {
      /* keepalive unload: silence */
    }
  };

  function renderSyncState() {
    if (!syncwarnEl) return;
    syncwarnEl.textContent = syncWarningText({ pending: uiOps.length > 0 });
  }

  function applyRemoteUI() {
    invalidateCardsSig();
    invalidateMap();
    invalidateBookmarks();
    renderCards();
    renderChatHead();
    renderMapTabs();
    renderMap();
    renderBookmarksPane();
  }

  function uiMutate(op) {
    if (destroyed) return;
    queueLocalOp(UI, op, uiRev, uiOps);
    savePendingOps(uiOps, storage);
    saveCachedUI(UI, storage);
    renderSyncState();
    renderBookmarksPane();
    renderWsInbox();
    clearTimeoutImpl(uiTimer);
    uiTimer = setTimeoutImpl(() => {
      uiTimer = null;
      flushUI();
    }, FLUSH_COALESCE_MS);
  }

  async function flushUI() {
    if (destroyed) return;
    if (!uiLoaded || !uiOps.length) return;
    const token = saveGate.enter();
    if (!token) return;
    uiSaving = true;   /* the value api.js reads, not the latch */
    try {
      const next = await flushUIState(
        { doc: UI, rev: uiRev, ops: uiOps, loaded: uiLoaded, saving: false },
        {
          fetchImpl,
          csrf,
          storage,
          onRemoteApplied: (remote) => {
            if (destroyed) return;
            UI = remote.doc;
            uiRev = remote.rev;
            uiOps = remote.ops;
            applyRemoteUI();
          },
        },
      );
      if (destroyed || !saveGate.holds(token)) return;
      UI = next.doc;
      uiRev = next.rev;
      uiOps = next.ops;
    } finally {
      if (saveGate.release(token)) {
        uiSaving = false;
        if (!destroyed) {
          renderSyncState();
          if (uiOps.length && syncwarnEl) {
            syncwarnEl.textContent = syncWarningText({ failed: true });
          }
        }
      }
    }
  }

  async function loadUI() {
    if (destroyed) return;
    const next = await loadUIState(
      { doc: UI, rev: uiRev, ops: uiOps, loaded: uiLoaded, saving: uiSaving },
      { fetchImpl, storage },
    );
    if (destroyed) return;
    UI = next.doc;
    uiRev = next.rev;
    uiOps = next.ops;
    uiLoaded = next.loaded;
    invalidateChat();
    applyRemoteUI();
    if (next.shouldFlush) flushUI();
  }

  async function pollUI() {
    if (destroyed) return;
    const next = await pollUIState(
      { doc: UI, rev: uiRev, ops: uiOps, loaded: uiLoaded, saving: uiSaving },
      { fetchImpl, storage },
    );
    if (destroyed) return;
    uiRev = next.rev;
    if (next.skipped || next.retained || next.unchanged || !next.applied) return;
    UI = next.doc;
    applyRemoteUI();
  }

  async function tick() {
    if (destroyed) return;
    const token = tickGate.enter();
    if (!token) return;
    try {
      let r;
      try {
        const req = stateGetRequest(stateEtag);
        /* A 2s ETag poll must not be served from the HTTP cache. */
        r = await fetchImpl(req.path, { ...req.opts, cache: "no-store" });
      } catch (err) {
        if (destroyed || !tickGate.holds(token)) return;
        /* Invisible to the user, but the only record of why a request failed. */
        globalThis.console?.debug?.("scimux: state request failed", err);
        setHostOnline(false);
        setServerUnreachable();
        return;
      }
      if (destroyed || !tickGate.holds(token)) return;
      if (r.status === 304) {
        setHostOnline(true);
        updateCardAges();
        await refreshChat();
        if (destroyed) return;
        pollUI(); /* fire-and-forget */
        return;
      }
      if (!r.ok) {
        setHostOnline(false);
        return;
      }
      setHostOnline(true);
      stateEtag = r.headers.get("ETag") || "";
      const st = await r.json();
      if (destroyed) return;
      publishState({
        nodes: st.nodes || [],
        unadopted: st.unadopted || [],
      });
      if (st.hostname) setHostname(st.hostname);
      if (st.version) setVersion(st.version);
      renderSys(st.sys);
      ensureSelection();
      renderCards();
      renderMap();
      renderBookmarksPane();
      await refreshChat();
      if (destroyed) return;
      pollUI(); /* fire-and-forget */
    } finally {
      tickGate.release(token);
    }
  }

  async function pollLoop(gen) {
    try {
      await tick();
    } catch (err) {
      /* A render effect that throws must not stop the clock: an unscheduled
         loop is a page that silently freezes and reads as an offline
         computer, which is exactly what it is not. */
      globalThis.console?.error("scimux: poll tick failed", err);
    }
    if (destroyed) return;
    if (!doc.hidden && gen === pollGen) {
      pollTimer = setTimeoutImpl(() => pollLoop(gen), POLL_INTERVAL_MS);
    }
  }

  function startPolling() {
    if (destroyed) return;
    if (pollTimer) clearTimeoutImpl(pollTimer);
    pollTimer = null;
    pollLoop(++pollGen);
    onStartPolling();
  }

  function invalidateStateEtag() {
    stateEtag = "";
  }

  function bind() {
    if (bound || destroyed) return;
    bound = true;
    doc.addEventListener("visibilitychange", onVisibility);
    win.addEventListener("pagehide", onPagehide);
  }

  function destroy() {
    if (destroyed) return;
    destroyed = true;
    pollGen++;
    if (pollTimer) clearTimeoutImpl(pollTimer);
    pollTimer = null;
    if (uiTimer) clearTimeoutImpl(uiTimer);
    uiTimer = null;
    if (bound) {
      doc.removeEventListener("visibilitychange", onVisibility);
      win.removeEventListener("pagehide", onPagehide);
      bound = false;
    }
  }

  return {
    bind,
    destroy,
    tick,
    startPolling,
    loadUI,
    uiMutate,
    invalidateStateEtag,
    getUI: () => UI,
    isUILoaded: () => uiLoaded,
  };
}
