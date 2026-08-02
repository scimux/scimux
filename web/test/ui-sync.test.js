/* Characterization tests for /api/ui ETag sync flows composed from api.js
 * request contracts + state.js reducers. Scripted fake fetch only; no real
 * network, DOM, timers, or localStorage global. */
import test from "node:test";
import assert from "node:assert/strict";
import {
  UI_PATH,
  MAX_FLUSH_ATTEMPTS,
  CSRF_HEADER,
  emptyUISyncState,
  loadUIState,
  flushUIState,
  pollUIState,
  pagehideShouldSend,
  pagehidePutRequest,
  uiPutRequest,
} from "../js/api.js";
import {
  CACHED_UI_KEY,
  normUI,
  queueLocalOp,
  loadPendingOps,
  savePendingOps,
  saveCachedUI,
} from "../js/state.js";

/* ---------- fakes ---------- */
function memStorage(seed = {}) {
  const map = new Map(Object.entries(seed));
  return {
    getItem(k) {
      return map.has(k) ? map.get(k) : null;
    },
    setItem(k, v) {
      map.set(k, String(v));
    },
    _map: map,
  };
}

function jsonResponse({ status = 200, body = {}, etag = "", ok } = {}) {
  const isOk = ok !== undefined ? ok : status >= 200 && status < 300;
  const headers = new Headers();
  if (etag) headers.set("ETag", etag);
  headers.set("Content-Type", "application/json");
  return {
    ok: isOk,
    status,
    headers,
    async json() {
      return typeof body === "string" ? JSON.parse(body) : body;
    },
    async text() {
      return typeof body === "string" ? body : JSON.stringify(body);
    },
  };
}

function scriptedFetch(handlers) {
  const calls = [];
  const fetchImpl = async (path, opts = {}) => {
    const entry = { path, opts, method: opts.method || "GET" };
    calls.push(entry);
    const h = handlers[calls.length - 1];
    if (!h) throw new Error(`unexpected fetch #${calls.length}: ${entry.method} ${path}`);
    if (typeof h === "function") return h(path, opts, entry);
    if (h instanceof Error) throw h;
    return h;
  };
  return { fetchImpl, calls };
}

function baseDoc(extra = {}) {
  return normUI({ groups: [], archived: [], bookmarks: [], lanes: [], pinned: [], ...extra });
}

/* ---------- loadUI ---------- */
test("loadUI: successful GET adopts server document and revision", async () => {
  const remote = baseDoc({ archived: ["n1"], bookmarks: [{ t: 1, text: "a" }] });
  const { fetchImpl, calls } = scriptedFetch([
    jsonResponse({ body: remote, etag: '"r1"' }),
  ]);
  const storage = memStorage();
  const out = await loadUIState(emptyUISyncState(), { fetchImpl, storage });
  assert.equal(calls.length, 1);
  assert.equal(calls[0].path, UI_PATH);
  assert.equal(calls[0].method, "GET");
  assert.equal(calls[0].opts.headers, undefined);
  assert.equal(out.rev, '"r1"');
  assert.deepEqual(out.doc.archived, ["n1"]);
  assert.equal(out.loaded, true);
  assert.equal(out.shouldFlush, false);
  assert.equal(JSON.parse(storage.getItem(CACHED_UI_KEY)).archived[0], "n1");
});

test("loadUI: persisted operations precede current-session operations", async () => {
  const remote = baseDoc({ bookmarks: [] });
  const { fetchImpl } = scriptedFetch([jsonResponse({ body: remote, etag: '"r1"' })]);
  const storage = memStorage();
  // Persisted (previous page): bookmark-add t=1
  savePendingOps([{ k: "bookmark-add", bookmark: { t: 1, text: "persisted" }, rev: '"r1"' }], storage);
  // Session (this page, before load finished): bookmark-add t=2
  const session = [{ k: "bookmark-add", bookmark: { t: 2, text: "session" }, rev: "" }];
  const out = await loadUIState(
    { ...emptyUISyncState(), ops: session },
    { fetchImpl, storage },
  );
  assert.deepEqual(
    out.doc.bookmarks.map((b) => b.t),
    [1, 2],
  );
  assert.equal(out.ops[0].bookmark.text, "persisted");
  assert.equal(out.ops[1].bookmark.text, "session");
  assert.equal(out.shouldFlush, true);
});

