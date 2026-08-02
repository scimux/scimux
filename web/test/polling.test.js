/* Characterization tests for web/js/polling.js — state tick orchestration,
 * generation/cancellation, UI-sync coordination, visibility/pagehide.
 * Controlled promises, fake timers/event targets/storage, injected effects.
 * Does not re-test reducer/conflict matrix (see ui-sync.test.js).
 * No browser emulator; no mutable production test/debug accessors. */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import {
  createPollingFeature,
  stateGetRequest,
  syncWarningText,
} from "../js/polling.js";
import { UI_PATH, CSRF_HEADER } from "../js/api.js";
import { CACHED_UI_KEY, PENDING_OPS_KEY, normUI } from "../js/state.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const pollingSrc = readFileSync(join(__dirname, "../js/polling.js"), "utf8");

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

function jsonResponse({ status = 200, body = {}, etag = "", ok, headers: extra } = {}) {
  const isOk = ok !== undefined ? ok : status >= 200 && status < 300;
  const headers = new Headers(extra || {});
  if (etag) headers.set("ETag", etag);
  if (!headers.has("Content-Type")) headers.set("Content-Type", "application/json");
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

/** Deferred promise for controlling async order. */
function deferred() {
  let resolve, reject;
  const promise = new Promise((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

/** Drain microtasks so async tick/pollLoop chains settle. */
async function flush(n = 8) {
  for (let i = 0; i < n; i++) await Promise.resolve();
}

/** Fake timers: manual flush by id / runDue. */
function fakeTimers() {
  let nextId = 1;
  const pending = new Map();
  let now = 0;
  return {
    setTimeout(fn, ms) {
      const id = nextId++;
      pending.set(id, { fn, due: now + (ms || 0) });
      return id;
    },
    clearTimeout(id) {
      pending.delete(id);
    },
    advance(ms) {
      now += ms;
      const due = [...pending.entries()]
        .filter(([, t]) => t.due <= now)
        .sort((a, b) => a[1].due - b[1].due);
      for (const [id, t] of due) {
        pending.delete(id);
        t.fn();
      }
    },
    pendingCount() {
      return pending.size;
    },
    get now() {
      return now;
    },
  };
}

/** Minimal EventTarget for visibility/pagehide. */
function fakeTarget(props = {}) {
  const listeners = new Map();
  return {
    ...props,
    addEventListener(type, fn) {
      if (!listeners.has(type)) listeners.set(type, new Set());
      listeners.get(type).add(fn);
    },
    removeEventListener(type, fn) {
      listeners.get(type)?.delete(fn);
    },
    dispatch(type) {
      for (const fn of listeners.get(type) || []) fn();
    },
    listenerCount(type) {
      return listeners.get(type)?.size || 0;
    },
  };
}

function effectLog() {
  const log = [];
  const push = (name) => (...args) => {
    log.push({ name, args });
  };
  return {
    log,
    names: () => log.map((e) => e.name),
    push,
    clear() {
      log.length = 0;
    },
  };
}

function baseDoc(extra = {}) {
  return normUI({ groups: [], archived: [], bookmarks: [], lanes: [], pinned: [], ...extra });
}

function makeFeature(overrides = {}) {
  const timers = overrides.timers || fakeTimers();
  const storage = overrides.storage || memStorage();
  const document = overrides.document || fakeTarget({ hidden: false });
  const window = overrides.window || fakeTarget();
  const effects = overrides.effects || effectLog();
  const syncwarnEl = overrides.syncwarnEl || { textContent: "" };
  const fetchBundle = overrides.fetchBundle || scriptedFetch([]);
  const f = createPollingFeature({
    document,
    window,
    storage,
    fetchImpl: fetchBundle.fetchImpl,
    csrf: overrides.csrf ?? "tok",
    setTimeout: timers.setTimeout.bind(timers),
    clearTimeout: timers.clearTimeout.bind(timers),
    syncwarnEl,
    setHostOnline: overrides.setHostOnline || effects.push("setHostOnline"),
    setServerUnreachable: overrides.setServerUnreachable || effects.push("setServerUnreachable"),
    setHostname: overrides.setHostname || effects.push("setHostname"),
    setVersion: overrides.setVersion || effects.push("setVersion"),
    publishState: overrides.publishState || effects.push("publishState"),
    renderSys: overrides.renderSys || effects.push("renderSys"),
    ensureSelection: overrides.ensureSelection || effects.push("ensureSelection"),
    updateCardAges: overrides.updateCardAges || effects.push("updateCardAges"),
    renderCards: overrides.renderCards || effects.push("renderCards"),
    renderMap: overrides.renderMap || effects.push("renderMap"),
    renderMapTabs: overrides.renderMapTabs || effects.push("renderMapTabs"),
    renderBookmarksPane: overrides.renderBookmarksPane || effects.push("renderBookmarksPane"),
    renderChatHead: overrides.renderChatHead || effects.push("renderChatHead"),
    refreshChat: overrides.refreshChat || (async () => { effects.push("refreshChat")(); }),
    renderWsInbox: overrides.renderWsInbox || effects.push("renderWsInbox"),
    invalidateCardsSig: overrides.invalidateCardsSig || effects.push("invalidateCardsSig"),
    invalidateMap: overrides.invalidateMap || effects.push("invalidateMap"),
    invalidateBookmarks: overrides.invalidateBookmarks || effects.push("invalidateBookmarks"),
    invalidateChat: overrides.invalidateChat || effects.push("invalidateChat"),
    onStartPolling: overrides.onStartPolling || effects.push("onStartPolling"),
    ...overrides.extraDeps,
  });
  return { f, timers, storage, document, window, effects, syncwarnEl, fetchBundle };
}

/* ---------- module shape ---------- */
test("polling.js exports factory and pure helpers; imports only api/state", () => {
  assert.match(pollingSrc, /export function createPollingFeature/);
  assert.match(pollingSrc, /Packet 7I ownership inventory/);
  assert.match(pollingSrc, /from "\.\/api\.js"/);
  assert.match(pollingSrc, /from "\.\/state\.js"/);
  assert.doesNotMatch(pollingSrc, /from "\.\/app\.js"/);
  assert.doesNotMatch(pollingSrc, /from "\.\/cards\.js"/);
  assert.doesNotMatch(pollingSrc, /from "\.\/chat\.js"/);
  assert.doesNotMatch(pollingSrc, /from "\.\/composer\.js"/);
  assert.doesNotMatch(pollingSrc, /from "\.\/notes\.js"/);
  assert.doesNotMatch(pollingSrc, /from "\.\/sheets\.js"/);
  assert.doesNotMatch(pollingSrc, /getStateEtag|_forTest|test\/debug/);
});

test("pure: stateGetRequest and syncWarningText", () => {
  assert.deepEqual(stateGetRequest(""), {
    path: "/api/state",
    opts: { headers: {} },
  });
  assert.deepEqual(stateGetRequest('"e1"'), {
    path: "/api/state",
    opts: { headers: { "If-None-Match": '"e1"' } },
  });
  assert.equal(syncWarningText({}), "");
  assert.equal(syncWarningText({ pending: true }), "syncing…");
  assert.equal(syncWarningText({ failed: true }), "changes not saved yet");
  assert.equal(syncWarningText({ pending: true, failed: true }), "changes not saved yet");
});

/* ---------- /api/state tick branches ---------- */
test("tick: initial request has no If-None-Match; conditional after 200", async () => {
  const body = { nodes: [{ id: "n1", title: "A" }], unadopted: [], hostname: "box", version: "1.0", sys: { mem_pct: 1 } };
  const { fetchImpl, calls } = scriptedFetch([
    jsonResponse({ body, etag: '"s1"' }),
    jsonResponse({ body, etag: '"s2"' }),
  ]);
  const { f } = makeFeature({ fetchBundle: { fetchImpl, calls } });
  await f.tick();
  assert.equal(calls[0].path, "/api/state");
  assert.deepEqual(calls[0].opts.headers, {});
  await f.tick();
  assert.equal(calls[1].opts.headers["If-None-Match"], '"s1"');
});

test("tick: network failure marks offline and server unreachable", async () => {
  const { fetchImpl, calls } = scriptedFetch([new Error("net")]);
  const effects = effectLog();
  const { f } = makeFeature({ fetchBundle: { fetchImpl, calls }, effects });
  await f.tick();
  assert.deepEqual(effects.names(), ["setHostOnline", "setServerUnreachable"]);
  assert.deepEqual(effects.log[0].args, [false]);
});

test("tick: non-OK marks offline without structural rebuild", async () => {
  const { fetchImpl, calls } = scriptedFetch([
    jsonResponse({ status: 500, ok: false, body: "err" }),
  ]);
  const effects = effectLog();
  const { f } = makeFeature({ fetchBundle: { fetchImpl, calls }, effects });
  await f.tick();
  assert.deepEqual(effects.names(), ["setHostOnline"]);
  assert.deepEqual(effects.log[0].args, [false]);
});

test("tick: 304 online → ages → await chat → fire-and-forget UI poll", async () => {
  const order = [];
  let pollUIStarted = false;
  let holdChat = false;
  let chatDone;
  const chatP = new Promise((r) => { chatDone = r; });
  const uiPollHold = deferred();
  let stateN = 0;
  let uiN = 0;
  const fetchImpl = async (path) => {
    if (path === UI_PATH) {
      uiN++;
      /* after load (1) + post-200 poll (2), the 304 path's poll is 3+ */
      if (holdChat && uiN >= 3) {
        pollUIStarted = true;
        order.push("pollUI-fetch");
        await uiPollHold.promise;
        return jsonResponse({ status: 304, body: {} });
      }
      return jsonResponse({ body: baseDoc(), etag: '"u1"' });
    }
    stateN++;
    if (stateN === 1) return jsonResponse({ body: { nodes: [], unadopted: [] }, etag: '"s1"' });
    return { ok: true, status: 304, headers: new Headers(), json: async () => ({}), text: async () => "" };
  };
  const effects = effectLog();
  const { f } = makeFeature({
    fetchBundle: { fetchImpl, calls: [] },
    effects,
    refreshChat: async () => {
      order.push("refreshChat-start");
      if (holdChat) await chatP;
      order.push("refreshChat-end");
      effects.push("refreshChat")();
    },
  });
  await f.loadUI();
  await f.tick(); /* 200 prime etag — chat not held */
  await Promise.resolve();
  await Promise.resolve();
  effects.clear();
  order.length = 0;
  pollUIStarted = false;
  holdChat = true;

  const tickP = f.tick();
  await Promise.resolve();
  assert.equal(pollUIStarted, false);
  assert.ok(order.includes("refreshChat-start"));
  chatDone();
  await tickP;
  assert.equal(pollUIStarted, true);
  assert.deepEqual(
    effects.names().filter((n) => n !== "refreshChat"),
    ["setHostOnline", "updateCardAges"],
  );
  assert.ok(!effects.names().includes("renderCards"), "304 must not structurally rebuild cards");
  assert.ok(!effects.names().includes("publishState"));
  uiPollHold.resolve();
  await Promise.resolve();
  await Promise.resolve();
});

test("tick: 200 publishes state then Cards → Map → Bookmarks → await Chat → UI poll", async () => {
  const order = [];
  const chatHold = deferred();
  const uiHold = deferred();
  let uiPhase = 0;
  const fetchImpl = async (path) => {
    if (path === UI_PATH) {
      uiPhase++;
      if (uiPhase === 1) return jsonResponse({ body: baseDoc(), etag: '"u0"' });
      order.push("ui-poll");
      await uiHold.promise;
      return jsonResponse({ status: 304, body: {} });
    }
    return jsonResponse({
      body: {
        nodes: [{ id: "a" }],
        unadopted: [{ id: "u" }],
        hostname: "host1",
        version: "v9",
        sys: { mem_pct: 10 },
      },
      etag: '"e200"',
    });
  };
  const { f } = makeFeature({
    fetchBundle: { fetchImpl, calls: [] },
    setHostOnline: () => order.push("online"),
    setHostname: (h) => order.push("host:" + h),
    setVersion: (v) => order.push("ver:" + v),
    publishState: (st) => order.push("publish:" + st.nodes[0].id + "/" + st.unadopted[0].id),
    renderSys: () => order.push("sys"),
    ensureSelection: () => order.push("sel"),
    renderCards: () => order.push("cards"),
    renderMap: () => order.push("map"),
    renderBookmarksPane: () => order.push("bm"),
    refreshChat: async () => {
      order.push("chat-start");
      await chatHold.promise;
      order.push("chat-end");
    },
  });
  await f.loadUI();
  order.length = 0;
  const tickP = f.tick();
  await Promise.resolve();
  await Promise.resolve();
  assert.deepEqual(order, [
    "online",
    "publish:a/u",
    "host:host1",
    "ver:v9",
    "sys",
    "sel",
    "cards",
    "map",
    "bm",
    "chat-start",
  ]);
  assert.ok(!order.includes("ui-poll"), "UI poll waits for chat");
  chatHold.resolve();
  await tickP;
  assert.ok(order.includes("chat-end"));
  assert.ok(order.includes("ui-poll"));
  uiHold.resolve();
});

test("tick: ensureSelection invoked on 200 for selected-node fallback", async () => {
  let ensured = 0;
  const { fetchImpl, calls } = scriptedFetch([
    jsonResponse({ body: { nodes: [{ id: "x" }], unadopted: [] }, etag: '"e"' }),
  ]);
  const { f } = makeFeature({
    fetchBundle: { fetchImpl, calls },
    ensureSelection: () => { ensured++; },
  });
  await f.tick();
  assert.equal(ensured, 1);
});

/* ---------- non-overlap ---------- */
test("tick: concurrent entry is suppressed; ticking released on every return", async () => {
  const hold = deferred();
  let entries = 0;
  const fetchImpl = async () => {
    entries++;
    if (entries === 1) {
      await hold.promise;
      return jsonResponse({ body: { nodes: [], unadopted: [] }, etag: '"e"' });
    }
    return jsonResponse({ status: 500, ok: false, body: "" });
  };
  const { f } = makeFeature({ fetchBundle: { fetchImpl, calls: [] } });
  const a = f.tick();
  const b = f.tick(); /* concurrent — no-op */
  await flush(2);
  assert.equal(entries, 1);
  hold.resolve();
  await a;
  await b;
  assert.equal(entries, 1);
  await f.tick(); /* released after completion */
  assert.equal(entries, 2);
});

test("tick: UI poll fire-and-forget allows overlapping next state tick", async () => {
  const uiHold = deferred();
  let stateFetches = 0;
  let uiPolls = 0;
  const fetchImpl = async (path) => {
    if (path === "/api/state") {
      stateFetches++;
      return jsonResponse({ body: { nodes: [], unadopted: [] }, etag: `"e${stateFetches}"` });
    }
    uiPolls++;
    if (uiPolls === 1) return jsonResponse({ body: baseDoc(), etag: '"u0"' }); /* load */
    if (uiPolls === 2) {
      await uiHold.promise;
      return jsonResponse({ status: 304, body: {} });
    }
    return jsonResponse({ status: 304, body: {} });
  };
  const { f } = makeFeature({ fetchBundle: { fetchImpl, calls: [] } });
  await f.loadUI();
  await f.tick();
  assert.equal(stateFetches, 1);
  assert.equal(uiPolls, 2);
  await f.tick(); /* second state tick while first UI poll still open */
  assert.equal(stateFetches, 2);
  uiHold.resolve();
  await Promise.resolve();
});

/* ---------- generation / visibility / destroy ---------- */
test("startPolling: generation restart; stale loop cannot reschedule", async () => {
  const timers = fakeTimers();
  let stateHits = 0;
  const fetchImpl = async (path) => {
    if (path === UI_PATH) return jsonResponse({ status: 304, body: {} });
    stateHits++;
    return jsonResponse({ body: { nodes: [], unadopted: [] }, etag: `"e${stateHits}"` });
  };
  const document = fakeTarget({ hidden: false });
  const { f } = makeFeature({
    timers,
    document,
    fetchBundle: { fetchImpl, calls: [] },
  });
  f.startPolling();
  await flush();
  assert.equal(stateHits, 1);
  assert.equal(timers.pendingCount(), 1);
  f.startPolling();
  await flush();
  assert.equal(stateHits, 2);
  timers.advance(2000);
  await flush();
  assert.equal(stateHits, 3);
  f.destroy();
});

test("visibility: hidden pauses; visible resumes via startPolling", async () => {
  const timers = fakeTimers();
  let primes = 0;
  let stateHits = 0;
  const fetchImpl = async (path) => {
    if (path === UI_PATH) return jsonResponse({ status: 304, body: {} });
    stateHits++;
    return jsonResponse({ body: { nodes: [], unadopted: [] }, etag: `"v${stateHits}"` });
  };
  const document = fakeTarget({ hidden: false });
  const { f } = makeFeature({
    timers,
    document,
    fetchBundle: { fetchImpl, calls: [] },
    onStartPolling: () => { primes++; },
  });
  f.bind();
  f.startPolling();
  await flush();
  assert.equal(primes, 1);
  assert.equal(stateHits, 1);
  assert.equal(timers.pendingCount(), 1);

  document.hidden = true;
  document.dispatch("visibilitychange");
  assert.equal(timers.pendingCount(), 0);

  document.hidden = false;
  document.dispatch("visibilitychange");
  await flush();
  assert.equal(primes, 2);
  assert.equal(stateHits, 2);
  f.destroy();
});

test("destroy: cleans listeners/timers; late fetch and timer ignored", async () => {
  const hold = deferred();
  const timers = fakeTimers();
  const document = fakeTarget({ hidden: false });
  const window = fakeTarget();
  const effects = effectLog();
  const { fetchImpl, calls } = scriptedFetch([
    async () => {
      await hold.promise;
      return jsonResponse({
        body: { nodes: [{ id: "late" }], unadopted: [], hostname: "x" },
        etag: '"late"',
      });
    },
  ]);
  const { f } = makeFeature({
    timers,
    document,
    window,
    effects,
    fetchBundle: { fetchImpl, calls },
  });
  f.bind();
  assert.equal(document.listenerCount("visibilitychange"), 1);
  assert.equal(window.listenerCount("pagehide"), 1);
  const p = f.tick();
  f.destroy();
  assert.equal(document.listenerCount("visibilitychange"), 0);
  assert.equal(window.listenerCount("pagehide"), 0);
  assert.equal(timers.pendingCount(), 0);
  hold.resolve();
  await p;
  assert.ok(!effects.names().includes("publishState"), "late tick must not publish after destroy");
  assert.ok(!effects.names().includes("renderCards"));
  /* destroy is idempotent */
  f.destroy();
  f.startPolling();
  f.uiMutate({ k: "pin", id: "n" });
  assert.equal(effects.names().includes("renderBookmarksPane"), false);
});

test("bind is idempotent; single visibility and pagehide listener", () => {
  const document = fakeTarget({ hidden: false });
  const window = fakeTarget();
  const { f } = makeFeature({ document, window, fetchBundle: scriptedFetch([]) });
  f.bind();
  f.bind();
  assert.equal(document.listenerCount("visibilitychange"), 1);
  assert.equal(window.listenerCount("pagehide"), 1);
});

test("invalidateStateEtag: next tick sends unconditional GET", async () => {
  const { fetchImpl, calls } = scriptedFetch([
    jsonResponse({ body: { nodes: [], unadopted: [] }, etag: '"s1"' }),
    jsonResponse({ body: { nodes: [], unadopted: [] }, etag: '"s2"' }),
  ]);
  const { f } = makeFeature({ fetchBundle: { fetchImpl, calls } });
  await f.tick();
  f.invalidateStateEtag();
  await f.tick();
  assert.deepEqual(calls[1].opts.headers, {});
});

/* ---------- UI mutation / flush coalesce / load / remote ---------- */
test("uiMutate: apply → persist → syncwarn → bookmarks → inbox → 300ms flush", async () => {
  const timers = fakeTimers();
  const storage = memStorage();
  const order = [];
  const syncwarnEl = { textContent: "" };
  const { fetchImpl, calls } = scriptedFetch([
    jsonResponse({ body: baseDoc(), etag: '"r1"' }),
    () => {
      order.push("put");
      return jsonResponse({ status: 200, body: {}, etag: '"r2"' });
    },
  ]);
  const { f } = makeFeature({
    timers,
    storage,
    syncwarnEl,
    fetchBundle: { fetchImpl, calls },
    renderBookmarksPane: () => order.push("bm"),
    renderWsInbox: () => order.push("inbox"),
  });
  await f.loadUI();
  order.length = 0;
  f.uiMutate({ k: "pin", id: "n1" });
  assert.deepEqual(f.getUI().pinned, ["n1"]);
  assert.equal(syncwarnEl.textContent, "syncing…");
  assert.deepEqual(order, ["bm", "inbox"]);
  assert.ok(storage.getItem(PENDING_OPS_KEY));
  assert.ok(storage.getItem(CACHED_UI_KEY));
  f.uiMutate({ k: "pin", id: "n2" });
  assert.equal(timers.pendingCount(), 1);
  timers.advance(299);
  assert.ok(!order.includes("put"));
  timers.advance(1);
  await flush();
  assert.ok(order.includes("put"));
  assert.deepEqual(f.getUI().pinned, ["n2", "n1"]);
  assert.equal(syncwarnEl.textContent, "");
  f.destroy();
});

test("flush coalesce is exactly 300ms from last mutate", async () => {
  const timers = fakeTimers();
  const storage = memStorage();
  let puts = 0;
  const { fetchImpl, calls } = scriptedFetch([
    jsonResponse({ body: baseDoc(), etag: '"r1"' }),
    () => {
      puts++;
      return jsonResponse({ status: 200, body: {}, etag: '"r2"' });
    },
  ]);
  const { f } = makeFeature({ timers, storage, fetchBundle: { fetchImpl, calls } });
  await f.loadUI();
  f.uiMutate({ k: "pin", id: "a" });
  timers.advance(200);
  f.uiMutate({ k: "pin", id: "b" });
  timers.advance(299);
  await Promise.resolve();
  assert.equal(puts, 0);
  timers.advance(1);
  await Promise.resolve();
  await Promise.resolve();
  assert.equal(puts, 1);
});

test("loadUI: applies remote, invalidates chat, applyRemoteUI order, flushes pending", async () => {
  const order = [];
  const storage = memStorage();
  storage.setItem(
    PENDING_OPS_KEY,
    JSON.stringify([{ k: "pin", id: "p1", rev: '"r1"' }]),
  );
  const { fetchImpl, calls } = scriptedFetch([
    jsonResponse({ body: baseDoc({ archived: ["a1"] }), etag: '"r1"' }),
    jsonResponse({ status: 200, body: {}, etag: '"r2"' }),
  ]);
  const { f } = makeFeature({
    storage,
    fetchBundle: { fetchImpl, calls },
    invalidateChat: () => order.push("invChat"),
    invalidateCardsSig: () => order.push("invCards"),
    invalidateMap: () => order.push("invMap"),
    invalidateBookmarks: () => order.push("invBm"),
    renderCards: () => order.push("cards"),
    renderChatHead: () => order.push("chatHead"),
    renderMapTabs: () => order.push("mapTabs"),
    renderMap: () => order.push("map"),
    renderBookmarksPane: () => order.push("bm"),
  });
  await f.loadUI();
  assert.equal(f.isUILoaded(), true);
  assert.deepEqual(f.getUI().archived, ["a1"]);
  assert.deepEqual(f.getUI().pinned, ["p1"]);
  assert.deepEqual(order.slice(0, 8), [
    "invChat",
    "invCards",
    "invMap",
    "invBm",
    "cards",
    "chatHead",
    "mapTabs",
    "map",
  ]);
  assert.ok(order.includes("bm"));
  /* pending flush issued */
  await Promise.resolve();
  await Promise.resolve();
  assert.ok(calls.some((c) => c.method === "PUT"));
});

test("pollUI via tick: applies remote with signature invalidation order", async () => {
  const order = [];
  const timers = fakeTimers();
  const remote = baseDoc({ groups: [{ id: "g", name: "G", lanes: [] }] });
  let uiGets = 0;
  const fetchImpl = async (path) => {
    if (path === UI_PATH) {
      uiGets++;
      if (uiGets === 1) return jsonResponse({ body: baseDoc(), etag: '"u1"' });
      return jsonResponse({ body: remote, etag: '"u2"' });
    }
    return jsonResponse({ body: { nodes: [], unadopted: [] }, etag: '"s1"' });
  };
  const { f } = makeFeature({
    timers,
    fetchBundle: { fetchImpl, calls: [] },
    renderCards: () => order.push("cards"),
    renderMap: () => order.push("map"),
    renderMapTabs: () => order.push("mapTabs"),
    renderBookmarksPane: () => order.push("bm"),
    renderChatHead: () => order.push("chatHead"),
    refreshChat: async () => order.push("chat"),
    invalidateCardsSig: () => order.push("invCards"),
    invalidateMap: () => order.push("invMap"),
    invalidateBookmarks: () => order.push("invBm"),
    invalidateChat: () => order.push("invChat"),
  });
  await f.loadUI();
  order.length = 0;
  await f.tick();
  /* drain fire-and-forget pollUI */
  await Promise.resolve();
  await Promise.resolve();
  assert.ok(order.includes("chat"));
  const remoteIdx = order.indexOf("invCards");
  assert.ok(remoteIdx >= 0, "remote apply invalidates cards");
  assert.ok(order.indexOf("invMap") > remoteIdx - 1);
  assert.ok(order.includes("invBm"));
  assert.ok(order.indexOf("chatHead") > remoteIdx);
  assert.deepEqual(f.getUI().groups[0].id, "g");
});

test("pollUI via tick: skips while local ops are in flight", async () => {
  const timers = fakeTimers();
  let uiPolls = 0;
  const fetchImpl = async (path, opts) => {
    if (path === UI_PATH) {
      if (!opts?.method || opts.method === "GET") {
        /* load or poll */
        if (uiPolls === 0 && !opts?.headers?.["If-None-Match"]) {
          /* initial load GET has no If-None-Match; poll may too when rev set */
        }
        uiPolls++;
        return jsonResponse({ body: baseDoc(), etag: '"u1"' });
      }
      return jsonResponse({ status: 200, body: {}, etag: '"u2"' });
    }
    return jsonResponse({ body: { nodes: [], unadopted: [] }, etag: '"s1"' });
  };
  const { f } = makeFeature({
    timers,
    fetchBundle: { fetchImpl, calls: [] },
  });
  await f.loadUI();
  const afterLoad = uiPolls;
  f.uiMutate({ k: "pin", id: "hold" });
  /* do not advance flush timer — ops still pending */
  await f.tick();
  await Promise.resolve();
  await Promise.resolve();
  /* pollUIState skips when ops.length — no extra UI GET beyond load */
  assert.equal(uiPolls, afterLoad);
});

test("uiMutate while not loaded queues; load flushes pending", async () => {
  const timers = fakeTimers();
  const storage = memStorage();
  const { fetchImpl, calls } = scriptedFetch([
    jsonResponse({ body: baseDoc(), etag: '"r1"' }),
    jsonResponse({ status: 200, body: {}, etag: '"r2"' }),
  ]);
  const { f } = makeFeature({ timers, storage, fetchBundle: { fetchImpl, calls } });
  f.uiMutate({ k: "bookmark-add", bookmark: { t: 1, text: "n" } });
  assert.equal(f.isUILoaded(), false);
  timers.advance(300);
  await Promise.resolve();
  /* flush gated on !uiLoaded */
  assert.ok(!calls.some((c) => c.method === "PUT"));
  await f.loadUI();
  await Promise.resolve();
  await Promise.resolve();
  assert.ok(calls.some((c) => c.method === "PUT" || c.path === UI_PATH));
});

test("sync warning: pending then failed text after unsuccessful flush", async () => {
  const timers = fakeTimers();
  const storage = memStorage();
  const syncwarnEl = { textContent: "" };
  const { fetchImpl, calls } = scriptedFetch([
    jsonResponse({ body: baseDoc(), etag: '"r1"' }),
    new Error("offline"),
  ]);
  const { f } = makeFeature({ timers, storage, syncwarnEl, fetchBundle: { fetchImpl, calls } });
  await f.loadUI();
  f.uiMutate({ k: "pin", id: "x" });
  assert.equal(syncwarnEl.textContent, "syncing…");
  timers.advance(300);
  await Promise.resolve();
  await Promise.resolve();
  assert.equal(syncwarnEl.textContent, "changes not saved yet");
});

/* ---------- pagehide ---------- */
test("pagehide: sends keepalive PUT when loaded+ops+rev; no send otherwise", async () => {
  const timers = fakeTimers();
  const window = fakeTarget();
  const storage = memStorage();
  const calls = [];
  const fetchImpl = async (path, opts = {}) => {
    calls.push({ path, opts, method: opts.method || "GET" });
    if (opts.method === "PUT") return jsonResponse({ status: 200, body: {}, etag: '"r2"' });
    return jsonResponse({ body: baseDoc(), etag: '"r1"' });
  };
  const { f } = makeFeature({
    timers,
    window,
    storage,
    fetchBundle: { fetchImpl, calls },
    csrf: "csrf-token",
  });
  f.bind();
  window.dispatch("pagehide");
  assert.equal(calls.length, 0); /* not loaded */

  await f.loadUI();
  window.dispatch("pagehide");
  assert.equal(calls.length, 1); /* only load GET */

  f.uiMutate({ k: "pin", id: "p" });
  const before = calls.length;
  window.dispatch("pagehide");
  assert.equal(calls.length, before + 1);
  const put = calls[calls.length - 1];
  assert.equal(put.method, "PUT");
  assert.equal(put.opts.keepalive, true);
  assert.equal(put.opts.headers.get("If-Match"), '"r1"');
  assert.equal(put.opts.headers.get(CSRF_HEADER), "csrf-token");
  assert.notEqual(put.opts.headers.get("If-Match"), "*");
  f.destroy(); /* cancel coalesced flush timer */
});

test("pagehide: thrown fetch is silenced", async () => {
  const timers = fakeTimers();
  const window = fakeTarget();
  const storage = memStorage();
  let n = 0;
  const fetchImpl = (path, opts = {}) => {
    n++;
    if (opts.method === "PUT") throw new Error("boom");
    return Promise.resolve(jsonResponse({ body: baseDoc(), etag: '"r1"' }));
  };
  const { f } = makeFeature({
    timers,
    window,
    storage,
    fetchBundle: { fetchImpl, calls: [] },
  });
  f.bind();
  await f.loadUI();
  f.uiMutate({ k: "pin", id: "p" });
  assert.doesNotThrow(() => window.dispatch("pagehide"));
  f.destroy();
});

test("two-second delay begins only after tick completion", async () => {
  const timers = fakeTimers();
  const hold = deferred();
  let hits = 0;
  const fetchImpl = async (path) => {
    if (path === UI_PATH) return jsonResponse({ status: 304, body: {} });
    hits++;
    if (hits === 1) {
      await hold.promise;
      return jsonResponse({ body: { nodes: [], unadopted: [] }, etag: '"e"' });
    }
    return jsonResponse({ body: { nodes: [], unadopted: [] }, etag: '"e2"' });
  };
  const document = fakeTarget({ hidden: false });
  const { f } = makeFeature({ timers, document, fetchBundle: { fetchImpl, calls: [] } });
  f.startPolling();
  await flush(2);
  assert.equal(hits, 1);
  timers.advance(2000);
  assert.equal(hits, 1); /* still in first tick */
  hold.resolve();
  await flush();
  assert.equal(hits, 1);
  assert.equal(timers.pendingCount(), 1);
  timers.advance(2000);
  await flush();
  assert.equal(hits, 2);
  f.destroy();
});
