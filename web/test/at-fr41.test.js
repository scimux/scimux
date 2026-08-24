/* S8 R1 — FR-41: the page owns the connection.
 *
 * "Owns" is about lifetime, not construction. The browser's
 * RTCPeerConnection is built by the rendezvous /p bundle (FR-33: a fixed
 * asset list sufficient only to establish the channel), and AT-FR-15-b
 * statically forbids the name from every served module here. So this layer
 * takes the peer and channel as injected dependencies and owns what happens
 * to them: the request registry, the FR-24 state, and teardown.
 *
 * The codec is injected for the same reason it is a separate concern — the
 * wire format is internal/remote/codec's binary framing, and its browser half
 * has a mechanical oracle (the Go implementation and the shared vectors).
 * Nothing here should encode a byte itself.
 *
 *   AT-FR-41-a  a dropped channel produces a named FR-24 state and an
 *               explicit reconnect, never a request that never settles
 *   AT-FR-41-b  every in-flight request at the moment of loss is rejected
 *               with a distinguishable error
 *   AT-FR-41-c  nothing schedules work in, or depends on, a context that
 *               outlives the page
 */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

import {
  createChannelTransport,
  CAUSE_ICE_FAILED,
  CAUSE_CONNECTED_THEN_LOST,
} from "../js/connection.js";

const HERE = dirname(fileURLToPath(import.meta.url));
const MODULE = join(HERE, "..", "js", "connection.js");

/* A data channel with the shape the real one has: readyState, send,
 * addEventListener, and events we can fire. Deliberately not an
 * RTCDataChannel stand-in beyond what this layer touches. */
function fakeChannel() {
  const listeners = new Map();
  return {
    readyState: "open",
    sent: [],
    send(data) {
      if (this.readyState !== "open") throw new Error("channel is not open");
      this.sent.push(data);
    },
    addEventListener(type, fn) {
      if (!listeners.has(type)) listeners.set(type, []);
      listeners.get(type).push(fn);
    },
    removeEventListener(type, fn) {
      const l = listeners.get(type) || [];
      const i = l.indexOf(fn);
      if (i >= 0) l.splice(i, 1);
    },
    fire(type, ev) {
      for (const fn of (listeners.get(type) || []).slice()) fn(ev);
    },
    listenerCount(type) {
      return (listeners.get(type) || []).length;
    },
  };
}

function fakePeer() {
  const listeners = new Map();
  return {
    connectionState: "connected",
    addEventListener(type, fn) {
      if (!listeners.has(type)) listeners.set(type, []);
      listeners.get(type).push(fn);
    },
    removeEventListener(type, fn) {
      const l = listeners.get(type) || [];
      const i = l.indexOf(fn);
      if (i >= 0) l.splice(i, 1);
    },
    fire(type, ev) {
      for (const fn of (listeners.get(type) || []).slice()) fn(ev);
    },
    listenerCount(type) {
      return (listeners.get(type) || []).length;
    },
  };
}

/* A JSON stand-in for internal/remote/codec's binary framing. The transport
 * must not care which one it has. */
function jsonCodec() {
  return {
    encode: req => JSON.stringify(req),
    decode: data => JSON.parse(String(data)),
  };
}

function harness({ reconnect } = {}) {
  const peer = fakePeer();
  const channel = fakeChannel();
  const states = [];
  const t = createChannelTransport({
    peer,
    channel,
    codec: jsonCodec(),
    reconnect: reconnect || (() => {}),
    onState: s => states.push(s),
  });
  return { peer, channel, transport: t, states };
}

/* FR-41's failure mode is a promise that never settles, so every wait below
 * is bounded. Without this a regression that leaves requests hanging wedges
 * the run instead of failing it — which is exactly what happened when this
 * suite was mutation-checked against a transport that stopped clearing its
 * registry on loss. Deliberately not used inside the timer-trap test, which
 * replaces setTimeout to count live handles. */
function within(p, what, ms = 1000) {
  return Promise.race([
    p,
    new Promise((_, reject) => {
      const h = setTimeout(() => reject(new Error(`never settled: ${what}`)), ms);
      if (h && typeof h.unref === "function") h.unref();
    }),
  ]);
}

/* Answer the request the transport most recently sent. */
function answerLast(channel, { status = 200, body = "ok" } = {}) {
  const req = JSON.parse(channel.sent[channel.sent.length - 1]);
  channel.fire("message", {
    data: JSON.stringify({ id: req.id, status, headers: {}, body }),
  });
  return req;
}