test("loadUI: offline falls back to cache; parse failure leaves document unchanged", async () => {
  const { fetchImpl } = scriptedFetch([new Error("offline")]);
  const prior = baseDoc({ pinned: ["keep-me"] });
  // Missing cache → defaults
  const storage1 = memStorage();
  const out1 = await loadUIState({ ...emptyUISyncState(), doc: prior }, { fetchImpl, storage: storage1 });
  // empty cache string path: loadCachedUI uses "{}" → defaults, which replaces prior
  // when getItem returns null. Characterize: successful cache read uses normUI.
  assert.equal(out1.loaded, true);
  assert.equal(out1.rev, "");

  // Valid cache
  const storage2 = memStorage();
  saveCachedUI(baseDoc({ pinned: ["from-cache"] }), storage2);
  const { fetchImpl: f2 } = scriptedFetch([new Error("offline")]);
  const out2 = await loadUIState({ ...emptyUISyncState(), doc: prior }, { fetchImpl: f2, storage: storage2 });
  assert.deepEqual(out2.doc.pinned, ["from-cache"]);

  // Malformed cache JSON → leave current document unchanged
  const storage3 = memStorage({ [CACHED_UI_KEY]: "{not-json" });
  const { fetchImpl: f3 } = scriptedFetch([new Error("offline")]);
  const out3 = await loadUIState(
    { ...emptyUISyncState(), doc: prior },
    { fetchImpl: f3, storage: storage3 },
  );
  assert.deepEqual(out3.doc.pinned, ["keep-me"]);
});

test("loadUI: non-OK response uses cache path", async () => {
  const { fetchImpl } = scriptedFetch([jsonResponse({ status: 500, ok: false, body: {} })]);
  const storage = memStorage();
  saveCachedUI(baseDoc({ groups: [{ id: "g1", name: "G" }] }), storage);
  const out = await loadUIState(emptyUISyncState(), { fetchImpl, storage });
  assert.equal(out.doc.groups[0].id, "g1");
  assert.equal(out.rev, "");
});

/* ---------- flushUI: no-revision adopt ---------- */
test("flushUI: writer with no revision first adopts a successful server revision", async () => {
  const server = baseDoc({ archived: ["remote"] });
  const localDoc = baseDoc({ bookmarks: [{ t: 9, text: "note" }] });
  // Op stamped against "" (offline); bookmark is idempotent so it replays
  const ops = [{ k: "bookmark-add", bookmark: { t: 9, text: "note" }, rev: "" }];
  const { fetchImpl, calls } = scriptedFetch([
    // adopt GET
    jsonResponse({ body: server, etag: '"s1"' }),
    // PUT after adopt
    jsonResponse({ status: 200, body: {}, etag: '"s2"' }),
  ]);
  const storage = memStorage();
  const out = await flushUIState(
    { doc: localDoc, rev: "", ops, loaded: true, saving: false },
    { fetchImpl, csrf: "tok", storage },
  );
  assert.equal(calls[0].method, "GET");
  assert.equal(calls[0].path, UI_PATH);
  assert.equal(calls[1].method, "PUT");
  assert.equal(calls[1].opts.headers.get("If-Match"), '"s1"');
  assert.notEqual(calls[1].opts.headers.get("If-Match"), "*");
  assert.equal(calls[1].opts.headers.get(CSRF_HEADER), "tok");
  assert.equal(out.rev, '"s2"');
  assert.deepEqual(out.ops, []);
  assert.equal(out.pending, false);
  // bookmark replayed onto server doc
  assert.equal(out.doc.bookmarks.some((b) => b.t === 9), true);
  assert.deepEqual(out.doc.archived, ["remote"]);
});

test("flushUI: no-revision adoption publishes before the bare cache write", async () => {
  const order = [];
  const storage = {
    getItem() { return null; },
    setItem(key) {
      order.push(key === CACHED_UI_KEY ? "cache" : "pending");
      if (key === CACHED_UI_KEY) throw new Error("quota");
    },
  };
  const { fetchImpl } = scriptedFetch([
    jsonResponse({ body: baseDoc(), etag: '"r1"' }),
  ]);
  await assert.rejects(
    () => flushUIState(
      {
        doc: baseDoc(), rev: "",
        ops: [{ k: "bookmark-add", bookmark: { t: 1, text: "x" }, rev: "" }],
        loaded: true, saving: false,
      },
      {
        fetchImpl, storage,
        onRemoteApplied() { order.push("publish"); },
      },
    ),
    /quota/,
  );
  assert.deepEqual(order, ["publish", "pending", "cache"]);
});

