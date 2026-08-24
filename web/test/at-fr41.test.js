/* S8 R1 — FR-41: the page owns the connection.
 *
 * "Owns" is about lifetime, not construction. The browser's
 * RTCPeerConnection is built by the rendezvous /p bundle (FR-33: a fixed
 * asset list sufficient only to establish the channel), and AT-FR-15-b
 * statically forbids the name from every served module here. So this layer
 * takes the peer and channel as injected dependencies and owns what happens
 * to them: the request registry, the FR-24 state, and teardown.
 *
 * The codec is injected because the wire format is a separate concern with
 * its own mechanical oracle (internal/remote/codec and its shared vectors).
 * The seam below mirrors codec.Request / codec.Response exactly — string
 * IDs, method/path/query/headers apart, and a *streamed* body — because a
 * seam invented from a simpler stand-in is a seam the real codec cannot
 * satisfy.
 *
 *   AT-FR-41-a  a dropped channel produces a named FR-24 state and an
 *               explicit reconnect, never a request that never settles
 *   AT-FR-41-b  every in-flight request at the moment of loss is rejected
 *               with a distinguishable error — including one whose head has
 *               arrived and whose body is still streaming
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

/* A JSON stand-in for internal/remote/codec's binary framing, implementing
 * the same seam: encodeRequest yields the chunks to write, receive() is fed
 * whatever arrives and yields zero or more decoded events. The transport
 * must not care which implementation it has. */
