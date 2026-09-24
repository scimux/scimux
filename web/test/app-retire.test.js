/* Boot-twice retirement for the composition root (web/js/app.js).
 *
 * A phone that sleeps loses the peer connection; rv re-dials on wake and runs
 * the FR-40 handover again, which re-mints every module as a fresh blob: URL,
 * installs a new head/body and calls createApp() a second time. The document's
 * *elements* are discarded with their listeners, but the document and window
 * objects are the same, so everything registered on them — and every timer —
 * outlives the swap. Nothing retired the instance that came before, so both
 * polled, both wrote the status bar, and because the retired one holds the dead
 * transport (FR-41: once `cause` is set, every request is refused forever) it
 * painted "server unreachable" every 2s over a connection the live instance was
 * demonstrably using. Each sleep/wake added another.
 *
 * These are behavioural tests, not source-text ratchets: they run the real
 * createApp against the fake host and read the listener/timer ledger.
 */
import test from "node:test";
import assert from "node:assert/strict";
import { installHost, identityAssetURL, jsonResponse } from "./s2-helpers.js";
import { APP_SLOT } from "../js/lifecycle.js";

const flush = async (n = 30) => { for (let i = 0; i < n; i++) await Promise.resolve(); };

/* Records listeners on the targets that survive a document swap, and every
   timer, without ever firing one. Element listeners are deliberately not
   tracked: those nodes are thrown away by the handover. */
function instrument(host) {
  const listeners = [];
  const timers = [];

  const track = (where, obj) => {
    obj.addEventListener = (type, fn, opts) => {
      listeners.push({ where, type, fn, opts, live: true });
    };
    obj.removeEventListener = (type, fn) => {
      for (const l of listeners) {
        if (l.live && l.where === where && l.type === type && l.fn === fn) {
          l.live = false;
          return;
        }
      }
    };
  };
  track("document", host.document);
  track("window", host.window);
  if (host.window.visualViewport) track("visualViewport", host.window.visualViewport);

  /* app.js reads matchMedia off the global; insets.js off the injected window. */
  const matchMedia = q => {
    const mql = {
      matches: /min-width:\s*900/.test(q),
      media: q,
      addListener() {},
      removeListener() {},
    };
    track("matchMedia " + q, mql);
    return mql;
  };
  host.window.matchMedia = matchMedia;
  globalThis.matchMedia = matchMedia;

  let nextId = 0;
  const clock = (kind) => (fn, ms) => {
    const id = ++nextId;
    timers.push({ kind, id, ms, fn, live: true });
    return id;
  };
  const stop = id => {
    const t = timers.find(t => t.id === id);
    if (t) t.live = false;
  };
  host.window.setTimeout = clock("timeout");
  host.window.setInterval = clock("interval");
  host.window.clearTimeout = stop;
  host.window.clearInterval = stop;

  return {
    listeners,
    timers,
    liveListeners: () => listeners.filter(l => l.live),
    liveTimers: () => timers.filter(t => t.live),
  };
}

async function bootTwice(t, { secondGraph = false } = {}) {
  const host = installHost({});
  const rec = instrument(host);
  const deps = () => ({
    fetchImpl: host.trapFetch,
    assetURL: identityAssetURL,
    document: host.document,
    window: host.window,
  });

  const first = await (await import("../js/app.js")).createApp(deps());
  await flush();
  const firstListeners = rec.liveListeners();
  const firstTimers = rec.liveTimers();

  const successor = secondGraph
    ? await import("../js/app.js?graph=successor")
    : await import("../js/app.js");
  const second = await successor.createApp(deps());
  await flush();

  t.after(() => {
    try { host.window[APP_SLOT]?.retire?.(); } catch { /* best effort */ }
  });
  return { host, rec, first, second, firstListeners, firstTimers };
}

test("app: booting twice in one window leaves no listener from the first instance", async (t) => {
  const { rec, firstListeners } = await bootTwice(t);
  assert.ok(firstListeners.length > 0,
    "the harness must actually observe the shell's document/window listeners");
  assert.deepEqual(
    firstListeners.filter(l => l.live).map(l => `${l.where} ${l.type}`),
    [],
    "a surviving visibilitychange listener is what restarted the retired instance's poll loop");
  assert.ok(rec.liveListeners().length > 0, "the second instance must still be wired");
});

test("app: booting twice in one window stops every timer the first instance started", async (t) => {
  const { firstTimers } = await bootTwice(t);
  assert.ok(firstTimers.length > 0, "the harness must actually observe the shell's cadences");
  assert.deepEqual(firstTimers.filter(t => t.live).map(t => `${t.kind} ${t.ms}`), [],
    "the 8s status phase and 30s usage cadences would otherwise double on every wake");
});