test("flushUI: never emits If-Match: * on the wire", async () => {
  const { fetchImpl, calls } = scriptedFetch([
    jsonResponse({ body: baseDoc(), etag: '"z"' }),
    jsonResponse({ status: 200, body: {}, etag: '"z2"' }),
  ]);
  await flushUIState(
    {
      doc: baseDoc(),
      rev: "",
      ops: [{ k: "bookmark-add", bookmark: { t: 1, text: "x" }, rev: "" }],
      loaded: true,
      saving: false,
    },
    { fetchImpl, csrf: "c" },
  );
  for (const c of calls) {
    if (c.method === "PUT") {
      const m = c.opts.headers.get("If-Match");
      assert.notEqual(m, "*");
      assert.equal(m, '"z"');
    }
  }
});

test("flushUI: stale non-idempotent ops drop; bookmark add/delete replay across revisions", async () => {
  const server = baseDoc({
    groups: [{ id: "new", name: "Server" }],
    bookmarks: [{ t: 1, text: "keep" }],
  });
  const ops = [
    { k: "groups", groups: [{ id: "stale", name: "Stale" }], rev: '"old"' },
    { k: "bookmark-add", bookmark: { t: 2, text: "fresh" }, rev: '"old"' },
    { k: "bookmark-del", t: 1, rev: '"old"' },
    { k: "arch", id: "n1", rev: '"old"' },
  ];
  const { fetchImpl, calls } = scriptedFetch([
    jsonResponse({ body: server, etag: '"new"' }),
    jsonResponse({ status: 200, body: {}, etag: '"new2"' }),
  ]);
  const out = await flushUIState(
    { doc: baseDoc(), rev: "", ops, loaded: true, saving: false },
    { fetchImpl, csrf: "c" },
  );
  // After adopt at "new", only bookmark ops remain (idempotent); groups/arch dropped
  assert.equal(calls[1].method, "PUT");
  // Document after replay: server groups preserved, bookmark 1 deleted, 2 added
  assert.deepEqual(out.doc.groups, [{ id: "new", name: "Server" }]);
  assert.equal(out.doc.bookmarks.some((b) => b.t === 1), false);
  assert.equal(out.doc.bookmarks.some((b) => b.t === 2), true);
  assert.equal(out.doc.archived.includes("n1"), false);
  assert.deepEqual(out.ops, []);
});

test("flushUI: successful PUT acknowledges and clears pending work", async () => {
  const doc = baseDoc({ pinned: ["p1"] });
  const ops = [{ k: "pin", id: "p1", rev: '"r1"' }];
  const storage = memStorage();
  savePendingOps(ops, storage);
  const { fetchImpl, calls } = scriptedFetch([
    jsonResponse({ status: 200, body: {}, etag: '"r2"' }),
  ]);
  const out = await flushUIState(
    { doc, rev: '"r1"', ops, loaded: true, saving: false },
    { fetchImpl, csrf: "c", storage },
  );
  assert.equal(calls.length, 1);
  assert.equal(calls[0].opts.headers.get("If-Match"), '"r1"');
  assert.deepEqual(out.ops, []);
  assert.equal(out.rev, '"r2"');
  assert.deepEqual(loadPendingOps(storage), []);
  assert.deepEqual(JSON.parse(storage.getItem(CACHED_UI_KEY)).pinned, ["p1"]);
});

