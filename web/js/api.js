/* CSRF fetch wrapper, response decoding, and /api/ui request contracts.
 *
 * Extracted in Packet 6E and wired into the sole browser entry in Packet 6F.
 * Named exports are exercised directly by Node tests and used by production.
 *
 * Explicit inputs (no CSRF global / document / window / localStorage / network):
 * - csrf token is supplied to withCsrf / api / UI put builders
 * - fetchImpl is supplied wherever a network call is made
 * - HeadersImpl defaults to the global Headers (injectable for tests)
 * - storage uses the state.js adapter shape { getItem, setItem }
 *
 * State transitions for load/flush/poll compose with state.js reducers. They
 * return plain { doc, rev, ops, loaded, saving, applied } objects and never
 * touch DOM, timers, addEventListener, or render* helpers.
 *
 * No document, window, localStorage, navigator, setTimeout, or app globals.
 */

import {
  normUI,
  adoptRemoteAndReplay,
  loadDocumentWithReplay,
  loadPendingOps,
  savePendingOps,
  saveCachedUI,
  loadCachedUI,
} from "./state.js";

/* Match the inline UNSAFE method detector (case-insensitive). */
export const UNSAFE = /^(POST|PUT|PATCH|DELETE)$/i;

/* Shared UI document path used by every GET/PUT contract. */
export const UI_PATH = "/api/ui";

/* flushUI retry ceiling — four attempts including the first. */
export const MAX_FLUSH_ATTEMPTS = 4;

/* CSRF header name (server middleware expects this exact key). */
export const CSRF_HEADER = "X-Scimux-CSRF";

/* ---------- CSRF / generic api ---------- */

/* Attach CSRF and optional JSON Content-Type for unsafe methods.
   Safe methods (GET/HEAD/OPTIONS/…) return opts unchanged (same reference).
   For unsafe methods: existing headers are retained, X-Scimux-CSRF is
   overwritten with the supplied token, and Content-Type becomes
   application/json only when body is a string and no Content-Type is set.
   FormData/multipart bodies are not strings, so they keep their own type. */
export function withCsrf(opts = {}, csrf = "", HeadersImpl = Headers) {
  if (!UNSAFE.test(opts.method || "GET")) return opts;
  const h = new HeadersImpl(opts.headers || {});
  h.set(CSRF_HEADER, csrf);
  /* JSON bodies are strings; multipart (FormData) sets its own content type */
  if (typeof opts.body === "string" && !h.has("Content-Type"))
    h.set("Content-Type", "application/json");
  return { ...opts, headers: h };
}

/* Decode a successful Response the way the inline `api` helper does.
   Non-OK → Error with response text message and numeric .status.
   content-type containing "json" → r.json() (malformed JSON rejects).
   otherwise → r.text(). */
export async function decodeResponse(r) {
  if (!r.ok) {
    const err = new Error(await r.text());
    err.status = r.status; /* callers can tell a 409 conflict from a failure */
    throw err;
  }
  return r.headers.get("content-type")?.includes("json") ? r.json() : r.text();
}

/* Generic fetch wrapper: withCsrf then decodeResponse.
   deps.fetchImpl is required; deps.csrf defaults to ""; deps.HeadersImpl
   defaults to global Headers. */
export async function api(path, opts, deps = {}) {
  const { fetchImpl, csrf = "", HeadersImpl = Headers } = deps;
  const r = await fetchImpl(path, withCsrf(opts, csrf, HeadersImpl));
  return decodeResponse(r);
}

/* ---------- /api/ui request contracts ---------- */

/* Unconditional initial GET — no conditional headers. */
export function uiInitialGetRequest() {
  return { path: UI_PATH, opts: {} };
}

/* Conditional poll GET. If-None-Match is present only when rev is non-empty.
   Empty/missing rev still sends an empty headers object (matches inline). */
export function uiPollGetRequest(rev) {
  return {
    path: UI_PATH,
    opts: { headers: rev ? { "If-None-Match": rev } : {} },
  };
}

/* Conditional PUT options (no fetch). Caller supplies csrf.
   Always sets exact If-Match to the provided rev string — never "*".
   keepalive is included only when true (pagehide path). */