test("app: the second boot's listener count matches the first, not double it", async (t) => {
  const { rec, firstListeners } = await bootTwice(t);
  assert.equal(rec.liveListeners().length, firstListeners.length,
    "two boots must leave exactly one instance's worth of surviving listeners");
});

test("app: a handover's fresh module graph still retires the predecessor", async (t) => {
  /* The production condition: the successor's app.js is a different module
     object, so nothing but the window connects the two instances. */
  const { rec, firstListeners, firstTimers } = await bootTwice(t, { secondGraph: true });
  assert.deepEqual(firstListeners.filter(l => l.live).map(l => `${l.where} ${l.type}`), []);
  assert.deepEqual(firstTimers.filter(t => t.live).map(t => `${t.kind} ${t.ms}`), []);
  assert.equal(rec.liveListeners().length, firstListeners.length);
});

test("app: createApp claims the window slot and returns a retire handle", async (t) => {
  const { host, second } = await bootTwice(t);
  assert.equal(typeof second?.retire, "function",
    "the handle is how a successor — or a caller that owns the page — retires an instance");
  assert.equal(host.window[APP_SLOT], second,
    "the slot lives on the window because that is the only identity a handover preserves");
});

test("app: retire() leaves nothing registered behind", async (t) => {
  const host = installHost({});
  const rec = instrument(host);
  const { createApp } = await import("../js/app.js");
  const app = await createApp({
    fetchImpl: host.trapFetch,
    assetURL: identityAssetURL,
    document: host.document,
    window: host.window,
  });
  await flush();
  assert.ok(rec.liveListeners().length > 0);
  app.retire();
  await flush();
  assert.deepEqual(rec.liveListeners().map(l => `${l.where} ${l.type}`), []);
  assert.deepEqual(rec.liveTimers().map(x => `${x.kind} ${x.ms}`), []);
  app.retire();  /* idempotent */
});

test("app: legacy media resolution runs the composed bookmark and Notes refresh callback", async () => {
  const host = installHost({});
  const calls = [];
  let failCapture = false;
  let circularProv = false;
  const fetchImpl = async (path, opts = {}) => {
    calls.push(String(path));
    if (String(path).startsWith("/api/ui")) return jsonResponse({lanes:[],groups:[],archived:[],pinned:[],bookmarks:[{
      t:"T",uid:"u",segment:0,record:0,text:"![one](scimux-asset:a_1)",
    }]});
    if (String(path).startsWith("/api/preview")) return jsonResponse({uid:"u",node:"n1",anchor:0,turns:[{uid:"u",segment:0,record:0}],assets:{a_1:{name:"one.png"}}});
    if (String(path).startsWith("/api/state")) return jsonResponse({nodes:[{id:"n1",title:"One",agent:"codex",created_at:"T0"}],unadopted:[],hostname:"computer",version:"test",sys:{}});
    if (String(path).startsWith("/api/nodes/n1/chat")) {
      const prov = circularProv ? {} : {source:"synthetic"}; if (circularProv) prov.self = prov;
      return jsonResponse({turns:[{role:"assistant",agent:"codex",prov,text:"![one](scimux-asset:a_1)",time:"T",uid:"u",segment:0,record:0}],segments:[],assets:{a_1:{name:"one.png"}}});
    }
    if (String(path) === "/api/reference-media") {
      if (failCapture) throw new Error("budget");
      return jsonResponse({text:"![one](scimux-asset:a_1)",media:{version:1,capture_id:"a".repeat(64),items:[{id:"0",key:"asset:a_1",name:"one.png",state:"ready"}]}});
    }
    return host.trapFetch(path, opts);
  };
  const { createApp } = await import("../js/app.js");
  const app = await createApp({fetchImpl,assetURL:identityAssetURL,document:host.document,window:host.window});
  await flush();
  host.byId.get("bookmarksbtn").dispatchEvent({type:"click",preventDefault(){},stopPropagation(){}});
  await new Promise(resolve => setTimeout(resolve, 5));
  assert.ok(calls.some(x => x.startsWith("/api/preview")), calls.join(","));
  const station = {dataset:{forkbookmark:"n1"},closest:sel=>sel === "[data-forkbookmark]" ? station : null};
  host.byId.get("maptoolbar").dispatchEvent({type:"click",target:station,preventDefault(){},stopPropagation(){}});
  await flush();
  assert.ok(calls.includes("/api/reference-media"), calls.join(","));
  failCapture = true; circularProv = true;
  host.byId.get("maptoolbar").dispatchEvent({type:"click",target:station,preventDefault(){},stopPropagation(){}});
  await flush();
  app.retire();
});