/* ---------- flushUI: conflict + offline + 5xx ---------- */
test("flushUI: 409 refetches, filters/replays, then retries", async () => {
  const afterConflict = baseDoc({
    groups: [{ id: "g2", name: "Other device" }],
    bookmarks: [],
  });
  const ops = [
    { k: "groups", groups: [{ id: "mine", name: "Mine" }], rev: '"r1"' },
    { k: "bookmark-add", bookmark: { t: 5, text: "note" }, rev: '"r1"' },
  ];
  const { fetchImpl, calls } = scriptedFetch([
    jsonResponse({ status: 409, ok: false, body: "conflict" }),
    jsonResponse({ body: afterConflict, etag: '"r2"' }),
    jsonResponse({ status: 200, body: {}, etag: '"r3"' }),
  ]);
  const out = await flushUIState(
    { doc: baseDoc({ groups: [{ id: "mine", name: "Mine" }] }), rev: '"r1"', ops, loaded: true, saving: false },
    { fetchImpl, csrf: "c" },
  );
  assert.equal(calls[0].method, "PUT");
  assert.equal(calls[1].method, "GET");
  assert.equal(calls[2].method, "PUT");
  assert.equal(calls[2].opts.headers.get("If-Match"), '"r2"');
  // groups op dropped (stale); bookmark replayed
  assert.deepEqual(out.doc.groups, [{ id: "g2", name: "Other device" }]);
  assert.equal(out.doc.bookmarks[0].t, 5);
  assert.deepEqual(out.ops, []);
  assert.equal(out.rev, '"r3"');
});

test("flushUI: conflict replay observes an op appended while PUT is in flight", async () => {
  const doc = baseDoc({ bookmarks: [{ t: 1, text: "first" }] });
  const ops = [
    { k: "bookmark-add", bookmark: { t: 1, text: "first" }, rev: '"r1"' },
  ];
  const late = { k: "bookmark-add", bookmark: { t: 2, text: "late" } };
  const { fetchImpl } = scriptedFetch([
    () => {
      // Models uiMutate running after fetch() starts but before its response.
      queueLocalOp(doc, late, '"r1"', ops);
      return jsonResponse({ status: 409, ok: false, body: "conflict" });
    },
    jsonResponse({ body: baseDoc(), etag: '"r2"' }),
    jsonResponse({ status: 200, body: {}, etag: '"r3"' }),
  ]);
  const out = await flushUIState(
    { doc, rev: '"r1"', ops, loaded: true, saving: false },
    { fetchImpl, csrf: "c" },
  );
  assert.deepEqual(out.doc.bookmarks.map((b) => b.t), [1, 2]);
  assert.deepEqual(out.ops, []);
});

test("flushUI: conflict persists the filtered queue before publishing and retrying", async () => {
  const order = [];
  const storage = {
    getItem() { return null; },
    setItem(key) {
      if (key !== CACHED_UI_KEY) order.push("pending");
    },
  };
  const { fetchImpl } = scriptedFetch([
    jsonResponse({ status: 409, ok: false, body: "conflict" }),
    jsonResponse({ body: baseDoc(), etag: '"r2"' }),
    () => {
      order.push("retry");
      return jsonResponse({ status: 200, body: {}, etag: '"r3"' });
    },
  ]);
  await flushUIState(
    {
      doc: baseDoc(), rev: '"r1"',
      ops: [{ k: "bookmark-add", bookmark: { t: 1, text: "x" }, rev: '"r1"' }],
      loaded: true, saving: false,
    },
    {
      fetchImpl, storage,
      onRemoteApplied() { order.push("publish"); },
    },
  );
  assert.deepEqual(order.slice(0, 3), ["pending", "publish", "retry"]);
});

test("flushUI: 428 is treated as conflict like 409", async () => {
  const { fetchImpl, calls } = scriptedFetch([
    jsonResponse({ status: 428, ok: false, body: "precondition" }),
    jsonResponse({ body: baseDoc(), etag: '"r9"' }),
    jsonResponse({ status: 200, body: {}, etag: '"r10"' }),
  ]);
  const out = await flushUIState(
    {
      doc: baseDoc(),
      rev: '"r0"',
      ops: [{ k: "bookmark-add", bookmark: { t: 1, text: "x" }, rev: '"r0"' }],
      loaded: true,
      saving: false,
    },
    { fetchImpl, csrf: "c" },
  );
  assert.equal(calls[0].method, "PUT");
  assert.equal(calls[1].method, "GET");
  assert.equal(calls[2].method, "PUT");
  assert.deepEqual(out.ops, []);
});