test("AT-FR-41-pre: a request over a live channel resolves (anti-vacuity)", async () => {
  const { channel, transport } = harness();
  const p = transport.fetchImpl("/api/state", {});
  assert.equal(channel.sent.length, 1, "the request was not sent over the channel");
  answerLast(channel, { status: 200, body: "hello" });
  const res = await p;
  assert.equal(res.ok, true);
  assert.equal(res.status, 200);
  assert.equal(await res.text(), "hello");
  // api.js reads r.headers.get("content-type") on every call; a response
  // without it breaks every consumer of the FR-42 seam.
  assert.equal(typeof res.headers.get, "function", "the response carries no headers.get");
});

test("AT-FR-41-pre: the response satisfies the shape api.js consumes", async () => {
  const { channel, transport } = harness();
  const p = transport.fetchImpl("/api/state", {});
  const req = JSON.parse(channel.sent[0]);
  channel.fire("message", {
    data: JSON.stringify({
      id: req.id,
      status: 200,
      headers: { "content-type": "application/json" },
      body: '{"n":1}',
    }),
  });
  const res = await p;
  assert.equal(res.headers.get("content-type"), "application/json");
  assert.equal(res.headers.get("Content-Type"), "application/json", "header lookup must be case-insensitive");
  assert.deepEqual(await res.json(), { n: 1 });
});

test("AT-FR-41-pre: a non-2xx answer resolves as a response, it is not an error", async () => {
  // api.js distinguishes a 409 conflict from a failure by r.status; a
  // transport that rejected on 4xx would destroy that.
  const { channel, transport } = harness();
  const p = transport.fetchImpl("/api/ui", {});
  answerLast(channel, { status: 409, body: "conflict" });
  const res = await p;
  assert.equal(res.ok, false);
  assert.equal(res.status, 409);
});

test("AT-FR-41-a: a dropped channel names an FR-24 state and offers an explicit reconnect", async () => {
  let reconnects = 0;
  const { channel, transport, states } = harness({ reconnect: () => { reconnects += 1; } });

  assert.equal(transport.state().connected, true, "should start connected");

  channel.readyState = "closed";
  channel.fire("close", {});

  const st = transport.state();
  assert.equal(st.connected, false);
  assert.equal(
    st.cause,
    CAUSE_CONNECTED_THEN_LOST,
    "a channel that was open and then closed is connected-then-lost, not a generic failure",
  );
  assert.ok(
    states.some(s => s.cause === CAUSE_CONNECTED_THEN_LOST),
    "the state change was not published",
  );

  // Explicit, not automatic: nothing may reconnect on its own.
  assert.equal(reconnects, 0, "the transport reconnected without being asked");
  transport.reconnect();
  assert.equal(reconnects, 1, "reconnect() did not reach the injected reconnect");
});

test("AT-FR-41-a: ICE failure is a distinct named state, not connected-then-lost", async () => {
  const { peer, transport } = harness();
  peer.connectionState = "failed";
  peer.fire("connectionstatechange", {});

  const st = transport.state();
  assert.equal(st.connected, false);
  assert.equal(
    st.cause,
    CAUSE_ICE_FAILED,
    "peer connectionState 'failed' is ice-failed; conflating it with connected-then-lost is the defect FR-24 guards",
  );
  assert.notEqual(CAUSE_ICE_FAILED, CAUSE_CONNECTED_THEN_LOST);
});

test("AT-FR-41-a: a request issued after loss rejects immediately rather than hanging", async () => {
  const { channel, transport } = harness();
  channel.readyState = "closed";
  channel.fire("close", {});

  const before = channel.sent.length;
  await assert.rejects(
    () => within(transport.fetchImpl("/api/state", {}), "a request issued after loss"),
    err => err.name === "remote-connection-lost" && err.cause === CAUSE_CONNECTED_THEN_LOST,
    "a request on a dead channel must reject with the named error, not wait",
  );
  assert.equal(channel.sent.length, before, "a request was written to a closed channel");
});

test("AT-FR-41-a: loss is refused by the transport, not by the channel throwing", async () => {
  // The sharp case, and the one a mutation found: after ICE failure the data
  // channel can still report readyState "open", so channel.send() does not
  // throw. A transport that relies on the throw to notice would queue the
  // request and leave it outstanding forever. The refusal has to come from
  // the transport's own FR-24 state.
  const { peer, channel, transport } = harness();
  peer.connectionState = "failed";
  peer.fire("connectionstatechange", {});
  assert.equal(channel.readyState, "open", "precondition: the channel still looks open");

  await assert.rejects(
    () => within(transport.fetchImpl("/api/state", {}), "a request issued after ICE failure"),
    err => err.name === "remote-connection-lost" && err.cause === CAUSE_ICE_FAILED,
  );
  assert.equal(channel.sent.length, 0, "a request was written after the peer failed");
  assert.equal(transport.pendingCount(), 0, "the request was left in the registry");
});