function jsonCodec() {
  return {
    encodeRequest: req => [JSON.stringify(req)],
    receive: data => {
      const msg = JSON.parse(String(data));
      return Array.isArray(msg) ? msg : [msg];
    },
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

const lastRequest = channel => JSON.parse(channel.sent[channel.sent.length - 1]);

function deliver(channel, events) {
  channel.fire("message", { data: JSON.stringify(events) });
}

/* Answer the most recent request in full: head, one body chunk, end. */
function answerLast(channel, { status = 200, headers = {}, body = "ok" } = {}) {
  const req = lastRequest(channel);
  deliver(channel, [
    { type: "response", id: req.id, status, headers },
    { type: "body", id: req.id, chunk: body },
    { type: "end", id: req.id },
  ]);
  return req;
}

/* Send only the head, leaving the body streaming. */
function headOnly(channel, { status = 200, headers = {} } = {}) {
  const req = lastRequest(channel);
  deliver(channel, [{ type: "response", id: req.id, status, headers }]);
  return req;
}

test("AT-FR-41-pre: a request over a live channel resolves (anti-vacuity)", async () => {
  const { channel, transport } = harness();
  const p = transport.fetchImpl("/api/state", {});
  assert.equal(channel.sent.length, 1, "the request was not sent over the channel");
  answerLast(channel, { status: 200, body: "hello" });
  const res = await within(p, "a request over a live channel");
  assert.equal(res.ok, true);
  assert.equal(res.status, 200);
  assert.equal(await res.text(), "hello");
});

test("AT-FR-41-pre: the encoded request matches codec.Request's shape", async () => {
  // codec.Request keeps ID, Method, Path, Query and Headers apart. A
  // transport that collapses them cannot be fed to the real codec.
  const { channel, transport } = harness();
  transport.fetchImpl("/api/nodes/n1/chat?history=1", {
    method: "POST",
    headers: { "X-Scimux-Csrf": "tok" },
    body: "hi",
  });
  const req = lastRequest(channel);

  assert.equal(typeof req.id, "string", "codec.Request.ID is a string, not a number");
  assert.notEqual(req.id, "", "the request carries no id");
  assert.equal(req.method, "POST");
  assert.equal(req.path, "/api/nodes/n1/chat", "query must not be left on the path");
  assert.equal(req.query, "history=1");
  assert.equal(req.headers["X-Scimux-Csrf"], "tok");
  assert.equal(req.body, "hi");
});

test("AT-FR-41-pre: method defaults to GET and an absent query is empty", async () => {
  const { channel, transport } = harness();
  transport.fetchImpl("/api/state", {});
  const req = lastRequest(channel);
  assert.equal(req.method, "GET");
  assert.equal(req.query, "");
  assert.equal(req.path, "/api/state");
});

test("AT-FR-41-pre: request ids are distinct", async () => {
  const { channel, transport } = harness();
  transport.fetchImpl("/api/state", {});
  transport.fetchImpl("/api/usage", {});
  const [a, b] = channel.sent.map(s => JSON.parse(s).id);
  assert.notEqual(a, b, "two concurrent requests share an id; answers cannot be routed");
});

test("AT-FR-41-pre: a streamed body is reassembled in order", async () => {
  const { channel, transport } = harness();
  const p = transport.fetchImpl("/api/state", {});
  const req = headOnly(channel, { headers: { "content-type": "application/json" } });

  const res = await within(p, "the response head");
  // The head resolves before the body completes — the same contract fetch
  // has, and the reason a body arriving in 0x02 frames is expressible here.
  deliver(channel, [{ type: "body", id: req.id, chunk: '{"a":' }]);
  deliver(channel, [{ type: "body", id: req.id, chunk: "1}" }]);
  deliver(channel, [{ type: "end", id: req.id }]);

  assert.deepEqual(await within(res.json(), "the streamed body"), { a: 1 });
});

test("AT-FR-41-pre: the response satisfies the shape api.js consumes", async () => {
  const { channel, transport } = harness();
  const p = transport.fetchImpl("/api/state", {});
  answerLast(channel, {
    status: 200,
    headers: { "content-type": "application/json" },
    body: '{"n":1}',
  });
  const res = await within(p, "a JSON response");
  // api.js reads r.headers.get("content-type") on every call, and HTTP
  // header names are case-insensitive.
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
  const res = await within(p, "a 409 response");
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

test("AT-FR-41-b: a body still streaming at the moment of loss rejects too", async () => {
  // The head resolved, so the caller already holds a response object. Its
  // body can never complete. Without this the request "succeeded" and the
  // hang simply moved from fetchImpl() to res.text() — which is worse,
  // because the FR-24 state says lost while a read sits there forever.
  const { channel, transport } = harness();
  const p = transport.fetchImpl("/api/state", {});
  const req = headOnly(channel);
  const res = await within(p, "the response head");

  deliver(channel, [{ type: "body", id: req.id, chunk: "partial" }]);
  const body = res.text().then(() => "resolved", e => e);

  channel.readyState = "closed";
  channel.fire("close", {});

  const r = await within(body, "a body still streaming at the moment of loss");
  assert.notEqual(r, "resolved", "a truncated body was returned as if complete");
  assert.equal(r.name, "remote-connection-lost");
  assert.equal(r.cause, CAUSE_CONNECTED_THEN_LOST);
});

test("AT-FR-41-b: an end with no head fails the request, it is not an empty success", async () => {
  // Found by mutation: resolving a bare "end" as a 200 with an empty body
  // turns a malformed answer into a plausible one, and api.js would parse it
  // as real data. There is one error vocabulary here — the caller only needs
  // to know no answer is coming.
  const { channel, transport } = harness();
  const p = transport.fetchImpl("/api/state", {});
  const req = lastRequest(channel);
  deliver(channel, [{ type: "end", id: req.id }]);

  await assert.rejects(
    () => within(p, "a request ended without a head"),
    err => err.name === "remote-connection-lost",
  );
  assert.equal(transport.pendingCount(), 0, "the request was left in the registry");
});

test("AT-FR-41-b: a reject frame settles the request distinguishably from loss", async () => {
  // codec.RejectError is the peer refusing this request — the connection is
  // fine. Reporting it as connection loss would send the UI to an FR-24
  // state over one bad request.
  const { channel, transport } = harness();
  const p = transport.fetchImpl("/api/state", {});
  const req = lastRequest(channel);
  deliver(channel, [{ type: "reject", id: req.id, class: "method", message: "method not allowed" }]);

  const err = await within(p.then(() => null, e => e), "a rejected request");
  assert.ok(err, "the request resolved despite being rejected");
  assert.notEqual(err.name, "remote-connection-lost", "a reject is not transport loss");
  assert.equal(err.class, "method");
  assert.equal(
    transport.state().connected,
    true,
    "a rejected request must not take the transport out of the connected state",
  );
  assert.equal(transport.pendingCount(), 0);
});

test("AT-FR-41-b: a late answer for a rejected request is ignored, not re-settled", async () => {
  const { channel, transport } = harness();
  const p = transport.fetchImpl("/api/state", {});
  const req = lastRequest(channel);
  const settled = p.then(() => "resolved", e => e.name);

  channel.readyState = "closed";
  channel.fire("close", {});
  assert.equal(await within(settled, "the in-flight request"), "remote-connection-lost");

  // The peer's answer arrives after we gave up. It must not throw and must
  // not resurrect anything.
  deliver(channel, [
    { type: "response", id: req.id, status: 200, headers: {} },
    { type: "end", id: req.id },
  ]);
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
  assert.equal(
    await within(settled, "the request outstanding at close()"),
    "remote-connection-lost",
  );
});

test("AT-FR-41-c: close() rejects a body still streaming", async () => {
  const { channel, transport } = harness();
  const p = transport.fetchImpl("/api/state", {});
  headOnly(channel);
  const res = await within(p, "the response head");
  const body = res.text().then(() => "resolved", e => e.name);
  transport.close();
  assert.equal(
    await within(body, "a body streaming at close()"),
    "remote-connection-lost",
  );
});