test("flushUI: offline and 5xx paths preserve pending work", async () => {
  const ops = [{ k: "pin", id: "n", rev: '"r1"' }];
  // offline PUT
  {
    const { fetchImpl } = scriptedFetch([new Error("network down")]);
    const out = await flushUIState(
      { doc: baseDoc({ pinned: ["n"] }), rev: '"r1"', ops: ops.slice(), loaded: true, saving: false },
      { fetchImpl, csrf: "c" },
    );
    assert.equal(out.ops.length, 1);
    assert.equal(out.pending, true);
    assert.equal(out.rev, '"r1"');
  }
  // 5xx
  {
    const { fetchImpl } = scriptedFetch([jsonResponse({ status: 503, ok: false, body: "busy" })]);
    const out = await flushUIState(
      { doc: baseDoc({ pinned: ["n"] }), rev: '"r1"', ops: ops.slice(), loaded: true, saving: false },
      { fetchImpl, csrf: "c" },
    );
    assert.equal(out.ops.length, 1);
    assert.equal(out.pending, true);
  }
  // no-rev adopt offline keeps ops
  {
    const { fetchImpl } = scriptedFetch([new Error("offline")]);
    const out = await flushUIState(
      {
        doc: baseDoc(),
        rev: "",
        ops: [{ k: "bookmark-add", bookmark: { t: 1, text: "x" }, rev: "" }],
        loaded: true,
        saving: false,
      },
      { fetchImpl, csrf: "c" },
    );
    assert.equal(out.ops.length, 1);
    assert.equal(out.rev, "");
  }
});

test("flushUI: maximum-four-attempt policy stops after four PUTs", async () => {
  const handlers = [];
  for (let i = 0; i < 4; i++) {
    handlers.push(jsonResponse({ status: 409, ok: false, body: "c" }));
    handlers.push(jsonResponse({ body: baseDoc(), etag: `"r${i}"` }));
  }
  // If a 5th PUT were attempted it would throw via scriptedFetch
  const { fetchImpl, calls } = scriptedFetch(handlers);
  const ops = [{ k: "bookmark-add", bookmark: { t: 1, text: "x" }, rev: '"r0"' }];
  // After each 409, filter keeps the bookmark (idempotent) so ops stay non-empty
  await flushUIState(
    { doc: baseDoc(), rev: '"r0"', ops, loaded: true, saving: false },
    { fetchImpl, csrf: "c" },
  );
  const puts = calls.filter((c) => c.method === "PUT");
  assert.equal(puts.length, MAX_FLUSH_ATTEMPTS);
  assert.equal(puts.length, 4);
});

test("flushUI: skips when not loaded, already saving, or no ops", async () => {
  const { fetchImpl, calls } = scriptedFetch([]);
  const base = { doc: baseDoc(), rev: '"r"', ops: [{ k: "pin", id: "n", rev: '"r"' }], loaded: true, saving: false };
  assert.equal((await flushUIState({ ...base, loaded: false }, { fetchImpl })).skipped, true);
  assert.equal((await flushUIState({ ...base, saving: true }, { fetchImpl })).skipped, true);
  assert.equal((await flushUIState({ ...base, ops: [] }, { fetchImpl })).skipped, true);
  assert.equal(calls.length, 0);
});

/* ---------- pollUI ---------- */
test("pollUI: retains state on 304 or failure", async () => {
  const doc = baseDoc({ pinned: ["x"] });
  const state = { doc, rev: '"r1"', ops: [], loaded: true, saving: false };

  {
    const { fetchImpl, calls } = scriptedFetch([jsonResponse({ status: 304, ok: false, body: "" })]);
    // 304 responses are not ok in the Fetch sense for some stacks; status check is what matters
    const out = await pollUIState(state, { fetchImpl });
    assert.equal(calls[0].opts.headers["If-None-Match"], '"r1"');
    assert.deepEqual(out.doc.pinned, ["x"]);
    assert.equal(out.rev, '"r1"');
    assert.equal(out.retained, true);
    assert.equal(out.applied, false);
  }
  {
    const { fetchImpl } = scriptedFetch([new Error("offline")]);
    const out = await pollUIState(state, { fetchImpl });
    assert.deepEqual(out.doc.pinned, ["x"]);
    assert.equal(out.retained, true);
  }
  {
    const { fetchImpl } = scriptedFetch([jsonResponse({ status: 500, ok: false, body: {} })]);
    const out = await pollUIState(state, { fetchImpl });
    assert.equal(out.retained, true);
  }
});