export function uiPutRequest(doc, rev, { csrf = "", keepalive = false, HeadersImpl = Headers } = {}) {
  const put = {
    method: "PUT",
    headers: { "If-Match": rev, "Content-Type": "application/json" },
    body: JSON.stringify(doc),
  };
  if (keepalive) put.keepalive = true;
  return {
    path: UI_PATH,
    opts: withCsrf(put, csrf, HeadersImpl),
  };
}

/* Read ETag from a Response/Headers-like object; missing → "". */
export function readETag(r) {
  return r.headers.get("ETag") || "";
}

/* Conflict statuses that trigger refetch+replay in flushUI. */
export function isConflictStatus(status) {
  return status === 409 || status === 428;
}

/* pagehide gate: send only when loaded, pending work exists, and a revision
   is known (never If-Match: *). */
export function pagehideShouldSend({ loaded, ops, rev }) {
  return !!(loaded && ops && ops.length && rev);
}

/* Build the pagehide keepalive PUT when the gate passes; null otherwise. */
export function pagehidePutRequest(doc, { loaded, ops, rev, csrf = "", HeadersImpl = Headers } = {}) {
  if (!pagehideShouldSend({ loaded, ops, rev })) return null;
  return uiPutRequest(doc, rev, { csrf, keepalive: true, HeadersImpl });
}

/* ---------- DOM-free UI sync transitions ----------
 *
 * These mirror loadUI / flushUI / pollUI HTTP+state decisions without DOM,
 * timers, or render side effects. Callers receive an `applied` flag when the
 * inline path would have called applyRemoteUI. storage is optional: omit it
 * to skip localStorage mirrors (tests that only care about network can omit).
 */

/* Snapshot-shaped state for the sync helpers. */
export function emptyUISyncState() {
  return {
    doc: normUI({}),
    rev: "",
    ops: [],
    loaded: false,
    saving: false,
  };
}

/* loadUI: unconditional GET, adopt remote+ETag, mirror cache, merge persisted
   then session ops with the load filter, replay, mark loaded. On fetch/parse
   failure, try the cached document; a null cache leaves doc unchanged. */
export async function loadUIState(state, deps = {}) {
  const { fetchImpl, storage } = deps;
  let doc = state.doc;
  let rev = state.rev;
  let sessionOps = state.ops || [];
  let applied = false;

  try {
    const { path, opts } = uiInitialGetRequest();
    const r = await fetchImpl(path, opts);
    if (!r.ok) throw new Error();
    rev = readETag(r);
    doc = normUI(await r.json());
    if (storage) saveCachedUI(doc, storage);
  } catch {
    if (storage) {
      const cached = loadCachedUI(storage);
      if (cached !== null) doc = cached;
      /* parse failure → leave current document unchanged */
    }
  }

  const persisted = storage ? loadPendingOps(storage) : [];
  const { doc: nextDoc, ops } = loadDocumentWithReplay(doc, persisted, sessionOps, rev);
  doc = nextDoc;
  applied = true;

  return {
    doc,
    rev,
    ops,
    loaded: true,
    saving: false,
    applied,
    shouldFlush: ops.length > 0,
  };
}

/* flushUI: gated on !saving && loaded && ops.length. A missing revision first
   adopts the server document (never If-Match: *), filters/replays, then PUTs
   up to MAX_FLUSH_ATTEMPTS times. 409/428 refetch+replay; offline/5xx keep
   ops. No DOM/render — returns next state plus applied when remote was adopted. */