test("AT-FR-41-b: every in-flight request is rejected distinguishably at the moment of loss", async () => {
  const { channel, transport } = harness();

  const inflight = [
    transport.fetchImpl("/api/state", {}),
    transport.fetchImpl("/api/usage", {}),
    transport.fetchImpl("/api/nodes/n1/chat", {}),
  ];
  assert.equal(channel.sent.length, 3, "anti-vacuity: not all three requests were sent");

  const settled = inflight.map(p => p.then(() => "resolved", e => e));
  channel.readyState = "closed";
  channel.fire("close", {});

  const results = await within(Promise.all(settled), "in-flight requests at the moment of loss");
  for (const [i, r] of results.entries()) {
    assert.notEqual(r, "resolved", `request ${i} resolved despite connection loss`);
    assert.equal(r.name, "remote-connection-lost", `request ${i} rejected with ${r.name}`);
    assert.equal(r.cause, CAUSE_CONNECTED_THEN_LOST, `request ${i} carries no FR-24 cause`);
  }

  assert.equal(transport.pendingCount(), 0, "the registry still holds requests after loss");
});

test("AT-FR-41-b: a late answer for a rejected request is ignored, not re-settled", async () => {
  const { channel, transport } = harness();
  const p = transport.fetchImpl("/api/state", {});
  const req = JSON.parse(channel.sent[0]);
  const settled = p.then(() => "resolved", e => e.name);

  channel.readyState = "closed";
  channel.fire("close", {});
  assert.equal(await within(settled, "the in-flight request"), "remote-connection-lost");

  // The peer's answer arrives after we gave up. It must not throw and must
  // not resurrect anything.
  channel.fire("message", {
    data: JSON.stringify({ id: req.id, status: 200, headers: {}, body: "late" }),
  });
  assert.equal(transport.pendingCount(), 0);
});

test("AT-FR-41-c: nothing depends on a context that outlives the page", async () => {
  const src = readFileSync(MODULE, "utf8");
  for (const banned of [
    /\bserviceWorker\b/,
    /\bSharedWorker\b/,
    /\bBroadcastChannel\b/,
    /\bnew\s+Worker\b/,
    /\bcaches\s*\./,
    /\blocalStorage\b/,
    /\bindexedDB\b/,
  ]) {
    assert.ok(
      !banned.test(src),
      `connection.js references ${banned} — FR-41 forbids depending on a context outliving the page`,
    );
  }
  // AT-FR-15-b bans the constructor name from served modules; this layer
  // takes the connection injected, so the ban must hold here too.
  assert.ok(
    !/\b(?:webkit)?RTCPeerConnection\b/.test(src) && !/\bRTCDataChannel\b/.test(src),
    "connection.js must take the peer and channel injected, never construct them",
  );
});

test("AT-FR-41-c: close() releases every listener and leaves no live timer", async () => {
  const realSetInterval = globalThis.setInterval;
  const realSetTimeout = globalThis.setTimeout;
  const live = new Set();
  globalThis.setInterval = (...a) => {
    const h = realSetInterval(...a);
    live.add(h);
    return h;
  };
  globalThis.setTimeout = (...a) => {
    const h = realSetTimeout(...a);
    live.add(h);
    return h;
  };
  const realClearInterval = globalThis.clearInterval;
  const realClearTimeout = globalThis.clearTimeout;
  globalThis.clearInterval = h => { live.delete(h); return realClearInterval(h); };
  globalThis.clearTimeout = h => { live.delete(h); return realClearTimeout(h); };

  try {
    const { peer, channel, transport } = harness();
    transport.fetchImpl("/api/state", {}).catch(() => {});
    transport.close();

    assert.equal(transport.pendingCount(), 0, "close() left requests pending");
    assert.equal(live.size, 0, "close() left a timer scheduled");
    assert.equal(channel.listenerCount("message"), 0, "close() left a message listener attached");
    assert.equal(channel.listenerCount("close"), 0, "close() left a close listener attached");
    assert.equal(
      peer.listenerCount("connectionstatechange"),
      0,
      "close() left a peer listener attached",
    );
  } finally {
    globalThis.setInterval = realSetInterval;
    globalThis.setTimeout = realSetTimeout;
    globalThis.clearInterval = realClearInterval;
    globalThis.clearTimeout = realClearTimeout;
  }
});

test("AT-FR-41-c: close() rejects in-flight requests too", async () => {
  const { transport } = harness();
  const p = transport.fetchImpl("/api/state", {});
  const settled = p.then(() => "resolved", e => e.name);
  transport.close();
  assert.equal(await within(settled, "the request outstanding at close()"), "remote-connection-lost");
});