test("pollUI: applies remote when document changes; skips while ops in flight", async () => {
  const storage = memStorage();
  const { fetchImpl } = scriptedFetch([
    jsonResponse({ body: baseDoc({ archived: ["a"] }), etag: '"r2"' }),
  ]);
  const out = await pollUIState(
    { doc: baseDoc(), rev: '"r1"', ops: [], loaded: true, saving: false },
    { fetchImpl, storage },
  );
  assert.deepEqual(out.doc.archived, ["a"]);
  assert.equal(out.rev, '"r2"');
  assert.equal(out.applied, true);

  const { fetchImpl: f2, calls } = scriptedFetch([]);
  const skipped = await pollUIState(
    { doc: baseDoc(), rev: '"r1"', ops: [{ k: "pin", id: "n" }], loaded: true, saving: false },
    { fetchImpl: f2 },
  );
  assert.equal(skipped.skipped, true);
  assert.equal(calls.length, 0);
});

test("pollUI: equal remote updates rev only and does not rewrite cache", async () => {
  const doc = baseDoc({ pinned: ["p"] });
  const storage = memStorage();
  saveCachedUI(baseDoc({ pinned: ["old-cache"] }), storage);
  const { fetchImpl } = scriptedFetch([
    jsonResponse({ body: baseDoc({ pinned: ["p"] }), etag: '"r-new"' }),
  ]);
  const out = await pollUIState(
    { doc, rev: '"r-old"', ops: [], loaded: true, saving: false },
    { fetchImpl, storage },
  );
  assert.equal(out.rev, '"r-new"');
  assert.equal(out.unchanged, true);
  assert.equal(out.applied, false);
  assert.deepEqual(JSON.parse(storage.getItem(CACHED_UI_KEY)).pinned, ["old-cache"]);
});

test("pollUI: conditional GET omits If-None-Match when no revision", async () => {
  const { fetchImpl, calls } = scriptedFetch([
    jsonResponse({ body: baseDoc(), etag: '"first"' }),
  ]);
  await pollUIState(
    { doc: baseDoc(), rev: "", ops: [], loaded: true, saving: false },
    { fetchImpl },
  );
  assert.deepEqual(calls[0].opts.headers, {});
});

/* ---------- pagehide + composition ---------- */
test("pagehide: gate and keepalive PUT body/headers", () => {
  const doc = baseDoc({ pinned: ["p"] });
  assert.equal(pagehideShouldSend({ loaded: true, ops: [{ k: "pin" }], rev: '"r"' }), true);
  const req = pagehidePutRequest(doc, {
    loaded: true,
    ops: [{ k: "pin", id: "p", rev: '"r"' }],
    rev: '"r"',
    csrf: "tok",
  });
  assert.equal(req.opts.keepalive, true);
  assert.equal(req.opts.headers.get("If-Match"), '"r"');
  assert.equal(req.opts.body, JSON.stringify(doc));

  assert.equal(
    pagehideShouldSend({ loaded: true, ops: [{ k: "pin" }], rev: "" }),
    false,
  );
});

test("uiPutRequest body is the current document JSON (not the op list)", () => {
  const doc = baseDoc({ lanes: [{ id: "L", name: "Lane" }] });
  const { opts } = uiPutRequest(doc, '"r"', { csrf: "c" });
  assert.equal(opts.body, JSON.stringify(doc));
  assert.equal(JSON.parse(opts.body).lanes[0].id, "L");
});

test("composition: queueLocalOp + flushUIState clears after ack", async () => {
  let doc = baseDoc();
  const queue = [];
  const { doc: d2, queue: q2 } = queueLocalOp(doc, { k: "pin", id: "n1" }, '"r1"', queue);
  doc = d2;
  assert.equal(q2[0].rev, '"r1"');
  const { fetchImpl } = scriptedFetch([jsonResponse({ status: 200, body: {}, etag: '"r2"' })]);
  const out = await flushUIState(
    { doc, rev: '"r1"', ops: q2, loaded: true, saving: false },
    { fetchImpl, csrf: "c" },
  );
  assert.deepEqual(out.ops, []);
  assert.equal(out.rev, '"r2"');
  assert.deepEqual(out.doc.pinned, ["n1"]);
});