export async function flushUIState(state, deps = {}) {
  const {
    fetchImpl,
    csrf = "",
    storage,
    HeadersImpl = Headers,
    onRemoteApplied = () => {},
  } = deps;
  let doc = state.doc;
  let rev = state.rev || "";
  /* Keep the caller's live queue reference until a replay filter deliberately
     replaces it. uiMutate can append while a fetch is in flight; the inline
     flush observes those additions before conflict/adoption replay. */
  let ops = state.ops || [];
  let applied = false;

  if (state.saving || !state.loaded || !ops.length) {
    return {
      doc,
      rev,
      ops,
      loaded: state.loaded,
      saving: state.saving,
      applied: false,
      skipped: true,
    };
  }

  let saving = true;
  try {
    /* R21.1: no-revision writer adopts first — never PUT with If-Match: *. */
    if (!rev) {
      let g;
      try {
        const req = uiInitialGetRequest();
        g = await fetchImpl(req.path, req.opts);
      } catch {
        return finish(doc, rev, ops, true, applied);
      }
      if (!g.ok) return finish(doc, rev, ops, true, applied);
      rev = readETag(g);
      const adopted = adoptRemoteAndReplay(await g.json(), ops, rev);
      doc = adopted.doc;
      ops = adopted.ops;
      applied = true;
      /* Inline order: publish/render the adopted remote before persisting the
         queue/cache. A bare cache write may throw after the UI is visible. */
      onRemoteApplied({ doc, rev, ops });
      if (storage) {
        savePendingOps(ops, storage);
        saveCachedUI(doc, storage);
      }
      if (!ops.length) return finish(doc, rev, ops, true, applied);
    }

    for (let attempt = 0; attempt < MAX_FLUSH_ATTEMPTS && ops.length; attempt++) {
      let r;
      try {
        const put = uiPutRequest(doc, rev, { csrf, HeadersImpl });
        r = await fetchImpl(put.path, put.opts);
      } catch {
        break; /* offline: keep ops, retry next tick */
      }
      if (r.ok) {
        rev = readETag(r);
        ops = [];
        if (storage) {
          savePendingOps(ops, storage);
          saveCachedUI(doc, storage);
        }
        break;
      }
      if (isConflictStatus(r.status)) {
        let g;
        try {
          const req = uiInitialGetRequest();
          g = await fetchImpl(req.path, req.opts);
        } catch {
          break;
        }
        if (!g.ok) break;
        rev = readETag(g);
        const adopted = adoptRemoteAndReplay(await g.json(), ops, rev);
        doc = adopted.doc;
        ops = adopted.ops;
        applied = true;
        if (storage) savePendingOps(ops, storage);
        /* Conflict order: persist the filtered queue, publish/render, retry. */
        onRemoteApplied({ doc, rev, ops });
        continue;
      }
      break; /* 5xx/413: keep ops, show unsynced */
    }
  } finally {
    saving = false;
  }

  return finish(doc, rev, ops, false /* saving already false */, applied);

  function finish(d, v, o, _savingDone, app) {
    return {
      doc: d,
      rev: v,
      ops: o,
      loaded: true,
      saving: false,
      applied: app,
      skipped: false,
      pending: o.length > 0,
    };
  }
}

/* pollUI: skip while not loaded, saving, or local ops in flight.
   Conditional GET; 304/non-OK/offline retain state. Equal document still
   updates rev but does not rewrite cache or signal applied. */
export async function pollUIState(state, deps = {}) {
  const { fetchImpl, storage } = deps;
  let doc = state.doc;
  let rev = state.rev || "";
  const ops = state.ops || [];

  if (!state.loaded || state.saving || ops.length) {
    return {
      doc,
      rev,
      ops,
      loaded: state.loaded,
      saving: state.saving,
      applied: false,
      skipped: true,
    };
  }

  let r;
  try {
    const req = uiPollGetRequest(rev);
    r = await fetchImpl(req.path, req.opts);
  } catch {
    return {
      doc,
      rev,
      ops,
      loaded: true,
      saving: false,
      applied: false,
      skipped: false,
      retained: true,
    };
  }
  if (r.status === 304 || !r.ok) {
    return {
      doc,
      rev,
      ops,
      loaded: true,
      saving: false,
      applied: false,
      skipped: false,
      retained: true,
    };
  }
  rev = readETag(r);
  const remote = normUI(await r.json());
  if (JSON.stringify(remote) === JSON.stringify(doc)) {
    return {
      doc,
      rev,
      ops,
      loaded: true,
      saving: false,
      applied: false,
      skipped: false,
      unchanged: true,
    };
  }
  doc = remote;
  if (storage) saveCachedUI(doc, storage);
  return {
    doc,
    rev,
    ops,
    loaded: true,
    saving: false,
    applied: true,
    skipped: false,
  };
}
